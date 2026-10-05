package logic

// 本文件锁 RecordRevenueMetric（计量写入）这个构造器的入口口径。
// 事务内函数（applyMetricInTx / reallocGroupCap）与 validateMetricUpsert 的分支
// 已由 metricmutate_test.go 覆盖，这里补的是「只有从入口进来才看得到」的四件事：
//   - 闸门必须开在事务之外：入参不合法时**一条语句都不该执行**（不烧事务、不抢行锁）。
//     这条只能用 db.calls 从 0 开始断言，validateMetricUpsert 的单测判不出来；
//   - 事务内的完整语句序列（含「失败就停在第几张表」）：这是本服务唯一的应计写入路径，
//     序列里多一条读 = 多抢一次行锁，少一条写 = 漏记账；
//   - 幂等与留痕用「调用前取基线 → 调用后取差值」断言，不看全局累加值；
//   - 出金边界（AGENTS.md §1）：计量写入绝不碰 cr_settlement 的任何写语句，
//     也不推进任何 payout_state。
// 现状缺陷原样钉住 + README 登记（计量域）：
//   - cr_metric 没有 request_id 列，同值重放路径又什么都不写，于是「谁在什么时候报过
//     同一个数」在库里完全不留痕（recordrevenuemetriclogic.go:42 声称已记 README）；
//   - 对外投影里没有 threshold_blocked（helpers.go:185 的 metricInfo、
//     proto RevenueMetricInfo 都没这一列），调用方只能从 capped==0 反推
//     「这条是被门槛拦了，还是被月度封顶扣光了」。

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"
)

// metricEntryPeriod 取「当前周期」：入口会拒未来周期，写死常量会让本文件跟着时钟漂移。
var metricEntryPeriod = model.CurrentPeriod()

func runRecord(t *testing.T, ctx *svc.ServiceContext, in *rpc.RecordRevenueMetricReq) (
	*rpc.RecordRevenueMetricReply, error,
) {
	t.Helper()
	return NewRecordRevenueMetricLogic(context.Background(), ctx).RecordRevenueMetric(in)
}

func metricInput(mut func(*rpc.RecordRevenueMetricReq)) *rpc.RecordRevenueMetricReq {
	in := &rpc.RecordRevenueMetricReq{
		Period: metricEntryPeriod, Mid: txMid, Aid: 11,
		SourceType: rpc.RevenueSourceType(model.SourceTypeVipWatch),
		RuleCode:   "R_VIP", Quantity: 120, Operator: "cron", RequestId: "req-entry-1",
	}
	if mut != nil {
		mut(in)
	}
	return in
}

// priceRule 造一条「除不尽」的规则：单价 1234 分/千单位，门槛 10，月度封顶 5000。
// 单价 1000 会整除，判不出取整方向，所以金额口径的用例一律用它。
func priceRule(mut func(*model.RevenueRule)) *model.RevenueRule {
	r := &model.RevenueRule{
		RuleCode: "R_VIP", SourceType: model.SourceTypeVipWatch, Name: "会员有效观看",
		UnitPricePer1000: 1234, Currency: "CNY", Unit: "minute", MinQuantity: 10,
		MonthlyCapMinor: 5000, State: model.RuleStateActive, Version: 3,
	}
	if mut != nil {
		mut(r)
	}
	return r
}

// readyMetric 装配「可计量」的最小环境：ENROLLED + ACTIVE 规则。
func readyMetric(t *testing.T) (*svc.ServiceContext, *fakeDB) {
	t.Helper()
	ctx, db := newTestSvc(t)
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addRule(priceRule(nil))
	return ctx, db
}

// mustCallsFrom 断言「第 from 条语句之后」的完整执行序列（逐项等长等值）。
// 只比前缀会漏掉「多写了一张表 / 多抢了一次行锁」这类越界访问。
func mustCallsFrom(t *testing.T, db *fakeDB, from int, want ...string) {
	t.Helper()
	got := db.calls[from:]
	if len(got) != len(want) {
		t.Fatalf("语句序列长度不符：期望 %d 条 %v，实际 %d 条 %v", len(want), want, len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条语句不符：期望 %s，实际 %s（完整序列 %v）", i, want[i], got[i], got)
		}
	}
}

// metricRowOf 取库内某一行的副本（只读视图，不参与任何断言的副作用计数）。
func metricRowOf(db *fakeDB, aid int64) *model.RevenueMetric {
	for _, m := range db.metrics {
		if m.Aid == aid && m.Period == metricEntryPeriod && m.Mid == txMid {
			c := *m
			return &c
		}
	}
	return nil
}

// ---------------------------------------------------------------- 入口闸门：不得开事务

// Ready 闸门在最前（ctx 未装配时必须回可诊断错误，而不是 nil panic）。
func TestRecordRevenueMetricReadyGate(t *testing.T) {
	ctx, db := readyMetric(t)
	ctx.Settlements = nil
	reply, err := runRecord(t, ctx, metricInput(nil))
	mustErrIs(t, err, model.ErrDBNotConfigured)
	if reply != nil {
		t.Fatalf("未配置库不得回结论：%+v", reply)
	}
	if len(db.calls) != 0 {
		t.Fatalf("Ready 闸门不该执行语句：%v", db.calls)
	}
}

