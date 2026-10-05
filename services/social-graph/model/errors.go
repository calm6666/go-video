package model

import "errors"

// social-graph 域错误。
var (
	ErrInvalidMid            = errors.New("social-graph: invalid mid")
	ErrInvalidFollowerMid    = errors.New("social-graph: invalid follower_mid")
	ErrInvalidBlackMid       = errors.New("social-graph: invalid black_mid")
	ErrInvalidSpecialMid     = errors.New("social-graph: invalid special_mid")
	ErrInvalidOwnerMid       = errors.New("social-graph: invalid owner mid")
	ErrTooManyOwners         = errors.New("social-graph: owners count exceeds 100")
	ErrTooManyMids           = errors.New("social-graph: mids count exceeds 100")
	ErrPsTooLarge            = errors.New("social-graph: ps exceeds 50")
	ErrSelfAction            = errors.New("social-graph: cannot act on self")
	ErrSpecialNeedFollow     = errors.New("social-graph: special require existing follow")
	ErrBlackNeedCancelFollow = errors.New("social-graph: black require cancel follow first")
)

// relation_follow 状态常量。
const (
	FollowStateNormal  = 0 // 正常关注
	FollowStateDeleted = 1 // 已取关（软删除）
)

// relation_black 状态常量。
const (
	BlackStateNormal  = 0 // 已拉黑
	BlackStateDeleted = 1 // 已取消拉黑（软删除）
)

// relation_special 状态常量。
const (
	SpecialStateNormal  = 0 // 已特别关注
	SpecialStateDeleted = 1 // 已取消特别关注（软删除）
)
