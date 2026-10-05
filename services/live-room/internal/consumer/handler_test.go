package consumer

// handler_test.go 钉住「位点能不能前进」的判定。
//
// 假象替身（fakeApplicator）记录每一次入参，因此「契约非法的事件绝不进 logic」
// 是可以用 calls==0 直接证明的，而不是靠读注释相信。
// 依赖故障路径断言的是返回的错误就是注入的那个错误（errors.Is），
// 保证消费者没有把故障吞成「成功」。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

// scriptedReply 一次调用的应答。
type scriptedReply struct {
	reply *rpc.ReportStreamStateReply
	err   error
}

// fakeApplicator 记录入参并按 script 应答；script 用尽后复用最后一条。
type fakeApplicator struct {
	mu     sync.Mutex
	calls  int
	reqs   []*rpc.ReportStreamStateReq
	script []scriptedReply
}

func (f *fakeApplicator) ReportStreamState(_ context.Context, in *rpc.ReportStreamStateReq) (*rpc.ReportStreamStateReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.reqs = append(f.reqs, in)
	idx := f.calls - 1
	if idx >= len(f.script) {
		idx = len(f.script) - 1
	}
	if idx < 0 {
		return &rpc.ReportStreamStateReply{Result: model.StreamResultApplied}, nil
	}
	return f.script[idx].reply, f.script[idx].err
}

func (f *fakeApplicator) snapshot() (int, []*rpc.ReportStreamStateReq) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]*rpc.ReportStreamStateReq(nil), f.reqs...)
}

func replyWith(result int32, msg string) *rpc.ReportStreamStateReply {
	return &rpc.ReportStreamStateReply{Result: result, Message: msg}
}

func newHandler(app Applicator, attempts int) *Handler {
	return NewHandler(app, Options{MaxAttempts: attempts})
}

func TestConsumeAppliesAndForwardsRequest(t *testing.T) {
	app := &fakeApplicator{script: []scriptedReply{{reply: replyWith(model.StreamResultApplied, "已推进")}}}
	h := newHandler(app, 5)

	if err := h.Consume(context.Background(), "S-EVT-32", producerMessage); err != nil {
		t.Fatalf("已应用的事件必须提交位点: %v", err)
	}
	calls, reqs := app.snapshot()
	if calls != 1 {
		t.Fatalf("logic 应被调用 1 次，实得 %d", calls)
	}
	got := reqs[0]
	if got.GetEventId() != "01J8Z4M7Q9T2W6K5R3N8XY6GBA" || got.GetRoomId() != 32 ||
		got.GetSessionId() != 77 || got.GetStreamSeq() != 7 || got.GetStreamState() != model.StreamStateInterrupted {
		t.Fatalf("入参翻译不符: %+v", got)
	}
	if st := h.Stats(); st.Applied != 1 || st.Skipped != 0 || st.Retried != 0 {
		t.Fatalf("计数不符: %+v", st)
	}
	if h.trackedAttempts("01J8Z4M7Q9T2W6K5R3N8XY6GBA") != 0 {
		t.Fatal("成功后必须释放重试台账")
	}
	if o := h.Options(); o.MaxAttempts != 5 || o.MaxTrackedEvents != defaultMaxTrackedEvents {
		t.Fatalf("Options 归一化不符: %+v", o)
	}
}

