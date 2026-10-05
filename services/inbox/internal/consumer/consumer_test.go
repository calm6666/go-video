package consumer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/inbox/internal/config"
	"go-video/services/inbox/internal/repository"
	"go-video/services/inbox/model"
)

// consumer_test.go 覆盖 event_id 去重状态机、退避重试、死信留档与消费者编排。
// Store 与 QueueFactory 都用假件：不连 Kafka/MySQL/Redis，但错误必须真实返回，
// 不用「永不失败的假实现」把状态机掩盖掉（AGENTS.md §9）。

var (
	fixedNow      = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	errDependency = errors.New("mock: dependency unavailable")
)

type claimCall struct {
	eventID string
	stale   int64
}

// fakeStore 复现 inbox_consumer_offset 的状态机语义：
// uniq(event_id) 决定首次/重复，retry 行只有 next_retry_at 到期才能被抢占，
// processing 超过 stale 窗口视为进程崩溃遗留。
type fakeStore struct {
	rows  map[string]*model.ConsumerOffset
	dlq   []*model.DeadLetter
	calls []string
	claim []claimCall

	delivered  []deliverCall
	deliverErr error
	dueRows    []*model.ConsumerOffset
	now        int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rows: make(map[string]*model.ConsumerOffset),
		now:  fixedNow.Unix(),
	}
}

func (s *fakeStore) record(op string) { s.calls = append(s.calls, op) }

func (s *fakeStore) state(t *testing.T, eventID string) string {
	t.Helper()
	row, ok := s.rows[eventID]
	if !ok {
		t.Fatalf("事件 %s 没有状态行", eventID)
	}
	return row.State
}

func (s *fakeStore) ClaimEvent(
	_ context.Context, ev *model.ConsumerOffset, staleSeconds int64,
) (model.ClaimOutcome, *model.ConsumerOffset, error) {
	s.record("claim")
	s.claim = append(s.claim, claimCall{eventID: ev.EventID, stale: staleSeconds})
	if ev.EventID == "" {
		return model.ClaimDeferred, nil, model.ErrEventIDEmpty
	}
	row, exists := s.rows[ev.EventID]
	if !exists {
		created := *ev
		created.State = model.ConsumerStateProcessing
		created.Ctime = s.now
		created.Mtime = s.now
		s.rows[ev.EventID] = &created
		return model.ClaimAcquired, &created, nil
	}
	switch row.State {
	case model.ConsumerStateSucceeded, model.ConsumerStateDeadLetter:
		return model.ClaimDuplicate, row, nil // 终态不可回退
	}
	dueRetry := (row.State == model.ConsumerStateReceived || row.State == model.ConsumerStateRetry) &&
		row.NextRetryAt <= s.now
	staleProcessing := row.State == model.ConsumerStateProcessing && row.Mtime < s.now-staleSeconds
	if !dueRetry && !staleProcessing {
		return model.ClaimDeferred, row, repository.ErrEventDeferred
	}
	row.State = model.ConsumerStateProcessing
	row.Mtime = s.now
	row.PartitionNo = ev.PartitionNo
	return model.ClaimAcquired, row, nil
}

func (s *fakeStore) MarkEventSucceeded(_ context.Context, eventID string) error {
	s.record("succeeded")
	row, ok := s.rows[eventID]
	if !ok {
		return fmt.Errorf("mock: no state row for %s", eventID)
	}
	row.State = model.ConsumerStateSucceeded
	row.NextRetryAt, row.LastError, row.Payload = 0, "", ""
	return nil
}

func (s *fakeStore) MarkEventRetry(
	_ context.Context, eventID string, nextRetryAt int64, reason, payload string,
) error {
	s.record("retry")
	row, ok := s.rows[eventID]
	if !ok {
		return fmt.Errorf("mock: no state row for %s", eventID)
	}
	row.State = model.ConsumerStateRetry
	row.RetryCount++
	row.NextRetryAt = nextRetryAt
	row.LastError = reason
	row.Payload = payload
	return nil
}

func (s *fakeStore) MarkEventDeadLetter(_ context.Context, eventID, reason, payload string) error {
	s.record("dead_letter")
	row, ok := s.rows[eventID]
	if !ok {
		return fmt.Errorf("mock: no state row for %s", eventID)
	}
	row.State = model.ConsumerStateDeadLetter
	row.LastError = reason
	row.Payload = payload
	return nil
}

