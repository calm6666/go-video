# 运营面 · `/admin/open-platform`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 阶段 4 运营面：open-platform 域（第三方应用、scope、配额与回调） | 免鉴权 | 1 |
| 阶段 4 运营面：open-platform 域（第三方应用、scope、配额与回调） | AdminPermission | 15 |

合计 **16** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 阶段 4 运营面：open-platform 域（第三方应用、scope、配额与回调）（免鉴权，1 条）

> -------------------- open-platform 纯目录读（不进 routePermissions） --------------------

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/open-platform/scope/list` | scope 目录读（可带 app_id 回该应用对每条的获批状态；契约无操作者位） | `openScopeList` | `openscopelistlogic.go` |

### POST `/admin/open-platform/scope/list` — scope 目录读（可带 app_id 回该应用对每条的获批状态；契约无操作者位）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/openscopelisthandler.go`
- 业务实现：`gateway/admin/internal/logic/openscopelistlogic.go`

请求：`ParamOpenScopeList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 否 | — | 0 = 只要目录本身（granted_state 整列恒 0） |
| `OnlyEnabled` | `only_enabled` | json | `bool` | 否 | — | true = 只回开放中的 scope |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenScopeListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenScopeListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 阶段 4 运营面：open-platform 域（第三方应用、scope、配额与回调）（AdminPermission，15 条）

> -------------------- open-platform 受保护面（凭证面读 + 全部写） --------------------
> 15 条：六条读（应用详情/列表、端点列表、配额规则与用量、投递流水）+ 九条写。
> 读挂判定的理由见「读侧参数」段头；写的每一条都改第三方能做什么或谁还能用。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/open-platform/application/list` | 应用台账分页（运营全量分支；按开发者筛属开发者侧，见契约缺口） | `openplatform:application` / `read` | `openApplicationList` | `openapplicationlistlogic.go` |
| POST | `/admin/open-platform/application/get` | 单个应用详情（含回调白名单与密钥状态；不存在与无权看服务回同一错误，网关不折叠成 found=false） | `openplatform:application` / `read` | `openApplicationGet` | `openapplicationgetlogic.go` |
| POST | `/admin/open-platform/application/state` | 推进应用状态机（只有状态：资料改动在运营通道被服务直接拒） | `openplatform:application` / `state` | `openApplicationState` | `openapplicationstatelogic.go` |
| POST | `/admin/open-platform/secret/rotate` | 轮换应用密钥（新明文仅此一次返回且不进日志；旧密钥宽限期由服务落地） | `openplatform:secret` / `rotate` | `openSecretRotate` | `opensecretrotatelogic.go` |
| POST | `/admin/open-platform/secret/revoke` | 吊销应用密钥（secret_id=0 = 全部生效密钥；token 不受影响，要撤授权走 /authorization/revoke） | `openplatform:secret` / `revoke` | `openSecretRevoke` | `opensecretrevokelogic.go` |
| POST | `/admin/open-platform/scope/grant` | scope 授予/回收（部分授予是正常结果，rejected 列表原样回；幂等键必填） | `openplatform:scope` / `grant` | `openScopeGrant` | `openscopegrantlogic.go` |
| POST | `/admin/open-platform/authorization/revoke` | 撤销授权（TOKEN/GRANT/USER_ALL 三种范围；必填组合由服务判，明文 hint 不进日志） | `openplatform:authorization` / `revoke` | `openAuthorizationRevoke` | `openauthorizationrevokelogic.go` |
| POST | `/admin/open-platform/quota/policy/list` | 配额规则目录分页（服务侧 requireOperator；app_id=0 是全局兜底层级、api_code=「*」是通配规则本身） | `openplatform:quota-policy` / `read` | `openQuotaPolicyList` | `openquotapolicylistlogic.go` |
| POST | `/admin/open-platform/quota/policy/upsert` | 新增/更新配额规则（policy_id=0 = 唯一键新建；enabled=false 在本入口无路径——proto 未暴露停用方法） | `openplatform:quota-policy` / `upsert` | `openQuotaPolicyUpsert` | `openquotapolicyupsertlogic.go` |
| POST | `/admin/open-platform/quota/usage/list` | 配额用量读（按应用；投影可漂移，跨应用汇总不在本契约） | `openplatform:quota` / `read` | `openQuotaUsageList` | `openquotausagelistlogic.go` |
| POST | `/admin/open-platform/quota/recompute` | 从调用流水重算配额投影（dry_run 先看差异；投影重算幂等故契约无幂等键位） | `openplatform:quota` / `recompute` | `openQuotaRecompute` | `openquotarecomputelogic.go` |
| POST | `/admin/open-platform/webhook/list` | 回调端点列表（地址属外部主体凭证面；软删行永远不回） | `openplatform:webhook` / `read` | `openWebhookList` | `openwebhooklistlogic.go` |
| POST | `/admin/open-platform/webhook/delete` | 删除回调端点并抑制未投递任务（本域刻意没有新增/改地址入口） | `openplatform:webhook` / `delete` | `openWebhookDelete` | `openwebhookdeletelogic.go` |
| POST | `/admin/open-platform/webhook/delivery/list` | 投递流水分页（只有 payload_digest 与脱敏错误；payload 正文不经本 RPC 外发） | `openplatform:delivery` / `read` | `openWebhookDeliveryList` | `openwebhookdeliverylistlogic.go` |
| POST | `/admin/open-platform/webhook/delivery/retry` | 死信重放（只重置既有记录的 attempt，不注入新事件） | `openplatform:delivery` / `retry` | `openWebhookDeliveryRetry` | `openwebhookdeliveryretrylogic.go` |

