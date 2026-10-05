-- =====================================================================
-- live-room 服务 - 直播场次表 live_session
-- =====================================================================
-- 用途：一次开播的完整生命周期（建档 → 直播中 → 终态）与它的开播快照、
--       流事件序号、回放引用。对应 RPC StartLive / EndLive / GetSession / ListSessions /
--       AttachReplay / ReportStreamState。
-- 数据所有者：live-room 服务（AGENTS.md §5）。
--       跨服务只存业务主键引用：stream_id → live-ingest、record_id → live-media、
--       record_asset_id → asset、record_aid → video、moderation_task_id → moderation-orchestrator。
--       本表不存推流密钥（含任何哈希）、不存流健康度、不复制媒资元数据，
--       也不建跨库外键、不回查对方库（AGENTS.md §5/§6）。
-- 与 model 的对应：列顺序与 services/live-room/model/live_session.go 的
--       liveSessionColumns 逐列一致。
-- 为什么不另建回放表：一场直播最多一条回放引用，且与场次同生命周期
--       （model 注释已锁定该判断），拆表只会引入 1:1 join 与两处状态。
-- 快照列：title_snapshot / area_id_snapshot 是开播那一刻的值，之后改房间资料或分区
--       不得影响历史记录（AGENTS.md §8 审计留存）。这是本表唯一「故意冗余」的两列，
--       冗余的是不可变时刻的值，不是可变主数据。
-- 状态机（与房间状态分离，两者各自独立推进）：
--       state：0 UNSPECIFIED、1 PENDING 已建档等推流、2 LIVING、3 ENDED（终态）、
--              4 TERMINATED（终态，禁播/关房/断流超时）。
--       replay_state：0 未指定、1 NONE、2 PROCESSING、3 AVAILABLE、4 REMOVED。
--       合法边由 model.CanSessionTransition / CanReplayTransition 定义。
--       「进行中场次」= state IN (1,2)（model.ActiveSessionStates）；
--       「可关联回放」= state IN (3,4)（model 的 terminalSessionStates）。
-- 时长簿记：进入终态时由同一条 UPDATE 写 ended_at / end_reason /
--       duration_seconds = GREATEST(ended_at - started_at, 0) —— 在 SQL 侧算，
--       因为应用实例时钟有漂移，由调用方传 duration 会写出三列不自洽的行。
--       迁移到 LIVING 用 GREATEST(started_at, ?) 补写开播时间，断流重连不刷新开播点。
--       因此 started_at/ended_at 必须 NOT NULL DEFAULT 0（0 是「尚未发生」的哨兵值，
--       参与 GREATEST 运算，换成 NULL 会让整行时长算成 NULL）。
-- 乱序守卫：last_stream_seq 是「已应用的最大 live.state.v1 事件序号」。
--       AdvanceStreamSeq 以 seq 严格大于当前值为写入条件，是 model 中唯一的落库点；
--       事件投递本身的去重在 live_room_idempotency（000008），两者职责不同不可合并。
--       DEFAULT 0 表示从未应用过流事件。
-- 索引依据（model 实际用到的访问路径）：
--       PRIMARY(session_id)        FindOne / Transition / SetStreamID / AdvanceStreamSeq /
--                                  SetModerationTaskID / AttachReplay 的行定位
--       idx_room_session           List(room_id[,mid][,state][,session_id<?] ORDER BY session_id DESC)、
--                                  FindLatest(room_id ORDER BY session_id DESC LIMIT 1 OFFSET ?)
--                                  —— cursor 分页的比较列就是 session_id
--       idx_room_state_session     ListActiveByRoom/FindActiveByRoom(room_id AND state IN(1,2)
--                                  ORDER BY session_id DESC)
--       idx_state_session          CountActive（全库进行中场次数，观测与配额用）
--       AttachReplay 的 room_id + 终态 + replay_state 三重条件走 idx_room_session 定位后
--       残余过滤即可：单房间的场次数有上限（一生也就几千场），不为此再建复合索引。
--       刻意不给 mid 建索引：model 所有按 mid 的场次查询都同时带 room_id（List 要求 room_id>0），
--       多余的索引只会放大这张高频写入表的写开销。
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可在线重复执行。
--       Transition/AdvanceStreamSeq/AttachReplay 都是按主键的单行 UPDATE，行锁极短；
--       与 live_room 的迁移同事务时，固定「先 live_room 后 live_session」或反向之一
--       （model 的 TransitionTx 由调用方按 000001→000004 顺序编排），避免交叉死锁。
--       本表是这批表里写入最频繁的（每场直播 1 行 + 每次状态迁移 1 次 UPDATE +
--       每个流事件 1 次 UPDATE），归档策略见服务 README；不做物理删除。
-- 回滚：DROP TABLE IF EXISTS `live_session`;
--       场次历史是审计证据，删除即不可恢复；回滚前必须先回滚/停用 000001
--       （live_room.active_session_id 引用本表主键），否则投影列会变成悬空引用。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_session` (
  `session_id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT '场次 ID（主键，单调递增；ListSessions 的游标与 GetSession 的「最近第 N 场」都按它排序）',
  `room_id`             BIGINT       NOT NULL COMMENT '房间 ID（live_room 主键引用）',
  `mid`                 BIGINT       NOT NULL COMMENT '开播主播 ID（user-profile 引用；下播/重连不改写，保留「这场谁开的」的事实）',
  `state`               TINYINT      NOT NULL DEFAULT 1 COMMENT '场次状态：0 未指定、1 PENDING 已建档待推流、2 LIVING、3 ENDED 终态、4 TERMINATED 终态；与 live_room.state 分离且独立推进',
  `title_snapshot`      VARCHAR(80)  NOT NULL DEFAULT '' COMMENT '开播瞬间的标题快照（与 live_room.title 等宽，不得截断；此后改标题不影响本场记录）',
  `area_id_snapshot`    BIGINT       NOT NULL DEFAULT 0 COMMENT '开播瞬间的分区 ID 快照（分区改名/停用不回写历史场次）',
  `stream_id`           VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT 'live-ingest 推流标识引用（只存值，不校验存在性、不回查其库）。空串 = 尚未登记：SetStreamID 以 stream_id=? AND 原值为空串 为条件，只回填一次，重连事件不会把场次漂到另一条流上。用 utf8mb4_bin 与 Go 侧字符串比较同口径',
  `started_at`          BIGINT       NOT NULL DEFAULT 0 COMMENT '实际开播时间（Unix 秒），0 表示尚未进入 LIVING；PENDING→LIVING 用 GREATEST 补写，断流重连不刷新',
  `ended_at`            BIGINT       NOT NULL DEFAULT 0 COMMENT '结束时间（Unix 秒），0 表示进行中；进入终态时由同一条 UPDATE 落定',
  `duration_seconds`    BIGINT       NOT NULL DEFAULT 0 COMMENT '直播时长（秒）= SQL 侧 GREATEST(ended_at - started_at, 0)，禁止由应用层传入（多实例时钟漂移会写出不自洽的行）',
  `end_reason`          TINYINT      NOT NULL DEFAULT 0 COMMENT '终止原因：0 未指定、1 主播下播、2 禁播、3 房间关闭、4 断流超时、5 更晚序号的停止事件补偿；进入终态必填（model.ErrEndReasonInvalid）',
  `last_stream_seq`     BIGINT       NOT NULL DEFAULT 0 COMMENT '已应用的最大 live.state.v1 事件序号（乱序守卫唯一落库点：seq 严格大于此值才允许写入）',
  `replay_state`        TINYINT      NOT NULL DEFAULT 1 COMMENT '回放状态：0 未指定、1 NONE 无回放、2 PROCESSING 录制/转码中、3 AVAILABLE 可回放、4 REMOVED 已下架；建档默认 1',
  `record_id`           BIGINT       NOT NULL DEFAULT 0 COMMENT 'live-media 录制记录 ID 引用，0 表示无；只存主键，不复制录制任务的任何状态字段',
  `record_asset_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '回放媒资 asset_id 引用（asset 服务主键），0 表示未产出；播放地址由 playback 侧签名，本表不存 URL',
  `record_aid`          BIGINT       NOT NULL DEFAULT 0 COMMENT '回放稿件 aid 引用（video 服务主键），0 表示尚未生成稿件',
  `moderation_task_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '开播送审任务 ID（moderation-orchestrator 主键引用），0 表示未送审；审核结论与命中规则一律不在本表',
  `trace_id`            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '建档时的链路追踪 ID（跨服务排障用，不含用户敏感标识）',
  `ctime`               BIGINT       NOT NULL DEFAULT 0 COMMENT '建档时间（Unix 秒）',
  `mtime`               BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`session_id`),
  KEY `idx_room_session` (`room_id`, `session_id`),
  KEY `idx_room_state_session` (`room_id`, `state`, `session_id`),
  KEY `idx_state_session` (`state`, `session_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='直播场次：状态机 + 开播快照 + 流事件序号 + 回放引用（时长在 SQL 侧算，引用列只存对方主键）';
