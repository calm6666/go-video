package logic

// startlive_logic_test.go 覆盖写侧方法 StartLive（startlivelogic.go）。
//
// 锁的结论（对应任务书的 1~6 条）：
//  1. request_id 三态：首次受理 / 命中重放（逐字段回原值 + replayed=true + 零新写，
//     用 callLog 起点切片与 st.counts() 双重锁死）/ 键被别的 RPC 用过必须报错。
//  2. 状态机：StartLive 的合法 from 态**不写死**，而是由 model.RoomTransitionTargets
//     推出「能进 LIVING 的态集合」，再要求 StartLive 恰好只接受这个集合（表驱动整跑 6 个态）。
//     非法态一律零写入（无场次、无审计、房间行不动）。
//  3. 并发：房间 CAS 用**读到的** state_version，所以 raceBefore 把版本推走后
//     本次必须 ErrConcurrentUpdate；同时锁住「假件不回滚」下的真实残留形态。
//  4. 事务边界：建档→CAS→场次迁移→两条审计全在一个 TransactCtx 里，
//     提交后的外部往返只有「读录制开关」一条（本服务无 live-media 客户端，README 已记缺口）。
//  5. 守卫顺序：入参守卫在抢键之前（键不烧），业务守卫在抢键之后（键烧但无结果）——
//     这是两个不同事实，必须分开锁，不能笼统写成「守卫拒绝」。
//  6. 依赖错误原样上抛，不退化成语义不同的错误。
//
// 本轮发现的生产缺陷（pin 当前行为，未改代码）：
//   - startlivelogic.go:121 日志文案「按最早一条判定冲突」与实际取值相反：
//     live_session.ListActiveByRoom 是 session_id **倒序**（model/live_session.go 的
//     ORDER BY session_id DESC LIMIT ?，见 fakes_test.go:1872 的复刻说明），
//     所以 actives[0] 与错误消息里的 session_id 都是**最新**一场。
//     后果：运维按错误消息里的 session_id 排障时会拿到错的场次（不影响判定结果本身，
//     因为只要 len(actives)>0 就拒绝）。已在用例里按真实取值断言。
//   - startlivelogic.go:57-73 在**任何业务校验之前**抢 request_id，所以「房间还没审核」
//     「状态不对」这类拒绝会把键消费掉且永不回填结果
//     （TestStartLiveAcceptsExactlyTheStatesThatCanReachLiving、
//     TestStartLiveUnverifiedRoomRejected 都用 wantKeyBurnedNoResult 钉住这一形态）。
//     同一 request_id 重试永远拿 ErrIdempotencyResultMissing，客户端必须换新键。
//     改法不唯一（要么把抢键挪到守卫之后，要么失败时删键），故只 pin 不改。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

const (
	startLiveNow  int64 = 1_700_000_000
	startLiveRoom int64 = 3001
	startLiveMid  int64 = 101
)

// startLiveReq 是一条「除 request_id 外都合法」的开播请求。
func startLiveReq(reqID string) *rpc.StartLiveReq {
	return &rpc.StartLiveReq{
		RoomId:    startLiveRoom,
		Mid:       startLiveMid,
		StreamId:  "stream-abc",
		RequestId: reqID,
		TraceId:   "trace-start",
	}
}

// newStartLiveLogic 装配一个「下游全 nil」的上下文：StartLive 不碰任何下游 client。
func newStartLiveLogic(t *testing.T, st *store, conf ...config.LiveRoomConf) *StartLiveLogic {
	t.Helper()
	c := testLiveRoomConf()
	if len(conf) > 0 {
		c = conf[0]
	}
	return NewStartLiveLogic(context.Background(), st.svcCtxWith(c))
}

// seedStartLiveScene 布一个「READY + 审核通过 + 房主绑定 + 有配置行」的标准开播前置。
func seedStartLiveScene(t *testing.T, st *store) *model.LiveRoom {
	t.Helper()
	r := st.seedRoom(baseRoom(startLiveRoom, startLiveMid, model.RoomStateReady))
	st.seedAnchor(baseAnchor(501, startLiveRoom, startLiveMid, model.AnchorRoleOwner))
	st.seedSetting(baseSetting(startLiveRoom, startLiveNow))
	return r
}

