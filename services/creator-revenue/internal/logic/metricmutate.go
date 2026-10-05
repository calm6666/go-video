// 本文件是 logic 包的手写扩展（计量台账的入参校验、折算、更正留痕与月度封顶重分配），
// 不是 goctl 生成产物。SQL 与库表读写一律留在 model（AGENTS.md §4）。

package logic

import (
	"context"
	"fmt"
	"math"
	"strings"

	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// thresholdBlockedValue / thresholdUnblockedValue 对齐 cr_metric.threshold_blocked 列。
const (
	thresholdUnblockedValue int32 = 0
	thresholdBlockedValue   int32 = 1
)

// metricOutcome 是一次台账写入的结论，决定 reply 的 created / corrected 两位。
type metricOutcome int

const (
	// metricCreated 首次写入该唯一键 (period, mid, aid, source_type) 的台账行。
	metricCreated metricOutcome = iota
	// metricCorrected 同唯一键的更正上报：旧值已进 cr_metric_change_log，新值就地覆盖。
	metricCorrected
	// metricUnchanged 同值重放：本次没有任何写入（created/corrected 双双为 false）。
	// 与 corrected 区分开很重要——「重复上报同一个数」不该在更正台账里
	// 留下一条 old==new 的噪音行，那会让复核面板看不出到底改过没有。
	metricUnchanged
)

// metricDraft 是校验完的台账写入意图，字段与 cr_metric 列一一对应。
type metricDraft struct {
	Period       string
	Mid          int64
	Aid          int64
	SourceType   int32
	RuleCode     string
	Quantity     int64
	Operator     string
	RequestId    string
	SourceDetail string
	Reason       string
}

// validateMetricUpsert 跑完 RecordRevenueMetric 所有「不查库就能判」的口径。
//
// 护栏：
//   - period 必填且不得晚于当前周期：给未来周期记台账等于凭空造应计；
//   - quantity 负数一律拒绝（负数会把「应付」折成「倒扣」，是脏数据入口）；
//   - 来源必须是真实枚举（UNSPECIFIED 不可计量，否则出单聚合无从归属）；
//   - reason：系统回填（cron/spm/creatorrevenue）是事实搬运，可不留；
//     运营工号是人为判断，必须留；ACTIVITY 来源按 proto 注释「手工回填必须有 reason」，
//     无论谁调都要留。
func validateMetricUpsert(in *rpc.RecordRevenueMetricReq) (*metricDraft, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: 请求为空", model.ErrInvalidPeriod)
	}
	d := &metricDraft{
		SourceType:   int32(in.SourceType),
		RuleCode:     strings.TrimSpace(in.RuleCode),
		Quantity:     in.Quantity,
		SourceDetail: strings.TrimSpace(in.SourceDetail),
	}
	period, err := requirePeriodNotFuture(in.Period)
	if err != nil {
		return nil, err
	}
	d.Period = period
	if d.Mid, err = normalizeMid(in.Mid); err != nil {
		return nil, err
	}
	if d.Aid, err = normalizeAid(in.Aid); err != nil {
		return nil, err
	}
	if err := validSourceType(d.SourceType); err != nil {
		return nil, err
	}
	if d.RuleCode == "" {
		return nil, model.ErrRuleCodeRequired
	}
	if err := checkText("rule_code", d.RuleCode, model.MaxRuleCodeBytes); err != nil {
		return nil, err
	}
	if d.Quantity < 0 {
		return nil, fmt.Errorf("%w: quantity=%d", model.ErrNegativeQuantity, d.Quantity)
	}
	if err := checkText("source_detail", d.SourceDetail, model.MaxSourceDetailBytes); err != nil {
		return nil, err
	}
	if d.Operator, err = requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if d.RequestId, err = requireRequestID(in.RequestId, model.MaxRequestIDBytes); err != nil {
		return nil, err
	}
	d.Reason = strings.TrimSpace(in.Reason)
	if err := checkText("reason", d.Reason, model.MaxReasonBytes); err != nil {
		return nil, err
	}
	if d.SourceType == model.SourceTypeActivity || !IsSystemOperator(d.Operator) {
		if d.Reason == "" {
			return nil, fmt.Errorf("%w: operator=%s source_type=%d 的台账回填/更正必须留 reason",
				model.ErrReasonRequired, d.Operator, d.SourceType)
		}
	}
	return d, nil
}

