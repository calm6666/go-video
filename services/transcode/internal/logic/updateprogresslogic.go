package logic

import (
	"context"
	"time"

	"go-video/services/transcode/internal/svc"
	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateProgressLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateProgressLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateProgressLogic {
	return &UpdateProgressLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UpdateProgress Worker 上报进度，校验状态机：PENDING→PROCESSING→SUCCEEDED/FAILED。
// 终态（SUCCEEDED/FAILED）不可再变更；非法转换返回错误。
func (l *UpdateProgressLogic) UpdateProgress(in *rpc.UpdateProgressReq) (*rpc.TaskReply, error) {
	if in.TaskId <= 0 {
		return nil, model.ErrInvalidTaskID
	}
	if in.Progress < 0 || in.Progress > 100 {
		return nil, model.ErrInvalidProgress
	}
	newState := int32(in.State)
	if newState != model.TaskStateProcessing &&
		newState != model.TaskStateSucceeded && newState != model.TaskStateFailed {
		return nil, model.ErrInvalidState
	}

	// 查询当前任务，校验状态机合法性。
	cur, err := l.svcCtx.Repository.GetTask(l.ctx, in.TaskId)
	if err != nil {
		if err == model.ErrTaskNotFound {
			return nil, err
		}
		l.Errorf("transcode/UpdateProgress: task_id=%d lookup err=%v", in.TaskId, err)
		return nil, err
	}
	if model.IsTerminal(cur.State) {
		return nil, model.ErrTerminalState
	}
	if !model.IsValidTransition(cur.State, newState) {
		l.Errorf("transcode/UpdateProgress: task_id=%d invalid transition %d→%d",
			in.TaskId, cur.State, newState)
		return nil, model.ErrInvalidTransition
	}

	updated, err := l.svcCtx.Repository.UpdateProgress(l.ctx, in.TaskId, in.Progress, newState, in.Errno, in.ErrMsg, time.Now().Unix())
	if err != nil {
		l.Errorf("transcode/UpdateProgress: task_id=%d err=%v", in.TaskId, err)
		return nil, err
	}
	return toTaskReply(updated), nil
}
