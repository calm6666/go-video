// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// cron RPC ↔ 管理后台投影 + 调用上下文装配。
//
// 与 conv_audit.go / conv_opsconfig.go 同一套职责边界（AGENTS.md §4/§5），但 cron 有两点差异：
//  1. cron 契约里没有 CallContext 消息，写接口的归因由三个字段承载：
//     operator（网关用 AdminPermission 会话身份渲染成 "gateway/admin:<admin_id>"，
//     不接受客户端声明）、idempotency_key（proto 注释必填，网关拦住空值）、trace_id（透传）。
//     受保护路由拿不到会话身份时一律 fail-closed，绝不退化成匿名写入；
//  2. cron 的列表接口是游标分页，page_size 的上限与越界判定由 cron 的 svcCtx.PageSize 负责
//     （越界回 ErrInvalidPageLimit），网关只做「非负」这一条传输层校验并原样透传，
//     避免「网关静默截断成 100、服务端以为你要 500 条」这种两边各说一半的口径。
//
// 网关不复算的领域规则（全部在 services/cron）：调度参数自洽性（ValidateTaskDefinition）、
// 处理器注册与串行/重试上限（Registry.ValidateDefinition）、expected_version 是否冲突、
// 租约所有权与栅栏令牌、游标推进的单调性、任务状态机合法性。
// 服务端记账字段（task_id / version / next_fire_at / last_fire_at / last_success_at /
// last_error / ctime / mtime / operator）由 cron 维护，请求里出现即留痕并清空——
// 让客户端伪造「上次成功时间」比多一次往返严重得多。

package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	cronrpc "go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// cronOperatorPrefix 是网关写进 cron operator 字段的身份前缀。
// 与 audit/operation 的 caller_service 口径一致（服务名，不是实例地址），
// 后面接会话里的 admin_id，保证 cron_task_audit.operator 能回溯到人。
const cronOperatorPrefix = "gateway/admin:"

// cronMaxCursorLen 是游标字符串的传输层长度上限。
// 游标内容（task_key、run_id、task_key+\x1f+scope_key）由 cron 解释，
// 网关只挡住明显异常的长度，避免把任意大的 blob 拼进 SQL 条件里。
const cronMaxCursorLen = 256

// errCronServiceNotConfigured：未配置 CronRPC 时 cron 域路由一律返回它。
// 不退化成伪造空列表——那会让后台把「下游没接」读成「没有定时任务」，
// 进而误判「补偿任务全都没跑」。
var errCronServiceNotConfigured = errors.New("cron service not configured")

// errCronSessionRequired：受 AdminPermission 保护的写路由拿不到会话身份。
var errCronSessionRequired = errors.New("gateway/admin: admin session identity required")

// errCronRequestMissing：请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景，出现即说明调用方漏装了参数。
var errCronRequestMissing = errors.New("gateway/admin: request body required")

// cronEnum 约束投影表可接受的枚举类型（protoc 生成的枚举底层都是 int32）。
type cronEnum interface {
	~int32
}

// 以下映射表显式列出每一个枚举值，故意不用 range Xxx_name 自动生成：
// 新增 proto 枚举值时 cron_admin_logic_test.go 的覆盖率断言会失败，
// 强制先确认后台展示口径（而不是让新值静默落到「未知」）。
var cronTaskStateToAPI = map[cronrpc.TaskState]int32{
	cronrpc.TaskState_TASK_STATE_UNSPECIFIED: 0,
	cronrpc.TaskState_TASK_STATE_ENABLED:     1,
	cronrpc.TaskState_TASK_STATE_PAUSED:      2,
	cronrpc.TaskState_TASK_STATE_DISABLED:    3,
}

var cronScheduleTypeToAPI = map[cronrpc.ScheduleType]int32{
	cronrpc.ScheduleType_SCHEDULE_TYPE_UNSPECIFIED: 0,
	cronrpc.ScheduleType_SCHEDULE_TYPE_CRON:        1,
	cronrpc.ScheduleType_SCHEDULE_TYPE_INTERVAL:    2,
	cronrpc.ScheduleType_SCHEDULE_TYPE_MANUAL:      3,
}

