package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go-video/common/eventenvelope"
	"go-video/common/outbox"
	"go-video/services/live-media/model"
)

// --- 替身 ---

// fakeOutboxModel 是 model.LiveMediaOutboxModel 的替身：只实现发布器用到的四个方法，
// 其余方法靠内嵌接口留空（被调用即 panic，能立刻暴露「发布器越权读了别的列或写了事务」）。
//
// 与 internal/logic 里的 fakeOutbox 不同，这里的 Mark* 必须返回 (受影响行数, error)，
// 因为本表的状态方法都是条件 UPDATE：适配层怎么折叠「0 行」正是本轮的判据，
// logic 那份只实现 Insert 的替身覆盖不到。
type fakeOutboxModel struct {
	model.LiveMediaOutboxModel
	rows      []*model.LiveMediaOutbox
	listErr   error
	affected  int64
	failIDs   map[int64]struct{}
	markErr   error
	calls     []string
	now       int64
	limit     int32
	publishAt map[int64]int64
	retryAt   map[int64]int64
	lastError map[int64]string
}

func newFakeOutbox(rows ...*model.LiveMediaOutbox) *fakeOutboxModel {
	return &fakeOutboxModel{
		rows:      rows,
		affected:  1,
		failIDs:   map[int64]struct{}{},
		markErr:   nil,
		publishAt: map[int64]int64{},
		retryAt:   map[int64]int64{},
		lastError: map[int64]string{},
	}
}

func (f *fakeOutboxModel) ListPending(_ context.Context, now int64, limit int32) ([]*model.LiveMediaOutbox, error) {
	f.calls = append(f.calls, "ListPending")
	f.now, f.limit = now, limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

// stateResult 记录一次条件 UPDATE 并给出「受影响行数」，failIDs 里的 id 返回写库错误。
func (f *fakeOutboxModel) stateResult(op string, id int64) (int64, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s:%d", op, id))
	if _, fail := f.failIDs[id]; fail {
		return 0, f.markErr
	}
	return f.affected, nil
}

func (f *fakeOutboxModel) MarkPublished(_ context.Context, id, publishedAt int64) (int64, error) {
	aff, err := f.stateResult("MarkPublished", id)
	f.publishAt[id] = publishedAt
	return aff, err
}

func (f *fakeOutboxModel) MarkRetry(_ context.Context, id, nextRetryAt int64, lastError string) (int64, error) {
	aff, err := f.stateResult("MarkRetry", id)
	f.retryAt[id] = nextRetryAt
	f.lastError[id] = lastError
	return aff, err
}

func (f *fakeOutboxModel) MarkFailed(_ context.Context, id int64, lastError string) (int64, error) {
	aff, err := f.stateResult("MarkFailed", id)
	f.lastError[id] = lastError
	return aff, err
}

// fakeSender 记录每次投递的入参，err 用于制造一次失败以驱动退避路径。
type fakeSender struct {
	mu   sync.Mutex
	sent []sendCall
	err  error
}

type sendCall struct {
	topic   string
	key     string
	payload string
	timeout time.Duration
}

func (f *fakeSender) Send(ctx context.Context, topic, key, payload string) error {
	var timeout time.Duration
	if d, ok := ctx.Deadline(); ok {
		timeout = time.Until(d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sendCall{topic: topic, key: key, payload: payload, timeout: timeout})
	return f.err
}

func (f *fakeSender) Close() error { return nil }

func (f *fakeSender) calls() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendCall(nil), f.sent...)
}

// pendingRow 造一行「业务事务已提交」的 outbox：
// payload 是完整信封 JSON，各列全部取自同一个 env，与 appendOutboxEvent 的生产路径同源。
func pendingRow(t *testing.T, id int64, eventType, eventID, aggregateType, aggregateID string) *model.LiveMediaOutbox {
	t.Helper()
	return &model.LiveMediaOutbox{
		Id: id, EventId: eventID, EventType: eventType,
		SchemaVersion: model.EventSchemaVersion, AggregateType: aggregateType,
		AggregateId: aggregateID, RoomId: 17, Payload: envelopeJSON(t, eventType, eventID, aggregateType, aggregateID),
		State: model.OutboxStatePending, OccurredAt: 1_700_000_000, TraceId: "trace-1",
	}
}

