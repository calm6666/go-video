package logic

// closeroom_logic_test.go 覆盖写侧方法 CloseRoom（closeroomlogic.go）。
//
// 锁的结论（全部由 closeroomlogic.go 的实现反推，不是理想设计）：
//  1. **FINISHED 是本服务唯一的「关房即终态」入口**：`model.RoomStateIsTerminal` 只认 FINISHED
//     （model/errors.go:560-562），所以已在 FINISHED 的房间走幂等重放语义（replayed=true、
//     零写入、不抢键），而 BANNED / DISABLED **不是**终态，照样能被关闭（矩阵里三者都有出边）。
//  2. expectVersion 恒传 0：closeroomlogic.go:131 的迁移条件是 `state=from` 而不是版本号，
//     关房因此**不参与乐观锁**——并发下第二条关闭语句命中 0 行才退回 ErrConcurrentUpdate。
//  3. 在播场次被强制终止与房间迁入 FINISHED 在**同一个事务**里，且顺序固定：
//     先场次、再审计（t3）、再房间、再审计（t1）。假件不回滚，所以中途失败时
//     「场次已 TERMINATED 而房间还 LIVING」这种半截形态是可观察的事实，逐条断言。
//  4. `admin=false` 时只有生效房主能关自己的房间，且这道归属校验在**抢幂等键之前**
//     （closeroomlogic.go:72-81）：陌生操作者被拒不烧键。`admin=true` 时完全不读 FindOwner。
//  5. 关房走 clearActiveSessionPatch()：active_session_id / active_stream_id 与状态迁移
//     在**同一条** UPDATE 里清掉（对比 BanRoom 的缺口：禁播只写 ban_until，留着挂机位）。
//  6. source 只有两个取值：admin=true → rpc_admin，否则 rpc_client；reason 只进
//     live_room_state_log，不进应答。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

const (
	clNow      int64 = 1_700_000_200
	clStart    int64 = 1_700_000_000
	clRoom     int64 = 3101
	clOwner    int64 = 9101
	clStranger int64 = 9999
	clSess     int64 = 5001
	clReason         = "下播清退"
)

// clReq 组一条除幂等键外都合法的关房请求。
func clReq(reqID string, admin bool) *rpc.CloseRoomReq {
	return &rpc.CloseRoomReq{
		RoomId: clRoom, OperatorMid: clOwner, Admin: admin,
		Reason: clReason, RequestId: reqID, TraceId: "trace-close",
	}
}

func newCloseLogic(t *testing.T, st *store) *CloseRoomLogic {
	t.Helper()
	return NewCloseRoomLogic(context.Background(), st.svcCtx())
}

// seedCloseScene 布「某状态的房间 + 生效房主」。
// 房主行始终布上：非终态分支要么用它做归属校验，要么被 admin=true 跳过。
func seedCloseScene(t *testing.T, st *store, state int32) *model.LiveRoom {
	t.Helper()
	r := baseRoom(clRoom, clOwner, state)
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(801, clRoom, clOwner, model.AnchorRoleOwner))
	return r
}

// seedCloseLivingScene 在上面再加一条进行中场次（房间处于 LIVING、挂机位已登记）。
func seedCloseLivingScene(t *testing.T, st *store) *model.LiveRoom {
	t.Helper()
	r := seedCloseScene(t, st, model.RoomStateLiving)
	r.ActiveSessionID = clSess
	r.ActiveStreamID = "stream-cl"
	st.rooms.rows[0] = r
	sess := baseSession(clSess, clRoom, clOwner, model.SessionStateLiving)
	sess.StartedAt = clStart
	sess.StreamID = "stream-cl"
	st.seedSession(sess)
	return r
}

