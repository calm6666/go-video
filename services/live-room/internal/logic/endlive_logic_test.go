package logic

// endlive_logic_test.go 覆盖写侧方法 EndLive（endlivelogic.go）。
//
// 锁的结论（对应任务书的 1~6 条）：
//  1. request_id 三态：首次受理 / 命中重放（逐字段回原值 + replayed=true + 零新写，
//     callLog 起点切片与 st.counts() 双重锁死）/ 键被别的 RPC 用过必须报错。
//  2. 状态机：场次 LIVING→ENDED、房间 LIVING→READY 两条边都由 model 矩阵守；
//     「EndLive 必须先于 AttachReplay」这一前置关系用「下播后场次确实进入终态、
//     且本方法绝不碰 replay_state / 不调 AttachReplay」两侧同时锁死；
//     场次已终态时按幂等重放处理（回真实终态、不再改状态）。
//  3. 并发：房间 CAS 与场次 CAS 各布一个 raceBefore 钩子，分别锁「房间版本被推走」
//     与「场次被 BanRoom 抢先终止」两种仲裁结果；钩子没触发时 checkRaces 直接判红。
//  4. 事务边界：场次迁移 + 房间迁移 + 两条审计在同一个 TransactCtx 内；
//     提交后的外部往返是 ClearActiveSession（仅房间非 LIVING 时）、场次回读、房间回读、
//     SaveResult。假件不回滚，所以失败用例断言的是**真实残留形态**。
//  5. 守卫顺序：入参守卫（含 end_reason 白名单）在抢键之前；房间/场次/绑定三类业务
//     守卫在抢键之后（键烧但无结果）。错误一律 errors.Is 原样上抛。
//  6. 不因下播而伪造回放可用性，也不因下播而解禁（BANNED 房间下播后仍是 BANNED）。
//
// 本轮发现的生产缺陷（pin 当前行为，未改代码）：见 startlive_logic_test.go 文件头的
// 汇总说明；EndLive 自身未发现新增缺陷——提交后回读失败会「已改状态但对外报错」
// （TestEndLivePostCommitReadFailureReportsErrorWithCommittedWrites 钉住），
// 这是 saveDedupResult 在回读之后调用的必然结果，属可接受形态，不算缺陷。

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
	endLiveNow    int64 = 1_700_000_000
	endLiveRoom   int64 = 3001
	endLiveMid    int64 = 101
	endLiveSess   int64 = 9001
	endLiveStream       = "stream-abc"
)

// endLiveReq 是一条「除 request_id 外都合法」的下播请求：
// session_id 留 0，即「由本服务按进行中场次解析」，这也是客户端主路径。
func endLiveReq(reqID string) *rpc.EndLiveReq {
	return &rpc.EndLiveReq{
		RoomId:    endLiveRoom,
		Mid:       endLiveMid,
		RequestId: reqID,
		TraceId:   "trace-end",
	}
}

func newEndLiveLogic(t *testing.T, st *store, conf ...config.LiveRoomConf) *EndLiveLogic {
	t.Helper()
	c := testLiveRoomConf()
	if len(conf) > 0 {
		c = conf[0]
	}
	return NewEndLiveLogic(context.Background(), st.svcCtxWith(c))
}

// seedEndLiveScene 布一个「LIVING 房间 + LIVING 场次 + 房主绑定」的标准在播形态：
// 房间带 active_session_id / active_stream_id 投影，场次 300 秒前开播。
func seedEndLiveScene(t *testing.T, st *store) (*model.LiveRoom, *model.LiveSession) {
	t.Helper()
	r := baseRoom(endLiveRoom, endLiveMid, model.RoomStateLiving)
	r.ActiveSessionID = endLiveSess
	r.ActiveStreamID = endLiveStream
	st.seedRoom(r)
	s := baseSession(endLiveSess, endLiveRoom, endLiveMid, model.SessionStateLiving)
	s.StartedAt = endLiveNow - 300
	s.StreamID = endLiveStream
	st.seedSession(s)
	st.seedAnchor(baseAnchor(501, endLiveRoom, endLiveMid, model.AnchorRoleOwner))
	return r, s
}

