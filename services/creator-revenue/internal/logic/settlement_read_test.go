package logic

// 本文件锁两个「结算台账查询」读入口（GetSettlement / ListSettlements）的判定口径：
//   - 归属校验必须逐条钉死：mid>0 时不一致回 ErrForbidden 且在分项表之前停下
//     （读轨迹逐项等长才判得出「停在哪」），mid=0 时**当前完全没有归属校验** ——
//     这是资金台账最容易出事的地方，原样钉住并登记 README 结算域高危缺口；
//   - 「查不到」与「查不了」必须分开：DB 故障一律原始错误上抛，绝不折成 found=false
//     （AGENTS.md §9 禁止伪成功：调用方会把故障读成「这张单不存在」然后去重发结算）；
//   - VOIDED 单照样查得到且带 void_reason：作废是审计证据不是删除；
//     state=0 的列表默认**含 VOIDED**，复核必须看得见作废历史；
//   - 出金边界（AGENTS.md §1）：读侧只回 payout_state 的原值，一个字节都不改写，
//     也不执行任何写语句 —— 本服务没有任何出金/提现/打款入口；
//   - 排序事实源：分项是 model/cr_settlement_item.go:85 的 `ORDER BY source_type ASC`，
//     列表是 model/cr_settlement.go:247 的 `ORDER BY settlement_id DESC`；
//     fake 照抄这两条 ORDER BY，种子按相反顺序插入，因此钉的是
//     「logic 原序投影、不自己重排、列表不额外去读分项表」；
//   - 现状缺陷原样钉住 + README 登记：ListSettlements 的状态越界复用了
//     ErrInvalidRuleState（helpers.go:97），错误码文案指向的是规则而不是结算单。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"
)

func runGetSettlement(t *testing.T, ctx *svc.ServiceContext, in *rpc.GetSettlementReq) (*rpc.GetSettlementReply, error) {
	t.Helper()
	return NewGetSettlementLogic(context.Background(), ctx).GetSettlement(in)
}

func runListSettlements(t *testing.T, ctx *svc.ServiceContext, in *rpc.ListSettlementsReq) (*rpc.ListSettlementsReply, error) {
	t.Helper()
	return NewListSettlementsLogic(context.Background(), ctx).ListSettlements(in)
}

// settleSeed 造一张结算单。金额/单号/操作人全是明显假值，不含任何真实账号。
func settleSeed(mut func(*model.Settlement)) *model.Settlement {
	s := &model.Settlement{
		SettlementId: 0, SettlementNo: "CRS202601-100-1", Period: txPeriod, Mid: txMid,
		AmountMinor: 123_456, CapAppliedMinor: 6_544, Currency: "CNY", MetricCount: 3,
		State: model.SettlementStateConfirmed, PayoutState: model.PayoutStateNotPayable,
		ConfirmedBy: txOperator, ConfirmedAt: 1_710_000_000,
		RequestId: "req-seed", VoidSeq: 0,
		Ctime: 1_700_000_000, Mtime: 1_710_000_000,
	}
	if mut != nil {
		mut(s)
	}
	return s
}

// addItem 放一行结算分项（读侧只按 settlement_no 取，ItemId 手工给以免和自增序列混）。
func addItem(db *fakeDB, itemID int64, no string, sourceType int32, rule string, qty, amount int64) {
	db.items = append(db.items, &model.SettlementItem{
		ItemId: itemID, SettlementNo: no, SourceType: sourceType,
		RuleCode: rule, Quantity: qty, AmountMinor: amount, Ctime: 1_700_000_000,
	})
}

func mustNoSeq(t *testing.T, rows []*rpc.SettlementInfo, want ...string) {
	t.Helper()
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.SettlementNo)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("settlement_no 序列不符：期望 %v，实际 %v", want, got)
	}
}

func mustItemSeq(t *testing.T, rows []*rpc.SettlementItem, want ...int32) {
	t.Helper()
	got := make([]int32, 0, len(rows))
	for _, it := range rows {
		got = append(got, int32(it.SourceType))
	}
	if len(got) != len(want) {
		t.Fatalf("分项 source_type 序列不符：期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个分项不符：期望 %v，实际 %v", i, want, got)
		}
	}
}

// ---------------------------------------------------------------- GetSettlement 入口闸门

