package model

import (
	"errors"
	"strings"
)

// event-collector 域哨兵错误。
//
// 本服务只有 gRPC，错误由调用方（gateway/app 或 services/cron）映射为对外响应；
// 文案必须可安全外发：不得包含 SQL 片段、事件 payload 原文、IP/设备号明文或盐值。
var (
	// ErrNotImplemented 契约轮占位错误：本轮只落契约、model 与迁移 SQL，
	// goctl 生成的 15 个 logic 一律返回该错误，禁止返回空 Reply 伪造成功（AGENTS.md §9）。
	// 逻辑轮（任务 #41）逐个方法删除：实现一个、删一个，不允许整体放开。
	ErrNotImplemented = errors.New("event-collector: not implemented in contract round")

	// ErrDataSourceMissing DSN 未配置（启动期应已被 config.Validate 拦住，运行期再兜底）。
	ErrDataSourceMissing = errors.New("event-collector: mysql dsn not configured")

	// --- 入参校验（logic 层在写库前逐条判定，映射为 rpc.RejectReason） ---

	// ErrBatchIDRequired 缺少批次幂等键 batch_id（REJECT_BATCH_ID_MISSING）。
	ErrBatchIDRequired = errors.New("event-collector: batch_id required (<=64 chars)")
	// ErrEventIDRequired 缺少事件幂等键 event_id（REJECT_MISSING_EVENT_ID）。
	ErrEventIDRequired = errors.New("event-collector: event_id required (<=64 chars)")
	// ErrIdempotencyKeyRequired 运营/兜底接口缺少 idempotency_key。
	ErrIdempotencyKeyRequired = errors.New("event-collector: idempotency_key required (<=128 chars)")
	// ErrOperatorRequired 变更类接口缺少操作人（策略切换、死信重放）。
	ErrOperatorRequired = errors.New("event-collector: operator required")
	// ErrEventsRequired 批次里没有任何事件。
	ErrEventsRequired = errors.New("event-collector: events required")
	// ErrBatchTooLarge 单请求条数或字节超过策略上限（REJECT_BATCH_TOO_LARGE）。
	ErrBatchTooLarge = errors.New("event-collector: batch exceeds size limit")
	// ErrInvalidPage 每页条数越界。
	ErrInvalidPage = errors.New("event-collector: invalid page size")
	// ErrInvalidCursor 游标无法解析。
	ErrInvalidCursor = errors.New("event-collector: invalid cursor")
	// ErrBatchLimitTooLarge limit 超过服务端上限。
	ErrBatchLimitTooLarge = errors.New("event-collector: limit exceeds server max")

	// --- 记录不存在（Find* 统一返回这些哨兵，logic 翻译为 reply.found=false） ---

	// ErrBatchNotFound 批次台账不存在。
	ErrBatchNotFound = errors.New("event-collector: ingest batch not found")
	// ErrRecordNotFound 事件台账不存在。
	ErrRecordNotFound = errors.New("event-collector: event record not found")
	// ErrPolicyNotFound 策略版本不存在。
	ErrPolicyNotFound = errors.New("event-collector: dispatch policy not found")
	// ErrNoActivePolicy 当前无生效策略：采集必须退回保守默认（全量、不采样）并报警，
	// 绝不允许「查不到策略就当采样率为 0」把事件静默丢掉。
	ErrNoActivePolicy = errors.New("event-collector: active dispatch policy not found")
	// ErrPendingNotFound 投递台账不存在（已被清理或 ID 错误）。
	ErrPendingNotFound = errors.New("event-collector: pending delivery not found")
	// ErrDeadLetterNotFound 死信不存在。
	ErrDeadLetterNotFound = errors.New("event-collector: dead letter not found")

	// --- 状态机与并发 ---

	// ErrInvalidStateTransition 非法状态迁移（如已 ARCHIVED 的策略改回 DRAFT）。
	ErrInvalidStateTransition = errors.New("event-collector: invalid state transition")
	// ErrActivePolicyImmutable ACTIVE 策略不允许原地修改，只能新建草稿版本再切换。
	ErrActivePolicyImmutable = errors.New("event-collector: active policy cannot be edited in place")
	// ErrPolicyNotDraft Upsert 只能作用于 DRAFT 行。
	ErrPolicyNotDraft = errors.New("event-collector: only draft policy can be updated")
	// ErrConcurrentUpdate 乐观校验失败（expected_current_version 不匹配或并发切换撞唯一键），
	// 调用方可安全重试。
	ErrConcurrentUpdate = errors.New("event-collector: concurrent policy activation, retry")
	// ErrIdempotencyKeyConflict 同一个幂等键被两个不同的动作/批次复用（batch_id 已存在但
	// idempotency_key 不同）。这不是「重试」而是调用方把两个动作写成了同一个键，
	// 必须报错而不是回放别人的结论。
	ErrIdempotencyKeyConflict = errors.New("event-collector: idempotency_key conflicts with stored batch")
	// ErrDeadLetterHandled 死信已是 replayed/discarded 终态，重放被跳过。
	ErrDeadLetterHandled = errors.New("event-collector: dead letter already handled")

	// --- 隐私与下游依赖 ---

	// ErrSaltMissing 盐值未能从 Secret/环境变量读到（DispatchPolicy.SaltRef 指向的变量为空）。
	// 缺失时禁止退化成「不加盐直接哈希」或「落库明文 IP/设备号」，采集一律拒绝并报警。
	ErrSaltMissing = errors.New("event-collector: privacy salt unavailable from configured ref")
	// ErrPrivacyFieldForbidden payload 含禁止入库字段（明文 ip/imei/phone/token 等），
	// 整条事件拒绝（REJECT_PRIVACY_FIELD）。
	ErrPrivacyFieldForbidden = errors.New("event-collector: payload contains forbidden privacy field")
	// PayloadTooLarge 单事件 payload 超过策略上限（REJECT_PAYLOAD_TOO_LARGE）。
	ErrPayloadTooLarge = errors.New("event-collector: payload exceeds max bytes")
	// ErrDispatcherMissing 未配置 MQ 投递通道（common/kq 写入器）。
	// 缺失时事件必须留在 ec_pending_delivery 由 RetryPendingDelivery 兜底，
	// 不允许「发不出去就当已发送」把台账标成 SENT。
	ErrDispatcherMissing = errors.New("event-collector: mq dispatcher not configured")
	// ErrSpmRPCNotConfigured 未配置 spm RPC（schema 真值在 spm 侧，见 README 疑点）。
	ErrSpmRPCNotConfigured = errors.New("event-collector: spm rpc not configured")
	// ErrRiskControlNotConfigured 未配置 risk-control RPC：限流/黑名单判定不能伪造「已通过」。
	ErrRiskControlNotConfigured = errors.New("event-collector: risk-control rpc not configured")
)

// IsDuplicate 判断是否唯一键冲突（幂等重放路径依赖它，不靠「先查后插」避免竞态）。
//
// MySQL 1062 / "Duplicate entry" 都可能出现在 error 链里，这里做字符串判定，
// 并把结果收敛成 bool，避免 logic 层到处写 strings.Contains。
func IsDuplicate(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}

// IsNotFound 判断是否为「记录不存在」，供 logic 统一翻译成 found=false。
func IsNotFound(err error) bool {
	return errors.Is(err, ErrBatchNotFound) ||
		errors.Is(err, ErrRecordNotFound) ||
		errors.Is(err, ErrPolicyNotFound) ||
		errors.Is(err, ErrNoActivePolicy) ||
		errors.Is(err, ErrPendingNotFound) ||
		errors.Is(err, ErrDeadLetterNotFound)
}
