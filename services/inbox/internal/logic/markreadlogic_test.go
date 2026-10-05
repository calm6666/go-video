package logic

// 标记已读（MarkRead）：状态推进 + 计数联动 + 幂等。
//
// 这个方法最要紧的三条不变量：
//  1. 「变更才刷新」：UPDATE 真实改到行数才重算快照、才失效 Redis，
//     重复标记已读必须 changed=0 且不二次扣减计数（否则用户刷新两次未读就归零）；
//  2. 「明细与快照同事务」：重算快照的两条语句都在同一个 TransactCtx 里（op 后缀 tx 是证据），
//     事务内任一步失败要整体回滚，读者不会看到「明细已读、快照仍计未读」；
//  3. 「写接口不静默降级」：变更已经提交，但随后读权威计数失败时必须报错，
//     不能返回 unread_total=0 的假成功（列表接口才允许降级，见 listmessageslogic_test.go）。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"
)

// markReadEnv 布 Alice 的 3 条未读（系统 2 条 + 互动 1 条）与一致快照，
// 返回系统消息与互动消息的 msg_id。三条必须在一次 seedUnreadFor 里布下去：
// 该助手会顺带写一份与明细一致的快照，分两次布会让后一次覆盖前一次的快照。
func markReadEnv(t *testing.T) (*env, []int64, []int64) {
	t.Helper()
	e := newEnv(t)
	ids := seedUnreadFor(t, e.st, midAlice, []int32{
		model.CategorySystem, model.CategorySystem, model.CategoryEngagement,
	})
	return e, ids[:2], ids[2:]
}