// 入口级断言：入参闸门未过时**一条语句都不执行**（既没开事务，也没抢任何行锁）。
// 只断言错误码判不出「先开事务再校验」这种实现——那会白烧一次事务并在 cr_enrollment 上留锁。
func TestRecordRevenueMetricGatesOpenNoTransaction(t *testing.T) {
	cases := []struct {
		name   string
		in     *rpc.RecordRevenueMetricReq
		target error
		want   string
	}{
		{"nil 请求", nil, model.ErrInvalidPeriod, "请求为空"},
		{"未来周期", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Period = "299912" }),
			model.ErrFuturePeriod, "当前周期"},
		{"周期格式非法", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Period = "2026-1" }),
			model.ErrInvalidPeriod, "不是 6 位数字"},
		{"mid 必须真实（0 也不放行）", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Mid = 0 }),
			model.ErrInvalidMid, "mid=0"},
		{"mid 负数", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Mid = -1 }),
			model.ErrInvalidMid, "mid=-1"},
		{"aid 负数拒绝", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Aid = -1 }),
			model.ErrInvalidAid, "0 表示不挂具体内容"},
		{"来源必须是真实枚举", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.SourceType = rpc.RevenueSourceType(model.SourceTypeUnspecified)
		}), model.ErrInvalidSourceType, ""},
		{"规则编码必填", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.RuleCode = "  " }),
			model.ErrRuleCodeRequired, ""},
		{"数量负数拒绝", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = -1 }),
			model.ErrNegativeQuantity, "quantity=-1"},
		{"操作人必填", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Operator = "" }),
			model.ErrOperatorRequired, ""},
		{"幂等键必填", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.RequestId = "" }),
			model.ErrRequestIDRequired, ""},
		{"运营工号必须留 reason", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.Operator = txOperator
		}), model.ErrReasonRequired, "必须留 reason"},
		{"活动激励无论谁调都要 reason", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.SourceType = rpc.RevenueSourceType(model.SourceTypeActivity)
		}), model.ErrReasonRequired, ""},
		{"规则编码超列宽", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.RuleCode = strings.Repeat("R", model.MaxRuleCodeBytes+1)
		}), model.ErrTextTooLong, "rule_code"},
		{"摘要超列宽", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.SourceDetail = strings.Repeat("d", model.MaxSourceDetailBytes+1)
		}), model.ErrTextTooLong, "source_detail"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := readyMetric(t)
			reply, err := runRecord(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			if len(db.calls) != 0 {
				t.Fatalf("闸门未过却已开事务/执行语句：%v", db.calls)
			}
			if len(db.reads) != 0 {
				t.Fatalf("闸门未过却已读库：%v", db.reads)
			}
			if len(db.metrics) != 0 {
				t.Fatalf("闸门未过却已落台账：%+v", db.metrics)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里缺少 %q：%v", c.want, err)
			}
		})
	}
}

// aid=0（运营活动激励不挂具体内容）与「当前周期本身」都必须放行：
// 拒在它们前面是「宁可少一寸」的过度收紧，会把合法的月度内回填判成非法请求。
func TestRecordRevenueMetricLegalBoundaryInputsAreAccepted(t *testing.T) {
	ctx, db := readyMetric(t)
	reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) {
		i.Aid = 0
		i.Quantity = 100
		i.Period = model.CurrentPeriod()
	}))
	mustNoErr(t, err)
	if !reply.Created {
		t.Fatalf("合法边界值被判成重放：%+v", reply)
	}
	row := metricRowOf(db, 0)
	if row == nil || row.Aid != 0 || row.Quantity != 100 || row.AmountMinor != 123 {
		t.Fatalf("aid=0 的台账没按入参落库：%+v", row)
	}
	if len(db.metrics) != 1 {
		t.Fatalf("台账行数错误：%+v", db.metrics)
	}
}

// 闸门先后顺序必须可判别：period → mid → aid → source_type → rule_code → quantity。
func TestRecordRevenueMetricGuardOrderIsDiscriminable(t *testing.T) {
	cases := []struct {
		name   string
		in     *rpc.RecordRevenueMetricReq
		target error
	}{
		{"周期先于 mid", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.Period, i.Mid = "20261", 0
		}), model.ErrInvalidPeriod},
		{"未来周期先于 mid", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.Period, i.Mid = "299912", 0
		}), model.ErrFuturePeriod},
		{"mid 先于 aid", metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Mid, i.Aid = -1, -1 }),
			model.ErrInvalidMid},
		{"aid 先于 source_type", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.Aid, i.SourceType = -1, rpc.RevenueSourceType(model.SourceTypeUnspecified)
		}), model.ErrInvalidAid},
		{"source_type 先于 rule_code", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.SourceType, i.RuleCode = rpc.RevenueSourceType(model.SourceTypeUnspecified), ""
		}), model.ErrInvalidSourceType},
		{"rule_code 先于 quantity", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.RuleCode, i.Quantity = "  ", -1
		}), model.ErrRuleCodeRequired},
		{"quantity 先于 operator", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.Quantity, i.Operator = -1, ""
		}), model.ErrNegativeQuantity},
		{"operator 先于 request_id", metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.Operator, i.RequestId = "", ""
		}), model.ErrOperatorRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := readyMetric(t)
			_, err := runRecord(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if len(db.calls) != 0 {
				t.Fatalf("闸门未过却执行了语句：%v", db.calls)
			}
		})
	}
}

