package logic

// reportstream_logic_test.go 覆盖写侧方法 ReportStreamState（reportstreamstatelogic.go）。
//
// 锁的结论（全部由 reportstreamstatelogic.go + helpers.go:streamEventPlan 反推）：
//  1. **抢 event_id 键在读房间之前**（:67 claimDedup → :76 FindOne）：与其余写方法
//     （先守卫后抢键）相反。后果是任何一次读失败/清理抖动都会把这个 event_id 永久消费掉，
//     消费者必须换新 event_id 才能重试 —— 见 TestReportStreamStateEventKeyIsBurnedBeforeReads。
//  2. 本方法**从不调用 saveDedupResult**（全文件无该调用）：kind=Event 的键永远停在
//     「已登记、result_json 为空」的形态，因此 duplicate() 里读 result_json 的分支（:257）
//     在生产路径上不可达，只有别的写入口回填过同名键才可能进入 —— 哨兵用例已钉住。
//  3. 「事件被丢弃」不用 gRPC error 表达：非法迁移/乱序/不匹配一律 result=2..5 + nil error，
//     只有入参本身非法与依赖失败才返回 error（:39-40 的设计说明）。
//  4. 事务内顺序固定：场次（AdvanceStreamSeqTx 或 BumpStreamSeqTx）→ 房间迁移 + t1 审计 →
//     t3 审计。房间迁移的合法性在开事务**之前**判完（:121），所以非法边不会留下半个写入。
//     但房间 CAS 未命中（:162）会留场次写：假件不回滚，残留形态照实断言。
//  5. seq 守卫未命中后必须回查（:186）区分「seq 陈旧」(result=3) 与「状态被并发推进」(result=4)，
//     两者给不同 result 决定消费者重投还是丢弃。
//  6. stream_id 只在场次原值为空时补登（:198），重连不漂流；补登失败只记日志不报错。
//  7. 应答里的状态一律回读自当前投影（:205-213 两次 FindOne），不回填「期望值」。

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
	rsNow         int64 = 1_700_000_400
	rsStart       int64 = 1_700_000_000
	rsRoom        int64 = 3201
	rsOwner       int64 = 9201
	rsSess        int64 = 5101
	rsStream            = "stream-rs"
	rsOtherStream       = "stream-other"
	rsOtherRoom   int64 = 3299
)

// rsReq 组一条除 event_id 外都合法的事件；streamState/seq 由用例给。
func rsReq(eventID string, streamState int32, seq int64) *rpc.ReportStreamStateReq {
	return &rpc.ReportStreamStateReq{
		EventId: eventID, RoomId: rsRoom, SessionId: rsSess, StreamId: rsStream,
		StreamState: streamState, StreamSeq: seq, OccurredAt: rsNow - 30, TraceId: "trace-rs",
	}
}

func newReportLogic(t *testing.T, st *store) *ReportStreamStateLogic {
	t.Helper()
	return NewReportStreamStateLogic(context.Background(), st.svcCtx())
}

// newReportLogicGrace 装配带「断流宽限期」的上下文：默认配置里该值是 0，
// 而 grace>0 才可能因断流终止场次，所以宽限期相关用例必须显式改配置。
func newReportLogicGrace(t *testing.T, st *store, grace int32) *ReportStreamStateLogic {
	t.Helper()
	conf := testLiveRoomConfDo(func(c *config.LiveRoomConf) { c.StreamInterruptGraceSeconds = grace })
	return NewReportStreamStateLogic(context.Background(), st.svcCtxWith(conf))
}

// seedReportScene 布「某状态的房间 + 生效房主 + 某状态的场次」。
// 只有房间处于 LIVING 时才登记挂机位（生产里那是 StartLive/PUBLISHING 迁移的 patch 写的）。
// sessionStreamID 传空串表示场次还没登记流引用（补登分支）。
func seedReportScene(t *testing.T, st *store, roomState, sessionState int32, sessionStreamID string) {
	t.Helper()
	r := baseRoom(rsRoom, rsOwner, roomState)
	if roomState == model.RoomStateLiving {
		r.ActiveSessionID = rsSess
		r.ActiveStreamID = rsStream
	}
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(901, rsRoom, rsOwner, model.AnchorRoleOwner))
	s := baseSession(rsSess, rsRoom, rsOwner, sessionState)
	s.StartedAt = rsStart
	s.StreamID = sessionStreamID
	st.seedSession(s)
}

// seedReportLiving 是「房间在播 + 场次直播中 + seq 已推进到 3」的常用前提。
func seedReportLiving(t *testing.T, st *store) {
	t.Helper()
	seedReportScene(t, st, model.RoomStateLiving, model.SessionStateLiving, rsStream)
	st.sessions.rows[0].LastStreamSeq = 3
}

// rsTrace 前缀各用例都写全（from=0），因为事件入口的轨迹里「抢键在最前」这条
// 只能靠完整序列才看得见。

