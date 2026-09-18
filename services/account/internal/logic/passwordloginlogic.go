package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PasswordLoginLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPasswordLoginLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PasswordLoginLogic {
	return &PasswordLoginLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 密码登录。
// 参考 passport/passport-login：登录标识（用户名/手机/邮箱）+ 密码校验，
// 成功后签发 token/refresh/csrf 会话并记录登录日志。
func (l *PasswordLoginLogic) PasswordLogin(in *rpc.LoginReq) (*rpc.LoginReply, error) {
	reply, err := l.svcCtx.Repository.PasswordLogin(l.ctx, in)
	if err != nil {
		l.Errorf("account/PasswordLogin: account=%s err=%v", in.Account, err)
		return nil, err
	}
	return reply, nil
}
