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

type GetPmSettingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询本人反骚扰偏好
func NewGetPmSettingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetPmSettingLogic {
	return &GetPmSettingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetPmSetting 返回本人偏好原文投影；allow_from=0（UNSPECIFIED）表示服务侧按 default 处理，
// 网关不把它改写成 ANYONE，否则客户端会把「未设置」误读成「所有人可发」。
func (l *GetPmSettingLogic) GetPmSetting(req *types.ParamPmSetting) (resp *types.PmUserSettingResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	setting, err := l.svcCtx.PrivateMessage.GetUserSetting(l.ctx, &privatemessagerpc.GetUserSettingReq{
		Mid:     req.Mid,
		TraceId: req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/getPmSetting: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.PmUserSettingResponse{
		Code:    0,
		Message: "ok",
		Data:    types.PmUserSettingData{Setting: pmSettingToAPI(setting)},
		TTL:     0,
	}, nil
}
