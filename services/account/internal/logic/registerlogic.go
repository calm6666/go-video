package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RegisterLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRegisterLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegisterLogic {
	return &RegisterLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 注册。
// 参考 passport-login：注册标识唯一性校验（手机/邮箱需验证码），
// 事务内写账号主表 + 凭证 + 密码密钥，成功后直接签发登录态。
func (l *RegisterLogic) Register(in *rpc.RegisterReq) (*rpc.RegisterReply, error) {
	reply, err := l.svcCtx.Repository.Register(l.ctx, in)
	if err != nil {
		l.Errorf("account/Register: account=%s err=%v", in.Account, err)
		return nil, err
	}
	return reply, nil
}
