package consumer

// mapping_test.go 钉住 live.state.v1 的读取侧契约。
//
// 关键用例是 TestTranslateReadsProducersVerbatimMessage：它用的是
// services/live-ingest/internal/logic/streamstate.go 真正写进 live_ingest_outbox.payload 的字节，
// 改任何一侧的字段名都会让这条用例变红。生产者与消费者的字段口径必须双向钉死，
// 否则「事件链路已打通」只是两侧各自的良好愿望。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go-video/common/eventenvelope"
	"go-video/services/live-room/model"
)

// producerMessage 是 live-ingest 侧的真实产出（字段名与顺序取自 stateEventPayload 与 Envelope）。
const producerMessage = `{"event_id":"01J8Z4M7Q9T2W6K5R3N8XY6GBA","event_type":"live.state","schema_version":1,` +
	`"occurred_at":"2026-10-04T10:00:00Z","producer":"live-ingest","trace_id":"trace-env-1",` +
	`"aggregate_type":"live_stream","aggregate_id":"S-EVT-32",` +
	`"payload":{"stream_id":"S-EVT-32","room_id":32,"anchor_mid":1002,"session_id":77,` +
	`"stream_state":3,"stream_seq":7,"occurred_at":1700000400,"interrupted_seconds":45,` +
	`"reason":"网络抖动","trace_id":"trace-payload-1"}}`

func mustEnv(t *testing.T, raw string) *eventenvelope.Envelope {
	t.Helper()
	env, err := ParseEnvelope([]byte(raw))
	if err != nil {
		t.Fatalf("信封解析失败: %v", err)
	}
	return env
}

// envelopeBytes 组装一条可序列化的信封（走真实 MarshalJSON，含自校验）。
func envelopeBytes(t *testing.T, eventID, eventType string, version int, aggregateID, payload string) []byte {
	t.Helper()
	raw, err := json.Marshal(&eventenvelope.Envelope{
		EventID:       eventID,
		EventType:     eventType,
		SchemaVersion: version,
		OccurredAt:    "2026-10-04T10:00:00Z",
		Producer:      "live-ingest",
		TraceID:       "trace-env-1",
		AggregateType: "live_stream",
		AggregateID:   aggregateID,
		Payload:       json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("信封序列化失败: %v", err)
	}
	return raw
}

const validPayload = `{"stream_id":"S-1","room_id":32,"session_id":77,"stream_state":2,"stream_seq":1,"occurred_at":1700000400}`

// TestSupportedTopicMatchesEnvelopeRules 版本号写死在代码里，topic 由信封规则推导。
// 有人把 SchemaVersionStreamState 改成 2 而忘了同步订阅配置时，这条用例先红。
func TestSupportedTopicMatchesEnvelopeRules(t *testing.T) {
	if SupportedTopic != "live.state.v1" {
		t.Fatalf("SupportedTopic=%q，与 docs/api-and-events.md §5 登记的 live.state.v1 不一致", SupportedTopic)
	}
	if got := eventenvelope.Topic(EventTypeStreamState, SchemaVersionStreamState); got != SupportedTopic {
		t.Fatalf("订阅串与推导串不一致：%s vs %s", got, SupportedTopic)
	}
}

func TestTranslateReadsProducersVerbatimMessage(t *testing.T) {
	req, err := Translate(mustEnv(t, producerMessage))
	if err != nil {
		t.Fatalf("真实生产者消息必须能翻译: %v", err)
	}
	if req.GetEventId() != "01J8Z4M7Q9T2W6K5R3N8XY6GBA" {
		t.Fatalf("event_id 不符: %q", req.GetEventId())
	}
	if req.GetRoomId() != 32 || req.GetSessionId() != 77 || req.GetStreamId() != "S-EVT-32" {
		t.Fatalf("引用字段不符: %+v", req)
	}
	if req.GetStreamState() != model.StreamStateInterrupted || req.GetStreamSeq() != 7 {
		t.Fatalf("状态口径不符: state=%d seq=%d", req.GetStreamState(), req.GetStreamSeq())
	}
	if req.GetOccurredAt() != 1_700_000_400 || req.GetInterruptedSeconds() != 45 {
		t.Fatalf("时间与中断秒数不符: %+v", req)
	}
	if req.GetReason() != "网络抖动" {
		t.Fatalf("原因丢失: %q", req.GetReason())
	}
	// payload 的 trace_id 优先于信封的：状态迁移发生在推流侧，那条链路的 trace 才对得上。
	if req.GetTraceId() != "trace-payload-1" {
		t.Fatalf("trace_id 应取 payload 值: %q", req.GetTraceId())
	}
}

func TestTranslateFallbacksAndTrimming(t *testing.T) {
	// stream_id 缺省回退 aggregate_id；payload 的 occurred_at 缺省回退信封时间；trace 同理。
	env := mustEnv(t, string(envelopeBytes(t, "evt-fallback", EventTypeStreamState, 1, "S-AGG",
		`{"room_id":8,"stream_state":4,"stream_seq":3}`)))
	req, err := Translate(env)
	if err != nil {
		t.Fatalf("回退路径不应报错: %v", err)
	}
	if req.GetStreamId() != "S-AGG" {
		t.Fatalf("stream_id 应回退 aggregate_id: %q", req.GetStreamId())
	}
	// "2026-10-04T10:00:00Z" 的 Unix 秒，独立算出来而不是复用被测函数的换算。
	if want := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC).Unix(); req.GetOccurredAt() != want {
		t.Fatalf("occurred_at 应回退信封时间：got=%d want=%d", req.GetOccurredAt(), want)
	}
	if req.GetTraceId() != "trace-env-1" {
		t.Fatalf("trace_id 应回退信封值: %q", req.GetTraceId())
	}
	if req.GetSessionId() != 0 || req.GetInterruptedSeconds() != 0 {
		t.Fatalf("缺省数值字段必须是 0 而不是猜测值: %+v", req)
	}

	// event_id 两侧空白要去掉：去重键是按字符串唯一的，留空白会多出一种「同一个事件的两个键」。
	envPad := mustEnv(t, string(envelopeBytes(t, "  evt-pad  ", EventTypeStreamState, 1, "S-1", validPayload)))
	reqPad, err := Translate(envPad)
	if err != nil {
		t.Fatalf("带空白 event_id 应可翻译: %v", err)
	}
	if reqPad.GetEventId() != "evt-pad" {
		t.Fatalf("event_id 未去空白: %q", reqPad.GetEventId())
	}
}

