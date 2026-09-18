package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CaptureLoginLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCaptureLoginLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CaptureLoginLogic {
	return &CaptureLoginLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 验证码登录。
// 参考 sms 验证码 + passport-login：手机 + 验证码校验成功后签发会话。
func (l *CaptureLoginLogic) CaptureLogin(in *rpc.LoginReq) (*rpc.LoginReply, error) {
	reply, err := l.svcCtx.Repository.CaptureLogin(l.ctx, in)
	if err != nil {
		l.Errorf("account/CaptureLogin: account=%s err=%v", in.Account, err)
		return nil, err
	}
	return reply, nil
}