// ---------------------------------------------------------------- 事务内语句序列

// 首写的完整序列：参与闸门 → 规则闸门 → 结算冻结闸门 → 台账行锁 → 落行 → 组内封顶重分配 → 回读。
func TestRecordRevenueMetricCreateStatementSequence(t *testing.T) {
	ctx, db := readyMetric(t)
	reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 1000 }))
	mustNoErr(t, err)
	mustCallsFrom(t, db, 0,
		"sel:cr_enrollment.lockByMid",
		"sel:cr_revenue_rule.byCodeLock",
		"sel:cr_settlement.active",
		"sel:cr_metric.byKeyLock",
		"ins:cr_metric",
		"sel:cr_metric.groupLock",
		"sel:cr_metric.byKey",
	)
	if !reply.Created || reply.Corrected {
		t.Fatalf("首写结论错误：created=%v corrected=%v", reply.Created, reply.Corrected)
	}
}

func TestRecordRevenueMetricReplyMatchesStoredRow(t *testing.T) {
	ctx, db := readyMetric(t)
	reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 1000 }))
	mustNoErr(t, err)
	if !reply.Created || reply.Corrected {
		t.Fatalf("首写两位结论错误：%+v", reply)
	}
	row := metricRowOf(db, 11)
	if row == nil {
		t.Fatal("台账没落库")
	}
	m := reply.Metric
	if m == nil {
		t.Fatal("没回台账投影")
	}
	// 回的是**库内回读值**（metricmutate.go:286 的 FindByKey），不是内存里算出的草稿：
	// 封顶重分配发生在回读之前，所以 capped 必须是重分配之后的数。
	if m.MetricId != row.MetricId || m.Period != row.Period || m.Mid != row.Mid || m.Aid != row.Aid {
		t.Fatalf("投影与行不符：%+v vs %+v", m, row)
	}
	if m.AmountMinor != row.AmountMinor || m.CappedAmountMinor != row.CappedAmountMinor {
		t.Fatalf("金额与库内不一致：reply %d/%d row %d/%d",
			m.AmountMinor, m.CappedAmountMinor, row.AmountMinor, row.CappedAmountMinor)
	}
	if m.Quantity != 1000 || m.Unit != "minute" || m.RuleCode != "R_VIP" || m.RuleVersion != 3 {
		t.Fatalf("台账必须冻结规则版本快照：%+v", m)
	}
	if int32(m.SourceType) != model.SourceTypeVipWatch {
		t.Fatalf("来源投影错误：%v", m.SourceType)
	}
	if row.Ctime <= 0 || row.Mtime <= 0 || m.Ctime != row.Ctime || m.Mtime != row.Mtime {
		t.Fatalf("时间戳没由库侧给出或投影丢了：row %d/%d reply %d/%d",
			row.Ctime, row.Mtime, m.Ctime, m.Mtime)
	}
	if !strings.Contains(m.SourceDetail, "R_VIP") || !strings.Contains(m.SourceDetail, "1234") {
		t.Fatalf("计算依据摘要必须能反推算法（含规则码与单价）：%q", m.SourceDetail)
	}
	// 首写不进更正台账：否则面板上会出现一条 old==new 的噪音行。
	if len(db.metricLogs) != 0 {
		t.Fatalf("首写不该留痕：%+v", db.metricLogs)
	}
}

