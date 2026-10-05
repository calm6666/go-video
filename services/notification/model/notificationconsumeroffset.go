package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 事件消费状态：与 rpc/notification.proto 的 EventState 一致（docs/api-and-events.md §6）。
const (
	// EventStateReceived 已收到，未开始处理。
	EventStateReceived int32 = 1
	// EventStateProcessing 处理中。
	EventStateProcessing int32 = 2
	// EventStateSucceeded 处理成功（终态）。
	EventStateSucceeded int32 = 3
	// EventStateRetry 处理失败，等待退避重试。
	EventStateRetry int32 = 4
	// EventStateDeadLetter 重试耗尽，转死信留档（终态）。
	EventStateDeadLetter int32 = 5
)

// NotificationConsumerOffset notification.request.v1 消费位点与幂等登记表。
// 去重以 event_id 为唯一键；partition_no/offset_no 仅留档，kq 由消费组自动提交位点，
// 不透传分区位点给 handler（见 README“消费语义”）。
// payload_json 暂存原始信封，用于 retry/dead_letter 后的重放（判重试或人工重投时不再依赖
// Kafka 是否重复投递）；事件成功后立即清空，避免报文长期留存。
type NotificationConsumerOffset struct {
	EventId     string `db:"event_id"`      // 事件 ID（唯一，去重键）
	EventType   string `db:"event_type"`    // 事件类型
	Topic       string `db:"topic"`         // 来源 topic
	PartitionNo int32  `db:"partition_no"`  // 分区号（留档，0 表示未知）
	OffsetNo    int64  `db:"offset_no"`     // 位点（留档，0 表示未知）
	State       int32  `db:"state"`         // 状态，见 EventState* 常量
	RetryCount  int32  `db:"retry_count"`   // 已重试次数
	NextRetryAt int64  `db:"next_retry_at"` // 下次重试时间（Unix 秒）
	LastError   string `db:"last_error"`    // 最近一次错误（脱敏）
	OccurredAt  int64  `db:"occurred_at"`   // 事件发生时间（Unix 秒）
	BodyDigest  string `db:"body_digest"`   // 原始报文 sha256 hex（不留明文）
	PayloadJson string `db:"payload_json"`  // 原始信封（重试/重投用，成功后清空）
	TraceId     string `db:"trace_id"`      // 链路 ID
	Ctime       int64  `db:"ctime"`         // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`         // 修改时间（Unix 秒）
}

// NotificationConsumerOffsetModel notification_consumer_offset 表读写接口。
type NotificationConsumerOffsetModel interface {
	// InsertIfAbsent 以 event_id 幂等登记事件；返回 false 表示该事件已存在（重复投递）。
	InsertIfAbsent(ctx context.Context, o *NotificationConsumerOffset) (bool, error)
	// FindOne 按 event_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, eventId string) (*NotificationConsumerOffset, error)
	// MarkState 带源状态守卫地流转状态，可同时更新重试信息。
	// to=Retry 时必须给 retryCount/nextRetryAt；终态把 next_retry_at 归零；
	// to=Succeeded 时同时清空 payload_json（报文不再需要重放）。
	MarkState(ctx context.Context, eventId string, to int32, retryCount int32, nextRetryAt int64, lastError string, from []int32) (bool, error)
	// ListDue 扫描到期待重试事件（state=retry 且 next_retry_at <= now）。
	ListDue(ctx context.Context, now int64, limit int32) ([]*NotificationConsumerOffset, error)
}

type defaultNotificationConsumerOffsetModel struct {
	conn sqlx.SqlConn
}

// NewNotificationConsumerOffsetModel 创建 NotificationConsumerOffsetModel 实现。
func NewNotificationConsumerOffsetModel(conn sqlx.SqlConn) NotificationConsumerOffsetModel {
	return &defaultNotificationConsumerOffsetModel{conn: conn}
}

const offsetCols = "event_id, event_type, topic, partition_no, offset_no, state, retry_count, next_retry_at, " +
	"last_error, occurred_at, body_digest, payload_json, trace_id, ctime, mtime"

func (m *defaultNotificationConsumerOffsetModel) InsertIfAbsent(ctx context.Context, o *NotificationConsumerOffset) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT IGNORE INTO notification_consumer_offset (event_id, event_type, topic, partition_no, offset_no, state, "+
			"retry_count, next_retry_at, last_error, occurred_at, body_digest, payload_json, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		o.EventId, o.EventType, o.Topic, o.PartitionNo, o.OffsetNo, o.State, o.RetryCount, o.NextRetryAt,
		o.LastError, o.OccurredAt, o.BodyDigest, o.PayloadJson, o.TraceId, o.Ctime, o.Mtime)
	if err != nil {
		return false, fmt.Errorf("notification_consumer_offset InsertIfAbsent: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("notification_consumer_offset InsertIfAbsent RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultNotificationConsumerOffsetModel) FindOne(ctx context.Context, eventId string) (*NotificationConsumerOffset, error) {
	var o NotificationConsumerOffset
	err := m.conn.QueryRowCtx(ctx, &o,
		"SELECT "+offsetCols+" FROM notification_consumer_offset WHERE event_id = ? LIMIT 1", eventId)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("notification_consumer_offset FindOne: %w", err)
	}
	return &o, nil
}

func (m *defaultNotificationConsumerOffsetModel) MarkState(ctx context.Context, eventId string, to int32,
	retryCount int32, nextRetryAt int64, lastError string, from []int32) (bool, error) {
	if len(from) == 0 {
		return false, ErrIllegalStateTransition
	}
	set := "state = ?, retry_count = ?, last_error = ?"
	switch to {
	case EventStateRetry:
		set += ", next_retry_at = ?"
	case EventStateSucceeded:
		// 成功后立即丢弃暂存信封：既不再需要重放，也避免报文长期留存。
		set += ", next_retry_at = 0, payload_json = ''"
	default:
		set += ", next_retry_at = 0"
	}
	args := []any{to, retryCount, truncate(lastError, 500)}
	if to == EventStateRetry {
		args = append(args, nextRetryAt)
	}
	q := "UPDATE notification_consumer_offset SET " + set + ", mtime = ? WHERE event_id = ? AND state IN (" + placeholders(len(from)) + ")"
	args = append(args, nowUnix(), eventId)
	args = append(args, int32Args(from)...)
	res, err := m.conn.ExecCtx(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("notification_consumer_offset MarkState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("notification_consumer_offset MarkState RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultNotificationConsumerOffsetModel) ListDue(ctx context.Context, now int64, limit int32) ([]*NotificationConsumerOffset, error) {
	if limit < 1 {
		limit = 64
	}
	var rows []*NotificationConsumerOffset
	q := "SELECT " + offsetCols + " FROM notification_consumer_offset WHERE state = ? AND next_retry_at <= ? " +
		"ORDER BY next_retry_at ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, q, EventStateRetry, now, limit); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("notification_consumer_offset ListDue: %w", err)
	}
	return rows, nil
}
