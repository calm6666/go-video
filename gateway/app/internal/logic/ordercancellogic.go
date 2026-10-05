// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	tradeorderrpc "go-video/services/trade-order/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OrderCancelLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消未支付订单
func NewOrderCancelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderCancelLogic {
	return &OrderCancelLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderCancel 取消未支付订单。哪条状态迁移合法（已支付必须走退款而不是取消）由 trade-order
// 的状态机判定，网关不预判也不开旁路；状态不符是服务侧错误，原样上抛。
// 契约把 reason 定为必填，因此空串在网关拒绝——取消是要进变更台账的动作，不能无理由。
// operator 由网关渲染成自助身份 "user"，mid 用于归属校验；request_id 原样透传保证重放不重复取消。
func (l *OrderCancelLogic) OrderCancel(req *types.ParamOrderCancel) (resp *types.OrderCancelResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errors.New("trade-order service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("order_no", req.OrderNo); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	if err := requireText("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.CancelOrder(l.ctx, &tradeorderrpc.CancelOrderReq{
		OrderNo:   req.OrderNo,
		Mid:       req.Mid,
		Operator:  commerceSelfOperator,
		RequestId: req.RequestId,
		Reason:    req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/app/orderCancel: mid=%d order_no=%s err=%v", req.Mid, req.OrderNo, err)
		return nil, err
	}
	return &types.OrderCancelResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderCancelData{
			Duplicated: reply.GetDuplicated(),
			Order:      orderToAPI(reply.GetOrder()),
		},
		TTL: 0,
	}, nil
}
