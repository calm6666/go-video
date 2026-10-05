# 运营面 · `/admin/creator-revenue`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化运营面：creator-revenue 域（分成计量与结算） | 免鉴权 | 6 |
| 商业化运营面：creator-revenue 域（分成计量与结算） | AdminPermission | 5 |

合计 **11** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化运营面：creator-revenue 域（分成计量与结算）（免鉴权，6 条）

> -------------------- creator-revenue 只读面（不进 routePermissions） --------------------
> 规则、参与名单、计量台账与结算单四类读取都是只读投影，与 membership/payment/order/coin
> 读面同口径。规则的历史版本读（/rule/get?version）与结算单分项（/settlement/get）是
> 争议复核的证据来源，越是有人质疑「这钱怎么算的」时越要能立刻读到，挂判定只会挡住复核。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/creator-revenue/rule/list` | 分成规则分页（后台可见全部状态；含 ARCHIVED，历史周期按它复核） | `revenueRuleList` | `revenuerulelistlogic.go` |
| POST | `/admin/creator-revenue/rule/get` | 分成规则读取（version>0 按历史版本读，结算争议复核用） | `revenueRuleGet` | `revenuerulegetlogic.go` |
| POST | `/admin/creator-revenue/enrollment/list` | 参与名单分页（ENROLLED/LEFT/SUSPENDED；含本人确认过的规则版本） | `revenueEnrollmentList` | `revenueenrollmentlistlogic.go` |
| POST | `/admin/creator-revenue/metric/list` | 计量台账分页（某周期某内容某来源的折算结果；应计金额，未支付） | `revenueMetricList` | `revenuemetriclistlogic.go` |
| POST | `/admin/creator-revenue/settlement/list` | 结算单分页（payout_state 恒 NOT_PAYABLE：本期无出金通道） | `revenueSettlementList` | `revenuesettlementlistlogic.go` |
| POST | `/admin/creator-revenue/settlement/get` | 结算单详情（含按来源拆的分项，让作者侧质疑时能一行行对） | `revenueSettlementGet` | `revenuesettlementgetlogic.go` |

### POST `/admin/creator-revenue/rule/list` — 分成规则分页（后台可见全部状态；含 ARCHIVED，历史周期按它复核）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/revenuerulelisthandler.go`
- 业务实现：`gateway/admin/internal/logic/revenuerulelistlogic.go`

请求：`ParamRevenueRuleList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | 0 = 不过滤 |
| `SourceType` | `source_type` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`RevenueRuleListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueRuleListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/creator-revenue/rule/get` — 分成规则读取（version>0 按历史版本读，结算争议复核用）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/revenuerulegethandler.go`
- 业务实现：`gateway/admin/internal/logic/revenuerulegetlogic.go`

请求：`ParamRevenueRuleGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 否 | — | — |
| `RuleCode` | `rule_code` | json | `string` | 否 | — | — |
| `Version` | `version` | json | `int64` | 否 | — | — |

响应：`RevenueRuleGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueRuleGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/creator-revenue/enrollment/list` — 参与名单分页（ENROLLED/LEFT/SUSPENDED；含本人确认过的规则版本）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/revenueenrollmentlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/revenueenrollmentlistlogic.go`

请求：`ParamRevenueEnrollmentList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`RevenueEnrollmentListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueEnrollmentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/creator-revenue/metric/list` — 计量台账分页（某周期某内容某来源的折算结果；应计金额，未支付）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/revenuemetriclisthandler.go`
- 业务实现：`gateway/admin/internal/logic/revenuemetriclistlogic.go`

请求：`ParamRevenueMetricList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Period` | `period` | json | `string` | 否 | — | YYYYMM |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Aid` | `aid` | json | `int64` | 否 | — | — |
| `SourceType` | `source_type` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`RevenueMetricListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueMetricListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/creator-revenue/settlement/list` — 结算单分页（payout_state 恒 NOT_PAYABLE：本期无出金通道）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/revenuesettlementlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/revenuesettlementlistlogic.go`

