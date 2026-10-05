package logic

// 本文件锁两个「收益概览与计量台账」读入口（GetRevenueSummary / ListRevenueMetrics）
// 的判定口径，重点在资金口径与窗口口径：
//   - current_estimate_minor 取的是 capped_amount_minor（封顶后应计），且 SQL 谓词只有
//     period+mid（model/cr_metric.go:317），既不过滤 threshold_blocked 也不看有没有出过单；
//   - total_confirmed_minor 的窗口由 CreatorRevenue.SummaryRecentPeriods 决定，
//     边界是 `period >= cutoff`（含 cutoff，model/cr_settlement.go:277）；
//     期望值用测试里独立推导的 periodBack 算，不复用被测代码调用的 model.PeriodCutoff，
//     否则就是拿实现当期望值的循环论证；
//   - last_settled_period 的在效判据是 void_seq=0（model/cr_settlement.go:294），不是 state；
//   - 出金红线：payout_available 恒 false，且结论必须由服务端 payout_note 给出；
//   - 概览与台账列表的请求里都没有调用方身份位：跨作者枚举只做得到全量数据，
//     已按现状钉住并登记 README（计量域）。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"
)

func runGetRevenueSummary(t *testing.T, ctx *svc.ServiceContext, in *rpc.GetRevenueSummaryReq) (*rpc.GetRevenueSummaryReply, error) {
	t.Helper()
	return NewGetRevenueSummaryLogic(context.Background(), ctx).GetRevenueSummary(in)
}

func runListRevenueMetrics(t *testing.T, ctx *svc.ServiceContext, in *rpc.ListRevenueMetricsReq) (*rpc.ListRevenueMetricsReply, error) {
	t.Helper()
	return NewListRevenueMetricsLogic(context.Background(), ctx).ListRevenueMetrics(in)
}

// periodBack 独立推导「比 current 早 back 个周期」的周期码。
// 不复用 model.PeriodCutoff：那是被测代码自己调的函数，拿它当期望值就测不出窗口写错。
func periodBack(t *testing.T, current string, back int) string {
	t.Helper()
	y, err := strconv.Atoi(current[:4])
	if err != nil {
		t.Fatalf("当前周期码异常：%s", current)
	}
	m, err := strconv.Atoi(current[4:])
	if err != nil {
		t.Fatalf("当前周期码异常：%s", current)
	}
	idx := y*12 + (m - 1) - back
	if idx < 0 {
		t.Fatal("种子周期早于公元 1 年，用例需重新设计")
	}
	return fmt.Sprintf("%04d%02d", idx/12, idx%12+1)
}

// summaryMetricSeed 造一行台账（金额全是明显假值）。
func summaryMetricSeed(period string, mid, aid int64, sourceType int32, amount, capped int64, blocked int32) *model.RevenueMetric {
	return &model.RevenueMetric{
		Period: period, Mid: mid, Aid: aid, SourceType: sourceType,
		RuleCode: "R_VIP", RuleVersion: 3, Quantity: amount, Unit: "minute",
		AmountMinor: amount, CappedAmountMinor: capped, ThresholdBlocked: blocked,
		SourceDetail: "口径摘要", Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	}
}

// summarySettleSeed 造一张结算单（默认 payout_state 与写侧一致：NOT_PAYABLE）。
func summarySettleSeed(no, period string, mid, amount int64, state, payoutState int32, voidSeq int64) *model.Settlement {
	return &model.Settlement{
		SettlementNo: no, Period: period, Mid: mid, AmountMinor: amount,
		CapAppliedMinor: 0, Currency: "CNY", MetricCount: 1,
		State: state, PayoutState: payoutState, ConfirmedBy: txOperator, ConfirmedAt: 1_700_000_000,
		VoidReason: "", RequestId: "seed-" + no, VoidSeq: voidSeq,
		Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	}
}

func metricKeySeq(rows []*rpc.RevenueMetricInfo) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s/%d/%d/%d", r.Period, r.Mid, r.Aid, int32(r.SourceType)))
	}
	return out
}

func mustMetricSeq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("台账键序列不符：期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项不符：期望 %v，实际 %v", i, want, got)
		}
	}
}

// ---------------------------------------------------------------- GetRevenueSummary

