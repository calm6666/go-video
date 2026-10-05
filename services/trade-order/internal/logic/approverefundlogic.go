package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	coinrpc "go-video/services/coin/rpc"
	memberrpc "go-video/services/membership/rpc"
	paymentrpc "go-video/services/payment/rpc"
	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type ApproveRefundLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewApproveRefundLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ApproveRefundLogic {
	return &ApproveRefundLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 审批通过并回收权益
//
// 顺序严格按 proto 注释：**先退钱（payment.RefundPayment, to_balance=true），再回收权益**，
// 最后才允许把订单标成 REFUNDED。三段之间任何一段失败都不会被伪装成成功：
//
//	refund 失败            → 订单留在 REFUND_REQUESTED，返回错误（什么都没改）。
//	退钱成功、推进失败      → Error 日志带 refund_no；重试会用同一把派生幂等键，
//	                        payment 侧回 duplicated 而不重复退钱。
//	退钱成功、回收失败      → 订单**停在 REFUND_APPROVED**，把「已退未回收」写进
//	                        fulfill_detail + 同态台账，并在 reply.revoke_detail 里回结论。
//	                        绝不标 REFUNDED（proto 注释明确要求）。
//	退钱 + 回收都成功       → CAS 到 REFUNDED，清空 fulfill_detail。
//
// 幂等锚点是「状态 + refunded_minor」而不是 request_id 台账：
// REFUND_APPROVED 的语义就是「款已退、回收未完成」，它必须是可重入的重试入口，
// 若被 request_id 短路成 duplicated，权益就永远收不回来了。
// 所有下游调用都用订单号派生的确定性幂等键（refund_/revoke_ + order_no），
// 因此重放不会重复退钱、也不会重复扣权益（下游回 duplicated 视为成功）。
//
// 必填项：operator（审批人只能由网关按会话渲染）、reason、request_id、expected_version。
// expected_version 是订单 CAS 位点，防止审批与用户取消/并发审批互相覆盖。
func (l *ApproveRefundLogic) ApproveRefund(in *rpc.ApproveRefundReq) (*rpc.ApproveRefundReply, error) {
	orderNo := strings.TrimSpace(in.GetOrderNo())
	if orderNo == "" {
		return nil, model.ErrOrderNoRequired
	}
	operator := strings.TrimSpace(in.GetOperator())
	if operator == "" {
		return nil, model.ErrOperatorRequired
	}
	operator = truncate(operator, maxOperatorLen)
	reason := strings.TrimSpace(in.GetReason())
	if reason == "" {
		return nil, model.ErrReasonRequired
	}
	requestID := strings.TrimSpace(in.GetRequestId())
	if requestID == "" {
		return nil, model.ErrRequestIdRequired
	}
	if in.GetExpectedVersion() <= 0 {
		return nil, model.ErrExpectedVersionRequired
	}

	order, err := l.svcCtx.Orders.FindByOrderNo(l.ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, model.ErrOrderNotFound
	}

	switch order.State {
	case model.StateRefunded:
		// 已结案件：幂等重放，钱和权益都已处理完。
		return &rpc.ApproveRefundReply{
			Duplicated: true,
			Order:      toOrderInfo(order),
			RefundNo:   l.lookupRefundNo(order),
			RevokeDetail: fmt.Sprintf("退款已完成，refunded_minor=%d/%d，本次为幂等重放",
				order.RefundedMinor, order.AmountMinor),
		}, nil
	case model.StateRefundRequested:
		// 往下走完整流程。
	case model.StateRefundApproved:
		// 重试入口：上一次已退钱、回收未完成，本次只重试回收。
		return l.retryRevoke(order, operator, requestID, reason, in.GetExpectedVersion())
	default:
		return nil, fmt.Errorf("%w: state=%s，只有 REFUND_REQUESTED/REFUND_APPROVED 可审批",
			model.ErrRefundNotRequested, model.StateName(order.State))
	}

	if order.PaymentNo == "" {
		return nil, fmt.Errorf("%w: order=%s 未绑定支付单，无法退款",
			model.ErrPaymentNoRequired, order.OrderNo)
	}
	// 金额在审批这一刻重算：申请到审批之间可能已被别的退款扣减。
	refundable := order.RefundableMinor()
	if refundable <= 0 {
		return nil, fmt.Errorf("%w: amount_minor=%d refunded_minor=%d",
			model.ErrRefundNothingToRefund, order.AmountMinor, order.RefundedMinor)
	}

	refundNo := ""
	var added int64
	upd := model.NewOrderUpdate().FulfillDetail(truncate("已退款待回收权益", maxDetailLen))
	if order.RefundedMinor > 0 {
		// REFUND_REQUESTED 且本地已记过退款额：只有人工改账才可能出现。
		// 不再退钱（派生幂等键也会挡住重复退款），只补推进与回收，退款单号回源 payment 查。
		l.Errorf("trade-order/ApproveRefund: order %s 处于 REFUND_REQUESTED 但 refunded_minor=%d，跳过退款只补推进与回收",
			order.OrderNo, order.RefundedMinor)
		refundNo = l.lookupRefundNo(order)
	} else {
		// 第 1 步：退钱。失败即返回，订单状态未动，可安全重试。
		rno, amount, rerr := l.refundToBalance(order, refundable, operator, reason)
		if rerr != nil {
			return nil, rerr
		}
		refundNo, added = rno, amount
		// 第 2 步：钱已动 → 立刻把事实落账（CAS + 台账同事务，refunded_minor 增量记账）。
		// refunded_minor 只在「payment 确认退款成功」这一步之后累加，它就是重试时判断
		// 「钱是不是已经退过」的唯一本地依据。
		upd.AddRefunded(added).
			FulfillDetail(truncate("已退款待回收权益，refund_no="+refundNo, maxDetailLen))
	}

	if err = transitionWithEvent(l.ctx, l.svcCtx, order.OrderNo,
		model.StateRefundRequested, model.StateRefundApproved, in.GetExpectedVersion(),
		operator, requestID,
		fmt.Sprintf("refund approved by %s, refund_no=%s, add_refunded_minor=%d, reason=%s",
			operator, refundNo, added, sanitize(reason)), upd); err != nil {
		if errors.Is(err, model.ErrConcurrentUpdate) {
			// 最需要人看的一条：钱退了、订单没推进。重试会用同一把派生键，不会重复退钱。
			l.Errorf("trade-order/ApproveRefund: order %s 退款已提交(refund_no=%s) "+
				"但订单推进 REFUND_APPROVED 未命中，请用最新 version 重试: %v",
				order.OrderNo, refundNo, err)
		}
		return nil, err
	}

	// 第 3 步：回读拿到位点后回收权益。
	current, err := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, model.ErrOrderNotFound
	}
	return l.revokeAndClose(current, operator, requestID, reason, current.Version, refundNo)
}

