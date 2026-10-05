-- =====================================================================
-- cron 服务 - 执行记录、租约与增量游标
-- =====================================================================
-- owner：cron 服务（数据所有者，AGENTS.md §5）。库：go_video_cron。
-- 用途：
--   1) cron_task_run    —— 每次「计划触发 + 第 N 次尝试」的执行记录（含执行级租约三元组）；
--   2) cron_task_lease  —— 任务级互斥租约（报表/清理/重建类任务同一时刻只允许一个实例）；
--   3) cron_task_checkpoint —— 增量任务的 CAS 游标，支撑「重放同一计划时刻不重复产生副作用」。
-- 幂等与重放：cron_task_run 的 UNIQUE(task_key, planned_at, attempt) 是调度幂等的核心：
--   同一 (task_key, planned_at) 并发 claim 只有一个赢家；重试是同一计划时刻的新 attempt 行，
--   终态行永不回改（历史不可篡改），因此重放只会追加轨迹、不会重复推进业务状态。
-- 回滚：DROP TABLE IF EXISTS `cron_task_checkpoint`, `cron_task_lease`, `cron_task_run`;
--   影响面：丢失执行轨迹与游标后，增量任务下次启动会从 value=0 重新扫描。
--   因此**回滚前必须先冻结游标**：
--     SELECT task_key, scope_key, value, value_str FROM cron_task_checkpoint; -- 导出留存
--   并确认下游写入本身幂等（领域服务都有唯一约束/幂等键），再重建表并回灌游标。
-- 锁风险：仅建表，无 ALTER；cron_task_run 是高频写入表（每个计划点至少 1 行），
--   清理任务按 ctime 分批 DELETE ... LIMIT，禁止一次删全月，避免长事务与主从延迟。
--   AUTO_INCREMENT 单表增长可控（默认保留 30 天）。
-- =====================================================================

