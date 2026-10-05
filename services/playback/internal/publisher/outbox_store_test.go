package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go-video/common/eventenvelope"
	"go-video/common/outbox"
	"go-video/services/playback/model"
)

// --- 替身 ---

// fakeOutboxModel 是 model.PlaybackOutboxModel 的替身：只实现发布器用到的四个方法，
// 其余方法靠内嵌接口留空（被调用即 panic，能立刻暴露「发布器越权读了别的列」）。
type fakeOutboxModel struct {
	model.PlaybackOutboxModel
	rows    []*model.PlaybackOutbox
	listErr error

	calls     []string
	now       int64
	limit     int32
	publishAt map[int64]int64
	retryArgs map[int64][2]int64 // id -> {retryCount, nextRetryAt}
	lastError map[int64]string
}

func newFakeOutbox(rows ...*model.PlaybackOutbox) *fakeOutboxModel {
	return &fakeOutboxModel{
		rows:      rows,
		publishAt: map[int64]int64{},
		retryArgs: map[int64][2]int64{},
		lastError: map[int64]string{},
	}
}

func (f *fakeOutboxModel) ListPending(_ context.Context, now int64, limit int32) ([]*model.PlaybackOutbox, error) {
	f.calls = append(f.calls, "ListPending")
	f.now, f.limit = now, limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeOutboxModel) MarkPublished(_ context.Context, id, publishedAt int64) error {
	f.calls = append(f.calls, fmt.Sprintf("MarkPublished:%d", id))
	f.publishAt[id] = publishedAt
	return nil
}

func (f *fakeOutboxModel) MarkRetry(_ context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	f.calls = append(f.calls, fmt.Sprintf("MarkRetry:%d", id))
	f.retryArgs[id] = [2]int64{int64(retryCount), nextRetryAt}
	f.lastError[id] = lastError
	return nil
}

func (f *fakeOutboxModel) MarkFailed(_ context.Context, id int64, lastError string) error {
	f.calls = append(f.calls, fmt.Sprintf("MarkFailed:%d", id))
	f.lastError[id] = lastError
	return nil
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
	timeout, _ := time.Duration(0), false
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

// pendingRow 造一行「ReportHeartbeat 已在事务内写好」的 outbox：
// payload 是完整信封 JSON，六个业务列全部取自同一个 env，与生产路径同源。
func pendingRow(t *testing.T, id int64, eventID, sessionID string) *model.PlaybackOutbox {
	t.Helper()
	return &model.PlaybackOutbox{
		ID: id, EventID: eventID, EventType: model.EventPlaybackHeartbeat,
		SchemaVersion: model.EventSchemaVersion, AggregateType: model.AggregateTypeSession,
		AggregateID: sessionID, Payload: envelopeJSON(t, eventID, sessionID),
		State: model.OutboxStatePending, OccurredAt: 1_700_000_000,
	}
}

// envelopeJSON 组装与生产路径一致的信封 JSON（显式 event_id，不走 New 的自动生成）。
func envelopeJSON(t *testing.T, eventID, sessionID string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "content_id": 7, "position_ms": 1000})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := &eventenvelope.Envelope{
		EventID: eventID, EventType: model.EventPlaybackHeartbeat, SchemaVersion: model.EventSchemaVersion,
		OccurredAt: "2026-10-04T00:00:00Z", Producer: model.Producer, TraceID: "trace-1",
		AggregateType: model.AggregateTypeSession, AggregateID: sessionID, Payload: body,
	}
	raw, err := env.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(raw)
}

// --- 测试 ---

// TestRequiredTopicIsCrossServiceContract 把 topic 字面量钉死：
// 它是 docs/api-and-events.md §5 登记的跨服务契约，改名等于换掉所有消费方的订阅。
func TestRequiredTopicIsCrossServiceContract(t *testing.T) {
	if got := RequiredTopic(); got != "playback.heartbeat.v1" {
		t.Fatalf("RequiredTopic=%q，期望 playback.heartbeat.v1（改动必须同步文档 §5 与 yaml）", got)
	}
	if got, want := RequiredTopic(), eventenvelope.Topic(model.EventPlaybackHeartbeat, model.EventSchemaVersion); got != want {
		t.Errorf("RequiredTopic 必须由 model 常量拼出：%q != %q", got, want)
	}
	if got := RequiredTopics(); len(got) != 1 || got[0] != RequiredTopic() {
		t.Errorf("RequiredTopics=%v，期望恰好一条", got)
	}
}

func TestNewOutboxStoreRejectsNil(t *testing.T) {
	if _, err := NewOutboxStore(nil); err == nil || !strings.Contains(err.Error(), "outbox model is required") {
		t.Fatalf("nil model 应报错：%v", err)
	}
}

// TestToRowMapsColumns 钉住列 → Row 的映射：topic 由列现场拼出、
// 分区键取聚合根（session_id），payload 原样透传不重新编码。
func TestToRowMapsColumns(t *testing.T) {
	row := pendingRow(t, 11, "01EVENTIDAAA", "sess-11")
	rec := toRow(row)

	if rec.Defect != "" {
		t.Fatalf("正常行不应有缺陷：%q", rec.Defect)
	}
	if rec.ID != 11 || rec.EventID != "01EVENTIDAAA" || rec.RetryCount != 0 {
		t.Errorf("基础列映射错：%+v", rec)
	}
	if rec.Topic != RequiredTopic() {
		t.Errorf("topic=%q，期望 %q", rec.Topic, RequiredTopic())
	}
	if rec.Key != "sess-11" {
		t.Errorf("分区键=%q，期望用聚合根 session_id（同会话心跳必须同分区）", rec.Key)
	}
	if rec.Payload != row.Payload {
		t.Errorf("payload 被改写了：期望原样透传")
	}
}

