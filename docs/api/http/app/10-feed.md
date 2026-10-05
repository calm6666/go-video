# 终端面 · `/feed`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| feed 域聚合（services/feed/rpc/feed.proto） | 免鉴权 | 4 |
| feed 增量：动态置顶与删除 | 免鉴权 | 3 |

合计 **7** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## feed 域聚合（services/feed/rpc/feed.proto）（免鉴权，4 条）

> feed 域路由

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/feed/pull` | 拉取关注流（cursor 翻页） | `pullFeed` | `pullfeedlogic.go` |
| GET | `/feed/user/:mid` | 查询某用户主页动态 | `listUserFeed` | `listuserfeedlogic.go` |
| GET | `/feed/unread` | 查询用户未读动态数 | `getUnreadCount` | `getunreadcountlogic.go` |
| POST | `/feed/clear_unread` | 清零未读计数 | `clearUnread` | `clearunreadlogic.go` |

### GET `/feed/pull` — 拉取关注流（cursor 翻页）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/pullfeedhandler.go`
- 业务实现：`gateway/app/internal/logic/pullfeedlogic.go`

请求：`ParamPullFeed`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Cursor` | `cursor` | form | `int64` | 是 | — | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`FeedResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FeedData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/feed/user/:mid` — 查询某用户主页动态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listuserfeedhandler.go`
- 业务实现：`gateway/app/internal/logic/listuserfeedlogic.go`

请求：`ParamUserFeed`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Cursor` | `cursor` | form | `int64` | 是 | — | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Vmid` | `mid` | path | `int64` | 是 | — | — |

响应：`FeedResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FeedData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/feed/unread` — 查询用户未读动态数

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getunreadcounthandler.go`
- 业务实现：`gateway/app/internal/logic/getunreadcountlogic.go`

请求：`ParamFeedMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`FeedUnreadResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FeedUnreadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/feed/clear_unread` — 清零未读计数

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/clearunreadhandler.go`
- 业务实现：`gateway/app/internal/logic/clearunreadlogic.go`

请求：`ParamFeedMid`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## feed 增量：动态置顶与删除（免鉴权，3 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/feed/pin` | 置顶本人动态 | `pinFeed` | `pinfeedlogic.go` |
| POST | `/feed/unpin` | 取消置顶动态 | `unpinFeed` | `unpinfeedlogic.go` |
| POST | `/feed/delete` | 删除本人动态（写扩散撤销由 feed 服务处理） | `deleteFeed` | `deletefeedlogic.go` |

### POST `/feed/pin` — 置顶本人动态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/pinfeedhandler.go`
- 业务实现：`gateway/app/internal/logic/pinfeedlogic.go`

请求：`ParamPinFeed`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `FeedId` | `feed_id` | form | `int64` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/feed/unpin` — 取消置顶动态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/unpinfeedhandler.go`
- 业务实现：`gateway/app/internal/logic/unpinfeedlogic.go`

请求：`ParamPinFeed`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `FeedId` | `feed_id` | form | `int64` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/feed/delete` — 删除本人动态（写扩散撤销由 feed 服务处理）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/deletefeedhandler.go`
- 业务实现：`gateway/app/internal/logic/deletefeedlogic.go`

请求：`ParamDeleteFeed`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `FeedId` | `feed_id` | form | `int64` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamPullFeed`

> feed 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Cursor` | `cursor` | form | `int64` | 是 | — | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `FeedResponse`

> feed 域响应信封

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FeedData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUserFeed`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Vmid` | `mid` | path | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Cursor` | `cursor` | form | `int64` | 是 | — | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamFeedMid`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `FeedUnreadResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FeedUnreadData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPinFeed`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `FeedId` | `feed_id` | form | `int64` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamDeleteFeed`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `FeedId` | `feed_id` | form | `int64` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `FeedData`

> feed 域响应数据载荷

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]FeedItem` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `int64` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |

### `FeedUnreadData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Unread` | `unread` | json | `int64` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `FeedItem`

> 动态条目（对应 feed.FeedItem）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Oid` | `oid` | json | `int64` | 是 | — | — |
| `OType` | `otype` | json | `int32` | 是 | — | — |
| `Action` | `action` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Cover` | `cover` | json | `string` | 是 | — | — |
| `Uri` | `uri` | json | `string` | 是 | — | — |
| `ForwardId` | `forward_id` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/10-feed.md -->
