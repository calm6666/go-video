// 本文件覆盖不变量 1（流状态机合法性 / 终态 / 乱序）与不变量 5 的
// 「事件与状态同事务、seq 单调、按 event_id 同源」部分。
//
// 为什么整组用例都从 ReportStreamState 这个真入口打，而不是直接调 applyStreamTransition：
// 引擎内部自洽是必要的但不充分——「拒绝时零副作用」只有在带着入参校验、幂等预检、
// 事务边界的完整路径上才可证伪。直接调私有函数会把「预检漏了」这类缺陷测不出来。

package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func reportReq(streamID, reportID string, state rpc.StreamState) *rpc.ReportStreamStateReq {
	return &rpc.ReportStreamStateReq{
		StreamId: streamID, State: state, ReportId: reportID, Reason: "用例推进",
	}
}

// seqFor 给出「处于某状态的流」的合理 seq：状态机断言里它必须与状态历史一致，
// 否则 CAS 的 seq 条件根本没被测到。
func seqFor(state int32) int64 {
	switch state {
	case model.StreamStateIdle:
		return 0
	case model.StreamStatePublishing:
		return 1
	case model.StreamStateInterrupted:
		return 2
	default:
		return 3
	}
}

// --- 不变量 1：合法迁移逐格推进 seq ---

func TestStreamStateMachine_LegalPathAdvancesSeqAndWritesEvent(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-LEGAL", RoomID: 7, Protocol: 1})

	before := e.effects()
	path := []rpc.StreamState{
		rpc.StreamState_STREAM_STATE_PUBLISHING,
		rpc.StreamState_STREAM_STATE_INTERRUPTED,
		rpc.StreamState_STREAM_STATE_PUBLISHING,
		rpc.StreamState_STREAM_STATE_STOPPED,
	}
	for i, target := range path {
		req := reportReq(s.StreamID, fmt.Sprintf("rep-legal-%d", i), target)
		reply, err := e.report(t, req)
		label := fmt.Sprintf("第 %d 步迁移到 %v", i+1, target)
		wantOK(t, reply, err, label)
		if !reply.GetApplied() {
			t.Fatalf("%s：期望 applied=true", label)
		}
		if reply.GetSeq() != int64(i+1) {
			t.Fatalf("%s：seq 应为 %d，实得 %d", label, i+1, reply.GetSeq())
		}
		row := e.streamRow(t, s.StreamID)
		if row.State != streamStateFromRPC(target) || row.Seq != int64(i+1) {
			t.Fatalf("%s：库内不一致 state=%d seq=%d", label, row.State, row.Seq)
		}
	}
	// 四步各写 1 事件 + 1 outbox；断流区间只在进 INTERRUPTED 时增一行（出来是闭合而非新增）
	wantDelta := [9]int{5: 4, 4: 4, 7: 1}
	if got := e.delta(before); got != wantDelta {
		t.Fatalf("整条合法路径的写入集合不符：\n  got =%v\n want=%v", got, wantDelta)
	}
}

// --- 不变量 1：迁移矩阵逐格穷举，非法格必须零副作用 ---

