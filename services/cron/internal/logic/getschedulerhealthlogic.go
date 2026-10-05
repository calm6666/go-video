package logic

import (
	"context"
	"sort"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetSchedulerHealthLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetSchedulerHealthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetSchedulerHealthLogic {
	return &GetSchedulerHealthLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// healthWindowSeconds 是「近一小时失败」与租约/执行统计的时间窗。
const healthWindowSeconds = 3600

// 到期积压的扫描上限：健康度只报量级（超过该值时 DueBacklog 会被截断），
// 不做无界 count，避免巡检脚本把调度库拖垮。
const dueBacklogScanLimit = model.MaxPageSize

// 调度健康度（积压、运行中、退避、近一小时失败、过期租约）。
//
// 任一子查询失败即整体返回错误：半张健康度表会让告警误判「一切正常」，假绿比红更危险。
func (l *GetSchedulerHealthLogic) GetSchedulerHealth(
	in *rpc.GetSchedulerHealthReq,
) (*rpc.GetSchedulerHealthReply, error) {
	if in == nil {
		in = &rpc.GetSchedulerHealthReq{}
	}
	now := in.Now
	if now <= 0 {
		now = l.svcCtx.ServerTime()
	}

	groupCounts, err := l.svcCtx.TaskDefinitions.CountByGroupState(l.ctx)
	if err != nil {
		l.Errorf("health: CountByGroupState failed")
		return nil, err
	}
	runCounts, err := l.svcCtx.Runs.CountByGroupStates(l.ctx, now-healthWindowSeconds)
	if err != nil {
		l.Errorf("health: CountByGroupStates failed")
		return nil, err
	}
	expiredLeases, err := l.svcCtx.Leases.CountExpiredByGroup(l.ctx, now)
	if err != nil {
		l.Errorf("health: CountExpiredByGroup failed")
		return nil, err
	}
	due, err := l.svcCtx.TaskDefinitions.ListDue(l.ctx, now, 0, in.TaskGroup, dueBacklogScanLimit)
	if err != nil {
		l.Errorf("health: ListDue failed, group=%s", in.TaskGroup)
		return nil, err
	}

	byGroup := map[string]*rpc.GroupHealth{}
	touch := func(name string) *rpc.GroupHealth {
		g, ok := byGroup[name]
		if !ok {
			g = &rpc.GroupHealth{TaskGroup: name}
			byGroup[name] = g
		}
		return g
	}
	for _, c := range groupCounts {
		if in.TaskGroup != "" && c.TaskGroup != in.TaskGroup {
			continue
		}
		g := touch(c.TaskGroup)
		switch c.State {
		case model.TaskStateEnabled:
			g.EnabledTasks = int32(c.Count)
		case model.TaskStatePaused:
			g.PausedTasks = int32(c.Count)
		}
	}
	for _, c := range runCounts {
		if in.TaskGroup != "" && c.TaskGroup != in.TaskGroup {
			continue
		}
		g := touch(c.TaskGroup)
		switch c.State {
		case model.RunStateRunning:
			g.Running = int32(c.Total)
		case model.RunStateRetrying:
			g.Retrying = int32(c.Total)
		case model.RunStateFailed:
			g.FailedLastHour = int32(c.Total)
		}
	}
	for _, c := range expiredLeases {
		if in.TaskGroup != "" && c.TaskGroup != in.TaskGroup {
			continue
		}
		touch(c.TaskGroup).ExpiredLeases = int32(c.Total)
	}
	for _, d := range due {
		g := touch(d.TaskGroup)
		g.DueBacklog++
		if g.OldestDuePlannedAt == 0 || d.NextFireAt < g.OldestDuePlannedAt {
			g.OldestDuePlannedAt = d.NextFireAt
		}
	}

	// 稳定顺序输出，便于巡检脚本做 diff 与阈值判定。
	groups := make([]*rpc.GroupHealth, 0, len(byGroup))
	for name := range byGroup {
		groups = append(groups, byGroup[name])
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].TaskGroup < groups[j].TaskGroup })

	// 「DB 有定义但进程没实现」必须可见：契约里没有承载 handler/worker 的字段，
	// 这里以 ERROR 日志暴露（README「契约缺口」记录了该问题）。
	if missing := l.missingHandlers(groupCounts); len(missing) > 0 {
		l.Errorf("health: handlers registered in DB but missing in this process: %v (worker=%s tick=%d handlers=%d)",
			missing, l.svcCtx.WorkerID(), l.svcCtx.TickCount(), l.svcCtx.Registry.Len())
	}

	return &rpc.GetSchedulerHealthReply{
		ServerTime: now,
		Groups:     groups,
		Version:    l.svcCtx.BuildVersion(),
	}, nil
}

// missingHandlers 找出「已启用的任务里，本进程注册表没有实现」的 handler 名。
func (l *GetSchedulerHealthLogic) missingHandlers(counts []model.GroupStateCount) []string {
	enabled := make([]string, 0, len(counts))
	for _, c := range counts {
		if c.State == model.TaskStateEnabled && c.Count > 0 {
			enabled = append(enabled, c.TaskGroup)
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var missing []string
	// 任务定义是小表：按分组各取一页即可覆盖绝大多数部署（超过上限时只报已扫到的缺口）。
	for _, group := range enabled {
		rows, _, err := l.svcCtx.TaskDefinitions.ListByCursor(l.ctx, model.TaskStateEnabled, group, "", 0, dueBacklogScanLimit)
		if err != nil {
			l.Errorf("health: list enabled tasks failed, group=%s", group)
			continue
		}
		for _, d := range rows {
			if _, ok := seen[d.Handler]; ok {
				continue
			}
			seen[d.Handler] = struct{}{}
			if !l.svcCtx.Registry.Has(d.Handler) {
				missing = append(missing, d.Handler)
			}
		}
	}
	sort.Strings(missing)
	return missing
}
