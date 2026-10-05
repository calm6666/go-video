package logic

import (
	"context"
	"strings"
	"testing"

	"go-video/services/coin/internal/config"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"
)

// 投币（TossCoin）写口的口径证明：
// README §3 的事务锁序、四类拒绝都是「业务结论」而不是 gRPC 错误、request_id 幂等与冲突、
// 余额只能被 DeductForTossTx 改动、余额恒等于流水之和（对账不变式）。

const (
	tossMid = int64(42)
	tossAid = int64(1001)
)

func tossReq(mutate func(*rpc.TossCoinReq)) *rpc.TossCoinReq {
	in := &rpc.TossCoinReq{
		Mid:       tossMid,
		TargetAid: tossAid,
		Count:     1,
		RequestId: "req-toss-1",
		Platform:  rpc.Platform_PLATFORM_ANDROID,
	}
	if mutate != nil {
		mutate(in)
	}
	return in
}

func wantDetail(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("文案缺少 %q：%s", w, got)
		}
	}
}

func TestTossCoinRejectsMalformedInputWithoutTouchingDB(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*rpc.TossCoinReq)
		wantErr error
	}{
		{"mid 非正数", func(in *rpc.TossCoinReq) { in.Mid = 0 }, model.ErrInvalidMid},
		{"mid 为负", func(in *rpc.TossCoinReq) { in.Mid = -7 }, model.ErrInvalidMid},
		{"缺 request_id", func(in *rpc.TossCoinReq) { in.RequestId = "" }, model.ErrRequestIDRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			_, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(tc.mutate))
			wantErrIs(t, err, tc.wantErr)
			if db.txRuns != 0 || len(db.calls) != 0 {
				t.Fatalf("入参校验不该触达数据库：txRuns=%d calls=%v", db.txRuns, db.calls)
			}
		})
	}
}

func TestTossCoinTargetAidNonPositiveIsConclusionNotError(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 5)

	reply, err := NewTossCoinLogic(context.Background(), sc).
		TossCoin(tossReq(func(in *rpc.TossCoinReq) { in.TargetAid = 0 }))
	if err != nil {
		t.Fatalf("aid<=0 是业务结论，不该返回错误：%v", err)
	}
	if reply.Accepted || reply.Reason != rpc.TossRejectReason_TOSS_REJECT_TARGET_INVALID {
		t.Fatalf("期待 TARGET_INVALID 结论，得到 accepted=%v reason=%v", reply.Accepted, reply.Reason)
	}
	wantDetail(t, reply.RejectDetail, "aid 必须为正整数")
	if db.txRuns != 0 {
		t.Errorf("无效目标不该开事务：%d 次", db.txRuns)
	}
	db.wantNoCall(t, "Accounts.DeductForTossTx", "Flows.InsertTx")
	if acc := db.account(t, tossMid); acc.Balance != 5 {
		t.Errorf("余额被动过：%d", acc.Balance)
	}
}

func TestTossCoinNormalizesNonPositiveCountToOne(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int32
	}{{"count=0", 0}, {"count 为负", -3}} {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedAccountWithLedger(db, tossMid, 5)

			reply, err := NewTossCoinLogic(context.Background(), sc).
				TossCoin(tossReq(func(in *rpc.TossCoinReq) { in.Count = tc.count }))
			if err != nil {
				t.Fatalf("count<=0 按 1 处理，不该报错：%v", err)
			}
			if !reply.Accepted {
				t.Fatalf("期待受理，得到 reason=%v detail=%s", reply.Reason, reply.RejectDetail)
			}
			flow := db.flowByRequestID(t, "req-toss-1")
			if flow.Delta != -1 {
				t.Errorf("首次扣减应为 1 枚，实际 delta=%d", flow.Delta)
			}
			if got := db.account(t, tossMid).Balance; got != 4 {
				t.Errorf("余额应为 4，实际 %d", got)
			}
			db.wantLedgerParity(t, tossMid)
		})
	}
}

