package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RenewTokenLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRenewTokenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenewTokenLogic {
	return &RenewTokenLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 刷新 token。
// 参考 passport-login /token/renew：校验 refresh 令牌后轮换新 access token。
func (l *RenewTokenLogic) RenewToken(in *rpc.RenewTokenReq) (*rpc.RenewTokenReply, error) {
	reply, err := l.svcCtx.Repository.RenewToken(l.ctx, in.RefreshToken)
	if err != nil {
		l.Errorf("account/RenewToken: err=%v", err)
		return nil, err
	}
	return reply, nil
}
