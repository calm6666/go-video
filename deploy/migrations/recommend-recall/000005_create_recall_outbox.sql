-- =====================================================================
-- recommend-recall 服务 - 领域事件 Outbox 表（事务性事件产出证据）
-- =====================================================================
-- 用途：建立 recall_outbox，让「池版本切换成功」这一事实与产生它的数据变更在同一事务里落库，
--   再由独立发布器异步投递 recall.pool.published.v1（AGENTS.md §5 Outbox）。
--   唯一写入点是 internal/logic/poolswitch.go writePublishedEvent，
--   PublishPoolVersion 与 RollbackPoolVersion 共用同一条路径（rollback=true 区分）。
-- 数据所有者：recommend-recall 服务（AGENTS.md §5）。其他服务禁止直接读写本表。
--
-- 数据库：go_video_recommend_recall（建库由 scripts/migrate.ps1 负责，本文件只建表）。
--
-- 事实定位：本表是事实（事件产出证据），不是投影。
--   事件一旦丢弃就无法重建：切换成功后旧版本随时被 PrunePoolVersions 删掉，
--   「何时从版本 A 切到版本 B、当时声明多少条」只能由这行 payload 复述。
--   因此不提供任何"改事件内容"的方法：model/outbox.go 的写方法只有 Insert（新增）、
--   MarkSent/MarkFailed（只动 state/retry_count/next_retry_at/last_error/mtime）与
--   DeleteSentBefore（只删已投递行）。事件正文一经写入不可变。
--
-- 唯一键与幂等设计（model/outbox.go Insert 的实际语句为准）：
--   Insert 是 13 列的普通 INSERT（不含 id），靠 isDuplicateErr 把 MySQL 1062
--   翻译成 model.ErrEventExists —— 生产者重试或人工补偿脚本重复回放同一 event_id 时，
--   冲突即幂等命中，不会产生第二条同 ID 事件。这条判定要求 event_id 上有唯一索引：
--   uniq_event_id (event_id)。
--   event_id 用 utf8mb4_bin：它是消费者的去重键，比较必须逐字节精确。
--   表级 utf8mb4_unicode_ci 是 PAD SPACE + 大小写不敏感，'01H...' 与 '01h... '
--   会被折叠成同一个键，后果是"一个事件被静默当成另一个事件的重投而丢弃"。
--   本列同时刻意不给 DEFAULT ''：唯一键列的默认值会让漏赋值的插入静默变成
--   第二条空串（与 model.ErrEventRequired 的意图相反），必须让插入失败暴露出来。
--
-- 索引取自真实查询路径（列名与顺序严格对应 model/outbox.go 的 SQL）：
--   1. PRIMARY KEY (id)：
--      · ListPending 的 ORDER BY id ASC —— 发布器必须按产生顺序投递，否则消费者
--        会先收到新版本事件再收到旧版本；
--      · MarkSent 的 WHERE id IN (...)、MarkFailed 的 WHERE id = ?。
--   2. uniq_event_id (event_id)：上面所述幂等锚点。
--   3. idx_state_next_retry (state, next_retry_at, id)：
--      ListPending「WHERE state IN (PENDING, FAILED) AND next_retry_at <= ? ORDER BY id ASC LIMIT ?」
--      与 CountPending「WHERE state IN (PENDING, FAILED)」。
--      第三列 id 不是冗余：InnoDB 二级索引本就隐含末尾补主键，显式写出既让
--      「等值 state + 范围 next_retry_at + id 升序」在索引内对齐，也让 COUNT 走覆盖索引。
--      诚实说明：ORDER BY id ASC 使优化器在待发行占多数时可能改走主键顺序扫描 + 提前终止，
--      本索引的收益集中在"已投递行占绝对多数"的常态（那时它把待发行从全表里挑出来）。
--   4. idx_state_occurred (state, occurred_at)：
--      CountStuck「WHERE state IN (?, ?) AND occurred_at < ?」与
--      DeleteSentBefore「WHERE state = ? AND occurred_at < ? LIMIT ?」——
--      滞留告警与保留期清理都以 (状态, 发生时间) 定位，没有这个索引就要扫全表，
--      而清理是 DELETE，扫过的行还会被加锁。
--   刻意不给 aggregate_type / aggregate_id 建索引（其他服务的 outbox 有 idx_aggregate）：
--   本服务没有任何"按聚合根回查事件"的读路径（ListPending/MarkSent/CountStuck 都不带聚合条件），
--   预留索引只会为每次切换多写一棵树（AGENTS.md §4 不为不存在的查询建索引）。
--   要按池回查事件请读 recall_request_log.versions_digest 与 recall_pool_version，
--   它们才是给在线/运维查询用的表。
--
-- 列宽依据（不凭惯例，逐列取自 model/outbox.go 与 internal/logic/poolswitch.go 的实际读写）：
--   event_type：model.EventPoolPublished = "recall.pool.published"（21 字符），VARCHAR(64) 留余量；
--   aggregate_type：model.AggregateTypePool = "recall_pool"（11 字符），VARCHAR(32)；
--   aggregate_id：aggregateIDOf 生成 "%d:%s:%d" = source + ":" + pool_key + ":" + version，
--     pool_key 上限 model.MaxPoolKeyLen=128，source 最坏 11 字符（int32 含负号），
--     version 最坏 20 字符（int64 含负号）→ 最坏 161 字符，取 VARCHAR(191)；
--     这也是本列不能用其他服务 VARCHAR(64) 的原因：pool_key 单独就可能占满 128。
--   payload：完整 eventenvelope.Envelope JSON（信封字段 + poolPublishedPayload），
--     远小于 TEXT 的 65535 字节上限；TEXT NOT NULL 且不带 DEFAULT ——
--     MySQL 禁止 TEXT/BLOB 设默认值，而 Insert 总显式写入本列，因此不依赖默认值。
--   last_error：MarkFailed 在 Go 侧按字节裁到 512，utf8mb4 下 VARCHAR(512) 是 512 个字符
--     （512 字节 ≤ 512 字符恒成立），两侧口径一致，不会出现"裁过还超长"。
--   schema_version：默认 1 = model.PoolVersionSchemaVersion；Insert 在 0 时回填该值，
--     与 rpc PoolVersionReply.schema_version 的 self-check 同向。
--   state：SMALLINT 与本服务既有表一致（recall_pool.source、recall_pool_version.state），
--     默认 0 = model.OutboxStatePending（Go int32 零值即"待发布"，两侧零值语义相同）。
--
-- 隐私（AGENTS.md §7）：payload 只含 (source, pool_key, version, previous_version, batch_id,
--   item_count, generator, rollback, operator, published_at) —— 无 mid、无 IP、无设备号；
--   pool_key 的 mid:<mid> 形态在落库前已由 logic 的脱敏口径处理（见 recall_request_log.poolKeyForLog
--   对游客/个性化键的处理约定），本表不引入新的敏感维度，也不得出现投放/分成等商业化字段。
--
-- 容量：行数 = 发布/回滚次数。每次 PublishPoolVersion / RollbackPoolVersion 恰好一行
--   （见"事件与幂等的一一对应"：一次切换 → 一个 event_id → 一行 outbox + 一行幂等标记）。
--   保留期由 DeleteSentBefore 按 occurred_at 分批清理（model.MaxDeleteRows=5000 上限），
--   待发行与死信行不在清理范围内 —— 删掉未投递的事件等于丢事实。
--
-- 回滚：
--   DROP TABLE IF EXISTS `recall_outbox`;
--   警告：本表是事实。删除后已切换但未投递的池版本变化永久丢失，
--   下游（若已接入消费者）会停在旧快照且无法追赶，只能靠全量重建对账。
--   本期没有消费者，行停留在 state=0；README「已知缺口」据此不声称已投递。
--   回滚前必须先把 PublishPoolVersion / RollbackPoolVersion 下线（它们的
--   writePublishedEvent 与业务写在同一事务内，表不存在会让切换整体失败——这是失败关闭，
--   不会静默丢事件，但也意味着发布能力不可用）。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不改已存在的表；
--   Insert 与指针 CAS 同事务（model 要求 session 非 nil 用于发布路径），
--   持锁时间等于事务长度，因此本表写入必须留在事务内、且事务里不得调用外部 RPC；
--   uniq_event_id 上的并发同键插入会形成插入意向锁等待，这是期望行为（挡住重复事件）。
--   MarkSent/MarkFailed/CountPending/DeleteSentBefore 由独立发布器/清理作业以自动提交执行，
--   拉取与清理都必须带 LIMIT（model.MaxOutboxBatch=1000、model.MaxDeleteRows=5000），
--   否则一次锁住整张表。后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `recall_outbox` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（发布器按 id 升序投递，保证同库事件顺序）',
  `event_id`       VARCHAR(64) COLLATE utf8mb4_bin NOT NULL COMMENT '事件唯一 ID（eventenvelope.New 生成的 ULID，26 字符；列宽留 64 以容纳将来 idgen.Prefixed 加前缀）：消费者去重键与唯一索引，逐字节精确比较，不给默认值',
  `event_type`     VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '事件类型（model.EventPoolPublished = recall.pool.published）；投递 topic 由 event_type + schema_version 拼成，见 model.TopicPoolPublished',
  `schema_version` INT             NOT NULL DEFAULT 1 COMMENT 'payload schema 版本，默认 1 = model.PoolVersionSchemaVersion；0 由 Insert 回填为 1',
  `aggregate_type` VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '聚合根类型（model.AggregateTypePool = recall_pool）',
  `aggregate_id`   VARCHAR(191)    NOT NULL DEFAULT '' COMMENT '聚合根 ID，形如 source:pool_key:version（logic aggregateIDOf）；pool_key 本身最长 128，故本列必须宽于其他服务 outbox 的 VARCHAR(64)',
  `payload`        TEXT            NOT NULL COMMENT '事件信封完整 JSON（common/eventenvelope.Envelope）：正文只含池/版本/批次/条数/操作者，不含用户标识；MySQL 禁止 TEXT 列设 DEFAULT，故由 Insert 显式写入',
  `state`          SMALLINT        NOT NULL DEFAULT 0 COMMENT '发布状态：0 待发布、1 已发布、2 超过重试上限转人工（编号与 model.OutboxState* 一致）',
  `retry_count`    INT             NOT NULL DEFAULT 0 COMMENT '已重试次数（MarkFailed 每次 +1；maxRetry>0 时以 retry_count < maxRetry 为更新条件，达上限的行不再被改回待发布）',
  `next_retry_at`  BIGINT          NOT NULL DEFAULT 0 COMMENT '下次可投递时间（Unix 秒），0 表示可立即投递；ListPending 的到期条件',
  `occurred_at`    BIGINT          NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒，切换生效时刻；Insert 未传时回落 ctime）：滞留统计与保留期清理按此列',
  `last_error`     VARCHAR(512)    NOT NULL DEFAULT '' COMMENT '最近一次投递错误（Go 侧按字节裁到 512；不含 SQL 片段与凭据）',
  `ctime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，与业务写同事务）',
  `mtime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒，MarkSent/MarkFailed 刷新；state=1 时即发布时间）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  -- 发布器轮询与待发布统计：等值 state + 范围 next_retry_at，尾列 id 与 ORDER BY id ASC 同向
  KEY `idx_state_next_retry` (`state`, `next_retry_at`, `id`),
  -- 事件滞留告警（CountStuck）与已投递行清理（DeleteSentBefore）：等值 state + 范围 occurred_at
  KEY `idx_state_occurred` (`state`, `occurred_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='召回池事件 Outbox（与版本切换同事务写入的 recall.pool.published.v1 事件，按 event_id 幂等，发布器异步投递）';
