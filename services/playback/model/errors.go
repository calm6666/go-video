package model

import "errors"

// playback 域错误。gRPC 直接返回这些哨兵错误，由 gateway/app 映射为统一 HTTP 响应信封的
// 非零 code；错误消息保持脱敏，不包含 Token、私钥或 SQL 片段（AGENTS.md §6）。
var (
	// ErrInvalidContentType content_type 不在 ContentType 取值范围内。
	ErrInvalidContentType = errors.New("playback: invalid content_type")
	// ErrInvalidContentID content_id 非正数。
	ErrInvalidContentID = errors.New("playback: invalid content_id")
	// ErrInvalidObjectKey 播放目标对象路径缺失或非法。
	ErrInvalidObjectKey = errors.New("playback: invalid object key")
	// ErrInvalidPlatform platform 不在 Platform 取值范围内。
	ErrInvalidPlatform = errors.New("playback: invalid platform")
	// ErrMissingRequestID 缺少幂等键。
	ErrMissingRequestID = errors.New("playback: request_id is required")
	// ErrMissingRegion PGC 内容未提供地区代码，无法校验版权窗口。
	ErrMissingRegion = errors.New("playback: region is required for pgc content")
	// ErrMissingSessionID 缺少 session_id。
	ErrMissingSessionID = errors.New("playback: session_id is required")
	// ErrSessionNotFound 播放会话不存在。
	ErrSessionNotFound = errors.New("playback: session not found")
	// ErrSessionExpired 会话（及其授权）已过期。客户端必须用新的 request_id 重新申请，
	// playback 不在幂等重放里延长授权，避免同一 request_id 反复续命。
	ErrSessionExpired = errors.New("playback: session expired, request a new token")
	// ErrSessionRevoked 播放会话已被撤销（版权撤回/风控处置）。
	ErrSessionRevoked = errors.New("playback: session revoked")
	// ErrCopyrightWindowUnavailable PGC 版权窗口不可用（未授权、过期、撤权或地区不匹配）。
	ErrCopyrightWindowUnavailable = errors.New("playback: copyright window unavailable")
	// ErrRightsUnavailable rights 服务未配置或调用失败；此时拒绝签发，不伪造成功。
	ErrRightsUnavailable = errors.New("playback: rights service unavailable")
	// ErrInvalidHeartbeat 心跳的播放位置/时长非法。
	ErrInvalidHeartbeat = errors.New("playback: invalid heartbeat position_ms/duration_ms")
	// ErrSessionIDConflict session_id/request_id 已存在但归属不同用户或内容（幂等键串用）。
	ErrSessionIDConflict = errors.New("playback: session id conflict")
	// ErrSignerMisconfigured 签名器构造失败（例如生产模式关闭了防盗链）。
	ErrSignerMisconfigured = errors.New("playback: signer misconfigured")
)

// 播放会话状态（playback_session.state），与 rpc.SessionState 编号一致。
const (
	SessionStateActive  = 1 // 有效，可回源
	SessionStateExpired = 2 // 已过期
	SessionStateRevoked = 3 // 已撤销
)

// 内容类型（playback_session.content_type），与 rpc.ContentType 编号一致。
// 注意：与 rights.ContentType 的编号不同，跨服务调用必须走 repository 的映射函数。
const (
	ContentTypeUGC = 1 // UGC/PUGC 稿件
	ContentTypePGC = 2 // 版权内容的一集
)

// 客户端平台（playback_session.platform），与 rpc.Platform 编号一致。
const (
	PlatformAndroid = 1
	PlatformIOS     = 2
	PlatformHarmony = 3
	PlatformDesktop = 4
)

// Outbox 发布状态（playback_outbox.state）。
const (
	OutboxStatePending   = 0 // 待发布
	OutboxStatePublished = 1 // 已发布
	OutboxStateFailed    = 2 // 超过最大重试，转人工处理
)

// 领域事件类型（Topic = event_type + ".v" + schema_version，见 docs/api-and-events.md §4/§5）。
const (
	// EventPlaybackHeartbeat 播放心跳与质量指标 → event-collector/spm。
	// 这是 docs/api-and-events.md §5 核心事件表中 playback 唯一的产出事件。
	EventPlaybackHeartbeat = "playback.heartbeat"
	// EventSchemaVersion 当前事件 schema 版本。
	EventSchemaVersion = 1
	// Producer 事件生产者标识。
	Producer = "playback"
	// AggregateTypeSession 事件聚合根类型。
	AggregateTypeSession = "playback_session"
)

// MaxDurationMS 是心跳总时长的合理上限（48 小时），超过视为客户端异常数据。
const MaxDurationMS int64 = 48 * 60 * 60 * 1000
