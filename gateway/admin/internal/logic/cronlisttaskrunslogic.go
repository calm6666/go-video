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

type CronListTaskRunsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 游标分页执行记录（按 planned_at,run_id 倒序）
func NewCronListTaskRunsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronListTaskRunsLogic {
	return &CronListTaskRunsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronListTaskRuns 聚合 cron ListTaskRuns。
// task_key 可空（跨任务排查失败潮），planned_to=0 表示不限；
// 时间窗只校验非负与不反向，跨度上限由 cron 判定。
func (l *CronListTaskRunsLogic) CronListTaskRuns(req *types.ParamCronListTaskRuns) (resp *types.CronRunsResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	state, err := cronRunStateFilter(req.State)
	if err != nil {
		return nil, err
	}
	if err := cronTimeWindow(req.PlannedFrom, req.PlannedTo); err != nil {
		return nil, err
	}
	if err := cronPageSize(req.PageSize); err != nil {
		return nil, err
	}
	if err := cronCursor(req.Cursor); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.ListTaskRuns(l.ctx, &cronrpc.ListTaskRunsReq{
		TaskKey:     req.TaskKey,
		State:       state,
		PlannedFrom: req.PlannedFrom,
		PlannedTo:   req.PlannedTo,
		Cursor:      req.Cursor,
		PageSize:    req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronListTaskRuns: task_key=%s state=%d planned=[%d,%d] cursor=%q page_size=%d err=%v",
			req.TaskKey, req.State, req.PlannedFrom, req.PlannedTo, req.Cursor, req.PageSize, err)
		return nil, err
	}
	return &types.CronRunsResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronRunsData{
			List:       cronRunsToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
			Total:      reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