请求：`ParamRevenueSettlementList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Period` | `period` | json | `string` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`RevenueSettlementListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueSettlementListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/creator-revenue/settlement/get` — 结算单详情（含按来源拆的分项，让作者侧质疑时能一行行对）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/revenuesettlementgethandler.go`
- 业务实现：`gateway/admin/internal/logic/revenuesettlementgetlogic.go`

请求：`ParamRevenueSettlementGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SettlementNo` | `settlement_no` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | 非 0 时服务校验归属 |

响应：`RevenueSettlementGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueSettlementGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 商业化运营面：creator-revenue 域（分成计量与结算）（AdminPermission，5 条）

> -------------------- creator-revenue 写面（受 AdminPermission 保护） --------------------
> 五条写入口按「后果差一个量级」分列，不合并：
>   - revenue:rule/update 只写 DRAFT（对未来没有任何影响）；
>   - revenue:rule/publish 切换 ACTIVE，立刻改变所有作者下期应计金额——本域最重的一步，
>     必须能单独授予与单独收回（与 collector 的 policy/update 与 policy/enable 同一口径）；
>   - revenue:enrollment/update 暂停/恢复某个作者的收益资格，直接决定这一期要不要给他出单；
>   - revenue:settlement/create 出单/重算，force_void_confirmed 还会把已确认单作废；
>   - revenue:settlement/confirm 冻结金额，之后只能作废重算、不能改数。
> 出单与确认分开是因为「算完了」和「认了」是两件事：合在一点上会让人一键生成并冻结一批
> 还没人复核的账。出金/提现/打款没有对应权限点，因为契约里就没有这些方法（本期范围外）。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/creator-revenue/rule/upsert` | 新建/修改分成规则草稿（无 state 位，改完不会自动生效；reason 必填） | `revenue:rule` / `update` | `revenueRuleUpsert` | `revenueruleupsertlogic.go` |
| POST | `/admin/creator-revenue/rule/state` | 规则状态迁移（DRAFT→ACTIVE→ARCHIVED；ACTIVE 不可原地改价） | `revenue:rule` / `publish` | `revenueRuleState` | `revenuerulestatelogic.go` |
| POST | `/admin/creator-revenue/enrollment/state` | 暂停/恢复参与（违规暂停期间不结算；加入与退出归创作者本人，不开后台口） | `revenue:enrollment` / `update` | `revenueEnrollmentState` | `revenueenrollmentstatelogic.go` |
| POST | `/admin/creator-revenue/settlement/generate` | 生成/重算周期结算单（mid=0 全量；force_void_confirmed 是危险位且必须带 reason） | `revenue:settlement` / `create` | `revenueSettlementGenerate` | `revenuesettlementgeneratelogic.go` |
| POST | `/admin/creator-revenue/settlement/confirm` | 确认结算单（金额冻结；只是认账，**不是钱已付出**，payout_state 恒 NOT_PAYABLE） | `revenue:settlement` / `confirm` | `revenueSettlementConfirm` | `revenuesettlementconfirmlogic.go` |

### POST `/admin/creator-revenue/rule/upsert` — 新建/修改分成规则草稿（无 state 位，改完不会自动生效；reason 必填）

- 权限口径：AdminPermission · 权限点 `revenue:rule` / `update`
- goctl 入口：`gateway/admin/internal/handler/revenueruleupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/revenueruleupsertlogic.go`

请求：`ParamRevenueRuleUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 否 | — | — |
| `RuleCode` | `rule_code` | json | `string` | 是 | — | — |
| `SourceType` | `source_type` | json | `int32` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `UnitPricePer1000Minor` | `unit_price_per_1000_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Unit` | `unit` | json | `string` | 是 | — | — |
| `MinQuantity` | `min_quantity` | json | `int64` | 否 | — | — |
| `MonthlyCapMinor` | `monthly_cap_minor` | json | `int64` | 否 | — | 0 = 不限 |
| `EffectiveFrom` | `effective_from` | json | `int64` | 否 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`RevenueRuleUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueRuleUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/creator-revenue/rule/state` — 规则状态迁移（DRAFT→ACTIVE→ARCHIVED；ACTIVE 不可原地改价）

