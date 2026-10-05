package logic

import (
	"context"

	"go-video/services/transcode/internal/svc"
	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"

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

// ListTasks 分页查询任务（按 asset_id 或 state 过滤）。
func (l *ListTasksLogic) ListTasks(in *rpc.ListReq) (*rpc.TasksReply, error) {
	if in.Ps < 0 || in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	state := int32(in.State)
	if state != 0 && state != model.TaskStatePending &&
		state != model.TaskStateProcessing && state != model.TaskStateSucceeded && state != model.TaskStateFailed {
		return nil, model.ErrInvalidState
	}
	tasks, total, err := l.svcCtx.Repository.ListTasks(l.ctx, in.AssetId, state, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("transcode/ListTasks: asset=%d state=%d err=%v", in.AssetId, state, err)
		return nil, err
	}
	reply := &rpc.TasksReply{
		Total: total,
		Tasks: make([]*rpc.TaskReply, 0, len(tasks)),
	}
	for _, t := range tasks {
		reply.Tasks = append(reply.Tasks, toTaskReply(t))
	}
	return reply, nil
}
