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

// 录制读侧与逐片登记：ReportRecordSegment / GetLiveRecordTask / ListLiveRecordTasks / ListRecordSegments。
//
// 这一组的方法分两类，各自的「必须」正好相反：
//
//	写侧只有 ReportRecordSegment（切片表只增不减，(record_id,seq) 上有 uniq_record_seq），
//	它必须能推进任务行的派生列（last_seq/segment_count/gap_count/recorded_duration_ms）；
//	另外三个是纯读侧，它们的「必须」是不动任何东西 —— 读接口一旦顺手重算写回，
//	就给只读账号开了一条写路径，而且会把 Worker 正在用的 version 撞废。
//
// ReportRecordSegment 要钉住的四条：
//  1. 幂等来自 uniq_record_seq：INSERT IGNORE 0 行不是失败，必须回读区分
//     「已是目标态」（成功）与「状态机不允许」（ErrInvalidTransition）；正向只有
//     UPLOADING→UPLOADED→VERIFIED，VERIFIED 之后不得回退（未校验的片绝不能进拼接），
//     缺口片也不能直接判 VERIFIED（补录的产物必须重新校验）；
//  2. 缺口必须显式成行：seq 跳号时补 MISSING 行，且缺口行的 bucket/object_key/checksum
//     必须是空串 —— 写了引用就等于告诉回收任务「这里有个可删对象」（DDL 列注释同口径）；
//     补不出洞（无分片时长/锚点）或跳跃超过 maxGapFillRows 时不写垃圾行，
//     但事件照发，并把「说不清有多少片丢了」的量放在 missing_untraced 里；
//  3. 派生列由 RefreshStatsTx 从切片表整体重算，last_seq 只前进；
//     逐片登记不得 ++version（否则 Worker 自己的进度上报永远撞版本冲突）；
//  4. 父任务终态拒绝登记（无人认领的孤儿对象），但 FAILED 不是终态 —— 它能断点续录。
//
// 事件口径：本方法只在真的补出洞或有洞说不清时发 livemedia.record.gap.detected，
// 一次事务一条；record.stopped 由 ReportLiveRecordProgress 负责（见 record_progress_test.go）。
// payload 只带标识与序号，不带对象引用，因此每个写路径用例都带 wantNoLeak。
//
// 交错用 db.onHit 构造「logic 读快照之后、写入之前别人改/删了行」。回滚会撤销钩子造出来的态，
// 所以这里断言的是归因与副作用（错误、事件条数、回读次数），不是交错后的最终库态
// （见 fakes_test.go 头注释）。

// ---------------------------------------------------------------- 请求构造与小工具

func getRecordReq(recordID int64) *rpc.LiveRecordTaskReq {
	return &rpc.LiveRecordTaskReq{RecordId: recordID}
}

func listRecordsReq(pn, ps int32) *rpc.ListLiveRecordTasksReq {
	return &rpc.ListLiveRecordTasksReq{Page: &rpc.PageParam{Pn: pn, Ps: ps}}
}

func listSegmentsReq(recordID, afterSeq int64, state rpc.SegmentState, limit int32) *rpc.ListRecordSegmentsReq {
	return &rpc.ListRecordSegmentsReq{RecordId: recordID, AfterSeq: afterSeq, State: state, Limit: limit}
}

// recordingTask 一行可登记的录制任务：StartAt 是补洞反推时间轴的锚点，
// 没有它 planMissingRows 会拒绝补洞，因此凡是要测缺口的用例都得给。
func recordingTask(db *store, mutate func(*model.LiveRecordTask)) *model.LiveRecordTask {
	return seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) {
		r.StartAt = 1000000
		r.SegmentSeconds = 10
		if mutate != nil {
			mutate(r)
		}
	})
}

// gapSegment 缺口行的种子形态：真实现里 MISSING 行由 InsertIgnoreMissing 写入，
// SQL 的 VALUES 里根本没有 bucket/object_key/checksum 列（model/live_record_segment.go:235），
// 所以种子也必须留空，否则测的是「有引用的缺口」这种库里不存在的行。
func gapSegment(db *store, recordID, seq int64, anchor int64) *model.LiveRecordSegment {
	return seedSegment(db, recordID, seq, model.SegmentStateMissing, func(s *model.LiveRecordSegment) {
		s.StartAt, s.EndAt = anchor+(seq-1)*10, anchor+seq*10
		s.Bucket, s.ObjectKey, s.Checksum, s.SizeBytes = "", "", "", 0
	})
}

func mustSegment(t *testing.T, db *store, recordID, seq int64) *model.LiveRecordSegment {
	t.Helper()
	row, ok := db.segments[recordID][seq]
	if !ok {
		t.Fatalf("录制 %d 的切片 seq=%d 不存在（现有 seq %+v）", recordID, seq, segmentSeqs(db, recordID))
	}
	return row
}

func segmentSeqs(db *store, recordID int64) []int64 {
	var seqs []int64
	for seq := range db.segments[recordID] {
		seqs = append(seqs, seq)
	}
	return seqs
}

func assertSegmentUnchanged(t *testing.T, db *store, recordID, seq int64, want model.LiveRecordSegment) {
	t.Helper()
	got := mustSegment(t, db, recordID, seq)
	if *got != want {
		t.Fatalf("非法路径改动了切片行：\n got=%+v\nwant=%+v", *got, want)
	}
}

// wantSegmentEchoesRow 钉住「响应 == 提交后的行」，逐列比对。
// registered_at/mtime 尤其重要：前者是「这片第一次被登记」的审计时刻，重放不得覆盖；
// 后者是推进证据，若等于 registered_at 就说明这次推进其实没写进去。
func wantSegmentEchoesRow(t *testing.T, label string, info *rpc.RecordSegmentInfo, row *model.LiveRecordSegment) {
	t.Helper()
	wantField(t, label, "id", info.GetId(), row.Id)
	wantField(t, label, "record_id", info.GetRecordId(), row.RecordId)
	wantField(t, label, "room_id", info.GetRoomId(), row.RoomId)
	wantField(t, label, "live_session_id", info.GetLiveSessionId(), row.LiveSession)
	wantField(t, label, "seq", info.GetSeq(), row.Seq)
	wantField(t, label, "start_at", info.GetStartAt(), row.StartAt)
	wantField(t, label, "end_at", info.GetEndAt(), row.EndAt)
	wantField(t, label, "duration_ms", info.GetDurationMs(), row.DurationMs)
	wantField(t, label, "state", int32(info.GetState()), row.State)
	wantField(t, label, "bucket", info.GetBucket(), row.Bucket)
	wantField(t, label, "object_key", info.GetObjectKey(), row.ObjectKey)
	wantField(t, label, "size_bytes", info.GetSizeBytes(), row.SizeBytes)
	wantField(t, label, "checksum", info.GetChecksum(), row.Checksum)
	wantField(t, label, "worker_id", info.GetWorkerId(), row.WorkerId)
	wantField(t, label, "registered_at", info.GetRegisteredAt(), row.RegisteredAt)
	wantField(t, label, "mtime", info.GetMtime(), row.Mtime)
}

// wantRecordEchoesRow 同样用于三个读方法：读接口一旦自己拼响应（例如顺手把 gap_count 报 0），
// 排障的人就会拿着一份库里不存在的账本做决定。
func wantRecordEchoesRow(t *testing.T, label string, info *rpc.LiveRecordTaskInfo, row *model.LiveRecordTask) {
	t.Helper()
	wantField(t, label, "record_id", info.GetRecordId(), row.RecordId)
	wantField(t, label, "room_id", info.GetRoomId(), row.RoomId)
	wantField(t, label, "live_session_id", info.GetLiveSessionId(), row.LiveSession)
	wantField(t, label, "source_task_id", info.GetSourceTaskId(), row.SourceTaskId)
	wantField(t, label, "state", int32(info.GetState()), row.State)
	wantField(t, label, "start_at", info.GetStartAt(), row.StartAt)
	wantField(t, label, "end_at", info.GetEndAt(), row.EndAt)
	wantField(t, label, "record_start_at", info.GetRecordStartAt(), row.RecordStartAt)
	wantField(t, label, "record_end_at", info.GetRecordEndAt(), row.RecordEndAt)
	wantField(t, label, "segment_seconds", info.GetSegmentSeconds(), row.SegmentSeconds)
	wantField(t, label, "last_seq", info.GetLastSeq(), row.LastSeq)
	wantField(t, label, "segment_count", info.GetSegmentCount(), row.SegmentCount)
	wantField(t, label, "gap_count", info.GetGapCount(), row.GapCount)
	wantField(t, label, "recorded_duration_ms", info.GetRecordedDurationMs(), row.RecordedDuration)
	wantField(t, label, "output_bucket", info.GetOutputBucket(), row.OutputBucket)
	wantField(t, label, "output_prefix", info.GetOutputPrefix(), row.OutputPrefix)
	wantField(t, label, "heartbeat_at", info.GetHeartbeatAt(), row.HeartbeatAt)
	wantField(t, label, "timeout_at", info.GetTimeoutAt(), row.TimeoutAt)
	wantField(t, label, "version", info.GetVersion(), row.Version)
	wantField(t, label, "reason", int32(info.GetReason()), row.Reason)
	wantField(t, label, "errno", info.GetErrno(), row.Errno)
	wantField(t, label, "err_msg", info.GetErrMsg(), row.ErrMsg)
	wantField(t, label, "request_id", info.GetRequestId(), row.RequestId)
	wantField(t, label, "trace_id", info.GetTraceId(), row.TraceId)
	wantField(t, label, "ctime", info.GetCtime(), row.Ctime)
	wantField(t, label, "mtime", info.GetMtime(), row.Mtime)
}

