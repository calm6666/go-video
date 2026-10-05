package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/svc"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

type UpdateDndPreferenceLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateDndPreferenceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateDndPreferenceLogic {
	return &UpdateDndPreferenceLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 更新用户通道偏好与免打扰设置。
//
// 全量覆盖语义（mid 主键 + Upsert）：重复提交同一请求结果一致，天然幂等。
// muted_channels 是“用户显式关闭的通道”，属于硬偏好，投递时任何优先级都会被拦截；
// quiet_start/quiet_end 只是延后发送时段，由投递调度器顺延（不丢弃任务）。
func (l *UpdateDndPreferenceLogic) UpdateDndPreference(in *rpc.UpdateDndPreferenceReq) (*rpc.UpdateDndPreferenceReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil request")
	}
	if in.GetMid() <= 0 {
		return nil, errors.New("notification/logic: mid is required")
	}
	codes, err := channelsToInt32(in.GetMutedChannels())
	if err != nil {
		return nil, err
	}
	start := strings.TrimSpace(in.GetQuietStart())
	end := strings.TrimSpace(in.GetQuietEnd())
	if (start == "") != (end == "") {
		return nil, ErrQuietTimePair
	}
	if _, err := policy.ParseClock(start); err != nil && start != "" {
		return nil, err
	}
	if _, err := policy.ParseClock(end); err != nil && end != "" {
		return nil, err
	}
	timezone := strings.TrimSpace(in.GetTimezone())
	if timezone == "" {
		timezone = l.svcCtx.Config.Notification.DefaultTimezone
	}
	loc, err := policy.LoadLocation(timezone, "")
	if err != nil {
		return nil, fmt.Errorf("%w: %q", err, timezone)
	}
	state := model.DndStateOff
	if in.GetEnabled() {
		state = model.DndStateOn
	}
	repo := l.svcCtx.Repository
	if err := repo.SaveDndPref(l.ctx, &model.NotificationDndPref{
		Mid:           in.GetMid(),
		MutedChannels: model.MaskOfChannels(codes),
		QuietStart:    start,
		QuietEnd:      end,
		Timezone:      loc.String(),
		State:         state,
	}); err != nil {
		return nil, err
	}
	// 读回落库结果再返回，避免把“请求里的值”当成“已生效的值”回给调用方。
	saved, err := repo.DndPref(l.ctx, in.GetMid())
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, fmt.Errorf("%w: dnd pref mid=%d", model.ErrNotFound, in.GetMid())
	}
	return &rpc.UpdateDndPreferenceReply{Preference: policy.ToDndPreference(saved)}, nil
}
