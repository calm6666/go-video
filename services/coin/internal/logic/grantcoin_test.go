package logic

import (
	"context"
	"strings"
	"testing"

	"go-video/services/coin/model"
	"go-video/services/coin/rpc"
)

// 发放/扣回硬币（GrantCoin）的口径证明：
// 这是投币之外唯一的余额变动入口，所以「哪些流水类型能从这儿进来」就是数据所有权的边界；
// 另外证明 operator 恒必填、ADMIN_GRANT 要理由、ORDER_PACK 要订单号、
// 扣回不得把余额写成负数、以及 request_id 幂等。

const grantMid = int64(77)

func grantReq(mutate func(*rpc.GrantCoinReq)) *rpc.GrantCoinReq {
	in := &rpc.GrantCoinReq{
		Mid:       grantMid,
		RequestId: "req-grant-1",
		FlowType:  rpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK,
		Delta:     10,
		BizNo:     "ORDER-20260305-0001",
		Operator:  "trade-order",
	}
	if mutate != nil {
		mutate(in)
	}
	return in
}

// TestGrantCoinRejectsForgedFlowTypes 证明所有权边界：TOSS / CANCEL_TOSS 由投币链路自己写，
// 从发放口塞进来就是伪造投币记录（投币记录还能带 target_aid，等于凭空给用户加一次「投过币」）；
// EXPIRE 本项目没有过期能力，未开启即一律拒绝。
func TestGrantCoinRejectsForgedFlowTypes(t *testing.T) {
	cases := []struct {
		name string
		ft   rpc.CoinFlowType
	}{
		{"TOSS 不能由发放口写入", rpc.CoinFlowType_COIN_FLOW_TYPE_TOSS},
		{"CANCEL_TOSS 不能由发放口写入", rpc.CoinFlowType_COIN_FLOW_TYPE_CANCEL_TOSS},
		{"EXPIRE 未开启", rpc.CoinFlowType_COIN_FLOW_TYPE_EXPIRE},
		{"UNSPECIFIED", rpc.CoinFlowType_COIN_FLOW_TYPE_UNSPECIFIED},
		{"枚举外的值", rpc.CoinFlowType(99)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedAccountWithLedger(db, grantMid, 8)
			_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(
				grantReq(func(in *rpc.GrantCoinReq) { in.FlowType = tc.ft }))
			wantErrIs(t, err, model.ErrGrantTypeInvalid)
			db.wantNoCall(t, "Accounts.ApplyGrantTx", "Accounts.EnsureTx", "Flows.InsertTx")
			if db.txRuns != 0 {
				t.Errorf("类型门禁不该开事务：%d", db.txRuns)
			}
			if acc := db.account(t, grantMid); acc.Balance != 8 {
				t.Errorf("被拒的发放改动了余额：%d", acc.Balance)
			}
		})
	}
}

// TestGrantCoinRequiresCredential 证明「谁发起的、为什么发起、对应哪笔订单」三项凭据缺一不可。
func TestGrantCoinRequiresCredential(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*rpc.GrantCoinReq)
		wantErr error
	}{
		{"ORDER_PACK 缺 operator", func(in *rpc.GrantCoinReq) { in.Operator = "" }, model.ErrGrantOperatorRequired},
		{"ADMIN_GRANT 缺 operator", func(in *rpc.GrantCoinReq) {
			in.FlowType = rpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT
			in.Operator = ""
		}, model.ErrGrantOperatorRequired},
		{"ADMIN_GRANT 无理由", func(in *rpc.GrantCoinReq) {
			in.FlowType = rpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT
			in.BizNo = ""
			in.Reason = ""
			in.Operator = "ops-001"
		}, model.ErrGrantReasonRequired},
		{"ORDER_PACK 无订单号", func(in *rpc.GrantCoinReq) { in.BizNo = "" }, model.ErrGrantBizNoRequired},
		{"ORDER_PACK 写了理由也不算订单号", func(in *rpc.GrantCoinReq) {
			in.BizNo = ""
			in.Reason = "补发"
		}, model.ErrGrantBizNoRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedAccountWithLedger(db, grantMid, 8)
			_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(tc.mutate))
			wantErrIs(t, err, tc.wantErr)
			db.wantNoCall(t, "Accounts.ApplyGrantTx", "Flows.InsertTx")
			if db.txRuns != 0 {
				t.Errorf("凭据校验不该开事务：%d", db.txRuns)
			}
		})
	}
}

