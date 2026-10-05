// Package registry 是 cron 的任务处理器注册表：把「DB 里的调度事实」与
// 「进程里的 Go 实现」用 handler 名字连起来。
//
// 为什么需要显式注册表（AGENTS.md §3、§9）：
//   - 定时任务散落在各服务里会导致同一件事被多个 goroutine 重复做，且没有统一的
//     超时/重试/租约策略；注册表保证「一个 handler 名只有一个实现」；
//   - 任务定义存在但进程里没有对应实现时，绝不允许静默跳过：调度侧必须把该执行
//     判为失败（model.ErrHandlerNotRegistered）并进入退避，让「部署少了代码」这件事
//     在健康度接口和审计里可见；
//   - 处理器签名只依赖本包的值对象，不暴露 *ServiceContext，避免第二轮实现时
//     处理器反向依赖 rpc 类型（领域逻辑要能脱离 gRPC 单测）。
//
// 本轮只提供结构与校验（含单测），具体任务实现留到第二轮：
// 届时在 internal/handlers/<domain>.go 里 spec.Register 或由一个 Install 函数批量注册。
package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go-video/services/cron/model"
)

// 注册与执行层面的错误。与 model 的领域哨兵分开：这些是「装配期」问题。
var (
	// ErrHandlerEmpty handler 名为空。
	ErrHandlerEmpty = errors.New("cron/registry: handler name is required")
	// ErrHandlerNil 注册了空的执行函数。
	ErrHandlerNil = errors.New("cron/registry: handler func is nil")
	// ErrHandlerDuplicate 同名重复注册：启动期即失败，避免后注册的悄悄覆盖先注册的。
	ErrHandlerDuplicate = errors.New("cron/registry: handler already registered")
	// ErrHandlerNotFound 注册表里没有该 handler（DB 定义领先于代码部署）。
	ErrHandlerNotFound = errors.New("cron/registry: handler not found")
	// ErrSpecConflict DB 任务定义与注册表声明的行为约束冲突。
	ErrSpecConflict = errors.New("cron/registry: task definition conflicts with handler spec")
)

// Input 是一次执行收到的最小上下文，由调度侧（第二轮）从任务定义 + 执行记录装配。
type Input struct {
	// RunID 是 cron_task_run.id；上报结果时必须回传。
	RunID int64
	// TaskKey 与 PlannedAt 共同构成幂等身份：处理器内部的外部副作用必须以此去重。
	TaskKey string
	// PlannedAt 计划时刻（Unix 秒）。同一计划时刻重放时值不变。
	PlannedAt int64
	// Attempt 第几次尝试（从 1 开始）；退避重试与人工重试都会推进它，而 PlannedAt 不变。
	Attempt int32
	// FenceToken 当前租约栅栏令牌。处理器把它透传给下游写接口可作为额外防重放手段；
	// 一旦续租失败（令牌变化或 ErrLeaseLost），调度侧会取消 ctx，处理器必须尽快返回。
	FenceToken int64
	// Params 任务定义里的 JSON 文本（或 TriggerTask 覆盖值）；密钥不在这里，
	// 只放环境变量名（secret_refs），由处理器自行 os.Getenv（AGENTS.md §4）。
	Params string
	// SecretRefs 逗号分隔的环境变量名，仅透传给处理器。
	SecretRefs string
	// Operator 手动触发/重试时的操作人；定时触发为空。
	Operator string
	// TriggerType 触发来源（model.TriggerType*），处理器可据此区分「补跑」与「常规跑」。
	TriggerType int32
	// TraceID 全链路追踪 ID。
	TraceID string
	// Timeout 本次执行的硬超时，调度侧已用它包裹 ctx；这里冗余给处理器做子预算分配。
	TimeoutSeconds int32
	// LeaseTTL 本次执行实际生效的租约 TTL（秒），已按配置上下界收敛。
	LeaseTTLSeconds int64
}

// Output 是一次执行的返回值。ResultSummary 有入库长度上限，禁止塞事件正文或大对象。
type Output struct {
	// ResultSummary 人读结果摘要，如 "scanned=1200,offlined=34"。
	ResultSummary string
	// Skipped 为真表示本轮无事可做且无副作用，调度侧记 RUN_STATE_SKIPPED。
	Skipped bool
	// SkipReason Skipped 的说明，会写进 last_error 供运营核对。
	SkipReason string
	// Retryable 指明失败是否值得按策略自动重试。
	// 例如「下游未配置」可重试（等部署补齐），而「参数非法」重试无意义，应直接终态。
	// 由处理器判断，调度侧据此决定 RETRYING 还是 FAILED。
	Retryable bool
	// Checkpoint 非空时，调度侧在上报结果的同一事务里以 CAS 推进游标，
	// 从而保证「副作用已完成」与「游标已前进」不会只成功一半。
	Checkpoint *model.TaskCheckpoint
	// ExpectedCheckpointVersion 推进 Checkpoint 时使用的 expected_version（0 表示首写）。
	ExpectedCheckpointVersion int64
}

