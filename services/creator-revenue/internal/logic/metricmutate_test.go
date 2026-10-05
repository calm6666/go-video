package logic

// 本文件锁 metricmutate.go 的计量台账口径：闸门顺序、折算复用 model 的纯函数、
// 幂等（唯一键 + 同值）、更正留痕顺序，以及月度封顶的组级确定性重分配。

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// metricRule 造一条 ACTIVE 折算规则：单价 1000 分/千分钟（即 1 分/分钟），
// 门槛 10 分钟，月度封顶 1500 分。
func metricRule() *model.RevenueRule {
	return &model.RevenueRule{
		RuleCode: "R_VIP", SourceType: model.SourceTypeVipWatch, Name: "会员有效观看",
		UnitPricePer1000: 1000, Currency: "CNY", Unit: "minute", MinQuantity: 10,
		MonthlyCapMinor: 1500, State: model.RuleStateActive, Version: 3,
	}
}

func metricDraftIn(period string, mid, aid, qty int64, operator, reason string) *metricDraft {
	return &metricDraft{
		Period: period, Mid: mid, Aid: aid, SourceType: model.SourceTypeVipWatch,
		RuleCode: "R_VIP", Quantity: qty, Operator: operator, RequestId: "req-" + operator, Reason: reason,
	}
}

// runMetricTx 从真实事务边界进入被测函数（RecordRevenueMetricLogic 就是
// svc.Conn.TransactCtx 里调 applyMetricInTx），这样失败路径的「整体回滚」也一并被断言。
func runMetricTx(db *fakeDB, d *metricDraft) (*model.RevenueMetric, metricOutcome, error) {
	var (
		row     *model.RevenueMetric
		outcome metricOutcome
	)
	err := fakeConn{db: db}.TransactCtx(context.Background(),
		func(ctx context.Context, tx sqlx.Session) error {
			var err error
			row, outcome, err = applyMetricInTx(ctx, tx, d)
			return err
		})
	return row, outcome, err
}

// ---------------------------------------------------------------- 入参校验