func replySegmentSeqs(rows []*rpc.RecordSegmentInfo) []int64 {
	seqs := make([]int64, 0, len(rows))
	for _, r := range rows {
		seqs = append(seqs, r.GetSeq())
	}
	return seqs
}

func joinInt64(rows []int64) string {
	parts := make([]string, 0, len(rows))
	for _, v := range rows {
		parts = append(parts, strconv.FormatInt(v, 10))
	}
	return strings.Join(parts, ",")
}

// mustReport 只关心「这次登记没有失败」的用例走这里：失败必须当场红，不许借「返回值没用」隐身。
func mustReport(t *testing.T, label string, svc *ReportRecordSegmentLogic, in *rpc.ReportRecordSegmentReq) {
	t.Helper()
	info, err := svc.ReportRecordSegment(in)
	if err != nil {
		t.Fatalf("%s：意外失败：%v", label, err)
	}
	if isNilPtr(info) {
		t.Fatalf("%s：成功路径必须带回投影：%+v", label, info)
	}
}

// ---------------------------------------------------------------- ReportRecordSegment

func TestReportRecordSegmentRegistersFirstRowAndAdvancesWatermark(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, nil)

	info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
		ReportRecordSegment(segmentReq(task.RecordId, 1, model.SegmentStateUploading))
	info = wantOK(t, info, err, "登记首片")

	row := mustSegment(t, db, task.RecordId, 1)
	wantSegmentEchoesRow(t, "登记首片", info, row)
	wantField(t, "登记首片", "state", row.State, model.SegmentStateUploading)
	wantField(t, "登记首片", "room_id 取自任务而非入参", row.RoomId, task.RoomId)
	wantField(t, "登记首片", "live_session_id 取自任务", row.LiveSession, task.LiveSession)
	wantField(t, "登记首片", "object_key 原样存相对路径（不签名）", row.ObjectKey, "rec/71001/1.m3u8")

	stored := mustRecord(t, db, task.RecordId)
	wantField(t, "登记首片", "last_seq", stored.LastSeq, int64(1))
	wantField(t, "登记首片", "segment_count", stored.SegmentCount, int64(1))
	wantField(t, "登记首片", "gap_count", stored.GapCount, int64(0))
	// 只有 VERIFIED 才计入有效时长：未校验的片进不了拼接，算进去等于虚报录制成果。
	wantField(t, "登记首片", "recorded_duration_ms", stored.RecordedDuration, int64(0))
	// 逐片登记不得推进 version：那是 Worker 进度上报的 CAS 依据，
	// 每片都 +1 会让正在录制的任务每次上报都撞版本冲突（logic 注释同口径）。
	wantField(t, "登记首片", "version 不变", stored.Version, int64(1))
	wantField(t, "登记首片", "state 不被登记改写", stored.State, model.RecordStateRecording)

	wantEvents(t, db, nil, "登记首片")
	wantCalls(t, db, "Segments.InsertIgnoreMissingTx", 0, 0, "seq=1 紧接水位，不得补洞")
	wantNoLeak(t, db, "登记首片")
}

func TestReportRecordSegmentForwardChainKeepsSingleRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, nil)
	svc := NewReportRecordSegmentLogic(context.Background(), svcCtx)

	if _, err := svc.ReportRecordSegment(segmentReq(task.RecordId, 1, model.SegmentStateUploading)); err != nil {
		t.Fatalf("UPLOADING 登记失败：%v", err)
	}
	first := *mustSegment(t, db, task.RecordId, 1)

	uploaded := segmentReq(task.RecordId, 1, model.SegmentStateUploaded)
	uploaded.SizeBytes, uploaded.WorkerId, uploaded.TraceId = 2<<20, "worker-b", "trace-seg-uploaded"
	info, err := svc.ReportRecordSegment(uploaded)
	info = wantOK(t, info, err, "推进到 UPLOADED")
	mid := mustSegment(t, db, task.RecordId, 1)
	wantSegmentEchoesRow(t, "推进到 UPLOADED", info, mid)

	wantField(t, "推进到 UPLOADED", "仍是同一行（uniq_record_seq）", int64(countSegments(db, task.RecordId)), int64(1))
	wantField(t, "推进到 UPLOADED", "id 不变", mid.Id, first.Id)
	wantField(t, "推进到 UPLOADED", "registered_at 不被推进覆盖", mid.RegisteredAt, first.RegisteredAt)
	wantField(t, "推进到 UPLOADED", "state", mid.State, model.SegmentStateUploaded)
	wantField(t, "推进到 UPLOADED", "size_bytes 刷新", mid.SizeBytes, int64(2<<20))
	wantField(t, "推进到 UPLOADED", "worker_id 刷新为本次上报方", mid.WorkerId, "worker-b")
	wantField(t, "推进到 UPLOADED", "trace_id 刷新", mid.TraceId, "trace-seg-uploaded")
	wantField(t, "推进到 UPLOADED", "recorded_duration_ms 仍为 0", mustRecord(t, db, task.RecordId).RecordedDuration, int64(0))

	verified := segmentReq(task.RecordId, 1, model.SegmentStateVerified)
	info, err = svc.ReportRecordSegment(verified)
	info = wantOK(t, info, err, "推进到 VERIFIED")
	wantSegmentEchoesRow(t, "推进到 VERIFIED", info, mustSegment(t, db, task.RecordId, 1))
	wantField(t, "推进到 VERIFIED", "行数仍为 1", int64(countSegments(db, task.RecordId)), int64(1))

	stored := mustRecord(t, db, task.RecordId)
	wantField(t, "推进到 VERIFIED", "recorded_duration_ms 校验通过才计入", stored.RecordedDuration, int64(10000))
	wantField(t, "推进到 VERIFIED", "segment_count", stored.SegmentCount, int64(1))
	wantField(t, "推进到 VERIFIED", "version 全程不变", stored.Version, int64(1))

	wantEvents(t, db, nil, "正向链路不产生缺口事件")
	wantCalls(t, db, "Segments.InsertIgnoreTx", 0, 3, "三次上报都先撞唯一键")
	wantCalls(t, db, "Segments.UpdateStateTx", 0, 2, "只有 0 行的两次才发推进 CAS")
	wantNoLeak(t, db, "正向链路")
}

// VERIFIED 之后不得回退：一次校验通过的切片是拼接的输入，
// 若允许被后来的迟到上报改回 UPLOADED，回放就会引用一条「状态未知」的片。
func TestReportRecordSegmentRejectsBackwardAfterVerified(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq, r.SegmentCount = 1, 1 })
	seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)
	segSnapshot := *mustSegment(t, db, task.RecordId, 1)
	taskSnapshot := *mustRecord(t, db, task.RecordId)

	info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
		ReportRecordSegment(segmentReq(task.RecordId, 1, model.SegmentStateUploaded))
	wantFail(t, info, err, model.ErrInvalidTransition, "VERIFIED 回退到 UPLOADED")
	if !strings.Contains(err.Error(), "segment state") {
		t.Errorf("错误必须给出「哪个态到哪个态不被允许」：%v", err)
	}
	// 0 行的归因必须靠回读，而不是猜：回读一次是这条分支的指纹。
	wantCalls(t, db, "Segments.FindBySeq", 0, 1, "0 行必须回读归因")
	assertSegmentUnchanged(t, db, task.RecordId, 1, segSnapshot)
	assertRowUnchanged(t, db, task.RecordId, taskSnapshot)
	wantEvents(t, db, nil, "非法回退不得发事件")
	wantNoLeak(t, db, "非法回退")
}

