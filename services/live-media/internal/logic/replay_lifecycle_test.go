package logic

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"
)

// 回放写侧四个方法：SubmitReplayTask / ReportReplayProgress / BindReplayAsset / ApplyReplayContentState。
//
// 这条链路只有一个是「本服务自己的判断」，其余三个都在守边界，用例因此按四条防线组织：
//
//	SubmitReplayTask 裁决「这份录制能不能拼回放」：录制必须 STOPPED（FAILED 可续录、尾部还在补，
//	按它拼会产出永远缺尾巴的回放），缺口量由 allow_gaps 与配置上限 maxGapSegments 共同决定；
//	ReportReplayProgress 是 Worker 的进度回执，三个状态永不上报（PENDING 属提交侧、
//	CANCELLED 属调用方、COMPLETED 属 video 投影），越界一律失败关闭而不是静默忽略；
//	BindReplayAsset 只登记跨服务主键引用（asset_id/aid/bvid），不推进稿件、不发「已发布」类事件；
//	ApplyReplayContentState 是 video→live-media 的**只读投影通道**：它只能改五个投影列，
//	以及两条被契约允许的联动（REVIEW_SUBMITTED→COMPLETED、引用生命周期 Normal→Pending），
//	其余任何一列被它碰到都是越权。
//
// 幂等口径每个方法都不一样，所以各自钉住：提交侧靠 uniq_request_id 三层（回放/复用/撞键回读），
// 上报侧靠 expected_version + 「什么都不改才算重放」，绑定侧靠天然键 replay_id 与三个唯一键，
// 投影侧靠 last_event_id + review_state_at 单调。
//
// 事件只有两条：livemedia.replay.review.submitted（仅 REVIEW_SUBMITTED 这条边）与
// livemedia.replay.content.state.changed（仅投影真的落地时）。SubmitReplayTask 与
// BindReplayAsset 一条都不发 —— 词表里没有「回放已登记」「引用已绑定」这两类，
// 按「不伪造 topic/event_type」的口径必须为零，因此这两个方法每个用例都断言 wantEvents(nil)；
// 反过来，「业务写与 Outbox 同事务」这条只在真有事件的两条边上可测，
// 于是回滚用例落在 ReportReplayProgress 与 ApplyReplayContentState 上（见那两节）。
//
// 下游三个 zrpc client（svc.Asset/svc.Video/svc.Moderation）在本包恒为 nil：
// README §8.2 明确「当前没有任何 logic 调用它们」，所以 TestReplayMethodsNeverTouchNilClients
// 钉的是「七个方法在 nil 下游下全部照常工作（真调了就是 panic）」+
// 「需要下游事实才能成立的动作一律回显式哨兵，绝不为过门禁而伪造 asset_id/aid」。
//
// 交错用例用 db.onHit 在写入瞬间改库：回滚会撤销钩子造出来的态（见 fakes_test.go 头注释），
// 因此断言的是归因错误与「没有半成品」，不是交错后的最终库态。

// ---------------------------------------------------------------- 请求构造与小工具

const (
	replayAssetID       int64 = 60001 // asset 主键引用（跨服务，本包只当数字用）
	replayAid           int64 = 70001 // video 稿件主键引用
	replayBvid                = "BV1replay01"
	replayProductBucket       = "live-replay"
	replayProductKey          = "replay/71001/88001/merged.m3u8"
)

// stoppedRecord 一行「可拼回放的录制结论」：STOPPED + 时间轴锚点齐全。
// RecordStartAt/RecordEndAt 是 SubmitReplayTask 里 start_at/end_at 的回落来源，
// 不给就只能测到「两列恒为 0」这条 weakest 路径。
func stoppedRecord(db *store, mutate func(*model.LiveRecordTask)) *model.LiveRecordTask {
	return seedRecord(db, model.RecordStateStopped, func(r *model.LiveRecordTask) {
		r.StartAt, r.EndAt = 1000000, 1000050
		r.RecordStartAt, r.RecordEndAt = 1000000, 1000050
		r.LastSeq, r.SegmentCount, r.RecordedDuration = 5, 5, 50000
		if mutate != nil {
			mutate(r)
		}
	})
}

// seedVerifiedRun 落 seq 1..to 的连续 VERIFIED 切片：SubmitReplayTask 的素材事实源。
// 全 VERIFIED 时 gaps=0，用例里的「有洞」一律靠 mutate 把某几行改成 MISSING/CORRUPT 造。
func seedVerifiedRun(db *store, recordID, to int64, mutate func(*model.LiveRecordSegment)) {
	for seq := int64(1); seq <= to; seq++ {
		seedSegment(db, recordID, seq, model.SegmentStateVerified, func(s *model.LiveRecordSegment) {
			if mutate != nil {
				mutate(s)
			}
		})
	}
}

// seedHole 一行缺口行：真实现里 MISSING 行的 bucket/object_key/checksum 是空串
// （model/live_record_segment.go 的 InsertIgnoreMissing 里根本没有这三列），
// 种子若顺手给了引用，就等于告诉回收任务「这里有个可删对象」。
func seedHole(db *store, recordID, seq int64, state int32) *model.LiveRecordSegment {
	return seedSegment(db, recordID, seq, state, func(s *model.LiveRecordSegment) {
		s.Bucket, s.ObjectKey, s.Checksum, s.SizeBytes = "", "", "", 0
	})
}

func submitReplayReq(recordID int64, requestID string) *rpc.SubmitReplayTaskReq {
	return &rpc.SubmitReplayTaskReq{
		RoomId: testRoomID, LiveSessionId: testSession, RecordId: recordID,
		AnchorMid: testAnchor, Title: "昨晚直播回放", RequestId: requestID, TraceId: "trace-replay-1",
	}
}

func reportReplayReq(replayID int64, state rpc.ReplayState) *rpc.ReportReplayProgressReq {
	return &rpc.ReportReplayProgressReq{
		ReplayId: replayID, State: state, WorkerId: "worker-merge", TraceId: "trace-replay-report",
	}
}

func bindReplayReq(replayID int64, requestID string) *rpc.BindReplayAssetReq {
	return &rpc.BindReplayAssetReq{
		ReplayId: replayID, AssetId: replayAssetID, Aid: replayAid, Bvid: replayBvid,
		Bucket: replayProductBucket, ObjectKey: replayProductKey, DurationMs: 60000,
		RequestId: requestID, TraceId: "trace-replay-bind",
	}
}

func applyContentReq(replayID int64, state rpc.ReviewState, eventID string) *rpc.ApplyReplayContentStateReq {
	return &rpc.ApplyReplayContentStateReq{
		ReplayId: replayID, ReviewState: state, EventId: eventID,
		Source: "content.published.v1", TraceId: "trace-replay-projection",
	}
}

// registeredReplay 一行「产物已在对象存储、引用未回填」的回放任务：绑定与送审两条边的共同起跑线。
// output_bucket/output_key 必须与 bindReplayReq 的产物一致，否则先撞产物引用一致性门禁。
func registeredReplay(db *store, mutate func(*model.LiveReplayTask)) *model.LiveReplayTask {
	return seedReplay(db, model.ReplayStateRegistered, func(r *model.LiveReplayTask) {
		r.OutputBucket, r.OutputKey = replayProductBucket, replayProductKey
		r.DurationMs = 60000
		if mutate != nil {
			mutate(r)
		}
	})
}

func boundReplay(db *store, mutate func(*model.LiveReplayTask)) *model.LiveReplayTask {
	return registeredReplay(db, func(r *model.LiveReplayTask) {
		r.AssetId, r.Aid, r.Bvid = replayAssetID, replayAid, replayBvid
		if mutate != nil {
			mutate(r)
		}
	})
}

func mustReplay(t *testing.T, db *store, replayID int64) *model.LiveReplayTask {
	t.Helper()
	row, ok := db.replays[replayID]
	if !ok {
		t.Fatalf("回放任务 %d 不存在（库内 %d 行）", replayID, len(db.replays))
	}
	return row
}

func mustRef(t *testing.T, db *store, refID int64) *model.LiveReplayAssetRef {
	t.Helper()
	row, ok := db.replayRefs[refID]
	if !ok {
		t.Fatalf("回放资产引用 %d 不存在（库内 %d 行）", refID, len(db.replayRefs))
	}
	return row
}

func assertReplayUnchanged(t *testing.T, db *store, replayID int64, want model.LiveReplayTask, label string) {
	t.Helper()
	got := mustReplay(t, db, replayID)
	if *got != want {
		t.Fatalf("%s：非法路径改动了回放行：\n got=%+v\nwant=%+v", label, *got, want)
	}
}

func assertRefUnchanged(t *testing.T, db *store, refID int64, want model.LiveReplayAssetRef, label string) {
	t.Helper()
	got := mustRef(t, db, refID)
	if *got != want {
		t.Fatalf("%s：非法路径改动了引用行：\n got=%+v\nwant=%+v", label, *got, want)
	}
}

// stripReplayDrive 抹掉「状态推进本身要写的列」，剩下的列必须逐字节相同。
// 用它来证明某条路径只推进了状态机，没有顺手改写产物、区间、主键引用或标题。
func stripReplayDrive(r model.LiveReplayTask) model.LiveReplayTask {
	r.State, r.Version, r.Mtime, r.TraceId = 0, 0, 0, ""
	return r
}

// stripReplayProgress 再抹掉「本次上报可以顺带改写的产物账」：
// segment_count / gap_count / duration_ms 是 Worker 的事实，挂在状态边上更新是合法的。
// 与 stripReplayDrive 组合起来就是「除状态机与产物账之外，其余一列都不许动」的判据。
func stripReplayProgress(r model.LiveReplayTask) model.LiveReplayTask {
	r.SegmentCount, r.GapCount, r.DurationMs = 0, 0, 0
	return r
}

// stripRefProjection 抹掉五个投影列与本方法允许随行的两列（trace_id/mtime）。
// ApplyReplayContentState 的写权限边界就是这张清单：其余 18 列一律不得变化。
func stripRefProjection(r model.LiveReplayAssetRef) model.LiveReplayAssetRef {
	r.ReviewState, r.ReviewStateAt, r.PublishedAt, r.LastEventId, r.Source = 0, 0, 0, "", ""
	r.TraceId, r.Mtime = "", 0
	return r
}

