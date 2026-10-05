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

type AcquireLeaseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAcquireLeaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AcquireLeaseLogic {
	return &AcquireLeaseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 抢占某个计划时刻（同事务写租约与执行记录）。
//
// 这是 cron 并发正确性的核心，全部在一个 TransactCtx 里完成：
// 「插/读执行记录 → 并发闸门 → 抢任务级租约 → 用租约的栅栏令牌启动执行 → 推进调度指针」
// 任何一步失败整体回滚，绝不会留下「有租约没记录」或「有记录没租约」的中间态。
//
// 互斥为什么成立（两层）：
//  1. uniq_fire_attempt(task_key, planned_at, attempt) 保证同一计划时刻的一次尝试
//     只有一行，抢插失败的实例只能读回赢家的行；
//  2. cron_task_lease 的 SELECT ... FOR UPDATE 保证「同一任务级锁」同时只有一个持有者，
//     过期才能被抢占，且抢占即递增 fence_token —— 被顶掉的旧实例随后续租/上报必然
//     拿到 ErrLeaseLost，两个实例同时推进同一任务在数据库层就不可能出现。
func (l *AcquireLeaseLogic) AcquireLease(in *rpc.AcquireLeaseReq) (*rpc.AcquireLeaseReply, error) {
	if in == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}
	if strings.TrimSpace(in.Owner) == "" {
		return nil, model.ErrLeaseOwnerRequired
	}
	if in.PlannedAt <= 0 {
		return nil, model.ErrPlannedAtRequired
	}
	if in.Attempt < 0 {
		return nil, fmt.Errorf("%w: attempt=%d 不能为负", model.ErrAttemptNotRetryable, in.Attempt)
	}
	scope := strings.TrimSpace(in.Scope)
	// lease_key 由 task_key 与 scope 拼成，超长只会在事务内的 INSERT 才炸（1406），
	// 报错点离根因很远，还白烧一次事务与一条审计行：在入口按列宽拒掉。
	if err := checkLeaseKeyLimit(taskKey, scope); err != nil {
		return nil, err
	}
	// owner 会同时落 cron_task_lease.owner_instance(128)、cron_task_run.lease_owner(128)
	// 与 cron_task_run.operator(64)（见 resolveFire），约束取三者里最紧的 64。
	if err := checkTextLimit("owner", strings.TrimSpace(in.Owner), model.MaxOperatorBytes); err != nil {
		return nil, err
	}
	if err := checkTextLimit("trace_id", in.TraceId, model.MaxTraceIDBytes); err != nil {
		return nil, err
	}
	triggerType, err := triggerTypeValue(in.TriggerType, model.TriggerTypeScheduled)
	if err != nil {
		return nil, err
	}

	def, err := l.svcCtx.TaskDefinitions.FindOne(l.ctx, taskKey)
	if err != nil {
		l.Errorf("AcquireLease read definition failed, task_key=%s", taskKey)
		return nil, err
	}
	if def == nil {
		return notRunnableReply(fmt.Sprintf("%s 没有任务定义，调度事实源缺失，拒绝领取", taskKey)), nil
	}
	if err := l.checkRunnable(def, in, triggerType); err != nil {
		return notRunnableReply(err.Error()), nil
	}
	ttl, err := l.svcCtx.LeaseTTL(orDefaultTTL(in.TtlSeconds, def.LeaseTTLSeconds))
	if err != nil {
		return nil, err
	}

	res := &acquireResult{}
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		return l.claimInTx(ctx, tx, in, def, triggerType, ttl, scope, res)
	})
	if err != nil {
		l.Errorf("AcquireLease failed, task_key=%s planned_at=%d attempt=%d owner=%s",
			taskKey, in.PlannedAt, in.Attempt, in.Owner)
		return nil, err
	}
	if res.reply != nil {
		return res.reply, nil
	}
	// 事务提交成功却没给出结论：属于装配错误，必须显式失败而不是回零值。
	l.Errorf("AcquireLease committed without outcome, task_key=%s planned_at=%d", taskKey, in.PlannedAt)
	return nil, model.ErrLeaseNotHeld
}

