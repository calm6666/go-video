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

type UpdateUnameLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 更新用户昵称
func NewUpdateUnameLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateUnameLogic {
	return &UpdateUnameLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 更新用户昵称：聚合 user-profile SetName RPC。
func (l *UpdateUnameLogic) UpdateUname(req *types.ParamUname) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if req.Name == "" {
		return nil, errors.New("name is required")
	}
	if _, err = l.svcCtx.UserProfile.SetName(l.ctx, &userprofilerc.UpdateUnameReq{Mid: req.Mid, Name: req.Name}); err != nil {
		l.Errorf("gateway/app/updateUname: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