// 缺口片的两条规矩：补录产物必须重新校验（不得直接 VERIFIED），
// 而 MISSING→UPLOADED 一旦成立，gap_count 要收敛回 0（计数是重算的，不是累加）。
func TestReportRecordSegmentGapRowNeedsReuploadBeforeVerified(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq, r.SegmentCount, r.GapCount = 2, 2, 1 })
	gap := gapSegment(db, task.RecordId, 3, task.StartAt)
	before := *mustSegment(t, db, task.RecordId, 3)

	info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
		ReportRecordSegment(segmentReq(task.RecordId, 3, model.SegmentStateVerified))
	wantFail(t, info, err, model.ErrInvalidTransition, "缺口片直接判 VERIFIED")
	assertSegmentUnchanged(t, db, task.RecordId, 3, before)
	wantEvents(t, db, nil, "缺口片直接判 VERIFIED")

	// 补录到达：引用必须回到行上，否则拼接方拿不到对象。
	up := segmentReq(task.RecordId, 3, model.SegmentStateUploaded)
	up.TraceId = "trace-seg-refill"
	info, err = NewReportRecordSegmentLogic(context.Background(), svcCtx).ReportRecordSegment(up)
	info = wantOK(t, info, err, "缺口片补录为 UPLOADED")
	row := mustSegment(t, db, task.RecordId, 3)
	wantSegmentEchoesRow(t, "缺口片补录", info, row)
	wantField(t, "缺口片补录", "state", row.State, model.SegmentStateUploaded)
	wantField(t, "缺口片补录", "seq 不变", row.Seq, gap.Seq)
	wantField(t, "缺口片补录", "id 复用缺口行", row.Id, before.Id)
	wantField(t, "缺口片补录", "object_key 回到行上", row.ObjectKey, "rec/71001/3.m3u8")
	wantField(t, "缺口片补录", "行数不增", int64(countSegments(db, task.RecordId)), int64(1))

	stored := mustRecord(t, db, task.RecordId)
	wantField(t, "缺口片补录", "gap_count 重算归零", stored.GapCount, int64(0))
	wantField(t, "缺口片补录", "segment_count", stored.SegmentCount, int64(1))
	wantField(t, "缺口片补录", "last_seq 只前进", stored.LastSeq, int64(3))
	wantEvents(t, db, nil, "补录不另发缺口事件")
	wantNoLeak(t, db, "缺口片补录")
}

// 「读不到行」与「状态机不允许」是 0 行的两种成因，必须给出不同结论：
// 前者说明这片的引用无人认领（并发清理/传错 record_id），不能谎报成非法迁移。
func TestReportRecordSegmentZeroRowWithoutRowIsNotFound(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, nil)
	seedSegment(db, task.RecordId, 1, model.SegmentStateUploading, nil)
	// 交错：CAS 执行的瞬间，另一条连接把这行清掉了（0 行 → 回读 → 无行）。
	db.onHit("Segments.UpdateStateTx", func() { delete(db.segments[task.RecordId], 1) })

	info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
		ReportRecordSegment(segmentReq(task.RecordId, 1, model.SegmentStateUploaded))
	wantFail(t, info, err, model.ErrSegmentNotFound, "0 行后回读无行")
	if strings.Contains(err.Error(), "segment state") {
		t.Errorf("无行不得报成状态机错误（会把调用方引去查状态机）：%v", err)
	}
	wantEvents(t, db, nil, "0 行后回读无行")
	// 事务回滚撤销了钩子的删除，但「logic 只做了归因、没写任何新行」仍然成立。
	wantField(t, "0 行后回读无行", "回滚后切片行数", int64(countSegments(db, task.RecordId)), int64(1))
	wantNoLeak(t, db, "0 行后回读无行")
}

// 缺口行的正确形态是「没有任何对象引用」。
//
// 这条同时是缺陷 #5 的回归用例：checkObjectRef 曾无条件要求 bucket 非空，
// 于是 Worker 上报一个连桶都不知道的缺口会被拒到无限重投（洞永远不落表，回放看起来却完整），
// 或者逼它编一个桶名 —— 那更糟：回收任务会以为那里有可删对象。
// 契约出处：rpc/livemedia.proto:431、deploy/migrations/live-media/000002:89。
func TestReportRecordSegmentGapRowCarriesNoObjectRef(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq = 1 })
	seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)

	missing := segmentReq(task.RecordId, 2, model.SegmentStateMissing)
	missing.Bucket, missing.ObjectKey, missing.Checksum, missing.SizeBytes = "", "rec/71001/2.m3u8", "", 0
	info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).ReportRecordSegment(missing)
	info = wantOK(t, info, err, "无桶无引用的缺口登记")
	row := mustSegment(t, db, task.RecordId, 2)
	wantSegmentEchoesRow(t, "缺口登记", info, row)
	wantField(t, "缺口登记", "state", row.State, model.SegmentStateMissing)
	wantField(t, "缺口登记", "bucket 为空串", row.Bucket, "")
	// 关键：只给 key 不给桶时必须整对清空，否则留下一行「有 key 无桶」的引用。
	wantField(t, "缺口登记", "object_key 被清空（不与空桶配成半截引用）", row.ObjectKey, "")
	wantField(t, "缺口登记", "checksum", row.Checksum, "")
	wantField(t, "缺口登记", "size_bytes", row.SizeBytes, int64(0))

	stored := mustRecord(t, db, task.RecordId)
	wantField(t, "缺口登记", "gap_count", stored.GapCount, int64(1))
	wantField(t, "缺口登记", "last_seq", stored.LastSeq, int64(2))

	// 显式登记缺口也要发一条事件（同一次事务一条），否则下游只能靠轮询发现断档。
	wantEvents(t, db, []string{model.EventTypeRecordGapDetected}, "显式缺口登记")
	ev := eventAt(t, db, 0)
	wantField(t, "显式缺口登记", "aggregate 类型", ev.AggregateType, model.AggregateRecordTask)
	wantField(t, "显式缺口登记", "aggregate_id", ev.AggregateId, strconv.FormatInt(task.RecordId, 10))
	p := eventPayload(t, ev)
	wantField(t, "显式缺口登记", "missing_recorded", toInt64(t, p, "missing_recorded"), int64(1))
	wantField(t, "显式缺口登记", "missing_untraced", toInt64(t, p, "missing_untraced"), int64(0))
	for _, forbidden := range []string{"bucket", "object_key", "checksum"} {
		if _, ok := p[forbidden]; ok {
			t.Errorf("缺口事件不得携带对象引用 %q：%v", forbidden, p)
		}
	}

	// 反向护栏（缺陷 #5 的修复不得放宽正常链路）：正常链路的切片没有桶就是非法引用。
	for _, state := range []int32{model.SegmentStateUploading, model.SegmentStateUploaded, model.SegmentStateVerified} {
		noBucket := segmentReq(task.RecordId, 9, state)
		noBucket.Bucket = ""
		before := snapshotWrites(db)
		got, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).ReportRecordSegment(noBucket)
		wantFail(t, got, err, model.ErrInvalidBucketRef, "正常链路缺桶")
		wantNoWrites(t, db, before, "正常链路缺桶")
	}
	wantEvents(t, db, []string{model.EventTypeRecordGapDetected}, "正常链路缺桶不得追加事件")
	wantNoLeak(t, db, "缺口登记")
}

// seq 跳号：中间空洞必须补成 MISSING 行，时间轴按 segment_seconds 与任务锚点反推。
// 静默跳过会让回放拼出一条「看起来完整」实则断档的录像。
func TestReportRecordSegmentFillsGapWithMissingRowsAndEmitsOneEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq, r.SegmentCount = 1, 1 })
	seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)

	info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
		ReportRecordSegment(segmentReq(task.RecordId, 4, model.SegmentStateUploaded))
	info = wantOK(t, info, err, "跳号登记")
	wantSegmentEchoesRow(t, "跳号登记", info, mustSegment(t, db, task.RecordId, 4))

	wantField(t, "跳号登记", "补洞后行数 = 1 片 + 2 洞 + 本片", int64(countSegments(db, task.RecordId)), int64(4))
	for _, seq := range []int64{2, 3} {
		row := mustSegment(t, db, task.RecordId, seq)
		wantField(t, "跳号登记", "洞的 state", row.State, model.SegmentStateMissing)
		wantField(t, "跳号登记", "洞无 bucket", row.Bucket, "")
		wantField(t, "跳号登记", "洞无 object_key", row.ObjectKey, "")
		wantField(t, "跳号登记", "洞无 checksum", row.Checksum, "")
		wantField(t, "跳号登记", "洞无 size_bytes", row.SizeBytes, int64(0))
		wantField(t, "跳号登记", "洞的起点按锚点反推", row.StartAt, task.StartAt+(seq-1)*int64(task.SegmentSeconds))
		wantField(t, "跳号登记", "洞的终点", row.EndAt, task.StartAt+seq*int64(task.SegmentSeconds))
		wantField(t, "跳号登记", "洞的时长 = segment_seconds", row.DurationMs, int64(task.SegmentSeconds*1000))
		wantField(t, "跳号登记", "洞归属自任务（不取自入参）", row.RoomId, task.RoomId)
	}

	stored := mustRecord(t, db, task.RecordId)
	wantField(t, "跳号登记", "last_seq 随本片前进", stored.LastSeq, int64(4))
	wantField(t, "跳号登记", "segment_count 含洞", stored.SegmentCount, int64(4))
	wantField(t, "跳号登记", "gap_count", stored.GapCount, int64(2))
	wantField(t, "跳号登记", "recorded_duration_ms 不含洞", stored.RecordedDuration, int64(10000))
	wantField(t, "跳号登记", "version 不变", stored.Version, int64(1))

	wantEvents(t, db, []string{model.EventTypeRecordGapDetected}, "补洞只发一条事件")
	p := eventPayload(t, eventAt(t, db, 0))
	wantField(t, "跳号登记", "from_seq = 旧水位+1", toInt64(t, p, "from_seq"), int64(2))
	wantField(t, "跳号登记", "to_seq = 本片", toInt64(t, p, "to_seq"), int64(4))
	wantField(t, "跳号登记", "missing_recorded", toInt64(t, p, "missing_recorded"), int64(2))
	wantField(t, "跳号登记", "missing_untraced", toInt64(t, p, "missing_untraced"), int64(0))
	wantField(t, "跳号登记", "record_id", toInt64(t, p, "record_id"), task.RecordId)
	wantField(t, "跳号登记", "room_id", toInt64(t, p, "room_id"), task.RoomId)
	if got, _ := p["worker_id"].(string); got != "worker-a" {
		t.Errorf("跳号登记：worker_id 应为本次上报方，实际 %v", p["worker_id"])
	}
	wantNoLeak(t, db, "跳号登记")
}

