# 运营面 · `/admin/live`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| live-room 域运营路由（只读面） | 免鉴权 | 7 |
| live-room 域运营路由（受 AdminPermission 保护） | AdminPermission | 5 |
| live-ingest 域运营路由（只读面） | 免鉴权 | 9 |
| live-ingest 域运营路由（受 AdminPermission 保护） | AdminPermission | 4 |
| live-gateway 域运营路由（只读面） | 免鉴权 | 4 |
| live-gateway 域运营路由（受 AdminPermission 保护） | AdminPermission | 4 |
| live-media 域运营路由（只读面） | 免鉴权 | 11 |
| live-media 域运营路由（受 AdminPermission 保护） | AdminPermission | 12 |

合计 **56** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## live-room 域运营路由（只读面）（免鉴权，7 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/live/room` | 房间详情：按 room_id 或房主 mid，可附带配置与进行中场次 | `liveRoomGet` | `liveroomgetlogic.go` |
| POST | `/admin/live/room/list` | 房间分页检索（状态/分区/房主过滤） | `liveRoomList` | `liveroomlistlogic.go` |
| GET | `/admin/live/room/bans` | 禁播台账（含运营内部 reason 与解除留痕） | `liveRoomBans` | `liveroombanslogic.go` |
| GET | `/admin/live/session` | 单场直播：按 session_id，或按房间取最近第 offset+1 场 | `liveSessionGet` | `livesessiongetlogic.go` |
| POST | `/admin/live/session/list` | 场次 cursor 分页（session_id 倒序，next_cursor 空表示到底） | `liveSessionList` | `livesessionlistlogic.go` |
| GET | `/admin/live/area/list` | 分区字典（含停用项，终端面裁掉的运营字段在此可见） | `liveAreaList` | `livearealistlogic.go` |
| GET | `/admin/live/anchor/list` | 房间主播绑定分页（含已解绑历史行） | `liveAnchorList` | `liveanchorlistlogic.go` |

### GET `/admin/live/room` — 房间详情：按 room_id 或房主 mid，可附带配置与进行中场次

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/liveroomgethandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroomgetlogic.go`

请求：`ParamLiveRoomGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 否 | — | 与 owner_mid 二选一，room_id 优先 |
| `OwnerMid` | `owner_mid` | form | `int64` | 否 | — | 按房主查其生效中的房间 |
| `WithSetting` | `with_setting` | form | `bool` | 否 | — | — |
| `WithActiveSession` | `with_active_session` | form | `bool` | 否 | — | — |

响应：`LiveRoomDetailResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/room/list` — 房间分页检索（状态/分区/房主过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/liveroomlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroomlistlogic.go`

请求：`ParamLiveRoomList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerMid` | `owner_mid` | json | `int64` | 否 | — | — |
| `AreaId` | `area_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Order` | `order` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int32` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | 0 由 live-room 取默认并截断到上限 |

响应：`LiveRoomListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/room/bans` — 禁播台账（含运营内部 reason 与解除留痕）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/liveroombanshandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroombanslogic.go`

请求：`ParamLiveRoomBans`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 否 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | 0 不过滤、1 生效、2 已解除、3 已过期 |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |

响应：`LiveRoomBansResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomBansData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/session` — 单场直播：按 session_id，或按房间取最近第 offset+1 场

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livesessiongethandler.go`
- 业务实现：`gateway/admin/internal/logic/livesessiongetlogic.go`

请求：`ParamLiveSessionGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | form | `int64` | 否 | — | — |
| `RoomId` | `room_id` | form | `int64` | 否 | — | session_id=0 时按房间取最近一场 |
| `Offset` | `offset` | form | `int32` | 否 | — | 从最近一场往前数，0 表示最近一场 |

响应：`LiveSessionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveSessionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/session/list` — 场次 cursor 分页（session_id 倒序，next_cursor 空表示到底）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livesessionlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livesessionlistlogic.go`

请求：`ParamLiveSessionList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | live-room 必填 |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | 上一页 next_cursor |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

响应：`LiveSessionListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveSessionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/area/list` — 分区字典（含停用项，终端面裁掉的运营字段在此可见）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livearealisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livearealistlogic.go`

请求：`ParamLiveAreaList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ParentAreaId` | `parent_area_id` | form | `int64` | 是 | default=-1 | -1 不过滤、0 只取一级分区 |
| `State` | `state` | form | `int32` | 是 | default=-1 | -1 不过滤、1 启用、0 停用 |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`LiveAreaListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAreaListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/anchor/list` — 房间主播绑定分页（含已解绑历史行）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/liveanchorlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/liveanchorlistlogic.go`

请求：`ParamLiveAnchorList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Role` | `role` | form | `int32` | 否 | — | — |
| `OnlyEnabled` | `only_enabled` | form | `bool` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`LiveAnchorListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAnchorListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## live-room 域运营路由（受 AdminPermission 保护）（AdminPermission，5 条）

> 五条写入口都要求 operator_mid > 0 与 request_id 非空；CloseRoom 的 admin 位由网关固定为 true。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/live/room/close` | 运营下架/关闭房间（admin=true，强制终止进行中场次并留审计） | `live:room` / `close` | `liveRoomClose` | `liveroomcloselogic.go` |
| POST | `/admin/live/room/ban` | 禁播：进入 BANNED 并终止场次（临时禁播需 duration_seconds） | `live:ban` / `create` | `liveRoomBan` | `liveroombanlogic.go` |
| POST | `/admin/live/room/ban/lift` | 解除禁播：BANNED→READY，ban_id=0 表示解除当前生效记录 | `live:ban` / `lift` | `liveRoomBanLift` | `liveroombanliftlogic.go` |
| POST | `/admin/live/setting/update` | 改直播配置（整段覆盖；live-room 无运营主体位，只认生效房主，见类型注释） | `live:setting` / `update` | `liveRoomSettingUpdate` | `liveroomsettingupdatelogic.go` |
| POST | `/admin/live/area/upsert` | 新建/修改直播分区（area_id=0 新建；名称唯一与停用占用校验在服务侧） | `live:area` / `update` | `liveAreaUpsert` | `liveareaupsertlogic.go` |

### POST `/admin/live/room/close` — 运营下架/关闭房间（admin=true，强制终止进行中场次并留审计）

- 权限口径：AdminPermission · 权限点 `live:room` / `close`
- goctl 入口：`gateway/admin/internal/handler/liveroomclosehandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroomcloselogic.go`

请求：`ParamLiveRoomClose`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | 只进 live_room_state_log，不进终端可见字段 |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveRoomCloseResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomCloseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/room/ban` — 禁播：进入 BANNED 并终止场次（临时禁播需 duration_seconds）

- 权限口径：AdminPermission · 权限点 `live:ban` / `create`
- goctl 入口：`gateway/admin/internal/handler/liveroombanhandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroombanlogic.go`

请求：`ParamLiveRoomBan`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `BanType` | `ban_type` | json | `int32` | 是 | — | — |
| `DurationSeconds` | `duration_seconds` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveRoomBanResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomBanData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/room/ban/lift` — 解除禁播：BANNED→READY，ban_id=0 表示解除当前生效记录

- 权限口径：AdminPermission · 权限点 `live:ban` / `lift`
- goctl 入口：`gateway/admin/internal/handler/liveroombanlifthandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroombanliftlogic.go`

请求：`ParamLiveRoomBanLift`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `BanId` | `ban_id` | json | `int64` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveRoomBanLiftResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomBanLiftData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/setting/update` — 改直播配置（整段覆盖；live-room 无运营主体位，只认生效房主，见类型注释）

- 权限口径：AdminPermission · 权限点 `live:setting` / `update`
- goctl 入口：`gateway/admin/internal/handler/liveroomsettingupdatehandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroomsettingupdatelogic.go`

请求：`ParamLiveRoomSettingUpdate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Setting` | `setting` | json | `LiveRoomSettingInput` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveRoomSettingUpdateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomSettingUpdateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/area/upsert` — 新建/修改直播分区（area_id=0 新建；名称唯一与停用占用校验在服务侧）

- 权限口径：AdminPermission · 权限点 `live:area` / `update`
- goctl 入口：`gateway/admin/internal/handler/liveareaupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/liveareaupsertlogic.go`

请求：`ParamLiveAreaUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AreaId` | `area_id` | json | `int64` | 否 | — | — |
| `AreaName` | `area_name` | json | `string` | 是 | — | — |
| `ParentAreaId` | `parent_area_id` | json | `int64` | 否 | — | — |
| `Sort` | `sort` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 启用、0 停用 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |

响应：`LiveAreaUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAreaUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## live-ingest 域运营路由（只读面）（免鉴权，9 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/live/stream` | 单流状态：按 stream_id 或房间的当前非终态流 | `liveStreamGet` | `livestreamgetlogic.go` |
| POST | `/admin/live/stream/list` | 流列表巡检（开播巡检/断流扫描；admin 作用域由网关声明，见类型注释） | `liveStreamList` | `livestreamlistlogic.go` |
| GET | `/admin/live/stream/key` | 推流密钥元数据（只有末 4 位辨认串与 Vault 引用，永不含明文） | `liveStreamKeyGet` | `livestreamkeygetlogic.go` |
| GET | `/admin/live/stream/key/list` | 推流密钥台账（key_id 倒序，含轮转链与吊销原因） | `liveStreamKeyList` | `livestreamkeylistlogic.go` |
| GET | `/admin/live/stream/health` | 流健康：当前判定 + 窗口聚合 + 最近采样点 | `liveStreamHealth` | `livestreamhealthlogic.go` |
| GET | `/admin/live/node/list` | 接入节点列表（health_score 降序，含摘流/离线节点） | `liveIngestNodeList` | `liveingestnodelistlogic.go` |
| GET | `/admin/live/assignment/list` | 节点分配台账（容量对账与排障；按流或按节点查） | `liveNodeAssignmentList` | `livenodeassignmentlistlogic.go` |
| GET | `/admin/live/interruption/list` | 断流与重连记录（含每次中断的起止、重连尝试数与关联事件 ID） | `liveStreamInterruptionList` | `livestreaminterruptionlistlogic.go` |
| GET | `/admin/live/event/list` | 流状态事件（按 seq 游标对账；后台只读，不能代写事件） | `liveStreamEventList` | `livestreameventlistlogic.go` |

### GET `/admin/live/stream` — 单流状态：按 stream_id 或房间的当前非终态流

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livestreamgethandler.go`
- 业务实现：`gateway/admin/internal/logic/livestreamgetlogic.go`

请求：`ParamLiveStreamGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 否 | — | — |
| `RoomId` | `room_id` | form | `int64` | 否 | — | 按房间取当前非终态流；与 stream_id 二选一 |

响应：`LiveStreamStateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/stream/list` — 流列表巡检（开播巡检/断流扫描；admin 作用域由网关声明，见类型注释）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livestreamlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livestreamlistlogic.go`

请求：`ParamLiveStreamList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomIds` | `room_ids` | json | `[]int64` | 否 | — | — |
| `NodeId` | `node_id` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Protocol` | `protocol` | json | `int32` | 否 | — | — |
| `HeartbeatBefore` | `heartbeat_before` | json | `int64` | 否 | — | Unix 秒 |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 上限由 live-ingest 夹取（MaxListPageSize） |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | 读取主体，必须 > 0 |

响应：`LiveStreamListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/stream/key` — 推流密钥元数据（只有末 4 位辨认串与 Vault 引用，永不含明文）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livestreamkeygethandler.go`
- 业务实现：`gateway/admin/internal/logic/livestreamkeygetlogic.go`

请求：`ParamLiveStreamKeyGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `KeyId` | `key_id` | form | `int64` | 否 | — | — |
| `StreamName` | `stream_name` | form | `string` | 否 | — | 接入排障：按流标识查当前生效密钥 |

响应：`LiveStreamKeyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamKeyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/stream/key/list` — 推流密钥台账（key_id 倒序，含轮转链与吊销原因）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livestreamkeylisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livestreamkeylistlogic.go`

请求：`ParamLiveStreamKeyList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 否 | — | — |
| `AnchorMid` | `anchor_mid` | form | `int64` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | 0 不限制 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | 读取主体，必须 > 0 |

响应：`LiveStreamKeyListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamKeyListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/stream/health` — 流健康：当前判定 + 窗口聚合 + 最近采样点

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livestreamhealthhandler.go`
- 业务实现：`gateway/admin/internal/logic/livestreamhealthlogic.go`

请求：`ParamLiveStreamHealth`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 是 | — | GetStreamHealthReq 只有这一个主体位 |
| `WindowSeconds` | `window_seconds` | form | `int32` | 否 | — | <=0 由服务取配置默认 |
| `SampleLimit` | `sample_limit` | form | `int32` | 否 | — | 上限由服务夹取（MaxSamplePoints） |

响应：`LiveStreamHealthResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamHealthData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/node/list` — 接入节点列表（health_score 降序，含摘流/离线节点）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/liveingestnodelisthandler.go`
- 业务实现：`gateway/admin/internal/logic/liveingestnodelistlogic.go`

请求：`ParamLiveIngestNodeList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Region` | `region` | form | `string` | 否 | — | — |
| `Protocol` | `protocol` | form | `int32` | 否 | — | 必须支持的协议，0 不限制 |
| `State` | `state` | form | `int32` | 否 | — | 0 不限制 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | 节点信息属运维面，proto 要求调用者 > 0 |

响应：`LiveIngestNodeListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveIngestNodeListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/assignment/list` — 节点分配台账（容量对账与排障；按流或按节点查）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livenodeassignmentlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livenodeassignmentlistlogic.go`

请求：`ParamLiveNodeAssignmentList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 否 | — | — |
| `NodeId` | `node_id` | form | `string` | 否 | — | 与 stream_id 二选一 |
| `State` | `state` | form | `int32` | 否 | — | 0 不限制 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |

响应：`LiveNodeAssignmentListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveNodeAssignmentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/interruption/list` — 断流与重连记录（含每次中断的起止、重连尝试数与关联事件 ID）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livestreaminterruptionlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livestreaminterruptionlistlogic.go`

请求：`ParamLiveStreamInterruptionList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 否 | — | — |
| `RoomId` | `room_id` | form | `int64` | 否 | — | 与 stream_id 二选一 |
| `OnlyOpen` | `only_open` | form | `bool` | 否 | — | — |
| `StartTime` | `start_time` | form | `int64` | 否 | — | started_at 下界（Unix 秒） |
| `EndTime` | `end_time` | form | `int64` | 否 | — | started_at 上界（Unix 秒） |
| `Limit` | `limit` | form | `int32` | 否 | — | 上限由服务夹取（MaxListPageSize） |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`LiveStreamInterruptionListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamInterruptionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/event/list` — 流状态事件（按 seq 游标对账；后台只读，不能代写事件）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livestreameventlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livestreameventlistlogic.go`

请求：`ParamLiveStreamEventList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 是 | — | — |
| `AfterSeq` | `after_seq` | form | `int64` | 否 | — | 0 表示从头 |
| `Limit` | `limit` | form | `int32` | 否 | — | 上限由服务夹取（MaxEventPageSize） |
| `Desc` | `desc` | form | `bool` | 否 | — | — |

响应：`LiveStreamEventListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamEventListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## live-ingest 域运营路由（受 AdminPermission 保护）（AdminPermission，4 条）

> 四条写入口都要求会话身份 + operator_mid > 0 + request_id 非空；
> CloseStream / RevokeStreamKey 的 admin 位由网关固定为 true。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/live/stream/close` | 强制断流（处置动作：IDLE/PUBLISHING/INTERRUPTED → STOPPED，级联释放配额） | `live:stream` / `close` | `liveStreamClose` | `livestreamcloselogic.go` |
| POST | `/admin/live/stream/key/revoke` | 吊销推流密钥（不可逆终态，可按需级联停止进行中的流） | `live:key` / `revoke` | `liveStreamKeyRevoke` | `livestreamkeyrevokelogic.go` |
| POST | `/admin/live/node/upsert` | 节点注册/元数据修改（派生字段由服务维护，后台只声明节点属性） | `live:node` / `update` | `liveIngestNodeUpsert` | `liveingestnodeupsertlogic.go` |
| POST | `/admin/live/event/retry` | 重试失败的流状态事件（outbox 运营补偿；request_id 幂等） | `live:event` / `retry` | `liveFailedEventRetry` | `livefailedeventretrylogic.go` |

### POST `/admin/live/stream/close` — 强制断流（处置动作：IDLE/PUBLISHING/INTERRUPTED → STOPPED，级联释放配额）

- 权限口径：AdminPermission · 权限点 `live:stream` / `close`
- goctl 入口：`gateway/admin/internal/handler/livestreamclosehandler.go`
- 业务实现：`gateway/admin/internal/logic/livestreamcloselogic.go`

请求：`ParamLiveStreamClose`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | json | `string` | 是 | — | — |
| `StopReason` | `stop_reason` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveStreamCloseResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamCloseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/stream/key/revoke` — 吊销推流密钥（不可逆终态，可按需级联停止进行中的流）

- 权限口径：AdminPermission · 权限点 `live:key` / `revoke`
- goctl 入口：`gateway/admin/internal/handler/livestreamkeyrevokehandler.go`
- 业务实现：`gateway/admin/internal/logic/livestreamkeyrevokelogic.go`

请求：`ParamLiveStreamKeyRevoke`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `KeyId` | `key_id` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `StopStream` | `stop_stream` | json | `bool` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveStreamKeyRevokeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamKeyRevokeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/node/upsert` — 节点注册/元数据修改（派生字段由服务维护，后台只声明节点属性）

- 权限口径：AdminPermission · 权限点 `live:node` / `update`
- goctl 入口：`gateway/admin/internal/handler/liveingestnodeupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/liveingestnodeupsertlogic.go`

请求：`ParamLiveIngestNodeUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Node` | `node` | json | `IngestNodeInput` | 是 | — | — |
| `CreateIfAbsent` | `create_if_absent` | json | `bool` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveIngestNodeUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveIngestNodeUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/event/retry` — 重试失败的流状态事件（outbox 运营补偿；request_id 幂等）

- 权限口径：AdminPermission · 权限点 `live:event` / `retry`
- goctl 入口：`gateway/admin/internal/handler/livefailedeventretryhandler.go`
- 业务实现：`gateway/admin/internal/logic/livefailedeventretrylogic.go`

请求：`ParamLiveFailedEventRetry`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventIds` | `event_ids` | json | `[]string` | 否 | — | — |
| `Limit` | `limit` | json | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveFailedEventRetryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveFailedEventRetryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## live-gateway 域运营路由（只读面）（免鉴权，4 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/live/connection/list` | 房间在线连接列表（Redis 视图；不回显重连票据） | `liveRoomConnectionList` | `liveroomconnectionlistlogic.go` |
| GET | `/admin/live/route/list` | 房间路由分页（按节点/状态过滤，含排空中与已下线） | `liveRoomRouteList` | `liveroomroutelistlogic.go` |
| GET | `/admin/live/broadcast/log` | 广播审计流水（按房间；只有载荷摘要，没有正文） | `liveBroadcastLogList` | `livebroadcastloglistlogic.go` |
| GET | `/admin/live/quota` | 某作用域生效的接入/广播配额（含继承链解析结果） | `liveAccessQuotaGet` | `liveaccessquotagetlogic.go` |

### GET `/admin/live/connection/list` — 房间在线连接列表（Redis 视图；不回显重连票据）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/liveroomconnectionlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroomconnectionlistlogic.go`

请求：`ParamLiveRoomConnectionList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | 必填：连接视图只在房间内有意义 |
| `Mid` | `mid` | form | `int64` | 否 | — | <=0 表示整个房间 |
| `Role` | `role` | form | `int32` | 否 | — | 0 不过滤（1 观众、2 主播、3 房管、4 运营、5 内部服务） |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | 上限由 live-gateway 夹取（PageParam.ps ≤ 50） |

响应：`LiveRoomConnectionListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomConnectionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/route/list` — 房间路由分页（按节点/状态过滤，含排空中与已下线）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/liveroomroutelisthandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroomroutelistlogic.go`

请求：`ParamLiveRoomRouteList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `NodeId` | `node_id` | form | `string` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | 0 不限制、1 承接、2 排空中、3 已下线 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |

响应：`LiveRoomRouteListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomRouteListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/broadcast/log` — 广播审计流水（按房间；只有载荷摘要，没有正文）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livebroadcastloglisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livebroadcastloglistlogic.go`

请求：`ParamLiveBroadcastLogList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | 必填：审计按房间查，避免全表扫 |
| `Kind` | `kind` | form | `int32` | 否 | — | — |
| `SenderMid` | `sender_mid` | form | `int64` | 否 | — | — |
| `OnlyDropped` | `only_dropped` | form | `bool` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |

响应：`LiveBroadcastLogListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveBroadcastLogListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/quota` — 某作用域生效的接入/广播配额（含继承链解析结果）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/liveaccessquotagethandler.go`
- 业务实现：`gateway/admin/internal/logic/liveaccessquotagetlogic.go`

请求：`ParamLiveAccessQuotaGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | form | `int32` | 是 | — | 1 全局、2 节点、3 房间、4 用户 |
| `ScopeId` | `scope_id` | form | `int64` | 否 | — | — |

响应：`LiveAccessQuotaResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAccessQuotaData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## live-gateway 域运营路由（受 AdminPermission 保护）（AdminPermission，4 条）

> 四条写入口都要求会话身份 + request_id（广播是 message_id）非空；
> operator 由会话生成（admin:<admin_id>），sender_role/sender_mid 由网关固定，不接受表单声明。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/live/connection/kick` | 强制下线（可撤销重连票据并写禁止重连窗口） | `live:connection` / `kick` | `liveConnectionKick` | `liveconnectionkicklogic.go` |
| POST | `/admin/live/route/drain` | 排空房间路由（expected_version 乐观校验，节点优雅下线） | `live:route` / `drain` | `liveRoomRouteDrain` | `liveroomroutedrainlogic.go` |
| POST | `/admin/live/broadcast/send` | 房间内公告/系统事件下发（运营身份由网关声明，可丢弃但原因必须可解释） | `live:broadcast` / `send` | `liveBroadcastSend` | `livebroadcastsendlogic.go` |
| POST | `/admin/live/quota/upsert` | 新建/更新接入与广播配额（修改者取会话身份，版本 CAS） | `live:quota` / `update` | `liveAccessQuotaUpsert` | `liveaccessquotaupsertlogic.go` |

### POST `/admin/live/connection/kick` — 强制下线（可撤销重连票据并写禁止重连窗口）

- 权限口径：AdminPermission · 权限点 `live:connection` / `kick`
- goctl 入口：`gateway/admin/internal/handler/liveconnectionkickhandler.go`
- 业务实现：`gateway/admin/internal/logic/liveconnectionkicklogic.go`

请求：`ParamLiveConnectionKick`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `LeaseId` | `lease_id` | json | `string` | 否 | — | — |
| `ConnId` | `conn_id` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `BanSeconds` | `ban_seconds` | json | `int32` | 否 | — | — |
| `RevokeTickets` | `revoke_tickets` | json | `bool` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveConnectionKickResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveConnectionKickData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/route/drain` — 排空房间路由（expected_version 乐观校验，节点优雅下线）

- 权限口径：AdminPermission · 权限点 `live:route` / `drain`
- goctl 入口：`gateway/admin/internal/handler/liveroomroutedrainhandler.go`
- 业务实现：`gateway/admin/internal/logic/liveroomroutedrainlogic.go`

请求：`ParamLiveRoomRouteDrain`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `NodeId` | `node_id` | json | `string` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 是 | — | — |
| `TargetNodeId` | `target_node_id` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveRoomRouteDrainResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomRouteDrainData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/broadcast/send` — 房间内公告/系统事件下发（运营身份由网关声明，可丢弃但原因必须可解释）

- 权限口径：AdminPermission · 权限点 `live:broadcast` / `send`
- goctl 入口：`gateway/admin/internal/handler/livebroadcastsendhandler.go`
- 业务实现：`gateway/admin/internal/logic/livebroadcastsendlogic.go`

请求：`ParamLiveBroadcastSend`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Kind` | `kind` | json | `int32` | 是 | — | 3 系统通知、5 审核处置、6 主播提词等，见 BroadcastKind |
| `MessageId` | `message_id` | json | `string` | 是 | — | 幂等键：(room_id, message_id) 唯一，本路由的门槛键 |
| `Payload` | `payload` | json | `string` | 是 | — | JSON/文本载荷，按 bytes 原样交给服务 |
| `TargetRoles` | `target_roles` | json | `[]string` | 否 | — | 空表示全体 |
| `TargetTopics` | `target_topics` | json | `[]string` | 否 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 否 | — | 0 表示不失效 |
| `Priority` | `priority` | json | `int32` | 否 | — | 0 普通、1 高 |
| `RequireReliable` | `require_reliable` | json | `bool` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveBroadcastSendResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveBroadcastSendData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/quota/upsert` — 新建/更新接入与广播配额（修改者取会话身份，版本 CAS）

- 权限口径：AdminPermission · 权限点 `live:quota` / `update`
- goctl 入口：`gateway/admin/internal/handler/liveaccessquotaupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/liveaccessquotaupsertlogic.go`

请求：`ParamLiveAccessQuotaUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Quota` | `quota` | json | `AccessQuotaInput` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveAccessQuotaUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAccessQuotaUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## live-media 域运营路由（只读面）（免鉴权，11 条）

> 与 audit / ops-config / cron / live-room / live-ingest / live-gateway / recommend 同一口径：
> 转码任务、分发档位、录制任务与切片、回放任务与引用、回收任务的读取走免中间件路由组。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/live/transcode` | 单个直播转码任务（state/attempt/heartbeat/version 全可见） | `liveMediaTranscodeGet` | `livemediatranscodegetlogic.go` |
| POST | `/admin/live/transcode/list` | 转码任务分页（房间/场次/状态/模板过滤） | `liveMediaTranscodeList` | `livemediatranscodelistlogic.go` |
| GET | `/admin/live/output/list` | 房间当前可分发档位（默认只在线，include_offline 带历史） | `liveMediaOutputList` | `livemediaoutputlistlogic.go` |
| GET | `/admin/live/record` | 单个录制任务（last_seq/gap_count 是断点续录与时间轴空洞的读数） | `liveMediaRecordGet` | `livemediarecordgetlogic.go` |
| POST | `/admin/live/record/list` | 录制任务分页（房间/场次/状态过滤） | `liveMediaRecordList` | `livemediarecordlistlogic.go` |
| POST | `/admin/live/record/segment/list` | 录制切片 keyset 分页（MISSING/CORRUPT 缺口必须看得见，否则回放像完整的） | `liveMediaRecordSegmentList` | `livemediarecordsegmentlistlogic.go` |
| GET | `/admin/live/replay` | 单个回放任务（asset_id/aid/bvid 只是引用，发布状态事实源在 video） | `liveMediaReplayGet` | `livemediareplaygetlogic.go` |
| POST | `/admin/live/replay/list` | 回放任务分页（房间/场次/状态过滤） | `liveMediaReplayList` | `livemediareplaylistlogic.go` |
| POST | `/admin/live/replay/asset/list` | 回放资产引用分页（含 video 侧审核/发布投影与回收标记） | `liveMediaReplayAssetList` | `livemediareplayassetlistlogic.go` |
| GET | `/admin/live/retention` | 单个回收任务（scanned/deleted/skipped 是先登记后执行的凭证） | `liveMediaRetentionGet` | `livemediaretentiongetlogic.go` |
| POST | `/admin/live/retention/list` | 回收任务分页（回收对象/状态/房间过滤） | `liveMediaRetentionList` | `livemediaretentionlistlogic.go` |

### GET `/admin/live/transcode` — 单个直播转码任务（state/attempt/heartbeat/version 全可见）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediatranscodegethandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediatranscodegetlogic.go`

请求：`ParamLiveMediaTranscodeGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | form | `int64` | 是 | — | — |

响应：`LiveMediaTranscodeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/transcode/list` — 转码任务分页（房间/场次/状态/模板过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediatranscodelisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediatranscodelistlogic.go`

请求：`ParamLiveMediaTranscodeList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `TemplateId` | `template_id` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`LiveMediaTranscodeListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/output/list` — 房间当前可分发档位（默认只在线，include_offline 带历史）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediaoutputlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediaoutputlistlogic.go`