func (s *fakeStore) SaveDeadLetter(_ context.Context, dl *model.DeadLetter) (bool, error) {
	s.record("save_dlq")
	for _, existing := range s.dlq {
		if existing.Topic == dl.Topic && existing.PayloadDigest == dl.PayloadDigest {
			return false, nil // uniq(topic,payload_digest)
		}
	}
	if dl.PayloadDigest == "" {
		return false, errors.New("mock: dead letter requires a payload digest")
	}
	s.dlq = append(s.dlq, dl)
	return true, nil
}

func (s *fakeStore) DueRetryEvents(_ context.Context, _ int64, _ int32) ([]*model.ConsumerOffset, error) {
	s.record("due")
	rows := s.dueRows
	s.dueRows = nil
	return rows, nil
}

// deliverCall 记录一次投递的参数，便于断言消息内容与收件人。
type deliverCall struct {
	msg        *model.InboxMessage
	recipients []int64
}

func (s *fakeStore) Deliver(_ context.Context, msg *model.InboxMessage, mids []int64) (*repository.DeliverResult, error) {
	s.record("deliver")
	if s.deliverErr != nil {
		return nil, s.deliverErr
	}
	copied := *msg
	recipients := append([]int64(nil), mids...)
	s.delivered = append(s.delivered, deliverCall{msg: &copied, recipients: recipients})
	return &repository.DeliverResult{MsgID: int64(len(s.delivered)), Delivered: int32(len(mids))}, nil
}

func newTestProcessor(store Store, maxAttempts int32) *Processor {
	p := NewProcessor(store, Options{
		MaxAttempts:     maxAttempts,
		BaseBackoff:     10 * time.Second,
		MaxBackoff:      300 * time.Second,
		StaleProcessing: 300 * time.Second,
		RetryBatchLimit: 100,
		SweepIdleWait:   time.Second,
	})
	p.now = func() time.Time { return fixedNow }
	return p
}

func likeEvent(t *testing.T, eventID string) string {
	t.Helper()
	return envelopeJSON(t, eventID, EventTypeEngagementAction,
		`{"action":"like","mid":100,"target_mid":200,"content_title":"标题"}`)
}

// --- 首次投递与去重 ---

func TestProcessDeliversOnceAndMarksSucceeded(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	raw := likeEvent(t, "evt-1")

	if err := p.Process(context.Background(), TopicEngagementAction, raw); err != nil {
		t.Fatalf("首次处理应成功，得到 %v", err)
	}
	if got := countCall(store, "deliver"); got != 1 {
		t.Fatalf("期望投递 1 次，得到 %d", got)
	}
	if state := store.state(t, "evt-1"); state != model.ConsumerStateSucceeded {
		t.Fatalf("期望状态 succeeded，得到 %s", state)
	}
	if store.rows["evt-1"].Payload != "" {
		t.Fatal("成功后必须清空暂存 payload，避免长期堆积原文")
	}
	if store.claim[0].stale != int64((300*time.Second)/time.Second) {
		t.Fatalf("stale 窗口应来自配置，得到 %d", store.claim[0].stale)
	}
}

func TestProcessDuplicateEventDoesNotRedeliver(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	raw := likeEvent(t, "evt-dup")

	for i := 0; i < 5; i++ {
		if err := p.Process(context.Background(), TopicEngagementAction, raw); err != nil {
			t.Fatalf("第 %d 次重复投递不应报错: %v", i+1, err)
		}
	}
	if got := countCall(store, "deliver"); got != 1 {
		t.Fatalf("同 event_id 重复 5 次只应投递 1 次，得到 %d 次", got)
	}
	if state := store.state(t, "evt-dup"); state != model.ConsumerStateSucceeded {
		t.Fatalf("终态不得被重复消息改写，得到 %s", state)
	}
}

func TestProcessConcurrentClaimIsDeferredNotRedelivered(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	raw := likeEvent(t, "evt-race")

	// 模拟另一实例正在处理（processing 且未过期）：本实例不得重复投递。
	store.rows["evt-race"] = &model.ConsumerOffset{
		EventID: "evt-race", State: model.ConsumerStateProcessing,
		Mtime: fixedNow.Unix(), RetryCount: 0,
	}
	err := p.Process(context.Background(), TopicEngagementAction, raw)
	if !errors.Is(err, repository.ErrEventDeferred) {
		t.Fatalf("正被处理的事件应返回退避错误让 MQ 重投，得到 %v", err)
	}
	if got := countCall(store, "deliver"); got != 0 {
		t.Fatalf("抢占失败时不得投递，得到 %d 次", got)
	}
}

