package logic

// 用户侧软删除（DeleteMessage）：只改本人收件行的 del_state。
//
// 要紧的结论：
//  1. 越权必须报错而不是静默成功：一条都不属于该收件人时返回 ErrMessageNotFound，
//     且不能「顺手」改动别人的行；
//  2. 删除是按 (mid,msg_id) 收窄的：消息主体与其它收件人的收件行都不能被波及
//     （站内信是一发多收，硬删主体会把别人的消息一起弄没）；
//  3. 幂等：重复删除 changed=0，未读总数不得二次扣减；
//  4. 删除已读的行 changed=1 但计数不变（真实 SQL 只按 del_state 收窄，未读数是重算出来的）。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"
)

// 固定 msg_id：77xx 段留给删除用例，与布景自增段（73xx）和列表用例（71xx）互不干扰。
const (
	msgAliceSystem1 int64 = 7701 // Alice 独有，未读
	msgAliceSystem2 int64 = 7702 // Alice 独有，未读（删除后仍剩它）
	msgShared       int64 = 7703 // Alice 与 Bob 共有，两人都是未读
	msgAliceRead    int64 = 7704 // Alice 独有，已是已读
	msgBobOnly      int64 = 7705 // 只属于 Bob，用来试越权
)

func deleteEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	st := e.st
	seedMsg := func(msgID int64, category int32, title string, ctime int64) {
		seedMessage(t, st, &model.InboxMessage{
			MsgID: msgID, Category: category, MsgType: model.MsgTypeText,
			Title: title, Content: "正文", BizType: "seed", BizID: fmt.Sprintf("b-%d", msgID), Ctime: ctime,
		})
	}
	seedMsg(msgAliceSystem1, model.CategorySystem, "实名认证通过", 1700000101)
	seedMsg(msgAliceSystem2, model.CategorySystem, "账号安全提醒", 1700000102)
	seedMsg(msgShared, model.CategoryEngagement, "有人回复了你", 1700000103)
	seedMsg(msgAliceRead, model.CategoryLive, "开播提醒", 1700000104)
	seedMsg(msgBobOnly, model.CategoryEngagement, "只发给 Bob 的回复", 1700000105)

	seedRow(t, st, &model.InboxUserMessage{
		Mid: midAlice, MsgID: msgAliceSystem1, Category: model.CategorySystem,
		ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: 1700000101,
	})
	seedRow(t, st, &model.InboxUserMessage{
		Mid: midAlice, MsgID: msgAliceSystem2, Category: model.CategorySystem,
		ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: 1700000102,
	})
	seedRow(t, st, &model.InboxUserMessage{
		Mid: midAlice, MsgID: msgShared, Category: model.CategoryEngagement,
		ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: 1700000103,
	})
	seedRow(t, st, &model.InboxUserMessage{
		Mid: midAlice, MsgID: msgAliceRead, Category: model.CategoryLive,
		ReadState: model.ReadStateRead, DelState: model.DelStateNormal, Ctime: 1700000104,
	})
	seedRow(t, st, &model.InboxUserMessage{
		Mid: midBob, MsgID: msgShared, Category: model.CategoryEngagement,
		ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: 1700000103,
	})
	seedRow(t, st, &model.InboxUserMessage{
		Mid: midBob, MsgID: msgBobOnly, Category: model.CategoryEngagement,
		ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: 1700000105,
	})
	seedStat(t, st, midAlice, map[int32]int64{model.CategorySystem: 2, model.CategoryEngagement: 1}, 1700000500)
	seedStat(t, st, midBob, map[int32]int64{model.CategoryEngagement: 2}, 1700000500)
	return e
}

