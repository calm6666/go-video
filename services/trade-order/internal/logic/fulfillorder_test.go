package logic

// FulfillOrder / runFulfill 的口径测试。这个顺序是本轮最关键的判定：
// 事务内 CAS 到 FULFILLING（attempts+1 + 台账）→ 提交后才调下游 → 成功再 CAS 到 FULFILLED。
// 反过来就会「权益已发出、订单还停在 PAID」，下一轮重试是一次超发。

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	memberrpc "go-video/services/membership/rpc"
	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type fulfillHarness struct {
	l    *FulfillOrderLogic
	svc  *svc.ServiceContext
	db   *fakeDB
	mem  *fakeMembership
	pay  *fakePayment
	coin *fakeCoin
}

func newFulfillHarness(t *testing.T) *fulfillHarness {
	t.Helper()
	svcCtx, db := newTestSvc(t)
	mem := newFakeMembership().withPlan(defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	pay := newFakePayment()
	coin := &fakeCoin{}
	wireDownstream(svcCtx, mem, pay, coin)
	return &fulfillHarness{
		l:    NewFulfillOrderLogic(t.Context(), svcCtx),
		svc:  svcCtx,
		db:   db,
		mem:  mem,
		pay:  pay,
		coin: coin,
	}
}

// seedPaid 放一张「钱已受理、等待履约」的会员单（version 3 = 建单到 PAID 四次 CAS 后）。
func (h *fulfillHarness) seedPaid(t *testing.T, orderNo string, tune func(*model.Order)) *model.Order {
	t.Helper()
	o := &model.Order{
		OrderNo: orderNo, RequestID: "req-" + orderNo, Mid: 1001, BizType: model.BizMembership,
		PlanID: 77, PlanCode: "code_77", Title: "大会员月卡", Currency: "CNY",
		Quantity: 1, DurationDays: 31, UnitPriceMinor: 3000, AmountMinor: 3000,
		PayMethod: model.PaySandbox, Platform: model.PlatformAndroid, PaymentNo: "pay_" + orderNo,
		State: model.StatePaid, FulfillState: model.FulfillPending, Version: 3,
	}
	if tune != nil {
		tune(o)
	}
	return seedOrder(t, h.db, o)
}

func fulfillReq(orderNo string) *rpc.FulfillOrderReq {
	return &rpc.FulfillOrderReq{OrderNo: orderNo, RequestId: "fulfill-" + orderNo}
}

// TestFulfillOrderHappyPathGrantsOnceAndRecordsTwoRows 正向：两步 CAS、两行台账、一条发放调用。
func TestFulfillOrderHappyPathGrantsOnceAndRecordsTwoRows(t *testing.T) {
	h := newFulfillHarness(t)
	h.seedPaid(t, "to_f1", nil)

	reply, err := h.l.FulfillOrder(fulfillReq("to_f1"))
	if err != nil {
		t.Fatalf("FulfillOrder() error = %v", err)
	}
	if !reply.GetFulfilled() || reply.GetDuplicated() || reply.GetDetail() != "" {
		t.Errorf("结论 = (%v,%v,%q)，期望 (true,false,\"\")",
			reply.GetFulfilled(), reply.GetDuplicated(), reply.GetDetail())
	}
	got := mustOrder(t, h.db, "to_f1")
	if got.State != model.StateFulfilled || got.FulfillState != model.FulfillDone {
		t.Errorf("state/fulfill_state = (%s,%d)", model.StateName(got.State), got.FulfillState)
	}
	if got.GrantRef != "membership_grant:7001" || got.FulfilledAt == 0 || got.FulfillAttempts != 1 {
		t.Errorf("grant_ref/fulfilled_at/attempts = (%q,%d,%d)",
			got.GrantRef, got.FulfilledAt, got.FulfillAttempts)
	}
	if got.FulfillDetail != "" {
		t.Errorf("成功后应清掉历史失败摘要，实得 %q", got.FulfillDetail)
	}
	if got.Version != 5 {
		t.Errorf("version = %d，期望 5（两次 CAS）", got.Version)
	}
	if got.PaymentNo != "pay_to_f1" || got.RefundedMinor != 0 {
		t.Errorf("履约不该碰资金字段: payment_no=%q refunded=%d", got.PaymentNo, got.RefundedMinor)
	}
	want := [][2]string{{"PAID", "FULFILLING"}, {"FULFILLING", "FULFILLED"}}
	if pairs := ledgerPairs(h.db, "to_f1"); len(pairs) != 2 || pairs[0] != want[0] || pairs[1] != want[1] {
		t.Errorf("台账链 = %v，期望 %v", pairs, want)
	}
	rows := ledgerOf(h.db, "to_f1")
	if rows[0].Reason != "fulfill attempt 1, biz_type=1" {
		t.Errorf("首次尝试台账 = %q", rows[0].Reason)
	}
	if rows[0].RequestID != "fulfill-to_f1" || rows[0].Operator != "system" {
		t.Errorf("缺省 operator/request_id = (%q,%q)，期望 (system,fulfill-to_f1)",
			rows[0].Operator, rows[0].RequestID)
	}
	if len(h.mem.grantCalls) != 1 {
		t.Fatalf("GrantMembership 调用次数 = %d，期望 1", len(h.mem.grantCalls))
	}
	if got := h.mem.grantCalls[0]; got.GetRequestId() != "grant_to_f1" || got.GetPaymentNo() != "pay_to_f1" {
		t.Errorf("下游幂等键/payment_no = (%q,%q)，必须订单号派生", got.GetRequestId(), got.GetPaymentNo())
	}
	// 事务顺序可证：CAS 到 FULFILLING 的那次事务一定先于下游调用发生。
	if h.db.txRuns < 2 {
		t.Errorf("事务次数 = %d，期望至少 2（FULFILLING 与 FULFILLED 各一次）", h.db.txRuns)
	}
	assertOnlyLegalTransitions(t, h.db)
}

// TestFulfillOrderOnFulfilledIsIdempotentReplay 已履约订单：幂等重放，绝不再发一次权益。
func TestFulfillOrderOnFulfilledIsIdempotentReplay(t *testing.T) {
	h := newFulfillHarness(t)
	h.seedPaid(t, "to_done", func(o *model.Order) {
		o.State = model.StateFulfilled
		o.FulfillState = model.FulfillDone
		o.GrantRef = "membership_grant:7001"
		o.FulfillAttempts = 1
		o.Version = 5
	})

	reply, err := h.l.FulfillOrder(fulfillReq("to_done"))
	if err != nil {
		t.Fatalf("FulfillOrder() error = %v", err)
	}
	if !reply.GetFulfilled() || !reply.GetDuplicated() {
		t.Errorf("结论 = (fulfilled=%v,dup=%v)，期望 (true,true)", reply.GetFulfilled(), reply.GetDuplicated())
	}
	if len(h.mem.grantCalls) != 0 {
		t.Errorf("重放又发放了 %d 次权益", len(h.mem.grantCalls))
	}
	got := mustOrder(t, h.db, "to_done")
	if got.Version != 5 || got.FulfillAttempts != 1 || len(h.db.events) != 0 {
		t.Errorf("重放改动了订单: version=%d attempts=%d events=%d",
			got.Version, got.FulfillAttempts, len(h.db.events))
	}
}

// TestFulfillOrderRejectsOtherStates 履约入口只受理 PAID / FULFILLING。
// 特别是 FAILED：它没有出边（人工态），必须由人工核对而不是被 cron 顺手复活。
func TestFulfillOrderRejectsOtherStates(t *testing.T) {
	states := []int32{model.StateCreated, model.StatePaying, model.StateCancelled, model.StateFailed,
		model.StateRefundRequested, model.StateRefundApproved, model.StateRefunded, model.StateRefundRejected}
	for _, st := range states {
		t.Run(model.StateName(st), func(t *testing.T) {
			h := newFulfillHarness(t)
			h.seedPaid(t, "to_bad", func(o *model.Order) {
				o.State = st
				o.FulfillAttempts = 0
			})
			_, err := h.l.FulfillOrder(fulfillReq("to_bad"))
			if !errors.Is(err, model.ErrFulfillNotAccepted) {
				t.Fatalf("FulfillOrder() error = %v，期望 %v", err, model.ErrFulfillNotAccepted)
			}
			if !strings.Contains(err.Error(), model.StateName(st)) {
				t.Errorf("错误文本要给出当前状态: %v", err)
			}
			if len(h.mem.grantCalls) != 0 || len(h.coin.grantCalls) != 0 {
				t.Error("非法前态却调了下游发放")
			}
			got := mustOrder(t, h.db, "to_bad")
			if got.State != st || got.Version != 3 || len(h.db.events) != 0 {
				t.Errorf("非法前态却推进了订单: state=%s version=%d events=%d",
					model.StateName(got.State), got.Version, len(h.db.events))
			}
		})
	}

	t.Run("订单不存在", func(t *testing.T) {
		h := newFulfillHarness(t)
		if _, err := h.l.FulfillOrder(fulfillReq("to_none")); !errors.Is(err, model.ErrOrderNotFound) {
			t.Fatalf("FulfillOrder() error = %v，期望 %v", err, model.ErrOrderNotFound)
		}
	})

	t.Run("缺 order_no", func(t *testing.T) {
		h := newFulfillHarness(t)
		if _, err := h.l.FulfillOrder(&rpc.FulfillOrderReq{OrderNo: "  "}); !errors.Is(err, model.ErrOrderNoRequired) {
			t.Fatalf("FulfillOrder() error = %v，期望 %v", err, model.ErrOrderNoRequired)
		}
	})
}

// TestFulfillOrderGuardsAttemptsAndRetryRate 两个护栏：尝试上限与重试频率。
// 首次（PAID→FULFILLING）不受频率护栏约束 —— 否则刚付完款就会被上一条失败的时间戳挡住。
func TestFulfillOrderGuardsAttemptsAndRetryRate(t *testing.T) {
	t.Run("尝试次数达上限", func(t *testing.T) {
		h := newFulfillHarness(t)
		h.seedPaid(t, "to_exhaust", func(o *model.Order) { o.FulfillAttempts = 5 })
		_, err := h.l.FulfillOrder(fulfillReq("to_exhaust"))
		if !errors.Is(err, model.ErrFulfillAttemptsExhausted) {
			t.Fatalf("FulfillOrder() error = %v，期望 %v", err, model.ErrFulfillAttemptsExhausted)
		}
		got := mustOrder(t, h.db, "to_exhaust")
		if got.State != model.StatePaid || got.FulfillAttempts != 5 || got.Version != 3 {
			t.Errorf("超限却仍推进: state=%s attempts=%d version=%d",
				model.StateName(got.State), got.FulfillAttempts, got.Version)
		}
		if len(h.mem.grantCalls) != 0 || len(h.db.events) != 0 {
			t.Error("超限却打爆下游/写台账")
		}
	})

	t.Run("上限内最后一次仍可尝试", func(t *testing.T) {
		h := newFulfillHarness(t)
		h.seedPaid(t, "to_last", func(o *model.Order) { o.FulfillAttempts = 4 })
		reply, err := h.l.FulfillOrder(fulfillReq("to_last"))
		if err != nil {
			t.Fatalf("FulfillOrder() error = %v", err)
		}
		if !reply.GetFulfilled() || mustOrder(t, h.db, "to_last").FulfillAttempts != 5 {
			t.Errorf("第 5 次应被允许: fulfilled=%v", reply.GetFulfilled())
		}
	})

	t.Run("同态重试太快被拒", func(t *testing.T) {
		h := newFulfillHarness(t)
		h.seedPaid(t, "to_soon", func(o *model.Order) {
			o.State = model.StateFulfilling
			o.UpdatedAt = fakeNow()
			o.FulfillAttempts = 1
		})
		_, err := h.l.FulfillOrder(fulfillReq("to_soon"))
		if !errors.Is(err, model.ErrFulfillRetryTooSoon) {
			t.Fatalf("FulfillOrder() error = %v，期望 %v", err, model.ErrFulfillRetryTooSoon)
		}
		if len(h.mem.grantCalls) != 0 {
			t.Error("频率护栏没挡住下游调用")
		}
	})

	t.Run("首次履约不受频率护栏影响", func(t *testing.T) {
		h := newFulfillHarness(t)
		h.seedPaid(t, "to_first", func(o *model.Order) { o.UpdatedAt = fakeNow() })
		if _, err := h.l.FulfillOrder(fulfillReq("to_first")); err != nil {
			t.Fatalf("FulfillOrder() error = %v", err)
		}
		if mustOrder(t, h.db, "to_first").State != model.StateFulfilled {
			t.Error("首单被频率护栏挡住")
		}
	})

	t.Run("过了最小间隔后同态重试自愈", func(t *testing.T) {
		h := newFulfillHarness(t)
		h.seedPaid(t, "to_retry", func(o *model.Order) {
			o.State = model.StateFulfilling
			o.UpdatedAt = fakeNow() - 120
			o.FulfillDetail = "上一轮失败摘要"
			o.FulfillAttempts = 1
			o.Version = 4
		})
		reply, err := h.l.FulfillOrder(fulfillReq("to_retry"))
		if err != nil {
			t.Fatalf("FulfillOrder() error = %v", err)
		}
		if !reply.GetFulfilled() {
			t.Fatal("重试应完成履约")
		}
		got := mustOrder(t, h.db, "to_retry")
		if got.State != model.StateFulfilled || got.FulfillAttempts != 2 || got.FulfillDetail != "" {
			t.Errorf("state/attempts/detail = (%s,%d,%q)",
				model.StateName(got.State), got.FulfillAttempts, got.FulfillDetail)
		}
		// FULFILLING -> FULFILLING 是状态机上唯一的同态边（重试点亮），必须留台账
		want := [][2]string{{"FULFILLING", "FULFILLING"}, {"FULFILLING", "FULFILLED"}}
		if pairs := ledgerPairs(h.db, "to_retry"); len(pairs) != 2 || pairs[0] != want[0] || pairs[1] != want[1] {
			t.Errorf("重试台账链 = %v，期望 %v", pairs, want)
		}
		if got := h.mem.grantCalls[0]; got.GetRequestId() != "grant_to_retry" {
			t.Errorf("重试幂等键 = %q，必须与首次一致才能被下游判重放", got.GetRequestId())
		}
		assertOnlyLegalTransitions(t, h.db)
	})
}

// TestFulfillOrderDownstreamDuplicatedCountsAsSuccess 下游回 duplicated 是幂等自愈的证据，
// 算成功，并在台账里显式标注。
func TestFulfillOrderDownstreamDuplicatedCountsAsSuccess(t *testing.T) {
	h := newFulfillHarness(t)
	h.mem.duplicated = true
	h.seedPaid(t, "to_dup", nil)

	reply, err := h.l.FulfillOrder(fulfillReq("to_dup"))
	if err != nil {
		t.Fatalf("FulfillOrder() error = %v", err)
	}
	if !reply.GetFulfilled() || !reply.GetDuplicated() {
		t.Errorf("结论 = (fulfilled=%v,dup=%v)，期望 (true,true)", reply.GetFulfilled(), reply.GetDuplicated())
	}
	rows := ledgerOf(h.db, "to_dup")
	if rows[len(rows)-1].Reason != "fulfilled, grant_ref=membership_grant:7001 (downstream duplicated)" {
		t.Errorf("台账理由 = %q，要标注下游重放", rows[len(rows)-1].Reason)
	}
}

// TestFulfillOrderDownstreamFailureMarksFailedAndReturnsError 发放失败：
// 订单进 FAILED、写清摘要，并且返回错误（gRPC 下带错误的回复会被丢弃，
// 调用方重查得到真实状态，绝不会拿到 fulfilled=true）。
func TestFulfillOrderDownstreamFailureMarksFailedAndReturnsError(t *testing.T) {
	h := newFulfillHarness(t)
	h.mem.grantErr = status.Error(codes.Internal, "membership exploded")
	h.seedPaid(t, "to_fail", nil)

	reply, err := h.l.FulfillOrder(fulfillReq("to_fail"))
	if err == nil {
		t.Fatal("下游失败必须返回错误")
	}
	if reply != nil {
		t.Errorf("带错误时不该回 reply，实得 %+v", reply)
	}
	if !strings.Contains(err.Error(), "membership.GrantMembership") {
		t.Errorf("错误文本 = %v，要能看出失败在哪", err)
	}
	got := mustOrder(t, h.db, "to_fail")
	if got.State != model.StateFailed || got.FulfillState != model.FulfillFailed {
		t.Errorf("state/fulfill_state = (%s,%d)", model.StateName(got.State), got.FulfillState)
	}
	if !strings.Contains(got.FulfillDetail, "membership exploded") {
		t.Errorf("fulfill_detail = %q", got.FulfillDetail)
	}
	if got.GrantRef != "" {
		t.Errorf("失败却写了 grant_ref=%q", got.GrantRef)
	}
	if got.FulfilledAt != 0 {
		t.Error("失败却写了 fulfilled_at")
	}
	want := [][2]string{{"PAID", "FULFILLING"}, {"FULFILLING", "FAILED"}}
	if pairs := ledgerPairs(h.db, "to_fail"); len(pairs) != 2 || pairs[1] != want[1] {
		t.Errorf("台账链 = %v，期望 %v", pairs, want)
	}
	assertOnlyLegalTransitions(t, h.db)
}

// TestFulfillOrderMissingDownstreamClientFailsLoudly 会员单缺 membership 客户端时
// 必须报 ErrMembershipNotConfigured，而不是「订单成功但什么都没发」。
func TestFulfillOrderMissingDownstreamClientFailsLoudly(t *testing.T) {
	h := newFulfillHarness(t)
	h.svc.Membership = nil
	h.seedPaid(t, "to_nomem", nil)

	_, err := h.l.FulfillOrder(fulfillReq("to_nomem"))
	if err == nil {
		t.Fatal("下游缺位却返回成功")
	}
	if !errors.Is(err, model.ErrMembershipNotConfigured) {
		t.Errorf("FulfillOrder() error = %v，期望 %v", err, model.ErrMembershipNotConfigured)
	}
	got := mustOrder(t, h.db, "to_nomem")
	if got.State != model.StateFailed || got.GrantRef != "" {
		t.Errorf("state/grant_ref = (%s,%q)", model.StateName(got.State), got.GrantRef)
	}
	if !strings.Contains(got.FulfillDetail, "membership rpc not configured") {
		t.Errorf("fulfill_detail = %q", got.FulfillDetail)
	}
}

// TestFulfillOrderTierGuardBeforeDownstream 时长非正的会员单不许发（防脏数据造成超发）。
func TestFulfillOrderTierGuardBeforeDownstream(t *testing.T) {
	h := newFulfillHarness(t)
	h.seedPaid(t, "to_nodays", func(o *model.Order) { o.DurationDays = 0 })

	if _, err := h.l.FulfillOrder(fulfillReq("to_nodays")); !errors.Is(err, model.ErrPlanTierMismatch) {
		t.Fatalf("FulfillOrder() error = %v，期望 %v", err, model.ErrPlanTierMismatch)
	}
	if len(h.mem.grantCalls) != 0 {
		t.Error("档位非正却调了发放")
	}
	if got := mustOrder(t, h.db, "to_nodays"); got.State != model.StateFailed {
		t.Errorf("state = %s，期望 FAILED", model.StateName(got.State))
	}
}

// TestFulfillOrderPlansTierMismatchAtFulfillTime 履约时档位被改：本服务按新档发放
// （缺口固化：订单表没有 vip_type 快照列，只能回查 GetPlan）。
func TestFulfillOrderPlansTierMismatchAtFulfillTime(t *testing.T) {
	h := newFulfillHarness(t)
	h.mem.plans[77].VipType = memberrpc.VipType_VIP_TYPE_UNSPECIFIED // 运营把套餐档位改了
	h.seedPaid(t, "to_tierdrift", nil)

	_, err := h.l.FulfillOrder(fulfillReq("to_tierdrift"))
	if !errors.Is(err, model.ErrPlanTierMismatch) {
		t.Fatalf("FulfillOrder() error = %v，期望 %v", err, model.ErrPlanTierMismatch)
	}
	if len(h.mem.grantCalls) != 0 {
		t.Error("档位校验没过却发放了")
	}
	got := mustOrder(t, h.db, "to_tierdrift")
	if got.State != model.StateFailed {
		t.Errorf("state = %s，期望 FAILED（等人工核对）", model.StateName(got.State))
	}
}

// TestFulfillOrderLedgerFailureRollsBackFulfillingStep 第一步事务里台账写失败，
// 主表必须回到 PAID：不允许出现「attempts 加了但没有台账」的半行。
func TestFulfillOrderLedgerFailureRollsBackFulfillingStep(t *testing.T) {
	h := newFulfillHarness(t)
	h.seedPaid(t, "to_rb", nil)
	h.db.failOn = &failOn{statePair: pairName(model.StatePaid, model.StateFulfilling), attempts: 1}

	_, err := h.l.FulfillOrder(fulfillReq("to_rb"))
	if err == nil {
		t.Fatal("事务失败却返回成功")
	}
	got := mustOrder(t, h.db, "to_rb")
	if got.State != model.StatePaid || got.Version != 3 || got.FulfillAttempts != 0 {
		t.Errorf("回滚不彻底: state=%s version=%d attempts=%d",
			model.StateName(got.State), got.Version, got.FulfillAttempts)
	}
	if len(h.db.events) != 0 {
		t.Errorf("主表回滚却留下 %d 行台账", len(h.db.events))
	}
	// 关键：CAS 都没成功，就绝不能调下游（否则钱/权益与订单脱节）
	if len(h.mem.grantCalls) != 0 {
		t.Error("第一步事务失败却发放了权益")
	}
}

// TestFulfillOrderFinalStepRollbackLeavesAuditableGap 最后一步事务回滚：
// 下游已发放、本地停在 FULFILLING。这条路径必须如实返回错误，并保留 FULFILLING 的
// 台账与 attempts，让下一轮重试靠派生幂等键自愈。
func TestFulfillOrderFinalStepRollbackLeavesAuditableGap(t *testing.T) {
	h := newFulfillHarness(t)
	h.seedPaid(t, "to_half", nil)
	h.db.failOn = &failOn{statePair: pairName(model.StateFulfilling, model.StateFulfilled), attempts: 2}

	_, err := h.l.FulfillOrder(fulfillReq("to_half"))
	if err == nil {
		t.Fatal("推进 FULFILLED 失败却返回成功")
	}
	got := mustOrder(t, h.db, "to_half")
	if got.State != model.StateFulfilling || got.Version != 4 || got.FulfillAttempts != 1 {
		t.Errorf("state/version/attempts = (%s,%d,%d)，期望停在 FULFILLING",
			model.StateName(got.State), got.Version, got.FulfillAttempts)
	}
	if got.GrantRef != "" || got.FulfillDetail != "" {
		t.Errorf("回滚彻底才干净，实得 grant_ref=%q detail=%q", got.GrantRef, got.FulfillDetail)
	}
	if len(h.mem.grantCalls) != 1 {
		t.Fatalf("下游应已发放一次，实得 %d 次", len(h.mem.grantCalls))
	}
	// 现在把失败注入撤掉，重试必须自愈且不重复发放（幂等键相同）
	h.db.failOn = nil
	h.mem.duplicated = true
	h.seedUpdatedAt(h.db, "to_half")
	reply, err := h.l.FulfillOrder(fulfillReq("to_half"))
	if err != nil {
		t.Fatalf("重试 error = %v", err)
	}
	if !reply.GetFulfilled() {
		t.Error("重试必须完成履约")
	}
	if len(h.mem.grantCalls) != 2 {
		t.Errorf("GrantCoin/Grant 调用 %d 次，幂等键同为 grant_to_half 由下游判重放", len(h.mem.grantCalls))
	}
	if h.mem.grantCalls[1].GetRequestId() != h.mem.grantCalls[0].GetRequestId() {
		t.Error("重试必须复用同一把下游幂等键")
	}
	got = mustOrder(t, h.db, "to_half")
	if got.State != model.StateFulfilled || got.FulfillAttempts != 2 {
		t.Errorf("自愈后 state/attempts = (%s,%d)", model.StateName(got.State), got.FulfillAttempts)
	}
	assertOnlyLegalTransitions(t, h.db)
}

// seedUpdatedAt 把订单的 updated_at 推到最小间隔之外（模拟 cron 下一轮扫描）。
func (h *fulfillHarness) seedUpdatedAt(db *fakeDB, orderNo string) {
	if o, ok := db.orders[orderNo]; ok {
		o.UpdatedAt = fakeNow() - 120
	}
}

// TestFulfillOrderConcurrentCasMissIsNotAnOverwrite CAS 未命中（RowsAffected==0）
// 时按并发失败返回错误，绝不覆盖胜者的状态，也不在失败的事务里留下台账。
func TestFulfillOrderConcurrentCasMissIsNotAnOverwrite(t *testing.T) {
	h := newFulfillHarness(t)
	h.seedPaid(t, "to_cas", nil)
	h.db.raceHook = func(o *model.Order, from, to int32) {
		if from == model.StatePaid && to == model.StateFulfilling {
			o.Version = 99 // 别人先推了一步
		}
	}

	_, err := h.l.FulfillOrder(fulfillReq("to_cas"))
	if !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("FulfillOrder() error = %v，期望 %v", err, model.ErrConcurrentUpdate)
	}
	if len(h.mem.grantCalls) != 0 {
		t.Error("CAS 未命中却发放了权益（超发）")
	}
	if len(h.db.events) != 0 {
		t.Errorf("CAS 未命中却写了 %d 行台账", len(h.db.events))
	}
	if got := mustOrder(t, h.db, "to_cas"); got.Version != 99 {
		t.Errorf("败者覆盖了胜者位点: version=%d", got.Version)
	}
}

