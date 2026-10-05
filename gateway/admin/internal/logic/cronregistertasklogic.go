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

type CronRegisterTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 注册任务定义（task_key 唯一；重复注册幂等返回 created=false）
func NewCronRegisterTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronRegisterTaskLogic {
	return &CronRegisterTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronRegisterTask 聚合 cron RegisterTask。
// 幂等：idempotency_key 必填并原样透传（改动即失去幂等语义），task_key 命中唯一键时
// cron 返回 created=false + dedupe_reason，网关不把它改写成错误；
// operator 只来自会话身份；调度自洽性与 state 缺省值由 cron 判定（AGENTS.md §5）。
func (l *CronRegisterTaskLogic) CronRegisterTask(req *types.ParamCronRegisterTask) (resp *types.CronRegisterTaskResponse, err error) {
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
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	definition, err := cronDefinitionForRegister(l.ctx, req.Definition)
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.RegisterTask(l.ctx, &cronrpc.RegisterTaskReq{
		Definition:     definition,
		IdempotencyKey: req.IdempotencyKey,
		Operator:       operator,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronRegisterTask: task_key=%s handler=%s idempotency_key=%s operator=%s err=%v",
			definition.GetTaskKey(), definition.GetHandler(), req.IdempotencyKey, operator, err)
		return nil, err
	}
	return &types.CronRegisterTaskResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronRegisterTaskData{
			Definition:   cronTaskToAPI(reply.GetDefinition()),
			Created:      reply.GetCreated(),
			DedupeReason: reply.GetDedupeReason(),
		},
		TTL: 0,
	}, nil
}
