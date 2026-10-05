package logic

// sessionread_logic_test.go 覆盖场次维度的两个读方法：ListSessions / GetSession。
//
// 这两个方法的口径核心是「游标而不是偏移」：
//   - ListSessions 只用 session_id 游标翻页（新场次不断插入，offset 必然重复/漏项），
//     所以它没有 total 也不数总数；游标解析失败必须报错，不能退化成「当作第一页」。
//   - GetSession 的 session_id 与 room_id 是二选一，session_id 优先；
//     按 room_id 取「最近第 offset+1 场」，负 offset 在触库前就要拒。
//     查不到一律 ErrSessionNotFound —— 回零值 SessionInfo 等于谎报「有一场没状态的直播」。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

// --- ListSessions ---

// TestListSessionsFirstPageIsOrderedBySessionIDDesc 锁「最近一场在前」与 LIMIT 送达。
func TestListSessionsFirstPageIsOrderedBySessionIDDesc(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	st.seedSession(baseSession(5003, 101, 9001, model.SessionStateEnded))
	st.seedSession(baseSession(5002, 101, 9001, model.SessionStateEnded))
	// 别的房间的场次不得混进来。
	st.seedSession(baseSession(5004, 102, 9002, model.SessionStateEnded))
	logic := NewListSessionsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, PageSize: 3})

	wantNoErr(t, "ListSessions", err)
	wantInt64sEQ(t, "session_id 倒序", "sessions", sessionIDs(reply.GetSessions()), []int64{5003, 5002, 5001})
	// 游标只在「页满」时给出：3 行 = 页大小 3，说明后面可能还有，必须给游标。
	wantEQ(t, "满页要给游标", "next_cursor", reply.GetNextCursor(), "5001")
	wantDeepEQ(t, "ListSessions 条件", "query", st.sessions.listQueries[0], model.SessionListModel{
		RoomID: 101, Limit: 3,
	})
	// 游标分页不数总数：一次读只有一条 SQL。
	wantSeq(t, "ListSessions 只有一条 List", st.log, 0, "live_session.List:101")
	wantNoCallAfter(t, "ListSessions", st.log, 1)
	wantNoDirectSQL(t, "ListSessions", st.conn)
}

// TestListSessionsCursorWalksBackAndStops 走完整个游标链：
// 第二页以第一页最后一条的 session_id 为游标，且**该条本身不得重复出现**（SQL 是 `<` 而不是 `<=`）。
func TestListSessionsCursorWalksBackAndStops(t *testing.T) {
	st := newStore()
	for _, id := range []int64{5001, 5002, 5003, 5004, 5005} {
		st.seedSession(baseSession(id, 101, 9001, model.SessionStateEnded))
	}
	logic := NewListSessionsLogic(context.Background(), st.svcCtx())

	first, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, PageSize: 2})
	wantNoErr(t, "第一页", err)
	wantInt64sEQ(t, "第一页", "sessions", sessionIDs(first.GetSessions()), []int64{5005, 5004})
	wantEQ(t, "第一页游标", "next_cursor", first.GetNextCursor(), "5004")

	second, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, PageSize: 2, Cursor: first.GetNextCursor()})
	wantNoErr(t, "第二页", err)
	// 5004 已经在第一页出现过了：游标是开区间，不能重复。
	wantInt64sEQ(t, "第二页", "sessions", sessionIDs(second.GetSessions()), []int64{5003, 5002})
	wantEQ(t, "游标送达 model", "BeforeSessionID", st.sessions.listQueries[1].BeforeSessionID, int64(5004))

	third, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, PageSize: 2, Cursor: second.GetNextCursor()})
	wantNoErr(t, "第三页", err)
	wantInt64sEQ(t, "第三页只剩一条", "sessions", sessionIDs(third.GetSessions()), []int64{5001})
	// 不满页就是到底：此时必须回空游标，否则客户端会拿旧游标原地打转。
	wantEQ(t, "到底要给空游标", "next_cursor", third.GetNextCursor(), "")
}