func TestValidateMetricUpsertGates(t *testing.T) {
	cur := model.CurrentPeriod()
	mk := func(mut func(*rpc.RecordRevenueMetricReq)) *rpc.RecordRevenueMetricReq {
		in := &rpc.RecordRevenueMetricReq{
			Period: cur, Mid: 100, Aid: 11, SourceType: rpc.RevenueSourceType(model.SourceTypeVipWatch),
			RuleCode: "R_VIP", Quantity: 120, Operator: "cron", RequestId: "req-1",
		}
		mut(in)
		return in
	}

	d, err := validateMetricUpsert(mk(func(in *rpc.RecordRevenueMetricReq) {
		in.SourceDetail = "spm 有效观看回填"
	}))
	mustNoErr(t, err)
	if d.Period != cur || d.Mid != 100 || d.Aid != 11 || d.Quantity != 120 {
		t.Fatalf("草稿字段未按入参归一：%+v", d)
	}
	// 系统回填（cron/spm/creatorrevenue）是事实搬运，允许不留 reason。
	if d.Reason != "" {
		t.Fatalf("系统回填不该要求 reason：%q", d.Reason)
	}

	cases := []struct {
		name   string
		in     *rpc.RecordRevenueMetricReq
		target error
	}{
		{"未来周期拒绝", mk(func(in *rpc.RecordRevenueMetricReq) { in.Period = "299912" }), model.ErrFuturePeriod},
		{"周期格式拒绝", mk(func(in *rpc.RecordRevenueMetricReq) { in.Period = "2026-1" }), model.ErrInvalidPeriod},
		{"mid 必须真实", mk(func(in *rpc.RecordRevenueMetricReq) { in.Mid = 0 }), model.ErrInvalidMid},
		{"aid 允许 0 但负数拒绝", mk(func(in *rpc.RecordRevenueMetricReq) { in.Aid = -1 }), model.ErrInvalidAid},
		{"来源必须是真实枚举", mk(func(in *rpc.RecordRevenueMetricReq) {
			in.SourceType = rpc.RevenueSourceType(model.SourceTypeUnspecified)
		}), model.ErrInvalidSourceType},
		{"rule_code 必填", mk(func(in *rpc.RecordRevenueMetricReq) { in.RuleCode = "  " }), model.ErrRuleCodeRequired},
		{"quantity 负数拒绝", mk(func(in *rpc.RecordRevenueMetricReq) { in.Quantity = -1 }), model.ErrNegativeQuantity},
		{"operator 必填", mk(func(in *rpc.RecordRevenueMetricReq) { in.Operator = "" }), model.ErrOperatorRequired},
		{"request_id 必填", mk(func(in *rpc.RecordRevenueMetricReq) { in.RequestId = "" }), model.ErrRequestIDRequired},
		// 运营工号是「人为判断」，与系统回填不同：必须留 reason。
		{"运营回填必须留 reason", mk(func(in *rpc.RecordRevenueMetricReq) {
			in.Operator = "ops-77"
		}), model.ErrReasonRequired},
		{"活动激励无论谁调都要 reason", mk(func(in *rpc.RecordRevenueMetricReq) {
			in.SourceType = rpc.RevenueSourceType(model.SourceTypeActivity)
		}), model.ErrReasonRequired},
		{"rule_code 超列宽拒绝", mk(func(in *rpc.RecordRevenueMetricReq) {
			in.RuleCode = strings.Repeat("R", model.MaxRuleCodeBytes+1)
		}), model.ErrTextTooLong},
		{"source_detail 超列宽拒绝", mk(func(in *rpc.RecordRevenueMetricReq) {
			in.SourceDetail = strings.Repeat("d", model.MaxSourceDetailBytes+1)
		}), model.ErrTextTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := validateMetricUpsert(c.in); !errors.Is(err, c.target) {
				t.Fatalf("期望 %v，实际 %v", c.target, err)
			}
		})
	}
	// aid=0（运营活动激励不挂具体内容）是合法的，不能被 normalizeAid 拒掉。
	if _, err := validateMetricUpsert(mk(func(in *rpc.RecordRevenueMetricReq) { in.Aid = 0 })); err != nil {
		t.Fatalf("aid=0 应合法：%v", err)
	}
}

// ---------------------------------------------------------------- 折算与闸门

func TestApplyMetricCreatesRowFromActiveRule(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(metricRule())

	row, outcome, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 1200, "cron", ""))
	mustNoErr(t, err)
	if outcome != metricCreated {
		t.Fatalf("首写应判 created，实际 %d", outcome)
	}
	// 金额只能由本服务的折算函数产出：1200 分钟 × 1000 分/千分钟 = 1200 分。
	if row.AmountMinor != 1200 || row.CappedAmountMinor != 1200 {
		t.Fatalf("折算结果错误：amount=%d capped=%d", row.AmountMinor, row.CappedAmountMinor)
	}
	if row.RuleCode != "R_VIP" || row.RuleVersion != 3 || row.Unit != "minute" {
		t.Fatalf("台账必须冻结规则版本快照：%+v", row)
	}
	if row.ThresholdBlocked != 0 || row.Corrected != 0 {
		t.Fatalf("达标行不该被拦：%+v", row)
	}
	if row.SourceDetail == "" || !strings.Contains(row.SourceDetail, "1200") {
		t.Fatalf("缺计算依据摘要：%q", row.SourceDetail)
	}
	if len(db.metricLogs) != 0 {
		t.Fatalf("首写不该进更正台账：%+v", db.metricLogs)
	}
}

func TestApplyMetricThresholdBlockedKeepsAmountAndZeroesCapped(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(metricRule())

	row, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 5, "cron", ""))
	mustNoErr(t, err)
	if row.AmountMinor != 5 || row.CappedAmountMinor != 0 || row.ThresholdBlocked != 1 {
		t.Fatalf("门槛拦掉的行要保留「本该多少」并清零参与封顶的金额：%+v", row)
	}
	if !strings.Contains(row.SourceDetail, "min_quantity=10") {
		t.Fatalf("门槛说明必须进摘要：%q", row.SourceDetail)
	}
}