func TestEndLiveHappyPathEndsSessionAndReturnsRoomToReady(t *testing.T) {
	fixClock(t, endLiveNow)
	st := newStore()
	seedEndLiveScene(t, st)
	lg := newEndLiveLogic(t, st)

	reply, err := lg.EndLive(endLiveReq("req-end"))
	wantNoErr(t, "EndLive", err)
	defer st.checkRaces(t)

	// 首次执行的完整轨迹：抢键 → 读房间/场次/绑定 → 一个事务（4 条写）→ 两次回读 → 回填结果。
	wantSeq(t, "EndLive 首次执行", st.log, 0,
		"live_room_idempotency.Claim:req-end",
		"live_room.FindOne:3001",
		fmt.Sprintf("live_session.FindActiveByRoom:%d", endLiveRoom),
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", endLiveRoom, endLiveMid),
		"db.TransactCtx",
		fmt.Sprintf("live_session.TransitionTx:%d:2->3/r1", endLiveSess),
		"live_room.TransitionTx:3001:3->2/v1",
		"live_room_state_log.InsertTx:t3:2->3:anchor_stop",
		"live_room_state_log.InsertTx:t1:3->2:",
		fmt.Sprintf("live_session.FindOne:%d", endLiveSess),
		"live_room.FindOne:3001",
		"live_room_idempotency.SaveResult:req-end",
	)
	wantTxCount(t, "EndLive 落库", st.conn, 1)

	// 应答的四个业务字段全部读自**回读**的投影，不是期望值。
	wantEQ(t, "下播应答", "session_id", reply.GetSessionId(), endLiveSess)
	wantEQ(t, "下播应答", "session_state", reply.GetSessionState(), rpc.SessionState_SESSION_STATE_ENDED)
	wantEQ(t, "下播应答", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_READY)
	wantEQ(t, "下播应答", "duration_seconds", reply.GetDurationSeconds(), int64(300))
	wantEQ(t, "下播应答", "replayed", reply.GetReplayed(), false)

	row := st.roomAt(t, endLiveRoom)
	wantEQ(t, "房间行", "state", row.State, model.RoomStateReady)
	wantEQ(t, "房间行", "state_version", row.StateVersion, int32(2))
	// 「清场次投影」必须在房间那条 UPDATE 里（clearActiveSessionPatch），
	// 否则会出现「状态已 READY 但 active_session_id 还指着老场次」的中间态。
	wantEQ(t, "房间行", "active_session_id", row.ActiveSessionID, int64(0))
	wantEQ(t, "房间行", "active_stream_id", row.ActiveStreamID, "")
	wantEQ(t, "房间行", "mtime", row.Mtime, endLiveNow)

	s := st.sessionAt(t, endLiveSess)
	wantEQ(t, "场次行", "state", s.State, model.SessionStateEnded)
	wantEQ(t, "场次行", "ended_at", s.EndedAt, endLiveNow)
	wantEQ(t, "场次行", "end_reason", s.EndReason, model.EndReasonAnchorStop)
	wantEQ(t, "场次行", "duration_seconds", s.DurationSeconds, int64(300))
	// EndLive 不得伪造回放可用性：回放槽位仍是建档时的 NONE，也没调 AttachReplay。
	wantEQ(t, "场次行", "replay_state", s.ReplayState, model.ReplayStateNone)
	wantMethodCount(t, "不起回放", st.log, "live_session.AttachReplay", 0)

	logs := st.logsOf(endLiveRoom)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2（场次态 + 房间态）", len(logs))
	}
	wantEQ(t, "场次审计", "state_type", logs[0].StateType, model.LogTypeSessionState)
	wantEQ(t, "场次审计", "from->to", fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "2->3")
	wantEQ(t, "场次审计", "reason", logs[0].Reason, "anchor_stop")
	wantEQ(t, "场次审计", "source", logs[0].Source, model.SourceRPCClient)
	wantEQ(t, "场次审计", "request_id", logs[0].RequestID, "req-end")
	wantEQ(t, "场次审计", "operator_mid", logs[0].OperatorMid, endLiveMid)
	wantEQ(t, "场次审计", "trace_id", logs[0].TraceID, "trace-end")
	wantEQ(t, "房间审计", "state_type", logs[1].StateType, model.LogTypeRoomState)
	wantEQ(t, "房间审计", "from->to", fmt.Sprintf("%d->%d", logs[1].FromState, logs[1].ToState), "3->2")
	wantEQ(t, "房间审计", "session_id", logs[1].SessionID, endLiveSess)
	wantEQ(t, "审计连续性", "log_id 差", logs[1].LogID-logs[0].LogID, int64(1))
}

// TestEndLiveSessionResolutionByExplicitID 锁「带 session_id 走 FindOne、不带走 FindActiveByRoom」：
// 两条路径的调用面不同，写侧不能混为一条（否则跨房间串改的守卫就被绕过了）。
func TestEndLiveSessionResolutionByExplicitID(t *testing.T) {
	fixClock(t, endLiveNow)
	st := newStore()
	seedEndLiveScene(t, st)
	lg := newEndLiveLogic(t, st)

	in := endLiveReq("req-byid")
	in.SessionId = endLiveSess
	reply, err := lg.EndLive(in)
	wantNoErr(t, "按 session_id 下播", err)
	// 与 TestEndLiveHappyPath 的唯一差别就是第 3 步：点名场次走 FindOne 直读，
	// 因此下面这条完整序列本身就是「两条路径不混为一条」的证据。
	wantSeq(t, "显式 session_id 的调用面", st.log, 1,
		"live_room.FindOne:3001",
		fmt.Sprintf("live_session.FindOne:%d", endLiveSess),
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", endLiveRoom, endLiveMid),
		"db.TransactCtx",
		fmt.Sprintf("live_session.TransitionTx:%d:2->3/r1", endLiveSess),
		"live_room.TransitionTx:3001:3->2/v1",
		"live_room_state_log.InsertTx:t3:2->3:anchor_stop",
		"live_room_state_log.InsertTx:t1:3->2:",
		fmt.Sprintf("live_session.FindOne:%d", endLiveSess),
		"live_room.FindOne:3001",
		"live_room_idempotency.SaveResult:req-byid",
	)
	wantMethodCount(t, "不查进行中场次", st.log, "live_session.FindActiveByRoom", 0)
	wantEQ(t, "下播结果", "session_state", reply.GetSessionState(), rpc.SessionState_SESSION_STATE_ENDED)
}

