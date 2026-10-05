package logic

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type ListMyOrdersLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListMyOrdersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListMyOrdersLogic {
	return &ListMyOrdersLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 我的订单
//
// 读路径固定走 idx_mid_state_created：mid 是必选条件，因此不需要时间窗
// （单用户的订单量有界，且这本来就是终端「我的订单」页）。
// 空列表回非 nil 切片，避免客户端为 null 数组写分支。
func (l *ListMyOrdersLogic) ListMyOrders(in *rpc.ListMyOrdersReq) (*rpc.ListMyOrdersReply, error) {
	if in.GetMid() <= 0 {
		return nil, model.ErrInvalidMid
	}
	offset, size, err := paginate(in.GetPage(), in.GetSize(), l.svcCtx.Config.TradeOrder.MaxPageSize)
	if err != nil {
		return nil, err
	}
	state := int32(in.GetState())
	if state != 0 && !model.ValidState(state) {
		return nil, fmt.Errorf("%w: state=%d", model.ErrInvalidStateTransition, state)
	}
	bizType := int32(in.GetBizType())
	if bizType != 0 && !model.ValidBizType(bizType) {
		return nil, fmt.Errorf("%w: biz_type=%d", model.ErrInvalidBizType, bizType)
	}

	rows, total, err := l.svcCtx.Orders.ListByFilter(l.ctx, &model.OrderFilter{
		Mid:     in.GetMid(),
		State:   state,
		BizType: bizType,
		Offset:  offset,
		Limit:   size,
	})
	if err != nil {
		return nil, err
	}
	return &rpc.ListMyOrdersReply{
		Orders: orderInfos(rows),
		Total:  total,
		Page:   normalizePage(in.GetPage()),
		Size:   size,
	}, nil
}

// normalizePage 回显实际生效的页码（0 被当成第 1 页）。
func normalizePage(page int64) int64 {
	if page <= 0 {
		return 1
	}
	return page
}
