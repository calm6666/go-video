package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type IsInMonitorLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIsInMonitorLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsInMonitorLogic {
	return &IsInMonitorLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询用户是否在监控名单。
// 参考 service.IsInMonitor：user_monitor 表 is_deleted=0 计数。
func (l *IsInMonitorLogic) IsInMonitor(in *rpc.MidReq) (*rpc.IsInMonitorReply, error) {
	inMonitor, err := l.svcCtx.Repository.IsInMonitor(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/IsInMonitor: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.IsInMonitorReply{IsInMonitor: inMonitor}, nil
}