// envelopeJSON 组装与生产路径一致的信封 JSON（显式 event_id，不走 New 的自动生成）。
func envelopeJSON(t *testing.T, eventType, eventID, aggregateType, aggregateID string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"record_id": aggregateID, "room_id": 17, "state": 4})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := &eventenvelope.Envelope{
		EventID: eventID, EventType: eventType, SchemaVersion: model.EventSchemaVersion,
		OccurredAt: "2026-10-04T00:00:00Z", Producer: model.ProducerName, TraceID: "trace-1",
		AggregateType: aggregateType, AggregateID: aggregateID, Payload: body,
	}
	raw, err := env.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(raw)
}

// recordStoppedRow 是本服务最典型的一行（录制停止 → 回放可拼接）。
func recordStoppedRow(t *testing.T, id int64) *model.LiveMediaOutbox {
	t.Helper()
	return pendingRow(t, id, model.EventTypeRecordStopped, "01RECORDEVENTID0000000000A",
		model.AggregateRecordTask, fmt.Sprintf("%d", id))
}

// --- RequiredTopics：跨服务契约的锚点 ---

// TestRequiredTopicsAreCrossServiceContract 把 9 个 topic 的字面量钉死：
// 它们是 docs/api-and-events.md §5 登记的跨服务契约，改名等于换掉所有消费方的订阅，
// 而这 9 条同时是 etc yaml 的 PublishTopics 与 ValidatePublishKafka 的判据来源。
func TestRequiredTopicsAreCrossServiceContract(t *testing.T) {
	want := []string{
		"livemedia.transcode.state.changed.v1",
		"livemedia.record.state.changed.v1",
		"livemedia.record.stopped.v1",
		"livemedia.record.gap.detected.v1",
		"livemedia.stream.output.online.v1",
		"livemedia.stream.output.offline.v1",
		"livemedia.replay.review.submitted.v1",
		"livemedia.replay.content.state.changed.v1",
		"livemedia.retention.finished.v1",
	}
	got := RequiredTopics()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("RequiredTopics=%v，期望（顺序也一致）%v", got, want)
	}
	if len(got) != 9 {
		t.Errorf("topic 数=%d，期望 9：数量变了必须同步 docs §5、yaml 与 deploy 建 topic 清单", len(got))
	}
	for _, topic := range got {
		if topic == "" || !strings.HasSuffix(topic, ".v1") {
			t.Errorf("topic %q 不合法：必须由 event_type + .v + schema_version 派生", topic)
		}
	}
	// 每个 topic 都能被 eventenvelope 的命名规则接受（违规名会让生产侧整笔事务回滚）。
	for _, topic := range got {
		eventType := strings.TrimSuffix(topic, ".v1")
		if _, err := eventenvelope.New(model.ProducerName, eventType, model.AggregateRecordTask, "1",
			model.EventSchemaVersion, json.RawMessage(`{}`), ""); err != nil {
			t.Errorf("event_type %q 过不了信封契约：%v", eventType, err)
		}
	}
}

