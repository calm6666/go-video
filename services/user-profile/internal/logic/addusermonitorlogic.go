package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddUserMonitorLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddUserMonitorLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddUserMonitorLogic {
	return &AddUserMonitorLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 添加用户到监控名单。
// 参考 service.AddUserMonitor：UPSERT 到 user_monitor 并清除软删除。
func (l *AddUserMonitorLogic) AddUserMonitor(in *rpc.AddUserMonitorReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.AddUserMonitor(l.ctx, in.Mid, in.Operator, in.Remark); err != nil {
		l.Errorf("user-profile/AddUserMonitor: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