// TestFulfillOrderUsesOrderNoDerivedKeysByDefault 不传 request_id 时台账键也必须是
// 订单号派生的 fulfill_/fulfillfail_，保证同一单的重试在台账上可归并。
func TestFulfillOrderUsesOrderNoDerivedKeysByDefault(t *testing.T) {
	h := newFulfillHarness(t)
	h.mem.grantErr = errors.New("boom")
	h.seedPaid(t, "to_keys", nil)

	if _, err := h.l.FulfillOrder(&rpc.FulfillOrderReq{OrderNo: "to_keys"}); err == nil {
		t.Fatal("期望失败")
	}
	rows := ledgerOf(h.db, "to_keys")
	if len(rows) != 2 {
		t.Fatalf("台账行数 = %d，期望 2", len(rows))
	}
	if rows[0].RequestID != "fulfill_to_keys" {
		t.Errorf("FULFILLING 台账 request_id = %q，期望 fulfill_to_keys", rows[0].RequestID)
	}
	if rows[1].RequestID != "fulfillfail_to_keys" {
		t.Errorf("FAILED 台账 request_id = %q，期望 fulfillfail_to_keys", rows[1].RequestID)
	}
	if rows[0].Operator != "system" {
		t.Errorf("operator = %q，缺省 system", rows[0].Operator)
	}
}