func TestStartLiveHappyPathWritesSessionRoomAndTwoAuditRowsInOneTransaction(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	lg := newStartLiveLogic(t, st)

	reply, err := lg.StartLive(startLiveReq("req-start"))
	wantNoErr(t, "StartLive", err)
	defer st.checkRaces(t)

	wantEQ(t, "StartLive", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_LIVING)
	wantEQ(t, "StartLive", "started_at", reply.GetStartedAt(), startLiveNow)
	wantEQ(t, "StartLive", "state_version", reply.GetStateVersion(), int32(2))
	wantEQ(t, "StartLive", "replayed", reply.GetReplayed(), false)

	// 新分配的 session_id 不写进期望序列的字面量：先读回来再拼（断言的是「迁移与审计
	// 确实挂在刚建出来的那一场」，比写死数字更强）。
	sessionID := reply.GetSessionId()
	wantSeq(t, "StartLive 首次执行", st.log, 0,
		"live_room_idempotency.Claim:req-start",
		"live_room.FindOne:3001",
		"live_room_anchor.IsEnabled:3001/101",
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", startLiveRoom, startLiveNow),
		"live_session.ListActiveByRoom:3001",
		"db.TransactCtx",
		"live_session.InsertTx:r3001",
		"live_room.TransitionTx:3001:2->3/v1",
		fmt.Sprintf("live_session.TransitionTx:%d:1->2/r0", sessionID),
		"live_room_state_log.InsertTx:t1:2->3:",
		"live_room_state_log.InsertTx:t3:1->2:",
		fmt.Sprintf("live_room_setting.RecordEnabledFor:%d", startLiveRoom),
		"live_room_idempotency.SaveResult:req-start",
	)
	wantTxCount(t, "StartLive 落库", st.conn, 1)

	row := st.roomAt(t, startLiveRoom)
	wantEQ(t, "房间行", "state", row.State, model.RoomStateLiving)
	wantEQ(t, "房间行", "state_version", row.StateVersion, int32(2))
	wantEQ(t, "房间行", "active_session_id", row.ActiveSessionID, sessionID)
	wantEQ(t, "房间行", "active_stream_id", row.ActiveStreamID, "stream-abc")
	wantEQ(t, "房间行", "mtime", row.Mtime, startLiveNow)
	// 开播不改资料列：title/area_id/verify_state 保持读到的值。
	wantEQ(t, "房间行", "verify_state", row.VerifyState, model.VerifyStatePassed)

	s := st.sessionAt(t, sessionID)
	wantEQ(t, "场次行", "room_id", s.RoomID, startLiveRoom)
	wantEQ(t, "场次行", "mid", s.Mid, startLiveMid)
	wantEQ(t, "场次行", "state", s.State, model.SessionStateLiving)
	wantEQ(t, "场次行", "started_at", s.StartedAt, startLiveNow)
	wantEQ(t, "场次行", "ended_at", s.EndedAt, int64(0))
	wantEQ(t, "场次行", "end_reason", s.EndReason, model.EndReasonUnspecified)
	wantEQ(t, "场次行", "last_stream_seq", s.LastStreamSeq, int64(0))
	wantEQ(t, "场次行", "stream_id", s.StreamID, "stream-abc")
	wantEQ(t, "场次行", "replay_state", s.ReplayState, model.ReplayStateNone)
	// 快照列取的是**开播那一刻**的房间值（之后改分区不回写历史场次）。
	wantEQ(t, "场次行", "area_id_snapshot", s.AreaIDSnapshot, int64(7001))
	wantEQ(t, "场次行", "title_snapshot", s.TitleSnapshot, "房间 3001")
	wantEQ(t, "场次行", "trace_id", s.TraceID, "trace-start")

	logs := st.logsOf(startLiveRoom)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2（房间态 + 场次态）", len(logs))
	}
	wantEQ(t, "房间审计", "state_type", logs[0].StateType, model.LogTypeRoomState)
	wantEQ(t, "房间审计", "from->to", fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "2->3")
	wantEQ(t, "房间审计", "source", logs[0].Source, model.SourceRPCClient)
	wantEQ(t, "房间审计", "request_id", logs[0].RequestID, "req-start")
	wantEQ(t, "房间审计", "operator_mid", logs[0].OperatorMid, startLiveMid)
	wantEQ(t, "房间审计", "session_id", logs[0].SessionID, sessionID)
	wantEQ(t, "场次审计", "state_type", logs[1].StateType, model.LogTypeSessionState)
	wantEQ(t, "场次审计", "from->to", fmt.Sprintf("%d->%d", logs[1].FromState, logs[1].ToState), "1->2")
	wantEQ(t, "场次审计", "trace_id", logs[1].TraceID, "trace-start")

	// 审计行号连续且自洽：同一次开播的两行必须紧邻（append-only 表，插入序=号序）。
	wantEQ(t, "审计连续性", "log_id 差", logs[1].LogID-logs[0].LogID, int64(1))
}

// TestStartLiveEmptyStreamIDIsAllowed 锁「stream_id 只是引用、可为空」：
// 空引用不得写成空字符串以外的形态，也不得让开播失败。
func TestStartLiveEmptyStreamIDIsAllowed(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	lg := newStartLiveLogic(t, st)

	in := startLiveReq("req-nostream")
	in.StreamId = "  " // 只有空白：checkRef 归一为空串
	reply, err := lg.StartLive(in)
	wantNoErr(t, "StartLive 空 stream_id", err)

	row := st.roomAt(t, startLiveRoom)
	wantEQ(t, "房间行", "active_stream_id", row.ActiveStreamID, "")
	wantEQ(t, "场次行", "stream_id", st.sessionAt(t, reply.GetSessionId()).StreamID, "")
}

