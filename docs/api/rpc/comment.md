# RPC · `comment`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/comment/rpc/comment.proto` |
| protobuf 包 | `comment.v1` |
| go_package | `go-video/services/comment/rpc` |
| 发现用的 etcd key | `comment.v1.rpc`（`services/comment/etc/comment.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`comment.v1.rpc`） |
| 监听 | `8082`（`services/comment/etc/comment.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_comment` |
| 方法数 | 7（service `Comment`） |
| 网关消费方 | `app:CommentRPC`、`admin:CommentRPC` |

## 契约说明

> 说明：参考仓库 openbilibili-go-common 没有独立 comment 服务层
> （评论相关逻辑散落在 interface/reply 与 reply-feed 推送服务中）。
> 本服务依据本项目 README 与 AGENTS.md §5 从头设计评论领域：
> 评论、楼中楼回复、举报、置顶/折叠状态、审核状态。
> 点赞数据归 engagement 服务，本服务仅记录评论自身状态与计数快照。

## service `Comment`

> Comment 视频评论及回复服务。 / 依据 AGENTS.md §5，本服务只持有评论自身状态与计数快照； / 点赞真实计数归 engagement 服务，本服务通过点赞事件异步刷新快照。

gRPC 方法前缀：`comment.v1.Comment/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `PostComment` | [`PostCommentReq`](#message-postcommentreq) | [`PostCommentReply`](#message-postcommentreply) | 发布评论或回复（state 通常为待审核） |
| 2 | `DeleteComment` | [`DeleteCommentReq`](#message-deletecommentreq) | [`EmptyReply`](#message-emptyreply) | 删除评论（本人或管理员） |
| 3 | `ListComments` | [`ListCommentsReq`](#message-listcommentsreq) | [`ListCommentsReply`](#message-listcommentsreply) | 分页查询目标下的根评论 |
| 4 | `ListReplies` | [`ListRepliesReq`](#message-listrepliesreq) | [`ListRepliesReply`](#message-listrepliesreply) | 分页查询某根评论下的楼中楼回复 |
| 5 | `PinComment` | [`PinCommentReq`](#message-pincommentreq) | [`EmptyReply`](#message-emptyreply) | 置顶/取消置顶评论 |
| 6 | `ReportComment` | [`ReportCommentReq`](#message-reportcommentreq) | [`EmptyReply`](#message-emptyreply) | 举报评论（写入 moderation-orchestrator 待审队列） |
| 7 | `CommentStats` | [`CommentStatsReq`](#message-commentstatsreq) | [`CommentStatsReply`](#message-commentstatsreply) | 查询目标下的评论计数快照 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### message `Target`

> 评论目标类型（与 obc reply 兼容：1 视频、4 评论回复、5 图片、6 视频（UGV）、9 番剧、10 番剧评论）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `oid` | `int64` | 1 | — | 目标 ID（视频 aid / 评论 root 等） |
| `tp` | `int32` | 2 | — | 目标类型，参见上文注释 |

### enum `SortMode`

> 评论排序

| 值 | 编号 | 说明 |
|---|---|---|
| `SORT_UNSPECIFIED` | 0 | 未指定（默认按热度） |
| `SORT_HOT` | 1 | 按热度 |
| `SORT_TIME` | 2 | 按时间倒序 |

### enum `CommentState`

> 评论状态

| 值 | 编号 | 说明 |
|---|---|---|
| `STATE_NORMAL` | 0 | 正常 |
| `STATE_FOLDED` | 1 | 折叠（被举报或低质） |
| `STATE_DELETED` | 2 | 已删除 |
| `STATE_PINNED` | 3 | 置顶 |
| `STATE_PENDING` | 4 | 待审核 |
| `STATE_REJECTED` | 5 | 审核驳回 |

### message `CommentInfo`

> 评论主体（DB 行投影）。 / 注：message 名取 CommentInfo 而非 Comment，避免与 service Comment 同名冲突。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rpid` | `int64` | 1 | — | 评论 ID |
| `oid` | `int64` | 2 | — | 目标 ID |
| `tp` | `int32` | 3 | — | 目标类型 |
| `root` | `int64` | 4 | — | 根评论 ID（0 表示本身是根评论） |
| `parent` | `int64` | 5 | — | 父评论 ID（0 表示直接对 oid 评论） |
| `mid` | `int64` | 6 | — | 评论者用户 ID |
| `content` | `string` | 7 | — | 评论内容（明文，富文本由 gateway 渲染） |
| `state` | `int32` | 8 | — | 评论状态，参见 CommentState |
| `ctime` | `int64` | 9 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 10 | — | 修改时间（Unix 秒） |
| `like_count` | `int32` | 11 | — | 点赞数快照（真实计数归 engagement） |
| `reply_count` | `int32` | 12 | — | 回复数快照 |

### message `PostCommentReq`

> --- 发布 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `oid` | `int64` | 1 | — | 目标 ID |
| `tp` | `int32` | 2 | — | 目标类型 |
| `root` | `int64` | 3 | — | 根评论 ID（直接对 oid 评论时为 0） |
| `parent` | `int64` | 4 | — | 父评论 ID（同上） |
| `mid` | `int64` | 5 | — | 评论者用户 ID |
| `content` | `string` | 6 | — | 评论内容 |
| `state` | `int32` | 7 | — | 初始状态（通常为 STATE_PENDING 待审核） |
| `trace_id` | `string` | 8 | — | 调用方透传 trace_id，便于审核链路关联 |

### message `PostCommentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rpid` | `int64` | 1 | — | 新评论 ID |
| `ctime` | `int64` | 2 | — | 创建时间 |

### message `DeleteCommentReq`

> --- 删除 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rpid` | `int64` | 1 | — | 评论 ID |
| `mid` | `int64` | 2 | — | 操作者用户 ID（用于鉴权校验） |
| `admin` | `bool` | 3 | — | 是否管理员操作（管理员可绕过本人校验） |

### message `ListCommentsReq`

> --- 查询 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `oid` | `int64` | 1 | — | 目标 ID |
| `tp` | `int32` | 2 | — | 目标类型 |
| `viewer_mid` | `int64` | 3 | — | 查看者用户 ID（用于过滤黑名单/屏蔽词） |
| `sort` | [`SortMode`](#enum-sortmode) | 4 | — | 排序方式 |
| `pn` | `int32` | 5 | — | 页码（从 1 开始） |
| `ps` | `int32` | 6 | — | 每页大小（最大 49，与 obc 一致） |

### message `ListCommentsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `comments` | [`CommentInfo`](#message-commentinfo) | 1 | repeated | 评论列表 |
| `total` | `int32` | 2 | — | 总数 |

### message `ListRepliesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `root` | `int64` | 1 | — | 根评论 ID |
| `viewer_mid` | `int64` | 2 | — | 查看者用户 ID |
| `pn` | `int32` | 3 | — | 页码 |
| `ps` | `int32` | 4 | — | 每页大小（最大 49） |

### message `ListRepliesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `replies` | [`CommentInfo`](#message-commentinfo) | 1 | repeated | 回复列表 |
| `total` | `int32` | 2 | — | 总数 |

### message `PinCommentReq`

> --- 置顶 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rpid` | `int64` | 1 | — | 评论 ID |
| `oid` | `int64` | 2 | — | 目标 ID（用于校验） |
| `pin` | `bool` | 3 | — | true 置顶、false 取消 |
| `admin_mid` | `int64` | 4 | — | 操作管理员（需为视频 UP 或管理员） |

### message `ReportCommentReq`

> --- 举报 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rpid` | `int64` | 1 | — | 被举报评论 ID |
| `reporter_mid` | `int64` | 2 | — | 举报者用户 ID |
| `reason` | `int32` | 3 | — | 举报原因码 |
| `content` | `string` | 4 | — | 举报理由补充说明 |
| `trace_id` | `string` | 5 | — | 透传 trace_id |

### message `CommentStatsReq`

> --- 计数快照 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `oid` | `int64` | 1 | — | 目标 ID |
| `tp` | `int32` | 2 | — | 目标类型 |

### message `CommentStatsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int64` | 1 | — | 目标下评论总数（含回复） |
| `root_total` | `int64` | 2 | — | 根评论数 |
