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

type UpdateFaceLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 更新用户头像
func NewUpdateFaceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateFaceLogic {
	return &UpdateFaceLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 更新用户头像：聚合 user-profile SetFace RPC。
func (l *UpdateFaceLogic) UpdateFace(req *types.ParamFace) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if _, err = l.svcCtx.UserProfile.SetFace(l.ctx, &userprofilerc.UpdateFaceReq{Mid: req.Mid, Face: req.Face}); err != nil {
		l.Errorf("gateway/app/updateFace: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
