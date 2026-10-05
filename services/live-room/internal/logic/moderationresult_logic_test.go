package logic

// moderationresult_logic_test.go 覆盖 ApplyRoomModerationResult（applyroommoderationresultlogic.go）。
//
// 这是 moderation.result.v1 的入口，真值恒在上游：本服务只保存投影，因此
// 「哪些结论被接受、落到哪一列、要不要顺带推业务状态」就是本文件的全部结论面。
//
// 锁的结论：
//  1. 入参守卫顺序 event_id → room_id → task_id → reason → verdict，且这五条全部在
//     **触库之前**（事件键不烧）；room_id 与 task_id 同时非法时先报 room_id，用来证明顺序。
//  2. 事件去重按 event_id：重复投递回 applied=false，且回的是**当前投影**而不是首次那份
//     快照（两者可以不同），也不再产生任何写。
//  3. task_id 必须等于 live_room.moderation_task_id：陈旧结论即使 verdict 合法也不得覆盖
//     新任务（ErrTaskMismatch）。
//  4. verify_state 与房间业务状态是两条矩阵：资料通过只把 PENDING 抬到 READY，
//     驳回只把 READY 降回 PENDING；LIVING/BANNED 没有对应合法边，只回写结论，
//     绝不伪造一次状态迁移；BANNED 不因资料通过而解封。
//  5. 两条矩阵都由 model 守：verify 侧只有 Reviewing->Passed/Rejected 这类边，
//     所以「已 PASSED 的房间被巡检打回」在本入口是**非法迁移**（缺口，见 README）。
//  6. 结论回写与审计在同一事务；房间不需要迁移时只写 verify_state/reject_reason，
//     且 SetVerifyResultTx 的 allowStates 直接取「刚读到的当前状态」，等价于只防并发插队。
//
// 本轮发现并 pin 的生产现状（未改代码，全部登记进 README 已知缺口）：
//   - 缺口 E：verdict 白名单只挡 UNSPECIFIED（applyroommoderationresultlogic.go:57-60），
//     未定义取值（如 99）穿过守卫、烧掉 event_id 后才被 verifyTargetForVerdict 拒绝。
//   - 缺口 F：同上，ErrTaskMismatch / ErrRoomNotFound / ErrInvalidVerifyTransition /
//     ErrConcurrentUpdate 都发生在 Claim 之后，一次坏投递就永久占掉这个 event_id。
//   - 缺口 G：资料结论「与现状一致」但房间在 PENDING 时仍会推 READY，并写出一条
//     verify_state 3->3 的幽灵审计（apply() 无条件补资料审计）。
//   - 缺口 H：operator 由调用方自报且不校验（对比 BanRoom 的 checkOperator），
//     机审填 0 合法，因此审计里的「处理人」不是可信归因。
//   - 缺口 I：applyroommoderationresultlogic.go:142 与 :159 的 truncateRunes(reason, 250)
//     不可达——checkReason 已经在 >250 时整条拒绝，超长尾巴永远不会被截断落库。

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
	mrNow   int64 = 1_700_000_000
	mrRoom  int64 = 4301
	mrTask  int64 = 8801
	mrEvent       = "evt-moderation-1"
)

// mrReq 布一条「除 event_id 外都合法」的结论回写请求：资料在审、结论通过。
func mrReq(eventID string) *rpc.ApplyRoomModerationResultReq {
	return &rpc.ApplyRoomModerationResultReq{
		RoomId:   mrRoom,
		TaskId:   mrTask,
		Verdict:  rpc.ModerationVerdict_VERDICT_PASS,
		Reason:   "机器审核通过",
		EventId:  eventID,
		TraceId:  "trace-mod",
		Operator: 0,
	}
}

func newMRLogic(t *testing.T, st *store, conf ...config.LiveRoomConf) *ApplyRoomModerationResultLogic {
	t.Helper()
	c := testLiveRoomConf()
	if len(conf) > 0 {
		c = conf[0]
	}
	return NewApplyRoomModerationResultLogic(context.Background(), st.svcCtxWith(c))
}

// seedMRScene 布一个「资料在审 + 指定业务状态」的房间，返回库存行本身便于改形态。
func seedMRScene(t *testing.T, st *store, roomState, verifyState int32) *model.LiveRoom {
	t.Helper()
	r := baseRoom(mrRoom, 101, roomState)
	r.VerifyState = verifyState
	r.ModerationTaskID = mrTask
	return st.seedRoom(r)
}

var errMRDown = errors.New("moderation result: dependency down")