### POST `/admin/open-platform/application/list` — 应用台账分页（运营全量分支；按开发者筛属开发者侧，见契约缺口）

- 权限口径：AdminPermission · 权限点 `openplatform:application` / `read`
- goctl 入口：`gateway/admin/internal/handler/openapplicationlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/openapplicationlistlogic.go`

请求：`ParamOpenApplicationList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Status` | `status` | json | `int32` | 否 | — | 0 = 不按状态过滤；未知取值由服务拒 |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 0 = 服务默认页大小 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenApplicationListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenApplicationListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/application/get` — 单个应用详情（含回调白名单与密钥状态；不存在与无权看服务回同一错误，网关不折叠成 found=false）

- 权限口径：AdminPermission · 权限点 `openplatform:application` / `read`
- goctl 入口：`gateway/admin/internal/handler/openapplicationgethandler.go`
- 业务实现：`gateway/admin/internal/logic/openapplicationgetlogic.go`

请求：`ParamOpenApplicationGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 否 | — | — |
| `AppKey` | `app_key` | json | `string` | 否 | — | — |
| `CallerMid` | `caller_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenApplicationGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenApplicationGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/application/state` — 推进应用状态机（只有状态：资料改动在运营通道被服务直接拒）

- 权限口径：AdminPermission · 权限点 `openplatform:application` / `state`
- goctl 入口：`gateway/admin/internal/handler/openapplicationstatehandler.go`
- 业务实现：`gateway/admin/internal/logic/openapplicationstatelogic.go`

请求：`ParamOpenApplicationState`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `TargetStatus` | `target_status` | json | `int32` | 是 | — | 必填且 >0：0 在写侧是「没选」，不是某种状态 |
| `Reason` | `reason` | json | `string` | 是 | — | 必填（服务 requireReason） |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenApplicationStateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenApplicationStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/secret/rotate` — 轮换应用密钥（新明文仅此一次返回且不进日志；旧密钥宽限期由服务落地）

- 权限口径：AdminPermission · 权限点 `openplatform:secret` / `rotate`
- goctl 入口：`gateway/admin/internal/handler/opensecretrotatehandler.go`
- 业务实现：`gateway/admin/internal/logic/opensecretrotatelogic.go`

请求：`ParamOpenSecretRotate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `GraceSeconds` | `grace_seconds` | json | `int64` | 否 | — | 0 = 旧密钥立即失效 |
| `Reason` | `reason` | json | `string` | 是 | — | 必填（泄露/例行轮换，审计） |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenSecretRotateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenSecretRotateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/secret/revoke` — 吊销应用密钥（secret_id=0 = 全部生效密钥；token 不受影响，要撤授权走 /authorization/revoke）

- 权限口径：AdminPermission · 权限点 `openplatform:secret` / `revoke`
- goctl 入口：`gateway/admin/internal/handler/opensecretrevokehandler.go`
- 业务实现：`gateway/admin/internal/logic/opensecretrevokelogic.go`

请求：`ParamOpenSecretRevoke`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `SecretId` | `secret_id` | json | `int64` | 否 | — | 0 = 全部生效密钥 |
| `Reason` | `reason` | json | `string` | 是 | — | 必填 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenSecretRevokeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenSecretRevokeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/scope/grant` — scope 授予/回收（部分授予是正常结果，rejected 列表原样回；幂等键必填）

- 权限口径：AdminPermission · 权限点 `openplatform:scope` / `grant`
- goctl 入口：`gateway/admin/internal/handler/openscopegranthandler.go`
- 业务实现：`gateway/admin/internal/logic/openscopegrantlogic.go`

请求：`ParamOpenScopeGrant`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `Grant` | `grant` | json | `[]string` | 否 | — | — |
| `Revoke` | `revoke` | json | `[]string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | 必填：重试不能把回收再执行一遍 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenScopeGrantResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenScopeGrantData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/authorization/revoke` — 撤销授权（TOKEN/GRANT/USER_ALL 三种范围；必填组合由服务判，明文 hint 不进日志）

- 权限口径：AdminPermission · 权限点 `openplatform:authorization` / `revoke`
- goctl 入口：`gateway/admin/internal/handler/openauthorizationrevokehandler.go`
- 业务实现：`gateway/admin/internal/logic/openauthorizationrevokelogic.go`

请求：`ParamOpenAuthorizationRevoke`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Target` | `target` | json | `int32` | 是 | — | 必填且 >0（UNSPECIFIED 服务侧直接拒） |
| `AppId` | `app_id` | json | `int64` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | 被撤销的授权主体（用户），0 = 本 target 不需要 |
| `TokenId` | `token_id` | json | `int64` | 否 | — | — |
| `TokenHint` | `token_hint` | json | `string` | 否 | — | 明文凭证，不进日志 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenAuthorizationRevokeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenAuthorizationRevokeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/quota/policy/list` — 配额规则目录分页（服务侧 requireOperator；app_id=0 是全局兜底层级、api_code=「*」是通配规则本身）

- 权限口径：AdminPermission · 权限点 `openplatform:quota-policy` / `read`
- goctl 入口：`gateway/admin/internal/handler/openquotapolicylisthandler.go`
- 业务实现：`gateway/admin/internal/logic/openquotapolicylistlogic.go`

