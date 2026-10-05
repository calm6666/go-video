package logic

// 状态机边界矩阵。
//
// 订单状态机是 trade-order 对外承诺的核心契约（AGENTS.md §5：状态机只属于本服务，
// payment / membership 不得改写订单状态），所以这张表是本服务的「宪法」：
//   - 期望边集是**手抄自 rpc/tradeorder.proto 文件头注释**的独立事实，
//     不是从 CanTransition 反推出来的，否则测试就成了同义反复；
//   - 代码里那条超出 proto 字面枚举的补偿边（FULFILLING → PAID）与
//     FAILED / REFUND_REJECTED 的「无出边 / 无入边」缺口都单独钉住，
//     将来谁偷偷加一条边，这里会先响。

import (
	"errors"
	"testing"

	"go-video/services/trade-order/model"
)

// allStates 是 to_order.state 的全部已定义取值（1..11）。
var allStates = []int32{
	model.StateCreated, model.StatePaying, model.StatePaid, model.StateFulfilling,
	model.StateFulfilled, model.StateCancelled, model.StateFailed,
	model.StateRefundRequested, model.StateRefundApproved, model.StateRefunded,
	model.StateRefundRejected,
}

// allowedEdges 手抄自 proto 文件头注释 + model.CanTransition 注释里明确记录的补偿边：
//
//	CREATED → PAYING | CANCELLED
//	PAYING → PAID | CANCELLED
//	PAID → FULFILLING | FAILED | REFUND_REQUESTED
//	FULFILLING → FULFILLED | FAILED | PAID(补偿) | FULFILLING(同态重试)
//	FULFILLED → REFUND_REQUESTED
//	REFUND_REQUESTED → REFUND_APPROVED | 回到原状态(PAID / FULFILLED)
//	REFUND_APPROVED → REFUNDED
//	CANCELLED / FAILED / REFUNDED / REFUND_REJECTED → 无出边
var allowedEdges = map[int32][]int32{
	model.StateCreated:         {model.StatePaying, model.StateCancelled},
	model.StatePaying:          {model.StatePaid, model.StateCancelled},
	model.StatePaid:            {model.StateFulfilling, model.StateFailed, model.StateRefundRequested},
	model.StateFulfilling:      {model.StateFulfilled, model.StateFailed, model.StatePaid, model.StateFulfilling},
	model.StateFulfilled:       {model.StateRefundRequested},
	model.StateRefundRequested: {model.StateRefundApproved, model.StateFulfilled, model.StatePaid},
	model.StateRefundApproved:  {model.StateRefunded},
	model.StateCancelled:       {},
	model.StateFailed:          {},
	model.StateRefunded:        {},
	model.StateRefundRejected:  {},
}

func TestTransitionMatrixMatchesContract(t *testing.T) {
	for _, from := range allStates {
		for _, to := range allStates {
			got := model.CanTransition(from, to)
			want := false
			for _, allow := range allowedEdges[from] {
				if allow == to {
					want = true
					break
				}
			}
			if got != want {
				t.Errorf("CanTransition(%s, %s)=%v，契约期望 %v",
					model.StateName(from), model.StateName(to), got, want)
			}
		}
	}
}

// TestTerminalStatesHaveNoOutgoingEdge 是「终态不可被任何接口推进」的全局护栏：
// 只要有任何一条出边被打开，退款/关单后的订单就可能被重新履约。
func TestTerminalStatesHaveNoOutgoingEdge(t *testing.T) {
	terminal := map[int32]string{
		model.StateCancelled:      "CANCELLED",
		model.StateRefunded:       "REFUNDED",
		model.StateFailed:         "FAILED（本轮契约无 FAILED → FULFILLING 重试边，见 README 缺口）",
		model.StateRefundRejected: "REFUND_REJECTED（既无入边也无出边）",
	}
	for s, note := range terminal {
		for _, to := range allStates {
			if model.CanTransition(s, to) {
				t.Errorf("%s 出现了出边 -> %s（%s）", model.StateName(s), model.StateName(to), note)
			}
		}
		if model.CanTransition(s, s) {
			t.Errorf("%s 允许同态迁移，重试会被伪装成一次成功推进", model.StateName(s))
		}
	}
}

// TestRefundRejectedHasNoIncomingEdge 钉住本轮实现口径：
// 驳回退款按「回到申请前的原状态」实现，绝不写 REFUND_REJECTED（写进去就是死胡同）。
func TestRefundRejectedHasNoIncomingEdge(t *testing.T) {
	for _, from := range allStates {
		if model.CanTransition(from, model.StateRefundRejected) {
			t.Errorf("%s → REFUND_REJECTED 被放开：钱没退、权益还在、订单永远动不了", model.StateName(from))
		}
	}
	if !model.ValidState(model.StateRefundRejected) {
		t.Error("REFUND_REJECTED 仍是已定义状态（proto 编号 11 不能回收）")
	}
}

