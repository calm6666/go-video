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

type ReportTaskResultLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportTaskResultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportTaskResultLogic {
	return &ReportTaskResultLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 上报执行结果（幂等，可带游标 CAS）。
//
// 原子性要求：结果与游标必须在同一个事务里落地。
// 「副作用已完成但游标没前进」会让重放再吃一遍，「游标前进了但结果没落」会让运营
// 以为跑完了 —— 两种都不可接受，所以游标 CAS 冲突时整体回滚返回 ErrCheckpointConflict。
//
// 幂等：同一 (run_id) 重复上报时 first_reported=false，直接回读既有终态行，
// 绝不覆盖历史轨迹；被抢占的旧实例（fence/owner 不一致）拿 ErrLeaseLost。
func (l *ReportTaskResultLogic) ReportTaskResult(in *rpc.ReportTaskResultReq) (*rpc.ReportTaskResultReply, error) {
	if in == nil || in.RunId <= 0 {
		return nil, model.ErrRunNotFound
	}
	if in.Owner == "" {
		return nil, model.ErrLeaseOwnerRequired
	}
	if in.FenceToken <= 0 {
		return nil, fmt.Errorf("%w: fence_token=%d 非法，必须回传 AcquireLease 返回的值",
			model.ErrLeaseNotHeld, in.FenceToken)
	}
	maxSummary := summaryLimit(l.svcCtx.Config.Task.MaxResultSummaryBytes)
	if err := checkTextLimit("result_summary", in.ResultSummary, maxSummary); err != nil {
		return nil, err
	}

	run, err := l.svcCtx.Runs.FindOne(l.ctx, in.RunId)
	if err != nil {
		l.Errorf("ReportTaskResult read run failed, run_id=%d", in.RunId)
		return nil, err
	}
	if run == nil {
		return nil, model.ErrRunNotFound
	}
	if run.FenceToken != in.FenceToken {
		return nil, fmt.Errorf("%w: run_id=%d 栅栏不一致（库内 %d，请求 %d）",
			model.ErrLeaseLost, run.ID, run.FenceToken, in.FenceToken)
	}
	if run.LeaseOwner != in.Owner {
		return nil, fmt.Errorf("%w: run_id=%d 当前持有者 %s，请求方 %s",
			model.ErrLeaseNotHeld, run.ID, run.LeaseOwner, in.Owner)
	}
	if model.IsTerminalRunState(run.State) {
		// 幂等重入：不覆盖终态行，也不动游标（上一次上报已把两者一起落过）。
		l.Infof("cron/report: run_id=%d 已是终态 %s，本次上报按幂等丢弃", run.ID, model.RunStateName(run.State))
		return &rpc.ReportTaskResultReply{
			Run:                runRecord(run),
			FirstReported:      false,
			CheckpointAdvanced: false,
			NextRetryAt:        run.NextRetryAt,
			NextAttempt:        0,
		}, nil
	}

	def, err := l.svcCtx.TaskDefinitions.FindOne(l.ctx, run.TaskKey)
	if err != nil {
		l.Errorf("ReportTaskResult read definition failed, task_key=%s", run.TaskKey)
		return nil, err
	}
	if def == nil {
		return nil, fmt.Errorf("%w: run_id=%d 的 task_key=%s 已无定义，无法按重试策略裁决",
			model.ErrTaskNotFound, run.ID, run.TaskKey)
	}
	now := l.svcCtx.ServerTime()
	toState, nextRetryAt, nextAttempt, err := reportDecision(run.State, in.State, run.Attempt,
		def.MaxAttempts, now, def.RetryBaseSeconds, def.RetryMaxSeconds)
	if err != nil {
		return nil, err
	}
	if toState == model.RunStateRetrying && !model.RetryAllowed(run.Attempt, def.MaxAttempts) {
		// reportDecision 已保证这一条，这里只是把「不退避」的结论钉死。
		return nil, fmt.Errorf("%w: attempt=%d max_attempts=%d", model.ErrInvalidRetryPolicy, run.Attempt, def.MaxAttempts)
	}

	var (
		checkpointAdvanced bool
		final              *model.TaskRun
	)
	checkpoint, checkpointExpected, err := reportCheckpoint(in, run)
	if err != nil {
		return nil, err
	}
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		if checkpoint != nil {
			ok, err := l.svcCtx.Checkpoints.SaveTx(ctx, tx, checkpoint, checkpointExpected)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: task_key=%s scope_key=%s expected_version=%d",
					model.ErrCheckpointConflict, checkpoint.TaskKey, checkpoint.ScopeKey, checkpointExpected)
			}
			checkpointAdvanced = true
		}
		ok, err := l.svcCtx.Runs.ReportTx(ctx, tx, run.ID, in.Owner, in.FenceToken, run.State, toState,
			in.ResultSummary, in.ErrorMessage, nextRetryAt, durationMs(run, now))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: run_id=%d 状态推进未命中（来源状态或栅栏已变化）",
				model.ErrLeaseLost, run.ID)
		}
		if model.IsTerminalRunState(toState) {
			if err := l.finishTerminal(ctx, tx, run, def, toState, in, now); err != nil {
				return err
			}
		}
		row, err := l.svcCtx.Runs.FindOneTx(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		final = row
		return nil
	})
	if err != nil {
		l.Errorf("ReportTaskResult failed, run_id=%d task_key=%s attempt=%d report_state=%s",
			in.RunId, run.TaskKey, run.Attempt, in.State.String())
		return nil, err
	}
	if final == nil {
		return nil, model.ErrRunNotFound
	}
	return &rpc.ReportTaskResultReply{
		Run:                runRecord(final),
		FirstReported:      true,
		CheckpointAdvanced: checkpointAdvanced,
		NextRetryAt:        nextRetryAt,
		NextAttempt:        nextAttempt,
	}, nil
}