// TestCloseRoomLivingRoomTerminatesSessionAndFinishesRoom 是本方法的骨架用例：
// 一条在播场次 + 房主自关，锁全序列、两侧写入与两条审计行的归因。
func TestCloseRoomLivingRoomTerminatesSessionAndFinishesRoom(t *testing.T) {
	fixClock(t, clNow)
	st := newStore()
	seedCloseLivingScene(t, st)

	reply, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-live", false))
	wantNoErr(t, "关闭在播房间", err)
	defer st.checkRaces(t)

	wantSeq(t, "关房首次执行", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", clRoom),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", clRoom),
		"live_room_idempotency.Claim:req-close-live",
		fmt.Sprintf("live_session.FindActiveByRoom:%d", clRoom),
		"db.TransactCtx",
		fmt.Sprintf("live_session.TransitionTx:%d:%d->%d/r%d",
			clSess, model.SessionStateLiving, model.SessionStateTerminated, model.EndReasonRoomClosed),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeSessionState, model.SessionStateLiving, model.SessionStateTerminated, clReason),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			clRoom, model.RoomStateLiving, model.RoomStateFinished, 0),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateLiving, model.RoomStateFinished, clReason),
		"live_room_idempotency.SaveResult:req-close-live",
	)
	wantTxCount(t, "关房", st.conn, 1)
	// 一次只有一个事务：场次与房间不得各自开一个。
	wantMethodCount(t, "关房", st.log, "db.TransactCtx", 1)
	wantMethodCount(t, "关房", st.log, "live_room.Transition", 0)
	// 挂机位由同一条迁移的 patch 清掉，不需要额外一句 ClearActiveSession。
	wantMethodCount(t, "关房", st.log, "live_room.ClearActiveSession", 0)

	wantEQ(t, "关房应答", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_FINISHED)
	wantEQ(t, "关房应答", "terminated_session_id", reply.GetTerminatedSessionId(), clSess)
	wantEQ(t, "关房应答", "replayed", reply.GetReplayed(), false)

	room := st.roomAt(t, clRoom)
	wantEQ(t, "关房后的房间", "state", room.State, model.RoomStateFinished)
	wantEQ(t, "关房后的房间", "state_version 只 +1", room.StateVersion, int32(2))
	wantEQ(t, "关房后的房间", "active_session_id 被清", room.ActiveSessionID, int64(0))
	wantEQ(t, "关房后的房间", "active_stream_id 被清", room.ActiveStreamID, "")
	wantEQ(t, "关房后的房间", "mtime", room.Mtime, clNow)
	wantEQ(t, "关房后的房间", "ctime", room.Ctime, 1000+clRoom)
	// reason 不进任何终端可见字段：应答里没有 reason 位，库里也只有审计行带它。

	sess := st.sessionAt(t, clSess)
	wantEQ(t, "被终止场次", "state", sess.State, model.SessionStateTerminated)
	wantEQ(t, "被终止场次", "end_reason", sess.EndReason, model.EndReasonRoomClosed)
	wantEQ(t, "被终止场次", "ended_at", sess.EndedAt, clNow)
	wantEQ(t, "被终止场次", "duration", sess.DurationSeconds, clNow-clStart)

	logs := st.logsOf(clRoom)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2（场次态 + 房间态）", len(logs))
	}
	wantEQ(t, "场次审计", "state_type", logs[0].StateType, model.LogTypeSessionState)
	wantEQ(t, "场次审计", "from->to", fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "2->4")
	wantEQ(t, "场次审计", "session_id", logs[0].SessionID, clSess)
	wantEQ(t, "房间审计", "state_type", logs[1].StateType, model.LogTypeRoomState)
	wantEQ(t, "房间审计", "from->to", fmt.Sprintf("%d->%d", logs[1].FromState, logs[1].ToState), "3->4")
	for i, l := range logs {
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "source", l.Source, model.SourceRPCClient)
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "operator_mid", l.OperatorMid, clOwner)
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "request_id", l.RequestID, "req-close-live")
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "event_id", l.EventID, "")
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "reason", l.Reason, clReason)
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "trace_id", l.TraceID, "trace-close")
	}
	wantDeepEQ(t, "关房落库面", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, sessions: 1, logs: 2, idem: 1})
}

