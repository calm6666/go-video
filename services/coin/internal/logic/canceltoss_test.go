package logic

import (
	"context"
	"testing"
	"time"

	"go-video/services/coin/model"
	"go-video/services/coin/rpc"
)

// 取消投币（CancelToss）的口径证明：窗口内全额退回、只能撤销生效中的记录、
// 余额与流水与投币状态与日额度必须同事务、重复取消不双退、
// total_tossed 不回退（proto 锁定的历史口径）。

func cancelReq(mutate func(*rpc.CancelTossReq)) *rpc.CancelTossReq {
	in := &rpc.CancelTossReq{
		Mid:       tossMid,
		TargetAid: tossAid,
		RequestId: "req-cancel-1",
		Operator:  "user",
	}
	if mutate != nil {
		mutate(in)
	}
	return in
}

// cancelSeed 描述一份「自洽的取消前置账」：balance == SUM(cn_flow.delta) 必须成立，
// 否则对账不变式的断言就变成了夹具自己的错。
type cancelSeed struct {
	balance     int64         // 落库余额（当前真值）
	count       int32         // 投币记录枚数
	ago         time.Duration // 距最后一次投币
	alreadyPaid bool          // 台账里已有一条取消流水（余额已含那次退款）
	cancelReqID string        // 已有取消流水的 request_id
	rowState    int32         // 投币行状态，0 即 ACTIVE
	target      int64         // 已有取消流水指向的 aid，0 即 tossAid
}

// seedCancelLedger 按「发放 → 投币 → （可选）取消」铺台账，日桶落在 last_toss_date 那天。
// balance 始终是**当前**余额：没退过币时发放额要比余额多 count（那 count 枚还扣着），
// 已经退过时发放额就是余额本身（投出又退回，净额为 0）。
func seedCancelLedger(t *testing.T, db *fakeDB, o cancelSeed) *model.Toss {
	t.Helper()
	now := model.NowUnix()
	last := now - int64(o.ago/time.Second)
	day := model.DayNo(time.Unix(last, 0))
	grant := o.balance + int64(o.count)
	if o.alreadyPaid {
		grant = o.balance
	}
	seedAccount(db, tossMid, o.balance)
	seedFlow(db, &model.Flow{Mid: tossMid, FlowType: model.FlowTypeAdminGrant, Delta: grant,
		BalanceAfter: grant, Operator: "seed", RequestID: "seed:grant:cancel-case", Ctime: last - 60})
	seedFlow(db, &model.Flow{Mid: tossMid, FlowType: model.FlowTypeToss, Delta: -int64(o.count),
		BalanceAfter: grant - int64(o.count), TargetAid: tossAid, Operator: "user",
		RequestID: "req-origin-toss", Ctime: last})
	if o.alreadyPaid {
		target := o.target
		if target == 0 {
			target = tossAid
		}
		seedFlow(db, &model.Flow{Mid: tossMid, FlowType: model.FlowTypeCancelToss, Delta: int64(o.count),
			BalanceAfter: grant, TargetAid: target, Operator: "user",
			RequestID: o.cancelReqID, Ctime: last + 30})
	}
	if o.rowState == model.TossStateCancelled {
		// 取消已回退日额度，桶里就该是 0（夹具自己不能留不一致）。
		seedDaily(db, tossMid, day, 0)
	} else {
		seedDaily(db, tossMid, day, o.count)
	}
	row := &model.Toss{Mid: tossMid, TargetAid: tossAid, Count: o.count,
		FirstTossedAt: last, LastTossedAt: last, LastRequestID: "req-origin-toss", LastTossDate: day}
	if o.alreadyPaid && o.rowState == model.TossStateCancelled {
		row.CancelledAt = last + 30
	}
	row.State = o.rowState
	return seedToss(db, row)
}

// seedActiveToss 放一条「窗口内、投了 count 枚」的生效投币记录 + 自洽台账。
func seedActiveToss(t *testing.T, db *fakeDB, balance int64, count int32, tossedAgo time.Duration) *model.Toss {
	t.Helper()
	return seedCancelLedger(t, db, cancelSeed{balance: balance, count: count, ago: tossedAgo})
}

func TestCancelTossRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*rpc.CancelTossReq)
		wantErr error
	}{
		{"mid 非正数", func(in *rpc.CancelTossReq) { in.Mid = 0 }, model.ErrInvalidMid},
		{"缺 request_id", func(in *rpc.CancelTossReq) { in.RequestId = "" }, model.ErrRequestIDRequired},
		{"target_aid 非正数", func(in *rpc.CancelTossReq) { in.TargetAid = -1 }, model.ErrInvalidTargetAid},
		{"代客操作不给理由", func(in *rpc.CancelTossReq) { in.Operator = "ops-001" }, model.ErrOperatorReasonRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedActiveToss(t, db, 8, 2, time.Minute)
			_, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(tc.mutate))
			wantErrIs(t, err, tc.wantErr)
			db.wantNoCall(t, "Accounts.RefundForCancelTx", "Flows.InsertTx")
			if db.txRuns != 0 {
				t.Errorf("入参/凭据校验不该开事务：%d 次", db.txRuns)
			}
		})
	}
}

func TestCancelTossWithoutTossRecordIsTargetInvalidConclusion(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 8)

	reply, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	if err != nil {
		t.Fatalf("没投过币是结论不是错误：%v", err)
	}
	if reply.Cancelled || reply.Reason != rpc.TossRejectReason_TOSS_REJECT_TARGET_INVALID {
		t.Fatalf("期待 TARGET_INVALID，得到 %+v", reply)
	}
	db.wantNoCall(t, "Accounts.RefundForCancelTx", "Flows.InsertTx")
	if acc := db.account(t, tossMid); acc.Balance != 8 {
		t.Errorf("余额被改动：%d", acc.Balance)
	}
	db.wantLedgerParity(t, tossMid)
}

func TestCancelTossAlreadyCancelledIsDuplicatedWithoutSecondRefund(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	// 投 2 枚 → 已退回 2 枚（余额回到 10），记录状态 CANCELLED。
	seedCancelLedger(t, db, cancelSeed{balance: 10, count: 2, ago: 2 * time.Minute,
		alreadyPaid: true, cancelReqID: "req-old-cancel", rowState: model.TossStateCancelled})

	// 换一把幂等键再取消同一条已取消记录：必须 duplicated=true 且不再退币。
	reply, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	if err != nil {
		t.Fatalf("重复取消不该失败：%v", err)
	}
	cancelFlow := db.flowByRequestID(t, "req-old-cancel")
	if !reply.Cancelled || !reply.Duplicated || reply.FlowId != cancelFlow.ID {
		t.Fatalf("期待 duplicated=true + 回捞最近一条取消流水，得到 %+v", reply)
	}
	db.wantNoCall(t, "Accounts.RefundForCancelTx", "Flows.InsertTx", "Tosses.CancelTx")
	if acc := db.account(t, tossMid); acc.Balance != 10 || acc.TotalTossed != 0 {
		t.Errorf("重复取消改动了账户：%+v", acc)
	}
	if db.txRuns != 0 {
		t.Errorf("已取消的重复请求不该开事务：%d", db.txRuns)
	}
	db.wantLedgerParity(t, tossMid)
}

func TestCancelTossWindowBoundary(t *testing.T) {
	cases := []struct {
		name    string
		ago     time.Duration
		wantOff bool
	}{
		{"窗口边界内（差 1 秒）", 86399 * time.Second, false},
		{"恰好等于窗口", 86400 * time.Second, false}, // 判定是 now-last > window
		{"刚过窗口 1 秒", 86401 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			fixedClock(t, clockAt(0))
			seedActiveToss(t, db, 8, 2, tc.ago)

			reply, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
			if err != nil {
				t.Fatalf("窗口判定不该报错：%v", err)
			}
			if tc.wantOff {
				if reply.Cancelled || reply.Reason != rpc.TossRejectReason_TOSS_REJECT_CANCEL_WINDOW_EXPIRED {
					t.Fatalf("超窗必须以结论返回，得到 %+v", reply)
				}
				wantDetail(t, reply.RejectDetail, "86400 秒")
				db.wantNoCall(t, "Accounts.RefundForCancelTx", "Flows.InsertTx")
				if acc := db.account(t, tossMid); acc.Balance != 8 {
					t.Errorf("超窗仍退了币：%d", acc.Balance)
				}
				db.wantLedgerParity(t, tossMid)
				return
			}
			if !reply.Cancelled || reply.Duplicated {
				t.Fatalf("窗口内应取消成功，得到 %+v", reply)
			}
		})
	}
}

