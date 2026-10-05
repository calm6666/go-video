package model

import (
	"errors"
	"strings"
)

// ErrNotImplemented 表示该用例的 logic 尚未落地（契约轮占位）。
// 风格与 services/account/internal/repository/repository.go:20 一致：显式哨兵错误，
// 绝不返回假成功的零值消息，让调用方（gRPC 客户端与网关）能明确区分
// "服务端还没实现" 和 "业务判定为否"。
// 后续逻辑轮会按 services/live-media/README.md 的方法表逐个替换掉它。
var ErrNotImplemented = errors.New("live-media: not implemented")

// live-media 域错误。gRPC 直接返回这些哨兵，由调用方映射为 HTTP 信封的非零 code；
// 错误文本保持脱敏，不含对象存储凭据、完整拉流地址或 SQL 片段（AGENTS.md §6）。
var (
	// ErrInvalidRoomID room_id 非正数。
	ErrInvalidRoomID = errors.New("live-media: invalid room_id")
	// ErrInvalidSessionID live_session_id 非正数。
	ErrInvalidSessionID = errors.New("live-media: invalid live_session_id")
	// ErrInvalidTemplateID template_id 非正数（直播转码必须引用 transcode 模板）。
	ErrInvalidTemplateID = errors.New("live-media: invalid template_id")
	// ErrEmptyRequestID 写接口缺少幂等键。
	ErrEmptyRequestID = errors.New("live-media: request_id is required")
	// ErrRequestIdDuplicated 幂等键唯一索引冲突：同一 request_id 已登记过任务。
	// 调用方应按 request_id 取回既有任务返回（幂等重放），而不是当作失败。
	ErrRequestIdDuplicated = errors.New("live-media: request_id already registered")
	// ErrTitleTooLong 回放标题超过配置上限。
	ErrTitleTooLong = errors.New("live-media: replay title too long")

	// ErrTranscodeTaskNotFound 转码任务不存在。
	ErrTranscodeTaskNotFound = errors.New("live-media: transcode task not found")
	// ErrStreamOutputNotFound 分发输出不存在。
	ErrStreamOutputNotFound = errors.New("live-media: stream output not found")
	// ErrStreamOutputAmbiguous 按 (room, level, protocol) 定位到多个在线档位：
	// 属数据异常，调用方必须改用 output_id 精确指定，服务端不猜要下线哪一个。
	ErrStreamOutputAmbiguous = errors.New("live-media: stream output ambiguous, output_id is required")
	// ErrSessionOutputOverflow 一场次同时在线的档位数超过 MaxSessionOutputs：属数据异常
	//（正常上限是「档位数 × 协议数」，远小于该值）。断流下线要么整场收敛、要么一条不动，
	// 扫不完却继续下线会造成「一半档位还在线」的观众侧故障，因此直接报错而不是静默截断。
	ErrSessionOutputOverflow = errors.New("live-media: too many online outputs in one session")
	// ErrRecordTaskNotFound 录制任务不存在。
	ErrRecordTaskNotFound = errors.New("live-media: record task not found")
	// ErrReplayTaskNotFound 回放任务不存在。
	ErrReplayTaskNotFound = errors.New("live-media: replay task not found")
	// ErrReplayRefNotFound 回放资产引用不存在。
	ErrReplayRefNotFound = errors.New("live-media: replay asset ref not found")
	// ErrRetentionTaskNotFound 回收任务不存在。
	ErrRetentionTaskNotFound = errors.New("live-media: retention task not found")
	// ErrSegmentNotFound 录制切片不存在。
	ErrSegmentNotFound = errors.New("live-media: record segment not found")

	// ErrInvalidTransition 状态机不允许该迁移（含从终态复活）。
	ErrInvalidTransition = errors.New("live-media: invalid state transition")
	// ErrTerminalState 任务已进入终态，不可再变更。
	ErrTerminalState = errors.New("live-media: task already in terminal state")
	// ErrVersionConflict expected_version 与服务端当前版本不一致（乐观并发）。
	ErrVersionConflict = errors.New("live-media: version conflict, reload and retry")
	// ErrAttemptExhausted 重试次数已达 max_attempts，禁止再次 Retry。
	ErrAttemptExhausted = errors.New("live-media: retry attempt exhausted")

	// ErrInvalidSeq 切片序号非正数。
	ErrInvalidSeq = errors.New("live-media: invalid segment seq")
	// ErrSeqNotMonotonic Worker 上报的 last_seq 小于已登记最大值（乱序上报）。
	ErrSeqNotMonotonic = errors.New("live-media: record last_seq must be monotonic")
	// ErrInvalidSegmentRange 回放请求的切片区间非法（from_seq > to_seq 或起点非 1）。
	ErrInvalidSegmentRange = errors.New("live-media: invalid segment range")
	// ErrSegmentRangeNotRecorded 区间内没有任何可拼接切片。
	ErrSegmentRangeNotRecorded = errors.New("live-media: no verifiable segment in range")
	// ErrReplayGapNotAllowed allow_gaps=false 且区间内存在缺口切片。
	ErrReplayGapNotAllowed = errors.New("live-media: segment gap not allowed by allow_gaps")

	// ErrInvalidBucketRef 产物必须给出 bucket/object_key 引用（大文件不入 MySQL）。
	ErrInvalidBucketRef = errors.New("live-media: bucket and object_key are required")
	// ErrInvalidAssetID asset_id 非正数：asset_id=0 是「未登记媒资」的占位值，
	// 不能用于反查（否则会把所有未登记任务都命中）。
	ErrInvalidAssetID = errors.New("live-media: invalid asset_id")
	// ErrInvalidAid aid 非正数：绑定回放引用必须同时给出稿件主键，
	// 否则引用行无法与 video 事实源对齐（0 是「未建稿」占位值）。
	ErrInvalidAid = errors.New("live-media: invalid aid")
	// ErrAssetRefConflict 同一回放已绑定不同 asset/aid（引用串用，禁止覆盖）。
	ErrAssetRefConflict = errors.New("live-media: replay asset ref conflict")
	// ErrInvalidReviewState 投影状态不在取值范围内。
	ErrInvalidReviewState = errors.New("live-media: invalid review state")
	// ErrEmptyEventID 投影同步缺少 event_id（无法幂等）。
	ErrEmptyEventID = errors.New("live-media: event_id is required")

	// ErrPsTooLarge 每页大小超过服务端上限。
	ErrPsTooLarge = errors.New("live-media: ps exceeds server limit")
	// ErrInvalidCursor keyset 游标非法。
	ErrInvalidCursor = errors.New("live-media: invalid cursor")
	// ErrInvalidPage 页码深到超出深翻页保护窗口（读侧列表用）。
	// 与 ErrPsTooLarge 的分工：ps 越界按 proto 契约夹取（PageParam 注释写明「超限取默认」），
	// 而 pn 大到 OFFSET 会退化成无人应答的线性扫时，契约从未承诺能翻过去，故明确拒绝。
	ErrInvalidPage = errors.New("live-media: page out of range")
	// ErrEmptyEventPayload Outbox 事件缺少类型或聚合主键。
	ErrEmptyEventPayload = errors.New("live-media: outbox event_type and aggregate are required")
	// ErrRetentionTargetRequired 回收任务必须给出 target_id 或 expire_before 之一。
	ErrRetentionTargetRequired = errors.New("live-media: retention needs target_id or expire_before")
	// ErrRetentionReasonRequired 回收原因必填（AGENTS.md §8 审计证据）。
	ErrRetentionReasonRequired = errors.New("live-media: retention reason is required")
	// ErrOperatorRequired 人工写入口必须有操作人（RetryLiveTranscode / CancelLiveTranscode /
	// SubmitRetentionTask）：这三处都是「谁发起的状态推进」，没有 operator 就是一条无法追责的审计记录。
	// 不能借 ErrRetentionReasonRequired 顶替：那是「回收原因」的哨兵，调用方按它做分支，
	// 混用会把「缺操作人」读成「缺回收原因」。
	ErrOperatorRequired = errors.New("live-media: operator required for audit attribution")
)