func wantReplayEchoesRow(t *testing.T, label string, info *rpc.LiveReplayTaskInfo, row *model.LiveReplayTask) {
	t.Helper()
	wantField(t, label, "replay_id", info.GetReplayId(), row.ReplayId)
	wantField(t, label, "room_id", info.GetRoomId(), row.RoomId)
	wantField(t, label, "live_session_id", info.GetLiveSessionId(), row.LiveSession)
	wantField(t, label, "record_id", info.GetRecordId(), row.RecordId)
	wantField(t, label, "state", int32(info.GetState()), row.State)
	wantField(t, label, "from_seq", info.GetFromSeq(), row.FromSeq)
	wantField(t, label, "to_seq", info.GetToSeq(), row.ToSeq)
	wantField(t, label, "segment_count", info.GetSegmentCount(), row.SegmentCount)
	wantField(t, label, "gap_count", info.GetGapCount(), row.GapCount)
	wantField(t, label, "start_at", info.GetStartAt(), row.StartAt)
	wantField(t, label, "end_at", info.GetEndAt(), row.EndAt)
	wantField(t, label, "duration_ms", info.GetDurationMs(), row.DurationMs)
	wantField(t, label, "allow_gaps", info.GetAllowGaps(), row.AllowGaps != 0)
	wantField(t, label, "output_bucket", info.GetOutputBucket(), row.OutputBucket)
	wantField(t, label, "output_key", info.GetOutputKey(), row.OutputKey)
	wantField(t, label, "asset_id", info.GetAssetId(), row.AssetId)
	wantField(t, label, "aid", info.GetAid(), row.Aid)
	wantField(t, label, "bvid", info.GetBvid(), row.Bvid)
	wantField(t, label, "anchor_mid", info.GetAnchorMid(), row.AnchorMid)
	wantField(t, label, "title", info.GetTitle(), row.Title)
	wantField(t, label, "version", info.GetVersion(), row.Version)
	wantField(t, label, "reason", int32(info.GetReason()), row.Reason)
	wantField(t, label, "errno", info.GetErrno(), row.Errno)
	wantField(t, label, "err_msg", info.GetErrMsg(), row.ErrMsg)
	wantField(t, label, "request_id", info.GetRequestId(), row.RequestId)
	wantField(t, label, "trace_id", info.GetTraceId(), row.TraceId)
	wantField(t, label, "ctime", info.GetCtime(), row.Ctime)
	wantField(t, label, "mtime", info.GetMtime(), row.Mtime)
}

func wantRefEchoesRow(t *testing.T, label string, info *rpc.ReplayAssetRefInfo, row *model.LiveReplayAssetRef) {
	t.Helper()
	wantField(t, label, "id", info.GetId(), row.Id)
	wantField(t, label, "room_id", info.GetRoomId(), row.RoomId)
	wantField(t, label, "live_session_id", info.GetLiveSessionId(), row.LiveSession)
	wantField(t, label, "replay_id", info.GetReplayId(), row.ReplayId)
	wantField(t, label, "record_id", info.GetRecordId(), row.RecordId)
	wantField(t, label, "asset_id", info.GetAssetId(), row.AssetId)
	wantField(t, label, "aid", info.GetAid(), row.Aid)
	wantField(t, label, "bvid", info.GetBvid(), row.Bvid)
	wantField(t, label, "anchor_mid", info.GetAnchorMid(), row.AnchorMid)
	wantField(t, label, "bucket", info.GetBucket(), row.Bucket)
	wantField(t, label, "object_key", info.GetObjectKey(), row.ObjectKey)
	wantField(t, label, "duration_ms", info.GetDurationMs(), row.DurationMs)
	wantField(t, label, "segment_from_seq", info.GetSegmentFromSeq(), row.SegmentFromSeq)
	wantField(t, label, "segment_to_seq", info.GetSegmentToSeq(), row.SegmentToSeq)
	wantField(t, label, "gap_count", info.GetGapCount(), row.GapCount)
	wantField(t, label, "review_state", int32(info.GetReviewState()), row.ReviewState)
	wantField(t, label, "review_state_at", info.GetReviewStateAt(), row.ReviewStateAt)
	wantField(t, label, "retention_state", info.GetRetentionState(), row.RetentionState)
	wantField(t, label, "published_at", info.GetPublishedAt(), row.PublishedAt)
	wantField(t, label, "ctime", info.GetCtime(), row.Ctime)
	wantField(t, label, "mtime", info.GetMtime(), row.Mtime)
	// 信封外的两列不进响应（last_event_id/source 只落库），只能直接钉库里值：
	// 它们是投影幂等与「谁改的投影」的唯一证据，响应里没有不等于库里可以没有。
	wantField(t, label, "last_event_id(仅库内)", info.GetAid(), row.Aid)
}

func replayIDs(rows []*model.LiveReplayTask) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ReplayId)
	}
	return ids
}

// onlyReplay 库内恰好一行的回放任务：多一行就是重复登记，少一行就是被删了。
func onlyReplay(t *testing.T, db *store, label string) *model.LiveReplayTask {
	t.Helper()
	if len(db.replays) != 1 {
		ids := make([]int64, 0, len(db.replays))
		for id := range db.replays {
			ids = append(ids, id)
		}
		t.Fatalf("%s：库内应恰好 1 行回放任务，实际 %d 行 %v", label, len(db.replays), ids)
	}
	for _, row := range db.replays {
		return row
	}
	return nil
}

// countRefs 库内引用行数（绑定动作的「不多不少一行」判据）。
func countRefs(db *store) int { return len(db.replayRefs) }

// ---------------------------------------------------------------- SubmitReplayTask

func TestSubmitReplayTaskRegistersPendingRowFromStoppedRecord(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := stoppedRecord(db, nil)
	seedVerifiedRun(db, task.RecordId, 5, nil)

	in := submitReplayReq(task.RecordId, "  req-submit-1  ") // 首尾空白：校验与落库必须用同一个归一值
	in.FromSeq, in.ToSeq = 2, 4
	in.StartAt, in.EndAt = 1000010, 1000040
	in.Description = "完整回放简介"
	info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(in)
	info = wantOK(t, info, err, "提交回放")

	row := onlyReplay(t, db, "提交回放")
	wantReplayEchoesRow(t, "提交回放", info, row)
	wantField(t, "提交回放", "state 恒为 PENDING（本方法无拼接能力）", row.State, model.ReplayStatePending)
	wantField(t, "提交回放", "version", row.Version, int64(1))
	wantField(t, "提交回放", "request_id 已归一", row.RequestId, "req-submit-1")
	wantField(t, "提交回放", "room_id 取入参", row.RoomId, testRoomID)
	wantField(t, "提交回放", "anchor_mid 落稿件归属人", row.AnchorMid, testAnchor)
	wantField(t, "提交回放", "title", row.Title, "昨晚直播回放")
	wantField(t, "提交回放", "description", row.Description, "完整回放简介")
	// 区间是入参原值：给了就用给的，不能被水位改写。
	wantField(t, "提交回放", "from_seq", row.FromSeq, int64(2))
	wantField(t, "提交回放", "to_seq", row.ToSeq, int64(4))
	// segment_count 落「可用素材数」，gap_count 落缺口数：这两个数是拼接前的素材账，
	// 与 Worker 之后上报的实际拼接数不是一回事（上报侧只增不减地覆盖它）。
	wantField(t, "提交回放", "segment_count = 区间内 VERIFIED 数", row.SegmentCount, int64(3))
	wantField(t, "提交回放", "gap_count", row.GapCount, int64(0))
	// duration_ms 必须是 0：产物时长只有 Worker 知道，把素材时长当产物时长会锁死后续上报。
	wantField(t, "提交回放", "duration_ms 留空", row.DurationMs, int64(0))
	wantField(t, "提交回放", "allow_gaps 落 tinyint 0", row.AllowGaps, int32(0))
	// 本方法一行 INSERT 都不带产物引用：拼接还没开始。
	wantField(t, "提交回放", "output_bucket 留空", row.OutputBucket, "")
	wantField(t, "提交回放", "asset_id 未登记", row.AssetId, int64(0))
	wantField(t, "提交回放", "aid 未建稿", row.Aid, int64(0))
	wantField(t, "提交回放", "ctime 已补", row.Ctime > 0, true)

	// 缺口判定读的必须是切片表真值，且区间与入参一致（StatsInRange 的参数只在库里可见）。
	wantCalls(t, db, "Segments.LastSeq", 0, 1, "水位取自切片表")
	wantField(t, "提交回放", "StatsInRange 区间=入参区间", db.lastSegmentStats.toSeq, int64(4))
	wantField(t, "提交回放", "StatsInRange 录制主键", db.lastSegmentStats.recordID, task.RecordId)
	// 幂等三层里前两层必须真的查过（不查就会重复登记）。
	wantCalls(t, db, "ReplayTasks.FindByRequestID", 0, 1, "先查幂等键")
	wantCalls(t, db, "ReplayTasks.FindByRecordAndRange", 0, 1, "再查同区间复用")
	wantCalls(t, db, "ReplayTasks.Insert", 0, 1, "只有一次 INSERT")
	// 无事件、无事务：词表里没有「回放已登记」，且这里只有一条 INSERT。
	wantEvents(t, db, nil, "提交回放")
	wantCalls(t, db, "DB.TransactCtx", 0, 0, "单条 INSERT 不该开事务")
	wantNoLeak(t, db, "提交回放")
}

