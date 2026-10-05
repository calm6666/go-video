-- =====================================================================
-- live-room 服务 - 幂等与事件去重表 live_room_idempotency
-- =====================================================================
-- 用途：为所有写 RPC 与所有入站事件提供「同一键只产生一次副作用」的落库锚点，
--       并保存首次执行的响应快照供重放原样返回。
--       覆盖 CreateRoom / UpdateRoomInfo / PrepareLive / StartLive / EndLive / CloseRoom /
--       BanRoom / LiftBan / MutateAnchor / UpdateRoomSetting / AttachReplay / UpsertArea 的
--       request_id，以及 ReportStreamState（live.state.v1）与 ApplyRoomModerationResult
--       （moderation.result.v1）的 event_id。
-- 数据所有者：live-room 服务（AGENTS.md §5）。
--       AGENTS.md §5 要求「所有写接口要设计幂等键」且「消费者必须按 event_id 去重」，
--       本表就是这两条在本服务的落地点。
-- 与 model 的对应：列顺序与 services/live-room/model/live_room_idempotency.go 的
--       liveRoomIdempotencyColumns 逐列一致。
-- 幂等依赖的唯一键（本表存在的唯一理由）：
--       uniq_dedup_key (dedup_key)
--           LiveRoomIdempotencyModel.Claim 用
--           INSERT ... ON DUPLICATE KEY UPDATE id = id 抢键：
--           MySQL 对真正插入的行返回 affected=1、对已存在的行返回 affected=0，
--           Claim 据此把「首次受理」与「重放/重复投递」区分开。
--       关键约束：除主键外本表只能有 uniq_dedup_key 这一个键。
--           一旦再加第二个唯一键（例如 (kind, dedup_key)），affected 的语义就会失真 ——
--           「命中新键的无变化更新」同样返回 0，首次受理会被误判为重放，
--           房间会被创建不出来、事件会被静默吞掉。要按 kind 检索请用普通二级索引，
--           绝不可升级为唯一键。
--       kind 只是键的来源分类（1 客户端 request_id、2 上游 event_id），
--           不参与唯一性：request_id 与 event_id 由 gateway/上游生成的都是全局唯一串，
--           真撞上了就是必须暴露的事故，不该被复合唯一键静默放过。
-- 为什么不用 Redis 代替：Redis 只能作前置加速，数据库唯一键是最终防线
--       （model 注释已锁定该口径）。Redis 丢数据不得导致重复开播或重复禁播。
-- result_json：首次执行成功后回填的响应快照（JSON，写入前必须已脱敏），
--       SaveResult 以 WHERE result_json = '' 为条件「先到先得」，后到的重放不覆盖既有事实。
--       列型用 TEXT 且不带 DEFAULT：MySQL 禁止 TEXT/BLOB 列设默认值，
--       而 model 的 INSERT 总是显式写入该列（未回填时为空串），所以不依赖默认值。
--       空串 = 键已抢占但结果尚未回填，此时调用方必须按 ErrIdempotencyResultMissing
--       让上游重试，绝不能自己再执行一遍副作用。
--       按前 100 字符估算，一次房间/场次响应的 JSON 远小于 TEXT 的 64KB 上限；
--       本列不建索引（无查询按内容检索，TEXT 也只能前缀索引）。
-- 敏感数据：result_json / trace_id 不得含推流密钥、对象存储凭据、明文 IP 或设备号
--       （AGENTS.md §6/§7）。rpc 列只存方法名，不存请求参数。
-- 索引依据（model 实际用到的访问路径）：
--       PRIMARY(id)             DeleteExpired 的 ORDER BY id ASC LIMIT ?
--       uniq_dedup_key          Claim 冲突键 + Find(dedup_key) + SaveResult 定位
--       idx_ctime               DeleteExpired(ctime < ?)，InnoDB 尾部隐含 id 与删除排序同向
--       刻意不给 room_id / session_id / kind 建索引：model 从不按它们检索，
--       本表按 IdempotencyRetentionDays 滚动清理、行数可控，加索引只放大写入。
-- 保留窗口与清理：保留期 config.LiveRoom.IdempotencyRetentionDays（默认 30 天）
--       必须大于「客户端最大重试间隔」与「MQ 最大重投间隔」，否则过期后幂等语义失效。
--       由 services/cron 调 DeleteExpired(before, limit) 分批物理删除 ——
--       这是本服务唯一允许物理删除的表（它是去重锚点不是业务事实，过期即无价值）。
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可在线重复执行。
--       每次写 RPC 会多一次本表 INSERT（与业务写入同事务时持锁时间等于事务长度）。
--       Claim 的 INSERT 会在唯一索引上加 next-key/插入意向锁：同一 dedup_key 的并发重试
--       会互相阻塞到对方提交，这是正确行为（正是它挡住重复副作用），
--       但因此**必须把 Claim 放在事务尽量靠后、或独立短事务里**，
--       不要让「已抢键但未提交」长时间挂住同键的重试方；抢键事务里禁止调用外部 RPC。
--       清理删除是大批量 DELETE，必须在低峰按 limit（SweepBatchLimit）分批执行。
-- 回滚：DROP TABLE IF EXISTS `live_room_idempotency`;
--       回滚后所有写 RPC 与事件消费失去去重锚点，重试会产生真实重复副作用
--       （重复建房、重复开播、重复禁播），属于降级而非无损回退：
--       回滚前必须先把写接口下线或改为只读。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_room_idempotency` (
  `id`          BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键；Claim 的 ON DUPLICATE KEY UPDATE id=id 只借用它做「无变化更新」',
  `dedup_key`   VARCHAR(64) COLLATE utf8mb4_bin NOT NULL COMMENT '去重键：客户端 request_id 或上游 event_id。用 utf8mb4_bin —— 键的比较必须逐字节精确，若按大小写不敏感 collation 折叠，两个仅大小写不同的 event_id 会被判为重复并静默丢弃一个事件',
  `kind`        TINYINT     NOT NULL COMMENT '键来源分类：1 request_id（写 RPC 重放）、2 event_id（live.state.v1 / moderation.result.v1 投递去重）；仅作标记与清理归因，不参与唯一性',
  `rpc`         VARCHAR(64) NOT NULL DEFAULT '' COMMENT '产生该键的 RPC 方法名（如 StartLive、ReportStreamState），排障时定位是哪条链路的键；不存请求参数',
  `room_id`     BIGINT      NOT NULL DEFAULT 0 COMMENT '关联房间 ID（观测与按房间排查用，非检索路径故不建索引），0 表示建档时尚未确定房间',
  `session_id`  BIGINT      NOT NULL DEFAULT 0 COMMENT '关联场次 ID，0 表示与场次无关',
  `result_json` TEXT        NOT NULL COMMENT '首次执行的响应快照（JSON，写入前已脱敏）；空串表示已抢键但结果未回填。MySQL 禁止 TEXT 列设 DEFAULT，故由 model 的 INSERT 显式写空串',
  `trace_id`    VARCHAR(64) NOT NULL DEFAULT '' COMMENT '首次执行的链路追踪 ID（不含明文 IP/设备号）',
  `ctime`       BIGINT      NOT NULL DEFAULT 0 COMMENT '首次受理时间（Unix 秒）；DeleteExpired 按本列判定保留窗口',
  `mtime`       BIGINT      NOT NULL DEFAULT 0 COMMENT '结果回填时间（Unix 秒）；与 ctime 的差即「抢键到出结果」的耗时，是幂等超时的观测指标',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_dedup_key` (`dedup_key`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='幂等与事件去重锚点（主键外只能有 uniq_dedup_key 一个键，否则 Claim 的 affected=1 判定失真；本服务唯一允许物理删除的表）';
