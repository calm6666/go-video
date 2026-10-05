package logic

// 本文件锁 settlecompute.go 的出单口径：金额只从 capped_amount_minor 聚合、
// 幂等重放/在效单状态机/闸门判定/币种判定，以及「确认≠打款」在数据上的体现
// （新单写入即 state=DRAFT、payout_state=NOT_PAYABLE）。

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"go-video/services/creator-revenue/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// txPeriod 是出单事务内的普通账期（不参与「不得晚于当前周期」的校验，
// 因此可取固定值，便于读断言）；入口用例要过该校验的，用 model.CurrentPeriod()。
const (
	txPeriod   = "202601"
	txMid      = int64(100)
	txReqKey   = "req-1#202601#100"
	txOperator = "ops-77"
)

func mustErrIs(t *testing.T, err error, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("期望哨兵 %v，实际 %v", target, err)
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
}

// ruleWithCurrency 造一条生效规则行（币种决定结算单币种口径）。
func ruleWithCurrency(code string, sourceType int32, currency string) *model.RevenueRule {
	return &model.RevenueRule{
		RuleCode: code, SourceType: sourceType, Name: code, UnitPricePer1000: 1000,
		Currency: currency, Unit: "minute", State: model.RuleStateActive, Version: 3,
	}
}

// ledgerSeed 是一行计量台账种子（出单阶段只读 capped_amount_minor）。
type ledgerSeed struct {
	Aid        int64
	SourceType int32
	Rule       string
	Quantity   int64
	Amount     int64
	Capped     int64
	Blocked    bool
}

func (db *fakeDB) addLedger(period string, mid int64, seeds ...ledgerSeed) {
	for _, s := range seeds {
		blocked := int32(0)
		if s.Blocked {
			blocked = 1
		}
		db.addMetric(&model.RevenueMetric{
			Period: period, Mid: mid, Aid: s.Aid, SourceType: s.SourceType,
			RuleCode: s.Rule, RuleVersion: 3, Quantity: s.Quantity, Unit: "minute",
			AmountMinor: s.Amount, CappedAmountMinor: s.Capped, ThresholdBlocked: blocked,
			SourceDetail: "seed",
		})
	}
}

func newSettleQuery(overrides ...func(*settleQuery)) *settleQuery {
	q := &settleQuery{
		Period: txPeriod, Mid: txMid, RequestKey: txReqKey,
		Operator: txOperator, Reason: "月度出单复核", DefaultCurrency: "CNY",
	}
	for _, o := range overrides {
		o(q)
	}
	return q
}

// seedEnrolled 放一条 ENROLLED 参与关系。
func seedEnrolled(db *fakeDB, mid int64, state int32) *model.Enrollment {
	return db.addEnrollment(&model.Enrollment{
		Mid: mid, State: state, AgreedRuleVersion: 3, EnrolledAt: fakeNow() - 86400*30,
		Operator: selfOperator,
	})
}

// ---------------------------------------------------------------- addAmount

func TestAddAmountRejectsNegativeAndOverflow(t *testing.T) {
	got, err := addAmount(1000, 2000, "amount")
	mustNoErr(t, err)
	if got != 3000 {
		t.Fatalf("正常累加应为 3000，实际 %d", got)
	}
	// 负金额只能是绕过写入口改坏的台账：必须报错，不能悄悄抵扣别行应计。
	if _, err := addAmount(1000, -1, "amount"); !errors.Is(err, model.ErrAmountOverflow) {
		t.Fatalf("负金额应回 ErrAmountOverflow，实际 %v", err)
	}
	if _, err := addAmount(math.MaxInt64, 1, "amount"); !errors.Is(err, model.ErrAmountOverflow) {
		t.Fatalf("溢出应回 ErrAmountOverflow，实际 %v", err)
	}
	// 边界：刚好到 MaxInt64 不算溢出（判定用的是 sum > Max-v）。
	if _, err := addAmount(math.MaxInt64-1, 1, "amount"); err != nil {
		t.Fatalf("MaxInt64 边界不该报错：%v", err)
	}
}

// ---------------------------------------------------------------- aggregateLedger