func TestDeleteMessageGuards(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.DeleteMessageReq
		want error
	}{
		{"mid 为 0", &rpc.DeleteMessageReq{Mid: 0, MsgIds: []int64{msgAliceSystem1}}, model.ErrInvalidMid},
		{"mid 为负", &rpc.DeleteMessageReq{Mid: -91001, MsgIds: []int64{msgAliceSystem1}}, model.ErrInvalidMid},
		{"msg_ids 为 nil", &rpc.DeleteMessageReq{Mid: midAlice}, model.ErrMessageNotFound},
		{"msg_ids 为空切片", &rpc.DeleteMessageReq{Mid: midAlice, MsgIds: []int64{}}, model.ErrMessageNotFound},
		{"mid 与 msg_ids 同时非法：先校验 mid", &rpc.DeleteMessageReq{Mid: 0}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := deleteEnv(t)
			before := e.st.log.snapshot()

			reply, err := NewDeleteMessageLogic(context.Background(), e.svcCtx).DeleteMessage(tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：守卫拒绝仍返回响应体 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

// 越权/不存在：一条都不属于该收件人时必须报错，且连事务都不开。
func TestDeleteMessageRejectsMessagesThatAreNotTheCallers(t *testing.T) {
	e := deleteEnv(t)
	st := e.st
	before := st.log.snapshot()

	reply, err := NewDeleteMessageLogic(context.Background(), e.svcCtx).DeleteMessage(
		&rpc.DeleteMessageReq{Mid: midAlice, MsgIds: []int64{msgBobOnly, 999999}})

	wantErrIs(t, "越权删除", err, model.ErrMessageNotFound)
	if reply != nil {
		t.Fatalf("越权删除仍返回响应体 %+v", reply)
	}
	wantOps(t, "越权删除只查一次归属", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.CountOwned:%d/%s", midAlice, idsTag([]int64{msgBobOnly, 999999})),
	})
	wantAbsent(t, "越权删除不得开事务", st.log, "conn.TransactCtx")
	wantAbsent(t, "越权删除不得改状态", st.log, "user.SoftDeleteBatch")
	for _, msgID := range st.rowIDs(midBob) {
		wantEQ(t, fmt.Sprintf("Bob 的明细 %d", msgID), "DelState",
			st.getRow(t, midBob, msgID).DelState, model.DelStateNormal)
	}
}

// 正常路径：混合「自己的 + 不存在的」只删真实属于本人的行，
// 逐字段核对状态推进、计数联动与调用顺序。
func TestDeleteMessageSoftDeletesOnlyCallersRowsAndRefreshesCount(t *testing.T) {
	e := deleteEnv(t)
	st := e.st
	before := st.log.snapshot()

	reply, err := NewDeleteMessageLogic(context.Background(), e.svcCtx).DeleteMessage(
		&rpc.DeleteMessageReq{Mid: midAlice, MsgIds: []int64{msgAliceSystem1, msgShared, 999999}})
	wantNoErr(t, "删除站内信", err)

	// 点了 3 个 id，只有 2 个是 Alice 的：changed 取真实变更行数。
	wantEQ(t, "删除响应", "Changed", reply.Changed, int32(2))
	wantEQ(t, "删除响应", "UnreadTotal", reply.UnreadTotal, int64(1))

	for _, msgID := range []int64{msgAliceSystem1, msgShared} {
		row := st.getRow(t, midAlice, msgID)
		wantEQ(t, fmt.Sprintf("Alice 明细 %d", msgID), "DelState", row.DelState, model.DelStateDeleted)
		// 真实 SQL 是 SET del_state=1, read_state=2：删掉就不再提醒，一并置为已读。
		wantEQ(t, fmt.Sprintf("Alice 明细 %d", msgID), "ReadState", row.ReadState, model.ReadStateRead)
	}
	// 没点名的行保持原状。
	for _, msgID := range []int64{msgAliceSystem2, msgAliceRead} {
		row := st.getRow(t, midAlice, msgID)
		wantEQ(t, fmt.Sprintf("未点名的 Alice 明细 %d", msgID), "DelState", row.DelState, model.DelStateNormal)
	}

	wantOps(t, "删除的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.CountOwned:%d/%s", midAlice, idsTag([]int64{msgAliceSystem1, msgShared, 999999})),
		"conn.TransactCtx",
		fmt.Sprintf("user.SoftDeleteBatch:%d/%s/tx", midAlice, idsTag([]int64{msgAliceSystem1, msgShared, 999999})),
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/tx", midAlice),
		fmt.Sprintf("cache.Invalidate:%d", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("cache.Set:%d/1", midAlice),
	})
	wantMapEQ(t, "重算后的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 1, model.CategoryEngagement: 0, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantMapEQ(t, "回填后的缓存副本", "cache", st.cache.cached(midAlice), map[int32]int64{
		model.CategorySystem: 1, model.CategoryEngagement: 0, model.CategoryContent: 0, model.CategoryLive: 0,
	})
}