func TestSubmitReplayTaskFallsBackToFullRecordedRange(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	svcCtx.Config.LiveMedia.MaxReplayGapSegments = 2
	task := stoppedRecord(db, nil)
	seedVerifiedRun(db, task.RecordId, 5, nil)
	// 水位上多一行 MISSING：MAX(seq) 含缺口行，因此默认区间会到 6，而 6 不是 VERIFIED。
	seedHole(db, task.RecordId, 6, model.SegmentStateMissing)

	in := submitReplayReq(task.RecordId, "req-range-default")
	in.AllowGaps = true // 尾部这片 MISSING 使默认区间带 1 个洞，不放行就只能被拒（见下）
	info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(in)
	info = wantOK(t, info, err, "区间回落到全片")

	row := mustReplay(t, db, info.GetReplayId())
	wantReplayEchoesRow(t, "区间回落到全片", info, row)
	wantField(t, "区间回落到全片", "from_seq 默认 1", row.FromSeq, int64(1))
	// to_seq 取 MAX(seq)=6（含缺口行），不是 VERIFIED 的最大序号 5：
	// 素材账必须覆盖到录制结论的最后一片，否则缺口会被静默藏掉。
	wantField(t, "区间回落到全片", "to_seq = MAX(seq)（含 MISSING）", row.ToSeq, int64(6))
	wantField(t, "区间回落到全片", "segment_count 只数 VERIFIED", row.SegmentCount, int64(5))
	wantField(t, "区间回落到全片", "gap_count 含 MISSING", row.GapCount, int64(1))
	// start_at/end_at 未提供时回落录制任务的记录区间（不是切片时间）。
	wantField(t, "区间回落到全片", "start_at 取 record_start_at", row.StartAt, task.RecordStartAt)
	wantField(t, "区间回落到全片", "end_at 取 record_end_at", row.EndAt, task.RecordEndAt)

	// 同一片素材、同样的默认区间，allow_gaps=false 时必须拒绝：
	// 「默认取满水位」不等于「默认允许洞」，宁可拒绝提交也不产出时间轴断裂的回放。
	noGap := submitReplayReq(task.RecordId, "req-range-default-nogap")
	noGap.FromSeq, noGap.ToSeq = 1, 6 // 换区间绕开同区间复用，让拒绝真的来自缺口门禁
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(noGap)
	wantFail(t, info, err, model.ErrReplayGapNotAllowed, "默认区间带洞且 allow_gaps=false")
	wantField(t, "默认区间带洞且 allow_gaps=false", "第二次提交没有建行", len(db.replays), 1)
	wantEvents(t, db, nil, "区间回落")
}

func TestSubmitReplayTaskRejectsRecordThatIsNotStopped(t *testing.T) {
	// 只有 STOPPED 允许拼接。FAILED 尤其危险：它可断点续录（recordTransitions 里
	// FAILED→PENDING/RECORDING 都合法），此刻尾部切片还可能在补录。
	cases := []struct {
		state int32
		name  string
	}{
		{model.RecordStatePending, "PENDING"},
		{model.RecordStateRecording, "RECORDING"},
		{model.RecordStateStopping, "STOPPING"},
		{model.RecordStateFailed, "FAILED（可续录，但尾部未定稿）"},
		{model.RecordStateCancelled, "CANCELLED"},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := stoppedRecord(db, nil)
		task.State = tc.state
		db.records[task.RecordId] = task
		seedVerifiedRun(db, task.RecordId, 5, nil)
		before := snapshotWrites(db)

		info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).
			SubmitReplayTask(submitReplayReq(task.RecordId, "req-not-stopped"))
		wantFail(t, info, err, model.ErrInvalidTransition, tc.name+" 不得建回放")
		wantNoWrites(t, db, before, tc.name)
		wantField(t, tc.name, "没有产出回放行", len(db.replays), 0)
		wantEvents(t, db, nil, tc.name)
	}
}

func TestSubmitReplayTaskRejectsRecordOfAnotherRoomOrMissing(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	foreign := stoppedRecord(db, func(r *model.LiveRecordTask) { r.RoomId, r.LiveSession = testRoomOther, testSession2 })
	seedVerifiedRun(db, foreign.RecordId, 5, nil)
	before := snapshotWrites(db)

	// 拿别人房间的录制主键提交：越权比拒绝严重得多，必须失败关闭。
	info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).
		SubmitReplayTask(submitReplayReq(foreign.RecordId, "req-foreign-room"))
	wantFail(t, info, err, model.ErrInvalidRoomID, "录制不属于本房间")
	wantNoWrites(t, db, before, "录制不属于本房间")

	// 场次对不上同样拒绝（README §8.9：场次 ID 没透传时会退化成 0，这里必须能区分）。
	wrongSession := submitReplayReq(foreign.RecordId, "req-foreign-session")
	wrongSession.RoomId, wrongSession.LiveSessionId = testRoomID, testSession
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(wrongSession)
	wantFail(t, info, err, model.ErrInvalidRoomID, "房间对但场次不对")
	// 录制主键不存在：回 ErrRecordTaskNotFound 而不是「拼一个空回放」。
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).
		SubmitReplayTask(submitReplayReq(999999, "req-no-record"))
	wantFail(t, info, err, model.ErrRecordTaskNotFound, "录制任务不存在")
	wantNoWrites(t, db, before, "录制不存在")
	wantField(t, "回放门禁", "未产出任何行", len(db.replays), 0)
	wantNoLeak(t, db, "回放门禁")
}

func TestSubmitReplayTaskGapDecision(t *testing.T) {
	// 缺口判据来自 StatsInRange：隐式空洞（区间长度 - 已登记行数）+ MISSING + CORRUPT。
	// allow_gaps 只是「要不要放行」，真正的量闸门是配置 MaxReplayGapSegments（默认 0）。
	cases := []struct {
		name      string
		allowGaps bool
		cfgMax    int32
		wantErr   error
		wantGaps  int64
	}{
		{name: "有洞且 allow_gaps=false", wantErr: model.ErrReplayGapNotAllowed},
		{name: "有洞 allow_gaps=true 但配置上限 0", allowGaps: true, wantErr: model.ErrReplayGapNotAllowed},
		{name: "有洞 allow_gaps=true 且配置允许 1 个", allowGaps: true, cfgMax: 1, wantGaps: 1},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		svcCtx.Config.LiveMedia.MaxReplayGapSegments = tc.cfgMax
		task := stoppedRecord(db, nil)
		// seq 3 整行不存在（隐式空洞）：1,2,4,5 四行覆盖 [1,5]。
		for _, seq := range []int64{1, 2, 4, 5} {
			seedSegment(db, task.RecordId, seq, model.SegmentStateVerified, nil)
		}
		before := snapshotWrites(db)

		in := submitReplayReq(task.RecordId, "req-gap-"+tc.name)
		in.AllowGaps = tc.allowGaps
		info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(in)
		if tc.wantErr != nil {
			wantFail(t, info, err, tc.wantErr, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantField(t, tc.name, "被拒时不产出行", len(db.replays), 0)
			wantEvents(t, db, nil, tc.name)
			continue
		}
		info = wantOK(t, info, err, tc.name)
		row := mustReplay(t, db, info.GetReplayId())
		wantReplayEchoesRow(t, tc.name, info, row)
		wantField(t, tc.name, "gap_count 如实落库", row.GapCount, tc.wantGaps)
		wantField(t, tc.name, "segment_count 只数 VERIFIED", row.SegmentCount, int64(4))
		wantField(t, tc.name, "allow_gaps 落 tinyint 1", row.AllowGaps, int32(1))
		wantField(t, tc.name, "响应 allow_gaps 与列值一致", info.GetAllowGaps(), true)
		wantEvents(t, db, nil, tc.name)
		wantNoLeak(t, db, tc.name)
	}
}

func TestSubmitReplayTaskRejectsRangeWithoutVerifiedMaterial(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := stoppedRecord(db, nil)
	before := snapshotWrites(db)

	// 一片都没登记：水位为 0，normalizeReplayRange 先拒绝。
	info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).
		SubmitReplayTask(submitReplayReq(task.RecordId, "req-no-material"))
	wantFail(t, info, err, model.ErrSegmentRangeNotRecorded, "录制尚无切片")

	// 全 MISSING/CORRUPT：allow_gaps 也救不了（拼接产物会是纯空洞）。
	seedHole(db, task.RecordId, 1, model.SegmentStateMissing)
	seedHole(db, task.RecordId, 2, model.SegmentStateCorrupt)
	allow := submitReplayReq(task.RecordId, "req-only-holes")
	allow.AllowGaps = true
	svcCtx.Config.LiveMedia.MaxReplayGapSegments = 10
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(allow)
	wantFail(t, info, err, model.ErrSegmentRangeNotRecorded, "区间内无 VERIFIED")

	// 越界：to_seq 超过水位（尾部还没落库，拼出来必然缺尾巴）。
	seedSegment(db, task.RecordId, 3, model.SegmentStateVerified, nil)
	over := submitReplayReq(task.RecordId, "req-over-range")
	over.ToSeq = 9
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(over)
	wantFail(t, info, err, model.ErrInvalidSegmentRange, "to_seq 越界")

	// from>to 与时间轴自相矛盾（start_at 早于录制开始 / end_at 晚于录制结束）。
	rev := submitReplayReq(task.RecordId, "req-reversed")
	rev.FromSeq, rev.ToSeq = 3, 1
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(rev)
	wantFail(t, info, err, model.ErrInvalidSegmentRange, "from_seq>to_seq")

	late := submitReplayReq(task.RecordId, "req-late-end")
	late.StartAt, late.EndAt = 1000000, 1999999
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(late)
	wantFail(t, info, err, model.ErrInvalidSegmentRange, "end_at 超出录制结束")

	early := submitReplayReq(task.RecordId, "req-early-start")
	early.StartAt, early.EndAt = 900000, 1000040
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(early)
	wantFail(t, info, err, model.ErrInvalidSegmentRange, "start_at 早于录制开始")

	same := submitReplayReq(task.RecordId, "req-equal-time")
	same.StartAt, same.EndAt = 1000020, 1000020
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(same)
	wantFail(t, info, err, model.ErrInvalidSegmentRange, "end_at<=start_at")

	wantNoWrites(t, db, before, "回放区间门禁")
	wantField(t, "回放区间门禁", "六次拒绝都没建行", len(db.replays), 0)
	wantEvents(t, db, nil, "回放区间门禁")
}

