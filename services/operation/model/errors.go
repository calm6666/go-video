package model

import "errors"

// operation 域错误。logic 层把它们映射为 gRPC status，
// 错误消息保持可读且不包含口令、token 或 SQL 片段（AGENTS.md §9）。
var (
	// ErrAdminNotFound 管理员账号不存在。
	ErrAdminNotFound = errors.New("operation: admin user not found")
	// ErrAdminExists 管理员账号名已存在。
	ErrAdminExists = errors.New("operation: admin username already exists")
	// ErrAdminDisabled 管理员账号已禁用。
	ErrAdminDisabled = errors.New("operation: admin user disabled")
	// ErrAdminLocked 管理员账号被锁定（防爆破）。
	ErrAdminLocked = errors.New("operation: admin user locked")
	// ErrAdminPasswordWrong 口令校验失败。
	ErrAdminPasswordWrong = errors.New("operation: password wrong")
	// ErrAdminPasswordEmpty 创建/重置时未提供口令。
	ErrAdminPasswordEmpty = errors.New("operation: password required")
	// ErrAdminPasswordWeak 口令强度不足。
	ErrAdminPasswordWeak = errors.New("operation: password too weak")
	// ErrAdminSelfDisable 禁止禁用自己的账号。
	ErrAdminSelfDisable = errors.New("operation: cannot disable yourself")
	// ErrSecondFactorRequired 该账号启用了二次校验但请求未带校验码。
	ErrSecondFactorRequired = errors.New("operation: second factor code required")
	// ErrSecondFactorWrong 二次校验码校验失败（由 account 验证码通道判定）。
	ErrSecondFactorWrong = errors.New("operation: second factor code wrong or expired")

	// ErrSessionInvalid 后台会话不存在、已吊销或已过期。
	ErrSessionInvalid = errors.New("operation: admin session invalid or expired")
	// ErrTokenSecretMissing 未配置 token 摘要密钥：AdminSession.TokenSecretRef 为空，
	// 或其指向的环境变量未注入。此时签发出去的是不带摘要段的弱 token，
	// 会绕过「打库前的本地格式校验」这条防线，因此直接拒绝登录并给出可执行提示，
	// 而不是静默降级（AGENTS.md §9：安全强度不得倒退；密钥只来自配置/环境变量）。
	ErrTokenSecretMissing = errors.New("operation: admin token signing secret missing, set the env var named by AdminSession.TokenSecretRef")

	// ErrRoleNotFound 角色不存在。
	ErrRoleNotFound = errors.New("operation: role not found")
	// ErrRoleExists 角色标识已存在。
	ErrRoleExists = errors.New("operation: role name already exists")
	// ErrRoleHasMembers 角色仍有成员，禁止删除。
	ErrRoleHasMembers = errors.New("operation: role still has members")

	// ErrPermissionNotFound 权限点不存在。
	ErrPermissionNotFound = errors.New("operation: permission not found")
	// ErrPermissionExists 权限点 (resource, action) 已存在。
	ErrPermissionExists = errors.New("operation: permission already exists")
	// ErrPermissionInvalid 权限点字段非法。
	ErrPermissionInvalid = errors.New("operation: invalid resource or action")

	// ErrMenuNotFound 菜单节点不存在。
	ErrMenuNotFound = errors.New("operation: menu not found")
	// ErrMenuParentSelf 菜单父级指向自身，会形成环。
	ErrMenuParentSelf = errors.New("operation: menu parent cannot be itself")
	// ErrMenuHasChildren 仍存在子节点，禁止隐藏式删除（本期只做状态切换保护）。
	ErrMenuHasChildren = errors.New("operation: menu still has children")

	// ErrConfigNotFound 运营配置不存在。
	ErrConfigNotFound = errors.New("operation: ops config not found")
	// ErrConfigVersionConflict 乐观锁冲突：expect_version 与库中版本不一致。
	ErrConfigVersionConflict = errors.New("operation: ops config version conflict, reload and retry")
	// ErrConfigKeyEmpty 配置键为空。
	ErrConfigKeyEmpty = errors.New("operation: cfg_key required")
	// ErrConfigValueInvalid 配置值与 value_type 不匹配。
	ErrConfigValueInvalid = errors.New("operation: cfg_value does not match value_type")

	// ErrTaskNotFound 任务不存在。
	ErrTaskNotFound = errors.New("operation: admin task not found")
	// ErrTaskBadTransition 任务状态非法迁移。
	ErrTaskBadTransition = errors.New("operation: illegal task state transition")
	// ErrTaskEmptySteps 任务步骤为空。
	ErrTaskEmptySteps = errors.New("operation: task requires at least one step")
	// ErrTaskTooManySteps 任务步骤超过上限。
	ErrTaskTooManySteps = errors.New("operation: task steps exceed limit")
	// ErrTaskParamsNotJSON params 不是合法 JSON。
	ErrTaskParamsNotJSON = errors.New("operation: task params must be valid JSON")
	// ErrTaskTypeUnknown 未知任务类型。
	ErrTaskTypeUnknown = errors.New("operation: unknown task type")

	// ErrInvalidPage 分页参数非法。
	ErrInvalidPage = errors.New("operation: invalid pagination")
	// ErrInvalidOperator 缺少操作者上下文（OpContext.operator_id <= 0）。
	ErrInvalidOperator = errors.New("operation: operator context required")
	// ErrDownstreamUnavailable 下游领域服务未配置（optional RPC 客户端为空）。
	ErrDownstreamUnavailable = errors.New("operation: downstream rpc client not configured")
)

