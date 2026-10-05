-- =====================================================================
-- operation 服务 - 运营配置与管理任务编排
-- =====================================================================
-- 数据库：go_video_operation（见 services/operation/etc/operation.v1.yaml 的 DataSource）。
-- 数据所有者：operation 服务。
--   * op_config 本期由 operation 持有；services/ops-config 落地时以「迁移 + RPC」接管，
--     调用方契约不变（见 services/operation/README.md「边界与后续工作」）。
--   * op_admin_task / op_admin_task_step 只描述「要编排哪些下游动作」，真实写入由
--     video / catalog / rights / moderation-orchestrator 的公开 RPC 完成，
--     本库绝不复制它们的稿件、目录、版权窗口或审核结论（AGENTS.md §5）。
--   * 本服务不内置分发 worker：推进入口是 RunAdminTask RPC，由 services/cron 或人工触发。
--
-- 表与代码对应关系（列名严格取自 services/operation/model/*.go 的 db tag 与 SQL 字符串）：
--   * op_config            model/config.go OpsConfig      —— 运营配置项（cfg_key + scope，version 乐观锁）
--   * op_admin_task        model/task.go AdminTask        —— 批量任务主体（request_id 幂等）
--   * op_admin_task_step   model/task.go AdminTaskStep    —— 任务步骤（一步一目标聚合）
--
-- 唯一键与幂等设计：
--   1. op_config.uniq_cfg_key_scope：OpsConfig.Insert 用
--      `INSERT ... ON DUPLICATE KEY UPDATE mtime = mtime` + RowsAffected==0 判定为
--      model.ErrConfigExists，repository 再转成「请带 expect_version 重试」的冲突错误。
--      version 列是乐观锁依据：UpdateWithVersion 的 WHERE 带 version = ?，
--      未命中行时返回 updated=false → model.ErrConfigVersionConflict（不依赖 driver 错误码）。
--   2. op_admin_task.uniq_request_id：SubmitAdminTask 强制要求 OpContext.request_id，
--      同一 request_id 重放只会存在一个任务（repository 命中 ErrTaskExists 后回查并 reused=true）。
--   3. op_admin_task_step.uniq_task_step (task_id, step_no)：步骤号由 repository 连续生成，
--      唯一键是「重试提交不会写出两份步骤」的最后防线；GetAdminTask 的明细顺序即按 step_no。
--
-- 商业化范围约束（AGENTS.md §1）：op_config 只承载内容展示、审核阈值、灰度开关等运营参数，
--   不写会员/订单/支付/投币/广告投放相关键；repository 不做键白名单，白名单由调用方契约约束。
--
-- 索引取自真实查询路径：
--   * op_config：FindOne(cfg_key, scope) 走唯一键；List(scope 过滤，ORDER BY cfg_key,scope)
--     走 idx_scope_cfg_key。
--   * op_admin_task：FindOne(task_id) 走主键、FindByRequestID 走唯一键、
--     List(state/task_type/operator 过滤 + task_id DESC 分页) → idx_state_task_id / idx_operator。
--   * op_admin_task_step：ListByTask(task_id ORDER BY step_no) 走唯一键、
--     ListExecutable(task_id + state + step_no) 与 CountByTask(task_id GROUP BY state)
--     → idx_task_state_step；MarkStep 按主键 id + state 条件更新。
--
-- owner：运营平台（operation 服务）；影响范围：仅新增 3 张表。
-- 回滚（配置与任务历史是运营证据，DROP 前必须备份）：
--   DROP TABLE IF EXISTS `op_admin_task_step`;
--   DROP TABLE IF EXISTS `op_admin_task`;
--   DROP TABLE IF EXISTS `op_config`;
-- 锁风险：全部为新建空表；op_admin_task_step 随任务量线性增长，
--   终态任务的归档/清理由后续 services/cron 承接（本期不做，见 README「已知缺口」）。
-- =====================================================================

-- 运营配置表。value_type：string/int/bool/json，写入前由 repository.validateConfigValue 校验，
-- 避免把脏值推给读取方；scope 为 global 或端标识（android/ios/harmony/desktop）。
-- state=2（下线）的配置对后台仍可读，是否生效由调用方按 State 判定。
CREATE TABLE IF NOT EXISTS `op_config` (
  `id`         BIGINT        NOT NULL AUTO_INCREMENT COMMENT '自增主键（业务定位用 cfg_key + scope）',
  `cfg_key`    VARCHAR(128)  NOT NULL COMMENT '配置键（如 moderation.auto_publish_threshold），最长 128 字符',
  `cfg_value`  VARCHAR(2000) NOT NULL DEFAULT '' COMMENT '配置值（文本承载，按 value_type 解析，最长 2000 字符）',
  `value_type` VARCHAR(16)   NOT NULL DEFAULT 'string' COMMENT '值类型：string/int/bool/json',
  `scope`      VARCHAR(32)   NOT NULL DEFAULT 'global' COMMENT '生效范围：global 或端标识（android/ios/harmony/desktop），最长 32 字符',
  `version`    BIGINT        NOT NULL DEFAULT 1 COMMENT '版本号，每次成功写入 +1；SaveOpsConfig 的乐观锁依据',
  `state`      TINYINT       NOT NULL DEFAULT 1 COMMENT '状态：1 生效、2 下线',
  `operator`   BIGINT        NOT NULL DEFAULT 0 COMMENT '最后修改人 admin_id',
  `remark`     VARCHAR(255)  NOT NULL DEFAULT '' COMMENT '变更说明（最长 255 字符）',
  `ctime`      BIGINT        NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`      BIGINT        NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_cfg_key_scope` (`cfg_key`, `scope`),
  KEY `idx_scope_cfg_key` (`scope`, `cfg_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='运营配置表（配置域键值 + 乐观锁版本）；不含任何商业化参数';

-- 管理任务表。state 只能通过 model.CanTaskTransition 允许的迁移推进：
--   pending → running/canceled；running → succeeded/partial/failed/canceled；终态无出边。
-- 写回都带 `WHERE state = ?` 条件（model.TransitionState），因此多个推进者并发
-- 调用 RunAdminTask 也不会把任务推回上一步；计数列由 SQL 侧自增（AddCounters）。
CREATE TABLE IF NOT EXISTS `op_admin_task` (
  `task_id`      BIGINT       NOT NULL AUTO_INCREMENT COMMENT '任务 ID（主键）',
  `task_type`    VARCHAR(64)  NOT NULL COMMENT '任务类型：batch_offline_submission/batch_offline_episode/batch_expire_window/batch_reject_appeal（model.ValidTaskType）',
  `params`       TEXT         NOT NULL COMMENT '任务级参数（JSON 对象文本，如 {"reason":"版权到期"}；空参数写 {}，代码侧不做长度截断故用 TEXT）',
  `state`        VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT '任务状态：pending/running/succeeded/partial/failed/canceled',
  `request_id`   VARCHAR(64)  NOT NULL COMMENT '提交幂等键（来自 OpContext.request_id，唯一索引；提交接口强制必填，不会出现多行空串）',
  `progress`     INT          NOT NULL DEFAULT 0 COMMENT '已执行步数（succeeded + failed，SQL 侧自增）',
  `total`        INT          NOT NULL DEFAULT 0 COMMENT '步骤总数（提交时按去重后的步骤数写入，上限 1000）',
  `succeeded`    INT          NOT NULL DEFAULT 0 COMMENT '成功步数',
  `failed`       INT          NOT NULL DEFAULT 0 COMMENT '失败步数',
  `operator`     BIGINT       NOT NULL DEFAULT 0 COMMENT '提交人 admin_id',
  `trace_id`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '提交时的链路 ID（与下游日志对齐）',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  `started_at`   BIGINT       NOT NULL DEFAULT 0 COMMENT '首次进入 running 的时间（Unix 秒），0 表示未开始',
  `finished_at`  BIGINT       NOT NULL DEFAULT 0 COMMENT '进入终态的时间（Unix 秒），0 表示未结束',
  PRIMARY KEY (`task_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  -- ListAdminTasks：state 过滤 + task_id DESC 分页
  KEY `idx_state_task_id` (`state`, `task_id`),
  KEY `idx_operator` (`operator`),
  KEY `idx_task_type` (`task_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='批量运营任务表（幂等提交 + 状态机推进），只编排下游 RPC，不持有领域主数据';

-- 任务步骤表：一行一个目标聚合。state 迁移同样受 model.CanStepTransition 约束，
-- MarkStep 带 `WHERE id = ? AND state = ?`，因此同一步骤不会被并发执行两次。
-- target_id 用字符串存，兼容不同下游主键形态（aid/episode_id/window_id/appeal_id）。
CREATE TABLE IF NOT EXISTS `op_admin_task_step` (
  `id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT '步骤自增主键（MarkStep 按此 + state 条件更新）',
  `task_id`     BIGINT       NOT NULL COMMENT '所属任务 ID（op_admin_task.task_id）',
  `step_no`     INT          NOT NULL COMMENT '任务内步骤序号，从 1 连续递增（提交时按去重后的目标顺序生成）',
  `target_type` VARCHAR(32)  NOT NULL COMMENT '目标类型：submission/episode/rights_window/moderation_appeal（必须与 task_type 匹配）',
  `target_id`   VARCHAR(64)  NOT NULL COMMENT '目标聚合 ID（十进制串，提交时校验为正整数）',
  `state`       VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT '步骤状态：pending/running/succeeded/failed/canceled',
  `result`      VARCHAR(500) NOT NULL DEFAULT '' COMMENT '下游返回摘要（写入前按 500 字符截断）',
  `err_msg`     VARCHAR(500) NOT NULL DEFAULT '' COMMENT '失败原因（脱敏：不含堆栈、密钥与下游连接串，按 500 字符截断）',
  `ctime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_task_step` (`task_id`, `step_no`),
  -- ListExecutable(task_id + state=pending ORDER BY step_no) / CountByTask(task_id GROUP BY state)
  KEY `idx_task_state_step` (`task_id`, `state`, `step_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='管理任务步骤表（一步一目标，唯一键保证重试提交不会产出两份步骤）';
