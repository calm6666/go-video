package model

import "errors"

// danmaku 域哨兵错误。
// logic 层直接返回这些错误，gateway 负责映射为 HTTP 响应信封的 code。
var (
	// ErrInvalidOid oid 非法。
	ErrInvalidOid = errors.New("danmaku: invalid oid")
	// ErrInvalidMid mid 非法（弹幕不支持游客发送）。
	ErrInvalidMid = errors.New("danmaku: invalid mid")
	// ErrInvalidDmid dmid 非法。
	ErrInvalidDmid = errors.New("danmaku: invalid dmid")
	// ErrInvalidProgress progress_ms 非法。
	ErrInvalidProgress = errors.New("danmaku: invalid progress_ms")
	// ErrInvalidMode mode 非法。
	ErrInvalidMode = errors.New("danmaku: invalid mode")
	// ErrInvalidColor color 超出 RGB 范围。
	ErrInvalidColor = errors.New("danmaku: invalid color")
	// ErrContentEmpty 弹幕正文为空。
	ErrContentEmpty = errors.New("danmaku: content is empty")
	// ErrContentTooLong 弹幕正文超长。
	ErrContentTooLong = errors.New("danmaku: content too long")
	// ErrIdempotencyKeyRequired 缺少幂等键。
	ErrIdempotencyKeyRequired = errors.New("danmaku: idempotency_key or client_msg_id required")
	// ErrDanmakuNotFound 弹幕不存在。
	ErrDanmakuNotFound = errors.New("danmaku: danmaku not found")
	// ErrForbidden 无权操作该弹幕（非本人且非管理员）。
	ErrForbidden = errors.New("danmaku: operation forbidden")
	// ErrInvalidStateTransition 状态机非法迁移。
	ErrInvalidStateTransition = errors.New("danmaku: invalid state transition")
	// ErrConcurrentUpdate 状态被并发修改，本次写入未生效，调用方可安全重试。
	ErrConcurrentUpdate = errors.New("danmaku: concurrent state update")
	// ErrInvalidVerdict 审核结论非法。
	ErrInvalidVerdict = errors.New("danmaku: invalid moderation verdict")
	// ErrInvalidSegRange 分段窗口非法（起始大于结束）。
	ErrInvalidSegRange = errors.New("danmaku: invalid segment range")
	// ErrSegRangeTooLarge 分段窗口超过服务端上限。
	ErrSegRangeTooLarge = errors.New("danmaku: segment range too large")
	// ErrRateLimited 命中防刷屏限流。
	ErrRateLimited = errors.New("danmaku: rate limit exceeded")
	// ErrOperatorRequired 运营/管理员身份缺失。
	ErrOperatorRequired = errors.New("danmaku: operator_mid required")
	// ErrInvalidBlockWord 屏蔽词非法。
	ErrInvalidBlockWord = errors.New("danmaku: invalid block word")
	// ErrInvalidUserBlock 用户屏蔽参数非法。
	ErrInvalidUserBlock = errors.New("danmaku: invalid user block target")
	// ErrBlockWordNotFound 屏蔽词不存在。
	ErrBlockWordNotFound = errors.New("danmaku: block word not found")
	// ErrModerationNotConfigured 未配置 moderation-orchestrator RPC，
	// 机审开关打开时不能伪造“已送审”。
	ErrModerationNotConfigured = errors.New("danmaku: moderation rpc not configured")
	// ErrReportNotFound 举报记录不存在。
	ErrReportNotFound = errors.New("danmaku: report not found")
)
