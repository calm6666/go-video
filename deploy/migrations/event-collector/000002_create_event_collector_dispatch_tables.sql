-- =====================================================================
-- event-collector 服务 - 采样/脱敏策略版本、投递 Outbox 与投递死信
-- =====================================================================
-- 用途：ec_dispatch_policy 管采样与脱敏配置版本；ec_pending_delivery 是 MQ 投递 Outbox；
--       ec_dead_letter 记录超过重试上限的事件摘要与人工处置审计。
-- 数据所有者：event-collector 服务（AGENTS.md §5）。库：go_video_event_collector。
--       spm/cron/operation 只能通过 gRPC EventCollector 读写这些语义，不得直连本库。
-- 关键不变量：
--   1) 同一时刻至多一版生效策略 —— 由 ec_dispatch_policy.uniq_active(active_flag) 保证：
--      只有 ACTIVE 行写 1，其余写 NULL（MySQL 唯一索引允许多个 NULL）。
--      切换必须在一个事务内「归档旧 ACTIVE + 激活新版本」，并发第二笔会撞唯一键而失败，
--      因此不存在「零个或两个 ACTIVE」的中间态（model.Activate 强制要求传事务 session）。
--   2) 一个事件对同一 topic 至多一条投递意图 —— uniq_event_topic(event_id, topic)，
--      重放与回填不会在 MQ 之前堆积重复行。
-- 回滚：DROP TABLE IF EXISTS `ec_dead_letter`, `ec_pending_delivery`, `ec_dispatch_policy`;
--       回滚前先执行：SELECT version FROM ec_dispatch_policy WHERE active_flag = 1;
--       并把该版本的完整参数抄进变更单——策略表是历史批次的归因依据，
--       ec_event_record.policy_version 会指向已不存在的版本，事后无法解释采样口径。
--       清理 Outbox 会丢未投递事件（台账仍在 ec_event_record，可据此重新入队）。
-- 锁风险：仅建表，无 ALTER，无锁风险；重复执行由 IF NOT EXISTS 兜底。
--       ec_pending_delivery 是「写入 + 频繁状态更新」表，容量按 事件数 × topic 数 预估；
--       已成功行由 DeleteSentBefore 按留存期瘦身，避免热表膨胀。
-- =====================================================================

