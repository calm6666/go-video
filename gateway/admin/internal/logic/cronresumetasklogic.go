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

type CronResumeTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 恢复任务（暂停期间过期点按 MisfirePolicy 处理）
func NewCronResumeTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronResumeTaskLogic {
	return &CronResumeTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronResumeTask 聚合 cron ResumeTask。
// 暂停期间错过哪些计划点、补跑几个由任务自己的 MisfirePolicy 决定（cron 判定），
// 网关不接受「立即补跑」之类的覆盖参数，避免绕过定义里已审计过的策略。
func (l *CronResumeTaskLogic) CronResumeTask(req *types.ParamCronResumeTask) (resp *types.CronTaskOperationResponse, err error) {
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
	if err := cronVersion(req.ExpectedVersion); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.ResumeTask(l.ctx, &cronrpc.ResumeTaskReq{
		TaskKey:         req.TaskKey,
		ExpectedVersion: req.ExpectedVersion,
		IdempotencyKey:  req.IdempotencyKey,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronResumeTask: task_key=%s expected_version=%d idempotency_key=%s operator=%s err=%v",
			req.TaskKey, req.ExpectedVersion, req.IdempotencyKey, operator, err)
		return nil, err
	}
	return cronTaskOperationResponse(reply), nil
}