// 事务内闸门的失败点必须停在被拒的那张表上：后面的表一次都不该锁。
func TestRecordRevenueMetricTxGateStopsAtFailedStep(t *testing.T) {
	cases := []struct {
		name   string
		seed   func(db *fakeDB)
		target error
		want   []string
	}{
		{"从未参加计划不产生台账", func(db *fakeDB) { db.enrollments = nil },
			model.ErrEnrollmentNotFound, []string{"sel:cr_enrollment.lockByMid"}},
		{"暂停期间不计量", func(db *fakeDB) {
			db.enrollments = nil
			seedEnrolled(db, txMid, model.EnrollmentStateSuspended)
		}, model.ErrEnrollmentSuspended, []string{"sel:cr_enrollment.lockByMid"}},
		{"退出后不再计量", func(db *fakeDB) {
			db.enrollments = nil
			seedEnrolled(db, txMid, model.EnrollmentStateLeft)
		}, model.ErrNotEnrolled, []string{"sel:cr_enrollment.lockByMid"}},
		{"规则不存在", func(db *fakeDB) { db.rules = nil },
			model.ErrRuleNotFound, []string{"sel:cr_enrollment.lockByMid", "sel:cr_revenue_rule.byCodeLock"}},
		{"必须按 ACTIVE 规则折算", func(db *fakeDB) {
			db.rules = nil
			db.addRule(priceRule(func(r *model.RevenueRule) { r.State = model.RuleStateDraft }))
		}, model.ErrRuleNotActive,
			[]string{"sel:cr_enrollment.lockByMid", "sel:cr_revenue_rule.byCodeLock"}},
		{"来源与规则声明不一致", func(db *fakeDB) {
			db.rules = nil
			db.addRule(priceRule(func(r *model.RevenueRule) { r.SourceType = model.SourceTypeCoin }))
		}, model.ErrRuleSourceMismatch,
			[]string{"sel:cr_enrollment.lockByMid", "sel:cr_revenue_rule.byCodeLock"}},
		{"周期早于规则生效起点", func(db *fakeDB) {
			db.rules = nil
			db.addRule(priceRule(func(r *model.RevenueRule) { r.EffectiveFrom = math.MaxInt32 }))
		}, model.ErrRuleNotEffective,
			[]string{"sel:cr_enrollment.lockByMid", "sel:cr_revenue_rule.byCodeLock"}},
		{"结算单已确认则台账冻结", func(db *fakeDB) {
			db.addSettlement(&model.Settlement{
				SettlementNo: "CRS-FROZEN", Period: metricEntryPeriod, Mid: txMid,
				AmountMinor: 424_200, Currency: "CNY", State: model.SettlementStateConfirmed,
				PayoutState: model.PayoutStateNotPayable, ConfirmedBy: txOperator, VoidSeq: 0,
			})
		}, model.ErrSettlementConfirmed, []string{
			"sel:cr_enrollment.lockByMid", "sel:cr_revenue_rule.byCodeLock", "sel:cr_settlement.active"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := readyMetric(t)
			c.seed(db)
			reply, err := runRecord(t, ctx, metricInput(nil))
			mustErrIs(t, err, c.target)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			mustCallsFrom(t, db, 0, c.want...)
			if len(db.metrics) != 0 || len(db.metricLogs) != 0 {
				t.Fatalf("闸门未过却写了台账/留痕：%+v %+v", db.metrics, db.metricLogs)
			}
		})
	}
}

// 已确认单冻结台账时，错误文案必须能指出「是哪张单、谁确认的」，
// 否则运营只能整周期猜（要改必须先作废重算）。
func TestRecordRevenueMetricFrozenErrorNamesTheConfirmedBill(t *testing.T) {
	ctx, db := readyMetric(t)
	db.addSettlement(&model.Settlement{
		SettlementNo: "CRS202609-100-1", Period: metricEntryPeriod, Mid: txMid,
		AmountMinor: 1, Currency: "CNY", State: model.SettlementStateConfirmed,
		PayoutState: model.PayoutStateNotPayable, ConfirmedBy: "ops-01", VoidSeq: 0,
	})
	_, err := runRecord(t, ctx, metricInput(nil))
	mustErrIs(t, err, model.ErrSettlementConfirmed)
	for _, frag := range []string{"CRS202609-100-1", "ops-01", "作废"} {
		if !strings.Contains(err.Error(), frag) {
			t.Fatalf("冻结原因里缺少 %q：%v", frag, err)
		}
	}
}

// ---------------------------------------------------------------- 金额口径

// 单位、取整方向、门槛边界、封顶扣减四组一起钉。
// 单价 1234 分/千单位刻意选成除不尽，整数除法的「向零取整」才有判别力。
func TestRecordRevenueMetricAmountBoundary(t *testing.T) {
	cases := []struct {
		name       string
		qty        int64
		wantAmount int64
		wantCapped int64
	}{
		{"0 数量：金额为 0，仍低于门槛", 0, 0, 0},
		{"门槛前一格：9 < min_quantity=10", 9, 11, 0},     // 9*1234=11106 → /1000 = 11
		{"门槛恰好等于：10 不算低于 min_quantity", 10, 12, 12}, // 10*1234=12340 → /1000 = 12
		{"除不尽向零取整", 999, 1232, 1232},                // 999*1234=1232766 → /1000 = 1232
		{"整千不放大", 1000, 1234, 1234},
		{"恰好等于月度封顶 5000", 4052, 5000, 5000}, // 4052*1234=5000168 → /1000 = 5000
		{"超封顶一格只扣差额", 4053, 5001, 5000},     // 4053*1234=5001402 → /1000 = 5001
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := readyMetric(t)
			reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) {
				i.Quantity = c.qty
			}))
			mustNoErr(t, err)
			row := metricRowOf(db, 11)
			if row == nil {
				t.Fatal("台账没落库")
			}
			if row.AmountMinor != c.wantAmount || row.CappedAmountMinor != c.wantCapped {
				t.Fatalf("金额口径错误：qty=%d 期望 amount/capped=%d/%d，实际 %d/%d",
					c.qty, c.wantAmount, c.wantCapped, row.AmountMinor, row.CappedAmountMinor)
			}
			if reply.Metric.AmountMinor != c.wantAmount || reply.Metric.CappedAmountMinor != c.wantCapped {
				t.Fatalf("回给调用方的金额与库内不符：%+v", reply.Metric)
			}
			wantBlocked := int32(0)
			if c.qty < 10 {
				wantBlocked = 1 // 低于门槛：capped 归零，但「本该多少」必须留在 amount_minor 里
			}
			if row.ThresholdBlocked != wantBlocked {
				t.Fatalf("threshold_blocked 位错误（期望 %d）：%+v", wantBlocked, row)
			}
		})
	}
}