// TestReportStreamStatePublishingAdvancesSessionAndRoom 是本方法的骨架用例：
// PUBLISHING 落在未开播的场次上 -> 场次进 LIVING、房间进 LIVING、两条审计、seq 推进。
func TestReportStreamStatePublishingAdvancesSessionAndRoom(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	seedReportScene(t, st, model.RoomStateReady, model.SessionStatePending, rsStream)

	reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-1", model.StreamStatePublishing, 7))
	wantNoErr(t, "推流到达", err)

	wantSeq(t, "推流到达", st.log, 0,
		"live_room_idempotency.Claim:evt-rs-1",
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
		"db.TransactCtx",
		fmt.Sprintf("live_session.AdvanceStreamSeqTx:%d:%d->%d/seq%d",
			rsSess, model.SessionStatePending, model.SessionStateLiving, 7),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			rsRoom, model.RoomStateReady, model.RoomStateLiving, 1),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateReady, model.RoomStateLiving, "推流到达，场次进入直播中"),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeSessionState, model.SessionStatePending, model.SessionStateLiving, "推流到达，场次进入直播中"),
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
	)
	wantTxCount(t, "推流到达", st.conn, 1)
	wantEQ(t, "推流到达", "result", reply.GetResult(), model.StreamResultApplied)
	wantEQ(t, "推流到达", "message", reply.GetMessage(), "推流到达，场次进入直播中")
	wantEQ(t, "推流到达", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_LIVING)
	wantEQ(t, "推流到达", "session_state", reply.GetSessionState(), rpc.SessionState_SESSION_STATE_LIVING)
	wantEQ(t, "推流到达", "session_id", reply.GetSessionId(), rsSess)

	room := st.roomAt(t, rsRoom)
	wantEQ(t, "推流到达落库", "state", room.State, model.RoomStateLiving)
	// expectVersion 走的是读到的 state_version（对比 CloseRoom 恒传 0）：房间参与乐观锁。
	wantEQ(t, "推流到达落库", "state_version", room.StateVersion, int32(2))
	wantEQ(t, "推流到达落库", "active_session_id", room.ActiveSessionID, rsSess)
	wantEQ(t, "推流到达落库", "active_stream_id", room.ActiveStreamID, rsStream)

	sess := st.sessionAt(t, rsSess)
	wantEQ(t, "推流到达落库", "state", sess.State, model.SessionStateLiving)
	wantEQ(t, "推流到达落库", "last_stream_seq", sess.LastStreamSeq, int64(7))
	// PUBLISHING 不是终态：ended_at / end_reason 一个都不许写。
	wantEQ(t, "推流到达落库", "ended_at", sess.EndedAt, int64(0))
	wantEQ(t, "推流到达落库", "end_reason", sess.EndReason, model.EndReasonUnspecified)

	logs := st.logsOf(rsRoom)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2", len(logs))
	}
	for i, l := range logs {
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "source", l.Source, model.SourceStreamEvent)
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "event_id", l.EventID, "evt-rs-1")
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "session_id", l.SessionID, rsSess)
		// 事件驱动路径没有操作者：operator_mid 留 0，归因靠 source+event_id。
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "operator_mid", l.OperatorMid, int64(0))
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "trace_id", l.TraceID, "trace-rs")
	}
	wantDeepEQ(t, "推流到达落库面", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, sessions: 1, logs: 2, idem: 1})
}

// TestReportStreamStateBackfillsMissingStreamID 钉住第 6 条的一半：
// 场次没登记流引用时，事件里的 stream_id 被补登（第 8 次调用才有它）。
func TestReportStreamStateBackfillsMissingStreamID(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	seedReportScene(t, st, model.RoomStateLiving, model.SessionStatePending, "")
	// 房间投影里的 active_stream_id 与场次不一致：补登只认场次，不改房间。
	st.rooms.rows[0].ActiveStreamID = rsOtherStream

	reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-fill", model.StreamStatePublishing, 4))
	wantNoErr(t, "补登流引用", err)

	wantSeq(t, "补登流引用", st.log, 0,
		"live_room_idempotency.Claim:evt-rs-fill",
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
		"db.TransactCtx",
		fmt.Sprintf("live_session.AdvanceStreamSeqTx:%d:%d->%d/seq%d",
			rsSess, model.SessionStatePending, model.SessionStateLiving, 4),
		// needRoomMove=false（房间已在 LIVING）：一句房间写都不该有。
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeSessionState, model.SessionStatePending, model.SessionStateLiving, "推流到达，场次进入直播中"),
		fmt.Sprintf("live_session.SetStreamID:%d:%s", rsSess, rsStream),
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
	)
	wantMethodCount(t, "补登流引用", st.log, "live_room.TransitionTx", 0)
	wantEQ(t, "补登流引用", "场次 stream_id", st.sessionAt(t, rsSess).StreamID, rsStream)
	wantEQ(t, "补登流引用", "场次 mtime", st.sessionAt(t, rsSess).Mtime, rsNow)
	wantEQ(t, "补登流引用", "房间 stream 不被改", st.roomAt(t, rsRoom).ActiveStreamID, rsOtherStream)
	wantEQ(t, "补登流引用应答", "session_state", reply.GetSessionState(), rpc.SessionState_SESSION_STATE_LIVING)
}