// requirePeriodNotFuture 校验周期码并拒掉晚于当前周期的值。
// 台账写入与出单共用这一条：本服务不接受任何「为还没开始的月份先记一笔应计」。
// 当前周期允许——月度内的增量回填是常态，收官前的不确定性由出单/确认两道闸门兜。
func requirePeriodNotFuture(raw string) (string, error) {
	period, err := model.ValidatePeriod(raw)
	if err != nil {
		return "", err
	}
	if period > model.CurrentPeriod() {
		return "", fmt.Errorf("%w: period=%s，当前周期=%s",
			model.ErrFuturePeriod, period, model.CurrentPeriod())
	}
	return period, nil
}

// applyMetricInTx 在一个事务里完成「参与闸门 + 规则闸门 + 折算 + 更正留痕 + 封顶重分配」。
//
// 事务边界为什么必须这么大：台账金额、封顶后金额与更正留痕若分两次提交，
// 中途失败会留下「新金额已生效但旧值没留痕」的断链，结算争议就只能靠猜（AGENTS.md §5、§8）。
//
// 返回的 row 是事务内回读的真值（含封顶重分配后的 capped_amount_minor），
// 调用方直接投影，不再用内存里算出的值冒充库内结论。
func applyMetricInTx(
	ctx context.Context, tx sqlx.Session, d *metricDraft,
) (*model.RevenueMetric, metricOutcome, error) {
	conn := sqlx.NewSqlConnFromSession(tx)
	enrollments := model.NewEnrollmentModel(conn)
	rules := model.NewRevenueRuleModel(conn)
	settlements := model.NewSettlementModel(conn)
	metrics := model.NewRevenueMetricModel(conn)
	logs := model.NewMetricChangeLogModel(conn)

	// 锁 cr_enrollment 的那一行，是本服务给「同一作者的计量与出单」定的串行点。
	// 不锁会写出这类孤儿事实：运营刚把账号置为 SUSPENDED，spm 的回填仍把应计记进台账。
	enr, err := enrollments.LockByMid(ctx, d.Mid)
	if err != nil {
		return nil, 0, err
	}
	if enr == nil {
		return nil, 0, fmt.Errorf("%w: mid=%d 未参加计划，不产生计量台账", model.ErrEnrollmentNotFound, d.Mid)
	}
	if enr.State != model.EnrollmentStateEnrolled {
		if enr.State == model.EnrollmentStateSuspended {
			return nil, 0, fmt.Errorf("%w: mid=%d state=%d", model.ErrEnrollmentSuspended, d.Mid, enr.State)
		}
		return nil, 0, fmt.Errorf("%w: mid=%d state=%d，只有 ENROLLED 可计量",
			model.ErrNotEnrolled, d.Mid, enr.State)
	}

	rule, err := rules.LockByCode(ctx, d.RuleCode)
	if err != nil {
		return nil, 0, err
	}
	if rule == nil {
		return nil, 0, fmt.Errorf("%w: rule_code=%s", model.ErrRuleNotFound, d.RuleCode)
	}
	// 锁行后仍要重判状态：SetRevenueRuleState 可能已经把它归档。
	if rule.State != model.RuleStateActive {
		return nil, 0, fmt.Errorf("%w: rule_code=%s 当前 state=%d，计量必须按 ACTIVE 规则折算",
			model.ErrRuleNotActive, rule.RuleCode, rule.State)
	}
	if rule.SourceType != d.SourceType {
		return nil, 0, fmt.Errorf("%w: rule_code=%s 的来源是 %d，请求声明 %d",
			model.ErrRuleSourceMismatch, rule.RuleCode, rule.SourceType, d.SourceType)
	}
	periodStart, err := model.PeriodStartUnix(d.Period)
	if err != nil {
		return nil, 0, err
	}
	if periodStart < rule.EffectiveFrom {
		return nil, 0, fmt.Errorf("%w: period=%s 起点早于 rule_code=%s 的 effective_from=%d",
			model.ErrRuleNotEffective, d.Period, rule.RuleCode, rule.EffectiveFrom)
	}

	// 本周期已出单且已确认 → 台账冻结：确认后的金额是运营对外承诺的口径，
	// 就地改台账会让「已确认的结算单」与「支撑它的台账」不一致，且这种不一致不可解释。
	// 要改必须先用 GenerateSettlement(force_void_confirmed=true) 作废重算。
	active, err := settlements.FindActive(ctx, d.Period, d.Mid)
	if err != nil {
		return nil, 0, err
	}
	if active != nil && active.State == model.SettlementStateConfirmed {
		return nil, 0, fmt.Errorf("%w: period=%s mid=%d 结算单 %s 已确认（confirmed_by=%s），"+
			"需更正请先作废重算", model.ErrSettlementConfirmed, d.Period, d.Mid,
			active.SettlementNo, active.ConfirmedBy)
	}

	amount, err := model.ComputeAmountMinor(d.Quantity, rule.UnitPricePer1000)
	if err != nil {
		return nil, 0, err
	}
	capped, blocked := model.ApplyThreshold(d.Quantity, rule.MinQuantity, amount)
	threshold := thresholdUnblockedValue
	if blocked {
		threshold = thresholdBlockedValue
	}

	cur, err := metrics.LockByKey(ctx, d.Period, d.Mid, d.Aid, d.SourceType)
	if err != nil {
		return nil, 0, err
	}
	outcome := metricCreated
	switch {
	case cur == nil:
		if _, err := metrics.Insert(ctx, &model.RevenueMetric{
			Period: d.Period, Mid: d.Mid, Aid: d.Aid, SourceType: d.SourceType,
			RuleCode: rule.RuleCode, RuleVersion: rule.Version,
			Quantity: d.Quantity, Unit: rule.Unit,
			AmountMinor: amount, CappedAmountMinor: capped, ThresholdBlocked: threshold,
			SourceDetail: metricSourceDetail(d, rule, amount, blocked),
			Corrected:    0,
		}); err != nil {
			if model.IsDuplicateErr(err) {
				// LockByKey 已在 uniq_metric_key 上取了 next-key 锁，正常不会撞；
				// 真撞了说明另有会话绕锁写入，回可重试错误而不是把重复键当新建成功。
				return nil, 0, fmt.Errorf("%w: 台账行 (%s,%d,%d,%d) 已被并发写入，请重试",
					model.ErrConcurrentUpdate, d.Period, d.Mid, d.Aid, d.SourceType)
			}
			return nil, 0, err
		}
	case cur.Quantity == d.Quantity && cur.RuleCode == rule.RuleCode &&
		cur.RuleVersion == rule.Version && cur.AmountMinor == amount:
		// 同值重放：不写主表、不写变更台账，只把封顶额度重算一遍（幂等且自愈）。
		outcome = metricUnchanged
	default:
		// 更正：旧值必须先进台账，再就地覆盖。顺序反过来的话，
		// 一旦覆盖成功而台账写入失败，「原来算多少」就永久丢失了。
		if _, err := logs.Insert(ctx, &model.MetricChangeLog{
			Period: d.Period, Mid: d.Mid, Aid: d.Aid, SourceType: d.SourceType,
			RuleCode:             rule.RuleCode,
			OldRuleVersion:       cur.RuleVersion,
			NewRuleVersion:       rule.Version,
			OldQuantity:          cur.Quantity,
			NewQuantity:          d.Quantity,
			OldAmountMinor:       cur.AmountMinor,
			NewAmountMinor:       amount,
			OldCappedAmountMinor: cur.CappedAmountMinor,
			NewCappedAmountMinor: capped,
			Operator:             d.Operator,
			Reason:               trunc(d.Reason, model.MaxReasonBytes),
			RequestId:            d.RequestId,
		}); err != nil {
			return nil, 0, err
		}
		next := *cur
		next.RuleCode = rule.RuleCode
		next.RuleVersion = rule.Version
		next.Quantity = d.Quantity
		next.Unit = rule.Unit
		next.AmountMinor = amount
		next.CappedAmountMinor = capped
		next.ThresholdBlocked = threshold
		next.SourceDetail = metricSourceDetail(d, rule, amount, blocked)
		ok, err := metrics.UpdateCorrection(ctx, &next, cur.Quantity)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			return nil, 0, fmt.Errorf("%w: metric_id=%d 更正未命中（数量已被并发改变），请重试",
				model.ErrConcurrentUpdate, cur.MetricId)
		}
		outcome = metricCorrected
	}

	// 月度封顶是 (period, mid, source_type) 组级约束，单行写完了还得整组重分配，
	// 否则「先写的行占满额度」会让后写的行永远拿不到应计。
	if err := reallocGroupCap(ctx, tx, d.Period, d.Mid, d.SourceType, rule.MonthlyCapMinor); err != nil {
		return nil, 0, err
	}

	after, err := metrics.FindByKey(ctx, d.Period, d.Mid, d.Aid, d.SourceType)
	if err != nil {
		return nil, 0, err
	}
	if after == nil {
		return nil, 0, fmt.Errorf("%w: (%s,%d,%d,%d) 提交前读不到",
			model.ErrMetricNotFound, d.Period, d.Mid, d.Aid, d.SourceType)
	}
	return after, outcome, nil
}

