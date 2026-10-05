package logic

// 全量标记已读（MarkAllRead）：按分类/全部分类推进状态，与 MarkRead 同一套
// 「变更才刷新 + 同事务重算 + 提交后失效缓存」编排。
//
// 这个方法独有风险：一条 UPDATE 影响很多行，所以
//  1. 已读的行不能被反复改写（写放大：mtime 抖动 + binlog 膨胀）；
//  2. 分类过滤必须在 SQL 的 WHERE 里，而不是取回后在 Go 里筛（否则跨分类未读会被误清零）；
//  3. 非法分类必须在开事务之前拒掉，不能留下一个空事务。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"
)

// markAllEnv 给 Alice 布 2 条系统未读 + 1 条互动未读 + 1 条系统已读，
// 并给 Bob 布 1 条互动未读（用来证明「全量已读」只作用于请求里的 mid）。
func markAllEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	st := e.st
	seedUnreadFor(t, st, midAlice, []int32{
		model.CategorySystem, model.CategorySystem, model.CategoryEngagement,
	})
	// 再补一条「已经是已读」的系统消息：changed 只能统计真实推进的行。
	readMsg := seedMessage(t, st, &model.InboxMessage{
		MsgID: 7501, Category: model.CategorySystem, MsgType: model.MsgTypeText,
		Title: "已读过的系统消息", Content: "上周的对账单", Ctime: 1700000400,
	})
	seedRow(t, st, &model.InboxUserMessage{
		Mid: midAlice, MsgID: readMsg.MsgID, Category: model.CategorySystem,
		ReadState: model.ReadStateRead, DelState: model.DelStateNormal, Ctime: 1700000400,
	})
	seedUnreadFor(t, st, midBob, []int32{model.CategoryEngagement})
	return e
}

func TestMarkAllReadGuards(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.MarkAllReadReq
		want error
	}{
		{"mid 为 0", &rpc.MarkAllReadReq{Mid: 0}, model.ErrInvalidMid},
		{"mid 为负", &rpc.MarkAllReadReq{Mid: -1}, model.ErrInvalidMid},
		// 分类守卫由 Repository 在开事务之前拒掉，同样要求零依赖调用。
		{"分类越界（9）", &rpc.MarkAllReadReq{Mid: midAlice, Category: rpc.Category(9)}, model.ErrInvalidCategory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := markAllEnv(t)
			before := e.st.log.snapshot()

			reply, err := NewMarkAllReadLogic(context.Background(), e.svcCtx).MarkAllRead(tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：守卫拒绝仍返回响应体 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
			wantEQ(t, tc.name, "Alice 未读快照保持原样", fmt.Sprint(e.st.statOf(midAlice)),
				fmt.Sprint(map[int32]int64{1: 2, 2: 1, 3: 0, 4: 0}))
		})
	}
}

// CATEGORY_UNSPECIFIED(0) = 不限分类：三条未读一次清完，已读那条不计入 changed。
func TestMarkAllReadClearsEveryCategoryAndSkipsAlreadyReadRows(t *testing.T) {
	e := markAllEnv(t)
	st := e.st
	before := st.log.snapshot()

	reply, err := NewMarkAllReadLogic(context.Background(), e.svcCtx).MarkAllRead(
		&rpc.MarkAllReadReq{Mid: midAlice, Category: rpc.Category_CATEGORY_UNSPECIFIED})
	wantNoErr(t, "全量已读", err)

	// changed=3 而不是 4：已经是已读的那行不得被再写一次。
	wantEQ(t, "全量已读响应", "Changed", reply.Changed, int32(3))
	wantEQ(t, "全量已读响应", "UnreadTotal", reply.UnreadTotal, int64(0))

	for _, msgID := range st.rowIDs(midAlice) {
		wantEQ(t, fmt.Sprintf("Alice 明细 %d", msgID), "ReadState", st.getRow(t, midAlice, msgID).ReadState, model.ReadStateRead)
	}
	wantMapEQ(t, "全量已读后的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 0, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantOps(t, "全量已读的调用链", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		fmt.Sprintf("user.MarkAllReadBatch:%d/0/tx", midAlice),
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/tx", midAlice),
		fmt.Sprintf("cache.Invalidate:%d", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("cache.Set:%d/0", midAlice),
	})
}