// reportCheckpoint 取出请求自带的游标与其 CAS 期望版本。
func reportCheckpoint(in *rpc.ReportTaskResultReq, run *model.TaskRun) (*model.TaskCheckpoint, int64, error) {
	if in.Checkpoint == nil {
		return nil, 0, nil
	}
	c, err := checkpointFromProto(in.Checkpoint)
	if err != nil {
		return nil, 0, err
	}
	// 游标必须属于这次执行的任务：否则一个 run 的结果能把别的任务的游标推走。
	if c.TaskKey != run.TaskKey {
		return nil, 0, fmt.Errorf("%w: checkpoint.task_key=%s 与 run_id=%d 的 %s 不一致",
			model.ErrTaskKeyEmpty, c.TaskKey, run.ID, run.TaskKey)
	}
	if in.Checkpoint.Version < 0 {
		return nil, 0, fmt.Errorf("%w: checkpoint.version=%d 不能为负",
			model.ErrCheckpointConflict, in.Checkpoint.Version)
	}
	return c, in.Checkpoint.Version, nil
}

// finishTerminal 终态收尾：回写任务级最近结果 + 交回任务级租约。
//
// 交回租约而不是等它自然过期，是为了让下一个计划点不必排队到 TTL 结束；
// 交回失败说明锁已被别人拿走（本实例的执行其实已被接管），此时必须报错回滚。
func (l *ReportTaskResultLogic) finishTerminal(
	ctx context.Context, tx sqlx.Session, run *model.TaskRun, def *model.TaskDefinition,
	toState int32, in *rpc.ReportTaskResultReq, now int64,
) error {
	successAt := int64(0)
	if toState == model.RunStateSucceeded {
		successAt = now
	}
	if err := l.svcCtx.TaskDefinitions.MarkResultTx(ctx, tx, run.TaskKey, successAt, in.ErrorMessage); err != nil {
		return err
	}
	lease, err := l.svcCtx.Leases.FindByRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if lease == nil {
		// 没有任务级锁行（例如只靠 run 租约串起来的历史行）：无需交回，不算失败。
		return nil
	}
	ok, err := l.svcCtx.Leases.ReleaseTx(ctx, tx, lease.LeaseKey, in.Owner, in.FenceToken)
	if err != nil {
		return err
	}
	if !ok && lease.OwnerInstance == in.Owner && lease.FenceToken == in.FenceToken {
		return fmt.Errorf("%w: 租约 %s 属于本实例却未能交回", model.ErrLeaseLost, lease.LeaseKey)
	}
	return nil
}
