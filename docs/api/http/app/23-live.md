# 终端面 · `/live`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| live-room 域聚合（services/live-room/rpc/liveroom.proto） | 免鉴权 | 7 |
| live-room 域聚合（services/live-room/rpc/liveroom.proto） | 免鉴权 | 8 |

合计 **15** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## live-room 域聚合（services/live-room/rpc/liveroom.proto）（免鉴权，7 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/live/room/info` | 观众面：按房间 ID 读取直播间 | `liveRoomInfo` | `liveroominfologic.go` |
| GET | `/live/room/by/up` | 观众面：按 UP 主读取其生效中的直播间 | `liveRoomByUp` | `liveroombyuplogic.go` |
| GET | `/live/rooms` | 观众面：房间分页浏览（发现页/主播主页） | `listLiveRooms` | `listliveroomslogic.go` |
| GET | `/live/areas` | 观众面：直播分区列表（客户端与运营共用读接口） | `listLiveAreas` | `listliveareaslogic.go` |
| GET | `/live/session` | 观众面：读单场直播（按场次 ID，或按房间取最近一场） | `getLiveSession` | `getlivesessionlogic.go` |
| GET | `/live/sessions` | 观众面：历史场次 cursor 分页 | `listLiveSessions` | `listlivesessionslogic.go` |
| GET | `/live/anchors` | 观众面：房间主播绑定列表 | `listLiveAnchors` | `listliveanchorslogic.go` |

### GET `/live/room/info` — 观众面：按房间 ID 读取直播间

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/liveroominfohandler.go`
- 业务实现：`gateway/app/internal/logic/liveroominfologic.go`

请求：`ParamLiveRoom`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `WithSetting` | `with_setting` | form | `bool` | 否 | — | — |
| `WithActiveSession` | `with_active_session` | form | `bool` | 否 | — | — |

响应：`LiveRoomResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/live/room/by/up` — 观众面：按 UP 主读取其生效中的直播间

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/liveroombyuphandler.go`
- 业务实现：`gateway/app/internal/logic/liveroombyuplogic.go`

请求：`ParamLiveRoomByUp`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerMid` | `owner_mid` | form | `int64` | 是 | — | — |
| `WithSetting` | `with_setting` | form | `bool` | 否 | — | — |
| `WithActiveSession` | `with_active_session` | form | `bool` | 否 | — | — |

响应：`LiveRoomResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/live/rooms` — 观众面：房间分页浏览（发现页/主播主页）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listliveroomshandler.go`
- 业务实现：`gateway/app/internal/logic/listliveroomslogic.go`

请求：`ParamLiveRooms`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerMid` | `owner_mid` | form | `int64` | 否 | — | — |
| `AreaId` | `area_id` | form | `int64` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Order` | `order` | form | `int32` | 否 | — | 1 直播中优先、2 创建时间倒序 |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | 0 由服务取默认并截断到上限 |

响应：`LiveRoomsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/live/areas` — 观众面：直播分区列表（客户端与运营共用读接口）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listliveareashandler.go`
- 业务实现：`gateway/app/internal/logic/listliveareaslogic.go`

请求：`ParamLiveAreas`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ParentAreaId` | `parent_area_id` | form | `int64` | 是 | default=-1 | -1 不过滤、0 只取一级分区 |
| `State` | `state` | form | `int32` | 是 | default=-1 | -1 不过滤、1 启用、0 停用 |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`LiveAreasResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAreasData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/live/session` — 观众面：读单场直播（按场次 ID，或按房间取最近一场）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getlivesessionhandler.go`
- 业务实现：`gateway/app/internal/logic/getlivesessionlogic.go`

请求：`ParamLiveSession`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | form | `int64` | 否 | — | — |
| `RoomId` | `room_id` | form | `int64` | 否 | — | session_id=0 时按房间取最近一场 |
| `Offset` | `offset` | form | `int32` | 否 | — | 从最近一场往前数 |

响应：`LiveSessionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveSessionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/live/sessions` — 观众面：历史场次 cursor 分页

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listlivesessionshandler.go`
- 业务实现：`gateway/app/internal/logic/listlivesessionslogic.go`