func TestCancelTossRefundsInOneTransaction(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedActiveToss(t, db, 8, 2, time.Minute) // 余额 8、投了 2 枚、total_tossed 0

	before := db.account(t, tossMid)
	reply, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	if err != nil {
		t.Fatalf("取消失败：%v", err)
	}
	if !reply.Cancelled || reply.Duplicated || reply.Reason != rpc.TossRejectReason_TOSS_ACCEPTED {
		t.Fatalf("期待取消成功，得到 %+v", reply)
	}
	if reply.FlowId == 0 || reply.Toss == nil || reply.Toss.State != rpc.TossState_TOSS_STATE_CANCELLED {
		t.Fatalf("回显不符：%+v", reply)
	}

	acc := db.account(t, tossMid)
	if acc.Balance != before.Balance+2 {
		t.Errorf("全额退回不成立：balance %d -> %d", before.Balance, acc.Balance)
	}
	if acc.TotalTossed != before.TotalTossed {
		t.Errorf("total_tossed 是历史口径，不该回退：%d -> %d", before.TotalTossed, acc.TotalTossed)
	}
	flow := db.wantFlowBalanceAfter(t, "req-cancel-1", acc.Balance)
	if flow.FlowType != model.FlowTypeCancelToss || flow.Delta != 2 || flow.TargetAid != tossAid ||
		flow.Operator != "user" {
		t.Errorf("取消流水不符： %+v", flow)
	}
	row := db.toss(t, tossMid, tossAid)
	if row.State != model.TossStateCancelled || row.CancelledAt != model.NowUnix() {
		t.Errorf("投币记录未置取消：%+v", row)
	}
	if d := db.daily[dailyKey{tossMid, row.LastTossDate}]; d == nil || d.Tossed != 0 {
		t.Errorf("日额度未回退：%+v", d)
	}
	db.wantLedgerParity(t, tossMid)
	db.wantBalanceWritesVia(t, "RefundForCancelTx")
	if db.txRuns != 1 || db.txCommits != 1 {
		t.Fatalf("应只有一次提交的事务：runs=%d commits=%d", db.txRuns, db.txCommits)
	}
	// 锁序与投币一致（README §3）：账户行锁 → 幂等复核 → 投币行锁 → 退款 → 置取消 → 回退额度 → 台账。
	db.wantCallOrder(t,
		"Accounts.LockForUpdateTx", "Flows.FindByRequestID", "Tosses.LockByTargetTx",
		"Accounts.RefundForCancelTx", "Tosses.CancelTx", "Daily.RollbackTx", "Flows.InsertTx")
}

func TestCancelTossRollsBackWhenLedgerWriteFails(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedActiveToss(t, db, 8, 2, time.Minute)
	db.Fail("Flows.InsertTx", errBoom)

	_, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	wantErrIs(t, err, errBoom)
	// 余额回退、投币状态、日额度必须一起回滚：不可能出现「退了币没流水」。
	if acc := db.account(t, tossMid); acc.Balance != 8 {
		t.Errorf("台账写失败但余额仍被退回：%d", acc.Balance)
	}
	row := db.toss(t, tossMid, tossAid)
	if row.State != model.TossStateActive {
		t.Errorf("台账写失败但投币记录被置取消：%+v", row)
	}
	if d := db.daily[dailyKey{tossMid, row.LastTossDate}]; d == nil || d.Tossed != 2 {
		t.Errorf("台账写失败但日额度被回退：%+v", d)
	}
	if db.txRollbacks != 1 {
		t.Errorf("期待事务回滚，实际 rollbacks=%d", db.txRollbacks)
	}
	db.wantLedgerParity(t, tossMid)
}