func TestGrantCoinAdminGrantWithoutBizNoIsAccepted(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 8)

	_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(func(in *rpc.GrantCoinReq) {
		in.FlowType = rpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT
		in.Operator = "ops-001"
		in.Reason = "活动补偿"
		in.BizNo = ""
	}))
	if err != nil {
		t.Fatalf("ADMIN_GRANT 不要求订单号：%v", err)
	}
	flow := db.flowByRequestID(t, "req-grant-1")
	if flow.BizNo != "" || flow.Operator != "ops-001" || flow.Remark != "活动补偿" {
		t.Errorf("运营发放留痕不符：%+v", flow)
	}
}

// TestGrantCoinDeltaBounds 证明 |delta| 上限挡住「运营少打一个 0」，且 0 枚不记账。
func TestGrantCoinDeltaBounds(t *testing.T) {
	maxDelta := testCoinConf().MaxGrantDelta
	cases := []struct {
		name    string
		delta   int64
		wantErr bool
	}{
		{"delta=0 无记账意义", 0, true},
		{"恰好等于上限", maxDelta, false},
		{"刚超上限", maxDelta + 1, true},
		{"负向恰好等于上限", -maxDelta, false},
		{"负向刚超上限", -maxDelta - 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedAccountWithLedger(db, grantMid, 5000)
			_, err := NewGrantCoinLogic(context.Background(), sc).
				GrantCoin(grantReq(func(in *rpc.GrantCoinReq) { in.Delta = tc.delta }))
			if tc.wantErr {
				wantErrIs(t, err, model.ErrGrantDeltaInvalid)
				db.wantNoCall(t, "Accounts.ApplyGrantTx", "Flows.InsertTx")
				if db.txRuns != 0 {
					t.Errorf("上限校验不该开事务：%d", db.txRuns)
				}
				return
			}
			if err != nil {
				t.Fatalf("上限内应放行：%v", err)
			}
			if n := db.countCall("Accounts.ApplyGrantTx"); n != 1 {
				t.Errorf("ApplyGrantTx 应调用一次，实际 %d", n)
			}
			db.wantLedgerParity(t, grantMid)
		})
	}
}

func TestGrantCoinAppliesInOneTransaction(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 8)

	reply, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(nil))
	if err != nil {
		t.Fatalf("发放失败：%v", err)
	}
	if reply.Duplicated || reply.FlowId == 0 {
		t.Fatalf("首次发放不该 duplicated：%+v", reply)
	}
	if reply.Account == nil || reply.Account.Balance != 18 {
		t.Fatalf("账户回显不符：%+v", reply.Account)
	}
	acc := db.account(t, grantMid)
	if acc.Balance != 18 {
		t.Errorf("落库余额 %d，期待 18", acc.Balance)
	}
	flow := db.wantFlowBalanceAfter(t, "req-grant-1", 18)
	if flow.FlowType != model.FlowTypeOrderPack || flow.Delta != 10 || flow.BizNo != "ORDER-20260305-0001" {
		t.Errorf("发放流水不符：%+v", flow)
	}
	if flow.TargetAid != 0 {
		t.Errorf("发放不该带 target_aid（那会伪装成投币）：%d", flow.TargetAid)
	}
	wantDetail(t, flow.Remark, "履约发放 10 枚", "ORDER-20260305-0001")
	db.wantLedgerParity(t, grantMid)
	db.wantBalanceWritesVia(t, "ApplyGrantTx")
	if db.txRuns != 1 || db.txCommits != 1 {
		t.Fatalf("应只有一次提交的事务：runs=%d commits=%d", db.txRuns, db.txCommits)
	}
	db.wantCallOrder(t, "Accounts.EnsureTx", "Accounts.LockForUpdateTx",
		"Flows.FindByRequestID", "Accounts.ApplyGrantTx", "Flows.InsertTx")
}

