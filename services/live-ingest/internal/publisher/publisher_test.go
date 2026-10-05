package publisher

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go-video/services/live-ingest/internal/config"
)

// fakeNow 是固定时钟：退避时间与位点都必须变成可预测的字面量，
// 否则「next_retry_at 是否真的按指数推进」这类断言只能靠放宽窗口糊过去。
var fakeNow = time.Unix(1_700_000_000, 0).UTC()

// rowState 是替身记住的一行最终结论。
type rowState struct {
	state       string // pending / published / failed
	retryCount  int32
	nextRetryAt int64
	lastError   string
}

// fakeStore 是 Store 的替身：按批次返回预设行，并把每次调用留痕。
// 带锁是因为启停用例会在后台循环运行时读这些痕迹（-race 下不能裸读）。
type fakeStore struct {
	mu        sync.Mutex
	batches   [][]*Record
	lists     int
	calls     []string
	states    map[int64]*rowState
	listNow   int64
	listLimit int32

	listErr   error
	pubErr    error
	retryErr  error
	failedErr error
}

func newFakeStore(rows ...*Record) *fakeStore {
	return &fakeStore{batches: [][]*Record{rows}, states: map[int64]*rowState{}}
}

func (f *fakeStore) ListPending(_ context.Context, now int64, limit int32) ([]*Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	f.calls = append(f.calls, "list")
	f.listNow, f.listLimit = now, limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	idx := f.lists - 1 // 第 1 次 list 用第 0 批，之后依次推进，越界后回空批
	if idx >= len(f.batches) {
		return nil, nil
	}
	return f.batches[idx], nil
}

func (f *fakeStore) MarkPublished(_ context.Context, id, publishedAt int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("published:%d@%d", id, publishedAt))
	if f.pubErr != nil {
		return f.pubErr
	}
	f.states[id] = &rowState{state: "published"}
	return nil
}

func (f *fakeStore) MarkRetry(_ context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("retry:%d:%d@%d", id, retryCount, nextRetryAt))
	if f.retryErr != nil {
		return f.retryErr
	}
	f.states[id] = &rowState{state: "pending", retryCount: retryCount, nextRetryAt: nextRetryAt, lastError: lastError}
	return nil
}

func (f *fakeStore) MarkFailed(_ context.Context, id int64, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("failed:%d", id))
	if f.failedErr != nil {
		return f.failedErr
	}
	f.states[id] = &rowState{state: "failed", lastError: lastError}
	return nil
}

func (f *fakeStore) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeStore) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

func (f *fakeStore) listArgs() (int64, int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listNow, f.listLimit
}

func (f *fakeStore) state(id int64) *rowState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[id]
}

// sendCall 记录一次投递尝试的全部入参（payload 单列，便于断言「原样投递」）。
type sendCall struct {
	topic       string
	key         string
	payload     string
	hasDeadline bool
	timeout     time.Duration
}

// fakeSender 是 Sender 的替身。errOn 按 1 起的调用序号决定失败，
// 这样「第一次失败、第二次成功」这种退避恢复路径可以精确构造。
type fakeSender struct {
	mu        sync.Mutex
	sent      []sendCall
	err       error
	errOn     map[int]error
	closeCnt  int
	closeErr  error
	failCount int
}

func (f *fakeSender) Send(ctx context.Context, topic, key, payload string) error {
	timeout, hasDeadline := time.Duration(0), false
	if d, ok := ctx.Deadline(); ok {
		timeout, hasDeadline = time.Until(d), true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCount++
	f.sent = append(f.sent, sendCall{topic: topic, key: key, payload: payload, hasDeadline: hasDeadline, timeout: timeout})
	if f.errOn != nil {
		if err, ok := f.errOn[f.failCount]; ok {
			return err
		}
	}
	return f.err
}

func (f *fakeSender) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCnt++
	return f.closeErr
}

func (f *fakeSender) sentCalls() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendCall(nil), f.sent...)
}

func (f *fakeSender) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCnt
}

