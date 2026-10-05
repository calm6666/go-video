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

type OrderListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的订单列表
func NewOrderListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderListLogic {
	return &OrderListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderList 只读本人订单（ListMyOrders 语义）：mid 必填且为正，运营面的 ListOrders
// 跨用户查询不在终端路由出现。state / biz_type 枚举位原样透传（0 不过滤），
// 网关不做取值白名单；page/page_size 原样透传。订单会被履约与退款推进，TTL 0。
func (l *OrderListLogic) OrderList(req *types.ParamOrderList) (resp *types.OrderListResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errors.New("trade-order service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.ListMyOrders(l.ctx, &tradeorderrpc.ListMyOrdersReq{
		Mid:     req.Mid,
		State:   tradeorderrpc.OrderState(req.State),
		BizType: tradeorderrpc.OrderBizType(req.BizType),
		Page:    int64(req.Page),
		Size:    int64(req.PageSize),
	})
	if err != nil {
		l.Errorf("gateway/app/orderList: mid=%d state=%d biz_type=%d page=%d err=%v", req.Mid, req.State, req.BizType, req.Page, err)
		return nil, err
	}
	return &types.OrderListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderListData{
			Orders:   ordersToAPI(reply.GetOrders()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
