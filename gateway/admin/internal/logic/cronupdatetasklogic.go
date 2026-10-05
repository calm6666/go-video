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

type CronUpdateTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 修改任务定义（expected_version 乐观锁；state 不在此处改）
func NewCronUpdateTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronUpdateTaskLogic {
	return &CronUpdateTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronUpdateTask 聚合 cron UpdateTask。
// 契约里没有 idempotency_key（见最终报告缺口清单），因此并发重放的唯一防线是
// expected_version：网关要求它 >=1（库里 version 默认 1，0 只能是「没读到版本」），
// 冲突由 cron 判定并原样上抛，绝不静默覆盖。
// state/task_key/version 等以服务端为准，投影时强制清空并对客户端声明留痕。
func (l *CronUpdateTaskLogic) CronUpdateTask(req *types.ParamCronUpdateTask) (resp *types.CronTaskResponse, err error) {
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
	if err := cronRequiredVersion(req.ExpectedVersion); err != nil {
		return nil, err
	}
	definition, err := cronDefinitionForUpdate(l.ctx, req.TaskKey, req.Definition)
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.UpdateTask(l.ctx, &cronrpc.UpdateTaskReq{
		TaskKey:         definition.GetTaskKey(),
		Definition:      definition,
		ExpectedVersion: req.ExpectedVersion,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronUpdateTask: task_key=%s expected_version=%d operator=%s err=%v",
			definition.GetTaskKey(), req.ExpectedVersion, operator, err)
		return nil, err
	}
	def := reply.GetDefinition()
	return &types.CronTaskResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronTaskData{
			Definition: cronTaskToAPI(def),
			Found:      def != nil,
		},
		TTL: 0,
	}, nil
}