// TestCloseRoomAdminFlagSkipsOwnerLookupAndChangesSource 钉住 admin 位的两件事：
// 归属校验整条被跳过（FindOwner 调用次数 0），审计 source 从 rpc_client 换成 rpc_admin。
// 房间刻意用 READY 且无场次，好让 source 断言不被场次写入干扰。
func TestCloseRoomAdminFlagSkipsOwnerLookupAndChangesSource(t *testing.T) {
	fixClock(t, clNow)
	st := newStore()
	seedCloseScene(t, st, model.RoomStateReady)

	reply, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-admin", true))
	wantNoErr(t, "运营关房", err)

	wantSeq(t, "运营关房", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", clRoom),
		"live_room_idempotency.Claim:req-close-admin",
		fmt.Sprintf("live_session.FindActiveByRoom:%d", clRoom),
		"db.TransactCtx",
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			clRoom, model.RoomStateReady, model.RoomStateFinished, 0),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateReady, model.RoomStateFinished, clReason),
		"live_room_idempotency.SaveResult:req-close-admin",
	)
	wantMethodCount(t, "运营关房不查房主", st.log, "live_room_anchor.FindOwner", 0)
	wantEQ(t, "运营关房应答", "terminated_session_id", reply.GetTerminatedSessionId(), int64(0))
	logs := st.logsOf(clRoom)
	if len(logs) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(logs))
	}
	wantEQ(t, "运营关房审计", "source", logs[0].Source, model.SourceRPCAdmin)
	wantEQ(t, "运营关房审计", "state_type", logs[0].StateType, model.LogTypeRoomState)
	// 无场次时 terminated_session_id 是 0，审计行的 session_id 也必须是 0（不是脏继承）。
	wantEQ(t, "运营关房审计", "session_id", logs[0].SessionID, int64(0))
	wantEQ(t, "运营关房", "场次行数", st.counts().sessions, 0)
}

// TestCloseRoomWithoutActiveSessionWritesOnlyRoomRows 与骨架用例成对：
// 房间处于 LIVING 但没有任何进行中场次（异常投影，model 的 FindActiveByRoom 只看场次表），
// 关房不得凭空造场次终止写，也不得报错。
func TestCloseRoomWithoutActiveSessionWritesOnlyRoomRows(t *testing.T) {
	fixClock(t, clNow)
	st := newStore()
	r := seedCloseScene(t, st, model.RoomStateLiving)
	r.ActiveSessionID = clSess // 房间投影里留着老场次号，但场次表里没有生效场次
	st.rooms.rows[0] = r
	// 一条早已结束的场次：FindActiveByRoom 不得把它当生效场次。
	done := baseSession(clSess, clRoom, clOwner, model.SessionStateEnded)
	st.seedSession(done)

	reply, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-nosess", false))
	wantNoErr(t, "无生效场次关房", err)

	wantSeq(t, "无生效场次关房", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", clRoom),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", clRoom),
		"live_room_idempotency.Claim:req-close-nosess",
		fmt.Sprintf("live_session.FindActiveByRoom:%d", clRoom),
		"db.TransactCtx",
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			clRoom, model.RoomStateLiving, model.RoomStateFinished, 0),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateLiving, model.RoomStateFinished, clReason),
		"live_room_idempotency.SaveResult:req-close-nosess",
	)
	wantMethodCount(t, "无生效场次关房", st.log, "live_session.TransitionTx", 0)
	wantEQ(t, "无生效场次关房应答", "terminated_session_id", reply.GetTerminatedSessionId(), int64(0))
	// 已结束场次原样不动。
	wantEQ(t, "无生效场次关房", "老场次 state", st.sessionAt(t, clSess).State, model.SessionStateEnded)
	wantEQ(t, "无生效场次关房", "审计行数", len(st.logsOf(clRoom)), 1)
}

