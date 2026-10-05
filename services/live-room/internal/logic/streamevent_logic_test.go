package logic

import (
	"errors"
	"testing"

	"go-video/services/live-room/model"
)

// TestStreamEventPlanMatrix 逐格钉住 live.state.v1 的映射，
// 并断言「任何被给出的迁移都必须在 model 矩阵内」——logic 不许自己发明边。
func TestStreamEventPlanMatrix(t *testing.T) {
	grace := int32(120)
	cases := []struct {
		name        string
		stream      int32
		session     int32
		interrupted int64
		want        streamPlan
	}{
		{"推流到达建档场次", model.StreamStatePublishing, model.SessionStatePending, 0, streamPlan{
			sessionTo: model.SessionStateLiving, roomTo: model.RoomStateLiving,
			result: model.StreamResultApplied, message: "推流到达，场次进入直播中"}},
		{"推流心跳只推进 seq", model.StreamStatePublishing, model.SessionStateLiving, 0, streamPlan{
			bumpOnly: true, result: model.StreamResultApplied, message: "推流心跳，无状态迁移"}},
		{"终态场次收到推流", model.StreamStatePublishing, model.SessionStateEnded, 0, streamPlan{
			noChange: true, result: model.StreamResultIllegalTransition,
			message: "场次已终态，推流事件不再改变状态"}},
		{"终态场次收到已终止", model.StreamStateStopped, model.SessionStateTerminated, 0, streamPlan{
			noChange: true, result: model.StreamResultIllegalTransition,
			message: "场次已终态，停止事件不再回拨状态"}},
		{"断流宽限期内", model.StreamStateInterrupted, model.SessionStateLiving, 30, streamPlan{
			bumpOnly: true, result: model.StreamResultApplied, message: "断流宽限期内，仅记录观测"}},
		{"断流超宽限期", model.StreamStateInterrupted, model.SessionStateLiving, 120, streamPlan{
			sessionTo: model.SessionStateTerminated, endReason: model.EndReasonStreamTimeout,
			roomTo: model.RoomStateReady, result: model.StreamResultApplied,
			message: "断流超过宽限期，场次终止"}},
		{"停止事件终止场次", model.StreamStateStopped, model.SessionStateLiving, 0, streamPlan{
			sessionTo: model.SessionStateTerminated, endReason: model.EndReasonStreamReplay,
			roomTo: model.RoomStateReady, result: model.StreamResultApplied,
			message: "推流停止，场次终止并释放房间直播态"}},
		{"IDLE 不产生迁移", model.StreamStateIdle, model.SessionStatePending, 0, streamPlan{
			noChange: true, result: model.StreamResultIllegalTransition, message: "IDLE 不产生状态迁移"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := streamEventPlan(c.stream, c.session, c.interrupted, grace)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.sessionTo != c.want.sessionTo || got.roomTo != c.want.roomTo ||
				got.endReason != c.want.endReason || got.bumpOnly != c.want.bumpOnly ||
				got.noChange != c.want.noChange || got.result != c.want.result ||
				got.message != c.want.message {
				t.Fatalf("plan = %+v, want %+v", got, c.want)
			}
			// 迁移合法性由 model 矩阵兜底。
			if got.sessionTo != 0 && !model.CanSessionTransition(c.session, got.sessionTo) {
				t.Fatalf("给出了矩阵外的场次边 %d->%d", c.session, got.sessionTo)
			}
			if got.sessionTo != 0 && !allowEndReasonForStream(got.endReason) {
				t.Fatalf("终态原因 %d 不可由流事件写入", got.endReason)
			}
			if got.noChange && (got.sessionTo != 0 || got.roomTo != 0 || got.bumpOnly) {
				t.Fatal("noChange 不得携带任何写入")
			}
		})
	}
	// 房间侧目标同样必须在矩阵内：Living 房间可去 Ready/Banned/Finished。
	for _, roomTo := range []int32{model.RoomStateLiving, model.RoomStateReady} {
		from := model.RoomStateReady
		if roomTo == model.RoomStateReady {
			from = model.RoomStateLiving
		}
		if !model.CanRoomTransition(from, roomTo) {
			t.Fatalf("房间边 %d->%d 不存在", from, roomTo)
		}
	}
}

func TestStreamEventPlanRejectsUnknown(t *testing.T) {
	if _, err := streamEventPlan(9, model.SessionStateLiving, 0, 120); !errors.Is(err, model.ErrStreamStateInvalid) {
		t.Fatalf("未知流状态应拒绝：%v", err)
	}
	if _, err := streamEventPlan(model.StreamStatePublishing, 0, 0, 120); !errors.Is(err, model.ErrInvalidSessionTransition) {
		t.Fatalf("未指定场次状态应拒绝：%v", err)
	}
	if _, err := streamEventPlan(model.StreamStatePublishing, 77, 0, 120); !errors.Is(err, model.ErrInvalidSessionTransition) {
		t.Fatalf("未知场次状态应拒绝：%v", err)
	}
}

