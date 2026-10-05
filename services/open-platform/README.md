# open-platform

第三方开放平台的领域服务：应用与密钥、scope 目录与审批、OAuth 授权与 token 生命周期、
接口配额、Webhook 回调投递的**数据所有者**。只提供 gRPC（无 `.api`，AGENTS.md §3/§4）。

- **拥有数据**（库 `go_video_open_platform`，12 张表）：
  `op_app`（应用主体 + 状态机 + 乐观锁版本 + 注册幂等键）、`op_app_secret`（client_secret 哈希，一行一把）、
  `op_scope`（权限点目录，声明读/写与风险级别）、`op_app_scope`（应用↔scope 审批关系）、
  `op_auth_code`（短期一次性授权码哈希）、`op_grant`（用户对该应用的授权关系，**兼作撤销位点**）、
  `op_token`（access/refresh 哈希与轮换链）、`op_quota_policy`（限额规则）、
  `op_quota_usage`（用量投影，可重算）、`op_api_call_log`（调用流水，**配额与审计的事实源**，append-only）、
  `op_webhook_endpoint`（回调端点，不存密钥材料）、`op_webhook_delivery`（投递任务与退避状态）。
- **提供能力**：应用注册/查询/改资料/推进状态、密钥轮换与紧急吊销、scope 目录与运营审批、
  授权码签发与消费、token 签发/轮换/撤销、token 校验（Introspect）、网关前置聚合判定
  （凭证 + scope + 配额扣减 + 流水）、配额规则与用量查询与重算、回调端点管理与事件入队与死信重放。
- **依赖**：MySQL、Redis（token 校验短缓存、nonce 防重放集合；真值始终在 MySQL）、
  `account`/`user-profile`（只存 `mid` 主键引用，用户资料不落本库）、`gateway/app`（对外 HTTP 入口、
  签名编排、响应信封、授权页跳转）、`audit`（运营动作审计）、`cron`（配额重算与留存清理的触发方）。
- **约束**：
  - 范围红线（AGENTS.md §1）：目录里**不存在**会员、订单、支付、投币、分成、广告投放/分析类权限点；
    `model.IsForbiddenScopeCategory` 在写入侧硬拦命中禁用词根的 scope，`ListScopes`/`GrantApplicationScopes`
    命中即整调用报错（`ErrForbiddenScopeCategory`），不静默丢行。
  - 凭证不可回显：`client_secret` / 授权码 / access / refresh 明文只在**签发那一次响应**中出现，
    库里只有 `salt + hash`；`ApplicationInfo`、`Introspect`、日志、事件 payload 都不携带凭证材料。
  - 撤销必须立即生效：除逐条标记 `op_token.state` 外，还比对 `op_grant.revoked_at` 位点，
    缓存只允许延后「拒绝」的传播，不允许延后「过期/撤销」。
  - 未接入下游或未注入密钥**不得当成通过**（fail-closed）：`ErrSecretVerificationUnavailable`、
    `ErrQuotaPolicyNotFound`（无生效规则 ≠ 无上限）、`ErrWebhookUnverified` 等哨兵显式报错。

## 本期落地范围

契约（`rpc/openplatform.proto` 24 个方法）+ 数据模型（`model/` 12 表 + 53 个哨兵错误）+
迁移 SQL + 配置装配 + logic 规格注释已完成；逐层用例数字见下面的「测试覆盖」节
（`gofmt`/`go build`/`go vet`/`go test` 的结论只在收口轮当场给出，本节不预先声称它绿）。

- **24 个 RPC 方法全部落地**（含此前剩下的 6 个 Webhook 方法：`RegisterWebhook`/`ListWebhooks`/
  `DeleteWebhook`/`EnqueueWebhookEvent`/`ListWebhookDeliveries`/`RetryWebhookDelivery`），
  `internal/logic/` 已无 `return nil, model.ErrNotImplemented`，也不返回空 Reply 伪装成功（AGENTS.md §9）。
  配额 4 个方法（`UpsertQuotaPolicy`/`ListQuotaPolicies`/
  `ListQuotaUsage`/`RecomputeQuota`）与 `AuthorizeRequest` 的扣减面已按下面的「配额策略」实现。
  每个方法的注释写死了校验次序、幂等键或唯一约束、
  事务边界、缓存与真值的一致性、依赖未配置时的显式错误、错误→gRPC status 映射，
  **门禁顺序本身即契约**（例如 `AuthorizeRequest` 的时间窗→nonce→应用→验签）。
- **3 个迁移文件已在隔离实例（`127.0.0.1:3399`）执行并复验**：库 `go_video_open_platform` 的
  12 张业务表与 `deploy/migrations/open-platform/` 的 `CREATE TABLE` 逐一对上（2026-09-21），
  `deploy/migrations/README.md` 记 `open-platform | go_video_open_platform | 3 | applied`。
  本机 3306 是维护者真实库，全程禁止写入 —— **真实/共享实例仍未执行**，上线前须由运维在目标实例跑；
  model ↔ DDL 的一致性另有逐列比对（见下节）。
- **没有 `internal/consumer`、没有 Outbox**：Webhook 事件目前只能由领域方同步调
  `EnqueueWebhookEvent`，`op_webhook_delivery` 是队列但没人消费（投递 worker 属下一轮）。

## 方法 ↔ 表 ↔ 幂等锚点