// TestEndLiveSessionResolutionFailuresAreDistinct 锁三种「找不到场次」的口径互不混用。
func TestEndLiveSessionResolutionFailuresAreDistinct(t *testing.T) {
	cases := []struct {
		name    string
		sess    int64
		seed    bool // 是否布场次
		resolve string
		want    error
	}{
		{"点名的场次不存在", 9999, false, "live_session.FindOne:9999", model.ErrSessionNotFound},
		{"点名的场次属于别的房间", endLiveSess, true,
			fmt.Sprintf("live_session.FindOne:%d", endLiveSess), model.ErrSessionRoomMismatch},
		{"没有进行中场次", 0, false,
			fmt.Sprintf("live_session.FindActiveByRoom:%d", endLiveRoom), model.ErrNoActiveSession},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, endLiveNow)
			st := newStore()
			r := baseRoom(endLiveRoom, endLiveMid, model.RoomStateReady)
			st.seedRoom(r)
			st.seedAnchor(baseAnchor(501, endLiveRoom, endLiveMid, model.AnchorRoleOwner))
			if tc.seed {
				s := baseSession(endLiveSess, 4004, endLiveMid, model.SessionStateLiving)
				s.StartedAt = endLiveNow - 60
				st.seedSession(s)
			}
			lg := newEndLiveLogic(t, st)

			in := endLiveReq("req-" + tc.name)
			in.SessionId = tc.sess
			_, err := lg.EndLive(in)
			wantErrIs(t, tc.name, err, tc.want)
			wantKeyBurnedNoResult(t, tc.name, "req-"+tc.name, st)
			wantTxCount(t, tc.name, st.conn, 0)
			// 完整轨迹只到「解析场次」这一步：绑定都没读，更没有任何一次写。
			// （序列一旦多出一条就会红，所以不需要额外的前缀计数。）
			wantSeq(t, tc.name+" 的调用面", st.log, 0,
				fmt.Sprintf("live_room_idempotency.Claim:req-%s", tc.name),
				fmt.Sprintf("live_room.FindOne:%d", endLiveRoom),
				tc.resolve,
			)
			wantMethodCount(t, "不迁移场次", st.log, "live_session.TransitionTx", 0)
			wantMethodCount(t, "不迁移房间", st.log, "live_room.TransitionTx", 0)
			wantMethodCount(t, "不写审计", st.log, "live_room_state_log.InsertTx", 0)
		})
	}
}

// TestEndLiveEndReasonAllowList 锁「终止原因白名单」：只有 ANCHOR_STOP / STREAM_REPLAY 能由
// 客户端指定，BANNED / ROOM_CLOSED / STREAM_TIMEOUT 是 BanRoom / CloseRoom / ReportStreamState
// 的专属归因——让终端自选等于允许伪造「被禁播」。白名单拒绝必须发生在抢键之前。
func TestEndLiveEndReasonAllowList(t *testing.T) {
	t.Run("允许的取值", func(t *testing.T) {
		cases := []struct {
			name       string
			in         rpc.EndReason
			wantReason int32
			wantAudit  string
		}{
			{"UNSPECIFIED 归一为 ANCHOR_STOP", rpc.EndReason_END_REASON_UNSPECIFIED, model.EndReasonAnchorStop, "anchor_stop"},
			{"ANCHOR_STOP", rpc.EndReason_END_REASON_ANCHOR_STOP, model.EndReasonAnchorStop, "anchor_stop"},
			{"STREAM_REPLAY", rpc.EndReason_END_REASON_STREAM_REPLAY, model.EndReasonStreamReplay, "stream_replay"},
		}
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fixClock(t, endLiveNow)
				st := newStore()
				seedEndLiveScene(t, st)
				lg := newEndLiveLogic(t, st)

				in := endLiveReq(fmt.Sprintf("req-reason-%d", i))
				in.EndReason = tc.in
				reply, err := lg.EndLive(in)
				wantNoErr(t, tc.name, err)
				wantEQ(t, tc.name+" 落库原因", "end_reason", st.sessionAt(t, endLiveSess).EndReason, tc.wantReason)
				wantEQ(t, tc.name+" 审计原因", "reason", st.logsOf(endLiveRoom)[0].Reason, tc.wantAudit)
				wantEQ(t, tc.name+" 时长", "duration_seconds", reply.GetDurationSeconds(), int64(300))
			})
		}
	})
	t.Run("拒绝的取值零写入且不烧键", func(t *testing.T) {
		for _, bad := range []rpc.EndReason{
			rpc.EndReason_END_REASON_BANNED,
			rpc.EndReason_END_REASON_ROOM_CLOSED,
			rpc.EndReason_END_REASON_STREAM_TIMEOUT,
			rpc.EndReason(99),
		} {
			fixClock(t, endLiveNow)
			st := newStore()
			seedEndLiveScene(t, st)
			lg := newEndLiveLogic(t, st)

			in := endLiveReq("req-badreason")
			in.EndReason = bad
			_, err := lg.EndLive(in)
			wantErrIs(t, "越权终止原因", err, model.ErrEndReasonInvalid)
			wantErrContains(t, "越权终止原因点名", err, fmt.Sprintf("%d", int32(bad)))
			wantNoCallAfter(t, "越权终止原因", st.log, 0)
			wantKeyUnburned(t, "越权终止原因", "req-badreason", st)
		}
	})
}

