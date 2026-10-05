package logic

import (
	"context"
	"encoding/json"

	"go-video/services/moderation-worker/internal/svc"
	"go-video/services/moderation-worker/model"
	"go-video/services/moderation-worker/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTaskResultLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTaskResultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTaskResultLogic {
	return &GetTaskResultLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetTaskResult 查询任务执行结果。
// 优先按 worker_task_id 反查；为空时按 orchestrator task_id 查最新一条。
// 任务不存在时返回 NotFound 错误。
func (l *GetTaskResultLogic) GetTaskResult(in *rpc.TaskResultReq) (*rpc.TaskResultReply, error) {
	if in.GetWorkerTaskId() == "" && in.GetTaskId() == "" {
		return nil, model.ErrInvalidWorkerTaskID
	}

	var (
		task *model.WorkerTask
		err  error
	)
	if in.GetWorkerTaskId() != "" {
		task, err = l.svcCtx.Repository.GetTask(l.ctx, in.GetWorkerTaskId())
	} else {
		task, err = l.svcCtx.Repository.GetTaskByOrchestrator(l.ctx, in.GetTaskId())
	}
	if err != nil {
		l.Errorf("moderation-worker/GetTaskResult: worker_task_id=%s task_id=%s err=%v",
			in.GetWorkerTaskId(), in.GetTaskId(), err)
		return nil, err
	}
	if task == nil {
		return nil, model.ErrTaskNotFound
	}

	var segments []*rpc.ResultSegment
	if task.ResultJSON != "" {
		_ = json.Unmarshal([]byte(task.ResultJSON), &segments)
	}

	return &rpc.TaskResultReply{
		WorkerTaskId:     task.WorkerTaskID,
		TaskId:           task.TaskID,
		Capability:       rpc.CapabilityType(task.Capability),
		State:            rpc.TaskState(task.State),
		AlgorithmVersion: task.AlgorithmVersion,
		ElapsedMs:        task.ElapsedMs,
		Segments:         segments,
		ErrorMessage:     task.ErrorMessage,
	}, nil
}
