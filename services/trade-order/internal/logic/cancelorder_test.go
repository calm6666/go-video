package logic

// CancelOrder 只允许取消「钱还没动」的单。本文件锁两条最容易被写坏的口径：
//  1. PAYING 单取消前必须向 payment 核对结论 —— 钱可能已在 payment 侧扣掉而本地还是 PAYING，
//     此时直接取消等于把钱吞在别人的台账里；
//  2. 越权与不存在走同一个 not-found 出口，不泄露订单号是否存在。

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	paymentrpc "go-video/services/payment/rpc"
	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type cancelHarness struct {
	t    *testing.T
	l    *CancelOrderLogic
	svc  *svc.ServiceContext
	db   *fakeDB
	pay  *fakePayment
	mem  *fakeMembership
	coin *fakeCoin
}

func newCancelHarness(t *testing.T) *cancelHarness {
	t.Helper()
	svcCtx, db := newTestSvc(t)
	mem := newFakeMembership()
	pay := newFakePayment()
	coin := &fakeCoin{}
	wireDownstream(svcCtx, mem, pay, coin)
	return &cancelHarness{t: t, l: NewCancelOrderLogic(t.Context(), svcCtx), svc: svcCtx, db: db, pay: pay, mem: mem, coin: coin}
}

func cancelReq(orderNo string, mid int64, operator, requestID, reason string) *rpc.CancelOrderReq {
	return &rpc.CancelOrderReq{OrderNo: orderNo, Mid: mid, Operator: operator, RequestId: requestID, Reason: reason}
}

func (h *cancelHarness) seed(orderNo string, state int32, tune func(*model.Order)) *model.Order {
	o := &model.Order{
		OrderNo: orderNo, RequestID: "req-" + orderNo, Mid: 1001, BizType: model.BizMembership,
		PlanID: 77, Currency: "CNY", Quantity: 1, UnitPriceMinor: 3000, AmountMinor: 3000,
		PayMethod: model.PaySandbox, Platform: model.PlatformAndroid,
		State: state, FulfillState: model.FulfillPending, Version: 1,
	}
	if state == model.StatePaying {
		o.Version = 2
	}
	if tune != nil {
		tune(o)
	}
	return seedOrder(h.t, h.db, o)
}

func TestCancelCreatedOrderByOwner(t *testing.T) {
	h := newCancelHarness(t)
	h.seed("to_c1", model.StateCreated, nil)

	reply, err := h.l.CancelOrder(cancelReq("to_c1", 1001, "", "cancel-1", "用户主动放弃"))
	if err != nil {
		t.Fatalf("CancelOrder() error = %v", err)
	}
	if reply.GetDuplicated() {
		t.Error("首次取消不该 duplicated")
	}
	got := mustOrder(t, h.db, "to_c1")
	if got.State != model.StateCancelled {
		t.Fatalf("state = %s", model.StateName(got.State))
	}
	if got.Version != 2 || got.ClosedAt == 0 {
		t.Errorf("version/closed_at = (%d,%d)", got.Version, got.ClosedAt)
	}
	if got.RefundedMinor != 0 || got.PaymentNo != "" {
		t.Error("取消不该产生退款或支付引用")
	}
	rows := ledgerOf(h.db, "to_c1")
	if len(rows) != 1 {
		t.Fatalf("台账行数 = %d，期望 1", len(rows))
	}
	if rows[0].Reason != "cancelled by user: 用户主动放弃" {
		t.Errorf("台账理由 = %q", rows[0].Reason)
	}
	if rows[0].Operator != "user" || rows[0].RequestID != "cancel-1" {
		t.Errorf("台账 operator/request_id = (%q,%q)", rows[0].Operator, rows[0].RequestID)
	}
	// CREATED 单没有资金路径，不该被拿去问 payment
	if len(h.pay.getCalls) != 0 {
		t.Errorf("CREATED 单取消却查了 %d 次 payment", len(h.pay.getCalls))
	}
	assertOnlyLegalTransitions(t, h.db)
}

