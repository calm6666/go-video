// outbox_store.go 把 model.EventOutboxModel（live_ingest_outbox 表）映射成发布器的 Store。
//
// 这一层是唯一知道「本服务产出哪些事件」的地方：topic 由行上的 event_type +
// schema_version 现场拼出，而不是从配置里读字符串，因此改 schema 版本不会出现
// 「列写 v2、topic 还写 v1」的漂移。
package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go-video/common/eventenvelope"
	"go-video/services/live-ingest/model"
)

// RequiredTopic 返回本服务唯一产出的事件 topic（当前为 live.state.v1）。
// 它由 model 的常量拼出，配置校验与单测都以它为锚点，不在任何地方重复字面量。
func RequiredTopic() string {
	return eventenvelope.Topic(model.EventTypeStreamState, model.SchemaVersionStreamState)
}

// OutboxStore 是 Store 的生产实现，逐行做三件事：
//  1. topic 派生：event_type + schema_version；
//  2. 分区键：aggregate_id（即 stream_id），保证同流事件落同一分区；
//  3. 一致性反查：payload 里的信封必须与行上的列同源，否则标记为不可发布。
//
// 第 3 条不是多余的防御：logic 的 buildStateEnvelopePayload 要求 event_id 同时是
// live_stream_event.event_id、outbox.event_id 与断流记录的 start/end 引用
// （见 internal/logic/streamstate.go）。三处一旦写歪，消费方会按「另一个事件」去重，
// 顺序与幂等同时失效。发布前用真信封反查一次，比让四个下游各自判死便宜得多。
type OutboxStore struct {
	outbox model.EventOutboxModel
}

// NewOutboxStore 构造 Store。outbox 为 nil 时直接失败，不返回「扫不到任何行」的空实现。
func NewOutboxStore(outbox model.EventOutboxModel) (*OutboxStore, error) {
	if outbox == nil {
		return nil, errors.New("live-ingest/publisher: outbox model is required")
	}
	return &OutboxStore{outbox: outbox}, nil
}

// ListPending 读取到期事件并映射成 Record。
// 排序、state 过滤与 next_retry_at 判定都留在 SQL 里（model.ListPending），
// 这里不重排：id 升序是同流顺序的前提。
func (s *OutboxStore) ListPending(ctx context.Context, now int64, limit int32) ([]*Record, error) {
	rows, err := s.outbox.ListPending(ctx, now, limit)
	if err != nil {
		return nil, fmt.Errorf("live-ingest/publisher: 读取 live_ingest_outbox: %w", err)
	}
	records := make([]*Record, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		records = append(records, toRecord(row))
	}
	return records, nil
}

// MarkPublished 转调 model：mtime 即发布时间（本表不另存 published_at）。
func (s *OutboxStore) MarkPublished(ctx context.Context, id, publishedAt int64) error {
	return s.outbox.MarkPublished(ctx, id, publishedAt)
}

// MarkRetry 转调 model，行保持待发布状态并带上退避到期时间。
func (s *OutboxStore) MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	return s.outbox.MarkRetry(ctx, id, retryCount, nextRetryAt, lastError)
}

// MarkFailed 转调 model：判死后只能由 RetryFailedEvents RPC 放行。
func (s *OutboxStore) MarkFailed(ctx context.Context, id int64, lastError string) error {
	return s.outbox.MarkFailed(ctx, id, lastError)
}

// toRecord 完成列 → Record 的映射与一致性反查。
// 发现缺陷不返回错误：那一行仍要落库给出结论（判死），错误留给真正的读库失败。
func toRecord(row *model.EventOutbox) *Record {
	rec := &Record{
		ID:         row.ID,
		EventID:    row.EventID,
		Topic:      eventenvelope.Topic(row.EventType, int(row.SchemaVersion)),
		Key:        row.AggregateID,
		Payload:    row.Payload,
		RetryCount: row.RetryCount,
	}
	required := RequiredTopic()
	if rec.Topic != required {
		rec.Defect = fmt.Sprintf("topic %q 不属于本服务（本表只产出 %s）", rec.Topic, required)
		return rec
	}
	if rec.Key == "" {
		rec.Defect = "aggregate_id 为空，无法用分区键保证同流顺序"
		return rec
	}
	// Envelope.UnmarshalJSON 自带 Validate：残缺信封在这里就拦下，不投给下游判死。
	var env eventenvelope.Envelope
	if err := json.Unmarshal([]byte(row.Payload), &env); err != nil {
		rec.Defect = "payload 不是合法事件信封: " + err.Error()
		return rec
	}
	if env.EventID != row.EventID {
		rec.Defect = fmt.Sprintf("payload event_id=%q 与列 event_id=%q 不一致", env.EventID, row.EventID)
		return rec
	}
	if env.AggregateID != row.AggregateID {
		rec.Defect = fmt.Sprintf("payload aggregate_id=%q 与列 aggregate_id=%q 不一致", env.AggregateID, row.AggregateID)
		return rec
	}
	return rec
}
