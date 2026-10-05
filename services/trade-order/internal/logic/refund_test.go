package logic

// 退款三接口（RequestRefund / ApproveRefund / RejectRefund）的口径测试。
//
// 这是本服务资金语义最重的一段，锁四件事：
//  1. 只登记申请不动钱；钱与权益都在 ApproveRefund 里处理，顺序是「先退钱 → 记账 → 再回收」；
//  2. 回收失败必须停在 REFUND_APPROVED（款已退、权益未回收），绝不标 REFUNDED；
//  3. 部分退款在本沙箱不支持（下游没有「按订单号查消耗量」的接口，拿不到消耗事实就无法折算），
//     任何自报金额都回 ErrRefundAmountInvalid —— 本文件把这条现状锁住，防止被「顺手支持一下」写坏；
//  4. 驳回按「回到申请前的原状态」实现，REFUND_REJECTED 不进入（那条边一旦走到就是死胡同）。

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	memberrpc "go-video/services/membership/rpc"
	paymentrpc "go-video/services/payment/rpc"
	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type refundHarness struct {
	t    *testing.T
	req  *RequestRefundLogic
	appr *ApproveRefundLogic
	rej  *RejectRefundLogic
	svc  *svc.ServiceContext
	db   *fakeDB
	mem  *fakeMembership
	pay  *fakePayment
	coin *fakeCoin
}

func newRefundHarness(t *testing.T) *refundHarness {
	t.Helper()
	svcCtx, db := newTestSvc(t)
	mem := newFakeMembership().withPlan(defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	pay := newFakePayment()
	coin := &fakeCoin{}
	wireDownstream(svcCtx, mem, pay, coin)
	ctx := t.Context()
	return &refundHarness{
		t: t, req: NewRequestRefundLogic(ctx, svcCtx), appr: NewApproveRefundLogic(ctx, svcCtx),
		rej: NewRejectRefundLogic(ctx, svcCtx), svc: svcCtx, db: db, mem: mem, pay: pay, coin: coin,
	}
}

// seedForRefund 造一张进入退款流程前各状态的单。
// 默认给「已履约的会员单」：version 5（建单 1 → PAID 3 → FULFILLED 5），带 grant_ref。
func (h *refundHarness) seedForRefund(orderNo string, tune func(*model.Order)) *model.Order {
	o := &model.Order{
		OrderNo: orderNo, RequestID: "req-" + orderNo, Mid: 1001, BizType: model.BizMembership,
		PlanID: 77, PlanCode: "code_77", Title: "大会员月卡", Currency: "CNY",
		Quantity: 1, DurationDays: 31, UnitPriceMinor: 3000, AmountMinor: 3000,
		PayMethod: model.PaySandbox, Platform: model.PlatformAndroid,
		PaymentNo: "pay_" + orderNo, GrantRef: "membership_grant:7001", FulfillAttempts: 1,
		State: model.StateFulfilled, FulfillState: model.FulfillDone, Version: 5,
	}
	if tune != nil {
		tune(o)
	}
	return seedOrder(h.t, h.db, o)
}

func requestRefundReq(orderNo string, mid int64, operator string, amount int64, requestID, reason string) *rpc.RequestRefundReq {
	return &rpc.RequestRefundReq{
		OrderNo: orderNo, Mid: mid, Operator: operator, AmountMinor: amount,
		RequestId: requestID, Reason: reason,
	}
}

func approveReq(orderNo string, version int64) *rpc.ApproveRefundReq {
	return &rpc.ApproveRefundReq{
		OrderNo: orderNo, Operator: "ops_007", RequestId: "approve-" + orderNo,
		Reason: "核对无误，同意退款", ExpectedVersion: version,
	}
}

func rejectReq(orderNo, requestID string) *rpc.RejectRefundReq {
	return &rpc.RejectRefundReq{
		OrderNo: orderNo, Operator: "ops_007", RequestId: requestID, Reason: "已使用权益，不符合退款条件",
	}
}

// ------------------------------------------------------------ RequestRefund ----

// TestRequestRefundOnlyRegistersApplication 申请阶段只登记：不动状态机之外的任何字段，
// 也绝不提前退钱或回收权益。
func TestRequestRefundOnlyRegistersApplication(t *testing.T) {
	cases := []struct {
		name string
		from int32
		ver  int64
	}{
		{"已履约单", model.StateFulfilled, 5},
		{"已支付但未履约单", model.StatePaid, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRefundHarness(t)
			h.seedForRefund("to_r1", func(o *model.Order) {
				o.State = tc.from
				o.Version = tc.ver
				o.GrantRef = ""
			})

			reply, err := h.req.RequestRefund(requestRefundReq("to_r1", 1001, "", 0, "refund-1", "误购，未使用"))
			if err != nil {
				t.Fatalf("RequestRefund() error = %v", err)
			}
			if reply.GetDuplicated() {
				t.Error("首次申请不该 duplicated")
			}
			got := mustOrder(t, h.db, "to_r1")
			if got.State != model.StateRefundRequested {
				t.Fatalf("state = %s", model.StateName(got.State))
			}
			if got.Version != tc.ver+1 || got.RefundedMinor != 0 || got.PaymentNo != "pay_to_r1" {
				t.Errorf("version/refunded/payment_no = (%d,%d,%q)",
					got.Version, got.RefundedMinor, got.PaymentNo)
			}
			if got.FulfillState != model.FulfillDone || got.GrantRef != "" {
				t.Error("申请退款不该改动履约结论")
			}
			if got.FulfillDetail != "" {
				t.Errorf("fulfill_detail 语义是「最近一次履约/回收摘要」，不该被待退金额占用: %q", got.FulfillDetail)
			}
			rows := ledgerOf(h.db, "to_r1")
			if len(rows) != 1 {
				t.Fatalf("台账行数 = %d", len(rows))
			}
			want := "refund requested by user, full amount_minor=3000, payment_no=pay_to_r1, reason=误购，未使用"
			if rows[0].Reason != want {
				t.Errorf("台账理由 = %q，期望 %q", rows[0].Reason, want)
			}
			if rows[0].FromState != tc.from || rows[0].ToState != model.StateRefundRequested {
				t.Errorf("台账 = (%s,%s)", model.StateName(rows[0].FromState), model.StateName(rows[0].ToState))
			}
			// 钱和权益都还没动：这是「申请 ≠ 退款」的可执行表达
			if len(h.pay.refundCalls) != 0 || len(h.mem.revokeCalls) != 0 || len(h.coin.grantCalls) != 0 {
				t.Error("申请阶段就退了钱或回收了权益")
			}
			assertOnlyLegalTransitions(t, h.db)
		})
	}
}

// TestRequestRefundRejectsPartialAmount 部分退款在本沙箱不可用：任何非零自报金额都拒。
// 这是已知契约缺口（RefundPaymentReq 没有 biz_order_no，也没有按单查消耗的接口），
// 若将来补齐，本用例应随之改成「允许并校验额度」。
func TestRequestRefundRejectsPartialAmount(t *testing.T) {
	h := newRefundHarness(t)
	h.seedForRefund("to_partial", nil)
	for _, amount := range []int64{1, 100, 2999, 3000, 3001, -1} {
		_, err := h.req.RequestRefund(requestRefundReq("to_partial", 1001, "", amount, "refund-partial", "只想退一部分"))
		if !errors.Is(err, model.ErrRefundAmountInvalid) {
			t.Fatalf("amount=%d error = %v，期望 %v", amount, err, model.ErrRefundAmountInvalid)
		}
		if !strings.Contains(err.Error(), "requested="+itoa(amount)) ||
			!strings.Contains(err.Error(), "全额退请传 0") {
			t.Errorf("错误文本要给出可执行的纠正方式: %v", err)
		}
	}
	got := mustOrder(t, h.db, "to_partial")
	if got.State != model.StateFulfilled || got.Version != 5 || len(h.db.events) != 0 {
		t.Errorf("拒了部分退款却动了订单: state=%s version=%d events=%d",
			model.StateName(got.State), got.Version, len(h.db.events))
	}
}

