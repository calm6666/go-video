-- =====================================================================
-- recommend-recall 服务 - 候选池条目表（投影）
-- =====================================================================
-- 用途：建立 recall_pool，存放「某召回路的某个池的某个版本」包含哪些稿件及池内分数。
--   在线召回主读路径（RecallCandidates -> Pool.TopByVersion）与运维快照读
--   （GetPoolSnapshot -> Pool.ListByVersion）都打在这张表上。
-- 数据所有者：recommend-recall 服务（AGENTS.md §5）。其他服务禁止直接读写本表，
--   只能经 rpc.Recall 的 UpsertPoolItems / PublishPoolVersion 等接口写入。
--
-- 数据库：go_video_recommend_recall（建库由 scripts/migrate.ps1 负责，本文件只建表）。
--
-- 本表是全库唯一「可从事实重算的投影」：
--   条目内容由生成批次（batch_id + generator，登记在 recall_pool_version）产出，
--   删除任意版本的条目都不会丢失业务事实，重跑生成作业即可整批重建；
--   回滚/上线只切 recall_pool_current 的版本指针，从不改写本表内容。
--   因此本表不需要唯一事实源级别的备份，允许 PrunePoolVersions 分批清理。
--
-- 唯一键与幂等设计：
--   uniq_pool_item (source, pool_key, version, aid) 是 UpsertPoolItems 的幂等锚点：
--   model/pool.go BatchUpsert 走 INSERT ... ON DUPLICATE KEY UPDATE
--   `score = VALUES(score), mtime = VALUES(mtime)`，同一批次重放只刷新分数、不产生第二条候选。
--   本表刻意只保留这一个唯一键：多个唯一键会让 ODKU 命中非预期索引而"更新错行"，
--   这是幂等写入最难查的一类故障。（"同一版本号不得属于两个批次"这条不变量
--   由 recall_pool_version 的唯一键负责，见 000002。）
--
-- 索引取自真实查询路径（列名与顺序严格对应 model/pool.go 的 SQL）：
--   1. uniq_pool_item 前缀 (source, pool_key, version) 覆盖三条路径：
--      · CountByVersion：SELECT COUNT(1) WHERE source/pool_key/version 等值；
--      · DeleteByVersions：DELETE ... AND version IN (...) LIMIT ?（分批清理，走 version 范围）；
--      · TopByVersion / ListByVersion 的等值定位部分。
--   2. idx_pool_version_score (source, pool_key, version, score)：在线召回只要"分数最高的前 N 条"
--      （ORDER BY score DESC, aid ASC LIMIT ?）。有了这一列，Top-N 变成索引有序扫描 + 提前终止，
--      而不是把整个版本（热门池可达数十万条）读出来排序。
--      刻意不使用 MySQL 8 的降序索引（score DESC, aid ASC）：那会把 DDL 绑死在 8.0 上，
--      而"同分候选按 aid 稳定排序"只在分数完全相等时才需要索引兜底，
--      反向扫描 + 有界排序（行数不超过 LIMIT 与同分组）代价可接受。
--   aid 不进第二条二级索引：本服务没有"按 aid 反查在哪些池里"的接口，
--   预留索引只会拖慢离线批量写入（AGENTS.md §4 不为不存在的查询建索引）。
--
-- 容量：条目数 = Σ(每池版本数 x 版本内条数)。当前按每池 <=50000 条、
--   每池保留 MinKeepVersions(2) 个历史版本估算，超限时靠 PrunePoolVersions 分批清理控制，
--   不分库分表；在线读走 Redis 缓存（CacheRedis），MySQL 只承担回源。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不改已存在的表；
--   批量写入单批不超过 model.MaxPoolItemBatch(2000) 行，删除必须带 LIMIT
--   （model.MaxDeleteRows=5000），避免一次锁住整段版本。
--   后续任何变更必须新增 0000NN_*.sql，禁止修改本文件。
--
-- 回滚：
--   DROP TABLE IF EXISTS `recall_pool`;
--   （本表是投影，回滚不丢事实；重建只需重跑生成作业 + UpsertPoolItems。）
-- =====================================================================

CREATE TABLE IF NOT EXISTS `recall_pool` (
  `id`       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（无业务含义，仅为聚簇索引稳定）',
  `source`   SMALLINT        NOT NULL DEFAULT 0 COMMENT '召回路，取值同 rpc.Source：1 热门、2 关注、3 标签、4 协同、5 向量、6 冷启动',
  `pool_key` VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '池键，语法受 model.ValidatePoolKey 约束（global / zone:<typeid> / mid:<mid> / tag:<tag_id> / aid:<seed_aid> / platform:<platform>），不接受调用方自由拼接',
  `version`  BIGINT          NOT NULL DEFAULT 0 COMMENT '池版本号（同池单调递增，由 recall_pool_version 登记）；0 不是合法版本，模型层直接拒绝',
  `aid`      BIGINT          NOT NULL DEFAULT 0 COMMENT '稿件 ID（video 服务主键，只存引用；本表不复制稿件字段，稿件可见性归 video）',
  `score`    DOUBLE          NOT NULL DEFAULT 0 COMMENT '池内分数：同一路内可比，跨路不可比（合并时按 model.SourcePriority 而非分数）',
  `ctime`    BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`    BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒，重复写入只刷新此列与 score）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_pool_item` (`source`, `pool_key`, `version`, `aid`),
  -- 在线召回 Top-N：WHERE source/pool_key/version 等值 + ORDER BY score DESC LIMIT N
  KEY `idx_pool_version_score` (`source`, `pool_key`, `version`, `score`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='召回候选池条目表（按 (召回路,池,版本) 分版本的候选集合；可从生成批次整批重算的投影）';
