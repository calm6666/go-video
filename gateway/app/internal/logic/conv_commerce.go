// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：商业化五域（membership / coin /
// payment / trade-order / creator-revenue）的 RPC → 客户端投影，以及这些路由共用的入参闸门。
//
// 边界（AGENTS.md §1 商业化范围修订条、§5、§6，gateway/app/api/app.api 商业化段落）：
//   - 网关不判权益、不重算金额、不推进状态机；这里只有「字段搬运 + 形态校验」。
//   - 金额一律取下游返回值投影，网关不做乘加、折扣或分↔元换算（显示口径归端上）。
//   - 幂等键（request_id）只在闸门里判空，判空后**原样透传**：改一个字符等于换了幂等键，
//     会造成重复扣款 / 重复发放，因此这里既不 TrimSpace 后回填，也不代客户端造 UUID。
//   - found / duplicated / accepted / skipped 这类结论位由 logic 如实投影成 Code:0 信封，
//     不在这里折成 HTTP 错误。
//
// 空列表统一投影成 []T{}（非 nil），否则 JSON 出 null，端上要额外判空。

package logic

import (
	"errors"
	"strings"

	"go-video/gateway/app/internal/types"
	coinrpc "go-video/services/coin/rpc"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"
	membershiprpc "go-video/services/membership/rpc"
	paymentrpc "go-video/services/payment/rpc"
	tradeorderrpc "go-video/services/trade-order/rpc"
)

// commerceSelfOperator 终端自助写操作的操作者标识。
// 契约（membership.proto / payment.proto / tradeorder.proto / creatorrevenue.proto）里
// operator 一律「由网关按会话渲染，不接受客户端自报」，且明确「用户自助为 user」；
// gateway/app 当前没有 jwt 中间件（登录态校验是网关侧已知缺口），因此这里只能渲染成
// 固定的自助身份 "user"，绝不取客户端传来的任意字符串当操作者。
const commerceSelfOperator = "user"

// ---------- 入参闸门（fail-closed，只做形态校验） ----------

// requireMid 终端商业路由的统一主体闸门：没有正数 mid 就无从判定「谁的」订单/余额。
func requireMid(mid int64) error {
	if mid <= 0 {
		return errors.New("mid is required")
	}
	return nil
}

// requireText 必填字符串闸门（request_id / order_no / reason 等）。
// 只 TrimSpace 判空，返回的是**原值**；调用方透传时必须用原值而不是修剪后的值。
func requireText(field, v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New(field + " is required")
	}
	return nil
}

// requirePositive 契约标注「必填」的数值位闸门（如 agreed_rule_version）。
// 只判「有没有给一个正数」这个形态，不判断它是否指向真实记录——那是服务侧的判定。
func requirePositive(field string, v int64) error {
	if v <= 0 {
		return errors.New(field + " is required")
	}
	return nil
}

// requirePlanTarget mbPlan 的套餐定位位：plan_id 与 plan_code 至少给一个，
// 否则下游只能拿到全零请求（等于「随便给我一个套餐」）。
func requirePlanTarget(planID int64, planCode string) error {
	if planID <= 0 && strings.TrimSpace(planCode) == "" {
		return errors.New("plan_id or plan_code is required")
	}
	return nil
}

// requireCodes mbEntitlements 的权益码闸门：空列表直接在网关拒绝，
// 不拿去问下游——空集合的判定结果必然是空 decisions，端上会把「没传码」读成「都没权益」。
func requireCodes(codes []string) error {
	if len(codes) == 0 {
		return errors.New("codes is required")
	}
	return nil
}

// requireAids coinTargetsSummary 的内容闸门：非空且逐个为正数 aid。
// 0 或负数 aid 会让下游按「不存在的稿件」聚合出全 0 汇总，伪装成真实结论。
func requireAids(aids []int64) error {
	if len(aids) == 0 {
		return errors.New("aids is required")
	}
	for _, aid := range aids {
		if aid <= 0 {
			return errors.New("aids contains invalid aid")
		}
	}
	return nil
}

// ---------- membership ----------

