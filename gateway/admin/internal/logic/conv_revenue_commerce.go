// 手写文件（不属于 goctl 生成产物）：/admin/creator-revenue 十一条路由共用的身份渲染、
// 传输层门槛与 RPC→后台投影。
//
// 放在这里而不是各 logic 里重复一遍（AGENTS.md §4/§5）：
//   - operator 只能来自会话身份，且要满足 creator-revenue 侧 operator 列宽；
//   - 分成规则、参与关系、计量台账与结算单**只属于 creator-revenue**（§5）：单价护栏上限、
//     门槛与封顶组合、rule_code 唯一性、规则状态机迁移（checkRuleTransition）、
//     周期格式（ValidatePeriod）、未来周期、应计金额折算与封顶扣减、幂等指纹一致性，
//     全部由服务判定，网关一个都不复算，也不代为放宽；
//   - **出金不在本期范围**（§1）：本域只做到「算出该给多少并落成可审计的结算单」。
//     结算单的 payout_state 由服务给出（当前恒为 NOT_PAYABLE），网关逐位转达、
//     不推断、不改写、不补任何「已打款/可提现」位；确认结算单只是「这份账认了」，
//     不等于钱已付出。契约里也没有出金方法，网关不会去凑；
//   - 台账金额是**应计金额**（分），不是已支付金额：投影里 amount_minor 与
//     capped_amount_minor、cap_applied_minor 三位并存就是为了让「折算了多少、封顶扣掉多少」
//     在后台可对齐，网关不合并、不取其中一位冒充合计；
//   - 计量原始事实（有效观看时长、收到的投币、互动）归 spm / coin（§5/§7），
//     本域不写计量台账（RecordRevenueMetric 不开后台路由），也不接受任何广告参数；
//   - 分页三元组照抄 reply，不复算、不裁剪。
//
// 与 membership/payment/order/coin 同口径：读侧没配下游时一律报错，不回 found=false
// 也不回空台账——那会让后台把「creator-revenue 没接」读成「这个作者一分别人都没赚过」，
// 然后用 /settlement/generate 或 /rule/upsert 去手工补数（§1 禁止的假成功）。

package logic

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// revenueOperatorPrefix 与 membership/payment/order/coin 同口径：operator 前缀说明
// 「哪个入口提交的」（服务名，不是实例地址），后面接会话 admin_id，让
// cr_rule.created_by / cr_rule_change.operator / cr_enrollment.operator /
// cr_settlement.confirmed_by 能回溯到人。
const revenueOperatorPrefix = "gateway/admin:"

// revenueMaxOperatorLength 是 cr_* 各表 operator / created_by / updated_by / confirmed_by
// 的列宽（VARCHAR(64)；服务侧收口在 services/creator-revenue/model/errors.go 的
// MaxOperatorBytes，同样 64，且 helpers.go 的 requireOperator 是**拒绝**而不是截断，
// 但 trunc() 在 remark 这类派生位上仍会切）。
// 前缀 14 + int64 最多 19 位，正常永远碰不到；留着是因为真超限时服务报的是
// "creatorrevenue: text exceeds column limit: operator ..."，从后台错误里看不出
// 这串是网关拼出来的（§5 审计证据要能读）。
const revenueMaxOperatorLength = 64

// errRevenueServiceNotConfigured 未配 CreatorRevenueRPC 时本域 11 条路由一律返回它。
// 不退化成空列表或 found=false：分成台账的「空」在后台就是一个结论（这一期没有应计），
// 用它冒充「下游没接」是 §1 禁止的假成功。
var errRevenueServiceNotConfigured = errors.New("gateway/admin: creator-revenue service client not configured")

// errRevenueRequestMissing 请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errRevenueRequestMissing = errors.New("gateway/admin: request body required")

// errRevenueSessionRequired 受 AdminPermission 保护的写路由拿不到会话身份。
// 此时说明这条路由没被中间件保护（权限表/挂载漂移），一律 fail-closed：
// 这五条写口会改单价、改生效规则、停作者收益资格、出单甚至作废已确认单、冻结金额，
// 没有主体就一个字都不写。
var errRevenueSessionRequired = errors.New("gateway/admin: admin session identity required")

