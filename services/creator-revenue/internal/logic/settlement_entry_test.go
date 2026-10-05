package logic

// 本文件锁两个出单/确认入口（GenerateSettlement / ConfirmSettlement）的判定口径：
// 入口闸门（不得晚于当前周期、危险位必须带原因、mid=0 与 mid>0 的语义分叉）、
// 全量模式的批量上限与「只回前若干条」的回显形状、逐作者事务的失败隔离边界，
// 以及「确认 ≠ 打款」在数据上的体现（确认只推 state，payout_state 一个字都不动，
// 确认原因/幂等键当前无处落库——本服务没有结算确认审计表）。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"
)

// genInput 造一条合法出单请求（周期取已收官的历史周期，免得用例跟着时钟漂移）。
func genInput(mut func(*rpc.GenerateSettlementReq)) *rpc.GenerateSettlementReq {
	in := &rpc.GenerateSettlementReq{
		Period: txPeriod, Mid: txMid, Operator: txOperator,
		RequestId: "req-gen", Reason: "月度出单复核",
	}
	if mut != nil {
		mut(in)
	}
	return in
}

func runGenerate(t *testing.T, ctx *svc.ServiceContext, in *rpc.GenerateSettlementReq,
) (*rpc.GenerateSettlementReply, error) {
	t.Helper()
	return NewGenerateSettlementLogic(context.Background(), ctx).GenerateSettlement(in)
}

func confirmInput(mut func(*rpc.ConfirmSettlementReq)) *rpc.ConfirmSettlementReq {
	in := &rpc.ConfirmSettlementReq{
		SettlementNos: []string{"CRS202601-100-1"},
		Operator:      txOperator, RequestId: "req-confirm", Reason: "金额已复核",
	}
	if mut != nil {
		mut(in)
	}
	return in
}

func runConfirm(t *testing.T, ctx *svc.ServiceContext, in *rpc.ConfirmSettlementReq,
) (*rpc.ConfirmSettlementReply, error) {
	t.Helper()
	return NewConfirmSettlementLogic(context.Background(), ctx).ConfirmSettlement(in)
}

// seedSettleable 放齐一个「可出单作者」：ENROLLED + 生效规则 + 一行未触顶台账。
func seedSettleable(db *fakeDB, period string, mid, capped int64) {
	code := "R" + strconv.FormatInt(mid, 10)
	if db.ruleByCode(code) == nil {
		db.addRule(ruleWithCurrency(code, model.SourceTypeVipWatch, "CNY"))
	}
	if db.enrollmentByMid(mid) == nil {
		seedEnrolled(db, mid, model.EnrollmentStateEnrolled)
	}
	db.addLedger(period, mid, ledgerSeed{
		Aid: mid * 1000, SourceType: model.SourceTypeVipWatch, Rule: code,
		Quantity: capped, Amount: capped, Capped: capped,
	})
}

// seedDraft 放一张 DRAFT 在效单（确认路径的种子）。
func seedDraft(db *fakeDB, no string, mid int64) *model.Settlement {
	return db.addSettlement(&model.Settlement{
		SettlementNo: no, Period: txPeriod, Mid: mid, AmountMinor: 1200,
		CapAppliedMinor: 300, Currency: "CNY", MetricCount: 2,
		State: model.SettlementStateDraft, PayoutState: model.PayoutStateNotPayable,
		RequestId: "seed-" + no, VoidSeq: 0,
	})
}

func mustNos(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("failed_nos 期望 %v，实际 %v", want, got)
	}
}

// ---------------------------------------------------------------- GenerateSettlement 入口闸门