func TestCancelPayingOrderChecksPaymentConclusion(t *testing.T) {
	t.Run("payment 里没有支付单：可以取消", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_c2", model.StatePaying, nil)
		if _, err := h.l.CancelOrder(cancelReq("to_c2", 1001, "", "cancel-2", "不要了")); err != nil {
			t.Fatalf("CancelOrder() error = %v", err)
		}
		if len(h.pay.getCalls) != 1 || h.pay.getCalls[0].GetBizOrderNo() != "to_c2" {
			t.Fatalf("PAYING 单必须先核对 payment，实得 %d 次调用", len(h.pay.getCalls))
		}
		if got := mustOrder(t, h.db, "to_c2"); got.State != model.StateCancelled {
			t.Errorf("state = %s", model.StateName(got.State))
		}
	})

	t.Run("未支付支付单先关单再取消", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_c3", model.StatePaying, nil)
		h.pay.getFound = true
		h.pay.getState = paymentrpc.PaymentState_PAYMENT_STATE_PENDING
		if _, err := h.l.CancelOrder(cancelReq("to_c3", 1001, "", "cancel-3", "不要了")); err != nil {
			t.Fatalf("CancelOrder() error = %v", err)
		}
		if len(h.pay.closeCalls) != 1 {
			t.Fatalf("未支付支付单必须关掉，实得 %d 次", len(h.pay.closeCalls))
		}
		cc := h.pay.closeCalls[0]
		if cc.GetRequestId() != "close_to_c3" || cc.GetPaymentNo() != "pay_to_c3" || cc.GetOperator() != "trade-order" {
			t.Errorf("ClosePayment 参数 = (%q,%q,%q)", cc.GetRequestId(), cc.GetPaymentNo(), cc.GetOperator())
		}
		if got := mustOrder(t, h.db, "to_c3"); got.State != model.StateCancelled {
			t.Errorf("state = %s", model.StateName(got.State))
		}
	})

	for _, st := range []paymentrpc.PaymentState{
		paymentrpc.PaymentState_PAYMENT_STATE_FAILED, paymentrpc.PaymentState_PAYMENT_STATE_CLOSED,
	} {
		t.Run("支付单"+st.String()+"没有资金占用", func(t *testing.T) {
			h := newCancelHarness(t)
			h.seed("to_c4", model.StatePaying, nil)
			h.pay.getFound = true
			h.pay.getState = st
			if _, err := h.l.CancelOrder(cancelReq("to_c4", 1001, "", "cancel-4", "不要了")); err != nil {
				t.Fatalf("CancelOrder() error = %v", err)
			}
			if len(h.pay.closeCalls) != 0 {
				t.Error("已经终结的支付单不该再关")
			}
			if got := mustOrder(t, h.db, "to_c4"); got.State != model.StateCancelled {
				t.Errorf("state = %s", model.StateName(got.State))
			}
		})
	}

	for _, st := range []paymentrpc.PaymentState{
		paymentrpc.PaymentState_PAYMENT_STATE_PAID, paymentrpc.PaymentState_PAYMENT_STATE_REFUNDED,
		paymentrpc.PaymentState_PAYMENT_STATE_PARTIALLY_REFUNDED,
	} {
		t.Run("钱已动过("+st.String()+")必须走退款", func(t *testing.T) {
			h := newCancelHarness(t)
			h.seed("to_c5", model.StatePaying, nil)
			h.pay.getFound = true
			h.pay.getState = st
			_, err := h.l.CancelOrder(cancelReq("to_c5", 1001, "", "cancel-5", "不要了"))
			if !errors.Is(err, model.ErrCancelRejectedPaid) {
				t.Fatalf("CancelOrder() error = %v，期望 %v", err, model.ErrCancelRejectedPaid)
			}
			if !strings.Contains(err.Error(), "refund instead") {
				t.Errorf("错误文本要指路退款: %v", err)
			}
			got := mustOrder(t, h.db, "to_c5")
			if got.State != model.StatePaying || got.Version != 2 || got.ClosedAt != 0 {
				t.Errorf("钱已动却取消了: state=%s version=%d closed_at=%d",
					model.StateName(got.State), got.Version, got.ClosedAt)
			}
			if len(h.db.events) != 0 || len(h.pay.closeCalls) != 0 {
				t.Error("拒绝取消却写了台账/关了支付单")
			}
		})
	}

	t.Run("查不到结论就不能取消", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_c6", model.StatePaying, nil)
		h.pay.getErr = status.Error(codes.Unavailable, "payment is down")
		_, err := h.l.CancelOrder(cancelReq("to_c6", 1001, "", "cancel-6", "不要了"))
		if err == nil || !strings.Contains(err.Error(), "payment.GetPayment") {
			t.Fatalf("CancelOrder() error = %v，期望带上游调用名", err)
		}
		if status.Code(err) != codes.Unavailable {
			t.Errorf("gRPC code 被吞: %v", err)
		}
		got := mustOrder(t, h.db, "to_c6")
		if got.State != model.StatePaying || len(h.db.events) != 0 {
			t.Errorf("资金状态未知却取消了: state=%s", model.StateName(got.State))
		}
	})

	t.Run("关不掉支付单就报错让调用方重试", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_c7", model.StatePaying, nil)
		h.pay.getFound = true
		h.pay.getState = paymentrpc.PaymentState_PAYMENT_STATE_PENDING
		h.pay.closeErr = status.Error(codes.Internal, "close failed")
		if _, err := h.l.CancelOrder(cancelReq("to_c7", 1001, "", "cancel-7", "不要了")); err == nil ||
			!strings.Contains(err.Error(), "payment.ClosePayment") {
			t.Fatalf("CancelOrder() error = %v", err)
		}
		if got := mustOrder(t, h.db, "to_c7"); got.State != model.StatePaying || len(h.db.events) != 0 {
			t.Errorf("支付单没关掉却取消了: state=%s events=%d",
				model.StateName(got.State), len(h.db.events))
		}
	})

	t.Run("未配置 payment 时取消是安全的", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_c8", model.StatePaying, nil)
		h.svc.Payment = nil
		if _, err := h.l.CancelOrder(cancelReq("to_c8", 1001, "", "cancel-8", "不要了")); err != nil {
			t.Fatalf("CancelOrder() error = %v", err)
		}
		if got := mustOrder(t, h.db, "to_c8"); got.State != model.StateCancelled {
			t.Errorf("state = %s", model.StateName(got.State))
		}
	})
}

