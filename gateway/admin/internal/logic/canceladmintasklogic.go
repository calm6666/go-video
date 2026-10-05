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

type CancelAdminTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消任务（仅 pending/running 可取消）
func NewCancelAdminTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelAdminTaskLogic {
	return &CancelAdminTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 取消任务：状态机（仅 pending/running 可取消，终态返回 ErrTaskBadTransition）
// 由 operation 判定，网关不缓存任务状态也不本地预判，避免并发下给出错误结论。
func (l *CancelAdminTaskLogic) CancelAdminTask(req *types.ParamCancelAdminTask) (resp *types.OperationTaskResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, true)
	if err != nil {
		return nil, err
	}
	if req.TaskId <= 0 {
		return nil, errors.New("gateway/admin: task_id required")
	}

	reply, err := l.svcCtx.Operation.CancelAdminTask(l.ctx, &operationrpc.CancelAdminTaskReq{
		Ctx:    opCtx,
		TaskId: req.TaskId,
		Reason: req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/cancelAdminTask: operator=%d task_id=%d err=%v", opCtx.GetOperatorId(), req.TaskId, err)
		return nil, err
	}
	l.Infof("gateway/admin/cancelAdminTask: operator=%d task_id=%d state=%s request_id=%s",
		opCtx.GetOperatorId(), req.TaskId, reply.GetTask().GetState(), opCtx.GetRequestId())
	return &types.OperationTaskResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationTaskData{Task: taskToAPI(reply.GetTask())},
		TTL:     0,
	}, nil
}