请求：`ParamLiveSessions`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`LiveSessionsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveSessionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/live/anchors` — 观众面：房间主播绑定列表

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listliveanchorshandler.go`
- 业务实现：`gateway/app/internal/logic/listliveanchorslogic.go`

请求：`ParamLiveAnchors`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Role` | `role` | form | `int32` | 否 | — | 1 房主、2 联合主播、3 房管 |
| `OnlyEnabled` | `only_enabled` | form | `bool` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`LiveAnchorsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAnchorsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## live-room 域聚合（services/live-room/rpc/liveroom.proto）（免鉴权，8 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/live/room/create` | 主播面：创建直播间（request_id 幂等，资料送审由服务发起） | `createLiveRoom` | `createliveroomlogic.go` |
| POST | `/live/room/info/update` | 主播面：修改标题/封面/分区（终态房间不可改，改动后重新送审） | `updateLiveRoomInfo` | `updateliveroominfologic.go` |
| POST | `/live/room/setting/update` | 主播面：更新直播配置（整段覆盖语义） | `updateLiveRoomSetting` | `updateliveroomsettinglogic.go` |
| POST | `/live/prepare` | 主播面：开播前置检查（资格与风控由 live-room 经 creator/risk-control RPC 判定） | `prepareLive` | `preparelivelogic.go` |
| POST | `/live/start` | 主播面：开播（READY→LIVING 并新建场次，只登记 stream_id 引用） | `startLive` | `startlivelogic.go` |
| POST | `/live/end` | 主播面：下播（LIVING→READY，场次置为 ENDED） | `endLive` | `endlivelogic.go` |
| POST | `/live/room/close` | 主播面：关闭房间（终态 FINISHED，强制终止进行中场次并保留审计） | `closeLiveRoom` | `closeliveroomlogic.go` |
| POST | `/live/anchor/mutate` | 主播面：绑定或解绑主播/房管（房主房间数上限由服务校验） | `mutateLiveAnchor` | `mutateliveanchorlogic.go` |

### POST `/live/room/create` — 主播面：创建直播间（request_id 幂等，资料送审由服务发起）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/createliveroomhandler.go`
- 业务实现：`gateway/app/internal/logic/createliveroomlogic.go`

请求：`ParamLiveRoomCreate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Title` | `title` | form | `string` | 是 | — | — |
| `Cover` | `cover` | form | `string` | 否 | — | — |
| `AreaId` | `area_id` | form | `int64` | 是 | — | — |
| `Platform` | `platform` | form | `int32` | 否 | — | 1 android、2 ios、3 harmony、4 desktop |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `DanmakuEnabled` | `danmaku_enabled` | form | `bool` | 否 | — | — |
| `ReplyEnabled` | `reply_enabled` | form | `bool` | 否 | — | — |
| `RecordEnabled` | `record_enabled` | form | `bool` | 否 | — | — |
| `LinkmicEnabled` | `linkmic_enabled` | form | `bool` | 否 | — | — |
| `LiveType` | `live_type` | form | `int32` | 否 | — | — |
| `MinClientVersionCode` | `min_client_version_code` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`LiveRoomCreateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomCreateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/live/room/info/update` — 主播面：修改标题/封面/分区（终态房间不可改，改动后重新送审）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updateliveroominfohandler.go`
- 业务实现：`gateway/app/internal/logic/updateliveroominfologic.go`

请求：`ParamLiveRoomInfoUpdate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `Title` | `title` | form | `string` | 否 | — | 空串表示不修改 |
| `Cover` | `cover` | form | `string` | 否 | — | — |
| `AreaId` | `area_id` | form | `int64` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`LiveRoomInfoUpdateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomInfoUpdateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/live/room/setting/update` — 主播面：更新直播配置（整段覆盖语义）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updateliveroomsettinghandler.go`
- 业务实现：`gateway/app/internal/logic/updateliveroomsettinglogic.go`

请求：`ParamLiveRoomSettingUpdate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `DanmakuEnabled` | `danmaku_enabled` | form | `bool` | 是 | — | — |
| `ReplyEnabled` | `reply_enabled` | form | `bool` | 是 | — | — |
| `RecordEnabled` | `record_enabled` | form | `bool` | 是 | — | — |
| `LinkmicEnabled` | `linkmic_enabled` | form | `bool` | 是 | — | — |
| `LiveType` | `live_type` | form | `int32` | 是 | — | — |
| `MinClientVersionCode` | `min_client_version_code` | form | `int32` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`LiveRoomSettingUpdateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomSettingUpdateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/live/prepare` — 主播面：开播前置检查（资格与风控由 live-room 经 creator/risk-control RPC 判定）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/preparelivehandler.go`
- 业务实现：`gateway/app/internal/logic/preparelivelogic.go`

请求：`ParamLivePrepare`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Platform` | `platform` | form | `int32` | 否 | — | — |
| `DeviceHash` | `device_hash` | form | `string` | 否 | — | — |
| `IpHash` | `ip_hash` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`LivePrepareResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LivePrepareData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/live/start` — 主播面：开播（READY→LIVING 并新建场次，只登记 stream_id 引用）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/startlivehandler.go`
- 业务实现：`gateway/app/internal/logic/startlivelogic.go`

请求：`ParamLiveStart`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `StreamId` | `stream_id` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`LiveStartResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStartData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/live/end` — 主播面：下播（LIVING→READY，场次置为 ENDED）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/endlivehandler.go`
- 业务实现：`gateway/app/internal/logic/endlivelogic.go`

