package outbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNow 是固定时钟：退避时间与发布时间戳都必须变成可预测的字面量，
// 否则「next_retry_at 是否真的按指数推进」这类断言只能靠放宽窗口糊过去。
var fakeNow = time.Unix(1_700_000_000, 0).UTC()

// rowState 是替身记住的一行最终结论。
type rowState struct {
	state       string // published / failed；缺省表示仍停在待发布
	retryCount  int32
	nextRetryAt int64
	lastError   string
}

// fakeStore 是 Store 的替身：按批次返回预设行，并把每次调用留痕。
// 带锁是因为启停用例会在后台循环运行时读这些痕迹（裸读在 -race 下会被判竞争）。
type fakeStore struct {
	mu      sync.Mutex
	batches [][]*Row
	lists   int
	calls   []string
	states  map[int64]*rowState
	listNow int64

	listErr   error
	pubErr    error
	retryErr  error
	failedErr error
}

func newFakeStore(batches ...[]*Row) *fakeStore {
	return &fakeStore{batches: batches, states: map[int64]*rowState{}}
}

func (f *fakeStore) ListPending(_ context.Context, now int64, limit int32) ([]*Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	f.calls = append(f.calls, fmt.Sprintf("list:%d", limit))
	f.listNow = now
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

func (f *fakeStore) listNowArg() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listNow
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
	callCount int
}