// retryRevoke 是 REFUND_APPROVED 的可重入重试：款已退，只补回收权益这一步。
//
// 这里必须真的再跑一次回收，而不是按 request_id 回 duplicated —— 见 ApproveRefund 的幂等口径注释。
func (l *ApproveRefundLogic) retryRevoke(order *model.Order, operator, requestID, reason string,
	version int64,
) (*rpc.ApproveRefundReply, error) {
	if order.Version != version {
		// 审批类写接口不接受「版本过期就顺着改」：先回读，让人确认状态没被并行动过。
		return nil, fmt.Errorf("%w: expected_version=%d, current_version=%d, state=%s",
			model.ErrConcurrentUpdate, version, order.Version, model.StateName(order.State))
	}
	l.Infof("trade-order/ApproveRefund: order %s 停在 REFUND_APPROVED，重试权益回收 by %s", order.OrderNo, operator)
	return l.revokeAndClose(order, operator, requestID, reason, order.Version, l.lookupRefundNo(order))
}

// revokeAndClose 回收权益并决定订单停在哪一步。
//
// 回收失败 → 停在 REFUND_APPROVED（原地补写 fulfill_detail + 同态台账），回 reply 带结论，
// 不返回错误：proto 把 revoke_detail 定义为回复字段，带错误的 gRPC 回复会被丢弃，
// 而订单状态本身已经如实表达了「未完成」。
func (l *ApproveRefundLogic) revokeAndClose(order *model.Order, operator, requestID, reason string,
	version int64, refundNo string,
) (*rpc.ApproveRefundReply, error) {
	revokeDetail, revokeErr := l.revokeEntitlement(order, operator, reason)

	if revokeErr != nil {
		note := truncate("款已退、权益未回收："+revokeDetail, maxDetailLen)
		l.Errorf("trade-order/ApproveRefund: order %s 已退款(refund_no=%s) 但权益回收失败，"+
			"停在 REFUND_APPROVED 等重试: %v", order.OrderNo, refundNo, revokeErr)
		if aerr := l.annotate(order, version, note, operator, requestID,
			"revoke failed: "+sanitize(revokeErr.Error())); aerr != nil {
			l.Errorf("trade-order/ApproveRefund: order %s 差异留痕失败: %v", order.OrderNo, aerr)
		}
		final, ferr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
		if ferr != nil {
			return nil, ferr
		}
		return &rpc.ApproveRefundReply{
			Duplicated:   false,
			Order:        toOrderInfo(final),
			RefundNo:     refundNo,
			RevokeDetail: truncate(note, maxDetailLen),
		}, nil
	}

	// 回收成功才允许进入 REFUNDED（终态）。
	err := transitionWithEvent(l.ctx, l.svcCtx, order.OrderNo,
		model.StateRefundApproved, model.StateRefunded, version, operator, requestID,
		fmt.Sprintf("refunded and entitlement revoked, refund_no=%s, %s, operator_note=%s",
			refundNo, revokeDetail, sanitize(reason)),
		model.NewOrderUpdate().FulfillDetail(""))
	if err != nil {
		if errors.Is(err, model.ErrConcurrentUpdate) {
			l.Errorf("trade-order/ApproveRefund: order %s 权益已回收(%s) 但推进 REFUNDED 未命中，"+
				"重试会以幂等键自愈: %v", order.OrderNo, revokeDetail, err)
		}
		return nil, err
	}

	final, ferr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
	if ferr != nil {
		return nil, ferr
	}
	l.Infof("trade-order/ApproveRefund: order %s refunded by %s, refund_no=%s", order.OrderNo, operator, refundNo)
	return &rpc.ApproveRefundReply{
		Duplicated:   false,
		Order:        toOrderInfo(final),
		RefundNo:     refundNo,
		RevokeDetail: truncate(revokeDetail, maxDetailLen),
	}, nil
}