// 闸门必须发生在任何数据访问之前：非法单号不该烧掉一次查库。
func TestGetSettlementGatesTouchNoData(t *testing.T) {
	tooLong := strings.Repeat("9", model.MaxSettlementNoBytes+1)

	cases := []struct {
		name   string
		in     *rpc.GetSettlementReq
		target error
		want   string
	}{
		{"单号为空", &rpc.GetSettlementReq{}, model.ErrSettlementNosRequired, "settlement_no 不能为空"},
		{"单号只有空白", &rpc.GetSettlementReq{SettlementNo: "   "}, model.ErrSettlementNosRequired, ""},
		{"单号超列宽", &rpc.GetSettlementReq{SettlementNo: tooLong}, model.ErrTextTooLong, "settlement_no"},
		{"mid 负数", &rpc.GetSettlementReq{SettlementNo: "CRS202601-100-1", Mid: -1},
			model.ErrInvalidMid, "0 表示不校验归属"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			db.addSettlement(settleSeed(nil))
			reply, err := runGetSettlement(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			if len(db.reads) != 0 || len(db.calls) != 0 {
				t.Fatalf("闸门未过却已访问数据：%v / %v", db.reads, db.calls)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里缺少 %q：%v", c.want, err)
			}
		})
	}
}

// 列宽是「超过」才拒：刚好 48 字节的单号必须放行（宁可少一寸也是错）。
func TestGetSettlementNoWidthBoundaryIsAllowed(t *testing.T) {
	ctx, db := newTestSvc(t)
	no := strings.Repeat("9", model.MaxSettlementNoBytes)
	db.addSettlement(settleSeed(func(s *model.Settlement) { s.SettlementNo = no }))

	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: no})
	mustNoErr(t, err)
	if !reply.Found || reply.Settlement == nil {
		t.Fatalf("刚好列宽的单号查不到：%+v", reply)
	}
	if reply.Settlement.SettlementNo != no {
		t.Fatalf("单号被改写了：len=%d %q", len(reply.Settlement.SettlementNo), reply.Settlement.SettlementNo)
	}
	db.assertReads(t, 0, "fake:settlements.FindOneByNo", "fake:settlementItems.ListByNo")
}

// 单号必填先于单号宽度、宽度先于 mid 符号：三条守卫的先后必须可判别。
// 任何一条被挪到前面，回的错误码就会变，这条用例即红。
func TestGetSettlementGuardOrderIsDiscriminable(t *testing.T) {
	tooLong := strings.Repeat("9", model.MaxSettlementNoBytes+1)

	ctx, db := newTestSvc(t)
	_, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: "  ", Mid: -1})
	mustErrIs(t, err, model.ErrSettlementNosRequired)
	if len(db.reads) != 0 {
		t.Fatalf("必填闸门却访问了数据：%v", db.reads)
	}

	ctx2, db2 := newTestSvc(t)
	_, err = runGetSettlement(t, ctx2, &rpc.GetSettlementReq{SettlementNo: tooLong, Mid: -1})
	mustErrIs(t, err, model.ErrTextTooLong)
	if len(db2.reads) != 0 {
		t.Fatalf("宽度闸门却访问了数据：%v", db2.reads)
	}
}

// Ready 闸门在入参校验之前（与 GetEnrollment 同口径）：
// 未配置库时必须回可诊断错误，而不是「单号为空」这种指向调用方的文案。
func TestGetSettlementReadyGatePrecedesValidation(t *testing.T) {
	ctx, db := newTestSvc(t)
	ctx.Settlements = nil
	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{})
	mustErrIs(t, err, model.ErrDBNotConfigured)
	if reply != nil {
		t.Fatalf("未配置库不得回结论：%+v", reply)
	}
	if len(db.reads) != 0 {
		t.Fatalf("Ready 闸门不该访问数据：%v", db.reads)
	}
}

// nil 请求在本入口被兜住（对比 GetEnrollment 的 nil panic，见 enrollment_read_test.go）。
func TestGetSettlementNilRequestIsTolerated(t *testing.T) {
	ctx, db := newTestSvc(t)
	_, err := runGetSettlement(t, ctx, nil)
	mustErrIs(t, err, model.ErrSettlementNosRequired)
	if len(db.reads) != 0 {
		t.Fatalf("nil 请求不该访问数据：%v", db.reads)
	}
}

// ---------------------------------------------------------------- GetSettlement 投影