// TestListSessionsCursorOnExactMultipleNeedsOneMoreRound 数据条数正好是页大小整数倍时，
// 最后一页仍会给游标（无法在不做 COUNT 的前提下知道后面还有没有），
// 下一轮回空列表 + 空游标。这里锁「不谎称到底」这个保守口径。
func TestListSessionsCursorOnExactMultipleNeedsOneMoreRound(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	st.seedSession(baseSession(5002, 101, 9001, model.SessionStateEnded))
	logic := NewListSessionsLogic(context.Background(), st.svcCtx())

	first, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, PageSize: 2})
	wantNoErr(t, "满页", err)
	wantEQ(t, "满页仍给游标", "next_cursor", first.GetNextCursor(), "5001")

	second, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, PageSize: 2, Cursor: "5001"})
	wantNoErr(t, "收尾页", err)
	wantEQ(t, "收尾页", "sessions 条数", len(second.GetSessions()), 0)
	wantEQ(t, "收尾页", "next_cursor", second.GetNextCursor(), "")
}

// TestListSessionsRejectsUnusableCursorInsteadOfRewinding 游标不可用时必须报错：
// 退化成「当作第一页」会让客户端以为自己翻到了开头而重复渲染整页。
func TestListSessionsRejectsUnusableCursorInsteadOfRewinding(t *testing.T) {
	cases := []struct{ name, cursor string }{
		{"非数字", "abc"},
		{"负数", "-5"},
		{"零", "0"},
		{"溢出成负数", "99999999999999999999"},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
		logic := NewListSessionsLogic(context.Background(), st.svcCtx())

		reply, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, Cursor: tc.cursor})

		wantErrIs(t, "ListSessions(游标 "+tc.name+")", err, model.ErrCursorInvalid)
		wantEQ(t, "ListSessions(游标 "+tc.name+")", "reply", reply, nil)
		wantNoCallAfter(t, "ListSessions(游标 "+tc.name+")", st.log, 0)
	}

	// 空白游标等价于「没有游标」= 第一页，这是 TrimSpace 的既有口径。
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	reply, err := NewListSessionsLogic(context.Background(), st.svcCtx()).
		ListSessions(&rpc.ListSessionsReq{RoomId: 101, Cursor: "   "})
	wantNoErr(t, "ListSessions(空白游标)", err)
	wantEQ(t, "空白游标当第一页", "sessions 条数", len(reply.GetSessions()), 1)
	wantEQ(t, "空白游标", "BeforeSessionID", st.sessions.listQueries[0].BeforeSessionID, int64(0))
}

func TestListSessionsRequiresRoomIDBeforeAnyQuery(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListSessionsReq
	}{
		{"nil 请求", nil},
		{"room_id 为 0", &rpc.ListSessionsReq{}},
		{"room_id 为负", &rpc.ListSessionsReq{RoomId: -1}},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
		logic := NewListSessionsLogic(context.Background(), st.svcCtx())

		reply, err := logic.ListSessions(tc.in)

		wantErrIs(t, "ListSessions("+tc.name+")", err, model.ErrInvalidRoomID)
		wantEQ(t, "ListSessions("+tc.name+")", "reply", reply, nil)
		wantNoCallAfter(t, "ListSessions("+tc.name+")", st.log, 0)
	}
}

// TestListSessionsMidFilterAndNegative 锁 mid 的三档：0 不过滤、>0 过滤、<0 拒绝。
// 拒绝必须在触库前，否则会退化成「查全房间历史」。
func TestListSessionsMidFilterAndNegative(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	st.seedSession(baseSession(5002, 101, 9002, model.SessionStateEnded))
	logic := NewListSessionsLogic(context.Background(), st.svcCtx())

	_, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, Mid: -1})
	wantErrIs(t, "ListSessions(mid 为负)", err, model.ErrInvalidMid)
	wantNoCallAfter(t, "ListSessions(mid 为负)", st.log, 0)

	mine, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, Mid: 9002})
	wantNoErr(t, "ListSessions(mid=9002)", err)
	wantInt64sEQ(t, "按主播", "sessions", sessionIDs(mine.GetSessions()), []int64{5002})
	wantEQ(t, "按主播", "条件 Mid", st.sessions.listQueries[0].Mid, int64(9002))

	all, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101})
	wantNoErr(t, "ListSessions(mid 缺省)", err)
	wantEQ(t, "缺省不过滤", "total 条数", len(all.GetSessions()), 2)
	wantEQ(t, "缺省不过滤", "条件 Mid", st.sessions.listQueries[1].Mid, int64(0))
}