// TestTerminalResultsCommit 逐个 result 断言位点可以前进：
// 这些都是 logic 已给出的终局结论，重投不会得到不同答案。
func TestTerminalResultsCommit(t *testing.T) {
	cases := []struct {
		result int32
		want   func(Stats) int64
		name   string
	}{
		{model.StreamResultApplied, func(s Stats) int64 { return s.Applied }, "Applied"},
		{model.StreamResultDuplicate, func(s Stats) int64 { return s.Duplicate }, "Duplicate"},
		{model.StreamResultStale, func(s Stats) int64 { return s.Stale }, "Stale"},
		{model.StreamResultIllegalTransition, func(s Stats) int64 { return s.Illegal }, "Illegal"},
		{model.StreamResultMismatch, func(s Stats) int64 { return s.Mismatch }, "Mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &fakeApplicator{script: []scriptedReply{{reply: replyWith(tc.result, "终态")}}}
			h := newHandler(app, 5)
			if err := h.Consume(context.Background(), "", producerMessage); err != nil {
				t.Fatalf("result=%d 必须允许提交位点: %v", tc.result, err)
			}
			if got := tc.want(h.Stats()); got != 1 {
				t.Fatalf("result=%d 的计数没有落到 %s: %+v", tc.result, tc.name, h.Stats())
			}
		})
	}
}

// TestContractViolationsNeverReachLogic 是「不白烧 event_id 去重键」这条设计唯一的行为证据。
func TestContractViolationsNeverReachLogic(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"空投递", "   "},
		{"残缺 JSON", `{`},
		{"缺 producer 的信封", `{"event_id":"e","event_type":"live.state","schema_version":1,` +
			`"occurred_at":"2026-10-04T10:00:00Z","aggregate_type":"live_stream","aggregate_id":"S","payload":{}}`},
		{"别人的事件类型", string(envelopeBytes(t, "evt-other", "video.published", 1, "S-1", validPayload))},
		{"版本不认识", string(envelopeBytes(t, "evt-v2", EventTypeStreamState, 2, "S-1", validPayload))},
		{"缺 room_id", string(envelopeBytes(t, "evt-noroom", EventTypeStreamState, 1, "S-1",
			`{"stream_state":2,"stream_seq":1}`))},
		{"未知流状态", string(envelopeBytes(t, "evt-state", EventTypeStreamState, 1, "S-1",
			`{"room_id":1,"stream_state":9,"stream_seq":1}`))},
		{"seq 缺失", string(envelopeBytes(t, "evt-seq", EventTypeStreamState, 1, "S-1",
			`{"room_id":1,"stream_state":2}`))},
		{"payload 不是对象", string(envelopeBytes(t, "evt-arr", EventTypeStreamState, 1, "S-1", `[2]`))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &fakeApplicator{}
			h := newHandler(app, 5)
			if err := h.Consume(context.Background(), "k", tc.value); err != nil {
				t.Fatalf("永久非法的投递必须确认掉，否则毒消息阻塞分区: %v", err)
			}
			if calls, _ := app.snapshot(); calls != 0 {
				t.Fatalf("契约非法不能送进 logic（会白烧 event_id 去重键），实得调用 %d 次", calls)
			}
			if st := h.Stats(); st.Skipped != 1 {
				t.Fatalf("应记为 skipped: %+v", st)
			}
		})
	}
}

// TestDependencyFailureRetriesThenGivesUp 封顶尝试次数：
// 没有上限就会让注定失败的事件在分区里无限热循环，把后面的事件全堵住。
func TestDependencyFailureRetriesThenGivesUp(t *testing.T) {
	boom := errors.New("dial tcp 127.0.0.1:3306: connect refused")
	app := &fakeApplicator{script: []scriptedReply{{err: boom}}}
	h := newHandler(app, 3)
	const eventID = "01J8Z4M7Q9T2W6K5R3N8XY6GBA"

	for i := 1; i <= 2; i++ {
		err := h.Consume(context.Background(), "", producerMessage)
		if !errors.Is(err, boom) {
			t.Fatalf("第 %d 次应把依赖错误原样交回队列（不提交位点）: %v", i, err)
		}
		if got := h.trackedAttempts(eventID); got != i {
			t.Fatalf("第 %d 次后台账应为 %d，实得 %d", i, i, got)
		}
	}
	if err := h.Consume(context.Background(), "", producerMessage); err != nil {
		t.Fatalf("第 3 次到达上限必须放弃并确认位点，否则无限热循环: %v", err)
	}
	if calls, _ := app.snapshot(); calls != 3 {
		t.Fatalf("logic 应被调用 3 次: %d", calls)
	}
	st := h.Stats()
	if st.Retried != 2 || st.GivenUp != 1 || st.Applied != 0 {
		t.Fatalf("计数不符: %+v", st)
	}
	if !strings.Contains(st.LastError, "connect refused") {
		t.Fatalf("LastError 应留下故障原因: %q", st.LastError)
	}
	if h.trackedAttempts(eventID) != 0 {
		t.Fatal("放弃后必须释放台账，否则 map 会堆满死事件")
	}
}

