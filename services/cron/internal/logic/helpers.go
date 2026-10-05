// 本文件是 logic 包的手写扩展（参数校验与纯规则函数），不是 goctl 生成产物。
//
// 分工：这里只放「可以被单测直接调用」的纯函数——游标编解码、租约键、幂等判定、
// 状态机裁决、misfire 计划点计算。SQL 与库表读写一律留在 model（AGENTS.md §4）。

package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 租约键分隔符：与 rpc.LeaseInfo.lease_key 的注释一致（task_key + "/" + scope）。
const leaseKeySeparator = "/"

// leaseKeyOf 计算任务级租约键。scope 为空表示任务级全局互斥。
func leaseKeyOf(taskKey, scope string) string {
	taskKey = strings.TrimSpace(taskKey)
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return taskKey
	}
	return taskKey + leaseKeySeparator + scope
}

// splitScopeFromLeaseKey 从 lease_key 反解 scope（投影/诊断用，找不到分隔符即无分片）。
func splitScopeFromLeaseKey(leaseKey string) (taskKey, scope string) {
	idx := strings.Index(leaseKey, leaseKeySeparator)
	if idx < 0 {
		return leaseKey, ""
	}
	return leaseKey[:idx], leaseKey[idx+1:]
}

// checkLeaseKeyLimit 校验 lease_key 及其两个来源列的长度。
//
// 只看 task_key/scope 各自合规是不够的：lease_key 是把它们拼起来的
// （task_key + "/" + scope），拼接结果才是真正写进 cron_task_lease.lease_key
// VARCHAR(128) 的值。model.MaxScopeBytes 已经按「128-64-1」留出分隔符，这里再把
// 组合长度显式校验一遍，防止有人日后加宽 task_key 而忘了同步。
func checkLeaseKeyLimit(taskKey, scope string) error {
	if err := checkTextLimit("task_key", taskKey, model.MaxTaskKeyBytes); err != nil {
		return err
	}
	if err := checkTextLimit("scope", scope, model.MaxScopeBytes); err != nil {
		return err
	}
	return checkTextLimit("lease_key", leaseKeyOf(taskKey, scope), model.MaxLeaseKeyBytes)
}

// decodeIDCursor 解析「上一页最后一行 id」型游标。
// 空串表示从头开始；非数字、0 或负数都是非法游标（绝不退化成「当作第一页」，
// 否则客户端拼错游标时会以为自己翻到了末页）。
func decodeIDCursor(cursor string) (int64, error) {
	trimmed := strings.TrimSpace(cursor)
	if trimmed == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", model.ErrInvalidCursor, cursor)
	}
	if id <= 0 {
		return 0, fmt.Errorf("%w: %q must be a positive id", model.ErrInvalidCursor, cursor)
	}
	return id, nil
}

// encodeIDCursor 生成下一页游标；0 表示没有更多。
func encodeIDCursor(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// decodeCompositeCursor 解析游标表的 (task_key, scope_key) 复合游标。
func decodeCompositeCursor(cursor string) (string, error) {
	trimmed := strings.TrimSpace(cursor)
	if trimmed == "" {
		return "", nil
	}
	taskKey, _, err := model.SplitCompositeCursor(trimmed)
	if err != nil {
		return "", err
	}
	if taskKey == "" {
		return "", fmt.Errorf("%w: empty task_key segment", model.ErrInvalidCursor)
	}
	return trimmed, nil
}

// requireIdempotencyKey 写接口的幂等键校验（AGENTS.md §5：所有写接口要有幂等设计）。
func requireIdempotencyKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return model.ErrIdempotencyKeyEmpty
	}
	return nil
}

// requireReason 暂停/停用/人工重试等运营动作必须留原因（审计可追溯）。
func requireReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return model.ErrReasonRequired
	}
	return nil
}

// checkTextLimit 文本长度校验，超限即报错而不是静默截断：
// 截断只在「结果摘要/错误原因」这类回填字段里由 model 做，任务参数不能悄悄被切一半。
func checkTextLimit(field, value string, max int) error {
	if max <= 0 || len(value) <= max {
		return nil
	}
	return fmt.Errorf("%w: %s length=%d, max=%d", model.ErrParamsTooLarge, field, len(value), max)
}

