package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go-video/common/eventenvelope"
	"go-video/common/outbox"
	"go-video/services/recommend-recall/model"
)

// --- 替身 ---

// fakeOutboxModel 是 model.RecallOutboxModel 的替身：只实现发布器用到的四个方法，
// 其余方法靠内嵌接口留空（被调用即 panic，能立刻暴露「发布器越权读了别的列或写了事务」）。
//
// 与 internal/logic 里的 fakeOutbox 不同，这里的 Mark* 必须返回 (受影响行数, error)，
// 因为本表的状态方法都是条件 UPDATE：适配层怎么折叠「0 行」正是本包的核心判据，
// logic 那份只实现 Insert 的替身覆盖不到。
type fakeOutboxModel struct {
	model.RecallOutboxModel
	rows      []*model.RecallOutbox
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

func newFakeOutbox(rows ...*model.RecallOutbox) *fakeOutboxModel {
	return &fakeOutboxModel{
		rows:      rows,
		affected:  1,
		failIDs:   map[int64]struct{}{},
		publishAt: map[int64]int64{},
		retryAt:   map[int64]int64{},
		lastError: map[int64]string{},
	}
}

func (f *fakeOutboxModel) ListPending(_ context.Context, now int64, limit int32) ([]*model.RecallOutbox, error) {
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

// poolPublishedRow 造一行「切换事务已提交」的 outbox：
// payload 是完整信封 JSON，各列全部取自同一个 env，与 writePublishedEvent 的生产路径同源。
func poolPublishedRow(t *testing.T, id int64, eventID string) *model.RecallOutbox {
	t.Helper()
	aggregateID := fmt.Sprintf("1:hot:%d", id)
	return &model.RecallOutbox{
		ID: id, EventID: eventID, EventType: model.EventPoolPublished,
		SchemaVersion: model.PoolVersionSchemaVersion, AggregateType: model.AggregateTypePool,
		AggregateID: aggregateID,
		Payload:     envelopeJSON(t, model.EventPoolPublished, eventID, model.AggregateTypePool, aggregateID),
		State:       model.OutboxStatePending, OccurredAt: 1_700_000_000,
	}
}

// envelopeJSON 组装与生产路径一致的信封 JSON（显式 event_id，不走 New 的自动生成）。
func envelopeJSON(t *testing.T, eventType, eventID, aggregateType, aggregateID string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"source": 1, "pool_key": "hot", "version": 7, "item_count": 128})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := &eventenvelope.Envelope{
		EventID: eventID, EventType: eventType, SchemaVersion: model.PoolVersionSchemaVersion,
		OccurredAt: "2026-10-04T00:00:00Z", Producer: model.ProducerName, TraceID: "trace-1",
		AggregateType: aggregateType, AggregateID: aggregateID, Payload: body,
	}
	raw, err := env.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(raw)
}

// --- RequiredTopic：跨服务契约的锚点 ---

// TestRequiredTopicMatchesModelConstant 钉住本服务唯一 topic 的字面量。
// 它是 docs/api-and-events.md §5 登记的跨服务契约，改名等于换掉所有消费方的订阅；
// 同时它必须等于 model.TopicPoolPublished：两处各写一遍而派生式跑偏时，
// 配置校验（拿 RequiredTopic 判）与生产侧（拿 model 常量拼）会各自自洽地指向不同 topic。
func TestRequiredTopicMatchesModelConstant(t *testing.T) {
	const want = "recall.pool.published.v1"
	if got := RequiredTopic(); got != want {
		t.Errorf("RequiredTopic()=%q，期望 %q（改版本或改名必须同步 docs §5、yaml 与建 topic 清单）", got, want)
	}
	if RequiredTopic() != model.TopicPoolPublished {
		t.Errorf("RequiredTopic()=%q 与 model.TopicPoolPublished=%q 分叉", RequiredTopic(), model.TopicPoolPublished)
	}
	topics := RequiredTopics()
	if len(topics) != 1 || topics[0] != RequiredTopic() {
		t.Errorf("RequiredTopics=%v，期望单元素 [%q]", topics, RequiredTopic())
	}
	// topic 必须由 event_type + .v + schema_version 派生，且事件名过得了信封契约
	//（生产侧 eventenvelope.New 会拒绝违规名，等于切换事务整笔回滚）。
	if !strings.HasSuffix(RequiredTopic(), ".v1") {
		t.Errorf("topic %q 不以 .v1 结尾：schema 版本递增必须走配置校验而不是静默改后缀", RequiredTopic())
	}
	if _, err := eventenvelope.New(model.ProducerName, model.EventPoolPublished,
		model.AggregateTypePool, "1:hot:7", model.PoolVersionSchemaVersion, json.RawMessage(`{}`), ""); err != nil {
		t.Errorf("event_type %q 过不了信封契约：%v", model.EventPoolPublished, err)
	}
}

