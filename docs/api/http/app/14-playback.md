# 终端面 · `/playback`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| playback 域聚合（services/playback/rpc/playback.proto） | 免鉴权 | 3 |

合计 **3** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## playback 域聚合（services/playback/rpc/playback.proto）（免鉴权，3 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/playback/token` | 签发短期防盗链播放地址（request_id 幂等） | `playbackToken` | `playbacktokenlogic.go` |
| POST | `/playback/heartbeat` | 上报播放心跳（进度单调不回退，事件走 Outbox） | `playbackHeartbeat` | `playbackheartbeatlogic.go` |
| GET | `/playback/session` | 查询本人播放会话与续播进度 | `playbackSession` | `playbacksessionlogic.go` |

### POST `/playback/token` — 签发短期防盗链播放地址（request_id 幂等）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/playbacktokenhandler.go`
- 业务实现：`gateway/app/internal/logic/playbacktokenlogic.go`

请求：`ParamPlaybackToken`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ContentType` | `content_type` | form | `int32` | 否 | — | 1=UGC(aid)、2=PGC(epid)，缺省按 UGC |
| `Aid` | `aid` | form | `int64` | 否 | — | — |
| `Epid` | `epid` | form | `int64` | 否 | — | — |
| `Vid` | `vid` | form | `string` | 否 | — | UGC bvid，仅排障与日志关联 |
| `TemplateId` | `template_id` | form | `int64` | 否 | — | 0 表示由网关选可用最高清晰度 |
| `Mid` | `mid` | form | `int64` | 否 | — | 0 游客 |
| `Platform` | `platform` | form | `int32` | 否 | — | 1 android/2 ios/3 harmony/4 desktop |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `Region` | `region` | form | `string` | 否 | — | PGC 必填，用于版权窗口校验 |
| `RequestId` | `request_id` | form | `string` | 否 | — | 幂等键，重试必须复用 |

响应：`PlaybackTokenResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PlaybackTokenData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/playback/heartbeat` — 上报播放心跳（进度单调不回退，事件走 Outbox）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/playbackheartbeathandler.go`
- 业务实现：`gateway/app/internal/logic/playbackheartbeatlogic.go`

请求：`ParamPlaybackHeartbeat`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | form | `string` | 是 | — | — |
| `PositionMs` | `position_ms` | form | `int64` | 是 | — | — |
| `DurationMs` | `duration_ms` | form | `int64` | 否 | — | — |
| `BufferCount` | `buffer_count` | form | `int32` | 否 | — | — |
| `AvgBitrate` | `avg_bitrate` | form | `int64` | 否 | — | — |
| `LastError` | `last_error` | form | `int32` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PlaybackHeartbeatResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PlaybackHeartbeatData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/playback/session` — 查询本人播放会话与续播进度

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/playbacksessionhandler.go`
- 业务实现：`gateway/app/internal/logic/playbacksessionlogic.go`

请求：`ParamPlaybackSession`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | form | `string` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | 必须与会话归属一致，网关做越权保护 |

响应：`PlaybackSessionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PlaybackSessionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamPlaybackToken`

> 网关职责（AGENTS.md §3/§5/§6）：把终端给的 aid/epid 解析成"当前可播放版次的 object_key"， / 再交给 playback 签名。解析链路：UGC 走 video.GetPlayableSource → transcode.ListTasks； / PGC 走 catalog.GetEpisode(asset_id) → transcode.ListTasks。playback 不读媒资表， / 终端也不得拿到对象存储长期密钥；CDN 回源校验（VerifyPlaybackToken）是边缘节点内部调用， / 不在面向终端的网关上开放。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ContentType` | `content_type` | form | `int32` | 否 | — | 1=UGC(aid)、2=PGC(epid)，缺省按 UGC |
| `Aid` | `aid` | form | `int64` | 否 | — | — |
| `Epid` | `epid` | form | `int64` | 否 | — | — |
| `Vid` | `vid` | form | `string` | 否 | — | UGC bvid，仅排障与日志关联 |
| `TemplateId` | `template_id` | form | `int64` | 否 | — | 0 表示由网关选可用最高清晰度 |
| `Mid` | `mid` | form | `int64` | 否 | — | 0 游客 |
| `Platform` | `platform` | form | `int32` | 否 | — | 1 android/2 ios/3 harmony/4 desktop |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `Region` | `region` | form | `string` | 否 | — | PGC 必填，用于版权窗口校验 |
| `RequestId` | `request_id` | form | `string` | 否 | — | 幂等键，重试必须复用 |

### `PlaybackTokenResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PlaybackTokenData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPlaybackHeartbeat`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | form | `string` | 是 | — | — |
| `PositionMs` | `position_ms` | form | `int64` | 是 | — | — |
| `DurationMs` | `duration_ms` | form | `int64` | 否 | — | — |
| `BufferCount` | `buffer_count` | form | `int32` | 否 | — | — |
| `AvgBitrate` | `avg_bitrate` | form | `int64` | 否 | — | — |
| `LastError` | `last_error` | form | `int32` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PlaybackHeartbeatResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PlaybackHeartbeatData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPlaybackSession`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | form | `string` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | 必须与会话归属一致，网关做越权保护 |

### `PlaybackSessionResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PlaybackSessionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `PlaybackTokenData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | json | `string` | 是 | — | — |
| `PlayUrl` | `play_url` | json | `string` | 是 | — | — |
| `AuthKey` | `auth_key` | json | `string` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |
| `KeyId` | `key_id` | json | `string` | 是 | — | — |
| `TemplateId` | `template_id` | json | `int64` | 是 | — | — |
| `Quality` | `quality` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |

### `PlaybackHeartbeatData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `AcceptedAt` | `accepted_at` | json | `int64` | 是 | — | — |
| `MaxPositionMs` | `max_position_ms` | json | `int64` | 是 | — | — |

### `PlaybackSessionData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Session` | `session` | json | `PlaybackSession` | 是 | — | — |
| `Progress` | `progress` | json | `PlaybackProgress` | 是 | — | — |
| `LatestProgress` | `latest_progress` | json | `PlaybackProgress` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |

### `PlaybackSession`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | json | `string` | 是 | — | — |
| `ContentType` | `content_type` | json | `int32` | 是 | — | — |
| `ContentId` | `content_id` | json | `int64` | 是 | — | — |
| `Vid` | `vid` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 是 | — | — |
| `AppVersion` | `app_version` | json | `string` | 是 | — | — |
| `Region` | `region` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |
| `Uri` | `uri` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `PlaybackProgress`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SessionId` | `session_id` | json | `string` | 是 | — | — |
| `ContentType` | `content_type` | json | `int32` | 是 | — | — |
| `ContentId` | `content_id` | json | `int64` | 是 | — | — |
| `Vid` | `vid` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `PositionMs` | `position_ms` | json | `int64` | 是 | — | — |
| `DurationMs` | `duration_ms` | json | `int64` | 是 | — | — |
| `BufferCount` | `buffer_count` | json | `int32` | 是 | — | — |
| `AvgBitrate` | `avg_bitrate` | json | `int64` | 是 | — | — |
| `LastError` | `last_error` | json | `int32` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/14-playback.md -->