请求：`ParamOpenQuotaPolicyList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 否 | — | — |
| `ApiCode` | `api_code` | json | `string` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 0 = 服务配置的默认页大小 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | 必填 >0，表单自报（见段头 mid 空间缺口） |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenQuotaPolicyListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenQuotaPolicyListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/quota/policy/upsert` — 新增/更新配额规则（policy_id=0 = 唯一键新建；enabled=false 在本入口无路径——proto 未暴露停用方法）

- 权限口径：AdminPermission · 权限点 `openplatform:quota-policy` / `upsert`
- goctl 入口：`gateway/admin/internal/handler/openquotapolicyupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/openquotapolicyupsertlogic.go`

请求：`ParamOpenQuotaPolicyUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PolicyId` | `policy_id` | json | `int64` | 否 | — | 0 = 新建 |
| `AppId` | `app_id` | json | `int64` | 否 | — | 0 = 全局兜底层级 |
| `ApiCode` | `api_code` | json | `string` | 是 | — | "*" = 该应用全部接口；空串的语义由服务判 |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | — |
| `Limit` | `limit` | json | `int64` | 是 | — | — |
| `Enabled` | `enabled` | json | `bool` | 否 | — | false 被就地拒（见上） |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenQuotaPolicyUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenQuotaPolicyUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/quota/usage/list` — 配额用量读（按应用；投影可漂移，跨应用汇总不在本契约）

- 权限口径：AdminPermission · 权限点 `openplatform:quota` / `read`
- goctl 入口：`gateway/admin/internal/handler/openquotausagelisthandler.go`
- 业务实现：`gateway/admin/internal/logic/openquotausagelistlogic.go`

请求：`ParamOpenQuotaUsageList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `ApiCode` | `api_code` | json | `string` | 否 | — | — |
| `WindowStart` | `window_start` | json | `int64` | 否 | — | 0 = 当前窗口 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenQuotaUsageListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenQuotaUsageListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/quota/recompute` — 从调用流水重算配额投影（dry_run 先看差异；投影重算幂等故契约无幂等键位）

- 权限口径：AdminPermission · 权限点 `openplatform:quota` / `recompute`
- goctl 入口：`gateway/admin/internal/handler/openquotarecomputehandler.go`
- 业务实现：`gateway/admin/internal/logic/openquotarecomputelogic.go`

请求：`ParamOpenQuotaRecompute`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | 必填 >0（proto 注释的「0=全部应用」实现不接受） |
| `ApiCode` | `api_code` | json | `string` | 否 | — | "*" 或空 = 全部接口 |
| `WindowStart` | `window_start` | json | `int64` | 是 | — | — |
| `WindowEnd` | `window_end` | json | `int64` | 是 | — | — |
| `DryRun` | `dry_run` | json | `bool` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenQuotaRecomputeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenQuotaRecomputeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/webhook/list` — 回调端点列表（地址属外部主体凭证面；软删行永远不回）

- 权限口径：AdminPermission · 权限点 `openplatform:webhook` / `read`
- goctl 入口：`gateway/admin/internal/handler/openwebhooklisthandler.go`
- 业务实现：`gateway/admin/internal/logic/openwebhooklistlogic.go`

请求：`ParamOpenWebhookList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `IncludeDisabled` | `include_disabled` | json | `bool` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenWebhookListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenWebhookListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/webhook/delete` — 删除回调端点并抑制未投递任务（本域刻意没有新增/改地址入口）

- 权限口径：AdminPermission · 权限点 `openplatform:webhook` / `delete`
- goctl 入口：`gateway/admin/internal/handler/openwebhookdeletehandler.go`
- 业务实现：`gateway/admin/internal/logic/openwebhookdeletelogic.go`

请求：`ParamOpenWebhookDelete`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `EndpointId` | `endpoint_id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenWebhookDeleteResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenWebhookDeleteData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/webhook/delivery/list` — 投递流水分页（只有 payload_digest 与脱敏错误；payload 正文不经本 RPC 外发）

- 权限口径：AdminPermission · 权限点 `openplatform:delivery` / `read`
- goctl 入口：`gateway/admin/internal/handler/openwebhookdeliverylisthandler.go`
- 业务实现：`gateway/admin/internal/logic/openwebhookdeliverylistlogic.go`

请求：`ParamOpenWebhookDeliveryList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `EndpointId` | `endpoint_id` | json | `int64` | 否 | — | 0 = 全部端点 |
| `State` | `state` | json | `int32` | 否 | — | 0 = 不过滤；其余取值是否合法由服务判 |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenWebhookDeliveryListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenWebhookDeliveryListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/open-platform/webhook/delivery/retry` — 死信重放（只重置既有记录的 attempt，不注入新事件）

- 权限口径：AdminPermission · 权限点 `openplatform:delivery` / `retry`
- goctl 入口：`gateway/admin/internal/handler/openwebhookdeliveryretryhandler.go`
- 业务实现：`gateway/admin/internal/logic/openwebhookdeliveryretrylogic.go`

请求：`ParamOpenWebhookDeliveryRetry`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeliveryId` | `delivery_id` | json | `int64` | 是 | — | — |
| `IgnoreDead` | `ignore_dead` | json | `bool` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OpenWebhookDeliveryRetryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenWebhookDeliveryRetryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamOpenScopeList`