| 方法 | 主表 | 幂等锚点（唯一约束/条件更新） | 要点 |
|---|---|---|---|
| `RegisterApplication` | `op_app` + `op_app_secret` | `uniq_register_token`（= `client_token`） | 命中即回放同一应用且 `replayed=true`，**不再签发第二把明文**；`(owner_mid,name)` 由 `uniq_owner_name` 约束 |
| `GetApplication` | `op_app` + `op_app_scope` | 读 | 不回显密钥，只回 `secret_state`/`secret_rotated_at` 投影 |
| `ListApplications` | `op_app` | 读 | `(mtime, app_id)` 倒序游标；`operator=true` 才可见非 ACTIVE |
| `UpdateApplication` | `op_app` | `expected_version` CAS + 状态机 | 版本不符 `ErrConcurrentUpdate`；非法迁移 `ErrInvalidStateTransition`；改状态必须是运营、改资料必须是 owner |
| `RotateApplicationSecret` | `op_app_secret` | 不幂等（每次产生新密钥行），以最后一次为准 | 新密钥先落、旧密钥后置历史；宽限期写 `expires_at`，`grace=0` 当场失效；响应一次性回新明文 |
| `RevokeApplicationSecret` | `op_app_secret` | `status` 生效位 CAS（`MarkAllHistory`） | `secret_id=0` 表示吊销全部；`reason` 必填；只影响签名调用，不影响已签 token（除非再 `RevokeAuthorization`） |
| `ListScopes` | `op_scope` (+`op_app_scope`) | 读 | 带 `app_id` 时回获批状态；命中禁用类目整调用失败 |
| `GrantApplicationScopes` | `op_app_scope` | `idempotency_key` 必填 + `uniq_app_scope` | 授予/回收同行动状态迁移；回收时写 `op_grant.revoked_at` 位点，受影响 token 下次校验即拒 |
| `IssueAuthorizationCode` | `op_auth_code` (+`op_grant`) | `uniq_code_hash` + 每用户每小时签发配额 | `consent_given` 必须 true；scope ⊆ 应用获批集；`redirect_uri` 必须命中白名单；TTL 默认 60s |
| `ExchangeAuthorizationCode` | `op_auth_code` + `op_grant` + `op_token` | `used_at` 由 0→非 0 的 CAS（一行只许成功一次） | 二次消费返回 `ErrAuthCodeUsed` 并累计重放计数；签发时比对 `redirect_uri`/`app_id` 一致 |
| `RefreshAccessToken` | `op_token` + `op_grant` | 旧行 `state` CAS 置 ROTATED，`uniq_refresh_hash` | 旧 refresh 再次出现 → 判定重放 → 撤销整条 grant（保守失效）+ `ErrRefreshReused`；scope 只可收窄 |
| `RevokeAuthorization` | `op_grant` + `op_token` | 位点写入天然幂等（重复撤销回 `applied=false`） | RFC 7009 语义；三种目标：单 token / 该用户对该应用 / 该用户全部第三方授权（改密或风控一键下线） |
| `IntrospectToken` | `op_token` + `op_grant` + `op_app` | 读 + `IntrospectCacheSeconds` 短缓存 | 顺序：token 行状态 → 过期 → **grant 位点（走 DB）** → 应用状态 → scope；只回 active/app_id/mid/scope/exp，不回哈希与盐 |
| `AuthorizeRequest` | `op_api_call_log` + `op_quota_usage` | `uniq_request_id` | 命中即原样重放首次判定，**不重复扣配额、不写第二条流水**；无生效限额规则 → `ErrQuotaPolicyNotFound`（fail-closed） |
| `UpsertQuotaPolicy` | `op_quota_policy` | `uniq_app_api_window(app_id,api_code,window_seconds)` | `app_id=0` 全局默认、`api_code='*'` 该应用全部接口；必须是运营；`limit<=0` 等价禁用 |
| `ListQuotaPolicies` | `op_quota_policy` | 读 | `(mtime, policy_id)` 游标 |
| `ListQuotaUsage` | `op_quota_usage` | 读（投影） | 窗口对齐到 `AlignWindow` 起点 |
| `RecomputeQuota` | `op_api_call_log` → `op_quota_usage` | 覆盖式重写，天然可反复执行 | `dry_run` 只报差异；漂移用 `max_delta` 观测 |
| `RegisterWebhook` | `op_webhook_endpoint` | `uniq_app_event_url(app_id,event_type,callback_url)` | 回 `verification_challenge`，`verified_at=0` 前不投递；URL 必须 https 且非内网/本机（SSRF）；签名密钥**不入库** |
| `ListWebhooks` | `op_webhook_endpoint` | 读 | 只回版本号，不回签名材料 |
| `DeleteWebhook` | `op_webhook_endpoint` + `op_webhook_delivery` | 软删 `deleted_at` CAS | 连带抑制未投递任务并回 `deliveries_suppressed` |
| `EnqueueWebhookEvent` | `op_webhook_delivery` | `uniq_event_endpoint(event_id,endpoint_id)` | 同一 `event_id` 重复入队回 `deduplicated=true`；`payload` 超 `WebhookPayloadMaxBytes` 直接拒；payload 禁含 token/secret/证件/手机号明文 |
| `ListWebhookDeliveries` | `op_webhook_delivery` | 读 | 只回 `payload_digest` 与截断脱敏后的 `last_error` |
| `RetryWebhookDelivery` | `op_webhook_delivery` | `delivery_id` + 状态 CAS | 运营触发，`attempt` 归零重新排程；`SUCCESS` 不得重放（会产生第二次副作用） |

## OAuth 流程与凭证生命周期

本期是 OAuth 2.0 授权码模式的**子集**（无 OIDC：不签发 `id_token`，`TokenSet` 里没有身份声明字段，
也没有发现端点；若要做身份登录必须新增 `openid` scope + 字段并补一轮评审）。