func TestAggregateLedgerSumsCappedOnlyAndCapDeductExcludesBlocked(t *testing.T) {
	_, db := newTestSvc(t)
	db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
	db.addRule(ruleWithCurrency("R2", model.SourceTypeVipWatch, "CNY"))
	db.addRule(ruleWithCurrency("R3", model.SourceTypeCoin, "CNY"))
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 11, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9000, Amount: 1000, Capped: 800},
		ledgerSeed{Aid: 12, SourceType: model.SourceTypeVipWatch, Rule: "R2", Quantity: 500, Amount: 500, Capped: 500},
		// 被门槛拦掉的行：amount 保留「本该多少」，capped=0 且不进 cap 扣减。
		ledgerSeed{Aid: 13, SourceType: model.SourceTypeCoin, Rule: "R3", Quantity: 1, Amount: 300, Capped: 0, Blocked: true},
	)

	agg, err := aggregateLedger(context.Background(), txOf(db), newSettleQuery())
	mustNoErr(t, err)
	if agg.amountMinor != 1300 {
		t.Fatalf("应计合计只取 capped：期望 1300，实际 %d", agg.amountMinor)
	}
	if agg.capAppliedMinor != 200 {
		t.Fatalf("cap 扣减只累计未拦门槛行（1000-800）：期望 200，实际 %d", agg.capAppliedMinor)
	}
	if agg.metricCount != 3 {
		t.Fatalf("metric_count 统计全部台账行，期望 3，实际 %d", agg.metricCount)
	}
	if agg.currency != "CNY" {
		t.Fatalf("币种应取规则声明的 CNY，实际 %q", agg.currency)
	}
	if len(agg.items) != 2 {
		t.Fatalf("分项按来源聚合，期望 2 行，实际 %d", len(agg.items))
	}
	if agg.items[0].SourceType != model.SourceTypeVipWatch || agg.items[0].AmountMinor != 1300 ||
		agg.items[0].Quantity != 9500 {
		t.Fatalf("会员观看分项错误：%+v", agg.items[0])
	}
	// 主规则取「本来源封顶后应计最高」那条（800 > 500）。
	if agg.items[0].RuleCode != "R1" {
		t.Fatalf("主规则应为 R1，实际 %q", agg.items[0].RuleCode)
	}
	if agg.items[1].SourceType != model.SourceTypeCoin || agg.items[1].AmountMinor != 0 {
		t.Fatalf("投币分项应为 0 应计：%+v", agg.items[1])
	}
}

func TestAggregateLedgerCappedGreaterThanAmountIsRejected(t *testing.T) {
	_, db := newTestSvc(t)
	db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 11, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9, Amount: 100, Capped: 101},
	)
	// capped > amount 只能是台账被绕过写入口改坏，绝不能进结算。
	_, err := aggregateLedger(context.Background(), txOf(db), newSettleQuery())
	mustErrIs(t, err, model.ErrAmountOverflow)
	if !strings.Contains(err.Error(), "capped=101") {
		t.Fatalf("错误里要能定位到坏行，实际：%v", err)
	}
}

func TestAggregateLedgerDeterministicMainRuleOnTie(t *testing.T) {
	_, db := newTestSvc(t)
	db.addRule(ruleWithCurrency("RB", model.SourceTypeVipWatch, "CNY"))
	db.addRule(ruleWithCurrency("RA", model.SourceTypeVipWatch, "CNY"))
	// 并列时取 rule_code 字典序小者：插入顺序相反也必须选 RA（重算两次得到同一分项说明）。
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "RB", Quantity: 5, Amount: 500, Capped: 500},
		ledgerSeed{Aid: 2, SourceType: model.SourceTypeVipWatch, Rule: "RA", Quantity: 5, Amount: 500, Capped: 500},
	)
	agg, err := aggregateLedger(context.Background(), txOf(db), newSettleQuery())
	mustNoErr(t, err)
	if agg.items[0].RuleCode != "RA" {
		t.Fatalf("并列主规则应为 RA，实际 %q", agg.items[0].RuleCode)
	}
}