// 一发多收的消息：Alice 删除只影响她自己的收件行，Bob 的行与消息主体都必须原样。
func TestDeleteMessageDoesNotDisturbOtherRecipientsOrTheMessageBody(t *testing.T) {
	e := deleteEnv(t)
	st := e.st
	bobStatBefore := fmt.Sprint(st.statOf(midBob))
	bobCachedBefore := fmt.Sprint(st.cache.cached(midBob))

	reply, err := NewDeleteMessageLogic(context.Background(), e.svcCtx).DeleteMessage(
		&rpc.DeleteMessageReq{Mid: midAlice, MsgIds: []int64{msgShared}})
	wantNoErr(t, "删除共有消息", err)
	wantEQ(t, "删除共有消息", "Changed", reply.Changed, int32(1))

	bobRow := st.getRow(t, midBob, msgShared)
	wantEQ(t, "Bob 的收件行", "DelState", bobRow.DelState, model.DelStateNormal)
	wantEQ(t, "Bob 的收件行", "ReadState", bobRow.ReadState, model.ReadStateUnread)

	body := st.getMessage(t, msgShared)
	if body == nil {
		t.Fatalf("消息主体被删掉了：站内信是共享主体，用户侧删除不得硬删 inbox_message")
	}
	wantEQ(t, "消息主体", "State", body.State, model.MessageStateNormal)
	wantEQ(t, "消息主体", "Title", body.Title, "有人回复了你")
	wantEQ(t, "消息主体", "Content", body.Content, "正文")
	// Bob 的派生数据也没被动过。
	wantEQ(t, "Bob 的快照", "stat", fmt.Sprint(st.statOf(midBob)), bobStatBefore)
	wantEQ(t, "Bob 的缓存副本", "cache", fmt.Sprint(st.cache.cached(midBob)), bobCachedBefore)
	wantAbsent(t, "不得失效 Bob 的缓存", st.log, fmt.Sprintf("cache.Invalidate:%d", midBob))
	wantAbsent(t, "不得撤回消息主体", st.log, "msg.Withdraw")
}

// 幂等：重复删除 changed=0，不重算快照、不失效缓存，未读总数不得二次扣减。
func TestDeleteMessageIsIdempotentAndNeverDoubleDecrements(t *testing.T) {
	e := deleteEnv(t)
	st := e.st
	ids := []int64{msgAliceSystem1, msgAliceSystem2}

	first, err := NewDeleteMessageLogic(context.Background(), e.svcCtx).DeleteMessage(
		&rpc.DeleteMessageReq{Mid: midAlice, MsgIds: ids})
	wantNoErr(t, "首次删除", err)
	wantEQ(t, "首次删除", "Changed", first.Changed, int32(2))
	wantEQ(t, "首次删除", "UnreadTotal", first.UnreadTotal, int64(1))

	before := st.log.snapshot()
	second, err := NewDeleteMessageLogic(context.Background(), e.svcCtx).DeleteMessage(
		&rpc.DeleteMessageReq{Mid: midAlice, MsgIds: ids})
	wantNoErr(t, "重复删除", err)
	wantEQ(t, "重复删除", "Changed", second.Changed, int32(0))
	wantEQ(t, "重复删除", "UnreadTotal 不得二次扣减", second.UnreadTotal, int64(1))

	wantOps(t, "重复删除只做归属检查与条件 UPDATE", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.CountOwned:%d/%s", midAlice, idsTag(ids)),
		"conn.TransactCtx",
		fmt.Sprintf("user.SoftDeleteBatch:%d/%s/tx", midAlice, idsTag(ids)),
		fmt.Sprintf("cache.Get:%d", midAlice),
	})
	wantAbsentFrom(t, "重复删除不得重算快照", st.log, before, "user.CountUnreadByCategory")
	wantAbsentFrom(t, "重复删除不得覆盖快照", st.log, before, "stat.ReplaceByMid")
	wantAbsentFrom(t, "重复删除不得失效缓存", st.log, before, "cache.Invalidate")
	wantAbsentFrom(t, "重复删除不得回源", st.log, before, "stat.ListByMid")
}

// 删除已读消息：真实 SQL 只按 del_state 收窄，所以 changed=1；
// 未读数是从明细重算的，因此计数一分不少（不是靠 -1 维护的）。
func TestDeleteMessageOfReadMessageChangesRowButNotCount(t *testing.T) {
	e := deleteEnv(t)
	st := e.st
	before := st.log.snapshot()

	reply, err := NewDeleteMessageLogic(context.Background(), e.svcCtx).DeleteMessage(
		&rpc.DeleteMessageReq{Mid: midAlice, MsgIds: []int64{msgAliceRead}})
	wantNoErr(t, "删除已读消息", err)

	wantEQ(t, "删除已读消息", "Changed", reply.Changed, int32(1))
	wantEQ(t, "删除已读消息", "UnreadTotal 保持不变", reply.UnreadTotal, int64(3))
	row := st.getRow(t, midAlice, msgAliceRead)
	wantEQ(t, "已读消息的行", "DelState", row.DelState, model.DelStateDeleted)
	wantOpsPrefix(t, "删除已读消息的调用链", st.log.opsFrom(before), []string{
		"user.CountOwned:", "conn.TransactCtx", "user.SoftDeleteBatch:",
		"user.CountUnreadByCategory:", "stat.ReplaceByMid:", "cache.Invalidate:",
		"cache.Get:", "stat.ListByMid:", "cache.Set:91001/3",
	})
}

