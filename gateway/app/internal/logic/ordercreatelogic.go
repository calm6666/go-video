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

type OrderCreateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 下单（金额由服务侧重算；沙箱渠道下建单即受理并履约）
func NewOrderCreateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderCreateLogic {
	return &OrderCreateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderCreate 建单：amount_minor 是客户端上报值，带下去只为让 trade-order 做一致性校验
// （防前端改价），网关绝不重算、不折扣、不改单位；返回的金额一律取服务侧重算值。
// biz_type / pay_method / platform 枚举位原样透传，取值是否受理由服务侧判定。
// request_id 必填且原样透传——它是建单幂等键，改一个字符就会多下一单。
// accepted=false（余额不足等）与 duplicated=true（重放命中）都是结论，返回 Code:0 并带上单据，
// reject_reason 原文透出，网关不改写文案也不降级成 HTTP 错误。
func (l *OrderCreateLogic) OrderCreate(req *types.ParamOrderCreate) (resp *types.OrderCreateResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errors.New("trade-order service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.CreateOrder(l.ctx, &tradeorderrpc.CreateOrderReq{
		Mid:           req.Mid,
		BizType:       tradeorderrpc.OrderBizType(req.BizType),
		PlanId:        req.PlanId,
		PlanCode:      req.PlanCode,
		Quantity:      req.Quantity, // <=0 由服务按 1 处理并裁剪上限
		PayMethod:     tradeorderrpc.PayMethod(req.PayMethod),
		AmountMinor:   req.AmountMinor,
		RequestId:     req.RequestId,
		Platform:      tradeorderrpc.Platform(req.Platform),
		ClientTraceId: req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/orderCreate: mid=%d biz_type=%d plan_id=%d plan_code=%s amount_minor=%d err=%v",
			req.Mid, req.BizType, req.PlanId, req.PlanCode, req.AmountMinor, err)
		return nil, err
	}
	return &types.OrderCreateResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderCreateData{
			Duplicated:   reply.GetDuplicated(),
			Accepted:     reply.GetAccepted(),
			RejectReason: reply.GetRejectReason(),
			Order:        orderToAPI(reply.GetOrder()),
		},
		TTL: 0,
	}, nil
}
