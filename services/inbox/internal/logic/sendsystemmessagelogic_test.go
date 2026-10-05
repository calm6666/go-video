package logic

// 系统站内信投递的主链路：一投多收、幂等重放、事务边界与故障传播（SendSystemMessage）。
//
// 要紧的结论：
//   - 消息主体 + 每个收件人的明细行 + 未读快照增量在同一个事务里，
//     事务内任何一步失败都必须整体回滚，且不得失效 Redis（否则用户会看到
//     「投递失败但计数已变」的半截状态）；
//   - 重复调用命中 uniq(idempotency_key) 时不得再写一行收件明细、不得二次加计数。
// 参数守卫与默认值补齐见 sendsystemmessage_test.go。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"
)

func TestSendSystemMessageDeliversOneRowPerRecipientInOneTransaction(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const key = "op-2026-09-22-verify"
	req := &rpc.SendSystemMessageReq{
		// 重复的 mid 必须在一次调用内去重，不能投两遍。
		Mids:           []int64{midAlice, midBob, midAlice},
		Title:          "账号已完成实名认证",
		Content:        "恭喜，你的实名认证已通过审核。",
		Category:       rpc.Category_CATEGORY_ENGAGEMENT,
		MsgType:        rpc.MsgType_MSG_TYPE_LINK,
		SenderMid:      88001,
		BizType:        "account",
		BizId:          "verify-20260922",
		Extra:          `{"jump":"hilibili://account/verify"}`,
		IdempotencyKey: "  " + key + "  ",
		Operator:       70001,
	}
	before := st.log.snapshot()

	reply, err := NewSendSystemMessageLogic(context.Background(), e.svcCtx).SendSystemMessage(req)
	wantNoErr(t, "投递系统站内信", err)

	wantEQ(t, "投递响应", "Delivered", reply.Delivered, int32(2))
	wantEQ(t, "投递响应", "Deduplicated", reply.Deduplicated, false)
	assertAround(t, "投递响应", "Ctime", reply.Ctime, nowUnix(), 5)

	stored := st.getMessage(t, reply.MsgId)
	if stored == nil {
		t.Fatalf("消息主体没落库，reply=%+v", reply)
	}
	wantEQ(t, "落库消息", "MsgID", stored.MsgID, reply.MsgId)
	wantEQ(t, "落库消息", "Ctime", stored.Ctime, reply.Ctime)
	wantEQ(t, "落库消息", "Category", stored.Category, model.CategoryEngagement)
	wantEQ(t, "落库消息", "MsgType", stored.MsgType, model.MsgTypeLink)
	wantEQ(t, "落库消息", "Title", stored.Title, req.Title)
	wantEQ(t, "落库消息", "Content", stored.Content, req.Content)
	wantEQ(t, "落库消息", "SenderMid", stored.SenderMid, int64(88001))
	wantEQ(t, "落库消息", "BizType", stored.BizType, "account")
	wantEQ(t, "落库消息", "BizID", stored.BizID, "verify-20260922")
	wantEQ(t, "落库消息", "Extra", stored.Extra, req.Extra)
	wantEQ(t, "落库消息", "State", stored.State, model.MessageStateNormal)
	// 幂等键服务端 trim，不代造。
	wantEQ(t, "落库消息", "IdempotencyKey", stored.IdempotencyKey, key)
	wantEQ(t, "落库消息", "Operator", stored.Operator, int64(70001))

	for _, mid := range []int64{midAlice, midBob} {
		row := st.getRow(t, mid, reply.MsgId)
		if row == nil {
			t.Fatalf("mid=%d 没有收到消息 %d（该用户现有收件行 %v）", mid, reply.MsgId, st.rowIDs(mid))
		}
		wantEQ(t, fmt.Sprintf("收件行 %d", mid), "MsgID", row.MsgID, reply.MsgId)
		wantEQ(t, fmt.Sprintf("收件行 %d", mid), "Mid", row.Mid, mid)
		wantEQ(t, fmt.Sprintf("收件行 %d", mid), "Category", row.Category, model.CategoryEngagement)
		wantEQ(t, fmt.Sprintf("收件行 %d", mid), "ReadState", row.ReadState, model.ReadStateUnread)
		wantEQ(t, fmt.Sprintf("收件行 %d", mid), "DelState", row.DelState, model.DelStateNormal)
		wantEQ(t, fmt.Sprintf("收件行 %d", mid), "Ctime", row.Ctime, reply.Ctime)
		// 快照走增量（IncrBy），只写被投递的那个分类，不做整表覆盖。
		wantEQ(t, fmt.Sprintf("未读快照 %d", mid), "stat", fmt.Sprint(st.statOf(mid)),
			fmt.Sprint(map[int32]int64{model.CategoryEngagement: 1}))
	}
	wantEQ(t, "去重后的收件行数", "alice 收件行数", len(st.rowIDs(midAlice)), 1)

	wantOps(t, "投递的调用链", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		"msg.InsertIdempotent:" + key + "/tx",
		fmt.Sprintf("user.InsertIdempotent:%d/%d/tx", midAlice, reply.MsgId),
		fmt.Sprintf("stat.IncrBy:%d/%d/1/tx", midAlice, model.CategoryEngagement),
		fmt.Sprintf("user.InsertIdempotent:%d/%d/tx", midBob, reply.MsgId),
		fmt.Sprintf("stat.IncrBy:%d/%d/1/tx", midBob, model.CategoryEngagement),
		fmt.Sprintf("cache.Invalidate:%d+%d", midAlice, midBob),
	})
}