func (f *fakeSender) Send(ctx context.Context, topic, key, payload string) error {
	timeout, hasDeadline := time.Duration(0), false
	if d, ok := ctx.Deadline(); ok {
		timeout, hasDeadline = time.Until(d), true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.sent = append(f.sent, sendCall{topic: topic, key: key, payload: payload, hasDeadline: hasDeadline, timeout: timeout})
	if f.errOn != nil {
		if err, ok := f.errOn[f.callCount]; ok {
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
		Name:        "testpub/publisher",
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

func okRow(id int64, eventID string) *Row {
	return &Row{
		ID:      id,
		EventID: eventID,
		Topic:   "domain.action.v1",
		Key:     fmt.Sprintf("agg-%d", id),
		Payload: fmt.Sprintf(`{"event_id":%q}`, eventID),
	}
}

func TestRunOncePublishesInStoreOrderAndMarksPublished(t *testing.T) {
	store := newFakeStore([]*Row{okRow(1, "E1"), nil, okRow(2, "E2")})
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	handled, err := pub.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if handled != 2 {
		t.Fatalf("nil 行不该计入处理数，实得 handled=%d", handled)
	}

	sent := sender.sentCalls()
	if len(sent) != 2 {
		t.Fatalf("应投递 2 条，实得 %d", len(sent))
	}
	// 原样投递：topic/key/payload 都必须是行上的列，发布器不做二次编码。
	if sent[0].topic != "domain.action.v1" || sent[0].key != "agg-1" || sent[0].payload != `{"event_id":"E1"}` {
		t.Fatalf("第 1 条投递参数失真: %+v", sent[0])
	}
	if sent[1].key != "agg-2" {
		t.Fatalf("第 2 条投递参数失真: %+v", sent[1])
	}
	want := []string{"list:100", "published:1@1700000000", "published:2@1700000000"}
	got := store.callLog()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("落库调用序列不符\nwant %v\ngot  %v", want, got)
	}
	if store.listNowArg() != fakeNow.Unix() {
		t.Fatalf("ListPending 的 now 必须来自注入时钟：want %d got %d", fakeNow.Unix(), store.listNowArg())
	}
	pubI, retryI, failedI, lastErr := pub.Stats()
	if pubI != 2 || retryI != 0 || failedI != 0 || lastErr != "" {
		t.Fatalf("Stats 计数失真 published=%d retried=%d failed=%d err=%q", pubI, retryI, failedI, lastErr)
	}
}

// TestRunOnceKeepsStoreOrder 证明发布器不自己排序：
// id 升序是各服务 model.ListPending 的 SQL 职责，这里重排会掩盖「忘写 ORDER BY」的真问题，
// 而忘写 ORDER BY 的后果是同聚合根事件乱序，下游的 seq 守卫会把它判成回退。
func TestRunOnceKeepsStoreOrder(t *testing.T) {
	store := newFakeStore([]*Row{okRow(9, "E9"), okRow(3, "E3"), okRow(7, "E7")})
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	if _, err := pub.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var keys []string
	for _, c := range sender.sentCalls() {
		keys = append(keys, c.key)
	}
	if want := []string{"agg-9", "agg-3", "agg-7"}; strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("发布器不得重排 store 给的顺序\nwant %v\ngot  %v", want, keys)
	}
}

// TestSendContextCarriesTimeout 钉住「每条投递都有超时」：
// 没有 deadline 时一次 broker hang 会占住整个发布循环，退避与判死都不会发生。
func TestSendContextCarriesTimeout(t *testing.T) {
	store := newFakeStore([]*Row{okRow(1, "E1")})
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, func(o *Options) { o.SendTimeout = 750 * time.Millisecond })

	if _, err := pub.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	calls := sender.sentCalls()
	if len(calls) != 1 || !calls[0].hasDeadline {
		t.Fatalf("Send 的 ctx 必须带 deadline，实得 %+v", calls)
	}
	if calls[0].timeout <= 500*time.Millisecond || calls[0].timeout > 750*time.Millisecond {
		t.Fatalf("deadline 应等于 SendTimeout，实得 %s", calls[0].timeout)
	}
}

// TestTransientSendFailureSchedulesBackoff 钉住第一次失败的落库形态：
// 行仍留在待发布，retry_count=1，next_retry_at = now + 基数，错误原文进 last_error。
func TestTransientSendFailureSchedulesBackoff(t *testing.T) {
	store := newFakeStore([]*Row{okRow(11, "E11")})
	sender := &fakeSender{err: errors.New("broker down")}
	pub := newTestPublisher(t, store, sender, nil)

	handled, err := pub.RunOnce(context.Background())
	if err != nil {
		// 投递失败不是循环失败：它已经落进该行的状态里。
		t.Fatalf("投递失败不该冒泡: %v", err)
	}
	if handled != 1 {
		t.Fatalf("handled=%d", handled)
	}
	st := store.state(11)
	if st == nil || st.state != "pending" {
		t.Fatalf("失败行必须仍停在待发布，实得 %+v", st)
	}
	if st.retryCount != 1 {
		t.Fatalf("retry_count=%d", st.retryCount)
	}
	if want := fakeNow.Add(2 * time.Second).Unix(); st.nextRetryAt != want {
		t.Fatalf("首次退避应为 now+BaseBackoff：want %d got %d", want, st.nextRetryAt)
	}
	if st.lastError != "broker down" {
		t.Fatalf("last_error 必须是投递错误原文，实得 %q", st.lastError)
	}
	for _, c := range store.callLog() {
		if strings.HasPrefix(c, "published:") {
			t.Fatalf("没送出去的行绝不能标已发布：%v", store.callLog())
		}
	}
}

// TestBackoffCurveIsExponentialAndCapped 用边界表钉住退避曲线。
// 后三行是移位防护：Go 的左移按 mod 位宽生效，shift 不夹住的话 2s<<64 会退回 2s
// （等于「退避越久间隔越短」），而 2s<<40 之类会溢出成负数；
// 负的 next_retry_at 会被 ListPending 当成立即到期，退避直接失效并把队列打满。
func TestBackoffCurveIsExponentialAndCapped(t *testing.T) {
	pub := newTestPublisher(t, newFakeStore(nil), &fakeSender{}, func(o *Options) {
		o.BaseBackoff = 2 * time.Second
		o.MaxBackoff = 1800 * time.Second
	})
	cases := []struct {
		name      string
		nextCount int64
		wantDelay time.Duration
	}{
		{"第 1 次失败=基数", 1, 2 * time.Second},
		{"第 2 次失败=2 倍", 2, 4 * time.Second},
		{"第 3 次失败=4 倍", 3, 8 * time.Second},
		{"第 10 次失败=512 倍", 10, 1024 * time.Second},
		{"第 11 次就超上限，夹住", 11, 1800 * time.Second},
		{"移位 64 不得按 mod 64 回绕", 65, 1800 * time.Second},
		{"int64 大数不得溢出为负", 1 << 40, 1800 * time.Second},
		{"列被写坏成 0 也按基数退避", 0, 2 * time.Second},
		{"列被写坏成负数同样", -7, 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pub.nextRetryAt(fakeNow, tc.nextCount)
			want := fakeNow.Add(tc.wantDelay).Unix()
			if got != want {
				t.Fatalf("nextCount=%d：want %d got %d（实得偏移 %ds）",
					tc.nextCount, want, got, got-fakeNow.Unix())
			}
			if got <= fakeNow.Unix() {
				t.Fatalf("退避结果必须严格晚于 now：%d vs %d", got, fakeNow.Unix())
			}
			if got > fakeNow.Add(pub.Options().MaxBackoff).Unix() {
				t.Fatalf("退避结果不得超过上限：%d", got)
			}
		})
	}
}