// TestEndLiveWhoCanEndSession 锁「只有开播者本人或房主能结束这一场」：
// 房管可以开播（见 StartLive）但不能把别人的场次踢下线。
func TestEndLiveWhoCanEndSession(t *testing.T) {
	cases := []struct {
		name    string
		mid     int64
		bind    int64 // 调用方的绑定行 id；0 = 只靠 500 那条房主绑定
		role    int32
		wantErr bool
	}{
		{"开播者本人（连麦身份）", endLiveMid, 501, model.AnchorRoleCohost, false},
		{"房主结束别人的场次", 999, 0, model.AnchorRoleOwner, false},
		{"房管结束别人的场次", 777, 502, model.AnchorRoleManager, true},
		{"未绑定的 mid", 888, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, endLiveNow)
			st := newStore()
			r := baseRoom(endLiveRoom, 999, model.RoomStateLiving)
			r.ActiveSessionID = endLiveSess
			st.seedRoom(r)
			s := baseSession(endLiveSess, endLiveRoom, endLiveMid, model.SessionStateLiving)
			s.StartedAt = endLiveNow - 300
			st.seedSession(s)
			// 500 是房主 999 的绑定行：一个房间只允许一条生效房主行（uniq_active_owner，
			// seedAnchor 会直接 panic），所以 tc.bind 只用于非房主身份。
			st.seedAnchor(baseAnchor(500, endLiveRoom, 999, model.AnchorRoleOwner))
			if tc.bind != 0 {
				st.seedAnchor(baseAnchor(tc.bind, endLiveRoom, tc.mid, tc.role))
			}
			lg := newEndLiveLogic(t, st)

			in := endLiveReq("req-who")
			in.Mid = tc.mid
			_, err := lg.EndLive(in)
			if tc.wantErr {
				wantErrIs(t, tc.name, err, model.ErrAnchorForbidden)
				// 权限守卫在事务之前：状态一个字都没改，但键已烧（抢键在业务守卫之前）。
				wantMethodCount(t, tc.name, st.log, "db.TransactCtx", 0)
				wantEQ(t, tc.name+" 场次态不变", "state", st.sessionAt(t, endLiveSess).State, model.SessionStateLiving)
				wantKeyBurnedNoResult(t, tc.name, "req-who", st)
				return
			}
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name+" 下播成功", "state", st.sessionAt(t, endLiveSess).State, model.SessionStateEnded)
		})
	}
}

// TestEndLiveTerminalSessionIsIdempotent 锁「场次已终态时按下播重放处理」：
// 回真实终态而不是报错，且**不再改任何状态**；房间若还挂着在播投影，
// 仍要用 ClearActiveSession 把它摘掉（这条不在事务里，是提交后的独立往返）。
func TestEndLiveTerminalSessionIsIdempotent(t *testing.T) {
	cases := []struct {
		name      string
		roomState int32
		sessState int32
		wantClear bool
		wantRoom  rpc.RoomState
		wantSess  rpc.SessionState
	}{
		{"ENDED 场次 + LIVING 房间要摘投影", model.RoomStateLiving, model.SessionStateEnded, true,
			rpc.RoomState_ROOM_STATE_LIVING, rpc.SessionState_SESSION_STATE_ENDED},
		{"TERMINATED 场次 + READY 房间无事可做", model.RoomStateReady, model.SessionStateTerminated, false,
			rpc.RoomState_ROOM_STATE_READY, rpc.SessionState_SESSION_STATE_TERMINATED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, endLiveNow)
			st := newStore()
			r := baseRoom(endLiveRoom, endLiveMid, tc.roomState)
			r.ActiveSessionID = endLiveSess
			r.ActiveStreamID = endLiveStream
			st.seedRoom(r)
			s := baseSession(endLiveSess, endLiveRoom, endLiveMid, tc.sessState)
			s.StreamID = endLiveStream
			st.seedSession(s)
			st.seedAnchor(baseAnchor(501, endLiveRoom, endLiveMid, model.AnchorRoleOwner))
			lg := newEndLiveLogic(t, st)

			// 必须点名 session_id：终态场次不在 model.ActiveSessionStates 里，
			// 不带 session_id 的主路径会先在 FindActiveByRoom 上拿到 ErrNoActiveSession，
			// 走不进 endlivelogic.go:119 的重放分支。
			in := endLiveReq("req-terminal")
			in.SessionId = endLiveSess
			reply, err := lg.EndLive(in)
			wantNoErr(t, tc.name, err)

			wantEQ(t, tc.name, "replayed", reply.GetReplayed(), true)
			wantEQ(t, tc.name, "session_id", reply.GetSessionId(), endLiveSess)
			wantEQ(t, tc.name, "session_state", reply.GetSessionState(), tc.wantSess)
			wantEQ(t, tc.name, "room_state", reply.GetRoomState(), tc.wantRoom)
			wantEQ(t, tc.name, "duration_seconds", reply.GetDurationSeconds(), int64(1000))

			// 完整轨迹：抢键 → 读房间/场次/绑定 →（仅房间 LIVING 时）摘投影 → 回填结果。
			// 序列本身就是「不起事务、零新写、不迁移、不补审计」的证据。
			wantOps := []string{
				"live_room_idempotency.Claim:req-terminal",
				fmt.Sprintf("live_room.FindOne:%d", endLiveRoom),
				fmt.Sprintf("live_session.FindOne:%d", endLiveSess),
				fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", endLiveRoom, endLiveMid),
			}
			if tc.wantClear {
				wantOps = append(wantOps, fmt.Sprintf("live_room.ClearActiveSession:%d/s%d", endLiveRoom, endLiveSess))
			}
			wantOps = append(wantOps, "live_room_idempotency.SaveResult:req-terminal")
			wantSeq(t, tc.name, st.log, 0, wantOps...)
			wantTxCount(t, tc.name+" 不起事务", st.conn, 0)
			wantMethodCount(t, tc.name+" 不迁移场次", st.log, "live_session.TransitionTx", 0)
			wantMethodCount(t, tc.name+" 不迁移房间", st.log, "live_room.TransitionTx", 0)
			wantMethodCount(t, tc.name+" 不补审计", st.log, "live_room_state_log.InsertTx", 0)
			wantEQ(t, tc.name+" 场次行原样", "state", st.sessionAt(t, endLiveSess).State, tc.sessState)
			// 房间状态一个字都没动：下播不解禁、也不把 LIVING 拉回 READY。
			wantEQ(t, tc.name+" 房间态不变", "state", st.roomAt(t, endLiveRoom).State, tc.roomState)
			if tc.wantClear {
				wantEQ(t, tc.name+" 投影已清", "active_session_id", st.roomAt(t, endLiveRoom).ActiveSessionID, int64(0))
			} else {
				// 重放分支只在房间确实 LIVING 时才摘投影（endlivelogic.go:120）：
				// READY 房间上的这条投影由正常下播事务内的那条 UPDATE 清掉，不由重放兜底。
				wantEQ(t, tc.name+" 投影不动", "active_session_id", st.roomAt(t, endLiveRoom).ActiveSessionID, endLiveSess)
			}
		})
	}
}