func TestGetRevenueSummaryGatesTouchNoData(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.GetRevenueSummaryReq
	}{
		{"mid 为零", &rpc.GetRevenueSummaryReq{Mid: 0}},
		{"mid 为负", &rpc.GetRevenueSummaryReq{Mid: -1}},
		{"nil 请求折成 mid=0", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			reply, err := runGetRevenueSummary(t, ctx, c.in)
			mustErrIs(t, err, model.ErrInvalidMid)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			if len(db.reads) != 0 || len(db.calls) != 0 {
				t.Fatalf("闸门未过却已访问数据：%v / %v", db.reads, db.calls)
			}
		})
	}
}

func TestGetRevenueSummaryReadyGate(t *testing.T) {
	ctx, db := newTestSvc(t)
	ctx.Settlements = nil
	reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustErrIs(t, err, model.ErrDBNotConfigured)
	if reply != nil {
		t.Fatalf("未配置库不得回结论：%+v", reply)
	}
	if len(db.reads) != 0 {
		t.Fatalf("Ready 闸门不该访问数据：%v", db.reads)
	}
}

// 四步聚合的调用轨迹必须逐项钉死：多读一张表就是越权访问，少读一张就是漏口径。
// 读侧一条 SQL 写语句都不该出现（db.calls 恒空）。
func TestGetRevenueSummaryReadOrderAndEstimateAggregation(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := model.CurrentPeriod()
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)

	db.addMetric(summaryMetricSeed(cur, txMid, 11, model.SourceTypeVipWatch, 700, 700, 0))
	db.addMetric(summaryMetricSeed(cur, txMid, 12, model.SourceTypeCoin, 300, 300, 0))
	db.addMetric(summaryMetricSeed(cur, txMid, 13, model.SourceTypeInteraction, 90, 0, 1)) // 被门槛拦掉
	// 现状哨兵：SumCappedByPeriod 的谓词只有 period+mid（cr_metric.go:317），
	// 不看 threshold_blocked。被拦掉的行按写侧口径 capped 必为 0，所以线上不出差异；
	// 一旦有人改写侧让拦掉的行也带 capped，概览会立刻把它算进「本月预估」——钉在这里。
	db.addMetric(summaryMetricSeed(cur, txMid, 14, model.SourceTypeActivity, 40, 40, 1))
	// 别的周期 / 别人的行都不得串味。
	db.addMetric(summaryMetricSeed(periodBack(t, cur, 1), txMid, 11, model.SourceTypeVipWatch, 9999, 9999, 0))
	db.addMetric(summaryMetricSeed(cur, 999, 11, model.SourceTypeVipWatch, 8888, 8888, 0))

	reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustNoErr(t, err)
	if reply.CurrentPeriod != cur {
		t.Fatalf("当前周期必须取服务端时钟：got=%s want=%s", reply.CurrentPeriod, cur)
	}
	if reply.CurrentEstimateMinor != 700+300+40 {
		t.Fatalf("本月预估口径错误（应为封顶后应计合计）：%d", reply.CurrentEstimateMinor)
	}
	if reply.Enrollment.State != rpc.EnrollmentState(model.EnrollmentStateEnrolled) ||
		reply.Enrollment.Mid != txMid {
		t.Fatalf("参与状态投影错误：%+v", reply.Enrollment)
	}
	db.assertReads(t, 0,
		"fake:enrollments.FindOne",
		"fake:metrics.SumCappedByPeriod",
		"fake:settlements.SumConfirmed",
		"fake:settlements.LatestActivePeriod",
	)
	if len(db.calls) != 0 {
		t.Fatalf("概览读侧执行了 SQL 写路径语句：%v", db.calls)
	}
}

// 从未参加计划：显式回一个只有 mid + UNSPECIFIED 的壳，而不是 nil、也不是错误。
func TestGetRevenueSummaryNeverEnrolledShell(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addMetric(summaryMetricSeed(model.CurrentPeriod(), txMid, 11, model.SourceTypeVipWatch, 500, 500, 0))

	reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustNoErr(t, err)
	if reply.Enrollment == nil {
		t.Fatal("未参加必须回非 nil 的壳，让客户端区分「没参加」与「没查」")
	}
	if reply.Enrollment.State != rpc.EnrollmentState_ENROLLMENT_STATE_UNSPECIFIED ||
		reply.Enrollment.Mid != txMid {
		t.Fatalf("壳投影错误：%+v", reply.Enrollment)
	}
	if reply.Enrollment.EnrolledAt != 0 || reply.Enrollment.Operator != "" {
		t.Fatalf("从未参加却回出时间戳/责任人：%+v", reply.Enrollment)
	}
	// 没有参与关系也照样聚合台账：概览是「读了什么算什么」，不替调用方判断资格。
	if reply.CurrentEstimateMinor != 500 {
		t.Fatalf("预估聚合不该因未参加而停摆：%d", reply.CurrentEstimateMinor)
	}
}