var cronMisfirePolicyToAPI = map[cronrpc.MisfirePolicy]int32{
	cronrpc.MisfirePolicy_MISFIRE_POLICY_UNSPECIFIED:   0,
	cronrpc.MisfirePolicy_MISFIRE_POLICY_FIRE_ONCE_NOW: 1,
	cronrpc.MisfirePolicy_MISFIRE_POLICY_SKIP_TO_NEXT:  2,
	cronrpc.MisfirePolicy_MISFIRE_POLICY_FIRE_ALL:      3,
}

var cronRunStateToAPI = map[cronrpc.RunState]int32{
	cronrpc.RunState_RUN_STATE_UNSPECIFIED: 0,
	cronrpc.RunState_RUN_STATE_PENDING:     1,
	cronrpc.RunState_RUN_STATE_RUNNING:     2,
	cronrpc.RunState_RUN_STATE_RETRYING:    3,
	cronrpc.RunState_RUN_STATE_SUCCEEDED:   4,
	cronrpc.RunState_RUN_STATE_FAILED:      5,
	cronrpc.RunState_RUN_STATE_TIMEOUT:     6,
	cronrpc.RunState_RUN_STATE_CANCELED:    7,
	cronrpc.RunState_RUN_STATE_SKIPPED:     8,
}

var cronTriggerTypeToAPI = map[cronrpc.TriggerType]int32{
	cronrpc.TriggerType_TRIGGER_TYPE_UNSPECIFIED: 0,
	cronrpc.TriggerType_TRIGGER_TYPE_SCHEDULED:   1,
	cronrpc.TriggerType_TRIGGER_TYPE_MANUAL:      2,
	cronrpc.TriggerType_TRIGGER_TYPE_RETRY:       3,
	cronrpc.TriggerType_TRIGGER_TYPE_REPLAY:      4,
}

var (
	cronTaskStateFromAPI     = invertCronEnum(cronTaskStateToAPI)
	cronScheduleTypeFromAPI  = invertCronEnum(cronScheduleTypeToAPI)
	cronMisfirePolicyFromAPI = invertCronEnum(cronMisfirePolicyToAPI)
	cronRunStateFromAPI      = invertCronEnum(cronRunStateToAPI)
	cronTriggerTypeFromAPI   = invertCronEnum(cronTriggerTypeToAPI)
)

// invertCronEnum 由「proto→后台」表派生「后台→proto」表，保证两侧同源不会漂移。
func invertCronEnum[V cronEnum](src map[V]int32) map[int32]V {
	dst := make(map[int32]V, len(src))
	for v, n := range src {
		dst[n] = v
	}
	return dst
}

// cronEnumToAPI 投影 proto→后台。未登记的编号原样透传并留痕：
// 把它压成 0 会让后台把「cron 新增的状态」显示成「未指定」，比报出真实编号更危险
// （运营会以为任务卡在初始态）。
func cronEnumToAPI[V cronEnum](v V, table map[V]int32, kind string) int32 {
	if n, ok := table[v]; ok {
		return n
	}
	logx.Errorf("gateway/admin/cron: unknown %s %d projected as-is", kind, int32(v))
	return int32(v)
}

// cronEnumFromAPI 校验后台→proto 的取值：0 表示「未指定」（查询语境＝全部，
// 注册语境＝由服务端取默认值），非 0 必须是已登记的枚举，否则直接报错。
// 拒绝未知编号而不是静默转成 0：打错一个 state=9 会被「全部」语义吞掉，
// 后台看到空列表就无法区分「没有数据」和「参数写错了」。
func cronEnumFromAPI[V cronEnum](v int32, table map[int32]V, kind string) (V, error) {
	var zero V
	got, ok := table[v]
	if !ok {
		return zero, fmt.Errorf("gateway/admin: unknown %s %d", kind, v)
	}
	return got, nil
}

