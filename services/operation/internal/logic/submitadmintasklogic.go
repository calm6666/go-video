package logic

import (
	"context"

	"go-video/services/operation/internal/repository"
	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitAdminTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitAdminTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitAdminTaskLogic {
	return &SubmitAdminTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 提交批量运营任务（request_id 幂等，状态 pending）
func (l *SubmitAdminTaskLogic) SubmitAdminTask(in *rpc.SubmitAdminTaskReq) (*rpc.SubmitAdminTaskReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	steps := make([]repository.TaskStepInput, 0, len(in.Steps))
	for _, s := range in.Steps {
		if s == nil {
			continue
		}
		steps = append(steps, repository.TaskStepInput{TargetType: s.TargetType, TargetID: s.TargetId})
	}
	task, reused, err := l.svcCtx.Repository.SubmitAdminTask(l.ctx, actor, repository.SubmitTaskInput{
		TaskType: in.TaskType,
		Params:   in.Params,
		Steps:    steps,
	})
	if err != nil {
		l.Errorf("operation/SubmitAdminTask: operator=%d type=%q request=%q err=%v",
			actor.AdminID, in.TaskType, in.Ctx.GetRequestId(), err)
		return nil, err
	}
	return &rpc.SubmitAdminTaskReply{Task: taskInfo(task), Reused: reused}, nil
}
