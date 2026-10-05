package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// outboxColumns 是 video_outbox 的列清单，必须与
// deploy/migrations/video/000004_create_video_outbox.sql 完全一致。
const outboxColumns = "id, event_id, event_type, schema_version, aggregate_type, aggregate_id, " +
	"payload, state, retry_count, next_retry_at, last_error, occurred_at, ctime, mtime"

// VideoOutbox 领域事件 Outbox（video_outbox 表）。
// 遵循 AGENTS.md §5：TransitionState 的「稿件状态推进 + 审计行 + 事件行」在同一事务内提交，
// 由 internal/publisher 按 id 升序投递到 MQ
// （Topic = event_type + ".v" + schema_version，见 docs/api-and-events.md §4/§5），
// 消费者按 event_id 幂等去重。
// payload 存 common/eventenvelope.Envelope 的完整 JSON，事件字段以信封为准。
type VideoOutbox struct {
	// ID 自增主键（发布器按此升序保证顺序）
	ID int64 `db:"id"`
	// EventID 事件唯一 ID（ULID，唯一索引，消费者据此幂等）
	EventID string `db:"event_id"`
	// EventType 事件类型（content.published）
	EventType string `db:"event_type"`
	// SchemaVersion 事件 schema 版本
	SchemaVersion int32 `db:"schema_version"`
	// AggregateType 聚合根类型（submission）
	AggregateType string `db:"aggregate_type"`
	// AggregateID 聚合根 ID（aid 的十进制字符串，同时是分区键）
	AggregateID string `db:"aggregate_id"`
	// Payload 事件信封完整 JSON（eventenvelope.Envelope）
	Payload string `db:"payload"`
	// State 发布状态：0 待发布、1 已发布、2 失败（超过最大重试，人工处理）
	State int32 `db:"state"`
	// RetryCount 已重试次数
	RetryCount int32 `db:"retry_count"`
	// NextRetryAt 下次重试时间（Unix 秒，0 表示可立即投递）
	NextRetryAt int64 `db:"next_retry_at"`
	// LastError 最近一次投递错误
	LastError string `db:"last_error"`
	// OccurredAt 事件发生时间（Unix 秒，与信封 occurred_at 对应）
	OccurredAt int64 `db:"occurred_at"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 修改时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// VideoOutboxModel video_outbox 表查询与写入接口。
type VideoOutboxModel interface {
	// Insert 在业务事务内写入事件（tx 为空时退化为自动提交）。
	Insert(ctx context.Context, tx sqlx.Session, out *VideoOutbox) error
	// ListPending 查询到期可投递事件（按 id 升序）。
	ListPending(ctx context.Context, now int64, limit int32) ([]*VideoOutbox, error)
	// MarkPublished 标记已发布。
	MarkPublished(ctx context.Context, id, publishedAt int64) error
	// MarkRetry 记录失败并设置下次重试时间（指数退避由调用方计算）。
	MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error
	// MarkFailed 超过最大重试后标记失败（人工处理）。
	MarkFailed(ctx context.Context, id int64, lastError string) error
}

type defaultVideoOutboxModel struct {
	conn sqlx.SqlConn
}

// NewVideoOutboxModel 创建 VideoOutboxModel 实现。
func NewVideoOutboxModel(conn sqlx.SqlConn) VideoOutboxModel {
	return &defaultVideoOutboxModel{conn: conn}
}

func (m *defaultVideoOutboxModel) Insert(ctx context.Context, tx sqlx.Session, out *VideoOutbox) error {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	if out.Ctime == 0 {
		out.Ctime = nowUnix()
	}
	out.Mtime = out.Ctime
	if out.OccurredAt == 0 {
		out.OccurredAt = out.Ctime
	}
	_, err := session.ExecCtx(ctx,
		"INSERT INTO video_outbox (event_id, event_type, schema_version, aggregate_type, aggregate_id, "+
			"payload, state, retry_count, next_retry_at, last_error, occurred_at, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		out.EventID, out.EventType, out.SchemaVersion, out.AggregateType, out.AggregateID,
		out.Payload, out.State, out.RetryCount, out.NextRetryAt, out.LastError,
		out.OccurredAt, out.Ctime, out.Mtime)
	if err != nil {
		return fmt.Errorf("video_outbox Insert: %w", err)
	}
	return nil
}

func (m *defaultVideoOutboxModel) ListPending(ctx context.Context, now int64, limit int32) ([]*VideoOutbox, error) {
	query := "SELECT " + outboxColumns + " FROM video_outbox " +
		"WHERE state = 0 AND (next_retry_at = 0 OR next_retry_at <= ?) ORDER BY id ASC LIMIT ?"
	var rows []*VideoOutbox
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("video_outbox ListPending: %w", err)
	}
	return rows, nil
}

func (m *defaultVideoOutboxModel) MarkPublished(ctx context.Context, id, publishedAt int64) error {
	// publishedAt 写入 mtime：本表不额外保存发布时间列，mtime 即最后一次状态变更时间。
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE video_outbox SET state = ?, last_error = '', mtime = ? WHERE id = ?",
		OutboxStatePublished, publishedAt, id)
	if err != nil {
		return fmt.Errorf("video_outbox MarkPublished: %w", err)
	}
	return nil
}

func (m *defaultVideoOutboxModel) MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE video_outbox SET state = ?, retry_count = ?, next_retry_at = ?, last_error = ?, mtime = ? WHERE id = ?",
		OutboxStatePending, retryCount, nextRetryAt, lastError, nowUnix(), id)
	if err != nil {
		return fmt.Errorf("video_outbox MarkRetry: %w", err)
	}
	return nil
}

func (m *defaultVideoOutboxModel) MarkFailed(ctx context.Context, id int64, lastError string) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE video_outbox SET state = ?, last_error = ?, mtime = ? WHERE id = ?",
		OutboxStateFailed, lastError, nowUnix(), id)
	if err != nil {
		return fmt.Errorf("video_outbox MarkFailed: %w", err)
	}
	return nil
}
