package logic

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"
)

// 回放引用侧两个写方法：BindReplayAsset 与 ApplyReplayContentState。
//
// 这两个方法都在守「跨服务边界」，不守自己的状态机，所以用例的主轴是写权限清单：
//
//	BindReplayAsset 只登记引用（asset_id/aid/bvid + 产物坐标），它必须
//	  - 不推进稿件、不发「已发布」类事件（本方法一条事件都不发，每个用例都以 wantEvents(nil) 收尾）；
//	  - 不越权推进回放任务状态：patch 里带的是刚读到的原状态，只回填主键列；
//	  - 房间/场次/录制主键/区间/缺口一律取自任务行 —— 请求里根本没有这些字段，
//	    「跨房间绑定」在本方法里是结构上不可表达的，用例钉的是「它没拿别的来源去填」；
//	  - 引用行 UPSERT 与任务行回填在同一事务里，任何一半失败都必须整体回滚（不能有
//	    「引用行有 asset_id、任务行还是 0」的半产品）。
//
//	ApplyReplayContentState 是 video→live-media 的单向投影，写权限只有五列投影
//	  （review_state/review_state_at/published_at/last_event_id/source）加两条被契约允许的联动：
//	  REVIEW_SUBMITTED→COMPLETED、引用生命周期 Normal→Pending。
//	  判据因此是「三条路径都要真实可达」：同事件重放零写入、旧事件被挡不报错也不再发事件、
//	  越界迁移失败关闭；再加上「除投影列与 trace_id/mtime 之外引用行一列都不许动」。
//
// 幂等口径两个方法完全不同，各自钉死：绑定侧是天然键 replay_id + bindingUnchanged 四列同值；
// 投影侧是 last_event_id 相等（读侧早退）与 review_state_at 单调（写侧 0 行后归因）。
//
// 交错用例沿用本包既有口径：db.onHit 在写入瞬间改库，事务回滚会撤销钩子造出来的态，
// 所以断言的是「归因结论 + 没有半成品」，而不是交错后的最终库态。

// ---------------------------------------------------------------- 小工具

// refForTask 落一行「已绑定但投影未同步」的引用行，字段与 BindReplayAsset 会写出来的形状一致。
// 投影通道与重绑用例都需要从「绑定已完成」这一点起跑，而 BindReplayAsset 自己造不出
// 投影已同步、生命周期已推进这些中点态（绑定时刻拿不到 video 结论）。
func refForTask(db *store, task *model.LiveReplayTask,
	mutate func(*model.LiveReplayAssetRef)) *model.LiveReplayAssetRef {
	return seedReplayRef(db, func(r *model.LiveReplayAssetRef) {
		r.RoomId, r.LiveSession = task.RoomId, task.LiveSession
		r.ReplayId, r.RecordId = task.ReplayId, task.RecordId
		r.AssetId, r.Aid, r.Bvid = replayAssetID, replayAid, replayBvid
		r.AnchorMid = task.AnchorMid
		r.Bucket, r.ObjectKey = replayProductBucket, replayProductKey
		r.DurationMs = 60000
		r.SegmentFromSeq, r.SegmentToSeq, r.GapCount = task.FromSeq, task.ToSeq, task.GapCount
		if mutate != nil {
			mutate(r)
		}
	})
}

// onlyRef 库内恰好一行的引用行：绑定动作「不多不少一行」的判据。
func onlyRef(t *testing.T, db *store, label string) *model.LiveReplayAssetRef {
	t.Helper()
	if len(db.replayRefs) != 1 {
		ids := make([]int64, 0, len(db.replayRefs))
		for id := range db.replayRefs {
			ids = append(ids, id)
		}
		t.Fatalf("%s：库内应恰好 1 行引用，实际 %d 行 %v", label, len(db.replayRefs), ids)
	}
	for _, row := range db.replayRefs {
		return row
	}
	return nil
}

// wantBool 钉 payload 里的布尔判据：写成 if p["x"] != true 会在 JSON 解码成非 bool 时静默通过。
func wantBool(t *testing.T, p map[string]any, key string, want bool) {
	t.Helper()
	v, ok := p[key]
	if !ok {
		t.Fatalf("payload 缺字段 %q：%v", key, p)
	}
	got, ok2 := v.(bool)
	if !ok2 {
		t.Fatalf("payload 字段 %q 不是布尔：%T", key, v)
	}
	if got != want {
		t.Fatalf("payload.%v=%v，期望 %v", key, got, want)
	}
}

func wantString(t *testing.T, p map[string]any, key, want string) {
	t.Helper()
	v, ok := p[key]
	if !ok {
		t.Fatalf("payload 缺字段 %q：%v", key, p)
	}
	got, ok2 := v.(string)
	if !ok2 {
		t.Fatalf("payload 字段 %q 不是字符串：%T", key, v)
	}
	if got != want {
		t.Fatalf("payload.%v=%q，期望 %q", key, got, want)
	}
}

// assetScopedReq 只给 asset_id 的投影请求（video 侧事件常常只带稿件主键）。
func assetScopedReq(assetID int64, state rpc.ReviewState, eventID string) *rpc.ApplyReplayContentStateReq {
	in := applyContentReq(0, state, eventID)
	in.AssetId = assetID
	return in
}

// firedWrites 列出 after 之后真的发生过的写方法（按 writeOps 的固定顺序拼串）。
// wantNoWrites 只能证「一条写都没有」，成功路径需要的是反向判据：
// 「副作用集合恰好等于这一条契约允许的那几个」—— 多一个就是越权（比如顺手写了 Outbox
// 或替 Worker 推进了状态），少一个就是半提交。
func firedWrites(db *store, before map[string]int) string {
	var out []string
	for _, op := range writeOps {
		if db.count(op) > before[op] {
			out = append(out, op)
		}
	}
	return strings.Join(out, ",")
}

// stripReplayBindKeys 抹掉「绑定动作有权回填的列」：跨服务主键引用、产物坐标与时长。
// 与 stripReplayDrive/stripReplayProgress 组合起来就是
// 「除状态机、产物账与本方法有权回填的主键引用之外，任务行一列都不许动」的判据。
func stripReplayBindKeys(r model.LiveReplayTask) model.LiveReplayTask {
	r.AssetId, r.Aid, r.Bvid = 0, 0, ""
	r.OutputBucket, r.OutputKey, r.DurationMs = "", "", 0
	return r
}

// ---------------------------------------------------------------- BindReplayAsset

// 首次绑定：一行引用 + 任务主键回填同事务提交，投影列一律留空。
//
// 这一条把「绑定写了什么」逐列钉死，其中三处最容易写错：
//   - 区间/缺口/录制主键取自任务行快照，而不是请求（请求里根本没有这些字段）；
//   - 五个投影列必须为空：写入权归 ApplyReplayContentState，绑定就把它填成 0 之外
//     的任何值，都是「本地替 video 下结论」；
//   - 任务状态保持 REGISTERED：本方法只回填主键，不推进状态机（version 仍会 ++，
//     因为行确实被改写过 —— 这是 CAS 方需要看到的事实）。
func TestBindReplayAssetWritesRefRowAndTaskKeysInOneCommit(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := registeredReplay(db, func(r *model.LiveReplayTask) {
		r.DurationMs = 59000 // 比本次绑定的 60000 小：允许增长，且让「回填了时长」可观测
	})
	was := *task
	before := snapshotWrites(db)

	in := bindReplayReq(task.ReplayId, "  req-bind-1  ") // 首尾空白：校验与落库必须用同一个归一值
	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(in)
	info = wantOK(t, info, err, "首次绑定")

	row := onlyRef(t, db, "首次绑定")
	wantRefEchoesRow(t, "首次绑定", info, row)
	wantField(t, "首次绑定", "asset_id", row.AssetId, replayAssetID)
	wantField(t, "首次绑定", "aid", row.Aid, replayAid)
	wantField(t, "首次绑定", "bvid", row.Bvid, replayBvid)
	wantField(t, "首次绑定", "request_id 归一后落库", row.RequestId, "req-bind-1")
	wantField(t, "首次绑定", "trace_id", row.TraceId, "trace-replay-bind")
	// 区间与缺口来自任务行快照：Worker 重复计算就会与回放结论分叉。
	wantField(t, "首次绑定", "record_id 取自任务行", row.RecordId, was.RecordId)
	wantField(t, "首次绑定", "segment_from_seq 取自任务行", row.SegmentFromSeq, was.FromSeq)
	wantField(t, "首次绑定", "segment_to_seq 取自任务行", row.SegmentToSeq, was.ToSeq)
	wantField(t, "首次绑定", "gap_count 取自任务行", row.GapCount, was.GapCount)
	// 投影列留空 + 生命周期正常：绑定时刻本服务对稿件一无所知。
	wantField(t, "首次绑定", "review_state 不得被本地结论污染", row.ReviewState, model.ReviewStateUnsynced)
	wantField(t, "首次绑定", "review_state_at", row.ReviewStateAt, int64(0))
	wantField(t, "首次绑定", "published_at", row.PublishedAt, int64(0))
	wantField(t, "首次绑定", "last_event_id", row.LastEventId, "")
	wantField(t, "首次绑定", "source", row.Source, "")
	wantField(t, "首次绑定", "retention_state", row.RetentionState, model.RefRetentionStateNormal)

	cur := mustReplay(t, db, task.ReplayId)
	wantField(t, "首次绑定", "任务 asset_id 回填", cur.AssetId, replayAssetID)
	wantField(t, "首次绑定", "任务 aid 回填", cur.Aid, replayAid)
	wantField(t, "首次绑定", "任务 bvid 回填", cur.Bvid, replayBvid)
	wantField(t, "首次绑定", "任务时长取本次值", cur.DurationMs, int64(60000))
	wantField(t, "首次绑定", "状态不越权推进", cur.State, model.ReplayStateRegistered)
	wantField(t, "首次绑定", "version 递增（行确实改写过）", cur.Version, was.Version+1)
	// 除状态机列、本次回填的主键引用与产物账之外，标题、区间、切片数、录制主键一律不得被顺手改写。
	if stripReplayBindKeys(stripReplayProgress(stripReplayDrive(*cur))) !=
		stripReplayBindKeys(stripReplayProgress(stripReplayDrive(was))) {
		t.Fatalf("绑定只该回填主键引用与时长：\n got=%+v\nwant=%+v",
			stripReplayBindKeys(stripReplayProgress(stripReplayDrive(*cur))),
			stripReplayBindKeys(stripReplayProgress(stripReplayDrive(was))))
	}

	// 两条写在同一事务里：一次 TransactCtx、一次 UPSERT、一次条件 UPDATE。
	wantCalls(t, db, "DB.TransactCtx", 0, 1, "首次绑定")
	wantCalls(t, db, "ReplayRefs.UpsertTx", 0, 1, "首次绑定")
	wantCalls(t, db, "ReplayTasks.UpdateStateTx", 0, 1, "首次绑定")
	wantCalls(t, db, "ReplayTasks.UpdateState", 0, 0, "首次绑定：事务外不得写任务行")
	wantField(t, "首次绑定", "副作用集合恰好是允许的那三个",
		firedWrites(db, before), "DB.TransactCtx,ReplayTasks.UpdateStateTx,ReplayRefs.UpsertTx")
	// 本方法一条事件都不发：词表里没有「引用已绑定」，按「不伪造 event_type」必须为零。
	wantEvents(t, db, nil, "首次绑定")
	wantNoLeak(t, db, "首次绑定")
}