// TestListSessionsStateFilterRejectsUndefinedValue 与 ListAnchors 同理：
// 未定义的枚举取值不等于「不过滤」。
func TestListSessionsStateFilterRejectsUndefinedValue(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	st.seedSession(baseSession(5002, 101, 9001, model.SessionStateLiving))
	logic := NewListSessionsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, State: rpc.SessionState(9)})
	wantErrIs(t, "ListSessions(state=9)", err, model.ErrInvalidSessionTransition)
	wantEQ(t, "ListSessions(state=9)", "reply", reply, nil)
	wantNoCallAfter(t, "ListSessions(state=9)", st.log, 0)

	ended, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, State: rpc.SessionState_SESSION_STATE_ENDED})
	wantNoErr(t, "ListSessions(state=ENDED)", err)
	wantInt64sEQ(t, "只看正常下播", "sessions", sessionIDs(ended.GetSessions()), []int64{5001})
	wantEQ(t, "只看正常下播", "条件 State", st.sessions.listQueries[0].State, int32(model.SessionStateEnded))

	// 终态之外的「直播中」也要能单独读出来（回放页只列已结束，列表页可能只看进行中）。
	living, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, State: rpc.SessionState_SESSION_STATE_LIVING})
	wantNoErr(t, "ListSessions(state=LIVING)", err)
	wantInt64sEQ(t, "只看直播中", "sessions", sessionIDs(living.GetSessions()), []int64{5002})
}

// TestListSessionsValidatesBeforeTouchingStore 锁守卫顺序：
// room_id → mid → state → page_size → cursor。四样都错时按这个次序报第一个。
func TestListSessionsValidatesBeforeTouchingStore(t *testing.T) {
	st := newStore()
	logic := NewListSessionsLogic(context.Background(), st.svcCtx())

	_, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 0, Mid: -1, State: rpc.SessionState(9), PageSize: 999, Cursor: "x"})
	wantErrIs(t, "顺序 1", err, model.ErrInvalidRoomID)
	_, err = logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, Mid: -1, State: rpc.SessionState(9), PageSize: 999, Cursor: "x"})
	wantErrIs(t, "顺序 2", err, model.ErrInvalidMid)
	_, err = logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, State: rpc.SessionState(9), PageSize: 999, Cursor: "x"})
	wantErrIs(t, "顺序 3", err, model.ErrInvalidSessionTransition)
	_, err = logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, PageSize: 101, Cursor: "x"})
	wantErrIs(t, "顺序 4（页大小上限 100）", err, model.ErrPageSizeTooLarge)
	_, err = logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101, PageSize: 50, Cursor: "x"})
	wantErrIs(t, "顺序 5", err, model.ErrCursorInvalid)
	wantNoCallAfter(t, "ListSessions(入参守卫)", st.log, 0)
}

// TestListSessionsProjectsEveryField 逐字段核对场次投影，重点是
// record_id / record_asset_id / record_aid 只作为引用下发（播放地址由 live-media 签发）。
func TestListSessionsProjectsEveryField(t *testing.T) {
	st := newStore()
	row := st.seedSession(&model.LiveSession{
		SessionID: 5001, RoomID: 101, Mid: 9001, State: model.SessionStateEnded,
		TitleSnapshot: "那场夜聊", AreaIDSnapshot: 7002, StreamID: "stream-5001",
		StartedAt: 1000, EndedAt: 2000, DurationSeconds: 1000,
		EndReason: model.EndReasonBanned, LastStreamSeq: 42,
		ReplayState: model.ReplayStateAvailable, RecordID: 8801,
		RecordAssetID: 9901, RecordAid: 7701, ModerationTaskID: 6601,
		Ctime: 111, Mtime: 222,
	})
	logic := NewListSessionsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101})

	wantNoErr(t, "ListSessions(投影)", err)
	if len(reply.GetSessions()) != 1 {
		t.Fatalf("sessions 条数 = %d, want 1", len(reply.GetSessions()))
	}
	info := reply.GetSessions()[0]
	wantEQ(t, "投影", "session_id", info.GetSessionId(), row.SessionID)
	wantEQ(t, "投影", "room_id", info.GetRoomId(), row.RoomID)
	wantEQ(t, "投影", "mid", info.GetMid(), row.Mid)
	wantEQ(t, "投影", "state", info.GetState(), rpc.SessionState_SESSION_STATE_ENDED)
	wantEQ(t, "投影", "title_snapshot", info.GetTitleSnapshot(), "那场夜聊")
	wantEQ(t, "投影", "area_id_snapshot", info.GetAreaIdSnapshot(), int64(7002))
	wantEQ(t, "投影", "stream_id", info.GetStreamId(), "stream-5001")
	wantEQ(t, "投影", "started_at", info.GetStartedAt(), row.StartedAt)
	wantEQ(t, "投影", "ended_at", info.GetEndedAt(), row.EndedAt)
	wantEQ(t, "投影", "duration_seconds", info.GetDurationSeconds(), row.DurationSeconds)
	wantEQ(t, "投影", "end_reason", info.GetEndReason(), rpc.EndReason_END_REASON_BANNED)
	wantEQ(t, "投影", "last_stream_seq", info.GetLastStreamSeq(), row.LastStreamSeq)
	wantEQ(t, "投影", "replay_state", info.GetReplayState(), rpc.ReplayState_REPLAY_STATE_AVAILABLE)
	wantEQ(t, "投影：只给引用不给地址", "record_id", info.GetRecordId(), row.RecordID)
	wantEQ(t, "投影：只给引用不给地址", "record_asset_id", info.GetRecordAssetId(), row.RecordAssetID)
	wantEQ(t, "投影：只给引用不给地址", "record_aid", info.GetRecordAid(), row.RecordAid)
	wantEQ(t, "投影", "moderation_task_id", info.GetModerationTaskId(), row.ModerationTaskID)
	wantEQ(t, "投影", "ctime", info.GetCtime(), int64(111))
	wantEQ(t, "投影", "mtime", info.GetMtime(), int64(222))
}