// itoa 只用于把错误文本断言写清楚（错误里是 %d 展开的金额）。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// TestRequestRefundRejectsWrongStates 未支付没有可退的钱；履约结论未定的单退了就是
// 「款权益两清不清」。REFUND_REJECTED 与终态同样不接受新申请。
func TestRequestRefundRejectsWrongStates(t *testing.T) {
	for _, st := range []int32{model.StateCreated, model.StatePaying, model.StateFulfilling,
		model.StateFailed, model.StateCancelled, model.StateRefundApproved, model.StateRefunded,
		model.StateRefundRejected} {
		t.Run(model.StateName(st), func(t *testing.T) {
			h := newRefundHarness(t)
			h.seedForRefund("to_ws", func(o *model.Order) { o.State = st })
			_, err := h.req.RequestRefund(requestRefundReq("to_ws", 1001, "", 0, "refund-w", "想退"))
			if st == model.StateRefundApproved || st == model.StateRefunded {
				// 已在退款流程中：按幂等重放处理（REFUND_REJECTED 不在这两态里）
				if err != nil {
					t.Fatalf("流程中的重复申请应按幂等返回，实得 %v", err)
				}
				return
			}
			if !errors.Is(err, model.ErrRefundNotAccepted) && !errors.Is(err, model.ErrPaymentNoRequired) {
				t.Fatalf("RequestRefund() error = %v", err)
			}
			if st == model.StateFailed && !strings.Contains(err.Error(), "FAILED") {
				t.Errorf("错误文本要给出当前状态: %v", err)
			}
			got := mustOrder(t, h.db, "to_ws")
			if got.State != st || len(h.db.events) != 0 {
				t.Errorf("非法前态却动了订单: state=%s events=%d", model.StateName(got.State), len(h.db.events))
			}
		})
	}
}

// TestRequestRefundInFlightRules 已在 REFUND_REQUESTED：同一 request_id 是重放，
// 换一把 request_id 是「重复申请」必须拒（否则同一单能挂两条退款流程）。
func TestRequestRefundInFlightRules(t *testing.T) {
	h := newRefundHarness(t)
	h.seedForRefund("to_mid", func(o *model.Order) {
		o.State = model.StateRefundRequested
		o.Version = 6
	})
	seedLedger(t, h.db, &model.OrderEvent{
		OrderNo: "to_mid", FromState: model.StateFulfilled, ToState: model.StateRefundRequested,
		Operator: "user", Reason: "refund requested", RequestID: "refund-origin",
	})

	reply, err := h.req.RequestRefund(requestRefundReq("to_mid", 1001, "", 0, "refund-origin", "重放"))
	if err != nil {
		t.Fatalf("同 request_id 重放 error = %v", err)
	}
	if !reply.GetDuplicated() {
		t.Error("同 request_id 必须 duplicated=true")
	}

	if _, err := h.req.RequestRefund(requestRefundReq("to_mid", 1001, "", 0, "refund-other-key", "再提一次")); !errors.Is(err, model.ErrRefundNotAccepted) {
		t.Errorf("换 request_id 的重复申请 error = %v，期望 %v", err, model.ErrRefundNotAccepted)
	}
	got := mustOrder(t, h.db, "to_mid")
	if got.State != model.StateRefundRequested || got.Version != 6 || len(h.db.events) != 1 {
		t.Errorf("重复申请动了订单: state=%s version=%d events=%d",
			model.StateName(got.State), got.Version, len(h.db.events))
	}
}

// TestRequestRefundNeedsPaymentReferenceAndBalance 没有支付单号就没有可退的钱；
// 已全额退过的单没有可退余额。
func TestRequestRefundNeedsPaymentReferenceAndBalance(t *testing.T) {
	t.Run("未绑支付单", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_nopay", func(o *model.Order) { o.PaymentNo = "" })
		_, err := h.req.RequestRefund(requestRefundReq("to_nopay", 1001, "", 0, "r", "想退"))
		if !errors.Is(err, model.ErrPaymentNoRequired) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrPaymentNoRequired)
		}
	})

	t.Run("已全额退过", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_refunded", func(o *model.Order) {
			o.State = model.StatePaid
			o.Version = 3
			o.RefundedMinor = 3000
		})
		_, err := h.req.RequestRefund(requestRefundReq("to_refunded", 1001, "", 0, "r2", "想再退"))
		if !errors.Is(err, model.ErrRefundNothingToRefund) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrRefundNothingToRefund)
		}
		if !strings.Contains(err.Error(), "amount_minor=3000 refunded_minor=3000") {
			t.Errorf("错误文本要给出两侧金额: %v", err)
		}
	})

	t.Run("部分退过仍可退余额", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_left", func(o *model.Order) { o.RefundedMinor = 1000 })
		if _, err := h.req.RequestRefund(requestRefundReq("to_left", 1001, "", 0, "r3", "退剩下的")); err != nil {
			t.Fatalf("error = %v", err)
		}
		if got := ledgerOf(h.db, "to_left")[0]; !strings.Contains(got.Reason, "full amount_minor=2000") {
			t.Errorf("待退金额应为可退余额，实得 %q", got.Reason)
		}
	})
}

// TestRequestRefundAuthorizationAndValidation 越权与不存在同出口；必填项先于读库。
func TestRequestRefundAuthorizationAndValidation(t *testing.T) {
	t.Run("越权与不可区分", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_own", func(o *model.Order) { o.Mid = 2002 })
		_, errA := h.req.RequestRefund(requestRefundReq("to_own", 1001, "", 0, "r", "想退"))
		_, errB := h.req.RequestRefund(requestRefundReq("to_missing", 1001, "", 0, "r", "想退"))
		if !errors.Is(errA, model.ErrOrderNotFound) || !errors.Is(errB, model.ErrOrderNotFound) {
			t.Fatalf("error = (%v,%v)，都应是 not-found", errA, errB)
		}
		if errA.Error() != errB.Error() {
			t.Errorf("两条分支可区分，能被用来枚举订单号: %q vs %q", errA, errB)
		}
	})

	t.Run("运营代提以 operator 记账", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_opsref", nil)
		if _, err := h.req.RequestRefund(requestRefundReq("to_opsref", 0, "ops_007", 0, "r", "客诉退款")); err != nil {
			t.Fatalf("error = %v", err)
		}
		row := ledgerOf(h.db, "to_opsref")[0]
		if row.Operator != "ops_007" || !strings.HasPrefix(row.Reason, "refund requested by ops_007") {
			t.Errorf("台账 = (%q,%q)", row.Operator, row.Reason)
		}
	})

	t.Run("理由里的凭据不进台账", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_leakr", nil)
		if _, err := h.req.RequestRefund(requestRefundReq("to_leakr", 1001, "", 0, "r",
			"用户提供了 token=abc123 作为凭证")); err != nil {
			t.Fatalf("error = %v", err)
		}
		row := ledgerOf(h.db, "to_leakr")[0]
		if strings.Contains(row.Reason, "abc123") || !strings.Contains(row.Reason, "redacted") {
			t.Errorf("台账理由 = %q", row.Reason)
		}
	})

	cases := []struct {
		name string
		req  *rpc.RequestRefundReq
		want error
	}{
		{"缺 order_no", requestRefundReq(" ", 1001, "", 0, "r", "理由"), model.ErrOrderNoRequired},
		{"缺 request_id", requestRefundReq("to_v", 1001, "", 0, " ", "理由"), model.ErrRequestIdRequired},
		{"缺 reason", requestRefundReq("to_v", 1001, "", 0, "r", "  "), model.ErrReasonRequired},
		{"无主体", requestRefundReq("to_v", 0, "   ", 0, "r", "理由"), model.ErrSubjectRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRefundHarness(t)
			h.seedForRefund("to_v", nil)
			if _, err := h.req.RequestRefund(tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v，期望 %v", err, tc.want)
			}
			if got := mustOrder(t, h.db, "to_v"); got.State != model.StateFulfilled || len(h.db.events) != 0 {
				t.Error("入参非法却动了订单")
			}
		})
	}
}