// TestRunOnceAppliesCappedBackoff 把曲线接回落库路径：
// retry_count 已经很大时，写回的 next_retry_at 必须是上限值而不是天量秒数。
func TestRunOnceAppliesCappedBackoff(t *testing.T) {
	store := newFakeStore([]*Row{{ID: 1, EventID: "E1", Topic: "t.v1", Key: "k",
		Payload: `{}`, RetryCount: 20}})
	sender := &fakeSender{err: errors.New("send failed")}
	pub := newTestPublisher(t, store, sender, func(o *Options) {
		o.MaxAttempts = 1 << 30 // 本用例只关心退避落库，不判死
	})

	if _, err := pub.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	st := store.state(1)
	if st == nil || st.state != "pending" {
		t.Fatalf("应安排重试，实得 %+v", st)
	}
	if want := fakeNow.Add(1800 * time.Second).Unix(); st.nextRetryAt != want {
		t.Fatalf("next_retry_at want %d got %d", want, st.nextRetryAt)
	}
	if st.retryCount != 21 {
		t.Fatalf("retry_count want 21 got %d", st.retryCount)
	}
}

// TestRetryCountNearInt32MaxFailsInsteadOfOverflowing 钉住计数溢出防护：
// retry_count 是库里的列，取到 int32 上限时加一会绕成负数，
// 判死边界随之失效，这一行会永远退避、永远不出去，且写回负的 retry_count。
func TestRetryCountNearInt32MaxFailsInsteadOfOverflowing(t *testing.T) {
	cases := []struct {
		name       string
		retryCount int32
		wantFailed bool
		wantCount  int32
	}{
		{"int32 上限：必须判死", 2147483647, true, 0},
		{"上限减一（MaxAttempts=5）：判死", 4, true, 0},
		{"列是负数：退避但计数从 1 起", -5, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore([]*Row{{ID: 8, EventID: "E8", Topic: "t.v1", Key: "k",
				Payload: `{}`, RetryCount: tc.retryCount}})
			sender := &fakeSender{err: errors.New("broker gone")}
			pub := newTestPublisher(t, store, sender, nil) // MaxAttempts=5

			if _, err := pub.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			st := store.state(8)
			if st == nil {
				t.Fatalf("这一行必须有结论")
			}
			if tc.wantFailed {
				if st.state != "failed" {
					t.Fatalf("应为 failed，实得 %+v", st)
				}
				if st.retryCount != 0 || st.nextRetryAt != 0 {
					t.Fatalf("判死不得再写退避字段，实得 %+v", st)
				}
				return
			}
			if st.state != "pending" || st.retryCount != tc.wantCount {
				t.Fatalf("应为 pending 且 retry_count=%d，实得 %+v", tc.wantCount, st)
			}
			if st.nextRetryAt <= fakeNow.Unix() {
				t.Fatalf("负的 retry_count 绝不能换来「立即可重试」：%d", st.nextRetryAt)
			}
		})
	}
}

// TestRetryBoundaryDecidesFailedVsRetry 钉住「达到上限即判死」的边界不 off-by-one：
// retry_count = MaxAttempts-1 时这次失败后必须判死，= MaxAttempts-2 时必须还给退避。
func TestRetryBoundaryDecidesFailedVsRetry(t *testing.T) {
	cases := []struct {
		name       string
		retryCount int32
		wantFailed bool
		wantReason string
	}{
		{"MaxAttempts-2 还差一次，继续退避", 3, false, ""},
		{"MaxAttempts-1 这次就是最后一次，判死", 4, true, "retries exhausted after 5 attempts: no route"},
		{"已经超上限（别的实例推过）也判死，次数按列真实值报", 7, true, "retries exhausted after 8 attempts: no route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore([]*Row{{ID: 5, EventID: "E5", Topic: "t.v1", Key: "k",
				Payload: `{}`, RetryCount: tc.retryCount}})
			sender := &fakeSender{err: errors.New("no route")}
			pub := newTestPublisher(t, store, sender, nil) // MaxAttempts=5

			if _, err := pub.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			st := store.state(5)
			if st == nil {
				t.Fatalf("这一行必须有结论")
			}
			if tc.wantFailed {
				if st.state != "failed" {
					t.Fatalf("应为 failed，实得 %+v", st)
				}
				if st.lastError != tc.wantReason {
					t.Fatalf("判死原因要含真实尝试次数与错误原文：want %q got %q", tc.wantReason, st.lastError)
				}
				for _, c := range store.callLog() {
					if strings.HasPrefix(c, "retry:") {
						t.Fatalf("判死的行不该再写退避：%v", store.callLog())
					}
				}
				return
			}
			if st.state != "pending" || st.retryCount != tc.retryCount+1 {
				t.Fatalf("应继续退避，实得 %+v", st)
			}
		})
	}
}