// TestListSessionsEmptyResultKeepsNilCursor 空历史是正常状态（新开播的房间），
// 必须回空列表 + 空游标，而不是 nil reply 或错误。
func TestListSessionsEmptyResultKeepsNilCursor(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 102, 9001, model.SessionStateEnded))
	logic := NewListSessionsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101})

	wantNoErr(t, "ListSessions(无历史)", err)
	if reply == nil {
		t.Fatal("空结果必须回 reply")
	}
	wantEQ(t, "无历史", "sessions 条数", len(reply.GetSessions()), 0)
	wantEQ(t, "无历史", "next_cursor", reply.GetNextCursor(), "")
	wantSeq(t, "无历史仍要查一次", st.log, 0, "live_session.List:101")
	// 场次列表不校验房间存在性，也不读绑定表。
	wantMethodCount(t, "不得回查房间表", st.log, "live_room.FindOne", 0)
	wantMethodCount(t, "不得读绑定表", st.log, "live_room_anchor.ListRoomsByMid", 0)
}

func TestListSessionsPropagatesStoreError(t *testing.T) {
	st := newStore()
	st.sessions.failWith("List", errors.New("db down"))
	logic := NewListSessionsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListSessions(&rpc.ListSessionsReq{RoomId: 101})

	wantErrContains(t, "ListSessions(故障)", err, "db down")
	wantEQ(t, "ListSessions(故障)", "reply", reply, nil)
	if errors.Is(err, model.ErrSessionNotFound) {
		t.Fatal("store 故障不得被翻译成「场次不存在」")
	}
}

// --- GetSession ---

func TestGetSessionBySessionIDProjectsRow(t *testing.T) {
	st := newStore()
	row := st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	logic := NewGetSessionLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetSession(&rpc.GetSessionReq{SessionId: 5001})

	wantNoErr(t, "GetSession(session_id)", err)
	info := reply.GetSession()
	if info == nil {
		t.Fatalf("GetSession 返回空 Session：reply=%#v", reply)
	}
	wantEQ(t, "投影", "session_id", info.GetSessionId(), row.SessionID)
	wantEQ(t, "投影", "room_id", info.GetRoomId(), row.RoomID)
	wantEQ(t, "投影", "state", info.GetState(), rpc.SessionState_SESSION_STATE_ENDED)
	wantEQ(t, "投影", "ended_at", info.GetEndedAt(), row.EndedAt)
	wantEQ(t, "投影", "duration_seconds", info.GetDurationSeconds(), row.DurationSeconds)
	wantSeq(t, "按主键一次读", st.log, 0, "live_session.FindOne:5001")
	wantNoDirectSQL(t, "GetSession", st.conn)
}

