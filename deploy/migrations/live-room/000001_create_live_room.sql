-- =====================================================================
-- live-room 服务 - 直播间主体表 live_room
-- =====================================================================
-- 用途：直播间的业务状态与资料真值。对应 RPC CreateRoom / UpdateRoomInfo / GetRoom /
--       ListRooms / PrepareLive / StartLive / EndLive / CloseRoom / BanRoom / LiftBan /
--       ReportStreamState / ApplyRoomModerationResult。
-- 数据所有者：live-room 服务（AGENTS.md §5）。本表是房间业务状态（state）的唯一事实源；
--       推流状态归 live-ingest、媒资归 live-media、审核结论归 moderation-orchestrator，
--       本表只存它们的业务主键引用（active_stream_id / moderation_task_id），
--       不建跨库外键、不复制对方可变主数据、绝不存推流密钥或其哈希。
-- 与 model 的对应：列顺序与 services/live-room/model/live_room.go 的 liveRoomColumns 逐列一致，
--       改任一列必须同时改本迁移与 model（新增 0000NN 文件，不修改已应用文件）。
-- 投影列（不是唯一事实源，可从事实重算）：
--       owner_mid 是 live_room_anchor 生效房主的投影（真值在绑定表，见 000003）；
--       active_session_id / active_stream_id 是 live_session 当前场次的投影（真值在 000004）。
--       三者由 MutateAnchor / StartLive / EndLive / ClearActiveSession 在同一事务内回写；
--       一致性校验/重建可由 services/cron 按绑定表与场次表重算，不需要人工修数。
-- 状态机：state 取值与 rpc.RoomState 严格一致且不可重排
--       （0 UNSPECIFIED、1 PENDING、2 READY、3 LIVING、4 FINISHED、5 BANNED、6 DISABLED）。
--       FINISHED 是终态。迁移一律走「条件 UPDATE + RowsAffected」
--       （WHERE room_id=? AND state=from [AND state_version=?]），禁止先读后写；
--       合法边由 model.CanRoomTransition 矩阵定义，本表不加 CHECK 约束（与全仓一致，
--       状态机在 Go 侧单点校验，避免 SQL 与 Go 两份矩阵漂移）。
-- 乐观并发：state_version 每次迁移 +1，既是 CAS 版本位也是事件乱序守卫；
--       INT 宽度按「一场直播最多几次迁移」留了 21 亿余量，不需回绕处理。
-- ban_until 语义：0 = 无禁播或永久禁播（永久禁播由 live_room_ban.end_at=0 表达，见 000005）。
--       到期扫描按 ban_until>0 AND ban_until<=now 取临时禁播，因此 0 不会被误解除。
-- 索引依据（model 实际用到的访问路径）：
--       PRIMARY(room_id)          FindOne / Transition / UpdateProfile / ClearActiveSession
--       idx_owner_room            ListByOwner、ListRooms(owner_mid)、CountByOwner
--       idx_area_room             ListRooms(area_id)、CountByArea（停用分区前的占用检查）
--       idx_state_room            ListRooms(state) + room_id 倒序
--       idx_state_ban             ListBansToExpire（state=5 AND 0<ban_until<=now，按 ban_until 升序）
--       idx_ctime                 ListRooms(order=ROOM_ORDER_CTIME_DESC)；InnoDB 二级索引隐含
--                                 (ctime, room_id)，正好匹配 "ctime DESC, room_id DESC"
-- 已知未覆盖路径（有意不建索引，登记在服务 README「已知缺口」）：
--       1) ROOM_ORDER_LIVING_FIRST 的 ORDER BY (state = 3) DESC 是表达式排序，索引给不了这个序，
--          必然 filesort；发现页在生产上应走 CacheRedis 榜单投影，直连本表时须带 area_id 过滤。
--       2) 无过滤条件的发现页列表 WHERE state > 0 匹配几乎全表，ORDER BY room_id DESC 退化成扫描；
--          同样按「必须带过滤条件或走缓存」约束调用方，不为此建全表索引。
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，只取元数据锁，可在线重复执行。
--       运行期 Transition 是单行 UPDATE（按 room_id 主键定位），持行锁到事务提交；
--       事务必须只包含「房间状态迁移 + live_room_state_log + live_session」，
--       不得在事务内调用 moderation/creator/risk-control 等外部 RPC，否则会长时间持锁。
--       上面两条未覆盖的排序路径是读侧开销风险，不加锁但会占 CPU，限流靠 page_size 上限。
-- 回滚：DROP TABLE IF EXISTS `live_room`;
--       房间不可物理删除（CloseRoom 只置 FINISHED 并保留审计），因此本表不提供 DELETE 型回滚；
--       若必须回退到建表前，需同时回滚 000003/000004/000005/000007（引用本表主键）。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_room` (
  `room_id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '房间 ID（主键，全局单调；对外可直接作为直播间号）',
  `owner_mid`         BIGINT       NOT NULL DEFAULT 0 COMMENT '房主用户 ID（live_room_anchor 生效房主的投影，真值在绑定表；换房主必须同事务回写）',
  `title`             VARCHAR(80)  NOT NULL DEFAULT '' COMMENT '房间标题（按 rune 计 1~80，与 config.LiveRoom.TitleMaxLength 同宽；不存 HTML/表情之外的控制字符由应用层保证）',
  `cover`             VARCHAR(255) NOT NULL DEFAULT '' COMMENT '封面对象引用（object key 或站内相对地址；禁止存带签名参数的 CDN 地址与对象存储密钥，AGENTS.md §6）',
  `area_id`           BIGINT       NOT NULL DEFAULT 0 COMMENT '直播分区 ID（引用 live_area.area_id，仅存主键不冗余分区名；分区改名不回写历史房间）',
  `state`             TINYINT      NOT NULL DEFAULT 1 COMMENT '房间业务状态：0 未指定、1 PENDING、2 READY、3 LIVING、4 FINISHED（终态）、5 BANNED、6 DISABLED；编号被 RPC 契约锁定不得重排',
  `verify_state`      TINYINT      NOT NULL DEFAULT 1 COMMENT '资料审核状态：0 未指定、1 NONE 未送审、2 REVIEWING、3 PASSED、4 REJECTED（默认 1：新建房间尚未送审）',
  `active_session_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '当前进行中场次 ID（live_session 投影），0 表示无进行中场次；与 state=LIVING 必须同事务变更',
  `active_stream_id`  VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '当前场次的 live-ingest 推流标识引用（不校验存在性、不回查 ingest 库）；空串表示无流。用 utf8mb4_bin 与 Go 侧字符串比较同口径',
  `state_version`     INT          NOT NULL DEFAULT 1 COMMENT '状态版本号：建档为 1，每次合法迁移 +1；作乐观并发 CAS 条件与事件乱序守卫',
  `reject_reason`     VARCHAR(255) NOT NULL DEFAULT '' COMMENT '资料驳回原因（verify_state=REJECTED 时有值；重新送审时必须清成空串，避免客户端显示旧理由）',
  `ban_until`         BIGINT       NOT NULL DEFAULT 0 COMMENT '生效禁播到期时间（Unix 秒）；0 表示无禁播或永久禁播（永久禁播的到期语义在 live_room_ban.end_at=0）',
  `platform`          TINYINT      NOT NULL DEFAULT 0 COMMENT '创建端：0 未指定、1 Android、2 iOS、3 HarmonyOS、4 电脑客户端（AGENTS.md §6 不得写死单端）',
  `app_version`       VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '创建时客户端版本号原样串（仅审计与灰度排查；版本比较用 live_room_setting.min_client_version_code）',
  `moderation_task_id` BIGINT      NOT NULL DEFAULT 0 COMMENT '最近一次资料送审任务 ID（moderation-orchestrator 主键引用），0 表示未送审；不存审核结论与规则明细',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）；与 ctime 由 model.nowUnix() 同一时刻取值，禁止用 DB 时钟（CURRENT_TIMESTAMP）以免双源',
  PRIMARY KEY (`room_id`),
  KEY `idx_owner_room` (`owner_mid`, `room_id`),
  KEY `idx_area_room` (`area_id`, `room_id`),
  KEY `idx_state_room` (`state`, `room_id`),
  KEY `idx_state_ban` (`state`, `ban_until`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='直播间主体：业务状态机 + 资料 + 当前场次投影（owner_mid/active_* 均可从事实表重算）';
