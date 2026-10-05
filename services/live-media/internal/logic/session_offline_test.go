package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"
)

// OfflineSessionOutputs：live.state.v1 断流事件驱动的「本场次在线档位整场下线」。
//
// 这组用例要钉住的是这条链路与运营手工下线（OfflineStreamOutput）的三个差别：
//  1. 一次调用覆盖**多个**档位，且每个档位各登记一条事件（下游按 output_id 收敛投影）；
//  2. 所有档位的「条件下线 + 事件」在同一个事务里：一半下线一半失败必须整体回滚，
//     否则观众侧会出现「源已断但档位仍可播」的中间态，比整场不动更难查；
//  3. 幂等不靠事件表，靠 MarkOfflineTx 的 state=在线 CAS：
//     同一事件重投第二次扫不到行，Affected=0 是成功结论（消费者据此提交位点）。
//
// 为什么不能用 OfflineStreamOutput 替代：事件只给 room_id + session_id（没有档位维度），
// 按自然键定位会命中多个在线档位并报 ErrStreamOutputAmbiguous，逐档位各开一个事务又会破坏第 2 条。
//
// 档位顺序一律按真 SQL 的 ORDER BY bitrate_level ASC, protocol ASC 断言
// （rpc 枚举里 HD=3、SD=4，所以 HD 先于 SD）：顺序就是事件提交顺序，写反了说明 fake 与生产不同式。

// sessionOfflineInput 一份合法的整场下线入参（用例按需在副本上改字段）。
func sessionOfflineInput(eventID string) OfflineSessionOutputsInput {
	return OfflineSessionOutputsInput{
		RoomID: testRoomID, SessionID: testSession,
		Reason: model.ReasonSourceLost, EventID: eventID, TraceID: "trace-session-offline",
	}
}

