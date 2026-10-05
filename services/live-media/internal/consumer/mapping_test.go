package consumer

// mapping_test.go 钉住 live.state.v1 的读取侧契约。
//
// 两条独立的钉法，缺一不可：
//  1. producerMessage 是 services/live-ingest/internal/logic/streamstate.go 真正写进
//     live_ingest_outbox.payload 的字节（含该服务才会有的字段顺序），改本包 StreamStatePayload
//     的字段名就会红；
//  2. TestConsumerContractMatchesProducerSource 直接读生产者源码，把 stateEventPayload 的
//     json tag 序列与 StreamState* 常量取值逐个比对。第 1 条只能证明「两边今天一致」，
//     第 2 条才能在有人只改一侧的下一轮把漂移当场判红（本仓库不 import 对方 model，
//     源码文本是唯一可用的跨服务对照物，手法先例见 common/eventenvelope/event_type_gate_test.go）。
//
// 本服务的判定比 live-room 更窄也更危险：只有 stream_state=Stopped 会动手，
// 而且动手就是「本场次整场档位下线」。因此非法事件必须报错（不能退化成不动作），
// 而 Stopped 缺 session_id 也必须拒绝（按房间下线会误伤刚开播的场次）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go-video/common/eventenvelope"
	"go-video/services/live-media/model"
)

// producerMessage 取自生产者真实产出（字段名与顺序来自 stateEventPayload 与 Envelope）。
// 这里的 stream_state=4 就是 STOPPED：整个下线链路只由它触发。
const producerMessage = `{"event_id":"01J8Z4M7Q9T2W6K5R3N8XY6GBA","event_type":"live.state","schema_version":1,` +
	`"occurred_at":"2026-10-04T10:00:00Z","producer":"live-ingest","trace_id":"trace-env-1",` +
	`"aggregate_type":"live_stream","aggregate_id":"S-EVT-32",` +
	`"payload":{"stream_id":"S-EVT-32","room_id":32,"anchor_mid":1002,"session_id":77,` +
	`"stream_state":4,"stream_seq":7,"occurred_at":1700000400,"interrupted_seconds":45,` +
	`"reason":"主播关闭推流","trace_id":"trace-payload-1"}}`

const stoppedPayload = `{"stream_id":"S-1","room_id":32,"session_id":77,"stream_state":4,"stream_seq":7,` +
	`"occurred_at":1700000400}`

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

// interpretBytes 解析并翻译一条投递，失败即 Fatalf（用例只关心判定分支时不再层层判错）。
func interpretBytes(t *testing.T, raw []byte) (*Event, Decision) {
	t.Helper()
	env, err := ParseEnvelope(raw)
	if err != nil {
		t.Fatalf("信封解析失败: %v", err)
	}
	cmd, decision, err := Interpret(env)
	if err != nil {
		t.Fatalf("期望放行的事件被拒绝: %v", err)
	}
	return cmd, decision
}

// handEnv 直接构造信封对象，绕过 eventenvelope 的序列化自校验。
//
// 表里的坏取值（空 event_id、schema_version=0）在真实投递上会被信封层先拦掉
// （见 TestParseEnvelopeRejectsBrokenEnvelopeFields），本用例要钉的是
// 「翻译层自己也不放过它们」：Interpret 的调用方不保证都先过信封层，
// 而 wiring 与单测里造出来的假事件本来就是手搓的 Envelope 结构体。
func handEnv(eventID, eventType string, version int, payload string) *eventenvelope.Envelope {
	return &eventenvelope.Envelope{
		EventID:       eventID,
		EventType:     eventType,
		SchemaVersion: version,
		OccurredAt:    "2026-10-04T10:00:00Z",
		Producer:      "live-ingest",
		TraceID:       "trace-env-1",
		AggregateType: "live_stream",
		AggregateID:   "S-1",
		Payload:       json.RawMessage(payload),
	}
}

