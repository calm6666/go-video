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

type TriggerTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewTriggerTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TriggerTaskLogic {
	return &TriggerTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 立即触发一次执行，或补跑指定计划时刻。
//
// 本方法只「排一次队」：插入 PENDING 执行记录 + 写审计，真正的执行由 worker 通过
// AcquireLease 领取，所以并发上限、租约互斥这些约束都在领取侧统一生效。
//
// 计划时刻的取值：planned_at=0 用服务端当前秒（不信任调用方时钟，否则同一动作在两个
// 实例上会算出两个计划时刻、跑两遍）；显式传入即「补跑某个历史计划时刻」，此时
// (task_key, planned_at) 就是幂等上下文，处理器要配合 checkpoint 游标保证不重复副作用。
func (l *TriggerTaskLogic) TriggerTask(in *rpc.TriggerTaskReq) (*rpc.TriggerTaskReply, error) {
	if in == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
		return nil, err
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}
	// 契约上有 params 覆盖位，但 cron_task_run 没有 params 列、AcquireLease 也不会把
	// 参数带给 worker：收下再丢掉等于骗调用方「按覆盖值跑了」，这里显式失败（AGENTS.md §9）。
	if strings.TrimSpace(in.Params) != "" {
		return nil, model.ErrRunParamsUnsupported
	}
	if in.PlannedAt < 0 {
		return nil, fmt.Errorf("%w: planned_at=%d 不能为负", model.ErrPlannedAtRequired, in.PlannedAt)
	}

	def, err := l.svcCtx.TaskDefinitions.FindOne(l.ctx, taskKey)
	if err != nil {
		l.Errorf("TriggerTask read definition failed, task_key=%s", taskKey)
		return nil, err
	}
	if def == nil {
		return nil, model.ErrTaskNotFound
	}
	if def.State == model.TaskStateDisabled {
		return nil, fmt.Errorf("%w: %s 已停用（DISABLED 是终态），要再跑请先重新 RegisterTask",
			model.ErrStateTransition, taskKey)
	}
	// PAUSED 允许手动触发：运营经常需要「先手动验证一次再恢复调度」。
	if _, err := l.svcCtx.Registry.Resolve(def.Handler); err != nil {
		return nil, err
	}

	plannedAt := in.PlannedAt
	now := l.svcCtx.ServerTime()
	if plannedAt == 0 {
		plannedAt = now
	}
	attempt, err := l.svcCtx.Runs.MaxAttempt(l.ctx, taskKey, plannedAt)
	if err != nil {
		l.Errorf("TriggerTask max attempt failed, task_key=%s planned_at=%d", taskKey, plannedAt)
		return nil, err
	}
	operator := operatorOf(in.Operator, l.svcCtx)
	run := &model.TaskRun{
		TaskKey:     taskKey,
		PlannedAt:   plannedAt,
		Attempt:     attempt + 1,
		TriggerType: model.TriggerTypeManual,
		State:       model.RunStatePending,
		Operator:    operator,
		TraceID:     in.TraceId,
	}

	created, stored, err := l.persist(run, def, operator, in, now)
	if err != nil {
		l.Errorf("TriggerTask failed, task_key=%s planned_at=%d attempt=%d", taskKey, plannedAt, run.Attempt)
		return nil, err
	}
	if stored == nil {
		return nil, model.ErrRunNotFound
	}
	return &rpc.TriggerTaskReply{Run: runRecord(stored), Created: created}, nil
}

// persist 插入执行记录并写 trigger 审计，二者同事务：
// 「跑了一次却没痕迹」和「有痕迹却没排队」都是不可接受的中间态。
func (l *TriggerTaskLogic) persist(
	run *model.TaskRun, def *model.TaskDefinition, operator string, in *rpc.TriggerTaskReq, now int64,
) (bool, *model.TaskRun, error) {
	audit := &model.TaskAudit{
		TaskKey:  run.TaskKey,
		Action:   model.AuditActionTrigger,
		Operator: operator,
		TraceID:  in.TraceId,
		Ctime:    now,
	}
	var (
		created bool
		stored  *model.TaskRun
	)
	err := l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		row, isNew, err := l.svcCtx.Runs.ClaimForFire(ctx, tx, run)
		if err != nil {
			return err
		}
		stored = row
		created = isNew
		if !isNew {
			// 同一计划时刻已有这一号：重复点击只留一条执行，读回既有行给调用方核对。
			return nil
		}
		audit.FromState = ""
		audit.ToState = model.RunStateName(row.State)
		audit.Detail = auditDetail(map[string]any{
			"run_id":           row.ID,
			"planned_at":       row.PlannedAt,
			"attempt":          row.Attempt,
			"task_state":       model.TaskStateName(def.State),
			"handler":          def.Handler,
			"idempotency_key":  in.IdempotencyKey,
			"explicit_planned": in.PlannedAt > 0,
		})
		return l.svcCtx.Audits.Insert(ctx, tx, audit)
	})
	if err != nil {
		return false, nil, err
	}
	return created, stored, nil
}
