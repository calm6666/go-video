# RPC · `live-gateway`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/live-gateway/rpc/livegateway.proto` |
| protobuf 包 | `livegateway.v1` |
| go_package | `go-video/services/live-gateway/rpc` |
| 发现用的 etcd key | `livegateway.v1.rpc`（`services/live-gateway/etc/livegateway.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`livegateway.v1.rpc`） |
| 监听 | `8121`（`services/live-gateway/etc/livegateway.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_live_gateway` |
| 方法数 | 22（service `LiveGateway`） |
| 网关消费方 | `admin:LiveGatewayRPC` |

## 契约说明

> live-gateway 服务：直播长连接（WebSocket）会话与房间广播的领域服务。
>
> 数据分层（本契约最重要的约束，AGENTS.md §5 + services/live-gateway/README.md）：
>   * 主存储 Redis（易失、可重建）：连接租约、房间订阅集合、心跳计数、广播去重与限流窗口、
>     重连票据的短期有效位。Redis 丢失只造成"客户端重连"，不造成业务事实丢失。
>   * 业务库 MySQL go_video_live_gateway（只存需要审计/重建的少数表）：
>     live_gw_room_route（房间→节点路由，重启后重建路由表）、
>     live_gw_access_quota（接入与广播配额配置）、
>     live_gw_broadcast_log（广播审计流水，只存摘要不存正文）、
>     live_gw_reconnect_ticket（重连票据签发/使用的审计与撤销名单）。
>   * 绝对不把逐条连接状态（每连接的在线表、心跳明细、订阅成员列表）写入 MySQL：
>     这类数据高频、易失、可从 Redis 与客户端重连恢复，落库只会拖垮主库。
>
> 安全约束：广播消息可以丢弃（限流、无路由、订阅者离线），但绝不伪造权限：
>   * 任何下发入口都必须先校验连接票据 / 租约的 (mid, room_id, 有效期) 三元组一致；
>   * 权限来源是调用方（gateway/app 已完成登录鉴权）传入的 mid + 本服务的路由/配额配置，
>     本服务不接受客户端自报的角色；
>   * 校验失败一律返回明确的 deny/drop 原因，不返回"成功下发 0 人"来掩盖越权。
>
> 本期不实现真实 WebSocket 服务端：仓库无 websocket 依赖（AGENTS.md 禁止新增依赖），
> 连接接入由 internal/connection 的 Manager 接口 + 显式 stub 承接，接入步骤见服务 README。
>
> 时间字段统一 Unix 秒（0 表示未设置），时长/间隔用毫秒或秒并在字段注释标明。
> 跨服务只传主键：room_id（live-room）、mid、asset_id/aid 等；本契约不 import 其它服务 proto。
>  
> ---------------------------------------------------------------------------
> 枚举
> ---------------------------------------------------------------------------

## service `LiveGateway`

> --------------------------------------------------------------------------- / 服务定义 / --------------------------------------------------------------------------- / LiveGateway 长连接租约、房间路由与广播服务。 / 不提供任何会员/付费直播/投币/广告相关的下发能力（AGENTS.md §1）。

gRPC 方法前缀：`livegateway.v1.LiveGateway/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `AcquireConnectionLease` | [`AcquireConnectionLeaseReq`](#message-acquireconnectionleasereq) | [`AcquireConnectionLeaseReply`](#message-acquireconnectionleasereply) | --- 连接租约与心跳（主存储 Redis） --- / 申请连接租约：校验 (mid, room_id, role) 与配额，签发 lease_id + 重连票据 |
| 2 | `RenewConnectionLease` | [`RenewConnectionLeaseReq`](#message-renewconnectionleasereq) | [`RenewConnectionLeaseReply`](#message-renewconnectionleasereply) | 续租（TTL 刷新）：三元组不匹配一律拒绝，不静默改绑 |
| 3 | `ReleaseConnectionLease` | [`ReleaseConnectionLeaseReq`](#message-releaseconnectionleasereq) | [`EmptyReply`](#message-emptyreply) | 释放租约（正常断开/切房） |
| 4 | `GetConnectionLease` | [`ConnectionLeaseReq`](#message-connectionleasereq) | [`ConnectionLeaseReply`](#message-connectionleasereply) | 查询租约（下发前的权限校验入口之一） |
| 5 | `ReportClientHeartbeat` | [`ReportClientHeartbeatReq`](#message-reportclientheartbeatreq) | [`ReportClientHeartbeatReply`](#message-reportclientheartbeatreply) | 客户端心跳簿记：推进 last_heartbeat/max_seq，抽样产出 QoE 事件 |
| 6 | `ListRoomConnections` | [`ListRoomConnectionsReq`](#message-listroomconnectionsreq) | [`ListRoomConnectionsReply`](#message-listroomconnectionsreply) | 房间在线连接列表（Redis 视图，运营排障与主播工具） |
| 7 | `IssueReconnectTicket` | [`IssueReconnectTicketReq`](#message-issuereconnectticketreq) | [`ReconnectTicketInfo`](#message-reconnectticketinfo) | --- 断线重连票据 --- / 以现有租约为凭据签发一次性重连票据 |
| 8 | `RedeemReconnectTicket` | [`RedeemReconnectTicketReq`](#message-redeemreconnectticketreq) | [`RedeemReconnectTicketReply`](#message-redeemreconnectticketreply) | 用票据换取新租约（校验 mid/room/有效期，一次性消费） |
| 9 | `RevokeReconnectTicket` | [`RevokeReconnectTicketReq`](#message-revokereconnectticketreq) | [`RevokeReconnectTicketReply`](#message-revokereconnectticketreply) | 撤销票据（封禁、踢人、房间关闭） |
| 10 | `JoinRoom` | [`JoinRoomReq`](#message-joinroomreq) | [`JoinRoomReply`](#message-joinroomreply) | --- 房间路由与订阅 --- / 加入房间（登记订阅关系 + 复用/创建房间路由） |
| 11 | `LeaveRoom` | [`LeaveRoomReq`](#message-leaveroomreq) | [`EmptyReply`](#message-emptyreply) | 退出房间 |
| 12 | `GetRoomRoute` | [`RoomRouteReq`](#message-roomroutereq) | [`RoomRouteInfo`](#message-roomrouteinfo) | 查询房间路由（广播第一跳与副本节点） |
| 13 | `ListRoomRoutes` | [`ListRoomRoutesReq`](#message-listroomroutesreq) | [`ListRoomRoutesReply`](#message-listroomroutesreply) | 分页查询房间路由（运营/发布排障） |
| 14 | `DrainRoomRoute` | [`DrainRoomRouteReq`](#message-drainroomroutereq) | [`RoomRouteInfo`](#message-roomrouteinfo) | 排空某节点上的房间路由（优雅下线，版本号乐观校验） |
| 15 | `BroadcastToRoom` | [`BroadcastToRoomReq`](#message-broadcasttoroomreq) | [`BroadcastToRoomReply`](#message-broadcasttoroomreply) | --- 广播、单播与事件转发 --- / 房间广播：权限矩阵 + 配额限流 + message_id 去重，允许丢弃但原因必须可解释 |
| 16 | `SendToUser` | [`SendToUserReq`](#message-sendtouserreq) | [`SendToUserReply`](#message-sendtouserreply) | 房间内单播（审核处置、私信提示等） |
| 17 | `ForwardDanmaku` | [`ForwardDanmakuReq`](#message-forwarddanmakureq) | [`ForwardDanmakuReply`](#message-forwarddanmakureply) | 弹幕转发（弹幕事实仍归 danmaku，本服务只扇出） |
| 18 | `ForwardSystemEvent` | [`ForwardSystemEventReq`](#message-forwardsystemeventreq) | [`ForwardSystemEventReply`](#message-forwardsystemeventreply) | 系统事件转发（开播/断流/下播/审核处置） |
| 19 | `KickConnection` | [`KickConnectionReq`](#message-kickconnectionreq) | [`KickConnectionReply`](#message-kickconnectionreply) | 强制下线（风控/审核/主播踢人），可同时撤销票据与写禁止重连窗口 |
| 20 | `ListBroadcastLogs` | [`ListBroadcastLogsReq`](#message-listbroadcastlogsreq) | [`ListBroadcastLogsReply`](#message-listbroadcastlogsreply) | --- 审计与配额配置 --- / 分页查询广播审计流水（按房间） |
| 21 | `GetAccessQuota` | [`AccessQuotaReq`](#message-accessquotareq) | [`AccessQuotaInfo`](#message-accessquotainfo) | 读取某作用域生效的配额（含继承链解析结果） |
| 22 | `UpsertAccessQuota` | [`UpsertAccessQuotaReq`](#message-upsertaccessquotareq) | [`AccessQuotaInfo`](#message-accessquotainfo) | 新建或更新配额配置（运营面，带版本与操作者审计） |

## 消息与枚举

### enum `Platform`

> 客户端平台（与 playback.Platform 编号保持一致，跨端排障不歧义；不支持小程序，AGENTS.md §1）。

| 值 | 编号 | 说明 |
|---|---|---|
| `PLATFORM_UNSPECIFIED` | 0 | 未指定 |
| `PLATFORM_ANDROID` | 1 | Android |
| `PLATFORM_IOS` | 2 | iOS |
| `PLATFORM_HARMONY` | 3 | HarmonyOS |
| `PLATFORM_DESKTOP` | 4 | 电脑客户端 |

### enum `ConnRole`

> 连接角色。注意：这是 live-gateway 下发权限的唯一依据， / 由调用方（已完成登录鉴权的 gateway/app 或内部服务）声明并由本服务对照房间归属校验， / 客户端自报角色一律按 VIEWER 处理。

| 值 | 编号 | 说明 |
|---|---|---|
| `CONN_ROLE_UNSPECIFIED` | 0 | 未指定（按 VIEWER 处理并记录告警） |
| `CONN_ROLE_VIEWER` | 1 | 观众 |
| `CONN_ROLE_ANCHOR` | 2 | 主播（room_id 必须等于其 own 的房间，否则拒绝） |
| `CONN_ROLE_ROOM_ADMIN` | 3 | 房间管理员（房管） |
| `CONN_ROLE_OPERATOR` | 4 | 运营（管理后台连接，只收系统事件） |
| `CONN_ROLE_SERVICE` | 5 | 内部服务（如 live-ingest 转发的机器事件） |

### enum `LeaseState`

> 租约状态（Redis 为事实源；DB 仅在审计视图里出现同样的取值）。

| 值 | 编号 | 说明 |
|---|---|---|
| `LEASE_STATE_UNSPECIFIED` | 0 | 未指定 |
| `LEASE_STATE_ACTIVE` | 1 | 有效（未过期） |
| `LEASE_STATE_EXPIRED` | 2 | 已过期（TTL 到，客户端需重连） |
| `LEASE_STATE_RELEASED` | 3 | 已释放（正常断开） |
| `LEASE_STATE_KICKED` | 4 | 被强制下线（风控/审核/主播踢人） |

### enum `TicketState`

> 重连票据状态（live_gw_reconnect_ticket.state）。

| 值 | 编号 | 说明 |
|---|---|---|
| `TICKET_STATE_UNSPECIFIED` | 0 | 未指定 |
| `TICKET_STATE_ISSUED` | 1 | 已签发，未使用 |
| `TICKET_STATE_USED` | 2 | 已换取新租约（一次性） |
| `TICKET_STATE_REVOKED` | 3 | 已撤销（封禁/踢人/房间关闭） |
| `TICKET_STATE_EXPIRED` | 4 | 已过期 |

### enum `RouteState`

> 房间路由状态（live_gw_room_route.state）。

| 值 | 编号 | 说明 |
|---|---|---|
| `ROUTE_STATE_UNSPECIFIED` | 0 | 未指定 |
| `ROUTE_STATE_SERVING` | 1 | 该节点承接房间广播 |
| `ROUTE_STATE_DRAINING` | 2 | 排空中（只出不进，节点优雅下线） |
| `ROUTE_STATE_OFFLINE` | 3 | 已下线（无有效路由） |

### enum `BroadcastKind`

> 广播消息类别（live_gw_broadcast_log.kind）。 / 只包含社区与运行事件：不含会员、投币、支付、广告等商业化消息（AGENTS.md §1）。

| 值 | 编号 | 说明 |
|---|---|---|
| `BROADCAST_KIND_UNSPECIFIED` | 0 | 未指定 |
| `BROADCAST_KIND_DANMAKU` | 1 | 直播弹幕（由 danmaku 或客户端经 gateway/app 转发） |
| `BROADCAST_KIND_ROOM_STATE` | 2 | 房间状态：开播、断流、下播 |
| `BROADCAST_KIND_SYSTEM` | 3 | 系统通知：公告、房间整改提示 |
| `BROADCAST_KIND_INTERACTION` | 4 | 社区互动提示：点赞/收藏/分享计数（非商业化） |
| `BROADCAST_KIND_MODERATION` | 5 | 审核/风控处置：禁言、封停、踢下线 |
| `BROADCAST_KIND_ANCHOR_TIP` | 6 | 主播提词（文本，不含任何付费提醒） |

### enum `DropReason`

> 下发/丢弃原因。丢弃必须可解释：不得用"成功"掩盖越权或配置缺失。

| 值 | 编号 | 说明 |
|---|---|---|
| `DROP_REASON_UNSPECIFIED` | 0 | 未指定 |
| `DROP_REASON_OK` | 1 | 未丢弃（正常下发） |
| `DROP_REASON_NO_ROUTE` | 2 | 房间无在线节点路由 |
| `DROP_REASON_NO_SUBSCRIBER` | 3 | 房间无订阅者（消息可丢弃，不报错） |
| `DROP_REASON_PERMISSION_DENIED` | 4 | 越权：角色无权发该类消息 / 票据三元组不匹配 |
| `DROP_REASON_BAD_TICKET` | 5 | 票据无效：过期、已撤销、与 mid/room 不匹配 |
| `DROP_REASON_RATE_LIMITED` | 6 | 超过配额（房间或发送者维度） |
| `DROP_REASON_DUPLICATED` | 7 | 同一 message_id 重复投递（幂等丢弃） |
| `DROP_REASON_PAYLOAD_TOO_LARGE` | 8 | 载荷超过 Broadcast.MaxPayloadBytes |
| `DROP_REASON_ROOM_CLOSED` | 9 | 房间已关闭/不可广播（由 live-room 判定，见已知缺口） |
| `DROP_REASON_TRANSPORT_UNAVAILABLE` | 10 | 下发通道未接线（stub，见已知缺口） |

### enum `DeliveryResult`

> 单播投递结果（SendToUser）。

| 值 | 编号 | 说明 |
|---|---|---|
| `DELIVERY_RESULT_UNSPECIFIED` | 0 | 未指定 |
| `DELIVERY_RESULT_SENT` | 1 | 已投递到该用户的连接 |
| `DELIVERY_RESULT_NO_LEASE` | 2 | 该用户在该房间无有效租约 |
| `DELIVERY_RESULT_DENIED` | 3 | 发送者越权 |
| `DELIVERY_RESULT_TRANSPORT_UNAVAILABLE` | 4 | 下发通道未接线（stub） |

### enum `QuotaScope`

> 配额作用域（live_gw_access_quota.scope）。

| 值 | 编号 | 说明 |
|---|---|---|
| `QUOTA_SCOPE_UNSPECIFIED` | 0 | 未指定 |
| `QUOTA_SCOPE_GLOBAL` | 1 | 全局默认（scope_id=0） |
| `QUOTA_SCOPE_NODE` | 2 | 单个网关节点（scope_id=node_id 的哈希） |
| `QUOTA_SCOPE_ROOM` | 3 | 单个房间（scope_id=room_id，大房间提额） |
| `QUOTA_SCOPE_USER` | 4 | 单个用户（scope_id=mid，风控降配） |

### message `EmptyReply`

> --------------------------------------------------------------------------- / 公共结构 / --------------------------------------------------------------------------- / 空响应

（空消息）

### message `PageParam`

> 统一分页参数：pn 从 1 开始，ps 上限 50（服务端夹取，不报错）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pn` | `int32` | 1 | — | 页码（从 1 开始，<=0 视为 1） |
| `ps` | `int32` | 2 | — | 每页大小（最大 50，<=0 或超限取默认 20） |

### message `PageResult`

> 统一分页响应头部。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 符合条件的总行数 |

### message `ClientInfo`

> 客户端标识（脱敏：不存 IP 明文、不存设备指纹原值）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `platform` | [`Platform`](#enum-platform) | 1 | — | 客户端平台 |
| `app_version` | `string` | 2 | — | 客户端版本号 |
| `device_id_hash` | `string` | 3 | — | 设备标识摘要（sha256 hex 前 32 位），禁止明文 |
| `network_type` | `string` | 4 | — | wifi/4g/5g 等接入类型（仅排障，不参与权限判定） |

### message `LeaseInfo`

> 连接租约（主存储在 Redis；返回体即客户端重连凭据）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_id` | `string` | 1 | — | 租约 ID（ULID） |
| `conn_id` | `string` | 2 | — | 连接标识（客户端生成，(node,conn) 维度唯一） |
| `room_id` | `int64` | 3 | — | 房间 ID |
| `mid` | `int64` | 4 | — | 用户 ID（0 表示游客） |
| `role` | [`ConnRole`](#enum-connrole) | 5 | — | 角色（本服务判定结果，不是客户端自报值） |
| `node_id` | `string` | 6 | — | 承载该连接的网关节点标识 |
| `state` | [`LeaseState`](#enum-leasestate) | 7 | — | 状态 |
| `issued_at` | `int64` | 8 | — | 签发时间（Unix 秒） |
| `expire_at` | `int64` | 9 | — | 过期时间（Unix 秒） |
| `ttl_seconds` | `int32` | 10 | — | 当前剩余 TTL（秒） |
| `renew_count` | `int64` | 11 | — | 续租次数（断线重连排障用） |
| `last_heartbeat_at` | `int64` | 12 | — | 最近一次心跳（Unix 秒） |
| `reconnect_ticket` | `string` | 13 | — | 随租约下发的重连票据（一次性，见 IssueReconnectTicket） |
| `trace_id` | `string` | 14 | — | — |

### message `AcquireConnectionLeaseReq`

> --------------------------------------------------------------------------- / 连接租约与心跳 / ---------------------------------------------------------------------------

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 必填 |
| `mid` | `int64` | 2 | — | 登录用户；0 表示游客（是否允许由配额配置决定） |
| `claimed_role` | [`ConnRole`](#enum-connrole) | 3 | — | 调用方声明的角色；本服务对照房间归属校验后可能降级为 VIEWER |
| `conn_id` | `string` | 4 | — | 客户端连接标识（与 node_id 组成幂等键） |
| `node_id` | `string` | 5 | — | 承载连接的网关节点标识（由 WS 接入层传入） |
| `client` | [`ClientInfo`](#message-clientinfo) | 6 | — | 客户端信息（脱敏） |
| `ttl_seconds` | `int32` | 7 | — | 期望租约 TTL（0 用服务端默认；上限受配额配置夹取） |
| `request_id` | `string` | 8 | — | 幂等键：同 request_id 重放返回同一租约 |
| `trace_id` | `string` | 9 | — | — |

### message `AcquireConnectionLeaseReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease` | [`LeaseInfo`](#message-leaseinfo) | 1 | — | 通过的租约 |
| `allowed` | `bool` | 2 | — | 是否允许接入 |
| `deny_reason` | [`DropReason`](#enum-dropreason) | 3 | — | 拒绝原因（allowed=false 必填，不返回空成功） |
| `deny_detail` | `string` | 4 | — | 拒绝说明（脱敏） |
| `quota_room_conns` | `int32` | 5 | — | 该房间当前连接数（Redis 计数，用于客户端提示） |

### message `RenewConnectionLeaseReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_id` | `string` | 1 | — | 必填 |
| `conn_id` | `string` | 2 | — | — |
| `room_id` | `int64` | 3 | — | 必须与租约一致，否则视为越权 |
| `mid` | `int64` | 4 | — | 必须与租约一致 |
| `ttl_seconds` | `int32` | 5 | — | 0 表示用默认 |
| `trace_id` | `string` | 6 | — | — |

### message `RenewConnectionLeaseReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `renewed` | `bool` | 1 | — | 是否续租成功 |
| `state` | [`LeaseState`](#enum-leasestate) | 2 | — | 当前状态 |
| `expire_at` | `int64` | 3 | — | 新过期时间（Unix 秒） |
| `ttl_seconds` | `int32` | 4 | — | 新 TTL |
| `deny_reason` | [`DropReason`](#enum-dropreason) | 5 | — | 失败原因（过期/被踢/三元组不匹配） |

### message `ReleaseConnectionLeaseReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_id` | `string` | 1 | — | — |
| `conn_id` | `string` | 2 | — | — |
| `room_id` | `int64` | 3 | — | — |
| `mid` | `int64` | 4 | — | — |
| `reason` | `string` | 5 | — | normal/shutdown/switch_room（仅审计，不影响释放） |
| `trace_id` | `string` | 6 | — | — |

### message `ConnectionLeaseReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_id` | `string` | 1 | — | lease_id 与 (room_id, mid) 二选一：后者用于"该用户在该房间是否有连接" |
| `conn_id` | `string` | 2 | — | — |
| `room_id` | `int64` | 3 | — | — |
| `mid` | `int64` | 4 | — | — |

### message `ConnectionLeaseReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `found` | `bool` | 1 | — | 是否存在 |
| `lease` | [`LeaseInfo`](#message-leaseinfo) | 2 | — | 命中时返回 |
| `connection_count` | `int32` | 3 | — | 该 (room,mid) 的连接数（多端同时在线场景） |

### message `ReportClientHeartbeatReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_id` | `string` | 1 | — | — |
| `conn_id` | `string` | 2 | — | — |
| `room_id` | `int64` | 3 | — | — |
| `mid` | `int64` | 4 | — | — |
| `client_time` | `int64` | 5 | — | 客户端发送心跳时刻（Unix 秒，用于时钟漂移观测） |
| `seq` | `int32` | 6 | — | 心跳序号（乱序到达时服务端只推进最大 seq） |
| `rtt_ms` | `int32` | 7 | — | 客户端观测往返时延（毫秒，播放质量类指标） |
| `received_lag_ms` | `int32` | 8 | — | 消息接收滞后（毫秒，QoE 观测） |
| `trace_id` | `string` | 9 | — | — |

### message `ReportClientHeartbeatReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `accepted` | `bool` | 1 | — | 是否接受（票据校验失败为 false 并给原因） |
| `server_time` | `int64` | 2 | — | 服务端时间（Unix 秒，客户端校准用） |
| `ttl_seconds` | `int32` | 3 | — | 剩余 TTL（提示客户端及时续租） |
| `deny_reason` | [`DropReason`](#enum-dropreason) | 4 | — | — |

### message `ListRoomConnectionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 必填 |
| `mid` | `int64` | 2 | — | <=0 表示整个房间 |
| `role` | [`ConnRole`](#enum-connrole) | 3 | — | 按角色过滤（UNSPECIFIED 不过滤） |
| `page` | [`PageParam`](#message-pageparam) | 4 | — | — |

### message `ListRoomConnectionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | [`PageResult`](#message-pageresult) | 1 | — | — |
| `leases` | [`LeaseInfo`](#message-leaseinfo) | 2 | repeated | — |
| `snapshot_from_cache` | `bool` | 3 | — | true 表示 Redis 不可用时返回的是本地短缓存快照（可能偏旧） |

### message `IssueReconnectTicketReq`

> --------------------------------------------------------------------------- / 断线重连票据 / ---------------------------------------------------------------------------

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_id` | `string` | 1 | — | 以现有租约为凭据签发（无有效租约则拒绝） |
| `room_id` | `int64` | 2 | — | — |
| `mid` | `int64` | 3 | — | — |
| `ttl_seconds` | `int32` | 4 | — | 票据有效期（0 用服务端默认，上限受配额配置夹取） |
| `request_id` | `string` | 5 | — | 幂等键 |
| `trace_id` | `string` | 6 | — | — |

### message `ReconnectTicketInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ticket` | `string` | 1 | — | 票据串（HMAC 签名，不含明文身份信息） |
| `ticket_id` | `string` | 2 | — | 票据 ID（撤销与审计主键） |
| `room_id` | `int64` | 3 | — | — |
| `mid` | `int64` | 4 | — | — |
| `conn_id` | `string` | 5 | — | — |
| `node_id` | `string` | 6 | — | — |
| `state` | [`TicketState`](#enum-ticketstate) | 7 | — | — |
| `role` | [`ConnRole`](#enum-connrole) | 8 | — | 票据绑定的角色（换取租约时不再接受客户端声明） |
| `issued_at` | `int64` | 9 | — | Unix 秒 |
| `expire_at` | `int64` | 10 | — | Unix 秒 |
| `used_at` | `int64` | 11 | — | Unix 秒（0 表示未使用） |
| `issue_reason` | `string` | 12 | — | 签发场景：normal/reconnect/room_switch |
| `trace_id` | `string` | 13 | — | — |

### message `RedeemReconnectTicketReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ticket` | `string` | 1 | — | 必填 |
| `room_id` | `int64` | 2 | — | 必须与票据一致 |
| `mid` | `int64` | 3 | — | 必须与票据一致 |
| `conn_id` | `string` | 4 | — | 新连接标识 |
| `node_id` | `string` | 5 | — | 新承载节点 |
| `ttl_seconds` | `int32` | 6 | — | — |
| `request_id` | `string` | 7 | — | 幂等键：同 request_id 重放返回同一新租约 |
| `trace_id` | `string` | 8 | — | — |

### message `RedeemReconnectTicketReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `allowed` | `bool` | 1 | — | — |
| `lease` | [`LeaseInfo`](#message-leaseinfo) | 2 | — | 换取到的新租约 |
| `ticket_state` | [`TicketState`](#enum-ticketstate) | 3 | — | 票据最终状态（USED/EXPIRED/REVOKED） |
| `deny_reason` | [`DropReason`](#enum-dropreason) | 4 | — | 拒绝原因（三元组不匹配/过期/撤销） |
| `deny_detail` | `string` | 5 | — | — |
| `last_offline_at` | `int64` | 6 | — | 上一次断开时间（Unix 秒，客户端可提示断线时长） |
| `missed_message_estimate` | `int64` | 7 | — | 断线期间估算丢失的广播条数（可丢弃语义，仅提示） |

### message `RevokeReconnectTicketReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ticket_id` | `string` | 1 | — | ticket_id 与 (room_id, mid) 二选一（后者撤销该用户在该房间的全部票据） |
| `room_id` | `int64` | 2 | — | — |
| `mid` | `int64` | 3 | — | — |
| `reason` | `string` | 4 | — | 必填：banned/kicked/room_closed/risk（审计） |
| `operator` | `string` | 5 | — | 操作者标识 |
| `request_id` | `string` | 6 | — | 幂等键 |
| `trace_id` | `string` | 7 | — | — |

### message `RevokeReconnectTicketReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `revoked` | `bool` | 1 | — | — |
| `revoked_count` | `int32` | 2 | — | 撤销的票据数 |
| `state` | [`TicketState`](#enum-ticketstate) | 3 | — | — |

### message `JoinRoomReq`

> --------------------------------------------------------------------------- / 房间路由与订阅 / ---------------------------------------------------------------------------

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_id` | `string` | 1 | — | 必填：先有租约才能订阅（防止无凭据的订阅注入） |
| `conn_id` | `string` | 2 | — | — |
| `room_id` | `int64` | 3 | — | — |
| `mid` | `int64` | 4 | — | — |
| `node_id` | `string` | 5 | — | 由 WS 接入层传入，本服务据此登记/复用房间路由 |
| `topics` | `string` | 6 | repeated | 订阅子通道（danmaku/state/interaction），空表示默认全集 |
| `request_id` | `string` | 7 | — | 幂等键 |
| `trace_id` | `string` | 8 | — | — |

### message `JoinRoomReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `joined` | `bool` | 1 | — | — |
| `joined_at` | `int64` | 2 | — | Unix 秒 |
| `room_connection_count` | `int32` | 3 | — | 房间连接数（Redis） |
| `route_state` | [`RouteState`](#enum-routestate) | 4 | — | 房间路由状态 |
| `deny_reason` | [`DropReason`](#enum-dropreason) | 5 | — | 拒绝原因（租约无效/越权/配额满） |
| `subscribed_topics` | `string` | 6 | repeated | — |

### message `LeaveRoomReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_id` | `string` | 1 | — | — |
| `conn_id` | `string` | 2 | — | — |
| `room_id` | `int64` | 3 | — | — |
| `mid` | `int64` | 4 | — | — |
| `topics` | `string` | 5 | repeated | 空表示退订全部 |
| `reason` | `string` | 6 | — | — |
| `trace_id` | `string` | 7 | — | — |

### message `RoomRouteReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 必填 |

### message `RoomRouteInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `node_id` | `string` | 2 | — | 主承接节点（房间广播的第一跳） |
| `replica_nodes` | `string` | 3 | repeated | 副本节点（大房间分片广播） |
| `state` | [`RouteState`](#enum-routestate) | 4 | — | — |
| `shard_count` | `int32` | 5 | — | 广播分片数 |
| `serving_connections` | `int32` | 6 | — | 该路由当前承载连接数（Redis 读数） |
| `version` | `int64` | 7 | — | 乐观并发版本（路由切换需带 expected_version） |
| `updated_at` | `int64` | 8 | — | Unix 秒 |
| `ctime` | `int64` | 9 | — | — |

### message `ListRoomRoutesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `node_id` | `string` | 1 | — | 按节点过滤（空表示不过滤） |
| `state` | [`RouteState`](#enum-routestate) | 2 | — | — |
| `page` | [`PageParam`](#message-pageparam) | 3 | — | — |

### message `ListRoomRoutesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | [`PageResult`](#message-pageresult) | 1 | — | — |
| `routes` | [`RoomRouteInfo`](#message-roomrouteinfo) | 2 | repeated | — |

### message `DrainRoomRouteReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `node_id` | `string` | 2 | — | 要排空的节点 |
| `expected_version` | `int64` | 3 | — | 乐观并发版本 |
| `target_node_id` | `string` | 4 | — | 迁移目标节点（空表示只排空不指定） |
| `reason` | `string` | 5 | — | 必填：node_shutdown/deploy/scale（审计） |
| `request_id` | `string` | 6 | — | 幂等键 |
| `operator` | `string` | 7 | — | — |
| `trace_id` | `string` | 8 | — | — |

### message `BroadcastToRoomReq`

> --------------------------------------------------------------------------- / 广播与单播 / ---------------------------------------------------------------------------

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 必填 |
| `kind` | [`BroadcastKind`](#enum-broadcastkind) | 2 | — | 消息类别（决定权限矩阵与限流口径） |
| `message_id` | `string` | 3 | — | 幂等键：同 message_id 在同一房间只下发一次 |
| `sender_mid` | `int64` | 4 | — | 发送者（系统消息为 0） |
| `sender_role` | [`ConnRole`](#enum-connrole) | 5 | — | 发送者角色（由调用方声明，本服务按权限矩阵校验） |
| `sender_lease_id` | `string` | 6 | — | 用户态消息必须带租约或票据，服务端校验 (mid, room_id, 有效期) |
| `sender_ticket` | `string` | 7 | — | 无租约场景（如 HTTP 触发）用重连票据证明身份，二选一 |
| `payload` | `bytes` | 8 | — | 载荷（JSON/二进制，受 Broadcast.MaxPayloadBytes 限制） |
| `target_roles` | `string` | 9 | repeated | 只投递给指定角色，空表示全体 |
| `target_topics` | `string` | 10 | repeated | 子通道过滤，空表示全体 |
| `expire_at` | `int64` | 11 | — | 过期时刻（Unix 秒）：过期的消息直接丢弃（可丢弃语义） |
| `priority` | `int32` | 12 | — | 0 普通、1 高（审核处置/房间状态优先，配额限流时可豁免） |
| `require_reliable` | `bool` | 13 | — | true 时投递失败要返回错误（用于审核处置）；默认 false 可丢弃 |
| `trace_id` | `string` | 14 | — | — |

### message `BroadcastToRoomReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `accepted` | `bool` | 1 | — | 是否受理 |
| `message_id` | `string` | 2 | — | 回显幂等键 |
| `drop_reason` | [`DropReason`](#enum-dropreason) | 3 | — | 未受理/丢弃原因（DROP_REASON_OK 表示正常下发） |
| `fanout_nodes` | `int32` | 4 | — | 需要扇出的节点数 |
| `targeted_connections` | `int32` | 5 | — | 估算命中的连接数 |
| `enqueued_at` | `int64` | 6 | — | 受理时间（Unix 秒） |
| `duplicated` | `bool` | 7 | — | 命中 message_id 去重（重复投递） |
| `rate_remaining` | `int32` | 8 | — | 该维度剩余配额（限流可观测） |

### message `SendToUserReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `target_mid` | `int64` | 2 | — | 目标用户 |
| `kind` | [`BroadcastKind`](#enum-broadcastkind) | 3 | — | — |
| `message_id` | `string` | 4 | — | 幂等键（(room,target,message_id) 唯一） |
| `sender_mid` | `int64` | 5 | — | — |
| `sender_role` | [`ConnRole`](#enum-connrole) | 6 | — | — |
| `sender_lease_id` | `string` | 7 | — | — |
| `sender_ticket` | `string` | 8 | — | — |
| `payload` | `bytes` | 9 | — | — |
| `require_reliable` | `bool` | 10 | — | — |
| `trace_id` | `string` | 11 | — | — |

### message `SendToUserReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `result` | [`DeliveryResult`](#enum-deliveryresult) | 1 | — | — |
| `deny_reason` | [`DropReason`](#enum-dropreason) | 2 | — | result=DENIED 时给出具体原因 |
| `delivered_leases` | `int32` | 3 | — | 命中该用户的连接数（多端） |
| `message_id` | `string` | 4 | — | — |

### message `ForwardDanmakuReq`

> ForwardDanmaku：danmaku / gateway/app 把直播弹幕交给房间广播。 / 服务端不改变弹幕事实（落库仍由 danmaku 负责），只做权限校验、限流与扇出。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `danmaku_id` | `int64` | 2 | — | danmaku 主键（引用，不复制弹幕正文入库） |
| `sender_mid` | `int64` | 3 | — | — |
| `sender_lease_id` | `string` | 4 | — | 发送者凭据（必须校验三元组） |
| `sender_ticket` | `string` | 5 | — | — |
| `content_digest` | `string` | 6 | — | 正文摘要（sha256 hex 前 32）；正文经 payload 传输，不在本服务落库 |
| `payload` | `bytes` | 7 | — | 下发给观看端的弹幕包（含正文，短期存在） |
| `sent_at` | `int64` | 8 | — | 客户端发送时刻（Unix 秒） |
| `message_id` | `string` | 9 | — | 幂等键 |
| `trace_id` | `string` | 10 | — | — |

### message `ForwardDanmakuReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `accepted` | `bool` | 1 | — | — |
| `drop_reason` | [`DropReason`](#enum-dropreason) | 2 | — | — |
| `fanout_nodes` | `int32` | 3 | — | — |
| `targeted_connections` | `int32` | 4 | — | — |
| `rate_remaining` | `int32` | 5 | — | — |
| `message_id` | `string` | 6 | — | — |

### message `ForwardSystemEventReq`

> ForwardSystemEvent：开播/断流/下播/审核处置等系统事件的转发入口。 / 事件源是 live-room / live-ingest / moderation，它们只传主键与本服务定义的载荷。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `event_id` | `string` | 2 | — | 上游事件 ID（按 event_id 幂等，重复转发只下发一次） |
| `kind` | [`BroadcastKind`](#enum-broadcastkind) | 3 | — | ROOM_STATE / MODERATION / SYSTEM |
| `event_type` | `string` | 4 | — | 语义串：room.open/room.disconnect/room.close/moderation.mute |
| `anchor_mid` | `int64` | 5 | — | 房间归属主播（本服务据此校验 ANCHOR 角色） |
| `payload` | `bytes` | 6 | — | 事件载荷 |
| `require_reliable` | `bool` | 7 | — | 审核处置类事件必须可靠下发（false 时允许丢弃） |
| `source_service` | `string` | 8 | — | 来源服务名（审计） |
| `trace_id` | `string` | 9 | — | — |

### message `ForwardSystemEventReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `accepted` | `bool` | 1 | — | — |
| `event_id` | `string` | 2 | — | — |
| `drop_reason` | [`DropReason`](#enum-dropreason) | 3 | — | — |
| `fanout_nodes` | `int32` | 4 | — | — |
| `targeted_connections` | `int32` | 5 | — | — |
| `duplicated` | `bool` | 6 | — | — |

### message `KickConnectionReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `mid` | `int64` | 2 | — | mid 与 lease_id 至少一个 |
| `lease_id` | `string` | 3 | — | — |
| `conn_id` | `string` | 4 | — | 只踢单条连接（空表示该用户在该房间全部连接） |
| `reason` | `string` | 5 | — | 必填：banned/risk/room_closed/anchor_block |
| `ban_seconds` | `int32` | 6 | — | >0 时在 Redis 写禁止重连窗口（0 表示只断开） |
| `revoke_tickets` | `bool` | 7 | — | 是否同时撤销该用户的重连票据 |
| `operator` | `string` | 8 | — | 操作者（审计） |
| `request_id` | `string` | 9 | — | 幂等键 |
| `trace_id` | `string` | 10 | — | — |

### message `KickConnectionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `kicked` | `bool` | 1 | — | — |
| `kicked_connections` | `int32` | 2 | — | — |
| `revoked_tickets` | `int32` | 3 | — | — |
| `ban_until` | `int64` | 4 | — | 禁止重连到该时刻（Unix 秒，0 表示不禁止） |
| `deny_reason` | [`DropReason`](#enum-dropreason) | 5 | — | 操作者无权限时给出原因 |

### message `BroadcastLogInfo`

> --------------------------------------------------------------------------- / 审计与配额配置 / ---------------------------------------------------------------------------

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `message_id` | `string` | 2 | — | 幂等键（房间维度唯一） |
| `room_id` | `int64` | 3 | — | — |
| `kind` | [`BroadcastKind`](#enum-broadcastkind) | 4 | — | — |
| `sender_mid` | `int64` | 5 | — | — |
| `sender_role` | [`ConnRole`](#enum-connrole) | 6 | — | — |
| `event_id` | `string` | 7 | — | 系统事件来源 ID（非事件投递为空） |
| `payload_digest` | `string` | 8 | — | 载荷摘要（sha256 hex 前 32），不存正文 |
| `payload_bytes` | `int32` | 9 | — | 载荷字节数 |
| `fanout_nodes` | `int32` | 10 | — | — |
| `targeted_connections` | `int32` | 11 | — | — |
| `state` | `int32` | 12 | — | 1 已下发、2 已丢弃、3 越权拒绝、4 重复丢弃 |
| `drop_reason` | [`DropReason`](#enum-dropreason) | 13 | — | — |
| `source_service` | `string` | 14 | — | — |
| `trace_id` | `string` | 15 | — | — |
| `ctime` | `int64` | 16 | — | Unix 秒 |

### message `ListBroadcastLogsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 必填（审计按房间查，避免全表扫） |
| `kind` | [`BroadcastKind`](#enum-broadcastkind) | 2 | — | — |
| `sender_mid` | `int64` | 3 | — | — |
| `only_dropped` | `bool` | 4 | — | 只看被丢弃/被拒绝的 |
| `page` | [`PageParam`](#message-pageparam) | 5 | — | — |

### message `ListBroadcastLogsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | [`PageResult`](#message-pageresult) | 1 | — | — |
| `logs` | [`BroadcastLogInfo`](#message-broadcastloginfo) | 2 | repeated | — |

### message `AccessQuotaInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scope` | [`QuotaScope`](#enum-quotascope) | 1 | — | — |
| `scope_id` | `int64` | 2 | — | GLOBAL=0，NODE=节点哈希，ROOM=room_id，USER=mid |
| `scope_key` | `string` | 3 | — | 可读标识（如 node_id），仅展示与排障 |
| `max_connections` | `int32` | 4 | — | 该作用域最大连接数（0 表示继承上一层） |
| `broadcast_qps` | `int32` | 5 | — | 广播 QPS 上限（0 表示继承） |
| `danmaku_qps` | `int32` | 6 | — | 弹幕转发 QPS 上限（0 表示继承） |
| `lease_ttl_seconds` | `int32` | 7 | — | 租约 TTL（0 表示默认） |
| `ticket_ttl_seconds` | `int32` | 8 | — | 重连票据 TTL（0 表示默认） |
| `max_payload_bytes` | `int32` | 9 | — | 单条广播载荷上限 |
| `allow_guest` | `bool` | 10 | — | 是否允许游客（mid=0）接入 |
| `version` | `int64` | 11 | — | 乐观并发版本 |
| `updated_by` | `string` | 12 | — | 最后修改者（运营账号，审计） |
| `ctime` | `int64` | 13 | — | — |
| `mtime` | `int64` | 14 | — | — |

### message `AccessQuotaReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scope` | [`QuotaScope`](#enum-quotascope) | 1 | — | 必填 |
| `scope_id` | `int64` | 2 | — | — |

### message `UpsertAccessQuotaReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `quota` | [`AccessQuotaInfo`](#message-accessquotainfo) | 1 | — | 必填（scope/scope_id 决定行） |
| `expected_version` | `int64` | 2 | — | 乐观并发版本（0 表示新建，行已存在则返回冲突） |
| `operator` | `string` | 3 | — | 必填：运营账号，写入审计 |
| `request_id` | `string` | 4 | — | 幂等键 |
| `trace_id` | `string` | 5 | — | — |
