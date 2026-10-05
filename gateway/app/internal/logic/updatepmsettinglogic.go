// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdatePmSettingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 更新本人反骚扰偏好
func NewUpdatePmSettingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdatePmSettingLogic {
	return &UpdatePmSettingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpdatePmSetting 用三态编码把「未填」与「显式关闭」区分开，避免部分更新把没传的开关写成 false。
// 偏好取值合法性（allow_from 枚举、门槛是否足以放行某次发送）由服务判定并原样上抛；
// 网关不缓存判定结果，也不预判黑名单/风控——那些依据在 social-graph / risk-control（AGENTS.md §5）。
func (l *UpdatePmSettingLogic) UpdatePmSetting(req *types.ParamPmSettingUpdate) (resp *types.PmUserSettingResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	rejectStranger, err := pmTristate("reject_stranger", req.RejectStranger)
	if err != nil {
		return nil, err
	}
	keywordFilter, err := pmTristate("keyword_filter", req.KeywordFilter)
	if err != nil {
		return nil, err
	}
	muteConversation, err := pmTristate("mute_conversation", req.MuteConversation)
	if err != nil {
		return nil, err
	}
	setting, err := l.svcCtx.PrivateMessage.UpdateUserSetting(l.ctx, &privatemessagerpc.UpdateUserSettingReq{
		Mid:              req.Mid,
		AllowFrom:        privatemessagerpc.AllowFrom(req.AllowFrom),
		RejectStranger:   rejectStranger,
		KeywordFilter:    keywordFilter,
		MuteConversation: muteConversation,
		TraceId:          req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/updatePmSetting: mid=%d allow_from=%d err=%v", req.Mid, req.AllowFrom, err)
		return nil, err
	}
	return &types.PmUserSettingResponse{
		Code:    0,
		Message: "ok",
		Data:    types.PmUserSettingData{Setting: pmSettingToAPI(setting)},
		TTL:     0,
	}, nil
}