// 现状记录（不是期望行为）：整组台账都被门槛拦掉（或单价为 0）时，
// 组内没有任何一行 capped > 0，主规则挑选条件 `capped > mainValue` 永不成立，
// aggregateLedger 交出来的分项 RuleCode 是空串，fillItemNo 随即判为「台账脏数据」。
// 见交付报告：正常业务下的「整月未达门槛」会走到这条路径。这里只锁两个各自成立的口径。
func TestAggregateLedgerAllBlockedGroupYieldsEmptyMainRule(t *testing.T) {
	_, db := newTestSvc(t)
	db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 1, Amount: 100, Capped: 0, Blocked: true},
	)
	agg, err := aggregateLedger(context.Background(), txOf(db), newSettleQuery())
	mustNoErr(t, err)
	if agg.items[0].RuleCode != "" {
		t.Fatalf("当前实现下主规则为空，实际 %q", agg.items[0].RuleCode)
	}
	if err := fillItemNo(agg.items, "CRS202601-100-1"); !errors.Is(err, model.ErrMetricNotFound) {
		t.Fatalf("空主规则必须被 fillItemNo 拒掉，实际 %v", err)
	}
}

func TestAggregateLedgerCurrencyRules(t *testing.T) {
	t.Run("跨币种台账禁止合并成一单", func(t *testing.T) {
		_, db := newTestSvc(t)
		db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
		db.addRule(ruleWithCurrency("R2", model.SourceTypeCoin, "USD"))
		db.addLedger(txPeriod, txMid,
			ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9, Amount: 9, Capped: 9},
			ledgerSeed{Aid: 2, SourceType: model.SourceTypeCoin, Rule: "R2", Quantity: 9, Amount: 9, Capped: 9},
		)
		_, err := aggregateLedger(context.Background(), txOf(db), newSettleQuery())
		mustErrIs(t, err, model.ErrRuleCurrencyMismatch)
		if !strings.Contains(err.Error(), "[CNY USD]") {
			t.Fatalf("错误里要列出冲突币种，实际 %v", err)
		}
	})
	t.Run("规则行缺失必须报错而不是回落到配置币种", func(t *testing.T) {
		_, db := newTestSvc(t)
		db.addLedger(txPeriod, txMid,
			ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "R_missing", Quantity: 9, Amount: 9, Capped: 9},
		)
		_, err := aggregateLedger(context.Background(), txOf(db), newSettleQuery())
		mustErrIs(t, err, model.ErrRuleNotFound)
	})
	t.Run("规则未声明币种时用配置默认值兜底", func(t *testing.T) {
		_, db := newTestSvc(t)
		db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, ""))
		db.addLedger(txPeriod, txMid,
			ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9, Amount: 9, Capped: 9},
		)
		agg, err := aggregateLedger(context.Background(), txOf(db), newSettleQuery())
		mustNoErr(t, err)
		if agg.currency != "CNY" {
			t.Fatalf("默认币种未生效：%q", agg.currency)
		}
	})
	t.Run("没有台账就不出单", func(t *testing.T) {
		_, db := newTestSvc(t)
		_, err := aggregateLedger(context.Background(), txOf(db), newSettleQuery())
		mustErrIs(t, err, model.ErrNoMetricsToSettle)
	})
}

// ---------------------------------------------------------------- fillItemNo / settlementRow

func TestFillItemNoStampsSettlementNoAndRejectsBlankRule(t *testing.T) {
	rows := []*model.SettlementItem{
		{SourceType: model.SourceTypeVipWatch, RuleCode: "R1", Quantity: 1, AmountMinor: 2},
	}
	mustNoErr(t, fillItemNo(rows, "CRS202601-100-7"))
	if rows[0].SettlementNo != "CRS202601-100-7" {
		t.Fatalf("分项未挂单号：%+v", rows[0])
	}
	if err := fillItemNo([]*model.SettlementItem{{SourceType: 1, RuleCode: "  "}}, "X"); !errors.Is(err, model.ErrMetricNotFound) {
		t.Fatalf("空白主规则必须拒绝，实际 %v", err)
	}
}