// TestReportStreamStateSetStreamIDFailureIsOnlyLogged 补登失败（驱动报错）不得把
// 已成功的状态迁移变成错误应答——事件已生效，消费者不该因它重投。
func TestReportStreamStateSetStreamIDFailureIsOnlyLogged(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	seedReportScene(t, st, model.RoomStateLiving, model.SessionStatePending, "")
	st.sessions.failWith("SetStreamID", errors.New("deadlock"))

	reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-fillfail", model.StreamStatePublishing, 4))
	wantNoErr(t, "补登失败仍成功", err)
	wantEQ(t, "补登失败仍成功", "result", reply.GetResult(), model.StreamResultApplied)
	wantEQ(t, "补登失败仍成功", "场次 state 已迁移", st.sessionAt(t, rsSess).State, model.SessionStateLiving)
	wantEQ(t, "补登失败仍成功", "场次 stream_id 未写入", st.sessionAt(t, rsSess).StreamID, "")
	wantSeq(t, "补登失败仍成功", st.log, 0,
		"live_room_idempotency.Claim:evt-rs-fillfail",
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
		"db.TransactCtx",
		fmt.Sprintf("live_session.AdvanceStreamSeqTx:%d:%d->%d/seq%d",
			rsSess, model.SessionStatePending, model.SessionStateLiving, 4),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeSessionState, model.SessionStatePending, model.SessionStateLiving, "推流到达，场次进入直播中"),
		fmt.Sprintf("live_session.SetStreamID:%d:%s", rsSess, rsStream),
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
	)
}

// TestReportStreamStateStaleSeqIsRejectedWithoutWrites 钉住第 5 条的前半：
// seq 不高于已应用序号 -> 事务里的 CAS 未命中 -> 回查后给 result=3，且没有任何写入。
func TestReportStreamStateStaleSeqIsRejectedWithoutWrites(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	// 房间必须处于 READY：PENDING 房间没有通往 LIVING 的边，会先被第 4 条短路掉。
	seedReportScene(t, st, model.RoomStateReady, model.SessionStatePending, rsStream)
	st.sessions.rows[0].LastStreamSeq = 9

	reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-stale", model.StreamStatePublishing, 5))
	wantNoErr(t, "陈旧 seq", err)
	wantSeq(t, "陈旧 seq", st.log, 0,
		"live_room_idempotency.Claim:evt-rs-stale",
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
		"db.TransactCtx",
		fmt.Sprintf("live_session.AdvanceStreamSeqTx:%d:%d->%d/seq%d",
			rsSess, model.SessionStatePending, model.SessionStateLiving, 5),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
	)
	wantEQ(t, "陈旧 seq", "result", reply.GetResult(), model.StreamResultStale)
	wantEQ(t, "陈旧 seq", "message", reply.GetMessage(), "事件序号不高于已应用序号，按乱序丢弃")
	wantEQ(t, "陈旧 seq", "session_id", reply.GetSessionId(), rsSess)
	// seqRejected 分支在事务里 return error -> 房间那句根本没执行。
	wantMethodCount(t, "陈旧 seq", st.log, "live_room.TransitionTx", 0)
	wantMethodCount(t, "陈旧 seq", st.log, "live_room_state_log.InsertTx", 0)
	wantEQ(t, "陈旧 seq 残留", "房间 state", st.roomAt(t, rsRoom).State, model.RoomStateReady)
	wantEQ(t, "陈旧 seq 残留", "seq 未被推高", st.sessionAt(t, rsSess).LastStreamSeq, int64(9))
	wantEQ(t, "陈旧 seq 残留", "场次 state", st.sessionAt(t, rsSess).State, model.SessionStatePending)
	wantTxCount(t, "陈旧 seq", st.conn, 1)
	wantDeepEQ(t, "陈旧 seq 落库面", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, sessions: 1, logs: 0, idem: 1})
}

// TestReportStreamStateConcurrentStateAdvanceIsReportedIllegal 是上一条的判别对照：
// seq 更新（seq 12 > 已应用 3）但场次状态被并发推走 -> 同一个 CAS 未命中，
// 回查后必须给 result=4 而不是 3，消费者据此丢弃而不是重投。
func TestReportStreamStateConcurrentStateAdvanceIsReportedIllegal(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	defer st.checkRaces(t)
	seedReportScene(t, st, model.RoomStateReady, model.SessionStatePending, rsStream)
	st.raceBefore("live_session.AdvanceStreamSeqTx", func() {
		st.sessions.rows[0].State = model.SessionStateLiving
	})

	reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-race", model.StreamStatePublishing, 12))
	wantNoErr(t, "并发推进", err)
	wantSeq(t, "并发推进", st.log, 0,
		"live_room_idempotency.Claim:evt-rs-race",
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
		"db.TransactCtx",
		fmt.Sprintf("live_session.AdvanceStreamSeqTx:%d:%d->%d/seq%d",
			rsSess, model.SessionStatePending, model.SessionStateLiving, 12),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
	)
	wantEQ(t, "并发推进", "result", reply.GetResult(), model.StreamResultIllegalTransition)
	wantEQ(t, "并发推进", "message", reply.GetMessage(), "场次状态已被并发推进，本次迁移未生效")
	// 回查读的是当前投影：应答里的状态是并发者写的 LIVING，不是本事件期望值。
	wantEQ(t, "并发推进", "session_state", reply.GetSessionState(), rpc.SessionState_SESSION_STATE_LIVING)
	wantEQ(t, "并发推进残留", "房间仍 READY", st.roomAt(t, rsRoom).State, model.RoomStateReady)
	wantEQ(t, "并发推进残留", "seq 未被推高", st.sessionAt(t, rsSess).LastStreamSeq, int64(0))
	wantKeyBurnedNoResult(t, "并发推进", "evt-rs-race", st)
}

