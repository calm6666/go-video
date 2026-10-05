// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	tradeorderrpc "go-video/services/trade-order/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OrderRefundRejectLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 驳回退款（订单回原状态，不动钱不动权益；reason 必填）
func NewOrderRefundRejectLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderRefundRejectLogic {
	return &OrderRefundRejectLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderRefundReject 转发 trade-order RejectRefund（驳回退款申请）。
//
// 与 approve 刻意分成两个权限点：能驳回的人不因此获得「能退款」的能力（admin.api 写面注释）。
// 驳回不动钱、不动权益，但它直接否掉用户的申退诉求，因此 reason 与 operator 同样是必填位。
//
// 网关只挡主体与必填，不复算这条链路上的任何判定（§5）：
//   - 「现在还有没有待审批的申请」→ 服务的 ErrRefundNotRequested；
//     「款已退（REFUND_APPROVED/REFUNDED）不能驳回」→ 服务的 ErrInvalidStateTransition；
//   - 「原状态是哪个」由服务从台账最后一次「进入 REFUND_REQUESTED」那行的 from_state 回查，
//     查不到就报 ErrRefundOriginUnknown 停下——网关绝不默认成 FULFILLED 去「补一个结论」，
//     也不因为 reply.order 里带着 state 就自己去推断这次驳回把订单推去了哪：
//     **回读 order.state 就是唯一真相**（注意它通常是 FULFILLED/PAID，
//     而 ORDER_STATE_REFUND_REJECTED(11) 在服务侧是条没有出边的枚举，已作为契约缺口上报）；
//   - 并发保护：RejectRefundReq 契约里**没有 expected_version 位**（与 ApproveRefund 不同），
//     服务用刚读到的当前版本做 CAS，未命中回 ErrConcurrentUpdate 原样上抛。
//     网关不因为「少了 CAS」就自己先去查一次订单再补版本——那一读一写之间版本早就可能变了，
//     补上去反而把「乐观锁冲突」伪装成「这次一定成功」。
//
// duplicated=true（同 request_id 命中台账、或订单已回到原状态）是**成功结论**并带首次结果，
// 网关不折叠成错误也不重复提交。
func (l *OrderRefundRejectLogic) OrderRefundReject(req *types.ParamOrderRefundReject) (resp *types.OrderRefundRejectResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errOrderServiceNotConfigured
	}
	if req == nil {
		return nil, errOrderRequestMissing
	}
	operator, err := orderOperator(l.ctx, "orderRefundReject", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("order_no", req.OrderNo); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.RejectRefund(l.ctx, &tradeorderrpc.RejectRefundReq{
		OrderNo:   req.OrderNo,
		Operator:  operator,
		RequestId: req.IdempotencyKey,
		Reason:    req.Reason,
	})
	if err != nil {
		// trace_id 只进日志（RejectRefundReq 没有该字段可下传）。
		l.Errorf("gateway/admin/orderRefundReject: order_no=%q operator=%s trace_id=%s err=%v",
			req.OrderNo, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/orderRefundReject: order_no=%q duplicated=%t state=%d operator=%s",
		req.OrderNo, reply.GetDuplicated(), int32(reply.GetOrder().GetState()), operator)
	return &types.OrderRefundRejectResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderRefundRejectData{
			Duplicated: reply.GetDuplicated(),
			Order:      orderToAPI(reply.GetOrder()),
		},
		TTL: 0,
	}, nil
}