func TestStreamStateMachine_FullMatrixMatchesDeclaredLegality(t *testing.T) {
	targets := []rpc.StreamState{
		rpc.StreamState_STREAM_STATE_PUBLISHING,
		rpc.StreamState_STREAM_STATE_INTERRUPTED,
		rpc.StreamState_STREAM_STATE_STOPPED,
	}
	froms := []int32{model.StreamStateIdle, model.StreamStatePublishing, model.StreamStateInterrupted, model.StreamStateStopped}

	for _, from := range froms {
		for _, rpcTo := range targets {
			from, to := from, streamStateFromRPC(rpcTo)
			t.Run(fmt.Sprintf("%s_to_%s", streamStateName(from), streamStateName(to)), func(t *testing.T) {
				e := newTestEnv(t)
				s := e.seedStream(t, &model.Stream{
					StreamID: fmt.Sprintf("S-%d-%d", from, to), RoomID: 11, Protocol: 1,
					State: from, Seq: seqFor(from),
				})
				before := e.effects()
				reply, err := e.report(t, reportReq(s.StreamID, "rep-matrix", rpcTo))

				switch {
				case from == to:
					// 同态：幂等 no-op——不占 seq、不写事件、不发信封，但也不报错。
					wantOK(t, reply, err, "同态上报")
					if reply.GetApplied() {
						t.Fatalf("同态上报 %s→%s 不该 applied=true", streamStateName(from), streamStateName(to))
					}
					if reply.GetSeq() != seqFor(from) {
						t.Fatalf("同态上报占了新 seq：%d（当前应为 %d）", reply.GetSeq(), seqFor(from))
					}
					e.requireDelta(t, before, [9]int{}, "同态上报必须零写入")
				case model.CanTransitionStreamState(from, to):
					wantOK(t, reply, err, "合法迁移")
					if !reply.GetApplied() || reply.GetSeq() != seqFor(from)+1 {
						t.Fatalf("合法迁移 %s→%s 未推进：applied=%v seq=%d",
							streamStateName(from), streamStateName(to), reply.GetApplied(), reply.GetSeq())
					}
					// 事件 + outbox 同事务落地是这一格的全部应有副作用；进 INTERRUPTED 另开一行断流区间
					want := [9]int{4: 1, 5: 1}
					if to == model.StreamStateInterrupted {
						want[7] = 1
					}
					e.requireDelta(t, before, want, "合法迁移的写入集合")
				default:
					// 终态无出边是独立哨兵（运维要能区分「已停的流」与「跳错了状态」）
					if from == model.StreamStateStopped {
						wantFail(t, err, model.ErrTerminalStream, "终态出边")
					} else {
						wantFail(t, err, model.ErrInvalidStateTransition, "非法迁移")
					}
					if reply != nil {
						t.Fatalf("被拒的迁移回了非 nil 响应：%+v", reply)
					}
					e.requireSameEffects(t, before, "非法迁移必须零副作用（含回滚）")
					row := e.streamRow(t, s.StreamID)
					if row.State != from || row.Seq != seqFor(from) {
						t.Fatalf("非法迁移却改动了流：%s→%s state=%d seq=%d",
							streamStateName(from), streamStateName(to), row.State, row.Seq)
					}
				}
			})
		}
	}
}

// --- 不变量 1 + 2：幂等与乱序 ---

func TestStreamStateMachine_ReplayedReportIdWritesNothing(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-REPLAY", RoomID: 3, Protocol: 1})
	first, err := e.report(t, reportReq(s.StreamID, "rep-once", rpc.StreamState_STREAM_STATE_PUBLISHING))
	wantOK(t, first, err, "首次上报")

	// 第二次带同一 report_id、但目标状态不同：重放必须赢，STOPPED 被彻底忽略
	second, err := e.report(t, reportReq(s.StreamID, "rep-once", rpc.StreamState_STREAM_STATE_STOPPED))
	wantOK(t, second, err, "重放上报")
	mustTrue(t, second.GetReplayed(), "重放标记")
	if second.GetApplied() {
		t.Fatalf("重放上报不该再应用一次")
	}
	if second.GetEventId() != first.GetEventId() || second.GetSeq() != first.GetSeq() {
		t.Fatalf("重放未回放首次结果：event %s/%s seq %d/%d",
			first.GetEventId(), second.GetEventId(), first.GetSeq(), second.GetSeq())
	}
	if second.GetState() != first.GetState() {
		t.Fatalf("重放按第二次请求的目标状态回了：%v（应为首次 %v）", second.GetState(), first.GetState())
	}
	before := e.effects()
	e.requireSameEffects(t, before, "重放上报之后不得再有写入（本用例只回看，不写）")
	e.requireNoTransaction(t, before, "report_id 重放在预检阶段就该返回，不该开事务")
	if row := e.streamRow(t, s.StreamID); row.State != model.StreamStatePublishing {
		t.Fatalf("重放把流推进到了 STOPPED：state=%d", row.State)
	}
	if n := len(e.db.outbox); n != 1 {
		t.Fatalf("重放多发了事件信封：outbox=%d", n)
	}
}