// TestReportStreamStateDuplicateEventIsReadOnly 钉住第 1、2 条：event_id 重投只回读投影。
// 三个子例分别对应 result_json 的两种形态与「键被别的 RPC 用过」。
func TestReportStreamStateDuplicateEventIsReadOnly(t *testing.T) {
	t.Run("首次未回填结果（生产唯一形态）", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportScene(t, st, model.RoomStateLiving, model.SessionStateLiving, rsStream)
		st.seedIdem("evt-rs-dup", rpcReportStreamState, "")

		reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-dup", model.StreamStatePublishing, 8))
		wantNoErr(t, "重投", err)
		wantSeq(t, "重投", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-dup",
			"live_room_idempotency.Find:evt-rs-dup",
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindOne:%d", rsSess),
		)
		wantEQ(t, "重投", "result", reply.GetResult(), model.StreamResultDuplicate)
		wantEQ(t, "重投", "message", reply.GetMessage(), "同一 event_id 重复投递，未产生新写入")
		wantEQ(t, "重投", "room_state 回读当前投影", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_LIVING)
		wantTxCount(t, "重投", st.conn, 0)
		wantMethodCount(t, "重投", st.log, "db.TransactCtx", 0)
		wantMethodCount(t, "重投", st.log, "live_room_state_log.InsertTx", 0)
		wantEQ(t, "重投", "seq 未被推高", st.sessionAt(t, rsSess).LastStreamSeq, int64(0))
	})

	t.Run("键上已有结果快照则沿用其 message", func(t *testing.T) {
		// 生产里 ReportStreamState 自己不回填，故这个形态只能由外部（或未来的修复）产生；
		// 布上它是为了证明 :257 的分支不是永真代码——没有这条，删掉整个 if 也不会有用例变红。
		fixClock(t, rsNow)
		st := newStore()
		seedReportScene(t, st, model.RoomStateLiving, model.SessionStateLiving, rsStream)
		st.seedIdem("evt-rs-dupjson", rpcReportStreamState,
			`{"result":1,"message":"推流心跳，无状态迁移"}`)

		reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-dupjson", model.StreamStatePublishing, 8))
		wantNoErr(t, "重投带快照", err)
		wantEQ(t, "重投带快照", "result", reply.GetResult(), model.StreamResultDuplicate)
		wantEQ(t, "重投带快照", "message", reply.GetMessage(), "推流心跳，无状态迁移（重复投递）")
	})

	t.Run("无 session_id 时按房间投影回读场次", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportScene(t, st, model.RoomStateLiving, model.SessionStateLiving, rsStream)
		st.seedIdem("evt-rs-dupact", rpcReportStreamState, "")
		in := rsReq("evt-rs-dupact", model.StreamStatePublishing, 8)
		in.SessionId = 0

		reply, err := newReportLogic(t, st).ReportStreamState(in)
		wantNoErr(t, "重投按投影回读", err)
		wantSeq(t, "重投按投影回读", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-dupact",
			"live_room_idempotency.Find:evt-rs-dupact",
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindOne:%d", rsSess),
		)
		wantMethodCount(t, "重投按投影回读", st.log, "live_session.FindActiveByRoom", 0)
		wantEQ(t, "重投按投影回读", "session_id", reply.GetSessionId(), rsSess)
	})

	t.Run("键被别的 RPC 用过时报错不回放", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportScene(t, st, model.RoomStateLiving, model.SessionStateLiving, rsStream)
		st.seedIdem("evt-rs-other", rpcBanRoom, `{"ban_id":1}`)

		_, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-other", model.StreamStatePublishing, 8))
		wantErrIs(t, "串方法的键", err, model.ErrRequestIDReused)
		wantErrContains(t, "串方法的键", err, rpcBanRoom)
		wantSeq(t, "串方法的键", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-other",
			"live_room_idempotency.Find:evt-rs-other",
		)
		wantTxCount(t, "串方法的键", st.conn, 0)
	})
}

