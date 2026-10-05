package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳（本服务所有时间列均为 BIGINT 秒）。
func nowUnix() int64 { return time.Now().Unix() }

// 通用状态约定：0 禁用/无效，1 启用/生效。
const (
	// StateDisabled 记录停用。
	StateDisabled int32 = 0
	// StateEnabled 记录生效。
	StateEnabled int32 = 1
)

// 受保护动作（与 rpc/riskcontrol.proto 的 GuardedAction 编号一一对应，禁止漂移）。
const (
	// ActionAll 仅用于规则：0 表示对所有动作生效。
	ActionAll int32 = 0
	// ActionSubmitVideo 投稿。
	ActionSubmitVideo int32 = 1
	// ActionComment 评论。
	ActionComment int32 = 2
	// ActionDanmaku 弹幕。
	ActionDanmaku int32 = 3
	// ActionFollow 关注。
	ActionFollow int32 = 4
	// ActionLogin 登录。
	ActionLogin int32 = 5
	// ActionRename 改名。
	ActionRename int32 = 6
	// ActionLiveStart 直播开播。
	ActionLiveStart int32 = 7
)

// 裁决（与 rpc Decision 对应）。
const (
	// DecisionAllow 放行。
	DecisionAllow int32 = 1
	// DecisionChallenge 要求人机/二次校验。
	DecisionChallenge int32 = 2
	// DecisionBlock 拒绝动作。
	DecisionBlock int32 = 3
	// DecisionReview 放行并进入复核。
	DecisionReview int32 = 4
)

// 指标（与 rpc Metric 对应）。model 层按名称保存，便于运营后台展示与规则解释。
const (
	// MetricActionCount mid+action 滑窗动作数。
	MetricActionCount = "action_count"
	// MetricDeviceActionCount device+action 滑窗动作数。
	MetricDeviceActionCount = "device_action_count"
	// MetricIpActionCount ip_hash+action 滑窗动作数。
	MetricIpActionCount = "ip_action_count"
	// MetricDeviceRiskScore 设备画像风险分。
	MetricDeviceRiskScore = "device_risk_score"
	// MetricDeviceMidCount 设备关联账号数。
	MetricDeviceMidCount = "device_mid_count"
)

// SupportedMetrics 返回本服务已实现的指标名集合。
// 新增指标必须先在这里注册并由 repository 实现取数，再允许运营写入规则，
// 否则规则会以「不可观测」被跳过（见 repository.observe）。
func SupportedMetrics() map[string]struct{} {
	return map[string]struct{}{
		MetricActionCount:       {},
		MetricDeviceActionCount: {},
		MetricIpActionCount:     {},
		MetricDeviceRiskScore:   {},
		MetricDeviceMidCount:    {},
	}
}

// 比较操作符（与 rpc CompareOp 对应）。
const (
	OpGT  int32 = 1
	OpGTE int32 = 2
	OpLT  int32 = 3
	OpLTE int32 = 4
	OpEQ  int32 = 5
)

// ValidOp 判定比较符是否在枚举范围内。
func ValidOp(op int32) bool { return op >= OpGT && op <= OpEQ }

// ValidMetric 判定指标名是否已实现取数。
func ValidMetric(name string) bool {
	_, ok := SupportedMetrics()[name]
	return ok
}

// Evaluate 按 op 比较观测值与阈值。op 非法时返回 false（宁可不命中也不误判）。
func Evaluate(op int32, observed, threshold int64) bool {
	switch op {
	case OpGT:
		return observed > threshold
	case OpGTE:
		return observed >= threshold
	case OpLT:
		return observed < threshold
	case OpLTE:
		return observed <= threshold
	case OpEQ:
		return observed == threshold
	default:
		return false
	}
}

// Severity 返回裁决的强度序，用于多条命中取最严重裁决。
// 顺序：ALLOW < CHALLENGE < REVIEW < BLOCK。
func Severity(decision int32) int32 {
	switch decision {
	case DecisionAllow:
		return 0
	case DecisionChallenge:
		return 1
	case DecisionReview:
		return 2
	case DecisionBlock:
		return 3
	default:
		return 0
	}
}

// 处罚状态（与 rpc PunishmentState 对应）。
const (
	// PunishmentStateActive 生效中。
	PunishmentStateActive int32 = 1
	// PunishmentStateLifted 已解除（终态）。
	PunishmentStateLifted int32 = 2
	// PunishmentStateExpired 已过期（终态）。
	PunishmentStateExpired int32 = 3
)

// ValidPunishmentState 判定处罚状态是否在枚举范围内。
func ValidPunishmentState(state int32) bool {
	return state >= PunishmentStateActive && state <= PunishmentStateExpired
}

// 名单类型（与 rpc ListType / TargetType 对应）。
const (
	// ListTypeBlack 黑名单：命中直接 BLOCK。
	ListTypeBlack int32 = 1
	// ListTypeWhite 白名单：命中跳过规则评估。
	ListTypeWhite int32 = 2
)

// ValidListType 判定名单类型是否合法。
func ValidListType(t int32) bool { return t == ListTypeBlack || t == ListTypeWhite }

const (
	// TargetTypeMid 名单目标为账号。
	TargetTypeMid int32 = 1
	// TargetTypeDevice 名单目标为设备受控 ID（hash）。
	TargetTypeDevice int32 = 2
	// TargetTypeIpHash 名单目标为调用方预哈希 IP。
	TargetTypeIpHash int32 = 3
)

// ValidTargetType 判定名单目标类型是否合法。
func ValidTargetType(t int32) bool { return t >= TargetTypeMid && t <= TargetTypeIpHash }

// ValidAction 判定受保护动作是否在枚举范围内（不含 ActionAll：0 只允许出现在规则里）。
func ValidAction(action int32) bool { return action >= ActionSubmitVideo && action <= ActionLiveStart }

// ValidRuleAction 判定规则适用动作是否合法（允许 0 表示全动作）。
func ValidRuleAction(action int32) bool { return action == ActionAll || ValidAction(action) }
