-- =====================================================================
-- spm 服务 - 用户兴趣画像、留存分桶与聚合作业
-- =====================================================================
-- 库名：go_video_spm（见 services/spm/etc/spm.v1.yaml 的 DataSource）。
-- 数据所有者：spm 服务（AGENTS.md §7）。对应 model 文件：
--   spm_user_interest     -> model/user_interest.go     画像投影（可重算，非事实源）
--   spm_retention_cohort  -> model/retention_cohort.go  留存投影（可重算，非事实源）
--   spm_aggregation_job   -> model/aggregation_job.go   作业与租约（执行台账，非投影）
-- 投影属性：前两张表都可由 spm_behavior_event 全量重算，不作为任何服务的事实源；
--   计数漂移的唯一修复入口是 Spm.RecomputeMetrics（从事实重算），不存在「手工改画像/改留存」的通道。
-- 隐私（AGENTS.md §7 第 1 条、docs/data-design.md §6）：
--   spm_user_interest 只存 mid + 受控兴趣键（zone:<id> / tag:<id> / catalog:<id> / up:<mid>），
--   权重是归一化数值；搜索词原文、稿件标题、UP 昵称等自由文本禁止作为兴趣键，
--   因此本表不需要设备/用户维度的加盐 hash 列（用户维度直接是 mid 主键，
--   设备维度的假名只出现在 spm_behavior_event.pseudonym，画像不落设备维度）。
--   spm_retention_cohort 只存分桶计数，不回落到个体：cohort 日 + 第 N 日的比值无法反推任何人。
-- 幂等键：
--   uniq_interest (mid, metric_version, interest_key)   ReplaceForMid 整组替换 + 批内去重
--   uniq_cohort   (cohort_type, cohort_date, zone_id, metric_version, day_offset) 覆盖式重写留存点
--   uniq_request_id (request_id)                        作业提交重放只产生一个作业
-- 回滚：
--   DROP TABLE IF EXISTS `spm_aggregation_job`;
--   DROP TABLE IF EXISTS `spm_retention_cohort`;
--   DROP TABLE IF EXISTS `spm_user_interest`;
-- 锁风险：
--   1) 新建空表，索引在空表上建立，无在线锁风险。
--   2) ReplaceForMid 是「同事务先 DELETE 后多值 INSERT」：一个 mid 一个事务，锁范围是
--      该 mid 在 uniq_interest 前缀上的区间。绝不能改成按 metric_version 全量替换
--      （那会把整表区段锁住），批量刷新必须由调用方按 mid 逐个提交。
--   3) 留存回填写的是「cohort 日 × 分区 × D0..D90」的点集，UpsertBatch 单批上限
--      maxRetentionRowsPerBatch=1000 行（与 11 列 = 1.1 万占位符同量级），超过即由调用方
--      拆批，避免超长 SQL 与长时间持锁；DeleteExpired 同样带 LIMIT 分批。
--   4) ClaimPending 是 `UPDATE ... WHERE (state=PENDING OR (state=RUNNING AND lease_until<now))
--      ORDER BY id LIMIT 1`：InnoDB 的 UPDATE...LIMIT 会加索引范围锁，领取节奏（每实例
--      每轮一条）必须远低于作业量，否则该语句会成为热点争用；这是「作业可被接手」的代价。
-- =====================================================================