// 现状哨兵（已登记 README 计量域）：threshold_blocked 不在对外投影里
// （helpers.go:185 的 metricInfo、proto RevenueMetricInfo 都没这一列），
// 调用方拿到 capped=0 时无法区分「低于门槛」与「被月度封顶扣光」。
func TestRecordRevenueMetricReplyCannotTellBlockedFromCapped(t *testing.T) {
	// A：低于门槛（qty 9）→ capped 0。
	ctxA, dbA := readyMetric(t)
	replyA, err := runRecord(t, ctxA, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 9 }))
	mustNoErr(t, err)

	// B：组内额度已被前一行占满（aid 11 先吃满 5000，aid 12 再写 100 分钟）→ capped 0。
	ctxB, dbB := readyMetric(t)
	_, err = runRecord(t, ctxB, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 4052 }))
	mustNoErr(t, err)
	replyB, err := runRecord(t, ctxB, metricInput(func(i *rpc.RecordRevenueMetricReq) {
		i.Aid = 12
		i.Quantity = 100
		i.RequestId = "req-entry-12"
	}))
	mustNoErr(t, err)

	if replyA.Metric.CappedAmountMinor != 0 || replyB.Metric.CappedAmountMinor != 0 {
		t.Fatalf("前置事实不成立（两行都该 capped=0）：%+v / %+v", replyA.Metric, replyB.Metric)
	}
	if metricRowOf(dbA, 11).ThresholdBlocked != 1 || metricRowOf(dbB, 12).ThresholdBlocked != 0 {
		t.Fatalf("库内两位本该不同（这正是现状的成因）：%+v / %+v",
			metricRowOf(dbA, 11), metricRowOf(dbB, 12))
	}
}

// 脏数据与溢出：折算失败必须显式报错并整体回滚，绝不回绕成负数应计。
func TestRecordRevenueMetricAmountFailuresRollBackCleanly(t *testing.T) {
	t.Run("数量溢出 int64", func(t *testing.T) {
		ctx, db := readyMetric(t)
		reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) {
			i.Quantity = math.MaxInt64
		}))
		mustErrIs(t, err, model.ErrAmountOverflow)
		if reply != nil {
			t.Fatalf("溢出不得回结论：%+v", reply)
		}
		// 折算发生在锁台账行之前：序列必须停在结算闸门那一步。
		mustCallsFrom(t, db, 0,
			"sel:cr_enrollment.lockByMid", "sel:cr_revenue_rule.byCodeLock", "sel:cr_settlement.active")
		if len(db.metrics) != 0 || len(db.metricLogs) != 0 {
			t.Fatalf("溢出路径不该留下任何行：%+v %+v", db.metrics, db.metricLogs)
		}
	})

	t.Run("库内脏单价（负数）", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
		db.addRule(priceRule(func(r *model.RevenueRule) { r.UnitPricePer1000 = -5 }))
		_, err := runRecord(t, ctx, metricInput(nil))
		mustErrIs(t, err, model.ErrNegativeUnitPrice)
		if len(db.metrics) != 0 {
			t.Fatalf("脏单价不得折出应计：%+v", db.metrics)
		}
	})
}

// ---------------------------------------------------------------- 幂等与更正

// 同值重放：两位结论都为 false，且**差值**上没有任何写语句、没有任何留痕。
// 换 request_id 重放也算重放（本表没有 request_id 列，判定只押在唯一键 + 同值上）。
func TestRecordRevenueMetricSameValueReplayWritesNothing(t *testing.T) {
	ctx, db := readyMetric(t)
	first, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 1200 }))
	mustNoErr(t, err)
	if !first.Created {
		t.Fatal("首写必须 created=true")
	}

	beforeCalls := len(db.calls)
	beforeRows, beforeLogs := len(db.metrics), len(db.metricLogs)
	again, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) {
		i.Quantity = 1200
		i.RequestId = "req-entry-a-different-key" // 幂等键换了，值没换
	}))
	mustNoErr(t, err)

	if again.Created || again.Corrected {
		t.Fatalf("同值重放不得产出新结论：created=%v corrected=%v", again.Created, again.Corrected)
	}
	if again.Metric.MetricId != first.Metric.MetricId || again.Metric.AmountMinor != first.Metric.AmountMinor {
		t.Fatalf("重放回读了另一行：%+v vs %+v", again.Metric, first.Metric)
	}
	// 差值断言：重放路径只做了 4 次定位读 + 组内重分配 + 回读，没有任何写。
	mustCallsFrom(t, db, beforeCalls,
		"sel:cr_enrollment.lockByMid",
		"sel:cr_revenue_rule.byCodeLock",
		"sel:cr_settlement.active",
		"sel:cr_metric.byKeyLock",
		"sel:cr_metric.groupLock",
		"sel:cr_metric.byKey",
	)
	for _, prefix := range []string{"ins:", "upd:", "del:"} {
		if got := db.countCallsAfter(beforeCalls, prefix); got != 0 {
			t.Fatalf("重放路径执行了 %s 语句 %d 次：%v", prefix, got, db.calls[beforeCalls:])
		}
	}
	// 现状（README 计量域）：变更台账是唯一能承载 request_id 的地方，重放路径不写它，
	// 所以这次带不同幂等键的调用在库里**完全不留痕**。
	if len(db.metricLogs) != beforeLogs || len(db.metrics) != beforeRows {
		t.Fatalf("重放改动了行数/留痕数：%d→%d 行，%d→%d 条留痕",
			beforeRows, len(db.metrics), beforeLogs, len(db.metricLogs))
	}
}

