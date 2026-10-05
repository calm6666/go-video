package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	paymentrpc "go-video/services/payment/rpc"
	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type CancelOrderLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelOrderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelOrderLogic {
	return &CancelOrderLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取消未支付订单
//
// 口径：
//  1. 只允许 CREATED / PAYING；已 PAID 及之后必须走退款，不能靠取消把钱吞掉。
//  2. 主体二选一：mid（本人自助，越权按 not-found 语义）或 operator（运营代取消）。
//     reason 必填（proto 注释即要求必填），因为它直接进状态流转台账。
//  3. request_id 幂等：同一 request_id 重放返回 duplicated=true 且不再动状态。
//  4. PAYING 单取消前必须向 payment 核对支付结论 —— 这是本方法最容易被忽略的一步：
//     资金可能已经在 payment 侧扣掉但本地订单还停在 PAYING（内联推进途中进程崩溃），
//     此时直接取消就等于把钱吞在别人的台账里。查得到已 PAID 的支付单一律拒绝取消，
//     查得到未支付的支付单先关单再取消，关不掉就报错让调用方重试。
func (l *CancelOrderLogic) CancelOrder(in *rpc.CancelOrderReq) (*rpc.CancelOrderReply, error) {
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
	// 不存在与不是他的订单走同一个出口，不泄露订单号是否存在。
	if order == nil || (mid > 0 && order.Mid != mid) {
		return nil, model.ErrOrderNotFound
	}

	// 运营代取消必须以 operator 记台账；用户自助记 "user"。
	subject := "user"
	if operator != "" {
		subject = truncate(operator, maxOperatorLen)
	}

	dup, err := ledgerDuplicated(l.ctx, l.svcCtx, order.OrderNo, requestID, model.StateCancelled)
	if err != nil {
		return nil, err
	}
	if dup || order.State == model.StateCancelled {
		return &rpc.CancelOrderReply{Duplicated: true, Order: toOrderInfo(order)}, nil
	}
	if order.State != model.StateCreated && order.State != model.StatePaying {
		return nil, fmt.Errorf("%w: state=%s，已支付的单请走 RequestRefund",
			model.ErrCancelRejectedPaid, model.StateName(order.State))
	}
	if order.PaymentNo != "" {
		// 已绑定支付单号说明支付结论已经落本地，属于「已付款」事实，只能退不能取消。
		return nil, fmt.Errorf("%w: state=%s, payment_no bound", model.ErrCancelRejectedPaid, model.StateName(order.State))
	}

	if order.State == model.StatePaying {
		if blocked, berr := l.paymentAlreadySettled(order); berr != nil {
			return nil, berr
		} else if blocked {
			return nil, fmt.Errorf("%w: payment settled but order still %s, refund instead",
				model.ErrCancelRejectedPaid, model.StateName(order.State))
		}
	}

	err = transitionWithEvent(l.ctx, l.svcCtx, order.OrderNo, order.State, model.StateCancelled, order.Version,
		subject, requestID, "cancelled by "+subject+": "+sanitize(reason),
		model.NewOrderUpdate().ClosedAt(model.NowUnix()))
	if err != nil {
		if !errors.Is(err, model.ErrConcurrentUpdate) {
			return nil, err
		}
		latest, lerr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
		if lerr != nil {
			return nil, lerr
		}
		if latest == nil {
			return nil, model.ErrOrderNotFound
		}
		return &rpc.CancelOrderReply{Duplicated: true, Order: toOrderInfo(latest)}, nil
	}

	final, ferr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
	if ferr != nil {
		return nil, ferr
	}
	l.Infof("trade-order/CancelOrder: order %s cancelled by %s", order.OrderNo, subject)
	return &rpc.CancelOrderReply{Duplicated: false, Order: toOrderInfo(final)}, nil
}

// paymentAlreadySettled 向 payment 核对：这单是否已经存在一笔「钱已经动过」的支付单。
// 返回 true 表示不允许取消。同时尽力把未支付的支付单关掉，避免留下悬账。
//
// payment 未配置时返回 false：没有客户端就没有任何资金路径能创建支付单，取消是安全的。
func (l *CancelOrderLogic) paymentAlreadySettled(order *model.Order) (bool, error) {
	if l.svcCtx.Payment == nil {
		return false, nil
	}
	reply, err := l.svcCtx.Payment.GetPayment(l.ctx, &paymentrpc.GetPaymentReq{BizOrderNo: order.OrderNo})
	if err != nil {
		// 查不到结论就不能取消：宁可让调用方重试，也不能在资金状态未知时把订单关掉。
		return false, fmt.Errorf("payment.GetPayment: %w", err)
	}
	if !reply.GetFound() || reply.GetPayment() == nil {
		return false, nil
	}
	p := reply.GetPayment()
	switch p.GetState() {
	case paymentrpc.PaymentState_PAYMENT_STATE_PAID,
		paymentrpc.PaymentState_PAYMENT_STATE_REFUNDED,
		paymentrpc.PaymentState_PAYMENT_STATE_PARTIALLY_REFUNDED:
		return true, nil
	case paymentrpc.PaymentState_PAYMENT_STATE_PENDING:
		if l.svcCtx.Payment == nil {
			return false, model.ErrPaymentNotConfigured
		}
		_, cerr := l.svcCtx.Payment.ClosePayment(l.ctx, &paymentrpc.ClosePaymentReq{
			PaymentNo: p.GetPaymentNo(),
			Operator:  "trade-order",
			RequestId: deriveKey("close", order.OrderNo),
			Reason:    "order cancelled before payment settled",
		})
		if cerr != nil {
			return false, fmt.Errorf("payment.ClosePayment: %w", cerr)
		}
		return false, nil
	default:
		// FAILED / CLOSED：没有资金占用，可以取消。
		return false, nil
	}
}