// TestStartLiveReplayHitsOriginalResultWithZeroWrites 锁命中重放：
// 逐字段回首次落库的原值、replayed=true、且**不产生任何新写**（序列 + 行数双重锁）。
func TestStartLiveReplayHitsOriginalResultWithZeroWrites(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	lg := newStartLiveLogic(t, st)

	first, err := lg.StartLive(startLiveReq("req-replay"))
	wantNoErr(t, "StartLive 首次", err)
	before := st.counts()
	txBefore := st.conn.transactions
	from := st.log.snapshot()

	second, err := lg.StartLive(startLiveReq("req-replay"))
	wantNoErr(t, "StartLive 重放", err)
	defer st.checkRaces(t)

	wantEQ(t, "重放", "replayed", second.GetReplayed(), true)
	wantEQ(t, "重放", "session_id", second.GetSessionId(), first.GetSessionId())
	wantEQ(t, "重放", "state", second.GetState(), first.GetState())
	wantEQ(t, "重放", "started_at", second.GetStartedAt(), first.GetStartedAt())
	wantEQ(t, "重放", "state_version", second.GetStateVersion(), first.GetStateVersion())

	wantSeq(t, "重放只准读键", st.log, from,
		"live_room_idempotency.Claim:req-replay",
		"live_room_idempotency.Find:req-replay",
	)
	if got := st.counts(); got != before {
		t.Errorf("重放后行数 = %+v, want 与首次一致 %+v", got, before)
	}
	wantTxCount(t, "重放不起事务", st.conn, txBefore)
	// 库里仍只有一个场次、房间版本没被推第二次。
	wantEQ(t, "房间行", "state_version", st.roomAt(t, startLiveRoom).StateVersion, int32(2))
	wantCount(t, "场次行数", st.log, "live_session.InsertTx", 1)
}

// TestStartLiveReplayReturnsStoredValuesNotCurrentOnes 锁「重放回的是首次那一刻的快照」：
// 结果快照取自 live_room_idempotency.result_json，之后房间状态再怎么变都不影响重放口径。
func TestStartLiveReplayReturnsStoredValuesNotCurrentOnes(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	st.seedIdem("req-stored", "StartLive",
		`{"session_id":77,"state":3,"started_at":1699999000,"state_version":9}`)
	lg := newStartLiveLogic(t, st)

	reply, err := lg.StartLive(startLiveReq("req-stored"))
	wantNoErr(t, "StartLive 重放已存结果", err)

	wantEQ(t, "重放", "replayed", reply.GetReplayed(), true)
	wantEQ(t, "重放", "session_id", reply.GetSessionId(), int64(77))
	wantEQ(t, "重放", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_LIVING)
	wantEQ(t, "重放", "started_at", reply.GetStartedAt(), int64(1699999000))
	wantEQ(t, "重放", "state_version", reply.GetStateVersion(), int32(9))
	// 号 77 在库里根本不存在：重放不校验、也不回填，只回快照（读回库存证明确实没建）。
	if got := st.counts().sessions; got != 0 {
		t.Errorf("重放写进场次数 = %d, want 0", got)
	}
	wantNoCallAfter(t, "重放", st.log, 2)
}

// TestStartLiveKeyUsedByAnotherRPCMustNotReplayTheirResult 锁「别人的键不能当自己的回放」。
func TestStartLiveKeyUsedByAnotherRPCMustNotReplayTheirResult(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	st.seedIdem("shared-key", "EndLive", `{"session_id":55}`)
	lg := newStartLiveLogic(t, st)

	reply, err := lg.StartLive(startLiveReq("shared-key"))
	wantErrIs(t, "StartLive 复用别人的键", err, model.ErrRequestIDReused)
	wantErrContains(t, "错误归因", err, "used by EndLive")
	if reply != nil {
		t.Errorf("应答 = %+v, want nil（不能把别的方法的结果当重放发出去）", reply)
	}
	wantSeq(t, "抢键后回查即止", st.log, 0,
		"live_room_idempotency.Claim:shared-key",
		"live_room_idempotency.Find:shared-key",
	)
	wantEQ(t, "既有行不被改写", "result_json", st.idemAt("shared-key").ResultJSON, `{"session_id":55}`)
	wantEQ(t, "既有行不被改写", "rpc", st.idemAt("shared-key").Rpc, "EndLive")
}

// TestStartLiveBurnedKeyWithoutResult 锁「键烧了但结果没回填」：
// 上一次执行仍在进行中，必须显式报 ErrIdempotencyResultMissing，绝不能重跑副作用。
func TestStartLiveBurnedKeyWithoutResult(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	st.seedIdem("half-done", "StartLive", "")
	lg := newStartLiveLogic(t, st)

	_, err := lg.StartLive(startLiveReq("half-done"))
	wantErrIs(t, "结果未就绪", err, model.ErrIdempotencyResultMissing)
	wantKeyBurnedNoResult(t, "半完成态", "half-done", st)
	wantSeq(t, "回查后必须止步", st.log, 0,
		"live_room_idempotency.Claim:half-done",
		"live_room_idempotency.Find:half-done",
	)
}

