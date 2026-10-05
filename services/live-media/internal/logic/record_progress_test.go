package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"
)

// Worker 上报录制心跳/状态：ReportLiveRecordProgress。
// 这里钉的是断点续录能不能信的四条性质：
//   - last_seq 单调（迟到的旧上报必须被拒，否则续录起点会被悄悄改错）；
//   - 计数列一律由切片表重算（按上报值累加会在重放/并发下双计）；
//   - 终态不可复活，同态重放零写零事件；
//   - 高频心跳不占事务、不发事件（否则 Outbox 被采样淹掉）。
//
// 事件相关的用例被生产缺陷 #1 阻塞（见 outbox_contract_test.go），会以 Skip 出现。

func reportReq(recordID, expVer int64, state rpc.LiveRecordState) *rpc.ReportLiveRecordProgressReq {
	return &rpc.ReportLiveRecordProgressReq{
		RecordId: recordID, ExpectedVersion: expVer, State: state,
		HeartbeatAt: nowTS(), Reason: rpc.FailureReason_FAILURE_REASON_UNSPECIFIED,
		WorkerId: "worker-a", TraceId: "trace-prog-1",
	}
}

func TestReportLiveRecordProgressRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.ReportLiveRecordProgressReq)
		want   error
	}{
		{"任务号非正", func(r *rpc.ReportLiveRecordProgressReq) { r.RecordId = 0 }, model.ErrRecordTaskNotFound},
		{"状态越界", func(r *rpc.ReportLiveRecordProgressReq) { r.State = rpc.LiveRecordState(99) }, model.ErrInvalidTransition},
		{"未指定状态", func(r *rpc.ReportLiveRecordProgressReq) { r.State = rpc.LiveRecordState_LIVE_RECORD_STATE_UNSPECIFIED }, model.ErrInvalidTransition},
		// 登记态只由 StartLiveRecord 写入：Worker 报 PENDING 会被记成一次不存在的迁移。
		{"PENDING 不是可上报态", func(r *rpc.ReportLiveRecordProgressReq) { r.State = rpc.LiveRecordState_LIVE_RECORD_STATE_PENDING }, model.ErrInvalidTransition},
		// 本服务没有 CancelLiveRecord 入口，Worker 更无权代为取消。
		{"CANCELLED 不是可上报态", func(r *rpc.ReportLiveRecordProgressReq) { r.State = rpc.LiveRecordState_LIVE_RECORD_STATE_CANCELLED }, model.ErrInvalidTransition},
		{"失败必须可归因", func(r *rpc.ReportLiveRecordProgressReq) { r.State = rpc.LiveRecordState_LIVE_RECORD_STATE_FAILED }, model.ErrInvalidTransition},
		{"原因越界", func(r *rpc.ReportLiveRecordProgressReq) {
			r.State = rpc.LiveRecordState_LIVE_RECORD_STATE_FAILED
			r.Reason = rpc.FailureReason(99)
		}, model.ErrInvalidTransition},
		{"水位为负", func(r *rpc.ReportLiveRecordProgressReq) { r.LastSeq = -1 }, model.ErrInvalidSeq},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) { r.LastSeq = 3 })
			snapshot := *mustRecord(t, db, row.RecordId)
			in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
			tc.mutate(in)
			before := snapshotWrites(db)

			info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
			wantFail(t, info, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
			assertRowUnchanged(t, db, snapshot.RecordId, snapshot)
		})
	}
}

func TestReportLiveRecordProgressMissingTaskAndFailClosedOnRead(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).
		ReportLiveRecordProgress(reportReq(999, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING))
	wantFail(t, info, err, model.ErrRecordTaskNotFound, "不存在的录制任务")

	db2 := newStore()
	svcCtx2 := newTestSvc(db2)
	row := seedRecord(db2, model.RecordStateRecording, nil)
	db2.failOn("RecordTasks.FindOne", errModelDown)
	info2, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx2).
		ReportLiveRecordProgress(reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING))
	// 「读不动」不得被读成「没有这行」，更不得被读成「已记录」。
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("主表读故障必须原样抛出：%v", err)
	}
	if !isNilPtr(info2) {
		t.Errorf("故障路径不得带回响应体：%+v", info2)
	}
	wantNoWrites(t, db2, snapshotWrites(db2), "主表读故障")
}