// TestModerationResultGuardsRejectBeforeAnyDependency 钉五条入参守卫都在抢键与触库之前：
// 事件消费方可以安全重投同一 event_id（键没烧），也不会留下半条投影。
func TestModerationResultGuardsRejectBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(req *rpc.ApplyRoomModerationResultReq)
		want   error
	}{
		{"event_id 为空", func(r *rpc.ApplyRoomModerationResultReq) { r.EventId = "" }, model.ErrEventIDRequired},
		{"event_id 只有空白", func(r *rpc.ApplyRoomModerationResultReq) { r.EventId = "   " }, model.ErrEventIDRequired},
		{"event_id 超列宽", func(r *rpc.ApplyRoomModerationResultReq) {
			r.EventId = strings.Repeat("e", maxDedupIDBytes+1)
		}, model.ErrDedupIDTooLong},
		{"room_id 非正", func(r *rpc.ApplyRoomModerationResultReq) { r.RoomId = 0 }, model.ErrInvalidRoomID},
		{"task_id 非正", func(r *rpc.ApplyRoomModerationResultReq) { r.TaskId = 0 }, model.ErrTaskMismatch},
		{"reason 超列宽", func(r *rpc.ApplyRoomModerationResultReq) {
			r.Reason = strings.Repeat("违", maxReasonRunes+1)
		}, model.ErrReasonTooLong},
		{"verdict UNSPECIFIED", func(r *rpc.ApplyRoomModerationResultReq) {
			r.Verdict = rpc.ModerationVerdict_VERDICT_UNSPECIFIED
		}, model.ErrVerdictInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, mrNow)
			st := newStore()
			seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)

			req := mrReq(mrEvent)
			tc.mutate(req)
			got, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：守卫拒绝后仍返回 reply %+v", tc.name, got)
			}
			wantNoCallAfter(t, tc.name, st.log, 0)
			wantKeyUnburned(t, tc.name, mrEvent, st)
			row := st.roomAt(t, mrRoom)
			wantEQ(t, tc.name, "房间状态不动", row.State, model.RoomStatePending)
			wantEQ(t, tc.name, "资料状态不动", row.VerifyState, model.VerifyStateReviewing)
		})
	}

	t.Run("nil 请求", func(t *testing.T) {
		st := newStore()
		seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)
		got, err := newMRLogic(t, st).ApplyRoomModerationResult(nil)
		wantErrIs(t, "nil 请求", err, model.ErrEventIDRequired)
		if got != nil {
			t.Errorf("nil 请求仍返回 reply %+v", got)
		}
		wantNoCallAfter(t, "nil 请求", st.log, 0)
	})

	// 顺序判别：room_id 与 task_id 同时非法，必须先报 room_id（否则守卫顺序变了也测不出来）。
	t.Run("守卫顺序 room_id 先于 task_id", func(t *testing.T) {
		st := newStore()
		seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)
		req := mrReq(mrEvent)
		req.RoomId = -1
		req.TaskId = -1
		_, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
		wantErrIs(t, "守卫顺序", err, model.ErrInvalidRoomID)
		wantNoCallAfter(t, "守卫顺序", st.log, 0)
	})
}

// TestModerationResultUndefinedVerdictBurnsEventKey 钉缺口 E：
// 白名单只挡 UNSPECIFIED。verdict=99 穿过守卫、把 event_id 登记成 kind=event 的行，
// 然后才被状态机裁决拒绝——于是这个键再也用不了（重投同一 event_id 会拿到重放应答）。
func TestModerationResultUndefinedVerdictBurnsEventKey(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)

	req := mrReq(mrEvent)
	req.Verdict = rpc.ModerationVerdict(99)
	_, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
	wantErrIs(t, "未定义 verdict", err, model.ErrVerdictInvalid)

	// 未定义 verdict 也先读了房间行才在状态机裁决处被拒：键已经烧掉。
	wantSeq(t, "未定义 verdict 轨迹", st.log, 0,
		fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
	)
	wantKeyBurnedNoResult(t, "未定义 verdict", mrEvent, st)
	if rec := st.idemAt(mrEvent); rec == nil || rec.Kind != model.IdempotencyKindEvent {
		t.Errorf("事件键的 kind 必须是 IdempotencyKindEvent，实得 %+v", rec)
	}
	wantTxCount(t, "未定义 verdict", st.conn, 0)

	// 判别面：同一 event_id 重投不再报错，而是拿到「重复投递」的应答。
	second, err := newMRLogic(t, st).ApplyRoomModerationResult(mrReq(mrEvent))
	wantNoErr(t, "未定义 verdict 后重投", err)
	wantEQ(t, "未定义 verdict 后重投", "applied", second.GetApplied(), false)
	wantEQ(t, "未定义 verdict 后重投", "message", second.GetMessage(), "同一 event_id 重复投递，未产生迁移")
}