- 权限口径：AdminPermission · 权限点 `revenue:rule` / `publish`
- goctl 入口：`gateway/admin/internal/handler/revenuerulestatehandler.go`
- 业务实现：`gateway/admin/internal/logic/revenuerulestatelogic.go`

请求：`ParamRevenueRuleState`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`RevenueRuleStateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueRuleStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/creator-revenue/enrollment/state` — 暂停/恢复参与（违规暂停期间不结算；加入与退出归创作者本人，不开后台口）

- 权限口径：AdminPermission · 权限点 `revenue:enrollment` / `update`
- goctl 入口：`gateway/admin/internal/handler/revenueenrollmentstatehandler.go`
- 业务实现：`gateway/admin/internal/logic/revenueenrollmentstatelogic.go`

请求：`ParamRevenueEnrollmentState`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | 只允许 ENROLLED / SUSPENDED |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`RevenueEnrollmentStateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueEnrollmentStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/creator-revenue/settlement/generate` — 生成/重算周期结算单（mid=0 全量；force_void_confirmed 是危险位且必须带 reason）

- 权限口径：AdminPermission · 权限点 `revenue:settlement` / `create`
- goctl 入口：`gateway/admin/internal/handler/revenuesettlementgeneratehandler.go`
- 业务实现：`gateway/admin/internal/logic/revenuesettlementgeneratelogic.go`

请求：`ParamRevenueSettlementGenerate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Period` | `period` | json | `string` | 是 | — | YYYYMM |
| `Mid` | `mid` | json | `int64` | 否 | — | 0 = 该周期全量 |
| `ForceVoidConfirmed` | `force_void_confirmed` | json | `bool` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | force_void_confirmed=true 时必填 |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`RevenueSettlementGenerateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueSettlementGenerateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/creator-revenue/settlement/confirm` — 确认结算单（金额冻结；只是认账，**不是钱已付出**，payout_state 恒 NOT_PAYABLE）

- 权限口径：AdminPermission · 权限点 `revenue:settlement` / `confirm`
- goctl 入口：`gateway/admin/internal/handler/revenuesettlementconfirmhandler.go`
- 业务实现：`gateway/admin/internal/logic/revenuesettlementconfirmlogic.go`

请求：`ParamRevenueSettlementConfirm`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SettlementNos` | `settlement_nos` | json | `[]string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`RevenueSettlementConfirmResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueSettlementConfirmData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamRevenueRuleList`

> ParamRevenueRuleList 1:1 对应 ListRevenueRulesReq（后台可见全部状态；创作者端只查 ACTIVE）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | 0 = 不过滤 |
| `SourceType` | `source_type` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `RevenueRuleListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueRuleListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueRuleGet`

> ParamRevenueRuleGet 1:1 对应 GetRevenueRuleReq；version>0 时按历史版本读， / 这是结算争议复核的关键能力（「这一期到底是按哪版规则算的」必须能查回来）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 否 | — | — |
| `RuleCode` | `rule_code` | json | `string` | 否 | — | — |
| `Version` | `version` | json | `int64` | 否 | — | — |

### `RevenueRuleGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueRuleGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueEnrollmentList`

> ParamRevenueEnrollmentList 1:1 对应 ListEnrollmentsReq。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `RevenueEnrollmentListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueEnrollmentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueMetricList`