// TestStartLiveInputGuardsRunBeforeAnyCall 锁「入参守卫在抢键之前」：零依赖调用 + 键不烧。
func TestStartLiveInputGuardsRunBeforeAnyCall(t *testing.T) {
	longReq := make([]byte, maxDedupIDBytes+1)
	for i := range longReq {
		longReq[i] = 'x'
	}
	cases := []struct {
		name string
		edit func(*rpc.StartLiveReq)
		want error
		frag string
	}{
		{"room_id 非正", func(in *rpc.StartLiveReq) { in.RoomId = 0 }, model.ErrInvalidRoomID, ""},
		{"mid 非正", func(in *rpc.StartLiveReq) { in.Mid = -1 }, model.ErrInvalidMid, ""},
		{"缺 request_id", func(in *rpc.StartLiveReq) { in.RequestId = "  " }, model.ErrRequestIDRequired, ""},
		{"request_id 超列宽", func(in *rpc.StartLiveReq) { in.RequestId = string(longReq) },
			model.ErrDedupIDTooLong, "max 64"},
		{"stream_id 含空格", func(in *rpc.StartLiveReq) { in.StreamId = "a b" },
			model.ErrStreamRefMismatch, "stream_id"},
		{"stream_id 是完整 URL", func(in *rpc.StartLiveReq) { in.StreamId = "https://cdn/s" },
			model.ErrStreamRefMismatch, "opaque reference"},
		{"stream_id 超列宽", func(in *rpc.StartLiveReq) { in.StreamId = string(longReq) },
			model.ErrStreamRefMismatch, "too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, startLiveNow)
			st := newStore()
			seedStartLiveScene(t, st)
			lg := newStartLiveLogic(t, st)

			in := startLiveReq("req-guard")
			tc.edit(in)
			_, err := lg.StartLive(in)
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name+" 细节", err, tc.frag)
			}
			wantNoCallAfter(t, tc.name, st.log, 0)
			wantKeyUnburned(t, tc.name, "req-guard", st)
			wantTxCount(t, tc.name, st.conn, 0)
		})
	}

	t.Run("nil 请求", func(t *testing.T) {
		fixClock(t, startLiveNow)
		st := newStore()
		lg := newStartLiveLogic(t, st)
		_, err := lg.StartLive(nil)
		wantErrIs(t, "nil 请求", err, model.ErrInvalidRoomID)
		wantNoCallAfter(t, "nil 请求", st.log, 0)
	})
}

// TestStartLiveAcceptsExactlyTheStatesThatCanReachLiving 是本轮状态机结论的主体：
// **不写死**「READY 才能开播」，而是从 model.RoomTransitionTargets 推出能进 LIVING 的态集合，
// 要求 StartLive 的成功集合与之完全相等；失败态必须一个字节都没写。
func TestStartLiveAcceptsExactlyTheStatesThatCanReachLiving(t *testing.T) {
	allStates := []int32{
		model.RoomStatePending, model.RoomStateReady, model.RoomStateLiving,
		model.RoomStateFinished, model.RoomStateBanned, model.RoomStateDisabled,
	}
	// 由矩阵推导期望集合（用例不复制矩阵内容，矩阵改了这里就跟着变）。
	var wantLiveable []int32
	for _, s := range allStates {
		for _, to := range model.RoomTransitionTargets(s) {
			if to == model.RoomStateLiving {
				wantLiveable = append(wantLiveable, s)
				break
			}
		}
	}

	fixClock(t, startLiveNow)
	st := newStore()
	var lived []int32
	lg := newStartLiveLogic(t, st)
	for i, s := range allStates {
		roomID := int64(3100 + i)
		st.seedRoom(baseRoom(roomID, startLiveMid, s))
		st.seedAnchor(baseAnchor(int64(600+i), roomID, startLiveMid, model.AnchorRoleOwner))
		st.seedSetting(baseSetting(roomID, startLiveNow))

		in := startLiveReq(fmt.Sprintf("req-state-%d", s))
		in.RoomId = roomID
		_, err := lg.StartLive(in)
		if err == nil {
			lived = append(lived, s)
			if got := st.roomAt(t, roomID).State; got != model.RoomStateLiving {
				t.Errorf("%s：成功后房间态 = %d, want LIVING", stateName(s), got)
			}
			continue
		}
		// 失败态：除抢键之外不得有任何写入，房间态与版本原封不动。
		wantKeyBurnedNoResult(t, stateName(s), in.RequestId, st)
		row := st.roomAt(t, roomID)
		wantEQ(t, stateName(s)+" 拒绝后状态", "state", row.State, s)
		wantEQ(t, stateName(s)+" 拒绝后版本", "state_version", row.StateVersion, int32(1))
		wantEQ(t, stateName(s)+" 拒绝后活跃场次", "active_session_id", row.ActiveSessionID, int64(0))
	}
	wantDeepEQ(t, "可开播态集合", "由矩阵推导 vs StartLive 实际接受", lived, wantLiveable)
	wantEQ(t, "写进场次数", "sessions", st.counts().sessions, len(wantLiveable))
	wantEQ(t, "审计行数", "logs", st.counts().logs, 2*len(wantLiveable))
}

