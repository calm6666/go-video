package logic

// attachreplay_logic_test.go 覆盖写侧方法 AttachReplay（attachreplaylogic.go）。
//
// 锁的结论（全部由 attachreplaylogic.go + model/live_session.go 的 AttachReplay 语义反推）：
//  1. 本方法**全程不开事务**：状态写是一句带条件的 UPDATE，审计走 StateLogs.Insert（非 Tx）。
//     所以「假件不回滚」在这里不是妥协而是事实：没有事务可回滚。
//  2. 归属与终态的**读侧**校验（房间匹配、场次终态）在抢键之前（:65-72），被拒不烧键；
//     同一道条件在 SQL 的 WHERE 里又存在一遍（:31-32 的注释），并发下以 SQL 为准。
//  3. 「同状态 + 同引用」走 :94-102 的短路：不写 UPDATE、不写审计，但**回包里 replayed=false**。
//     与其余写方法的重放语义不一致，已作为缺口登记并被哨兵钉住。
//  4. 三个引用列只在 >0 时写（model/live_session.go 的 CASE WHEN），所以补全引用是**增量**的：
//     先用 record_id 进 PROCESSING、再用 record_asset_id 进 AVAILABLE 不会把彼此擦掉。
//  5. target 只允许 PROCESSING/AVAILABLE/REMOVED（helpers.go:allowReplayTarget）：
//     状态机里 PROCESSING→NONE 那条「确认无回放」的边**没有入口**，见 README 已知缺口。
//  6. AVAILABLE 前置要求 record_asset_id>0（:55）：本服务不自行判定媒资可播，但拒绝
//     「AVAILABLE 却没有任何媒资引用」的不可解释数据。
//  7. 审计写失败被 l.Errorf 吞掉（:112-118）：状态已改、审计缺失、应答仍是成功，
//     与 §8「状态变更必须留证」相悖，已登记并被哨兵钉住。审计行的 source 也固定写
//     stream_event，即使这是终端/admin 直调的 RPC。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

const (
	arNow      int64 = 1_700_000_600
	arRoom     int64 = 3401
	arOwner    int64 = 9401
	arSess     int64 = 5201
	arRecord   int64 = 66001
	arAsset    int64 = 77001
	arAid      int64 = 88001
	arOtherRoo int64 = 3499
)

// arReq 组一条「把场次推进到 PROCESSING 并登记 record_id」的合法请求。
func arReq(reqID string, target rpc.ReplayState) *rpc.AttachReplayReq {
	return &rpc.AttachReplayReq{
		SessionId: arSess, RoomId: arRoom, ReplayState: target,
		RecordId: arRecord, RecordAssetId: arAsset, RecordAid: arAid,
		RequestId: reqID, TraceId: "trace-ar",
	}
}

func newAttachReplayLogic(t *testing.T, st *store) *AttachReplayLogic {
	t.Helper()
	return NewAttachReplayLogic(context.Background(), st.svcCtx())
}

// seedReplayScene 布一条已结束的场次（replay_state 由用例给），房间与房主齐全。
func seedReplayScene(t *testing.T, st *store, replayState int32) *model.LiveSession {
	t.Helper()
	st.seedRoom(baseRoom(arRoom, arOwner, model.RoomStateFinished))
	st.seedAnchor(baseAnchor(1001, arRoom, arOwner, model.AnchorRoleOwner))
	s := baseSession(arSess, arRoom, arOwner, model.SessionStateEnded)
	s.ReplayState = replayState
	st.seedSession(s)
	return s
}

