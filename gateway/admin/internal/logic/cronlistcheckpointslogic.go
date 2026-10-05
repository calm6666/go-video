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

type CronListCheckpointsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 游标分页增量游标（按 task_key,scope_key 升序）
func NewCronListCheckpointsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronListCheckpointsLogic {
	return &CronListCheckpointsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronListCheckpoints 聚合 cron ListCheckpoints。
// task_key 可空（对比同批任务的推进水位）；游标是 (task_key, scope_key) 复合键，
// 网关不解析其内容，只保证形态与页大小非负，越界由 cron 回 ErrInvalidPageLimit。
func (l *CronListCheckpointsLogic) CronListCheckpoints(req *types.ParamCronListCheckpoints) (resp *types.CronCheckpointsResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	if err := cronPageSize(req.PageSize); err != nil {
		return nil, err
	}
	if err := cronCursor(req.Cursor); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.ListCheckpoints(l.ctx, &cronrpc.ListCheckpointsReq{
		TaskKey:  req.TaskKey,
		Cursor:   req.Cursor,
		PageSize: req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronListCheckpoints: task_key=%s cursor=%q page_size=%d err=%v",
			req.TaskKey, req.Cursor, req.PageSize, err)
		return nil, err
	}
	return &types.CronCheckpointsResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronCheckpointsData{
			List:       cronCheckpointsToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
			Total:      reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