// reallocGroupCap 按 aid、metric_id 升序把月度封顶额度确定性地分给同组台账行。
//
// 为什么按这个顺序：同一批数据必须在任何写入顺序下得到相同结果，
// 否则「重算/更正两次算出两个金额」，运营无法解释、也无法复核。
// 被门槛拦掉的行不占额度（threshold_blocked 落库就是为了不依赖「规则当时的门槛值」这种历史推断）。
// capMinor <= 0 表示本组不限额。
func reallocGroupCap(
	ctx context.Context, tx sqlx.Session, period string, mid int64, sourceType int32, capMinor int64,
) error {
	metrics := model.NewRevenueMetricModel(sqlx.NewSqlConnFromSession(tx))
	rows, err := metrics.ListGroupForUpdate(ctx, period, mid, sourceType)
	if err != nil {
		return err
	}
	var used int64
	for _, r := range rows {
		if r.ThresholdBlocked == thresholdBlockedValue {
			if r.CappedAmountMinor != 0 {
				if err := metrics.SetCappedAmount(ctx, r.MetricId, 0); err != nil {
					return err
				}
			}
			continue
		}
		room := int64(math.MaxInt64)
		if capMinor > 0 {
			room = capMinor - used
		}
		alloc, _ := model.AllocateCap(r.AmountMinor, room)
		if used > 0 && alloc > math.MaxInt64-used {
			return fmt.Errorf("%w: 组内已用额度 %d + 本行 %d 超出 int64",
				model.ErrAmountOverflow, used, alloc)
		}
		used += alloc
		if alloc != r.CappedAmountMinor {
			if err := metrics.SetCappedAmount(ctx, r.MetricId, alloc); err != nil {
				return err
			}
		}
	}
	return nil
}

// metricSourceDetail 组装「计算依据摘要」（proto 要求不含 PII）。
// 调用方给了就用调用方的；没给就按公式渲染一句人话，保证复核时能反推算法。
func metricSourceDetail(d *metricDraft, rule *model.RevenueRule, amount int64, blocked bool) string {
	base := d.SourceDetail
	if base == "" {
		base = fmt.Sprintf("按规则 %s(v%d) 折算：quantity=%d %s × %d 分/千单位 = %d 分",
			rule.RuleCode, rule.Version, d.Quantity, rule.Unit, rule.UnitPricePer1000, amount)
	}
	if blocked {
		base = fmt.Sprintf("%s；低于规则门槛 min_quantity=%d，本条不结算，也不占用月度封顶额度",
			base, rule.MinQuantity)
	}
	return trunc(base, model.MaxSourceDetailBytes)
}