// TestAttachReplayNoneToProcessingWritesStateAndAudit 骨架用例：
// 读场次 -> 抢键 -> 一句 UPDATE -> 一条 t4 审计 -> 回填结果，全程零事务。
func TestAttachReplayNoneToProcessingWritesStateAndAudit(t *testing.T) {
	fixClock(t, arNow)
	st := newStore()
	seedReplayScene(t, st, model.ReplayStateNone)

	reply, err := newAttachReplayLogic(t, st).AttachReplay(
		arReq("req-ar-proc", rpc.ReplayState_REPLAY_STATE_PROCESSING))
	wantNoErr(t, "挂回放", err)

	wantSeq(t, "挂回放", st.log, 0,
		fmt.Sprintf("live_session.FindOne:%d", arSess),
		"live_room_idempotency.Claim:req-ar-proc",
		fmt.Sprintf("live_session.AttachReplay:%d:%d->%d", arSess, model.ReplayStateNone, model.ReplayStateProcessing),
		// reason 传空：审计键的尾巴就是空串，与 BanRoom/LiftBan 的必填原因形成对照。
		fmt.Sprintf("live_room_state_log.Insert:t%d:%d->%d:%s",
			model.LogTypeReplayState, model.ReplayStateNone, model.ReplayStateProcessing, ""),
		"live_room_idempotency.SaveResult:req-ar-proc",
	)
	wantTxCount(t, "挂回放", st.conn, 0)
	wantMethodCount(t, "挂回放", st.log, "db.TransactCtx", 0)
	wantMethodCount(t, "挂回放", st.log, "live_room_state_log.InsertTx", 0)

	wantEQ(t, "挂回放应答", "session_id", reply.GetSessionId(), arSess)
	wantEQ(t, "挂回放应答", "replay_state", reply.GetReplayState(), rpc.ReplayState_REPLAY_STATE_PROCESSING)
	wantEQ(t, "挂回放应答", "replayed", reply.GetReplayed(), false)

	sess := st.sessionAt(t, arSess)
	wantEQ(t, "挂回放落库", "replay_state", sess.ReplayState, model.ReplayStateProcessing)
	wantEQ(t, "挂回放落库", "record_id", sess.RecordID, arRecord)
	wantEQ(t, "挂回放落库", "record_asset_id", sess.RecordAssetID, arAsset)
	wantEQ(t, "挂回放落库", "record_aid", sess.RecordAid, arAid)
	wantEQ(t, "挂回放落库", "mtime", sess.Mtime, arNow)
	wantEQ(t, "挂回放落库", "state 不被回放改写", sess.State, model.SessionStateEnded)

	logs := st.logsOf(arRoom)
	if len(logs) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(logs))
	}
	wantEQ(t, "回放审计", "state_type", logs[0].StateType, model.LogTypeReplayState)
	wantEQ(t, "回放审计", "from->to", fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "1->2")
	wantEQ(t, "回放审计", "request_id", logs[0].RequestID, "req-ar-proc")
	wantEQ(t, "回放审计", "trace_id", logs[0].TraceID, "trace-ar")
	wantEQ(t, "回放审计", "session_id", logs[0].SessionID, arSess)
	// 第 7 条：这是一次 RPC，却被归因成 stream_event；operator_mid 干脆没填。
	wantEQ(t, "回放审计", "source", logs[0].Source, model.SourceStreamEvent)
	wantEQ(t, "回放审计", "operator_mid", logs[0].OperatorMid, int64(0))
	wantDeepEQ(t, "挂回放落库面", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, sessions: 1, logs: 1, idem: 1})
}

// TestAttachReplayRefsAreFilledIncrementally 钉住第 4 条：引用列只在 >0 时写。
// 从 PROCESSING 升 AVAILABLE 时只带 record_asset_id，先前落的 record_id / record_aid 不得被清零。
func TestAttachReplayRefsAreFilledIncrementally(t *testing.T) {
	fixClock(t, arNow)
	st := newStore()
	seedReplayScene(t, st, model.ReplayStateProcessing)
	st.sessions.rows[0].RecordID = arRecord // 首次只登记了 record_id

	in := arReq("req-ar-avail", rpc.ReplayState_REPLAY_STATE_AVAILABLE)
	in.RecordId = 0
	in.RecordAid = 0
	reply, err := newAttachReplayLogic(t, st).AttachReplay(in)
	wantNoErr(t, "升 AVAILABLE", err)
	wantEQ(t, "升 AVAILABLE 应答", "replay_state", reply.GetReplayState(),
		rpc.ReplayState_REPLAY_STATE_AVAILABLE)

	wantSeq(t, "升 AVAILABLE", st.log, 0,
		fmt.Sprintf("live_session.FindOne:%d", arSess),
		"live_room_idempotency.Claim:req-ar-avail",
		fmt.Sprintf("live_session.AttachReplay:%d:%d->%d", arSess,
			model.ReplayStateProcessing, model.ReplayStateAvailable),
		fmt.Sprintf("live_room_state_log.Insert:t%d:%d->%d:%s",
			model.LogTypeReplayState, model.ReplayStateProcessing, model.ReplayStateAvailable, ""),
		"live_room_idempotency.SaveResult:req-ar-avail",
	)
	sess := st.sessionAt(t, arSess)
	wantEQ(t, "增量补全", "旧 record_id 保留", sess.RecordID, arRecord)
	wantEQ(t, "增量补全", "旧 record_aid 保留", sess.RecordAid, int64(0))
	wantEQ(t, "增量补全", "新 asset 写入", sess.RecordAssetID, arAsset)
	wantEQ(t, "增量补全", "replay_state", sess.ReplayState, model.ReplayStateAvailable)
}