// summaryLimit 收敛「执行结果摘要」的入库上限。
//
// 不能直接用 config.Task.MaxResultSummaryBytes：
//   - 配成 0/负数时 checkTextLimit 的 max<=0 分支等于**完全不校验**；
//   - 配得比列宽还大时同样形同虚设，超长文本一路撞到 cron_task_run.result_summary
//     VARCHAR(1024) 的 INSERT 才炸，处理器会把它误读成「上报失败要重试」。
//
// 所以缺省与越界都夹回 model 的列宽上限。
func summaryLimit(configured int) int {
	if configured <= 0 || configured > model.MaxResultSummaryBytes {
		return model.MaxResultSummaryBytes
	}
	return configured
}

// scheduleTypeValue 把 RPC 调度枚举转成落库值；未指定即非法（注册/更新必须显式选一种）。
func scheduleTypeValue(t rpc.ScheduleType) (int32, error) {
	switch t {
	case rpc.ScheduleType_SCHEDULE_TYPE_CRON:
		return model.ScheduleTypeCron, nil
	case rpc.ScheduleType_SCHEDULE_TYPE_INTERVAL:
		return model.ScheduleTypeInterval, nil
	case rpc.ScheduleType_SCHEDULE_TYPE_MANUAL:
		return model.ScheduleTypeManual, nil
	default:
		return 0, fmt.Errorf("%w: schedule_type 未指定或非法(%d)", model.ErrInvalidSchedule, int32(t))
	}
}

// misfirePolicyValue 转换 misfire 策略；未指定时按服务默认 FIRE_ONCE_NOW 处理。
func misfirePolicyValue(p rpc.MisfirePolicy) (int32, error) {
	switch p {
	case rpc.MisfirePolicy_MISFIRE_POLICY_UNSPECIFIED:
		return model.MisfirePolicyFireOnceNow, nil
	case rpc.MisfirePolicy_MISFIRE_POLICY_FIRE_ONCE_NOW:
		return model.MisfirePolicyFireOnceNow, nil
	case rpc.MisfirePolicy_MISFIRE_POLICY_SKIP_TO_NEXT:
		return model.MisfirePolicySkipToNext, nil
	case rpc.MisfirePolicy_MISFIRE_POLICY_FIRE_ALL:
		return model.MisfirePolicyFireAll, nil
	default:
		return 0, fmt.Errorf("%w: misfire_policy=%d", model.ErrInvalidSchedule, int32(p))
	}
}

// taskStateValue 转换任务状态；未指定时按 defaultState 处理（注册语境即 ENABLED）。
func taskStateValue(s rpc.TaskState, defaultState int32) (int32, error) {
	switch s {
	case rpc.TaskState_TASK_STATE_UNSPECIFIED:
		return defaultState, nil
	case rpc.TaskState_TASK_STATE_ENABLED:
		return model.TaskStateEnabled, nil
	case rpc.TaskState_TASK_STATE_PAUSED:
		return model.TaskStatePaused, nil
	case rpc.TaskState_TASK_STATE_DISABLED:
		return model.TaskStateDisabled, nil
	default:
		return 0, fmt.Errorf("cron: invalid task state %d", int32(s))
	}
}

// runStateValue 转换执行状态枚举（ReleaseLease 的 final_state 用）。
func runStateValue(s rpc.RunState) (int32, error) {
	switch s {
	case rpc.RunState_RUN_STATE_SKIPPED:
		return model.RunStateSkipped, nil
	case rpc.RunState_RUN_STATE_CANCELED:
		return model.RunStateCanceled, nil
	case rpc.RunState_RUN_STATE_PENDING, rpc.RunState_RUN_STATE_RUNNING, rpc.RunState_RUN_STATE_RETRYING,
		rpc.RunState_RUN_STATE_SUCCEEDED, rpc.RunState_RUN_STATE_FAILED, rpc.RunState_RUN_STATE_TIMEOUT:
		return 0, fmt.Errorf("%w: release 只能落 SKIPPED/CANCELED，收到 %s",
			model.ErrInvalidFinalState, model.RunStateName(int32(s)))
	default:
		return 0, fmt.Errorf("%w: run_state=%d 未指定", model.ErrInvalidFinalState, int32(s))
	}
}

