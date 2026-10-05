package logic

import (
	"context"

	"go-video/services/search-indexer/internal/svc"
	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRebuildTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRebuildTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRebuildTaskLogic {
	return &GetRebuildTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个重建任务进度。
//
// dlq_count 是观测值（当前待处理死信总数），用于判断「重建期间是否还在漏事件」；
// 读不到时按 0 返回但记录日志——它不属于任务本身的字段，不该让查询失败。
func (l *GetRebuildTaskLogic) GetRebuildTask(in *rpc.GetRebuildTaskReq) (*rpc.RebuildTask, error) {
	task, err := l.svcCtx.Repository.RebuildTask(l.ctx, in.TaskId)
	if err != nil {
		l.Errorf("search-indexer/GetRebuildTask: task_id=%s err=%v", in.TaskId, err)
		return nil, err
	}
	if task == nil {
		return nil, model.ErrTaskNotFound
	}
	dlq, err := l.svcCtx.Repository.DeadLetterCount(l.ctx, model.DLQStateOpen)
	if err != nil {
		// dlq_count 只是观测值，读失败不影响任务查询本身，但必须留痕。
		l.Errorf("search-indexer/GetRebuildTask: 统计死信失败，dlq_count 上报为 0 err=%v", err)
		dlq = 0
	}
	return taskToRPC(task, dlq), nil
}