// TestAttachReplaySameStateSameRefsIsNoOp 覆盖第 3 条的短路，并把「回包不标 replayed」
// 这个不一致钉成哨兵（README 已知缺口）。
// TODO(缺陷): :94-102 自称「幂等重放语义」却不置 replayed=true，终端无法区分
// 「这次真的写了」与「本来就已经是这样」；同请求换个 request_id 就会重复占键。
func TestAttachReplaySameStateSameRefsIsNoOp(t *testing.T) {
	fixClock(t, arNow)
	st := newStore()
	seedReplayScene(t, st, model.ReplayStateAvailable)
	st.sessions.rows[0].RecordID = arRecord
	st.sessions.rows[0].RecordAssetID = arAsset
	st.sessions.rows[0].RecordAid = arAid
	before := st.sessionAt(t, arSess).Mtime

	reply, err := newAttachReplayLogic(t, st).AttachReplay(arReq("req-ar-noop",
		rpc.ReplayState_REPLAY_STATE_AVAILABLE))
	wantNoErr(t, "同状态同引用", err)
	wantSeq(t, "同状态同引用", st.log, 0,
		fmt.Sprintf("live_session.FindOne:%d", arSess),
		"live_room_idempotency.Claim:req-ar-noop",
		"live_room_idempotency.SaveResult:req-ar-noop",
	)
	wantMethodCount(t, "同状态同引用", st.log, "live_session.AttachReplay", 0)
	wantMethodCount(t, "同状态同引用", st.log, "live_room_state_log.Insert", 0)
	wantEQ(t, "同状态同引用", "mtime 未动", st.sessionAt(t, arSess).Mtime, before)
	wantEQ(t, "同状态同引用", "审计行数", len(st.logsOf(arRoom)), 0)
	wantEQ(t, "同状态同引用应答", "replay_state", reply.GetReplayState(),
		rpc.ReplayState_REPLAY_STATE_AVAILABLE)
	// 哨兵：这条分支的语义就是重放，但契约位没置起来。
	wantEQ(t, "同状态同引用应答（缺陷现状）", "replayed", reply.GetReplayed(), false)
}

