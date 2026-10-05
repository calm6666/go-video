package logic

import (
	"context"
	"testing"
	"time"

	"go-video/services/coin/model"
	"go-video/services/coin/rpc"
)

// 读侧方法的口径证明。核心只有两条：
//  1. 读失败必须上抛，不能折叠成「余额 0 / 空台账 / 没投过币」——那是把故障伪装成业务事实；
//  2. 分页与批量裁剪的口径：超上限的 size 报错（不静默截断），超上限的 aids 裁剪（不让整屏 feed 失败）。

func TestGetCoinAccountRejectsBadMid(t *testing.T) {
	sc, db := newTestSvc(t)
	for _, mid := range []int64{0, -1} {
		_, err := NewGetCoinAccountLogic(context.Background(), sc).
			GetCoinAccount(&rpc.GetCoinAccountReq{Mid: mid})
		wantErrIs(t, err, model.ErrInvalidMid)
	}
	db.wantNoCall(t, "Accounts.FindOne", "Daily.FindOne")
}

// TestGetCoinAccountNeverWrites 「看一眼余额」不许顺手建仓：
// 否则每次遍历查询都会凭空产生一笔初始币发放流水，发行量失控。
func TestGetCoinAccountNeverWrites(t *testing.T) {
	sc, db := newTestSvc(t)
	reply, err := NewGetCoinAccountLogic(context.Background(), sc).
		GetCoinAccount(&rpc.GetCoinAccountReq{Mid: tossMid})
	if err != nil {
		t.Fatalf("查无账户不是错误：%v", err)
	}
	if reply.Found {
		t.Error("没有账户行却回 found=true")
	}
	if reply.Account.Balance != 0 || reply.Account.TotalTossed != 0 {
		t.Errorf("缺账户一律回 0，不得凭空给初始币：%+v", reply.Account)
	}
	if len(db.accounts) != 0 || len(db.flows) != 0 {
		t.Errorf("读路径写库了：accounts=%d flows=%d", len(db.accounts), len(db.flows))
	}
	db.wantNoCall(t, "Accounts.EnsureTx", "Flows.InsertTx", "Accounts.ApplyGrantTx")
	if db.txRuns != 0 {
		t.Errorf("读路径不该开事务：%d", db.txRuns)
	}
}

func TestGetCoinAccountProjectsBalanceAndTodayQuota(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedAccountWithLedger(db, tossMid, 6)
	seedDaily(db, tossMid, model.TodayDayNo(), 4)

	reply, err := NewGetCoinAccountLogic(context.Background(), sc).
		GetCoinAccount(&rpc.GetCoinAccountReq{Mid: tossMid})
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Found {
		t.Fatal("有账户行必须 found=true")
	}
	acc := reply.Account
	if acc.Mid != tossMid || acc.Balance != 6 || acc.TodayTossed != 4 {
		t.Errorf("投影不符：%+v", acc)
	}
	coin := sc.Coin()
	if acc.TodayLimit != coin.DailyLimit || acc.PerTargetLimit != coin.PerTargetLimit ||
		acc.CancelWindowSeconds != coin.CancelWindowSeconds {
		t.Errorf("限额未回显：%+v", acc)
	}
}

// TestGetCoinAccountReadFailurePropagates 折叠成 found=false 会让客户端显示「余额 0」，
// 用户以为币被扣光了 —— 比报错严重得多。
func TestGetCoinAccountReadFailurePropagates(t *testing.T) {
	cases := []struct {
		name string
		op   string
	}{
		{"账户读失败", "Accounts.FindOne"},
		{"日额度读失败", "Daily.FindOne"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedAccountWithLedger(db, tossMid, 6)
			db.Fail(tc.op, errBoom)
			reply, err := NewGetCoinAccountLogic(context.Background(), sc).
				GetCoinAccount(&rpc.GetCoinAccountReq{Mid: tossMid})
			wantErrIs(t, err, errBoom)
			if reply != nil {
				t.Errorf("读失败时不得返回半截响应：%+v", reply)
			}
		})
	}
}