// 同值重放是成功而不是冲突，且必须「什么都不写」：
// 回写会把 request_id 换成重放方的、mtime 推后、version ++，
// 于是一次纯读操作伪造出「产物刚被重新登记过」的证据，还会撞掉并发方的 CAS。
func TestBindReplayAssetIdenticalRetryWritesNothing(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, nil)
	ref := refForTask(db, task, nil) // 与 bindReplayReq 逐列同值
	wasRef, wasTask := *ref, *task
	before := snapshotWrites(db)

	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-retry"))
	info = wantOK(t, info, err, "同值重放")

	wantRefEchoesRow(t, "同值重放", info, mustRef(t, db, ref.Id))
	assertRefUnchanged(t, db, ref.Id, wasRef, "同值重放")
	assertReplayUnchanged(t, db, task.ReplayId, wasTask, "同值重放")
	wantField(t, "同值重放", "request_id 保留首次登记的键", mustRef(t, db, ref.Id).RequestId, wasRef.RequestId)
	wantField(t, "同值重放", "version 不动", mustReplay(t, db, task.ReplayId).Version, wasTask.Version)
	// 早退发生在读任务行之前：连「读一眼任务」都不该发生，更不该开事务。
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, "同值重放不得读任务行")
	wantCalls(t, db, "DB.TransactCtx", 0, 0, "同值重放不得开事务")
	wantNoWrites(t, db, before, "同值重放")
	wantEvents(t, db, nil, "同值重放")
}

// 同一 (asset_id, aid) 下刷新 bvid / 时长是允许的（video 侧稍后才补齐 bvid 是正常链路）。
// 关键判据是 ON DUPLICATE 的更新列表：它不含 asset_id/aid/replay_id/id/ctime 与五个投影列，
// 也不含 retention_state —— 重绑既不能把引用挪到别的稿件上，也不能把已同步的投影抹平，
// 更不能把「待回收」的生命周期倒回正常。
func TestBindReplayAssetRefreshKeepsProjectionAndNaturalKeys(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, nil)
	ref := refForTask(db, task, func(r *model.LiveReplayAssetRef) {
		r.Bvid = "BV1old000000000"
		r.ReviewState, r.ReviewStateAt = model.ReviewStatePublished, 1000002
		r.PublishedAt, r.LastEventId, r.Source = 1000001, "content.published.v1:evt-old", "video.rpc"
		r.RetentionState = model.RefRetentionStatePending
		r.Ctime = 900000
	})
	wasProjection := *ref

	in := bindReplayReq(task.ReplayId, "req-bind-refresh")
	in.Bvid = "BV1refreshed"
	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(in)
	info = wantOK(t, info, err, "同稿件刷新")

	cur := mustRef(t, db, ref.Id)
	wantRefEchoesRow(t, "同稿件刷新", info, cur)
	wantField(t, "同稿件刷新", "仍是同一行（未新增）", int64(countRefs(db)), ref.Id)
	wantField(t, "同稿件刷新", "bvid 已刷新", cur.Bvid, "BV1refreshed")
	wantField(t, "同稿件刷新", "request_id 刷新", cur.RequestId, "req-bind-refresh")
	// 天然键与投影列：ON DUPLICATE 的更新列表里没有它们，写了就是越权。
	wantField(t, "同稿件刷新", "id 不变", cur.Id, wasProjection.Id)
	wantField(t, "同稿件刷新", "ctime 不变", cur.Ctime, wasProjection.Ctime)
	wantField(t, "同稿件刷新", "replay_id 不变", cur.ReplayId, wasProjection.ReplayId)
	wantField(t, "同稿件刷新", "asset_id 不变", cur.AssetId, wasProjection.AssetId)
	wantField(t, "同稿件刷新", "aid 不变", cur.Aid, wasProjection.Aid)
	wantField(t, "同稿件刷新", "review_state 未被抹平", cur.ReviewState, model.ReviewStatePublished)
	wantField(t, "同稿件刷新", "review_state_at 未被抹平", cur.ReviewStateAt, int64(1000002))
	wantField(t, "同稿件刷新", "published_at 未被抹平", cur.PublishedAt, int64(1000001))
	wantField(t, "同稿件刷新", "last_event_id 未被抹平", cur.LastEventId, "content.published.v1:evt-old")
	wantField(t, "同稿件刷新", "source 未被抹平", cur.Source, "video.rpc")
	wantField(t, "同稿件刷新", "retention_state 未倒退", cur.RetentionState, model.RefRetentionStatePending)
	wantField(t, "同稿件刷新", "任务状态不推进", mustReplay(t, db, task.ReplayId).State, model.ReplayStateRegistered)
	wantCalls(t, db, "DB.TransactCtx", 0, 1, "同稿件刷新")
	wantNoLeak(t, db, "同稿件刷新")
}

// 改绑到另一份产物/稿件必须拒绝：一条回放只能对应一份产物一份稿件。
// logic 先给可读结论（ErrAssetRefConflict），而不是让 UPSERT 去撞唯一键。
func TestBindReplayAssetRejectsRebindingToAnotherAsset(t *testing.T) {
	cases := []struct {
		name       string
		asset, aid int64
		want       error
	}{
		{"换媒资", replayAssetID + 1, replayAid, model.ErrAssetRefConflict},
		{"换稿件", replayAssetID, replayAid + 1, model.ErrAssetRefConflict},
		{"媒资与稿件一起换", replayAssetID + 1, replayAid + 1, model.ErrAssetRefConflict},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := boundReplay(db, nil)
		ref := refForTask(db, task, nil)
		wasRef, wasTask := *ref, *task
		before := snapshotWrites(db)

		in := bindReplayReq(task.ReplayId, "req-bind-other-asset")
		in.AssetId, in.Aid = tc.asset, tc.aid
		info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(in)
		wantFail(t, info, err, tc.want, tc.name)
		assertRefUnchanged(t, db, ref.Id, wasRef, tc.name)
		assertReplayUnchanged(t, db, task.ReplayId, wasTask, tc.name)
		wantNoWrites(t, db, before, tc.name)
		wantEvents(t, db, nil, tc.name)
		wantNoLeak(t, db, tc.name)
	}
}

// 同一份媒资已被「另一条回放」引用时的真实后果：三个唯一键里只有 uniq_replay_id 在
// ON DUPLICATE 的更新列表之外能被 logic 预读拦住，uniq_asset_id 命中时 MySQL 不报 1062，
// 而是把别人的引用行按本请求的产物字段改写一遍。
//
// 这里钉两件事（两条都必须成立，否则就是数据损坏）：
//  1. 调用不能回成功 —— 回读的是「本 replay_id 的 row」，拿不到就必须报错；
//  2. 那次越权改写必须随事务一起回滚，另一条回放的引用行不得留下任何变化。
//
// 归因缺陷见 .gotmp/agents/round4-livemedia.md：错误码是 ErrReplayRefNotFound 而不是
// ErrAssetRefConflict，调用方读不出「这份媒资已被别人认领」。本轮为纯测试轮，不改生产码。
func TestBindReplayAssetSameAssetUnderAnotherReplayRollsBackWithoutHalfProduct(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	other := boundReplay(db, func(r *model.LiveReplayTask) { r.RoomId, r.LiveSession = testRoomOther, testSession2 })
	otherRef := refForTask(db, other, func(r *model.LiveReplayAssetRef) {
		r.ReviewState, r.ReviewStateAt = model.ReviewStatePublished, 1000002
	})
	wasOtherRef := *otherRef

	mine := registeredReplay(db, nil)
	wasMine := *mine

	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(mine.ReplayId, "req-bind-steal"))
	if err == nil {
		t.Fatalf("跨回放抢同一份媒资不得回成功，实际 %+v", info)
	}
	if !isNilPtr(info) {
		t.Fatalf("失败路径不得带回响应体：%+v", info)
	}
	// 越权改写确实发过（计数不参与回滚），但数据必须整体回滚。
	wantCalls(t, db, "ReplayRefs.UpsertTx", 0, 1, "抢同一份媒资：UPSERT 发过一次")
	wantCalls(t, db, "DB.TransactCtx", 0, 1, "抢同一份媒资：事务开过一次")
	if len(db.replayRefs) != 1 {
		t.Fatalf("回滚后库内应只剩另一条回放的引用行，实际 %d 行", len(db.replayRefs))
	}
	assertRefUnchanged(t, db, otherRef.Id, wasOtherRef, "抢同一份媒资：别人的引用行")
	assertReplayUnchanged(t, db, mine.ReplayId, wasMine, "抢同一份媒资：本任务行")
	wantField(t, "抢同一份媒资", "本任务 asset_id 未被回填", mustReplay(t, db, mine.ReplayId).AssetId, int64(0))
	wantEvents(t, db, nil, "抢同一份媒资")
}

