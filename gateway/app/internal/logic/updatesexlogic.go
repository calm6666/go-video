// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateSexLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 更新用户性别
func NewUpdateSexLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateSexLogic {
	return &UpdateSexLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 更新用户性别：聚合 user-profile SetSex RPC。
func (l *UpdateSexLogic) UpdateSex(req *types.ParamSex) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if _, err = l.svcCtx.UserProfile.SetSex(l.ctx, &userprofilerc.UpdateSexReq{Mid: req.Mid, Sex: req.Sex}); err != nil {
		l.Errorf("gateway/app/updateSex: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
