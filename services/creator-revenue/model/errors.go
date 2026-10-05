// 本文件是 model 包的手写扩展，不是 goctl 生成产物。

package model

import (
	"errors"
	"strings"
)

// 列宽上限（与 deploy/migrations/creator-revenue/000001_create_creator_revenue_tables.sql 逐字对齐）。
// logic 入口按这些常量拒掉超长入参，避免错误发生在 INSERT 里（离根因太远，还白烧一次事务）。
const (
	MaxRuleCodeBytes    = 64  // cr_revenue_rule.rule_code / cr_metric.rule_code
	MaxRuleNameBytes    = 64  // cr_revenue_rule.name
	MaxDescriptionBytes = 512 // cr_revenue_rule.description
	MaxCurrencyBytes    = 8   // 记账币种码
	MaxUnitBytes        = 16  // 计量单位：minute / coin / interaction
	MaxPeriodBytes      = 6   // YYYYMM
	// MaxSettlementNoBytes 对齐 cr_settlement.settlement_no VARCHAR(48)。
	// 单号格式 CRS<period>-<mid>-<rev> = 3+6+1+len(mid)+1+len(rev)，
	// 48 宽允许 mid 最多 19 位（int64 上限刚好），rev 留到 15 位；
	// 因此 logic 侧只需保证 mid 是合法 int64（normalizeMid 已拒负数/0）。
	MaxSettlementNoBytes = 48  // cr_settlement.settlement_no
	MaxOperatorBytes     = 64  // 各表 operator / created_by / updated_by / confirmed_by
	MaxReasonBytes       = 512 // 变更原因
	MaxRequestIDBytes    = 64  // 幂等键
	MaxSourceDetailBytes = 512 // 计算依据摘要（不含 PII）
	MaxRemarkBytes       = 512 // 参与关系备注
	MaxVoidReasonBytes   = 512 // 作废原因
	MaxConfirmBatch      = 200 // ConfirmSettlement 单次确认单号数上限
)