// TestEventTypesAndRequiredTopicsStayInSync 是全仓 event_type 门禁在本服务内部的补集：
// 前者管命名合法性，本用例管「常量声明了但没登记进 topic 集合」。
// 漏登记的后果不是编译错误：那一类事件的每一行都会被判成「topic 不属于本服务」而直接判死，
// 而生产侧的 appendOutboxEvent 完全成功，事务照常提交。
func TestEventTypesAndRequiredTopicsStayInSync(t *testing.T) {
	declared := map[string]bool{}
	for _, eventType := range eventTypes {
		declared[eventType] = true
	}
	if len(eventTypes) != len(declared) {
		t.Fatalf("eventTypes 里有重复项：%v", eventTypes)
	}
	for _, topic := range RequiredTopics() {
		if _, ok := declared[strings.TrimSuffix(topic, ".v1")]; !ok {
			t.Errorf("topic %q 的来源常量不在 eventTypes 里", topic)
		}
	}
	// model 里的 EventType* 常量必须逐个出现在 eventTypes 里（逐个反查，不靠数量相等）。
	for _, c := range modelEventTypeConstants(t) {
		if !declared[c] {
			t.Errorf("model 声明了事件类型 %q，但 publisher.eventTypes 没登记它：%v 的行会被直接判死",
				c, eventenvelope.Topic(c, model.EventSchemaVersion))
		}
	}
}

// modelEventTypeConstants 从 model/live_media_outbox.go 源码里抓 EventType* 常量取值。
// 用源码扫描而不是反射：常量没有注册表可枚举，而这份文件就是本服务事件类型的事实来源
// （手法与 common/eventenvelope/event_type_gate_test.go 一致）。
func modelEventTypeConstants(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../../model/live_media_outbox.go")
	if err != nil {
		t.Fatalf("读取 model/live_media_outbox.go 失败（本服务的 topic 集合以它为准）: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		assign := strings.Index(trimmed, `= "`)
		if !strings.HasPrefix(trimmed, "EventType") || assign < 0 {
			continue
		}
		rest := trimmed[assign+len(`= "`):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			t.Fatalf("EventType* 常量 %q 的取值没有右引号，扫描规则失效", trimmed)
		}
		if value := rest[:end]; value != "" {
			out = append(out, value)
		}
	}
	if len(out) != 9 {
		t.Fatalf("只抓到 %d 个 EventType* 常量：%v（声明形态变了，本门禁会退化成永真检查）", len(out), out)
	}
	return out
}

// --- Store 适配 ---

func TestNewOutboxStoreRejectsNil(t *testing.T) {
	if _, err := NewOutboxStore(nil); err == nil || !strings.Contains(err.Error(), "outbox model is required") {
		t.Fatalf("nil model 应报错：%v", err)
	}
}

// TestToRowMapsColumns 钉住列 → Row 的映射：topic 由列现场拼出、
// 分区键取聚合根主键（同聚合的事件必须同分区），payload 原样透传不重新编码。
func TestToRowMapsColumns(t *testing.T) {
	for _, tc := range []struct {
		eventType, aggregate string
	}{
		{model.EventTypeTranscodeStateChanged, model.AggregateTranscodeTask},
		{model.EventTypeRecordStateChanged, model.AggregateRecordTask},
		{model.EventTypeRecordStopped, model.AggregateRecordTask},
		{model.EventTypeRecordGapDetected, model.AggregateRecordSegment},
		{model.EventTypeStreamOutputOnline, model.AggregateStreamOutput},
		{model.EventTypeStreamOutputOffline, model.AggregateStreamOutput},
		{model.EventTypeReplayReviewSubmitted, model.AggregateReplayTask},
		{model.EventTypeReplayContentStateChanged, model.AggregateReplayAssetRef},
		{model.EventTypeRetentionFinished, model.AggregateRetentionTask},
	} {
		t.Run(tc.eventType, func(t *testing.T) {
			row := pendingRow(t, 11, tc.eventType, "01EVENTIDAAA0000000000000B", tc.aggregate, "4242")
			rec := toRow(row)
			if rec.Defect != "" {
				t.Fatalf("正常行不应有缺陷：%q", rec.Defect)
			}
			if rec.ID != 11 || rec.EventID != "01EVENTIDAAA0000000000000B" || rec.RetryCount != 0 {
				t.Errorf("基础列映射错：%+v", rec)
			}
			if rec.Topic != eventenvelope.Topic(tc.eventType, model.EventSchemaVersion) {
				t.Errorf("topic=%q，期望由列派生", rec.Topic)
			}
			if rec.Key != "4242" {
				t.Errorf("分区键=%q，期望用聚合根 aggregate_id", rec.Key)
			}
			if rec.Payload != row.Payload {
				t.Error("payload 被改写了：期望原样透传")
			}
		})
	}
}