func stateName(s int32) string {
	if s == model.RoomStateReady {
		return "READY"
	}
	return fmt.Sprintf("state=%d", s)
}

// TestStartLiveRejectsIllegalTransitionsWithNamedErrors 逐个点名非 READY 态的拒绝口径，
// 保证「非法迁移」不被退化成一个笼统的 unavailable。
// 每例都布成「生产写得出来」的形态：LIVING 房间必带活跃场次指针与那一场次，
// BANNED 房间必带生效禁播记录与 ban_until。
func TestStartLiveRejectsIllegalTransitionsWithNamedErrors(t *testing.T) {
	cases := []struct {
		name string
		seed func(*store)
		want error
		frag string
	}{
		{"终态房间", func(s *store) { s.seedRoom(baseRoom(startLiveRoom, startLiveMid, model.RoomStateFinished)) },
			model.ErrRoomFinished, ""},
		{"禁播房间", func(s *store) {
			r := baseRoom(startLiveRoom, startLiveMid, model.RoomStateBanned)
			r.BanUntil = startLiveNow + 3600
			s.seedRoom(r)
			b := baseBan(8811, startLiveRoom, model.BanStateActive)
			b.BanType = model.BanTypeTemporary
			b.StartAt = startLiveNow - 60
			b.EndAt = startLiveNow + 3600
			s.seedBan(b)
		}, model.ErrRoomBanned, ""},
		{"在播房间", func(s *store) {
			r := baseRoom(startLiveRoom, startLiveMid, model.RoomStateLiving)
			r.ActiveSessionID = 9011
			r.ActiveStreamID = "stream-abc"
			s.seedRoom(r)
			ls := baseSession(9011, startLiveRoom, startLiveMid, model.SessionStateLiving)
			ls.StartedAt = startLiveNow - 300
			s.seedSession(ls)
		}, model.ErrSessionAlreadyActive, ""},
		{"待完善房间", func(s *store) { s.seedRoom(baseRoom(startLiveRoom, startLiveMid, model.RoomStatePending)) },
			model.ErrInvalidRoomTransition, "开播前必须先通过 PrepareLive"},
		{"停用房间", func(s *store) { s.seedRoom(baseRoom(startLiveRoom, startLiveMid, model.RoomStateDisabled)) },
			model.ErrInvalidRoomTransition, "state=6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, startLiveNow)
			st := newStore()
			tc.seed(st)
			st.seedAnchor(baseAnchor(501, startLiveRoom, startLiveMid, model.AnchorRoleOwner))
			st.seedSetting(baseSetting(startLiveRoom, startLiveNow))
			lg := newStartLiveLogic(t, st)
			want := int32(st.rooms.rows[0].State)

			_, err := lg.StartLive(startLiveReq("req-" + tc.name))
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name+" 细节", err, tc.frag)
			}
			// 抢键发生在业务守卫之前（文件头缺陷 2），所以这里键已烧但无结果。
			wantKeyBurnedNoResult(t, tc.name, "req-"+tc.name, st)
			wantSeq(t, tc.name+" 只走到抢键+读房间", st.log, 0,
				"live_room_idempotency.Claim:req-"+tc.name,
				"live_room.FindOne:3001",
			)
			wantTxCount(t, tc.name, st.conn, 0)
			row := st.roomAt(t, startLiveRoom)
			wantEQ(t, tc.name+" 房间态不变", "state", row.State, want)
			wantEQ(t, tc.name+" 版本不变", "state_version", row.StateVersion, int32(1))
		})
	}
}

// TestStartLiveUnverifiedRoomRejected 锁「审核未通过不给开播」，
// 且这个守卫在锚点绑定检查**之前**（不多读一次绑定表）。
func TestStartLiveUnverifiedRoomRejected(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	st.rooms.rows[0].VerifyState = model.VerifyStateReviewing
	lg := newStartLiveLogic(t, st)

	_, err := lg.StartLive(startLiveReq("req-unverified"))
	wantErrIs(t, "未审核", err, model.ErrNotVerified)
	wantSeq(t, "未审核的调用面", st.log, 0,
		"live_room_idempotency.Claim:req-unverified",
		"live_room.FindOne:3001",
	)
	wantKeyBurnedNoResult(t, "未审核", "req-unverified", st)
}