> -------------------- open-platform 读侧参数与响应 -------------------- / 划线口径（比 spm/feature-store 更严，原因是本契约的读方法自带运营主体位）： /   * **只有 /scope/list 免判定**——它是纯目录读，`ListScopesReq` 里没有任何操作者位； /   * 其余五条读（应用详情/列表、回调端点、配额规则与用量、投递流水）都要求 /     `operator_mid`/`caller_mid` > 0：服务侧把 mid==0 解释成「owner 自查，归属已由网关校验」 /     （见 listwebhookslogic 与 listquotausagelogic 的身份口径注释），而后台无法替开发者做这个 /     归属校验，所以必须显式给 mid 才能表达「这是运营在读」。既然要信这个 mid，就必须先确认 /     调用方是后台会话，因此这五条一律挂 AdminPermission。 /   * 应用配置（redirect_uri 白名单、已批 scope、密钥状态）与回调地址本身还属**外部主体的 /     凭证面**——白名单可用于构造授权钓鱼。这是与「有没有主体位」独立的第二条理由，两条指向同一结论。 / 配额规则目录更硬：`ListQuotaPolicies` 用 requireOperator 直接拒 mid<=0（规则集含运营意图， / 不给 owner 切片外发），网关漏配也拿不到数据。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 否 | — | 0 = 只要目录本身（granted_state 整列恒 0） |
| `OnlyEnabled` | `only_enabled` | json | `bool` | 否 | — | true = 只回开放中的 scope |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenScopeListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenScopeListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenApplicationList`

> ParamOpenApplicationList 运营侧应用台账（网关按会话把契约的 operator 位置 true， / 因此走的是服务的「全量分支」）。 / **表单里没有 owner_mid**：契约的运营分支完全不读这一位（ListAll 只按 status+游标筛）， / 放上它就是一条「填了也不生效」的假筛选项。「按开发者查他的应用」属开发者侧 / （gateway/app 的开发者中心按会话 mid 走 owner 分支）。契约缺口见文末。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Status` | `status` | json | `int32` | 否 | — | 0 = 不按状态过滤；未知取值由服务拒 |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 0 = 服务默认页大小 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenApplicationListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenApplicationListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenApplicationGet`

> ParamOpenApplicationGet 单个应用详情。app_id 与 app_key 二选一（两者都给时服务以 app_id 为准， / 这条优先级写在契约注释里：允许 app_key 覆盖 app_id 等于给「拿公开标识探测他人应用」留口子）。 / caller_mid 必填 >0：GetApplication 的身份判定是 requireOwnerOrOperator(caller_mid, operator=true)， / 即便运营分支也要求 mid 非空——审计要落到具体的人，「匿名运营」在本域不被接受。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 否 | — | — |
| `AppKey` | `app_key` | json | `string` | 否 | — | — |
| `CallerMid` | `caller_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenApplicationGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenApplicationGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenApplicationState`

> -------------------- open-platform 受保护写面 -------------------- / 每条都有外部后果：状态推进决定第三方能不能调用、密钥轮换/吊销决定谁的签名请求失效、 / scope 授予决定它能读到什么、撤销授权直接打死用户凭证、配额规则决定限流、 / 端点删除与死信重放决定事件投递。因此一条一个权限点，且 reason 在服务侧必填的这里一律必填。 / ParamOpenApplicationState 推进应用状态机（PENDING_REVIEW→ACTIVE/REJECTED、ACTIVE↔SUSPENDED、 / 任意态→OFFLINE）。本入口只有状态：name/description/redirect_uris 不属后台职权 / （服务侧运营通道见到任一资料字段就回 ErrOwnerRequired，网关不放表单进去也不替调用方清空）。 / **表单里没有 expected_version**：UpdateApplicationReq 的这一位只在资料通道被要求 >0 / （applyProfile 判并发），运营状态通道完全不读它——落库的并发保护是 / `UPDATE ... WHERE app_id=? AND status=<服务刚读到的 from>` 这一状态 CAS， / 加上 CanTransitionAppStatus 的迁移表。放一个填了也不生效的乐观锁位，等于让运营 / 以为「带上版本号就安全了」，而真正的保护在别处（缺口记在文末）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `TargetStatus` | `target_status` | json | `int32` | 是 | — | 必填且 >0：0 在写侧是「没选」，不是某种状态 |
| `Reason` | `reason` | json | `string` | 是 | — | 必填（服务 requireReason） |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenApplicationStateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenApplicationStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenSecretRotate`

> ParamOpenSecretRotate 轮换应用密钥。grace_seconds=0 是「旧密钥立即失效」的真实语义， / 不是「不限」也不是「不填」，因此原样下传（填 0 与不填在本入口同义，后果由服务落地）。 / 宽限期多长合理、应用状态是否允许轮换，全由服务判。 / 响应里的 client_secret 是**一次性明文**：网关不写日志、不落 cache、ttl 固定 0。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `GraceSeconds` | `grace_seconds` | json | `int64` | 否 | — | 0 = 旧密钥立即失效 |
| `Reason` | `reason` | json | `string` | 是 | — | 必填（泄露/例行轮换，审计） |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenSecretRotateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenSecretRotateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenSecretRevoke`

> ParamOpenSecretRevoke 紧急吊销密钥（怀疑泄露）。secret_id=0 是契约里的「吊销该应用全部 / 生效密钥」，不是「没选」；到底吊销了几把由服务回读（reply.revoked），网关不猜。 / 已签发的 token 不受影响（proto 明确），要一并打死授权走 /authorization/revoke。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `SecretId` | `secret_id` | json | `int64` | 否 | — | 0 = 全部生效密钥 |
| `Reason` | `reason` | json | `string` | 是 | — | 必填 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenSecretRevokeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenSecretRevokeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenScopeGrant`

