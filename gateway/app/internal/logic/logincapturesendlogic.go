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

type LoginCaptureSendLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 发送登录验证码
func NewLoginCaptureSendLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginCaptureSendLogic {
	return &LoginCaptureSendLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 发送登录验证码：聚合 account SendCapture RPC（biz=登录）。
func (l *LoginCaptureSendLogic) LoginCaptureSend(req *types.ParamCaptureSend) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	if _, err = l.svcCtx.Account.SendCapture(l.ctx, &accountrpc.SendCaptureReq{
		Biz:    1, // 登录
		Target: req.Target,
		Ip:     req.IP,
	}); err != nil {
		l.Errorf("gateway/app/loginCaptureSend: target=%s err=%v", req.Target, err)
		return nil, err
	}
	return emptyResponse(), nil
}
