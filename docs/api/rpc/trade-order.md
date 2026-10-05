# RPC · `trade-order`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/trade-order/rpc/tradeorder.proto` |
| protobuf 包 | `tradeorder.v1` |
| go_package | `go-video/services/trade-order/rpc` |
| 发现用的 etcd key | `tradeorder.v1.rpc`（`services/trade-order/etc/tradeorder.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`tradeorder.v1.rpc`） |
| 监听 | `8162`（`services/trade-order/etc/tradeorder.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_trade_order` |
| 方法数 | 12（service `TradeOrder`） |
| 网关消费方 | `app:TradeOrderRPC`、`admin:TradeOrderRPC` |

## 契约说明

> 商业订单域契约。
>
> 数据所有权（§5）：订单事实与履约指令只属于本服务。payment 只写资金台账，
> membership 只写会员授予，两者都不得回写订单状态；本服务通过 RPC 驱动它们。
>
> 状态机（只允许下列迁移，其他一律拒绝）：
>   CREATED → PAYING → PAID → FULFILLING → FULFILLED
>   CREATED/PAYING → CANCELLED
>   PAID/FULFILLING → FAILED              （受理成功但无法履约，等人工或 cron 重试）
>   FULFILLED → REFUND_REQUESTED → REFUND_APPROVED → REFUNDED
>   REFUND_REQUESTED → FULFILLED          （驳回退款，回到原状态）
>
> 沙箱语义：本服务不碰钱。金额一律以服务端重算为准（下单时向 membership 取套餐价，
> 客户端上报的 amount_minor 只做一致性校验，不作为扣款依据）；pay_method 只有
> BALANCE 与 SANDBOX_CHANNEL 两档，都不产生真实资金移动。
> 需要真实渠道才成立的能力（渠道回调、原路退回、发票、对账单下载）不开接口。

## service `TradeOrder`

> TradeOrder 商业订单服务。

gRPC 方法前缀：`tradeorder.v1.TradeOrder/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `CreateOrder` | [`CreateOrderReq`](#message-createorderreq) | [`CreateOrderReply`](#message-createorderreply) | 下单（服务端重算金额，沙箱下内联受理） |
| 2 | `GetOrder` | [`GetOrderReq`](#message-getorderreq) | [`GetOrderReply`](#message-getorderreply) | 订单详情（带归属校验） |
| 3 | `ListMyOrders` | [`ListMyOrdersReq`](#message-listmyordersreq) | [`ListMyOrdersReply`](#message-listmyordersreply) | 我的订单 |
| 4 | `ListOrders` | [`ListOrdersReq`](#message-listordersreq) | [`ListOrdersReply`](#message-listordersreply) | 运营面订单查询（有界窗口） |
| 5 | `ListOrderEvents` | [`ListOrderEventsReq`](#message-listordereventsreq) | [`ListOrderEventsReply`](#message-listordereventsreply) | 订单状态流转台账 |
| 6 | `CancelOrder` | [`CancelOrderReq`](#message-cancelorderreq) | [`CancelOrderReply`](#message-cancelorderreply) | 取消未支付订单 |
| 7 | `BindPayment` | [`BindPaymentReq`](#message-bindpaymentreq) | [`BindPaymentReply`](#message-bindpaymentreply) | 绑定支付结论（补偿推进） |
| 8 | `FulfillOrder` | [`FulfillOrderReq`](#message-fulfillorderreq) | [`FulfillOrderReply`](#message-fulfillorderreply) | 执行/重试履约 |
| 9 | `ListStuckOrders` | [`ListStuckOrdersReq`](#message-liststuckordersreq) | [`ListStuckOrdersReply`](#message-liststuckordersreply) | cron：卡单扫描 |
| 10 | `RequestRefund` | [`RequestRefundReq`](#message-requestrefundreq) | [`RequestRefundReply`](#message-requestrefundreply) | 申请退款 |
| 11 | `ApproveRefund` | [`ApproveRefundReq`](#message-approverefundreq) | [`ApproveRefundReply`](#message-approverefundreply) | 审批通过并回收权益 |
| 12 | `RejectRefund` | [`RejectRefundReq`](#message-rejectrefundreq) | [`RejectRefundReply`](#message-rejectrefundreply) | 驳回退款 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `OrderBizType`

| 值 | 编号 | 说明 |
|---|---|---|
| `ORDER_BIZ_TYPE_UNSPECIFIED` | 0 | — |
| `ORDER_BIZ_TYPE_MEMBERSHIP` | 1 | 买会员：履约调 membership.GrantMembership |
| `ORDER_BIZ_TYPE_COIN_PACK` | 2 | 买硬币包：履约调 coin.GrantCoin（发的是社区硬币，不是钱） |

### enum `OrderState`

| 值 | 编号 | 说明 |
|---|---|---|
| `ORDER_STATE_UNSPECIFIED` | 0 | — |
| `ORDER_STATE_CREATED` | 1 | — |
| `ORDER_STATE_PAYING` | 2 | — |
| `ORDER_STATE_PAID` | 3 | — |
| `ORDER_STATE_FULFILLING` | 4 | — |
| `ORDER_STATE_FULFILLED` | 5 | — |
| `ORDER_STATE_CANCELLED` | 6 | — |
| `ORDER_STATE_FAILED` | 7 | — |
| `ORDER_STATE_REFUND_REQUESTED` | 8 | — |
| `ORDER_STATE_REFUND_APPROVED` | 9 | — |
| `ORDER_STATE_REFUNDED` | 10 | — |
| `ORDER_STATE_REFUND_REJECTED` | 11 | — |

### enum `PayMethod`

| 值 | 编号 | 说明 |
|---|---|---|
| `PAY_METHOD_UNSPECIFIED` | 0 | — |
| `PAY_METHOD_BALANCE` | 1 | 余额支付（payment 扣台账） |
| `PAY_METHOD_SANDBOX_CHANNEL` | 2 | 沙箱渠道（payment 直接置 PAID） |

### enum `FulfillState`

> 履约结果。区别于订单状态：履约是「该给的东西给到没有」。

| 值 | 编号 | 说明 |
|---|---|---|
| `FULFILL_STATE_UNSPECIFIED` | 0 | — |
| `FULFILL_STATE_PENDING` | 1 | — |
| `FULFILL_STATE_SUCCEEDED` | 2 | — |
| `FULFILL_STATE_FAILED` | 3 | — |

### enum `Platform`

| 值 | 编号 | 说明 |
|---|---|---|
| `PLATFORM_UNSPECIFIED` | 0 | — |
| `PLATFORM_ANDROID` | 1 | — |
| `PLATFORM_IOS` | 2 | — |
| `PLATFORM_HARMONY` | 3 | — |
| `PLATFORM_DESKTOP` | 4 | — |
| `PLATFORM_WEB` | 5 | — |

### message `OrderInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `order_no` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `biz_type` | [`OrderBizType`](#enum-orderbiztype) | 3 | — | — |
| `plan_id` | `int64` | 4 | — | 会员套餐或硬币包 SKU |
| `plan_code` | `string` | 5 | — | — |
| `title` | `string` | 6 | — | 下单时快照的商品名（改价改名不影响历史单） |
| `quantity` | `int32` | 7 | — | 份数 |
| `duration_days` | `int32` | 8 | — | 会员单：本次总时长（单价时长 × 份数） |
| `coin_amount` | `int32` | 9 | — | 硬币包：本次发放硬币数 |
| `unit_price_minor` | `int64` | 10 | — | 服务端重算的单价快照 |
| `amount_minor` | `int64` | 11 | — | 应付=实付总额 |
| `refunded_minor` | `int64` | 12 | — | — |
| `currency` | `string` | 13 | — | — |
| `pay_method` | [`PayMethod`](#enum-paymethod) | 14 | — | — |
| `state` | [`OrderState`](#enum-orderstate) | 15 | — | — |
| `fulfill_state` | [`FulfillState`](#enum-fulfillstate) | 16 | — | — |
| `fulfill_attempts` | `int32` | 17 | — | — |
| `fulfill_detail` | `string` | 18 | — | 最近一次失败摘要（不写堆栈、不写 PII） |
| `payment_no` | `string` | 19 | — | — |
| `grant_ref` | `string` | 20 | — | 履约产物引用：会员 grant_id 或硬币 flow_id |
| `expire_at` | `int64` | 21 | — | 未支付关单时间 |
| `client_trace_id` | `string` | 22 | — | — |
| `platform` | [`Platform`](#enum-platform) | 23 | — | — |
| `request_id` | `string` | 24 | — | 建单幂等键 |
| `version` | `int64` | 25 | — | CAS |
| `created_at` | `int64` | 26 | — | — |
| `updated_at` | `int64` | 27 | — | — |
| `paid_at` | `int64` | 28 | — | — |
| `fulfilled_at` | `int64` | 29 | — | — |
| `closed_at` | `int64` | 30 | — | — |

### message `OrderEventInfo`

> 状态流转台账：每次迁移一行，含操作者与理由。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `event_id` | `int64` | 1 | — | — |
| `order_no` | `string` | 2 | — | — |
| `from_state` | [`OrderState`](#enum-orderstate) | 3 | — | — |
| `to_state` | [`OrderState`](#enum-orderstate) | 4 | — | — |
| `operator` | `string` | 5 | — | "user" / 运营工号 / "cron" / "system" |
| `reason` | `string` | 6 | — | — |
| `ctime` | `int64` | 7 | — | — |

### message `CreateOrderReq`

> --- 建单与读取 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `biz_type` | [`OrderBizType`](#enum-orderbiztype) | 2 | — | — |
| `plan_id` | `int64` | 3 | — | — |
| `plan_code` | `string` | 4 | — | 与 plan_id 二选一，服务端以套餐表为准 |
| `quantity` | `int32` | 5 | — | <=0 视为 1；上限由服务侧配置裁剪 |
| `pay_method` | [`PayMethod`](#enum-paymethod) | 6 | — | — |
| `amount_minor` | `int64` | 7 | — | 客户端上报值：与服务端重算不一致即拒绝（防前端改价） |
| `request_id` | `string` | 8 | — | 必填幂等键 |
| `platform` | [`Platform`](#enum-platform) | 9 | — | — |
| `client_trace_id` | `string` | 10 | — | — |
| `title` | `string` | 11 | — | 可空；服务端优先用套餐名快照 |

### message `CreateOrderReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | 命中 request_id 重放：返回首次订单，未重复建单 |
| `order` | [`OrderInfo`](#message-orderinfo) | 2 | — | — |
| `accepted` | `bool` | 3 | — | 沙箱下建单即受理，受理结果在这里给全，避免客户端再查一次。 |
| `reject_reason` | `string` | 4 | — | accepted=false 时的可读结论（余额不足等），不是错误码 |

### message `GetOrderReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `order_no` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | 非 0 时校验归属，越权返回 not-found 语义而不是 FORBIDDEN |

### message `GetOrderReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `found` | `bool` | 1 | — | — |
| `order` | [`OrderInfo`](#message-orderinfo) | 2 | — | — |

### message `ListMyOrdersReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `state` | [`OrderState`](#enum-orderstate) | 2 | — | UNSPECIFIED 不过滤 |
| `biz_type` | [`OrderBizType`](#enum-orderbiztype) | 3 | — | — |
| `page` | `int64` | 4 | — | — |
| `size` | `int64` | 5 | — | — |

### message `ListMyOrdersReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `orders` | [`OrderInfo`](#message-orderinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `ListOrdersReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 运营面：mid=0 且 filters 非空才允许全表扫，无界扫描必须拒绝。 |
| `state` | [`OrderState`](#enum-orderstate) | 2 | — | — |
| `biz_type` | [`OrderBizType`](#enum-orderbiztype) | 3 | — | — |
| `pay_method` | [`PayMethod`](#enum-paymethod) | 4 | — | — |
| `order_no` | `string` | 5 | — | — |
| `payment_no` | `string` | 6 | — | — |
| `from_ts` | `int64` | 7 | — | — |
| `to_ts` | `int64` | 8 | — | — |
| `page` | `int64` | 9 | — | — |
| `size` | `int64` | 10 | — | — |
| `max_window_seconds` | `int64` | 11 | — | 0 表示用服务侧默认窗口 |

### message `ListOrdersReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `orders` | [`OrderInfo`](#message-orderinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `ListOrderEventsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `order_no` | `string` | 1 | — | — |
| `page` | `int64` | 2 | — | — |
| `size` | `int64` | 3 | — | — |

### message `ListOrderEventsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `events` | [`OrderEventInfo`](#message-ordereventinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `CancelOrderReq`

> --- 生命周期 --- / 取消（未支付前）。已支付订单要走退款，不能靠取消把钱吞掉。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `order_no` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | 用户自助取消时校验归属 |
| `operator` | `string` | 3 | — | 运营取消时必填；与 mid 二者取其一作为主体 |
| `request_id` | `string` | 4 | — | — |
| `reason` | `string` | 5 | — | 必填 |

### message `CancelOrderReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `order` | [`OrderInfo`](#message-orderinfo) | 2 | — | — |

### message `BindPaymentReq`

> 绑定支付结论并把订单推进到 PAID。幂等：已 PAID 及之后返回 duplicated=true。 / 供 cron/对账补偿使用（沙箱下建单链路已内联推进，这一步只兜「受理成功但订单没推进」的卡单）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `order_no` | `string` | 1 | — | — |
| `payment_no` | `string` | 2 | — | — |
| `amount_minor` | `int64` | 3 | — | 与订单金额校验，不一致拒绝 |
| `operator` | `string` | 4 | — | — |
| `request_id` | `string` | 5 | — | — |

### message `BindPaymentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `order` | [`OrderInfo`](#message-orderinfo) | 2 | — | — |

### message `FulfillOrderReq`

> 执行履约（幂等，可重试）。PAID/FULFILLING 状态才受理； / 会员单调 membership.GrantMembership，硬币包单调 coin.GrantCoin， / 成功后订单置 FULFILLED 并把 grant/flow 引用写进 grant_ref。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `order_no` | `string` | 1 | — | — |
| `operator` | `string` | 2 | — | "system" / "cron" / 运营工号 |
| `request_id` | `string` | 3 | — | — |

### message `FulfillOrderReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `fulfilled` | `bool` | 1 | — | — |
| `duplicated` | `bool` | 2 | — | 已履约，未重复发放 |
| `order` | [`OrderInfo`](#message-orderinfo) | 3 | — | — |
| `detail` | `string` | 4 | — | 失败原因摘要；成功时可为空 |

### message `ListStuckOrdersReq`

> cron：扫描卡在 PAYING/PAID/FULFILLING 且超时的订单。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `older_than_seconds` | `int64` | 1 | — | 更新至今超过该秒数才算卡单 |
| `states` | [`OrderState`](#enum-orderstate) | 2 | repeated | — |
| `limit` | `int64` | 3 | — | — |

### message `ListStuckOrdersReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `orders` | [`OrderInfo`](#message-orderinfo) | 1 | repeated | — |

### message `RequestRefundReq`

> --- 退款 --- / 申请退款（用户或运营代提）。只允许对 FULFILLED/PAID 的单发起。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `order_no` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `operator` | `string` | 3 | — | — |
| `amount_minor` | `int64` | 4 | — | 0 表示全额；部分退款由服务侧按已消耗时长折算上限 |
| `reason` | `string` | 5 | — | 必填 |
| `request_id` | `string` | 6 | — | — |

### message `RequestRefundReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `order` | [`OrderInfo`](#message-orderinfo) | 2 | — | — |

### message `ApproveRefundReq`

> 审批通过：先调 payment 退款（退回余额），再按业务回收权益 / （会员单调 membership.RevokeMembership 扣回未使用时长，硬币包单扣回未消耗硬币）。 / 回收失败不撤销已退的款，但订单停在 REFUND_APPROVED 并把差异写进 fulfill_detail。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `order_no` | `string` | 1 | — | — |
| `operator` | `string` | 2 | — | 必填：审批人只能由网关按会话渲染 |
| `request_id` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | 必填 |
| `expected_version` | `int64` | 5 | — | 订单 CAS：防止审批与用户取消并发 |

### message `ApproveRefundReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `order` | [`OrderInfo`](#message-orderinfo) | 2 | — | — |
| `refund_no` | `string` | 3 | — | — |
| `revoke_detail` | `string` | 4 | — | 权益回收结论（含「已退但未回收」这种部分成功） |

### message `RejectRefundReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `order_no` | `string` | 1 | — | — |
| `operator` | `string` | 2 | — | — |
| `request_id` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | 必填 |

### message `RejectRefundReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `order` | [`OrderInfo`](#message-orderinfo) | 2 | — | — |
