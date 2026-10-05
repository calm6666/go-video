package model

import "strconv"

// 投递退避与状态机纯函数（ec_pending_delivery / ec_dead_letter / ec_dispatch_policy）。

// DeliveryState* 投递状态，取值与 rpc.DeliveryState、ec_pending_delivery.state 一致，
// 变更必须同步 proto 与迁移 SQL 注释（AGENTS.md §5：状态用显式枚举，不做位掩码）。
const (
	// DeliveryStateNone 不投递（被采样丢弃、重复计数或校验拒绝）。
	DeliveryStateNone int32 = 1
	// DeliveryStatePending 待投递。
	DeliveryStatePending int32 = 2
	// DeliveryStateSent 已投递到 MQ。
	DeliveryStateSent int32 = 3
	// DeliveryStateRetrying 投递失败，退避中。
	DeliveryStateRetrying int32 = 4
	// DeliveryStateDead 超过最大次数，已转 ec_dead_letter。
	DeliveryStateDead int32 = 5
)

// ValidDeliveryState 判定取值是否落在已开放枚举内。
func ValidDeliveryState(s int32) bool { return s >= DeliveryStateNone && s <= DeliveryStateDead }

// DeliveryStateOpen 判断该状态是否仍占用投递容量（未进终态）。
// SENT/DEAD/NONE 是终态，不能再被 ClaimDue 捞起，否则会把已发送事件重复投进 MQ。
func DeliveryStateOpen(s int32) bool {
	return s == DeliveryStatePending || s == DeliveryStateRetrying
}

// BatchState* 批次接收状态，与 rpc.BatchState、ec_ingest_batch.state 一致。
const (
	BatchStateReceived    int32 = 1
	BatchStateValidated   int32 = 2
	BatchStateDispatching int32 = 3
	BatchStateDispatched  int32 = 4
	BatchStatePartial     int32 = 5
	BatchStateRejected    int32 = 6
)

// ValidBatchState 判定取值是否落在已开放枚举内。
func ValidBatchState(s int32) bool { return s >= BatchStateReceived && s <= BatchStateRejected }

// BatchStateTerminal 批次是否已终结（终态不得回退到 RECEIVED，否则幂等回放会重跑校验）。
func BatchStateTerminal(s int32) bool {
	return s == BatchStateDispatched || s == BatchStateRejected
}

// Decision* 单事件处理结论，与 rpc.EventDecision 一致。
const (
	DecisionAccepted   int32 = 1
	DecisionDuplicated int32 = 2
	DecisionRejected   int32 = 3
	DecisionSampledOut int32 = 4
	DecisionDeferred   int32 = 5
)

// ValidDecision 判定取值是否落在已开放枚举内。
func ValidDecision(s int32) bool { return s >= DecisionAccepted && s <= DecisionDeferred }

// DecisionStored 结论是否需要落 ec_event_record 台账。
// 五类结论都要留痕（含 SAMPLED_OUT），否则事后无法解释「客户端说报了、库里没有」。
func DecisionStored(d int32) bool { return ValidDecision(d) && d != DecisionDeferred }

// Reason* 拒绝原因码，与 rpc.RejectReason 一致（这里只列台账写入用的区间断言）。
const (
	ReasonNone            int32 = 1
	ReasonMaxEnumerated   int32 = 24
	ReasonRateLimitedCode int32 = 18
	ReasonStoreFailedCode int32 = 23
)

// ValidReason 判定原因码是否落在 proto 已定义区间，越界一律写成 ReasonInternal 之外？
// 不是：越界视为编程错误，logic 必须改用已定义枚举，避免下游按未知码做错误归因。
func ValidReason(r int32) bool { return r >= ReasonNone && r <= ReasonMaxEnumerated }

// PolicyState* 策略版本状态，与 rpc.PolicyState、ec_dispatch_policy.state 一致。
const (
	PolicyStateDraft    int32 = 1
	PolicyStateActive   int32 = 2
	PolicyStateArchived int32 = 3
)

