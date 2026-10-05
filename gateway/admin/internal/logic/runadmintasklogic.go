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

type RunAdminTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 推进任务（逐步骤调用下游 RPC，由 cron 或人工触发）
func NewRunAdminTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunAdminTaskLogic {
	return &RunAdminTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 推进任务：这是唯一会真正触发批量下架/过期/申诉处理的入口，因此必须带 request_id，
// max_steps<=0 时由 operation 按服务端默认（Cache.RunSteps，默认 100）推进，
// 网关不擅自放大批量，也不在这里传"全部"这类语义。
//
// 运维提示：HTTP 入口的 Timeout（etc/admin.yaml）默认 3000ms，而 operation 单次推进
// 最多跑 RunSteps 步、每步再调用下游 RPC，人工点「推进」可能先触发网关超时——
// 服务端仍会继续执行完，重试请沿用同一个 op.request_id。定时触发应走 services/cron。
func (l *RunAdminTaskLogic) RunAdminTask(req *types.ParamRunAdminTask) (resp *types.OperationRunTaskResponse, err error) {
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

	reply, err := l.svcCtx.Operation.RunAdminTask(l.ctx, &operationrpc.RunAdminTaskReq{
		Ctx:      opCtx,
		TaskId:   req.TaskId,
		MaxSteps: req.MaxSteps,
	})
	if err != nil {
		l.Errorf("gateway/admin/runAdminTask: operator=%d task_id=%d max_steps=%d err=%v",
			opCtx.GetOperatorId(), req.TaskId, req.MaxSteps, err)
		return nil, err
	}
	l.Infof("gateway/admin/runAdminTask: operator=%d task_id=%d executed=%d state=%s succeeded=%d failed=%d",
		opCtx.GetOperatorId(), req.TaskId, reply.GetExecuted(), reply.GetTask().GetState(),
		reply.GetTask().GetSucceeded(), reply.GetTask().GetFailed())
	return &types.OperationRunTaskResponse{
		Code:    0,
		Message: "ok",
		Data: types.OperationRunTaskData{
			Task:     taskToAPI(reply.GetTask()),
			Steps:    taskStepsToAPI(reply.GetSteps()),
			Executed: reply.GetExecuted(),
		},
		TTL: 0,
	}, nil
}
