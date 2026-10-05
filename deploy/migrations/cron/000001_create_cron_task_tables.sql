-- =====================================================================
-- cron 服务 - 任务定义与变更审计
-- =====================================================================
-- owner：cron 服务（数据所有者，AGENTS.md §3「定时任务放在 services/cron」、§5）。
--       库：go_video_cron。
-- 用途：登记全仓定时/补偿任务的调度事实源（谁在什么时候跑、跑多久、失败怎么退避），
--       以及暂停/恢复/停用/人工重试的审计留痕。两张表都不承载业务数据：
--       业务状态一律由处理器调用领域 RPC 推进（AGENTS.md §5、§8）。
-- 影响：新建 2 张表，不改动任何已有表，不建跨库外键（只存 task_key 这类自有主键）。
-- 回滚：DROP TABLE IF EXISTS `cron_task_audit`, `cron_task_definition`;
--       本服务尚未上线跑批，回滚仅丢失已登记的任务定义，可由 RegisterTask 重新注册；
--       已注册任务若被其他服务依赖，回滚前先 PauseTask 停止新触发。
-- 锁风险：仅建表，无 ALTER、无回填，无锁风险；重复执行由 IF NOT EXISTS 兜底。
--       后续如需给已上线的 cron_task_definition 加列，必须另起迁移文件并评估
--       ALGORITHM=INPLACE 的元数据锁窗口。
-- 时间口径：所有时间列统一 Unix 秒（BIGINT），与 RPC 契约一致；
--       duration 类字段才使用毫秒，且不存在本表。
-- =====================================================================

-- 任务定义：调度事实源。
-- 幂等：uniq_task_key 保证一个任务键只有一行定义，RegisterTask 走
--       INSERT ... ON DUPLICATE KEY UPDATE id=id 区分「首次注册」与「幂等重入」。
-- 到期扫描：idx_state_next_fire (state, next_fire_at) 服务 ListDueTasks 的
--       state=ENABLED AND next_fire_at BETWEEN ... 条件，避免全表扫。
CREATE TABLE IF NOT EXISTS `cron_task_definition` (
  `id`                     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `task_key`               VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '任务唯一键，如 rights.expire_scan（注册表索引键）',
  `name`                   VARCHAR(128) NOT NULL DEFAULT '' COMMENT '展示名',
  `handler`                VARCHAR(128) NOT NULL DEFAULT '' COMMENT '进程内任务注册表的处理器名（internal/registry）',
  `task_group`             VARCHAR(64)  NOT NULL DEFAULT 'default' COMMENT '分组：rights/index/report/cleanup/replay，用于批量暂停与健康统计',
  `schedule_type`          TINYINT      NOT NULL DEFAULT 1 COMMENT '1 CRON 表达式 2 固定间隔 3 仅手动触发',
  `cron_expr`              VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'schedule_type=1 时的 cron 表达式',
  `interval_seconds`       INT          NOT NULL DEFAULT 0 COMMENT 'schedule_type=2 时的间隔秒数',
  `timezone`               VARCHAR(64)  NOT NULL DEFAULT 'Asia/Shanghai' COMMENT 'cron 表达式解释时区',
  `timeout_seconds`        INT          NOT NULL DEFAULT 300 COMMENT '单次执行超时，超时判 TIMEOUT',
  `max_attempts`           INT          NOT NULL DEFAULT 1 COMMENT '含首次的最大尝试次数（1 表示不重试）',
  `retry_base_seconds`     INT          NOT NULL DEFAULT 30 COMMENT '退避基数：base * 2^(attempt-1)',
  `retry_max_seconds`      INT          NOT NULL DEFAULT 1800 COMMENT '退避上限',
  `concurrency_limit`      INT          NOT NULL DEFAULT 1 COMMENT '同一任务允许并行的执行数，1 表示串行',
  `lease_ttl_seconds`      INT          NOT NULL DEFAULT 300 COMMENT '租约 TTL，到期即可被其它实例抢占',
  `misfire_policy`         TINYINT      NOT NULL DEFAULT 1 COMMENT '1 合并补跑一次 2 跳到下一点 3 逐个补齐',
  `misfire_backfill_limit` INT          NOT NULL DEFAULT 5 COMMENT 'misfire_policy=3 时单轮最多补齐的计划点数',
  `params`                 VARCHAR(4096) NOT NULL DEFAULT '' COMMENT '处理器参数 JSON 文本；密钥只写 Secret 环境变量名，不写值',
  `secret_refs`            VARCHAR(512) NOT NULL DEFAULT '' COMMENT '逗号分隔的环境变量名，处理器自行取值',
  `state`                  TINYINT      NOT NULL DEFAULT 1 COMMENT '1 ENABLED 2 PAUSED 3 DISABLED',
  `next_fire_at`           BIGINT       NOT NULL DEFAULT 0 COMMENT '下一个计划时刻（Unix 秒），0 表示不参与到期扫描',
  `last_fire_at`           BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次产生执行记录的计划时刻',
  `last_success_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次成功完成时间',
  `last_error`             VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次失败摘要（截断，不含堆栈与敏感数据）',
  `version`                BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观锁版本，Update/Pause/Resume/Disable 必须回传',
  `owner`                  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '责任团队或服务',
  `operator`               VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次变更操作人',
  `ctime`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_task_key` (`task_key`),
  KEY `idx_state_next_fire` (`state`, `next_fire_at`),
  KEY `idx_group_state` (`task_group`, `state`),
  KEY `idx_handler` (`handler`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='定时任务定义：调度与重试策略的事实源，不承载业务数据';

-- 任务变更审计：只追加、不更新，记录暂停/恢复/停用/人工重试等运营动作。
-- 说明：任务输出的摘要在 cron_task_run.result_summary，本表只记录「谁改动了调度面」，
--       因此 detail 限制在 2048 字节且禁止写入事件正文或密钥。
-- 留存：默认保留 180 天，超期由本服务的 cron.cleanup.audit 任务先归档再按 ctime 删除
--       （见 services/cron/README.md「留存与回收」），idx_ctime 专为该删除条件服务。
CREATE TABLE IF NOT EXISTS `cron_task_audit` (
  `id`          BIGINT        NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `task_key`    VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '任务键',
  `action`      VARCHAR(32)   NOT NULL DEFAULT '' COMMENT 'register/update/pause/resume/disable/trigger/retry/replay',
  `from_state`  VARCHAR(24)   NOT NULL DEFAULT '' COMMENT '变更前状态文本，新建为空',
  `to_state`    VARCHAR(24)   NOT NULL DEFAULT '' COMMENT '变更后状态文本',
  `operator`    VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '操作人：管理员 mid 字符串或实例名',
  `detail`      VARCHAR(2048) NOT NULL DEFAULT '' COMMENT '变更/原因摘要 JSON 文本，不含密钥与事件正文',
  `trace_id`    VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '链路 ID，用于与网关/下游日志对齐',
  `ctime`       BIGINT        NOT NULL DEFAULT 0 COMMENT '记录时间（Unix 秒）',
  PRIMARY KEY (`id`),
  KEY `idx_task_ctime` (`task_key`, `ctime`),
  KEY `idx_action_ctime` (`action`, `ctime`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='任务调度面变更审计：运营动作必须可追溯';
