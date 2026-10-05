package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/common/eventenvelope"
	"go-video/services/live-ingest/model"
)

// fakeOutboxModel 是 model.EventOutboxModel 的替身：只实现发布器用到的四个方法，
// 其余方法靠内嵌接口留空（被调用即 panic，能立刻暴露「发布器越权读了别的列」）。
type fakeOutboxModel struct {
	model.EventOutboxModel
	rows    []*model.EventOutbox
	listErr error

	calls     []string
	now       int64
	limit     int32
	publishAt map[int64]int64
	retryArgs map[int64][2]int64 // id -> {retryCount, nextRetryAt}
	failedErr map[int64]string
}

func newFakeOutbox(rows ...*model.EventOutbox) *fakeOutboxModel {
	return &fakeOutboxModel{
		rows:      rows,
		publishAt: map[int64]int64{},
		retryArgs: map[int64][2]int64{},
		failedErr: map[int64]string{},
	}
}

func (f *fakeOutboxModel) ListPending(_ context.Context, now int64, limit int32) ([]*model.EventOutbox, error) {
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
	f.failedErr[id] = lastError
	return nil
}

func (f *fakeOutboxModel) MarkFailed(_ context.Context, id int64, lastError string) error {
	f.calls = append(f.calls, fmt.Sprintf("MarkFailed:%d", id))
	f.failedErr[id] = lastError
	return nil
}

// pendingRow 造一行「logic 已在事务内写好」的 outbox：payload 是完整信封 JSON，
// 与 internal/logic/streamstate.go 的 buildStateEnvelopePayload 同源。
func pendingRow(t *testing.T, id int64, eventID, streamID string) *model.EventOutbox {
	t.Helper()
	return &model.EventOutbox{
		ID: id, EventID: eventID, EventType: model.EventTypeStreamState,
		SchemaVersion: model.SchemaVersionStreamState, AggregateType: model.AggregateTypeStream,
		AggregateID: streamID, StreamID: streamID, RoomID: 100 + id, Seq: id,
		Payload: envelopeJSON(t, eventID, streamID), State: model.OutboxStatePending,
	}
}

// envelopeJSON 组装与生产路径一致的信封 JSON（显式 event_id，不走 New 的自动生成）。
func envelopeJSON(t *testing.T, eventID, streamID string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"stream_id": streamID, "room_id": 1, "stream_state": 2, "stream_seq": 1})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := &eventenvelope.Envelope{
		EventID: eventID, EventType: model.EventTypeStreamState, SchemaVersion: model.SchemaVersionStreamState,
		OccurredAt: "2026-10-04T00:00:00Z", Producer: "live-ingest", TraceID: "trace-1",
		AggregateType: model.AggregateTypeStream, AggregateID: streamID, Payload: body,
	}
	raw, err := env.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(raw)
}

// TestRequiredTopicIsCrossServiceContract 把 topic 字面量钉死：
// 它是 docs/api-and-events.md §5 登记的跨服务契约，改名等于换掉所有消费方的订阅。
func TestRequiredTopicIsCrossServiceContract(t *testing.T) {
	if got := RequiredTopic(); got != "live.state.v1" {
		t.Fatalf("RequiredTopic=%q，期望 live.state.v1（改动必须同步 proto、迁移注释与文档 §5）", got)
	}
	if got, want := RequiredTopic(), eventenvelope.Topic(model.EventTypeStreamState, model.SchemaVersionStreamState); got != want {
		t.Errorf("RequiredTopic 必须由 model 常量拼出：%q != %q", got, want)
	}
}

func TestNewOutboxStoreRejectsNil(t *testing.T) {
	if _, err := NewOutboxStore(nil); err == nil || !strings.Contains(err.Error(), "outbox model is required") {
		t.Fatalf("nil model 应报错：%v", err)
	}
}

// TestToRecordMapsColumns 钉住列 → Record 的映射：topic 由列现场拼出、
// 分区键取聚合根（stream_id），payload 原样透传不重新编码。
func TestToRecordMapsColumns(t *testing.T) {
	row := pendingRow(t, 11, "01EVENTIDAAA", "stream-11")
	rec := toRecord(row)

	if rec.Defect != "" {
		t.Fatalf("正常行不应有缺陷：%q", rec.Defect)
	}
	if rec.ID != 11 || rec.EventID != "01EVENTIDAAA" || rec.RetryCount != 0 {
		t.Errorf("基础列映射错：%+v", rec)
	}
	if rec.Topic != RequiredTopic() {
		t.Errorf("topic=%q，期望 %q", rec.Topic, RequiredTopic())
	}
	if rec.Key != "stream-11" {
		t.Errorf("分区键=%q，期望用聚合根 stream_id（同流事件必须同分区）", rec.Key)
	}
	if rec.Payload != row.Payload {
		t.Errorf("payload 被改写了：期望原样透传")
	}
}

