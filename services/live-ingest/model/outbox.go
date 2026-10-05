package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// EventTypeStreamState 是本服务产出的唯一事件类型；topic = live.state.v1
// （由 eventenvelope.Topic 拼出），与 docs/api-and-events.md §4 的事件清单一致。
const EventTypeStreamState = "live.state"

// SchemaVersionStreamState live.state.v1 的 schema 版本。字段变更必须递增。
const SchemaVersionStreamState = 1

// AggregateTypeStream 事件聚合根类型（payload 里的 aggregate_id = stream_id）。
const AggregateTypeStream = "live_stream"

// outboxColumns 是 live_ingest_outbox 的列清单，必须与
// deploy/migrations/live-ingest/000003_create_live_stream_event_tables.sql 完全一致。
const outboxColumns = "id, event_id, event_type, schema_version, aggregate_type, aggregate_id, stream_id, room_id, " +
	"seq, payload, state, retry_count, next_retry_at, last_error, occurred_at, ctime, mtime"

// EventOutbox 领域事件 Outbox（live_ingest_outbox 表）。
//
// 遵循 AGENTS.md §5：状态迁移与事件记录在同一事务内提交，由独立发布器按 id 升序
// 投递到 MQ（Topic = live.state.v1），消费者按 event_id 幂等去重、按 seq 拒绝回退。
// payload 存 common/eventenvelope.Envelope 的完整 JSON，事件字段以信封为准。
type EventOutbox struct {
	ID            int64  `db:"id"`             // 自增主键（发布器按此升序保证同流顺序）
	EventID       string `db:"event_id"`       // 事件唯一 ID（ULID，与 live_stream_event 同源）
	EventType     string `db:"event_type"`     // live.state
	SchemaVersion int32  `db:"schema_version"` // schema 版本
	AggregateType string `db:"aggregate_type"` // live_stream
	AggregateID   string `db:"aggregate_id"`   // 聚合根 ID（stream_id）
	StreamID      string `db:"stream_id"`      // 冗余列，便于按流对账
	RoomID        int64  `db:"room_id"`        // 房间引用（live-room 消费方按此路由）
	Seq           int64  `db:"seq"`            // 该流单调事件序号
	Payload       string `db:"payload"`        // 事件信封完整 JSON
	State         int32  `db:"state"`          // 见 OutboxState*
	RetryCount    int32  `db:"retry_count"`    // 已重试次数（指数退避依据）
	NextRetryAt   int64  `db:"next_retry_at"`  // 下次重试时间（Unix 秒，0 表示可立即投递）
	LastError     string `db:"last_error"`     // 最近一次投递错误（不含密钥）
	OccurredAt    int64  `db:"occurred_at"`    // 事件发生时间（Unix 秒）
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// OutboxCheckpoint 是发布位点快照（GetEventPublishCheckpoint 的数据来源）。
type OutboxCheckpoint struct {
	LastPublishedID int64 `db:"last_published_id"` // 已发布的最大 outbox id
	LastPublishedAt int64 `db:"last_published_at"` // 最近发布时间（Unix 秒）
	PendingCount    int64 `db:"pending_count"`     // 待发布行数（含退避中）
	FailedCount     int64 `db:"failed_count"`      // 失败行数
	OldestPendingID int64 `db:"oldest_pending_id"` // 最老待发布 outbox id，0 表示无
	OldestPendingAt int64 `db:"oldest_pending_at"` // 最老待发布事件的 occurred_at
}

// EventOutboxModel live_ingest_outbox 表查询与写入接口。
type EventOutboxModel interface {
	// Insert 在业务事务内写入事件；tx 为空时退化为自动提交（仅供测试与回填）。
	// uniq_event_id 冲突表示重复投递，调用方必须按幂等成功处理。
	Insert(ctx context.Context, tx sqlx.Session, out *EventOutbox) error
	// ListPending 查询到期可投递事件（state=待发布 且 next_retry_at 已到），按 id 升序。
	ListPending(ctx context.Context, now int64, limit int32) ([]*EventOutbox, error)
	// MarkPublished 标记已发布（mtime 即发布时间，本表不另存 published_at）。
	MarkPublished(ctx context.Context, id, publishedAt int64) error
	// MarkRetry 记录失败并设置下次重试时间（退避秒数由调用方计算）。
	MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error
	// MarkFailed 超过最大重试后标记失败（等 RetryFailedEvents 人工放行）。
	MarkFailed(ctx context.Context, id int64, lastError string) error
	// ResetFailed 把失败事件重置为待发布：同时清零 retry_count 与 next_retry_at
	// （否则发布器会按旧重试计数立刻再次判失败）。eventIDs 为空时按 limit 批量重置，
	// 返回被重置的行数（RetryFailedEvents 的落地点）。
	ResetFailed(ctx context.Context, tx sqlx.Session, eventIDs []string, limit int32, reason string) (int64, error)
	// CountByState 统计某发布状态的行数（RetryFailedEvents 回带 remaining_failed）。
	CountByState(ctx context.Context, state int32) (int64, error)
	// Checkpoint 汇总位点与滞后度（单条聚合查询，全部走 state 索引）。
	Checkpoint(ctx context.Context) (*OutboxCheckpoint, error)
	// ListByState 按状态取样（位点响应的 pending/failed 样本），limit 强制生效。
	ListByState(ctx context.Context, state int32, limit int32) ([]*EventOutbox, error)
	// PrunePublished 归档已发布事件（由 services/cron 调用），返回影响行数。
	PrunePublished(ctx context.Context, before int64, limit int32) (int64, error)
}

type defaultEventOutboxModel struct {
	conn sqlx.SqlConn
}

// NewEventOutboxModel 创建 EventOutboxModel 实现。
func NewEventOutboxModel(conn sqlx.SqlConn) EventOutboxModel {
	return &defaultEventOutboxModel{conn: conn}
}

func (m *defaultEventOutboxModel) Insert(ctx context.Context, tx sqlx.Session, out *EventOutbox) error {
	session := pickSession(m.conn, tx)
	if out.Ctime == 0 {
		out.Ctime = nowUnix()
	}
	out.Mtime = out.Ctime
	if out.OccurredAt == 0 {
		out.OccurredAt = out.Ctime
	}
	if out.EventType == "" {
		out.EventType = EventTypeStreamState
	}
	if out.SchemaVersion == 0 {
		out.SchemaVersion = SchemaVersionStreamState
	}
	if out.AggregateType == "" {
		out.AggregateType = AggregateTypeStream
	}
	if out.AggregateID == "" {
		out.AggregateID = out.StreamID
	}
	if out.State == 0 {
		out.State = OutboxStatePending
	}
	_, err := session.ExecCtx(ctx,
		"INSERT INTO live_ingest_outbox (event_id, event_type, schema_version, aggregate_type, aggregate_id, "+
			"stream_id, room_id, seq, payload, state, retry_count, next_retry_at, last_error, occurred_at, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		out.EventID, out.EventType, out.SchemaVersion, out.AggregateType, out.AggregateID,
		out.StreamID, out.RoomID, out.Seq, out.Payload, out.State, out.RetryCount, out.NextRetryAt,
		truncate(out.LastError, 512), out.OccurredAt, out.Ctime, out.Mtime)
	if err != nil {
		return fmt.Errorf("live_ingest_outbox Insert: %w", err)
	}
	return nil
}

func (m *defaultEventOutboxModel) ListPending(ctx context.Context, now int64, limit int32) ([]*EventOutbox, error) {
	query := "SELECT " + outboxColumns + " FROM live_ingest_outbox " +
		"WHERE state = ? AND (next_retry_at = 0 OR next_retry_at <= ?) ORDER BY id ASC LIMIT ?"
	var rows []*EventOutbox
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, OutboxStatePending, now, clampLimit(limit, 500)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_ingest_outbox ListPending: %w", err)
	}
	return rows, nil
}