func testOptions() Options {
	return Options{
		Interval:    2 * time.Second,
		Batch:       100,
		MaxAttempts: 5,
		BaseBackoff: 2 * time.Second,
		MaxBackoff:  1800 * time.Second,
		SendTimeout: 5 * time.Second,
	}
}

// newTestPublisher 用固定时钟装配，返回发布器与投递替身。
func newTestPublisher(t *testing.T, store Store, sender *fakeSender, mutate func(*Options)) *Publisher {
	t.Helper()
	opts := testOptions()
	if mutate != nil {
		mutate(&opts)
	}
	pub, err := New(store, sender, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pub.now = func() time.Time { return fakeNow }
	return pub
}

func okRecord(id int64, eventID string) *Record {
	return &Record{
		ID:      id,
		EventID: eventID,
		Topic:   RequiredTopic(),
		Key:     fmt.Sprintf("stream-%d", id),
		Payload: fmt.Sprintf(`{"event_id":%q}`, eventID),
	}
}

// TestRunOncePublishesInIdOrderAndMarksPublished 钉住成功路径：
// 按 store 给出的顺序逐条同步投递，成功后才置已发布，位点时间用注入的时钟。
func TestRunOncePublishesInIdOrderAndMarksPublished(t *testing.T) {
	store := newFakeStore(okRecord(7, "e7"), okRecord(8, "e8"), okRecord(9, "e9"))
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	handled, err := pub.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if handled != 3 {
		t.Fatalf("handled=%d，期望 3", handled)
	}
	nowArg, limitArg := store.listArgs()
	if nowArg != fakeNow.Unix() {
		t.Errorf("ListPending 的 now=%d，期望注入时钟 %d", nowArg, fakeNow.Unix())
	}
	if limitArg != testOptions().Batch {
		t.Errorf("ListPending 的 limit=%d，期望 Batch=%d", limitArg, testOptions().Batch)
	}
	want := []string{
		"list",
		"published:7@" + strconv.FormatInt(fakeNow.Unix(), 10),
		"published:8@" + strconv.FormatInt(fakeNow.Unix(), 10),
		"published:9@" + strconv.FormatInt(fakeNow.Unix(), 10),
	}
	if got := strings.Join(store.callLog(), ","); got != strings.Join(want, ",") {
		t.Fatalf("状态写入序列=%s，期望 %v", got, want)
	}
	sent := sender.sentCalls()
	if len(sent) != 3 || sent[0].key != "stream-7" || sent[2].payload != `{"event_id":"e9"}` {
		t.Fatalf("投递内容与顺序不符：%+v", sent)
	}
	for _, c := range sent {
		if c.topic != RequiredTopic() {
			t.Errorf("topic=%q，期望 %q", c.topic, RequiredTopic())
		}
	}
	published, retried, failed, lastErr := pub.Stats()
	if published != 3 || retried != 0 || failed != 0 || lastErr != "" {
		t.Fatalf("Stats=(%d,%d,%d,%q)，期望 (3,0,0,\"\")", published, retried, failed, lastErr)
	}
}

// TestRunOnceKeepsStoreOrder 证明发布器不自己排序：
// id 升序是 model.ListPending 的 SQL 职责，这里重排会掩盖「忘写 ORDER BY」的真问题，
// 而忘写 ORDER BY 的后果是同流事件乱序，live-room 的 seq 守卫会把它判成回退。
func TestRunOnceKeepsStoreOrder(t *testing.T) {
	store := newFakeStore(okRecord(10, "e10"), okRecord(2, "e2"), okRecord(8, "e8"))
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	if _, err := pub.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var keys []string
	for _, c := range sender.sentCalls() {
		keys = append(keys, c.key)
	}
	want := []string{"stream-10", "stream-2", "stream-8"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("投递顺序=%v，期望沿用 store 顺序 %v", keys, want)
	}
}

// TestSendContextCarriesTimeout 钉住「每条投递都有超时」：
// 没有 deadline 时一次 broker hang 会占住整个发布循环，退避与判死都不会发生。
func TestSendContextCarriesTimeout(t *testing.T) {
	store := newFakeStore(okRecord(1, "e1"))
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	if _, err := pub.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	sent := sender.sentCalls()
	if len(sent) != 1 || !sent[0].hasDeadline {
		t.Fatalf("投递没有携带 deadline：%+v", sent)
	}
	if got := sent[0].timeout; got <= 0 || got > 5*time.Second {
		t.Errorf("投递超时=%s，期望为正且不超过 SendTimeout=5s", got)
	}
}

// TestTransientSendFailureSchedulesBackoff 钉住第一次失败的落库形态：
// 行仍留在待发布，retry_count=1，next_retry_at = now + 基数，错误原文进 last_error。
func TestTransientSendFailureSchedulesBackoff(t *testing.T) {
	store := newFakeStore(okRecord(3, "e3"))
	sendErr := errors.New("dial tcp 127.0.0.1:9092: connection refused")
	pub := newTestPublisher(t, store, &fakeSender{err: sendErr}, nil)

	handled, err := pub.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("投递失败不该让 RunOnce 报错（结论已按状态机落库）：%v", err)
	}
	if handled != 1 {
		t.Fatalf("handled=%d", handled)
	}
	st := store.state(3)
	if st == nil || st.state != "pending" {
		t.Fatalf("行状态=%+v，期望仍是 pending", st)
	}
	wantNext := fakeNow.Add(2 * time.Second).Unix()
	if st.retryCount != 1 || st.nextRetryAt != wantNext {
		t.Errorf("retry_count=%d next_retry_at=%d，期望 1 / %d", st.retryCount, st.nextRetryAt, wantNext)
	}
	if st.lastError != sendErr.Error() {
		t.Errorf("last_error=%q，期望保留投递错误原文 %q", st.lastError, sendErr)
	}
	if _, retried, _, _ := pub.Stats(); retried != 1 {
		t.Errorf("retried 计数=%d，期望 1", retried)
	}
	for _, c := range store.callLog() {
		if strings.HasPrefix(c, "published:") {
			t.Fatalf("投递失败却写了已发布：%v", store.callLog())
		}
	}
}

