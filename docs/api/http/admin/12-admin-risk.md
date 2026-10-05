# 运营面 · `/admin/risk`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| risk 域运营路由 | 免鉴权 | 4 |
| risk 域写入口（受 AdminPermission 保护） | AdminPermission | 7 |

合计 **11** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## risk 域运营路由（免鉴权，4 条）

> 规则管理、名单管理、处罚下发与解除全部落 risk-control（AGENTS.md §5 数据所有者）。
> check/report 是内部接口，这里仅供运营排障与规则调试使用：
> 网关只透传裁决结果，不在本地做任何风险判定，也不下发终端提示文案。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/risk/device/:device_id` | 查询设备画像（只回传 device_hash） | `getRiskDevice` | `getriskdevicelogic.go` |
| GET | `/admin/risk/punishments` | 分页查询处罚记录 | `listPunishments` | `listpunishmentslogic.go` |
| GET | `/admin/risk/rules` | 分页查询风控规则 | `listRiskRules` | `listriskruleslogic.go` |
| GET | `/admin/risk/list_entries` | 分页查询黑白名单条目 | `listRiskListEntries` | `listrisklistentrieslogic.go` |

### GET `/admin/risk/device/:device_id` — 查询设备画像（只回传 device_hash）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getriskdevicehandler.go`
- 业务实现：`gateway/admin/internal/logic/getriskdevicelogic.go`

请求：`ParamRiskDevice`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeviceHash` | `device_hash` | form | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeviceId` | `device_id` | path | `string` | 是 | — | — |

响应：`RiskDeviceProfileResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskDeviceProfileData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/risk/punishments` — 分页查询处罚记录

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listpunishmentshandler.go`
- 业务实现：`gateway/admin/internal/logic/listpunishmentslogic.go`

请求：`ParamListPunishments`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `Scope` | `scope` | form | `int32` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `OnlyActive` | `only_active` | form | `bool` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

响应：`RiskPunishmentsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskPunishmentsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/risk/rules` — 分页查询风控规则

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listriskruleshandler.go`
- 业务实现：`gateway/admin/internal/logic/listriskruleslogic.go`

请求：`ParamListRiskRules`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ActionType` | `action_type` | form | `int32` | 否 | — | — |
| `Metric` | `metric` | form | `int32` | 否 | — | — |
| `State` | `state` | form | `int32` | 是 | default=-1 | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

响应：`RiskRulesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskRulesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/risk/list_entries` — 分页查询黑白名单条目

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listrisklistentrieshandler.go`
- 业务实现：`gateway/admin/internal/logic/listrisklistentrieslogic.go`

请求：`ParamListRiskListEntries`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ListType` | `list_type` | form | `int32` | 否 | — | — |
| `TargetType` | `target_type` | form | `int32` | 否 | — | — |
| `TargetValue` | `target_value` | form | `string` | 否 | — | — |
| `State` | `state` | form | `int32` | 是 | default=-1 | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

响应：`RiskListEntriesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskListEntriesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## risk 域写入口（受 AdminPermission 保护）（AdminPermission，7 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/risk/check` | 调试用同步裁决一次受保护动作（返回可解释裁决与命中明细） | `risk:check` / `run` | `riskCheck` | `riskchecklogic.go` |
| POST | `/admin/risk/report` | 调试用行为上报（写滑窗计数，event_id 幂等） | `risk:report` / `create` | `riskReport` | `riskreportlogic.go` |
| POST | `/admin/risk/device` | 写入/更新设备画像与设备-账号关联 | `risk:device` / `update` | `upsertRiskDevice` | `upsertriskdevicelogic.go` |
| POST | `/admin/risk/punishment` | 下发处罚（operator_id 与 idempotency_key 必填） | `risk:punishment` / `create` | `applyPunishment` | `applypunishmentlogic.go` |
| POST | `/admin/risk/punishment/lift` | 解除处罚（幂等，已终态返回当前状态） | `risk:punishment` / `lift` | `liftPunishment` | `liftpunishmentlogic.go` |
| POST | `/admin/risk/rule` | 新增/更新风控规则（版本递增，operator_id 必填） | `risk:rule` / `update` | `upsertRiskRule` | `upsertriskrulelogic.go` |
| POST | `/admin/risk/list_entry` | 新增/更新黑白名单条目 | `risk:list-entry` / `update` | `upsertRiskListEntry` | `upsertrisklistentrylogic.go` |

