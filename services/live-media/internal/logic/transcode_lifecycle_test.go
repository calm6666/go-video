package logic

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"
)

// 转码生命周期六个方法：StopLiveTranscode / ReportLiveTranscodeProgress /
// RetryLiveTranscode / CancelLiveTranscode / GetLiveTranscodeTask / ListLiveTranscodeTasks。
//
// 这组方法是「Worker 与运营台共同写的同一行状态」，因此每个用例都同时钉三件事：
//  1. 迁移合法性与归因：状态机判定在 model/transcodeTransitions，logic 只负责「取目标态 →
//     查表 → 条件 UPDATE」，非法迁移必须零副作用（不写行、不写事件、不 ++version）；
//  2. 幂等口径以状态为准而不是以 request_id 为准：request_id 是 StartLiveTranscode 的登记
//     幂等键（uniq_request_id），任何状态推进都不得覆写它，否则登记回放会失效；
//  3. 返回体必须等于提交后的行：终态、心跳、水位这些字段一旦被 logic「顺手」改写，
//     观众侧与超时清扫就会同时得到错误结论。
//
// 交错（「读快照之后行被别人改掉」）一律用 db.onHit 构造，断言的是归因与副作用；
// 交错后的最终库态只有真 MySQL 能证明，不在单测里假装覆盖（见 fakes_test.go 头注释）。

// ---------------------------------------------------------------- 请求构造与断言小工具

func stopTranscodeReq(taskID, expectedVersion int64) *rpc.StopLiveTranscodeReq {
	return &rpc.StopLiveTranscodeReq{
		TaskId: taskID, ExpectedVersion: expectedVersion,
		Reason: rpc.FailureReason_FAILURE_REASON_MANUAL, RequestId: "req-stop-tc-1",
		Operator: "ops-alice", TraceId: "trace-stop-tc-1",
	}
}

func reportTranscodeReq(taskID int64, state rpc.LiveTranscodeState) *rpc.ReportLiveTranscodeProgressReq {
	return &rpc.ReportLiveTranscodeProgressReq{
		TaskId: taskID, State: state, Progress: 55,
		WorkerId: "worker-a", TraceId: "trace-report-tc-1",
	}
}

func retryTranscodeReq(taskID int64) *rpc.RetryLiveTranscodeReq {
	return &rpc.RetryLiveTranscodeReq{
		TaskId: taskID, Reason: "机房网络抖动，换节点重跑", RequestId: "req-retry-tc-1",
		Operator: "ops-bob", TraceId: "trace-retry-tc-1",
	}
}

func cancelTranscodeReq(taskID int64) *rpc.CancelLiveTranscodeReq {
	return &rpc.CancelLiveTranscodeReq{
		TaskId: taskID, Reason: rpc.FailureReason_FAILURE_REASON_MANUAL,
		RequestId: "req-cancel-tc-1", Operator: "ops-carol", TraceId: "trace-cancel-tc-1",
	}
}

func mustTranscode(t *testing.T, db *store, taskID int64) *model.LiveTranscodeTask {
	t.Helper()
	row, ok := db.transcodes[taskID]
	if !ok {
		t.Fatalf("转码任务 %d 不存在", taskID)
	}
	return row
}

func assertTranscodeRowUnchanged(t *testing.T, db *store, taskID int64, want model.LiveTranscodeTask) {
	t.Helper()
	got := mustTranscode(t, db, taskID)
	if *got != want {
		t.Fatalf("非法路径改动了行：\n got=%+v\nwant=%+v", *got, want)
	}
}

// wantField 逐字段比对（投影测试用）：一次报出所有错位的列，便于定位「哪一列串了」。
func wantField[T comparable](t *testing.T, label, name string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s=%v，期望 %v", label, name, got, want)
	}
}

// ---------------------------------------------------------------- StopLiveTranscode

// 停止指令只做「RUNNING→STOPPING + 归因」两件事。
// 尤其是 heartbeat_at/timeout_at 不能被顺延：停止之后 Worker 若真失联，
// 超时清扫必须仍按原来的窗口判超时，否则「下发过停止指令」反而成了免死金牌。
func TestStopLiveTranscodeAdvancesRunningToStoppingOnce(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	started := nowTS() - 500
	row := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) {
		r.StartedAt, r.Progress, r.Attempt = started, 40, 2
	})
	before := *mustTranscode(t, db, row.TaskId)

	info, err := NewStopLiveTranscodeLogic(context.Background(), svcCtx).
		StopLiveTranscode(stopTranscodeReq(row.TaskId, before.Version))
	info = wantOK(t, info, err, "停止转码")

	if info.GetState() != rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPING {
		t.Fatalf("状态应推进到 STOPPING，实际 %v", info.GetState())
	}
	if info.GetVersion() != before.Version+1 {
		t.Errorf("一次成功推进恰好 ++version：%d → %d", before.Version, info.GetVersion())
	}
	wantField(t, "停止转码", "reason", int32(info.GetReason()), model.ReasonManual)
	// 本方法只下发指令：真正退出时刻只能由 Worker 上报，提前写 stopped_at 会让
	// 「已停止」在库里早于进程真的停下，回放尾部就会缺片。
	if info.GetStoppedAt() != 0 {
		t.Errorf("stopped_at 必须由 Worker 收尾时上报，收到停止指令就写终值：%d", info.GetStoppedAt())
	}
	committed := mustTranscode(t, db, row.TaskId)
	if committed.HeartbeatAt != before.HeartbeatAt || committed.TimeoutAt != before.TimeoutAt {
		t.Errorf("停止指令不得改动心跳与超时窗口：heartbeat %d→%d timeout %d→%d",
			before.HeartbeatAt, committed.HeartbeatAt, before.TimeoutAt, committed.TimeoutAt)
	}
	if committed.Progress != 40 || committed.StartedAt != started || committed.Attempt != 2 {
		t.Errorf("停止指令不得改动进度、首次启动时刻与重试次数：%+v", *committed)
	}
	// request_id 是登记幂等键：被覆写后 StartLiveTranscode 的回放就找不回这一行。
	if committed.RequestId != before.RequestId {
		t.Errorf("停止不得覆写 request_id：%q → %q", before.RequestId, committed.RequestId)
	}
	if committed.TraceId != "trace-stop-tc-1" {
		t.Errorf("停止归因应写 trace_id，实际 %q", committed.TraceId)
	}
	if committed.SourceRef != before.SourceRef || committed.TemplateId != before.TemplateId {
		t.Errorf("停止不得改写主数据：source_ref=%q template_id=%d", committed.SourceRef, committed.TemplateId)
	}

	wantEvents(t, db, []string{model.EventTypeTranscodeStateChanged}, "停止指令")
	ev := eventAt(t, db, 0)
	if ev.AggregateType != model.AggregateTranscodeTask ||
		ev.AggregateId != strconv.FormatInt(row.TaskId, 10) {
		t.Errorf("事件聚合引用不对：%s/%s", ev.AggregateType, ev.AggregateId)
	}
	if ev.RoomId != testRoomID {
		t.Errorf("事件冗余的 room_id 便于消费方分流，应等于任务房间：%d", ev.RoomId)
	}
	p := eventPayload(t, ev)
	wantField(t, "停止事件", "prev_state", toInt64(t, p, "prev_state"), int64(model.TranscodeStateRunning))
	wantField(t, "停止事件", "state", toInt64(t, p, "state"), int64(model.TranscodeStateStopping))
	wantField(t, "停止事件", "task_id", toInt64(t, p, "task_id"), row.TaskId)
	wantField(t, "停止事件", "room_id", toInt64(t, p, "room_id"), testRoomID)
	wantField(t, "停止事件", "live_session_id", toInt64(t, p, "live_session_id"), testSession)
	wantField(t, "停止事件", "reason", toInt64(t, p, "reason"), int64(model.ReasonManual))
	if op, _ := p["operator"].(string); op != "ops-alice" {
		t.Errorf("事件必须带操作人归因，实际 %q", op)
	}
	wantNoLeak(t, db, "停止转码")
}

// 停止指令的幂等以状态为准：已在 STOPPING 的重投原样回行，不再推进、不再发事件、不再 ++version。
func TestStopLiveTranscodeReplayOnStoppingIsSideEffectFree(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateStopping, nil)
	before := snapshotWrites(db)
	snapshot := *mustTranscode(t, db, row.TaskId)

	// expected_version 传 0：重放路径必须在 CAS 之前就按状态收敛，不依赖调用方持有版本。
	info, err := NewStopLiveTranscodeLogic(context.Background(), svcCtx).
		StopLiveTranscode(stopTranscodeReq(row.TaskId, 0))
	info = wantOK(t, info, err, "停止重放")

	if info.GetState() != rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPING {
		t.Fatalf("重放不得改写状态：%v", info.GetState())
	}
	if info.GetVersion() != snapshot.Version {
		t.Errorf("重放不得 ++version：%d → %d", snapshot.Version, info.GetVersion())
	}
	if info.GetRequestId() != snapshot.RequestId {
		t.Errorf("重放的响应必须来自库里那一行，request_id 都变了：%q vs %q",
			info.GetRequestId(), snapshot.RequestId)
	}
	wantNoWrites(t, db, before, "停止重放")
	wantEvents(t, db, nil, "停止重放不得再发事件")
}

// 非法迁移取自 model 的迁移表：PENDING 没进程可停（走 Cancel）、FAILED 只能走 Retry、
// 两个终态不可改写。四条路径都必须「一个字节都没写过」。
func TestStopLiveTranscodeRefusesIllegalStatesAndMutatesNothing(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		want  error
	}{
		{"未拉起不可停止，只能取消", model.TranscodeStatePending, model.ErrInvalidTransition},
		{"失败态只能走 Retry", model.TranscodeStateFailed, model.ErrInvalidTransition},
		{"终态 STOPPED 不可改写", model.TranscodeStateStopped, model.ErrTerminalState},
		{"终态 CANCELLED 不可改写", model.TranscodeStateCancelled, model.ErrTerminalState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedTranscode(db, tc.state, nil)
			snapshot := *mustTranscode(t, db, row.TaskId)
			before := snapshotWrites(db)

			info, err := NewStopLiveTranscodeLogic(context.Background(), svcCtx).
				StopLiveTranscode(stopTranscodeReq(row.TaskId, 0))
			wantFail(t, info, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
			assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
		})
	}
}