// TestAttachReplaySameStateDifferentRefsCannotBeApplied 与上一条成对，判出短路第二个条件的
// 真实作用域：引用不同 -> 不走短路；但 replayTransitions 没有自环
// （model/errors.go:416-421，AVAILABLE 的出边只有 REMOVED），所以 :104 之后的
// CanReplayTransition 守卫必然先拒（model/live_session.go:424-426 在发 SQL 之前返回错误）。
// 合起来：同状态请求只有「静默无操作」与「报错」两种结局，replayRefsEqual 的第二个条件
// 永远不可能把一次同状态请求变成一次写 —— 「只补引用、不改状态」这个入口不存在。
// TODO(缺陷): model/live_session.go:429 的注释写着「回放是逐步补全的（先 record_id，
// 再 asset_id，最后 aid）」，但在本 RPC 上只有伴随状态推进的补全才可能落库；
// 已 AVAILABLE 后发现 asset_id 录错，无任何路径可修正（引用列也永不回 0）。
// 下表用「同样的引用改动 + 合法状态边」作对照，判别出拦下它的是状态机而不是引用校验。
func TestAttachReplaySameStateDifferentRefsCannotBeApplied(t *testing.T) {
	t.Run("同状态改引用被状态机拒绝", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		seedReplayScene(t, st, model.ReplayStateAvailable)
		st.sessions.rows[0].RecordID = arRecord
		st.sessions.rows[0].RecordAssetID = arAsset
		st.sessions.rows[0].RecordAid = arAid
		before := st.sessionAt(t, arSess).Mtime

		in := arReq("req-ar-refchange", rpc.ReplayState_REPLAY_STATE_AVAILABLE)
		in.RecordId = arRecord + 1 // 只有引用变了，状态没变

		_, err := newAttachReplayLogic(t, st).AttachReplay(in)
		wantErrIs(t, "同状态改引用", err, model.ErrInvalidReplayTransition)
		wantSeq(t, "同状态改引用", st.log, 0,
			fmt.Sprintf("live_session.FindOne:%d", arSess),
			"live_room_idempotency.Claim:req-ar-refchange",
			fmt.Sprintf("live_session.AttachReplay:%d:%d->%d", arSess,
				model.ReplayStateAvailable, model.ReplayStateAvailable),
		)
		wantMethodCount(t, "同状态改引用", st.log, "live_room_state_log.Insert", 0)
		sess := st.sessionAt(t, arSess)
		wantEQ(t, "同状态改引用残留", "record_id 未改", sess.RecordID, arRecord)
		wantEQ(t, "同状态改引用残留", "replay_state 未改", sess.ReplayState, model.ReplayStateAvailable)
		wantEQ(t, "同状态改引用残留", "mtime 未改", sess.Mtime, before)
		wantKeyBurnedNoResult(t, "同状态改引用", "req-ar-refchange", st)
	})

	t.Run("对照：同样的引用改动配上合法状态边就能落", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		seedReplayScene(t, st, model.ReplayStateProcessing)
		st.sessions.rows[0].RecordID = arRecord
		st.sessions.rows[0].RecordAssetID = arAsset

		in := arReq("req-ar-refchange-legal", rpc.ReplayState_REPLAY_STATE_AVAILABLE)
		in.RecordId = arRecord + 1 // 与上一条同样的改动
		_, err := newAttachReplayLogic(t, st).AttachReplay(in)
		wantNoErr(t, "对照", err)

		sess := st.sessionAt(t, arSess)
		wantEQ(t, "对照落库", "record_id 改了", sess.RecordID, arRecord+1)
		wantEQ(t, "对照落库", "旧 asset_id 保留", sess.RecordAssetID, arAsset)
		wantEQ(t, "对照落库", "replay_state", sess.ReplayState, model.ReplayStateAvailable)
		wantMethodCount(t, "对照", st.log, "live_session.AttachReplay", 1)
	})
}

// TestAttachReplayConditionMissIsTranslated 复现「读到 PROCESSING，写之前被并发升成 AVAILABLE」：
// UPDATE 的 replay_state=from 条件未命中 -> 回查一次 -> 翻译成具体原因，
// 而不是笼统的「并发冲突」。
func TestAttachReplayConditionMissIsTranslated(t *testing.T) {
	fixClock(t, arNow)
	st := newStore()
	defer st.checkRaces(t)
	seedReplayScene(t, st, model.ReplayStateProcessing)
	st.raceBefore("live_session.AttachReplay", func() {
		st.sessions.rows[0].ReplayState = model.ReplayStateAvailable
		st.sessions.rows[0].RecordAssetID = arAsset
	})

	_, err := newAttachReplayLogic(t, st).AttachReplay(arReq("req-ar-race",
		rpc.ReplayState_REPLAY_STATE_AVAILABLE))
	wantErrIs(t, "条件未命中", err, model.ErrInvalidReplayTransition)
	wantErrContains(t, "条件未命中", err, "3->3")
	wantSeq(t, "条件未命中", st.log, 0,
		fmt.Sprintf("live_session.FindOne:%d", arSess),
		"live_room_idempotency.Claim:req-ar-race",
		fmt.Sprintf("live_session.AttachReplay:%d:%d->%d", arSess,
			model.ReplayStateProcessing, model.ReplayStateAvailable),
		// replayMissCause 的回查
		fmt.Sprintf("live_session.FindOne:%d", arSess),
	)
	wantMethodCount(t, "条件未命中", st.log, "live_room_state_log.Insert", 0)
	wantEQ(t, "条件未命中残留", "状态是并发者写的", st.sessionAt(t, arSess).ReplayState,
		model.ReplayStateAvailable)
	wantKeyBurnedNoResult(t, "条件未命中", "req-ar-race", st)
}