> ParamOpenScopeGrant scope 授予/回收（运营审批）。grant 与 revoke 是否可以同时给、 / 单个 scope 是否要求逐次同意、高风险级别能否批量授予，全部由服务判定（本方法在服务侧 / 就要求 reason 与 idempotency_key）。响应里的 rejected 是「目录中不存在或已停用」的 scope， / 网关不把它折叠成整体失败——部分授予是这条 RPC 的正常结果。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `Grant` | `grant` | json | `[]string` | 否 | — | — |
| `Revoke` | `revoke` | json | `[]string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | 必填：重试不能把回收再执行一遍 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenScopeGrantResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenScopeGrantData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenAuthorizationRevoke`

> ParamOpenAuthorizationRevoke 撤销授权（RFC 7009 语义，写位点 + 逐条标记双保险）。 / 三种 target 的必填组合由服务判（GRANT 要 app_id+mid、USER_ALL 只要 mid、 / TOKEN 要 app_id + (token_id 或 token_hint)）；网关不重做这张矩阵，也不猜「没填 mid 是不是想撤全部」。 / USER_ALL 是 blast radius 最大的一条（该用户对所有第三方应用的授权一起失效）， / 因此本入口的 reason 必填——运营通道下服务侧同样要求 reason。 / token_hint 是明文凭证：只在本次请求内使用，**永不进网关日志**。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Target` | `target` | json | `int32` | 是 | — | 必填且 >0（UNSPECIFIED 服务侧直接拒） |
| `AppId` | `app_id` | json | `int64` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | 被撤销的授权主体（用户），0 = 本 target 不需要 |
| `TokenId` | `token_id` | json | `int64` | 否 | — | — |
| `TokenHint` | `token_hint` | json | `string` | 否 | — | 明文凭证，不进日志 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenAuthorizationRevokeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenAuthorizationRevokeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenQuotaPolicyList`

> ParamOpenQuotaPolicyList 规则目录分页。app_id=0 是「只看全局兜底层级」的合法取值， / 不是「不限」；要跨层级看就不传（契约没有「全部层级」哨兵，网关不发明）。 / api_code 空 = 不过滤，"*" = 只查通配规则；游标是服务给的 (mtime, policy_id) 位点，原样回传。 / operator_mid 必填 >0：本方法在服务侧就是 requireOperator，mid=0 直接被拒（不猜是谁在读规则集）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 否 | — | — |
| `ApiCode` | `api_code` | json | `string` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 0 = 服务配置的默认页大小 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | 必填 >0，表单自报（见段头 mid 空间缺口） |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenQuotaPolicyListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenQuotaPolicyListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenQuotaPolicyUpsert`

> ParamOpenQuotaPolicyUpsert 新增/更新配额规则。policy_id=0 = 按 (app_id, api_code, / window_seconds) 唯一键新建；改哪条规则、这个组合是否已存在、limit 合不合理由服务判， / 网关不预读列表去判断「算不算新建」，也不夹取 limit。 / enabled 是 bool：契约没有「未声明」这一态，不传即 false。 / **但 false 在本入口没有可执行路径**：服务对 !enabled 直接回 errReasonRequired，因为 / 「关掉一条限额」要有问责原因、而 UpsertQuotaPolicyReq 里根本没有 reason 位（停用只走 / model.QuotaPolicies.Disable，proto 没有暴露对应 RPC 方法）。网关因此就地拒 enabled=false， / 给一条说得清的错，而不是让调用方收到一句指向它没填过的字段的「reason required」； / 这条能力缺口记在文末与本域 README，不在网关侧伪造「已停用」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PolicyId` | `policy_id` | json | `int64` | 否 | — | 0 = 新建 |
| `AppId` | `app_id` | json | `int64` | 否 | — | 0 = 全局兜底层级 |
| `ApiCode` | `api_code` | json | `string` | 是 | — | "*" = 该应用全部接口；空串的语义由服务判 |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | — |
| `Limit` | `limit` | json | `int64` | 是 | — | — |
| `Enabled` | `enabled` | json | `bool` | 否 | — | false 被就地拒（见上） |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenQuotaPolicyUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenQuotaPolicyUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenQuotaUsageList`

> ParamOpenQuotaUsageList 用量读。app_id 必填 >0：用量投影按真实 app_id 记账， / 契约没有跨应用汇总（那属 SPM 报表，不是配额）。window_start=0 = 当前窗口。 / operator_mid 必填 >0：mid==0 在服务侧被解释成「owner 自查自己的额度」，后台不是 owner。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `ApiCode` | `api_code` | json | `string` | 否 | — | — |
| `WindowStart` | `window_start` | json | `int64` | 否 | — | 0 = 当前窗口 |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenQuotaUsageListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenQuotaUsageListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenQuotaRecompute`