// TestToRowFlagsDefects 逐条钉住「不可发布」的判定与原因点名。
// 这些都必须是判死而不是重试：重试不会让列写歪的行变对。
func TestToRowFlagsDefects(t *testing.T) {
	base := func() *model.LiveMediaOutbox {
		return pendingRow(t, 12, model.EventTypeRecordStopped, "01EVENTIDAAA0000000000000C",
			model.AggregateRecordTask, "4242")
	}
	cases := []struct {
		name   string
		mutate func(*model.LiveMediaOutbox)
		want   string
	}{
		{"别的服务的事件", func(r *model.LiveMediaOutbox) { r.EventType = "content.published" }, "不属于本服务"},
		{"schema 版本升到 v2", func(r *model.LiveMediaOutbox) { r.SchemaVersion = 2 }, "livemedia.record.stopped.v2"},
		{"event_type 为空", func(r *model.LiveMediaOutbox) { r.EventType = "" }, `topic ""`},
		{"event_id 列为空", func(r *model.LiveMediaOutbox) { r.EventId = "" }, "event_id 列为空"},
		{"聚合根为空", func(r *model.LiveMediaOutbox) { r.AggregateId = "" }, "分区键为空"},
		{"payload 不是 JSON", func(r *model.LiveMediaOutbox) { r.Payload = "not-json" }, "不是合法事件信封"},
		{"payload 缺 producer", func(r *model.LiveMediaOutbox) {
			r.Payload = strings.Replace(r.Payload, `"`+model.ProducerName+`"`, `""`, 1)
		}, "producer is required"},
		{"payload 的 event_id 与列不一致", func(r *model.LiveMediaOutbox) {
			r.Payload = strings.Replace(r.Payload, "01EVENTIDAAA0000000000000C", "01OTHERIDAAA0000000000000D", 1)
		}, "payload event_id"},
		{"payload 的 aggregate_id 与列不一致", func(r *model.LiveMediaOutbox) { r.AggregateId = "9999" }, "payload aggregate_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := base()
			tc.mutate(row)
			rec := toRow(row)
			if rec.Defect == "" {
				t.Fatal("应给出不可发布原因")
			}
			if !strings.Contains(rec.Defect, tc.want) {
				t.Errorf("缺陷原因=%q，期望点名 %q", rec.Defect, tc.want)
			}
		})
	}
}

// TestOutboxStoreListPendingMapsAndWraps 确认列映射发生在读取处，
// 读库错误必须带上表名冒泡（否则运维只知道「扫不动」而不知道扫的是哪张表）。
func TestOutboxStoreListPendingMapsAndWraps(t *testing.T) {
	m := newFakeOutbox(
		recordStoppedRow(t, 1),
		nil,
		pendingRow(t, 2, model.EventTypeRetentionFinished, "01B00000000000000000000002",
			model.AggregateRetentionTask, "777"),
	)
	store, err := NewOutboxStore(m)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	records, err := store.ListPending(context.Background(), 1_700_000_123, 42)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records=%d，期望 2（nil 行必须跳过而不是塞进批次）", len(records))
	}
	if records[0].EventID != "01RECORDEVENTID0000000000A" || records[1].Key != "777" {
		t.Fatalf("映射结果不符：%+v", records)
	}
	// now / limit 必须原样透传到 model：退避到期与 state 判定留在 SQL 里，
	// 在这层吞掉参数就等于「提前投递」或「超量投递」。
	if m.now != 1_700_000_123 || m.limit != 42 {
		t.Errorf("透传参数=(%d,%d)，期望 (1700000123,42)", m.now, m.limit)
	}

	failing := newFakeOutbox()
	failing.listErr = errors.New("context deadline exceeded")
	store2, err := NewOutboxStore(failing)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	if _, err := store2.ListPending(context.Background(), 1, 1); err == nil ||
		!strings.Contains(err.Error(), "live_media_outbox") ||
		!strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("读库错误应点名表名并保留底层错误：%v", err)
	}
}

