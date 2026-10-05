package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ConsumerOffset 事件消费状态行（inbox_consumer_offset 表投影）。
// 状态机：received -> processing -> succeeded
//
//	-> retry(退避) -> processing -> ... -> dead_letter
//
// 真值是 event_id 唯一键：任何重复、乱序或迟到消息都只能读到既有状态，
// 不会把 succeeded 改回 processing（AGENTS.md §5、docs/api-and-events.md §6）。
type ConsumerOffset struct {
	ID          int64  `db:"id"`            // 主键 ID
	EventID     string `db:"event_id"`      // 事件唯一 ID
	EventType   string `db:"event_type"`    // 事件类型
	Topic       string `db:"topic"`         // 来源 topic
	PartitionNo int32  `db:"partition_no"`  // Kafka 分区（kq 不上报时为 0）
	MsgOffset   int64  `db:"msg_offset"`    // Kafka 位点（kq 不上报时为 0）
	State       string `db:"state"`         // received/processing/succeeded/retry/dead_letter
	RetryCount  int32  `db:"retry_count"`   // 已失败次数
	NextRetryAt int64  `db:"next_retry_at"` // 退避到期时间（Unix 秒）
	LastError   string `db:"last_error"`    // 最近失败原因（脱敏截断）
	// Payload 只在失败路径写入（retry/dead_letter），成功行由 MarkSucceeded 清空，
	// 因此不会长期堆积原始事件；它让退避重投不依赖 Kafka 是否重投同一条消息。
	Payload    string `db:"payload"`     // 原始事件信封 JSON（可为空）
	OccurredAt int64  `db:"occurred_at"` // 事件发生时间（Unix 秒）
	Ctime      int64  `db:"ctime"`       // 首次收到时间（Unix 秒）
	Mtime      int64  `db:"mtime"`       // 状态变更时间（Unix 秒）
}

// ClaimOutcome 领取事件的结果。
type ClaimOutcome int

const (
	// ClaimAcquired 获得处理权：首次投递、退避到期重投或崩溃后回收。
	ClaimAcquired ClaimOutcome = iota
	// ClaimDuplicate 事件已终结（succeeded/dead_letter）：直接确认，不重复投递消息。
	ClaimDuplicate
	// ClaimDeferred 退避窗口未到或别的处理器正在处理：返回错误让 Kafka 重投。
	ClaimDeferred
)

// String 便于日志与测试断言。
func (o ClaimOutcome) String() string {
	switch o {
	case ClaimAcquired:
		return "acquired"
	case ClaimDuplicate:
		return "duplicate"
	case ClaimDeferred:
		return "deferred"
	default:
		return "unknown"
	}
}

// ConsumerOffsetModel inbox_consumer_offset 表读写接口。
type ConsumerOffsetModel interface {
	// Claim 按 event_id 领取处理权。staleSeconds 用于回收进程崩溃后
	// 停留在 processing 的事件。返回的 offset 携带当前 retry_count，
	// 调用方据此决定继续退避还是判死。
	Claim(ctx context.Context, ev *ConsumerOffset, staleSeconds int64) (ClaimOutcome, *ConsumerOffset, error)
	// MarkSucceeded 标记事件处理完成，同时清空暂存的 payload。
	MarkSucceeded(ctx context.Context, eventID string) error
	// MarkRetry 标记失败并写入退避到期时间、原因与供重投的 payload。
	MarkRetry(ctx context.Context, eventID string, nextRetryAt int64, reason, payload string) error
	// MarkDeadLetter 标记事件判死（配合 inbox_dead_letter 留档），保留 payload 以便人工重放。
	MarkDeadLetter(ctx context.Context, eventID, reason, payload string) error
	// FindOne 查询事件状态；不存在返回 nil。不读取 payload，避免抢位点路径上的无谓大字段。
	FindOne(ctx context.Context, eventID string) (*ConsumerOffset, error)
	// ListOverdueRetry 列出退避到期待重投的事件（state=retry 且 next_retry_at<=now），含 payload。
	ListOverdueRetry(ctx context.Context, now int64, limit int32) ([]*ConsumerOffset, error)
}

