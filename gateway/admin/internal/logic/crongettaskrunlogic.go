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

type CronGetTaskRunLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单条执行记录（attempt/fence_token/lease_owner 全可见）
func NewCronGetTaskRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronGetTaskRunLogic {
	return &CronGetTaskRunLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronGetTaskRun 聚合 cron GetTaskRun。
// run_id=0 只会得到「未找到」，没有排查价值，因此在网关就拦住；
// run 为空按 found=false 表达，与 GetTask 同一口径。
func (l *CronGetTaskRunLogic) CronGetTaskRun(req *types.ParamCronGetTaskRun) (resp *types.CronRunResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	if err := cronPositiveID("run_id", req.RunId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.GetTaskRun(l.ctx, &cronrpc.GetTaskRunReq{RunId: req.RunId})
	if err != nil {
		l.Errorf("gateway/admin/cronGetTaskRun: run_id=%d err=%v", req.RunId, err)
		return nil, err
	}
	run := reply.GetRun()
	return &types.CronRunResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronRunData{
			Run:   cronRunToAPI(run),
			Found: run != nil,
		},
		TTL: 0,
	}, nil
}