func TestSubmitReplayTaskRejectsBadArgumentsBeforeAnyRead(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)
	cases := []struct {
		name    string
		mutate  func(in *rpc.SubmitReplayTaskReq)
		wantErr error
	}{
		{"room_id<=0", func(in *rpc.SubmitReplayTaskReq) { in.RoomId = 0 }, model.ErrInvalidRoomID},
		{"live_session_id<=0", func(in *rpc.SubmitReplayTaskReq) { in.LiveSessionId = -1 }, model.ErrInvalidSessionID},
		{"record_id<=0", func(in *rpc.SubmitReplayTaskReq) { in.RecordId = 0 }, model.ErrRecordTaskNotFound},
		{"anchor_mid<=0", func(in *rpc.SubmitReplayTaskReq) { in.AnchorMid = 0 }, model.ErrInvalidAid},
		{"request_id 为空", func(in *rpc.SubmitReplayTaskReq) { in.RequestId = "   " }, model.ErrEmptyRequestID},
		{"request_id 超列宽", func(in *rpc.SubmitReplayTaskReq) {
			in.RequestId = strings.Repeat("r", maxRequestIDRunes+1)
		}, model.ErrEmptyRequestID},
		{"start_at 为负", func(in *rpc.SubmitReplayTaskReq) { in.StartAt = -1 }, model.ErrInvalidSegmentRange},
	}
	for _, tc := range cases {
		in := submitReplayReq(5001, "req-arg")
		tc.mutate(in)
		info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(in)
		wantFail(t, info, err, tc.wantErr, "SubmitReplayTask "+tc.name)
	}
	wantNoWrites(t, db, before, "入参门禁")
	// 入参非法时连「读录制任务」都不该发生：门禁顺序本身就是契约。
	wantCalls(t, db, "RecordTasks.FindOne", 0, 0, "入参门禁之后不得查库")
}

func TestSubmitReplayTaskTitleRuneCapAndDescriptionClamp(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := stoppedRecord(db, nil)
	seedVerifiedRun(db, task.RecordId, 5, nil)

	// 标题 81 rune：拒绝，不截断（截出来的标题与投稿标题就是两份事实）。
	long := submitReplayReq(task.RecordId, "req-title-long")
	long.Title = strings.Repeat("长", 81)
	info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(long)
	wantFail(t, info, err, model.ErrTitleTooLong, "标题 81 rune")
	wantField(t, "标题 81 rune", "被拒时没有建行", len(db.replays), 0)

	// 边界值 80 rune 必须通过：上限判断用 rune 而不是字节（中文 3 字节/rune）。
	edge := submitReplayReq(task.RecordId, "req-title-edge")
	edge.Title = strings.Repeat("长", 80)
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(edge)
	info = wantOK(t, info, err, "标题 80 rune")
	wantField(t, "标题 80 rune", "按 rune 计数", runeLen(mustReplay(t, db, info.GetReplayId()).Title), 80)

	// 简介 2100 rune：截断到 2048 并放行（纯展示列，拒绝整次提交才是错的）。
	// 三次成功提交各用不同区间：同区间的第二次会被「未终态复用」挡掉，
	// 那样断言的其实是复用分支而不是文本归一（这条复用口径由下面的用例单独钉）。
	desc := submitReplayReq(task.RecordId, "req-desc-long")
	desc.Description = strings.Repeat("简", 2100)
	desc.FromSeq, desc.ToSeq = 2, 5
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(desc)
	info = wantOK(t, info, err, "简介超长截断")
	row := mustReplay(t, db, info.GetReplayId())
	wantField(t, "简介超长截断", "截到 maxDescRunes", runeLen(row.Description), maxDescRunes)
	wantField(t, "简介超长截断", "response 不含简介（Info 无该列）", info.GetTitle(), row.Title)

	// 标题里带明文凭据：脱敏后落库，凭据原文绝不进文本列。
	// 注意脱敏只替换值、不改长度，所以带凭据的标题同样受 80 rune 上限约束。
	leaky := submitReplayReq(task.RecordId, "req-title-secret")
	leaky.Title = "回放 token=" + leakMarker
	leaky.FromSeq, leaky.ToSeq = 3, 5
	info, err = NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(leaky)
	info = wantOK(t, info, err, "标题含凭据")
	row = mustReplay(t, db, info.GetReplayId())
	if strings.Contains(row.Title, leakMarker) {
		t.Fatalf("标题把凭据原文落库了：%s", row.Title)
	}
	wantField(t, "标题含凭据", "只脱敏不拒绝（标题仍在）", strings.HasPrefix(row.Title, "回放 token="), true)

	wantField(t, "标题与简介门禁", "只落了三次成功的行", len(db.replays), 3)
	wantEvents(t, db, nil, "标题与简介门禁")
	wantNoLeak(t, db, "标题与简介门禁")
}

func TestSubmitReplayTaskSameRequestIdReplaysExistingRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := stoppedRecord(db, nil)
	seedVerifiedRun(db, task.RecordId, 5, nil)
	// 既有行：MERGING 中、区间与本次提交完全不同 —— 幂等回放必须原样返回它。
	existed := seedReplay(db, model.ReplayStateMerging, func(r *model.LiveReplayTask) {
		r.RequestId = "req-idem"
		r.RecordId, r.FromSeq, r.ToSeq = task.RecordId, 9, 9
		r.SegmentCount, r.GapCount = 1, 0
	})
	snapshot := *existed
	before := snapshotWrites(db)

	in := submitReplayReq(task.RecordId, " req-idem ")
	in.FromSeq, in.ToSeq = 1, 5
	info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).SubmitReplayTask(in)
	info = wantOK(t, info, err, "同 request_id 重放")

	// 返回的是既有行：replay_id 与全部列值都取自它，而不是本次入参。
	wantReplayEchoesRow(t, "同 request_id 重放", info, mustReplay(t, db, existed.ReplayId))
	wantField(t, "同 request_id 重放", "replay_id", info.GetReplayId(), existed.ReplayId)
	wantField(t, "同 request_id 重放", "to_seq 仍是既有值", info.GetToSeq(), int64(9))
	assertReplayUnchanged(t, db, existed.ReplayId, snapshot, "同 request_id 重放")
	wantField(t, "同 request_id 重放", "没有第二行", len(db.replays), 1)
	wantCalls(t, db, "ReplayTasks.Insert", 0, 0, "重放不得再 INSERT")
	wantNoWrites(t, db, before, "同 request_id 重放")
	wantEvents(t, db, nil, "同 request_id 重放")
}

func TestSubmitReplayTaskReusesUnfinishedTaskOnSameRange(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := stoppedRecord(db, nil)
	seedVerifiedRun(db, task.RecordId, 5, nil)
	running := seedReplay(db, model.ReplayStateUploading, func(r *model.LiveReplayTask) {
		r.RequestId, r.RecordId, r.FromSeq, r.ToSeq = "req-other", task.RecordId, 1, 5
	})
	snapshot := *running

	info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).
		SubmitReplayTask(submitReplayReq(task.RecordId, "req-new-key-same-range"))
	info = wantOK(t, info, err, "同区间未终态复用")
	wantReplayEchoesRow(t, "同区间未终态复用", info, mustReplay(t, db, running.ReplayId))
	wantField(t, "同区间未终态复用", "复用既有 replay_id", info.GetReplayId(), running.ReplayId)
	wantField(t, "同区间未终态复用", "新 request_id 未落库（复用行保留原键）",
		mustReplay(t, db, running.ReplayId).RequestId, "req-other")
	assertReplayUnchanged(t, db, running.ReplayId, snapshot, "同区间未终态复用")
	wantField(t, "同区间未终态复用", "没有产出第二份稿件的前身", len(db.replays), 1)
	wantCalls(t, db, "ReplayTasks.Insert", 0, 0, "复用不 INSERT")
	wantEvents(t, db, nil, "同区间未终态复用")
}

func TestSubmitReplayTaskFailedRangeAllowsNewTask(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := stoppedRecord(db, nil)
	seedVerifiedRun(db, task.RecordId, 5, nil)
	failed := seedReplay(db, model.ReplayStateFailed, func(r *model.LiveReplayTask) {
		r.RequestId, r.RecordId, r.FromSeq, r.ToSeq = "req-failed-run", task.RecordId, 1, 5
		r.Reason, r.ErrMsg = model.ReasonStorage, "上传失败"
	})
	failedSnapshot := *failed

	info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).
		SubmitReplayTask(submitReplayReq(task.RecordId, "req-retry-range"))
	info = wantOK(t, info, err, "失败区间允许重投")

	row := mustReplay(t, db, info.GetReplayId())
	wantReplayEchoesRow(t, "失败区间允许重投", info, row)
	wantField(t, "失败区间允许重投", "是新的 PENDING 行", row.State, model.ReplayStatePending)
	if row.ReplayId == failed.ReplayId {
		t.Fatalf("重投复用了失败行（会把失败归因与 version 继承过来）：%+v", row)
	}
	assertReplayUnchanged(t, db, failed.ReplayId, failedSnapshot, "失败区间允许重投")
	wantField(t, "失败区间允许重投", "两行并存", len(db.replays), 2)
	wantEvents(t, db, nil, "失败区间允许重投")
	wantNoLeak(t, db, "失败区间允许重投")
}