// 逐列投影：金额、封顶扣减、确认人/时间、时间戳都要各不相等，否则两列互换也能过。
func TestGetSettlementProjectsEveryColumn(t *testing.T) {
	ctx, db := newTestSvc(t)
	row := db.addSettlement(settleSeed(func(s *model.Settlement) {
		s.AmountMinor = 222_222
		s.CapAppliedMinor = 33_333
		s.MetricCount = 7
	}))
	addItem(db, 1, row.SettlementNo, model.SourceTypeVipWatch, "R_VIP", 120, 200_000)

	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: row.SettlementNo})
	mustNoErr(t, err)
	if !reply.Found || reply.Settlement == nil {
		t.Fatalf("found=false：%+v", reply)
	}
	got := reply.Settlement
	if got.SettlementNo != row.SettlementNo || got.Period != row.Period || got.Mid != row.Mid {
		t.Fatalf("主体不符：%+v", got)
	}
	if got.AmountMinor != 222_222 || got.CapAppliedMinor != 33_333 || got.MetricCount != 7 {
		t.Fatalf("金额口径丢失：%+v", got)
	}
	if got.Currency != "CNY" {
		t.Fatalf("币种错误：%s", got.Currency)
	}
	if got.State != rpc.SettlementState_SETTLEMENT_STATE_CONFIRMED {
		t.Fatalf("状态错误：%v", got.State)
	}
	if got.ConfirmedBy != txOperator || got.ConfirmedAt != 1_710_000_000 {
		t.Fatalf("确认审计位错误：%+v", got)
	}
	if got.VoidReason != "" {
		t.Fatalf("在效单不该带作废原因：%q", got.VoidReason)
	}
	if got.Ctime != 1_700_000_000 || got.Mtime != 1_710_000_000 || got.Ctime == got.Mtime {
		t.Fatalf("时间戳两列互换或丢了：%+v", got)
	}
	// 出金边界（AGENTS.md §1）：读侧只回库里的 NOT_PAYABLE，不得回「可支付」。
	if got.PayoutState != rpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE {
		t.Fatalf("payout_state 被改写：%v", got.PayoutState)
	}
	if len(reply.Items) != 1 {
		t.Fatalf("分项数量错误：%d", len(reply.Items))
	}
	it := reply.Items[0]
	if it.RuleCode != "R_VIP" || it.Quantity != 120 || it.AmountMinor != 200_000 ||
		it.SourceType != rpc.RevenueSourceType_REVENUE_SOURCE_VIP_WATCH {
		t.Fatalf("分项投影错误：%+v", it)
	}
	// 读轨迹逐项等长：只碰主表 + 分项表，且一条写语句都没有。
	db.assertReads(t, 0, "fake:settlements.FindOneByNo", "fake:settlementItems.ListByNo")
	if len(db.calls) != 0 {
		t.Fatalf("读入口执行了写语句：%v", db.calls)
	}
}

// 作废单必须照样查得到、且把 void_reason 原样带回来（作废是审计证据，不是删除）。
func TestGetSettlementVoidedRowStaysQueryable(t *testing.T) {
	ctx, db := newTestSvc(t)
	row := db.addSettlement(settleSeed(func(s *model.Settlement) {
		s.State = model.SettlementStateVoided
		s.PayoutState = model.PayoutStateNotPayable
		s.ConfirmedBy = ""
		s.ConfirmedAt = 0
		s.VoidReason = "台账更正后重算"
		s.VoidSeq = 9 // 真实路径写 settlement_id；读侧不暴露这一列，只要不是 0 就代表槽位已释放
		s.Mid = 900
	}))

	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: row.SettlementNo, Mid: 900})
	mustNoErr(t, err)
	if !reply.Found {
		t.Fatalf("VOIDED 单查不到：%+v", reply)
	}
	if reply.Settlement.State != rpc.SettlementState_SETTLEMENT_STATE_VOIDED {
		t.Fatalf("状态没回 VOIDED：%v", reply.Settlement.State)
	}
	if reply.Settlement.VoidReason != "台账更正后重算" {
		t.Fatalf("作废原因丢了：%q", reply.Settlement.VoidReason)
	}
	if reply.Settlement.PayoutState != rpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE {
		t.Fatalf("作废单的出金位被改写：%v", reply.Settlement.PayoutState)
	}
}

// 分项顺序的事实源是 cr_settlement_item.go:85 的 `ORDER BY source_type ASC`。
// 种子按 4/1/3/2 插入，若哪天改成回插入序立刻红。
func TestGetSettlementItemsAreOrderedBySourceTypeAsc(t *testing.T) {
	ctx, db := newTestSvc(t)
	row := db.addSettlement(settleSeed(nil))
	addItem(db, 11, row.SettlementNo, model.SourceTypeActivity, "R_ACT", 1, 400)
	addItem(db, 12, row.SettlementNo, model.SourceTypeVipWatch, "R_VIP", 2, 100)
	addItem(db, 13, row.SettlementNo, model.SourceTypeInteraction, "R_INT", 4, 300)
	addItem(db, 14, row.SettlementNo, model.SourceTypeCoin, "R_COIN", 3, 200)
	// 另一张单的分项绝不能混进来（uniq_no_source 的隔离边界）。
	addItem(db, 15, "CRS202601-100-2", model.SourceTypeVipWatch, "R_OTHER", 9, 999)

	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: row.SettlementNo})
	mustNoErr(t, err)
	mustItemSeq(t, reply.Items,
		model.SourceTypeVipWatch, model.SourceTypeCoin, model.SourceTypeInteraction, model.SourceTypeActivity)
	if reply.Items[0].RuleCode != "R_VIP" || reply.Items[3].RuleCode != "R_ACT" {
		t.Fatalf("分项与顺序错位：%+v", reply.Items)
	}
}