// TestDefectRowFailsWithoutSend 钉住不可发布行的处理：不投、不占重试次数，直接判死。
// 重试不会让一行列错的 payload 变对，占着退避队列只会掩盖问题。
func TestDefectRowFailsWithoutSend(t *testing.T) {
	row := okRow(6, "E6")
	row.Defect = `topic "other.thing.v1" 不属于本服务`
	store := newFakeStore([]*Row{row, okRow(7, "E7")})
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	handled, err := pub.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if handled != 2 {
		t.Fatalf("handled=%d", handled)
	}
	if len(sender.sentCalls()) != 1 {
		t.Fatalf("缺陷行绝不能投递，实得 %d 条", len(sender.sentCalls()))
	}
	st := store.state(6)
	if st == nil || st.state != "failed" {
		t.Fatalf("缺陷行应判死，实得 %+v", st)
	}
	if !strings.HasPrefix(st.lastError, "unpublishable: ") || !strings.Contains(st.lastError, "不属于本服务") {
		t.Fatalf("判死原因必须带 unpublishable 前缀与缺陷原文，实得 %q", st.lastError)
	}
	if store.state(7) == nil || store.state(7).state != "published" {
		t.Fatalf("缺陷行不能连累同批次的正常行")
	}
}

// TestStoreErrorsPropagate 钉住四类库操作失败都必须冒泡并中断本批：
// 吞掉它们会让「这一行有结论了」成为假象，而实际行还停在待发布。
func TestStoreErrorsPropagate(t *testing.T) {
	storeErr := errors.New("db gone")
	cases := []struct {
		name      string
		mutate    func(*fakeStore)
		sendErr   error
		rows      []*Row
		wantCalls []string
		wantMsg   string
	}{
		{"扫描失败", func(f *fakeStore) { f.listErr = storeErr }, nil, nil,
			[]string{"list:100"}, "读取待发布事件"},
		{"标记已发布失败", func(f *fakeStore) { f.pubErr = storeErr }, nil,
			[]*Row{okRow(1, "E1"), okRow(2, "E2")},
			[]string{"list:100", "published:1@1700000000"}, "标记已发布"},
		{"记录重试失败", func(f *fakeStore) { f.retryErr = storeErr }, errors.New("send failed"),
			[]*Row{okRow(1, "E1"), okRow(2, "E2")},
			[]string{"list:100", "retry:1:1@1700000002"}, "记录重试"},
		{"判死失败（缺陷行）", func(f *fakeStore) { f.failedErr = storeErr }, nil,
			[]*Row{{ID: 1, EventID: "E1", Topic: "t.v1", Key: "k", Payload: `{}`, Defect: "缺 aggregate_id"}, okRow(2, "E2")},
			[]string{"list:100", "failed:1"}, "判死写库"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(tc.rows)
			tc.mutate(store)
			sender := &fakeSender{err: tc.sendErr}
			pub := newTestPublisher(t, store, sender, nil)

			handled, err := pub.RunOnce(context.Background())
			if err == nil {
				t.Fatalf("%s 必须冒泡错误", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) || !errors.Is(err, storeErr) {
				t.Fatalf("错误必须点名环节并保留原始错误: %v", err)
			}
			if handled != 0 {
				t.Fatalf("写库失败的那一行不能计入 handled，实得 %d", handled)
			}
			got := store.callLog()
			if strings.Join(got, ",") != strings.Join(tc.wantCalls, ",") {
				t.Fatalf("本批必须在该行中断，后续行不再处理\nwant %v\ngot  %v", tc.wantCalls, got)
			}
			_, _, _, lastErr := pub.Stats()
			if lastErr != "" {
				t.Fatalf("RunOnce 只把错误交给调用方，lastBatchErr 是后台循环的口径：%q", lastErr)
			}
			if !strings.Contains(err.Error(), "testpub/publisher:") {
				t.Fatalf("错误必须带组件标签，实得 %v", err)
			}
		})
	}
}