func mbPlanToAPI(p *membershiprpc.PlanInfo) types.MbPlan {
	if p == nil {
		return types.MbPlan{}
	}
	platforms := make([]int32, 0, len(p.GetPlatforms()))
	for _, pf := range p.GetPlatforms() {
		platforms = append(platforms, int32(pf))
	}
	return types.MbPlan{
		PlanId:             p.GetPlanId(),
		PlanCode:           p.GetPlanCode(),
		Name:               p.GetName(),
		Description:        p.GetDescription(),
		VipType:            int32(p.GetVipType()),
		DurationDays:       p.GetDurationDays(),
		UnitCount:          p.GetUnitCount(),
		PriceMinor:         p.GetPriceMinor(),
		PromPriceMinor:     p.GetPromPriceMinor(),
		Currency:           p.GetCurrency(),
		Platforms:          platforms,
		AutoRenewSupported: p.GetAutoRenewSupported(),
		State:              int32(p.GetState()),
	}
}

func mbPlansToAPI(list []*membershiprpc.PlanInfo) []types.MbPlan {
	out := make([]types.MbPlan, 0, len(list))
	for _, p := range list {
		out = append(out, mbPlanToAPI(p))
	}
	return out
}

// mbMembershipToAPI 会员身份投影。expire_at 原样透出，网关不替端上算「还剩几天」，
// 端上配合同一次响应里的 server_now 自行判过期。
func mbMembershipToAPI(m *membershiprpc.MembershipInfo) types.MbMembership {
	if m == nil {
		return types.MbMembership{}
	}
	return types.MbMembership{
		Mid:               m.GetMid(),
		VipType:           int32(m.GetVipType()),
		StartAt:           m.GetStartAt(),
		ExpireAt:          m.GetExpireAt(),
		AutoRenew:         m.GetAutoRenew(),
		AutoRenewChannel:  m.GetAutoRenewChannel(),
		AutoRenewSignedAt: m.GetAutoRenewSignedAt(),
		Source:            int32(m.GetSource()),
		PaidMonthCount:    m.GetPaidMonthCount(),
	}
}

// mbEntitlementBriefsToAPI 权益码展示位投影：只给 code/name/min_vip_type，
// 判定不在此（唯一出口是 CheckEntitlement(s)）。
func mbEntitlementBriefsToAPI(list []*membershiprpc.EntitlementInfo) []types.MbEntitlementBrief {
	out := make([]types.MbEntitlementBrief, 0, len(list))
	for _, e := range list {
		out = append(out, types.MbEntitlementBrief{
			Code:       e.GetCode(),
			Name:       e.GetName(),
			MinVipType: int32(e.GetMinVipType()),
		})
	}
	return out
}

func mbEntitlementDecisionsToAPI(list []*membershiprpc.EntitlementDecision) []types.MbEntitlementDecision {
	out := make([]types.MbEntitlementDecision, 0, len(list))
	for _, d := range list {
		out = append(out, types.MbEntitlementDecision{
			Code:    d.GetCode(),
			Granted: d.GetGranted(),
			Reason:  int32(d.GetReason()),
		})
	}
	return out
}

// mbGrantsToAPI 用户侧开通记录投影。刻意不投影 operator / payment_no / request_id：
// 那是运营与对账口径（operator 里可能是工号），对用户没有信息量（见 app.api MbGrant 注释）。
func mbGrantsToAPI(list []*membershiprpc.GrantInfo) []types.MbGrant {
	out := make([]types.MbGrant, 0, len(list))
	for _, g := range list {
		out = append(out, types.MbGrant{
			GrantId:        g.GetGrantId(),
			VipType:        int32(g.GetVipType()),
			Action:         g.GetAction(),
			DeltaDays:      g.GetDeltaDays(),
			PlanId:         g.GetPlanId(),
			Source:         int32(g.GetSource()),
			BizOrderNo:     g.GetBizOrderNo(),
			BeforeExpireAt: g.GetBeforeExpireAt(),
			AfterExpireAt:  g.GetAfterExpireAt(),
			Reason:         g.GetReason(),
			Ctime:          g.GetCtime(),
		})
	}
	return out
}

// ---------- coin ----------