// triggerTypeValue 转换触发来源枚举；未指定时按 scheduled 处理（到期扫描是唯一自动来源）。
func triggerTypeValue(t rpc.TriggerType, defaultType int32) (int32, error) {
	switch t {
	case rpc.TriggerType_TRIGGER_TYPE_UNSPECIFIED:
		return defaultType, nil
	case rpc.TriggerType_TRIGGER_TYPE_SCHEDULED:
		return model.TriggerTypeScheduled, nil
	case rpc.TriggerType_TRIGGER_TYPE_MANUAL:
		return model.TriggerTypeManual, nil
	case rpc.TriggerType_TRIGGER_TYPE_RETRY:
		return model.TriggerTypeRetry, nil
	case rpc.TriggerType_TRIGGER_TYPE_REPLAY:
		return model.TriggerTypeReplay, nil
	default:
		return 0, fmt.Errorf("cron: invalid trigger_type %d", int32(t))
	}
}

// definitionFromProto 把 RPC 任务定义转成落库行，并补齐服务端默认值。
//
// full=true 用于 RegisterTask（所有调度字段都必须自洽）；
// full=false 用于 UpdateTask（只取可改字段，state/version/时间戳由调用方从库里保留）。
func (l *ServiceDefaults) definitionFromProto(in *rpc.TaskDefinition, full bool) (*model.TaskDefinition, error) {
	if in == nil {
		return nil, errors.New("cron: definition is required")
	}
	d := &model.TaskDefinition{
		TaskKey:            strings.TrimSpace(in.TaskKey),
		Name:               strings.TrimSpace(in.Name),
		Handler:            strings.TrimSpace(in.Handler),
		TaskGroup:          strings.TrimSpace(in.TaskGroup),
		CronExpr:           strings.TrimSpace(in.CronExpr),
		IntervalSeconds:    in.IntervalSeconds,
		Timezone:           strings.TrimSpace(in.Timezone),
		TimeoutSeconds:     in.TimeoutSeconds,
		MaxAttempts:        in.MaxAttempts,
		RetryBaseSeconds:   in.RetryBaseSeconds,
		RetryMaxSeconds:    in.RetryMaxSeconds,
		ConcurrencyLimit:   in.ConcurrencyLimit,
		LeaseTTLSeconds:    in.LeaseTtlSeconds,
		Params:             in.Params,
		SecretRefs:         strings.TrimSpace(in.SecretRefs),
		Owner:              strings.TrimSpace(in.Owner),
		MisfireBackfillLim: in.MisfireBackfillLimit,
	}
	if d.TaskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}
	if err := checkTextLimit("params", d.Params, l.MaxParamsBytes); err != nil {
		return nil, err
	}
	// 逐列按 cron_task_definition 的列宽校验（列宽见 deploy/migrations/cron，
	// 与 model 常量的对齐关系由 model.TestTextColumnWidthMatchesModelLimits 钉住）。
	// 缺了这一步的后果不是「报个难懂的错」而是注册接口 500：超长的 task_key/handler
	// 会一路走到 INSERT 才炸 1406 Data too long，而审计行已在同事务里写好。
	for _, lim := range []struct {
		field string
		value string
		max   int
	}{
		{"secret_refs", d.SecretRefs, model.MaxSecretRefsBytes},
		{"task_key", d.TaskKey, model.MaxTaskKeyBytes},
		{"name", d.Name, model.MaxTaskNameBytes},
		{"handler", d.Handler, model.MaxHandlerBytes},
		{"task_group", d.TaskGroup, model.MaxTaskGroupBytes},
		{"cron_expr", d.CronExpr, model.MaxCronExprBytes},
		{"timezone", d.Timezone, model.MaxTimezoneBytes},
		{"owner", d.Owner, model.MaxOwnerBytes},
	} {
		if err := checkTextLimit(lim.field, lim.value, lim.max); err != nil {
			return nil, err
		}
	}
	if d.TaskGroup == "" && full {
		// 只在注册语境补默认分组：更新语境留空表示「不改」，
		// 这里塞默认值会把已有分组冲掉（见 mergeDefinition 的约定）。
		d.TaskGroup = l.DefaultTaskGroup
	}
	if d.Timezone == "" && full {
		d.Timezone = l.Timezone
	}

	scheduleType, err := scheduleTypeValue(in.ScheduleType)
	if err != nil {
		return nil, err
	}
	d.ScheduleType = scheduleType
	misfire, err := misfirePolicyValue(in.MisfirePolicy)
	if err != nil {
		return nil, err
	}
	if !full && in.MisfirePolicy == rpc.MisfirePolicy_MISFIRE_POLICY_UNSPECIFIED {
		// 更新语境：未显式声明的策略当作「不改」，绝不悄悄把 FIRE_ALL 降级成 FIRE_ONCE_NOW。
		misfire = model.MisfirePolicyUnspecified
	}
	d.MisfirePolicy = misfire
	if full && d.MisfireBackfillLim <= 0 {
		d.MisfireBackfillLim = l.DefaultBackfillLimit
	}

	if !full {
		return d, nil
	}

	// 注册语境：补齐调度/重试/租约的服务端默认值，再做结构自洽校验。
	applyDefinitionDefaults(d, *l)
	if d.LeaseTTLSeconds > model.MaxLeaseTTLSeconds {
		return nil, fmt.Errorf("%w: lease_ttl_seconds=%d, max=%d",
			model.ErrInvalidLeaseTTL, d.LeaseTTLSeconds, model.MaxLeaseTTLSeconds)
	}
	state, err := taskStateValue(in.State, model.TaskStateEnabled)
	if err != nil {
		return nil, err
	}
	if state == model.TaskStateUnspecified {
		state = model.TaskStateEnabled
	}
	d.State = state
	if err := model.ValidateTaskDefinition(d); err != nil {
		return nil, err
	}
	return d, nil
}

