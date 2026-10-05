# RPC · `inbox`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/inbox/rpc/inbox.proto` |
| protobuf 包 | `inbox.v1` |
| go_package | `go-video/services/inbox/rpc` |
| 发现用的 etcd key | `inbox.v1.rpc`（`services/inbox/etc/inbox.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`inbox.v1.rpc`） |
| 监听 | `8104`（`services/inbox/etc/inbox.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_inbox` |
| 方法数 | 7（service `Inbox`） |
| 网关消费方 | `app:InboxRPC`、`admin:InboxRPC` |

## 契约说明

> 说明：inbox 是站内信（系统消息）领域的所有者服务，只提供 gRPC（无 .api），
> 遵循 AGENTS.md §3/§4：客户端入口在 gateway/app，本服务不返回数据库原始对象。
>
> 数据边界（AGENTS.md §5）：
>   - 本服务只写 go_video_inbox 库自己的表，不读 feed/social-graph/engagement 的表；
>     feed 的“动态未读”与 inbox 的“站内信未读”是两个不同域，各自计数。
>   - 上游互动/发布/开播数据只通过版本化领域事件（internal/consumer）进入本服务。
>   - 外部渠道（Push/短信/邮件）归 notification 服务，本服务只管站内收件箱。

## service `Inbox`

> Inbox 站内信服务。 / 方法名表达领域动作，不暴露数据库 CRUD（docs/api-and-events.md §1.1）。

gRPC 方法前缀：`inbox.v1.Inbox/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `SendSystemMessage` | [`SendSystemMessageReq`](#message-sendsystemmessagereq) | [`SendSystemMessageReply`](#message-sendsystemmessagereply) | 系统/运营向单个或多个用户投递站内信（同事务写主体与收件行）。 |
| 2 | `ListMessages` | [`ListMessagesReq`](#message-listmessagesreq) | [`ListMessagesReply`](#message-listmessagesreply) | 按分类分页拉取收件箱（cursor 优先）。 |
| 3 | `MarkRead` | [`MarkReadReq`](#message-markreadreq) | [`MarkReadReply`](#message-markreadreply) | 幂等标记已读。 |
| 4 | `MarkAllRead` | [`MarkAllReadReq`](#message-markallreadreq) | [`MarkAllReadReply`](#message-markallreadreply) | 幂等把某分类（或全部）标记已读。 |
| 5 | `GetUnreadCount` | [`GetUnreadCountReq`](#message-getunreadcountreq) | [`GetUnreadCountReply`](#message-getunreadcountreply) | 分类未读数（Redis 加速，缺失回落 DB 快照）。 |
| 6 | `RecomputeUnread` | [`RecomputeUnreadReq`](#message-recomputeunreadreq) | [`RecomputeUnreadReply`](#message-recomputeunreadreply) | 从明细表重算未读并修复快照与 Redis。 |
| 7 | `DeleteMessage` | [`DeleteMessageReq`](#message-deletemessagereq) | [`DeleteMessageReply`](#message-deletemessagereply) | 用户侧软删除。 |

## 消息与枚举

### message `EmptyReply`

> 空响应。

（空消息）

### enum `Category`

> 消息分类（与 inbox_message.category 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `CATEGORY_UNSPECIFIED` | 0 | 未指定：查询语境表示“全部分类” |
| `CATEGORY_SYSTEM` | 1 | 系统通知（运营/审核/账号） |
| `CATEGORY_ENGAGEMENT` | 2 | 互动消息（点赞、收藏、关注、分享、@） |
| `CATEGORY_CONTENT` | 3 | 内容消息（投稿发布、版权目录下架） |
| `CATEGORY_LIVE` | 4 | 直播消息（开播、断流、下播） |

### enum `MsgType`

> 消息载体类型。

| 值 | 编号 | 说明 |
|---|---|---|
| `MSG_TYPE_UNSPECIFIED` | 0 | — |
| `MSG_TYPE_TEXT` | 1 | 纯文本 |
| `MSG_TYPE_LINK` | 2 | 文本 + 跳转（跳转信息放 extra JSON） |
| `MSG_TYPE_RICH` | 3 | 结构化富文本（渲染由客户端按 extra 决定） |

### enum `ReadState`

> 收件状态。

| 值 | 编号 | 说明 |
|---|---|---|
| `READ_STATE_UNSPECIFIED` | 0 | — |
| `READ_STATE_UNREAD` | 1 | 未读 |
| `READ_STATE_READ` | 2 | 已读 |

### message `Message`

> Message 是站内信在“某个收件人视角”下的投影（不是数据库行本身）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `msg_id` | `int64` | 1 | — | 消息 ID |
| `category` | [`Category`](#enum-category) | 2 | — | 分类 |
| `msg_type` | [`MsgType`](#enum-msgtype) | 3 | — | 载体类型 |
| `title` | `string` | 4 | — | 标题 |
| `content` | `string` | 5 | — | 正文 |
| `sender_mid` | `int64` | 6 | — | 发送方 mid（0 表示系统） |
| `biz_type` | `string` | 7 | — | 业务类型（submission/comment/live_room/...） |
| `biz_id` | `string` | 8 | — | 业务主键（字符串，跨域 ID 不建外键） |
| `extra` | `string` | 9 | — | 扩展 JSON 文本（客户端渲染所需，不含敏感数据） |
| `ctime` | `int64` | 10 | — | 投递到收件箱的时间（Unix 秒） |
| `read_state` | [`ReadState`](#enum-readstate) | 11 | — | 该收件人的已读状态 |

### message `SendSystemMessageReq`

> --- 发送 --- / SendSystemMessageReq 由 operation/审核/账号等上游给单个或多个用户发站内信。 / 幂等：idempotency_key 必填，重复提交返回首次结果且不重复投递。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mids` | `int64` | 1 | repeated | 接收人列表（单个用户传 1 个元素） |
| `title` | `string` | 2 | — | 标题 |
| `content` | `string` | 3 | — | 正文 |
| `category` | [`Category`](#enum-category) | 4 | — | 分类，未指定按 CATEGORY_SYSTEM 处理 |
| `msg_type` | [`MsgType`](#enum-msgtype) | 5 | — | 载体类型，未指定按 MSG_TYPE_TEXT 处理 |
| `sender_mid` | `int64` | 6 | — | 发送方 mid，0 表示系统账号 |
| `biz_type` | `string` | 7 | — | 业务类型 |
| `biz_id` | `string` | 8 | — | 业务主键 |
| `extra` | `string` | 9 | — | 扩展 JSON 文本 |
| `idempotency_key` | `string` | 10 | — | 幂等键（必填，调用方保证唯一） |
| `operator` | `int64` | 11 | — | 运营管理员 mid，用于审计 |
| `trace_id` | `string` | 12 | — | 透传 trace_id |

### message `SendSystemMessageReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `msg_id` | `int64` | 1 | — | 消息主体 ID |
| `delivered` | `int32` | 2 | — | 本次新投递的收件人数 |
| `deduplicated` | `bool` | 3 | — | true 表示命中幂等键，未重复写入 |
| `ctime` | `int64` | 4 | — | 消息创建时间 |

### message `ListMessagesReq`

> --- 查询 --- / ListMessagesReq 收件箱分页。按 docs/api-and-events.md §2 采用 cursor 优先： / cursor 为空表示第一页；next_cursor 为空表示没有更多数据。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 收件人 |
| `category` | [`Category`](#enum-category) | 2 | — | 分类过滤，CATEGORY_UNSPECIFIED 表示全部 |
| `cursor` | `string` | 3 | — | 上一页 next_cursor |
| `ps` | `int32` | 4 | — | 每页大小，0 表示使用服务端默认值 |
| `unread_only` | `bool` | 5 | — | 只看未读 |

### message `ListMessagesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`Message`](#message-message) | 1 | repeated | 按 ctime 倒序 |
| `next_cursor` | `string` | 2 | — | 空表示到底 |
| `has_more` | `bool` | 3 | — | — |
| `unread_total` | `int64` | 4 | — | 该用户未读总数（含当前分类过滤后的值） |

### message `MarkReadReq`

> --- 已读 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 收件人 |
| `msg_ids` | `int64` | 2 | repeated | 待标记的消息 ID |

### message `MarkReadReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `changed` | `int32` | 1 | — | 实际从未读变为已读的数量（重复调用为 0） |
| `unread_total` | `int64` | 2 | — | 变更后的未读总数 |

### message `MarkAllReadReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 收件人 |
| `category` | [`Category`](#enum-category) | 2 | — | 分类，CATEGORY_UNSPECIFIED 表示全部 |

### message `MarkAllReadReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `changed` | `int32` | 1 | — | — |
| `unread_total` | `int64` | 2 | — | — |

### message `GetUnreadCountReq`

> --- 未读计数 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `force_recompute` | `bool` | 2 | — | true 时忽略 Redis，从 inbox_user_message 重算 |

### message `GetUnreadCountReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int64` | 1 | — | — |
| `by_category` | `map<int32, int64>` | 2 | — | category -> 未读数 |
| `mtime` | `int64` | 3 | — | 计数快照更新时间 |

### message `RecomputeUnreadReq`

> RecomputeUnread 从 inbox_user_message 重算未读并回写 DB 快照与 Redis， / 是计数漂移的唯一修复入口（cron 或运营后台调用）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |

### message `RecomputeUnreadReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int64` | 1 | — | — |
| `by_category` | `map<int32, int64>` | 2 | — | — |

### message `DeleteMessageReq`

> --- 删除 --- / DeleteMessageReq 用户侧软删除：只改本人 inbox_user_message.del_state， / 不影响同一消息其它收件人，也不删除消息主体。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `msg_ids` | `int64` | 2 | repeated | — |

### message `DeleteMessageReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `changed` | `int32` | 1 | — | — |
| `unread_total` | `int64` | 2 | — | — |
