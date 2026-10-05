package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type RequestRefundLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRequestRefundLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RequestRefundLogic {
	return &RequestRefundLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 申请退款
//
// 判定口径（逐条对应交付要求）：
//  1. 只允许 PAID / FULFILLED：未支付单没有可退的钱（走 CancelOrder），
//     FULFILLING/FAILED 的履约结论未定，退了钱就出现「款权益两清不清」，一律拒绝。
//  2. 主体二选一：mid（用户自助，越权按 not-found 语义）或 operator（运营代提），
//     reason 必填 —— 它直接进 to_order_event 台账，是「为什么退」的唯一留证处。
//  3. request_id 幂等：同一 request_id 重放返回 duplicated=true，不再改状态。
//  4. 金额一律服务侧算：amount_minor=0 表示全额退（退 to_order.amount_minor - refunded_minor）。
//     **本沙箱不支持部分退款**：proto 注释把部分退款上限写成「按已消耗时长/已消耗硬币折算」，
//     但 membership 与 coin 都没有「按订单号查消耗量」的接口
//     （membership 只有 GetMembership/CheckEntitlement 的整体剩余，coin 只有账户余额，
//     无法区分某枚硬币来自哪一单），拿不到消耗事实就不给折算额度 ——
//     所以这里只接受 amount_minor=0，任何自报的部分金额都回 ErrRefundAmountInvalid。
//     这条缺口同时写进 README 与交付报告；正确解法是给两个下游各加一个
//     per-order consumption 查询，或给订单加消耗快照列。
//  5. 本方法只登记退款申请，不动钱也不动权益：退款与回收都发生在 ApproveRefund。
func (l *RequestRefundLogic) RequestRefund(in *rpc.RequestRefundReq) (*rpc.RequestRefundReply, error) {
	orderNo := strings.TrimSpace(in.GetOrderNo())
	if orderNo == "" {
		return nil, model.ErrOrderNoRequired
	}
	requestID := strings.TrimSpace(in.GetRequestId())
	if requestID == "" {
		return nil, model.ErrRequestIdRequired
	}
	reason := strings.TrimSpace(in.GetReason())
	if reason == "" {
		return nil, model.ErrReasonRequired
	}
	mid := in.GetMid()
	operator := strings.TrimSpace(in.GetOperator())
	if mid <= 0 && operator == "" {
		return nil, model.ErrSubjectRequired
	}

	order, err := l.svcCtx.Orders.FindByOrderNo(l.ctx, orderNo)
	if err != nil {
		return nil, err
	}
	// 不是他的单与不存在的单同一个出口，不泄露订单号是否存在。
	if order == nil || (mid > 0 && order.Mid != mid) {
		return nil, model.ErrOrderNotFound
	}

	subject := "user"
	if operator != "" {
		subject = truncate(operator, maxOperatorLen)
	}

	// 已在退款流程中：同一 request_id 的重放返回幂等成功，不同 request_id 拒绝重复申请。
	if order.State == model.StateRefundRequested || order.State == model.StateRefundApproved ||
		order.State == model.StateRefunded {
		dup, derr := ledgerDuplicated(l.ctx, l.svcCtx, order.OrderNo, requestID, model.StateRefundRequested)
		if derr != nil {
			return nil, derr
		}
		if dup || order.State != model.StateRefundRequested {
			return &rpc.RequestRefundReply{Duplicated: true, Order: toOrderInfo(order)}, nil
		}
		return nil, fmt.Errorf("%w: state=%s", model.ErrRefundNotAccepted, model.StateName(order.State))
	}

	if order.State != model.StatePaid && order.State != model.StateFulfilled {
		return nil, fmt.Errorf("%w: state=%s", model.ErrRefundNotAccepted, model.StateName(order.State))
	}
	// 没有 payment_no 就没有可退的资金单据：现在拒绝，比审批时才发现诚实。
	if order.PaymentNo == "" {
		return nil, fmt.Errorf("%w: order %s state=%s 未绑定支付单，无法退款",
			model.ErrPaymentNoRequired, order.OrderNo, model.StateName(order.State))
	}

	refundable := order.RefundableMinor()
	if refundable <= 0 {
		return nil, fmt.Errorf("%w: amount_minor=%d refunded_minor=%d",
			model.ErrRefundNothingToRefund, order.AmountMinor, order.RefundedMinor)
	}
	if req := in.GetAmountMinor(); req != 0 {
		return nil, fmt.Errorf("%w: order=%s amount_minor=%d refunded_minor=%d requested=%d，全额退请传 0",
			model.ErrRefundAmountInvalid, order.OrderNo, order.AmountMinor, order.RefundedMinor, req)
	}

	dup, err := ledgerDuplicated(l.ctx, l.svcCtx, order.OrderNo, requestID, model.StateRefundRequested)
	if err != nil {
		return nil, err
	}
	if dup {
		return &rpc.RequestRefundReply{Duplicated: true, Order: toOrderInfo(order)}, nil
	}

	err = transitionWithEvent(l.ctx, l.svcCtx, order.OrderNo, order.State, model.StateRefundRequested,
		order.Version, subject, requestID,
		fmt.Sprintf("refund requested by %s, full amount_minor=%d, payment_no=%s, reason=%s",
			subject, refundable, order.PaymentNo, sanitize(reason)),
		// 附加写入留空：本次申请要退的金额只进台账 reason。fulfill_detail 的语义是
		// 「最近一次履约/回收结论摘要」，不能被待退金额占用，否则审批时看不出真实差异。
		nil)
	if err != nil {
		if !errors.Is(err, model.ErrConcurrentUpdate) {
			return nil, err
		}
		latest, lerr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
		if lerr != nil {
			return nil, lerr
		}
		return &rpc.RequestRefundReply{Duplicated: true, Order: toOrderInfo(latest)}, nil
	}

	final, ferr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
	if ferr != nil {
		return nil, ferr
	}
	l.Infof("trade-order/RequestRefund: order %s refund requested by %s amount=%d",
		order.OrderNo, subject, refundable)
	return &rpc.RequestRefundReply{Duplicated: false, Order: toOrderInfo(final)}, nil
}