// acquireResult 承载事务内的结论：事务回调只能返回 error，结论用结构体带出来。
type acquireResult struct {
	reply *rpc.AcquireLeaseReply
}

// checkRunnable 判定「这个任务此刻能不能跑」，不能跑时给出原因（NOT_RUNNABLE 不是错误）。
//
// PAUSED + 人工触发（MANUAL/RETRY）是刻意放行的组合：TriggerTask 允许「先手动验证
// 再恢复调度」，RetryRun 也允许在暂停期排队，否则人点完「重试」会得到一个永远领不到的行。
// DISABLED 一律拒绝：它是「不再被支持」的终态声明。
func (l *AcquireLeaseLogic) checkRunnable(def *model.TaskDefinition, in *rpc.AcquireLeaseReq, triggerType int32) error {
	if _, err := l.svcCtx.Registry.Resolve(def.Handler); err != nil {
		return fmt.Errorf("%w: 本进程注册表没有 handler=%s，拒绝领取以免任务被静默跳过", err, def.Handler)
	}
	switch def.State {
	case model.TaskStateEnabled:
		return nil
	case model.TaskStatePaused:
		if triggerType == model.TriggerTypeManual || triggerType == model.TriggerTypeRetry {
			l.Infof("cron/acquire: task_key=%s 处于 PAUSED，因触发来源 %s 属人工动作而放行",
				def.TaskKey, triggerTypeName(triggerType))
			return nil
		}
		return fmt.Errorf("%w: %s 已暂停，只接受人工触发来源（MANUAL/RETRY），当前 %s",
			model.ErrStateTransition, def.TaskKey, triggerTypeName(triggerType))
	default:
		return fmt.Errorf("%w: %s 处于 %s 状态，拒绝领取", model.ErrStateTransition,
			def.TaskKey, model.TaskStateName(def.State))
	}
}

// triggerTypeName 把落库的触发来源还原成人读文本，只用于日志与拒绝原因。
func triggerTypeName(v int32) string {
	switch v {
	case model.TriggerTypeScheduled:
		return "SCHEDULED"
	case model.TriggerTypeManual:
		return "MANUAL"
	case model.TriggerTypeRetry:
		return "RETRY"
	case model.TriggerTypeReplay:
		return "REPLAY"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", v)
	}
}

// orDefaultTTL 请求没给 TTL 时用任务定义的 TTL。
func orDefaultTTL(requested, declared int32) int32 {
	if requested > 0 {
		return requested
	}
	return declared
}

// claimInTx 在事务里完成「排队 → 闸门 → 抢锁 → 启动 → 推进指针」。
//
// res.reply 三种来源：
//   - 已被别人持有/已终结：ALREADY_OWNED / ALREADY_CLAIMED（幂等结论，不是错误）；
//   - 并发上限：CONCURRENCY_LIMIT，并把这次的 PENDING 行落成 SKIPPED（有记录、无副作用）；
//   - 成功：ACQUIRED + run_id + fence_token + lease_expire_at。
func (l *AcquireLeaseLogic) claimInTx(
	ctx context.Context, tx sqlx.Session, in *rpc.AcquireLeaseReq, def *model.TaskDefinition,
	triggerType int32, ttl int64, scope string, res *acquireResult,
) error {
	now := l.svcCtx.ServerTime()
	run, created, err := l.resolveFire(ctx, tx, in, def, triggerType, now, res)
	if err != nil {
		return err
	}
	if run == nil {
		return model.ErrRunNotFound
	}
	if !created {
		if res.reply != nil {
			// resolveFire 已经给出幂等结论（自己重入 / 他人持有 / 已终结）。
			return nil
		}
		return l.takeoverOrExplain(ctx, tx, in, run, ttl, scope, now, res)
	}
	if limit := int64(def.ConcurrencyLimit); limit > 0 {
		running, err := l.svcCtx.Runs.CountRunningTx(ctx, tx, def.TaskKey)
		if err != nil {
			return err
		}
		if running >= limit {
			return l.skipForConcurrency(ctx, tx, run, in, limit, running, res)
		}
	}
	return l.acquireAndStart(ctx, tx, in, def, run, ttl, scope, now, res)
}

