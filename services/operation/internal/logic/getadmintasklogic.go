package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetAdminTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetAdminTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAdminTaskLogic {
	return &GetAdminTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询任务与步骤明细
func (l *GetAdminTaskLogic) GetAdminTask(in *rpc.GetAdminTaskReq) (*rpc.GetAdminTaskReply, error) {
	if _, err := actorFrom(in.Ctx); err != nil {
		return nil, err
	}
	view, err := l.svcCtx.Repository.GetAdminTask(l.ctx, in.TaskId, in.RequestId)
	if err != nil {
		l.Errorf("operation/GetAdminTask: task=%d request=%q err=%v", in.TaskId, in.RequestId, err)
		return nil, err
	}
	return &rpc.GetAdminTaskReply{Task: taskInfo(view.Task), Steps: stepInfos(view.Steps)}, nil
}