func TestStopLiveTranscodeMissingRowAndVersionConflict(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)

	info, err := NewStopLiveTranscodeLogic(context.Background(), svcCtx).StopLiveTranscode(stopTranscodeReq(4242, 0))
	wantFail(t, info, err, model.ErrTranscodeTaskNotFound, "不存在的转码任务")
	wantNoWrites(t, db, before, "不存在的转码任务")

	row := seedTranscode(db, model.TranscodeStateRunning, nil)
	snapshot := *mustTranscode(t, db, row.TaskId)
	info, err = NewStopLiveTranscodeLogic(context.Background(), svcCtx).
		StopLiveTranscode(stopTranscodeReq(row.TaskId, row.Version+5))
	wantFail(t, info, err, model.ErrVersionConflict, "过期版本")
	wantEvents(t, db, nil, "过期版本")
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
}

// 预读后、CAS 前被对手抢先推进：0 行必须回读归因，且事件一条都不能留下。
//
// 归因顺序由 classifyTranscodeZeroRow 决定（先版本、再终态、最后非法迁移），因此
// 「带 expected_version 的抢先迁移」一律报版本冲突（调用方必须重读后重来），
// 只有不带版本的调用方才拿得到「对手写进去的那个状态」的业务判定。
func TestStopLiveTranscodeLosesRaceAndClassifiesFromReread(t *testing.T) {
	cases := []struct {
		name       string
		toState    int32
		want       error
		useVersion bool // 是否把预读到的 version 当 expected_version 带上
	}{
		{"抢先收尾成终态", model.TranscodeStateStopped, model.ErrTerminalState, false},
		{"抢先被取消", model.TranscodeStateCancelled, model.ErrTerminalState, false},
		{"抢先改判失败", model.TranscodeStateFailed, model.ErrInvalidTransition, false},
		{"带版本时抢先迁移先报版本冲突", model.TranscodeStateStopped, model.ErrVersionConflict, true},
		// 版本被抢先推进而状态没变（对手只动了心跳）：归因必须是版本冲突，不是「非法迁移」。
		{"只动了版本", model.TranscodeStateRunning, model.ErrVersionConflict, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedTranscode(db, model.TranscodeStateRunning, nil)
			wantVer := row.Version
			db.onHit("TranscodeTasks.UpdateStateTx", func() {
				// 真实写路径每次状态迁移都会 ++version（conditionalUpdate 恒带 version=version+1），
				// 所以「只改状态不改版本」的交错在生产里不存在，fake 也不许造。
				cur := mustTranscode(t, db, row.TaskId)
				cur.State = tc.toState
				cur.Version++
			})

			var exp int64 // 0 = 调用方不持版本（重放式指令），否则拿预读版本做 CAS
			if tc.useVersion {
				exp = wantVer
			}
			info, err := NewStopLiveTranscodeLogic(context.Background(), svcCtx).
				StopLiveTranscode(stopTranscodeReq(row.TaskId, exp))
			wantFail(t, info, err, tc.want, tc.name)
			wantCalls(t, db, "TranscodeTasks.UpdateStateTx", 0, 1, "CAS 只发生一次")
			wantCalls(t, db, "Outbox.Insert", 0, 0, "CAS 0 行不得尝试写事件")
			wantEvents(t, db, nil, tc.name)
			// 归因必须来自「回读那一行」，而不是 logic 自己猜：整条路径恰好两次主键读
			// （预读 + 0 行后的归因读）。交错后的最终库态只有真 MySQL 能证明（本 fake 的
			// 钩子改动脉落在回滚的事务里会一起被撤销），故不在此断言。
			wantCalls(t, db, "TranscodeTasks.FindOne", 0, 2, "0 行归因只回读一次")
		})
	}
}

// 事件写失败必须连状态推进一起回滚（AGENTS.md §5）。
func TestStopLiveTranscodeRollsBackWhenEventWriteFails(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateRunning, nil)
	snapshot := *mustTranscode(t, db, row.TaskId)
	db.failOn("Outbox.Insert", errOutboxDown)

	info, err := NewStopLiveTranscodeLogic(context.Background(), svcCtx).
		StopLiveTranscode(stopTranscodeReq(row.TaskId, 0))
	wantFail(t, info, err, errOutboxDown, "事件写失败")
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
	wantCalls(t, db, "TranscodeTasks.UpdateStateTx", 0, 1, "事务内确实尝试过 CAS")
	wantEvents(t, db, nil, "回滚后不得留下事件")
}

func TestStopLiveTranscodeValidatesInputAndWritesNothing(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.StopLiveTranscodeReq)
		want   error
	}{
		{"任务号为零", func(r *rpc.StopLiveTranscodeReq) { r.TaskId = 0 }, model.ErrTranscodeTaskNotFound},
		{"任务号为负", func(r *rpc.StopLiveTranscodeReq) { r.TaskId = -7 }, model.ErrTranscodeTaskNotFound},
		{"缺幂等键", func(r *rpc.StopLiveTranscodeReq) { r.RequestId = "   " }, model.ErrEmptyRequestID},
		{"幂等键超列宽",
			func(r *rpc.StopLiveTranscodeReq) { r.RequestId = strings.Repeat("r", maxRequestIDRunes+1) },
			model.ErrEmptyRequestID},
		{"越界停止原因", func(r *rpc.StopLiveTranscodeReq) { r.Reason = rpc.FailureReason(99) }, model.ErrInvalidTransition},
		{"负数原因", func(r *rpc.StopLiveTranscodeReq) { r.Reason = rpc.FailureReason(-1) }, model.ErrInvalidTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			seedTranscode(db, model.TranscodeStateRunning, nil)
			req := stopTranscodeReq(1, 0) // 库里真有行：被拒必须是因为入参，而不是「查无此行」
			tc.mutate(req)
			before := snapshotWrites(db)

			info, err := NewStopLiveTranscodeLogic(context.Background(), svcCtx).StopLiveTranscode(req)
			wantFail(t, info, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
			// 校验必须发生在读库之前：非法入参不该消耗一次主键查询。
			wantCalls(t, db, "TranscodeTasks.FindOne", 0, 0, tc.name)
		})
	}
}

// operator 是审计文本：带凭据时必须脱敏后才进事件（AGENTS.md §6）。
func TestStopLiveTranscodeRedactsCredentialBearingOperator(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateRunning, nil)
	req := stopTranscodeReq(row.TaskId, 0)
	req.Operator = "ops token=" + leakMarker

	info, err := NewStopLiveTranscodeLogic(context.Background(), svcCtx).StopLiveTranscode(req)
	info = wantOK(t, info, err, "脱敏操作人")
	if info.GetState() != rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPING {
		t.Fatalf("状态未推进：%v", info.GetState())
	}
	wantNoLeak(t, db, "operator 脱敏")
	p := eventPayload(t, eventAt(t, db, 0))
	op, _ := p["operator"].(string)
	if strings.Contains(op, leakMarker) || op == "" {
		t.Fatalf("事件里的 operator 未脱敏或缺失：%q", op)
	}
}

// ---------------------------------------------------------------- ReportLiveTranscodeProgress

// 首次 RUNNING：started_at 只记一次（Worker 重启后的第二次 RUNNING 不能把真实启动点往后推），
// 状态迁移才发事件。
func TestReportLiveTranscodeProgressFirstRunningSetsStartedAtAndEmitsEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	now := nowTS()
	row := seedTranscode(db, model.TranscodeStatePending, func(r *model.LiveTranscodeTask) {
		// heartbeat/timeout 成对写入且差 180 秒：这是登记时刻的超时快照，
		// 与配置里的 60 秒故意不同，用于证明心跳顺延不回读配置。
		r.StartedAt, r.HeartbeatAt, r.TimeoutAt, r.Attempt = 0, now-180, now, 1
	})
	req := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING)
	req.Progress = 5

	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
	info = wantOK(t, info, err, "上报 RUNNING")

	if info.GetState() != rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING {
		t.Fatalf("状态应推进到 RUNNING，实际 %v", info.GetState())
	}
	if info.GetStartedAt() < now {
		t.Fatalf("首次 RUNNING 必须写 started_at，实际 %d", info.GetStartedAt())
	}
	if info.GetProgress() != 5 || info.GetStoppedAt() != 0 {
		t.Errorf("进度应落库且不得写 stopped_at：progress=%d stopped_at=%d", info.GetProgress(), info.GetStoppedAt())
	}
	// timeout_at 用「登记时刻的无心跳秒数」快照顺延，不回读配置（配置 60 ≠ 快照 180）。
	if got := info.GetTimeoutAt() - info.GetHeartbeatAt(); got != 180 {
		t.Errorf("心跳顺延应沿用登记的 180 秒窗口，实际 %d", got)
	}
	if info.GetRequestId() != row.RequestId {
		t.Errorf("上报不得覆写 request_id：%q → %q", row.RequestId, info.GetRequestId())
	}
	wantEvents(t, db, []string{model.EventTypeTranscodeStateChanged}, "首次 RUNNING")
	p := eventPayload(t, eventAt(t, db, 0))
	wantField(t, "RUNNING 事件", "prev_state", toInt64(t, p, "prev_state"), int64(model.TranscodeStatePending))
	wantField(t, "RUNNING 事件", "state", toInt64(t, p, "state"), int64(model.TranscodeStateRunning))
	wantField(t, "RUNNING 事件", "progress", toInt64(t, p, "progress"), 5)
	wantField(t, "RUNNING 事件", "attempt", toInt64(t, p, "attempt"), 1)
	wantField(t, "RUNNING 事件", "bitrate_level", toInt64(t, p, "bitrate_level"), int64(testLevel))
	if worker, _ := p["worker_id"].(string); worker != "worker-a" {
		t.Errorf("worker_id 只进日志与事件（表里没有该列），必须能在此归因：%q", worker)
	}
	if _, ok := p["source_ref"]; ok {
		t.Errorf("事件 payload 不得带拉流地址：%v", p)
	}
	wantNoLeak(t, db, "首次 RUNNING 上报")
}