func TestGetTossConfigEchoesEffectiveValues(t *testing.T) {
	sc, db := newTestSvc(t)
	sc.Config.Coin.DailyLimit = 33
	sc.Config.Coin.CancelWindowSeconds = 60
	reply, err := NewGetTossConfigLogic(context.Background(), sc).GetTossConfig(&rpc.GetTossConfigReq{})
	if err != nil {
		t.Fatal(err)
	}
	// 回显的必须是运行时生效值（客户端按它决定按钮与文案），不是写死的一套数字。
	if reply.DailyLimit != 33 || reply.CancelWindowSeconds != 60 ||
		reply.PerTargetLimit != sc.Coin().PerTargetLimit ||
		reply.MinBalanceToToss != sc.Coin().MinBalanceToToss ||
		reply.InitialBalance != sc.Coin().InitialBalance {
		t.Errorf("回显与生效配置漂移：%+v", reply)
	}
	db.wantNoCall(t, "Accounts.FindOne", "Tosses.FindOne")
	if db.txRuns != 0 {
		t.Errorf("读配置不该碰数据库：%d", db.txRuns)
	}
}

// seedTossRows 播 n 条投给不同内容的记录（state 指定），返回条数。
func seedTossRows(db *fakeDB, mid int64, n int, state int32) {
	now := model.NowUnix()
	for i := 1; i <= n; i++ {
		seedToss(db, &model.Toss{Mid: mid, TargetAid: int64(2000 + i), Count: int32(i), State: state,
			FirstTossedAt: now - int64(i)*10, LastTossedAt: now - int64(i)*10, LastTossDate: model.TodayDayNo()})
	}
}

func TestListMyTossesValidation(t *testing.T) {
	cases := []struct {
		name    string
		in      *rpc.ListMyTossesReq
		wantErr error
	}{
		{"mid 非正数", &rpc.ListMyTossesReq{Mid: 0}, model.ErrInvalidMid},
		{"状态枚举外", &rpc.ListMyTossesReq{Mid: 7, State: rpc.TossState(9)}, model.ErrInvalidStateFilter},
		{"size 超上限（不静默截断）", &rpc.ListMyTossesReq{Mid: 7, Size: 101}, model.ErrPageSizeTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedTossRows(db, 7, 3, model.TossStateActive)
			_, err := NewListMyTossesLogic(context.Background(), sc).ListMyTosses(tc.in)
			wantErrIs(t, err, tc.wantErr)
			db.wantNoCall(t, "Tosses.ListByMid", "Tosses.CountByMid")
		})
	}
}

