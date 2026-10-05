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

type CronGetLeaseLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个任务租约（fence_token 与 takeover_count 是抢占证据）
func NewCronGetLeaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronGetLeaseLogic {
	return &CronGetLeaseLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronGetLease 聚合 cron GetLease。
// scope 空表示按 task_key 全局互斥的租约；found=false 表示从未有人 claim 过。
// 这里只读租约，不做抢占：AcquireLease/RenewLease/ReleaseLease 属执行器语义，未开 HTTP 入口。
func (l *CronGetLeaseLogic) CronGetLease(req *types.ParamCronGetLease) (resp *types.CronLeaseResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	if err := requireNonEmpty("task_key", req.TaskKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.GetLease(l.ctx, &cronrpc.GetLeaseReq{
		TaskKey: req.TaskKey,
		Scope:   req.Scope,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronGetLease: task_key=%s scope=%s err=%v", req.TaskKey, req.Scope, err)
		return nil, err
	}
	return &types.CronLeaseResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronLeaseData{
			Lease: cronLeaseToAPI(reply.GetLease()),
			Found: reply.GetFound(),
		},
		TTL: 0,
	}, nil
}