// coinAccountToAPI 硬币账户投影：today_limit / per_target_limit / cancel_window_seconds
// 是服务侧配置的逐次快照，端上不要缓存（TTL 0）。
func coinAccountToAPI(a *coinrpc.CoinAccountInfo) types.CoinAccount {
	if a == nil {
		return types.CoinAccount{}
	}
	return types.CoinAccount{
		Mid:                 a.GetMid(),
		Balance:             a.GetBalance(),
		TotalTossed:         a.GetTotalTossed(),
		TodayTossed:         a.GetTodayTossed(),
		TodayLimit:          a.GetTodayLimit(),
		PerTargetLimit:      a.GetPerTargetLimit(),
		CancelWindowSeconds: a.GetCancelWindowSeconds(),
	}
}

func coinTossToAPI(t *coinrpc.TossInfo) types.CoinTossInfo {
	if t == nil {
		return types.CoinTossInfo{}
	}
	return types.CoinTossInfo{
		TossId:        t.GetTossId(),
		TargetAid:     t.GetTargetAid(),
		Count:         t.GetCount(),
		State:         int32(t.GetState()),
		FirstTossedAt: t.GetFirstTossedAt(),
		LastTossedAt:  t.GetLastTossedAt(),
		CancelledAt:   t.GetCancelledAt(),
	}
}

func coinTossesToAPI(list []*coinrpc.TossInfo) []types.CoinTossInfo {
	out := make([]types.CoinTossInfo, 0, len(list))
	for _, t := range list {
		out = append(out, coinTossToAPI(t))
	}
	return out
}

// coinTargetToAPI 内容侧硬币汇总。不投影 like_count：点赞归 engagement，
// coin 服务留这个字段恒为 0 只是让调用方别去猜（见 coin.proto 注释）。
func coinTargetToAPI(s *coinrpc.TargetCoinSummary) types.CoinTargetSummary {
	if s == nil {
		return types.CoinTargetSummary{}
	}
	return types.CoinTargetSummary{
		Aid:           s.GetAid(),
		CoinCount:     s.GetCoinCount(),
		CoinUserCount: s.GetCoinUserCount(),
	}
}

func coinTargetsToAPI(list []*coinrpc.TargetCoinSummary) []types.CoinTargetSummary {
	out := make([]types.CoinTargetSummary, 0, len(list))
	for _, s := range list {
		out = append(out, coinTargetToAPI(s))
	}
	return out
}

// ---------- payment ----------

func walletToAPI(w *paymentrpc.WalletInfo) types.WalletBalance {
	if w == nil {
		return types.WalletBalance{}
	}
	return types.WalletBalance{
		Mid:          w.GetMid(),
		BalanceMinor: w.GetBalanceMinor(),
		FrozenMinor:  w.GetFrozenMinor(),
		Currency:     w.GetCurrency(),
	}
}

// walletRechargeToAPI 充值单投影。operator / request_id / mtime 属于台账与对账口径，
// types.WalletRecharge 里没有对应位，网关不塞字段（要改先改 app.api 再重新生成）。
func walletRechargeToAPI(r *paymentrpc.RechargeInfo) types.WalletRecharge {
	if r == nil {
		return types.WalletRecharge{}
	}
	return types.WalletRecharge{
		RechargeNo:  r.GetRechargeNo(),
		AmountMinor: r.GetAmountMinor(),
		Currency:    r.GetCurrency(),
		Channel:     int32(r.GetChannel()),
		State:       int32(r.GetState()),
		Reason:      r.GetReason(),
		SettledAt:   r.GetSettledAt(),
		Ctime:       r.GetCtime(),
	}
}

func walletRechargesToAPI(list []*paymentrpc.RechargeInfo) []types.WalletRecharge {
	out := make([]types.WalletRecharge, 0, len(list))
	for _, r := range list {
		out = append(out, walletRechargeToAPI(r))
	}
	return out
}

func walletFlowsToAPI(list []*paymentrpc.FlowInfo) []types.WalletFlow {
	out := make([]types.WalletFlow, 0, len(list))
	for _, f := range list {
		out = append(out, types.WalletFlow{
			FlowId:            f.GetFlowId(),
			BizType:           int32(f.GetBizType()),
			BizNo:             f.GetBizNo(),
			DeltaMinor:        f.GetDeltaMinor(),
			BalanceAfterMinor: f.GetBalanceAfterMinor(),
			Currency:          f.GetCurrency(),
			Remark:            f.GetRemark(),
			Ctime:             f.GetCtime(),
		})
	}
	return out
}