// TestModerationResultGuardsAfterClaimBurnTheEventKey 钉缺口 F：
// 三类业务失败都在 Claim 之后，事件键被消费但没有结果快照。
// 这里锁的是「坏投递的代价」，不是要求它成功——消费方若按「报错就重投」实现，
// 第二次就会被当成重复投递，坏结论反而永远进不来。
func TestModerationResultGuardsAfterClaimBurnTheEventKey(t *testing.T) {
	cases := []struct {
		name      string
		wantErr   error
		prep      func(st *store)
		mutateReq func(req *rpc.ApplyRoomModerationResultReq)
	}{
		{"陈旧 task_id", model.ErrTaskMismatch, func(st *store) {
			st.rooms.rows[0].ModerationTaskID = mrTask + 100 // 房间已经换了新任务
		}, nil},
		{"非法资料迁移（PASSED 被巡检打回）", model.ErrInvalidVerifyTransition, func(st *store) {
			st.rooms.rows[0].VerifyState = model.VerifyStatePassed
		}, func(req *rpc.ApplyRoomModerationResultReq) {
			req.Verdict = rpc.ModerationVerdict_VERDICT_REJECT
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, mrNow)
			st := newStore()
			seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)
			tc.prep(st)

			req := mrReq(mrEvent)
			if tc.mutateReq != nil {
				tc.mutateReq(req)
			}
			_, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
			wantErrIs(t, tc.name, err, tc.wantErr)
			wantKeyBurnedNoResult(t, tc.name, mrEvent, st)
			wantTxCount(t, tc.name, st.conn, 0)
			wantEQ(t, tc.name, "无审计", len(st.logsOf(mrRoom)), 0)
		})
	}

	// 房间不存在：Claim 先成功，随后 FindOne 空——同样留下「已消费但无结果」的键。
	t.Run("房间不存在", func(t *testing.T) {
		fixClock(t, mrNow)
		st := newStore()
		_, err := newMRLogic(t, st).ApplyRoomModerationResult(mrReq(mrEvent))
		wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
		wantSeq(t, "房间不存在轨迹", st.log, 0,
			fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
			fmt.Sprintf("live_room.FindOne:%d", mrRoom),
		)
		wantKeyBurnedNoResult(t, "房间不存在", mrEvent, st)
		wantTxCount(t, "房间不存在", st.conn, 0)
	})
}

// TestModerationResultPassPromotesPendingRoom 钉主路径 PENDING+REVIEWING+PASS：
// 一个事务里「房间 PENDING->READY（patch 带上资料结论）+ 两条审计」，
// 应答读自提交后的回读，事件键回填结果。
func TestModerationResultPassPromotesPendingRoom(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)

	reply, err := newMRLogic(t, st).ApplyRoomModerationResult(mrReq(mrEvent))
	wantNoErr(t, "PASS 抬 READY", err)
	wantSeq(t, "PASS 轨迹", st.log, 0,
		fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
		"db.TransactCtx",
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v1", mrRoom, model.RoomStatePending, model.RoomStateReady),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s", model.LogTypeRoomState, model.RoomStatePending, model.RoomStateReady, "机器审核通过"),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s", model.LogTypeVerifyState, model.VerifyStateReviewing, model.VerifyStatePassed, "机器审核通过"),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
		fmt.Sprintf("live_room_idempotency.SaveResult:%s", mrEvent),
	)
	wantTxCount(t, "PASS 落库", st.conn, 1)

	wantEQ(t, "PASS 应答", "room_id", reply.GetRoomId(), mrRoom)
	wantEQ(t, "PASS 应答", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_PASSED)
	wantEQ(t, "PASS 应答", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_READY)
	wantEQ(t, "PASS 应答", "applied", reply.GetApplied(), true)
	wantEQ(t, "PASS 应答", "message 留空（正常推进不需要解释）", reply.GetMessage(), "")

	row := st.roomAt(t, mrRoom)
	wantEQ(t, "PASS 库存", "state", row.State, model.RoomStateReady)
	wantEQ(t, "PASS 库存", "verify_state", row.VerifyState, model.VerifyStatePassed)
	wantEQ(t, "PASS 库存", "state_version", row.StateVersion, int32(2))
	wantEQ(t, "PASS 库存", "mtime", row.Mtime, mrNow)
	// 通过时 reject_reason 必须被清空（patch 恒写，不留下一次驳回的残渣）。
	wantEQ(t, "PASS 库存", "reject_reason", row.RejectReason, "")

	logs := st.logsOf(mrRoom)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2（房间态 + 资料态）", len(logs))
	}
	wantEQ(t, "PASS 审计", "第一条是房间态", logs[0].StateType, model.LogTypeRoomState)
	wantEQ(t, "PASS 审计", "第二条是资料态", logs[1].StateType, model.LogTypeVerifyState)
	for i, l := range logs {
		wantEQ(t, fmt.Sprintf("PASS 审计[%d]", i), "source", l.Source, model.SourceModerationResult)
		wantEQ(t, fmt.Sprintf("PASS 审计[%d]", i), "event_id", l.EventID, mrEvent)
		wantEQ(t, fmt.Sprintf("PASS 审计[%d]", i), "trace_id", l.TraceID, "trace-mod")
		wantEQ(t, fmt.Sprintf("PASS 审计[%d]", i), "operator 由调用方自报（缺口 H）", l.OperatorMid, int64(0))
	}
	// 事件键登记的是 kind=event + 本方法名，且结果快照已回填。
	rec := st.idemAt(mrEvent)
	if rec == nil {
		t.Fatalf("event_id %q 未登记", mrEvent)
	}
	wantEQ(t, "PASS 事件键", "kind", rec.Kind, model.IdempotencyKindEvent)
	wantEQ(t, "PASS 事件键", "rpc", rec.Rpc, rpcApplyRoomModerationResult)
	wantEQ(t, "PASS 事件键", "room_id", rec.RoomID, mrRoom)
	if rec.ResultJSON == "" {
		t.Errorf("首次执行必须回填结果快照")
	}
}

