// Package model 定义 user-profile 服务的数据库实体、常量与查询接口。
// 本包只持有 user-profile 自有的表（AGENTS.md §5）：
// 用户基础资料、经验等级、节操、官方认证、实名认证、属性审核、监控名单、
// 变更日志与领域事件 Outbox。
// 移植自参考仓库 openbilibili-go-common/app/service/main/member/model。
package model

// ==================== 经验与等级（参考 model/member.go） ====================
const (
	// ExpMulti 经验值放大倍数：对外接口的经验单位为“分”，入库值 = 分 × 100。
	ExpMulti = 100
	// LevelMax 满级标记：等级上限 6，next_exp 为 -1 表示已满级。
	LevelMax = -1
	// 各等级经验阈值（单位：分）。
	level1 = 1
	level2 = 200
	level3 = 1500
	level4 = 4500
	level5 = 10800
	level6 = 28800
)

// BuildLevel 根据入库经验值（分×100）计算等级信息（参考 LevelInfo.BuildLevel）。
// sexp 为 true 时回填当前经验值（Exp 接口），false 只返回等级阈值（Level 接口）。
func BuildLevel(exp int64, sexp bool) (cur, min, nowExp, nextExp int32) {
	exp = exp / ExpMulti
	switch {
	case exp < level1:
		return 0, 0, now(sexp, exp), level1
	case exp < level2:
		return 1, level1, now(sexp, exp), level2
	case exp < level3:
		return 2, level2, now(sexp, exp), level3
	case exp < level4:
		return 3, level3, now(sexp, exp), level4
	case exp < level5:
		return 4, level4, now(sexp, exp), level5
	case exp < level6:
		return 5, level5, now(sexp, exp), level6
	default:
		return 6, level6, now(sexp, exp), LevelMax
	}
}

func now(sexp bool, exp int64) int32 {
	if sexp {
		return int32(exp)
	}
	return 0
}

// ==================== 基础资料（参考 model/base.go） ====================
const (
	// URLNoFace 未设置头像时的默认头像地址。
	URLNoFace = "http://static.hdslb.com/images/member/noface.gif"
	// DefaultRank 默认排名。
	DefaultRank = 5000
	// DefaultTime 未设置生日时的默认时间（Unix 秒，负值表示未设置）。
	DefaultTime = -28800
	// CacheKeyBase 基础资料缓存 key 模板，与参考仓库保持一致。
	CacheKeyBase = "bs_%d"
)

// SexStr 把数字性别转换为展示字符串（参考 BaseInfo.SexStr）。
func SexStr(sex int64) string {
	switch sex {
	case 1:
		return "男"
	case 2:
		return "女"
	default:
		return "保密"
	}
}

// ==================== 节操值（参考 model/moral.go） ====================
const (
	// DefaultMoral 节操值初始值（基准 70.00，单位为 1/100）。
	DefaultMoral = 7000
	// MaxMoral 节操值上限（100.00）。
	MaxMoral = 10000

	// 原因类型：1 弹幕、2 评论、3 TAG、4 电波、5 账号、6 管理系统。
	DMReasonType      = 1
	ReplyReasonType   = 2
	TagReasonType     = 3
	ElecReasonType    = 4
	AccountReasonType = 5
	SysReasonType     = 6

	// 日志状态：0 可撤销、1 已撤销、2 不可撤销。
	RevocableMoralStatus   = 0
	RevokedMoralStatus     = 1
	IrrevocableMoralStatus = 2

	// 来源类型：1 举报奖励、2 违规惩罚、3 撤销奖励、4 撤销惩罚、5 自动恢复、6 手动修改。
	ReportRewardType   = 1
	PunishmentType     = 2
	CancelRewardType   = 3
	CancelPunishType   = 4
	ManualRecoveryType = 5
	ManualChangeType   = 6
)

// MoralOrigin 节操变更来源定义。
type MoralOrigin struct {
	// Name 来源名称。
	Name string
	// NeedReason 是否需要填写原因。
	NeedReason bool
}

// MoralOrigins 来源类型表（参考 model.OriginTypes）。
var MoralOrigins = map[int64]*MoralOrigin{
	ReportRewardType:   {Name: "举报奖励", NeedReason: true},
	PunishmentType:     {Name: "违规惩罚", NeedReason: true},
	CancelRewardType:   {Name: "撤销奖励", NeedReason: true},
	CancelPunishType:   {Name: "撤销惩罚", NeedReason: true},
	ManualRecoveryType: {Name: "自动恢复", NeedReason: true},
	ManualChangeType:   {Name: "手动修改", NeedReason: false},
}