// mergeDefinition 把 UpdateTask 请求里的可改字段并入既有行。
//
// task_key、state、version、next_fire_at/last_* 与时间戳都以服务端为准：
// 后台不能「顺手」把停用任务改回启用，状态迁移只能走 Pause/Resume/Disable。
//
// proto3 没有「字段是否被显式设置」的信息，因此约定（README「更新语义」）：
//   - 数值型调度参数 0 一律表示「不改」——这些列的合法值都 >0，
//     0 不可能是有效配置，所以不会被误当成清空；
//   - 文本字段里 params / secret_refs 允许显式清空（空串就是「无参数」），
//     其余（name/handler/group/timezone/cron_expr/owner）空串表示「不改」，
//     否则漏传一个 handler 就会把线上任务改成无 handler 的死定义。
func mergeDefinition(base *model.TaskDefinition, patch *model.TaskDefinition) *model.TaskDefinition {
	merged := *base
	merged.Name = keepIfEmpty(patch.Name, base.Name)
	merged.Handler = keepIfEmpty(patch.Handler, base.Handler)
	merged.TaskGroup = keepIfEmpty(patch.TaskGroup, base.TaskGroup)
	merged.Timezone = keepIfEmpty(patch.Timezone, base.Timezone)
	merged.Owner = keepIfEmpty(patch.Owner, base.Owner)
	merged.Params = patch.Params
	merged.SecretRefs = patch.SecretRefs
	if patch.ScheduleType != model.ScheduleTypeUnspecified {
		merged.ScheduleType = patch.ScheduleType
	}
	// 换调度方式时旧表达式/旧间隔要跟着让位：留着 cron_expr 会让下一次读回时误判。
	if patch.ScheduleType != 0 && patch.ScheduleType != base.ScheduleType {
		merged.CronExpr = patch.CronExpr
		merged.IntervalSeconds = patch.IntervalSeconds
	} else {
		merged.CronExpr = keepIfEmpty(patch.CronExpr, base.CronExpr)
		merged.IntervalSeconds = keepIfPositive(patch.IntervalSeconds, base.IntervalSeconds)
	}
	merged.TimeoutSeconds = keepIfPositive(patch.TimeoutSeconds, base.TimeoutSeconds)
	merged.MaxAttempts = keepIfPositive(patch.MaxAttempts, base.MaxAttempts)
	merged.ConcurrencyLimit = keepIfPositive(patch.ConcurrencyLimit, base.ConcurrencyLimit)
	merged.LeaseTTLSeconds = keepIfPositive(patch.LeaseTTLSeconds, base.LeaseTTLSeconds)
	// 退避参数允许显式改小，但 0 仍然不是合法值（model 要求 base>0 才能重试），
	// 所以这里同样按「0=不改」处理。
	merged.RetryBaseSeconds = keepIfPositive(patch.RetryBaseSeconds, base.RetryBaseSeconds)
	merged.RetryMaxSeconds = keepIfPositive(patch.RetryMaxSeconds, base.RetryMaxSeconds)
	merged.MisfirePolicy = keepIfPositive(patch.MisfirePolicy, base.MisfirePolicy)
	merged.MisfireBackfillLim = keepIfPositive(patch.MisfireBackfillLim, base.MisfireBackfillLim)
	if merged.Name == "" {
		merged.Name = base.TaskKey
	}
	merged.TaskKey = base.TaskKey
	merged.State = base.State
	return &merged
}