// 更正值：留痕必须排在覆盖之前（顺序反了就永久丢掉「原来算多少」）。
func TestRecordRevenueMetricCorrectionLogsOldValueBeforeOverwrite(t *testing.T) {
	ctx, db := readyMetric(t)
	_, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 1200 }))
	mustNoErr(t, err)

	before := len(db.calls)
	beforeLogs := len(db.metricLogs)
	reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) {
		i.Quantity = 300
		i.Operator = txOperator
		i.Reason = "spm 重算后回填"
		i.RequestId = "req-entry-fix"
	}))
	mustNoErr(t, err)
	if reply.Created || !reply.Corrected {
		t.Fatalf("更正结论错误：created=%v corrected=%v", reply.Created, reply.Corrected)
	}
	mustCallsFrom(t, db, before,
		"sel:cr_enrollment.lockByMid",
		"sel:cr_revenue_rule.byCodeLock",
		"sel:cr_settlement.active",
		"sel:cr_metric.byKeyLock",
		"ins:cr_metric_change_log",
		"upd:cr_metric.correction",
		"sel:cr_metric.groupLock",
		"sel:cr_metric.byKey",
	)
	if len(db.metricLogs) != beforeLogs+1 {
		t.Fatalf("更正必须留且只留一条痕：%+v", db.metricLogs)
	}
	lg := db.metricLogs[beforeLogs]
	if lg.OldQuantity != 1200 || lg.NewQuantity != 300 ||
		lg.OldAmountMinor != 1480 || lg.NewAmountMinor != 370 ||
		lg.OldCappedAmountMinor != 1480 || lg.NewCappedAmountMinor != 370 {
		t.Fatalf("留痕里的旧值/新值不符（旧值一旦丢失就无从复核）：%+v", lg)
	}
	if lg.Operator != txOperator || lg.Reason != "spm 重算后回填" || lg.RequestId != "req-entry-fix" {
		t.Fatalf("更正的操作人/原因/幂等键没进台账：%+v", lg)
	}
	if lg.Period != metricEntryPeriod || lg.Mid != txMid || lg.Aid != 11 ||
		int32(lg.SourceType) != model.SourceTypeVipWatch || lg.RuleCode != "R_VIP" {
		t.Fatalf("留痕的定位键与本次写入不符：%+v", lg)
	}
	if lg.OldRuleVersion != 3 || lg.NewRuleVersion != 3 {
		t.Fatalf("规则版本没记下：%+v", lg)
	}
	row := metricRowOf(db, 11)
	if row.Quantity != 300 || row.AmountMinor != 370 || row.CappedAmountMinor != 370 {
		t.Fatalf("覆盖没生效：%+v", row)
	}
	if row.Corrected != 1 {
		t.Fatalf("主表 corrected 位必须是 1：%+v", row)
	}
	// 更正过的行绝不能被门槛误伤：300 ≥ min_quantity=10。
	if row.ThresholdBlocked != 0 {
		t.Fatalf("达门槛行被拦了：%+v", row)
	}
}

// 重放判定必须逐项比（数量、规则编码、规则版本、金额）：
// 只有金额相同但规则版本变了，也是「口径变了」，必须走更正并留痕。
func TestRecordRevenueMetricReplayRequiresRuleVersionMatch(t *testing.T) {
	ctx, db := readyMetric(t)
	_, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 1000 }))
	mustNoErr(t, err)

	// 把规则版本号推上去而单价不变：折算出的金额一模一样（1234 分），但口径确实换了。
	// 必须改**活行**：db.ruleByCode 回的是副本，改它等于什么都没改。
	db.rules[0].Version = 4

	reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) {
		i.Quantity = 1000
		i.Operator = txOperator
		i.Reason = "规则改版后同值重报"
		i.RequestId = "req-entry-v4"
	}))
	mustNoErr(t, err)
	if reply.Created || !reply.Corrected {
		t.Fatalf("规则版本变了必须判更正：%+v", reply)
	}
	if reply.Metric.AmountMinor != 1234 || reply.Metric.RuleVersion != 4 {
		t.Fatalf("更正后的行没冻结新版本：%+v", reply.Metric)
	}
	if len(db.metricLogs) != 1 {
		t.Fatalf("必须留一条痕：%+v", db.metricLogs)
	}
	lg := db.metricLogs[0]
	if lg.OldRuleVersion != 3 || lg.NewRuleVersion != 4 ||
		lg.OldAmountMinor != 1234 || lg.NewAmountMinor != 1234 {
		t.Fatalf("同金额、跨版本的更正必须把新旧版本都记下来：%+v", lg)
	}
}