// TestEmptySweepTouchesNothing 确认空批不写状态、不投任何东西。
func TestEmptySweepTouchesNothing(t *testing.T) {
	store := newFakeStore(nil)
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, nil)

	handled, err := pub.RunOnce(context.Background())
	if err != nil || handled != 0 {
		t.Fatalf("空批应返回 (0, nil)，实得 (%d, %v)", handled, err)
	}
	if len(store.callLog()) != 1 || len(sender.sentCalls()) != 0 {
		t.Fatalf("空批只该有一次扫描: store=%v sender=%v", store.callLog(), sender.sentCalls())
	}
}

// TestNewGuardsNilDependencies 钉住构造期就失败，而不是启动后「安静地什么都不做」。
func TestNewGuardsNilDependencies(t *testing.T) {
	store := newFakeStore(nil)
	sender := &fakeSender{}

	if _, err := New(nil, sender, testOptions()); err == nil || !strings.Contains(err.Error(), "store is required") {
		t.Fatalf("nil store 必须报错，实得 %v", err)
	}
	if _, err := New(store, nil, testOptions()); err == nil || !strings.Contains(err.Error(), "sender is required") {
		t.Fatalf("nil sender 必须报错，实得 %v", err)
	}
	bad := testOptions()
	bad.Interval = 0
	if _, err := New(store, sender, bad); err == nil || !strings.Contains(err.Error(), "Options.Interval") {
		t.Fatalf("非法参数必须报错，实得 %v", err)
	}
	// Name 留空只影响日志前缀，不影响可用性。
	anon := testOptions()
	anon.Name = ""
	pub, err := New(store, sender, anon)
	if err != nil {
		t.Fatalf("Name 为空应回落到默认前缀: %v", err)
	}
	if !strings.HasPrefix(pub.opts.label(), "common/outbox") {
		t.Fatalf("默认前缀失真: %s", pub.opts.label())
	}
	if pub.Options().Name != "" {
		t.Fatalf("Options() 必须原样返回生效参数")
	}
}

// TestOptionsValidateNamesKeyAndLabel 钉住每个非法参数都在错误里点名对应的 yaml 键，
// 且前缀用 Options.Name：多服务共用本包时，日志必须能指出是哪份循环。
func TestOptionsValidateNamesKeyAndLabel(t *testing.T) {
	cases := []struct {
		mutate func(*Options)
		want   string
	}{
		{func(o *Options) { o.Interval = 0 }, "Kafka.PollIntervalSec"},
		{func(o *Options) { o.Batch = 0 }, "Kafka.BatchLimit"},
		{func(o *Options) { o.MaxAttempts = 0 }, "Kafka.MaxRetries"},
		{func(o *Options) { o.BaseBackoff = 0 }, "Kafka.RetryBackoffSec"},
		{func(o *Options) { o.MaxBackoff = time.Second }, "Kafka.RetryMaxBackoffSec"},
		{func(o *Options) { o.SendTimeout = -1 }, "Kafka.SendTimeoutSec"},
	}
	for _, tc := range cases {
		opts := testOptions()
		tc.mutate(&opts)
		err := opts.validate()
		if err == nil {
			t.Fatalf("want error for %s", tc.want)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("错误要点名配置键 %s，实得 %v", tc.want, err)
		}
		if !strings.HasPrefix(err.Error(), "testpub/publisher:") {
			t.Fatalf("错误必须带组件标签，实得 %v", err)
		}
	}
	// 多个键同时不合格时全部列出：只报第一个会逼运维改一轮、重启一轮。
	opts := testOptions()
	opts.Batch, opts.MaxAttempts, opts.SendTimeout = 0, 0, 0
	err := opts.validate()
	if err == nil || strings.Count(err.Error(), "Kafka.") != 3 {
		t.Fatalf("三个不合格键都要点名，实得 %v", err)
	}
}

