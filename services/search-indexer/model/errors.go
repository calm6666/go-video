package model

import "errors"

// search-indexer 域错误。
// 说明：索引是投影，不是事实源；所有错误都必须是可观测、可重试或可人工介入的，
// 禁止把错误吞掉后返回“写入成功”（AGENTS.md §9）。
var (
	// ErrInvalidContentID content_id 非法（<=0）。
	ErrInvalidContentID = errors.New("search-indexer: invalid content_id")
	// ErrInvalidDocRevision 缺少 doc_revision，无法做防旧覆盖新判定。
	ErrInvalidDocRevision = errors.New("search-indexer: doc_revision is required (Unix 毫秒)")
	// ErrStaleDoc incoming 版本不比索引中的新，拒绝覆盖。
	ErrStaleDoc = errors.New("search-indexer: stale doc_revision, write rejected")
	// ErrContentSnapshotRequired 上游未携带事实快照，本服务不猜测字段（AGENTS.md §5）。
	ErrContentSnapshotRequired = errors.New("search-indexer: content snapshot is required, 本服务不直连上游库表")
	// ErrTaskNotFound 重建任务不存在。
	ErrTaskNotFound = errors.New("search-indexer: rebuild task not found")
	// ErrInvalidScope scope 不在 full/partition/content_type 之内。
	ErrInvalidScope = errors.New("search-indexer: invalid rebuild scope")
	// ErrInvalidRequestID request_id 为空，无法保证幂等。
	ErrInvalidRequestID = errors.New("search-indexer: request_id is required")
	// ErrAliasNotFound 别名在 OpenSearch 中不存在。
	ErrAliasNotFound = errors.New("search-indexer: alias not found")
	// ErrAliasMismatch expected_current 与别名当前指向不一致（乐观校验失败）。
	ErrAliasMismatch = errors.New("search-indexer: alias expected_current mismatch")
	// ErrTargetIndexNotFound 切换目标索引不存在或没有文档。
	ErrTargetIndexNotFound = errors.New("search-indexer: target index not found")
	// ErrIndexNameMismatch 目标索引命名不符合 <alias>_ 前缀约束。
	ErrIndexNameMismatch = errors.New("search-indexer: target index must belong to the alias")
	// ErrVersionNotFound 未登记索引版本（尚未发生过写入）。
	ErrVersionNotFound = errors.New("search-indexer: index version record not found")
	// ErrIndexPrefixEmpty IndexPrefix 未配置。
	ErrIndexPrefixEmpty = errors.New("search-indexer: opensearch index_prefix is empty")
	// ErrEventNotFound 消费流水记录不存在。
	ErrEventNotFound = errors.New("search-indexer: consumer offset record not found")
)

// 重建任务状态（search_index_task.state，与 rpc RebuildTask.state 字符串一致）。
const (
	TaskStatePending   = "pending"
	TaskStateRunning   = "running"
	TaskStateSucceeded = "succeeded"
	TaskStateFailed    = "failed"
	TaskStateCanceled  = "canceled"
)

// 重建范围（search_index_task.scope）。
const (
	ScopeFull        = "full"
	ScopePartition   = "partition"
	ScopeContentType = "content_type"
)

// 索引版本状态（search_index_version.state）。
const (
	VersionStateActive   = "active"   // 当前承接写入、查询别名指向
	VersionStateRetiring = "retiring" // 已被切换下来，等待确认无查询后转 history
	VersionStateHistory  = "history"  // 历史索引，可被运维清理
)

// 消费流水状态（search_consumer_offset.state，见 docs/api-and-events.md §6）。
const (
	OffsetStateReceived   = "received"
	OffsetStateProcessing = "processing"
	OffsetStateSucceeded  = "succeeded"
	OffsetStateRetry      = "retry"
	OffsetStateDeadLetter = "dead_letter"
)

// 死信处理状态（search_dead_letter.state）。
const (
	DLQStateOpen      = "open"      // 待人工/工具重放
	DLQStateReplayed  = "replayed"  // 已重放成功
	DLQStateDiscarded = "discarded" // 已确认丢弃（保留审计行）
)

// 死信保留策略（README 说明，清理由 services/cron 执行）。
const (
	// DLQRetentionDays 死信记录保留天数；超期由运维任务按 ctime 清理，
	// 清理前必须先归档，保证下架/版权撤回可追溯（AGENTS.md §8）。
	DLQRetentionDays = 30
)
