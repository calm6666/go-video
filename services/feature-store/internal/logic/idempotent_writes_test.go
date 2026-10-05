// 测试域 2：幂等写入与 CAS 冲突检测。
//
// README 的三条承诺在这里可检查：
//  1. 同一 request_id 重放只回放首次结果，绝不重做副作用（不重复写、不重复删、不重复计数）；
//  2. 同一 request_id 换内容必须是 ErrRequestIdReused —— 幂等键不能当「随便重发」的通行证；
//  3. 同一自然键 (feature_key, version, entity_scope, entity_id) 的重复写必须收敛到一行。
//
// 并发版本变更（CAS）失败时事务整体回滚：审计与指针不能只有一半。
package logic

import (
	"errors"
	"testing"

	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"
)

// --- 造写入入参 ---

func i64Write(key string, version int32, entityID string, val, eventTime int64) *rpc.FeatureWrite {
	return &rpc.FeatureWrite{
		Feature: &rpc.FeatureRef{FeatureKey: key, Version: version},
		Entity:  &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID, EntityId: entityID},
		Value: &rpc.FeatureValue{
			ValueType:  rpc.FeatureValueType_FEATURE_VALUE_TYPE_INT64,
			Int64Value: val,
		},
		EventTime:       eventTime,
		SourceMetricKey: testSourceMetricKey,
	}
}

func writeReq(requestID, operator string, ws ...*rpc.FeatureWrite) *rpc.WriteFeaturesReq {
	return &rpc.WriteFeaturesReq{
		Writes:    ws,
		Writer:    rpc.FeatureSource_FEATURE_SOURCE_SPM_METRIC,
		RequestId: requestID,
		Operator:  operator,
	}
}

func callWrite(f *fixture, in *rpc.WriteFeaturesReq) (*rpc.WriteFeaturesReply, error) {
	return NewWriteFeaturesLogic(f.ctx, f.ServiceContext).WriteFeatures(in)
}

func rowResult(results []*rpc.WriteFeaturesReply_RowResult, i int) (bool, string) {
	if i >= len(results) {
		return false, "<missing>"
	}
	return results[i].GetOk(), results[i].GetError()
}

// --- 写路径幂等 ---

func TestWriteReplayDoesNotWriteTwice(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	req := writeReq("req-replay", "system:spm", i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60))

	first, err := callWrite(f, req)
	if err != nil {
		t.Fatalf("first WriteFeatures: %v", err)
	}
	if first.GetWritten() != 1 || first.GetReused() {
		t.Fatalf("first reply = written %d reused %v, want 1/false", first.GetWritten(), first.GetReused())
	}

	second, err := callWrite(f, req)
	if err != nil {
		t.Fatalf("replayed WriteFeatures: %v", err)
	}
	if !second.GetReused() {
		t.Error("reused=false for a replayed request_id: the caller cannot tell a replay from a real write")
	}
	if second.GetWritten() != 1 {
		t.Errorf("replayed written=%d, want the first run's 1", second.GetWritten())
	}
	if n := countCalled(f.values.calls, "values.BatchUpsert"); n != 1 {
		t.Errorf("BatchUpsert calls=%d, want exactly 1: a replay must not redo the write", n)
	}
	if n := countCalled(f.receipts.calls, "receipts.MarkDone"); n != 1 {
		t.Errorf("MarkDone calls=%d, want 1", n)
	}
	if f.txBegins() != 1 {
		t.Errorf("transactions=%d, want 1 (the replay must not open a second write tx)", f.txBegins())
	}
}

func TestWriteReplayReturnsFirstRunPerRowResults(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	// 一批里混一条未注册的 key：首次就有一行被拒。
	req := writeReq("req-partial", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60),
		i64Write("user_never_registered_7d", 1, testMid, 7, testNow-60))

	first, err := callWrite(f, req)
	if err != nil {
		t.Fatalf("WriteFeatures: %v", err)
	}
	if first.GetWritten() != 1 || first.GetRejected() != 1 {
		t.Fatalf("first reply = written %d rejected %d, want 1/1", first.GetWritten(), first.GetRejected())
	}
	if ok, code := rowResult(first.GetResults(), 1); ok || code != "FEATURE_NOT_FOUND" {
		t.Fatalf("row 1 = ok %v code %q, want the rejection recorded", ok, code)
	}

	replay, err := callWrite(f, req)
	if err != nil {
		t.Fatalf("replayed WriteFeatures: %v", err)
	}
	if replay.GetRejected() != 1 {
		t.Errorf("replayed rejected=%d, want the first run's 1", replay.GetRejected())
	}
	if len(replay.GetResults()) != 2 {
		t.Fatalf("replay lost the per-row detail: %d results", len(replay.GetResults()))
	}
	if ok, code := rowResult(replay.GetResults(), 1); ok || code != "FEATURE_NOT_FOUND" {
		t.Errorf("replayed row 1 = ok %v code %q, want it identical to the first run", ok, code)
	}
	if replay.GetResults()[0].GetFeatureKey() != "user_play_finish_7d" {
		t.Error("replayed rows lost their ordering/identity")
	}
}