### POST `/admin/risk/check` — 调试用同步裁决一次受保护动作（返回可解释裁决与命中明细）

- 权限口径：AdminPermission · 权限点 `risk:check` / `run`
- goctl 入口：`gateway/admin/internal/handler/riskcheckhandler.go`
- 业务实现：`gateway/admin/internal/logic/riskchecklogic.go`

请求：`ParamRiskCheck`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `int32` | 是 | — | — |
| `DeviceId` | `device_id` | json | `string` | 否 | — | — |
| `IpHash` | `ip_hash` | json | `string` | 否 | — | — |
| `Platform` | `platform` | json | `string` | 否 | — | — |
| `AppVersion` | `app_version` | json | `string` | 否 | — | — |
| `RequestContext` | `request_context` | json | `map[string]string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |

响应：`RiskCheckResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskCheckData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/risk/report` — 调试用行为上报（写滑窗计数，event_id 幂等）

- 权限口径：AdminPermission · 权限点 `risk:report` / `create`
- goctl 入口：`gateway/admin/internal/handler/riskreporthandler.go`
- 业务实现：`gateway/admin/internal/logic/riskreportlogic.go`

请求：`ParamRiskReport`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `int32` | 是 | — | — |
| `DeviceId` | `device_id` | json | `string` | 否 | — | — |
| `IpHash` | `ip_hash` | json | `string` | 否 | — | — |
| `Platform` | `platform` | json | `string` | 否 | — | — |
| `Count` | `count` | json | `int64` | 否 | — | — |
| `OccurredAt` | `occurred_at` | json | `int64` | 否 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |

响应：`RiskReportResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskReportData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/risk/device` — 写入/更新设备画像与设备-账号关联

- 权限口径：AdminPermission · 权限点 `risk:device` / `update`
- goctl 入口：`gateway/admin/internal/handler/upsertriskdevicehandler.go`
- 业务实现：`gateway/admin/internal/logic/upsertriskdevicelogic.go`

请求：`ParamUpsertRiskDevice`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeviceId` | `device_id` | json | `string` | 否 | — | — |
| `DeviceHash` | `device_hash` | json | `string` | 否 | — | — |
| `Labels` | `labels` | json | `[]string` | 否 | — | — |
| `RiskScore` | `risk_score` | json | `int32` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Source` | `source` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RiskUpsertDeviceResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskUpsertDeviceData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/risk/punishment` — 下发处罚（operator_id 与 idempotency_key 必填）

- 权限口径：AdminPermission · 权限点 `risk:punishment` / `create`
- goctl 入口：`gateway/admin/internal/handler/applypunishmenthandler.go`
- 业务实现：`gateway/admin/internal/logic/applypunishmentlogic.go`

请求：`ParamApplyPunishment`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Scope` | `scope` | json | `int32` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `ReasonCode` | `reason_code` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `DurationSeconds` | `duration_seconds` | json | `int64` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`RiskApplyPunishmentResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskApplyPunishmentData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/risk/punishment/lift` — 解除处罚（幂等，已终态返回当前状态）

- 权限口径：AdminPermission · 权限点 `risk:punishment` / `lift`
- goctl 入口：`gateway/admin/internal/handler/liftpunishmenthandler.go`
- 业务实现：`gateway/admin/internal/logic/liftpunishmentlogic.go`

请求：`ParamLiftPunishment`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PunishmentId` | `punishment_id` | json | `int64` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Scope` | `scope` | json | `int32` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RiskLiftPunishmentResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskLiftPunishmentData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/risk/rule` — 新增/更新风控规则（版本递增，operator_id 必填）

- 权限口径：AdminPermission · 权限点 `risk:rule` / `update`
- goctl 入口：`gateway/admin/internal/handler/upsertriskrulehandler.go`
- 业务实现：`gateway/admin/internal/logic/upsertriskrulelogic.go`

