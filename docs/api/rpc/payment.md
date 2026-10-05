# RPC · `payment`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/payment/rpc/payment.proto` |
| protobuf 包 | `payment.v1` |
| go_package | `go-video/services/payment/rpc` |
| 发现用的 etcd key | `payment.v1.rpc`（`services/payment/etc/payment.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`payment.v1.rpc`） |
| 监听 | `8161`（`services/payment/etc/payment.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_payment` |
| 方法数 | 14（service `Payment`） |
| 网关消费方 | `app:PaymentRPC`、`admin:PaymentRPC` |

## 契约说明

> 资金域契约（AGENTS.md §1 商业化范围 2026-09-22 修订后纳入）。
>
> 本服务是「钱」的唯一写入口（§5）：余额、充值单、支付单、退款单、资金流水。
> 其他服务只保存本服务返回的单据号，不得自记余额。
>
> 沙箱语义（必须读清楚，否则会误判成真实收单）：
>   - 唯一可用渠道是 SANDBOX。OpenRecharge/SettleSandboxRecharge/CreatePayment
>     只在本地台账上改数，不请求任何第三方支付网关，不产生真实资金移动；
>   - 因此订单「支付成功 → 会员生效」是真实状态推进，而「真实收单」在本项目不存在；
>   - 需要真实渠道才成立的能力一律不开接口：渠道异步回调验签、退款到银行卡、
>     提现、打款出金、对账文件下载、发票税务。调用已声明但渠道未配置的方法时，
>     返回 FailedPrecondition + "payment channel not configured"，绝不返回成功。
>   - 硬币（虚拟社区货币，归 coin 服务）与本服务的现金余额是分开的两套账，不互换。

## service `Payment`

> Payment 资金服务：余额、充值、支付、退款与流水。

gRPC 方法前缀：`payment.v1.Payment/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `GetWallet` | [`GetWalletReq`](#message-getwalletreq) | [`GetWalletReply`](#message-getwalletreply) | 查询余额 |
| 2 | `AdjustBalance` | [`AdjustBalanceReq`](#message-adjustbalancereq) | [`AdjustBalanceReply`](#message-adjustbalancereply) | 运营调整余额（有台账、有理由） |
| 3 | `OpenRecharge` | [`OpenRechargeReq`](#message-openrechargereq) | [`OpenRechargeReply`](#message-openrechargereply) | 开充值单（仅 SANDBOX） |
| 4 | `SettleSandboxRecharge` | [`SettleSandboxRechargeReq`](#message-settlesandboxrechargereq) | [`SettleSandboxRechargeReply`](#message-settlesandboxrechargereply) | 沙箱结算充值单并入账 |
| 5 | `CancelRecharge` | [`CancelRechargeReq`](#message-cancelrechargereq) | [`CancelRechargeReply`](#message-cancelrechargereply) | 取消未结算充值单 |
| 6 | `ListRecharges` | [`ListRechargesReq`](#message-listrechargesreq) | [`ListRechargesReply`](#message-listrechargesreply) | 充值台账分页 |
| 7 | `CreatePayment` | [`CreatePaymentReq`](#message-createpaymentreq) | [`CreatePaymentReply`](#message-createpaymentreply) | 受理支付（余额扣减或沙箱渠道立即成功） |
| 8 | `GetPayment` | [`GetPaymentReq`](#message-getpaymentreq) | [`GetPaymentReply`](#message-getpaymentreply) | 支付单读取（按 payment_no 或订单号） |
| 9 | `ClosePayment` | [`ClosePaymentReq`](#message-closepaymentreq) | [`ClosePaymentReply`](#message-closepaymentreply) | 关闭未支付/未履约支付单 |
| 10 | `ListPayments` | [`ListPaymentsReq`](#message-listpaymentsreq) | [`ListPaymentsReply`](#message-listpaymentsreply) | 支付台账分页 |
| 11 | `RefundPayment` | [`RefundPaymentReq`](#message-refundpaymentreq) | [`RefundPaymentReply`](#message-refundpaymentreply) | 退款（只退回余额；原路退回渠道返回 not-configured） |
| 12 | `ListRefunds` | [`ListRefundsReq`](#message-listrefundsreq) | [`ListRefundsReply`](#message-listrefundsreply) | 退款台账分页 |
| 13 | `ListFlows` | [`ListFlowsReq`](#message-listflowsreq) | [`ListFlowsReply`](#message-listflowsreply) | 资金流水分页 |
| 14 | `DescribeChannels` | [`DescribeChannelsReq`](#message-describechannelsreq) | [`DescribeChannelsReply`](#message-describechannelsreply) | 渠道能力自述（沙箱模式与真实渠道是否配置） |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `PayChannel`

> 充值/支付渠道。除 SANDBOX 外都是「已定义但未接入」的占位档， / 出现任何非 SANDBOX 入参都会被拒绝，而不是默默按沙箱处理。

| 值 | 编号 | 说明 |
|---|---|---|
| `PAY_CHANNEL_UNSPECIFIED` | 0 | — |
| `PAY_CHANNEL_SANDBOX` | 1 | 唯一可用：本地台账，无真实资金 |

### enum `PayMethod`

> 支付方式。BALANCE 扣本服务余额；SANDBOX_CHANNEL 表示「沙箱收单，立即成功」。

| 值 | 编号 | 说明 |
|---|---|---|
| `PAY_METHOD_UNSPECIFIED` | 0 | — |
| `PAY_METHOD_BALANCE` | 1 | — |
| `PAY_METHOD_SANDBOX_CHANNEL` | 2 | — |

### enum `RechargeState`

| 值 | 编号 | 说明 |
|---|---|---|
| `RECHARGE_STATE_UNSPECIFIED` | 0 | — |
| `RECHARGE_STATE_PENDING` | 1 | — |
| `RECHARGE_STATE_SUCCESS` | 2 | — |
| `RECHARGE_STATE_CANCELLED` | 3 | — |
| `RECHARGE_STATE_FAILED` | 4 | — |

### enum `PaymentState`

| 值 | 编号 | 说明 |
|---|---|---|
| `PAYMENT_STATE_UNSPECIFIED` | 0 | — |
| `PAYMENT_STATE_PENDING` | 1 | — |
| `PAYMENT_STATE_PAID` | 2 | — |
| `PAYMENT_STATE_FAILED` | 3 | — |
| `PAYMENT_STATE_CLOSED` | 4 | — |
| `PAYMENT_STATE_REFUNDED` | 5 | 全额退款 |
| `PAYMENT_STATE_PARTIALLY_REFUNDED` | 6 | 部分退款（refunded_minor 记录已退额） |

### enum `RefundState`

| 值 | 编号 | 说明 |
|---|---|---|
| `REFUND_STATE_UNSPECIFIED` | 0 | — |
| `REFUND_STATE_SUCCEEDED` | 1 | 沙箱余额退回是同步完成的 |
| `REFUND_STATE_FAILED` | 2 | — |

### enum `FlowBizType`

> 流水业务类型

| 值 | 编号 | 说明 |
|---|---|---|
| `FLOW_BIZ_TYPE_UNSPECIFIED` | 0 | — |
| `FLOW_BIZ_TYPE_RECHARGE` | 1 | 充值入账 |
| `FLOW_BIZ_TYPE_PAYMENT` | 2 | 消费出账 |
| `FLOW_BIZ_TYPE_REFUND` | 3 | 退款入账 |
| `FLOW_BIZ_TYPE_ADMIN_ADJUST` | 4 | 运营调整（正负都可能，必须有 reason） |

### message `WalletInfo`

> 余额账户。金额一律 int64 最小货币单位（分）+ 显式币种，不用浮点。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `balance_minor` | `int64` | 2 | — | — |
| `frozen_minor` | `int64` | 3 | — | 预留位；本项目无预授权流程，恒为 0，读侧不要拿它当可用余额 |
| `currency` | `string` | 4 | — | — |
| `version` | `int64` | 5 | — | — |
| `ctime` | `int64` | 6 | — | — |
| `mtime` | `int64` | 7 | — | — |

### message `RechargeInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `recharge_no` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `amount_minor` | `int64` | 3 | — | — |
| `currency` | `string` | 4 | — | — |
| `channel` | [`PayChannel`](#enum-paychannel) | 5 | — | — |
| `state` | [`RechargeState`](#enum-rechargestate) | 6 | — | — |
| `operator` | `string` | 7 | — | 沙箱结算由谁触发：user / 运营工号 / "cron" |
| `request_id` | `string` | 8 | — | — |
| `reason` | `string` | 9 | — | — |
| `settled_at` | `int64` | 10 | — | — |
| `ctime` | `int64` | 11 | — | — |
| `mtime` | `int64` | 12 | — | — |

### message `PaymentInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `payment_no` | `string` | 1 | — | — |
| `biz_order_no` | `string` | 2 | — | 订单号（trade-order 的主键引用，唯一索引：一单一支付） |
| `mid` | `int64` | 3 | — | — |
| `amount_minor` | `int64` | 4 | — | — |
| `refunded_minor` | `int64` | 5 | — | — |
| `currency` | `string` | 6 | — | — |
| `method` | [`PayMethod`](#enum-paymethod) | 7 | — | — |
| `state` | [`PaymentState`](#enum-paymentstate) | 8 | — | — |
| `subject` | `string` | 9 | — | 摘要：不含 PII、不写凭据 |
| `paid_at` | `int64` | 10 | — | — |
| `expire_at` | `int64` | 11 | — | 受理超时；0 表示不过期 |
| `request_id` | `string` | 12 | — | — |
| `ctime` | `int64` | 13 | — | — |
| `mtime` | `int64` | 14 | — | — |
| `operator` | `string` | 15 | — | operator/remark 是「谁把这单推到当前态、理由是什么」的审计回显位（无 PII、无凭据）。 / 运营面必须能看到经办人，否则关单/人工结算就成了无从核对的黑盒。 |
| `remark` | `string` | 16 | — | — |

### message `RefundInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `refund_no` | `string` | 1 | — | — |
| `payment_no` | `string` | 2 | — | — |
| `biz_order_no` | `string` | 3 | — | — |
| `mid` | `int64` | 4 | — | — |
| `amount_minor` | `int64` | 5 | — | — |
| `currency` | `string` | 6 | — | — |
| `state` | [`RefundState`](#enum-refundstate) | 7 | — | — |
| `destination` | `string` | 8 | — | BALANCE（退余额）；原路退回渠道在本项目不可用 |
| `operator` | `string` | 9 | — | — |
| `request_id` | `string` | 10 | — | — |
| `reason` | `string` | 11 | — | — |
| `ctime` | `int64` | 12 | — | — |

### message `FlowInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `flow_id` | `int64` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `biz_type` | [`FlowBizType`](#enum-flowbiztype) | 3 | — | — |
| `biz_no` | `string` | 4 | — | recharge_no / payment_no / refund_no / adjust 单号 |
| `delta_minor` | `int64` | 5 | — | 正入负出 |
| `balance_after_minor` | `int64` | 6 | — | — |
| `currency` | `string` | 7 | — | — |
| `remark` | `string` | 8 | — | — |
| `operator` | `string` | 9 | — | — |
| `request_id` | `string` | 10 | — | — |
| `ctime` | `int64` | 11 | — | — |

### message `GetWalletReq`

> --- 钱包 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `currency` | `string` | 2 | — | 空表示默认币种 CNY |

### message `GetWalletReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `wallet` | [`WalletInfo`](#message-walletinfo) | 1 | — | — |

### message `AdjustBalanceReq`

> 运营/沙箱批量补余额用的调整流水（正负皆可）。与充值分开：充值要有充值单可查， / 调整只有台账，二者的审计口径不同，不能混成一条路径。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `delta_minor` | `int64` | 2 | — | 不允许为 0 |
| `currency` | `string` | 3 | — | — |
| `operator` | `string` | 4 | — | 网关按会话渲染 |
| `request_id` | `string` | 5 | — | — |
| `reason` | `string` | 6 | — | 必填 |

### message `AdjustBalanceReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `wallet` | [`WalletInfo`](#message-walletinfo) | 2 | — | — |
| `flow_id` | `int64` | 3 | — | — |

### message `OpenRechargeReq`

> --- 充值（沙箱）---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `amount_minor` | `int64` | 2 | — | — |
| `currency` | `string` | 3 | — | — |
| `channel` | [`PayChannel`](#enum-paychannel) | 4 | — | 只有 SANDBOX 受理 |
| `request_id` | `string` | 5 | — | — |
| `client_trace_id` | `string` | 6 | — | — |

### message `OpenRechargeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | 命中 request_id 重放，未重复建单 |
| `recharge` | [`RechargeInfo`](#message-rechargeinfo) | 2 | — | — |

### message `SettleSandboxRechargeReq`

> 沙箱结算：直接把充值单置为 SUCCESS 并入账。真实渠道下这一步应由渠道回调驱动， / 本项目没有回调，因此这一步就是「钱到账」的唯一入口——只动本地台账。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `recharge_no` | `string` | 1 | — | — |
| `operator` | `string` | 2 | — | 发起人；自动结算为 "cron" |
| `request_id` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | — |

### message `SettleSandboxRechargeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `recharge` | [`RechargeInfo`](#message-rechargeinfo) | 2 | — | — |
| `wallet` | [`WalletInfo`](#message-walletinfo) | 3 | — | — |
| `flow_id` | `int64` | 4 | — | — |

### message `CancelRechargeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `recharge_no` | `string` | 1 | — | — |
| `operator` | `string` | 2 | — | — |
| `request_id` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | 必填 |

### message `CancelRechargeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `recharge` | [`RechargeInfo`](#message-rechargeinfo) | 2 | — | — |

### message `ListRechargesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 0 表示跨用户（运营面） |
| `state` | [`RechargeState`](#enum-rechargestate) | 2 | — | — |
| `from_ts` | `int64` | 3 | — | — |
| `to_ts` | `int64` | 4 | — | — |
| `page` | `int64` | 5 | — | — |
| `size` | `int64` | 6 | — | — |

### message `ListRechargesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `recharges` | [`RechargeInfo`](#message-rechargeinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `CreatePaymentReq`

> --- 支付单（供 trade-order 使用）--- / 建单即受理：BALANCE 走余额扣减（不足则 FailedPrecondition，且不写流水）； / SANDBOX_CHANNEL 直接 PAID。biz_order_no 唯一索引保证一单一支付， / 同单号不同金额必须冲突报错，不能静默改价。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `biz_order_no` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `amount_minor` | `int64` | 3 | — | — |
| `currency` | `string` | 4 | — | — |
| `method` | [`PayMethod`](#enum-paymethod) | 5 | — | — |
| `subject` | `string` | 6 | — | — |
| `expire_at` | `int64` | 7 | — | — |
| `request_id` | `string` | 8 | — | — |
| `operator` | `string` | 9 | — | 用户自助为 "user" |

### message `CreatePaymentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `payment` | [`PaymentInfo`](#message-paymentinfo) | 2 | — | — |
| `wallet` | [`WalletInfo`](#message-walletinfo) | 3 | — | 余额支付时回余额快照；渠道支付为受理前快照 |

### message `GetPaymentReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `payment_no` | `string` | 1 | — | 与 biz_order_no 二选一 |
| `biz_order_no` | `string` | 2 | — | — |

### message `GetPaymentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `found` | `bool` | 1 | — | — |
| `payment` | [`PaymentInfo`](#message-paymentinfo) | 2 | — | — |

### message `ClosePaymentReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `payment_no` | `string` | 1 | — | — |
| `operator` | `string` | 2 | — | — |
| `request_id` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | 必填 |

### message `ClosePaymentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `payment` | [`PaymentInfo`](#message-paymentinfo) | 2 | — | — |

### message `ListPaymentsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `state` | [`PaymentState`](#enum-paymentstate) | 2 | — | — |
| `method` | [`PayMethod`](#enum-paymethod) | 3 | — | — |
| `from_ts` | `int64` | 4 | — | — |
| `to_ts` | `int64` | 5 | — | — |
| `page` | `int64` | 6 | — | — |
| `size` | `int64` | 7 | — | — |

### message `ListPaymentsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `payments` | [`PaymentInfo`](#message-paymentinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `RefundPaymentReq`

> --- 退款 --- / 只支持退回到余额（destination=BALANCE）：沙箱里钱本就是台账加的，退回到台账是自洽的。 / 原路退回真实渠道需要渠道凭证，本服务不开该路径，被请求时返回 not-configured。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `payment_no` | `string` | 1 | — | — |
| `amount_minor` | `int64` | 2 | — | 0 表示全额剩余可退 |
| `to_balance` | `bool` | 3 | — | false 即要求原路退回渠道 → 拒绝 |
| `operator` | `string` | 4 | — | — |
| `request_id` | `string` | 5 | — | — |
| `reason` | `string` | 6 | — | 必填 |

### message `RefundPaymentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `refund` | [`RefundInfo`](#message-refundinfo) | 2 | — | — |
| `payment` | [`PaymentInfo`](#message-paymentinfo) | 3 | — | — |
| `wallet` | [`WalletInfo`](#message-walletinfo) | 4 | — | — |

### message `ListRefundsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `payment_no` | `string` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |
| `from_ts` | `int64` | 5 | — | 跨用户审计（mid=0）必须给时间窗，与 ListPayments/ListFlows 同一收口口径； / 缺这两个字段时服务只能靠 payment_no 兜，无法按时间段核对退款台账。 |
| `to_ts` | `int64` | 6 | — | — |

### message `ListRefundsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `refunds` | [`RefundInfo`](#message-refundinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `ListFlowsReq`

> --- 流水与渠道自述 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 0 表示跨用户（运营面） |
| `biz_type` | [`FlowBizType`](#enum-flowbiztype) | 2 | — | — |
| `biz_no` | `string` | 3 | — | — |
| `from_ts` | `int64` | 4 | — | — |
| `to_ts` | `int64` | 5 | — | — |
| `page` | `int64` | 6 | — | — |
| `size` | `int64` | 7 | — | — |

### message `ListFlowsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `flows` | [`FlowInfo`](#message-flowinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `DescribeChannelsReq`

> 渠道自述：把「沙箱、没有真实资金」这件事说成可查询的事实， / 而不是只写在 README 里——运营页和排障都要能直接读到。

（空消息）

### message `ChannelState`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `channel` | [`PayChannel`](#enum-paychannel) | 1 | — | — |
| `enabled` | `bool` | 2 | — | — |
| `real_money` | `bool` | 3 | — | 恒为 false；留这一列是为了让前端能显式标注「非真实资金」 |
| `note` | `string` | 4 | — | — |

### message `DescribeChannelsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `sandbox_only` | `bool` | 1 | — | — |
| `channels` | [`ChannelState`](#message-channelstate) | 2 | repeated | — |
| `currency_default` | `string` | 3 | — | — |
