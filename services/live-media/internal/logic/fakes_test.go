package logic

// fakes_test.go 提供本包 logic 单测用的「内存版 model」与「假事务」。
//
// 为什么可以这样注入（以及为什么这不算伪装通过）：
//   - svc.ServiceContext 的八个 model 字段是接口类型，测试直接赋值即可，不需要碰任何驱动；
//   - 真正的并发正确性建立在 MySQL 的 uniq_request_id / uniq_record_seq 与条件 UPDATE 上，
//     单测不复刻行锁。这里复刻的是**语义**：唯一键命中即返回包装 ErrRequestIdDuplicated 的错误、
//     INSERT IGNORE 命中即 0 行、CAS（state IN (...) + version=expected）不命中即 0 行、
//     last_seq 用 GREATEST 只前进、终态行的 patch 不再推进、"没有这行"返回 (nil, nil)
//     而"读不动这行"返回错误。断言的是 logic 面对这些返回值的裁决顺序与副作用，
//     这正是无库条件下可证明的部分；只有 MySQL 才能证明的东西（索引真的存在、
//     INSERT IGNORE 真的按 uniq_record_seq 去重、GREATEST 的 SQL 拼写、事务隔离级别）
//     由 model 层的 live_media_model_test.go 与迁移 SQL 负责，不在本文件重复假装覆盖；
//   - fake 逐条对齐 model 的真实实现：InsertTx 只**返回**自增主键、不回填结构体字段
//     （live_record_task.go:181、live_transcode_task.go:180 都是 return id, nil），
//     录制登记的 SQL 里 last_seq/segment_count/gap_count/recorded_duration_ms 与 version
//     是写死的字面量（live_record_task.go:167）。这些"看着不起眼"的口径正是 Start*
//     能否回读到自己刚建的行的关键，fake 若顺手"好心"回填就会把生产缺陷掩盖掉；
//   - 每个 fake 只内嵌接口并覆写被测路径用到的方法，其余方法由内嵌的 nil 接口提升：
//     测试一旦走到未实现的方法会当场 panic（响的失败）。
//     ReplayTasks / ReplayRefs 从「回放生命周期」批次起必须有 fake（SubmitReplayTask 起的
//     四个写方法、回放读侧三个方法全在它们上面）；RetentionTasks 与本批七个方法无关，
//     三个 zrpc client 在本包已覆盖的方法里都不该被触碰，故保持 nil。
//   - TransactCtx 在入口快照内存态、回调报错时整体回滚，
//     因此可以真正断言「事务中途失败不留半成品行、不留下半条事件」。
//     注意：调用计数（calls）不参与回滚——它记录「发生过几次尝试」，回滚只撤销数据状态；
//     断言副作用一律「计数增量 + 数据状态」两条一起看。
//
// 本文件只服务本包测试，不连接 MySQL/Redis/etcd/网络：Cache 恒为 nil，
// 读侧因此必然走「缓存缺失 → 回源主表」那条分支（与线上未配置 Redis 时一致）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/common/eventenvelope"
	"go-video/services/live-media/internal/config"
	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

const (
	testLevel    = int32(rpc.BitrateLevel_BITRATE_LEVEL_HD)
	testProtocol = int32(rpc.StreamProtocol_STREAM_PROTOCOL_HLS)
)

const (
	testRoomID    int64 = 71001
	testRoomOther int64 = 71002 // 另一个房间，用于越权/串房间断言
	testSession   int64 = 88001
	testSession2  int64 = 88002
	testTemplate  int64 = 501
	testAnchor    int64 = 42001

	// leakMarker 是「绝不能以明文出现在任何落库文本里」的假凭据值。
	// 它本身不含 secretMarkers 的关键字，因此一旦泄漏就能被 wantNoLeak 精确抓到。
	leakMarker = "deadbeef9876543210"
	// leakSignedURL 一枚形态真实的签名拉流地址（含 X-Amz-Signature 查询参数）。
	leakSignedURL = "https://pull.example.com/live/room71001.flv?X-Amz-Algorithm=SHA1&X-Amz-Signature=" +
		leakMarker
	// leakObjectKey 带签名参数的对象 key（checkObjectRef 必须拒绝）。
	leakObjectKey = "rec/71001/88001/seg-3.m3u8?x-amz-signature=" + leakMarker
)

// nowTS 真实时钟：logic 一律走 model.NowUnix()，用例用「相对现在的偏移」构造时间，
// 不依赖挂钟具体读数，也不给生产代码开时钟注入口。
func nowTS() int64 { return time.Now().Unix() }

// startTranscodeReq 一份合法的开始转码请求（用例按需在副本上改字段）。
func startTranscodeReq(requestID string) *rpc.StartLiveTranscodeReq {
	return &rpc.StartLiveTranscodeReq{
		RoomId: testRoomID, LiveSessionId: testSession, TemplateId: testTemplate,
		BitrateLevel: rpc.BitrateLevel(testLevel), Protocol: rpc.StreamProtocol(testProtocol),
		SourceRef: "rtmp://origin.example.com/live/room71001", AnchorMid: testAnchor,
		MaxAttempts: 3, TimeoutSeconds: 60, RequestId: requestID, TraceId: "trace-tc-1",
	}
}

// segmentReq 一份合法的切片登记请求。
func segmentReq(recordID, seq int64, state int32) *rpc.ReportRecordSegmentReq {
	base := seq * 10
	return &rpc.ReportRecordSegmentReq{
		RecordId: recordID, Seq: seq, StartAt: 1000000 + base - 10, EndAt: 1000000 + base,
		DurationMs: 10000, State: rpc.SegmentState(state),
		Bucket: "live-rec", ObjectKey: "rec/71001/" + strconv.FormatInt(seq, 10) + ".m3u8",
		SizeBytes: 1 << 20, Checksum: strings.Repeat("a", 64),
		WorkerId: "worker-a", TraceId: "trace-seg-1",
	}
}

// dupErr 复刻 MySQL 1062 的报文形态：model.isDuplicateErr 就是按报文判定的
// （见 model/errors.go 说明：本仓库不 import 驱动专有错误类型）。
// 所以这里必须造报文而不是造自定义错误类型，否则测的是假实现的脾气。
func dupErr(key string) error {
	return fmt.Errorf("Error 1062 (23000): Duplicate entry '%s' for key '%s'", key, key)
}

// ------------------------------------------------- 生产缺陷 #1 的回归护栏（2026-09-22 已修）

// errEventTypeNameInvalid 按报文识别 eventenvelope 的 event_type 语法拒绝。
// 与 model.isDuplicateErr 同一口径：本仓库不 import 对方包的自定义错误类型，报文就是接口。
const errEventTypeNameInvalid = "must be lowercase dot-separated"

// envelopeProbe 用与 helpers.go 完全相同的调用形式探测本服务当前能否写出任何一条事件。
//
// 缺陷 #1（已修）：本服务 9 个 EventType* 常量曾在点号后用下划线
// （如 "livemedia.record_state_changed"），而 eventenvelope 的 isValidEventType 只接受
// 小写字母、数字与点号。后果不是「事件名不美观」，而是 appendOutboxEvent 里的
// eventenvelope.New 恒返回错误 → 承载它的事务恒回滚 → 所有「推进状态并登记事件」的写路径失败。
// 本探针的作用是让下面的 Skip 机制只在「确实是这类契约错误」时生效：修好后它返回 nil，
// 于是 requireNoEnvelopeBug 不再 Skip，用例恢复真断言（当前全包 0 Skip）。
// 全仓同类问题由 common/eventenvelope/event_type_gate_test.go 拦。
func envelopeProbe() error {
	_, err := eventenvelope.New(model.ProducerName, model.EventTypeRecordStateChanged,
		model.AggregateRecordTask, "1", model.EventSchemaVersion, json.RawMessage(`{"a":1}`), "trace-probe")
	return err
}

// requireNoEnvelopeBug 放在「本用例要断言的效果必须等事务提交后才存在」的调用点之后：
// 若失败原因正是缺陷 #1，则 Skip（Skip 不是通过，它会把原因打进 -v 输出）。
// 缺陷修好后这里自动变成普通断言路径，用例重新获得完整的失败能力。
func requireNoEnvelopeBug(t *testing.T, err error, label string) {
	t.Helper()
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), errEventTypeNameInvalid) {
		return // 与缺陷无关的失败：照常红，不许借此隐身
	}
	if perr := envelopeProbe(); perr == nil {
		t.Fatalf("%s：报的是 event_type 契约错误，但探针显示信封构造正常（缺陷 #1 可能已修，"+
			"或本用例走了另一条路径）：%v", label, err)
	}
	t.Skipf("%s：被生产缺陷 #1 阻塞（live-media 的 EventType* 常量含下划线，eventenvelope 只接受"+
		"小写字母/数字/点号）→ appendOutboxEvent 失败 → 整个事务回滚，无提交效果可断言。"+
		"缺陷由 TestOutboxEventTypesSatisfyEnvelopeContract 钉住，修复后本用例自动恢复断言。原始错误：%v",
		label, err)
}

// acceptEnvelopeBugForSnapshot 用于「断言对象是事务内构造出来的写入快照」的用例。
//
// 与 requireNoEnvelopeBug 的分工：那里的断言依赖提交后的可见效果，只能 Skip；
// 而 store.triedRecords/triedTranscodes 记录的是「INSERT 的入参」，回滚抹不掉它，
// 所以登记快照本身的断言今天就有失败能力，不该被缺陷 #1 一起废掉。
// 这里仍然不吞错误：err 非空时只接受「恰好是缺陷 #1 的契约失败」，其它错误当场 Fatalf。
func acceptEnvelopeBugForSnapshot(t *testing.T, err error, label string) {
	t.Helper()
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), errEventTypeNameInvalid) {
		t.Fatalf("%s：意外错误（不属于缺陷 #1 的 envelope 契约失败，不许借缺陷隐身）：%v", label, err)
	}
	if perr := envelopeProbe(); perr == nil {
		t.Fatalf("%s：探针显示信封构造正常，却仍报 event_type 契约错误：%v", label, err)
	}
	t.Logf("%s：缺陷 #1 使整笔事务回滚（见 outbox_contract_test.go）；本用例只断言回滚前构造的写入快照", label)
}

// ---------------------------------------------------------------- 断言小工具

// wantOK 断言成功并回传结果。
func wantOK[T any](t *testing.T, got T, err error, label string) T {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：意外失败：%v", label, err)
	}
	return got
}

// wantFail 断言「未通过门禁」：必须回指定哨兵，且响应体为空。
// reply 往往是 typed nil 指针，`reply != nil` 恒真，因此用 reflect 判空。
func wantFail(t *testing.T, reply any, err error, want error, label string) {
	t.Helper()
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("%s：应报 %v，实际 err=%v", label, want, err)
	}
	if isNilPtr(reply) {
		return
	}
	t.Fatalf("%s：失败路径不得带回响应体：%+v", label, reply)
}

// isNilPtr 判「接口里装的到底是不是 nil」。
func isNilPtr(v any) bool {
	rv := reflect.ValueOf(v)
	return !rv.IsValid() || (rv.Kind() == reflect.Ptr && rv.IsNil())
}

// wantCalls 断言某个 model 方法的调用增量（before 由调用方在动作前取 db.count 得到）。
func wantCalls(t *testing.T, db *store, op string, before, want int, label string) {
	t.Helper()
	if got := db.count(op) - before; got != want {
		t.Fatalf("%s：%s 调用增量=%d，期望 %d", label, op, got, want)
	}
}

// writeOps 全部写侧方法名（断言零副作用用）。读方法不进这个集合：
// 判定链读几次是实现细节，但「写了没有」是契约。
var writeOps = []string{
	"DB.TransactCtx",
	"TranscodeTasks.InsertTx", "TranscodeTasks.UpdateState", "TranscodeTasks.UpdateStateTx",
	"RecordTasks.InsertTx", "RecordTasks.UpdateState", "RecordTasks.UpdateStateTx",
	"RecordTasks.RefreshStatsTx",
	"Segments.InsertIgnoreTx", "Segments.InsertIgnoreMissingTx", "Segments.UpdateStateTx",
	"StreamOutputs.UpsertTx", "StreamOutputs.MarkOfflineTx",
	"ReplayTasks.Insert", "ReplayTasks.InsertTx",
	"ReplayTasks.UpdateState", "ReplayTasks.UpdateStateTx",
	"ReplayRefs.Upsert", "ReplayRefs.UpsertTx",
	"ReplayRefs.ApplyContentState", "ReplayRefs.ApplyContentStateTx",
	"ReplayRefs.MarkRetentionState", "ReplayRefs.MarkRetentionStateTx",
	"RetentionTasks.Insert", "RetentionTasks.InsertTx",
	"RetentionTasks.UpdateState", "RetentionTasks.UpdateStateTx",
	"Outbox.Insert",
}

func snapshotWrites(db *store) map[string]int {
	out := make(map[string]int, len(writeOps))
	for _, op := range writeOps {
		out[op] = db.count(op)
	}
	return out
}

// wantNoWrites 断言「零副作用」：整条写路径一次都没发生。
func wantNoWrites(t *testing.T, db *store, before map[string]int, label string) {
	t.Helper()
	for op, n := range before {
		if db.count(op) != n {
			t.Fatalf("%s：%s 不该被调用（%d → %d）", label, op, n, db.count(op))
		}
	}
}

// wantEvents 断言 Outbox 事件条数与类型序列（顺序即提交顺序，同事务才谈得上顺序）。
func wantEvents(t *testing.T, db *store, wantTypes []string, label string) {
	t.Helper()
	got := make([]string, 0, len(db.outbox))
	for _, e := range db.outbox {
		got = append(got, e.EventType)
	}
	if strings.Join(got, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("%s：事件序列=%v，期望 %v", label, got, wantTypes)
	}
}

