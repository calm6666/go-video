// Package policy 是 risk-control 的决策引擎，只做纯计算：
// 输入是一次裁决所需的最小事实（Facts），输出是可解释的 Result。
//
// 设计约束（AGENTS.md §5/§6/§7）：
//   - 引擎不访问 Redis/MySQL，所有事实由 repository 装载，因此决策可单测；
//   - 裁决不是布尔值：必须返回 decision、score、命中规则（含版本）、决策依据 basis；
//   - 依赖不可用时按显式降级策略产出裁决并在 Result.Degraded 中标记，
//     不伪造命中、不把「读不到」当成「没风险」；
//   - 输入结构体只携带脱敏后的字段（mid、device_hash、ip_hash），
//     不接受明文 IP、手机号、身份证或设备号原文。
package policy

// 决策依据（basis）。稳定字符串，客户端与日志按此分类，不得随意改名。
const (
	// BasisBlacklist 命中黑名单，直接拒绝。
	BasisBlacklist = "blacklist"
	// BasisWhitelist 命中白名单，跳过规则评估。
	BasisWhitelist = "whitelist"
	// BasisPunishment 处于生效处罚期。
	BasisPunishment = "punishment"
	// BasisRules 由规则评估得出非放行裁决。
	BasisRules = "rules"
	// BasisNoRule 完成规则评估且无命中。
	BasisNoRule = "no_rule"
	// BasisFallbackDB 规则/名单/处罚所在 DB 不可用，按配置降级。
	BasisFallbackDB = "fallback_db_unavailable"
	// BasisFallbackLocalLimit Redis 计数器不可用且进程内兜底限流已触发。
	BasisFallbackLocalLimit = "fallback_local_rate_limited"
)

// 建议动作文案 code。服务端只给稳定 key，UI 文案由客户端按平台渲染
// （AGENTS.md §6：不在服务端写死客户端行为）。
const (
	// ActionCodeNone 无需额外动作。
	ActionCodeNone = ""
	// ActionCodeChallenge 需要人机/二次校验。
	ActionCodeChallenge = "risk.action.challenge"
	// ActionCodeBlocked 动作被拒绝。
	ActionCodeBlocked = "risk.action.blocked"
	// ActionCodeReview 已放行但进入复核。
	ActionCodeReview = "risk.action.review_pending"
	// ActionCodePunished 因生效处罚被拒/受限，客户端可展示处罚剩余时长。
	ActionCodePunished = "risk.action.punished"
	// ActionCodeUnavailable 风控依赖故障且按保守策略拒绝，建议稍后重试。
	ActionCodeUnavailable = "risk.action.temporarily_unavailable"
)

// Input 是一次裁决请求的脱敏视图。
type Input struct {
	RequestID  string // 幂等键，空时由 Engine 生成
	Mid        int64
	Action     int32
	DeviceHash string // sha256(设备号)，可为空
	IPHash     string // 调用方预哈希，可为空
	Platform   string
	AppVersion string
	Now        int64 // 裁决时间（Unix 秒），注入后时间边界可单测
}

// PunishmentView 是生效处罚的最小视图。
type PunishmentView struct {
	ID         int64
	Scope      int32 // 0 表示全域
	Decision   int32
	StartAt    int64
	EndAt      int64 // 0 表示永久
	ReasonCode string
}

// Permanent 判定处罚是否无期限。
func (p *PunishmentView) Permanent() bool { return p != nil && p.EndAt == 0 }

// RemainingSeconds 返回剩余秒数；永久或已到期为 0。
func (p *PunishmentView) RemainingSeconds(now int64) int64 {
	if p == nil || p.EndAt == 0 {
		return 0
	}
	if p.EndAt <= now {
		return 0
	}
	return p.EndAt - now
}

// Observation 是一条规则在裁决时刻的观测结果。
// Available=false 表示指标不可观测（Redis/画像缺失或指标未实现），
// 引擎会把它放进 skipped_rule_ids，而不是当作 0 命中。
type Observation struct {
	RuleID        int64
	Version       int32
	Name          string
	Metric        string
	Op            int32
	Threshold     int64
	WindowSeconds int64
	Decision      int32
	Priority      int32
	Value         int64
	Available     bool
}

// Facts 是引擎评估所需的全部外部事实，由 repository 一次性装载。
type Facts struct {
	// BlacklistHits/WhitelistHits 是可读的名单命中说明，如 "black:mid=123"。
	BlacklistHits []string
	WhitelistHits []string
	// Punishment 为当前动作生效的处罚，nil 表示无。
	Punishment *PunishmentView
	// Observations 为该动作启用的规则观测。
	Observations []Observation
	// CounterAvailable 表示 Redis 滑窗计数是否可读。
	CounterAvailable bool
	// LocalLimited 表示 Redis 不可用时进程内兜底限流是否已触发。
	LocalLimited bool
}

// Hit 是单条规则命中明细。
type Hit struct {
	RuleID        int64
	Version       int32
	Name          string
	Metric        string
	Op            int32
	Threshold     int64
	Observed      int64
	WindowSeconds int64
	Decision      int32
	Priority      int32
}

// Result 是可解释裁决。字段全部可 JSON 序列化，供 Redis 幂等回放。
type Result struct {
	RequestID           string
	Decision            int32
	Score               int32
	HitRuleIDs          []int64
	Hits                []Hit
	HitVersions         []int32
	Punishment          *PunishmentView
	ActionCode          string
	ChallengeTTLSeconds int64
	Basis               string
	SkippedRuleIDs      []int64
	Evaluated           bool
	Degraded            bool
}