func TestGenerateSettlementEntryGates(t *testing.T) {
	maxScoped := model.MaxRequestIDBytes - settleKeySuffixBytes
	longRequestID := strings.Repeat("r", maxScoped+1)
	longReason := strings.Repeat("理", model.MaxReasonBytes+1)
	longOperator := strings.Repeat("o", model.MaxOperatorBytes+1)
	nextPeriod := time.Now().AddDate(0, 1, 0).Format("200601")

	cases := []struct {
		name   string
		in     *rpc.GenerateSettlementReq
		target error
		want   string // 错误文案里的必备片段（空表示不校验）
	}{
		{"周期格式非法", genInput(func(i *rpc.GenerateSettlementReq) { i.Period = "20261" }), model.ErrInvalidPeriod, ""},
		{"月份越界", genInput(func(i *rpc.GenerateSettlementReq) { i.Period = "202613" }), model.ErrInvalidPeriod, ""},
		{"未来周期不出单", genInput(func(i *rpc.GenerateSettlementReq) { i.Period = nextPeriod }), model.ErrFuturePeriod, "当前周期"},
		{"缺操作人", genInput(func(i *rpc.GenerateSettlementReq) { i.Operator = "  " }), model.ErrOperatorRequired, ""},
		{"操作人超列宽", genInput(func(i *rpc.GenerateSettlementReq) { i.Operator = longOperator }), model.ErrTextTooLong, "operator"},
		{"缺幂等键", genInput(func(i *rpc.GenerateSettlementReq) { i.RequestId = "" }), model.ErrRequestIDRequired, ""},
		{"幂等键放不下行级后缀", genInput(func(i *rpc.GenerateSettlementReq) { i.RequestId = longRequestID }), model.ErrTextTooLong, "request_id"},
		{"危险位没有原因", genInput(func(i *rpc.GenerateSettlementReq) { i.ForceVoidConfirmed = true; i.Reason = " " }), model.ErrForceVoidReasonRequired, ""},
		{"原因超列宽", genInput(func(i *rpc.GenerateSettlementReq) { i.Reason = longReason }), model.ErrTextTooLong, "reason"},
		{"mid 负数", genInput(func(i *rpc.GenerateSettlementReq) { i.Mid = -1 }), model.ErrInvalidMid, "0 表示该周期全量"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			reply, err := runGenerate(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			// 闸门必须发生在开事务之前：非法请求不该烧掉一次事务。
			if len(db.calls) != 0 {
				t.Fatalf("闸门未过却已执行语句：%v", db.calls)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里缺少 %q：%v", c.want, err)
			}
		})
	}
}

func TestGenerateSettlementRequestIdWidthBoundaryIsAllowed(t *testing.T) {
	ctx, _ := newTestSvc(t)
	// 刚好留出 27 字节后缀空间的父键必须放行：拒在它前面是「宁可少一寸」的过度收紧。
	in := genInput(func(i *rpc.GenerateSettlementReq) {
		i.RequestId = strings.Repeat("r", model.MaxRequestIDBytes-settleKeySuffixBytes)
	})
	_, err := runGenerate(t, ctx, in)
	if errors.Is(err, model.ErrTextTooLong) {
		t.Fatalf("边界宽度被判超长：%v", err)
	}
	// 放行后走到闸门分支（该 mid 从未参加计划），而不是幂等键问题。
	mustErrIs(t, err, model.ErrEnrollmentNotFound)
}

// ---------------------------------------------------------------- GenerateSettlement 单作者出单

func TestGenerateSettlementSingleMidWritesDraftWithItems(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedSettleable(db, txPeriod, txMid, 800)

	reply, err := runGenerate(t, ctx, genInput(nil))
	mustNoErr(t, err)
	if reply.Generated != 1 || reply.Duplicated || reply.Truncated {
		t.Fatalf("首单应记 generated=1：%+v", reply)
	}
	if len(reply.Settlements) != 1 {
		t.Fatalf("单作者模式必须回显这张单：%v", reply.Settlements)
	}
	info := reply.Settlements[0]
	wantNo := model.BuildSettlementNo(txPeriod, txMid, 1)
	if info.SettlementNo != wantNo || info.AmountMinor != 800 || info.Currency != "CNY" {
		t.Fatalf("回显结论错误：%+v", info)
	}
	// 出金边界：入口回给调用方的那张单也必须写着「不可出金」。
	if info.State != rpc.SettlementState_SETTLEMENT_STATE_DRAFT ||
		info.PayoutState != rpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE {
		t.Fatalf("新单状态/出金态错误：state=%v payout=%v", info.State, info.PayoutState)
	}
	row := db.settlementByNo(wantNo)
	if row.RequestId != "req-gen#"+txPeriod+"#"+strconv.FormatInt(txMid, 10) {
		t.Fatalf("行级幂等键未落库：%q", row.RequestId)
	}
	if items := db.itemsOf(wantNo); len(items) != 1 || items[0].AmountMinor != 800 {
		t.Fatalf("分项未随主体落库：%+v", items)
	}
}