// 补不出洞的两种情形：没有锚点/分片时长（反推不出时间轴），以及跳跃量级超过
// maxGapFillRows（几乎一定是 Worker 传错了 record_id 或序号）。
// 两者都不得写垃圾行，但必须把「丢了多少片说不清」如实放进事件，让下游知道这块无法拼接。
func TestReportRecordSegmentUnfillableGapReportedAsUntraced(t *testing.T) {
	t.Run("缺锚点", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := recordingTask(db, func(r *model.LiveRecordTask) {
			r.StartAt, r.RecordStartAt, r.SegmentSeconds, r.LastSeq = 0, 0, 0, 1
		})
		seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)

		_, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
			ReportRecordSegment(segmentReq(task.RecordId, 4, model.SegmentStateUploaded))
		wantOK(t, struct{}{}, err, "缺锚点仍收下已上报的片")
		wantField(t, "缺锚点", "只落了本片（不臆造时间轴）", int64(countSegments(db, task.RecordId)), int64(2))
		wantField(t, "缺锚点", "本片 seq", mustSegment(t, db, task.RecordId, 4).Seq, int64(4))
		wantEvents(t, db, []string{model.EventTypeRecordGapDetected}, "缺锚点")
		p := eventPayload(t, eventAt(t, db, 0))
		wantField(t, "缺锚点", "missing_recorded", toInt64(t, p, "missing_recorded"), int64(0))
		wantField(t, "缺锚点", "missing_untraced = 说不清的洞数", toInt64(t, p, "missing_untraced"), int64(2))
		wantField(t, "缺锚点", "计数由重算得到", mustRecord(t, db, task.RecordId).SegmentCount, int64(2))
		wantNoLeak(t, db, "缺锚点")
	})

	// 跳跃 5000 个序号：宁可不补，也不能一次写进几千行 MISSING ——
	// 那会把切片表变成垃圾场，还会让 gap_count 报出一个荒谬数字。
	t.Run("超过补洞上限", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq = 1 })
		seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)

		_, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
			ReportRecordSegment(segmentReq(task.RecordId, 5001, model.SegmentStateUploaded))
		wantOK(t, struct{}{}, err, "超大跳跃仍收下本片")

		wantField(t, "超过补洞上限", "行数 = 1 片 + 本片", int64(countSegments(db, task.RecordId)), int64(2))
		stored := mustRecord(t, db, task.RecordId)
		wantField(t, "超过补洞上限", "gap_count 不被灌成 4999", stored.GapCount, int64(0))
		wantField(t, "超过补洞上限", "last_seq 仍是本片 seq", stored.LastSeq, int64(5001))
		wantEvents(t, db, []string{model.EventTypeRecordGapDetected}, "超过补洞上限")
		p := eventPayload(t, eventAt(t, db, 0))
		wantField(t, "超过补洞上限", "missing_untraced", toInt64(t, p, "missing_untraced"), int64(4999))
		wantField(t, "超过补洞上限", "missing_recorded", toInt64(t, p, "missing_recorded"), int64(0))
		wantNoLeak(t, db, "超过补洞上限")
	})
}

// 并发补洞：两个 Worker 同时探测到同一段空洞时，洞只能记一次、事件也只能有一条。
// 钩子在 InsertIgnoreMissingTx 被调用的瞬间替对手把洞填好，于是本次补洞命中 0 行。
func TestReportRecordSegmentConcurrentGapFillEmitsNoSecondEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq, r.SegmentCount = 1, 1 })
	seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)
	db.onHit("Segments.InsertIgnoreMissingTx", func() {
		for _, seq := range []int64{2, 3} {
			gapSegment(db, task.RecordId, seq, task.StartAt)
			mustSegment(t, db, task.RecordId, seq).TraceId = "trace-racer"
		}
	})

	_, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
		ReportRecordSegment(segmentReq(task.RecordId, 4, model.SegmentStateUploaded))
	wantOK(t, struct{}{}, err, "对手已补过洞")

	wantField(t, "并发补洞", "没有第二份洞", int64(countSegments(db, task.RecordId)), int64(4))
	wantField(t, "并发补洞", "对手的洞不被改写", mustSegment(t, db, task.RecordId, 2).TraceId, "trace-racer")
	wantField(t, "并发补洞", "本片照常落库", mustSegment(t, db, task.RecordId, 4).State, model.SegmentStateUploaded)
	// 一条事件都不该有：newMissing=0（洞不是本次补的）且 unfillable=0（洞有据可查）。
	wantEvents(t, db, nil, "并发补洞不得补第二条事件")
	wantCalls(t, db, "Segments.InsertIgnoreMissingTx", 0, 1, "补洞语句只发一次")
	stored := mustRecord(t, db, task.RecordId)
	wantField(t, "并发补洞", "gap_count 由重算得到（不靠本次增量）", stored.GapCount, int64(2))
	wantNoLeak(t, db, "并发补洞")
}

// 终态拒绝新切片：STOPPED/CANCELLED 的任务再收片会产生无人认领的孤儿对象。
// FAILED 不在其列 —— 它能被断点续录复活，拒收就会丢掉续录后的第一批片。
func TestReportRecordSegmentRejectsTerminalTaskButAcceptsFailedResume(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int32
	}{
		{"STOPPED", model.RecordStateStopped},
		{"CANCELLED", model.RecordStateCancelled},
	} {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := recordingTask(db, func(r *model.LiveRecordTask) { r.State = tc.state })
		before := snapshotWrites(db)

		info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
			ReportRecordSegment(segmentReq(task.RecordId, 1, model.SegmentStateUploaded))
		wantFail(t, info, err, model.ErrTerminalState, tc.name+"任务登记切片")
		wantNoWrites(t, db, before, tc.name+"任务登记切片")
		wantEvents(t, db, nil, tc.name+"任务登记切片")
		wantField(t, tc.name, "一行切片都没落", int64(countSegments(db, task.RecordId)), int64(0))
	}

	db := newStore()
	svcCtx := newTestSvc(db)
	failed := recordingTask(db, func(r *model.LiveRecordTask) { r.State = model.RecordStateFailed })
	_, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
		ReportRecordSegment(segmentReq(failed.RecordId, 1, model.SegmentStateUploaded))
	wantOK(t, struct{}{}, err, "FAILED 续录后登记切片")
	wantField(t, "FAILED 可续录", "state 不被登记改写", mustRecord(t, db, failed.RecordId).State, model.RecordStateFailed)
	wantEvents(t, db, nil, "FAILED 续录登记")
}

