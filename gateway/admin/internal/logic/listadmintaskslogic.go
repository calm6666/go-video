// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	operationrpc "go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAdminTasksLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询管理任务（state/task_type/operator_id 过滤）
func NewListAdminTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAdminTasksLogic {
	return &ListAdminTasksLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询任务：state/task_type 是服务端字符串枚举，网关不校验取值（写错只会查空，
// 提前拦住反而会把 operation 新增的任务类型挡在门外）。
func (l *ListAdminTasksLogic) ListAdminTasks(req *types.ParamListAdminTasks) (resp *types.OperationTasksResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOperationPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Operation.ListAdminTasks(l.ctx, &operationrpc.ListAdminTasksReq{
		Ctx:        opCtx,
		State:      req.State,
		TaskType:   req.TaskType,
		OperatorId: req.OperatorId,
		Pn:         pn,
		Ps:         ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listAdminTasks: operator=%d state=%q task_type=%q target_operator=%d pn=%d ps=%d err=%v",
			opCtx.GetOperatorId(), req.State, req.TaskType, req.OperatorId, pn, ps, err)
		return nil, err
	}
	return &types.OperationTasksResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationTasksData{Total: reply.GetTotal(), Items: tasksToAPI(reply.GetItems())},
		TTL:     0,
	}, nil
}
