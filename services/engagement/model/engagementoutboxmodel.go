package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// EngagementOutbox 领域事件 Outbox（engagement_outbox 表）。
// 遵循 AGENTS.md §5：Like/AddFav/DelFav/AddShare 的「互动关系行 + 计数增量 + 事件行」
// 在同一事务内提交，由 internal/publisher 按 id 升序投递到 engagement.action.v1
// （Topic = event_type + ".v" + schema_version，见 docs/api-and-events.md §4/§5），
// 消费者按 event_id 幂等去重。
// payload 存 common/eventenvelope.Envelope 的完整 JSON，事件字段以信封为准。
//
// 幂等口径分两层，别混淆：本表的 UNIQUE 索引 (event_id) 只保证「同一事件不写两行」；
// 「同一用户对同一对象只算一次」由 thumbup_like (business, mid, message_id)、
// favorite_item (mid, oid, tp)、share_log (oid, mid, tp, day) 三个唯一键负责。
// 因此同一对象在本表可以有很多行（不同用户、不同动作），计数没有变化的分支不写本表。
type EngagementOutbox struct {
	// ID 自增主键（发布器按此升序保证同对象顺序）
	ID int64 `db:"id"`
	// EventID 事件唯一 ID（ULID，唯一索引，消费者据此幂等）
	EventID string `db:"event_id"`
	// EventType 事件类型（engagement.action）
	EventType string `db:"event_type"`
	// SchemaVersion 事件 schema 版本
	SchemaVersion int32 `db:"schema_version"`
	// AggregateType 聚合根类型（content）
	AggregateType string `db:"aggregate_type"`
	// AggregateID 聚合根 ID（对象 ID 的十进制字符串，同时是分区键）
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

// EngagementOutboxModel engagement_outbox 表查询与写入接口。
type EngagementOutboxModel interface {
	// Insert 在业务事务内写入事件（tx 为空时退化为自动提交）。
	Insert(ctx context.Context, tx sqlx.Session, out *EngagementOutbox) error
	// ListPending 查询到期可投递事件（按 id 升序）。
	ListPending(ctx context.Context, now int64, limit int32) ([]*EngagementOutbox, error)
	// MarkPublished 标记已发布。
	MarkPublished(ctx context.Context, id, publishedAt int64) error
	// MarkRetry 记录失败并设置下次重试时间（指数退避由调用方计算）。
	MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error
	// MarkFailed 超过最大重试后标记失败（人工处理）。
	MarkFailed(ctx context.Context, id int64, lastError string) error
}

type defaultEngagementOutboxModel struct {
	conn sqlx.SqlConn
}

// NewEngagementOutboxModel 创建 EngagementOutboxModel 实现。
func NewEngagementOutboxModel(conn sqlx.SqlConn) EngagementOutboxModel {
	return &defaultEngagementOutboxModel{conn: conn}
}

// Insert 写一行待发布事件。
// event_id 与 payload 都是 NOT NULL 且没有 DEFAULT 的必填列（见建表注释的幂等段落）：
// 给 event_id 一个空串默认值会让第二行「忘了填」的写入直接撞唯一键，
// 报错信息指向 uniq_event_id，运维会误判成「重复投递」而不是「装配漏字段」。
func (m *defaultEngagementOutboxModel) Insert(ctx context.Context, tx sqlx.Session, out *EngagementOutbox) error {
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
		"INSERT INTO engagement_outbox (event_id, event_type, schema_version, aggregate_type, aggregate_id, "+
			"payload, state, retry_count, next_retry_at, last_error, occurred_at, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		out.EventID, out.EventType, out.SchemaVersion, out.AggregateType, out.AggregateID,
		out.Payload, out.State, out.RetryCount, out.NextRetryAt, out.LastError,
		out.OccurredAt, out.Ctime, out.Mtime)
	if err != nil {
		return fmt.Errorf("engagement_outbox Insert: %w", err)
	}
	return nil
}

// ListPending 取 state=0 且退避已到期的行，按 id 升序。
// 只取待发布：state=2（判死）的行不再被重捞，这是 recommend-recall 那类
// 「把死行每轮重投」缺陷的防线（见 common/outbox/README.md）。
func (m *defaultEngagementOutboxModel) ListPending(ctx context.Context, now int64, limit int32) ([]*EngagementOutbox, error) {
	query := "SELECT id, event_id, event_type, schema_version, aggregate_type, aggregate_id, " +
		"payload, state, retry_count, next_retry_at, last_error, occurred_at, ctime, mtime " +
		"FROM engagement_outbox " +
		"WHERE state = 0 AND (next_retry_at = 0 OR next_retry_at <= ?) ORDER BY id ASC LIMIT ?"
	var rows []*EngagementOutbox
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("engagement_outbox ListPending: %w", err)
	}
	return rows, nil
}

// MarkPublished 置已发布。publishedAt 写入 mtime：本表不额外保存发布时间列。
func (m *defaultEngagementOutboxModel) MarkPublished(ctx context.Context, id, publishedAt int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE engagement_outbox SET state = ?, last_error = '', mtime = ? WHERE id = ?",
		OutboxStatePublished, publishedAt, id)
	if err != nil {
		return fmt.Errorf("engagement_outbox MarkPublished: %w", err)
	}
	return nil
}

// MarkRetry 行保持待发布状态，只推进 retry_count 与 next_retry_at。
func (m *defaultEngagementOutboxModel) MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE engagement_outbox SET state = ?, retry_count = ?, next_retry_at = ?, last_error = ?, mtime = ? WHERE id = ?",
		OutboxStatePending, retryCount, nextRetryAt, lastError, nowUnix(), id)
	if err != nil {
		return fmt.Errorf("engagement_outbox MarkRetry: %w", err)
	}
	return nil
}

// MarkFailed 判死：state=2 之后 ListPending 不再取它，本服务没有人工放行接口。
func (m *defaultEngagementOutboxModel) MarkFailed(ctx context.Context, id int64, lastError string) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE engagement_outbox SET state = ?, last_error = ?, mtime = ? WHERE id = ?",
		OutboxStateFailed, lastError, nowUnix(), id)
	if err != nil {
		return fmt.Errorf("engagement_outbox MarkFailed: %w", err)
	}
	return nil
}
