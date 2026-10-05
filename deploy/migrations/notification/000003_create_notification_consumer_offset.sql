-- =====================================================================
-- notification 服务 - 事件消费位点与幂等登记表
-- =====================================================================
-- 数据所有者：notification 服务（AGENTS.md §5）。
-- 用途：消费 notification.request.v1 时按 event_id 去重，状态至少覆盖
--       received/processing/succeeded/retry/dead_letter（docs/api-and-events.md §6）。
-- 说明：kq 由 Kafka 消费组自动提交位点，不透传分区/位点给 handler，
--       因此 partition_no/offset_no 仅作留档字段（默认 0），最终一致性依赖本表状态机。
-- payload_json：按 docs/api-and-events.md §6，消费失败要能退避重试与人工重投，
--       因此暂存原始信封用于重放（不依赖 Kafka 是否再次投递同一消息）；
--       事件进入 succeeded 终态时由应用立即清空，死信留档只保留摘要（见 000004）。
--       信封本身按契约不得包含明文手机号/邮箱（docs/api-and-events.md §4）。
-- 影响：新建表；重复、乱序或迟到消息不会破坏终态（主键 + 源状态守卫更新）。
-- 锁风险：CREATE TABLE IF NOT EXISTS，仅元数据锁，可在线执行。
-- 回滚：DROP TABLE IF EXISTS `notification_consumer_offset`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `notification_consumer_offset` (
  `event_id`      VARCHAR(64)  NOT NULL COMMENT '事件 ID（去重键）',
  `event_type`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件类型',
  `topic`         VARCHAR(128) NOT NULL DEFAULT '' COMMENT '来源 topic',
  `partition_no`  INT          NOT NULL DEFAULT 0 COMMENT '分区号（留档，0 表示未透传）',
  `offset_no`     BIGINT       NOT NULL DEFAULT 0 COMMENT '位点（留档，0 表示未透传）',
  `state`         TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 received、2 processing、3 succeeded、4 retry、5 dead_letter',
  `retry_count`   INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
  `next_retry_at` BIGINT       NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒），0 表示不再重试',
  `last_error`    VARCHAR(500) NOT NULL DEFAULT '' COMMENT '最近一次错误（脱敏）',
  `occurred_at`   BIGINT       NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒）',
  `body_digest`   CHAR(64)     NOT NULL DEFAULT '' COMMENT '原始报文 sha256 hex',
  `payload_json`  TEXT         NOT NULL COMMENT '原始信封（retry/dead_letter 重放用，成功后清空）',
  `trace_id`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路 ID',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`event_id`),
  KEY `idx_state_retry` (`state`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='notification.request.v1 消费状态表';