// TestCancelRejectedWhenPaymentAlreadyBound 本地已绑 payment_no 就是「已付款」事实，
// 连核对都不必做，直接拒绝，也不允许调 payment。
func TestCancelRejectedWhenPaymentAlreadyBound(t *testing.T) {
	h := newCancelHarness(t)
	h.seed("to_bound", model.StatePaying, func(o *model.Order) { o.PaymentNo = "pay_to_bound" })
	_, err := h.l.CancelOrder(cancelReq("to_bound", 1001, "", "cancel-b", "不要了"))
	if !errors.Is(err, model.ErrCancelRejectedPaid) {
		t.Fatalf("CancelOrder() error = %v，期望 %v", err, model.ErrCancelRejectedPaid)
	}
	if len(h.pay.getCalls) != 0 {
		t.Error("本地已绑款却还去核对，白打一次 RPC")
	}
	if got := mustOrder(t, h.db, "to_bound"); got.State != model.StatePaying || len(h.db.events) != 0 {
		t.Errorf("拒绝取消却动了订单: state=%s events=%d", model.StateName(got.State), len(h.db.events))
	}
}

// TestCancelRejectedForPaidAndLaterStates 已支付及之后必须走退款，不能靠取消把钱吞掉。
func TestCancelRejectedForPaidAndLaterStates(t *testing.T) {
	for _, st := range []int32{model.StatePaid, model.StateFulfilling, model.StateFulfilled,
		model.StateFailed, model.StateRefundRequested, model.StateRefundApproved, model.StateRefunded} {
		t.Run(model.StateName(st), func(t *testing.T) {
			h := newCancelHarness(t)
			h.seed("to_later", st, func(o *model.Order) {
				o.Version = 4
				o.PaymentNo = "pay_x"
			})
			_, err := h.l.CancelOrder(cancelReq("to_later", 1001, "ops_1", "cancel-x", "想取消"))
			if !errors.Is(err, model.ErrCancelRejectedPaid) {
				t.Fatalf("CancelOrder() error = %v，期望 %v", err, model.ErrCancelRejectedPaid)
			}
			if !strings.Contains(err.Error(), "RequestRefund") {
				t.Errorf("错误文本要指路退款接口: %v", err)
			}
			got := mustOrder(t, h.db, "to_later")
			if got.State != st || got.Version != 4 || got.ClosedAt != 0 || len(h.db.events) != 0 {
				t.Errorf("非法前态却动了订单: state=%s version=%d events=%d",
					model.StateName(got.State), got.Version, len(h.db.events))
			}
		})
	}
}