// TestOutboxStoreDelegatesStateWrites 钉住状态写库的参数没有被吞或错位：
// publishedAt / next_retry_at / last_error 原样传给 model，
// 而引擎算出的 retryCount 必须**不**出现在这些位置上（本表的计数由 SQL 自增）。
func TestOutboxStoreDelegatesStateWrites(t *testing.T) {
	m := newFakeOutbox()
	store, err := NewOutboxStore(m)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	ctx := context.Background()
	if err := store.MarkPublished(ctx, 7, 1234); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	// 引擎传 retryCount=3；若适配器把它错位在 next_retry_at 上，行会被当成「3 秒后到期」。
	if err := store.MarkRetry(ctx, 8, 3, 1_700_000_5678, "send error"); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
	if err := store.MarkFailed(ctx, 9, "exhausted"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if m.publishAt[7] != 1234 {
		t.Errorf("publishedAt=%d，期望 1234（本表用独立位点列，PurgePublished 按它归档）", m.publishAt[7])
	}
	if m.retryAt[8] != 1_700_000_5678 {
		t.Errorf("nextRetryAt=%d，期望 1700005678（不能把引擎的次数当时间戳写进去）", m.retryAt[8])
	}
	if m.lastError[8] != "send error" || m.lastError[9] != "exhausted" {
		t.Errorf("last_error 没有透传：%v", m.lastError)
	}
	want := "MarkPublished:7,MarkRetry:8,MarkFailed:9"
	if got := strings.Join(m.calls, ","); got != want {
		t.Errorf("model 调用序列=%s，期望 %s", got, want)
	}
}

// TestConditionalMissIsLoggedNotFatal 钉住本适配层与 playback 唯一的口径差异：
// 条件 UPDATE 命中 0 行返回 nil（只写日志），不冒泡成错误。
// 依据是这三条 UPDATE 的 WHERE 只可能在「行已有结论」或「行已被清理」时不命中，
// 而这在 at-least-once + 无租约列的多副本部署里是正常事件；
// 把它当错误会让 RunOnce 中断整批，把同批其余正常行也拖成「本轮没结论」。
func TestConditionalMissIsLoggedNotFatal(t *testing.T) {
	m := newFakeOutbox()
	m.affected = 0
	store, err := NewOutboxStore(m)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	ctx := context.Background()
	if err := store.MarkPublished(ctx, 1, 100); err != nil {
		t.Errorf("0 行不该是错误：%v", err)
	}
	if err := store.MarkRetry(ctx, 2, 1, 200, "x"); err != nil {
		t.Errorf("0 行不该是错误：%v", err)
	}
	if err := store.MarkFailed(ctx, 3, "x"); err != nil {
		t.Errorf("0 行不该是错误：%v", err)
	}
}

// TestMarkWritesWrapRealErrors 确认「0 行宽容」没有被扩成「什么错误都吞」：
// 真正的写库失败必须点名表名与操作并保留底层错误。
func TestMarkWritesWrapRealErrors(t *testing.T) {
	m := newFakeOutbox()
	m.markErr = errors.New("1406 Data too long for column 'last_error'")
	for _, id := range []int64{1, 2, 3} {
		m.failIDs[id] = struct{}{}
	}
	store, err := NewOutboxStore(m)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"MarkPublished", func() error { return store.MarkPublished(ctx, 1, 1) }, "标记已发布"},
		{"MarkRetry", func() error { return store.MarkRetry(ctx, 2, 1, 2, "e") }, "记录重试"},
		{"MarkFailed", func() error { return store.MarkFailed(ctx, 3, "e") }, "判死"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("写库失败必须冒泡")
			}
			for _, want := range []string{"live_media_outbox", tc.want, "1406"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误=%v，期望点名 %q", err, want)
				}
			}
		})
	}
}

