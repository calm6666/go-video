package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAdminTasksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListAdminTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAdminTasksLogic {
	return &ListAdminTasksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询任务
func (l *ListAdminTasksLogic) ListAdminTasks(in *rpc.ListAdminTasksReq) (*rpc.ListAdminTasksReply, error) {
	if _, err := actorFrom(in.Ctx); err != nil {
		return nil, err
	}
	rows, total, err := l.svcCtx.Repository.ListAdminTasks(l.ctx, in.State, in.TaskType, in.OperatorId, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("operation/ListAdminTasks: state=%q type=%q err=%v", in.State, in.TaskType, err)
		return nil, err
	}
	return &rpc.ListAdminTasksReply{Items: taskInfos(rows), Total: total}, nil
}
