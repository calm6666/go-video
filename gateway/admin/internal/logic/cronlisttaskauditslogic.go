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

type CronListTaskAuditsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 游标分页任务变更审计（register/update/pause/resume/disable/trigger/retry/replay）
func NewCronListTaskAuditsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronListTaskAuditsLogic {
	return &CronListTaskAuditsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronListTaskAudits 聚合 cron ListTaskAudits。
// action 是 cron 落库的稳定动词（register/update/…），网关按自由文本透传：
// 取值集合由 cron 拥有，网关写死一份就会在服务端新增动作时把它们挡在门外。
func (l *CronListTaskAuditsLogic) CronListTaskAudits(req *types.ParamCronListTaskAudits) (resp *types.CronTaskAuditsResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	if err := cronTimeWindow(req.CtimeFrom, req.CtimeTo); err != nil {
		return nil, err
	}
	if err := cronPageSize(req.PageSize); err != nil {
		return nil, err
	}
	if err := cronCursor(req.Cursor); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.ListTaskAudits(l.ctx, &cronrpc.ListTaskAuditsReq{
		TaskKey:   req.TaskKey,
		Action:    req.Action,
		CtimeFrom: req.CtimeFrom,
		CtimeTo:   req.CtimeTo,
		Cursor:    req.Cursor,
		PageSize:  req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronListTaskAudits: task_key=%s action=%s ctime=[%d,%d] cursor=%q page_size=%d err=%v",
			req.TaskKey, req.Action, req.CtimeFrom, req.CtimeTo, req.Cursor, req.PageSize, err)
		return nil, err
	}
	return &types.CronTaskAuditsResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronTaskAuditsData{
			List:       cronTaskAuditsToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
			Total:      reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