// TestCancelAuthorization 越权与不存在同义（not-found 语义），运营代取消以 operator 记台账。
func TestCancelAuthorization(t *testing.T) {
	t.Run("别人的订单等于不存在", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_other", model.StateCreated, nil)
		_, err := h.l.CancelOrder(cancelReq("to_other", 2002, "", "cancel-o", "误操作"))
		if !errors.Is(err, model.ErrOrderNotFound) {
			t.Fatalf("CancelOrder() error = %v", err)
		}
		_, err2 := h.l.CancelOrder(cancelReq("to_missing", 2002, "", "cancel-o2", "误操作"))
		if !errors.Is(err2, model.ErrOrderNotFound) {
			t.Fatalf("不存在的 error = %v", err2)
		}
		// 两条分支的可见结论必须完全一致，否则可以被用来枚举订单号
		if err.Error() != err2.Error() {
			t.Errorf("越权与不存在文本可区分: %q vs %q", err, err2)
		}
		if len(h.db.events) != 0 {
			t.Error("越权尝试却动了订单")
		}
	})

	t.Run("运营代取消以 operator 记台账", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_ops", model.StateCreated, nil)
		if _, err := h.l.CancelOrder(cancelReq("to_ops", 0, "ops_007", "cancel-ops", "用户来电要求取消")); err != nil {
			t.Fatalf("CancelOrder() error = %v", err)
		}
		rows := ledgerOf(h.db, "to_ops")
		if rows[0].Operator != "ops_007" || !strings.HasPrefix(rows[0].Reason, "cancelled by ops_007: ") {
			t.Errorf("台账 = (%q,%q)", rows[0].Operator, rows[0].Reason)
		}
	})

	t.Run("mid 与 operator 同时给出时以 operator 记账", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_both", model.StateCreated, nil)
		if _, err := h.l.CancelOrder(cancelReq("to_both", 1001, "ops_009", "cancel-both", "代客取消")); err != nil {
			t.Fatalf("CancelOrder() error = %v", err)
		}
		if got := ledgerOf(h.db, "to_both")[0]; got.Operator != "ops_009" {
			t.Errorf("operator = %q，代操作必须以运营身份记", got.Operator)
		}
	})
}

// TestCancelIsIdempotent 同一 request_id 重放与「已经取消」都是幂等成功，不再动状态。
func TestCancelIsIdempotent(t *testing.T) {
	t.Run("台账命中", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_dupcancel", model.StatePaying, nil)
		seedLedger(t, h.db, &model.OrderEvent{
			OrderNo: "to_dupcancel", FromState: model.StatePaying, ToState: model.StateCancelled,
			Operator: "user", Reason: "cancelled by user: 旧理由", RequestID: "cancel-dup",
		})
		reply, err := h.l.CancelOrder(cancelReq("to_dupcancel", 1001, "", "cancel-dup", "重复提交"))
		if err != nil {
			t.Fatalf("CancelOrder() error = %v", err)
		}
		if !reply.GetDuplicated() {
			t.Error("期望 duplicated=true")
		}
		got := mustOrder(t, h.db, "to_dupcancel")
		if got.State != model.StatePaying || got.Version != 2 || len(h.db.events) != 1 {
			t.Errorf("幂等重放却推进了: state=%s version=%d events=%d",
				model.StateName(got.State), got.Version, len(h.db.events))
		}
		// 台账命中发生在向 payment 核对之前，所以重放不会重复关支付单
		if len(h.pay.closeCalls) != 0 {
			t.Error("重放又去关了支付单")
		}
	})

	t.Run("订单已是 CANCELLED", func(t *testing.T) {
		h := newCancelHarness(t)
		h.seed("to_cancelled", model.StateCancelled, func(o *model.Order) { o.Version = 2 })
		reply, err := h.l.CancelOrder(cancelReq("to_cancelled", 1001, "", "cancel-again", "再取消一次"))
		if err != nil {
			t.Fatalf("CancelOrder() error = %v", err)
		}
		if !reply.GetDuplicated() {
			t.Error("已取消订单的再次取消必须按幂等返回，而不是报「非法迁移」")
		}
		if len(h.db.events) != 0 {
			t.Error("终态重放不该写台账（CANCELLED 没有出边）")
		}
		assertOnlyLegalTransitions(t, h.db)
	})
}