// TestCloseRoomAlreadyFinishedIsIdempotentReplay 钉住终态分支：
// 目标态已达成 → replayed=true，且**一次依赖调用都不许在 FindOne 之后发生**
// （不抢键、不开事务、不写审计）。BANNED / DISABLED 作为对照：它们不是终态，照样关得掉。
func TestCloseRoomAlreadyFinishedIsIdempotentReplay(t *testing.T) {
	fixClock(t, clNow)
	st := newStore()
	seedCloseScene(t, st, model.RoomStateFinished)

	reply, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-done", false))
	wantNoErr(t, "重复关房", err)
	wantSeq(t, "重复关房", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", clRoom))
	wantEQ(t, "重复关房应答", "replayed", reply.GetReplayed(), true)
	wantEQ(t, "重复关房应答", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_FINISHED)
	wantEQ(t, "重复关房应答", "terminated_session_id", reply.GetTerminatedSessionId(), int64(0))
	wantKeyUnburned(t, "重复关房", "req-close-done", st)
	wantTxCount(t, "重复关房", st.conn, 0)
	wantEQ(t, "重复关房", "state_version 不动", st.roomAt(t, clRoom).StateVersion, int32(1))
	wantEQ(t, "重复关房", "审计行数", len(st.logsOf(clRoom)), 0)
}

// TestCloseRoomBannedAndDisabledAreNotTerminal 是上一条的判别对照：
// 只有 FINISHED 算终态，所以被禁播/被停用的房间关得掉（矩阵里 5->4、6->4 都存在）。
func TestCloseRoomBannedAndDisabledAreNotTerminal(t *testing.T) {
	for _, from := range []int32{model.RoomStateBanned, model.RoomStateDisabled} {
		t.Run(fmt.Sprintf("state=%d", from), func(t *testing.T) {
			fixClock(t, clNow)
			st := newStore()
			seedCloseScene(t, st, from)

			reply, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-nonliving", true))
			wantNoErr(t, "关闭非在播房间", err)
			wantEQ(t, "关闭非在播房间", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_FINISHED)
			wantEQ(t, "关闭非在播房间", "replayed", reply.GetReplayed(), false)
			wantSeq(t, "关闭非在播房间", st.log, 0,
				fmt.Sprintf("live_room.FindOne:%d", clRoom),
				"live_room_idempotency.Claim:req-close-nonliving",
				fmt.Sprintf("live_session.FindActiveByRoom:%d", clRoom),
				"db.TransactCtx",
				fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d", clRoom, from, model.RoomStateFinished, 0),
				fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
					model.LogTypeRoomState, from, model.RoomStateFinished, clReason),
				"live_room_idempotency.SaveResult:req-close-nonliving",
			)
			// 禁播投影不因关房而清：ban_until 保持原值（关房 patch 里没有它）。
			wantEQ(t, "关闭非在播房间", "ban_until", st.roomAt(t, clRoom).BanUntil, int64(0))
		})
	}
}

// TestCloseRoomOwnerGuardRunsBeforeClaim 钉住第 4 条：归属被拒不烧键。
// 三条子例覆盖「有房主但不是他」「根本没有生效房主」「房主行已停用」。
func TestCloseRoomOwnerGuardRunsBeforeClaim(t *testing.T) {
	t.Run("操作者不是房主", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateReady)
		in := clReq("req-close-notowner", false)
		in.OperatorMid = clStranger

		_, err := newCloseLogic(t, st).CloseRoom(in)
		wantErrIs(t, "非房主关房", err, model.ErrAnchorNotOwner)
		wantErrContains(t, "非房主关房", err, "关闭需 admin=true")
		wantSeq(t, "非房主关房", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", clRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", clRoom),
		)
		wantKeyUnburned(t, "非房主关房", "req-close-notowner", st)
		wantTxCount(t, "非房主关房", st.conn, 0)
		wantEQ(t, "非房主关房", "房间 state", st.roomAt(t, clRoom).State, model.RoomStateReady)
	})

	t.Run("房间没有生效房主", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		// 只布房主行然后停用，等价于「查不到生效房主」（FindOwner 的条件含 state=1）。
		a := baseAnchor(801, clRoom, clOwner, model.AnchorRoleOwner)
		a.State = model.BindStateDisabled
		a.OwnerRoomID = sql.NullInt64{}
		st.seedRoom(baseRoom(clRoom, clOwner, model.RoomStateReady))
		st.seedAnchor(a)

		_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-noowner", false))
		wantErrIs(t, "无生效房主", err, model.ErrAnchorNotOwner)
		wantSeq(t, "无生效房主", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", clRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", clRoom),
		)
		wantKeyUnburned(t, "无生效房主", "req-close-noowner", st)
	})

	t.Run("admin=true 时同一个陌生操作者放行", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateReady)
		in := clReq("req-close-admin-stranger", true)
		in.OperatorMid = clStranger

		_, err := newCloseLogic(t, st).CloseRoom(in)
		wantNoErr(t, "运营侧陌生操作者", err)
		wantMethodCount(t, "运营侧陌生操作者", st.log, "live_room_anchor.FindOwner", 0)
		logs := st.logsOf(clRoom)
		// 归因照抄入参：运营侧的 operator_mid 由 gateway/admin 判定后带进来，本服务不复核。
		wantEQ(t, "运营侧陌生操作者", "operator_mid", logs[0].OperatorMid, clStranger)
		wantEQ(t, "运营侧陌生操作者", "source", logs[0].Source, model.SourceRPCAdmin)
	})
}