请求：`ParamLiveEnd`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `SessionId` | `session_id` | form | `int64` | 否 | — | 0 表示该房间当前进行中场次 |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `EndReason` | `end_reason` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`LiveEndResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveEndData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/live/room/close` — 主播面：关闭房间（终态 FINISHED，强制终止进行中场次并保留审计）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/closeliveroomhandler.go`
- 业务实现：`gateway/app/internal/logic/closeliveroomlogic.go`

请求：`ParamLiveClose`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`LiveCloseResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveCloseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/live/anchor/mutate` — 主播面：绑定或解绑主播/房管（房主房间数上限由服务校验）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/mutateliveanchorhandler.go`
- 业务实现：`gateway/app/internal/logic/mutateliveanchorlogic.go`

请求：`ParamLiveAnchorMutate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `TargetMid` | `target_mid` | form | `int64` | 是 | — | — |
| `Action` | `action` | form | `int32` | 是 | — | 1 绑定或重新启用、2 解绑 |
| `Role` | `role` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`LiveAnchorMutateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAnchorMutateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamLiveRoom`

> 边界（AGENTS.md §5/§8）：直播间业务状态、主播绑定、场次、禁播与分区归 live-room； / 推流密钥/流健康度/接入节点归 live-ingest，录制产物归 live-media。 / 网关只透传主键与引用（room_id / session_id / stream_id / asset_id / aid）， / 不接收推流密钥、不校验流状态、不判定开播资格。 /  / 开播前置检查 PrepareLive 在 live-room 内部经 creator（主播资格）与 / risk-control（ACTION_LIVE_START）RPC 完成，逐项结论（checks/deny_code）原样透出； / 网关不代为判定，也不把 degraded 当通过。 /  / proto 21 个方法中进入终端入口的是下面 15 个；其余 6 个不属于终端面： /   - ReportStreamState：live-ingest 的 live.state.v1 事件入口 /   - ApplyRoomModerationResult：moderation.result.v1 消费者入口 /   - AttachReplay：live-media 回放引用回写 /   - BanRoom / LiftBan / ListRoomBans：运营处置与审计（gateway/admin） /   - UpsertArea：分区维护（运营面，ListAreas 客户端与运营共用） /  / 登录要求：观众面读取沿用本文件既有约定（公开读路由不挂中间件，与 /search、/catalog、 / /comment 列表一致；AppkeyVerify 是 /account/privacy 的白名单校验，不适用于公开读）。 / 主播面所有写操作都要求必填 mid / operator_mid，权限判定在 live-room 侧。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `WithSetting` | `with_setting` | form | `bool` | 否 | — | — |
| `WithActiveSession` | `with_active_session` | form | `bool` | 否 | — | — |

### `LiveRoomResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomByUp`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerMid` | `owner_mid` | form | `int64` | 是 | — | — |
| `WithSetting` | `with_setting` | form | `bool` | 否 | — | — |
| `WithActiveSession` | `with_active_session` | form | `bool` | 否 | — | — |