// TestStartLiveAnchorGuard 锁绑定关系三态：未绑定 / 已解绑 / 联合主播。
func TestStartLiveAnchorGuard(t *testing.T) {
	cases := []struct {
		name    string
		bind    bool
		role    int32
		state   int32
		wantErr bool
	}{
		{"未绑定", false, 0, 0, true},
		{"已解绑的联合主播", true, model.AnchorRoleCohost, model.BindStateDisabled, true},
		{"联合主播（生效）", true, model.AnchorRoleCohost, model.BindStateEnabled, false},
		{"房管（生效）", true, model.AnchorRoleManager, model.BindStateEnabled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, startLiveNow)
			st := newStore()
			st.seedRoom(baseRoom(startLiveRoom, startLiveMid, model.RoomStateReady))
			st.seedSetting(baseSetting(startLiveRoom, startLiveNow))
			if tc.bind {
				a := baseAnchor(501, startLiveRoom, startLiveMid, tc.role)
				a.State = tc.state
				st.seedAnchor(a)
			}
			lg := newStartLiveLogic(t, st)

			reply, err := lg.StartLive(startLiveReq("req-anchor"))
			if tc.wantErr {
				wantErrIs(t, tc.name, err, model.ErrAnchorForbidden)
				wantSeq(t, tc.name, st.log, 0,
					"live_room_idempotency.Claim:req-anchor",
					"live_room.FindOne:3001",
					"live_room_anchor.IsEnabled:3001/101",
				)
				wantEQ(t, tc.name+" 未建场次", "sessions", st.counts().sessions, 0)
				return
			}
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name+" 开播成功", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_LIVING)
		})
	}
}

// TestStartLiveBanGuard 锁「房间禁播中不给开播」，并区分「已解除」「已过期」两种不算生效的记录。
func TestStartLiveBanGuard(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(*store)
		wantErr bool
	}{
		{"永久禁播", func(s *store) { s.seedBan(baseBan(8801, startLiveRoom, model.BanStateActive)) }, true},
		{"临时禁播未到期", func(s *store) {
			b := baseBan(8802, startLiveRoom, model.BanStateActive)
			b.BanType = model.BanTypeTemporary
			b.StartAt = startLiveNow - 10
			b.EndAt = startLiveNow + 10
			s.seedBan(b)
		}, true},
		{"临时禁播已到期", func(s *store) {
			b := baseBan(8803, startLiveRoom, model.BanStateActive)
			b.BanType = model.BanTypeTemporary
			b.StartAt = startLiveNow - 100
			b.EndAt = startLiveNow - 10
			s.seedBan(b)
		}, false},
		{"已解除的禁播", func(s *store) { s.seedBan(baseBan(8804, startLiveRoom, model.BanStateLifted)) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, startLiveNow)
			st := newStore()
			seedStartLiveScene(t, st)
			tc.seed(st)
			lg := newStartLiveLogic(t, st)

			_, err := lg.StartLive(startLiveReq("req-ban"))
			if tc.wantErr {
				wantErrIs(t, tc.name, err, model.ErrRoomBanned)
				wantKeyBurnedNoResult(t, tc.name, "req-ban", st)
				wantEQ(t, tc.name+" 未建场次", "sessions", st.counts().sessions, 0)
				wantMethodCount(t, tc.name+" 不查场次", st.log, "live_session.ListActiveByRoom", 0)
				return
			}
			wantNoErr(t, tc.name, err)
		})
	}
}

// TestStartLiveRejectsLingeringNonTerminalSession 锁「同一房间不得并存两个非终态场次」：
// 房间已是 READY 但库里还挂着 PENDING 场次，必须拒绝并点名那一场的号。
func TestStartLiveRejectsLingeringNonTerminalSession(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	st.seedSession(baseSession(9001, startLiveRoom, startLiveMid, model.SessionStatePending))
	lg := newStartLiveLogic(t, st)

	_, err := lg.StartLive(startLiveReq("req-lingering"))
	wantErrIs(t, "残留场次", err, model.ErrSessionAlreadyActive)
	wantErrContains(t, "残留场次点名", err, "session_id=9001")
	wantSeq(t, "残留场次的调用面", st.log, 0,
		"live_room_idempotency.Claim:req-lingering",
		"live_room.FindOne:3001",
		"live_room_anchor.IsEnabled:3001/101",
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", startLiveRoom, startLiveNow),
		"live_session.ListActiveByRoom:3001",
	)
	wantTxCount(t, "残留场次", st.conn, 0)
	wantEQ(t, "残留场次不新建", "sessions", st.counts().sessions, 1)
}