// TestRequestRefundConcurrentMissReloads CAS 未命中按并发重放回读，不覆盖胜者。
func TestRequestRefundConcurrentMissReloads(t *testing.T) {
	h := newRefundHarness(t)
	h.seedForRefund("to_rcas", nil)
	h.db.raceHook = func(o *model.Order, from, to int32) {
		if to == model.StateRefundRequested {
			o.State = model.StateCancelled
			o.Version = 9
		}
	}
	reply, err := h.req.RequestRefund(requestRefundReq("to_rcas", 1001, "", 0, "r", "并发申请"))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !reply.GetDuplicated() || reply.GetOrder().GetState() != rpc.OrderState_ORDER_STATE_CANCELLED {
		t.Errorf("回复 = (%v,%v)，期望 duplicated + 胜者的 CANCELLED",
			reply.GetDuplicated(), reply.GetOrder().GetState())
	}
	if got := mustOrder(t, h.db, "to_rcas"); got.State != model.StateCancelled || len(h.db.events) != 0 {
		t.Errorf("败者覆盖了胜者: state=%s events=%d", model.StateName(got.State), len(h.db.events))
	}
}

// ------------------------------------------------------------ ApproveRefund ----

// refundRequestedOrder 造一张「已申请、等审批」的会员单（version 6）。
func (h *refundHarness) refundRequestedOrder(orderNo string, tune func(*model.Order)) *model.Order {
	return h.seedForRefund(orderNo, func(o *model.Order) {
		o.State = model.StateRefundRequested
		o.Version = 6
		if tune != nil {
			tune(o)
		}
	})
}

// TestApproveRefundRefundsThenRevokesThenCloses 三段顺序与最终落点。
func TestApproveRefundRefundsThenRevokesThenCloses(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_a1", nil)

	reply, err := h.appr.ApproveRefund(approveReq("to_a1", 6))
	if err != nil {
		t.Fatalf("ApproveRefund() error = %v", err)
	}
	if reply.GetDuplicated() {
		t.Error("首次审批不该 duplicated")
	}
	if reply.GetRefundNo() != "rf_pay_to_a1" {
		t.Errorf("refund_no = %q", reply.GetRefundNo())
	}
	if !strings.Contains(reply.GetRevokeDetail(), "会员权益已回收") ||
		!strings.Contains(reply.GetRevokeDetail(), "revoke_grant_id=9001") {
		t.Errorf("revoke_detail = %q", reply.GetRevokeDetail())
	}
	got := mustOrder(t, h.db, "to_a1")
	if got.State != model.StateRefunded {
		t.Fatalf("state = %s，期望 REFUNDED", model.StateName(got.State))
	}
	if got.RefundedMinor != 3000 || got.Version != 8 {
		t.Errorf("refunded_minor/version = (%d,%d)，期望 (3000,8)", got.RefundedMinor, got.Version)
	}
	if got.FulfillDetail != "" {
		t.Errorf("退款闭环后该清空差异摘要，实得 %q", got.FulfillDetail)
	}
	if got.PaymentNo != "pay_to_a1" {
		t.Errorf("payment_no 被改写了: %q", got.PaymentNo)
	}

	// --- 退钱：只走余额，幂等键由订单号派生 ---
	if len(h.pay.refundCalls) != 1 {
		t.Fatalf("RefundPayment 调用次数 = %d，期望 1", len(h.pay.refundCalls))
	}
	rc := h.pay.refundCalls[0]
	if rc.GetPaymentNo() != "pay_to_a1" || rc.GetAmountMinor() != 3000 || !rc.GetToBalance() {
		t.Errorf("RefundPayment 参数 = (%q,%d,%v)", rc.GetPaymentNo(), rc.GetAmountMinor(), rc.GetToBalance())
	}
	if rc.GetRequestId() != "refund_to_a1" {
		t.Errorf("退款幂等键 = %q，期望 refund_to_a1（订单号派生，重试不重复退）", rc.GetRequestId())
	}
	if !strings.Contains(rc.GetReason(), "order refund to_a1") {
		t.Errorf("退款理由 = %q", rc.GetReason())
	}

	// --- 回收：ClearRemaining=true 且不带 DeltaDays ---
	if len(h.mem.revokeCalls) != 1 {
		t.Fatalf("RevokeMembership 调用次数 = %d", len(h.mem.revokeCalls))
	}
	rv := h.mem.revokeCalls[0]
	if rv.GetRequestId() != "revoke_to_a1" || !rv.GetClearRemaining() || rv.GetDeltaDays() != 0 {
		t.Errorf("RevokeMembership 参数 = (%q,clear=%v,delta=%d)",
			rv.GetRequestId(), rv.GetClearRemaining(), rv.GetDeltaDays())
	}
	if rv.GetVipType() != memberrpc.VipType_VIP_TYPE_PREMIUM || rv.GetMid() != 1001 {
		t.Errorf("RevokeMembership mid/vip = (%d,%d)", rv.GetMid(), rv.GetVipType())
	}
	if rv.GetOperator() != "ops_007" {
		t.Errorf("回收 operator = %q，审批人必须透传到下游", rv.GetOperator())
	}
	if len(h.coin.grantCalls) != 0 {
		t.Error("会员单却去扣了硬币")
	}

	want := [][2]string{{"REFUND_REQUESTED", "REFUND_APPROVED"}, {"REFUND_APPROVED", "REFUNDED"}}
	pairs := ledgerPairs(h.db, "to_a1")
	if len(pairs) != 2 || pairs[0] != want[0] || pairs[1] != want[1] {
		t.Fatalf("台账链 = %v，期望 %v", pairs, want)
	}
	rows := ledgerOf(h.db, "to_a1")
	if !strings.Contains(rows[0].Reason, "add_refunded_minor=3000") ||
		!strings.Contains(rows[0].Reason, "refund_no=rf_pay_to_a1") ||
		!strings.Contains(rows[0].Reason, "核对无误") {
		t.Errorf("退款台账 = %q，要能回答「退了多少、哪张退款单、谁批的、为什么」", rows[0].Reason)
	}
	if !strings.Contains(rows[1].Reason, "refunded and entitlement revoked") {
		t.Errorf("闭环台账 = %q", rows[1].Reason)
	}
	assertOnlyLegalTransitions(t, h.db)
}

// TestApproveRefundRefundFailureChangesNothing 退钱失败：订单原地不动，什么都没改，
// 可以直接用同一把派生键重试。
func TestApproveRefundRefundFailureChangesNothing(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakePayment)
		want  error
	}{
		{"RPC 报错", func(p *fakePayment) { p.refundErr = status.Error(codes.Unavailable, "payment down") }, nil},
		{"下游回未成功", func(p *fakePayment) {
			p.refundState = paymentrpc.RefundState_REFUND_STATE_FAILED
		}, model.ErrRefundNotAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRefundHarness(t)
			h.refundRequestedOrder("to_a2", nil)
			tc.setup(h.pay)

			_, err := h.appr.ApproveRefund(approveReq("to_a2", 6))
			if err == nil {
				t.Fatal("退钱失败却返回成功")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error = %v，期望 %v", err, tc.want)
			}
			got := mustOrder(t, h.db, "to_a2")
			if got.State != model.StateRefundRequested || got.Version != 6 || got.RefundedMinor != 0 {
				t.Errorf("退钱失败却动了订单: state=%s version=%d refunded=%d",
					model.StateName(got.State), got.Version, got.RefundedMinor)
			}
			if len(h.db.events) != 0 {
				t.Errorf("什么都没发生却写了 %d 行台账", len(h.db.events))
			}
			if len(h.mem.revokeCalls) != 0 {
				t.Error("钱没退成就先收权益")
			}
		})
	}
}