// 有单无分项：回非 nil 空数组（网关要能区分「没有分项」与「没查分项」）。
func TestGetSettlementEmptyItemsIsNonNilSlice(t *testing.T) {
	ctx, db := newTestSvc(t)
	row := db.addSettlement(settleSeed(nil))
	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: row.SettlementNo})
	mustNoErr(t, err)
	if reply.Items == nil {
		t.Fatal("空分项必须是 nil 之外的空切片")
	}
	if len(reply.Items) != 0 {
		t.Fatalf("不该有分项：%+v", reply.Items)
	}
	db.assertReads(t, 0, "fake:settlements.FindOneByNo", "fake:settlementItems.ListByNo")
}

// 单号不存在 → found=false 的正常业务结论（不是错误）。
// 现状哨兵：此时 Items 保持 nil，而 found=true 的空分项回非 nil 空数组，两处口径不一致。
func TestGetSettlementUnknownNoIsFoundFalseNotError(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addSettlement(settleSeed(func(s *model.Settlement) { s.SettlementNo = "CRS202601-100-9" }))

	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: "CRS-not-exist"})
	mustNoErr(t, err)
	if reply.Found || reply.Settlement != nil {
		t.Fatalf("不存在的单回了结论：%+v", reply)
	}
	if reply.Items != nil {
		t.Fatalf("现状已变：未找到时分项也被初始化了（%v），README 结算域该项需撤销", reply.Items)
	}
	// 主表判空后不得再去读分项表。
	db.assertReads(t, 0, "fake:settlements.FindOneByNo")
}

// ---------------------------------------------------------------- GetSettlement 归属校验

// mid>0 且归属不符：ErrForbidden，而且必须停在分项表之前 ——
// 分项里是「钱从哪来」的明细，越权探测连条目的规模都不该拿到。
func TestGetSettlementOwnershipMismatchStopsBeforeItems(t *testing.T) {
	ctx, db := newTestSvc(t)
	row := db.addSettlement(settleSeed(func(s *model.Settlement) { s.Mid = 100 }))
	addItem(db, 1, row.SettlementNo, model.SourceTypeVipWatch, "R_VIP", 120, 200_000)

	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: row.SettlementNo, Mid: 200})
	mustErrIs(t, err, model.ErrForbidden)
	if reply != nil {
		t.Fatalf("越权不得回结论：%+v", reply)
	}
	if !strings.Contains(err.Error(), "mid=100") || !strings.Contains(err.Error(), "mid=200") {
		t.Fatalf("错误里没带上两个 mid（运营无法据此定位串单）：%v", err)
	}
	db.assertReads(t, 0, "fake:settlements.FindOneByNo") // 分项表一次都没读
}

// mid>0 且归属一致：正常回全量（证明 ErrForbidden 判的是真归属而不是「凡带 mid 就拒」）。
func TestGetSettlementOwnershipMatchReturnsFullDetail(t *testing.T) {
	ctx, db := newTestSvc(t)
	row := db.addSettlement(settleSeed(func(s *model.Settlement) { s.Mid = 100 }))
	addItem(db, 1, row.SettlementNo, model.SourceTypeVipWatch, "R_VIP", 120, 200_000)

	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: row.SettlementNo, Mid: 100})
	mustNoErr(t, err)
	if !reply.Found || reply.Settlement.Mid != 100 || len(reply.Items) != 1 {
		t.Fatalf("归属相符却查不到：%+v", reply)
	}
	db.assertReads(t, 0, "fake:settlements.FindOneByNo", "fake:settlementItems.ListByNo")
}