func TestSubmitReplayTaskConcurrentInsertLosesRequestIdRace(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := stoppedRecord(db, nil)
	seedVerifiedRun(db, task.RecordId, 5, nil)

	// 时序：本方法预读 request_id 时对手还没提交（missOnce 复刻），
	// 到 INSERT 那一刻对手的行刚落库 → 撞 uniq_request_id。
	db.missOnce("ReplayTasks.FindByRequestID")
	var competitor *model.LiveReplayTask
	db.onHit("ReplayTasks.Insert", func() {
		competitor = seedReplay(db, model.ReplayStateMerging, func(r *model.LiveReplayTask) {
			r.RequestId, r.RecordId, r.FromSeq, r.ToSeq = "req-race", task.RecordId, 1, 5
		})
	})

	info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).
		SubmitReplayTask(submitReplayReq(task.RecordId, "req-race"))
	info = wantOK(t, info, err, "并发撞 request_id 必须回读既有行")

	// 撞键不是失败：回读对手的行列值返回，且库里恰好一行。
	wantField(t, "并发撞 request_id", "replay_id 是对手的", info.GetReplayId(), competitor.ReplayId)
	wantField(t, "并发撞 request_id", "state 取库内真值", int32(info.GetState()), competitor.State)
	wantField(t, "并发撞 request_id", "库内只有一行", len(db.replays), 1)
	wantCalls(t, db, "ReplayTasks.Insert", 0, 1, "只发过一次 INSERT")
	wantField(t, "并发撞 request_id", "第二次读幂等键用于回读", db.count("ReplayTasks.FindByRequestID"), 2)
	wantEvents(t, db, nil, "并发撞 request_id")
	wantNoLeak(t, db, "并发撞 request_id")
}

func TestSubmitReplayTaskFailsClosedOnModelErrors(t *testing.T) {
	// 依赖故障一律「不知道就当失败」：读不到录制/水位/统计时不得凭空登记任务。
	ops := []string{
		"RecordTasks.FindOne", "Segments.LastSeq", "Segments.StatsInRange",
		"ReplayTasks.FindByRequestID", "ReplayTasks.FindByRecordAndRange", "ReplayTasks.Insert",
	}
	for _, op := range ops {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := stoppedRecord(db, nil)
		seedVerifiedRun(db, task.RecordId, 5, nil)
		db.failOn(op, errModelDown)

		info, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).
			SubmitReplayTask(submitReplayReq(task.RecordId, "req-fail-"+op))
		if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
			t.Fatalf("%s 故障时应原样上抛（不得吞错或当成功）：%v", op, err)
		}
		if !isNilPtr(info) {
			t.Fatalf("%s 故障时不得带回响应体：%+v", op, info)
		}
		// 只有真发生过的写才算写：这里断言的是「没有新行」，比 wantNoWrites 更直接。
		wantField(t, op+" 故障", "未产出回放行", len(db.replays), 0)
		wantEvents(t, db, nil, op+" 故障")
		wantNoLeak(t, db, op+" 故障")
	}
}

// ---------------------------------------------------------------- ReportReplayProgress

// 三个状态永不上报：PENDING 属提交侧（幂等键在那边）、CANCELLED 属调用方、
// COMPLETED 属 video 事实投影。拒绝必须是失败关闭而不是静默忽略，
// 且判定发生在读库之前 —— 所以这里连「读过一次」都不允许发生。
func TestReportReplayProgressRejectsStatesThatAreNotTheWorkersToSet(t *testing.T) {
	cases := []struct {
		name  string
		state rpc.ReplayState
	}{
		{"PENDING 由 SubmitReplayTask 建立", rpc.ReplayState_REPLAY_STATE_PENDING},
		{"CANCELLED 是调用方/运营的动作，Worker 不得代为取消", rpc.ReplayState_REPLAY_STATE_CANCELLED},
		{"COMPLETED 只能由 ApplyReplayContentState 驱动", rpc.ReplayState_REPLAY_STATE_COMPLETED},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		// 起跑线把三条前置门禁全满足（产物引用 + asset/aid），
		// 否则用例可能只是碰巧撞在别的门禁上，测不到「这条状态永不上报」。
		row := boundReplay(db, nil)
		was := *row
		before := snapshotWrites(db)

		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).
			ReportReplayProgress(reportReplayReq(row.ReplayId, tc.state))
		wantFail(t, info, err, model.ErrInvalidTransition, tc.name)
		wantNoWrites(t, db, before, tc.name)
		wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, tc.name+"：门禁在读库之前")
		assertReplayUnchanged(t, db, row.ReplayId, was, tc.name)
		wantEvents(t, db, nil, tc.name)
	}
}

// 合法前进边：逐格推进状态机，并钉住「成功边不带失败信息」「中间态零事件」。
func TestReportReplayProgressAdvancesAlongStateMachineWithoutEvents(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedReplay(db, model.ReplayStatePending, nil)
	from := row.ReplayId

	// PENDING→MERGING：携带新计数，同时故意带 errno/err_msg。
	in := reportReplayReq(from, rpc.ReplayState_REPLAY_STATE_MERGING)
	in.SegmentCount, in.GapCount = 3, 1
	in.ExpectedVersion = row.Version
	in.Errno, in.ErrMsg = 42, "上一次失败的残留摘要"
	info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
	info = wantOK(t, info, err, "PENDING→MERGING")
	cur := mustReplay(t, db, from)
	wantReplayEchoesRow(t, "PENDING→MERGING", info, cur)
	wantField(t, "PENDING→MERGING", "state", cur.State, model.ReplayStateMerging)
	wantField(t, "PENDING→MERGING", "version 前进一格", cur.Version, int64(2))
	wantField(t, "PENDING→MERGING", "segment_count 取本次上报", cur.SegmentCount, int64(3))
	wantField(t, "PENDING→MERGING", "gap_count 取本次上报", cur.GapCount, int64(1))
	wantField(t, "PENDING→MERGING", "trace_id 随回执改写", cur.TraceId, "trace-replay-report")
	// 前进边不带失败信息：errno/err_msg 不在 SET 列表里，残留的 42 也不该被这次上报带进来。
	wantField(t, "PENDING→MERGING", "成功边不写 errno", cur.Errno, int32(0))
	wantField(t, "PENDING→MERGING", "成功边不写 err_msg", cur.ErrMsg, "")
	wantField(t, "PENDING→MERGING", "成功边不写 reason", cur.Reason, int32(0))
	wantCalls(t, db, "DB.TransactCtx", 0, 1, "状态推进在事务里")
	wantEvents(t, db, nil, "PENDING→MERGING 不发事件")

	// MERGING→UPLOADING：什么都不带（心跳型回执），产物账不能被零值抹掉。
	before := *cur
	info, err = NewReportReplayProgressLogic(context.Background(), svcCtx).
		ReportReplayProgress(reportReplayReq(from, rpc.ReplayState_REPLAY_STATE_UPLOADING))
	info = wantOK(t, info, err, "MERGING→UPLOADING")
	cur = mustReplay(t, db, from)
	wantReplayEchoesRow(t, "MERGING→UPLOADING", info, cur)
	wantField(t, "MERGING→UPLOADING", "state", cur.State, model.ReplayStateUploading)
	wantField(t, "MERGING→UPLOADING", "version", cur.Version, int64(3))
	wantField(t, "MERGING→UPLOADING", "未提供的 segment_count 沿用行内值", cur.SegmentCount, before.SegmentCount)
	wantField(t, "MERGING→UPLOADING", "未提供的 gap_count 沿用行内值", cur.GapCount, before.GapCount)
	wantEvents(t, db, nil, "MERGING→UPLOADING 不发事件")

	// UPLOADING→REGISTERED：产物引用必须在这条边上给全（拼接+上传已完成）。
	up := reportReplayReq(from, rpc.ReplayState_REPLAY_STATE_REGISTERED)
	up.OutputBucket, up.OutputKey = replayProductBucket, replayProductKey
	up.DurationMs = 60000
	info, err = NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(up)
	info = wantOK(t, info, err, "UPLOADING→REGISTERED")
	cur = mustReplay(t, db, from)
	wantReplayEchoesRow(t, "UPLOADING→REGISTERED", info, cur)
	wantField(t, "UPLOADING→REGISTERED", "state", cur.State, model.ReplayStateRegistered)
	wantField(t, "UPLOADING→REGISTERED", "output_bucket", cur.OutputBucket, replayProductBucket)
	wantField(t, "UPLOADING→REGISTERED", "output_key 是相对 key", cur.OutputKey, replayProductKey)
	wantField(t, "UPLOADING→REGISTERED", "duration_ms", cur.DurationMs, int64(60000))
	wantField(t, "UPLOADING→REGISTERED", "version", cur.Version, int64(4))
	// asset_id/aid 不由上报路径回填：这里仍是 0，回填归 BindReplayAsset。
	wantField(t, "UPLOADING→REGISTERED", "asset_id 未被上报伪造", cur.AssetId, int64(0))
	wantField(t, "UPLOADING→REGISTERED", "aid 未被上报伪造", cur.Aid, int64(0))

	// REGISTERED→FAILED：失败归因只有在这条边上才写。
	fail := reportReplayReq(from, rpc.ReplayState_REPLAY_STATE_FAILED)
	fail.Reason = rpc.FailureReason_FAILURE_REASON_DATA_GAP
	fail.Errno = 74
	fail.ErrMsg = "拼接缺 3 片 token=" + leakMarker
	info, err = NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(fail)
	info = wantOK(t, info, err, "REGISTERED→FAILED")
	cur = mustReplay(t, db, from)
	wantReplayEchoesRow(t, "REGISTERED→FAILED", info, cur)
	wantField(t, "REGISTERED→FAILED", "state", cur.State, model.ReplayStateFailed)
	wantField(t, "REGISTERED→FAILED", "reason", cur.Reason, int32(rpc.FailureReason_FAILURE_REASON_DATA_GAP))
	wantField(t, "REGISTERED→FAILED", "errno", cur.Errno, int32(74))
	if !strings.HasPrefix(cur.ErrMsg, "拼接缺 3 片 token=") || strings.Contains(cur.ErrMsg, leakMarker) {
		t.Fatalf("REGISTERED→FAILED：err_msg 应保留排障上下文并掩掉凭据，实际 %q", cur.ErrMsg)
	}
	wantField(t, "REGISTERED→FAILED", "version", cur.Version, int64(5))
	// 全程零事件：model 词表里没有通用 replay_state_changed / 失败事件，
	// 本服务也不为此伪造一个类型（README §8.4），中间态靠读侧观测。
	wantEvents(t, db, nil, "前进链全程")
	wantNoLeak(t, db, "前进链全程")
}

