package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SearchConsumerOffset 事件消费流水（search_consumer_offset 表）。
// 作用（docs/api-and-events.md §6）：
//  1. 按 event_id 去重：uniq_event_id 保证重复投递只生效一次；
//  2. 状态机 received → processing → succeeded / retry → dead_letter；
//  3. payload_json 保存重投所需原文（进程重启后由 retry sweeper 继续退避重试），
//     事件进入终态（succeeded/dead_letter）后必须清空，避免长期堆积原文。
//
// 注：payload 只存放生产者已脱敏的事件信封，不落手机号/身份证/Token/IP（AGENTS.md §7）。
type SearchConsumerOffset struct {
	ID          int64  `db:"id"`            // 自增主键
	EventID     string `db:"event_id"`      // 事件 ID（幂等键，ULID）
	EventType   string `db:"event_type"`    // 事件类型，如 content.published
	Topic       string `db:"topic"`         // 来源 topic，如 content.published.v1
	PartitionNo int32  `db:"partition_no"`  // Kafka 分区号
	OffsetNo    int64  `db:"offset_no"`     // Kafka offset
	State       string `db:"state"`         // received/processing/succeeded/retry/dead_letter
	RetryCount  int32  `db:"retry_count"`   // 已重试次数
	NextRetryAt int64  `db:"next_retry_at"` // 下次重试时间（Unix 秒，0 表示不需要）
	LastError   string `db:"last_error"`    // 最近一次失败原因（脱敏）
	OccurredAt  int64  `db:"occurred_at"`   // 事件发生时间（Unix 秒）
	PayloadJSON string `db:"payload_json"`  // 重投原文（终态清空）
	Ctime       int64  `db:"ctime"`         // 入库时间（Unix 秒）
	Mtime       int64  `db:"mtime"`         // 修改时间（Unix 秒）
}

// SearchConsumerOffsetModel search_consumer_offset 表查询与写入接口。
type SearchConsumerOffsetModel interface {
	// MarkReceived 登记新事件；命中 uniq_event_id 时返回 duplicate=true，不覆盖既有状态。
	MarkReceived(ctx context.Context, rec *SearchConsumerOffset) (duplicate bool, err error)
	// FindByEventID 按 event_id 查询；不存在返回 (nil, nil)。
	FindByEventID(ctx context.Context, eventID string) (*SearchConsumerOffset, error)
	// MarkProcessing 置为 processing（仅 received/retry 可进入，避免终态回退）。
	MarkProcessing(ctx context.Context, eventID string, now int64) error
	// MarkSucceeded 置为 succeeded 并清空 payload_json / last_error。
	MarkSucceeded(ctx context.Context, eventID string, now int64) error
	// MarkRetry 记录一次失败并安排退避重试时间。
	MarkRetry(ctx context.Context, eventID string, retryCount int32, nextRetryAt int64, lastError string, now int64) error
	// MarkDeadLetter 置为 dead_letter 并清空 payload_json（原文摘要已写入 search_dead_letter）。
	MarkDeadLetter(ctx context.Context, eventID, lastError string, now int64) error
	// ListDueForRetry 查询到期的 retry 记录（idx_state_retry）。
	ListDueForRetry(ctx context.Context, now int64, limit int) ([]*SearchConsumerOffset, error)
	// CountByState 统计某状态的记录数（健康检查/积压告警）。
	CountByState(ctx context.Context, state string) (int64, error)
}

type defaultOffsetModel struct {
	conn sqlx.SqlConn
}

// NewSearchConsumerOffsetModel 创建 SearchConsumerOffsetModel 实现。
func NewSearchConsumerOffsetModel(conn sqlx.SqlConn) SearchConsumerOffsetModel {
	return &defaultOffsetModel{conn: conn}
}

const offsetColumns = "id, event_id, event_type, topic, partition_no, offset_no, state, retry_count, next_retry_at, last_error, occurred_at, payload_json, ctime, mtime"

func (m *defaultOffsetModel) MarkReceived(ctx context.Context, rec *SearchConsumerOffset) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT IGNORE INTO search_consumer_offset (event_id, event_type, topic, partition_no, offset_no, state, retry_count, next_retry_at, last_error, occurred_at, payload_json, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		rec.EventID, rec.EventType, rec.Topic, rec.PartitionNo, rec.OffsetNo, rec.State, rec.RetryCount,
		rec.NextRetryAt, rec.LastError, rec.OccurredAt, rec.PayloadJSON, rec.Ctime, rec.Mtime)
	if err != nil {
		return false, fmt.Errorf("search_consumer_offset MarkReceived: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("search_consumer_offset MarkReceived RowsAffected: %w", err)
	}
	return aff == 0, nil
}

func (m *defaultOffsetModel) FindByEventID(ctx context.Context, eventID string) (*SearchConsumerOffset, error) {
	var rec SearchConsumerOffset
	query := "SELECT " + offsetColumns + " FROM search_consumer_offset WHERE event_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &rec, query, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_consumer_offset FindByEventID: %w", err)
	}
	return &rec, nil
}

// MarkProcessing 置为 processing；只在 received/retry 上生效，避免终态被回退。
func (m *defaultOffsetModel) MarkProcessing(ctx context.Context, eventID string, now int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE search_consumer_offset SET state = ?, mtime = ? WHERE event_id = ? AND state IN (?, ?)",
		OffsetStateProcessing, now, eventID, OffsetStateReceived, OffsetStateRetry); err != nil {
		return fmt.Errorf("search_consumer_offset MarkProcessing: %w", err)
	}
	return nil
}

func (m *defaultOffsetModel) MarkSucceeded(ctx context.Context, eventID string, now int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE search_consumer_offset SET state = ?, next_retry_at = 0, last_error = '', payload_json = '', mtime = ? WHERE event_id = ?",
		OffsetStateSucceeded, now, eventID); err != nil {
		return fmt.Errorf("search_consumer_offset MarkSucceeded: %w", err)
	}
	return nil
}

func (m *defaultOffsetModel) MarkRetry(ctx context.Context, eventID string, retryCount int32, nextRetryAt int64, lastError string, now int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE search_consumer_offset SET state = ?, retry_count = ?, next_retry_at = ?, last_error = ?, mtime = ? WHERE event_id = ? AND state <> ?",
		OffsetStateRetry, retryCount, nextRetryAt, lastError, now, eventID, OffsetStateDeadLetter); err != nil {
		return fmt.Errorf("search_consumer_offset MarkRetry: %w", err)
	}
	return nil
}

func (m *defaultOffsetModel) MarkDeadLetter(ctx context.Context, eventID, lastError string, now int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE search_consumer_offset SET state = ?, last_error = ?, payload_json = '', mtime = ? WHERE event_id = ?",
		OffsetStateDeadLetter, lastError, now, eventID); err != nil {
		return fmt.Errorf("search_consumer_offset MarkDeadLetter: %w", err)
	}
	return nil
}

func (m *defaultOffsetModel) ListDueForRetry(ctx context.Context, now int64, limit int) ([]*SearchConsumerOffset, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var rows []*SearchConsumerOffset
	query := "SELECT " + offsetColumns + " FROM search_consumer_offset WHERE state = ? AND next_retry_at <= ? ORDER BY next_retry_at ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, OffsetStateRetry, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_consumer_offset ListDueForRetry: %w", err)
	}
	return rows, nil
}

func (m *defaultOffsetModel) CountByState(ctx context.Context, state string) (int64, error) {
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM search_consumer_offset WHERE state = ?", state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("search_consumer_offset CountByState: %w", err)
	}
	return total, nil
}