// TestGrantCoinLazyAccountKeepsInitialGrantFlow 证明建仓送币留得下流水：
// 少了这条，「余额 = SUM(流水 delta)」在第一个用户身上就破了，本次发放的 balance_after 也对不上历史。
func TestGrantCoinLazyAccountKeepsInitialGrantFlow(t *testing.T) {
	sc, db := newTestSvc(t)

	reply, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(func(in *rpc.GrantCoinReq) {
		in.Delta = 7
	}))
	if err != nil {
		t.Fatalf("新账户发放失败：%v", err)
	}
	initial := int64(5) // testCoinConf().InitialBalance
	want := initial + 7
	if reply.Account == nil || reply.Account.Balance != want {
		t.Fatalf("回显余额 %+v，期待 %d（初始币只能算一次）", reply.Account, want)
	}
	acc := db.account(t, grantMid)
	if acc.Balance != want {
		t.Fatalf("落库余额 %d，期待 %d", acc.Balance, want)
	}
	init := db.flowByRequestID(t, model.InitialGrantRequestID(grantMid))
	if init.Delta != initial || init.BalanceAfter != initial || init.BizNo != model.InitialGrantBizNo ||
		init.Operator != model.InitialGrantOperator || init.FlowType != model.FlowTypeAdminGrant {
		t.Errorf("建仓流水不符：%+v", init)
	}
	db.wantFlowBalanceAfter(t, "req-grant-1", want)
	db.wantLedgerParity(t, grantMid)
	if n := db.countCall("Flows.InsertTx"); n != 2 {
		t.Errorf("应有建仓流水 + 本次发放流水两条，实际 %d 次插入", n)
	}
}

// TestGrantCoinClawbackCannotGoNegative 现金币不分叉：扣回超过余额必须以错误挡住并整体回滚。
func TestGrantCoinClawbackCannotGoNegative(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 3)

	_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(func(in *rpc.GrantCoinReq) {
		in.Delta = -4
	}))
	wantErrIs(t, err, model.ErrGrantBalanceWouldGoNegative)
	wantErrContains(t, err, "当前余额 3 枚")
	if acc := db.account(t, grantMid); acc.Balance != 3 {
		t.Errorf("被拒的扣回改动了余额：%d", acc.Balance)
	}
	db.wantNoCall(t, "Flows.InsertTx")
	if db.txRollbacks != 1 {
		t.Errorf("期待事务回滚，实际 %d", db.txRollbacks)
	}
	db.wantLedgerParity(t, grantMid)
}

func TestGrantCoinClawbackToExactZero(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 4)

	reply, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(func(in *rpc.GrantCoinReq) {
		in.Delta = -4
	}))
	if err != nil {
		t.Fatalf("恰好扣平应允许：%v", err)
	}
	acc := db.account(t, grantMid)
	if acc.Balance != 0 || reply.Account.Balance != 0 {
		t.Fatalf("落库 %d 回显 %d，期待 0", acc.Balance, reply.Account.Balance)
	}
	db.wantFlowBalanceAfter(t, "req-grant-1", 0)
	db.wantLedgerParity(t, grantMid)
}

func TestGrantCoinReplayReturnsFirstConclusionWithoutSecondApply(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 8)
	first := seedFlow(db, &model.Flow{Mid: grantMid, FlowType: model.FlowTypeOrderPack, Delta: 10,
		BalanceAfter: 18, BizNo: "ORDER-20260305-0001", Operator: "trade-order", RequestID: "req-grant-1"})
	seedAccount(db, grantMid, 18) // 首次发放已入账

	reply, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(nil))
	if err != nil {
		t.Fatalf("重放不该失败：%v", err)
	}
	if !reply.Duplicated || reply.FlowId != first.ID {
		t.Fatalf("期待 duplicated=true + 首次 flow_id，得到 %+v", reply)
	}
	if reply.Account.Balance != 18 {
		t.Errorf("重放回显余额 %d，期待 18", reply.Account.Balance)
	}
	db.wantNoCall(t, "Accounts.ApplyGrantTx", "Flows.InsertTx", "Accounts.EnsureTx")
	if db.txRuns != 0 {
		t.Errorf("快路径命中重放不该开事务：%d", db.txRuns)
	}
	if acc := db.account(t, grantMid); acc.Balance != 18 {
		t.Errorf("重放又扣/加了一次：%d", acc.Balance)
	}
	db.wantLedgerParity(t, grantMid)
}