func TestApplyMetricGates(t *testing.T) {
	ruleWith := func(mut func(*model.RevenueRule)) *model.RevenueRule {
		r := metricRule()
		mut(r)
		return r
	}
	cases := []struct {
		name   string
		enroll bool
		state  int32
		rule   *model.RevenueRule
		settle *model.Settlement
		target error
	}{
		{name: "未参加计划不产生台账", target: model.ErrEnrollmentNotFound},
		{name: "暂停期间不计量", enroll: true, state: model.EnrollmentStateSuspended, rule: metricRule(),
			target: model.ErrEnrollmentSuspended},
		{name: "退出后不再计量", enroll: true, state: model.EnrollmentStateLeft, rule: metricRule(),
			target: model.ErrNotEnrolled},
		{name: "规则不存在", enroll: true, state: model.EnrollmentStateEnrolled, target: model.ErrRuleNotFound},
		{name: "必须按 ACTIVE 规则折算", enroll: true, state: model.EnrollmentStateEnrolled,
			rule:   ruleWith(func(r *model.RevenueRule) { r.State = model.RuleStateDraft }),
			target: model.ErrRuleNotActive},
		{name: "来源与规则不一致", enroll: true, state: model.EnrollmentStateEnrolled,
			rule:   ruleWith(func(r *model.RevenueRule) { r.SourceType = model.SourceTypeCoin }),
			target: model.ErrRuleSourceMismatch},
		{name: "周期早于规则生效起点", enroll: true, state: model.EnrollmentStateEnrolled,
			rule:   ruleWith(func(r *model.RevenueRule) { r.EffectiveFrom = math.MaxInt32 }),
			target: model.ErrRuleNotEffective},
		{name: "结算单已确认则台账冻结", enroll: true, state: model.EnrollmentStateEnrolled,
			rule: metricRule(),
			settle: &model.Settlement{SettlementNo: "CRS202601-100-1", Period: txPeriod, Mid: txMid,
				AmountMinor: 1, Currency: "CNY", State: model.SettlementStateConfirmed,
				PayoutState: model.PayoutStateNotPayable, ConfirmedBy: "ops-01", VoidSeq: 0},
			target: model.ErrSettlementConfirmed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, db := newTestSvc(t)
			if c.enroll {
				seedEnrolled(db, txMid, c.state)
			}
			if c.rule != nil {
				db.addRule(c.rule)
			}
			if c.settle != nil {
				db.addSettlement(c.settle)
			}
			_, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 100, "cron", ""))
			mustErrIs(t, err, c.target)
			if len(db.metrics) != 0 {
				t.Fatalf("闸门未过却写了台账：%+v", db.metrics)
			}
		})
	}
}

// DRAFT 在效单不冻结台账：出单只是「算完没确认」，运营还要能改。
func TestApplyMetricAllowsCorrectionWithDraftSettlement(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(metricRule())
	db.addSettlement(&model.Settlement{SettlementNo: "CRS202601-100-1", Period: txPeriod, Mid: txMid,
		AmountMinor: 1, Currency: "CNY", State: model.SettlementStateDraft,
		PayoutState: model.PayoutStateNotPayable, VoidSeq: 0})
	_, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 100, "cron", ""))
	mustNoErr(t, err)
}

// ---------------------------------------------------------------- 幂等与更正