type defaultConsumerOffsetModel struct {
	conn sqlx.SqlConn
}

// NewConsumerOffsetModel 创建 ConsumerOffsetModel 实现。
func NewConsumerOffsetModel(conn sqlx.SqlConn) ConsumerOffsetModel {
	return &defaultConsumerOffsetModel{conn: conn}
}

// consumerOffsetColumns 是状态判定路径需要的轻量列（不含 payload）。
const consumerOffsetColumns = "id, event_id, event_type, topic, partition_no, msg_offset, state," +
	" retry_count, next_retry_at, last_error, occurred_at, ctime, mtime"

// consumerOffsetFullColumns 额外带出 payload，只在重投/重放路径使用。
// 读 payload 时统一用 COALESCE 兜住 NULL，使结构体可直接映射成 string，
// 不必引入 sql.NullString。
const consumerOffsetFullColumns = consumerOffsetColumns + ", COALESCE(payload, '') AS payload"

// nullPayload 把空串写成 NULL，避免成功/首发路径在 TEXT 列里堆积空值。
func nullPayload(payload string) any {
	if payload == "" {
		return nil
	}
	return payload
}

func (m *defaultConsumerOffsetModel) Claim(
	ctx context.Context, ev *ConsumerOffset, staleSeconds int64,
) (ClaimOutcome, *ConsumerOffset, error) {
	if ev.EventID == "" {
		return ClaimDeferred, nil, ErrEventIDEmpty
	}
	now := nowUnix()
	if staleSeconds <= 0 {
		staleSeconds = 300
	}

	// 第一次投递：直接以 processing 落库。ON DUPLICATE KEY UPDATE 的自赋值
	// 让重复键场景 affected=0，从而区分“新事件”与“已存在事件”。
	// payload 只在失败/崩溃时才有重放价值，但首次插入就写入：
	// 否则进程在 claim 与回写之间被杀掉，这条事件就没有可重放的原文了。
	insert := "INSERT INTO inbox_consumer_offset (event_id, event_type, topic, partition_no, msg_offset," +
		" state, retry_count, next_retry_at, last_error, payload, occurred_at, ctime, mtime)" +
		" VALUES (?, ?, ?, ?, ?, ?, 0, 0, '', ?, ?, ?, ?)" +
		" ON DUPLICATE KEY UPDATE event_id = event_id"
	res, err := m.conn.ExecCtx(ctx, insert,
		ev.EventID, ev.EventType, ev.Topic, ev.PartitionNo, ev.MsgOffset,
		ConsumerStateProcessing, nullPayload(ev.Payload), ev.OccurredAt, now, now)
	if err != nil {
		return ClaimDeferred, nil, fmt.Errorf("inbox_consumer_offset Claim insert: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ClaimDeferred, nil, fmt.Errorf("inbox_consumer_offset Claim RowsAffected: %w", err)
	}
	if affected == 1 {
		ev.State = ConsumerStateProcessing
		ev.Ctime = now
		ev.Mtime = now
		return ClaimAcquired, ev, nil
	}

	// 已存在：只有在“退避到期的 received/retry”或“崩溃遗留的过期 processing”时才可抢占。
	// 条件写进 WHERE 而不是先读后写，避免并发处理器同时执行同一事件。
	res, err = m.conn.ExecCtx(ctx,
		"UPDATE inbox_consumer_offset SET state = ?, mtime = ?, partition_no = ?, msg_offset = ?"+
			" WHERE event_id = ?"+
			" AND ((state IN (?, ?) AND next_retry_at <= ?) OR (state = ? AND mtime < ?))",
		ConsumerStateProcessing, now, ev.PartitionNo, ev.MsgOffset,
		ev.EventID,
		ConsumerStateReceived, ConsumerStateRetry, now,
		ConsumerStateProcessing, now-staleSeconds)
	if err != nil {
		return ClaimDeferred, nil, fmt.Errorf("inbox_consumer_offset Claim reclaim: %w", err)
	}
	affected, err = res.RowsAffected()
	if err != nil {
		return ClaimDeferred, nil, fmt.Errorf("inbox_consumer_offset Claim reclaim RowsAffected: %w", err)
	}
	if affected == 1 {
		row, err := m.FindOne(ctx, ev.EventID)
		if err != nil {
			return ClaimDeferred, nil, err
		}
		return ClaimAcquired, row, nil
	}

	// 抢占失败：按既有状态判定是重复还是延后。
	row, err := m.FindOne(ctx, ev.EventID)
	if err != nil {
		return ClaimDeferred, nil, err
	}
	if row == nil {
		// 极端情况：被并发清理任务删除，视为新事件重试。
		return ClaimDeferred, nil, ErrConflictProcessing
	}
	switch row.State {
	case ConsumerStateSucceeded, ConsumerStateDeadLetter:
		return ClaimDuplicate, row, nil
	default:
		return ClaimDeferred, row, nil
	}
}

func (m *defaultConsumerOffsetModel) MarkSucceeded(ctx context.Context, eventID string) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE inbox_consumer_offset SET state = ?, next_retry_at = 0, last_error = '',"+
			" payload = NULL, mtime = ? WHERE event_id = ?",
		ConsumerStateSucceeded, nowUnix(), eventID)
	if err != nil {
		return fmt.Errorf("inbox_consumer_offset MarkSucceeded: %w", err)
	}
	return nil
}

