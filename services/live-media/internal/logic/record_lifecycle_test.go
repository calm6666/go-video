package logic

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"
)

// 录制域登记与停止：StartLiveRecord / StopLiveRecord。
// 重点是四条不变量：request_id 重放不产生第二行、同场次不双录、非法状态迁移零副作用、
// 业务写与 Outbox 事件同事务（事件写失败必须连任务行一起回滚）。
//
// 标注「缺陷 #1 阻塞」的用例依赖事务提交后的可见效果，在当前生产缺陷下会 Skip
// （见 outbox_contract_test.go 头注释与 fakes_test.go:requireNoEnvelopeBug）。

func startRecordReq(requestID string) *rpc.StartLiveRecordReq {
	return &rpc.StartLiveRecordReq{
		RoomId: testRoomID, LiveSessionId: testSession, SegmentSeconds: 10,
		TimeoutSeconds: 90, OutputBucket: "live-rec", OutputPrefix: "rec/71001",
		RequestId: requestID, TraceId: "trace-start-1",
	}
}

// 登记快照的内容：这是「事务里打算写的行」，回滚也改不了它，因此今天就能断言。
func TestStartLiveRecordBuildsCorrectRegistrationSnapshot(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)

	_, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(startRecordReq("req-rec-1"))
	acceptEnvelopeBugForSnapshot(t, err, "登记（仅断言写入快照）")

	row := triedRecord(t, db, 0)
	if row.State != model.RecordStatePending {
		t.Errorf("登记态必须是 PENDING，实际 %d", row.State)
	}
	if row.RoomId != testRoomID || row.LiveSession != testSession {
		t.Errorf("房间/场次没有原样落库：room=%d session=%d", row.RoomId, row.LiveSession)
	}
	if row.RequestId != "req-rec-1" || row.TraceId != "trace-start-1" {
		t.Errorf("幂等键与 trace 必须原样落库：request_id=%q trace_id=%q", row.RequestId, row.TraceId)
	}
	if row.SegmentSeconds != 10 || row.OutputBucket != "live-rec" || row.OutputPrefix != "rec/71001" {
		t.Errorf("分片时长与产物引用快照不对：%d %q %q",
			row.SegmentSeconds, row.OutputBucket, row.OutputPrefix)
	}
	// heartbeat_at/timeout_at 成对写入：差值就是登记的无心跳秒数快照（见 timeoutBudget）。
	if row.TimeoutAt-row.HeartbeatAt != 90 {
		t.Errorf("timeout_at-heartbeat_at 应等于登记的 timeout_seconds，实际 %d", row.TimeoutAt-row.HeartbeatAt)
	}
	// 登记行不得自带水位/计数：真值只能从切片表算出来（否则断点续录会跳过未落的片）。
	if row.LastSeq != 0 || row.SegmentCount != 0 || row.GapCount != 0 || row.RecordedDuration != 0 {
		t.Errorf("登记行不该带水位或派生计数：%+v", row)
	}
	if row.Version != 1 {
		t.Errorf("新行 version 应为 1，实际 %d", row.Version)
	}
	if row.RecordStartAt != 0 || row.RecordEndAt != 0 {
		t.Errorf("实际起止时刻只能由 Worker 上报，登记时必须为空：%d/%d",
			row.RecordStartAt, row.RecordEndAt)
	}
}