// walletChannelsToAPI 渠道自述投影。sandbox_only / real_money 是「这不是真实资金」的
// 显式声明，必须原样透出去（哪怕全为 false 也不省略），端上据此标注文案。
func walletChannelsToAPI(list []*paymentrpc.ChannelState) []types.WalletChannelState {
	out := make([]types.WalletChannelState, 0, len(list))
	for _, c := range list {
		out = append(out, types.WalletChannelState{
			Channel:   int32(c.GetChannel()),
			Enabled:   c.GetEnabled(),
			RealMoney: c.GetRealMoney(),
			Note:      c.GetNote(),
		})
	}
	return out
}

// ---------- trade-order ----------

// orderToAPI 订单投影：金额（unit_price_minor / amount_minor / refunded_minor）全部是
// trade-order 服务端重算值，网关不换算不折扣；state / fulfill_state / fulfill_detail
// 原样透出，让用户能看到「卡在哪一步」。
func orderToAPI(o *tradeorderrpc.OrderInfo) types.OrderInfo {
	if o == nil {
		return types.OrderInfo{}
	}
	return types.OrderInfo{
		OrderNo:        o.GetOrderNo(),
		BizType:        int32(o.GetBizType()),
		PlanId:         o.GetPlanId(),
		PlanCode:       o.GetPlanCode(),
		Title:          o.GetTitle(),
		Quantity:       o.GetQuantity(),
		DurationDays:   o.GetDurationDays(),
		CoinAmount:     o.GetCoinAmount(),
		UnitPriceMinor: o.GetUnitPriceMinor(),
		AmountMinor:    o.GetAmountMinor(),
		RefundedMinor:  o.GetRefundedMinor(),
		Currency:       o.GetCurrency(),
		PayMethod:      int32(o.GetPayMethod()),
		State:          int32(o.GetState()),
		FulfillState:   int32(o.GetFulfillState()),
		FulfillDetail:  o.GetFulfillDetail(),
		PaymentNo:      o.GetPaymentNo(),
		ExpireAt:       o.GetExpireAt(),
		Platform:       int32(o.GetPlatform()),
		CreatedAt:      o.GetCreatedAt(),
		PaidAt:         o.GetPaidAt(),
		FulfilledAt:    o.GetFulfilledAt(),
		ClosedAt:       o.GetClosedAt(),
	}
}

func ordersToAPI(list []*tradeorderrpc.OrderInfo) []types.OrderInfo {
	out := make([]types.OrderInfo, 0, len(list))
	for _, o := range list {
		out = append(out, orderToAPI(o))
	}
	return out
}

// orderEventsToAPI 状态流转投影。不投影 operator：终端不需要看到运营工号，
// from_state/to_state/reason/ctime 已足以让用户知道单子在谁手里走到哪一步。
func orderEventsToAPI(list []*tradeorderrpc.OrderEventInfo) []types.OrderEvent {
	out := make([]types.OrderEvent, 0, len(list))
	for _, e := range list {
		out = append(out, types.OrderEvent{
			EventId:   e.GetEventId(),
			FromState: int32(e.GetFromState()),
			ToState:   int32(e.GetToState()),
			Reason:    e.GetReason(),
			Ctime:     e.GetCtime(),
		})
	}
	return out
}

// ---------- creator-revenue ----------

func revRuleToAPI(r *creatorrevenuerpc.RevenueRuleInfo) types.RevRule {
	if r == nil {
		return types.RevRule{}
	}
	return types.RevRule{
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
	}
}

func revRulesToAPI(list []*creatorrevenuerpc.RevenueRuleInfo) []types.RevRule {
	out := make([]types.RevRule, 0, len(list))
	for _, r := range list {
		out = append(out, revRuleToAPI(r))
	}
	return out
}

// revEnrollmentToAPI 参与关系投影。不投影 operator / remark：
// 后者可能带运营内部备注（含工号），终端面没有信息价值。
func revEnrollmentToAPI(e *creatorrevenuerpc.EnrollmentInfo) types.RevEnrollment {
	if e == nil {
		return types.RevEnrollment{}
	}
	return types.RevEnrollment{
		Mid:               e.GetMid(),
		State:             int32(e.GetState()),
		AgreedRuleVersion: e.GetAgreedRuleVersion(),
		EnrolledAt:        e.GetEnrolledAt(),
		LeftAt:            e.GetLeftAt(),
		UpdatedAt:         e.GetUpdatedAt(),
	}
}

