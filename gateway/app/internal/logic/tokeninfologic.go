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

type TokenInfoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询 token 登录态
func NewTokenInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TokenInfoLogic {
	return &TokenInfoLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询 token 登录态：聚合 account TokenInfo RPC。
func (l *TokenInfoLogic) TokenInfo(req *types.ParamToken) (resp *types.SessionResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.TokenInfo(l.ctx, &accountrpc.GetTokenInfoReq{Token: req.Token})
	if err != nil {
		l.Errorf("gateway/app/tokenInfo: err=%v", err)
		return nil, err
	}
	return &types.SessionResponse{
		Code:    0,
		Message: "ok",
		Data: types.SessionData{
			IsLogin: reply.GetIsLogin(),
			Mid:     reply.GetMid(),
			Csrf:    reply.GetCsrf(),
			Expires: reply.GetExpires(),
		},
		TTL: 0,
	}, nil
}