// TestBackoffIsExponentialAndCapped 用边界表钉住退避曲线（含 2^30 溢出防护）。
func TestBackoffIsExponentialAndCapped(t *testing.T) {
	pub, err := New(newFakeStore(), &fakeSender{}, Options{
		Interval: time.Second, Batch: 10, MaxAttempts: 100,
		BaseBackoff: 2 * time.Second, MaxBackoff: 30 * time.Second, SendTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		nextCount int32
		wantDelay time.Duration
	}{
		{1, 2 * time.Second},     // 基数
		{2, 4 * time.Second},     // 2^1
		{3, 8 * time.Second},     // 2^2
		{4, 16 * time.Second},    // 2^3
		{5, 30 * time.Second},    // 32s > 上限 30s，夹住
		{20, 30 * time.Second},   // 远大于上限
		{1000, 30 * time.Second}, // 大指数不允许溢出
		{0, 2 * time.Second},     // retry_count 异常时不允许把退避算成负数
	}
	for _, tc := range cases {
		got := pub.nextRetryAt(fakeNow, tc.nextCount)
		want := fakeNow.Add(tc.wantDelay).Unix()
		if got != want {
			t.Errorf("nextCount=%d 得到 next_retry_at=%d，期望 %d（延迟 %s）", tc.nextCount, got, want, tc.wantDelay)
		}
		if got < fakeNow.Unix() {
			t.Errorf("nextCount=%d 的退避时间早于当前时间，会把行变成「立即重投」", tc.nextCount)
		}
	}
}

// TestRetryBoundaryDecidesFailedVsRetry 钉住「达到上限即判死」的边界不 off-by-one：
// retry_count = MaxAttempts-1 时这次失败后必须判死，= MaxAttempts-2 时必须还给退避。
func TestRetryBoundaryDecidesFailedVsRetry(t *testing.T) {
	cases := []struct {
		name       string
		retryCount int32
		wantFailed bool
	}{
		{"还差一次", 3, false},
		{"最后一次尝试", 4, true},
		{"列值已越界", 7, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := okRecord(5, "e5")
			rec.RetryCount = tc.retryCount
			store := newFakeStore(rec)
			pub := newTestPublisher(t, store, &fakeSender{err: errors.New("broker unavailable")}, nil)

			if _, err := pub.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			st := store.state(5)
			if st == nil {
				t.Fatal("没有任何状态写入")
			}
			if tc.wantFailed {
				if st.state != "failed" || !strings.Contains(st.lastError, "retries exhausted") {
					t.Fatalf("state=%q lastError=%q，期望 failed + retries exhausted", st.state, st.lastError)
				}
				if !strings.Contains(st.lastError, "broker unavailable") {
					t.Errorf("判死原因丢了底层错误：%q", st.lastError)
				}
				return
			}
			if st.state != "pending" || st.retryCount != 4 {
				t.Fatalf("state=%q retry_count=%d，期望继续退避到 4", st.state, st.retryCount)
			}
		})
	}
}