func TestStartLiveRecordRegistersPendingTaskAndOneEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)

	info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(startRecordReq("req-rec-1"))
	requireNoEnvelopeBug(t, err, "首次登记")
	info = wantOK(t, info, err, "首次登记")

	if info.GetRecordId() <= 0 {
		t.Fatalf("登记必须返回自增 record_id，实际 %d", info.GetRecordId())
	}
	if info.GetState() != rpc.LiveRecordState_LIVE_RECORD_STATE_PENDING {
		t.Errorf("登记态必须是 PENDING，实际 %v", info.GetState())
	}
	if info.GetLastSeq() != 0 || info.GetSegmentCount() != 0 || info.GetGapCount() != 0 {
		t.Errorf("登记行的水位与计数应为 0：%+v", info)
	}
	if len(db.records) != 1 {
		t.Fatalf("首次登记应恰好 1 行，实际 %d", len(db.records))
	}
	row := mustRecord(t, db, info.GetRecordId())
	if row.Version != 1 || row.SegmentSeconds != 10 || row.OutputBucket != "live-rec" {
		t.Errorf("提交后的行快照不对：version=%d segment_seconds=%d bucket=%q",
			row.Version, row.SegmentSeconds, row.OutputBucket)
	}
	wantEvents(t, db, []string{model.EventTypeRecordStateChanged}, "首次登记")
	ev := eventAt(t, db, 0)
	if ev.AggregateType != model.AggregateRecordTask {
		t.Errorf("聚合类型=%s", ev.AggregateType)
	}
	if ev.AggregateId != strconv.FormatInt(info.GetRecordId(), 10) {
		t.Errorf("事件聚合主键必须是新任务主键（消费者据此定位任务），实际 %q vs %d",
			ev.AggregateId, info.GetRecordId())
	}
	p := eventPayload(t, ev)
	if toInt64(t, p, "state") != int64(model.RecordStatePending) ||
		toInt64(t, p, "record_id") != info.GetRecordId() {
		t.Errorf("事件 payload 缺事实：%v", p)
	}
	// 事件里只允许出现 bucket/prefix 引用，不得出现凭据形态的值。
	wantNoLeak(t, db, "首次登记事件")
}

// 幂等第一层：同 request_id 重放必须原样回首次登记的任务，零写、零事件。
func TestStartLiveRecordReplaysSameRequestIDWithoutSecondRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seeded := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) {
		r.RequestId = "req-rec-dup"
		r.LastSeq = 42
	})

	before := snapshotWrites(db)
	info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(startRecordReq("req-rec-dup"))
	info = wantOK(t, info, err, "幂等重放")

	if info.GetRecordId() != seeded.RecordId || info.GetLastSeq() != 42 {
		t.Fatalf("重放必须原样回首次登记的任务，实际 record_id=%d last_seq=%d", info.GetRecordId(), info.GetLastSeq())
	}
	if len(db.records) != 1 {
		t.Fatalf("uniq_request_id 命中却新增了行：现有 %d 行", len(db.records))
	}
	wantNoWrites(t, db, before, "幂等重放")
	wantEvents(t, db, nil, "幂等重放不得再发事件")
}

// 幂等第二层：request_id 不同但同场次已有活跃任务 → 复用（同场次双录会让
// uniq_record_seq 上的切片序号互相覆盖）。这条路径不写库、不发事件。
func TestStartLiveRecordReusesActiveTaskOfSameSession(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	active := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) {
		r.LastSeq = 9
	})
	// 另一场次的活跃任务不得被复用（跨场次复用会把两场的切片混在一起）。
	seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) {
		r.LiveSession = testSession2
		r.LastSeq = 77
	})
	before := snapshotWrites(db)

	in := startRecordReq("req-rec-new-session")
	in.LiveSessionId = active.LiveSession
	info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(in)
	info = wantOK(t, info, err, "同场次复用")

	if info.GetRecordId() != active.RecordId || info.GetLastSeq() != 9 {
		t.Fatalf("应复用本场次活跃任务 %d，实际 %d", active.RecordId, info.GetRecordId())
	}
	if len(db.records) != 2 {
		t.Fatalf("复用路径不得新增行，现有 %d 行", len(db.records))
	}
	wantNoWrites(t, db, before, "同场次复用")
	wantEvents(t, db, nil, "同场次复用")

	// 换个场次就得真的登记（说明复用判定按 (room, session) 精确匹配，不是按房间一把抓）。
	in2 := startRecordReq("req-rec-other-session")
	in2.LiveSessionId = testSession2
	_, err = NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(in2)
	requireNoEnvelopeBug(t, err, "另一场次登记")
	if err != nil {
		t.Fatalf("另一场次复用已存在的活跃任务本身就是缺陷：预期复用，实际 %v", err)
	}
	if len(db.records) != 2 {
		t.Fatalf("testSession2 已有活跃任务，应复用而非新增：%d 行", len(db.records))
	}
}

