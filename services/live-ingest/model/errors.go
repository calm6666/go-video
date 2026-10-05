package model

import "errors"

// ErrNotImplemented 表示本轮只落了契约、model 与迁移，该用例的业务实现尚未落地。
// logic 层把它原样返回给 gRPC，绝不用零值响应伪装成功（AGENTS.md §9）。
// 后续逻辑轮实现完成后，本哨兵应随对应 logic 一起删除。
var ErrNotImplemented = errors.New("live-ingest: not implemented in contract round")

// 推流协议位图，与 live_stream_key.protocol_mask / live_ingest_node.protocol_mask 列一致。
// 注意：位图取值（1/2/4）与 rpc.IngestProtocol 枚举取值（1/2/3）不是一回事，
// 转换只允许走 ProtocolMask，禁止把枚举值直接当位图存库。
const (
	// ProtocolMaskRtmp 允许 RTMP/RTMPS 接入。
	ProtocolMaskRtmp uint32 = 1 << 0
	// ProtocolMaskSrt 允许 SRT 接入。
	ProtocolMaskSrt uint32 = 1 << 1
	// ProtocolMaskWebrtc 允许 WebRTC 接入。
	ProtocolMaskWebrtc uint32 = 1 << 2
)

// 流状态，与 live_stream.state 列一致，并且与 live-room
// ReportStreamStateReq.stream_state 的取值严格对齐（1 IDLE、2 PUBLISHING、
// 3 INTERRUPTED、4 STOPPED）。这个编号是 live.state.v1 事件 payload 的一部分，
// 变更会破坏 live-room / live-gateway / live-media 的已落库投影，禁止重排。
const (
	// StreamStateIdle 已建档：接入鉴权通过，等待媒体帧真正到达。
	StreamStateIdle int32 = 1
	// StreamStatePublishing 推流中：帧持续到达且健康度在阈值内。
	StreamStatePublishing int32 = 2
	// StreamStateInterrupted 断流：心跳/帧中断，宽限期内允许重连。
	StreamStateInterrupted int32 = 3
	// StreamStateStopped 已停止：本次推流会话终态（重推分配新的 stream_id）。
	StreamStateStopped int32 = 4
)

// 流状态合法迁移矩阵（AGENTS.md §8：回调与上报只能推进合法状态）：
//
//	IDLE        → PUBLISHING（首帧到达） | STOPPED（鉴权后未推流超时 / 密钥吊销 / 运营停流）
//	PUBLISHING  → INTERRUPTED（心跳或帧中断） | STOPPED（正常下播 / 强制停流）
//	INTERRUPTED → PUBLISHING（宽限期内重连成功） | STOPPED（宽限期耗尽 / 主动停流）
//	STOPPED     → 终态，无出边；同态上报视为幂等 no-op，不算迁移。
//
// 一次「推流会话」= 一个 stream_id。断流重连复用同一 stream_id（否则无法与
// live-room 的 seq 守卫对齐），而下播后重新开播是新 stream_id。
var streamTransitions = map[int32][]int32{
	StreamStateIdle:        {StreamStatePublishing, StreamStateStopped},
	StreamStatePublishing:  {StreamStateInterrupted, StreamStateStopped},
	StreamStateInterrupted: {StreamStatePublishing, StreamStateStopped},
	StreamStateStopped:     {},
}

// 密钥状态，与 live_stream_key.state 列一致。
const (
	// KeyStateActive 生效，可用于接入鉴权。
	KeyStateActive int32 = 1
	// KeyStateRotating 轮转中的旧密钥：grace_until 前仍可用于断流重连。
	KeyStateRotating int32 = 2
	// KeyStateRetired 轮转宽限期结束后的旧密钥终态。
	KeyStateRetired int32 = 3
	// KeyStateExpired 超过 expire_at，需重新签发。
	KeyStateExpired int32 = 4
	// KeyStateRevoked 已吊销（禁播/泄露/风控），不可恢复。
	KeyStateRevoked int32 = 5
)

// 健康判定，与 live_stream.health_state 列和 live_stream_health_report 判定一致。
const (
	// HealthStateHealthy 正常。
	HealthStateHealthy int32 = 1
	// HealthStateDegraded 劣化：越过告警阈值但未断流。
	HealthStateDegraded int32 = 2
	// HealthStateCritical 危险：接近断流，logic 应触发 INTERRUPTED。
	HealthStateCritical int32 = 3
	// HealthStateNoData 无采样：超过 no_data 宽限期未收到健康上报（建档默认值）。
	HealthStateNoData int32 = 4
)