func TestGenerateSettlementSameRequestIdIsReplayNotSecondAccrual(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedSettleable(db, txPeriod, txMid, 800)

	first, err := runGenerate(t, ctx, genInput(nil))
	mustNoErr(t, err)
	// 台账又被写了 100（并发补录），但同一 request_id 重放必须回首次那张单、金额不变。
	db.addLedger(txPeriod, txMid, ledgerSeed{
		Aid: 999, SourceType: model.SourceTypeVipWatch, Rule: "R" + strconv.FormatInt(txMid, 10),
		Quantity: 100, Amount: 100, Capped: 100,
	})
	second, err := runGenerate(t, ctx, genInput(nil))
	mustNoErr(t, err)
	if second.Generated != 0 || !second.Duplicated {
		t.Fatalf("重放不得计入 generated：%+v", second)
	}
	if second.Settlements[0].AmountMinor != first.Settlements[0].AmountMinor {
		t.Fatalf("重放金额被改写：%v vs %v", second.Settlements[0].AmountMinor, first.Settlements[0].AmountMinor)
	}
	if len(db.settlements) != 1 {
		t.Fatalf("一次 request_id 只能出一张单，当前 %d 张", len(db.settlements))
	}
	// 换个 request_id 才是真的重算（同一 DRAFT 单就地覆盖，仍只有一张在效单）。
	third, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.RequestId = "req-gen-2" }))
	mustNoErr(t, err)
	if third.Generated != 1 || len(db.settlements) != 1 {
		t.Fatalf("新 request_id 应就地重算：%+v / %d 张", third, len(db.settlements))
	}
	if third.Settlements[0].AmountMinor != 900 {
		t.Fatalf("重算未反映新台账：%+v", third.Settlements[0])
	}
}

func TestGenerateSettlementConfirmedNeedsForceAtEntry(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedSettleable(db, txPeriod, txMid, 800)
	confirmed := db.addSettlement(&model.Settlement{
		SettlementNo: model.BuildSettlementNo(txPeriod, txMid, 1), Period: txPeriod, Mid: txMid,
		AmountMinor: 66000, Currency: "CNY", MetricCount: 1,
		State: model.SettlementStateConfirmed, PayoutState: model.PayoutStateNotPayable,
		ConfirmedBy: "ops-01", ConfirmedAt: fakeNow() - 10, RequestId: "first-req", VoidSeq: 0,
	})

	reply, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.RequestId = "req-x" }))
	mustNoErr(t, err)
	if reply.Generated != 0 || !reply.Duplicated {
		t.Fatalf("已确认单未强制时应判 duplicated：%+v", reply)
	}
	if db.settlementByNo(confirmed.SettlementNo).AmountMinor != 66000 {
		t.Fatalf("已确认金额必须冻结")
	}

	forced, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) {
		i.RequestId = "req-force"
		i.ForceVoidConfirmed = true
		i.Reason = "台账更正后重算"
	}))
	mustNoErr(t, err)
	if forced.Generated != 1 || forced.Duplicated {
		t.Fatalf("强制重算应计入 generated：%+v", forced)
	}
	if forced.Settlements[0].SettlementNo == confirmed.SettlementNo {
		t.Fatalf("强制重算必须另起单号：%+v", forced.Settlements[0])
	}
	// 旧单的对外口径改不了，只能作废留痕；且作废也不产生任何出金语义。
	old := db.settlementByNo(confirmed.SettlementNo)
	if old.State != model.SettlementStateVoided || old.VoidReason != "台账更正后重算" {
		t.Fatalf("旧确认单未留作废证据：%+v", old)
	}
	if old.PayoutState != model.PayoutStateNotPayable {
		t.Fatalf("payout_state 被改写：%d", old.PayoutState)
	}
}