// 幂等重放：同一把 key 第二次调用不得再写收件行、不得二次加计数，
// 必须原样返回首次的 msg_id / ctime，并且不触碰缓存。
func TestSendSystemMessageIsIdempotentOnReplay(t *testing.T) {
	e := newEnv(t)
	st := e.st
	req := &rpc.SendSystemMessageReq{
		Mids: []int64{midAlice, midBob}, Title: "直播已开始", Content: "你关注的主播开播了",
		Category:       rpc.Category_CATEGORY_LIVE,
		IdempotencyKey: "live-started-77",
	}

	first, err := NewSendSystemMessageLogic(context.Background(), e.svcCtx).SendSystemMessage(req)
	wantNoErr(t, "首次投递", err)
	wantEQ(t, "首次投递", "Delivered", first.Delivered, int32(2))
	firstStat := st.statOf(midAlice)
	firstRows := len(st.tb.rows)

	before := st.log.snapshot()
	second, err := NewSendSystemMessageLogic(context.Background(), e.svcCtx).SendSystemMessage(req)
	wantNoErr(t, "重放投递", err)

	wantEQ(t, "重放", "Deduplicated", second.Deduplicated, true)
	wantEQ(t, "重放", "Delivered", second.Delivered, int32(0))
	wantEQ(t, "重放", "MsgId 与首次一致", second.MsgId, first.MsgId)
	wantEQ(t, "重放", "Ctime 与首次一致", second.Ctime, first.Ctime)
	wantOps(t, "重放只读幂等键", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		"msg.InsertIdempotent:live-started-77/tx",
	})
	wantAbsentFrom(t, "重放不得再投收件行", st.log, before, "user.InsertIdempotent")
	wantAbsentFrom(t, "重放不得再加计数", st.log, before, "stat.IncrBy")
	wantAbsentFrom(t, "重放不得失效缓存", st.log, before, "cache.Invalidate")
	wantEQ(t, "重放后的收件行总数", "len(rows)", len(st.tb.rows), firstRows)
	wantEQ(t, "重放后的快照", "alice 快照", fmt.Sprint(st.statOf(midAlice)), fmt.Sprint(firstStat))

	// 换一把 key 投同一批人：是新消息，必须各多一行、各多加一次计数。
	req.IdempotencyKey = "live-started-78"
	third, err := NewSendSystemMessageLogic(context.Background(), e.svcCtx).SendSystemMessage(req)
	wantNoErr(t, "换 key 再投", err)
	wantEQ(t, "换 key", "Deduplicated", third.Deduplicated, false)
	wantEQ(t, "换 key", "Delivered", third.Delivered, int32(2))
	wantEQ(t, "换 key", "alice 收件行数", len(st.rowIDs(midAlice)), 2)
	wantEQ(t, "换 key", "alice 快照", st.statOf(midAlice)[model.CategoryLive], int64(2))
}

