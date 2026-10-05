# 终端面 · `/private-message`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| private-message 域聚合（services/private-message/rpc/privatemessage.proto） | 免鉴权 | 11 |

合计 **11** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## private-message 域聚合（services/private-message/rpc/privatemessage.proto）（免鉴权，11 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/private-message/conversations` | 会话列表（cursor 分页，黑名单/风控/隐藏会话在服务侧过滤） | `listPmConversations` | `listpmconversationslogic.go` |
| POST | `/private-message/conversation/get-or-create` | 按对方 mid 定位或创建单聊会话（pair_key 幂等） | `getOrCreatePmConversation` | `getorcreatepmconversationlogic.go` |
| GET | `/private-message/messages` | 会话内消息分页（seq 游标倒序） | `listPmMessages` | `listpmmessageslogic.go` |
| POST | `/private-message/send` | 发送私信（client_msg_id 幂等键透传） | `sendPmMessage` | `sendpmmessagelogic.go` |
| POST | `/private-message/withdraw` | 撤回消息（终端仅自助撤回，服务侧校验窗口与授权） | `withdrawPmMessage` | `withdrawpmmessagelogic.go` |
| POST | `/private-message/conversation/hide` | 隐藏/恢复本方会话（不删除对方数据） | `hidePmConversation` | `hidepmconversationlogic.go` |
| POST | `/private-message/read` | 前移已读游标（幂等，只前进不回退） | `markPmRead` | `markpmreadlogic.go` |
| GET | `/private-message/unread` | 未读汇总（角标用，投影可重算） | `pmUnreadSummary` | `pmunreadsummarylogic.go` |
| POST | `/private-message/report` | 举报私信（写举报事实并向 moderation 送审） | `reportPmMessage` | `reportpmmessagelogic.go` |
| GET | `/private-message/setting` | 查询本人反骚扰偏好 | `getPmSetting` | `getpmsettinglogic.go` |
| POST | `/private-message/setting/update` | 更新本人反骚扰偏好 | `updatePmSetting` | `updatepmsettinglogic.go` |

### GET `/private-message/conversations` — 会话列表（cursor 分页，黑名单/风控/隐藏会话在服务侧过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listpmconversationshandler.go`
- 业务实现：`gateway/app/internal/logic/listpmconversationslogic.go`

请求：`ParamPmConversations`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `Ps` | `ps` | form | `int32` | 否 | — | 0 表示由服务按 PrivateMessage.PageSize 决定 |
| `OnlyUnread` | `only_unread` | form | `bool` | 否 | — | — |
| `IncludeHidden` | `include_hidden` | form | `bool` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmConversationsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmConversationsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/private-message/conversation/get-or-create` — 按对方 mid 定位或创建单聊会话（pair_key 幂等）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getorcreatepmconversationhandler.go`
- 业务实现：`gateway/app/internal/logic/getorcreatepmconversationlogic.go`

请求：`ParamPmConversationGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `PeerMid` | `peer_mid` | form | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmConversationCreateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmConversationCreateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/private-message/messages` — 会话内消息分页（seq 游标倒序）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listpmmessageshandler.go`
- 业务实现：`gateway/app/internal/logic/listpmmessageslogic.go`

请求：`ParamPmMessages`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConversationId` | `conversation_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | 查看者：解密授权与可见性过滤口径 |
| `CursorSeq` | `cursor_seq` | form | `int64` | 否 | — | 0 表示从最新开始 |
| `Ps` | `ps` | form | `int32` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmMessagesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmMessagesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/private-message/send` — 发送私信（client_msg_id 幂等键透传）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/sendpmmessagehandler.go`
- 业务实现：`gateway/app/internal/logic/sendpmmessagelogic.go`

请求：`ParamPmSend`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ConversationId` | `conversation_id` | form | `int64` | 否 | — | — |
| `PeerMid` | `peer_mid` | form | `int64` | 否 | — | — |
| `MsgType` | `msg_type` | form | `int32` | 是 | — | 1 文本、2 图、3 语音、4 短视频、5 分享卡片 |
| `Content` | `content` | form | `string` | 否 | — | — |
| `MediaRef` | `media_ref` | form | `string` | 否 | — | 非文本载体必填，服务侧校验 |
| `ClientMsgId` | `client_msg_id` | form | `string` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmSendResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmSendData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/private-message/withdraw` — 撤回消息（终端仅自助撤回，服务侧校验窗口与授权）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/withdrawpmmessagehandler.go`
- 业务实现：`gateway/app/internal/logic/withdrawpmmessagelogic.go`