// TestReportStreamStateStoppedReleasesRoom 覆盖 STOPPED：场次 TERMINATED（原因=流断）、
// 房间退回 READY 并清挂机位，ended_at 用的是事件发生时刻而不是 now。
func TestReportStreamStateStoppedReleasesRoom(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	seedReportLiving(t, st)

	reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-stop", model.StreamStateStopped, 4))
	wantNoErr(t, "推流停止", err)
	wantSeq(t, "推流停止", st.log, 0,
		"live_room_idempotency.Claim:evt-rs-stop",
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
		"db.TransactCtx",
		fmt.Sprintf("live_session.AdvanceStreamSeqTx:%d:%d->%d/seq%d",
			rsSess, model.SessionStateLiving, model.SessionStateTerminated, 4),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			rsRoom, model.RoomStateLiving, model.RoomStateReady, 1),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateLiving, model.RoomStateReady, "推流停止，场次终止并释放房间直播态"),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeSessionState, model.SessionStateLiving, model.SessionStateTerminated, "推流停止，场次终止并释放房间直播态"),
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
	)
	wantEQ(t, "推流停止", "result", reply.GetResult(), model.StreamResultApplied)
	wantEQ(t, "推流停止", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_READY)

	room := st.roomAt(t, rsRoom)
	wantEQ(t, "推流停止落库", "state", room.State, model.RoomStateReady)
	wantEQ(t, "推流停止落库", "active_session_id 被清", room.ActiveSessionID, int64(0))
	wantEQ(t, "推流停止落库", "active_stream_id 被清", room.ActiveStreamID, "")

	sess := st.sessionAt(t, rsSess)
	wantEQ(t, "推流停止落库", "state", sess.State, model.SessionStateTerminated)
	wantEQ(t, "推流停止落库", "end_reason", sess.EndReason, model.EndReasonStreamReplay)
	// ended_at 取 occurred_at（事件发生时刻），不是处理时刻 rsNow：乱序补投才不会算出假时长。
	wantEQ(t, "推流停止落库", "ended_at", sess.EndedAt, rsNow-30)
	wantEQ(t, "推流停止落库", "duration", sess.DurationSeconds, rsNow-30-rsStart)
	wantEQ(t, "推流停止落库", "seq", sess.LastStreamSeq, int64(4))
}

// TestReportStreamStateHeartbeatOnlyBumpsSeq 覆盖 LIVING 场次上的 PUBLISHING：
// 心跳只推 seq，state 与房间一律不动，且**不写审计**（bumpOnly 跳过两条 InsertTx）。
func TestReportStreamStateHeartbeatOnlyBumpsSeq(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	seedReportLiving(t, st)

	reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-hb", model.StreamStatePublishing, 40))
	wantNoErr(t, "推流心跳", err)
	wantSeq(t, "推流心跳", st.log, 0,
		"live_room_idempotency.Claim:evt-rs-hb",
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
		"db.TransactCtx",
		fmt.Sprintf("live_session.BumpStreamSeqTx:%d/seq%d", rsSess, 40),
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
	)
	wantEQ(t, "推流心跳", "result", reply.GetResult(), model.StreamResultApplied)
	wantEQ(t, "推流心跳", "message", reply.GetMessage(), "推流心跳，无状态迁移")
	wantEQ(t, "推流心跳落库", "seq", st.sessionAt(t, rsSess).LastStreamSeq, int64(40))
	wantEQ(t, "推流心跳落库", "场次 state", st.sessionAt(t, rsSess).State, model.SessionStateLiving)
	wantEQ(t, "推流心跳落库", "房间 state", st.roomAt(t, rsRoom).State, model.RoomStateLiving)
	wantEQ(t, "推流心跳落库", "state_version 不动", st.roomAt(t, rsRoom).StateVersion, int32(1))
	wantEQ(t, "推流心跳落库", "mtime 被 bump 改写", st.sessionAt(t, rsSess).Mtime, rsNow)
	wantEQ(t, "推流心跳落库", "审计行数", len(st.logsOf(rsRoom)), 0)
}