// TestModerationResultRejectDemotesReadyRoom 钉 READY+REVIEWING+REJECT：
// READY->PENDING 是唯一合法的「降级」边，驳回原因落到 reject_reason。
func TestModerationResultRejectDemotesReadyRoom(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStateReady, model.VerifyStateReviewing)

	req := mrReq(mrEvent)
	req.Verdict = rpc.ModerationVerdict_VERDICT_REJECT
	req.Reason = "封面含违规元素"
	reply, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
	wantNoErr(t, "REJECT 降级", err)
	wantSeq(t, "REJECT 轨迹", st.log, 0,
		fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
		"db.TransactCtx",
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v1", mrRoom, model.RoomStateReady, model.RoomStatePending),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s", model.LogTypeRoomState, model.RoomStateReady, model.RoomStatePending, "封面含违规元素"),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s", model.LogTypeVerifyState, model.VerifyStateReviewing, model.VerifyStateRejected, "封面含违规元素"),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
		fmt.Sprintf("live_room_idempotency.SaveResult:%s", mrEvent),
	)
	wantTxCount(t, "REJECT 落库", st.conn, 1)
	wantEQ(t, "REJECT 应答", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_REJECTED)
	wantEQ(t, "REJECT 应答", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_PENDING)
	wantEQ(t, "REJECT 应答", "applied", reply.GetApplied(), true)

	row := st.roomAt(t, mrRoom)
	wantEQ(t, "REJECT 库存", "state", row.State, model.RoomStatePending)
	wantEQ(t, "REJECT 库存", "verify_state", row.VerifyState, model.VerifyStateRejected)
	wantEQ(t, "REJECT 库存", "reject_reason", row.RejectReason, "封面含违规元素")
	wantEQ(t, "REJECT 库存", "state_version", row.StateVersion, int32(2))
}

// TestModerationResultLivingRoomOnlyWritesConclusion 钉「两条矩阵分开」：
// 房间在 LIVING 时没有 Living->Pending 这条合法边，所以驳回只回写资料结论，
// 走 SetVerifyResultTx（不推进业务状态、不推 state_version），
// 并且 applied 仍为 true + message 说明「状态未迁移」。
func TestModerationResultLivingRoomOnlyWritesConclusion(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	r := seedMRScene(t, st, model.RoomStateLiving, model.VerifyStateReviewing)
	r.ActiveSessionID = 9001
	r.ActiveStreamID = "stream-1"

	req := mrReq(mrEvent)
	req.Verdict = rpc.ModerationVerdict_VERDICT_REJECT
	req.Reason = "直播内容违规"
	reply, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
	wantNoErr(t, "LIVING 只回写结论", err)
	wantSeq(t, "LIVING 轨迹", st.log, 0,
		fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
		"db.TransactCtx",
		fmt.Sprintf("live_room.SetVerifyResultTx:%d:%d", mrRoom, model.VerifyStateRejected),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s", model.LogTypeVerifyState, model.VerifyStateReviewing, model.VerifyStateRejected, "直播内容违规"),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
		fmt.Sprintf("live_room_idempotency.SaveResult:%s", mrEvent),
	)
	wantTxCount(t, "LIVING 落库", st.conn, 1)
	wantMethodCount(t, "LIVING 不推业务状态", st.log, "live_room.TransitionTx", 0)
	wantEQ(t, "LIVING 应答", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_LIVING)
	wantEQ(t, "LIVING 应答", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_REJECTED)
	wantEQ(t, "LIVING 应答", "applied（结论确实落库了）", reply.GetApplied(), true)
	wantEQ(t, "LIVING 应答", "message", reply.GetMessage(), "资料结论已回写；房间当前状态不允许业务状态迁移")

	row := st.roomAt(t, mrRoom)
	wantEQ(t, "LIVING 库存", "state 仍在直播", row.State, model.RoomStateLiving)
	wantEQ(t, "LIVING 库存", "state_version 不推", row.StateVersion, int32(1))
	wantEQ(t, "LIVING 库存", "verify_state", row.VerifyState, model.VerifyStateRejected)
	wantEQ(t, "LIVING 库存", "reject_reason", row.RejectReason, "直播内容违规")
	// 资料结论回写不是业务迁移：在播投影不能被顺手清掉。
	wantEQ(t, "LIVING 库存", "active_session_id", row.ActiveSessionID, int64(9001))
	wantEQ(t, "LIVING 库存", "active_stream_id", row.ActiveStreamID, "stream-1")
	wantEQ(t, "LIVING 库存", "审计只有一条（资料态）", len(st.logsOf(mrRoom)), 1)
}