```
开发者                gateway/app                 open-platform                    MySQL
  │  注册应用            │  RegisterApplication(client_token)  ──► op_app + op_app_secret
  │ ◄── app_key + 一次性 client_secret ──┐                        （库里只有 salt+hash）
  │  授权页跳转          │  IssueAuthorizationCode(mid,scope,consent_given=true)
  │                                        ──► op_grant(scope 快照) + op_auth_code(hash, expires_at)
  │  302 回跳 code       │  ExchangeAuthorizationCode(code, redirect_uri)
  │                                        ──► code.used_at 0→非0 CAS
  │                                        ──► op_token(access_hash+refresh_hash, state=ACTIVE)
  │                                        ──► op_grant.current_token_id 前移
  │  带 Bearer 调开放 API │  AuthorizeRequest / IntrospectToken ──► 判定链 + 配额扣减 + op_api_call_log
  │  过期后续期           │  RefreshAccessToken ──► 旧行 ROTATED → 新行 → parent_token_id 链
  │  撤销                 │  RevokeAuthorization ──► op_grant.revoked_at 位点 + 逐条标记 op_token
```

三档时效与失效口径：

| 凭证 | TTL 来源 | 失效判定 | 明文可见处 |
|---|---|---|---|
| `client_secret` | `SecretValidDays`（0=长期，靠轮换/吊销） | `status` 生效位 + `expires_at` 宽限期（`AppSecret.Usable`） | 仅 `RegisterApplication`/`RotateApplicationSecret` 响应各一次 |
| 授权码 | `AuthCodeTTLSeconds`，`(0,600]` 秒（Validate 强制短期） | `expires_at` 或 `used_at != 0`（一次性） | 仅 `IssueAuthorizationCode` → 302 回跳 |
| access token | `AccessTokenTTLSeconds`（默认 1h） | 行 `state` → `access_expires_at` → **`grant.revoked_at` 位点** → 应用状态 → scope | 仅签发响应 |
| refresh token | `RefreshTokenTTLSeconds`（默认 30d） | 同上，且重放即撤销整条 grant | 仅签发响应 |

**为什么撤销能立即生效**：`op_grant.revoked_at` 是「位点」，凡 `op_token.ctime <= revoked_at`
一律拒绝（`Grant.TokenGranted` 实现）。网关即便握着 `IntrospectCacheSeconds` 的缓存结果，
缓存也只延后「拒绝」的传播，位点比对始终走 DB，且该值被 `Validate` 约束为
`<= AccessTokenTTLSeconds`——过期时间永远是硬上限。

## 哈希口径（两处刻意不同，必须对齐读路径）

| 场景 | 算式 | 为什么 |
|---|---|---|
| 按值定位的凭证：授权码、access、refresh | `HMAC-SHA256(pepper, 明文)`（每行仍存 `salt` 供审计与将来加盐升级） | 校验入口是 `WHERE access_hash = ?`，算式里若掺入随机 salt 就无法由明文反查唯一键，`uniq_access_hash`/`uniq_code_hash` 也就失去意义 |
| 按 `app_id` 定位的凭证：`client_secret` | `HMAC-SHA256(pepper, salt \|\| secret)` | 定位键是 `app_id`，不需要由值反查，掺 salt 让同一把 secret 在两行里哈希不同，抗跨行比对 |
| Webhook 投递签名 | `signKey = HMAC(WebhookMasterPepper, app_id \|\| key_version)`，**派生不落库** | 表里只有 `sign_key_version`；库泄露既拿不到历史签名密钥也伪造不了合法回调 |

`CredentialPepper` / `WebhookMasterPepper` 只从 Secret/Vault 注入，示例配置必须留空
（`internal/config/config_load_test.go` 断言 yaml 无 pepper、无口令，且 `DataSource` 指向
`go_video_open_platform`）。留空时 `svc` 不阻断启动（服务还能提供只读能力），
但所有验签/哈希路径返回 `ErrSecretVerificationUnavailable`，**不退化成「无 pepper 比较」或放行**。
与 `services/account` 的差异已在 `op_app_secret` 的 DDL 注释里显式记录：account 沿用历史
MD5 以兼容 passport 客户端，本域无历史包袱，直接用 salt + HMAC-SHA256。

## 应用级签名（`AuthorizeRequest` 的 signature 模式）

`signature = HMAC-SHA256(client_secret, canonical)`，canonical 定死为
`method \n path \n timestamp \n nonce \n api_code \n sha256(body_digest)`：
时间窗 `|now-timestamp| <= SignatureSkewSeconds`（超窗 `ErrSignatureExpired`）→
nonce 在 Redis `SETNX`（TTL `NonceTTLSeconds`，`Validate` 强制 `>= 2*Skew`，否则窗口内可重放）→
应用必须 ACTIVE → 候选密钥取宽限期内新旧两把、恒定时间比对。
当前 `model/` 还**没有**这套纯函数（见已知缺口），跨语言客户端需要的规范化细节
（空串占位、百分号编码、大小写）必须在实现时定死并配不连库单测。

## 配额策略

- 层级四档，**取第一个非空层级、同层级多窗口同时限流、不跨层相加**（`model.NarrowPolicies`）：
  本应用+精确 api → 本应用+`*` → 全局+精确 api → 全局+`*`。跨层相加会让运营放开的限额被全局收紧，行为不可解释。
- `op_api_call_log` 是真值（append-only，`uniq_request_id` 幂等），`op_quota_usage` 是投影：
  `Add` 原子自增、`RecomputeOverwrite` 从流水按 `AlignWindow(now, window)` 覆盖重算。
  投影丢失只会**短时放宽**限额，不会误拒，也不会影响业务数据。