// refundToBalance 调 payment 把余额退回去（沙箱只开这一条退款路径）。
//
// 幂等键是 refund_ + order_no（订单号派生，不是本次 request_id）：
// 审批重试退的是同一笔钱，若透传 request_id 就会重复退款。
// 返回的 refunded 以 payment 回的实际金额为准，两侧台账分叉时不猜数。
func (l *ApproveRefundLogic) refundToBalance(order *model.Order, amount int64, operator, reason string,
) (refundNo string, refunded int64, err error) {
	if l.svcCtx.Payment == nil {
		return "", 0, fmt.Errorf("%w: order=%s 退款必须经 payment 台账", model.ErrPaymentNotConfigured, order.OrderNo)
	}
	reply, cerr := l.svcCtx.Payment.RefundPayment(l.ctx, &paymentrpc.RefundPaymentReq{
		PaymentNo:   order.PaymentNo,
		AmountMinor: amount,
		ToBalance:   true, // 原路退回真实渠道需要渠道凭证，本服务不开该路径。
		Operator:    operator,
		RequestId:   deriveKey("refund", order.OrderNo),
		Reason:      truncate("order refund "+order.OrderNo+": "+sanitize(reason), maxReasonLen),
	})
	if cerr != nil {
		return "", 0, fmt.Errorf("payment.RefundPayment: %w", cerr)
	}
	if reply == nil || reply.GetRefund() == nil {
		return "", 0, errors.New("payment.RefundPayment: empty reply")
	}
	r := reply.GetRefund()
	if r.GetState() != paymentrpc.RefundState_REFUND_STATE_SUCCEEDED {
		return r.GetRefundNo(), 0, fmt.Errorf("%w: payment 退款未成功，refund_no=%s state=%d",
			model.ErrRefundNotAccepted, r.GetRefundNo(), int32(r.GetState()))
	}
	actual := r.GetAmountMinor()
	if actual <= 0 {
		actual = amount
	}
	if actual != amount {
		// 不因为对方面额不同就猜：按对方实际退的记账，并把差异写进日志与摘要。
		l.Errorf("trade-order/ApproveRefund: order %s 退款面额分叉 order_asked=%d payment_refunded=%d",
			order.OrderNo, amount, actual)
	}
	if r.GetCurrency() != "" && r.GetCurrency() != order.Currency {
		l.Errorf("trade-order/ApproveRefund: order %s 币种分叉 order=%s payment=%s",
			order.OrderNo, order.Currency, r.GetCurrency())
	}
	if reply.GetDuplicated() {
		l.Infof("trade-order/ApproveRefund: order %s 退款被 payment 判为重放 refund_no=%s", order.OrderNo, r.GetRefundNo())
	}
	return r.GetRefundNo(), actual, nil
}