// resolveFire 定位（必要时新建）本次要领取的执行记录。
//
// attempt 只允许两种取值：
//   - 0/1：首次尝试，插入 (task_key, planned_at, 1)；
//   - >1：必须是「上一号正在 RETRYING 且退避已到期」的下一号，否则报
//     ErrAttemptNotRetryable / ErrRetryNotDue —— 不能靠猜一个更大的 attempt 绕过退避。
func (l *AcquireLeaseLogic) resolveFire(
	ctx context.Context, tx sqlx.Session, in *rpc.AcquireLeaseReq, def *model.TaskDefinition,
	triggerType int32, now int64, res *acquireResult,
) (*model.TaskRun, bool, error) {
	attempt := in.Attempt
	if attempt <= 1 {
		attempt = 1
	} else {
		prev, err := l.svcCtx.Runs.FindByFireTx(ctx, tx, in.TaskKey, in.PlannedAt, attempt-1)
		if err != nil {
			return nil, false, err
		}
		if prev == nil || prev.State != model.RunStateRetrying {
			return nil, false, fmt.Errorf("%w: task_key=%s planned_at=%d 找不到 attempt=%d 的 RETRYING 执行，"+
				"重试号必须由服务端的上报结果产生", model.ErrAttemptNotRetryable, in.TaskKey, in.PlannedAt, attempt-1)
		}
		if prev.NextRetryAt > now {
			return nil, false, fmt.Errorf("%w: task_key=%s attempt=%d 要等到 %d 才可领取",
				model.ErrRetryNotDue, in.TaskKey, attempt, prev.NextRetryAt)
		}
		// 人工重试会先追加一行 PENDING（trigger_type=RETRY），此时沿用那一行的来源，
		// 不要把 RetryRun 排队的行改写成自动退避的痕迹。
		if prev.TriggerType == model.TriggerTypeRetry {
			triggerType = model.TriggerTypeRetry
		}
	}
	run := &model.TaskRun{
		TaskKey:     def.TaskKey,
		PlannedAt:   in.PlannedAt,
		Attempt:     attempt,
		TriggerType: triggerType,
		State:       model.RunStatePending,
		Operator:    in.Owner,
		TraceID:     in.TraceId,
	}
	stored, created, err := l.svcCtx.Runs.ClaimForFire(ctx, tx, run)
	if err != nil {
		return nil, false, err
	}
	if created {
		return stored, true, nil
	}
	outcome, message := classifyExistingRun(stored, in.Owner, now)
	if outcome != rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED {
		return stored, false, l.settle(res, outcome, stored, in, message)
	}
	return stored, false, nil
}

// settle 组装一个「没有取得锁」的结论（幂等重入或他人持有）。
func (l *AcquireLeaseLogic) settle(
	res *acquireResult, outcome rpc.LeaseOutcome, run *model.TaskRun, in *rpc.AcquireLeaseReq, message string,
) error {
	reply := &rpc.AcquireLeaseReply{
		Outcome:    outcome,
		RunId:      run.ID,
		FenceToken: run.FenceToken,
		Message:    message,
		ServerTime: l.svcCtx.ServerTime(),
	}
	if run.LeaseExpireAt > 0 && !model.LeaseExpired(run.LeaseExpireAt, l.svcCtx.ServerTime()) {
		reply.LeaseExpireAt = run.LeaseExpireAt
	}
	res.reply = reply
	return nil
}

// skipForConcurrency 并发闸门命中时把这次的 PENDING 行就地记为 SKIPPED。
//
// 「静默丢弃计划点」是调度系统最难查的故障：这里留下明确的行与原因，
// 运营在 ListTaskRuns 里能看到「这个计划点因为并发上限没跑」。
func (l *AcquireLeaseLogic) skipForConcurrency(
	ctx context.Context, tx sqlx.Session, run *model.TaskRun, in *rpc.AcquireLeaseReq,
	limit, running int64, res *acquireResult,
) error {
	reason := fmt.Sprintf("并发上限 %d 已满（当前 RUNNING %d），本次领取记为 SKIPPED", limit, running)
	ok, err := l.svcCtx.Runs.ReportTx(ctx, tx, run.ID, "", 0, model.RunStatePending,
		model.RunStateSkipped, "", reason, 0, 0)
	if err != nil {
		return err
	}
	if !ok {
		// 行状态被并发改走：不能假装写成功，让调用方重试。
		return fmt.Errorf("%w: 并发闸门回收 PENDING 行失败，run_id=%d", model.ErrStateTransition, run.ID)
	}
	res.reply = &rpc.AcquireLeaseReply{
		Outcome:    rpc.LeaseOutcome_LEASE_OUTCOME_CONCURRENCY_LIMIT,
		RunId:      run.ID,
		Message:    reason,
		ServerTime: l.svcCtx.ServerTime(),
	}
	return nil
}