func TestWriteSameRequestIDDifferentBodyIsRejected(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	if _, err := callWrite(f, writeReq("req-reuse", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60))); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// 幂等摘要覆盖「op + 目标 + 行数 + 操作人」：换一批行就是另一件事。
	_, err := callWrite(f, writeReq("req-reuse", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 999, testNow-60),
		i64Write("user_play_finish_7d", 1, "10087", 998, testNow-60)))
	if !errors.Is(err, model.ErrRequestIdReused) {
		t.Fatalf("err=%v, want %v: request_id is not a re-send licence", err, model.ErrRequestIdReused)
	}
	// 冲突的那一次不能已经把新值写进去。
	row, ok := f.valueRow(model.ValueKey{FeatureKey: "user_play_finish_7d", Version: 1,
		EntityScope: model.EntityScopeMid, EntityID: testMid})
	if !ok || row.Int64Value != 100 {
		t.Errorf("stored row = %+v, want the first write's 100 untouched", row)
	}
}

func TestRepeatedWritesConvergeOnTheNaturalKey(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	body := i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60)

	for _, id := range []string{"req-a", "req-b", "req-c"} {
		reply, err := callWrite(f, writeReq(id, "system:spm", body))
		if err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		if reply.GetWritten() != 1 {
			t.Errorf("write %s reported written=%d", id, reply.GetWritten())
		}
	}
	key := model.ValueKey{FeatureKey: "user_play_finish_7d", Version: 1,
		EntityScope: model.EntityScopeMid, EntityID: testMid}
	row, ok := f.valueRow(key)
	if !ok {
		t.Fatal("the natural key is missing")
	}
	if n := len(f.values.rows); n != 1 {
		t.Errorf("feature_value rows=%d, want exactly 1: three identical writes must converge", n)
	}
	if row.ValueID == 0 {
		t.Error("the converged row lost its primary key (an update must not re-insert)")
	}
}

func TestLateOlderEventTimeCannotRollBackTheCurrentValue(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	if _, err := callWrite(f, writeReq("req-new", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 100, testNow-10))); err != nil {
		t.Fatalf("fresh write: %v", err)
	}

	reply, err := callWrite(f, writeReq("req-late", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 4242, testNow-7200)))
	if err != nil {
		t.Fatalf("late write: %v", err)
	}
	if ok, code := rowResult(reply.GetResults(), 0); ok || code != "STALE_EVENT_TIME" {
		t.Errorf("late row = ok %v code %q, want it rejected with STALE_EVENT_TIME", ok, code)
	}
	row, _ := f.valueRow(model.ValueKey{FeatureKey: "user_play_finish_7d", Version: 1,
		EntityScope: model.EntityScopeMid, EntityID: testMid})
	if row.Int64Value != 100 {
		t.Errorf("stored value=%d: one late-arriving batch rolled the live value back to history", row.Int64Value)
	}
}

func TestDuplicateKeyInsideOneBatchMarksTheEarlierRow(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	reply, err := callWrite(f, writeReq("req-dup", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 111, testNow-60),
		i64Write("user_play_finish_7d", 1, testMid, 222, testNow-60)))
	if err != nil {
		t.Fatalf("WriteFeatures: %v", err)
	}
	if ok, code := rowResult(reply.GetResults(), 0); ok || code != "DUPLICATE_ROW_IN_BATCH" {
		t.Errorf("earlier row = ok %v code %q, want DUPLICATE_ROW_IN_BATCH: an upstream that double-computes must see it", ok, code)
	}
	if ok, _ := rowResult(reply.GetResults(), 1); !ok {
		t.Error("the later row of the same key must win and be reported OK")
	}
	if reply.GetWritten() != 1 || reply.GetRejected() != 1 {
		t.Errorf("written/rejected = %d/%d, want 1/1", reply.GetWritten(), reply.GetRejected())
	}
}

