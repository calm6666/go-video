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

type OrderDetailLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 订单详情（强制按 mid 校验归属）
func NewOrderDetailLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderDetailLogic {
	return &OrderDetailLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderDetail 强制带 mid 读取：归属由 trade-order 二次校验，越权与不存在都按 not-found
// 语义返回（服务侧刻意不返回 FORBIDDEN，免得暴露单号是否存在）。
// 契约缺口：types.OrderDetailResponse 没有 found 位，所以 found=false 只能投影成
// order_no 为空串的零值 order + Code:0（不是 HTTP 错误），端上按 data.order_no=="" 判定无此单。
// 订单状态随时被履约推进，TTL 0。
func (l *OrderDetailLogic) OrderDetail(req *types.ParamOrderDetail) (resp *types.OrderDetailResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errors.New("trade-order service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("order_no", req.OrderNo); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.GetOrder(l.ctx, &tradeorderrpc.GetOrderReq{
		OrderNo: req.OrderNo,
		Mid:     req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/app/orderDetail: mid=%d order_no=%s err=%v", req.Mid, req.OrderNo, err)
		return nil, err
	}
	return &types.OrderDetailResponse{
		Code:    0,
		Message: "ok",
		Data:    orderToAPI(reply.GetOrder()),
		TTL:     0,
	}, nil
}
