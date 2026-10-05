package logic

import (
	"context"

	"go-video/services/moderation-orchestrator/internal/svc"
	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTaskLogic {
	return &GetTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询任务详情。
func (l *GetTaskLogic) GetTask(in *rpc.TaskReq) (*rpc.TaskReply, error) {
	if in.TaskId <= 0 {
		return nil, model.ErrInvalidTaskID
	}
	t, err := l.svcCtx.Repository.GetTask(l.ctx, in.TaskId)
	if err != nil {
		l.Errorf("moderation/GetTask: task=%d err=%v", in.TaskId, err)
		return nil, err
	}
	return &rpc.TaskReply{Task: taskToRPC(t)}, nil
}