// acquireAndStart 抢任务级租约并用返回的栅栏令牌启动执行。
func (l *AcquireLeaseLogic) acquireAndStart(
	ctx context.Context, tx sqlx.Session, in *rpc.AcquireLeaseReq, def *model.TaskDefinition,
	run *model.TaskRun, ttl int64, scope string, now int64, res *acquireResult,
) error {
	acq, leaseKey, err := l.grabTaskLease(ctx, tx, def.TaskKey, scope, in.Owner, ttl, run.ID)
	if err != nil {
		return err
	}
	if acq == nil {
		return l.settle(res, rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED, run, in, fmt.Sprintf(
			"租约 %s 仍由其他实例持有，本次未取得锁", leaseKey))
	}
	if err := l.startWithLease(ctx, tx, in, run, acq.Lease.FenceToken, ttl); err != nil {
		return err
	}
	if err := l.advancePointer(ctx, tx, def, run); err != nil {
		return err
	}
	res.reply = acquiredReply(run, acq, now)
	if acq.TakenOver {
		l.Infof("cron/acquire: task_key=%s 抢占过期租约，fence_token=%d takeover_count=%d",
			def.TaskKey, acq.Lease.FenceToken, acq.Lease.TakeoverCount)
	}
	return nil
}

// grabTaskLease 抢任务级租约。未取得时返回 (nil, leaseKey, nil)，由调用方给幂等结论。
func (l *AcquireLeaseLogic) grabTaskLease(
	ctx context.Context, tx sqlx.Session, taskKey, scope, owner string, ttl, runID int64,
) (*model.LeaseAcquire, string, error) {
	leaseKey := leaseKeyOf(taskKey, scope)
	acq, err := l.svcCtx.Leases.AcquireTx(ctx, tx, leaseKey, taskKey, scope, owner, ttl, runID)
	if err != nil {
		return nil, leaseKey, err
	}
	if !acq.Acquired {
		return nil, leaseKey, nil
	}
	return acq, leaseKey, nil
}

// startWithLease 用租约给出的栅栏令牌把执行记录推进到 RUNNING。
// 命中条件是硬要求：CAS 未命中说明行已被并发改写，绝不能回 ACQUIRED。
func (l *AcquireLeaseLogic) startWithLease(
	ctx context.Context, tx sqlx.Session, in *rpc.AcquireLeaseReq, run *model.TaskRun,
	newFence, ttl int64,
) error {
	if run.State == model.RunStatePending {
		ok, err := l.svcCtx.Runs.StartTx(ctx, tx, run.ID, in.Owner, newFence, ttl)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: run_id=%d 已不在 PENDING，启动失败（不伪造领取成功）",
				model.ErrStateTransition, run.ID)
		}
		return nil
	}
	ok, err := l.svcCtx.Runs.TakeoverTx(ctx, tx, run.ID, in.Owner, run.FenceToken, newFence, ttl,
		fmt.Sprintf("租约过期被 %s 接管（原持有者 %s）", in.Owner, run.LeaseOwner))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: run_id=%d 抢占条件未命中（栅栏或状态已变化）", model.ErrLeaseLost, run.ID)
	}
	return nil
}

// acquiredReply 组装 ACQUIRED 响应，run_id/fence/过期时间全部取库里的真值。
func acquiredReply(run *model.TaskRun, acq *model.LeaseAcquire, now int64) *rpc.AcquireLeaseReply {
	reply := &rpc.AcquireLeaseReply{
		Outcome:       rpc.LeaseOutcome_LEASE_OUTCOME_ACQUIRED,
		RunId:         run.ID,
		FenceToken:    acq.Lease.FenceToken,
		LeaseExpireAt: acq.Lease.ExpireAt,
		ServerTime:    now,
	}
	if acq.TakenOver {
		reply.Message = fmt.Sprintf("接管已过期租约（原持有者 %s），fence_token 已递增", acq.Lease.OwnerInstance)
	}
	return reply
}

