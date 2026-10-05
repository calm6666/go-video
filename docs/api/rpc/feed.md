# RPC · `feed`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/feed/rpc/feed.proto` |
| protobuf 包 | `feed.v1` |
| go_package | `go-video/services/feed/rpc` |
| 发现用的 etcd key | `feed.v1.rpc`（`services/feed/etc/feed.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`feed.v1.rpc`） |
| 监听 | `8092`（`services/feed/etc/feed.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_feed` |
| 方法数 | 8（service `Feed`） |
| 网关消费方 | `app:FeedRPC` |

## 契约说明

> 说明：本契约覆盖动态与关注流领域：
>   - 写扩散：PushFeed 时把新动态 fan-out 到所有粉丝的 Redis ZSet 收件箱。
>   - 读关注流：PullFeed 走 ZREVRANGEBYSCORE 翻页。
>   - 个人主页：ListUserFeed 从作者个人发件箱 ZSet 翻页。
>   - 置顶、未读、删除均为领域动作。
> 依据 AGENTS.md §1，不实现投币、广告或任何商业化；
> 依据 §5，本服务只持有动态投影、收件箱和未读计数，
> 不复制视频主数据，不直接更新推荐结果，不直连 social-graph 的 MySQL。

## service `Feed`

> Feed 动态与关注流服务。 / 依据 AGENTS.md §1，不实现投币、广告或商业化； / 依据 §5，本服务只持有动态投影、收件箱和未读计数， / 不复制视频主数据，不直接更新推荐结果。

gRPC 方法前缀：`feed.v1.Feed/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `PushFeed` | [`PushFeedReq`](#message-pushfeedreq) | [`EmptyReply`](#message-emptyreply) | 领域服务推送新动态（写扩散：fan-out 到所有粉丝收件箱） |
| 2 | `PullFeed` | [`PullFeedReq`](#message-pullfeedreq) | [`FeedReply`](#message-feedreply) | 拉取关注流（cursor 翻页） |
| 3 | `ListUserFeed` | [`ListUserFeedReq`](#message-listuserfeedreq) | [`FeedReply`](#message-feedreply) | 查询某用户主页动态 |
| 4 | `PinFeed` | [`PinFeedReq`](#message-pinfeedreq) | [`EmptyReply`](#message-emptyreply) | 置顶动态 |
| 5 | `UnpinFeed` | [`UnpinFeedReq`](#message-unpinfeedreq) | [`EmptyReply`](#message-emptyreply) | 取消置顶 |
| 6 | `GetUnreadCount` | [`MidReq`](#message-midreq) | [`UnreadReply`](#message-unreadreply) | 查询用户未读动态数 |
| 7 | `ClearUnread` | [`MidReq`](#message-midreq) | [`EmptyReply`](#message-emptyreply) | 清零未读计数 |
| 8 | `DeleteFeed` | [`DeleteFeedReq`](#message-deletefeedreq) | [`EmptyReply`](#message-emptyreply) | 删除自己的动态 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `OType`

> 对象类型

| 值 | 编号 | 说明 |
|---|---|---|
| `OTYPE_UNSPECIFIED` | 0 | 未指定 |
| `OTYPE_UGC_VIDEO` | 1 | UGC 视频稿件 |
| `OTYPE_PGC_WORK` | 2 | PGC 番剧/影视/纪录片 |
| `OTYPE_LIVE` | 3 | 直播间开播 |
| `OTYPE_ARTICLE` | 4 | 专栏 |

### enum `Action`

> 动作类型

| 值 | 编号 | 说明 |
|---|---|---|
| `ACTION_UNSPECIFIED` | 0 | 未指定 |
| `ACTION_PUBLISH` | 1 | 发布 |
| `ACTION_FORWARD` | 2 | 转发 |
| `ACTION_UPDATE` | 3 | 修改 |

### message `FeedItem`

> 动态条目

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | 动态 ID（与 feed_outbox.id 一致） |
| `mid` | `int64` | 2 | — | 发布者 ID |
| `oid` | `int64` | 3 | — | 对象 ID（如视频稿件 ID） |
| `otype` | [`OType`](#enum-otype) | 4 | — | 对象类型 |
| `action` | [`Action`](#enum-action) | 5 | — | 动作类型 |
| `ctime` | `int64` | 6 | — | 发布时间（Unix 秒） |
| `title` | `string` | 7 | — | 标题 |
| `cover` | `string` | 8 | — | 封面 URL |
| `uri` | `string` | 9 | — | 跳转 URI |
| `forward_id` | `int64` | 10 | — | 转发的源动态 ID（0 表示原创） |

### message `MidReq`

> 单个 mid 请求（用于未读相关接口）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `real_ip` | `string` | 2 | — | 请求来源 IP |

### message `PushFeedReq`

> PushFeed：领域服务（video/catalog/live-room）推送新动态入收件箱

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 发布者 ID |
| `oid` | `int64` | 2 | — | 对象 ID |
| `otype` | [`OType`](#enum-otype) | 3 | — | 对象类型 |
| `action` | [`Action`](#enum-action) | 4 | — | 动作类型 |
| `ctime` | `int64` | 5 | — | 发布时间（Unix 秒；0 表示服务端取当前时间） |
| `title` | `string` | 6 | — | 标题 |
| `cover` | `string` | 7 | — | 封面 URL |
| `uri` | `string` | 8 | — | 跳转 URI |
| `forward_id` | `int64` | 9 | — | 转发的源动态 ID（0 表示原创） |
| `source` | `string` | 10 | — | 来源服务名（如 video/catalog/live-room） |
| `operator` | `string` | 11 | — | 操作者（系统/运营/本人） |
| `real_ip` | `string` | 12 | — | 调用方 IP |

### message `PullFeedReq`

> PullFeed：用户拉取关注流（cursor 翻页）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 当前用户 ID |
| `cursor` | `int64` | 2 | — | 翻页游标（上页最后一条 ctime，0 表示从头开始） |
| `ps` | `int32` | 3 | — | 每页大小（最大 50，默认 20） |
| `real_ip` | `string` | 4 | — | 调用方 IP |

### message `FeedReply`

> 动态列表响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`FeedItem`](#message-feeditem) | 1 | repeated | 动态列表（ctime 倒序） |
| `next_cursor` | `int64` | 2 | — | 下一页游标（0 表示无更多） |
| `has_more` | `bool` | 3 | — | 是否还有更多 |

### message `ListUserFeedReq`

> ListUserFeed：查询某用户主页动态

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `vmid` | `int64` | 1 | — | 被查看用户 ID |
| `mid` | `int64` | 2 | — | 当前用户 ID（用于可见性判断，0 表示未登录） |
| `cursor` | `int64` | 3 | — | 翻页游标 |
| `ps` | `int32` | 4 | — | 每页大小（最大 50，默认 20） |
| `real_ip` | `string` | 5 | — | 调用方 IP |

### message `PinFeedReq`

> PinFeed：置顶动态

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID（动态所属空间） |
| `feed_id` | `int64` | 2 | — | 动态 ID |
| `operator` | `string` | 3 | — | 操作者（本人/运营） |
| `real_ip` | `string` | 4 | — | 调用方 IP |

### message `UnpinFeedReq`

> UnpinFeed：取消置顶

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `feed_id` | `int64` | 2 | — | 动态 ID |
| `operator` | `string` | 3 | — | 操作者 |
| `real_ip` | `string` | 4 | — | 调用方 IP |

### message `UnreadReply`

> 未读计数响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `unread` | `int64` | 1 | — | 未读动态数 |

### message `DeleteFeedReq`

> DeleteFeed：删除自己的动态（本人或运营）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 发布者 ID（用于定位 outbox） |
| `feed_id` | `int64` | 2 | — | 动态 ID |
| `operator` | `string` | 3 | — | 操作者（本人/运营） |
| `real_ip` | `string` | 4 | — | 调用方 IP |