// cronTaskState 转换任务定义状态（0 合法：注册时由 cron 落库为 ENABLED）。
func cronTaskState(v int32) (cronrpc.TaskState, error) {
	return cronEnumFromAPI(v, cronTaskStateFromAPI, "task state")
}

// cronRunStateFilter 转换执行记录状态过滤值（0 = 全部）。
func cronRunStateFilter(v int32) (cronrpc.RunState, error) {
	return cronEnumFromAPI(v, cronRunStateFromAPI, "run state")
}

// cronMisfirePolicy 转换错过计划点策略（0 = 未指定，由 cron 取默认）。
func cronMisfirePolicy(v int32) (cronrpc.MisfirePolicy, error) {
	return cronEnumFromAPI(v, cronMisfirePolicyFromAPI, "misfire policy")
}

// cronScheduleType 转换调度方式；注册与修改都必须显式指定，
// 因此这里额外拒绝 0：没有调度方式的定义既不能到期也不能被 Trigger 补跑，
// cron 的 ValidateTaskDefinition 同样会拒绝（两侧口径一致，不会出现假成功）。
func cronScheduleType(v int32) (cronrpc.ScheduleType, error) {
	got, err := cronEnumFromAPI(v, cronScheduleTypeFromAPI, "schedule type")
	if err != nil {
		return 0, err
	}
	if got == cronrpc.ScheduleType_SCHEDULE_TYPE_UNSPECIFIED {
		return 0, errors.New("gateway/admin: schedule_type required (1=cron, 2=interval, 3=manual)")
	}
	return got, nil
}

// cronOperator 渲染写接口的 operator。cron 契约里 operator 是自由文本，
// 因此身份只能来自中间件会话，不看请求体：受保护路由缺会话身份即拒绝。
func cronOperator(ctx context.Context) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errCronSessionRequired
	}
	if err := requireOperatorID(id.AdminID); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%d", cronOperatorPrefix, id.AdminID), nil
}

// cronPageSize 只做非负校验：0 = 用 cron 的默认页大小，上限与越界由服务端判定。
func cronPageSize(ps int32) error {
	if ps < 0 {
		return errors.New("gateway/admin: page_size must be >= 0")
	}
	return nil
}

// cronCursor 校验游标的传输层形态：空串表示第一页，非空不得超长。
// 游标语义（上一页最后一行的 task_key / run_id / 复合键）由 cron 解析，网关不预判。
func cronCursor(cursor string) error {
	if len(cursor) > cronMaxCursorLen {
		return fmt.Errorf("gateway/admin: cursor too long (%d > %d)", len(cursor), cronMaxCursorLen)
	}
	return nil
}

// cronTimestamp 校验 Unix 秒非负（0 在契约里表示「服务端当前时间/不限」，是合法值）。
func cronTimestamp(field string, v int64) error {
	if v < 0 {
		return fmt.Errorf("gateway/admin: %s must be >= 0", field)
	}
	return nil
}

// cronTimeWindow 校验时间窗：两端非负，且两端都给定时不得反向。
// 跨度上限、是否需要收窄维度都由 cron 判定，网关不复算。
func cronTimeWindow(from, to int64) error {
	if err := cronTimestamp("time window from", from); err != nil {
		return err
	}
	if err := cronTimestamp("time window to", to); err != nil {
		return err
	}
	if from > 0 && to > 0 && from > to {
		return errors.New("gateway/admin: time window from must be <= to")
	}
	return nil
}

// cronVersion 校验乐观锁/CAS 版本号非负（0 在部分契约里表示「新建/不校验」）。
func cronVersion(v int64) error {
	if v < 0 {
		return errors.New("gateway/admin: version must be >= 0")
	}
	return nil
}