// TestGetSessionIgnoresRoomIDWhenSessionIDGiven 两个键都给时走 session_id：
// 按 room_id 再查一次会给出「另一个答案」（最近一场 ≠ 指定那一场）。
func TestGetSessionIgnoresRoomIDWhenSessionIDGiven(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	st.seedSession(baseSession(5002, 101, 9001, model.SessionStateEnded))
	logic := NewGetSessionLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetSession(&rpc.GetSessionReq{SessionId: 5001, RoomId: 101, Offset: 0})

	wantNoErr(t, "GetSession(两个键都给)", err)
	wantEQ(t, "session_id 优先", "session_id", reply.GetSession().GetSessionId(), int64(5001))
	wantSeq(t, "两个键都给", st.log, 0, "live_session.FindOne:5001")
	wantMethodCount(t, "不得再按 room_id 找最近一场", st.log, "live_session.FindLatest", 0)
}

// TestGetSessionByRoomIDOffsetWalksBackwards 按 room_id 读时 offset 是「从最近一场往前数」，
// 游标语义与 ListSessions 一致（DESC + OFFSET），offset 原样送达 model。
func TestGetSessionByRoomIDOffsetWalksBackwards(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	st.seedSession(baseSession(5002, 101, 9001, model.SessionStateEnded))
	st.seedSession(baseSession(5003, 101, 9001, model.SessionStateEnded))
	// 别的房间的场次不得参与排序。
	st.seedSession(baseSession(5004, 102, 9002, model.SessionStateEnded))
	logic := NewGetSessionLogic(context.Background(), st.svcCtx())

	for _, tc := range []struct {
		offset int32
		want   int64
	}{
		{0, 5003},
		{1, 5002},
		{2, 5001},
	} {
		reply, err := logic.GetSession(&rpc.GetSessionReq{RoomId: 101, Offset: tc.offset})
		wantNoErr(t, "GetSession(offset)", err)
		wantEQ(t, "offset 取到的场次", "session_id", reply.GetSession().GetSessionId(), tc.want)
		wantSeq(t, "offset 送达", st.log, int(tc.offset),
			fmt.Sprintf("live_session.FindLatest:101/%d", tc.offset))
	}
	wantMethodCount(t, "三次都只走 FindLatest", st.log, "live_session.FindOne", 0)
}

// TestGetSessionOffsetPastHistoryIsNotFound 超出历史深度必须报 ErrSessionNotFound，
// 不能回零值，也不能回「最近一场」（那会让客户端把第 99 场看成第 1 场）。
func TestGetSessionOffsetPastHistoryIsNotFound(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	logic := NewGetSessionLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetSession(&rpc.GetSessionReq{RoomId: 101, Offset: 5})

	wantErrIs(t, "GetSession(offset 越界)", err, model.ErrSessionNotFound)
	wantEQ(t, "GetSession(offset 越界)", "reply", reply, nil)
	wantSeq(t, "offset 越界", st.log, 0, "live_session.FindLatest:101/5")
}

// TestGetSessionRejectsNegativeOffsetBeforeQuery 负 offset 在触库前拒绝：
// model 侧的 FindLatest 会把负数夹成 0，若 logic 不拦就会把「上一页」变成「第一页」。
func TestGetSessionRejectsNegativeOffsetBeforeQuery(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	logic := NewGetSessionLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetSession(&rpc.GetSessionReq{RoomId: 101, Offset: -1})

	wantErrIs(t, "GetSession(负 offset)", err, model.ErrOffsetInvalid)
	wantEQ(t, "GetSession(负 offset)", "reply", reply, nil)
	wantNoCallAfter(t, "GetSession(负 offset)", st.log, 0)
}

func TestGetSessionMissingKeysRejectedWithoutQuery(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.GetSessionReq
	}{
		{"nil 请求", nil},
		{"两个键都是 0", &rpc.GetSessionReq{}},
		{"session_id 为负且无 room_id", &rpc.GetSessionReq{SessionId: -7}},
		{"只有 room_id 为负", &rpc.GetSessionReq{RoomId: -7}},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
		logic := NewGetSessionLogic(context.Background(), st.svcCtx())

		reply, err := logic.GetSession(tc.in)

		// 注意：两个键都不合法时报的是 ErrInvalidSessionID（switch 的 default 分支），
		// 不是 ErrInvalidRoomID —— 这是当前真实行为，用例按现状锁定。
		wantErrIs(t, "GetSession("+tc.name+")", err, model.ErrInvalidSessionID)
		wantEQ(t, "GetSession("+tc.name+")", "reply", reply, nil)
		wantNoCallAfter(t, "GetSession("+tc.name+")", st.log, 0)
	}
}