// TestSettlementRowNeverPayable 锁 AGENTS.md §1 的范围边界：
// 出单产出的每一行台账在数据上就必须写着「这笔钱不可出金」。
func TestSettlementRowNeverPayable(t *testing.T) {
	agg := &settlementAgg{amountMinor: 1300, capAppliedMinor: 200, metricCount: 3, currency: "CNY"}
	row := settlementRow("CRS202601-100-1", newSettleQuery(), agg)
	if row.State != model.SettlementStateDraft {
		t.Fatalf("新单必须是 DRAFT，实际 %d", row.State)
	}
	if row.PayoutState != model.PayoutStateNotPayable {
		t.Fatalf("payout_state 写入即 NOT_PAYABLE，实际 %d", row.PayoutState)
	}
	if row.RequestId != txReqKey {
		t.Fatalf("行级幂等键未落行：%q", row.RequestId)
	}
	if row.VoidSeq != 0 {
		t.Fatalf("在效槽位必须是 0，实际 %d", row.VoidSeq)
	}
	// 确认人/确认时间必须为空：应计口径冻结过与否由 state 表达，不由本方法伪造。
	if row.ConfirmedBy != "" || row.ConfirmedAt != 0 {
		t.Fatalf("出单不得写确认信息：%+v", row)
	}
}

// ---------------------------------------------------------------- applySettlementInTx 状态机

func TestApplySettlementCreatesDraftWithItems(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 11, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9000, Amount: 1000, Capped: 800},
	)

	row, action, err := applySettlementInTx(context.Background(), txOf(db), newSettleQuery())
	mustNoErr(t, err)
	if action != settleCreated || !action.countsAsGenerated() {
		t.Fatalf("首单必须是 settleCreated，实际 %d", action)
	}
	if row.SettlementNo != "CRS202601-100-1" || row.AmountMinor != 800 || row.Currency != "CNY" {
		t.Fatalf("回读结论错误：%+v", row)
	}
	if row.PayoutState != model.PayoutStateNotPayable || row.State != model.SettlementStateDraft {
		t.Fatalf("新单状态/出金态错误：%+v", row)
	}
	items := db.itemsOf(row.SettlementNo)
	if len(items) != 1 || items[0].RuleCode != "R1" || items[0].AmountMinor != 800 {
		t.Fatalf("分项必须与主体同事务落库：%+v", items)
	}
}

func TestApplySettlementReplayReturnsFirstRowWithoutWriting(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 11, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9, Amount: 9, Capped: 9},
	)
	// 首单已被后续请求作废：同一 request_id 重放仍要回那张原单，而不是重新出一张。
	first := db.addSettlement(&model.Settlement{
		SettlementNo: "CRS202601-100-9", Period: txPeriod, Mid: txMid, AmountMinor: 123,
		Currency: "CNY", MetricCount: 1, State: model.SettlementStateVoided,
		PayoutState: model.PayoutStateNotPayable, RequestId: txReqKey, VoidSeq: 9,
	})

	before := len(db.calls)
	row, action, err := applySettlementInTx(context.Background(), txOf(db), newSettleQuery())
	mustNoErr(t, err)
	if action != settleReplayed || action.countsAsGenerated() {
		t.Fatalf("命中幂等键必须判重放且不计入 generated，实际 %d", action)
	}
	if row.SettlementNo != first.SettlementNo || row.AmountMinor != 123 {
		t.Fatalf("重放必须回首次那张单：%+v", row)
	}
	if len(db.settlements) != 1 {
		t.Fatalf("重放不得新增结算单，当前 %d 行", len(db.settlements))
	}
	if got := len(db.calls) - before; got > 2 {
		t.Fatalf("重放只应做定位读，实际执行了 %d 条语句：%v", got, db.calls[before:])
	}
}

