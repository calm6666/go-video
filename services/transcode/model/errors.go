package model

import "errors"

// transcode 域错误。
var (
	ErrTaskNotFound      = errors.New("transcode: task not found")
	ErrTemplateNotFound  = errors.New("transcode: template not found")
	ErrInvalidTaskID     = errors.New("transcode: invalid task_id")
	ErrInvalidTemplateID = errors.New("transcode: invalid template_id")
	ErrInvalidAssetID    = errors.New("transcode: invalid asset_id")
	ErrInvalidState      = errors.New("transcode: invalid task state")
	ErrInvalidProgress   = errors.New("transcode: invalid progress")
	ErrInvalidTransition = errors.New("transcode: invalid state transition")
	ErrTerminalState     = errors.New("transcode: task already in terminal state")
	ErrTemplateNameEmpty = errors.New("transcode: template name is empty")
	ErrPsTooLarge        = errors.New("transcode: ps exceeds 50")
)

// 转码任务状态常量（与 rpc.TaskState 枚举值对齐）。
const (
	TaskStatePending    = int32(1) // 待处理（任务已创建，等待 Worker 拉起）
	TaskStateProcessing = int32(2) // 处理中（Worker 已开始转码）
	TaskStateSucceeded  = int32(3) // 成功（转码完成）
	TaskStateFailed     = int32(4) // 失败（终态）
)

// IsTerminal 判断状态是否为终态（SUCCEEDED/FAILED）。
func IsTerminal(state int32) bool {
	return state == TaskStateSucceeded || state == TaskStateFailed
}

// IsValidTransition 判断转码任务状态机是否允许 old→new 转换。
// 合法路径：PENDING/PROCESSING → PROCESSING/SUCCEEDED/FAILED。
// 终态（SUCCEEDED/FAILED）不可再变更；不允许回到 PENDING 或未指定。
func IsValidTransition(old, new int32) bool {
	if new != TaskStateProcessing && new != TaskStateSucceeded && new != TaskStateFailed {
		return false
	}
	switch old {
	case TaskStatePending, TaskStateProcessing:
		return true
	default:
		return false
	}
}