func TestRowResultsAlignWithRequestsAndBadRowsDoNotBreakTheBatch(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	reply, err := callWrite(f, writeReq("req-mixed", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 1, testNow-60),
		i64Write("user_play_finish_7d", 0, testMid, 2, testNow-60), // version 0 且无指针 -> 解析后成功
		&rpc.FeatureWrite{
			Feature: &rpc.FeatureRef{FeatureKey: "user_play_finish_7d", Version: 1},
			// 明文设备号当 MID：隐私闸门必须把它挡在 SQL 之前
			Entity: &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
				EntityId: plaintextDeviceSerial},
			Value:           &rpc.FeatureValue{ValueType: rpc.FeatureValueType_FEATURE_VALUE_TYPE_INT64},
			SourceMetricKey: testSourceMetricKey,
		},
	))
	if err != nil {
		t.Fatalf("WriteFeatures: %v", err)
	}
	if len(reply.GetResults()) != 3 {
		t.Fatalf("results=%d, want one per requested row in order", len(reply.GetResults()))
	}
	if ok, code := rowResult(reply.GetResults(), 2); ok || code != "ENTITY_ID_INVALID" {
		t.Errorf("plaintext identifier row = ok %v code %q, want ENTITY_ID_INVALID", ok, code)
	}
	if reply.GetWritten()+reply.GetRejected() != 3 {
		t.Errorf("written %d + rejected %d != 3", reply.GetWritten(), reply.GetRejected())
	}
	for k := range f.values.rows {
		if k.EntityID == plaintextDeviceSerial {
			t.Fatal("a plaintext identifier reached feature_value")
		}
	}
}

func TestWriteTransactionFailureRollsBackEveryRow(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("user_play_finish_7d", 1))
	f.putInt64(d, 1, "10087", 5, testNow-60) // 已有的别的主体行，必须活着
	f.values.errBatchUpsert = errors.New("deadlock found")

	_, err := callWrite(f, writeReq("req-deadlock", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60)))
	if err == nil {
		t.Fatal("a failing batch must surface as an error")
	}
	if len(f.values.rows) != 1 {
		t.Fatalf("feature_value rows=%d: the failed transaction left partial writes", len(f.values.rows))
	}
	if _, ok := f.valueRow(model.ValueKey{FeatureKey: "user_play_finish_7d", Version: 1,
		EntityScope: model.EntityScopeMid, EntityID: "10087"}); !ok {
		t.Error("the rollback also destroyed an unrelated pre-existing row")
	}
	if begins, commits, rollbacks := f.txBegins(), f.txCommits(), f.txRollbacks(); begins != 1 || commits != 0 || rollbacks != 1 {
		t.Errorf("boundaries begin/commit/rollback = %d/%d/%d, want 1/0/1", begins, commits, rollbacks)
	}
	// 失败必须落 MarkFailed，否则同一 request_id 会被后来的重试挡成「正在进行」。
	if len(f.receipts.failed) != 1 {
		t.Fatalf("receipt failed marks=%d, want 1: a failure must release the execution right", len(f.receipts.failed))
	}
}

func TestWriteCommitStageFailureLeavesNoHalfWrittenRows(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.db.failCommit = true // 回调全部成功、提交阶段才断连：最容易留下「半批」的窗口

	_, err := callWrite(f, writeReq("req-commit", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60),
		i64Write("user_play_finish_7d", 1, "10087", 101, testNow-60)))
	if err == nil {
		t.Fatal("commit failure must be reported")
	}
	if n := len(f.values.rows); n != 0 {
		t.Errorf("feature_value rows=%d after a failed commit, want 0", n)
	}
	if f.txCommits() != 0 || f.txRollbacks() != 1 {
		t.Errorf("commit/rollback = %d/%d, want 0/1", f.txCommits(), f.txRollbacks())
	}
}

func TestInFlightReceiptBlocksASecondExecutionUntilLeaseExpires(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	req := writeReq("req-inflight", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60))
	f.receipts.errMarkDone = errors.New("receipt write failed") // 收尾失败：执行权留在 in_progress

	if _, err := callWrite(f, req); err == nil {
		t.Fatal("a failed MarkDone must not be reported as success")
	}
	f.receipts.errMarkDone = nil

	// 首次执行还没收尾就重发：第二条只能被告知「正在进行」，不能二次写。
	_, err := callWrite(f, writeReq("req-inflight", "system:spm", req.GetWrites()...))
	if !errors.Is(err, model.ErrReceiptInProgress) {
		t.Fatalf("err=%v, want %v", err, model.ErrReceiptInProgress)
	}
	if n := countCalled(f.values.calls, "values.BatchUpsert"); n != 1 {
		t.Errorf("BatchUpsert calls=%d, want 1", n)
	}

	// 租约过期后同一 request_id 可以接管，并且仍然只收敛到一行。
	f.advance(f.Config.Write.ReceiptLeaseSeconds + 1)
	reply, err := callWrite(f, writeReq("req-inflight", "system:spm", req.GetWrites()...))
	if err != nil {
		t.Fatalf("takeover after lease expiry: %v", err)
	}
	if reply.GetWritten() != 1 {
		t.Errorf("takeover wrote %d rows, want 1", reply.GetWritten())
	}
	if n := len(f.values.rows); n != 1 {
		t.Errorf("feature_value rows=%d, want 1 after the takeover re-applied the same natural key", n)
	}
}