// --- 直播转码任务状态（live_transcode_task.state），与 rpc.LiveTranscodeState 编号一致 ---

const (
	TranscodeStatePending   int32 = 1 // 待拉起
	TranscodeStateRunning   int32 = 2 // 运行中
	TranscodeStateStopping  int32 = 3 // 停止中
	TranscodeStateStopped   int32 = 4 // 已停止（终态）
	TranscodeStateFailed    int32 = 5 // 失败（可 Retry）
	TranscodeStateCancelled int32 = 6 // 已取消（终态）
)

// TranscodeTerminalStates 终态集合：STOPPED/CANCELLED 不可再变更；
// FAILED 只能通过 RetryLiveTranscode 回到 PENDING，不是任意写都能复活。
func TranscodeTerminalStates() []int32 {
	return []int32{TranscodeStateStopped, TranscodeStateCancelled}
}

// IsTranscodeTerminal 判断是否终态。
func IsTranscodeTerminal(state int32) bool {
	return state == TranscodeStateStopped || state == TranscodeStateCancelled
}

// transcodeTransitions 直播转码任务合法迁移表。
// 键为当前状态，值为允许的目标状态集合（含 RUNNING→RUNNING 的心跳自更新）。
var transcodeTransitions = map[int32][]int32{
	TranscodeStatePending:  {TranscodeStateRunning, TranscodeStateFailed, TranscodeStateCancelled},
	TranscodeStateRunning:  {TranscodeStateRunning, TranscodeStateStopping, TranscodeStateFailed},
	TranscodeStateStopping: {TranscodeStateStopped, TranscodeStateFailed, TranscodeStateCancelled},
	TranscodeStateFailed:   {TranscodeStatePending}, // 只由 RetryLiveTranscode 触发
}

