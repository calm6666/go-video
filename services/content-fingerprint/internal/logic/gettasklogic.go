package logic

import (
	"context"

	"go-video/services/content-fingerprint/internal/svc"
	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"

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

// GetTask 查询单个任务详情；任务不存在返回 ErrTaskNotFound。
// 走 Redis 短缓存（5 分钟），miss 时回源 DB 并回填缓存。
func (l *GetTaskLogic) GetTask(in *rpc.TaskReq) (*rpc.TaskReply, error) {
	if in.TaskId <= 0 {
		return nil, model.ErrInvalidTaskID
	}
	t, err := l.svcCtx.Repository.GetTask(l.ctx, in.TaskId)
	if err != nil {
		l.Errorf("content-fingerprint/GetTask: task_id=%d err=%v", in.TaskId, err)
		return nil, err
	}
	if t == nil {
		return nil, model.ErrTaskNotFound
	}
	return taskToReply(t), nil
}
