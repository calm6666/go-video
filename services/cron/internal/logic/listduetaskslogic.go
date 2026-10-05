package logic

import (
	"context"
	"fmt"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListDueTasksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListDueTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListDueTasksLogic {
	return &ListDueTasksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 拉取到期任务清单（只读，不占租约）。
//
// 本方法不写 cron_task_run、也不占用 cron_task_lease，claim 一律由 AcquireLease 完成，
// 因此可以被多个副本同时安全调用；返回的 server_time 供调度端校正本地时钟漂移。
func (l *ListDueTasksLogic) ListDueTasks(in *rpc.ListDueTasksReq) (*rpc.ListDueTasksReply, error) {
	if in == nil {
		in = &rpc.ListDueTasksReq{}
	}
	now := in.Now
	if now <= 0 {
		now = l.svcCtx.ServerTime()
	}
	limit, err := dueScanLimit(l.svcCtx, in.Limit)
	if err != nil {
		return nil, err
	}
	lookahead := in.LookaheadSeconds
	if lookahead < 0 {
		lookahead = 0
	}

	rows, err := l.svcCtx.TaskDefinitions.ListDue(l.ctx, now, int64(lookahead), in.TaskGroup, limit)
	if err != nil {
		l.Errorf("ListDueTasks scan failed, group=%s limit=%d", in.TaskGroup, limit)
		return nil, err
	}
	list := make([]*rpc.DueTask, 0, len(rows))
	for _, d := range rows {
		running, err := l.svcCtx.Runs.CountRunning(l.ctx, d.TaskKey)
		if err != nil {
			// 并发数拿不到就整体失败：返回 running_count=0 会让调度端误判「没在跑」而超发。
			l.Errorf("ListDueTasks count running failed, task_key=%s", d.TaskKey)
			return nil, err
		}
		if item := dueTaskInfo(d, running); item != nil {
			list = append(list, item)
		}
	}
	return &rpc.ListDueTasksReply{List: list, ServerTime: l.svcCtx.ServerTime()}, nil
}

// dueScanLimit 收敛单轮扫描量：0 用配置的 BatchSize，越界报错不放大。
func dueScanLimit(svcCtx *svc.ServiceContext, requested int32) (int, error) {
	if requested <= 0 {
		batch := svcCtx.Config.Scheduler.BatchSize
		if batch <= 0 {
			batch = model.DefaultPageSize
		}
		if int64(batch) > model.MaxPageSize {
			batch = model.MaxPageSize
		}
		return int(batch), nil
	}
	if requested > model.MaxPageSize {
		return 0, fmt.Errorf("%w: limit=%d, max=%d", model.ErrInvalidPageLimit, requested, model.MaxPageSize)
	}
	return int(requested), nil
}