// TestStartLiveConcurrentVersionBumpLosesAndLeavesResidue 是并发 + 事务边界两条结论的合体：
// 读到 state_version=1 之后、CAS 之前，别的入口把版本推到 2（状态仍是 READY），
// StartLive 用**读到的**版本做 CAS，所以必须 ErrConcurrentUpdate；
// 又因为假件不回滚，事务里已经建出来的场次就是可观察残留（真实 MySQL 会回滚，
// 这里断言的是「哪些写进了同一个 TransactCtx」这一事实，不是「残留应当存在」）。
func TestStartLiveConcurrentVersionBumpLosesAndLeavesResidue(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	st.raceBefore("live_room.TransitionTx:3001:2->3", func() {
		r := st.rooms.rows[0]
		r.StateVersion = 2
		r.Mtime = startLiveNow + 1
	})
	lg := newStartLiveLogic(t, st)

	_, err := lg.StartLive(startLiveReq("req-race"))
	wantErrIs(t, "并发推版本", err, model.ErrConcurrentUpdate)
	st.checkRaces(t)

	wantSeq(t, "并发失败前的调用面", st.log, 0,
		"live_room_idempotency.Claim:req-race",
		"live_room.FindOne:3001",
		"live_room_anchor.IsEnabled:3001/101",
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", startLiveRoom, startLiveNow),
		"live_session.ListActiveByRoom:3001",
		"db.TransactCtx",
		"live_session.InsertTx:r3001",
		"live_room.TransitionTx:3001:2->3/v1",
	)
	// 仲裁结果：房间没进 LIVING（CAS 未命中），版本是插队者的 2，不是本次写的。
	row := st.roomAt(t, startLiveRoom)
	wantEQ(t, "房间行", "state", row.State, model.RoomStateReady)
	wantEQ(t, "房间行", "state_version", row.StateVersion, int32(2))
	wantEQ(t, "房间行", "active_session_id", row.ActiveSessionID, int64(0))
	// 残留：一个 PENDING 场次、零审计。真实库里它随事务回滚，所以「同事务」这一事实
	// 由 wantTxCount==1 与上面的序列证明，而不是由残留证明。
	wantEQ(t, "残留场次", "sessions", st.counts().sessions, 1)
	wantEQ(t, "残留审计", "logs", st.counts().logs, 0)
	lingering := st.sessionAt(t, 1)
	wantEQ(t, "残留场次状态", "state", lingering.State, model.SessionStatePending)
	wantEQ(t, "残留场次开播时间", "started_at", lingering.StartedAt, int64(0))
	wantKeyBurnedNoResult(t, "并发失败", "req-race", st)
}

// TestStartLiveAuditFailurePropagatesVerbatim 锁事务内第 4 步失败：
// 错误必须是 model 原样抛出的驱动错误（不能退化成 ErrConcurrentUpdate 之类），
// 且前 3 步的写入形态可读回（假件不回滚）。
func TestStartLiveAuditFailurePropagatesVerbatim(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	boom := errors.New("audit table unavailable")
	st.stateLogs.failWith("InsertTx", boom)
	lg := newStartLiveLogic(t, st)

	_, err := lg.StartLive(startLiveReq("req-audit-fail"))
	wantErrIs(t, "审计写入失败", err, boom)
	wantErrContains(t, "错误原样上抛", err, "live_room_state_log Insert")
	wantCount(t, "第一条审计都没写成", st.log, "live_room_state_log.InsertTx", 1)
	// 房间与场次都已在事务里改完——这正是「四步必须同事务」的原因。
	wantEQ(t, "房间行", "state", st.roomAt(t, startLiveRoom).State, model.RoomStateLiving)
	wantEQ(t, "审计行数", "logs", st.counts().logs, 0)
	wantKeyBurnedNoResult(t, "审计失败", "req-audit-fail", st)
}

// TestStartLiveSessionInsertFailureKeepsRoomUntouched 锁事务内第 1 步就失败：
// 房间一行都不该动（对比上一个用例的「已改完」形态）。
func TestStartLiveSessionInsertFailureKeepsRoomUntouched(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	seedStartLiveScene(t, st)
	boom := errors.New("dup session")
	st.sessions.failWith("InsertTx", boom)
	lg := newStartLiveLogic(t, st)

	_, err := lg.StartLive(startLiveReq("req-insert-fail"))
	wantErrIs(t, "建档失败", err, boom)
	row := st.roomAt(t, startLiveRoom)
	wantEQ(t, "房间行", "state", row.State, model.RoomStateReady)
	wantEQ(t, "房间行", "state_version", row.StateVersion, int32(1))
	wantEQ(t, "场次行数", "sessions", st.counts().sessions, 0)
	wantMethodCount(t, "不跑迁移", st.log, "live_room.TransitionTx", 0)
}

