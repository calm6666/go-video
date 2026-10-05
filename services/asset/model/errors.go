package model

import "errors"

// asset 域错误。
var (
	ErrAssetNotFound     = errors.New("asset: not found")
	ErrInvalidAssetID    = errors.New("asset: invalid asset_id")
	ErrInvalidUploadID   = errors.New("asset: invalid upload_id")
	ErrInvalidMid        = errors.New("asset: invalid mid")
	ErrInvalidBucket     = errors.New("asset: invalid bucket")
	ErrInvalidObjectKey  = errors.New("asset: invalid object_key")
	ErrInvalidState      = errors.New("asset: invalid state")
	ErrInvalidLang       = errors.New("asset: invalid lang")
	ErrInvalidTimestamp  = errors.New("asset: invalid timestamp")
	ErrInvalidTransition = errors.New("asset: illegal state transition")
	ErrPsTooLarge        = errors.New("asset: ps exceeds 50")
)

// asset_meta 状态常量。依据 AGENTS.md §8：
//
//	UPLOADED → SCANNED → TRANSCODED
const (
	StateUnspecified = 0
	StateUploaded    = 1
	StateScanned     = 2
	StateTranscoded  = 3
	StateFailed      = 4
)

// validTransitions 描述合法的状态机推进。
// key 为当前状态，value 为允许推进到的目标状态集合。
var validTransitions = map[int32]map[int32]struct{}{
	StateUploaded:   {StateScanned: {}, StateFailed: {}},
	StateScanned:    {StateTranscoded: {}, StateFailed: {}},
	StateTranscoded: {StateFailed: {}},
	StateFailed:     {},
}

// CanTransition 校验从 from 到 to 的状态推进是否合法。
func CanTransition(from, to int32) bool {
	targets, ok := validTransitions[from]
	if !ok {
		return false
	}
	_, ok = targets[to]
	return ok
}