func TestApplySettlementRecomputesDraftInPlaceAndReplacesItems(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 11, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9000, Amount: 1000, Capped: 800},
	)
	db.addSettlement(&model.Settlement{
		SettlementNo: "CRS202601-100-1", Period: txPeriod, Mid: txMid, AmountMinor: 1,
		CapAppliedMinor: 1, Currency: "CNY", MetricCount: 9, State: model.SettlementStateDraft,
		PayoutState: model.PayoutStateNotPayable, RequestId: "old-req", VoidSeq: 0,
	})
	// 上一轮留下的分项：重算后某来源可能整组归零，必须先清后写，
	// 否则运营会看到一笔「已经不存在的钱」。
	db.items = append(db.items, &model.SettlementItem{
		ItemId: 1, SettlementNo: "CRS202601-100-1", SourceType: model.SourceTypeActivity,
		RuleCode: "R_OLD", Quantity: 7, AmountMinor: 777,
	})

	row, action, err := applySettlementInTx(context.Background(), txOf(db), newSettleQuery())
	mustNoErr(t, err)
	if action != settleRecomputed || !action.countsAsGenerated() {
		t.Fatalf("DRAFT 在效单应就地重算，实际 %d", action)
	}
	if row.SettlementNo != "CRS202601-100-1" {
		t.Fatalf("重算不得换单号，实际 %q", row.SettlementNo)
	}
	if row.AmountMinor != 800 || row.CapAppliedMinor != 200 || row.MetricCount != 1 || row.RequestId != txReqKey {
		t.Fatalf("重算结果未覆盖：%+v", row)
	}
	items := db.itemsOf(row.SettlementNo)
	if len(items) != 1 || items[0].RuleCode != "R1" {
		t.Fatalf("旧分项必须被清掉：%+v", items)
	}
}

func TestApplySettlementConfirmedNeedsForceAndVoidLeavesAuditTrail(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 11, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9000, Amount: 1000, Capped: 800},
	)
	confirmed := db.addSettlement(&model.Settlement{
		SettlementNo: "CRS202601-100-1", Period: txPeriod, Mid: txMid, AmountMinor: 66000,
		Currency: "CNY", MetricCount: 1, State: model.SettlementStateConfirmed,
		PayoutState: model.PayoutStateNotPayable, ConfirmedBy: "ops-01", ConfirmedAt: fakeNow() - 10,
		RequestId: "first-req", VoidSeq: 0,
	})

	// 不带危险位：零写入，只回 duplicated（已确认金额是对外承诺过的口径）。
	row, action, err := applySettlementInTx(context.Background(), txOf(db), newSettleQuery())
	mustNoErr(t, err)
	if action != settleDuplicated || action.countsAsGenerated() {
		t.Fatalf("已确认单未强制时必须判 duplicated，实际 %d", action)
	}
	if row.AmountMinor != 66000 || row.State != model.SettlementStateConfirmed {
		t.Fatalf("duplicated 必须原样回读在效单：%+v", row)
	}
	if len(db.settlements) != 1 || len(db.items) != 0 {
		t.Fatalf("duplicated 路径一个字都不该写：%d/%d", len(db.settlements), len(db.items))
	}

	// 带危险位：旧单置 VOIDED（原因留痕、槽位释放）+ 另起新单号。
	q := newSettleQuery(func(sq *settleQuery) { sq.Force = true; sq.Reason = "台账更正后重算" })
	row2, action2, err := applySettlementInTx(context.Background(), txOf(db), q)
	mustNoErr(t, err)
	if action2 != settleVoidedAndCreated || !action2.countsAsGenerated() {
		t.Fatalf("强制重算应判 voidedAndCreated，实际 %d", action2)
	}
	if row2.SettlementNo != "CRS202601-100-2" {
		t.Fatalf("强制重算必须另起单号，实际 %q", row2.SettlementNo)
	}
	if row2.PayoutState != model.PayoutStateNotPayable || row2.State != model.SettlementStateDraft {
		t.Fatalf("新单仍是「未确认、不可出金」：%+v", row2)
	}
	old := db.settlementByNo(confirmed.SettlementNo)
	if old.State != model.SettlementStateVoided || old.VoidReason != "台账更正后重算" || old.VoidSeq != old.SettlementId {
		t.Fatalf("旧确认单未留作废证据：%+v", old)
	}
	// 「确认 ≠ 打款」：作废重算也不得让旧单或新单进入任何已出金状态。
	if old.PayoutState != model.PayoutStateNotPayable {
		t.Fatalf("payout_state 被改写：%d", old.PayoutState)
	}
}