// RUNNING→RUNNING 只前进进度：走事务、发 UPDATE，但不发事件（转码健康度是高频采样，
// 逐次发事件会刷爆 Outbox）。
func TestReportLiveTranscodeProgressRunningToRunningAdvancesProgressWithoutEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	started := nowTS() - 600
	row := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) {
		r.StartedAt, r.Progress = started, 20
	})
	req := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING)
	req.Progress = 80

	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
	info = wantOK(t, info, err, "进度前进")

	if info.GetProgress() != 80 || info.GetState() != rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING {
		t.Fatalf("进度未落库：progress=%d state=%v", info.GetProgress(), info.GetState())
	}
	if info.GetStartedAt() != started {
		t.Errorf("第二次 RUNNING 不得把真实启动点往后推：%d → %d", started, info.GetStartedAt())
	}
	wantCalls(t, db, "TranscodeTasks.UpdateStateTx", 0, 1, "进度前进走事务内 CAS")
	wantCalls(t, db, "TranscodeTasks.UpdateState", 0, 0, "进度变化不能走非事务心跳分支")
	wantEvents(t, db, nil, "仅进度变化不得发事件")
}

// 不变量 3：心跳不能被版本卡死。同状态同进度的重投走「非事务 + 放弃版本校验」那条分支，
// 否则 Worker 每次心跳重试都要先重读 version，超时清扫会误杀正常任务。
func TestReportLiveTranscodeProgressHeartbeatOnlyAvoidsTransaction(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	now := nowTS()
	row := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) {
		r.Progress, r.HeartbeatAt, r.TimeoutAt = 55, now-40, now+20 // budget=60
		r.StartedAt = now - 300
	})
	req := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING)
	req.Progress = 55        // 与库里一致 → 纯心跳
	req.ExpectedVersion = 99 // 故意拿过期版本：心跳路径不校验版本，必须仍然成功

	before := snapshotWrites(db)
	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
	info = wantOK(t, info, err, "纯心跳")

	if info.GetHeartbeatAt() <= row.HeartbeatAt {
		t.Errorf("心跳必须推进 heartbeat_at：%d → %d", row.HeartbeatAt, info.GetHeartbeatAt())
	}
	if got := info.GetTimeoutAt() - info.GetHeartbeatAt(); got != 60 {
		t.Errorf("超时窗口应按登记的 60 秒顺延，实际 %d", got)
	}
	if info.GetStartedAt() != row.StartedAt {
		t.Errorf("心跳不得改动 started_at：%d → %d", row.StartedAt, info.GetStartedAt())
	}
	if info.GetProgress() != 55 {
		t.Errorf("心跳不得改动进度：%d", info.GetProgress())
	}
	// 副作用清单就是这条分支的全部契约：不开事务、只发一条非事务 CAS、一条事件都不写。
	// before 取在调用之前，因此下面四条 wantCalls 是在数「这一次调用产生的增量」。
	wantCalls(t, db, "DB.TransactCtx", before["DB.TransactCtx"], 0, "纯心跳不开事务")
	wantCalls(t, db, "TranscodeTasks.UpdateState", before["TranscodeTasks.UpdateState"], 1, "纯心跳只走非事务 CAS")
	wantCalls(t, db, "TranscodeTasks.UpdateStateTx", before["TranscodeTasks.UpdateStateTx"], 0, "纯心跳不得走事务 CAS")
	wantCalls(t, db, "Outbox.Insert", before["Outbox.Insert"], 0, "纯心跳不发事件")
	wantEvents(t, db, nil, "纯心跳")
	// 心跳本身也 ++version（conditionalUpdate 对该表恒带 version=version+1）：
	// 因此拿「心跳之前」的版本去做状态推进必然撞冲突 —— 这正是 invariant 3 存在的原因。
	if info.GetVersion() != row.Version+1 {
		t.Errorf("心跳恰好 ++version：%d → %d", row.Version, info.GetVersion())
	}

	stale := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPING)
	stale.Progress = 99                 // 真状态迁移：必须校验版本
	stale.ExpectedVersion = row.Version // 心跳已过时的那一版
	info, err = NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(stale)
	wantFail(t, info, err, model.ErrVersionConflict, "心跳后拿旧版本推状态")
}

// 心跳/进度上报的 reason 守卫：健康上报不带原因时，不得把已记录的原因抹成 UNSPECIFIED
// （重试判定与审计都靠它）。
func TestReportLiveTranscodeProgressKeepsRecordedReason(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) {
		r.Reason, r.Progress = model.ReasonSourceLost, 10
	})
	req := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING)
	req.Progress = 30 // 进度变化走状态分支：那里的 reason 守卫最容易被漏掉

	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
	info = wantOK(t, info, err, "不带原因的健康上报")

	if int32(info.GetReason()) != model.ReasonSourceLost {
		t.Fatalf("健康上报不得抹掉已记录的失败原因：%d", int32(info.GetReason()))
	}
	req2 := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING)
	req2.Progress = 40
	req2.Reason = rpc.FailureReason_FAILURE_REASON_CDN
	info, err = NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req2)
	info = wantOK(t, info, err, "带原因的上报")
	if int32(info.GetReason()) != model.ReasonCDN {
		t.Errorf("显式带上原因时必须覆盖：%d", int32(info.GetReason()))
	}
}

// 终态不可复活：STOPPED 的行不接受迟到的 RUNNING 心跳（会把它改回运行中），
// 但同态重放（Worker 重投同一条终态上报）必须返回原行且零写。
func TestReportLiveTranscodeProgressTerminalRules(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	stoppedAt := nowTS() - 10
	row := seedTranscode(db, model.TranscodeStateStopped, func(r *model.LiveTranscodeTask) {
		r.StoppedAt, r.Progress = stoppedAt, 100
	})
	snapshot := *mustTranscode(t, db, row.TaskId)
	before := snapshotWrites(db)

	late := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING)
	late.Progress = 60
	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(late)
	wantFail(t, info, err, model.ErrTerminalState, "终态行的迟到心跳")
	wantNoWrites(t, db, before, "终态行的迟到心跳")
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)

	replay := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPED)
	replay.Progress = 60 // 连进度都不一致：同态即重放，不做任何写入
	info, err = NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(replay)
	info = wantOK(t, info, err, "终态同值重放")
	if info.GetVersion() != snapshot.Version || info.GetStoppedAt() != stoppedAt {
		t.Fatalf("终态重放必须原样回那一行：version %d→%d stopped_at %d→%d",
			snapshot.Version, info.GetVersion(), stoppedAt, info.GetStoppedAt())
	}
	if info.GetProgress() != 100 {
		t.Errorf("重放不得改写进度：库里 100，响应 %d", info.GetProgress())
	}
	wantNoWrites(t, db, before, "终态同值重放")
	wantEvents(t, db, nil, "终态重放")

	// CANCELLED 同属终态（TranscodeTerminalStates），迟到的 FAILED 也不能把它改判。
	// 带上原因：入参门禁在终态判定之前，不带原因的 FAILED 只会撞上「FAILED 必须可归因」。
	other := seedTranscode(db, model.TranscodeStateCancelled, nil)
	tooLate := reportTranscodeReq(other.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_FAILED)
	tooLate.Reason = rpc.FailureReason_FAILURE_REASON_WORKER_CRASH
	info, err = NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(tooLate)
	wantFail(t, info, err, model.ErrTerminalState, "取消终态不接受失败改判")
}

// 非法迁移逐条取自迁移表：STOPPING 只能到 STOPPED/FAILED/CANCELLED，
// STOPPED→STOPPING（撤销停止指令）与 STOPPING→RUNNING（假装还在跑）都必须被拒。
func TestReportLiveTranscodeProgressRefusesIllegalEdges(t *testing.T) {
	cases := []struct {
		name   string
		from   int32
		target rpc.LiveTranscodeState
	}{
		{"PENDING 不能直接报 STOPPED", model.TranscodeStatePending, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPED},
		{"PENDING 不能直接报 STOPPING", model.TranscodeStatePending, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPING},
		{"RUNNING 不能直接报 CANCELLED", model.TranscodeStateRunning, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_CANCELLED},
		{"STOPPING 不能回到 RUNNING", model.TranscodeStateStopping, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING},
		{"STOPPING 不能撤销停止指令", model.TranscodeStateStopping, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPING},
		{"FAILED 只能由 Retry 回到 PENDING", model.TranscodeStateFailed, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedTranscode(db, tc.from, nil)
			snapshot := *mustTranscode(t, db, row.TaskId)
			before := snapshotWrites(db)

			req := reportTranscodeReq(row.TaskId, tc.target)
			req.Reason = rpc.FailureReason_FAILURE_REASON_SOURCE_LOST // 带原因也不行：迁移本身非法
			info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
			wantFail(t, info, err, model.ErrInvalidTransition, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
			assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
		})
	}
}

// PENDING 是登记态（只由 Start/Retry 写入），FAILED 必须可归因。
func TestReportLiveTranscodeProgressRejectsPendingTargetAndReasonlessFailed(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateRunning, nil)
	before := snapshotWrites(db)

	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).
		ReportLiveTranscodeProgress(reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_PENDING))
	wantFail(t, info, err, model.ErrInvalidTransition, "上报 PENDING")

	noReason := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_FAILED)
	info, err = NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(noReason)
	wantFail(t, info, err, model.ErrInvalidTransition, "FAILED 不带原因")
	if !strings.Contains(err.Error(), "concrete reason") {
		t.Errorf("错误必须说明「缺具体原因」，而不是只报非法迁移：%v", err)
	}
	wantNoWrites(t, db, before, "上报入参门禁")
	wantEvents(t, db, nil, "上报入参门禁")
}