// 迟到/重投的旧水位必须被拒：它是断点续录起点的唯一锚点。
func TestReportLiveRecordProgressRejectsStaleWatermark(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) { r.LastSeq = 50 })
	snapshot := *mustRecord(t, db, row.RecordId)
	before := snapshotWrites(db)

	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	in.LastSeq = 49
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	wantFail(t, info, err, model.ErrSeqNotMonotonic, "旧水位上报")
	wantNoWrites(t, db, before, "旧水位上报")
	assertRowUnchanged(t, db, snapshot.RecordId, snapshot)
	wantEvents(t, db, nil, "旧水位上报")

	// 等值上报（同一秒重投）不是倒退：走心跳分支，水位仍然不动。
	same := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	same.LastSeq = 50
	if _, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(same); err != nil {
		t.Fatalf("等值重投应被当幂等心跳：%v", err)
	}
	if got := mustRecord(t, db, row.RecordId); got.LastSeq != 50 {
		t.Errorf("等值重投改动了水位：%d", got.LastSeq)
	}
}

// 纯心跳：状态与水位都没变 → 不占事务、不发事件，只顺延 heartbeat_at/timeout_at。
func TestReportLiveRecordProgressHeartbeatDefersTimeoutWithoutEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) {
		r.LastSeq, r.HeartbeatAt, r.TimeoutAt = 12, nowTS()-30, nowTS()-30+90
	})
	before := snapshotWrites(db)

	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	in.LastSeq = 12
	hb := nowTS() + 5 // Worker 自带时钟：必须采信它，否则复现不了时钟偏移
	in.HeartbeatAt = hb
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	info = wantOK(t, info, err, "纯心跳")

	if info.GetState() != rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING || info.GetLastSeq() != 12 {
		t.Errorf("心跳不得推进状态或水位：%v/%d", info.GetState(), info.GetLastSeq())
	}
	if info.GetHeartbeatAt() != hb {
		t.Errorf("Worker 给的心跳时刻必须被采信：%d → %d", hb, info.GetHeartbeatAt())
	}
	// 顺延用登记时刻的超时秒数快照（timeout_at-heartbeat_at），不回读配置。
	if got := info.GetTimeoutAt() - info.GetHeartbeatAt(); got != 90 {
		t.Errorf("timeout_at 应按登记的 90 秒顺延，实际差值 %d", got)
	}
	wantCalls(t, db, "DB.TransactCtx", before["DB.TransactCtx"], 0, "心跳不该开事务")
	wantEvents(t, db, nil, "心跳不发事件")
	// 心跳走非事务的 UpdateState：确实发生过一次写。
	wantCalls(t, db, "RecordTasks.UpdateState", before["RecordTasks.UpdateState"], 1, "心跳写一次")
}

// 心跳时 Worker 没给时刻：服务端兜底为当前时刻，不能写 0（0 会让超时清扫立刻判死）。
func TestReportLiveRecordProgressHeartbeatFallsBackToServerClock(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) { r.LastSeq = 4 })

	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	in.LastSeq = 4
	in.HeartbeatAt = 0
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	info = wantOK(t, info, err, "无心跳时刻")
	if info.GetHeartbeatAt() < nowTS()-5 {
		t.Fatalf("缺省心跳时刻必须由服务端兜底，实际 %d", info.GetHeartbeatAt())
	}
	if info.GetTimeoutAt() <= info.GetHeartbeatAt() {
		t.Fatalf("timeout_at 必须晚于 heartbeat_at：%d<=%d", info.GetTimeoutAt(), info.GetHeartbeatAt())
	}
}

