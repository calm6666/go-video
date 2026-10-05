// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	notificationrpc "go-video/services/notification/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetDndPreferenceLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询本人通道偏好与免打扰设置
func NewGetDndPreferenceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetDndPreferenceLogic {
	return &GetDndPreferenceLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetDndPreference 聚合 notification GetDndPreference RPC：本人通道偏好与免打扰设置。
// 从未设置过时 notification 返回默认值且 found=false，网关不代为补默认时段。
func (l *GetDndPreferenceLogic) GetDndPreference(req *types.ParamGetDnd) (resp *types.NotifyDndResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	reply, err := l.svcCtx.Notification.GetDndPreference(l.ctx, &notificationrpc.GetDndPreferenceReq{
		Mid: req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/app/getDndPreference: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.NotifyDndResponse{
		Code:    0,
		Message: "ok",
		Data: types.NotifyDndData{
			Preference: dndPreferenceToAPI(reply.GetPreference()),
			Found:      reply.GetFound(),
		},
		TTL: 0,
	}, nil
}
