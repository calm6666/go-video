-- =====================================================================
-- risk-control 服务 - 决策三表：规则 / 名单 / 处罚
-- =====================================================================
-- 数据库：go_video_risk_control（取自 services/risk-control/etc/riskcontrol.v1.yaml 的
--   DataSource；建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
-- 数据所有者：risk-control 服务（AGENTS.md §5）。其他服务只能通过 RiskControl RPC 读写，
--   禁止直连本库；内容安全结论在 moderation-orchestrator 库，本库只存行为/账号风控事实。
--
-- 表与代码对应关系（列名严格取自 services/risk-control/model/*.go 的 db tag 与 SQL 字符串）：
--   * risk_rule        model/rule.go RiskRule        —— 可解释规则，含 version（决策日志按 rule_id@version 回放）
--   * risk_list        model/list.go RiskList        —— 黑/白名单，target_value 只存受控值（mid 十进制串或摘要）
--   * risk_punishment  model/punishment.go RiskPunishment —— 处罚状态机 ACTIVE→LIFTED/EXPIRED
--
-- 唯一键与幂等设计：
--   1. risk_rule.uniq_name：规则名全局唯一，UpsertRule 的新建分支靠它拒绝同名并发写入
--      （model.isDuplicateEntry 捕获 1062 后转 ErrRuleNameDuplicated）。
--   2. risk_list.uniq_target：(list_type, target_type, target_value) 唯一，是
--      Upsert 的 ON DUPLICATE KEY UPDATE 锚点；同一目标在黑名单与白名单可各存一行，
--      裁决时按「黑名单优先」分流，因此 list_type 必须在唯一键内。
--   3. risk_punishment.uniq_idempotency_key：ApplyPunishment 的幂等锚点，
--      INSERT ... ON DUPLICATE KEY UPDATE punishment_id = LAST_INSERT_ID(punishment_id)
--      依赖该唯一索引，运营重试同一 idempotency_key 返回既有处罚而不是再下发一条。
--      注意：(mid, scope) 不建唯一索引——终态（LIFTED/EXPIRED）记录必须保留作审计，
--      同一维度会有多条历史行；「同时只允许一条 ACTIVE」由 ApplyPunishment 先查后写保证。
--
-- 敏感信息约束（AGENTS.md §7）：target_value / device 类值只存受控 ID 与摘要，
--   不存明文 IP、手机号、设备号原文；reason 是运营内部说明，不下发终端。
--
-- 索引取自真实查询路径：
--   * risk_rule：ListActiveByAction(state=1 AND action_type IN (0,action) ORDER BY priority DESC,rule_id)
--     与 List(按 action_type/metric/state 过滤，rule_id DESC 分页)、CountAll(state)。
--   * risk_list：FindActive(state + expire_at + (target_type,target_value))、
--     FindOne(唯一键)、List(list_type/target_type/target_value/state 过滤 + id DESC 分页)。
--   * risk_punishment：ListActiveByMid(mid,state,start_at/end_at)、ExpireStale(mid,state,end_at)、
--     List(mid/scope/state 过滤 + punishment_id DESC 分页)、FindByIDempotencyKey(唯一键)。
--
-- 回滚（本期只建表，DROP 即可完全回滚；表内数据是风控配置与处罚史，DROP 前必须先备份）：
--   DROP TABLE IF EXISTS `risk_punishment`;
--   DROP TABLE IF EXISTS `risk_list`;
--   DROP TABLE IF EXISTS `risk_rule`;
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不触碰既有表；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `risk_rule` (
  `rule_id`        BIGINT       NOT NULL AUTO_INCREMENT COMMENT '规则 ID（主键）',
  `name`           VARCHAR(128) NOT NULL COMMENT '规则名，全局唯一（决策日志与运营展示用）',
  `action_type`    TINYINT      NOT NULL DEFAULT 0 COMMENT '适用动作：0 全部动作、1 投稿、2 评论、3 弹幕、4 关注、5 登录、6 改名、7 直播开播',
  `metric`         VARCHAR(32)  NOT NULL COMMENT '指标名：action_count/device_action_count/ip_action_count/device_risk_score/device_mid_count（须与 model.SupportedMetrics 一致）',
  `op`             TINYINT      NOT NULL DEFAULT 1 COMMENT '比较符：1 >、2 >=、3 <、4 <=、5 =',
  `threshold`      BIGINT       NOT NULL DEFAULT 0 COMMENT '阈值（LT/LTE 时必须 > 0，由代码校验）',
  `window_seconds` INT          NOT NULL DEFAULT 60 COMMENT '统计窗口（秒），取值 1-86400；超出 Redis 计数档位上限时规则按不可观测跳过',
  `decision`       TINYINT      NOT NULL DEFAULT 2 COMMENT '命中裁决：2 CHALLENGE、3 BLOCK、4 REVIEW（不允许 1 ALLOW）',
  `priority`       INT          NOT NULL DEFAULT 0 COMMENT '优先级，越大越先出现在 hit_rule_ids',
  `state`          TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 启用、0 停用（停用保留行便于规则回放）',
  `version`        INT          NOT NULL DEFAULT 1 COMMENT '规则版本，评估字段变更即 +1，决策日志记 rule_id@version',
  `operator`       BIGINT       NOT NULL DEFAULT 0 COMMENT '最近变更的运营 ID（审计必填，代码侧 ErrOperatorRequired）',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`rule_id`),
  UNIQUE KEY `uniq_name` (`name`),
  KEY `idx_action_state_priority` (`state`, `action_type`, `priority`, `rule_id`),
  KEY `idx_metric_state` (`metric`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='风控规则表（可解释决策的唯一规则源，仅 risk-control 读写）';

-- 回滚：DROP TABLE IF EXISTS `risk_list`;
CREATE TABLE IF NOT EXISTS `risk_list` (
  `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `list_type`    TINYINT      NOT NULL DEFAULT 1 COMMENT '名单类型：1 黑名单（命中即 BLOCK）、2 白名单（跳过规则评估）',
  `target_type`  TINYINT      NOT NULL DEFAULT 1 COMMENT '目标类型：1 账号 mid、2 设备受控 ID（sha256 摘要）、3 调用方预哈希 IP',
  `target_value` VARCHAR(64)  NOT NULL COMMENT '目标受控值：mid 十进制串或 8-64 位十六进制摘要，禁止裸 IP（model.NormalizeTargetValue 校验）',
  `reason`       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '运营内部说明，不下发终端',
  `operator`     BIGINT       NOT NULL DEFAULT 0 COMMENT '写入人运营 ID（必填）',
  `expire_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '到期时间（Unix 秒），0 表示永久',
  `state`        TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 生效、0 停用（停用保留行便于审计）',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_target` (`list_type`, `target_type`, `target_value`),
  KEY `idx_target_state` (`target_type`, `target_value`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='风控名单表（黑/白名单，一次查询按 (target_type,target_value) 命中三维度）';

-- 回滚：DROP TABLE IF EXISTS `risk_punishment`;
CREATE TABLE IF NOT EXISTS `risk_punishment` (
  `punishment_id`   BIGINT      NOT NULL AUTO_INCREMENT COMMENT '处罚 ID（主键）',
  `mid`             BIGINT      NOT NULL COMMENT '被处罚账号 ID',
  `scope`           TINYINT     NOT NULL DEFAULT 0 COMMENT '生效动作范围：0 全域、1-7 同 GuardedAction 枚举',
  `decision`        TINYINT     NOT NULL DEFAULT 3 COMMENT '生效裁决：2 CHALLENGE、3 BLOCK、4 REVIEW（引擎按 clampPunishmentDecision 处理非法值）',
  `reason`          VARCHAR(255) NOT NULL DEFAULT '' COMMENT '运营内部说明，禁止下发终端',
  `reason_code`     VARCHAR(64) NOT NULL DEFAULT '' COMMENT '面向端的稳定原因码（客户端按平台渲染文案）',
  `operator`        BIGINT      NOT NULL DEFAULT 0 COMMENT '下发人运营 ID（必填）',
  `start_at`        BIGINT      NOT NULL DEFAULT 0 COMMENT '生效时间（Unix 秒），允许运营设定未来生效',
  `end_at`          BIGINT      NOT NULL DEFAULT 0 COMMENT '到期时间（Unix 秒），0 表示永久；非 0 时必须 > start_at',
  `state`           TINYINT     NOT NULL DEFAULT 1 COMMENT '状态：1 ACTIVE 生效中、2 LIFTED 已解除、3 EXPIRED 已过期（终态，读时惰性推进 + cron 归档）',
  `idempotency_key` VARCHAR(64) NOT NULL COMMENT '幂等键（唯一），运营重试返回既有处罚',
  `lift_operator`   BIGINT      NOT NULL DEFAULT 0 COMMENT '解除人（0 表示未解除或系统过期）',
  `lift_reason`     VARCHAR(255) NOT NULL DEFAULT '' COMMENT '解除说明',
  `ctime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`punishment_id`),
  UNIQUE KEY `uniq_idempotency_key` (`idempotency_key`),
  KEY `idx_mid_state_start` (`mid`, `state`, `start_at`, `punishment_id`),
  KEY `idx_state_end` (`state`, `end_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='风控处罚表（ACTIVE→LIFTED/EXPIRED 状态机，裁决在黑名单之后、白名单之前生效）';
