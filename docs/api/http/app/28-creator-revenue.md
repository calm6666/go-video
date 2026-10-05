# 终端面 · `/creator/revenue`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto） | 免鉴权 | 8 |

合计 **8** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto）（免鉴权，8 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/creator/revenue/summary` | 我的收益概览（应计金额，非已到账） | `revSummary` | `revsummarylogic.go` |
| GET | `/creator/revenue/rules` | 生效中的分成规则（终端面只读 ACTIVE） | `revRules` | `revruleslogic.go` |
| GET | `/creator/revenue/enrollment` | 我的分成参与状态 | `revEnrollment` | `revenrollmentlogic.go` |
| POST | `/creator/revenue/enroll` | 参加分成计划（必须带已确认的规则版本） | `revEnroll` | `revenrolllogic.go` |
| POST | `/creator/revenue/leave` | 退出分成计划 | `revLeave` | `revleavelogic.go` |
| GET | `/creator/revenue/metrics` | 我的收益计量明细 | `revMetrics` | `revmetricslogic.go` |
| GET | `/creator/revenue/settlements` | 我的结算单列表 | `revSettlements` | `revsettlementslogic.go` |
| GET | `/creator/revenue/settlement` | 结算单详情（含分项，便于核对） | `revSettlement` | `revsettlementlogic.go` |

### GET `/creator/revenue/summary` — 我的收益概览（应计金额，非已到账）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/revsummaryhandler.go`
- 业务实现：`gateway/app/internal/logic/revsummarylogic.go`

请求：`ParamRevSummary`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`RevSummaryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevSummaryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/creator/revenue/rules` — 生效中的分成规则（终端面只读 ACTIVE）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/revruleshandler.go`
- 业务实现：`gateway/app/internal/logic/revruleslogic.go`

请求：`ParamRevRules`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SourceType` | `source_type` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`RevRulesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevRulesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/creator/revenue/enrollment` — 我的分成参与状态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/revenrollmenthandler.go`
- 业务实现：`gateway/app/internal/logic/revenrollmentlogic.go`

请求：`ParamRevEnrollment`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`RevEnrollmentResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Data` | `data` | json | `RevEnrollment` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/creator/revenue/enroll` — 参加分成计划（必须带已确认的规则版本）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/revenrollhandler.go`
- 业务实现：`gateway/app/internal/logic/revenrolllogic.go`

请求：`ParamRevEnroll`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `AgreedRuleVersion` | `agreed_rule_version` | form | `int64` | 是 | — | 必填：未确认规则版本不得参加 |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`RevEnrollResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevEnrollData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/creator/revenue/leave` — 退出分成计划

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/revleavehandler.go`
- 业务实现：`gateway/app/internal/logic/revleavelogic.go`

请求：`ParamRevLeave`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`RevEnrollResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevEnrollData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/creator/revenue/metrics` — 我的收益计量明细

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/revmetricshandler.go`
- 业务实现：`gateway/app/internal/logic/revmetricslogic.go`

请求：`ParamRevMetrics`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Period` | `period` | form | `string` | 否 | — | — |
| `Aid` | `aid` | form | `int64` | 否 | — | — |
| `SourceType` | `source_type` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`RevMetricsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevMetricsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/creator/revenue/settlements` — 我的结算单列表

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/revsettlementshandler.go`
- 业务实现：`gateway/app/internal/logic/revsettlementslogic.go`

请求：`ParamRevSettlements`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Period` | `period` | form | `string` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`RevSettlementsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevSettlementsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/creator/revenue/settlement` — 结算单详情（含分项，便于核对）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/revsettlementhandler.go`
- 业务实现：`gateway/app/internal/logic/revsettlementlogic.go`

请求：`ParamRevSettlement`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `SettlementNo` | `settlement_no` | form | `string` | 是 | — | — |

响应：`RevSettlementDetailResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevSettlementDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamRevSummary`

> 出金不在范围内：本组路由只读「应计金额」与结算单，payout_available 恒 false， / 网关不得把「已确认结算」渲染成「已到账」——那是两件事，混淆会构成对用户的虚假承诺。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `RevSummaryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevSummaryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevRules`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SourceType` | `source_type` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `RevRulesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevRulesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevEnrollment`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `RevEnrollmentResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Data` | `data` | json | `RevEnrollment` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevEnroll`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `AgreedRuleVersion` | `agreed_rule_version` | form | `int64` | 是 | — | 必填：未确认规则版本不得参加 |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `RevEnrollResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevEnrollData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevLeave`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `ParamRevMetrics`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Period` | `period` | form | `string` | 否 | — | — |
| `Aid` | `aid` | form | `int64` | 否 | — | — |
| `SourceType` | `source_type` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `RevMetricsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevMetricsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevSettlements`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Period` | `period` | form | `string` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `RevSettlementsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevSettlementsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRevSettlement`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `SettlementNo` | `settlement_no` | form | `string` | 是 | — | — |

### `RevSettlementDetailResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RevSettlementDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `RevSummaryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Enrolled` | `enrolled` | json | `bool` | 是 | — | — |
| `EnrollmentState` | `enrollment_state` | json | `int32` | 是 | — | — |
| `CurrentPeriod` | `current_period` | json | `string` | 是 | — | — |
| `CurrentEstimateMinor` | `current_estimate_minor` | json | `int64` | 是 | — | 未结算，会随更正变动 |
| `TotalConfirmedMinor` | `total_confirmed_minor` | json | `int64` | 是 | — | 已确认应计，非已支付 |
| `LastSettledPeriod` | `last_settled_period` | json | `int64` | 是 | — | — |
| `PayoutAvailable` | `payout_available` | json | `bool` | 是 | — | 恒 false |
| `PayoutNote` | `payout_note` | json | `string` | 是 | — | — |

