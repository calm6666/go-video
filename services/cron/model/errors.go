package model

import (
	"errors"
	"fmt"
)

// cron 域错误。
// 说明：cron 只做调度与审计，不碰业务数据；所有错误都必须是可观测、可重试或
// 可人工介入的，禁止吞掉错误后返回「执行成功」（AGENTS.md §9）。
var (
	// ErrNotImplemented 本轮只交付契约与装配，业务 logic 由第二轮实现。
	// logic 层直接返回该哨兵，绝不返回假成功的零值响应（AGENTS.md §9）。
	ErrNotImplemented = errors.New("cron: not implemented")

	// ErrTaskKeyEmpty task_key 为空：任务定义与执行记录都以它为主索引键。
	ErrTaskKeyEmpty = errors.New("cron: task_key is required")
	// ErrIdempotencyKeyEmpty 写接口缺少幂等键（AGENTS.md §5）。
	ErrIdempotencyKeyEmpty = errors.New("cron: idempotency_key is required")
	// ErrTaskNotFound 任务定义不存在。
	ErrTaskNotFound = errors.New("cron: task definition not found")
	// ErrTaskAlreadyExists RegisterTask 命中已存在的 task_key 且定义不一致。
	ErrTaskAlreadyExists = errors.New("cron: task_key already registered")
	// ErrVersionConflict expected_version 与服务端当前版本不一致（乐观锁失败）。
	ErrVersionConflict = errors.New("cron: expected_version mismatch, reload and retry")
	// ErrInvalidSchedule 调度配置不自洽：CRON 缺表达式、INTERVAL 间隔 <= 0 等。
	ErrInvalidSchedule = errors.New("cron: invalid schedule definition")
	// ErrInvalidRetryPolicy max_attempts < 1 或退避参数非正。
	ErrInvalidRetryPolicy = errors.New("cron: invalid retry policy")
	// ErrInvalidLeaseTTL 租约 TTL 不在允许区间（过小会导致频繁抢占抖动）。
	ErrInvalidLeaseTTL = errors.New("cron: lease ttl out of range")
	// ErrInvalidPageLimit page_size 超过服务端上限。
	ErrInvalidPageLimit = errors.New("cron: page_size exceeds limit")
	// ErrInvalidCursor 游标格式非法。
	ErrInvalidCursor = errors.New("cron: invalid cursor")
	// ErrRunNotFound 执行记录不存在。
	ErrRunNotFound = errors.New("cron: task run not found")
	// ErrRunNotRetryable 只有终态 FAILED/TIMEOUT 的执行可以人工重试。
	ErrRunNotRetryable = errors.New("cron: run is not retryable, only failed/timeout runs can be retried")
	// ErrInvalidFinalState ReleaseLease/ReportTaskResult 携带了不允许的状态。
	ErrInvalidFinalState = errors.New("cron: invalid final run state")
	// ErrStateTransition 执行记录状态机非法迁移。
	ErrStateTransition = errors.New("cron: illegal run state transition")
	// ErrLeaseLost 栅栏令牌不匹配：租约已被其它实例抢占，调用方必须放弃本次执行。
	ErrLeaseLost = errors.New("cron: lease lost, fence token mismatch; abort this run and do not write downstream")
	// ErrLeaseNotHeld 当前实例并不持有该租约，无法续租/上报。
	ErrLeaseNotHeld = errors.New("cron: lease is not held by this instance")
	// ErrCheckpointConflict SaveCheckpoint 的 expected_version 与当前游标版本不一致。
	ErrCheckpointConflict = errors.New("cron: checkpoint version conflict")
	// ErrParamsTooLarge params/result_summary 超过配置上限。
	ErrParamsTooLarge = errors.New("cron: params exceed size limit")
	// ErrReasonRequired 暂停/停用/重放等操作必须给出原因（审计要求）。
	ErrReasonRequired = errors.New("cron: reason is required")
	// ErrDownstreamNotConfigured 任务处理器依赖的下游 RPC 未配置。
	// 注意：cron 只能通过领域 RPC 推进业务状态，缺失配置时任务失败而不是绕过 RPC 写库。
	ErrDownstreamNotConfigured = errors.New("cron: downstream rpc client is not configured")
	// ErrHandlerNotRegistered 注册表里没有该 handler 名（进程与 DB 定义不一致）。
	ErrHandlerNotRegistered = errors.New("cron: task handler is not registered")
	// ErrInvalidTimeRange 查询时间窗自相矛盾（from > to）。
	ErrInvalidTimeRange = errors.New("cron: invalid time range, from must not be greater than to")
	// ErrRunParamsUnsupported TriggerTask 的按次参数覆盖无法落地：
	// cron_task_run 没有 params 列，AcquireLease 也不会把参数带给 worker，
	// 因此契约上的 params 字段目前无法在不改表的前提下持久化。
	// 这里显式失败，绝不「收下参数但丢掉」假装按覆盖值跑了（AGENTS.md §9）。
	ErrRunParamsUnsupported = errors.New("cron: per-run params override is not persisted by the current schema, " +
		"leave params empty or change it on the task definition with UpdateTask")
	// ErrLeaseOwnerRequired AcquireLease/RenewLease/ReleaseLease 缺少实例标识：
	// 租约的可抢占性完全依赖 owner 唯一，空 owner 会让互斥失效。
	ErrLeaseOwnerRequired = errors.New("cron: lease owner is required (hostname-pid-random)")
	// ErrPlannedAtRequired 计划时刻缺失：(task_key, planned_at) 是执行的幂等身份。
	ErrPlannedAtRequired = errors.New("cron: planned_at is required for the fire identity")
	// ErrRetryNotDue 该重试号尚未进入退避窗口，不能被领取。
	ErrRetryNotDue = errors.New("cron: retry attempt is not due yet")
	// ErrAttemptNotRetryable attempt>1 必须对应服务端已有的 RETRYING 执行。
	ErrAttemptNotRetryable = errors.New("cron: attempt is not backed by a retrying run")
	// ErrRunAlreadyFinished 执行已进终态，不能再领取或改写。
	ErrRunAlreadyFinished = errors.New("cron: run already reached a terminal state")
	// ErrPreemptionDisabled 排障冻结窗口内禁止抢占他人租约。
	ErrPreemptionDisabled = errors.New("cron: lease preemption is disabled by configuration")
)

