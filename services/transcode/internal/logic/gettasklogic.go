package logic

import (
	"context"

	"go-video/services/transcode/internal/svc"
	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"

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

// GetTask 查询任务详情。
func (l *GetTaskLogic) GetTask(in *rpc.TaskReq) (*rpc.TaskReply, error) {
	if in.TaskId <= 0 {
		return nil, model.ErrInvalidTaskID
	}
	task, err := l.svcCtx.Repository.GetTask(l.ctx, in.TaskId)
	if err != nil {
		if err == model.ErrTaskNotFound {
			return nil, err
		}
		l.Errorf("transcode/GetTask: task_id=%d err=%v", in.TaskId, err)
		return nil, err
	}
	return toTaskReply(task), nil
}
