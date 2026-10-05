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

type RejectRefundLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRejectRefundLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RejectRefundLogic {
	return &RejectRefundLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 驳回退款
//
// 目标状态取「申请前的原状态」而不是新增一个 REFUND_REJECTED 落点，理由写在这里：
// proto 状态机注释只给了 `REFUND_REQUESTED → FULFILLED（驳回退款，回到原状态）` 这一条边，
// 而 ORDER_STATE_REFUND_REJECTED(11) 在文件头的迁移表里既没有入边也没有出边 ——
// 真把订单推进去就成了死胡同（钱没退、权益还在、订单永远动不了）。
// 所以「驳回」按契约意图实现为回到原状态，驳回这一事实由 to_order_event 台账留证
// （from=REFUND_REQUESTED、to=原状态、reason 带 refund rejected），
// 主表不留痕是符合「订单事实 = 当前状态」这个口径的。
// 这条枚举缺口已写进 README 与交付报告。
//
// 原状态不猜：从台账里最后一次「进入 REFUND_REQUESTED」的那行读 from_state。
// 读不到就报 ErrRefundOriginUnknown 停下，绝不默认成 FULFILLED（那是伪造历史）。
//
// 只有 REFUND_REQUESTED 可驳回：REFUND_APPROVED/REFUNDED 意味着钱已经退掉，
// 这时候「驳回」等于把退款事实抹掉，一律拒绝。
func (l *RejectRefundLogic) RejectRefund(in *rpc.RejectRefundReq) (*rpc.RejectRefundReply, error) {
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

	order, err := l.svcCtx.Orders.FindByOrderNo(l.ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, model.ErrOrderNotFound
	}

	switch order.State {
	case model.StateRefundRequested:
		// 正常路径，往下走。
	case model.StateRefundRejected:
		return &rpc.RejectRefundReply{Duplicated: true, Order: toOrderInfo(order)}, nil
	case model.StateRefundApproved, model.StateRefunded:
		return nil, fmt.Errorf("%w: state=%s，款已退不能驳回",
			model.ErrInvalidStateTransition, model.StateName(order.State))
	default:
		// 已经回到 PAID/FULFILLED：只有本次 request_id 的驳回台账命中才算幂等重放。
		if model.IsPaidOrLater(order.State) {
			dup, derr := ledgerDuplicated(l.ctx, l.svcCtx, order.OrderNo, requestID, order.State)
			if derr != nil {
				return nil, derr
			}
			if dup {
				return &rpc.RejectRefundReply{Duplicated: true, Order: toOrderInfo(order)}, nil
			}
		}
		return nil, fmt.Errorf("%w: state=%s，没有待审批的退款申请",
			model.ErrRefundNotRequested, model.StateName(order.State))
	}

	origin, err := l.refundOrigin(order)
	if err != nil {
		return nil, err
	}

	// 契约上没有 expected_version（RejectRefundReq 只有 4 个字段），
	// 所以用刚读到的当前版本做 CAS：并发被抢先时回 ErrConcurrentUpdate 而不是覆盖。
	err = transitionWithEvent(l.ctx, l.svcCtx, order.OrderNo, model.StateRefundRequested, origin,
		order.Version, operator, requestID,
		fmt.Sprintf("refund rejected by %s: %s", operator, sanitize(reason)), nil)
	if err != nil {
		if errors.Is(err, model.ErrConcurrentUpdate) {
			latest, lerr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
			if lerr != nil {
				return nil, lerr
			}
			if latest != nil && latest.State == origin {
				return &rpc.RejectRefundReply{Duplicated: true, Order: toOrderInfo(latest)}, nil
			}
		}
		return nil, err
	}

	final, ferr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
	if ferr != nil {
		return nil, ferr
	}
	l.Infof("trade-order/RejectRefund: order %s refund rejected by %s, back to %s",
		order.OrderNo, operator, model.StateName(origin))
	return &rpc.RejectRefundReply{Duplicated: false, Order: toOrderInfo(final)}, nil
}

// refundOrigin 从台账里取「进入 REFUND_REQUESTED 之前」那个状态。
// 会员单可能是 PAID（履约没成功就申请退款），硬币包通常是 FULFILLED —— 只能回查，不能假设。
func (l *RejectRefundLogic) refundOrigin(order *model.Order) (int32, error) {
	e, err := l.svcCtx.OrderEvents.FindLastTransitionTo(l.ctx, order.OrderNo, model.StateRefundRequested)
	if err != nil {
		return 0, err
	}
	if e == nil || !model.ValidState(e.FromState) {
		return 0, fmt.Errorf("%w: order=%s 台账里没有进入 REFUND_REQUESTED 的记录",
			model.ErrRefundOriginUnknown, order.OrderNo)
	}
	if !model.CanTransition(model.StateRefundRequested, e.FromState) {
		// 台账里的来源状态本身不合法（脏数据）：停下来人工核对，不把错值写回主表。
		return 0, fmt.Errorf("%w: order=%s ledger from_state=%s",
			model.ErrRefundOriginUnknown, order.OrderNo, model.StateName(e.FromState))
	}
	return e.FromState, nil
}