// TestModerationResultBannedRoomNeverAutoUnbanned 钉「BANNED 不因资料通过而解封」：
// 只有 LiftBan 能改房间出 BANNED。资料通过时只回写 verify_state，
// 应答照样声称 applied=true，但 room_state 仍是 BANNED。
func TestModerationResultBannedRoomNeverAutoUnbanned(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	b := seedMRScene(t, st, model.RoomStateBanned, model.VerifyStateReviewing)
	b.RejectReason = "上一轮驳回"

	reply, err := newMRLogic(t, st).ApplyRoomModerationResult(mrReq(mrEvent))
	wantNoErr(t, "BANNED 通过", err)
	wantMethodCount(t, "BANNED 不解封", st.log, "live_room.TransitionTx", 0)
	wantMethodCount(t, "BANNED 不解封", st.log, "live_room.SetBanUntilTx", 0)
	wantEQ(t, "BANNED 应答", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_BANNED)
	wantEQ(t, "BANNED 应答", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_PASSED)
	wantEQ(t, "BANNED 应答", "message", reply.GetMessage(), "资料结论已回写；房间当前状态不允许业务状态迁移")

	row := st.roomAt(t, mrRoom)
	wantEQ(t, "BANNED 库存", "state", row.State, model.RoomStateBanned)
	wantEQ(t, "BANNED 库存", "state_version", row.StateVersion, int32(1))
	wantEQ(t, "BANNED 库存", "verify_state", row.VerifyState, model.VerifyStatePassed)
	// PASS 走的是 patch/回写里的「清空 reject_reason」，不是保留旧驳回。
	wantEQ(t, "BANNED 库存", "reject_reason 被清", row.RejectReason, "")
}

// TestModerationResultSameConclusionStillMovesRoom 钉缺口 G：
// 资料已经是 PASSED、房间还停在 PENDING 时，重复的 PASS 结论仍会推 PENDING->READY，
// 并写出一条 verify_state 3->3 的「幽灵审计」——审计表里出现了一次没发生的迁移。
func TestModerationResultSameConclusionStillMovesRoom(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStatePending, model.VerifyStatePassed)

	reply, err := newMRLogic(t, st).ApplyRoomModerationResult(mrReq(mrEvent))
	wantNoErr(t, "同结论仍推房间", err)
	wantEQ(t, "同结论", "applied", reply.GetApplied(), true)
	wantEQ(t, "同结论", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_READY)
	wantEQ(t, "同结论", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_PASSED)

	logs := st.logsOf(mrRoom)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2", len(logs))
	}
	wantEQ(t, "同结论", "房间态迁移", fmt.Sprintf("%d:%d->%d", logs[0].StateType, logs[0].FromState, logs[0].ToState),
		fmt.Sprintf("%d:%d->%d", model.LogTypeRoomState, model.RoomStatePending, model.RoomStateReady))
	// 这条就是「没发生的迁移」也被记了一次：from==to。
	wantEQ(t, "同结论（缺口 G）", "资料态幽灵审计",
		fmt.Sprintf("%d:%d->%d", logs[1].StateType, logs[1].FromState, logs[1].ToState),
		fmt.Sprintf("%d:%d->%d", model.LogTypeVerifyState, model.VerifyStatePassed, model.VerifyStatePassed))
}

// TestModerationResultNoOpConclusionWritesNothing 钉「无事可做」分支：
// REVIEW 落在 REVIEWING 上（结论与现状一致且没有合法房间迁移）时 applied=false，
// 不开事务、不改行、不写审计、**连回读都不做**，只回填结果快照。
func TestModerationResultNoOpConclusionWritesNothing(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStateReady, model.VerifyStateReviewing)

	req := mrReq(mrEvent)
	req.Verdict = rpc.ModerationVerdict_VERDICT_REVIEW
	req.Reason = ""
	reply, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
	wantNoErr(t, "REVIEW 无迁移", err)
	wantSeq(t, "REVIEW 轨迹", st.log, 0,
		fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
		fmt.Sprintf("live_room_idempotency.SaveResult:%s", mrEvent),
	)
	wantTxCount(t, "REVIEW 无迁移", st.conn, 0)
	wantEQ(t, "REVIEW 应答", "applied", reply.GetApplied(), false)
	wantEQ(t, "REVIEW 应答", "message", reply.GetMessage(), "结论与当前状态一致，未产生迁移")
	wantEQ(t, "REVIEW 应答", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_REVIEWING)
	wantEQ(t, "REVIEW 应答", "room_state", reply.GetRoomState(), rpc.RoomState_ROOM_STATE_READY)

	row := st.roomAt(t, mrRoom)
	wantEQ(t, "REVIEW 库存", "state", row.State, model.RoomStateReady)
	wantEQ(t, "REVIEW 库存", "mtime 不变", row.Mtime, 1000+mrRoom)
	wantEQ(t, "REVIEW 库存", "无审计", len(st.logsOf(mrRoom)), 0)
}