// ---------------------------------------------------------------- GenerateSettlement 全量模式

func TestGenerateSettlementFullBatchRespectsMaxBatchAndProbe(t *testing.T) {
	ctx, db := newTestSvc(t)
	ctx.Config.CreatorRevenue.GenerateMaxBatch = 1
	seedSettleable(db, txPeriod, 100, 800)
	seedSettleable(db, txPeriod, 101, 900)
	f := settleable(ctx, db, 100, 101)

	reply, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.Mid = 0 }))
	mustNoErr(t, err)
	if !reply.Truncated || reply.Generated != 1 {
		t.Fatalf("超出单批上限必须截断：%+v", reply)
	}
	if len(db.settlements) != 1 || db.settlements[0].Mid != 100 {
		t.Fatalf("截断后只能处理前 N 条：%+v", db.settlements)
	}
	// 候选扫描只多取一条用于判定「还有没有」，不为了多写一张单。
	if f.limitSeen != 2 {
		t.Fatalf("候选扫描 limit 应为 GenerateMaxBatch+1=2，实际 %d", f.limitSeen)
	}
}

func TestGenerateSettlementFallsBackToDefaultMaxBatchWhenUnset(t *testing.T) {
	ctx, db := newTestSvc(t)
	ctx.Config.CreatorRevenue.GenerateMaxBatch = 0
	f := settleable(ctx, db)

	// 漏配不能让「无界出单」发生：兜底成 defaultGenerateMaxBatch。
	_, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.Mid = 0 }))
	mustNoErr(t, err)
	if f.limitSeen != defaultGenerateMaxBatch+1 {
		t.Fatalf("兜底批上限未生效，limit=%d", f.limitSeen)
	}
}

func TestGenerateSettlementEchoesOnlyFirstSettlements(t *testing.T) {
	ctx, db := newTestSvc(t)
	const n = settlementEchoLimit + 3
	mids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		mid := int64(200 + i)
		seedSettleable(db, txPeriod, mid, int64(100+i))
		mids = append(mids, mid)
	}
	settleable(ctx, db, mids...)

	reply, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.Mid = 0 }))
	mustNoErr(t, err)
	if reply.Generated != int64(n) {
		t.Fatalf("整批都应出单：%+v", reply)
	}
	if reply.Truncated || reply.Duplicated {
		t.Fatalf("未超上限不该报截断/重复：%+v", reply)
	}
	// proto 口径「全量时只回前若干条，完整结果查台账」：落库 n 张，回显封顶 n 里的 50 张。
	if len(reply.Settlements) != settlementEchoLimit {
		t.Fatalf("回显条数应为 %d，实际 %d", settlementEchoLimit, len(reply.Settlements))
	}
	if len(db.settlements) != n {
		t.Fatalf("回显截断不得影响落库，当前 %d 张", len(db.settlements))
	}
	if reply.Settlements[0].Mid != 200 {
		t.Fatalf("回显应按候选升序：%+v", reply.Settlements[0])
	}
}

