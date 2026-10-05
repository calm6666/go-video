# 终端面 · `/comment`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| comment 域聚合（services/comment/rpc/comment.proto） | 免鉴权 | 7 |

合计 **7** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## comment 域聚合（services/comment/rpc/comment.proto）（免鉴权，7 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/comment/post` | 发布评论或楼中楼回复（一律以待审状态落库） | `postComment` | `postcommentlogic.go` |
| GET | `/comment/list` | 分页查询目标下的根评论 | `listComments` | `listcommentslogic.go` |
| GET | `/comment/replies` | 分页查询某根评论下的楼中楼 | `listCommentReplies` | `listcommentreplieslogic.go` |
| POST | `/comment/delete` | 删除本人评论（管理员删除走 gateway/admin） | `deleteComment` | `deletecommentlogic.go` |
| POST | `/comment/pin` | 稿件 UP 主置顶/取消置顶评论 | `pinComment` | `pincommentlogic.go` |
| POST | `/comment/report` | 举报评论（进入 moderation 待审队列） | `reportComment` | `reportcommentlogic.go` |
| GET | `/comment/stats` | 目标下的评论计数快照 | `commentStats` | `commentstatslogic.go` |

### POST `/comment/post` — 发布评论或楼中楼回复（一律以待审状态落库）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/postcommenthandler.go`
- 业务实现：`gateway/app/internal/logic/postcommentlogic.go`

请求：`ParamPostComment`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Root` | `root` | form | `int64` | 否 | — | — |
| `Parent` | `parent` | form | `int64` | 否 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Content` | `content` | form | `string` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`CommentPostResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CommentPostData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/comment/list` — 分页查询目标下的根评论

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listcommentshandler.go`
- 业务实现：`gateway/app/internal/logic/listcommentslogic.go`

请求：`ParamListComments`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | 查看者，用于服务侧过滤黑名单 |
| `Sort` | `sort` | form | `int32` | 否 | — | 0/1 热度、2 时间倒序 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`CommentListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CommentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/comment/replies` — 分页查询某根评论下的楼中楼

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listcommentreplieshandler.go`
- 业务实现：`gateway/app/internal/logic/listcommentreplieslogic.go`

请求：`ParamListCommentReplies`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Root` | `root` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`CommentListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CommentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/comment/delete` — 删除本人评论（管理员删除走 gateway/admin）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/deletecommenthandler.go`
- 业务实现：`gateway/app/internal/logic/deletecommentlogic.go`

请求：`ParamDeleteComment`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/comment/pin` — 稿件 UP 主置顶/取消置顶评论

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/pincommenthandler.go`
- 业务实现：`gateway/app/internal/logic/pincommentlogic.go`

请求：`ParamPinComment`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Pin` | `pin` | form | `bool` | 是 | — | — |
| `AdminMid` | `admin_mid` | form | `int64` | 是 | — | 稿件 UP 主或管理员，由 comment 服务校验归属 |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/comment/report` — 举报评论（进入 moderation 待审队列）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/reportcommenthandler.go`
- 业务实现：`gateway/app/internal/logic/reportcommentlogic.go`

请求：`ParamReportComment`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | form | `int64` | 是 | — | — |
| `ReporterMid` | `reporter_mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `int32` | 是 | — | — |
| `Content` | `content` | form | `string` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/comment/stats` — 目标下的评论计数快照

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/commentstatshandler.go`
- 业务实现：`gateway/app/internal/logic/commentstatslogic.go`

请求：`ParamCommentStats`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |

响应：`CommentStatsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CommentStatsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamPostComment`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Root` | `root` | form | `int64` | 否 | — | — |
| `Parent` | `parent` | form | `int64` | 否 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Content` | `content` | form | `string` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `CommentPostResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CommentPostData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListComments`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | 查看者，用于服务侧过滤黑名单 |
| `Sort` | `sort` | form | `int32` | 否 | — | 0/1 热度、2 时间倒序 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `CommentListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CommentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListCommentReplies`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Root` | `root` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `ParamDeleteComment`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPinComment`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Pin` | `pin` | form | `bool` | 是 | — | — |
| `AdminMid` | `admin_mid` | form | `int64` | 是 | — | 稿件 UP 主或管理员，由 comment 服务校验归属 |

### `ParamReportComment`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | form | `int64` | 是 | — | — |
| `ReporterMid` | `reporter_mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `int32` | 是 | — | — |
| `Content` | `content` | form | `string` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `ParamCommentStats`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Tp` | `tp` | form | `int32` | 是 | — | — |

### `CommentStatsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CommentStatsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CommentPostData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rpid` | `rpid` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `CommentListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CommentItem` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `CommentStatsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `RootTotal` | `root_total` | json | `int64` | 是 | — | — |

### `CommentItem`

> 评论/楼中楼回复。依据 AGENTS.md §5，评论点赞计数归 engagement，本域只投影 / comment 自己的状态与计数快照；内容判定与折叠由 moderation 经服务侧状态推进， / 网关不做内容审核，只注入 mid 与 trace_id（AGENTS.md §8）。

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


<!-- file: docs/api/http/app/18-comment.md -->