// 越界进度夹取而非拒绝：拒绝只会让 Worker 无限重投。
func TestReportLiveTranscodeProgressClampsOutOfRangeProgress(t *testing.T) {
	cases := []struct {
		name string
		// 终态上报只能从 STOPPING 走（RUNNING→STOPPED 是非法迁移，见迁移表）。
		from   int32
		to     rpc.LiveTranscodeState
		got    int32
		want   int32
		reason rpc.FailureReason
	}{
		{"负进度", model.TranscodeStateStopping, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPED, -5, 0,
			rpc.FailureReason_FAILURE_REASON_MANUAL},
		{"超 100 的进度", model.TranscodeStateStopping, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPED, 140, 100,
			rpc.FailureReason_FAILURE_REASON_MANUAL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedTranscode(db, tc.from, nil)
			req := reportTranscodeReq(row.TaskId, tc.to)
			req.Progress = tc.got
			req.Reason = tc.reason
			info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
			info = wantOK(t, info, err, tc.name)
			if info.GetProgress() != tc.want {
				t.Errorf("进度应被夹到 %d，实际 %d", tc.want, info.GetProgress())
			}
			if int32(info.GetState()) != int32(tc.to) {
				t.Errorf("夹取不得妨碍状态推进：%v", info.GetState())
			}
			wantEvents(t, db, []string{model.EventTypeTranscodeStateChanged}, tc.name)
		})
	}

	// 夹取后与库里同值时，这次「状态迁移请求」会降级成纯心跳：既不开事务也不发事件。
	// 这条分支的价值在于证明夹取发生在判定之前，否则 Worker 重投同一条越界上报会被读成状态倒退。
	db2 := newStore()
	svcCtx2 := newTestSvc(db2)
	seeded := seedTranscode(db2, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) {
		r.Progress = 100
	})
	same := reportTranscodeReq(seeded.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING)
	same.Progress = 900
	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx2).ReportLiveTranscodeProgress(same)
	info = wantOK(t, info, err, "夹取后同值上报")
	if info.GetProgress() != 100 {
		t.Errorf("夹取后的同值上报不得改动进度：%d", info.GetProgress())
	}
	wantCalls(t, db2, "DB.TransactCtx", 0, 0, "夹取后同值上报不开事务")
	wantEvents(t, db2, nil, "夹取后同值上报")
}

// 终局上报（STOPPED / FAILED）写 stopped_at，FAILED 必须带原因且原因落库。
func TestReportLiveTranscodeProgressTerminalWrite(t *testing.T) {
	cases := []struct {
		name   string
		target rpc.LiveTranscodeState
		reason rpc.FailureReason
	}{
		{"收尾成功", rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPED, rpc.FailureReason_FAILURE_REASON_MANUAL},
		{"失败退出", rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_FAILED, rpc.FailureReason_FAILURE_REASON_SOURCE_LOST},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			now := nowTS()
			row := seedTranscode(db, model.TranscodeStateStopping, func(r *model.LiveTranscodeTask) {
				r.HeartbeatAt, r.TimeoutAt = now-30, now+30 // budget=60
				r.Reason = model.ReasonUnspecified
			})
			req := reportTranscodeReq(row.TaskId, tc.target)
			req.Reason = tc.reason
			req.Errno = 124
			req.ErrMsg = "ffmpeg exited"

			info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
			info = wantOK(t, info, err, tc.name)
			if int32(info.GetState()) != int32(tc.target) {
				t.Fatalf("目标态没落库：%v", info.GetState())
			}
			if info.GetStoppedAt() < now {
				t.Errorf("终态必须写 stopped_at，实际 %d", info.GetStoppedAt())
			}
			if int32(info.GetReason()) != int32(tc.reason) {
				t.Errorf("终态原因必须落库：%d", int32(info.GetReason()))
			}
			if info.GetErrno() != 124 || info.GetErrMsg() != "ffmpeg exited" {
				t.Errorf("errno/err_msg 必须落库：%d/%q", info.GetErrno(), info.GetErrMsg())
			}
			if got := info.GetTimeoutAt() - info.GetHeartbeatAt(); got != 60 {
				t.Errorf("终态上报同样顺延心跳（清扫器靠它判过期）：期望窗口 60，实际 %d", got)
			}
			wantEvents(t, db, []string{model.EventTypeTranscodeStateChanged}, tc.name)
			p := eventPayload(t, eventAt(t, db, 0))
			wantField(t, tc.name, "prev_state", toInt64(t, p, "prev_state"), int64(model.TranscodeStateStopping))
			wantField(t, tc.name, "reason", toInt64(t, p, "reason"), int64(tc.reason))
			wantField(t, tc.name, "errno", toInt64(t, p, "errno"), 124)
		})
	}
}

// err_msg 是 Worker 给的自由文本，常含签名拉流地址：入库前必须脱敏并按列宽截断。
func TestReportLiveTranscodeProgressSanitizesErrMsg(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateRunning, nil)
	req := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_FAILED)
	req.Reason = rpc.FailureReason_FAILURE_REASON_STORAGE
	req.ErrMsg = "pull " + leakSignedURL + "\n第二个换行\n" + strings.Repeat("x", 600)

	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
	info = wantOK(t, info, err, "err_msg 脱敏")
	wantNoLeak(t, db, "err_msg 脱敏")
	if strings.Contains(info.GetErrMsg(), leakMarker) {
		t.Fatalf("响应里回显了未脱敏的 err_msg：%q", info.GetErrMsg())
	}
	if runeLen(info.GetErrMsg()) > maxErrMsgRunes {
		t.Errorf("err_msg 未截断到列宽：%d > %d", runeLen(info.GetErrMsg()), maxErrMsgRunes)
	}
	if !strings.HasSuffix(info.GetErrMsg(), "…") {
		t.Errorf("截断必须留省略号，否则调用方会以为拿到了完整摘要：%q", info.GetErrMsg())
	}
	if strings.Contains(info.GetErrMsg(), "\n") {
		t.Errorf("err_msg 不得含换行（日志注入）：%q", info.GetErrMsg())
	}
}

func TestReportLiveTranscodeProgressVersionConflictAndRaceAttribution(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) { r.Progress = 1 })
	snapshot := *mustTranscode(t, db, row.TaskId)

	stale := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPING)
	stale.Progress = 2
	stale.ExpectedVersion = row.Version + 3
	stale.Reason = rpc.FailureReason_FAILURE_REASON_MANUAL
	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(stale)
	wantFail(t, info, err, model.ErrVersionConflict, "过期版本")
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
	wantEvents(t, db, nil, "过期版本")

	// CAS 前被抢先收尾：0 行必须回读归因成终态冲突，而不是「成功」。
	raced := seedTranscode(db, model.TranscodeStateStopping, func(r *model.LiveTranscodeTask) { r.Progress = 7 })
	db.onHit("TranscodeTasks.UpdateStateTx", func() {
		cur := mustTranscode(t, db, raced.TaskId)
		cur.State = model.TranscodeStateStopped
		cur.Version++
	})
	req := reportTranscodeReq(raced.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPED)
	req.Progress = 8
	info, err = NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
	wantFail(t, info, err, model.ErrTerminalState, "抢先收尾")
	wantCalls(t, db, "Outbox.Insert", 0, 0, "CAS 0 行不得尝试写事件")

	// 心跳分支的 0 行（状态被并发改掉）：本次心跳作废，交回调用方重判。
	lost := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) { r.Progress = 50 })
	db.onHit("TranscodeTasks.UpdateState", func() {
		cur := mustTranscode(t, db, lost.TaskId)
		cur.State = model.TranscodeStateStopping
		cur.Version++
	})
	hb := reportTranscodeReq(lost.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING)
	hb.Progress = 50
	info, err = NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(hb)
	wantFail(t, info, err, model.ErrInvalidTransition, "心跳撞并发状态迁移")
}

func TestReportLiveTranscodeProgressRollsBackWhenEventWriteFails(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) { r.Progress = 10 })
	snapshot := *mustTranscode(t, db, row.TaskId)
	db.failOn("Outbox.Insert", errOutboxDown)

	// 合法的目标态：这里要测的是「事件写失败连状态推进一起回滚」，
	// 不是迁移判定（RUNNING→STOPPED 是非法边，会被更早的门禁挡掉）。
	req := reportTranscodeReq(row.TaskId, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPING)
	req.Progress = 100
	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(req)
	wantFail(t, info, err, errOutboxDown, "事件写失败")
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
	wantEvents(t, db, nil, "事件写失败")
}

func TestReportLiveTranscodeProgressValidatesInputAndFailClosed(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)

	_, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).
		ReportLiveTranscodeProgress(reportTranscodeReq(0, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING))
	if err == nil || !strings.Contains(err.Error(), "task_id") {
		t.Fatalf("task_id<=0 必须被拒绝且不查库，实际 %v", err)
	}
	wantCalls(t, db, "TranscodeTasks.FindOne", 0, 0, "task_id<=0 不查库")

	unknown := reportTranscodeReq(1, rpc.LiveTranscodeState(9))
	info, err := NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).ReportLiveTranscodeProgress(unknown)
	wantFail(t, info, err, model.ErrInvalidTransition, "未知状态码")
	wantNoWrites(t, db, before, "上报入参门禁")

	// 查无此行与读故障必须是两种不同结果：不能把「读不动」伪装成「没有这行」。
	info, err = NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).
		ReportLiveTranscodeProgress(reportTranscodeReq(4242, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING))
	wantFail(t, info, err, model.ErrTranscodeTaskNotFound, "不存在的任务")

	db.failOn("TranscodeTasks.FindOne", errModelDown)
	info, err = NewReportLiveTranscodeProgressLogic(context.Background(), svcCtx).
		ReportLiveTranscodeProgress(reportTranscodeReq(1, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING))
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("读故障必须原样抛出（不得读成查无此行）：%v", err)
	}
	if !isNilPtr(info) {
		t.Errorf("依赖故障路径不得带回响应体：%+v", info)
	}
	wantNoWrites(t, db, before, "读故障 fail closed")
}