// ==================== 官方认证（参考 model/official.go） ====================
// 官方认证审核状态。
const (
	OfficialStateWait   = 0 // 待审核
	OfficialStatePass   = 1 // 通过
	OfficialStateNoPass = 2 // 不通过
	OfficialStateReWait = 3 // 重新提交
)

// 官方认证角色。
const (
	OfficialRoleUnauth   = 0 // 未认证
	OfficialRoleUp       = 1 // UP 主
	OfficialRoleIdentify = 2 // 身份认证
	OfficialRoleBusiness = 3 // 企业认证
	OfficialRoleGov      = 4 // 政府认证
	OfficialRoleMedia    = 5 // 媒体认证
	OfficialRoleOther    = 6 // 其他
)

// ==================== 实名认证（参考 model/realname.go） ====================
// RealnameStatus 实名状态。
const (
	RealnameStatusFalse = 0 // 未通过
	RealnameStatusTrue  = 1 // 已通过
)

// RealnameApplyStatus 实名申请流程状态。
const (
	RealnameApplyStatusPending = 0 // 审核中
	RealnameApplyStatusPass    = 1 // 通过
	RealnameApplyStatusBack    = 2 // 驳回
	RealnameApplyStatusNone    = 3 // 未申请
)

// 实名渠道。
const (
	RealnameChannelMain   = 0 // 主站
	RealnameChannelAlipay = 1 // 支付宝（暂不启用）
)

// 实名国家与证件类型。
const (
	RealnameCountryChina     = 0
	RealnameCardTypeIdentity = 0 // 身份证
)

// RealnameAdultType 成年状态。
const (
	RealnameAdultTypeFalse   = 0 // 未成年
	RealnameAdultTypeTrue    = 1 // 已成年
	RealnameAdultTypeUnknown = 2 // 未知（未绑定身份证）
)

// ==================== 属性审核（参考 model/property_review.go） ====================
// 审核状态。
const (
	ReviewStateWait     = 0  // 待审核
	ReviewStatePass     = 1  // 通过
	ReviewStateNoPass   = 2  // 驳回
	ReviewStateArchived = 3  // 已归档
	ReviewStateQueuing  = 10 // 自动审核中
)

// 审核属性。
const (
	ReviewProperty     = 0 // 无意义
	ReviewPropertyFace = 1 // 头像
	ReviewPropertySign = 2 // 签名
	ReviewPropertyName = 3 // 昵称
)

// ==================== 用户标志位（参考 model/user_flag.go） ====================
const (
	// NickUpdated 已首次修改昵称标志位。
	NickUpdated = uint(1)
)

// HasAttr 判断标志位是否已置位。
func HasAttr(flag uint, bit uint) bool {
	return flag&bit == bit
}

// SetAttr 置位。
func SetAttr(flag uint, bit uint) uint {
	return flag | bit
}

// ==================== 变更日志（移植自 member_log.go/hbase.go 的替代方案） ====================
// 日志类型。
const (
	// LogTypeExp 经验变更日志（参考业务号 11）。
	LogTypeExp = 11
	// LogTypeMoral 节操变更日志（参考业务号 12）。
	LogTypeMoral = 12
)

// 日志状态。
const (
	// LogStatusActive 有效。
	LogStatusActive = 0
	// LogStatusRevoked 已撤销（UndoMoral 后置位）。
	LogStatusRevoked = 1
)

// ==================== 领域事件（Outbox） ====================
// 事件类型（小写点分隔，见 common/eventenvelope）。
const (
	// EventProfileUpdated 资料更新事件：消费者为 account 服务的
	// POST /account/cache/clear（参考仓库 databus 的 MemberService-AccountNotify 主题）。
	EventProfileUpdated = "user.profile_updated"
	// EventMoralNotice 节操阈值通知事件：消费者为 notification 服务（待接入）。
	EventMoralNotice = "user.moral_notice"
)

// Outbox 状态。
const (
	OutboxStatusPending   = 0 // 待发布
	OutboxStatusPublished = 1 // 已发布
	OutboxStatusFailed    = 2 // 失败（超过最大重试次数，人工处理）
)