func TestApplyMetricSameValueReplayWritesNothing(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(metricRule())
	d := metricDraftIn(txPeriod, txMid, 11, 1200, "cron", "")

	first, outcome, err := runMetricTx(db, d)
	mustNoErr(t, err)
	if outcome != metricCreated {
		t.Fatalf("首写应 created：%d", outcome)
	}
	// 同值重放：当前真实去重口径是「唯一键 (period,mid,aid,source_type) + 值相同」，
	// cr_metric 没有 request_id 列（proto 缺口，本期不加字段），所以 request_id 不同也照样判重放。
	before := len(db.calls)
	row, outcome, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 1200, "cron", ""))
	mustNoErr(t, err)
	if outcome != metricUnchanged {
		t.Fatalf("同值重放必须判 unchanged（既不 created 也不 corrected），实际 %d", outcome)
	}
	if row.CappedAmountMinor != first.CappedAmountMinor {
		t.Fatalf("重放读到的金额被改动：%d vs %d", row.CappedAmountMinor, first.CappedAmountMinor)
	}
	if len(db.metrics) != 1 {
		t.Fatalf("重放不该产生第二行台账：%d", len(db.metrics))
	}
	if len(db.metricLogs) != 0 {
		t.Fatalf("重放不该在更正台账留 old==new 的噪音行：%+v", db.metricLogs)
	}
	if db.countCallsAfter(before, "upd:cr_metric.correction") != 0 ||
		db.countCallsAfter(before, "ins:cr_metric") != 0 {
		t.Fatalf("重放路径不得写主表，实际语句：%v", db.calls[before:])
	}
	// 幂等自愈：重放仍会做一遍组级封顶重算（同组别的行可能已经变了）。
	if db.countCallsAfter(before, "sel:cr_metric.groupLock") == 0 {
		t.Fatal("重放必须重算封顶额度")
	}
}

func TestApplyMetricCorrectionLogsOldValueBeforeOverwrite(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(metricRule())
	if _, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 1200, "cron", "")); err != nil {
		t.Fatalf("首写失败：%v", err)
	}

	row, outcome, err := runMetricTx(db, &metricDraft{
		Period: txPeriod, Mid: txMid, Aid: 11, SourceType: model.SourceTypeVipWatch,
		RuleCode: "R_VIP", Quantity: 700, Operator: "ops-77", RequestId: "req-fix", Reason: "spm 口径修正",
	})
	mustNoErr(t, err)
	if outcome != metricCorrected {
		t.Fatalf("值变了必须判 corrected，实际 %d", outcome)
	}
	if row.Quantity != 700 || row.AmountMinor != 700 || row.Corrected != 1 {
		t.Fatalf("更正未落到主表：%+v", row)
	}
	if len(db.metricLogs) != 1 {
		t.Fatalf("更正必须留一条旧值台账：%+v", db.metricLogs)
	}
	l := db.metricLogs[0]
	if l.OldQuantity != 1200 || l.NewQuantity != 700 || l.OldAmountMinor != 1200 || l.NewAmountMinor != 700 {
		t.Fatalf("旧值/新值记录错误：%+v", l)
	}
	if l.Reason != "spm 口径修正" || l.RequestId != "req-fix" || l.Operator != "ops-77" {
		t.Fatalf("更正留痕缺责任人：%+v", l)
	}
	if l.OldRuleVersion != 3 || l.NewRuleVersion != 3 {
		t.Fatalf("规则版本快照缺失：%+v", l)
	}
}

func TestApplyMetricCorrectionCasMissIsRetryable(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(metricRule())
	if _, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 1200, "cron", "")); err != nil {
		t.Fatalf("首写失败：%v", err)
	}
	db.noRowsFor["upd:cr_metric.correction"] = true // 数量已被并发改变
	_, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 700, "ops-77", "改小"))
	mustErrIs(t, err, model.ErrConcurrentUpdate)
	if len(db.metricLogs) != 0 {
		t.Fatal("并发失败时更正台账也要一起回滚")
	}
}

