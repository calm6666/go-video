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

type GetAdminTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询任务与步骤明细（task_id 或 request_id）
func NewGetAdminTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAdminTaskLogic {
	return &GetAdminTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询任务与步骤明细：task_id 与 request_id 二选一（两者都为空时 operation 会报错，
// 网关先挡一次，给出能对应到表单字段的错误消息）。
// 步骤明细里的 err_msg 已由 operation 脱敏，网关不再改写。
func (l *GetAdminTaskLogic) GetAdminTask(req *types.ParamGetAdminTask) (resp *types.OperationTaskDetailResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, false)
	if err != nil {
		return nil, err
	}
	if req.TaskId <= 0 && req.RequestId == "" {
		return nil, errors.New("gateway/admin: task_id or request_id required")
	}

	reply, err := l.svcCtx.Operation.GetAdminTask(l.ctx, &operationrpc.GetAdminTaskReq{
		Ctx:       opCtx,
		TaskId:    req.TaskId,
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/admin/getAdminTask: operator=%d task_id=%d request_id=%s err=%v",
			opCtx.GetOperatorId(), req.TaskId, req.RequestId, err)
		return nil, err
	}
	return &types.OperationTaskDetailResponse{
		Code:    0,
		Message: "ok",
		Data: types.OperationTaskDetailData{
			Task:  taskToAPI(reply.GetTask()),
			Steps: taskStepsToAPI(reply.GetSteps()),
		},
		TTL: 0,
	}, nil
}