请求：`ParamLiveMediaOutputList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | form | `int64` | 否 | — | — |
| `IncludeOffline` | `include_offline` | form | `bool` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 否 | — | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |

响应：`LiveMediaOutputListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaOutputListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/record` — 单个录制任务（last_seq/gap_count 是断点续录与时间轴空洞的读数）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediarecordgethandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediarecordgetlogic.go`

请求：`ParamLiveMediaRecordGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RecordId` | `record_id` | form | `int64` | 是 | — | — |

响应：`LiveMediaRecordResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/record/list` — 录制任务分页（房间/场次/状态过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediarecordlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediarecordlistlogic.go`

请求：`ParamLiveMediaRecordList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`LiveMediaRecordListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/record/segment/list` — 录制切片 keyset 分页（MISSING/CORRUPT 缺口必须看得见，否则回放像完整的）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediarecordsegmentlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediarecordsegmentlistlogic.go`

请求：`ParamLiveMediaRecordSegmentList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `AfterSeq` | `after_seq` | json | `int64` | 否 | — | — |
| `Limit` | `limit` | json | `int32` | 否 | — | — |

响应：`LiveMediaRecordSegmentListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordSegmentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/replay` — 单个回放任务（asset_id/aid/bvid 只是引用，发布状态事实源在 video）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediareplaygethandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediareplaygetlogic.go`

请求：`ParamLiveMediaReplayGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReplayId` | `replay_id` | form | `int64` | 是 | — | — |

响应：`LiveMediaReplayResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/replay/list` — 回放任务分页（房间/场次/状态过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediareplaylisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediareplaylistlogic.go`

请求：`ParamLiveMediaReplayList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`LiveMediaReplayListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/replay/asset/list` — 回放资产引用分页（含 video 侧审核/发布投影与回收标记）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediareplayassetlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediareplayassetlistlogic.go`

请求：`ParamLiveMediaReplayAssetList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `ReviewState` | `review_state` | json | `int32` | 否 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`LiveMediaReplayAssetListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayAssetListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/live/retention` — 单个回收任务（scanned/deleted/skipped 是先登记后执行的凭证）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediaretentiongethandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediaretentiongetlogic.go`

请求：`ParamLiveMediaRetentionGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RetentionId` | `retention_id` | form | `int64` | 是 | — | — |

响应：`LiveMediaRetentionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRetentionTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/retention/list` — 回收任务分页（回收对象/状态/房间过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/livemediaretentionlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediaretentionlistlogic.go`

请求：`ParamLiveMediaRetentionList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TargetKind` | `target_kind` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`LiveMediaRetentionListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRetentionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## live-media 域运营路由（受 AdminPermission 保护）（AdminPermission，12 条）

> 十二条写入口都要求会话身份 + request_id 非空（唯一例外 ApplyReplayContentState 用 event_id，
> 见上面的契约缺口）；有 operator 位的五条（转码 stop/retry/cancel、录制 stop、回收 submit）
> 的 operator 一律由会话渲染成 admin:<admin_id>，表单不得声明。
> 状态机合法性（当前状态能不能停/重试/取消、attempt 是否用尽、切片区间是否完整、
> 回收对象是否仍被引用）全部由 live-media 判定，网关只转达入参并投影结论（AGENTS.md §5/§8）。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/live/transcode/start` | 登记直播转码任务（PENDING，request_id 幂等；不在此拉起 FFmpeg） | `live:transcode` / `start` | `liveMediaTranscodeStart` | `livemediatranscodestartlogic.go` |
| POST | `/admin/live/transcode/stop` | 请求停止转码（RUNNING→STOPPING，Worker 收尾后 STOPPED） | `live:transcode` / `stop` | `liveMediaTranscodeStop` | `livemediatranscodestoplogic.go` |
| POST | `/admin/live/transcode/retry` | 重试失败的转码任务（FAILED→PENDING，attempt+1，受 max_attempts 限制） | `live:transcode` / `retry` | `liveMediaTranscodeRetry` | `livemediatranscoderetrylogic.go` |
| POST | `/admin/live/transcode/cancel` | 取消转码任务（PENDING\|STOPPING→CANCELLED 终态，只能重新登记） | `live:transcode` / `cancel` | `liveMediaTranscodeCancel` | `livemediatranscodecancellogic.go` |
| POST | `/admin/live/output/upsert` | 登记/刷新一个码率档位的分发输出（(room,session,level,protocol) 唯一） | `live:output` / `update` | `liveMediaOutputUpsert` | `livemediaoutputupsertlogic.go` |
| POST | `/admin/live/output/offline` | 下线一个档位（断流/到期/人工；只影响观众侧可用性，与回放发布状态无关） | `live:output` / `offline` | `liveMediaOutputOffline` | `livemediaoutputofflinelogic.go` |
| POST | `/admin/live/record/start` | 登记录制任务（PENDING，request_id 幂等） | `live:record` / `start` | `liveMediaRecordStart` | `livemediarecordstartlogic.go` |
| POST | `/admin/live/record/stop` | 停止录制（RECORDING→STOPPING，最后一片落库后 STOPPED 才能拼回放） | `live:record` / `stop` | `liveMediaRecordStop` | `livemediarecordstoplogic.go` |
| POST | `/admin/live/replay/submit` | 提交回放拼接任务（只登记与校验切片区间，不拼接、不发布） | `live:replay` / `submit` | `liveMediaReplaySubmit` | `livemediareplaysubmitlogic.go` |
| POST | `/admin/live/replay/asset/bind` | 回填回放产物与 asset/稿件的引用（只存引用，不推进稿件状态） | `live:replay` / `bind` | `liveMediaReplayAssetBind` | `livemediareplayassetbindlogic.go` |
| POST | `/admin/live/replay/content/state` | 手工刷新 video 侧审核/发布投影（source 固定 manual；方向单一，不反向推进稿件） | `live:replay` / `state` | `liveMediaReplayContentState` | `livemediareplaycontentstatelogic.go` |
| POST | `/admin/live/retention/submit` | 提交回收任务（超期切片/回放产物/残留档位，先登记后执行；purge=true 才真删） | `live:retention` / `submit` | `liveMediaRetentionSubmit` | `livemediaretentionsubmitlogic.go` |

### POST `/admin/live/transcode/start` — 登记直播转码任务（PENDING，request_id 幂等；不在此拉起 FFmpeg）

- 权限口径：AdminPermission · 权限点 `live:transcode` / `start`
- goctl 入口：`gateway/admin/internal/handler/livemediatranscodestarthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediatranscodestartlogic.go`

请求：`ParamLiveMediaTranscodeStart`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `TemplateId` | `template_id` | json | `int64` | 是 | — | — |
| `BitrateLevel` | `bitrate_level` | json | `int32` | 是 | — | — |
| `Protocol` | `protocol` | json | `int32` | 是 | — | — |
| `SourceRef` | `source_ref` | json | `string` | 是 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 否 | — | — |
| `MaxAttempts` | `max_attempts` | json | `int32` | 否 | — | — |
| `TimeoutSeconds` | `timeout_seconds` | json | `int32` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaTranscodeStartResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/transcode/stop` — 请求停止转码（RUNNING→STOPPING，Worker 收尾后 STOPPED）

- 权限口径：AdminPermission · 权限点 `live:transcode` / `stop`
- goctl 入口：`gateway/admin/internal/handler/livemediatranscodestophandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediatranscodestoplogic.go`

请求：`ParamLiveMediaTranscodeStop`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaTranscodeStopResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/transcode/retry` — 重试失败的转码任务（FAILED→PENDING，attempt+1，受 max_attempts 限制）

- 权限口径：AdminPermission · 权限点 `live:transcode` / `retry`
- goctl 入口：`gateway/admin/internal/handler/livemediatranscoderetryhandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediatranscoderetrylogic.go`

请求：`ParamLiveMediaTranscodeRetry`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaTranscodeRetryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/transcode/cancel` — 取消转码任务（PENDING|STOPPING→CANCELLED 终态，只能重新登记）

- 权限口径：AdminPermission · 权限点 `live:transcode` / `cancel`
- goctl 入口：`gateway/admin/internal/handler/livemediatranscodecancelhandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediatranscodecancellogic.go`

请求：`ParamLiveMediaTranscodeCancel`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaTranscodeCancelResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/output/upsert` — 登记/刷新一个码率档位的分发输出（(room,session,level,protocol) 唯一）

- 权限口径：AdminPermission · 权限点 `live:output` / `update`
- goctl 入口：`gateway/admin/internal/handler/livemediaoutputupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediaoutputupsertlogic.go`

请求：`ParamLiveMediaOutputUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `TaskId` | `task_id` | json | `int64` | 否 | — | 0 表示源流直出 |
| `BitrateLevel` | `bitrate_level` | json | `int32` | 是 | — | — |
| `Protocol` | `protocol` | json | `int32` | 是 | — | — |
| `Bucket` | `bucket` | json | `string` | 否 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 否 | — | — |
| `CdnDomain` | `cdn_domain` | json | `string` | 否 | — | — |
| `Width` | `width` | json | `int32` | 否 | — | — |
| `Height` | `height` | json | `int32` | 否 | — | — |
| `BitrateKbps` | `bitrate_kbps` | json | `int32` | 否 | — | — |
| `Fps` | `fps` | json | `int32` | 否 | — | — |
| `OnlineExpireAt` | `online_expire_at` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaOutputUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaStreamOutputInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/output/offline` — 下线一个档位（断流/到期/人工；只影响观众侧可用性，与回放发布状态无关）

- 权限口径：AdminPermission · 权限点 `live:output` / `offline`
- goctl 入口：`gateway/admin/internal/handler/livemediaoutputofflinehandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediaoutputofflinelogic.go`

请求：`ParamLiveMediaOutputOffline`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OutputId` | `output_id` | json | `int64` | 否 | — | — |
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `BitrateLevel` | `bitrate_level` | json | `int32` | 否 | — | — |
| `Protocol` | `protocol` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaOutputOfflineResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaStreamOutputInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/record/start` — 登记录制任务（PENDING，request_id 幂等）

- 权限口径：AdminPermission · 权限点 `live:record` / `start`
- goctl 入口：`gateway/admin/internal/handler/livemediarecordstarthandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediarecordstartlogic.go`

请求：`ParamLiveMediaRecordStart`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `SourceTaskId` | `source_task_id` | json | `int64` | 否 | — | 0 表示原画源 |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `SegmentSeconds` | `segment_seconds` | json | `int32` | 否 | — | — |
| `TimeoutSeconds` | `timeout_seconds` | json | `int32` | 否 | — | — |
| `OutputBucket` | `output_bucket` | json | `string` | 否 | — | — |
| `OutputPrefix` | `output_prefix` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaRecordStartResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/record/stop` — 停止录制（RECORDING→STOPPING，最后一片落库后 STOPPED 才能拼回放）

- 权限口径：AdminPermission · 权限点 `live:record` / `stop`
- goctl 入口：`gateway/admin/internal/handler/livemediarecordstophandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediarecordstoplogic.go`

请求：`ParamLiveMediaRecordStop`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaRecordStopResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/replay/submit` — 提交回放拼接任务（只登记与校验切片区间，不拼接、不发布）

- 权限口径：AdminPermission · 权限点 `live:replay` / `submit`
- goctl 入口：`gateway/admin/internal/handler/livemediareplaysubmithandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediareplaysubmitlogic.go`

请求：`ParamLiveMediaReplaySubmit`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `FromSeq` | `from_seq` | json | `int64` | 否 | — | — |
| `ToSeq` | `to_seq` | json | `int64` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `AllowGaps` | `allow_gaps` | json | `bool` | 否 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 否 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaReplaySubmitResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/replay/asset/bind` — 回填回放产物与 asset/稿件的引用（只存引用，不推进稿件状态）

- 权限口径：AdminPermission · 权限点 `live:replay` / `bind`
- goctl 入口：`gateway/admin/internal/handler/livemediareplayassetbindhandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediareplayassetbindlogic.go`

请求：`ParamLiveMediaReplayAssetBind`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReplayId` | `replay_id` | json | `int64` | 是 | — | — |
| `AssetId` | `asset_id` | json | `int64` | 否 | — | — |
| `Aid` | `aid` | json | `int64` | 否 | — | — |
| `Bvid` | `bvid` | json | `string` | 否 | — | — |
| `Bucket` | `bucket` | json | `string` | 否 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 否 | — | — |
| `DurationMs` | `duration_ms` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaReplayAssetBindResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayAssetRefInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/replay/content/state` — 手工刷新 video 侧审核/发布投影（source 固定 manual；方向单一，不反向推进稿件）

- 权限口径：AdminPermission · 权限点 `live:replay` / `state`
- goctl 入口：`gateway/admin/internal/handler/livemediareplaycontentstatehandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediareplaycontentstatelogic.go`

请求：`ParamLiveMediaReplayContentState`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReplayId` | `replay_id` | json | `int64` | 否 | — | — |
| `AssetId` | `asset_id` | json | `int64` | 否 | — | — |
| `ReviewState` | `review_state` | json | `int32` | 是 | — | — |
| `PublishedAt` | `published_at` | json | `int64` | 否 | — | — |
| `EventId` | `event_id` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaReplayContentStateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayAssetRefInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/live/retention/submit` — 提交回收任务（超期切片/回放产物/残留档位，先登记后执行；purge=true 才真删）

- 权限口径：AdminPermission · 权限点 `live:retention` / `submit`
- goctl 入口：`gateway/admin/internal/handler/livemediaretentionsubmithandler.go`
- 业务实现：`gateway/admin/internal/logic/livemediaretentionsubmitlogic.go`