### `ParamLiveRooms`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerMid` | `owner_mid` | form | `int64` | 否 | — | — |
| `AreaId` | `area_id` | form | `int64` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Order` | `order` | form | `int32` | 否 | — | 1 直播中优先、2 创建时间倒序 |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | 0 由服务取默认并截断到上限 |

### `LiveRoomsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveAreas`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ParentAreaId` | `parent_area_id` | form | `int64` | 是 | default=-1 | -1 不过滤、0 只取一级分区 |
| `State` | `state` | form | `int32` | 是 | default=-1 | -1 不过滤、1 启用、0 停用 |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `LiveAreasResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAreasData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveSession`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | form | `int64` | 否 | — | — |
| `RoomId` | `room_id` | form | `int64` | 否 | — | session_id=0 时按房间取最近一场 |
| `Offset` | `offset` | form | `int32` | 否 | — | 从最近一场往前数 |

### `LiveSessionResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveSessionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveSessions`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `LiveSessionsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveSessionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveAnchors`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Role` | `role` | form | `int32` | 否 | — | 1 房主、2 联合主播、3 房管 |
| `OnlyEnabled` | `only_enabled` | form | `bool` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `LiveAnchorsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAnchorsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomCreate`

> ParamLiveRoomCreate 创建直播间。platform 必填语义见 AGENTS.md §6（不写死单一端）； / request_id 是幂等键，客户端重试必须复用同一个值，网关不生成也不改写。 / setting 各字段为 false/0 时服务按「未显式指定」处理，缺省策略归服务配置。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Title` | `title` | form | `string` | 是 | — | — |
| `Cover` | `cover` | form | `string` | 否 | — | — |
| `AreaId` | `area_id` | form | `int64` | 是 | — | — |
| `Platform` | `platform` | form | `int32` | 否 | — | 1 android、2 ios、3 harmony、4 desktop |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `DanmakuEnabled` | `danmaku_enabled` | form | `bool` | 否 | — | — |
| `ReplyEnabled` | `reply_enabled` | form | `bool` | 否 | — | — |
| `RecordEnabled` | `record_enabled` | form | `bool` | 否 | — | — |
| `LinkmicEnabled` | `linkmic_enabled` | form | `bool` | 否 | — | — |
| `LiveType` | `live_type` | form | `int32` | 否 | — | — |
| `MinClientVersionCode` | `min_client_version_code` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `LiveRoomCreateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomCreateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomInfoUpdate`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `Title` | `title` | form | `string` | 否 | — | 空串表示不修改 |
| `Cover` | `cover` | form | `string` | 否 | — | — |
| `AreaId` | `area_id` | form | `int64` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `LiveRoomInfoUpdateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomInfoUpdateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomSettingUpdate`

> ParamLiveRoomSettingUpdate 整段覆盖：proto 明确「字段为 0/false 视为显式关闭」， / 因此这里不用三态，客户端必须先读后写全量提交。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `DanmakuEnabled` | `danmaku_enabled` | form | `bool` | 是 | — | — |
| `ReplyEnabled` | `reply_enabled` | form | `bool` | 是 | — | — |
| `RecordEnabled` | `record_enabled` | form | `bool` | 是 | — | — |
| `LinkmicEnabled` | `linkmic_enabled` | form | `bool` | 是 | — | — |
| `LiveType` | `live_type` | form | `int32` | 是 | — | — |
| `MinClientVersionCode` | `min_client_version_code` | form | `int32` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `LiveRoomSettingUpdateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomSettingUpdateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLivePrepare`

> ParamLivePrepare device_hash / ip_hash 由客户端预哈希后传入（风控入参禁止明文设备号与 IP）。 / 网关绝不用 r.RemoteAddr 合成 ip_hash——那会把明文 IP 交给下游并伪造风控依据。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Platform` | `platform` | form | `int32` | 否 | — | — |
| `DeviceHash` | `device_hash` | form | `string` | 否 | — | — |
| `IpHash` | `ip_hash` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `LivePrepareResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LivePrepareData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStart`

