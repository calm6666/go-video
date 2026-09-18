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

type LoginCaptureCheckLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 校验登录验证码
func NewLoginCaptureCheckLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginCaptureCheckLogic {
	return &LoginCaptureCheckLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 校验登录验证码：聚合 account CheckCapture RPC（biz=登录）。
func (l *LoginCaptureCheckLogic) LoginCaptureCheck(req *types.ParamCaptureVerify) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	if _, err = l.svcCtx.Account.CheckCapture(l.ctx, &accountrpc.CheckCaptureReq{
		Biz:         1, // 登录
		Target:      req.Target,
		CaptureCode: req.CaptureCode,
	}); err != nil {
		l.Errorf("gateway/app/loginCaptureCheck: target=%s err=%v", req.Target, err)
		return nil, err
	}
	return emptyResponse(), nil
}