- 窗口算式与 SQL 侧同口径：`AlignWindow(ts, w) == FLOOR(ctime/w)*w`，`pure_test.go` 把这条钉住，
  否则重算永远对不上。
- `000003` seed 了 `app_id=0, api_code='*'` 的全局兜底规则：任何请求都至少落进一层限额，
  「没有规则」一律 `ErrQuotaPolicyNotFound` 而不是「没有上限」。
- 写面口径（`UpsertQuotaPolicy`）：只写生效规则（`enabled=1`），**停用一律走 `QuotaPolicies.Disable`**
  ——本 RPC 没有 `reason` 字段，而关闭限额必须留问责原因，因此 `enabled=false` 直接拒（`errReasonRequired`）。
  `quota_limit=0` 且 `enabled=1` 等价「禁用该接口」（`Denied()` 语义），负数拒（`ErrQuotaLimitInvalid`）。
- 读面口径（`ListQuotaUsage`）：`limit` 回显**当前生效限额**而不是 `limit_snapshot`
  （快照只解释「当时为什么被拒」，两者不同就意味着限额漂移）；层级判定与扣减面共用
  `effectiveQuotaPolicies`（`ListCandidates` + `NarrowPolicies`），保证「看到的限额」= 「实际扣的限额」。
  `api_code` 为空的汇总视图以**规则目录**为枚举源（最多 `quotaUsageAPICodeLimit` 个接口，超出显式报错，
  不做静默截断）；`'*'` 条目的 `used` 恒为 0，因为窗口行的 `api_code` 列记的是请求里的真实接口名。
- 重算口径（`RecomputeQuota`）：只接受 `operator_mid>0` 且 `app_id>0`——冻结的 model 读法在
  `app_id<=0` 时会把所有应用并进同一个窗口桶，用它覆盖单应用行会打穿限额语义，所以
  「全部应用」由 cron 逐应用调用本方法实现；`api_code='*'`（或空）按规则目录枚举具体接口，
  `'*'` 本身不作为重算目标。区间长度上限 = `QuotaRecomputeLookbackSeconds × 4`、
  单次窗口数上限 2000，超出分别报 `errRecomputeRangeTooWide` / `errRecomputeTooManyWindows`；
  被区间切断的跨界窗口单独按 `[window_start, window_start+window)` 取流水数，避免越重算越偏。

## Webhook 重试策略

`PENDING → DELIVERING(租约 `WebhookLeaseSeconds`，崩溃后可被抢占) → SUCCESS | RETRY_SCHEDULED | DEAD`
，另有 `IGNORED`（人工忽略，终态）。
退避 `next_retry_at = now + min(base * 2^(attempt-1), max)`（`model.NextRetryAt`，配置为 0 时退回 30/3600 安全默认），
超过 `WebhookMaxAttempts` 进 `DEAD` 等运营 `RetryWebhookDelivery`（`attempt` 归零）。
`verified_at=0` 的端点不参与任何投递；正文按 `WebhookPayloadRetentionDays` 清空
`payload` 列、保留 `payload_digest` 与投递结论（清空后无法重放原文，需上游重新生产事件）。

## 表与迁移文件

| 迁移文件 | 表 |
|---|---|
| `deploy/migrations/open-platform/000001_create_open_platform_app_tables.sql` | `op_app`、`op_app_secret`、`op_scope`、`op_app_scope`（+ 开放 scope 目录 seed，幂等：只覆盖描述列，不覆盖 `enabled`/`disable_reason`） |
| `deploy/migrations/open-platform/000002_create_open_platform_authorization_tables.sql` | `op_auth_code`、`op_grant`、`op_token` |
| `deploy/migrations/open-platform/000003_create_open_platform_quota_webhook_tables.sql` | `op_quota_policy`、`op_quota_usage`、`op_api_call_log`、`op_webhook_endpoint`、`op_webhook_delivery`（+ 全局默认限额 seed） |

幂等键与唯一约束逐条核对结果（`INSERT ... ON DUPLICATE KEY UPDATE` 与「按值反查」路径全部有对应索引）：
`uniq_register_token`、`uniq_owner_name`、`uniq_app_key`、`uniq_app_scope`、`uniq_code_hash`、
`uniq_app_mid`、`uniq_access_hash`、`uniq_refresh_hash`、`uniq_app_api_window`、`uniq_window`、
`uniq_request_id`、`uniq_app_event_url`、`uniq_event_endpoint`，`op_scope` 主键即 `scope`。
逐列比对（名称/类型/可空/默认值）**model 有而 SQL 缺 = 空集，SQL 有而 model 不用 = 空集**，
因此本轮无需 `000004_*.sql`。未被读路径使用的索引/列已记录在下面「已知缺口」里。

## 配置