func TestSupportedTopicMatchesEnvelopeRules(t *testing.T) {
	if SupportedTopic != "live.state.v1" {
		t.Fatalf("SupportedTopic=%q，与 docs/api-and-events.md §5 登记的 live.state.v1 不一致", SupportedTopic)
	}
	if got := eventenvelope.Topic(EventTypeStreamState, SchemaVersionStreamState); got != SupportedTopic {
		t.Fatalf("订阅串与推导串不一致：%s vs %s", got, SupportedTopic)
	}
	if EventTypeStreamState != "live.state" {
		t.Fatalf("event_type 必须逐字等于生产者写入的 live.state（改名会同时破坏 live-room 消费者与命名门禁），got=%q",
			EventTypeStreamState)
	}
}

// 真实生产者消息必须翻成「整场档位下线」，且四个坐标一个都不能错。
func TestInterpretReadsProducersVerbatimMessage(t *testing.T) {
	env, err := ParseEnvelope([]byte(producerMessage))
	if err != nil {
		t.Fatalf("真实生产者消息必须能解析: %v", err)
	}
	ev, decision, err := Interpret(env)
	if err != nil {
		t.Fatalf("真实生产者消息必须能翻译: %v", err)
	}
	if decision != DecisionOffline {
		t.Fatalf("stream_state=Stopped 必须判定下线，got=%s", decision)
	}
	cmd := ev.Command
	if cmd == nil {
		t.Fatalf("DecisionOffline 必须带回命令")
	}
	if cmd.EventID != "01J8Z4M7Q9T2W6K5R3N8XY6GBA" {
		t.Fatalf("event_id 不符: %q", cmd.EventID)
	}
	if cmd.RoomID != 32 || cmd.SessionID != 77 {
		t.Fatalf("下线坐标不符: room=%d session=%d", cmd.RoomID, cmd.SessionID)
	}
	if cmd.Reason != model.ReasonSourceLost {
		t.Fatalf("断流下线的 reason 必须是 SOURCE_LOST(%d)，got=%d", model.ReasonSourceLost, cmd.Reason)
	}
	// payload 的 trace 优先于信封的 trace：信封 trace 可能是上游内部触发时的空值。
	if cmd.TraceID != "trace-payload-1" {
		t.Fatalf("trace_id 应取 payload 侧，got=%q", cmd.TraceID)
	}
	// 未参与判定的字段也必须原样带回来：排障时要能说出这条事件是哪条流第几号、断了多久。
	if ev.Payload.StreamID != "S-EVT-32" || ev.Payload.AnchorMid != 1002 || ev.Payload.StreamSeq != 7 {
		t.Fatalf("负载字段不符: %+v", ev.Payload)
	}
	if ev.Payload.InterruptedSeconds != 45 || ev.Payload.OccurredAt != 1_700_000_400 {
		t.Fatalf("中断秒数/事件时间不符: %+v", ev.Payload)
	}
	if ev.Payload.Reason != "主播关闭推流" {
		t.Fatalf("生产者给的文本原因丢失: %q", ev.Payload.Reason)
	}
}

// 只有 Stopped 动手，其余三个状态是正常噪声：判定为「不动作」但要把负载带回来写日志。
// 每条都断言 Command==nil，否则「不动作」会退化成拿着 nil 命令去调 logic。
func TestInterpretTakesNoActionOnNonStoppedStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int32
		label string
	}{
		{"未推流", streamStateIdle, "IDLE"},
		{"推流中", streamStatePublishing, "PUBLISHING"},
		{"中断", streamStateInterrupted, "INTERRUPTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(
				`{"stream_id":"S-1","room_id":32,"session_id":77,"stream_state":%d,"stream_seq":3,"occurred_at":1700000400}`,
				tc.state)
			ev, decision := interpretBytes(t, envelopeBytes(t, "evt-"+tc.label, EventTypeStreamState, 1, "S-1", payload))
			if decision != DecisionNone {
				t.Fatalf("%s 不该触发动作，got=%s", tc.name, decision)
			}
			if ev.Command != nil {
				t.Fatalf("%s 的判定必须不带命令，got=%+v", tc.name, *ev.Command)
			}
			if ev.Payload.StreamState != tc.state || ev.Payload.RoomID != 32 || ev.Payload.SessionID != 77 {
				t.Fatalf("不动作也要能报出这条事件是哪个房间哪一场什么状态: %+v", ev.Payload)
			}
			if stateLabel(tc.state) != tc.label {
				t.Fatalf("日志里的状态名必须与契约一致，got=%q want=%q", stateLabel(tc.state), tc.label)
			}
		})
	}
}