> ParamRevenueMetricList 1:1 对应 ListRevenueMetricsReq（计量台账只读；写入方是 spm/coin/cron）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Period` | `period` | json | `string` | 否 | — | YYYYMM |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `Aid` | `aid` | json | `int64` | 否 | — | — |
| `SourceType` | `source_type` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `RevenueMetricListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueMetricListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueSettlementList`

> ParamRevenueSettlementList 1:1 对应 ListSettlementsReq。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Period` | `period` | json | `string` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `RevenueSettlementListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueSettlementListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueSettlementGet`

> ParamRevenueSettlementGet 1:1 对应 GetSettlementReq（含分项 items）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SettlementNo` | `settlement_no` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | 非 0 时服务校验归属 |

### `RevenueSettlementGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueSettlementGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueRuleUpsert`

> ParamRevenueRuleUpsert 1:1 对应 UpsertRevenueRuleReq 的可填位。 / 表单**没有 state 位**：改草稿与让某版规则生效是两个权限点（revenue:rule/update 与 / revenue:rule/publish），合并会让「调一下单价」顺带改变所有作者的下期应计。 / effective_from 决定追溯边界——晚于它的周期才用本规则，历史单不受影响； / 单价非负、门槛与封顶的组合合法性由 creator-revenue 判定。reason 必填（改单价是有后果的动作）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 否 | — | — |
| `RuleCode` | `rule_code` | json | `string` | 是 | — | — |
| `SourceType` | `source_type` | json | `int32` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `UnitPricePer1000Minor` | `unit_price_per_1000_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Unit` | `unit` | json | `string` | 是 | — | — |
| `MinQuantity` | `min_quantity` | json | `int64` | 否 | — | — |
| `MonthlyCapMinor` | `monthly_cap_minor` | json | `int64` | 否 | — | 0 = 不限 |
| `EffectiveFrom` | `effective_from` | json | `int64` | 否 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `RevenueRuleUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueRuleUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueRuleState`

> ParamRevenueRuleState 1:1 对应 SetRevenueRuleStateReq。只允许 DRAFT→ACTIVE、 / ACTIVE→ARCHIVED、DRAFT→ARCHIVED；ACTIVE 规则不可原地改价（必须先新草稿）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `RevenueRuleStateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueRuleStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueEnrollmentState`

> ParamRevenueEnrollmentState 1:1 对应 SetEnrollmentStateReq：运营侧只有暂停/恢复 / （ENROLLED↔SUSPENDED），违规暂停期间收益不结算。加入与退出不是运营动作，见上面说明。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | 只允许 ENROLLED / SUSPENDED |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `RevenueEnrollmentStateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueEnrollmentStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueSettlementGenerate`

> ParamRevenueSettlementGenerate 1:1 对应 GenerateSettlementReq。 / mid=0 表示该周期全量出单（一次可生成多张，settlements 只回前若干条、truncated 说明是否截断）。 / force_void_confirmed 是**危险位**：true 才允许把已 CONFIRMED 的单置 VOIDED 重算， / 此时 reason 必填；金额冻结的语义就是靠这一位显式化，网关不默认关也不代为打开。 / 只对 ENROLLED 且非 SUSPENDED 的作者出单，幂等由 (period, mid) 保证。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Period` | `period` | json | `string` | 是 | — | YYYYMM |
| `Mid` | `mid` | json | `int64` | 否 | — | 0 = 该周期全量 |
| `ForceVoidConfirmed` | `force_void_confirmed` | json | `bool` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | force_void_confirmed=true 时必填 |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `RevenueSettlementGenerateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueSettlementGenerateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevenueSettlementConfirm`

> ParamRevenueSettlementConfirm 1:1 对应 ConfirmSettlementReq（批量确认，空列表由服务拒绝）。 / 确认=金额冻结不可重算，是「这份账认了」的动作；它**不等于**钱已经付出—— / payout_state 恒 NOT_PAYABLE，本期没有出金通道。failed_nos 原样回，不合并成「全部成功」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SettlementNos` | `settlement_nos` | json | `[]string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `RevenueSettlementConfirmResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevenueSettlementConfirmData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `RevenueRuleListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]RevenueRuleItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `RevenueRuleGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Rule` | `rule` | json | `RevenueRuleItem` | 是 | — | — |