// 窗口口径：SummaryRecentPeriods=12 → cutoff = 早 11 个周期，边界含 cutoff（period >= ?）。
// 只取 CONFIRMED：DRAFT（未确认）与 VOIDED（被推翻）都不算「已确认应计」。
func TestGetRevenueSummaryConfirmedWindowAndStateFilter(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := model.CurrentPeriod() // newTestSvc 配的 SummaryRecentPeriods = 12

	db.addSettlement(summarySettleSeed("S-IN", periodBack(t, cur, 11), txMid, 111,
		model.SettlementStateConfirmed, model.PayoutStateNotPayable, 0))
	db.addSettlement(summarySettleSeed("S-CUR", cur, txMid, 222,
		model.SettlementStateConfirmed, model.PayoutStateNotPayable, 0))
	db.addSettlement(summarySettleSeed("S-OUT", periodBack(t, cur, 12), txMid, 88888,
		model.SettlementStateConfirmed, model.PayoutStateNotPayable, 0))
	db.addSettlement(summarySettleSeed("S-DRAFT", cur, txMid, 44444,
		model.SettlementStateDraft, model.PayoutStateNotPayable, 0))
	db.addSettlement(summarySettleSeed("S-VOID", cur, txMid, 55555,
		model.SettlementStateVoided, model.PayoutStateNotPayable, 55555))
	db.addSettlement(summarySettleSeed("S-OTHER", cur, 999, 66666,
		model.SettlementStateConfirmed, model.PayoutStateNotPayable, 0))

	reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustNoErr(t, err)
	if reply.TotalConfirmedMinor != 111+222 {
		t.Fatalf("窗口/状态口径错误：got=%d want=%d（越窗的 88888、DRAFT 的 44444、VOIDED 的 55555 都不该进来）",
			reply.TotalConfirmedMinor, 111+222)
	}

	// 窗口开关：SummaryRecentPeriods=0 表示全历史（model/now.go:87 空 cutoff 不开窗）。
	ctx.Config.CreatorRevenue.SummaryRecentPeriods = 0
	all, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustNoErr(t, err)
	if all.TotalConfirmedMinor != 111+222+88888 {
		t.Fatalf("窗口关闭后应含全历史：got=%d", all.TotalConfirmedMinor)
	}
}