func (m *defaultEventOutboxModel) MarkPublished(ctx context.Context, id, publishedAt int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE live_ingest_outbox SET state = ?, last_error = '', mtime = ? WHERE id = ? AND state <> ?",
		OutboxStatePublished, publishedAt, id, OutboxStatePublished)
	if err != nil {
		return fmt.Errorf("live_ingest_outbox MarkPublished: %w", err)
	}
	return nil
}

func (m *defaultEventOutboxModel) MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE live_ingest_outbox SET state = ?, retry_count = ?, next_retry_at = ?, last_error = ?, mtime = ? "+
			"WHERE id = ? AND state = ?",
		OutboxStatePending, retryCount, nextRetryAt, truncate(lastError, 512), nowUnix(), id, OutboxStatePending)
	if err != nil {
		return fmt.Errorf("live_ingest_outbox MarkRetry: %w", err)
	}
	return nil
}

func (m *defaultEventOutboxModel) MarkFailed(ctx context.Context, id int64, lastError string) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE live_ingest_outbox SET state = ?, last_error = ?, mtime = ? WHERE id = ? AND state = ?",
		OutboxStateFailed, truncate(lastError, 512), nowUnix(), id, OutboxStatePending)
	if err != nil {
		return fmt.Errorf("live_ingest_outbox MarkFailed: %w", err)
	}
	return nil
}

