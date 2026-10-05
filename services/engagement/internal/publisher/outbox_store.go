// outbox_store.go 是 engagement_outbox 到 outbox.Store 的适配器。
//
// 与 video 的同名文件同构，差异只在两处：本表的聚合根是「被互动的对象」（aggregate_id
// 为 content_id 的十进制串），以及本表没有 published_at 列（mtime 即发布时间）。
// 判据本身复用 outbox.CheckRow：五条反查在每张 outbox 表上都一样，抄第二份必然漏一条。
package publisher

import (
	"context"
	"errors"
	"fmt"

	"go-video/common/eventenvelope"
	"go-video/common/outbox"
	"go-video/services/engagement/model"
)

// 复用通用引擎：本包不维护第二份状态机。
type (
	// Publisher 是 engagement_outbox 的发布循环（= common/outbox 的引擎）。
	Publisher = outbox.Publisher
	// Sender 是一次事件投递的抽象，真实实现按构建标签提供。
	Sender = outbox.Sender
	// Options 是发布循环参数。
	Options = outbox.Options
)

// label 是日志与错误信息里的组件标签，运维据此分辨是哪个服务的循环。
const label = "engagement/publisher"

// RequiredTopic 返回本服务唯一产出的事件 topic（当前为 engagement.action.v1）。
// 它由 model 的 event_type + schema_version 常量拼出，配置校验与单测都以它为锚点，
// 不在任何 yaml 或字面量里重复一遍。
func RequiredTopic() string {
	return eventenvelope.Topic(model.EventEngagementAction, model.EventSchemaVersion)
}

// RequiredTopics 是 RequiredTopic 的集合形态，供 CheckRow 与配置校验使用。
func RequiredTopics() []string { return []string{RequiredTopic()} }

// OutboxStore 是 outbox.Store 的 engagement 实现：列映射加一致性反查。
//
// 反查不是多余的防御：Like/AddFav/DelFav/AddShare 写的事件六个业务列全部取自同一个 env
// （repository.buildActionEvent），一旦将来有人改成部分列来自请求参数，
// 「列的 event_id」与「payload 的 event_id」就会分裂，
// 消费方按其中一个去重，等于把另一个当新事件重复生效（同一个赞被索引计两次）。
type OutboxStore struct {
	outboxMd model.EngagementOutboxModel
}

// NewOutboxStore 构造 Store。model 为 nil 时直接失败，不返回「扫不到任何行」的空实现。
func NewOutboxStore(outboxMd model.EngagementOutboxModel) (*OutboxStore, error) {
	if outboxMd == nil {
		return nil, errors.New(label + ": outbox model is required")
	}
	return &OutboxStore{outboxMd: outboxMd}, nil
}

// ListPending 读取到期事件并映射成 outbox.Row。
// 排序、state 过滤与 next_retry_at 判定都留在 SQL 里（model.ListPending 只取 state=0），
// 这里不重排：id 升序是同一对象「先点赞后取消点赞」这类相邻事件顺序的前提。
func (s *OutboxStore) ListPending(ctx context.Context, now int64, limit int32) ([]*outbox.Row, error) {
	rows, err := s.outboxMd.ListPending(ctx, now, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: 读取 engagement_outbox: %w", label, err)
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

// MarkPublished 转调 model：engagement_outbox 不另存 published_at，mtime 即发布时间。
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
func toRow(row *model.EngagementOutbox) *outbox.Row {
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
