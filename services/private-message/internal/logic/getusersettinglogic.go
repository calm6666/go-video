package logic

import (
	"context"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetUserSettingLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetUserSettingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUserSettingLogic {
	return &GetUserSettingLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询反骚扰偏好。
//
// 实现要点：
//  1. 只读本人视角：调用者身份由 gateway 注入 mid，服务侧只认这个 mid（越权在网关拦截）；
//  2. 缺行不落库：按站点级缺省合成一份返回，mtime=0 表示「从未设置过」，
//     客户端据此区分「未设置」与「显式设置成默认」，读路径不放大写；
//  3. 枚举映射：AllowFrom 归一到明确的 model.AllowFrom* 取值再回传，
//     库里若存在历史脏值（0/越界）也按站点级默认回灌，不把 UNSPECIFIED 透给客户端。
func (l *GetUserSettingLogic) GetUserSetting(in *rpc.GetUserSettingReq) (*rpc.UserSettingInfo, error) {
	mid := in.GetMid()
	if err := checkMid(mid); err != nil {
		return nil, err
	}
	row, err := loadSettingOrDefault(l.ctx, l.svcCtx, mid)
	if err != nil {
		return nil, err
	}
	normalizeSetting(l.svcCtx, row)
	return settingInfo(row), nil
}