// TestReportStreamStateInterruptedGraceBoundary 是三条一对的判别用例：
// 断流是否终止场次**只取决于 configured 宽限期**，与 seq、与已经断了多久（除比较外）无关。
func TestReportStreamStateInterruptedGraceBoundary(t *testing.T) {
	t.Run("宽限期内仅记录观测", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportLiving(t, st)

		reply, err := newReportLogicGrace(t, st, 60).ReportStreamState(
			rsReq("evt-rs-grace-in", model.StreamStateInterrupted, 11))
		wantNoErr(t, "宽限期内", err)
		wantEQ(t, "宽限期内", "result", reply.GetResult(), model.StreamResultApplied)
		wantEQ(t, "宽限期内", "message", reply.GetMessage(), "断流宽限期内，仅记录观测")
		wantEQ(t, "宽限期内", "场次仍 LIVING", st.sessionAt(t, rsSess).State, model.SessionStateLiving)
		wantEQ(t, "宽限期内", "房间仍 LIVING", st.roomAt(t, rsRoom).State, model.RoomStateLiving)
		wantEQ(t, "宽限期内", "seq 已推进", st.sessionAt(t, rsSess).LastStreamSeq, int64(11))
		wantSeq(t, "宽限期内", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-grace-in",
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindOne:%d", rsSess),
			"db.TransactCtx",
			fmt.Sprintf("live_session.BumpStreamSeqTx:%d/seq%d", rsSess, 11),
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindOne:%d", rsSess),
		)
		wantEQ(t, "宽限期内", "审计行数", len(st.logsOf(rsRoom)), 0)
	})

	t.Run("达到宽限期即终止", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportLiving(t, st)

		req := rsReq("evt-rs-grace-out", model.StreamStateInterrupted, 11)
		req.InterruptedSeconds = 60
		reply, err := newReportLogicGrace(t, st, 60).ReportStreamState(req)
		wantNoErr(t, "超过宽限期", err)
		wantEQ(t, "超过宽限期", "result", reply.GetResult(), model.StreamResultApplied)
		wantEQ(t, "超过宽限期", "message", reply.GetMessage(), "断流超过宽限期，场次终止")
		wantSeq(t, "超过宽限期", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-grace-out",
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindOne:%d", rsSess),
			"db.TransactCtx",
			fmt.Sprintf("live_session.AdvanceStreamSeqTx:%d:%d->%d/seq%d",
				rsSess, model.SessionStateLiving, model.SessionStateTerminated, 11),
			fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
				rsRoom, model.RoomStateLiving, model.RoomStateReady, 1),
			fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
				model.LogTypeRoomState, model.RoomStateLiving, model.RoomStateReady, "断流超过宽限期，场次终止"),
			fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
				model.LogTypeSessionState, model.SessionStateLiving, model.SessionStateTerminated, "断流超过宽限期，场次终止"),
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindOne:%d", rsSess),
		)
		sess := st.sessionAt(t, rsSess)
		wantEQ(t, "超过宽限期", "场次 state", sess.State, model.SessionStateTerminated)
		// 落点原因与 STOPPED 不同：超时是 StreamTimeout，正常停流是 StreamReplay。
		wantEQ(t, "超过宽限期", "end_reason", sess.EndReason, model.EndReasonStreamTimeout)
		wantEQ(t, "超过宽限期", "房间退回 READY", st.roomAt(t, rsRoom).State, model.RoomStateReady)
		wantEQ(t, "超过宽限期", "挂机位已清", st.roomAt(t, rsRoom).ActiveSessionID, int64(0))
	})

	t.Run("宽限期未配置时永不终止", func(t *testing.T) {
		// graceSeconds<=0 时 streamEventPlan 直接短路（helpers.go:567），
		// 所以 yaml 里漏配这一项 = 断流再多也只记观测。默认 testLiveRoomConf 正是 0。
		fixClock(t, rsNow)
		st := newStore()
		seedReportLiving(t, st)
		req := rsReq("evt-rs-grace-zero", model.StreamStateInterrupted, 11)
		req.InterruptedSeconds = 999999

		reply, err := newReportLogic(t, st).ReportStreamState(req)
		wantNoErr(t, "未配宽限期", err)
		wantEQ(t, "未配宽限期", "message", reply.GetMessage(), "断流宽限期内，仅记录观测")
		wantEQ(t, "未配宽限期", "场次仍 LIVING", st.sessionAt(t, rsSess).State, model.SessionStateLiving)
		wantMethodCount(t, "未配宽限期", st.log, "live_room.TransitionTx", 0)
	})
}

// TestReportStreamStateNoChangeResultsWriteNothing 覆盖两个「计划判定为不改」的入口：
// IDLE（契约里既不是开播也不是结束）与已终态场次收到 PUBLISHING。
// 两者都给 result=4（非法迁移）而不是 1，且抢键之后一次写入都没有。
func TestReportStreamStateNoChangeResultsWriteNothing(t *testing.T) {
	t.Run("IDLE 不产生迁移", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportLiving(t, st)

		reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-idle", model.StreamStateIdle, 99))
		wantNoErr(t, "IDLE", err)
		wantSeq(t, "IDLE", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-idle",
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindOne:%d", rsSess),
		)
		wantEQ(t, "IDLE", "result", reply.GetResult(), model.StreamResultIllegalTransition)
		wantEQ(t, "IDLE", "message", reply.GetMessage(), "IDLE 不产生状态迁移")
		wantEQ(t, "IDLE", "seq 未推进", st.sessionAt(t, rsSess).LastStreamSeq, int64(3))
		wantTxCount(t, "IDLE", st.conn, 0)
	})

	t.Run("已终态场次不被复活", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportScene(t, st, model.RoomStateReady, model.SessionStateEnded, rsStream)

		reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-terminal", model.StreamStatePublishing, 99))
		wantNoErr(t, "终态场次", err)
		wantEQ(t, "终态场次", "result", reply.GetResult(), model.StreamResultIllegalTransition)
		wantEQ(t, "终态场次", "message", reply.GetMessage(), "场次已终态，推流事件不再改变状态")
		wantEQ(t, "终态场次", "场次 state", st.sessionAt(t, rsSess).State, model.SessionStateEnded)
		wantEQ(t, "终态场次", "房间 state", st.roomAt(t, rsRoom).State, model.RoomStateReady)
		wantSeq(t, "终态场次", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-terminal",
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindOne:%d", rsSess),
		)
	})
}