// revokeEntitlement 回收已发放的权益。返回结论摘要与失败原因。
//
// 会员单：RevokeMembership(ClearRemaining=true) —— 全额定金退回后剩余时长立即失效。
// 之所以不是「按未使用天数扣回」，与不支持部分退款是同一个原因：
// 拿不到本单的消耗事实，扣天数会把别人授予的区间也扣掉。
// 硬币包：GrantCoin(delta=-coin_amount) 整包扣回；用户已把硬币投出去时 coin 会拒绝
// 把余额扣成负数，这里就停在 REFUND_APPROVED 等人工处理（钱已退，权益收不回来），
// 绝不因为「退不了」而假装回收成功。
func (l *ApproveRefundLogic) revokeEntitlement(order *model.Order, operator, reason string,
) (string, error) {
	if order.GrantRef == "" {
		// 未履约成功的单没有发放记录，没有东西可回收。
		return "无发放记录（grant_ref 为空），无需回收权益", nil
	}
	switch order.BizType {
	case model.BizMembership:
		if l.svcCtx.Membership == nil {
			return "membership 未配置，无法回收会员权益", model.ErrMembershipNotConfigured
		}
		vipType, verr := planVipType(l.ctx, l.svcCtx, order)
		if verr != nil {
			return "回查套餐档位失败，未回收会员权益: " + sanitize(verr.Error()), verr
		}
		reply, cerr := l.svcCtx.Membership.RevokeMembership(l.ctx, &memberrpc.RevokeMembershipReq{
			Mid:            order.Mid,
			VipType:        vipType,
			ClearRemaining: true, // 全额退款 ⇒ 剩余时长作废（此时 DeltaDays 必须为 0）。
			Operator:       operator,
			RequestId:      deriveKey("revoke", order.OrderNo),
			Reason:         truncate("refund revoke "+order.OrderNo+": "+sanitize(reason), maxReasonLen),
		})
		if cerr != nil {
			return "membership.RevokeMembership 失败，未回收会员权益: " + sanitize(cerr.Error()),
				fmt.Errorf("membership.RevokeMembership: %w", cerr)
		}
		if reply == nil {
			return "membership.RevokeMembership 回复为空，未确认回收", errors.New("membership.RevokeMembership: empty reply")
		}
		return fmt.Sprintf("会员权益已回收（剩余时长作废），revoke_grant_id=%d%s",
			reply.GetGrantId(), dupMark(reply.GetDuplicated())), nil

	case model.BizCoinPack:
		if l.svcCtx.Coin == nil {
			return "coin 未配置，无法扣回硬币", model.ErrCoinNotConfigured
		}
		if order.CoinAmount <= 0 {
			return "订单 coin_amount 非正，无需扣回硬币", nil
		}
		reply, cerr := l.svcCtx.Coin.GrantCoin(l.ctx, &coinrpc.GrantCoinReq{
			Mid:       order.Mid,
			Delta:     -int64(order.CoinAmount), // 负数即扣回，幂等键保证重试不重复扣。
			FlowType:  coinrpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK,
			BizNo:     order.OrderNo,
			Operator:  "trade-order",
			RequestId: deriveKey("revoke", order.OrderNo),
			Reason:    truncate("refund revoke "+order.OrderNo+": "+sanitize(reason), maxReasonLen),
		})
		if cerr != nil {
			// 最常见的一条：硬币已花掉，余额扣不成负数 —— 钱已退但收不回，必须人工介决。
			return "coin.GrantCoin 扣回失败（硬币可能已消耗），未回收硬币: " + sanitize(cerr.Error()),
				fmt.Errorf("coin.GrantCoin revoke: %w", cerr)
		}
		if reply == nil {
			return "coin.GrantCoin 回复为空，未确认扣回", errors.New("coin.GrantCoin: empty reply")
		}
		return fmt.Sprintf("硬币已扣回 %d 枚，revoke_flow_id=%d%s",
			order.CoinAmount, reply.GetFlowId(), dupMark(reply.GetDuplicated())), nil

	default:
		return fmt.Sprintf("未知 biz_type=%d，未回收权益", order.BizType),
			fmt.Errorf("%w: biz_type=%d", model.ErrInvalidBizType, order.BizType)
	}
}

