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

type OrderGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 订单详情（mid 非 0 时服务校验归属，越权按 not-found 回）
func NewOrderGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderGetLogic {
	return &OrderGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderGet 转发 trade-order GetOrder（订单详情，带归属校验）。
//
// found=false 是「这一单不存在，或不属于 mid 所指的用户」的真实读结论，不是错误：
// 服务刻意把两种情况合并成同一个出口（ErrOrderNotFound / found=false），因为错误码差异
// 能让调用方枚举出「哪些订单号真实存在」。网关原样转达这个不区分，绝不把越权改写成
// 「无权限」这种更具体的结论，也不在没有订单时编一个零值单去填满 data.order。
//
// mid=0 是「运营面按单号取单、不做归属校验」的合法哨兵（proto 注释：非 0 时才校验），
// 网关不替它填某个 mid，也不因为「后台总该带归属」就拒绝。负数没有任何对应语义。
//
// 只读路由：不挂 AdminPermission、不发 operator、也不做任何状态推断
// （§5 状态机归 trade-order，网关连「这单看起来能退」都不说）。
func (l *OrderGetLogic) OrderGet(req *types.ParamOrderGet) (resp *types.OrderGetResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errOrderServiceNotConfigured
	}
	if req == nil {
		return nil, errOrderRequestMissing
	}
	if err := requireNonEmpty("order_no", req.OrderNo); err != nil {
		return nil, err
	}
	if err := orderNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.GetOrder(l.ctx, &tradeorderrpc.GetOrderReq{
		OrderNo: req.OrderNo,
		Mid:     req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/admin/orderGet: order_no=%q mid=%d err=%v", req.OrderNo, req.Mid, err)
		return nil, err
	}
	return &types.OrderGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderGetData{
			Found: reply.GetFound(),
			Order: orderToAPI(reply.GetOrder()),
		},
		TTL: 0,
	}, nil
}