// IsValidTranscodeTransition 判断转码任务状态机是否允许 old→new。
func IsValidTranscodeTransition(old, new int32) bool {
	for _, allow := range transcodeTransitions[old] {
		if allow == new {
			return true
		}
	}
	return false
}

// --- 录制任务状态（live_record_task.state），与 rpc.LiveRecordState 编号一致 ---

const (
	RecordStatePending   int32 = 1 // 待开始
	RecordStateRecording int32 = 2 // 录制中
	RecordStateStopping  int32 = 3 // 停止中
	RecordStateStopped   int32 = 4 // 已停止（终态，可拼接回放）
	RecordStateFailed    int32 = 5 // 失败（可续录）
	RecordStateCancelled int32 = 6 // 已取消（终态）
)

// IsRecordTerminal 录制终态：STOPPED/CANCELLED。
func IsRecordTerminal(state int32) bool {
	return state == RecordStateStopped || state == RecordStateCancelled
}

// recordTransitions 录制状态机。FAILED 允许回到 PENDING（重新登记）或直接 RECORDING
// （断点续录，从 last_seq+1 继续），但绝不跳过缺口登记。
var recordTransitions = map[int32][]int32{
	RecordStatePending:   {RecordStateRecording, RecordStateFailed, RecordStateCancelled},
	RecordStateRecording: {RecordStateRecording, RecordStateStopping, RecordStateFailed},
	RecordStateStopping:  {RecordStateStopped, RecordStateFailed, RecordStateCancelled},
	RecordStateFailed:    {RecordStatePending, RecordStateRecording},
}

// IsValidRecordTransition 判断录制任务状态机是否允许 old→new。
func IsValidRecordTransition(old, new int32) bool {
	for _, allow := range recordTransitions[old] {
		if allow == new {
			return true
		}
	}
	return false
}

// --- 录制切片状态（live_record_segment.state），与 rpc.SegmentState 编号一致 ---

const (
	SegmentStateUploading int32 = 1 // 上传中
	SegmentStateUploaded  int32 = 2 // 已上传（未校验）
	SegmentStateVerified  int32 = 3 // 已校验（可参与回放拼接）
	SegmentStateMissing   int32 = 4 // 缺口（探测到的空洞）
	SegmentStateCorrupt   int32 = 5 // 损坏（永不参与拼接）
)

// SegmentStateRank 正常链路的推进序（MISSING/CORRUPT 不参与排名，返回 -1）。
// 用于「同 seq 重放时状态只允许前进不允许后退」。
func SegmentStateRank(state int32) int32 {
	if state >= SegmentStateUploading && state <= SegmentStateVerified {
		return state
	}
	return -1
}