// revenueOperator 渲染下传给 creator-revenue 的 operator。
//
// claimed 是请求体里的 operator 位（.api 注释「必须 > 0」）：它**只**用于两件事——
// 按契约确认表单确实填了审计主体，以及在与会话不一致时留一条越权线索日志。
// 真正进台账的永远是会话 admin_id 渲染出的 gateway/admin:<id>，否则请求体就等于
// 能自称是任意后台账号（§5：分成台账归 creator-revenue 持有，网关不能投喂未证实主体）。
//
// 特别注意：服务侧 SetEnrollmentState 明确拒绝 operator=="user"（暂停/恢复是运营处置，
// 不接受自助身份）。网关永远给得出 gateway/admin:<id>，这条拒绝在本域实际落在
// 「没有会话主体」上——正因如此不能放开。
//
// 日志不打 reason 正文与 idempotency_key：前者是人读文案、后者可被重放（§7 脱敏口径）。
func revenueOperator(ctx context.Context, route string, claimed int64) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errRevenueSessionRequired
	}
	if err := requireOperatorID(id.AdminID); err != nil {
		return "", err
	}
	if err := requireOperator("operator", claimed); err != nil {
		return "", err
	}
	if claimed != id.AdminID {
		logx.WithContext(ctx).Errorf("gateway/admin/%s: operator mismatch session=%d claimed=%d",
			route, id.AdminID, claimed)
	}
	operator := fmt.Sprintf("%s%d", revenueOperatorPrefix, id.AdminID)
	if utf8.RuneCountInString(operator) > revenueMaxOperatorLength {
		return "", fmt.Errorf("gateway/admin: rendered operator exceeds %d characters", revenueMaxOperatorLength)
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d operator=%s", route, id.AdminID, operator)
	return operator, nil
}

// --- 传输层门槛 ---

// revenueNonNeg 只挡负数：0 在本域普遍是合法哨兵（page/size=0 用服务默认页、
// mid=0 跨作者查台账或「不校验归属」地看某一单、aid=0 不挂具体内容、
// state/source_type=0 不按该位过滤、rule_id=0 走 rule_code 定位、version=0 读当前版本、
// monthly_cap_minor=0 不限封顶、min_quantity=0 不设门槛、effective_from=0 不推迟生效、
// expected_version=0 交给服务按「这条口要不要 CAS」判定），
// 负数没有任何对应语义，透传只会换来一次无意义往返。
// 上限（MaxPageSize、单价/封顶护栏、列宽）与「这个枚举值存不存在」一律由服务拒绝，网关不复算。
func revenueNonNeg(field string, v int64) error {
	if v < 0 {
		return fmt.Errorf("gateway/admin: %s must be >= 0, got %d", field, v)
	}
	return nil
}

// revenuePositive 给「这一位没有 0 语义」的必填位设下界：
// 参与关系以 mid 为主键（服务 normalizeMid 对 mid<=0 回 ErrInvalidMid，分成没有游客作者号）、
// 规则状态切换只认 rule_id（服务对 rule_id<=0 直接回 ErrRuleTargetRequired）、
// 表单的 operator 审计主体由 requireOperator 管。
//
// 注意它**只**用在服务同样硬性要求的位上。检索口的 mid=0 是「跨作者查台账」的合法哨兵
// （有界性由服务用 ErrQueryScopeRequired 判），所以那里用 revenueNonNeg 而不是这个函数。
func revenuePositive(field string, v int64) error {
	if v <= 0 {
		return fmt.Errorf("gateway/admin: %s required (must be > 0, got %d)", field, v)
	}
	return nil
}

// revenueRuleSourceType 只管「新建规则时这一位到底填没填」。
//
// 0 = REVENUE_SOURCE_TYPE_UNSPECIFIED 在本路由没有含义：一条规则必须说明它折算哪一路
// 收益（会员观看/投币/互动/活动激励），留空落库就是造出一条永远不会被计量命中的规则，
// 而后台还以为它生效了。.api 也把 source_type 标成必填（无 optional）。
// 1..4 之外的编号（例如 5）**不在这里拒**——枚举里有哪些值是 creator-revenue 的结论
// （validSourceType / model.SourceTypeValid），网关写死就等于第二处规则源。
func revenueRuleSourceType(v int32) error {
	if v == 0 {
		return errors.New("gateway/admin: source_type required (a rule must name the revenue source it prices)")
	}
	return revenueNonNeg("source_type", int64(v))
}

// revenueRuleTargetState 只挡「没选目标状态」：0/UNSPECIFIED 不是一次状态迁移请求。
// 1 DRAFT / 2 ACTIVE / 3 ARCHIVED 都可能是合法目标，具体这一刀合不合法
// （DRAFT→ACTIVE、ACTIVE→ARCHIVED、DRAFT→ARCHIVED，且 ACTIVE 不可原地改价）由服务的
// checkRuleTransition 结合**当前行**判定 —— 网关不知道前态，预先放行/预先拒绝都会漂移。
// 越界编号由服务回 ErrInvalidRuleState。
func revenueRuleTargetState(v int32) error {
	if v == 0 {
		return errors.New("gateway/admin: target_state required (DRAFT=1 / ACTIVE=2 / ARCHIVED=3)")
	}
	return revenueNonNeg("target_state", int64(v))
}