// 非法事件一律报错，绝不退化成 DecisionNone。
// 表里成对放「相邻但结论不同」的取值：状态 0/5 非法而 1..4 合法、场次 0 非法而 1 合法，
// 这样把边界写成 <= 或 < 的任一形式都会红在其中一条上。
func TestInterpretRejectsContractViolations(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		version   int
		eventID   string
		payload   string
		wantErr   error
		wantText  string
	}{
		{"别人的事件类型", "content.published", 1, "evt-1", stoppedPayload, ErrUnsupportedEventType, "content.published"},
		{"事件类型缺前缀", "state", 1, "evt-1", stoppedPayload, ErrUnsupportedEventType, "state"},
		{"版本不是 v1", EventTypeStreamState, 2, "evt-1", stoppedPayload, ErrUnsupportedSchemaVersion, "v1"},
		{"版本为 0", EventTypeStreamState, 0, "evt-1", stoppedPayload, ErrUnsupportedSchemaVersion, ""},
		{"空 payload", EventTypeStreamState, 1, "evt-1", `{}`, ErrEmptyPayload, ""},
		{"payload 不是对象", EventTypeStreamState, 1, "evt-1", `[1,2]`, ErrInvalidEvent, ""},
		{"event_id 为空", EventTypeStreamState, 1, ``, stoppedPayload, ErrInvalidEvent, "event_id"},
		{"event_id 全空白", EventTypeStreamState, 1, "   ", stoppedPayload, ErrInvalidEvent, "event_id"},
		{"缺 room_id", EventTypeStreamState, 1, "evt-1",
			`{"session_id":77,"stream_state":4,"stream_seq":1}`, ErrInvalidEvent, "room_id"},
		{"room_id 为 0", EventTypeStreamState, 1, "evt-1",
			`{"room_id":0,"session_id":77,"stream_state":4,"stream_seq":1}`, ErrInvalidEvent, "room_id=0"},
		{"room_id 为负", EventTypeStreamState, 1, "evt-1",
			`{"room_id":-1,"session_id":77,"stream_state":4,"stream_seq":1}`, ErrInvalidEvent, "room_id"},
		{"未知流状态 0", EventTypeStreamState, 1, "evt-1",
			`{"room_id":32,"session_id":77,"stream_state":0,"stream_seq":1}`, ErrInvalidEvent, "stream_state=0"},
		{"未知流状态 5", EventTypeStreamState, 1, "evt-1",
			`{"room_id":32,"session_id":77,"stream_state":5,"stream_seq":1}`, ErrInvalidEvent, "stream_state=5"},
		{"seq 为负", EventTypeStreamState, 1, "evt-1",
			`{"room_id":32,"session_id":77,"stream_state":4,"stream_seq":-2}`, ErrInvalidEvent, "stream_seq"},
		{"中断秒数为负", EventTypeStreamState, 1, "evt-1",
			`{"room_id":32,"session_id":77,"stream_state":4,"stream_seq":1,"interrupted_seconds":-1}`,
			ErrInvalidEvent, "interrupted_seconds"},
		{"事件时间为负", EventTypeStreamState, 1, "evt-1",
			`{"room_id":32,"session_id":77,"stream_state":4,"stream_seq":1,"occurred_at":-5}`,
			ErrInvalidEvent, "occurred_at"},
		{"Stopped 但 session_id 为 0", EventTypeStreamState, 1, "evt-1",
			`{"room_id":32,"session_id":0,"stream_state":4,"stream_seq":1}`, ErrInvalidEvent, "session_id"},
		{"Stopped 但 session_id 为负", EventTypeStreamState, 1, "evt-1",
			`{"room_id":32,"session_id":-9,"stream_state":4,"stream_seq":1}`, ErrInvalidEvent, "session_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, decision, err := Interpret(handEnv(tc.eventID, tc.eventType, tc.version, tc.payload))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got=%v want=%v", err, tc.wantErr)
			}
			if decision != DecisionNone {
				t.Fatalf("非法事件不得被判成可执行动作，got=%s", decision)
			}
			if ev != nil {
				t.Fatalf("非法事件不得带回事件对象（调用方会拿着它继续走）：%+v", *ev)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("错误必须点名是哪个字段：%v", err)
			}
		})
	}
}