// 现状哨兵（高危，已登记 README 结算域）：mid=0 是「不校验归属」的合法入参，
// 于是任何拿到单号的调用方都能读出别人的应计金额、封顶扣减、确认人与分项明细。
// proto 里 mid 的注释（rpc/creatorrevenue.proto:342）就是这一现状的自述。
func TestGetSettlementOwnershipCheckIsOptional(t *testing.T) {
	ctx, db := newTestSvc(t)
	row := db.addSettlement(settleSeed(func(s *model.Settlement) {
		s.Mid = 400100 // 明显假值：不属于本次调用的「作者」
	}))
	addItem(db, 1, row.SettlementNo, model.SourceTypeVipWatch, "R_VIP", 120, 200_000)

	reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: row.SettlementNo, Mid: 0})
	mustNoErr(t, err)
	if !reply.Found {
		t.Fatalf("现状已变：mid=0 不再放行跨作者查询，请撤销 README 结算域的该项登记：%+v", reply)
	}
	if reply.Settlement.Mid != 400100 || reply.Settlement.AmountMinor != 123_456 ||
		reply.Settlement.ConfirmedBy != txOperator || len(reply.Items) != 1 {
		t.Fatalf("现状钉的是「整张单连金额带明细全泄漏」，实际读到：%+v / %+v", reply.Settlement, reply.Items)
	}
	if len(db.calls) != 0 {
		t.Fatalf("读入口不该有写语句：%v", db.calls)
	}
}

// ---------------------------------------------------------------- GetSettlement 下游故障

func TestGetSettlementReadFailuresPropagateRaw(t *testing.T) {
	t.Run("主表故障不得折成查不到", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		db.addSettlement(settleSeed(nil))
		boom := errors.New("dial tcp: settlement master down")
		db.readFailOn["fake:settlements.FindOneByNo"] = boom

		reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: "CRS202601-100-1", Mid: txMid})
		if !errors.Is(err, boom) {
			t.Fatalf("原始错误没上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("故障不得回伪结论：%+v", reply)
		}
		db.assertReads(t, 0, "fake:settlements.FindOneByNo")
	})

	t.Run("分项表故障不得只回半张单", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		db.addSettlement(settleSeed(nil))
		boom := errors.New("dial tcp: item table down")
		db.readFailOn["fake:settlementItems.ListByNo"] = boom

		reply, err := runGetSettlement(t, ctx, &rpc.GetSettlementReq{SettlementNo: "CRS202601-100-1"})
		if !errors.Is(err, boom) {
			t.Fatalf("原始错误没上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("故障不得回伪结论（半张单比报错更危险）：%+v", reply)
		}
		db.assertReads(t, 0, "fake:settlements.FindOneByNo", "fake:settlementItems.ListByNo")
	})
}

// ---------------------------------------------------------------- ListSettlements 入口闸门

func TestListSettlementsGatesTouchNoData(t *testing.T) {
	cases := []struct {
		name   string
		in     *rpc.ListSettlementsReq
		target error
		want   string
	}{
		{"period 与 mid 都不给（无界扫描）", &rpc.ListSettlementsReq{}, model.ErrQueryScopeRequired,
			"period 或 mid"},
		{"period 只有空白等同没给", &rpc.ListSettlementsReq{Period: "   "}, model.ErrQueryScopeRequired, ""},
		{"period 位数不足", &rpc.ListSettlementsReq{Period: "2026-1"}, model.ErrInvalidPeriod, "不是 6 位数字"},
		{"period 月份越界", &rpc.ListSettlementsReq{Period: "202613"}, model.ErrInvalidPeriod, "月份"},
		{"period 含非数字", &rpc.ListSettlementsReq{Period: "2026ab"}, model.ErrInvalidPeriod, "非数字"},
		{"mid 负数", &rpc.ListSettlementsReq{Period: txPeriod, Mid: -1}, model.ErrInvalidMid, "mid=-1"},
		{"mid 负数且没给 period", &rpc.ListSettlementsReq{Mid: -1}, model.ErrInvalidMid, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			db.addSettlement(settleSeed(nil))
			reply, err := runListSettlements(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			if len(db.reads) != 0 || len(db.calls) != 0 {
				t.Fatalf("闸门未过却已访问数据：%v / %v", db.reads, db.calls)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里缺少 %q：%v", c.want, err)
			}
		})
	}
}

// 守卫先后必须可判别：格式 → 范围 → mid 符号 → state 枚举。
// 任何一条被挪到前面，回的错误码就会变，这条用例即红。
func TestListSettlementsGuardOrderIsDiscriminable(t *testing.T) {
	cases := []struct {
		name   string
		in     *rpc.ListSettlementsReq
		target error
	}{
		{"格式先于范围", &rpc.ListSettlementsReq{Period: "2026x"}, model.ErrInvalidPeriod},
		{"格式先于 mid 符号", &rpc.ListSettlementsReq{Period: "202613", Mid: -1}, model.ErrInvalidPeriod},
		{"格式先于 state 枚举", &rpc.ListSettlementsReq{Period: "202613", State: 9}, model.ErrInvalidPeriod},
		{"范围先于 state 枚举", &rpc.ListSettlementsReq{State: 9}, model.ErrQueryScopeRequired},
		{"mid 符号先于 state 枚举", &rpc.ListSettlementsReq{Mid: -1, State: 9}, model.ErrInvalidMid},
		{"mid=0 加 state 越界才轮到枚举", &rpc.ListSettlementsReq{Period: txPeriod, State: 9},
			model.ErrInvalidRuleState},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			_, err := runListSettlements(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if len(db.reads) != 0 {
				t.Fatalf("闸门未过却已访问数据：%v", db.reads)
			}
		})
	}
}