// 幂等第三层：预读时对手事务尚未提交，写入时才撞 uniq_request_id → 必须回读既有行，
// 不当失败、不写第二行（这是 gRPC 重试与并发 Start 的常见交错）。
func TestStartLiveRecordInsertRaceFallsBackToExistingRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	// 对手已提交的行（同 request_id、不同场次，故不会走「同场次复用」那条更早的分支）。
	racer := seedRecord(db, model.RecordStatePending, func(r *model.LiveRecordTask) {
		r.RequestId = "req-rec-race"
		r.LiveSession = testSession2
	})
	// 本次请求的预读发生在对手提交之前：读不到，于是走到 INSERT，撞唯一键。
	db.missOnce("RecordTasks.FindByRequestID")

	info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(startRecordReq("req-rec-race"))
	info = wantOK(t, info, err, "唯一键竞态回读")

	if info.GetRecordId() != racer.RecordId {
		t.Fatalf("应回读竞态方登记的行 %d，实际 %d", racer.RecordId, info.GetRecordId())
	}
	if len(db.records) != 1 {
		t.Fatalf("竞态路径写了第二行：%d", len(db.records))
	}
	wantCalls(t, db, "RecordTasks.InsertTx", 0, 1, "竞态路径只尝试一次插入")
	wantEvents(t, db, nil, "竞态回读不得再发事件")
}

// 唯一键报错但回读仍为空（对手回滚了）：必须把失败如实抛出，不能伪造成功。
func TestStartLiveRecordDupWithoutRowIsNotFabricatedSuccess(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	db.onHit("RecordTasks.InsertTx", func() {
		// 撞键：库里塞一行同 request_id，但让随后的回读读不到（对手已回滚的形态）。
		seedRecord(db, model.RecordStatePending, func(r *model.LiveRecordTask) {
			r.RequestId = "req-rec-ghost"
			r.LiveSession = testSession2
		})
		db.missOnce("RecordTasks.FindByRequestID")
	})

	info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(startRecordReq("req-rec-ghost"))
	wantFail(t, info, err, model.ErrRequestIdDuplicated, "撞键又查无此行")
	if len(db.records) != 0 {
		t.Fatalf("鬼行随事务回滚后仍留在库里：%d 行（用例前提失效）", len(db.records))
	}
	wantEvents(t, db, nil, "失败路径不得留下事件")
}

func TestStartLiveRecordValidatesInputAndWritesNothing(t *testing.T) {
	longRequestID := strings.Repeat("r", maxRequestIDRunes+1)
	longPrefix := strings.Repeat("p", maxPrefixRunes+1)

	cases := []struct {
		name   string
		mutate func(*rpc.StartLiveRecordReq)
		// want 为 nil 表示「必须被拒绝，但不保证包装了哨兵」（见缺陷 #4）。
		want error
	}{
		{"房间号为零", func(r *rpc.StartLiveRecordReq) { r.RoomId = 0 }, model.ErrInvalidRoomID},
		{"场次号为零", func(r *rpc.StartLiveRecordReq) { r.LiveSessionId = 0 }, model.ErrInvalidSessionID},
		{"终点早于起点", func(r *rpc.StartLiveRecordReq) { r.StartAt, r.EndAt = 2000, 1000 }, model.ErrInvalidSegmentRange},
		{"负起点", func(r *rpc.StartLiveRecordReq) { r.StartAt = -1 }, model.ErrInvalidSegmentRange},
		{"缺幂等键", func(r *rpc.StartLiveRecordReq) { r.RequestId = "   " }, model.ErrEmptyRequestID},
		{"幂等键超列宽", func(r *rpc.StartLiveRecordReq) { r.RequestId = longRequestID }, model.ErrEmptyRequestID},
		{"分片时长超上限", func(r *rpc.StartLiveRecordReq) { r.SegmentSeconds = 61 }, model.ErrInvalidTransition},
		{"对象 key 带签名参数", func(r *rpc.StartLiveRecordReq) { r.OutputPrefix = leakObjectKey }, model.ErrInvalidBucketRef},
		{"桶名写成 URL", func(r *rpc.StartLiveRecordReq) { r.OutputBucket = "https://live-rec" }, model.ErrInvalidBucketRef},
		{"桶名为空", func(r *rpc.StartLiveRecordReq) { r.OutputBucket = "" }, model.ErrInvalidBucketRef},
		// 缺陷 #4：helpers.go:233-235 的「列宽超限」分支没有 %w 包装哨兵，只能断言被拒绝。
		{"前缀超列宽", func(r *rpc.StartLiveRecordReq) { r.OutputPrefix = longPrefix }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			in := startRecordReq("req-rec-bad")
			tc.mutate(in)
			before := snapshotWrites(db)

			info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(in)
			if err == nil {
				t.Fatalf("%s：应被拒绝，实际返回 %+v", tc.name, info)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("%s：应报 %v，实际 err=%v", tc.name, tc.want, err)
			}
			if !isNilPtr(info) {
				t.Fatalf("%s：失败路径不得带回响应体：%+v", tc.name, info)
			}
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
			wantNoLeak(t, db, tc.name)
		})
	}
}