// creator-revenue 域哨兵错误。
// logic 层直接返回这些错误，gateway 负责映射为 HTTP 响应信封的 code。
//
// 统一原则（AGENTS.md §9）：任何「判定不通过」都必须是可见的错误或明确的
// 幂等结论，禁止折叠成空台账、零值响应冒充成功。
var (
	// ErrNotImplemented 尚未实现的方法占位错误。
	// 交付轮次未覆盖的方法一律回该哨兵，绝不回 `&rpc.XxxReply{}, nil` 的伪成功骨架。
	ErrNotImplemented = errors.New("creatorrevenue: not implemented")

	// ErrInvalidPeriod period 不是 YYYYMM（0<month<=12）或为空。
	ErrInvalidPeriod = errors.New("creatorrevenue: invalid period, expected YYYYMM")
	// ErrInvalidMid mid 非法（写接口要求真实作者 ID）。
	ErrInvalidMid = errors.New("creatorrevenue: invalid mid")
	// ErrInvalidAid aid 非法。
	ErrInvalidAid = errors.New("creatorrevenue: invalid aid")
	// ErrInvalidPageParam page/size 为负或游标越界。
	ErrInvalidPageParam = errors.New("creatorrevenue: invalid page/size")
	// ErrQueryScopeRequired 台账/结算列表必须带 period 或 mid 之一：
	// cr_metric 是亿级表，无界扫描会拖垮主库，也不能让运营拿到一份「随机样本」。
	ErrQueryScopeRequired = errors.New("creatorrevenue: period or mid is required to bound the ledger scan")

	// ErrRuleNotFound 规则不存在。
	ErrRuleNotFound = errors.New("creatorrevenue: revenue rule not found")
	// ErrRuleCodeRequired UpsertRevenueRule 未带 rule_code。
	ErrRuleCodeRequired = errors.New("creatorrevenue: rule_code is required")
	// ErrRuleCodeConflict 新建规则时 rule_code 已存在（且未带 rule_id 定位）。
	ErrRuleCodeConflict = errors.New("creatorrevenue: rule_code already exists")
	// ErrRuleTargetRequired 既没给 rule_id 也没给 rule_code，无法定位规则。
	ErrRuleTargetRequired = errors.New("creatorrevenue: rule_id or rule_code is required")
	// ErrInvalidSourceType 收益来源类型不在枚举内。
	ErrInvalidSourceType = errors.New("creatorrevenue: invalid source_type")
	// ErrInvalidRuleState 规则状态取值不在枚举内。
	ErrInvalidRuleState = errors.New("creatorrevenue: invalid rule state")
	// ErrRuleStateTransition 规则状态机非法迁移（只允许 DRAFT→ACTIVE、ACTIVE→ARCHIVED、DRAFT→ARCHIVED）。
	ErrRuleStateTransition = errors.New("creatorrevenue: illegal rule state transition")
	// ErrRuleNotDraft 只有 DRAFT 规则可被编辑（ACTIVE 单价直接决定应计金额，禁止就地改）。
	ErrRuleNotDraft = errors.New("creatorrevenue: only DRAFT rules can be edited")
	// ErrNegativeUnitPrice 单价为负：负单价会把「应付」算成「倒扣」，一律拒绝。
	ErrNegativeUnitPrice = errors.New("creatorrevenue: unit_price_per_1000_minor must not be negative")
	// ErrNegativeQuantity quantity 为负：计量事实不允许反向。
	ErrNegativeQuantity = errors.New("creatorrevenue: quantity must not be negative")
	// ErrInvalidRuleParams min_quantity / monthly_cap_minor 为负。
	ErrInvalidRuleParams = errors.New("creatorrevenue: min_quantity and monthly_cap_minor must not be negative")
	// ErrRuleUnitPriceTooLarge 单价超过配置护栏（防手滑写成天价）。
	ErrRuleUnitPriceTooLarge = errors.New("creatorrevenue: unit_price_per_1000_minor exceeds configured ceiling")
	// ErrRuleCapTooLarge 月度封顶超过配置护栏。
	ErrRuleCapTooLarge = errors.New("creatorrevenue: monthly_cap_minor exceeds configured ceiling")
	// ErrRuleCurrencyMismatch 台账币种与规则币种不一致（不允许跨币种混算）。
	ErrRuleCurrencyMismatch = errors.New("creatorrevenue: currency mismatch with rule")
	// ErrRuleNotActive 计量必须按 ACTIVE 规则折算，DRAFT/ARCHIVED 一律拒绝。
	ErrRuleNotActive = errors.New("creatorrevenue: revenue rule is not ACTIVE")
	// ErrRuleNotEffective 周期起点早于规则生效起点，本周期不适用该规则。
	ErrRuleNotEffective = errors.New("creatorrevenue: revenue rule not effective for this period")
	// ErrRuleSourceMismatch 上报的 rule_code 对应规则的 source_type 与请求不一致。
	ErrRuleSourceMismatch = errors.New("creatorrevenue: rule_code source_type mismatch")

	// ErrOperatorRequired 运营/系统身份缺失（网关按会话渲染，缺失即拒绝写入）。
	ErrOperatorRequired = errors.New("creatorrevenue: operator is required")
	// ErrReasonRequired 有后果的动作（改价、切状态、更正、作废、确认）必须留原因。
	ErrReasonRequired = errors.New("creatorrevenue: reason is required")
	// ErrRequestIDRequired 写接口缺少幂等键（AGENTS.md §5）。
	ErrRequestIDRequired = errors.New("creatorrevenue: request_id is required")
	// ErrRequestReplayed 同一 request_id 重复提交：返回首次结果而不是二次生效。
	ErrRequestReplayed = errors.New("creatorrevenue: request_id already applied")
	// ErrRequestIDConflict 同一 request_id 之后该规则又发生了新变更，本次无法安全重放。
	ErrRequestIDConflict = errors.New("creatorrevenue: request_id conflicts with a newer change")
	// ErrTextTooLong 入参超过列宽。
	ErrTextTooLong = errors.New("creatorrevenue: text exceeds column limit")
	// ErrBatchTooLarge 批量确认单号数超过上限。
	ErrBatchTooLarge = errors.New("creatorrevenue: batch too large")
	// ErrSettlementNosRequired 空列表拒绝：空批量是调用方装配错误，不是「确认成功 0 条」。
	ErrSettlementNosRequired = errors.New("creatorrevenue: settlement_nos is required")

	// ErrVersionConflict expected_version 与服务端当前版本不一致（乐观锁失败）。
	ErrVersionConflict = errors.New("creatorrevenue: expected_version mismatch, reload and retry")
	// ErrConcurrentUpdate 同一判定被并发修改，本次写入未生效，调用方可安全重试。
	ErrConcurrentUpdate = errors.New("creatorrevenue: concurrent update, retry this request")

	// ErrEnrollmentNotFound 参与关系不存在（从未参加过）。
	ErrEnrollmentNotFound = errors.New("creatorrevenue: enrollment not found")
	// ErrAgreedRuleVersionRequired 未确认规则版本不得参加计划。
	ErrAgreedRuleVersionRequired = errors.New("creatorrevenue: agreed_rule_version is required")
	// ErrEnrollmentStateTransition 参与状态机非法迁移。
	ErrEnrollmentStateTransition = errors.New("creatorrevenue: illegal enrollment state transition")
	// ErrNotEnrolled 作者未处于 ENROLLED 状态，不产生收益结算。
	ErrNotEnrolled = errors.New("creatorrevenue: creator is not enrolled in the revenue plan")
	// ErrEnrollmentSuspended 违规暂停：收益不结算（ENROLLED 之外的状态一律不出单）。
	ErrEnrollmentSuspended = errors.New("creatorrevenue: enrollment is suspended, no settlement")

	// ErrMetricNotFound 计量台账行不存在。
	ErrMetricNotFound = errors.New("creatorrevenue: revenue metric not found")
	// ErrSettlementConfirmed 该周期结算单已 CONFIRMED，拒绝更正台账。
	// 要改必须先作废重算（GenerateSettlement 的 force_void_confirmed 危险位）。
	ErrSettlementConfirmed = errors.New("creatorrevenue: settlement already confirmed, metric correction rejected")
	// ErrAmountOverflow quantity * unit_price 溢出 int64（异常量级，拒绝而不是回负数）。
	ErrAmountOverflow = errors.New("creatorrevenue: amount calculation overflow")

	// ErrSettlementNotFound 结算单不存在。
	ErrSettlementNotFound = errors.New("creatorrevenue: settlement not found")
	// ErrSettlementNotDraft 只有 DRAFT 结算单可确认/重算。
	ErrSettlementNotDraft = errors.New("creatorrevenue: settlement is not in DRAFT state")
	// ErrSettlementVoided 结算单已作废。
	ErrSettlementVoided = errors.New("creatorrevenue: settlement is voided")
	// ErrForceVoidReasonRequired 强制作废已确认单必须留原因。
	ErrForceVoidReasonRequired = errors.New("creatorrevenue: reason is required when force_void_confirmed is true")
	// ErrFuturePeriod 拒绝为尚未开始的周期出单/记账。
	ErrFuturePeriod = errors.New("creatorrevenue: period is in the future")
	// ErrNoMetricsToSettle 该周期该作者没有任何计量台账，不出空结算单。
	ErrNoMetricsToSettle = errors.New("creatorrevenue: no revenue metric ledger for this period, nothing to settle")
	// ErrForbidden 请求的归属与结算单不一致（GetSettlement 带 mid 校验归属）。
	ErrForbidden = errors.New("creatorrevenue: settlement does not belong to the given mid")

	// ErrDBNotConfigured 未配置 DataSource，写路径必须显式失败而不是回零值。
	ErrDBNotConfigured = errors.New("creatorrevenue: database is not configured")
	// ErrPayoutNotAvailable 出金能力不在本项目范围内（提现/打款/发票/对账）。
	// 本服务不开这类接口；若未来被误调用，必须回该错误而不是假成功。
	ErrPayoutNotAvailable = errors.New("creatorrevenue: payout is out of scope, no payment channel is configured")
)

// IsDuplicateErr 识别 MySQL 唯一索引冲突（错误号 1062 / Duplicate entry）。
//
// 本服务的幂等全部押在唯一键上（uniq_rule_code、uniq_metric_key、
// uniq_active_period_mid、uniq_request_id），因此「撞唯一键」必须能和
// 「其它 DB 故障」区分开：前者是并发重放的正常分支，后者必须上抛错误。
// 不 import 驱动专有错误码常量，与仓内其它服务保持同一字符串判定口径。
func IsDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}