func (m *defaultEventOutboxModel) ResetFailed(
	ctx context.Context,
	tx sqlx.Session,
	eventIDs []string,
	limit int32,
	reason string,
) (int64, error) {
	session := pickSession(m.conn, tx)
	args := []interface{}{OutboxStatePending, truncate(reason, 512), nowUnix()}

	var query string
	if len(eventIDs) == 0 {
		query = "UPDATE live_ingest_outbox SET state = ?, retry_count = 0, next_retry_at = 0, last_error = ?, mtime = ? " +
			"WHERE state = ? ORDER BY id ASC LIMIT ?"
		args = append(args, OutboxStateFailed, clampLimit(limit, 500))
	} else {
		if len(eventIDs) > 500 {
			eventIDs = eventIDs[:500]
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventIDs)), ",")
		query = "UPDATE live_ingest_outbox SET state = ?, retry_count = 0, next_retry_at = 0, last_error = ?, mtime = ? " +
			"WHERE state = ? AND event_id IN (" + placeholders + ")"
		args = append(args, OutboxStateFailed)
		args = append(args, stringArgs(eventIDs)...)
	}

	res, err := session.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("live_ingest_outbox ResetFailed: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_ingest_outbox ResetFailed RowsAffected: %w", err)
	}
	return affected, nil
}

func (m *defaultEventOutboxModel) CountByState(ctx context.Context, state int32) (int64, error) {
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM live_ingest_outbox WHERE state = ? LIMIT 1", state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_ingest_outbox CountByState: %w", err)
	}
	return cnt, nil
}

func (m *defaultEventOutboxModel) Checkpoint(ctx context.Context) (*OutboxCheckpoint, error) {
	// 一次扫表拿到全部位点指标：published 的最大 id/时间与 pending/failed 的计数
	// 都来自同一 state 索引，避免多次往返造成快照不一致。
	query := "SELECT " +
		"COALESCE(MAX(CASE WHEN state = ? THEN id END), 0) AS last_published_id, " +
		"COALESCE(MAX(CASE WHEN state = ? THEN mtime END), 0) AS last_published_at, " +
		"COALESCE(SUM(CASE WHEN state = ? THEN 1 ELSE 0 END), 0) AS pending_count, " +
		"COALESCE(SUM(CASE WHEN state = ? THEN 1 ELSE 0 END), 0) AS failed_count, " +
		"COALESCE(MIN(CASE WHEN state = ? THEN id END), 0) AS oldest_pending_id, " +
		"COALESCE(MIN(CASE WHEN state = ? THEN occurred_at END), 0) AS oldest_pending_at " +
		"FROM live_ingest_outbox WHERE state IN (?, ?, ?) LIMIT 1"
	var cp OutboxCheckpoint
	err := m.conn.QueryRowCtx(ctx, &cp, query,
		OutboxStatePublished, OutboxStatePublished, OutboxStatePending, OutboxStateFailed,
		OutboxStatePending, OutboxStatePending, OutboxStatePending, OutboxStatePublished, OutboxStateFailed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &OutboxCheckpoint{}, nil
		}
		return nil, fmt.Errorf("live_ingest_outbox Checkpoint: %w", err)
	}
	return &cp, nil
}

func (m *defaultEventOutboxModel) ListByState(ctx context.Context, state int32, limit int32) ([]*EventOutbox, error) {
	query := "SELECT " + outboxColumns + " FROM live_ingest_outbox WHERE state = ? ORDER BY id ASC LIMIT ?"
	var rows []*EventOutbox
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, state, clampLimit(limit, 500)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_ingest_outbox ListByState: %w", err)
	}
	return rows, nil
}

func (m *defaultEventOutboxModel) PrunePublished(ctx context.Context, before int64, limit int32) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM live_ingest_outbox WHERE state = ? AND occurred_at < ? ORDER BY id ASC LIMIT ?",
		OutboxStatePublished, before, clampLimit(limit, 1000))
	if err != nil {
		return 0, fmt.Errorf("live_ingest_outbox PrunePublished: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_ingest_outbox PrunePublished RowsAffected: %w", err)
	}
	return affected, nil
}