// TestModerationResultVerifyMatrixIsTheGate 钉资料侧矩阵是唯一闸门：
// 只有 Reviewing->Passed / Reviewing->Rejected 两条结论边可行，
// 从 PASSED/REJECTED/NONE 出发的结论一律非法（并在 Claim 之后才报错）。
func TestModerationResultVerifyMatrixIsTheGate(t *testing.T) {
	cases := []struct {
		name        string
		fromVerify  int32
		verdict     rpc.ModerationVerdict
		wantTarget  int32
		wantIllegal bool
	}{
		{"Reviewing+PASS", model.VerifyStateReviewing, rpc.ModerationVerdict_VERDICT_PASS, model.VerifyStatePassed, false},
		{"Reviewing+REJECT", model.VerifyStateReviewing, rpc.ModerationVerdict_VERDICT_REJECT, model.VerifyStateRejected, false},
		{"Reviewing+REVIEW 是同值（无迁移）", model.VerifyStateReviewing, rpc.ModerationVerdict_VERDICT_REVIEW, model.VerifyStateReviewing, false},
		{"PASSED+REJECT 被矩阵拒绝（巡检打回不可达）", model.VerifyStatePassed, rpc.ModerationVerdict_VERDICT_REJECT, 0, true},
		{"REJECTED+PASS 被矩阵拒绝（复审改判不可达）", model.VerifyStateRejected, rpc.ModerationVerdict_VERDICT_PASS, 0, true},
		{"NONE+PASS 被矩阵拒绝", model.VerifyStateNone, rpc.ModerationVerdict_VERDICT_PASS, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, mrNow)
			st := newStore()
			// 房间停在 READY：既不影响资料矩阵判定，也不触发业务迁移。
			seedMRScene(t, st, model.RoomStateReady, tc.fromVerify)

			req := mrReq(mrEvent)
			req.Verdict = tc.verdict
			reply, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
			if tc.wantIllegal {
				wantErrIs(t, tc.name, err, model.ErrInvalidVerifyTransition)
				wantKeyBurnedNoResult(t, tc.name, mrEvent, st)
				wantEQ(t, tc.name, "库存不动", st.roomAt(t, mrRoom).VerifyState, tc.fromVerify)
				wantEQ(t, tc.name, "无审计", len(st.logsOf(mrRoom)), 0)
				return
			}
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "落点", reply.GetVerifyState(), rpc.VerifyState(tc.wantTarget))
			wantEQ(t, tc.name, "库存落点", st.roomAt(t, mrRoom).VerifyState, tc.wantTarget)
		})
	}
}

// TestModerationResultDuplicateEventReturnsCurrentProjection 钉重复投递：
// 回的是**当前投影** + applied=false，而不是首次那份结果快照（两者可以完全不同），
// 且不产生任何写、不回填结果。
func TestModerationResultDuplicateEventReturnsCurrentProjection(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)

	lg := newMRLogic(t, st)
	first, err := lg.ApplyRoomModerationResult(mrReq(mrEvent))
	wantNoErr(t, "首次", err)
	wantEQ(t, "首次", "applied", first.GetApplied(), true)
	stored := st.idemAt(mrEvent).ResultJSON
	counts := st.counts()
	before := st.log.snapshot()

	// 处置之后又有别的入口把房间推进到 LIVING（真实投递顺序不保证）。
	st.rooms.rows[0].State = model.RoomStateLiving
	st.rooms.rows[0].StateVersion = 2

	second, err := lg.ApplyRoomModerationResult(mrReq(mrEvent))
	wantNoErr(t, "重投", err)
	wantSeq(t, "重投轨迹", st.log, before,
		fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
		fmt.Sprintf("live_room_idempotency.Find:%s", mrEvent),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
	)
	wantEQ(t, "重投", "applied", second.GetApplied(), false)
	wantEQ(t, "重投", "message", second.GetMessage(), "同一 event_id 重复投递，未产生迁移")
	// 关键判别：重投应答读自当前投影，而不是快照里的 READY。
	wantEQ(t, "重投", "room_state 是当前值", second.GetRoomState(), rpc.RoomState_ROOM_STATE_LIVING)
	wantEQ(t, "重投", "verify_state", second.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_PASSED)
	wantEQ(t, "重投", "库存没被重投改动", st.roomAt(t, mrRoom).State, model.RoomStateLiving)
	wantEQ(t, "重投", "库存形态不变", st.counts(), counts)
	// 重投不回填结果：首次的快照原样保留。
	wantEQ(t, "重投", "结果快照未被覆盖", st.idemAt(mrEvent).ResultJSON, stored)
	wantTxCount(t, "重投无事务", st.conn, 1)
}