// eventAt 取第 idx 条事件（越界即失败，不允许「没有事件」被读成「测试没检查」）。
func eventAt(t *testing.T, db *store, idx int) *model.LiveMediaOutbox {
	t.Helper()
	if idx >= len(db.outbox) {
		t.Fatalf("事件 #%d 不存在（当前共 %d 条）", idx, len(db.outbox))
	}
	return db.outbox[idx]
}

// eventPayload 解出信封里的 payload（logic 写入的是完整信封 JSON，见 helpers.go appendOutboxEvent）。
//
// 这里刻意不直接用 eventenvelope.Envelope 反序列化：它的 UnmarshalJSON 会顺带跑 Validate，
// 而 event_type 语法契约由 TestOutboxEventTypesSatisfyEnvelopeContract 单独钉住（缺陷 #1 期间
// 它必然是红的）。读数据用的解码器不该把「另一条契约」混进来，否则一个缺陷会污染一片用例。
func eventPayload(t *testing.T, ev *model.LiveMediaOutbox) map[string]any {
	t.Helper()
	var env struct {
		EventID       string          `json:"event_id"`
		EventType     string          `json:"event_type"`
		SchemaVersion int             `json:"schema_version"`
		OccurredAt    string          `json:"occurred_at"`
		Producer      string          `json:"producer"`
		TraceID       string          `json:"trace_id"`
		AggregateType string          `json:"aggregate_type"`
		AggregateID   string          `json:"aggregate_id"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(ev.Payload), &env); err != nil {
		t.Fatalf("事件 %s 信封无法解析：%v", ev.EventId, err)
	}
	if env.EventID != ev.EventId || env.EventType != ev.EventType {
		t.Fatalf("信封与列值不一致：envelope=%s/%s column=%s/%s",
			env.EventID, env.EventType, ev.EventId, ev.EventType)
	}
	if env.Producer != model.ProducerName || env.SchemaVersion != model.EventSchemaVersion {
		t.Fatalf("信封 producer/schema 不对：%s/%d", env.Producer, env.SchemaVersion)
	}
	if env.AggregateID != ev.AggregateId || env.AggregateType != ev.AggregateType {
		t.Fatalf("信封聚合引用与列值不一致：envelope=%s/%s column=%s/%s",
			env.AggregateType, env.AggregateID, ev.AggregateType, ev.AggregateId)
	}
	if env.OccurredAt == "" {
		t.Fatalf("信封缺 occurred_at：%s 的消费者无法判序", ev.EventId)
	}
	var p map[string]any
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("事件 %s payload 无法解析：%v", ev.EventId, err)
	}
	return p
}

// triedRecord 取第 idx 次「InsertTx 收到的录制行副本」（不参与回滚，越界即失败）。
func triedRecord(t *testing.T, db *store, idx int) *model.LiveRecordTask {
	t.Helper()
	if idx >= len(db.triedRecords) {
		t.Fatalf("RecordTasks.InsertTx 未被调用过 %d 次（共 %d 次）", idx+1, len(db.triedRecords))
	}
	return db.triedRecords[idx]
}

// triedTranscode 取第 idx 次「InsertTx 收到的转码行副本」。
func triedTranscode(t *testing.T, db *store, idx int) *model.LiveTranscodeTask {
	t.Helper()
	if idx >= len(db.triedTranscodes) {
		t.Fatalf("TranscodeTasks.InsertTx 未被调用过 %d 次（共 %d 次）", idx+1, len(db.triedTranscodes))
	}
	return db.triedTranscodes[idx]
}

// triedOutput 取第 idx 次「UpsertTx 收到的档位行副本」（不参与回滚，越界即失败）。
func triedOutput(t *testing.T, db *store, idx int) *model.LiveStreamOutput {
	t.Helper()
	if idx >= len(db.triedOutputs) {
		t.Fatalf("StreamOutputs.UpsertTx 未被调用过 %d 次（共 %d 次）", idx+1, len(db.triedOutputs))
	}
	return db.triedOutputs[idx]
}

// triedRetention 取第 idx 次「InsertTx 收到的回收任务行副本」（不参与回滚，越界即失败）。
func triedRetention(t *testing.T, db *store, idx int) *model.LiveRetentionTask {
	t.Helper()
	if idx >= len(db.triedRetentions) {
		t.Fatalf("RetentionTasks.InsertTx 未被调用过 %d 次（共 %d 次）", idx+1, len(db.triedRetentions))
	}
	return db.triedRetentions[idx]
}

// wantNoLeak 断言假凭据没有以明文落进任何存储文本（行、事件、切片）。
// 覆盖列：err_msg / source_ref / trace_id / worker_id / bucket / object_key / cdn_domain /
// output_prefix / request_id / operator / reason 与事件 payload
// —— 只查「读接口返回什么」是不够的，泄漏发生在落库那一刻。
func wantNoLeak(t *testing.T, db *store, label string) {
	t.Helper()
	var hits []string
	for _, row := range db.transcodes {
		hits = append(hits, row.ErrMsg, row.SourceRef, row.TraceId)
	}
	for _, row := range db.records {
		hits = append(hits, row.ErrMsg, row.OutputPrefix, row.TraceId)
	}
	for _, bySeq := range db.segments {
		for _, seg := range bySeq {
			hits = append(hits, seg.Bucket, seg.ObjectKey, seg.TraceId)
		}
	}
	for _, row := range db.outputs {
		hits = append(hits, row.Bucket, row.ObjectKey, row.CdnDomain, row.TraceId, row.RequestId)
	}
	for _, row := range db.replays {
		hits = append(hits, row.OutputBucket, row.OutputKey, row.Bvid, row.Title, row.Description,
			row.ErrMsg, row.TraceId, row.RequestId)
	}
	for _, row := range db.replayRefs {
		hits = append(hits, row.Bucket, row.ObjectKey, row.Bvid, row.TraceId, row.RequestId,
			row.LastEventId, row.Source)
	}
	for _, row := range db.retentions {
		hits = append(hits, row.Reason, row.Operator, row.ErrMsg, row.TraceId, row.RequestId)
	}
	for _, ev := range db.outbox {
		hits = append(hits, ev.Payload, ev.TraceId)
	}
	// 「打算写入但被回滚」的同样算泄漏：落库语句一旦进了生产 SQL 就是泄漏。
	for _, row := range db.triedRecords {
		hits = append(hits, row.OutputPrefix, row.TraceId, row.ErrMsg)
	}
	for _, row := range db.triedTranscodes {
		hits = append(hits, row.SourceRef, row.TraceId, row.ErrMsg)
	}
	for _, row := range db.triedOutputs {
		hits = append(hits, row.Bucket, row.ObjectKey, row.CdnDomain, row.TraceId, row.RequestId)
	}
	for _, seg := range append(append([]*model.LiveRecordSegment(nil), db.triedSegments...), db.triedMissing...) {
		hits = append(hits, seg.Bucket, seg.ObjectKey, seg.TraceId, seg.WorkerId)
	}
	for _, row := range db.triedReplays {
		hits = append(hits, row.OutputBucket, row.OutputKey, row.Bvid, row.Title, row.Description,
			row.ErrMsg, row.TraceId, row.RequestId)
	}
	for _, row := range db.triedReplayRefs {
		hits = append(hits, row.Bucket, row.ObjectKey, row.Bvid, row.TraceId, row.RequestId,
			row.LastEventId, row.Source)
	}
	for _, row := range db.triedRetentions {
		hits = append(hits, row.Reason, row.Operator, row.ErrMsg, row.TraceId, row.RequestId)
	}
	for _, s := range hits {
		if strings.Contains(s, leakMarker) {
			t.Fatalf("%s：明文凭据泄漏到存储：%s", label, s)
		}
	}
}

// ---------------------------------------------------------------- 内存库

type store struct {
	transcodes map[int64]*model.LiveTranscodeTask
	records    map[int64]*model.LiveRecordTask
	segments   map[int64]map[int64]*model.LiveRecordSegment // record_id -> seq -> 行
	outputs    map[int64]*model.LiveStreamOutput            // output_id -> 行（天然键另见 naturalKey）
	replays    map[int64]*model.LiveReplayTask              // replay_id -> 行
	replayRefs map[int64]*model.LiveReplayAssetRef          // id -> 行（三个唯一键另见下）
	retentions map[int64]*model.LiveRetentionTask           // retention_id -> 行（uniq_request_id 另见下）
	outbox     []*model.LiveMediaOutbox

	seq   map[string]int64
	calls map[string]int
	errs  map[string]error
	// hooks 在某个 model 方法被调用的**瞬间**执行注入动作（一次性）。
	// 只用于表达「logic 读到快照之后、写入之前，库里的行被别人改掉了」这类交错：
	// 纯前置构造做不到（logic 读的就是同一个值），改 logic 传进来的副本更是不影响库。
	// 注意：钩子改出来的状态若落在被回滚的事务里，会随快照一起被撤销（见 fakeConn.TransactCtx），
	// 所以它只能用来验证「logic 面对 0 行如何归因」，不能用来验证交错后的最终库态。
	hooks map[string][]func()
	// misses 让某个读方法一次性「读不到」（返回 nil 行而非错误）：
	// 复刻「logic 预读时对手事务尚未提交，写入时才撞唯一键」这种真实时序。
	misses map[string]int

	// 读侧透传参数快照：logic 归一后的 pn/ps/limit 到底传没传给 model，只能这样钉住。
	lastTranscodeList *model.TranscodeTaskFilter
	lastRecordList    *model.RecordTaskFilter
	lastSegmentList   *segmentQuery
	lastSegmentStats  *segmentStatsQuery
	lastOutputList    *outputListQuery
	lastReplayList    *model.ReplayTaskFilter
	lastReplayRefList *model.ReplayRefFilter
	lastRetentionList *model.RetentionFilter

	// 「尝试写入的行」：与 calls 一样不参与事务回滚。
	// 事务回滚后库里没有行，但「logic 打算写什么」仍然是可断言的事实——
	// 这让登记快照（列宽、状态、水位、成对心跳）在缺陷 #1 阻塞提交时依然可测。
	triedRecords    []*model.LiveRecordTask
	triedTranscodes []*model.LiveTranscodeTask
	triedSegments   []*model.LiveRecordSegment
	triedMissing    []*model.LiveRecordSegment
	triedOutputs    []*model.LiveStreamOutput
	triedReplays    []*model.LiveReplayTask
	triedReplayRefs []*model.LiveReplayAssetRef
	triedRetentions []*model.LiveRetentionTask
}

type segmentQuery struct {
	recordID, afterSeq int64
	state              int32
	limit              int32
}

// outputListQuery 钉住 ListByRoom 的全部入参：档位读侧的三条口径（是否只看在线、
// 是否按场次收敛、分页是否被夹取）都只体现在参数里，返回值看不出来。
type outputListQuery struct {
	roomID, sessionID int64
	includeOffline    bool
	pn, ps, maxPS     int32
}

// segmentStatsQuery 钉住 StatsInRange 的区间参数：缺口判定「按上报水位还是按库内 MAX(seq)」
// 的差别只在参数里，返回值看不出来。
type segmentStatsQuery struct {
	recordID, fromSeq, toSeq int64
}

func newStore() *store {
	return &store{
		transcodes: map[int64]*model.LiveTranscodeTask{},
		records:    map[int64]*model.LiveRecordTask{},
		segments:   map[int64]map[int64]*model.LiveRecordSegment{},
		outputs:    map[int64]*model.LiveStreamOutput{},
		replays:    map[int64]*model.LiveReplayTask{},
		replayRefs: map[int64]*model.LiveReplayAssetRef{},
		retentions: map[int64]*model.LiveRetentionTask{},
		seq:        map[string]int64{},
		calls:      map[string]int{},
		errs:       map[string]error{},
		hooks:      map[string][]func(){},
		misses:     map[string]int{},
	}
}

// hit 记一次调用；先跑该算子的一次性钩子（模拟并发交错），再返回注入错误。
func (db *store) hit(op string) error {
	db.calls[op]++
	if hs := db.hooks[op]; len(hs) > 0 {
		hs[0]()
		db.hooks[op] = hs[1:]
	}
	return db.errs[op]
}

func (db *store) count(op string) int { return db.calls[op] }

// failOn 让某个 model 方法从此刻起报错：用于「依赖故障必须 fail closed」与事务回滚的用例。
func (db *store) failOn(op string, err error) { db.errs[op] = err }

// onHit 在某个 model 方法下一次被调用的瞬间执行 fn（一次性）。
func (db *store) onHit(op string, fn func()) { db.hooks[op] = append(db.hooks[op], fn) }

// missOnce 让某个读方法接下来的 n 次读取「查无此行」（返回 nil,nil，不是错误）。
// 参与事务回滚吗？不参与：它表达的是读时序，不是数据状态。
func (db *store) missOnce(op string) { db.misses[op]++ }

// missed 消耗一次 missOnce 额度。
func (db *store) missed(op string) bool {
	if db.misses[op] > 0 {
		db.misses[op]--
		return true
	}
	return false
}

func (db *store) next(k string) int64 { db.seq[k]++; return db.seq[k] }

// putSegment 直写切片行（种子数据用），并维持 (record_id, seq) 唯一。
func (db *store) putSegment(s *model.LiveRecordSegment) *model.LiveRecordSegment {
	if s.Id == 0 {
		s.Id = db.next("segment")
	}
	if s.RegisteredAt == 0 {
		s.RegisteredAt = nowTS()
	}
	if s.Mtime == 0 {
		s.Mtime = s.RegisteredAt
	}
	bySeq, ok := db.segments[s.RecordId]
	if !ok {
		bySeq = map[int64]*model.LiveRecordSegment{}
		db.segments[s.RecordId] = bySeq
	}
	bySeq[s.Seq] = s
	return s
}

// segmentRows 按 seq 升序返回某录制任务的全部切片（fake 内部读序与 ORDER BY seq ASC 一致）。
func (db *store) segmentRows(recordID int64) []*model.LiveRecordSegment {
	var rows []*model.LiveRecordSegment
	for _, s := range db.segments[recordID] {
		c := *s
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	return rows
}

// --- 快照 / 回滚（只覆盖数据态，不覆盖调用计数、读参数快照与自增序列）

type txSnapshot struct {
	transcodes map[int64]*model.LiveTranscodeTask
	records    map[int64]*model.LiveRecordTask
	segments   map[int64]map[int64]*model.LiveRecordSegment
	outputs    map[int64]*model.LiveStreamOutput
	replays    map[int64]*model.LiveReplayTask
	replayRefs map[int64]*model.LiveReplayAssetRef
	retentions map[int64]*model.LiveRetentionTask
	outbox     []*model.LiveMediaOutbox
}

func cloneByID[T any](m map[int64]*T) map[int64]*T {
	out := make(map[int64]*T, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

func (db *store) snapshot() *txSnapshot {
	segments := make(map[int64]map[int64]*model.LiveRecordSegment, len(db.segments))
	for rid, bySeq := range db.segments {
		segments[rid] = cloneByID(bySeq)
	}
	return &txSnapshot{
		transcodes: cloneByID(db.transcodes),
		records:    cloneByID(db.records),
		segments:   segments,
		outputs:    cloneByID(db.outputs),
		replays:    cloneByID(db.replays),
		replayRefs: cloneByID(db.replayRefs),
		retentions: cloneByID(db.retentions),
		outbox:     append([]*model.LiveMediaOutbox(nil), db.outbox...),
	}
}

// restore 只回滚数据态。自增序列刻意不回滚（与 MySQL 一致：AUTO_INCREMENT 回滚后不重用），
// 否则会出现「回滚后新行拿到一个已存在的 id」这种内存版独有的假象。
func (db *store) restore(s *txSnapshot) {
	db.transcodes, db.records = s.transcodes, s.records
	db.segments = s.segments
	db.outputs = s.outputs
	db.replays, db.replayRefs = s.replays, s.replayRefs
	db.retentions = s.retentions
	db.outbox = append([]*model.LiveMediaOutbox(nil), s.outbox...)
}

// fakeConn 只实现真正被测的 TransactCtx，其余 sqlx 方法由内嵌 nil 接口提升：
// 任何「想拿真连接执行 SQL」的写法都会当场 panic。
type fakeConn struct {
	sqlx.SqlConn
	db *store
}

func (c fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.db.calls["DB.TransactCtx"]++
	if err := c.db.errs["DB.TransactCtx"]; err != nil {
		return err // 连事务都开不起来（连接池/驱动故障）
	}
	snap := c.db.snapshot()
	// session 传 nil：本包 fake 一律忽略 session。真实现的 pick(session, conn) 在 session
	// 非空时走同一会话，内存版里「事务内的写」对后续读天然可见，语义等价。
	if err := fn(ctx, nil); err != nil {
		c.db.restore(snap)
		return err
	}
	return nil
}

// ---------------------------------------------------------------- live_transcode_task

type fakeTranscodeTasks struct {
	model.LiveTranscodeTaskModel
	db *store
}

func (f fakeTranscodeTasks) InsertTx(_ context.Context, _ sqlx.Session,
	t *model.LiveTranscodeTask) (int64, error) {
	if err := f.db.hit("TranscodeTasks.InsertTx"); err != nil {
		return 0, err
	}
	if t.Version == 0 {
		t.Version = 1
	}
	now := nowTS()
	if t.Ctime == 0 {
		t.Ctime = now
	}
	if t.Mtime == 0 {
		t.Mtime = now
	}
	// uniq_request_id：撞键即返回与真实现同构的错误（logic 据此走幂等回读）。
	for _, row := range f.db.transcodes {
		if row.RequestId == t.RequestId {
			return 0, fmt.Errorf("request_id=%s: %w", t.RequestId,
				model.ErrRequestIdDuplicated)
		}
	}
	// 真实现只 return id, nil，不回填 t.TaskId（live_transcode_task.go:166-180）——
	// fake 必须同样不回填，否则会把「logic 用了零值主键」这个缺陷掩盖掉。
	id := f.db.next("task")
	row := *t
	row.TaskId = id
	f.db.transcodes[id] = &row
	tried := *t
	f.db.triedTranscodes = append(f.db.triedTranscodes, &tried)
	return id, nil
}

func (f fakeTranscodeTasks) FindOne(_ context.Context, taskID int64) (*model.LiveTranscodeTask, error) {
	if err := f.db.hit("TranscodeTasks.FindOne"); err != nil {
		return nil, err
	}
	if taskID <= 0 || f.db.missed("TranscodeTasks.FindOne") {
		return nil, nil // WHERE task_id = ?：查不到，但不是错误
	}
	row, ok := f.db.transcodes[taskID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeTranscodeTasks) FindByRequestID(_ context.Context,
	requestID string) (*model.LiveTranscodeTask, error) {
	if err := f.db.hit("TranscodeTasks.FindByRequestID"); err != nil {
		return nil, err
	}
	if requestID == "" || f.db.missed("TranscodeTasks.FindByRequestID") {
		return nil, nil
	}
	for _, row := range f.db.transcodes {
		if row.RequestId == requestID {
			c := *row
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakeTranscodeTasks) FindActiveByRoom(_ context.Context, roomID, sessionID int64,
	bitrateLevel, protocol int32) (*model.LiveTranscodeTask, error) {
	if err := f.db.hit("TranscodeTasks.FindActiveByRoom"); err != nil {
		return nil, err
	}
	if f.db.missed("TranscodeTasks.FindActiveByRoom") {
		return nil, nil
	}
	var best *model.LiveTranscodeTask
	for _, row := range f.db.transcodes {
		if row.RoomId != roomID || row.LiveSession != sessionID ||
			row.BitrateLevel != bitrateLevel || row.Protocol != protocol {
			continue
		}
		// 真实现：state IN (PENDING, RUNNING, STOPPING) ORDER BY task_id DESC LIMIT 1
		if row.State != model.TranscodeStatePending && row.State != model.TranscodeStateRunning &&
			row.State != model.TranscodeStateStopping {
			continue
		}
		if best == nil || row.TaskId > best.TaskId {
			c := *row
			best = &c
		}
	}
	if best == nil {
		return nil, nil
	}
	return best, nil
}

func (f fakeTranscodeTasks) List(_ context.Context,
	ff model.TranscodeTaskFilter) ([]*model.LiveTranscodeTask, int32, error) {
	if err := f.db.hit("TranscodeTasks.List"); err != nil {
		return nil, 0, err
	}
	cp := ff
	f.db.lastTranscodeList = &cp
	var rows []*model.LiveTranscodeTask
	for _, row := range f.db.transcodes {
		if ff.RoomId > 0 && row.RoomId != ff.RoomId {
			continue
		}
		if ff.SessionId > 0 && row.LiveSession != ff.SessionId {
			continue
		}
		if ff.State > 0 && row.State != ff.State {
			continue
		}
		if ff.TemplateId > 0 && row.TemplateId != ff.TemplateId {
			continue
		}
		c := *row
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TaskId > rows[j].TaskId })
	total := int32(len(rows))
	limit, offset := fakePage(ff.Pn, ff.Ps, ff.MaxPageSize)
	rows = slicePage(rows, limit, offset)
	return rows, total, nil
}

func (f fakeTranscodeTasks) UpdateState(ctx context.Context, taskID int64, fromStates []int32,
	expectedVersion int64, to int32, p model.TranscodePatch) (int64, error) {
	return f.update(ctx, "TranscodeTasks.UpdateState", taskID, fromStates, expectedVersion, to, p)
}

func (f fakeTranscodeTasks) UpdateStateTx(ctx context.Context, _ sqlx.Session, taskID int64,
	fromStates []int32, expectedVersion int64, to int32,
	p model.TranscodePatch) (int64, error) {
	return f.update(ctx, "TranscodeTasks.UpdateStateTx", taskID, fromStates, expectedVersion, to, p)
}

// update 复刻条件 UPDATE：WHERE task_id=? AND state IN (?) [AND version=?]
// SET <patch> , state=?, version=version+1, mtime=now —— 0 行不是错误。
func (f fakeTranscodeTasks) update(_ context.Context, op string, taskID int64, fromStates []int32,
	expectedVersion int64, to int32, p model.TranscodePatch) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_transcode_task UpdateState: empty fromStates %w", model.ErrInvalidTransition)
	}
	row, ok := f.db.transcodes[taskID]
	if !ok {
		return 0, nil // 行不存在：UPDATE 影响 0 行（由 logic 回读归因）
	}
	if !containsState(fromStates, row.State) {
		return 0, nil
	}
	if expectedVersion > 0 && row.Version != expectedVersion {
		return 0, nil
	}
	if p.Progress != nil {
		row.Progress = *p.Progress
	}
	if p.StartedAt != nil {
		row.StartedAt = *p.StartedAt
	}
	if p.StoppedAt != nil {
		row.StoppedAt = *p.StoppedAt
	}
	if p.HeartbeatAt != nil {
		row.HeartbeatAt = *p.HeartbeatAt
	}
	if p.TimeoutAt != nil {
		row.TimeoutAt = *p.TimeoutAt
	}
	if p.Attempt != nil {
		row.Attempt = *p.Attempt
	}
	if p.Reason != nil {
		row.Reason = *p.Reason
	}
	if p.Errno != nil {
		row.Errno = *p.Errno
	}
	if p.ErrMsg != nil {
		row.ErrMsg = *p.ErrMsg
	}
	if p.TraceID != nil {
		row.TraceId = *p.TraceID
	}
	row.State = to
	row.Version++
	row.Mtime = nowTS()
	return 1, nil
}

// ---------------------------------------------------------------- live_record_task

type fakeRecordTasks struct {
	model.LiveRecordTaskModel
	db *store
}

func (f fakeRecordTasks) InsertTx(_ context.Context, _ sqlx.Session,
	t *model.LiveRecordTask) (int64, error) {
	if err := f.db.hit("RecordTasks.InsertTx"); err != nil {
		return 0, err
	}
	if t.Version == 0 {
		t.Version = 1
	}
	now := nowTS()
	if t.Ctime == 0 {
		t.Ctime = now
	}
	if t.Mtime == 0 {
		t.Mtime = now
	}
	for _, row := range f.db.records {
		if row.RequestId == t.RequestId {
			return 0, fmt.Errorf("request_id=%s: %w", t.RequestId, model.ErrRequestIdDuplicated)
		}
	}
	// 真实现的 VALUES 里 last_seq/segment_count/gap_count/recorded_duration_ms 写死 0、
	// version 写死 1（live_record_task.go:163-170）：登记行不带水位、不带计数。
	id := f.db.next("record")
	row := *t
	row.RecordId = id
	row.LastSeq, row.SegmentCount, row.GapCount, row.RecordedDuration = 0, 0, 0, 0
	row.Version = 1
	f.db.records[id] = &row
	tried := *t
	f.db.triedRecords = append(f.db.triedRecords, &tried)
	return id, nil
}

func (f fakeRecordTasks) FindOne(_ context.Context, recordID int64) (*model.LiveRecordTask, error) {
	if err := f.db.hit("RecordTasks.FindOne"); err != nil {
		return nil, err
	}
	if recordID <= 0 || f.db.missed("RecordTasks.FindOne") {
		return nil, nil // WHERE record_id = ?：查不到，但不是错误
	}
	row, ok := f.db.records[recordID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeRecordTasks) FindByRequestID(_ context.Context,
	requestID string) (*model.LiveRecordTask, error) {
	if err := f.db.hit("RecordTasks.FindByRequestID"); err != nil {
		return nil, err
	}
	if requestID == "" || f.db.missed("RecordTasks.FindByRequestID") {
		return nil, nil
	}
	for _, row := range f.db.records {
		if row.RequestId == requestID {
			c := *row
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakeRecordTasks) FindActiveBySession(_ context.Context, roomID, sessionID int64) (*model.LiveRecordTask, error) {
	if err := f.db.hit("RecordTasks.FindActiveBySession"); err != nil {
		return nil, err
	}
	if f.db.missed("RecordTasks.FindActiveBySession") {
		return nil, nil
	}
	var best *model.LiveRecordTask
	for _, row := range f.db.records {
		if row.RoomId != roomID || row.LiveSession != sessionID {
			continue
		}
		if row.State != model.RecordStatePending && row.State != model.RecordStateRecording &&
			row.State != model.RecordStateStopping {
			continue
		}
		if best == nil || row.RecordId > best.RecordId {
			c := *row
			best = &c
		}
	}
	return best, nil
}

func (f fakeRecordTasks) List(_ context.Context,
	ff model.RecordTaskFilter) ([]*model.LiveRecordTask, int32, error) {
	if err := f.db.hit("RecordTasks.List"); err != nil {
		return nil, 0, err
	}
	cp := ff
	f.db.lastRecordList = &cp
	var rows []*model.LiveRecordTask
	for _, row := range f.db.records {
		if ff.RoomId > 0 && row.RoomId != ff.RoomId {
			continue
		}
		if ff.SessionId > 0 && row.LiveSession != ff.SessionId {
			continue
		}
		if ff.State > 0 && row.State != ff.State {
			continue
		}
		c := *row
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RecordId > rows[j].RecordId })
	total := int32(len(rows))
	limit, offset := fakePage(ff.Pn, ff.Ps, ff.MaxPageSize)
	rows = slicePage(rows, limit, offset)
	return rows, total, nil
}

func (f fakeRecordTasks) UpdateState(ctx context.Context, recordID int64, fromStates []int32,
	expectedVersion int64, p model.RecordPatch) (int64, error) {
	return f.update(ctx, "RecordTasks.UpdateState", recordID, fromStates, expectedVersion, p)
}

func (f fakeRecordTasks) UpdateStateTx(ctx context.Context, _ sqlx.Session, recordID int64,
	fromStates []int32, expectedVersion int64, p model.RecordPatch) (int64, error) {
	return f.update(ctx, "RecordTasks.UpdateStateTx", recordID, fromStates, expectedVersion, p)
}

func (f fakeRecordTasks) update(_ context.Context, op string, recordID int64, fromStates []int32,
	expectedVersion int64, p model.RecordPatch) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_record_task UpdateState: empty fromStates %w", model.ErrInvalidTransition)
	}
	row, ok := f.db.records[recordID]
	if !ok {
		return 0, nil
	}
	if !containsState(fromStates, row.State) {
		return 0, nil
	}
	if expectedVersion > 0 && row.Version != expectedVersion {
		return 0, nil
	}
	if p.LastSeq != nil {
		// GREATEST(last_seq, ?)：水位只前进（RecordPatch.sets 的 rawExpr）
		if *p.LastSeq > row.LastSeq {
			row.LastSeq = *p.LastSeq
		}
	}
	if p.EndAt != nil {
		row.EndAt = *p.EndAt // 期望终点，与 record_end_at 分离
	}
	if p.HeartbeatAt != nil {
		row.HeartbeatAt = *p.HeartbeatAt
	}
	if p.TimeoutAt != nil {
		row.TimeoutAt = *p.TimeoutAt
	}
	if p.RecordStartAt != nil {
		row.RecordStartAt = *p.RecordStartAt
	}
	if p.RecordEndAt != nil {
		row.RecordEndAt = *p.RecordEndAt
	}
	if p.Reason != nil {
		row.Reason = *p.Reason
	}
	if p.Errno != nil {
		row.Errno = *p.Errno
	}
	if p.ErrMsg != nil {
		row.ErrMsg = *p.ErrMsg
	}
	if p.TraceID != nil {
		row.TraceId = *p.TraceID
	}
	row.State = p.State
	row.Version++
	row.Mtime = nowTS()
	return 1, nil
}

// RefreshStatsTx 与真实现同构：计数全部从切片表重算，last_seq 只前进不回退。
func (f fakeRecordTasks) RefreshStats(_ context.Context, recordID int64) (int64, error) {
	return f.refresh("RecordTasks.RefreshStats", recordID)
}

func (f fakeRecordTasks) RefreshStatsTx(_ context.Context, _ sqlx.Session, recordID int64) (int64, error) {
	return f.refresh("RecordTasks.RefreshStatsTx", recordID)
}

func (f fakeRecordTasks) refresh(op string, recordID int64) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	row, ok := f.db.records[recordID]
	if !ok {
		return 0, nil
	}
	var count, gaps, dur, maxSeq int64
	for _, s := range f.db.segments[recordID] {
		count++
		switch s.State {
		case model.SegmentStateMissing, model.SegmentStateCorrupt:
			gaps++
		case model.SegmentStateVerified:
			dur += s.DurationMs
		}
		if s.Seq > maxSeq {
			maxSeq = s.Seq
		}
	}
	row.SegmentCount, row.GapCount, row.RecordedDuration = count, gaps, dur
	if maxSeq > row.LastSeq {
		row.LastSeq = maxSeq
	}
	row.Mtime = nowTS()
	return 1, nil
}

// ---------------------------------------------------------------- live_record_segment

type fakeSegments struct {
	model.LiveRecordSegmentModel
	db *store
}

func (f fakeSegments) InsertIgnoreTx(_ context.Context, _ sqlx.Session,
	s *model.LiveRecordSegment) (int64, error) {
	if err := f.db.hit("Segments.InsertIgnoreTx"); err != nil {
		return 0, err
	}
	if s.Seq <= 0 {
		return 0, fmt.Errorf("live_record_segment InsertIgnore: seq=%d %w", s.Seq, model.ErrInvalidSeq)
	}
	if s.State == model.SegmentStateUploading || s.State == model.SegmentStateUploaded ||
		s.State == model.SegmentStateVerified {
		if s.Bucket == "" || s.ObjectKey == "" {
			return 0, fmt.Errorf("live_record_segment seq=%d state=%d: %w", s.Seq, s.State,
				model.ErrInvalidBucketRef)
		}
	}
	if _, ok := f.db.segments[s.RecordId][s.Seq]; ok {
		return 0, nil // INSERT IGNORE 命中 uniq_record_seq
	}
	tried := *s
	f.db.triedSegments = append(f.db.triedSegments, &tried)
	f.db.putSegment(s)
	return 1, nil
}

// InsertIgnoreMissingTx 只写缺口行：真实现连 bucket/object_key 列都不出现在 VALUES 里。
func (f fakeSegments) InsertIgnoreMissingTx(_ context.Context, _ sqlx.Session,
	rows []*model.LiveRecordSegment) (int64, error) {
	if err := f.db.hit("Segments.InsertIgnoreMissingTx"); err != nil {
		return 0, err
	}
	var aff int64
	for i, r := range rows {
		if r.Seq <= 0 {
			return 0, fmt.Errorf("live_record_segment InsertIgnoreMissing: index=%d %w", i, model.ErrInvalidSeq)
		}
		if _, ok := f.db.segments[r.RecordId][r.Seq]; ok {
			continue
		}
		cp := *r
		cp.Bucket, cp.ObjectKey, cp.Checksum, cp.SizeBytes = "", "", "", 0
		cp.State = model.SegmentStateMissing
		tried := cp
		f.db.triedMissing = append(f.db.triedMissing, &tried)
		f.db.putSegment(&cp)
		aff++
	}
	return aff, nil
}

// FindOne 按主键查切片行（live_record_segment.go:250-259 的 WHERE id = ?）。
// SubmitRetentionTask 的 SEGMENT 分支就靠它判定「被回收对象是否真实存在」，
// 因此这里必须扫全部 record：切片表是 record_id→seq 双层图，主键不在这条索引上。
func (f fakeSegments) FindOne(_ context.Context, id int64) (*model.LiveRecordSegment, error) {
	if err := f.db.hit("Segments.FindOne"); err != nil {
		return nil, err
	}
	if id <= 0 || f.db.missed("Segments.FindOne") {
		return nil, nil // 查不到，但不是错误
	}
	for _, bySeq := range f.db.segments {
		for _, row := range bySeq {
			if row.Id == id {
				c := *row
				return &c, nil
			}
		}
	}
	return nil, nil
}

func (f fakeSegments) FindBySeq(_ context.Context, recordID, seq int64) (*model.LiveRecordSegment, error) {
	if err := f.db.hit("Segments.FindBySeq"); err != nil {
		return nil, err
	}
	if f.db.missed("Segments.FindBySeq") {
		return nil, nil
	}
	row, ok := f.db.segments[recordID][seq]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeSegments) ListAfter(_ context.Context, recordID, afterSeq int64, state,
	limit int32) ([]*model.LiveRecordSegment, error) {
	if err := f.db.hit("Segments.ListAfter"); err != nil {
		return nil, err
	}
	f.db.lastSegmentList = &segmentQuery{recordID: recordID, afterSeq: afterSeq, state: state, limit: limit}
	var rows []*model.LiveRecordSegment
	for _, s := range f.db.segmentRows(recordID) {
		if afterSeq > 0 && s.Seq <= afterSeq {
			continue
		}
		if state > 0 && s.State != state {
			continue
		}
		rows = append(rows, s)
	}
	if limit <= 0 {
		limit = 200
	}
	if int32(len(rows)) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (f fakeSegments) CountByRecord(_ context.Context, recordID int64) (int64, error) {
	if err := f.db.hit("Segments.CountByRecord"); err != nil {
		return 0, err
	}
	return int64(len(f.db.segments[recordID])), nil
}

// LastSeq 与真实现同式（live_record_segment.go:273）：COALESCE(MAX(seq),0)，
// 不看状态、不看区间——缺口行同样把水位顶上去。SubmitReplayTask 用它当 to_seq 的默认值，
// 所以「MAX 是否含 MISSING/CORRUPT」直接决定提交被拒还是被接受，必须照抄而不是"只数有效片"。
func (f fakeSegments) LastSeq(_ context.Context, recordID int64) (int64, error) {
	if err := f.db.hit("Segments.LastSeq"); err != nil {
		return 0, err
	}
	var last int64
	for _, s := range f.db.segments[recordID] {
		if s.Seq > last {
			last = s.Seq
		}
	}
	return last, nil
}

func (f fakeSegments) StatsInRange(_ context.Context, recordID, fromSeq, toSeq int64) (*model.SegmentStats, error) {
	if err := f.db.hit("Segments.StatsInRange"); err != nil {
		return nil, err
	}
	f.db.lastSegmentStats = &segmentStatsQuery{recordID: recordID, fromSeq: fromSeq, toSeq: toSeq}
	if fromSeq <= 0 {
		fromSeq = 1
	}
	if toSeq < fromSeq {
		return nil, fmt.Errorf("live_record_segment StatsInRange: [%d,%d] %w", fromSeq, toSeq,
			model.ErrInvalidSegmentRange)
	}
	st := &model.SegmentStats{}
	for _, s := range f.db.segmentRows(recordID) {
		if s.Seq < fromSeq || s.Seq > toSeq {
			continue
		}
		st.Registered++
		switch s.State {
		case model.SegmentStateUploading:
			st.Uploading++
		case model.SegmentStateUploaded:
			st.Uploaded++
		case model.SegmentStateVerified:
			st.Verified++
			st.DurationMs += s.DurationMs
		case model.SegmentStateMissing:
			st.Missing++
		case model.SegmentStateCorrupt:
			st.Corrupt++
		}
		if st.MinSeq == 0 || s.Seq < st.MinSeq {
			st.MinSeq = s.Seq
		}
		if s.Seq > st.MaxSeq {
			st.MaxSeq = s.Seq
		}
	}
	return st, nil
}

// UpdateStateTx 的前置态集合与真实现同源：由 model.IsValidSegmentTransition 反推，
// 绝不在 fake 里另写一套迁移表（否则测的是两份状态机）。
func (f fakeSegments) UpdateStateTx(_ context.Context, _ sqlx.Session, recordID, seq int64, to int32,
	p model.SegmentPatch) (int64, error) {
	if err := f.db.hit("Segments.UpdateStateTx"); err != nil {
		return 0, err
	}
	row, ok := f.db.segments[recordID][seq]
	if !ok {
		return 0, nil
	}
	if !model.IsValidSegmentTransition(row.State, to) {
		return 0, nil // WHERE state IN (segmentFromStates(to)) 不命中
	}
	if p.StartAt != nil {
		row.StartAt = *p.StartAt
	}
	if p.EndAt != nil {
		row.EndAt = *p.EndAt
	}
	if p.DurationMs != nil {
		row.DurationMs = *p.DurationMs
	}
	if p.SizeBytes != nil {
		row.SizeBytes = *p.SizeBytes
	}
	if p.Bucket != nil && *p.Bucket != "" {
		row.Bucket = *p.Bucket
	}
	if p.ObjectKey != nil && *p.ObjectKey != "" {
		row.ObjectKey = *p.ObjectKey
	}
	if p.Checksum != nil && *p.Checksum != "" {
		row.Checksum = *p.Checksum
	}
	if p.WorkerID != nil && *p.WorkerID != "" {
		row.WorkerId = *p.WorkerID
	}
	if p.TraceID != nil && *p.TraceID != "" {
		row.TraceId = *p.TraceID
	}
	row.State = to
	row.Mtime = nowTS() // 切片表不 bump version（conditionalUpdate bumpVersion=false）
	return 1, nil
}

// ---------------------------------------------------------------- live_media_outbox

type fakeOutbox struct {
	model.LiveMediaOutboxModel
	db *store
}

func (f fakeOutbox) Insert(_ context.Context, _ sqlx.Session, e *model.LiveMediaOutbox) error {
	if err := f.db.hit("Outbox.Insert"); err != nil {
		return err
	}
	if e.EventId == "" || e.EventType == "" || e.AggregateType == "" || e.AggregateId == "" {
		return fmt.Errorf("live_media_outbox Insert: event_id=%q event_type=%q aggregate=%s/%s %w",
			e.EventId, e.EventType, e.AggregateType, e.AggregateId, model.ErrEmptyEventPayload)
	}
	for _, old := range f.db.outbox {
		if old.EventId == e.EventId {
			return fmt.Errorf("event_id=%s: %w", e.EventId, model.ErrRequestIdDuplicated)
		}
	}
	now := nowTS()
	row := *e
	row.Id = f.db.next("outbox")
	if row.SchemaVersion <= 0 {
		row.SchemaVersion = model.EventSchemaVersion
	}
	if row.Ctime == 0 {
		row.Ctime = now
	}
	row.Mtime = row.Ctime
	if row.OccurredAt == 0 {
		row.OccurredAt = row.Ctime
	}
	if row.State == 0 {
		row.State = model.OutboxStatePending
	}
	f.db.outbox = append(f.db.outbox, &row)
	return nil
}

// ---------------------------------------------------------------- live_stream_output

// outputNaturalKey 是 uniq_output_natural(room_id, live_session_id, bitrate_level, protocol)
// 的内存形态。档位表的幂等完全靠这条唯一键（request_id 上没有唯一索引），
// 所以 fake 必须按同一条键判定「走 INSERT 还是走 ON DUPLICATE」，绝不能按主键。
type outputNaturalKey struct {
	roomID, sessionID int64
	level, protocol   int32
}

func keyOfOutput(o *model.LiveStreamOutput) outputNaturalKey {
	return outputNaturalKey{roomID: o.RoomId, sessionID: o.LiveSession, level: o.BitrateLevel, protocol: o.Protocol}
}

// rowByNaturalKey 返回天然键命中的行；命中多行说明测试自己写坏了内存态
// （生产里这条唯一索引保证它不许存在两行），当场 panic 而不是任选一行。
func (db *store) rowByNaturalKey(k outputNaturalKey) *model.LiveStreamOutput {
	var found *model.LiveStreamOutput
	for _, row := range db.outputs {
		if keyOfOutput(row) != k {
			continue
		}
		if found != nil {
			panic("fake: live_stream_output 同一天然键存在两行，uniq_output_natural 前提被测试写坏")
		}
		found = row
	}
	return found
}

type fakeStreamOutputs struct {
	model.LiveStreamOutputModel
	db *store
}

// UpsertTx 复刻 live_stream_output.go:84-121 三条容易想当然的口径：
//  1. state 写的是常量 StreamOutputStateOnline，不是入参 o.State —— 结构体里的 state 不入库；
//  2. 命中唯一键的更新分支不碰 ctime 与 online_at（首次登记时刻与首次上线时刻都是审计事实），
//     并把 offline_at/offline_reason 复位为 0（这就是「下线后再上线」的全部语义）；
//  3. OnlineAt==0 时真实现会改写调用方传进来的结构体（o.OnlineAt = now），fake 同样改写：
//     logic 之后还要用同一个结构体拼事件 payload，不改写就会测不到「事件与行同源」。
//
// 主键返回值：更新分支统一按天然键回读（真实现注释写明「ON DUPLICATE 时 LastInsertId 不可信」），
// 回读不到时报与真实现同构的 ErrStreamOutputNotFound，而不是返回 0 让上层以为成功。
func (f fakeStreamOutputs) UpsertTx(ctx context.Context, _ sqlx.Session,
	o *model.LiveStreamOutput) (int64, error) {
	if err := f.db.hit("StreamOutputs.UpsertTx"); err != nil {
		return 0, err
	}
	now := nowTS()
	if o.OnlineAt == 0 {
		o.OnlineAt = now
	}
	tried := *o
	f.db.triedOutputs = append(f.db.triedOutputs, &tried)

	if row := f.db.rowByNaturalKey(keyOfOutput(o)); row != nil {
		row.TaskId, row.Bucket, row.ObjectKey, row.CdnDomain = o.TaskId, o.Bucket, o.ObjectKey, o.CdnDomain
		row.Width, row.Height, row.BitrateKbps, row.Fps = o.Width, o.Height, o.BitrateKbps, o.Fps
		row.State = model.StreamOutputStateOnline
		row.OnlineExpire, row.RequestId, row.TraceId = o.OnlineExpire, o.RequestId, o.TraceId
		row.OfflineAt, row.OfflineReason, row.Mtime = 0, 0, now
		existing, err := f.FindByNaturalKey(ctx, o.RoomId, o.LiveSession, o.BitrateLevel, o.Protocol)
		if err != nil {
			return 0, err
		}
		if existing == nil {
			return 0, fmt.Errorf("live_stream_output Upsert: %w", model.ErrStreamOutputNotFound)
		}
		return existing.OutputId, nil
	}
	id := f.db.next("output")
	row := *o
	row.OutputId, row.State, row.Ctime, row.Mtime = id, model.StreamOutputStateOnline, now, now
	row.OfflineAt = 0 // INSERT 分支的 VALUES 里 offline_at 写死 0
	f.db.outputs[id] = &row
	return id, nil
}

func (f fakeStreamOutputs) FindOne(_ context.Context, outputID int64) (*model.LiveStreamOutput, error) {
	if err := f.db.hit("StreamOutputs.FindOne"); err != nil {
		return nil, err
	}
	if outputID <= 0 || f.db.missed("StreamOutputs.FindOne") {
		return nil, nil // WHERE output_id = ?：查不到，但不是错误
	}
	row, ok := f.db.outputs[outputID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeStreamOutputs) FindByNaturalKey(_ context.Context, roomID, sessionID int64,
	bitrateLevel, protocol int32) (*model.LiveStreamOutput, error) {
	if err := f.db.hit("StreamOutputs.FindByNaturalKey"); err != nil {
		return nil, err
	}
	if f.db.missed("StreamOutputs.FindByNaturalKey") {
		return nil, nil
	}
	if row := f.db.rowByNaturalKey(outputNaturalKey{roomID, sessionID, bitrateLevel, protocol}); row != nil {
		c := *row
		return &c, nil
	}
	return nil, nil
}

// FindOnlineByLevel 与真实现同式：roomID<=0 先拒绝（否则等价于全表找在线档位），
// ORDER BY output_id DESC LIMIT 2 后按行数裁决 —— 两行即「同档位两场同时在线」的数据异常，
// 猜错对象会把还在分发的档位摘掉，因此报 ErrStreamOutputAmbiguous 而不是任选一行。
func (f fakeStreamOutputs) FindOnlineByLevel(_ context.Context, roomID int64,
	bitrateLevel, protocol int32) (*model.LiveStreamOutput, error) {
	if err := f.db.hit("StreamOutputs.FindOnlineByLevel"); err != nil {
		return nil, err
	}
	if roomID <= 0 {
		return nil, model.ErrInvalidRoomID
	}
	if f.db.missed("StreamOutputs.FindOnlineByLevel") {
		return nil, nil
	}
	var rows []*model.LiveStreamOutput
	for _, row := range f.db.outputs {
		if row.RoomId != roomID || row.State != model.StreamOutputStateOnline ||
			row.BitrateLevel != bitrateLevel || row.Protocol != protocol {
			continue
		}
		c := *row
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].OutputId > rows[j].OutputId })
	if len(rows) > 2 {
		rows = rows[:2] // 真 SQL 就是 LIMIT 2：多取只为判定歧义，不为了报出准确台数
	}
	switch len(rows) {
	case 0:
		return nil, nil
	case 1:
		return rows[0], nil
	default:
		return nil, fmt.Errorf("live-media: room=%d level=%d protocol=%d has %d online outputs, use output_id: %w",
			roomID, bitrateLevel, protocol, len(rows), model.ErrStreamOutputAmbiguous)
	}
}

// ListOnlineBySession 与 model/live_stream_output.go 的 ListOnlineBySession 同式：
// roomID/sessionID 非正先拒绝（否则等价于「全表找在线档位」或按房间跨场次误伤），
// 只返回 state=online 的行，按 (bitrate_level, protocol) 升序，
// 取到 MaxSessionOutputs+1 条即报 ErrSessionOutputOverflow（真 SQL 的 LIMIT 就是这个形状）。
// 返回的是行副本：logic 拿到之后还会在事务里改内存态之外的字段，给引用会污染内存态。
func (f fakeStreamOutputs) ListOnlineBySession(_ context.Context, roomID, sessionID int64) ([]*model.LiveStreamOutput, error) {
	if err := f.db.hit("StreamOutputs.ListOnlineBySession"); err != nil {
		return nil, err
	}
	if roomID <= 0 {
		return nil, model.ErrInvalidRoomID
	}
	if sessionID <= 0 {
		return nil, model.ErrInvalidSessionID
	}
	if f.db.missed("StreamOutputs.ListOnlineBySession") {
		return nil, nil
	}
	var rows []*model.LiveStreamOutput
	for _, row := range f.db.outputs {
		if row.RoomId != roomID || row.LiveSession != sessionID ||
			row.State != model.StreamOutputStateOnline {
			continue
		}
		c := *row
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].BitrateLevel != rows[j].BitrateLevel {
			return rows[i].BitrateLevel < rows[j].BitrateLevel
		}
		if rows[i].Protocol != rows[j].Protocol {
			return rows[i].Protocol < rows[j].Protocol
		}
		return rows[i].OutputId < rows[j].OutputId
	})
	if len(rows) > model.MaxSessionOutputs {
		return nil, fmt.Errorf("live-media: room=%d session=%d has more than %d online outputs: %w",
			roomID, sessionID, model.MaxSessionOutputs, model.ErrSessionOutputOverflow)
	}
	return rows, nil
}

// ListByRoom 与真实现同式：roomID<=0 拒绝、sessionID<=0 不参与过滤、
// includeOffline=false 才附加 state=online、COUNT=0 时短路返回 (nil, 0, nil)。
func (f fakeStreamOutputs) ListByRoom(_ context.Context, roomID, sessionID int64, includeOffline bool,
	pn, ps, maxPS int32) ([]*model.LiveStreamOutput, int32, error) {
	if err := f.db.hit("StreamOutputs.ListByRoom"); err != nil {
		return nil, 0, err
	}
	q := outputListQuery{roomID: roomID, sessionID: sessionID, includeOffline: includeOffline,
		pn: pn, ps: ps, maxPS: maxPS}
	f.db.lastOutputList = &q
	if roomID <= 0 {
		return nil, 0, model.ErrInvalidRoomID
	}
	var rows []*model.LiveStreamOutput
	for _, row := range f.db.outputs {
		if row.RoomId != roomID {
			continue
		}
		if sessionID > 0 && row.LiveSession != sessionID {
			continue
		}
		if !includeOffline && row.State != model.StreamOutputStateOnline {
			continue
		}
		c := *row
		rows = append(rows, &c)
	}
	// ORDER BY bitrate_level ASC, protocol ASC：真 SQL 对同 (档位,协议) 的行不保证次序，
	// 这里以 output_id ASC 兜底，保证用例可复现（不是对生产的额外承诺）。
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].BitrateLevel != rows[j].BitrateLevel {
			return rows[i].BitrateLevel < rows[j].BitrateLevel
		}
		if rows[i].Protocol != rows[j].Protocol {
			return rows[i].Protocol < rows[j].Protocol
		}
		return rows[i].OutputId < rows[j].OutputId
	})
	total := int32(len(rows))
	if total == 0 {
		return nil, 0, nil
	}
	limit, offset := fakePage(pn, ps, maxPS)
	return slicePage(rows, limit, offset), total, nil
}

// MarkOfflineTx 复刻 conditionalUpdate(bumpVersion=false, WHERE output_id=? AND state=online)：
// 档位表没有 version 列，state 本身就是 CAS 条件；trace_id 只非空时才进 SET。
func (f fakeStreamOutputs) MarkOfflineTx(_ context.Context, _ sqlx.Session, outputID int64,
	reason int32, traceID string) (int64, error) {
	if err := f.db.hit("StreamOutputs.MarkOfflineTx"); err != nil {
		return 0, err
	}
	row, ok := f.db.outputs[outputID]
	if !ok || row.State != model.StreamOutputStateOnline {
		return 0, nil // 0 行不是错误：由 logic 回读归因
	}
	now := nowTS()
	row.State, row.OfflineAt, row.OfflineReason = model.StreamOutputStateOffline, now, reason
	if traceID != "" {
		row.TraceId = traceID
	}
	row.Mtime = now
	return 1, nil
}

// 未实现的 Upsert / MarkOffline / MarkExpiredOffline 由内嵌 nil 接口提升：
// 前两个是非事务版本（写路径一律走 Tx 变体），MarkExpiredOffline 归定时清扫器，
// 本批 9 个方法都不该碰它们。

// ---------------------------------------------------------------- live_replay_task

// applyReplayPatch 与 model/live_replay_task.go:83-128 的 ReplayPatch.sets() 逐列同构。
// sets() 是 model 包私有的，fake 只能复刻一份，因此把两处最容易想当然的口径写出来：
//   - nil 指针 = 本次不更新这一列（Worker 心跳常带零值，"0" 不等于"清空"）；
//   - output_bucket / output_key / bvid / trace_id 还额外要求非空串：
//     Patch 根本表达不出「把产物引用清空」，fake 若把空串当覆盖值写入，
//     就会造出一条生产 SQL 永远写不出的行，"清空引用"这类用例因此毫无意义；
//   - err_msg / errno / reason 反过来：给了空串也照写（失败摘要被清空是真实语义）。
func applyReplayPatch(row *model.LiveReplayTask, p model.ReplayPatch) {
	if p.SegmentCount != nil {
		row.SegmentCount = *p.SegmentCount
	}
	if p.GapCount != nil {
		row.GapCount = *p.GapCount
	}
	if p.DurationMs != nil {
		row.DurationMs = *p.DurationMs
	}
	if p.StartAt != nil {
		row.StartAt = *p.StartAt
	}
	if p.EndAt != nil {
		row.EndAt = *p.EndAt
	}
	if p.OutputBucket != nil && *p.OutputBucket != "" {
		row.OutputBucket = *p.OutputBucket
	}
	if p.OutputKey != nil && *p.OutputKey != "" {
		row.OutputKey = *p.OutputKey
	}
	if p.AssetId != nil {
		row.AssetId = *p.AssetId
	}
	if p.Aid != nil {
		row.Aid = *p.Aid
	}
	if p.Bvid != nil && *p.Bvid != "" {
		row.Bvid = *p.Bvid
	}
	if p.Reason != nil {
		row.Reason = *p.Reason
	}
	if p.Errno != nil {
		row.Errno = *p.Errno
	}
	if p.ErrMsg != nil {
		row.ErrMsg = *p.ErrMsg
	}
	if p.TraceID != nil && *p.TraceID != "" {
		row.TraceId = *p.TraceID
	}
}

type fakeReplayTasks struct {
	model.LiveReplayTaskModel
	db *store
}

func (f fakeReplayTasks) Insert(_ context.Context, t *model.LiveReplayTask) (int64, error) {
	return f.insert("ReplayTasks.Insert", t)
}

func (f fakeReplayTasks) InsertTx(_ context.Context, _ sqlx.Session,
	t *model.LiveReplayTask) (int64, error) {
	return f.insert("ReplayTasks.InsertTx", t)
}

// insert 复刻 live_replay_task.go:170-207 三条口径：
//  1. 默认值补在**调用方传进来的结构体**上（Ctime/Mtime/Version），
//     所以 SubmitReplayTask 之后再用同一个 row 拼别的语句时能看到这些值；
//  2. 主键只 return，不回写 ReplayId（缺陷 #2 那一轮已确认过的口径）：
//     logic 若拿 row.ReplayId 回读就会读到 0；
//  3. allow_gaps 落库前被折算成 0/1（tinyint），传 7 也只存 1。
func (f fakeReplayTasks) insert(op string, t *model.LiveReplayTask) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	now := nowTS()
	if t.Ctime == 0 {
		t.Ctime = now
	}
	if t.Mtime == 0 {
		t.Mtime = now
	}
	if t.Version == 0 {
		t.Version = 1
	}
	allow := int32(0)
	if t.AllowGaps != 0 {
		allow = 1
	}
	for _, row := range f.db.replays {
		if row.RequestId == t.RequestId {
			return 0, fmt.Errorf("request_id=%s: %w", t.RequestId, model.ErrRequestIdDuplicated)
		}
	}
	tried := *t
	f.db.triedReplays = append(f.db.triedReplays, &tried)
	id := f.db.next("replay")
	row := *t
	row.ReplayId, row.AllowGaps = id, allow
	f.db.replays[id] = &row
	return id, nil
}

func (f fakeReplayTasks) FindOne(_ context.Context, replayID int64) (*model.LiveReplayTask, error) {
	if err := f.db.hit("ReplayTasks.FindOne"); err != nil {
		return nil, err
	}
	if replayID <= 0 || f.db.missed("ReplayTasks.FindOne") {
		return nil, nil // WHERE replay_id = ?：查不到，但不是错误
	}
	row, ok := f.db.replays[replayID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeReplayTasks) FindByRequestID(_ context.Context,
	requestID string) (*model.LiveReplayTask, error) {
	if err := f.db.hit("ReplayTasks.FindByRequestID"); err != nil {
		return nil, err
	}
	if requestID == "" || f.db.missed("ReplayTasks.FindByRequestID") {
		return nil, nil
	}
	for _, row := range f.db.replays {
		if row.RequestId == requestID {
			c := *row
			return &c, nil
		}
	}
	return nil, nil
}

// FindByRecordAndRange 与真 SQL 同式（live_replay_task.go:233-246）：
// 排除 COMPLETED/FAILED/CANCELLED 三个终态 —— 失败区间允许重投新任务（产新稿件），
// 未终态区间则复用，避免同一段录像产出两份稿件。ORDER BY replay_id DESC LIMIT 1。
func (f fakeReplayTasks) FindByRecordAndRange(_ context.Context, recordID, fromSeq,
	toSeq int64) (*model.LiveReplayTask, error) {
	if err := f.db.hit("ReplayTasks.FindByRecordAndRange"); err != nil {
		return nil, err
	}
	if f.db.missed("ReplayTasks.FindByRecordAndRange") {
		return nil, nil
	}
	var best *model.LiveReplayTask
	for _, row := range f.db.replays {
		if row.RecordId != recordID || row.FromSeq != fromSeq || row.ToSeq != toSeq {
			continue
		}
		if model.IsReplayTerminal(row.State) {
			continue
		}
		if best == nil || row.ReplayId > best.ReplayId {
			c := *row
			best = &c
		}
	}
	return best, nil
}

// FindByAssetID 保留 model 的前置拒绝：asset_id<=0 报 ErrInvalidAssetID。
// 0 是「未登记媒资」的占位值，若当过滤条件会把所有未绑定任务都命中。
func (f fakeReplayTasks) FindByAssetID(_ context.Context, assetID int64) (*model.LiveReplayTask, error) {
	if err := f.db.hit("ReplayTasks.FindByAssetID"); err != nil {
		return nil, err
	}
	if assetID <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	if f.db.missed("ReplayTasks.FindByAssetID") {
		return nil, nil
	}
	var best *model.LiveReplayTask
	for _, row := range f.db.replays {
		if row.AssetId != assetID {
			continue
		}
		if best == nil || row.ReplayId > best.ReplayId {
			c := *row
			best = &c
		}
	}
	return best, nil
}

func (f fakeReplayTasks) List(_ context.Context,
	ff model.ReplayTaskFilter) ([]*model.LiveReplayTask, int32, error) {
	if err := f.db.hit("ReplayTasks.List"); err != nil {
		return nil, 0, err
	}
	cp := ff
	f.db.lastReplayList = &cp
	var rows []*model.LiveReplayTask
	for _, row := range f.db.replays {
		if ff.RoomId > 0 && row.RoomId != ff.RoomId {
			continue
		}
		if ff.SessionId > 0 && row.LiveSession != ff.SessionId {
			continue
		}
		if ff.RecordId > 0 && row.RecordId != ff.RecordId {
			continue
		}
		if ff.State > 0 && row.State != ff.State {
			continue
		}
		if ff.AnchorMid > 0 && row.AnchorMid != ff.AnchorMid {
			continue
		}
		c := *row
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ReplayId > rows[j].ReplayId })
	total := int32(len(rows))
	if total == 0 {
		return nil, 0, nil // COUNT=0 时真实现短路返回，不再发第二条 SELECT
	}
	limit, offset := fakePage(ff.Pn, ff.Ps, ff.MaxPageSize)
	return slicePage(rows, limit, offset), total, nil
}

func (f fakeReplayTasks) UpdateState(_ context.Context, replayID int64, fromStates []int32,
	expectedVersion int64, p model.ReplayPatch) (int64, error) {
	return f.update("ReplayTasks.UpdateState", replayID, fromStates, expectedVersion, p)
}

func (f fakeReplayTasks) UpdateStateTx(_ context.Context, _ sqlx.Session, replayID int64,
	fromStates []int32, expectedVersion int64, p model.ReplayPatch) (int64, error) {
	return f.update("ReplayTasks.UpdateStateTx", replayID, fromStates, expectedVersion, p)
}

// update 复刻 conditionalUpdate(bumpVersion=true)：
// WHERE replay_id=? AND state IN (?) [AND version=?] SET <patch>, state=?, version=version+1, mtime=now。
// fromStates 为空是 model 的前置拒绝（不是 0 行）；行不存在/条件不命中都是 0 行而不是错误。
func (f fakeReplayTasks) update(op string, replayID int64, fromStates []int32, expectedVersion int64,
	p model.ReplayPatch) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_replay_task UpdateState: empty fromStates %w", model.ErrInvalidTransition)
	}
	row, ok := f.db.replays[replayID]
	if !ok || !containsState(fromStates, row.State) {
		return 0, nil
	}
	if expectedVersion > 0 && row.Version != expectedVersion {
		return 0, nil
	}
	applyReplayPatch(row, p)
	row.State = p.State
	row.Version++
	row.Mtime = nowTS()
	return 1, nil
}

// ---------------------------------------------------------------- live_replay_asset_ref

type fakeReplayAssetRefs struct {
	model.LiveReplayAssetRefModel
	db *store
}

// refConflictRow 按 uniq_replay_id → uniq_asset_id → uniq_aid 的顺序找第一个被唯一键挡到的行。
// 真库里的命中顺序由 InnoDB 决定，测试不依赖它：唯一键冲突的三种后果都会撞上
// 「要么改到别人的行、要么报冲突」这两类结局之一，这里取前者（见 upsert 的注释）。
func (f fakeReplayAssetRefs) refConflictRow(r *model.LiveReplayAssetRef) *model.LiveReplayAssetRef {
	for _, row := range f.db.replayRefs {
		if row.ReplayId == r.ReplayId {
			return row
		}
	}
	for _, row := range f.db.replayRefs {
		if row.AssetId == r.AssetId {
			return row
		}
	}
	for _, row := range f.db.replayRefs {
		if row.Aid == r.Aid {
			return row
		}
	}
	return nil
}

func (f fakeReplayAssetRefs) Upsert(ctx context.Context, r *model.LiveReplayAssetRef) (int64, error) {
	return f.upsert(ctx, "ReplayRefs.Upsert", r)
}

func (f fakeReplayAssetRefs) UpsertTx(ctx context.Context, _ sqlx.Session,
	r *model.LiveReplayAssetRef) (int64, error) {
	return f.upsert(ctx, "ReplayRefs.UpsertTx", r)
}

// upsert 逐条对齐 live_replay_asset_ref.go:120-176，其中两条是本轮用例的关键：
//
//  1. 「命中唯一键 → 改哪一列」严格按 ON DUPLICATE 的更新列表来：
//     asset_id / aid / replay_id / ctime 与五个投影列（review_state、review_state_at、
//     published_at、last_event_id、source）都不在列表里，因此重绑**不会**把投影抹掉，
//     也不会把引用挪到别的回放/媒资上；retention_state 同样不动（生命周期归回收流程）。
//  2. ON DUPLICATE KEY UPDATE 命中唯一键时 MySQL 不返回 1062 —— 所以 model 里
//     isDuplicateErr → ErrAssetRefConflict 那个分支对这三个唯一键是不可达的，
//     「媒资被别的回放认领」在这里表现为「别人的引用行被本请求的产物字段改写」，
//     而不是 ErrAssetRefConflict。fake 若替 model 报冲突，就会把这个缺口藏起来，
//     所以这里照 MySQL 的行为写；主键返回值照 model 的读侧口径走 replay_id 回读
//     （注释原文：ON DUPLICATE 时 LastInsertId 不可信），回读不到即 ErrReplayRefNotFound。
func (f fakeReplayAssetRefs) upsert(ctx context.Context, op string,
	r *model.LiveReplayAssetRef) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	if r.ReplayId <= 0 {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: replay_id=%d %w", r.ReplayId,
			model.ErrInvalidTransition)
	}
	if r.AssetId <= 0 {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: asset_id=%d %w", r.AssetId,
			model.ErrInvalidAssetID)
	}
	if r.Aid <= 0 {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: aid=%d %w", r.Aid, model.ErrInvalidAid)
	}
	if r.Bucket == "" || r.ObjectKey == "" {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: replay_id=%d %w", r.ReplayId,
			model.ErrInvalidBucketRef)
	}
	now := nowTS()
	if r.ReviewState == 0 {
		// 绑定时刻拿不到 video 结论：投影保持未同步（真实现同样改写调用方的结构体）。
		r.ReviewState = model.ReviewStateUnsynced
		r.ReviewStateAt = 0
	}
	tried := *r
	f.db.triedReplayRefs = append(f.db.triedReplayRefs, &tried)

	if row := f.refConflictRow(r); row != nil {
		row.RoomId, row.LiveSession, row.RecordId = r.RoomId, r.LiveSession, r.RecordId
		row.Bvid, row.AnchorMid = r.Bvid, r.AnchorMid
		row.Bucket, row.ObjectKey = r.Bucket, r.ObjectKey
		row.DurationMs = r.DurationMs
		row.SegmentFromSeq, row.SegmentToSeq, row.GapCount = r.SegmentFromSeq, r.SegmentToSeq, r.GapCount
		row.RequestId, row.TraceId, row.Mtime = r.RequestId, r.TraceId, now
		existing, err := f.FindByReplayID(ctx, r.ReplayId)
		if err != nil {
			return 0, err
		}
		if existing == nil {
			return 0, fmt.Errorf("live_replay_asset_ref Upsert: replay_id=%d %w", r.ReplayId,
				model.ErrReplayRefNotFound)
		}
		return existing.Id, nil
	}
	id := f.db.next("ref")
	row := *r
	row.Id, row.Ctime, row.Mtime = id, now, now
	f.db.replayRefs[id] = &row
	return id, nil
}

func (f fakeReplayAssetRefs) FindOne(_ context.Context, id int64) (*model.LiveReplayAssetRef, error) {
	if err := f.db.hit("ReplayRefs.FindOne"); err != nil {
		return nil, err
	}
	if id <= 0 || f.db.missed("ReplayRefs.FindOne") {
		return nil, nil // WHERE id = ?：查不到，但不是错误
	}
	row, ok := f.db.replayRefs[id]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeReplayAssetRefs) FindByReplayID(_ context.Context,
	replayID int64) (*model.LiveReplayAssetRef, error) {
	if err := f.db.hit("ReplayRefs.FindByReplayID"); err != nil {
		return nil, err
	}
	if replayID <= 0 || f.db.missed("ReplayRefs.FindByReplayID") {
		return nil, nil
	}
	for _, row := range f.db.replayRefs {
		if row.ReplayId == replayID {
			c := *row
			return &c, nil
		}
	}
	return nil, nil
}

// FindByAssetID / FindByAid 保留 model 的前置拒绝（<=0 报哨兵）：
// 0 是「未登记媒资 / 未建稿」的占位值，当反查条件会把所有未绑定引用都命中。
func (f fakeReplayAssetRefs) FindByAssetID(_ context.Context,
	assetID int64) (*model.LiveReplayAssetRef, error) {
	if err := f.db.hit("ReplayRefs.FindByAssetID"); err != nil {
		return nil, err
	}
	if assetID <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	return f.findByColumn("ReplayRefs.FindByAssetID", "asset_id", assetID), nil
}

func (f fakeReplayAssetRefs) FindByAid(_ context.Context, aid int64) (*model.LiveReplayAssetRef, error) {
	if err := f.db.hit("ReplayRefs.FindByAid"); err != nil {
		return nil, err
	}
	if aid <= 0 {
		return nil, model.ErrInvalidAid
	}
	return f.findByColumn("ReplayRefs.FindByAid", "aid", aid), nil
}

// findByColumn 的 op 参数必须是 hit 用过的同一个键名，否则 missOnce 挂不上。
func (f fakeReplayAssetRefs) findByColumn(op, which string, v int64) *model.LiveReplayAssetRef {
	if f.db.missed(op) {
		return nil
	}
	for _, row := range f.db.replayRefs {
		if (which == "asset_id" && row.AssetId == v) || (which == "aid" && row.Aid == v) {
			c := *row
			return &c
		}
	}
	return nil
}

func (f fakeReplayAssetRefs) List(_ context.Context,
	ff model.ReplayRefFilter) ([]*model.LiveReplayAssetRef, int32, error) {
	if err := f.db.hit("ReplayRefs.List"); err != nil {
		return nil, 0, err
	}
	cp := ff
	f.db.lastReplayRefList = &cp
	var rows []*model.LiveReplayAssetRef
	for _, row := range f.db.replayRefs {
		if ff.RoomId > 0 && row.RoomId != ff.RoomId {
			continue
		}
		if ff.SessionId > 0 && row.LiveSession != ff.SessionId {
			continue
		}
		if ff.AnchorMid > 0 && row.AnchorMid != ff.AnchorMid {
			continue
		}
		if ff.ReviewState > 0 && row.ReviewState != ff.ReviewState {
			continue
		}
		if ff.OnlyUnsynced && row.ReviewState != model.ReviewStateUnsynced {
			continue
		}
		c := *row
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Id > rows[j].Id })
	total := int32(len(rows))
	if total == 0 {
		return nil, 0, nil
	}
	limit, offset := fakePage(ff.Pn, ff.Ps, ff.MaxPageSize)
	return slicePage(rows, limit, offset), total, nil
}

func (f fakeReplayAssetRefs) ApplyContentState(ctx context.Context,
	in model.ContentStateApply) (int64, error) {
	return f.apply(ctx, "ReplayRefs.ApplyContentState", in)
}

func (f fakeReplayAssetRefs) ApplyContentStateTx(ctx context.Context, _ sqlx.Session,
	in model.ContentStateApply) (int64, error) {
	return f.apply(ctx, "ReplayRefs.ApplyContentStateTx", in)
}

// apply 是「video → live-media 单向投影」这条通道的内存形态，
// 三条 SQL 语义都必须照抄，否则投影边界的用例就是空的：
//   - SET 列表**只有** review_state / review_state_at / published_at / last_event_id / source
//     （+ trace_id 非空时），replay_id / asset_id / aid / bucket / object_key / duration_ms
//     以及 live_replay_task 的任何列都不在这里，改不到；
//   - published_at 用 GREATEST 保证单调（撤销发布由 review_state 表达，本地时间戳只记首次）；
//   - WHERE 里 last_event_id <> eventID 挡同事件重放、review_state_at <= stateAt 挡旧事件覆盖，
//     两条都命中 0 行而不是错误。
func (f fakeReplayAssetRefs) apply(_ context.Context, op string,
	in model.ContentStateApply) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	if !model.ValidReviewState(in.ReviewState) {
		return 0, fmt.Errorf("live_replay_asset_ref ApplyContentState: review_state=%d %w",
			in.ReviewState, model.ErrInvalidReviewState)
	}
	if in.EventId == "" {
		return 0, model.ErrEmptyEventID
	}
	var row *model.LiveReplayAssetRef
	switch {
	case in.ReplayId > 0:
		for _, r := range f.db.replayRefs {
			if r.ReplayId == in.ReplayId {
				c := r
				row = c
			}
		}
	case in.AssetId > 0:
		for _, r := range f.db.replayRefs {
			if r.AssetId == in.AssetId {
				c := r
				row = c
			}
		}
	default:
		// 两个键都没给：宁可报错也不能退化成全表刷新投影（live_replay_asset_ref.go:287）。
		return 0, model.ErrReplayRefNotFound
	}
	if row == nil {
		return 0, nil // WHERE 不命中：0 行，由 logic 回读归因
	}
	stateAt := in.StateAt
	if stateAt <= 0 {
		stateAt = nowTS()
	}
	if row.LastEventId == in.EventId || row.ReviewStateAt > stateAt {
		return 0, nil // 同事件重放 / 旧事件被更新的投影挡住
	}
	row.ReviewState = in.ReviewState
	row.ReviewStateAt = stateAt
	if in.PublishedAt > row.PublishedAt { // GREATEST(published_at, ?)
		row.PublishedAt = in.PublishedAt
	}
	row.LastEventId, row.Source, row.Mtime = in.EventId, in.Source, nowTS()
	if in.TraceId != "" {
		row.TraceId = in.TraceId
	}
	return 1, nil
}

func (f fakeReplayAssetRefs) MarkRetentionState(ctx context.Context, id int64,
	from, to int32) (int64, error) {
	return f.markRetention(ctx, "ReplayRefs.MarkRetentionState", id, from, to)
}

func (f fakeReplayAssetRefs) MarkRetentionStateTx(ctx context.Context, _ sqlx.Session, id int64,
	from, to int32) (int64, error) {
	return f.markRetention(ctx, "ReplayRefs.MarkRetentionStateTx", id, from, to)
}

// markRetention 前置校验与真实现同一条（ValidRefRetentionState + IsValidRefRetentionTransition），
// 不在 fake 里另写一套生命周期表。0 行按 MySQL 的 changed-rows 语义给：
// from==to 时值没变，受影响行数是 0（logic 正是按「0 行 = 生命周期已被推进」幂等处理的）。
func (f fakeReplayAssetRefs) markRetention(_ context.Context, op string, id int64,
	from, to int32) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	if !model.ValidRefRetentionState(from) || !model.IsValidRefRetentionTransition(from, to) {
		return 0, fmt.Errorf("live_replay_asset_ref MarkRetentionState: %d->%d %w", from, to,
			model.ErrInvalidTransition)
	}
	row, ok := f.db.replayRefs[id]
	if !ok || row.RetentionState != from || row.RetentionState == to {
		return 0, nil
	}
	row.RetentionState = to
	row.Mtime = nowTS()
	return 1, nil
}

// ---------------------------------------------------------------- live_retention_task

// applyRetentionPatch 与 model/live_retention_task.go:66-90 的 RetentionPatch.sets() 逐列同构。
// sets() 是 model 包私有的，fake 只能复刻，两条最容易被想当然的口径写在这里：
//   - nil 指针 = 本次不更新这一列（计数用「覆盖写」表达，缺列才是「不动」）；
//   - trace_id 还额外要求非空串：Patch 表达不出「清空 trace_id」，
//     fake 若把空串当覆盖值写进去，就会造出一条生产 SQL 永远写不出的行；
//   - err_msg 反过来：给了空串也照写（失败摘要被清空是真实语义，见 SUCCEEDED 边清痕）。
func applyRetentionPatch(row *model.LiveRetentionTask, p model.RetentionPatch) {
	if p.Scanned != nil {
		row.Scanned = *p.Scanned
	}
	if p.Deleted != nil {
		row.Deleted = *p.Deleted
	}
	if p.Skipped != nil {
		row.Skipped = *p.Skipped
	}
	if p.FailReason != nil {
		row.FailReason = *p.FailReason
	}
	if p.Errno != nil {
		row.Errno = *p.Errno
	}
	if p.ErrMsg != nil {
		row.ErrMsg = *p.ErrMsg
	}
	if p.TraceID != nil && *p.TraceID != "" {
		row.TraceId = *p.TraceID
	}
}

type fakeRetentionTasks struct {
	model.LiveRetentionTaskModel
	db *store
}

func (f fakeRetentionTasks) Insert(_ context.Context, t *model.LiveRetentionTask) (int64, error) {
	return f.insert("RetentionTasks.Insert", t)
}

func (f fakeRetentionTasks) InsertTx(_ context.Context, _ sqlx.Session,
	t *model.LiveRetentionTask) (int64, error) {
	return f.insert("RetentionTasks.InsertTx", t)
}

// insert 复刻 live_retention_task.go:130-175 四条口径，前三条都是「logic 藏不住」的细节：
//  1. 三条前置拒绝（target_kind 非法 / 既无 target_id 又无 expire_before / reason 为空）
//     返回的是**错误**而不是 0 行，且发生在自增之前；logic 若以为自己能兜住就会登记空转任务；
//  2. scanned/deleted/skipped 在 VALUES 里是字面量 0（live_retention_task.go:161）：
//     登记时给这三个计数赋值是无效的，只有上报路径（UpdateStateTx）能写它们；
//  3. 主键只 return、不回写 t.RetentionId（与另两张表同一口径，缺陷 #2 那轮确认过）；
//  4. purge 落库前折成 0/1，默认值补在调用方的结构体上。
func (f fakeRetentionTasks) insert(op string, t *model.LiveRetentionTask) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	if !model.ValidRetentionTarget(t.TargetKind) {
		return 0, fmt.Errorf("live_retention_task Insert: target_kind=%d %w", t.TargetKind,
			model.ErrInvalidTransition)
	}
	if t.TargetId <= 0 && t.ExpireBefore <= 0 {
		return 0, model.ErrRetentionTargetRequired
	}
	if t.Reason == "" {
		return 0, model.ErrRetentionReasonRequired
	}
	now := nowTS()
	if t.Ctime == 0 {
		t.Ctime = now
	}
	if t.Mtime == 0 {
		t.Mtime = now
	}
	if t.Version == 0 {
		t.Version = 1
	}
	if t.State == 0 {
		t.State = model.RetentionStatePending
	}
	purge := int32(0)
	if t.Purge != 0 {
		purge = 1
	}
	for _, row := range f.db.retentions {
		if row.RequestId == t.RequestId {
			return 0, fmt.Errorf("request_id=%s: %w", t.RequestId, model.ErrRequestIdDuplicated)
		}
	}
	tried := *t
	f.db.triedRetentions = append(f.db.triedRetentions, &tried)
	id := f.db.next("retention")
	row := *t
	row.RetentionId, row.Purge = id, purge
	row.Scanned, row.Deleted, row.Skipped = 0, 0, 0 // INSERT 语句里是字面量 0
	f.db.retentions[id] = &row
	return id, nil
}

func (f fakeRetentionTasks) FindOne(_ context.Context,
	retentionID int64) (*model.LiveRetentionTask, error) {
	if err := f.db.hit("RetentionTasks.FindOne"); err != nil {
		return nil, err
	}
	if retentionID <= 0 || f.db.missed("RetentionTasks.FindOne") {
		return nil, nil // WHERE retention_id = ?：查不到，但不是错误
	}
	row, ok := f.db.retentions[retentionID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeRetentionTasks) FindByRequestID(_ context.Context,
	requestID string) (*model.LiveRetentionTask, error) {
	if err := f.db.hit("RetentionTasks.FindByRequestID"); err != nil {
		return nil, err
	}
	if requestID == "" || f.db.missed("RetentionTasks.FindByRequestID") {
		return nil, nil
	}
	for _, row := range f.db.retentions {
		if row.RequestId == requestID {
			c := *row
			return &c, nil
		}
	}
	return nil, nil
}

// FindUnfinishedByTarget 与真 SQL 同式（live_retention_task.go:201-215）：
// target_id<=0 的批量任务在 model 层就被拒绝（它们的集合语义允许重叠，不能拿来复用）；
// 只认 PENDING/RUNNING 两个未完成态，ORDER BY retention_id DESC LIMIT 1。
func (f fakeRetentionTasks) FindUnfinishedByTarget(_ context.Context, targetKind,
	targetID int64) (*model.LiveRetentionTask, error) {
	if err := f.db.hit("RetentionTasks.FindUnfinishedByTarget"); err != nil {
		return nil, err
	}
	if targetID <= 0 || f.db.missed("RetentionTasks.FindUnfinishedByTarget") {
		return nil, nil
	}
	var best *model.LiveRetentionTask
	for _, row := range f.db.retentions {
		if int64(row.TargetKind) != targetKind || row.TargetId != targetID {
			continue
		}
		if row.State != model.RetentionStatePending && row.State != model.RetentionStateRunning {
			continue
		}
		if best == nil || row.RetentionId > best.RetentionId {
			c := *row
			best = &c
		}
	}
	return best, nil
}

func (f fakeRetentionTasks) List(_ context.Context,
	ff model.RetentionFilter) ([]*model.LiveRetentionTask, int32, error) {
	if err := f.db.hit("RetentionTasks.List"); err != nil {
		return nil, 0, err
	}
	cp := ff
	f.db.lastRetentionList = &cp
	var rows []*model.LiveRetentionTask
	for _, row := range f.db.retentions {
		if ff.TargetKind > 0 && row.TargetKind != ff.TargetKind {
			continue
		}
		if ff.State > 0 && row.State != ff.State {
			continue
		}
		if ff.RoomId > 0 && row.RoomId != ff.RoomId {
			continue
		}
		c := *row
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RetentionId > rows[j].RetentionId })
	total := int32(len(rows))
	if total == 0 {
		return nil, 0, nil // COUNT=0 时真实现短路返回，不再发第二条 SELECT
	}
	limit, offset := fakePage(ff.Pn, ff.Ps, ff.MaxPageSize)
	return slicePage(rows, limit, offset), total, nil
}

// ListByState 是 Worker 领取队列的入口（retention_id ASC，先登记先执行），
// rpc 未暴露（见 ListRetentionTasks 的注释），本批 logic 用例打不到它；
// 但接口要有实现，否则内嵌 nil 接口的 fake 一旦被调用就是 panic，反而藏住了缺口。
func (f fakeRetentionTasks) ListByState(_ context.Context, state int32,
	limit int32) ([]*model.LiveRetentionTask, error) {
	if err := f.db.hit("RetentionTasks.ListByState"); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	var rows []*model.LiveRetentionTask
	for _, row := range f.db.retentions {
		if row.State != state {
			continue
		}
		c := *row
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RetentionId < rows[j].RetentionId })
	if int32(len(rows)) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (f fakeRetentionTasks) UpdateState(_ context.Context, retentionID int64, fromStates []int32,
	expectedVersion int64, p model.RetentionPatch) (int64, error) {
	return f.update("RetentionTasks.UpdateState", retentionID, fromStates, expectedVersion, p)
}

func (f fakeRetentionTasks) UpdateStateTx(_ context.Context, _ sqlx.Session, retentionID int64,
	fromStates []int32, expectedVersion int64, p model.RetentionPatch) (int64, error) {
	return f.update("RetentionTasks.UpdateStateTx", retentionID, fromStates, expectedVersion, p)
}

// update 复刻 conditionalUpdate(bumpVersion=true)（live_retention_task.go:263-282）：
// WHERE retention_id=? AND state IN (?) [AND version=?] SET <patch>, state=?, version=version+1, mtime=now。
// fromStates 为空是 model 的前置拒绝（不是 0 行）；行不存在/条件不命中都是 0 行而不是错误。
func (f fakeRetentionTasks) update(op string, retentionID int64, fromStates []int32,
	expectedVersion int64, p model.RetentionPatch) (int64, error) {
	if err := f.db.hit(op); err != nil {
		return 0, err
	}
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_retention_task UpdateState: empty fromStates %w",
			model.ErrInvalidTransition)
	}
	row, ok := f.db.retentions[retentionID]
	if !ok || !containsState(fromStates, row.State) {
		return 0, nil
	}
	if expectedVersion > 0 && row.Version != expectedVersion {
		return 0, nil
	}
	applyRetentionPatch(row, p)
	row.State = p.State
	row.Version++
	row.Mtime = nowTS()
	return 1, nil
}

// ---------------------------------------------------------------- 分页口径（与 model.clampPage 同式）

func fakePage(pn, ps, maxPS int32) (limit, offset int32) {
	if maxPS <= 0 {
		maxPS = 50
	}
	if pn < 1 {
		pn = 1
	}
	switch {
	case ps <= 0 || ps > maxPS:
		limit = 20
		if limit > maxPS {
			limit = maxPS
		}
	default:
		limit = ps
	}
	return limit, (pn - 1) * limit
}

func slicePage[T any](rows []T, limit, offset int32) []T {
	start := int(offset)
	if start > len(rows) {
		start = len(rows)
	}
	rows = rows[start:]
	if int(limit) < len(rows) {
		rows = rows[:limit]
	}
	return rows
}

// ---------------------------------------------------------------- 组装

// testConf 与 etc/live-media.yaml 同口径的显式默认值：
// 单测不走 conf.Load，因此 json 标签里的 default 必须在这里手写出来，
// 否则「配置缺失时的行为」会被误测成「配置生效时的行为」。
func testConf() config.LiveMediaConf {
	return config.LiveMediaConf{
		DefaultTranscodeTimeoutSeconds: 60,
		DefaultRecordTimeoutSeconds:    90,
		DefaultMaxAttempts:             3,
		DefaultRecordSegmentSeconds:    10,
		MaxRecordSegmentSeconds:        60,
		MaxSegmentPageSize:             500,
		DefaultSegmentPageSize:         200,
		MaxReplayGapSegments:           0,
		ReplayTitleMaxLength:           80,
		DefaultRetentionBatchLimit:     100,
		MaxRetentionBatchLimit:         500,
		MaxListPageSize:                50,
		TaskCacheTTLSeconds:            60,
		StreamOutputCacheTTLSeconds:    15,
		TaskTimeoutSweepEnabled:        false,
	}
}

// newTestSvc 装配一个只依赖内存的 ServiceContext：
// Cache 恒为 nil（读侧必然回源），三个下游 zrpc client 恒为 nil（本批方法不得触碰）。
func newTestSvc(db *store) *svc.ServiceContext {
	cfg := config.Config{}
	cfg.Name = "livemedia-test"
	cfg.LiveMedia = testConf()
	return &svc.ServiceContext{
		Config:         cfg,
		DB:             fakeConn{db: db},
		TranscodeTasks: fakeTranscodeTasks{db: db},
		RecordTasks:    fakeRecordTasks{db: db},
		Segments:       fakeSegments{db: db},
		StreamOutputs:  fakeStreamOutputs{db: db},
		ReplayTasks:    fakeReplayTasks{db: db},
		ReplayRefs:     fakeReplayAssetRefs{db: db},
		RetentionTasks: fakeRetentionTasks{db: db},
		Outbox:         fakeOutbox{db: db},
	}
}

// ---------------------------------------------------------------- 种子数据

// seedTranscode 直接落一行转码任务（绕开 Start*，让状态机用例能从任意状态起跑）。
func seedTranscode(db *store, state int32, mutate func(*model.LiveTranscodeTask)) *model.LiveTranscodeTask {
	now := nowTS()
	row := &model.LiveTranscodeTask{
		TaskId: db.next("task"), RoomId: testRoomID, LiveSession: testSession,
		TemplateId: testTemplate, BitrateLevel: testLevel,
		Protocol: testProtocol, SourceRef: "rtmp://origin.example.com/live/room71001",
		AnchorMid: testAnchor, State: state, MaxAttempts: 3,
		HeartbeatAt: now, TimeoutAt: now + 60, Version: 1,
		RequestId: "seed-task-" + strconv.FormatInt(now, 10) + "-" + strconv.FormatInt(int64(len(db.transcodes)+1), 10),
		Ctime:     now - 100, Mtime: now - 100,
	}
	if mutate != nil {
		mutate(row)
	}
	// 入库的是副本：返回给测试的指针因此是「种子时刻的快照」，不会被 logic 的写改写。
	stored := *row
	db.transcodes[stored.TaskId] = &stored
	return row
}

// seedRecord 直接落一行录制任务。
func seedRecord(db *store, state int32, mutate func(*model.LiveRecordTask)) *model.LiveRecordTask {
	now := nowTS()
	row := &model.LiveRecordTask{
		RecordId: db.next("record"), RoomId: testRoomID, LiveSession: testSession,
		State: state, SegmentSeconds: 10, OutputBucket: "live-rec", OutputPrefix: "rec/71001",
		HeartbeatAt: now, TimeoutAt: now + 90, Version: 1,
		RequestId: "seed-rec-" + strconv.FormatInt(now, 10) + "-" + strconv.FormatInt(int64(len(db.records)+1), 10),
		Ctime:     now - 100, Mtime: now - 100,
	}
	if mutate != nil {
		mutate(row)
	}
	stored := *row
	db.records[stored.RecordId] = &stored
	return row
}

// seedSegment 落一行切片（seq 必填）。
func seedSegment(db *store, recordID, seq int64, state int32, mutate func(*model.LiveRecordSegment)) *model.LiveRecordSegment {
	now := nowTS()
	seg := &model.LiveRecordSegment{
		RecordId: recordID, RoomId: testRoomID, LiveSession: testSession, Seq: seq,
		StartAt: now + (seq-1)*10, EndAt: now + seq*10, DurationMs: 10000, State: state,
		Bucket: "live-rec", ObjectKey: "rec/71001/" + strconv.FormatInt(seq, 10) + ".m3u8",
		SizeBytes: 1 << 20, WorkerId: "worker-a",
	}
	if mutate != nil {
		mutate(seg)
	}
	return db.putSegment(seg)
}

// countSegments 某录制任务的切片行数（含缺口行）。
func countSegments(db *store, recordID int64) int { return len(db.segments[recordID]) }

// seedOutput 直接落一行分发档位（绕开 UpsertStreamOutput）：
// 「已下线的历史行」「同档位两场同时在线的异常数据」都是 Upsert 本身造不出来的前置态，
// 前者来自下线，后者只可能来自数据迁移或人工修表，但恰恰是 FindOnlineByLevel 必须拒绝的形态。
func seedOutput(db *store, state int32, mutate func(*model.LiveStreamOutput)) *model.LiveStreamOutput {
	now := nowTS()
	row := &model.LiveStreamOutput{
		OutputId: db.next("output"), RoomId: testRoomID, LiveSession: testSession,
		BitrateLevel: testLevel, Protocol: testProtocol,
		Bucket: "live-hls", ObjectKey: "live/71001/88001/hd.m3u8", CdnDomain: "pull.example.com",
		Width: 1920, Height: 1080, BitrateKbps: 6000, Fps: 30,
		State: state, OnlineAt: now - 300,
		RequestId: "seed-output-" + strconv.FormatInt(now, 10) + "-" + strconv.FormatInt(int64(len(db.outputs)+1), 10),
		TraceId:   "trace-seed-output",
		Ctime:     now - 400, Mtime: now - 400,
	}
	if state == model.StreamOutputStateOffline {
		row.OfflineAt, row.OfflineReason = now-50, model.ReasonSourceLost
	}
	if mutate != nil {
		mutate(row)
	}
	stored := *row
	db.outputs[stored.OutputId] = &stored
	return row
}

// seedReplay 直接落一行回放任务（绕开 SubmitReplayTask）：
// 状态机用例必须能从 MERGING/UPLOADING/REGISTERED/REVIEW_SUBMITTED/终态任意一点起跑，
// 而 SubmitReplayTask 只产出 PENDING。默认区间 [1,5] 与「5 片全有效」配对，
// 这样 ReportReplayProgress 的计数上界判据（replayProgressMonotonic）默认就是合法的。
func seedReplay(db *store, state int32, mutate func(*model.LiveReplayTask)) *model.LiveReplayTask {
	now := nowTS()
	row := &model.LiveReplayTask{
		ReplayId: db.next("replay"), RoomId: testRoomID, LiveSession: testSession,
		RecordId: db.next("record"), State: state,
		FromSeq: 1, ToSeq: 5, SegmentCount: 5, GapCount: 0,
		StartAt: now - 500, EndAt: now - 400,
		AnchorMid: testAnchor, Title: "回放标题", Version: 1,
		RequestId: "seed-replay-" + strconv.FormatInt(now, 10) + "-" + strconv.FormatInt(int64(len(db.replays)+1), 10),
		TraceId:   "trace-seed-replay",
		Ctime:     now - 100, Mtime: now - 100,
	}
	if mutate != nil {
		mutate(row)
	}
	// 入库的是副本：返回给测试的指针因此是「种子时刻的快照」，不会被 logic 的写改写。
	stored := *row
	db.replays[stored.ReplayId] = &stored
	return row
}

// seedReplayRef 直接落一行回放资产引用（绕开 BindReplayAsset）：
// 「投影已同步」「生命周期已进 Pending」这些前置态都是 BindReplayAsset 造不出来的
// （绑定时刻拿不到 video 结论，投影列恒为 0），只有直写才能把投影通道放到起跑线上。
func seedReplayRef(db *store, mutate func(*model.LiveReplayAssetRef)) *model.LiveReplayAssetRef {
	now := nowTS()
	row := &model.LiveReplayAssetRef{
		Id: db.next("ref"), RoomId: testRoomID, LiveSession: testSession,
		ReplayId: db.next("replay"), RecordId: db.next("record"),
		AssetId: 60001, Aid: 70001, Bvid: "BV1replay01", AnchorMid: testAnchor,
		Bucket: "live-replay", ObjectKey: "replay/71001/88001/merged.m3u8",
		DurationMs: 60000, SegmentFromSeq: 1, SegmentToSeq: 5,
		ReviewState: model.ReviewStateUnsynced, RetentionState: model.RefRetentionStateNormal,
		RequestId: "seed-ref-" + strconv.FormatInt(now, 10) + "-" + strconv.FormatInt(int64(len(db.replayRefs)+1), 10),
		TraceId:   "trace-seed-ref",
		Ctime:     now - 100, Mtime: now - 100,
	}
	if mutate != nil {
		mutate(row)
	}
	stored := *row
	db.replayRefs[stored.Id] = &stored
	return row
}

// seedRetention 直接落一行回收任务（绕开 SubmitRetentionTask）：
// 「已被 Worker 认领（RUNNING）」「已进终态并带着计数证据」这些前置态都是
// SubmitRetentionTask 造不出来的（它只会产出 PENDING 且三个计数恒为 0），
// 而上报接口的三条判据（认领后才能进终态、计数不得回退、终态只允许同值重放）
// 恰好只能从这些中点起跑。
// 默认是「按 expire_before 批量扫描」的任务（target_id=0）：定点回收必须显式给出
// 被回收对象主键，用例靠 mutate 填，这里不凭空造一个指向不存在对象的 id。
func seedRetention(db *store, state int32, mutate func(*model.LiveRetentionTask)) *model.LiveRetentionTask {
	now := nowTS()
	row := &model.LiveRetentionTask{
		RetentionId: db.next("retention"), TargetKind: model.RetentionTargetReplay,
		RoomId: testRoomID, ExpireBefore: now - 86400*30,
		Purge: 1, BatchLimit: 100, State: state,
		Reason: "超期清理", Operator: "system", Version: 1,
		RequestId: "seed-retention-" + strconv.FormatInt(now, 10) + "-" + strconv.FormatInt(int64(len(db.retentions)+1), 10),
		TraceId:   "trace-seed-retention",
		Ctime:     now - 100, Mtime: now - 100,
	}
	if mutate != nil {
		mutate(row)
	}
	stored := *row
	db.retentions[stored.RetentionId] = &stored
	return row
}

// errOutboxDown 事件写入故障：用于「Outbox 与业务写同事务、失败必须一起回滚」的用例。
var errOutboxDown = errors.New("fake: live_media_outbox write failed")

// errModelDown 通用 model 读故障。
var errModelDown = errors.New("fake: live_* read failed")

// 编译期确保 fake 满足接口：漏方法在编译期暴露，而不是运行到才 panic。
var (
	_ sqlx.SqlConn                  = fakeConn{}
	_ model.LiveTranscodeTaskModel  = fakeTranscodeTasks{}
	_ model.LiveRecordTaskModel     = fakeRecordTasks{}
	_ model.LiveRecordSegmentModel  = fakeSegments{}
	_ model.LiveStreamOutputModel   = fakeStreamOutputs{}
	_ model.LiveReplayTaskModel     = fakeReplayTasks{}
	_ model.LiveReplayAssetRefModel = fakeReplayAssetRefs{}
	_ model.LiveRetentionTaskModel  = fakeRetentionTasks{}
	_ model.LiveMediaOutboxModel    = fakeOutbox{}
)