// --- 端到端：真适配器 + 假 model/假发送端 ---

// TestRunOnceThroughRealStore 走一遍列 → Row → 投递 → 状态回写。
// 这里刻意不注入时钟：common/outbox 的 now 是包内私有，本用例用真实时钟的前后区间做界限，
// 正是为了验证「默认装配（outbox.New）也能跑通」。
func TestRunOnceThroughRealStore(t *testing.T) {
	m := newFakeOutbox(recordStoppedRow(t, 21))
	store, err := NewOutboxStore(m)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	sender := &fakeSender{}
	pub, err := outbox.New(store, sender, testOptions())
	if err != nil {
		t.Fatalf("outbox.New: %v", err)
	}
	before := time.Now().Unix()
	handled, err := pub.RunOnce(context.Background())
	after := time.Now().Unix()
	if err != nil || handled != 1 {
		t.Fatalf("RunOnce=(%d,%v)，期望 (1,nil)", handled, err)
	}
	sent := sender.calls()
	if len(sent) != 1 || sent[0].topic != "livemedia.record.stopped.v1" || sent[0].key != "21" {
		t.Fatalf("投递不符：%+v", sent)
	}
	if sent[0].payload != m.rows[0].Payload {
		t.Error("投递的 payload 与 outbox 列不一致")
	}
	if got := m.publishAt[21]; got < before || got > after {
		t.Errorf("已发布位点=%d，不在 [%d,%d] 内", got, before, after)
	}
	// 发布器只能走非事务的四个状态方法：Insert 属于 logic 的事务权限。
	if got := strings.Join(m.calls, ","); got != "ListPending,MarkPublished:21" {
		t.Errorf("model 调用序列=%s，期望只有 ListPending 与 MarkPublished", got)
	}
}

// TestRunOnceRetriesOnSendError 确认适配器没有把投递失败咽掉：
// 失败必须落到 MarkRetry，行仍保持待发布，而不是被写成已发布。
// RunOnce 对「投递失败」返回 nil 是引擎口径（失败已经写进行的状态里），
// 因此这里的证据只能是退避时间与 last_error，而不是 error 本身。
func TestRunOnceRetriesOnSendError(t *testing.T) {
	m := newFakeOutbox(recordStoppedRow(t, 31))
	store, err := NewOutboxStore(m)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	sender := &fakeSender{err: errors.New("broker down")}
	opts := testOptions()
	opts.MaxAttempts = 5
	pub, err := outbox.New(store, sender, opts)
	if err != nil {
		t.Fatalf("outbox.New: %v", err)
	}
	handled, err := pub.RunOnce(context.Background())
	if err != nil || handled != 1 {
		t.Fatalf("RunOnce=(%d,%v)，期望 (1,nil)：投递失败已落进行状态，不该中断批次", handled, err)
	}
	if _, ok := m.publishAt[31]; ok {
		t.Error("投递失败却被写成已发布：state 语义已破")
	}
	if got := m.retryAt[31]; got <= time.Now().Unix() {
		t.Errorf("next_retry_at=%d，期望在未来（退避没生效就是打满重试）", got)
	}
	if m.lastError[31] != "broker down" {
		t.Errorf("last_error=%q，期望 broker down", m.lastError[31])
	}
	if got := strings.Join(m.calls, ","); got != "ListPending,MarkRetry:31" {
		t.Errorf("model 调用序列=%s，期望只写退避不写已发布", got)
	}
}