> ParamOpenQuotaRecompute 从调用流水重算配额投影。dry_run=true 只返回差异不写回—— / 排障时先看漂移再决定要不要动，这条位由调用方显式给，网关不默认成 false 去「顺手写回」。 / 区间是否过大（服务的 lookback×4 上界）与窗口数上限由服务判；投影重算本身幂等 / （同区间重跑结论一致），因此契约没有幂等键位，网关也不编一个塞进别的字段。 / **app_id 必须 >0**：proto:492 注释写「0 表示全部应用」，但实现把 app_id<=0 直接拒 / （recomputequotalogic 第 2 步：跨应用会把所有应用的流水并进同一个窗口桶， / 拿合并计数覆盖单应用行等于打穿限额语义）。网关按**实现**而非注释立闸， / 并把这条注释/实现分歧记在文末缺口清单。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | 必填 >0（proto 注释的「0=全部应用」实现不接受） |
| `ApiCode` | `api_code` | json | `string` | 否 | — | "*" 或空 = 全部接口 |
| `WindowStart` | `window_start` | json | `int64` | 是 | — | — |
| `WindowEnd` | `window_end` | json | `int64` | 是 | — | — |
| `DryRun` | `dry_run` | json | `bool` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenQuotaRecomputeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenQuotaRecomputeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenWebhookList`

> ParamOpenWebhookList 端点列表（按应用）。include_disabled=false 只回启用中的行； / 软删行永远不回（它们只在投递台账里解释归属），所以本方法也不是「已删配置」的查询面。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `IncludeDisabled` | `include_disabled` | json | `bool` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenWebhookListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenWebhookListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenWebhookDelete`

> ParamOpenWebhookDelete 删除回调端点（同时抑制其未投递任务）。 / 本域刻意没有「新增端点」路由（见段头口径 1），因此这里也没有「改地址」—— / 改地址等价于换数据去向，只能由归属者重新注册。deleted=false 时服务照样回台账， / 网关不把它伪装成失败。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `EndpointId` | `endpoint_id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenWebhookDeleteResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenWebhookDeleteData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenWebhookDeliveryList`

> ParamOpenWebhookDeliveryList 投递流水分页（排障：这条事件为什么没送到、重试到哪一步）。 / 台账永远按应用读，服务不提供跨应用枚举；endpoint_id=0 表示该应用全部端点， / 非 0 时服务会校验它确实属于本应用（防交叉查询）。state=0 = 不按状态过滤。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `EndpointId` | `endpoint_id` | json | `int64` | 否 | — | 0 = 全部端点 |
| `State` | `state` | json | `int32` | 否 | — | 0 = 不过滤；其余取值是否合法由服务判 |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenWebhookDeliveryListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenWebhookDeliveryListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpenWebhookDeliveryRetry`

> ParamOpenWebhookDeliveryRetry 死信重放：只把既有投递记录重新排入队列（重置 attempt）， / 不制造新事件——这是它与刻意不开服的 EnqueueWebhookEvent 的分工。 / ignore_dead=true 允许重放已判死信的记录，属于「明知上游还没修好也要再打一次」， / 因此 reason 必填；能否重放（状态机、端点是否还在）由服务判。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeliveryId` | `delivery_id` | json | `int64` | 是 | — | — |
| `IgnoreDead` | `ignore_dead` | json | `bool` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OpenWebhookDeliveryRetryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpenWebhookDeliveryRetryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `OpenScopeListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]OpenScope` | 是 | — | — |

### `OpenApplicationListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]OpenApplication` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |

### `OpenApplicationGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `App` | `app` | json | `OpenApplication` | 是 | — | — |

### `OpenApplicationStateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `App` | `app` | json | `OpenApplication` | 是 | — | — |
| `Changed` | `changed` | json | `bool` | 是 | — | false = 目标态与当前一致（服务判定，网关不复算） |

### `OpenSecretRotateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ClientSecret` | `client_secret` | json | `string` | 是 | — | 仅此一次返回，不进日志 |
| `SecretId` | `secret_id` | json | `int64` | 是 | — | — |
| `OldSecretId` | `old_secret_id` | json | `int64` | 是 | — | 0 = 没有被替换的旧密钥 |
| `OldSecretExpiresAt` | `old_secret_expires_at` | json | `int64` | 是 | — | — |
| `RotatedAt` | `rotated_at` | json | `int64` | 是 | — | — |

### `OpenSecretRevokeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Revoked` | `revoked` | json | `int32` | 是 | — | — |
| `EffectiveAt` | `effective_at` | json | `int64` | 是 | — | — |

### `OpenScopeGrantData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Granted` | `granted` | json | `[]string` | 是 | — | — |
| `Revoked` | `revoked` | json | `[]string` | 是 | — | — |
| `Rejected` | `rejected` | json | `[]string` | 是 | — | 目录里没有或已停用：原样回，不伪装成已授予 |
| `AppVersion` | `app_version` | json | `int32` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `OpenAuthorizationRevokeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `GrantsRevoked` | `grants_revoked` | json | `int32` | 是 | — | — |
| `TokensRevoked` | `tokens_revoked` | json | `int32` | 是 | — | — |
| `EffectiveAt` | `effective_at` | json | `int64` | 是 | — | 撤销位点时间：晚于它的缓存校验结果也会被拒 |

### `OpenQuotaPolicyListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]OpenQuotaPolicy` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |

### `OpenQuotaPolicyUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PolicyId` | `policy_id` | json | `int64` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | 服务回读的新旧判定，网关不复算 |

### `OpenQuotaUsageListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]OpenQuotaUsage` | 是 | — | — |

### `OpenQuotaRecomputeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `WindowsScanned` | `windows_scanned` | json | `int64` | 是 | — | — |
| `WindowsFixed` | `windows_fixed` | json | `int64` | 是 | — | — |
| `MaxDelta` | `max_delta` | json | `int64` | 是 | — | 单窗口最大修正量：漂移幅度的观测位 |

