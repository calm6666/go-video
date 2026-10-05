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

type OrderRefundLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 申请退款（进入待审批，不代表已退）
func NewOrderRefundLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderRefundLogic {
	return &OrderRefundLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderRefund 自助提交退款申请：RequestRefund 只把订单推进到「待审批」，钱和权益都还没动，
// 审批（ApproveRefund）是运营面动作，终端不提供。哪张单可退、可退多少由 trade-order 判定，
// 网关不预判也不开旁路，状态不符是服务侧错误，原样上抛。
// 闸门：mid/order_no/request_id/reason 必填——退款是要进变更台账与对账的动作，不能无理由；
// amount_minor 是「0=全额」的可选位，网关不补默认值也不裁剪，原样交给服务侧按已消耗口径折算。
// operator 由网关渲染成自助身份 "user"，request_id 原样透传保证重放不重复申请。
// 结论投影：duplicated=true 命中 request_id 重放，仍是 Code:0（不是错误）。退款会改订单，TTL 0。
func (l *OrderRefundLogic) OrderRefund(req *types.ParamOrderRefundRequest) (resp *types.OrderRefundResponse, err error) {
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
	reply, err := l.svcCtx.TradeOrder.RequestRefund(l.ctx, &tradeorderrpc.RequestRefundReq{
		OrderNo:     req.OrderNo,
		Mid:         req.Mid,
		Operator:    commerceSelfOperator,
		AmountMinor: req.AmountMinor,
		Reason:      req.Reason,
		RequestId:   req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/orderRefund: mid=%d order_no=%s err=%v", req.Mid, req.OrderNo, err)
		return nil, err
	}
	return &types.OrderRefundResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderRefundData{
			Duplicated: reply.GetDuplicated(),
			Order:      orderToAPI(reply.GetOrder()),
		},
		TTL: 0,
	}, nil
}