// 任务定义状态（cron_task_definition.state，与 rpc TaskState 数值一致）。
const (
	TaskStateUnspecified = int32(0)
	TaskStateEnabled     = int32(1)
	TaskStatePaused      = int32(2)
	TaskStateDisabled    = int32(3)
)

// 调度方式（cron_task_definition.schedule_type）。
const (
	ScheduleTypeUnspecified = int32(0)
	ScheduleTypeCron        = int32(1)
	ScheduleTypeInterval    = int32(2)
	ScheduleTypeManual      = int32(3)
)

// 错过的计划点策略（cron_task_definition.misfire_policy）。
const (
	MisfirePolicyUnspecified = int32(0)
	MisfirePolicyFireOnceNow = int32(1)
	MisfirePolicySkipToNext  = int32(2)
	MisfirePolicyFireAll     = int32(3)
)

// 执行记录状态（cron_task_run.state）。
const (
	RunStateUnspecified = int32(0)
	RunStatePending     = int32(1)
	RunStateRunning     = int32(2)
	RunStateRetrying    = int32(3)
	RunStateSucceeded   = int32(4)
	RunStateFailed      = int32(5)
	RunStateTimeout     = int32(6)
	RunStateCanceled    = int32(7)
	RunStateSkipped     = int32(8)
)

// 触发来源（cron_task_run.trigger_type）。
const (
	TriggerTypeUnspecified = int32(0)
	TriggerTypeScheduled   = int32(1)
	TriggerTypeManual      = int32(2)
	TriggerTypeRetry       = int32(3)
	TriggerTypeReplay      = int32(4)
)

// 任务审计动作（cron_task_audit.action）。
const (
	AuditActionRegister = "register"
	AuditActionUpdate   = "update"
	AuditActionPause    = "pause"
	AuditActionResume   = "resume"
	AuditActionDisable  = "disable"
	AuditActionTrigger  = "trigger"
	AuditActionRetry    = "retry"
	AuditActionReplay   = "replay"
)