// TestCloseRoomGuardTableRejectsWithZeroDependencyCalls 入参守卫表：
// 每一项都必须在**触库之前**被拒（房间与房主都布好了，走通就会写库）。
func TestCloseRoomGuardTableRejectsWithZeroDependencyCalls(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.CloseRoomReq)
		want   error
		frag   string
	}{
		{"缺房间号", func(in *rpc.CloseRoomReq) { in.RoomId = 0 }, model.ErrInvalidRoomID, ""},
		{"房间号为负", func(in *rpc.CloseRoomReq) { in.RoomId = -7 }, model.ErrInvalidRoomID, ""},
		{"缺操作者", func(in *rpc.CloseRoomReq) { in.OperatorMid = 0 }, model.ErrOperatorRequired, ""},
		{"操作者为负", func(in *rpc.CloseRoomReq) { in.OperatorMid = -1 }, model.ErrOperatorRequired, ""},
		{"幂等键空", func(in *rpc.CloseRoomReq) { in.RequestId = "" }, model.ErrRequestIDRequired, ""},
		{"幂等键纯空白", func(in *rpc.CloseRoomReq) { in.RequestId = "   " }, model.ErrRequestIDRequired, ""},
		{"幂等键超长", func(in *rpc.CloseRoomReq) {
			in.RequestId = strings.Repeat("r", maxDedupIDBytes+1)
		}, model.ErrDedupIDTooLong, ""},
		{"原因超长", func(in *rpc.CloseRoomReq) {
			in.Reason = strings.Repeat("故", maxReasonRunes+1)
		}, model.ErrReasonTooLong, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, clNow)
			st := newStore()
			seedCloseScene(t, st, model.RoomStateLiving)
			in := clReq("req-close-guard", false)
			tc.mutate(in)

			before := st.log.snapshot()
			reply, err := newCloseLogic(t, st).CloseRoom(in)
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name, err, tc.frag)
			}
			if reply != nil {
				t.Fatalf("%s：应答 = %+v, want nil", tc.name, reply)
			}
			wantNoCallAfter(t, tc.name, st.log, before)
			wantTxCount(t, tc.name, st.conn, 0)
			wantNoDirectSQL(t, tc.name, st.conn)
		})
	}

	t.Run("空 reason 合法（关房不强制填原因）", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateReady)
		in := clReq("req-close-emptyreason", true)
		in.Reason = "   "

		_, err := newCloseLogic(t, st).CloseRoom(in)
		wantNoErr(t, "空原因关房", err)
		wantEQ(t, "空原因关房", "审计 reason 归一为空串", st.logsOf(clRoom)[0].Reason, "")
	})

	t.Run("nil 请求", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		lg := newCloseLogic(t, st)
		before := st.log.snapshot()
		_, err := lg.CloseRoom(nil)
		wantErrIs(t, "nil 请求", err, model.ErrInvalidRoomID)
		wantNoCallAfter(t, "nil 请求", st.log, before)
	})
}

// TestCloseRoomMissingRoomRejectedWithoutClaim 房间不存在必须在抢键之前拒绝。
func TestCloseRoomMissingRoomRejectedWithoutClaim(t *testing.T) {
	fixClock(t, clNow)
	st := newStore()

	_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-missing", true))
	wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
	wantSeq(t, "房间不存在", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", clRoom))
	wantKeyUnburned(t, "房间不存在", "req-close-missing", st)
	wantTxCount(t, "房间不存在", st.conn, 0)
}

// TestCloseRoomReplayIdempotencyTriad 幂等三态：回读原结果 / 键被别的方法用过 /
// 键被烧掉但没有结果。三条共用一条轨迹前缀（FindOne + FindOwner + Claim + Find）。
func TestCloseRoomReplayIdempotencyTriad(t *testing.T) {
	t.Run("回读首次结果", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateReady)
		st.seedIdem("req-close-replay", rpcCloseRoom,
			fmt.Sprintf(`{"state":%d,"terminated_session_id":%d}`, model.RoomStateFinished, clSess))

		reply, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-replay", false))
		wantNoErr(t, "关房重放", err)
		wantSeq(t, "关房重放", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", clRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", clRoom),
			"live_room_idempotency.Claim:req-close-replay",
			"live_room_idempotency.Find:req-close-replay",
		)
		wantEQ(t, "关房重放", "replayed", reply.GetReplayed(), true)
		wantEQ(t, "关房重放", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_FINISHED)
		wantEQ(t, "关房重放", "terminated_session_id", reply.GetTerminatedSessionId(), clSess)
		// 回读的是**快照**而不是当前投影：房间还 READY，应答照样说 FINISHED。
		wantEQ(t, "关房重放残留", "房间 state", st.roomAt(t, clRoom).State, model.RoomStateReady)
		wantTxCount(t, "关房重放", st.conn, 0)
		wantEQ(t, "关房重放残留", "审计行数", len(st.logsOf(clRoom)), 0)
	})

	t.Run("键被别的方法用过", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateReady)
		st.seedIdem("shared-close", rpcBanRoom, `{"ban_id":1}`)

		_, err := newCloseLogic(t, st).CloseRoom(clReq("shared-close", true))
		wantErrIs(t, "键冲突", err, model.ErrRequestIDReused)
		wantErrContains(t, "键冲突", err, "used by BanRoom")
		wantSeq(t, "键冲突", st.log, 1,
			"live_room_idempotency.Claim:shared-close",
			"live_room_idempotency.Find:shared-close",
		)
		wantTxCount(t, "键冲突", st.conn, 0)
	})

	t.Run("烧过的键没有结果", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateReady)
		st.seedIdem("burned-close", rpcCloseRoom, "")

		reply, err := newCloseLogic(t, st).CloseRoom(clReq("burned-close", true))
		wantErrIs(t, "烧过的键", err, model.ErrIdempotencyResultMissing)
		if reply != nil {
			t.Fatalf("应答 = %+v, want nil", reply)
		}
		wantSeq(t, "烧过的键", st.log, 1,
			"live_room_idempotency.Claim:burned-close",
			"live_room_idempotency.Find:burned-close",
		)
		wantTxCount(t, "烧过的键", st.conn, 0)
	})
}