### `RevRulesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rules` | `rules` | json | `[]RevRule` | 是 | — | 终端面固定只含 ACTIVE |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `PageSize` | `page_size` | json | `int64` | 是 | — | — |

### `RevEnrollment`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 参加、2 退出、3 暂停 |
| `AgreedRuleVersion` | `agreed_rule_version` | json | `int64` | 是 | — | — |
| `EnrolledAt` | `enrolled_at` | json | `int64` | 是 | — | — |
| `LeftAt` | `left_at` | json | `int64` | 是 | — | — |
| `UpdatedAt` | `updated_at` | json | `int64` | 是 | — | — |

### `RevEnrollData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `Enrollment` | `enrollment` | json | `RevEnrollment` | 是 | — | — |

### `RevMetricsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Metrics` | `metrics` | json | `[]RevMetric` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `PageSize` | `page_size` | json | `int64` | 是 | — | — |

### `RevSettlementsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Settlements` | `settlements` | json | `[]RevSettlement` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `PageSize` | `page_size` | json | `int64` | 是 | — | — |

### `RevSettlementDetailData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Settlement` | `settlement` | json | `RevSettlement` | 是 | — | — |
| `Items` | `items` | json | `[]RevSettlementItem` | 是 | — | — |

### `RevRule`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleCode` | `rule_code` | json | `string` | 是 | — | — |
| `SourceType` | `source_type` | json | `int32` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `UnitPricePer1000Minor` | `unit_price_per_1000_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Unit` | `unit` | json | `string` | 是 | — | — |
| `MinQuantity` | `min_quantity` | json | `int64` | 是 | — | — |
| `MonthlyCapMinor` | `monthly_cap_minor` | json | `int64` | 是 | — | 0 = 不限 |
| `State` | `state` | json | `int32` | 是 | — | — |
| `EffectiveFrom` | `effective_from` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |

### `RevMetric`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Period` | `period` | json | `string` | 是 | — | — |
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `SourceType` | `source_type` | json | `int32` | 是 | — | — |
| `RuleCode` | `rule_code` | json | `string` | 是 | — | — |
| `Quantity` | `quantity` | json | `int64` | 是 | — | — |
| `Unit` | `unit` | json | `string` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | — |
| `CappedAmountMinor` | `capped_amount_minor` | json | `int64` | 是 | — | — |
| `SourceDetail` | `source_detail` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `RevSettlement`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SettlementNo` | `settlement_no` | json | `string` | 是 | — | — |
| `Period` | `period` | json | `string` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | — |
| `CapAppliedMinor` | `cap_applied_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `MetricCount` | `metric_count` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 草稿、2 已确认、3 已作废 |
| `PayoutState` | `payout_state` | json | `int32` | 是 | — | 恒 1 NOT_PAYABLE |
| `ConfirmedAt` | `confirmed_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `RevSettlementItem`

> RevSettlementItem 结算单分项（按收益来源拆开，让作者能核对钱是怎么算出来的）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SourceType` | `source_type` | json | `int32` | 是 | — | — |
| `RuleCode` | `rule_code` | json | `string` | 是 | — | — |
| `Quantity` | `quantity` | json | `int64` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/28-creator-revenue.md -->