// TestModerationResultEventKeyIsGlobalNotPerMethod 钉去重键的作用域：
// dedup_key 是全局唯一列，kind 又不在回放校验里，所以「同一个键被别的 RPC 用过」
// 只能靠 rpc 名拦；名字相同但 kind 不同（客户端 request_id 撞上事件 event_id）
// 会被当作本方法的重复投递放行——这是键空间没有按 kind 分域的直接后果。
func TestModerationResultEventKeyIsGlobalNotPerMethod(t *testing.T) {
	t.Run("键被 ReportStreamState 用过", func(t *testing.T) {
		fixClock(t, mrNow)
		st := newStore()
		seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)
		st.seedIdem(mrEvent, rpcReportStreamState, `{"room_id":4301}`)

		_, err := newMRLogic(t, st).ApplyRoomModerationResult(mrReq(mrEvent))
		wantErrIs(t, "键复用", err, model.ErrRequestIDReused)
		wantSeq(t, "键复用轨迹", st.log, 0,
			fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
			fmt.Sprintf("live_room_idempotency.Find:%s", mrEvent),
		)
		wantEQ(t, "键复用", "库存不动", st.roomAt(t, mrRoom).State, model.RoomStatePending)
	})

	t.Run("kind 不参与判定", func(t *testing.T) {
		fixClock(t, mrNow)
		st := newStore()
		seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)
		// 同一个 rpc 名，但登记成客户端 request 域。
		rec := st.seedIdem(mrEvent, rpcApplyRoomModerationResult,
			`{"room_id":4301,"verify_state":3,"room_state":2,"applied":true}`)
		rec.Kind = model.IdempotencyKindRequest

		_, err := newMRLogic(t, st).ApplyRoomModerationResult(mrReq(mrEvent))
		wantNoErr(t, "kind 不校验", err)
		wantEQ(t, "kind 不校验", "仍按重复投递处理", st.roomAt(t, mrRoom).State, model.RoomStatePending)
	})
}

// TestModerationResultReasonWidthBoundary 钉缺口 I 的另一侧：
// 250 字符整放行并原样落库，251 字符整条拒绝——所以 apply() 里的
// truncateRunes(reason, 250) 永远不会截断任何东西（死代码）。
func TestModerationResultReasonWidthBoundary(t *testing.T) {
	edge := strings.Repeat("违", maxReasonRunes) // 250 个字符，正好在列宽内
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStateReady, model.VerifyStateReviewing)

	req := mrReq(mrEvent)
	req.Verdict = rpc.ModerationVerdict_VERDICT_REJECT
	req.Reason = edge
	_, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
	wantNoErr(t, "reason 250 字符", err)
	// 原样落库，一个字符都没被截掉。
	wantEQ(t, "reason 250 字符", "reject_reason", st.roomAt(t, mrRoom).RejectReason, edge)
	if l := st.logsOf(mrRoom); len(l) != 2 {
		t.Fatalf("审计行数 = %d, want 2", len(l))
	} else {
		wantEQ(t, "reason 250 字符", "审计 reason 与库值一致（AGENTS.md §8）", l[0].Reason, edge)
	}

	fixClock(t, mrNow)
	st2 := newStore()
	seedMRScene(t, st2, model.RoomStateReady, model.VerifyStateReviewing)
	req2 := mrReq(mrEvent)
	req2.Verdict = rpc.ModerationVerdict_VERDICT_REJECT
	req2.Reason = edge + "多"
	_, err = newMRLogic(t, st2).ApplyRoomModerationResult(req2)
	wantErrIs(t, "reason 251 字符", err, model.ErrReasonTooLong)
	wantNoCallAfter(t, "reason 251 字符", st2.log, 0)
	wantEQ(t, "reason 251 字符", "库存不动", st2.roomAt(t, mrRoom).VerifyState, model.VerifyStateReviewing)
}

// TestModerationResultCasLossOnRoomTransition 钉并发窗口：
// 读到房间之后、事务内的 UPDATE 之前，别的入口推走了 state_version，
// CAS 不命中必须整笔事务失败（ErrConcurrentUpdate），不能只回写一半。
func TestModerationResultCasLossOnRoomTransition(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)
	st.raceBefore("live_room.TransitionTx:4301:1->2", func() {
		st.rooms.rows[0].StateVersion = 42
	})

	reply, err := newMRLogic(t, st).ApplyRoomModerationResult(mrReq(mrEvent))
	wantErrIs(t, "并发丢失", err, model.ErrConcurrentUpdate)
	if reply != nil {
		t.Errorf("并发失败仍返回 reply %+v", reply)
	}
	wantKeyBurnedNoResult(t, "并发丢失", mrEvent, st)
	wantEQ(t, "并发丢失", "资料状态未落", st.roomAt(t, mrRoom).VerifyState, model.VerifyStateReviewing)
	wantEQ(t, "并发丢失", "state_version 是插队者的", st.roomAt(t, mrRoom).StateVersion, int32(42))
	wantEQ(t, "并发丢失", "无审计", len(st.logsOf(mrRoom)), 0)
	st.checkRaces(t)
}