// TestEndLiveBannedRoomStaysBanned 锁「被禁播的房间下播后仍是 BANNED」：
// 场次照样 ENDED，房间投影用提交后的 ClearActiveSession 摘掉，
// 但不得写 BANNED→READY 那条边（只有 LiftBan 能改，见 model.roomTransitions 注释）。
func TestEndLiveBannedRoomStaysBanned(t *testing.T) {
	fixClock(t, endLiveNow)
	st := newStore()
	r := baseRoom(endLiveRoom, endLiveMid, model.RoomStateBanned)
	r.ActiveSessionID = endLiveSess
	r.ActiveStreamID = endLiveStream
	r.BanUntil = endLiveNow + 3600
	st.seedRoom(r)
	b := baseBan(8801, endLiveRoom, model.BanStateActive)
	b.BanType = model.BanTypeTemporary
	b.StartAt = endLiveNow - 60
	b.EndAt = endLiveNow + 3600
	st.seedBan(b)
	s := baseSession(endLiveSess, endLiveRoom, endLiveMid, model.SessionStateLiving)
	s.StartedAt = endLiveNow - 300
	s.StreamID = endLiveStream
	st.seedSession(s)
	st.seedAnchor(baseAnchor(501, endLiveRoom, endLiveMid, model.AnchorRoleOwner))
	lg := newEndLiveLogic(t, st)

	reply, err := lg.EndLive(endLiveReq("req-banned"))
	wantNoErr(t, "禁播中下播", err)

	wantSeq(t, "禁播中下播的调用面", st.log, 0,
		"live_room_idempotency.Claim:req-banned",
		"live_room.FindOne:3001",
		fmt.Sprintf("live_session.FindActiveByRoom:%d", endLiveRoom),
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", endLiveRoom, endLiveMid),
		"db.TransactCtx",
		fmt.Sprintf("live_session.TransitionTx:%d:2->3/r1", endLiveSess),
		"live_room_state_log.InsertTx:t3:2->3:anchor_stop",
		fmt.Sprintf("live_room.ClearActiveSession:%d/s%d", endLiveRoom, endLiveSess),
		fmt.Sprintf("live_session.FindOne:%d", endLiveSess),
		"live_room.FindOne:3001",
		"live_room_idempotency.SaveResult:req-banned",
	)
	wantMethodCount(t, "不写 BANNED→READY", st.log, "live_room.TransitionTx", 0)
	wantEQ(t, "禁播中下播", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_BANNED)

	row := st.roomAt(t, endLiveRoom)
	wantEQ(t, "房间行", "state", row.State, model.RoomStateBanned)
	wantEQ(t, "房间行", "state_version", row.StateVersion, int32(1))
	wantEQ(t, "房间行", "active_session_id", row.ActiveSessionID, int64(0))
	wantEQ(t, "房间行", "ban_until", row.BanUntil, endLiveNow+3600)
	// 只有场次那一条审计：房间没迁移就不该有 t1 行（审计必须与真实迁移一一对应）。
	wantEQ(t, "审计行数", "logs", len(st.logsOf(endLiveRoom)), 1)
	wantEQ(t, "场次已终态", "state", st.sessionAt(t, endLiveSess).State, model.SessionStateEnded)
}

