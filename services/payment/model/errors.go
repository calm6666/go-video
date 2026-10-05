package model

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// payment 域错误。
//
// 与多数读侧服务不同，这里直接把 gRPC code 定在错误里：
// payment.proto 的注释对每个判定都写明了要返回 FailedPrecondition 还是
// InvalidArgument（例如余额不足、原路退回未配置、reason 必填），
// 而这些判定是跨服务的资金契约，不能让网关按「未知错误」兜底成 500。
// 消息一律以 `payment: ` 前缀开头，且不包含 SQL 片段、PII 或任何凭据（AGENTS.md §6）。
var (
	// --- InvalidArgument：入参不合法，改调用方 ---

	// ErrInvalidMid mid 非法（资金接口不支持游客与 mid=0）。
	ErrInvalidMid = errf(codes.InvalidArgument, "payment: invalid mid")
	// ErrUnsupportedCurrency 本服务只支持配置中的单一币种，不做隐式换汇。
	ErrUnsupportedCurrency = errf(codes.InvalidArgument, "payment: unsupported currency, only the configured default currency is supported")
	// ErrAmountNotPositive 金额必须为正（分）。
	ErrAmountNotPositive = errf(codes.InvalidArgument, "payment: amount_minor must be greater than zero")
	// ErrRechargeAmountOutOfRange 充值金额超出 Payment.MinRechargeMinor/MaxRechargeMinor 区间。
	ErrRechargeAmountOutOfRange = errf(codes.InvalidArgument, "payment: recharge amount out of configured range")
	// ErrAdjustDeltaZero 运营调整的 delta 不允许为 0（0 变动也会写出无意义流水）。
	ErrAdjustDeltaZero = errf(codes.InvalidArgument, "payment: delta_minor must not be zero")
	// ErrAdjustAmountOutOfRange 单次运营调整绝对值超出 Payment.MaxAdjustMinor。
	ErrAdjustAmountOutOfRange = errf(codes.InvalidArgument, "payment: adjust delta out of configured range")
	// ErrRequestIDRequired 写接口缺少幂等键；没有它无法区分重试与重复下单。
	ErrRequestIDRequired = errf(codes.InvalidArgument, "payment: request_id required for write operations")
	// ErrOperatorRequired 写接口缺少操作人，资金变更必须有审计主体。
	ErrOperatorRequired = errf(codes.InvalidArgument, "payment: operator required")
	// ErrReasonRequired Cancel/Refund/Close/Adjust 的 reason 必填。
	ErrReasonRequired = errf(codes.InvalidArgument, "payment: reason required")
	// ErrRechargeNoRequired recharge_no 为空。
	ErrRechargeNoRequired = errf(codes.InvalidArgument, "payment: recharge_no required")
	// ErrPaymentNoRequired payment_no 为空。
	ErrPaymentNoRequired = errf(codes.InvalidArgument, "payment: payment_no required")
	// ErrBizOrderNoRequired biz_order_no 为空（一单一支付的判定键）。
	ErrBizOrderNoRequired = errf(codes.InvalidArgument, "payment: biz_order_no required")
	// ErrGetPaymentKeyRequired GetPayment 必须给 payment_no 或 biz_order_no 之一。
	ErrGetPaymentKeyRequired = errf(codes.InvalidArgument, "payment: payment_no or biz_order_no required")
	// ErrGetPaymentKeyExclusive GetPayment 不允许同时给两个键（避免歧义命中）。
	ErrGetPaymentKeyExclusive = errf(codes.InvalidArgument, "payment: payment_no and biz_order_no are mutually exclusive")
	// ErrInvalidMethod 支付方式不在受理范围（只支持 BALANCE 与 SANDBOX_CHANNEL）。
	ErrInvalidMethod = errf(codes.InvalidArgument, "payment: unsupported pay method")
	// ErrInvalidChannel 渠道非法：本服务只定义并只受理 SANDBOX。
	ErrInvalidChannel = errf(codes.InvalidArgument, "payment: unsupported pay channel, only SANDBOX exists")
	// ErrInvalidBizType 流水业务类型非法（不在 1..4 内）。
	// 过滤条件写错必须报错，否则运营会把「查错类型」当成「没有资金变动」。
	ErrInvalidBizType = errf(codes.InvalidArgument, "payment: unsupported flow biz_type")
	// ErrInvalidTimeRange 列表时间窗 from_ts > to_ts。
	ErrInvalidTimeRange = errf(codes.InvalidArgument, "payment: from_ts must not be greater than to_ts")
	// ErrListWindowRequired 跨用户（mid=0）查询必须给时间窗，否则就是全表扫。
	ErrListWindowRequired = errf(codes.InvalidArgument, "payment: cross-user listing (mid=0) requires both from_ts and to_ts")
	// ErrListWindowTooLarge 跨用户查询时间窗超过 Payment.MaxListWindowSeconds，拒绝而不是扫全表。
	ErrListWindowTooLarge = errf(codes.InvalidArgument, "payment: listing time window exceeds Payment.MaxListWindowSeconds")
	// ErrPageSizeTooLarge size 超过 Payment.MaxPageSize。
	ErrPageSizeTooLarge = errf(codes.InvalidArgument, "payment: page size exceeds Payment.MaxPageSize")
	// ErrListOffsetTooDeep 翻页偏移超过 Payment.MaxListOffset，要求改用时间窗缩小集合。
	ErrListOffsetTooDeep = errf(codes.InvalidArgument, "payment: listing offset exceeds Payment.MaxListOffset")
	// ErrExpireInPast expire_at 给了过去的时间点。
	ErrExpireInPast = errf(codes.InvalidArgument, "payment: expire_at must be 0 (never expire) or a future timestamp")
	// ErrRefundAmountNotPositive 退款金额 <= 0（0 表示全额剩余可退，负数非法）。
	ErrRefundAmountNotPositive = errf(codes.InvalidArgument, "payment: refund amount_minor must be >= 0")

	// --- FailedPrecondition：入参可能没问题，但当前台账状态不允许 ---

	// ErrChannelNotConfigured 请求了未接入的真实渠道。消息保留 proto 的原话
	// （"payment channel not configured"）作为子串，只加了服务名前缀。
	ErrChannelNotConfigured = errf(codes.FailedPrecondition, "payment: payment channel not configured")
	// ErrRefundToChannelNotConfigured 要求原路退回渠道：沙箱里没有渠道凭证可退，
	// 明确拒绝，绝不返回「已退回」。
	ErrRefundToChannelNotConfigured = errf(codes.FailedPrecondition,
		"payment: refund to original channel not configured; only refund to balance is supported")
	// ErrRefundRequiresBalancePayment 退余额只对余额支付成立。沙箱渠道支付的单
	// 从未占用余额，退成余额等于凭空造出台账数字，因此一并拒绝。
	ErrRefundRequiresBalancePayment = errf(codes.FailedPrecondition,
		"payment: refund to balance requires a balance payment; original-channel refund not configured")
	// ErrInsufficientBalance 条件扣减影响行数为 0：余额不足。
	ErrInsufficientBalance = errf(codes.FailedPrecondition, "payment: insufficient balance")
	// ErrRechargeNotPending 只有 PENDING 充值单可取消。
	ErrRechargeNotPending = errf(codes.FailedPrecondition, "payment: only a pending recharge can be cancelled")
	// ErrRechargeAlreadySettled 已入账的充值单不能靠取消回滚，必须走退款/调整。
	ErrRechargeAlreadySettled = errf(codes.FailedPrecondition,
		"payment: recharge already settled, cannot cancel; reverse it with a refund or AdjustBalance")
	// ErrPaymentNotPending 只有 PENDING 支付单可关闭。
	ErrPaymentNotPending = errf(codes.FailedPrecondition, "payment: only a pending payment can be closed")
	// ErrPaymentAlreadyPaid 已支付的单要退回必须走退款路径。
	ErrPaymentAlreadyPaid = errf(codes.FailedPrecondition,
		"payment: payment already paid, cannot close; use RefundPayment instead")
	// ErrPaymentNotRefundable PENDING/FAILED/CLOSED 单没有真实收账，不可退。
	ErrPaymentNotRefundable = errf(codes.FailedPrecondition,
		"payment: payment state is not refundable (pending/failed/closed never captured money)")
	// ErrRefundExceedsAmount 累计退款不得超过 amount_minor。
	ErrRefundExceedsAmount = errf(codes.FailedPrecondition, "payment: refund amount exceeds refundable balance")
	// ErrInvalidStateTransition 兜底：状态被并发推进到不可操作的位置。
	ErrInvalidStateTransition = errf(codes.FailedPrecondition, "payment: invalid state transition")

	// --- NotFound ---

	// ErrRechargeNotFound 充值单不存在。
	ErrRechargeNotFound = errf(codes.NotFound, "payment: recharge not found")
	// ErrPaymentNotFound 支付单不存在。
	ErrPaymentNotFound = errf(codes.NotFound, "payment: payment not found")

	// --- AlreadyExists ---

	// ErrPaymentOrderConflict 同一 biz_order_no 已存在金额或币种不同的支付单：
	// 必须报错，绝不静默改价。
	ErrPaymentOrderConflict = errf(codes.AlreadyExists,
		"payment: biz_order_no already has a payment with different amount or currency")
	// ErrRequestIDReused 同一个 request_id 被用在另一笔资金变动上：流水唯一键冲突，
	// 事务已整体回滚（台账没有任何变化），调用方必须换新号重试。
	ErrRequestIDReused = errf(codes.AlreadyExists,
		"payment: request_id already used by another ledger entry, nothing was charged or credited, retry with a new request_id")

	// --- Aborted：并发冲突，调用方可安全重试 ---

	// ErrConcurrentUpdate 条件更新未命中且状态已被他人推进。
	ErrConcurrentUpdate = errf(codes.Aborted, "payment: concurrent ledger update, safe to retry")

	// --- Unimplemented：本轮明确不开的能力 ---

	// ErrNotImplemented 兜底未实现方法。宁可报未实现，也不返回 goctl 骨架的伪成功。
	ErrNotImplemented = errf(codes.Unimplemented, "payment: not implemented")

	// --- Internal：环境故障，不是调用方能修的问题 ---

	// ErrDocumentNoUnavailable 单据号生成器（common/idgen）取不到熵。
	// 这时必须失败，不能退化成「用时间戳拼一个」——那会造成撞号并留下重复入账口子。
	ErrDocumentNoUnavailable = errf(codes.Internal, "payment: document number generator unavailable")
)

// errf 构造带 gRPC code 的错误。
func errf(code codes.Code, msg string) error { return status.Error(code, msg) }

// ErrTextTooLong 摘要/理由类字段超长（DB 列宽 255，按 rune 判定）。
func ErrTextTooLong(field string) error {
	return errf(codes.InvalidArgument, "payment: "+field+" too long")
}

// ErrDetail 在哨兵消息后补充排障上下文（不写凭据与 PII）。
func ErrDetail(base error, detail string) error {
	if st, ok := status.FromError(base); ok {
		return errf(st.Code(), st.Message()+"; "+detail)
	}
	return errf(codes.FailedPrecondition, base.Error()+"; "+detail)
}

// IsNotFound 判断是否为单据不存在。
func IsNotFound(err error) bool { return status.Code(err) == codes.NotFound }

// isDuplicateErr 识别 MySQL 唯一索引冲突（1062 / "Duplicate entry"）。
// 不引入 go-sql-driver/mysql 的 *MySQLError（本仓库禁止新增依赖）。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}