// TestDefectiveRowIsFailedWithoutSend 钉住不可发布行的处理：不投、不占重试次数，直接判死。
func TestDefectiveRowIsFailedWithoutSend(t *testing.T) {
	rec := okRecord(6, "e6")
	rec.Defect = `payload event_id="other" 与列 event_id="e6" 不一致`
	store := newFakeStore(rec)
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	if _, err := pub.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(sender.sentCalls()) != 0 {
		t.Fatalf("缺陷行仍然被投递：%+v", sender.sentCalls())
	}
	st := store.state(6)
	if st == nil || st.state != "failed" {
		t.Fatalf("state=%+v，期望 failed", st)
	}
	if !strings.HasPrefix(st.lastError, "unpublishable: ") || !strings.Contains(st.lastError, "不一致") {
		t.Errorf("判死原因=%q，期望带 unpublishable 前缀并保留缺陷描述", st.lastError)
	}
	if _, _, failed, _ := pub.Stats(); failed != 1 {
		t.Errorf("failed 计数=%d，期望 1", failed)
	}
}

// TestStoreWriteErrorsPropagate 钉住四类库操作失败都必须冒泡并中断本批：
// 吞掉它们会让「这一行有结论了」成为假象，而实际行还停在待发布。
func TestStoreWriteErrorsPropagate(t *testing.T) {
	storeErr := errors.New("deadline exceeded")
	cases := []struct {
		name       string
		retryCount int32
		sendFails  bool
		perRow     bool // 是否已经定位到具体某一行（读库失败时还没有行，不能报 outbox id）
		setup      func(*fakeStore)
		want       string
	}{
		{name: "读库失败", setup: func(f *fakeStore) { f.listErr = storeErr }, want: "读取待发布事件"},
		{name: "已发布写失败", perRow: true, setup: func(f *fakeStore) { f.pubErr = storeErr }, want: "标记已发布"},
		{name: "退避写失败", perRow: true, sendFails: true, setup: func(f *fakeStore) { f.retryErr = storeErr }, want: "记录重试"},
		{name: "判死写失败", perRow: true, retryCount: 4, sendFails: true, setup: func(f *fakeStore) { f.failedErr = storeErr }, want: "判死写库"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := okRecord(1, "e1")
			rec.RetryCount = tc.retryCount
			store := newFakeStore(rec, okRecord(2, "e2"))
			tc.setup(store)
			sender := &fakeSender{}
			if tc.sendFails {
				sender.err = errors.New("broker down")
			}
			pub := newTestPublisher(t, store, sender, nil)

			handled, err := pub.RunOnce(context.Background())
			if err == nil {
				t.Fatal("状态写库失败必须返回错误")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误=%v，期望点名 %q", err, tc.want)
			}
			if tc.perRow {
				if !strings.Contains(err.Error(), "outbox id=1") {
					t.Errorf("错误=%v，写库失败必须带上出问题的 outbox id", err)
				}
			} else if strings.Contains(err.Error(), "outbox id=") {
				// 读库失败发生在「还没有任何一行」的时刻，编一个 id 会把排障指向错误的行。
				t.Errorf("读库失败不应报 outbox id：%v", err)
			}
			if handled != 0 {
				t.Errorf("handled=%d，写库失败的一行不该计入已处理", handled)
			}
			// 本批必须在中断处停下：第二行不该被处理。
			for _, c := range store.callLog() {
				if strings.Contains(c, ":2") {
					t.Fatalf("本批未在中断处停下：%v", store.callLog())
				}
			}
			// 读库失败时连投递都不该发生。
			if !tc.perRow && len(sender.sentCalls()) != 0 {
				t.Errorf("读库失败仍尝试投递：%+v", sender.sentCalls())
			}
		})
	}
}

