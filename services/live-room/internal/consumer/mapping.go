// Package consumer 是 live-room 的事件消费侧（AGENTS.md §3：MQ 消费者放在拥有写入权的服务里）。
//
// 它只承担一条链路（docs/api-and-events.md §5）：
//
//	live.state.v1（生产者 live-ingest）-> logic.ReportStreamState -> 房间/场次投影
//
// 为什么本包可以很薄：事件消费最难的两件事（按 event_id 去重、按 stream_seq 挡乱序）
// 已经在 logic.ReportStreamState 里落库完成（live_room_idempotency + AdvanceStreamSeqTx），
// 所以这里只做「信封 -> RPC 入参」的翻译和「结果 -> 是否提交位点」的判定，
// 不复制一份去重状态机，也不新增消费位点表。
//
// 依赖边界：本包不 import live-ingest 的任何包（AGENTS.md §5 禁止跨服务直连别人的 model），
// 事件契约以本文件里的 StreamStatePayload 为准，并由 mapping_test.go 与
// services/live-ingest/internal/logic/streamstate.go 的 payload 字段做双向钉死。
// Kafka 客户端只出现在 kafkaruntime_kafka.go（-tags liveroom_kafka），
// 默认构建由 kafkaruntime_disabled.go 显式声明运行时未链接。
package consumer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/common/eventenvelope"
	"go-video/common/timeutil"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

// 本服务消费的事件契约（与 live-ingest 的 model/outbox.go 取值一致）。
// 复制常量而不是 import：跨服务 import 对方 internal/model 违反 AGENTS.md §5。
const (
	EventTypeStreamState = "live.state"
	// SchemaVersionStreamState 当前唯一被翻译的版本。上游递增版本号时必须同时改这里
	// 并补映射用例，否则事件会被判成 ErrUnsupportedSchemaVersion 而丢掉（不猜字段）。
	SchemaVersionStreamState = 1
)

// SupportedTopic 本服务唯一能消费的 topic，由信封规则推导。
// mapping_test.go 钉住它必须等于 "live.state.v1"：改版本号会让订阅串和实际 topic 静默错位。
var SupportedTopic = eventenvelope.Topic(EventTypeStreamState, SchemaVersionStreamState)

// maxEventIDBytes 对齐 live_room_idempotency.dedup_key 的列宽，
// 与 internal/logic/helpers.go:31 的 maxDedupIDBytes 同值。
// 超长的 event_id 进 logic 只会撞 "Data too long"，属于依赖故障而不是契约错误，
// 因此必须在消费侧按永久错误挡掉（见 handler.go 的重试判定）。
const maxEventIDBytes = 64

// 信封或 payload 不满足契约时的错误。全部按「永久」处理：重投不会让它变合法。
var (
	// ErrNilEvent 信封为空。
	ErrNilEvent = errors.New("live-room/consumer: nil event")
	// ErrEmptyPayload payload 缺失或为空。
	ErrEmptyPayload = errors.New("live-room/consumer: empty payload")
	// ErrUnsupportedEventType 事件类型不在本服务的消费契约内。
	ErrUnsupportedEventType = errors.New("live-room/consumer: unsupported event_type")
	// ErrUnsupportedSchemaVersion 类型对但 schema_version 不是本包已翻译的版本。
	ErrUnsupportedSchemaVersion = errors.New("live-room/consumer: unsupported schema_version")
	// ErrInvalidEvent 信封字段无法翻译成合法 RPC 入参（缺 room_id、未知流状态、seq<=0 等）。
	ErrInvalidEvent = errors.New("live-room/consumer: invalid stream state event")
)

// StreamStatePayload 是 live.state.v1 的业务负载，逐字段对齐生产者
// services/live-ingest/internal/logic/streamstate.go 的 stateEventPayload。
//
// 只声明用到的字段，未声明的字段在反序列化时被忽略，因此上游即使误投递
// 密钥、客户端 IP 等敏感字段也不会进入本服务的日志与状态流水（AGENTS.md §6、§7）。
type StreamStatePayload struct {
	StreamID           string `json:"stream_id"`
	RoomID             int64  `json:"room_id"`
	AnchorMid          int64  `json:"anchor_mid"`
	SessionID          int64  `json:"session_id"`
	StreamState        int32  `json:"stream_state"`
	StreamSeq          int64  `json:"stream_seq"`
	OccurredAt         int64  `json:"occurred_at"`
	InterruptedSeconds int64  `json:"interrupted_seconds"`
	Reason             string `json:"reason"`
	TraceID            string `json:"trace_id"`
}