// TestModerationResultAuditInsertFailurePropagates 钉事务内第二笔写失败：
// 审计 INSERT 失败会把整笔事务带崩（对比 PrepareLive 的缺口 C：那里审计失败只记日志）。
// 假件不回滚，所以这里的「状态已改、审计没有」是对真实 MySQL 中不会出现的形态的
// 保守观测：本用例锁的是**错误必须上抛且键不回填**，不是锁残留形态。
func TestModerationResultAuditInsertFailurePropagates(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)
	st.stateLogs.failWith("InsertTx", errMRDown)

	_, err := newMRLogic(t, st).ApplyRoomModerationResult(mrReq(mrEvent))
	wantErrIs(t, "审计写失败", err, errMRDown)
	wantTxCount(t, "审计写失败", st.conn, 1)
	wantEQ(t, "审计写失败", "rolledBack", st.conn.rolledBack, 1)
	wantKeyBurnedNoResult(t, "审计写失败", mrEvent, st)
	// 第一条审计（房间态）就失败：第二条审计、提交后的回读与结果回填都不许发生。
	wantSeq(t, "审计写失败轨迹", st.log, 0,
		fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
		fmt.Sprintf("live_room.FindOne:%d", mrRoom),
		"db.TransactCtx",
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v1", mrRoom, model.RoomStatePending, model.RoomStateReady),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s", model.LogTypeRoomState, model.RoomStatePending, model.RoomStateReady, "机器审核通过"),
	)
}

// TestModerationResultReadFailuresPropagate 钉读依赖失败原样上抛：
// 既不降级成「未迁移」的成功应答，也不伪装成房间不存在。
func TestModerationResultReadFailuresPropagate(t *testing.T) {
	cases := []struct {
		name string
		at   string
		arm  func(st *store)
	}{
		{"房间读失败", fmt.Sprintf("live_room.FindOne:%d", mrRoom),
			func(st *store) { st.rooms.failWith("FindOne", errMRDown) }},
		{"Claim 自身失败", fmt.Sprintf("live_room_idempotency.Claim:%s", mrEvent),
			func(st *store) { st.idem.failWith("Claim", errMRDown) }},
		{"资料回写 SQL 失败", fmt.Sprintf("live_room.SetVerifyResultTx:%d:%d", mrRoom, model.VerifyStateRejected),
			func(st *store) { st.rooms.failWith("SetVerifyResultTx", errMRDown) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, mrNow)
			st := newStore()
			// LIVING + REVIEWING：只走资料回写那条不推业务状态的分支。
			seedMRScene(t, st, model.RoomStateLiving, model.VerifyStateReviewing)
			tc.arm(st)

			req := mrReq(mrEvent)
			req.Verdict = rpc.ModerationVerdict_VERDICT_REJECT
			reply, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
			wantErrIs(t, tc.name, err, errMRDown)
			if reply != nil {
				t.Errorf("%s 仍返回 reply %+v", tc.name, reply)
			}
			if errors.Is(err, model.ErrRoomNotFound) {
				t.Errorf("%s：DB 故障被降级成「房间不存在」：%v", tc.name, err)
			}
			wantLastOp(t, tc.name, st.log, tc.at)
			wantEQ(t, tc.name, "资料状态不动", st.roomAt(t, mrRoom).VerifyState, model.VerifyStateReviewing)
		})
	}
}

// TestModerationResultOperatorIsSelfReported 钉缺口 H：
// 本入口不校验 operator（对比 BanRoom 的 checkOperator：operator_mid<=0 直接拒）。
// 审计里的处理人完全由调用方填，机审填 0、伪造他人 mid 都无从分辨。
func TestModerationResultOperatorIsSelfReported(t *testing.T) {
	fixClock(t, mrNow)
	st := newStore()
	seedMRScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)

	req := mrReq(mrEvent)
	req.Operator = 999 // 自报的「处理人」，没有任何主体校验
	reply, err := newMRLogic(t, st).ApplyRoomModerationResult(req)
	wantNoErr(t, "operator 自报", err)
	wantEQ(t, "operator 自报", "applied", reply.GetApplied(), true)

	logs := st.logsOf(mrRoom)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2", len(logs))
	}
	wantEQ(t, "operator 自报", "两条审计都记 999", logs[0].OperatorMid, int64(999))
	wantEQ(t, "operator 自报", "资料审计同样", logs[1].OperatorMid, int64(999))
}
