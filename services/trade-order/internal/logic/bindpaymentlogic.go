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

type BindPaymentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBindPaymentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BindPaymentLogic {
	return &BindPaymentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 绑定支付结论（补偿推进）
//
// 供 cron / 运营兜底使用：沙箱建单链路已经内联推进，这里只救「payment 已受理但订单没走到 PAID」的卡单。
// 三条口径：
//  1. 必须带 payment_no，且金额与订单一致才推进 —— 金额不一致说明两边台账已经分叉，
//     推进只会把错账固化，所以直接拒绝。
//  2. 已 PAID 及之后一律 duplicated=true（幂等），不动状态、不再写台账。
//  3. 已 CANCELLED 的订单收到绑款是「钱到了但订单已被关掉」的资金事故信号：
//     拒绝推进，但仍写一行同态台账留证（状态不变，台账必须有迹可循），
//     并以 model.ErrBindOnCancelledOrder 返回，日志按 Error 级别打出来给告警用。
func (l *BindPaymentLogic) BindPayment(in *rpc.BindPaymentReq) (*rpc.BindPaymentReply, error) {
	orderNo := strings.TrimSpace(in.GetOrderNo())
	if orderNo == "" {
		return nil, model.ErrOrderNoRequired
	}
	paymentNo := strings.TrimSpace(in.GetPaymentNo())
	if paymentNo == "" {
		return nil, model.ErrPaymentNoRequired
	}
	requestID := strings.TrimSpace(in.GetRequestId())
	if requestID == "" {
		return nil, model.ErrRequestIdRequired
	}
	operator := strings.TrimSpace(in.GetOperator())
	if operator == "" {
		operator = "cron"
	}
	operator = truncate(operator, maxOperatorLen)

	order, err := l.svcCtx.Orders.FindByOrderNo(l.ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, model.ErrOrderNotFound
	}

	if order.State == model.StateCancelled {
		return l.rejectBindOnCancelled(order, paymentNo, operator, requestID)
	}
	if model.IsPaidOrLater(order.State) {
		if order.PaymentNo != "" && order.PaymentNo != paymentNo {
			// 已经绑过另一笔支付：不覆盖，只如实返回 duplicated，并打日志让人去核对。
			l.Errorf("trade-order/BindPayment: order %s 已绑定 payment_no=%s，本次 %s 被忽略",
				order.OrderNo, order.PaymentNo, paymentNo)
		}
		return &rpc.BindPaymentReply{Duplicated: true, Order: toOrderInfo(order)}, nil
	}
	if in.GetAmountMinor() <= 0 || in.GetAmountMinor() != order.AmountMinor {
		return nil, fmt.Errorf("%w: order_amount_minor=%d bind_amount_minor=%d",
			model.ErrBindAmountMismatch, order.AmountMinor, in.GetAmountMinor())
	}
	dup, err := ledgerDuplicated(l.ctx, l.svcCtx, order.OrderNo, requestID, model.StatePaid)
	if err != nil {
		return nil, err
	}
	if dup {
		return &rpc.BindPaymentReply{Duplicated: true, Order: toOrderInfo(order)}, nil
	}

	// CREATED 不能一步跳到 PAID：先补 PAYING（同一状态机口径，与建单链路一致）。
	if order.State == model.StateCreated {
		if err = transitionWithEvent(l.ctx, l.svcCtx, order.OrderNo,
			model.StateCreated, model.StatePaying, order.Version, operator, requestID,
			"bind payment: CREATED -> PAYING", nil); err != nil {
			if !errors.Is(err, model.ErrConcurrentUpdate) {
				return nil, err
			}
			return &rpc.BindPaymentReply{Duplicated: true, Order: l.reload(order.OrderNo)}, nil
		}
		order, err = l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
		if err != nil {
			return nil, err
		}
		if order == nil {
			return nil, model.ErrOrderNotFound
		}
		if model.IsPaidOrLater(order.State) {
			return &rpc.BindPaymentReply{Duplicated: true, Order: toOrderInfo(order)}, nil
		}
	}
	if order.State != model.StatePaying {
		return nil, fmt.Errorf("%w: 当前状态 %s，只有 CREATED/PAYING 可以绑款",
			model.ErrInvalidStateTransition, model.StateName(order.State))
	}

	if err = transitionWithEvent(l.ctx, l.svcCtx, order.OrderNo,
		model.StatePaying, model.StatePaid, order.Version, operator, requestID,
		"payment bound: "+paymentNo,
		model.NewOrderUpdate().
			PaymentNo(truncate(paymentNo, maxPaymentNoLen)).
			PaidAt(model.NowUnix()).
			FulfillState(model.FulfillPending)); err != nil {
		if !errors.Is(err, model.ErrConcurrentUpdate) {
			return nil, err
		}
		return &rpc.BindPaymentReply{Duplicated: true, Order: l.reload(order.OrderNo)}, nil
	}

	final, ferr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
	if ferr != nil {
		return nil, ferr
	}
	l.Infof("trade-order/BindPayment: order %s advanced to PAID via %s", order.OrderNo, operator)
	return &rpc.BindPaymentReply{Duplicated: false, Order: toOrderInfo(final)}, nil
}

// reload 取当前行（并发抢先时的回复，绝不返回内存里的旧状态）。
func (l *BindPaymentLogic) reload(orderNo string) *rpc.OrderInfo {
	cur, err := l.svcCtx.Orders.FindByOrderNo(l.ctx, orderNo)
	if err != nil || cur == nil {
		return nil
	}
	return toOrderInfo(cur)
}

// rejectBindOnCancelledOrder 落一行「同态」台账留证后拒绝推进。
//
// 台账表的定位是状态迁移轨迹，这里写 from_state = to_state = CANCELLED 是有意为之：
// 它不改订单状态（CANCELLED 也没有任何合法出边，见 model.CanTransition），
// 但让运营能从 to_order_event 直接查出「这笔钱到了但订单是关着的」，
// 而不是只在日志里留一条迟早会滚掉的 Error。
func (l *BindPaymentLogic) rejectBindOnCancelled(order *model.Order, paymentNo, operator, requestID string) (*rpc.BindPaymentReply, error) {
	_, ierr := l.svcCtx.OrderEvents.InsertTx(l.ctx, nil, &model.OrderEvent{
		OrderNo:   order.OrderNo,
		FromState: model.StateCancelled,
		ToState:   model.StateCancelled,
		Operator:  operator,
		Reason:    truncate("bind payment rejected on CANCELLED order, payment_no="+paymentNo+" requires manual refund", maxReasonLen),
		RequestID: truncate(requestID, maxRequestIDLen),
	})
	if ierr != nil {
		l.Errorf("trade-order/BindPayment: 台账留证失败 order %s: %v", order.OrderNo, ierr)
	}
	l.Errorf("trade-order/BindPayment: 已取消订单收到绑款 order=%s payment_no=%s，需人工退回资金",
		order.OrderNo, paymentNo)
	return nil, fmt.Errorf("%w: order=%s payment_no=%s", model.ErrBindOnCancelledOrder, order.OrderNo, paymentNo)
}