```yaml
Name: openplatform.v1.rpc
ListenOn: 0.0.0.0:8151          # 批次 D1 分配；8080 是 gateway/app 的 HTTP 端口
Etcd: {Hosts: [127.0.0.1:2379], Key: openplatform.v1.rpc}
CacheRedis: {Host: 127.0.0.1:6379, Type: node}   # 不能叫 Redis：RpcServerConf 内嵌同名 RedisKeyConf
DataSource: root@tcp(127.0.0.1:3306)/go_video_open_platform?charset=utf8mb4&parseTime=true&loc=Local
                                  # 示例 DSN 口令留空：真实口令走配置中心/Secret，
                                  # 或按 docs/commands.md §8 用 scripts/migrate.ps1 的 -OverrideUser/-OverridePassword
OpenPlatform: {AuthCodeTTLSeconds: 60, AccessTokenTTLSeconds: 3600, RefreshTokenTTLSeconds: 2592000,
               SecretValidDays: 0, RotationGraceSeconds: 3600, PageSize: 20, MaxPageSize: 50,
               MaxRedirectURIs: 5, MaxRedirectURIBytes: 512, MaxWebhookEndpointsPerApp: 20,
               WebhookMaxAttempts: 6, WebhookRetryBaseSeconds: 30, WebhookRetryMaxSeconds: 3600,
               WebhookLeaseSeconds: 60, WebhookHTTPTimeoutMS: 5000, WebhookPayloadMaxBytes: 32768,
               WebhookPayloadRetentionDays: 30, TokenRetentionDays: 90,
               AuthCodeMaxPerUserPerHour: 30, QuotaRecomputeLookbackSeconds: 7200,
               IntrospectCacheSeconds: 5, PostQps: 400, PostBurst: 100}
Security: {CredentialPepper: "", WebhookMasterPepper: "", KeyVersion: 1,
           SignatureSkewSeconds: 300, NonceTTLSeconds: 600}
```

`Config.Validate()` 在 `svc.NewServiceContext` 里调用，「能加载但语义危险」的组合直接 `Severe` 终止启动：
授权码不是短期（`<=0` 或 `>600`）、`RefreshTokenTTLSeconds <= AccessTokenTTLSeconds`、
`IntrospectCacheSeconds > AccessTokenTTLSeconds`（缓存只延后拒绝，不得延后过期）、
`MaxPageSize < PageSize`、Webhook 重试参数不成序、`WebhookLeaseSeconds <= 0`（投递中状态无法回收）、
`WebhookPayloadRetentionDays/TokenRetentionDays <= 0`（凭证与正文永不清理）、
`KeyVersion <= 0`、`SignatureSkewSeconds <= 0`、`NonceTTLSeconds < 2*SignatureSkewSeconds`（窗口内可重放）。
`*_Seconds`/`*_MS` 一律 `int64`（计数/天数/页大小用 `int`/`int32`），避免 TTL 之间跨类型比较。

## 运行与测试

```powershell
go run ./services/open-platform -f services/open-platform/etc/openplatform.v1.yaml
powershell -File scripts/gen.ps1 -Service open-platform   # 契约变更后重新生成；禁止手改 rpc/*.pb.go、internal/server
```

- `go test -p 1 -count=1 ./services/open-platform/...` **不连接 MySQL/Redis/etcd**；
  用例清单、替身口径与覆盖边界见下节「测试覆盖」。
- 53 个哨兵错误文案带 `open-platform:` 前缀，供 `gateway/app` 用 `errors.Is` 映射成响应信封 `code`；
  `allowed=false` 是业务结果（gRPC OK），只有依赖故障才回错误码。

## 测试覆盖

离线单测（内存版 model + 假事务，不连 MySQL / Redis / etcd / 对象存储，也不需要网络）。
数字为 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。
规模合计 **163 顶层 + 129 子用例**（logic `147/128`、model `11/0`、config `5/1`），
本服务 0 条用例处于 `t.Skip` 状态。