// TestAttachReplayAuditFailureIsSwallowed 哨兵用例（README 已知缺口，第 7 条）：
// 审计写失败只 l.Errorf，应答仍是成功 -> 回放状态改了却查不到任何证据。
// TODO(缺陷): §8 要求状态变更留证；这里应当至少让调用方知道审计缺失。
func TestAttachReplayAuditFailureIsSwallowed(t *testing.T) {
	fixClock(t, arNow)
	st := newStore()
	seedReplayScene(t, st, model.ReplayStateNone)
	st.stateLogs.failWith("Insert", errors.New("audit table down"))

	reply, err := newAttachReplayLogic(t, st).AttachReplay(
		arReq("req-ar-auditfail", rpc.ReplayState_REPLAY_STATE_PROCESSING))
	wantNoErr(t, "审计写失败被吞", err)
	wantEQ(t, "审计写失败应答", "replay_state", reply.GetReplayState(),
		rpc.ReplayState_REPLAY_STATE_PROCESSING)
	wantSeq(t, "审计写失败", st.log, 0,
		fmt.Sprintf("live_session.FindOne:%d", arSess),
		"live_room_idempotency.Claim:req-ar-auditfail",
		fmt.Sprintf("live_session.AttachReplay:%d:%d->%d", arSess,
			model.ReplayStateNone, model.ReplayStateProcessing),
		fmt.Sprintf("live_room_state_log.Insert:t%d:%d->%d:%s",
			model.LogTypeReplayState, model.ReplayStateNone, model.ReplayStateProcessing, ""),
		"live_room_idempotency.SaveResult:req-ar-auditfail",
	)
	wantEQ(t, "审计写失败后果", "状态已改", st.sessionAt(t, arSess).ReplayState,
		model.ReplayStateProcessing)
	wantEQ(t, "审计写失败后果", "审计行数", len(st.logsOf(arRoom)), 0)
}

// TestAttachReplayGuardsRunBeforeClaim 三条读侧拒绝都在抢键之前，键完好可重试。
func TestAttachReplayGuardsRunBeforeClaim(t *testing.T) {
	t.Run("场次不存在", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		_, err := newAttachReplayLogic(t, st).AttachReplay(
			arReq("req-ar-nosess", rpc.ReplayState_REPLAY_STATE_PROCESSING))
		wantErrIs(t, "场次不存在", err, model.ErrSessionNotFound)
		wantSeq(t, "场次不存在", st.log, 0, fmt.Sprintf("live_session.FindOne:%d", arSess))
		wantKeyUnburned(t, "场次不存在", "req-ar-nosess", st)
	})

	t.Run("场次不属于该房间", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		seedReplayScene(t, st, model.ReplayStateNone)
		in := arReq("req-ar-room", rpc.ReplayState_REPLAY_STATE_PROCESSING)
		in.RoomId = arOtherRoo

		_, err := newAttachReplayLogic(t, st).AttachReplay(in)
		wantErrIs(t, "跨房间", err, model.ErrSessionRoomMismatch)
		wantErrContains(t, "跨房间", err, fmt.Sprintf("属于房间 %d", arRoom))
		wantSeq(t, "跨房间", st.log, 0, fmt.Sprintf("live_session.FindOne:%d", arSess))
		wantKeyUnburned(t, "跨房间", "req-ar-room", st)
	})

	t.Run("场次未终态不得挂回放", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		st.seedRoom(baseRoom(arRoom, arOwner, model.RoomStateLiving))
		s := baseSession(arSess, arRoom, arOwner, model.SessionStateLiving)
		s.StartedAt = arNow - 100
		st.seedSession(s)

		_, err := newAttachReplayLogic(t, st).AttachReplay(
			arReq("req-ar-alive", rpc.ReplayState_REPLAY_STATE_PROCESSING))
		wantErrIs(t, "进行中场次", err, model.ErrSessionNotTerminal)
		wantErrContains(t, "进行中场次", err, "state=2")
		wantSeq(t, "进行中场次", st.log, 0, fmt.Sprintf("live_session.FindOne:%d", arSess))
		wantKeyUnburned(t, "进行中场次", "req-ar-alive", st)
	})
}