请求：`ParamLiveMediaRetentionSubmit`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TargetKind` | `target_kind` | json | `int32` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 否 | — | 0 表示全局扫描（服务侧 <=0，网关拒负数） |
| `TargetId` | `target_id` | json | `int64` | 否 | — | 0 表示按 expire_before 批量 |
| `ExpireBefore` | `expire_before` | json | `int64` | 否 | — | — |
| `Purge` | `purge` | json | `bool` | 否 | — | — |
| `BatchLimit` | `batch_limit` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`LiveMediaRetentionSubmitResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRetentionTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamLiveRoomGet`

> 边界（AGENTS.md §5/§8）：直播间业务状态、主播绑定、场次、禁播、分区与直播配置归 live-room； / 推流密钥/流健康度/接入节点归 live-ingest，录制产物归 live-media，网关一律不碰。 /  / 只开放运营面 12 条：proto 21 个方法里 9 个刻意不进后台—— /   - CreateRoom / UpdateRoomInfo / PrepareLive / StartLive / EndLive / MutateAnchor： /     主播自有动作，这些契约没有「运营」主体位，live-room 一律按生效房主判定， /     后台代做只会得到 ErrAnchorNotOwner（setting/update 的缺口见下方类型注释）； /   - ReportStreamState / ApplyRoomModerationResult：live.state.v1 与 moderation.result.v1 /     的事件入口，按 event_id 去重，只能由事件链路调用； /   - AttachReplay：live-media 的回放引用回写。 /  / 归属判定： /   - 只读面（房间/场次/分区/主播/禁播台账）不挂 AdminPermission，与 audit、ops-config、cron /     读面同一口径：后台列表页每次刷新都会打一次 RPC，全量挂判定会把 operation 变成读放大瓶颈； /   - 写面 5 条全部挂 AdminPermission，并要求 operator_mid > 0 与 request_id 非空， /     在出网关之前就拒掉——live-room 同样校验这两条，网关先拦是为了不产生无主体的写请求。 /  / 已知契约缺口：liveroom.proto 的审计主体字段是 operator_mid（mid 语义），而 AdminPermission / 会话解析出的是 operation.op_admin_user 的 admin_id，两者不是同一编号空间，因此网关 / **不用会话 admin_id 覆盖** operator_mid（那会把处置记到一个无关用户的 mid 上），而是要求 / 后台表单显式声明执行运营的 mid，同时把会话 admin_id 与权限点留在网关日志里，保证 / 「谁在后台点的按钮」和「台账上记的谁」两条线都能追。补齐方式是在 live-room 契约里 / 增加运营主体位（本期不改 services/**，缺口另见 gateway/admin/README.md）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 否 | — | 与 owner_mid 二选一，room_id 优先 |
| `OwnerMid` | `owner_mid` | form | `int64` | 否 | — | 按房主查其生效中的房间 |
| `WithSetting` | `with_setting` | form | `bool` | 否 | — | — |
| `WithActiveSession` | `with_active_session` | form | `bool` | 否 | — | — |

### `LiveRoomDetailResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomList`

> ParamLiveRoomList state/order 传 0（ROOM_STATE_UNSPECIFIED / ROOM_ORDER_UNSPECIFIED）表示 / 不过滤/默认 room_id 倒序；取值合法性与页大小上限都由 live-room 判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerMid` | `owner_mid` | json | `int64` | 否 | — | — |
| `AreaId` | `area_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Order` | `order` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int32` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | 0 由 live-room 取默认并截断到上限 |

### `LiveRoomListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomBans`

> ParamLiveRoomBans 禁播台账查询。live-room 把「无主体的读取」直接拒（reason 属运营内部说明）， / 所以 operator_mid 在只读面同样是必填门槛。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 否 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | 0 不过滤、1 生效、2 已解除、3 已过期 |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |

### `LiveRoomBansResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomBansData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveSessionGet`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | form | `int64` | 否 | — | — |
| `RoomId` | `room_id` | form | `int64` | 否 | — | session_id=0 时按房间取最近一场 |
| `Offset` | `offset` | form | `int32` | 否 | — | 从最近一场往前数，0 表示最近一场 |

### `LiveSessionResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveSessionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveSessionList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | live-room 必填 |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | 上一页 next_cursor |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

### `LiveSessionListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveSessionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveAreaList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ParentAreaId` | `parent_area_id` | form | `int64` | 是 | default=-1 | -1 不过滤、0 只取一级分区 |
| `State` | `state` | form | `int32` | 是 | default=-1 | -1 不过滤、1 启用、0 停用 |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `LiveAreaListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAreaListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveAnchorList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `Role` | `role` | form | `int32` | 否 | — | — |
| `OnlyEnabled` | `only_enabled` | form | `bool` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `LiveAnchorListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAnchorListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomClose`

> ParamLiveRoomClose 运营下架/关闭房间。admin 位**不在 .api 暴露**：本组路由挂在 / AdminPermission 下，网关固定写 admin=true，让 live-room 记 SourceRPCAdmin 并跳过 / 「必须生效房主」的判定；若允许客户端声明，等于让后台表单能伪装主播主动关房。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | 只进 live_room_state_log，不进终端可见字段 |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveRoomCloseResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomCloseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomBan`

> ParamLiveRoomBan ban_type：1 临时（duration_seconds 必须 > 0）、2 永久（end_at=0，只能 LiftBan）； / 时长与到期时间的组合由 live-room 判定，网关不做秒数换算。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `BanType` | `ban_type` | json | `int32` | 是 | — | — |
| `DurationSeconds` | `duration_seconds` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveRoomBanResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomBanData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomBanLift`

> ParamLiveRoomBanLift ban_id 传 0 表示解除当前生效记录；指定 ban_id 时由 live-room 校验归属。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `BanId` | `ban_id` | json | `int64` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveRoomBanLiftResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomBanLiftData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomSettingUpdate`

> ParamLiveRoomSettingUpdate 整段覆盖语义要求 setting 必传：live-room 明确拒绝 nil setting， / 因为「未传」与「全部关闭」无法区分，一次漏传把房间功能全关比拒绝请求危险得多。 / 契约缺口：UpdateRoomSettingReq 没有运营主体位，live-room 只认生效房主 / （services/live-room/internal/logic/updateroomsettinglogic.go），所以本路由实际只在 / operator_mid 恰为该房间生效房主时成功，否则原样上抛 ErrAnchorNotOwner——网关不伪造房主身份。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Setting` | `setting` | json | `LiveRoomSettingInput` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveRoomSettingUpdateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomSettingUpdateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveAreaUpsert`

> ParamLiveAreaUpsert area_id=0 新建、>0 修改；名称唯一性、父级环检测、停用前占用校验 / 全在 live-room（含 area_name 长度上限取服务配置）。UpsertAreaReq 没有 trace_id 字段， / 因此本请求也不带——不为日志方便而发明下游无处安放的参数。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AreaId` | `area_id` | json | `int64` | 否 | — | — |
| `AreaName` | `area_name` | json | `string` | 是 | — | — |
| `ParentAreaId` | `parent_area_id` | json | `int64` | 否 | — | — |
| `Sort` | `sort` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 启用、0 停用 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |

### `LiveAreaUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAreaUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStreamGet`

> 边界（AGENTS.md §5/§8）：推流密钥元数据（只有哈希与 Vault 引用）、接入节点、流状态机、 / 断流/重连记录与 live.state.v1 事件归 live-ingest；房间业务状态归 live-room（上面那组）， / 录制/回放归 live-media，长连接下发归 live-gateway。网关不跨服务拼状态，也不代为推进状态机。 /  / proto 22 个方法只开放 13 条（9 读 + 4 写）。刻意不接的入口逐条记在 gateway/admin/README.md， / 关键三条： /   - IssueStreamKey / RotateStreamKey：reply 带 plaintext_key 与内嵌明文密钥的 publish_url /     （契约注明「仅此一次返回」）。控制台不该成为一次性机密的通道——响应体会进访问日志、 /     浏览器历史与工单截图。密钥泄露的运营处置面是 RevokeStreamKey，重新签发留在主播端。 /   - ReportStreamState / ReportStreamHealth：live.state.v1 的事实来源上报口（节点带 report_id /     推进 + 分配 seq），后台开面等于给流状态机开后门（AGENTS.md §8）。 /   - AssignIngestNode / ReleaseIngestNode：控制面分配/释放，成对调用才有意义（人工单边释放 /     会让节点配额与实际流对不上）；运营要看的是分配台账 /assignment/list。 /  / 归属判定：与 live-room 域同一口径——只读面不挂 AdminPermission（后台列表页每次刷新都会打一次 / RPC，全量挂判定会把 operation 变成读放大瓶颈），写面 4 条全部挂 AdminPermission， / 并要求 operator_mid > 0 与 request_id 非空，在出网关之前就拒掉。 /  / 已知缺口（另见 README）： /   - 主体字段同样是 operator_mid（用户 mid 空间），与会话解析出的 op_admin_user.admin_id /     不是同一编号空间，因此沿用 live-room 的处理：网关**不用 admin_id 覆盖** operator_mid， /     要求表单显式声明执行运营的 mid，并把 admin_id + operator_mid 一起写进网关日志。 /   - ListStreamsReq.admin / ListStreamKeysReq.admin 是**服务端断言，不是表单字段**： /     .api 里刻意不出现 admin，网关按「这条路由只存在于 /admin/live 运营面」写 true /     （false 会被 live-ingest 收敛成只看 operator_mid 自己的行，巡检页就失去意义）； /     CloseStream/RevokeStreamKey 的 admin 位与 live-room CloseRoom 同一先例固定 true， /     且这两条写在 AdminPermission 组里，没有会话身份根本到不了 logic。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 否 | — | — |
| `RoomId` | `room_id` | form | `int64` | 否 | — | 按房间取当前非终态流；与 stream_id 二选一 |

### `LiveStreamStateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStreamList`

> ParamLiveStreamList 开播巡检/断流扫描。state 传 0 按 live-ingest 的口径是「只看非终态」， / 不是「不过滤」——这条语义写在 proto 注释里，网关不改写取值，避免后台把「断流」读成「没有流」。 / heartbeat_before 用于「心跳早于某时刻」的滞后扫描，server_time 回读用来算滞后秒数。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomIds` | `room_ids` | json | `[]int64` | 否 | — | — |
| `NodeId` | `node_id` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Protocol` | `protocol` | json | `int32` | 否 | — | — |
| `HeartbeatBefore` | `heartbeat_before` | json | `int64` | 否 | — | Unix 秒 |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 上限由 live-ingest 夹取（MaxListPageSize） |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | 读取主体，必须 > 0 |

### `LiveStreamListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStreamKeyGet`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `KeyId` | `key_id` | form | `int64` | 否 | — | — |
| `StreamName` | `stream_name` | form | `string` | 否 | — | 接入排障：按流标识查当前生效密钥 |

### `LiveStreamKeyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamKeyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStreamKeyList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 否 | — | — |
| `AnchorMid` | `anchor_mid` | form | `int64` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | 0 不限制 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | 读取主体，必须 > 0 |

### `LiveStreamKeyListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamKeyListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStreamHealth`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 是 | — | GetStreamHealthReq 只有这一个主体位 |
| `WindowSeconds` | `window_seconds` | form | `int32` | 否 | — | <=0 由服务取配置默认 |
| `SampleLimit` | `sample_limit` | form | `int32` | 否 | — | 上限由服务夹取（MaxSamplePoints） |

### `LiveStreamHealthResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamHealthData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveIngestNodeList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Region` | `region` | form | `string` | 否 | — | — |
| `Protocol` | `protocol` | form | `int32` | 否 | — | 必须支持的协议，0 不限制 |
| `State` | `state` | form | `int32` | 否 | — | 0 不限制 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | 节点信息属运维面，proto 要求调用者 > 0 |

### `LiveIngestNodeListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveIngestNodeListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveNodeAssignmentList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 否 | — | — |
| `NodeId` | `node_id` | form | `string` | 否 | — | 与 stream_id 二选一 |
| `State` | `state` | form | `int32` | 否 | — | 0 不限制 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |

### `LiveNodeAssignmentListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveNodeAssignmentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStreamInterruptionList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 否 | — | — |
| `RoomId` | `room_id` | form | `int64` | 否 | — | 与 stream_id 二选一 |
| `OnlyOpen` | `only_open` | form | `bool` | 否 | — | — |
| `StartTime` | `start_time` | form | `int64` | 否 | — | started_at 下界（Unix 秒） |
| `EndTime` | `end_time` | form | `int64` | 否 | — | started_at 上界（Unix 秒） |
| `Limit` | `limit` | form | `int32` | 否 | — | 上限由服务夹取（MaxListPageSize） |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `LiveStreamInterruptionListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamInterruptionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStreamEventList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | form | `string` | 是 | — | — |
| `AfterSeq` | `after_seq` | form | `int64` | 否 | — | 0 表示从头 |
| `Limit` | `limit` | form | `int32` | 否 | — | 上限由服务夹取（MaxEventPageSize） |
| `Desc` | `desc` | form | `bool` | 否 | — | — |

### `LiveStreamEventListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamEventListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStreamClose`

> ParamLiveStreamClose 运营强制断流（处置动作）。admin 位不在 .api 暴露：本路由在 / AdminPermission 组里，能走到 logic 就说明权限已放行，网关固定写 true，让 live-ingest 走 / 运营分支而不要求「主播本人」。stop_reason 传 0 时由服务归一为 ADMIN，网关不代填。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | json | `string` | 是 | — | — |
| `StopReason` | `stop_reason` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveStreamCloseResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamCloseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveStreamKeyRevoke`