// TestApproveRefundStaysApprovedWhenRevokeFails 款已退、权益收不回：
// 停在 REFUND_APPROVED 并留同态台账 + fulfill_detail 差异，绝不标 REFUNDED。
func TestApproveRefundStaysApprovedWhenRevokeFails(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_a3", nil)
	h.mem.revokeErr = status.Error(codes.FailedPrecondition, "membership refuses")

	reply, err := h.appr.ApproveRefund(approveReq("to_a3", 6))
	if err != nil {
		t.Fatalf("回收失败要回结论而不是错误（带错误的 gRPC 回复会被丢弃）: %v", err)
	}
	if !strings.HasPrefix(reply.GetRevokeDetail(), "款已退、权益未回收：") {
		t.Errorf("revoke_detail = %q", reply.GetRevokeDetail())
	}
	if reply.GetRefundNo() != "rf_pay_to_a3" {
		t.Errorf("refund_no = %q，钱确实退了，单号必须给出", reply.GetRefundNo())
	}
	got := mustOrder(t, h.db, "to_a3")
	if got.State != model.StateRefundApproved {
		t.Fatalf("state = %s，期望停在 REFUND_APPROVED", model.StateName(got.State))
	}
	if got.RefundedMinor != 3000 {
		t.Errorf("refunded_minor = %d，退款事实必须记账（它是重试时判断是否退过钱的唯一本地依据）", got.RefundedMinor)
	}
	if !strings.Contains(got.FulfillDetail, "款已退、权益未回收") ||
		!strings.Contains(got.FulfillDetail, "membership refuses") {
		t.Errorf("fulfill_detail = %q", got.FulfillDetail)
	}
	pairs := ledgerPairs(h.db, "to_a3")
	want := [][2]string{{"REFUND_REQUESTED", "REFUND_APPROVED"}, {"REFUND_APPROVED", "REFUND_APPROVED"}}
	if len(pairs) != 2 || pairs[1] != want[1] {
		t.Fatalf("台账链 = %v，期望第二行是同态留证 %v", pairs, want[1])
	}
	last := ledgerOf(h.db, "to_a3")[1]
	if !strings.HasPrefix(last.Reason, "revoke failed: ") {
		t.Errorf("同态台账理由 = %q", last.Reason)
	}
	// 停在 REFUND_APPROVED 后不能再退一次钱
	if len(h.pay.refundCalls) != 1 {
		t.Errorf("RefundPayment 调用 %d 次", len(h.pay.refundCalls))
	}
	assertOnlyLegalTransitions(t, h.db)
}

// TestApproveRefundOnApprovedIsReentrantNotDuplicated REFUND_APPROVED 是重试入口：
// 必须真的再跑一次回收（若被 request_id 短路成 duplicated，权益就永远收不回来了）。
func TestApproveRefundOnApprovedIsReentrantNotDuplicated(t *testing.T) {
	h := newRefundHarness(t)
	h.seedForRefund("to_a4", func(o *model.Order) {
		o.State = model.StateRefundApproved
		o.Version = 8
		o.RefundedMinor = 3000
		o.FulfillDetail = "款已退、权益未回收：上一次下游拒绝"
	})
	h.pay.listRefunds = []*paymentrpc.RefundInfo{
		{RefundNo: "rf_pay_to_a4", PaymentNo: "pay_to_a4", RequestId: "refund_to_a4", AmountMinor: 3000},
		{RefundNo: "rf_other", PaymentNo: "pay_to_a4", RequestId: "unrelated_key", AmountMinor: 1},
	}

	reply, err := h.appr.ApproveRefund(approveReq("to_a4", 8))
	if err != nil {
		t.Fatalf("ApproveRefund() error = %v", err)
	}
	if reply.GetDuplicated() {
		t.Error("REFUND_APPROVED 重试不能按幂等短路")
	}
	if reply.GetRefundNo() != "rf_pay_to_a4" {
		t.Errorf("refund_no = %q，期望按派生幂等键回源命中，而不是取列表第一条", reply.GetRefundNo())
	}
	if len(h.pay.refundCalls) != 0 {
		t.Errorf("重试又退了一次钱: %d 次", len(h.pay.refundCalls))
	}
	if len(h.mem.revokeCalls) != 1 {
		t.Fatalf("重试必须补回收，实得 %d 次", len(h.mem.revokeCalls))
	}
	got := mustOrder(t, h.db, "to_a4")
	if got.State != model.StateRefunded || got.RefundedMinor != 3000 || got.FulfillDetail != "" {
		t.Errorf("state/refunded/detail = (%s,%d,%q)",
			model.StateName(got.State), got.RefundedMinor, got.FulfillDetail)
	}
}

// TestApproveRefundRejectsStaleExpectedVersion 审批类接口不接受「版本过期就顺着改」。
func TestApproveRefundRejectsStaleExpectedVersion(t *testing.T) {
	h := newRefundHarness(t)
	h.seedForRefund("to_a5", func(o *model.Order) {
		o.State = model.StateRefundApproved
		o.Version = 8
		o.RefundedMinor = 3000
	})
	_, err := h.appr.ApproveRefund(approveReq("to_a5", 7))
	if !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("error = %v，期望 %v", err, model.ErrConcurrentUpdate)
	}
	if !strings.Contains(err.Error(), "expected_version=7") || !strings.Contains(err.Error(), "current_version=8") {
		t.Errorf("错误文本要给出两个版本让人回读: %v", err)
	}
	if len(h.mem.revokeCalls) != 0 || len(h.pay.refundCalls) != 0 {
		t.Error("位点过期却动了下游")
	}
	got := mustOrder(t, h.db, "to_a5")
	if got.State != model.StateRefundApproved || got.Version != 8 || len(h.db.events) != 0 {
		t.Errorf("位点过期却动了订单: state=%s version=%d", model.StateName(got.State), got.Version)
	}
}

// TestApproveRefundOnRefundedIsIdempotent 已结案件：幂等重放，不再退钱也不再回收。
func TestApproveRefundOnRefundedIsIdempotent(t *testing.T) {
	h := newRefundHarness(t)
	h.seedForRefund("to_a6", func(o *model.Order) {
		o.State = model.StateRefunded
		o.Version = 8
		o.RefundedMinor = 3000
	})
	h.pay.listRefunds = []*paymentrpc.RefundInfo{
		{RefundNo: "rf_pay_to_a6", RequestId: "refund_to_a6"},
	}

	reply, err := h.appr.ApproveRefund(approveReq("to_a6", 8))
	if err != nil {
		t.Fatalf("ApproveRefund() error = %v", err)
	}
	if !reply.GetDuplicated() {
		t.Error("REFUNDED 重放必须 duplicated=true")
	}
	if reply.GetRefundNo() != "rf_pay_to_a6" {
		t.Errorf("refund_no = %q，期望回源拿到的单号", reply.GetRefundNo())
	}
	if !strings.Contains(reply.GetRevokeDetail(), "幂等重放") {
		t.Errorf("revoke_detail = %q", reply.GetRevokeDetail())
	}
	if len(h.pay.refundCalls) != 0 || len(h.mem.revokeCalls) != 0 || len(h.db.events) != 0 {
		t.Error("幂等重放动了钱、权益或台账")
	}
	got := mustOrder(t, h.db, "to_a6")
	if got.State != model.StateRefunded || got.Version != 8 {
		t.Errorf("重放改动了订单: state=%s version=%d", model.StateName(got.State), got.Version)
	}

	// 回源查不到就不编单号：宁可少一个字段
	h2 := newRefundHarness(t)
	h2.seedForRefund("to_a7", func(o *model.Order) {
		o.State = model.StateRefunded
		o.Version = 8
		o.RefundedMinor = 3000
	})
	r2, err := h2.appr.ApproveRefund(approveReq("to_a7", 8))
	if err != nil {
		t.Fatalf("ApproveRefund() error = %v", err)
	}
	if r2.GetRefundNo() != "" {
		t.Errorf("无凭据时 refund_no = %q，期望空串而不是编一个", r2.GetRefundNo())
	}
}