// TestAttachReplayGuardTableRejectsWithZeroDependencyCalls 入参守卫表：全部零调用。
func TestAttachReplayGuardTableRejectsWithZeroDependencyCalls(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.AttachReplayReq)
		want   error
		frag   string
	}{
		{"session_id 非法", func(in *rpc.AttachReplayReq) { in.SessionId = 0 }, model.ErrInvalidSessionID, ""},
		{"room_id 非法", func(in *rpc.AttachReplayReq) { in.RoomId = -1 }, model.ErrInvalidRoomID, ""},
		{"缺幂等键", func(in *rpc.AttachReplayReq) { in.RequestId = "" }, model.ErrRequestIDRequired, ""},
		{"目标态为 UNSPECIFIED", func(in *rpc.AttachReplayReq) {
			in.ReplayState = rpc.ReplayState_REPLAY_STATE_UNSPECIFIED
		}, model.ErrReplayStateInvalid, "replay_state=0"},
		{"目标态为 NONE（状态机里有这条边，但没有入口）", func(in *rpc.AttachReplayReq) {
			in.ReplayState = rpc.ReplayState_REPLAY_STATE_NONE
		}, model.ErrReplayStateInvalid, "replay_state=1"},
		{"record_id 为负", func(in *rpc.AttachReplayReq) { in.RecordId = -1 }, model.ErrSettingInvalid, ""},
		{"record_asset_id 为负", func(in *rpc.AttachReplayReq) { in.RecordAssetId = -5 }, model.ErrSettingInvalid, ""},
		{"AVAILABLE 缺 record_asset_id", func(in *rpc.AttachReplayReq) {
			in.ReplayState = rpc.ReplayState_REPLAY_STATE_AVAILABLE
			in.RecordAssetId = 0
		}, model.ErrReplayStateInvalid, "record_asset_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, arNow)
			st := newStore()
			seedReplayScene(t, st, model.ReplayStateNone)
			in := arReq("req-ar-guard", rpc.ReplayState_REPLAY_STATE_PROCESSING)
			tc.mutate(in)

			_, err := newAttachReplayLogic(t, st).AttachReplay(in)
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name, err, tc.frag)
			}
			wantNoCallAfter(t, tc.name, st.log, 0)
			wantEQ(t, tc.name, "幂等表行数", st.counts().idem, 0)
		})
	}
}

// TestAttachReplayRemovedIsTerminal 钉住状态机：REMOVED 没有出边，
// 所以从 REMOVED 出发的一切目标都在 SQL 条件上失败（并发者除外，见下）。
// 这里读侧的 target 校验放行了 AVAILABLE，只有 WHERE 拦得住 —— 判别出「两级校验」的分工。
func TestAttachReplayRemovedIsTerminal(t *testing.T) {
	fixClock(t, arNow)
	st := newStore()
	seedReplayScene(t, st, model.ReplayStateRemoved)
	st.sessions.rows[0].RecordAssetID = arAsset

	_, err := newAttachReplayLogic(t, st).AttachReplay(
		arReq("req-ar-removed", rpc.ReplayState_REPLAY_STATE_AVAILABLE))
	// 假件的 CanReplayTransition 守卫在发 SQL 之前返回错误（与真实 WHERE 命中 0 行不同源，
	// 但两条路都到不了写：这里断言的是「没有 UPDATE 落库」这一共同后果）。
	wantErrIs(t, "REMOVED 无出边", err, model.ErrInvalidReplayTransition)
	wantSeq(t, "REMOVED 无出边", st.log, 0,
		fmt.Sprintf("live_session.FindOne:%d", arSess),
		"live_room_idempotency.Claim:req-ar-removed",
		fmt.Sprintf("live_session.AttachReplay:%d:%d->%d", arSess,
			model.ReplayStateRemoved, model.ReplayStateAvailable),
	)
	wantMethodCount(t, "REMOVED 无出边", st.log, "live_room_state_log.Insert", 0)
	wantEQ(t, "REMOVED 无出边残留", "replay_state 未变", st.sessionAt(t, arSess).ReplayState,
		model.ReplayStateRemoved)
	wantKeyBurnedNoResult(t, "REMOVED 无出边", "req-ar-removed", st)
}