// TestEmptySweepTouchesNothing 确认空批不写状态、不投任何东西。
func TestEmptySweepTouchesNothing(t *testing.T) {
	store := newFakeStore()
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	handled, err := pub.RunOnce(context.Background())
	if err != nil || handled != 0 {
		t.Fatalf("RunOnce=(%d,%v)，期望 (0,nil)", handled, err)
	}
	if len(sender.sentCalls()) != 0 {
		t.Fatalf("空批不应投递：%+v", sender.sentCalls())
	}
	if got := store.callLog(); len(got) != 1 || got[0] != "list" {
		t.Fatalf("calls=%v，期望只有 list", got)
	}
}

func TestNewGuardsNilDependencies(t *testing.T) {
	if _, err := New(nil, &fakeSender{}, testOptions()); err == nil ||
		!strings.Contains(err.Error(), "store is required") {
		t.Errorf("nil store 应报错：%v", err)
	}
	if _, err := New(newFakeStore(), nil, testOptions()); err == nil ||
		!strings.Contains(err.Error(), "sender is required") {
		t.Errorf("nil sender 应报错：%v", err)
	}
	// 依赖齐全但参数非法也必须拒绝（而不是静默 normalize 成默认值继续跑）。
	bad := testOptions()
	bad.MaxAttempts = 0
	if _, err := New(newFakeStore(), &fakeSender{}, bad); err == nil {
		t.Error("MaxAttempts=0 应被拒绝")
	}
}

// TestOptionsValidateNamesConfigKey 钉住每个非法参数都在错误里点名对应的 yaml 键。
func TestOptionsValidateNamesConfigKey(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Options)
		want string
	}{
		{"轮询间隔", func(o *Options) { o.Interval = 0 }, "Kafka.PollIntervalSec"},
		{"批量上限", func(o *Options) { o.Batch = -1 }, "Kafka.BatchLimit"},
		{"尝试上限", func(o *Options) { o.MaxAttempts = 0 }, "Kafka.MaxRetries"},
		{"退避基数", func(o *Options) { o.BaseBackoff = 0 }, "Kafka.RetryBackoffSec"},
		{"退避上限小于基数", func(o *Options) { o.MaxBackoff = time.Second }, "Kafka.RetryMaxBackoffSec"},
		{"投递超时", func(o *Options) { o.SendTimeout = 0 }, "Kafka.SendTimeoutSec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			tc.mut(&opts)
			err := opts.validate()
			if err == nil {
				t.Fatal("非法参数应报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误=%v，期望点名 %s", err, tc.want)
			}
		})
	}
}

// TestOptionsFromMapsEveryField 用一组互不相同的非默认值钉住映射，
// 新增配置键时若忘了透传，本用例必红。
func TestOptionsFromMapsEveryField(t *testing.T) {
	k := config.KafkaConf{
		Enabled:            true,
		Brokers:            []string{"10.0.0.1:9092"},
		PublishTopics:      []string{RequiredTopic()},
		MaxRetries:         7,
		RetryBackoffSec:    3,
		RetryMaxBackoffSec: 900,
		PollIntervalSec:    11,
		BatchLimit:         45,
		SendTimeoutSec:     8,
	}
	got := OptionsFrom(k)
	want := Options{
		Interval:    11 * time.Second,
		Batch:       45,
		MaxAttempts: 7,
		BaseBackoff: 3 * time.Second,
		MaxBackoff:  900 * time.Second,
		SendTimeout: 8 * time.Second,
	}
	if got != want {
		t.Fatalf("OptionsFrom=%+v，期望 %+v", got, want)
	}
	if err := got.validate(); err != nil {
		t.Fatalf("映射结果自身应合法：%v", err)
	}
}