// Handler 任务处理器实现。ctx 已带超时与取消（租约丢失即取消），
// 返回 error 时调度侧按 RetryPolicy 决定退避或终态；不得返回「假成功」。
type Handler func(ctx context.Context, in *Input) (*Output, error)

// Spec 注册表里一个 handler 的行为约束。
//
// 注意分工：调度参数（cron 表达式、间隔、时区、misfire 策略）属于 DB 任务定义，
// 可以按环境不同而不同；而这里的字段是**代码能力的边界**，
// DB 定义只能在它允许的范围内配置，越界即 ErrSpecConflict。
type Spec struct {
	// Name 处理器名，与 cron_task_definition.handler 一致，建议用 domain.action 小写点分。
	Name string
	// Description 人读说明（健康度/审计里可见），避免「这个任务到底改了什么」无从判断。
	Description string
	// MinLeaseTTLSeconds 该处理器可接受的最小租约 TTL；0 表示用服务全局下限。
	MinLeaseTTLSeconds int64
	// SuggestedTimeout 建议超时（秒）。DB 未显式配置时调度侧用它兜底。
	SuggestedTimeout int32
	// SuggestedMaxAttempts 建议的最大尝试次数；DB 配置只能 ≤ 该值（防止把不可重试的
	// 破坏性动作刷成风暴）。0 表示不限制。
	SuggestedMaxAttempts int32
	// SerialOnly 为真表示该任务全集群同时只允许一个执行（concurrency_limit 必须为 1）。
	// 典型是切换索引别名、批量下架这类有全局副作用的动作。
	SerialOnly bool
	// RequiresDownstream 该处理器依赖的下游 RPC 名（config 字段名，如 RightsRPC）。
	// 供启动期自检与运维定位「为什么这个任务一直退避」。
	RequiresDownstream []string
	// Idempotent 声明该处理器以 (task_key, planned_at) + 游标为幂等上下文，
	// 重放安全。为 false 时调度侧不会自动重试（只能人工 TriggerTask）。
	Idempotent bool
	// handler 实际实现。
	handler Handler
}

// WithHandler 返回一个绑定了实现的 Spec 副本。
//
// 为什么需要：handler 字段是私有的，注册之后没有任何路径能替换实现（避免「后注册的
// 悄悄覆盖先注册的」），因此包外的装配代码——第二轮 internal/handlers 的批量注册、
// 以及 internal/logic 的单测——只能经由这里把「契约 + 实现」凑齐后交给 Register。
// 这里不绕过任何校验：负边界值、重名、空名仍由 Register 拒绝。
func (s Spec) WithHandler(h Handler) Spec {
	s.handler = h
	return s
}

// Registry 并发安全的注册表。
type Registry struct {
	mu    sync.RWMutex
	specs map[string]*Spec
}

// New 创建空注册表。
func New() *Registry {
	return &Registry{specs: make(map[string]*Spec)}
}

// Register 注册一个处理器。同名重复注册返回 ErrHandlerDuplicate，
// 绝不覆盖：覆盖会让「两个包都想实现同一个 handler」变成随机行为。
func (r *Registry) Register(spec Spec) error {
	spec.Name = strings.TrimSpace(spec.Name)
	switch {
	case spec.Name == "":
		return ErrHandlerEmpty
	case spec.handler == nil:
		return fmt.Errorf("%w: %s", ErrHandlerNil, spec.Name)
	case strings.ContainsAny(spec.Name, " /"):
		return fmt.Errorf("%w: handler 名不能包含空格或斜杠: %q", ErrHandlerEmpty, spec.Name)
	}
	if spec.MinLeaseTTLSeconds < 0 || spec.SuggestedTimeout < 0 || spec.SuggestedMaxAttempts < 0 {
		return fmt.Errorf("%w: %s 的边界值不能为负", ErrSpecConflict, spec.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.specs[spec.Name]; exists {
		return fmt.Errorf("%w: %s", ErrHandlerDuplicate, spec.Name)
	}
	cp := spec
	r.specs[spec.Name] = &cp
	return nil
}

// MustRegister 供包级 init 使用：注册失败直接 panic（装配错误不该带着跑）。
func (r *Registry) MustRegister(spec Spec) {
	if err := r.Register(spec); err != nil {
		panic(err)
	}
}

// SetHandler 只绑定实现、保留已登记的 Spec（第二轮用于「契约先登记、实现后注入」）。
func (r *Registry) SetHandler(name string, h Handler) error {
	name = strings.TrimSpace(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	spec, ok := r.specs[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrHandlerNotFound, name)
	}
	if h == nil {
		return fmt.Errorf("%w: %s", ErrHandlerNil, name)
	}
	spec.handler = h
	return nil
}

// Get 返回注册的 Spec 副本。
func (r *Registry) Get(name string) (Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.specs[strings.TrimSpace(name)]
	if !ok {
		return Spec{}, false
	}
	return *spec, true
}

// Has 判断 handler 是否已注册且可执行。
func (r *Registry) Has(name string) bool {
	spec, ok := r.Get(name)
	return ok && spec.handler != nil
}

// Names 返回全部 handler 名（升序），供健康度接口列出「本进程能跑哪些任务」。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.specs))
	for name := range r.specs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Len 已注册的 handler 数。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.specs)
}