// TestRunOnceDistinguishesMissFromWriteError 把两条口径放进同一次批次里对比：
// 0 行（别的副本已给结论）继续跑完，真写库失败立刻中断并把剩余行留给下一轮。
func TestRunOnceDistinguishesMissFromWriteError(t *testing.T) {
	rows := []*model.LiveMediaOutbox{recordStoppedRow(t, 41), recordStoppedRow(t, 42), recordStoppedRow(t, 43)}

	t.Run("0行不中断", func(t *testing.T) {
		m := newFakeOutbox(rows...)
		m.affected = 0
		store, err := NewOutboxStore(m)
		if err != nil {
			t.Fatalf("NewOutboxStore: %v", err)
		}
		pub, err := outbox.New(store, &fakeSender{}, testOptions())
		if err != nil {
			t.Fatalf("outbox.New: %v", err)
		}
		handled, err := pub.RunOnce(context.Background())
		if err != nil || handled != 3 {
			t.Fatalf("RunOnce=(%d,%v)，期望 (3,nil)：0 行是本副本的正常让位", handled, err)
		}
	})

	t.Run("写库失败中断本批", func(t *testing.T) {
		m := newFakeOutbox(rows...)
		m.markErr = errors.New("deadlock found")
		m.failIDs[42] = struct{}{}
		store, err := NewOutboxStore(m)
		if err != nil {
			t.Fatalf("NewOutboxStore: %v", err)
		}
		pub, err := outbox.New(store, &fakeSender{}, testOptions())
		if err != nil {
			t.Fatalf("outbox.New: %v", err)
		}
		handled, err := pub.RunOnce(context.Background())
		if err == nil || handled != 1 {
			t.Fatalf("RunOnce=(%d,%v)，期望 (1,error)：id=42 状态写不进去必须冒泡，剩余行留给下一轮", handled, err)
		}
		for _, want := range []string{"live_media_outbox", "42", "deadlock found"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("错误=%v，期望点名 %q", err, want)
			}
		}
		if got := strings.Join(m.calls, ","); got != "ListPending,MarkPublished:41,MarkPublished:42" {
			t.Errorf("调用序列=%s，期望在第 2 行失败后即停", got)
		}
	})
}

// TestRunOnceJudgesForeignEventDead 钉住「本表只产出这 9 类事件」这一侧的守卫：
// 一行 event_type 不属于本服务的事件必须直接判死，而不是投出去或无限重试。
func TestRunOnceJudgesForeignEventDead(t *testing.T) {
	m := newFakeOutbox(pendingRow(t, 51, "content.published", "01FOREIGNEVENT00000000000A",
		"submission", "51"))
	store, err := NewOutboxStore(m)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	sender := &fakeSender{}
	pub, err := outbox.New(store, sender, testOptions())
	if err != nil {
		t.Fatalf("outbox.New: %v", err)
	}
	handled, err := pub.RunOnce(context.Background())
	if err != nil || handled != 1 {
		t.Fatalf("RunOnce=(%d,%v)", handled, err)
	}
	if len(sender.calls()) != 0 {
		t.Errorf("外来事件被投出去了：%+v", sender.calls())
	}
	if !strings.Contains(m.lastError[51], "不属于本服务") {
		t.Errorf("判死原因=%q，期望点名 topic 归属", m.lastError[51])
	}
}

// testOptions 给出一组合法且最小的发布参数（真实时钟；退避细节由 common/outbox 的用例负责）。
func testOptions() outbox.Options {
	return outbox.Options{
		Name:        label,
		Interval:    2 * time.Second,
		Batch:       100,
		MaxAttempts: 5,
		BaseBackoff: 2 * time.Second,
		MaxBackoff:  1800 * time.Second,
		SendTimeout: 5 * time.Second,
	}
}

// 编译期确认适配器满足引擎接口：签名一旦漂移（例如 limit 从 int32 改 int），
// 默认构建就会在这里红，而不是等 svc 启动时才炸。
var _ outbox.Store = (*OutboxStore)(nil)