// 断流结束原因，与 live_stream_interruption.end_reason 列一致。
const (
	// InterruptionEndReconnected INTERRUPTED → PUBLISHING。
	InterruptionEndReconnected int32 = 1
	// InterruptionEndTimeout INTERRUPTED → STOPPED（宽限期耗尽）。
	InterruptionEndTimeout int32 = 2
	// InterruptionEndClosed INTERRUPTED → STOPPED（主播/运营主动停流）。
	InterruptionEndClosed int32 = 3
)

// 停流原因，与 live_stream.stop_reason / live_stream_event.stop_reason 列一致。
const (
	// StopReasonAnchorStop 主播正常下播。
	StopReasonAnchorStop int32 = 1
	// StopReasonNodeTimeout 接入节点心跳丢失且宽限期耗尽。
	StopReasonNodeTimeout int32 = 2
	// StopReasonUnhealthy 健康度持续危险，服务端主动切断。
	StopReasonUnhealthy int32 = 3
	// StopReasonRevoked 密钥被吊销引发的级联停流。
	StopReasonRevoked int32 = 4
	// StopReasonAdmin 运营强制停流。
	StopReasonAdmin int32 = 5
	// StopReasonKeyExpired 密钥过期前未推流，回收 IDLE 流。
	StopReasonKeyExpired int32 = 6
)

// 接入节点状态，与 live_ingest_node.state 列一致。
const (
	// NodeStateOnline 在线可分配。
	NodeStateOnline int32 = 1
	// NodeStateDraining 摘流中：不接新流，存量跑完下线。
	NodeStateDraining int32 = 2
	// NodeStateOffline 离线：心跳丢失或人工下线。
	NodeStateOffline int32 = 3
)

// 节点分配状态，与 live_node_assignment.state 列一致。
const (
	// AssignmentStateActive 生效中，占用节点配额。
	AssignmentStateActive int32 = 1
	// AssignmentStateReleased 已释放。
	AssignmentStateReleased int32 = 2
	// AssignmentStateMigrated 已迁移到新节点。
	AssignmentStateMigrated int32 = 3
)

// Outbox 发布状态，与 live_ingest_outbox.state 列一致。
// 取值与 rpc.OutboxState 枚举逐字对齐（0 保留给 UNSPECIFIED，不落库）。
const (
	// OutboxStatePending 待发布（含退避重试中）。
	OutboxStatePending int32 = 1
	// OutboxStatePublished 已发布。
	OutboxStatePublished int32 = 2
	// OutboxStateFailed 超过最大重试，等 RetryFailedEvents 人工放行。
	OutboxStateFailed int32 = 3
)

// CDN 回调判定结果，与 live_cdn_callback.verify_result 列一致。
const (
	// CallbackResultPending 尚未判定（留证写入时的初值）。
	CallbackResultPending int32 = 0
	// CallbackResultPassed 签名与时间窗都通过。
	CallbackResultPassed int32 = 1
	// CallbackResultBadSignature 签名不匹配。
	CallbackResultBadSignature int32 = 2
	// CallbackResultTimestampSkew 时间戳超出允许窗口。
	CallbackResultTimestampSkew int32 = 3
	// CallbackResultReplayed nonce 已出现过（重放）。
	CallbackResultReplayed int32 = 4
	// CallbackResultDomainUnbound 回调域名未绑定任何密钥/流。
	CallbackResultDomainUnbound int32 = 5
	// CallbackResultStreamNotFound 回调无法归属到已知流。
	CallbackResultStreamNotFound int32 = 6
)

// ProtocolMask 把 rpc.IngestProtocol 枚举值（1/2/3）转成 protocol_mask 位。
// 未知枚举返回 (0, false)，调用方必须拒绝而不是按 RTMP 兜底。
func ProtocolMask(protocol int32) (uint32, bool) {
	switch protocol {
	case 1:
		return ProtocolMaskRtmp, true
	case 2:
		return ProtocolMaskSrt, true
	case 3:
		return ProtocolMaskWebrtc, true
	default:
		return 0, false
	}
}

// ProtocolEnum 把 protocol_mask 单个位反查为 rpc.IngestProtocol 枚举值。
// 传入位图（多位于 1）时返回 (0, false)，避免把组合值当枚举写库。
func ProtocolEnum(mask uint32) (int32, bool) {
	switch mask {
	case ProtocolMaskRtmp:
		return 1, true
	case ProtocolMaskSrt:
		return 2, true
	case ProtocolMaskWebrtc:
		return 3, true
	default:
		return 0, false
	}
}