func TestProcessSkipsUnsupportedEventTypeWithoutState(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	raw := envelopeJSON(t, "evt-unsub", "moderation.decision", `{"action":"approve"}`)

	if err := p.Process(context.Background(), "moderation.decision.v1", raw); err != nil {
		t.Fatalf("不支持的事件类型应跳过而不是报错: %v", err)
	}
	if len(store.rows) != 0 || len(store.dlq) != 0 {
		t.Fatalf("跳过的事件不得写状态表或死信：rows=%d dlq=%d", len(store.rows), len(store.dlq))
	}
	if got := countCall(store, "claim"); got != 0 {
		t.Fatalf("不支持的事件类型不该抢处理权，得到 %d 次", got)
	}
}

func TestProcessEmptyDeliveryIsAcked(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	for _, value := range []string{"", "   ", "\t\n"} {
		if err := p.Process(context.Background(), TopicEngagementAction, value); err != nil {
			t.Fatalf("空投递 %q 应直接确认: %v", value, err)
		}
	}
	if len(store.calls) != 0 {
		t.Fatalf("空投递不得触碰任何依赖，得到 %v", store.calls)
	}
}

// --- 退避重试与死信 ---

func TestProcessTransientFailureSchedulesBackoff(t *testing.T) {
	store := newFakeStore()
	store.deliverErr = errDependency
	p := newTestProcessor(store, 8)

	err := p.Process(context.Background(), TopicEngagementAction, likeEvent(t, "evt-retry"))
	if !errors.Is(err, errDependency) {
		t.Fatalf("可重试错误必须原样返回（不提交位点），得到 %v", err)
	}
	row := store.rows["evt-retry"]
	if row.State != model.ConsumerStateRetry || row.RetryCount != 1 {
		t.Fatalf("期望 retry/1，得到 %s/%d", row.State, row.RetryCount)
	}
	want := fixedNow.Add(10 * time.Second).Unix()
	if row.NextRetryAt != want {
		t.Fatalf("首次失败退避应为 base*1，期望 %d 得到 %d", want, row.NextRetryAt)
	}
	if !strings.Contains(row.Payload, `"event_id":"evt-retry"`) {
		t.Fatal("退避行必须暂存原文，否则进程崩溃后无法重放")
	}
	if len(store.dlq) != 0 {
		t.Fatal("还没到重试上限，不该留死信")
	}
}

func TestProcessRetriesAdvanceBackoffThenDeadLetter(t *testing.T) {
	store := newFakeStore()
	store.deliverErr = errDependency
	p := newTestProcessor(store, 3)
	raw := likeEvent(t, "evt-exhaust")

	// 每次处理前把退避窗口打开，等价于「清扫器判定已到期」。
	for attempt := 1; attempt <= 3; attempt++ {
		if row, ok := store.rows["evt-exhaust"]; ok {
			row.NextRetryAt = fixedNow.Unix() - 1
		}
		if err := p.Process(context.Background(), TopicEngagementAction, raw); err == nil && attempt < 3 {
			t.Fatalf("第 %d 次失败应返回错误", attempt)
		}
	}
	row := store.rows["evt-exhaust"]
	if row.State != model.ConsumerStateDeadLetter {
		t.Fatalf("达到 MaxAttempts=3 后应判死，得到 %s", row.State)
	}
	if row.RetryCount != 2 {
		t.Fatalf("前两次失败各记一次 retry，期望 2 得到 %d", row.RetryCount)
	}
	if len(store.dlq) != 1 {
		t.Fatalf("判死必须留档 1 条，得到 %d", len(store.dlq))
	}
	if store.dlq[0].EventID != "evt-exhaust" || store.dlq[0].Reason == "" {
		t.Fatalf("死信缺少定位信息：%+v", store.dlq[0])
	}
	// 判死后重投：直接确认，不再投递、不再新增死信。
	if err := p.Process(context.Background(), TopicEngagementAction, raw); err != nil {
		t.Fatalf("已判死的事件应被确认: %v", err)
	}
	if len(store.dlq) != 1 {
		t.Fatalf("重复判死不得新增死信，得到 %d", len(store.dlq))
	}
	if got := countCall(store, "deliver"); got != 3 {
		t.Fatalf("判死前共尝试 3 次，得到 %d", got)
	}
}

