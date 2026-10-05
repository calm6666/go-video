package model

import "errors"

// upload 域错误。
var (
	ErrUploadNotFound     = errors.New("upload: session not found")
	ErrUploadCompleted    = errors.New("upload: session already completed")
	ErrUploadAborted      = errors.New("upload: session already aborted")
	ErrChunkNotFound      = errors.New("upload: chunk not found")
	ErrChunkMismatch      = errors.New("upload: chunk list mismatch")
	ErrInvalidUploadID    = errors.New("upload: invalid upload_id")
	ErrInvalidMid         = errors.New("upload: invalid mid")
	ErrInvalidFilename    = errors.New("upload: invalid filename")
	ErrInvalidSize        = errors.New("upload: invalid size")
	ErrInvalidChunkNo     = errors.New("upload: invalid chunk_no")
	ErrInvalidTotalChunks = errors.New("upload: invalid total_chunks")
)

// 会话状态常量（与 rpc.UploadState 数值保持一致）。
const (
	SessionStateInitialized = 1 // 已初始化，等待分片上传
	SessionStateUploading   = 2 // 上传中
	SessionStateCompleted   = 3 // 已完成（OSS 分片合并成功）
	SessionStateAborted     = 4 // 已取消
	SessionStateFailed      = 5 // 失败
)

// 分片状态常量（与 rpc.ChunkState 数值保持一致）。
const (
	ChunkStatePending  = 1 // 待上传
	ChunkStateUploaded = 2 // 已上传到 OSS
	ChunkStateVerified = 3 // 已校验（ETag 匹配）
)

// Outbox 发布状态常量（与 upload_outbox.state 列取值、deploy/migrations/upload/000003 注释一致）。
// 编号不可重排：state=0 是发布器唯一的取行条件，改成别的编号会让循环扫不到任何行而不自知。
const (
	OutboxStatePending   = 0 // 待发布
	OutboxStatePublished = 1 // 已发布
	OutboxStateFailed    = 2 // 投递尝试耗尽或行不可发布，需人工处理
)

// 领域事件契约常量。topic 由 eventenvelope.Topic(EventMediaTask, EventSchemaVersion) 现场拼出，
// 配置与文档里的 `media.task.v1` 都必须等于这个派生值（publisher 的校验据此拒收多余 topic）。
const (
	// Producer 事件生产者标识。
	Producer = "upload"
	// EventMediaTask 分片上传完成事实 → asset/transcode/content-fingerprint 接管后续媒资处理。
	EventMediaTask = "media.task"
	// EventSchemaVersion 当前事件 schema 版本。
	EventSchemaVersion = 1
	// AggregateTypeUploadSession 事件聚合根类型，同时是 outbox.aggregate_id 的口径说明。
	AggregateTypeUploadSession = "upload_session"
)