func TestReportRecordSegmentValidatesInputAndWritesNothing(t *testing.T) {
	longObjectKey := strings.Repeat("k", maxObjectKeyRunes+1)
	longChecksum := strings.Repeat("a", maxChecksumRunes+1)

	cases := []struct {
		name   string
		mutate func(*rpc.ReportRecordSegmentReq)
		// want 为 nil 表示「必须被拒绝，但不保证包装了哨兵」（缺陷 #4：helpers.go 的列宽
		// 超限分支没有 %w，见 record_lifecycle_test.go 同一处标注）。
		want error
	}{
		{"record_id 为零", func(r *rpc.ReportRecordSegmentReq) { r.RecordId = 0 }, model.ErrRecordTaskNotFound},
		{"record_id 为负", func(r *rpc.ReportRecordSegmentReq) { r.RecordId = -1 }, model.ErrRecordTaskNotFound},
		{"seq 为零", func(r *rpc.ReportRecordSegmentReq) { r.Seq = 0 }, model.ErrInvalidSeq},
		{"seq 为负", func(r *rpc.ReportRecordSegmentReq) { r.Seq = -3 }, model.ErrInvalidSeq},
		{"状态未指定", func(r *rpc.ReportRecordSegmentReq) { r.State = rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED }, model.ErrInvalidTransition},
		{"状态越界", func(r *rpc.ReportRecordSegmentReq) { r.State = rpc.SegmentState(9) }, model.ErrInvalidTransition},
		{"起点为零", func(r *rpc.ReportRecordSegmentReq) { r.StartAt = 0 }, model.ErrInvalidSegmentRange},
		{"终点等于起点", func(r *rpc.ReportRecordSegmentReq) { r.EndAt = r.StartAt }, model.ErrInvalidSegmentRange},
		{"终点早于起点", func(r *rpc.ReportRecordSegmentReq) { r.EndAt = r.StartAt - 1 }, model.ErrInvalidSegmentRange},
		{"时长为零", func(r *rpc.ReportRecordSegmentReq) { r.DurationMs = 0 }, model.ErrInvalidSegmentRange},
		{"时长为负", func(r *rpc.ReportRecordSegmentReq) { r.DurationMs = -1 }, model.ErrInvalidSegmentRange},
		{"字节数为负", func(r *rpc.ReportRecordSegmentReq) { r.SizeBytes = -1 }, model.ErrInvalidSegmentRange},
		{"缺 object_key", func(r *rpc.ReportRecordSegmentReq) { r.ObjectKey = "" }, model.ErrInvalidBucketRef},
		{"object_key 带签名参数", func(r *rpc.ReportRecordSegmentReq) { r.ObjectKey = leakObjectKey }, model.ErrInvalidBucketRef},
		{"object_key 是绝对地址", func(r *rpc.ReportRecordSegmentReq) { r.ObjectKey = "https://x/a.m3u8" }, model.ErrInvalidBucketRef},
		{"object_key 以斜杠开头", func(r *rpc.ReportRecordSegmentReq) { r.ObjectKey = "/rec/a.m3u8" }, model.ErrInvalidBucketRef},
		{"object_key 超列宽", func(r *rpc.ReportRecordSegmentReq) { r.ObjectKey = longObjectKey }, nil},
		{"桶名写成 URL", func(r *rpc.ReportRecordSegmentReq) { r.Bucket = "https://live-rec" }, model.ErrInvalidBucketRef},
		{"checksum 超列宽", func(r *rpc.ReportRecordSegmentReq) { r.Checksum = longChecksum }, model.ErrInvalidSegmentRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			task := recordingTask(db, nil)
			in := segmentReq(task.RecordId, 1, model.SegmentStateUploaded)
			tc.mutate(in)
			before := snapshotWrites(db)

			info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).ReportRecordSegment(in)
			if err == nil {
				t.Fatalf("%s：应被拒绝，实际返回 %+v", tc.name, info)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("%s：应报 %v，实际 err=%v", tc.name, tc.want, err)
			}
			if !isNilPtr(info) {
				t.Fatalf("%s：失败路径不得带回响应体：%+v", tc.name, info)
			}
			// 入参门禁在查库之前：非法请求不得占用主连接。
			wantCalls(t, db, "RecordTasks.FindOne", 0, 0, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
		})
	}
}

// 切片行、派生计数与缺口事件同生共死：事件写失败必须连行一起回滚，
// 否则出现「库里有个洞、下游从未被告知」的静默断档。
func TestReportRecordSegmentRollsBackRowsAndEventWhenOutboxFails(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq, r.SegmentCount = 1, 1 })
	seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)
	segSnapshot := *mustSegment(t, db, task.RecordId, 1)
	taskSnapshot := *mustRecord(t, db, task.RecordId)
	db.failOn("Outbox.Insert", errOutboxDown)

	info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
		ReportRecordSegment(segmentReq(task.RecordId, 4, model.SegmentStateUploaded))
	if err == nil || !strings.Contains(err.Error(), errOutboxDown.Error()) {
		t.Fatalf("事件写故障必须原样抛出：%v", err)
	}
	if !isNilPtr(info) {
		t.Errorf("失败路径不得带回响应体：%+v", info)
	}
	wantField(t, "事件故障回滚", "本片与洞一起回滚", int64(countSegments(db, task.RecordId)), int64(1))
	assertSegmentUnchanged(t, db, task.RecordId, 1, segSnapshot)
	assertRowUnchanged(t, db, task.RecordId, taskSnapshot)
	wantEvents(t, db, nil, "事件故障回滚")
	wantNoLeak(t, db, "事件故障回滚")
}

// 每个 model 依赖点都要 fail closed：读不动不得当成「没有切片」，写不动不得当成「已登记」。
func TestReportRecordSegmentFailsClosedOnModelErrors(t *testing.T) {
	t.Run("预读任务失败", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := recordingTask(db, nil)
		db.failOn("RecordTasks.FindOne", errModelDown)
		before := snapshotWrites(db)

		_, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
			ReportRecordSegment(segmentReq(task.RecordId, 1, model.SegmentStateUploaded))
		wantModelDown(t, err)
		wantNoWrites(t, db, before, "预读任务失败")
		wantEvents(t, db, nil, "预读任务失败")
	})

	for _, op := range []string{"Segments.InsertIgnoreTx", "Segments.InsertIgnoreMissingTx", "RecordTasks.RefreshStatsTx"} {
		t.Run(op+" 失败", func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq, r.SegmentCount = 1, 1 })
			seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)
			taskSnapshot := *mustRecord(t, db, task.RecordId)
			db.failOn(op, errModelDown)

			_, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
				ReportRecordSegment(segmentReq(task.RecordId, 4, model.SegmentStateUploaded))
			wantModelDown(t, err)
			wantField(t, op+" 失败", "半成品行不留库", int64(countSegments(db, task.RecordId)), int64(1))
			assertRowUnchanged(t, db, task.RecordId, taskSnapshot)
			wantEvents(t, db, nil, op+" 失败")
		})
	}

	// 提交之后才失败：数据已经落库，此时只能报错让调用方重投（重投会命中 uniq_record_seq 幂等），
	// 绝不能退化成「当作成功」或返回一个零值 Info。
	t.Run("Segments.FindBySeq 提交后失败", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq, r.SegmentCount = 1, 1 })
		seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)
		db.failOn("Segments.FindBySeq", errModelDown)

		info, err := NewReportRecordSegmentLogic(context.Background(), svcCtx).
			ReportRecordSegment(segmentReq(task.RecordId, 4, model.SegmentStateUploaded))
		wantModelDown(t, err)
		if !isNilPtr(info) {
			t.Errorf("回读失败不得带回响应体：%+v", info)
		}
		// 单位工作已在事务里提交：本片 + 两个洞都在，事件也在（回读失败不许顺带「撤销」它们）。
		wantField(t, "提交后回读失败", "已提交的行都在库", int64(countSegments(db, task.RecordId)), int64(4))
		wantField(t, "提交后回读失败", "已提交的计数都在", mustRecord(t, db, task.RecordId).LastSeq, int64(4))
		wantEvents(t, db, []string{model.EventTypeRecordGapDetected}, "提交后回读失败")
	})
}

func wantModelDown(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("依赖故障必须原样抛出（不得伪装成空结果或静默成功）：%v", err)
	}
}

// ---------------------------------------------------------------- GetLiveRecordTask

func TestGetLiveRecordTaskReturnsEveryCommittedColumn(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) {
		r.SourceTaskId, r.StartAt, r.EndAt = 777, 1000000, 1003600
		r.RecordStartAt, r.RecordEndAt, r.SegmentSeconds = 1000005, 0, 10
		r.LastSeq, r.SegmentCount, r.GapCount, r.RecordedDuration = 42, 44, 2, 420000
		r.Reason, r.Errno, r.ErrMsg = model.ReasonTimeout, 11, "heartbeat missed"
		r.Version, r.TraceId = 4, "trace-rec-get"
	})
	before := snapshotWrites(db)

	info, err := NewGetLiveRecordTaskLogic(context.Background(), svcCtx).GetLiveRecordTask(getRecordReq(task.RecordId))
	info = wantOK(t, info, err, "查询录制任务")
	wantRecordEchoesRow(t, "查询录制任务", info, mustRecord(t, db, task.RecordId))
	if info.GetRecordId() != task.RecordId || info.GetLastSeq() != 42 {
		t.Fatalf("读接口必须报出库里的水位：record_id=%d last_seq=%d", info.GetRecordId(), info.GetLastSeq())
	}

	// 缓存恒为 nil（未配置 Redis）：必然回源，且两次读结果一致、version 不被读操作推进。
	info2, err := NewGetLiveRecordTaskLogic(context.Background(), svcCtx).GetLiveRecordTask(getRecordReq(task.RecordId))
	info2 = wantOK(t, info2, err, "第二次查询")
	wantRecordEchoesRow(t, "第二次查询", info2, mustRecord(t, db, task.RecordId))
	wantField(t, "第二次查询", "version 不变", mustRecord(t, db, task.RecordId).Version, int64(4))

	// 读侧重算派生列 = 给只读账号开了写路径，还会撞废 Worker 正在用的 version。
	wantCalls(t, db, "RecordTasks.RefreshStats", 0, 0, "读接口不得重算统计")
	wantCalls(t, db, "RecordTasks.RefreshStatsTx", 0, 0, "读接口不得重算统计")
	wantNoWrites(t, db, before, "查询录制任务")
	wantEvents(t, db, nil, "查询录制任务")
}