func TestTossCoinSingleRequestOverPerTargetLimitSkipsTransaction(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)

	reply, err := NewTossCoinLogic(context.Background(), sc).
		TossCoin(tossReq(func(in *rpc.TossCoinReq) { in.Count = 3 })) // PerTargetLimit=2
	if err != nil {
		t.Fatalf("超单片上限是结论：%v", err)
	}
	if reply.Reason != rpc.TossRejectReason_TOSS_REJECT_TARGET_LIMIT {
		t.Fatalf("期待 TARGET_LIMIT，得到 %v", reply.Reason)
	}
	wantDetail(t, reply.RejectDetail, "累计最多投 2 枚", "本次请求 3 枚")
	if db.txRuns != 0 {
		t.Errorf("结论恒为拒绝时不该开事务（README §3），实际 %d 次", db.txRuns)
	}
	db.wantNoCall(t, "Accounts.DeductForTossTx", "Daily.AccumulateTx", "Flows.InsertTx")
	if got := db.account(t, tossMid).Balance; got != 50 {
		t.Errorf("余额被预检阶段的拒绝改动了：%d", got)
	}
}

func TestTossCoinFirstTossWritesAccountTossAndFlowInOneTransaction(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	day := model.TodayDayNo()

	reply, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	if err != nil {
		t.Fatalf("投币失败：%v", err)
	}
	if !reply.Accepted || reply.Duplicated || reply.Reason != rpc.TossRejectReason_TOSS_ACCEPTED {
		t.Fatalf("期待首次受理，得到 %+v", reply)
	}
	if reply.FlowId == 0 || reply.Toss == nil || reply.Toss.TossId == 0 {
		t.Fatalf("受理后必须回显 flow_id 与投币记录，得到 %+v", reply)
	}

	// 建仓初始币 + 本次投币，两条流水同事务提交。
	init := db.flowByRequestID(t, model.InitialGrantRequestID(tossMid))
	if init.FlowType != model.FlowTypeAdminGrant || init.BizNo != model.InitialGrantBizNo || init.Delta != 5 {
		t.Errorf("建仓初始币流水不符：%s", db.flowDump(tossMid))
	}
	if init.BalanceAfter != 5 {
		t.Errorf("建仓流水的 balance_after 应为初始币 5，实际 %d", init.BalanceAfter)
	}
	// 本笔投币流水的 balance_after 必须等于落库余额（初始币只算一次）。
	flow := db.wantFlowBalanceAfter(t, "req-toss-1", 4)
	if flow.FlowType != model.FlowTypeToss || flow.Delta != -1 || flow.TargetAid != tossAid ||
		flow.Operator != "user" {
		t.Errorf("投币流水不符： %+v", flow)
	}
	acc := db.account(t, tossMid)
	if acc.Balance != 4 || acc.TotalTossed != 1 {
		t.Errorf("余额/历史投币不符：balance=%d total_tossed=%d", acc.Balance, acc.TotalTossed)
	}
	row := db.toss(t, tossMid, tossAid)
	if row.State != model.TossStateActive || row.Count != 1 || row.LastTossDate != day ||
		row.LastRequestID != "req-toss-1" || row.Platform != int32(rpc.Platform_PLATFORM_ANDROID) {
		t.Errorf("投币记录不符：%+v", row)
	}
	if d := db.daily[dailyKey{tossMid, day}]; d == nil || d.Tossed != 1 {
		t.Fatalf("日额度没记下本次投币：%+v", d)
	}
	if reply.Account.TodayTossed != 1 || reply.Account.TodayLimit != 10 {
		t.Errorf("回显的今日额度不符：%+v", reply.Account)
	}
	db.wantLedgerParity(t, tossMid)
	// 数据所有权（AGENTS.md §5）：余额只经由本服务的投币扣减入口变动。
	db.wantBalanceWritesVia(t, "DeductForTossTx")
	if db.txRuns != 1 || db.txCommits != 1 || db.txRollbacks != 0 {
		t.Fatalf("应只有一次提交的事务：%d/%d/%d", db.txRuns, db.txCommits, db.txRollbacks)
	}
	// README §3 锁序：账户行锁 → 幂等复核 → 投币行锁 → 日额度 → 扣减 → 投币落库 → 台账。
	db.wantCallOrder(t,
		"Accounts.EnsureTx", "Accounts.LockForUpdateTx", "Flows.FindByRequestID",
		"Tosses.LockByTargetTx", "Daily.EnsureTx", "Daily.AccumulateTx",
		"Accounts.DeductForTossTx", "Tosses.InsertTx", "Flows.InsertTx")
}