// TestCloseRoomSessionCASMissAbortsBeforeRoomTransition 复现「读到场次之后、
// 事务里终止之前，别的入口把场次收掉了」：假件不回滚，但这次没有任何前置写，
// 所以残留只有一条烧掉没回填的键，房间一行都没动。
func TestCloseRoomSessionCASMissAbortsBeforeRoomTransition(t *testing.T) {
	fixClock(t, clNow)
	st := newStore()
	defer st.checkRaces(t)
	seedCloseLivingScene(t, st)
	st.raceBefore("live_session.TransitionTx", func() {
		// EndLive 抢先收掉场次（ENDED 而非 TERMINATED，落点原因也不同）。
		for _, s := range st.sessions.rows {
			if s.SessionID == clSess {
				s.State = model.SessionStateEnded
				s.EndReason = model.EndReasonAnchorStop
				s.EndedAt = clNow - 1
			}
		}
	})

	_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-race", true))
	wantErrIs(t, "场次 CAS 未命中", err, model.ErrConcurrentUpdate)
	wantTxCount(t, "场次 CAS 未命中", st.conn, 1)
	wantSeq(t, "场次 CAS 未命中", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", clRoom),
		"live_room_idempotency.Claim:req-close-race",
		fmt.Sprintf("live_session.FindActiveByRoom:%d", clRoom),
		"db.TransactCtx",
		fmt.Sprintf("live_session.TransitionTx:%d:%d->%d/r%d",
			clSess, model.SessionStateLiving, model.SessionStateTerminated, model.EndReasonRoomClosed),
	)
	wantKeyBurnedNoResult(t, "场次 CAS 未命中", "req-close-race", st)
	wantEQ(t, "场次 CAS 未命中残留", "房间 state", st.roomAt(t, clRoom).State, model.RoomStateLiving)
	wantEQ(t, "场次 CAS 未命中残留", "房间挂机位", st.roomAt(t, clRoom).ActiveSessionID, clSess)
	wantEQ(t, "场次 CAS 未命中残留", "审计行数", len(st.logsOf(clRoom)), 0)
	wantEQ(t, "场次 CAS 未命中残留", "场次终态原因是别人的", st.sessionAt(t, clSess).EndReason,
		model.EndReasonAnchorStop)
}