// 派生列脏了也照实报：修复它的是写路径上的 RefreshStats，不是读接口顺手改。
func TestGetLiveRecordTaskReportsStaleDerivedColumnsVerbatim(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := seedRecord(db, model.RecordStateStopped, func(r *model.LiveRecordTask) {
		r.SegmentCount, r.GapCount, r.LastSeq, r.RecordedDuration = 99, 7, 3, 120000
	})

	info, err := NewGetLiveRecordTaskLogic(context.Background(), svcCtx).GetLiveRecordTask(getRecordReq(task.RecordId))
	info = wantOK(t, info, err, "脏计数照实返回")
	wantField(t, "脏计数", "segment_count", info.GetSegmentCount(), int64(99))
	wantField(t, "脏计数", "gap_count", info.GetGapCount(), int64(7))
	wantField(t, "脏计数", "库里的真实切片行数（说明读侧没重算）", int64(countSegments(db, task.RecordId)), int64(0))
	wantField(t, "脏计数", "行仍是脏的", mustRecord(t, db, task.RecordId).SegmentCount, int64(99))
}

func TestGetLiveRecordTaskRejectsNonPositiveIDWithoutQuerying(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedRecord(db, model.RecordStateRecording, nil)
	before := snapshotWrites(db)

	for _, id := range []int64{0, -1} {
		info, err := NewGetLiveRecordTaskLogic(context.Background(), svcCtx).GetLiveRecordTask(getRecordReq(id))
		wantFail(t, info, err, model.ErrRecordTaskNotFound, "非正数 record_id")
	}
	wantCalls(t, db, "RecordTasks.FindOne", 0, 0, "非正数 record_id 不得查库")
	wantNoWrites(t, db, before, "非正数 record_id")
	wantEvents(t, db, nil, "非正数 record_id")
}

// 查不到必须是错误，不能是零值 Info：Worker 会把 last_seq=0 读成「从第一片重录」，
// 于是覆盖已登记切片；version=0 又会让它之后的每次上报永远撞版本冲突。
func TestGetLiveRecordTaskMissingRowIsNotFoundNotZeroInfo(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedRecord(db, model.RecordStateRecording, nil)

	info, err := NewGetLiveRecordTaskLogic(context.Background(), svcCtx).GetLiveRecordTask(getRecordReq(4242))
	wantFail(t, info, err, model.ErrRecordTaskNotFound, "不存在的录制任务")
	wantCalls(t, db, "RecordTasks.FindOne", 0, 1, "不存在的录制任务应真的查过一次")
	wantNoLeak(t, db, "不存在的录制任务")
}

func TestGetLiveRecordTaskFailsClosedOnReadError(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := seedRecord(db, model.RecordStateRecording, nil)
	db.failOn("RecordTasks.FindOne", errModelDown)

	info, err := NewGetLiveRecordTaskLogic(context.Background(), svcCtx).GetLiveRecordTask(getRecordReq(task.RecordId))
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("读故障必须原样抛出（不得伪装成 not found）：%v", err)
	}
	if !isNilPtr(info) {
		t.Errorf("读故障路径不得带回响应体：%+v", info)
	}
	wantEvents(t, db, nil, "读故障")
}

// ---------------------------------------------------------------- ListLiveRecordTasks

func TestListLiveRecordTasksPassesFiltersToModel(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	a := seedRecord(db, model.RecordStateRecording, nil)
	b := seedRecord(db, model.RecordStateStopped, func(r *model.LiveRecordTask) { r.LiveSession = testSession2 })
	c := seedRecord(db, model.RecordStateRecording, func(r *model.LiveRecordTask) { r.RoomId = testRoomOther })

	svc := NewListLiveRecordTasksLogic(context.Background(), svcCtx)
	in := listRecordsReq(1, 10)
	in.RoomId = testRoomID
	reply, err := svc.ListLiveRecordTasks(in)
	reply = wantOK(t, reply, err, "按房间过滤")
	wantField(t, "按房间过滤", "行数", int32(len(reply.GetTasks())), int32(2))
	wantField(t, "按房间过滤", "total", reply.GetPage().GetTotal(), int32(2))
	wantField(t, "按房间过滤", "排序为 record_id DESC",
		joinInt64([]int64{reply.GetTasks()[0].GetRecordId(), reply.GetTasks()[1].GetRecordId()}),
		joinInt64([]int64{b.RecordId, a.RecordId}))
	q := db.lastRecordList
	wantField(t, "按房间过滤", "透传 room_id", q.RoomId, testRoomID)
	wantField(t, "按房间过滤", "未给的场次不参与过滤", q.SessionId, int64(0))
	wantField(t, "按房间过滤", "状态未过滤", q.State, int32(0))
	wantField(t, "按房间过滤", "透传 ps", q.Ps, int32(10))
	wantField(t, "按房间过滤", "透传 max_ps（夹取口径由 model 与 logic 共用）", q.MaxPageSize, int32(50))

	// 场次 + 状态一起过滤：三行里只剩 b。
	in2 := listRecordsReq(1, 10)
	in2.RoomId = testRoomID
	in2.LiveSessionId = testSession2
	in2.State = rpc.LiveRecordState_LIVE_RECORD_STATE_STOPPED
	reply2, err := svc.ListLiveRecordTasks(in2)
	reply2 = wantOK(t, reply2, err, "按场次+状态过滤")
	wantField(t, "按场次+状态过滤", "行数", int32(len(reply2.GetTasks())), int32(1))
	wantField(t, "按场次+状态过滤", "命中行", reply2.GetTasks()[0].GetRecordId(), b.RecordId)
	wantField(t, "按场次+状态过滤", "透传 session_id", db.lastRecordList.SessionId, testSession2)
	wantField(t, "按场次+状态过滤", "透传 state", db.lastRecordList.State, model.RecordStateStopped)
	wantField(t, "按场次+状态过滤", "total 是过滤后的总数", reply2.GetPage().GetTotal(), int32(1))

	// 全表读（运营排障）仍允许，房间/场次原样透传：model 只在 >0 时附加条件。
	reply3, err := svc.ListLiveRecordTasks(listRecordsReq(1, 10))
	reply3 = wantOK(t, reply3, err, "不带过滤条件")
	wantField(t, "不带过滤条件", "行数（含跨房间）", int32(len(reply3.GetTasks())), int32(3))
	if _, ok := findRecordRow(reply3, c.RecordId); !ok {
		t.Errorf("跨房间的行必须出现（本方法不是房间隔离读口）：%+v", reply3)
	}

	// 未知的枚举取值不得退化成「恒空结果集」：那会把调用方的取值错误伪装成「没有数据」。
	before := snapshotWrites(db)
	listCalls := db.count("RecordTasks.List")
	bad := listRecordsReq(1, 10)
	bad.State = rpc.LiveRecordState(9)
	info, err := svc.ListLiveRecordTasks(bad)
	wantFail(t, info, err, model.ErrInvalidTransition, "状态过滤值越界")
	wantCalls(t, db, "RecordTasks.List", listCalls, 0, "越界枚举不得查库")
	wantNoWrites(t, db, before, "状态过滤值越界")
	wantEvents(t, db, nil, "状态过滤值越界")
}

func findRecordRow(reply *rpc.ListLiveRecordTasksReply, recordID int64) (*rpc.LiveRecordTaskInfo, bool) {
	for _, row := range reply.GetTasks() {
		if row.GetRecordId() == recordID {
			return row, true
		}
	}
	return nil, false
}