// ---------------------------------------------------------------- RetryLiveTranscode

// 重试是把 FAILED 拉回 PENDING 并「重新起一份预算」：attempt+1、清掉上一次的错误、
// 心跳窗口成对重置，但首因 reason 与登记幂等键 request_id 都不动。
func TestRetryLiveTranscodeResetsAttemptAndHeartbeatWindow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	now := nowTS()
	row := seedTranscode(db, model.TranscodeStateFailed, func(r *model.LiveTranscodeTask) {
		r.Attempt, r.MaxAttempts = 1, 3
		r.Reason, r.Errno, r.ErrMsg = model.ReasonTimeout, 7, "heartbeat timeout"
		r.HeartbeatAt, r.TimeoutAt = now-500, now-320 // budget=180，与配置的 60 不同
		r.StoppedAt, r.Progress = now-500, 66
	})
	before := *mustTranscode(t, db, row.TaskId)

	info, err := NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(retryTranscodeReq(row.TaskId))
	info = wantOK(t, info, err, "重试")

	if info.GetState() != rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_PENDING {
		t.Fatalf("重试必须回到 PENDING 等 Worker 领取，实际 %v", info.GetState())
	}
	if info.GetAttempt() != 2 {
		t.Errorf("attempt 必须 +1：%d → %d", before.Attempt, info.GetAttempt())
	}
	if info.GetErrno() != 0 || info.GetErrMsg() != "" {
		t.Errorf("重试必须清掉上一次失败证据，否则清扫器会把新任务当旧失败：%d/%q",
			info.GetErrno(), info.GetErrMsg())
	}
	if int32(info.GetReason()) != model.ReasonTimeout {
		t.Errorf("reason 列保留首次失败原因（重试说明只进事件），实际 %d", int32(info.GetReason()))
	}
	if info.GetRequestId() != before.RequestId {
		t.Errorf("重试不得覆写 uniq_request_id：%q → %q", before.RequestId, info.GetRequestId())
	}
	if info.GetProgress() != 66 {
		t.Errorf("重试不改进度（那是下一次上报的事实）：%d", info.GetProgress())
	}
	// heartbeat/timeout 必须成对写：只写 heartbeat 会让 timeout_at 停在过去的时刻，
	// 新任务刚拉起就被清扫器判超时。
	if info.GetHeartbeatAt() < now {
		t.Errorf("重试必须把心跳重置为本次时刻：%d", info.GetHeartbeatAt())
	}
	if budget := info.GetTimeoutAt() - info.GetHeartbeatAt(); budget != 180 {
		t.Errorf("heartbeat/timeout 必须成对写入并沿用登记的 180 秒窗口（timeoutBudget）：%d", budget)
	}
	wantEvents(t, db, []string{model.EventTypeTranscodeStateChanged}, "重试")
	p := eventPayload(t, eventAt(t, db, 0))
	wantField(t, "重试事件", "prev_state", toInt64(t, p, "prev_state"), int64(model.TranscodeStateFailed))
	wantField(t, "重试事件", "state", toInt64(t, p, "state"), int64(model.TranscodeStatePending))
	wantField(t, "重试事件", "attempt", toInt64(t, p, "attempt"), 2)
	wantField(t, "重试事件", "max_attempts", toInt64(t, p, "max_attempts"), 3)
	if op, _ := p["operator"].(string); op != "ops-bob" {
		t.Errorf("重试必须留操作人归因：%q", op)
	}
	if note, _ := p["note"].(string); !strings.Contains(note, "换节点重跑") {
		t.Errorf("重试说明必须进事件（库里没有该列）：%q", note)
	}
	wantNoLeak(t, db, "重试")
}

// 重放判定用 trace_id（request_id 不允许被状态推进覆写）：行已是 PENDING 且 trace 相同
// 即视为同一次重试的重投，不再 ++attempt、不再发事件。
func TestRetryLiveTranscodeReplayOnSameTraceIDDoesNotDoubleAttempt(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStatePending, func(r *model.LiveTranscodeTask) {
		r.Attempt, r.TraceId = 2, "trace-retry-tc-1"
	})
	before := snapshotWrites(db)
	snapshot := *mustTranscode(t, db, row.TaskId)

	req := retryTranscodeReq(row.TaskId)
	info, err := NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(req)
	info = wantOK(t, info, err, "重试重放")

	if info.GetAttempt() != 2 {
		t.Fatalf("同 trace_id 的重放不得 ++attempt：%d", info.GetAttempt())
	}
	if info.GetVersion() != snapshot.Version {
		t.Errorf("重放不得 ++version：%d → %d", snapshot.Version, info.GetVersion())
	}
	wantNoWrites(t, db, before, "重试重放")
	wantEvents(t, db, nil, "重试重放")

	// 换一个 trace_id 就是新一次重试：PENDING 行不允许再被重试（会绕过 attempt 预算），
	// 归因必须是非法迁移。
	req2 := retryTranscodeReq(row.TaskId)
	req2.TraceId = "trace-retry-tc-2"
	req2.RequestId = "req-retry-tc-2"
	info, err = NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(req2)
	wantFail(t, info, err, model.ErrInvalidTransition, "PENDING 行换 trace 再重试")
	wantNoWrites(t, db, before, "PENDING 行换 trace 再重试")
}

// 重试预算：attempt+1 > max_attempts 直接拒绝，不改库、不发事件
// （直播转码是常驻进程，无限拉起会打满机器）。
func TestRetryLiveTranscodeStopsAtMaxAttempts(t *testing.T) {
	cases := []struct {
		name        string
		attempt     int32
		maxAttempts int32
		wantErr     bool
		wantAttempt int32
	}{
		{"最后一次预算", 2, 3, false, 3},
		{"恰好打满", 3, 3, true, 3},
		{"脏数据已超预算", 5, 3, true, 5},
		{"max_attempts=0 视为无预算", 0, 0, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedTranscode(db, model.TranscodeStateFailed, func(r *model.LiveTranscodeTask) {
				r.Attempt, r.MaxAttempts = tc.attempt, tc.maxAttempts
			})
			before := snapshotWrites(db)

			info, err := NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(retryTranscodeReq(row.TaskId))
			if tc.wantErr {
				wantFail(t, info, err, model.ErrAttemptExhausted, tc.name)
				wantNoWrites(t, db, before, tc.name)
				wantEvents(t, db, nil, tc.name)
				if got := mustTranscode(t, db, row.TaskId).Attempt; got != tc.wantAttempt {
					t.Fatalf("预算耗尽却改了 attempt：%d → %d", tc.wantAttempt, got)
				}
				return
			}
			wantOK(t, info, err, tc.name)
			if info.GetAttempt() != tc.wantAttempt {
				t.Fatalf("attempt 应为 %d，实际 %d", tc.wantAttempt, info.GetAttempt())
			}
			wantEvents(t, db, []string{model.EventTypeTranscodeStateChanged}, tc.name)
		})
	}
}

// 只有 FAILED 能重试：终态报 ErrTerminalState，其余在跑/待跑的状态报非法迁移。
func TestRetryLiveTranscodeRefusesNonFailedStates(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		want  error
	}{
		{"运行中不可重试", model.TranscodeStateRunning, model.ErrInvalidTransition},
		{"停止中不可重试", model.TranscodeStateStopping, model.ErrInvalidTransition},
		{"终态 STOPPED", model.TranscodeStateStopped, model.ErrTerminalState},
		{"终态 CANCELLED", model.TranscodeStateCancelled, model.ErrTerminalState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedTranscode(db, tc.state, func(r *model.LiveTranscodeTask) { r.TraceId = "other-trace" })
			snapshot := *mustTranscode(t, db, row.TaskId)
			before := snapshotWrites(db)

			info, err := NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(retryTranscodeReq(row.TaskId))
			wantFail(t, info, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
			assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
		})
	}
}

// 审计门禁：operator 与 trace_id 都是重试的必需归因，缺一即拒且零副作用。
func TestRetryLiveTranscodeRequiresOperatorAndTraceID(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateFailed, nil)
	snapshot := *mustTranscode(t, db, row.TaskId)
	before := snapshotWrites(db)

	noOperator := retryTranscodeReq(row.TaskId)
	noOperator.Operator = "  "
	info, err := NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(noOperator)
	wantFail(t, info, err, model.ErrOperatorRequired, "缺操作人")
	// 缺陷 #6 的回归位：错误文本必须说的是 operator，不能借用语义完全不同的回收原因哨兵。
	if want := "operator required"; !strings.Contains(err.Error(), want) {
		t.Fatalf("错误文本必须说明缺 operator（含 %q），实际 %v", want, err)
	}

	noTrace := retryTranscodeReq(row.TaskId)
	noTrace.TraceId = ""
	info, err = NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(noTrace)
	wantFail(t, info, err, model.ErrEmptyRequestID, "缺 trace_id")
	if !strings.Contains(err.Error(), "trace_id") {
		t.Errorf("错误文本必须点名 trace_id：%v", err)
	}

	tooLong := retryTranscodeReq(row.TaskId)
	tooLong.Operator = strings.Repeat("x", maxOperatorRunes+1)
	info, err = NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(tooLong)
	// operator 不落库、只进事件，但超列宽仍必须拒绝而不是静默截断：
	// 截断后的名字可能是另一个人，审计归因一旦被伪造就找不回真实操作者。
	if err == nil || !strings.Contains(err.Error(), "operator too long") {
		t.Fatalf("operator 超列宽必须被拒绝：%v", err)
	}

	longNote := retryTranscodeReq(row.TaskId)
	longNote.Reason = strings.Repeat("说", maxReasonRunes+1)
	info, err = NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(longNote)
	if err == nil || !strings.Contains(err.Error(), "retry note") {
		t.Fatalf("重试说明超列宽必须被拒绝而不是截断：%v", err)
	}

	noKey := retryTranscodeReq(row.TaskId)
	noKey.RequestId = ""
	info, err = NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(noKey)
	wantFail(t, info, err, model.ErrEmptyRequestID, "缺幂等键")

	wantNoWrites(t, db, before, "重试审计门禁")
	wantEvents(t, db, nil, "重试审计门禁")
	// 五道门禁都在读库之前，因此那一行必须与种子时刻逐字节相同。
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
	wantCalls(t, db, "TranscodeTasks.FindOne", 0, 0, "审计门禁不得消耗主键读")
}

