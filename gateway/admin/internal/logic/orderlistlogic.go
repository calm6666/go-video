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

type OrderListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 订单分页（有界窗口；mid=0 且无过滤条件时由服务拒绝全表扫）
func NewOrderListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderListLogic {
	return &OrderListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderList 转发 trade-order ListOrders（运营面订单检索）。
//
// 只读路由，不挂 AdminPermission（见 admin.api 的 trade-order 只读面注释），因此这里
// 没有 operator 可言：网关一侧不发任何写、也不看会话。
//
// 网关只挡形状（非负 + 窗口不倒着给），有界性一条都不接管（§5 订单事实归 trade-order）：
//   - 「mid=0 且没有过滤条件」→ ErrFilterRequired、「只有状态过滤没有时间窗」→
//     ErrListWindowRequired、「窗口超过 MaxListWindowSeconds」→ ErrListWindowTooLarge，
//     三条都依赖服务侧的配置与「哪个点位能替代时间窗」的判断，网关自己复算只会漂移；
//   - max_window_seconds=0 是「用服务默认窗口」而不是「不限窗口」，原样下传；
//     它只能收紧窗口，这条规则也在服务里；
//   - state/biz_type/pay_method 的 0 是「不按该位过滤」，非 0 是否是本域已定义枚举由服务
//     校验（ValidState/ValidBizType/ValidPayMethod），网关不替它挑一个值；
//   - size 越上限是**拒绝**而不是裁剪（paginate 注释写明「裁剪会让调用方误以为就这么多」），
//     所以网关更不能自己夹一刀。
//
// total/page/size 照抄服务回显。空列表投影成 []，但「查不到」在这里本来就不是错误：
// 服务真失败时回的是 error，那一路原样上抛，绝不折叠成空列表冒充成功。
func (l *OrderListLogic) OrderList(req *types.ParamOrderList) (resp *types.OrderListResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errOrderServiceNotConfigured
	}
	if req == nil {
		return nil, errOrderRequestMissing
	}
	if err := orderNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := orderNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := orderNonNeg("biz_type", int64(req.BizType)); err != nil {
		return nil, err
	}
	if err := orderNonNeg("pay_method", int64(req.PayMethod)); err != nil {
		return nil, err
	}
	if err := orderTimeWindow(req.FromTs, req.ToTs); err != nil {
		return nil, err
	}
	if err := orderNonNeg("max_window_seconds", req.MaxWindowSeconds); err != nil {
		return nil, err
	}
	if err := orderNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := orderNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.ListOrders(l.ctx, &tradeorderrpc.ListOrdersReq{
		Mid:              req.Mid,
		State:            tradeorderrpc.OrderState(req.State),
		BizType:          tradeorderrpc.OrderBizType(req.BizType),
		PayMethod:        tradeorderrpc.PayMethod(req.PayMethod),
		OrderNo:          req.OrderNo,
		PaymentNo:        req.PaymentNo,
		FromTs:           req.FromTs,
		ToTs:             req.ToTs,
		Page:             req.Page,
		Size:             req.Size,
		MaxWindowSeconds: req.MaxWindowSeconds,
	})
	if err != nil {
		l.Errorf("gateway/admin/orderList: mid=%d state=%d biz_type=%d pay_method=%d order_no=%q payment_no=%q from_ts=%d to_ts=%d page=%d size=%d err=%v",
			req.Mid, req.State, req.BizType, req.PayMethod, req.OrderNo, req.PaymentNo,
			req.FromTs, req.ToTs, req.Page, req.Size, err)
		return nil, err
	}
	return &types.OrderListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderListData{
			List:  ordersToAPI(reply.GetOrders()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
