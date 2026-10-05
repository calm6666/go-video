package logic

// BindPayment 是「payment 已受理但订单没走到 PAID」的补偿推进口，供 cron/运营兜底。
// 本文件锁三条口径：金额不一致不固化错账；PAID 及之后一律幂等；
// 已取消订单收到绑款是资金事故信号 —— 拒绝推进但仍写同态台账留证。

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

func bindReq(orderNo, paymentNo string, amount int64, requestID string) *rpc.BindPaymentReq {
	return &rpc.BindPaymentReq{
		OrderNo: orderNo, PaymentNo: paymentNo, AmountMinor: amount, RequestId: requestID,
	}
}

// payableOrder 造一张「刚受理、还没绑款」的订单（PAYING，version 2）。
func payableOrder(t *testing.T, db *fakeDB, orderNo string, state int32) *model.Order {
	t.Helper()
	if state == 0 {
		state = model.StatePaying
	}
	version := int64(2) // 建单(1) + CREATED→PAYING(2)
	if state == model.StateCreated {
		version = 1 // 还停在出生版本
	}
	return seedOrder(t, db, &model.Order{
		OrderNo: orderNo, RequestID: "req-" + orderNo, Mid: 1001, BizType: model.BizMembership,
		PlanID: 77, PlanCode: "code_77", Title: "大会员月卡", Currency: "CNY",
		Quantity: 1, DurationDays: 31, UnitPriceMinor: 3000, AmountMinor: 3000,
		PayMethod: model.PaySandbox, Platform: model.PlatformAndroid,
		State: state, FulfillState: model.FulfillPending, Version: version,
	})
}

func newBindLogic(t *testing.T) (*BindPaymentLogic, *fakeDB) {
	t.Helper()
	svcCtx, db := newTestSvc(t)
	wireDownstream(svcCtx, newFakeMembership(), newFakePayment(), &fakeCoin{})
	return NewBindPaymentLogic(t.Context(), svcCtx), db
}

// TestBindPaymentAdvivesPayingOrderToPaid 正向：PAYING → PAID，绑定 payment_no 与 paid_at。
func TestBindPaymentAdvivesPayingOrderToPaid(t *testing.T) {
	l, db := newBindLogic(t)
	payableOrder(t, db, "to_bind1", model.StatePaying)

	reply, err := l.BindPayment(bindReq("to_bind1", "pay_to_bind1", 3000, "bind-1"))
	if err != nil {
		t.Fatalf("BindPayment() error = %v", err)
	}
	if reply.GetDuplicated() {
		t.Error("首次绑款不该 duplicated")
	}
	got := mustOrder(t, db, "to_bind1")
	if got.State != model.StatePaid {
		t.Fatalf("state = %s，期望 PAID", model.StateName(got.State))
	}
	if got.PaymentNo != "pay_to_bind1" || got.PaidAt == 0 {
		t.Errorf("payment_no/paid_at = (%q,%d)", got.PaymentNo, got.PaidAt)
	}
	if got.Version != 3 {
		t.Errorf("version = %d，期望 3", got.Version)
	}
	if got.FulfillState != model.FulfillPending {
		t.Errorf("fulfill_state = %d，绑款不该改变履约结论", got.FulfillState)
	}
	rows := ledgerOf(db, "to_bind1")
	if len(rows) != 1 {
		t.Fatalf("台账行数 = %d，期望 1", len(rows))
	}
	if rows[0].FromState != model.StatePaying || rows[0].ToState != model.StatePaid {
		t.Errorf("台账 = (%s,%s)", model.StateName(rows[0].FromState), model.StateName(rows[0].ToState))
	}
	if rows[0].Reason != "payment bound: pay_to_bind1" {
		t.Errorf("台账理由 = %q，要带 payment_no", rows[0].Reason)
	}
	// operator 缺省按 cron 记（这是 cron 的兜底入口，不能记成 user）
	if rows[0].Operator != "cron" {
		t.Errorf("operator = %q，期望默认 cron", rows[0].Operator)
	}
	if rows[0].RequestID != "bind-1" {
		t.Errorf("request_id = %q", rows[0].RequestID)
	}
	assertOnlyLegalTransitions(t, db)
}

