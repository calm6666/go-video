package model

import "errors"

// moderation-worker 域错误。
var (
	ErrInvalidTaskID       = errors.New("moderation-worker: invalid task_id")
	ErrInvalidWorkerTaskID = errors.New("moderation-worker: invalid worker_task_id")
	ErrInvalidCapability   = errors.New("moderation-worker: invalid capability")
	ErrInvalidMediaURI     = errors.New("moderation-worker: invalid media_uri")
	ErrCapabilityMismatch  = errors.New("moderation-worker: capability mismatch with method")
	ErrTaskNotFound        = errors.New("moderation-worker: task not found")
	ErrTaskAlreadyRunning  = errors.New("moderation-worker: task already running")
)

// 任务状态常量（与 proto TaskState 对齐）。
const (
	TaskStatePending   = int32(1)
	TaskStateRunning   = int32(2)
	TaskStateSucceeded = int32(3)
	TaskStateFailed    = int32(4)
	TaskStateTimeout   = int32(5)
)

// 识别能力常量（与 proto CapabilityType 对齐）。
const (
	CapabilityOCR   = int32(1)
	CapabilityASR   = int32(2)
	CapabilityImage = int32(3)
	CapabilityAudio = int32(4)
)