func TestListMyTossesPaginationAndStateFilter(t *testing.T) {
	sc, db := newTestSvc(t)
	seedTossRows(db, 7, 5, model.TossStateActive)
	seedToss(db, &model.Toss{Mid: 7, TargetAid: 3001, Count: 1, State: model.TossStateCancelled,
		LastTossedAt: model.NowUnix() - 999, CancelledAt: model.NowUnix()})

	all, err := NewListMyTossesLogic(context.Background(), sc).
		ListMyTosses(&rpc.ListMyTossesReq{Mid: 7})
	if err != nil {
		t.Fatal(err)
	}
	// state=UNSPECIFIED 是「不限状态」：必须含已取消，客户端才能区分「投过又取消」和「没投过」。
	if all.Total != 6 || len(all.Tosses) != 6 {
		t.Fatalf("默认分页（size=20）应一次给全：total=%d len=%d", all.Total, len(all.Tosses))
	}
	if all.Size != int64(sc.Coin().DefaultPageSize) || all.Page != 1 {
		t.Errorf("size<=0 应回默认页宽：size=%d page=%d", all.Size, all.Page)
	}
	var cancelled int
	for _, row := range all.Tosses {
		if row.State == rpc.TossState_TOSS_STATE_CANCELLED {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Errorf("已取消记录未出现在「不限状态」列表里：%d", cancelled)
	}

	active, err := NewListMyTossesLogic(context.Background(), sc).
		ListMyTosses(&rpc.ListMyTossesReq{Mid: 7, State: rpc.TossState_TOSS_STATE_ACTIVE, Page: 2, Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if active.Total != 5 || active.Page != 2 || active.Size != 2 || len(active.Tosses) != 2 {
		t.Fatalf("生效态分页不符：total=%d page=%d size=%d len=%d", active.Total, active.Page, active.Size, len(active.Tosses))
	}
	for _, row := range active.Tosses {
		if row.State != rpc.TossState_TOSS_STATE_ACTIVE {
			t.Errorf("state 过滤漏了已取消记录：%+v", row)
		}
	}
	// 第 2 页必须是第 1 页之外的行（offset 真生效）。
	if active.Tosses[0].TossId == all.Tosses[0].TossId {
		t.Error("page=2 仍返回了第 1 页的首行")
	}

	// 翻过头是列表页常态：空数组 + 正确 total，而不是错误。
	tail, err := NewListMyTossesLogic(context.Background(), sc).
		ListMyTosses(&rpc.ListMyTossesReq{Mid: 7, Page: 99, Size: 2})
	if err != nil {
		t.Fatalf("越界页码不该报错：%v", err)
	}
	if tail.Tosses == nil || len(tail.Tosses) != 0 || tail.Total != 6 {
		t.Errorf("越界页码应回空数组：%#v total=%d", tail.Tosses, tail.Total)
	}
}

func TestListMyTossesReadFailurePropagates(t *testing.T) {
	cases := []string{"Tosses.ListByMid", "Tosses.CountByMid"}
	for _, op := range cases {
		t.Run(op, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedTossRows(db, 7, 2, model.TossStateActive)
			db.Fail(op, errBoom)
			_, err := NewListMyTossesLogic(context.Background(), sc).
				ListMyTosses(&rpc.ListMyTossesReq{Mid: 7})
			wantErrIs(t, err, errBoom)
		})
	}
}

func TestListTargetTossersOnlyActiveAndPaginates(t *testing.T) {
	sc, db := newTestSvc(t)
	for i := 1; i <= 4; i++ {
		seedToss(db, &model.Toss{Mid: int64(700 + i), TargetAid: tossAid, Count: int32(i),
			State: model.TossStateActive, LastTossedAt: int64(1000 + i)})
	}
	seedToss(db, &model.Toss{Mid: 999, TargetAid: tossAid, Count: 9,
		State: model.TossStateCancelled, LastTossedAt: 9999})

	reply, err := NewListTargetTossersLogic(context.Background(), sc).
		ListTargetTossers(&rpc.ListTargetTossersReq{TargetAid: tossAid, Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Total != 4 || len(reply.Tosses) != 3 {
		t.Fatalf("只该数生效投币：total=%d len=%d", reply.Total, len(reply.Tosses))
	}
	// ORDER BY last_tossed_at DESC：最新一条在最前，且不含已取消。
	if reply.Tosses[0].Mid != 704 || reply.Tosses[0].Count != 4 {
		t.Errorf("排序不符：%+v", reply.Tosses[0])
	}
	for _, row := range reply.Tosses {
		if row.State != rpc.TossState_TOSS_STATE_ACTIVE {
			t.Errorf("列出了已取消的投币人：%+v", row)
		}
		// 本服务不复制可变主资料（昵称/头像归 user-profile）。
		if row.Mid == 0 {
			t.Errorf("缺少 mid，调用方无法回查用户资料：%+v", row)
		}
	}

	if _, err := NewListTargetTossersLogic(context.Background(), sc).
		ListTargetTossers(&rpc.ListTargetTossersReq{TargetAid: 0}); err == nil {
		t.Error("target_aid<=0 必须报错")
	} else {
		wantErrIs(t, err, model.ErrInvalidTargetAid)
	}
	if _, err := NewListTargetTossersLogic(context.Background(), sc).
		ListTargetTossers(&rpc.ListTargetTossersReq{TargetAid: tossAid, Size: 100000}); err == nil {
		t.Error("size 超上限必须报错而不是静默截断")
	} else {
		wantErrIs(t, err, model.ErrPageSizeTooLarge)
	}
}

func TestGetTargetSummaryAggregatesActiveTosses(t *testing.T) {
	sc, db := newTestSvc(t)
	seedToss(db, &model.Toss{Mid: 1, TargetAid: tossAid, Count: 2, State: model.TossStateActive})
	seedToss(db, &model.Toss{Mid: 2, TargetAid: tossAid, Count: 3, State: model.TossStateActive})
	seedToss(db, &model.Toss{Mid: 3, TargetAid: tossAid, Count: 5, State: model.TossStateCancelled})

	reply, err := NewGetTargetSummaryLogic(context.Background(), sc).
		GetTargetSummary(&rpc.GetTargetSummaryReq{Aid: tossAid})
	if err != nil {
		t.Fatal(err)
	}
	s := reply.Summary
	if s == nil || s.Aid != tossAid || s.CoinCount != 5 || s.CoinUserCount != 2 {
		t.Fatalf("汇总应按 cn_toss 生效行聚合（已取消不计）：%+v", s)
	}
	if s.LikeCount != 0 {
		t.Errorf("点赞归 engagement，本服务恒回 0：%d", s.LikeCount)
	}
	db.wantNoCall(t, "Accounts.ApplyGrantTx", "Tosses.InsertTx")
}

func TestGetTargetSummaryEdgeCases(t *testing.T) {
	t.Run("aid 非正数", func(t *testing.T) {
		sc, _ := newTestSvc(t)
		_, err := NewGetTargetSummaryLogic(context.Background(), sc).
			GetTargetSummary(&rpc.GetTargetSummaryReq{Aid: 0})
		wantErrIs(t, err, model.ErrInvalidTargetAid)
	})
	t.Run("无投币回全 0 汇总而不是 nil", func(t *testing.T) {
		sc, _ := newTestSvc(t)
		reply, err := NewGetTargetSummaryLogic(context.Background(), sc).
			GetTargetSummary(&rpc.GetTargetSummaryReq{Aid: 4004})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Summary == nil || reply.Summary.CoinCount != 0 || reply.Summary.CoinUserCount != 0 {
			t.Errorf("详情页要的是「0 币」而不是「查不到」：%+v", reply.Summary)
		}
	})
	t.Run("聚合读失败必须上抛", func(t *testing.T) {
		sc, db := newTestSvc(t)
		seedToss(db, &model.Toss{Mid: 1, TargetAid: tossAid, Count: 2, State: model.TossStateActive})
		db.Fail("Tosses.SummarizeTargets", errBoom)
		_, err := NewGetTargetSummaryLogic(context.Background(), sc).
			GetTargetSummary(&rpc.GetTargetSummaryReq{Aid: tossAid})
		wantErrIs(t, err, errBoom)
	})
}

func TestBatchGetTargetSummaryTrimsInsteadOfFailing(t *testing.T) {
	sc, db := newTestSvc(t)
	maxAids := sc.Coin().MaxBatchAids
	seedToss(db, &model.Toss{Mid: 1, TargetAid: tossAid, Count: 2, State: model.TossStateActive})
	// 构造「上限 + 10 个有效 aid，外加重复与非法项」：只应裁掉超量部分，整个请求不能失败。
	aids := make([]int64, 0, maxAids+12)
	for i := int64(0); i < maxAids+10; i++ {
		aids = append(aids, int64(5000)+i)
	}
	aids = append(aids, tossAid, 0, -3)

	reply, err := NewBatchGetTargetSummaryLogic(context.Background(), sc).
		BatchGetTargetSummary(&rpc.BatchGetTargetSummaryReq{Aids: aids})
	if err != nil {
		t.Fatalf("超量只该裁剪不该让整屏 feed 失败：%v", err)
	}
	if int64(len(reply.Summaries)) != maxAids {
		t.Fatalf("裁剪到上限失败：返回 %d 条，上限 %d", len(reply.Summaries), maxAids)
	}
	if reply.Summaries[0].Aid != 5000 || reply.Summaries[1].Aid != 5001 {
		t.Errorf("裁剪后顺序必须保持请求顺序：%+v", reply.Summaries[:2])
	}
	// 被裁掉的 aid 不出现在结果里（调用方下一屏再取），且没有任何一条来自非法 aid。
	seen := map[int64]int{}
	for _, s := range reply.Summaries {
		seen[s.Aid]++
		if s.Aid <= 0 {
			t.Errorf("非法 aid 混进结果：%d", s.Aid)
		}
	}
	if seen[tossAid] != 0 {
		t.Errorf("排在超限位置的 aid 应被裁掉，不该出现：%d", seen[tossAid])
	}
	if len(seen) != len(reply.Summaries) {
		t.Error("结果里出现了重复 aid（去重失败会让 IN 列表膨胀）")
	}
}

func TestBatchGetTargetSummaryInputConclusions(t *testing.T) {
	t.Run("空入参回空数组", func(t *testing.T) {
		sc, _ := newTestSvc(t)
		reply, err := NewBatchGetTargetSummaryLogic(context.Background(), sc).
			BatchGetTargetSummary(&rpc.BatchGetTargetSummaryReq{})
		if err != nil {
			t.Fatalf("空请求是合法的：%v", err)
		}
		if reply.Summaries == nil || len(reply.Summaries) != 0 {
			t.Errorf("应回 [] 而不是 null：%#v", reply.Summaries)
		}
	})
	t.Run("给了 aid 但全部非法按参数错误", func(t *testing.T) {
		sc, db := newTestSvc(t)
		_, err := NewBatchGetTargetSummaryLogic(context.Background(), sc).
			BatchGetTargetSummary(&rpc.BatchGetTargetSummaryReq{Aids: []int64{0, -1, -7}})
		wantErrIs(t, err, model.ErrInvalidAids)
		db.wantNoCall(t, "Tosses.SummarizeTargets")
	})
	t.Run("去重后只查一次", func(t *testing.T) {
		sc, db := newTestSvc(t)
		seedToss(db, &model.Toss{Mid: 1, TargetAid: tossAid, Count: 2, State: model.TossStateActive})
		reply, err := NewBatchGetTargetSummaryLogic(context.Background(), sc).
			BatchGetTargetSummary(&rpc.BatchGetTargetSummaryReq{Aids: []int64{tossAid, tossAid, tossAid}})
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Summaries) != 1 || reply.Summaries[0].CoinCount != 2 {
			t.Fatalf("去重不符：%+v", reply.Summaries)
		}
		if n := db.countCall("Tosses.SummarizeTargets"); n != 1 {
			t.Errorf("应只回源一次，实际 %d 次", n)
		}
	})
	t.Run("缺失的 aid 补全 0 而不是漏项", func(t *testing.T) {
		sc, db := newTestSvc(t)
		seedToss(db, &model.Toss{Mid: 1, TargetAid: tossAid, Count: 7, State: model.TossStateActive})
		reply, err := NewBatchGetTargetSummaryLogic(context.Background(), sc).
			BatchGetTargetSummary(&rpc.BatchGetTargetSummaryReq{Aids: []int64{tossAid, 6006}})
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Summaries) != 2 {
			t.Fatalf("每个请求里的有效 aid 都要有一条：%+v", reply.Summaries)
		}
		if reply.Summaries[1].Aid != 6006 || reply.Summaries[1].CoinCount != 0 {
			t.Errorf("无投币的 aid 应回全 0 汇总：%+v", reply.Summaries[1])
		}
	})
	t.Run("聚合读失败必须上抛", func(t *testing.T) {
		sc, db := newTestSvc(t)
		db.Fail("Tosses.SummarizeTargets", errBoom)
		_, err := NewBatchGetTargetSummaryLogic(context.Background(), sc).
			BatchGetTargetSummary(&rpc.BatchGetTargetSummaryReq{Aids: []int64{1}})
		wantErrIs(t, err, errBoom)
	})
}

func TestListCoinFlowsRejectsUnusableQueries(t *testing.T) {
	cases := []struct {
		name    string
		in      *rpc.ListCoinFlowsReq
		wantErr error
	}{
		{"mid 为负", &rpc.ListCoinFlowsReq{Mid: -1}, model.ErrInvalidMid},
		{"流水类型枚举外", &rpc.ListCoinFlowsReq{Mid: 7, FlowType: rpc.CoinFlowType(42)}, model.ErrInvalidFlowType},
		{"负时间戳", &rpc.ListCoinFlowsReq{Mid: 7, FromTs: -1}, model.ErrInvalidTimeRange},
		{"from 晚于 to", &rpc.ListCoinFlowsReq{Mid: 7, FromTs: 200, ToTs: 100}, model.ErrInvalidTimeRange},
		{"size 超上限", &rpc.ListCoinFlowsReq{Mid: 7, Size: 101}, model.ErrPageSizeTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedAccountWithLedger(db, tossMid, 8)
			reply, err := NewListCoinFlowsLogic(context.Background(), sc).ListCoinFlows(tc.in)
			wantErrIs(t, err, tc.wantErr)
			if reply != nil {
				t.Errorf("参数非法时不得返回半截台账：%+v", reply)
			}
			db.wantNoCall(t, "Flows.List", "Flows.Count")
		})
	}
}

// TestListCoinFlowsRejectsUnboundedScans 跨用户（mid=0）台账没给时间窗/订单号就是
// 对 append-only 大表的全表扫描 + COUNT(*)，一次运营误查就能把主库拖住。
// 拒绝发生在 model 层（FlowFilter.Bounded），logic 必须原样上抛而不是当成「空台账」。
func TestListCoinFlowsRejectsUnboundedScans(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListCoinFlowsReq
	}{
		{"跨用户无任何边界", &rpc.ListCoinFlowsReq{}},
		{"只给 flow_type 仍无界", &rpc.ListCoinFlowsReq{FlowType: rpc.CoinFlowType_COIN_FLOW_TYPE_TOSS}},
		{"单边时间窗仍无界（只有 from）", &rpc.ListCoinFlowsReq{FromTs: 100}},
		{"单边时间窗仍无界（只有 to）", &rpc.ListCoinFlowsReq{ToTs: 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedAccountWithLedger(db, tossMid, 8)
			reply, err := NewListCoinFlowsLogic(context.Background(), sc).ListCoinFlows(tc.in)
			wantErrIs(t, err, model.ErrUnboundedLedgerQuery)
			if reply != nil {
				t.Errorf("无界扫描不能返回空台账冒充事实：%+v", reply)
			}
			// 拒绝在第一次读就发生，连 COUNT(*) 都不该发出去。
			if n := db.countCall("Flows.List"); n != 1 {
				t.Errorf("应只发一次被拒的 List，实际 %d", n)
			}
			db.wantNoCall(t, "Flows.Count")
		})
	}
}