// TestBindPaymentBackfillsPayingForCreatedOrder CREATED 不能一步跳到 PAID：
// 先补 PAYING 再绑款，与建单链路同一套状态机口径。
func TestBindPaymentBackfillsPayingForCreatedOrder(t *testing.T) {
	l, db := newBindLogic(t)
	payableOrder(t, db, "to_bind_created", model.StateCreated)

	if _, err := l.BindPayment(bindReq("to_bind_created", "pay_x", 3000, "bind-c")); err != nil {
		t.Fatalf("BindPayment() error = %v", err)
	}
	got := mustOrder(t, db, "to_bind_created")
	if got.State != model.StatePaid || got.Version != 3 {
		t.Errorf("state/version = (%s,%d)，期望 (PAID,3)", model.StateName(got.State), got.Version)
	}
	want := [][2]string{{"CREATED", "PAYING"}, {"PAYING", "PAID"}}
	if pairs := ledgerPairs(db, "to_bind_created"); len(pairs) != 2 || pairs[0] != want[0] || pairs[1] != want[1] {
		t.Errorf("台账链 = %v，期望 %v", pairs, want)
	}
	assertOnlyLegalTransitions(t, db)
}

// TestBindPaymentIsIdempotentAfterPaid PAID 及之后一律幂等：不动状态、不再写台账。
// FAILED 也在 IsPaidOrLater 之内（钱收过、履约没成），绑款重放同样只回 duplicated ——
// 这是代码实际口径，锁住它，防止「补一次绑款把 FAILED 复活」这种绕过状态机的写法。
func TestBindPaymentIsIdempotentAfterPaid(t *testing.T) {
	for _, st := range []int32{model.StatePaid, model.StateFulfilling, model.StateFulfilled,
		model.StateRefundRequested, model.StateRefundApproved, model.StateRefunded, model.StateFailed} {
		t.Run(model.StateName(st), func(t *testing.T) {
			l, db := newBindLogic(t)
			o := payableOrder(t, db, "to_paid", st)
			o.PaymentNo = "pay_to_paid"
			o.GrantRef = "membership_grant:7001"
			db.orders["to_paid"] = o

			reply, err := l.BindPayment(bindReq("to_paid", "pay_other", 3000, "bind-x"))
			if err != nil {
				t.Fatalf("BindPayment() error = %v", err)
			}
			if !reply.GetDuplicated() {
				t.Error("已绑款后的请求必须 duplicated=true")
			}
			got := mustOrder(t, db, "to_paid")
			if got.State != st || got.Version != 2 {
				t.Errorf("幂等重放改动了订单: state=%s version=%d", model.StateName(got.State), got.Version)
			}
			if got.PaymentNo != "pay_to_paid" {
				t.Errorf("已有 payment_no 被覆盖成 %q", got.PaymentNo)
			}
			if len(db.events) != 0 {
				t.Errorf("幂等重放写了 %d 行台账", len(db.events))
			}
		})
	}
}

// TestBindPaymentAmountMismatchIsRejected 金额不一致（含 <=0）一律拒：
// 两边台账已分叉，推进只会把错账固化。
func TestBindPaymentAmountMismatchIsRejected(t *testing.T) {
	for _, amount := range []int64{0, -5, 2999, 3001} {
		l, db := newBindLogic(t)
		payableOrder(t, db, "to_amt", model.StatePaying)
		_, err := l.BindPayment(bindReq("to_amt", "pay_amt", amount, "bind-amt"))
		if !errors.Is(err, model.ErrBindAmountMismatch) {
			t.Fatalf("amount=%d error = %v，期望 %v", amount, err, model.ErrBindAmountMismatch)
		}
		if !strings.Contains(err.Error(), "order_amount_minor=3000") ||
			!strings.Contains(err.Error(), "bind_amount_minor=") {
			t.Errorf("错误文本没给对账信息: %v", err)
		}
		got := mustOrder(t, db, "to_amt")
		if got.State != model.StatePaying || got.PaymentNo != "" || len(db.events) != 0 {
			t.Errorf("金额不符却推进了: state=%s payment_no=%q events=%d",
				model.StateName(got.State), got.PaymentNo, len(db.events))
		}
	}
}