// TestToRecordFlagsDefects 逐条钉住「不可发布」的判定与原因点名。
// 这些都必须是判死而不是重试：重试不会让列写歪的行变对。
func TestToRecordFlagsDefects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.EventOutbox)
		want   string
	}{
		{"event_type 写歪", func(r *model.EventOutbox) { r.EventType = "content.published" }, "不属于本服务"},
		{"schema 版本升到 v2", func(r *model.EventOutbox) { r.SchemaVersion = 2 }, "live.state.v2"},
		{"event_type 为空", func(r *model.EventOutbox) { r.EventType = "" }, "topic \"\""},
		{"聚合根为空", func(r *model.EventOutbox) { r.AggregateID = "" }, "aggregate_id 为空"},
		{"payload 不是 JSON", func(r *model.EventOutbox) { r.Payload = "not-json" }, "不是合法事件信封"},
		{"payload 缺 producer", func(r *model.EventOutbox) {
			r.Payload = strings.Replace(r.Payload, `"producer":"live-ingest"`, `"producer":""`, 1)
		}, "producer is required"},
		{"payload 的 event_id 与列不一致", func(r *model.EventOutbox) {
			r.Payload = strings.Replace(r.Payload, "01EVENTIDAAA", "01OTHEREVENTID", 1)
		}, "payload event_id"},
		{"payload 的 aggregate_id 与列不一致", func(r *model.EventOutbox) {
			r.AggregateID = "stream-99"
		}, "payload aggregate_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingRow(t, 12, "01EVENTIDAAA", "stream-12")
			tc.mutate(row)
			rec := toRecord(row)
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
	store, err := NewOutboxStore(newFakeOutbox(pendingRow(t, 1, "e1", "s1"), nil, pendingRow(t, 2, "e2", "s2")))
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
	if records[0].EventID != "e1" || records[1].Key != "s2" {
		t.Fatalf("映射结果不符：%+v", records)
	}
	// now / limit 必须原样透传到 model：退避到期判定留在 SQL 里，
	// 在这层吞掉参数就等于「提前投递」。
	m := newFakeOutbox(pendingRow(t, 3, "e3", "s3"))
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
		!strings.Contains(err.Error(), "live_ingest_outbox") ||
		!strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("读库错误应点名表名并保留底层错误：%v", err)
	}
}

// TestOutboxStoreDelegatesStateWrites 钉住状态写库的参数没有被吞：
// publishedAt / retry_count / next_retry_at / last_error 都必须原样传给 model，
// 否则 GetEventPublishCheckpoint 的滞后度与 RetryFailedEvents 的放行都会失真。
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
		t.Errorf("publishedAt=%d，期望 1234", m.publishAt[7])
	}
	if got := m.retryArgs[8]; got[0] != 3 || got[1] != 5678 {
		t.Errorf("MarkRetry 参数=%v，期望 [3 5678]", got)
	}
	if m.failedErr[8] != "send error" || m.failedErr[9] != "exhausted" {
		t.Errorf("last_error 没有透传：%v", m.failedErr)
	}
	want := []string{"MarkPublished:7", "MarkRetry:8", "MarkFailed:9"}
	if strings.Join(m.calls, ",") != strings.Join(want, ",") {
		t.Errorf("调用序列=%v，期望 %v", m.calls, want)
	}
}

// TestRunOnceThroughRealStore 用真适配器 + 假 model 走一遍端到端：
// 列 → Record → 投递 → 状态回写，任何一环把 topic 或分区键弄错都会在这里红。
func TestRunOnceThroughRealStore(t *testing.T) {
	m := newFakeOutbox(pendingRow(t, 21, "01EVENT", "stream-21"))
	store, err := NewOutboxStore(m)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	handled, err := pub.RunOnce(context.Background())
	if err != nil || handled != 1 {
		t.Fatalf("RunOnce=(%d,%v)，期望 (1,nil)", handled, err)
	}
	sent := sender.sentCalls()
	if len(sent) != 1 || sent[0].topic != "live.state.v1" || sent[0].key != "stream-21" {
		t.Fatalf("投递不符：%+v", sent)
	}
	if sent[0].payload != string(m.rows[0].Payload) {
		t.Error("投递的 payload 与 outbox 列不一致")
	}
	if got := m.publishAt[21]; got != fakeNow.Unix() {
		t.Errorf("已发布位点=%d，期望注入时钟 %d", got, fakeNow.Unix())
	}
	// 发布器只能走非事务的四个状态方法：Insert/ResetFailed 属于 logic 与 RPC 的权限。
	if got := strings.Join(m.calls, ","); got != "ListPending,MarkPublished:21" {
		t.Errorf("model 调用序列=%s，期望只有 ListPending 与 MarkPublished", got)
	}
}