-- 用户兴趣画像投影：一行 = 某用户某口径版本的一个兴趣键。
-- 写入方：本服务画像聚合链路（ReplaceForMid 整组替换）；读取方：Spm.GetUserInterest。
-- 「兴趣消失」是正常结果：整组替换而不是逐行 upsert，否则不再感兴趣的键会永久残留。
CREATE TABLE IF NOT EXISTS `spm_user_interest` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `mid`            BIGINT          NOT NULL DEFAULT 0 COMMENT '用户 mid（画像主体；隐私删除链路走 DeleteByMid）',
  `metric_version` INT             NOT NULL COMMENT '兴趣口径版本（不同版本的权重不可比，读取必须钉住一个版本）',
  `interest_key`   VARCHAR(100)    NOT NULL COMMENT '受控兴趣键：zone:<id> / tag:<id> / catalog:<id> / up:<mid>，禁止自由文本',
  `weight`         DOUBLE          NOT NULL DEFAULT 0 COMMENT '归一化权重（同一 mid 同一版本内和 <= 1）',
  `sample_count`   BIGINT          NOT NULL DEFAULT 0 COMMENT '支撑该兴趣的样本数（低样本兴趣由读取侧按冷启动处理）',
  `event_time`     BIGINT          NOT NULL DEFAULT 0 COMMENT '最近一次更新时间（Unix 秒，画像过期 stale 判定依据）',
  `ctime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_interest` (`mid`, `metric_version`, `interest_key`),
  -- 画像过期观测：CountStaleMids 按 event_time 区间统计（非精确计数，允许走索引扫）
  KEY `idx_event_time` (`event_time`)
  -- 隐私删除（DeleteByMid WHERE mid = ?）与按用户读取都走 uniq_interest 的 mid 前缀，
  -- 因此不再单独建 idx_mid：这是一张写多读多的表，冗余索引只放大写入。
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='用户兴趣画像投影（脱敏权重，不返回行为明细；可由 spm_behavior_event 重算，不是唯一事实源）';

