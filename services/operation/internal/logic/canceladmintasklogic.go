package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CancelAdminTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelAdminTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelAdminTaskLogic {
	return &CancelAdminTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取消任务（仅 pending/running 可取消）
func (l *CancelAdminTaskLogic) CancelAdminTask(in *rpc.CancelAdminTaskReq) (*rpc.CancelAdminTaskReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	task, err := l.svcCtx.Repository.CancelAdminTask(l.ctx, actor, in.TaskId, in.Reason)
	if err != nil {
		l.Errorf("operation/CancelAdminTask: task=%d operator=%d err=%v", in.TaskId, actor.AdminID, err)
		return nil, err
	}
	return &rpc.CancelAdminTaskReply{Task: taskInfo(task)}, nil
}
