// Package consumer 是 live-media 的事件消费侧（AGENTS.md §3：MQ 消费者放在拥有写入权的服务里）。
//
// 它只承担一条链路（docs/api-and-events.md §5）：
//
//	live.state.v1（生产者 live-ingest）-> logic.OfflineSessionOutputs -> 本场次档位整场下线
//
// 语义：主播停止推流（stream_state=Stopped）后，这一场次开出来的所有分发档位都不再有源可发，
// 必须由本服务把 live_stream_output 置为已下线，并为每个档位登记
// livemedia.stream.output.offline 事件（否则 live-gateway/playback 侧继续把死档位发给观众）。
//
// 为什么本包不需要消费位点表（与 inbox、search-indexer、notification 的差别）：
// 下线的幂等性由行本身保证（MarkOfflineTx 的 CAS 条件 state=在线），
// 同一事件重投第二次扫不到行、Affected=0，不会补第二条事件；
// 乱序防护由 live_session_id 维度保证（迟到的上一场 Stopped 只命中上一场的档位行）。
// 因此这里只做「信封 -> 内部命令」的翻译和「结果 -> 是否提交位点」的判定。
// 代价是失败事件没有持久化死信：达到尝试上限即记 given_up 错误日志并跨过位点，
// 人工重放入口是 admin 的 ListStreamOutputs + OfflineStreamOutput（逐档位），
// 见 README「已知缺口」。
//
// 依赖边界：本包不 import live-ingest 与 live-room 的任何包（AGENTS.md §5 禁止跨服务直连），
// 事件契约以本文件的 StreamStatePayload 与 streamState* 常量为准，
// 由 mapping_test.go 与 live-ingest 侧的 payload / 状态枚举逐字段、逐取值钉死。
// Kafka 客户端只出现在 kafkaruntime_kafka.go（-tags livemedia_kafka），
// 默认构建由 kafkaruntime_disabled.go 显式声明运行时未链接。
package consumer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/common/eventenvelope"
	"go-video/services/live-media/model"
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
// mapping_test.go 钉住它必须等于 "live.state.v1"：改版本号会让订阅串和实际 topic 静默错位，
// 表现是「消费者在跑、一条消息也收不到」。
var SupportedTopic = eventenvelope.Topic(EventTypeStreamState, SchemaVersionStreamState)

// 流状态取值：与 live-ingest/model/errors.go 的 StreamState* 一致（1..4），
// 也是 live-room/model/errors.go 的同一份值域。三份副本靠 mapping_test.go 的
// TestConsumerContractMatchesProducerSource 逐个取值比对生产者源码，任一侧重排都会红。
const (
	streamStateIdle        int32 = 1
	streamStatePublishing  int32 = 2
	streamStateInterrupted int32 = 3
	streamStateStopped     int32 = 4
)

// maxEventIDRunes 对齐本服务把 event_id 写进下线事件 payload 的长度口径，
// 与 internal/logic/helpers.go:35 的 maxEventIDRunes 同值。
// 超长的 event_id 进 logic 只会撞长度校验，属于契约违反而不是依赖故障，
// 必须在消费侧按永久错误挡掉（见 handler.go 的分支）。
const maxEventIDRunes = 64