func TestApplySettlementCasMissIsRetryable(t *testing.T) {
	t.Run("作废未命中", func(t *testing.T) {
		_, db := newTestSvc(t)
		seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
		db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
		db.addLedger(txPeriod, txMid,
			ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9, Amount: 9, Capped: 9},
		)
		db.addSettlement(&model.Settlement{
			SettlementNo: "CRS202601-100-1", Period: txPeriod, Mid: txMid, AmountMinor: 1,
			Currency: "CNY", MetricCount: 1, State: model.SettlementStateConfirmed,
			PayoutState: model.PayoutStateNotPayable, RequestId: "first", VoidSeq: 0,
		})
		db.noRowsFor["upd:cr_settlement.void"] = true // 条件不命中 → 0 行
		_, _, err := applySettlementInTx(context.Background(), txOf(db),
			newSettleQuery(func(sq *settleQuery) { sq.Force = true }))
		mustErrIs(t, err, model.ErrConcurrentUpdate)
	})
	t.Run("DRAFT 重算未命中", func(t *testing.T) {
		_, db := newTestSvc(t)
		seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
		db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
		db.addLedger(txPeriod, txMid,
			ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9, Amount: 9, Capped: 9},
		)
		db.addSettlement(&model.Settlement{
			SettlementNo: "CRS202601-100-1", Period: txPeriod, Mid: txMid, AmountMinor: 1,
			Currency: "CNY", MetricCount: 1, State: model.SettlementStateDraft,
			PayoutState: model.PayoutStateNotPayable, RequestId: "first", VoidSeq: 0,
		})
		db.noRowsFor["upd:cr_settlement.draftAmounts"] = true
		_, _, err := applySettlementInTx(context.Background(), txOf(db), newSettleQuery())
		mustErrIs(t, err, model.ErrSettlementNotDraft)
	})
}

func TestApplySettlementEnrollmentGates(t *testing.T) {
	cases := []struct {
		name   string
		seed   bool
		state  int32
		target error
	}{
		{"从未参加不出单", false, 0, model.ErrEnrollmentNotFound},
		{"退出后不再计量", true, model.EnrollmentStateLeft, model.ErrNotEnrolled},
		{"暂停期间不出单", true, model.EnrollmentStateSuspended, model.ErrEnrollmentSuspended},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, db := newTestSvc(t)
			if c.seed {
				seedEnrolled(db, txMid, c.state)
			}
			db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
			db.addLedger(txPeriod, txMid,
				ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9, Amount: 9, Capped: 9},
			)
			_, _, err := applySettlementInTx(context.Background(), txOf(db), newSettleQuery())
			mustErrIs(t, err, c.target)
			if len(db.settlements) != 0 {
				t.Fatalf("闸门未过却写了结算单：%+v", db.settlements)
			}
		})
	}
}

func TestApplySettlementRequiresDeterminableCurrency(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, ""))
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9, Amount: 9, Capped: 9},
	)
	// 规则没写币种且 DefaultCurrency 漏配：不能凭空挑一个币种发单号。
	_, _, err := applySettlementInTx(context.Background(), txOf(db),
		newSettleQuery(func(sq *settleQuery) { sq.DefaultCurrency = "  " }))
	mustErrIs(t, err, model.ErrRuleCurrencyMismatch)
}

func TestApplySettlementRollbackLeavesNoHalfSettlement(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY"))
	db.addLedger(txPeriod, txMid,
		ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "R1", Quantity: 9, Amount: 9, Capped: 9},
	)
	// 预置一行会撞 uniq_no_source 的分项：主体与分项同事务，分项写失败时主体必须一起回滚，
	// 否则库里留下一张「运营看得到总额、看不到钱从哪来」的单。
	db.items = append(db.items, &model.SettlementItem{
		ItemId: 1, SettlementNo: "CRS202601-100-1", SourceType: model.SourceTypeVipWatch,
		RuleCode: "R_OLD", Quantity: 1, AmountMinor: 1,
	})
	err := fakeConn{db: db}.TransactCtx(context.Background(),
		func(ctx context.Context, tx sqlx.Session) error {
			_, _, err := applySettlementInTx(ctx, tx, newSettleQuery())
			return err
		})
	if err == nil {
		t.Fatal("分项冲突必须让整次出单失败")
	}
	if !model.IsDuplicateErr(err) {
		t.Fatalf("应回唯一键冲突，实际 %v", err)
	}
	if len(db.settlements) != 0 {
		t.Fatalf("回滚后不得留下结算单：%+v", db.settlements)
	}
	if len(db.items) != 1 || db.items[0].RuleCode != "R_OLD" {
		t.Fatalf("回滚不该动预置分项：%+v", db.items)
	}
}