// runTransitions 描述执行记录的合法状态推进。
// PENDING 是 claim 成功但 worker 尚未开始的瞬时状态；终态不再出边，
// 人工重放/重试都是「新增 attempt 行」而不是把终态改回运行中，
// 这样历史执行轨迹不可篡改（AGENTS.md §8 的审计要求）。
var runTransitions = map[int32]map[int32]struct{}{
	RunStatePending: {
		RunStateRunning:  {},
		RunStateSkipped:  {},
		RunStateCanceled: {},
		RunStateTimeout:  {},
		RunStateFailed:   {},
	},
	RunStateRunning: {
		RunStateSucceeded: {},
		RunStateRetrying:  {},
		RunStateFailed:    {},
		RunStateTimeout:   {},
		RunStateCanceled:  {},
		RunStateSkipped:   {},
	},
	RunStateRetrying: {
		RunStateRunning:  {}, // 退避到期后由同一计划时刻的新 attempt 承接
		RunStateFailed:   {},
		RunStateTimeout:  {},
		RunStateCanceled: {},
	},
	RunStateSucceeded: {},
	RunStateFailed:    {},
	RunStateTimeout:   {},
	RunStateCanceled:  {},
	RunStateSkipped:   {},
}

// CanTransition 校验执行记录从 from 到 to 的推进是否合法。
func CanTransition(from, to int32) bool {
	targets, ok := runTransitions[from]
	if !ok {
		return false
	}
	_, ok = targets[to]
	return ok
}

// IsTerminalRunState 判断是否终态（终态不可回退，只允许追加 attempt 行）。
func IsTerminalRunState(state int32) bool {
	switch state {
	case RunStateSucceeded, RunStateFailed, RunStateTimeout, RunStateCanceled, RunStateSkipped:
		return true
	default:
		return false
	}
}

// ValidateTaskDefinition 校验任务定义自洽性（调度、重试、租约三组参数）。
// 这里只做「能不能调度」的结构校验，业务前置条件由处理器第二轮负责。
func ValidateTaskDefinition(d *TaskDefinition) error {
	if d == nil {
		return errors.New("cron: nil task definition")
	}
	if d.TaskKey == "" {
		return ErrTaskKeyEmpty
	}
	if d.Handler == "" {
		return errors.New("cron: handler is required")
	}
	switch d.ScheduleType {
	case ScheduleTypeCron:
		if d.CronExpr == "" {
			return fmt.Errorf("%w: cron_expr is required for SCHEDULE_TYPE_CRON", ErrInvalidSchedule)
		}
	case ScheduleTypeInterval:
		if d.IntervalSeconds <= 0 {
			return fmt.Errorf("%w: interval_seconds must be positive", ErrInvalidSchedule)
		}
	case ScheduleTypeManual:
		// 手动任务不需要调度参数。
	default:
		return fmt.Errorf("%w: schedule_type=%d", ErrInvalidSchedule, d.ScheduleType)
	}
	if d.MaxAttempts < 1 {
		return fmt.Errorf("%w: max_attempts=%d", ErrInvalidRetryPolicy, d.MaxAttempts)
	}
	if d.MaxAttempts > 1 && d.RetryBaseSeconds <= 0 {
		return fmt.Errorf("%w: retry_base_seconds must be positive when retries are enabled", ErrInvalidRetryPolicy)
	}
	if d.TimeoutSeconds <= 0 {
		return fmt.Errorf("%w: timeout_seconds must be positive", ErrInvalidLeaseTTL)
	}
	if d.LeaseTTLSeconds <= 0 {
		return fmt.Errorf("%w: lease_ttl_seconds must be positive", ErrInvalidLeaseTTL)
	}
	// 心跳/续租窗口必须显著小于 TTL，否则崩溃实例会造成任务长时间不可抢占。
	if d.LeaseTTLSeconds < d.TimeoutSeconds && d.LeaseTTLSeconds < MinLeaseTTLSeconds {
		return fmt.Errorf("%w: lease_ttl_seconds=%d, minimum=%d", ErrInvalidLeaseTTL,
			d.LeaseTTLSeconds, MinLeaseTTLSeconds)
	}
	if d.ConcurrencyLimit < 1 {
		return errors.New("cron: concurrency_limit must be positive")
	}
	return nil
}