// ValidStreamState 判断是否为已定义的流状态取值。
func ValidStreamState(state int32) bool {
	switch state {
	case StreamStateIdle, StreamStatePublishing, StreamStateInterrupted, StreamStateStopped:
		return true
	default:
		return false
	}
}

// TerminalStreamState 判断是否终态（终态不可再迁移）。
func TerminalStreamState(state int32) bool {
	return state == StreamStateStopped
}

// CanTransitionStreamState 判断 from→to 是否是合法迁移。
// 同态（from==to）返回 false：那是幂等 no-op，由 logic 按「已应用」直接返回，
// 不得占用 seq、也不得产生新事件。
func CanTransitionStreamState(from, to int32) bool {
	if !ValidStreamState(from) || !ValidStreamState(to) || from == to {
		return false
	}
	for _, next := range streamTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ActiveStreamStates 返回「非终态」状态集合，供活跃指针与配额统计使用。
func ActiveStreamStates() []int32 {
	return []int32{StreamStateIdle, StreamStatePublishing, StreamStateInterrupted}
}

// AuthAllowedKeyState 判断密钥状态是否允许接入鉴权：
// ACTIVE 始终允许；ROTATING 只在宽限期内允许（便于 OBS 不重启切换）。
// 宽限期是否真的到期由 logic 用 grace_until 与当前时间比较决定，本函数只做状态位判断。
func AuthAllowedKeyState(state int32) bool {
	return state == KeyStateActive || state == KeyStateRotating
}

// live-ingest 域哨兵错误。logic 层直接返回这些错误，由 gRPC 层映射为稳定 status；
// 错误消息一律不含明文密钥、哈希、Vault 引用或厂商签名（AGENTS.md §7/§9）。
var (
	// ErrIdempotencyKeyRequired 写接口缺少 request_id / report_id / nonce。
	ErrIdempotencyKeyRequired = errors.New("live-ingest: idempotency key required")
	// ErrInvalidRoomId room_id 非法（<=0）。
	ErrInvalidRoomId = errors.New("live-ingest: invalid room_id")
	// ErrInvalidMid 主播或操作者 mid 非法（<=0）。
	ErrInvalidMid = errors.New("live-ingest: invalid mid")
	// ErrOperatorRequired 运营/管理员身份缺失或权限不足。
	ErrOperatorRequired = errors.New("live-ingest: operator forbidden")
	// ErrInvalidKeyId key_id 非法。
	ErrInvalidKeyId = errors.New("live-ingest: invalid key_id")
	// ErrInvalidStreamId stream_id 非法。
	ErrInvalidStreamId = errors.New("live-ingest: invalid stream_id")
	// ErrInvalidProtocol 推流协议为 UNSPECIFIED 或不在密钥允许集合内。
	ErrInvalidProtocol = errors.New("live-ingest: invalid ingest protocol")
	// ErrInvalidTtl 密钥有效期非法（<=0 或超过配置上限）。
	ErrInvalidTtl = errors.New("live-ingest: invalid key ttl")
	// ErrStreamKeyNotFound 密钥不存在。
	ErrStreamKeyNotFound = errors.New("live-ingest: stream key not found")
	// ErrStreamKeyRevoked 密钥已吊销，不可恢复。
	ErrStreamKeyRevoked = errors.New("live-ingest: stream key revoked")
	// ErrStreamKeyExpired 密钥已过期或被轮转为终态。
	ErrStreamKeyExpired = errors.New("live-ingest: stream key expired")
	// ErrStreamKeyNotUsable 密钥当前状态不允许该操作（如轮转已退役的旧密钥）。
	ErrStreamKeyNotUsable = errors.New("live-ingest: stream key state not usable")
	// ErrKeyRefUnresolvable 无法构造密钥的 Secret/Vault 引用（缺少 stream_name/代次等归因要素）。
	// 密钥材料不可用时必须拒签，绝不退化成「只存哈希不给引用」或回显更弱的凭据。
	ErrKeyRefUnresolvable = errors.New("live-ingest: stream key vault reference unavailable")
	// ErrPublishDenied 接入鉴权失败（不区分细节时统一用它，避免泄露密钥存在性）。
	ErrPublishDenied = errors.New("live-ingest: publish auth denied")
	// ErrStreamQuotaExceeded 同一密钥的并发非终态流超过 max_streams。
	ErrStreamQuotaExceeded = errors.New("live-ingest: stream quota exceeded")
	// ErrStreamNotFound 流不存在。
	ErrStreamNotFound = errors.New("live-ingest: stream not found")
	// ErrInvalidStreamState 上报的目标状态非法（UNSPECIFIED 或终态之外的未知值）。
	ErrInvalidStreamState = errors.New("live-ingest: invalid stream state")
	// ErrInvalidStateTransition 流状态机非法迁移（例如 STOPPED → PUBLISHING）。
	ErrInvalidStateTransition = errors.New("live-ingest: invalid state transition")
	// ErrConcurrentUpdate CAS 未命中：状态已被并发修改，调用方可按幂等语义重试。
	ErrConcurrentUpdate = errors.New("live-ingest: concurrent state update")
	// ErrSeqConflict expect_seq 与当前 seq 不一致（乱序或另一路上报抢先）。
	ErrSeqConflict = errors.New("live-ingest: seq conflict")
	// ErrTerminalStream 流已处于终态，本次上报按幂等成功返回但不产生迁移。
	ErrTerminalStream = errors.New("live-ingest: stream already stopped")
	// ErrNodeNotFound 接入节点不存在。
	ErrNodeNotFound = errors.New("live-ingest: ingest node not found")
	// ErrNodeNotAssignable 节点不可分配（离线、摘流中、配额已满）。
	ErrNodeNotAssignable = errors.New("live-ingest: ingest node not assignable")
	// ErrNoAvailableNode 没有满足协议/区域条件的可用节点。
	ErrNoAvailableNode = errors.New("live-ingest: no available ingest node")
	// ErrNodeProtocolMismatch 节点不支持请求的推流协议。
	ErrNodeProtocolMismatch = errors.New("live-ingest: node protocol mismatch")
	// ErrAssignmentNotFound 节点分配记录不存在或已释放。
	ErrAssignmentNotFound = errors.New("live-ingest: node assignment not found")
	// ErrLeaseNotHeld 调用方要求释放的租约不是当前生效的那条（节点不匹配）：
	// 显式冲突而不是静默成功，否则两个调度器会互相抵消配额。
	ErrLeaseNotHeld = errors.New("live-ingest: ingest node lease not held by caller")
	// ErrNoFailedEvents 没有处于失败态的事件可重试。
	ErrNoFailedEvents = errors.New("live-ingest: no failed events to retry")
	// ErrCallbackDomainUnbound 回调域名未绑定（防任意来源打回调）。
	ErrCallbackDomainUnbound = errors.New("live-ingest: cdn callback domain unbound")
	// ErrCallbackSignatureMismatch 回调签名不匹配。
	ErrCallbackSignatureMismatch = errors.New("live-ingest: cdn callback signature mismatch")
	// ErrCallbackTimestampSkew 回调时间戳超出允许窗口。
	ErrCallbackTimestampSkew = errors.New("live-ingest: cdn callback timestamp skew")
	// ErrCallbackReplayed 回调 nonce 已处理过（重放），按幂等成功返回首次结果。
	ErrCallbackReplayed = errors.New("live-ingest: cdn callback replayed")
	// ErrCallbackSignerUnavailable 回调签名密钥的 Secret/Vault 引用解析不出来（未注入或形态不支持）。
	// 无法算出 HMAC 就无从判定真伪：一律按未通过处理并留证，绝不默认放行。
	ErrCallbackSignerUnavailable = errors.New("live-ingest: cdn callback signer unavailable")
	// ErrCdnNotConfigured 未配置 CDN/媒体入口适配器：不伪造「已下发/已踢流」。
	ErrCdnNotConfigured = errors.New("live-ingest: cdn ingest gateway not configured")
	// ErrInvalidSampleMetrics 健康上报指标非法（负值、超出物理上限）。
	ErrInvalidSampleMetrics = errors.New("live-ingest: invalid health metrics")
	// ErrPageSizeInvalid 分页参数非法（pn/ps <= 0 时服务端取默认值，不返回该错误；
	// 仅在调用方显式要求超出硬上限且配置禁止夹取时使用）。
	ErrPageSizeInvalid = errors.New("live-ingest: invalid page size")
	// ErrReasonTooLong 审计/原因文本超过列宽：拒绝而不是静默截断，
	// 否则库里的文本与评审时看到的文本不一致。
	ErrReasonTooLong = errors.New("live-ingest: reason text too long")
	// ErrReasonRequired 破坏性写（吊销、强制停流）必须留归因文本，空文本次拒绝。
	ErrReasonRequired = errors.New("live-ingest: reason text required")
	// ErrCacheUnavailable 需要抢占幂等键却没有可用的缓存客户端：
	// 判定不了「是不是重放」就不执行副作用。
	ErrCacheUnavailable = errors.New("live-ingest: idempotency cache unavailable")
)