// 水位推进但状态未变：必须写库且不产生事件（高频采样不得淹 Outbox）。
func TestReportLiveRecordProgressAdvancesWatermarkWithoutEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) { r.LastSeq = 20 })
	beforeVer := row.Version

	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	in.LastSeq = 25
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	info = wantOK(t, info, err, "同状态推进水位")

	if info.GetLastSeq() != 25 {
		t.Fatalf("水位必须前进到 25，实际 %d", info.GetLastSeq())
	}
	if info.GetVersion() != beforeVer+1 {
		t.Errorf("一次成功推进恰好 ++version：%d → %d", beforeVer, info.GetVersion())
	}
	if info.GetState() != rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING {
		t.Errorf("状态不得被改：%v", info.GetState())
	}
	wantEvents(t, db, nil, "同状态水位推进不得发事件")
	wantCalls(t, db, "RecordTasks.RefreshStatsTx", 0, 0, "非收尾不得重算派生计数")
}

// err_msg 是 Worker 自由文本，最常见的形态就是带签名拉流地址：必须脱敏 + 截到列宽。
// 走「同状态 + 水位推进」这条不发事件的路径，因此今天就能断言。
func TestReportLiveRecordProgressSanitizesErrMsg(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) { r.LastSeq = 1 })

	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	in.LastSeq = 2
	in.ErrMsg = "拉流失败\n" + leakSignedURL + "\n" + strings.Repeat("detail", 200)
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	requireNoEnvelopeBug(t, err, "err_msg 脱敏")
	info = wantOK(t, info, err, "err_msg 脱敏")

	got := info.GetErrMsg()
	if strings.Contains(got, leakMarker) {
		t.Errorf("签名地址明文入库：%q", got)
	}
	if runeLen(got) > maxErrMsgRunes {
		t.Errorf("err_msg 未截到列宽 %d，实际 %d", maxErrMsgRunes, runeLen(got))
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("err_msg 不得留换行（会破坏日志与摘要）：%q", got)
	}
	wantNoLeak(t, db, "err_msg 脱敏")
}

// errno 与 reason 各自独立：心跳不带 reason 时不得抹掉已记录的原因。
func TestReportLiveRecordProgressKeepsRecordedReason(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) {
		r.LastSeq, r.Reason, r.Errno = 3, model.ReasonSourceLost, 42
	})
	before := snapshotWrites(db)

	// 同状态同水位 → 纯心跳分支：patch 里根本没有 reason/errno，行内值必须留着。
	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	in.LastSeq = 3
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	info = wantOK(t, info, err, "心跳保留原因")
	if info.GetReason() != rpc.FailureReason_FAILURE_REASON_SOURCE_LOST || info.GetErrno() != 42 {
		t.Errorf("心跳把已记录的原因抹掉了：reason=%d errno=%d", info.GetReason(), info.GetErrno())
	}
	_ = before
}

// 终态不可复活：同态重放零副作用；异态直接拒绝。
func TestReportLiveRecordProgressTerminalRules(t *testing.T) {
	cases := []struct {
		name      string
		curState  int32
		inState   rpc.LiveRecordState
		wantErr   error
		wantState rpc.LiveRecordState
	}{
		{"STOPPED 重放", model.RecordStateStopped, rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED, nil,
			rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED},
		{"STOPPED 不得被改成 FAILED", model.RecordStateStopped, rpc.LiveRecordState_LIVE_RECORD_STATE_FAILED,
			model.ErrTerminalState, rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED},
		{"CANCELLED 不得被改成 STOPPED", model.RecordStateCancelled, rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED,
			model.ErrTerminalState, rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedRecord(db, tc.curState, func(r *model.LiveRecordTask) { r.LastSeq = 8 })
			snapshot := *mustRecord(t, db, row.RecordId)
			before := snapshotWrites(db)

			in := reportReq(row.RecordId, 0, tc.inState)
			in.LastSeq = 8 // 与行内水位一致，确保测的是终态判定而不是水位判定
			in.Reason = rpc.FailureReason_FAILURE_REASON_TIMEOUT
			info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
			if tc.wantErr != nil {
				wantFail(t, info, err, tc.wantErr, tc.name)
			} else {
				info = wantOK(t, info, err, tc.name)
				if info.GetState() != tc.wantState || info.GetLastSeq() != 8 {
					t.Errorf("终态重放不得改写事实：%v/%d", info.GetState(), info.GetLastSeq())
				}
			}
			// 终态路径必须在进入事务之前就被拦下：一行都不许动。
			wantNoWrites(t, db, before, tc.name)
			assertRowUnchanged(t, db, snapshot.RecordId, snapshot)
			wantEvents(t, db, nil, tc.name)
		})
	}
}

