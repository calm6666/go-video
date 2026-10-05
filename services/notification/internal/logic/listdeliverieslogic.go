package logic

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/svc"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

type ListDeliveriesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListDeliveriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListDeliveriesLogic {
	return &ListDeliveriesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询投递记录（按创建时间倒序）。
// 过滤条件全部可选；state 传 DELIVERY_STATE_UNSPECIFIED 表示不过滤。
func (l *ListDeliveriesLogic) ListDeliveries(in *rpc.ListDeliveriesReq) (*rpc.ListDeliveriesReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil request")
	}
	pn, ps := normalizePage(in.GetPn(), in.GetPs())
	rows, total, err := l.svcCtx.Repository.ListDeliveries(l.ctx, model.DeliveryFilter{
		Mid:        in.GetMid(),
		Channel:    int32(in.GetChannel()),
		State:      policy.DeliveryStateCodeOf(in.GetState()),
		BizKey:     in.GetBizKey(),
		StartCtime: in.GetStartCtime(),
		EndCtime:   in.GetEndCtime(),
	}, pn, ps)
	if err != nil {
		return nil, err
	}
	return &rpc.ListDeliveriesReply{Deliveries: policy.ToDeliveryInfos(rows), Total: total}, nil
}