// 信封层（eventenvelope 的序列化自校验）是这道链路的第一道闸：
// 表里的空 event_id / schema_version=0 在真实投递上根本到不了 Interpret。
// 本用例钉住「闸确实在」，而不是让读者以为 Interpret 是唯一防线：
// 把这两条从信封层去掉，坏事件就会带着空 event_id 进下线事件 payload，
// 之后按 event_id 反查「档位为什么被摘」会查到一条空引用。
func TestParseEnvelopeRejectsBrokenEnvelopeFields(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantText string
	}{
		{"event_id 为空", brokenEnvelope("", "live.state", 1, "live-ingest"), "event_id"},
		{"schema_version 为 0", brokenEnvelope("evt-1", "live.state", 0, "live-ingest"), "schema_version"},
		{"缺 producer", brokenEnvelope("evt-1", "live.state", 1, ""), "producer"},
		{"event_type 含大写", brokenEnvelope("evt-1", "Live.State", 1, "live-ingest"), "event_type"},
		{"event_type 为空", brokenEnvelope("evt-1", "", 1, "live-ingest"), "event_type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := ParseEnvelope([]byte(tc.raw))
			if err == nil {
				t.Fatalf("坏信封必须被 eventenvelope 拒绝，got env=%+v", env)
			}
			if env != nil {
				t.Fatalf("拒绝时不该带回信封对象：%+v", *env)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("错误必须点名是哪个字段：%v", err)
			}
		})
	}
}

// brokenEnvelope 手拼一条坏信封：只有绕过 MarshalJSON 才能把坏取值送进解析层，
// 从结构体 marshal 出去会在序列化阶段就报错，测不到 UnmarshalJSON 的校验。
func brokenEnvelope(eventID, eventType string, version int, producer string) string {
	return `{"event_id":"` + eventID + `","event_type":"` + eventType + `","schema_version":` +
		strconv.Itoa(version) + `,"occurred_at":"2026-10-04T10:00:00Z","producer":"` + producer +
		`","aggregate_type":"live_stream","aggregate_id":"S-1","payload":` + stoppedPayload + `}`
}

// 长度门禁成对：64 字符（与本服务事件引用列宽一致）放行，65 字符拒绝。
// 只测超长那一条会让「上限写成任意值」看不出来；两条一起才钉住边界本身。
func TestInterpretEventIDLengthBoundary(t *testing.T) {
	atLimit := strings.Repeat("e", maxEventIDRunes)
	env := mustEnv(t, string(envelopeBytes(t, atLimit, EventTypeStreamState, 1, "S-1", stoppedPayload)))
	if _, decision, err := Interpret(env); err != nil || decision != DecisionOffline {
		t.Fatalf("恰好到上限的事件 ID 必须放行：decision=%s err=%v", decision, err)
	}

	over := atLimit + "x"
	envOver := mustEnv(t, string(envelopeBytes(t, over, EventTypeStreamState, 1, "S-1", stoppedPayload)))
	if _, _, err := Interpret(envOver); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("超长事件 ID 必须按契约违反拒绝：got=%v", err)
	} else if !strings.Contains(err.Error(), fmt.Sprint(maxEventIDRunes)) {
		t.Fatalf("错误必须给出上限：%v", err)
	}
}

// 非 ASCII 的 event_id 按「字符数」而不是字节数判：
// 一个 64 汉字的 ID 只有 192 字节但 64 字符，必须放行（否则按字节判会红在这里）。
func TestInterpretEventIDCountsRunesNotBytes(t *testing.T) {
	id := strings.Repeat("汉", maxEventIDRunes)
	env := mustEnv(t, string(envelopeBytes(t, id, EventTypeStreamState, 1, "S-1", stoppedPayload)))
	if _, _, err := Interpret(env); err != nil {
		t.Fatalf("%d 字符（%d 字节）的事件 ID 不该被拒：%v", maxEventIDRunes, len([]byte(id)), err)
	}
	envLong := mustEnv(t, string(envelopeBytes(t, id+"汉", EventTypeStreamState, 1, "S-1", stoppedPayload)))
	if _, _, err := Interpret(envLong); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("超出 1 个字符就该拒绝：got=%v", err)
	}
}

