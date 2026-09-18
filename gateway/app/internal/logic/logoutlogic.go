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

type LogoutLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 登出
func NewLogoutLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LogoutLogic {
	return &LogoutLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 登出：聚合 account Logout RPC（吊销 token）。
func (l *LogoutLogic) Logout(req *types.ParamToken) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	if _, err = l.svcCtx.Account.Logout(l.ctx, &accountrpc.LogoutReq{Token: req.Token}); err != nil {
		l.Errorf("gateway/app/logout: err=%v", err)
		return nil, err
	}
	return emptyResponse(), nil
}
