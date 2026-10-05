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

type ResetPasswordLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 验证码校验后找回密码
func NewResetPasswordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ResetPasswordLogic {
	return &ResetPasswordLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ResetPassword 聚合 account ResetPassword RPC：验证码校验与 RSA 密文解密都在 account 完成；
// 找回入口是匿名可调的，日志只留请求来源，不打账号与口令。
func (l *ResetPasswordLogic) ResetPassword(req *types.ParamResetPassword) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	if _, err = l.svcCtx.Account.ResetPassword(l.ctx, &accountrpc.ResetPasswordReq{
		Account:     req.Account,
		CaptureCode: req.CaptureCode,
		NewPassword: req.NewPassword,
		Ip:          req.IP,
	}); err != nil {
		l.Errorf("gateway/app/resetPassword: ip=%s err=%v", req.IP, err)
		return nil, err
	}
	return emptyResponse(), nil
}