// 租约与分页的服务端硬下限/上限：配置可以更严，但不能突破这里的常量，
// 避免误配置把调度退化成「无 TTL 的锁」或「每 tick 扫全表」。
const (
	// MinLeaseTTLSeconds 租约 TTL 下限：小于该值时进程抖动就会反复抢占，得不偿失。
	MinLeaseTTLSeconds = 30
	// MaxLeaseTTLSeconds 租约 TTL 上限：超过一天说明任务本身该拆分成多段。
	MaxLeaseTTLSeconds = 86400
	// DefaultPageSize 列表接口默认条数。
	DefaultPageSize = 20
	// MaxPageSize 列表接口上限，防止深分页拖垮数据库。
	MaxPageSize = 200
	// MaxParamsBytes params JSON 文本上限。
	MaxParamsBytes = 4096
	// MaxResultSummaryBytes 执行结果摘要上限（摘要不承载事件正文）。
	MaxResultSummaryBytes = 1024
	// MaxLastErrorBytes 错误原因入库前的截断长度。
	MaxLastErrorBytes = 512
)

// NormalizePageSize 把请求分页大小收敛到 [1, MaxPageSize]；0 表示用默认值。
func NormalizePageSize(requested int32) (int, error) {
	if requested <= 0 {
		return DefaultPageSize, nil
	}
	if requested > MaxPageSize {
		return 0, fmt.Errorf("%w: page_size=%d, max=%d", ErrInvalidPageLimit, requested, MaxPageSize)
	}
	return int(requested), nil
}

// 各文本列的宽度上限，与 deploy/migrations/cron/*.sql 的 VARCHAR 长度逐一对齐。
//
// 为什么要在 model 里钉一份（而不是只在 logic 里写数字）：
//   - 注册/更新/写游标这类入口必须「在服务端就把超限请求拒掉」，否则 MySQL 严格模式
//     会把它变成 1406 写入错误——调用方看到的是 500，而不是可纠正的参数错误；
//   - migration_parity_test.go 会拿这份常量与建表语句比对，加宽/收窄列时必然要同步改这里，
//     两处不可能再各说各话。
const (
	MaxTaskKeyBytes    = 64  // cron_task_definition.task_key / run.task_key / lease.task_key / checkpoint.task_key / audit.task_key
	MaxTaskNameBytes   = 128 // cron_task_definition.name
	MaxHandlerBytes    = 128 // cron_task_definition.handler
	MaxTaskGroupBytes  = 64  // cron_task_definition.task_group
	MaxCronExprBytes   = 64  // cron_task_definition.cron_expr
	MaxTimezoneBytes   = 64  // cron_task_definition.timezone
	MaxOwnerBytes      = 64  // cron_task_definition.owner
	MaxOperatorBytes   = 64  // 各表 operator
	MaxSecretRefsBytes = 512 // cron_task_definition.secret_refs
	MaxTraceIDBytes    = 64  // 各表 trace_id
	MaxLeaseKeyBytes   = 128 // cron_task_lease.lease_key 列宽（组合键上限，见 logic.leaseKeyOf）
	// MaxScopeBytes 比 lease_key 列窄 1 字节：lease_key = task_key + "/" + scope，
	// 只有 scope <= 128 - 64 - 1 = 63 才能保证「两个各自合规的入参拼出的键一定写得进」。
	MaxScopeBytes       = 63  // cron_task_lease.scope
	MaxLeaseOwnerBytes  = 128 // cron_task_run.lease_owner / cron_task_lease.owner_instance
	MaxScopeKeyBytes    = 128 // cron_task_checkpoint.scope_key
	MaxValueStrBytes    = 255 // cron_task_checkpoint.value_str
	MaxAuditActionBytes = 32  // cron_task_audit.action
	MaxAuditStateBytes  = 24  // cron_task_audit.from_state / to_state
)
