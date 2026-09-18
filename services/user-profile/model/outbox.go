package model

import (
	"context"
	"database/sql"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// MemberOutbox 对应数据库 member_outbox 表，记录 user-profile 的领域事件。
// 遵循 AGENTS.md §5：业务写操作与 Outbox 记录在同一事务内提交，
// 由独立发布器按序投递，消费者按 event_id 幂等。
type MemberOutbox struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// EventID 事件唯一 ID（ULID，唯一索引）
	EventID string `db:"event_id"`
	// EventType 事件类型（user.profile_updated / user.moral_notice）
	EventType string `db:"event_type"`
	// AggregateID 聚合根 ID（mid 十进制字符串）
	AggregateID string `db:"aggregate_id"`
	// Payload 事件信封完整 JSON（common/eventenvelope.Envelope）
	Payload string `db:"payload"`
	// Status 发布状态：0 待发布、1 已发布、2 失败（超最大重试）
	Status int8 `db:"status"`
	// Attempts 已投递次数
	Attempts int32 `db:"attempts"`
	// NextRetryAt 下次重试时间（Unix 秒；0 表示可立即投递）
	NextRetryAt int64 `db:"next_retry_at"`
	// LastError 最近一次投递错误
	LastError string `db:"last_error"`
	// CreatedAt 创建时间（Unix 秒）
	CreatedAt int64 `db:"created_at"`
	// PublishedAt 发布时间（Unix 秒；未发布为 0）
	PublishedAt int64 `db:"published_at"`
}

// MemberOutboxModel 抽象 member_outbox 表的查询接口。
type MemberOutboxModel interface {
	// Insert 写入事件（在业务事务内调用）。
	Insert(ctx context.Context, tx sqlx.Session, out *MemberOutbox) error
	// ListPending 查询到期可投递的事件（按 ID 升序，保证发布顺序）。
	ListPending(ctx context.Context, now int64, limit int) ([]*MemberOutbox, error)
	// MarkPublished 标记已发布。
	MarkPublished(ctx context.Context, id int64, publishedAt int64) error
	// MarkRetry 记录失败并设置下次重试时间（退避）。
	MarkRetry(ctx context.Context, id int64, attempts int32, nextRetryAt int64, lastError string) error
	// MarkFailed 超过最大重试次数后标记失败（人工处理）。
	MarkFailed(ctx context.Context, id int64, lastError string) error
}

type defaultMemberOutboxModel struct {
	conn sqlx.SqlConn
}

// NewMemberOutboxModel 创建基于 sqlx 的 MemberOutboxModel 实现。
func NewMemberOutboxModel(conn sqlx.SqlConn) MemberOutboxModel {
	return &defaultMemberOutboxModel{conn: conn}
}

func (m *defaultMemberOutboxModel) Insert(ctx context.Context, tx sqlx.Session, out *MemberOutbox) error {
	if out.CreatedAt == 0 {
		out.CreatedAt = time.Now().Unix()
	}
	query := `INSERT INTO member_outbox (event_id, event_type, aggregate_id, payload, status, attempts, next_retry_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	res, err := tx.ExecCtx(ctx, query, out.EventID, out.EventType, out.AggregateID, out.Payload,
		out.Status, out.Attempts, out.NextRetryAt, out.CreatedAt)
	if err != nil {
		return err
	}
	out.ID, err = res.LastInsertId()
	return err
}

func (m *defaultMemberOutboxModel) ListPending(ctx context.Context, now int64, limit int) ([]*MemberOutbox, error) {
	query := `SELECT id, event_id, event_type, aggregate_id, payload, status, attempts, next_retry_at, last_error, created_at, published_at
		FROM member_outbox WHERE status = 0 AND (next_retry_at = 0 OR next_retry_at <= ?) ORDER BY id ASC LIMIT ?`
	var rows []*MemberOutbox
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, now, limit); err != nil {
		if err == sql.ErrNoRows {
			return []*MemberOutbox{}, nil
		}
		return nil, err
	}
	return rows, nil
}

func (m *defaultMemberOutboxModel) MarkPublished(ctx context.Context, id int64, publishedAt int64) error {
	query := `UPDATE member_outbox SET status = ?, published_at = ?, last_error = '' WHERE id = ?`
	_, err := m.conn.ExecCtx(ctx, query, OutboxStatusPublished, publishedAt, id)
	return err
}

func (m *defaultMemberOutboxModel) MarkRetry(ctx context.Context, id int64, attempts int32, nextRetryAt int64, lastError string) error {
	query := `UPDATE member_outbox SET status = 0, attempts = ?, next_retry_at = ?, last_error = ? WHERE id = ?`
	_, err := m.conn.ExecCtx(ctx, query, attempts, nextRetryAt, lastError, id)
	return err
}

func (m *defaultMemberOutboxModel) MarkFailed(ctx context.Context, id int64, lastError string) error {
	query := `UPDATE member_outbox SET status = ?, last_error = ? WHERE id = ?`
	_, err := m.conn.ExecCtx(ctx, query, OutboxStatusFailed, lastError, id)
	return err
}
