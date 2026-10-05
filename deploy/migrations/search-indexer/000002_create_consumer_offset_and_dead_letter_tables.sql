-- =====================================================================
-- search-indexer 服务 - 事件消费流水表 / 死信登记表
-- =====================================================================
-- 数据所有者：search-indexer 服务（AGENTS.md §5）。事件生产者是 video / catalog /
--   rights（content.published.v1）与 engagement / danmaku / comment（engagement.action.v1），
--   它们只发事件，绝不读写本目录的表；本服务也不直连它们的库表（AGENTS.md §5 禁止项）。
--
-- 库名：go_video_search_indexer（与 000001 同库；建库由 scripts/migrate.ps1 按 DSN 执行）。
--
-- 表与写入路径（列名严格对应 model/consumeroffsetmodel.go、model/deadlettermodel.go）：
--   1. search_consumer_offset —— 消费流水 + 退避重试队列（SearchConsumerOffset，offsetColumns）。
--        · 去重：uniq_event_id + INSERT IGNORE，MarkReceived 以 RowsAffected=0 判 duplicate，
--          重复投递只生效一次且不覆盖既有状态（因此不写 last_write_wins）。
--        · 状态机：received → processing → succeeded / retry → dead_letter；
--          MarkProcessing 只接受 state IN ('received','retry')，MarkRetry 排除
--          state='dead_letter'，避免终态回退与死信复活。
--        · 断点续跑：retry 行保留 payload_json，进程重启后 RetrySweeper 按
--          next_retry_at 继续退避重试，不依赖 MQ 重投（消费端 offset 不由本表提交）。
--        · 终态清理：MarkSucceeded / MarkDeadLetter 把 payload_json 与 last_error 置空串，
--          防止业务原文长期堆积（AGENTS.md §7 隐私）。
--   2. search_dead_letter    —— 死信登记（SearchDeadLetter，deadLetterColumns）。
--        只存 payload 的 sha256 摘要前 32 hex（repository.PayloadDigest），不存原文；
--        重放按 event_id 回到源 topic 指定 offset（README「死信重放」）。
--        无法解析信封时用 "malformed_"+sha256[:24] 合成 event_id，保证坏消息可追溯。
--
-- 索引取舍：
--   - search_consumer_offset.idx_state_next_retry 服务 ListDueForRetry
--     （WHERE state='retry' AND next_retry_at<=? ORDER BY next_retry_at ASC LIMIT ?），
--     最左前缀 state 同时覆盖 CountByState（积压告警）。
--   - 按分区/位点回放的定位走 uniq_event_id，不建 (topic,partition_no,offset_no) 索引：
--     代码中没有该查询路径，避免为不存在的查询付写入代价。
--   - search_dead_letter.idx_state_id 服务 ListOpen（WHERE state='open' ORDER BY id ASC）；
--     idx_state_ctime 服务超过 model.DLQRetentionDays 的归档清理（services/cron 执行）。
--
-- 影响：仅新增本服务的 2 张表，不改动既有表、不改数据、不写 schema_migrations；
--   不建跨服务外键（event_id 只做业务主键引用）。
-- 回滚：
--   DROP TABLE IF EXISTS `search_dead_letter`;
--   DROP TABLE IF EXISTS `search_consumer_offset`;
--   回滚前必须停止消费者进程：丢失去重流水后，MQ 重投会被当作首次事件再次写入索引
--   （doc_revision 守卫可保证不被旧数据覆盖，但死信与积压监控会失去历史）。
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行；payload_json 为 MEDIUMTEXT，
--   终态置空由 UPDATE 完成，后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