// 黄金路径：本场次两个在线档位整场下线，别场/别房/已下线的行一律不碰。
func TestOfflineSessionOutputsOfflinesEveryOnlineLevelInSession(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)

	hd := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_HD)
		o.Protocol = int32(rpc.StreamProtocol_STREAM_PROTOCOL_HLS)
		o.TaskId = 9001
	})
	sd := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_SD)
		o.Protocol = int32(rpc.StreamProtocol_STREAM_PROTOCOL_HTTP_FLV)
		o.TaskId = 9002
	})
	// 已下线的历史行：必须落在另一个场次，否则与 sd 撞 uniq_output_natural。
	offlineBefore := seedOutput(db, model.StreamOutputStateOffline, func(o *model.LiveStreamOutput) {
		o.LiveSession = testSession2
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_SD)
		o.Protocol = int32(rpc.StreamProtocol_STREAM_PROTOCOL_HTTP_FLV)
	})
	offlineSnapshot := *offlineBefore
	// 另一场次仍在推流的档位：迟到的上一场 Stopped 不得把它摘掉（场次维度是唯一护栏）。
	otherSession := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.LiveSession = testSession2
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_SD)
		o.Protocol = int32(rpc.StreamProtocol_STREAM_PROTOCOL_HLS)
	})
	// 场次对但房间不同属脏数据：WHERE 带 room_id，必须当扫不到而不是照下线。
	otherRoom := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.RoomId = testRoomOther
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_UHD)
	})

	in := sessionOfflineInput("evt-live-state-stop-1")
	res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).OfflineSessionOutputs(in)
	res = wantOK(t, res, err, "整场下线")

	wantField(t, "整场下线", "Affected", res.Affected, int32(2))
	wantField(t, "整场下线", "Scanned", res.Scanned, int32(2))
	if len(res.OutputIDs) != 2 || res.OutputIDs[0] != hd.OutputId || res.OutputIDs[1] != sd.OutputId {
		t.Fatalf("OutputIDs 必须按 (bitrate_level, protocol) 升序且只含本次真正下线的行，got=%v want=[%d %d]",
			res.OutputIDs, hd.OutputId, sd.OutputId)
	}
	if res.Noop() {
		t.Fatalf("下线了 2 个档位的结论不能是 Noop")
	}

	for _, id := range []int64{hd.OutputId, sd.OutputId} {
		row := mustOutput(t, db, id)
		wantField(t, "整场下线", "state", row.State, model.StreamOutputStateOffline)
		wantField(t, "整场下线", "offline_reason", row.OfflineReason, model.ReasonSourceLost)
		wantField(t, "整场下线", "trace_id", row.TraceId, "trace-session-offline")
		if row.OfflineAt <= 0 {
			t.Fatalf("offline_at 必须由服务端取时钟写入，output_id=%d 实际 %d", id, row.OfflineAt)
		}
		// 下线不是删除：产物引用要留着（回收任务与排障靠它）。
		if row.ObjectKey == "" || row.Bucket == "" {
			t.Fatalf("下线后产物引用被清空：output_id=%d", id)
		}
	}
	// 三个「不该动」的行逐条钉住：只断言「本房间都下线了」抓不到跨场次误伤。
	assertOutputRowUnchanged(t, db, offlineBefore.OutputId, offlineSnapshot)
	wantField(t, "整场下线", "另一场次在线档位不被摘",
		mustOutput(t, db, otherSession.OutputId).State, model.StreamOutputStateOnline)
	wantField(t, "整场下线", "跨房间脏数据行不被摘",
		mustOutput(t, db, otherRoom.OutputId).State, model.StreamOutputStateOnline)

	wantEvents(t, db, []string{model.EventTypeStreamOutputOffline, model.EventTypeStreamOutputOffline}, "整场下线")
	for idx, want := range []*model.LiveStreamOutput{hd, sd} {
		ev := eventAt(t, db, idx)
		p := eventPayload(t, ev)
		wantField(t, "下线事件", "aggregate_type", ev.AggregateType, model.AggregateStreamOutput)
		wantField(t, "下线事件", "aggregate_id", ev.AggregateId, fmt.Sprint(want.OutputId))
		wantField(t, "下线事件", "room_id", ev.RoomId, want.RoomId)
		wantField(t, "下线事件", "trace_id", ev.TraceId, "trace-session-offline")
		wantField(t, "下线事件", "payload.output_id", toInt64(t, p, "output_id"), want.OutputId)
		wantField(t, "下线事件", "payload.room_id", toInt64(t, p, "room_id"), testRoomID)
		wantField(t, "下线事件", "payload.live_session_id", toInt64(t, p, "live_session_id"), testSession)
		wantField(t, "下线事件", "payload.bitrate_level", toInt64(t, p, "bitrate_level"), int64(want.BitrateLevel))
		wantField(t, "下线事件", "payload.protocol", toInt64(t, p, "protocol"), int64(want.Protocol))
		wantField(t, "下线事件", "payload.task_id", toInt64(t, p, "task_id"), want.TaskId)
		wantField(t, "下线事件", "payload.prev_state", toInt64(t, p, "prev_state"),
			int64(model.StreamOutputStateOnline))
		wantField(t, "下线事件", "payload.state", toInt64(t, p, "state"), int64(model.StreamOutputStateOffline))
		wantField(t, "下线事件", "payload.offline_reason", toInt64(t, p, "offline_reason"),
			int64(model.ReasonSourceLost))
		// request_id 前缀 evt: 是「自动下线 vs 人工下线」的判据，运营据此决定要不要重开档位。
		wantField(t, "下线事件", "payload.request_id", p["request_id"], "evt:evt-live-state-stop-1")
		wantField(t, "下线事件", "payload.source_event_id", p["source_event_id"], "evt-live-state-stop-1")
		wantField(t, "下线事件", "payload.source_event_type", p["source_event_type"], "live.state")
		// offline_at 不进 payload：行本身才是事实源（与 OfflineStreamOutput 同口径）。
		if _, ok := p["offline_at"]; ok {
			t.Fatalf("下线事件 payload 不应带 offline_at：%v", p)
		}
		// 产物引用与签名地址绝不进事件（AGENTS.md §6）。
		if _, ok := p["object_key"]; ok {
			t.Fatalf("下线事件 payload 不应带 object_key：%v", p)
		}
	}
	// 只读一次档位表：整场下线不能退化成逐档位各查一遍（N 次读会放大故障面）。
	wantCalls(t, db, "StreamOutputs.ListOnlineBySession", 0, 1, "整场下线只扫一次")
	wantNoLeak(t, db, "整场下线")
}

