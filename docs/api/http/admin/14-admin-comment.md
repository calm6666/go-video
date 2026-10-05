# 运营面 · `/admin/comment`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| comment 域运营路由（services/comment/rpc/comment.proto） | 免鉴权 | 3 |
| comment 域写入口（受 AdminPermission 保护） | AdminPermission | 2 |

合计 **5** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## comment 域运营路由（services/comment/rpc/comment.proto）（免鉴权，3 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/comment/list` | 分页查询目标下的根评论（含被折叠/待审状态，运营可见全量） | `adminListComments` | `adminlistcommentslogic.go` |
| GET | `/admin/comment/replies` | 分页查询某根评论下的楼中楼 | `adminListCommentReplies` | `adminlistcommentreplieslogic.go` |
| GET | `/admin/comment/stats` | 目标下的评论计数快照 | `adminCommentStats` | `admincommentstatslogic.go` |

### GET `/admin/comment/list` — 分页查询目标下的根评论（含被折叠/待审状态，运营可见全量）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/adminlistcommentshandler.go`
- 业务实现：`gateway/admin/internal/logic/adminlistcommentslogic.go`

请求：`ParamAdminListComments`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Sort` | `sort` | form | `int32` | 否 | — | 0/1 热度、2 时间倒序 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`AdminCommentListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminCommentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/comment/replies` — 分页查询某根评论下的楼中楼

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/adminlistcommentreplieshandler.go`
- 业务实现：`gateway/admin/internal/logic/adminlistcommentreplieslogic.go`

请求：`ParamAdminListCommentReplies`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Root` | `root` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`AdminCommentListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminCommentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/comment/stats` — 目标下的评论计数快照

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/admincommentstatshandler.go`
- 业务实现：`gateway/admin/internal/logic/admincommentstatslogic.go`

请求：`ParamAdminCommentStats`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |

响应：`AdminCommentStatsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminCommentStatsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## comment 域写入口（受 AdminPermission 保护）（AdminPermission，2 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/comment/delete` | 运营删除评论（admin=true，由 comment 服务写审计） | `comment:item` / `delete` | `adminDeleteComment` | `admindeletecommentlogic.go` |
| POST | `/admin/comment/pin` | 运营置顶/取消置顶评论 | `comment:item` / `pin` | `adminPinComment` | `adminpincommentlogic.go` |

### POST `/admin/comment/delete` — 运营删除评论（admin=true，由 comment 服务写审计）

- 权限口径：AdminPermission · 权限点 `comment:item` / `delete`
- goctl 入口：`gateway/admin/internal/handler/admindeletecommenthandler.go`
- 业务实现：`gateway/admin/internal/logic/admindeletecommentlogic.go`

请求：`ParamAdminDeleteComment`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/comment/pin` — 运营置顶/取消置顶评论

- 权限口径：AdminPermission · 权限点 `comment:item` / `pin`
- goctl 入口：`gateway/admin/internal/handler/adminpincommenthandler.go`
- 业务实现：`gateway/admin/internal/logic/adminpincommentlogic.go`

请求：`ParamAdminPinComment`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | json | `int64` | 是 | — | — |
| `Oid` | `oid` | json | `int64` | 是 | — | — |
| `Pin` | `pin` | json | `bool` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamAdminListComments`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Sort` | `sort` | form | `int32` | 否 | — | 0/1 热度、2 时间倒序 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `AdminCommentListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminCommentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAdminListCommentReplies`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Root` | `root` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `ParamAdminCommentStats`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |

### `AdminCommentStatsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminCommentStatsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAdminDeleteComment`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAdminPinComment`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | json | `int64` | 是 | — | — |
| `Oid` | `oid` | json | `int64` | 是 | — | — |
| `Pin` | `pin` | json | `bool` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |

### `AdminCommentListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]AdminCommentItem` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `AdminCommentStatsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `RootTotal` | `root_total` | json | `int64` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `AdminCommentItem`

> 运营侧只做「查、删、置顶、看计数」：删除带 admin=true 由 comment 服务落审计， / 内容判定与折叠仍由 moderation 推进（AGENTS.md §5/§8），后台不直接改状态字段。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | json | `int64` | 是 | — | — |
| `Oid` | `oid` | json | `int64` | 是 | — | — |
| `Tp` | `tp` | json | `int32` | 是 | — | — |
| `Root` | `root` | json | `int64` | 是 | — | — |
| `Parent` | `parent` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Content` | `content` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `LikeCount` | `like_count` | json | `int32` | 是 | — | — |
| `ReplyCount` | `reply_count` | json | `int32` | 是 | — | — |


<!-- file: docs/api/http/admin/14-admin-comment.md -->