// TestStartLiveReadFailuresPropagateVerbatim 锁「依赖错误不退化」：
// 抢键、读房间、读绑定、读禁播、读场次每一处失败都必须原样上抛。
func TestStartLiveReadFailuresPropagateVerbatim(t *testing.T) {
	type inject struct {
		name  string
		fail  func(*store, error)
		until int // 期望走到第几条调用
	}
	cases := []inject{
		{"抢键失败", func(s *store, e error) { s.idem.failWith("Claim", e) }, 1},
		{"读房间失败", func(s *store, e error) { s.rooms.failWith("FindOne", e) }, 2},
		{"读绑定失败", func(s *store, e error) { s.anchors.failWith("IsEnabled", e) }, 3},
		{"读禁播失败", func(s *store, e error) { s.bans.failWith("FindActiveByRoom", e) }, 4},
		{"读场次失败", func(s *store, e error) { s.sessions.failWith("ListActiveByRoom", e) }, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, startLiveNow)
			st := newStore()
			seedStartLiveScene(t, st)
			boom := errors.New("db down")
			tc.fail(st, boom)
			lg := newStartLiveLogic(t, st)

			_, err := lg.StartLive(startLiveReq("req-read-fail"))
			wantErrIs(t, tc.name, err, boom)
			if got := len(st.log.ops); got != tc.until {
				t.Errorf("%s：调用数 = %d, want %d（轨迹 %v）", tc.name, got, tc.until, st.log.ops)
			}
			wantTxCount(t, tc.name, st.conn, 0)
		})
	}
}

// TestStartLiveRoomMissing 锁「房间不存在」：读回 nil 行必须由 logic 翻成 ErrRoomNotFound，
// 不能拿去解引用，也不能因「查不到」而当成开播成功。
func TestStartLiveRoomMissing(t *testing.T) {
	fixClock(t, startLiveNow)
	st := newStore()
	lg := newStartLiveLogic(t, st)

	_, err := lg.StartLive(startLiveReq("req-missing"))
	wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
	wantSeq(t, "房间不存在", st.log, 0,
		"live_room_idempotency.Claim:req-missing",
		"live_room.FindOne:3001",
	)
	wantKeyBurnedNoResult(t, "房间不存在", "req-missing", st)
}

// TestStartLiveRecordingSwitchFailuresOnlyLogged 锁提交后的外部读取：
// 录制开关读不到（缺行或 DB 失败）都不得影响已完成的开播，也不得伪造「已起录制」。
// 两条分支的差别只有日志，共同点是**没有**任何 live-media 往返（本服务无该客户端）。
func TestStartLiveRecordingSwitchFailuresOnlyLogged(t *testing.T) {
	t.Run("缺配置行", func(t *testing.T) {
		fixClock(t, startLiveNow)
		st := newStore()
		st.seedRoom(baseRoom(startLiveRoom, startLiveMid, model.RoomStateReady))
		st.seedAnchor(baseAnchor(501, startLiveRoom, startLiveMid, model.AnchorRoleOwner))
		lg := newStartLiveLogic(t, st)

		reply, err := lg.StartLive(startLiveReq("req-nosetting"))
		wantNoErr(t, "缺配置行仍开播", err)
		wantEQ(t, "开播结果", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_LIVING)
		wantMethodCount(t, "不写配置行", st.log, "live_room_setting.Upsert", 0)
		wantTxCount(t, "开关读取不在事务里", st.conn, 1)
	})
	t.Run("开关读取失败", func(t *testing.T) {
		fixClock(t, startLiveNow)
		st := newStore()
		seedStartLiveScene(t, st)
		st.settings.failWith("RecordEnabledFor", errors.New("setting db down"))
		lg := newStartLiveLogic(t, st)

		reply, err := lg.StartLive(startLiveReq("req-setting-fail"))
		wantNoErr(t, "开关读失败仍开播", err)
		wantEQ(t, "开播结果", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_LIVING)
		wantCount(t, "只读一次开关", st.log, "live_room_setting.RecordEnabledFor", 1)
	})
	t.Run("开关为开也只声明不起录制", func(t *testing.T) {
		fixClock(t, startLiveNow)
		st := newStore()
		st.seedRoom(baseRoom(startLiveRoom, startLiveMid, model.RoomStateReady))
		st.seedAnchor(baseAnchor(501, startLiveRoom, startLiveMid, model.AnchorRoleOwner))
		s := baseSetting(startLiveRoom, startLiveNow)
		s.RecordEnabled = model.BoolToInt32(true)
		st.seedSetting(s)
		lg := newStartLiveLogic(t, st)

		_, err := lg.StartLive(startLiveReq("req-record"))
		wantNoErr(t, "开关为开", err)
		wantEQ(t, "开关读到开", "record_enabled", st.settingAt(t, startLiveRoom).RecordEnabled, model.BoolToInt32(true))
		// 全序列里没有任何下游 client 调用：本服务没有 live-media 客户端，
		// 「声明要起录制」只落日志，不伪造已起录制。
		for _, op := range st.log.ops {
			for _, prefix := range []string{"creator.", "risk.", "moderation."} {
				if strings.HasPrefix(op, prefix) {
					t.Errorf("StartLive 不应触达任何下游 client，轨迹含 %q", op)
				}
			}
		}
	})
}