// keepIfEmpty 空串表示「不改」。
func keepIfEmpty(patch, base string) string {
	if strings.TrimSpace(patch) == "" {
		return base
	}
	return patch
}

// keepIfPositive 非正数表示「不改」：被调用的字段合法域都是正数。
func keepIfPositive[T int32 | int64](patch, base T) T {
	if patch <= 0 {
		return base
	}
	return patch
}

// ServiceDefaults 是从 config 抽出来的一组默认值，
// 让纯函数（注册/恢复/上报的决策）能脱离 ServiceContext 单测。
type ServiceDefaults struct {
	// Timezone 任务未声明时区时的缺省值（config.Task.DefaultTimezone）。
	Timezone string
	// MaxParamsBytes params 文本上限（config.Task.MaxParamsBytes）。
	MaxParamsBytes int
	// DefaultLeaseTTL 任务未声明 lease_ttl_seconds 时的兜底 TTL（秒）。
	DefaultLeaseTTL int64
	// DefaultBackfillLimit FIRE_ALL 单轮补齐上限的默认值。
	DefaultBackfillLimit int32
	// DefaultTaskGroup 任务未分组时的组名。
	DefaultTaskGroup string
	// PreemptionEnabled 是否允许抢占过期租约（排障冻结窗口会关掉）。
	PreemptionEnabled bool
}

// planResumeFireAt 计算任务从 PAUSED 恢复后的首个计划时刻（MisfirePolicy 的唯一实现处）。
//
//   - FIRE_ONCE_NOW：把暂停期间所有过期点合并成一次补跑（指针指向最早未跑点，
//     若已无法回溯就指向 now）；
//   - SKIP_TO_NEXT：丢弃过期点，指针指向 now 之后的第一个计划点；
//   - FIRE_ALL：指针指向最早未跑点，由到期扫描逐个向前推进，
//     单轮补齐上限由 MisfireBackfillLimit 约束（扫描侧每轮最多取该多个点）。
//
// anchor 取 last_fire_at（最后一次产生执行记录的计划时刻）；从未跑过时用 now。
func planResumeFireAt(d *model.TaskDefinition, defaults ServiceDefaults, now int64) (int64, error) {
	if d == nil {
		return 0, model.ErrTaskNotFound
	}
	if d.ScheduleType == model.ScheduleTypeManual {
		return 0, nil
	}
	anchor := d.LastFireAt
	if anchor <= 0 {
		anchor = now - 1
	}
	next, err := model.NextFireAfter(d, defaults.Timezone, anchor)
	if err != nil {
		return 0, err
	}
	switch d.MisfirePolicy {
	case model.MisfirePolicySkipToNext:
		return model.NextFireAfter(d, defaults.Timezone, now)
	case model.MisfirePolicyFireAll, model.MisfirePolicyFireOnceNow:
		// 计划点仍在未来（暂停期没跨过任何一个点）：按原计划继续。
		if next > now {
			return next, nil
		}
		if d.MisfirePolicy == model.MisfirePolicyFireOnceNow {
			// 合并补跑：立即跑一次，之后的点按正常节奏推进。
			return now, nil
		}
		return next, nil
	default:
		return 0, fmt.Errorf("%w: misfire_policy=%d", model.ErrInvalidSchedule, d.MisfirePolicy)
	}
}