// TestSameStateRetryOnlyForFulfilling：同态迁移只放开 FULFILLING，
// 它代表「再试一轮履约」，必须落台账并让 fulfill_attempts 可见增长。
func TestSameStateRetryOnlyForFulfilling(t *testing.T) {
	for _, s := range allStates {
		got := model.CanTransition(s, s)
		want := s == model.StateFulfilling
		if got != want {
			t.Errorf("同态 %s->%s = %v，期望 %v", model.StateName(s), model.StateName(s), got, want)
		}
	}
}

// TestFulfillingToPaidCompensationEdge 是唯一一条 proto 字面枚举里没有、
// 由 model 注释显式记录的补偿边。它存在是有意为之（履约回退后可重驱动），
// 但也意味着 proto 注释与实现不一致 —— 这条断言就是那处偏差的登记处。
func TestFulfillingToPaidCompensationEdge(t *testing.T) {
	if !model.CanTransition(model.StateFulfilling, model.StatePaid) {
		t.Error("补偿边 FULFILLING → PAID 被删掉了，履约回退路径会当场失效")
	}
	// 反向（PAID → PAID 之外的回退到未支付态）一律不允许：不能倒退到 PAYING/CREATED。
	for _, to := range []int32{model.StateCreated, model.StatePaying} {
		if model.CanTransition(model.StatePaid, to) {
			t.Errorf("PAID 可以退到 %s：已收钱的单被回退到未受理态", model.StateName(to))
		}
	}
}

func TestCanTransitionRejectsUndefinedValues(t *testing.T) {
	for _, v := range []int32{0, -1, 12, 99, model.StateRefundRejected + 1} {
		if model.ValidState(v) {
			t.Errorf("ValidState(%d) 应为 false", v)
		}
		for _, s := range allStates {
			if model.CanTransition(v, s) || model.CanTransition(s, v) {
				t.Errorf("未定义取值 %d 参与了迁移判定", v)
			}
		}
	}
}

func TestStateNamesCoverEveryDefinedState(t *testing.T) {
	want := map[int32]string{
		model.StateCreated: "CREATED", model.StatePaying: "PAYING", model.StatePaid: "PAID",
		model.StateFulfilling: "FULFILLING", model.StateFulfilled: "FULFILLED",
		model.StateCancelled: "CANCELLED", model.StateFailed: "FAILED",
		model.StateRefundRequested: "REFUND_REQUESTED", model.StateRefundApproved: "REFUND_APPROVED",
		model.StateRefunded: "REFUNDED", model.StateRefundRejected: "REFUND_REJECTED",
	}
	for s, name := range want {
		if got := model.StateName(s); got != name {
			t.Errorf("StateName(%d)=%s，期望 %s", s, got, name)
		}
	}
	if got := model.StateName(0); got != "UNKNOWN(0)" {
		t.Errorf("未定义取值不该有可读名（运营页会误读），得到 %q", got)
	}
}

// TestIsPaidOrLater 钉住「钱已受理」这个集合：BindPayment 的幂等与
// CancelOrder 的「已支付不能取消」都靠它，多一个少一个都会改变资金结论。
func TestIsPaidOrLater(t *testing.T) {
	paidOrLater := map[int32]bool{
		model.StatePaid: true, model.StateFulfilling: true, model.StateFulfilled: true,
		model.StateRefundRequested: true, model.StateRefundApproved: true, model.StateRefunded: true,
		model.StateRefundRejected: true, model.StateFailed: true,
		model.StateCreated: false, model.StatePaying: false, model.StateCancelled: false,
	}
	for s, want := range paidOrLater {
		if got := model.IsPaidOrLater(s); got != want {
			t.Errorf("IsPaidOrLater(%s)=%v，期望 %v", model.StateName(s), got, want)
		}
	}
}

// TestModelRejectsIllegalTransition 证明「双层护栏」里的第二层是真的：
// 即使 logic 传了非法前态/后态组合，model 自己也不会发出 UPDATE。
func TestModelRejectsIllegalTransition(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	o := seedOrder(t, db, &model.Order{OrderNo: "to_x", Mid: 7, State: model.StateCancelled, Version: 3})

	cases := []struct{ from, to int32 }{
		{model.StateCancelled, model.StatePaid},
		{model.StatePaid, model.StateCreated},
		{model.StateFulfilled, model.StatePaid},
		{model.StateFailed, model.StateFulfilling},
		{model.StateRefundRequested, model.StateRefundRejected},
		{o.State, model.StateFulfilled},
	}
	for _, c := range cases {
		_, err := svcCtx.Orders.TransitionTx(t.Context(), nil, o.OrderNo, c.from, c.to, o.Version, nil)
		if !errors.Is(err, model.ErrInvalidStateTransition) {
			t.Errorf("%s -> %s 应被 model 拦住，得到 %v", model.StateName(c.from), model.StateName(c.to), err)
		}
		if got := orderState(t, db, o.OrderNo); got != model.StateCancelled {
			t.Fatalf("被拒的迁移把状态改成了 %s", model.StateName(got))
		}
	}
}

