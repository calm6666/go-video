-- =====================================================================
-- recommend-recall 服务 - 池版本登记表（可追溯性证据链）
-- =====================================================================
-- 用途：建立 recall_pool_version，登记「哪个生成批次产出了哪个版本、多少条、当前什么状态」。
--   在线召回只读 recall_pool_current 指向的版本，但每一次"为什么给了这批候选"
--   都必须能回指到这张表的一行 —— 它是候选可追溯性的证据链。
-- 数据所有者：recommend-recall 服务（AGENTS.md §5）。
--
-- 数据库：go_video_recommend_recall。
--
-- 事实定位：本表是半事实（不是投影）。
--   recall_pool 的条目可以从批次重算，但「批次 X 在时间 T 产出了版本 V、当时声明多少条」
--   这一事实无法从条目本身推导，因此本表不随条目重算而丢失，
--   只有 PrunePoolVersions 判定为超出保留窗口的 RETIRED/FAILED 行才允许删除。
--
-- 唯一键与幂等设计：
--   1. uniq_pool_version (source, pool_key, version)：UpsertPoolItems 首批登记（Register）的
--      ODKU 锚点。重复注册只刷新 generator/schema_version/operator/note/mtime，
--      绝不覆盖 batch_id 与 state —— 批次归属和状态机是本表的核心不变量。
--      Register 写完后回读 batch_id 比对，不一致返回 model.ErrVersionReuseBlocked，
--      这就是"两个离线作业串写到同一版本号"的硬失败点。
--   2. uniq_pool_version_batch (source, pool_key, batch_id)：一个批次在同一池内至多产出一个版本；
--      供 FindByBatch 用批次号反查版本（离线作业重跑时先查自己上次落到哪个版本）。
--
-- 「一个池同时只有一个 CURRENT」为什么不用唯一索引：
--   MySQL 没有部分唯一索引（partial/filtered index）。
--   若建 UNIQUE (source, pool_key, state)，则同一池的多行 RETIRED 会互相冲突，直接不可行；
--   若引入 generated column 只对 state=CURRENT 赋值再建唯一键，会让所有 UPDATE 语句都要考虑
--   该虚拟列，收益不如把不变量收敛到一处：
--   本服务把 recall_pool_current 的版本指针 CAS（见 000003）当作唯一权威，
--   本表 state 由同一事务内的 UpdateState/SetPublishedAt 同步，属可重建的冗余视图。
--
-- 索引取自真实查询路径（对应 model/poolversion.go）：
--   · FindOne / ListByRefs：WHERE (source, pool_key, version) 等值 -> uniq_pool_version；
--   · MaxVersion / PrunableBefore / ListByPool / ListPrunable：同池按 version 排序或范围扫描
--     -> uniq_pool_version 的 (source, pool_key) 前缀 + version 范围（version < ?）；
--   · FindCurrent：WHERE source, pool_key, state=CURRENT ORDER BY version DESC LIMIT 1
--     -> idx_pool_state_version 直接命中，避免"扫完该池全部版本再筛状态"
--     （热门池版本数会随 cron 每日产出持续累积）；
--   · FindByID / UpdateState / SetPublishedAt / UpdateItemCount / Delete：按主键 id；
--   · Delete：WHERE state IN (RETIRED, FAILED) AND id IN (...)，state 条件只做二次确认，
--     定位仍走主键，因此不为 state 单独建索引。
--
-- 容量：版本行数 = Σ(池数 x 保留版本数)。cron 每日为热门/冷启动池产出新版本，
--   PrunePoolVersions 按 keep_versions 保留窗口分批删除（含本表行与 recall_pool 条目）。
--
-- 回滚：
--   DROP TABLE IF EXISTS `recall_pool_version`;
--   注意：删本表会让"候选来自哪个批次"不可追溯，且 RecallCandidates 的版本核对会失败，
--   回滚前必须确认没有在线流量（本表不是可随意重建的投影）。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `recall_pool_version` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（状态推进与删除按此定位）',
  `source`         SMALLINT        NOT NULL DEFAULT 0 COMMENT '召回路，取值同 rpc.Source',
  `pool_key`       VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '池键（语法同 recall_pool.pool_key，受 model.ValidatePoolKey 约束）',
  `version`        BIGINT          NOT NULL DEFAULT 0 COMMENT '版本号，同池单调递增；由生成方指定（不是自增列），因此必须靠唯一键防串写',
  `batch_id`       VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '生成批次 ID（ULID/作业运行号），可追溯性的关键列；Register 之后永不改写',
  `generator`      VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '产出方标识（cron job 名或离线作业 ID），区分"谁写的这个版本"',
  `schema_version` INT             NOT NULL DEFAULT 1 COMMENT '池条目结构版本（model.PoolVersionSchemaVersion），读取方据此判断能否解释条目',
  `item_count`     BIGINT          NOT NULL DEFAULT 0 COMMENT '该版本条目数（UpsertPoolItems 累计；发布前与 COUNT(*) 核对，是核对用的声明值）',
  `state`          SMALLINT        NOT NULL DEFAULT 1 COMMENT '版本状态，取值同 rpc.PoolVersionState：1 BUILDING、2 READY、3 CURRENT、4 RETIRED、5 FAILED；BUILDING/FAILED 永不在线出数',
  `published_at`   BIGINT          NOT NULL DEFAULT 0 COMMENT '生效时间（Unix 秒），0 表示从未上线；GetRecallConfig 的 stale 判定按此列',
  `operator`       VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '最后操作者（作业或运营标识），写接口必填',
  `note`           VARCHAR(255)    NOT NULL DEFAULT '' COMMENT '变更说明（切换/回滚原因，审计用；不含用户敏感信息）',
  `ctime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_pool_version` (`source`, `pool_key`, `version`),
  UNIQUE KEY `uniq_pool_version_batch` (`source`, `pool_key`, `batch_id`),
  -- FindCurrent：某池 state=CURRENT 的那一行，按 version DESC 取首行
  KEY `idx_pool_state_version` (`source`, `pool_key`, `state`, `version`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='召回池版本登记表（批次->版本->状态的证据链，UpsertPoolItems/PublishPoolVersion/PrunePoolVersions 的共同支点）';