func TestStreamStateMachine_SameStateTwiceEchoesFirstEventId(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-SAME", RoomID: 3, Protocol: 1})
	first, err := e.report(t, reportReq(s.StreamID, "rep-a", rpc.StreamState_STREAM_STATE_PUBLISHING))
	wantOK(t, first, err, "首次上报")

	before := e.effects()
	// 不同 report_id、同状态：是 no-op 而不是重放，且必须回查得到首次 event_id
	again, err := e.report(t, reportReq(s.StreamID, "rep-b", rpc.StreamState_STREAM_STATE_PUBLISHING))
	wantOK(t, again, err, "同态上报")
	if again.GetApplied() || again.GetReplayed() {
		t.Fatalf("同态上报 applied/replayed 都该是 false：applied=%v replayed=%v",
			again.GetApplied(), again.GetReplayed())
	}
	if again.GetEventId() != first.GetEventId() {
		t.Fatalf("同态上报未回显首次 event_id：%s vs %s", again.GetEventId(), first.GetEventId())
	}
	e.requireDelta(t, before, [9]int{}, "同态上报不得留下第二份事件")
}

func TestStreamStateMachine_ExpectSeqMismatchIsRejectedWithoutWrite(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-SEQ", RoomID: 4, Protocol: 1})

	before := e.effects()
	in := reportReq(s.StreamID, "r", rpc.StreamState_STREAM_STATE_PUBLISHING)
	in.ExpectSeq = 99
	_, err := e.report(t, in)
	wantFail(t, err, model.ErrSeqConflict, "expect_seq 不符")
	e.requireSameEffects(t, before, "expect_seq 不符必须零副作用")
}

func TestStreamStateMachine_StaleTimestampIsRejected(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{
		StreamID: "S-STALE", RoomID: 5, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1, StateChangedAt: nowUnix(),
	})

	before := e.effects()
	in := reportReq(s.StreamID, "r", rpc.StreamState_STREAM_STATE_INTERRUPTED)
	in.OccurredAt = nowUnix() - 600
	_, err := e.report(t, in)
	wantFail(t, err, model.ErrSeqConflict, "滞后上报")
	e.requireSameEffects(t, before, "滞后上报必须零副作用")
	e.requireNoTransaction(t, before, "乱序判定在预检阶段就该完成，不该白开一次事务")
}

func TestStreamStateMachine_FutureTimestampBeyondSkewIsRejected(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-FUTURE", RoomID: 6, Protocol: 1})
	before := e.effects()

	in := reportReq(s.StreamID, "r", rpc.StreamState_STREAM_STATE_PUBLISHING)
	in.OccurredAt = nowUnix() + e.svc.Config.LiveIngest.CallbackSkewSeconds + 3600
	_, err := e.report(t, in)
	wantFail(t, err, model.ErrCallbackTimestampSkew, "超前时间戳")
	e.requireSameEffects(t, before, "超前时间戳必须零副作用")
	e.requireNoTransaction(t, before, "时间戳越界须在开事务前就被拒掉")
}

func TestStreamStateMachine_IdleTargetIsNotReportable(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-IDLE", RoomID: 8, Protocol: 1})
	before := e.effects()

	_, err := e.report(t, reportReq(s.StreamID, "r", rpc.StreamState_STREAM_STATE_IDLE))
	wantFail(t, err, model.ErrInvalidStreamState, "IDLE 不可上报")
	e.requireSameEffects(t, before, "IDLE 上报必须零副作用")
	e.requireNoTransaction(t, before, "目标状态不合法时不该走到事务")
}

func TestStreamStateMachine_UnknownStreamIsNotFound(t *testing.T) {
	e := newTestEnv(t)
	before := e.effects()
	_, err := e.report(t, reportReq("S-NOPE", "r", rpc.StreamState_STREAM_STATE_PUBLISHING))
	wantFail(t, err, model.ErrStreamNotFound, "未知流")
	e.requireSameEffects(t, before, "未知流必须零副作用")
	e.requireNoTransaction(t, before, "流不存在时不该开事务（预读已判定）")
}

// --- 不变量 1 + 5：断流区间开合与 seq 单调 ---

