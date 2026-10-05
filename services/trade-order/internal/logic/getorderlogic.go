package logic

import (
	"context"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type GetOrderLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetOrderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetOrderLogic {
	return &GetOrderLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 订单详情（带归属校验）
//
// 带 mid 时校验归属，非本人一律回 found=false 而不是 FORBIDDEN：
// 错误码差异会让调用方能枚举出「哪些订单号真实存在」。
func (l *GetOrderLogic) GetOrder(in *rpc.GetOrderReq) (*rpc.GetOrderReply, error) {
	orderNo := strings.TrimSpace(in.GetOrderNo())
	if orderNo == "" {
		return nil, model.ErrOrderNoRequired
	}
	order, err := l.svcCtx.Orders.FindByOrderNo(l.ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return &rpc.GetOrderReply{Found: false}, nil
	}
	if mid := in.GetMid(); mid > 0 && order.Mid != mid {
		l.Errorf("trade-order/GetOrder: order %s belongs to another mid, answered not-found", orderNo)
		return &rpc.GetOrderReply{Found: false}, nil
	}
	return &rpc.GetOrderReply{Found: true, Order: toOrderInfo(order)}, nil
}