### `RevenueEnrollmentListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]RevenueEnrollmentItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `RevenueMetricListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]RevenueMetricItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `RevenueSettlementListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]RevenueSettlementItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `RevenueSettlementGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Settlement` | `settlement` | json | `RevenueSettlementItem` | 是 | — | — |
| `Items` | `items` | json | `[]RevenueSettlementDetailItem` | 是 | — | — |

### `RevenueRuleUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rule` | `rule` | json | `RevenueRuleItem` | 是 | — | — |

### `RevenueRuleStateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rule` | `rule` | json | `RevenueRuleItem` | 是 | — | — |

### `RevenueEnrollmentStateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `Enrollment` | `enrollment` | json | `RevenueEnrollmentItem` | 是 | — | — |

### `RevenueSettlementGenerateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | 已有未作废单且本次未强制重算 |
| `Generated` | `generated` | json | `int64` | 是 | — | 本次出单数（全量时 >1） |
| `Truncated` | `truncated` | json | `bool` | 是 | — | true = 完整结果请查 /settlement/list |
| `Settlements` | `settlements` | json | `[]RevenueSettlementItem` | 是 | — | — |

### `RevenueSettlementConfirmData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Confirmed` | `confirmed` | json | `int64` | 是 | — | — |
| `FailedNos` | `failed_nos` | json | `[]string` | 是 | — | 状态不符/已作废的单号，原样回 |

### `RevenueRuleItem`

> 契约来源 services/creator-revenue/rpc/creatorrevenue.proto（冻结）。 /  / 本域语义边界（AGENTS.md §1/§5/§7，请逐条读）： /   - creator-revenue 持有分成规则、参与关系、**折算后的**计量台账与结算单； /     计量原始事实（有效观看时长、收到的投币、互动）归 spm / coin，本域不复制它们的主数据； /   - 台账金额是**应计金额**（分），不是已支付金额； /   - **出金不在本期范围内**：本服务只做到「算出该给多少并落成可审计的结算单」。 /     提现、打款、银行卡、发票、税务、对账文件一概不开接口，也不做后台路由； /     结算单的 payout_state **恒为 NOT_PAYABLE**、创作者概览的 payout_available **恒为 false**， /     任何调用方都不能从这里得到「钱已出账」的结论。这两个字段保留在投影里， /     就是为了让「没打款」在数据与响应里都可见，而不是靠 README 提醒； /   - 计量输入只来自行为分析与投币事实，不接受任何广告参数（§7）。 / 金额一律 int64 最小货币单位（分）+ 显式 currency；单价按「每 1000 单位」计， / 避免时长/互动这类小颗粒度收益被整除成 0，网关不做二次折算。 /  / 刻意**不开**的路由（理由写在这里，不是漏实现）： /   - EnrollCreator / LeavePlan：加入/退出分成计划是**创作者本人**的动作，且必须带 /     「本人已确认的规则版本」（agreed_rule_version），归 gateway/app。后台代签既拿不到 /     真实同意，也无法在争议时作为证据； /   - RecordRevenueMetric：计量事实的写入方是 spm / coin（或 cron 回填）。后台代写等于 /     污染计量源——一旦允许手工塞一条 quantity，这份台账就再也无法用来复核规则是否被如实执行； /   - GetEnrollment / GetRevenueSummary：创作者本人视角的读（我的参与状态、本月预估与累计）， /     归 gateway/app；后台的等价读能力已由 /enrollment/list 与 /settlement/list 覆盖； /   - 出金/提现/打款/退款到卡/发票/对账文件：真实资金能力，本期范围外，契约里没有这些方法， /     网关也不会凭空调用其它服务凑出一条「钱已付出」的结论。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 是 | — | — |
| `RuleCode` | `rule_code` | json | `string` | 是 | — | 稳定编码，计量台账按它定位规则 |
| `SourceType` | `source_type` | json | `int32` | 是 | — | RevenueSourceType：1 会员观看 2 投币 3 互动 4 活动激励 |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `UnitPricePer1000Minor` | `unit_price_per_1000_minor` | json | `int64` | 是 | — | 每 1000 单位的分价，负数由服务拒 |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Unit` | `unit` | json | `string` | 是 | — | minute / coin / interaction |
| `MinQuantity` | `min_quantity` | json | `int64` | 是 | — | 低于此量不结算（防刷门槛） |
| `MonthlyCapMinor` | `monthly_cap_minor` | json | `int64` | 是 | — | 单用户单来源月度封顶，0 表示不限 |
| `State` | `state` | json | `int32` | 是 | — | RuleState：1 DRAFT 2 ACTIVE 3 ARCHIVED |
| `EffectiveFrom` | `effective_from` | json | `int64` | 是 | — | 晚于它的周期才用本规则（不追溯改历史单） |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `CreatedBy` | `created_by` | json | `string` | 是 | — | — |
| `UpdatedBy` | `updated_by` | json | `string` | 是 | — | — |

### `RevenueEnrollmentItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | EnrollmentState：1 ENROLLED 2 LEFT 3 SUSPENDED（违规暂停，不结算） |
| `AgreedRuleVersion` | `agreed_rule_version` | json | `int64` | 是 | — | 参与者确认时看到的规则版本，必须可回溯 |
| `EnrolledAt` | `enrolled_at` | json | `int64` | 是 | — | — |
| `LeftAt` | `left_at` | json | `int64` | 是 | — | — |
| `UpdatedAt` | `updated_at` | json | `int64` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | 自助为 "user" |
| `Remark` | `remark` | json | `string` | 是 | — | — |