func TestTossCoinInsufficientBalanceRollsBackEverything(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 1) // 余额 1，本次要投 2
	fixedClock(t, clockAt(0))

	reply, err := NewTossCoinLogic(context.Background(), sc).
		TossCoin(tossReq(func(in *rpc.TossCoinReq) { in.Count = 2 }))
	if err != nil {
		t.Fatalf("余额不足是结论不是错误：%v", err)
	}
	if reply.Accepted || reply.Reason != rpc.TossRejectReason_TOSS_REJECT_INSUFFICIENT_BALANCE {
		t.Fatalf("期待 INSUFFICIENT_BALANCE，得到 %+v", reply)
	}
	wantDetail(t, reply.RejectDetail, "当前余额 1 枚", "本次需要 2 枚")
	if got := db.account(t, tossMid).Balance; got != 1 {
		t.Errorf("余额被扣了：%d", got)
	}
	if len(db.flows) != 1 {
		t.Errorf("失败的投币不该留台账（只剩播种那条）：%s", db.flowDump(tossMid))
	}
	if len(db.tosses) != 0 {
		t.Errorf("失败的投币不该留投币记录：%v", db.tosses)
	}
	// 事务内已累加过的日额度必须随回滚一起消失，否则「扣币失败但额度照扣」。
	if d := db.daily[dailyKey{tossMid, model.TodayDayNo()}]; d != nil && d.Tossed != 0 {
		t.Errorf("回滚后日额度仍被占用：tossed=%d", d.Tossed)
	}
	db.wantNoCall(t, "Flows.InsertTx")
}

func TestTossCoinHoldsMinBalanceThreshold(t *testing.T) {
	// MinBalanceToToss=3 时，余额 2 投 1 枚必须被条件扣减挡住（门槛取 max(count, MinBalanceToToss)）。
	sc, db := newTestSvcWith(t, func(c *config.CoinConf) { c.MinBalanceToToss = 3 })
	seedAccountWithLedger(db, tossMid, 2)
	fixedClock(t, clockAt(0))

	reply, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	if err != nil {
		t.Fatalf("余额门槛是结论：%v", err)
	}
	if reply.Accepted || reply.Reason != rpc.TossRejectReason_TOSS_REJECT_INSUFFICIENT_BALANCE {
		t.Fatalf("期待 INSUFFICIENT_BALANCE，得到 %+v", reply)
	}
	if got := db.account(t, tossMid).Balance; got != 2 {
		t.Errorf("门槛挡住后余额仍被改动：%d", got)
	}
	db.wantNoCall(t, "Flows.InsertTx")
}

func TestTossCoinDailyLimitExceededReportsRemainingQuota(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	seedDaily(db, tossMid, model.TodayDayNo(), 9) // 上限 10，本次要 2

	reply, err := NewTossCoinLogic(context.Background(), sc).
		TossCoin(tossReq(func(in *rpc.TossCoinReq) { in.Count = 2 }))
	if err != nil {
		t.Fatalf("日额度不足是结论：%v", err)
	}
	if reply.Accepted || reply.Reason != rpc.TossRejectReason_TOSS_REJECT_DAILY_LIMIT {
		t.Fatalf("期待 DAILY_LIMIT，得到 %+v", reply)
	}
	wantDetail(t, reply.RejectDetail, "今日已投 9 枚", "上限 10 枚", "还可投 1 枚", "本次需要 2 枚")
	if got := db.account(t, tossMid).Balance; got != 50 {
		t.Errorf("余额被改动了：%d", got)
	}
	db.wantNoCall(t, "Accounts.DeductForTossTx", "Flows.InsertTx")
	if len(db.tosses) != 0 {
		t.Errorf("额度不足不该留下投币记录：%v", db.tosses)
	}
}

func TestTossCoinDailyLimitReadFailureDegradesDetailOnly(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	seedDaily(db, tossMid, model.TodayDayNo(), 10)
	// 判定已由事务内条件累加给出；组装文案的那次回读失败时退化，不影响结论本身。
	// （随后的账户回显仍要求读到真值，所以只注入第一次读，见 rejectWithAccount。）
	db.FailOnce("Daily.FindOne", errBoom)

	reply, err := NewTossCoinLogic(context.Background(), sc).
		TossCoin(tossReq(func(in *rpc.TossCoinReq) { in.Count = 1 }))
	if err != nil {
		t.Fatalf("文案回读失败不该让结论变错误：%v", err)
	}
	if reply.Reason != rpc.TossRejectReason_TOSS_REJECT_DAILY_LIMIT {
		t.Fatalf("期待 DAILY_LIMIT，得到 %v", reply.Reason)
	}
	wantDetail(t, reply.RejectDetail, "今日投币额度不足")
	if reply.Account.TodayLimit != 10 {
		t.Errorf("限额回显必须照抄生效配置，得到 %d", reply.Account.TodayLimit)
	}
}

