# 运营面 · `/admin/payment`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化运营面：payment 域（资金台账） | 免鉴权 | 6 |
| 商业化运营面：payment 域（资金台账） | AdminPermission | 2 |

合计 **8** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化运营面：payment 域（资金台账）（免鉴权，6 条）

> -------------------- payment 只读面（不进 routePermissions） --------------------
> 钱包、四类台账与渠道自述都是读取，与 membership/collector 读面同口径不挂判定。
> /channel/describe 尤其不能挂：它是「让运营页读到沙箱事实」的入口，排障时可能还没有权限数据。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/payment/wallet/get` | 余额查询（frozen_minor 恒 0，可用余额只看 balance_minor） | `paymentWalletGet` | `paymentwalletgetlogic.go` |
| POST | `/admin/payment/recharge/list` | 充值台账分页（mid=0 跨用户） | `paymentRechargeList` | `paymentrechargelistlogic.go` |
| POST | `/admin/payment/payment/list` | 支付台账分页（按状态/支付方式/时间窗） | `paymentList` | `paymentlistlogic.go` |
| POST | `/admin/payment/refund/list` | 退款台账分页（只回退款单，不发起退款） | `paymentRefundList` | `paymentrefundlistlogic.go` |
| POST | `/admin/payment/flow/list` | 资金流水分页（只增台账；充值/消费/退款/运营调整四类） | `paymentFlowList` | `paymentflowlistlogic.go` |
| POST | `/admin/payment/channel/describe` | 渠道能力自述：sandbox_only 与每渠道 real_money（恒 false），让「没有真实资金」可查询 | `paymentChannelDescribe` | `paymentchanneldescribelogic.go` |

### POST `/admin/payment/wallet/get` — 余额查询（frozen_minor 恒 0，可用余额只看 balance_minor）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/paymentwalletgethandler.go`
- 业务实现：`gateway/admin/internal/logic/paymentwalletgetlogic.go`

请求：`ParamPaymentWalletGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 否 | — | — |

响应：`PaymentWalletGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentWalletGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/payment/recharge/list` — 充值台账分页（mid=0 跨用户）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/paymentrechargelisthandler.go`
- 业务实现：`gateway/admin/internal/logic/paymentrechargelistlogic.go`

请求：`ParamPaymentRechargeList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`PaymentRechargeListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentRechargeListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/payment/payment/list` — 支付台账分页（按状态/支付方式/时间窗）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/paymentlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/paymentlistlogic.go`

请求：`ParamPaymentList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Method` | `method` | json | `int32` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`PaymentListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/payment/refund/list` — 退款台账分页（只回退款单，不发起退款）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/paymentrefundlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/paymentrefundlistlogic.go`

请求：`ParamPaymentRefundList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `PaymentNo` | `payment_no` | json | `string` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`PaymentRefundListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentRefundListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/payment/flow/list` — 资金流水分页（只增台账；充值/消费/退款/运营调整四类）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/paymentflowlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/paymentflowlistlogic.go`

请求：`ParamPaymentFlowList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `BizType` | `biz_type` | json | `int32` | 否 | — | — |
| `BizNo` | `biz_no` | json | `string` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`PaymentFlowListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentFlowListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/payment/channel/describe` — 渠道能力自述：sandbox_only 与每渠道 real_money（恒 false），让「没有真实资金」可查询

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/paymentchanneldescribehandler.go`
- 业务实现：`gateway/admin/internal/logic/paymentchanneldescribelogic.go`

请求：`ParamPaymentChannelDescribe`

（该类型无字段：空请求 / 空响应。）

响应：`PaymentChannelDescribeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentChannelDescribeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 商业化运营面：payment 域（资金台账）（AdminPermission，2 条）

> -------------------- payment 写面（受 AdminPermission 保护） --------------------
> 两条口各占一个权限点，拆开的理由是「钱的来源是否可回溯」：
>   - payment:recharge/settle 结算的是一张**用户自己开出来**的充值单，影响面受限且单据可追；
>   - payment:balance/adjust 没有任何上游单据，凭空写一笔余额，是本仓库唯一的直接改账口，
>     必须能单独授予、单独收回，绝不与结算合并（合并等于「能结算就能白送钱」）。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/payment/recharge/settle` | 结算沙箱充值单（置 SUCCESS 并入账；只动本地台账，无渠道回调） | `payment:recharge` / `settle` | `paymentRechargeSettle` | `paymentrechargesettlelogic.go` |
| POST | `/admin/payment/balance/adjust` | 余额差错更正（唯一直接改资金台账的后台口；必须 idempotency_key + operator + reason） | `payment:balance` / `adjust` | `paymentBalanceAdjust` | `paymentbalanceadjustlogic.go` |

### POST `/admin/payment/recharge/settle` — 结算沙箱充值单（置 SUCCESS 并入账；只动本地台账，无渠道回调）

- 权限口径：AdminPermission · 权限点 `payment:recharge` / `settle`
- goctl 入口：`gateway/admin/internal/handler/paymentrechargesettlehandler.go`
- 业务实现：`gateway/admin/internal/logic/paymentrechargesettlelogic.go`

请求：`ParamPaymentRechargeSettle`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RechargeNo` | `recharge_no` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0，渲染成 proto 的 operator 字符串 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，重复提交回首次结论 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`PaymentRechargeSettleResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentRechargeSettleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/payment/balance/adjust` — 余额差错更正（唯一直接改资金台账的后台口；必须 idempotency_key + operator + reason）

