-- =====================================================================
-- notification 服务 - 死信留档表
-- =====================================================================
-- 数据所有者：notification 服务（AGENTS.md §5）。
-- 用途：事件重试耗尽（source=event）与投递任务重试耗尽（source=delivery）统一留档，
--       供运营侧 ListDeadLetters/RetryDeadLetter 使用。
-- 隐私：只存报文摘要与原因，不存原始内容，避免敏感信息长期留档；
--       事件死信需由生产者按 event_id 重放 Outbox，投递死信可直接重投任务。
-- 影响：新建表；唯一索引 (source, event_id, delivery_id) 保证重复登记幂等。
-- 锁风险：CREATE TABLE IF NOT EXISTS，仅元数据锁，可在线执行。
-- 回滚：DROP TABLE IF EXISTS `notification_dead_letter`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `notification_dead_letter` (
  `id`             BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `event_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件 ID（source=event 时必填）',
  `event_type`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件类型',
  `topic`          VARCHAR(128) NOT NULL DEFAULT '' COMMENT '来源 topic；RPC 直投写 rpc:send',
  `source`         VARCHAR(16)  NOT NULL DEFAULT 'event' COMMENT '来源：event|delivery',
  `delivery_id`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '关联投递任务 ID（source=delivery 时必填）',
  `payload_digest` CHAR(64)     NOT NULL DEFAULT '' COMMENT '原始报文/渲染结果 sha256 hex',
  `reason`         VARCHAR(500) NOT NULL DEFAULT '' COMMENT '死信原因（脱敏）',
  `state`          TINYINT      NOT NULL DEFAULT 1 COMMENT '处置状态：1 pending、2 retried、3 discarded',
  `operator`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '处置人',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_source_event_delivery` (`source`, `event_id`, `delivery_id`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  KEY `idx_delivery` (`delivery_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='通知死信留档表';
