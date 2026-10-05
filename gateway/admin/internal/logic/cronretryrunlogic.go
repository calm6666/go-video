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

type CronRetryRunLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 人工重试已终结执行（同计划时刻追加 attempt，保持幂等上下文）
func NewCronRetryRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronRetryRunLogic {
	return &CronRetryRunLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronRetryRun 聚合 cron RetryRun。
// 人工重试属高危：reason 必填并进入 cron_task_audit，idempotency_key 必填。
// 网关不判断「哪条 run 已终结可以重试」，那是 cron 的状态机规则；
// 新 attempt 落在同一 (task_key, planned_at) 下，处理器幂等上下文不变。
func (l *CronRetryRunLogic) CronRetryRun(req *types.ParamCronRetryRun) (resp *types.CronRetryRunResponse, err error) {
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
	if err := cronPositiveID("run_id", req.RunId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.RetryRun(l.ctx, &cronrpc.RetryRunReq{
		RunId:          req.RunId,
		IdempotencyKey: req.IdempotencyKey,
		Operator:       operator,
		Reason:         req.Reason,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronRetryRun: run_id=%d idempotency_key=%s operator=%s err=%v",
			req.RunId, req.IdempotencyKey, operator, err)
		return nil, err
	}
	return &types.CronRetryRunResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronRetryRunData{
			Run:     cronRunToAPI(reply.GetRun()),
			Created: reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
