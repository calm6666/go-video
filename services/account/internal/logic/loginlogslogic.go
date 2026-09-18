package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LoginLogsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLoginLogsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginLogsLogic {
	return &LoginLogsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 登录日志查询。
// 参考 passport RPC.LoginLogs：最近 N 条登录/注册记录（时间倒序）。
func (l *LoginLogsLogic) LoginLogs(in *rpc.LoginLogsReq) (*rpc.LoginLogsReply, error) {
	logs, err := l.svcCtx.Repository.LoginLogs(l.ctx, in.Mid, in.Limit)
	if err != nil {
		l.Errorf("account/LoginLogs: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.LoginLogsReply{Logs: logs}, nil
}
