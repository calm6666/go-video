// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	operationrpc "go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitAdminTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交批量运营任务（op.request_id 幂等，步骤最多 1000）
func NewSubmitAdminTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitAdminTaskLogic {
	return &SubmitAdminTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 提交批量任务：任务真实执行只由 operation 逐步调用 video/catalog/rights/moderation
// 的 RPC 完成（AGENTS.md §5），网关这里既不触发下架也不猜测任务类型是否受支持。
// 网关先挡两条门槛：request_id（幂等键，缺失会产生第二个批量任务）与步骤数上限。
func (l *SubmitAdminTaskLogic) SubmitAdminTask(req *types.ParamSubmitAdminTask) (resp *types.OperationSubmitTaskResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, true)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("task_type", req.TaskType); err != nil {
		return nil, err
	}
	// 与 operation 的 maxTaskSteps 同口径：超出直接拒绝，不让一条请求把下游打爆。
	if len(req.Steps) > operationMaxTaskSteps {
		return nil, fmt.Errorf("gateway/admin: steps %d exceeds maximum %d", len(req.Steps), operationMaxTaskSteps)
	}

	reply, err := l.svcCtx.Operation.SubmitAdminTask(l.ctx, &operationrpc.SubmitAdminTaskReq{
		Ctx:      opCtx,
		TaskType: req.TaskType,
		Params:   req.Params,
		Steps:    operationSteps(req.Steps),
	})
	if err != nil {
		l.Errorf("gateway/admin/submitAdminTask: operator=%d task_type=%s steps=%d request_id=%s err=%v",
			opCtx.GetOperatorId(), req.TaskType, len(req.Steps), opCtx.GetRequestId(), err)
		return nil, err
	}
	// reused=true 表示命中 request_id 幂等，返回的是既有任务：必须留痕，
	// 否则运营会误以为提交了两批。
	l.Infof("gateway/admin/submitAdminTask: operator=%d task_id=%d state=%s reused=%v request_id=%s",
		opCtx.GetOperatorId(), reply.GetTask().GetTaskId(), reply.GetTask().GetState(),
		reply.GetReused(), opCtx.GetRequestId())
	return &types.OperationSubmitTaskResponse{
		Code:    0,
		Message: "ok",
		Data: types.OperationSubmitTaskData{
			Task:   taskToAPI(reply.GetTask()),
			Reused: reply.GetReused(),
		},
		TTL: 0,
	}, nil
}