// Resolve 取出可执行 Spec：未注册或无实现都返回 model.ErrHandlerNotRegistered，
// 调度侧据此把执行判为失败（而不是跳过）。
func (r *Registry) Resolve(name string) (Spec, error) {
	spec, ok := r.Get(name)
	if !ok || spec.handler == nil {
		return Spec{}, fmt.Errorf("%w: %s", model.ErrHandlerNotRegistered, name)
	}
	return spec, nil
}

// ValidateDefinition 校验 DB 任务定义与注册表 Spec 是否兼容（注册/更新/领取执行前都要跑）。
// 它只裁决「代码边界」，业务前置条件由处理器第二轮自行判断。
func (r *Registry) ValidateDefinition(d *model.TaskDefinition) error {
	spec, ok := r.Get(d.Handler)
	if !ok {
		return fmt.Errorf("%w: %s（task_key=%s）", model.ErrHandlerNotRegistered, d.Handler, d.TaskKey)
	}
	if spec.handler == nil {
		return fmt.Errorf("%w: %s 已登记但未注入实现", model.ErrHandlerNotRegistered, d.Handler)
	}
	if spec.SerialOnly && d.ConcurrencyLimit != 1 {
		return fmt.Errorf("%w: %s 只允许串行，concurrency_limit=%d", ErrSpecConflict, d.Handler, d.ConcurrencyLimit)
	}
	if spec.SuggestedMaxAttempts > 0 && d.MaxAttempts > spec.SuggestedMaxAttempts {
		return fmt.Errorf("%w: %s max_attempts=%d 超过上限 %d",
			ErrSpecConflict, d.Handler, d.MaxAttempts, spec.SuggestedMaxAttempts)
	}
	minTTL := spec.MinLeaseTTLSeconds
	if minTTL == 0 {
		minTTL = model.MinLeaseTTLSeconds
	}
	if int64(d.LeaseTTLSeconds) < minTTL {
		return fmt.Errorf("%w: %s lease_ttl_seconds=%d 低于下限 %d",
			ErrSpecConflict, d.Handler, d.LeaseTTLSeconds, minTTL)
	}
	return nil
}

// EffectiveTimeout DB 未配置 timeout_seconds 时用 Spec 的建议值兜底。
func (r *Registry) EffectiveTimeout(d *model.TaskDefinition) (int32, error) {
	if d.TimeoutSeconds > 0 {
		return d.TimeoutSeconds, nil
	}
	spec, ok := r.Get(d.Handler)
	if !ok {
		return 0, fmt.Errorf("%w: %s", model.ErrHandlerNotRegistered, d.Handler)
	}
	if spec.SuggestedTimeout <= 0 {
		return 0, fmt.Errorf("%w: %s 既无 DB timeout_seconds 也无 Spec 建议值", ErrSpecConflict, d.Handler)
	}
	return spec.SuggestedTimeout, nil
}

// Run 执行一个已注册处理器（第二轮的调度循环调用；本轮已可被单测覆盖）。
func (r *Registry) Run(ctx context.Context, name string, in *Input) (*Output, error) {
	spec, err := r.Resolve(name)
	if err != nil {
		return nil, err
	}
	out, err := spec.handler(ctx, in)
	if err != nil {
		return nil, err
	}
	if out == nil {
		// 处理器返回 nil 视为「未实现/结果未知」，绝不当成成功（AGENTS.md §9）。
		return nil, fmt.Errorf("%w: %s 返回了空结果", model.ErrNotImplemented, name)
	}
	return out, nil
}
