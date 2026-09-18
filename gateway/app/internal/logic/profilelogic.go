// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	accountrpc "go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ProfileLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询用户完整资料
func NewProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ProfileLogic {
	return &ProfileLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询用户完整资料：聚合 account Profile3 RPC。
func (l *ProfileLogic) Profile(req *types.ParamMid) (resp *types.ProfileResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Profile3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/profile: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.ProfileResponse{
		Code:    0,
		Message: "ok",
		Data:    types.ProfileData{Profile: toProfile(reply.GetProfile())},
		TTL:     0,
	}, nil
}