func TestProcessPermanentContractErrorDeadLettersImmediately(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	// payload 为 {}：契约类错误，重试也不会变好，必须直接判死而不是消耗退避配额。
	raw := envelopeJSON(t, "evt-perm", EventTypeEngagementAction, `{}`)

	if err := p.Process(context.Background(), TopicEngagementAction, raw); err != nil {
		t.Fatalf("已留档的死信要确认位点，得到 %v", err)
	}
	if got := countCall(store, "retry"); got != 0 {
		t.Fatalf("永久错误不得进入退避，得到 %d 次", got)
	}
	if state := store.state(t, "evt-perm"); state != model.ConsumerStateDeadLetter {
		t.Fatalf("期望 dead_letter，得到 %s", state)
	}
	if len(store.dlq) != 1 {
		t.Fatalf("期望留档 1 条死信，得到 %d", len(store.dlq))
	}
}

func TestProcessMalformedEnvelopeArchivedWithoutClaim(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	garbage := `{"event_id":"oops","payload":`

	for i := 0; i < 3; i++ {
		if err := p.Process(context.Background(), TopicEngagementAction, garbage); err != nil {
			t.Fatalf("毒消息留档后必须确认位点，否则分区被卡住: %v", err)
		}
	}
	if got := countCall(store, "claim"); got != 0 {
		t.Fatalf("连 event_id 都拿不到时不得抢处理权，得到 %d 次", got)
	}
	if len(store.dlq) != 1 {
		t.Fatalf("同一条毒消息按摘要幂等，期望 1 条留档，得到 %d", len(store.dlq))
	}
	if store.dlq[0].PayloadDigest == "" || store.dlq[0].Topic != TopicEngagementAction {
		t.Fatalf("死信缺少摘要或 topic：%+v", store.dlq[0])
	}
	if strings.Contains(store.dlq[0].PayloadPreview, `"payload"`) &&
		strings.HasSuffix(store.dlq[0].PayloadPreview, "{") {
		t.Fatal("预览被截断成半条语句时不该保留原文结构")
	}
}

func TestProcessOversizedEventIDIsArchivedNotRetried(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	huge := strings.Repeat("x", maxEventIDLen+1)
	raw := envelopeJSON(t, huge, EventTypeEngagementAction,
		`{"action":"like","mid":1,"target_mid":2}`)

	if err := p.Process(context.Background(), TopicEngagementAction, raw); err != nil {
		t.Fatalf("超长 event_id 应按毒消息留档: %v", err)
	}
	if got := countCall(store, "claim"); got != 0 {
		t.Fatalf("落不进 event_id 列的事件不该抢处理权空转退避，得到 %d 次", got)
	}
	if len(store.dlq) != 1 {
		t.Fatalf("期望留档 1 条，得到 %d", len(store.dlq))
	}
}

func TestDeadLetterArchiveFailureKeepsOffsetUncommitted(t *testing.T) {
	store := dlqFailingStore{fakeStore: newFakeStore()}
	p := newTestProcessor(store, 8)
	raw := envelopeJSON(t, "evt-perm2", EventTypeEngagementAction, `{}`)

	if err := p.Process(context.Background(), TopicEngagementAction, raw); err == nil {
		t.Fatal("死信留档失败必须返回错误（不提交位点），否则消息静默丢失")
	}
	if store.state2(model.ConsumerStateDeadLetter) {
		t.Fatal("留档失败时不得把事件标成 dead_letter")
	}
}

// dlqFailingStore 只覆盖 SaveDeadLetter 的错误分支。
type dlqFailingStore struct{ *fakeStore }

func (s dlqFailingStore) SaveDeadLetter(context.Context, *model.DeadLetter) (bool, error) {
	s.record("save_dlq")
	return false, errDependency
}

func (s dlqFailingStore) state2(want string) bool {
	for _, row := range s.rows {
		if row != nil && row.State == want {
			return true
		}
	}
	return false
}

// --- 清扫循环 ---

