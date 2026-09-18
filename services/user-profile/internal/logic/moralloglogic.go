package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MoralLogLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMoralLogLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MoralLogLogic {
	return &MoralLogLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询节操值变更日志（最近 7 天）。
// 参考 service.MoralLog：原实现读报表搜索服务，本项目读本地 member_log 表。
func (l *MoralLogLogic) MoralLog(in *rpc.MemberMidReq) (*rpc.UserLogsReply, error) {
	logs, err := l.svcCtx.Repository.MoralLog(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/MoralLog: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.UserLogsReply{UserLogs: logs}, nil
}
