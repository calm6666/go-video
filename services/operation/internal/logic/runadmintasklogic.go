package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RunAdminTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRunAdminTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunAdminTaskLogic {
	return &RunAdminTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 推进任务：逐步骤调用下游 RPC（由 cron 或人工触发，本服务不内置 worker）
func (l *RunAdminTaskLogic) RunAdminTask(in *rpc.RunAdminTaskReq) (*rpc.RunAdminTaskReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	view, executed, err := l.svcCtx.Repository.RunAdminTask(l.ctx, actor, in.TaskId, in.MaxSteps)
	if err != nil {
		l.Errorf("operation/RunAdminTask: task=%d operator=%d err=%v", in.TaskId, actor.AdminID, err)
		return nil, err
	}
	return &rpc.RunAdminTaskReply{
		Task:     taskInfo(view.Task),
		Steps:    stepInfos(view.Steps),
		Executed: executed,
	}, nil
}