// TestCancelReasonNeverLeaks 理由直接进台账：折行、凭据都必须被剥掉。
func TestCancelReasonNeverLeaks(t *testing.T) {
	h := newCancelHarness(t)
	h.seed("to_leak", model.StateCreated, nil)
	_, err := h.l.CancelOrder(cancelReq("to_leak", 1001, "", "cancel-leak",
		"用户投诉\n\t账号 password=S3cr3t@tcp(10.0.0.1:3306)/db 被锁"))
	if err != nil {
		t.Fatalf("CancelOrder() error = %v", err)
	}
	reason := ledgerOf(h.db, "to_leak")[0].Reason
	if reason != "cancelled by user: downstream error message redacted" {
		t.Errorf("台账理由 = %q", reason)
	}
	for _, leak := range []string{"S3cr3t", "@tcp(", "password"} {
		if strings.Contains(reason, leak) {
			t.Errorf("台账泄露 %q: %q", leak, reason)
		}
	}
	if strings.Contains(reason, "\n") {
		t.Error("台账理由必须是单行（列里存多行会让运营页错位）")
	}
}

// TestCancelConcurrentMissReloads CAS 未命中时按并发重放返回当前行，不覆盖胜者。
func TestCancelConcurrentMissReloads(t *testing.T) {
	h := newCancelHarness(t)
	h.seed("to_crace", model.StateCreated, nil)
	h.db.raceHook = func(o *model.Order, from, to int32) {
		if from == model.StateCreated && to == model.StateCancelled {
			o.State = model.StatePaying
			o.Version = 7
		}
	}
	reply, err := h.l.CancelOrder(cancelReq("to_crace", 1001, "", "cancel-race", "并发取消"))
	if err != nil {
		t.Fatalf("CancelOrder() error = %v", err)
	}
	if !reply.GetDuplicated() {
		t.Error("CAS 未命中必须 duplicated=true")
	}
	if reply.GetOrder().GetState() != rpc.OrderState_ORDER_STATE_PAYING ||
		reply.GetOrder().GetVersion() != 7 {
		t.Errorf("回复 = (%v,%d)，期望回读胜者的 PAYING/7",
			reply.GetOrder().GetState(), reply.GetOrder().GetVersion())
	}
	if got := mustOrder(t, h.db, "to_crace"); got.State != model.StatePaying || len(h.db.events) != 0 {
		t.Errorf("败者覆盖了胜者: state=%s events=%d", model.StateName(got.State), len(h.db.events))
	}
}

// TestCancelValidatesInput 必填项与主体校验全部在读取订单之前完成。
func TestCancelValidatesInput(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.CancelOrderReq
		want error
	}{
		{"缺 order_no", cancelReq(" ", 1001, "", "r", "理由"), model.ErrOrderNoRequired},
		{"缺 request_id", cancelReq("to_x", 1001, "", "  ", "理由"), model.ErrRequestIdRequired},
		{"缺 reason", cancelReq("to_x", 1001, "", "r", "   "), model.ErrReasonRequired},
		{"既无 mid 也无 operator", cancelReq("to_x", 0, "  ", "r", "理由"), model.ErrSubjectRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCancelHarness(t)
			h.seed("to_x", model.StateCreated, nil)
			if _, err := h.l.CancelOrder(tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("CancelOrder() error = %v，期望 %v", err, tc.want)
			}
			got := mustOrder(t, h.db, "to_x")
			if got.State != model.StateCreated || len(h.db.events) != 0 {
				t.Error("入参非法却动了订单")
			}
		})
	}
}