请求：`ParamUpsertRiskRule`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 否 | — | — |
| `Name` | `name` | json | `string` | 否 | — | — |
| `ActionType` | `action_type` | json | `int32` | 否 | — | — |
| `Metric` | `metric` | json | `int32` | 是 | — | — |
| `Op` | `op` | json | `int32` | 是 | — | — |
| `Threshold` | `threshold` | json | `int64` | 是 | — | — |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Priority` | `priority` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RiskUpsertRuleResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskUpsertRuleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/risk/list_entry` — 新增/更新黑白名单条目

- 权限口径：AdminPermission · 权限点 `risk:list-entry` / `update`
- goctl 入口：`gateway/admin/internal/handler/upsertrisklistentryhandler.go`
- 业务实现：`gateway/admin/internal/logic/upsertrisklistentrylogic.go`

请求：`ParamUpsertRiskListEntry`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ListType` | `list_type` | json | `int32` | 是 | — | — |
| `TargetType` | `target_type` | json | `int32` | 是 | — | — |
| `TargetValue` | `target_value` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `DurationSeconds` | `duration_seconds` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RiskUpsertListEntryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskUpsertListEntryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamRiskDevice`

> device_id 是设备标识原文（服务端只落库 sha256 摘要）； / device_hash 是已受控 ID，二者都可用，服务端 device_id 优先。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeviceId` | `device_id` | path | `string` | 是 | — | — |
| `DeviceHash` | `device_hash` | form | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

### `RiskDeviceProfileResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskDeviceProfileData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListPunishments`

> state：0 不过滤（PunishmentState 无 0 以外的「全部」语义时用 0 表示不过滤）； / ps 上限 50（与 risk-control 分页口径一致）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `Scope` | `scope` | form | `int32` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `OnlyActive` | `only_active` | form | `bool` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

### `RiskPunishmentsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskPunishmentsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListRiskRules`

> action_type/metric 传 0 表示不过滤；state 传 -1 表示不过滤；ps 上限 50。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ActionType` | `action_type` | form | `int32` | 否 | — | — |
| `Metric` | `metric` | form | `int32` | 否 | — | — |
| `State` | `state` | form | `int32` | 是 | default=-1 | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

### `RiskRulesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskRulesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListRiskListEntries`

> list_type/target_type 传 0 表示不过滤；target_value 为空表示不过滤；state 传 -1 表示不过滤。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ListType` | `list_type` | form | `int32` | 否 | — | — |
| `TargetType` | `target_type` | form | `int32` | 否 | — | — |
| `TargetValue` | `target_value` | form | `string` | 否 | — | — |
| `State` | `state` | form | `int32` | 是 | default=-1 | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

### `RiskListEntriesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskListEntriesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRiskCheck`

> risk 域请求参数 / action：1 投稿、2 评论、3 弹幕、4 关注、5 登录、6 改名、7 直播开播； / ip_hash 必须是调用方预哈希摘要（禁止明文 IP）；operator_id 是网关审计主体， / 只写进行为日志，risk-control 的 CheckAction 契约本身没有运营字段。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `int32` | 是 | — | — |
| `DeviceId` | `device_id` | json | `string` | 否 | — | — |
| `IpHash` | `ip_hash` | json | `string` | 否 | — | — |
| `Platform` | `platform` | json | `string` | 否 | — | — |
| `AppVersion` | `app_version` | json | `string` | 否 | — | — |
| `RequestContext` | `request_context` | json | `map[string]string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |

### `RiskCheckResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskCheckData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRiskReport`

> count <=0 由服务端按 1 处理；event_id 是幂等键（必填，同 event_id 只累加一次）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `int32` | 是 | — | — |
| `DeviceId` | `device_id` | json | `string` | 否 | — | — |
| `IpHash` | `ip_hash` | json | `string` | 否 | — | — |
| `Platform` | `platform` | json | `string` | 否 | — | — |
| `Count` | `count` | json | `int64` | 否 | — | — |
| `OccurredAt` | `occurred_at` | json | `int64` | 否 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |

### `RiskReportResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskReportData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUpsertRiskDevice`