- 权限口径：AdminPermission · 权限点 `payment:balance` / `adjust`
- goctl 入口：`gateway/admin/internal/handler/paymentbalanceadjusthandler.go`
- 业务实现：`gateway/admin/internal/logic/paymentbalanceadjustlogic.go`

请求：`ParamPaymentBalanceAdjust`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `DeltaMinor` | `delta_minor` | json | `int64` | 是 | — | 正入负出，不允许 0 |
| `Currency` | `currency` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，必填 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`PaymentBalanceAdjustResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentBalanceAdjustData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamPaymentWalletGet`

> ParamPaymentWalletGet 1:1 对应 GetWalletReq（currency 空表示默认币种）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 否 | — | — |

### `PaymentWalletGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentWalletGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPaymentRechargeList`

> ParamPaymentRechargeList 1:1 对应 ListRechargesReq；mid=0 才是跨用户（运营面）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `PaymentRechargeListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentRechargeListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPaymentList`

> ParamPaymentList 1:1 对应 ListPaymentsReq。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Method` | `method` | json | `int32` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `PaymentListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPaymentRefundList`

> ParamPaymentRefundList 1:1 对应 ListRefundsReq。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `PaymentNo` | `payment_no` | json | `string` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `PaymentRefundListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentRefundListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPaymentFlowList`

> ParamPaymentFlowList 1:1 对应 ListFlowsReq。流水是只增台账，本域读面没有任何写能力。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `BizType` | `biz_type` | json | `int32` | 否 | — | — |
| `BizNo` | `biz_no` | json | `string` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `PaymentFlowListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentFlowListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPaymentChannelDescribe`

> ParamPaymentChannelDescribe 对应 DescribeChannelsReq（空请求）。 / 这个路由存在的理由是「把沙箱、没有真实资金说成可查询的事实」，而不是只写在 README 里： / 运营页与排障都要能直接读到 sandbox_only 与每个渠道的 real_money（恒 false）。

（该类型无字段：空请求 / 空响应。）

### `PaymentChannelDescribeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentChannelDescribeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPaymentRechargeSettle`

> ParamPaymentRechargeSettle 1:1 对应 SettleSandboxRechargeReq。 / 沙箱下这一步就是「钱到账」的唯一入口——只动本地台账，不请求渠道、不等回调。 / 已终态单由 payment 拒绝重复推进；后台不得用它去「补」一张不该成功的单（reason 会进台账）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RechargeNo` | `recharge_no` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0，渲染成 proto 的 operator 字符串 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，重复提交回首次结论 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `PaymentRechargeSettleResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentRechargeSettleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPaymentBalanceAdjust`

> ParamPaymentBalanceAdjust 1:1 对应 AdjustBalanceReq。 /  / **这是本仓库唯一能直接改动资金台账（余额）的后台口**，用途严格限定为沙箱差错更正： /   - 沙箱里的余额是台账加出来的，测试与迁移过程中会出现「数对不上」，这一步是唯一的 /     人工纠偏入口；它不是「送钱」功能，也不产生任何真实资金移动； /   - 必须带 idempotency_key（→ request_id）：没有它，手抖点两次就是两笔调整流水； /   - 必须带 operator（>0）与非空 reason：payment 侧把这一笔记成 FLOW_BIZ_TYPE_ADMIN_ADJUST /     并留审计（AGENTS.md §5：资金台账归 payment，服务侧是唯一写主）； /   - delta_minor 正负皆可但不允许为 0；余额不足扣成负数、币种不匹配、超上限由 payment 判定， /     网关不做任何「看起来更合理」的兜底或默认值。 / 除差错更正外的动钱路径都应经订单：买会员/硬币包走 trade-order，退款走订单审批。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `DeltaMinor` | `delta_minor` | json | `int64` | 是 | — | 正入负出，不允许 0 |
| `Currency` | `currency` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，必填 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `PaymentBalanceAdjustResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PaymentBalanceAdjustData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `PaymentWalletGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Wallet` | `wallet` | json | `PaymentWalletItem` | 是 | — | — |

### `PaymentRechargeListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]PaymentRechargeItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `PaymentListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]PaymentPaymentItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `PaymentRefundListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]PaymentRefundItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `PaymentFlowListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]PaymentFlowItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `PaymentChannelDescribeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SandboxOnly` | `sandbox_only` | json | `bool` | 是 | — | — |
| `Channels` | `channels` | json | `[]PaymentChannelItem` | 是 | — | — |
| `CurrencyDefault` | `currency_default` | json | `string` | 是 | — | — |

### `PaymentRechargeSettleData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `FlowId` | `flow_id` | json | `int64` | 是 | — | — |
| `Recharge` | `recharge` | json | `PaymentRechargeItem` | 是 | — | — |
| `Wallet` | `wallet` | json | `PaymentWalletItem` | 是 | — | — |