// ParseEnvelope 反序列化并让 eventenvelope 自校验（缺 producer、event_type 非法等
// 都会在 UnmarshalJSON 阶段报错，不留到业务层）。
func ParseEnvelope(raw []byte) (*eventenvelope.Envelope, error) {
	if len(raw) == 0 {
		return nil, ErrEmptyPayload
	}
	var env eventenvelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("live-room/consumer: %w", err)
	}
	return &env, nil
}

// DecodePayload 解析流状态负载。空 payload 是永久错误：没有任何字段可翻译，重投也不会变好。
func DecodePayload(env *eventenvelope.Envelope) (*StreamStatePayload, error) {
	if env == nil {
		return nil, ErrNilEvent
	}
	if len(env.Payload) == 0 || string(env.Payload) == "{}" {
		return nil, ErrEmptyPayload
	}
	var p StreamStatePayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return nil, fmt.Errorf("live-room/consumer: live.state payload 解析失败 event_id=%s: %w",
			env.EventID, err)
	}
	return &p, nil
}

// Translate 把一条信封翻译成 ReportStreamState 的入参。
//
// 为什么在进 logic 之前还要自己判一遍非法：ReportStreamState 占用 event_id 去重键发生在
// 它的第一次读之前（reportstreamstatelogic.go:67 claimDedup → :76 FindOne）。
// 一条注定不可能被应用的事件只要进了它，这个 event_id 就永久作废，
// 之后同 event_id 的重投全部落进 result=2 重复分支，投影再也不会前进。
// 因此凡是本包能判定的非法（类型、版本、room_id、流状态、seq、标识长度）都在这里拦住。
func Translate(env *eventenvelope.Envelope) (*rpc.ReportStreamStateReq, error) {
	if env == nil {
		return nil, ErrNilEvent
	}
	if env.EventType != EventTypeStreamState {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedEventType, env.EventType)
	}
	if env.SchemaVersion != SchemaVersionStreamState {
		return nil, fmt.Errorf("%w: %d，本服务只翻译 v%d",
			ErrUnsupportedSchemaVersion, env.SchemaVersion, SchemaVersionStreamState)
	}
	eventID := strings.TrimSpace(env.EventID)
	if eventID == "" {
		return nil, fmt.Errorf("%w: event_id 为空", ErrInvalidEvent)
	}
	if len(eventID) > maxEventIDBytes {
		return nil, fmt.Errorf("%w: event_id %d 字节，超过 dedup_key 列宽 %d",
			ErrInvalidEvent, len(eventID), maxEventIDBytes)
	}

	p, err := DecodePayload(env)
	if err != nil {
		return nil, err
	}
	if p.RoomID <= 0 {
		return nil, fmt.Errorf("%w: room_id=%d，事件没有可路由的房间", ErrInvalidEvent, p.RoomID)
	}
	if !model.ValidStreamState(p.StreamState) {
		return nil, fmt.Errorf("%w: stream_state=%d 不在 1..4", ErrInvalidEvent, p.StreamState)
	}
	if p.StreamSeq <= 0 {
		return nil, fmt.Errorf("%w: stream_seq=%d，没有序号的事件无法参与乱序守卫",
			ErrInvalidEvent, p.StreamSeq)
	}
	if p.InterruptedSeconds < 0 {
		return nil, fmt.Errorf("%w: interrupted_seconds=%d 为负", ErrInvalidEvent, p.InterruptedSeconds)
	}

	// stream_id 缺省时回退信封的 aggregate_id：生产者的聚合根就是 stream_id
	// （live-ingest 的 model.AggregateTypeStream），两处不一致时以 payload 为准。
	streamID := strings.TrimSpace(p.StreamID)
	if streamID == "" {
		streamID = strings.TrimSpace(env.AggregateID)
	}
	// occurred_at 为负是契约违反，必须在这里拒绝：logic 也会拒（ErrStreamStateInvalid），
	// 但它拒之前已经占用了 event_id 去重键。
	if p.OccurredAt < 0 {
		return nil, fmt.Errorf("%w: occurred_at=%d 为负", ErrInvalidEvent, p.OccurredAt)
	}
	// occurred_at 缺省（0）时回退信封时间：payload 里的才是状态真正发生的时刻，
	// 信封的 occurred_at 是事件产出时刻，两者在实时链路上通常相差几百毫秒。
	occurredAt := p.OccurredAt
	if occurredAt == 0 {
		occurredAt = occurredAtSeconds(env.OccurredAt)
	}
	if occurredAt <= 0 {
		return nil, fmt.Errorf("%w: occurred_at 无法确定（payload 未给且信封时间不可解析）", ErrInvalidEvent)
	}
	traceID := strings.TrimSpace(p.TraceID)
	if traceID == "" {
		traceID = strings.TrimSpace(env.TraceID)
	}

	return &rpc.ReportStreamStateReq{
		EventId:            eventID,
		RoomId:             p.RoomID,
		SessionId:          p.SessionID,
		StreamId:           streamID,
		StreamState:        p.StreamState,
		StreamSeq:          p.StreamSeq,
		OccurredAt:         occurredAt,
		InterruptedSeconds: p.InterruptedSeconds,
		Reason:             strings.TrimSpace(p.Reason),
		TraceId:            traceID,
	}, nil
}