// revenueEnrollmentTargetState 是本路由的身份声明，不是复算服务判定：
// admin.api 与 creatorrevenue.proto 在这一点上口径完全一致 —— SetEnrollmentState 只处理
// ENROLLED↔SUSPENDED。因此这里用**白名单**：
//   - LEFT=2 是合法枚举值，但「退出计划」是创作者本人的动作（proto: LeavePlan，归 gateway/app），
//     从后台塞进来等于代签退出，争议时这份记录不能当证据；
//   - UNSPECIFIED 与未知编号没有对应动作。
//
// 刻意不把非法值改写或降级成 SUSPENDED（那就是伪造处置方向）；「当前态不匹配」
// （对 LEFT 作者做暂停）由服务回 ErrEnrollmentStateTransition，逐字上抛。
func revenueEnrollmentTargetState(v int32) error {
	switch creatorrevenuerpc.EnrollmentState(v) {
	case creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_ENROLLED,
		creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_SUSPENDED:
		return nil
	default:
		return fmt.Errorf("gateway/admin: target_state %d is not accepted on this route (only ENROLLED=1 / SUSPENDED=3; leaving the plan is the creator's own action)", v)
	}
}

// --- rpc → 后台 types ---

// revenueRuleToAPI 投影分成规则行。
//
// 三位「不该被美化」的：state 是规则当前档位（DRAFT 对未来没有任何影响，ACTIVE 才参与折算，
// 网关不把 DRAFT 显示成生效）、version 是 CAS 位点（下一轮改价/生效要带回去，裁掉等于废掉
// 乐观锁）、effective_from 是追溯边界（晚于它的周期才用本规则，历史单不受影响）。
// unit_price_per_1000_minor 按「每 1000 单位」计，网关不做二次折算、不换算成单价。
func revenueRuleToAPI(r *creatorrevenuerpc.RevenueRuleInfo) types.RevenueRuleItem {
	if r == nil {
		return types.RevenueRuleItem{}
	}
	return types.RevenueRuleItem{
		RuleId:                r.GetRuleId(),
		RuleCode:              r.GetRuleCode(),
		SourceType:            int32(r.GetSourceType()),
		Name:                  r.GetName(),
		Description:           r.GetDescription(),
		UnitPricePer1000Minor: r.GetUnitPricePer_1000Minor(),
		Currency:              r.GetCurrency(),
		Unit:                  r.GetUnit(),
		MinQuantity:           r.GetMinQuantity(),
		MonthlyCapMinor:       r.GetMonthlyCapMinor(),
		State:                 int32(r.GetState()),
		EffectiveFrom:         r.GetEffectiveFrom(),
		Version:               r.GetVersion(),
		Ctime:                 r.GetCtime(),
		Mtime:                 r.GetMtime(),
		CreatedBy:             r.GetCreatedBy(),
		UpdatedBy:             r.GetUpdatedBy(),
	}
}

func revenueRulesToAPI(list []*creatorrevenuerpc.RevenueRuleInfo) []types.RevenueRuleItem {
	out := make([]types.RevenueRuleItem, 0, len(list))
	for _, r := range list {
		out = append(out, revenueRuleToAPI(r))
	}
	return out
}

// revenueEnrollmentToAPI 投影参与关系行。operator/remark 是「谁在什么时候把这个人怎么了」
// 的证据位（自助为 "user"，运营处置为 gateway/admin:<id>），网关不覆盖也不折叠；
// agreed_rule_version 是本人确认过的规则版本，争议复核全靠它，不能丢也不能补。
func revenueEnrollmentToAPI(e *creatorrevenuerpc.EnrollmentInfo) types.RevenueEnrollmentItem {
	if e == nil {
		return types.RevenueEnrollmentItem{}
	}
	return types.RevenueEnrollmentItem{
		Mid:               e.GetMid(),
		State:             int32(e.GetState()),
		AgreedRuleVersion: e.GetAgreedRuleVersion(),
		EnrolledAt:        e.GetEnrolledAt(),
		LeftAt:            e.GetLeftAt(),
		UpdatedAt:         e.GetUpdatedAt(),
		Operator:          e.GetOperator(),
		Remark:            e.GetRemark(),
	}
}

func revenueEnrollmentsToAPI(list []*creatorrevenuerpc.EnrollmentInfo) []types.RevenueEnrollmentItem {
	out := make([]types.RevenueEnrollmentItem, 0, len(list))
	for _, e := range list {
		out = append(out, revenueEnrollmentToAPI(e))
	}
	return out
}