// TestAttachReplayReplayTriad 幂等三态。
func TestAttachReplayReplayTriad(t *testing.T) {
	t.Run("有结果快照", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		seedReplayScene(t, st, model.ReplayStateProcessing)
		st.seedIdem("req-ar-replay", rpcAttachReplay,
			fmt.Sprintf(`{"session_id":%d,"replay_state":3}`, arSess))

		reply, err := newAttachReplayLogic(t, st).AttachReplay(
			arReq("req-ar-replay", rpc.ReplayState_REPLAY_STATE_AVAILABLE))
		wantNoErr(t, "重放", err)
		wantSeq(t, "重放", st.log, 0,
			fmt.Sprintf("live_session.FindOne:%d", arSess),
			"live_room_idempotency.Claim:req-ar-replay",
			"live_room_idempotency.Find:req-ar-replay",
		)
		wantEQ(t, "重放应答", "replayed", reply.GetReplayed(), true)
		wantEQ(t, "重放应答", "replay_state 沿用快照", reply.GetReplayState(),
			rpc.ReplayState_REPLAY_STATE_AVAILABLE)
		wantEQ(t, "重放", "状态未被再改", st.sessionAt(t, arSess).ReplayState,
			model.ReplayStateProcessing)
	})

	t.Run("无结果快照", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		seedReplayScene(t, st, model.ReplayStateNone)
		st.seedIdem("req-ar-noreplay", rpcAttachReplay, "")

		_, err := newAttachReplayLogic(t, st).AttachReplay(
			arReq("req-ar-noreplay", rpc.ReplayState_REPLAY_STATE_PROCESSING))
		wantErrIs(t, "结果缺失", err, model.ErrIdempotencyResultMissing)
	})

	t.Run("键被别的 RPC 用过", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		seedReplayScene(t, st, model.ReplayStateNone)
		st.seedIdem("req-ar-cross", rpcEndLive, `{"session_id":1}`)

		_, err := newAttachReplayLogic(t, st).AttachReplay(
			arReq("req-ar-cross", rpc.ReplayState_REPLAY_STATE_PROCESSING))
		wantErrIs(t, "串方法", err, model.ErrRequestIDReused)
	})
}

// TestAttachReplayDependencyFailuresPropagate 两处依赖失败都必须原样抛出。
func TestAttachReplayDependencyFailurePropagates(t *testing.T) {
	dbFail := errors.New("dial tcp 127.0.0.1:3306: connect refused")

	t.Run("首读失败", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		seedReplayScene(t, st, model.ReplayStateNone)
		st.sessions.failWith("FindOne", dbFail)

		_, err := newAttachReplayLogic(t, st).AttachReplay(
			arReq("req-ar-readfail", rpc.ReplayState_REPLAY_STATE_PROCESSING))
		wantErrIs(t, "首读失败", err, dbFail)
		wantSeq(t, "首读失败", st.log, 0, fmt.Sprintf("live_session.FindOne:%d", arSess))
		wantKeyUnburned(t, "首读失败", "req-ar-readfail", st)
	})

	t.Run("UPDATE 失败", func(t *testing.T) {
		fixClock(t, arNow)
		st := newStore()
		seedReplayScene(t, st, model.ReplayStateNone)
		st.sessions.failWith("AttachReplay", dbFail)

		_, err := newAttachReplayLogic(t, st).AttachReplay(
			arReq("req-ar-writefail", rpc.ReplayState_REPLAY_STATE_PROCESSING))
		wantErrContains(t, "UPDATE 失败", err, "connect refused")
		wantSeq(t, "UPDATE 失败", st.log, 0,
			fmt.Sprintf("live_session.FindOne:%d", arSess),
			"live_room_idempotency.Claim:req-ar-writefail",
			fmt.Sprintf("live_session.AttachReplay:%d:%d->%d", arSess,
				model.ReplayStateNone, model.ReplayStateProcessing),
		)
		wantEQ(t, "UPDATE 失败残留", "replay_state", st.sessionAt(t, arSess).ReplayState,
			model.ReplayStateNone)
		wantKeyBurnedNoResult(t, "UPDATE 失败", "req-ar-writefail", st)
	})
}