### 1. `internal/logic`（10 个文件含 `fakes_test.go`）— `147/128`

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `quota_test.go` | 26/13 | 配额四法（`UpsertQuotaPolicy`/`ListQuotaPolicies`/`ListQuotaUsage`/`RecomputeQuota`）：规则是限额真值（响应逐字段==入库行、同唯一键原地更新保留 `policy_id`/`ctime` 且只有一行）；`limit=0` 合法而负数是参数错、`enabled=false` 因无 `reason` 通道必须失败关闭；`policy_id` 只是定位辅助（指向不同行或不存在一律拒）；生效层级只由 `model.NarrowPolicies` 决定，读面回显**当前生效限额**而非 `limit_snapshot`，窗口起点与扣减面共用 `AlignWindow`；用量来自 `op_quota_usage` 的 `Peek`，既不聚合流水也绝不扣减；重算以 `op_api_call_log` 为真值（幂等、`dry_run` 真的不落库、不改规则行、跨界窗口按整窗口计数）；失败路径 `wantNoWrites`，两处并发抢跑各只留一行 |
| `oauth_flow_test.go` | 23/46 | OAuth 五法的安全语义：`redirect_uri` 签发期逐字命中白名单（前缀/加路径段/加查询串都算 miss）、换码期不一致**绝不消耗 code**；授权码一次性（重放、过期、并发只有一方成功且留下 `replay_count`）；scope 只能收窄不能放大（请求 ⊆ 应用获批 ⊆ 目录开放）；非 ACTIVE 应用一律拒（`requireActiveApp` 唯一实现）；`Introspect` 判定完全走 DB 真值（撤销/过期/换主密钥立刻反映，缓存不参与结论）；撤销按目标与归属收紧、可重入且不再外呼；失败路径零副作用 |
| `authorize_request_test.go` | 14/30 | 网关前置聚合判定：判定链与 `IntrospectToken` **共用同一张库状态表**（两门漂移即绕过）；拒绝原因只能取固定原因码集合，不外泄下游错误文本；放行必须同时完成配额扣减 + 一条流水且响应==入库真值（每条生效规则各扣一次，多窗口不短路）；`request_id` 是硬幂等锚点（命中即重放首次判定，不再读凭证/扣配额/写流水，并发抢跑只认先落库者）；fail-closed——任何 model 错误都以 gRPC error 返回，绝不折算成 `allowed`；参数与凭证模式门禁零副作用，写令牌桶无余量时限流发生在扣配额之前；流水只落摘要与脱敏 IP（明文 access/refresh、完整 `client_ip` 全库找不到） |
| `app_lifecycle_test.go` | 19/14 | 应用生命周期四法：注册幂等（`client_token` 命中回放同一应用且**不再签发第二把明文**）、明文只在首次响应出现一次、`SecretValidDays` 配置驱动到期、注册后停在 PENDING_REVIEW；改资料与推状态**两条通道分离**且各自门禁（owner vs operator）、`expected_version` CAS；详情读归属门禁 + 脱敏投影（只回 `secret_state`/`secret_rotated_at`）；列表分页边界 + 批量投影不产生 N+1；`drainWriteBucket` 证明限流发生在任何写之前 |
| `app_read_scope_test.go` | 18/4 | 读面与审批：列表每字段==入库真值（含派生 `scopes`/`secret_state`）且不含凭证材料；游标位点严格有序、页与页不重叠不遗漏、空结果回空数组；`ps` 夹取两侧都钉（`MaxPageSize` 允许、+1 才拒、未传走默认）；目录读面停用项在 `only_enabled=false` 时必须可见、未指定 `app_id` 时 `granted_state` 恒 0、已回收关系行回到 0；审批只认「目录里存在且 enabled=1」的 scope，身份门禁是 `operator_mid>0` 而不是请求里的布尔位；授予/回收对同一 `uniq (app_id, scope)` 行原地更新、无效回收零副作用且不前移 `app.version` |
| `webhook_delivery_test.go` | 17/0 | 投递链路三法：扇出规模 `matched` == 实际入队行数（未验证/停用/已删/订别的事件一律不计，且不得入队前预写）；`(event_id, endpoint_id)` 重放只回既有 ID 且不覆盖已入库正文、新增端点只补那一行；正文门禁按 `WebhookPayloadMaxBytes` 边界两侧各测一次、凭证键名与 PII 拒整条而不是截断；台账只回 `payload_digest`；**复合序 `(ctime DESC, delivery_id DESC)` 用与主键不同向的时间 + 同 `ctime` 行对真实钉住**；重放只承认 DEAD/IGNORED 且 DEAD 需 `ignore_dead`，`attempt` 归零但 `max_attempts` 是入队快照不得抬高；重放与 worker 抢占互斥（CAS 落败方回库里真值 DELIVERING）；内部撤销通知尽力而为 |
| `webhook_test.go` | 15/0 | 端点三法：响应字段逐条等于入库真值（`sign_key_version` 尤其不得回显「配置当前版本」）；验证挑战明文只在首次登记响应出现一次、重放回空串；回调地址护栏与 `redirect_uri` 同源（非 https/userinfo/fragment/内部主机名/回环私网 CGNAT 元数据 IP 全拒，**公网字面 IP 必须放行**否则护栏退化成全拒而自证通过）；新登记 `verified_at=0` 绝不因 ctime 存在就可投递；条数上限不吃掉幂等回放；删除与「抑制在途任务」同一次 `TransactCtx`、重复删除回 `deleted=false`；归属与身份先于任何读写，越权与不存在同口径回 `ErrWebhookNotFound` |
| `rotateapplicationsecretlogic_test.go` | 10/9 | `client_secret` 生命周期两法：轮换宽限期语义（`grace_seconds=0` 立即失效、>0 到点失效、本方法不幂等以最后一次为准）、吊销只影响签名调用**绝不影响已签发 token**、`reason` 必填、明文只在新密钥返回值出现一次（库里只有 salt+hash）、门禁失败与依赖故障零副作用、连续两次轮换的链条有界 |
| `refreshaccesstokenlogic_test.go` | 5/12 | `RefreshAccessToken`：旧 refresh 立即置 ROTATED、旧值再出现判定重放并撤销整条 grant（保守失效）、scope 只可收窄、停用应用一律拒 |
| `fakes_test.go` | 0/0（替身层） | 见第 4 组 |

### 2. 其他层（同口径实测）

- `model/` — **1 文件 `11/0`**：`pure_test.go` 只覆盖不连库的纯函数（logic 判定的唯一算式来源）：
  `JoinScopes`/`SplitScopes` 去重升序与往返、scope 精确包含而非前缀、`AlignWindow` 与 SQL `FLOOR` 同口径、
  `NarrowPolicies` 四档优先级与不跨应用泄漏、`NextRetryAt` 退避与上限与 `attempt=0`、
  `CanTransitionAppStatus` 合法/非法/未定义一律拒、`Grant.TokenGranted` 撤销位点、
  `Token.AccessUsable/RefreshUsable`、`AuthCode.Usable`、`AppSecret.Usable` 宽限期、
  `QuotaUsage.Remaining/Exceeded/ResetAt`、`WebhookEndpoint.Deliverable`、
  `WebhookDelivery.Retryable/ManuallyReplayable`、`IsForbiddenScopeCategory` 商业化红线。
- `internal/config` — **1 文件 `5/1`**：`config_load_test.go` 用 `conf.Load` 真实加载 `etc/` 每个 yaml，
  反射遍历断言不存在字段名 `Redis`（`CacheRedis` 必须是唯一 `redis.RedisConf` 字段）、
  yaml 无 pepper/无口令、`DataSource` 指向 `go_video_open_platform`，并跑 `Validate()`。
- **`internal/repository` 层不存在**（`svc.ServiceContext` 直接注入 12 个 model，见「已知缺口」），
  因此没有该层用例；`internal/consumer` 亦不存在（无 Outbox、无投递 worker，见「已知缺口」）。