// IsValidSegmentTransition 切片状态迁移：
// 正向只有 UPLOADING→UPLOADED→VERIFIED，VERIFIED 之后不得回退（未校验的切片绝不能进拼接）；
// MISSING 是「补录到达」入口，可回 UPLOADING/UPLOADED，但不能直接 VERIFIED（必须重新校验）；
// 缺失/损坏判定作用于一切状态（含 VERIFIED）：上传时校验通过、事后对象被删或损坏时必须能改判，
// 否则回放拼接会引用不存在的对象；唯独已判 CORRUPT 不再改判 MISSING（两者都是终局坏态，避免来回翻转）。
// old==new 一律放行，供 Worker 重复回报同一切片时幂等重放。
func IsValidSegmentTransition(old, new int32) bool {
	if old == new {
		return true // 幂等重放
	}
	switch new {
	case SegmentStateMissing, SegmentStateCorrupt:
		return old != SegmentStateCorrupt // 已判损坏不再改判缺失
	case SegmentStateUploading:
		return old == SegmentStateMissing // 补录重新开始
	case SegmentStateUploaded:
		return old == SegmentStateUploading || old == SegmentStateMissing
	case SegmentStateVerified:
		return old == SegmentStateUploaded
	default:
		return false
	}
}

// --- 回放任务状态（live_replay_task.state），与 rpc.ReplayState 编号一致 ---

const (
	ReplayStatePending         int32 = 1 // 待拼接
	ReplayStateMerging         int32 = 2 // 拼接中
	ReplayStateUploading       int32 = 3 // 产物上传中
	ReplayStateRegistered      int32 = 4 // 已登记媒资（asset_id 回填）
	ReplayStateReviewSubmitted int32 = 5 // 已提交审核（aid 回填）
	ReplayStateCompleted       int32 = 6 // 回放可用（由 video 投影驱动）
	ReplayStateFailed          int32 = 7 // 失败（终态）
	ReplayStateCancelled       int32 = 8 // 已取消（终态）
)

// IsReplayTerminal 回放终态：COMPLETED/FAILED/CANCELLED。
func IsReplayTerminal(state int32) bool {
	return state == ReplayStateCompleted || state == ReplayStateFailed || state == ReplayStateCancelled
}

// replayTransitions 回放状态机。
// 关键：REVIEW_SUBMITTED→COMPLETED 只能由 ApplyReplayContentState（video 事实投影）驱动，
// 本服务不得自行把回放推到 COMPLETED，更没有"直接发布"的入口（AGENTS.md §5/§8）。
var replayTransitions = map[int32][]int32{
	ReplayStatePending:         {ReplayStateMerging, ReplayStateFailed, ReplayStateCancelled},
	ReplayStateMerging:         {ReplayStateUploading, ReplayStateFailed, ReplayStateCancelled},
	ReplayStateUploading:       {ReplayStateRegistered, ReplayStateFailed},
	ReplayStateRegistered:      {ReplayStateReviewSubmitted, ReplayStateFailed},
	ReplayStateReviewSubmitted: {ReplayStateCompleted, ReplayStateFailed},
}

// IsValidReplayTransition 判断回放任务状态机是否允许 old→new。
func IsValidReplayTransition(old, new int32) bool {
	for _, allow := range replayTransitions[old] {
		if allow == new {
			return true
		}
	}
	return false
}

// --- 分发输出状态（live_stream_output.state），与 rpc.StreamOutputInfo.state 一致 ---

const (
	StreamOutputStateOnline  int32 = 1 // 在线（可分发）
	StreamOutputStateOffline int32 = 2 // 已下线（终态，可被回收任务清理）
)

// --- 回放资产引用的投影状态（live_replay_asset_ref.review_state），与 rpc.ReviewState 一致 ---
//
// 该字段是 video 服务事实状态的本地只读投影：
// 事实源永远是 video，本服务不会据此反向推进稿件状态。

const (
	ReviewStateUnsynced  int32 = 0 // 未同步
	ReviewStateReviewing int32 = 1 // 审核中
	ReviewStateRejected  int32 = 2 // 驳回
	ReviewStatePublished int32 = 3 // video 侧已发布（回放可用）
	ReviewStateOffline   int32 = 4 // video 侧已下架
	ReviewStateDeleted   int32 = 5 // video 侧已删除
)

// ValidReviewState 判断投影取值是否在范围内。
func ValidReviewState(state int32) bool {
	return state >= ReviewStateReviewing && state <= ReviewStateDeleted
}

// --- 回收任务（live_retention_task），与 rpc.RetentionTargetKind / RetentionState 一致 ---

