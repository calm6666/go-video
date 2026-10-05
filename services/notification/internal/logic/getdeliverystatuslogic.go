package logic

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/svc"
	"go-video/services/notification/rpc"
)

type GetDeliveryStatusLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetDeliveryStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetDeliveryStatusLogic {
	return &GetDeliveryStatusLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单条投递记录（含供应商回执）。
// 未找到时返回 found=false 而不是报错：调用方需要区分“任务不存在”和“任务还没投出去”。
func (l *GetDeliveryStatusLogic) GetDeliveryStatus(in *rpc.GetDeliveryStatusReq) (*rpc.GetDeliveryStatusReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil request")
	}
	if in.GetDeliveryId() == "" {
		return nil, errors.New("notification/logic: delivery_id is required")
	}
	row, err := l.svcCtx.Repository.FindDelivery(l.ctx, in.GetDeliveryId())
	if err != nil {
		return nil, err
	}
	if row == nil {
		return &rpc.GetDeliveryStatusReply{Found: false}, nil
	}
	return &rpc.GetDeliveryStatusReply{Delivery: policy.ToDeliveryInfo(row), Found: true}, nil
}