// classifyExistingRun 判定「同一计划时刻已有一行执行记录」时的领取结果。
//
// 幂等重入（同一实例再次 claim）返回 ALREADY_CLAIMED 并带回原栅栏令牌，
// 让调用方能继续心跳；他人持有且未过期返回 ALREADY_OWNED；
// 已终态返回 ACQUIRED=false 语义的 ALREADY_CLAIMED + 说明文案（绝不重复跑）。
func classifyExistingRun(existing *model.TaskRun, owner string, now int64) (rpc.LeaseOutcome, string) {
	if existing == nil {
		return rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, ""
	}
	switch {
	case model.IsTerminalRunState(existing.State):
		return rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_CLAIMED, fmt.Sprintf(
			"run %d 已终结于 %s，同计划时刻不重复执行（需要再跑请走 RetryRun/TriggerTask）",
			existing.ID, model.RunStateName(existing.State))
	case existing.State == model.RunStateRunning:
		if existing.LeaseOwner == owner {
			return rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_CLAIMED, fmt.Sprintf(
				"本实例已持有 run %d，继续用 fence_token=%d 心跳", existing.ID, existing.FenceToken)
		}
		if model.LeaseExpired(existing.LeaseExpireAt, now) {
			return rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, ""
		}
		return rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED, fmt.Sprintf(
			"run %d 由 %s 持有至 %d", existing.ID, existing.LeaseOwner, existing.LeaseExpireAt)
	case existing.State == model.RunStateRetrying:
		if model.LeaseExpired(existing.LeaseExpireAt, now) || existing.LeaseExpireAt == 0 {
			return rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, ""
		}
		return rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED, fmt.Sprintf(
			"run %d 正在退避，下次可执行时间 %d", existing.ID, existing.NextRetryAt)
	default: // PENDING：已 claim 但还没 Start
		if existing.LeaseOwner == owner || existing.LeaseOwner == "" {
			return rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, ""
		}
		if model.LeaseExpired(existing.LeaseExpireAt, now) {
			return rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, ""
		}
		return rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED, fmt.Sprintf(
			"run %d 已被 %s claim（PENDING）", existing.ID, existing.LeaseOwner)
	}
}

// reportDecision 是 ReportTaskResult 的状态机裁决（纯函数，可表驱动单测）。
//
// REPORT_STATE_FAILED 时由服务端按重试策略决定 RETRYING 还是 FAILED：
// 契约里没有「处理器声明 retryable」的字段（见 README「契约缺口」），
// 因此这里只看 attempt 与 max_attempts 的关系。
func reportDecision(
	fromState int32, reportState rpc.ReportState, attempt, maxAttempts int32,
	now int64, baseSeconds, maxSeconds int32,
) (toState int32, nextRetryAt int64, nextAttempt int32, err error) {
	switch reportState {
	case rpc.ReportState_REPORT_STATE_SUCCEEDED:
		toState = model.RunStateSucceeded
	case rpc.ReportState_REPORT_STATE_TIMEOUT:
		toState = model.RunStateTimeout
	case rpc.ReportState_REPORT_STATE_CANCELED:
		toState = model.RunStateCanceled
	case rpc.ReportState_REPORT_STATE_SKIPPED:
		toState = model.RunStateSkipped
	case rpc.ReportState_REPORT_STATE_FAILED:
		if model.RetryAllowed(attempt, maxAttempts) {
			toState = model.RunStateRetrying
			nextRetryAt = model.NextRetryAt(now, attempt, baseSeconds, maxSeconds)
			nextAttempt = attempt + 1
		} else {
			toState = model.RunStateFailed
		}
	default:
		return 0, 0, 0, fmt.Errorf("%w: report_state=%d", model.ErrInvalidFinalState, int32(reportState))
	}
	if !model.CanTransition(fromState, toState) {
		return 0, 0, 0, fmt.Errorf("%w: %s -> %s", model.ErrStateTransition,
			model.RunStateName(fromState), model.RunStateName(toState))
	}
	return toState, nextRetryAt, nextAttempt, nil
}

// auditDetail 生成审计用的变更摘要 JSON。
// 只放调度面事实与原因文本，禁止写入 params 原文之外的敏感数据与密钥（AGENTS.md §4）。
func auditDetail(fields map[string]any) string {
	if len(fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		keyJSON, err := json.Marshal(k)
		if err != nil {
			continue
		}
		valJSON, err := json.Marshal(fields[k])
		if err != nil {
			valJSON = []byte(`"unserializable"`)
		}
		sb.Write(keyJSON)
		sb.WriteByte(':')
		sb.Write(valJSON)
	}
	sb.WriteByte('}')
	return sb.String()
}

// defaultMisfireBackfillLimit FIRE_ALL 单轮补齐计划点的兜底上限。
// 配置里没有独立项（见 README「契约缺口」），这里给一个「足够补一晚、又不至于把
// 下游打爆」的保守值：任务自己可以在 misfire_backfill_limit 里声明更大值。
const defaultMisfireBackfillLimit int32 = 5