// TestTranslateRejectsBeforeLogic 逐条列出「进 logic 也必然失败」的形态。
// 断言里同时检查错误归属：类型/版本与契约非法要能被 handler 区分开。
func TestTranslateRejectsBeforeLogic(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		version   int
		payload   string
		want      error
		msgPart   string
	}{
		{"未知事件类型", "video.published", 1, validPayload, ErrUnsupportedEventType, "video.published"},
		{"版本不是 v1", EventTypeStreamState, 2, validPayload, ErrUnsupportedSchemaVersion, "v1"},
		{"空 payload", EventTypeStreamState, 1, `{}`, ErrEmptyPayload, ""},
		{"缺 room_id", EventTypeStreamState, 1, `{"stream_state":2,"stream_seq":1}`, ErrInvalidEvent, "room_id"},
		{"room_id 为负", EventTypeStreamState, 1, `{"room_id":-1,"stream_state":2,"stream_seq":1}`, ErrInvalidEvent, "room_id"},
		{"未知流状态 0", EventTypeStreamState, 1, `{"room_id":1,"stream_state":0,"stream_seq":1}`, ErrInvalidEvent, "stream_state"},
		{"未知流状态 5", EventTypeStreamState, 1, `{"room_id":1,"stream_state":5,"stream_seq":1}`, ErrInvalidEvent, "stream_state=5"},
		{"seq 为 0", EventTypeStreamState, 1, `{"room_id":1,"stream_state":2,"stream_seq":0}`, ErrInvalidEvent, "stream_seq"},
		{"seq 为负", EventTypeStreamState, 1, `{"room_id":1,"stream_state":2,"stream_seq":-2}`, ErrInvalidEvent, "stream_seq"},
		{"中断秒数为负", EventTypeStreamState, 1, `{"room_id":1,"stream_state":2,"stream_seq":1,"interrupted_seconds":-1}`, ErrInvalidEvent, "interrupted_seconds"},
		{"occurred_at 为负", EventTypeStreamState, 1, `{"room_id":1,"stream_state":2,"stream_seq":1,"occurred_at":-5}`, ErrInvalidEvent, "occurred_at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Translate(mustEnv(t, string(envelopeBytes(t, "evt-"+tc.name,
				tc.eventType, tc.version, "S-1", tc.payload))))
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望 %v，实得 %v", tc.want, err)
			}
			if tc.msgPart != "" && !strings.Contains(err.Error(), tc.msgPart) {
				t.Fatalf("错误应点名 %q，实得 %v", tc.msgPart, err)
			}
		})
	}

	// event_id 超过 dedup_key 列宽：logic 会把它当成数据库错误，重投一万次也不会变好，
	// 必须在消费侧按契约非法挡住（64 字节边界两侧各测一次）。
	longID := strings.Repeat("k", maxEventIDBytes)
	if _, err := Translate(mustEnv(t, string(envelopeBytes(t, longID, EventTypeStreamState, 1, "S-1", validPayload)))); err != nil {
		t.Fatalf("%d 字节 event_id 应可通过（等于列宽）: %v", maxEventIDBytes, err)
	}
	_, err := Translate(mustEnv(t, string(envelopeBytes(t, longID+"x", EventTypeStreamState, 1, "S-1", validPayload))))
	if !errors.Is(err, ErrInvalidEvent) || !strings.Contains(err.Error(), "dedup_key") {
		t.Fatalf("超长 event_id 应点名 dedup_key 列宽: %v", err)
	}

	if _, err := Translate(nil); !errors.Is(err, ErrNilEvent) {
		t.Fatalf("nil 信封应报 ErrNilEvent: %v", err)
	}
	// 只有 event_id 是空白也不行：logic 会按 ErrEventIDRequired 拒绝，但那之前键已被尝试占用。
	if _, err := Translate(mustEnv(t, string(envelopeBytes(t, "   ", EventTypeStreamState, 1, "S-1", validPayload)))); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("空白 event_id 应报 ErrInvalidEvent: %v", err)
	}
	// payload 没给 occurred_at、信封时间又换算不出正秒数：不能把 0 当合法时间送进 logic
	// （logic 的 occurred_at<0 守卫在占用去重键之后才跑，那会把键白烧掉）。
	bogus := &eventenvelope.Envelope{
		EventID: "evt-bogus-time", EventType: EventTypeStreamState, SchemaVersion: 1,
		OccurredAt: "0001-01-01T00:00:00Z", Producer: "live-ingest",
		AggregateType: "live_stream", AggregateID: "S-1",
		Payload: json.RawMessage(`{"room_id":5,"stream_state":2,"stream_seq":1}`),
	}
	if _, err := Translate(bogus); !errors.Is(err, ErrInvalidEvent) ||
		!strings.Contains(err.Error(), "occurred_at") {
		t.Fatalf("无法确定 occurred_at 应点名 occurred_at: %v", err)
	}
}