// Classify 把 ReportStreamState 的 result 翻译成消费结论。
//
// 1..5 全是「这条事件不用再投」的终态（logic 已把丢弃原因区分清楚），
// 未知 result 按重试处理：本包不认识的结果说明契约变了，绝不能当成成功提交位点后丢掉。
func Classify(result int32) Outcome {
	switch result {
	case model.StreamResultApplied:
		return OutcomeApplied
	case model.StreamResultDuplicate:
		return OutcomeDuplicate
	case model.StreamResultStale:
		return OutcomeStale
	case model.StreamResultIllegalTransition:
		return OutcomeIllegal
	case model.StreamResultMismatch:
		return OutcomeMismatch
	default:
		return OutcomeUnexpected
	}
}

// Outcome 一条投递的处理结论。Commit 决定队列是否提交位点。
type Outcome int

const (
	// OutcomeApplied 投影已按事件推进。
	OutcomeApplied Outcome = iota
	// OutcomeDuplicate 同一 event_id 已处理过（重复投递）。
	OutcomeDuplicate
	// OutcomeStale seq 落后于已应用序号，事件被丢弃。
	OutcomeStale
	// OutcomeIllegal 非法迁移，未产生写入。
	OutcomeIllegal
	// OutcomeMismatch 房间/场次/流引用不匹配。
	OutcomeMismatch
	// OutcomeSkipped 不是本服务的事件或契约非法：不投 logic，直接确认位点。
	OutcomeSkipped
	// OutcomeRetry 依赖故障且未达尝试上限：返回错误，不提交位点。
	OutcomeRetry
	// OutcomeGivenUp 依赖故障且已达尝试上限：放弃重投，写错误日志后确认位点。
	OutcomeGivenUp
	// OutcomeUnexpected logic 返回了本包不认识的 result，按可重试处理。
	OutcomeUnexpected
)

// Commit 表示本条投递是否可以推进消费位点。
// 只有 OutcomeRetry 不提交（其余终态与永久错误都提交，避免毒消息阻塞分区）。
func (o Outcome) Commit() bool { return o != OutcomeRetry }

func (o Outcome) String() string {
	switch o {
	case OutcomeApplied:
		return "applied"
	case OutcomeDuplicate:
		return "duplicate"
	case OutcomeStale:
		return "stale"
	case OutcomeIllegal:
		return "illegal_transition"
	case OutcomeMismatch:
		return "mismatch"
	case OutcomeSkipped:
		return "skipped"
	case OutcomeRetry:
		return "retry"
	case OutcomeGivenUp:
		return "given_up"
	case OutcomeUnexpected:
		return "unexpected_result"
	default:
		return "outcome_" + strconv.Itoa(int(o))
	}
}

// occurredAtSeconds 把信封的 RFC3339 时间换成 Unix 秒；解析失败返回 0（由调用方判非法）。
func occurredAtSeconds(s string) int64 {
	t, err := timeutil.ParseRFC3339(s)
	if err != nil {
		return 0
	}
	return t.Unix()
}