### `OpenWebhookListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]OpenWebhookEndpoint` | 是 | — | — |

### `OpenWebhookDeleteData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Deleted` | `deleted` | json | `bool` | 是 | — | — |
| `DeliveriesSuppressed` | `deliveries_suppressed` | json | `int32` | 是 | — | 被抑制的待投递数（服务回读） |

### `OpenWebhookDeliveryListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]OpenWebhookDelivery` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |

### `OpenWebhookDeliveryRetryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeliveryId` | `delivery_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 重放后服务给的真实状态，不是「已入队」的猜测 |
| `NextRetryAt` | `next_retry_at` | json | `int64` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |

### `OpenScope`

> OpenScope 1:1 对应 rpc.ScopeInfo（scope 目录行）。 / granted_state 是「传入 app_id 时该应用对这一条的获批状态」（0 未申请 / 1 待审批 / 2 已获批）； / 不传 app_id 时整列恒为 0——网关不把它解释成「都没获批」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | json | `string` | 是 | — | — |
| `DisplayName` | `display_name` | json | `string` | 是 | — | — |
| `Access` | `access` | json | `int32` | 是 | — | 1 只读 / 2 写（写必须再声明是否要求逐次同意） |
| `RiskLevel` | `risk_level` | json | `int32` | 是 | — | — |
| `RequiresUserConsent` | `requires_user_consent` | json | `bool` | 是 | — | — |
| `Enabled` | `enabled` | json | `bool` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | 停用原因（目录文案，不是操作理由） |
| `GrantedState` | `granted_state` | json | `int32` | 是 | — | — |

### `OpenApplication`

> 契约来源 services/open-platform/rpc/openplatform.proto（冻结）。本服务是「哪个第三方应用存在、 / 它拿到过哪些 scope、它的配额规则与回调端点、用户对它的授权是否已被撤销」的所有者。 /  / 本域三条硬口径，决定了下面为什么只开这 16 条路由（24 个 RPC 方法里的其余 8 个见文末说明）： /   1. **后台不代替开发者表达意愿**：RegisterApplication 不接路由——它替某个 owner_mid 建应用并 /      首发一次性明文密钥；UpdateApplication 的资料分支不接路由——服务侧在运营通道里 /      见到 name/description/redirect_uris 就直接回 ErrOwnerRequired（helpers 的通道分离： /      开发者只能改资料、运营只能推进状态，混用会造成「顺手改下线原因时把别人的回调白名单也覆盖了」）； /      RegisterWebhook 不接路由——回调地址决定「这个应用的事件数据流向谁」，由运营代设等于 /      把第三方数据去向交到平台手里（SSRF 与越权读取的应用侧后果）。后台需要处置错误配置时走 /      /webhook/delete，让归属者自己重新注册。 /   2. **后台不签发也不换发用户凭证**：IssueAuthorizationCode（必须用户在授权页显式同意）、 /      ExchangeAuthorizationCode、RefreshAccessToken、IntrospectToken、AuthorizeRequest 一律不接 /      ——前四个只在开放平台对外 HTTP 面上按用户/应用凭证发生，AuthorizeRequest 是每次开放调用 /      都要走的热路径（凭证 + scope + 配额扣减 + 调用流水），搬到后台等于允许「代签一个能用的 token」 /      或把在线配额吃在排障刷新上。 /   3. **后台不注入事实**：EnqueueWebhookEvent 不接路由——它是领域事件投递口（按 event_id 幂等）， /      手工入队一条等于伪造「已经发生过的事件」；要看投递结果走 /webhook/delivery/list， /      要重放死信走 /webhook/delivery/retry（只重置既有记录的 attempt，不制造新事实）。 /  / **运营主体落在 mid 空间（与 /admin/live 同一口径的已知缺口）**：本契约用 operator_mid / / caller_mid 表达操作者，那属用户 mid 空间；AdminPermission 会话给的是 op_admin_user.admin_id， / 两者不是同一编号空间，网关把 admin_id 填进去等于把处置记到无关用户头上。因此本组所有需要 / 运营主体的路由都要求表单**显式**给出 >0 的 mid，网关不代为渲染、不覆盖，只把 admin_id 与 / 表单 mid 一起打日志（「谁点的按钮」与「台账落在谁身上」两条都可追）。 / 反之 `is_operator` / `operator` 位**一律由网关按「这就是后台入口」置 true**，表单里没有这一位： / 服务侧按这个位区分 owner 自查与运营（见各 logic 注释「归属由网关校验」），漏置会让运营读被 / 当成 owner 自查，多置也不会放大任何权限（前面已有 AdminPermission 判定）。 /  / 网关在这 16 条上只挡「下游没有对应语义」的形状：负数主体/ID、写侧枚举位为 UNSPECIFIED、 / 必填的 reason 与幂等键为空。以下 0 值都是契约里的合法哨兵，一律原样下传，网关不替调用方挑值： / app_id=0（配额规则的全局兜底层级；**RecomputeQuota 例外**，那里实现拒绝 0，见文末）、 / secret_id=0（吊销该应用全部生效密钥）、 / grace_seconds=0（旧密钥立即失效，不是「不限」）、window_start=0（当前窗口）、 / ps=0（服务配置的默认页大小）、policy_id=0（按唯一键新建）、api_code=""（该维度不过滤）。 / **api_code="*" 是通配规则自身的取值**，不是「不过滤」，网关不做归一化（否则「列出通配规则」 / 这条查询就表达不出来了）。 / 一次性明文只出现在两处响应里：RotateApplicationSecret 的 client_secret 与 / RevokeAuthorization 的 token_hint（入参）。两者**永不进网关日志**，ttl 固定 0 提示客户端不缓存； / 密钥材料本身在服务侧只存摘要（见 services/open-platform/README.md）。 / OpenApplication 1:1 对应 rpc.ApplicationInfo（应用投影）。契约保证没有任何密钥材料： / secret_state 只说「有没有生效密钥」，scopes 与 secret_state 都是派生列（真值在 / op_app_scope / op_app_secret），因此本投影不能当写入依据，只能作为下一次乐观锁请求的参考。 / app_key 签发后不可变；version 是乐观锁版本，改资料/推状态都要带上它。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `AppKey` | `app_key` | json | `string` | 是 | — | 公开标识（不可变，不是秘密） |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `OwnerMid` | `owner_mid` | json | `int64` | 是 | — | 归属开发者 |
| `Status` | `status` | json | `int32` | 是 | — | — |
| `RedirectUris` | `redirect_uris` | json | `[]string` | 是 | — | 授权回调白名单（凭证面数据） |
| `Scopes` | `scopes` | json | `[]string` | 是 | — | 当前已获批 scope |
| `SecretState` | `secret_state` | json | `int32` | 是 | — | 1 有生效密钥 / 2 全部已撤销 / 3 从未签发 |
| `SecretRotatedAt` | `secret_rotated_at` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | 乐观锁，回读后才能安全再改一次 |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `OfflineAt` | `offline_at` | json | `int64` | 是 | — | 0 = 未下线 |

