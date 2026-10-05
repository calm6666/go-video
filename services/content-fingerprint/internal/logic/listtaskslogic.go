package logic

import (
	"context"

	"go-video/services/content-fingerprint/internal/svc"
	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"

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

// ListTasks 分页查询任务，按 asset_id 或 state 过滤。
// ps 上限 50，超过返回 ErrPsTooLarge。
func (l *ListTasksLogic) ListTasks(in *rpc.ListReq) (*rpc.TasksReply, error) {
	if in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	tasks, total, err := l.svcCtx.Repository.ListTasks(l.ctx, in.AssetId, taskStateToModel(in.State), in.Pn, in.Ps)
	if err != nil {
		l.Errorf("content-fingerprint/ListTasks: asset_id=%d state=%v pn=%d ps=%d err=%v",
			in.AssetId, in.State, in.Pn, in.Ps, err)
		return nil, err
	}
	reply := &rpc.TasksReply{
		Total: total,
		Tasks: make([]*rpc.TaskReply, 0, len(tasks)),
	}
	for _, t := range tasks {
		reply.Tasks = append(reply.Tasks, taskToReply(t))
	}
	return reply, nil
}