// TestReportStreamStateMismatchCases 钉住三种「不猜、不自愈」：全部 result=5、零写入，
// 且都在抢键之后（键已消费）——这是事件入口与请求入口语义不同的地方。
func TestReportStreamStateMismatchCases(t *testing.T) {
	t.Run("场次不属于该房间", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportScene(t, st, model.RoomStateLiving, model.SessionStateLiving, rsStream)
		// 把 5101 挪到另一个房间：房间投影里仍是它，判据是 session.RoomID != room.RoomID。
		st.sessions.rows[0].RoomID = rsOtherRoom

		// 事件点名 session_id=5101，但那一行已不属于 3201：不改任何状态。
		reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-cross", model.StreamStateStopped, 9))
		wantNoErr(t, "跨房间场次", err)
		wantEQ(t, "跨房间场次", "result", reply.GetResult(), model.StreamResultMismatch)
		wantEQ(t, "跨房间场次", "message", reply.GetMessage(), "场次与房间不匹配或不存在")
		wantTxCount(t, "跨房间场次", st.conn, 0)
		wantEQ(t, "跨房间场次", "本房间场次未被动", st.sessionAt(t, rsSess).State, model.SessionStateLiving)
	})

	t.Run("session_id 指向不存在的场次", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportLiving(t, st)
		in := rsReq("evt-rs-nosess", model.StreamStateStopped, 9)
		in.SessionId = 8888

		reply, err := newReportLogic(t, st).ReportStreamState(in)
		wantNoErr(t, "场次不存在", err)
		wantEQ(t, "场次不存在", "result", reply.GetResult(), model.StreamResultMismatch)
		wantSeq(t, "场次不存在", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-nosess",
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			"live_session.FindOne:8888",
		)
		// 应答里的 session_id 从房间投影兜底（reply() 的最后一段）。
		wantEQ(t, "场次不存在", "session_id", reply.GetSessionId(), rsSess)
		wantEQ(t, "场次不存在", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_LIVING)
	})

	t.Run("无 session_id 且房间没有进行中场次", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		st.seedRoom(baseRoom(rsRoom, rsOwner, model.RoomStateReady))
		in := rsReq("evt-rs-noactive", model.StreamStatePublishing, 9)
		in.SessionId = 0

		reply, err := newReportLogic(t, st).ReportStreamState(in)
		wantNoErr(t, "无进行中场次", err)
		wantSeq(t, "无进行中场次", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-noactive",
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindActiveByRoom:%d", rsRoom),
		)
		wantEQ(t, "无进行中场次", "result", reply.GetResult(), model.StreamResultMismatch)
		wantEQ(t, "无进行中场次", "message", reply.GetMessage(), "房间没有进行中场次")
		wantEQ(t, "无进行中场次", "session_id 为 0", reply.GetSessionId(), int64(0))
	})

	t.Run("stream_id 与场次登记值冲突", func(t *testing.T) {
		fixClock(t, rsNow)
		st := newStore()
		seedReportLiving(t, st)
		in := rsReq("evt-rs-stream", model.StreamStatePublishing, 9)
		in.StreamId = rsOtherStream

		reply, err := newReportLogic(t, st).ReportStreamState(in)
		wantNoErr(t, "流引用冲突", err)
		wantSeq(t, "流引用冲突", st.log, 0,
			"live_room_idempotency.Claim:evt-rs-stream",
			fmt.Sprintf("live_room.FindOne:%d", rsRoom),
			fmt.Sprintf("live_session.FindOne:%d", rsSess),
		)
		wantEQ(t, "流引用冲突", "result", reply.GetResult(), model.StreamResultMismatch)
		wantEQ(t, "流引用冲突", "message", reply.GetMessage(), "stream_id 与场次登记值不一致")
		wantEQ(t, "流引用冲突", "seq 未推进", st.sessionAt(t, rsSess).LastStreamSeq, int64(3))
	})
}

// TestReportStreamStateIllegalRoomEdgeShortCircuitsBeforeTx 钉住第 4 条的前半句：
// 房间侧迁移的合法性在开事务之前判完，所以「场次可迁、房间无边」这种组合一句写都没有。
func TestReportStreamStateIllegalRoomEdgeShortCircuitsBeforeTx(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	// 房间在 FINISHED（矩阵里出边为空集），场次却还没终态：PUBLISHING 想把房间推到 LIVING。
	seedReportScene(t, st, model.RoomStateFinished, model.SessionStatePending, rsStream)

	reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-roomedge", model.StreamStatePublishing, 5))
	wantNoErr(t, "房间无边", err)
	wantSeq(t, "房间无边", st.log, 0,
		"live_room_idempotency.Claim:evt-rs-roomedge",
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
		fmt.Sprintf("live_session.FindOne:%d", rsSess),
	)
	wantEQ(t, "房间无边", "result", reply.GetResult(), model.StreamResultIllegalTransition)
	wantEQ(t, "房间无边", "message", reply.GetMessage(),
		"房间状态 4 不接受该流事件（推流到达，场次进入直播中）")
	wantTxCount(t, "房间无边", st.conn, 0)
	wantEQ(t, "房间无边", "场次仍是 PENDING", st.sessionAt(t, rsSess).State, model.SessionStatePending)
	wantEQ(t, "房间无边", "房间仍是 FINISHED", st.roomAt(t, rsRoom).State, model.RoomStateFinished)
}