// 产物还不存在时不许绑定：PENDING/MERGING/UPLOADING 直接拒，
// FAILED/CANCELLED/COMPLETED 是终态（引用必须在完成之前绑定），也不回补。
func TestBindReplayAssetRejectsStatesThatAreNotBindable(t *testing.T) {
	cases := []struct {
		state int32
		want  error
	}{
		{model.ReplayStatePending, model.ErrInvalidTransition},
		{model.ReplayStateMerging, model.ErrInvalidTransition},
		{model.ReplayStateUploading, model.ErrInvalidTransition},
		{model.ReplayStateCompleted, model.ErrTerminalState},
		{model.ReplayStateFailed, model.ErrTerminalState},
		{model.ReplayStateCancelled, model.ErrTerminalState},
	}
	for _, tc := range cases {
		label := "replay state=" + rpc.ReplayState(tc.state).String()
		db := newStore()
		svcCtx := newTestSvc(db)
		task := seedReplay(db, tc.state, func(r *model.LiveReplayTask) {
			r.OutputBucket, r.OutputKey = replayProductBucket, replayProductKey
			r.DurationMs = 60000
		})
		was := *task
		before := snapshotWrites(db)

		info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
			BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-badstate"))
		wantFail(t, info, err, tc.want, label)
		assertReplayUnchanged(t, db, task.ReplayId, was, label)
		wantCalls(t, db, "DB.TransactCtx", 0, 0, label+"：不开事务")
		wantNoWrites(t, db, before, label)
		wantEvents(t, db, nil, label)
	}
	// 两条允许绑定的边作对照：REGISTERED（产物已上传）与 REVIEW_SUBMITTED
	// （aid 常在送审返回后才拿到，只放开 REGISTERED 会让正常链路走不通）。
	for _, st := range []int32{model.ReplayStateRegistered, model.ReplayStateReviewSubmitted} {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := seedReplay(db, st, func(r *model.LiveReplayTask) {
			r.OutputBucket, r.OutputKey = replayProductBucket, replayProductKey
			r.DurationMs, r.AssetId, r.Aid = 60000, replayAssetID, replayAid
		})
		if _, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
			BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-ok")); err != nil {
			t.Fatalf("state=%d 应允许绑定：%v", st, err)
		}
	}
}

// 门禁顺序：非法入参必须在「第一次读库之前」被拒。
// 绑定动作有两次前置读（引用行、任务行），一旦校验排在读之后，
// 一个坏请求就能刷出两次查库，还会把不存在的 replay_id 变成可枚举探针。
func TestBindReplayAssetValidatesArgumentsBeforeAnyRead(t *testing.T) {
	longReq := strings.Repeat("r", maxRequestIDRunes+1)
	longBvid := strings.Repeat("v", maxBvidRunes+1)
	cases := []struct {
		name   string
		mutate func(*rpc.BindReplayAssetReq)
		want   error
		needle string
	}{
		{"replay_id=0", func(in *rpc.BindReplayAssetReq) { in.ReplayId = 0 }, model.ErrReplayTaskNotFound, ""},
		{"replay_id<0", func(in *rpc.BindReplayAssetReq) { in.ReplayId = -1 }, model.ErrReplayTaskNotFound, ""},
		{"asset_id=0", func(in *rpc.BindReplayAssetReq) { in.AssetId = 0 }, model.ErrInvalidAssetID, ""},
		{"asset_id<0", func(in *rpc.BindReplayAssetReq) { in.AssetId = -1 }, model.ErrInvalidAssetID, ""},
		{"aid=0", func(in *rpc.BindReplayAssetReq) { in.Aid = 0 }, model.ErrInvalidAid, ""},
		{"时长为 0", func(in *rpc.BindReplayAssetReq) { in.DurationMs = 0 }, model.ErrInvalidBucketRef, ""},
		{"时长为负", func(in *rpc.BindReplayAssetReq) { in.DurationMs = -5 }, model.ErrInvalidBucketRef, ""},
		{"request_id 全空白", func(in *rpc.BindReplayAssetReq) { in.RequestId = "   " }, model.ErrEmptyRequestID, ""},
		{"request_id 超长", func(in *rpc.BindReplayAssetReq) { in.RequestId = longReq }, model.ErrEmptyRequestID, "max"},
		{"bucket 带 scheme", func(in *rpc.BindReplayAssetReq) { in.Bucket = "https://oss.example.com" }, model.ErrInvalidBucketRef, "bare bucket"},
		{"bucket 带空格", func(in *rpc.BindReplayAssetReq) { in.Bucket = "live replay" }, model.ErrInvalidBucketRef, "bare bucket"},
		{"object_key 缺失", func(in *rpc.BindReplayAssetReq) { in.ObjectKey = "" }, model.ErrInvalidBucketRef, ""},
		{"object_key 绝对路径", func(in *rpc.BindReplayAssetReq) { in.ObjectKey = "/replay/merged.m3u8" }, model.ErrInvalidBucketRef, "relative"},
		{"object_key 带签名参数", func(in *rpc.BindReplayAssetReq) { in.ObjectKey = leakObjectKey }, model.ErrInvalidBucketRef, "signature"},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := registeredReplay(db, nil)
		before := snapshotWrites(db)

		in := bindReplayReq(task.ReplayId, "req-bind-validate")
		tc.mutate(in)
		info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(in)
		wantFail(t, info, err, tc.want, tc.name)
		if tc.needle != "" && !strings.Contains(err.Error(), tc.needle) {
			t.Errorf("%s：错误应包含 %q，实际 %v", tc.name, tc.needle, err)
		}
		wantCalls(t, db, "ReplayRefs.FindByReplayID", 0, 0, tc.name+"：不得读引用表")
		wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, tc.name+"：不得读任务表")
		wantNoWrites(t, db, before, tc.name)
		wantField(t, tc.name, "不得凭空造出引用行", int64(countRefs(db)), int64(0))
	}

	// bvid 超长没有专属哨兵（跨服务主键不能截断），但必须拒绝且不查库。
	db := newStore()
	svcCtx := newTestSvc(db)
	task := registeredReplay(db, nil)
	before := snapshotWrites(db)
	in := bindReplayReq(task.ReplayId, "req-bind-longbvid")
	in.Bvid = longBvid
	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(in)
	if err == nil || !strings.Contains(err.Error(), "bvid") {
		t.Fatalf("bvid 超长必须拒绝且说明是哪个字段，实际 err=%v", err)
	}
	if !isNilPtr(info) {
		t.Fatalf("校验失败不得带回响应体：%+v", info)
	}
	wantCalls(t, db, "ReplayRefs.FindByReplayID", 0, 0, "bvid 超长：不得读引用表")
	wantNoWrites(t, db, before, "bvid 超长")
	wantNoLeak(t, db, "绑定入参校验")
}

// 房间、场次、录制主键、主播一律取自任务行：请求里没有这些字段。
// 「跨房间/跨应用绑定」在本方法里必须是结构上不可表达的 —— 用例因此故意把任务放在
// 另一个房间里：如果实现哪天改用请求里的别的字段去凑，引用行的房间就会与任务分叉。
func TestBindReplayAssetTakesRoomAndSessionFromTaskRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := registeredReplay(db, func(r *model.LiveReplayTask) {
		r.RoomId, r.LiveSession = testRoomOther, testSession2
		r.AnchorMid = testAnchor + 7
	})
	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-otherroom"))
	info = wantOK(t, info, err, "任务在另一房间")

	row := onlyRef(t, db, "任务在另一房间")
	wantField(t, "归属取自任务行", "room_id", row.RoomId, testRoomOther)
	wantField(t, "归属取自任务行", "live_session_id", row.LiveSession, testSession2)
	wantField(t, "归属取自任务行", "anchor_mid", row.AnchorMid, testAnchor+7)
	wantField(t, "归属取自任务行", "响应 room_id", info.GetRoomId(), testRoomOther)
	wantField(t, "归属取自任务行", "响应 live_session_id", info.GetLiveSessionId(), testSession2)
	wantField(t, "归属取自任务行", "响应 anchor_mid", info.GetAnchorMid(), testAnchor+7)
}

// 产物引用一致性：ReportReplayProgress(REGISTERED) 已登记过 output_bucket/output_key，
// 两处指向不同文件说明 Worker 串了任务；时长倒退同理（产物事实只增不减）。
// 两条都必须在引用行落地之前拦住。
func TestBindReplayAssetRejectsProductMismatchWithReportedOutput(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := registeredReplay(db, nil)
	before := snapshotWrites(db)

	other := bindReplayReq(task.ReplayId, "req-bind-otherfile")
	other.ObjectKey = "replay/71001/88001/another.m3u8"
	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(other)
	wantFail(t, info, err, model.ErrInvalidBucketRef, "产物指向另一个文件")
	if !strings.Contains(err.Error(), "differs from bound") {
		t.Errorf("错误必须说清是两处产物不一致，实际 %v", err)
	}
	wantNoWrites(t, db, before, "产物指向另一个文件")

	shorter := bindReplayReq(task.ReplayId, "req-bind-shrink")
	shorter.DurationMs = 59000 // 任务已记 60000
	info, err = NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(shorter)
	wantFail(t, info, err, model.ErrInvalidTransition, "时长倒退")
	if !strings.Contains(err.Error(), "shrinks") {
		t.Errorf("错误必须说清是时长倒退，实际 %v", err)
	}
	wantField(t, "时长倒退", "任务时长未被改写", mustReplay(t, db, task.ReplayId).DurationMs, int64(60000))
	wantNoWrites(t, db, before, "时长倒退")
	wantEvents(t, db, nil, "产物一致性")

	// 桶名不一致同样拒（两处坐标必须成对一致）。
	otherBucket := bindReplayReq(task.ReplayId, "req-bind-otherbucket")
	otherBucket.Bucket = "live-replay-archive"
	info, err = NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(otherBucket)
	wantFail(t, info, err, model.ErrInvalidBucketRef, "产物在另一个桶")
	wantNoWrites(t, db, before, "产物在另一个桶")
}

