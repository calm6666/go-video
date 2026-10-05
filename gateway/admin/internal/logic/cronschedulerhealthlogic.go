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

type CronSchedulerHealthLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 调度健康度：积压、运行中、退避、近一小时失败、过期租约
func NewCronSchedulerHealthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronSchedulerHealthLogic {
	return &CronSchedulerHealthLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronSchedulerHealth 聚合 cron GetSchedulerHealth。
// 响应里的 server_time 与 version 必须透传：前者是调用方判断「积压是不是时钟漂移造成的」
// 的唯一依据，后者用于把「哪个 cron 构建在漏跑」定位到具体版本。
func (l *CronSchedulerHealthLogic) CronSchedulerHealth(req *types.ParamCronSchedulerHealth) (resp *types.CronHealthResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	if err := cronTimestamp("now", req.Now); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.GetSchedulerHealth(l.ctx, &cronrpc.GetSchedulerHealthReq{
		Now:       req.Now,
		TaskGroup: req.TaskGroup,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronSchedulerHealth: now=%d task_group=%s err=%v", req.Now, req.TaskGroup, err)
		return nil, err
	}
	return &types.CronHealthResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronHealthData{
			ServerTime: reply.GetServerTime(),
			Version:    reply.GetVersion(),
			Groups:     cronGroupHealthsToAPI(reply.GetGroups()),
		},
		TTL: 0,
	}, nil
}
