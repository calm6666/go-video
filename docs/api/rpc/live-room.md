# RPC · `live-room`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/live-room/rpc/liveroom.proto` |
| protobuf 包 | `liveroom.v1` |
| go_package | `go-video/services/live-room/rpc` |
| 发现用的 etcd key | `liveroom.v1.rpc`（`services/live-room/etc/liveroom.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`liveroom.v1.rpc`） |
| 监听 | `8119`（`services/live-room/etc/liveroom.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_live_room` |
| 方法数 | 21（service `LiveRoom`） |
| 网关消费方 | `app:LiveRoomRPC`、`admin:LiveRoomRPC` |

## 契约说明

> 说明：live-room 是「直播间业务状态」领域服务，是房间/主播绑定/场次/回放引用/
> 禁播与分区数据的唯一所有者（AGENTS.md §5）。
>
> 与 live-ingest 的边界（本期强约束，见 services/live-room/README.md §1）：
>   - 房间状态与推流状态分离：本服务不保存推流密钥（含哈希）、不保存流健康度、
>     不做接入节点分配；这些一律归 live-ingest。
>   - 本服务只保存 stream_id 引用（不校验、不回查 ingest 库），流状态由
>     live-ingest 产出的 live.state.v1 事件经 ReportStreamState 推进，
>     按 event_id 去重、按 seq 拒绝乱序回退。
>   - 跨服务只传业务主键（mid / room_id / session_id / asset_id / aid），
>     本文件不 import 任何其他服务的 proto。
>
> 开播前置检查的外部读依赖：creator（主播身份，UpAttr from=2/3）、
> risk-control（GuardedAction.ACTION_LIVE_START）、
> moderation-orchestrator（ContentType.CONTENT_TYPE_LIVE 送审）。
> 一律走 RPC 只读，禁止直连他人库表。

## service `LiveRoom`

> LiveRoom 直播间业务状态服务。 / 数据所有权见 AGENTS.md §5：房间、主播绑定、场次、禁播、分区、配置与回放引用 / 全部归本服务；房间状态只能由本服务状态机推进， / 外部（live-ingest 事件、moderation 结论）只能通过带 event_id 幂等的入口推进合法状态。

gRPC 方法前缀：`liveroom.v1.LiveRoom/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `CreateRoom` | [`CreateRoomReq`](#message-createroomreq) | [`CreateRoomReply`](#message-createroomreply) | 创建直播间：校验分区有效与房间数上限 → 落 PENDING → 建绑定与配置 → 送资料审核 |
| 2 | `UpdateRoomInfo` | [`UpdateRoomInfoReq`](#message-updateroominforeq) | [`UpdateRoomInfoReply`](#message-updateroominforeply) | 修改标题/封面/分区：终态房间不可改；改动后重新送审（PENDING/READY 才允许） |
| 3 | `GetRoom` | [`GetRoomReq`](#message-getroomreq) | [`GetRoomReply`](#message-getroomreply) | 读房间（可按 room_id 或房主 mid），可附带配置与进行中场次 |
| 4 | `ListRooms` | [`ListRoomsReq`](#message-listroomsreq) | [`ListRoomsReply`](#message-listroomsreply) | 分页浏览房间（发现页/主播主页/运营列表） |
| 5 | `PrepareLive` | [`PrepareLiveReq`](#message-preparelivereq) | [`PrepareLiveReply`](#message-preparelivereply) | 开播前置检查：主播资格(creator) + 风控(risk-control) + 资料审核 + 未禁播，全通过才 PENDING→READY |
| 6 | `StartLive` | [`StartLiveReq`](#message-startlivereq) | [`StartLiveReply`](#message-startlivereply) | 开播：READY→LIVING 并新建场次（不接收推流密钥，只登记 stream_id 引用） |
| 7 | `EndLive` | [`EndLiveReq`](#message-endlivereq) | [`EndLiveReply`](#message-endlivereply) | 下播：LIVING→READY 并把场次置为 ENDED（时长簿记） |
| 8 | `CloseRoom` | [`CloseRoomReq`](#message-closeroomreq) | [`CloseRoomReply`](#message-closeroomreply) | 关闭房间：任意非终态 → FINISHED，强制终止进行中场次并保留审计 |
| 9 | `ReportStreamState` | [`ReportStreamStateReq`](#message-reportstreamstatereq) | [`ReportStreamStateReply`](#message-reportstreamstatereply) | 推流状态事件入口（live.state.v1 消费者或 live-ingest 直调）：event_id 去重 + seq 乱序守卫 |
| 10 | `ApplyRoomModerationResult` | [`ApplyRoomModerationResultReq`](#message-applyroommoderationresultreq) | [`ApplyRoomModerationResultReply`](#message-applyroommoderationresultreply) | 资料审核结论回写（moderation.result.v1 消费者入口），按合法状态机推进 |
| 11 | `BanRoom` | [`BanRoomReq`](#message-banroomreq) | [`BanRoomReply`](#message-banroomreply) | 禁播：进入 BANNED 并终止进行中场次（运营/系统，需 operator_mid） |
| 12 | `LiftBan` | [`LiftBanReq`](#message-liftbanreq) | [`LiftBanReply`](#message-liftbanreply) | 解除禁播：BANNED→READY |
| 13 | `ListRoomBans` | [`ListRoomBansReq`](#message-listroombansreq) | [`ListRoomBansReply`](#message-listroombansreply) | 分页查询禁播记录（运营侧审计） |
| 14 | `GetSession` | [`GetSessionReq`](#message-getsessionreq) | [`GetSessionReply`](#message-getsessionreply) | 读场次（按 session_id，或按 room_id 取最近 N 场之一） |
| 15 | `ListSessions` | [`ListSessionsReq`](#message-listsessionsreq) | [`ListSessionsReply`](#message-listsessionsreply) | cursor 分页拉取历史场次 |
| 16 | `AttachReplay` | [`AttachReplayReq`](#message-attachreplayreq) | [`AttachReplayReply`](#message-attachreplayreply) | 关联回放：只写 record/asset/aid 引用与回放状态，不落媒资数据 |
| 17 | `UpdateRoomSetting` | [`UpdateRoomSettingReq`](#message-updateroomsettingreq) | [`UpdateRoomSettingReply`](#message-updateroomsettingreply) | 更新直播配置（弹幕/回复/录制/连麦/直播类型） |
| 18 | `MutateAnchor` | [`MutateAnchorReq`](#message-mutateanchorreq) | [`MutateAnchorReply`](#message-mutateanchorreply) | 绑定或解绑主播（房主/联合主播/房管），含单主播房间数上限校验 |
| 19 | `ListAnchors` | [`ListAnchorsReq`](#message-listanchorsreq) | [`ListAnchorsReply`](#message-listanchorsreply) | 分页查询房间主播绑定 |
| 20 | `UpsertArea` | [`UpsertAreaReq`](#message-upsertareareq) | [`UpsertAreaReply`](#message-upsertareareply) | 运营侧新建/修改直播分区 |
| 21 | `ListAreas` | [`ListAreasReq`](#message-listareasreq) | [`ListAreasReply`](#message-listareasreply) | 分区列表（客户端与运营共用） |

## 消息与枚举

### message `EmptyReply`

> 通用空响应（幂等重放场景由业务 reply 自带 replayed 标记）

（空消息）

### enum `RoomState`

> 房间业务状态机（与 live_room.state 列一致，取值不可变更）

| 值 | 编号 | 说明 |
|---|---|---|
| `ROOM_STATE_UNSPECIFIED` | 0 | 未指定，视为非法入参 |
| `ROOM_STATE_PENDING` | 1 | 待完善：创建后的初始态，资料未审核通过 |
| `ROOM_STATE_READY` | 2 | 可开播：资料审核通过 + 主播资格与风控检查通过 |
| `ROOM_STATE_LIVING` | 3 | 直播中：存在一个 LIVING 场次 |
| `ROOM_STATE_FINISHED` | 4 | 已关闭：终态，房间不再复用（历史与审计保留） |
| `ROOM_STATE_BANNED` | 5 | 违规禁播：由 BanRoom 进入，LiftBan 或到期回 READY |
| `ROOM_STATE_DISABLED` | 6 | 停用：主播自行停用或运营下架（非违规），可回 READY |

### enum `VerifyState`

> 资料审核状态（与 live_room.verify_state 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `VERIFY_STATE_UNSPECIFIED` | 0 | 未指定 |
| `VERIFY_STATE_NONE` | 1 | 未提交审核 |
| `VERIFY_STATE_REVIEWING` | 2 | 审核中（已提交 moderation） |
| `VERIFY_STATE_PASSED` | 3 | 通过 |
| `VERIFY_STATE_REJECTED` | 4 | 驳回：需改资料后重新送审 |

### enum `SessionState`

> 直播场次状态（与 live_session.state 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `SESSION_STATE_UNSPECIFIED` | 0 | 未指定 |
| `SESSION_STATE_PENDING` | 1 | 已建档，等待推流到达 |
| `SESSION_STATE_LIVING` | 2 | 直播中 |
| `SESSION_STATE_ENDED` | 3 | 正常下播（终态） |
| `SESSION_STATE_TERMINATED` | 4 | 异常终止：禁播/关闭房间/断流超时（终态） |

### enum `EndReason`

> 场次终止原因（与 live_session.end_reason 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `END_REASON_UNSPECIFIED` | 0 | 未指定 |
| `END_REASON_ANCHOR_STOP` | 1 | 主播主动下播 |
| `END_REASON_BANNED` | 2 | 风控/运营禁播 |
| `END_REASON_ROOM_CLOSED` | 3 | 房间被关闭 |
| `END_REASON_STREAM_TIMEOUT` | 4 | 断流超过宽限期未重连 |
| `END_REASON_STREAM_REPLAY` | 5 | 收到更晚序号的停止事件补偿 |

### enum `ReplayState`

> 回放状态（与 live_session.replay_state 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `REPLAY_STATE_UNSPECIFIED` | 0 | 未指定 |
| `REPLAY_STATE_NONE` | 1 | 无回放 |
| `REPLAY_STATE_PROCESSING` | 2 | 录制/转码中（由 live-media 推进） |
| `REPLAY_STATE_AVAILABLE` | 3 | 可回放 |
| `REPLAY_STATE_REMOVED` | 4 | 回放已下架（版权撤回或违规） |

### enum `AnchorRole`

> 主播绑定角色（与 live_room_anchor.role 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `ANCHOR_ROLE_UNSPECIFIED` | 0 | 未指定 |
| `ANCHOR_ROLE_OWNER` | 1 | 房主：一个房间同一时刻只有一个生效房主 |
| `ANCHOR_ROLE_COHOST` | 2 | 联合主播（连麦嘉宾，不含商业化分成语义） |
| `ANCHOR_ROLE_MANAGER` | 3 | 房间管理员（房管） |

### enum `AnchorAction`

> 主播绑定动作

| 值 | 编号 | 说明 |
|---|---|---|
| `ANCHOR_ACTION_UNSPECIFIED` | 0 | 未指定，服务端拒绝 |
| `ANCHOR_ACTION_BIND` | 1 | 绑定或重新启用 |
| `ANCHOR_ACTION_UNBIND` | 2 | 解绑（保留行，state=0） |

### enum `BanType`

> 禁播类型（与 live_room_ban.ban_type 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `BAN_TYPE_UNSPECIFIED` | 0 | 未指定 |
| `BAN_TYPE_TEMPORARY` | 1 | 临时禁播，end_at 必须 > start_at |
| `BAN_TYPE_PERMANENT` | 2 | 永久禁播，end_at = 0，必须 LiftBan 解除 |

### enum `ModerationVerdict`

> 审核结论（取值与 moderation.v1.Verdict 对齐，本服务自定义以避免 import）

| 值 | 编号 | 说明 |
|---|---|---|
| `VERDICT_UNSPECIFIED` | 0 | 未指定，服务端拒绝 |
| `VERDICT_PASS` | 1 | 通过：资料审核置为 PASSED |
| `VERDICT_REVIEW` | 2 | 转人审：保持 REVIEWING |
| `VERDICT_REJECT` | 3 | 驳回：置为 REJECTED，房间回 PENDING |

### enum `Platform`

> 客户端平台（与 live_room.platform 列一致，AGENTS.md §6 不写死单一端）

| 值 | 编号 | 说明 |
|---|---|---|
| `PLATFORM_UNSPECIFIED` | 0 | 未指定（服务端按 android 记录） |
| `PLATFORM_ANDROID` | 1 | — |
| `PLATFORM_IOS` | 2 | — |
| `PLATFORM_HARMONY` | 3 | — |
| `PLATFORM_DESKTOP` | 4 | 电脑客户端（含直播 OBS 类推流助手） |

### enum `RoomOrder`

> 房间列表排序

| 值 | 编号 | 说明 |
|---|---|---|
| `ROOM_ORDER_UNSPECIFIED` | 0 | 默认：room_id 倒序 |
| `ROOM_ORDER_LIVING_FIRST` | 1 | 直播中优先，其次 room_id 倒序 |
| `ROOM_ORDER_CTIME_DESC` | 2 | 创建时间倒序 |

### message `RoomInfo`

> 直播间主体（live_room 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `owner_mid` | `int64` | 2 | — | 房主用户 ID（live_room_anchor 生效房主的投影） |
| `title` | `string` | 3 | — | 房间标题 |
| `cover` | `string` | 4 | — | 封面对象引用（object key 或站内相对地址，不含签名） |
| `area_id` | `int64` | 5 | — | 直播分区 ID |
| `state` | [`RoomState`](#enum-roomstate) | 6 | — | 房间业务状态 |
| `verify_state` | [`VerifyState`](#enum-verifystate) | 7 | — | 资料审核状态 |
| `active_session_id` | `int64` | 8 | — | 当前场次 ID，0 表示无进行中场次 |
| `active_stream_id` | `string` | 9 | — | 当前场次关联的 stream_id 引用（无则空串） |
| `state_version` | `int32` | 10 | — | 状态版本号，每次状态迁移 +1（乐观并发与事件乱序守卫） |
| `reject_reason` | `string` | 11 | — | 资料驳回原因（verify_state=REJECTED 时有值） |
| `ban_until` | `int64` | 12 | — | 生效禁播的到期时间（Unix 秒），0 表示无禁播/永久 |
| `ctime` | `int64` | 13 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 14 | — | 修改时间（Unix 秒） |

### message `RoomSetting`

> 直播配置（live_room_setting 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `danmaku_enabled` | `bool` | 2 | — | 是否开启弹幕 |
| `reply_enabled` | `bool` | 3 | — | 是否开启回复/评论 |
| `record_enabled` | `bool` | 4 | — | 是否录制回放（回放归 live-media 产出，本服务只存开关） |
| `linkmic_enabled` | `bool` | 5 | — | 是否允许连麦 |
| `live_type` | `int32` | 6 | — | 直播类型：1 视频直播、2 语音直播、3 屏幕分享 |
| `min_client_version_code` | `int32` | 7 | — | 允许的最低客户端版本号（0 不限制） |
| `mtime` | `int64` | 8 | — | 修改时间（Unix 秒） |

### message `SessionInfo`

> 直播场次（live_session 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `int64` | 1 | — | 场次 ID |
| `room_id` | `int64` | 2 | — | 房间 ID |
| `mid` | `int64` | 3 | — | 开播主播 ID |
| `state` | [`SessionState`](#enum-sessionstate) | 4 | — | 场次状态 |
| `title_snapshot` | `string` | 5 | — | 开播时标题快照（改标题不影响历史） |
| `area_id_snapshot` | `int64` | 6 | — | 开播时分区的快照 |
| `stream_id` | `string` | 7 | — | 推流标识引用（由 live-ingest 拥有，本字段只存值） |
| `started_at` | `int64` | 8 | — | 实际开播时间（Unix 秒，0 表示未开播） |
| `ended_at` | `int64` | 9 | — | 结束时间（Unix 秒，0 表示进行中） |
| `duration_seconds` | `int64` | 10 | — | 直播时长（秒） |
| `end_reason` | [`EndReason`](#enum-endreason) | 11 | — | 终止原因 |
| `last_stream_seq` | `int64` | 12 | — | 已应用的最大流事件序号（乱序守卫） |
| `replay_state` | [`ReplayState`](#enum-replaystate) | 13 | — | 回放状态 |
| `record_id` | `int64` | 14 | — | live-media 录制记录 ID 引用，0 表示无 |
| `record_asset_id` | `int64` | 15 | — | 回放媒资 asset_id 引用，0 表示无 |
| `record_aid` | `int64` | 16 | — | 回放稿件 aid 引用，0 表示无 |
| `moderation_task_id` | `int64` | 17 | — | 开播送审任务 ID（0 表示未送审） |
| `ctime` | `int64` | 18 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 19 | — | 修改时间（Unix 秒） |

### message `RoomBanInfo`

> 禁播记录（live_room_ban 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ban_id` | `int64` | 1 | — | 禁播记录 ID |
| `room_id` | `int64` | 2 | — | 房间 ID |
| `mid` | `int64` | 3 | — | 被禁主播 ID |
| `ban_type` | [`BanType`](#enum-bantype) | 4 | — | 禁播类型 |
| `reason` | `string` | 5 | — | 禁播原因（运营内部说明，不下发终端） |
| `start_at` | `int64` | 6 | — | 生效时间（Unix 秒） |
| `end_at` | `int64` | 7 | — | 结束时间（Unix 秒），0 表示永久 |
| `state` | `int32` | 8 | — | 1 生效、2 已解除、3 已过期 |
| `operator_mid` | `int64` | 9 | — | 下发运营/系统 ID |
| `lift_operator_mid` | `int64` | 10 | — | 解除运营 ID，0 表示未解除 |
| `lift_reason` | `string` | 11 | — | 解除原因 |
| `lifted_at` | `int64` | 12 | — | 解除时间（Unix 秒） |
| `ctime` | `int64` | 13 | — | 创建时间（Unix 秒） |

### message `AreaInfo`

> 直播分区（live_area 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `area_id` | `int64` | 1 | — | 分区 ID |
| `area_name` | `string` | 2 | — | 分区名 |
| `parent_area_id` | `int64` | 3 | — | 上级分区 ID，0 表示一级分区 |
| `sort` | `int32` | 4 | — | 排序权重（越小越前） |
| `state` | `int32` | 5 | — | 1 启用、0 停用 |
| `operator_mid` | `int64` | 6 | — | 最近操作运营 ID |
| `ctime` | `int64` | 7 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 8 | — | 修改时间（Unix 秒） |

### message `PrepareCheckItem`

> 开播前置检查结果（PrepareLive 明细，逐项可解释，不做静默放行）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `code` | `string` | 1 | — | 检查项稳定 key：anchor_qualification / risk_control / room_verified / not_banned / setting_ok |
| `passed` | `bool` | 2 | — | 是否通过 |
| `detail` | `string` | 3 | — | 脱敏说明（不含下游原始响应与任何凭据） |
| `degraded` | `bool` | 4 | — | true 表示下游不可用导致该项未能评估（按未通过处理） |

### message `CreateRoomReq`

> --- 房间生命周期 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 主播用户 ID（gateway 注入，本服务不解析 token） |
| `title` | `string` | 2 | — | 房间标题（1~80 字符） |
| `cover` | `string` | 3 | — | 封面对象引用 |
| `area_id` | `int64` | 4 | — | 直播分区 ID（必须启用） |
| `platform` | [`Platform`](#enum-platform) | 5 | — | 创建端 |
| `app_version` | `string` | 6 | — | 客户端版本号 |
| `setting` | [`RoomSetting`](#message-roomsetting) | 7 | — | 初始配置，缺省按服务端默认 |
| `request_id` | `string` | 8 | — | 幂等键，客户端重试必须复用同一个值 |
| `trace_id` | `string` | 9 | — | 链路追踪 ID |

### message `CreateRoomReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID（幂等重放返回原值） |
| `state` | [`RoomState`](#enum-roomstate) | 2 | — | 初始状态，恒为 ROOM_STATE_PENDING |
| `verify_state` | [`VerifyState`](#enum-verifystate) | 3 | — | 初始资料审核状态 |
| `replayed` | `bool` | 4 | — | true 表示命中 request_id，未产生新写入 |
| `moderation_task_id` | `int64` | 5 | — | 送审任务 ID，0 表示未送审 |

### message `UpdateRoomInfoReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `operator_mid` | `int64` | 2 | — | 操作者：房主或生效联合主播 |
| `title` | `string` | 3 | — | 新标题，空串表示不修改 |
| `cover` | `string` | 4 | — | 新封面引用，空串表示不修改 |
| `area_id` | `int64` | 5 | — | 新分区 ID，0 表示不修改 |
| `request_id` | `string` | 6 | — | 幂等键 |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `UpdateRoomInfoReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room` | [`RoomInfo`](#message-roominfo) | 1 | — | 更新后的房间 |
| `replayed` | `bool` | 2 | — | true 表示命中 request_id，返回既有结果 |
| `moderation_task_id` | `int64` | 3 | — | 触发重新送审时的任务 ID，0 表示未送审 |

### message `GetRoomReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID（与 owner_mid 二选一，room_id 优先） |
| `owner_mid` | `int64` | 2 | — | 按房主查其生效中的房间（0 表示不按此查） |
| `with_setting` | `bool` | 3 | — | 是否附带配置 |
| `with_active_session` | `bool` | 4 | — | 是否附带进行中场次 |

### message `GetRoomReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room` | [`RoomInfo`](#message-roominfo) | 1 | — | 房间；不存在时 reply 为空且 code 由 gRPC 错误表达 |
| `setting` | [`RoomSetting`](#message-roomsetting) | 2 | — | with_setting=true 时返回 |
| `active_session` | [`SessionInfo`](#message-sessioninfo) | 3 | — | with_active_session=true 且进行中场次存在时返回 |

### message `ListRoomsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `owner_mid` | `int64` | 1 | — | 按房主过滤（0 不过滤） |
| `area_id` | `int64` | 2 | — | 按分区过滤（0 不过滤） |
| `state` | [`RoomState`](#enum-roomstate) | 3 | — | 按状态过滤（ROOM_STATE_UNSPECIFIED 不过滤） |
| `order` | [`RoomOrder`](#enum-roomorder) | 4 | — | 排序 |
| `page` | `int32` | 5 | — | 页码，从 1 开始 |
| `page_size` | `int32` | 6 | — | 每页大小（服务端截断到上限） |

### message `ListRoomsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rooms` | [`RoomInfo`](#message-roominfo) | 1 | repeated | 房间列表 |
| `total` | `int32` | 2 | — | 符合条件的总数 |
| `page` | `int32` | 3 | — | 实际返回页码 |
| `page_size` | `int32` | 4 | — | 实际每页大小 |

### message `PrepareLiveReq`

> --- 开播 / 下播 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `mid` | `int64` | 2 | — | 申请开播的主播 ID（必须是生效绑定主播） |
| `platform` | [`Platform`](#enum-platform) | 3 | — | 客户端平台 |
| `device_hash` | `string` | 4 | — | 设备摘要（风控入参，禁止传明文设备号） |
| `ip_hash` | `string` | 5 | — | 调用方预哈希的 IP 摘要（禁止传明文 IP） |
| `request_id` | `string` | 6 | — | 幂等键 |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `PrepareLiveReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `state` | [`RoomState`](#enum-roomstate) | 2 | — | 检查后的房间状态 |
| `checks` | [`PrepareCheckItem`](#message-preparecheckitem) | 3 | repeated | 逐项检查结果（可解释，顺序稳定） |
| `ready` | `bool` | 4 | — | 全部通过并可迁移到 READY 时为 true |
| `deny_code` | `string` | 5 | — | 未通过时的稳定错误码（anchor_not_verified / risk_denied / not_ready ...） |
| `retry_after_seconds` | `int64` | 6 | — | 建议重试间隔（秒），0 表示无需定时重试 |
| `replayed` | `bool` | 7 | — | true 表示命中 request_id 的结果回放 |

### message `StartLiveReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `mid` | `int64` | 2 | — | 开播主播 ID |
| `stream_id` | `string` | 3 | — | live-ingest 分配的推流标识引用（可空：推流到达后再回填） |
| `request_id` | `string` | 4 | — | 幂等键（同 request_id 重放返回同一 session_id） |
| `trace_id` | `string` | 5 | — | 链路追踪 ID |

### message `StartLiveReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `int64` | 1 | — | 新建场次 ID |
| `state` | [`RoomState`](#enum-roomstate) | 2 | — | 迁移后的房间状态（LIVING） |
| `started_at` | `int64` | 3 | — | 开播时间（Unix 秒） |
| `state_version` | `int32` | 4 | — | 迁移后的状态版本号 |
| `replayed` | `bool` | 5 | — | true 表示命中 request_id，未产生新场次 |

### message `EndLiveReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `session_id` | `int64` | 2 | — | 场次 ID（0 表示该房间当前进行中场次） |
| `mid` | `int64` | 3 | — | 操作主播 ID |
| `end_reason` | [`EndReason`](#enum-endreason) | 4 | — | 结束原因（仅允许 ANCHOR_STOP/STREAM_REPLAY，其余由内部方法写） |
| `request_id` | `string` | 5 | — | 幂等键 |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `EndLiveReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `int64` | 1 | — | 被结束的场次 ID |
| `session_state` | [`SessionState`](#enum-sessionstate) | 2 | — | 终态（ENDED） |
| `room_state` | [`RoomState`](#enum-roomstate) | 3 | — | 房间状态（回到 READY） |
| `duration_seconds` | `int64` | 4 | — | 本场时长（秒） |
| `replayed` | `bool` | 5 | — | true 表示幂等重放 |

### message `CloseRoomReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `operator_mid` | `int64` | 2 | — | 操作者：房主或运营（operator 权限由 gateway/admin 侧判定） |
| `admin` | `bool` | 3 | — | true 表示运营侧关闭 |
| `reason` | `string` | 4 | — | 关闭原因（审计留存） |
| `request_id` | `string` | 5 | — | 幂等键 |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `CloseRoomReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | [`RoomState`](#enum-roomstate) | 1 | — | 迁移后的状态（FINISHED） |
| `terminated_session_id` | `int64` | 2 | — | 被强制终止的进行中场次，0 表示无 |
| `replayed` | `bool` | 3 | — | true 表示幂等重放 |

### message `ReportStreamStateReq`

> --- 推流状态事件入口（live-ingest → live-room） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `event_id` | `string` | 1 | — | 事件唯一 ID（live.state.v1 的 event_id，去重锚点） |
| `room_id` | `int64` | 2 | — | 房间 ID |
| `session_id` | `int64` | 3 | — | 场次 ID，0 表示由本服务按进行中场次解析 |
| `stream_id` | `string` | 4 | — | 推流标识引用 |
| `stream_state` | `int32` | 5 | — | live-ingest 流状态：1 IDLE、2 PUBLISHING、3 INTERRUPTED、4 STOPPED |
| `stream_seq` | `int64` | 6 | — | 该流的单调事件序号（小于已应用序号的事件被丢弃） |
| `occurred_at` | `int64` | 7 | — | 事件发生时间（Unix 秒） |
| `interrupted_seconds` | `int64` | 8 | — | stream_state=4 时的本次累计中断秒数（观测用，可为 0） |
| `reason` | `string` | 9 | — | 原因摘要 |
| `trace_id` | `string` | 10 | — | 链路追踪 ID |

### message `ReportStreamStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `result` | `int32` | 1 | — | 1 已应用、2 重复投递、3 乱序丢弃、4 非法迁移未变更、5 房间/场次不匹配 |
| `room_state` | [`RoomState`](#enum-roomstate) | 2 | — | 处理后的房间状态 |
| `session_id` | `int64` | 3 | — | 关联场次 ID |
| `session_state` | [`SessionState`](#enum-sessionstate) | 4 | — | 处理后的场次状态 |
| `message` | `string` | 5 | — | 说明 |

### message `ApplyRoomModerationResultReq`

> --- 审核结论回写（moderation.result.v1 消费者入口） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `task_id` | `int64` | 2 | — | moderation 任务 ID |
| `verdict` | [`ModerationVerdict`](#enum-moderationverdict) | 3 | — | 审核结论 |
| `reason` | `string` | 4 | — | 结论原因 |
| `operator` | `int64` | 5 | — | 处理人，0 表示机审 |
| `event_id` | `string` | 6 | — | moderation.result.v1 的 event_id，消费去重 |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `ApplyRoomModerationResultReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `verify_state` | [`VerifyState`](#enum-verifystate) | 2 | — | 迁移后的资料审核状态 |
| `room_state` | [`RoomState`](#enum-roomstate) | 3 | — | 迁移后的房间状态 |
| `applied` | `bool` | 4 | — | false 表示重复投递或无需迁移 |
| `message` | `string` | 5 | — | 说明 |

### message `BanRoomReq`

> --- 禁播与解封 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `ban_type` | [`BanType`](#enum-bantype) | 2 | — | 禁播类型 |
| `duration_seconds` | `int64` | 3 | — | 临时禁播时长（秒），BAN_TYPE_TEMPORARY 必填且 > 0 |
| `reason` | `string` | 4 | — | 禁播原因 |
| `operator_mid` | `int64` | 5 | — | 下发运营 ID，必须 > 0 |
| `request_id` | `string` | 6 | — | 幂等键 |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `BanRoomReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ban_id` | `int64` | 1 | — | 禁播记录 ID（重放返回原值） |
| `state` | [`RoomState`](#enum-roomstate) | 2 | — | 房间状态（BANNED） |
| `terminated_session_id` | `int64` | 3 | — | 被强制终止的场次，0 表示无 |
| `end_at` | `int64` | 4 | — | 禁播到期时间（Unix 秒），0 表示永久 |
| `replayed` | `bool` | 5 | — | true 表示幂等重放 |

### message `LiftBanReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `ban_id` | `int64` | 2 | — | 指定解除的禁播记录，0 表示解除当前生效记录 |
| `operator_mid` | `int64` | 3 | — | 解除运营 ID，必须 > 0 |
| `reason` | `string` | 4 | — | 解除原因 |
| `request_id` | `string` | 5 | — | 幂等键 |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `LiftBanReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ban_id` | `int64` | 1 | — | 被解除的记录 ID，0 表示无生效记录 |
| `state` | [`RoomState`](#enum-roomstate) | 2 | — | 房间状态（BANNED → READY） |
| `replayed` | `bool` | 3 | — | true 表示幂等重放 |
| `message` | `string` | 4 | — | 说明 |

### message `ListRoomBansReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 按房间过滤（0 不过滤） |
| `mid` | `int64` | 2 | — | 按主播过滤（0 不过滤） |
| `state` | `int32` | 3 | — | 1 生效、2 已解除、3 已过期，0 不过滤 |
| `page` | `int32` | 4 | — | 页码，从 1 开始 |
| `page_size` | `int32` | 5 | — | 每页大小（最大 100） |
| `operator_mid` | `int64` | 6 | — | 查询运营 ID，必须 > 0 |

### message `ListRoomBansReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `bans` | [`RoomBanInfo`](#message-roombaninfo) | 1 | repeated | 禁播记录 |
| `total` | `int32` | 2 | — | 总数 |
| `page` | `int32` | 3 | — | 实际页码 |
| `page_size` | `int32` | 4 | — | 实际每页大小 |

### message `GetSessionReq`

> --- 场次与回放 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `int64` | 1 | — | 场次 ID（与 room_id 二选一，session_id 优先） |
| `room_id` | `int64` | 2 | — | room_id>0 且 session_id=0 时取该房间最近一场 |
| `offset` | `int32` | 3 | — | 从最近一场往前的偏移（0 表示最近一场） |

### message `GetSessionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session` | [`SessionInfo`](#message-sessioninfo) | 1 | — | 场次 |

### message `ListSessionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID，必填 |
| `mid` | `int64` | 2 | — | 按主播过滤（0 不过滤） |
| `state` | [`SessionState`](#enum-sessionstate) | 3 | — | 状态过滤（SESSION_STATE_UNSPECIFIED 不过滤） |
| `cursor` | `string` | 4 | — | 上一页 next_cursor，空表示第一页 |
| `page_size` | `int32` | 5 | — | 每页大小（最大 100） |

### message `ListSessionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `sessions` | [`SessionInfo`](#message-sessioninfo) | 1 | repeated | 场次列表，session_id 倒序 |
| `next_cursor` | `string` | 2 | — | 空表示到底 |

### message `AttachReplayReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `int64` | 1 | — | 场次 ID（必须已终态） |
| `room_id` | `int64` | 2 | — | 房间 ID（校验归属，防串改） |
| `record_id` | `int64` | 3 | — | live-media 录制记录 ID 引用 |
| `record_asset_id` | `int64` | 4 | — | 回放媒资 asset_id 引用（只存主键，不查 asset 库） |
| `record_aid` | `int64` | 5 | — | 回放稿件 aid 引用（0 表示未生成稿件） |
| `replay_state` | [`ReplayState`](#enum-replaystate) | 6 | — | 目标状态，仅允许 PROCESSING/AVAILABLE/REMOVED |
| `request_id` | `string` | 7 | — | 幂等键 |
| `trace_id` | `string` | 8 | — | 链路追踪 ID |

### message `AttachReplayReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `int64` | 1 | — | 场次 ID |
| `replay_state` | [`ReplayState`](#enum-replaystate) | 2 | — | 迁移后的回放状态 |
| `replayed` | `bool` | 3 | — | true 表示幂等重放 |

### message `UpdateRoomSettingReq`

> --- 配置 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `operator_mid` | `int64` | 2 | — | 操作者（房主或运营） |
| `setting` | [`RoomSetting`](#message-roomsetting) | 3 | — | 新配置（整段落库，字段为 0/false 视为显式关闭） |
| `request_id` | `string` | 4 | — | 幂等键 |
| `trace_id` | `string` | 5 | — | 链路追踪 ID |

### message `UpdateRoomSettingReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `setting` | [`RoomSetting`](#message-roomsetting) | 1 | — | 更新后的配置 |
| `replayed` | `bool` | 2 | — | true 表示幂等重放 |

### message `MutateAnchorReq`

> --- 主播绑定 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `operator_mid` | `int64` | 2 | — | 操作者：房主或运营 |
| `target_mid` | `int64` | 3 | — | 被绑定/解绑的主播 ID |
| `action` | [`AnchorAction`](#enum-anchoraction) | 4 | — | 动作 |
| `role` | [`AnchorRole`](#enum-anchorrole) | 5 | — | BIND 时必填；UNBIND 时可选（0 表示按 target_mid 解绑全部非房主） |
| `request_id` | `string` | 6 | — | 幂等键 |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `MutateAnchorReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `target_mid` | `int64` | 2 | — | 目标主播 |
| `state` | `int32` | 3 | — | 1 生效、0 已解绑 |
| `bound_count` | `int32` | 4 | — | 该主播当前生效的房主房间数（上限校验结果） |
| `replayed` | `bool` | 5 | — | true 表示幂等重放 |

### message `ListAnchorsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间 ID |
| `role` | [`AnchorRole`](#enum-anchorrole) | 2 | — | 角色过滤（ANCHOR_ROLE_UNSPECIFIED 不过滤） |
| `only_enabled` | `bool` | 3 | — | 仅生效绑定 |
| `page` | `int32` | 4 | — | 页码 |
| `page_size` | `int32` | 5 | — | 每页大小（最大 100） |

### message `ListAnchorsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `anchors` | [`AnchorInfo`](#message-anchorinfo) | 1 | repeated | 绑定列表 |
| `total` | `int32` | 2 | — | 总数 |

### message `AnchorInfo`

> 主播绑定条目（live_room_anchor 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | 记录 ID |
| `room_id` | `int64` | 2 | — | 房间 ID |
| `mid` | `int64` | 3 | — | 主播 ID |
| `role` | [`AnchorRole`](#enum-anchorrole) | 4 | — | 角色 |
| `state` | `int32` | 5 | — | 1 生效、0 已解绑 |
| `ctime` | `int64` | 6 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 7 | — | 修改时间（Unix 秒） |

### message `UpsertAreaReq`

> --- 分区（运营侧） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `area_id` | `int64` | 1 | — | 0 表示新建，> 0 表示修改 |
| `area_name` | `string` | 2 | — | 分区名（1~32 字符，全局唯一） |
| `parent_area_id` | `int64` | 3 | — | 上级分区 ID，0 表示一级 |
| `sort` | `int32` | 4 | — | 排序权重 |
| `state` | `int32` | 5 | — | 1 启用、0 停用 |
| `operator_mid` | `int64` | 6 | — | 操作运营 ID，必须 > 0 |
| `request_id` | `string` | 7 | — | 幂等键 |

### message `UpsertAreaReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `area_id` | `int64` | 1 | — | 分区 ID |
| `created` | `bool` | 2 | — | true 表示本次新建 |

### message `ListAreasReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `parent_area_id` | `int64` | 1 | — | 按上级过滤（-1 不过滤，0 表示只取一级） |
| `state` | `int32` | 2 | — | 1 启用、0 停用，-1 不过滤 |
| `page` | `int32` | 3 | — | 页码 |
| `page_size` | `int32` | 4 | — | 每页大小（最大 200） |

### message `ListAreasReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `areas` | [`AreaInfo`](#message-areainfo) | 1 | repeated | 分区列表 |
| `total` | `int32` | 2 | — | 总数 |