func revMetricsToAPI(list []*creatorrevenuerpc.RevenueMetricInfo) []types.RevMetric {
	out := make([]types.RevMetric, 0, len(list))
	for _, m := range list {
		out = append(out, types.RevMetric{
			Period:            m.GetPeriod(),
			Aid:               m.GetAid(),
			SourceType:        int32(m.GetSourceType()),
			RuleCode:          m.GetRuleCode(),
			Quantity:          m.GetQuantity(),
			Unit:              m.GetUnit(),
			AmountMinor:       m.GetAmountMinor(),
			CappedAmountMinor: m.GetCappedAmountMinor(),
			SourceDetail:      m.GetSourceDetail(),
			Ctime:             m.GetCtime(),
		})
	}
	return out
}

// revSettlementToAPI 结算单投影。payout_state 恒为 NOT_PAYABLE 是契约的一部分
// （creator-revenue.proto：本项目无出金通道），网关不得用 state==CONFIRMED 推断「已到账」。
func revSettlementToAPI(s *creatorrevenuerpc.SettlementInfo) types.RevSettlement {
	if s == nil {
		return types.RevSettlement{}
	}
	return types.RevSettlement{
		SettlementNo:    s.GetSettlementNo(),
		Period:          s.GetPeriod(),
		AmountMinor:     s.GetAmountMinor(),
		CapAppliedMinor: s.GetCapAppliedMinor(),
		Currency:        s.GetCurrency(),
		MetricCount:     s.GetMetricCount(),
		State:           int32(s.GetState()),
		PayoutState:     int32(s.GetPayoutState()),
		ConfirmedAt:     s.GetConfirmedAt(),
		Ctime:           s.GetCtime(),
	}
}

func revSettlementsToAPI(list []*creatorrevenuerpc.SettlementInfo) []types.RevSettlement {
	out := make([]types.RevSettlement, 0, len(list))
	for _, s := range list {
		out = append(out, revSettlementToAPI(s))
	}
	return out
}

func revSettlementItemsToAPI(list []*creatorrevenuerpc.SettlementItem) []types.RevSettlementItem {
	out := make([]types.RevSettlementItem, 0, len(list))
	for _, i := range list {
		out = append(out, types.RevSettlementItem{
			SourceType:  int32(i.GetSourceType()),
			RuleCode:    i.GetRuleCode(),
			Quantity:    i.GetQuantity(),
			AmountMinor: i.GetAmountMinor(),
		})
	}
	return out
}

// revSummaryToAPI 收益概览投影。
//
// enrolled 是「是否存在有效参与关系」的便捷位：除「从未参加」(UNSPECIFIED) 与「已退出」(LEFT)
// 外都为 true，含被运营暂停（SUSPENDED）——暂停者仍在计划里、只是本周期不结算，渲染成
// enrolled=false 会让端上错误地显示「立即参加」。真实口径以同时投影出的 enrollment_state 为准，
// 两位必须一起给，端上不能只看布尔。
//
// 金额位全部取下游返回值：current_estimate_minor 是未结算预估（会随更正变动），
// total_confirmed_minor 是已确认**应计**合计，两者都不是「已到账」；
// payout_available / payout_note 原样透出（本项目无出金通道，恒 false），
// 网关不把它推断成任何支付结论。
func revSummaryToAPI(r *creatorrevenuerpc.GetRevenueSummaryReply) types.RevSummaryData {
	if r == nil {
		return types.RevSummaryData{}
	}
	state := r.GetEnrollment().GetState()
	return types.RevSummaryData{
		Enrolled: state != creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_UNSPECIFIED &&
			state != creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_LEFT,
		EnrollmentState:      int32(state),
		CurrentPeriod:        r.GetCurrentPeriod(),
		CurrentEstimateMinor: r.GetCurrentEstimateMinor(),
		TotalConfirmedMinor:  r.GetTotalConfirmedMinor(),
		LastSettledPeriod:    r.GetLastSettledPeriod(),
		PayoutAvailable:      r.GetPayoutAvailable(),
		PayoutNote:           r.GetPayoutNote(),
	}
}