### `OpenQuotaPolicy`

> OpenQuotaPolicy 1:1 对应 rpc.QuotaPolicyInfo（配额规则）。 / app_id=0 是全局兜底层级，api_code="*" 是该应用全部接口的通配规则——两者都是规则取值， / 不是「未填」。operator 是**最后改这条规则的人的 mid**（服务侧 int64 列），不是本次操作者。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PolicyId` | `policy_id` | json | `int64` | 是 | — | — |
| `AppId` | `app_id` | json | `int64` | 是 | — | 0 = 全局默认层级 |
| `ApiCode` | `api_code` | json | `string` | 是 | — | "*" = 该应用全部接口 |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | — |
| `Limit` | `limit` | json | `int64` | 是 | — | — |
| `Enabled` | `enabled` | json | `bool` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OpenQuotaUsage`

> OpenQuotaUsage 1:1 对应 rpc.QuotaUsageInfo（配额用量投影，可重算）。 / 投影不是事实源：真值在 op_api_call_log，漂移时用 /quota/recompute 按区间重算， / 因此这里的 remaining 是「按投影算出的余额」，服务在读不到规则时会回 ErrQuotaPolicyNotFound / 而不是给一个看起来正常的 0 余额。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `ApiCode` | `api_code` | json | `string` | 是 | — | — |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | — |
| `WindowStart` | `window_start` | json | `int64` | 是 | — | — |
| `Used` | `used` | json | `int64` | 是 | — | — |
| `Limit` | `limit` | json | `int64` | 是 | — | — |
| `Remaining` | `remaining` | json | `int64` | 是 | — | — |
| `UpdatedAt` | `updated_at` | json | `int64` | 是 | — | — |

### `OpenWebhookEndpoint`

> OpenWebhookEndpoint 1:1 对应 rpc.WebhookEndpointInfo。 / url 决定事件数据去向（属凭证/外泄面，因此本读挂权限点）；verified_at=0 表示还没通过验证， / 未验证的端点服务侧一律不投递。sign_key_version 只是「当前签名密钥版本」的告知位， / 签名材料由服务端 master pepper 派生，不入库也不外发。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EndpointId` | `endpoint_id` | json | `int64` | 是 | — | — |
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `EventType` | `event_type` | json | `int32` | 是 | — | — |
| `Url` | `url` | json | `string` | 是 | — | 回调地址：只回给调用方，不进网关日志 |
| `SignKeyVersion` | `sign_key_version` | json | `int32` | 是 | — | — |
| `Enabled` | `enabled` | json | `bool` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `VerifiedAt` | `verified_at` | json | `int64` | 是 | — | 0 = 未验证（不投递） |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OpenWebhookDelivery`

> OpenWebhookDelivery 1:1 对应 rpc.WebhookDeliveryInfo（投递台账）。 / 只有 payload_digest（sha256:<hex>），正文不存也不回；last_error 在服务侧已截断脱敏。 / 响应里刻意回 attempt/max_attempts/next_retry_at：重放前要先看得懂「为什么这条被判死信」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeliveryId` | `delivery_id` | json | `int64` | 是 | — | — |
| `AppId` | `app_id` | json | `int64` | 是 | — | — |
| `EndpointId` | `endpoint_id` | json | `int64` | 是 | — | — |
| `EventType` | `event_type` | json | `int32` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `PayloadDigest` | `payload_digest` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Attempt` | `attempt` | json | `int32` | 是 | — | — |
| `MaxAttempts` | `max_attempts` | json | `int32` | 是 | — | — |
| `NextRetryAt` | `next_retry_at` | json | `int64` | 是 | — | — |
| `LastStatusCode` | `last_status_code` | json | `int64` | 是 | — | — |
| `LastError` | `last_error` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/32-admin-open-platform.md -->
