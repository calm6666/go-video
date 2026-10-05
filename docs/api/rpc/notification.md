# RPC · `notification`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/notification/rpc/notification.proto` |
| protobuf 包 | `notification.v1` |
| go_package | `go-video/services/notification/rpc` |
| 发现用的 etcd key | `notification.v1.rpc`（`services/notification/etc/notification.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`notification.v1.rpc`） |
| 监听 | `8108`（`services/notification/etc/notification.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_notification` |
| 方法数 | 11（service `Notification`） |
| 网关消费方 | `app:NotificationRPC`、`admin:NotificationRPC` |

## 契约说明

> 说明：本文件是 notification 领域对外契约的唯一来源（AGENTS.md §4：.proto 可人工维护）。
> 依据 docs/service-catalog.md，notification 只做“外部通道投递”（Push/短信/邮件），
> 站内信本体归 inbox（两者在 notify 合并域内保持包级边界，AGENTS.md §3/§5）。
> 依据 AGENTS.md §1，本契约不提供营销/广告投放能力，也不定义小程序通道。
> 隐私约束（docs/api-and-events.md §4）：接收人标识只允许 mid 或受控引用 target_ref，
> 明文手机号/邮箱不进入本契约与数据库。

## service `Notification`

> Notification 外部通道投递。 / 只负责 Push/短信/邮件的模板渲染、频控与免打扰校验、供应商投递、退避重试与死信留档； / 站内信写入归 inbox，本服务不提供任何营销/广告投放接口（AGENTS.md §1）。

gRPC 方法前缀：`notification.v1.Notification/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `SendNotification` | [`SendNotificationReq`](#message-sendnotificationreq) | [`SendNotificationReply`](#message-sendnotificationreply) | 投递通知：模板渲染 -> 频次/免打扰校验 -> 落投递任务（biz_key 幂等）-> 按配置同步或异步投递 |
| 2 | `RenderTemplate` | [`RenderTemplateReq`](#message-rendertemplatereq) | [`RenderTemplateReply`](#message-rendertemplatereply) | 仅渲染模板供上游预览，不落库 |
| 3 | `UpsertTemplate` | [`UpsertTemplateReq`](#message-upserttemplatereq) | [`UpsertTemplateReply`](#message-upserttemplatereply) | 新增或更新模板（草稿或直接发布新版本） |
| 4 | `ListTemplates` | [`ListTemplatesReq`](#message-listtemplatesreq) | [`ListTemplatesReply`](#message-listtemplatesreply) | 分页查询模板 |
| 5 | `PublishTemplate` | [`PublishTemplateReq`](#message-publishtemplatereq) | [`PublishTemplateReply`](#message-publishtemplatereply) | 发布指定草稿版本 |
| 6 | `GetDeliveryStatus` | [`GetDeliveryStatusReq`](#message-getdeliverystatusreq) | [`GetDeliveryStatusReply`](#message-getdeliverystatusreply) | 查询单条投递记录（含供应商回执） |
| 7 | `ListDeliveries` | [`ListDeliveriesReq`](#message-listdeliveriesreq) | [`ListDeliveriesReply`](#message-listdeliveriesreply) | 分页查询投递记录 |
| 8 | `ListDeadLetters` | [`ListDeadLettersReq`](#message-listdeadlettersreq) | [`ListDeadLettersReply`](#message-listdeadlettersreply) | 分页查询死信 |
| 9 | `RetryDeadLetter` | [`RetryDeadLetterReq`](#message-retrydeadletterreq) | [`RetryDeadLetterReply`](#message-retrydeadletterreply) | 运营侧重投死信（按 operator 记审计，幂等） |
| 10 | `UpdateDndPreference` | [`UpdateDndPreferenceReq`](#message-updatedndpreferencereq) | [`UpdateDndPreferenceReply`](#message-updatedndpreferencereply) | 更新用户通道偏好与免打扰设置 |
| 11 | `GetDndPreference` | [`GetDndPreferenceReq`](#message-getdndpreferencereq) | [`GetDndPreferenceReply`](#message-getdndpreferencereply) | 查询用户通道偏好与免打扰设置 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `Channel`

> 投递通道（不含小程序，AGENTS.md §6）

| 值 | 编号 | 说明 |
|---|---|---|
| `CHANNEL_UNSPECIFIED` | 0 | 未指定 |
| `CHANNEL_PUSH` | 1 | 应用推送（Android/iOS/HarmonyOS/桌面端） |
| `CHANNEL_SMS` | 2 | 短信 |
| `CHANNEL_EMAIL` | 3 | 邮件 |

### enum `Language`

> 模板语言（不含小程序端专属语言包）

| 值 | 编号 | 说明 |
|---|---|---|
| `LANGUAGE_UNSPECIFIED` | 0 | 未指定，按用户偏好或全局默认回落 |
| `LANGUAGE_ZH_CN` | 1 | 简体中文 |
| `LANGUAGE_ZH_TW` | 2 | 繁体中文 |
| `LANGUAGE_EN` | 3 | 英文 |

### enum `DeliveryState`

> 投递任务状态机（合法迁移见 services/notification/README.md）

| 值 | 编号 | 说明 |
|---|---|---|
| `DELIVERY_STATE_UNSPECIFIED` | 0 | 未指定 |
| `DELIVERY_PENDING` | 1 | 已落库待投递 |
| `DELIVERY_SENT` | 2 | 供应商已受理（终态） |
| `DELIVERY_FAILED` | 3 | 不可重试失败（终态，需人工介入） |
| `DELIVERY_RETRY` | 4 | 待退避重试 |
| `DELIVERY_DEAD_LETTER` | 5 | 超过重试上限，转死信留档（终态） |
| `DELIVERY_SUPPRESSED` | 6 | 被免打扰/频控/去重拦截，未调用供应商（终态） |

### enum `TemplateState`

> 模板状态

| 值 | 编号 | 说明 |
|---|---|---|
| `TEMPLATE_STATE_UNSPECIFIED` | 0 | 未指定 |
| `TEMPLATE_STATE_DRAFT` | 1 | 草稿，不可用于投递 |
| `TEMPLATE_STATE_PUBLISHED` | 2 | 已发布，可投递 |
| `TEMPLATE_STATE_OFFLINE` | 3 | 已下线，只读留档 |

### enum `EventState`

> 事件消费状态（docs/api-and-events.md §6）

| 值 | 编号 | 说明 |
|---|---|---|
| `EVENT_STATE_UNSPECIFIED` | 0 | 未指定 |
| `EVENT_RECEIVED` | 1 | 已收到，未开始处理 |
| `EVENT_PROCESSING` | 2 | 处理中 |
| `EVENT_SUCCEEDED` | 3 | 处理成功（终态） |
| `EVENT_RETRY` | 4 | 处理失败，等待退避重试 |
| `EVENT_DEAD_LETTER` | 5 | 重试耗尽，转死信留档（终态） |

### enum `DeadLetterState`

> 死信处置状态

| 值 | 编号 | 说明 |
|---|---|---|
| `DEAD_LETTER_STATE_UNSPECIFIED` | 0 | 未指定 |
| `DEAD_LETTER_PENDING` | 1 | 待运营处理 |
| `DEAD_LETTER_RETRIED` | 2 | 已被重投（终态） |
| `DEAD_LETTER_DISCARDED` | 3 | 已丢弃（终态） |

### enum `Priority`

> 投递优先级；HIGH 用于验证码/安全提醒，可绕过免打扰窗口（不绕过频控）

| 值 | 编号 | 说明 |
|---|---|---|
| `PRIORITY_UNSPECIFIED` | 0 | — |
| `PRIORITY_LOW` | 1 | 低优先（运营类提醒） |
| `PRIORITY_NORMAL` | 2 | 普通 |
| `PRIORITY_HIGH` | 3 | 高优先（安全提醒、审核结论） |

### message `Recipient`

> 接收人（只携带受控标识，不放明文联系方式）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID（首选，account 域主键） |
| `target_ref` | `string` | 2 | — | 受控投递引用：push 设备 token 引用、供应商侧收件人 ID 或哈希标识 |
| `language` | [`Language`](#enum-language) | 3 | — | 期望语言；LANGUAGE_UNSPECIFIED 时按用户偏好回落 |
| `device_id` | `string` | 4 | — | push 设备标识（客户端上报，可用于多设备去重） |

### message `TemplateInfo`

> 模板（notification_template 表投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | 自增主键 |
| `template_code` | `string` | 2 | — | 模板业务码 |
| `channel` | [`Channel`](#enum-channel) | 3 | — | 通道 |
| `language` | [`Language`](#enum-language) | 4 | — | 语言 |
| `title_tpl` | `string` | 5 | — | 标题模板 |
| `body_tpl` | `string` | 6 | — | 正文模板 |
| `version` | `int32` | 7 | — | 版本号（同一 code/channel/lang 递增） |
| `state` | [`TemplateState`](#enum-templatestate) | 8 | — | 模板状态 |
| `operator` | `string` | 9 | — | 最后操作人（运营账号，来自 operation 域） |
| `ctime` | `int64` | 10 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 11 | — | 修改时间（Unix 秒） |

### message `DeliveryInfo`

> 投递记录（notification_delivery 表投影，含供应商回执）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `delivery_id` | `string` | 1 | — | 投递任务 ID |
| `biz_key` | `string` | 2 | — | 业务幂等键（唯一索引） |
| `mid` | `int64` | 3 | — | 接收人用户 ID（0 表示仅有 target_ref） |
| `channel` | [`Channel`](#enum-channel) | 4 | — | 通道 |
| `template_code` | `string` | 5 | — | 模板码 |
| `template_version` | `int32` | 6 | — | 模板版本 |
| `target_ref` | `string` | 7 | — | 受控投递标识（非明文号码） |
| `payload_digest` | `string` | 8 | — | 渲染结果摘要（sha256 hex），不落明文正文 |
| `state` | [`DeliveryState`](#enum-deliverystate) | 9 | — | 状态 |
| `provider` | `string` | 10 | — | 实际使用的通道适配器名 |
| `provider_msg_id` | `string` | 11 | — | 供应商回执消息 ID |
| `retry_count` | `int32` | 12 | — | 已重试次数 |
| `next_retry_at` | `int64` | 13 | — | 下次重试时间（Unix 秒，0 表示不再重试） |
| `last_error` | `string` | 14 | — | 最近一次错误（脱敏） |
| `sent_at` | `int64` | 15 | — | 投递成功时间（Unix 秒） |
| `expire_at` | `int64` | 16 | — | 过期时间（Unix 秒，0 表示不过期） |
| `priority` | `int32` | 17 | — | 优先级，参见 Priority |
| `trace_id` | `string` | 18 | — | 链路 ID |
| `ctime` | `int64` | 19 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 20 | — | 修改时间（Unix 秒） |

### message `DeadLetterInfo`

> 死信记录（notification_dead_letter 表投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | 自增主键 |
| `event_id` | `string` | 2 | — | 事件 ID（去重键） |
| `event_type` | `string` | 3 | — | 事件类型 |
| `topic` | `string` | 4 | — | 来源 topic |
| `payload_digest` | `string` | 5 | — | 原始报文摘要（不落明文，避免敏感信息留档） |
| `reason` | `string` | 6 | — | 死信原因 |
| `state` | [`DeadLetterState`](#enum-deadletterstate) | 7 | — | 处置状态 |
| `operator` | `string` | 8 | — | 处置人 |
| `ctime` | `int64` | 9 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 10 | — | 修改时间（Unix 秒） |

### message `DndPreference`

> 用户通道偏好与免打扰设置（notification_dnd_pref 表投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `muted_channels` | [`Channel`](#enum-channel) | 2 | repeated | 已关闭的通道（不在列表内表示允许） |
| `quiet_start` | `string` | 3 | — | 免打扰开始时间 HH:MM（本地时区） |
| `quiet_end` | `string` | 4 | — | 免打扰结束时间 HH:MM（支持跨天，如 22:00-08:00） |
| `timezone` | `string` | 5 | — | IANA 时区名，例如 Asia/Shanghai |
| `enabled` | `bool` | 6 | — | 免打扰总开关 |
| `ctime` | `int64` | 7 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 8 | — | 修改时间（Unix 秒） |

### message `SendNotificationReq`

> ==================== 投递 ====================

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `recipients` | [`Recipient`](#message-recipient) | 1 | repeated | 接收人列表（1..200，超出拒绝，避免批量放大） |
| `channel` | [`Channel`](#enum-channel) | 2 | — | 通道 |
| `template_code` | `string` | 3 | — | 模板码（必须为已发布版本） |
| `template_params` | `map<string, string>` | 4 | — | 模板变量（禁止放手机号/身份证等明文敏感值） |
| `idempotency_key` | `string` | 5 | — | 调用方幂等键；同 key 重复调用不产生新任务 |
| `biz_key` | `string` | 6 | — | 业务唯一键（落库唯一索引，跨实例去重） |
| `priority` | [`Priority`](#enum-priority) | 7 | — | 优先级 |
| `expire_at` | `int64` | 8 | — | 过期时间（Unix 秒）；0 表示不过期，过期任务不再投递 |
| `trace_id` | `string` | 9 | — | 调用方 trace_id |
| `default_language` | [`Language`](#enum-language) | 10 | — | 请求级默认语言；接收人未指定语言时回落此值，再回落全局默认 |

### message `SendNotificationReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `deliveries` | [`DeliveryInfo`](#message-deliveryinfo) | 1 | repeated | 每个接收人一条投递记录 |
| `suppressed` | `int32` | 2 | — | 被免打扰/频控拦截的数量 |
| `duplicate` | `bool` | 3 | — | biz_key 是否命中已存在的任务（幂等回放） |

### message `RenderTemplateReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `channel` | [`Channel`](#enum-channel) | 1 | — | 通道 |
| `template_code` | `string` | 2 | — | 模板码 |
| `version` | `int32` | 3 | — | 指定版本；0 表示使用当前已发布版本 |
| `language` | [`Language`](#enum-language) | 4 | — | 语言 |
| `template_params` | `map<string, string>` | 5 | — | 模板变量 |
| `operator` | `string` | 6 | — | 预览操作人（必填，供审计） |

### message `RenderTemplateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `title` | `string` | 1 | — | 渲染后标题 |
| `body` | `string` | 2 | — | 渲染后正文 |
| `version` | `int32` | 3 | — | 实际使用的模板版本 |
| `language` | [`Language`](#enum-language) | 4 | — | 实际使用的语言 |
| `missing_vars` | `string` | 5 | repeated | 缺失变量名（非空即渲染被拒绝） |

### message `UpsertTemplateReq`

> ==================== 模板管理 ====================

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `template_code` | `string` | 1 | — | 模板码 |
| `channel` | [`Channel`](#enum-channel) | 2 | — | 通道 |
| `language` | [`Language`](#enum-language) | 3 | — | 语言 |
| `title_tpl` | `string` | 4 | — | 标题模板 |
| `body_tpl` | `string` | 5 | — | 正文模板 |
| `operator` | `string` | 6 | — | 操作人（必填） |
| `publish` | `bool` | 7 | — | true 时直接发布为新版本；false 保存为草稿 |

### message `UpsertTemplateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `template` | [`TemplateInfo`](#message-templateinfo) | 1 | — | 落库后的模板（含 version 与 state） |

### message `ListTemplatesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `template_code` | `string` | 1 | — | 可选过滤 |
| `channel` | [`Channel`](#enum-channel) | 2 | — | 可选过滤（CHANNEL_UNSPECIFIED 表示不过滤） |
| `language` | [`Language`](#enum-language) | 3 | — | 可选过滤 |
| `state` | [`TemplateState`](#enum-templatestate) | 4 | — | 可选过滤 |
| `pn` | `int32` | 5 | — | 页码，从 1 开始 |
| `ps` | `int32` | 6 | — | 每页大小，最大 100 |

### message `ListTemplatesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `templates` | [`TemplateInfo`](#message-templateinfo) | 1 | repeated | 模板列表 |
| `total` | `int64` | 2 | — | 总数 |

### message `PublishTemplateReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `template_code` | `string` | 1 | — | 模板码 |
| `channel` | [`Channel`](#enum-channel) | 2 | — | 通道 |
| `language` | [`Language`](#enum-language) | 3 | — | 语言 |
| `version` | `int32` | 4 | — | 要发布的草稿版本 |
| `operator` | `string` | 5 | — | 操作人（必填） |

### message `PublishTemplateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `template` | [`TemplateInfo`](#message-templateinfo) | 1 | — | 发布后的模板 |

### message `GetDeliveryStatusReq`

> ==================== 投递记录查询 ====================

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `delivery_id` | `string` | 1 | — | 投递任务 ID |

### message `GetDeliveryStatusReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `delivery` | [`DeliveryInfo`](#message-deliveryinfo) | 1 | — | 投递记录（未找到时为默认值，found=false） |
| `found` | `bool` | 2 | — | 是否存在 |

### message `ListDeliveriesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 可选：按接收人过滤 |
| `channel` | [`Channel`](#enum-channel) | 2 | — | 可选过滤 |
| `state` | [`DeliveryState`](#enum-deliverystate) | 3 | — | 可选过滤 |
| `biz_key` | `string` | 4 | — | 可选：按幂等键精确查询 |
| `start_ctime` | `int64` | 5 | — | 可选：创建时间下界（Unix 秒） |
| `end_ctime` | `int64` | 6 | — | 可选：创建时间上界（Unix 秒） |
| `pn` | `int32` | 7 | — | 页码，从 1 开始 |
| `ps` | `int32` | 8 | — | 每页大小，最大 100 |

### message `ListDeliveriesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `deliveries` | [`DeliveryInfo`](#message-deliveryinfo) | 1 | repeated | 投递记录 |
| `total` | `int64` | 2 | — | 总数 |

### message `ListDeadLettersReq`

> ==================== 死信处理 ====================

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `event_id` | `string` | 1 | — | 可选：按事件 ID 精确查询 |
| `state` | [`DeadLetterState`](#enum-deadletterstate) | 2 | — | 可选过滤 |
| `topic` | `string` | 3 | — | 可选过滤 |
| `pn` | `int32` | 4 | — | 页码，从 1 开始 |
| `ps` | `int32` | 5 | — | 每页大小，最大 100 |

### message `ListDeadLettersReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `dead_letters` | [`DeadLetterInfo`](#message-deadletterinfo) | 1 | repeated | 死信列表 |
| `total` | `int64` | 2 | — | 总数 |

### message `RetryDeadLetterReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | 死信记录 ID |
| `operator` | `string` | 2 | — | 操作人（必填，写审计） |
| `reason` | `string` | 3 | — | 重投原因 |

### message `RetryDeadLetterReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `delivery_ids` | `string` | 1 | repeated | 重投产生的投递任务 ID |
| `retried` | `int32` | 2 | — | 重投数量 |
| `message` | `string` | 3 | — | 结果说明（脱敏） |

### message `UpdateDndPreferenceReq`

> ==================== 用户偏好 ====================

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `muted_channels` | [`Channel`](#enum-channel) | 2 | repeated | 关闭的通道列表（全量覆盖语义） |
| `quiet_start` | `string` | 3 | — | HH:MM，空串表示不设置免打扰时段 |
| `quiet_end` | `string` | 4 | — | HH:MM，与 quiet_start 成对 |
| `timezone` | `string` | 5 | — | IANA 时区名，空串表示使用全局默认 |
| `enabled` | `bool` | 6 | — | 免打扰总开关 |

### message `UpdateDndPreferenceReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `preference` | [`DndPreference`](#message-dndpreference) | 1 | — | 落库后的偏好 |

### message `GetDndPreferenceReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |

### message `GetDndPreferenceReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `preference` | [`DndPreference`](#message-dndpreference) | 1 | — | 偏好（未设置过时返回默认值） |
| `found` | `bool` | 2 | — | 是否已存在设置 |