// 跳格、回退、以及「在终态行上继续上报」三类非法边。
func TestReportReplayProgressRejectsSkippedBackwardAndTerminalEdges(t *testing.T) {
	cases := []struct {
		name   string
		from   int32
		target rpc.ReplayState
		want   error
		// needRef/needAsset 用来满足目标态的前置门禁，确保失败来自状态机而不是门禁。
		needRef bool
	}{
		{"跳过 MERGING/UPLOADING 直达 REGISTERED", model.ReplayStatePending,
			rpc.ReplayState_REPLAY_STATE_REGISTERED, model.ErrInvalidTransition, true},
		{"跳过三格直达 REVIEW_SUBMITTED", model.ReplayStatePending,
			rpc.ReplayState_REPLAY_STATE_REVIEW_SUBMITTED, model.ErrInvalidTransition, false},
		{"回退 UPLOADING→MERGING", model.ReplayStateUploading,
			rpc.ReplayState_REPLAY_STATE_MERGING, model.ErrInvalidTransition, false},
		{"已送审后回退到 UPLOADING", model.ReplayStateReviewSubmitted,
			rpc.ReplayState_REPLAY_STATE_UPLOADING, model.ErrInvalidTransition, false},
		{"FAILED 后重跑 MERGING（失败是终态，重投走新任务）", model.ReplayStateFailed,
			rpc.ReplayState_REPLAY_STATE_MERGING, model.ErrTerminalState, false},
		{"CANCELLED 后继续上报", model.ReplayStateCancelled,
			rpc.ReplayState_REPLAY_STATE_MERGING, model.ErrTerminalState, false},
		{"投影已驱动 COMPLETED 后 Worker 迟到回执", model.ReplayStateCompleted,
			rpc.ReplayState_REPLAY_STATE_REVIEW_SUBMITTED, model.ErrTerminalState, false},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		row := boundReplay(db, func(r *model.LiveReplayTask) { r.State = tc.from })
		was := *row
		before := snapshotWrites(db)

		in := reportReplayReq(row.ReplayId, tc.target)
		if tc.needRef {
			in.OutputBucket, in.OutputKey = replayProductBucket, replayProductKey
		}
		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
		wantFail(t, info, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
		assertReplayUnchanged(t, db, row.ReplayId, was, tc.name)
		wantEvents(t, db, nil, tc.name)
	}

	// COMPLETED 行上报 FAILED：终态之间也不许互相改写（尤其不许把已发布的回放改成失败）。
	db := newStore()
	svcCtx := newTestSvc(db)
	row := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateCompleted })
	was := *row
	before := snapshotWrites(db)
	late := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_FAILED)
	late.Reason = rpc.FailureReason_FAILURE_REASON_SOURCE_LOST
	info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(late)
	wantFail(t, info, err, model.ErrTerminalState, "COMPLETED 行上报 FAILED")
	wantNoWrites(t, db, before, "COMPLETED 行上报 FAILED")
	assertReplayUnchanged(t, db, row.ReplayId, was, "COMPLETED 行上报 FAILED")
	wantEvents(t, db, nil, "COMPLETED 行上报 FAILED")
}

// FAILED 必须带具体归因，reason 也不许越界：否则终态行留下一条「不知道为什么失败」的记录。
func TestReportReplayProgressFailedEdgeNeedsConcreteReason(t *testing.T) {
	cases := []struct {
		name   string
		reason rpc.FailureReason
	}{
		{"不带 reason（UNSPECIFIED）", rpc.FailureReason_FAILURE_REASON_UNSPECIFIED},
		{"reason 超出词表", rpc.FailureReason(99)},
		{"reason 为负", rpc.FailureReason(-1)},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		row := seedReplay(db, model.ReplayStateMerging, nil)
		was := *row
		before := snapshotWrites(db)

		in := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_FAILED)
		in.Reason = tc.reason
		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
		wantFail(t, info, err, model.ErrInvalidTransition, tc.name)
		wantNoWrites(t, db, before, tc.name)
		wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, tc.name+"：reason 门禁在读库之前")
		assertReplayUnchanged(t, db, row.ReplayId, was, tc.name)
	}
}

// 同值重放 = 回执丢了重投：回当前行，不 ++version、不发事件、不改任何一列。
func TestReportReplayProgressIdenticalReceiptReplayChangesNothing(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedReplay(db, model.ReplayStateMerging, func(r *model.LiveReplayTask) {
		r.SegmentCount, r.GapCount, r.DurationMs = 3, 1, 45000
		r.OutputBucket, r.OutputKey = replayProductBucket, replayProductKey
	})
	was := *row
	before := snapshotWrites(db)

	cases := []struct {
		name string
		mut  func(*rpc.ReportReplayProgressReq)
	}{
		{"逐字段同值重放", func(in *rpc.ReportReplayProgressReq) {
			in.SegmentCount, in.GapCount, in.DurationMs = 3, 1, 45000
			in.OutputBucket, in.OutputKey = replayProductBucket, replayProductKey
			in.ExpectedVersion = was.Version
		}},
		{"零值心跳（什么都不提供）", func(in *rpc.ReportReplayProgressReq) {
			in.WorkerId = "worker-其他"
		}},
	}
	for _, tc := range cases {
		in := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_MERGING)
		tc.mut(in)
		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
		info = wantOK(t, info, err, tc.name)
		wantReplayEchoesRow(t, tc.name, info, &was)
		wantNoWrites(t, db, before, tc.name)
		assertReplayUnchanged(t, db, row.ReplayId, was, tc.name)
		wantEvents(t, db, nil, tc.name)
	}
	// 幂等键（expected_version）在这里只用于「调用方看过更新的行吗」，
	// 同值重放路径根本不写库，所以不带版本号也照样成功。
	wantField(t, "同值重放", "行内 version 未动", mustReplay(t, db, row.ReplayId).Version, int64(1))
}

// 同状态携带不同事实不是「刷新」：replayTransitions 里没有自环，
// 否则等于给 Worker 开一条「不推进状态也能改写产物事实」的后门。
func TestReportReplayProgressSameStateWithDifferentFactsIsNotASelfTransition(t *testing.T) {
	cases := []struct {
		name string
		from int32
		mut  func(*rpc.ReportReplayProgressReq)
	}{
		{"同状态改时长", model.ReplayStateMerging, func(in *rpc.ReportReplayProgressReq) {
			in.DurationMs = 50000 // 行内 45000
		}},
		{"同状态改素材数", model.ReplayStateMerging, func(in *rpc.ReportReplayProgressReq) {
			in.SegmentCount = 4
		}},
		{"同状态改产物引用", model.ReplayStateRegistered, func(in *rpc.ReportReplayProgressReq) {
			in.OutputBucket = "live-replay-backup"
			in.OutputKey = replayProductKey
		}},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		row := seedReplay(db, tc.from, func(r *model.LiveReplayTask) {
			r.SegmentCount, r.GapCount, r.DurationMs = 3, 1, 45000
			r.OutputBucket, r.OutputKey = replayProductBucket, replayProductKey
		})
		was := *row
		before := snapshotWrites(db)

		in := reportReplayReq(row.ReplayId, rpc.ReplayState(tc.from))
		tc.mut(in)
		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
		wantFail(t, info, err, model.ErrInvalidTransition, tc.name)
		wantNoWrites(t, db, before, tc.name)
		assertReplayUnchanged(t, db, row.ReplayId, was, tc.name)
		wantField(t, tc.name, "产物引用仍是原值", mustReplay(t, db, row.ReplayId).OutputBucket, replayProductBucket)
		wantEvents(t, db, nil, tc.name)
	}
}

// 终态重投：同一个归因回当前行；换一个失败原因必须被拒——
// 已记录的归因可能已被运营用于统计，覆盖它等于篡改结论。
func TestReportReplayProgressLateFailedReportKeepsRecordedAttribution(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedReplay(db, model.ReplayStateFailed, func(r *model.LiveReplayTask) {
		r.Reason = int32(rpc.FailureReason_FAILURE_REASON_TIMEOUT)
		r.Errno, r.ErrMsg = 11, "merge timeout after 30min"
		r.SegmentCount, r.GapCount, r.DurationMs = 3, 1, 45000
		r.Version = 5
	})
	was := *row
	before := snapshotWrites(db)

	// 同归因 + 同事实：回当前行，version 停在 5。
	same := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_FAILED)
	same.Reason = rpc.FailureReason_FAILURE_REASON_TIMEOUT
	same.SegmentCount, same.GapCount, same.DurationMs = 3, 1, 45000
	same.Errno, same.ErrMsg = 999, "重投时改口的失败摘要"
	info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(same)
	info = wantOK(t, info, err, "终态同归因重投")
	wantReplayEchoesRow(t, "终态同归因重投", info, &was)
	wantNoWrites(t, db, before, "终态同归因重投")
	assertReplayUnchanged(t, db, row.ReplayId, was, "终态同归因重投")
	wantField(t, "终态同归因重投", "errno 未被重投改写", was.Errno, int32(11))

	// 换一个原因重投：ErrTerminalState，行内归因一字未动。
	other := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_FAILED)
	other.Reason = rpc.FailureReason_FAILURE_REASON_STORAGE
	info, err = NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(other)
	wantFail(t, info, err, model.ErrTerminalState, "终态换归因重投")
	wantNoWrites(t, db, before, "终态换归因重投")
	assertReplayUnchanged(t, db, row.ReplayId, was, "终态换归因重投")

	// 不带 reason 的同态重投：FAILED 的归因门禁不看行内已有值 —— 一次「没有归因的失败上报」
	// 本身就是无效主张，必须重投同一个 reason 才算重放（这里连读库都不该发生）。
	quiet := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_FAILED)
	info, err = NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(quiet)
	wantFail(t, info, err, model.ErrInvalidTransition, "终态不带 reason 的重投")
	wantNoWrites(t, db, before, "终态不带 reason 的重投")
	assertReplayUnchanged(t, db, row.ReplayId, was, "终态不带 reason 的重投")
	wantEvents(t, db, nil, "FAILED 终态一律零事件")
}