func TestSweepOnceReprocessesDueRetryRows(t *testing.T) {
	store := newFakeStore()
	raw := likeEvent(t, "evt-sweep")
	// 先制造一条失败的 retry 行。
	store.deliverErr = errDependency
	p := newTestProcessor(store, 8)
	if err := p.Process(context.Background(), TopicEngagementAction, raw); err == nil {
		t.Fatal("前置条件：首次处理应失败")
	}
	store.deliverErr = nil

	// DueRetryEvents 在真实 SQL 里只返回 next_retry_at <= now 的行，假件按同一约定放开退避窗口。
	store.rows["evt-sweep"].NextRetryAt = fixedNow.Unix() - 1
	store.dueRows = []*model.ConsumerOffset{store.rows["evt-sweep"]}
	n, err := p.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("期望处理 1 条，得到 %d", n)
	}
	if state := store.state(t, "evt-sweep"); state != model.ConsumerStateSucceeded {
		t.Fatalf("清扫后应收敛为 succeeded，得到 %s", state)
	}
	if got := countCall(store, "deliver"); got != 2 {
		t.Fatalf("期望共投递 2 次（含重试），得到 %d", got)
	}
}

func TestSweepOnceWithUnparseablePayloadDeadLetters(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	orphan := &model.ConsumerOffset{
		EventID: "evt-orphan", EventType: EventTypeEngagementAction, Topic: TopicEngagementAction,
		State: model.ConsumerStateRetry, Payload: `{"broken":`,
	}
	store.rows["evt-orphan"] = orphan
	store.dueRows = []*model.ConsumerOffset{orphan}

	if _, err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("单条无法重放不应让整轮清扫失败: %v", err)
	}
	if orphan.State != model.ConsumerStateDeadLetter {
		t.Fatalf("无法重放的行应转死信交人工，得到 %s", orphan.State)
	}
	if len(store.dlq) != 1 {
		t.Fatalf("期望留档 1 条，得到 %d", len(store.dlq))
	}
}

func TestSweepOnceErrorPropagates(t *testing.T) {
	store := dueFailingStore{fakeStore: newFakeStore()}
	p := newTestProcessor(store, 8)
	if _, err := p.SweepOnce(context.Background()); err == nil {
		t.Fatal("读取到期事件失败必须上报")
	}
}

type dueFailingStore struct{ *fakeStore }

func (s dueFailingStore) DueRetryEvents(context.Context, int64, int32) ([]*model.ConsumerOffset, error) {
	s.record("due")
	return nil, errDependency
}

func TestRunRetrySweeperStopsOnContextCancel(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.RunRetrySweeper(ctx) }()

	// SweepIdleWait=1s：等到第一轮 due 查询发生后再取消，避免依赖 sleep 时长。
	for i := 0; i < 100 && countCall(store, "due") == 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后应返回 context.Canceled，得到 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunRetrySweeper 未在 ctx 取消后退出")
	}
	if countCall(store, "due") == 0 {
		t.Fatal("清扫循环没有真正跑过")
	}
}

// --- 退避函数 ---

func TestBackoffDelayGrowsAndCaps(t *testing.T) {
	base, max := 10*time.Second, 300*time.Second
	prev := time.Duration(0)
	for attempts := int32(1); attempts <= 12; attempts++ {
		delay := BackoffDelay(attempts, base, max)
		if delay < base || delay > max {
			t.Fatalf("attempts=%d 退避 %v 越界 [%v,%v]", attempts, delay, base, max)
		}
		if attempts > 1 && delay < prev {
			t.Fatalf("attempts=%d 退避必须单调不减：%v < %v", attempts, delay, prev)
		}
		prev = delay
	}
	if got := BackoffDelay(1, base, max); got != base {
		t.Fatalf("首次失败也应等 base，得到 %v", got)
	}
	if got := BackoffDelay(0, base, max); got != base {
		t.Fatalf("attempts=0 应按首次处理，得到 %v", got)
	}
}