// TestBindPaymentLedgerReplay 同一 request_id 已把订单推到 PAID 时，
// 即使订单被回退到 PAYING（人工修账）也必须按重放处理，不再写一遍台账。
func TestBindPaymentLedgerReplay(t *testing.T) {
	l, db := newBindLogic(t)
	payableOrder(t, db, "to_ledgerdup", model.StatePaying)
	seedLedger(t, db, &model.OrderEvent{
		OrderNo: "to_ledgerdup", FromState: model.StatePaying, ToState: model.StatePaid,
		Operator: "cron", Reason: "payment bound: pay_ledgerdup", RequestID: "bind-dup",
	})

	reply, err := l.BindPayment(bindReq("to_ledgerdup", "pay_ledgerdup", 3000, "bind-dup"))
	if err != nil {
		t.Fatalf("BindPayment() error = %v", err)
	}
	if !reply.GetDuplicated() {
		t.Error("台账命中必须 duplicated=true")
	}
	if got := mustOrder(t, db, "to_ledgerdup"); got.State != model.StatePaying || got.Version != 2 {
		t.Errorf("台账命中却仍推进: state=%s version=%d", model.StateName(got.State), got.Version)
	}
	if len(db.events) != 1 {
		t.Errorf("台账命中却多写了 %d 行", len(db.events)-1)
	}
}

// TestBindPaymentOnCancelledOrderLeavesEvidence 已取消订单收到绑款：
// 拒绝推进 + 同态台账留证 + ErrBindOnCancelledOrder（资金事故，需人工退回）。
func TestBindPaymentOnCancelledOrderLeavesEvidence(t *testing.T) {
	l, db := newBindLogic(t)
	payableOrder(t, db, "to_cancelled", model.StateCancelled)

	_, err := l.BindPayment(bindReq("to_cancelled", "pay_too_late", 3000, "bind-late"))
	if !errors.Is(err, model.ErrBindOnCancelledOrder) {
		t.Fatalf("BindPayment() error = %v，期望 %v", err, model.ErrBindOnCancelledOrder)
	}
	if !strings.Contains(err.Error(), "pay_too_late") {
		t.Errorf("错误文本要带上游 payment_no: %v", err)
	}
	got := mustOrder(t, db, "to_cancelled")
	if got.State != model.StateCancelled || got.Version != 2 {
		t.Errorf("已取消订单被绑款推进了: state=%s version=%d", model.StateName(got.State), got.Version)
	}
	if got.PaymentNo != "" {
		t.Errorf("拒绝推进却仍写了 payment_no=%q", got.PaymentNo)
	}
	rows := ledgerOf(db, "to_cancelled")
	if len(rows) != 1 {
		t.Fatalf("留证台账行数 = %d，期望 1", len(rows))
	}
	if rows[0].FromState != model.StateCancelled || rows[0].ToState != model.StateCancelled {
		t.Error("留证必须是同态行（CANCELLED 没有合法出边）")
	}
	if !strings.Contains(rows[0].Reason, "requires manual refund") ||
		!strings.Contains(rows[0].Reason, "pay_too_late") {
		t.Errorf("台账理由 = %q，要能直接看出需要人工退回资金", rows[0].Reason)
	}
	if rows[0].Operator != "cron" || rows[0].RequestID != "bind-late" {
		t.Errorf("台账 operator/request_id = (%q,%q)", rows[0].Operator, rows[0].RequestID)
	}
	assertOnlyLegalTransitions(t, db)
}

