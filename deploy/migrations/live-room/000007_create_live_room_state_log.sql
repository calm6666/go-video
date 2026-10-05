-- =====================================================================
-- live-room 服务 - 状态流转日志表 live_room_state_log
-- =====================================================================
-- 用途：房间 / 资料审核 / 场次 / 回放四类状态的每一次迁移留一行，
--       回答「这个房间什么时候变成这个状态、是谁或哪个事件促成的」。
--       供运营审计（GetRoom 排障、后台状态历史）与故障回放使用。
-- 数据所有者：live-room 服务（AGENTS.md §5）。
--       本表是「本服务状态变迁」的证据，不是审核结论的存放地：
--       审核意见与命中规则的真值在 moderation-orchestrator，本表只记
--       from_state/to_state 与来源标识（source=moderation_result）及 task/event 引用。
-- 与 model 的对应：列顺序与 services/live-room/model/live_room_state_log.go 的
--       liveRoomStateLogColumns 逐列一致。
--       本表同样没有 mtime：它是 append-only 的日志，任何一行都不允许被改写。
-- append-only 与写入方式：LiveRoomStateLogModel.Insert / InsertTx 只做 INSERT。
--       状态迁移与对应日志行必须在同一事务内提交（model.TransitionTx 存在的理由），
--       否则会出现「状态变了但没留下证据」（AGENTS.md §8）。
-- 唯一键说明：本表不设唯一键。同一房间在同一状态之间来回迁移是正常业务
--       （PENDING↔READY 每次改资料重审都会走一遍），去重发生在
--       live_room_idempotency（000008）而不是这里；给 (room_id, from, to, source) 建唯一键
--       会直接杀掉合法的第二次迁移。
-- state_type：1 房间状态、2 资料审核状态、3 场次状态、4 回放状态
--       （model.LogType* 常量；from_state/to_state 的含义随 state_type 变化，
--       因此这两列只解释为「该类别下的状态值」，不做跨表 join）。
--       未知取值由 model.insert 前置拒绝（ErrStateTypeInvalid），不入库。
-- source 取值（稳定字符串，新增来源要同步改 README 的状态机矩阵，禁止自由文本）：
--       rpc_client 终端经 gateway 触发 / rpc_admin 运营后台触发 /
--       stream_event live.state.v1（live-ingest）推进 / moderation_result 审核结论推进 /
--       cron services/cron 的到期补偿（禁播到期、断流超时兜底）。
--       VARCHAR(32) 足够容纳最长项 moderation_result（17 字符）。
-- 索引依据（model 实际用到的访问路径）：
--       PRIMARY(log_id)         两个列表接口都以 log_id 倒序输出，天然复用聚簇序
--       idx_room_type_log       ListByRoom(room_id[, state_type] ORDER BY log_id DESC)
--                               —— InnoDB 二级索引隐含尾部主键，正好匹配排序列
--       idx_ctime               ListByTimeRange(ctime BETWEEN ? AND ? ORDER BY log_id DESC)
--                               —— ctime 范围收敛后仍需按 log_id 排序；本表按时间追加，
--                                  时间窗与 log_id 序高度相关，实际排序集很小。
--       刻意不给 request_id / event_id / trace_id 建索引：model 从不按它们查询，
--       而这是全服务写入最频繁的表，每多一条索引就是每次状态迁移多一次索引维护。
--       按幂等键反查日志属于排障临时需求，走离线日志系统而不是主库索引。
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可在线重复执行。
--       本服务写入量最大、增长最快的表：每次房间/资料/场次/回放迁移各一行，
--       一次完整开播（开播 + 断流 + 重连 + 下播 + 回放推进）就能产生 5~10 行。
--       只有 INSERT 与只读 SELECT，不加行锁竞争；风险全在容量：
--       必须按 config.LiveRoom.StateLogRetentionDays（默认 365 天）由 services/cron
--       归档后分批 DELETE，不做物理清空、不 TRUNCATE。
--       因此本表禁止任何无时间范围的扫描 —— model.ListByTimeRange 对
--       from/to 缺失直接返回 ErrQueryRangeRequired，就是这个原因。
-- 回滚：DROP TABLE IF EXISTS `live_room_state_log`;
--       删除即永久丢失状态变迁证据，与 AGENTS.md §8 的审计要求冲突：
--       除非整服务下线，否则不得回滚本表；回滚不影响其它表的写入正确性（无人引用 log_id）。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_room_state_log` (
  `log_id`       BIGINT       NOT NULL AUTO_INCREMENT COMMENT '日志 ID（主键，单调递增；两个列表接口都按它倒序）',
  `room_id`      BIGINT       NOT NULL COMMENT '房间 ID（live_room 主键引用）',
  `session_id`   BIGINT       NOT NULL DEFAULT 0 COMMENT '关联场次 ID（live_session 主键引用），0 表示本次迁移与场次无关（如纯资料审核状态变化）',
  `state_type`   TINYINT      NOT NULL COMMENT '状态类别：1 房间状态、2 资料审核状态、3 场次状态、4 回放状态；未知取值由 model 拒绝入库（ErrStateTypeInvalid）',
  `from_state`   TINYINT      NOT NULL DEFAULT 0 COMMENT '迁移前状态值（含义随 state_type 变化，不与其它类别混用）',
  `to_state`     TINYINT      NOT NULL DEFAULT 0 COMMENT '迁移后状态值',
  `operator_mid` BIGINT       NOT NULL DEFAULT 0 COMMENT '操作人 ID，0 表示系统触发（事件消费、cron 补偿都是 0）',
  `source`       VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '迁移来源稳定字符串：rpc_client / rpc_admin / stream_event / moderation_result / cron；禁止自由文本，否则看板归因会碎裂',
  `request_id`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '触发本次迁移的幂等键（与 live_room_idempotency.dedup_key 同值，便于人工串联；空表示非客户端触发）',
  `event_id`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '触发本次迁移的上游事件 ID（live.state.v1 / moderation.result.v1）；空表示非事件驱动',
  `reason`       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '迁移原因摘要（运营内部说明，不下发终端；不含举报人标识、手机号、下游原始响应与任何凭据）',
  `trace_id`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID，与当次 RPC/消费的 trace 一致',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '记录时间（Unix 秒）；与对应的状态迁移同一事务、同一 nowUnix() 取值',
  PRIMARY KEY (`log_id`),
  KEY `idx_room_type_log` (`room_id`, `state_type`, `log_id`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='状态流转日志（append-only 审计证据，与状态迁移同事务写入；按 ctime 归档，不做物理清空）';
