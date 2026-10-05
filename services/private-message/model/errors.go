package model

import (
	"errors"
	"strconv"
)

// private-message 域哨兵错误。
// logic 层直接返回这些错误，gateway/app 负责映射为 HTTP 响应信封的 code（AGENTS.md §6）。
// 错误文案必须可安全外发：不得包含 SQL 片段、密文、正文或下游密钥。
var (
	// ErrNotImplemented 契约轮的占位错误：本轮只落契约、model 与迁移，
	// 生成的 logic 骨架一律返回该错误，禁止伪造成功（AGENTS.md §9）。
	// 逻辑轮（批次 A1-D1 之后的 logic 实现轮）逐个方法删除。
	ErrNotImplemented = errors.New("private-message: not implemented in contract round")

	// ErrInvalidMid mid 非法（私信不支持游客）。
	ErrInvalidMid = errors.New("private-message: invalid mid")
	// ErrSelfConversation 不能给自己发私信。
	ErrSelfConversation = errors.New("private-message: cannot message self")
	// ErrConversationNotFound 会话不存在。
	ErrConversationNotFound = errors.New("private-message: conversation not found")
	// ErrNotConversationMember 调用者不是该会话成员，越权访问。
	ErrNotConversationMember = errors.New("private-message: not a member of the conversation")
	// ErrMessageNotFound 消息不存在。
	ErrMessageNotFound = errors.New("private-message: message not found")
	// ErrContentEmpty 文本消息正文为空。
	ErrContentEmpty = errors.New("private-message: content is empty")
	// ErrContentTooLong 正文超过 PrivateMessage.MaxTextLength 上限（按 rune 计）。
	ErrContentTooLong = errors.New("private-message: content too long")
	// ErrMediaRefRequired 图片/语音/视频/分享消息缺少 media_ref。
	ErrMediaRefRequired = errors.New("private-message: media_ref required")
	// ErrInvalidMsgType 载体类型非法。
	ErrInvalidMsgType = errors.New("private-message: invalid msg type")
	// ErrClientMsgIDRequired 缺少幂等键 client_msg_id。
	ErrClientMsgIDRequired = errors.New("private-message: client_msg_id required (<=64 chars)")
	// ErrIdempotencyKeyRequired 缺少幂等键（运营侧接口使用）。
	ErrIdempotencyKeyRequired = errors.New("private-message: idempotency_key required (<=128 chars)")
	// ErrInvalidCursor 游标无法解析。
	ErrInvalidCursor = errors.New("private-message: invalid cursor")
	// ErrInvalidPage 分页参数非法（page size 未落到 model 层默认区间）。
	ErrInvalidPage = errors.New("private-message: invalid page size")
	// ErrPsTooLarge 每页大小超过服务端上限。
	ErrPsTooLarge = errors.New("private-message: ps exceeds server limit")
	// ErrInvalidStateTransition 消息状态机非法迁移（例如已撤回再撤、已驳回再改判）。
	ErrInvalidStateTransition = errors.New("private-message: invalid state transition")
	// ErrConcurrentUpdate 乐观锁/游标被并发修改，调用方可安全重试。
	ErrConcurrentUpdate = errors.New("private-message: concurrent state update")
	// ErrWithdrawWindowClosed 超过本人撤回时间窗。
	ErrWithdrawWindowClosed = errors.New("private-message: withdraw window closed")
	// ErrWithdrawForbidden 无权撤回该消息（非发送者且非运营/审核）。
	ErrWithdrawForbidden = errors.New("private-message: withdraw forbidden")
	// ErrOperatorRequired 运营/管理员身份缺失（ListReports、HandleReport、Purge 等）。
	ErrOperatorRequired = errors.New("private-message: operator_mid required")
	// ErrReportNotFound 举报记录不存在。
	ErrReportNotFound = errors.New("private-message: report not found")
	// ErrInvalidReportAction 举报处置动作非法。
	ErrInvalidReportAction = errors.New("private-message: invalid report action")
	// ErrInvalidVerdict 审核结论非法（UNSPECIFIED 一律拒绝）。
	ErrInvalidVerdict = errors.New("private-message: invalid moderation verdict")
	// InvalidAllowFrom 接收范围取值非法。
	ErrInvalidAllowFrom = errors.New("private-message: invalid allow_from")
	// ErrBlockedByPeer 被对方黑名单或接收范围门槛拒收（黑名单真值在 social-graph）。
	ErrBlockedByPeer = errors.New("private-message: rejected by receiver anti-spam setting")
	// ErrRiskDenied risk-control 裁决拒绝该次发送。
	ErrRiskDenied = errors.New("private-message: denied by risk control")
	// ErrConversationFrozen 会话被风控冻结。
	ErrConversationFrozen = errors.New("private-message: conversation frozen")
	// ErrSocialGraphNotConfigured 未配置 social-graph RPC。
	// 反骚扰门禁不能默认放行：未配置时返回错误而不是“允许发送”。
	ErrSocialGraphNotConfigured = errors.New("private-message: social-graph rpc not configured")
	// ErrRiskControlNotConfigured 未配置 risk-control RPC，同上，不可伪造“已风控”。
	ErrRiskControlNotConfigured = errors.New("private-message: risk-control rpc not configured")
	// ErrModerationNotConfigured 未配置 moderation-orchestrator RPC，
	// 机审门禁开启时不能把未送审的私信标成可见。
	ErrModerationNotConfigured = errors.New("private-message: moderation rpc not configured")
	// ErrCipherKeyMissing 缺少内容加密密钥（Cipher.KeyID/主密钥未从 Secret 注入）。
	// 缺失时禁止退化成明文落库（AGENTS.md §6）。
	ErrCipherKeyMissing = errors.New("private-message: message cipher key missing")
	// ErrDecryptFailed 正文解密失败（密钥轮换遗漏或数据损坏），
	// 对外不区分“无权限”和“解密失败”，避免侧信道探测。
	ErrDecryptFailed = errors.New("private-message: content decrypt failed")
	// ErrBatchLimitTooLarge 清理批量超过配置上限。
	ErrBatchLimitTooLarge = errors.New("private-message: batch_limit exceeds server limit")
	// ErrInvalidRetentionWindow 留存截止点非法（未来时间或越出可清理区间）。
	// 清理是不可逆操作：截止点被写歪（例如误传 now+10 年）等于清空全部正文，
	// 因此宁可拒绝也不按「看起来能用」的入参执行。
	ErrInvalidRetentionWindow = errors.New("private-message: invalid retention cutoff")
)

// PairKey 生成单聊会话的规范化唯一键：mid 升序 + 冒号分隔。
//
// 会话唯一性依赖该键（pm_conversation.uniq_pair_key），(a,b) 与 (b,a) 必须落到同一行，
// 因此排序在这里定死，logic 层不得自行拼接。
func PairKey(a, b int64) string {
	if a > b {
		a, b = b, a
	}
	return strconv.FormatInt(a, 10) + ":" + strconv.FormatInt(b, 10)
}