-- 执行记录：租约 + 状态机 + 结果摘要三合一，claim 与租约写在同一事务里。
-- idx_state_lease 服务「RUNNING 且租约过期」的孤儿执行扫描；
-- idx_state_retry 服务退避到期重投；idx_fire 服务同一计划时刻的 attempt 查询。
CREATE TABLE IF NOT EXISTS `cron_task_run` (
  `id`              BIGINT        NOT NULL AUTO_INCREMENT COMMENT 'run_id',
  `task_key`        VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '任务键',
  `planned_at`      BIGINT        NOT NULL DEFAULT 0 COMMENT '计划时刻（Unix 秒），与 task_key 构成幂等身份',
  `attempt`         INT           NOT NULL DEFAULT 1 COMMENT '第几次尝试，从 1 开始；重试是同计划时刻的新行',
  `trigger_type`    TINYINT       NOT NULL DEFAULT 1 COMMENT '1 定时 2 手动 3 退避重试 4 人工重放',
  `state`           TINYINT       NOT NULL DEFAULT 1 COMMENT '1 PENDING 2 RUNNING 3 RETRYING 4 SUCCEEDED 5 FAILED 6 TIMEOUT 7 CANCELED 8 SKIPPED',
  `lease_owner`     VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '持有租约的实例（hostname-pid-random），用于审计谁跑的',
  `lease_expire_at` BIGINT        NOT NULL DEFAULT 0 COMMENT '执行级租约到期时间，<=now 即可被抢占；终态清 0',
  `fence_token`     BIGINT        NOT NULL DEFAULT 0 COMMENT '栅栏令牌，取自任务级租约，上报时必须匹配',
  `started_at`      BIGINT        NOT NULL DEFAULT 0 COMMENT '开始执行时间（Unix 秒）',
  `finished_at`     BIGINT        NOT NULL DEFAULT 0 COMMENT '结束时间（Unix 秒），0 表示未结束',
  `duration_ms`     BIGINT        NOT NULL DEFAULT 0 COMMENT '执行耗时（毫秒），由 worker 测量回传',
  `result_summary`  VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '处理器回填的结果摘要，禁止承载事件正文',
  `last_error`      VARCHAR(512)  NOT NULL DEFAULT '' COMMENT '最近一次失败原因（截断，不含堆栈与密钥）',
  `next_retry_at`   BIGINT        NOT NULL DEFAULT 0 COMMENT 'RETRYING 的下次可执行时间（退避窗口）',
  `operator`        VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '手动触发/重试的操作人',
  `trace_id`        VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '链路 ID',
  `ctime`           BIGINT        NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT        NOT NULL DEFAULT 0 COMMENT '状态变更时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_fire_attempt` (`task_key`, `planned_at`, `attempt`),
  KEY `idx_fire` (`task_key`, `planned_at`),
  KEY `idx_state_retry` (`state`, `next_retry_at`),
  KEY `idx_state_lease` (`state`, `lease_expire_at`),
  KEY `idx_planned` (`planned_at`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='任务执行记录：同一计划时刻同一尝试号只允许一行，终态不可回改';

-- 任务级互斥租约：可抢占 + TTL + 单调递增栅栏令牌。
-- 抢占走 SELECT ... FOR UPDATE 的事务路径（model.TaskLeaseModel.Acquire）：
--   expire_at <= now 即视为持有者崩溃，任何实例可接管并把 fence_token +1；
--   被顶掉的旧实例随后心跳/上报会因 fence_token 不一致失败，必须停止写下游。
-- takeover_count 是实例稳定性信号：持续增长说明调度进程频繁崩溃或 GC 停顿超过 TTL。
CREATE TABLE IF NOT EXISTS `cron_task_lease` (
  `id`              BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `lease_key`       VARCHAR(128) NOT NULL DEFAULT '' COMMENT '租约键：默认等于 task_key，带分片时为 task_key/scope',
  `task_key`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '冗余任务键，便于按任务查询与批量暂停',
  `scope`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '分片键，空表示任务级全局互斥',
  `owner_instance`  VARCHAR(128) NOT NULL DEFAULT '' COMMENT '当前持有实例，空串表示无人持有',
  `fence_token`     BIGINT       NOT NULL DEFAULT 0 COMMENT '栅栏令牌，每次抢占 +1，永不回退',
  `acquired_at`     BIGINT       NOT NULL DEFAULT 0 COMMENT '本次持有的开始时间（Unix 秒）',
  `renewed_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次续租时间（Unix 秒）',
  `expire_at`       BIGINT       NOT NULL DEFAULT 0 COMMENT '租约到期时间，<=now 即可被抢占；释放清 0',
  `takeover_count`  INT          NOT NULL DEFAULT 0 COMMENT '因过期被抢占的累计次数',
  `run_id`          BIGINT       NOT NULL DEFAULT 0 COMMENT '当前持有租约对应的 cron_task_run.id，0 表示无',
  `ctime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_lease_key` (`lease_key`),
  KEY `idx_task_key` (`task_key`),
  KEY `idx_expire` (`expire_at`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='任务级互斥租约：TTL 到期可抢占，fence_token 单调递增防止双跑';

-- 增量任务游标：报表/归档/重建/死信重放类任务的「已处理水位」。
-- version 是 CAS 版本：处理器必须带 expected_version 前进游标，
-- 冲突时重读而不是覆盖，保证同一计划时刻重放时不会重复推进已完成区间。
CREATE TABLE IF NOT EXISTS `cron_task_checkpoint` (
  `id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `task_key`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '任务键',
  `scope_key`  VARCHAR(128) NOT NULL DEFAULT '' COMMENT '同任务内的分片/维度键（如 shard=7），空表示默认游标',
  `value`      BIGINT       NOT NULL DEFAULT 0 COMMENT '数值游标（已处理到的主键或时间水位）',
  `value_str`  VARCHAR(255) NOT NULL DEFAULT '' COMMENT '字符串游标（索引别名、分区名等）',
  `version`    BIGINT       NOT NULL DEFAULT 1 COMMENT 'CAS 版本，每次成功写入 +1',
  `operator`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次推进者（实例名或操作人）',
  `ctime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次推进时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_task_scope` (`task_key`, `scope_key`),
  KEY `idx_mtime` (`mtime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='增量任务 CAS 游标：重放同一计划时刻不重复产生副作用的落点';