func TestWriteReceiptRecordsOnlyCountsAndShortCodes(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	if _, err := callWrite(f, writeReq("req-snapshot", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60))); err != nil {
		t.Fatalf("WriteFeatures: %v", err)
	}
	row, ok := f.receiptRow("req-snapshot", model.ReceiptOpWrite)
	if !ok {
		t.Fatal("the write receipt is missing")
	}
	if row.State != model.ReceiptStateDone {
		t.Errorf("receipt state=%q, want done", row.State)
	}
	if row.RequestDigest == "" || row.ResultDigest == "" {
		t.Errorf("receipt digests empty: digests=%q result=%q", row.RequestDigest, row.ResultDigest)
	}
	if !row.Replayable() {
		t.Errorf("a small result set must stay replayable, kept=%d", row.DetailKept)
	}
}

// --- 写入状态闸门（打真正的入口判断）---

// TestWriteFeaturesRejectsDraftAndRetiredTargetsAtTheEntry 守住 model 侧闸门收窄后的另一半：
// 「外部写入只接受 ACTIVE」这条规则现在只存在于 WriteFeatures 入口一处
// （internal/logic/writefeatureslogic.go checkRow）。
// 行构造入口 NewFeatureValue 允许 DRAFT 是给回填/上线前铺值这条内部路径用的
// （见 model/featurevalue_writegate_test.go），对外部调用方一寸都没有放开：
// DRAFT 目标行报 FEATURE_NOT_ACTIVE、RETIRED 目标行报 FEATURE_RETIRED，
// 两条都不许碰到 values.BatchUpsert。
func TestWriteFeaturesRejectsDraftAndRetiredTargetsAtTheEntry(t *testing.T) {
	f := newFixture(t)
	f.putDef(newDef("user_play_finish_7d", 1, withState(model.FeatureStateDraft)))
	f.putDef(newDef("user_hot_score_1h", 1, withState(model.FeatureStateRetired)))

	reply, err := callWrite(f, writeReq("req-state-gate", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60),
		i64Write("user_hot_score_1h", 1, testMid, 100, testNow-60)))
	if err != nil {
		t.Fatalf("WriteFeatures: %v", err)
	}
	if reply.GetWritten() != 0 || reply.GetRejected() != 2 {
		t.Errorf("written/rejected = %d/%d, want 0/2", reply.GetWritten(), reply.GetRejected())
	}
	if ok, code := rowResult(reply.GetResults(), 0); ok || code != "FEATURE_NOT_ACTIVE" {
		t.Errorf("DRAFT row = ok %v code %q, want FEATURE_NOT_ACTIVE", ok, code)
	}
	if ok, code := rowResult(reply.GetResults(), 1); ok || code != "FEATURE_RETIRED" {
		t.Errorf("RETIRED row = ok %v code %q, want FEATURE_RETIRED", ok, code)
	}
	if n := countCalled(f.values.calls, "values.BatchUpsert"); n != 0 {
		t.Errorf("BatchUpsert calls=%d, want 0: a rejected target must not reach SQL", n)
	}
	if n := len(f.values.rows); n != 0 {
		t.Errorf("feature_value rows=%d, want 0: nothing may be stored for a non-ACTIVE target", n)
	}
}

func TestWriteWithoutIdempotencyKeyIsRejected(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	_, err := callWrite(f, writeReq("   ", "system:spm",
		i64Write("user_play_finish_7d", 1, testMid, 100, testNow-60)))
	if !errors.Is(err, model.ErrRequestIdRequired) {
		t.Fatalf("err=%v, want %v", err, model.ErrRequestIdRequired)
	}
	if n := countCalled(f.receipts.calls, "receipts.Begin"); n != 0 {
		t.Errorf("receipts.Begin calls=%d: an unkeyed write must not even take an execution right", n)
	}
	if f.txBegins() != 0 {
		t.Error("an unkeyed write reached the database")
	}
}