// 最近出单周期：判据是 void_seq=0（在效），取 period 最大者并数值化。
func TestGetRevenueSummaryLastSettledPeriod(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := model.CurrentPeriod()
	older := periodBack(t, cur, 3)
	middle := periodBack(t, cur, 1)

	// 三张在效单里 DRAFT 也算「出过单」（单已生成，只是没确认）。
	db.addSettlement(summarySettleSeed("S-OLD", older, txMid, 10, model.SettlementStateDraft,
		model.PayoutStateNotPayable, 0))
	db.addSettlement(summarySettleSeed("S-MID", middle, txMid, 10, model.SettlementStateConfirmed,
		model.PayoutStateNotPayable, 0))
	// 作废单：周期更新，但 void_seq 已改成本行 id → 必须被排除，
	// 否则「被推翻的重算」会显示成「已结算到更近的月份」。
	db.addSettlement(summarySettleSeed("S-VOID", cur, txMid, 10, model.SettlementStateVoided,
		model.PayoutStateNotPayable, 777))
	// 别人的更近一单不得串味。
	db.addSettlement(summarySettleSeed("S-OTHER", cur, 999, 10, model.SettlementStateConfirmed,
		model.PayoutStateNotPayable, 0))

	reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustNoErr(t, err)
	want, err := strconv.ParseInt(middle, 10, 64)
	mustNoErr(t, err)
	if reply.LastSettledPeriod != want {
		t.Fatalf("最近出单周期错误：got=%d want=%d（%s）", reply.LastSettledPeriod, want, middle)
	}

	// 从没出过单 → 0（不是「当前周期」，也不是空串冒充的某个数）。
	fresh, _ := newTestSvc(t)
	none, err := runGetRevenueSummary(t, fresh, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustNoErr(t, err)
	if none.LastSettledPeriod != 0 {
		t.Fatalf("未出单必须回 0：%d", none.LastSettledPeriod)
	}
	if none.CurrentEstimateMinor != 0 || none.TotalConfirmedMinor != 0 {
		t.Fatalf("空台账应回 0 而不是报错：%+v", none)
	}
	if none.Enrollment.State != rpc.EnrollmentState_ENROLLMENT_STATE_UNSPECIFIED {
		t.Fatalf("空台账下参与壳错误：%+v", none.Enrollment)
	}
}

// 现状哨兵（计量/结算口径，登记 README）：LatestActivePeriod 的在效判据是
// `void_seq = 0`（model/cr_settlement.go:294）而不是 `state <> VOIDED`。
// 出单与作废路径永远成对写这两个字段，所以脏行只能来自绕过写入口的手工改库；
// 但一旦漏写，概览会把作废单显示成「已结算到那个月」，且没有任何错误信号。
func TestGetRevenueSummaryLastSettledPeriodKeysOnVoidSeqNotState(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := model.CurrentPeriod()
	db.addSettlement(summarySettleSeed("S-DIRTY", cur, txMid, 10,
		model.SettlementStateVoided, model.PayoutStateNotPayable, 0)) // state 已作废、void_seq 仍为 0

	reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustNoErr(t, err)
	dirty, err := strconv.ParseInt(cur, 10, 64)
	mustNoErr(t, err)
	if reply.LastSettledPeriod != dirty {
		t.Fatalf("现状：在效判据是 void_seq，脏行必被当成在效单（若改为按 state 判定请撤销本用例与 README 项）：%d",
			reply.LastSettledPeriod)
	}
}

// 出金红线（AGENTS.md §1/§5）：三种数据形态下 payout_available 都必须 false，
// 且结论由服务端 payout_note 给出（不能让各端自己猜）；金额一律是「应计」而非「已到账」。
func TestGetRevenueSummaryPayoutNeverAvailableInAnyShape(t *testing.T) {
	note := "当前版本未接入提现与打款通道，此处金额均为应计金额（分），不代表已到账"
	shapes := []struct {
		name string
		seed func(t *testing.T, db *fakeDB, cur string)
	}{
		{"空台账", func(t *testing.T, db *fakeDB, cur string) {}},
		{"有应计无结算", func(t *testing.T, db *fakeDB, cur string) {
			db.addMetric(summaryMetricSeed(cur, txMid, 11, model.SourceTypeVipWatch, 700, 700, 0))
		}},
		{"已确认结算单（最容易被误读成已到账）", func(t *testing.T, db *fakeDB, cur string) {
			db.addMetric(summaryMetricSeed(cur, txMid, 11, model.SourceTypeVipWatch, 700, 700, 0))
			db.addSettlement(summarySettleSeed("S-CONF", cur, txMid, 700,
				model.SettlementStateConfirmed, model.PayoutStateNotPayable, 0))
		}},
	}
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			cur := model.CurrentPeriod()
			s.seed(t, db, cur)
			reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
			mustNoErr(t, err)
			if reply.PayoutAvailable {
				t.Fatal("出金红线：任何数据形态下 payout_available 都必须 false")
			}
			// 文案逐字钉住：这是「没打款」这个结论在服务端唯一的落点，
			// 客户端换端不改代码，所以口径不能漂。
			if reply.PayoutNote != note {
				t.Fatalf("出金结论必须由服务端原样给出：got=%q", reply.PayoutNote)
			}
			if len(db.calls) != 0 {
				t.Fatalf("概览不得触发任何写语句：%v", db.calls)
			}
			// 已确认金额照样回，但 payout 位不变——证明「确认」与「出金」在数据上是两件事。
			switch s.name {
			case "已确认结算单（最容易被误读成已到账）":
				if reply.TotalConfirmedMinor != 700 || reply.LastSettledPeriod == 0 {
					t.Fatalf("确认金额应可见（它是应计台账，不是被打款吞掉了）：%+v", reply)
				}
			default:
				if reply.TotalConfirmedMinor != 0 {
					t.Fatalf("未确认不得计进已确认合计：%+v", reply)
				}
			}
		})
	}
}

