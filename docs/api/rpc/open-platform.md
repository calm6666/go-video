# RPC · `open-platform`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/open-platform/rpc/openplatform.proto` |
| protobuf 包 | `openplatform.v1` |
| go_package | `go-video/services/open-platform/rpc` |
| 发现用的 etcd key | `openplatform.v1.rpc`（`services/open-platform/etc/openplatform.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`openplatform.v1.rpc`） |
| 监听 | `8151`（`services/open-platform/etc/openplatform.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_open_platform` |
| 方法数 | 24（service `OpenPlatform`） |
| 网关消费方 | `admin:OpenPlatformRPC` |

## 契约说明

> 说明：open-platform 是第三方开放应用与授权领域的数据所有者，只提供 gRPC（无 .api），
> 遵循 AGENTS.md §3/§4：对外开放 HTTP 入口、签名校验编排和响应信封由 gateway 承担，
> 本服务提供授权判定、密钥/凭证生命周期、配额与回调投递，不返回数据库原始对象。
>
> 范围红线（AGENTS.md §1）：只开放明确的非商业化能力。scope 目录中不存在会员、订单、
> 支付、投币、分成、广告投放或广告位分析相关权限点，本服务也不提供这些接口位。
>
> 密钥与凭证（对齐 services/account 的凭证存储方式）：
>   - client_secret / authorization_code / access_token / refresh_token 一律只存哈希
>     （salt + HMAC-SHA256，服务端 pepper 来自 Secret/Vault，不入库、不入仓库）。
>   - 明文 secret 只在 RegisterApplication 与 RotateApplicationSecret 的响应中出现一次，
>     其余任何接口（含运营后台、列表、详情、日志）都不得回显，也不得写进事件 payload。
>   - account 的密码哈希沿用历史 MD5 以兼容 passport 客户端；本域没有历史包袱，
>     采用 salt + HMAC-SHA256(pepper, secret)，表结构仍与 account_secret 同形
>     （salt/hash/status 生效位/ctime/mtime，轮换把旧行置历史），差异在此显式记录。
>
> 授权与撤销：授权码短期一次性；refresh 可轮换、可撤销；access token 校验除自身状态外
> 还必须比对撤销位点 op_grant.revoked_at（grant 版本），保证“撤销立刻生效”，
> 即使 token 未被显式逐条标记也能在下次校验时被拒。
>
> 配额：op_quota_usage 是投影（应用 × 接口 × 时间窗计数），真值可由 op_api_call_log 重算
> （RecomputeQuota）；计数丢失只会短时放宽限额，不会破坏业务正确性。

## service `OpenPlatform`

> OpenPlatform 开放平台服务。 / 方法名表达领域动作，不暴露数据库 CRUD（docs/api-and-events.md §1.1）。 / 不提供任何会员、订单、支付、投币、分成或广告相关的接口位（AGENTS.md §1）。

gRPC 方法前缀：`openplatform.v1.OpenPlatform/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `RegisterApplication` | [`RegisterApplicationReq`](#message-registerapplicationreq) | [`RegisterApplicationReply`](#message-registerapplicationreply) | 注册应用（client_token 幂等），secret 仅此一次返回。 |
| 2 | `GetApplication` | [`GetApplicationReq`](#message-getapplicationreq) | [`GetApplicationReply`](#message-getapplicationreply) | 查询应用（不回显密钥）。 |
| 3 | `ListApplications` | [`ListApplicationsReq`](#message-listapplicationsreq) | [`ListApplicationsReply`](#message-listapplicationsreply) | 开发者/运营侧应用分页列表。 |
| 4 | `UpdateApplication` | [`UpdateApplicationReq`](#message-updateapplicationreq) | [`UpdateApplicationReply`](#message-updateapplicationreply) | 修改资料或推进状态机（乐观锁版本）。 |
| 5 | `RotateApplicationSecret` | [`RotateApplicationSecretReq`](#message-rotateapplicationsecretreq) | [`RotateApplicationSecretReply`](#message-rotateapplicationsecretreply) | 轮换密钥（旧密钥宽限期后可用性明确）。 |
| 6 | `RevokeApplicationSecret` | [`RevokeApplicationSecretReq`](#message-revokeapplicationsecretreq) | [`RevokeApplicationSecretReply`](#message-revokeapplicationsecretreply) | 吊销密钥（疑似泄露的应急处置）。 |
| 7 | `ListScopes` | [`ListScopesReq`](#message-listscopesreq) | [`ListScopesReply`](#message-listscopesreply) | scope 目录（含读写与风险级别声明）。 |
| 8 | `GrantApplicationScopes` | [`GrantApplicationScopesReq`](#message-grantapplicationscopesreq) | [`GrantApplicationScopesReply`](#message-grantapplicationscopesreply) | 运营审批 scope 授予/回收。 |
| 9 | `IssueAuthorizationCode` | [`IssueAuthorizationCodeReq`](#message-issueauthorizationcodereq) | [`IssueAuthorizationCodeReply`](#message-issueauthorizationcodereply) | 用户同意后签发短期授权码。 |
| 10 | `ExchangeAuthorizationCode` | [`ExchangeAuthorizationCodeReq`](#message-exchangeauthorizationcodereq) | [`ExchangeAuthorizationCodeReply`](#message-exchangeauthorizationcodereply) | 授权码换 token（一次性消费 + 重放检测）。 |
| 11 | `RefreshAccessToken` | [`RefreshAccessTokenReq`](#message-refreshaccesstokenreq) | [`RefreshAccessTokenReply`](#message-refreshaccesstokenreply) | refresh token 轮换（旧值重放即撤销整条 grant）。 |
| 12 | `RevokeAuthorization` | [`RevokeAuthorizationReq`](#message-revokeauthorizationreq) | [`RevokeAuthorizationReply`](#message-revokeauthorizationreply) | 撤销授权（写撤销位点，立即生效）。 |
| 13 | `IntrospectToken` | [`IntrospectTokenReq`](#message-introspecttokenreq) | [`IntrospectTokenReply`](#message-introspecttokenreply) | token 校验（含 grant 撤销位点比对）。 |
| 14 | `AuthorizeRequest` | [`AuthorizeRequestReq`](#message-authorizerequestreq) | [`AuthorizeRequestReply`](#message-authorizerequestreply) | 网关前置聚合检查：凭证 + scope + 配额扣减 + 调用流水（request_id 幂等）。 |
| 15 | `UpsertQuotaPolicy` | [`UpsertQuotaPolicyReq`](#message-upsertquotapolicyreq) | [`UpsertQuotaPolicyReply`](#message-upsertquotapolicyreply) | 新增/更新配额规则（运营）。 |
| 16 | `ListQuotaPolicies` | [`ListQuotaPoliciesReq`](#message-listquotapoliciesreq) | [`ListQuotaPoliciesReply`](#message-listquotapoliciesreply) | 配额规则分页。 |
| 17 | `ListQuotaUsage` | [`ListQuotaUsageReq`](#message-listquotausagereq) | [`ListQuotaUsageReply`](#message-listquotausagereply) | 配额用量查询（投影）。 |
| 18 | `RecomputeQuota` | [`RecomputeQuotaReq`](#message-recomputequotareq) | [`RecomputeQuotaReply`](#message-recomputequotareply) | 从调用流水重算配额投影。 |
| 19 | `RegisterWebhook` | [`RegisterWebhookReq`](#message-registerwebhookreq) | [`RegisterWebhookReply`](#message-registerwebhookreply) | 注册回调端点（需验证后才投递）。 |
| 20 | `ListWebhooks` | [`ListWebhooksReq`](#message-listwebhooksreq) | [`ListWebhooksReply`](#message-listwebhooksreply) | 回调端点列表。 |
| 21 | `DeleteWebhook` | [`DeleteWebhookReq`](#message-deletewebhookreq) | [`DeleteWebhookReply`](#message-deletewebhookreply) | 删除回调端点（抑制未投递任务）。 |
| 22 | `EnqueueWebhookEvent` | [`EnqueueWebhookEventReq`](#message-enqueuewebhookeventreq) | [`EnqueueWebhookEventReply`](#message-enqueuewebhookeventreply) | 领域事件入队（event_id 幂等）。 |
| 23 | `ListWebhookDeliveries` | [`ListWebhookDeliveriesReq`](#message-listwebhookdeliveriesreq) | [`ListWebhookDeliveriesReply`](#message-listwebhookdeliveriesreply) | 投递记录分页（观测与排障）。 |
| 24 | `RetryWebhookDelivery` | [`RetryWebhookDeliveryReq`](#message-retrywebhookdeliveryreq) | [`RetryWebhookDeliveryReply`](#message-retrywebhookdeliveryreply) | 死信重放（运营）。 |

## 消息与枚举

### message `EmptyReply`

> 空响应。

（空消息）

### enum `AppStatus`

> 应用状态机（与 op_app.status 一致）。 / PENDING_REVIEW → ACTIVE/REJECTED；ACTIVE ↔ SUSPENDED；任意态 → OFFLINE（终态，需重新注册）。

| 值 | 编号 | 说明 |
|---|---|---|
| `APP_STATUS_UNSPECIFIED` | 0 | 未指定 |
| `APP_STATUS_PENDING_REVIEW` | 1 | 待运营审核 |
| `APP_STATUS_ACTIVE` | 2 | 正常可用 |
| `APP_STATUS_SUSPENDED` | 3 | 已停用（违规/风控），token 与配额全部拒绝 |
| `APP_STATUS_REJECTED` | 4 | 审核驳回 |
| `APP_STATUS_OFFLINE` | 5 | 已下线（终态） |

### enum `ScopeAccess`

> scope 读写属性（与 op_scope.access 一致）：每个权限点必须显式声明读或写。

| 值 | 编号 | 说明 |
|---|---|---|
| `SCOPE_ACCESS_UNSPECIFIED` | 0 | 未指定：服务端拒绝注册该 scope |
| `SCOPE_ACCESS_READ` | 1 | 只读 |
| `SCOPE_ACCESS_WRITE` | 2 | 写（必须再声明是否要求用户逐次同意） |

### enum `ScopeRiskLevel`

> scope 风险级别（与 op_scope.risk_level 一致），决定授权页展示强度与是否可批量授予。

| 值 | 编号 | 说明 |
|---|---|---|
| `SCOPE_RISK_LEVEL_UNSPECIFIED` | 0 | — |
| `SCOPE_RISK_LEVEL_LOW` | 1 | 低：可默认勾选 |
| `SCOPE_RISK_LEVEL_MEDIUM` | 2 | 中：需用户显式确认 |
| `SCOPE_RISK_LEVEL_HIGH` | 3 | 高：需运营审批且不可批量授予 |

### enum `GrantType`

> 授权换取方式（与 op_token.grant_type 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `GRANT_TYPE_UNSPECIFIED` | 0 | — |
| `GRANT_TYPE_AUTHORIZATION_CODE` | 1 | 授权码换 token |
| `GRANT_TYPE_REFRESH_TOKEN` | 2 | refresh 轮换出的新 token |

### enum `TokenState`

> token 状态（与 op_token.state 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `TOKEN_STATE_UNSPECIFIED` | 0 | — |
| `TOKEN_STATE_ACTIVE` | 1 | 有效（仍需比对 grant 撤销位点） |
| `TOKEN_STATE_ROTATED` | 2 | 已被轮换替代（旧 refresh/access） |
| `TOKEN_STATE_REVOKED` | 3 | 已撤销 |
| `TOKEN_STATE_EXPIRED` | 4 | 已过期（惰性归档） |

### enum `RevokeTarget`

> 撤销目标（RevokeAuthorization 使用）。

| 值 | 编号 | 说明 |
|---|---|---|
| `REVOKE_TARGET_UNSPECIFIED` | 0 | 未指定：服务端拒绝 |
| `REVOKE_TARGET_TOKEN` | 1 | 撤销单个 token（access 或 refresh） |
| `REVOKE_TARGET_GRANT` | 2 | 撤销“该用户对该应用”的全部授权 |
| `REVOKE_TARGET_USER_ALL` | 3 | 撤销该用户的全部第三方授权（改密/风控一键下线） |

### enum `WebhookEventType`

> Webhook 事件类型（与 op_webhook_endpoint.event_type、op_webhook_delivery.event_type 一致）。 / 只包含内容/授权/配额类事件，不存在商业化事件。

| 值 | 编号 | 说明 |
|---|---|---|
| `WEBHOOK_EVENT_TYPE_UNSPECIFIED` | 0 | — |
| `WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT` | 1 | 第三方投稿转码/审核结果 |
| `WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE` | 2 | 内容下架、版权撤回 |
| `WEBHOOK_EVENT_TYPE_GRANT_REVOKED` | 3 | 用户或平台撤销授权 |
| `WEBHOOK_EVENT_TYPE_QUOTA_WARNING` | 4 | 配额接近上限告警 |

### enum `WebhookDeliveryState`

> Webhook 投递状态（与 op_webhook_delivery.state 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `WEBHOOK_DELIVERY_STATE_UNSPECIFIED` | 0 | — |
| `WEBHOOK_DELIVERY_STATE_PENDING` | 1 | 待投递 |
| `WEBHOOK_DELIVERY_STATE_DELIVERING` | 2 | 投递中（超时后可被抢占） |
| `WEBHOOK_DELIVERY_STATE_SUCCESS` | 3 | 成功（HTTP 2xx 且签名回执校验通过） |
| `WEBHOOK_DELIVERY_STATE_RETRY_SCHEDULED` | 4 | 已排定退避重试 |
| `WEBHOOK_DELIVERY_STATE_DEAD` | 5 | 死信（超过最大次数，等待人工重放） |
| `WEBHOOK_DELIVERY_STATE_IGNORED` | 6 | 人工忽略（终态） |

### enum `SecretState`

> client_secret 状态（应用信息投影字段，绝不含密钥材料）。

| 值 | 编号 | 说明 |
|---|---|---|
| `SECRET_STATE_UNSPECIFIED` | 0 | — |
| `SECRET_STATE_CONFIGURED` | 1 | 存在生效密钥 |
| `SECRET_STATE_REVOKED` | 2 | 全部密钥已撤销，签名调用将被拒 |
| `SECRET_STATE_UNSET` | 3 | 从未签发 |

### message `ApplicationInfo`

> ApplicationInfo 应用投影。注意：没有 client_secret 字段。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | 应用 ID（跨服务只传这个主键） |
| `app_key` | `string` | 2 | — | 公开标识（签发后不可变） |
| `name` | `string` | 3 | — | 应用名 |
| `description` | `string` | 4 | — | 应用简介 |
| `owner_mid` | `int64` | 5 | — | 归属开发者 mid |
| `status` | [`AppStatus`](#enum-appstatus) | 6 | — | 状态 |
| `redirect_uris` | `string` | 7 | repeated | 授权回调白名单 |
| `scopes` | `string` | 8 | repeated | 已获批 scope 列表 |
| `secret_state` | [`SecretState`](#enum-secretstate) | 9 | — | 密钥状态（不回显密钥） |
| `secret_rotated_at` | `int64` | 10 | — | 最近一次轮换时间（Unix 秒） |
| `version` | `int32` | 11 | — | 乐观锁版本 |
| `ctime` | `int64` | 12 | — | 创建时间 |
| `mtime` | `int64` | 13 | — | 最近更新时间 |
| `offline_at` | `int64` | 14 | — | 下线时间（0 表示未下线） |

### message `ScopeInfo`

> ScopeInfo 权限点目录。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scope` | `string` | 1 | — | 权限点标识，如 video.publish |
| `display_name` | `string` | 2 | — | 展示名（授权页文案） |
| `access` | [`ScopeAccess`](#enum-scopeaccess) | 3 | — | 读/写声明 |
| `risk_level` | [`ScopeRiskLevel`](#enum-scoperisklevel) | 4 | — | 风险级别 |
| `requires_user_consent` | `bool` | 5 | — | 是否需要用户逐次同意 |
| `enabled` | `bool` | 6 | — | 目录内是否开放 |
| `reason` | `string` | 7 | — | 停用原因 |
| `granted_state` | `int32` | 8 | — | 请求方上下文中的应用获批状态：0 未申请、1 待审批、2 已获批 |

### message `TokenSet`

> TokenSet 一次性签发的令牌组（只在签发响应中出现，不进入任何查询接口）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `access_token` | `string` | 1 | — | 明文 access token（仅本次响应出现） |
| `token_type` | `string` | 2 | — | 固定 Bearer |
| `expires_in` | `int64` | 3 | — | access 有效期（秒） |
| `refresh_token` | `string` | 4 | — | 明文 refresh token（仅本次响应出现） |
| `refresh_expires_in` | `int64` | 5 | — | refresh 有效期（秒） |
| `scope` | `string` | 6 | repeated | 实际授予 scope（可能小于申请值） |
| `grant_id` | `int64` | 7 | — | 授权关系 ID（撤销位点锚点） |
| `token_id` | `int64` | 8 | — | op_token 行 ID |
| `issued_at` | `int64` | 9 | — | 签发时间（Unix 秒） |

### message `RegisterApplicationReq`

> --- 应用生命周期 --- / RegisterApplicationReq 注册第三方应用。 / 幂等：client_token 必填，同 client_token 重试返回同一应用且不重复签发 secret。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `name` | `string` | 1 | — | 应用名 |
| `description` | `string` | 2 | — | 简介 |
| `owner_mid` | `int64` | 3 | — | 开发者 mid（gateway 从会话注入） |
| `redirect_uris` | `string` | 4 | repeated | 回调白名单，https only |
| `scopes` | `string` | 5 | repeated | 申请的 scope（未获批前不生效） |
| `client_token` | `string` | 6 | — | 幂等键（必填） |
| `trace_id` | `string` | 7 | — | — |

### message `RegisterApplicationReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app` | [`ApplicationInfo`](#message-applicationinfo) | 1 | — | — |
| `client_secret` | `string` | 2 | — | 明文密钥，仅本次返回，任何接口不再回显 |
| `secret_expires_at` | `int64` | 3 | — | 密钥有效期（0 表示按配置长期有效） |
| `replayed` | `bool` | 4 | — | true 表示命中 client_token，未新建应用且不再回显旧密钥 |

### message `GetApplicationReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | 与 app_key 二选一 |
| `app_key` | `string` | 2 | — | — |
| `caller_mid` | `int64` | 3 | — | 调用者（owner 或运营） |
| `operator` | `bool` | 4 | — | true 表示运营侧查询（可查非 ACTIVE 应用） |
| `trace_id` | `string` | 5 | — | — |

### message `GetApplicationReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app` | [`ApplicationInfo`](#message-applicationinfo) | 1 | — | — |

### message `ListApplicationsReq`

> ListApplicationsReq 开发者看自己的应用；operator=true 时运营看全量。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `owner_mid` | `int64` | 1 | — | — |
| `status` | [`AppStatus`](#enum-appstatus) | 2 | — | UNSPECIFIED 表示不过滤 |
| `cursor` | `string` | 3 | — | (mtime, app_id) 游标 |
| `ps` | `int32` | 4 | — | — |
| `operator` | `bool` | 5 | — | 运营侧全量查询 |
| `trace_id` | `string` | 6 | — | — |

### message `ListApplicationsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`ApplicationInfo`](#message-applicationinfo) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |

### message `UpdateApplicationReq`

> UpdateApplicationReq 修改资料或推进状态机。 / 并发：expected_version 不匹配返回 ErrConcurrentUpdate（状态版本双重防重，AGENTS.md §5）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `name` | `string` | 2 | — | 空串表示不修改 |
| `description` | `string` | 3 | — | — |
| `redirect_uris` | `string` | 4 | repeated | 空列表表示不修改 |
| `target_status` | [`AppStatus`](#enum-appstatus) | 5 | — | UNSPECIFIED 表示不改状态 |
| `expected_version` | `int32` | 6 | — | 乐观锁 |
| `operator_mid` | `int64` | 7 | — | 状态推进必须是运营；资料修改必须是 owner |
| `is_operator` | `bool` | 8 | — | — |
| `reason` | `string` | 9 | — | 状态变更原因（审计） |
| `trace_id` | `string` | 10 | — | — |

### message `UpdateApplicationReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app` | [`ApplicationInfo`](#message-applicationinfo) | 1 | — | — |
| `changed` | `bool` | 2 | — | — |

### message `RotateApplicationSecretReq`

> RotateApplicationSecretReq 轮换密钥：新密钥立即生效，旧密钥在 grace_seconds 后失效 / （宽限期内旧密钥仍可用以便应用平滑切换；grace_seconds=0 表示立即失效）。 / 响应一次性返回新明文 secret。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `operator_mid` | `int64` | 2 | — | owner 或运营 |
| `is_operator` | `bool` | 3 | — | — |
| `grace_seconds` | `int64` | 4 | — | 旧密钥宽限秒数，0 表示立即失效 |
| `reason` | `string` | 5 | — | 轮换原因（泄露/例行轮换，审计） |
| `trace_id` | `string` | 6 | — | — |

### message `RotateApplicationSecretReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `client_secret` | `string` | 1 | — | 新明文密钥，仅本次返回 |
| `secret_id` | `int64` | 2 | — | 新密钥行 ID |
| `old_secret_id` | `int64` | 3 | — | 被替换的旧密钥行 ID（0 表示无） |
| `old_secret_expires_at` | `int64` | 4 | — | 旧密钥失效时间（Unix 秒） |
| `rotated_at` | `int64` | 5 | — | — |

### message `RevokeApplicationSecretReq`

> RevokeApplicationSecretReq 紧急吊销密钥（怀疑泄露）：可指定单把或全部。 / 吊销后所有依赖 secret 的签名调用立即被拒，已签发的 token 不受影响 / （除非同时 RevokeAuthorization）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `secret_id` | `int64` | 2 | — | 0 表示吊销该应用全部生效密钥 |
| `operator_mid` | `int64` | 3 | — | — |
| `is_operator` | `bool` | 4 | — | — |
| `reason` | `string` | 5 | — | 审计原因，必填 |
| `trace_id` | `string` | 6 | — | — |

### message `RevokeApplicationSecretReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `revoked` | `int32` | 1 | — | — |
| `effective_at` | `int64` | 2 | — | — |

### message `ListScopesReq`

> --- scope 目录与审批 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | 传入则返回该应用对每个 scope 的获批状态 |
| `only_enabled` | `bool` | 2 | — | 只返回开放的 scope |
| `trace_id` | `string` | 3 | — | — |

### message `ListScopesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`ScopeInfo`](#message-scopeinfo) | 1 | repeated | — |

### message `GrantApplicationScopesReq`

> GrantApplicationScopesReq 运营审批 scope 授予/回收。回收立即生效， / 并在受影响 token 的下次校验时因 scope 不足被拒（同时写撤销位点）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `grant` | `string` | 2 | repeated | 授予的 scope |
| `revoke` | `string` | 3 | repeated | 回收的 scope |
| `operator_mid` | `int64` | 4 | — | 运营，必填 > 0 |
| `reason` | `string` | 5 | — | — |
| `idempotency_key` | `string` | 6 | — | 幂等键 |
| `trace_id` | `string` | 7 | — | — |

### message `GrantApplicationScopesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `granted` | `string` | 1 | repeated | — |
| `revoked` | `string` | 2 | repeated | — |
| `rejected` | `string` | 3 | repeated | 目录中不存在或已停用的 scope |
| `app_version` | `int32` | 4 | — | — |
| `replayed` | `bool` | 5 | — | — |

### message `IssueAuthorizationCodeReq`

> --- OAuth --- / IssueAuthorizationCodeReq 已登录用户同意授权后由 gateway 调用签发授权码。 / consent_given 必须为 true（服务端不接受“隐式同意”）；scope 必须是应用已获批 scope 的子集。 / 授权码短期（AuthCodeTTLSeconds，默认 60s）且一次性消费。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `mid` | `int64` | 2 | — | 授权用户（gateway 会话注入） |
| `scope` | `string` | 3 | repeated | 用户同意的 scope |
| `redirect_uri` | `string` | 4 | — | 必须在应用白名单内 |
| `state` | `string` | 5 | — | 客户端 state，仅随响应回显 |
| `consent_given` | `bool` | 6 | — | 必须 true |
| `trace_id` | `string` | 7 | — | — |

### message `IssueAuthorizationCodeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `code` | `string` | 1 | — | 授权码明文（仅经 gateway 302 回跳，不入库明文） |
| `expires_in` | `int64` | 2 | — | — |
| `scope` | `string` | 3 | repeated | 实际授予 |
| `grant_id` | `int64` | 4 | — | — |

### message `ExchangeAuthorizationCodeReq`

> ExchangeAuthorizationCodeReq 授权码换 token：同一 code 二次消费返回 ErrAuthCodeUsed / 并触发一次重放告警（记录调用方，便于发现泄露）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `code` | `string` | 2 | — | — |
| `redirect_uri` | `string` | 3 | — | 与签发时一致性校验 |
| `trace_id` | `string` | 4 | — | — |

### message `ExchangeAuthorizationCodeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `token` | [`TokenSet`](#message-tokenset) | 1 | — | — |

### message `RefreshAccessTokenReq`

> RefreshAccessTokenReq 轮换 refresh token：旧 refresh 立即置 ROTATED， / 若旧值被再次使用则判定重放并撤销整条 grant（保守失效）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `refresh_token` | `string` | 2 | — | — |
| `scope` | `string` | 3 | repeated | 允许收窄，不允许扩大 |
| `trace_id` | `string` | 4 | — | — |

### message `RefreshAccessTokenReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `token` | [`TokenSet`](#message-tokenset) | 1 | — | — |
| `rotated` | `bool` | 2 | — | 是否签发了新的 refresh_token |

### message `RevokeAuthorizationReq`

> RevokeAuthorizationReq 撤销授权（RFC 7009 语义）。 / 生效方式：写 op_grant.revoked_at 位点 + 逐条标记 op_token，双保险保证立即生效， / 即使网关侧持有缓存的 token 校验结果也会在 TTL 到期后被位点拒绝。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `target` | [`RevokeTarget`](#enum-revoketarget) | 1 | — | — |
| `app_id` | `int64` | 2 | — | TOKEN/GRANT 必填 |
| `mid` | `int64` | 3 | — | GRANT/USER_ALL 必填 |
| `token_id` | `int64` | 4 | — | TOKEN 必填（或传 token_hint） |
| `token_hint` | `string` | 5 | — | access/refresh 明文（仅本次请求内使用，不日志） |
| `operator_mid` | `int64` | 6 | — | 用户本人或运营 |
| `is_operator` | `bool` | 7 | — | — |
| `reason` | `string` | 8 | — | 审计原因 |
| `trace_id` | `string` | 9 | — | — |

### message `RevokeAuthorizationReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `grants_revoked` | `int32` | 1 | — | — |
| `tokens_revoked` | `int32` | 2 | — | — |
| `effective_at` | `int64` | 3 | — | 撤销位点时间（Unix 秒） |

### message `IntrospectTokenReq`

> IntrospectTokenReq token 校验（gateway 鉴权中间件调用）。 / 校验顺序：token 行状态 → 过期时间 → grant 撤销位点 → 应用状态 → scope（若带 api_code）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `access_token` | `string` | 1 | — | — |
| `token_id` | `int64` | 2 | — | 已知 token_id 时可省 access_token |
| `api_code` | `string` | 3 | — | 带接口标识时一并做 scope 判定 |
| `required_scope` | `string` | 4 | — | — |
| `trace_id` | `string` | 5 | — | — |

### message `IntrospectTokenReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `active` | `bool` | 1 | — | — |
| `app_id` | `int64` | 2 | — | — |
| `mid` | `int64` | 3 | — | 0 表示应用级凭证（本期不签发） |
| `scope` | `string` | 4 | repeated | — |
| `expires_at` | `int64` | 5 | — | — |
| `grant_id` | `int64` | 6 | — | — |
| `token_id` | `int64` | 7 | — | — |
| `deny_reason` | `string` | 8 | — | 拒绝原因码（inactive/expired/revoked/app_suspended/scope_missing） |

### message `AuthorizeRequestReq`

> AuthorizeRequestReq 网关前置检查的聚合入口：应用状态 + 凭证有效性 + scope + 配额扣减 + 调用流水。 / 两种凭证模式：access_token（用户授权）或 app_key + signature（应用级签名， / signature = HMAC-SHA256(secret, canonical)，canonical 含 method/path/timestamp/nonce/body 摘要）。 / 幂等：request_id 唯一，重试返回首次判定结果且不重复扣配额。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `access_token` | `string` | 1 | — | — |
| `app_key` | `string` | 2 | — | — |
| `signature` | `string` | 3 | — | 签名模式必填 |
| `timestamp` | `int64` | 4 | — | 请求时间戳（Unix 秒），超出 SkewSeconds 拒绝 |
| `nonce` | `string` | 5 | — | 防重放随机串 |
| `method` | `string` | 6 | — | — |
| `path` | `string` | 7 | — | — |
| `body_digest` | `string` | 8 | — | 请求体 sha256 摘要，不含原文 |
| `api_code` | `string` | 9 | — | 接口标识（配额与 scope 的键） |
| `required_scope` | `string` | 10 | — | — |
| `request_id` | `string` | 11 | — | 幂等键，必填 |
| `client_ip` | `string` | 12 | — | 记录用（脱敏后落库，不外发） |
| `trace_id` | `string` | 13 | — | — |

### message `AuthorizeRequestReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `allowed` | `bool` | 1 | — | — |
| `deny_reason` | `string` | 2 | — | 拒绝原因码 |
| `app_id` | `int64` | 3 | — | — |
| `mid` | `int64` | 4 | — | — |
| `scope` | `string` | 5 | repeated | — |
| `quota_limit` | `int64` | 6 | — | — |
| `quota_remaining` | `int64` | 7 | — | — |
| `window_reset_at` | `int64` | 8 | — | — |
| `retry_after_seconds` | `int64` | 9 | — | 限流拒绝时的建议等待 |
| `call_log_id` | `int64` | 10 | — | — |

### message `QuotaPolicyInfo`

> --- 配额 --- / QuotaPolicyInfo 配额规则（应用 × 接口 × 时间窗）。app_id=0 表示全局默认， / api_code="*" 表示该应用全部接口。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `policy_id` | `int64` | 1 | — | — |
| `app_id` | `int64` | 2 | — | — |
| `api_code` | `string` | 3 | — | — |
| `window_seconds` | `int64` | 4 | — | — |
| `limit` | `int64` | 5 | — | — |
| `enabled` | `bool` | 6 | — | — |
| `operator` | `int64` | 7 | — | — |
| `ctime` | `int64` | 8 | — | — |
| `mtime` | `int64` | 9 | — | — |

### message `UpsertQuotaPolicyReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `policy_id` | `int64` | 1 | — | 0 表示新建（按唯一键 upsert） |
| `app_id` | `int64` | 2 | — | 0 表示全局默认 |
| `api_code` | `string` | 3 | — | "*" 表示全部接口 |
| `window_seconds` | `int64` | 4 | — | — |
| `limit` | `int64` | 5 | — | — |
| `enabled` | `bool` | 6 | — | — |
| `operator_mid` | `int64` | 7 | — | 运营，必填 > 0 |
| `trace_id` | `string` | 8 | — | — |

### message `UpsertQuotaPolicyReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `policy_id` | `int64` | 1 | — | — |
| `created` | `bool` | 2 | — | — |

### message `ListQuotaPoliciesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `api_code` | `string` | 2 | — | — |
| `cursor` | `string` | 3 | — | — |
| `ps` | `int32` | 4 | — | — |
| `operator_mid` | `int64` | 5 | — | — |
| `trace_id` | `string` | 6 | — | — |

### message `ListQuotaPoliciesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`QuotaPolicyInfo`](#message-quotapolicyinfo) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |

### message `QuotaUsageInfo`

> QuotaUsageInfo 配额用量投影（时间窗对齐到窗口起点，可重算）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `api_code` | `string` | 2 | — | — |
| `window_seconds` | `int64` | 3 | — | — |
| `window_start` | `int64` | 4 | — | — |
| `used` | `int64` | 5 | — | — |
| `limit` | `int64` | 6 | — | — |
| `remaining` | `int64` | 7 | — | — |
| `updated_at` | `int64` | 8 | — | — |

### message `ListQuotaUsageReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `api_code` | `string` | 2 | — | — |
| `window_start` | `int64` | 3 | — | 0 表示当前窗口 |
| `operator_mid` | `int64` | 4 | — | — |
| `trace_id` | `string` | 5 | — | — |

### message `ListQuotaUsageReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`QuotaUsageInfo`](#message-quotausageinfo) | 1 | repeated | — |

### message `RecomputeQuotaReq`

> RecomputeQuotaReq 从 op_api_call_log 重算配额投影（cron 或运营触发）。 / 说明：配额计数是投影，不作为唯一事实源；重算结果差异只影响限额判定，不影响业务数据。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | 0 表示全部应用 |
| `api_code` | `string` | 2 | — | "*" 表示全部接口 |
| `window_start` | `int64` | 3 | — | 重算区间起（含） |
| `window_end` | `int64` | 4 | — | 重算区间止（不含） |
| `dry_run` | `bool` | 5 | — | true 只返回差异不写回 |
| `operator_mid` | `int64` | 6 | — | — |
| `trace_id` | `string` | 7 | — | — |

### message `RecomputeQuotaReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `windows_scanned` | `int64` | 1 | — | — |
| `windows_fixed` | `int64` | 2 | — | — |
| `max_delta` | `int64` | 3 | — | 单窗口最大修正量（观测漂移） |

### message `WebhookEndpointInfo`

> --- Webhook --- / WebhookEndpointInfo 回调端点。sign_key_version 用于告知应用当前签名密钥版本； / 签名材料本身不入库（由服务端 master pepper 与 app_id + key_version 派生， / 因此数据库泄露也无法伪造合法签名，见 README）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `endpoint_id` | `int64` | 1 | — | — |
| `app_id` | `int64` | 2 | — | — |
| `event_type` | [`WebhookEventType`](#enum-webhookeventtype) | 3 | — | — |
| `url` | `string` | 4 | — | — |
| `sign_key_version` | `int32` | 5 | — | — |
| `enabled` | `bool` | 6 | — | — |
| `description` | `string` | 7 | — | — |
| `verified_at` | `int64` | 8 | — | 验证通过时间（未验证为 0，未验证不投递） |
| `ctime` | `int64` | 9 | — | — |
| `mtime` | `int64` | 10 | — | — |

### message `RegisterWebhookReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `event_type` | [`WebhookEventType`](#enum-webhookeventtype) | 2 | — | — |
| `url` | `string` | 3 | — | https only，禁止指向内网地址（SSRF 防护） |
| `description` | `string` | 4 | — | — |
| `operator_mid` | `int64` | 5 | — | owner 或运营 |
| `is_operator` | `bool` | 6 | — | — |
| `trace_id` | `string` | 7 | — | — |

### message `RegisterWebhookReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `endpoint_id` | `int64` | 1 | — | — |
| `created` | `bool` | 2 | — | — |
| `sign_key_version` | `int32` | 3 | — | — |
| `verification_challenge` | `string` | 4 | — | 需应用回显以完成验证 |

### message `ListWebhooksReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `include_disabled` | `bool` | 2 | — | — |
| `operator_mid` | `int64` | 3 | — | — |
| `trace_id` | `string` | 4 | — | — |

### message `ListWebhooksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`WebhookEndpointInfo`](#message-webhookendpointinfo) | 1 | repeated | — |

### message `DeleteWebhookReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `endpoint_id` | `int64` | 2 | — | — |
| `operator_mid` | `int64` | 3 | — | — |
| `is_operator` | `bool` | 4 | — | — |
| `reason` | `string` | 5 | — | — |
| `trace_id` | `string` | 6 | — | — |

### message `DeleteWebhookReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `deleted` | `bool` | 1 | — | — |
| `deliveries_suppressed` | `int32` | 2 | — | 被抑制的待投递数 |

### message `EnqueueWebhookEventReq`

> EnqueueWebhookEventReq 领域方向应用投递事件（幂等：event_id 唯一， / 同一 event_id 重复入队不产生第二条投递任务）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `event_type` | [`WebhookEventType`](#enum-webhookeventtype) | 2 | — | — |
| `event_id` | `string` | 3 | — | 幂等键（必填，来自领域 event envelope） |
| `payload` | `string` | 4 | — | JSON 文本，禁止含 token/secret/身份证/手机号明文 |
| `occurred_at` | `int64` | 5 | — | 事件发生时间（Unix 秒） |
| `mid` | `int64` | 6 | — | 关联用户（可为 0） |
| `biz_type` | `string` | 7 | — | — |
| `biz_id` | `string` | 8 | — | — |
| `trace_id` | `string` | 9 | — | — |

### message `EnqueueWebhookEventReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `delivery_ids` | `int64` | 1 | repeated | 命中的端点数对应的投递任务 |
| `matched_endpoints` | `int32` | 2 | — | — |
| `deduplicated` | `bool` | 3 | — | true 表示 event_id 已入过队 |

### message `WebhookDeliveryInfo`

> WebhookDeliveryInfo 投递记录（重试退避与死信）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `delivery_id` | `int64` | 1 | — | — |
| `app_id` | `int64` | 2 | — | — |
| `endpoint_id` | `int64` | 3 | — | — |
| `event_type` | [`WebhookEventType`](#enum-webhookeventtype) | 4 | — | — |
| `event_id` | `string` | 5 | — | — |
| `payload_digest` | `string` | 6 | — | sha256:<hex>，不存正文 |
| `state` | [`WebhookDeliveryState`](#enum-webhookdeliverystate) | 7 | — | — |
| `attempt` | `int32` | 8 | — | — |
| `max_attempts` | `int32` | 9 | — | — |
| `next_retry_at` | `int64` | 10 | — | — |
| `last_status_code` | `int64` | 11 | — | — |
| `last_error` | `string` | 12 | — | 截断且脱敏 |
| `ctime` | `int64` | 13 | — | — |
| `mtime` | `int64` | 14 | — | — |

### message `ListWebhookDeliveriesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `app_id` | `int64` | 1 | — | — |
| `endpoint_id` | `int64` | 2 | — | — |
| `state` | [`WebhookDeliveryState`](#enum-webhookdeliverystate) | 3 | — | UNSPECIFIED 表示不过滤 |
| `cursor` | `string` | 4 | — | — |
| `ps` | `int32` | 5 | — | — |
| `operator_mid` | `int64` | 6 | — | — |
| `trace_id` | `string` | 7 | — | — |

### message `ListWebhookDeliveriesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`WebhookDeliveryInfo`](#message-webhookdeliveryinfo) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |

### message `RetryWebhookDeliveryReq`

> RetryWebhookDeliveryReq 死信重放（运营触发，幂等：同一 delivery 重放会重置 attempt 计数）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `delivery_id` | `int64` | 1 | — | — |
| `operator_mid` | `int64` | 2 | — | 必填 > 0 |
| `ignore_dead` | `bool` | 3 | — | true 允许重放 DEAD 记录 |
| `reason` | `string` | 4 | — | — |
| `trace_id` | `string` | 5 | — | — |

### message `RetryWebhookDeliveryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `delivery_id` | `int64` | 1 | — | — |
| `state` | [`WebhookDeliveryState`](#enum-webhookdeliverystate) | 2 | — | — |
| `next_retry_at` | `int64` | 3 | — | — |
| `replayed` | `bool` | 4 | — | — |