// trace_id 的三级取值：payload 有则用 payload，没有则退回信封，都没有就是空串（不编造）。
func TestInterpretTraceIDFallbackOrder(t *testing.T) {
	t.Run("payload 优先", func(t *testing.T) {
		env := mustEnv(t, string(envelopeBytes(t, "evt-t1", EventTypeStreamState, 1, "S-1",
			`{"room_id":32,"session_id":77,"stream_state":4,"stream_seq":1,"trace_id":"trace-payload"}`)))
		ev, _, err := Interpret(env)
		if err != nil {
			t.Fatalf("期望放行的事件被拒绝: %v", err)
		}
		if ev.Command.TraceID != "trace-payload" {
			t.Fatalf("got=%q", ev.Command.TraceID)
		}
	})
	t.Run("退回信封", func(t *testing.T) {
		env := mustEnv(t, string(envelopeBytes(t, "evt-t2", EventTypeStreamState, 1, "S-1", stoppedPayload)))
		ev, _, err := Interpret(env)
		if err != nil {
			t.Fatalf("期望放行的事件被拒绝: %v", err)
		}
		if ev.Command.TraceID != "trace-env-1" {
			t.Fatalf("payload 没有 trace 时必须用信封的，got=%q", ev.Command.TraceID)
		}
	})
	t.Run("两处都没有", func(t *testing.T) {
		raw, err := json.Marshal(&eventenvelope.Envelope{
			EventID: "evt-t3", EventType: EventTypeStreamState, SchemaVersion: 1,
			OccurredAt: "2026-10-04T10:00:00Z", Producer: "live-ingest",
			AggregateType: "live_stream", AggregateID: "S-1",
			Payload: json.RawMessage(stoppedPayload),
		})
		if err != nil {
			t.Fatalf("信封序列化失败: %v", err)
		}
		env := mustEnv(t, string(raw))
		ev, _, err := Interpret(env)
		if err != nil {
			t.Fatalf("期望放行的事件被拒绝: %v", err)
		}
		if ev.Command.TraceID != "" {
			t.Fatalf("没有 trace 时必须是空串而不是编造值，got=%q", ev.Command.TraceID)
		}
	})
}

// session_id 合法但极小（1）也必须放行：判定只看正负，不能把小 ID 当成缺失。
func TestInterpretAcceptsMinimalSessionID(t *testing.T) {
	env := mustEnv(t, string(envelopeBytes(t, "evt-min", EventTypeStreamState, 1, "S-1",
		`{"room_id":1,"session_id":1,"stream_state":4,"stream_seq":0}`)))
	ev, decision, err := Interpret(env)
	if err != nil || decision != DecisionOffline {
		t.Fatalf("got decision=%s err=%v", decision, err)
	}
	if ev.Command.RoomID != 1 || ev.Command.SessionID != 1 {
		t.Fatalf("坐标被改写: %+v", *ev.Command)
	}
}

func TestParseEnvelopeAndDecodePayloadGuards(t *testing.T) {
	if _, err := ParseEnvelope(nil); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("空投递必须报缺 payload，got=%v", err)
	}
	if _, err := ParseEnvelope([]byte("not json")); err == nil {
		t.Fatal("非法 JSON 必须报错")
	}
	// 信封自校验在 UnmarshalJSON 里：缺 producer 的事件连解析都过不了（不留给业务层猜来源）。
	if _, err := ParseEnvelope([]byte(`{"event_id":"e","event_type":"live.state","schema_version":1,` +
		`"occurred_at":"2026-10-04T10:00:00Z","aggregate_type":"live_stream","aggregate_id":"S","payload":{}}`)); err == nil {
		t.Fatal("缺 producer 的信封必须被 eventenvelope 拒绝")
	}
	if _, err := DecodePayload(nil); !errors.Is(err, ErrNilEvent) {
		t.Fatalf("nil 信封必须报 ErrNilEvent，got=%v", err)
	}
	if _, _, err := Interpret(nil); !errors.Is(err, ErrNilEvent) {
		t.Fatalf("Interpret(nil) 必须报错而不是返回默认判定，got=%v", err)
	}
}

