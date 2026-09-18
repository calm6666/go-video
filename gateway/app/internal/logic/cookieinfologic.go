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

type CookieInfoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询 cookie 会话登录态
func NewCookieInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CookieInfoLogic {
	return &CookieInfoLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询 cookie 会话登录态：聚合 account CookieInfo RPC。
func (l *CookieInfoLogic) CookieInfo(req *types.ParamCookie) (resp *types.SessionResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.CookieInfo(l.ctx, &accountrpc.GetCookieInfoReq{Cookie: req.Cookie})
	if err != nil {
		l.Errorf("gateway/app/cookieInfo: err=%v", err)
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
