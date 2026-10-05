-- =====================================================================
-- user-profile 服务 - 领域事件 Outbox 表迁移
-- =====================================================================
-- 用途：按 AGENTS.md §5 的 Outbox 模式承载领域事件。业务写操作与
--   事件记录在同一事务内提交，由独立发布器轮询投递（消费者按 event_id
--   幂等，失败指数退避重试，超过上限标记失败转人工处理）。
-- 事件类型：
--   user.profile.updated  资料更新 → 发布器调用 account 服务的 DelCache RPC
--                         失效 account 侧缓存（对应参考仓库 databus 的
--                         MemberService-AccountNotify 主题，本项目服务间只用 RPC）
--   user.moral.notice     节操阈值通知 → notification 服务（待接入）
-- 数据所有者：user-profile 服务。
-- 回滚：DROP TABLE IF EXISTS member_outbox;
-- 锁风险：仅建表；uk_event_id/idx_status_next_retry 空库建索引无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `member_outbox`
(
    -- id 自增主键（发布器按 id 升序发布，保证事件顺序）
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（发布器按 id 升序发布，保证顺序）',

    -- event_id 事件唯一 ID（ULID，common/eventenvelope 生成）
    `event_id` CHAR(26) NOT NULL DEFAULT '' COMMENT '事件唯一 ID（ULID，消费者按此幂等去重）',

    -- event_type 事件类型：user.profile.updated / user.moral.notice
    `event_type` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '事件类型（user.profile.updated / user.moral.notice）',

    -- aggregate_id 聚合根 ID（mid 十进制字符串）
    `aggregate_id` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '聚合根 ID（mid 十进制字符串）',

    -- payload 事件信封完整 JSON（common/eventenvelope.Envelope：含 schema_version 等）
    `payload` MEDIUMTEXT NOT NULL COMMENT '事件信封完整 JSON（eventenvelope.Envelope，含 schema_version）',

    -- status 发布状态：0 待发布、1 已发布、2 失败（超最大重试，人工处理）
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '发布状态：0 待发布、1 已发布、2 失败（超最大重试，人工处理）',

    -- attempts 已投递次数（每次失败 +1，用于退避计算）
    `attempts` INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '已投递次数（失败 +1，用于指数退避计算）',

    -- next_retry_at 下次重试时间（Unix 秒，0 表示可立即投递）
    `next_retry_at` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒，0 表示可立即投递）',

    -- last_error 最近一次投递错误信息
    `last_error` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次投递错误信息',

    -- created_at 创建时间（Unix 秒）
    `created_at` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',

    -- published_at 发布时间（Unix 秒，未发布为 0）
    `published_at` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '发布时间（Unix 秒，未发布为 0）',

    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_event_id` (`event_id`),
    -- 发布器轮询索引：待发布 + 到期时间
    KEY `idx_status_next_retry` (`status`, `next_retry_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务领域事件 Outbox 表：业务事务内写入，发布器异步投递（幂等、退避重试、死信人工处理）。';