// 同一事件重投：第二次扫不到在线行，Affected=0 且一条事件都不补。
// 这是消费者判定「可以提交位点」的依据，也是本服务不建事件占用表的全部理由。
func TestOfflineSessionOutputsReplayOfSameEventIsNoop(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	hd := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_HD)
	})
	sd := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_SD)
	})

	in := sessionOfflineInput("evt-dup-1")
	first, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).OfflineSessionOutputs(in)
	first = wantOK(t, first, err, "重投第一次")
	wantField(t, "重投第一次", "Affected", first.Affected, int32(2))

	eventsAfterFirst := len(db.outbox)
	writes := snapshotWrites(db)
	offlineCalls := db.count("StreamOutputs.MarkOfflineTx")
	hdSnapshot := *mustOutput(t, db, hd.OutputId)
	sdSnapshot := *mustOutput(t, db, sd.OutputId)

	second, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).OfflineSessionOutputs(in)
	second = wantOK(t, second, err, "同一事件重投")
	wantField(t, "重投第二次", "Affected", second.Affected, int32(0))
	wantField(t, "重投第二次", "Scanned", second.Scanned, int32(0))
	if len(second.OutputIDs) != 0 {
		t.Fatalf("重投不该报出下线档位：got=%v", second.OutputIDs)
	}
	if !second.Noop() {
		t.Fatalf("无可下线档位必须是 Noop（成功结论），否则消费者不会提交位点")
	}
	wantField(t, "重投第二次", "事件条数不增加", len(db.outbox), eventsAfterFirst)
	wantCalls(t, db, "StreamOutputs.MarkOfflineTx", offlineCalls, 0, "重投第二次不得再条件下线")
	wantNoWrites(t, db, writes, "重投第二次零写入")
	assertOutputRowUnchanged(t, db, hd.OutputId, hdSnapshot)
	assertOutputRowUnchanged(t, db, sd.OutputId, sdSnapshot)
}

// 入参门禁：每种非法都要落到各自的哨兵，且一律零写入。
// 表里刻意成对放「相邻但结论不同」的取值（0 与 -1、0 与 9、64 与 65 字符），
// 这样把判定写成 `<= 0` 之外的形式（只判 ==0、只判 <0、按字节数判长度）就会红。
func TestOfflineSessionOutputsValidationGateTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*OfflineSessionOutputsInput)
		wantErr error
	}{
		{"房间为 0", func(in *OfflineSessionOutputsInput) { in.RoomID = 0 }, model.ErrInvalidRoomID},
		{"房间为负", func(in *OfflineSessionOutputsInput) { in.RoomID = -1 }, model.ErrInvalidRoomID},
		{"场次为 0", func(in *OfflineSessionOutputsInput) { in.SessionID = 0 }, model.ErrInvalidSessionID},
		{"场次为负", func(in *OfflineSessionOutputsInput) { in.SessionID = -1 }, model.ErrInvalidSessionID},
		{"原因未指定", func(in *OfflineSessionOutputsInput) { in.Reason = model.ReasonUnspecified },
			model.ErrInvalidTransition},
		{"原因为负数", func(in *OfflineSessionOutputsInput) { in.Reason = -1 }, model.ErrInvalidTransition},
		{"原因为枚举外值", func(in *OfflineSessionOutputsInput) { in.Reason = model.ReasonDataGap + 1 },
			model.ErrInvalidTransition},
		{"事件 ID 为空", func(in *OfflineSessionOutputsInput) { in.EventID = "" }, model.ErrEmptyEventID},
		{"事件 ID 全空白", func(in *OfflineSessionOutputsInput) { in.EventID = "   " }, model.ErrEmptyEventID},
		{"事件 ID 超一字符", func(in *OfflineSessionOutputsInput) {
			in.EventID = strings.Repeat("e", maxEventIDRunes+1)
		}, model.ErrEmptyEventID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			online := seedOutput(db, model.StreamOutputStateOnline, nil)
			before := snapshotWrites(db)
			snapshot := *mustOutput(t, db, online.OutputId)

			in := sessionOfflineInput("evt-gate-1")
			tc.mutate(&in)
			res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).OfflineSessionOutputs(in)
			if res != nil {
				t.Fatalf("非法入参必须返回空结果，got=%+v", res)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误归因不对：got=%v want=%v", err, tc.wantErr)
			}
			assertOutputRowUnchanged(t, db, online.OutputId, snapshot)
			wantNoWrites(t, db, before, tc.name)
			if len(db.outbox) != 0 {
				t.Fatalf("非法入参不得登记事件，got=%d", len(db.outbox))
			}
		})
	}
}

// 合法边界配对：恰好 64 字符的事件 ID 必须放行（否则上一例的「超一字符」会因为上限写反而一起绿）。
func TestOfflineSessionOutputsAcceptsEventIDAtColumnWidth(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedOutput(db, model.StreamOutputStateOnline, nil)

	in := sessionOfflineInput(strings.Repeat("e", maxEventIDRunes))
	res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).OfflineSessionOutputs(in)
	res = wantOK(t, res, err, "恰好到列宽的事件 ID")
	wantField(t, "恰好到列宽", "Affected", res.Affected, int32(1))
	wantField(t, "恰好到列宽", "payload.request_id",
		eventPayload(t, eventAt(t, db, 0))["request_id"], any("evt:"+strings.Repeat("e", maxEventIDRunes)))
}