> risk_score 必须显式传值：>=0 覆盖画像分值，-1 表示不修改（漏传会被当成写 0）； / source 允许 login/gateway/operation/system，operation 必须带 operator_id； / labels 追加合并去重，单次最多 20 个。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeviceId` | `device_id` | json | `string` | 否 | — | — |
| `DeviceHash` | `device_hash` | json | `string` | 否 | — | — |
| `Labels` | `labels` | json | `[]string` | 否 | — | — |
| `RiskScore` | `risk_score` | json | `int32` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Source` | `source` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RiskUpsertDeviceResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskUpsertDeviceData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamApplyPunishment`

> scope：0 全域处罚、其余同 GuardedAction；decision 只能是 2 需校验、3 拒绝、4 转复核； / duration_seconds > 0 限期、0 永久；idempotency_key 必填，运营重试不得产生第二条处罚。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Scope` | `scope` | json | `int32` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `ReasonCode` | `reason_code` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `DurationSeconds` | `duration_seconds` | json | `int64` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `RiskApplyPunishmentResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskApplyPunishmentData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLiftPunishment`

> punishment_id 优先；为 0 时按 (mid, scope) 取最新生效处罚。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PunishmentId` | `punishment_id` | json | `int64` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Scope` | `scope` | json | `int32` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RiskLiftPunishmentResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskLiftPunishmentData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUpsertRiskRule`

> rule_id=0 表示新建（此时 name 必填）；metric 必须是已实现指标； / decision 不能是 1 放行；window_seconds 必须 > 0；state 0 禁用、1 启用。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 否 | — | — |
| `Name` | `name` | json | `string` | 否 | — | — |
| `ActionType` | `action_type` | json | `int32` | 否 | — | — |
| `Metric` | `metric` | json | `int32` | 是 | — | — |
| `Op` | `op` | json | `int32` | 是 | — | — |
| `Threshold` | `threshold` | json | `int64` | 是 | — | — |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Priority` | `priority` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RiskUpsertRuleResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskUpsertRuleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUpsertRiskListEntry`

> target_value：target_type=1 时是 mid 十进制字符串、2 时是 device_hash、3 时是 ip_hash， / 禁止传明文 IP；duration_seconds > 0 限期、0 永久；state 0 停用、1 生效。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ListType` | `list_type` | json | `int32` | 是 | — | — |
| `TargetType` | `target_type` | json | `int32` | 是 | — | — |
| `TargetValue` | `target_value` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `DurationSeconds` | `duration_seconds` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RiskUpsertListEntryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RiskUpsertListEntryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `RiskDeviceProfileData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Profile` | `profile` | json | `RiskDeviceProfileItem` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |

### `RiskPunishmentsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |
| `Punishments` | `punishments` | json | `[]RiskPunishmentItem` | 是 | — | — |

### `RiskRulesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |
| `Rules` | `rules` | json | `[]RiskRuleItem` | 是 | — | — |

### `RiskListEntriesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |
| `Entries` | `entries` | json | `[]RiskListEntryItem` | 是 | — | — |

### `RiskCheckData`

> CheckAction 裁决结果；has_punishment=false 时 punishment 为零值 / basis：blacklist/whitelist/punishment/rules/no_rule/fallback_db_unavailable/fallback_invalid

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Score` | `score` | json | `int32` | 是 | — | — |
| `HitRuleIds` | `hit_rule_ids` | json | `[]int64` | 是 | — | — |
| `RuleHits` | `rule_hits` | json | `[]RiskRuleHitItem` | 是 | — | — |
| `HasPunishment` | `has_punishment` | json | `bool` | 是 | — | — |
| `Punishment` | `punishment` | json | `RiskPunishmentSnapshotItem` | 是 | — | — |
| `ActionCode` | `action_code` | json | `string` | 是 | — | — |
| `ChallengeTtlSeconds` | `challenge_ttl_seconds` | json | `int64` | 是 | — | — |
| `Basis` | `basis` | json | `string` | 是 | — | — |
| `SkippedRuleIds` | `skipped_rule_ids` | json | `[]int64` | 是 | — | — |
| `Evaluated` | `evaluated` | json | `bool` | 是 | — | — |
| `Degraded` | `degraded` | json | `bool` | 是 | — | — |

### `RiskReportData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | — |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | — |
| `MidCount` | `mid_count` | json | `int64` | 是 | — | — |
| `DeviceCount` | `device_count` | json | `int64` | 是 | — | — |
| `IpCount` | `ip_count` | json | `int64` | 是 | — | — |