// 四类读的故障必须逐个原样上抛：任何一步失败都不许回「半截概览」（0 值看起来像「没赚钱」）。
// wantReads 钉的是「失败发生在第几步」——多一步说明错误被吞了还在继续往下查。
func TestGetRevenueSummaryReadFailuresPropagateRaw(t *testing.T) {
	all := []string{
		"fake:enrollments.FindOne",
		"fake:metrics.SumCappedByPeriod",
		"fake:settlements.SumConfirmed",
		"fake:settlements.LatestActivePeriod",
	}
	for i, op := range all {
		op, i := op, i
		t.Run(op, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			cur := model.CurrentPeriod()
			seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
			db.addMetric(summaryMetricSeed(cur, txMid, 11, model.SourceTypeVipWatch, 700, 700, 0))
			db.addSettlement(summarySettleSeed("S-CONF", cur, txMid, 700,
				model.SettlementStateConfirmed, model.PayoutStateNotPayable, 0))
			boom := errors.New("invalid connection")
			db.readFailOn[op] = boom

			reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
			if !errors.Is(err, boom) {
				t.Fatalf("必须原样上抛：%v", err)
			}
			if reply != nil {
				t.Fatalf("故障不得回半截概览：%+v", reply)
			}
			db.assertReads(t, 0, all[:i+1]...)
		})
	}
}

// 现状哨兵（高危，登记 README 计量域）：概览是创作者端首页接口，
// 但 GetRevenueSummaryReq 只有 mid，没有任何调用方身份位，服务端无从校验归属。
// 只要网关把会话里的 mid 换成请求参数，任何人都能读到别人的
// 本月预估、历史已确认合计与最近出单周期。
func TestGetRevenueSummaryHasNoCallerIdentity(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := model.CurrentPeriod()
	seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
	db.addMetric(summaryMetricSeed(cur, txMid, 11, model.SourceTypeVipWatch, 4321, 4321, 0))
	db.addSettlement(summarySettleSeed("S-CONF", cur, txMid, 9876,
		model.SettlementStateConfirmed, model.PayoutStateNotPayable, 0))

	reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustNoErr(t, err)
	if reply.CurrentEstimateMinor != 4321 || reply.TotalConfirmedMinor != 9876 {
		t.Fatalf("现状：请求方身份缺失，服务端照抄 mid 就回全量金额：%+v", reply)
	}
}

// ---------------------------------------------------------------- ListRevenueMetrics

func TestListRevenueMetricsGatesTouchNoData(t *testing.T) {
	cur := model.CurrentPeriod()
	cases := []struct {
		name   string
		in     *rpc.ListRevenueMetricsReq
		target error
		want   string // 错误文案里的必备片段（空表示不校验）
	}{
		{"period 位数不足", &rpc.ListRevenueMetricsReq{Period: "2026-1"}, model.ErrInvalidPeriod, "不是 6 位数字"},
		{"period 月份越界", &rpc.ListRevenueMetricsReq{Period: "202613"}, model.ErrInvalidPeriod, "月份"},
		{"period 含非数字", &rpc.ListRevenueMetricsReq{Period: "2026ab"}, model.ErrInvalidPeriod, "非数字"},
		{"period 与 mid 都不给（无界扫描）", &rpc.ListRevenueMetricsReq{}, model.ErrQueryScopeRequired, "period 或 mid"},
		{"period 只有空白等同没给", &rpc.ListRevenueMetricsReq{Period: "   "}, model.ErrQueryScopeRequired, ""},
		{"mid 负数", &rpc.ListRevenueMetricsReq{Period: cur, Mid: -1}, model.ErrInvalidMid, "mid=-1"},
		{"aid 负数", &rpc.ListRevenueMetricsReq{Period: cur, Aid: -1}, model.ErrInvalidAid, "0 表示不挂具体内容"},
		{"source_type 越界", &rpc.ListRevenueMetricsReq{Period: cur, SourceType: 9}, model.ErrInvalidSourceType, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			reply, err := runListRevenueMetrics(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里缺少 %q：%v", c.want, err)
			}
			if len(db.reads) != 0 || len(db.calls) != 0 {
				t.Fatalf("闸门未过却已访问数据：%v / %v", db.reads, db.calls)
			}
		})
	}
}

