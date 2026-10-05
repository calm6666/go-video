package model

import "errors"

// feed 域错误。
var (
	ErrInvalidMid       = errors.New("feed: invalid mid")
	ErrInvalidVmid      = errors.New("feed: invalid vmid")
	ErrInvalidFeedID    = errors.New("feed: invalid feed_id")
	ErrInvalidOid       = errors.New("feed: invalid oid")
	ErrInvalidCursor    = errors.New("feed: invalid cursor")
	ErrPsTooLarge       = errors.New("feed: ps exceeds 50")
	ErrFeedNotFound     = errors.New("feed: feed not found")
	ErrPinNotFound      = errors.New("feed: pin not found")
	ErrPinAlreadyExists = errors.New("feed: pin already exists")
)

// feed_outbox 状态常量。
const (
	FeedStateNormal  = 0 // 正常
	FeedStateUpdated = 1 // 已修改
	FeedStateDeleted = 2 // 已删除
)

// feed_pin 状态常量。
const (
	PinStateNormal  = 0 // 正常
	PinStateDeleted = 1 // 已删除
)