// 登记快照被写坏（heartbeat/timeout 不成对，budget<=0）时，重试必须回落到配置默认值，
// 否则新任务永远处于「不判超时」的裸奔状态。
func TestRetryLiveTranscodeFallsBackToConfiguredTimeoutWhenBudgetBroken(t *testing.T) {
	cases := []struct {
		name        string
		heartbeatAt int64
		timeoutAt   int64
		wantBudget  int64
	}{
		{"从未有心跳", 0, 0, 60},
		{"超时点早于心跳点（脏数据）", nowTS(), nowTS() - 10, 60},
		{"预算完好则不读配置", nowTS() - 200, nowTS() - 200 + 120, 120},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedTranscode(db, model.TranscodeStateFailed, func(r *model.LiveTranscodeTask) {
				r.HeartbeatAt, r.TimeoutAt = tc.heartbeatAt, tc.timeoutAt
			})
			info, err := NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(retryTranscodeReq(row.TaskId))
			info = wantOK(t, info, err, tc.name)
			if got := info.GetTimeoutAt() - info.GetHeartbeatAt(); got != tc.wantBudget {
				t.Fatalf("重试后的无心跳窗口应为 %d，实际 %d", tc.wantBudget, got)
			}
		})
	}
}

func TestRetryLiveTranscodeMissingRowAndVersionConflict(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)
	info, err := NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(retryTranscodeReq(4242))
	wantFail(t, info, err, model.ErrTranscodeTaskNotFound, "不存在的任务")
	wantNoWrites(t, db, before, "不存在的任务")

	row := seedTranscode(db, model.TranscodeStateFailed, nil)
	snapshot := *mustTranscode(t, db, row.TaskId)
	req := retryTranscodeReq(row.TaskId)
	req.ExpectedVersion = row.Version + 2
	info, err = NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(req)
	wantFail(t, info, err, model.ErrVersionConflict, "过期版本")
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
	wantEvents(t, db, nil, "过期版本")

	db.failOn("Outbox.Insert", errOutboxDown)
	info, err = NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(retryTranscodeReq(row.TaskId))
	wantFail(t, info, err, errOutboxDown, "事件写失败")
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
}

// 重试说明同样是自由文本：凭据必须脱敏后才进事件。
func TestRetryLiveTranscodeRedactsAuditNote(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateFailed, nil)
	req := retryTranscodeReq(row.TaskId)
	req.Reason = "重试，源地址 " + leakSignedURL

	info, err := NewRetryLiveTranscodeLogic(context.Background(), svcCtx).RetryLiveTranscode(req)
	wantOK(t, info, err, "重试说明脱敏")
	wantNoLeak(t, db, "重试说明脱敏")
	p := eventPayload(t, eventAt(t, db, 0))
	note, _ := p["note"].(string)
	if strings.Contains(note, leakMarker) || note == "" {
		t.Fatalf("事件里的重试说明未脱敏或缺失：%q", note)
	}
}

// ---------------------------------------------------------------- CancelLiveTranscode

func TestCancelLiveTranscodeFromPendingWritesTerminalState(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	now := nowTS()
	row := seedTranscode(db, model.TranscodeStatePending, func(r *model.LiveTranscodeTask) {
		r.HeartbeatAt, r.TimeoutAt = now-100, now-40
	})
	before := *mustTranscode(t, db, row.TaskId)

	info, err := NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(cancelTranscodeReq(row.TaskId))
	info = wantOK(t, info, err, "取消未拉起的任务")

	if info.GetState() != rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_CANCELLED {
		t.Fatalf("PENDING 可直接取消，实际 %v", info.GetState())
	}
	if info.GetVersion() != before.Version+1 {
		t.Errorf("恰好 ++version：%d → %d", before.Version, info.GetVersion())
	}
	if info.GetStoppedAt() < now {
		t.Errorf("取消必须记 stopped_at（终态行没有心跳，审计靠它排序），实际 %d", info.GetStoppedAt())
	}
	if int32(info.GetReason()) != model.ReasonManual {
		t.Errorf("取消原因必须落库：%d", int32(info.GetReason()))
	}
	if info.GetRequestId() != before.RequestId {
		t.Errorf("取消不得覆写 request_id：%q → %q", before.RequestId, info.GetRequestId())
	}
	// 取消不顺延超时窗口：终态行不该再被清扫器按心跳判定。
	committed := mustTranscode(t, db, row.TaskId)
	if committed.HeartbeatAt != before.HeartbeatAt || committed.TimeoutAt != before.TimeoutAt {
		t.Errorf("取消不得改动 heartbeat/timeout：%+v", *committed)
	}
	wantEvents(t, db, []string{model.EventTypeTranscodeStateChanged}, "取消")
	p := eventPayload(t, eventAt(t, db, 0))
	wantField(t, "取消事件", "prev_state", toInt64(t, p, "prev_state"), int64(model.TranscodeStatePending))
	wantField(t, "取消事件", "state", toInt64(t, p, "state"), int64(model.TranscodeStateCancelled))
	if op, _ := p["operator"].(string); op != "ops-carol" {
		t.Errorf("取消必须留操作人：%q", op)
	}
}

// STOPPING 也允许取消（Worker 迟迟不收尾时运营要有终态出口），RUNNING/FAILED 则必须先走
// Stop / Retry —— 直接取消 RUNNING 会把「库里已取消、对象存储还在长切片」的孤儿留下。
func TestCancelLiveTranscodeAllowedAndRefusedStates(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		want  error
	}{
		{"STOPPING 可取消", model.TranscodeStateStopping, nil},
		{"RUNNING 必须先 Stop", model.TranscodeStateRunning, model.ErrInvalidTransition},
		{"FAILED 只能 Retry", model.TranscodeStateFailed, model.ErrInvalidTransition},
		{"终态 STOPPED 不可取消", model.TranscodeStateStopped, model.ErrTerminalState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedTranscode(db, tc.state, nil)
			snapshot := *mustTranscode(t, db, row.TaskId)
			before := snapshotWrites(db)

			info, err := NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(cancelTranscodeReq(row.TaskId))
			if tc.want != nil {
				wantFail(t, info, err, tc.want, tc.name)
				wantNoWrites(t, db, before, tc.name)
				wantEvents(t, db, nil, tc.name)
				assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
				return
			}
			wantOK(t, info, err, tc.name)
			if info.GetState() != rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_CANCELLED {
				t.Fatalf("未推进：%v", info.GetState())
			}
			wantEvents(t, db, []string{model.EventTypeTranscodeStateChanged}, tc.name)
		})
	}
}

// 取消是终态写入：已是 CANCELLED 的行原样返回（重放不再 ++version、不再发事件）。
func TestCancelLiveTranscodeReplayOnCancelledIsSideEffectFree(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateCancelled, func(r *model.LiveTranscodeTask) {
		r.Reason, r.StoppedAt = model.ReasonManual, nowTS()-50
	})
	snapshot := *mustTranscode(t, db, row.TaskId)
	before := snapshotWrites(db)

	req := cancelTranscodeReq(row.TaskId)
	req.Reason = rpc.FailureReason_FAILURE_REASON_TIMEOUT // 重放带来不同原因：必须被忽略
	info, err := NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(req)
	info = wantOK(t, info, err, "取消重放")

	if info.GetVersion() != snapshot.Version {
		t.Errorf("重放不得 ++version：%d → %d", snapshot.Version, info.GetVersion())
	}
	if int32(info.GetReason()) != model.ReasonManual {
		t.Errorf("重放不得覆写终态原因：%d", int32(info.GetReason()))
	}
	wantNoWrites(t, db, before, "取消重放")
	wantEvents(t, db, nil, "取消重放")
}

// 当前实现口径：停止/取消的归因总是一次覆写 reason（包括写成 UNSPECIFIED 0），
// 与 Report 的「只带原因才覆写」刻意不同 —— 停止类指令是「谁最后动了它」的事实源。
// 本用例钉住现状：若将来改成保留首因，这里会红并提示同步改注释。
func TestCancelLiveTranscodeReasonAlwaysOverwritten(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStatePending, func(r *model.LiveTranscodeTask) {
		r.Reason = model.ReasonTimeout
	})
	req := cancelTranscodeReq(row.TaskId)
	req.Reason = rpc.FailureReason_FAILURE_REASON_UNSPECIFIED
	info, err := NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(req)
	info = wantOK(t, info, err, "取消不带原因")
	if int32(info.GetReason()) != model.ReasonUnspecified {
		t.Fatalf("取消路径的 reason 是无条件覆写（proto 未强制原因），实际 %d", int32(info.GetReason()))
	}
	if got := mustTranscode(t, db, row.TaskId).TraceId; got != "trace-cancel-tc-1" {
		t.Errorf("取消必须写归因 trace_id，实际 %q", got)
	}
}