// applyDefinitionDefaults 补齐「调用方没给」的调度字段。
// 只填零值、不覆盖显式声明，保证注册与更新走同一套兜底规则。
func applyDefinitionDefaults(d *model.TaskDefinition, defaults ServiceDefaults) {
	if d.Name == "" {
		d.Name = d.TaskKey
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = 1
	}
	if d.ConcurrencyLimit <= 0 {
		d.ConcurrencyLimit = 1
	}
	if d.MisfireBackfillLim <= 0 {
		d.MisfireBackfillLim = defaults.DefaultBackfillLimit
	}
	if d.LeaseTTLSeconds <= 0 {
		d.LeaseTTLSeconds = int32(defaults.DefaultLeaseTTL)
	}
}

// operatorOf 取操作人；后台没传时退化为实例标识，
// 保证审计里永远能回答「这个动作是谁做的」（AGENTS.md §8）。
func operatorOf(operator string, svcCtx *svc.ServiceContext) string {
	if trimmed := strings.TrimSpace(operator); trimmed != "" {
		return trimmed
	}
	return svcCtx.WorkerID()
}

// scheduleSummary 把调度三组字段压成一行可读文本，供审计 detail 使用。
// 目的：回答「这次把计划改成了什么」，不需要人工再比对 schedule_type 数值。
func scheduleSummary(d *model.TaskDefinition) string {
	switch d.ScheduleType {
	case model.ScheduleTypeCron:
		return fmt.Sprintf("cron(%q, tz=%s)", d.CronExpr, d.Timezone)
	case model.ScheduleTypeInterval:
		return fmt.Sprintf("interval(%ds, tz=%s)", d.IntervalSeconds, d.Timezone)
	case model.ScheduleTypeManual:
		return "manual"
	default:
		return fmt.Sprintf("unknown(%d)", d.ScheduleType)
	}
}

// defaultsOf 从运行时上下文提取服务端默认值。
// 纯规则函数只吃这个结构而不是 *svc.ServiceContext，目的是能在不连库的情况下单测。
func defaultsOf(svcCtx *svc.ServiceContext) ServiceDefaults {
	task := svcCtx.Config.Task
	lease := svcCtx.Config.Lease
	defaultTTL := lease.DefaultTTLSeconds
	if defaultTTL <= 0 {
		defaultTTL = int64(model.MinLeaseTTLSeconds)
	}
	maxParams := task.MaxParamsBytes
	if maxParams <= 0 || maxParams > model.MaxParamsBytes {
		maxParams = model.MaxParamsBytes
	}
	return ServiceDefaults{
		Timezone:             firstNonEmpty(task.DefaultTimezone, "UTC"),
		MaxParamsBytes:       maxParams,
		DefaultLeaseTTL:      defaultTTL,
		DefaultBackfillLimit: defaultMisfireBackfillLimit,
		DefaultTaskGroup:     defaultTaskGroupName,
		PreemptionEnabled:    lease.PreemptionEnabled,
	}
}

// defaultTaskGroupName 未分组任务的组名：健康度列表里要能区分「没分组」和「分组叫别的」。
const defaultTaskGroupName = "default"

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// notRunnableReply 组装 NOT_RUNNABLE 响应：任务不存在/已暂停/未注册 handler 都走这里，
// 明确带 message，绝不返回 ACQUIRED 零值冒充「可以跑」。
func notRunnableReply(message string) *rpc.AcquireLeaseReply {
	return &rpc.AcquireLeaseReply{
		Outcome: rpc.LeaseOutcome_LEASE_OUTCOME_NOT_RUNNABLE,
		Message: message,
	}
}

// stateTransition 描述一次「任务状态迁移 + 审计」的写操作。
//
// Pause/Resume/Disable 三个动作的骨架完全一样（读定义 → 判当前状态 → 乐观锁 CAS →
// 可选改写调度指针 → 同事务写审计），抽成一个结构体避免三份互相漂移的实现；
// 差异全部通过字段表达，尤其 planNextFireAt 让「恢复时重算指针」这种依赖库内最新行
// 的决策留在事务里，不会读到过期快照。
type stateTransition struct {
	taskKey        string
	action         string
	operator       string
	reason         string
	traceID        string
	toState        int32
	allowedFrom    []int32
	extra          map[string]any
	planNextFireAt func(cur *model.TaskDefinition) (int64, error)
}

