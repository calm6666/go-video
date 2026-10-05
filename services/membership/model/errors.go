package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// membership 域哨兵错误。logic 层直接返回这些错误，
// gateway（app/admin）负责把它们映射为 HTTP 响应信封的 code 与 gRPC status。
//
// 重要口径：判定类接口（CheckEntitlement 等）的「未通过」不是错误，而是
// rpc.EntitlementReason 结论值；只有「读不出可信结论」（DB 故障、参数非法）才是错误。
// 任何实现都不得把 DB 错误折叠成 granted=false 的正常未开通结论。
var (
	// ErrNotImplemented 预留给「尚未实现的用例」：返回它而不是零值伪成功（AGENTS.md §9 禁止假实现）。
	// 当前 16 个 RPC 均已实现，本错误没有任何引用点；将来契约加方法时才用它占位。
	ErrNotImplemented = errors.New("membership: not implemented")

	// --- 通用参数 ---
	// ErrInvalidMid mid 非正数（游客态由调用方自行判定不通过，不打到会员域）。
	ErrInvalidMid = errors.New("membership: invalid mid")
	// ErrInvalidVipType vip_type 不在 PREMIUM/PREMIUM_PLUS 之内。
	ErrInvalidVipType = errors.New("membership: invalid vip_type")
	// ErrRequestIdRequired 写接口缺少幂等键 request_id。
	ErrRequestIdRequired = errors.New("membership: request_id required")
	// ErrRequestIdReused 同一 request_id 携带了不同的关键参数：
	// 幂等键只能用于重放同一请求，不允许借它改口径，必须返回冲突而不是静默生效。
	ErrRequestIdReused = errors.New("membership: request_id reused with different parameters")
	// ErrOperatorRequired 运营写接口缺少操作者身份（由网关按会话渲染）。
	ErrOperatorRequired = errors.New("membership: operator required")
	// ErrFieldTooLong 字段长度超过列宽；必须在落库前拒掉，
	// 不能让 MySQL 报 "Data too long"——那种错误没有字段名，也来不及回滚台账。
	ErrFieldTooLong = errors.New("membership: field exceeds column width")
	// ErrReasonRequired 有后果的动作（收回、上下架）缺少理由。
	ErrReasonRequired = errors.New("membership: reason required")
	// ErrConcurrentUpdate CAS 版本不匹配（expected_version 过期），调用方需重读后重试。
	ErrConcurrentUpdate = errors.New("membership: concurrent version update")

	// --- 套餐 ---
	// ErrPlanIdentifierRequired GetPlan 需要 plan_id 或 plan_code 至少一个。
	ErrPlanIdentifierRequired = errors.New("membership: plan_id or plan_code required")
	// ErrPlanNotFound 套餐不存在。
	ErrPlanNotFound = errors.New("membership: plan not found")
	// ErrPlanCodeRequired 套餐缺少对外稳定编码 plan_code。
	ErrPlanCodeRequired = errors.New("membership: plan_code required")
	// ErrPlanNameRequired 套餐缺少展示名。
	ErrPlanNameRequired = errors.New("membership: plan name required")
	// ErrPlanIdentifierMismatch plan_id 与 plan_code 指向的不是同一个套餐，
	// 调用方传参自相矛盾；必须让它重读后重试，不能猜它想改哪一个。
	ErrPlanIdentifierMismatch = errors.New("membership: plan_id and plan_code point to different plans")
	// ErrInvalidPlanDuration duration_days/unit_count 非正，或换算后超过单次上限。
	ErrInvalidPlanDuration = errors.New("membership: invalid plan duration")
	// ErrInvalidPlanPrice 价格为负、促销价不低于原价，或促销价非法。
	ErrInvalidPlanPrice = errors.New("membership: invalid plan price")
	// ErrUnsupportedCurrency 币种不在允许集合（本轮只放开 CNY）。
	ErrUnsupportedCurrency = errors.New("membership: unsupported currency")
	// ErrInvalidPlatform 套餐平台枚举非法（PLAN_PLATFORM_UNSPECIFIED 或越界）。
	ErrInvalidPlatform = errors.New("membership: invalid plan platform")
	// ErrInvalidPlanState 套餐状态枚举非法。
	ErrInvalidPlanState = errors.New("membership: invalid plan sale state")
	// ErrInvalidPlanStateTransition 上下架状态机非法迁移
	//（只允许 DRAFT→ON_SALE、ON_SALE→OFF_SALE、OFF_SALE→ON_SALE）。
	ErrInvalidPlanStateTransition = errors.New("membership: invalid plan state transition")
	// ErrPlanSpecImmutable 套餐已离开 DRAFT 后不得改档位/时长/价格/币种：
	// 这类变更会对已下单用户追溯生效，必须新建 DRAFT 版本再上下架切换。
	ErrPlanSpecImmutable = errors.New("membership: plan vip_type/duration/price are immutable after leaving DRAFT, create a new draft plan")
	// ErrPlanPlatformsRequired 套餐至少要有一个可见平台，否则任何端都买不到。
	ErrPlanPlatformsRequired = errors.New("membership: at least one platform required")

	// --- 权益码 ---
	// ErrEntitlementCodeRequired 权益码为空。
	ErrEntitlementCodeRequired = errors.New("membership: entitlement code required")
	// ErrEntitlementNotFound 权益码在目录里不存在（判定侧必须暴露为 CODE_UNKNOWN，不得放行）。
	ErrEntitlementNotFound = errors.New("membership: entitlement code not found")

	// --- 会员身份 ---
	// ErrMembershipNotFound 该 (mid, vip_type) 从未开通过，无法续期/收回/签约。
	ErrMembershipNotFound = errors.New("membership: membership not found")
	// ErrInvalidGrantDelta delta_days 非正，或绝对值超过 MaxGrantDeltaDays。
	ErrInvalidGrantDelta = errors.New("membership: invalid delta_days")
	// ErrGrantSourceRequired 授予来源缺失或非法。
	ErrGrantSourceRequired = errors.New("membership: grant source required")
	// ErrGrantSourceNeedsOrder ADMIN_OPS/EXPERIENCE 之外的来源必须能回溯到订单或资金流水，
	// 否则等于给「无凭据的开通」开门。
	ErrGrantSourceNeedsOrder = errors.New("membership: biz_order_no or payment_no required for this grant source")
	// ErrAutoRenewUnsupported 目标身份不允许自动续费（套餐未支持或已过期）。
	ErrAutoRenewUnsupported = errors.New("membership: auto renew unsupported")
	// ErrAutoRenewChannelRequired 开启自动续费缺少渠道标识。
	ErrAutoRenewChannelRequired = errors.New("membership: auto renew channel required")
	// ErrAutoRenewChannelRejected 渠道不在允许集合：沙箱环境只接受 SANDBOX，
	// 真实代扣渠道一律拒绝，避免落库一个永远不会生效的协议位。
	ErrAutoRenewChannelRejected = errors.New("membership: auto renew channel not allowed")

	// --- 批处理 ---
	// ErrInvalidExpireRange 到期区间非法（from >= to）。
	ErrInvalidExpireRange = errors.New("membership: invalid expire scan range")
	// ErrInvalidPlanCode admin 查询关键字非法。
	ErrInvalidQueryFilter = errors.New("membership: invalid query filter")
)

// Fingerprint 把关键参数压成 sha256 hex，用于「同 request_id 不同参数」的冲突判定。
// 逐段加 0x00 分隔，避免 ["ab","c"] 与 ["a","bc"] 折叠成同一个指纹。
func Fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// isDuplicateErr 识别 MySQL 唯一索引冲突（1062 / Duplicate entry）。
// 本仓库不 import 驱动专有错误类型（换驱动或加代理层就会失效），改用报文判定，
// 并且只在「确实预期唯一索引兜底」的分支上使用，绝不因为「大概是 1062」吞掉其它失败。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}