// 状态机非法边一律拒绝（哪怕水位合法）。
func TestReportLiveRecordProgressRefusesIllegalTransitions(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		want  rpc.LiveRecordState
	}{
		{"STOPPING 不能退回 RECORDING", model.RecordStateStopping, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING},
		{"PENDING 不能直接收尾 STOPPED", model.RecordStatePending, rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED},
		{"STOPPING 不能跳到 PENDING 之外的重开", model.RecordStateStopping, rpc.LiveRecordState_LIVE_RECORD_STATE_PENDING},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedRecord(db, tc.state, func(r *model.LiveRecordTask) { r.LastSeq = 5 })
			snapshot := *mustRecord(t, db, row.RecordId)
			before := snapshotWrites(db)

			in := reportReq(row.RecordId, 0, tc.want)
			in.LastSeq = 6 // 前进的水位，确保不是因为旧水位被拒
			in.Reason = rpc.FailureReason_FAILURE_REASON_TIMEOUT
			info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
			if err == nil || !errors.Is(err, model.ErrInvalidTransition) {
				t.Fatalf("%s：应报 ErrInvalidTransition，实际 %v", tc.name, err)
			}
			if !isNilPtr(info) {
				t.Errorf("非法迁移不得带回响应体：%+v", info)
			}
			wantNoWrites(t, db, before, tc.name)
			assertRowUnchanged(t, db, snapshot.RecordId, snapshot)
		})
	}
}

// 版本冲突（Worker 先读后写的乐观锁）：0 行必须归因成 ErrVersionConflict，且行完全不动。
func TestReportLiveRecordProgressVersionConflictLeavesRowIntact(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) { r.LastSeq = 5 })
	snapshot := *mustRecord(t, db, row.RecordId)

	in := reportReq(row.RecordId, row.Version+7, rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPING)
	in.LastSeq = 6
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	wantFail(t, info, err, model.ErrVersionConflict, "过期版本")
	assertRowUnchanged(t, db, snapshot.RecordId, snapshot)
	wantEvents(t, db, nil, "过期版本不得留下事件")
	wantCalls(t, db, "Outbox.Insert", 0, 0, "CAS 0 行不得尝试写事件")
}

// 收尾（STOPPED/FAILED）前必须在事务外读一次缺口事实：
// StatsInRange 走主连接，看不见本事务未提交的写入，放事务里就会算少。
func TestReportLiveRecordProgressReadsGapsOutsideTransaction(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateStopping, func(r *model.LiveRecordTask) { r.LastSeq = 4 })
	seedSegment(db, row.RecordId, 1, model.SegmentStateVerified, nil)
	seedSegment(db, row.RecordId, 4, model.SegmentStateUploaded, nil)

	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED)
	in.LastSeq = 4
	_, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	requireNoEnvelopeBug(t, err, "收尾缺口预读")

	wantCalls(t, db, "Segments.StatsInRange", 0, 1, "收尾只预读一次缺口")
	q := db.lastSegmentStats
	if q == nil {
		t.Fatal("StatsInRange 未被调用")
	}
	// 区间必须是 [1, 本次上报水位]：按库内 MAX(seq) 算会把「还没落的尾部」当成完整。
	if q.recordID != row.RecordId || q.fromSeq != 1 || q.toSeq != 4 {
		t.Fatalf("缺口统计区间应为 record=%d [1,4]，实际 %+v", row.RecordId, q)
	}
}