func TestGrantCoinReplayWithDifferentParamsIsConflict(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.GrantCoinReq)
	}{
		{"金额不同", func(in *rpc.GrantCoinReq) { in.Delta = 11 }},
		{"订单号不同", func(in *rpc.GrantCoinReq) { in.BizNo = "ORDER-OTHER" }},
		{"流水类型不同", func(in *rpc.GrantCoinReq) {
			in.FlowType = rpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT
			in.Reason = "换个理由"
		}},
		{"用户不同", func(in *rpc.GrantCoinReq) { in.Mid = grantMid + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedAccountWithLedger(db, grantMid, 18)
			seedFlow(db, &model.Flow{Mid: grantMid, FlowType: model.FlowTypeOrderPack, Delta: 10,
				BalanceAfter: 18, BizNo: "ORDER-20260305-0001", Operator: "trade-order", RequestID: "req-grant-1"})

			_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(tc.mutate))
			wantErrIs(t, err, model.ErrIdempotencyConflict)
			db.wantNoCall(t, "Accounts.ApplyGrantTx", "Flows.InsertTx")
		})
	}
}

// TestGrantCoinReplayAgainstTossFlowIsConflict 同 request_id 若已被投币链路用掉，
// 发放口不能「顺着写进去」——那是把一笔订单发放伪装成用户投币（或反之）。
func TestGrantCoinReplayAgainstTossFlowIsConflict(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccount(db, grantMid, 6)
	seedFlow(db, &model.Flow{Mid: grantMid, FlowType: model.FlowTypeAdminGrant, Delta: 8,
		BalanceAfter: 8, Operator: "seed", RequestID: "seed:grant:conflict"})
	seedFlow(db, &model.Flow{Mid: grantMid, FlowType: model.FlowTypeToss, Delta: -2,
		BalanceAfter: 6, TargetAid: 2002, RequestID: "req-grant-1"})

	_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(nil))
	wantErrIs(t, err, model.ErrIdempotencyConflict)
	wantErrContains(t, err, "aid=2002")
	db.wantNoCall(t, "Accounts.ApplyGrantTx", "Flows.InsertTx")
}

// TestGrantCoinReplayWithOverLongBizNoIsStillReplay 落库时 biz_no 收敛到列宽（VARCHAR(64)），
// 幂等复核必须比同一份口径，否则同一把 key 的合法重放会被误判成串号冲突。
func TestGrantCoinReplayWithOverLongBizNoIsStillReplay(t *testing.T) {
	sc, db := newTestSvc(t)
	long := "B" + strings.Repeat("0", maxIDLen) // 65 字节，落库后只剩前 64 字节
	seedAccount(db, grantMid, 18)
	first := seedFlow(db, &model.Flow{Mid: grantMid, FlowType: model.FlowTypeOrderPack, Delta: 10,
		BalanceAfter: 18, BizNo: clipID(long), Operator: "trade-order", RequestID: "req-grant-1"})

	reply, err := NewGrantCoinLogic(context.Background(), sc).
		GrantCoin(grantReq(func(in *rpc.GrantCoinReq) { in.BizNo = long }))
	if err != nil {
		t.Fatalf("合法重放不该被误判冲突：%v", err)
	}
	if !reply.Duplicated || reply.FlowId != first.ID {
		t.Fatalf("期待 duplicated=true，得到 %+v", reply)
	}
	db.wantNoCall(t, "Accounts.ApplyGrantTx", "Flows.InsertTx")
}

func TestGrantCoinConcurrentReplayBecomesDuplicated(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 8)
	// 并发方在本事务快检之后提交了同 request_id 的发放，并把余额改到了 18。
	db.seedConcurrentCommit(&model.Flow{Mid: grantMid, FlowType: model.FlowTypeOrderPack, Delta: 10,
		BalanceAfter: 18, BizNo: "ORDER-20260305-0001", Operator: "trade-order", RequestID: "req-grant-1"}, 18)

	reply, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(nil))
	if err != nil {
		t.Fatalf("并发重放要翻译成 duplicated：%v", err)
	}
	if !reply.Duplicated || reply.FlowId == 0 {
		t.Fatalf("期待 duplicated=true + 并发方的 flow_id，得到 %+v", reply)
	}
	// 幂等复核在扣减之前就退出：本事务一次余额都没碰过，只有并发方那一次生效。
	db.wantNoCall(t, "Accounts.ApplyGrantTx", "Flows.InsertTx")
	if acc := db.account(t, grantMid); acc.Balance != 18 {
		t.Errorf("落库余额 %d，期待并发方的 18（不得二次累加）", acc.Balance)
	}
	if db.txRollbacks != 1 {
		t.Errorf("期待事务回滚，实际 %d", db.txRollbacks)
	}
	db.wantLedgerParity(t, grantMid)
	if n := db.countCall("Flows.FindByRequestID"); n < 3 {
		t.Errorf("快检 + 事务内复核 + 回滚后回捞都该读幂等键，实际 %d 次", n)
	}
}