// cronRequiredVersion 用于 UpdateTask：库里 version 默认 1，0 只能是「没读到版本」，
// 带着它去更新等于放弃乐观锁，会出现后写覆盖前写的静默丢失。
func cronRequiredVersion(v int64) error {
	if v <= 0 {
		return errors.New("gateway/admin: expected_version required (read the current version first)")
	}
	return nil
}

// cronPositiveID 校验主键存在（run_id 之类，0 表示客户端漏传）。
func cronPositiveID(field string, id int64) error {
	if id <= 0 {
		return fmt.Errorf("gateway/admin: %s required", field)
	}
	return nil
}

// cronScheduleFromAPI 装配调度相关字段，集中做枚举转换。
func cronScheduleFromAPI(d types.CronTaskDefinition) (*cronrpc.TaskDefinition, error) {
	scheduleType, err := cronScheduleType(d.ScheduleType)
	if err != nil {
		return nil, err
	}
	misfire, err := cronMisfirePolicy(d.MisfirePolicy)
	if err != nil {
		return nil, err
	}
	return &cronrpc.TaskDefinition{
		Name:                 d.Name,
		Handler:              d.Handler,
		TaskGroup:            d.TaskGroup,
		ScheduleType:         scheduleType,
		CronExpr:             d.CronExpr,
		IntervalSeconds:      d.IntervalSeconds,
		Timezone:             d.Timezone,
		TimeoutSeconds:       d.TimeoutSeconds,
		MaxAttempts:          d.MaxAttempts,
		RetryBaseSeconds:     d.RetryBaseSeconds,
		RetryMaxSeconds:      d.RetryMaxSeconds,
		ConcurrencyLimit:     d.ConcurrencyLimit,
		LeaseTtlSeconds:      d.LeaseTtlSeconds,
		MisfirePolicy:        misfire,
		MisfireBackfillLimit: d.MisfireBackfillLimit,
		Params:               d.Params,
		SecretRefs:           d.SecretRefs,
		Owner:                d.Owner,
	}, nil
}

// cronDropServerOwned 拦住客户端对服务端记账字段的声明：留痕后由调用方丢弃。
// 这里不报错是因为后台表单常把整行读回来的定义原样提交，报错会让「改一个字段」
// 变成不可能操作；但伪造痕迹必须能从日志里查到。
func cronDropServerOwned(ctx context.Context, action, taskKey string, d types.CronTaskDefinition) {
	if d.TaskId != 0 || d.Version != 0 || d.NextFireAt != 0 || d.LastFireAt != 0 ||
		d.LastSuccessAt != 0 || d.Ctime != 0 || d.Mtime != 0 || d.LastError != "" || d.Operator != "" {
		logx.WithContext(ctx).Errorf(
			"gateway/admin/cron: %s %s claimed server-owned fields task_id=%d version=%d next_fire_at=%d "+
				"last_fire_at=%d last_success_at=%d ctime=%d mtime=%d operator=%q dropped",
			action, taskKey, d.TaskId, d.Version, d.NextFireAt, d.LastFireAt, d.LastSuccessAt,
			d.Ctime, d.Mtime, d.Operator)
	}
}

