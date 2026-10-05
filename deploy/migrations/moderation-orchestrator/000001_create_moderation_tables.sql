-- =====================================================================
-- moderation-orchestrator 服务 - 审核任务、规则、结论与申诉
-- =====================================================================
-- 数据所有者：moderation-orchestrator（AGENTS.md §5）。审核结论只能由本服务写入，
-- 其它服务不得绕过审核直接发布（AGENTS.md §8）。
-- 回滚：DROP TABLE IF EXISTS `moderation_task`,`moderation_rule`,`moderation_result`,`moderation_appeal`;
-- =====================================================================

-- 审核任务表
-- 幂等：唯一索引 (business, submission_id) 配合 INSERT ... ON DUPLICATE KEY UPDATE
--   id = LAST_INSERT_ID(id)，同一对象重复提审返回既有任务 ID 而不新增行。
CREATE TABLE IF NOT EXISTS `moderation_task` (
  `id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT '任务 ID（主键）',
  `submission_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '提交对象 ID（内容主键）',
  `content_type`  TINYINT      NOT NULL DEFAULT 0 COMMENT '内容类型（ContentType 枚举）',
  `mid`           BIGINT       NOT NULL DEFAULT 0 COMMENT '提交用户 ID',
  `up_mid`        BIGINT       NOT NULL DEFAULT 0 COMMENT 'UP 主 ID',
  `business`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务名（与 submission_id 共同定位对象）',
  `reason`        VARCHAR(500) NOT NULL DEFAULT '' COMMENT '提交审核原因',
  `state`         TINYINT      NOT NULL DEFAULT 0 COMMENT '任务状态（TaskState 枚举）',
  `operator`      BIGINT       NOT NULL DEFAULT 0 COMMENT '操作人（运营 ID，0 表示系统）',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_business_submission` (`business`, `submission_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_state_id` (`state`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='审核任务表';

-- 审核规则表：启用规则按 priority 降序匹配（ListEnabled 走 idx_state_priority）
-- 回滚：DROP TABLE IF EXISTS `moderation_rule`;
CREATE TABLE IF NOT EXISTS `moderation_rule` (
  `id`        BIGINT       NOT NULL AUTO_INCREMENT COMMENT '规则 ID（主键）',
  `name`      VARCHAR(128) NOT NULL DEFAULT '' COMMENT '规则名',
  `keywords`  VARCHAR(2000) NOT NULL DEFAULT '' COMMENT '关键词（逗号分隔，可空）',
  `model_id`  VARCHAR(128) NOT NULL DEFAULT '' COMMENT '模型 ID（机审模型版本，可空）',
  `priority`  INT          NOT NULL DEFAULT 0 COMMENT '优先级（越大越优先）',
  `action`    TINYINT      NOT NULL DEFAULT 0 COMMENT '命中后的结论动作（Verdict）',
  `state`     TINYINT      NOT NULL DEFAULT 1 COMMENT '0 禁用、1 启用',
  `operator`  BIGINT       NOT NULL DEFAULT 0 COMMENT '操作人（运营 ID）',
  `ctime`     BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`     BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  KEY `idx_state_priority` (`state`, `priority`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='审核规则表';

-- 审核结论表：唯一索引 (task_id) 保证一任务一结论，Upsert 幂等覆盖。
-- 回滚：DROP TABLE IF EXISTS `moderation_result`;
CREATE TABLE IF NOT EXISTS `moderation_result` (
  `id`       BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `task_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '关联审核任务 ID',
  `verdict`  TINYINT      NOT NULL DEFAULT 0 COMMENT '结论（Verdict 枚举）',
  `reason`   VARCHAR(500) NOT NULL DEFAULT '' COMMENT '结论原因（关键词命中、模型分数等）',
  `worker_id` BIGINT      NOT NULL DEFAULT 0 COMMENT 'worker 实例 ID（机审）',
  `reviewer` BIGINT       NOT NULL DEFAULT 0 COMMENT '人审员 ID（人审）',
  `ctime`    BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_task` (`task_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='审核结论表';

-- 申诉表：待处理申诉 state=0，处理后置 1 并记录最终结论与处理人。
-- 回滚：DROP TABLE IF EXISTS `moderation_appeal`;
CREATE TABLE IF NOT EXISTS `moderation_appeal` (
  `id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT '申诉 ID（主键）',
  `task_id`       BIGINT       NOT NULL DEFAULT 0 COMMENT '关联审核任务 ID',
  `mid`           BIGINT       NOT NULL DEFAULT 0 COMMENT '申诉人 ID',
  `content`       VARCHAR(1000) NOT NULL DEFAULT '' COMMENT '申诉理由',
  `final_verdict` TINYINT      NOT NULL DEFAULT 0 COMMENT '最终结论（处理后）',
  `final_reason`  VARCHAR(500) NOT NULL DEFAULT '' COMMENT '处理说明',
  `handler`       BIGINT       NOT NULL DEFAULT 0 COMMENT '处理人（运营 ID）',
  `state`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0 待处理、1 已处理',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '处理时间（Unix 秒）',
  PRIMARY KEY (`id`),
  KEY `idx_task_state` (`task_id`, `state`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_state_id` (`state`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='审核申诉表';