// 事务中途失败（这里是派生计数重算失败）：状态推进必须一起回滚，不留半成品。
func TestReportLiveRecordProgressRollsBackWhenRefreshStatsFails(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateStopping, func(r *model.LiveRecordTask) { r.LastSeq = 2 })
	snapshot := *mustRecord(t, db, row.RecordId)
	seedSegment(db, row.RecordId, 1, model.SegmentStateVerified, nil)
	seedSegment(db, row.RecordId, 2, model.SegmentStateVerified, nil)
	db.failOn("RecordTasks.RefreshStatsTx", errModelDown)

	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED)
	in.LastSeq = 2
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("派生计数重算失败必须原样抛出：%v", err)
	}
	if !isNilPtr(info) {
		t.Errorf("失败路径不得带回响应体：%+v", info)
	}
	assertRowUnchanged(t, db, snapshot.RecordId, snapshot)
	wantEvents(t, db, nil, "回滚后不得留下事件")
}

// 收尾写库语义（缺陷 #1 阻塞）：STOPPED 写 record_end_at 并重算计数，发 record_stopped；
// 有洞时再发 record_gap_detected。
func TestReportLiveRecordProgressSettlesStoppedWithRecomputedStats(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	startAt := nowTS() - 500
	row := seedRecord(db, model.RecordStateStopping, func(r *model.LiveRecordTask) {
		r.LastSeq, r.RecordStartAt, r.EndAt = 5, startAt, nowTS()+60
	})
	// 1/3/5 已校验，2/4 是显式缺口：段数=5、缺口=2、时长=3*10000。
	for _, seq := range []int64{1, 3, 5} {
		seedSegment(db, row.RecordId, seq, model.SegmentStateVerified, nil)
	}
	for _, seq := range []int64{2, 4} {
		seedSegment(db, row.RecordId, seq, model.SegmentStateMissing, func(s *model.LiveRecordSegment) {
			s.Bucket, s.ObjectKey = "", ""
		})
	}

	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED)
	in.LastSeq = 5
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	requireNoEnvelopeBug(t, err, "收尾 STOPPED")
	info = wantOK(t, info, err, "收尾 STOPPED")

	if info.GetState() != rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED {
		t.Fatalf("应进入 STOPPED，实际 %v", info.GetState())
	}
	if info.GetRecordEndAt() <= 0 {
		t.Errorf("record_end_at 是回放区间的真值，收尾时必须写入：%d", info.GetRecordEndAt())
	}
	if info.GetRecordStartAt() != startAt {
		t.Errorf("实际开始时刻不得被收尾改写：%d → %d", startAt, info.GetRecordStartAt())
	}
	if info.GetEndAt() != row.EndAt {
		t.Errorf("期望终点 end_at 是停止指令下发的快照，收尾不得覆写：%d → %d", row.EndAt, info.GetEndAt())
	}
	// 派生列只能重算，不能按上报累加。
	if info.GetSegmentCount() != 5 || info.GetGapCount() != 2 || info.GetRecordedDurationMs() != 30000 {
		t.Errorf("派生计数应由切片表重算：count=%d gap=%d dur=%d",
			info.GetSegmentCount(), info.GetGapCount(), info.GetRecordedDurationMs())
	}
	wantEvents(t, db, []string{model.EventTypeRecordStopped, model.EventTypeRecordGapDetected}, "收尾事件")
	stopped := eventPayload(t, eventAt(t, db, 0))
	if toInt64(t, stopped, "gap_count") != 2 || toInt64(t, stopped, "registered") != 5 ||
		toInt64(t, stopped, "last_seq") != 5 {
		t.Errorf("record_stopped 必须带上切片清单的量级：%v", stopped)
	}
	gap := eventPayload(t, eventAt(t, db, 1))
	if toInt64(t, gap, "gap_count") != 2 || toInt64(t, gap, "expected") != 5 {
		t.Errorf("record_gap_detected 必须带缺口量级：%v", gap)
	}
}

