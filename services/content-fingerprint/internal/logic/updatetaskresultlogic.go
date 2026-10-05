package logic

import (
	"context"

	"go-video/services/content-fingerprint/internal/svc"
	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateTaskResultLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateTaskResultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateTaskResultLogic {
	return &UpdateTaskResultLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UpdateTaskResult 由 Worker 回写任务结果（PENDING → SUCCEEDED / FAILED），
// 同时 Upsert fingerprint_record（仅 SUCCEEDED 时写 video_key/audio_key 到 record）。
// 依据 AGENTS.md §8，状态机必须经合法路径，不能跳过 PENDING。
func (l *UpdateTaskResultLogic) UpdateTaskResult(in *rpc.UpdateResultReq) (*rpc.TaskReply, error) {
	if in.TaskId <= 0 {
		return nil, model.ErrInvalidTaskID
	}
	target := taskStateToModel(in.State)
	if target != model.TaskStateSucceeded && target != model.TaskStateFailed {
		return nil, model.ErrInvalidState
	}
	updated, err := l.svcCtx.Repository.UpdateTaskResult(
		l.ctx, in.TaskId, target, in.VideoKey, in.AudioKey, in.VideoHash, in.AudioHash)
	if err != nil {
		l.Errorf("content-fingerprint/UpdateTaskResult: task_id=%d state=%v err=%v",
			in.TaskId, in.State, err)
		return nil, err
	}
	return taskToReply(updated), nil
}