-- 事件消费流水（去重 + 退避重试队列，状态机见 model.OffsetState*）
CREATE TABLE IF NOT EXISTS `search_consumer_offset` (
  `id`            BIGINT        NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `event_id`      VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '事件 ID（eventenvelope.Envelope.event_id，ULID 26 字符；无法解析时存 malformed_+摘要）',
  `event_type`    VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '事件类型：content.published / engagement.action',
  `topic`         VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '版本化来源 topic（eventenvelope.Topic 推导，如 content.published.v1；推导失败回退投递 topic）',
  `partition_no`  INT           NOT NULL DEFAULT 0 COMMENT 'MQ 分区号（仅排障定位，不参与幂等判定）',
  `offset_no`     BIGINT        NOT NULL DEFAULT 0 COMMENT 'MQ 位点（同上，位点提交由读取端适配器负责）',
  `state`         VARCHAR(16)   NOT NULL DEFAULT 'received' COMMENT '状态机：received/processing/succeeded/retry/dead_letter',
  `retry_count`   INT           NOT NULL DEFAULT 0 COMMENT '累计失败次数（含进程内即时重试，达到 Kafka.MaxRetries 转死信）',
  `next_retry_at` BIGINT        NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒，指数退避结果；终态清 0）',
  `last_error`    VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '最近一次失败摘要（formatAttemptError/sanitizeError 截断 500 字符，不含堆栈与凭据）',
  `occurred_at`   BIGINT        NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒，信封 occurred_at；解析失败退回库时间）',
  `payload_json`  MEDIUMTEXT    NOT NULL COMMENT '重投所需事件信封原文（生产者已脱敏，不含手机号/身份证/Token/IP；终态置空串）',
  `ctime`         BIGINT        NOT NULL DEFAULT 0 COMMENT '入库时间（Unix 秒）',
  `mtime`         BIGINT        NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒，每次状态推进刷新）',
  PRIMARY KEY (`id`),
  -- 幂等去重：MarkReceived 的 INSERT IGNORE 与所有状态推进都以 event_id 定位
  UNIQUE KEY `uniq_event_id` (`event_id`),
  -- 到期重试扫描：WHERE state='retry' AND next_retry_at<=? ORDER BY next_retry_at ASC
  -- 最左前缀 state 兼作 CountByState 的积压统计
  KEY `idx_state_next_retry` (`state`, `next_retry_at`),
  -- 排障：按事件类型看某段时间的投递与失败（运维定位上游批量重投）
  KEY `idx_type_ctime` (`event_type`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='search-indexer 事件消费流水（event_id 去重 + 退避重试队列，非 MQ 位点存储）';

-- 死信登记（重试用尽或永久错误，等待人工/工具重放；只存摘要不存原文）
CREATE TABLE IF NOT EXISTS `search_dead_letter` (
  `id`             BIGINT        NOT NULL AUTO_INCREMENT COMMENT '自增主键（ListOpen 按此升序重放，保持上游事件顺序）',
  `event_id`       VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '事件 ID（与 search_consumer_offset.event_id 同值，无法解析信封时为 malformed_+摘要）',
  `event_type`     VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '事件类型（解析失败时为空串）',
  `topic`          VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '来源 topic（重放回投目标）',
  `payload_digest` CHAR(32)      NOT NULL DEFAULT '' COMMENT 'payload 摘要（sha256 hex 前 32 字符，比对与排障用，不存原文）',
  `reason`         VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '死信原因（脱敏截断，不含堆栈与密钥）',
  `state`          VARCHAR(16)   NOT NULL DEFAULT 'open' COMMENT '处理状态：open 待重放/replayed 已重放/discarded 已确认丢弃（保留行以支撑审计）',
  `ctime`          BIGINT        NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，超 model.DLQRetentionDays 的终态行由 services/cron 归档后清理）',
  `mtime`          BIGINT        NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  -- 同一事件只登记一次：INSERT IGNORE 命中此键即返回 existed=true，不覆盖首次原因
  UNIQUE KEY `uniq_event_id` (`event_id`),
  -- 运维重放队列：WHERE state='open' ORDER BY id ASC LIMIT ?
  KEY `idx_state_id` (`state`, `id`),
  -- 保留期清理：WHERE state IN ('replayed','discarded') AND ctime<? 归档后删除
  KEY `idx_state_ctime` (`state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='search-indexer 死信登记（永久失败与超重试上限事件，仅存摘要可审计）';