// TestRecoveryAfterFailureResetsLedger 一次成功要把尝试计数清零：
// 否则同一事件后续的真故障会被前面的历史失败直接判死。
func TestRecoveryAfterFailureResetsLedger(t *testing.T) {
	boom := errors.New("临时故障")
	const eventID = "01J8Z4M7Q9T2W6K5R3N8XY6GBA"
	app := &fakeApplicator{script: []scriptedReply{
		{err: boom},
		{reply: replyWith(model.StreamResultApplied, "已推进")},
		{err: boom},
		{reply: replyWith(model.StreamResultDuplicate, "重复")},
	}}
	h := newHandler(app, 2)

	if err := h.Consume(context.Background(), "", producerMessage); !errors.Is(err, boom) {
		t.Fatalf("首次故障应重投: %v", err)
	}
	if err := h.Consume(context.Background(), "", producerMessage); err != nil {
		t.Fatalf("恢复后应提交位点: %v", err)
	}
	if got := h.trackedAttempts(eventID); got != 0 {
		t.Fatalf("成功后台账没清: %d", got)
	}
	// 第三/四次：新的故障周期，仍然从第 1 次开始，不会因为历史失败被判死。
	if err := h.Consume(context.Background(), "", producerMessage); !errors.Is(err, boom) {
		t.Fatalf("新一轮故障仍应允许重投: %v", err)
	}
	if err := h.Consume(context.Background(), "", producerMessage); err != nil {
		t.Fatalf("最终收敛应提交位点: %v", err)
	}
	if st := h.Stats(); st.Retried != 2 || st.GivenUp != 0 || st.Applied != 1 || st.Duplicate != 1 {
		t.Fatalf("计数不符: %+v", st)
	}
}

// TestUnknownResultIsNotCommittedSilently logic 返回本包不认识的 result（上游改了枚举而没同步这里）
// 时不能当成成功：先重投，到上限再带着「未知 result」的原因留下 LastError。
func TestUnknownResultIsNotCommittedSilently(t *testing.T) {
	app := &fakeApplicator{script: []scriptedReply{{reply: replyWith(99, "上游新增的结果码")}}}
	h := newHandler(app, 2)

	err := h.Consume(context.Background(), "", producerMessage)
	if err == nil || !strings.Contains(err.Error(), "result=99") {
		t.Fatalf("未知 result 必须作为故障交回队列并点名 result: %v", err)
	}
	if st := h.Stats(); st.Retried != 1 || st.Unexpected != 0 {
		t.Fatalf("未知 result 在重投阶段记 retry: %+v", st)
	}
	if err := h.Consume(context.Background(), "", producerMessage); err != nil {
		t.Fatalf("到达上限后应放弃: %v", err)
	}
	if st := h.Stats(); st.GivenUp != 1 || !strings.Contains(st.LastError, "result=99") {
		t.Fatalf("放弃时要留下可排查的原因: %+v", st)
	}
}

func TestNilReplyWithoutErrorIsRetryable(t *testing.T) {
	app := &fakeApplicator{script: []scriptedReply{{reply: nil, err: nil}}}
	h := newHandler(app, 5)
	if err := h.Consume(context.Background(), "", producerMessage); !errors.Is(err, ErrNilReply) {
		t.Fatalf("(nil, nil) 必须按故障处理: %v", err)
	}
}