// 一次调用投满上限：三条明细、三次快照增量，缓存失效只发一次并带上全部收件人。
func TestSendSystemMessageInvalidatesAllRecipientsInOneCall(t *testing.T) {
	e := newEnv(t)
	st := e.st
	before := st.log.snapshot()

	reply, err := NewSendSystemMessageLogic(context.Background(), e.svcCtx).SendSystemMessage(
		&rpc.SendSystemMessageReq{
			Mids: []int64{midAlice, midBob, midCarol}, Title: "系统升级", Content: "明早 3 点停机",
			IdempotencyKey: "upgrade-3-users",
		})
	wantNoErr(t, "投满上限", err)
	wantEQ(t, "投满上限", "Delivered", reply.Delivered, int32(3))

	for _, mid := range []int64{midAlice, midBob, midCarol} {
		wantEQ(t, fmt.Sprintf("收件行 %d", mid), "已落库", st.getRow(t, mid, reply.MsgId) != nil, true)
		wantEQ(t, fmt.Sprintf("快照 %d", mid), "system 未读", st.statOf(mid)[model.CategorySystem], int64(1))
	}
	wantOps(t, "整批投递的调用链", st.log.opsFrom(before), []string{
		"conn.TransactCtx",
		"msg.InsertIdempotent:upgrade-3-users/tx",
		fmt.Sprintf("user.InsertIdempotent:%d/%d/tx", midAlice, reply.MsgId),
		"stat.IncrBy:91001/1/1/tx",
		fmt.Sprintf("user.InsertIdempotent:%d/%d/tx", midBob, reply.MsgId),
		"stat.IncrBy:91002/1/1/tx",
		fmt.Sprintf("user.InsertIdempotent:%d/%d/tx", midCarol, reply.MsgId),
		"stat.IncrBy:91003/1/1/tx",
		fmt.Sprintf("cache.Invalidate:%d+%d+%d", midAlice, midBob, midCarol),
	})
}

func TestSendSystemMessagePropagatesDependencyErrors(t *testing.T) {
	errInjected := errors.New("inbox-test-deliver-down")

	cases := []struct {
		name    string
		fail    func(st *store)
		wantOps []string
	}{
		{
			name:    "写消息主体失败",
			fail:    func(st *store) { st.msgs.failWith("InsertIdempotent", errInjected) },
			wantOps: []string{"conn.TransactCtx", "msg.InsertIdempotent:err-key/tx"},
		},
		{
			name:    "写收件明细失败",
			fail:    func(st *store) { st.users.failWith("InsertIdempotent", errInjected) },
			wantOps: []string{"conn.TransactCtx", "msg.InsertIdempotent:err-key/tx", "user.InsertIdempotent:"},
		},
		{
			name:    "未读快照增量失败",
			fail:    func(st *store) { st.stats.failWith("IncrBy", errInjected) },
			wantOps: []string{"conn.TransactCtx", "msg.InsertIdempotent:err-key/tx", "user.InsertIdempotent:", "stat.IncrBy:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			tc.fail(st)
			before := st.log.snapshot()

			reply, err := NewSendSystemMessageLogic(context.Background(), e.svcCtx).SendSystemMessage(
				&rpc.SendSystemMessageReq{
					Mids: []int64{midAlice, midBob}, Title: "系统维护", Content: "今晚 02:00 停服",
					IdempotencyKey: "err-key",
				})

			if reply != nil {
				t.Fatalf("%s：下游失败仍返回响应体 %+v", tc.name, reply)
			}
			wantErrIs(t, tc.name, err, errInjected)
			got := st.log.opsFrom(before)
			if len(got) != len(tc.wantOps) {
				t.Fatalf("%s：调用序列 = [%s], want %d 项", tc.name, strings.Join(got, " → "), len(tc.wantOps))
			}
			for i, prefix := range tc.wantOps {
				if !strings.HasPrefix(got[i], prefix) {
					t.Errorf("%s：第 %d 次调用 = %s, want 前缀 %s（完整序列 [%s]）",
						tc.name, i+1, got[i], prefix, strings.Join(got, " → "))
				}
			}
			// 半截副作用：事务必须整体回滚，缓存一个字节都不许动。
			wantAbsent(t, tc.name+"：不得失效缓存", st.log, "cache.Invalidate")
			wantEQ(t, tc.name+"：消息主体未落库", "len(msgs)", len(st.tb.msgs), 0)
			wantEQ(t, tc.name+"：收件明细未落库", "len(rows)", len(st.tb.rows), 0)
			wantEQ(t, tc.name+"：快照未变化", "len(stat)", len(st.tb.stat), 0)
			wantEQ(t, tc.name+"：缓存未写入", "len(cache.data)", len(st.cache.data), 0)
		})
	}
}