// 信封或 payload 不满足契约时的错误。全部按「永久」处理：重投不会让它变合法。
var (
	// ErrNilEvent 信封为空。
	ErrNilEvent = errors.New("livemedia/consumer: nil event")
	// ErrEmptyPayload payload 缺失或为空。
	ErrEmptyPayload = errors.New("livemedia/consumer: empty payload")
	// ErrUnsupportedEventType 事件类型不在本服务的消费契约内。
	ErrUnsupportedEventType = errors.New("livemedia/consumer: unsupported event_type")
	// ErrUnsupportedSchemaVersion 类型对但 schema_version 不是本包已翻译的版本。
	ErrUnsupportedSchemaVersion = errors.New("livemedia/consumer: unsupported schema_version")
	// ErrInvalidEvent 信封字段无法翻译成合法下线命令（缺 room_id、未知流状态、缺 session_id 等）。
	ErrInvalidEvent = errors.New("livemedia/consumer: invalid stream state event")
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

// OfflineCommand 是一条投递翻译出来的内部命令，由 logic.OfflineSessionOutputs 执行
// （wiring.go 负责把它映射成 logic 的入参）。
//
// 刻意不带 occurred_at：下线时刻由 MarkOfflineTx 在服务端取时钟（与 OfflineStreamOutput 同口径），
// 把事件时间当成结论时刻会让同一条事件的两份结论不一致，也让「迟到五分钟的事件」倒写历史。
type OfflineCommand struct {
	// EventID 驱动这次下线的事件 ID，进下线事件 payload 做反向追溯。
	EventID string
	// RoomID 与 SessionID 一起界定下线范围；SessionID 恒为正（见 Interpret）。
	RoomID    int64
	SessionID int64
	// Reason 是 live_stream_output.offline_reason 的取值（本链路固定 SOURCE_LOST）。
	Reason int32
	// TraceID 关联链路，允许为空（生产者在内部触发时可能没有 trace）。
	TraceID string
}

// Event 是一条已解析并通过了值域校验的 live.state 事件。
// Command 只在 Decision 为 DecisionOffline 时非 nil：其余状态的日志与排障仍要能说出
// 「这条事件是哪个房间哪一场什么状态」，所以 payload 一律带回来。
type Event struct {
	Envelope *eventenvelope.Envelope
	Payload  StreamStatePayload
	// Command 交给 logic 的下线命令；nil 表示本服务对这条事件没有写入动作。
	Command *OfflineCommand
}

// Decision 一条合法事件「要不要本服务动手」。
type Decision int

const (
	// DecisionOffline 事件要求下线本场次全部在线档位（stream_state=Stopped）。
	DecisionOffline Decision = iota
	// DecisionNone 事件合法但本服务无需动作（未推流 / 推流中 / 中断）。
	DecisionNone
)

func (d Decision) String() string {
	if d == DecisionOffline {
		return "offline_session_outputs"
	}
	return "none"
}

// ParseEnvelope 反序列化并让 eventenvelope 自校验（缺 producer、event_type 非法等
// 都会在 UnmarshalJSON 阶段报错，不留到业务层）。
func ParseEnvelope(raw []byte) (*eventenvelope.Envelope, error) {
	if len(raw) == 0 {
		return nil, ErrEmptyPayload
	}
	var env eventenvelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("livemedia/consumer: %w", err)
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
		return nil, fmt.Errorf("livemedia/consumer: live.state payload 解析失败 event_id=%s: %w",
			env.EventID, err)
	}
	return &p, nil
}