// TestBindPaymentConcurrentMissReturnsDuplicatedWithFreshState CAS 未命中
// （真实实现是 RowsAffected==0）时按并发失败处理，并回读当前行而不是回内存旧值。
func TestBindPaymentConcurrentMissReturnsDuplicatedWithFreshState(t *testing.T) {
	l, db := newBindLogic(t)
	payableOrder(t, db, "to_race", model.StatePaying)
	// 别人抢先把订单推到了 PAID 并改了 payment_no
	db.raceHook = func(o *model.Order, from, to int32) {
		if from == model.StatePaying && to == model.StatePaid {
			o.State = model.StatePaid
			o.Version = 9
			o.PaymentNo = "pay_winner"
		}
	}

	reply, err := l.BindPayment(bindReq("to_race", "pay_loser", 3000, "bind-race"))
	if err != nil {
		t.Fatalf("BindPayment() error = %v", err)
	}
	if !reply.GetDuplicated() {
		t.Error("CAS 未命中必须按并发重放返回")
	}
	if reply.GetOrder().GetPaymentNo() != "pay_winner" ||
		reply.GetOrder().GetState() != rpc.OrderState_ORDER_STATE_PAID {
		t.Errorf("回复 = (%q,%v)，期望胜者的 payment_no 与 PAID",
			reply.GetOrder().GetPaymentNo(), reply.GetOrder().GetState())
	}
	got := mustOrder(t, db, "to_race")
	if got.PaymentNo != "pay_winner" || got.Version != 9 {
		t.Errorf("败者覆盖了胜者结论: payment_no=%q version=%d", got.PaymentNo, got.Version)
	}
}

// TestBindPaymentValidatesInput 入参与订单存在性校验：全部直接报错，不动数据。
func TestBindPaymentValidatesInput(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.BindPaymentReq
		want error
	}{
		{"缺 order_no", bindReq(" ", "pay_1", 3000, "r"), model.ErrOrderNoRequired},
		{"缺 payment_no", bindReq("to_1", "  ", 3000, "r"), model.ErrPaymentNoRequired},
		{"缺 request_id", bindReq("to_1", "pay_1", 3000, " "), model.ErrRequestIdRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, db := newBindLogic(t)
			payableOrder(t, db, "to_1", model.StatePaying)
			if _, err := l.BindPayment(tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("BindPayment() error = %v，期望 %v", err, tc.want)
			}
			got := mustOrder(t, db, "to_1")
			if got.State != model.StatePaying || got.PaymentNo != "" || len(db.events) != 0 {
				t.Error("入参非法却改动了订单")
			}
		})
	}

	t.Run("订单不存在", func(t *testing.T) {
		l, db := newBindLogic(t)
		if _, err := l.BindPayment(bindReq("to_none", "pay_1", 3000, "r")); !errors.Is(err, model.ErrOrderNotFound) {
			t.Fatalf("BindPayment() error = %v，期望 %v", err, model.ErrOrderNotFound)
		}
		if len(db.orders) != 0 {
			t.Error("订单不存在却凭空建了单")
		}
	})
}

// TestBindPaymentOperatorTruncatedAndBounded operator 超列宽按 rune 裁切，
// 且台账里的 payment_no 也按列宽保护（否则严格模式直接报错）。
func TestBindPaymentOperatorTruncatedAndBounded(t *testing.T) {
	l, db := newBindLogic(t)
	payableOrder(t, db, "to_oplen", model.StatePaying)
	req := bindReq("to_oplen", strings.Repeat("单", 100), 3000, "bind-op")
	req.Operator = strings.Repeat("运营", 60) // 120 rune > 64

	if _, err := l.BindPayment(req); err != nil {
		t.Fatalf("BindPayment() error = %v", err)
	}
	got := mustOrder(t, db, "to_oplen")
	if len([]rune(got.PaymentNo)) != maxPaymentNoLen {
		t.Errorf("payment_no 长度 = %d，期望裁到 %d", len([]rune(got.PaymentNo)), maxPaymentNoLen)
	}
	rows := ledgerOf(db, "to_oplen")
	if len([]rune(rows[0].Operator)) != maxOperatorLen {
		t.Errorf("operator 长度 = %d，期望 %d", len([]rune(rows[0].Operator)), maxOperatorLen)
	}
	if len([]rune(rows[0].RequestID)) > maxRequestIDLen {
		t.Error("台账 request_id 超列宽")
	}
}