// revenueMetricToAPI 投影计量台账行。
//
// amount_minor（封顶前）与 capped_amount_minor（门槛/封顶后的实际应计）是两位不是一位：
// 合并成一位就看不出「这一期被封顶砍了多少」。rule_version 是计算时锁定的规则版本
// （重算口径可追溯），source_detail 是服务已经脱敏过的摘要，网关不再改写也不追加内容。
func revenueMetricToAPI(m *creatorrevenuerpc.RevenueMetricInfo) types.RevenueMetricItem {
	if m == nil {
		return types.RevenueMetricItem{}
	}
	return types.RevenueMetricItem{
		MetricId:          m.GetMetricId(),
		Period:            m.GetPeriod(),
		Mid:               m.GetMid(),
		Aid:               m.GetAid(),
		SourceType:        int32(m.GetSourceType()),
		RuleCode:          m.GetRuleCode(),
		RuleVersion:       m.GetRuleVersion(),
		Quantity:          m.GetQuantity(),
		Unit:              m.GetUnit(),
		AmountMinor:       m.GetAmountMinor(),
		CappedAmountMinor: m.GetCappedAmountMinor(),
		SourceDetail:      m.GetSourceDetail(),
		Ctime:             m.GetCtime(),
		Mtime:             m.GetMtime(),
	}
}

func revenueMetricsToAPI(list []*creatorrevenuerpc.RevenueMetricInfo) []types.RevenueMetricItem {
	out := make([]types.RevenueMetricItem, 0, len(list))
	for _, m := range list {
		out = append(out, revenueMetricToAPI(m))
	}
	return out
}

// revenueSettlementToAPI 投影结算单行。
//
// payout_state 是本期语义的关键一位，必须**逐字转达服务给的值**：服务当前恒回
// PAYOUT_STATE_NOT_PAYABLE(1)，网关既不写死成 1（那是在伪造「服务说了」），
// 也不因为「后台点了确认」就翻成任何已出账含义 —— confirm 只是「这份账认了」。
// amount_minor 是应计合计而不是已支付；cap_applied_minor 是被封顶扣掉的额度（透明化位）；
// void_reason 让被强制作废的旧单看得见原因；confirmed_by 是认账的人。
func revenueSettlementToAPI(s *creatorrevenuerpc.SettlementInfo) types.RevenueSettlementItem {
	if s == nil {
		return types.RevenueSettlementItem{}
	}
	return types.RevenueSettlementItem{
		SettlementNo:    s.GetSettlementNo(),
		Period:          s.GetPeriod(),
		Mid:             s.GetMid(),
		AmountMinor:     s.GetAmountMinor(),
		CapAppliedMinor: s.GetCapAppliedMinor(),
		Currency:        s.GetCurrency(),
		MetricCount:     s.GetMetricCount(),
		State:           int32(s.GetState()),
		PayoutState:     int32(s.GetPayoutState()),
		ConfirmedAt:     s.GetConfirmedAt(),
		ConfirmedBy:     s.GetConfirmedBy(),
		VoidReason:      s.GetVoidReason(),
		Ctime:           s.GetCtime(),
		Mtime:           s.GetMtime(),
	}
}

func revenueSettlementsToAPI(list []*creatorrevenuerpc.SettlementInfo) []types.RevenueSettlementItem {
	out := make([]types.RevenueSettlementItem, 0, len(list))
	for _, s := range list {
		out = append(out, revenueSettlementToAPI(s))
	}
	return out
}

// revenueSettlementItemToAPI 投影结算单分项（按来源拆开）。
// 分项的 amount_minor 之和与合计之间的差额由 cap_applied_minor 解释，网关不去配平、
// 不补「其它」行，也不因为对不上就少回几条。
func revenueSettlementItemToAPI(i *creatorrevenuerpc.SettlementItem) types.RevenueSettlementDetailItem {
	if i == nil {
		return types.RevenueSettlementDetailItem{}
	}
	return types.RevenueSettlementDetailItem{
		SourceType:  int32(i.GetSourceType()),
		RuleCode:    i.GetRuleCode(),
		Quantity:    i.GetQuantity(),
		AmountMinor: i.GetAmountMinor(),
	}
}

func revenueSettlementItemsToAPI(list []*creatorrevenuerpc.SettlementItem) []types.RevenueSettlementDetailItem {
	out := make([]types.RevenueSettlementDetailItem, 0, len(list))
	for _, i := range list {
		out = append(out, revenueSettlementItemToAPI(i))
	}
	return out
}