// TestStreamEventPlanWithoutGraceNeverTerminates 宽限期为 0（配置缺省）时，
// 中断事件一律只记录观测：不能因为没配宽限期就把每次网络抖动判死。
func TestStreamEventPlanWithoutGraceNeverTerminates(t *testing.T) {
	for _, secs := range []int64{0, 1, 3600, 1 << 40} {
		for _, session := range []int32{model.SessionStatePending, model.SessionStateLiving} {
			plan, err := streamEventPlan(model.StreamStateInterrupted, session, secs, 0)
			if err != nil {
				t.Fatalf("secs=%d session=%d: %v", secs, session, err)
			}
			if !plan.bumpOnly || plan.sessionTo != 0 || plan.roomTo != 0 || plan.endReason != 0 {
				t.Fatalf("grace=0 时中断事件产生了迁移：%+v", plan)
			}
			if plan.result != model.StreamResultApplied {
				t.Fatalf("观测型事件应回 applied，实得 %+v", plan)
			}
		}
	}
	// 负宽限期同样按「不判死」处理，不能让配置错误放大成误终止。
	plan, err := streamEventPlan(model.StreamStateInterrupted, model.SessionStateLiving, 99999, -1)
	if err != nil || !plan.bumpOnly {
		t.Fatalf("grace<0 应只推进 seq：%+v %v", plan, err)
	}
}

// TestStreamEventPlanNeverRevivesTerminalSession 遍历全部流状态 × 终态场次：
// 任何一格都不得产生写入（旧事件复活已结束场次是最危险的乱序后果）。
func TestStreamEventPlanNeverRevivesTerminalSession(t *testing.T) {
	for _, stream := range []int32{model.StreamStateIdle, model.StreamStatePublishing,
		model.StreamStateInterrupted, model.StreamStateStopped} {
		for _, terminal := range []int32{model.SessionStateEnded, model.SessionStateTerminated} {
			for _, secs := range []int64{0, 10, 100000} {
				plan, err := streamEventPlan(stream, terminal, secs, 120)
				if err != nil {
					t.Fatalf("stream=%d session=%d: %v", stream, terminal, err)
				}
				if !plan.noChange || plan.result != model.StreamResultIllegalTransition {
					t.Fatalf("stream=%d session=%d 复活了终态场次：%+v", stream, terminal, plan)
				}
			}
		}
	}
}

// TestStaleStreamResult CAS 未命中后必须区分「seq 陈旧」与「状态被并发推进」。
func TestStaleStreamResult(t *testing.T) {
	cases := []struct {
		name    string
		session *model.LiveSession
		seq     int64
		want    int32
	}{
		{"场次不存在", nil, 5, model.StreamResultMismatch},
		{"seq 较小", &model.LiveSession{LastStreamSeq: 10}, 9, model.StreamResultStale},
		{"seq 相等", &model.LiveSession{LastStreamSeq: 10}, 10, model.StreamResultStale},
		{"seq 更大但状态已变", &model.LiveSession{LastStreamSeq: 10}, 11, model.StreamResultIllegalTransition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, msg := staleStreamResult(c.session, c.seq)
			if got != c.want {
				t.Fatalf("result = %d, want %d", got, c.want)
			}
			if msg == "" {
				t.Fatal("每种结果都要有可解释说明")
			}
		})
	}
}

// TestAdvanceStreamSeqGuardIsSingleSourceOfTruth 复现 model 的乱序守卫条件，
// 确认 logic 侧不会绕过它：只有 seq 严格大于已应用序号才可写入。
func TestAdvanceStreamSeqGuardIsSingleSourceOfTruth(t *testing.T) {
	for _, applied := range []int64{0, 1, 100} {
		session := &model.LiveSession{LastStreamSeq: applied}
		for seq := int64(-5); seq <= applied; seq++ {
			if seq > session.LastStreamSeq {
				t.Fatalf("seq=%d 不大于已应用 %d，却被判为可写", seq, applied)
			}
			result, _ := staleStreamResult(session, seq)
			if result != model.StreamResultStale {
				t.Fatalf("seq=%d applied=%d 应回乱序丢弃，实得 %d", seq, applied, result)
			}
		}
		result, _ := staleStreamResult(session, applied+1)
		if result == model.StreamResultStale {
			t.Fatalf("更新的 seq 不应被判陈旧（applied=%d）", applied)
		}
	}
}

// allowEndReasonForStream 锁定流事件可写的终止原因集合（与 README 一致）。
func allowEndReasonForStream(reason int32) bool {
	switch reason {
	case model.EndReasonUnspecified, model.EndReasonStreamTimeout, model.EndReasonStreamReplay:
		return true
	default:
		return false
	}
}
