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

type UpdateSignLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 更新用户签名
func NewUpdateSignLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateSignLogic {
	return &UpdateSignLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 更新用户签名：聚合 user-profile SetSign RPC。
func (l *UpdateSignLogic) UpdateSign(req *types.ParamUserSign) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if _, err = l.svcCtx.UserProfile.SetSign(l.ctx, &userprofilerc.UpdateSignReq{Mid: req.Mid, Sign: req.UserSign}); err != nil {
		l.Errorf("gateway/app/updateSign: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
