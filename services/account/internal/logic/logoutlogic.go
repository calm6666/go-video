package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LogoutLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLogoutLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LogoutLogic {
	return &LogoutLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 登出。
// 参考 passport-login /token/delete：吊销 token 并删除会话缓存。
func (l *LogoutLogic) Logout(in *rpc.LogoutReq) (*rpc.DelCacheReply, error) {
	if err := l.svcCtx.Repository.Logout(l.ctx, in.Token); err != nil {
		l.Errorf("account/Logout: err=%v", err)
		return nil, err
	}
	return &rpc.DelCacheReply{}, nil
}