func TestGenerateSettlementEmptyCandidatesDiffersFromEmptyLedger(t *testing.T) {
	t.Run("全量模式扫到空候选是合法结论", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		settleable(ctx, db)
		reply, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.Mid = 0 }))
		mustNoErr(t, err)
		if reply.Generated != 0 || reply.Duplicated || reply.Truncated {
			t.Fatalf("空候选应安静返回 0 单：%+v", reply)
		}
		if reply.Settlements == nil {
			t.Fatal("回显必须是空数组而不是 nil，网关要能区分「没有」与「没查」")
		}
		if len(db.settlements) != 0 {
			t.Fatalf("没有候选却写了单：%+v", db.settlements)
		}
	})
	t.Run("单作者模式没有台账必须报错", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		seedEnrolled(db, txMid, model.EnrollmentStateEnrolled)
		// 绝不能出 0 元单冒充「这个月已结清」。
		_, err := runGenerate(t, ctx, genInput(nil))
		mustErrIs(t, err, model.ErrNoMetricsToSettle)
		if len(db.settlements) != 0 {
			t.Fatalf("不该有结算单产出：%+v", db.settlements)
		}
	})
}

func TestGenerateSettlementCandidateScanFailurePropagates(t *testing.T) {
	ctx, db := newTestSvc(t)
	settleable(ctx, db)
	f := ctx.Metrics.(*fakeMetrics)
	f.midsE = errors.New("scan down")
	_, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.Mid = 0 }))
	if err == nil || !strings.Contains(err.Error(), "scan down") {
		t.Fatalf("候选扫描故障必须上抛，实际 %v", err)
	}
}

// TestGenerateSettlementPerAuthorIsolation 锁「每个作者一个事务」的边界：
// 只有「这个人本就不该出单」的合法结论可以跳过，真故障/脏数据必须整体失败，
// 否则运营会把「少出一个人」当成「这个月已经出完单了」。
func TestGenerateSettlementPerAuthorIsolation(t *testing.T) {
	t.Run("状态并发改动只跳过该作者", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		seedSettleable(db, txPeriod, 100, 800)
		seedEnrolled(db, 101, model.EnrollmentStateLeft)      // 扫描快照之后退出
		seedEnrolled(db, 102, model.EnrollmentStateSuspended) // 扫描快照之后被暂停
		db.addLedger(txPeriod, 101, ledgerSeed{Aid: 1, SourceType: model.SourceTypeVipWatch, Rule: "RX", Quantity: 1, Amount: 1, Capped: 1})
		db.addLedger(txPeriod, 102, ledgerSeed{Aid: 2, SourceType: model.SourceTypeVipWatch, Rule: "RX", Quantity: 1, Amount: 1, Capped: 1})
		settleable(ctx, db, 100, 101, 102)

		reply, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.Mid = 0 }))
		mustNoErr(t, err)
		if reply.Generated != 1 || !reply.Duplicated {
			t.Fatalf("跳过项不该算成功出单，且要留下 duplicated 信号：%+v", reply)
		}
		if len(db.settlements) != 1 || db.settlements[0].Mid != 100 {
			t.Fatalf("只应给仍在效的作者出单：%+v", db.settlements)
		}
	})
	t.Run("脏数据必须让整批失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		seedSettleable(db, txPeriod, 100, 800)
		seedEnrolled(db, 101, model.EnrollmentStateEnrolled)
		// 引用了不存在的规则行：不是「这个人不该出单」，是数据被改坏。
		db.addLedger(txPeriod, 101, ledgerSeed{Aid: 7, SourceType: model.SourceTypeVipWatch, Rule: "R_ghost", Quantity: 1, Amount: 1, Capped: 1})
		settleable(ctx, db, 100, 101)

		_, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.Mid = 0 }))
		mustErrIs(t, err, model.ErrRuleNotFound)
		if len(db.settlements) != 1 || db.settlements[0].Mid != 100 {
			t.Fatalf("逐作者事务：前一个人已提交的单不该被回滚，也不该有第二个人的半张单：%+v", db.settlements)
		}
	})
	t.Run("单作者模式同样错误直接上抛", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		seedEnrolled(db, txMid, model.EnrollmentStateLeft)
		_, err := runGenerate(t, ctx, genInput(func(i *rpc.GenerateSettlementReq) { i.Mid = txMid }))
		mustErrIs(t, err, model.ErrNotEnrolled)
	})
}

// ---------------------------------------------------------------- ConfirmSettlement 入口闸门