func TestCancelTossReplayReturnsDuplicatedWithoutRefund(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	// 该 request_id 的取消早已落账（余额已含退款），投币行因取消后又投了一次而回到 ACTIVE。
	seedCancelLedger(t, db, cancelSeed{balance: 10, count: 2, ago: time.Minute,
		alreadyPaid: true, cancelReqID: "req-cancel-1"})

	reply, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	if err != nil {
		t.Fatalf("重放不该失败：%v", err)
	}
	first := db.flowByRequestID(t, "req-cancel-1")
	if !reply.Cancelled || !reply.Duplicated || reply.FlowId != first.ID {
		t.Fatalf("期待 duplicated=true + 首次 flow_id，得到 %+v", reply)
	}
	db.wantNoCall(t, "Accounts.RefundForCancelTx", "Flows.InsertTx")
	if db.txRuns != 0 {
		t.Errorf("快路径命中重放不该开事务：%d", db.txRuns)
	}
	if acc := db.account(t, tossMid); acc.Balance != 10 {
		t.Errorf("重放退了第二次币：%d", acc.Balance)
	}
	db.wantLedgerParity(t, tossMid)
}

func TestCancelTossReplayWithDifferentTargetIsConflict(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	// 同一个 request_id 已用于取消另一个内容：串号必须报错而不是任选一份结论。
	seedCancelLedger(t, db, cancelSeed{balance: 10, count: 2, ago: time.Minute,
		alreadyPaid: true, cancelReqID: "req-cancel-1", target: 2002})

	_, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	wantErrIs(t, err, model.ErrIdempotencyConflict)
	wantErrContains(t, err, "aid=2002")
	db.wantNoCall(t, "Accounts.RefundForCancelTx", "Flows.InsertTx")
	if acc := db.account(t, tossMid); acc.Balance != 10 {
		t.Errorf("冲突请求改动了余额：%d", acc.Balance)
	}
}

func TestCancelTossConcurrentCancelByOtherRequestIsDuplicated(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedActiveToss(t, db, 8, 2, time.Minute)
	// 并发方在快检之后把这条记录取消了（用的是别的 request_id）：
	// 本事务只许认输回滚，不许再退一次币。
	now := model.NowUnix()
	db.seedConcurrentToss(&model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 2,
		State: model.TossStateCancelled, FirstTossedAt: now - 60, LastTossedAt: now - 60,
		CancelledAt: now - 1, LastTossDate: model.TodayDayNo()})

	reply, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	if err != nil {
		t.Fatalf("并发取消要被翻译成 duplicated：%v", err)
	}
	if !reply.Duplicated || reply.FlowId != 0 {
		t.Fatalf("期待 duplicated=true 且无本次流水，得到 %+v", reply)
	}
	db.wantNoCall(t, "Accounts.RefundForCancelTx", "Flows.InsertTx")
	if acc := db.account(t, tossMid); acc.Balance != 8 {
		t.Errorf("并发取消导致二次退币：%d", acc.Balance)
	}
	if db.txRollbacks != 1 {
		t.Errorf("期待事务回滚，实际 %d", db.txRollbacks)
	}
	db.wantLedgerParity(t, tossMid)
}

func TestCancelTossWindowRecheckedOnLockedRow(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedActiveToss(t, db, 8, 2, time.Minute)
	// 快检时还在窗口内，锁到行时它已是 90000 秒前的投币（数据竞态/时钟回拨）：
	// 必须以锁定行复核的结论拒绝。
	now := model.NowUnix()
	db.seedConcurrentToss(&model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 2,
		State: model.TossStateActive, FirstTossedAt: now - 90000, LastTossedAt: now - 90000,
		LastTossDate: model.DayNo(time.Unix(now-90000, 0))})

	reply, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	if err != nil {
		t.Fatalf("超窗是结论：%v", err)
	}
	if reply.Cancelled || reply.Reason != rpc.TossRejectReason_TOSS_REJECT_CANCEL_WINDOW_EXPIRED {
		t.Fatalf("期待锁定行复核后的超窗结论，得到 %+v", reply)
	}
	wantDetail(t, reply.RejectDetail, "锁定行复核", "86400 秒")
	db.wantNoCall(t, "Accounts.RefundForCancelTx", "Flows.InsertTx")
	if acc := db.account(t, tossMid); acc.Balance != 8 {
		t.Errorf("复核超窗仍退了币：%d", acc.Balance)
	}
	if db.txRollbacks != 1 {
		t.Errorf("复核超窗必须回滚，实际 %d", db.txRollbacks)
	}
}