func TestDeleteMessagePropagatesDependencyErrors(t *testing.T) {
	errDB := errors.New("inbox-test-delete-down")

	cases := []struct {
		name    string
		fail    func(st *store)
		wantOps []string
	}{
		{
			name:    "归属检查失败",
			fail:    func(st *store) { st.users.failWith("CountOwned", errDB) },
			wantOps: []string{"user.CountOwned:"},
		},
		{
			name:    "软删除 UPDATE 失败",
			fail:    func(st *store) { st.users.failWith("SoftDeleteBatch", errDB) },
			wantOps: []string{"user.CountOwned:", "conn.TransactCtx", "user.SoftDeleteBatch:"},
		},
		{
			name:    "事务内重算未读失败",
			fail:    func(st *store) { st.users.failWith("CountUnreadByCategory", errDB) },
			wantOps: []string{"user.CountOwned:", "conn.TransactCtx", "user.SoftDeleteBatch:", "user.CountUnreadByCategory:"},
		},
		{
			name: "快照覆盖失败",
			fail: func(st *store) { st.stats.failWith("ReplaceByMid", errDB) },
			wantOps: []string{"user.CountOwned:", "conn.TransactCtx", "user.SoftDeleteBatch:",
				"user.CountUnreadByCategory:", "stat.ReplaceByMid:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := deleteEnv(t)
			st := e.st
			tc.fail(st)
			before := st.log.snapshot()

			reply, err := NewDeleteMessageLogic(context.Background(), e.svcCtx).DeleteMessage(
				&rpc.DeleteMessageReq{Mid: midAlice, MsgIds: []int64{msgAliceSystem1, msgShared}})

			wantErrIs(t, tc.name, err, errDB)
			if reply != nil {
				t.Fatalf("%s：下游失败仍返回响应体 %+v", tc.name, reply)
			}
			wantOpsPrefix(t, tc.name+" 的调用链", st.log.opsFrom(before), tc.wantOps)
			wantAbsent(t, tc.name+"：不得失效缓存", st.log, "cache.Invalidate")
			wantAbsent(t, tc.name+"：不得回填缓存", st.log, "cache.Set")
			// 事务回滚证据：两条被点名的行仍是未删除，快照仍是布景值。
			for _, msgID := range []int64{msgAliceSystem1, msgShared} {
				wantEQ(t, tc.name+"：事务回滚", "DelState",
					st.getRow(t, midAlice, msgID).DelState, model.DelStateNormal)
			}
			wantMapEQ(t, tc.name+"：快照未被半截改写", "stat", st.statOf(midAlice), map[int32]int64{
				model.CategorySystem: 2, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
			})
		})
	}
}

// 删除已提交、随后读权威计数失败：报错但保留提交，不返回 unread_total=0 的假成功。
func TestDeleteMessageReportsFailureWhenAuthoritativeCountReadBreaks(t *testing.T) {
	errStat := errors.New("inbox-test-stat-down")
	e := deleteEnv(t)
	st := e.st
	before := st.log.snapshot()
	st.stats.failWith("ListByMid", errStat)

	reply, err := NewDeleteMessageLogic(context.Background(), e.svcCtx).DeleteMessage(
		&rpc.DeleteMessageReq{Mid: midAlice, MsgIds: []int64{msgAliceSystem1}})

	wantErrIs(t, "删除后计数读失败", err, errStat)
	if reply != nil {
		t.Fatalf("计数读失败仍返回响应体 %+v", reply)
	}
	wantOpsPrefix(t, "调用链", st.log.opsFrom(before), []string{
		"user.CountOwned:", "conn.TransactCtx", "user.SoftDeleteBatch:",
		"user.CountUnreadByCategory:", "stat.ReplaceByMid:", "cache.Invalidate:",
		"cache.Get:", "stat.ListByMid:",
	})
	wantEQ(t, "已提交的行", "DelState", st.getRow(t, midAlice, msgAliceSystem1).DelState, model.DelStateDeleted)
	wantMapEQ(t, "已提交的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 1, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantEQ(t, "计数读失败不得留下缓存", "len(cache.data)", len(st.cache.data), 0)
}