### `PaymentBalanceAdjustData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `FlowId` | `flow_id` | json | `int64` | 是 | — | — |
| `Wallet` | `wallet` | json | `PaymentWalletItem` | 是 | — | — |

### `PaymentWalletItem`

> 契约来源 services/payment/rpc/payment.proto（冻结）。payment 是「钱」的唯一写入口（§5）： / 余额、充值单、支付单、退款单、资金流水；其它服务只保存它返回的单据号，不得自记余额。 /  / 沙箱语义（必须读清楚，否则会误判成真实收单）： /   - 唯一可用渠道是 SANDBOX。开单/结算/支付只在本地台账上改数，**不请求任何第三方支付网关**、 /     不产生真实资金移动，因此「充值结算 → 余额到账」是台账内的真实推进，不是伪造成功； /   - 需要真实渠道才成立的能力一概不开接口，也不做后台路由：渠道异步回调验签、退款到银行卡、 /     提现、打款出金、对账文件、发票税务； /   - frozen_minor 本项目恒为 0（无预授权流程），读侧不要把它当可用余额； /   - 硬币是另一套账（coin 服务），与本域现金余额不互换。 / 金额一律 int64 最小货币单位（分）+ 显式 currency，禁止浮点（§6）。 /  / 刻意**不开**的路由（理由写在这里，不是漏实现）： /   - OpenRecharge / CancelRecharge：充值单由用户在自己端发起与取消，归 gateway/app； /     后台代开充值单等于替用户决定要充多少； /   - CreatePayment / ClosePayment / RefundPayment：**唯一驱动方是 trade-order 的订单状态机** /     （§5：商业订单状态机与履约指令归 trade-order）。从后台直连这三条资金写口会造出第二个 /     写主——同一笔钱既能被订单推进、又能被按钮推进，两边幂等键不共享，对不上时无人能判定真值。 /     退款请走 POST /admin/order/refund/approve（由 trade-order 代为调 payment）。 /   - GetPayment：单条支付单读取没有独立后台场景（列表已按 mid/订单号过滤），不开， /     避免与 /payment/list 形成两套读取口径。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `BalanceMinor` | `balance_minor` | json | `int64` | 是 | — | — |
| `FrozenMinor` | `frozen_minor` | json | `int64` | 是 | — | 恒为 0：本项目无预授权流程 |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `PaymentRechargeItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RechargeNo` | `recharge_no` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | PayChannel：1 SANDBOX（唯一可用） |
| `State` | `state` | json | `int32` | 是 | — | RechargeState：1 PENDING 2 SUCCESS 3 CANCELLED 4 FAILED |
| `Operator` | `operator` | json | `string` | 是 | — | 沙箱结算由谁触发：user / 运营工号 / "cron" |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `SettledAt` | `settled_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `PaymentPaymentItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PaymentNo` | `payment_no` | json | `string` | 是 | — | — |
| `BizOrderNo` | `biz_order_no` | json | `string` | 是 | — | 订单号引用（一单一支付，唯一索引） |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | — |
| `RefundedMinor` | `refunded_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Method` | `method` | json | `int32` | 是 | — | PayMethod：1 余额 2 沙箱渠道 |
| `State` | `state` | json | `int32` | 是 | — | PaymentState，5 全额退款 6 部分退款 |
| `Subject` | `subject` | json | `string` | 是 | — | 摘要：不含 PII、不写凭据 |
| `PaidAt` | `paid_at` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | 0 表示不过期 |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `PaymentRefundItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RefundNo` | `refund_no` | json | `string` | 是 | — | — |
| `PaymentNo` | `payment_no` | json | `string` | 是 | — | — |
| `BizOrderNo` | `biz_order_no` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | RefundState：1 成功（沙箱同步） 2 失败 |
| `Destination` | `destination` | json | `string` | 是 | — | BALANCE；原路退回渠道在本项目不可用 |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `PaymentFlowItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FlowId` | `flow_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `BizType` | `biz_type` | json | `int32` | 是 | — | FlowBizType：1 充值 2 消费 3 退款 4 运营调整 |
| `BizNo` | `biz_no` | json | `string` | 是 | — | — |
| `DeltaMinor` | `delta_minor` | json | `int64` | 是 | — | 正入负出 |
| `BalanceAfterMinor` | `balance_after_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `PaymentChannelItem`

> PaymentChannelItem 投影 payment.v1.ChannelState。real_money **恒为 false**： / 留着这一列就是为了让前端能显式标注「非真实资金」，而不是靠人去读文档。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Channel` | `channel` | json | `int32` | 是 | — | PayChannel：1 SANDBOX |
| `Enabled` | `enabled` | json | `bool` | 是 | — | — |
| `RealMoney` | `real_money` | json | `bool` | 是 | — | 恒 false |
| `Note` | `note` | json | `string` | 是 | — | — |


<!-- file: docs/api/http/admin/26-admin-payment.md -->