// 分类过滤必须落在 SQL 的 WHERE 里：只清系统分类，互动未读必须原样保留。
func TestMarkAllReadScopedToCategoryLeavesOtherCategoriesUnread(t *testing.T) {
	e := markAllEnv(t)
	st := e.st
	before := st.log.snapshot()

	reply, err := NewMarkAllReadLogic(context.Background(), e.svcCtx).MarkAllRead(
		&rpc.MarkAllReadReq{Mid: midAlice, Category: rpc.Category_CATEGORY_SYSTEM})
	wantNoErr(t, "按分类已读", err)

	wantEQ(t, "按分类已读响应", "Changed", reply.Changed, int32(2))
	wantEQ(t, "按分类已读响应", "UnreadTotal 保留互动未读", reply.UnreadTotal, int64(1))
	wantMapEQ(t, "按分类已读后的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantOps(t, "按分类已读的调用链", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		fmt.Sprintf("user.MarkAllReadBatch:%d/%d/tx", midAlice, model.CategorySystem),
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/tx", midAlice),
		fmt.Sprintf("cache.Invalidate:%d", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("cache.Set:%d/1", midAlice),
	})

	// 再来一次：该分类已全读，changed=0 且零写放大（不重算、不覆盖快照、不失效缓存）。
	repeat := st.log.snapshot()
	again, err := NewMarkAllReadLogic(context.Background(), e.svcCtx).MarkAllRead(
		&rpc.MarkAllReadReq{Mid: midAlice, Category: rpc.Category_CATEGORY_SYSTEM})
	wantNoErr(t, "重复按分类已读", err)
	wantEQ(t, "重复按分类已读", "Changed", again.Changed, int32(0))
	wantEQ(t, "重复按分类已读", "UnreadTotal 不得二次扣减", again.UnreadTotal, int64(1))
	wantOps(t, "重复全量已读只做一次条件 UPDATE", st.log.opsFrom(repeat), []string{
		"conn.TransactCtx",
		fmt.Sprintf("user.MarkAllReadBatch:%d/%d/tx", midAlice, model.CategorySystem),
		fmt.Sprintf("cache.Get:%d", midAlice),
	})
	wantAbsentFrom(t, "重复全量已读", st.log, repeat, "stat.ReplaceByMid")
	wantAbsentFrom(t, "重复全量已读", st.log, repeat, "cache.Invalidate")
}

// 全量已读是「本人的操作」：其它收件人的明细与快照不得被顺带改写。
func TestMarkAllReadNeverTouchesOtherRecipients(t *testing.T) {
	e := markAllEnv(t)
	st := e.st

	reply, err := NewMarkAllReadLogic(context.Background(), e.svcCtx).MarkAllRead(
		&rpc.MarkAllReadReq{Mid: midAlice})
	wantNoErr(t, "Alice 全量已读", err)
	wantEQ(t, "Alice 全量已读", "Changed", reply.Changed, int32(3))

	for _, msgID := range st.rowIDs(midBob) {
		row := st.getRow(t, midBob, msgID)
		wantEQ(t, fmt.Sprintf("Bob 的明细 %d 未被波及", msgID), "ReadState", row.ReadState, model.ReadStateUnread)
	}
	wantMapEQ(t, "Bob 的快照未被改写", "stat", st.statOf(midBob), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantAbsent(t, "Alice 的操作不得失效 Bob 的缓存", st.log, fmt.Sprintf("cache.Invalidate:%d", midBob))
}

// 快照行缺失的用户：MarkAllReadBatch 改到 0 行，不空转事务，但计数读必须自己兜底重算。
func TestMarkAllReadOnEmptyInboxFallsBackToRecompute(t *testing.T) {
	e := newEnv(t)
	st := e.st
	before := st.log.snapshot()

	reply, err := NewMarkAllReadLogic(context.Background(), e.svcCtx).MarkAllRead(
		&rpc.MarkAllReadReq{Mid: midCarol})
	wantNoErr(t, "空收件箱全量已读", err)
	wantEQ(t, "空收件箱", "Changed", reply.Changed, int32(0))
	wantEQ(t, "空收件箱", "UnreadTotal", reply.UnreadTotal, int64(0))
	wantOps(t, "空收件箱的调用链", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		fmt.Sprintf("user.MarkAllReadBatch:%d/0/tx", midCarol),
		fmt.Sprintf("cache.Get:%d", midCarol),
		fmt.Sprintf("stat.ListByMid:%d", midCarol),
		fmt.Sprintf("user.CountUnreadByCategory:%d", midCarol),
		fmt.Sprintf("stat.ReplaceByMid:%d/conn", midCarol),
		fmt.Sprintf("cache.Set:%d/0", midCarol),
	})
	wantMapEQ(t, "补出来的零快照", "stat", st.statOf(midCarol), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 0, model.CategoryContent: 0, model.CategoryLive: 0,
	})
}