> ParamLiveStart stream_id 是 live-ingest 分配的推流标识引用（可空，推流到达后由事件回填）； / 本入口不接收推流密钥、不返回播放或推流地址。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `StreamId` | `stream_id` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `LiveStartResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStartData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveEnd`

> ParamLiveEnd end_reason 只允许 0（默认按 1 主播主动下播）或 1； / 禁播/关房/断流等终止原因由服务侧内部方法写入，客户端不得代填。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `SessionId` | `session_id` | form | `int64` | 否 | — | 0 表示该房间当前进行中场次 |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `EndReason` | `end_reason` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `LiveEndResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveEndData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveClose`

> ParamLiveClose 终端入口固定 admin=false（运营关闭走 gateway/admin）， / operator_mid 是否有权关闭由 live-room 判定，网关不预判。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `LiveCloseResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveCloseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveAnchorMutate`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `TargetMid` | `target_mid` | form | `int64` | 是 | — | — |
| `Action` | `action` | form | `int32` | 是 | — | 1 绑定或重新启用、2 解绑 |
| `Role` | `role` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `LiveAnchorMutateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAnchorMutateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `LiveRoomData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Room` | `room` | json | `LiveRoomInfo` | 是 | — | — |
| `Setting` | `setting` | json | `LiveRoomSetting` | 是 | — | — |
| `HasSetting` | `has_setting` | json | `bool` | 是 | — | false 表示未请求或服务侧无配置行 |
| `ActiveSession` | `active_session` | json | `LiveSessionInfo` | 是 | — | — |
| `HasActiveSession` | `has_active_session` | json | `bool` | 是 | — | — |

### `LiveRoomsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rooms` | `rooms` | json | `[]LiveRoomInfo` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Page` | `page` | json | `int32` | 是 | — | — |
| `PageSize` | `page_size` | json | `int32` | 是 | — | — |

### `LiveAreasData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Areas` | `areas` | json | `[]LiveArea` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `LiveSessionData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Session` | `session` | json | `LiveSessionInfo` | 是 | — | — |

### `LiveSessionsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Sessions` | `sessions` | json | `[]LiveSessionInfo` | 是 | — | session_id 倒序 |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | 空表示到底 |

### `LiveAnchorsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Anchors` | `anchors` | json | `[]LiveAnchor` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `LiveRoomCreateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `VerifyState` | `verify_state` | json | `int32` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | true 表示命中 request_id，未产生新写入 |
| `ModerationTaskId` | `moderation_task_id` | json | `int64` | 是 | — | — |

### `LiveRoomInfoUpdateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Room` | `room` | json | `LiveRoomInfo` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |
| `ModerationTaskId` | `moderation_task_id` | json | `int64` | 是 | — | 触发重新送审时的任务 ID |

### `LiveRoomSettingUpdateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Setting` | `setting` | json | `LiveRoomSetting` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LivePrepareData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Checks` | `checks` | json | `[]LivePrepareCheckItem` | 是 | — | — |
| `Ready` | `ready` | json | `bool` | 是 | — | — |
| `DenyCode` | `deny_code` | json | `string` | 是 | — | — |
| `RetryAfterSeconds` | `retry_after_seconds` | json | `int64` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LiveStartData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `StartedAt` | `started_at` | json | `int64` | 是 | — | — |
| `StateVersion` | `state_version` | json | `int32` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LiveEndData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | json | `int64` | 是 | — | — |
| `SessionState` | `session_state` | json | `int32` | 是 | — | — |
| `RoomState` | `room_state` | json | `int32` | 是 | — | — |
| `DurationSeconds` | `duration_seconds` | json | `int64` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LiveCloseData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 是 | — | — |
| `TerminatedSessionId` | `terminated_session_id` | json | `int64` | 是 | — | 被强制终止的进行中场次，0 表示无 |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LiveAnchorMutateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `TargetMid` | `target_mid` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 生效、0 已解绑 |
| `BoundCount` | `bound_count` | json | `int32` | 是 | — | 该主播当前生效房主房间数（上限校验结果） |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LiveRoomInfo`

> LiveRoomInfo 房间业务状态投影。state：1 待完善、2 可开播、3 直播中、4 已关闭、 / 5 违规禁播、6 停用；verify_state：1 未提交、2 审核中、3 通过、4 驳回。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `OwnerMid` | `owner_mid` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Cover` | `cover` | json | `string` | 是 | — | 封面引用，不含签名地址 |
| `AreaId` | `area_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `VerifyState` | `verify_state` | json | `int32` | 是 | — | — |
| `ActiveSessionId` | `active_session_id` | json | `int64` | 是 | — | — |
| `ActiveStreamId` | `active_stream_id` | json | `string` | 是 | — | 推流标识引用，非推流地址 |
| `StateVersion` | `state_version` | json | `int32` | 是 | — | — |
| `RejectReason` | `reject_reason` | json | `string` | 是 | — | — |
| `BanUntil` | `ban_until` | json | `int64` | 是 | — | 0 表示无禁播或永久 |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveRoomSetting`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `DanmakuEnabled` | `danmaku_enabled` | json | `bool` | 是 | — | — |
| `ReplyEnabled` | `reply_enabled` | json | `bool` | 是 | — | — |
| `RecordEnabled` | `record_enabled` | json | `bool` | 是 | — | — |
| `LinkmicEnabled` | `linkmic_enabled` | json | `bool` | 是 | — | — |
| `LiveType` | `live_type` | json | `int32` | 是 | — | 1 视频、2 语音、3 屏幕分享 |
| `MinClientVersionCode` | `min_client_version_code` | json | `int32` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveSessionInfo`

> LiveSessionInfo 场次投影。last_stream_seq 与 moderation_task_id 是服务内部 / 乱序守卫/送审关联字段，不下发终端。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | json | `int64` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 待推流、2 直播中、3 正常结束、4 异常终止 |
| `TitleSnapshot` | `title_snapshot` | json | `string` | 是 | — | — |
| `AreaIdSnapshot` | `area_id_snapshot` | json | `int64` | 是 | — | — |
| `StreamId` | `stream_id` | json | `string` | 是 | — | — |
| `StartedAt` | `started_at` | json | `int64` | 是 | — | — |
| `EndedAt` | `ended_at` | json | `int64` | 是 | — | — |
| `DurationSeconds` | `duration_seconds` | json | `int64` | 是 | — | — |
| `EndReason` | `end_reason` | json | `int32` | 是 | — | — |
| `ReplayState` | `replay_state` | json | `int32` | 是 | — | 1 无、2 转码中、3 可回放、4 已下架 |
| `RecordAssetId` | `record_asset_id` | json | `int64` | 是 | — | — |
| `RecordAid` | `record_aid` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveArea`

> LiveArea 分区投影，裁剪运营侧 operator_mid/ctime/mtime。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AreaId` | `area_id` | json | `int64` | 是 | — | — |
| `AreaName` | `area_name` | json | `string` | 是 | — | — |
| `ParentAreaId` | `parent_area_id` | json | `int64` | 是 | — | — |
| `Sort` | `sort` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |

### `LiveAnchor`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Role` | `role` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 生效、0 已解绑 |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LivePrepareCheckItem`

> LivePrepareCheckItem 开播前置检查逐项结论；degraded=true 表示下游不可用导致未评估， / 按未通过处理，网关不得美化成通过。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `string` | 是 | — | anchor_qualification / risk_control / room_verified / not_banned / setting_ok |
| `Passed` | `passed` | json | `bool` | 是 | — | — |
| `Detail` | `detail` | json | `string` | 是 | — | — |
| `Degraded` | `degraded` | json | `bool` | 是 | — | — |


<!-- file: docs/api/http/app/23-live.md -->