### `RevenueMetricItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricId` | `metric_id` | json | `int64` | 是 | — | — |
| `Period` | `period` | json | `string` | 是 | — | YYYYMM |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `SourceType` | `source_type` | json | `int32` | 是 | — | — |
| `RuleCode` | `rule_code` | json | `string` | 是 | — | — |
| `RuleVersion` | `rule_version` | json | `int64` | 是 | — | 计算时锁定的规则版本（重算口径可追溯） |
| `Quantity` | `quantity` | json | `int64` | 是 | — | — |
| `Unit` | `unit` | json | `string` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | 应计金额（分），封顶前 |
| `CappedAmountMinor` | `capped_amount_minor` | json | `int64` | 是 | — | 封顶/门槛后的实际应计 |
| `SourceDetail` | `source_detail` | json | `string` | 是 | — | 计算依据摘要，不含 PII |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `RevenueSettlementItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SettlementNo` | `settlement_no` | json | `string` | 是 | — | — |
| `Period` | `period` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | 应计合计（分），不是已支付 |
| `CapAppliedMinor` | `cap_applied_minor` | json | `int64` | 是 | — | 因封顶被扣减的额度（透明化，不让运营猜） |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `MetricCount` | `metric_count` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | SettlementState：1 DRAFT 2 CONFIRMED 3 VOIDED |
| `PayoutState` | `payout_state` | json | `int32` | 是 | — | **恒为 1 NOT_PAYABLE**：本项目没有出金通道 |
| `ConfirmedAt` | `confirmed_at` | json | `int64` | 是 | — | — |
| `ConfirmedBy` | `confirmed_by` | json | `string` | 是 | — | — |
| `VoidReason` | `void_reason` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `RevenueSettlementDetailItem`

> RevenueSettlementDetailItem 投影 creatorrevenue.v1.SettlementItem（结算单分项，按来源拆开）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SourceType` | `source_type` | json | `int32` | 是 | — | — |
| `RuleCode` | `rule_code` | json | `string` | 是 | — | — |
| `Quantity` | `quantity` | json | `int64` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/29-admin-creator-revenue.md -->
