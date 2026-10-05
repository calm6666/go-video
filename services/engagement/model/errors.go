package model

import (
	"errors"
	"time"
)

// engagement 域错误。
var (
	ErrFolderNotFoundOrForbidden = errors.New("engagement: folder not found or not owner")
	ErrInvalidBusiness           = errors.New("engagement: invalid business")
	ErrInvalidMid                = errors.New("engagement: invalid mid")
	ErrInvalidMessage            = errors.New("engagement: invalid message_id")
	ErrInvalidMessageID          = errors.New("engagement: invalid message_id")
	ErrInvalidOid                = errors.New("engagement: invalid oid")
	ErrInvalidFid                = errors.New("engagement: invalid fid")
	ErrTooManyIDs                = errors.New("engagement: ids count exceeds 50")
	ErrTooManyMessageIDs         = errors.New("engagement: message_ids count exceeds 100")
	ErrInconsistentIDs           = errors.New("engagement: origin_ids and message_ids length mismatch")
	ErrPsTooLarge                = errors.New("engagement: ps exceeds 50")
	ErrFolderNameEmpty           = errors.New("engagement: folder name is empty")
	ErrFolderLimitExceeded       = errors.New("engagement: folder count exceeds 100")
)

// 收藏夹状态常量。
const (
	FolderStateNormal  = 0
	FolderStateDeleted = 1
)

// ThumbupLike 状态常量。
const (
	LikeStateCancel  = 0
	LikeStateLike    = 1
	LikeStateDislike = 2
)

// Outbox 发布状态常量（engagement_outbox.state）。
// 编号与建表 COMMENT 严格一致，不可重排：ListPending 只认 0，
// 改成别的编号会让待发布行永远不被投递且不报错。
const (
	OutboxStatePending   = 0
	OutboxStatePublished = 1
	OutboxStateFailed    = 2
)

// 领域事件契约（docs/api-and-events.md §4/§5）。
// topic 不在此写死：由 common/eventenvelope.Topic(event_type, schema_version) 现场拼出，
// 避免「配置里的 topic」与「事件类型 + 版本」两份真相。
const (
	// Producer 本服务在事件信封中的生产者标识。
	Producer = "engagement"
	// EventEngagementAction 互动动作事件类型；与 schema 版本共同决定 topic engagement.action.v1。
	EventEngagementAction = "engagement.action"
	// EventSchemaVersion 当前事件 schema 版本。
	EventSchemaVersion = 1
	// AggregateTypeAction 互动事件的聚合根类型：被互动的内容对象（aggregate_id 为其十进制 ID）。
	AggregateTypeAction = "content"
)

// 互动动作名（事件 payload 的 action 字段取值）。
// 字符串是本事件契约的一部分：消费者（inbox、search-indexer）按这些字面量分支，
// 改名等同破坏式变更，必须同时升 schema_version。
const (
	ActionLike           = "like"
	ActionCancelLike     = "cancel_like"
	ActionDislike        = "dislike"
	ActionCancelDislike  = "cancel_dislike"
	ActionFavorite       = "favorite"
	ActionCancelFavorite = "cancel_favorite"
	ActionShare          = "share"
)

// currentDayYyyymmdd 返回当前日期的 YYYYMMDD 整数（用于分享幂等）。
func currentDayYyyymmdd() int32 {
	now := time.Now()
	return int32(now.Year()*10000 + int(now.Month())*100 + now.Day())
}