func TestOptionsFromUsesDefaultsAndClampsMax(t *testing.T) {
	opts := OptionsFrom(config.KafkaConf{})
	if opts.MaxAttempts != 0 || opts.BaseBackoff != 10*time.Second || opts.MaxBackoff != 1800*time.Second {
		t.Fatalf("零值配置应落到安全默认：%+v", opts)
	}
	if opts.StaleProcessing != 300*time.Second || opts.RetryBatchLimit != 100 {
		t.Fatalf("清扫参数不符：%+v", opts)
	}
	// max < base 时抬高到 base，否则退避会倒挂。
	inverted := OptionsFrom(config.KafkaConf{RetryBaseSeconds: 600, RetryMaxSeconds: 60})
	if inverted.MaxBackoff < inverted.BaseBackoff {
		t.Fatalf("max=%v 不得小于 base=%v", inverted.MaxBackoff, inverted.BaseBackoff)
	}
	p := NewProcessor(newFakeStore(), Options{}) // normalize()
	if got := p.Options(); got.MaxAttempts != 8 || got.BaseBackoff <= 0 {
		t.Fatalf("直接构造的 Options 也必须被规范化：%+v", got)
	}
}

// --- Supervisor / 配置校验 ---

func validKafkaConf() config.KafkaConf {
	return config.KafkaConf{
		Enabled: true, Brokers: []string{"127.0.0.1:9092"}, Group: "inbox.v1.consumer",
		Offset: "last", Conns: 1, Consumers: 2, Processors: 4,
		MaxAttempts: 8, RetryBaseSeconds: 10, RetryMaxSeconds: 1800, StaleProcessingSeconds: 300,
	}
}

func validConfig() config.Config {
	c := config.Config{}
	c.Name = "inbox.v1.rpc"
	c.Kafka = validKafkaConf()
	return c
}

func TestValidateKafkaNamesTheBrokenKey(t *testing.T) {
	if err := ValidateKafka(validKafkaConf()); err != nil {
		t.Fatalf("完整配置应通过校验: %v", err)
	}
	cases := []struct {
		mutate func(k *config.KafkaConf)
		wants  string
	}{
		{func(k *config.KafkaConf) { k.Brokers = nil }, "Kafka.Brokers"},
		{func(k *config.KafkaConf) { k.Group = "  " }, "Kafka.Group"},
		{func(k *config.KafkaConf) { k.Offset = "earliest" }, "Kafka.Offset"},
		{func(k *config.KafkaConf) { k.Conns = 0 }, "Kafka.Conns"},
		{func(k *config.KafkaConf) { k.Consumers = -1 }, "Kafka.Consumers"},
		{func(k *config.KafkaConf) { k.Processors = 0 }, "Kafka.Processors"},
		{func(k *config.KafkaConf) { k.MaxAttempts = 0 }, "Kafka.MaxAttempts"},
		{func(k *config.KafkaConf) { k.RetryBaseSeconds = 0 }, "Kafka.RetryBaseSeconds"},
		{func(k *config.KafkaConf) { k.StaleProcessingSeconds = 0 }, "Kafka.StaleProcessingSeconds"},
		{func(k *config.KafkaConf) { k.Username = "user" }, "Kafka.Username 与 Kafka.Password"},
		{func(k *config.KafkaConf) { k.Password = "pass" }, "Kafka.Username 与 Kafka.Password"},
	}
	for _, c := range cases {
		k := validKafkaConf()
		c.mutate(&k)
		err := ValidateKafka(k)
		if err == nil || !strings.Contains(err.Error(), c.wants) {
			t.Fatalf("错误里应点名 %s，得到 %v", c.wants, err)
		}
	}
}

func TestNewSupervisorRejectsIncompleteWiring(t *testing.T) {
	c := validConfig()
	c.Kafka.Brokers = nil
	if _, err := NewSupervisor(c, newFakeStore(), &fakeFactory{}); err == nil {
		t.Fatal("配置不完整时 NewSupervisor 必须报错（不能静默不消费）")
	}
	if _, err := NewSupervisor(validConfig(), nil, &fakeFactory{}); err == nil {
		t.Fatal("缺少 store 必须报错")
	}
	if _, err := NewSupervisor(validConfig(), newFakeStore(), nil); err == nil {
		t.Fatal("缺少 factory 必须报错")
	}
}