// apply 执行状态迁移。返回 changed=false 表示目标状态已达成（幂等重入，不写审计）。
//
// 并发正确性：状态判定与 CAS 在同一个事务里完成，SetState 带 expected_version，
// 未命中即 ErrVersionConflict，调用方必须重读后重试，绝不静默覆盖别人的变更。
func (t *stateTransition) apply(
	ctx context.Context, svcCtx *svc.ServiceContext,
) (changed bool, auditID int64, current *model.TaskDefinition, err error) {
	err = svcCtx.Transact(ctx, func(ctx context.Context, tx sqlx.Session) error {
		cur, err := svcCtx.TaskDefinitions.FindOneTx(ctx, tx, t.taskKey)
		if err != nil {
			return err
		}
		if cur == nil {
			return model.ErrTaskNotFound
		}
		current = cur
		if cur.State == t.toState {
			// 幂等重入：目标态已达成，不写审计（写了反而制造「反复暂停」的假痕迹）。
			changed = false
			return nil
		}
		if !stateAllowed(t.allowedFrom, cur.State) {
			return fmt.Errorf("%w: %s -> %s 不是合法操作，当前状态为 %s",
				model.ErrStateTransition, model.TaskStateName(cur.State),
				model.TaskStateName(t.toState), model.TaskStateName(cur.State))
		}
		nextFireAt := cur.NextFireAt
		if t.planNextFireAt != nil {
			v, err := t.planNextFireAt(cur)
			if err != nil {
				return err
			}
			nextFireAt = v
		}
		// allowedFrom 可能含多个来源状态，SetState 的单值守卫交给 version 承担。
		ok, err := svcCtx.TaskDefinitions.SetStateTx(ctx, tx, t.taskKey,
			model.TaskStateUnspecified, t.toState, cur.Version, t.operator)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: task_key=%s version=%d", model.ErrVersionConflict, t.taskKey, cur.Version)
		}
		// SetState 只在「暂停」时清零指针；停用/恢复要显式落指针，否则到期扫描会读到旧值。
		if nextFireAt != cur.NextFireAt {
			if err := svcCtx.TaskDefinitions.SetNextFireAtTx(ctx, tx, t.taskKey, nextFireAt); err != nil {
				return err
			}
		}
		fields := map[string]any{
			"reason":           t.reason,
			"expected_version": cur.Version,
			"next_fire_at":     nextFireAt,
			"prev_next_fire":   cur.NextFireAt,
		}
		for k, v := range t.extra {
			fields[k] = v
		}
		audit := &model.TaskAudit{
			TaskKey:   t.taskKey,
			Action:    t.action,
			FromState: model.TaskStateName(cur.State),
			ToState:   model.TaskStateName(t.toState),
			Operator:  t.operator,
			Detail:    auditDetail(fields),
			TraceID:   t.traceID,
		}
		if err := svcCtx.Audits.Insert(ctx, tx, audit); err != nil {
			return err
		}
		auditID = audit.ID
		changed = true
		return nil
	})
	if err != nil {
		return false, 0, current, err
	}
	return changed, auditID, current, nil
}

// stateAllowed 判定的来源状态集合是否覆盖当前状态（在事务内对库里的最新行做，
// 因此传入的 state 一定是刚读到的值，不存在用陈旧快照放行的问题）。
func stateAllowed(allowed []int32, state int32) bool {
	for _, a := range allowed {
		if a == state {
			return true
		}
	}
	return false
}

// durationMs 服务端算执行耗时（毫秒）：结束时刻（本次上报/释放的 now）减 started。
// 不信客户端上报的耗时，否则实例时钟漂移会把统计污染。未开始的行（PENDING）记 0。
func durationMs(run *model.TaskRun, now int64) int64 {
	if run == nil || run.StartedAt <= 0 {
		return 0
	}
	seconds := now - run.StartedAt
	if seconds < 0 {
		return 0
	}
	return seconds * int64(time.Second/time.Millisecond)
}

// operationReply 组装任务操作响应。
//
// 事务里的快照在提交后已经过期（version 也 +1 了），所以这里必须回读落库结果，
// 不能把「改之前」的行当作变更结果返回给调用方。
func operationReply(
	ctx context.Context, svcCtx *svc.ServiceContext, taskKey string, changed bool, auditID int64,
) (*rpc.TaskOperationReply, error) {
	row, err := svcCtx.TaskDefinitions.FindOne(ctx, taskKey)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, model.ErrTaskNotFound
	}
	return &rpc.TaskOperationReply{
		Definition: taskDefinitionInfo(row),
		Changed:    changed,
		AuditId:    auditID,
	}, nil
}