// TestUnknownPayloadFieldsAreIgnored 上游误投敏感字段时不得进入本服务：
// 未声明的 JSON 键在反序列化时被丢弃，req 里不可能带出 IP/密钥。
func TestUnknownPayloadFieldsAreIgnored(t *testing.T) {
	env := mustEnv(t, string(envelopeBytes(t, "evt-redact", EventTypeStreamState, 1, "S-1",
		`{"room_id":9,"stream_state":2,"stream_seq":4,"client_ip":"10.1.2.3","stream_key":"sk-secret","token":"tk-topsecret"}`)))
	req, err := Translate(env)
	if err != nil {
		t.Fatalf("多余字段不应报错: %v", err)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("req 序列化失败: %v", err)
	}
	for _, leak := range []string{"10.1.2.3", "sk-secret", "tk-topsecret", "client_ip", "stream_key"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("入参里出现上游误投的敏感字段 %q: %s", leak, raw)
		}
	}
}

func TestParseEnvelopeRejectsMalformed(t *testing.T) {
	if _, err := ParseEnvelope(nil); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("空字节应报 ErrEmptyPayload: %v", err)
	}
	// 语法残缺：json 在进入信封自定义 UnmarshalJSON 之前就失败，
	// 只保证「带包名前缀报出来」，不假装它是信封校验错误。
	if _, err := ParseEnvelope([]byte(`{`)); err == nil ||
		!strings.Contains(err.Error(), "live-room/consumer") {
		t.Fatalf("残缺 JSON 必须报错并带包名前缀: %v", err)
	}
	// 语法完整但字段不满足信封契约：由 eventenvelope 自校验拦住，不留到业务层。
	if _, err := ParseEnvelope([]byte(`{"event_id":"e1","event_type":"live.state","schema_version":1,` +
		`"occurred_at":"2026-10-04T10:00:00Z","aggregate_type":"live_stream","aggregate_id":"S","payload":{}}`)); err == nil ||
		!strings.Contains(err.Error(), "producer") {
		t.Fatalf("缺 producer 应被信封校验拒绝: %v", err)
	}
	if _, err := ParseEnvelope([]byte(`{"event_id":"","event_type":"live.state","schema_version":1,` +
		`"occurred_at":"2026-10-04T10:00:00Z","producer":"live-ingest","aggregate_type":"live_stream",` +
		`"aggregate_id":"S","payload":{}}`)); err == nil || !strings.Contains(err.Error(), "event_id") {
		t.Fatalf("空 event_id 应被信封校验拒绝: %v", err)
	}
	// 载荷本身不是对象。
	if _, err := Translate(mustEnv(t, string(envelopeBytes(t, "evt-bad", EventTypeStreamState, 1, "S-1", `[1,2]`)))); err == nil ||
		!strings.Contains(err.Error(), "payload") {
		t.Fatalf("payload 类型不符应点名 payload: %v", err)
	}
}

