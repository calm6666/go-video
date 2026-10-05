# 终端面 · `/inbox`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| inbox 域聚合（services/inbox/rpc/inbox.proto） | 免鉴权 | 5 |

合计 **5** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## inbox 域聚合（services/inbox/rpc/inbox.proto）（免鉴权，5 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/inbox/messages` | 收件箱分页（cursor 优先） | `listInboxMessages` | `listinboxmessageslogic.go` |
| POST | `/inbox/read` | 幂等标记已读 | `markInboxRead` | `markinboxreadlogic.go` |
| POST | `/inbox/read/all` | 按分类全部标记已读 | `markInboxAllRead` | `markinboxallreadlogic.go` |
| GET | `/inbox/unread` | 未读总数与分类未读 | `inboxUnread` | `inboxunreadlogic.go` |
| POST | `/inbox/delete` | 用户侧软删除站内信（只影响本人收件箱） | `deleteInboxMessage` | `deleteinboxmessagelogic.go` |

### GET `/inbox/messages` — 收件箱分页（cursor 优先）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listinboxmessageshandler.go`
- 业务实现：`gateway/app/internal/logic/listinboxmessageslogic.go`

请求：`ParamInboxMessages`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Category` | `category` | form | `int32` | 否 | — | 0 全部分类 |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `UnreadOnly` | `unread_only` | form | `bool` | 否 | — | — |

响应：`InboxMessagesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InboxMessagesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/inbox/read` — 幂等标记已读

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/markinboxreadhandler.go`
- 业务实现：`gateway/app/internal/logic/markinboxreadlogic.go`

请求：`ParamInboxMarkRead`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `MsgIds` | `msg_ids` | form | `[]int64` | 是 | split | — |
| `Category` | `category` | form | `int32` | 否 | — | — |

响应：`InboxOpResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InboxOpData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/inbox/read/all` — 按分类全部标记已读

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/markinboxallreadhandler.go`
- 业务实现：`gateway/app/internal/logic/markinboxallreadlogic.go`

请求：`ParamInboxMarkRead`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `MsgIds` | `msg_ids` | form | `[]int64` | 是 | split | — |
| `Category` | `category` | form | `int32` | 否 | — | — |

响应：`InboxOpResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InboxOpData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/inbox/unread` — 未读总数与分类未读

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/inboxunreadhandler.go`
- 业务实现：`gateway/app/internal/logic/inboxunreadlogic.go`

请求：`ParamInboxUnread`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ForceRecompute` | `force_recompute` | form | `bool` | 否 | — | — |

响应：`InboxUnreadResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InboxUnreadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/inbox/delete` — 用户侧软删除站内信（只影响本人收件箱）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/deleteinboxmessagehandler.go`
- 业务实现：`gateway/app/internal/logic/deleteinboxmessagelogic.go`

请求：`ParamInboxDelete`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `MsgIds` | `msg_ids` | form | `[]int64` | 是 | split | — |

响应：`InboxOpResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InboxOpData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamInboxMessages`

> 站内信收件箱：列表/已读/未读数/软删除均为"本人视角"操作，网关只透传 mid。 / 投递（SendSystemMessage）与未读重算是运营/系统入口，不在终端网关暴露。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Category` | `category` | form | `int32` | 否 | — | 0 全部分类 |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `UnreadOnly` | `unread_only` | form | `bool` | 否 | — | — |

### `InboxMessagesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InboxMessagesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamInboxMarkRead`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `MsgIds` | `msg_ids` | form | `[]int64` | 是 | split | — |
| `Category` | `category` | form | `int32` | 否 | — | — |

### `InboxOpResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InboxOpData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamInboxUnread`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ForceRecompute` | `force_recompute` | form | `bool` | 否 | — | — |

### `InboxUnreadResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InboxUnreadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamInboxDelete`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `MsgIds` | `msg_ids` | form | `[]int64` | 是 | split | — |

### `InboxMessagesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]InboxMessage` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `UnreadTotal` | `unread_total` | json | `int64` | 是 | — | — |

### `InboxOpData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Changed` | `changed` | json | `int32` | 是 | — | — |
| `UnreadTotal` | `unread_total` | json | `int64` | 是 | — | — |

### `InboxUnreadData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `ByCategory` | `by_category` | json | `[]InboxCategoryUnread` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `InboxMessage`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MsgId` | `msg_id` | json | `int64` | 是 | — | — |
| `Category` | `category` | json | `int32` | 是 | — | — |
| `MsgType` | `msg_type` | json | `int32` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Content` | `content` | json | `string` | 是 | — | — |
| `SenderMid` | `sender_mid` | json | `int64` | 是 | — | — |
| `BizType` | `biz_type` | json | `string` | 是 | — | — |
| `BizId` | `biz_id` | json | `string` | 是 | — | — |
| `Extra` | `extra` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `ReadState` | `read_state` | json | `int32` | 是 | — | — |

### `InboxCategoryUnread`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Category` | `category` | json | `int32` | 是 | — | — |
| `Count` | `count` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/17-inbox.md -->