func TestTossCoinCrossDayResetsDailyQuota(t *testing.T) {
	// 日桶口径（README §3）：昨天投满不代表今天不能投。
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	yesterday := model.DayNo(clockAt(0).AddDate(0, 0, -1))
	seedDaily(db, tossMid, yesterday, 10) // 昨天的桶已满

	reply, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	if err != nil {
		t.Fatalf("跨日投币失败：%v", err)
	}
	if !reply.Accepted {
		t.Fatalf("今天没有日额度行应当=0 可用，得到 %+v", reply)
	}
	if d := db.daily[dailyKey{tossMid, yesterday}]; d.Tossed != 10 {
		t.Errorf("昨天的计数被今天的路径改动了：%d", d.Tossed)
	}
	if d := db.daily[dailyKey{tossMid, model.TodayDayNo()}]; d == nil || d.Tossed != 1 {
		t.Errorf("今天的桶不符：%+v", d)
	}
}

func TestTossCoinAccumulatesOnActiveRow(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	now := model.NowUnix()
	seedToss(db, &model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 1,
		State: model.TossStateActive, FirstTossedAt: now - 60, LastTossedAt: now - 60})

	reply, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	if err != nil {
		t.Fatalf("累计投币失败：%v", err)
	}
	if !reply.Accepted {
		t.Fatalf("期待受理：%+v", reply)
	}
	row := db.toss(t, tossMid, tossAid)
	if row.Count != 2 {
		t.Errorf("单片累计应为 2 枚，实际 %d", row.Count)
	}
	if row.FirstTossedAt != now-60 {
		t.Errorf("首次投币时刻不该被回拨：%d", row.FirstTossedAt)
	}
	db.wantNoCall(t, "Tosses.InsertTx") // 走累加分支，不再插新行（唯一键也会撞）
	db.wantLedgerParity(t, tossMid)
	wantDetail(t, reply.RejectDetail, "本次投出 1 枚", "对该稿件累计已投 2 枚")
}

func TestTossCoinPerTargetAccumulateLimitRollsBack(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	now := model.NowUnix()
	seedToss(db, &model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 2, // 已达上限
		State: model.TossStateActive, FirstTossedAt: now - 60, LastTossedAt: now - 60})

	reply, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	if err != nil {
		t.Fatalf("超单片上限是结论：%v", err)
	}
	if reply.Accepted || reply.Reason != rpc.TossRejectReason_TOSS_REJECT_TARGET_LIMIT {
		t.Fatalf("期待 TARGET_LIMIT，得到 %+v", reply)
	}
	acc := db.account(t, tossMid)
	if acc.Balance != 50 || acc.TotalTossed != 0 {
		t.Errorf("条件累加失败后账户仍被改动：%+v", acc)
	}
	if d := db.daily[dailyKey{tossMid, model.TodayDayNo()}]; d != nil && d.Tossed != 0 {
		t.Errorf("回滚后日额度仍被占用：tossed=%d", d.Tossed)
	}
	db.wantNoCall(t, "Flows.InsertTx")
}

func TestTossCoinOnCancelledRowOverridesCountInsteadOfAdding(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	now := model.NowUnix()
	seedToss(db, &model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 2,
		State: model.TossStateCancelled, FirstTossedAt: now - 600, LastTossedAt: now - 600,
		CancelledAt: now - 300})

	reply, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	if err != nil {
		t.Fatalf("取消后重新投币应允许：%v", err)
	}
	if !reply.Accepted {
		t.Fatalf("期待受理：%+v", reply)
	}
	row := db.toss(t, tossMid, tossAid)
	if row.State != model.TossStateActive || row.Count != 1 || row.CancelledAt != 0 {
		t.Fatalf("复活后的记录不符：%+v", row)
	}
	db.wantNoCall(t, "Tosses.InsertTx") // 同一唯一键上复活，不能插第二行
	db.wantLedgerParity(t, tossMid)
}