// REGISTERED 之前产物坐标为空时（Worker 没登记 output 引用），绑定必须把它补上：
// 否则引用行指向一个对象，任务行却是空串，回放读侧答不出产物在哪。
func TestBindReplayAssetBackfillsEmptyOutputRef(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := registeredReplay(db, func(r *model.LiveReplayTask) { r.OutputBucket, r.OutputKey = "", "" })
	if _, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-backfill")); err != nil {
		t.Fatalf("产物坐标为空时应回补：%v", err)
	}
	cur := mustReplay(t, db, task.ReplayId)
	wantField(t, "空产物坐标回补", "output_bucket", cur.OutputBucket, replayProductBucket)
	wantField(t, "空产物坐标回补", "output_key", cur.OutputKey, replayProductKey)
	ref := onlyRef(t, db, "空产物坐标回补")
	wantField(t, "空产物坐标回补", "引用行同值", ref.Bucket+"/"+ref.ObjectKey,
		replayProductBucket+"/"+replayProductKey)
	// 请求里没有 bvid 时不得把已有 bvid 清空（patch.Bvid 只在非空时给）。
	// 这是**另一个**回放任务：asset_id / aid / 产物坐标都必须与上面那行引用互不相同，
	// 否则 ON DUPLICATE KEY 会命中上一行的唯一键、把它的产物字段改写（那条缺口已由
	// fakeReplayAssetRefs.upsert 的注释与相应用例钉住），而这里要验的只是 bvid 不被抹掉。
	secondKey := replayProductKey + "/second.m3u8"
	withBvid := seedReplay(db, model.ReplayStateRegistered, func(r *model.LiveReplayTask) {
		r.OutputBucket, r.OutputKey = replayProductBucket, secondKey
		r.DurationMs, r.Bvid = 60000, "BV1keepme"
		r.AssetId, r.Aid = replayAssetID+1, replayAid+1
	})
	noBvid := bindReplayReq(withBvid.ReplayId, "req-bind-nobvid")
	noBvid.Bvid = ""
	noBvid.AssetId, noBvid.Aid = replayAssetID+1, replayAid+1
	noBvid.Bucket, noBvid.ObjectKey = replayProductBucket, secondKey
	if _, err := NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(noBvid); err != nil {
		t.Fatalf("bvid 留空应允许（尚未补齐）：%v", err)
	}
	wantField(t, "bvid 留空", "已有 bvid 不被清空", mustReplay(t, db, withBvid.ReplayId).Bvid, "BV1keepme")
	wantField(t, "bvid 留空", "新引用行 bvid 为空", mustRef(t, db, int64(countRefs(db))).Bvid, "")
	wantField(t, "bvid 留空", "本次绑定不得改写上一行引用", ref.Bvid, replayBvid)
	wantEvents(t, db, nil, "产物坐标回补")
}

// 回放任务不存在 / 引用行回读不到：两种都必须失败关闭，且不能凭空留下引用行。
func TestBindReplayAssetMissingTaskAndMissingRereadFailClosed(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedReplay(db, model.ReplayStateRegistered, nil) // 库里另有行，但 replay_id=4242 不存在
	before := snapshotWrites(db)

	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(bindReplayReq(4242, "req-bind-ghost"))
	wantFail(t, info, err, model.ErrReplayTaskNotFound, "任务行不存在")
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 1, "任务行不存在：确实查过一次")
	wantField(t, "任务行不存在", "不得留下引用行", int64(countRefs(db)), int64(0))
	wantNoWrites(t, db, before, "任务行不存在")

	// 引用行写成功了，但提交后回读为空：不能把「写成功」当成答复 —— 那是伪造成功。
	// 库里那一行必须还在（回读失败不是回滚），调用方重试时靠同值重放收敛。
	task := registeredReplay(db, nil)
	// 顺序：预读(1) → UPSERT 无冲突 → 提交后回读(2)。missOnce 两次即可只让回读落空。
	db.missOnce("ReplayRefs.FindByReplayID")
	db.missOnce("ReplayRefs.FindByReplayID")
	info, err = NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-noreread"))
	wantFail(t, info, err, model.ErrReplayRefNotFound, "提交后回读为空")
	wantCalls(t, db, "ReplayRefs.UpsertTx", 0, 1, "提交后回读为空：写入发生过")
	wantField(t, "提交后回读为空", "引用行确实已提交", int64(countRefs(db)), int64(1))
	wantField(t, "提交后回读为空", "任务 asset_id 已提交", mustReplay(t, db, task.ReplayId).AssetId, replayAssetID)
	wantEvents(t, db, nil, "绑定失败关闭")
}

// 依赖故障 fail closed：UPSERT 失败与任务行 UPDATE 失败都必须让整笔事务回滚，
// 不能出现「引用行有 asset_id、任务行还是 0」或反之的半产品。
func TestBindReplayAssetRollsBackBothSidesOnDependencyErrors(t *testing.T) {
	// 1) 引用行写失败：任务行一次都不该被写。
	db := newStore()
	svcCtx := newTestSvc(db)
	task := registeredReplay(db, nil)
	was := *task
	db.failOn("ReplayRefs.UpsertTx", errModelDown)
	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-refdown"))
	wantModelDown(t, err)
	if !isNilPtr(info) {
		t.Fatalf("故障路径不得带回响应体：%+v", info)
	}
	wantCalls(t, db, "ReplayTasks.UpdateStateTx", 0, 0, "UPSERT 失败时不得写任务行")
	wantField(t, "UPSERT 失败", "库内无引用行", int64(countRefs(db)), int64(0))
	assertReplayUnchanged(t, db, task.ReplayId, was, "UPSERT 失败")
	wantEvents(t, db, nil, "UPSERT 失败")

	// 2) 任务行 UPDATE 失败：已写入的引用行必须回滚（tried 快照仍在，证明发过）。
	db = newStore()
	svcCtx = newTestSvc(db)
	task = registeredReplay(db, nil)
	was = *task
	db.failOn("ReplayTasks.UpdateStateTx", errModelDown)
	info, err = NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-taskdown"))
	wantModelDown(t, err)
	wantCalls(t, db, "ReplayRefs.UpsertTx", 0, 1, "UPDATE 失败：UPSERT 发过一次")
	wantField(t, "UPDATE 失败", "引用行已回滚", int64(countRefs(db)), int64(0))
	assertReplayUnchanged(t, db, task.ReplayId, was, "UPDATE 失败")
	if len(db.triedReplayRefs) != 1 {
		t.Fatalf("应留下一份「打算写入的引用行」快照，实际 %d 份", len(db.triedReplayRefs))
	}
	tried := db.triedReplayRefs[0]
	wantField(t, "UPDATE 失败", "回滚前那一行的 asset_id", tried.AssetId, replayAssetID)
	wantField(t, "UPDATE 失败", "回滚前那一行的 replay_id", tried.ReplayId, task.ReplayId)
	wantEvents(t, db, nil, "UPDATE 失败")
	wantNoLeak(t, db, "绑定事务回滚")

	// 3) 连事务都开不起来：一次写都不该发生。
	db = newStore()
	svcCtx = newTestSvc(db)
	task = registeredReplay(db, nil)
	db.failOn("DB.TransactCtx", errModelDown)
	info, err = NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-txdown"))
	wantModelDown(t, err)
	wantCalls(t, db, "ReplayRefs.UpsertTx", 0, 0, "事务开不起来时不得写引用行")
	wantField(t, "事务开不起来", "库内无引用行", int64(countRefs(db)), int64(0))
}

// 交错用例：logic 读到 state=REGISTERED 之后、条件 UPDATE 发出之前，
// 投影通道把这条回放推到 COMPLETED。fromStates=[REGISTERED] 必然命中 0 行，
// 归因走 classifyReplayZeroRow（没带 expected_version ⇒ 按「行已在终态」回答），
// 并且引用行随事务回滚 —— 不能留下「引用已绑定、任务已完成」这种没人授权过的组合。
func TestBindReplayAssetLosesCasOnConcurrentlyAdvancedTask(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := registeredReplay(db, nil)
	was := *task
	db.onHit("ReplayTasks.UpdateStateTx", func() {
		live := db.replays[task.ReplayId]
		live.State, live.Version = model.ReplayStateCompleted, live.Version+1
	})

	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-race"))
	wantFail(t, info, err, model.ErrTerminalState, "CAS 输给并发投影")
	wantCalls(t, db, "ReplayTasks.UpdateStateTx", 0, 1, "CAS 输给并发投影：只发过一次条件 UPDATE")
	wantCalls(t, db, "ReplayRefs.UpsertTx", 0, 1, "CAS 输给并发投影：UPSERT 发过一次")
	wantField(t, "CAS 输给并发投影", "引用行已回滚", int64(countRefs(db)), int64(0))
	assertReplayUnchanged(t, db, task.ReplayId, was, "CAS 输给并发投影")
	wantEvents(t, db, nil, "CAS 输给并发投影")
	wantNoLeak(t, db, "CAS 输给并发投影")

	// 并发把行推成非终态（Worker 同时上报 UPLOADING）：0 行仍归因为迁移冲突。
	db = newStore()
	svcCtx = newTestSvc(db)
	task = registeredReplay(db, nil)
	db.onHit("ReplayTasks.UpdateStateTx", func() {
		db.replays[task.ReplayId].State = model.ReplayStateUploading
	})
	info, err = NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-race2"))
	wantFail(t, info, err, model.ErrInvalidTransition, "CAS 输给非终态并发")
	wantField(t, "CAS 输给非终态并发", "引用行已回滚", int64(countRefs(db)), int64(0))
	wantEvents(t, db, nil, "CAS 输给非终态并发")
}