> ParamLiveStreamKeyRevoke 吊销密钥（禁播/泄露/风控）。admin 位由网关固定 true： / 运营侧才能吊销他人密钥，主播侧的自助吊销留在 gateway/app。stop_stream 是运营选择是否 / 级联停流，网关不代替它决定——停流会让直播立刻中断。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `KeyId` | `key_id` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `StopStream` | `stop_stream` | json | `bool` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveStreamKeyRevokeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveStreamKeyRevokeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveIngestNodeUpsert`

> ParamLiveIngestNodeUpsert 节点注册/元数据修改。 / heartbeat_only **不暴露**：proto 里它的语义是「只刷新 active_streams/health_score/ / last_heartbeat_at」，前两个是节点自己上报的观测量、第三个由服务取当前时间， / 后台表单拿不到这些事实，硬开一个开关只会把占用数与心跳清零（未注册节点还会被服务拒成 / ErrNodeNotFound）。节点心跳是机器链路，不进运营面。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Node` | `node` | json | `IngestNodeInput` | 是 | — | — |
| `CreateIfAbsent` | `create_if_absent` | json | `bool` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveIngestNodeUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveIngestNodeUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveFailedEventRetry`

> ParamLiveFailedEventRetry 把超过重试上限的 live.state.v1 事件重置为待发布。 / event_ids 为空时按 limit 批量重试全部失败事件，limit 上限由服务夹取（MaxEventRetryBatch）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventIds` | `event_ids` | json | `[]string` | 否 | — | — |
| `Limit` | `limit` | json | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveFailedEventRetryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveFailedEventRetryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomConnectionList`

> 边界（AGENTS.md §5）：长连接租约、房间路由、广播下发与广播审计流水、接入配额归 live-gateway； / 「谁在直播」的业务事实仍属 live-room / live-ingest，网关不在这里补任何房间状态判断。 /  / proto 18 个方法只开放 8 条（4 读 + 4 写），刻意不接的逐条记在 gateway/admin/README.md： /   - Acquire/Renew/Release/GetConnectionLease、ReportClientHeartbeat、JoinRoom、LeaveRoom、 /     Issue/RedeemReconnectTicket：客户端会话与断线重连路径，凭据（lease_id、ticket）必须由 /     持有者自己经过 gateway/app 的登录鉴权换取，后台代做等于代用户建连。 /   - RevokeReconnectTicket：撤销票据是踢人的附带效果，KickConnection(revoke_tickets=true) /     已覆盖；单开一个入口只会在审计里留下「撤了票据但没有处置连接」的孤行。 /   - GetRoomRoute：单房间路由查询被 /route/list 覆盖，广播第一跳的解析属服务内部。 /   - ForwardDanmaku / ForwardSystemEvent：弹幕与系统事件的服务间转发口，弹幕事实归 danmaku、 /     事件事实归 live-ingest/moderation，后台开面只会多一条伪造来源。 /   - SendToUser：对任意用户的定向推送等同后门私信。仓库既定政策是 admin 面对用户私有内容 /     既无 read-as-user 也无 act-as-user（见 /admin/inbox 与 gateway/app 的私信边界）， /     本路由必须等单独的授权设计，不在本轮开 HTTP 面。 /  / 归属判定：与 live-room / live-ingest 同一口径——只读面不挂 AdminPermission，写面 4 条全部挂。 /  / 主体口径与本域的另一处不同（比 live-room 更严）：livegateway.proto 的操作者字段是 / `operator string`（审计用，不是 mid 空间），因此网关**直接用会话 admin_id 生成** / `admin:<admin_id>`，表单不得声明 operator——后台的处置都能追到具体账号， / 不存在 live-room 那种「台账落在谁身上」的编号空间歧义（README 保留该缺口只针对 live-room）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | 必填：连接视图只在房间内有意义 |
| `Mid` | `mid` | form | `int64` | 否 | — | <=0 表示整个房间 |
| `Role` | `role` | form | `int32` | 否 | — | 0 不过滤（1 观众、2 主播、3 房管、4 运营、5 内部服务） |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | 上限由 live-gateway 夹取（PageParam.ps ≤ 50） |

### `LiveRoomConnectionListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomConnectionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomRouteList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `NodeId` | `node_id` | form | `string` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | 0 不限制、1 承接、2 排空中、3 已下线 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |

### `LiveRoomRouteListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomRouteListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveBroadcastLogList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | 必填：审计按房间查，避免全表扫 |
| `Kind` | `kind` | form | `int32` | 否 | — | — |
| `SenderMid` | `sender_mid` | form | `int64` | 否 | — | — |
| `OnlyDropped` | `only_dropped` | form | `bool` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |

### `LiveBroadcastLogListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveBroadcastLogListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveAccessQuotaGet`

> ParamLiveAccessQuotaGet 读取某作用域生效的配额（含服务侧继承链解析结果）。 / scope 传 0（QUOTA_SCOPE_UNSPECIFIED）没有对应语义，网关先拒；scope_id 的合法组合由服务判定 / （GLOBAL 必须 0、其余必须 >0 是 live-gateway 的口径，网关不复算）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | form | `int32` | 是 | — | 1 全局、2 节点、3 房间、4 用户 |
| `ScopeId` | `scope_id` | form | `int64` | 否 | — | — |

### `LiveAccessQuotaResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAccessQuotaData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveConnectionKick`

> ParamLiveConnectionKick 强制下线（风控/审核处置）。mid 与 lease_id 至少给一个， / conn_id 为空表示断该用户在该房间的全部连接；ban_seconds>0 会写禁止重连窗口， / 因此本路由与 /route/drain 分属不同权限点。reason 的取值合法性由 live-gateway 判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `LeaseId` | `lease_id` | json | `string` | 否 | — | — |
| `ConnId` | `conn_id` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `BanSeconds` | `ban_seconds` | json | `int32` | 否 | — | — |
| `RevokeTickets` | `revoke_tickets` | json | `bool` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveConnectionKickResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveConnectionKickData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveRoomRouteDrain`

> ParamLiveRoomRouteDrain 排空某节点上的房间路由（优雅下线）。expected_version 原样透传： / 版本不符由服务判冲突，网关不重试、不自动重读版本（那会把并发发布场景下的误伤藏起来）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `NodeId` | `node_id` | json | `string` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 是 | — | — |
| `TargetNodeId` | `target_node_id` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveRoomRouteDrainResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveRoomRouteDrainData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveBroadcastSend`

