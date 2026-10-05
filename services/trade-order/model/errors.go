package model

import "errors"

// trade-order 域哨兵错误。
//
// 错误语义与 gRPC code 的对应关系（本仓约定：logic 直接返回错误，由 gateway 侧统一映射，
// 参见 services/creator/internal/logic/errors.go 的同款口径）：
//   - Err*Required / ErrInvalid* / ErrAmountMismatch / ErrListWindow* → InvalidArgument
//   - Err*NotFound → not-found 语义（GetOrder 返回 found=false，不暴露订单是否存在）
//   - ErrInvalidStateTransition / Err*NotConfigured / ErrFulfillRetry* → FailedPrecondition
//   - ErrConcurrentUpdate → Aborted（调用方可安全重试）
//
// 这里没有任何一条错误会被映射成「成功」：本服务最危险的失效模式是
// 建出一张看起来已支付、或伪造一次发放（AGENTS.md §9 禁止假成功）。
var (
	// --- 入参校验 ---

	// ErrInvalidMid mid 非正数（订单不支持游客）。
	ErrInvalidMid = errors.New("trade-order: invalid mid")
	// ErrRequestIdRequired 缺少幂等键 request_id。
	ErrRequestIdRequired = errors.New("trade-order: request_id required")
	// ErrOrderNoRequired 缺少订单号。
	ErrOrderNoRequired = errors.New("trade-order: order_no required")
	// ErrInvalidBizType biz_type 不在 MEMBERSHIP/COIN_PACK 之内。
	ErrInvalidBizType = errors.New("trade-order: invalid biz_type")
	// ErrInvalidPayMethod pay_method 只接受 BALANCE 与 SANDBOX_CHANNEL。
	ErrInvalidPayMethod = errors.New("trade-order: invalid pay_method, only BALANCE/SANDBOX_CHANNEL")
	// ErrInvalidPlatform platform 未指明或不属已知端。
	ErrInvalidPlatform = errors.New("trade-order: invalid platform")
	// ErrReasonRequired 有后果的动作（取消/退款/驳回/关单）必须写理由，进台账。
	ErrReasonRequired = errors.New("trade-order: reason required")
	// ErrOperatorRequired 运营主体身份缺失。
	ErrOperatorRequired = errors.New("trade-order: operator required")
	// ErrSubjectRequired CancelOrder 的主体：要么 mid（本人）要么 operator（运营代操作）。
	ErrSubjectRequired = errors.New("trade-order: either mid or operator required as cancel subject")
	// ErrExpectedVersionRequired 审批类写接口必须带 CAS 版本。
	ErrExpectedVersionRequired = errors.New("trade-order: expected_version required")
	// ErrInvalidPage 分页参数非法（page<1 或 size 越上限）。
	ErrInvalidPage = errors.New("trade-order: invalid page/size")

	// --- 下游未配置（显式失败，绝不伪造）---

	// ErrMembershipNotConfigured 未配置 membership RPC。
	// CreateOrder 靠它取价（唯一可信价格来源），会员单靠它发放/回收，缺了就必然失败。
	ErrMembershipNotConfigured = errors.New("trade-order: membership rpc not configured")
	// ErrPaymentNotConfigured 未配置 payment RPC：本服务不持有资金台账，
	// 没有它就无处受理支付，因此不允许落一张「看起来已支付」的订单。
	ErrPaymentNotConfigured = errors.New("trade-order: payment rpc not configured")
	// ErrCoinNotConfigured 未配置 coin RPC：硬币包无法发放，履约保持失败态。
	ErrCoinNotConfigured = errors.New("trade-order: coin rpc not configured")

	// --- 套餐（价格来源）判定 ---

	// ErrPlanRequired plan_id 与 plan_code 至少要有一个。
	ErrPlanRequired = errors.New("trade-order: plan_id or plan_code required")
	// ErrPlanNotFound 套餐不存在（membership 明确回 found=false）。
	ErrPlanNotFound = errors.New("trade-order: plan not found")
	// ErrPlanNotOnSale 套餐非 ON_SALE（DRAFT/OFF_SALE）不可下单。
	ErrPlanNotOnSale = errors.New("trade-order: plan not on sale")
	// ErrPlanNotVisibleOnPlatform 套餐对下单端不可见（platforms 未含该端）。
	ErrPlanNotVisibleOnPlatform = errors.New("trade-order: plan not visible on this platform")
	// ErrPlanTierMismatch 套餐与 biz_type 档位不匹配（会员单缺 vip 档、硬币包每份枚数非正）。
	ErrPlanTierMismatch = errors.New("trade-order: plan tier mismatch for biz_type")
	// ErrPlanPriceUnavailable 套餐价格不可用（原价与促销价都非正），无法重算金额。
	ErrPlanPriceUnavailable = errors.New("trade-order: plan price unavailable")
	// ErrAmountMismatch 客户端上报金额与服务端重算不一致（防前端改价）。
	// 包装文本必须带服务端重算值，让调用方能自证并改口径。
	ErrAmountMismatch = errors.New("trade-order: amount_minor mismatch with server recalculated value")

	// --- 状态机与并发 ---

	// ErrOrderNotFound 订单不存在，或带 mid 查询时不属于该用户（not-found 语义，
	// 避免订单号枚举探测）。
	ErrOrderNotFound = errors.New("trade-order: order not found")
	// ErrInvalidStateTransition 状态机不允许的迁移，包装文本必须给出当前状态。
	ErrInvalidStateTransition = errors.New("trade-order: invalid state transition")
	// ErrConcurrentUpdate version CAS 未命中：状态被并发修改，本次写入未生效，调用方可重试。
	ErrConcurrentUpdate = errors.New("trade-order: concurrent state update")
	// ErrFulfillNotAccepted FulfillOrder 只受理 PAID/FULFILLING。
	ErrFulfillNotAccepted = errors.New("trade-order: order is not fulfillable")
	// ErrFulfillAttemptsExhausted 履约尝试已达上限，必须人工介入而不是继续重试。
	ErrFulfillAttemptsExhausted = errors.New("trade-order: fulfill attempts exhausted, manual intervention required")
	// ErrFulfillRetryTooSoon 距上次履约尝试小于 MinSecondsBetweenFulfillRetry，
	// 拒绝以防 cron 打爆下游。
	ErrFulfillRetryTooSoon = errors.New("trade-order: fulfill retry too soon")
	// ErrCancelRejectedPaid 已支付订单不能取消，必须走退款。
	ErrCancelRejectedPaid = errors.New("trade-order: paid order cannot be cancelled, use refund")

	// --- 支付绑定 ---

	// ErrPaymentNoRequired BindPayment 必须带 payment_no。
	ErrPaymentNoRequired = errors.New("trade-order: payment_no required")
	// ErrBindAmountMismatch 绑定的支付金额与订单金额不一致。
	ErrBindAmountMismatch = errors.New("trade-order: bound payment amount mismatch with order amount")
	// ErrBindOnCancelledOrder 已取消订单收到绑款：资金可能已到账，必须人工介入
	// （本服务会留一行台账事件并把错误返回给调用方）。
	ErrBindOnCancelledOrder = errors.New("trade-order: bind payment on cancelled order rejected, manual intervention required")
	// ErrPaymentNotPaid payment 受理成功但未返回 PAID 结论（沙箱下不应发生）。
	ErrPaymentNotPaid = errors.New("trade-order: payment accepted but not settled to PAID")

	// --- 退款 ---

	// ErrRefundNotAccepted RequestRefund 只受理 PAID/FULFILLED。
	ErrRefundNotAccepted = errors.New("trade-order: refund only allowed on PAID/FULFILLED orders")
	// ErrRefundAmountInvalid 请求的退款额非法（>0 的部分退款本沙箱下不支持，见 README 缺口）
	ErrRefundAmountInvalid = errors.New("trade-order: only full refund supported, downstream has no per-order consumption query")
	// ErrRefundNothingToRefund 已全额退过，没有可退余额。
	ErrRefundNothingToRefund = errors.New("trade-order: no refundable amount left on this order")
	// ErrRefundOriginUnknown 台账里找不到进入 REFUND_REQUESTED 的来源状态，
	// 无法确定「回到原状态」的目标，必须人工核对而不是猜一个。
	ErrRefundOriginUnknown = errors.New("trade-order: refund origin state unknown from ledger, manual check required")
	// ErrRefundNotRequested ApproveRefund/RejectRefund 前置状态不满足。
	ErrRefundNotRequested = errors.New("trade-order: order has no pending refund request")

	// --- 查询窗口 ---

	// ErrListWindowRequired 跨用户查询必须给时间窗（无界扫描会锁表）。
	ErrListWindowRequired = errors.New("trade-order: from_ts/to_ts required for cross-user listing")
	// ErrListWindowTooLarge 时间窗超过 MaxListWindowSeconds。
	ErrListWindowTooLarge = errors.New("trade-order: list time window exceeds allowed maximum")
	// ErrFilterRequired ListOrders 至少要有一个过滤条件。
	ErrFilterRequired = errors.New("trade-order: at least one filter required for listing")

	// ErrStuckStateNotAllowed ListStuckOrders 只允许扫描「会卡住」的非终态：
	// 允许终态（CANCELLED/REFUNDED…）做超时扫描没有意义，还会诱导 cron 去重试已结案件。
	ErrStuckStateNotAllowed = errors.New("trade-order: stuck scan only accepts non-terminal states")

	// --- 未完成能力的显式出口 ---

	// ErrNotImplemented 「未实现」占位错误。本轮 12 个 rpc 方法均已实现，没有方法回这个值；
	// 保留它是为了契约新增方法时有一个明确出口：
	// 任何 goctl 生成的骨架都不允许以 `return &rpc.XxxReply{}, nil` 的形式留在仓库里，
	// 那是对调用方的伪成功。缺失的能力宁可报错也不要返回空对象。
	ErrNotImplemented = errors.New("trade-order: not implemented in this round")

	// ErrOrderNoCollision order_no 撞唯一索引（ULID 碰撞，理论极低概率）：
	// 让调用方重试建单，而不是写坏数据。
	ErrOrderNoCollision = errors.New("trade-order: order_no collision, retry create order")
)
