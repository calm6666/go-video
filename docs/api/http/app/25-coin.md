# 终端面 · `/coin`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto） | 免鉴权 | 7 |

合计 **7** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto）（免鉴权，7 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/coin/account` | 我的硬币账户（含今日剩余额度） | `coinAccount` | `coinaccountlogic.go` |
| GET | `/coin/config` | 投币限额参数（客户端不写死日限/单片上限） | `coinConfig` | `coinconfiglogic.go` |
| POST | `/coin/toss` | 投币（扣币+记录+限额判定，request_id 幂等） | `coinToss` | `cointosslogic.go` |
| POST | `/coin/toss/cancel` | 取消投币（窗口内全额退回） | `coinTossCancel` | `cointosscancellogic.go` |
| GET | `/coin/toss/mine` | 我的投币记录 | `coinTossMine` | `cointossminelogic.go` |
| GET | `/coin/target/summary` | 单内容投币汇总（详情页） | `coinTargetSummary` | `cointargetsummarylogic.go` |
| GET | `/coin/targets/summary` | 批量内容投币汇总（列表页） | `coinTargetsSummary` | `cointargetssummarylogic.go` |

### GET `/coin/account` — 我的硬币账户（含今日剩余额度）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/coinaccounthandler.go`
- 业务实现：`gateway/app/internal/logic/coinaccountlogic.go`

请求：`ParamCoinAccount`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`CoinAccountResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinAccountData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/coin/config` — 投币限额参数（客户端不写死日限/单片上限）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/coinconfighandler.go`
- 业务实现：`gateway/app/internal/logic/coinconfiglogic.go`

请求：无参数体。

响应：`CoinTossConfigResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTossConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/coin/toss` — 投币（扣币+记录+限额判定，request_id 幂等）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/cointosshandler.go`
- 业务实现：`gateway/app/internal/logic/cointosslogic.go`

请求：`ParamCoinToss`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `TargetAid` | `target_aid` | form | `int64` | 是 | — | — |
| `Count` | `count` | form | `int32` | 否 | — | <=0 由服务按 1 处理 |
| `Platform` | `platform` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | 幂等键：同一 key 重放不重复扣币 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`CoinTossResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTossData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/coin/toss/cancel` — 取消投币（窗口内全额退回）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/cointosscancelhandler.go`
- 业务实现：`gateway/app/internal/logic/cointosscancellogic.go`

请求：`ParamCoinTossCancel`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `TargetAid` | `target_aid` | form | `int64` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`CoinTossResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTossData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/coin/toss/mine` — 我的投币记录

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/cointossminehandler.go`
- 业务实现：`gateway/app/internal/logic/cointossminelogic.go`

请求：`ParamCoinTossMine`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`CoinTossMineResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTossMineData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/coin/target/summary` — 单内容投币汇总（详情页）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/cointargetsummaryhandler.go`
- 业务实现：`gateway/app/internal/logic/cointargetsummarylogic.go`

请求：`ParamCoinTarget`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | form | `int64` | 是 | — | — |

响应：`CoinTargetSummaryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTargetSummary` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/coin/targets/summary` — 批量内容投币汇总（列表页）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/cointargetssummaryhandler.go`
- 业务实现：`gateway/app/internal/logic/cointargetssummarylogic.go`

请求：`ParamCoinTargets`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aids` | `aids` | form | `[]int64` | 是 | split | — |

响应：`CoinTargetsSummaryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTargetsSummaryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamCoinAccount`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `CoinAccountResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinAccountData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CoinTossConfigResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTossConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCoinToss`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `TargetAid` | `target_aid` | form | `int64` | 是 | — | — |
| `Count` | `count` | form | `int32` | 否 | — | <=0 由服务按 1 处理 |
| `Platform` | `platform` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | 幂等键：同一 key 重放不重复扣币 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `CoinTossResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTossData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCoinTossCancel`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `TargetAid` | `target_aid` | form | `int64` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `ParamCoinTossMine`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `CoinTossMineResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTossMineData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCoinTarget`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | form | `int64` | 是 | — | — |

### `CoinTargetSummaryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTargetSummary` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCoinTargets`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aids` | `aids` | form | `[]int64` | 是 | split | — |

### `CoinTargetsSummaryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTargetsSummaryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CoinAccountData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | false = 从未有过硬币账户，balance 为 0 |
| `Account` | `account` | json | `CoinAccount` | 是 | — | — |

### `CoinTossConfigData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DailyLimit` | `daily_limit` | json | `int64` | 是 | — | — |
| `PerTargetLimit` | `per_target_limit` | json | `int64` | 是 | — | — |
| `CancelWindowSeconds` | `cancel_window_seconds` | json | `int64` | 是 | — | — |
| `MinBalanceToToss` | `min_balance_to_toss` | json | `int64` | 是 | — | — |
| `InitialBalance` | `initial_balance` | json | `int64` | 是 | — | — |

### `CoinTossData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Accepted` | `accepted` | json | `bool` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | coin.TossRejectReason |
| `ReasonText` | `reason_text` | json | `string` | 是 | — | 服务侧可读结论（「今日还可投 2 枚」） |
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | true = 命中幂等重放，未重复扣币 |
| `Account` | `account` | json | `CoinAccount` | 是 | — | — |
| `Toss` | `toss` | json | `CoinTossInfo` | 是 | — | — |
| `FlowId` | `flow_id` | json | `int64` | 是 | — | — |

### `CoinTossMineData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tosses` | `tosses` | json | `[]CoinTossInfo` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `PageSize` | `page_size` | json | `int64` | 是 | — | — |

### `CoinTargetSummary`

> CoinTargetSummary 内容收到的硬币投影（按未取消的投币记录聚合）。 / 不含 like_count：点赞归 engagement，网关各自取，不在此伪造。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `CoinCount` | `coin_count` | json | `int64` | 是 | — | — |
| `CoinUserCount` | `coin_user_count` | json | `int64` | 是 | — | — |

### `CoinTargetsSummaryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Summaries` | `summaries` | json | `[]CoinTargetSummary` | 是 | — | — |

### `CoinAccount`

> 硬币是社区虚拟币，不是钱（与 payment 的现金余额两套账，不互换）。 / 投币的日限/单片上限/取消窗口全部由 coin 服务判定，网关只把结论翻译成端上字段， / 绝不因为「看起来像失败」就把业务结论转成 HTTP 错误——端上要靠 reason 决定文案。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Balance` | `balance` | json | `int64` | 是 | — | — |
| `TotalTossed` | `total_tossed` | json | `int64` | 是 | — | 历史口径：取消投币不回退 |
| `TodayTossed` | `today_tossed` | json | `int64` | 是 | — | — |
| `TodayLimit` | `today_limit` | json | `int64` | 是 | — | — |
| `PerTargetLimit` | `per_target_limit` | json | `int64` | 是 | — | — |
| `CancelWindowSeconds` | `cancel_window_seconds` | json | `int64` | 是 | — | — |

### `CoinTossInfo`

> CoinTossInfo 单用户对单内容的累计投币记录。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TossId` | `toss_id` | json | `int64` | 是 | — | — |
| `TargetAid` | `target_aid` | json | `int64` | 是 | — | — |
| `Count` | `count` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 生效、2 已取消 |
| `FirstTossedAt` | `first_tossed_at` | json | `int64` | 是 | — | — |
| `LastTossedAt` | `last_tossed_at` | json | `int64` | 是 | — | — |
| `CancelledAt` | `cancelled_at` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/25-coin.md -->