// Decision 与 Outcome 的字符串是本包日志与运维排查的口径，必须稳定可读。
func TestDecisionAndStateLabels(t *testing.T) {
	if DecisionOffline.String() != "offline_session_outputs" || DecisionNone.String() != "none" {
		t.Fatalf("Decision 文本变了：%s / %s", DecisionOffline, DecisionNone)
	}
	if got := stateLabel(99); got != "STATE_99" {
		t.Fatalf("未知状态必须在日志里显式露出编号，got=%q", got)
	}
}

// ---------------------------------------------------------------- 生产者源码对照

var (
	producerPayloadBlock = regexp.MustCompile(`(?s)type stateEventPayload struct \{(.*?)\n\}`)
	jsonTagRe            = regexp.MustCompile(`json:"([^",]+)`)
	producerStateConst   = regexp.MustCompile(`(StreamState\w+)\s+int32\s*=\s*(\d+)`)
)

// TestConsumerContractMatchesProducerSource 逐字段、逐取值比对生产者源码。
//
// 为什么不能只靠上面的 producerMessage 常量：那是手抄的副本，生产者改了字段名，
// 副本不会自己变，于是两边各自绿、线上断链。本用例直接读
// services/live-ingest/internal/logic/streamstate.go 与 model/errors.go，
// 因此任何一侧单独改动都会在这里红（本包不 import 对方 internal，源码文本是唯一对照物）。
func TestConsumerContractMatchesProducerSource(t *testing.T) {
	src := readProducerFile(t, "../../../../services/live-ingest/internal/logic/streamstate.go")
	m := producerPayloadBlock.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("生产者源码里找不到 stateEventPayload：字段表已被改名或删除，本用例必须跟着改，不能删")
	}
	producerTags := jsonTagRe.FindAllStringSubmatch(m[1], -1)
	want := make([]string, 0, len(producerTags))
	for _, g := range producerTags {
		want = append(want, g[1])
	}

	got := consumerJSONTags(t, StreamStatePayload{})
	if len(got) != len(want) {
		t.Fatalf("消费者负载字段数与生产者不一致：got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个字段口径不一致：消费者 %q，生产者 %q（字段名漂移=读不到，静默丢下线）",
				i+1, got[i], want[i])
		}
	}

	stateSrc := readProducerFile(t, "../../../../services/live-ingest/model/errors.go")
	producerStates := map[string]int32{}
	for _, g := range producerStateConst.FindAllStringSubmatch(stateSrc, -1) {
		var v int64
		if _, err := fmt.Sscanf(g[2], "%d", &v); err != nil {
			t.Fatalf("解析生产者状态常量 %s=%s 失败：%v", g[1], g[2], err)
		}
		producerStates[g[1]] = int32(v)
	}
	local := map[string]int32{
		"StreamStateIdle":        streamStateIdle,
		"StreamStatePublishing":  streamStatePublishing,
		"StreamStateInterrupted": streamStateInterrupted,
		"StreamStateStopped":     streamStateStopped,
	}
	for name, mine := range local {
		theirs, ok := producerStates[name]
		if !ok {
			t.Fatalf("生产者源码里找不到 %s：状态表已被改名，本包的值域必须重读", name)
		}
		if mine != theirs {
			t.Fatalf("状态 %s 取值不一致：消费者 %d，生产者 %d（重排会让 Stopped 变成别人的状态）",
				name, mine, theirs)
		}
	}
	if len(producerStates) != len(local) {
		t.Fatalf("生产者状态数(%d)与本包值域(%d)不一致：新增状态必须补翻译而不是安静吃掉",
			len(producerStates), len(local))
	}
}

// readProducerFile 读生产者的源文件。读不到不是跳过理由：那说明目录结构变了，
// 对照点必须重新安置，Skip 会把「契约无人核对」伪装成「测试通过」。
func readProducerFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("读取生产者源码失败 %s: %v（结构变动后必须同步本用例的对照路径）", rel, err)
	}
	return string(raw)
}

// consumerJSONTags 按字段声明顺序取出 json tag，顺序与生产者不一致时也要能报出来
// （顺序不影响解码，但影响「两边是不是同一份表」的可核对性）。
func consumerJSONTags(t *testing.T, v StreamStatePayload) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	tags := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" {
			t.Fatalf("字段 %s 没有 json tag，无法与生产者比对", typ.Field(i).Name)
		}
		tags = append(tags, name)
	}
	return tags
}