-- 采样与脱敏策略版本表。
-- 版本语义：DRAFT 可反复改；ACTIVE 只读（要改必须新建版本再切换）；ARCHIVED 永久保留。
-- 隐私：salt_ref 只是取盐的环境变量名（如 EVENT_COLLECTOR_SALT_V2），盐值本身永不入库；
--       drop_fields 明确列出禁止入库的字段名（明文 ip/imei/phone/token 等）。
-- 参数即上限：max_* 列是采集与投递的服务端硬约束，逐批写入 ec_ingest_batch/ec_event_record
--       以保证「这条数据当初是按哪套规则收的」可查。
CREATE TABLE IF NOT EXISTS `ec_dispatch_policy` (
  `id`                       BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `version`                  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '语义化版本，如 2026.09.20-1',
  `state`                    TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 草稿、2 生效中、3 已下线（只读归因依据）',
  `active_flag`              TINYINT      NULL COMMENT '仅 ACTIVE 行为 1，其余 NULL：uniq_active 保证单生效版本',
  `sample_rules`             TEXT         NOT NULL COMMENT '采样规则 JSON 数组：[{event_type,sample_bps,quality_events}]，bps 0..10000',
  `salt_version`             INT          NOT NULL DEFAULT 0 COMMENT '脱敏哈希盐版本（轮换后旧数据不可逆推）',
  `salt_ref`                 VARCHAR(128) NOT NULL DEFAULT '' COMMENT '取盐的环境变量名，不含盐值本身',
  `field_whitelist`          TEXT         NOT NULL COMMENT 'payload 允许保留的字段名（JSON 数组）',
  `drop_fields`              TEXT         NOT NULL COMMENT '明确禁止入库的字段名（JSON 数组：明文 ip/imei/phone/token 等）',
  `max_events_per_batch`     INT          NOT NULL DEFAULT 200 COMMENT '单请求事件条数上限',
  `max_request_bytes`        BIGINT       NOT NULL DEFAULT 1048576 COMMENT '单请求字节上限',
  `max_event_payload_bytes`  INT          NOT NULL DEFAULT 8192 COMMENT '单事件 payload 字节上限',
  `max_clock_skew_seconds`   INT          NOT NULL DEFAULT 300 COMMENT '允许的时钟偏差绝对值（秒）',
  `max_backfill_seconds`     INT          NOT NULL DEFAULT 86400 COMMENT '允许的回补窗口（秒），过旧即拒绝',
  `keyword_max_runes`        INT          NOT NULL DEFAULT 64 COMMENT '搜索词截断长度（rune）',
  `retention_days`           INT          NOT NULL DEFAULT 30 COMMENT '接收台账保留天数（cron 清理）',
  `deliver_max_attempts`     INT          NOT NULL DEFAULT 8 COMMENT '投递重试上限，超过转 ec_dead_letter',
  `retry_base_seconds`       BIGINT       NOT NULL DEFAULT 5 COMMENT '退避基数：delay = base * 2^(attempts-1)',
  `retry_max_seconds`        BIGINT       NOT NULL DEFAULT 3600 COMMENT '退避上限',
  `note`                     VARCHAR(512) NOT NULL DEFAULT '' COMMENT '本版为什么改（审计说明）',
  `operator`                 VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最后修改人（服务账号或运营 ID）',
  `ctime`                    BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                    BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_version` (`version`),
  UNIQUE KEY `uniq_active` (`active_flag`),
  KEY `idx_state_id` (`state`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='采样/脱敏策略版本表：ACTIVE 至多一版（uniq_active），历史版本保留供归因';

-- 投递 Outbox 表：与「批次 + 事件台账」同事务写入，MQ 发送在事务外完成（AGENTS.md §5）。
-- 幂等：uniq_event_topic(event_id, topic)。至少一次投递，下游 spm 按 envelope event_id 去重。
-- 并发：lease_owner + lease_until 是租约，多实例并行推进时同一行只被一个持有者取走；
--       worker 崩溃后租约到期即可被接管，不会出现永久卡在 RETRYING 的行。
-- 退避：next_retry_at 由 model.NextRetryAt 按指数退避计算并封顶 retry_max_seconds。
-- 清理：state=SENT 的历史行由 DeleteSentBefore 按留存期删除，可重算（不影响事实）。
CREATE TABLE IF NOT EXISTS `ec_pending_delivery` (
  `id`                BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `event_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '台账事件 ID（ec_event_record.event_id）',
  `batch_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '所属批次（死信排查时回溯上报方）',
  `topic`             VARCHAR(128) NOT NULL DEFAULT '' COMMENT '投递目标 topic，如 behavior.play.v1',
  `envelope_event_id` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '信封 event_id（下游去重键，与入参分开记账）',
  `payload_digest`    VARCHAR(80)  NOT NULL DEFAULT '' COMMENT '正文摘要 sha256:<hex>（原文不入库）',
  `state`             TINYINT      NOT NULL DEFAULT 2 COMMENT '状态：1 不投递、2 待投递、3 已发送、4 退避中、5 死信',
  `attempts`          INT          NOT NULL DEFAULT 0 COMMENT '已尝试次数',
  `next_retry_at`     BIGINT       NOT NULL DEFAULT 0 COMMENT '下次可投递时间（Unix 秒）',
  `lease_owner`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '当前租约持有者（worker 标识），空表示未占用',
  `lease_until`       BIGINT       NOT NULL DEFAULT 0 COMMENT '租约到期时刻（Unix 秒，0 未占用）',
  `sent_at`           BIGINT       NOT NULL DEFAULT 0 COMMENT '投递成功时刻（Unix 秒，0 未成功）',
  `last_error`        VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次失败摘要（已脱敏）',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_topic` (`event_id`, `topic`),
  KEY `idx_claim` (`state`, `next_retry_at`, `id`),
  KEY `idx_lease` (`lease_owner`, `lease_until`),
  KEY `idx_topic_state` (`topic`, `state`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='MQ 投递 Outbox：与接收同事务落库，uniq_event_topic 幂等，租约防并发重复发送';

-- 投递死信表：超过重试上限的事件摘要与人工处置审计。
-- 事件原文不入库（只有 payload_digest），重放需要正文时按 event_id 回对象存储引用取件。
-- 处置语义：state=open 才能被 ReplayDeadLetter 迁移；replayed/discarded 是终态，
--       重复重放计入 skipped 而不是覆盖首次原因。
-- 审计：operator + replay_key + replay_reason 三列共同回答「谁、凭哪次请求、为什么重放」。
--       open 行永不自动清理（DeleteBefore 只删终态行），保留证据（AGENTS.md §8）。
CREATE TABLE IF NOT EXISTS `ec_dead_letter` (
  `id`             BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `event_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '台账事件 ID',
  `batch_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '所属批次',
  `event_type`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件类型',
  `topic`          VARCHAR(128) NOT NULL DEFAULT '' COMMENT '目标 topic',
  `payload_digest` VARCHAR(80)  NOT NULL DEFAULT '' COMMENT '正文摘要 sha256:<hex>，原文不入库',
  `reason`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '失败原因分类（稳定枚举串：mq_timeout/mq_auth/payload_oversize…）',
  `reason_detail`  VARCHAR(512) NOT NULL DEFAULT '' COMMENT '已脱敏的错误摘要',
  `attempts`       INT          NOT NULL DEFAULT 0 COMMENT '死信前的投递尝试次数',
  `state`          VARCHAR(16)  NOT NULL DEFAULT 'open' COMMENT '处置状态：open/replayed/discarded',
  `created_at`     BIGINT       NOT NULL DEFAULT 0 COMMENT '死信生成时刻（Unix 秒）',
  `handled_at`     BIGINT       NOT NULL DEFAULT 0 COMMENT '处置时刻（Unix 秒，0 未处置）',
  `operator`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '处置人（服务账号或运营 ID）',
  `replay_key`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '重放请求的 idempotency_key',
  `replay_reason`  VARCHAR(512) NOT NULL DEFAULT '' COMMENT '重放/废弃理由（必填，审计用）',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，翻页游标第 1 列）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_topic` (`event_id`, `topic`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  KEY `idx_topic_state` (`topic`, `state`, `ctime`),
  KEY `idx_ctime_id` (`ctime`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='投递死信与处置审计：uniq_event_topic 防重复死信；open 行只能人工处置后清理';