// 预读故障必须原样抛出（fail closed），不能被读成「没有既有引用 → 直接新建」。
// 这条与上一条是一对：前者怕把故障当幂等，后者怕把并发当已提交。
func TestBindReplayAssetFailsClosedOnPreReadErrors(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := registeredReplay(db, nil)
	before := snapshotWrites(db)
	db.failOn("ReplayRefs.FindByReplayID", errModelDown)
	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-refreaddown"))
	wantModelDown(t, err)
	if !isNilPtr(info) {
		t.Fatalf("预读故障不得带回响应体：%+v", info)
	}
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, "引用预读故障时不得继续读任务行")
	wantNoWrites(t, db, before, "引用预读故障")
	wantField(t, "引用预读故障", "库内无引用行", int64(countRefs(db)), int64(0))

	db = newStore()
	svcCtx = newTestSvc(db)
	task = registeredReplay(db, nil)
	before = snapshotWrites(db)
	db.failOn("ReplayTasks.FindOne", errModelDown)
	info, err = NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-bind-taskreaddown"))
	wantModelDown(t, err)
	wantCalls(t, db, "DB.TransactCtx", 0, 0, "任务读故障时不得开事务")
	wantNoWrites(t, db, before, "任务读故障")
	wantField(t, "任务读故障", "库内无引用行", int64(countRefs(db)), int64(0))
}

// ---------------------------------------------------------------- ApplyReplayContentState

// 唯一驱动 COMPLETED 的入口：投影 PUBLISHED + 任务 REVIEW_SUBMITTED。
// 这一条同时钉三件事：五列投影落地、任务联动推进、事件与投影写同事务且只一条。
func TestApplyReplayContentStatePublishDrivesCompletedAndEmitsOneEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateReviewSubmitted })
	ref := refForTask(db, mustReplay(t, db, task.ReplayId), nil)
	wasRef, wasTask := *ref, *mustReplay(t, db, task.ReplayId)
	before := snapshotWrites(db)

	in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_PUBLISHED, "evt-pub-1")
	in.PublishedAt = 1000001
	info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
	info = wantOK(t, info, err, "PUBLISHED 驱动 COMPLETED")

	cur := mustRef(t, db, ref.Id)
	wantRefEchoesRow(t, "PUBLISHED 驱动 COMPLETED", info, cur)
	wantField(t, "PUBLISHED", "review_state", cur.ReviewState, model.ReviewStatePublished)
	if cur.ReviewStateAt <= 0 {
		t.Fatalf("review_state_at 必须是刷新时刻，实际 %d", cur.ReviewStateAt)
	}
	wantField(t, "PUBLISHED", "published_at 取 video 侧发布时刻", cur.PublishedAt, int64(1000001))
	wantField(t, "PUBLISHED", "last_event_id 落库（幂等依据）", cur.LastEventId, "evt-pub-1")
	wantField(t, "PUBLISHED", "source 落库（谁刷的投影）", cur.Source, "content.published.v1")
	wantField(t, "PUBLISHED", "trace_id", cur.TraceId, "trace-replay-projection")
	// 写权限边界：除五列投影与 trace_id/mtime 之外，引用行其余 18 列一列都不许动。
	if stripRefProjection(*cur) != stripRefProjection(wasRef) {
		t.Fatalf("投影通道改动了引用行的非投影列：\n got=%+v\nwant=%+v",
			stripRefProjection(*cur), stripRefProjection(wasRef))
	}

	curTask := mustReplay(t, db, task.ReplayId)
	wantField(t, "PUBLISHED", "任务推进 COMPLETED", curTask.State, model.ReplayStateCompleted)
	wantField(t, "PUBLISHED", "version 递增", curTask.Version, wasTask.Version+1)
	if stripReplayDrive(*curTask) != stripReplayDrive(wasTask) {
		t.Fatalf("投影通道改动了任务行的非状态列：\n got=%+v\nwant=%+v",
			stripReplayDrive(*curTask), stripReplayDrive(wasTask))
	}
	wantField(t, "PUBLISHED", "引用生命周期不被顺手推进", cur.RetentionState, model.RefRetentionStateNormal)
	wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 0, "PUBLISHED 不该碰回收生命周期")

	wantEvents(t, db, []string{"livemedia.replay.content.state.changed"}, "PUBLISHED")
	ev := eventAt(t, db, 0)
	// 事件类型写字面量而不是常量：常量被改名时 Topic 会跟着漂移，而下游订阅的是这个字符串。
	wantField(t, "投影事件", "event_type 与常量一致", ev.EventType, model.EventTypeReplayContentStateChanged)
	wantField(t, "投影事件", "aggregate_type", ev.AggregateType, model.AggregateReplayAssetRef)
	wantField(t, "投影事件", "aggregate_id 是引用行主键", ev.AggregateId, strconv.FormatInt(ref.Id, 10))
	wantField(t, "投影事件", "room_id 冗余列", ev.RoomId, wasRef.RoomId)
	wantField(t, "投影事件", "state 待发布", ev.State, model.OutboxStatePending)
	p := eventPayload(t, ev)
	wantString(t, p, "event_id", "evt-pub-1")
	wantString(t, p, "source", "content.published.v1")
	wantField(t, "投影事件 payload", "prev_review_state 是事务前旧值", toInt64(t, p, "prev_review_state"), int64(model.ReviewStateUnsynced))
	wantField(t, "投影事件 payload", "review_state 是本次新值", toInt64(t, p, "review_state"), int64(model.ReviewStatePublished))
	wantField(t, "投影事件 payload", "replay_id", toInt64(t, p, "replay_id"), task.ReplayId)
	wantField(t, "投影事件 payload", "id 是引用行主键", toInt64(t, p, "id"), ref.Id)
	wantField(t, "投影事件 payload", "record_id", toInt64(t, p, "record_id"), wasRef.RecordId)
	wantField(t, "投影事件 payload", "asset_id", toInt64(t, p, "asset_id"), replayAssetID)
	wantField(t, "投影事件 payload", "aid", toInt64(t, p, "aid"), replayAid)
	wantField(t, "投影事件 payload", "room_id", toInt64(t, p, "room_id"), wasRef.RoomId)
	wantField(t, "投影事件 payload", "live_session_id", toInt64(t, p, "live_session_id"), wasRef.LiveSession)
	wantField(t, "投影事件 payload", "published_at", toInt64(t, p, "published_at"), int64(1000001))
	wantBool(t, p, "task_completed", true)
	wantBool(t, p, "retention_pending", false)
	wantField(t, "PUBLISHED 驱动 COMPLETED", "副作用集合恰好是允许的那四个",
		firedWrites(db, before), "DB.TransactCtx,ReplayTasks.UpdateStateTx,ReplayRefs.ApplyContentStateTx,Outbox.Insert")
	wantNoLeak(t, db, "PUBLISHED 投影")
}

// 投影迁移表逐边钉：合法边落库并发一条事件，非法边失败关闭且零写入。
// 期望值一张一张手写在表里，不是拿 isValidReviewProjectionTransition 自己比自己 ——
// 否则把迁移表整体放开成「任意边」也不会有一条用例变红。
//
// 起跑线故意统一用「引用已在 PUBLISHED/OFFLINE/…、任务已 COMPLETED、生命周期已 Pending」：
// 这样每条用例只有「投影列 + 事件」一处可能变化，被拒时无需再排除联动的干扰。
func TestApplyReplayContentStateProjectionEdges(t *testing.T) {
	cases := []struct {
		name string
		from int32
		to   rpc.ReviewState
		want error // nil 表示合法边
	}{
		{"首次同步：未同步→审核中", model.ReviewStateUnsynced, rpc.ReviewState_REVIEW_STATE_REVIEWING, nil},
		{"首次同步可直接落已发布", model.ReviewStateUnsynced, rpc.ReviewState_REVIEW_STATE_PUBLISHED, nil},
		{"审核中→驳回", model.ReviewStateReviewing, rpc.ReviewState_REVIEW_STATE_REJECTED, nil},
		{"审核中→已发布", model.ReviewStateReviewing, rpc.ReviewState_REVIEW_STATE_PUBLISHED, nil},
		{"审核中→已删除", model.ReviewStateReviewing, rpc.ReviewState_REVIEW_STATE_DELETED, nil},
		{"驳回后重新送审", model.ReviewStateRejected, rpc.ReviewState_REVIEW_STATE_REVIEWING, nil},
		{"驳回不能直接跳已发布", model.ReviewStateRejected, rpc.ReviewState_REVIEW_STATE_PUBLISHED, model.ErrInvalidTransition},
		{"已发布→下架", model.ReviewStatePublished, rpc.ReviewState_REVIEW_STATE_OFFLINE, nil},
		{"已发布不能退回审核中", model.ReviewStatePublished, rpc.ReviewState_REVIEW_STATE_REVIEWING, model.ErrInvalidTransition},
		{"下架后可重新发布", model.ReviewStateOffline, rpc.ReviewState_REVIEW_STATE_PUBLISHED, nil},
		{"已删除是投影终局", model.ReviewStateDeleted, rpc.ReviewState_REVIEW_STATE_PUBLISHED, model.ErrInvalidTransition},
		{"已删除不能退回审核中", model.ReviewStateDeleted, rpc.ReviewState_REVIEW_STATE_REVIEWING, model.ErrInvalidTransition},
		{"同值重放允许刷新时间戳", model.ReviewStatePublished, rpc.ReviewState_REVIEW_STATE_PUBLISHED, nil},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateCompleted })
		ref := refForTask(db, mustReplay(t, db, task.ReplayId), func(r *model.LiveReplayAssetRef) {
			r.ReviewState = tc.from
			r.ReviewStateAt = nowTS() - 50
			r.RetentionState = model.RefRetentionStatePending // 挡掉 Normal→Pending 联动，隔离出投影本身
		})
		wasRef := *ref
		before := snapshotWrites(db)

		in := applyContentReq(task.ReplayId, tc.to, "evt-edge-"+tc.name)
		if tc.to == rpc.ReviewState_REVIEW_STATE_PUBLISHED {
			in.PublishedAt = 1000001
		}
		info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
		if tc.want != nil {
			wantFail(t, info, err, tc.want, tc.name)
			assertRefUnchanged(t, db, ref.Id, wasRef, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
			continue
		}
		info = wantOK(t, info, err, tc.name)
		cur := mustRef(t, db, ref.Id)
		wantRefEchoesRow(t, tc.name, info, cur)
		wantField(t, tc.name, "投影落到本次取值", cur.ReviewState, int32(tc.to))
		wantField(t, tc.name, "last_event_id 更新", cur.LastEventId, "evt-edge-"+tc.name)
		wantField(t, tc.name, "生命周期未被改动", cur.RetentionState, model.RefRetentionStatePending)
		wantField(t, tc.name, "任务状态未被越级推进", mustReplay(t, db, task.ReplayId).State, model.ReplayStateCompleted)
		wantEvents(t, db, []string{model.EventTypeReplayContentStateChanged}, tc.name)
	}
}