// annotate 在不改变状态的前提下把差异写进 fulfill_detail，并补一行同态台账留证。
func (l *ApproveRefundLogic) annotate(order *model.Order, version int64, detail, operator, requestID, note string) error {
	return l.svcCtx.Transact(l.ctx, func(txCtx context.Context, tx sqlSession) error {
		ok, err := l.svcCtx.Orders.AnnotateTx(txCtx, tx, order.OrderNo, model.StateRefundApproved, version, detail)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		_, err = l.svcCtx.OrderEvents.InsertTx(txCtx, tx, &model.OrderEvent{
			OrderNo:   order.OrderNo,
			FromState: model.StateRefundApproved,
			ToState:   model.StateRefundApproved, // 同态行：状态没动，但必须留下「谁在何时发现差异」的证据。
			Operator:  operator,
			Reason:    truncate(note, maxReasonLen),
			RequestID: truncate(requestID, maxRequestIDLen),
		})
		return err
	})
}

// lookupRefundNo 回查 payment 拿这笔订单的退款单号（幂等重放时本地没有该列，只能回源）。
// 用派生幂等键 refund_ + order_no 精确匹配，而不是取「列表里最新一条」——
// 列表排序不是契约，靠它挑单号会在多次退款场景里拿错。
// 查不到就回空串：宁可让回复少一个字段，也不编一个单号出来。
func (l *ApproveRefundLogic) lookupRefundNo(order *model.Order) string {
	if l.svcCtx.Payment == nil || order == nil || order.PaymentNo == "" {
		return ""
	}
	reply, err := l.svcCtx.Payment.ListRefunds(l.ctx, &paymentrpc.ListRefundsReq{
		Mid:       order.Mid,
		PaymentNo: order.PaymentNo,
		Page:      1,
		Size:      20,
	})
	if err != nil || reply == nil {
		l.Infof("trade-order/ApproveRefund: 回查退款单号失败 order=%s: %v", order.OrderNo, err)
		return ""
	}
	key := deriveKey("refund", order.OrderNo)
	for _, r := range reply.GetRefunds() {
		if r.GetRequestId() == key {
			return r.GetRefundNo()
		}
	}
	return ""
}