-- 留存分桶投影：一行 = 「某分桶日的一批用户在第 N 日仍活跃」。
-- 写入方：留存回填作业；读取方：Spm.GetRetention。
-- 为什么存 rate：读取时恒可用 retained/cohort_size 复核；写入口要求
--   0 <= retained <= cohort_size 且 rate == retained/cohort_size（model 侧不变量），
--   否则曲线上会出现「第 30 日留存 120%」这种无法回溯的图。
-- 口径边界：COHORT_TYPE_REGISTER_DAY 依赖 account 的注册时间（本库无该真值，见 README 契约缺口）；
--   COHORT_TYPE_FIRST_PLAY_DAY 只依赖本库事实，但按 mid 去重，因此只带 mid_hash 的
--   playback.heartbeat 不参与留存计数（否则游客与登录用户会被并进同一个桶）。
CREATE TABLE IF NOT EXISTS `spm_retention_cohort` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `cohort_type`    TINYINT         NOT NULL DEFAULT 0 COMMENT '分桶类型：1 注册日、2 首次播放日（rpc.GetRetentionReq.CohortType）',
  `cohort_date`    BIGINT          NOT NULL DEFAULT 0 COMMENT '分桶日（UTC 零点 Unix 秒，入库前统一 dayStartUnix 对齐）',
  `zone_id`        BIGINT          NOT NULL DEFAULT 0 COMMENT '分区（0 = 全站；分区留存要求 spm_content_projection 能给出 zone_id）',
  `metric_version` INT             NOT NULL COMMENT '留存口径版本（活跃定义变更必须新增版本）',
  `day_offset`     INT             NOT NULL DEFAULT 0 COMMENT '第 N 日（0 = 分桶当日，上限 model.maxRetentionDay=90）',
  `cohort_size`    BIGINT          NOT NULL DEFAULT 0 COMMENT '分桶规模（该 cohort 日的活跃/注册用户数，作为分母）',
  `retained`       BIGINT          NOT NULL DEFAULT 0 COMMENT '第 N 日仍活跃数（必须 <= cohort_size）',
  `rate`           DOUBLE          NOT NULL DEFAULT 0 COMMENT '留存率 = retained / cohort_size（不是百分比，落库前已校验一致）',
  `event_time`     BIGINT          NOT NULL DEFAULT 0 COMMENT '最近一次计算时间（Unix 秒，不是事件时刻；缺省由 model 补 now）',
  `ctime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_cohort` (`cohort_type`, `cohort_date`, `zone_id`, `metric_version`, `day_offset`),
  -- 保留期清理：DeleteExpired 按分桶日区间分批删除
  KEY `idx_cohort_date` (`cohort_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='留存曲线投影（分子分母与比率同时存，可由 spm_behavior_event 重算，不是唯一事实源）';

-- 聚合作业表：实时窗口聚合 / 离线回填 / 从事实重算三条链路的统一任务面。
-- 写入方：Spm.SubmitAggregationJob、Spm.RecomputeMetrics（派生 JOB_TYPE_RECOMPUTE）与
--   本服务作业执行侧的 ClaimPending/RenewLease/UpdateProgress/MarkFinished/CancelPending。
-- 可重放：claim + 租约（claimed_by/lease_until）+ 进度计数让崩溃后的实例能接手，
--   而不是把窗口指标写花；因此本表不是投影，而是执行台账（终态不可回退）。
-- 幂等：uniq_request_id 让重复提交回放到同一作业，绝不并发起两个改写同段窗口的作业。
CREATE TABLE IF NOT EXISTS `spm_aggregation_job` (
  `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（契约里的 job_id）',
  `job_type`          TINYINT         NOT NULL DEFAULT 0 COMMENT '作业类型：1 实时窗口聚合、2 离线回填、3 重算修复（rpc.JobType）',
  `state`             TINYINT         NOT NULL DEFAULT 1 COMMENT '状态：1 PENDING、2 RUNNING、3 SUCCEEDED、4 FAILED、5 CANCELLED（终态不可回退）',
  `subject_type`      TINYINT         NOT NULL DEFAULT 0 COMMENT '主体类型（0 = 全部主体）',
  `subject_id`        BIGINT          NOT NULL DEFAULT 0 COMMENT '主体主键（0 = 不限主体）',
  `metric_key`        VARCHAR(100)    NOT NULL DEFAULT '' COMMENT '指标键（空 = 该作业类型下全部指标）',
  `metric_version`    INT             NOT NULL DEFAULT 0 COMMENT '口径版本：回填/重算必须显式版本（拿 ACTIVE 去改写历史窗口事后无法解释）',
  `window_type`       TINYINT         NOT NULL DEFAULT 0 COMMENT '窗口粒度（rpc.WindowType）',
  `window_start_from` BIGINT          NOT NULL DEFAULT 0 COMMENT '起始窗口（含，入库前已按粒度规整）',
  `window_start_to`   BIGINT          NOT NULL DEFAULT 0 COMMENT '结束窗口（含，0 = 提交时刻）',
  `windows_total`     INT             NOT NULL DEFAULT 0 COMMENT '计划重算的窗口数',
  `windows_done`      INT             NOT NULL DEFAULT 0 COMMENT '已完成窗口数（只单调前进）',
  `windows_failed`    INT             NOT NULL DEFAULT 0 COMMENT '失败窗口数（只单调前进）',
  `claimed_by`        VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '当前持有租约的领取令牌（每次领取唯一，不是每进程唯一）',
  `lease_until`       BIGINT          NOT NULL DEFAULT 0 COMMENT '租约到期时间（Unix 秒，过期可被其他实例接手；终态清零）',
  `request_id`        VARCHAR(128)    NOT NULL COMMENT '提交幂等键（唯一索引，必填因此不会有空串互相撞键）',
  `operator`          VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '触发者（system/cron/admin:<id>）；取消时覆写为取消人',
  `reason`            VARCHAR(500)    NOT NULL DEFAULT '' COMMENT '触发原因（回填范围说明、故障修复单号；取消时覆写为取消理由）',
  `last_error`        VARCHAR(512)    NOT NULL DEFAULT '' COMMENT '最近失败原因（已脱敏截断，不含堆栈与 SQL）',
  `finished_at`       BIGINT          NOT NULL DEFAULT 0 COMMENT '终态时间（Unix 秒，0 = 未结束）',
  `ctime`             BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  -- 领取队列：PENDING 或租约过期的 RUNNING（谓词带括号，AND 优先于 OR）
  KEY `idx_claim` (`state`, `lease_until`, `job_type`),
  -- 按令牌回读自己刚领到的作业（UPDATE...LIMIT 拿不到主键，只能靠令牌）
  KEY `idx_claimed_by` (`claimed_by`, `state`, `lease_until`),
  -- 作业列表：按类型/状态 + 时间过滤，倒序翻页
  KEY `idx_type_ctime` (`job_type`, `ctime`),
  KEY `idx_state_ctime` (`state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='聚合作业与租约台账（实时/回填/重算三条链路共用，可被接手重放，不是投影）';