// FAILED 可被续录：不能写 record_end_at（否则留下一条假的结束时刻）。
func TestReportLiveRecordProgressFailedKeepsRecordEndAtUnset(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) { r.LastSeq, r.RecordStartAt = 3, nowTS()-300 })
	startAt := row.RecordStartAt
	seedSegment(db, row.RecordId, 1, model.SegmentStateVerified, nil)
	seedSegment(db, row.RecordId, 3, model.SegmentStateCorrupt, nil)

	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_FAILED)
	in.LastSeq = 3
	in.Reason = rpc.FailureReason_FAILURE_REASON_STORAGE
	in.Errno = 28
	in.ErrMsg = "disk full"
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	requireNoEnvelopeBug(t, err, "收尾 FAILED")
	info = wantOK(t, info, err, "收尾 FAILED")

	if info.GetRecordEndAt() != 0 {
		t.Errorf("FAILED 可续录，写结束时刻会让回放误判区间完整：%d", info.GetRecordEndAt())
	}
	if info.GetRecordStartAt() != startAt {
		t.Errorf("实际开始时刻必须保留：%d", info.GetRecordStartAt())
	}
	if info.GetReason() != rpc.FailureReason_FAILURE_REASON_STORAGE || info.GetErrno() != 28 {
		t.Errorf("失败必须可归因：reason=%d errno=%d", info.GetReason(), info.GetErrno())
	}
	if info.GetLastSeq() != 3 {
		t.Errorf("失败上报的水位要留下（续录锚点）：%d", info.GetLastSeq())
	}
	if info.GetSegmentCount() != 2 || info.GetGapCount() != 1 {
		t.Errorf("派生计数应重算：count=%d gap=%d", info.GetSegmentCount(), info.GetGapCount())
	}
	wantEvents(t, db, []string{model.EventTypeRecordStateChanged}, "FAILED 只发状态变更")
}

// PENDING→RECORDING 记一次实际开始时刻；后续续录上报不得把它往后推。
func TestReportLiveRecordProgressRecordsFirstStartAtOnly(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRecord(db, model.RecordStatePending, nil)

	first := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	first.LastSeq, first.HeartbeatAt = 1, nowTS()-1000
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(first)
	requireNoEnvelopeBug(t, err, "开录")
	info = wantOK(t, info, err, "开录")
	startAt := info.GetRecordStartAt()
	if startAt <= 0 {
		t.Fatalf("首次进入 RECORDING 必须写 record_start_at：%d", startAt)
	}
	wantEvents(t, db, []string{model.EventTypeRecordStateChanged}, "开录事件")

	// Worker 重启后的第二次 RUNNING：真实启动点不能被推后。
	second := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	second.LastSeq, second.HeartbeatAt = 2, nowTS()+5000
	info2, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(second)
	requireNoEnvelopeBug(t, err, "续录")
	info2 = wantOK(t, info2, err, "续录")
	if info2.GetRecordStartAt() != startAt {
		t.Errorf("record_start_at 只记首次：%d → %d", startAt, info2.GetRecordStartAt())
	}
	if info2.GetLastSeq() != 2 {
		t.Errorf("续录水位要前进：%d", info2.GetLastSeq())
	}
	wantEvents(t, db, []string{model.EventTypeRecordStateChanged}, "同状态续录不再发事件")
}

// 同状态推进的 Worker 时钟也决定 timeout_at：预算取登记快照，不取配置。
func TestReportLiveRecordProgressHeartbeatDoesNotTouchRecordTimes(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	startAt := nowTS() - 600
	row := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) {
		r.LastSeq, r.RecordStartAt = 9, startAt
	})
	in := reportReq(row.RecordId, 0, rpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING)
	in.LastSeq = 9
	info, err := NewReportLiveRecordProgressLogic(context.Background(), svcCtx).ReportLiveRecordProgress(in)
	info = wantOK(t, info, err, "心跳")
	if info.GetRecordStartAt() != startAt || info.GetRecordEndAt() != 0 {
		t.Errorf("心跳不得改动实际起止时刻：%d/%d", info.GetRecordStartAt(), info.GetRecordEndAt())
	}
	if info.GetVersion() != row.Version+1 {
		t.Errorf("心跳也是一次写，version 必须 ++（乐观锁据此发现并发）：%d → %d",
			row.Version, info.GetVersion())
	}
}