// TestApproveRefundRecordsWhatPaymentActuallyMoved 退款面额以 payment 回的实际值记账，
// 两侧分叉时不猜数、不改订单币种。
func TestApproveRefundRecordsWhatPaymentActuallyMoved(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_a8", nil)
	h.pay.refundAmount = 2999
	h.pay.refundCur = "USD" // 币种分叉：只告警，不擅自换算

	reply, err := h.appr.ApproveRefund(approveReq("to_a8", 6))
	if err != nil {
		t.Fatalf("ApproveRefund() error = %v", err)
	}
	got := mustOrder(t, h.db, "to_a8")
	if got.RefundedMinor != 2999 {
		t.Errorf("refunded_minor = %d，期望记下游实际退的 2999", got.RefundedMinor)
	}
	if got.Currency != "CNY" || got.AmountMinor != 3000 {
		t.Errorf("订单快照被改写了: currency=%q amount=%d", got.Currency, got.AmountMinor)
	}
	if got.State != model.StateRefunded {
		t.Errorf("state = %s", model.StateName(got.State))
	}
	row := ledgerOf(h.db, "to_a8")[0]
	if !strings.Contains(row.Reason, "add_refunded_minor=2999") {
		t.Errorf("台账 = %q，要记录实际入账的增量", row.Reason)
	}
	if reply.GetRefundNo() != "rf_pay_to_a8" {
		t.Errorf("refund_no = %q，期望下游回的单号原样透出", reply.GetRefundNo())
	}
	if reply.GetOrder().GetState() != rpc.OrderState_ORDER_STATE_REFUNDED {
		t.Errorf("reply.order.state = %v", reply.GetOrder().GetState())
	}
}

// TestApproveRefundRefusesWhenNothingLeftToRefund 本地已记满退款额却仍停在
// REFUND_REQUESTED（只有人工改账才可能出现）：审批在退钱之前就拒，不动钱也不动权益。
// 这条与下一个用例合起来定义 refunded_minor 的两条分叉：记满 = 拒；记了但没记满 = 只补推进。
func TestApproveRefundRefusesWhenNothingLeftToRefund(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_a9", func(o *model.Order) { o.RefundedMinor = 3000 })

	_, err := h.appr.ApproveRefund(approveReq("to_a9", 6))
	if !errors.Is(err, model.ErrRefundNothingToRefund) {
		t.Fatalf("ApproveRefund() error = %v，期望 %v", err, model.ErrRefundNothingToRefund)
	}
	if !strings.Contains(err.Error(), "amount_minor=3000 refunded_minor=3000") {
		t.Errorf("错误文本要给两侧数字: %v", err)
	}
	if len(h.pay.refundCalls) != 0 || len(h.mem.revokeCalls) != 0 {
		t.Errorf("拒绝后仍动了钱或权益: refund=%d revoke=%d", len(h.pay.refundCalls), len(h.mem.revokeCalls))
	}
	got := mustOrder(t, h.db, "to_a9")
	if got.State != model.StateRefundRequested || got.Version != 6 || len(ledgerOf(h.db, "to_a9")) != 0 {
		t.Errorf("订单被动过: state=%s version=%d ledger=%d",
			model.StateName(got.State), got.Version, len(ledgerOf(h.db, "to_a9")))
	}
}

// TestApproveRefundSkipsRefundWhenPartiallyBooked REFUND_REQUESTED 但本地已记过部分退款额
// （人工改账）：不再退钱（派生幂等键也会挡住重复退款），只补推进与回收，
// 退款单号回源 payment 按派生键精确匹配。
//
// 顺带锁住一个可诊断的不一致：这条路走完是 REFUNDED 而 refunded_minor 仍小于 amount_minor，
// 说明本地记账与终态对不上，只能靠人工核对，代码不会擅自把差额补记上去。
func TestApproveRefundSkipsRefundWhenPartiallyBooked(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_a9b", func(o *model.Order) { o.RefundedMinor = 1000 })
	h.pay.listRefunds = []*paymentrpc.RefundInfo{
		// 无关那笔排在前面：证明单号是按派生幂等键挑的，不是取列表第一条
		{RefundNo: "rf_other", PaymentNo: "pay_to_a9b", RequestId: "refund_something_else"},
		{RefundNo: "rf_earlier", PaymentNo: "pay_to_a9b", RequestId: "refund_to_a9b"},
	}

	reply, err := h.appr.ApproveRefund(approveReq("to_a9b", 6))
	if err != nil {
		t.Fatalf("ApproveRefund() error = %v", err)
	}
	if len(h.pay.refundCalls) != 0 {
		t.Errorf("本地已记过退款额却退了第二次: %d 次", len(h.pay.refundCalls))
	}
	if reply.GetRefundNo() != "rf_earlier" {
		t.Errorf("refund_no = %q，期望按 refund_<order_no> 键回源匹配", reply.GetRefundNo())
	}
	got := mustOrder(t, h.db, "to_a9b")
	if got.State != model.StateRefunded || got.RefundedMinor != 1000 {
		t.Errorf("state/refunded = (%s,%d)，期望 REFUNDED 且不退第二次钱、也不补记差额",
			model.StateName(got.State), got.RefundedMinor)
	}
	row := ledgerOf(h.db, "to_a9b")[0]
	if !strings.Contains(row.Reason, "add_refunded_minor=0") {
		t.Errorf("台账 = %q，本次没有新增退款额就该记 0", row.Reason)
	}
	if len(h.mem.revokeCalls) != 1 {
		t.Errorf("revoke 调用次数 = %d，权益回收必须照常补做", len(h.mem.revokeCalls))
	}
	assertOnlyLegalTransitions(t, h.db)
}

// TestApproveRefundWithoutGrantRefClosesImmediately 未履约的单没有东西可回收，
// 退完钱直接 REFUNDED，并且不调下游回收。
func TestApproveRefundWithoutGrantRefClosesImmediately(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_aa", func(o *model.Order) {
		o.State = model.StateRefundRequested
		o.Version = 4
		o.GrantRef = ""
	})

	reply, err := h.appr.ApproveRefund(approveReq("to_aa", 4))
	if err != nil {
		t.Fatalf("ApproveRefund() error = %v", err)
	}
	if !strings.Contains(reply.GetRevokeDetail(), "无需回收") {
		t.Errorf("revoke_detail = %q", reply.GetRevokeDetail())
	}
	if len(h.mem.revokeCalls) != 0 || len(h.mem.grantCalls) != 0 {
		t.Error("没有发放记录却去回收")
	}
	if got := mustOrder(t, h.db, "to_aa"); got.State != model.StateRefunded {
		t.Errorf("state = %s", model.StateName(got.State))
	}
}