// 事件级幂等：last_event_id 已等于本次事件 ⇒ 在读侧早退。
// 一次 UPDATE 都不该发（UPDATE 里同一条件是最终防线，两处都省不掉），
// 更不该重复推进任务或再发一条事件 —— 否则 MQ 重投一次就多一条「已发布」记录。
func TestApplyReplayContentStateSameEventIdReplaysAtReadSide(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateCompleted })
	ref := refForTask(db, mustReplay(t, db, task.ReplayId), func(r *model.LiveReplayAssetRef) {
		r.ReviewState, r.ReviewStateAt = model.ReviewStatePublished, nowTS()-10
		r.PublishedAt, r.LastEventId, r.Source = 1000001, "evt-dup-1", "video.rpc"
	})
	wasRef, wasTask := *ref, *mustReplay(t, db, task.ReplayId)
	before := snapshotWrites(db)

	in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_PUBLISHED, "  evt-dup-1  ") // 带空白：必须与落库值同义
	in.PublishedAt = 1000001
	info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
	info = wantOK(t, info, err, "同事件重放")

	wantRefEchoesRow(t, "同事件重放", info, mustRef(t, db, ref.Id))
	assertRefUnchanged(t, db, ref.Id, wasRef, "同事件重放")
	assertReplayUnchanged(t, db, task.ReplayId, wasTask, "同事件重放")
	// 早退发生在开事务、读任务行之前。
	wantCalls(t, db, "ReplayRefs.ApplyContentStateTx", 0, 0, "同事件重放不得发 UPDATE")
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, "同事件重放不得读任务行")
	wantCalls(t, db, "DB.TransactCtx", 0, 0, "同事件重放不得开事务")
	wantNoWrites(t, db, before, "同事件重放")
	wantEvents(t, db, nil, "同事件重放")
}

// 旧事件迟到：投影写侧命中 0 行（review_state_at 比本次事件更新）。
// 这条必须「既不算失败也不再发事件」，否则 MQ 会对一条已经落地的投影无限退避重投；
// 同时答复的必须是当前投影，而不是本次事件想表达的那个更旧的状态。
func TestApplyReplayContentStateLateEventIsBlockedWithoutError(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateCompleted })
	ref := refForTask(db, mustReplay(t, db, task.ReplayId), func(r *model.LiveReplayAssetRef) {
		r.ReviewState, r.ReviewStateAt = model.ReviewStatePublished, nowTS()+3600 // 未来：新事件已落地
		r.LastEventId, r.Source = "evt-newer", "video.rpc"
	})
	wasRef := *ref

	in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_OFFLINE, "evt-older")
	info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
	info = wantOK(t, info, err, "旧事件被挡")

	cur := mustRef(t, db, ref.Id)
	wantField(t, "旧事件被挡", "投影保持在更新的结论上", cur.ReviewState, model.ReviewStatePublished)
	wantField(t, "旧事件被挡", "last_event_id 未被旧事件覆盖", cur.LastEventId, "evt-newer")
	assertRefUnchanged(t, db, ref.Id, wasRef, "旧事件被挡")
	wantRefEchoesRow(t, "旧事件被挡", info, cur)
	if int32(info.GetReviewState()) != model.ReviewStatePublished {
		t.Fatalf("答复必须是当前投影而不是旧事件的目标态，实际 %v", info.GetReviewState())
	}
	// 事务照常提交（内部零写入），归因走两次回读：classify 一次、提交后一次。
	wantCalls(t, db, "ReplayRefs.ApplyContentStateTx", 0, 1, "旧事件被挡：UPDATE 发过")
	wantCalls(t, db, "DB.TransactCtx", 0, 1, "旧事件被挡：事务提交过")
	wantCalls(t, db, "ReplayRefs.FindOne", 0, 2, "旧事件被挡：归因 + 提交后各回读一次")
	wantEvents(t, db, nil, "旧事件被挡")
	wantNoLeak(t, db, "旧事件被挡")
}

// DELETED 边只做两件事：落投影 + 生命周期 Normal→Pending。
// 它不删对象、不登记回收任务、不推进回放状态 —— 「留证据、由 Worker 真删」是 AGENTS.md §8。
func TestApplyReplayContentStateDeletedMarksPendingAndKeepsEvidence(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateCompleted })
	ref := refForTask(db, mustReplay(t, db, task.ReplayId), func(r *model.LiveReplayAssetRef) {
		r.ReviewState, r.ReviewStateAt = model.ReviewStatePublished, nowTS()-60
		r.PublishedAt, r.LastEventId = 1000001, "evt-pub"
	})
	wasRef := *ref

	in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_DELETED, "evt-del-1")
	info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
	info = wantOK(t, info, err, "DELETED 标记待回收")

	cur := mustRef(t, db, ref.Id)
	wantField(t, "DELETED", "review_state", cur.ReviewState, model.ReviewStateDeleted)
	wantField(t, "DELETED", "生命周期 Normal→Pending", cur.RetentionState, model.RefRetentionStatePending)
	// 审计证据一列不少：产物坐标、时长、区间、已发布时刻全部保留。
	wantField(t, "DELETED 留证据", "bucket", cur.Bucket, wasRef.Bucket)
	wantField(t, "DELETED 留证据", "object_key", cur.ObjectKey, wasRef.ObjectKey)
	wantField(t, "DELETED 留证据", "duration_ms", cur.DurationMs, wasRef.DurationMs)
	wantField(t, "DELETED 留证据", "published_at 不回退", cur.PublishedAt, wasRef.PublishedAt)
	wantField(t, "DELETED 留证据", "replay_id", cur.ReplayId, wasRef.ReplayId)
	wantField(t, "DELETED 留证据", "任务状态不动", mustReplay(t, db, task.ReplayId).State, model.ReplayStateCompleted)

	wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 1, "DELETED 标记待回收")
	wantCalls(t, db, "ReplayTasks.UpdateStateTx", 0, 0, "DELETED 不得推进任务状态")
	// 本方法没有回收执行权：不登记回收任务、不动切片。
	wantCalls(t, db, "RetentionTasks.InsertTx", 0, 0, "DELETED 不登记回收任务")
	wantCalls(t, db, "RetentionTasks.UpdateStateTx", 0, 0, "DELETED 不改回收任务")
	wantCalls(t, db, "Segments.UpdateStateTx", 0, 0, "DELETED 不碰切片")
	wantEvents(t, db, []string{model.EventTypeReplayContentStateChanged}, "DELETED")
	p := eventPayload(t, eventAt(t, db, 0))
	wantBool(t, p, "retention_pending", true)
	wantBool(t, p, "task_completed", false)
	wantField(t, "DELETED 事件 payload", "prev_review_state", toInt64(t, p, "prev_review_state"), int64(model.ReviewStatePublished))
	wantField(t, "DELETED 事件 payload", "review_state", toInt64(t, p, "review_state"), int64(model.ReviewStateDeleted))

	// 生命周期已在 Pending/Reclaimed：MarkRetentionStateTx 命中 0 行也不能报错，
	// 投影仍要落（这是 video 的事实），事件仍要发一条。
	db = newStore()
	svcCtx = newTestSvc(db)
	task = boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateCompleted })
	ref = refForTask(db, mustReplay(t, db, task.ReplayId), func(r *model.LiveReplayAssetRef) {
		r.ReviewState = model.ReviewStateDeleted
		r.RetentionState = model.RefRetentionStatePending
	})
	in = applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_DELETED, "evt-del-2")
	if _, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in); err != nil {
		t.Fatalf("生命周期已推进时 DELETED 重放应成功：%v", err)
	}
	wantField(t, "生命周期已在 Pending", "不倒退", mustRef(t, db, ref.Id).RetentionState, model.RefRetentionStatePending)
	wantField(t, "生命周期已在 Pending", "投影仍落到 DELETED", mustRef(t, db, ref.Id).ReviewState, model.ReviewStateDeleted)
	wantEvents(t, db, []string{model.EventTypeReplayContentStateChanged}, "生命周期已在 Pending")
}