// TestLedgerIsBounded 容量只是内存防线：打满时逐出一条，被逐出的事件重新获得额度。
// 正确性仍由 event_id 去重键保证，所以这里的代价是多做几次无用功而不是重复推进状态。
func TestLedgerIsBounded(t *testing.T) {
	boom := errors.New("依赖故障")
	app := &fakeApplicator{script: []scriptedReply{{err: boom}}}
	h := NewHandler(app, Options{MaxAttempts: 3, MaxTrackedEvents: 1})

	for _, eventID := range []string{"evt-A", "evt-B", "evt-C"} {
		value := strings.Replace(producerMessage, "01J8Z4M7Q9T2W6K5R3N8XY6GBA", eventID, 1)
		if err := h.Consume(context.Background(), "", value); !errors.Is(err, boom) {
			t.Fatalf("%s 的首次故障应重投: %v", eventID, err)
		}
	}
	if got := h.trackedAttempts("evt-A"); got != 0 {
		t.Fatalf("台账容量只有 1，evt-A 应已被逐出（逐出=重新获得尝试额度），实得 %d", got)
	}
	if got := h.trackedAttempts("evt-B"); got != 0 {
		t.Fatalf("evt-B 同样被 evt-C 逐出: %d", got)
	}
	if got := h.trackedAttempts("evt-C"); got != 1 {
		t.Fatalf("台账里必须只剩最后写入的 evt-C: %d", got)
	}
	// 逐出只影响「封顶」而不影响正确性：evt-A 再来一次仍会送去 logic 走 event_id 去重，
	// 多做一次无用功，但不会重复推进状态（由 logic 侧的 dedup_key 唯一键兜底）。
	if err := h.Consume(context.Background(), "",
		strings.Replace(producerMessage, "01J8Z4M7Q9T2W6K5R3N8XY6GBA", "evt-A", 1)); !errors.Is(err, boom) {
		t.Fatalf("被逐出的事件重投应重新获得额度并继续重投: %v", err)
	}
	if calls, _ := app.snapshot(); calls != 4 {
		t.Fatalf("四次投递都应到达 logic（去重由 logic 负责，不在本包）: %d", calls)
	}
}

// TestConcurrentConsumeKeepsCounters 并发投递只能靠互斥保证计数与台账一致。
// 本机没装 cgo，-race 跑不了，因此这里用确定性的总量断言兜住（少一次就是丢计数）。
func TestConcurrentConsumeKeepsCounters(t *testing.T) {
	app := &fakeApplicator{script: []scriptedReply{{reply: replyWith(model.StreamResultApplied, "ok")}}}
	h := newHandler(app, 5)

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			value := strings.Replace(producerMessage, "01J8Z4M7Q9T2W6K5R3N8XY6GBA",
				"evt-concurrent-"+string(rune('a'+i%26))+string(rune('a'+i/26)), 1)
			if err := h.Consume(context.Background(), "", value); err != nil {
				t.Errorf("并发投递不应报错: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if calls, _ := app.snapshot(); calls != n {
		t.Fatalf("logic 调用次数应为 %d: %d", n, calls)
	}
	if st := h.Stats(); st.Applied != n {
		t.Fatalf("计数应精确等于投递数: %+v", st)
	}
}

func TestOptionsFromUsesConfiguredAttempts(t *testing.T) {
	if o := OptionsFrom(config.KafkaConf{MaxRetries: 9}); o.MaxAttempts != 9 || o.MaxTrackedEvents != defaultMaxTrackedEvents {
		t.Fatalf("OptionsFrom 没有把 Kafka.MaxRetries 接到尝试上限: %+v", o)
	}
	// 配置缺省（0）时归一化到 5：没有上限等于允许无限热循环。
	if o := OptionsFrom(config.KafkaConf{}); o.MaxAttempts != 5 {
		t.Fatalf("MaxRetries 缺省应归一化为 5: %+v", o)
	}
}