// 并发绕锁写入时，LockByKey 读空、INSERT 撞 uniq_metric_key：
// 必须回可重试的 ErrConcurrentUpdate，而不是把重复键当「新建成功」。
func TestApplyMetricDuplicateKeyOnInsertIsConcurrentUpdate(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(metricRule())
	rival := db.addMetric(&model.RevenueMetric{
		Period: txPeriod, Mid: txMid, Aid: 11, SourceType: model.SourceTypeVipWatch,
		RuleCode: "R_VIP", RuleVersion: 3, Quantity: 9, AmountMinor: 9, CappedAmountMinor: 9,
	})
	db.hideForRead[rival.MetricId] = true

	_, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 1200, "cron", ""))
	mustErrIs(t, err, model.ErrConcurrentUpdate)
	// 失败方不得留下任何副作用：不新增行、不覆盖别人的行、不写更正台账。
	if len(db.metrics) != 1 {
		t.Fatalf("并发冲突后多出了台账行：%+v", db.metrics)
	}
	if db.metrics[0].Quantity != 9 || db.metrics[0].AmountMinor != 9 {
		t.Fatalf("并发冲突把他人已提交的行覆盖了：%+v", db.metrics[0])
	}
	if len(db.metricLogs) != 0 {
		t.Fatalf("写入未成功却留了更正台账：%+v", db.metricLogs)
	}
	if db.countCalls("ins:cr_metric_change_log") != 0 {
		t.Fatalf("变更台账不该被触及：%v", db.calls)
	}
}

// ---------------------------------------------------------------- 月度封顶重分配

func TestReallocGroupCapIsOrderIndependent(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(metricRule())
	// 先写大额（2000）再写小额（500）：封顶 1500 必须全给 aid 小的那行。
	if _, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 2000, "cron", "")); err != nil {
		t.Fatalf("首写失败：%v", err)
	}
	if _, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 12, 500, "cron", "")); err != nil {
		t.Fatalf("次写失败：%v", err)
	}
	group := db.metricsOf(txPeriod, txMid, model.SourceTypeVipWatch)
	if group[0].CappedAmountMinor != 1500 || group[1].CappedAmountMinor != 0 {
		t.Fatalf("封顶额度分配错误：%+v", group)
	}

	// 反过来写（先小后大）必须得到同一结论：分配顺序押在 aid 上而不是到达顺序上。
	_, db2 := newTestSvc(t)
	seedEnrolled(db2, txMid, model.EnrollmentStateEnrolled)
	db2.addRule(metricRule())
	if _, _, err := runMetricTx(db2, metricDraftIn(txPeriod, txMid, 12, 500, "cron", "")); err != nil {
		t.Fatalf("首写失败：%v", err)
	}
	if _, _, err := runMetricTx(db2, metricDraftIn(txPeriod, txMid, 11, 2000, "cron", "")); err != nil {
		t.Fatalf("次写失败：%v", err)
	}
	g2 := db2.metricsOf(txPeriod, txMid, model.SourceTypeVipWatch)
	if g2[0].CappedAmountMinor != 1500 || g2[1].CappedAmountMinor != 0 {
		t.Fatalf("写入顺序影响了封顶分配：%+v", g2)
	}
}

func TestReallocGroupCapReleasesRoomAfterCorrection(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(metricRule())
	if _, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 2000, "cron", "")); err != nil {
		t.Fatalf("首写失败：%v", err)
	}
	if _, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 12, 500, "cron", "")); err != nil {
		t.Fatalf("次写失败：%v", err)
	}
	// 把 aid 11 更正成 1000：腾出的 500 分必须回到 aid 12，组内合计仍等于封顶。
	if _, _, err := runMetricTx(db, metricDraftIn(txPeriod, txMid, 11, 1000, "ops-77", "口径修正")); err != nil {
		t.Fatalf("更正失败：%v", err)
	}
	group := db.metricsOf(txPeriod, txMid, model.SourceTypeVipWatch)
	var sum int64
	for _, m := range group {
		sum += m.CappedAmountMinor
	}
	if group[0].CappedAmountMinor != 1000 || group[1].CappedAmountMinor != 500 || sum != 1500 {
		t.Fatalf("封顶额度未随更正重分配：%+v 合计 %d", group, sum)
	}
}