// 守卫先后顺序必须可判别：范围守卫（period 或 mid 至少一个）排在 mid 正负判定之前，
// 所以「period 为空 + mid=-1」能绕过范围守卫，再由下一个守卫拒掉——
// 若哪天有人把 mid<0 的校验挪到前面，回的错误码就会从 ErrInvalidMid 变成 ErrQueryScopeRequired。
func TestListRevenueMetricsScopeGuardPrecedesMidSignGuard(t *testing.T) {
	ctx, db := newTestSvc(t)
	_, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Mid: -1})
	mustErrIs(t, err, model.ErrInvalidMid)
	_, err = runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Mid: 0})
	mustErrIs(t, err, model.ErrQueryScopeRequired)
	if len(db.reads) != 0 || len(db.calls) != 0 {
		t.Fatalf("闸门未过却已访问数据：%v / %v", db.reads, db.calls)
	}
}

func TestListRevenueMetricsReadyGate(t *testing.T) {
	ctx, db := newTestSvc(t)
	ctx.Metrics = nil
	reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Period: "202601"})
	mustErrIs(t, err, model.ErrDBNotConfigured)
	if reply != nil {
		t.Fatalf("未配置库不得回结论：%+v", reply)
	}
	if len(db.reads) != 0 {
		t.Fatalf("Ready 闸门不该访问数据：%v", db.reads)
	}
}

// 排序事实源：model/cr_metric.go:270
// `ORDER BY period ASC, mid ASC, aid ASC, source_type ASC`。
// 四个键全打乱插入，插入序泄漏即红；再逐页拼接与全量读比对。
func TestListRevenueMetricsOrderByFourKeysAndPages(t *testing.T) {
	ctx, db := newTestSvc(t)
	mk := func(period string, mid, aid int64, st int32) {
		db.addMetric(summaryMetricSeed(period, mid, aid, st, 100, 100, 0))
	}
	mk("202602", 200, 30, model.SourceTypeCoin)     // 最后
	mk("202601", 100, 11, model.SourceTypeVipWatch) // 最先
	mk("202601", 100, 11, model.SourceTypeCoin)     // 同前三键，source_type 更大
	mk("202601", 100, 22, model.SourceTypeVipWatch) // aid 更大
	mk("202601", 300, 11, model.SourceTypeVipWatch) // mid 更大

	full, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Mid: 0, Period: ""})
	if err == nil {
		t.Fatal("不带 period 与 mid 的全量查询必须被范围守卫拒掉")
	}
	_ = full

	all, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Period: "202601", Size: 50})
	mustNoErr(t, err)
	mustMetricSeq(t, metricKeySeq(all.Metrics),
		"202601/100/11/1", "202601/100/11/2", "202601/100/22/1", "202601/300/11/1")
	if all.Total != 4 {
		t.Fatalf("total 与过滤条件不同源：%d", all.Total)
	}
	if w := db.window("fake:metrics.List"); w.offset != 0 || w.limit != 50 {
		t.Fatalf("分页位没进查询：%+v", w)
	}

	// 跨周期全量（period 只给到范围守卫能过的 mid）：拼接顺序必须是 period 优先。
	var paged []string
	for page := int64(1); page <= 3; page++ {
		reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Mid: 200, Page: page, Size: 1})
		mustNoErr(t, err)
		if w := db.window("fake:metrics.List"); w.offset != page-1 || w.limit != 1 {
			t.Fatalf("第 %d 页分页位错误：%+v", page, w)
		}
		paged = append(paged, metricKeySeq(reply.Metrics)...)
	}
	mustMetricSeq(t, paged, "202602/200/30/2")

	// 跨作者拼接：只带 period 时按 (period, mid, aid, source_type) 全局有序。
	var byPeriod []string
	for page := int64(1); page <= 3; page++ {
		reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Period: "202601", Page: page, Size: 2})
		mustNoErr(t, err)
		byPeriod = append(byPeriod, metricKeySeq(reply.Metrics)...)
	}
	mustMetricSeq(t, byPeriod,
		"202601/100/11/1", "202601/100/11/2", "202601/100/22/1", "202601/300/11/1")
}

// period 归一化沿用 model.ValidatePeriod 的宽容写法（容忍 - / _ / 空白），
// 但送进查询的必须是归一后的 6 位码，否则一条也匹配不上。
func TestListRevenueMetricsNormalizesPeriodBeforeQuery(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addMetric(summaryMetricSeed("202601", txMid, 11, model.SourceTypeVipWatch, 100, 100, 0))
	db.addMetric(summaryMetricSeed("202512", txMid, 11, model.SourceTypeVipWatch, 100, 100, 0))

	for _, raw := range []string{"2026-01", "2026_01", " 202601 ", "2026 01"} {
		from := len(db.reads)
		reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Period: raw, Mid: txMid})
		mustNoErr(t, err)
		mustMetricSeq(t, metricKeySeq(reply.Metrics), "202601/100/11/1")
		if reply.Total != 1 {
			t.Fatalf("归一化后仍按原串过滤（%q）：total=%d", raw, reply.Total)
		}
		db.assertReads(t, from, "fake:metrics.List", "fake:metrics.Count")
	}
}