func TestListCoinFlowsFiltersAndPages(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	now := model.NowUnix()
	seedFlow(db, &model.Flow{Mid: tossMid, FlowType: model.FlowTypeToss, Delta: -1,
		BalanceAfter: 7, TargetAid: tossAid, RequestID: "f1", Ctime: now - 300})
	seedFlow(db, &model.Flow{Mid: tossMid, FlowType: model.FlowTypeOrderPack, Delta: 20,
		BalanceAfter: 27, BizNo: "ORDER-A", RequestID: "f2", Ctime: now - 200})
	seedFlow(db, &model.Flow{Mid: tossMid, FlowType: model.FlowTypeAdminGrant, Delta: -9,
		BalanceAfter: 18, BizNo: "ORDER-B", Operator: "ops-1", RequestID: "f3", Ctime: now - 100})

	all, err := NewListCoinFlowsLogic(context.Background(), sc).
		ListCoinFlows(&rpc.ListCoinFlowsReq{Mid: tossMid})
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 3 || len(all.Flows) != 3 {
		t.Fatalf("本人台账不分页时应全给：total=%d len=%d", all.Total, len(all.Flows))
	}
	if all.Flows[0].RequestId != "f3" || all.Flows[2].RequestId != "f1" {
		t.Errorf("台账必须按时间倒序：%s %s %s",
			all.Flows[0].RequestId, all.Flows[1].RequestId, all.Flows[2].RequestId)
	}
	first := all.Flows[0]
	if first.FlowId == 0 || first.FlowType != rpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT ||
		first.Delta != -9 || first.BalanceAfter != 18 || first.BizNo != "ORDER-B" || first.Operator != "ops-1" {
		t.Errorf("流水投影不符：%+v", first)
	}

	byType, err := NewListCoinFlowsLogic(context.Background(), sc).
		ListCoinFlows(&rpc.ListCoinFlowsReq{Mid: tossMid, FlowType: rpc.CoinFlowType_COIN_FLOW_TYPE_TOSS})
	if err != nil {
		t.Fatal(err)
	}
	if byType.Total != 1 || byType.Flows[0].RequestId != "f1" {
		t.Fatalf("类型过滤不符：%+v", byType.Flows)
	}

	byWindow, err := NewListCoinFlowsLogic(context.Background(), sc).
		ListCoinFlows(&rpc.ListCoinFlowsReq{Mid: 0, FromTs: now - 250, ToTs: now})
	if err != nil {
		t.Fatalf("跨用户查询给了时间窗就该放行：%v", err)
	}
	if byWindow.Total != 2 {
		t.Errorf("时间窗过滤不符：total=%d", byWindow.Total)
	}

	byBiz, err := NewListCoinFlowsLogic(context.Background(), sc).
		ListCoinFlows(&rpc.ListCoinFlowsReq{Mid: 0, BizNo: "ORDER-A"})
	if err != nil {
		t.Fatal(err)
	}
	if byBiz.Total != 1 || byBiz.Flows[0].RequestId != "f2" {
		t.Errorf("订单号过滤不符：%+v", byBiz.Flows)
	}

	page2, err := NewListCoinFlowsLogic(context.Background(), sc).
		ListCoinFlows(&rpc.ListCoinFlowsReq{Mid: tossMid, Page: 2, Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page2.Total != 3 || len(page2.Flows) != 1 || page2.Flows[0].RequestId != "f1" {
		t.Errorf("第 2 页内容不符：total=%d flows=%+v", page2.Total, page2.Flows)
	}
}

// TestListCoinFlowsReadFailureNotCollapsedToEmpty 空列表在调用方眼里等于
// 「这个人没有过任何资金变动」，把故障伪装成对账事实是最坏的错误。
func TestListCoinFlowsReadFailureNotCollapsedToEmpty(t *testing.T) {
	for _, op := range []string{"Flows.List", "Flows.Count"} {
		t.Run(op, func(t *testing.T) {
			sc, db := newTestSvc(t)
			seedFlow(db, &model.Flow{Mid: tossMid, FlowType: model.FlowTypeToss, Delta: -1,
				BalanceAfter: 7, RequestID: "f1"})
			db.Fail(op, errBoom)
			reply, err := NewListCoinFlowsLogic(context.Background(), sc).
				ListCoinFlows(&rpc.ListCoinFlowsReq{Mid: tossMid})
			wantErrIs(t, err, errBoom)
			if reply != nil {
				t.Errorf("读失败时不得返回半截台账：%+v", reply)
			}
		})
	}
}

func TestReadPathsNeverEnterTransactions(t *testing.T) {
	sc, db := newTestSvc(t)
	fixedClock(t, clockAt(0))
	seedAccountWithLedger(db, tossMid, 8)
	seedToss(db, &model.Toss{Mid: tossMid, TargetAid: tossAid, Count: 2, State: model.TossStateActive,
		LastTossedAt: model.NowUnix() - 10})
	ctx := context.Background()

	if _, err := NewGetCoinAccountLogic(ctx, sc).GetCoinAccount(&rpc.GetCoinAccountReq{Mid: tossMid}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewListMyTossesLogic(ctx, sc).ListMyTosses(&rpc.ListMyTossesReq{Mid: tossMid}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewListTargetTossersLogic(ctx, sc).ListTargetTossers(&rpc.ListTargetTossersReq{TargetAid: tossAid}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGetTargetSummaryLogic(ctx, sc).GetTargetSummary(&rpc.GetTargetSummaryReq{Aid: tossAid}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBatchGetTargetSummaryLogic(ctx, sc).BatchGetTargetSummary(
		&rpc.BatchGetTargetSummaryReq{Aids: []int64{tossAid}}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewListCoinFlowsLogic(ctx, sc).ListCoinFlows(&rpc.ListCoinFlowsReq{Mid: tossMid}); err != nil {
		t.Fatal(err)
	}
	// 任何一次余额写口被读路径碰到，都是所有权边界被穿透。
	db.wantNoCall(t, "Accounts.EnsureTx", "Accounts.DeductForTossTx", "Accounts.RefundForCancelTx",
		"Accounts.ApplyGrantTx", "Flows.InsertTx", "Tosses.InsertTx", "Tosses.CancelTx",
		"Daily.EnsureTx", "Daily.AccumulateTx", "Daily.RollbackTx")
	if db.txRuns != 0 {
		t.Errorf("读路径开了 %d 次事务", db.txRuns)
	}
	if acc := db.account(t, tossMid); acc.Balance != 8 {
		t.Errorf("读路径改了余额：%d", acc.Balance)
	}
	db.wantLedgerParity(t, tossMid)
}

// 汇总缓存未启用时必须回源 MySQL 而不是返回空（GetTargetSummary/Batch 各证一次）。
func TestSummaryWithoutCacheFallsBackToMySQL(t *testing.T) {
	sc, db := newTestSvc(t)
	sc.Config.Coin.TargetSummaryCacheTTLSeconds = 0
	seedToss(db, &model.Toss{Mid: 1, TargetAid: tossAid, Count: 4, State: model.TossStateActive})

	single, err := NewGetTargetSummaryLogic(context.Background(), sc).
		GetTargetSummary(&rpc.GetTargetSummaryReq{Aid: tossAid})
	if err != nil {
		t.Fatal(err)
	}
	if single.Summary.CoinCount != 4 || single.Summary.CoinUserCount != 1 {
		t.Errorf("未启用缓存时必须回源：%+v", single.Summary)
	}
	batch, err := NewBatchGetTargetSummaryLogic(context.Background(), sc).
		BatchGetTargetSummary(&rpc.BatchGetTargetSummaryReq{Aids: []int64{tossAid}})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Summaries) != 1 || batch.Summaries[0].CoinCount != 4 {
		t.Errorf("批量同理必须回源：%+v", batch.Summaries)
	}
	if n := db.countCall("Tosses.SummarizeTargets"); n != 2 {
		t.Errorf("两次都应回源，实际 %d 次", n)
	}
}

func TestReadPathsUseInjectedClockDay(t *testing.T) {
	sc, db := newTestSvc(t)
	// 日额度按注入时钟的「今天」取桶：时钟换到次日，读到的就是次日的空桶（跨日天然重置）。
	fixedClock(t, clockAt(0))
	seedAccountWithLedger(db, tossMid, 8)
	seedDaily(db, tossMid, model.TodayDayNo(), 9)
	first, err := NewGetCoinAccountLogic(context.Background(), sc).
		GetCoinAccount(&rpc.GetCoinAccountReq{Mid: tossMid})
	if err != nil {
		t.Fatal(err)
	}
	if first.Account.TodayTossed != 9 {
		t.Fatalf("今日已投未投影：%+v", first.Account)
	}
	fixedClock(t, clockAt(int(24*time.Hour/time.Second)))
	if model.DayNo(clockAt(0)) == model.TodayDayNo() {
		t.Fatal("时钟没跨日，本用例失去意义")
	}
	second, err := NewGetCoinAccountLogic(context.Background(), sc).
		GetCoinAccount(&rpc.GetCoinAccountReq{Mid: tossMid})
	if err != nil {
		t.Fatal(err)
	}
	if second.Account.TodayTossed != 0 {
		t.Errorf("跨日未重置今日额度：%+v", second.Account)
	}
	if second.Account.Balance != 8 {
		t.Errorf("跨日不该动余额：%+v", second.Account)
	}
}
