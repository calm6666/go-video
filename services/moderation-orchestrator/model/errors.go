package model

import "errors"

// moderation-orchestrator 域错误。
var (
	// ErrInvalidSubmissionID 提交对象 ID 非法。
	ErrInvalidSubmissionID = errors.New("moderation: invalid submission_id")
	// ErrInvalidContentType 内容类型未指定。
	ErrInvalidContentType = errors.New("moderation: invalid content_type")
	// ErrInvalidMid 用户 ID 非法。
	ErrInvalidMid = errors.New("moderation: invalid mid")
	// ErrInvalidBusiness 业务名缺失。
	ErrInvalidBusiness = errors.New("moderation: invalid business")
	// ErrInvalidTaskID 任务 ID 非法。
	ErrInvalidTaskID = errors.New("moderation: invalid task_id")
	// ErrInvalidAppealID 申诉 ID 非法。
	ErrInvalidAppealID = errors.New("moderation: invalid appeal_id")
	// ErrInvalidVerdict 审核结论非法。
	ErrInvalidVerdict = errors.New("moderation: invalid verdict")
	// ErrInvalidReason 结论原因缺失。
	ErrInvalidReason = errors.New("moderation: invalid reason")
	// ErrInvalidHandler 处理人缺失。
	ErrInvalidHandler = errors.New("moderation: invalid handler")
	// ErrInvalidContent 申诉理由缺失。
	ErrInvalidContent = errors.New("moderation: invalid appeal content")
	// ErrPsTooLarge 分页大小超限。
	ErrPsTooLarge = errors.New("moderation: ps exceeds 50")
	// ErrTaskNotFound 任务不存在。
	ErrTaskNotFound = errors.New("moderation: task not found")
	// ErrResultNotFound 审核结论不存在。
	ErrResultNotFound = errors.New("moderation: result not found")
	// ErrAppealNotFound 申诉不存在。
	ErrAppealNotFound = errors.New("moderation: appeal not found")
	// ErrRuleNotFound 规则不存在。
	ErrRuleNotFound = errors.New("moderation: rule not found")
	// ErrDuplicateTask 同一提交对象已存在审核中任务。
	ErrDuplicateTask = errors.New("moderation: duplicate task for submission")
	// ErrInvalidStateTransition 任务状态机非法迁移。
	// 依据 AGENTS.md §8：SubmitForReview 只接受未审核任务，
	// worker 回调只能推进 PENDING→PROCESSING→DONE，不能直接置 APPROVED/PUBLISHED。
	ErrInvalidStateTransition = errors.New("moderation: invalid state transition")
	// ErrAppealAlreadyHandled 申诉已被处理。
	ErrAppealAlreadyHandled = errors.New("moderation: appeal already handled")
	// ErrWorkerNotConfigured moderation-worker 下游未配置。
	ErrWorkerNotConfigured = errors.New("moderation: moderation-worker rpc not configured")
)

// 任务状态常量。
// 与 proto 的 TaskState 对齐：0 UNSPECIFIED、1 PENDING、2 PROCESSING、
// 3 DONE、4 APPEALED、5 APPEAL_DONE、9 CANCELED。
const (
	TaskStateUnspecified = 0
	TaskStatePending     = 1
	TaskStateProcessing  = 2
	TaskStateDone        = 3
	TaskStateAppealed    = 4
	TaskStateAppealDone  = 5
	TaskStateCanceled    = 9
)

// 审核结论常量。
// 与 proto 的 Verdict 对齐：0 UNSPECIFIED、1 PASS、2 REVIEW、3 REJECT。
const (
	VerdictUnspecified = 0
	VerdictPass        = 1
	VerdictReview      = 2
	VerdictReject      = 3
)

// 规则状态常量。
const (
	RuleStateDisabled = 0
	RuleStateEnabled  = 1
)