// 拉流凭据一旦入库就是长期泄漏：签名地址必须被拒绝，且不得有任何明文残留。
func TestStartLiveRecordRejectsSourceTaskFromAnotherRoom(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	foreign := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) { r.RoomId = testRoomOther })

	in := startRecordReq("req-rec-src")
	in.SourceTaskId = foreign.TaskId
	before := snapshotWrites(db)
	info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(in)
	wantFail(t, info, err, model.ErrInvalidRoomID, "录制源跨房间")
	wantNoWrites(t, db, before, "录制源跨房间")

	db2 := newStore()
	svcCtx2 := newTestSvc(db2)
	before2 := snapshotWrites(db2)
	in2 := startRecordReq("req-rec-src2")
	in2.SourceTaskId = 999999 // 不存在的转码任务：宁可拒绝登记，也不留悬空引用
	info2, err := NewStartLiveRecordLogic(context.Background(), svcCtx2).StartLiveRecord(in2)
	wantFail(t, info2, err, model.ErrTranscodeTaskNotFound, "悬空录制源")
	if !strings.Contains(err.Error(), "source_task_id") {
		t.Fatalf("错误里必须带上可归因的 source_task_id，实际 %v", err)
	}
	wantNoWrites(t, db2, before2, "悬空录制源")

	in3 := startRecordReq("req-rec-src3")
	in3.SourceTaskId = -1
	info3, err := NewStartLiveRecordLogic(context.Background(), svcCtx2).StartLiveRecord(in3)
	wantFail(t, info3, err, model.ErrTranscodeTaskNotFound, "负的录制源")
	wantNoWrites(t, db2, before2, "负的录制源")
}

// 同房间、同场次的源任务合法：录制必须能把来源审计链写进 source_task_id。
func TestStartLiveRecordAcceptsSourceTaskOfSameRoom(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	src := seedTranscode(db, model.TranscodeStateRunning, nil)

	in := startRecordReq("req-rec-src-ok")
	in.SourceTaskId = src.TaskId
	_, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(in)
	acceptEnvelopeBugForSnapshot(t, err, "合法录制源")

	row := triedRecord(t, db, 0)
	if row.SourceTaskId != src.TaskId {
		t.Errorf("source_task_id 必须落库，实际 %d", row.SourceTaskId)
	}
}

// 事件写失败必须连任务行一起回滚（AGENTS.md §5：业务写与 Outbox 同事务）。
func TestStartLiveRecordRollsBackWhenEventWriteFails(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	db.failOn("Outbox.Insert", errOutboxDown)

	info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(startRecordReq("req-rec-tx"))
	requireNoEnvelopeBug(t, err, "事件写失败回滚")
	wantFail(t, info, err, errOutboxDown, "事件写失败")
	if len(db.records) != 0 {
		t.Fatalf("事务回滚后仍留下任务行：%+v", db.records)
	}
	if len(db.outbox) != 0 {
		t.Fatalf("回滚后不该有事件行：%d", len(db.outbox))
	}
	wantCalls(t, db, "RecordTasks.InsertTx", 0, 1, "事务内确实尝试过插入")
}