// 管理员账号状态（op_admin_user.state）。
const (
	AdminStateNormal  int32 = 1 // 正常
	AdminStateDisable int32 = 2 // 禁用
	AdminStateLock    int32 = 3 // 锁定（防爆破，locked_until 到期后可自动恢复）
)

// 会话状态（op_admin_session.state）。
const (
	SessionStateActive  int32 = 1 // 有效
	SessionStateRevoked int32 = 2 // 已吊销
)

// 角色/菜单/配置通用状态。
const (
	StateEnable  int32 = 1 // 启用/显示/生效
	StateDisable int32 = 2 // 停用/隐藏/下线
)

// 审计结果（op_audit_index.result）。
const (
	AuditResultOK     = "ok"
	AuditResultDenied = "denied"
	AuditResultError  = "error"
)

// 任务与步骤状态（op_admin_task.state / op_admin_task_step.state）。
const (
	TaskStatePending   = "pending"
	TaskStateRunning   = "running"
	TaskStateSucceeded = "succeeded"
	TaskStatePartial   = "partial"
	TaskStateFailed    = "failed"
	TaskStateCanceled  = "canceled"

	StepStatePending   = "pending"
	StepStateRunning   = "running"
	StepStateSucceeded = "succeeded"
	StepStateFailed    = "failed"
	StepStateCanceled  = "canceled"
)

// 支持的任务类型（决定调用哪个下游 RPC）。
const (
	TaskTypeBatchOfflineSubmission = "batch_offline_submission" // → video.TransitionState(OFFLINE)
	TaskTypeBatchOfflineEpisode    = "batch_offline_episode"    // → catalog.OfflineEpisode
	TaskTypeBatchExpireWindow      = "batch_expire_window"      // → rights.ExpireWindow
	TaskTypeBatchRejectAppeal      = "batch_reject_appeal"      // → moderation.ProcessAppeal
)

// ValidTaskType 判断任务类型是否受支持。
func ValidTaskType(t string) bool {
	switch t {
	case TaskTypeBatchOfflineSubmission, TaskTypeBatchOfflineEpisode,
		TaskTypeBatchExpireWindow, TaskTypeBatchRejectAppeal:
		return true
	default:
		return false
	}
}

// taskTransitions 是管理任务状态机的合法转换表（AGENTS.md §8：状态只能通过
// 合法迁移推进）。终态没有出边。
var taskTransitions = map[string][]string{
	TaskStatePending:   {TaskStateRunning, TaskStateCanceled},
	TaskStateRunning:   {TaskStateSucceeded, TaskStatePartial, TaskStateFailed, TaskStateCanceled},
	TaskStateSucceeded: {},
	TaskStatePartial:   {},
	TaskStateFailed:    {},
	TaskStateCanceled:  {},
}

// CanTaskTransition 校验 from → to 是否为合法的任务状态迁移。
// from 为未知状态时返回 false；已是终态时不允许再迁移。
func CanTaskTransition(from, to string) bool {
	targets, ok := taskTransitions[from]
	if !ok {
		return false
	}
	for _, t := range targets {
		if t == to {
			return true
		}
	}
	return false
}

// IsTaskFinalState 判断任务状态是否为终态。
func IsTaskFinalState(state string) bool {
	switch state {
	case TaskStateSucceeded, TaskStatePartial, TaskStateFailed, TaskStateCanceled:
		return true
	default:
		return false
	}
}

// ResolveTaskFinalState 依据步骤成功/失败计数推导任务收敛状态。
// 全部成功 → succeeded；全部失败 → failed；部分失败 → partial。
// 调用方需保证 total > 0。
func ResolveTaskFinalState(total, succeeded, failed int32) string {
	switch {
	case failed == 0 && succeeded >= total:
		return TaskStateSucceeded
	case succeeded == 0 && failed >= total:
		return TaskStateFailed
	default:
		return TaskStatePartial
	}
}

// stepTransitions 步骤状态机的合法转换。
var stepTransitions = map[string][]string{
	StepStatePending:   {StepStateRunning, StepStateCanceled, StepStateFailed},
	StepStateRunning:   {StepStateSucceeded, StepStateFailed, StepStateCanceled},
	StepStateSucceeded: {},
	StepStateFailed:    {},
	StepStateCanceled:  {},
}

// CanStepTransition 校验步骤状态迁移是否合法。
func CanStepTransition(from, to string) bool {
	targets, ok := stepTransitions[from]
	if !ok {
		return false
	}
	for _, t := range targets {
		if t == to {
			return true
		}
	}
	return false
}