func TestConfirmSettlementEntryGates(t *testing.T) {
	tooMany := make([]string, 0, model.MaxConfirmBatch+1)
	for i := 0; i <= model.MaxConfirmBatch; i++ {
		tooMany = append(tooMany, "CRS202601-100-"+strconv.Itoa(i))
	}
	cases := []struct {
		name   string
		in     *rpc.ConfirmSettlementReq
		target error
		want   string
	}{
		{"空列表拒绝", confirmInput(func(i *rpc.ConfirmSettlementReq) { i.SettlementNos = nil }), model.ErrSettlementNosRequired, ""},
		{"全空白也算空", confirmInput(func(i *rpc.ConfirmSettlementReq) { i.SettlementNos = []string{"", "  "} }), model.ErrSettlementNosRequired, "第 0 个单号为空"},
		{"超过批量上限", confirmInput(func(i *rpc.ConfirmSettlementReq) { i.SettlementNos = tooMany }), model.ErrBatchTooLarge, "单次最多 200"},
		{"单号超列宽", confirmInput(func(i *rpc.ConfirmSettlementReq) {
			i.SettlementNos = []string{strings.Repeat("N", model.MaxSettlementNoBytes+1)}
		}), model.ErrTextTooLong, "settlement_nos"},
		{"缺操作人", confirmInput(func(i *rpc.ConfirmSettlementReq) { i.Operator = "" }), model.ErrOperatorRequired, ""},
		{"确认必须给原因", confirmInput(func(i *rpc.ConfirmSettlementReq) { i.Reason = "  " }), model.ErrReasonRequired, ""},
		{"缺幂等键", confirmInput(func(i *rpc.ConfirmSettlementReq) { i.RequestId = "" }), model.ErrRequestIDRequired, ""},
		{"幂等键超列宽", confirmInput(func(i *rpc.ConfirmSettlementReq) {
			i.RequestId = strings.Repeat("r", model.MaxRequestIDBytes+1)
		}), model.ErrTextTooLong, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			// 前置一张 DRAFT 单：闸门失败时它必须一个字段都没变。
			draft := seedDraft(db, "CRS202601-100-1", txMid)
			reply, err := runConfirm(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			if len(db.calls) != 0 {
				t.Fatalf("闸门未过却已执行语句：%v", db.calls)
			}
			after := db.settlementByNo(draft.SettlementNo)
			if after.State != model.SettlementStateDraft || after.ConfirmedBy != "" {
				t.Fatalf("闸门未过却改了单：%+v", after)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里缺少 %q：%v", c.want, err)
			}
		})
	}
}

// ---------------------------------------------------------------- ConfirmSettlement 状态机

func TestConfirmSettlementFreezesAmountButNeverPayout(t *testing.T) {
	ctx, db := newTestSvc(t)
	draft := seedDraft(db, "CRS202601-100-1", txMid)

	reply, err := runConfirm(t, ctx, confirmInput(nil))
	mustNoErr(t, err)
	if reply.Confirmed != 1 || len(reply.FailedNos) != 0 {
		t.Fatalf("DRAFT 单应确认成功：%+v", reply)
	}
	row := db.settlementByNo(draft.SettlementNo)
	if row.State != model.SettlementStateConfirmed || row.ConfirmedBy != txOperator || row.ConfirmedAt == 0 {
		t.Fatalf("确认结论未落库：%+v", row)
	}
	// 「确认 ≠ 打款」：本服务没有任何出金路径，payout_state 必须停在 NOT_PAYABLE。
	if row.PayoutState != model.PayoutStateNotPayable {
		t.Fatalf("payout_state 被确认改写：%d", row.PayoutState)
	}
	if row.AmountMinor != draft.AmountMinor || row.CapAppliedMinor != draft.CapAppliedMinor {
		t.Fatalf("确认不得改金额：%+v vs 种子 %+v", row, draft)
	}
}