// validKafkaConf 是一份「只差 Enabled 就能开」的完整发布配置。
func validKafkaConf() config.KafkaConf {
	return config.KafkaConf{
		Enabled:            true,
		Brokers:            []string{"127.0.0.1:9092"},
		Group:              "live-ingest.v1",
		PublishTopics:      []string{RequiredTopic()},
		MaxRetries:         5,
		RetryBackoffSec:    2,
		RetryMaxBackoffSec: 1800,
		PollIntervalSec:    2,
		BatchLimit:         100,
		SendTimeoutSec:     5,
	}
}

// TestValidatePublishKafkaRejectsEachIncompleteKey 逐键点名，
// 保证「配置没写对」表现为启动失败，而不是安静地不发事件。
func TestValidatePublishKafkaRejectsEachIncompleteKey(t *testing.T) {
	if err := ValidatePublishKafka(validKafkaConf()); err != nil {
		t.Fatalf("完整配置不应报错：%v", err)
	}
	cases := []struct {
		name string
		mut  func(*config.KafkaConf)
		want string
	}{
		{"缺少 broker", func(k *config.KafkaConf) { k.Brokers = nil }, "Kafka.Brokers 为空"},
		{"broker 空串", func(k *config.KafkaConf) { k.Brokers = []string{"  "} }, "Kafka.Brokers[0]"},
		{"缺少 topic", func(k *config.KafkaConf) { k.PublishTopics = nil }, "Kafka.PublishTopics 为空"},
		{"topic 含空串", func(k *config.KafkaConf) { k.PublishTopics = []string{"  "} }, "含空串"},
		{"topic 版本号写错", func(k *config.KafkaConf) { k.PublishTopics = []string{"live.state.v2"} }, "本服务不产出该事件"},
		{"多写一个别人的 topic", func(k *config.KafkaConf) {
			k.PublishTopics = []string{RequiredTopic(), "content.published.v1"}
		}, "content.published.v1"},
		{"尝试上限 0", func(k *config.KafkaConf) { k.MaxRetries = 0 }, "Kafka.MaxRetries"},
		{"退避基数 0", func(k *config.KafkaConf) { k.RetryBackoffSec = 0 }, "Kafka.RetryBackoffSec"},
		{"退避上限 0", func(k *config.KafkaConf) { k.RetryMaxBackoffSec = 0 }, "Kafka.RetryMaxBackoffSec"},
		{"上限小于基数", func(k *config.KafkaConf) { k.RetryMaxBackoffSec = 1 }, "不得小于"},
		{"轮询 0", func(k *config.KafkaConf) { k.PollIntervalSec = 0 }, "Kafka.PollIntervalSec"},
		{"批量 0", func(k *config.KafkaConf) { k.BatchLimit = 0 }, "Kafka.BatchLimit"},
		{"投递超时 0", func(k *config.KafkaConf) { k.SendTimeoutSec = 0 }, "Kafka.SendTimeoutSec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := validKafkaConf()
			tc.mut(&k)
			err := ValidatePublishKafka(k)
			if err == nil {
				t.Fatal("应报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误=%v，期望点名 %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "live-ingest: Kafka 发布配置不完整") {
				t.Errorf("错误前缀应能定位服务：%v", err)
			}
		})
	}
	// 反向边界：Group 不参与必填判定（生产路径不读它），空 Group 必须放行，
	// 否则就是在校验一个没人读的键。
	k := validKafkaConf()
	k.Group = ""
	if err := ValidatePublishKafka(k); err != nil {
		t.Errorf("空 Kafka.Group 不应报错（本服务不消费）：%v", err)
	}
}

func TestSenderSettingsFromDedupesTrimsAndCopies(t *testing.T) {
	k := config.KafkaConf{
		Brokers:       []string{"127.0.0.1:9092", "127.0.0.1:9093"},
		PublishTopics: []string{RequiredTopic(), " " + RequiredTopic() + " ", "", RequiredTopic()},
	}
	got := SenderSettingsFrom(k)
	if strings.Join(got.Topics, "|") != RequiredTopic() {
		t.Errorf("topics=%v，期望去重去空白后只剩 %s", got.Topics, RequiredTopic())
	}
	if strings.Join(got.Brokers, "|") != "127.0.0.1:9092|127.0.0.1:9093" {
		t.Errorf("brokers=%v", got.Brokers)
	}
	// 返回的切片必须是副本：改它不能污染配置对象。
	got.Brokers[0] = "mutated"
	if k.Brokers[0] != "127.0.0.1:9092" {
		t.Errorf("SenderSettingsFrom 复用了配置的底层数组：%v", k.Brokers)
	}
}