func TestMarkReadGuards(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.MarkReadReq
		want error
	}{
		{"mid 为 0", &rpc.MarkReadReq{Mid: 0, MsgIds: []int64{7301}}, model.ErrInvalidMid},
		{"mid 为负", &rpc.MarkReadReq{Mid: -91001, MsgIds: []int64{7301}}, model.ErrInvalidMid},
		{"msg_ids 为 nil", &rpc.MarkReadReq{Mid: midAlice, MsgIds: nil}, model.ErrMessageNotFound},
		{"msg_ids 为空切片", &rpc.MarkReadReq{Mid: midAlice, MsgIds: []int64{}}, model.ErrMessageNotFound},
		{"mid 与 msg_ids 同时非法：先校验 mid", &rpc.MarkReadReq{Mid: 0}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()

			reply, err := NewMarkReadLogic(context.Background(), e.svcCtx).MarkRead(tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：守卫拒绝仍返回响应体 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

// 正常路径：改状态、重算快照、失效缓存、回读权威计数，逐字段投影并钉死调用顺序。
func TestMarkReadAdvancesStateAndRefreshesCountInOneTransaction(t *testing.T) {
	e, sys, eng := markReadEnv(t)
	st := e.st
	before := st.log.snapshot()

	reply, err := NewMarkReadLogic(context.Background(), e.svcCtx).MarkRead(
		&rpc.MarkReadReq{Mid: midAlice, MsgIds: []int64{sys[0], sys[1]}})
	wantNoErr(t, "标记已读", err)

	wantEQ(t, "标记已读响应", "Changed", reply.Changed, int32(2))
	wantEQ(t, "标记已读响应", "UnreadTotal", reply.UnreadTotal, int64(1))

	for _, msgID := range sys {
		row := st.getRow(t, midAlice, msgID)
		wantEQ(t, fmt.Sprintf("明细 %d", msgID), "ReadState", row.ReadState, model.ReadStateRead)
		wantEQ(t, fmt.Sprintf("明细 %d", msgID), "DelState", row.DelState, model.DelStateNormal)
	}
	wantEQ(t, "未点名的那条明细", "ReadState", st.getRow(t, midAlice, eng[0]).ReadState, model.ReadStateUnread)
	// 快照按整体覆盖写入：四个分类都在，被标记的两个系统归零，互动保持 1。
	wantMapEQ(t, "重算后的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantMapEQ(t, "回填后的缓存副本", "cache", st.cache.cached(midAlice), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})

	wantOps(t, "标记已读的调用链", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		fmt.Sprintf("user.MarkReadBatch:%d/%s/tx", midAlice, idsTag([]int64{sys[0], sys[1]})),
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/tx", midAlice),
		fmt.Sprintf("cache.Invalidate:%d", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("cache.Set:%d/1", midAlice),
	})
}

// 幂等：同一批 msg_id 第二次调用 changed=0，不重算快照、不失效缓存，
// 且 unread_total 必须保持第一次的结果而不是再扣一次。
func TestMarkReadIsIdempotentAndNeverDoubleDecrements(t *testing.T) {
	e, sys, _ := markReadEnv(t)
	st := e.st

	first, err := NewMarkReadLogic(context.Background(), e.svcCtx).MarkRead(
		&rpc.MarkReadReq{Mid: midAlice, MsgIds: sys})
	wantNoErr(t, "首次标记", err)
	wantEQ(t, "首次标记", "Changed", first.Changed, int32(2))
	wantEQ(t, "首次标记", "UnreadTotal", first.UnreadTotal, int64(1))
	statAfterFirst := st.statOf(midAlice)

	before := st.log.snapshot()
	second, err := NewMarkReadLogic(context.Background(), e.svcCtx).MarkRead(
		&rpc.MarkReadReq{Mid: midAlice, MsgIds: sys})
	wantNoErr(t, "重复标记", err)
	wantEQ(t, "重复标记", "Changed", second.Changed, int32(0))
	wantEQ(t, "重复标记", "UnreadTotal 不得二次扣减", second.UnreadTotal, int64(1))

	wantOps(t, "重复标记只做一次条件 UPDATE", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		fmt.Sprintf("user.MarkReadBatch:%d/%s/tx", midAlice, idsTag(sys)),
		fmt.Sprintf("cache.Get:%d", midAlice),
	})
	wantAbsentFrom(t, "重复标记不得重算快照", st.log, before, "user.CountUnreadByCategory")
	wantAbsentFrom(t, "重复标记不得覆盖快照", st.log, before, "stat.ReplaceByMid")
	wantAbsentFrom(t, "重复标记不得失效缓存", st.log, before, "cache.Invalidate")
	wantAbsentFrom(t, "重复标记不得回源", st.log, before, "stat.ListByMid")
	wantMapEQ(t, "重复标记后的快照", "stat", st.statOf(midAlice), statAfterFirst)
}

// 未读数不是靠加减维护的：第二次标记同一条明细时，changed 由 UPDATE 的真实影响行数决定，
// 快照由明细表重算决定，所以「重复标记」与「标记一条不属于本人的消息」都必须计数不变。
func TestMarkReadOfForeignOrUnknownMessageLeavesCountAlone(t *testing.T) {
	e, _, eng := markReadEnv(t)
	st := e.st
	// Bob 的消息：Alice 的 mid 下没有这行明细。
	bobMsg := seedMessage(t, st, &model.InboxMessage{
		MsgID: 7450, Category: model.CategorySystem, MsgType: model.MsgTypeText,
		Title: "只发给 Bob", Content: "Bob 的账单", Ctime: 1700000700,
	})
	seedRow(t, st, &model.InboxUserMessage{
		Mid: midBob, MsgID: bobMsg.MsgID, Category: model.CategorySystem,
		ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: 1700000700,
	})
	before := st.log.snapshot()

	reply, err := NewMarkReadLogic(context.Background(), e.svcCtx).MarkRead(&rpc.MarkReadReq{
		Mid: midAlice, MsgIds: []int64{bobMsg.MsgID, 999999, eng[0]},
	})
	wantNoErr(t, "混合标记", err)
	// 三条里只有 eng[0] 属于 Alice：changed=1，另外两条不得凭空扣计数。
	wantEQ(t, "混合标记", "Changed", reply.Changed, int32(1))
	wantEQ(t, "混合标记", "UnreadTotal", reply.UnreadTotal, int64(2))
	wantOps(t, "混合标记的调用链", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		fmt.Sprintf("user.MarkReadBatch:%d/%s/tx", midAlice, idsTag([]int64{bobMsg.MsgID, 999999, eng[0]})),
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/tx", midAlice),
		fmt.Sprintf("cache.Invalidate:%d", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("cache.Set:%d/2", midAlice),
	})
	// 越权那行必须还是 Bob 的未读，谁都没替它改状态。
	bobRow := st.getRow(t, midBob, bobMsg.MsgID)
	wantEQ(t, "Bob 的明细", "ReadState", bobRow.ReadState, model.ReadStateUnread)
	wantMapEQ(t, "Bob 的快照", "stat", st.statOf(midBob), map[int32]int64{})
}

// 失败传播：明细 UPDATE / 事务内重算 / 事务后读计数 三个下游各失败一次。
// 前两种必须整体回滚（明细回到未读、快照与缓存一字未动），
// 第三种必须保留已提交的变更同时把错误抛出去——不能返回 unread_total=0 的假成功。
func TestMarkReadPropagatesDependencyErrors(t *testing.T) {
	errDB := errors.New("inbox-test-markread-down")

	cases := []struct {
		name    string
		fail    func(st *store)
		wantOps []string
	}{
		{
			name:    "明细 UPDATE 失败",
			fail:    func(st *store) { st.users.failWith("MarkReadBatch", errDB) },
			wantOps: []string{"conn.TransactCtx", "user.MarkReadBatch:"},
		},
		{
			name:    "事务内重算未读失败",
			fail:    func(st *store) { st.users.failWith("CountUnreadByCategory", errDB) },
			wantOps: []string{"conn.TransactCtx", "user.MarkReadBatch:", "user.CountUnreadByCategory:"},
		},
		{
			name:    "快照覆盖失败",
			fail:    func(st *store) { st.stats.failWith("ReplaceByMid", errDB) },
			wantOps: []string{"conn.TransactCtx", "user.MarkReadBatch:", "user.CountUnreadByCategory:", "stat.ReplaceByMid:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, sys, _ := markReadEnv(t)
			st := e.st
			tc.fail(st)
			before := st.log.snapshot()

			reply, err := NewMarkReadLogic(context.Background(), e.svcCtx).MarkRead(
				&rpc.MarkReadReq{Mid: midAlice, MsgIds: sys})

			wantErrIs(t, tc.name, err, errDB)
			if reply != nil {
				t.Fatalf("%s：下游失败仍返回响应体 %+v", tc.name, reply)
			}
			wantOpsPrefix(t, tc.name+" 的调用链", st.log.opsFrom(before), tc.wantOps)
			wantAbsent(t, tc.name+"：不得失效缓存", st.log, "cache.Invalidate")
			wantAbsent(t, tc.name+"：不得写缓存", st.log, "cache.Set")
			// 回滚证据：明细仍是未读，快照仍是布景时的 2/1。
			for _, msgID := range sys {
				wantEQ(t, tc.name+"：事务回滚", "ReadState", st.getRow(t, midAlice, msgID).ReadState, model.ReadStateUnread)
			}
			wantMapEQ(t, tc.name+"：快照未被半截改写", "stat", st.statOf(midAlice), map[int32]int64{
				model.CategorySystem: 2, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
			})
		})
	}
}

// 变更已提交、随后读权威计数失败：必须报错，且不能把已提交的写回滚掉。
// 这是 unread_total 与列表接口口径不同的地方（写接口宁可不返回也不能返回 0）。
func TestMarkReadReportsFailureWhenAuthoritativeCountReadBreaks(t *testing.T) {
	errStat := errors.New("inbox-test-stat-down")
	e, sys, _ := markReadEnv(t)
	st := e.st
	before := st.log.snapshot()
	st.stats.failWith("ListByMid", errStat)

	reply, err := NewMarkReadLogic(context.Background(), e.svcCtx).MarkRead(
		&rpc.MarkReadReq{Mid: midAlice, MsgIds: sys})

	wantErrIs(t, "计数读失败", err, errStat)
	if reply != nil {
		t.Fatalf("计数读失败仍返回响应体 %+v", reply)
	}
	wantOps(t, "先提交再读计数的调用链", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		fmt.Sprintf("user.MarkReadBatch:%d/%s/tx", midAlice, idsTag(sys)),
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/tx", midAlice),
		fmt.Sprintf("cache.Invalidate:%d", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
	})
	// 事务已提交：明细与快照都保持变更后的状态，且失效缓存不被计数读失败影响。
	wantEQ(t, "已提交的明细", "ReadState", st.getRow(t, midAlice, sys[0]).ReadState, model.ReadStateRead)
	wantMapEQ(t, "已提交的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantEQ(t, "计数读失败不得留下缓存", "len(cache.data)", len(st.cache.data), 0)
}