// ---------------------------------------------------------------- 组内封顶重分配

// 封顶是 (period, mid, source_type) 组级约束：后写的行会把额度重分配到前面已落的行上，
// 回给调用方的必须是重分配之后的真值。
func TestRecordRevenueMetricGroupCapReallocIsVisibleInReply(t *testing.T) {
	ctx, db := readyMetric(t)
	// 换成单价 1000 分/千分钟、月度封顶 1500 分的规则：1000 + 700 之后只剩 500 给第三行。
	db.rules = nil
	db.addRule(metricRule())

	first, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 1000 }))
	mustNoErr(t, err)
	if first.Metric.AmountMinor != 1000 || first.Metric.CappedAmountMinor != 1000 {
		t.Fatalf("第一行应拿到全部额度：%+v", first.Metric)
	}

	beforeSecond := len(db.calls)
	second, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) {
		i.Aid = 22
		i.Quantity = 700
		i.RequestId = "req-entry-22"
	}))
	mustNoErr(t, err)
	// 本行算出 700，但组内只剩 500：回给调用方的必须是 500，不是内存里算出的 700。
	if second.Metric.AmountMinor != 700 || second.Metric.CappedAmountMinor != 500 {
		t.Fatalf("第二次写的回显没反映重分配：%+v", second.Metric)
	}
	if got := db.countCallsAfter(beforeSecond, "upd:cr_metric.capped"); got != 1 {
		t.Fatalf("第二次写应回写一行旧账的 capped，实际 %d 次：%v",
			got, db.calls[beforeSecond:])
	}

	beforeThird := len(db.calls)
	third, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) {
		i.Aid = 33
		i.Quantity = 500
		i.RequestId = "req-entry-33"
	}))
	mustNoErr(t, err)
	if third.Metric.AmountMinor != 500 || third.Metric.CappedAmountMinor != 0 {
		t.Fatalf("第三行应被封顶清零（但保留 amount 供复核）：%+v", third.Metric)
	}
	// 第三次只该回写第三行自己：前两行的分配已经稳定，重算必须可复现。
	if got := db.countCallsAfter(beforeThird, "upd:cr_metric.capped"); got != 1 {
		t.Fatalf("第三次写不该重排已定的行，实际回写 %d 次：%v", got, db.calls[beforeThird:])
	}

	// 组内三行的最终额度分配必须等于「按 aid 升序吃满 1500」。
	for aid, want := range map[int64]int64{11: 1000, 22: 500, 33: 0} {
		got := metricRowOf(db, aid)
		if got == nil || got.CappedAmountMinor != want {
			t.Fatalf("aid=%d 的封顶后金额期望 %d，实际 %+v", aid, want, got)
		}
	}
	sum := metricRowOf(db, 11).CappedAmountMinor + metricRowOf(db, 22).CappedAmountMinor +
		metricRowOf(db, 33).CappedAmountMinor
	if sum != 1500 {
		t.Fatalf("组内合计必须恰好用满月度封顶，实际 %d", sum)
	}
}

// ---------------------------------------------------------------- 下游故障与并发

// 事务中途的真库故障：原始错误上抛 + 整体回滚（台账行不能留下半条）。
func TestRecordRevenueMetricDbFaultRollsBackWholeTx(t *testing.T) {
	ctx, db := readyMetric(t)
	boom := errors.New("deadlock detected on cr_metric")
	db.failOn["ins:cr_metric"] = boom

	reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 1200 }))
	if reply != nil {
		t.Fatalf("故障不得回伪结论：%+v", reply)
	}
	if err == nil {
		t.Fatal("故障被吞掉了")
	}
	// 三类故障之一：原始错误上抛（既不折叠成「幂等成功」也不折叠成「写成功」）。
	if !errors.Is(err, boom) {
		t.Fatalf("原始错误没上抛：%v", err)
	}
	if len(db.metrics) != 0 || len(db.metricLogs) != 0 {
		t.Fatalf("回滚没撤干净：%+v %+v", db.metrics, db.metricLogs)
	}
	// 事务确实开到过那一步（失败点可判别），而不是根本没执行就报错。
	mustCallsFrom(t, db, 0,
		"sel:cr_enrollment.lockByMid", "sel:cr_revenue_rule.byCodeLock",
		"sel:cr_settlement.active", "sel:cr_metric.byKeyLock", "ins:cr_metric")
}