func TestDecodePayloadGuards(t *testing.T) {
	if _, err := DecodePayload(nil); !errors.Is(err, ErrNilEvent) {
		t.Fatalf("nil 信封: %v", err)
	}
	env := &eventenvelope.Envelope{Payload: nil}
	if _, err := DecodePayload(env); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("nil payload: %v", err)
	}
}

func TestClassifyCoversEveryLogicResult(t *testing.T) {
	cases := map[int32]Outcome{
		model.StreamResultApplied:           OutcomeApplied,
		model.StreamResultDuplicate:         OutcomeDuplicate,
		model.StreamResultStale:             OutcomeStale,
		model.StreamResultIllegalTransition: OutcomeIllegal,
		model.StreamResultMismatch:          OutcomeMismatch,
		0:                                   OutcomeUnexpected,
		99:                                  OutcomeUnexpected,
		-1:                                  OutcomeUnexpected,
	}
	for result, want := range cases {
		if got := Classify(result); got != want {
			t.Fatalf("result=%d 期望 %s，实得 %s", result, want, got)
		}
	}
}

// TestOutcomeCommitRule 位点推进的唯一例外是「还可以再投」。
// 如果 OutcomeRetry 被判成可提交，故障事件会被直接确认掉，房间投影永久落后。
func TestOutcomeCommitRule(t *testing.T) {
	for _, o := range []Outcome{OutcomeApplied, OutcomeDuplicate, OutcomeStale, OutcomeIllegal,
		OutcomeMismatch, OutcomeSkipped, OutcomeGivenUp, OutcomeUnexpected} {
		if !o.Commit() {
			t.Fatalf("%s 必须允许提交位点，否则毒消息会阻塞整个分区", o)
		}
	}
	if OutcomeRetry.Commit() {
		t.Fatal("OutcomeRetry 不能提交位点")
	}
	if got := []string{OutcomeApplied.String(), OutcomeGivenUp.String(), OutcomeUnexpected.String(), Outcome(42).String()}; got[0] != "applied" ||
		got[1] != "given_up" || got[2] != "unexpected_result" || !strings.HasPrefix(got[3], "outcome_") {
		t.Fatalf("Outcome 文案不符，日志会看不出结论: %v", got)
	}
}