// TestToRowFlagsDefects 逐条钉住「不可发布」的判定与原因点名。
// 这些都必须是判死而不是重试：重试不会让列写歪的行变对。
func TestToRowFlagsDefects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.PlaybackOutbox)
		want   string
	}{
		{"event_type 写歪", func(r *model.PlaybackOutbox) { r.EventType = "content.published" }, "不属于本服务"},
		{"schema 版本升到 v2", func(r *model.PlaybackOutbox) { r.SchemaVersion = 2 }, "playback.heartbeat.v2"},
		{"event_type 为空", func(r *model.PlaybackOutbox) { r.EventType = "" }, `topic ""`},
		{"event_id 列为空", func(r *model.PlaybackOutbox) { r.EventID = "" }, "event_id 列为空"},
		{"聚合根为空", func(r *model.PlaybackOutbox) { r.AggregateID = "" }, "分区键为空"},
		{"payload 不是 JSON", func(r *model.PlaybackOutbox) { r.Payload = "not-json" }, "不是合法事件信封"},
		{"payload 缺 producer", func(r *model.PlaybackOutbox) {
			r.Payload = strings.Replace(r.Payload, `"producer":"playback"`, `"producer":""`, 1)
		}, "producer is required"},
		{"payload 的 event_id 与列不一致", func(r *model.PlaybackOutbox) {
			r.Payload = strings.Replace(r.Payload, "01EVENTIDAAA", "01OTHEREVENTID", 1)
		}, "payload event_id"},
		{"payload 的 aggregate_id 与列不一致", func(r *model.PlaybackOutbox) { r.AggregateID = "sess-99" }, "payload aggregate_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingRow(t, 12, "01EVENTIDAAA", "sess-12")
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
	store, err := NewOutboxStore(newFakeOutbox(pendingRow(t, 1, "e1", "sess-1"), nil, pendingRow(t, 2, "e2", "sess-2")))
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	records, err := store.ListPending(context.Background(), 999, 50)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records=%d，期望 2（nil 行必须跳过而不是塞进批次）", len(records))
	}
	if records[0].EventID != "e1" || records[1].Key != "sess-2" {
		t.Fatalf("映射结果不符：%+v", records)
	}
	// now / limit 必须原样透传到 model：退避到期与 state 判定留在 SQL 里，
	// 在这层吞掉参数就等于「提前投递」或「超量投递」。
	m := newFakeOutbox(pendingRow(t, 3, "e3", "sess-3"))
	store3, err := NewOutboxStore(m)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	if _, err := store3.ListPending(context.Background(), 1_700_000_123, 42); err != nil {
		t.Fatalf("ListPending: %v", err)
	}
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
		!strings.Contains(err.Error(), "playback_outbox") ||
		!strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("读库错误应点名表名并保留底层错误：%v", err)
	}
}

// TestOutboxStoreDelegatesStateWrites 钉住状态写库的参数没有被吞：
// publishedAt / retry_count / next_retry_at / last_error 都必须原样传给 model。
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
	if err := store.MarkRetry(ctx, 8, 3, 5678, "send error"); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
	if err := store.MarkFailed(ctx, 9, "exhausted"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if m.publishAt[7] != 1234 {
		t.Errorf("publishedAt=%d，期望 1234（playback_outbox 用它写 mtime）", m.publishAt[7])
	}
	if got := m.retryArgs[8]; got[0] != 3 || got[1] != 5678 {
		t.Errorf("MarkRetry 参数=%v，期望 [3 5678]", got)
	}
	if m.lastError[8] != "send error" || m.lastError[9] != "exhausted" {
		t.Errorf("last_error 没有透传：%v", m.lastError)
	}
	want := "MarkPublished:7,MarkRetry:8,MarkFailed:9"
	if got := strings.Join(m.calls, ","); got != want {
		t.Errorf("调用序列=%s，期望 %s", got, want)
	}
}

// TestRunOnceThroughRealStore 用真适配器 + 假 model 走一遍端到端：
// 列 → Row → 投递 → 状态回写，任何一环把 topic 或分区键弄错都会在这里红。
// 这里刻意不注入时钟：common/outbox 的 now 是包内私有，本用例用真实时钟的
// 前后区间做界限，正是为了验证「默认装配（outbox.New）也能跑通」。
func TestRunOnceThroughRealStore(t *testing.T) {
	m := newFakeOutbox(pendingRow(t, 21, "01EVENT", "sess-21"))
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
	if len(sent) != 1 || sent[0].topic != RequiredTopic() || sent[0].key != "sess-21" {
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
// 因此这里的证据只能是 retry_count 与 last_error，而不是 error 本身。
func TestRunOnceRetriesOnSendError(t *testing.T) {
	m := newFakeOutbox(pendingRow(t, 31, "01EVENT", "sess-31"))
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
	if got := m.retryArgs[31]; got[0] != 1 || got[1] <= time.Now().Unix() {
		t.Errorf("MarkRetry 参数=%v，期望 retry_count=1 且 next_retry_at 在未来", got)
	}
	if m.lastError[31] != "broker down" {
		t.Errorf("last_error=%q，期望 broker down", m.lastError[31])
	}
	if got := strings.Join(m.calls, ","); got != "ListPending,MarkRetry:31" {
		t.Errorf("model 调用序列=%s，期望只写退避不写已发布", got)
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