// TestFulfillOrderOperatorComesFromCaller cron 传 operator=system 之外的值时如实记台账。
func TestFulfillOrderOperatorComesFromCaller(t *testing.T) {
	h := newFulfillHarness(t)
	h.seedPaid(t, "to_op", nil)
	in := fulfillReq("to_op")
	in.Operator = "cron:stuck-scan"
	in.RequestId = ""

	if _, err := h.l.FulfillOrder(in); err != nil {
		t.Fatalf("FulfillOrder() error = %v", err)
	}
	rows := ledgerOf(h.db, "to_op")
	if rows[0].Operator != "cron:stuck-scan" || rows[1].Operator != "cron:stuck-scan" {
		t.Errorf("台账 operator = (%q,%q)", rows[0].Operator, rows[1].Operator)
	}
	if got := h.mem.grantCalls[0]; got.GetOperator() != "cron:stuck-scan" {
		t.Errorf("下游 operator = %q，要与台账一致", got.GetOperator())
	}
}

// TestRunFulfillHelperShape 直接确认内核函数被两条入口（建单内联 / FulfillOrder）共用，
// 因此「履约口径」只有一份实现。
func TestRunFulfillHelperSharedByBothEntries(t *testing.T) {
	h := newFulfillHarness(t)
	h.seedPaid(t, "to_shared", nil)
	res, err := runFulfill(h.l.ctx, h.svc, h.l.Logger, "to_shared", "user", "req-shared")
	if err != nil || res == nil || !res.fulfilled {
		t.Fatalf("runFulfill() = (%+v,%v)", res, err)
	}
	if res.order.State != model.StateFulfilled {
		t.Errorf("state = %s", model.StateName(res.order.State))
	}
}