// 进度字段的算术约束：负数、超出拼接区间、时长倒退。
// 这些都发生在写库之前，所以每例都断言零写；行内既有事实不得被拒掉的请求污染。
func TestReportReplayProgressProgressFieldsMustStayInsideRangeAndMonotonic(t *testing.T) {
	cases := []struct {
		name                   string
		segment, gap, duration int64
		want                   error
	}{
		{"segment_count 为负", -1, 0, 0, model.ErrInvalidTransition},
		{"gap_count 为负", 0, -1, 0, model.ErrInvalidTransition},
		{"duration_ms 为负", 0, 0, -1, model.ErrInvalidTransition},
		{"segment_count 超出拼接区间", 6, 0, 0, model.ErrInvalidSegmentRange},
		{"gap_count 超出拼接区间", 0, 6, 0, model.ErrInvalidSegmentRange},
		{"素材数+缺口数 超出拼接区间", 3, 3, 0, model.ErrInvalidSegmentRange},
		{"时长倒退（产物时长只增不减）", 0, 0, 30000, model.ErrInvalidTransition},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		// from=MERGING、to=UPLOADING 是合法边：唯一的失败来源只能是字段算术。
		row := seedReplay(db, model.ReplayStateMerging, func(r *model.LiveReplayTask) {
			r.SegmentCount, r.GapCount, r.DurationMs = 3, 1, 45000
			r.FromSeq, r.ToSeq = 1, 5
		})
		was := *row
		before := snapshotWrites(db)

		in := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_UPLOADING)
		in.SegmentCount, in.GapCount, in.DurationMs = tc.segment, tc.gap, tc.duration
		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
		wantFail(t, info, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
		wantCalls(t, db, "ReplayTasks.FindOne", 0, 1, tc.name+"：判据要读行，但只读一次")
		assertReplayUnchanged(t, db, row.ReplayId, was, tc.name)
		wantEvents(t, db, nil, tc.name)
	}

	// 边界值：恰好等于区间长度（3+2=5）合法，且落库后仍与 [from_seq,to_seq] 自洽。
	db := newStore()
	svcCtx := newTestSvc(db)
	edge := seedReplay(db, model.ReplayStateMerging, func(r *model.LiveReplayTask) {
		r.SegmentCount, r.GapCount, r.DurationMs = 3, 1, 45000
		r.FromSeq, r.ToSeq = 1, 5
	})
	in := reportReplayReq(edge.ReplayId, rpc.ReplayState_REPLAY_STATE_UPLOADING)
	in.SegmentCount, in.GapCount = 3, 2
	info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
	info = wantOK(t, info, err, "计数恰好铺满区间")
	cur := mustReplay(t, db, edge.ReplayId)
	wantField(t, "计数恰好铺满区间", "state", cur.State, model.ReplayStateUploading)
	wantField(t, "计数恰好铺满区间", "segment_count", cur.SegmentCount, int64(3))
	wantField(t, "计数恰好铺满区间", "gap_count", cur.GapCount, int64(2))
	wantField(t, "计数恰好铺满区间", "素材数+缺口数 = 区间长度",
		cur.SegmentCount+cur.GapCount, cur.ToSeq-cur.FromSeq+1)
	wantEvents(t, db, nil, "计数恰好铺满区间")
}

// REGISTERED 的产物引用门禁：给了就得给全，而且只能是桶 + 相对 key。
// 签名地址与绝对路径一律拒绝，且被拒的引用不得在任何地方留下痕迹。
func TestReportReplayProgressRegisteredNeedsCleanRelativeRef(t *testing.T) {
	cases := []struct {
		name   string
		bucket string
		key    string
	}{
		{"只给桶不给 key", replayProductBucket, ""},
		{"key 是绝对路径", replayProductBucket, "/replay/71001/merged.m3u8"},
		{"key 是带签名的完整 URL", replayProductBucket, "https://cdn.example.com/replay.m3u8?sig=" + leakMarker},
		{"桶为空 key 有值", "", replayProductKey},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		row := seedReplay(db, model.ReplayStateUploading, nil)
		was := *row
		before := snapshotWrites(db)

		in := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_REGISTERED)
		in.OutputBucket, in.OutputKey = tc.bucket, tc.key
		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
		wantFail(t, info, err, model.ErrInvalidBucketRef, tc.name)
		wantNoWrites(t, db, before, tc.name)
		assertReplayUnchanged(t, db, row.ReplayId, was, tc.name)
		wantEvents(t, db, nil, tc.name)
		wantNoLeak(t, db, tc.name+"：被拒的引用不留痕")
	}

	// 「空 = 本次不提供」：行内已有完整引用时，REGISTERED 不必重报引用。
	db := newStore()
	svcCtx := newTestSvc(db)
	keep := registeredReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateUploading })
	info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).
		ReportReplayProgress(reportReplayReq(keep.ReplayId, rpc.ReplayState_REPLAY_STATE_REGISTERED))
	info = wantOK(t, info, err, "REGISTERED 沿用行内引用")
	cur := mustReplay(t, db, keep.ReplayId)
	wantReplayEchoesRow(t, "REGISTERED 沿用行内引用", info, cur)
	wantField(t, "REGISTERED 沿用行内引用", "state", cur.State, model.ReplayStateRegistered)
	wantField(t, "REGISTERED 沿用行内引用", "output_bucket 沿用", cur.OutputBucket, replayProductBucket)
	wantField(t, "REGISTERED 沿用行内引用", "output_key 沿用", cur.OutputKey, replayProductKey)
	wantEvents(t, db, nil, "REGISTERED 沿用行内引用")

	// 换引用必须挂在状态边上：UPLOADING→REGISTERED 同时把引用改到新的桶。
	db = newStore()
	svcCtx = newTestSvc(db)
	move := seedReplay(db, model.ReplayStateUploading, func(r *model.LiveReplayTask) {
		r.OutputBucket, r.OutputKey = "old-bucket", "old/merged.m3u8"
	})
	chg := reportReplayReq(move.ReplayId, rpc.ReplayState_REPLAY_STATE_REGISTERED)
	chg.OutputBucket, chg.OutputKey = replayProductBucket, replayProductKey
	info, err = NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(chg)
	info = wantOK(t, info, err, "REGISTERED 换引用")
	cur = mustReplay(t, db, move.ReplayId)
	wantField(t, "REGISTERED 换引用", "output_bucket 已换", cur.OutputBucket, replayProductBucket)
	wantField(t, "REGISTERED 换引用", "output_key 已换", cur.OutputKey, replayProductKey)
	wantEvents(t, db, nil, "REGISTERED 换引用")
}

// REVIEW_SUBMITTED 的前置事实是「asset_id/aid 已由 BindReplayAsset 回填」：
// 缺引用就发「已送审」事件，下游拿到的是 asset_id=0 的空壳。
// 本方法既不能自己造一个 asset_id，也不能放行。
func TestReportReplayProgressReviewSubmittedNeedsBoundAssetKeys(t *testing.T) {
	cases := []struct {
		name       string
		asset, aid int64
	}{
		{"未绑定媒资", 0, 0},
		{"只登记了 asset 未建稿", replayAssetID, 0},
		{"只建了稿未登记 asset", 0, replayAid},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		row := registeredReplay(db, func(r *model.LiveReplayTask) { r.AssetId, r.Aid = tc.asset, tc.aid })
		was := *row
		before := snapshotWrites(db)

		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).
			ReportReplayProgress(reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_REVIEW_SUBMITTED))
		wantFail(t, info, err, model.ErrInvalidAssetID, tc.name)
		wantNoWrites(t, db, before, tc.name)
		assertReplayUnchanged(t, db, row.ReplayId, was, tc.name)
		wantField(t, tc.name, "不得为过门禁伪造 asset_id", mustReplay(t, db, row.ReplayId).AssetId, tc.asset)
		wantField(t, tc.name, "不得为过门禁伪造 aid", mustReplay(t, db, row.ReplayId).Aid, tc.aid)
		wantEvents(t, db, nil, tc.name)
	}
}