// TestStartStopLifecycle 钉住启停：启动即扫一次、重复 Start 报错、
// Stop 只关一次连接且之后不再扫描、未启动时 Stop 是 no-op。
func TestStartStopLifecycle(t *testing.T) {
	store := newFakeStore(nil)
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, func(o *Options) { o.Interval = 20 * time.Millisecond })
	if pub.Running() {
		t.Fatalf("构造后不该处于运行态")
	}
	pub.Stop() // 未启动：必须什么都不做，绝不能 close(nil) 或关连接
	if sender.closeCount() != 0 || store.listCount() != 0 {
		t.Fatalf("未启动时 Stop 不该有副作用: close=%d lists=%d", sender.closeCount(), store.listCount())
	}

	if err := pub.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !pub.Running() {
		t.Fatalf("Start 后应为运行态")
	}
	if err := pub.Start(); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("重复 Start 必须报错，实得 %v", err)
	}
	// 启动即扫：不等一个 Interval 就该看到第一次扫描，进程重启后的积压才能立刻处理。
	deadline := time.Now().Add(2 * time.Second)
	for store.listCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if store.listCount() == 0 {
		t.Fatalf("启动后必须立刻扫一轮")
	}
	pub.Stop()
	if pub.Running() {
		t.Fatalf("Stop 后应为停止态")
	}
	if sender.closeCount() != 1 {
		t.Fatalf("Stop 必须关一次发送端，实得 %d", sender.closeCount())
	}
	afterLists := store.listCount()
	time.Sleep(60 * time.Millisecond) // > Interval，确认循环真的退出而不是只是没数据
	if store.listCount() != afterLists {
		t.Fatalf("Stop 之后不得继续扫描：%d -> %d", afterLists, store.listCount())
	}
	pub.Stop() // 幂等
	if sender.closeCount() != 1 {
		t.Fatalf("重复 Stop 不该再关一次连接，实得 %d", sender.closeCount())
	}
}

// TestLoopReportsSweepError 确认后台循环把扫描错误记进 Stats 的最近错误，
// 而不是只打日志：运维要能回答「发布循环到底有没有在工作」。
func TestLoopReportsSweepError(t *testing.T) {
	store := newFakeStore(nil)
	store.listErr = errors.New("connection refused")
	sender := &fakeSender{}
	pub := newTestPublisher(t, store, sender, func(o *Options) { o.Interval = 20 * time.Millisecond })

	if err := pub.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, _, lastErr := pub.Stats()
		if strings.Contains(lastErr, "connection refused") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("循环应把扫描错误写进 Stats，实得 %q", lastErr)
		}
		time.Sleep(2 * time.Millisecond)
	}
	pub.Stop()
	if pub.published+pub.retried+pub.failed != 0 {
		t.Fatalf("扫描失败时不该有计数")
	}
}

// TestStopWaitsForInFlightBatch 钉住 Stop 会等当前批次收尾：
// 半截批次就关连接的话，我们连「哪几行没落库」都说不清。
func TestStopWaitsForInFlightBatch(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	sender := &fakeSender{}
	store := &blockingStore{
		fakeStore: *newFakeStore([]*Row{okRow(1, "E1")}),
		gate:      release,
		entered:   entered,
	}
	pub := newTestPublisher(t, store, sender, func(o *Options) { o.Interval = time.Hour })
	if err := pub.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-store.entered // 投递已经卡住
	stopped := make(chan struct{})
	go func() { pub.Stop(); close(stopped) }()
	select {
	case <-stopped:
		close(release)
		t.Fatalf("批次未收尾时 Stop 不得返回（否则会半截关连接）")
	case <-time.After(80 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatalf("批次收尾后 Stop 必须返回")
	}
	if st := store.state(1); st == nil || st.state != "published" {
		t.Fatalf("收尾批次里的行必须有结论，实得 %+v", st)
	}
	if sender.closeCount() != 1 {
		t.Fatalf("Stop 应关一次连接，实得 %d", sender.closeCount())
	}
}

// blockingStore 在第一次投递前阻塞，用来模拟「broker 慢，进程要退出」。
type blockingStore struct {
	fakeStore
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blockingStore) ListPending(ctx context.Context, now int64, limit int32) ([]*Row, error) {
	rows, err := b.fakeStore.ListPending(ctx, now, limit)
	if rows != nil && len(rows) > 0 {
		b.once.Do(func() { close(b.entered) })
		<-b.gate
	}
	return rows, err
}
