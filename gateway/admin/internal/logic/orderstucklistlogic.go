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

type OrderStuckListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 卡单扫描（只读：核对哪些单停在 PAYING/PAID/FULFILLING 超时，后台不代为推进）
func NewOrderStuckListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderStuckListLogic {
	return &OrderStuckListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderStuckList 转发 trade-order ListStuckOrders（cron 的卡单扫描面，后台只读它做核对）。
//
// **本路由不提供任何补偿动作**（§5 订单状态机与履约指令只属于 trade-order）：扫出来的单
// 由调用方自己投递给 FulfillOrder/BindPayment/ApproveRefund，那些写入口各自带幂等键与限频。
// 网关在这里既不调那些方法，也不「顺手把看起来能修的单推一把」。
//
// 网关只挡形状：older_than_seconds/limit 非负、states 里没有负数编号。
// 判定一个都不接管：
//   - older_than_seconds<=0 会退化用服务侧 OrderExpireSeconds（不是全表扫），所以 0 是
//     合法哨兵，网关不代填数字、也不把它当「立即过期」；
//   - states 为空由服务使用默认集合 PAYING/PAID/FULFILLING；显式传入时「是不是已定义状态」
//     「是不是还会卡住的非终态」（stuckAllowedStates）全由服务判定并回
//     ErrStuckStateNotAllowed —— 网关不预先剔除已结案状态（静默裁剪会让运营以为扫全了）；
//   - limit 越 StuckScanMaxLimit 是**拒绝**而不是裁剪（裁剪会让巡检漏单），网关不夹。
//
// 契约缺口（只报不改）：ListStuckOrdersReq/Reply 两侧都**没有分页**——req 只有 limit，
// reply 只有 orders。因此 OrderStuckListData 也只有 list 一位，网关不伪造 page/size/total，
// 也不在本地截断成「前 N 条」。一次扫描最多 StuckScanMaxLimit 条、超出部分如何翻页续扫，
// 目前契约里无解（已上报，需改 proto 才有 total/游标）。
func (l *OrderStuckListLogic) OrderStuckList(req *types.ParamOrderStuckList) (resp *types.OrderStuckListResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errOrderServiceNotConfigured
	}
	if req == nil {
		return nil, errOrderRequestMissing
	}
	if err := orderNonNeg("older_than_seconds", req.OlderThanSeconds); err != nil {
		return nil, err
	}
	if err := orderStatesNonNeg(req.States); err != nil {
		return nil, err
	}
	if err := orderNonNeg("limit", req.Limit); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.ListStuckOrders(l.ctx, &tradeorderrpc.ListStuckOrdersReq{
		OlderThanSeconds: req.OlderThanSeconds,
		States:           orderStatesToRPC(req.States),
		Limit:            req.Limit,
	})
	if err != nil {
		l.Errorf("gateway/admin/orderStuckList: older_than_seconds=%d states=%v limit=%d err=%v",
			req.OlderThanSeconds, req.States, req.Limit, err)
		return nil, err
	}
	// 只透出服务给的这一批：matched=0 是「这一轮没有卡单」的真实结论，
	// 但下游报错时走上面那条 return，不会被渲染成空列表。
	l.Infof("gateway/admin/orderStuckList: older_than_seconds=%d limit=%d matched=%d",
		req.OlderThanSeconds, req.Limit, len(reply.GetOrders()))
	return &types.OrderStuckListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderStuckListData{
			List: ordersToAPI(reply.GetOrders()),
		},
		TTL: 0,
	}, nil
}