// TestEndLiveConcurrentRoomVersionBumpLoses 锁房间 CAS 的仲裁：
// 读到 state_version=1 之后、UPDATE 之前被别的入口推走版本，本次必须 ErrConcurrentUpdate。
// 假件不回滚，所以「场次已 ENDED、审计零行」是**这次执行真实留在库里**的形态
// （真实 MySQL 随事务回滚；本用例证明的是「四步同事务」这一事实，不是「残留应当存在」）。
func TestEndLiveConcurrentRoomVersionBumpLoses(t *testing.T) {
	fixClock(t, endLiveNow)
	st := newStore()
	seedEndLiveScene(t, st)
	st.raceBefore("live_room.TransitionTx:3001:3->2", func() {
		r := st.rooms.rows[0]
		r.StateVersion = 7
		r.Mtime = endLiveNow + 1
	})
	lg := newEndLiveLogic(t, st)

	_, err := lg.EndLive(endLiveReq("req-race-room"))
	wantErrIs(t, "房间版本被推走", err, model.ErrConcurrentUpdate)
	st.checkRaces(t)

	wantSeq(t, "并发失败前的调用面", st.log, 0,
		"live_room_idempotency.Claim:req-race-room",
		"live_room.FindOne:3001",
		fmt.Sprintf("live_session.FindActiveByRoom:%d", endLiveRoom),
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", endLiveRoom, endLiveMid),
		"db.TransactCtx",
		fmt.Sprintf("live_session.TransitionTx:%d:2->3/r1", endLiveSess),
		"live_room.TransitionTx:3001:3->2/v1",
	)
	row := st.roomAt(t, endLiveRoom)
	wantEQ(t, "仲裁结果：房间仍在播", "state", row.State, model.RoomStateLiving)
	wantEQ(t, "仲裁结果：版本是插队者的", "state_version", row.StateVersion, int32(7))
	wantEQ(t, "本次未写审计", "logs", len(st.logsOf(endLiveRoom)), 0)
	wantEQ(t, "场次残留", "state", st.sessionAt(t, endLiveSess).State, model.SessionStateEnded)
	wantKeyBurnedNoResult(t, "房间并发失败", "req-race-room", st)
}

// TestEndLiveConcurrentSessionTerminatedLoses 锁场次 CAS 的仲裁：
// 读到场次 LIVING 之后、UPDATE 之前被 BanRoom 抢先终止（TERMINATED + end_reason=BANNED），
// 本次不得把「被禁播」改写成「主播主动下播」——归因不可覆盖。
func TestEndLiveConcurrentSessionTerminatedLoses(t *testing.T) {
	fixClock(t, endLiveNow)
	st := newStore()
	seedEndLiveScene(t, st)
	st.raceBefore("live_session.TransitionTx", func() {
		s := st.sessions.rows[0]
		s.State = model.SessionStateTerminated
		s.EndedAt = endLiveNow
		s.EndReason = model.EndReasonBanned
		s.DurationSeconds = 100
	})
	lg := newEndLiveLogic(t, st)

	_, err := lg.EndLive(endLiveReq("req-race-sess"))
	wantErrIs(t, "场次被抢先终止", err, model.ErrConcurrentUpdate)
	st.checkRaces(t)

	// 场次 CAS 是事务里第一条语句，失败后房间那条根本不该执行。
	wantMethodCount(t, "不迁房间", st.log, "live_room.TransitionTx", 0)
	wantCount(t, "零审计", st.log, "live_room_state_log.InsertTx", 0)
	s := st.sessionAt(t, endLiveSess)
	wantEQ(t, "归因不被覆盖", "end_reason", s.EndReason, model.EndReasonBanned)
	wantEQ(t, "归因不被覆盖", "state", s.State, model.SessionStateTerminated)
	wantEQ(t, "房间原封不动", "state", st.roomAt(t, endLiveRoom).State, model.RoomStateLiving)
	wantEQ(t, "房间版本原封不动", "state_version", st.roomAt(t, endLiveRoom).StateVersion, int32(1))
	wantKeyBurnedNoResult(t, "场次并发失败", "req-race-sess", st)
}

// TestEndLiveAuditFailurePropagatesVerbatim 锁事务内最后一步失败：
// 错误必须是 model 原样抛出的驱动错误，且前两步（场次 ENDED + 房间 READY）已写进同一事务。
func TestEndLiveAuditFailurePropagatesVerbatim(t *testing.T) {
	fixClock(t, endLiveNow)
	st := newStore()
	seedEndLiveScene(t, st)
	boom := errors.New("audit table unavailable")
	st.stateLogs.failWith("InsertTx", boom)
	lg := newEndLiveLogic(t, st)

	_, err := lg.EndLive(endLiveReq("req-audit-fail"))
	wantErrIs(t, "审计写入失败", err, boom)
	wantErrContains(t, "错误原样上抛", err, "live_room_state_log Insert")
	// 第一条（场次）审计都没写成，第二条（房间）自然也没发。
	wantCount(t, "第一条审计都没写成", st.log, "live_room_state_log.InsertTx", 1)
	wantEQ(t, "房间已在事务里回 READY", "state", st.roomAt(t, endLiveRoom).State, model.RoomStateReady)
	wantEQ(t, "场次已在事务里 ENDED", "state", st.sessionAt(t, endLiveSess).State, model.SessionStateEnded)
	// 提交后的回读与摘投影都不该发生（错误直接从 TransactCtx 抛出）。
	wantMethodCount(t, "不回读", st.log, "live_session.FindOne", 0)
	wantMethodCount(t, "不摘投影", st.log, "live_room.ClearActiveSession", 0)
	wantKeyBurnedNoResult(t, "审计失败", "req-audit-fail", st)
}

// TestEndLiveTransitionFailurePropagatesVerbatim 锁两条 CAS 的 DB 失败都是原样上抛，
// 不退化成 ErrConcurrentUpdate（语义不同：一个是「没命中」，一个是「查不动」）。
func TestEndLiveTransitionFailurePropagatesVerbatim(t *testing.T) {
	cases := []struct {
		name string
		fail func(*store, error)
	}{
		{"场次迁移失败", func(s *store, e error) { s.sessions.failWith("TransitionTx", e) }},
		{"房间迁移失败", func(s *store, e error) { s.rooms.failWith("TransitionTx", e) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, endLiveNow)
			st := newStore()
			seedEndLiveScene(t, st)
			boom := errors.New("deadlock retried out")
			tc.fail(st, boom)
			lg := newEndLiveLogic(t, st)

			_, err := lg.EndLive(endLiveReq("req-tx-fail"))
			wantErrIs(t, tc.name, err, boom)
			if errors.Is(err, model.ErrConcurrentUpdate) {
				t.Errorf("%s：错误退化成 ErrConcurrentUpdate，调用方会以为可以安全重试同一条迁移", tc.name)
			}
			wantCount(t, tc.name+" 零审计", st.log, "live_room_state_log.InsertTx", 0)
			wantKeyBurnedNoResult(t, tc.name, "req-tx-fail", st)
		})
	}
}