请求：`ParamPmWithdraw`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MsgId` | `msg_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `Source` | `source` | form | `int32` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 否 | — | 仅入审计，不回流给对端 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmWithdrawResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmWithdrawData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/private-message/conversation/hide` — 隐藏/恢复本方会话（不删除对方数据）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/hidepmconversationhandler.go`
- 业务实现：`gateway/app/internal/logic/hidepmconversationlogic.go`

请求：`ParamPmHideConversation`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ConversationId` | `conversation_id` | form | `int64` | 是 | — | — |
| `Hide` | `hide` | form | `bool` | 是 | — | true 隐藏、false 恢复；只影响本人列表 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/private-message/read` — 前移已读游标（幂等，只前进不回退）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/markpmreadhandler.go`
- 业务实现：`gateway/app/internal/logic/markpmreadlogic.go`

请求：`ParamPmMarkRead`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConversationId` | `conversation_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ReadSeq` | `read_seq` | form | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmMarkReadResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmMarkReadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/private-message/unread` — 未读汇总（角标用，投影可重算）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/pmunreadsummaryhandler.go`
- 业务实现：`gateway/app/internal/logic/pmunreadsummarylogic.go`

请求：`ParamPmUnread`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Force` | `force` | form | `bool` | 否 | — | true 忽略缓存回源重算 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmUnreadResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmUnreadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/private-message/report` — 举报私信（写举报事实并向 moderation 送审）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/reportpmmessagehandler.go`
- 业务实现：`gateway/app/internal/logic/reportpmmessagelogic.go`

请求：`ParamPmReport`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MsgId` | `msg_id` | form | `int64` | 是 | — | — |
| `ReporterMid` | `reporter_mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `int32` | 是 | — | 稳定原因码，由端与网关约定 |
| `Description` | `description` | form | `string` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmReportResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmReportData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/private-message/setting` — 查询本人反骚扰偏好

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getpmsettinghandler.go`
- 业务实现：`gateway/app/internal/logic/getpmsettinglogic.go`

请求：`ParamPmSetting`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmUserSettingResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmUserSettingData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/private-message/setting/update` — 更新本人反骚扰偏好

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updatepmsettinghandler.go`
- 业务实现：`gateway/app/internal/logic/updatepmsettinglogic.go`

请求：`ParamPmSettingUpdate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `AllowFrom` | `allow_from` | form | `int32` | 否 | — | — |
| `RejectStranger` | `reject_stranger` | form | `int32` | 否 | — | — |
| `KeywordFilter` | `keyword_filter` | form | `int32` | 否 | — | — |
| `MuteConversation` | `mute_conversation` | form | `int32` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`PmUserSettingResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmUserSettingData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamPmConversations`

