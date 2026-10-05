-- =====================================================================
-- search-query 服务 - 查询衍生数据表（搜索历史 / 查询日志 / 热词快照 / 屏蔽词 / 事件 Outbox）
-- =====================================================================
-- 数据所有者：search-query 服务（AGENTS.md §5）。其它服务禁止直接读写本目录的表，
--   包括 search-indexer：它只维护 OpenSearch 索引与读别名，本服务不写索引，
--   它也不写本目录任何表。跨服务只保留业务主键引用（如 author_mid），不建外键。
--
-- 库名：go_video_search_query（取自 services/search-query/etc/searchquery.v1.yaml 的 DataSource）。
--
-- 表与写入路径（本服务查询链路只写前 3 张，后 2 张由离线/运营维护）：
--   1. search_history      —— 登录用户的搜索历史。uniq(mid, keyword) +
--        INSERT ... ON DUPLICATE KEY UPDATE 实现幂等（同用户重复搜同一词只更新时间）。
--        单用户规模由服务端 Prune 限制（repository.historyKeepRows=300）。
--        隐私：用户删除/清空为物理 DELETE，不做软删保留；不存原始 IP、设备号。
--   2. search_query_log    —— 查询行为摘要（ReportQuery 写入），是热词聚合与
--        实时大盘的事实源。query_id 唯一索引 + INSERT IGNORE 承载上报幂等。
--        只存规范化关键词与 ip_hash（脱敏列宽按定长标识收紧）；device_id_hash 只进
--        search_outbox 的事件 payload，不落列，避免按设备维度可查。
--        保留期由清理任务按 ctime 截断（README 记录默认 90 天）。
--   3. search_outbox       —— 与 search_query_log 同一事务写入的领域事件
--        （search.query.v1，AGENTS.md §5 Outbox）。发布器接入前行停留在 state=0，
--        本服务只保证“事件已可靠产生”，不声称已投递（README「已知缺口」）。
--   4. search_hot_keyword  —— 热度快照表（投影，非事实源）：由离线/定时聚合任务
--        从 search_query_log 计算后 UpsertSnapshot + PruneStale 刷新；
--        本服务查询链路只读，快照为空时接口返回空列表且 snapshot_at=0，不编造榜单。
--   5. search_block_word   —— 屏蔽词字典（运营维护，写入口归 services/operation，
--        评审后接入）。本服务查询链路只读，命中即不查引擎。
--
-- 索引取舍：
--   - search_history 的联想前缀查询 ListByPrefix（mid + state + keyword LIKE 'x%'）
--     复用 uniq_mid_keyword 的最左前缀，单用户行数受 Prune 限制，无需额外前缀索引；
--     历史列表 ListByKeyset 走 idx_mid_state_mtime（等值 mid,state + mtime,id 有序）。
--   - 二级索引不写 keyword 全文：全文检索在 OpenSearch 侧完成，MySQL 不做 LIKE 全表扫。
--
-- 影响：仅新增本服务的 5 张表，不改动任何既有表、不改数据、不写 schema_migrations
--   （版本记录由 scripts/migrate.ps1 维护）。
-- 回滚：
--   DROP TABLE IF EXISTS `search_outbox`;
--   DROP TABLE IF EXISTS `search_block_word`;
--   DROP TABLE IF EXISTS `search_hot_keyword`;
--   DROP TABLE IF EXISTS `search_query_log`;
--   DROP TABLE IF EXISTS `search_history`;
--   （历史与日志含用户数据，回滚前必须确认已按隐私流程导出/销毁。）
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，无锁风险；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