> ParamLiveBroadcastSend 房间内公告/系统事件下发。 / sender_role 与 sender_mid **都不是表单字段**：后台发的就是运营消息，网关固定写 / CONN_ROLE_OPERATOR + sender_mid=0（proto：系统消息为 0），否则 live-gateway 的权限矩阵 / 会把后台伪装成主播或观众的消息按用户态消息要求租约/票据（后台拿不到，也不该拿）。 / sender_lease_id / sender_ticket 同样不暴露：那是客户端凭据，后台面没有它们。 / 契约缺口：BroadcastToRoomReq **没有 operator 位**（只有 sender_mid/sender_role/trace_id）， / 因此本路由的后台身份只能落在网关日志（admin_id）与 trace_id 上，广播台账里看不到 / 「哪个运营账号发的」——补法是先给 proto 增加运营主体字段（本轮不改 services/**）。 / 已知缺口：require_reliable=true 时若下发通道未接线（服务 README 的 stub 缺口）， / 服务会显式失败而不是静默丢失——网关原样上抛，不把它兜成「已发送」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Kind` | `kind` | json | `int32` | 是 | — | 3 系统通知、5 审核处置、6 主播提词等，见 BroadcastKind |
| `MessageId` | `message_id` | json | `string` | 是 | — | 幂等键：(room_id, message_id) 唯一，本路由的门槛键 |
| `Payload` | `payload` | json | `string` | 是 | — | JSON/文本载荷，按 bytes 原样交给服务 |
| `TargetRoles` | `target_roles` | json | `[]string` | 否 | — | 空表示全体 |
| `TargetTopics` | `target_topics` | json | `[]string` | 否 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 否 | — | 0 表示不失效 |
| `Priority` | `priority` | json | `int32` | 否 | — | 0 普通、1 高 |
| `RequireReliable` | `require_reliable` | json | `bool` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveBroadcastSendResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveBroadcastSendData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveAccessQuotaUpsert`

> ParamLiveAccessQuotaUpsert 新建/更新配额。expected_version=0 表示新建（行已存在则服务回冲突）， / 非 0 是 CAS：网关不改写版本值，也不在冲突后自动重试——配额直接影响所有下发。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Quota` | `quota` | json | `AccessQuotaInput` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveAccessQuotaUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveAccessQuotaUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaTranscodeGet`

> ParamLiveMediaTranscodeGet 单个转码任务。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | form | `int64` | 是 | — | — |

### `LiveMediaTranscodeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaTranscodeList`

> ParamLiveMediaTranscodeList 转码任务分页。四个过滤位的 0/UNSPECIFIED 都是「不过滤」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `TemplateId` | `template_id` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `LiveMediaTranscodeListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaOutputList`

> ParamLiveMediaOutputList 房间当前可分发档位（live-gateway/live-room 的只读投影同源）。 / live_session_id=0 表示只看当前在线档位（服务侧语义是 <=0，网关先拒负数）； / include_offline 决定是否带历史档位。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | form | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | form | `int64` | 否 | — | — |
| `IncludeOffline` | `include_offline` | form | `bool` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 否 | — | — |
| `Ps` | `ps` | form | `int32` | 否 | — | — |

### `LiveMediaOutputListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaOutputListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaRecordGet`

> ParamLiveMediaRecordGet 单个录制任务（含 last_seq，供断点续录核对）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RecordId` | `record_id` | form | `int64` | 是 | — | — |

### `LiveMediaRecordResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaRecordList`

> ParamLiveMediaRecordList 录制任务分页。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `LiveMediaRecordListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaRecordSegmentList`

> ParamLiveMediaRecordSegmentList 切片 keyset 分页：一场三小时直播是数千行， / 深翻页用 after_seq 游标而非 pn/ps（limit 上限由服务夹取）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `AfterSeq` | `after_seq` | json | `int64` | 否 | — | — |
| `Limit` | `limit` | json | `int32` | 否 | — | — |

### `LiveMediaRecordSegmentListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordSegmentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaReplayGet`

> ParamLiveMediaReplayGet 单个回放任务。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReplayId` | `replay_id` | form | `int64` | 是 | — | — |

### `LiveMediaReplayResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaReplayList`

> ParamLiveMediaReplayList 回放任务分页。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `LiveMediaReplayListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaReplayAssetList`

> ParamLiveMediaReplayAssetList 回放资产引用分页（房间/场次/主播/投影状态过滤）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `ReviewState` | `review_state` | json | `int32` | 否 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `LiveMediaReplayAssetListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayAssetListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaRetentionGet`

> ParamLiveMediaRetentionGet 单个回收任务。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RetentionId` | `retention_id` | form | `int64` | 是 | — | — |

### `LiveMediaRetentionResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRetentionTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaRetentionList`

> ParamLiveMediaRetentionList 回收任务分页。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TargetKind` | `target_kind` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `LiveMediaRetentionListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRetentionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaTranscodeStart`

> ParamLiveMediaTranscodeStart 登记直播转码任务（服务侧落 PENDING，不在此拉起 FFmpeg）。 / 幂等键 request_id 必填：同一键重放返回同一任务，运营连点或前端重试都不会多开一路转码。 / max_attempts/timeout_seconds 传 0 表示用服务默认；operator 位在本契约里不存在（见上面的缺口说明）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `TemplateId` | `template_id` | json | `int64` | 是 | — | — |
| `BitrateLevel` | `bitrate_level` | json | `int32` | 是 | — | — |
| `Protocol` | `protocol` | json | `int32` | 是 | — | — |
| `SourceRef` | `source_ref` | json | `string` | 是 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 否 | — | — |
| `MaxAttempts` | `max_attempts` | json | `int32` | 否 | — | — |
| `TimeoutSeconds` | `timeout_seconds` | json | `int32` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaTranscodeStartResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaTranscodeStop`

> ParamLiveMediaTranscodeStop 请求停止（RUNNING→STOPPING，Worker 收尾后才到 STOPPED）。 / expected_version=0 表示不校验版本（仍受状态机约束）；reason 必须说明为什么停（人工停止是 7 MANUAL）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaTranscodeStopResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaTranscodeRetry`

> ParamLiveMediaTranscodeRetry 重试失败任务（FAILED→PENDING，attempt+1，受 max_attempts 限制）。 / reason 是网关侧的审计门槛：重开一路转码必须留下「为什么重开」的说明； / 同 request_id 重放不会重复 ++attempt。能否重试（状态是否 FAILED、次数是否用尽）由服务判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaTranscodeRetryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaTranscodeCancel`

> ParamLiveMediaTranscodeCancel 取消未运行/停止中的任务（PENDING|STOPPING→CANCELLED 终态）。 / 与 stop 分开授权：cancel 是放弃任务且不可复活（只能重新 start），stop 只是让运行中的任务收尾。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaTranscodeCancelResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaTranscodeTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaOutputUpsert`

> ParamLiveMediaOutputUpsert 登记或刷新一个码率档位的分发输出（(room,session,level,protocol) 唯一）。 / 档位参数（width/height/bitrate_kbps/fps）是**下发时刻的快照**，不是 transcode 模板的镜像， / 模板后续变更不回写历史行；bucket 与 object_key 必须同时给或同时不给（只给一半的引用不可用）。 / online_expire_at=0 表示由断流事件下线。operator 位契约里没有（缺口见 README）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `TaskId` | `task_id` | json | `int64` | 否 | — | 0 表示源流直出 |
| `BitrateLevel` | `bitrate_level` | json | `int32` | 是 | — | — |
| `Protocol` | `protocol` | json | `int32` | 是 | — | — |
| `Bucket` | `bucket` | json | `string` | 否 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 否 | — | — |
| `CdnDomain` | `cdn_domain` | json | `string` | 否 | — | — |
| `Width` | `width` | json | `int32` | 否 | — | — |
| `Height` | `height` | json | `int32` | 否 | — | — |
| `BitrateKbps` | `bitrate_kbps` | json | `int32` | 否 | — | — |
| `Fps` | `fps` | json | `int32` | 否 | — | — |
| `OnlineExpireAt` | `online_expire_at` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaOutputUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaStreamOutputInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaOutputOffline`

> ParamLiveMediaOutputOffline 下线一个档位。寻址二选一：output_id，或 / (room_id, bitrate_level, protocol) 三元组；两者都不全时服务无从定位，网关先拒。 / 与回放发布状态无关（断流即下线档位），因此该动作只影响观众侧可用性，权限点单列。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OutputId` | `output_id` | json | `int64` | 否 | — | — |
| `RoomId` | `room_id` | json | `int64` | 否 | — | — |
| `BitrateLevel` | `bitrate_level` | json | `int32` | 否 | — | — |
| `Protocol` | `protocol` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaOutputOfflineResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaStreamOutputInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaRecordStart`

> ParamLiveMediaRecordStart 登记录制任务（服务侧落 PENDING）。 / start_at/end_at 是期望区间（0 分别表示立即 / 随场次结束），窗口倒置网关先拒； / segment_seconds/timeout_seconds 传 0 用服务默认；切片上限与分片时长合法值由服务判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `SourceTaskId` | `source_task_id` | json | `int64` | 否 | — | 0 表示原画源 |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `SegmentSeconds` | `segment_seconds` | json | `int32` | 否 | — | — |
| `TimeoutSeconds` | `timeout_seconds` | json | `int32` | 否 | — | — |
| `OutputBucket` | `output_bucket` | json | `string` | 否 | — | — |
| `OutputPrefix` | `output_prefix` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaRecordStartResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaRecordStop`

> ParamLiveMediaRecordStop 停止录制（RECORDING→STOPPING，最后一片落库后 STOPPED）。 / end_at=0 表示立即停止；只有 STOPPED 之后才允许拼接回放（服务判定，网关不复算）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaRecordStopResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRecordTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaReplaySubmit`

> ParamLiveMediaReplaySubmit 提交回放拼接任务：**只登记与校验切片区间**， / 不拼接、不写 asset、不建稿件、更不推进发布状态（后续由 Worker 回填）。 / allow_gaps=false 时切片有缺口会被服务直接拒（避免产出坏回放）；title 会成为稿件标题， / 因此网关要求非空，但标题是否合规由 video/审核链路判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `live_session_id` | json | `int64` | 否 | — | — |
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `FromSeq` | `from_seq` | json | `int64` | 否 | — | — |
| `ToSeq` | `to_seq` | json | `int64` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `AllowGaps` | `allow_gaps` | json | `bool` | 否 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 否 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaReplaySubmitResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaReplayAssetBind`

> ParamLiveMediaReplayAssetBind 回填「回放产物 ↔ asset/稿件」引用。 / 关键约束：live-media 只写引用行，不动 asset_meta、不动 video_submission、 / 不调用任何推进稿件状态的路径；回放的审核与发布归 video/moderation-orchestrator（AGENTS.md §5/§8）。 / asset_id 与 aid 至少给一个（两阶段回填：先媒资、后建稿），都为空时这一行没有任何引用可存。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReplayId` | `replay_id` | json | `int64` | 是 | — | — |
| `AssetId` | `asset_id` | json | `int64` | 否 | — | — |
| `Aid` | `aid` | json | `int64` | 否 | — | — |
| `Bvid` | `bvid` | json | `string` | 否 | — | — |
| `Bucket` | `bucket` | json | `string` | 否 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 否 | — | — |
| `DurationMs` | `duration_ms` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaReplayAssetBindResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayAssetRefInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaReplayContentState`

> ParamLiveMediaReplayContentState 手工刷新 video 侧的审核/发布投影（排障入口）。 / 方向单一：video → live-media。本路由**不能**推进稿件状态，review_state 必须是 / 从 video 看到的事实值（1 审核中 … 5 已删除），0（未同步）在这里没有语义因此拒掉。 / source 由网关固定为 `manual`（不是表单字段）：后台点出来的刷新不可能是 / content.published.v1 消费者或 video.rpc 触发的，伪造来源会让引用行的投影留痕失真。 / 缺口：本方法没有 operator 位，「谁刷的」只落在网关日志；event_id 可选（见上面的说明）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReplayId` | `replay_id` | json | `int64` | 否 | — | — |
| `AssetId` | `asset_id` | json | `int64` | 否 | — | — |
| `ReviewState` | `review_state` | json | `int32` | 是 | — | — |
| `PublishedAt` | `published_at` | json | `int64` | 否 | — | — |
| `EventId` | `event_id` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaReplayContentStateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaReplayAssetRefInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiveMediaRetentionSubmit`

> ParamLiveMediaRetentionSubmit 提交回收任务（超期切片 / 回放产物 / 残留档位）。 / 回收是「先登记意图、再执行、最后留证」的三步流程，禁止边查边删：本路由只登记， / 执行与结果回报都在 Worker。purge=false 只登记并置标记，purge=true 才真删对象存储引用， / 两者共用一个 submit 权限点（purge 的额外后果由 reason 必填 + 服务侧审计承担， / reason 在契约里就注明「审计必填」）。batch_limit 上限（500）由服务夹取。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TargetKind` | `target_kind` | json | `int32` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 否 | — | 0 表示全局扫描（服务侧 <=0，网关拒负数） |
| `TargetId` | `target_id` | json | `int64` | 否 | — | 0 表示按 expire_before 批量 |
| `ExpireBefore` | `expire_before` | json | `int64` | 否 | — | — |
| `Purge` | `purge` | json | `bool` | 否 | — | — |
| `BatchLimit` | `batch_limit` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `LiveMediaRetentionSubmitResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LiveMediaRetentionTaskInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `LiveRoomDetailData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Room` | `room` | json | `LiveRoomInfo` | 是 | — | — |
| `Setting` | `setting` | json | `LiveRoomSetting` | 是 | — | — |
| `HasSetting` | `has_setting` | json | `bool` | 是 | — | false 表示未请求或服务侧无配置行 |
| `ActiveSession` | `active_session` | json | `LiveSessionInfo` | 是 | — | — |
| `HasActiveSession` | `has_active_session` | json | `bool` | 是 | — | — |

### `LiveRoomListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]LiveRoomInfo` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Page` | `page` | json | `int32` | 是 | — | — |
| `PageSize` | `page_size` | json | `int32` | 是 | — | — |

### `LiveRoomBansData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]LiveRoomBanInfo` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Page` | `page` | json | `int32` | 是 | — | — |
| `PageSize` | `page_size` | json | `int32` | 是 | — | — |

### `LiveSessionData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Session` | `session` | json | `LiveSessionInfo` | 是 | — | — |

### `LiveSessionListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]LiveSessionInfo` | 是 | — | session_id 倒序 |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | 空表示到底 |

### `LiveAreaListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]LiveAreaInfo` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `LiveAnchorListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]LiveAnchorInfo` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `LiveRoomCloseData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 是 | — | 迁移后状态（FINISHED） |
| `TerminatedSessionId` | `terminated_session_id` | json | `int64` | 是 | — | 被强制终止的场次，0 表示无 |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LiveRoomBanData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BanId` | `ban_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `TerminatedSessionId` | `terminated_session_id` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | 0 表示永久 |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LiveRoomBanLiftData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BanId` | `ban_id` | json | `int64` | 是 | — | 0 表示无生效记录 |
| `State` | `state` | json | `int32` | 是 | — | BANNED → READY |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |

### `LiveRoomSettingInput`

> LiveRoomSettingInput 只列 protobuf RoomSetting 里可被写入的字段：room_id 由请求顶层的 / room_id 决定、mtime 由服务维护，两者不接受后台声明（伪造 mtime 会污染配置变更审计）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DanmakuEnabled` | `danmaku_enabled` | json | `bool` | 是 | — | — |
| `ReplyEnabled` | `reply_enabled` | json | `bool` | 是 | — | — |
| `RecordEnabled` | `record_enabled` | json | `bool` | 是 | — | — |
| `LinkmicEnabled` | `linkmic_enabled` | json | `bool` | 是 | — | — |
| `LiveType` | `live_type` | json | `int32` | 否 | — | 0 由服务按视频直播处理 |
| `MinClientVersionCode` | `min_client_version_code` | json | `int32` | 否 | — | 0 表示不限制 |

### `LiveRoomSettingUpdateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Setting` | `setting` | json | `LiveRoomSetting` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LiveAreaUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AreaId` | `area_id` | json | `int64` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | true 表示本次新建 |

### `LiveStreamStateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Stream` | `stream` | json | `LiveStreamInfo` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | false 表示无匹配记录（此时 stream 是全零值） |

### `LiveStreamListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]LiveStreamInfo` | 是 | — | last_heartbeat_at 升序（最可疑的在前） |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |
| `ServerTime` | `server_time` | json | `int64` | 是 | — | — |

### `LiveStreamKeyData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Key` | `key` | json | `StreamKeyInfo` | 是 | — | — |
| `HasKey` | `has_key` | json | `bool` | 是 | — | false 表示服务没给这一段（密钥不存在） |

### `LiveStreamKeyListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]StreamKeyInfo` | 是 | — | key_id 倒序 |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |

### `LiveStreamHealthData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `HealthState` | `health_state` | json | `int32` | 是 | — | 1 正常、2 劣化、3 危险、4 无采样 |
| `HealthReportedAt` | `health_reported_at` | json | `int64` | 是 | — | — |
| `AvgVideoBitrateBps` | `avg_video_bitrate_bps` | json | `int64` | 是 | — | — |
| `MinVideoBitrateBps` | `min_video_bitrate_bps` | json | `int64` | 是 | — | — |
| `MaxPacketLossPpm` | `max_packet_loss_ppm` | json | `int32` | 是 | — | — |
| `SampleCount` | `sample_count` | json | `int32` | 是 | — | — |
| `Samples` | `samples` | json | `[]LiveHealthSample` | 是 | — | — |
| `InterruptedTotalSeconds` | `interrupted_total_seconds` | json | `int64` | 是 | — | — |
| `InterruptionCount` | `interruption_count` | json | `int32` | 是 | — | — |

### `LiveIngestNodeListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]IngestNodeInfo` | 是 | — | health_score 降序 |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |

### `LiveNodeAssignmentListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]NodeAssignmentInfo` | 是 | — | assignment_id 倒序 |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |

### `LiveStreamInterruptionListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]StreamInterruptionInfo` | 是 | — | started_at 升序 |
| `Total` | `total` | json | `int32` | 是 | — | 命中服务侧计数上限时为 -1 |

### `LiveStreamEventListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]StreamEventInfo` | 是 | — | — |
| `MaxSeq` | `max_seq` | json | `int64` | 是 | — | 该流当前最大 seq，据此判断是否追平 |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |

### `LiveStreamCloseData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 是 | — | 迁移后状态（STOPPED） |
| `Seq` | `seq` | json | `int64` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `InterruptedTotalSeconds` | `interrupted_total_seconds` | json | `int64` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | 命中 request_id |
| `Applied` | `applied` | json | `bool` | 是 | — | false 表示本就终态、无变更（幂等成功） |
| `Message` | `message` | json | `string` | 是 | — | — |

### `LiveStreamKeyRevokeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 是 | — | 吊销后状态（REVOKED） |
| `StoppedStreamIds` | `stopped_stream_ids` | json | `[]string` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |

### `IngestNodeInput`

> IngestNodeInput 节点可写字段白名单：active_streams / last_heartbeat_at / ctime / mtime / 都是 live-ingest 从分配记录与节点心跳派生的值，不接受后台声明（手填占用数会让配额与打分失真）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `NodeId` | `node_id` | json | `string` | 是 | — | 必填，运维分配的稳定标识 |
| `Name` | `name` | json | `string` | 否 | — | — |
| `Region` | `region` | json | `string` | 否 | — | — |
| `Protocols` | `protocols` | json | `[]int32` | 否 | — | — |
| `EndpointRtmp` | `endpoint_rtmp` | json | `string` | 否 | — | — |
| `EndpointSrt` | `endpoint_srt` | json | `string` | 否 | — | — |
| `EndpointWebrtc` | `endpoint_webrtc` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 在线、2 摘流中、3 离线 |
| `CapacityStreams` | `capacity_streams` | json | `int32` | 否 | — | — |
| `HealthScore` | `health_score` | json | `int32` | 否 | — | 0~100 由服务夹取 |
| `Labels` | `labels` | json | `string` | 否 | — | — |

### `LiveIngestNodeUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Node` | `node` | json | `IngestNodeInfo` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `LiveFailedEventRetryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Retried` | `retried` | json | `int32` | 是 | — | — |
| `RemainingFailed` | `remaining_failed` | json | `int32` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |

### `LiveRoomConnectionListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]LiveConnectionLease` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `SnapshotFromCache` | `snapshot_from_cache` | json | `bool` | 是 | — | true 表示 Redis 不可用、读数偏旧 |

### `LiveRoomRouteListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]RoomRouteInfo` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `LiveBroadcastLogListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]BroadcastLogInfo` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `LiveAccessQuotaData`

> LiveAccessQuotaData 回的是**解析后的生效值**，不是某一行原始配置：GetAccessQuota 按 / QuotaScopeChain 自外向内覆盖并在无 GLOBAL 行时回落到进程配置默认值（服务保证不报错）， / 因此本路由无法区分「显式配置」与「继承默认」——契约缺口（服务的 AccessQuotaInfo 没有 / hit_scopes 字段，见其 README），要补得先在 proto 加字段（本轮不改 services/**）。 / has_quota 只表示「服务有没有回这一段」，与 live-room 的 has_setting 同一口径。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Quota` | `quota` | json | `AccessQuotaInfo` | 是 | — | — |
| `HasQuota` | `has_quota` | json | `bool` | 是 | — | — |

### `LiveConnectionKickData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Kicked` | `kicked` | json | `bool` | 是 | — | — |
| `KickedConnections` | `kicked_connections` | json | `int32` | 是 | — | — |
| `RevokedTickets` | `revoked_tickets` | json | `int32` | 是 | — | — |
| `BanUntil` | `ban_until` | json | `int64` | 是 | — | 0 表示不禁止重连 |
| `DenyReason` | `deny_reason` | json | `int32` | 是 | — | 操作者无权限时的原因，必须可解释 |

### `LiveRoomRouteDrainData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Route` | `route` | json | `RoomRouteInfo` | 是 | — | — |

### `LiveBroadcastSendData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Accepted` | `accepted` | json | `bool` | 是 | — | — |
| `MessageId` | `message_id` | json | `string` | 是 | — | — |
| `DropReason` | `drop_reason` | json | `int32` | 是 | — | 1 表示正常下发；丢弃时必须有原因 |
| `FanoutNodes` | `fanout_nodes` | json | `int32` | 是 | — | — |
| `TargetedConnections` | `targeted_connections` | json | `int32` | 是 | — | — |
| `EnqueuedAt` | `enqueued_at` | json | `int64` | 是 | — | — |
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | 命中 message_id 去重 |
| `RateRemaining` | `rate_remaining` | json | `int32` | 是 | — | — |

### `AccessQuotaInput`

> AccessQuotaInput 配额可写字段：version / updated_by / ctime / mtime 由服务维护 / （并发版本走请求顶层 expected_version，修改者走会话），后台不得声明「谁改的」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | json | `int32` | 是 | — | 必填：1 全局、2 节点、3 房间、4 用户 |
| `ScopeId` | `scope_id` | json | `int64` | 否 | — | GLOBAL 必须 0，由服务判定 |
| `ScopeKey` | `scope_key` | json | `string` | 否 | — | 可读标识（node_id 等），仅展示与排障 |
| `MaxConnections` | `max_connections` | json | `int32` | 否 | — | 0 表示继承上一层 |
| `BroadcastQps` | `broadcast_qps` | json | `int32` | 否 | — | — |
| `DanmakuQps` | `danmaku_qps` | json | `int32` | 否 | — | — |
| `LeaseTtlSeconds` | `lease_ttl_seconds` | json | `int32` | 否 | — | — |
| `TicketTtlSeconds` | `ticket_ttl_seconds` | json | `int32` | 否 | — | — |
| `MaxPayloadBytes` | `max_payload_bytes` | json | `int32` | 否 | — | — |
| `AllowGuest` | `allow_guest` | json | `bool` | 否 | — | — |

### `LiveAccessQuotaUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Quota` | `quota` | json | `AccessQuotaInfo` | 是 | — | — |

### `LiveMediaTranscodeTaskInfo`

> 边界（AGENTS.md §5/§8）：live-media 拥有 live_transcode_task / live_stream_output / / live_record_task / live_record_segment / live_replay_task / live_replay_asset_ref / / live_retention_task / live_media_outbox。直播间与场次归 live-room、推流密钥与流状态归 / live-ingest、长连接下发归 live-gateway、媒资元数据归 asset、稿件与发布状态归 video、 / 转码模板主数据归 transcode —— 本域只保存它们的主键，网关也不跨服务拼状态、不代为推进 / 任何其它服务的状态机。 /  / proto 28 个方法只开放 23 条（11 读 + 12 写）。刻意不接的 5 个 Report* 逐条记在 / gateway/admin/README.md，它们是同一类入口： /   - ReportLiveTranscodeProgress / ReportLiveRecordProgress / ReportReplayProgress / /     ReportRecordSegment / ReportRetentionResult：全部是 **Worker→服务** 的回报口， /     入参带 worker_id 与 expected_version（乐观并发令牌），事实来源只能是执行体本身。 /     后台开面等于让运营手写「Worker 说它跑完了」，直接把转码/录制/回放/回收状态机 /     的推进权交给控制台（AGENTS.md §8「回调只能推进合法状态，不能直接写入终态」）。 /  / 归属判定与 live-room / live-ingest / live-gateway / recommend 同一口径： /   - 只读面不挂 AdminPermission（后台列表页每次刷新都会打一次 RPC，全量挂判定会把 /     operation 变成读放大瓶颈），任务台账本身不带隐私正文，切片与产物只是对象存储引用； /   - 写面 12 条全部挂 AdminPermission，并要求会话身份存在 + request_id 非空， /     在出网关之前就拒掉。 /  / 主体口径（沿用 live-gateway / cron / recommend，比 live-room 更严）：本域 proto 的操作者字段是 / `operator string`（审计用），因此凡有该位的方法（Stop/Retry/Cancel 转码、Stop 录制、 / 提交回收）一律由会话 admin_id 渲染成 `admin:<admin_id>`，**表单不声明 operator**。 /  / 已知契约缺口（另见 README，本轮不改 services/**）： /   - StartLiveTranscode / StartLiveRecord / UpsertStreamOutput / OfflineStreamOutput / /     SubmitReplayTask / BindReplayAsset / ApplyReplayContentState **没有 operator 位**： /     后台点了哪个按钮只能落在网关日志上，任务行/引用行看不到操作者； /   - ApplyReplayContentState 的幂等键是 event_id（契约语义是「驱动本次同步的事件 ID」）， /     人工刷新没有事件可引，网关不伪造随机值（那会让服务侧去重永远命不中、形同放开重复写）， /     因此该路由的 event_id 为可选，且它是 12 条写路由里唯一没有 request_id 的一条。 /  / 数值口径：0 在本域普遍是合法哨兵（expected_version=0 不校验版本、from_seq/to_seq=0 表示 / 全区间、max_attempts/timeout_seconds/segment_seconds/batch_limit/ps=0 用服务默认、 / room_id/target_id=0 表示不按该维度过滤或全局扫描、live_session_id=0 表示未提供）， / 服务侧写成 <=0 的过滤位在网关统一收敛成「负数先拒、0 才是未提供」； / 只有 `*_UNSPECIFIED` 的枚举目标位（reason/review_state/target_kind/bitrate_level/protocol） / 在语义上不可省略，网关直接拒绝，具体取值合法性与状态迁移仍由 live-media 判定。 / LiveMediaTranscodeTaskInfo 转码任务整行（live_transcode_task 投影）。 / version 是乐观并发令牌（上报与处置都要回传），heartbeat_at/timeout_at 是判活依据， / attempt/max_attempts 决定还能不能 Retry，errno/err_msg 已脱敏（契约注明不含密钥与完整 URL）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `LiveSessionId` | `live_session_id` | json | `int64` | 是 | — | — |
| `TemplateId` | `template_id` | json | `int64` | 是 | — | 引用 transcode 主键，不复制模板主数据 |
| `BitrateLevel` | `bitrate_level` | json | `int32` | 是 | — | 1 原画 … 6 纯音频 |
| `Protocol` | `protocol` | json | `int32` | 是 | — | 1 HLS、2 HTTP-FLV、3 RTMP、4 ARTC |
| `SourceRef` | `source_ref` | json | `string` | 是 | — | 拉流源引用，非长期密钥 |
| `AnchorMid` | `anchor_mid` | json | `int64` | 是 | — | 仅审计，不做商业化判断 |
| `State` | `state` | json | `int32` | 是 | — | 1 PENDING … 6 CANCELLED |
| `Progress` | `progress` | json | `int32` | 是 | — | 0-100，含义随 state |
| `Attempt` | `attempt` | json | `int32` | 是 | — | — |
| `MaxAttempts` | `max_attempts` | json | `int32` | 是 | — | — |
| `StartedAt` | `started_at` | json | `int64` | 是 | — | — |
| `StoppedAt` | `stopped_at` | json | `int64` | 是 | — | — |
| `HeartbeatAt` | `heartbeat_at` | json | `int64` | 是 | — | — |
| `TimeoutAt` | `timeout_at` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | FailureReason |
| `Errno` | `errno` | json | `int32` | 是 | — | — |
| `ErrMsg` | `err_msg` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveMediaTranscodeListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `List` | `list` | json | `[]LiveMediaTranscodeTaskInfo` | 是 | — | — |

### `LiveMediaOutputListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `List` | `list` | json | `[]LiveMediaStreamOutputInfo` | 是 | — | — |

### `LiveMediaRecordTaskInfo`

> LiveMediaRecordTaskInfo 录制任务整行。 / last_seq/segment_count/gap_count/recorded_duration_ms 是断点续录与「回放有没有洞」的证据， / 一个都不能裁。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `LiveSessionId` | `live_session_id` | json | `int64` | 是 | — | — |
| `SourceTaskId` | `source_task_id` | json | `int64` | 是 | — | 0 表示原画源 |
| `State` | `state` | json | `int32` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `RecordStartAt` | `record_start_at` | json | `int64` | 是 | — | — |
| `RecordEndAt` | `record_end_at` | json | `int64` | 是 | — | — |
| `SegmentSeconds` | `segment_seconds` | json | `int32` | 是 | — | — |
| `LastSeq` | `last_seq` | json | `int64` | 是 | — | — |
| `SegmentCount` | `segment_count` | json | `int64` | 是 | — | — |
| `GapCount` | `gap_count` | json | `int64` | 是 | — | — |
| `RecordedDurationMs` | `recorded_duration_ms` | json | `int64` | 是 | — | — |
| `OutputBucket` | `output_bucket` | json | `string` | 是 | — | — |
| `OutputPrefix` | `output_prefix` | json | `string` | 是 | — | — |
| `HeartbeatAt` | `heartbeat_at` | json | `int64` | 是 | — | — |
| `TimeoutAt` | `timeout_at` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `Errno` | `errno` | json | `int32` | 是 | — | — |
| `ErrMsg` | `err_msg` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveMediaRecordListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `List` | `list` | json | `[]LiveMediaRecordTaskInfo` | 是 | — | — |

### `LiveMediaRecordSegmentListData`

> LiveMediaRecordSegmentListData total 是 int64：切片列表走的是 proto 里独立的 / ListRecordSegmentsReply.total（int64，该 record 下切片总数含缺口）， / 与其余五张台账用的 PageResult.total（int32）不是同一个字段，网关不做窄化转换。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `NextAfterSeq` | `next_after_seq` | json | `int64` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `List` | `list` | json | `[]LiveMediaRecordSegmentInfo` | 是 | — | — |

### `LiveMediaReplayTaskInfo`

> LiveMediaReplayTaskInfo 回放拼接任务整行。 / asset_id/aid/bvid 只是引用，bvid 是冗余展示字段（事实源仍是 video）； / state=6 COMPLETED 的含义是「由 video 投影得知回放已可用」，本服务永不把自己或稿件写成已发布。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReplayId` | `replay_id` | json | `int64` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `LiveSessionId` | `live_session_id` | json | `int64` | 是 | — | — |
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `FromSeq` | `from_seq` | json | `int64` | 是 | — | — |
| `ToSeq` | `to_seq` | json | `int64` | 是 | — | — |
| `SegmentCount` | `segment_count` | json | `int64` | 是 | — | — |
| `GapCount` | `gap_count` | json | `int64` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `DurationMs` | `duration_ms` | json | `int64` | 是 | — | — |
| `AllowGaps` | `allow_gaps` | json | `bool` | 是 | — | — |
| `OutputBucket` | `output_bucket` | json | `string` | 是 | — | — |
| `OutputKey` | `output_key` | json | `string` | 是 | — | — |
| `AssetId` | `asset_id` | json | `int64` | 是 | — | — |
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `Bvid` | `bvid` | json | `string` | 是 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `Errno` | `errno` | json | `int32` | 是 | — | — |
| `ErrMsg` | `err_msg` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveMediaReplayListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `List` | `list` | json | `[]LiveMediaReplayTaskInfo` | 是 | — | — |

### `LiveMediaReplayAssetListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `List` | `list` | json | `[]LiveMediaReplayAssetRefInfo` | 是 | — | — |

### `LiveMediaRetentionTaskInfo`

> LiveMediaRetentionTaskInfo 回收任务整行（scanned/deleted/skipped 是「先登记后执行」的凭证， / purge=false 时 deleted 恒为 0）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RetentionId` | `retention_id` | json | `int64` | 是 | — | — |
| `TargetKind` | `target_kind` | json | `int32` | 是 | — | 1 切片、2 回放产物、3 残留档位 |
| `RoomId` | `room_id` | json | `int64` | 是 | — | 0 表示全局扫描 |
| `TargetId` | `target_id` | json | `int64` | 是 | — | 0 表示按 expire_before 批量 |
| `ExpireBefore` | `expire_before` | json | `int64` | 是 | — | — |
| `Purge` | `purge` | json | `bool` | 是 | — | — |
| `BatchLimit` | `batch_limit` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Scanned` | `scanned` | json | `int32` | 是 | — | — |
| `Deleted` | `deleted` | json | `int32` | 是 | — | — |
| `Skipped` | `skipped` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `FailReason` | `fail_reason` | json | `int32` | 是 | — | — |
| `Errno` | `errno` | json | `int32` | 是 | — | — |
| `ErrMsg` | `err_msg` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveMediaRetentionListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `List` | `list` | json | `[]LiveMediaRetentionTaskInfo` | 是 | — | — |

### `LiveMediaStreamOutputInfo`

> LiveMediaStreamOutputInfo 分发档位（live_stream_output 投影）。 / state 在 proto 里就是 int32（1 在线、2 已下线），与房间/流状态无关，网关按值投影； / bucket/object_key/cdn_domain 只是相对路径与域名，不含签名。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OutputId` | `output_id` | json | `int64` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `LiveSessionId` | `live_session_id` | json | `int64` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | 0 表示源流直出不经转码 |
| `BitrateLevel` | `bitrate_level` | json | `int32` | 是 | — | — |
| `Protocol` | `protocol` | json | `int32` | 是 | — | — |
| `Bucket` | `bucket` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |
| `CdnDomain` | `cdn_domain` | json | `string` | 是 | — | — |
| `Width` | `width` | json | `int32` | 是 | — | — |
| `Height` | `height` | json | `int32` | 是 | — | — |
| `BitrateKbps` | `bitrate_kbps` | json | `int32` | 是 | — | — |
| `Fps` | `fps` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `OnlineAt` | `online_at` | json | `int64` | 是 | — | — |
| `OfflineAt` | `offline_at` | json | `int64` | 是 | — | — |
| `OnlineExpireAt` | `online_expire_at` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | 下线原因（state=2 时有效） |

### `LiveMediaReplayAssetRefInfo`

> LiveMediaReplayAssetRefInfo 回放产物 ↔ asset/稿件 引用行。 / review_state/published_at/review_state_at 都来自 video 的**只读投影**， / 事实源不是本服务；retention_state 是引用行的生命周期标记（0 正常、1 待回收、2 已回收）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `LiveSessionId` | `live_session_id` | json | `int64` | 是 | — | — |
| `ReplayId` | `replay_id` | json | `int64` | 是 | — | — |
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `AssetId` | `asset_id` | json | `int64` | 是 | — | — |
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `Bvid` | `bvid` | json | `string` | 是 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 是 | — | — |
| `Bucket` | `bucket` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |
| `DurationMs` | `duration_ms` | json | `int64` | 是 | — | — |
| `SegmentFromSeq` | `segment_from_seq` | json | `int64` | 是 | — | — |
| `SegmentToSeq` | `segment_to_seq` | json | `int64` | 是 | — | — |
| `GapCount` | `gap_count` | json | `int64` | 是 | — | — |
| `ReviewState` | `review_state` | json | `int32` | 是 | — | — |
| `ReviewStateAt` | `review_state_at` | json | `int64` | 是 | — | — |
| `RetentionState` | `retention_state` | json | `int32` | 是 | — | — |
| `PublishedAt` | `published_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveRoomInfo`

> LiveRoomInfo 房间业务状态投影（运营面全字段，含 state_version 与 reject_reason）。 / state：1 待完善、2 可开播、3 直播中、4 已关闭、5 违规禁播、6 停用； / verify_state：1 未提交、2 审核中、3 通过、4 驳回。

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
| `BanUntil` | `ban_until` | json | `int64` | 是 | — | 0 表示无禁播或永久禁播 |
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

> LiveSessionInfo 场次投影。运营面保留终端面裁掉的内部字段：last_stream_seq 是流事件 / 乱序守卫、record_id 是 live-media 录制记录引用、moderation_task_id 是开播送审关联—— / 排障与审计都要看，但它们只在 live-room 自己的状态机里有意义，网关不解释。

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
| `EndReason` | `end_reason` | json | `int32` | 是 | — | 1 主播下播、2 禁播、3 房间关闭、4 断流超时、5 更晚停止事件补偿 |
| `LastStreamSeq` | `last_stream_seq` | json | `int64` | 是 | — | — |
| `ReplayState` | `replay_state` | json | `int32` | 是 | — | 1 无、2 转码中、3 可回放、4 已下架 |
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `RecordAssetId` | `record_asset_id` | json | `int64` | 是 | — | — |
| `RecordAid` | `record_aid` | json | `int64` | 是 | — | — |
| `ModerationTaskId` | `moderation_task_id` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveRoomBanInfo`

> LiveRoomBanInfo 禁播台账。reason / lift_reason 是运营内部说明（proto 明确不下发终端）， / 所以只出现在本运营面。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BanId` | `ban_id` | json | `int64` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `BanType` | `ban_type` | json | `int32` | 是 | — | 1 临时、2 永久 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | 0 表示永久 |
| `State` | `state` | json | `int32` | 是 | — | 1 生效、2 已解除、3 已过期 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `LiftOperator` | `lift_operator_mid` | json | `int64` | 是 | — | — |
| `LiftReason` | `lift_reason` | json | `string` | 是 | — | — |
| `LiftedAt` | `lifted_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `LiveAreaInfo`

> LiveAreaInfo 分区投影。运营面带 operator_mid/ctime/mtime（终端面裁掉的就是这三个）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AreaId` | `area_id` | json | `int64` | 是 | — | — |
| `AreaName` | `area_name` | json | `string` | 是 | — | — |
| `ParentAreaId` | `parent_area_id` | json | `int64` | 是 | — | — |
| `Sort` | `sort` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 启用、0 停用 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveAnchorInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Role` | `role` | json | `int32` | 是 | — | 1 房主、2 联合主播、3 房管 |
| `State` | `state` | json | `int32` | 是 | — | 1 生效、0 已解绑 |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveStreamInfo`

> LiveStreamInfo 流状态与累计指标（live_stream 行投影）。 / health_state 在 proto 里就是 int32（不是枚举），网关按值投影不二次解释； / fps 是 ×100 后的整数、packet_loss_ppm 是百万分比，口径由 live-ingest 定义。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `StreamId` | `stream_id` | json | `string` | 是 | — | — |
| `KeyId` | `key_id` | json | `int64` | 是 | — | — |
| `StreamName` | `stream_name` | json | `string` | 是 | — | 流标识，非密钥 |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `session_id` | json | `int64` | 是 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 是 | — | — |
| `Protocol` | `protocol` | json | `int32` | 是 | — | 1 RTMP、2 SRT、3 WebRTC |
| `NodeId` | `node_id` | json | `string` | 是 | — | 空串表示未分配接入节点 |
| `State` | `state` | json | `int32` | 是 | — | 1 已建档、2 推流中、3 断流、4 已停止 |
| `Seq` | `seq` | json | `int64` | 是 | — | live.state.v1 的当前事件序号 |
| `PublishStartedAt` | `publish_started_at` | json | `int64` | 是 | — | — |
| `StateChangedAt` | `state_changed_at` | json | `int64` | 是 | — | — |
| `LastHeartbeatAt` | `last_heartbeat_at` | json | `int64` | 是 | — | — |
| `InterruptedTotalSeconds` | `interrupted_total_seconds` | json | `int64` | 是 | — | — |
| `InterruptionCount` | `interruption_count` | json | `int32` | 是 | — | — |
| `StopReason` | `stop_reason` | json | `int32` | 是 | — | 非终态为 0 |
| `HealthState` | `health_state` | json | `int32` | 是 | — | — |
| `HealthReportedAt` | `health_reported_at` | json | `int64` | 是 | — | — |
| `VideoBitrateBps` | `video_bitrate_bps` | json | `int64` | 是 | — | — |
| `AudioBitrateBps` | `audio_bitrate_bps` | json | `int64` | 是 | — | — |
| `Fps` | `fps` | json | `int32` | 是 | — | — |
| `PacketLossPpm` | `packet_loss_ppm` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `StreamKeyInfo`

> StreamKeyInfo 推流密钥元数据。契约保证**永不含明文与哈希**：key_hint_tail 是明文末 4 位 / （只用于主播在多个密钥之间辨认，不可用于鉴权），key_ref 是 Secret/Vault 引用。 / 因此本投影可以安全出现在后台页面；明文只在 Issue/Rotate 的响应里存在一次，那两条路由不开面。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `KeyId` | `key_id` | json | `int64` | 是 | — | — |
| `StreamName` | `stream_name` | json | `string` | 是 | — | — |
| `KeyHintTail` | `key_hint_tail` | json | `string` | 是 | — | — |
| `KeyRef` | `key_ref` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 生效、2 轮转中、3 已退役、4 已过期、5 已吊销 |
| `Version` | `version` | json | `int32` | 是 | — | — |
| `PrevKeyId` | `prev_key_id` | json | `int64` | 是 | — | — |
| `Protocols` | `protocols` | json | `[]int32` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `session_id` | json | `int64` | 是 | — | — |
| `AnchorMid` | `anchor_mid` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `GraceUntil` | `grace_until` | json | `int64` | 是 | — | 0 表示不适用 |
| `CurrentStreamId` | `current_stream_id` | json | `string` | 是 | — | — |
| `RotateToKeyId` | `rotate_to_key_id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveHealthSample`

> LiveHealthSample 健康采样点。fps_x100 与 packet_loss_ppm 沿用 proto 口径（×100 / 百万分比）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OccurredAt` | `occurred_at` | json | `int64` | 是 | — | — |
| `VideoBitrateBps` | `video_bitrate_bps` | json | `int64` | 是 | — | — |
| `AudioBitrateBps` | `audio_bitrate_bps` | json | `int64` | 是 | — | — |
| `FpsX100` | `fps_x100` | json | `int32` | 是 | — | — |
| `PacketLossPpm` | `packet_loss_ppm` | json | `int32` | 是 | — | — |
| `RttMs` | `rtt_ms` | json | `int64` | 是 | — | — |

### `IngestNodeInfo`

> IngestNodeInfo 接入节点投影。三个 endpoint 是接入地址（proto 注明不含密钥，可下发客户端）， / 不是推流地址；active_streams / last_heartbeat_at 是节点侧派生值，运营面只读、不能声明。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `NodeId` | `node_id` | json | `string` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Region` | `region` | json | `string` | 是 | — | — |
| `Protocols` | `protocols` | json | `[]int32` | 是 | — | — |
| `EndpointRtmp` | `endpoint_rtmp` | json | `string` | 是 | — | — |
| `EndpointSrt` | `endpoint_srt` | json | `string` | 是 | — | — |
| `EndpointWebrtc` | `endpoint_webrtc` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 在线、2 摘流中、3 离线 |
| `CapacityStreams` | `capacity_streams` | json | `int32` | 是 | — | — |
| `ActiveStreams` | `active_streams` | json | `int32` | 是 | — | — |
| `HealthScore` | `health_score` | json | `int32` | 是 | — | — |
| `LastHeartbeatAt` | `last_heartbeat_at` | json | `int64` | 是 | — | — |
| `Labels` | `labels` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `NodeAssignmentInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AssignmentId` | `assignment_id` | json | `int64` | 是 | — | — |
| `StreamId` | `stream_id` | json | `string` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `NodeId` | `node_id` | json | `string` | 是 | — | — |
| `Protocol` | `protocol` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 生效、2 已释放、3 已迁移 |
| `Score` | `score` | json | `int32` | 是 | — | — |
| `PrevNodeId` | `prev_node_id` | json | `string` | 是 | — | — |
| `AssignedAt` | `assigned_at` | json | `int64` | 是 | — | — |
| `ReleasedAt` | `released_at` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |

### `StreamInterruptionInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `InterruptionId` | `interruption_id` | json | `int64` | 是 | — | — |
| `StreamId` | `stream_id` | json | `string` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `EpisodeNo` | `episode_no` | json | `int32` | 是 | — | 该流第几次断流，从 1 递增 |
| `NodeId` | `node_id` | json | `string` | 是 | — | — |
| `StartedAt` | `started_at` | json | `int64` | 是 | — | — |
| `EndedAt` | `ended_at` | json | `int64` | 是 | — | 0 表示仍在中断中 |
| `DurationSeconds` | `duration_seconds` | json | `int64` | 是 | — | — |
| `EndReason` | `end_reason` | json | `int32` | 是 | — | 1 重连成功、2 断流超时、3 主动停流 |
| `ReconnectAttempts` | `reconnect_attempts` | json | `int32` | 是 | — | — |
| `StartEventId` | `start_event_id` | json | `string` | 是 | — | — |
| `EndEventId` | `end_event_id` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `StreamEventInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventId` | `event_id` | json | `string` | 是 | — | 消费方去重锚点 |
| `StreamId` | `stream_id` | json | `string` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `SessionId` | `session_id` | json | `int64` | 是 | — | — |
| `Seq` | `seq` | json | `int64` | 是 | — | — |
| `FromState` | `from_state` | json | `int32` | 是 | — | — |
| `ToState` | `to_state` | json | `int32` | 是 | — | — |
| `NodeId` | `node_id` | json | `string` | 是 | — | — |
| `InterruptionId` | `interruption_id` | json | `int64` | 是 | — | — |
| `InterruptedSeconds` | `interrupted_seconds` | json | `int32` | 是 | — | — |
| `StopReason` | `stop_reason` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | 原因摘要（契约保证不含明文密钥） |
| `OccurredAt` | `occurred_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `LiveConnectionLease`

> LiveConnectionLease 连接租约投影（Redis 视图，非 MySQL 台账）。 / **刻意不投影 reconnect_ticket**：proto 注释写明「返回体即客户端重连凭据」，票据能一次性换取 / 该 (mid, room) 的新租约——把它回显到后台页面等于让控制台成为凭据通道，与不接 / RotateStreamKey 同一条理由。lease_id 保留：运营踢人要用它定位单条连接， / 而 KickConnection 仍需服务端校验三元组，泄露一个 ID 不构成会话接管。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `LeaseId` | `lease_id` | json | `string` | 是 | — | — |
| `ConnId` | `conn_id` | json | `string` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | 0 表示游客 |
| `Role` | `role` | json | `int32` | 是 | — | 服务判定结果，不是客户端自报值 |
| `NodeId` | `node_id` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 有效、2 已过期、3 已释放、4 被强制下线 |
| `IssuedAt` | `issued_at` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `TtlSeconds` | `ttl_seconds` | json | `int32` | 是 | — | — |
| `RenewCount` | `renew_count` | json | `int64` | 是 | — | — |
| `LastHeartbeatAt` | `last_heartbeat_at` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |

### `RoomRouteInfo`

> RoomRouteInfo 房间路由投影。version 是乐观并发版本：排空前必须回读， / 后台不能凭「我记得它是 3」去改路由，网关也不代为递增或忽略。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `NodeId` | `node_id` | json | `string` | 是 | — | — |
| `ReplicaNodes` | `replica_nodes` | json | `[]string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `ShardCount` | `shard_count` | json | `int32` | 是 | — | — |
| `ServingConnections` | `serving_connections` | json | `int32` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `UpdatedAt` | `updated_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `BroadcastLogInfo`

> BroadcastLogInfo 广播审计流水。只存 payload_digest 与字节数，**没有正文字段** / （弹幕/私信正文不落 live-gateway），后台因此看不到消息内容——这是契约自带的隐私边界。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `MessageId` | `message_id` | json | `string` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `Kind` | `kind` | json | `int32` | 是 | — | — |
| `SenderMid` | `sender_mid` | json | `int64` | 是 | — | — |
| `SenderRole` | `sender_role` | json | `int32` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `PayloadDigest` | `payload_digest` | json | `string` | 是 | — | — |
| `PayloadBytes` | `payload_bytes` | json | `int32` | 是 | — | — |
| `FanoutNodes` | `fanout_nodes` | json | `int32` | 是 | — | — |
| `TargetedConnections` | `targeted_connections` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 已下发、2 已丢弃、3 越权拒绝、4 重复丢弃 |
| `DropReason` | `drop_reason` | json | `int32` | 是 | — | — |
| `SourceService` | `source_service` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `AccessQuotaInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | json | `int32` | 是 | — | — |
| `ScopeId` | `scope_id` | json | `int64` | 是 | — | — |
| `ScopeKey` | `scope_key` | json | `string` | 是 | — | — |
| `MaxConnections` | `max_connections` | json | `int32` | 是 | — | — |
| `BroadcastQps` | `broadcast_qps` | json | `int32` | 是 | — | — |
| `DanmakuQps` | `danmaku_qps` | json | `int32` | 是 | — | — |
| `LeaseTtlSeconds` | `lease_ttl_seconds` | json | `int32` | 是 | — | — |
| `TicketTtlSeconds` | `ticket_ttl_seconds` | json | `int32` | 是 | — | — |
| `MaxPayloadBytes` | `max_payload_bytes` | json | `int32` | 是 | — | — |
| `AllowGuest` | `allow_guest` | json | `bool` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `UpdatedBy` | `updated_by` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `LiveMediaRecordSegmentInfo`

> LiveMediaRecordSegmentInfo 录制切片（live_record_segment 投影）。 / state=4 MISSING / 5 CORRUPT 是时间轴上的洞与坏片，回放拼接必须看得见； / checksum 是内容摘要（sha256 hex），契约注明它不是凭据，可以回后台。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `RecordId` | `record_id` | json | `int64` | 是 | — | — |
| `RoomId` | `room_id` | json | `int64` | 是 | — | — |
| `LiveSessionId` | `live_session_id` | json | `int64` | 是 | — | — |
| `Seq` | `seq` | json | `int64` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `DurationMs` | `duration_ms` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Bucket` | `bucket` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |
| `SizeBytes` | `size_bytes` | json | `int64` | 是 | — | — |
| `Checksum` | `checksum` | json | `string` | 是 | — | — |
| `WorkerId` | `worker_id` | json | `string` | 是 | — | — |
| `RegisteredAt` | `registered_at` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/21-admin-live.md -->