> 边界（AGENTS.md §5/§8）：网关只做参数校验、上下文透传与 DTO 投影裁剪。 / 反骚扰门禁（黑名单/接收范围/风控）、client_msg_id 幂等、正文 AES-GCM 信封加密、 / 撤回窗口与审核结论全部归 private-message 服务，网关不重复实现也不绕过。 / 下游返回未实现（ErrNotImplemented）或门禁拒绝错误时按现有 logic 口径原样上抛， / 严禁在网关伪造空列表成功——端上会把「拿不到」误显示成「没有消息」。 /  / proto 15 个方法中进入终端入口的是下面 11 个；其余 4 个不属于终端面： /   - ListReports / HandleReport：运营面（gateway/admin） /   - ApplyModerationVerdict：moderation.result.v1 消费者的结论回写入口 /   - PurgeExpiredMessages：留存治理，归 cron /  / 登录要求：本文件既有约定是「mid 为必填 form 参数」（gateway/app 未配置 go-zero jwt 中间件）， / 私信所有路由都要求 mid，未登录请求由 mid 缺失直接被参数校验拒绝。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `Ps` | `ps` | form | `int32` | 否 | — | 0 表示由服务按 PrivateMessage.PageSize 决定 |
| `OnlyUnread` | `only_unread` | form | `bool` | 否 | — | — |
| `IncludeHidden` | `include_hidden` | form | `bool` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmConversationsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmConversationsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmConversationGet`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `PeerMid` | `peer_mid` | form | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmConversationCreateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmConversationCreateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmMessages`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConversationId` | `conversation_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | 查看者：解密授权与可见性过滤口径 |
| `CursorSeq` | `cursor_seq` | form | `int64` | 否 | — | 0 表示从最新开始 |
| `Ps` | `ps` | form | `int32` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmMessagesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmMessagesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmSend`

> ParamPmSend 发送私信。client_msg_id 是幂等键，重试必须复用同一个值； / conversation_id 为 0 时按 peer_mid 建会话，两者都缺由服务拒绝。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ConversationId` | `conversation_id` | form | `int64` | 否 | — | — |
| `PeerMid` | `peer_mid` | form | `int64` | 否 | — | — |
| `MsgType` | `msg_type` | form | `int32` | 是 | — | 1 文本、2 图、3 语音、4 短视频、5 分享卡片 |
| `Content` | `content` | form | `string` | 否 | — | — |
| `MediaRef` | `media_ref` | form | `string` | 否 | — | 非文本载体必填，服务侧校验 |
| `ClientMsgId` | `client_msg_id` | form | `string` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmSendResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmSendData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmWithdraw`

> ParamPmWithdraw 终端只允许自助撤回（source 1 发送者 / 2 接收方）。 / 审核撤回（3）与运营撤回（4）由服务侧内部入口写入，网关入口直接拒绝， / 也不下发 audit_task_id——这是参数口径，真正的授权判定仍在服务侧。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MsgId` | `msg_id` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `Source` | `source` | form | `int32` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 否 | — | 仅入审计，不回流给对端 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmWithdrawResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmWithdrawData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmHideConversation`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ConversationId` | `conversation_id` | form | `int64` | 是 | — | — |
| `Hide` | `hide` | form | `bool` | 是 | — | true 隐藏、false 恢复；只影响本人列表 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmMarkRead`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConversationId` | `conversation_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ReadSeq` | `read_seq` | form | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmMarkReadResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmMarkReadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmUnread`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Force` | `force` | form | `bool` | 否 | — | true 忽略缓存回源重算 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmUnreadResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmUnreadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmReport`

> ParamPmReport 举报私信：只写本域举报事实并向 moderation 送审， / 网关不判断违规、也不代替 moderation 给出结论（AGENTS.md §5）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MsgId` | `msg_id` | form | `int64` | 是 | — | — |
| `ReporterMid` | `reporter_mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `int32` | 是 | — | 稳定原因码，由端与网关约定 |
| `Description` | `description` | form | `string` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmReportResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmReportData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmSetting`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmUserSettingResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PmUserSettingData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPmSettingUpdate`

> ParamPmSettingUpdate 三态口径：0 表示不修改（对应 proto 的 UNSPECIFIED / optional 未设置）， / 1 显式开启、2 显式关闭。bool 无法区分「未传」与「传 false」， / 因此开关字段用 int32 三态下发，避免把客户端没填的字段误写成关闭。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `AllowFrom` | `allow_from` | form | `int32` | 否 | — | — |
| `RejectStranger` | `reject_stranger` | form | `int32` | 否 | — | — |
| `KeywordFilter` | `keyword_filter` | form | `int32` | 否 | — | — |
| `MuteConversation` | `mute_conversation` | form | `int32` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `PmConversationsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]PmConversation` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | 空表示到底 |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `UnreadTotal` | `unread_total` | json | `int64` | 是 | — | — |

### `PmConversationCreateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConversationId` | `conversation_id` | json | `int64` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | true 表示本次新建（pair_key 唯一索引保证幂等） |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `PmMessagesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]PmMessage` | 是 | — | 按 seq 倒序 |
| `NextCursorSeq` | `next_cursor_seq` | json | `int64` | 是 | — | 0 表示到底 |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `ReadSeq` | `read_seq` | json | `int64` | 是 | — | 查看者当前已读游标 |

### `PmSendData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MsgId` | `msg_id` | json | `int64` | 是 | — | 幂等回放返回原值 |
| `ConversationId` | `conversation_id` | json | `int64` | 是 | — | — |
| `Seq` | `seq` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | true 表示命中 client_msg_id，未产生新消息 |
| `AuditTaskId` | `audit_task_id` | json | `int64` | 是 | — | — |
| `Preview` | `preview` | json | `string` | 是 | — | 写会话列表用的脱敏摘要，非原文 |

### `PmWithdrawData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MsgId` | `msg_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Withdrawn` | `withdrawn` | json | `bool` | 是 | — | false 表示已撤回过或不在窗口内 |
| `WithdrawTime` | `withdraw_time` | json | `int64` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `PmMarkReadData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReadSeq` | `read_seq` | json | `int64` | 是 | — | 生效后的游标（只前进不回退） |
| `Changed` | `changed` | json | `bool` | 是 | — | false 表示重复或回退请求 |
| `UnreadCount` | `unread_count` | json | `int64` | 是 | — | — |

### `PmUnreadData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UnreadTotal` | `unread_total` | json | `int64` | 是 | — | — |
| `UnreadConversations` | `unread_conversations` | json | `int64` | 是 | — | — |
| `ComputedAt` | `computed_at` | json | `int64` | 是 | — | — |

### `PmReportData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReportId` | `report_id` | json | `int64` | 是 | — | — |
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | true 表示同一举报人对同一消息已举报过 |
| `AuditTaskId` | `audit_task_id` | json | `int64` | 是 | — | — |

### `PmUserSettingData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Setting` | `setting` | json | `PmUserSetting` | 是 | — | — |

### `PmConversation`

> PmConversation 是会话在「某个用户视角」下的投影（未读数与隐藏状态是用户侧的）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConversationId` | `conversation_id` | json | `int64` | 是 | — | — |
| `PeerMid` | `peer_mid` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 正常、2 风控冻结 |
| `LastMsgId` | `last_msg_id` | json | `int64` | 是 | — | — |
| `LastSeq` | `last_seq` | json | `int64` | 是 | — | — |
| `LastMsgType` | `last_msg_type` | json | `int32` | 是 | — | — |
| `LastPreview` | `last_preview` | json | `string` | 是 | — | 脱敏摘要，恒不等于正文原文 |
| `LastMsgTime` | `last_msg_time` | json | `int64` | 是 | — | — |
| `ReadSeq` | `read_seq` | json | `int64` | 是 | — | — |
| `UnreadCount` | `unread_count` | json | `int64` | 是 | — | — |
| `Hidden` | `hidden` | json | `bool` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `PmMessage`

