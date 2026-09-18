// Package eventenvelope 定义 go-video 中领域事件的统一信封结构。
// 该结构对齐 docs/api-and-events.md §4，
// 保证生产者和消费者共享单一、版本化的契约。
//
// 生产者应通过 New 构造信封，将序列化后的 JSON 与业务事务一起
// 写入 outbox 表，由独立的发布器转发到消息队列。
// 消费者从队列读回信封后，应使用 Validate 在处理前拒绝格式错误的事件。
package eventenvelope

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"go-video/common/idgen"
	"go-video/common/timeutil"
)

// Envelope 是统一的领域事件包装结构。
type Envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    string          `json:"occurred_at"`
	Producer      string          `json:"producer"`
	TraceID       string          `json:"trace_id,omitempty"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	Payload       json.RawMessage `json:"payload"`
}

// New 构造完整填充的 Envelope。
// 会自动生成 event_id（ULID），用当前 UTC 时间填充 occurred_at，
// 并在返回前执行 Validate。若请求未携带 trace 上下文，传入空字符串即可。
func New(
	producer, eventType, aggregateType, aggregateID string,
	schemaVersion int,
	payload json.RawMessage,
	traceID string,
) (*Envelope, error) {
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	eventID, err := idgen.ULID()
	if err != nil {
		return nil, fmt.Errorf("eventenvelope: generate event_id: %w", err)
	}
	e := &Envelope{
		EventID:       eventID,
		EventType:     eventType,
		SchemaVersion: schemaVersion,
		OccurredAt:    timeutil.FormatRFC3339(timeutil.Now()),
		Producer:      producer,
		TraceID:       traceID,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Payload:       payload,
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}

// Validate 检查必填字段、schema_version 是否为正数，以及 payload 是否非 nil。
// 该方法不检查 payload 内容；内容校验由消费方按 schema_version 自行负责。
func (e *Envelope) Validate() error {
	if e == nil {
		return errors.New("eventenvelope: envelope is nil")
	}
	if e.EventID == "" {
		return errors.New("eventenvelope: event_id is required")
	}
	if e.EventType == "" {
		return errors.New("eventenvelope: event_type is required")
	}
	if !isValidEventType(e.EventType) {
		return fmt.Errorf("eventenvelope: event_type %q must be lowercase dot-separated", e.EventType)
	}
	if e.SchemaVersion <= 0 {
		return fmt.Errorf("eventenvelope: schema_version must be positive, got %d", e.SchemaVersion)
	}
	if e.OccurredAt == "" {
		return errors.New("eventenvelope: occurred_at is required")
	}
	if _, err := timeutil.ParseRFC3339(e.OccurredAt); err != nil {
		return fmt.Errorf("eventenvelope: occurred_at must be RFC3339: %w", err)
	}
	if e.Producer == "" {
		return errors.New("eventenvelope: producer is required")
	}
	if e.AggregateType == "" {
		return errors.New("eventenvelope: aggregate_type is required")
	}
	if e.AggregateID == "" {
		return errors.New("eventenvelope: aggregate_id is required")
	}
	if e.Payload == nil {
		return errors.New("eventenvelope: payload must not be nil")
	}
	return nil
}

// MarshalJSON 序列化信封。所有字段均显式声明 JSON tag，
// 故使用默认结构体编码；该方法主要用于在调用方绕过 New 时
// 于序列化阶段暴露校验错误。
func (e *Envelope) MarshalJSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	type alias Envelope
	return json.Marshal((*alias)(e))
}

// UnmarshalJSON 反序列化信封并执行校验。
// 格式错误或不完整的信封返回错误，使消费方可将其路由到死信队列，
// 而不是静默丢弃字段。
func (e *Envelope) UnmarshalJSON(b []byte) error {
	type alias Envelope
	tmp := &alias{}
	if err := json.Unmarshal(b, tmp); err != nil {
		return fmt.Errorf("eventenvelope: unmarshal: %w", err)
	}
	*e = Envelope(*tmp)
	return e.Validate()
}

// Topic 返回事件类型和 schema 版本对应的标准 topic 名：
// `producer.event.vN`。其中 producer 取自 eventType 的第一段
// （例如 "content.published" -> producer "content"）。
//
// 示例：Topic("content.published", 1) -> "content.published.v1"。
func Topic(eventType string, schemaVersion int) string {
	if eventType == "" || schemaVersion <= 0 {
		return ""
	}
	return fmt.Sprintf("%s.v%d", eventType, schemaVersion)
}

// isValidEventType 要求 event_type 仅包含小写字母、数字和点号；
// 不允许首尾出现点号，也不允许连续两个点号。
func isValidEventType(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLower(r) || unicode.IsDigit(r) || r == '.') {
			return false
		}
	}
	return true
}