func TestStreamStateMachine_InterruptionWindowOpensAndCloses(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-INT", RoomID: 9, Protocol: 1})
	if _, err := e.report(t, reportReq(s.StreamID, "i0", rpc.StreamState_STREAM_STATE_PUBLISHING)); err != nil {
		t.Fatalf("进入 PUBLISHING：%v", err)
	}
	down, err := e.report(t, reportReq(s.StreamID, "i1", rpc.StreamState_STREAM_STATE_INTERRUPTED))
	wantOK(t, down, err, "进入 INTERRUPTED")

	open, err := e.repo.StreamInterruption.FindOpenByStream(bg(), s.StreamID)
	wantOK(t, open, err, "查询断流区间")
	if open == nil {
		t.Fatalf("断流区间未开启")
	}
	if open.EpisodeNo != 1 || open.InterruptionID != down.GetInterruptionId() {
		t.Fatalf("断流区间不符：episode=%d id=%d（reply=%d）", open.EpisodeNo, open.InterruptionID, down.GetInterruptionId())
	}
	if row := e.streamRow(t, s.StreamID); row.InterruptionCount != 1 {
		t.Fatalf("流上累计断流次数未记：%d", row.InterruptionCount)
	}

	up, err := e.report(t, reportReq(s.StreamID, "i2", rpc.StreamState_STREAM_STATE_PUBLISHING))
	wantOK(t, up, err, "重连回 PUBLISHING")
	again, err := e.repo.StreamInterruption.FindOpenByStream(bg(), s.StreamID)
	wantOK(t, again, err, "查询")
	if again != nil {
		t.Fatalf("重连后断流区间未闭合")
	}
	if up.GetInterruptionId() != open.InterruptionID {
		t.Fatalf("重连回显了新的 interruption_id：%d vs %d", up.GetInterruptionId(), open.InterruptionID)
	}
	closed := e.db.interrupt[open.InterruptionID]
	if closed.EndedAt == 0 || closed.EndReason != model.InterruptionEndReconnected {
		t.Fatalf("断流记录未按「重连」结束：ended_at=%d end_reason=%d", closed.EndedAt, closed.EndReason)
	}
	if row := e.streamRow(t, s.StreamID); row.InterruptedTotalSecs != closed.DurationSeconds {
		t.Fatalf("累计中断秒数与断流记录不符：%d vs %d", row.InterruptedTotalSecs, closed.DurationSeconds)
	}

	// 第二次断流：episode_no 必须递增，且以「主动停流」结束
	if _, err := e.report(t, reportReq(s.StreamID, "i3", rpc.StreamState_STREAM_STATE_INTERRUPTED)); err != nil {
		t.Fatalf("第二次断流：%v", err)
	}
	second, err := e.repo.StreamInterruption.FindOpenByStream(bg(), s.StreamID)
	wantOK(t, second, err, "查询第二段断流")
	if second == nil || second.EpisodeNo != 2 {
		t.Fatalf("episode_no 未递增：%+v", second)
	}
	if _, err := e.report(t, reportReq(s.StreamID, "i4", rpc.StreamState_STREAM_STATE_STOPPED)); err != nil {
		t.Fatalf("停流：%v", err)
	}
	if got := e.db.interrupt[second.InterruptionID].EndReason; got != model.InterruptionEndClosed {
		t.Fatalf("停流时断流区间结束原因应为 CLOSED，实得 %d", got)
	}

	// seq 单调：五步迁移必须留下严格递增的 1..5
	events, err := e.repo.StreamEvent.ListAfterSeq(bg(), s.StreamID, 0, 100, false)
	wantOK(t, events, err, "ListAfterSeq")
	for i, want := range []int64{1, 2, 3, 4, 5} {
		if i >= len(events) || events[i].Seq != want {
			t.Fatalf("事件 seq 不单调：%v", eventSeqs(events))
		}
	}
}

func eventSeqs(rows []*model.StreamEvent) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Seq)
	}
	return out
}