func TestReallocGroupCapRules(t *testing.T) {
	t.Run("封顶非正数表示不限额", func(t *testing.T) {
		_, db := newTestSvc(t)
		db.addLedger(txPeriod, txMid,
			ledgerSeed{Aid: 11, SourceType: model.SourceTypeVipWatch, Rule: "R_VIP", Quantity: 9, Amount: math.MaxInt64 / 2, Capped: 1},
			ledgerSeed{Aid: 12, SourceType: model.SourceTypeVipWatch, Rule: "R_VIP", Quantity: 9, Amount: 100, Capped: 0, Blocked: true},
		)
		mustNoErr(t, reallocGroupCap(context.Background(), txOf(db), txPeriod, txMid, model.SourceTypeVipWatch, 0))
		group := db.metricsOf(txPeriod, txMid, model.SourceTypeVipWatch)
		if group[0].CappedAmountMinor != math.MaxInt64/2 {
			t.Fatalf("不限额时应全额计入：%+v", group[0])
		}
		// 被门槛拦掉的行即使之前有金额也必须清零（不占额度）。
		if group[1].CappedAmountMinor != 0 {
			t.Fatalf("拦掉的行必须清零 capped：%+v", group[1])
		}
	})
	t.Run("组内累计溢出必须报错", func(t *testing.T) {
		_, db := newTestSvc(t)
		db.addLedger(txPeriod, txMid,
			ledgerSeed{Aid: 11, SourceType: model.SourceTypeVipWatch, Rule: "R_VIP", Quantity: 1, Amount: 5, Capped: 5},
			ledgerSeed{Aid: 12, SourceType: model.SourceTypeVipWatch, Rule: "R_VIP", Quantity: 1, Amount: math.MaxInt64, Capped: 0},
		)
		err := reallocGroupCap(context.Background(), txOf(db), txPeriod, txMid, model.SourceTypeVipWatch, 0)
		mustErrIs(t, err, model.ErrAmountOverflow)
	})
	t.Run("封顶恰好给满时后写行为 0", func(t *testing.T) {
		_, db := newTestSvc(t)
		db.addLedger(txPeriod, txMid,
			ledgerSeed{Aid: 11, SourceType: model.SourceTypeVipWatch, Rule: "R_VIP", Quantity: 1, Amount: 900, Capped: 900},
			ledgerSeed{Aid: 12, SourceType: model.SourceTypeVipWatch, Rule: "R_VIP", Quantity: 1, Amount: 900, Capped: 900},
		)
		mustNoErr(t, reallocGroupCap(context.Background(), txOf(db), txPeriod, txMid, model.SourceTypeVipWatch, 900))
		group := db.metricsOf(txPeriod, txMid, model.SourceTypeVipWatch)
		if group[0].CappedAmountMinor != 900 || group[1].CappedAmountMinor != 0 {
			t.Fatalf("封顶 900 必须只给第一行：%+v", group)
		}
	})
}

func TestMetricSourceDetailContract(t *testing.T) {
	rule := metricRule()
	d := &metricDraft{SourceDetail: "调用方给的摘要"}
	// 调用方给了就用调用方的（网关侧可能有更准确的口径描述）。
	if got := metricSourceDetail(d, rule, 100, false); got != "调用方给的摘要" {
		t.Fatalf("摘要优先级错误：%q", got)
	}
	got := metricSourceDetail(&metricDraft{}, rule, 100, true)
	if !strings.Contains(got, "R_VIP(v3)") || !strings.Contains(got, "min_quantity=10") {
		t.Fatalf("自动生成摘要必须能反推算法：%q", got)
	}
	long := metricSourceDetail(&metricDraft{SourceDetail: strings.Repeat("x", model.MaxSourceDetailBytes+50)}, rule, 1, false)
	if len(long) != model.MaxSourceDetailBytes {
		t.Fatalf("摘要未按列宽截断：%d", len(long))
	}
}