func TestCancelTossRecordVanishedInsideTransaction(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedActiveToss(t, db, 8, 2, time.Minute)
	db.hideTossInTx = true // 快检读到行，锁行时没了（被别的写入路径删掉）

	reply, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	if err != nil {
		t.Fatalf("记录消失是结论：%v", err)
	}
	if reply.Cancelled || reply.Reason != rpc.TossRejectReason_TOSS_REJECT_TARGET_INVALID {
		t.Fatalf("期待 TARGET_INVALID，得到 %+v", reply)
	}
	db.wantNoCall(t, "Accounts.RefundForCancelTx", "Flows.InsertTx")
	db.wantLedgerParity(t, tossMid)
}

func TestCancelTossRollsBackOnlyLastTossDateAndClampsAtZero(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	yesterday := model.DayNo(clockAt(0).AddDate(0, 0, -1))
	today := model.TodayDayNo()
	seedAccountWithLedger(db, tossMid, 8)
	seedToss(db, &model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 2,
		State: model.TossStateActive, FirstTossedAt: model.NowUnix() - 60,
		LastTossedAt: model.NowUnix() - 60, LastRequestID: "req-origin-toss", LastTossDate: yesterday})
	seedDaily(db, tossMid, yesterday, 1) // 昨日桶只有 1 枚，回退 2 枚必须夹到 0 而不是 -1
	seedDaily(db, tossMid, today, 5)     // 本次投的是昨天，今日桶一枚不许动

	reply, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	if err != nil {
		t.Fatalf("跨日取消失败：%v", err)
	}
	if !reply.Cancelled {
		t.Fatalf("期待取消成功：%+v", reply)
	}
	if d := db.daily[dailyKey{tossMid, yesterday}]; d == nil || d.Tossed != 0 {
		t.Errorf("回退没有夹底到 0：%+v", d)
	}
	if d := db.daily[dailyKey{tossMid, today}]; d == nil || d.Tossed != 5 {
		t.Errorf("只该回退 last_toss_date 那一个日桶，今日桶被改动了：%+v", d)
	}
	if n := db.countCall("Daily.RollbackTx"); n != 1 {
		t.Errorf("只该回退一个日桶，RollbackTx 调了 %d 次", n)
	}
	db.wantLedgerParity(t, tossMid)
}

func TestCancelTossOnBehalfOfUserRequiresReasonAndRecordsIt(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedActiveToss(t, db, 8, 2, time.Minute)

	reply, err := NewCancelTossLogic(context.Background(), sc).
		CancelToss(cancelReq(func(in *rpc.CancelTossReq) {
			in.Operator = "ops-1001"
			in.Reason = "用户误操作申请撤币"
		}))
	if err != nil {
		t.Fatalf("代客取消失败：%v", err)
	}
	if !reply.Cancelled {
		t.Fatalf("期待取消成功：%+v", reply)
	}
	flow := db.flowByRequestID(t, "req-cancel-1")
	if flow.Operator != "ops-1001" {
		t.Errorf("流水必须记下实际操作者，得到 %q", flow.Operator)
	}
	wantDetail(t, flow.Remark, "aid=1001", "退回 2 枚", "用户误操作申请撤币")
}

func TestCancelTossAccountEchoReadFailurePropagates(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedActiveToss(t, db, 8, 2, time.Minute)
	db.Fail("Accounts.FindOne", errBoom) // 只在提交后的回显阶段失败（事务内走 LockForUpdateTx）

	_, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	wantErrIs(t, err, errBoom)
	// 退款本身已经提交成功（幂等键已落），只是不返回半截响应冒充「取消没生效」。
	if acc := db.account(t, tossMid); acc.Balance != 10 {
		t.Errorf("落库余额应为 10，实际 %d", acc.Balance)
	}
	db.wantLedgerParity(t, tossMid)
}

func TestCancelTossConclusionReadFailurePropagates(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedActiveToss(t, db, 8, 2, time.Minute)
	db.Fail("Daily.FindOne", errBoom) // 取消成功，但组装响应时读日桶失败

	_, err := NewCancelTossLogic(context.Background(), sc).CancelToss(cancelReq(nil))
	wantErrIs(t, err, errBoom)
	if acc := db.account(t, tossMid); acc.Balance != 10 {
		t.Errorf("取消已提交，落库余额应为 10，实际 %d", acc.Balance)
	}
}
