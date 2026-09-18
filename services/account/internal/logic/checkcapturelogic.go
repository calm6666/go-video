package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CheckCaptureLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCheckCaptureLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CheckCaptureLogic {
	return &CheckCaptureLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 校验验证码。
// 参考 passport-login /captcha/check：比对 Redis 验证码，错误次数超限后失效。
func (l *CheckCaptureLogic) CheckCapture(in *rpc.CheckCaptureReq) (*rpc.DelCacheReply, error) {
	if err := l.svcCtx.Repository.CheckCapture(l.ctx, in.Biz, in.Target, in.CaptureCode); err != nil {
		l.Errorf("account/CheckCapture: biz=%d target=%s err=%v", in.Biz, in.Target, err)
		return nil, err
	}
	return &rpc.DelCacheReply{}, nil
}
