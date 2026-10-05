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

type CronListLeasesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 游标分页租约（only_expired=true 排查实例崩溃）
func NewCronListLeasesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronListLeasesLogic {
	return &CronListLeasesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronListLeases 聚合 cron ListLeases。
// now=0 交给 cron 用服务端当前时间，避免多后台标签页各自的本地时钟得出不同的「已过期」结论；
// 传入非 0 即人工设定判定基准（回看历史某个时刻有哪些租约处于过期可抢占状态）。
func (l *CronListLeasesLogic) CronListLeases(req *types.ParamCronListLeases) (resp *types.CronLeasesResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	if err := cronTimestamp("now", req.Now); err != nil {
		return nil, err
	}
	if err := cronPageSize(req.PageSize); err != nil {
		return nil, err
	}
	if err := cronCursor(req.Cursor); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.ListLeases(l.ctx, &cronrpc.ListLeasesReq{
		TaskKey:     req.TaskKey,
		OnlyExpired: req.OnlyExpired,
		Now:         req.Now,
		Cursor:      req.Cursor,
		PageSize:    req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronListLeases: task_key=%s only_expired=%v now=%d cursor=%q page_size=%d err=%v",
			req.TaskKey, req.OnlyExpired, req.Now, req.Cursor, req.PageSize, err)
		return nil, err
	}
	return &types.CronLeasesResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronLeasesData{
			List:       cronLeasesToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
			Total:      reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