// 现状哨兵（已登记 README 结算域）：结算单状态越界复用了规则状态的哨兵
// ErrInvalidRuleState（helpers.go:97），调用方按错误码分支时读到的是「invalid rule state」。
// 这里钉的是「越界必须被拒」这一实质结论，错误码错配只登记不放宽。
func TestListSettlementsStateFilterIsRangeChecked(t *testing.T) {
	for _, st := range []rpc.SettlementState{4, 99, -1, 255} {
		ctx, db := newTestSvc(t)
		reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: txPeriod, State: st})
		mustErrIs(t, err, model.ErrInvalidRuleState)
		if reply != nil {
			t.Fatalf("越界状态不得回结论：%+v", reply)
		}
		// 关键实质：越界绝不能被渲染成「这个状态没单」的空列表。
		if len(db.reads) != 0 {
			t.Fatalf("越界状态却已查库：%v", db.reads)
		}
	}
}

func TestListSettlementsReadyGate(t *testing.T) {
	ctx, db := newTestSvc(t)
	ctx.Settlements = nil
	reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: txPeriod})
	mustErrIs(t, err, model.ErrDBNotConfigured)
	if reply != nil {
		t.Fatalf("未配置库不得回结论：%+v", reply)
	}
	if len(db.reads) != 0 {
		t.Fatalf("Ready 闸门不该访问数据：%v", db.reads)
	}
}

func TestListSettlementsNilRequestIsTolerated(t *testing.T) {
	ctx, db := newTestSvc(t)
	reply, err := runListSettlements(t, ctx, nil)
	mustErrIs(t, err, model.ErrQueryScopeRequired)
	if reply != nil {
		t.Fatalf("nil 请求不得回结论：%+v", reply)
	}
	if len(db.reads) != 0 {
		t.Fatalf("nil 请求不该访问数据：%v", db.reads)
	}
}

// ---------------------------------------------------------------- ListSettlements 排序与分页

// 排序事实源：cr_settlement.go:247 `ORDER BY settlement_id DESC`（新单优先）。
// 种子按 10/30/20 的插入序放，回成插入序即红。
func TestListSettlementsOrdersBySettlementIdDesc(t *testing.T) {
	ctx, db := newTestSvc(t)
	for _, s := range []*model.Settlement{
		settleSeed(func(x *model.Settlement) { x.SettlementId = 10; x.SettlementNo = "NO_10" }),
		settleSeed(func(x *model.Settlement) { x.SettlementId = 30; x.SettlementNo = "NO_30" }),
		settleSeed(func(x *model.Settlement) { x.SettlementId = 20; x.SettlementNo = "NO_20" }),
	} {
		db.addSettlement(s)
	}

	full, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: txPeriod, Size: 50})
	mustNoErr(t, err)
	mustNoSeq(t, full.Settlements, "NO_30", "NO_20", "NO_10")
	if full.Total != 3 {
		t.Fatalf("total 错误：%d", full.Total)
	}
	if w := db.window("fake:settlements.List"); w.offset != 0 || w.limit != 50 {
		t.Fatalf("分页位没进查询：%+v", w)
	}

	var paged []*rpc.SettlementInfo
	for page := int64(1); page <= 3; page++ {
		from := len(db.reads)
		reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: txPeriod, Page: page, Size: 2})
		mustNoErr(t, err)
		if w := db.window("fake:settlements.List"); w.offset != (page-1)*2 || w.limit != 2 {
			t.Fatalf("第 %d 页分页位错误：%+v", page, w)
		}
		if reply.Page != page || reply.Size != 2 || reply.Total != 3 {
			t.Fatalf("第 %d 页回显错误：page=%d size=%d total=%d", page, reply.Page, reply.Size, reply.Total)
		}
		// 每页都只该读主表两次（List+Count）：分页越界不得触发补读或分项扇出。
		db.assertReads(t, from, "fake:settlements.List", "fake:settlements.Count")
		paged = append(paged, reply.Settlements...)
	}
	mustNoSeq(t, paged, "NO_30", "NO_20", "NO_10")
}