// 定位与交叉校验：replay_id 优先，其次 asset_id；两个都给时媒资必须对得上。
// asset_id=0 是「未登记媒资」的占位值，两个键都为 0 时必须拒绝而不是全表刷新。
func TestApplyReplayContentStateLocatesRefAndCrossChecksAsset(t *testing.T) {
	// 只给 asset_id：走 FindByAssetID，一次都不该碰 FindByReplayID。
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateCompleted })
	ref := refForTask(db, mustReplay(t, db, task.ReplayId), nil)
	info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).
		ApplyReplayContentState(assetScopedReq(ref.AssetId, rpc.ReviewState_REVIEW_STATE_REVIEWING, "evt-by-asset"))
	info = wantOK(t, info, err, "按 asset_id 定位")
	wantRefEchoesRow(t, "按 asset_id 定位", info, mustRef(t, db, ref.Id))
	wantCalls(t, db, "ReplayRefs.FindByAssetID", 0, 1, "按 asset_id 定位")
	wantCalls(t, db, "ReplayRefs.FindByReplayID", 0, 0, "按 asset_id 定位时不得反查 replay_id")
	wantField(t, "按 asset_id 定位", "投影已更新", mustRef(t, db, ref.Id).ReviewState, model.ReviewStateReviewing)

	// 两个键都给但媒资不一致：必须冲突退出，不能挑一个用。
	db = newStore()
	svcCtx = newTestSvc(db)
	task = boundReplay(db, nil)
	ref = refForTask(db, mustReplay(t, db, task.ReplayId), nil)
	wasRef := *ref
	before := snapshotWrites(db)
	both := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_REVIEWING, "evt-mismatch")
	both.AssetId = replayAssetID + 5
	info, err = NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(both)
	wantFail(t, info, err, model.ErrAssetRefConflict, "两个键的媒资不一致")
	assertRefUnchanged(t, db, ref.Id, wasRef, "两个键的媒资不一致")
	wantNoWrites(t, db, before, "两个键的媒资不一致")
	wantEvents(t, db, nil, "两个键的媒资不一致")

	// 两个键都为 0：拒绝，且不查库（0 会把所有未绑定引用都命中）。
	db = newStore()
	svcCtx = newTestSvc(db)
	seedReplayRef(db, nil)
	noneBefore := snapshotWrites(db)
	none := applyContentReq(0, rpc.ReviewState_REVIEW_STATE_REVIEWING, "evt-nokey")
	info, err = NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(none)
	wantFail(t, info, err, model.ErrReplayRefNotFound, "两个键都为 0")
	wantCalls(t, db, "ReplayRefs.FindByReplayID", 0, 0, "两个键都为 0 时不得查库")
	wantCalls(t, db, "ReplayRefs.FindByAssetID", 0, 0, "两个键都为 0 时不得查库")
	wantNoWrites(t, db, noneBefore, "两个键都为 0")
}

// 引用行还不存在（事件早于 BindReplayAsset 到达）必须回 ErrReplayRefNotFound 让 MQ 退避重试：
// 静默丢弃一条 video 事实事件，等于本地投影永久错着。依赖故障同理必须原样抛出。
func TestApplyReplayContentStateMissingRefAndReadErrorsFailClosed(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := seedReplay(db, model.ReplayStateReviewSubmitted, nil) // 有任务、无引用行
	before := snapshotWrites(db)

	in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_REVIEWING, "evt-early")
	info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
	wantFail(t, info, err, model.ErrReplayRefNotFound, "事件早于绑定")
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, "引用行都不存在时不得读任务行")
	wantNoWrites(t, db, before, "事件早于绑定")
	wantEvents(t, db, nil, "事件早于绑定")

	// 查无此行（nil,nil）与读故障：两者都不能被读成「成功但没改动」。
	db = newStore()
	svcCtx = newTestSvc(db)
	task = boundReplay(db, nil)
	refForTask(db, mustReplay(t, db, task.ReplayId), nil)
	db.missOnce("ReplayRefs.FindByReplayID")
	info, err = NewApplyReplayContentStateLogic(context.Background(), svcCtx).
		ApplyReplayContentState(applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_REVIEWING, "evt-miss"))
	wantFail(t, info, err, model.ErrReplayRefNotFound, "引用行查无")

	db = newStore()
	svcCtx = newTestSvc(db)
	task = boundReplay(db, nil)
	refForTask(db, mustReplay(t, db, task.ReplayId), nil)
	before = snapshotWrites(db)
	db.failOn("ReplayRefs.FindByReplayID", errModelDown)
	info, err = NewApplyReplayContentStateLogic(context.Background(), svcCtx).
		ApplyReplayContentState(applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_REVIEWING, "evt-down"))
	wantModelDown(t, err)
	wantNoWrites(t, db, before, "引用预读故障")

	// 提交后回读失败：写已经提交了，但不能因此回成功。
	db = newStore()
	svcCtx = newTestSvc(db)
	task = boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateReviewSubmitted })
	ref := refForTask(db, mustReplay(t, db, task.ReplayId), nil)
	db.failOn("ReplayRefs.FindOne", errModelDown)
	info, err = NewApplyReplayContentStateLogic(context.Background(), svcCtx).
		ApplyReplayContentState(applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_REVIEWING, "evt-postreread"))
	wantModelDown(t, err)
	cur := mustRef(t, db, ref.Id)
	wantField(t, "回读故障", "投影其实已提交", cur.ReviewState, model.ReviewStateReviewing)
	wantEvents(t, db, []string{model.EventTypeReplayContentStateChanged}, "回读故障：事件与投影同事务")
}

// 入参门禁必须在定位引用行之前完成，取值与迁移语义分开：
// 投影取值本身非法（0/6/-1）是 ErrInvalidReviewState，
// 取值合法但来源不在白名单里是 ErrInvalidTransition（无法归因的投影一律拒绝）。
func TestApplyReplayContentStateValidatesArgumentsBeforeLocating(t *testing.T) {
	longEvent := strings.Repeat("e", maxEventIDRunes+1)
	cases := []struct {
		name   string
		mutate func(*rpc.ApplyReplayContentStateReq)
		want   error
	}{
		{"投影取值 UNSPECIFIED", func(in *rpc.ApplyReplayContentStateReq) { in.ReviewState = rpc.ReviewState_REVIEW_STATE_UNSPECIFIED }, model.ErrInvalidReviewState},
		{"投影取值越上界", func(in *rpc.ApplyReplayContentStateReq) { in.ReviewState = rpc.ReviewState(6) }, model.ErrInvalidReviewState},
		{"投影取值为负", func(in *rpc.ApplyReplayContentStateReq) { in.ReviewState = rpc.ReviewState(-1) }, model.ErrInvalidReviewState},
		{"event_id 为空", func(in *rpc.ApplyReplayContentStateReq) { in.EventId = "  " }, model.ErrEmptyEventID},
		{"event_id 超长", func(in *rpc.ApplyReplayContentStateReq) { in.EventId = longEvent }, model.ErrEmptyEventID},
		{"published_at 为负", func(in *rpc.ApplyReplayContentStateReq) { in.PublishedAt = -1 }, model.ErrInvalidReviewState},
		{"已发布但无发布时刻", func(in *rpc.ApplyReplayContentStateReq) {
			in.ReviewState, in.PublishedAt = rpc.ReviewState_REVIEW_STATE_PUBLISHED, 0
		}, model.ErrInvalidReviewState},
		{"来源不在白名单", func(in *rpc.ApplyReplayContentStateReq) { in.Source = "cron-replay" }, model.ErrInvalidTransition},
		{"replay_id 为负", func(in *rpc.ApplyReplayContentStateReq) { in.ReplayId = -1 }, model.ErrReplayRefNotFound},
		{"asset_id 为负", func(in *rpc.ApplyReplayContentStateReq) { in.AssetId = -2 }, model.ErrReplayRefNotFound},
	}
	for _, tc := range cases {
		db := newStore()
		svcCtx := newTestSvc(db)
		task := boundReplay(db, nil)
		ref := refForTask(db, mustReplay(t, db, task.ReplayId), nil)
		wasRef := *ref
		before := snapshotWrites(db)

		in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_REVIEWING, "evt-valid")
		tc.mutate(in)
		info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
		wantFail(t, info, err, tc.want, tc.name)
		wantCalls(t, db, "ReplayRefs.FindByReplayID", 0, 0, tc.name+"：不得定位引用行")
		wantCalls(t, db, "ReplayRefs.FindByAssetID", 0, 0, tc.name+"：不得定位引用行")
		assertRefUnchanged(t, db, ref.Id, wasRef, tc.name)
		wantNoWrites(t, db, before, tc.name)
		wantEvents(t, db, nil, tc.name)
	}
}

// 业务写与 Outbox 同生共死：事件写失败时投影与任务联动一起回滚。
// 否则「已发布」的投影配一条不存在的事件，下游永远等不到投递。
func TestApplyReplayContentStateRollsBackWhenOutboxInsertFails(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateReviewSubmitted })
	ref := refForTask(db, mustReplay(t, db, task.ReplayId), nil)
	wasRef, wasTask := *ref, *mustReplay(t, db, task.ReplayId)
	db.failOn("Outbox.Insert", errOutboxDown)

	in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_PUBLISHED, "evt-rollback")
	in.PublishedAt = 1000001
	info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
	if err == nil || !strings.Contains(err.Error(), errOutboxDown.Error()) {
		t.Fatalf("Outbox 故障必须原样上抛，实际 %v", err)
	}
	if !isNilPtr(info) {
		t.Fatalf("Outbox 故障时不得带回响应体：%+v", info)
	}
	assertRefUnchanged(t, db, ref.Id, wasRef, "Outbox 故障回滚")
	assertReplayUnchanged(t, db, task.ReplayId, wasTask, "Outbox 故障回滚")
	// 计数不参与回滚：这里断言「三条写都发过、事件一条没落」，比只看错误码更硬。
	wantCalls(t, db, "ReplayRefs.ApplyContentStateTx", 0, 1, "Outbox 故障回滚：投影 UPDATE 发过")
	wantCalls(t, db, "ReplayTasks.UpdateStateTx", 0, 1, "Outbox 故障回滚：任务 UPDATE 发过")
	wantCalls(t, db, "Outbox.Insert", 0, 1, "Outbox 故障回滚：INSERT 发过")
	wantEvents(t, db, nil, "Outbox 故障回滚")
	if len(db.outbox) != 0 {
		t.Fatalf("Outbox 故障后不得留下事件行，实际 %d 条", len(db.outbox))
	}
	wantNoLeak(t, db, "Outbox 故障回滚")
}