func TestStreamStateMachine_StoppedReleasesKeyLeaseAndQuota(t *testing.T) {
	e := newTestEnv(t)
	key := e.seedKey(t, &model.StreamKey{
		StreamName: "live_21_a", RoomID: 21, AnchorMid: 31, Version: 1,
		ProtocolMask: model.ProtocolMaskRtmp,
	})
	e.seedAssignedNode(t,
		&model.Stream{StreamID: "S-STOP", RoomID: 21, KeyID: key.KeyID, Protocol: 1,
			State: model.StreamStatePublishing, Seq: 1, NodeID: "n1"},
		&model.IngestNode{NodeID: "n1", ProtocolMask: model.ProtocolMaskRtmp, CapacityStreams: 3, HealthScore: 90},
		"assign-1")
	if got := e.nodeRow(t, "n1").ActiveStreams; got != 1 {
		t.Fatalf("种子后节点占用应为 1，实得 %d", got)
	}
	before := e.effects()

	_, err := e.report(t, reportReq("S-STOP", "stop-1", rpc.StreamState_STREAM_STATE_STOPPED))
	wantOK(t, (*rpc.ReportStreamStateReply)(nil), err, "停流上报") // 只要不报错，返回值下面按库断言

	row := e.streamRow(t, "S-STOP")
	if row.State != model.StreamStateStopped {
		t.Fatalf("流未进入终态：%d", row.State)
	}
	if row.HealthState != model.HealthStateNoData {
		t.Fatalf("停流后健康位应归位 NO_DATA，实得 %d", row.HealthState)
	}
	if row.StopReason != model.StopReasonAnchorStop {
		t.Fatalf("入口上报的 unpublish 应记 ANCHOR_STOP，实得 %d", row.StopReason)
	}
	if got := e.nodeRow(t, "n1").ActiveStreams; got != 0 {
		t.Fatalf("节点配额未归还：active=%d", got)
	}
	lease, err := e.repo.NodeAssignment.FindActiveByStream(bg(), nil, "S-STOP")
	wantOK(t, lease, err, "查询租约")
	if lease != nil {
		t.Fatalf("停流后仍有生效租约：assignment=%d state=%d", lease.AssignmentID, lease.State)
	}
	if got := e.keyRow(t, key.KeyID).CurrentStreamID; got != "" {
		t.Fatalf("停流后密钥活跃指针未释放：%s", got)
	}
	if got := e.delta(before); got[4] != 1 || got[5] != 1 {
		t.Fatalf("停流的应有写入是「1 事件 + 1 outbox」，实得 %v", got)
	}
}

// 缺陷 #1 已修复（2026-10-04）：停流事务现在与 ReleaseIngestNode 走同一条
// `SetNode(streamID, "", s.NodeID)` CAS 清掉流行上的节点指针。
//
// 事实：applyStreamTransition 的 STOPPED 收尾做了 MarkStopped（健康归位）、
// ReleaseActiveStream（密钥活跃指针）、NodeAssignment.Release + ReleaseQuota（租约与配额），
// 并在 transitionResult 里把 nodeID 置空回给调用方；本用例钉住第四件：live_stream.node_id 也必须清。
// 修复前库里留下「终态流仍住在已归还配额的节点上」，按节点筛流与运营面板都会算错在线占用。
func TestStreamStateMachine_StoppedClearsNodePointer(t *testing.T) {
	e := newTestEnv(t)
	e.seedAssignedNode(t,
		&model.Stream{StreamID: "S-STALE-PTR", RoomID: 22, Protocol: 1,
			State: model.StreamStatePublishing, Seq: 1, NodeID: "n1"},
		&model.IngestNode{NodeID: "n1", ProtocolMask: model.ProtocolMaskRtmp, CapacityStreams: 3, HealthScore: 90},
		"assign-stale")

	e.mustReport(t, reportReq("S-STALE-PTR", "stop-stale", rpc.StreamState_STREAM_STATE_STOPPED))
	if row := e.streamRow(t, "S-STALE-PTR"); row.NodeID != "" {
		t.Fatalf("停流后流上仍挂着节点指针：%s", row.NodeID)
	}
}

// --- 不变量 5：事件与状态同事务（原子性可被证伪） ---

// failingOutbox 让 Outbox 写失败：真库里这一半失败必须把状态迁移一起带回滚，
// 否则 live-room 的投影会永久落后。
type failingOutbox struct {
	*fakeOutbox
}

func (f *failingOutbox) Insert(context.Context, sqlx.Session, *model.EventOutbox) error {
	return errors.New("kafka 不可用")
}

