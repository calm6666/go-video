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

type OrderEventsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 订单状态流转记录（让用户看到卡在哪一步）
func NewOrderEventsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderEventsLogic {
	return &OrderEventsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderEvents 读订单状态流转台账。
//
// 这里是本包唯一刻意做两次下游读的地方，原因是契约本身有归属缺口：
// ListOrderEventsReq 没有 mid 位，trade-order 侧也明确「不校验归属：本方法是运营/排障入口」
// （services/trade-order/internal/logic/listordereventslogic.go）。终端路由若直接拿客户端
// 传来的 order_no 去查，等于让任何登录用户翻别人的订单轨迹（单号可被枚举），违反 AGENTS.md §5
// 「只读自己那一份数据」。因此先 GetOrder(order_no, mid) 让**归属的唯一定义者** trade-order
// 判定归属，非本人一律 not-found，再决定是否继续读台账。
// 两次都是无副作用的读操作，不存在「写成功但读失败」的错配风险。
//
// found=false（单号不存在或不属于本人）投影成空台账 + Code:0，与 orderDetail 的 not-found
// 口径一致：网关不据此区分「不存在」和「别人的单」，那正是服务侧刻意不区分的东西。
// 台账随履约/退款追加，TTL 0。
func (l *OrderEventsLogic) OrderEvents(req *types.ParamOrderEvents) (resp *types.OrderEventsResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errors.New("trade-order service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("order_no", req.OrderNo); err != nil {
		return nil, err
	}
	owned, err := l.svcCtx.TradeOrder.GetOrder(l.ctx, &tradeorderrpc.GetOrderReq{
		OrderNo: req.OrderNo,
		Mid:     req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/app/orderEvents ownership check: mid=%d order_no=%s err=%v", req.Mid, req.OrderNo, err)
		return nil, err
	}
	if !owned.GetFound() {
		return &types.OrderEventsResponse{
			Code:    0,
			Message: "ok",
			Data: types.OrderEventsData{
				Events:   []types.OrderEvent{},
				Total:    0,
				Page:     int64(req.Page),
				PageSize: int64(req.PageSize),
			},
			TTL: 0,
		}, nil
	}
	reply, err := l.svcCtx.TradeOrder.ListOrderEvents(l.ctx, &tradeorderrpc.ListOrderEventsReq{
		OrderNo: req.OrderNo,
		Page:    int64(req.Page),
		Size:    int64(req.PageSize),
	})
	if err != nil {
		l.Errorf("gateway/app/orderEvents: mid=%d order_no=%s page=%d err=%v", req.Mid, req.OrderNo, req.Page, err)
		return nil, err
	}
	return &types.OrderEventsResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderEventsData{
			Events:   orderEventsToAPI(reply.GetEvents()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
