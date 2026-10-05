package consumer

// handler_test.go 钉住「位点能不能前进」的判定。
//
// 与 live-room 的消费者相比，这里的后果更重：判定通过就是「本场次所有在线档位整场下线」。
// 因此每条判定线都要能证明它真的把动作做（或不做）到位：
//  1. 契约非法（含 Stopped 却没有 session_id）必须 calls==0，用替身的调用次数直接证明
//     「绝不进 logic」，而不是靠读注释相信；
//  2. Affected=0 是成功结论（重复/迟到事件），位点照常前进；
//  3. 依赖故障把注入的错误原样交回队列（errors.Is），并且本进程内封顶尝试次数。
//
// 本包不做事件去重：重复投递仍会进 logic，由 MarkOfflineTx 的 state=在线 CAS 条件兜底
// （见 TestRedeliveryReachesLogicAgain，它把这条设计钉成断言而不是文档）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go-video/services/live-media/internal/config"
	"go-video/services/live-media/model"
)

const stoppedEventID = "01J8Z4M7Q9T2W6K5R3N8XY6GBA"

// scriptedReply 一次调用的应答。
type scriptedReply struct {
	res *OfflineResult
	err error
}

// fakeApplicator 记录每一次入参并按 script 应答；script 用尽后复用最后一条。
type fakeApplicator struct {
	mu     sync.Mutex
	calls  int
	cmds   []*OfflineCommand
	script []scriptedReply
}

func (f *fakeApplicator) OfflineSessionOutputs(_ context.Context, cmd *OfflineCommand) (*OfflineResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.cmds = append(f.cmds, cmd)
	idx := f.calls - 1
	if idx >= len(f.script) {
		idx = len(f.script) - 1
	}
	if idx < 0 {
		return &OfflineResult{Affected: 1, Scanned: 1}, nil
	}
	return f.script[idx].res, f.script[idx].err
}

func (f *fakeApplicator) snapshot() (int, []*OfflineCommand) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]*OfflineCommand(nil), f.cmds...)
}

func resultWith(affected, scanned int32) *OfflineResult {
	return &OfflineResult{Affected: affected, Scanned: scanned}
}

func newHandler(app Applicator, attempts int) *Handler {
	return NewHandler(app, Options{MaxAttempts: attempts})
}

// stoppedMessageWith 换掉真实生产者消息里的 event_id，其余字节不动：
// 用例只需要区分事件身份，不该顺带改到负载。
func stoppedMessageWith(eventID string) string {
	return strings.Replace(producerMessage, stoppedEventID, eventID, 1)
}

func statePayload(state int32) string {
	return fmt.Sprintf(
		`{"stream_id":"S-1","room_id":32,"session_id":77,"stream_state":%d,"stream_seq":3,"occurred_at":1700000400}`,
		state)
}

func TestConsumeOfflinesSessionAndForwardsCommand(t *testing.T) {
	app := &fakeApplicator{script: []scriptedReply{{res: resultWith(3, 3)}}}
	h := newHandler(app, 5)

	if err := h.Consume(context.Background(), "S-EVT-32", producerMessage); err != nil {
		t.Fatalf("已下线的事件必须提交位点: %v", err)
	}
	calls, cmds := app.snapshot()
	if calls != 1 {
		t.Fatalf("logic 应被调用 1 次，实得 %d", calls)
	}
	got := cmds[0]
	if got.EventID != stoppedEventID || got.RoomID != 32 || got.SessionID != 77 {
		t.Fatalf("下线坐标翻译不符: %+v", *got)
	}
	if got.Reason != model.ReasonSourceLost {
		t.Fatalf("断流下线必须是 SOURCE_LOST(%d)，实得 %d（写成 MANUAL 会让运营以为是人摘的）",
			model.ReasonSourceLost, got.Reason)
	}
	if got.TraceID != "trace-payload-1" {
		t.Fatalf("trace_id 必须取自 payload: %q", got.TraceID)
	}
	st := h.Stats()
	// Applied 按「事件」计数而不是按档位数：Affected=3 也只算处理成功一次。
	if st.Applied != 1 || st.Skipped != 0 || st.Retried != 0 || st.Noop != 0 {
		t.Fatalf("计数不符: %+v", st)
	}
	if h.trackedAttempts(stoppedEventID) != 0 {
		t.Fatal("成功后必须释放重试台账")
	}
	if o := h.Options(); o.MaxAttempts != 5 || o.MaxTrackedEvents != defaultMaxTrackedEvents {
		t.Fatalf("Options 归一化不符: %+v", o)
	}
}