// TestCloseRoomRoomCASMissLeavesTerminatedSession 是本方法最能暴露「假件不回滚」的一条：
// 场次已经被终止、t3 审计已经落库，房间迁移却因并发抢跑而命中 0 行。
// 真实 MySQL 会回滚这两笔写，所以这里断言的是**测试环境下的残留事实**，
// 并且它同时说明：关房的房间 CAS 用的是 state=from，房间被并发推进就一定失败。
func TestCloseRoomRoomCASMissLeavesTerminatedSession(t *testing.T) {
	fixClock(t, clNow)
	st := newStore()
	seedCloseLivingScene(t, st)
	st.raceBefore("live_room.TransitionTx:3101:3->4", func() {
		for _, r := range st.rooms.rows {
			if r.RoomID == clRoom {
				r.State = model.RoomStateReady // 别的入口把场次收掉并把房间退回 READY
				r.StateVersion++
			}
		}
	})

	_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-roomrace", true))
	wantErrIs(t, "房间 CAS 未命中", err, model.ErrConcurrentUpdate)
	wantSeq(t, "房间 CAS 未命中", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", clRoom),
		"live_room_idempotency.Claim:req-close-roomrace",
		fmt.Sprintf("live_session.FindActiveByRoom:%d", clRoom),
		"db.TransactCtx",
		fmt.Sprintf("live_session.TransitionTx:%d:%d->%d/r%d",
			clSess, model.SessionStateLiving, model.SessionStateTerminated, model.EndReasonRoomClosed),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeSessionState, model.SessionStateLiving, model.SessionStateTerminated, clReason),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d", clRoom, model.RoomStateLiving,
			model.RoomStateFinished, 0),
	)
	// 残留：场次已终态 + 一条场次审计；房间没关，键烧了没结果。
	wantEQ(t, "残留", "场次 state", st.sessionAt(t, clSess).State, model.SessionStateTerminated)
	wantEQ(t, "残留", "房间 state", st.roomAt(t, clRoom).State, model.RoomStateReady)
	wantEQ(t, "残留", "房间挂机位未清", st.roomAt(t, clRoom).ActiveSessionID, clSess)
	wantDeepEQ(t, "残留", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, sessions: 1, logs: 1, idem: 1})
	wantKeyBurnedNoResult(t, "残留", "req-close-roomrace", st)
}

// TestCloseRoomAuditFailureInsideTxLeavesRowsMovedWithoutAudit 钉住第 3 条的顺序事实：
// 关房事务里**场次审计写在房间迁移之前**，所以一旦审计写失败，失败点取决于有没有在播场次。
// 假件不回滚，因此这里断言的是测试环境下真实残留的形态（真 MySQL 会整笔回滚），
// 两点都必须成立：错误原样抛出、键烧了不回填结果。
func TestCloseRoomAuditFailureInsideTxLeavesRowsMovedWithoutAudit(t *testing.T) {
	auditErr := errors.New("audit table down")

	// 无场次：事务里只有房间迁移 + t1 审计，失败落在最后一句 -> 房间已 FINISHED 却无审计证据。
	t.Run("无场次时房间审计写失败", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateReady)
		st.stateLogs.failWith("InsertTx", auditErr)

		_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-auditfail", true))
		wantErrIs(t, "房间审计写失败", err, auditErr)
		wantSeq(t, "房间审计写失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", clRoom),
			"live_room_idempotency.Claim:req-close-auditfail",
			fmt.Sprintf("live_session.FindActiveByRoom:%d", clRoom),
			"db.TransactCtx",
			fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
				clRoom, model.RoomStateReady, model.RoomStateFinished, 0),
			fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
				model.LogTypeRoomState, model.RoomStateReady, model.RoomStateFinished, clReason),
		)
		wantEQ(t, "房间审计写失败残留", "房间 state", st.roomAt(t, clRoom).State, model.RoomStateFinished)
		wantEQ(t, "房间审计写失败残留", "审计行数", len(st.logsOf(clRoom)), 0)
		wantMethodCount(t, "房间审计写失败", st.log, "live_room_idempotency.SaveResult", 0)
		wantKeyBurnedNoResult(t, "房间审计写失败", "req-close-auditfail", st)
	})

	// 有场次：t3 审计写在房间迁移之前 -> 失败时房间根本没被动过，只有场次被终止了。
	// 与上一条成对，判别出「场次审计先于房间迁移」这条固定顺序。
	t.Run("有场次时场次审计写失败", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseLivingScene(t, st)
		st.stateLogs.failWith("InsertTx", auditErr)

		_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-auditfail2", true))
		wantErrIs(t, "场次审计写失败", err, auditErr)
		wantSeq(t, "场次审计写失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", clRoom),
			"live_room_idempotency.Claim:req-close-auditfail2",
			fmt.Sprintf("live_session.FindActiveByRoom:%d", clRoom),
			"db.TransactCtx",
			fmt.Sprintf("live_session.TransitionTx:%d:%d->%d/r%d",
				clSess, model.SessionStateLiving, model.SessionStateTerminated, model.EndReasonRoomClosed),
			fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
				model.LogTypeSessionState, model.SessionStateLiving, model.SessionStateTerminated, clReason),
		)
		wantMethodCount(t, "场次审计写失败", st.log, "live_room.TransitionTx", 0)
		wantEQ(t, "场次审计写失败残留", "房间 state", st.roomAt(t, clRoom).State, model.RoomStateLiving)
		wantEQ(t, "场次审计写失败残留", "房间挂机位未清", st.roomAt(t, clRoom).ActiveSessionID, clSess)
		wantEQ(t, "场次审计写失败残留", "场次 state", st.sessionAt(t, clSess).State,
			model.SessionStateTerminated)
		wantDeepEQ(t, "场次审计写失败残留", "counts", st.counts(),
			storeCounts{rooms: 1, anchors: 1, sessions: 1, logs: 0, idem: 1})
		wantKeyBurnedNoResult(t, "场次审计写失败", "req-close-auditfail2", st)
	})
}

