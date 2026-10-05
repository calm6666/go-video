package logic

import (
	"context"

	"go-video/services/moderation-orchestrator/internal/svc"
	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTasksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTasksLogic {
	return &ListTasksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询任务列表（运营后台用）。
func (l *ListTasksLogic) ListTasks(in *rpc.ListTasksReq) (*rpc.TasksReply, error) {
	if in.Ps <= 0 {
		in.Ps = 20
	}
	if in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	if in.Pn <= 0 {
		in.Pn = 1
	}

	tasks, total, err := l.svcCtx.Repository.ListTasks(l.ctx,
		in.Mid, int32(in.ContentType), int32(in.State), in.Pn, in.Ps)
	if err != nil {
		l.Errorf("moderation/ListTasks: mid=%d type=%v state=%v err=%v",
			in.Mid, in.ContentType, in.State, err)
		return nil, err
	}
	out := make([]*rpc.Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskToRPC(t))
	}
	return &rpc.TasksReply{Tasks: out, Total: total}, nil
}