// TestNewPublisherGuardsConfigAndDeps 钉住 svc 入口的失败顺序：
// 配置不合格时先报配置错（连 sender 都不看），再报依赖缺失。
func TestNewPublisherGuardsConfigAndDeps(t *testing.T) {
	c := config.Config{}
	c.Kafka = config.KafkaConf{PublishTopics: []string{RequiredTopic()}}
	if _, err := NewPublisher(c, &fakeOutboxModel{}, &fakeSender{}); err == nil ||
		!strings.Contains(err.Error(), "Kafka.Brokers 为空") {
		t.Fatalf("空 Brokers 应最先报错：%v", err)
	}

	valid := config.Config{}
	valid.Kafka = validKafkaConf()
	if _, err := NewPublisher(valid, &fakeOutboxModel{}, nil); err == nil ||
		!strings.Contains(err.Error(), "sender is required") {
		t.Errorf("nil sender 应报错：%v", err)
	}
	if _, err := NewPublisher(valid, nil, &fakeSender{}); err == nil ||
		!strings.Contains(err.Error(), "outbox model is required") {
		t.Errorf("nil outbox model 应报错：%v", err)
	}
	pub, err := NewPublisher(valid, &fakeOutboxModel{}, &fakeSender{})
	if err != nil {
		t.Fatalf("完整配置应能构造：%v", err)
	}
	if pub.Options().MaxAttempts != 5 || pub.Options().Batch != 100 || pub.Options().SendTimeout != 5*time.Second {
		t.Fatalf("参数没透传：%+v", pub.Options())
	}
}

// TestStartStopLifecycle 钉住启停：启动即扫一次、重复 Start 报错、
// Stop 只关一次连接且之后不再扫描、未启动时 Stop 是 no-op。
func TestStartStopLifecycle(t *testing.T) {
	store := newFakeStore()
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, func(o *Options) { o.Interval = 5 * time.Millisecond })

	if err := pub.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !pub.Running() {
		t.Fatal("Start 后 Running 应为 true")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && store.listCount() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if store.listCount() == 0 {
		t.Fatal("启动后应立即扫一次 outbox，而不是等第一个 ticker")
	}
	if err := pub.Start(); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Errorf("重复 Start 必须报错：%v", err)
	}

	pub.Stop()
	if pub.Running() {
		t.Error("Stop 后 Running 应为 false")
	}
	if sender.closeCount() != 1 {
		t.Errorf("Close 调用 %d 次，期望恰好 1 次", sender.closeCount())
	}
	listsAfterStop := store.listCount()
	time.Sleep(30 * time.Millisecond)
	if got := store.listCount(); got != listsAfterStop {
		t.Errorf("Stop 后仍在扫描：%d → %d", listsAfterStop, got)
	}
	pub.Stop() // 幂等
	if sender.closeCount() != 1 {
		t.Errorf("重复 Stop 不应再关连接：%d", sender.closeCount())
	}

	fresh, err := New(newFakeStore(), &fakeSender{}, testOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fresh.Stop() // 未启动就 Stop：no-op，不 panic
}

// TestLoopReportsSweepError 确认后台循环把扫描错误记进 Stats 的最近错误，
// 而不是只打日志：运维要能回答「发布循环到底有没有在工作」。
func TestLoopReportsSweepError(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("sql: connection is closed")
	pub := newTestPublisher(t, store, &fakeSender{}, func(o *Options) { o.Interval = 5 * time.Millisecond })

	if err := pub.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(pub.Stop)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, _, lastErr := pub.Stats(); strings.Contains(lastErr, "connection is closed") {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("扫描错误没有记入 Stats")
}
