# 终端面 · `/order`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto） | 免鉴权 | 6 |

合计 **6** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto）（免鉴权，6 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/order/create` | 下单（金额由服务侧重算；沙箱渠道下建单即受理并履约） | `orderCreate` | `ordercreatelogic.go` |
| GET | `/order/detail` | 订单详情（强制按 mid 校验归属） | `orderDetail` | `orderdetaillogic.go` |
| GET | `/order/list` | 我的订单列表 | `orderList` | `orderlistlogic.go` |
| POST | `/order/cancel` | 取消未支付订单 | `orderCancel` | `ordercancellogic.go` |
| POST | `/order/refund/request` | 申请退款（进入待审批，不代表已退） | `orderRefund` | `orderrefundlogic.go` |
| GET | `/order/events` | 订单状态流转记录（让用户看到卡在哪一步） | `orderEvents` | `ordereventslogic.go` |

### POST `/order/create` — 下单（金额由服务侧重算；沙箱渠道下建单即受理并履约）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/ordercreatehandler.go`
- 业务实现：`gateway/app/internal/logic/ordercreatelogic.go`

请求：`ParamOrderCreate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `BizType` | `biz_type` | form | `int32` | 是 | — | — |
| `PlanId` | `plan_id` | form | `int64` | 否 | — | — |
| `PlanCode` | `plan_code` | form | `string` | 否 | — | — |
| `Quantity` | `quantity` | form | `int32` | 否 | — | — |
| `PayMethod` | `pay_method` | form | `int32` | 是 | — | 1 余额、2 沙箱渠道 |
| `AmountMinor` | `amount_minor` | form | `int64` | 否 | — | 客户端上报值，仅用于服务端一致性校验 |
| `Platform` | `platform` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | 必填幂等键 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`OrderCreateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderCreateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/order/detail` — 订单详情（强制按 mid 校验归属）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/orderdetailhandler.go`
- 业务实现：`gateway/app/internal/logic/orderdetaillogic.go`

请求：`ParamOrderDetail`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | 必填：网关强制按归属读取，服务侧会二次校验 |
| `OrderNo` | `order_no` | form | `string` | 是 | — | — |

响应：`OrderDetailResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/order/list` — 我的订单列表

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/orderlisthandler.go`
- 业务实现：`gateway/app/internal/logic/orderlistlogic.go`

请求：`ParamOrderList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `BizType` | `biz_type` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`OrderListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/order/cancel` — 取消未支付订单

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/ordercancelhandler.go`
- 业务实现：`gateway/app/internal/logic/ordercancellogic.go`

请求：`ParamOrderCancel`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OrderNo` | `order_no` | form | `string` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |

响应：`OrderCancelResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderCancelData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/order/refund/request` — 申请退款（进入待审批，不代表已退）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/orderrefundhandler.go`
- 业务实现：`gateway/app/internal/logic/orderrefundlogic.go`

请求：`ParamOrderRefundRequest`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OrderNo` | `order_no` | form | `string` | 是 | — | — |
| `AmountMinor` | `amount_minor` | form | `int64` | 否 | — | 0 = 全额 |
| `Reason` | `reason` | form | `string` | 是 | — | 必填 |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

响应：`OrderRefundResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderRefundData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/order/events` — 订单状态流转记录（让用户看到卡在哪一步）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/ordereventshandler.go`
- 业务实现：`gateway/app/internal/logic/ordereventslogic.go`

请求：`ParamOrderEvents`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OrderNo` | `order_no` | form | `string` | 是 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`OrderEventsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderEventsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamOrderCreate`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `BizType` | `biz_type` | form | `int32` | 是 | — | — |
| `PlanId` | `plan_id` | form | `int64` | 否 | — | — |
| `PlanCode` | `plan_code` | form | `string` | 否 | — | — |
| `Quantity` | `quantity` | form | `int32` | 否 | — | — |
| `PayMethod` | `pay_method` | form | `int32` | 是 | — | 1 余额、2 沙箱渠道 |
| `AmountMinor` | `amount_minor` | form | `int64` | 否 | — | 客户端上报值，仅用于服务端一致性校验 |
| `Platform` | `platform` | form | `int32` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | 必填幂等键 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `OrderCreateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderCreateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderDetail`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | 必填：网关强制按归属读取，服务侧会二次校验 |
| `OrderNo` | `order_no` | form | `string` | 是 | — | — |

