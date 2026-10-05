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

type CronPauseTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 暂停任务（可恢复；重复暂停 changed=false）
func NewCronPauseTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronPauseTaskLogic {
	return &CronPauseTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronPauseTask 聚合 cron PauseTask。
// reason 必填（审计要求可追溯），idempotency_key 必填；expected_version 可选，
// 0 表示「不校验版本直接暂停」，是否已达成目标状态由 cron 以 changed 表达。
func (l *CronPauseTaskLogic) CronPauseTask(req *types.ParamCronPauseTask) (resp *types.CronTaskOperationResponse, err error) {
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
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := cronVersion(req.ExpectedVersion); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.PauseTask(l.ctx, &cronrpc.PauseTaskReq{
		TaskKey:         req.TaskKey,
		Reason:          req.Reason,
		ExpectedVersion: req.ExpectedVersion,
		IdempotencyKey:  req.IdempotencyKey,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronPauseTask: task_key=%s expected_version=%d idempotency_key=%s operator=%s err=%v",
			req.TaskKey, req.ExpectedVersion, req.IdempotencyKey, operator, err)
		return nil, err
	}
	return cronTaskOperationResponse(reply), nil
}
