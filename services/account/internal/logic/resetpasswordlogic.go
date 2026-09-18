package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ResetPasswordLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewResetPasswordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ResetPasswordLogic {
	return &ResetPasswordLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 重置密码（账号找回）。
// 参考 account-recovery：找回验证码校验后覆盖密码并吊销全部会话。
func (l *ResetPasswordLogic) ResetPassword(in *rpc.ResetPasswordReq) (*rpc.DelCacheReply, error) {
	if err := l.svcCtx.Repository.ResetPassword(l.ctx, in.Account, in.CaptureCode, in.NewPassword, in.Ip); err != nil {
		l.Errorf("account/ResetPassword: account=%s err=%v", in.Account, err)
		return nil, err
	}
	return &rpc.DelCacheReply{}, nil
}
