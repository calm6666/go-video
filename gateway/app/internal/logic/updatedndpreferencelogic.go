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

type UpdateDndPreferenceLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 更新本人通道偏好与免打扰设置（全量覆盖）
func NewUpdateDndPreferenceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateDndPreferenceLogic {
	return &UpdateDndPreferenceLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpdateDndPreference 聚合 notification UpdateDndPreference RPC：全量覆盖本人免打扰设置。
// muted_channels 为空即表示所有通道允许；HH:MM 与时区合法性由 notification 服务判定。
// UpdateDndPreferenceReply 只有 preference 字段，写入成功后设置必然存在，故 found 回 true。
func (l *UpdateDndPreferenceLogic) UpdateDndPreference(req *types.ParamUpdateDnd) (resp *types.NotifyDndResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	reply, err := l.svcCtx.Notification.UpdateDndPreference(l.ctx, &notificationrpc.UpdateDndPreferenceReq{
		Mid:           req.Mid,
		MutedChannels: dndChannelsFromParam(req.MutedChannels),
		QuietStart:    req.QuietStart,
		QuietEnd:      req.QuietEnd,
		Timezone:      req.Timezone,
		Enabled:       req.Enabled,
	})
	if err != nil {
		l.Errorf("gateway/app/updateDndPreference: mid=%d enabled=%t muted=%d err=%v",
			req.Mid, req.Enabled, len(req.MutedChannels), err)
		return nil, err
	}
	return &types.NotifyDndResponse{
		Code:    0,
		Message: "ok",
		Data: types.NotifyDndData{
			Preference: dndPreferenceToAPI(reply.GetPreference()),
			Found:      true,
		},
		TTL: 0,
	}, nil
}