// 依赖读故障时 fail closed：不能把「读不动」伪装成「没有这行」而继续登记。
func TestStartLiveRecordFailsClosedWhenPreReadErrors(t *testing.T) {
	for _, op := range []string{"RecordTasks.FindByRequestID", "RecordTasks.FindActiveBySession",
		"TranscodeTasks.FindOne", "RecordTasks.InsertTx"} {
		t.Run(op, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			db.failOn(op, errModelDown)
			in := startRecordReq("req-rec-down")
			if op == "TranscodeTasks.FindOne" {
				in.SourceTaskId = 5 // 只有给了录制源才会走这条读
			}
			before := snapshotWrites(db)
			info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).StartLiveRecord(in)
			if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
				t.Fatalf("%s 故障必须原样抛出（不得伪造成功）：%v", op, err)
			}
			if !isNilPtr(info) {
				t.Errorf("依赖故障路径不得带回响应体：%+v", info)
			}
			// 只有 InsertTx 自身失败时事务才算过；其余在事务外就被拦住。
			if op != "RecordTasks.InsertTx" {
				wantNoWrites(t, db, before, op)
			}
		})
	}
}

func stopRecordReq(id, expVer int64) *rpc.StopLiveRecordReq {
	return &rpc.StopLiveRecordReq{
		RecordId: id, ExpectedVersion: expVer, EndAt: nowTS() + 600,
		Reason: rpc.FailureReason_FAILURE_REASON_MANUAL, RequestId: "req-stop-1",
		Operator: "ops-alice", TraceId: "trace-stop-1",
	}
}

func TestStopLiveRecordAdvancesRecordingToStoppingOnce(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) {
		r.LastSeq, r.RecordStartAt = 7, nowTS()-300
	})
	wantVer := row.Version

	info, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(stopRecordReq(row.RecordId, wantVer))
	requireNoEnvelopeBug(t, err, "停止录制")
	info = wantOK(t, info, err, "停止录制")

	if info.GetState() != rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPING {
		t.Fatalf("状态应推进到 STOPPING，实际 %v", info.GetState())
	}
	if info.GetLastSeq() != 7 || info.GetRecordStartAt() != row.RecordStartAt {
		t.Errorf("停止指令不得改动水位与实际开始时刻：last_seq=%d record_start_at=%d",
			info.GetLastSeq(), info.GetRecordStartAt())
	}
	// 本方法只下发指令：实际结束时刻与终态 STOPPED 只能由 Worker 上报，
	// 提前写终值会让回放误判区间完整而尾部还在写。
	if info.GetRecordEndAt() != 0 {
		t.Errorf("record_end_at 必须由 Worker 收尾时上报，收到停止指令就写终值：%d", info.GetRecordEndAt())
	}
	if info.GetEndAt() <= 0 {
		t.Errorf("期望终点 end_at 应被下发（%d）", info.GetEndAt())
	}
	if info.GetVersion() != wantVer+1 {
		t.Errorf("一次成功推进恰好 ++version：%d → %d", wantVer, info.GetVersion())
	}
	wantEvents(t, db, []string{model.EventTypeRecordStateChanged}, "停止指令")
	p := eventPayload(t, eventAt(t, db, 0))
	if toInt64(t, p, "prev_state") != int64(model.RecordStateRecording) ||
		toInt64(t, p, "state") != int64(model.RecordStateStopping) || toInt64(t, p, "last_seq") != 7 {
		t.Errorf("事件未记录迁移前后态与水位：%v", p)
	}
	if _, ok := p["operator"]; !ok {
		t.Errorf("事件应带操作人归因：%v", p)
	}
	// 幂等键不得被状态推进覆写（它是 StartLiveRecord 的回放依据）。
	if mustRecord(t, db, row.RecordId).RequestId != row.RequestId {
		t.Errorf("StopLiveRecord 不得覆写 request_id")
	}
}

// 停止指令的幂等以状态为准：已在 STOPPING 的重投不再推进、不再发事件、不再 ++version。
func TestStopLiveRecordReplayOnStoppingIsSideEffectFree(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateStopping, nil)
	before := snapshotWrites(db)

	info, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(stopRecordReq(row.RecordId, 0))
	info = wantOK(t, info, err, "停止重放")
	if info.GetState() != rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPING {
		t.Fatalf("重放不得改写状态：%v", info.GetState())
	}
	if info.GetVersion() != row.Version {
		t.Errorf("重放不得 ++version：%d → %d", row.Version, info.GetVersion())
	}
	wantNoWrites(t, db, before, "停止重放")
	wantEvents(t, db, nil, "停止重放")
}