> PmMessage 单条私信。content 是服务侧通过可见性门禁后解密出的明文； / 撤回/驳回时服务返回固定占位文案，网关不自行替换也不缓存正文。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MsgId` | `msg_id` | json | `int64` | 是 | — | — |
| `ConversationId` | `conversation_id` | json | `int64` | 是 | — | — |
| `Seq` | `seq` | json | `int64` | 是 | — | — |
| `SenderMid` | `sender_mid` | json | `int64` | 是 | — | — |
| `MsgType` | `msg_type` | json | `int32` | 是 | — | — |
| `Content` | `content` | json | `string` | 是 | — | — |
| `MediaRef` | `media_ref` | json | `string` | 是 | — | asset 主键引用，不含对象存储地址 |
| `State` | `state` | json | `int32` | 是 | — | 1 正常、2 待审核、3 已撤回、4 驳回、5 已删除 |
| `AuditTaskId` | `audit_task_id` | json | `int64` | 是 | — | — |
| `ClientMsgId` | `client_msg_id` | json | `string` | 是 | — | 幂等回放时回显 |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `WithdrawTime` | `withdraw_time` | json | `int64` | 是 | — | 0 表示未撤回 |

### `PmUserSetting`

> PmUserSetting 反骚扰偏好。allow_from：1 所有人、2 仅我关注、3 仅互关、4 关闭私信。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `AllowFrom` | `allow_from` | json | `int32` | 是 | — | — |
| `RejectStranger` | `reject_stranger` | json | `bool` | 是 | — | — |
| `KeywordFilter` | `keyword_filter` | json | `bool` | 是 | — | — |
| `MuteConversation` | `mute_conversation` | json | `bool` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/22-private-message.md -->
