-- =====================================================================
-- moderation-worker 服务 - 机审执行记录
-- =====================================================================
-- 数据所有者：moderation-worker。worker_task_id 为业务主键兼幂等键：
-- Upsert 走 INSERT ... ON DUPLICATE KEY UPDATE mtime，重复下发不新增行。
-- 结果以 JSON 片段保存，不写回 orchestrator 的表（AGENTS.md §5）。
-- 回滚：DROP TABLE IF EXISTS `worker_task`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `worker_task` (
  `worker_task_id`    VARCHAR(64)  NOT NULL COMMENT 'worker 内部任务 ID（主键，幂等键）',
  `task_id`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'orchestrator 任务 ID',
  `capability`        TINYINT      NOT NULL DEFAULT 0 COMMENT '识别能力：1 OCR、2 ASR、3 Image、4 Audio',
  `media_uri`         VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '媒体访问 URI',
  `duration_ms`       BIGINT       NOT NULL DEFAULT 0 COMMENT '媒体时长（毫秒）',
  `params_json`       TEXT         COMMENT '算法参数 JSON',
  `timeout_ms`        BIGINT       NOT NULL DEFAULT 0 COMMENT '单任务超时（毫秒）',
  `trace_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '调用方 trace_id',
  `state`             TINYINT      NOT NULL DEFAULT 0 COMMENT '任务状态（TaskState 常量）',
  `algorithm_version` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '算法版本',
  `elapsed_ms`        BIGINT       NOT NULL DEFAULT 0 COMMENT '执行耗时（毫秒）',
  `result_json`       TEXT         COMMENT '结构化结果片段 JSON',
  `error_message`     VARCHAR(500) NOT NULL DEFAULT '' COMMENT '失败原因',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`worker_task_id`),
  KEY `idx_task_id_mtime` (`task_id`, `mtime`),
  KEY `idx_state_mtime` (`state`, `mtime`),
  KEY `idx_trace_id` (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='机审 worker 执行记录表';
