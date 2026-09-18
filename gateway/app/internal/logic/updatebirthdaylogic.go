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

type UpdateBirthdayLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 更新用户生日
func NewUpdateBirthdayLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateBirthdayLogic {
	return &UpdateBirthdayLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 更新用户生日：聚合 user-profile SetBirthday RPC。
func (l *UpdateBirthdayLogic) UpdateBirthday(req *types.ParamBirthday) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if _, err = l.svcCtx.UserProfile.SetBirthday(l.ctx, &userprofilerc.UpdateBirthdayReq{Mid: req.Mid, Birthday: req.Birthday}); err != nil {
		l.Errorf("gateway/app/updateBirthday: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