// Interpret 把一条信封翻译成「本服务该不该动手、动手做什么」。
//
// 判定顺序刻意先校验后决策：非法事件必须在这里就报错，不能退化成 DecisionNone。
// 区分这两者的意义是「上游写坏了契约」和「上游发了与本服务无关的状态」是两种排障路径，
// 前者要告警到生产者侧（live-ingest），后者只是正常噪声。
//
// 不变式：返回非 nil error 时事件对象一定为 nil。handler.go 拿到错误只写日志，
// 但把「半个事件」交回调用方等于邀请它忽略错误继续走，而这条链路的动作是整场档位下线。
//
// DecisionNone 的三种状态各自为什么不动作：
//   - Idle(1)：这一场从没推起来，档位表里不该有它的在线行（有则由到期清扫兜底）；
//   - Publishing(2)：正在推流，下线等于自己把直播掐了；
//   - Interrupted(3)：短断可在宽限期内重连续推，此时摘全房间档位会让一次网络抖动
//     变成观众可见的整场停播；真正收敛的结论是后续的 Stopped。
//
// 为什么 session_id 缺失是「拒绝」而不是「按房间兜底下线」：按房间下线会连带摘掉
// 刚开播场次的分发，一条迟到的旧事件就能把新直播打死，这个误伤比档位多在线一会儿严重得多。
func Interpret(env *eventenvelope.Envelope) (*Event, Decision, error) {
	if env == nil {
		return nil, DecisionNone, ErrNilEvent
	}
	if env.EventType != EventTypeStreamState {
		return nil, DecisionNone, fmt.Errorf("%w: %s", ErrUnsupportedEventType, env.EventType)
	}
	if env.SchemaVersion != SchemaVersionStreamState {
		return nil, DecisionNone, fmt.Errorf("%w: %d，本服务只翻译 v%d",
			ErrUnsupportedSchemaVersion, env.SchemaVersion, SchemaVersionStreamState)
	}
	eventID := strings.TrimSpace(env.EventID)
	if eventID == "" {
		return nil, DecisionNone, fmt.Errorf("%w: event_id 为空", ErrInvalidEvent)
	}
	if len([]rune(eventID)) > maxEventIDRunes {
		return nil, DecisionNone, fmt.Errorf("%w: event_id %d 字符，超过本服务事件引用上限 %d",
			ErrInvalidEvent, len([]rune(eventID)), maxEventIDRunes)
	}

	p, err := DecodePayload(env)
	if err != nil {
		// 负载层面再套一层 ErrInvalidEvent：payload 不是对象、字段类型写错与缺 room_id
		// 是同一类结论（上游违反了 live.state.v1），调用方用一个 sentinel 就能判完，
		// 底层错误仍被 %w 保留（ErrEmptyPayload 依旧可单独 Is 出来）。
		return nil, DecisionNone, fmt.Errorf("%w: live.state 负载不可用: %w", ErrInvalidEvent, err)
	}
	if p.RoomID <= 0 {
		return nil, DecisionNone, fmt.Errorf("%w: room_id=%d，事件没有可路由的房间", ErrInvalidEvent, p.RoomID)
	}
	if !validStreamState(p.StreamState) {
		return nil, DecisionNone, fmt.Errorf("%w: stream_state=%d 不在 1..4", ErrInvalidEvent, p.StreamState)
	}
	if p.StreamSeq < 0 {
		return nil, DecisionNone, fmt.Errorf("%w: stream_seq=%d 为负", ErrInvalidEvent, p.StreamSeq)
	}
	if p.InterruptedSeconds < 0 {
		return nil, DecisionNone, fmt.Errorf("%w: interrupted_seconds=%d 为负", ErrInvalidEvent, p.InterruptedSeconds)
	}
	if p.OccurredAt < 0 {
		return nil, DecisionNone, fmt.Errorf("%w: occurred_at=%d 为负", ErrInvalidEvent, p.OccurredAt)
	}
	ev := &Event{Envelope: env, Payload: *p}
	if p.StreamState != streamStateStopped {
		return ev, DecisionNone, nil
	}
	if p.SessionID <= 0 {
		return nil, DecisionNone, fmt.Errorf("%w: stream_state=Stopped 但 session_id=%d，"+
			"没有场次维度无法界定下线范围（按房间下线会误伤刚开播的场次）", ErrInvalidEvent, p.SessionID)
	}

	traceID := strings.TrimSpace(p.TraceID)
	if traceID == "" {
		traceID = strings.TrimSpace(env.TraceID)
	}

	// 下线动作只需要四个坐标（房间、场次、原因、事件引用）；
	// seq/occurred_at/interrupted_seconds 留在 Event.Payload 里供日志与排障使用。
	ev.Command = &OfflineCommand{
		EventID:   eventID,
		RoomID:    p.RoomID,
		SessionID: p.SessionID,
		// 下线原因固定为 SOURCE_LOST：断流下线与「配置有效期到点」（MarkExpiredOffline 写 TIMEOUT）
		// 和「运营手工下线」（MANUAL）必须是三种可区分的结论，否则运营无法判断该不该重开档位。
		Reason:  model.ReasonSourceLost,
		TraceID: traceID,
	}
	return ev, DecisionOffline, nil
}

// validStreamState 判定值域。未知状态一律按非法（而不是「不动作」）处理：
// 上游新增状态时这里会红，逼着补翻译而不是把事件安静吃掉。
func validStreamState(state int32) bool {
	switch state {
	case streamStateIdle, streamStatePublishing, streamStateInterrupted, streamStateStopped:
		return true
	default:
		return false
	}
}

// stateLabel 把流状态编号换成日志里可读的名字（不参与任何判定）。
func stateLabel(state int32) string {
	switch state {
	case streamStateIdle:
		return "IDLE"
	case streamStatePublishing:
		return "PUBLISHING"
	case streamStateInterrupted:
		return "INTERRUPTED"
	case streamStateStopped:
		return "STOPPED"
	default:
		return "STATE_" + strconv.FormatInt(int64(state), 10)
	}
}