func TestConfirmSettlementNonDraftAndUnknownGoToFailedNos(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addSettlement(&model.Settlement{
		SettlementNo: "CRS202601-101-1", Period: txPeriod, Mid: 101, AmountMinor: 10,
		Currency: "CNY", MetricCount: 1, State: model.SettlementStateConfirmed,
		PayoutState: model.PayoutStateNotPayable, ConfirmedBy: "ops-other", ConfirmedAt: fakeNow() - 5,
		RequestId: "r101", VoidSeq: 0,
	})
	voided := db.addSettlement(&model.Settlement{
		SettlementNo: "CRS202601-102-1", Period: txPeriod, Mid: 102, AmountMinor: 10,
		Currency: "CNY", MetricCount: 1, State: model.SettlementStateVoided,
		PayoutState: model.PayoutStateNotPayable, VoidReason: "重算作废旧单",
		RequestId: "r102", VoidSeq: 9,
	})
	draft := seedDraft(db, "CRS202601-100-1", txMid)

	reply, err := runConfirm(t, ctx, confirmInput(func(i *rpc.ConfirmSettlementReq) {
		i.SettlementNos = []string{"CRS202601-100-1", "CRS202601-101-1", "CRS202601-102-1", "CRS202601-999-1"}
	}))
	mustNoErr(t, err)
	if reply.Confirmed != 1 {
		t.Fatalf("只有 DRAFT 那张该成功：%+v", reply)
	}
	mustNos(t, reply.FailedNos, "CRS202601-101-1", "CRS202601-102-1", "CRS202601-999-1")
	if db.settlementByNo(draft.SettlementNo).State != model.SettlementStateConfirmed {
		t.Fatal("同批里合法的单必须成功")
	}
	if db.settlementByNo(voided.SettlementNo).State != model.SettlementStateVoided {
		t.Fatal("已作废单不能被确认复活")
	}
}

func TestConfirmSettlementSameOperatorReplayIsNotFailure(t *testing.T) {
	ctx, db := newTestSvc(t)
	confirmed := db.addSettlement(&model.Settlement{
		SettlementNo: "CRS202601-100-1", Period: txPeriod, Mid: txMid, AmountMinor: 10,
		Currency: "CNY", MetricCount: 1, State: model.SettlementStateConfirmed,
		PayoutState: model.PayoutStateNotPayable, ConfirmedBy: txOperator, ConfirmedAt: 1700000000,
		RequestId: "r", VoidSeq: 0,
	})

	reply, err := runConfirm(t, ctx, confirmInput(nil))
	mustNoErr(t, err)
	if reply.Confirmed != 1 || len(reply.FailedNos) != 0 {
		t.Fatalf("同一人重复确认（超时重试）不该被显示成失败：%+v", reply)
	}
	after := db.settlementByNo(confirmed.SettlementNo)
	if after.ConfirmedAt != 1700000000 {
		t.Fatalf("重放不得刷新确认时间：%d", after.ConfirmedAt)
	}
}

func TestConfirmSettlementDeduplicatesSameNoWithinBatch(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedDraft(db, "CRS202601-100-1", txMid)

	reply, err := runConfirm(t, ctx, confirmInput(func(i *rpc.ConfirmSettlementReq) {
		i.SettlementNos = []string{"CRS202601-100-1", "  CRS202601-100-1  ", "CRS202601-100-1"}
	}))
	mustNoErr(t, err)
	// 不去重就会把一张单计成三张：reply.confirmed 比真实冻结的单数多。
	if reply.Confirmed != 1 {
		t.Fatalf("同一单号重复提交只能冻结一次：%+v", reply)
	}
	if got := db.countCalls("upd:cr_settlement.confirm"); got != 1 {
		t.Fatalf("去重后只该发一条 CAS，实际 %d 条：%v", got, db.calls)
	}
}

