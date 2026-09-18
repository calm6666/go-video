package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SendCaptureLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSendCaptureLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SendCaptureLogic {
	return &SendCaptureLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 发送登录/注册/找回验证码。
// 参考 sms 服务的账号侧验证码能力：验证码入 Redis（10 分钟有效），
// 发送记录落库；短信下发由 notification 服务承接（未接入前为开发降级日志）。
func (l *SendCaptureLogic) SendCapture(in *rpc.SendCaptureReq) (*rpc.DelCacheReply, error) {
	if err := l.svcCtx.Repository.SendCapture(l.ctx, in.Biz, in.Target, in.Ip); err != nil {
		l.Errorf("account/SendCapture: biz=%d target=%s err=%v", in.Biz, in.Target, err)
		return nil, err
	}
	return &rpc.DelCacheReply{}, nil
}