func (m *defaultConsumerOffsetModel) MarkRetry(
	ctx context.Context, eventID string, nextRetryAt int64, reason, payload string,
) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE inbox_consumer_offset SET state = ?, retry_count = retry_count + 1,"+
			" next_retry_at = ?, last_error = ?, payload = ?, mtime = ? WHERE event_id = ?",
		ConsumerStateRetry, nextRetryAt, truncate(reason, 512), nullPayload(payload), nowUnix(), eventID)
	if err != nil {
		return fmt.Errorf("inbox_consumer_offset MarkRetry: %w", err)
	}
	return nil
}

func (m *defaultConsumerOffsetModel) MarkDeadLetter(
	ctx context.Context, eventID, reason, payload string,
) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE inbox_consumer_offset SET state = ?, last_error = ?, payload = ?, mtime = ? WHERE event_id = ?",
		ConsumerStateDeadLetter, truncate(reason, 512), nullPayload(payload), nowUnix(), eventID)
	if err != nil {
		return fmt.Errorf("inbox_consumer_offset MarkDeadLetter: %w", err)
	}
	return nil
}

func (m *defaultConsumerOffsetModel) FindOne(ctx context.Context, eventID string) (*ConsumerOffset, error) {
	var row ConsumerOffset
	query := "SELECT " + consumerOffsetColumns + " FROM inbox_consumer_offset WHERE event_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("inbox_consumer_offset FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultConsumerOffsetModel) ListOverdueRetry(
	ctx context.Context, now int64, limit int32,
) ([]*ConsumerOffset, error) {
	if limit <= 0 {
		limit = 100
	}
	query := "SELECT " + consumerOffsetFullColumns +
		" FROM inbox_consumer_offset WHERE state = ? AND next_retry_at <= ? ORDER BY next_retry_at ASC LIMIT ?"
	var rows []*ConsumerOffset
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, ConsumerStateRetry, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("inbox_consumer_offset ListOverdueRetry: %w", err)
	}
	return rows, nil
}

// truncate 折叠换行后按字节上限截断，并保证不切断 UTF-8 序列，
// 否则 utf8mb4 列会因非法字节拒绝写入。
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	replaced := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' || c == '\t' {
			c = ' '
		}
		replaced = append(replaced, c)
	}
	if len(replaced) <= max {
		return string(replaced)
	}
	cut := max
	// 回退到合法字符边界（续字节形如 0b10xxxxxx）。
	for cut > 0 && replaced[cut]&0xC0 == 0x80 {
		cut--
	}
	return string(replaced[:cut])
}