// TestNoopIsASuccessfulConclusion Affected=0（重复事件、迟到的旧事件、该场次从未开档位）
// 是成功而不是故障：不提交位点会让分区永久卡在这条事件上，后面的断流全都进不来。
func TestNoopIsASuccessfulConclusion(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  *OfflineResult
	}{
		{"重复事件（一场都没扫到）", resultWith(0, 0)},
		{"扫到但都被并发入口摘了", resultWith(0, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &fakeApplicator{script: []scriptedReply{{res: tc.res}}}
			h := newHandler(app, 5)
			if err := h.Consume(context.Background(), "", producerMessage); err != nil {
				t.Fatalf("无可下线档位也必须提交位点: %v", err)
			}
			st := h.Stats()
			if st.Noop != 1 || st.Applied != 0 || st.Retried != 0 {
				t.Fatalf("应记为 noop: %+v", st)
			}
		})
	}
}

// 未推流 / 推流中 / 中断：事件合法但本服务没有写入动作，一条都不能送进 logic。
// 这条判定线是整个设计里最容易被写歪的地方（把 DecisionNone 当成「执行一个空命令」
// 就会让 Interrupted 事件摘掉整房间档位）。
func TestNonStoppedStatesNeverReachLogic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int32
	}{
		{"未推流", streamStateIdle},
		{"推流中", streamStatePublishing},
		{"中断", streamStateInterrupted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := string(envelopeBytes(t, "evt-"+tc.name, EventTypeStreamState, 1, "S-1", statePayload(tc.state)))
			app := &fakeApplicator{}
			h := newHandler(app, 5)
			if err := h.Consume(context.Background(), "k", value); err != nil {
				t.Fatalf("%s 事件必须提交位点: %v", tc.name, err)
			}
			if calls, _ := app.snapshot(); calls != 0 {
				t.Fatalf("%s 绝不能进 logic（进去就是整场档位下线），实得调用 %d 次", tc.name, calls)
			}
			if st := h.Stats(); st.Skipped != 1 || st.Applied != 0 {
				t.Fatalf("应记为 skipped: %+v", st)
			}
		})
	}
}

