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

type CronListTasksLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 游标分页任务定义（state/task_group/handler 过滤）
func NewCronListTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronListTasksLogic {
	return &CronListTasksLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronListTasks 聚合 cron ListTasks。
// 网关只卡「state 是已知枚举 + page_size 非负 + 游标形态」，
// 页大小上限（越界回 ErrInvalidPageLimit）与游标解析（按 task_key 升序）都由 cron 判定，
// 两边不复算同一套规则（AGENTS.md §5）。
func (l *CronListTasksLogic) CronListTasks(req *types.ParamCronListTasks) (resp *types.CronTasksResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	state, err := cronTaskState(req.State)
	if err != nil {
		return nil, err
	}
	if err := cronPageSize(req.PageSize); err != nil {
		return nil, err
	}
	if err := cronCursor(req.Cursor); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.ListTasks(l.ctx, &cronrpc.ListTasksReq{
		State:     state,
		TaskGroup: req.TaskGroup,
		Handler:   req.Handler,
		Cursor:    req.Cursor,
		PageSize:  req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronListTasks: state=%d group=%s handler=%s cursor=%q page_size=%d err=%v",
			req.State, req.TaskGroup, req.Handler, req.Cursor, req.PageSize, err)
		return nil, err
	}
	return &types.CronTasksResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronTasksData{
			List:       cronTasksToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
			Total:      reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
