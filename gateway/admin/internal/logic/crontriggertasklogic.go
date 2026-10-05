// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	cronrpc "go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CronTriggerTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 立即触发一次执行，或补跑指定计划时刻
func NewCronTriggerTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronTriggerTaskLogic {
	return &CronTriggerTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronTriggerTask 聚合 cron TriggerTask。
// planned_at=0 表示服务端当前时间；显式传入即补跑某个计划时刻，此时 (task_key, planned_at)
// 就是幂等身份，重复提交只会回到同一条 run（created=false）。
// 触发只是「产生 PENDING 执行记录」，抢占与执行仍由 worker 的 AcquireLease 完成，
// 因此这里不涉及租约所有权，也不会绕过审核或状态机。
func (l *CronTriggerTaskLogic) CronTriggerTask(req *types.ParamCronTriggerTask) (resp *types.CronTriggerResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	operator, err := cronOperator(l.ctx)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("task_key", req.TaskKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := cronTimestamp("planned_at", req.PlannedAt); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.TriggerTask(l.ctx, &cronrpc.TriggerTaskReq{
		TaskKey:        req.TaskKey,
		Params:         req.Params,
		PlannedAt:      req.PlannedAt,
		IdempotencyKey: req.IdempotencyKey,
		Operator:       operator,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronTriggerTask: task_key=%s planned_at=%d idempotency_key=%s operator=%s err=%v",
			req.TaskKey, req.PlannedAt, req.IdempotencyKey, operator, err)
		return nil, err
	}
	return &types.CronTriggerResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronTriggerData{
			Run:     cronRunToAPI(reply.GetRun()),
			Created: reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