### `OrderDetailResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `BizType` | `biz_type` | form | `int32` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `OrderListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderCancel`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OrderNo` | `order_no` | form | `string` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |

### `OrderCancelResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderCancelData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderRefundRequest`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OrderNo` | `order_no` | form | `string` | 是 | — | — |
| `AmountMinor` | `amount_minor` | form | `int64` | 否 | — | 0 = 全额 |
| `Reason` | `reason` | form | `string` | 是 | — | 必填 |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |

### `OrderRefundResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderRefundData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderEvents`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OrderNo` | `order_no` | form | `string` | 是 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `OrderEventsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderEventsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `OrderCreateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `Accepted` | `accepted` | json | `bool` | 是 | — | false = 受理未成功（余额不足等），order 仍有单据可查 |
| `RejectReason` | `reject_reason` | json | `string` | 是 | — | 服务侧结论原文，网关不改写 |
| `Order` | `order` | json | `OrderInfo` | 是 | — | — |

### `OrderInfo`

> 金额口径：amount_minor 由 trade-order 服务端按套餐价重算，网关把客户端上报值 / **原样**带过去只为让服务做一致性校验（防前端改价）；网关自己不重算、不折扣。 / 状态机与退款审批都在服务侧（AGENTS.md §8），网关不做任何状态推进的旁路。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `BizType` | `biz_type` | json | `int32` | 是 | — | 1 会员、2 硬币包 |
| `PlanId` | `plan_id` | json | `int64` | 是 | — | — |
| `PlanCode` | `plan_code` | json | `string` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Quantity` | `quantity` | json | `int32` | 是 | — | — |
| `DurationDays` | `duration_days` | json | `int32` | 是 | — | — |
| `CoinAmount` | `coin_amount` | json | `int32` | 是 | — | — |
| `UnitPriceMinor` | `unit_price_minor` | json | `int64` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | — |
| `RefundedMinor` | `refunded_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `PayMethod` | `pay_method` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 见 tradeorder.OrderState |
| `FulfillState` | `fulfill_state` | json | `int32` | 是 | — | — |
| `FulfillDetail` | `fulfill_detail` | json | `string` | 是 | — | — |
| `PaymentNo` | `payment_no` | json | `string` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 是 | — | — |
| `CreatedAt` | `created_at` | json | `int64` | 是 | — | — |
| `PaidAt` | `paid_at` | json | `int64` | 是 | — | — |
| `FulfilledAt` | `fulfilled_at` | json | `int64` | 是 | — | — |
| `ClosedAt` | `closed_at` | json | `int64` | 是 | — | — |

### `OrderListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Orders` | `orders` | json | `[]OrderInfo` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `PageSize` | `page_size` | json | `int64` | 是 | — | — |

### `OrderCancelData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `Order` | `order` | json | `OrderInfo` | 是 | — | — |

### `OrderRefundData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `Order` | `order` | json | `OrderInfo` | 是 | — | — |

### `OrderEventsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Events` | `events` | json | `[]OrderEvent` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `PageSize` | `page_size` | json | `int64` | 是 | — | — |

### `OrderEvent`

> OrderEvent 订单状态流转（用户看得到「卡在哪一步」，但不暴露内部 operator 取值）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventId` | `event_id` | json | `int64` | 是 | — | — |
| `FromState` | `from_state` | json | `int32` | 是 | — | — |
| `ToState` | `to_state` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/27-order.md -->
