package model

import "errors"

// video 域错误。
var (
	ErrInvalidStateTransition = errors.New("video: invalid state transition")
	ErrSubmissionNotFound     = errors.New("video: submission not found")
	ErrNotOwner               = errors.New("video: not submission owner")
	ErrInvalidAid             = errors.New("video: invalid aid")
	ErrInvalidMid             = errors.New("video: invalid mid")
	ErrInvalidTitle           = errors.New("video: invalid title")
	ErrInvalidTypeid          = errors.New("video: invalid typeid")
	ErrPsTooLarge             = errors.New("video: ps exceeds 50")
	ErrSubmissionNotDraft     = errors.New("video: submission not in DRAFT state")
	ErrInvalidTargetState     = errors.New("video: invalid target state")
	// ErrSubmissionNotPlayable 稿件当前不可对外播放：未处于 PUBLISHED，
	// 或已发布但没有带媒资的版次（转码/登记未完成）。
	ErrSubmissionNotPlayable = errors.New("video: submission not playable")
)

// Outbox 发布状态常量（与 video_outbox.state 列取值、
// deploy/migrations/video/000004 注释一致）。
// 编号不可重排：state=0 是发布器唯一的取行条件，改成别的编号会让循环扫不到任何行而不自知。
const (
	OutboxStatePending   = 0 // 待发布
	OutboxStatePublished = 1 // 已发布
	OutboxStateFailed    = 2 // 投递尝试耗尽或行不可发布，需人工处理
)

// 领域事件契约常量。topic 由 eventenvelope.Topic(EventContentPublished, EventSchemaVersion)
// 现场拼出，配置与文档里的 `content.published.v1` 都必须等于这个派生值
// （publisher 的校验据此拒收多余 topic）。
const (
	// Producer 事件生产者标识。
	Producer = "video"
	// EventContentPublished 稿件对外可见性变更事实 → search-indexer 投影、inbox 作者通知。
	EventContentPublished = "content.published"
	// EventSchemaVersion 当前事件 schema 版本。
	EventSchemaVersion = 1
	// AggregateTypeSubmission 事件聚合根类型；aggregate_id 恒为 aid 的十进制字符串，
	// 同时是分区键，因此同一稿件的事件必然落在同一分区并按 id 升序生效。
	AggregateTypeSubmission = "submission"
)

// content.published 的 action 取值（与两个消费方的常量字面量一致：
// search-indexer internal/consumer/mapping.go 的 Action*、inbox 的 contentTemplate 分支）。
// 字面量改动即改契约，必须同步递增 EventSchemaVersion。
const (
	ActionPublish = "publish" // SCHEDULED → PUBLISHED：整篇投影 upsert
	ActionOffline = "offline" // PUBLISHED → OFFLINE：投影下线，不重建整篇
	ActionExpired = "expired" // PUBLISHED → EXPIRED：同上
	ActionDelete  = "delete"  // 已可见稿件 → DELETED：投影下线并清除
)

// ContentTypeUGC 是 payload 里 content_type 的取值（1 UGC、2 PGC、3 直播）。
// 本服务只产 UGC；search-indexer 的 ContentDoc.Validate 只接受 1..3，
// 越界值会被判为永久错误进死信。
const ContentTypeUGC = 1