func TestStopLiveRecordRefusesIllegalStatesAndMutatesNothing(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		want  error
	}{
		{"未开录不可停止", model.RecordStatePending, model.ErrInvalidTransition},
		{"终态 STOPPED 不可改写", model.RecordStateStopped, model.ErrTerminalState},
		{"终态 CANCELLED 不可改写", model.RecordStateCancelled, model.ErrTerminalState},
		{"失败态只能重新开录", model.RecordStateFailed, model.ErrInvalidTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedRecord(db, tc.state, nil)
			snapshot := *mustRecord(t, db, row.RecordId)
			before := snapshotWrites(db)

			info, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(stopRecordReq(row.RecordId, 0))
			wantFail(t, info, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
			assertRowUnchanged(t, db, snapshot.RecordId, snapshot)
		})
	}
}

func TestStopLiveRecordMissingRowAndVersionConflict(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)
	info, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(stopRecordReq(4242, 0))
	wantFail(t, info, err, model.ErrRecordTaskNotFound, "不存在的录制任务")
	// 预读之外一行都没写：0 行归因必须靠读，不能靠写。
	wantNoWrites(t, db, before, "不存在的录制任务")

	row := seedRecord(db, model.RecordStateRecording, nil)
	beforeVer := row.Version
	stale := stopRecordReq(row.RecordId, row.Version+5)
	info, err = NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(stale)
	wantFail(t, info, err, model.ErrVersionConflict, "过期版本")
	if got := mustRecord(t, db, row.RecordId); got.State != model.RecordStateRecording || got.Version != beforeVer {
		t.Fatalf("版本冲突却推进了行：state=%d version=%d", got.State, got.Version)
	}
	wantEvents(t, db, nil, "过期版本")
}

// 条件 UPDATE 命中 0 行但预读时行还在：只能靠回读归因（终态）。
// 事务回滚会把钩子造出来的交错态一并撤销，所以这里断言的是「归因与副作用」，
// 不是交错后的库态（库态由真 MySQL 的并发用例负责）。
func TestStopLiveRecordLosesRaceAndClassifiesFromReread(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, nil)
	db.onHit("RecordTasks.UpdateStateTx", func() {
		db.records[row.RecordId].State = model.RecordStateStopped // 别人抢先收尾成终态
		db.records[row.RecordId].Version++
	})

	info, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(stopRecordReq(row.RecordId, 0))
	wantFail(t, info, err, model.ErrTerminalState, "抢先收尾归因")
	wantCalls(t, db, "RecordTasks.UpdateStateTx", 0, 1, "CAS 只发生一次")
	if len(db.outbox) != 0 {
		t.Fatalf("CAS 0 行仍写了事件：%d", len(db.outbox))
	}
	wantCalls(t, db, "Outbox.Insert", 0, 0, "CAS 0 行不得尝试写事件")
}

// 抢先收尾成「非终态的其他状态」时归因必须是 ErrInvalidTransition，而不是笼统冲突。
func TestStopLiveRecordRaceToOtherStateIsInvalidTransition(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, nil)
	db.onHit("RecordTasks.UpdateStateTx", func() {
		db.records[row.RecordId].State = model.RecordStateFailed
	})
	info, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(stopRecordReq(row.RecordId, 0))
	wantFail(t, info, err, model.ErrInvalidTransition, "抢先改判")
}

func TestStopLiveRecordValidatesInput(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) { r.RecordStartAt = 5000 })
	before := snapshotWrites(db)

	_, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(stopRecordReq(0, 0))
	if err == nil || !strings.Contains(err.Error(), "record_id") {
		t.Fatalf("record_id<=0 必须被拒绝且不查库，实际 %v", err)
	}
	noKey := stopRecordReq(row.RecordId, 0)
	noKey.RequestId = ""
	info, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(noKey)
	wantFail(t, info, err, model.ErrEmptyRequestID, "缺幂等键")
	badReason := stopRecordReq(row.RecordId, 0)
	badReason.Reason = rpc.FailureReason(99)
	info, err = NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(badReason)
	wantFail(t, info, err, model.ErrInvalidTransition, "越界原因")
	earlyEnd := stopRecordReq(row.RecordId, 0)
	earlyEnd.EndAt = 4000 // 早于实际开始时刻
	info, err = NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(earlyEnd)
	wantFail(t, info, err, model.ErrInvalidSegmentRange, "终点早于实际开始")
	wantNoWrites(t, db, before, "入参校验")
}