// 列表页绝不去扇出分项表：一次 100 行的分页若逐行补读 cr_settlement_item 就是一次 N+1，
// 而这个入口的应答里根本没有分项字段。
func TestListSettlementsNeverFansOutToItemTable(t *testing.T) {
	ctx, db := newTestSvc(t)
	row := db.addSettlement(settleSeed(nil))
	addItem(db, 1, row.SettlementNo, model.SourceTypeVipWatch, "R_VIP", 120, 200_000)

	reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Mid: txMid})
	mustNoErr(t, err)
	if len(reply.Settlements) != 1 {
		t.Fatalf("没查到单：%+v", reply)
	}
	db.assertReads(t, 0, "fake:settlements.List", "fake:settlements.Count")
}

// period / mid / state 三个条件在 List 与 Count 里必须同源（settlementFilter, cr_settlement.go:321）。
func TestListSettlementsFiltersSharePredicateWithCount(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addSettlement(settleSeed(func(s *model.Settlement) {
		s.SettlementId, s.SettlementNo, s.Mid = 41, "D_100_202601", txMid
		s.Period, s.State = txPeriod, model.SettlementStateDraft
	}))
	db.addSettlement(settleSeed(func(s *model.Settlement) {
		s.SettlementId, s.SettlementNo, s.Mid = 42, "C_100_202601", txMid
		s.Period, s.State = txPeriod, model.SettlementStateConfirmed
	}))
	db.addSettlement(settleSeed(func(s *model.Settlement) {
		s.SettlementId, s.SettlementNo, s.Mid = 43, "V_100_202601", txMid
		s.Period, s.State, s.VoidSeq = txPeriod, model.SettlementStateVoided, 43
	}))
	db.addSettlement(settleSeed(func(s *model.Settlement) {
		s.SettlementId, s.SettlementNo, s.Mid = 44, "C_200_202601", int64(200)
		s.Period, s.State = txPeriod, model.SettlementStateConfirmed
	}))
	db.addSettlement(settleSeed(func(s *model.Settlement) {
		s.SettlementId, s.SettlementNo, s.Mid = 45, "C_100_202512", txMid
		s.Period, s.State = "202512", model.SettlementStateConfirmed
	}))

	cases := []struct {
		name   string
		period string
		mid    int64
		state  int32
		want   []string
	}{
		// 期望序列按 settlement_id 倒序（41/42/43/44/45 的降序），不是插入序。
		{"本周期全部（含作废历史，复核必须看得见）", txPeriod, 0, 0,
			[]string{"C_200_202601", "V_100_202601", "C_100_202601", "D_100_202601"}},
		{"只看草稿", txPeriod, 0, model.SettlementStateDraft, []string{"D_100_202601"}},
		{"只看已确认", txPeriod, 0, model.SettlementStateConfirmed,
			[]string{"C_200_202601", "C_100_202601"}},
		{"只看已作废", txPeriod, 0, model.SettlementStateVoided, []string{"V_100_202601"}},
		{"本作者本周期", txPeriod, txMid, 0,
			[]string{"V_100_202601", "C_100_202601", "D_100_202601"}},
		{"本作者已确认（跨周期）", "", txMid, model.SettlementStateConfirmed,
			[]string{"C_100_202512", "C_100_202601"}},
		{"三个条件叠加后为空", txPeriod, int64(200), model.SettlementStateDraft, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			from := len(db.reads)
			reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{
				Period: c.period, Mid: c.mid, State: rpc.SettlementState(c.state), Size: 10,
			})
			mustNoErr(t, err)
			mustNoSeq(t, reply.Settlements, c.want...)
			if reply.Total != int64(len(c.want)) {
				t.Fatalf("total 与过滤结果不同源：total=%d rows=%d", reply.Total, len(reply.Settlements))
			}
			if reply.Settlements == nil {
				t.Fatal("空结果也要回非 nil 空数组")
			}
			db.assertReads(t, from, "fake:settlements.List", "fake:settlements.Count")
			for _, row := range reply.Settlements {
				// 出金边界：列表里每一行的 payout_state 都是库中原值（NOT_PAYABLE），
				// 绝不能被渲染成 0/Unspecified 之外的任何「像是可支付」的值。
				if row.PayoutState != rpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE {
					t.Fatalf("payout_state 被读侧改写：%+v", row)
				}
			}
		})
	}
}

// 带分隔符的周期写法在入口归一后才去查（否则 'period = "2026-01"' 会查空，
// 运营看到的是一份「这个月没出单」的假账）。
func TestListSettlementsNormalizesPeriodBeforeQuery(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addSettlement(settleSeed(func(s *model.Settlement) { s.SettlementNo = "NO_1" }))

	first, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: txPeriod})
	mustNoErr(t, err)
	mustNoSeq(t, first.Settlements, "NO_1")

	for _, raw := range []string{"2026-01", "2026_01", " 202601 ", "2026 01"} {
		reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: raw})
		mustNoErr(t, err)
		mustNoSeq(t, reply.Settlements, "NO_1")
	}
}