-- 用户搜索历史（幂等 upsert，物理删除）
CREATE TABLE IF NOT EXISTS `search_history` (
  `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `mid`          BIGINT       NOT NULL DEFAULT 0 COMMENT '用户 ID（登录态，>0）',
  `keyword`      VARCHAR(128) NOT NULL DEFAULT '' COMMENT '规范化关键词（与引擎查询、缓存 key 同源）',
  `keyword_hash` CHAR(64)     NOT NULL DEFAULT '' COMMENT '关键词 sha256 hex（等值定位与聚合，避免长串比较）',
  `platform`     VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '最近一次来源端（android/ios/harmony/desktop/web）',
  `state`        TINYINT      NOT NULL DEFAULT 0 COMMENT '0 正常、1 运营标记、2 待清理（非 0 不下发客户端）',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '首次搜索时间（Unix 秒）',
  `mtime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次搜索时间（Unix 秒，列表按此倒序）',
  PRIMARY KEY (`id`),
  -- 幂等写入依赖：Upsert 用 ON DUPLICATE KEY UPDATE 只更新时间与端信息
  UNIQUE KEY `uniq_mid_keyword` (`mid`, `keyword`),
  -- 历史列表 keyset 分页：WHERE mid,state AND (mtime,id) 倒序
  KEY `idx_mid_state_mtime` (`mid`, `state`, `mtime`),
  -- 单条删除/查找按 (mid, keyword_hash) 精确定位
  KEY `idx_mid_keyword_hash` (`mid`, `keyword_hash`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='search-query 用户搜索历史（隐私数据，删除为物理删除）';

-- 查询行为日志摘要（ReportQuery 写入，热词聚合与实时大盘的事实源）
CREATE TABLE IF NOT EXISTS `search_query_log` (
  `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `query_id`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '上报幂等键（客户端/网关生成，唯一）',
  `mid`          BIGINT       NOT NULL DEFAULT 0 COMMENT '用户 ID，0 表示游客',
  `keyword`      VARCHAR(128) NOT NULL DEFAULT '' COMMENT '规范化关键词（搜索分析与推荐所需，不含敏感原文）',
  `keyword_hash` CHAR(64)     NOT NULL DEFAULT '' COMMENT '关键词 sha256 hex（热词聚合分组键）',
  `hit_count`    BIGINT       NOT NULL DEFAULT 0 COMMENT '命中数（引擎返回；降级/屏蔽时为 0）',
  `result_state` VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '结果状态：ok/empty/degraded/blocked',
  `latency_ms`   BIGINT       NOT NULL DEFAULT 0 COMMENT '引擎耗时（毫秒）',
  `platform`     VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '端标识',
  `app_version`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '客户端版本',
  `ip_hash`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '脱敏来源标识（禁止写原始 IP）',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，清理任务按此截断）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_query_id` (`query_id`),
  -- 热词聚合：按关键词哈希 + 时间窗统计（也供 Redis 计数重建）
  KEY `idx_keyword_hash_ctime` (`keyword_hash`, `ctime`),
  -- 用户维度分析与按时间清理
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='search-query 查询行为日志摘要（脱敏，事件与热词聚合事实源）';

-- 热词快照（投影表：由离线聚合任务刷新，查询链路只读）
CREATE TABLE IF NOT EXISTS `search_hot_keyword` (
  `id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `scope`       VARCHAR(64)  NOT NULL DEFAULT 'global' COMMENT '作用域：global 或 zone:<zone_id>',
  `keyword`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '热词（规范化后）',
  `score`       DOUBLE       NOT NULL DEFAULT 0 COMMENT '热度分（聚合窗口内加权搜索量，仅用于排序）',
  `snapshot_at` BIGINT       NOT NULL DEFAULT 0 COMMENT '本批快照生成时间（Unix 秒，0 表示无快照）',
  `ctime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_scope_keyword` (`scope`, `keyword`),
  -- 读取路径：WHERE scope=? ORDER BY score DESC, id ASC LIMIT n
  KEY `idx_scope_score` (`scope`, `score`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='search-query 热词快照（投影，可从 search_query_log 重算）';

-- 屏蔽词字典（运营维护，查询链路只读；命中即不查询引擎）
CREATE TABLE IF NOT EXISTS `search_block_word` (
  `id`       BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `word`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '屏蔽词（规范化后的完整关键词，等值匹配）',
  `state`    TINYINT      NOT NULL DEFAULT 0 COMMENT '0 生效、1 停用（停用保留行用于审计，不再生效）',
  `operator` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '维护人/来源系统（审计用）',
  `ctime`    BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`    BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_word` (`word`),
  -- 生效词字典刷新：WHERE state=? AND id>? ORDER BY id ASC LIMIT n
  KEY `idx_state_id` (`state`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='search-query 屏蔽词字典（安全过滤，等值命中）';

-- 领域事件 Outbox（与 search_query_log 同事务写入，发布器异步投递）
CREATE TABLE IF NOT EXISTS `search_outbox` (
  `id`              BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键（发布器按 id 升序投递，保证同库事件顺序）',
  `event_id`        CHAR(26)    NOT NULL DEFAULT '' COMMENT '事件唯一 ID（ULID，消费者按此幂等去重）',
  `event_type`      VARCHAR(64) NOT NULL DEFAULT '' COMMENT '事件类型（search.query）',
  `schema_version`  INT         NOT NULL DEFAULT 1 COMMENT 'payload schema 版本',
  `aggregate_type`  VARCHAR(64) NOT NULL DEFAULT '' COMMENT '聚合根类型（search_query）',
  `aggregate_id`    VARCHAR(64) NOT NULL DEFAULT '' COMMENT '聚合根 ID（query_id）',
  `payload`         MEDIUMTEXT  NOT NULL COMMENT '事件信封完整 JSON（common/eventenvelope.Envelope）',
  `state`           TINYINT     NOT NULL DEFAULT 0 COMMENT '发布状态：0 待发布、1 已发布、2 失败（超重试上限，人工处理）',
  `retry_count`     INT         NOT NULL DEFAULT 0 COMMENT '已重试次数（用于指数退避）',
  `next_retry_at`   BIGINT      NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒，0 表示可立即投递）',
  `occurred_at`     BIGINT      NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒）',
  `last_error`      VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次投递错误信息',
  `ctime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  -- 幂等回填：重复上报 query_id 时按聚合根取首次产生的 event_id
  KEY `idx_aggregate` (`aggregate_type`, `aggregate_id`),
  -- 发布器轮询：待发布 + 到期时间
  KEY `idx_state_next_retry` (`state`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='search-query 领域事件 Outbox（业务事务内写入，发布器异步投递）';