// cronDefinitionForRegister 装配注册请求的任务定义。
// 必填口径来自 proto RegisterTaskReq 注释（task_key/name/handler/schedule）；
// 调度自洽性与 state 缺省值（0→ENABLED）由 cron 判定。
func cronDefinitionForRegister(ctx context.Context, d types.CronTaskDefinition) (*cronrpc.TaskDefinition, error) {
	if err := requireNonEmpty("definition.task_key", d.TaskKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("definition.name", d.Name); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("definition.handler", d.Handler); err != nil {
		return nil, err
	}
	state, err := cronTaskState(d.State)
	if err != nil {
		return nil, err
	}
	cronDropServerOwned(ctx, "register", d.TaskKey, d)
	base, err := cronScheduleFromAPI(d)
	if err != nil {
		return nil, err
	}
	base.TaskKey = d.TaskKey
	base.State = state
	return base, nil
}

// cronDefinitionForUpdate 装配修改请求的任务定义。
// task_key 以路由参数为准（两处不一致时拒绝，避免「改 A 的表单落到 B」）；
// state 不在本接口生效（.api/proto 均声明以服务端为准），因此强制为 UNSPECIFIED；
// 定义级字段（name/handler/schedule）沿用注册口径的必填要求。
func cronDefinitionForUpdate(ctx context.Context, taskKey string, d types.CronTaskDefinition) (*cronrpc.TaskDefinition, error) {
	if err := requireNonEmpty("task_key", taskKey); err != nil {
		return nil, err
	}
	if d.TaskKey != "" && d.TaskKey != taskKey {
		return nil, fmt.Errorf("gateway/admin: definition.task_key %q conflicts with task_key %q", d.TaskKey, taskKey)
	}
	if err := requireNonEmpty("definition.name", d.Name); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("definition.handler", d.Handler); err != nil {
		return nil, err
	}
	if d.State != int32(cronrpc.TaskState_TASK_STATE_UNSPECIFIED) {
		logx.WithContext(ctx).Errorf(
			"gateway/admin/cron: update %s claimed state=%d which is not changeable here, dropped", taskKey, d.State)
	}
	cronDropServerOwned(ctx, "update", taskKey, d)
	base, err := cronScheduleFromAPI(d)
	if err != nil {
		return nil, err
	}
	base.TaskKey = taskKey
	base.State = cronrpc.TaskState_TASK_STATE_UNSPECIFIED
	return base, nil
}

// cronCheckpointForSave 装配独立推进游标的请求。
// version 与 operator 由服务端维护（.api 注释），客户端声明一律留痕并覆盖，
// 否则后台可以伪造「某个游标是谁推进的」。
func cronCheckpointForSave(ctx context.Context, c types.CronCheckpoint, operator string) (*cronrpc.Checkpoint, error) {
	if err := requireNonEmpty("checkpoint.task_key", c.TaskKey); err != nil {
		return nil, err
	}
	if c.Version != 0 || c.Operator != "" || c.Ctime != 0 || c.Mtime != 0 {
		logx.WithContext(ctx).Errorf(
			"gateway/admin/cron: save checkpoint %s claimed version=%d operator=%q ctime=%d mtime=%d, server values win",
			c.TaskKey, c.Version, c.Operator, c.Ctime, c.Mtime)
	}
	return &cronrpc.Checkpoint{
		TaskKey:  c.TaskKey,
		ScopeKey: c.ScopeKey,
		Value:    c.Value,
		ValueStr: c.ValueStr,
		Version:  0, // CAS 版本只由 expected_version 表达
		Operator: operator,
	}, nil
}

// --- cron.v1 → 后台投影 ---

// cronTaskToAPI 投影任务定义。nil（未命中）投影成零值而不是 panic，
// 由调用方的 found 字段表达「不存在」，后台因此能区分空值和缺字段。
func cronTaskToAPI(d *cronrpc.TaskDefinition) types.CronTaskDefinition {
	if d == nil {
		return types.CronTaskDefinition{}
	}
	return types.CronTaskDefinition{
		TaskId:               d.GetTaskId(),
		TaskKey:              d.GetTaskKey(),
		Name:                 d.GetName(),
		Handler:              d.GetHandler(),
		TaskGroup:            d.GetTaskGroup(),
		ScheduleType:         cronEnumToAPI(d.GetScheduleType(), cronScheduleTypeToAPI, "schedule type"),
		CronExpr:             d.GetCronExpr(),
		IntervalSeconds:      d.GetIntervalSeconds(),
		Timezone:             d.GetTimezone(),
		TimeoutSeconds:       d.GetTimeoutSeconds(),
		MaxAttempts:          d.GetMaxAttempts(),
		RetryBaseSeconds:     d.GetRetryBaseSeconds(),
		RetryMaxSeconds:      d.GetRetryMaxSeconds(),
		ConcurrencyLimit:     d.GetConcurrencyLimit(),
		LeaseTtlSeconds:      d.GetLeaseTtlSeconds(),
		MisfirePolicy:        cronEnumToAPI(d.GetMisfirePolicy(), cronMisfirePolicyToAPI, "misfire policy"),
		MisfireBackfillLimit: d.GetMisfireBackfillLimit(),
		Params:               d.GetParams(),
		SecretRefs:           d.GetSecretRefs(),
		State:                cronEnumToAPI(d.GetState(), cronTaskStateToAPI, "task state"),
		NextFireAt:           d.GetNextFireAt(),
		LastFireAt:           d.GetLastFireAt(),
		LastSuccessAt:        d.GetLastSuccessAt(),
		LastError:            d.GetLastError(),
		Version:              d.GetVersion(),
		Owner:                d.GetOwner(),
		Operator:             d.GetOperator(),
		Ctime:                d.GetCtime(),
		Mtime:                d.GetMtime(),
	}
}

func cronTasksToAPI(list []*cronrpc.TaskDefinition) []types.CronTaskDefinition {
	out := make([]types.CronTaskDefinition, 0, len(list))
	for _, item := range list {
		out = append(out, cronTaskToAPI(item))
	}
	return out
}

// cronRunToAPI 投影执行记录。attempt / fence_token / lease_expire_at 必须逐字段透传：
// 缺了栅栏令牌就无法判断「被抢占后的僵尸写」，这是排查重复副作用的唯一线索。
func cronRunToAPI(r *cronrpc.RunRecord) types.CronTaskRun {
	if r == nil {
		return types.CronTaskRun{}
	}
	return types.CronTaskRun{
		RunId:         r.GetRunId(),
		TaskKey:       r.GetTaskKey(),
		PlannedAt:     r.GetPlannedAt(),
		Attempt:       r.GetAttempt(),
		TriggerType:   cronEnumToAPI(r.GetTriggerType(), cronTriggerTypeToAPI, "trigger type"),
		State:         cronEnumToAPI(r.GetState(), cronRunStateToAPI, "run state"),
		LeaseOwner:    r.GetLeaseOwner(),
		LeaseExpireAt: r.GetLeaseExpireAt(),
		FenceToken:    r.GetFenceToken(),
		StartedAt:     r.GetStartedAt(),
		FinishedAt:    r.GetFinishedAt(),
		DurationMs:    r.GetDurationMs(),
		ResultSummary: r.GetResultSummary(),
		LastError:     r.GetLastError(),
		NextRetryAt:   r.GetNextRetryAt(),
		TraceId:       r.GetTraceId(),
		Ctime:         r.GetCtime(),
		Mtime:         r.GetMtime(),
	}
}

func cronRunsToAPI(list []*cronrpc.RunRecord) []types.CronTaskRun {
	out := make([]types.CronTaskRun, 0, len(list))
	for _, item := range list {
		out = append(out, cronRunToAPI(item))
	}
	return out
}

// cronCheckpointToAPI 投影增量游标。version 是「推进了几次」的证据，
// 后台靠它判断是否存在反复 CAS 冲突。
func cronCheckpointToAPI(c *cronrpc.Checkpoint) types.CronCheckpoint {
	if c == nil {
		return types.CronCheckpoint{}
	}
	return types.CronCheckpoint{
		TaskKey:  c.GetTaskKey(),
		ScopeKey: c.GetScopeKey(),
		Value:    c.GetValue(),
		ValueStr: c.GetValueStr(),
		Version:  c.GetVersion(),
		Operator: c.GetOperator(),
		Ctime:    c.GetCtime(),
		Mtime:    c.GetMtime(),
	}
}

func cronCheckpointsToAPI(list []*cronrpc.Checkpoint) []types.CronCheckpoint {
	out := make([]types.CronCheckpoint, 0, len(list))
	for _, item := range list {
		out = append(out, cronCheckpointToAPI(item))
	}
	return out
}

// cronLeaseToAPI 投影租约：takeover_count 与 fence_token 是实例不稳定与抢占的证据。
func cronLeaseToAPI(l *cronrpc.LeaseInfo) types.CronLease {
	if l == nil {
		return types.CronLease{}
	}
	return types.CronLease{
		LeaseKey:      l.GetLeaseKey(),
		Owner:         l.GetOwner(),
		FenceToken:    l.GetFenceToken(),
		ExpireAt:      l.GetExpireAt(),
		AcquiredAt:    l.GetAcquiredAt(),
		TakeoverCount: l.GetTakeoverCount(),
	}
}

func cronLeasesToAPI(list []*cronrpc.LeaseInfo) []types.CronLease {
	out := make([]types.CronLease, 0, len(list))
	for _, item := range list {
		out = append(out, cronLeaseToAPI(item))
	}
	return out
}

// cronTaskAuditToAPI 投影任务变更审计。from_state/to_state 在 proto 里是字符串，
// 原样透传（可能是 cron 内部的状态别名），网关不做数值化解释。
func cronTaskAuditToAPI(a *cronrpc.TaskAudit) types.CronTaskAudit {
	if a == nil {
		return types.CronTaskAudit{}
	}
	return types.CronTaskAudit{
		Id:        a.GetId(),
		TaskKey:   a.GetTaskKey(),
		Action:    a.GetAction(),
		FromState: a.GetFromState(),
		ToState:   a.GetToState(),
		Operator:  a.GetOperator(),
		Detail:    a.GetDetail(),
		TraceId:   a.GetTraceId(),
		Ctime:     a.GetCtime(),
	}
}

func cronTaskAuditsToAPI(list []*cronrpc.TaskAudit) []types.CronTaskAudit {
	out := make([]types.CronTaskAudit, 0, len(list))
	for _, item := range list {
		out = append(out, cronTaskAuditToAPI(item))
	}
	return out
}

// cronGroupHealthToAPI 投影分组健康度。oldest_due_planned_at=0 表示无积压，
// 不是「1970 年积压」，因此不得改写成默认时间。
func cronGroupHealthToAPI(g *cronrpc.GroupHealth) types.CronGroupHealth {
	if g == nil {
		return types.CronGroupHealth{}
	}
	return types.CronGroupHealth{
		TaskGroup:          g.GetTaskGroup(),
		EnabledTasks:       g.GetEnabledTasks(),
		PausedTasks:        g.GetPausedTasks(),
		DueBacklog:         g.GetDueBacklog(),
		Running:            g.GetRunning(),
		Retrying:           g.GetRetrying(),
		FailedLastHour:     g.GetFailedLastHour(),
		ExpiredLeases:      g.GetExpiredLeases(),
		OldestDuePlannedAt: g.GetOldestDuePlannedAt(),
	}
}

func cronGroupHealthsToAPI(list []*cronrpc.GroupHealth) []types.CronGroupHealth {
	out := make([]types.CronGroupHealth, 0, len(list))
	for _, item := range list {
		out = append(out, cronGroupHealthToAPI(item))
	}
	return out
}

// cronTaskOperationResponse 装配 Pause/Resume/Disable 三个接口的共用响应。
// audit_id 是「这次变更落在 cron_task_audit 的哪一行」的证据指针，必须透传：
// 后台据此把一次点击和一个可核对的审计记录连起来，changed=false 时 cron 不写审计，
// audit_id 为 0 也是合法值（表达「没有新证据，因为没有任何变更」）。
func cronTaskOperationResponse(reply *cronrpc.TaskOperationReply) *types.CronTaskOperationResponse {
	return &types.CronTaskOperationResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronTaskOperationData{
			Definition: cronTaskToAPI(reply.GetDefinition()),
			Changed:    reply.GetChanged(),
			AuditId:    reply.GetAuditId(),
		},
		TTL: 0,
	}
}
