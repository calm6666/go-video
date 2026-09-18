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

type LoginLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 登录（密码/验证码，login_type 区分）
func NewLoginLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginLogic {
	return &LoginLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 登录（密码/验证码）：按 login_type 聚合 account 的 PasswordLogin/CaptureLogin RPC。
func (l *LoginLogic) Login(req *types.ParamLogin) (resp *types.LoginResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	in := &accountrpc.LoginReq{
		Account:     req.Account,
		Password:    req.Password,
		LoginType:   req.LoginType,
		CaptureCode: req.CaptureCode,
		Ip:          req.IP,
		Device:      req.Device,
		Buvid:       req.Buvid,
	}
	var reply *accountrpc.LoginReply
	switch req.LoginType {
	case 2:
		reply, err = l.svcCtx.Account.CaptureLogin(l.ctx, in)
	default:
		reply, err = l.svcCtx.Account.PasswordLogin(l.ctx, in)
	}
	if err != nil {
		l.Errorf("gateway/app/login: account=%s type=%d err=%v", req.Account, req.LoginType, err)
		return nil, err
	}
	return &types.LoginResponse{
		Code:    0,
		Message: "ok",
		Data: types.LoginData{
			Mid:          reply.GetMid(),
			Token:        reply.GetToken(),
			RefreshToken: reply.GetRefreshToken(),
			Csrf:         reply.GetCsrf(),
			Expires:      reply.GetExpires(),
		},
		TTL: 0,
	}, nil
}