func TestMarkAllReadPropagatesDependencyErrors(t *testing.T) {
	errDB := errors.New("inbox-test-markall-down")

	cases := []struct {
		name       string
		fail       func(st *store)
		wantOps    []string
		wantRolled bool
	}{
		{
			name:    "批量 UPDATE 失败",
			fail:    func(st *store) { st.users.failWith("MarkAllReadBatch", errDB) },
			wantOps: []string{"conn.TransactCtx", "user.MarkAllReadBatch:"},
		},
		{
			name:    "事务内重算未读失败",
			fail:    func(st *store) { st.users.failWith("CountUnreadByCategory", errDB) },
			wantOps: []string{"conn.TransactCtx", "user.MarkAllReadBatch:", "user.CountUnreadByCategory:"},
		},
		{
			name:    "快照覆盖失败",
			fail:    func(st *store) { st.stats.failWith("ReplaceByMid", errDB) },
			wantOps: []string{"conn.TransactCtx", "user.MarkAllReadBatch:", "user.CountUnreadByCategory:", "stat.ReplaceByMid:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := markAllEnv(t)
			st := e.st
			tc.fail(st)
			before := st.log.snapshot()

			reply, err := NewMarkAllReadLogic(context.Background(), e.svcCtx).MarkAllRead(
				&rpc.MarkAllReadReq{Mid: midAlice})

			wantErrIs(t, tc.name, err, errDB)
			if reply != nil {
				t.Fatalf("%s：下游失败仍返回响应体 %+v", tc.name, reply)
			}
			wantOpsPrefix(t, tc.name+" 的调用链", st.log.opsFrom(before), tc.wantOps)
			wantAbsent(t, tc.name+"：不得失效缓存", st.log, "cache.Invalidate")
			wantAbsent(t, tc.name+"：不得回填缓存", st.log, "cache.Set")
			// 三条未读必须整体回滚，读者看到的还是布景时的 2/1。
			var stillUnread int
			for _, row := range st.tb.rows {
				if row.Mid == midAlice && row.ReadState == model.ReadStateUnread {
					stillUnread++
				}
			}
			wantEQ(t, tc.name+"：事务回滚后的未读行数", "stillUnread", stillUnread, 3)
			wantMapEQ(t, tc.name+"：快照未被半截改写", "stat", st.statOf(midAlice), map[int32]int64{
				model.CategorySystem: 2, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
			})
		})
	}
}

// 变更已提交、随后读权威计数失败：报错但保留提交，绝不返回 unread_total=0 的假成功。
func TestMarkAllReadReportsFailureWhenAuthoritativeCountReadBreaks(t *testing.T) {
	errStat := errors.New("inbox-test-stat-down")
	e := markAllEnv(t)
	st := e.st
	before := st.log.snapshot()
	st.stats.failWith("ListByMid", errStat)

	reply, err := NewMarkAllReadLogic(context.Background(), e.svcCtx).MarkAllRead(
		&rpc.MarkAllReadReq{Mid: midAlice})

	wantErrIs(t, "全量已读后计数读失败", err, errStat)
	if reply != nil {
		t.Fatalf("计数读失败仍返回响应体 %+v", reply)
	}
	wantOpsPrefix(t, "调用链", st.log.opsFrom(before), []string{
		"conn.TransactCtx", "user.MarkAllReadBatch:", "user.CountUnreadByCategory:",
		"stat.ReplaceByMid:", "cache.Invalidate:", "cache.Get:", "stat.ListByMid:",
	})
	for _, msgID := range st.rowIDs(midAlice) {
		wantEQ(t, "已提交的明细", "ReadState", st.getRow(t, midAlice, msgID).ReadState, model.ReadStateRead)
	}
	wantMapEQ(t, "已提交的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 0, model.CategoryContent: 0, model.CategoryLive: 0,
	})
}
