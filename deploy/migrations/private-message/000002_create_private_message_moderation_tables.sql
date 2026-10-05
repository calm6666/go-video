-- =====================================================================
-- private-message 服务 - 反骚扰偏好、举报与撤回审计
-- =====================================================================
-- owner：private-message 服务（数据所有者，AGENTS.md §5）。库：go_video_private_message。
-- 影响：新建 3 张表（用户私信偏好、举报事实、撤回流水），不改动任何已有表；
--       黑名单/关注关系的真值在 social-graph，风控名单在 risk-control，
--       本域只保存「本人意愿」与「谁举报了哪条消息」，不复制对方主数据。
-- 回滚：DROP TABLE IF EXISTS `pm_withdraw_log`,`pm_report`,`pm_user_setting`;
--       pm_withdraw_log 是处置证据，回滚前必须完成审计导出（AGENTS.md §8/§9）；
--       pm_user_setting 缺失时按 DefaultUserSetting 兜底，回滚不阻断私信主链路。
-- 锁风险：仅建表，无 ALTER，无锁风险；重复执行由 IF NOT EXISTS 兜底。
-- =====================================================================

-- 用户私信偏好表：一个用户一行（mid 即主键），是反骚扰判定的本地唯一配置来源。
-- 判定顺序（logic 侧，README 有完整表格）：黑名单 → 接收范围门槛 → 风控陌生人限制 →
-- 会话冻结 → 频控。三条链路（发送、会话列表、未读汇总）共用同一过滤器，禁止只在发送侧拦截。
CREATE TABLE IF NOT EXISTS `pm_user_setting` (
  `mid`               BIGINT  NOT NULL COMMENT '用户 mid（主键，即 account 主键引用）',
  `allow_from`        TINYINT NOT NULL DEFAULT 1 COMMENT '接收范围：1 所有人、2 仅我关注、3 仅互相关注、4 关闭私信',
  `reject_stranger`   TINYINT NOT NULL DEFAULT 0 COMMENT '是否拒收非互关陌生人首条消息：0 否、1 是（互关后自动放行）',
  `keyword_filter`    TINYINT NOT NULL DEFAULT 1 COMMENT '是否启用关键词过滤（命中即送审/拦截）：0 关闭、1 开启',
  `mute_conversation` TINYINT NOT NULL DEFAULT 0 COMMENT '会话免打扰：0 正常提醒、1 只落库不推送',
  `created_at`        BIGINT  NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `updated_at`        BIGINT  NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`mid`),
  KEY `idx_updated_at` (`updated_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='用户私信偏好表：缺失行按 DefaultUserSetting 处理（保守拒绝陌生人由站点级配置决定）';

-- 举报事实表：只记录「谁举报了哪条消息」，不直连 moderation 库（AGENTS.md §5）。
-- 送审通过 moderation-orchestrator RPC 拿 audit_task_id；审核结论只由
-- ApplyModerationVerdict（moderation-orchestrator 侧调用）推进本域状态，
-- 因此「谁写的结论」在契约上是单一入口，避免双写。
-- 幂等：uniq_msg_reporter 保证同一举报人对同一消息只留一条；
--       handle_idempotency_key 可空唯一索引（MySQL 下 NULL 不参与唯一性判定），
--       未处置为 NULL，处置后写入调用方幂等键，重复提交直接回放首次结果。
CREATE TABLE IF NOT EXISTS `pm_report` (
  `report_id`              BIGINT       NOT NULL AUTO_INCREMENT COMMENT '举报单 ID（主键）',
  `conversation_id`        BIGINT       NOT NULL DEFAULT 0 COMMENT '会话 ID（冗余快照，运营侧免 JOIN）',
  `msg_id`                 BIGINT       NOT NULL DEFAULT 0 COMMENT '被举报消息 ID（pm_message.msg_id）',
  `reporter_mid`           BIGINT       NOT NULL DEFAULT 0 COMMENT '举报人 mid',
  `target_mid`             BIGINT       NOT NULL DEFAULT 0 COMMENT '被举报人 mid（通常是消息发送方）',
  `reason`                 INT          NOT NULL DEFAULT 0 COMMENT '举报原因码（稳定枚举，由 gateway/客户端约定，禁止复用为自由文本）',
  `description`            VARCHAR(500) NOT NULL DEFAULT '' COMMENT '举报补充描述（用户输入，展示前需脱敏与长度截断）',
  `state`                  TINYINT      NOT NULL DEFAULT 1 COMMENT '处理状态：1 待处理、2 已处理、3 已驳回',
  `audit_task_id`          BIGINT       NOT NULL DEFAULT 0 COMMENT '送审任务 ID（moderation-orchestrator 主键引用，0 表示未送审）',
  `handler`                BIGINT       NOT NULL DEFAULT 0 COMMENT '处置人 mid（运营；0 表示未处置或系统自动处置）',
  `handle_note`            VARCHAR(500) NOT NULL DEFAULT '' COMMENT '处置备注（审计，禁止粘贴私信正文）',
  `handle_idempotency_key` VARCHAR(128) DEFAULT NULL COMMENT '处置幂等键（未处置为 NULL，处置后唯一）',
  `trace_id`               VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID（只存 ID，不存内容）',
  `ctime`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '举报时间（Unix 秒）',
  `mtime`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '最近状态变更时间（Unix 秒）',
  PRIMARY KEY (`report_id`),
  UNIQUE KEY `uniq_msg_reporter` (`msg_id`, `reporter_mid`),
  UNIQUE KEY `uniq_handle_key` (`handle_idempotency_key`),
  KEY `idx_state_report_id` (`state`, `report_id`),
  KEY `idx_target_ctime` (`target_mid`, `ctime`),
  KEY `idx_conversation` (`conversation_id`),
  KEY `idx_audit_task` (`audit_task_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='私信举报事实表：审核结论由 moderation-orchestrator 单入口写回，本表不复制审核结论细节';

-- 撤回审计流水表：只追加、不改写、不删除（AGENTS.md §8「删除与撤回必须留证」）。
-- 即使 pm_message.content_cipher 已按留存策略物理清除，撤回处置链依旧可追溯。
-- reason 只允许写脱敏描述（命中规则、举报单号），禁止写私信正文。
CREATE TABLE IF NOT EXISTS `pm_withdraw_log` (
  `log_id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT '流水 ID（主键）',
  `msg_id`          BIGINT       NOT NULL DEFAULT 0 COMMENT '被撤回消息 ID',
  `conversation_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '会话 ID（冗余，便于按会话回溯处置）',
  `seq`             BIGINT       NOT NULL DEFAULT 0 COMMENT '消息 seq 快照（消息行被清理后仍可定位位置）',
  `sender_mid`      BIGINT       NOT NULL DEFAULT 0 COMMENT '原发送方 mid',
  `operator_mid`    BIGINT       NOT NULL DEFAULT 0 COMMENT '操作者 mid：发送者/接收者本人，或处置运营 mid',
  `source`          TINYINT      NOT NULL DEFAULT 1 COMMENT '撤回来源：1 发送者限时撤回、2 接收方撤回、3 审核结论、4 运营处置',
  `reason`          VARCHAR(255) NOT NULL DEFAULT '' COMMENT '脱敏原因（命中规则 ID、举报单号），禁止写正文',
  `audit_task_id`   BIGINT       NOT NULL DEFAULT 0 COMMENT '关联审核任务 ID（source=3 时必填）',
  `report_id`       BIGINT       NOT NULL DEFAULT 0 COMMENT '关联举报单 ID（source=3/4 时可选）',
  `ctime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '撤回时间（Unix 秒）',
  PRIMARY KEY (`log_id`),
  KEY `idx_msg` (`msg_id`, `log_id`),
  KEY `idx_operator_ctime` (`operator_mid`, `ctime`),
  KEY `idx_conversation` (`conversation_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='私信撤回审计流水表：append-only，留存期长于消息正文（正文到期清理、留证不清理）';