// TestContractViolationsNeverReachLogic 覆盖三条判定线里的第一条：
// 本包能判定的非法（含 Stopped 却没有 session_id）不送进 logic。
func TestContractViolationsNeverReachLogic(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"空投递", "   "},
		{"残缺 JSON", `{`},
		{"缺 producer 的信封", `{"event_id":"e","event_type":"live.state","schema_version":1,` +
			`"occurred_at":"2026-10-04T10:00:00Z","aggregate_type":"live_stream","aggregate_id":"S","payload":{}}`},
		{"别人的事件类型", string(envelopeBytes(t, "evt-other", "content.published", 1, "S-1", stoppedPayload))},
		{"版本不认识", string(envelopeBytes(t, "evt-v2", EventTypeStreamState, 2, "S-1", stoppedPayload))},
		{"空 payload", string(envelopeBytes(t, "evt-empty", EventTypeStreamState, 1, "S-1", `{}`))},
		{"payload 不是对象", string(envelopeBytes(t, "evt-arr", EventTypeStreamState, 1, "S-1", `[2]`))},
		{"缺 room_id", string(envelopeBytes(t, "evt-noroom", EventTypeStreamState, 1, "S-1",
			`{"session_id":77,"stream_state":4,"stream_seq":1}`))},
		{"未知流状态", string(envelopeBytes(t, "evt-state", EventTypeStreamState, 1, "S-1",
			`{"room_id":32,"session_id":77,"stream_state":9,"stream_seq":1}`))},
		{"Stopped 但没有 session_id", string(envelopeBytes(t, "evt-nosession", EventTypeStreamState, 1, "S-1",
			`{"room_id":32,"stream_state":4,"stream_seq":1}`))},
		{"event_id 超长", string(envelopeBytes(t, strings.Repeat("e", maxEventIDRunes+1),
			EventTypeStreamState, 1, "S-1", stoppedPayload))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &fakeApplicator{}
			h := newHandler(app, 5)
			if err := h.Consume(context.Background(), "k", tc.value); err != nil {
				t.Fatalf("永久非法的投递必须确认掉，否则毒消息阻塞分区: %v", err)
			}
			if calls, _ := app.snapshot(); calls != 0 {
				t.Fatalf("契约非法不能送进 logic（会拿半个坐标去下线整场档位），实得调用 %d 次", calls)
			}
			if st := h.Stats(); st.Skipped != 1 {
				t.Fatalf("应记为 skipped: %+v", st)
			}
		})
	}
}

// 非法事件的留痕口径也要钉住：LastError 必须留下原因，否则「档位为什么还在线」查不出来。
func TestContractViolationKeepsLastError(t *testing.T) {
	app := &fakeApplicator{}
	h := newHandler(app, 5)
	value := string(envelopeBytes(t, "evt-nosession", EventTypeStreamState, 1, "S-1",
		`{"room_id":32,"stream_state":4,"stream_seq":1}`))
	if err := h.Consume(context.Background(), "k", value); err != nil {
		t.Fatalf("非法事件应确认掉: %v", err)
	}
	st := h.Stats()
	if !strings.Contains(st.LastError, "session_id") {
		t.Fatalf("LastError 必须点名字段，实得 %q", st.LastError)
	}
	// 事件类型/版本不认识属于正常噪声，不计入 LastError：
	// 否则订阅面一宽，LastError 就被噪声占满，真契约违反看不出来。
	noise := &fakeApplicator{}
	hn := newHandler(noise, 5)
	if err := hn.Consume(context.Background(), "k",
		string(envelopeBytes(t, "evt-v9", EventTypeStreamState, 9, "S-1", stoppedPayload))); err != nil {
		t.Fatalf("不认识的版本应跳过: %v", err)
	}
	if got := hn.Stats(); got.LastError == "" || !strings.Contains(got.LastError, "schema_version") {
		t.Fatalf("版本不认识的跳过原因也要留痕: %+v", got)
	}
}