// 并发抢跑：某个档位在读快照之后、条件下线之前被别人摘掉（到期清扫或运营手工下线）。
// 结论必须是「抢跑的那个不计 Affected、不补事件」，而不是整场失败或谎报下线了两个。
func TestOfflineSessionOutputsSkipsLevelLostToRace(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	hd := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_HD)
	})
	sd := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_SD)
	})

	// 第一次条件下线（HD，因为它档位号小排在前）之前，把 SD 抢先置为已下线：
	// 模拟另一个入口在同一事务窗口内提交。
	db.onHit("StreamOutputs.MarkOfflineTx", func() {
		row := db.outputs[sd.OutputId]
		row.State, row.OfflineReason = model.StreamOutputStateOffline, model.ReasonTimeout
	})

	in := sessionOfflineInput("evt-race-1")
	res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).OfflineSessionOutputs(in)
	res = wantOK(t, res, err, "抢跑")
	wantField(t, "抢跑", "Scanned 仍是 2", res.Scanned, int32(2))
	wantField(t, "抢跑", "Affected 只算真正下线的", res.Affected, int32(1))
	if len(res.OutputIDs) != 1 || res.OutputIDs[0] != hd.OutputId {
		t.Fatalf("OutputIDs 只能含真正下线的档位，got=%v want=[%d]", res.OutputIDs, hd.OutputId)
	}
	wantEvents(t, db, []string{model.EventTypeStreamOutputOffline}, "抢跑")
	wantField(t, "抢跑", "事件对应的是 HD 档",
		toInt64(t, eventPayload(t, eventAt(t, db, 0)), "output_id"), hd.OutputId)
	// 被抢跑的行保留别人的归因，不被本事件覆写。
	wantField(t, "抢跑", "抢跑者的 reason 不被改写",
		mustOutput(t, db, sd.OutputId).OfflineReason, model.ReasonTimeout)
}

// 事务原子性两条分支：条件下线失败、事件登记失败都必须整体回滚。
// 只测第一条会漏掉「行改了但事件没写」这种下游永远收不到结论的坏情况。
func TestOfflineSessionOutputsRollsBackWholeTransaction(t *testing.T) {
	seedTwoLevels := func(db *store) (hdID, sdID int64) {
		hd := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
			o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_HD)
		})
		sd := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
			o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_SD)
		})
		return hd.OutputId, sd.OutputId
	}

	t.Run("条件下线失败", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		hdID, sdID := seedTwoLevels(db)
		db.failOn("StreamOutputs.MarkOfflineTx", errors.New("deadlock found"))

		res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).
			OfflineSessionOutputs(sessionOfflineInput("evt-fail-cas"))
		if res != nil || err == nil {
			t.Fatalf("条件下线失败必须回传错误且不给结果：res=%v err=%v", res, err)
		}
		if !strings.Contains(err.Error(), "deadlock") {
			t.Fatalf("错误必须带上游原因，got=%v", err)
		}
		wantField(t, "条件下线失败", "SD 档仍在线", mustOutput(t, db, sdID).State, model.StreamOutputStateOnline)
		wantField(t, "条件下线失败", "HD 档仍在线", mustOutput(t, db, hdID).State, model.StreamOutputStateOnline)
		if len(db.outbox) != 0 {
			t.Fatalf("回滚后不得留下事件，got=%d", len(db.outbox))
		}
	})

	t.Run("事件登记失败", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		hdID, sdID := seedTwoLevels(db)
		db.failOn("Outbox.Insert", errors.New("duplicate column"))

		res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).
			OfflineSessionOutputs(sessionOfflineInput("evt-fail-event"))
		if res != nil || err == nil {
			t.Fatalf("事件登记失败必须回传错误：res=%v err=%v", res, err)
		}
		// 关键断言：绝不能出现「一个档位已下线、另一个还挂着」的半下线态。
		wantField(t, "事件登记失败", "HD 档未被单独下线", mustOutput(t, db, hdID).State, model.StreamOutputStateOnline)
		wantField(t, "事件登记失败", "SD 档未被单独下线", mustOutput(t, db, sdID).State, model.StreamOutputStateOnline)
		wantField(t, "事件登记失败", "offline_reason 未残留", mustOutput(t, db, hdID).OfflineReason, int32(0))
		wantField(t, "事件登记失败", "offline_at 未残留", mustOutput(t, db, hdID).OfflineAt, int64(0))
		if len(db.outbox) != 0 {
			t.Fatalf("回滚后不得留下事件，got=%d", len(db.outbox))
		}
	})
}