func TestStreamStateMachine_RollbackLeavesNoHalfAppliedTransition(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-ROLL", RoomID: 31, Protocol: 1})
	e.repo.Outbox = &failingOutbox{fakeOutbox: &fakeOutbox{db: e.db}}

	before := e.effects()
	if _, err := e.report(t, reportReq(s.StreamID, "roll-1", rpc.StreamState_STREAM_STATE_PUBLISHING)); err == nil {
		t.Fatalf("Outbox 写失败时上报必须整体失败")
	}
	// 整库回滚：状态、事件都不得留下「半截事实」
	e.requireSameEffects(t, before, "Outbox 写失败必须回滚状态与事件")
	row := e.streamRow(t, s.StreamID)
	if row.State != model.StreamStateIdle || row.Seq != 0 {
		t.Fatalf("回滚不完整：state=%d seq=%d", row.State, row.Seq)
	}
}

func TestStreamStateMachine_EventAndOutboxShareOneEventId(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-EVT", RoomID: 32, AnchorMid: 1002, SessionID: 77, Protocol: 1})
	reply, err := e.report(t, reportReq(s.StreamID, "evt-1", rpc.StreamState_STREAM_STATE_PUBLISHING))
	wantOK(t, reply, err, "上报")

	events, err := e.repo.StreamEvent.ListAfterSeq(bg(), s.StreamID, 0, 10, false)
	wantOK(t, events, err, "查询事件")
	if len(events) != 1 || events[0].EventID != reply.GetEventId() {
		t.Fatalf("事件行与响应不同源：%d 行", len(events))
	}
	if events[0].FromState != model.StreamStateIdle || events[0].ToState != model.StreamStatePublishing {
		t.Fatalf("事件记录的迁移方向不符：%d→%d", events[0].FromState, events[0].ToState)
	}
	if events[0].Source != model.EventSourceEntry {
		t.Fatalf("入口上报的事件来源应为 entry，实得 %q", events[0].Source)
	}

	out, err := e.repo.Outbox.ListByState(bg(), model.OutboxStatePending, 10)
	wantOK(t, out, err, "查询 outbox")
	if len(out) != 1 {
		t.Fatalf("应有 1 行待发布 outbox，实得 %d", len(out))
	}
	if out[0].EventID != events[0].EventID {
		t.Fatalf("outbox 与事件表 event_id 不同源：%s vs %s", out[0].EventID, events[0].EventID)
	}
	if out[0].Seq != events[0].Seq || out[0].StreamID != s.StreamID {
		t.Fatalf("outbox 的 seq/stream 与事件表不符")
	}

	// 信封本身：字段必须是消费方可直接路由的口径
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out[0].Payload), &env); err != nil {
		t.Fatalf("outbox.payload 不是合法 JSON：%v", err)
	}
	for key, want := range map[string]string{
		"event_id": reply.GetEventId(), "event_type": model.EventTypeStreamState,
		"producer": "live-ingest", "aggregate_type": model.AggregateTypeStream, "aggregate_id": s.StreamID,
	} {
		if got := strings.Trim(string(env[key]), `"`); got != want {
			t.Fatalf("信封字段 %s 不符：got=%q want=%q", key, got, want)
		}
	}
	var payload stateEventPayload
	if err := json.Unmarshal(env["payload"], &payload); err != nil {
		t.Fatalf("信封 payload 解析失败：%v", err)
	}
	if payload.StreamState != model.StreamStatePublishing || payload.StreamSeq != 1 || payload.RoomID != 32 {
		t.Fatalf("payload 状态口径不符：%+v", payload)
	}
	// anchor_mid 必须随事件带出：inbox 只认事件里的收件人，拿不到它就只能跳过，
	// 「live.state.v1 已接线」会变成假结论。字段名也一起钉，因为它就是线上契约。
	if payload.AnchorMid != 1002 {
		t.Fatalf("payload.anchor_mid=%d，期望带上主播 ID 1002：%+v", payload.AnchorMid, payload)
	}
	if !strings.Contains(out[0].Payload, `"anchor_mid":1002`) {
		t.Fatalf("信封里没有 anchor_mid 的线上字段名：%s", out[0].Payload)
	}
	if strings.Contains(out[0].Payload, "key_hash") || strings.Contains(out[0].Payload, "plaintext") {
		t.Fatalf("事件信封里出现了密钥字段：%s", out[0].Payload)
	}
}