// TestDependencyFailureRetriesThenGivesUp 封顶尝试次数：
// 本服务没有持久化消费位点表，没有上限就会让注定失败的事件在分区里无限热循环，
// 把后面的断流事件全堵住（后果是整个房间的档位迟迟不摘）。
func TestDependencyFailureRetriesThenGivesUp(t *testing.T) {
	boom := errors.New("dial tcp 127.0.0.1:3306: connect refused")
	app := &fakeApplicator{script: []scriptedReply{{err: boom}}}
	h := newHandler(app, 3)

	for i := 1; i <= 2; i++ {
		err := h.Consume(context.Background(), "", producerMessage)
		if !errors.Is(err, boom) {
			t.Fatalf("第 %d 次应把依赖错误原样交回队列（不提交位点）: %v", i, err)
		}
		if got := h.trackedAttempts(stoppedEventID); got != i {
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
	if h.trackedAttempts(stoppedEventID) != 0 {
		t.Fatal("放弃后必须释放台账，否则 map 会堆满死事件")
	}
}

// TestRecoveryAfterFailureResetsLedger 一次成功要把尝试计数清零：
// 否则同一场次后续的真故障会被上一轮历史失败直接判死。
func TestRecoveryAfterFailureResetsLedger(t *testing.T) {
	boom := errors.New("临时故障：事务失败")
	app := &fakeApplicator{script: []scriptedReply{
		{err: boom},
		{res: resultWith(2, 2)},
		{err: boom},
		{res: resultWith(0, 0)},
	}}
	h := newHandler(app, 2)

	if err := h.Consume(context.Background(), "", producerMessage); !errors.Is(err, boom) {
		t.Fatalf("首次故障应重投: %v", err)
	}
	if err := h.Consume(context.Background(), "", producerMessage); err != nil {
		t.Fatalf("恢复后应提交位点: %v", err)
	}
	if got := h.trackedAttempts(stoppedEventID); got != 0 {
		t.Fatalf("成功后台账没清: %d", got)
	}
	// 第三/四次：新的故障周期，仍从第 1 次开始，不会因为历史失败被判死。
	if err := h.Consume(context.Background(), "", producerMessage); !errors.Is(err, boom) {
		t.Fatalf("新一轮故障仍应允许重投: %v", err)
	}
	if err := h.Consume(context.Background(), "", producerMessage); err != nil {
		t.Fatalf("最终收敛应提交位点: %v", err)
	}
	if st := h.Stats(); st.Retried != 2 || st.GivenUp != 0 || st.Applied != 1 || st.Noop != 1 {
		t.Fatalf("计数不符: %+v", st)
	}
}

// TestNilResultWithoutErrorIsRetryable logic 给出 (nil, nil) 时既没结论也没错误，
// 不能当成「下线成功」：那会把位点推过去，档位却一条没摘。
func TestNilResultWithoutErrorIsRetryable(t *testing.T) {
	app := &fakeApplicator{script: []scriptedReply{{res: nil, err: nil}}}
	h := newHandler(app, 5)
	if err := h.Consume(context.Background(), "", producerMessage); !errors.Is(err, ErrNilResult) {
		t.Fatalf("(nil, nil) 必须按故障处理: %v", err)
	}
	if st := h.Stats(); st.Retried != 1 || st.Applied != 0 || st.Noop != 0 {
		t.Fatalf("空结果不能记成成功: %+v", st)
	}
}

// TestRedeliveryReachesLogicAgain 把「本包不做事件去重」钉成断言。
// 去重靠 live_stream_output 的行状态（CAS 条件 state=在线），第二次调用会返回 Affected=0；
// 如果这里改成按 event_id 拦，就需要一张持久化位点表，而这张表本服务没有。
func TestRedeliveryReachesLogicAgain(t *testing.T) {
	app := &fakeApplicator{script: []scriptedReply{{res: resultWith(2, 2)}, {res: resultWith(0, 0)}}}
	h := newHandler(app, 5)
	if err := h.Consume(context.Background(), "", producerMessage); err != nil {
		t.Fatalf("首次投递应提交位点: %v", err)
	}
	if err := h.Consume(context.Background(), "", producerMessage); err != nil {
		t.Fatalf("重投同样应提交位点: %v", err)
	}
	calls, _ := app.snapshot()
	if calls != 2 {
		t.Fatalf("本包必须让重投继续进 logic（幂等由行状态保证），实得 %d 次", calls)
	}
	if st := h.Stats(); st.Applied != 1 || st.Noop != 1 {
		t.Fatalf("两次投递应分别记 applied 与 noop: %+v", st)
	}
}

// TestLedgerIsBounded 容量只是内存防线：打满时逐出一条，被逐出的事件重新获得额度。
// 正确性仍由 CAS 下线条件保证，所以这里的代价是多做几次无用功而不是重复下线。
func TestLedgerIsBounded(t *testing.T) {
	boom := errors.New("依赖故障")
	app := &fakeApplicator{script: []scriptedReply{{err: boom}}}
	h := NewHandler(app, Options{MaxAttempts: 3, MaxTrackedEvents: 1})

	for _, eventID := range []string{"evt-A", "evt-B", "evt-C"} {
		if err := h.Consume(context.Background(), "", stoppedMessageWith(eventID)); !errors.Is(err, boom) {
			t.Fatalf("%s 的首次故障应重投: %v", eventID, err)
		}
	}
	if got := h.trackedAttempts("evt-A"); got != 0 {
		t.Fatalf("台账容量只有 1，evt-A 应已被逐出（逐出=重新获得尝试额度），实得 %d", got)
	}
	if got := h.trackedAttempts("evt-B"); got != 0 {
		t.Fatalf("evt-B 同样被逐出: %d", got)
	}
	if got := h.trackedAttempts("evt-C"); got != 1 {
		t.Fatalf("台账里必须只剩最后写入的 evt-C: %d", got)
	}
	// 逐出只影响「封顶」而不影响正确性：evt-A 再来一次仍会进 logic，
	// 上一次没提交成功的那些档位仍是在线态，多下线的行由 CAS 条件挡掉。
	if err := h.Consume(context.Background(), "", stoppedMessageWith("evt-A")); !errors.Is(err, boom) {
		t.Fatalf("被逐出的事件重投应重新获得额度并继续重投: %v", err)
	}
	if calls, _ := app.snapshot(); calls != 4 {
		t.Fatalf("四次投递都应到达 logic（去重由行状态负责，不在本包）: %d", calls)
	}
}

// TestConcurrentConsumeKeepsCounters 并发投递只能靠互斥保证计数与台账一致。
// 本机没装 cgo，-race 跑不了，因此这里用确定性的总量断言兜住（少一次就是丢计数）。
func TestConcurrentConsumeKeepsCounters(t *testing.T) {
	app := &fakeApplicator{script: []scriptedReply{{res: resultWith(1, 1)}}}
	h := newHandler(app, 5)

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			eventID := fmt.Sprintf("evt-concurrent-%02d", i)
			if err := h.Consume(context.Background(), "", stoppedMessageWith(eventID)); err != nil {
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

// TestOutcomeCommitAndLabels 只有 OutcomeRetry 不提交位点：
// 一旦写反（例如让 Skipped 也重投），一条毒消息就能永久阻塞分区。
func TestOutcomeCommitAndLabels(t *testing.T) {
	cases := []struct {
		outcome Outcome
		label   string
		commit  bool
	}{
		{OutcomeApplied, "applied", true},
		{OutcomeNoop, "noop", true},
		{OutcomeSkipped, "skipped", true},
		{OutcomeRetry, "retry", false},
		{OutcomeGivenUp, "given_up", true},
	}
	for _, tc := range cases {
		if tc.outcome.Commit() != tc.commit {
			t.Fatalf("%s 的位点判定不符: commit=%v want=%v", tc.label, tc.outcome.Commit(), tc.commit)
		}
		if tc.outcome.String() != tc.label {
			t.Fatalf("Outcome 文本变了（日志与运维口径）: got=%q want=%q", tc.outcome, tc.label)
		}
	}
	if got := Outcome(99).String(); got != "outcome_99" {
		t.Fatalf("未知 Outcome 必须显式露出编号，got=%q", got)
	}
}

func TestOptionsFromUsesConfiguredAttempts(t *testing.T) {
	if o := OptionsFrom(config.KafkaConf{MaxRetries: 9}); o.MaxAttempts != 9 ||
		o.MaxTrackedEvents != defaultMaxTrackedEvents {
		t.Fatalf("OptionsFrom 没有把 Kafka.MaxRetries 接到尝试上限: %+v", o)
	}
	// 配置缺省（0）时归一化到 5：没有上限等于允许无限热循环。
	if o := OptionsFrom(config.KafkaConf{}); o.MaxAttempts != 5 {
		t.Fatalf("MaxRetries 缺省应归一化为 5: %+v", o)
	}
	if o := OptionsFrom(config.KafkaConf{MaxRetries: -3}); o.MaxAttempts != 5 {
		t.Fatalf("负数上限同样要归一化: %+v", o)
	}
}
