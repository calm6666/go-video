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

type RenewTokenLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 刷新 token
func NewRenewTokenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenewTokenLogic {
	return &RenewTokenLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 刷新 token：聚合 account RenewToken RPC。
func (l *RenewTokenLogic) RenewToken(req *types.ParamRenew) (resp *types.RenewResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.RenewToken(l.ctx, &accountrpc.RenewTokenReq{RefreshToken: req.RefreshToken})
	if err != nil {
		l.Errorf("gateway/app/renewToken: err=%v", err)
		return nil, err
	}
	return &types.RenewResponse{
		Code:    0,
		Message: "ok",
		Data:    types.RenewData{Token: reply.GetToken(), Csrf: reply.GetCsrf(), Expires: reply.GetExpires()},
		TTL:     0,
	}, nil
}
