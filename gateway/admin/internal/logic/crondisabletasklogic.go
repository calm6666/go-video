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

type CronDisableTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 停用任务（终态，保留历史，只能重新注册恢复）
func NewCronDisableTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronDisableTaskLogic {
	return &CronDisableTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronDisableTask 聚合 cron DisableTask。
// 停用是终态操作（恢复只能重新 Register），因此 reason 与 idempotency_key 都必填，
// 拿不到会话身份时直接拒绝——审计里出现匿名停用就等于没有审计。
func (l *CronDisableTaskLogic) CronDisableTask(req *types.ParamCronDisableTask) (resp *types.CronTaskOperationResponse, err error) {
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
	reply, err := l.svcCtx.Cron.DisableTask(l.ctx, &cronrpc.DisableTaskReq{
		TaskKey:         req.TaskKey,
		Reason:          req.Reason,
		ExpectedVersion: req.ExpectedVersion,
		IdempotencyKey:  req.IdempotencyKey,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronDisableTask: task_key=%s expected_version=%d idempotency_key=%s operator=%s err=%v",
			req.TaskKey, req.ExpectedVersion, req.IdempotencyKey, operator, err)
		return nil, err
	}
	return cronTaskOperationResponse(reply), nil
}