func TestListRevenueMetricsFiltersAreOptional(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addMetric(summaryMetricSeed("202601", 100, 11, model.SourceTypeVipWatch, 100, 100, 0))
	db.addMetric(summaryMetricSeed("202601", 100, 11, model.SourceTypeCoin, 200, 200, 0))
	db.addMetric(summaryMetricSeed("202601", 100, 22, model.SourceTypeVipWatch, 300, 300, 0))
	db.addMetric(summaryMetricSeed("202601", 200, 11, model.SourceTypeVipWatch, 400, 400, 0))

	cases := []struct {
		name       string
		period     string
		mid, aid   int64
		sourceType int32
		want       []string
		wantTotal  int64
	}{
		{"只带 mid（跨周期）", "", 100, 0, 0, []string{"202601/100/11/1", "202601/100/11/2", "202601/100/22/1"}, 3},
		{"mid + 来源", "202601", 100, 0, model.SourceTypeCoin, []string{"202601/100/11/2"}, 1},
		{"mid + aid", "202601", 100, 22, 0, []string{"202601/100/22/1"}, 1},
		{"只带 period（跨作者）", "202601", 0, 0, 0,
			[]string{"202601/100/11/1", "202601/100/11/2", "202601/100/22/1", "202601/200/11/1"}, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			from := len(db.reads)
			reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{
				Period: c.period, Mid: c.mid, Aid: c.aid,
				SourceType: rpc.RevenueSourceType(c.sourceType), Size: 50,
			})
			mustNoErr(t, err)
			mustMetricSeq(t, metricKeySeq(reply.Metrics), c.want...)
			if reply.Total != c.wantTotal {
				t.Fatalf("total=%d 与 rows=%d 不同源", reply.Total, len(reply.Metrics))
			}
			db.assertReads(t, from, "fake:metrics.List", "fake:metrics.Count")
		})
	}
}

// 现状哨兵（登记 README 计量域）：aid=0 在写入侧合法（运营激励不挂具体内容），
// 在查询侧却被 metricFilter 当成「不过滤」（cr_metric.go:365）。
// 同一个值两个语义：既无法只查「不挂内容」的激励行，也无法区分「没传 aid」。
func TestListRevenueMetricsAidZeroCannotSelectActivityRows(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addMetric(summaryMetricSeed("202601", txMid, 0, model.SourceTypeActivity, 500, 500, 0))
	db.addMetric(summaryMetricSeed("202601", txMid, 55, model.SourceTypeVipWatch, 100, 100, 0))

	want := "202601/100/0/4"
	// aid=0 与「不存在的 aid」两种取值一起看才能判出 0 到底是过滤值还是不过滤。
	cases := []struct {
		aid  int64
		want []string
	}{
		{0, []string{want, "202601/100/55/1"}}, // 现状：不过滤，两行都回
		{55, []string{"202601/100/55/1"}},      // 非 0 才真正下推成 aid = ?
		{777777, nil},                          // 反证：精确匹配确实生效
	}
	for _, c := range cases {
		reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Period: "202601", Mid: txMid, Aid: c.aid})
		mustNoErr(t, err)
		if len(c.want) == 0 {
			if len(reply.Metrics) != 0 {
				t.Fatalf("aid=%d 应为精确匹配后无行，实际 %v", c.aid, metricKeySeq(reply.Metrics))
			}
			continue
		}
		got := metricKeySeq(reply.Metrics)
		if len(got) != len(c.want) || got[0] != c.want[0] {
			t.Fatalf("现状：aid=%d 的结果不符（期望 %v，实际 %v）", c.aid, c.want, got)
		}
	}
}