func TestListLiveRecordTasksPagingAndClamping(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	for i := 0; i < 3; i++ {
		seedRecord(db, model.RecordStateRecording, nil)
	}
	svc := NewListLiveRecordTasksLogic(context.Background(), svcCtx)

	first, err := svc.ListLiveRecordTasks(listRecordsReq(1, 2))
	first = wantOK(t, first, err, "第一页")
	wantField(t, "第一页", "行数", int32(len(first.GetTasks())), int32(2))
	wantField(t, "第一页", "total 是全量数而非本页数", first.GetPage().GetTotal(), int32(3))

	second, err := svc.ListLiveRecordTasks(listRecordsReq(2, 2))
	second = wantOK(t, second, err, "第二页")
	wantField(t, "第二页", "行数", int32(len(second.GetTasks())), int32(1))
	if _, ok := findRecordRow(second, first.GetTasks()[0].GetRecordId()); ok {
		t.Errorf("第二页与第一页首行重叠：OFFSET 没生效")
	}

	if _, err := svc.ListLiveRecordTasks(listRecordsReq(1, 999)); err != nil {
		t.Fatalf("ps 越界应夹取而不是报错：%v", err)
	}
	wantField(t, "ps 越界", "夹取后的 ps", db.lastRecordList.Ps, int32(20))

	// Page 整个为空也必须能读（proto 里 page 是可选字段）。
	if _, err := svc.ListLiveRecordTasks(&rpc.ListLiveRecordTasksReq{}); err != nil {
		t.Fatalf("缺 page 应归一为 1/20：%v", err)
	}
	wantField(t, "缺 page", "pn", db.lastRecordList.Pn, int32(1))
	wantField(t, "缺 page", "ps", db.lastRecordList.Ps, int32(20))

	if _, err := svc.ListLiveRecordTasks(listRecordsReq(-3, -5)); err != nil {
		t.Fatalf("pn/ps 非正数应归一而不是报错：%v", err)
	}
	wantField(t, "pn/ps 归一", "pn", db.lastRecordList.Pn, int32(1))
	wantField(t, "pn/ps 归一", "ps", db.lastRecordList.Ps, int32(20))

	// OFFSET 的代价是「线性扫描后丢弃的行数」，页码乘页大小就是扫描面：超窗口拒绝且不查库。
	listCalls := db.count("RecordTasks.List")
	before := snapshotWrites(db)
	deep, err := svc.ListLiveRecordTasks(listRecordsReq(int32(maxListOffset/20+2), 20))
	wantFail(t, deep, err, model.ErrInvalidPage, "深翻页")
	if !strings.Contains(err.Error(), "OFFSET") {
		t.Errorf("错误必须说明是 OFFSET 代价失控：%v", err)
	}
	wantCalls(t, db, "RecordTasks.List", listCalls, 0, "深翻页不得扫库")
	wantNoWrites(t, db, before, "深翻页")

	// 窗口边缘（offset == maxListOffset）仍允许：判定用的是夹取后的 ps，不是客户端传来的数字。
	if _, err := svc.ListLiveRecordTasks(listRecordsReq(int32(maxListOffset/20+1), 999)); err != nil {
		t.Fatalf("恰好落在窗口边缘不应拒绝：%v", err)
	}
}

// 逐行投影必须与库里的行一致，并且读接口不重算派生列。
func TestListLiveRecordTasksProjectsRowsVerbatim(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	a := seedRecord(db, model.RecordStateStopped, func(r *model.LiveRecordTask) {
		r.SegmentCount, r.GapCount, r.LastSeq, r.RecordedDuration = 99, 7, 3, 120000
		r.Reason, r.Errno, r.ErrMsg = model.ReasonManual, 0, "stopped by ops"
		r.Version = 6
	})
	b := seedRecord(db, model.RecordStateFailed, func(r *model.LiveRecordTask) {
		r.RoomId, r.LiveSession, r.OutputPrefix = testRoomOther, testSession2, "rec/71002"
	})
	before := snapshotWrites(db)

	reply, err := NewListLiveRecordTasksLogic(context.Background(), svcCtx).ListLiveRecordTasks(listRecordsReq(1, 10))
	reply = wantOK(t, reply, err, "逐行投影")
	wantField(t, "逐行投影", "行数", int32(len(reply.GetTasks())), int32(2))
	wantRecordEchoesRow(t, "逐行投影/最新行", reply.GetTasks()[0], mustRecord(t, db, b.RecordId))
	wantRecordEchoesRow(t, "逐行投影/次行", reply.GetTasks()[1], mustRecord(t, db, a.RecordId))
	// 「脏 99/7」照实返回：修复派生列是写路径（RefreshStats）的职责。
	wantField(t, "逐行投影", "脏计数不被读接口改写", reply.GetTasks()[1].GetSegmentCount(), int64(99))
	wantField(t, "逐行投影", "FAILED 如实可见（可续录，不是终态）",
		int32(reply.GetTasks()[0].GetState()), model.RecordStateFailed)
	wantCalls(t, db, "RecordTasks.RefreshStats", 0, 0, "列表不得重算统计")
	wantNoWrites(t, db, before, "逐行投影")
	wantEvents(t, db, nil, "逐行投影")
}

func TestListLiveRecordTasksEmptyResultAndFailClosed(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)

	reply, err := NewListLiveRecordTasksLogic(context.Background(), svcCtx).ListLiveRecordTasks(listRecordsReq(1, 10))
	reply = wantOK(t, reply, err, "空列表")
	wantField(t, "空列表", "行数", int32(len(reply.GetTasks())), int32(0))
	wantField(t, "空列表", "total", reply.GetPage().GetTotal(), int32(0))
	wantNoWrites(t, db, before, "空列表")
	wantEvents(t, db, nil, "空列表")

	db.failOn("RecordTasks.List", errModelDown)
	reply2, err := NewListLiveRecordTasksLogic(context.Background(), svcCtx).ListLiveRecordTasks(listRecordsReq(1, 10))
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("读故障必须原样抛出（不得伪装成空列表）：%v", err)
	}
	if !isNilPtr(reply2) {
		t.Errorf("读故障路径不得带回响应体：%+v", reply2)
	}
}

// ---------------------------------------------------------------- ListRecordSegments

func TestListRecordSegmentsWalksCursorToTheEnd(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, nil)
	for seq := int64(1); seq <= 5; seq++ {
		seedSegment(db, task.RecordId, seq, model.SegmentStateVerified, nil)
	}
	before := snapshotWrites(db)
	svc := NewListRecordSegmentsLogic(context.Background(), svcCtx)
	totalCalls := db.count("Segments.CountByRecord")

	var seen []int64
	after, rounds := int64(0), 0
	for {
		rounds++
		if rounds > 10 {
			t.Fatalf("游标没有推进（疑似死循环）：after=%d seen=%v", after, seen)
		}
		reply, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, after, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 2))
		reply = wantOK(t, reply, err, "keyset 翻页")
		wantField(t, "keyset 翻页", "total 恒为全量", reply.GetTotal(), int64(5))
		wantField(t, "keyset 翻页", "透传游标", db.lastSegmentList.afterSeq, after)
		seen = append(seen, replySegmentSeqs(reply.GetSegments())...)
		if !reply.GetHasMore() {
			// 末页只有一行：has_more 由「本页取满 limit」推出，不是由「游标到末尾」推出。
			wantField(t, "keyset 翻页", "末页游标停在最后一行 seq", reply.GetNextAfterSeq(), int64(5))
			break
		}
		if reply.GetNextAfterSeq() <= after {
			t.Fatalf("has_more 为真时游标必须前进：after=%d next=%d", after, reply.GetNextAfterSeq())
		}
		after = reply.GetNextAfterSeq()
	}
	wantField(t, "keyset 翻页", "翻三轮取满（5 行 / limit 2）", int64(rounds), int64(3))
	// 有序、不重不漏：拼接方就是靠这个序列判断时间轴完整性。
	wantField(t, "keyset 翻页", "覆盖的 seq", joinInt64(seen), "1,2,3,4,5")
	// 每页各算一次 total：多算说明读侧在拼凑第二份账，少算说明 total 是硬编码。
	wantCalls(t, db, "Segments.CountByRecord", totalCalls, rounds, "total 每页各算一次")

	wantField(t, "keyset 翻页", "排序固定 seq ASC", joinInt64(replySegmentSeqs(allSegmentsOf(t, svc, task.RecordId))), "1,2,3,4,5")
	wantSegmentsEchoStore(t, db, svc, task.RecordId)
	wantNoWrites(t, db, before, "keyset 翻页")
	wantEvents(t, db, nil, "keyset 翻页")
}

// allSegmentsOf 取一次「不限量」的整页，用于校验排序与逐行投影。
func allSegmentsOf(t *testing.T, svc *ListRecordSegmentsLogic, recordID int64) []*rpc.RecordSegmentInfo {
	t.Helper()
	reply, err := svc.ListRecordSegments(listSegmentsReq(recordID, 0, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 500))
	reply = wantOK(t, reply, err, "整页")
	return reply.GetSegments()
}

// wantSegmentsEchoStore 逐行比对「响应 == 库里的行」。
// 读接口一旦自己拼响应（少投影一列、把 object_key 拼成 CDN 地址、把 state 归一化），
// 只有拿库里的行当基准才看得出来。
func wantSegmentsEchoStore(t *testing.T, db *store, svc *ListRecordSegmentsLogic, recordID int64) {
	t.Helper()
	rows := allSegmentsOf(t, svc, recordID)
	if int64(len(rows)) != int64(countSegments(db, recordID)) {
		t.Fatalf("整页行数 %d 与库里 %d 不符", len(rows), countSegments(db, recordID))
	}
	for _, row := range rows {
		wantSegmentEchoesRow(t, "逐行投影", row, mustSegment(t, db, row.GetRecordId(), row.GetSeq()))
	}
}