func TestTossCoinUnknownStateInDBIsSurfacedNotIgnored(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	// 枚举外的 state 只能是被绕过本服务写过的数据，必须拒绝而不是当成 ACTIVE 累加。
	seedToss(db, &model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 1, State: 7,
		FirstTossedAt: model.NowUnix(), LastTossedAt: model.NowUnix()})

	_, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	wantErrIs(t, err, model.ErrConcurrentUpdate)
	db.wantNoCall(t, "Flows.InsertTx")
	if got := db.account(t, tossMid).Balance; got != 50 {
		t.Errorf("异常状态下仍扣了币：%d", got)
	}
}

func TestTossCoinReplayReturnsFirstConclusionWithoutSecondDeduct(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	now := model.NowUnix()
	first := seedFlow(db, &model.Flow{Mid: tossMid, FlowType: model.FlowTypeToss, Delta: -1,
		BalanceAfter: 49, TargetAid: tossAid, Operator: "user", RequestID: "req-toss-1"})
	seedToss(db, &model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 1,
		State: model.TossStateActive, FirstTossedAt: now, LastTossedAt: now, LastRequestID: "req-toss-1"})

	reply, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	if err != nil {
		t.Fatalf("重放不该失败：%v", err)
	}
	if !reply.Accepted || !reply.Duplicated {
		t.Fatalf("期待 duplicated=true 的首次结论，得到 %+v", reply)
	}
	if reply.FlowId != first.ID {
		t.Errorf("重放必须回首次的 flow_id=%d，得到 %d", first.ID, reply.FlowId)
	}
	wantDetail(t, reply.RejectDetail, "重复请求", "首次扣减 1 枚")
	db.wantNoCall(t, "Flows.InsertTx", "Accounts.DeductForTossTx", "Daily.AccumulateTx")
	if db.txRuns != 0 {
		t.Errorf("快路径命中重放时不该开事务：%d 次", db.txRuns)
	}
	if got := db.account(t, tossMid).Balance; got != 50 {
		t.Errorf("重放把余额二次扣减了：%d", got)
	}
	if row := db.toss(t, tossMid, tossAid); row.Count != 1 {
		t.Errorf("重放二次累加了投币记录：count=%d", row.Count)
	}
}

func TestTossCoinReplayWithDifferentParametersIsConflict(t *testing.T) {
	cases := []struct {
		name   string
		stored *model.Flow
		mutate func(*rpc.TossCoinReq)
	}{
		{"换稿件", nil, func(in *rpc.TossCoinReq) { in.TargetAid = 2002 }},
		{"换枚数", nil, func(in *rpc.TossCoinReq) { in.Count = 2 }},
		{"换用户", nil, func(in *rpc.TossCoinReq) { in.Mid = 43 }},
		{"键被取消流水占用", &model.Flow{FlowType: model.FlowTypeCancelToss, Delta: 1}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedAccountWithLedger(db, tossMid, 50)
			stored := &model.Flow{Mid: tossMid, FlowType: model.FlowTypeToss, Delta: -1,
				BalanceAfter: 49, TargetAid: tossAid, Operator: "user", RequestID: "req-toss-1"}
			if tc.stored != nil {
				stored.FlowType = tc.stored.FlowType
				stored.Delta = tc.stored.Delta
			}
			seedFlow(db, stored)

			_, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(tc.mutate))
			wantErrIs(t, err, model.ErrIdempotencyConflict)
			wantErrContains(t, err, "req-toss-1")
			db.wantNoCall(t, "Flows.InsertTx", "Accounts.DeductForTossTx")
		})
	}
}

func TestTossCoinCountZeroReplayMatchesNormalizedDelta(t *testing.T) {
	// 首次以 count=0 落库（delta=-1），重放也必须按归一化后的 1 枚比较，否则误判冲突。
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	seedFlow(db, &model.Flow{Mid: tossMid, FlowType: model.FlowTypeToss, Delta: -1,
		BalanceAfter: 49, TargetAid: tossAid, Operator: "user", RequestID: "req-toss-1"})

	reply, err := NewTossCoinLogic(context.Background(), sc).
		TossCoin(tossReq(func(in *rpc.TossCoinReq) { in.Count = 0 }))
	if err != nil {
		t.Fatalf("归一化后的重放不该被判冲突：%v", err)
	}
	if !reply.Duplicated {
		t.Fatalf("期待 duplicated=true，得到 %+v", reply)
	}
}