// TestEndLivePostCommitReadFailureReportsErrorWithCommittedWrites 锁事务边界另一侧：
// 状态已经在事务里提交完成，之后的回读失败仍然对外报错——
// 「写已生效」与「应答失败」同时成立，客户端**不能**凭错误重试同一 request_id 期待幂等成功
// （这里会把键停在「已烧无结果」形态）。
func TestEndLivePostCommitReadFailureReportsErrorWithCommittedWrites(t *testing.T) {
	fixClock(t, endLiveNow)
	st := newStore()
	seedEndLiveScene(t, st)
	lg := newEndLiveLogic(t, st)
	// 先正常走完一次，拿到「已提交」的形态；再注入回读失败重放同一场。
	_, errOK := lg.EndLive(endLiveReq("req-ok"))
	wantNoErr(t, "前置下播", errOK)

	st2 := newStore()
	seedEndLiveScene(t, st2)
	boom := errors.New("read replica lag")
	lg2 := newEndLiveLogic(t, st2)
	// 回读发生在事务提交之后：用钩子在那一刻把场次行删掉，模拟「读不到」。
	st2.raceBefore("live_room.TransitionTx:3001:3->2", func() {
		st2.sessions.failWith("FindOne", boom)
	})
	_, err := lg2.EndLive(endLiveReq("req-postcommit"))
	wantErrIs(t, "提交后回读失败", err, boom)
	st2.checkRaces(t)
	wantEQ(t, "状态变更已提交", "state", st2.sessionAt(t, endLiveSess).State, model.SessionStateEnded)
	wantEQ(t, "房间已回 READY", "state", st2.roomAt(t, endLiveRoom).State, model.RoomStateReady)
	wantCount(t, "两条审计都已提交", st2.log, "live_room_state_log.InsertTx", 2)
	wantMethodCount(t, "结果没回填", st2.log, "live_room_idempotency.SaveResult", 0)
	wantKeyBurnedNoResult(t, "提交后回读失败", "req-postcommit", st2)
}

// TestEndLiveReadFailuresPropagateVerbatim 锁「依赖错误不退化」：
// 抢键、读房间、读进行中场次、读绑定的每一处失败都必须原样上抛。
func TestEndLiveReadFailuresPropagateVerbatim(t *testing.T) {
	cases := []struct {
		name  string
		fail  func(*store, error)
		until int
	}{
		{"抢键失败", func(s *store, e error) { s.idem.failWith("Claim", e) }, 1},
		{"读房间失败", func(s *store, e error) { s.rooms.failWith("FindOne", e) }, 2},
		{"读进行中场次失败", func(s *store, e error) { s.sessions.failWith("FindActiveByRoom", e) }, 3},
		{"读点名场次失败", func(s *store, e error) { s.sessions.failWith("FindOne", e) }, 3},
		{"读绑定失败", func(s *store, e error) { s.anchors.failWith("IsEnabled", e) }, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, endLiveNow)
			st := newStore()
			seedEndLiveScene(t, st)
			boom := errors.New("db down")
			tc.fail(st, boom)
			lg := newEndLiveLogic(t, st)

			in := endLiveReq("req-read-fail")
			if tc.name == "读点名场次失败" {
				in.SessionId = endLiveSess
			}
			_, err := lg.EndLive(in)
			wantErrIs(t, tc.name, err, boom)
			if got := len(st.log.ops); got != tc.until {
				t.Errorf("%s：调用数 = %d, want %d（轨迹 %v）", tc.name, got, tc.until, st.log.ops)
			}
			wantTxCount(t, tc.name, st.conn, 0)
			wantEQ(t, tc.name+" 状态不变", "state", st.sessionAt(t, endLiveSess).State, model.SessionStateLiving)
		})
	}
}

// TestEndLiveRoomMissing 锁「房间不存在」。
func TestEndLiveRoomMissing(t *testing.T) {
	fixClock(t, endLiveNow)
	st := newStore()
	lg := newEndLiveLogic(t, st)

	_, err := lg.EndLive(endLiveReq("req-noroom"))
	wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
	wantSeq(t, "房间不存在", st.log, 0,
		"live_room_idempotency.Claim:req-noroom",
		"live_room.FindOne:3001",
	)
	wantKeyBurnedNoResult(t, "房间不存在", "req-noroom", st)
}