func TestListRecordSegmentsCursorAndLimitNormalization(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, nil)
	seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)
	seedSegment(db, task.RecordId, 2, model.SegmentStateVerified, nil)
	svc := NewListRecordSegmentsLogic(context.Background(), svcCtx)
	before := snapshotWrites(db)

	// 负游标是传坏了：0 才是「从头」。静默归零会让客户端以为游标已推进，
	// 实际在同一批数据上死循环 —— 所以拒绝，且不查库。
	listCalls := db.count("Segments.ListAfter")
	info, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, -1, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 10))
	wantFail(t, info, err, model.ErrInvalidCursor, "负游标")
	if !strings.Contains(err.Error(), "0 means from start") {
		t.Errorf("错误必须说明 0 才是从头：%v", err)
	}
	wantCalls(t, db, "Segments.ListAfter", listCalls, 0, "负游标不得查库")

	// after_seq=0：从头开始，原样透传（不归一为 1，否则第一片会被跳过）。
	head, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, 0, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 10))
	head = wantOK(t, head, err, "游标 0")
	wantField(t, "游标 0", "透传 after_seq", db.lastSegmentList.afterSeq, int64(0))
	wantField(t, "游标 0", "第一片不被跳过", joinInt64(replySegmentSeqs(head.GetSegments())), "1,2")

	// limit 夹取而非拒绝：keyset 的 limit 不改变扫描量级（命中 uniq_record_seq 区间扫）。
	for _, tc := range []struct {
		name string
		in   int32
		want int32
	}{
		{"limit 为零取默认", 0, 200},
		{"limit 为负取默认", -5, 200},
		{"limit 越界夹到上限", 9999, 500},
		{"limit 合法原样透传", 7, 7},
	} {
		if _, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, 0, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, tc.in)); err != nil {
			t.Fatalf("%s：%v", tc.name, err)
		}
		wantField(t, tc.name, "透传 limit", db.lastSegmentList.limit, tc.want)
	}

	// 游标越过最后一行：空页 + 原样回传游标（同一游标可稳定重试），has_more=false。
	empty, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, 99, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 10))
	empty = wantOK(t, empty, err, "游标越过末尾")
	wantField(t, "游标越过末尾", "行数", int32(len(empty.GetSegments())), int32(0))
	wantField(t, "游标越过末尾", "游标原样回传", empty.GetNextAfterSeq(), int64(99))
	wantField(t, "游标越过末尾", "has_more", empty.GetHasMore(), false)
	wantField(t, "游标越过末尾", "total 仍是全量", empty.GetTotal(), int64(2))

	// 「本页取满即 has_more」：宁可让客户端再要一次空页，也不谎报「还有」。
	full, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, 0, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 2))
	full = wantOK(t, full, err, "取满 limit")
	wantField(t, "取满 limit", "行数", int32(len(full.GetSegments())), int32(2))
	wantField(t, "取满 limit", "has_more", full.GetHasMore(), true)
	wantField(t, "取满 limit", "next_after_seq = 本页末行", full.GetNextAfterSeq(), int64(2))
	tail, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, full.GetNextAfterSeq(), rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 2))
	tail = wantOK(t, tail, err, "跟随空页")
	wantField(t, "跟随空页", "has_more", tail.GetHasMore(), false)

	wantNoWrites(t, db, before, "游标与 limit 归一")
	wantEvents(t, db, nil, "游标与 limit 归一")
}

// 状态过滤只筛行，不改 total：两者口径不同是 proto 契约（total 含缺口行），
// 拼接方靠 total-vs-行数 的差才能发现「这一页被过滤掉了东西」。
func TestListRecordSegmentsStateFilterKeepsTotal(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, func(r *model.LiveRecordTask) { r.LastSeq = 4 })
	seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)
	gapSegment(db, task.RecordId, 2, task.StartAt)
	seedSegment(db, task.RecordId, 3, model.SegmentStateCorrupt, func(s *model.LiveRecordSegment) {
		s.Bucket, s.ObjectKey, s.Checksum, s.SizeBytes = "", "", "", 0
	})
	seedSegment(db, task.RecordId, 4, model.SegmentStateUploading, nil)
	before := snapshotWrites(db)
	svc := NewListRecordSegmentsLogic(context.Background(), svcCtx)

	all, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, 0, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 0))
	all = wantOK(t, all, err, "不过滤")
	wantField(t, "不过滤", "行数", int32(len(all.GetSegments())), int32(4))
	wantField(t, "不过滤", "透传 state=0", db.lastSegmentList.state, int32(0))

	missing, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, 0, rpc.SegmentState_SEGMENT_STATE_MISSING, 0))
	missing = wantOK(t, missing, err, "只看缺口")
	wantField(t, "只看缺口", "行数", int32(len(missing.GetSegments())), int32(1))
	wantField(t, "只看缺口", "total 不随过滤变化", missing.GetTotal(), int64(4))
	wantField(t, "只看缺口", "透传 state", db.lastSegmentList.state, model.SegmentStateMissing)
	wantField(t, "只看缺口", "seq", missing.GetSegments()[0].GetSeq(), int64(2))
	// 缺口行在响应里也必须「无引用」：一旦带上 key，下游就会去取一个不存在的对象。
	wantField(t, "只看缺口", "bucket 为空", missing.GetSegments()[0].GetBucket(), "")
	wantField(t, "只看缺口", "object_key 为空", missing.GetSegments()[0].GetObjectKey(), "")
	wantField(t, "只看缺口", "size_bytes 为 0", missing.GetSegments()[0].GetSizeBytes(), int64(0))

	// 过滤后为空：游标原样回传、has_more=false，total 仍给全量。
	none, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, 0, rpc.SegmentState_SEGMENT_STATE_UPLOADED, 0))
	none = wantOK(t, none, err, "过滤后为空")
	wantField(t, "过滤后为空", "行数", int32(len(none.GetSegments())), int32(0))
	wantField(t, "过滤后为空", "游标原样回传", none.GetNextAfterSeq(), int64(0))
	wantField(t, "过滤后为空", "has_more", none.GetHasMore(), false)
	wantField(t, "过滤后为空", "total", none.GetTotal(), int64(4))

	// 未知取值不退化成「恒空」：那会把调用方的版本错误伪装成「没有数据」。
	listCalls := db.count("Segments.ListAfter")
	bad, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, 0, rpc.SegmentState(9), 0))
	wantFail(t, bad, err, model.ErrInvalidTransition, "切片状态过滤越界")
	wantCalls(t, db, "Segments.ListAfter", listCalls, 0, "越界枚举不得查库")

	wantNoWrites(t, db, before, "状态过滤")
	wantEvents(t, db, nil, "状态过滤")
}

// record_id 必填：切片表是只增不减的大表（三小时直播数千行），
// 没有 record_id 就是让它做全表扫，代价由下一个请求承担。
func TestListRecordSegmentsRejectsMissingRecordIDAndFailsClosed(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := recordingTask(db, nil)
	seedSegment(db, task.RecordId, 1, model.SegmentStateVerified, nil)
	before := snapshotWrites(db)
	svc := NewListRecordSegmentsLogic(context.Background(), svcCtx)

	for _, id := range []int64{0, -1} {
		info, err := svc.ListRecordSegments(listSegmentsReq(id, 0, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 10))
		wantFail(t, info, err, model.ErrRecordTaskNotFound, "非正数 record_id")
	}
	wantCalls(t, db, "Segments.ListAfter", 0, 0, "非正数 record_id 不得查库")
	wantCalls(t, db, "Segments.CountByRecord", 0, 0, "非正数 record_id 不得数行")
	wantNoWrites(t, db, before, "非正数 record_id")

	db.failOn("Segments.ListAfter", errModelDown)
	if _, err := svc.ListRecordSegments(listSegmentsReq(task.RecordId, 0, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 10)); err == nil ||
		!strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("切片读故障必须原样抛出：%v", err)
	}

	// total 与行分两次读：第二次失败时不得只把行返回出去（total=0 会被下游读成「这片没录」）。
	db2 := newStore()
	svcCtx2 := newTestSvc(db2)
	task2 := recordingTask(db2, nil)
	seedSegment(db2, task2.RecordId, 1, model.SegmentStateVerified, nil)
	db2.failOn("Segments.CountByRecord", errModelDown)
	before2 := snapshotWrites(db2)
	reply, err := NewListRecordSegmentsLogic(context.Background(), svcCtx2).
		ListRecordSegments(listSegmentsReq(task2.RecordId, 0, rpc.SegmentState_SEGMENT_STATE_UNSPECIFIED, 10))
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("total 读失败必须报错，不得返回半份结果：%v", err)
	}
	if !isNilPtr(reply) {
		t.Errorf("失败路径不得带回响应体：%+v", reply)
	}
	wantEvents(t, db, nil, "读故障")
	wantNoWrites(t, db2, before2, "total 读失败")
}
