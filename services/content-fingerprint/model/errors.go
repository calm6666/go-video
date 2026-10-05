package model

import "errors"

// content-fingerprint 域错误。
var (
	ErrTaskNotFound   = errors.New("content-fingerprint: task not found")
	ErrInvalidAssetID = errors.New("content-fingerprint: invalid asset_id")
	ErrInvalidFpType  = errors.New("content-fingerprint: invalid fp_type")
	ErrInvalidTaskID  = errors.New("content-fingerprint: invalid task_id")
	ErrInvalidState   = errors.New("content-fingerprint: invalid task state")
	ErrInvalidFpKey   = errors.New("content-fingerprint: invalid fp_key")
	ErrPsTooLarge     = errors.New("content-fingerprint: ps exceeds 50")
	ErrInvalidTopN    = errors.New("content-fingerprint: top_n exceeds 50")
	ErrIllegalState   = errors.New("content-fingerprint: illegal state transition")
)

// 任务状态常量。
const (
	TaskStatePending   = int32(1) // 待处理
	TaskStateSucceeded = int32(2) // 成功
	TaskStateFailed    = int32(3) // 失败
)

// 指纹类型常量。
const (
	FpTypeVideo = int32(1) // 视频指纹
	FpTypeAudio = int32(2) // 音频指纹
)
