package logic

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/svc"
	"go-video/services/notification/rpc"
)

type GetDndPreferenceLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetDndPreferenceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetDndPreferenceLogic {
	return &GetDndPreferenceLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询用户通道偏好与免打扰设置。
// 用户从未设置过时返回 found=false + 默认值（全通道允许、不设时段、时区为服务默认值），
// 让客户端可以直接渲染表单，而不需要区分“没设置”和“设置了但全开”。
func (l *GetDndPreferenceLogic) GetDndPreference(in *rpc.GetDndPreferenceReq) (*rpc.GetDndPreferenceReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil request")
	}
	if in.GetMid() <= 0 {
		return nil, errors.New("notification/logic: mid is required")
	}
	pref, err := l.svcCtx.Repository.DndPref(l.ctx, in.GetMid())
	if err != nil {
		return nil, err
	}
	reply := &rpc.GetDndPreferenceReply{
		Preference: policy.ToDndPreference(pref),
		Found:      pref != nil,
	}
	if pref == nil {
		reply.Preference = &rpc.DndPreference{
			Mid:      in.GetMid(),
			Timezone: l.svcCtx.Config.Notification.DefaultTimezone,
		}
	}
	return reply, nil
}
