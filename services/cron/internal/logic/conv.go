// 本文件是 logic 包的手写扩展（rpc ↔ model 投影），不是 goctl 生成产物。
//
// AGENTS.md §4/§5：领域服务不返回数据库原始对象。投影只搬运事实字段，
// 不在这里做业务判定；也不要把 params/secret_refs 之外的敏感内容塞进响应。

package logic

import (
	"strings"

	"go-video/services/cron/model"
	"go-video/services/cron/rpc"
)

// taskDefinitionInfo 把 cron_task_definition 行投影为 rpc.TaskDefinition。
func taskDefinitionInfo(d *model.TaskDefinition) *rpc.TaskDefinition {
	if d == nil {
		return nil
	}
	return &rpc.TaskDefinition{
		TaskId:               d.ID,
		TaskKey:              d.TaskKey,
		Name:                 d.Name,
		Handler:              d.Handler,
		TaskGroup:            d.TaskGroup,
		ScheduleType:         rpc.ScheduleType(d.ScheduleType),
		CronExpr:             d.CronExpr,
		IntervalSeconds:      d.IntervalSeconds,
		Timezone:             d.Timezone,
		TimeoutSeconds:       d.TimeoutSeconds,
		MaxAttempts:          d.MaxAttempts,
		RetryBaseSeconds:     d.RetryBaseSeconds,
		RetryMaxSeconds:      d.RetryMaxSeconds,
		ConcurrencyLimit:     d.ConcurrencyLimit,
		LeaseTtlSeconds:      d.LeaseTTLSeconds,
		MisfirePolicy:        rpc.MisfirePolicy(d.MisfirePolicy),
		MisfireBackfillLimit: d.MisfireBackfillLim,
		Params:               d.Params,
		SecretRefs:           d.SecretRefs,
		State:                rpc.TaskState(d.State),
		NextFireAt:           d.NextFireAt,
		LastFireAt:           d.LastFireAt,
		LastSuccessAt:        d.LastSuccessAt,
		LastError:            d.LastError,
		Version:              d.Version,
		Owner:                d.Owner,
		Operator:             d.Operator,
		Ctime:                d.Ctime,
		Mtime:                d.Mtime,
	}
}

