package logic

import (
	"context"
	"fmt"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RetryRunLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRetryRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryRunLogic {
	return &RetryRunLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 人工重试已终结的执行（同计划时刻追加 attempt）。
//
// 关键约定：planned_at 保持不变、原终态行不改写，只追加新行。
// 于是幂等上下文 (task_key, planned_at) 与失败那次完全一致——处理器配合
// cron_task_checkpoint 的 CAS 游标，重放只会推进未完成部分（README「重放语义」）。
//
// 只接受 FAILED/TIMEOUT：SUCCEEDED 的历史执行要「再跑一遍」属于补跑语义，
// 必须走 TriggerTask（它会留下 trigger_type=MANUAL 的痕迹），不在这里混用，
// 否则审计里再也分不清「系统退避耗尽」和「人工把成功的结果又刷了一遍」。
func (l *RetryRunLogic) RetryRun(in *rpc.RetryRunReq) (*rpc.RetryRunReply, error) {
	if in == nil || in.RunId <= 0 {
		return nil, model.ErrRunNotFound
	}
	if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}

	origin, err := l.svcCtx.Runs.FindOne(l.ctx, in.RunId)
	if err != nil {
		l.Errorf("RetryRun read failed, run_id=%d", in.RunId)
		return nil, err
	}
	if origin == nil {
		return nil, model.ErrRunNotFound
	}
	if origin.State != model.RunStateFailed && origin.State != model.RunStateTimeout {
		return nil, fmt.Errorf("%w: run %d 当前是 %s；补跑请用 TriggerTask，退避中的执行由调度侧自己续跑",
			model.ErrRunNotRetryable, origin.ID, model.RunStateName(origin.State))
	}
	def, err := l.svcCtx.TaskDefinitions.FindOne(l.ctx, origin.TaskKey)
	if err != nil {
		l.Errorf("RetryRun read definition failed, task_key=%s", origin.TaskKey)
		return nil, err
	}
	if def == nil {
		return nil, model.ErrTaskNotFound
	}
	if def.State == model.TaskStateDisabled {
		return nil, fmt.Errorf("%w: %s 已停用（DISABLED 是终态），人工重试也不能跑",
			model.ErrStateTransition, origin.TaskKey)
	}
	if _, err := l.svcCtx.Registry.Resolve(def.Handler); err != nil {
		return nil, err
	}

	operator := operatorOf(in.Operator, l.svcCtx)
	created, stored, err := l.appendAttempt(origin, def, operator, in)
	if err != nil {
		l.Errorf("RetryRun failed, run_id=%d task_key=%s", origin.ID, origin.TaskKey)
		return nil, err
	}
	if stored == nil {
		return nil, model.ErrRunNotFound
	}
	return &rpc.RetryRunReply{Run: runRecord(stored), Created: created}, nil
}

// appendAttempt 在同一事务里「取号 + 插入新行 + 写审计」。
//
// 取号必须在事务里做：并发点两次「重试」时，两个事务都会读到同一个 MaxAttempt，
// 只有一个能命中 uniq_fire_attempt，落败方读回赢方那行并得到 created=false。
func (l *RetryRunLogic) appendAttempt(
	origin *model.TaskRun, def *model.TaskDefinition, operator string, in *rpc.RetryRunReq,
) (bool, *model.TaskRun, error) {
	now := l.svcCtx.ServerTime()
	audit := &model.TaskAudit{
		TaskKey:  origin.TaskKey,
		Action:   model.AuditActionRetry,
		Operator: operator,
		TraceID:  in.TraceId,
		Ctime:    now,
	}
	var (
		created bool
		stored  *model.TaskRun
	)
	err := l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		maxAttempt, err := l.svcCtx.Runs.MaxAttemptTx(ctx, tx, origin.TaskKey, origin.PlannedAt)
		if err != nil {
			return err
		}
		row, isNew, err := l.svcCtx.Runs.ClaimForFire(ctx, tx, &model.TaskRun{
			TaskKey:     origin.TaskKey,
			PlannedAt:   origin.PlannedAt,
			Attempt:     maxAttempt + 1,
			TriggerType: model.TriggerTypeRetry,
			State:       model.RunStatePending,
			Operator:    operator,
			TraceID:     in.TraceId,
		})
		if err != nil {
			return err
		}
		stored, created = row, isNew
		if !isNew {
			return nil
		}
		audit.ToState = model.RunStateName(row.State)
		audit.Detail = auditDetail(map[string]any{
			"origin_run_id":   origin.ID,
			"origin_state":    model.RunStateName(origin.State),
			"origin_attempt":  origin.Attempt,
			"new_run_id":      row.ID,
			"new_attempt":     row.Attempt,
			"planned_at":      row.PlannedAt,
			"max_attempts":    def.MaxAttempts,
			"reason":          in.Reason,
			"idempotency_key": in.IdempotencyKey,
		})
		return l.svcCtx.Audits.Insert(ctx, tx, audit)
	})
	if err != nil {
		return false, nil, err
	}
	return created, stored, nil
}
