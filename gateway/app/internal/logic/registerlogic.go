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

type RegisterLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 注册（用户名+密码 或 手机/邮箱+验证码+密码）
func NewRegisterLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegisterLogic {
	return &RegisterLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 注册：聚合 account Register RPC，成功后直接返回登录态。
func (l *RegisterLogic) Register(req *types.ParamRegister) (resp *types.LoginResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Register(l.ctx, &accountrpc.RegisterReq{
		Account:     req.Account,
		Password:    req.Password,
		CaptureCode: req.CaptureCode,
		Ip:          req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/register: account=%s err=%v", req.Account, err)
		return nil, err
	}
	login := reply.GetLogin()
	return &types.LoginResponse{
		Code:    0,
		Message: "ok",
		Data: types.LoginData{
			Mid:          login.GetMid(),
			Token:        login.GetToken(),
			RefreshToken: login.GetRefreshToken(),
			Csrf:         login.GetCsrf(),
			Expires:      login.GetExpires(),
		},
		TTL: 0,
	}, nil
}