// TestApproveRefundCoinPackReclaimsCoins 硬币包回收 = 负数 delta 的整包扣回，
// 与发放用同一条 flow 类型、另一把 revoke_ 幂等键（不与 grant_ 撞同一个键）。
func TestApproveRefundCoinPackReclaimsCoins(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_ab", func(o *model.Order) {
		o.BizType = model.BizCoinPack
		o.CoinAmount = 300
		o.DurationDays = 0
		o.GrantRef = "coin_flow:5001"
	})

	reply, err := h.appr.ApproveRefund(approveReq("to_ab", 6))
	if err != nil {
		t.Fatalf("ApproveRefund() error = %v", err)
	}
	if !strings.Contains(reply.GetRevokeDetail(), "硬币已扣回 300 枚") {
		t.Errorf("revoke_detail = %q", reply.GetRevokeDetail())
	}
	if len(h.coin.grantCalls) != 1 {
		t.Fatalf("GrantCoin 调用次数 = %d", len(h.coin.grantCalls))
	}
	gc := h.coin.grantCalls[0]
	if gc.GetDelta() != -300 || gc.GetRequestId() != "revoke_to_ab" || gc.GetBizNo() != "to_ab" {
		t.Errorf("GrantCoin 参数 = (%d,%q,%q)", gc.GetDelta(), gc.GetRequestId(), gc.GetBizNo())
	}
	if len(h.mem.revokeCalls) != 0 {
		t.Error("硬币包却去回收会员")
	}
	if got := mustOrder(t, h.db, "to_ab"); got.State != model.StateRefunded {
		t.Errorf("state = %s", model.StateName(got.State))
	}
}

// TestApproveRefundCoinAlreadySpentStopsForManualHandling 硬币已被花掉时 coin 会拒绝
// 把余额扣成负数：这里必须停在 REFUND_APPROVED 等人工，而不是假装回收成功。
func TestApproveRefundCoinAlreadySpentStopsForManualHandling(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_ac", func(o *model.Order) {
		o.BizType = model.BizCoinPack
		o.CoinAmount = 300
		o.DurationDays = 0
		o.GrantRef = "coin_flow:5001"
	})
	h.coin.grantErr = status.Error(codes.FailedPrecondition, "余额不足，不能扣成负数")

	reply, err := h.appr.ApproveRefund(approveReq("to_ac", 6))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(reply.GetRevokeDetail(), "硬币可能已消耗") {
		t.Errorf("revoke_detail = %q", reply.GetRevokeDetail())
	}
	got := mustOrder(t, h.db, "to_ac")
	if got.State != model.StateRefundApproved || got.RefundedMinor != 3000 {
		t.Errorf("state/refunded = (%s,%d)，钱退了权益没收回，必须停在 REFUND_APPROVED",
			model.StateName(got.State), got.RefundedMinor)
	}
	assertOnlyLegalTransitions(t, h.db)
}

// TestApproveRefundMissingDownstreamConfigured 缺 payment/coin 时显式失败，
// 绝不留下「已退款」的假账。
func TestApproveRefundMissingDownstreamConfigured(t *testing.T) {
	t.Run("无 payment", func(t *testing.T) {
		h := newRefundHarness(t)
		h.refundRequestedOrder("to_ad", nil)
		h.svc.Payment = nil
		_, err := h.appr.ApproveRefund(approveReq("to_ad", 6))
		if !errors.Is(err, model.ErrPaymentNotConfigured) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrPaymentNotConfigured)
		}
		got := mustOrder(t, h.db, "to_ad")
		if got.State != model.StateRefundRequested || got.RefundedMinor != 0 || len(h.db.events) != 0 {
			t.Errorf("缺下游却动了订单: state=%s refunded=%d", model.StateName(got.State), got.RefundedMinor)
		}
	})

	t.Run("硬币包无 coin 客户端", func(t *testing.T) {
		h := newRefundHarness(t)
		h.refundRequestedOrder("to_ae", func(o *model.Order) {
			o.BizType = model.BizCoinPack
			o.CoinAmount = 300
			o.GrantRef = "coin_flow:5001"
		})
		h.svc.Coin = nil
		reply, err := h.appr.ApproveRefund(approveReq("to_ae", 6))
		if err != nil {
			t.Fatalf("钱已退，回收失败应回结论而不是错误: %v", err)
		}
		if !strings.Contains(reply.GetRevokeDetail(), "coin 未配置") {
			t.Errorf("revoke_detail = %q", reply.GetRevokeDetail())
		}
		if got := mustOrder(t, h.db, "to_ae"); got.State != model.StateRefundApproved {
			t.Errorf("state = %s，缺客户端绝不能标 REFUNDED", model.StateName(got.State))
		}
	})
}

// TestApproveRefundValidationAndStateGates 必填项与「只有退款流程中的单可审批」。
func TestApproveRefundValidationAndStateGates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.ApproveRefundReq)
		want   error
	}{
		{"缺 order_no", func(r *rpc.ApproveRefundReq) { r.OrderNo = " " }, model.ErrOrderNoRequired},
		{"缺 operator", func(r *rpc.ApproveRefundReq) { r.Operator = "" }, model.ErrOperatorRequired},
		{"缺 reason", func(r *rpc.ApproveRefundReq) { r.Reason = "  " }, model.ErrReasonRequired},
		{"缺 request_id", func(r *rpc.ApproveRefundReq) { r.RequestId = "" }, model.ErrRequestIdRequired},
		{"缺 expected_version", func(r *rpc.ApproveRefundReq) { r.ExpectedVersion = 0 }, model.ErrExpectedVersionRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRefundHarness(t)
			h.refundRequestedOrder("to_af", nil)
			in := approveReq("to_af", 6)
			tc.mutate(in)
			if _, err := h.appr.ApproveRefund(in); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v，期望 %v", err, tc.want)
			}
			if got := mustOrder(t, h.db, "to_af"); got.State != model.StateRefundRequested || len(h.pay.refundCalls) != 0 {
				t.Error("入参非法却动了订单或退了钱")
			}
		})
	}

	t.Run("非退款流程中的单不可审批", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_ag", func(o *model.Order) { o.State = model.StatePaid; o.Version = 3 })
		_, err := h.appr.ApproveRefund(approveReq("to_ag", 3))
		if !errors.Is(err, model.ErrRefundNotRequested) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrRefundNotRequested)
		}
		if len(h.pay.refundCalls) != 0 {
			t.Error("没申请就退了钱")
		}
	})

	t.Run("REFUND_REJECTED 不可审批", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_ah", func(o *model.Order) { o.State = model.StateRefundRejected; o.Version = 7 })
		_, err := h.appr.ApproveRefund(approveReq("to_ah", 7))
		if !errors.Is(err, model.ErrRefundNotRequested) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrRefundNotRequested)
		}
	})

	t.Run("申请了但没绑支付单", func(t *testing.T) {
		h := newRefundHarness(t)
		h.refundRequestedOrder("to_ai", func(o *model.Order) { o.PaymentNo = "" })
		if _, err := h.appr.ApproveRefund(approveReq("to_ai", 6)); !errors.Is(err, model.ErrPaymentNoRequired) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrPaymentNoRequired)
		}
	})

	t.Run("订单不存在", func(t *testing.T) {
		h := newRefundHarness(t)
		if _, err := h.appr.ApproveRefund(approveReq("to_none", 1)); !errors.Is(err, model.ErrOrderNotFound) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrOrderNotFound)
		}
	})
}