// 并发窗口：另一会话绕过 SELECT ... FOR UPDATE 抢先落行 → INSERT 撞 uniq_metric_key。
// 必须回可重试的 ErrConcurrentUpdate，而不是把「重复键」当新建成功。
func TestRecordRevenueMetricRaceOnInsertIsRetryableNotFakeSuccess(t *testing.T) {
	ctx, db := readyMetric(t)
	rival := db.addMetric(&model.RevenueMetric{
		Period: metricEntryPeriod, Mid: txMid, Aid: 11, SourceType: model.SourceTypeVipWatch,
		RuleCode: "R_VIP", RuleVersion: 3, Quantity: 999, Unit: "minute",
		AmountMinor: 1232, CappedAmountMinor: 1232, SourceDetail: "另一会话先落的行",
	})
	db.hideForRead[rival.MetricId] = true // 本会话的行锁读不到它

	reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 1200 }))
	mustErrIs(t, err, model.ErrConcurrentUpdate)
	if reply != nil {
		t.Fatalf("并发冲突不得回结论：%+v", reply)
	}
	if live := metricRowOf(db, 11); live == nil || live.Quantity != 999 || live.AmountMinor != 1232 {
		t.Fatalf("回滚不该改动对方已落的行：%+v", live)
	}
	// 撞键后必须立刻停下：不得继续做组内重分配（那会把别人的行按本请求的规则重算一遍）。
	mustCallsFrom(t, db, 0,
		"sel:cr_enrollment.lockByMid", "sel:cr_revenue_rule.byCodeLock",
		"sel:cr_settlement.active", "sel:cr_metric.byKeyLock", "ins:cr_metric")
}

// 更正的 CAS 不命中（数量被并发改变）：留痕与覆盖要么都在要么都不在。
func TestRecordRevenueMetricCorrectionCasMissRollsBackLog(t *testing.T) {
	ctx, db := readyMetric(t)
	_, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) { i.Quantity = 1200 }))
	mustNoErr(t, err)
	db.noRowsFor["upd:cr_metric.correction"] = true

	reply, err := runRecord(t, ctx, metricInput(func(i *rpc.RecordRevenueMetricReq) {
		i.Quantity = 300
		i.Operator = txOperator
		i.Reason = "并发更正"
		i.RequestId = "req-entry-cas"
	}))
	mustErrIs(t, err, model.ErrConcurrentUpdate)
	if reply != nil {
		t.Fatalf("CAS 未命中不得回结论：%+v", reply)
	}
	if len(db.metricLogs) != 0 {
		t.Fatalf("覆盖失败时旧值留痕必须一起回滚，否则台账断链：%+v", db.metricLogs)
	}
	if row := metricRowOf(db, 11); row == nil || row.Quantity != 1200 || row.AmountMinor != 1480 {
		t.Fatalf("主表必须回到更之前的值：%+v", row)
	}
}

// ---------------------------------------------------------------- 出金边界（AGENTS.md §1）

// 计量写入是本服务唯一的应计产出口，但它绝不能碰结算单：
// 一条 UPDATE/INSERT cr_settlement 都没有，payout_state 一个字节都不动。
func TestRecordRevenueMetricNeverTouchesSettlementOrPayout(t *testing.T) {
	ctx, db := readyMetric(t)
	draft := db.addSettlement(&model.Settlement{
		SettlementNo: "CRS-DRAFT-UNTUCHED", Period: metricEntryPeriod, Mid: txMid,
		AmountMinor: 1_000, CapAppliedMinor: 0, Currency: "CNY", MetricCount: 1,
		State: model.SettlementStateDraft, PayoutState: model.PayoutStateNotPayable,
		VoidSeq: 0, RequestId: "seed-draft",
	})

	// 首写、更正、同值重放三种结论各来一遍。
	qtys := []int64{1200, 900, 900}
	for i, qty := range qtys {
		in := metricInput(func(x *rpc.RecordRevenueMetricReq) {
			x.Quantity = qty
			x.RequestId = "req-payout-" + strconv.Itoa(i)
		})
		if i == 1 {
			in.Operator = txOperator
			in.Reason = "更正金额"
		}
		_, err := runRecord(t, ctx, in)
		mustNoErr(t, err)
	}

	if n := db.countCalls("ins:cr_settlement"); n != 0 {
		t.Fatalf("计量入口出了新单：%d 次（%v）", n, db.calls)
	}
	if n := db.countCalls("upd:cr_settlement"); n != 0 {
		t.Fatalf("计量入口改了结算单（含 payout_state）：%d 次（%v）", n, db.calls)
	}
	if n := db.countCalls("del:"); n != 0 {
		t.Fatalf("计量入口删了东西：%d 次（%v）", n, db.calls)
	}
	// 唯一碰 cr_settlement 的语句是「本周期是否已确认」这一条读，且三次调用各一次。
	if n := db.countCalls("sel:cr_settlement.active"); n != len(qtys) {
		t.Fatalf("结算冻结闸门读次数错误：%d（期望 %d）", n, len(qtys))
	}
	after := db.settlementByNo(draft.SettlementNo)
	if after.AmountMinor != 1_000 || after.CapAppliedMinor != 0 ||
		after.State != model.SettlementStateDraft ||
		after.PayoutState != model.PayoutStateNotPayable || after.MetricCount != 1 {
		t.Fatalf("在效结算单被计量写入改动了：写后 %+v（写前 %+v）", after, draft)
	}
}