- `internal/svc/` **无离线单测**；`internal/server/`、`rpc/openplatform.pb.go` 是 goctl/protoc 生成壳，
  不在单测范围。

### 3. 构造器级覆盖

`internal/logic` 的 24 个 RPC 构造器 **24/24**（探针 `PROBE 24 gaps:` 后为空，无缺口名字），
即 proto 的 24 个方法都有直接调用其构造器的用例。

### 4. 替身层与断言口径

`internal/logic/fakes_test.go` 提供「内存版 model」与「假事务」，注入合法（`svc.ServiceContext` 的
model 字段全是接口类型，测试直接赋值）。它复刻的是**语义**而不是并发：唯一键命中即 `created=false`、
CAS 条件不命中即 `applied=false`、`revoked_at` 一旦写下不可回退、`FindActive` 按 `secret_id` 倒序；
真正的并发正确性建立在 MySQL 的 `uniq_*` 索引与条件 UPDATE 上，单测不复刻锁。
每个 fake 只内嵌接口并覆写被测路径用到的方法，其余方法由内嵌 nil 接口提升——测试走到未实现的方法会
当场 panic（响的失败），不会被悄悄写成「通过」。`TransactCtx` 在入口快照内存态、回调报错时整体回滚，
因此能真断「事务中途失败不留半成品行」；**调用计数不参与回滚**（它记录尝试次数），所以副作用断言
一律「计数增量 + 数据状态」两条一起看。`Cache` 恒为 nil，而判定链设计上不依赖缓存（`tokencheck.go` 头），
因此测试路径与线上一致。

证明不了的：SQL 文本、列名、占位符与索引命中；`INSERT ... ON DUPLICATE KEY UPDATE` 的真实 affected 口径；
Redis SETNX 的 nonce 防重放与真实 TTL；HMAC 验签所需的 canonical 纯函数尚不存在（见「已知缺口」）。

### 5. 覆盖边界

- 用例不连接 MySQL / Redis / etcd / 对象存储，也不发起任何 HTTP 外呼。
- **Webhook 投递没有端到端覆盖**：`op_webhook_delivery` 的 PENDING 行无人消费（无 `internal/consumer`、
  无投递 worker），实际 HTTP 外呼、退避重试、死信收敛都在覆盖之外；`verified_at` 因契约缺
  `VerifyWebhook` 而没有合法写入入口。
- **`AuthorizeRequest` 没有真实流量入口**：设计上由对外网关宿主前置调用，该宿主缺位
  （`gateway/app` 仍未接），配额扣减面只有单测与 admin 的读/重算口在跑。
- 迁移 SQL **已在隔离实例 `127.0.0.1:3399` 执行并复验**（见「本期落地范围」一节：3 个迁移 ↔ 12 张表逐一对上，
  2026-09-21，`deploy/migrations/README.md` 记 `applied`）；**真实/共享实例仍未执行**，
  上线前须由运维在目标实例跑 000001~000003。`model/` 也没有连库的列级门禁，一致性靠本轮记录的逐列比对结论。
- `internal/server`、`rpc/*.pb.go`、入口文件与 `types` 不在单测范围内。

### 6. 验证命令

```bash
go test -p 1 -count=1 ./services/open-platform/...
go vet ./services/open-platform/...
gofmt -l services/open-platform          # 必须无输出
```

`-p 1` 是硬要求：Windows 页面文件限制下并发编译/运行多个测试包会 OOM（`errno=1455`），
测试门禁一律串行跑包（见 docs/commands.md）。

## 消费方（谁在调本服务）

- **`gateway/admin`（2026-09-22 接入）**：`/admin/open-platform/*` 共 **16 条**运营路由，全部挂
  `AdminPermission` 判定；客户端字段 `OpenPlatformRPC`（etcd key `openplatform.v1.rpc`），
  未配置时该域路由一律显式失败而不是回空目录。24 个方法里剩下 **8 个刻意不开后台路由**：
  `RegisterApplication`/`RegisterWebhook`（归属者注册，后台不代替开发者表达意愿）、
  `IssueAuthorizationCode`/`ExchangeAuthorizationCode`/`RefreshAccessToken`/`IntrospectToken`/
  `AuthorizeRequest`（用户凭证的签发、换发、探测与网关前置判定，不是运营职权）、
  `EnqueueWebhookEvent`（回调事实由领域事件注入）。逐条口径与路由表见
  `gateway/admin/api/admin.api` 的 open-platform 段与 `gateway/admin/README.md`。
- **`gateway/app`**：仍未接。对外开放 HTTP 面（`/oauth/*`、`/open/v1/*`）落 `gateway/app`
  还是独立 `gateway/open` 子服务属待评审项（AGENTS.md §3 不允许新增顶层替代目录），见 docs/roadmap.md。
- **本服务对自身 RPC 的依赖**：`AuthorizeRequest` 设计上由**对外网关宿主**在每次开放 API 调用前置调用
  （验签 + scope + 配额扣减 + 流水），该宿主目前缺位，因此配额扣减面在线上没有真实流量入口，
  只有 `gateway/admin` 的读/重算口和单测在跑。

## 已知缺口 / 待评审