func TestGrantCoinVanishedRequestIDAfterRollbackIsError(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 8)
	db.seedConcurrentFlow(&model.Flow{Mid: grantMid, FlowType: model.FlowTypeOrderPack, Delta: 10,
		BalanceAfter: 18, BizNo: "ORDER-20260305-0001", Operator: "trade-order", RequestID: "req-grant-1"})
	db.keepConcurrentHidden = true // 并发方随后也回滚了：幂等键又不见了

	_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(nil))
	wantErrIs(t, err, model.ErrConcurrentUpdate)
	wantErrContains(t, err, "vanished")
	// 幂等键消失时既不承认成功、也不留下任何改动：调用方重试即可，不会双发。
	db.wantNoCall(t, "Accounts.ApplyGrantTx", "Flows.InsertTx")
	if acc := db.account(t, grantMid); acc.Balance != 8 {
		t.Errorf("幂等键消失时余额必须回到 8，实际 %d", acc.Balance)
	}
	if len(db.flows) != 1 {
		t.Errorf("并发方回滚后台账不该多出行：%s", db.flowDump(grantMid))
	}
	db.wantLedgerParity(t, grantMid)
}

func TestGrantCoinIdempotencyReadFailurePropagates(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 8)
	db.Fail("Flows.FindByRequestID", errBoom)

	_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(nil))
	wantErrIs(t, err, errBoom)
	db.wantNoCall(t, "Accounts.ApplyGrantTx", "Flows.InsertTx")
	if db.txRuns != 0 {
		t.Errorf("读幂等键失败时不该继续开事务：%d", db.txRuns)
	}
	db.wantLedgerParity(t, grantMid)
}

func TestGrantCoinLedgerWriteFailureRollsBackBalance(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 8)
	db.Fail("Flows.InsertTx", errBoom)

	_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(nil))
	wantErrIs(t, err, errBoom)
	if acc := db.account(t, grantMid); acc.Balance != 8 {
		t.Errorf("没有流水却改了余额：%d", acc.Balance)
	}
	db.wantBalanceWritesVia(t, "ApplyGrantTx")
	db.wantLedgerParity(t, grantMid)
}

// TestGrantCoinEchoUsesPostChangeSnapshot 提交后回读失败时，兜底回显必须是「改完之后」的余额：
// 给运营看一个改动前的数字，比报错更容易被当成「发放没生效」而重复操作。
func TestGrantCoinEchoUsesPostChangeSnapshot(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 8)
	db.Fail("Accounts.FindOne", errBoom)

	reply, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(nil))
	if err != nil {
		t.Fatalf("发放本身已提交，回显读失败不该整体失败：%v", err)
	}
	if reply.Account.Balance != 18 {
		t.Errorf("兜底回显余额 %d，期待 18（落库真值）", reply.Account.Balance)
	}
	if acc := db.account(t, grantMid); acc.Balance != 18 {
		t.Errorf("落库余额 %d", acc.Balance)
	}
	db.wantFlowBalanceAfter(t, "req-grant-1", 18)
	db.wantLedgerParity(t, grantMid)
}

// TestGrantCoinOverlongCredentialClippedNotRejected 超长的操作者/订单号收敛到列宽，
// 不能让一笔已判定成功的发放因为 VARCHAR(64) 被数据库打回。
func TestGrantCoinOverlongCredentialClippedNotRejected(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, grantMid, 8)

	_, err := NewGrantCoinLogic(context.Background(), sc).GrantCoin(grantReq(func(in *rpc.GrantCoinReq) {
		in.Operator = strings.Repeat("ops", 100)
		in.BizNo = strings.Repeat("B", 300)
	}))
	if err != nil {
		t.Fatalf("超长凭据应被收敛而不是失败：%v", err)
	}
	flow := db.flowByRequestID(t, "req-grant-1")
	if len(flow.Operator) != maxIDLen || len(flow.BizNo) != maxIDLen {
		t.Errorf("凭据未收敛到列宽：operator=%d biz_no=%d", len(flow.Operator), len(flow.BizNo))
	}
	if r := []rune(flow.Remark); len(r) != maxRemarkLen {
		t.Errorf("超长 remark 应被夹到列宽上限 %d，实际 %d 字符", maxRemarkLen, len(r))
	}
	db.wantLedgerParity(t, grantMid)
}
