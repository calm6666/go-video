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

type OrderEventListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 状态流转台账（每次迁移一行，含操作者与理由）
func NewOrderEventListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderEventListLogic {
	return &OrderEventListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderEventList 转发 trade-order ListOrderEvents（状态流转台账）。
//
// 这条路由是「谁在什么时候凭什么把单推到哪一步」的证据链本体（admin.api 只读面注释
// 点名它重要），所以它的价值全在**一位不丢地转达**：operator、reason、from_state/to_state、
// ctime 全部原样投影，网关不折叠同态行（REFUND_APPROVED→REFUND_APPROVED 是「款已退、
// 权益未回收」的留证，看着像重复但它不是）、不按状态对台账做二次解读。
//
// 排序口径在服务（按时间正序，便于重建轨迹），网关不重排也不 reverse。
// 台账「不属于本单」的归属校验刻意不存在（服务注释写明它是运营/排障入口，且行内无 PII），
// 网关也不补一条：那是伪造服务没有的规则。
//
// 网关只挡 order_no 必填与 page/size 非负；页大小上限（MaxPageSize，越限拒绝而非裁剪）
// 与「page<1」的归一全在服务。total/page/size 照抄回显。
func (l *OrderEventListLogic) OrderEventList(req *types.ParamOrderEventList) (resp *types.OrderEventListResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errOrderServiceNotConfigured
	}
	if req == nil {
		return nil, errOrderRequestMissing
	}
	if err := requireNonEmpty("order_no", req.OrderNo); err != nil {
		return nil, err
	}
	if err := orderNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := orderNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.ListOrderEvents(l.ctx, &tradeorderrpc.ListOrderEventsReq{
		OrderNo: req.OrderNo,
		Page:    req.Page,
		Size:    req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/orderEventList: order_no=%q page=%d size=%d err=%v",
			req.OrderNo, req.Page, req.Size, err)
		return nil, err
	}
	return &types.OrderEventListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderEventListData{
			List:  orderEventsToAPI(reply.GetEvents()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