// 唯一发事件的边：状态推进与 Outbox 同事务，事件带的是「本次生效值」而不是事务前的旧账。
func TestReportReplayProgressReviewSubmittedEmitsEventWithEffectiveCounts(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := boundReplay(db, nil) // REGISTERED + asset/aid + 产物齐全
	was := *row

	in := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_REVIEW_SUBMITTED)
	in.SegmentCount, in.GapCount, in.DurationMs = 4, 1, 61000
	in.ExpectedVersion = row.Version
	in.WorkerId = "  worker-merge  "
	info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
	info = wantOK(t, info, err, "REVIEW_SUBMITTED")

	cur := mustReplay(t, db, row.ReplayId)
	wantReplayEchoesRow(t, "REVIEW_SUBMITTED", info, cur)
	wantField(t, "REVIEW_SUBMITTED", "state", cur.State, model.ReplayStateReviewSubmitted)
	wantField(t, "REVIEW_SUBMITTED", "version", cur.Version, was.Version+1)
	wantField(t, "REVIEW_SUBMITTED", "segment_count", cur.SegmentCount, int64(4))
	wantField(t, "REVIEW_SUBMITTED", "gap_count", cur.GapCount, int64(1))
	wantField(t, "REVIEW_SUBMITTED", "duration_ms", cur.DurationMs, int64(61000))
	// 送审边只许改状态机列与本次上报的产物账：产物引用、主键引用、标题、区间一律不得被顺手改写，
	// 更不许把回放推到 COMPLETED。
	if stripReplayProgress(stripReplayDrive(*cur)) != stripReplayProgress(stripReplayDrive(was)) {
		t.Fatalf("REVIEW_SUBMITTED 只该改状态机与产物账列：\n got=%+v\nwant=%+v",
			stripReplayProgress(stripReplayDrive(*cur)), stripReplayProgress(stripReplayDrive(was)))
	}

	wantEvents(t, db, []string{"livemedia.replay.review.submitted"}, "REVIEW_SUBMITTED")
	ev := eventAt(t, db, 0)
	// 事件类型写字面量而不是常量：常量被改名时 Topic 会跟着漂移，而下游订阅的是这个字符串。
	wantField(t, "送审事件", "event_type 与常量一致", ev.EventType, model.EventTypeReplayReviewSubmitted)
	wantField(t, "送审事件", "aggregate_type", ev.AggregateType, model.AggregateReplayTask)
	wantField(t, "送审事件", "aggregate_id", ev.AggregateId, strconv.FormatInt(row.ReplayId, 10))
	wantField(t, "送审事件", "room_id 冗余列", ev.RoomId, was.RoomId)
	wantField(t, "送审事件", "schema_version", ev.SchemaVersion, model.EventSchemaVersion)
	wantField(t, "送审事件", "state 待发布", ev.State, model.OutboxStatePending)
	if ev.EventId == "" {
		t.Fatalf("送审事件没有 event_id，消费者无法幂等")
	}

	p := eventPayload(t, ev)
	wantField(t, "送审事件 payload", "replay_id", toInt64(t, p, "replay_id"), row.ReplayId)
	wantField(t, "送审事件 payload", "record_id", toInt64(t, p, "record_id"), was.RecordId)
	wantField(t, "送审事件 payload", "room_id", toInt64(t, p, "room_id"), was.RoomId)
	wantField(t, "送审事件 payload", "live_session_id", toInt64(t, p, "live_session_id"), was.LiveSession)
	wantField(t, "送审事件 payload", "asset_id", toInt64(t, p, "asset_id"), replayAssetID)
	wantField(t, "送审事件 payload", "aid", toInt64(t, p, "aid"), replayAid)
	wantField(t, "送审事件 payload", "anchor_mid", toInt64(t, p, "anchor_mid"), was.AnchorMid)
	wantField(t, "送审事件 payload", "from_seq", toInt64(t, p, "from_seq"), was.FromSeq)
	wantField(t, "送审事件 payload", "to_seq", toInt64(t, p, "to_seq"), was.ToSeq)
	// 三条「本次生效值」：用事务前的旧账发事件会让下游拿到一条与状态推进不匹配的素材数。
	wantField(t, "送审事件 payload", "segment_count 取本次值", toInt64(t, p, "segment_count"), int64(4))
	wantField(t, "送审事件 payload", "gap_count 取本次值", toInt64(t, p, "gap_count"), int64(1))
	wantField(t, "送审事件 payload", "duration_ms 取本次值", toInt64(t, p, "duration_ms"), int64(61000))
	// worker_id 归一后入事件：下游按它定位是哪个拼接 Worker 送的审。
	if got, _ := p["worker_id"].(string); got != "worker-merge" {
		t.Fatalf("送审事件 worker_id 应为归一后的 \"worker-merge\"，实际 %q", got)
	}
	// 事件里只有主键引用与相对量：不得出现拉流地址/签名参数/对象存储凭据。
	wantNoLeak(t, db, "送审事件")
}

// 业务写与 Outbox 必须同生共死：事件写失败时状态边也得回滚，
// 否则「已送审」的行配一条不存在的事件，下游永远等不到投递。
func TestReportReplayProgressOutboxFailureRollsBackTheReviewSubmittedEdge(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := boundReplay(db, nil)
	was := *row
	db.failOn("Outbox.Insert", errOutboxDown)

	in := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_REVIEW_SUBMITTED)
	in.SegmentCount, in.GapCount = 4, 1
	info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
	if err == nil || !strings.Contains(err.Error(), errOutboxDown.Error()) {
		t.Fatalf("Outbox 故障必须原样上抛，实际 %v", err)
	}
	if !isNilPtr(info) {
		t.Fatalf("Outbox 故障时不得带回响应体：%+v", info)
	}
	// 半产品检查：状态、版本、计数全部回到事务前。
	assertReplayUnchanged(t, db, row.ReplayId, was, "Outbox 故障回滚")
	wantField(t, "Outbox 故障回滚", "state 未推进", was.State, model.ReplayStateRegistered)
	// 调用计数不参与回滚（与 MySQL 的 AUTO_INCREMENT 同一口径）：
	// 这里断言「UPDATE 真的发过、事件真的没落」，比只看错误码更硬。
	wantCalls(t, db, "ReplayTasks.UpdateStateTx", 0, 1, "Outbox 故障回滚：UPDATE 发过")
	wantCalls(t, db, "Outbox.Insert", 0, 1, "Outbox 故障回滚：INSERT 发过")
	wantEvents(t, db, nil, "Outbox 故障回滚")
	if len(db.outbox) != 0 {
		t.Fatalf("Outbox 故障后不得留下事件行，实际 %d 条", len(db.outbox))
	}
	wantNoLeak(t, db, "Outbox 故障回滚")
}

// 交错用例：logic 读到 state=REGISTERED 之后、UPDATE 发出之前，
// 另一条链路（回收/投影）把行推到了 COMPLETED。conditional UPDATE 必然 0 行，
// 归因由 classifyReplayZeroRow 给：带了 expected_version 就是版本冲突，
// 没带就按「行已在终态」回答；两种情况都不得留下半成品。
func TestReportReplayProgressConcurrentEdgeLosesCasAndLeavesNoHalfProduct(t *testing.T) {
	cases := []struct {
		name string
		ev   int64
		want error
	}{
		{"声明版本 → 归因版本冲突", 1, model.ErrVersionConflict},
		{"不声明版本 → 归因终态", 0, model.ErrTerminalState},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		row := boundReplay(db, nil)
		was := *row
		db.onHit("ReplayTasks.UpdateStateTx", func() {
			// 模拟并发方：投影把这条回放推到 COMPLETED 并 ++version。
			live := db.replays[row.ReplayId]
			live.State, live.Version = model.ReplayStateCompleted, live.Version+1
		})

		in := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_REVIEW_SUBMITTED)
		in.ExpectedVersion = tc.ev
		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(in)
		wantFail(t, info, err, tc.want, tc.name)
		// 事务回滚撤销了钩子造出来的态，也撤销了 logic 自己的写：库里就是提交前的样子。
		assertReplayUnchanged(t, db, row.ReplayId, was, tc.name)
		wantCalls(t, db, "ReplayTasks.UpdateStateTx", 0, 1, tc.name+"：只发过一次条件 UPDATE")
		wantCalls(t, db, "Outbox.Insert", 0, 0, tc.name+"：0 行时不得登记事件")
		wantEvents(t, db, nil, tc.name)
		wantNoLeak(t, db, tc.name)
	}
}

// 迟到的 expected_version（调用方看过更新的行）：视图不可信，让它重读。
func TestReportReplayProgressRejectsLateAndAheadVersions(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedReplay(db, model.ReplayStateMerging, func(r *model.LiveReplayTask) { r.Version = 7 })
	was := *row
	before := snapshotWrites(db)

	// 负版本号：连读都不必读，直接拒。
	neg := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_UPLOADING)
	neg.ExpectedVersion = -1
	info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(neg)
	wantFail(t, info, err, model.ErrVersionConflict, "expected_version 为负")
	wantNoWrites(t, db, before, "expected_version 为负")
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, "expected_version 为负：门禁在读库之前")

	// 版本超前 + 什么都不改：走「同值重放」分支里的版本检查，回 ErrVersionConflict 而不是回行。
	ahead := reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_MERGING)
	ahead.ExpectedVersion = 99
	info, err = NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(ahead)
	wantFail(t, info, err, model.ErrVersionConflict, "同值重放但版本超前")
	wantNoWrites(t, db, before, "同值重放但版本超前")
	assertReplayUnchanged(t, db, row.ReplayId, was, "同值重放但版本超前")

	// 版本滞后 + 真要推进：条件 UPDATE 0 行 → 归因版本冲突，事务回滚。
	ahead = reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_UPLOADING)
	ahead.ExpectedVersion = 6
	info, err = NewReportReplayProgressLogic(context.Background(), svcCtx).ReportReplayProgress(ahead)
	wantFail(t, info, err, model.ErrVersionConflict, "版本滞后的前进边")
	assertReplayUnchanged(t, db, row.ReplayId, was, "版本滞后的前进边")
	wantCalls(t, db, "ReplayTasks.UpdateStateTx", 0, 1, "版本滞后的前进边：0 行 UPDATE")
	wantEvents(t, db, nil, "版本口径")
}

// 回放任务不存在 / model 故障：一律失败关闭，不得凭空造行。
func TestReportReplayProgressFailsClosedOnMissingRowAndModelErrors(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)

	info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).
		ReportReplayProgress(reportReplayReq(424242, rpc.ReplayState_REPLAY_STATE_MERGING))
	wantFail(t, info, err, model.ErrReplayTaskNotFound, "回放不存在")
	wantNoWrites(t, db, before, "回放不存在")

	// replay_id<=0 的门禁也回 ErrReplayTaskNotFound（调用方据此区分「没这条路」）。
	info, err = NewReportReplayProgressLogic(context.Background(), svcCtx).
		ReportReplayProgress(reportReplayReq(0, rpc.ReplayState_REPLAY_STATE_MERGING))
	wantFail(t, info, err, model.ErrReplayTaskNotFound, "replay_id 缺失")
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 1, "replay_id<=0 不查库")

	for _, op := range []string{"ReplayTasks.FindOne", "ReplayTasks.UpdateStateTx", "DB.TransactCtx"} {
		db := newStore()
		svcCtx := newTestSvc(db)
		row := seedReplay(db, model.ReplayStateMerging, nil)
		was := *row
		db.failOn(op, errModelDown)
		info, err := NewReportReplayProgressLogic(context.Background(), svcCtx).
			ReportReplayProgress(reportReplayReq(row.ReplayId, rpc.ReplayState_REPLAY_STATE_UPLOADING))
		if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
			t.Fatalf("%s 故障应原样上抛：%v", op, err)
		}
		if !isNilPtr(info) {
			t.Fatalf("%s 故障时不得带回响应体：%+v", op, info)
		}
		assertReplayUnchanged(t, db, row.ReplayId, was, op+" 故障")
		wantEvents(t, db, nil, op+" 故障")
	}
}