// 停止指令里的 operator 是审计字段：带凭据的文本必须先脱敏再入事件。
// 缺陷 #1 阻塞：operator 只出现在事件 payload 里，事件写不进就看不到结果。
func TestStopLiveRecordRedactsCredentialBearingOperator(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, nil)
	req := stopRecordReq(row.RecordId, 0)
	req.Operator = "ops token=" + leakMarker

	info, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(req)
	requireNoEnvelopeBug(t, err, "脱敏操作人")
	info = wantOK(t, info, err, "脱敏操作人")
	if info.GetState() != rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPING {
		t.Fatalf("状态未推进：%v", info.GetState())
	}
	wantNoLeak(t, db, "operator 脱敏")
	p := eventPayload(t, eventAt(t, db, 0))
	op, _ := p["operator"].(string)
	if strings.Contains(op, leakMarker) || op == "" {
		t.Fatalf("事件里的 operator 未脱敏或缺失：%q", op)
	}
}

// operator 超列宽（64）必须截断而不是让整条停止指令失败：它是审计文本不是业务事实。
func TestStopLiveRecordTruncatesOverlongOperator(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, nil)
	req := stopRecordReq(row.RecordId, 0)
	req.Operator = strings.Repeat("运", maxOperatorRunes+10)

	_, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(req)
	requireNoEnvelopeBug(t, err, "超长 operator")
	if err != nil {
		t.Fatalf("审计文本超长不该让整个停止失败：%v", err)
	}
	wantEvents(t, db, []string{model.EventTypeRecordStateChanged}, "超长 operator")
	p := eventPayload(t, eventAt(t, db, 0))
	if got, _ := p["operator"].(string); runeLen(got) > maxOperatorRunes {
		t.Errorf("operator 未截断到列宽：%d > %d", runeLen(got), maxOperatorRunes)
	}
}

// 依赖故障必须 fail closed：读不动与写不动都不能被读成「无事发生」的成功。
func TestStopLiveRecordFailsClosedOnDependencyErrors(t *testing.T) {
	for _, op := range []string{"RecordTasks.FindOne", "RecordTasks.UpdateStateTx", "Outbox.Insert",
		"DB.TransactCtx"} {
		t.Run(op, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedRecord(db, model.RecordStateRecording, nil)
			db.failOn(op, errModelDown)

			info, err := NewStopLiveRecordLogic(context.Background(), svcCtx).StopLiveRecord(stopRecordReq(row.RecordId, 0))
			if op == "Outbox.Insert" {
				// 缺陷 #1 期间走不到 Outbox.Insert（信封构造更早失败），由 gate 显式 Skip。
				requireNoEnvelopeBug(t, err, op)
			}
			if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
				t.Fatalf("%s 故障必须原样抛出：%v", op, err)
			}
			if !isNilPtr(info) {
				t.Errorf("故障路径不得带回响应体：%+v", info)
			}
			if op == "RecordTasks.UpdateStateTx" || op == "Outbox.Insert" || op == "DB.TransactCtx" {
				if got := mustRecord(t, db, row.RecordId); got.State != model.RecordStateRecording {
					t.Errorf("事务回滚后行态被改：%d", got.State)
				}
			}
			wantEvents(t, db, nil, op+" 之后不得留下事件")
		})
	}
}

// ---------------------------------------------------------------- 小工具

func mustRecord(t *testing.T, db *store, id int64) *model.LiveRecordTask {
	t.Helper()
	row, ok := db.records[id]
	if !ok {
		t.Fatalf("录制任务 %d 不存在", id)
	}
	return row
}

func assertRowUnchanged(t *testing.T, db *store, id int64, want model.LiveRecordTask) {
	t.Helper()
	got := mustRecord(t, db, id)
	if *got != want {
		t.Fatalf("非法路径改动了行：\n got=%+v\nwant=%+v", *got, want)
	}
}

func toInt64(t *testing.T, m map[string]any, key string) int64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("payload 缺字段 %q：%v", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("payload 字段 %q 不是数值：%T", key, v)
	}
	return int64(f)
}