// TestRequiredTopicsCoverDeclaredEventTypes 是全仓 event_type 门禁在本服务内部的补集：
// 前者管命名合法性，本用例管「model 声明了新事件常量但没登记进 topic 集合」。
// 漏登记的后果不是编译错误：那一类事件的每一行都会被 CheckRow 判成
// 「topic 不属于本服务」而直接判死，而 writePublishedEvent 完全成功、事务照常提交。
func TestRequiredTopicsCoverDeclaredEventTypes(t *testing.T) {
	declared := declaredEventTypes(t)
	known := map[string]bool{strings.TrimSuffix(RequiredTopic(), ".v1"): true}
	for _, eventType := range declared {
		if !known[eventType] {
			t.Errorf("model 声明了事件类型 %q，但 publisher.RequiredTopics 没有它：%s 的行会被直接判死",
				eventType, eventenvelope.Topic(eventType, model.PoolVersionSchemaVersion))
		}
	}
	for _, topic := range RequiredTopics() {
		eventType := strings.TrimSuffix(topic, ".v1")
		found := false
		for _, c := range declared {
			if c == eventType {
				found = true
			}
		}
		if !found {
			t.Errorf("topic %q 的来源常量 %q 不在 model 声明里", topic, eventType)
		}
	}
}

// declaredEventTypes 从 model/*.go（非测试文件）源码里抓 `Event* = "..."` 常量取值。
// 用源码扫描而不是反射：常量没有注册表可枚举，而 errors.go 是本服务事件类型的事实来源
// （手法与 common/eventenvelope/event_type_gate_test.go 一致）。
// 数量下限必须显式断言：一旦声明形态变了（比如挪进带类型名的分组），扫描会返回空集，
// 上面的双向检查就退化成永真，所以这里直接把「只声明了一个」写死为前置条件。
func declaredEventTypes(t *testing.T) []string {
	t.Helper()
	dir := filepath.FromSlash("../../model")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 %s 失败（本服务的 topic 集合以它为准）: %v", dir, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读取 model/%s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			assign := strings.Index(trimmed, `= "`)
			if !strings.HasPrefix(trimmed, "Event") || assign < 0 {
				continue
			}
			rest := trimmed[assign+len(`= "`):]
			end := strings.Index(rest, `"`)
			if end < 0 {
				t.Fatalf("常量 %q 的取值没有右引号，扫描规则失效", trimmed)
			}
			value := rest[:end]
			if !strings.Contains(value, ".") {
				// 事件类型必须是点分名（信封契约要求），不含点的是别的 Event* 常量。
				continue
			}
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
	}
	if len(out) != 1 {
		t.Fatalf("只抓到 %d 个事件类型常量：%v（本服务当前只应产出 recall.pool.published，"+
			"数量变了必须同步 RequiredTopics、docs §5 与 yaml）", len(out), out)
	}
	if out[0] != model.EventPoolPublished {
		t.Fatalf("抓到的事件类型 %q 与 model.EventPoolPublished=%q 不一致，扫描规则已失效",
			out[0], model.EventPoolPublished)
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
// 分区键取聚合根 aggregate_id（同池同版本的事件必须同分区），payload 原样透传不重新编码。
func TestToRowMapsColumns(t *testing.T) {
	row := poolPublishedRow(t, 31, "01EVENTIDAAA0000000000000A")
	rec := toRow(row)
	if rec.Defect != "" {
		t.Fatalf("正常行不应有缺陷：%q", rec.Defect)
	}
	if rec.ID != 31 || rec.EventID != "01EVENTIDAAA0000000000000A" || rec.RetryCount != 0 {
		t.Errorf("基础列映射错：%+v", rec)
	}
	if rec.Topic != RequiredTopic() {
		t.Errorf("topic=%q，期望由 event_type+schema_version 列派生 %q", rec.Topic, RequiredTopic())
	}
	if rec.Key != row.AggregateID {
		t.Errorf("分区键=%q，期望用聚合根 aggregate_id（%q）", rec.Key, row.AggregateID)
	}
	if rec.Key == rec.EventID {
		t.Error("分区键取了 event_id：同一池的事件会被打散到不同分区，顺序保证直接失效")
	}
	if rec.Payload != row.Payload {
		t.Error("payload 被改写了：期望原样透传")
	}
}

// TestToRowFlagsDefects 逐条钉住「不可发布」的判定与原因点名。
// 这些都必须是判死而不是重试：重试不会让列写歪的行变对。
func TestToRowFlagsDefects(t *testing.T) {
	base := func() *model.RecallOutbox {
		return poolPublishedRow(t, 41, "01EVENTIDAAA0000000000000B")
	}
	cases := []struct {
		name   string
		mutate func(*model.RecallOutbox)
		want   string
	}{
		{"别的服务的事件", func(r *model.RecallOutbox) { r.EventType = "content.published" }, "不属于本服务"},
		{"schema 版本升到 v2", func(r *model.RecallOutbox) { r.SchemaVersion = 2 }, "recall.pool.published.v2"},
		{"event_type 为空", func(r *model.RecallOutbox) { r.EventType = "" }, `topic ""`},
		{"event_id 列为空", func(r *model.RecallOutbox) { r.EventID = "" }, "event_id 列为空"},
		{"聚合根为空", func(r *model.RecallOutbox) { r.AggregateID = "" }, "分区键为空"},
		{"payload 不是 JSON", func(r *model.RecallOutbox) { r.Payload = "not-json" }, "不是合法事件信封"},
		{"payload 缺 producer", func(r *model.RecallOutbox) {
			r.Payload = strings.Replace(r.Payload, `"`+model.ProducerName+`"`, `""`, 1)
		}, "producer is required"},
		{"payload 的 event_id 与列不一致", func(r *model.RecallOutbox) {
			r.Payload = strings.Replace(r.Payload, "01EVENTIDAAA0000000000000B", "01OTHERIDAAA0000000000000C", 1)
		}, "payload event_id"},
		{"payload 的 aggregate_id 与列不一致", func(r *model.RecallOutbox) { r.AggregateID = "1:hot:9999" }, "payload aggregate_id"},
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
		poolPublishedRow(t, 11, "01EVENTIDAAA0000000000000D"),
		nil,
		poolPublishedRow(t, 12, "01EVENTIDAAA0000000000000E"),
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
	if records[0].EventID != "01EVENTIDAAA0000000000000D" || records[1].Key != "1:hot:12" {
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
		!strings.Contains(err.Error(), "recall_outbox") ||
		!strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("读库错误应点名表名并保留底层错误：%v", err)
	}
}

// TestOutboxStoreDelegatesStateWrites 钉住本适配层与 model 的参数口径：
// mtime（发布时间）/ next_retry_at / last_error 原样传给 model，
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
	if err := store.MarkRetry(ctx, 8, 3, 1_700_005_678, "send error"); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
	if err := store.MarkFailed(ctx, 9, "exhausted"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if m.publishAt[7] != 1234 {
		t.Errorf("publishedAt=%d，期望 1234（本表把它写进 mtime，这是唯一的发布位点）", m.publishAt[7])
	}
	if m.retryAt[8] != 1_700_005_678 {
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

// TestConditionalMissIsLoggedNotFatal 钉住本适配层与引擎的口径：
// 条件 UPDATE 命中 0 行返回 nil（只写日志），不冒泡成错误。
// 依据是这三条 UPDATE 的 WHERE 只可能在「行已有结论」或「行已被 DeleteSentBefore 清掉」时不命中，
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
			for _, want := range []string{"recall_outbox", tc.want, "1406"} {
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
	m := newFakeOutbox(poolPublishedRow(t, 21, "01EVENTIDAAA0000000000000F"))
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
	if len(sent) != 1 || sent[0].topic != RequiredTopic() || sent[0].key != "1:hot:21" {
		t.Fatalf("投递不符：%+v", sent)
	}
	if sent[0].payload != m.rows[0].Payload {
		t.Error("投递的 payload 与 outbox 列不一致")
	}
	if sent[0].timeout <= 0 || sent[0].timeout > testOptions().SendTimeout {
		t.Errorf("投递超时=%s，期望 (0,%s]：每次投递必须有截止时间", sent[0].timeout, testOptions().SendTimeout)
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
	m := newFakeOutbox(poolPublishedRow(t, 31, "01EVENTIDAAA0000000000000G"))
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
	rows := []*model.RecallOutbox{
		poolPublishedRow(t, 41, "01EVENTIDAAA0000000000000H"),
		poolPublishedRow(t, 42, "01EVENTIDAAA0000000000000I"),
		poolPublishedRow(t, 43, "01EVENTIDAAA0000000000000J"),
	}

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
		for _, want := range []string{"recall_outbox", "42", "deadlock found"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("错误=%v，期望点名 %q", err, want)
			}
		}
		if got := strings.Join(m.calls, ","); got != "ListPending,MarkPublished:41,MarkPublished:42" {
			t.Errorf("调用序列=%s，期望在第 2 行失败后即停", got)
		}
	})
}

// TestRunOnceJudgesForeignEventDead 钉住「本表只产出这一类事件」这一侧的守卫：
// 一行 event_type 不属于本服务的事件必须直接判死，而不是投出去或无限重试。
func TestRunOnceJudgesForeignEventDead(t *testing.T) {
	row := poolPublishedRow(t, 51, "01FOREIGNEVENT00000000000A")
	row.EventType = "content.published"
	row.Payload = envelopeJSON(t, "content.published", row.EventID, "submission", "1:hot:51")
	m := newFakeOutbox(row)
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

// TestRunOnceEmptyBatchTouchesNothing 确认空批次不会留下任何写库调用：
// 一轮空转就把 mtime 刷新的话，CountStuck 的「事件滞留」口径会永远读不到真实积压。
func TestRunOnceEmptyBatchTouchesNothing(t *testing.T) {
	m := newFakeOutbox()
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
	if err != nil || handled != 0 {
		t.Fatalf("RunOnce=(%d,%v)，期望 (0,nil)", handled, err)
	}
	if got := strings.Join(m.calls, ","); got != "ListPending" {
		t.Errorf("调用序列=%s，期望只有 ListPending", got)
	}
	if len(sender.calls()) != 0 {
		t.Errorf("空批次仍有投递：%+v", sender.calls())
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