func TestListSettlementsPageSizeFoldingReachesQuery(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addSettlement(settleSeed(nil))

	cases := []struct {
		name               string
		page, size         int64
		wantPage, wantSize int64
		wantOffset         int64
	}{
		{"页宽超上限", 1, 99_999, 1, 100, 0},
		{"页宽 0", 1, 0, 1, 100, 0},
		{"页宽负数", 1, -20, 1, 100, 0},
		{"页码 0", 0, 10, 1, 10, 0},
		{"页码负数", -7, 20, 1, 20, 0},
		{"深页保留 offset", 4, 25, 4, 25, 75},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{
				Period: txPeriod, Page: c.page, Size: c.size,
			})
			mustNoErr(t, err)
			if reply.Page != c.wantPage || reply.Size != c.wantSize {
				t.Fatalf("回显分页错误：got page=%d size=%d want page=%d size=%d",
					reply.Page, reply.Size, c.wantPage, c.wantSize)
			}
			if w := db.window("fake:settlements.List"); w.offset != c.wantOffset || w.limit != c.wantSize {
				t.Fatalf("折好的分页位没进查询：%+v（want offset=%d limit=%d）",
					w, c.wantOffset, c.wantSize)
			}
		})
	}
}

func TestListSettlementsEmptyLedgerIsNonNilSlice(t *testing.T) {
	ctx, db := newTestSvc(t)
	reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: "203012"})
	mustNoErr(t, err)
	if reply.Settlements == nil {
		t.Fatal("空列表必须是非 nil 空数组")
	}
	if len(reply.Settlements) != 0 || reply.Total != 0 {
		t.Fatalf("空库回了东西：%+v", reply)
	}
	if len(db.calls) != 0 {
		t.Fatalf("列表入口不该有写语句：%v", db.calls)
	}
}

// 下游故障三类之一：原始错误上抛。列表两次读各自失败都要上抛，
// 且失败点之前的读轨迹必须可判别（List 失败时绝不能再去 Count）。
func TestListSettlementsReadFailuresPropagateRaw(t *testing.T) {
	t.Run("列表读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		db.addSettlement(settleSeed(nil))
		boom := errors.New("read timeout on cr_settlement")
		db.readFailOn["fake:settlements.List"] = boom

		reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: txPeriod})
		if !errors.Is(err, boom) {
			t.Fatalf("原始错误没上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("故障不得回伪结论：%+v", reply)
		}
		db.assertReads(t, 0, "fake:settlements.List")
	})

	t.Run("计数读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		db.addSettlement(settleSeed(nil))
		boom := errors.New("read timeout on count")
		db.readFailOn["fake:settlements.Count"] = boom

		reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: txPeriod})
		if !errors.Is(err, boom) {
			t.Fatalf("原始错误没上抛：%v", err)
		}
		if reply != nil {
			// 现状钉的是「不回半截分页」：rows 到手却数不出 total 时宁可整体失败。
			t.Fatalf("故障不得回伪结论：%+v", reply)
		}
		db.assertReads(t, 0, "fake:settlements.List", "fake:settlements.Count")
	})
}

// 现状哨兵（高危，已登记 README 结算域）：只给 period 不给 mid 是合法入参，
// 于是任何调用方都能一次读出「全平台每个作者这个月的应计金额」。
func TestListSettlementsPeriodOnlyExposesAllAuthors(t *testing.T) {
	ctx, db := newTestSvc(t)
	for _, mid := range []int64{400100, 400200, 400300} {
		db.addSettlement(settleSeed(func(s *model.Settlement) {
			s.Mid = mid
			s.AmountMinor = 111_000 + mid%1000
			s.ConfirmedBy = "ops-99"
		}))
	}

	reply, err := runListSettlements(t, ctx, &rpc.ListSettlementsReq{Period: txPeriod, Size: 50})
	mustNoErr(t, err)
	if len(reply.Settlements) != 3 || reply.Total != 3 {
		t.Fatalf("现状已变：只给 period 不再回跨作者结果，请撤销 README 结算域的该项登记：%+v", reply)
	}
	var sum int64
	for _, row := range reply.Settlements {
		if row.Mid != 400100 && row.Mid != 400200 && row.Mid != 400300 {
			t.Fatalf("混进了没种子的作者：%+v", row)
		}
		if row.ConfirmedBy != "ops-99" {
			t.Fatalf("现状钉的是连确认人一起泄漏：%+v", row)
		}
		sum += row.AmountMinor
	}
	if sum != 333_000+600 {
		t.Fatalf("三个作者的应计金额没被原样读出：%d", sum)
	}
}