- **Webhook 只有「台账与门禁」，没有投递 worker**：6 个方法已落地（注册/列表/软删抑制/入队/流水/人工重放），
  但 `op_webhook_delivery` 的 PENDING 行没人消费 —— 实际 HTTP 外呼、退避重试、死信收敛属下一轮。
  另两处缺口：契约没有 `VerifyWebhook`（注册期下发的 challenge 无回传入口，`MarkVerified` 无合法调用方，
  故重复注册时 `verification_challenge` 恒回空串）。
  测试轮已落地 `webhook_test.go` `15/0` 与 `webhook_delivery_test.go` `17/0`，
  `payloadCarriesPII`（两条入队入口共用同一扫描、内部路径不得成后门）、软删幂等与
  「端点已停但任务未抑制」的收敛窗口、重放门禁都有逐条断言，明细见「测试覆盖」节第 1 组；
  本条早先登记的「按指令未写测试」「fake 只有 `ListMatching`/`Insert` 两条、其余会命中内嵌 nil 接口 panic」
  两句都已过期——`CountByApp`/`FindByID`/`ListByApp`/`SoftDelete`/`ListByCursor`/`ResetForReplay`/
  `SuppressByEndpoint` 现在都有替身实现（实测逐个能在 `internal/logic/*_test.go` 看到声明）。
  **仍成立的缺口**是投递 worker 本体与真实 HTTP 外呼：用例只钉台账与门禁，没有任何用例把消息发出去过。
- **没有 `internal/repository` 层**
  （`svc.ServiceContext` 直接注入 12 个 model，与 `live-room`/`private-message` 同风格），
  跨表事务（授权码换 token、轮换、scope 回收连带位点）的编排暂放 logic 里，编排重复再抽层。
- **缺 HMAC / canonical 签名纯函数**：`AuthorizeRequest` 与 Webhook 投递验签需要的
  「签名串规范化 + HMAC-SHA256 + 恒定时间比对」在 `model/` 里还没有实现（logic 注释已标为待落地），
  落地时必须配不连库单测锁死跨语言口径。
- **缺 `VerifyWebhook` RPC**：`RegisterWebhook` 返回 `verification_challenge`，但契约里没有
  「应用回显 challenge 完成验证」的方法，`verified_at` 因此没有合法的写入入口。
  这是 proto 契约缺口，需新增方法后 `scripts/gen.ps1` 重新生成（本轮不手改生成物）。
- **`TokenRetentionDays` 无法闭环**：`TokenModel.PurgeByIDs` 已有，但缺少「列出已过期 token 主键」的
  查询（`idx_state_expires` 已具备），cron 侧清理跑不起来；同理 `WebhookPayloadRetentionDays` 的
  `PurgePayloadBefore` 在终态行量大时缺 `(state, ctime)` 组合索引（属观测项，本轮不加迁移）。
- **无消费者/无投递 worker/无 Outbox**：`internal/consumer` 目录不存在，
  `op_webhook_delivery` 的 `Claim` 无人调用，对外回调实际不会发生。
- **未被读路径使用的索引/列**（保留不影响正确性，后续按需清理或补齐）：
  `op_auth_code.idx_app_mid_ctime`（签发限频只按 `(mid,ctime)` 查）、
  `op_api_call_log.idx_token`（流水当前只按 `request_id` 与 `(app_id,api_code,ctime)` 检索）、
  `op_scope.idx_enabled_access` 的 `access` 列部分（目录筛选只用 `enabled`）、
  以及 `op_grant.last_code_id`（审计链「grant ← code ← token」尚未被任何写入路径填）。
- **契约缺口（接入 `gateway/admin` 时发现，只能在服务侧修，网关已按实现而不是注释立闸）**：
  1. **运营主体与用户主体共用 mid 空间**：本域所有操作者位都是 `operator_mid` / `caller_mid`
     （`op_admin_user.admin_id` 是另一个编号空间），网关因此不能用会话身份覆盖它们，只能要求表单
     自报 mid + 双主体日志（和 `/admin/live` 同一缺口）。要真正闭环需 proto 补管理员主体
     （`admin_id` 字段或统一 `Operator` 消息）。
  2. `ListApplicationsReq` **没有任何主体位**（运营分支只看 `operator` 布尔），因此「谁翻了这一页应用
     列表」在服务侧落不下证据；本域唯一没有审计主体的读口就是它。
  3. **没有 `DisableQuotaPolicy`**，而 `UpsertQuotaPolicyReq` 没有 `reason` 位，服务对 `enabled=false`
     fail-closed（停用限额必须留问责原因）——于是「在后台关掉一条限额规则」这条运营动作**没有可执行路径**，
     网关只能就地返回说得清的错，不能折算成成功。
  4. `UpdateApplicationReq.expected_version` **只在 owner 资料通道生效**：运营状态通道走
     `UpdateStatus(... WHERE status=from)` 的按状态 CAS（`applyStatus`），版本位被忽略（末位实参硬编 0），
     因此后台连点两次「下架」是后一次覆盖前一次，而不是乐观锁冲突。要让状态迁移也吃版本，
     需要 model 侧把 `expected_version` 传进 `UpdateStatus`。
  5. `RecomputeQuotaReq.app_id` 的 **proto 注释与实现分歧**：注释写「0 = 全部应用」，实现要求
     `app_id>0`（冻结的 model 读法会把所有应用并进同一个窗口桶，覆盖单应用行会打穿限额语义，
     「全部应用」由 cron 逐应用调用实现）。网关按**实现**立闸并在契约注释里记录分歧；改注释或改实现
     都可，但不能继续留着互相矛盾的两者。
- **`gateway/app` 仍无开放平台路由**（对外 OAuth / 开放 API 宿主未定，见上节与 docs/roadmap.md）；
  `gateway/admin` 的 16 条运营路由已于 2026-09-22 接入。
- **迁移已在隔离实例复验**（`deploy/migrations/README.md` 记 `open-platform | go_video_open_platform | 3 | applied`）；
  **真实/共享实例仍未执行**，上线前须由运维在目标实例跑 000001~000003。
