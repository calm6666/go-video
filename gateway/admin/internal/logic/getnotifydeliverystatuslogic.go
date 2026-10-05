// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	notificationrpc "go-video/services/notification/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetNotifyDeliveryStatusLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询单条投递记录与供应商回执
func NewGetNotifyDeliveryStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetNotifyDeliveryStatusLogic {
	return &GetNotifyDeliveryStatusLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 单条投递状态：聚合 notification GetDeliveryStatus RPC。
// found=false 是合法结果（任务不存在）而不是错误：后台要能区分「没这条任务」和「任务还没投出去」，
// 此时 delivery 投影成零值，网关不伪造 ID 也不补投递结论。
func (l *GetNotifyDeliveryStatusLogic) GetNotifyDeliveryStatus(req *types.ParamNotifyDeliveryStatus) (resp *types.NotifyDeliveryResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	if err := requireNonEmpty("delivery_id", req.DeliveryId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Notification.GetDeliveryStatus(l.ctx, &notificationrpc.GetDeliveryStatusReq{
		DeliveryId: req.DeliveryId,
	})
	if err != nil {
		l.Errorf("gateway/admin/getNotifyDeliveryStatus: delivery_id=%s err=%v", req.DeliveryId, err)
		return nil, err
	}
	return &types.NotifyDeliveryResponse{
		Code:    0,
		Message: "ok",
		Data: types.NotifyDeliveryData{
			Delivery: notifyDeliveryToAPI(reply.GetDelivery()),
			Found:    reply.GetFound(),
		},
		TTL: 0,
	}, nil
}
