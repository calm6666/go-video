-- =====================================================================
-- feature-store 服务 - 特征回填作业台账表（断点、租约与进度）
-- =====================================================================
-- 用途：建立 feature_backfill_job，一行 = 一次「给某个特征版本补历史值」的作业，
--   含取数窗口、显式主体列表、进度三计数、断点游标、执行权租约与收尾结果。
-- 数据所有者：feature-store 服务（AGENTS.md §5）。
--   作业由本服务 worker + services/cron 认领；提交方（spm 离线产出、运营后台）
--   只能通过 SubmitBackfillJob RPC 落行，禁止直连本表。
--
-- 数据库：go_video_feature_store。
--
-- 为什么是作业台账而不是同步 RPC（契约 SubmitBackfillJob 的前提）：
--   给一个版本补 30 天历史可能要处理上百万主体，同步调用一定被超时打断并留下
--   「补了一半」的版本 —— 而半补的版本被切成 ACTIVE 就是线上事故。
--   作业化后才能限批推进、可续跑、可取消，进度与失败数才可解释。
--   本表就是这条流程的唯一持久台账：进程重启后「跑到哪了、谁在跑」必须在库里。
--
-- 幂等与并发：
--   · UNIQUE KEY uniq_request_id (request_id)：SubmitBackfillJob 的整请求幂等支点。
--     Insert 用 INSERT ... ON DUPLICATE KEY UPDATE mtime = mtime 探测，
--     RowsAffected=0 → ErrJobExists，logic 回查首次作业并返回 reused=true，
--     而不是把同一批补数提交两次（重复提交同一批补数是常态，报错只会让上游重试风暴）。
--     单列唯一即可：一个 request_id 在这里只对应一个作业，不存在跨接口串味
--     （那是 feature_write_receipt 需要 op_type 参与唯一键的原因，见 000006）。
--   · 认领用「租约 + 条件更新」而不是分布式锁：
--     Claim = UPDATE ... SET state=RUNNING, lease_owner=?, lease_expire_at=?
--             WHERE job_id=? AND (state=PENDING OR (state=RUNNING AND lease_expire_at<now))
--     两个 cron 实例同时 Claim 只有一个能拿到；原持有者崩溃后租约到期即可被接管，
--     不需要额外锁服务与锁泄漏治理。
--   · progress_seq 每次 AddProgress 无条件 +1：go-sql-driver 默认返回「实际改变的行数」，
--     一次 done=0/failed=0/游标没动的空心跳会改不到任何列而返回 0，
--     于是租约仍完好的 worker 被误判成「已被接管」。有了这一列命中即改变，
--     RowsAffected=0 就只剩「不是当前持有者」一个含义；它同时是识别僵尸 worker
--     重复心跳的证据。
--
-- 游标双列（为什么两列而不是一个 VARCHAR）：
--   · cursor_entity_id：数值型主体（mid/aid/zone/item）的最近处理值，能被范围比较；
--   · cursor_entity_str：entity_id 原文，哈希/搜索词维度不是数字，实际游标看这列。
--   扫描统一按 entity_id 升序，因此两种游标可共存且都单调，续跑不会漏前半段。
--
-- 隐私与脱敏：
--   · entity_ids 是「显式主体白名单」（逗号分隔升序），只允许主键十进制串或
--     加盐哈希摘要，逐个由 ValidEntityID 复验；明文 PII 拒绝入库；
--   · last_error 截断保存到 maxJobErrorLen=512，且只允许哨兵短码：
--     不含 SQL 片段与特征值原文（AGENTS.md §6）；
--   · 不建跨库外键：feature_key/version 只是对 feature_definition 的引用。
--
-- 事实定位：台账 + 控制位，不是特征值事实源。
--   作业行本身不可重算（它是「谁在什么时候补过哪些值」的审计证据，
--   feature_value.backfill_job_id 反向指向它）；但它不承载任何业务值。
--   entities_total 只是进度分母，允许近似，绝不作为「补齐了」的判据 ——
--   判据是 state=SUCCEEDED 且 entities_failed=0。
--
-- 索引：
--   · idx_state_lease (state, lease_expire_at, job_id)：
--     ListClaimable 的 WHERE state=PENDING OR (state=RUNNING AND lease_expire_at<?)
--     ORDER BY job_id ASC（升序保证先到先得、不互相插队），
--     以及 Claim 的条件更新定位；
--   · idx_key_version_state (feature_key, version, state)：
--     CountUnfinished / CountSucceeded —— UpdateFeatureState 把版本切 ACTIVE 前
--     必须看到 PENDING/RUNNING = 0（边回填边对外读 = 线上值在无人复核下持续变化），
--     且「补过历史」需要 SUCCEEDED 的证据；
--   · idx_key_job (feature_key, job_id)：ListBackfillJobs 按 key 倒序翻页；
--   · idx_mtime 不建：本表按作业数增长（量级 10^3~10^4），保留期清理走主键扫描。
--
-- 回滚：
--   DROP TABLE IF EXISTS `feature_backfill_job`;
--   本表不可重算：删掉后「哪些版本补过历史、补到哪、失败多少」全部丢失，
--   feature_value.backfill_job_id 会同时悬空（回填批次无法追溯）。
--   更危险的副作用是正在 RUNNING 的作业失去台账：worker 的心跳会全部条件不命中，
--   既不停也不报错，等于一批无主流水线在持续写值。DROP 前必须先停 worker。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不改动既有数据。
--   Claim/AddProgress/Finish 都是单行等值条件更新，只锁一行；
--   但 worker 的批量写值与本表心跳不应放在同一事务里 ——
--   心跳只需百毫秒，把上千行的 UPSERT 圈进来会让同一作业的租约行被长时间持有，
--   拖慢接管（不是拖慢自己，是拖慢「崩溃后多久能被接管」）。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `feature_backfill_job` (
  `job_id`               BIGINT      NOT NULL AUTO_INCREMENT COMMENT '作业 ID（job_id），契约与 feature_value.backfill_job_id 都引用它；自增即提交顺序，可认领列表按它升序先到先得',
  `feature_key`          VARCHAR(64) NOT NULL COMMENT '目标特征键',
  `version`              INT         NOT NULL COMMENT '目标版本，必须处于 DRAFT（ErrBackfillTargetNotDraft）：给 ACTIVE 版本补历史值会让线上读到的值在无人复核的情况下改变',
  `entity_scope`         SMALLINT    NOT NULL COMMENT '主体类型，必须与定义一致；决定 entity_ids 与游标的形态校验',
  `source`               SMALLINT    NOT NULL COMMENT '取数来源，必须与 feature_definition.source 一致（ErrBackfillSourceMismatch），防止跨链路串写',
  `state`                SMALLINT    NOT NULL DEFAULT 1 COMMENT '状态机：1 PENDING 2 RUNNING 3 SUCCEEDED 4 FAILED 5 CANCELLED。禁止 PENDING→FAILED（worker 必须先 Claim 才能观测失败，跳过 RUNNING 会让「谁在何时开始跑」在审计里消失）与 RUNNING→PENDING（接管由租约过期表达，退回会丢进度归属导致重复累加）',
  `window_from`          BIGINT      NOT NULL DEFAULT 0 COMMENT '回填数据时间范围起点（Unix 秒），强制 > 0：无窗口的全历史回填会锁住一整张源表的扫描',
  `window_to`            BIGINT      NOT NULL DEFAULT 0 COMMENT '时间范围终点（Unix 秒），0 = 提交时刻；跨度上限 92 天（maxBackfillWindowSeconds），更久历史按窗口拆多个作业，每个可独立重放与回滚',
  `entity_ids`           TEXT        NOT NULL COMMENT '显式主体列表（逗号分隔、去重升序），空串 = 全量扫描。条数上限 1000、编码字节上限 65000（MaxBackfillEntityIDsBytes）：按列宽而不是条数卡，否则 1000 个 64 字符摘要会在插入时才炸，且全量/显式两条路径的内存上界会完全不同',
  `entities_truncated`   SMALLINT    NOT NULL DEFAULT 0 COMMENT '1 = 列表因超上限被截断。此时作业必须转 FAILED，不允许「补一半」：截断的补数看起来是成功，实际缺一批主体',
  `entities_total`       BIGINT      NOT NULL DEFAULT 0 COMMENT '计划处理主体数（扫描前为 0）。仅当进度分母用，允许近似，不作为「补齐」判据',
  `entities_done`        BIGINT      NOT NULL DEFAULT 0 COMMENT '已成功处理主体数（AddProgress 按租约持有者累加，不重复计数）',
  `entities_failed`      BIGINT      NOT NULL DEFAULT 0 COMMENT '失败主体数。done + failed <= total，超出即数据坏了，作业转 FAILED',
  `progress_seq`         BIGINT      NOT NULL DEFAULT 0 COMMENT '推进次数计数器，每次 AddProgress 无条件 +1（存在理由见文件头：让 RowsAffected=0 只剩「不是当前持有者」一个含义）',
  `cursor_entity_id`     BIGINT      NOT NULL DEFAULT 0 COMMENT '断点游标（数值形态）：最近处理的 mid/aid/zone/item。哈希与搜索词维度保持 0，实际游标看 cursor_entity_str',
  `cursor_entity_str`    VARCHAR(64) NOT NULL DEFAULT '' COMMENT '断点游标（原文形态）：最近处理的 entity_id，受控标识而非明文 PII。扫描按 entity_id 升序，故两列游标都单调',
  `lease_owner`          VARCHAR(128) NOT NULL DEFAULT '' COMMENT '当前认领者 <pod 名>#<worker id>，非持有者的推进/收尾写入会被拒绝',
  `lease_expire_at`      BIGINT      NOT NULL DEFAULT 0 COMMENT '租约到期时间（Unix 秒）：过期后作业可被别的 worker 接管，原推进者的进度写入被 ErrJobLeaseExpired 拒绝',
  `auto_switch`          TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '回填成功后是否自动切 ACTIVE（Go bool）。放开它的前提是 from_version 给出乐观基线',
  `from_version`         INT         NOT NULL DEFAULT 0 COMMENT 'auto_switch 的期望当前生效版本（0 = 从无到有）：有这一列，「回填期间有人手工切过版本」不会被自动切换悄悄覆盖',
  `request_id`           VARCHAR(64) NOT NULL COMMENT '提交幂等键（唯一索引 uniq_request_id）',
  `operator`             VARCHAR(64) NOT NULL COMMENT '提交人（必填，无主作业不可解释）',
  `reason`               VARCHAR(512) NOT NULL COMMENT '提交理由（必填：补数依据、评估单号）。禁止粘贴特征值原文',
  `last_error`           VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次失败的哨兵短码，截断保存（maxJobErrorLen=512）：不含 SQL 片段与特征值原文',
  `trace_id`             VARCHAR(64) NOT NULL DEFAULT '' COMMENT '提交时的链路 ID（不含主体标识明文）',
  `ctime`                BIGINT      NOT NULL DEFAULT 0 COMMENT '提交时间（Unix 秒），ListBackfillJobs 的时间过滤列',
  `mtime`                BIGINT      NOT NULL DEFAULT 0 COMMENT '最后推进时间（Unix 秒）；心跳与收尾都会刷新，是「这个作业还活着」的观测口径',
  `started_at`           BIGINT      NOT NULL DEFAULT 0 COMMENT '首次被认领开始执行的时间（Unix 秒），0 = 从未开始',
  `finished_at`          BIGINT      NOT NULL DEFAULT 0 COMMENT '进入终态的时间（Unix 秒），0 = 未终态。重复收尾不覆盖本列（Finish 带 state=RUNNING + 租约双条件）',
  PRIMARY KEY (`job_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_state_lease` (`state`, `lease_expire_at`, `job_id`),
  KEY `idx_key_version_state` (`feature_key`, `version`, `state`),
  KEY `idx_key_job` (`feature_key`, `job_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='特征回填作业台账表（断点/租约/进度；不可重算的审计证据，不承载任何特征值）';
