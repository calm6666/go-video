// consumer_test.go 用 fake Store 驱动消费状态机，不连任何真实中间件
// （AGENTS.md §9：fake 必须按用例返回真实错误，不允许「永不失败」）。
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/common/eventenvelope"
	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/model"
)

// fakeStore 记录调用序列与关键参数。
type fakeStore struct {
	calls []string

	receivedDuplicate bool
	receivedErr       error
	processingErr     error
	succeededErr      error
	recordDLQExisted  bool
	upsertErr         error
	patchErr          error
	patchOutcome      string
	deleteErr         error
	due               []*model.SearchConsumerOffset

	// failFor 按 content_id 精确控制写失败，让一次 Sweep 能覆盖多条分支。
	failFor map[int64]error

	lastReceived *model.SearchConsumerOffset
	lastRetry    struct {
		eventID     string
		retryCount  int32
		nextRetryAt int64
		lastError   string
	}
	upserted  []*esclient.ContentDoc
	patched   []esclient.Heat
	deleted   []string
	dlqEvents []string
}

func (f *fakeStore) log(name string) { f.calls = append(f.calls, name) }

func (f *fakeStore) MarkEventReceived(_ context.Context, rec *model.SearchConsumerOffset) (bool, error) {
	f.log("received")
	if f.receivedErr != nil {
		return false, f.receivedErr
	}
	f.lastReceived = rec
	return f.receivedDuplicate, nil
}

func (f *fakeStore) MarkEventProcessing(_ context.Context, eventID string) error {
	f.log("processing:" + eventID)
	return f.processingErr
}

func (f *fakeStore) MarkEventSucceeded(_ context.Context, eventID string) error {
	f.log("succeeded:" + eventID)
	return f.succeededErr
}

func (f *fakeStore) MarkEventRetry(_ context.Context, eventID string, retryCount int32, nextRetryAt int64, lastError string) error {
	f.log("retry")
	f.lastRetry.eventID, f.lastRetry.retryCount, f.lastRetry.nextRetryAt, f.lastRetry.lastError = eventID, retryCount, nextRetryAt, lastError
	return nil
}

func (f *fakeStore) MarkEventDeadLetter(_ context.Context, eventID, _ string) error {
	f.log("dead_letter:" + eventID)
	return nil
}

func (f *fakeStore) RecordDeadLetter(_ context.Context, eventID, _, _ string, _ []byte, _ string) (bool, error) {
	f.log("record_dlq")
	f.dlqEvents = append(f.dlqEvents, eventID)
	return f.recordDLQExisted, nil
}

func (f *fakeStore) DueRetryEvents(_ context.Context, _ int) ([]*model.SearchConsumerOffset, error) {
	f.log("due")
	return f.due, nil
}

func (f *fakeStore) UpsertDoc(_ context.Context, doc *esclient.ContentDoc, _ bool) (*repository.UpsertResult, error) {
	f.log("upsert")
	err := f.upsertErr
	if e, ok := f.failFor[doc.ContentID]; ok {
		err = e
	}
	if err != nil {
		return nil, err
	}
	f.upserted = append(f.upserted, doc)
	return &repository.UpsertResult{Index: "idx", Outcome: repository.OutcomeCreated, DocRevision: doc.DocRevision}, nil
}

func (f *fakeStore) PatchHeat(_ context.Context, _ int64, _ int32, heat esclient.Heat, _ bool) (*repository.UpsertResult, error) {
	f.log("patch_heat")
	if f.patchErr != nil {
		return nil, f.patchErr
	}
	outcome := f.patchOutcome
	if outcome == "" {
		outcome = repository.OutcomeWritten
	}
	f.patched = append(f.patched, heat)
	return &repository.UpsertResult{Index: "idx", Outcome: outcome}, nil
}

func (f *fakeStore) DeleteContent(_ context.Context, contentID int64, contentType int32, purge bool, _ string) (*repository.DeleteOutcome, error) {
	f.log(fmt.Sprintf("delete:%d:%v", contentID, purge))
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deleted = append(f.deleted, esclient.DocID(contentType, contentID))
	return &repository.DeleteOutcome{Index: "idx", Outcome: repository.OutcomeDeleted}, nil
}

var _ Store = (*fakeStore)(nil)

