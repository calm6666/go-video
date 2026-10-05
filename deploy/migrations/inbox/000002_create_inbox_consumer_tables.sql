-- =====================================================================
-- inbox 服务 - Kafka 事件消费幂等与死信
-- =====================================================================
-- owner：inbox 服务（数据所有者，AGENTS.md §5）。库：go_video_inbox。
-- 影响：新建 2 张表（消费状态机、死信），供 internal/consumer 使用；
--       不改动任何已有表，不写跨服务外键。
-- 回滚：DROP TABLE IF EXISTS `inbox_dead_letter`,`inbox_consumer_offset`;
--       删除后消费者会把历史事件当作首次投递，但 inbox_message 的
--       uniq_idempotency_key(evt:<event_id>) 仍然阻止重复投递，可安全回滚。
-- 锁风险：仅建表，无 ALTER，无锁风险；重复执行由 IF NOT EXISTS 兜底。
-- 命名说明：partition/offset 在 MySQL 中是保留字，这里用 partition_no /
--       msg_offset 表达 Kafka 分区与位点（kq 的 ConsumeHandler 不上报位点，
--       默认写 0，幂等真值始终是 event_id）。
-- 保留策略：payload 保存最近一次收到的原始事件信封，用于退避重投与进程崩溃后重放；
--       MarkSucceeded 会把 payload 置回 NULL，因此只有未终结（retry/processing）的行
--       持有原文。历史行（succeeded/dead_letter）由 services/cron 的清理任务按 ctime
--       过期删除（详见 services/inbox/README.md「留存与回收」），两张表都为此单列
--       idx_ctime：按 ctime 删除不会走 idx_topic_ctime / idx_state_ctime 的前缀。
-- =====================================================================

-- 消费状态机表：按 event_id 幂等去重，状态至少覆盖
-- received/processing/succeeded/retry/dead_letter（docs/api-and-events.md §6）。
CREATE TABLE IF NOT EXISTS `inbox_consumer_offset` (
  `id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `event_id`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件唯一 ID（幂等键，来自事件信封）',
  `event_type`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件类型，如 engagement.action',
  `topic`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '来源 topic，如 engagement.action.v1',
  `partition_no`  INT          NOT NULL DEFAULT 0 COMMENT 'Kafka 分区（kq 不上报时为 0）',
  `msg_offset`    BIGINT       NOT NULL DEFAULT 0 COMMENT 'Kafka 位点（kq 不上报时为 0）',
  `state`         VARCHAR(16)  NOT NULL DEFAULT 'received' COMMENT 'received/processing/succeeded/retry/dead_letter',
  `retry_count`   INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
  `next_retry_at` BIGINT       NOT NULL DEFAULT 0 COMMENT '下次可处理时间（Unix 秒，退避窗口）',
  `last_error`    VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次失败原因（截断保存，不含堆栈）',
  `payload`       TEXT         NULL COMMENT '最近一次收到的原始事件信封 JSON，供退避重投/崩溃重放；成功后置回 NULL',
  `occurred_at`   BIGINT       NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒，用于乱序/迟到判断）',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '首次收到时间（Unix 秒）',
  `mtime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '状态变更时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  KEY `idx_state_retry` (`state`, `next_retry_at`),
  KEY `idx_topic_ctime` (`topic`, `ctime`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='站内信消费者事件状态表：重复/乱序/迟到消息不得破坏最终状态';

-- 死信表：信封校验失败、topic 与 event_type 不匹配、payload 不可解析、
-- 或重试超过上限的事件在此留档，只保存摘要与脱敏预览，不保存原始 payload。
CREATE TABLE IF NOT EXISTS `inbox_dead_letter` (
  `id`             BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `event_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件 ID（信封不可解析时为空串）',
  `event_type`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件类型（信封不可解析时为空串）',
  `topic`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '来源 topic',
  `payload_digest` VARCHAR(128) NOT NULL DEFAULT '' COMMENT '原始消息体摘要：sha256:<hex>，用于重复留档去重',
  `payload_preview` VARCHAR(256) NOT NULL DEFAULT '' COMMENT '脱敏后的原始消息体前缀（数字串掩码，不含完整正文）',
  `reason`         VARCHAR(512) NOT NULL DEFAULT '' COMMENT '进入死信的原因（校验失败/重试超限/不支持的载荷）',
  `consumed_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '判死时间（Unix 秒）',
  `state`          VARCHAR(16)  NOT NULL DEFAULT 'open' COMMENT 'open/replayed/ignored，人工或重放任务维护',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_topic_digest` (`topic`, `payload_digest`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='站内信消费死信表：保留可审计摘要，不保留敏感原文';