### `RiskUpsertDeviceData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Profile` | `profile` | json | `RiskDeviceProfileItem` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | — |
| `RelationAdded` | `relation_added` | json | `bool` | 是 | — | — |

### `RiskApplyPunishmentData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Punishment` | `punishment` | json | `RiskPunishmentItem` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | — |

### `RiskLiftPunishmentData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Punishment` | `punishment` | json | `RiskPunishmentItem` | 是 | — | — |
| `Changed` | `changed` | json | `bool` | 是 | — | — |

### `RiskUpsertRuleData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rule` | `rule` | json | `RiskRuleItem` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | — |

### `RiskUpsertListEntryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Entry` | `entry` | json | `RiskListEntryItem` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | — |

### `RiskDeviceProfileItem`

> 设备画像（只回传 device_hash，不回传设备号原文；risk_score 取值 0-100）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeviceHash` | `device_hash` | json | `string` | 是 | — | — |
| `Labels` | `labels` | json | `[]string` | 是 | — | — |
| `RiskScore` | `risk_score` | json | `int32` | 是 | — | — |
| `FirstSeen` | `first_seen` | json | `int64` | 是 | — | — |
| `LastSeen` | `last_seen` | json | `int64` | 是 | — | — |
| `RelatedMidCount` | `related_mid_count` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `RiskPunishmentItem`

> 处罚记录（运营视角，reason 禁止下发终端； / state：1 生效中、2 已解除、3 已过期；end_at=0 表示永久）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PunishmentId` | `punishment_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Scope` | `scope` | json | `int32` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `ReasonCode` | `reason_code` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `LiftOperator` | `lift_operator` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `RiskRuleItem`

> 风控规则（与 riskcontrol.v1.Rule 对齐） / action_type：0 全部动作、1 投稿、2 评论、3 弹幕、4 关注、5 登录、6 改名、7 直播开播 / metric：1 mid+动作次数、2 设备+动作次数、3 ip_hash+动作次数、4 设备风险分、5 设备关联账号数 / op：1 >、2 >=、3 <、4 <=、5 == / decision：1 放行、2 需校验、3 拒绝、4 转复核；state：0 禁用、1 启用

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `ActionType` | `action_type` | json | `int32` | 是 | — | — |
| `Metric` | `metric` | json | `int32` | 是 | — | — |
| `Op` | `op` | json | `int32` | 是 | — | — |
| `Threshold` | `threshold` | json | `int64` | 是 | — | — |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Priority` | `priority` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `RiskListEntryItem`

> 名单条目（list_type：1 黑名单、2 白名单； / target_type：1 账号 mid、2 设备 hash、3 ip_hash；expire_at=0 表示永久）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `ListType` | `list_type` | json | `int32` | 是 | — | — |
| `TargetType` | `target_type` | json | `int32` | 是 | — | — |
| `TargetValue` | `target_value` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `RiskRuleHitItem`

> 规则命中明细（决策可解释性来源，字段口径与 riskcontrol.v1.RuleHit 一致）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Metric` | `metric` | json | `int32` | 是 | — | — |
| `Op` | `op` | json | `int32` | 是 | — | — |
| `Threshold` | `threshold` | json | `int64` | 是 | — | — |
| `Observed` | `observed` | json | `int64` | 是 | — | — |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Priority` | `priority` | json | `int32` | 是 | — | — |

### `RiskPunishmentSnapshotItem`

> 生效处罚摘要（与 riskcontrol.v1.PunishmentSnapshot 对齐，不含运营内部 reason）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PunishmentId` | `punishment_id` | json | `int64` | 是 | — | — |
| `Scope` | `scope` | json | `int32` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Permanent` | `permanent` | json | `bool` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `RemainingSeconds` | `remaining_seconds` | json | `int64` | 是 | — | — |
| `ReasonCode` | `reason_code` | json | `string` | 是 | — | — |


<!-- file: docs/api/http/admin/12-admin-risk.md -->
