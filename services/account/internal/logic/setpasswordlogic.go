package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetPasswordLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetPasswordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetPasswordLogic {
	return &SetPasswordLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 设置/修改密码。
// 参考 secure 服务：已有密码时校验旧密码，事务内置历史 + 新密钥生效，
// 改密后吊销全部会话。
func (l *SetPasswordLogic) SetPassword(in *rpc.SetPasswordReq) (*rpc.DelCacheReply, error) {
	if err := l.svcCtx.Repository.SetPassword(l.ctx, in.Mid, in.OldPassword, in.NewPassword, in.Ip); err != nil {
		l.Errorf("account/SetPassword: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.DelCacheReply{}, nil
}