// TestApproveRefundCasMissAfterMoneyMoved 钱已退、订单推进未命中（RowsAffected==0）：
// 本地不记退款额、不推进，返回错误；重试靠同一把派生幂等键自愈。
// 这是本服务最危险的窗口，必须留下可定位的断言。
func TestApproveRefundCasMissAfterMoneyMoved(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_aj", nil)
	h.db.raceHook = func(o *model.Order, from, to int32) {
		if from == model.StateRefundRequested && to == model.StateRefundApproved {
			o.Version = 42 // 并发方先动了订单
		}
	}

	_, err := h.appr.ApproveRefund(approveReq("to_aj", 6))
	if !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("error = %v，期望 %v", err, model.ErrConcurrentUpdate)
	}
	if len(h.pay.refundCalls) != 1 {
		t.Fatalf("RefundPayment 调用次数 = %d", len(h.pay.refundCalls))
	}
	got := mustOrder(t, h.db, "to_aj")
	if got.State != model.StateRefundRequested || got.Version != 42 || got.RefundedMinor != 0 {
		t.Errorf("CAS 未命中本地结论 = (state=%s,version=%d,refunded=%d)",
			model.StateName(got.State), got.Version, got.RefundedMinor)
	}
	if len(h.db.events) != 0 {
		t.Errorf("推进失败却写了 %d 行台账", len(h.db.events))
	}
	if len(h.mem.revokeCalls) != 0 {
		t.Error("订单没进 REFUND_APPROVED 就回收权益")
	}

	// 重试：用最新位点，派生幂等键让 payment 判重放，不重复退钱
	h.pay.duplicated = true
	h.db.raceHook = nil
	if _, err := h.appr.ApproveRefund(approveReq("to_aj", 42)); err != nil {
		t.Fatalf("重试 error = %v", err)
	}
	got = mustOrder(t, h.db, "to_aj")
	if got.State != model.StateRefunded || got.RefundedMinor != 3000 {
		t.Errorf("自愈后 state/refunded = (%s,%d)", model.StateName(got.State), got.RefundedMinor)
	}
	if h.pay.refundCalls[1].GetRequestId() != h.pay.refundCalls[0].GetRequestId() {
		t.Error("重试必须复用同一把退款幂等键")
	}
	assertOnlyLegalTransitions(t, h.db)
}

// TestApproveRefundLedgerFailureRollsBackRefundBookkeeping 推进事务里台账写失败：
// 主表状态与 refunded_minor 一起回滚，退款事实只能靠重试补记（钱确实已在下游退出）。
func TestApproveRefundLedgerFailureRollsBackRefundBookkeeping(t *testing.T) {
	h := newRefundHarness(t)
	h.refundRequestedOrder("to_ak", nil)
	h.db.failOn = &failOn{statePair: pairName(model.StateRefundRequested, model.StateRefundApproved), attempts: 1}

	_, err := h.appr.ApproveRefund(approveReq("to_ak", 6))
	if err == nil {
		t.Fatal("台账失败却返回成功")
	}
	got := mustOrder(t, h.db, "to_ak")
	if got.State != model.StateRefundRequested || got.Version != 6 || got.RefundedMinor != 0 {
		t.Errorf("回滚不彻底: state=%s version=%d refunded=%d",
			model.StateName(got.State), got.Version, got.RefundedMinor)
	}
	if len(h.db.events) != 0 || len(h.mem.revokeCalls) != 0 {
		t.Errorf("半行状态: events=%d revoke=%d", len(h.db.events), len(h.mem.revokeCalls))
	}
	assertOnlyLegalTransitions(t, h.db)
}

// ------------------------------------------------------------ RejectRefund ----

// TestRejectRefundGoesBackToOriginState 驳回按「回到申请前的原状态」实现，
// 事实由台账留证；REFUND_REJECTED 不进入（它没有出边，走到就是死胡同）。
func TestRejectRefundGoesBackToOriginState(t *testing.T) {
	for _, origin := range []int32{model.StateFulfilled, model.StatePaid} {
		t.Run("原状态 "+model.StateName(origin), func(t *testing.T) {
			h := newRefundHarness(t)
			h.refundRequestedOrder("to_rj", func(o *model.Order) { o.Version = 7 })
			seedLedger(t, h.db, &model.OrderEvent{
				OrderNo: "to_rj", FromState: origin, ToState: model.StateRefundRequested,
				Operator: "user", Reason: "refund requested", RequestID: "req-to_rj",
			})

			reply, err := h.rej.RejectRefund(rejectReq("to_rj", "reject-1"))
			if err != nil {
				t.Fatalf("RejectRefund() error = %v", err)
			}
			if reply.GetDuplicated() {
				t.Error("首次驳回不该 duplicated")
			}
			got := mustOrder(t, h.db, "to_rj")
			if got.State != origin {
				t.Fatalf("state = %s，期望回到 %s", model.StateName(got.State), model.StateName(origin))
			}
			if got.Version != 8 {
				t.Errorf("version = %d，期望 8", got.Version)
			}
			if got.RefundedMinor != 0 {
				t.Errorf("驳回却记了退款额: %d", got.RefundedMinor)
			}
			rows := ledgerOf(h.db, "to_rj")
			last := rows[len(rows)-1]
			if last.FromState != model.StateRefundRequested || last.ToState != origin {
				t.Errorf("驳回台账 = (%s,%s)", model.StateName(last.FromState), model.StateName(last.ToState))
			}
			if !strings.HasPrefix(last.Reason, "refund rejected by ops_007: ") ||
				!strings.Contains(last.Reason, "不符合退款条件") {
				t.Errorf("台账理由 = %q，驳回事实只能靠它留证", last.Reason)
			}
			if last.RequestID != "reject-1" || last.Operator != "ops_007" {
				t.Errorf("台账 operator/request_id = (%q,%q)", last.Operator, last.RequestID)
			}
			// 驳回不能碰钱，也不能碰权益
			if len(h.pay.refundCalls) != 0 || len(h.mem.revokeCalls) != 0 || len(h.coin.grantCalls) != 0 {
				t.Error("驳回动作了资金或权益")
			}
			// 全程没有进入 REFUND_REJECTED
			for _, e := range ledgerPairs(h.db, "to_rj") {
				if e[1] == "REFUND_REJECTED" {
					t.Errorf("出现了 REFUND_REJECTED 落点: %v", e)
				}
			}
			assertOnlyLegalTransitions(t, h.db)
		})
	}
}

// TestRejectRefundNeedsLedgerOrigin 原状态不猜：台账里没有进入 REFUND_REQUESTED 的行，
// 或者那行的 from_state 本身不是合法前态，都必须停下人工核对。
func TestRejectRefundNeedsLedgerOrigin(t *testing.T) {
	t.Run("没有申请记录", func(t *testing.T) {
		h := newRefundHarness(t)
		h.refundRequestedOrder("to_noorigin", nil)
		if _, err := h.rej.RejectRefund(rejectReq("to_noorigin", "r")); !errors.Is(err, model.ErrRefundOriginUnknown) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrRefundOriginUnknown)
		}
		if got := mustOrder(t, h.db, "to_noorigin"); got.State != model.StateRefundRequested || len(ledgerOf(h.db, "to_noorigin")) != 0 {
			t.Error("查不到原状态却动了订单")
		}
	})

	t.Run("台账里是脏值", func(t *testing.T) {
		h := newRefundHarness(t)
		h.refundRequestedOrder("to_dirty", nil)
		seedLedger(t, h.db, &model.OrderEvent{
			OrderNo: "to_dirty", FromState: model.StateCreated, ToState: model.StateRefundRequested,
			Operator: "user", Reason: "脏数据", RequestID: "x",
		})
		_, err := h.rej.RejectRefund(rejectReq("to_dirty", "r"))
		if !errors.Is(err, model.ErrRefundOriginUnknown) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrRefundOriginUnknown)
		}
		if !strings.Contains(err.Error(), "CREATED") {
			t.Errorf("错误文本要给出脏值: %v", err)
		}
		if got := mustOrder(t, h.db, "to_dirty"); got.State != model.StateRefundRequested {
			t.Errorf("脏数据被当作原状态写回主表: state=%s", model.StateName(got.State))
		}
	})

	t.Run("取最后一次进入申请态的行", func(t *testing.T) {
		h := newRefundHarness(t)
		h.refundRequestedOrder("to_last", func(o *model.Order) { o.Version = 9 })
		seedLedger(t, h.db, &model.OrderEvent{OrderNo: "to_last", FromState: model.StatePaid, ToState: model.StateRefundRequested, Operator: "user", Reason: "第一次申请", RequestID: "a"})
		seedLedger(t, h.db, &model.OrderEvent{OrderNo: "to_last", FromState: model.StateFulfilled, ToState: model.StateRefundRequested, Operator: "user", Reason: "第二次申请", RequestID: "b"})
		if _, err := h.rej.RejectRefund(rejectReq("to_last", "r")); err != nil {
			t.Fatalf("error = %v", err)
		}
		if got := mustOrder(t, h.db, "to_last"); got.State != model.StateFulfilled {
			t.Errorf("state = %s，期望按最后一行回到 FULFILLED", model.StateName(got.State))
		}
	})
}

