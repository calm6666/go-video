package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ExpLogLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewExpLogLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExpLogLogic {
	return &ExpLogLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询经验变更日志（最近 7 天）。
// 参考 service.ExpLog：原实现读报表搜索服务，本项目读本地 member_log 表。
func (l *ExpLogLogic) ExpLog(in *rpc.MidReq) (*rpc.UserLogsReply, error) {
	logs, err := l.svcCtx.Repository.ExpLog(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/ExpLog: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.UserLogsReply{UserLogs: logs}, nil
}