// 交错用例：投影 UPDATE 已落地、任务 UPDATE 发出之前，Worker 把这条回放判成 FAILED。
// 契约是「不复活、不改判，但本次投影仍是既成事实，事务不回滚」：
// 于是并发方的结论保留、投影照样落、事件照发、调用方拿到成功。
func TestApplyReplayContentStateCompletedDriveLosesRaceButProjectionKept(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateReviewSubmitted })
	ref := refForTask(db, mustReplay(t, db, task.ReplayId), nil)
	db.onHit("ReplayTasks.UpdateStateTx", func() {
		live := db.replays[task.ReplayId]
		live.State, live.Version = model.ReplayStateFailed, live.Version+1
	})

	in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_PUBLISHED, "evt-race-pub")
	in.PublishedAt = 1000001
	info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
	info = wantOK(t, info, err, "COMPLETED 驱动撞车")

	curTask := mustReplay(t, db, task.ReplayId)
	wantField(t, "驱动撞车", "并发方的 FAILED 保留（不复活）", curTask.State, model.ReplayStateFailed)
	wantField(t, "驱动撞车", "未被本方法改判", curTask.State, model.ReplayStateFailed)
	cur := mustRef(t, db, ref.Id)
	wantField(t, "驱动撞车", "投影照样落地", cur.ReviewState, model.ReviewStatePublished)
	wantField(t, "驱动撞车", "last_event_id 落地", cur.LastEventId, "evt-race-pub")
	wantEvents(t, db, []string{model.EventTypeReplayContentStateChanged}, "驱动撞车")
	p := eventPayload(t, eventAt(t, db, 0))
	// 事件说的是「驱动意图」而不是「驱动结果」：下游据此判断要不要动，本服务不替它猜。
	wantBool(t, p, "task_completed", true)
	wantRefEchoesRow(t, "驱动撞车", info, cur)
	wantNoLeak(t, db, "驱动撞车")
}

// video 已发布但本地任务还没走到 REVIEW_SUBMITTED：投影照样落（它是事实），
// 任务态绝不越级推进 —— 合法边只有 REVIEW_SUBMITTED→COMPLETED。
func TestApplyReplayContentStatePublishedDoesNotSkipTaskStates(t *testing.T) {
	for _, st := range []int32{model.ReplayStateUploading, model.ReplayStateRegistered, model.ReplayStateFailed} {
		label := "task state=" + rpc.ReplayState(st).String()
		db := newStore()
		svcCtx := newTestSvc(db)
		task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = st })
		was := *task
		ref := refForTask(db, mustReplay(t, db, task.ReplayId), nil)

		in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_PUBLISHED, "evt-noskip")
		in.PublishedAt = 1000001
		info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in)
		info = wantOK(t, info, err, label)

		wantField(t, label, "投影落地", mustRef(t, db, ref.Id).ReviewState, model.ReviewStatePublished)
		assertReplayUnchanged(t, db, task.ReplayId, was, label+"：任务态不越级")
		wantCalls(t, db, "ReplayTasks.UpdateStateTx", 0, 0, label+"：一条任务 UPDATE 都不该发")
		wantEvents(t, db, []string{model.EventTypeReplayContentStateChanged}, label)
		p := eventPayload(t, eventAt(t, db, 0))
		wantBool(t, p, "task_completed", false)
	}
}

// manual 来源是「人工刷新」这条后门：允许，但必须留下可归因的落库事实（source='manual'），
// 且它不得成为绕过迁移表的路径 —— 已删除的投影依然不能被人工复活。
func TestApplyReplayContentStateManualSourceIsAttributableButNotALabelLoophole(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := boundReplay(db, func(r *model.LiveReplayTask) { r.State = model.ReplayStateCompleted })
	ref := refForTask(db, mustReplay(t, db, task.ReplayId), nil)

	in := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_REVIEWING, "evt-manual-1")
	in.Source = "  manual  " // 归一后必须与落库值同源
	if _, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(in); err != nil {
		t.Fatalf("manual 来源应被接受：%v", err)
	}
	cur := mustRef(t, db, ref.Id)
	wantField(t, "人工刷新", "source 归一后落库", cur.Source, "manual")
	wantField(t, "人工刷新", "last_event_id", cur.LastEventId, "evt-manual-1")
	p := eventPayload(t, eventAt(t, db, 0))
	wantString(t, p, "source", "manual")
	wantField(t, "人工刷新", "事件条数", int64(len(db.outbox)), int64(1))

	// 走到已删除终态：manual 也救不回来，且一行都不许改。
	for _, tc := range []struct {
		event string
		state rpc.ReviewState
	}{
		{"evt-manual-2", rpc.ReviewState_REVIEW_STATE_PUBLISHED},
		{"evt-manual-3", rpc.ReviewState_REVIEW_STATE_DELETED},
	} {
		req := applyContentReq(task.ReplayId, tc.state, tc.event)
		req.Source = "manual"
		if tc.state == rpc.ReviewState_REVIEW_STATE_PUBLISHED {
			req.PublishedAt = 1000002
		}
		if _, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).
			ApplyReplayContentState(req); err != nil {
			t.Fatalf("%s→%v 应合法：%v", tc.event, tc.state, err)
		}
	}
	wasDeleted := *mustRef(t, db, ref.Id)
	wantField(t, "已删除终态", "review_state", wasDeleted.ReviewState, model.ReviewStateDeleted)
	before := snapshotWrites(db)
	revive := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_PUBLISHED, "evt-manual-4")
	revive.Source = "manual"
	revive.PublishedAt = 1000003
	info, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).ApplyReplayContentState(revive)
	wantFail(t, info, err, model.ErrInvalidTransition, "manual 不得复活已删投影")
	assertRefUnchanged(t, db, ref.Id, wasDeleted, "manual 不得复活已删投影")
	wantNoWrites(t, db, before, "manual 不得复活已删投影")
	// wantEvents 比的是整条 outbox：manual-1/-2/-3 各一条，被拒的 manual-4 不得追加第 4 条。
	wantEvents(t, db, []string{model.EventTypeReplayContentStateChanged, model.EventTypeReplayContentStateChanged,
		model.EventTypeReplayContentStateChanged}, "被拒的那一步不得追加事件")
}

// ---------------------------------------------------------------- 下游 nil client

// 回放/归档七个方法都不得越进程去问 video/asset/moderation：
// 本包 ServiceContext 的三个下游 client 恒为 nil（README §8.2「当前没有任何 logic 调用它们」），
// 真调一次就是 panic。这条用例同时钉住「需要下游事实才能成立的动作一律显式失败」——
// 绑定要 asset_id/aid 由调用方给、投影要结论由 video 推，本服务自己不猜。
func TestReplayMethodsNeverTouchNilClients(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	if svcCtx.Asset != nil || svcCtx.Video != nil || svcCtx.Moderation != nil {
		t.Fatalf("本用例的前提是三个下游 client 恒为 nil：asset=%v video=%v moderation=%v",
			svcCtx.Asset != nil, svcCtx.Video != nil, svcCtx.Moderation != nil)
	}

	record := stoppedRecord(db, nil)
	seedVerifiedRun(db, record.RecordId, 5, nil)
	if _, err := NewSubmitReplayTaskLogic(context.Background(), svcCtx).
		SubmitReplayTask(submitReplayReq(record.RecordId, "req-nil-1")); err != nil {
		t.Fatalf("SubmitReplayTask 不该依赖下游：%v", err)
	}
	task := boundReplay(db, nil)
	if _, err := NewReportReplayProgressLogic(context.Background(), svcCtx).
		ReportReplayProgress(reportReplayReq(task.ReplayId, rpc.ReplayState_REPLAY_STATE_REVIEW_SUBMITTED)); err != nil {
		t.Fatalf("ReportReplayProgress 不该依赖下游：%v", err)
	}
	if _, err := NewBindReplayAssetLogic(context.Background(), svcCtx).
		BindReplayAsset(bindReplayReq(task.ReplayId, "req-nil-2")); err != nil {
		t.Fatalf("BindReplayAsset 不该依赖下游：%v", err)
	}
	// 送审后的任务已在 REVIEW_SUBMITTED，引用行也已绑定：投影通道照常工作。
	refForTask(db, mustReplay(t, db, task.ReplayId), nil)
	pub := applyContentReq(task.ReplayId, rpc.ReviewState_REVIEW_STATE_PUBLISHED, "evt-nil")
	pub.PublishedAt = 1000001
	if _, err := NewApplyReplayContentStateLogic(context.Background(), svcCtx).
		ApplyReplayContentState(pub); err != nil {
		t.Fatalf("ApplyReplayContentState 不该依赖下游：%v", err)
	}
	if _, err := NewGetReplayTaskLogic(context.Background(), svcCtx).GetReplayTask(getReplayReq(task.ReplayId)); err != nil {
		t.Fatalf("GetReplayTask 不该依赖下游：%v", err)
	}
	if _, err := NewListReplayTasksLogic(context.Background(), svcCtx).
		ListReplayTasks(listReplaysReq(1, 10)); err != nil {
		t.Fatalf("ListReplayTasks 不该依赖下游：%v", err)
	}
	if _, err := NewListReplayAssetRefsLogic(context.Background(), svcCtx).
		ListReplayAssetRefs(listRefsReq(1, 10)); err != nil {
		t.Fatalf("ListReplayAssetRefs 不该依赖下游：%v", err)
	}
	// 唯一「需要下游事实」的地方是拿到 asset_id/aid：缺它必须显式失败，而不是伪造一个去过门禁。
	unbound := registeredReplay(db, func(r *model.LiveReplayTask) { r.AssetId, r.Aid = 0, 0 })
	in := bindReplayReq(unbound.ReplayId, "req-nil-3")
	in.AssetId, in.Aid = 0, 0
	info, err := NewBindReplayAssetLogic(context.Background(), svcCtx).BindReplayAsset(in)
	wantFail(t, info, err, model.ErrInvalidAssetID, "缺媒资不得伪造 asset_id 去过门禁")
	wantNoLeak(t, db, "nil 下游")
}