// TestEndLiveInputGuardsRunBeforeAnyCall 锁「入参守卫在抢键之前」：零依赖调用 + 键不烧。
func TestEndLiveInputGuardsRunBeforeAnyCall(t *testing.T) {
	longReq := strings.Repeat("x", maxDedupIDBytes+1)
	cases := []struct {
		name string
		edit func(*rpc.EndLiveReq)
		want error
		frag string
	}{
		{"room_id 非正", func(in *rpc.EndLiveReq) { in.RoomId = 0 }, model.ErrInvalidRoomID, ""},
		{"mid 非正", func(in *rpc.EndLiveReq) { in.Mid = -1 }, model.ErrInvalidMid, ""},
		{"缺 request_id", func(in *rpc.EndLiveReq) { in.RequestId = "  " }, model.ErrRequestIDRequired, ""},
		{"request_id 超列宽", func(in *rpc.EndLiveReq) { in.RequestId = longReq },
			model.ErrDedupIDTooLong, "max 64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, endLiveNow)
			st := newStore()
			seedEndLiveScene(t, st)
			lg := newEndLiveLogic(t, st)

			in := endLiveReq("req-guard")
			tc.edit(in)
			_, err := lg.EndLive(in)
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
		fixClock(t, endLiveNow)
		st := newStore()
		lg := newEndLiveLogic(t, st)
		_, err := lg.EndLive(nil)
		wantErrIs(t, "nil 请求", err, model.ErrInvalidRoomID)
		wantNoCallAfter(t, "nil 请求", st.log, 0)
	})
}

// TestEndLiveRequestIdTriState 是任务书第 1 条的完整三态：
// 首次受理 / 命中重放（逐字段原值 + replayed=true + 零新写）/ 键被别的方法用过必须报错。
func TestEndLiveRequestIdTriState(t *testing.T) {
	fixClock(t, endLiveNow)

	t.Run("命中重放回原值且零新写", func(t *testing.T) {
		st := newStore()
		seedEndLiveScene(t, st)
		lg := newEndLiveLogic(t, st)

		first, err := lg.EndLive(endLiveReq("req-replay"))
		wantNoErr(t, "EndLive 首次", err)
		before := st.counts()
		txBefore := st.conn.transactions
		from := st.log.snapshot()

		second, err := lg.EndLive(endLiveReq("req-replay"))
		wantNoErr(t, "EndLive 重放", err)
		defer st.checkRaces(t)

		wantEQ(t, "重放", "replayed", second.GetReplayed(), true)
		wantEQ(t, "重放", "session_id", second.GetSessionId(), first.GetSessionId())
		wantEQ(t, "重放", "session_state", second.GetSessionState(), first.GetSessionState())
		wantEQ(t, "重放", "room_state", second.GetRoomState(), first.GetRoomState())
		wantEQ(t, "重放", "duration_seconds", second.GetDurationSeconds(), first.GetDurationSeconds())

		wantSeq(t, "重放只准读键", st.log, from,
			"live_room_idempotency.Claim:req-replay",
			"live_room_idempotency.Find:req-replay",
		)
		if got := st.counts(); got != before {
			t.Errorf("重放后行数 = %+v, want 与首次一致 %+v", got, before)
		}
		wantTxCount(t, "重放不起事务", st.conn, txBefore)
	})

	t.Run("重放回的是首次那一刻的快照", func(t *testing.T) {
		st := newStore()
		seedEndLiveScene(t, st)
		st.seedIdem("req-stored", "EndLive",
			`{"session_id":77,"session_state":3,"room_state":2,"duration_seconds":42}`)
		lg := newEndLiveLogic(t, st)

		reply, err := lg.EndLive(endLiveReq("req-stored"))
		wantNoErr(t, "EndLive 重放已存结果", err)
		wantEQ(t, "重放", "replayed", reply.GetReplayed(), true)
		wantEQ(t, "重放", "session_id", reply.GetSessionId(), int64(77))
		wantEQ(t, "重放", "session_state", reply.GetSessionState(), rpc.SessionState_SESSION_STATE_ENDED)
		wantEQ(t, "重放", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_READY)
		wantEQ(t, "重放", "duration_seconds", reply.GetDurationSeconds(), int64(42))
		wantNoCallAfter(t, "重放", st.log, 2)
		wantEQ(t, "库里没动过", "state", st.sessionAt(t, endLiveSess).State, model.SessionStateLiving)
	})

	t.Run("键被别的方法用过必须报错", func(t *testing.T) {
		st := newStore()
		seedEndLiveScene(t, st)
		st.seedIdem("shared-key", "CloseRoom", `{"state":4}`)
		lg := newEndLiveLogic(t, st)

		reply, err := lg.EndLive(endLiveReq("shared-key"))
		wantErrIs(t, "复用别人的键", err, model.ErrRequestIDReused)
		wantErrContains(t, "错误归因", err, "used by CloseRoom")
		if reply != nil {
			t.Errorf("应答 = %+v, want nil（不能把别的方法的结果当重放发出去）", reply)
		}
		wantSeq(t, "抢键后回查即止", st.log, 0,
			"live_room_idempotency.Claim:shared-key",
			"live_room_idempotency.Find:shared-key",
		)
		wantEQ(t, "既有行不被改写", "result_json", st.idemAt("shared-key").ResultJSON, `{"state":4}`)
		wantEQ(t, "既有行不被改写", "rpc", st.idemAt("shared-key").Rpc, "CloseRoom")
	})

	t.Run("键已烧但结果未就绪", func(t *testing.T) {
		st := newStore()
		seedEndLiveScene(t, st)
		st.seedIdem("half-done", "EndLive", "")
		lg := newEndLiveLogic(t, st)

		_, err := lg.EndLive(endLiveReq("half-done"))
		wantErrIs(t, "结果未就绪", err, model.ErrIdempotencyResultMissing)
		wantKeyBurnedNoResult(t, "半完成态", "half-done", st)
		wantSeq(t, "回查后必须止步", st.log, 0,
			"live_room_idempotency.Claim:half-done",
			"live_room_idempotency.Find:half-done",
		)
	})
}