// TestCloseRoomReadFailuresPropagateVerbatim 三条读路径的错误必须原样抛出，
// 其中 FindActiveByRoom 失败发生在抢键之后（键已烧），前两条在抢键之前（键完好）。
func TestCloseRoomReadFailuresPropagateVerbatim(t *testing.T) {
	dbFail := errors.New("dial tcp 127.0.0.1:3306: connect: refused")

	t.Run("房间读失败", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateLiving)
		st.rooms.failWith("FindOne", dbFail)
		_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-r1", false))
		wantErrIs(t, "房间读失败", err, dbFail)
		wantSeq(t, "房间读失败", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", clRoom))
		wantKeyUnburned(t, "房间读失败", "req-close-r1", st)
	})

	t.Run("房主读失败", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateLiving)
		st.anchors.failWith("FindOwner", dbFail)
		_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-r2", false))
		wantErrIs(t, "房主读失败", err, dbFail)
		wantSeq(t, "房主读失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", clRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", clRoom),
		)
		wantKeyUnburned(t, "房主读失败", "req-close-r2", st)
	})

	t.Run("生效场次读失败在抢键之后", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseLivingScene(t, st)
		st.sessions.failWith("FindActiveByRoom", dbFail)
		_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-r3", true))
		wantErrIs(t, "场次读失败", err, dbFail)
		wantSeq(t, "场次读失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", clRoom),
			"live_room_idempotency.Claim:req-close-r3",
			fmt.Sprintf("live_session.FindActiveByRoom:%d", clRoom),
		)
		wantKeyBurnedNoResult(t, "场次读失败", "req-close-r3", st)
		wantTxCount(t, "场次读失败", st.conn, 0)
	})

	t.Run("抢键失败", func(t *testing.T) {
		fixClock(t, clNow)
		st := newStore()
		seedCloseScene(t, st, model.RoomStateLiving)
		st.idem.failWith("Claim", dbFail)
		_, err := newCloseLogic(t, st).CloseRoom(clReq("req-close-r4", true))
		wantErrIs(t, "抢键失败", err, dbFail)
		wantSeq(t, "抢键失败", st.log, 1, "live_room_idempotency.Claim:req-close-r4")
		wantTxCount(t, "抢键失败", st.conn, 0)
	})
}

// TestCloseRoomTraceIDTrimmedToColumnWidth trace_id 超长不进应答、只进审计，
// 并且是被裁到列宽而不是让整个写失败（helpers.go:180-186）。
func TestCloseRoomTraceIDTrimmedToColumnWidth(t *testing.T) {
	fixClock(t, clNow)
	st := newStore()
	seedCloseScene(t, st, model.RoomStateReady)
	in := clReq("req-close-trace", true)
	in.TraceId = strings.Repeat("t", maxTraceIDBytes+50)

	_, err := newCloseLogic(t, st).CloseRoom(in)
	wantNoErr(t, "超长 trace_id", err)
	got := st.logsOf(clRoom)[0].TraceID
	wantEQ(t, "超长 trace_id", "长度", len(got), maxTraceIDBytes)
	wantEQ(t, "超长 trace_id", "前缀保住", got[:8], "tttttttt")
	rec := st.idemAt("req-close-trace")
	wantEQ(t, "超长 trace_id", "去重表也裁过", len(rec.TraceID), maxTraceIDBytes)
}