func TestTossCoinConcurrentReplayBecomesDuplicated(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	db.seedConcurrentFlow(&model.Flow{Mid: tossMid, FlowType: model.FlowTypeToss, Delta: -1,
		BalanceAfter: 49, TargetAid: tossAid, Operator: "user", RequestID: "req-toss-1"})

	reply, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	if err != nil {
		t.Fatalf("并发重放要被翻译成 duplicated 结论：%v", err)
	}
	if !reply.Accepted || !reply.Duplicated {
		t.Fatalf("期待 accepted+duplicated，得到 %+v", reply)
	}
	if db.txRollbacks != 1 {
		t.Errorf("期待本次事务回滚一次，实际 %d", db.txRollbacks)
	}
	if got := db.account(t, tossMid).Balance; got != 50 {
		t.Errorf("并发重放导致二次扣币：余额 %d", got)
	}
	// 账户已存在（created=false），本次事务在幂等复核处就退出，一条台账都不该写。
	db.wantNoCall(t, "Flows.InsertTx", "Accounts.DeductForTossTx")
	// 只剩播种那条 +1 并发方那条 -1：SUM(delta)=49 就是本次回滚后落库的余额口径。
	if got := db.sumDelta(tossMid); got != 49 {
		t.Errorf("并发方的流水没被采纳，SUM(delta)=%d", got)
	}
	if len(db.flows) != 2 {
		t.Errorf("台账条数不符（应为播种 1 + 并发方 1）：%s", db.flowDump(tossMid))
	}
}

func TestTossCoinVanishedRequestIDAfterRollbackIsErrorNotSuccess(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	db.keepConcurrentHidden = true
	db.seedConcurrentFlow(&model.Flow{Mid: tossMid, FlowType: model.FlowTypeToss, Delta: -1,
		BalanceAfter: 49, TargetAid: tossAid, Operator: "user", RequestID: "req-toss-1"})

	_, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	wantErrIs(t, err, model.ErrConcurrentUpdate) // 宁可让调用方重试，也不能当成成功
	wantErrContains(t, err, "vanished")
	if got := db.account(t, tossMid).Balance; got != 50 {
		t.Errorf("键消失时仍动了余额：%d", got)
	}
}

func TestTossCoinIdempotencyReadFailurePropagates(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	db.Fail("Flows.FindByRequestID", errBoom)

	_, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	wantErrIs(t, err, errBoom) // 折叠成「没有重放」就会二次扣币
	db.wantNoCall(t, "Accounts.DeductForTossTx")
}

func TestTossCoinAccountReadFailureOnRejectPropagates(t *testing.T) {
	sc, db := newTestSvc(t)
	seedAccountWithLedger(db, tossMid, 50)
	fixedClock(t, clockAt(0))
	seedToss(db, &model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 2,
		State: model.TossStateActive, FirstTossedAt: model.NowUnix(), LastTossedAt: model.NowUnix()})
	db.Fail("Accounts.FindOne", errBoom)

	_, err := NewTossCoinLogic(context.Background(), sc).
		TossCoin(tossReq(func(in *rpc.TossCoinReq) { in.TargetAid = 0 }))
	wantErrIs(t, err, errBoom) // 拒绝结论也要回显账户；读不到就报错，不拿「余额 0」冒充
}

func TestTossCoinCommittedButRereadFailureStillReportsAccepted(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	db.Fail("Accounts.FindOne", errBoom)

	reply, err := NewTossCoinLogic(context.Background(), sc).TossCoin(tossReq(nil))
	if err != nil {
		t.Fatalf("已经提交的投币不该被报成失败（客户端会当失败重试）：%v", err)
	}
	if !reply.Accepted || reply.FlowId == 0 {
		t.Fatalf("期待受理 + flow_id 回显，得到 %+v", reply)
	}
	if acc := db.account(t, tossMid); acc.Balance != 4 {
		t.Errorf("落库余额应为 4，实际 %d", acc.Balance)
	}
	db.wantLedgerParity(t, tossMid)
	// 已知取舍（README §3 注释）：此时账户回显退化为「余额 0 + 限额照抄」，
	// 因为 accountInfo(nil) 不凭空造数；真值仍在库里，客户端下一次读就对了。
	if reply.Account.Balance != 0 || reply.Account.TodayLimit != 10 {
		t.Errorf("回显退化不符预期：%+v", reply.Account)
	}
}