func TestGetSessionMissingRowIsNotFoundNotZeroReply(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	logic := NewGetSessionLogic(context.Background(), st.svcCtx())

	byID, err := logic.GetSession(&rpc.GetSessionReq{SessionId: 404})
	wantErrIs(t, "GetSession(不存在 session_id)", err, model.ErrSessionNotFound)
	wantEQ(t, "GetSession(不存在 session_id)", "reply", byID, nil)

	byRoom, err := logic.GetSession(&rpc.GetSessionReq{RoomId: 999})
	wantErrIs(t, "GetSession(无历史的房间)", err, model.ErrSessionNotFound)
	wantEQ(t, "GetSession(无历史的房间)", "reply", byRoom, nil)
	wantSeq(t, "两次独立读", st.log, 0, "live_session.FindOne:404", "live_session.FindLatest:999/0")
}

func TestGetSessionPropagatesStoreErrorsFromBothBranches(t *testing.T) {
	st := newStore()
	st.sessions.failWith("FindOne", errors.New("db down"))
	reply, err := NewGetSessionLogic(context.Background(), st.svcCtx()).
		GetSession(&rpc.GetSessionReq{SessionId: 5001})
	wantErrContains(t, "GetSession(FindOne 故障)", err, "db down")
	wantEQ(t, "GetSession(FindOne 故障)", "reply", reply, nil)
	if errors.Is(err, model.ErrSessionNotFound) {
		t.Fatal("store 故障不得被翻译成「场次不存在」")
	}

	st2 := newStore()
	st2.sessions.failWith("FindLatest", errors.New("latest boom"))
	reply2, err := NewGetSessionLogic(context.Background(), st2.svcCtx()).
		GetSession(&rpc.GetSessionReq{RoomId: 101})
	wantErrContains(t, "GetSession(FindLatest 故障)", err, "latest boom")
	wantEQ(t, "GetSession(FindLatest 故障)", "reply", reply2, nil)
}

// TestGetSessionLiveRowHasNoTerminalFields 进行中场次的 ended_at/duration/end_reason
// 必须保持 0（未发生），不能被投影填成「刚结束」——终端据此决定要不要显示回放入口。
func TestGetSessionLiveRowHasNoTerminalFields(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateLiving))
	logic := NewGetSessionLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetSession(&rpc.GetSessionReq{SessionId: 5001})

	wantNoErr(t, "GetSession(进行中)", err)
	info := reply.GetSession()
	wantEQ(t, "进行中", "state", info.GetState(), rpc.SessionState_SESSION_STATE_LIVING)
	wantEQ(t, "未结束不能有 ended_at", "ended_at", info.GetEndedAt(), int64(0))
	wantEQ(t, "未结束不能有时长", "duration_seconds", info.GetDurationSeconds(), int64(0))
	wantEQ(t, "未结束不能有结束原因", "end_reason", info.GetEndReason(), rpc.EndReason_END_REASON_UNSPECIFIED)
	wantEQ(t, "未生成回放", "replay_state", info.GetReplayState(), rpc.ReplayState_REPLAY_STATE_NONE)
	wantEQ(t, "未生成回放", "record_id", info.GetRecordId(), int64(0))
}

// TestGetSessionDoesNotCheckRoomExistence 场次记录本身就是事实来源：
// 房间关闭甚至删除后仍要能读到历史场次（回放页依赖这一点），
// 所以本方法不得回查 live_room，也不得因为房间不在而报 ErrRoomNotFound。
func TestGetSessionDoesNotCheckRoomExistence(t *testing.T) {
	st := newStore()
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateEnded))
	logic := NewGetSessionLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetSession(&rpc.GetSessionReq{SessionId: 5001})

	wantNoErr(t, "GetSession(房间不存在)", err)
	wantEQ(t, "房间不存在也能读场次", "session_id", reply.GetSession().GetSessionId(), int64(5001))
	wantMethodCount(t, "不得回查房间表", st.log, "live_room.FindOne", 0)
	wantCount(t, "不得读房间的任何表", st.log, "live_room.", 0)
}