// TestRejectRefundStateRules 款已退就不能驳回（等于抹掉退款事实）；
// REFUND_REJECTED 若被人工写进去则按幂等重放返回（代码现状）。
func TestRejectRefundStateRules(t *testing.T) {
	t.Run("款已退不能驳回", func(t *testing.T) {
		for _, st := range []int32{model.StateRefundApproved, model.StateRefunded} {
			h := newRefundHarness(t)
			h.seedForRefund("to_"+model.StateName(st), func(o *model.Order) {
				o.State = st
				o.Version = 8
				o.RefundedMinor = 3000
			})
			_, err := h.rej.RejectRefund(rejectReq("to_"+model.StateName(st), "r"))
			if !errors.Is(err, model.ErrInvalidStateTransition) {
				t.Errorf("state=%s error = %v，期望 %v", model.StateName(st), err, model.ErrInvalidStateTransition)
			}
			if got := mustOrder(t, h.db, "to_"+model.StateName(st)); got.RefundedMinor != 3000 || got.State != st {
				t.Errorf("驳回抹掉了退款事实: state=%s refunded=%d",
					model.StateName(got.State), got.RefundedMinor)
			}
		}
	})

	t.Run("REFUND_REJECTED 现状是幂等重放", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_rr", func(o *model.Order) { o.State = model.StateRefundRejected; o.Version = 7 })
		reply, err := h.rej.RejectRefund(rejectReq("to_rr", "r"))
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if !reply.GetDuplicated() {
			t.Error("已驳回态再驳回应幂等返回（该状态没有出边）")
		}
		if len(h.db.events) != 0 {
			t.Error("幂等重放却写台账")
		}
	})

	t.Run("没有待审批申请", func(t *testing.T) {
		h := newRefundHarness(t)
		h.seedForRefund("to_noreq", func(o *model.Order) { o.State = model.StateFulfilled })
		_, err := h.rej.RejectRefund(rejectReq("to_noreq", "r"))
		if !errors.Is(err, model.ErrRefundNotRequested) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrRefundNotRequested)
		}
	})

	t.Run("驳回成功后同 request_id 重放按幂等", func(t *testing.T) {
		h := newRefundHarness(t)
		h.refundRequestedOrder("to_again", func(o *model.Order) {
			o.State = model.StateFulfilled // 已回到原状态
			o.Version = 7
		})
		seedLedger(t, h.db, &model.OrderEvent{
			OrderNo: "to_again", FromState: model.StateRefundRequested, ToState: model.StateFulfilled,
			Operator: "ops_007", Reason: "refund rejected by ops_007: 已使用", RequestID: "reject-again",
		})
		reply, err := h.rej.RejectRefund(rejectReq("to_again", "reject-again"))
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if !reply.GetDuplicated() {
			t.Error("同 request_id 的驳回重放必须 duplicated=true")
		}
		if len(h.db.events) != 1 {
			t.Errorf("重放多写了台账: %d 行", len(h.db.events))
		}
	})
}

// TestRejectRefundValidationAndCas 必填项与并发保护（该接口没有 expected_version，
// 所以用刚读到的当前版本做 CAS，被抢先时报并发失败而不是覆盖）。
func TestRejectRefundValidationAndCas(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.RejectRefundReq)
		want   error
	}{
		{"缺 order_no", func(r *rpc.RejectRefundReq) { r.OrderNo = " " }, model.ErrOrderNoRequired},
		{"缺 operator", func(r *rpc.RejectRefundReq) { r.Operator = "" }, model.ErrOperatorRequired},
		{"缺 reason", func(r *rpc.RejectRefundReq) { r.Reason = "  " }, model.ErrReasonRequired},
		{"缺 request_id", func(r *rpc.RejectRefundReq) { r.RequestId = "" }, model.ErrRequestIdRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRefundHarness(t)
			h.refundRequestedOrder("to_rv", nil)
			seedLedger(t, h.db, &model.OrderEvent{OrderNo: "to_rv", FromState: model.StateFulfilled,
				ToState: model.StateRefundRequested, Operator: "user", Reason: "申请", RequestID: "q"})
			in := rejectReq("to_rv", "r")
			tc.mutate(in)
			if _, err := h.rej.RejectRefund(in); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v，期望 %v", err, tc.want)
			}
			if got := mustOrder(t, h.db, "to_rv"); got.State != model.StateRefundRequested {
				t.Errorf("入参非法却动了订单: state=%s", model.StateName(got.State))
			}
		})
	}

	t.Run("并发被抢先且已达原状态按幂等", func(t *testing.T) {
		h := newRefundHarness(t)
		h.refundRequestedOrder("to_rc", nil)
		seedLedger(t, h.db, &model.OrderEvent{OrderNo: "to_rc", FromState: model.StateFulfilled,
			ToState: model.StateRefundRequested, Operator: "user", Reason: "申请", RequestID: "q"})
		h.db.raceHook = func(o *model.Order, from, to int32) {
			if from == model.StateRefundRequested && to == model.StateFulfilled {
				o.State = model.StateFulfilled // 另一个审批人已经驳回
				o.Version = 7
			}
		}
		reply, err := h.rej.RejectRefund(rejectReq("to_rc", "r"))
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if !reply.GetDuplicated() || reply.GetOrder().GetState() != rpc.OrderState_ORDER_STATE_FULFILLED {
			t.Errorf("回复 = (%v,%v)", reply.GetDuplicated(), reply.GetOrder().GetState())
		}
	})

	t.Run("并发被抢先且状态不是原状态报并发失败", func(t *testing.T) {
		h := newRefundHarness(t)
		h.refundRequestedOrder("to_rc2", nil)
		seedLedger(t, h.db, &model.OrderEvent{OrderNo: "to_rc2", FromState: model.StateFulfilled,
			ToState: model.StateRefundRequested, Operator: "user", Reason: "申请", RequestID: "q"})
		h.db.raceHook = func(o *model.Order, from, to int32) {
			if from == model.StateRefundRequested {
				o.State = model.StateRefundApproved // 别人审批通过了
				o.Version = 7
			}
		}
		if _, err := h.rej.RejectRefund(rejectReq("to_rc2", "r")); !errors.Is(err, model.ErrConcurrentUpdate) {
			t.Fatalf("error = %v，期望 %v", err, model.ErrConcurrentUpdate)
		}
		if got := mustOrder(t, h.db, "to_rc2"); got.State != model.StateRefundApproved || len(h.db.events) != 1 {
			t.Errorf("败者覆盖了胜者: state=%s events=%d", model.StateName(got.State), len(h.db.events))
		}
	})
}