// TestConfirmSettlementCasMissFailsOnlyThatNo 锁 CAS 语义：
// cr_* 全部走 RowsAffected（DSN 禁 clientFoundRows），条件不命中就是 0 行 → 该单进 failed_nos，
// 但同批其它单照常处理，不能因为一个人不命中就整批回滚。
func TestConfirmSettlementCasMissFailsOnlyThatNo(t *testing.T) {
	ctx, db := newTestSvc(t)
	draft := seedDraft(db, "CRS202601-100-1", txMid)
	db.noRowsFor["upd:cr_settlement.confirm"] = true

	reply, err := runConfirm(t, ctx, confirmInput(nil))
	mustNoErr(t, err)
	if reply.Confirmed != 0 {
		t.Fatalf("0 行受影响不能算确认成功：%+v", reply)
	}
	mustNos(t, reply.FailedNos, "CRS202601-100-1")
	if db.settlementByNo(draft.SettlementNo).State != model.SettlementStateDraft {
		t.Fatal("未命中的单必须保持 DRAFT")
	}
}

func TestConfirmSettlementDbFaultRollsBackWholeBatch(t *testing.T) {
	ctx, db := newTestSvc(t)
	first := seedDraft(db, "CRS202601-100-1", txMid)
	second := seedDraft(db, "CRS202601-101-1", 101)
	db.failOn["upd:cr_settlement.confirm"] = errors.New("deadlock")

	reply, err := runConfirm(t, ctx, confirmInput(func(i *rpc.ConfirmSettlementReq) {
		i.SettlementNos = []string{"CRS202601-100-1", "CRS202601-101-1"}
	}))
	if reply != nil || err == nil {
		t.Fatalf("DB 故障必须整批失败，实际 reply=%+v err=%v", reply, err)
	}
	if !strings.Contains(err.Error(), "deadlock") {
		t.Fatalf("故障原因要能看见：%v", err)
	}
	for _, no := range []string{first.SettlementNo, second.SettlementNo} {
		row := db.settlementByNo(no)
		if row.State != model.SettlementStateDraft || row.ConfirmedBy != "" {
			t.Fatalf("逐单吞掉错误会让运营以为「这批都冻结了」：%+v", row)
		}
	}
}

// TestConfirmSettlementWritesNoAuditRow 记录代码事实（交付报告里的已知缺口）：
// 本服务没有结算确认审计表，reason 与 request_id 只进服务日志，
// 一旦有人新增 DDL/写入路径，这条断言会立刻失效并提醒补文档。
func TestConfirmSettlementWritesNoAuditRow(t *testing.T) {
	ctx, db := newTestSvc(t)
	draft := seedDraft(db, "CRS202601-100-1", txMid)

	_, err := runConfirm(t, ctx, confirmInput(nil))
	mustNoErr(t, err)
	if got := db.countCalls("ins:"); got != 0 {
		t.Fatalf("确认路径当前只该 UPDATE，实际写了 %d 行 INSERT：%v", got, db.calls)
	}
	if len(db.ruleLogs) != 0 || len(db.metricLogs) != 0 {
		t.Fatalf("确认不该借用其它台账留痕：%d/%d", len(db.ruleLogs), len(db.metricLogs))
	}
	row := db.settlementByNo(draft.SettlementNo)
	if row.RequestId != draft.RequestId || row.VoidReason != draft.VoidReason {
		t.Fatalf("确认原因被写进了 cr_settlement 的既有列，口径变了：%+v", row)
	}
}

// ---------------------------------------------------------------- normalizeSettlementNos

func TestNormalizeSettlementNosTrimsAndKeepsOrder(t *testing.T) {
	got, err := normalizeSettlementNos([]string{" B ", "A", "B", "", " "})
	if got != nil || err == nil {
		t.Fatalf("含空白项必须整批拒绝：%v %v", got, err)
	}
	mustErrIs(t, err, model.ErrSettlementNosRequired)
	if !strings.Contains(err.Error(), "第 3 个单号为空") {
		t.Fatalf("报错要指出是第几个：%v", err)
	}
	out, err := normalizeSettlementNos([]string{" B ", "A", "B"})
	mustNoErr(t, err)
	mustNos(t, out, "B", "A")
}