func contentMessage(t *testing.T, payload map[string]interface{}) *Message {
	t.Helper()
	return &Message{Topic: "content.published.v1", Partition: 1, Offset: 42, Value: envelopeBytes(t, EventTypeContentPublished, payload)}
}

func engagementMessage(t *testing.T, payload map[string]interface{}) *Message {
	t.Helper()
	return &Message{Topic: "engagement.action.v1", Partition: 0, Offset: 7, Value: envelopeBytes(t, EventTypeEngagementAction, payload)}
}

func envelopeBytes(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	env := &eventenvelope.Envelope{
		EventID: "evt-0001", EventType: eventType, SchemaVersion: 1,
		OccurredAt: "2026-01-02T03:04:05Z", Producer: "video",
		AggregateType: "content", AggregateID: fmt.Sprint(payload["content_id"]), Payload: raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func fastOptions() Options {
	return Options{
		MaxRetries: 5, InProcessAttempts: 1, // 1 次即交给退避队列，单测不真的 sleep
		BaseBackoff: time.Second, MaxBackoff: time.Minute,
		RetryBatchLimit: 10, SweepIdleWait: time.Millisecond,
	}
}

func assertCalls(t *testing.T, store *fakeStore, want ...string) {
	t.Helper()
	got := strings.Join(store.calls, ",")
	wantStr := strings.Join(want, ",")
	if got != wantStr {
		t.Fatalf("调用序列 = [%s], want [%s]", got, wantStr)
	}
}

func TestProcessMessage_HappyPathReceivedProcessingSucceeded(t *testing.T) {
	store := &fakeStore{}
	c := New(store, fastOptions())
	msg := contentMessage(t, map[string]interface{}{
		"action": ActionPublish, "content_id": 1001, "content_type": 1,
		"title": "新视频", "doc_revision": 5000,
	})
	if err := c.ProcessMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	// 合法迁移：received → processing → succeeded，中间不跳步。
	assertCalls(t, store, "received", "processing:evt-0001", "upsert", "succeeded:evt-0001")
	if store.lastReceived.State != model.OffsetStateReceived {
		t.Fatalf("登记状态应为 received，实际 %s", store.lastReceived.State)
	}
	if store.lastReceived.EventID != "evt-0001" || store.lastReceived.Topic != "content.published.v1" {
		t.Fatalf("去重键/Topic 登记异常: %+v", store.lastReceived)
	}
	if store.lastReceived.PartitionNo != 1 || store.lastReceived.OffsetNo != 42 {
		t.Fatalf("投递元数据未落库: %+v", store.lastReceived)
	}
	if store.lastReceived.PayloadJSON == "" {
		t.Fatal("必须保存原文以支持进程重启后续跑")
	}
	if len(store.upserted) != 1 || store.upserted[0].ContentID != 1001 {
		t.Fatalf("投影未写入或写错: %+v", store.upserted)
	}
}

func TestProcessMessage_DuplicateSkippedWithoutWrite(t *testing.T) {
	store := &fakeStore{receivedDuplicate: true}
	c := New(store, fastOptions())
	if err := c.ProcessMessage(context.Background(), contentMessage(t, map[string]interface{}{
		"action": ActionPublish, "content_id": 1, "content_type": 1, "title": "T", "doc_revision": 1,
	})); err != nil {
		t.Fatal(err)
	}
	// event_id 去重命中后直接返回：不写索引、不推进状态，重放安全。
	assertCalls(t, store, "received")
	if len(store.upserted) != 0 {
		t.Fatal("重复事件不得再次写索引")
	}
}

func TestProcessMessage_UnsupportedEventTypeIgnored(t *testing.T) {
	store := &fakeStore{}
	c := New(store, fastOptions())
	msg := &Message{Topic: "other.v1", Value: envelopeBytes(t, "feed.publish", map[string]interface{}{"content_id": 1})}
	if err := c.ProcessMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	// 未订阅的事件不是错误：既不写索引也不进死信，避免淹没真实故障。
	assertCalls(t, store)
}

func TestProcessMessage_EmptyMessageIsNoop(t *testing.T) {
	store := &fakeStore{}
	c := New(store, fastOptions())
	for _, m := range []*Message{nil, {Topic: "t"}} {
		if err := c.ProcessMessage(context.Background(), m); err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
	}
	assertCalls(t, store)
}

func TestProcessMessage_MalformedEnvelopeGoesToDeadLetter(t *testing.T) {
	store := &fakeStore{}
	c := New(store, fastOptions())
	err := c.ProcessMessage(context.Background(), &Message{Topic: "content.published.v1", Value: []byte(`{"event_id":`)})
	if !isPermanent(err) {
		t.Fatalf("坏信封应判永久错误，实际 %v", err)
	}
	// 拿不到 event_id 时用摘要合成主键，坏消息可追溯而不是被丢弃。
	if len(store.dlqEvents) != 1 || !strings.HasPrefix(store.dlqEvents[0], "malformed_") {
		t.Fatalf("死信主键异常: %v", store.dlqEvents)
	}
	if len(store.dlqEvents[0]) != len("malformed_")+24 {
		t.Fatalf("合成主键长度异常: %q", store.dlqEvents[0])
	}
	assertCalls(t, store, "record_dlq")
}

func TestProcessMessage_TransientFailureSchedulesRetry(t *testing.T) {
	store := &fakeStore{upsertErr: errors.New("esclient: 503 unavailable")}
	c := New(store, fastOptions())
	cause := c.ProcessMessage(context.Background(), contentMessage(t, map[string]interface{}{
		"action": ActionPublish, "content_id": 2, "content_type": 1, "title": "T", "doc_revision": 1,
	}))
	if cause == nil {
		t.Fatal("写索引失败必须把错误返回给上层")
	}
	if isPermanent(cause) {
		t.Fatal("引擎抖动不是永久错误，否则一次 503 就把事件打进死信")
	}

	assertCalls(t, store, "received", "processing:evt-0001", "upsert", "retry")
	if store.lastRetry.eventID != "evt-0001" || store.lastRetry.retryCount != 1 {
		t.Fatalf("重试计数异常: %+v", store.lastRetry)
	}
	now := time.Now().Unix()
	if store.lastRetry.nextRetryAt <= now || store.lastRetry.nextRetryAt > now+60 {
		t.Fatalf("next_retry_at=%d 不在 (now, now+60] 内", store.lastRetry.nextRetryAt)
	}
	if !strings.Contains(store.lastRetry.lastError, "unavailable") {
		t.Fatalf("失败原因未落库: %q", store.lastRetry.lastError)
	}
}

func TestProcessMessage_PermanentFailureDeadLettersWithoutRetry(t *testing.T) {
	store := &fakeStore{}
	c := New(store, fastOptions())
	// 未知 action 属于契约违规：重试一万次也不会成功。
	err := c.ProcessMessage(context.Background(), contentMessage(t, map[string]interface{}{
		"action": "unpublished", "content_id": 3, "content_type": 1, "title": "T", "doc_revision": 1,
	}))
	if !isPermanent(err) {
		t.Fatalf("未知 action 必须判永久错误，实际 %v", err)
	}
	assertCalls(t, store, "received", "processing:evt-0001", "dead_letter:evt-0001", "record_dlq")
	if len(store.upserted) != 0 {
		t.Fatal("未知 action 不得猜测着写索引")
	}
}

func TestProcessMessage_RetriesExhaustedGoesToDeadLetter(t *testing.T) {
	store := &fakeStore{upsertErr: errors.New("engine down")}
	opts := fastOptions()
	opts.MaxRetries = 1 // 首次失败即耗尽配额
	c := New(store, opts)
	if err := c.ProcessMessage(context.Background(), contentMessage(t, map[string]interface{}{
		"action": ActionPublish, "content_id": 4, "content_type": 1, "title": "T", "doc_revision": 1,
	})); err == nil {
		t.Fatal("失败必须返回错误")
	}
	assertCalls(t, store, "received", "processing:evt-0001", "upsert", "dead_letter:evt-0001", "record_dlq")
}

func TestProcessMessage_StoreFailurePropagates(t *testing.T) {
	// 去重表不可用时绝不跳过幂等检查直接写索引。
	store := &fakeStore{receivedErr: errors.New("mysql down")}
	c := New(store, fastOptions())
	if err := c.ProcessMessage(context.Background(), contentMessage(t, map[string]interface{}{
		"action": ActionPublish, "content_id": 5, "content_type": 1, "title": "T", "doc_revision": 1,
	})); err == nil {
		t.Fatal("去重表故障必须返回错误")
	}
	assertCalls(t, store, "received")

	// 状态机写入失败也要冒泡，否则事件被确认却没记录状态，重放时会双写。
	store2 := &fakeStore{succeededErr: errors.New("mysql gone")}
	c2 := New(store2, fastOptions())
	if err := c2.ProcessMessage(context.Background(), contentMessage(t, map[string]interface{}{
		"action": ActionPublish, "content_id": 6, "content_type": 1, "title": "T", "doc_revision": 1,
	})); err == nil {
		t.Fatal("succeeded 落库失败必须返回错误")
	}
}

func TestProcessMessage_TakedownAndDeleteRouting(t *testing.T) {
	cases := []struct {
		action string
		purge  bool
	}{
		{ActionOffline, false},
		{ActionExpired, false},
		{ActionDelete, true},
	}
	for _, c := range cases {
		store := &fakeStore{}
		cons := New(store, fastOptions())
		msg := contentMessage(t, map[string]interface{}{
			"action": c.action, "content_id": 77, "content_type": 2, "doc_revision": 1,
		})
		if err := cons.ProcessMessage(context.Background(), msg); err != nil {
			t.Fatalf("%s: %v", c.action, err)
		}
		want := fmt.Sprintf("delete:77:%v", c.purge)
		assertCalls(t, store, "received", "processing:evt-0001", want, "succeeded:evt-0001")
		if store.deleted[0] != "2_77" {
			t.Fatalf("下架删除定位异常: %v", store.deleted)
		}
	}
}

func TestProcessMessage_TakedownWithoutContentTypeFails(t *testing.T) {
	store := &fakeStore{}
	c := New(store, fastOptions())
	// 没有 content_type 就无法定位 <type>_<id> 主键，必须判永久错误而不是猜类型。
	err := c.ProcessMessage(context.Background(), contentMessage(t, map[string]interface{}{
		"action": ActionOffline, "content_id": 78,
	}))
	if !isPermanent(err) {
		t.Fatalf("缺 content_type 的下架事件必须判永久错误，实际 %v", err)
	}
	assertCalls(t, store, "received", "processing:evt-0001", "dead_letter:evt-0001", "record_dlq")
}

func TestProcessMessage_HeatArrivesBeforeDocRetries(t *testing.T) {
	// 互动比发布更快到达（乱序）：正文投影缺失必须退避等待，不能丢热度。
	store := &fakeStore{patchOutcome: repository.OutcomeMissing}
	c := New(store, fastOptions())
	err := c.ProcessMessage(context.Background(), engagementMessage(t, map[string]interface{}{
		"content_id": 90, "content_type": 1, "action": "like",
		"counters": map[string]interface{}{"like_count": 3, "heat_revision": 10},
	}))
	if err == nil || isPermanent(err) {
		t.Fatalf("正文未就绪应作为可重试错误，实际 %v", err)
	}
	assertCalls(t, store, "received", "processing:evt-0001", "patch_heat", "retry")
}

func TestProcessMessage_HeatStaleSnapshotSucceeds(t *testing.T) {
	store := &fakeStore{patchOutcome: repository.OutcomeSkippedStale}
	c := New(store, fastOptions())
	if err := c.ProcessMessage(context.Background(), engagementMessage(t, map[string]interface{}{
		"content_id": 91, "content_type": 1, "action": "like",
		"counters": map[string]interface{}{"like_count": 1, "heat_revision": 5},
	})); err != nil {
		t.Fatalf("快照过旧是预期路径，应确认成功: %v", err)
	}
	// 重放过旧快照只会再次被拒，因此标记 succeeded 而不是无限重试。
	assertCalls(t, store, "received", "processing:evt-0001", "patch_heat", "succeeded:evt-0001")
}

func TestProcessMessage_MissingHeatSnapshotDeadLetters(t *testing.T) {
	store := &fakeStore{}
	c := New(store, fastOptions())
	if err := c.ProcessMessage(context.Background(), engagementMessage(t, map[string]interface{}{
		"content_id": 92, "content_type": 1, "action": "like",
	})); !errors.Is(err, ErrHeatSnapshotRequired) {
		t.Fatalf("缺绝对快照必须直接死信，实际 %v", err)
	}
	assertCalls(t, store, "received", "processing:evt-0001", "dead_letter:evt-0001", "record_dlq")
}

// TestSweepOnce_AdvancesStateMachine 覆盖进程重启后的续跑路径：
// payload 存在流水表里，不依赖 MQ 重投；四条记录分别走成功 / 继续退避 /
// 配额用尽 / 原文损坏四条分支。
func TestSweepOnce_AdvancesStateMachine(t *testing.T) {
	payload := func(contentID int) string {
		return string(envelopeBytes(t, EventTypeContentPublished, map[string]interface{}{
			"action": ActionPublish, "content_id": contentID, "content_type": 1, "title": "T", "doc_revision": 1,
		}))
	}
	engineDown := errors.New("esclient: HTTP 503")
	// 注意：payload 内的 event_id 是 evt-0001，与流水行的 event_id 不同。
	// 状态推进必须以「行」为准，否则死信会写到不存在的 event_id 上、真实行永远停在 retry。
	store := &fakeStore{
		failFor: map[int64]error{202: engineDown, 203: engineDown},
		due: []*model.SearchConsumerOffset{
			// 1) 重放成功 → succeeded
			{EventID: "e-ok", EventType: EventTypeContentPublished, RetryCount: 1, PayloadJSON: payload(201)},
			// 2) 再次失败但仍有配额 → retry，计数 +1、退避时间后移
			{EventID: "e-again", EventType: EventTypeContentPublished, RetryCount: 1, PayloadJSON: payload(202)},
			// 3) 累计到上限 → dead_letter
			{EventID: "e-last", EventType: EventTypeContentPublished, RetryCount: 4, PayloadJSON: payload(203)},
			// 4) 库内原文已损坏 → 直接死信，不占用配额
			{EventID: "e-broken", EventType: EventTypeContentPublished, RetryCount: 0, PayloadJSON: "{坏数据"},
		},
	}
	c := New(store, fastOptions())

	n, err := c.SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("处理条数 = %d, want 4", n)
	}

	assertCalls(t, store,
		"due",
		"processing:e-ok", "upsert", "succeeded:e-ok",
		"processing:e-again", "upsert", "retry",
		"processing:e-last", "upsert", "dead_letter:e-last", "record_dlq",
		"dead_letter:e-broken", "record_dlq",
	)
	if store.lastRetry.eventID != "e-again" || store.lastRetry.retryCount != 2 {
		t.Fatalf("重试计数未累加: %+v", store.lastRetry)
	}
	now := time.Now().Unix()
	if store.lastRetry.nextRetryAt <= now || store.lastRetry.nextRetryAt > now+60 {
		t.Fatalf("next_retry_at=%d 未落在退避窗口内", store.lastRetry.nextRetryAt)
	}
	if len(store.dlqEvents) != 2 || store.dlqEvents[0] != "e-last" || store.dlqEvents[1] != "e-broken" {
		t.Fatalf("死信登记异常: %v", store.dlqEvents)
	}
	if len(store.upserted) != 1 || store.upserted[0].ContentID != 201 {
		t.Fatalf("成功路径应只写一条投影: %+v", store.upserted)
	}
}

// TestSweepOnce_RecoveryAfterEngineRestored 模拟引擎恢复后重放成功。
func TestSweepOnce_RecoveryAfterEngineRestored(t *testing.T) {
	payload := string(envelopeBytes(t, EventTypeContentPublished, map[string]interface{}{
		"action": ActionPublish, "content_id": 300, "content_type": 1, "title": "T", "doc_revision": 1,
	}))
	store := &fakeStore{due: []*model.SearchConsumerOffset{
		{EventID: "e-recover", EventType: EventTypeContentPublished, RetryCount: 2, PayloadJSON: payload},
	}}
	c := New(store, fastOptions())
	n, err := c.SweepOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertCalls(t, store, "due", "processing:e-recover", "upsert", "succeeded:e-recover")
}

func TestSweepOnce_StoreErrorPropagates(t *testing.T) {
	store := &fakeStore{}
	// 流水表不可用时整轮清扫必须报错，不能返回「处理了 0 条」的假成功。
	c := New(&failingDueStore{fakeStore: store, err: errors.New("mysql gone")}, fastOptions())
	if _, err := c.SweepOnce(context.Background()); err == nil {
		t.Fatal("流水表不可用时 SweepOnce 必须报错")
	}
	if store.calls != nil {
		t.Fatalf("读取失败时不应产生后续写入: %v", store.calls)
	}
}

type failingDueStore struct {
	*fakeStore
	err error
}

func (f *failingDueStore) DueRetryEvents(context.Context, int) ([]*model.SearchConsumerOffset, error) {
	return nil, f.err
}