func TestListRevenueMetricsPageSizeFoldingReachesQuery(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addMetric(summaryMetricSeed("202601", txMid, 11, model.SourceTypeVipWatch, 100, 100, 0))

	cases := []struct {
		name               string
		page, size         int64
		wantPage, wantSize int64
		wantOffset         int64
	}{
		{"页宽超上限", 1, 100000, 1, 100, 0},
		{"页宽 0 折到上限", 1, 0, 1, 100, 0},
		{"页码 0 折到第一页", 0, 20, 1, 20, 0},
		{"深页保留 offset", 6, 20, 6, 20, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{
				Period: "202601", Page: c.page, Size: c.size,
			})
			mustNoErr(t, err)
			if reply.Page != c.wantPage || reply.Size != c.wantSize {
				t.Fatalf("回显分页错误：got page=%d size=%d want page=%d size=%d",
					reply.Page, reply.Size, c.wantPage, c.wantSize)
			}
			if w := db.window("fake:metrics.List"); w.offset != c.wantOffset || w.limit != c.wantSize {
				t.Fatalf("实际查询分页位错误：%+v want offset=%d limit=%d", w, c.wantOffset, c.wantSize)
			}
		})
	}
}

func TestListRevenueMetricsEmptyIsNonNilSlice(t *testing.T) {
	ctx, db := newTestSvc(t)
	reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Period: "202601", Mid: txMid})
	mustNoErr(t, err)
	if reply.Metrics == nil {
		t.Fatal("空台账必须回非 nil 空数组")
	}
	if len(reply.Metrics) != 0 || reply.Total != 0 {
		t.Fatalf("空台账结论错误：%+v", reply)
	}
	db.assertReads(t, 0, "fake:metrics.List", "fake:metrics.Count")
}

func TestListRevenueMetricsReadFailuresPropagateRaw(t *testing.T) {
	t.Run("列表读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		boom := errors.New("context deadline exceeded")
		db.readFailOn["fake:metrics.List"] = boom
		reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Period: "202601", Mid: txMid})
		if !errors.Is(err, boom) {
			t.Fatalf("必须原样上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("故障不得折叠成空台账（会显示成「本月还没有收益」）：%+v", reply)
		}
		db.assertReads(t, 0, "fake:metrics.List")
	})

	t.Run("计数读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		db.addMetric(summaryMetricSeed("202601", txMid, 11, model.SourceTypeVipWatch, 100, 100, 0))
		boom := errors.New("connection refused")
		db.readFailOn["fake:metrics.Count"] = boom
		reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Period: "202601", Mid: txMid})
		if !errors.Is(err, boom) {
			t.Fatalf("计数失败必须上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("计数失败不得回带行的半截结论：%+v", reply)
		}
		db.assertReads(t, 0, "fake:metrics.List", "fake:metrics.Count")
	})
}

// 现状哨兵（高危，登记 README 计量域）：只带 period 就能翻页拿到
// 全平台所有作者、所有内容的应计金额与来源明细（ListRevenueMetricsReq 无身份位）。
// 这一条与 GetRevenueSummary 的 mid 串味是同一根因：契约里不存在调用方身份。
func TestListRevenueMetricsPeriodOnlyExposesAllAuthors(t *testing.T) {
	ctx, db := newTestSvc(t)
	for _, mid := range []int64{100, 200, 300} {
		db.addMetric(summaryMetricSeed("202601", mid, mid, model.SourceTypeVipWatch, 7777, 7777, 0))
	}
	reply, err := runListRevenueMetrics(t, ctx, &rpc.ListRevenueMetricsReq{Period: "202601", Size: 100})
	mustNoErr(t, err)
	mustMetricSeq(t, metricKeySeq(reply.Metrics),
		"202601/100/100/1", "202601/200/200/1", "202601/300/300/1")
	for _, r := range reply.Metrics {
		if r.AmountMinor != 7777 || r.SourceDetail != "口径摘要" {
			t.Fatalf("现状：他人金额与来源明细整体外泄：%+v", r)
		}
	}
}

// 时钟护栏：CurrentPeriod 按服务端本地时钟出周期码，用例不得跟着午夜漂移。
// 这里钉的是「窗口边界与当前周期同源」：periodBack 的推导与 seed 都用同一个 cur。
func TestGetRevenueSummaryPeriodFollowsServiceClock(t *testing.T) {
	ctx, _ := newTestSvc(t)
	reply, err := runGetRevenueSummary(t, ctx, &rpc.GetRevenueSummaryReq{Mid: txMid})
	mustNoErr(t, err)
	want := fmt.Sprintf("%04d%02d", time.Now().Year(), int(time.Now().Month()))
	if reply.CurrentPeriod != want || reply.CurrentPeriod != model.CurrentPeriod() {
		t.Fatalf("当前周期口径错误：got=%s want=%s", reply.CurrentPeriod, want)
	}
}
