# RPC · `private-message`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/private-message/rpc/privatemessage.proto` |
| protobuf 包 | `privatemessage.v1` |
| go_package | `go-video/services/private-message/rpc` |
| 发现用的 etcd key | `privatemessage.v1.rpc`（`services/private-message/etc/privatemessage.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`privatemessage.v1.rpc`） |
| 监听 | `8150`（`services/private-message/etc/privatemessage.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_private_message` |
| 方法数 | 15（service `PrivateMessage`） |
| 网关消费方 | `app:PrivateMessageRPC`、`admin:PrivateMessageRPC` |

## 契约说明

> 说明：private-message 是用户私信（单聊）领域的数据所有者，只提供 gRPC（无 .api），
> 遵循 AGENTS.md §3/§4：面向终端的 HTTP/WebSocket 入口在 gateway/app，本服务不返回数据库原始对象。
>
> 数据边界（AGENTS.md §5）：
>   - 本服务只写 go_video_private_message 库自身的表；账号资料归 account/user-profile，
>     关注与黑名单归 social-graph，风控名单与处罚归 risk-control，审核结论归 moderation-orchestrator。
>     跨服务只传主键（mid / conversation_id / msg_id / task_id），不建跨库外键。
>   - 私信与站内信（inbox）是两个不同域：会话与消息必须分库分表，禁止与系统消息共表
>     （services/private-message/README.md 约束）。
>
> 隐私与留存（AGENTS.md §6/§9）：
>   - 私信正文属最高敏感级（隐私级别 P4：私密通信内容）。正文以 AES-GCM 信封加密后落库
>     （content_cipher + key_version），密钥在 Secret/Vault，轮换只改 key_version 不改历史行。
>   - 明文只在通过可见性门禁（黑名单/风控/审核状态/撤回状态）后于 RPC 响应中短暂出现，
>     列表与预览一律使用脱敏摘要，日志与事件严禁打印正文（见 SendMessage 的 preview 语义）。
>   - 留存天数由服务配置 PrivateMessage.MessageRetentionDays 决定；到期行由 PurgeExpiredMessages
>     物理清除，但撤回/举报审计记录保留更久（合规证据链优先于正文留存）。
>
> 反骚扰：黑名单、互关门槛与陌生人限制在**查询与发送两条路径上统一过滤**，
> 判定依据来自 social-graph / risk-control 的 RPC，本服务只缓存结果不落对方数据。

## service `PrivateMessage`

> PrivateMessage 私信服务。 / 方法名表达领域动作，不暴露数据库 CRUD（docs/api-and-events.md §1.1）。

gRPC 方法前缀：`privatemessage.v1.PrivateMessage/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `GetOrCreateConversation` | [`GetOrCreateConversationReq`](#message-getorcreateconversationreq) | [`GetOrCreateConversationReply`](#message-getorcreateconversationreply) | 定位或创建单聊会话（pair_key 唯一索引保证幂等）。 |
| 2 | `SendMessage` | [`SendMessageReq`](#message-sendmessagereq) | [`SendMessageReply`](#message-sendmessagereply) | 发送私信（client_msg_id 幂等 + 门禁顺序见 SendMessageReq 注释）。 |
| 3 | `ListConversations` | [`ListConversationsReq`](#message-listconversationsreq) | [`ListConversationsReply`](#message-listconversationsreply) | 会话列表（cursor 分页，黑名单/风控/隐藏会话在查询层过滤）。 |
| 4 | `ListMessages` | [`ListMessagesReq`](#message-listmessagesreq) | [`ListMessagesReply`](#message-listmessagesreply) | 会话内消息分页（conversation_id + seq 游标，禁止 offset 全表扫）。 |
| 5 | `MarkRead` | [`MarkReadReq`](#message-markreadreq) | [`MarkReadReply`](#message-markreadreply) | 前移已读游标（幂等，只前进不回退）。 |
| 6 | `GetUnreadSummary` | [`GetUnreadSummaryReq`](#message-getunreadsummaryreq) | [`GetUnreadSummaryReply`](#message-getunreadsummaryreply) | 未读汇总（投影，可重算）。 |
| 7 | `WithdrawMessage` | [`WithdrawMessageReq`](#message-withdrawmessagereq) | [`WithdrawMessageReply`](#message-withdrawmessagereply) | 撤回消息（只改可见性标记 + 写审计）。 |
| 8 | `HideConversation` | [`HideConversationReq`](#message-hideconversationreq) | [`EmptyReply`](#message-emptyreply) | 本方隐藏/恢复会话。 |
| 9 | `UpdateUserSetting` | [`UpdateUserSettingReq`](#message-updateusersettingreq) | [`UserSettingInfo`](#message-usersettinginfo) | 更新反骚扰偏好。 |
| 10 | `GetUserSetting` | [`GetUserSettingReq`](#message-getusersettingreq) | [`UserSettingInfo`](#message-usersettinginfo) | 查询反骚扰偏好。 |
| 11 | `ReportMessage` | [`ReportMessageReq`](#message-reportmessagereq) | [`ReportMessageReply`](#message-reportmessagereply) | 举报私信（写本域举报事实并向 moderation 送审）。 |
| 12 | `ListReports` | [`ListReportsReq`](#message-listreportsreq) | [`ListReportsReply`](#message-listreportsreply) | 运营侧举报分页。 |
| 13 | `HandleReport` | [`HandleReportReq`](#message-handlereportreq) | [`HandleReportReply`](#message-handlereportreply) | 运营侧举报处置（幂等键防重复处置）。 |
| 14 | `ApplyModerationVerdict` | [`ApplyModerationVerdictReq`](#message-applymoderationverdictreq) | [`ApplyModerationVerdictReply`](#message-applymoderationverdictreply) | 审核结论回写（唯一写结论入口，按 event_id 去重）。 |
| 15 | `PurgeExpiredMessages` | [`PurgeExpiredMessagesReq`](#message-purgeexpiredmessagesreq) | [`PurgeExpiredMessagesReply`](#message-purgeexpiredmessagesreply) | 留存到期清理（正文物理删除，审计保留）。 |

## 消息与枚举

### message `EmptyReply`

> 空响应。

（空消息）

### enum `MsgType`

> 消息载体类型（与 pm_message.msg_type 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `MSG_TYPE_UNSPECIFIED` | 0 | 未指定：服务端拒绝 |
| `MSG_TYPE_TEXT` | 1 | 纯文本 |
| `MSG_TYPE_IMAGE` | 2 | 图片（只存 asset 主键引用，不存 OSS 地址） |
| `MSG_TYPE_AUDIO` | 3 | 语音（同上） |
| `MSG_TYPE_VIDEO` | 4 | 短视频（同上） |
| `MSG_TYPE_SHARE` | 5 | 稿件/用户/直播间分享卡片（引用放 media_ref，正文放 content） |

### enum `MsgState`

> 消息状态机（与 pm_message.state 一致，取值不可变更）。 / 撤回与审核下架都只改状态位（可见性标记），行与审计记录保留。

| 值 | 编号 | 说明 |
|---|---|---|
| `MSG_STATE_UNSPECIFIED` | 0 | 未指定：服务端拒绝 |
| `MSG_STATE_NORMAL` | 1 | 正常：会话双方可见 |
| `MSG_STATE_PENDING_REVIEW` | 2 | 待审核：仅发送者本人可见（机审结论回写前不外露） |
| `MSG_STATE_WITHDRAWN` | 3 | 已撤回：双方不再展示正文，保留审计 |
| `MSG_STATE_REJECTED` | 4 | 审核驳回：命中违规，双方不可见 |
| `MSG_STATE_DELETED` | 5 | 运营/司法处置删除：保留行与处置记录 |

### enum `ConversationState`

> 会话状态（与 pm_conversation.state 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `CONVERSATION_STATE_UNSPECIFIED` | 0 | 未指定 |
| `CONVERSATION_STATE_NORMAL` | 1 | 正常 |
| `CONVERSATION_STATE_FROZEN` | 2 | 风控冻结：禁止新发送，历史仍可查看 |

### enum `WithdrawSource`

> 撤回来源（与 pm_withdraw_log.source 一致），区分用户自助撤回与平台处置。

| 值 | 编号 | 说明 |
|---|---|---|
| `WITHDRAW_SOURCE_UNSPECIFIED` | 0 | 未指定：服务端拒绝 |
| `WITHDRAW_SOURCE_SENDER` | 1 | 发送者本人限时撤回 |
| `WITHDRAW_SOURCE_RECEIVER` | 2 | 接收方撤回自己的这条消息（仅对双方隐藏） |
| `WITHDRAW_SOURCE_MODERATION` | 3 | 审核结论驱动的系统撤回 |
| `WITHDRAW_SOURCE_ADMIN` | 4 | 运营/管理员处置撤回 |

### enum `ModerationVerdict`

> 审核结论（取值与 moderation.v1.Verdict 一致，只有 moderation-orchestrator 能写入）。

| 值 | 编号 | 说明 |
|---|---|---|
| `VERDICT_UNSPECIFIED` | 0 | 未指定：服务端拒绝 |
| `VERDICT_PASS` | 1 | 通过：PENDING_REVIEW → NORMAL |
| `VERDICT_REVIEW` | 2 | 转人审：保持 PENDING_REVIEW |
| `VERDICT_REJECT` | 3 | 拒绝：PENDING_REVIEW/其他 → REJECTED |

### enum `AllowFrom`

> 接收范围（反骚扰门槛，与 pm_user_setting.allow_from 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `ALLOW_FROM_UNSPECIFIED` | 0 | 未指定：服务端按 default 处理 |
| `ALLOW_FROM_ANYONE` | 1 | 所有人 |
| `ALLOW_FROM_FOLLOWED` | 2 | 仅我关注的人 |
| `ALLOW_FROM_MUTUAL` | 3 | 仅互相关注 |
| `ALLOW_FROM_NONE` | 4 | 关闭私信 |

### enum `ReportState`

> 举报处理状态（与 pm_report.state 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `REPORT_STATE_UNSPECIFIED` | 0 | 未指定 |
| `REPORT_STATE_PENDING` | 1 | 待处理 |
| `REPORT_STATE_HANDLED` | 2 | 已处理（含撤回/处罚） |
| `REPORT_STATE_DISMISSED` | 3 | 已驳回 |

### enum `ReportAction`

> 举报处置动作（HandleReport 使用，需 operator_mid）。

| 值 | 编号 | 说明 |
|---|---|---|
| `REPORT_ACTION_UNSPECIFIED` | 0 | 未指定：服务端拒绝 |
| `REPORT_ACTION_DISMISS` | 1 | 驳回举报 |
| `REPORT_ACTION_WITHDRAW` | 2 | 撤回被举报消息（source=ADMIN） |
| `REPORT_ACTION_PUNISH` | 3 | 转交 risk-control 处罚（本服务只记录已转交） |
| `REPORT_ACTION_ESCALATE` | 4 | 升级送 moderation 人审 |

### message `ConversationInfo`

> ConversationInfo 是会话在“某个用户视角”下的投影（未读数与隐藏状态是用户侧的）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `conversation_id` | `int64` | 1 | — | 会话 ID |
| `peer_mid` | `int64` | 2 | — | 对方 mid（本视角） |
| `state` | `int32` | 3 | — | 会话状态，参见 ConversationState |
| `last_msg_id` | `int64` | 4 | — | 最后一条消息 ID |
| `last_seq` | `int64` | 5 | — | 会话内最后序列号 |
| `last_msg_type` | `int32` | 6 | — | 最后消息载体类型 |
| `last_preview` | `string` | 7 | — | 最后消息脱敏摘要（不含正文原文；撤回为固定文案） |
| `last_msg_time` | `int64` | 8 | — | 最后消息时间（Unix 秒，列表游标） |
| `read_seq` | `int64` | 9 | — | 本方已读游标（本方看到的最大 seq） |
| `unread_count` | `int64` | 10 | — | 未读数（last_seq - read_seq 的可见面估算，投影可重算） |
| `hidden` | `bool` | 11 | — | 本方是否隐藏该会话（不影响对方） |
| `ctime` | `int64` | 12 | — | 会话创建时间（Unix 秒） |

### message `MessageInfo`

> MessageInfo 是单条私信。content 是解密后的明文，只在通过可见性门禁后返回。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `msg_id` | `int64` | 1 | — | 消息 ID |
| `conversation_id` | `int64` | 2 | — | 会话 ID |
| `seq` | `int64` | 3 | — | 会话内单调递增序列号（分页游标） |
| `sender_mid` | `int64` | 4 | — | 发送者 mid |
| `msg_type` | `int32` | 5 | — | 载体类型，参见 MsgType |
| `content` | `string` | 6 | — | 明文正文（撤回/驳回时返回固定占位文案） |
| `media_ref` | `string` | 7 | — | 媒体引用（asset 主键字符串，不含对象存储地址） |
| `state` | `int32` | 8 | — | 消息状态，参见 MsgState |
| `audit_task_id` | `int64` | 9 | — | 机审任务 ID（0 表示未送审） |
| `client_msg_id` | `string` | 10 | — | 客户端消息 ID（幂等回放时回显） |
| `ctime` | `int64` | 11 | — | 发送时间（Unix 秒） |
| `withdraw_time` | `int64` | 12 | — | 撤回时间（0 表示未撤回） |

### message `GetOrCreateConversationReq`

> --- 会话 --- / GetOrCreateConversationReq 按 (mid, peer_mid) 规范化排序定位或创建单聊会话。 / 幂等：pair_key = min:max 的唯一索引，重复调用返回同一 conversation_id。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 发起人（必须是会话双方之一） |
| `peer_mid` | `int64` | 2 | — | 对方 |
| `trace_id` | `string` | 3 | — | 链路追踪 ID |

### message `GetOrCreateConversationReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `conversation_id` | `int64` | 1 | — | 会话 ID |
| `created` | `bool` | 2 | — | true 表示本次新建 |
| `state` | `int32` | 3 | — | 会话状态 |
| `ctime` | `int64` | 4 | — | 创建时间 |

### message `ListConversationsReq`

> ListConversationsReq 会话列表。按 docs/api-and-events.md §2 采用 cursor： / 游标为 (last_msg_time, conversation_id) 的倒序位点，禁止 offset 深翻页。 / 黑名单/风控/已隐藏会话在查询层统一过滤。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 归属用户 |
| `cursor` | `string` | 2 | — | 上一页 next_cursor，空表示第一页 |
| `ps` | `int32` | 3 | — | 每页大小，0 表示服务端默认值 |
| `only_unread` | `bool` | 4 | — | 只看有未读的会话 |
| `include_hidden` | `bool` | 5 | — | 是否包含本方隐藏的会话（默认不含） |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `ListConversationsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`ConversationInfo`](#message-conversationinfo) | 1 | repeated | 按 last_msg_time 倒序 |
| `next_cursor` | `string` | 2 | — | 空表示到底 |
| `has_more` | `bool` | 3 | — | — |
| `unread_total` | `int64` | 4 | — | 该用户未读会话数（过滤后的口径） |

### message `HideConversationReq`

> HideConversationReq 用户侧隐藏会话（只影响本人列表，不删除对方数据）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 操作者 |
| `conversation_id` | `int64` | 2 | — | 会话 ID |
| `hide` | `bool` | 3 | — | true 隐藏、false 恢复 |
| `trace_id` | `string` | 4 | — | 链路追踪 ID |

### message `SendMessageReq`

> --- 发送与分页 --- / SendMessageReq 发送私信。 / 幂等：client_msg_id 必填且按 (sender_mid, client_msg_id) 唯一，重试返回首次结果。 / 门禁顺序：参数校验 → social-graph 黑名单/接收范围 → risk-control CheckAction → / 内容长度与敏感预检 → 落库（PENDING_REVIEW 或 NORMAL）→ 提交机审 → 更新会话游标。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 发送者（gateway 注入，本服务不解析 token） |
| `conversation_id` | `int64` | 2 | — | 目标会话；为 0 时按 peer_mid 建会话 |
| `peer_mid` | `int64` | 3 | — | conversation_id 为 0 时必填 |
| `msg_type` | [`MsgType`](#enum-msgtype) | 4 | — | 载体类型 |
| `content` | `string` | 5 | — | 正文（按 MaxContentRunes 限制，日志禁止打印） |
| `media_ref` | `string` | 6 | — | 媒体引用（asset 主键；IMAGE/AUDIO/VIDEO/SHARE 必填） |
| `client_msg_id` | `string` | 7 | — | 客户端消息 ID（幂等键，必填） |
| `trace_id` | `string` | 8 | — | 链路追踪 ID |

### message `SendMessageReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `msg_id` | `int64` | 1 | — | 消息 ID（幂等回放返回原值） |
| `conversation_id` | `int64` | 2 | — | 会话 ID |
| `seq` | `int64` | 3 | — | 会话内序列号 |
| `state` | `int32` | 4 | — | 落库状态 |
| `ctime` | `int64` | 5 | — | 发送时间 |
| `replayed` | `bool` | 6 | — | true 表示命中 client_msg_id，未产生新消息 |
| `audit_task_id` | `int64` | 7 | — | 机审任务 ID（0 表示未送审） |
| `preview` | `string` | 8 | — | 脱敏摘要（写进会话列表用，非原文） |

### message `ListMessagesReq`

> ListMessagesReq 消息分页：按 conversation_id + seq 游标倒序拉取， / 走 uniq_conv_seq 索引，禁止 offset 全表扫。撤回/驳回的消息按占位文案返回。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `conversation_id` | `int64` | 1 | — | 会话 ID |
| `mid` | `int64` | 2 | — | 查看者（用于黑名单/风控/权限过滤与明文解密授权） |
| `cursor_seq` | `int64` | 3 | — | 上一页 next_cursor_seq，0 表示从最新开始 |
| `ps` | `int32` | 4 | — | 每页大小，0 表示服务端默认值 |
| `trace_id` | `string` | 5 | — | 链路追踪 ID |

### message `ListMessagesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`MessageInfo`](#message-messageinfo) | 1 | repeated | 按 seq 倒序 |
| `next_cursor_seq` | `int64` | 2 | — | 0 表示到底 |
| `has_more` | `bool` | 3 | — | — |
| `read_seq` | `int64` | 4 | — | 查看者当前已读游标 |

### message `MarkReadReq`

> MarkReadReq 前移已读游标。 / 语义取舍：已读是“每会话每成员一行游标”，不是逐条已读状态表。 / 容量：逐条回执的行数级是 会话成员数 × 消息数（百万级消息即亿级行）， / 游标方案固定 2 行/会话，未读数由 last_seq - read_seq 估算 + 可重算，代价是 / 无法回答“某人读过哪一条”，产品只需“读到哪”，因此选游标（README 有说明）。 / 幂等：只允许游标前进，回退请求返回当前值且 changed=false。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `conversation_id` | `int64` | 1 | — | 会话 ID |
| `mid` | `int64` | 2 | — | 读者 |
| `read_seq` | `int64` | 3 | — | 前进到的序列号 |
| `trace_id` | `string` | 4 | — | 链路追踪 ID |

### message `MarkReadReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `read_seq` | `int64` | 1 | — | 生效后的游标 |
| `changed` | `bool` | 2 | — | false 表示重复或回退请求 |
| `unread_count` | `int64` | 3 | — | 该会话剩余未读数 |

### message `GetUnreadSummaryReq`

> GetUnreadSummaryReq 未读汇总（角标用）。计数是投影，可由成员游标重算。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 |
| `force` | `bool` | 2 | — | true 时忽略缓存回源重算 |
| `trace_id` | `string` | 3 | — | 链路追踪 ID |

### message `GetUnreadSummaryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `unread_total` | `int64` | 1 | — | 未读消息总数 |
| `unread_conversations` | `int64` | 2 | — | 有未读的会话数 |
| `computed_at` | `int64` | 3 | — | 计算时间（Unix 秒） |

### message `WithdrawMessageReq`

> --- 撤回 --- / WithdrawMessageReq 撤回消息。只改 pm_message.state 与可见性标记， / 并写 pm_withdraw_log 保留审计证据（谁、何时、以什么理由、哪个来源）。 / 授权：sender 本人在窗口内限时撤回；RECEIVER 只能撤自己收到的那条； / MODERATION/ADMIN 必须带 operator/task_id 且写审计。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `msg_id` | `int64` | 1 | — | 消息 ID |
| `operator_mid` | `int64` | 2 | — | 操作者 mid（本人撤回时为发送者；admin 时为管理员） |
| `source` | [`WithdrawSource`](#enum-withdrawsource) | 3 | — | 撤回来源 |
| `reason` | `string` | 4 | — | 原因（审计用，不返回给客户端） |
| `audit_task_id` | `int64` | 5 | — | source=MODERATION 时对应的审核任务 ID |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `WithdrawMessageReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `msg_id` | `int64` | 1 | — | 消息 ID |
| `state` | `int32` | 2 | — | 撤回后状态 |
| `withdrawn` | `bool` | 3 | — | false 表示已撤回过或不允许撤回 |
| `withdraw_time` | `int64` | 4 | — | 生效时间 |

### message `UserSettingInfo`

> --- 用户偏好（反骚扰） --- / UserSettingInfo 用户侧私信偏好。判定结果缓存由本服务负责，源头在 social-graph/risk-control。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 |
| `allow_from` | [`AllowFrom`](#enum-allowfrom) | 2 | — | 接收范围门槛 |
| `reject_stranger` | `bool` | 3 | — | 拒绝陌生人（未互关）消息 |
| `keyword_filter` | `bool` | 4 | — | 是否启用敏感词/引流词前置过滤 |
| `mute_conversation` | `bool` | 5 | — | 会话免打扰（仅影响推送，不影响入库） |
| `mtime` | `int64` | 6 | — | 最近更新时间 |

### message `UpdateUserSettingReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 |
| `allow_from` | [`AllowFrom`](#enum-allowfrom) | 2 | — | 传 UNSPECIFIED 表示不修改 |
| `reject_stranger` | `bool` | 3 | optional | — |
| `keyword_filter` | `bool` | 4 | optional | — |
| `mute_conversation` | `bool` | 5 | optional | — |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `GetUserSettingReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 |
| `trace_id` | `string` | 2 | — | 链路追踪 ID |

### message `ReportMessageReq`

> --- 举报与审核联动 --- / ReportMessageReq 举报私信：只写本域举报事实并向 moderation 送审， / 不直连 moderation 库（AGENTS.md §5）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `msg_id` | `int64` | 1 | — | 被举报消息 |
| `reporter_mid` | `int64` | 2 | — | 举报者（必须是会话成员） |
| `reason` | `int32` | 3 | — | 举报原因码（稳定枚举，由 gateway/客户端约定） |
| `description` | `string` | 4 | — | 补充说明（不写正文） |
| `trace_id` | `string` | 5 | — | 链路追踪 ID |

### message `ReportMessageReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `report_id` | `int64` | 1 | — | 举报记录 ID |
| `duplicated` | `bool` | 2 | — | true 表示同一举报人对同一消息已举报过 |
| `audit_task_id` | `int64` | 3 | — | 送审任务 ID（0 表示未送审） |

### message `ReportInfo`

> ReportInfo 举报记录（运营/审核侧投影，不返回消息正文）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `report_id` | `int64` | 1 | — | 举报 ID |
| `conversation_id` | `int64` | 2 | — | 会话 ID |
| `msg_id` | `int64` | 3 | — | 消息 ID |
| `reporter_mid` | `int64` | 4 | — | 举报者 |
| `target_mid` | `int64` | 5 | — | 被举报人 |
| `reason` | `int32` | 6 | — | 原因码 |
| `description` | `string` | 7 | — | 补充说明 |
| `state` | `int32` | 8 | — | 处理状态，参见 ReportState |
| `audit_task_id` | `int64` | 9 | — | 审核任务 ID |
| `handler` | `int64` | 10 | — | 处理人（0 表示未处理） |
| `handle_note` | `string` | 11 | — | 处理结论备注 |
| `ctime` | `int64` | 12 | — | 举报时间 |
| `mtime` | `int64` | 13 | — | 最近更新时间 |

### message `ListReportsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | [`ReportState`](#enum-reportstate) | 1 | — | 状态过滤，UNSPECIFIED 表示全部 |
| `target_mid` | `int64` | 2 | — | 按被举报人过滤（0 不限） |
| `cursor` | `string` | 3 | — | 游标（report_id 倒序位点） |
| `ps` | `int32` | 4 | — | 每页大小 |
| `operator_mid` | `int64` | 5 | — | 运营/管理员，必须 > 0 |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `ListReportsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`ReportInfo`](#message-reportinfo) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |

### message `HandleReportReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `report_id` | `int64` | 1 | — | 举报 ID |
| `action` | [`ReportAction`](#enum-reportaction) | 2 | — | 处置动作 |
| `handler` | `int64` | 3 | — | 处理人（运营/管理员），必须 > 0 |
| `note` | `string` | 4 | — | 处置备注（审计） |
| `withdraw_message` | `bool` | 5 | — | 是否同时撤回被举报消息 |
| `idempotency_key` | `string` | 6 | — | 幂等键（重复处置返回首次结果） |
| `trace_id` | `string` | 7 | — | — |

### message `HandleReportReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `report_id` | `int64` | 1 | — | — |
| `state` | `int32` | 2 | — | 处理后状态 |
| `replayed` | `bool` | 3 | — | true 表示命中幂等键 |
| `withdraw_msg_id` | `int64` | 4 | — | 连带撤回的消息（0 表示未撤回） |

### message `ApplyModerationVerdictReq`

> ApplyModerationVerdictReq 是审核结论的唯一写入口（moderation-orchestrator 或 / moderation.result.v1 消费者调用）。业务方不得自行把消息标记为“已审核通过”。 / 按 event_id 去重，重复投递返回 applied=false。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `msg_id` | `int64` | 1 | — | 私信消息 ID |
| `task_id` | `int64` | 2 | — | moderation 任务 ID |
| `verdict` | [`ModerationVerdict`](#enum-moderationverdict) | 3 | — | 结论 |
| `reason` | `string` | 4 | — | 结论原因（命中词/模型分等，脱敏） |
| `operator` | `int64` | 5 | — | 处理人（0 表示机审） |
| `event_id` | `string` | 6 | — | moderation.result.v1 的 event_id，用于去重 |
| `trace_id` | `string` | 7 | — | — |

### message `ApplyModerationVerdictReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `msg_id` | `int64` | 1 | — | — |
| `state` | `int32` | 2 | — | 迁移后的状态 |
| `applied` | `bool` | 3 | — | false 表示重复投递或非法迁移 |
| `message` | `string` | 4 | — | 说明（重复投递/终态/非法迁移） |

### message `PurgeExpiredMessagesReq`

> --- 留存治理 --- / PurgeExpiredMessagesReq 按留存策略物理清除到期正文（保留撤回/举报审计）。 / 由 cron 或服务自检调用；批处理上限与干跑模式避免长事务与主从延迟放大。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `before_time` | `int64` | 1 | — | 清理 ctime 早于该时间的消息，0 表示按配置窗口推算 |
| `batch_limit` | `int32` | 2 | — | 单次清理上限，0 表示服务端默认值 |
| `dry_run` | `bool` | 3 | — | true 只统计不删除 |
| `operator` | `int64` | 4 | — | 触发者（cron 传 0） |
| `trace_id` | `string` | 5 | — | — |

### message `PurgeExpiredMessagesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `expired_before` | `int64` | 1 | — | 本次生效的留存截止点 |
| `scanned` | `int64` | 2 | — | 扫描到的到期消息数 |
| `purged` | `int64` | 3 | — | 实际清除数（dry_run 时为 0） |
| `remaining` | `int64` | 4 | — | 估算剩余到期数 |