// TestReportStreamStateGuardTableRejectsBeforeClaim 入参守卫表：全部在抢键之前被拒。
// 这一条与下面的哨兵成对——判别出「守卫 vs 抢键」的边界到底在哪一行。
func TestReportStreamStateGuardTableRejectsBeforeClaim(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.ReportStreamStateReq)
		want   error
		frag   string
	}{
		{"缺 event_id", func(in *rpc.ReportStreamStateReq) { in.EventId = "  " }, model.ErrEventIDRequired, ""},
		{"超长 event_id", func(in *rpc.ReportStreamStateReq) { in.EventId = strings.Repeat("e", 65) }, model.ErrDedupIDTooLong, "65"},
		{"房间号非法", func(in *rpc.ReportStreamStateReq) { in.RoomId = 0 }, model.ErrInvalidRoomID, ""},
		{"未知 stream_state", func(in *rpc.ReportStreamStateReq) { in.StreamState = 9 }, model.ErrStreamStateInvalid, "stream_state=9"},
		{"stream_state=0", func(in *rpc.ReportStreamStateReq) { in.StreamState = 0 }, model.ErrStreamStateInvalid, ""},
		{"seq 为 0", func(in *rpc.ReportStreamStateReq) { in.StreamSeq = 0 }, model.ErrStreamSeqStale, "seq=0"},
		{"seq 为负", func(in *rpc.ReportStreamStateReq) { in.StreamSeq = -3 }, model.ErrStreamSeqStale, ""},
		{"occurred_at 为负", func(in *rpc.ReportStreamStateReq) { in.OccurredAt = -1 }, model.ErrStreamStateInvalid, "不得为负"},
		{"中断秒数为负", func(in *rpc.ReportStreamStateReq) { in.InterruptedSeconds = -1 }, model.ErrStreamStateInvalid, ""},
		{"stream_id 超长", func(in *rpc.ReportStreamStateReq) { in.StreamId = strings.Repeat("s", 65) }, model.ErrStreamRefMismatch, "stream_id"},
		{"stream_id 含协议头", func(in *rpc.ReportStreamStateReq) { in.StreamId = "rtmp://x/y" }, model.ErrStreamRefMismatch, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, rsNow)
			st := newStore()
			seedReportLiving(t, st)
			in := rsReq("evt-rs-guard", model.StreamStatePublishing, 4)
			tc.mutate(in)

			_, err := newReportLogic(t, st).ReportStreamState(in)
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name, err, tc.frag)
			}
			wantNoCallAfter(t, tc.name, st.log, 0)
			// 一条 event 键都不许登记：守卫必须整体在 Claim 之前。
			wantEQ(t, tc.name, "幂等表行数", st.counts().idem, 0)
		})
	}
}

// TestReportStreamStateEventKeyIsBurnedBeforeReads 哨兵用例（README 已知缺口）：
// event_id 在读房间之前就被 Claim 消费（:67 vs :76），所以一次瞬时读失败会把该事件
// 永久标记为「已处理」，消费者按同一 event_id 重投只会拿到 result=2 的空重放。
// TODO(缺陷): 期望行为是「读失败时不消费 event_id」，与请求侧方法（守卫先于抢键）保持一致。
// 这里钉的是**当前实现的可观察后果**，不是设计意图。
func TestReportStreamStateEventKeyIsBurnedBeforeReads(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	seedReportLiving(t, st)
	dbFail := errors.New("dial tcp 127.0.0.1:3306: connect refused")
	st.rooms.failWith("FindOne", dbFail)

	_, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-readfail", model.StreamStateStopped, 9))
	wantErrIs(t, "读失败", err, dbFail)
	wantSeq(t, "读失败", st.log, 0,
		"live_room_idempotency.Claim:evt-rs-readfail",
		fmt.Sprintf("live_room.FindOne:%d", rsRoom),
	)
	wantKeyBurnedNoResult(t, "读失败", "evt-rs-readfail", st)

	// 同一个 event_id 再来一次：Claim 说不是首次 -> 走 duplicate() -> 事件被静默丢弃。
	// 撤掉注入，模拟「瞬时故障恢复后消费者按同一 event_id 重投」
	st.rooms.by = nil
	reply, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-readfail", model.StreamStateStopped, 9))
	wantNoErr(t, "同一事件重投", err)
	wantEQ(t, "同一事件重投", "result", reply.GetResult(), model.StreamResultDuplicate)
	wantEQ(t, "同一事件重投", "房间状态其实没变", st.roomAt(t, rsRoom).State, model.RoomStateLiving)
}

// TestReportStreamStateNeverSavesResultJSON 哨兵用例（README 已知缺口）：
// 本方法全程没有 saveDedupResult 调用，所以成功处理完的键也停在 result_json=""，
// duplicate() 里读快照的分支因此不可达。
// TODO(缺陷): 要么补回填（与其他写方法一致），要么删掉不可达分支与其注释。
func TestReportStreamStateNeverSavesResultJSON(t *testing.T) {
	fixClock(t, rsNow)
	st := newStore()
	seedReportLiving(t, st)

	_, err := newReportLogic(t, st).ReportStreamState(rsReq("evt-rs-nosave", model.StreamStatePublishing, 41))
	wantNoErr(t, "成功处理", err)
	wantMethodCount(t, "成功处理", st.log, "live_room_idempotency.SaveResult", 0)

	rec := st.idemAt("evt-rs-nosave")
	if rec == nil {
		t.Fatal("event_id 未登记，Claim 没执行？")
	}
	wantEQ(t, "成功处理后的键", "result_json", rec.ResultJSON, "")
	wantEQ(t, "成功处理后的键", "kind", rec.Kind, model.IdempotencyKindEvent)
	wantEQ(t, "成功处理后的键", "rpc", rec.Rpc, rpcReportStreamState)
	wantEQ(t, "成功处理后的键", "room_id", rec.RoomID, rsRoom)
	wantEQ(t, "成功处理后的键", "session_id", rec.SessionID, rsSess)
}