// 扫档失败（MySQL 读不动）：错误原样回传、零写入。
// 这条分支决定消费者会不会提交位点：静默当「没有档位」处理就会把故障洗成成功。
func TestOfflineSessionOutputsPropagatesScanFailure(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	online := seedOutput(db, model.StreamOutputStateOnline, nil)
	before := snapshotWrites(db)
	snapshot := *mustOutput(t, db, online.OutputId)
	db.failOn("StreamOutputs.ListOnlineBySession", errors.New("too many connections"))

	res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).
		OfflineSessionOutputs(sessionOfflineInput("evt-scan-fail"))
	if res != nil || err == nil {
		t.Fatalf("扫档失败必须回传错误：res=%v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "too many connections") {
		t.Fatalf("错误必须带上游原因，got=%v", err)
	}
	assertOutputRowUnchanged(t, db, online.OutputId, snapshot)
	wantNoWrites(t, db, before, "扫档失败")
}

// 一场次在线档位数超过上限属脏数据：fail closed，一个都不摘。
// 挑一部分下线会让「哪些档位还在分发」变成不可复现的问题，比整场不动更糟。
func TestOfflineSessionOutputsFailsClosedOnOverflow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	ids := make([]int64, 0, model.MaxSessionOutputs+1)
	for i := 0; i <= model.MaxSessionOutputs; i++ {
		level := int32(1000 + i) // 每个档位号唯一，避开 uniq_output_natural
		row := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
			o.BitrateLevel = level
		})
		ids = append(ids, row.OutputId)
	}
	before := snapshotWrites(db)

	res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).
		OfflineSessionOutputs(sessionOfflineInput("evt-overflow"))
	if res != nil {
		t.Fatalf("溢出必须返回空结果，got=%+v", res)
	}
	if !errors.Is(err, model.ErrSessionOutputOverflow) {
		t.Fatalf("必须报档位溢出：got=%v", err)
	}
	for _, id := range ids {
		wantField(t, "溢出", "所有档位都保持在线", mustOutput(t, db, id).State, model.StreamOutputStateOnline)
	}
	wantNoWrites(t, db, before, "溢出")
	if len(db.outbox) != 0 {
		t.Fatalf("溢出不得登记事件，got=%d", len(db.outbox))
	}
}

// 上限配对：恰好 MaxSessionOutputs 个在线档位必须正常下线（否则上一例的溢出判定写成 >= 也会绿）。
func TestOfflineSessionOutputsHandlesExactlyMaxSessionOutputs(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	for i := 0; i < model.MaxSessionOutputs; i++ {
		level := int32(1000 + i)
		seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
			o.BitrateLevel = level
		})
	}

	res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).
		OfflineSessionOutputs(sessionOfflineInput("evt-at-limit"))
	res = wantOK(t, res, err, "恰好到上限")
	wantField(t, "恰好到上限", "Affected", res.Affected, int32(model.MaxSessionOutputs))
	wantField(t, "恰好到上限", "Scanned", res.Scanned, int32(model.MaxSessionOutputs))
	wantField(t, "恰好到上限", "事件条数", len(db.outbox), int(model.MaxSessionOutputs))
}

// trace_id 超列宽按列宽裁剪而不是整体失败（与 OfflineStreamOutput 同口径）；
// event_id 超长却必须拒绝：截断后的事件 ID 与原始事件不同义，重放时对不上。
func TestOfflineSessionOutputsTruncatesTraceIDOnly(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	hd := seedOutput(db, model.StreamOutputStateOnline, nil)

	in := sessionOfflineInput("evt-trace-cut")
	in.TraceID = strings.Repeat("t", maxTraceIDRunes+20)
	res, err := NewOfflineSessionOutputsLogic(context.Background(), svcCtx).OfflineSessionOutputs(in)
	res = wantOK(t, res, err, "超长 trace")
	wantField(t, "超长 trace", "Affected", res.Affected, int32(1))

	got := mustOutput(t, db, hd.OutputId).TraceId
	if len([]rune(got)) != maxTraceIDRunes {
		t.Fatalf("trace_id 必须裁到列宽 %d，got %d 字符：%q", maxTraceIDRunes, len([]rune(got)), got)
	}
	wantField(t, "超长 trace", "事件 trace 同为裁剪后的值", eventAt(t, db, 0).TraceId, got)
	wantField(t, "超长 trace", "事件 ID 不受裁剪影响",
		eventPayload(t, eventAt(t, db, 0))["source_event_id"], "evt-trace-cut")
}