// ValidPolicyState 判定取值是否落在已开放枚举内。
func ValidPolicyState(s int32) bool { return s >= PolicyStateDraft && s <= PolicyStateArchived }

// CanTransitPolicyState 策略状态机（互斥、单向）：
//
//	DRAFT → ACTIVE → ARCHIVED
//	  └──→ (废弃) ARCHIVED        ARCHIVED 不可复活
//
// ACTIVE 只能由 Activate 路径写入（同事务里把旧 ACTIVE 归档 + uniq_active 兜底），
// 任何「ARCHIVED → ACTIVE」的回滚都必须改为新建版本，避免同一 version 语义漂移。
func CanTransitPolicyState(from, to int32) bool {
	if !ValidPolicyState(from) || !ValidPolicyState(to) || from == to {
		return false
	}
	switch from {
	case PolicyStateDraft:
		return to == PolicyStateActive || to == PolicyStateArchived
	case PolicyStateActive:
		return to == PolicyStateArchived
	default:
		return false
	}
}

// ActiveFlag 返回 ACTIVE 行的唯一键标记值（非 ACTIVE 必须写 NULL）。
// ec_dispatch_policy.uniq_active(active_flag) 依赖「MySQL 唯一索引允许多个 NULL」，
// 从数据库层保证同一时刻至多一版生效策略；切换失败会撞唯一键 → IsDuplicate。
func ActiveFlag(state int32) interface{} {
	if state == PolicyStateActive {
		return 1
	}
	return nil
}

// NextRetryAt 计算第 attempts 次失败后的重试时间（指数退避 + 上限封顶）：
//
//	delay = min(base * 2^(attempts-1), maxSeconds)
//
// attempts<=0 表示还没失败过，返回 now+base（首次投递即入队时的时间）。
// base/max<=0 时退化为立即重试（now），但绝不允许返回过去时间导致扫描任务空转：
// 因此结果至少为 now。序列对 attempts 单调不减，便于单测锁定。
func NextRetryAt(now int64, attempts int32, baseSeconds, maxSeconds int64) int64 {
	base := baseSeconds
	if base <= 0 {
		base = 1
	}
	max := maxSeconds
	if max < base {
		max = base
	}
	if attempts <= 0 {
		return now + base
	}
	shift := attempts - 1
	if shift > 30 {
		shift = 30 // 防溢出：2^30 * base 早已超过任何合理上限
	}
	delay := base << shift
	if delay <= 0 || delay > max { // 溢出或越界都封顶
		delay = max
	}
	return now + delay
}

// ShouldDeadLetter 判定本次失败后是否直接转死信：attempts 达到策略上限即转，
// 比较用 >= 而不是 ==，避免历史行被人工改大 attempts 后永远逃过死信。
func ShouldDeadLetter(attempts, maxAttempts int32) bool {
	if maxAttempts <= 0 {
		return true // 配置为 0 次重试 = 不重试，失败即死信（比无限重试更安全）
	}
	return attempts >= maxAttempts
}

// LeaseKey 租约持有者标识（worker id / pod 名 + 轮次），写进 ec_pending_delivery.lease_owner。
func LeaseKey(worker string, round int64) string {
	if worker == "" {
		worker = "unknown"
	}
	return worker + ":" + strconv.FormatInt(round, 10)
}

// DeadLetter 状态字符串常量（proto DeadLetter.state 是 string，取值 open/replayed/discarded）。
const (
	DeadLetterOpen      = "open"
	DeadLetterReplayed  = "replayed"
	DeadLetterDiscarded = "discarded"
)

// ValidDeadLetterState 死信状态白名单判定（禁止任意字符串入库，否则运营筛选会漏）。
func ValidDeadLetterState(s string) bool {
	switch s {
	case DeadLetterOpen, DeadLetterReplayed, DeadLetterDiscarded:
		return true
	default:
		return false
	}
}

// DeadLetterTerminal 死信是否已处置完毕（重放/丢弃都是终态，重复重放按 skipped 计）。
func DeadLetterTerminal(s string) bool { return s == DeadLetterReplayed || s == DeadLetterDiscarded }