const (
	RetentionTargetSegment      int32 = 1 // 录制切片
	RetentionTargetReplay       int32 = 2 // 回放产物
	RetentionTargetStreamOutput int32 = 3 // 直播分发残留
)

const (
	RetentionStatePending   int32 = 1 // 待执行（含 dry_run 预演）
	RetentionStateRunning   int32 = 2 // 执行中
	RetentionStateSucceeded int32 = 3 // 已完成（终态）
	RetentionStateFailed    int32 = 4 // 失败（终态）
	RetentionStateCancelled int32 = 5 // 已取消（终态）
)

// IsRetentionTerminal 回收终态：SUCCEEDED/FAILED/CANCELLED。
func IsRetentionTerminal(state int32) bool {
	return state >= RetentionStateSucceeded && state <= RetentionStateCancelled
}

// retentionTransitions 回收状态机：先登记意图、再执行、最后留计数证据。
var retentionTransitions = map[int32][]int32{
	RetentionStatePending: {RetentionStateRunning, RetentionStateCancelled, RetentionStateFailed},
	RetentionStateRunning: {RetentionStateSucceeded, RetentionStateFailed, RetentionStateCancelled},
}

// IsValidRetentionTransition 判断回收任务状态机是否允许 old→new。
func IsValidRetentionTransition(old, new int32) bool {
	for _, allow := range retentionTransitions[old] {
		if allow == new {
			return true
		}
	}
	return false
}

// ValidRetentionTarget 判断回收对象类型是否合法。
func ValidRetentionTarget(kind int32) bool {
	return kind >= RetentionTargetSegment && kind <= RetentionTargetStreamOutput
}

// --- 失败原因（live_*.reason），与 rpc.FailureReason 编号一致 ---

const (
	ReasonUnspecified int32 = 0 // 未指定
	ReasonTimeout     int32 = 1 // 心跳超时
	ReasonSourceLost  int32 = 2 // 源流丢失
	ReasonWorkerCrash int32 = 3 // Worker 异常
	ReasonStorage     int32 = 4 // 对象存储失败
	ReasonCDN         int32 = 5 // CDN 失败
	ReasonUpstream    int32 = 6 // 上游服务拒绝
	ReasonManual      int32 = 7 // 人工停止/取消
	ReasonDataGap     int32 = 8 // 数据缺口
)

// --- Outbox 发布状态（live_media_outbox.state），与 services/playback 的同名字段编号一致 ---

const (
	// ProducerName 事件信封的 producer 字段：与 services/recommend-recall、search-query 同口径，
	// 取服务目录名，便于消费方按来源过滤（docs/api-and-events.md 的 envelope 示例）。
	ProducerName = "live-media"
	// EventSchemaVersion 本服务首发事件 schema 版本；结构与 Topic 的 .v1 后缀保持一致。
	EventSchemaVersion = 1

	OutboxStatePending   = 0 // 待发布
	OutboxStatePublished = 1 // 已发布
	OutboxStateFailed    = 2 // 超过最大重试，转人工处理
)

// --- 引用行生命周期（live_replay_asset_ref.retention_state） ---

const (
	RefRetentionStateNormal    int32 = 0 // 正常
	RefRetentionStatePending   int32 = 1 // 待回收
	RefRetentionStateReclaimed int32 = 2 // 已回收（引用保留审计，产物已删）
)

// ValidRefRetentionState 判断引用行生命周期取值是否合法。
func ValidRefRetentionState(state int32) bool {
	return state >= RefRetentionStateNormal && state <= RefRetentionStateReclaimed
}

// IsValidRefRetentionTransition 引用行生命周期推进：
// 正常↔待回收（回收任务提交/取消），待回收→已回收（执行完成）。
// 已回收不可回到正常：产物已删，只能由重新拼接的新回放产生新引用行。
func IsValidRefRetentionTransition(from, to int32) bool {
	if from == to {
		return true
	}
	switch from {
	case RefRetentionStateNormal:
		return to == RefRetentionStatePending
	case RefRetentionStatePending:
		return to == RefRetentionStateNormal || to == RefRetentionStateReclaimed
	default:
		return false
	}
}

// isDuplicateErr 识别 MySQL 唯一索引冲突（错误号 1062 / Duplicate entry）。
// 与 services/notification/model/common.go 同样的做法：按错误文本判定，
// 不引入 go-sql-driver/mysql 的 *MySQLError（本仓库禁止新增依赖）。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}