// taskDefinitionList 批量投影，nil 元素跳过（model 不会返回 nil 行，这里只是防御）。
func taskDefinitionList(rows []*model.TaskDefinition) []*rpc.TaskDefinition {
	out := make([]*rpc.TaskDefinition, 0, len(rows))
	for _, r := range rows {
		if info := taskDefinitionInfo(r); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// runRecord 把 cron_task_run 行投影为 rpc.RunRecord。
func runRecord(r *model.TaskRun) *rpc.RunRecord {
	if r == nil {
		return nil
	}
	return &rpc.RunRecord{
		RunId:         r.ID,
		TaskKey:       r.TaskKey,
		PlannedAt:     r.PlannedAt,
		Attempt:       r.Attempt,
		TriggerType:   rpc.TriggerType(r.TriggerType),
		State:         rpc.RunState(r.State),
		LeaseOwner:    r.LeaseOwner,
		LeaseExpireAt: r.LeaseExpireAt,
		FenceToken:    r.FenceToken,
		StartedAt:     r.StartedAt,
		FinishedAt:    r.FinishedAt,
		DurationMs:    r.DurationMs,
		ResultSummary: r.ResultSummary,
		LastError:     r.LastError,
		NextRetryAt:   r.NextRetryAt,
		TraceId:       r.TraceID,
		Ctime:         r.Ctime,
		Mtime:         r.Mtime,
	}
}

// runRecordList 批量投影执行记录。
func runRecordList(rows []*model.TaskRun) []*rpc.RunRecord {
	out := make([]*rpc.RunRecord, 0, len(rows))
	for _, r := range rows {
		if info := runRecord(r); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// leaseInfo 把 cron_task_lease 行投影为 rpc.LeaseInfo。
// owner 为空即「无人持有」，expire_at=0 与之对应。
func leaseInfo(l *model.TaskLease) *rpc.LeaseInfo {
	if l == nil {
		return nil
	}
	return &rpc.LeaseInfo{
		LeaseKey:      l.LeaseKey,
		Owner:         l.OwnerInstance,
		FenceToken:    l.FenceToken,
		ExpireAt:      l.ExpireAt,
		AcquiredAt:    l.AcquiredAt,
		TakeoverCount: l.TakeoverCount,
	}
}

// leaseInfoList 批量投影租约。
func leaseInfoList(rows []*model.TaskLease) []*rpc.LeaseInfo {
	out := make([]*rpc.LeaseInfo, 0, len(rows))
	for _, r := range rows {
		if info := leaseInfo(r); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// checkpointInfo 把 cron_task_checkpoint 行投影为 rpc.Checkpoint。
func checkpointInfo(c *model.TaskCheckpoint) *rpc.Checkpoint {
	if c == nil {
		return nil
	}
	return &rpc.Checkpoint{
		TaskKey:  c.TaskKey,
		ScopeKey: c.ScopeKey,
		Value:    c.Value,
		ValueStr: c.ValueStr,
		Version:  c.Version,
		Operator: c.Operator,
		Ctime:    c.Ctime,
		Mtime:    c.Mtime,
	}
}

// checkpointInfoList 批量投影游标。
func checkpointInfoList(rows []*model.TaskCheckpoint) []*rpc.Checkpoint {
	out := make([]*rpc.Checkpoint, 0, len(rows))
	for _, r := range rows {
		if info := checkpointInfo(r); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// checkpointFromProto 把请求里的游标转成落库行。
// task_key 必填；scope_key 允许空串（默认游标）。
//
// 长度按 cron_task_checkpoint 的列宽逐列校验：value_str 只有 VARCHAR(255)，
// 以前用 MaxParamsBytes(4096) 当上限，等于「校验形同不存在」——超长的别名/分区名
// 会带着 200 走到 INSERT 才炸，而 CAS 语义下处理器会把这次失败误读成「版本冲突」。
func checkpointFromProto(in *rpc.Checkpoint) (*model.TaskCheckpoint, error) {
	if in == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}
	c := &model.TaskCheckpoint{
		TaskKey:  taskKey,
		ScopeKey: in.ScopeKey,
		Value:    in.Value,
		ValueStr: in.ValueStr,
		Operator: strings.TrimSpace(in.Operator),
	}
	for _, lim := range []struct {
		field string
		value string
		max   int
	}{
		{"checkpoint.task_key", c.TaskKey, model.MaxTaskKeyBytes},
		{"checkpoint.scope_key", c.ScopeKey, model.MaxScopeKeyBytes},
		{"checkpoint.value_str", c.ValueStr, model.MaxValueStrBytes},
		{"checkpoint.operator", c.Operator, model.MaxOperatorBytes},
	} {
		if err := checkTextLimit(lim.field, lim.value, lim.max); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// auditInfo 把 cron_task_audit 行投影为 rpc.TaskAudit。
func auditInfo(a *model.TaskAudit) *rpc.TaskAudit {
	if a == nil {
		return nil
	}
	return &rpc.TaskAudit{
		Id:        a.ID,
		TaskKey:   a.TaskKey,
		Action:    a.Action,
		FromState: a.FromState,
		ToState:   a.ToState,
		Operator:  a.Operator,
		Detail:    a.Detail,
		TraceId:   a.TraceID,
		Ctime:     a.Ctime,
	}
}

// auditInfoList 批量投影审计。
func auditInfoList(rows []*model.TaskAudit) []*rpc.TaskAudit {
	out := make([]*rpc.TaskAudit, 0, len(rows))
	for _, r := range rows {
		if info := auditInfo(r); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// dueTaskInfo 组装一个可执行的计划点。
// plannedAt 取定义的 next_fire_at；running 是该任务当前未过期 RUNNING 数。
func dueTaskInfo(d *model.TaskDefinition, running int64) *rpc.DueTask {
	if d == nil {
		return nil
	}
	return &rpc.DueTask{
		TaskKey:          d.TaskKey,
		PlannedAt:        d.NextFireAt,
		Handler:          d.Handler,
		Params:           d.Params,
		TimeoutSeconds:   d.TimeoutSeconds,
		MaxAttempts:      d.MaxAttempts,
		LeaseTtlSeconds:  d.LeaseTTLSeconds,
		ConcurrencyLimit: d.ConcurrencyLimit,
		RunningCount:     running,
		TaskGroup:        d.TaskGroup,
	}
}
