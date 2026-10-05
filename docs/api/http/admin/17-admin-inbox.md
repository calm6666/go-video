# 运营面 · `/admin/inbox`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| inbox 域运营面 | AdminPermission | 2 |

合计 **2** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## inbox 域运营面（AdminPermission，2 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/inbox/message/send` | 下发系统/运营站内信（同事务写主体与收件行，idempotency_key 幂等） | `inbox:message` / `send` | `adminSendInboxMessage` | `adminsendinboxmessagelogic.go` |
| GET | `/admin/inbox/unread/recompute` | 重算某用户未读快照并回填缓存（计数漂移修复工具，幂等） | `inbox:unread` / `recompute` | `adminRecomputeInboxUnread` | `adminrecomputeinboxunreadlogic.go` |

### POST `/admin/inbox/message/send` — 下发系统/运营站内信（同事务写主体与收件行，idempotency_key 幂等）

- 权限口径：AdminPermission · 权限点 `inbox:message` / `send`
- goctl 入口：`gateway/admin/internal/handler/adminsendinboxmessagehandler.go`
- 业务实现：`gateway/admin/internal/logic/adminsendinboxmessagelogic.go`

请求：`ParamAdminSendInboxMessage`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Mids` | `mids` | json | `[]int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Content` | `content` | json | `string` | 是 | — | — |
| `Category` | `category` | json | `int32` | 否 | — | — |
| `MsgType` | `msg_type` | json | `int32` | 否 | — | — |
| `SenderMid` | `sender_mid` | json | `int64` | 否 | — | — |
| `BizType` | `biz_type` | json | `string` | 否 | — | — |
| `BizId` | `biz_id` | json | `string` | 否 | — | — |
| `Extra` | `extra` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`AdminSendInboxMessageResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminSendInboxMessageData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/inbox/unread/recompute` — 重算某用户未读快照并回填缓存（计数漂移修复工具，幂等）

- 权限口径：AdminPermission · 权限点 `inbox:unread` / `recompute`
- goctl 入口：`gateway/admin/internal/handler/adminrecomputeinboxunreadhandler.go`
- 业务实现：`gateway/admin/internal/logic/adminrecomputeinboxunreadlogic.go`

请求：`ParamAdminRecomputeInboxUnread`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`AdminRecomputeInboxUnreadResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminRecomputeInboxUnreadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamAdminSendInboxMessage`

> 系统站内信的人工投递入口：事件驱动的投递走 MQ 消费者，本路由只覆盖运营手动下发的场景。 / 收件箱正文属于用户隐私，后台不开放代读、代改已读、代删，因此这里只有「发」和「修未读」。 / 下发系统站内信。idempotency_key 必填（重试不得重复投递），接收人规模上限由 inbox 服务的 / Inbox.MaxRecipients 判定，网关不另设一套阈值；category/msg_type 传 0 表示用下游默认值。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Mids` | `mids` | json | `[]int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Content` | `content` | json | `string` | 是 | — | — |
| `Category` | `category` | json | `int32` | 否 | — | — |
| `MsgType` | `msg_type` | json | `int32` | 否 | — | — |
| `SenderMid` | `sender_mid` | json | `int64` | 否 | — | — |
| `BizType` | `biz_type` | json | `string` | 否 | — | — |
| `BizId` | `biz_id` | json | `string` | 否 | — | — |
| `Extra` | `extra` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `AdminSendInboxMessageResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminSendInboxMessageData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAdminRecomputeInboxUnread`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `AdminRecomputeInboxUnreadResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminRecomputeInboxUnreadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `AdminSendInboxMessageData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MsgId` | `msg_id` | json | `int64` | 是 | — | — |
| `Delivered` | `delivered` | json | `int32` | 是 | — | — |
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `AdminRecomputeInboxUnreadData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `ByCategory` | `by_category` | json | `[]AdminInboxUnreadItem` | 是 | — | — |

### `AdminInboxUnreadItem`

> 未读修复结果与终端侧 /inbox/unread 保持同一形状（按分类升序数组），便于两端对账。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Category` | `category` | json | `int32` | 是 | — | — |
| `Count` | `count` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/17-admin-inbox.md -->