// advancePointer 只在「领取的正是当前计划点」时推进调度指针。
//
// 手动补跑（planned_at 是历史时刻）与重试号都不能改动指针，否则会把未来的计划点
// 提前吃掉。MarkFired 自带 state=ENABLED 与 last_fire_at<=planned_at 守卫。
func (l *AcquireLeaseLogic) advancePointer(
	ctx context.Context, tx sqlx.Session, def *model.TaskDefinition, run *model.TaskRun,
) error {
	if def.State != model.TaskStateEnabled || run.PlannedAt != def.NextFireAt {
		return nil
	}
	next, err := model.NextFireAfter(def, defaultsOf(l.svcCtx).Timezone, run.PlannedAt)
	if err != nil {
		return err
	}
	return l.svcCtx.TaskDefinitions.MarkFiredTx(ctx, tx, def.TaskKey, run.PlannedAt, next)
}

// takeoverOrExplain 处理「同一计划时刻已有一行、但 classifyExistingRun 说可以再试」的情形。
//
// 为什么抢占是「就地接管」而不是把旧行改 TIMEOUT 再补一行：
//   - 改 TIMEOUT 会让「这个计划点跑了几次」的统计虚高，而实际一次都没重跑；
//   - 接管后旧实例的续租/上报会因栅栏不一致失败（ErrLeaseLost），副作用仍只有一份。
//
// 接管必须同时拿到任务级租约，所以先走 Acquire（它顶掉过期持有者并递增 fence_token），
// 再用新栅栏改写 run 的租约三元组；两步同事务，失败整体回滚。
//
// RETRYING 不在这里接管：退避中的执行必须用「下一个 attempt 号」重新排队，
// 直接把它改回 RUNNING 会让 attempt 计数与 next_retry_at 失去意义。
func (l *AcquireLeaseLogic) takeoverOrExplain(
	ctx context.Context, tx sqlx.Session, in *rpc.AcquireLeaseReq, run *model.TaskRun,
	ttl int64, scope string, now int64, res *acquireResult,
) error {
	if run.State == model.RunStateRetrying {
		if run.NextRetryAt > now {
			return fmt.Errorf("%w: task_key=%s planned_at=%d attempt=%d 退避到 %d 才可再次执行",
				model.ErrRetryNotDue, run.TaskKey, run.PlannedAt, run.Attempt, run.NextRetryAt)
		}
		return fmt.Errorf("%w: task_key=%s planned_at=%d 的 attempt=%d 已进入退避，"+
			"请用 attempt=%d 重新领取（重试号只能由服务端追加）",
			model.ErrAttemptNotRetryable, run.TaskKey, run.PlannedAt, run.Attempt, run.Attempt+1)
	}
	if !defaultsOf(l.svcCtx).PreemptionEnabled {
		res.reply = &rpc.AcquireLeaseReply{
			Outcome: rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED,
			RunId:   run.ID,
			Message: fmt.Sprintf("%v：本实例 %s 不能顶掉 run %d 的过期持有者 %s",
				model.ErrPreemptionDisabled, in.Owner, run.ID, run.LeaseOwner),
			ServerTime: now,
		}
		return nil
	}
	leaseKey := leaseKeyOf(run.TaskKey, scope)
	acq, _, err := l.grabTaskLease(ctx, tx, run.TaskKey, scope, in.Owner, ttl, run.ID)
	if err != nil {
		return err
	}
	if acq == nil {
		return l.settle(res, rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED, run, in, fmt.Sprintf(
			"租约 %s 仍由其他实例持有，本次未取得锁", leaseKey))
	}
	if err := l.startWithLease(ctx, tx, in, run, acq.Lease.FenceToken, ttl); err != nil {
		return err
	}
	res.reply = acquiredReply(run, acq, now)
	return nil
}
