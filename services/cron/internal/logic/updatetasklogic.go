package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type UpdateTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateTaskLogic {
	return &UpdateTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 修改任务定义（乐观锁）。
//
// 可改字段就是「调度面」：name/handler/group/schedule/timeout/retry/concurrency/
// lease_ttl/misfire/params/secret_refs/owner。task_key、state、version 与所有时间戳
// 一律以服务端为准，请求里的值被忽略——后台不能「顺手」把停用任务改回启用，
// 状态迁移只能走 Pause/Resume/Disable（那条路才带 reason + 审计）。
func (l *UpdateTaskLogic) UpdateTask(in *rpc.UpdateTaskReq) (*rpc.UpdateTaskReply, error) {
	if in == nil || in.Definition == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}
	if in.ExpectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version 必须显式给出（先 GetTask 再改）", model.ErrVersionConflict)
	}
	defaults := defaultsOf(l.svcCtx)
	patch, err := defaults.definitionFromProto(in.Definition, false)
	if err != nil {
		return nil, err
	}
	// task_key 以路径参数为准：请求体里的 definition.task_key 允许省略，
	// 给了但不一致时必须失败，否则会「改 A 任务的字段写到 B 上」。
	if body := strings.TrimSpace(in.Definition.TaskKey); body != "" && body != taskKey {
		return nil, fmt.Errorf("%w: task_key=%s 与 definition.task_key=%s 不一致，更新的目标只能是同一个任务",
			model.ErrTaskKeyEmpty, taskKey, body)
	}
	patch.TaskKey = taskKey

	before, err := l.svcCtx.TaskDefinitions.FindOne(l.ctx, taskKey)
	if err != nil {
		l.Errorf("UpdateTask read failed, task_key=%s", taskKey)
		return nil, err
	}
	if before == nil {
		return nil, model.ErrTaskNotFound
	}
	merged := mergeDefinition(before, patch)
	applyDefinitionDefaults(merged, defaults)
	if merged.TimeoutSeconds <= 0 {
		tmo, err := l.svcCtx.Registry.EffectiveTimeout(merged)
		if err != nil {
			l.Errorf("UpdateTask effective timeout failed, task_key=%s handler=%s", taskKey, merged.Handler)
			return nil, err
		}
		merged.TimeoutSeconds = tmo
	}
	if err := l.svcCtx.Registry.ValidateDefinition(merged); err != nil {
		return nil, err
	}
	if err := model.ValidateTaskDefinition(merged); err != nil {
		return nil, err
	}

	// 调度参数变了才重算指针；ENABLED 才需要指针（暂停/停用的指针由 SetState 与本文件负责）。
	nextFireAt := before.NextFireAt
	scheduleChanged := before.ScheduleType != merged.ScheduleType ||
		before.CronExpr != merged.CronExpr ||
		before.IntervalSeconds != merged.IntervalSeconds ||
		before.Timezone != merged.Timezone
	if scheduleChanged && merged.State == model.TaskStateEnabled {
		if merged.ScheduleType == model.ScheduleTypeManual {
			nextFireAt = 0
		} else {
			computed, err := model.FirstFireAt(merged, defaults.Timezone, l.svcCtx.ServerTime())
			if err != nil {
				return nil, err
			}
			nextFireAt = computed
		}
	}

	operator := operatorOf(in.Operator, l.svcCtx)
	updated, err := l.persist(merged, nextFireAt, scheduleChanged, before, operator, in.TraceId)
	if err != nil {
		l.Errorf("UpdateTask failed, task_key=%s expected_version=%d", taskKey, in.ExpectedVersion)
		return nil, err
	}
	return &rpc.UpdateTaskReply{Definition: taskDefinitionInfo(updated)}, nil
}

// persist 把「改定义 + 落调度指针 + 写审计」放进同一事务。
// 指针变化必须写进 detail 的旧值/新值：调度计划被改是线上事故里最常见的成因。
func (l *UpdateTaskLogic) persist(
	merged *model.TaskDefinition, nextFireAt int64, scheduleChanged bool,
	before *model.TaskDefinition, operator, traceID string,
) (*model.TaskDefinition, error) {
	var advanced bool
	err := l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		ok, err := l.svcCtx.TaskDefinitions.UpdateMutableTx(ctx, tx, merged, before.Version)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: task_key=%s expected_version=%d 已被他人改动，请重读后重试",
				model.ErrVersionConflict, merged.TaskKey, before.Version)
		}
		if nextFireAt != before.NextFireAt {
			if err := l.svcCtx.TaskDefinitions.SetNextFireAtTx(ctx, tx, merged.TaskKey, nextFireAt); err != nil {
				return err
			}
		}
		fields := map[string]any{
			"expected_version": before.Version,
			"schedule_changed": scheduleChanged,
			"prev_schedule":    scheduleSummary(before),
			"schedule":         scheduleSummary(merged),
			"handler_changed":  before.Handler != merged.Handler,
			"params_bytes":     len(merged.Params),
			"operator":         operator,
		}
		if nextFireAt != before.NextFireAt {
			fields["prev_next_fire_at"] = before.NextFireAt
			fields["next_fire_at"] = nextFireAt
		}
		audit := &model.TaskAudit{
			TaskKey:   merged.TaskKey,
			Action:    model.AuditActionUpdate,
			FromState: model.TaskStateName(before.State),
			ToState:   model.TaskStateName(merged.State),
			Operator:  operator,
			Detail:    auditDetail(fields),
			TraceID:   traceID,
		}
		if err := l.svcCtx.Audits.Insert(ctx, tx, audit); err != nil {
			return err
		}
		advanced = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !advanced {
		return nil, model.ErrVersionConflict
	}
	row, err := l.svcCtx.TaskDefinitions.FindOne(l.ctx, merged.TaskKey)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, model.ErrTaskNotFound
	}
	return row, nil
}
