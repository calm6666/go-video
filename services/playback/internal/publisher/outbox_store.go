// Package publisher 把 playback_outbox 里「已在业务事务内提交」的事件投递到消息队列。
//
// 分层与 live-ingest 的同类包一致，但发布循环本身不再重写：
// 顺序、退避、判死与写库失败中断这些与业务无关的决策全在 go-video/common/outbox，
// 本包只回答 playback 的三个问题：
//  1. 这张表的行怎么映射成 outbox.Row（topic 由 event_type + schema_version 派生）；
//  2. 哪些 topic 是本服务真实产出的（RequiredTopic，锚在 model 常量上）；
//  3. 真实队列客户端怎么建（只在 kafkaruntime_kafka.go，`-tags playback_kafka`）。
//
// 证据边界（AGENTS.md §9）：本仓库从未连接过任何 broker。这里能给的结论只到
// 「可编译 / 可静态检查 / 本包单测通过」；「事件真的送达」必须先在 Redpanda 上跑通才算数。
package publisher

import (
	"context"
	"errors"
	"fmt"

	"go-video/common/eventenvelope"
	"go-video/common/outbox"
	"go-video/services/playback/model"
)

// 复用通用引擎：本包不维护第二份状态机。
type (
	// Publisher 是 playback_outbox 的发布循环（= common/outbox 的引擎）。
	Publisher = outbox.Publisher
	// Sender 是一次事件投递的抽象，真实实现按构建标签提供。
	Sender = outbox.Sender
	// Options 是发布循环参数。
	Options = outbox.Options
)

// label 是日志与错误信息里的组件标签，运维据此分辨是哪个服务的循环。
const label = "playback/publisher"

// RequiredTopic 返回本服务唯一产出的事件 topic（当前为 playback.heartbeat.v1）。
// 它由 model 的 event_type + schema_version 常量拼出，配置校验与单测都以它为锚点，
// 不在任何 yaml 或字面量里重复一遍。
func RequiredTopic() string {
	return eventenvelope.Topic(model.EventPlaybackHeartbeat, model.EventSchemaVersion)
}

// RequiredTopics 是 RequiredTopic 的集合形态，供 CheckRow 与配置校验使用。
func RequiredTopics() []string { return []string{RequiredTopic()} }

// OutboxStore 是 outbox.Store 的 playback 实现：列映射加一致性反查。
//
// 反查不是多余的防御：ReportHeartbeat 的六个列全部取自同一个 env
// （repository.ReportHeartbeat），一旦将来有人改成部分列来自请求参数，
// 「列的 event_id」与「payload 的 event_id」就会分裂，
// 消费方按其中一个去重，等于把另一个当新事件重复生效。
type OutboxStore struct {
	outboxMd model.PlaybackOutboxModel
}

// NewOutboxStore 构造 Store。model 为 nil 时直接失败，不返回「扫不到任何行」的空实现。
func NewOutboxStore(outboxMd model.PlaybackOutboxModel) (*OutboxStore, error) {
	if outboxMd == nil {
		return nil, errors.New(label + ": outbox model is required")
	}
	return &OutboxStore{outboxMd: outboxMd}, nil
}

// ListPending 读取到期事件并映射成 outbox.Row。
// 排序、state 过滤与 next_retry_at 判定都留在 SQL 里（model.ListPending 只取 state=0），
// 这里不重排：id 升序是同会话事件顺序的前提。
func (s *OutboxStore) ListPending(ctx context.Context, now int64, limit int32) ([]*outbox.Row, error) {
	rows, err := s.outboxMd.ListPending(ctx, now, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: 读取 playback_outbox: %w", label, err)
	}
	records := make([]*outbox.Row, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		records = append(records, toRow(row))
	}
	return records, nil
}

// MarkPublished 转调 model：playback_outbox 不另存 published_at，mtime 即发布时间。
func (s *OutboxStore) MarkPublished(ctx context.Context, id, publishedAt int64) error {
	return s.outboxMd.MarkPublished(ctx, id, publishedAt)
}

// MarkRetry 转调 model，行保持待发布状态并带上退避到期时间。
func (s *OutboxStore) MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	return s.outboxMd.MarkRetry(ctx, id, retryCount, nextRetryAt, lastError)
}

// MarkFailed 转调 model：判死后 state=2，本服务没有人工放行接口（见 README「已知缺口」）。
func (s *OutboxStore) MarkFailed(ctx context.Context, id int64, lastError string) error {
	return s.outboxMd.MarkFailed(ctx, id, lastError)
}

// toRow 完成列 → outbox.Row 的映射与一致性反查。
// 发现缺陷不返回错误：那一行仍要落库给出结论（判死），错误留给真正的读库失败。
func toRow(row *model.PlaybackOutbox) *outbox.Row {
	rec := &outbox.Row{
		ID:         row.ID,
		EventID:    row.EventID,
		Topic:      eventenvelope.Topic(row.EventType, int(row.SchemaVersion)),
		Key:        row.AggregateID,
		Payload:    row.Payload,
		RetryCount: row.RetryCount,
	}
	rec.Defect = outbox.CheckRow(rec, RequiredTopics())
	return rec
}