func TestCancelLiveTranscodeValidatesAndRollsBack(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)

	_, err := NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(cancelTranscodeReq(0))
	if err == nil || !strings.Contains(err.Error(), "task_id") {
		t.Fatalf("task_id<=0 必须被拒绝且不查库，实际 %v", err)
	}
	wantCalls(t, db, "TranscodeTasks.FindOne", 0, 0, "task_id<=0 不查库")

	row := seedTranscode(db, model.TranscodeStatePending, nil)
	noOperator := cancelTranscodeReq(row.TaskId)
	noOperator.Operator = ""
	info, err := NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(noOperator)
	wantFail(t, info, err, model.ErrOperatorRequired, "缺操作人")

	badReason := cancelTranscodeReq(row.TaskId)
	badReason.Reason = rpc.FailureReason(42)
	info, err = NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(badReason)
	wantFail(t, info, err, model.ErrInvalidTransition, "越界原因")

	noKey := cancelTranscodeReq(row.TaskId)
	noKey.RequestId = "req-too-long-" + strings.Repeat("z", maxRequestIDRunes)
	info, err = NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(noKey)
	wantFail(t, info, err, model.ErrEmptyRequestID, "幂等键超列宽")
	wantNoWrites(t, db, before, "取消入参门禁")
	wantEvents(t, db, nil, "取消入参门禁")

	// 版本冲突与事件回滚
	snapshot := *mustTranscode(t, db, row.TaskId)
	stale := cancelTranscodeReq(row.TaskId)
	stale.ExpectedVersion = snapshot.Version + 9
	info, err = NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(stale)
	wantFail(t, info, err, model.ErrVersionConflict, "过期版本")
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)

	db.failOn("Outbox.Insert", errOutboxDown)
	info, err = NewCancelLiveTranscodeLogic(context.Background(), svcCtx).CancelLiveTranscode(cancelTranscodeReq(row.TaskId))
	wantFail(t, info, err, errOutboxDown, "事件写失败")
	assertTranscodeRowUnchanged(t, db, row.TaskId, snapshot)
}

// ---------------------------------------------------------------- GetLiveTranscodeTask

// 投影逐列取自 live_transcode_task：每个字段给一个互不相同的值，任何一列串位都会当场报出来。
func TestGetLiveTranscodeTaskProjectsEveryColumn(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seed := &model.LiveTranscodeTask{
		TaskId: 0, RoomId: 71001, LiveSession: 88001, TemplateId: 501,
		BitrateLevel: 3, Protocol: 2, SourceRef: "rtmp://origin.example.com/live/a",
		AnchorMid: 42001, State: model.TranscodeStateRunning, Progress: 41,
		Attempt: 2, MaxAttempts: 5, StartedAt: 1700000101, StoppedAt: 1700000202,
		HeartbeatAt: 1700000303, TimeoutAt: 1700000404, Version: 7,
		Reason: model.ReasonSourceLost, Errno: 121, ErrMsg: "boom",
		RequestId: "req-proj-1", TraceId: "trace-proj-1", Ctime: 1700000505, Mtime: 1700000606,
	}
	seed.TaskId = db.next("task")
	db.transcodes[seed.TaskId] = seed
	before := snapshotWrites(db)

	info, err := NewGetLiveTranscodeTaskLogic(context.Background(), svcCtx).
		GetLiveTranscodeTask(&rpc.LiveTranscodeTaskReq{TaskId: seed.TaskId})
	info = wantOK(t, info, err, "查详情")

	wantField(t, "投影", "task_id", info.GetTaskId(), seed.TaskId)
	wantField(t, "投影", "room_id", info.GetRoomId(), seed.RoomId)
	wantField(t, "投影", "live_session_id", info.GetLiveSessionId(), seed.LiveSession)
	wantField(t, "投影", "template_id", info.GetTemplateId(), seed.TemplateId)
	wantField(t, "投影", "bitrate_level", int32(info.GetBitrateLevel()), seed.BitrateLevel)
	wantField(t, "投影", "protocol", int32(info.GetProtocol()), seed.Protocol)
	wantField(t, "投影", "source_ref", info.GetSourceRef(), seed.SourceRef)
	wantField(t, "投影", "anchor_mid", info.GetAnchorMid(), seed.AnchorMid)
	wantField(t, "投影", "state", int32(info.GetState()), seed.State)
	wantField(t, "投影", "progress", info.GetProgress(), seed.Progress)
	wantField(t, "投影", "attempt", info.GetAttempt(), seed.Attempt)
	wantField(t, "投影", "max_attempts", info.GetMaxAttempts(), seed.MaxAttempts)
	wantField(t, "投影", "started_at", info.GetStartedAt(), seed.StartedAt)
	wantField(t, "投影", "stopped_at", info.GetStoppedAt(), seed.StoppedAt)
	wantField(t, "投影", "heartbeat_at", info.GetHeartbeatAt(), seed.HeartbeatAt)
	wantField(t, "投影", "timeout_at", info.GetTimeoutAt(), seed.TimeoutAt)
	wantField(t, "投影", "version", info.GetVersion(), seed.Version)
	wantField(t, "投影", "reason", int32(info.GetReason()), seed.Reason)
	wantField(t, "投影", "errno", info.GetErrno(), seed.Errno)
	wantField(t, "投影", "err_msg", info.GetErrMsg(), seed.ErrMsg)
	wantField(t, "投影", "request_id", info.GetRequestId(), seed.RequestId)
	wantField(t, "投影", "trace_id", info.GetTraceId(), seed.TraceId)
	wantField(t, "投影", "ctime", info.GetCtime(), seed.Ctime)
	wantField(t, "投影", "mtime", info.GetMtime(), seed.Mtime)

	wantNoWrites(t, db, before, "详情是只读方法")
}

// task_id<=0 是「未指定」的占位值：直接判不存在且不查库，
// 否则参数错误会被伪装成一次正常的空结果。
func TestGetLiveTranscodeTaskRejectsBadIDAndMissingRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)

	for _, id := range []int64{0, -1} {
		info, err := NewGetLiveTranscodeTaskLogic(context.Background(), svcCtx).
			GetLiveTranscodeTask(&rpc.LiveTranscodeTaskReq{TaskId: id})
		wantFail(t, info, err, model.ErrTranscodeTaskNotFound, "非正数 task_id")
		wantCalls(t, db, "TranscodeTasks.FindOne", 0, 0, "非正数 task_id 不查库")
	}
	info, err := NewGetLiveTranscodeTaskLogic(context.Background(), svcCtx).
		GetLiveTranscodeTask(&rpc.LiveTranscodeTaskReq{TaskId: 4242})
	wantFail(t, info, err, model.ErrTranscodeTaskNotFound, "查无此行")
	wantNoWrites(t, db, before, "详情只读")

	db.failOn("TranscodeTasks.FindOne", errModelDown)
	info, err = NewGetLiveTranscodeTaskLogic(context.Background(), svcCtx).
		GetLiveTranscodeTask(&rpc.LiveTranscodeTaskReq{TaskId: 1})
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("读故障必须原样抛出（不得读成查无此行）：%v", err)
	}
	if !isNilPtr(info) {
		t.Errorf("故障路径不得带回响应体：%+v", info)
	}
}

// Cache 恒为 nil（等价线上未配 Redis）：终态行也必须每次回源，不能读到陈旧副本。
func TestGetLiveTranscodeTaskAlwaysReadsMainTableWithoutCache(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedTranscode(db, model.TranscodeStateStopped, nil)
	l := NewGetLiveTranscodeTaskLogic(context.Background(), svcCtx)

	for i := 0; i < 3; i++ {
		info, err := l.GetLiveTranscodeTask(&rpc.LiveTranscodeTaskReq{TaskId: row.TaskId})
		info = wantOK(t, info, err, "重复读终态")
		if info.GetState() != rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_STOPPED {
			t.Fatalf("第 %d 次读到状态 %v", i, info.GetState())
		}
	}
	wantCalls(t, db, "TranscodeTasks.FindOne", 0, 3, "无缓存时每次都要回源主表")
}

// ---------------------------------------------------------------- ListLiveTranscodeTasks

func listTranscodeReq(pn, ps int32) *rpc.ListLiveTranscodeTasksReq {
	return &rpc.ListLiveTranscodeTasksReq{
		RoomId: testRoomID, Page: &rpc.PageParam{Pn: pn, Ps: ps},
	}
}

func TestListLiveTranscodeTasksPassesNormalizedFiltersToModel(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedTranscode(db, model.TranscodeStateRunning, nil)
	before := snapshotWrites(db)

	in := listTranscodeReq(3, 10)
	in.LiveSessionId = testSession2
	in.TemplateId = testTemplate
	in.State = rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING
	reply, err := NewListLiveTranscodeTasksLogic(context.Background(), svcCtx).ListLiveTranscodeTasks(in)
	reply = wantOK(t, reply, err, "列表过滤")

	ff := db.lastTranscodeList
	if ff == nil {
		t.Fatal("model 未被调用，过滤条件无从校验")
	}
	wantField(t, "列表口径", "room_id", ff.RoomId, testRoomID)
	wantField(t, "列表口径", "session_id", ff.SessionId, testSession2)
	wantField(t, "列表口径", "template_id", ff.TemplateId, testTemplate)
	wantField(t, "列表口径", "state", ff.State, int32(model.TranscodeStateRunning))
	wantField(t, "列表口径", "pn", ff.Pn, int32(3))
	wantField(t, "列表口径", "ps", ff.Ps, int32(10))
	wantField(t, "列表口径", "max_page_size", ff.MaxPageSize, int32(testConf().MaxListPageSize))
	if reply.GetPage().GetTotal() != 0 || len(reply.GetTasks()) != 0 {
		t.Errorf("该过滤条件下确实没有行：total=%d len=%d", reply.GetPage().GetTotal(), len(reply.GetTasks()))
	}
	wantNoWrites(t, db, before, "列表只读")
}

// 未知状态过滤值必须报错，而不是退化成「恒空结果集」——
// 那会把调用方的枚举版本错误读成「这个房间没有任务」。
func TestListLiveTranscodeTasksRejectsUnknownStateFilter(t *testing.T) {
	for _, st := range []rpc.LiveTranscodeState{rpc.LiveTranscodeState(0), rpc.LiveTranscodeState(7), rpc.LiveTranscodeState(-1)} {
		name := strconv.Itoa(int(st))
		t.Run("state="+name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			seedTranscode(db, model.TranscodeStateRunning, nil)
			before := snapshotWrites(db)
			in := listTranscodeReq(1, 20)
			in.State = st

			reply, err := NewListLiveTranscodeTasksLogic(context.Background(), svcCtx).ListLiveTranscodeTasks(in)
			if st == 0 {
				wantOK(t, reply, err, "UNSPECIFIED 表示不过滤")
				if db.lastTranscodeList.State != 0 {
					t.Errorf("UNSPECIFIED 必须折算成「不过滤」，实际传给 model 的 state=%d", db.lastTranscodeList.State)
				}
				if len(reply.GetTasks()) != 1 {
					t.Errorf("不过滤应能看到那一行，实际 %d", len(reply.GetTasks()))
				}
				return
			}
			wantFail(t, reply, err, model.ErrInvalidTransition, "越界状态过滤")
			wantNoWrites(t, db, before, "越界状态过滤")
			if db.lastTranscodeList != nil {
				t.Errorf("越界过滤值不得被带进 SQL：%+v", *db.lastTranscodeList)
			}
		})
	}
}