// TestTransitionCasMissIsNotAnError 复刻 MySQL 的 RowsAffected 语义（DSN 禁 clientFoundRows）：
// 条件不命中时是「0 行受影响」而不是「查询失败」，logic 必须据此判定并发失败。
func TestTransitionCasMissIsNotAnError(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_cas", Mid: 7, State: model.StatePaid, Version: 4})

	cases := []struct {
		name    string
		from    int32
		to      int32
		version int64
	}{
		{"版本过期", model.StatePaid, model.StateFulfilling, 3},
		{"未来版本", model.StatePaid, model.StateFulfilling, 5},
		{"前态已被并发推走", model.StateFulfilling, model.StateFulfilled, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, err := svcCtx.Orders.TransitionTx(t.Context(), nil, "to_cas", c.from,
				c.to, c.version, nil)
			if err != nil {
				t.Fatalf("CAS 未命中不该报错（0 行受影响），得到 %v", err)
			}
			if ok {
				t.Error("CAS 未命中却返回 true，等于把并发覆盖放行")
			}
			if got := mustOrder(t, db, "to_cas"); got.State != model.StatePaid || got.Version != 4 {
				t.Errorf("未命中的 CAS 改动了行: state=%s version=%d", model.StateName(got.State), got.Version)
			}
		})
	}
	if _, err := svcCtx.Orders.TransitionTx(t.Context(), nil, "to_cas", model.StatePaid,
		model.StateFulfilling, 0, nil); !errors.Is(err, model.ErrExpectedVersionRequired) {
		t.Errorf("expected_version<=0 必须报错，得到 %v", err)
	}
}

// TestTransitionIncrementsVersionAndRecordsLedger 是「状态与台账同事务」的最小证明：
// 走一次 transitionWithEvent，主表版本 +1、台账多一行，且台账记录的 from/to 与实际一致。
func TestTransitionIncrementsVersionAndRecordsLedger(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_tx", Mid: 7, State: model.StatePaid, Version: 4})

	if err := transitionWithEvent(t.Context(), svcCtx, "to_tx", model.StatePaid, model.StateFulfilling, 4,
		"cron", "req-1", "attempt 1", model.NewOrderUpdate().IncFulfillAttempts()); err != nil {
		t.Fatalf("合法迁移不该失败: %v", err)
	}
	got := mustOrder(t, db, "to_tx")
	if got.State != model.StateFulfilling || got.Version != 5 || got.FulfillAttempts != 1 {
		t.Errorf("推进结果不符: state=%s version=%d attempts=%d",
			model.StateName(got.State), got.Version, got.FulfillAttempts)
	}
	rows := ledgerOf(db, "to_tx")
	if len(rows) != 1 {
		t.Fatalf("台账行数 %d，期望 1", len(rows))
	}
	e := rows[0]
	if e.FromState != model.StatePaid || e.ToState != model.StateFulfilling ||
		e.Operator != "cron" || e.RequestID != "req-1" || e.Reason != "attempt 1" {
		t.Errorf("台账行内容与迁移不一致: %+v", e)
	}
	assertOnlyLegalTransitions(t, db)
}

// TestTransitionLedgerFailureRollsBackMainRow 证明「同事务」不是注释里的一句话：
// 台账写入失败时，主表状态必须回到迁移前（否则会出现「状态变了但没人知道是谁推的」）。
func TestTransitionLedgerFailureRollsBackMainRow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_rb", Mid: 7, State: model.StatePaid, Version: 4})
	db.failOn = &failOn{statePair: pairName(model.StatePaid, model.StateFulfilling), attempts: db.txRuns + 1}

	err := transitionWithEvent(t.Context(), svcCtx, "to_rb", model.StatePaid, model.StateFulfilling, 4,
		"cron", "req-1", "x", nil)
	if err == nil {
		t.Fatal("台账写入失败时事务必须报错")
	}
	got := mustOrder(t, db, "to_rb")
	if got.State != model.StatePaid || got.Version != 4 {
		t.Errorf("事务回滚不彻底: state=%s version=%d", model.StateName(got.State), got.Version)
	}
	if len(ledgerOf(db, "to_rb")) != 0 {
		t.Error("回滚后台账还留着半行")
	}
}

// TestRefundTransitionChainIsLinear 按 proto 承诺的退款链路走一遍，
// 断言它只能按 REFUND_REQUESTED → REFUND_APPROVED → REFUNDED 的顺序推进。
func TestRefundTransitionChainIsLinear(t *testing.T) {
	chain := []int32{model.StateRefundRequested, model.StateRefundApproved, model.StateRefunded}
	for i := 1; i < len(chain); i++ {
		if !model.CanTransition(chain[i-1], chain[i]) {
			t.Errorf("链路断了: %s -> %s", model.StateName(chain[i-1]), model.StateName(chain[i]))
		}
	}
	if model.CanTransition(model.StateRefundRequested, model.StateRefunded) {
		t.Error("可以跳过 REFUND_APPROVED 直接 REFUNDED：退款事实与权益回收就没有分界点了")
	}
	if model.CanTransition(model.StateRefunded, model.StateRefundApproved) {
		t.Error("已退款的单可以退回 REFUND_APPROVED：终态被打开")
	}
}
