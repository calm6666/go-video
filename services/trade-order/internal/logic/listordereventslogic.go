package logic

import (
	"context"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type ListOrderEventsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListOrderEventsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListOrderEventsLogic {
	return &ListOrderEventsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 订单状态流转台账
//
// 台账是订单事实的一部分，只追加不修改：这张表回答「谁在什么时候把订单从 A 推到 B、
// 理由是什么」，运营排障与退款审批都靠它，因此按时间正序返回（重建迁移轨迹）。
// 不校验归属：本方法是运营/排障入口，终端用户订单详情由 GetOrder 承载；
// 台账行里没有 PII（operator/reason 都是写入侧脱敏过的摘要）。
func (l *ListOrderEventsLogic) ListOrderEvents(in *rpc.ListOrderEventsReq) (*rpc.ListOrderEventsReply, error) {
	orderNo := strings.TrimSpace(in.GetOrderNo())
	if orderNo == "" {
		return nil, model.ErrOrderNoRequired
	}
	offset, size, err := paginate(in.GetPage(), in.GetSize(), l.svcCtx.Config.TradeOrder.MaxPageSize)
	if err != nil {
		return nil, err
	}
	rows, total, err := l.svcCtx.OrderEvents.ListByOrderNo(l.ctx, orderNo, offset, size)
	if err != nil {
		return nil, err
	}
	return &rpc.ListOrderEventsReply{
		Events: eventInfos(rows),
		Total:  total,
		Page:   normalizePage(in.GetPage()),
		Size:   size,
	}, nil
}