// ps 口径（proto PageParam 注释 + README §8.7）：越界不报错，静默取默认 20（不是夹到上限 50）；
// pn 深到 OFFSET 超出保护窗口才拒绝。
func TestListLiveTranscodeTasksClampsPageSizeAndRejectsDeepPage(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	for i := 0; i < 25; i++ {
		seedTranscode(db, model.TranscodeStateStopped, nil)
	}
	l := NewListLiveTranscodeTasksLogic(context.Background(), svcCtx)

	cases := []struct {
		name     string
		pn, ps   int32
		wantPn   int32
		wantPS   int32
		wantRows int
		wantErr  bool
	}{
		{"ps 超上限取默认 20", 1, 999, 1, 20, 20, false},
		{"ps=0 取默认 20", 1, 0, 1, 20, 20, false},
		{"ps 负数取默认 20", 1, -5, 1, 20, 20, false},
		{"ps 恰好等于上限原样通过", 1, 50, 1, 50, 25, false},
		{"pn=0 视为 1", 0, 10, 1, 10, 10, false},
		// 深翻页边界按 maxListOffset 反推，不写死数字：常量一改这条用例就跟着动。
		{"OFFSET 恰好等于保护窗口仍允许", int32(maxListOffset/50) + 1, 50, int32(maxListOffset/50) + 1, 50, 0, false},
		{"OFFSET 超出保护窗口拒绝", int32(maxListOffset/50) + 2, 50, 0, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshotWrites(db)
			reply, err := l.ListLiveTranscodeTasks(listTranscodeReq(tc.pn, tc.ps))
			// 读侧无论如何都不得触到写路径（before 取在调用之前，这条才有失败能力）。
			wantNoWrites(t, db, before, tc.name)
			if tc.wantErr {
				wantFail(t, reply, err, model.ErrInvalidPage, tc.name)
				return
			}
			wantOK(t, reply, err, tc.name)
			if db.lastTranscodeList.Pn != tc.wantPn || db.lastTranscodeList.Ps != tc.wantPS {
				t.Fatalf("归一后的分页没传对：pn=%d ps=%d，期望 pn=%d ps=%d",
					db.lastTranscodeList.Pn, db.lastTranscodeList.Ps, tc.wantPn, tc.wantPS)
			}
			if len(reply.GetTasks()) != tc.wantRows {
				t.Fatalf("行数=%d，期望 %d", len(reply.GetTasks()), tc.wantRows)
			}
			if reply.GetPage().GetTotal() != 25 {
				t.Errorf("越界页也要回真实 total（客户端据此收敛），实际 %d", reply.GetPage().GetTotal())
			}
		})
	}
}

// 排序固定 task_id DESC（新任务优先），调用方无法把 order by 传进 SQL。
func TestListLiveTranscodeTasksOrderIsTaskIDDesc(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	first := seedTranscode(db, model.TranscodeStateRunning, nil)
	second := seedTranscode(db, model.TranscodeStateRunning, nil)
	seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) { r.RoomId = testRoomOther })

	reply, err := NewListLiveTranscodeTasksLogic(context.Background(), svcCtx).
		ListLiveTranscodeTasks(listTranscodeReq(1, 20))
	reply = wantOK(t, reply, err, "列表排序")

	if len(reply.GetTasks()) != 2 {
		t.Fatalf("房间过滤失效：返回 %d 行", len(reply.GetTasks()))
	}
	wantField(t, "排序", "第 1 行", reply.GetTasks()[0].GetTaskId(), second.TaskId)
	wantField(t, "排序", "第 2 行", reply.GetTasks()[1].GetTaskId(), first.TaskId)
	if reply.GetTasks()[0].GetRoomId() != testRoomID {
		t.Errorf("响应里的房间号串了：%d", reply.GetTasks()[0].GetRoomId())
	}
}

// ---------------------------------------------------------------- 缺陷 #5 的回归门禁

// 缺陷 #5（本轮补测试时发现，已修）：StopLiveTranscode / RetryLiveTranscode /
// CancelLiveTranscode 与 ReportLiveTranscodeProgress 的心跳分支在提交后回读自己那一行时，
// 只判了 err，没有判「读到了但库里已经没有这一行」。
//
// 为什么这算缺陷而不是风格问题：model 的 FindOne 契约是「查无此行返回 (nil, nil)」
// （live_transcode_task.go:200 的 ErrNotFound 分支），同包其它每个写方法都在提交后
// 补了 `if latest == nil { return nil, <哨兵> }`（ReportLiveTranscodeProgress 的主路径 188 行、
// OfflineStreamOutput 两处、UpsertStreamOutput 一处）。缺这道门禁时函数返回
// (typed nil, nil)：gRPC 侧状态码是 OK 而响应体是空消息，调用方会把「已经成功的写」
// 读成「一个 task_id=0 的任务」——主键 0 恰好是 Stop/Get/List 明确拒绝的非法值。
// 触发条件不是纯理论：事务提交与回读之间读不到，正是「从库/代理路由到旧副本」
// 与「并发删除」这两个真实场景。
func TestTranscodeWriteMethodsNeverReturnEmptyReplyOnMissingReread(t *testing.T) {
	cases := []struct {
		name string
		// seed 起跑状态，wantState 成功推进后的目标态：用来证明「写确实提交了」，
		// 否则这条门禁就退化成「反正失败了返回空也没关系」。
		seed      int32
		wantState int32
		// wantEvents 推进成功应有事件；心跳分支没有（见 Report 的 invariant 3）。
		// 这道门禁不得把「提交成功但回读为空」误判成回滚，所以事件也必须按原口径留下。
		wantEvents []string
		// arm 构造「提交后回读读不到」：在最后一次写发生的那一刻布下 missOnce，
		// 于是被 miss 掉的一定是回读那一次（预读发生在布防之前）。
		arm  func(db *store)
		call func(ctx context.Context, s *svc.ServiceContext, taskID int64) (any, error)
	}{
		{
			name: "StopLiveTranscode", seed: model.TranscodeStateRunning, wantState: model.TranscodeStateStopping,
			wantEvents: []string{model.EventTypeTranscodeStateChanged},
			arm: func(db *store) {
				db.onHit("TranscodeTasks.UpdateStateTx", func() { db.missOnce("TranscodeTasks.FindOne") })
			},
			call: func(ctx context.Context, s *svc.ServiceContext, id int64) (any, error) {
				info, err := NewStopLiveTranscodeLogic(ctx, s).StopLiveTranscode(stopTranscodeReq(id, 0))
				return info, err
			},
		},
		{
			name: "RetryLiveTranscode", seed: model.TranscodeStateFailed, wantState: model.TranscodeStatePending,
			wantEvents: []string{model.EventTypeTranscodeStateChanged},
			arm: func(db *store) {
				db.onHit("TranscodeTasks.UpdateStateTx", func() { db.missOnce("TranscodeTasks.FindOne") })
			},
			call: func(ctx context.Context, s *svc.ServiceContext, id int64) (any, error) {
				info, err := NewRetryLiveTranscodeLogic(ctx, s).RetryLiveTranscode(retryTranscodeReq(id))
				return info, err
			},
		},
		{
			name: "CancelLiveTranscode", seed: model.TranscodeStatePending, wantState: model.TranscodeStateCancelled,
			wantEvents: []string{model.EventTypeTranscodeStateChanged},
			arm: func(db *store) {
				db.onHit("TranscodeTasks.UpdateStateTx", func() { db.missOnce("TranscodeTasks.FindOne") })
			},
			call: func(ctx context.Context, s *svc.ServiceContext, id int64) (any, error) {
				info, err := NewCancelLiveTranscodeLogic(ctx, s).CancelLiveTranscode(cancelTranscodeReq(id))
				return info, err
			},
		},
		{
			name: "ReportLiveTranscodeProgress 心跳分支", seed: model.TranscodeStateRunning, wantState: model.TranscodeStateRunning,
			wantEvents: nil, // 纯心跳本来就不发事件
			arm: func(db *store) {
				db.onHit("TranscodeTasks.UpdateState", func() { db.missOnce("TranscodeTasks.FindOne") })
			},
			call: func(ctx context.Context, s *svc.ServiceContext, id int64) (any, error) {
				// 同状态同进度 = 纯心跳：这条分支的进度必须与种子里的 Progress 一致。
				info, err := NewReportLiveTranscodeProgressLogic(ctx, s).
					ReportLiveTranscodeProgress(reportTranscodeReq(id, rpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING))
				return info, err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedTranscode(db, tc.seed, func(r *model.LiveTranscodeTask) { r.Progress = 55 })
			tc.arm(db)

			reply, err := tc.call(context.Background(), svcCtx, row.TaskId)
			if err == nil && isNilPtr(reply) {
				t.Fatalf("缺陷 #5：%s 写已提交但回读为空时返回 (nil, nil)，"+
					"gRPC 会把它编成 OK + 空消息（task_id=0），调用方读到的是不存在的那一行", tc.name)
			}
			wantFail(t, reply, err, model.ErrTranscodeTaskNotFound, tc.name+" 回读为空")
			if got := mustTranscode(t, db, row.TaskId).State; got != tc.wantState {
				t.Errorf("这道门禁不该回滚已提交的状态推进：期望 %d，实际 %d", tc.wantState, got)
			}
			wantEvents(t, db, tc.wantEvents, tc.name+" 的登记事件不因门禁而丢失")
		})
	}
}