func TestSupervisorDeduplicatesTopics(t *testing.T) {
	c := validConfig()
	c.Kafka.Topics = []string{" engagement.action.v1 ", "engagement.action.v1", "live.state.v1", ""}
	if _, err := NewSupervisor(c, newFakeStore(), &fakeFactory{}); err == nil {
		t.Fatal("Topics 含空串必须报错")
	}
	c.Kafka.Topics = c.Kafka.Topics[:3]
	sup, err := NewSupervisor(c, newFakeStore(), &fakeFactory{})
	if err != nil {
		t.Fatalf("NewSupervisor 失败: %v", err)
	}
	want := []string{"engagement.action.v1", "live.state.v1"}
	if strings.Join(sup.Topics(), ",") != strings.Join(want, ",") {
		t.Fatalf("期望去重后的 topic %v，得到 %v", want, sup.Topics())
	}
	// 未显式配置 Topics 时使用 DefaultTopics。
	c2 := validConfig()
	c2.Kafka.Topics = nil
	sup2, err := NewSupervisor(c2, newFakeStore(), &fakeFactory{})
	if err != nil {
		t.Fatalf("NewSupervisor(default topics) 失败: %v", err)
	}
	if len(sup2.Topics()) != len(config.DefaultTopics) {
		t.Fatalf("默认 topic 数量不符：%v", sup2.Topics())
	}
}

type fakeQueue struct {
	started, stopped int
	topic            string
}

func (q *fakeQueue) Start() { q.started++ }
func (q *fakeQueue) Stop()  { q.stopped++ }

type fakeFactory struct {
	created []*fakeQueue
	failOn  string
	err     error
}

func (f *fakeFactory) New(s Settings, topic string, h *Handler) (MessageQueue, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.failOn == topic {
		return nil, errDependency
	}
	if len(s.Brokers) == 0 || s.Group == "" || h == nil || h.Topic() != topic {
		return nil, fmt.Errorf("mock: 传给工厂的参数不完整 %+v", s)
	}
	q := &fakeQueue{topic: topic}
	f.created = append(f.created, q)
	return q, nil
}

func TestSupervisorStartStopLifecycle(t *testing.T) {
	factory := &fakeFactory{}
	sup, err := NewSupervisor(validConfig(), newFakeStore(), factory)
	if err != nil {
		t.Fatalf("NewSupervisor 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if len(factory.created) != len(sup.Topics()) {
		t.Fatalf("每个 topic 一个消费者：期望 %d 得到 %d", len(sup.Topics()), len(factory.created))
	}
	for _, q := range factory.created {
		if q.started != 1 {
			t.Fatalf("topic %s 未启动", q.topic)
		}
	}
	if err := sup.Start(ctx); err == nil {
		t.Fatal("重复 Start 必须报错，而不是起两套消费者重复消费")
	}
	sup.Stop()
	for _, q := range factory.created {
		if q.stopped != 1 {
			t.Fatalf("topic %s 未停止", q.topic)
		}
	}
	sup.Stop() // 幂等
}

func TestSupervisorStartRollsBackOnFailure(t *testing.T) {
	topics := []string{"engagement.action.v1", "content.published.v1", "live.state.v1"}
	c := validConfig()
	c.Kafka.Topics = topics
	factory := &fakeFactory{failOn: topics[2]}
	sup, err := NewSupervisor(c, newFakeStore(), factory)
	if err != nil {
		t.Fatalf("NewSupervisor 失败: %v", err)
	}
	if err := sup.Start(context.Background()); err == nil {
		t.Fatal("建队列失败必须报错")
	} else if !strings.Contains(err.Error(), topics[2]) {
		t.Fatalf("错误里应点名失败的 topic，得到 %v", err)
	}
	sup.Stop() // 回滚路径也要能安全收尾
}

func TestHandlerConsumePassesTopic(t *testing.T) {
	store := newFakeStore()
	p := newTestProcessor(store, 8)
	h := NewHandler(TopicEngagementAction, p)
	if h.Topic() != TopicEngagementAction {
		t.Fatalf("handler 应绑定 topic，得到 %s", h.Topic())
	}
	if err := h.Consume(context.Background(), "ignored-key", likeEvent(t, "evt-handler")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if countCall(store, "deliver") != 1 {
		t.Fatalf("Consume 未走到投递：%v", store.calls)
	}
	// Handler 必须把绑定的 topic 传给 Processor：死信留档依赖它定位来源。
	if store.delivered[0].msg.BizType != EventTypeEngagementAction {
		t.Fatalf("topic 绑定后消息口径不符：%+v", store.delivered[0].msg)
	}
	if got := store.rows["evt-handler"].Topic; got != TopicEngagementAction {
		t.Fatalf("状态行 topic 期望 %s，得到 %s", TopicEngagementAction, got)
	}
}

func countCall(store *fakeStore, op string) int {
	n := 0
	for _, c := range store.calls {
		if c == op {
			n++
		}
	}
	return n
}
