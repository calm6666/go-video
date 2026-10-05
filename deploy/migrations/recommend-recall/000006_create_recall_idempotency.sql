-- =====================================================================
-- recommend-recall 服务 - 写接口幂等控制表（recall_idempotency）
-- =====================================================================
-- 用途：为三个写 RPC 提供「同一幂等键只产生一次副作用」的落库锚点，并保存首次执行的
--   回复投影供重放原样返回。作用域与 model.IdempotencyScope* 一一对应：
--   upsert_pool_items（候选批次写入）、publish_pool_version（版本上线）、
--   rollback_pool_version（版本回滚）。
-- 数据所有者：recommend-recall 服务（AGENTS.md §5）。其他服务禁止读写本表。
--
-- 数据库：go_video_recommend_recall（建库由 scripts/migrate.ps1 负责，本文件只建表）。
--
-- 为什么必须有这张表（不是"照惯例加一张"）：
--   rpc.UpsertPoolItemsReply / PublishPoolVersionReply / RollbackPoolVersionReply 都带
--   deduplicated 字段（"本次是重放还是新执行"）。没有落库的幂等记录就无法区分二者 ——
--   只能靠调用方自觉，离线作业重试一次就会把同一批候选当成两次执行、
--   把一次指针切换当成两次切换（switch_count 虚高，发布频率观测随之失真）。
--
-- 事实定位：控制位，不是事实源。
--   按 expire_at 清理后最坏的副作用是"重复请求再执行一次"，而条目写入本身是
--   INSERT ... ON DUPLICATE KEY UPDATE（recall_pool.uniq_pool_item）、指针切换是 CAS
--   （recall_pool_current.Switch），重复执行不产生第二份数据。
--
-- 唯一键与幂等设计（model/idempotency.go Claim 的实际语句为准）：
--   Claim = INSERT ... ON DUPLICATE KEY UPDATE id = id。MySQL 对新插入返回 affected=1、
--   对"值未改变的重复键更新"返回 affected=0，Claim 据此判定"这次是谁把行插进来的"：
--   affected=1 → First=true，允许执行业务写；affected=0 → 回读已有行决定 Held/重新认领。
--   这要求 (scope, idempotency_key) 上有且仅有一个唯一索引 uniq_scope_key。
--
--   关键约束：除主键外本表只能有 uniq_scope_key 这一个键。
--   再加第二个唯一键（例如给 request_hash 或 event_id 建唯一键）会让 affected 的语义失真 ——
--   插入可能命中"非预期那个键"而返回 0，首次受理被误判为重放，
--   后果是候选批次被丢弃、发布请求被当成重复而不再切换。
--   同键不同 request_hash 的防护也不靠唯一索引：Claim 回读后显式比对指纹并返回
--   model.ErrIdempotencyFingerprintMismatch（可读错误，且保留"键已被占用"这一事实），
--   比在插入阶段撞 1062 更准确。要按 state / operator 检索请走普通二级索引，绝不可升级为唯一键。
--
--   为什么是 (scope, idempotency_key) 复合而不是单列 idempotency_key：
--   三个作用域共用一个键字符串空间。若单列唯一，作业用同一个 batch_id 既做写入键又做
--   发布键时，第二次调用会被误判为重复而静默不切换 —— 跨接口的键复用必须由 scope 隔离。
--
--   scope 与 idempotency_key 用 utf8mb4_bin：它们是唯一性判定列，比较必须逐字节精确。
--   表级 utf8mb4_unicode_ci 是 PAD SPACE + 大小写不敏感，会把 "batch-1" 与 "BATCH-1 "
--   折成同一个键，两条本应各自生效的请求会有一条被当成重放吞掉。
--   注意 model.checkIdempotencyArgs 只用 TrimSpace 判空、并不裁短键，
--   因此尾随空格确实可能落库 —— 这正是必须用 bin 的直接证据。
--   request_hash 与 event_id 同样用 bin：前者是 Go 侧 `exist.RequestHash != in.RequestHash`
--   的精确比对（口径必须与存储一致），后者要与 recall_outbox.event_id 逐字节对齐。
--   state 不设 bin：它只被写入三个常量字面量（string(idempotency.State*)，全小写），
--   且参与的是 SQL 侧 IN/<><> 集合判定；保留表级 ci 让 DBA 手工修数据时的大小写误写
--   不至于把行"藏起来"（仍能被 state IN ('pending','failed') 命中）。
--
-- 索引取自真实查询路径（列名与顺序严格对应 model/idempotency.go 的 SQL）：
--   1. PRIMARY KEY (id)：
--      · Claim 的 ON DUPLICATE KEY UPDATE id = id 借用它做"无变化更新"（最便宜的 no-op）；
--      · ListExpired 的 ORDER BY id ASC LIMIT ?（清理按插入顺序推进，避免尾部饿死）。
--   2. uniq_scope_key (scope, idempotency_key)：Claim 的冲突键；同时服务
--      find、MarkSucceeded、MarkFailed 的 WHERE scope = ? AND idempotency_key = ? 定位
--      —— 三条写语句都走这个键，不需要再建重复索引。
--      scope 在前是刻意的：三个作用域的清理/回放常按 scope 收窄，最左前缀即可命中。
--   3. idx_expire_at (expire_at)：
--      ListExpired「WHERE expire_at > 0 AND expire_at < ?」与
--      DeleteExpired「DELETE ... WHERE expire_at > 0 AND expire_at < ? LIMIT ?」。
--      收益集中在 DeleteExpired：清理语句只有这一个过滤条件，没有索引就要扫全表，
--      并给扫过而不删的行加锁（REPEATABLE READ 下还会加间隙锁），正是清理作业要避免的。
--      诚实说明：ListExpired 的 ORDER BY id ASC 让优化器可能改走主键顺序扫描 + 提前终止。
--   刻意不给 state / operator / lease_expire_at 建索引：
--   model 没有"按状态列取记录"的路径（状态只作为条件更新的一部分，定位仍靠 uniq_scope_key），
--   租约过期是重新认领路径上的二次条件而不是检索入口。本表行数受保留期约束
--   （config.Recall.IdempotencyRetentionSeconds，默认 86400 秒），加索引只放大写入
--   （AGENTS.md §4）。
--
-- 列宽依据（逐列取自 model/idempotency.go 与 internal/logic 的实际读写，不凭惯例）：
--   scope：受控三值中最长的是 "rollback_pool_version"（21 字符），VARCHAR(32)；
--     ValidIdempotencyScope 拒绝任何白名单外的串，所以宽度只是防御性余量。
--   idempotency_key：VARCHAR(128) —— 必须与 model.MaxIdempotencyKeyLen=128 完全一致，
--     该常量的注释本身就写着"与迁移本列列宽对齐"；超长在 Go 侧返回 ErrIdempotencyKeyTooLong，
--     不允许靠数据库静默截断（截断后两把不同的键会变成同一把）。
--   request_hash：CHAR(64) —— model.RequestHashLen=64，checkIdempotencyArgs 要求长度恰好等于 64
--     （repository.RequestFingerprint 产出 sha256 小写 hex，恒为 64 字符），
--     定长列既省空间也让"指纹不是一整套 hex"在列型层就无处藏身。
--   state：VARCHAR(16) —— 三值中最长 "succeeded"（9 字符）。
--   result_payload：TEXT NOT NULL，且不带 DEFAULT —— MySQL 禁止 TEXT/BLOB 设默认值；
--     Claim 的 INSERT 显式写空串，MarkSucceeded 回填。空串 = 已抢键但结果未落，
--     logic 的 replayPayload 据此返回包装后的 model.ErrIdempotencyExists 让调用方重发，
--     绝不回一个零值响应冒充上次结果。字节上限由 Go 侧 MaxIdempotencyPayloadLen=4096
--     （MarkSucceeded 超过即 ErrTooManyItems）把关，远低于 TEXT 的 65535 字节。
--   event_id：VARCHAR(64)，与 recall_outbox.event_id 同宽同 collation（切换类操作才回填，
--     其余为 Insert 写入的空串）。
--   operator：VARCHAR(64)，与本服务既有表的 operator 同宽；model 只要求非空白。
--   lease_expire_at：PENDING 租约到期（Unix 秒）= 认领时刻 + config.Recall.IdempotencyLeaseSeconds
--     （默认 300）；MarkSucceeded / MarkFailed 都把它置回 0（不再占租约）。
--   expire_at：保留期 = 认领时刻 + config.Recall.IdempotencyRetentionSeconds（默认 86400），
--     必须大于租约（config.Validate 有独立闸门），否则记录会在执行中还活着、执行完就被清掉。
--     0 表示"永不清理"，所以两条清理 SQL 都带 expire_at > 0，不能把 0 当成"已过期"。
--   execution_count：首次插入为 1（Insert 显式写 1），重新认领 +1 —— 观测重试风暴的口径。
--   last_error：VARCHAR(512)，MarkFailed 在 Go 侧按字节裁到 512，两侧口径一致。
--
-- 隐私（AGENTS.md §7）：不落请求原文，只落 sha256 指纹；result_payload 是回复投影
--   （版本、条数、event_id、deduplicated 等控制信息），本服务的写接口调用方是离线作业与
--   运营，不含终端用户标识；也不得出现投放/分成等商业化字段。
--
-- 容量：行数 ≈ 写接口调用数 × 保留期。三个作用域里只有 upsert_pool_items 随批次量增长
--   （cron 每批一次），publish/rollback 是低频人工/作业操作。
--   清理由 services/cron 调 ListExpired + DeleteExpired 分批执行，maxRows 上限
--   model.MaxDeleteRows=5000（ListExpired 单批上限 model.MaxOutboxBatch=1000）。
--
-- 回滚：
--   DROP TABLE IF EXISTS `recall_idempotency`;
--   回滚后 deduplicated 无法判定、所有写接口失去"只执行一次"的锚点：
--   重试会真实产生重复切换（表现为 switch_count 虚高与不必要的事件），
--   属降级而非无损回退 —— 回滚前必须先停写接口或明确接受重放语义退化。
--   本表是服务内唯一允许按保留期物理删除的表（它是去重锚点不是业务事实，过期即无价值）。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不改已存在的表；
--   Claim 的 INSERT 会在 uniq_scope_key 上加 next-key / 插入意向锁：同一
--   (scope, idempotency_key) 的并发重试会互相阻塞到对方提交 —— 这正是挡住重复副作用的机制。
--   因此认领必须以自动提交方式独立执行（logic 的 claimWrite 传 session=nil）：
--   若放进业务事务，回滚会连"我占了这把键"一起抹掉，重试就变成第二次真实执行；
--   同时租约（IdempotencyLeaseSeconds）给崩溃遗留的 PENDING 行设了上界，过期后允许重新认领。
--   MarkSucceeded 与业务写同事务提交（幂等标记与数据变更同生死）；MarkFailed 独立自动提交。
--   清理是大批量 DELETE，必须低峰按 maxRows 分批。后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `recall_idempotency` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键；Claim 的 ON DUPLICATE KEY UPDATE id = id 借用它做"无变化更新"，ListExpired 也按它升序推进',
  `scope`            VARCHAR(32) COLLATE utf8mb4_bin NOT NULL COMMENT '幂等作用域，取值只能是 model.IdempotencyScope* 三值之一（ValidIdempotencyScope 拒绝自由串）；唯一键左列，不给默认值（空 scope 意味着"不知道是哪个接口的键"）',
  `idempotency_key`  VARCHAR(128) COLLATE utf8mb4_bin NOT NULL COMMENT '调用方提供的幂等键（写入批次键/切换键）；宽度与 model.MaxIdempotencyKeyLen=128 严格一致，超长在 Go 侧硬失败而不允许数据库截断；与 scope 组成唯一键，必须逐字节比较',
  `request_hash`     CHAR(64) COLLATE utf8mb4_bin NOT NULL COMMENT 'sha256(请求关键字段) 的小写 hex（model.RequestFingerprint）：同键不同指纹即由 Claim 返回 ErrIdempotencyFingerprintMismatch；定长 64 由 model.RequestHashLen 约束，不建唯一键（那会破坏 Claim 的 affected 判定）',
  `state`            VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT '执行状态：pending 执行中（受 lease_expire_at 租约保护）、succeeded 已完成可回放、failed 已失败可立即重试；取值即 common/idempotency.State*，Claim 之前必须过 ValidIdempotencyState',
  `result_payload`   TEXT         NOT NULL COMMENT '首次执行的回复投影 JSON（重放时原样回给调用方）；字节上限由 Go 侧 model.MaxIdempotencyPayloadLen=4096 把关；空串 = 已抢键但结果未回填，调用方按 ErrIdempotencyExists 重发而不是执行第二次副作用。MySQL 禁止 TEXT 列设 DEFAULT，故由 Insert 显式写空串',
  `event_id`         VARCHAR(64) COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '本次执行产生的 recall_outbox.event_id（仅切换类操作回填）：事件与幂等记录一一对应的证据；MarkSucceeded 与 Outbox.Insert 同事务写入',
  `operator`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '执行者（离线作业或运营标识）；checkIdempotencyArgs 要求非空白，重新认领时刷新为最新执行者',
  `lease_expire_at`  BIGINT       NOT NULL DEFAULT 0 COMMENT 'PENDING 租约到期时间（Unix 秒）= 认领时刻 + config.Recall.IdempotencyLeaseSeconds；过期后允许被重新认领（条件 UPDATE 带 lease_expire_at <= now 保证并发只有一个赢家）；MarkSucceeded/MarkFailed 置 0 表示不再占租约',
  `expire_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '记录保留期限（Unix 秒）= 认领时刻 + config.Recall.IdempotencyRetentionSeconds（必须大于租约）；0 表示永不清理，故清理 SQL 一律带 expire_at > 0；本服务唯一允许物理删除的表',
  `execution_count`  INT          NOT NULL DEFAULT 0 COMMENT '同一键被执行（含重新认领尝试）的次数：首次插入写 1，之后每次重新认领 +1；持续大于 1 是重试风暴与租约过短的观测口径',
  `last_error`       VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次失败原因（MarkFailed 写入并在 Go 侧裁到 512 字节；MarkSucceeded 与重新认领会清空）；不含堆栈、SQL 片段与凭据',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，首次抢键时刻）；Claim 的 INSERT 用同一个 now 同时写 ctime 与 mtime',
  `mtime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）；与 ctime 之差即"抢键到出结果"的耗时',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_scope_key` (`scope`, `idempotency_key`),
  -- 保留期清理：ListExpired / DeleteExpired 的唯一定位条件（DELETE 扫全表会锁住不删的行）
  KEY `idx_expire_at` (`expire_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='写接口幂等控制表（scope+key 唯一键是 Claim 判重放/首执行的锚点：除主键外不得再加第二个唯一键；按 expire_at 分批清理）';
