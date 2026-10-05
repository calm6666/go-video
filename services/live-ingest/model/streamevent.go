package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// streamEventColumns 是 live_stream_event 的列清单，必须与
// deploy/migrations/live-ingest/000003_create_live_stream_event_tables.sql 完全一致。
const streamEventColumns = "id, event_id, stream_id, room_id, session_id, seq, from_state, to_state, node_id, " +
	"interruption_id, interrupted_seconds, stop_reason, report_id, source, reason, occurred_at, trace_id, ctime"

// 事件来源（live_stream_event.source 列），用于排障时区分是谁推动了状态机。
const (
	// EventSourceEntry 接入节点/入口上报（ReportStreamState）。
	EventSourceEntry = "entry"
	// EventSourceHealth 健康采样越界触发（ReportStreamHealth）。
	EventSourceHealth = "health"
	// EventSourceCdn CDN 回调触发（VerifyCdnCallback → ReportStreamState）。
	EventSourceCdn = "cdn"
	// EventSourceAdmin 运营/主播主动停流（CloseStream、RevokeStreamKey 级联）。
	EventSourceAdmin = "admin"
	// EventSourceSweeper 后台扫描器（心跳超时、宽限期耗尽、密钥过期回收）。
	EventSourceSweeper = "sweeper"
)

// StreamEvent 流状态迁移事件（live_stream_event 表投影）。
//
// 这张表是 live.state.v1 的事实来源：event_id 与 (stream_id, seq) 双唯一索引，
// 保证「一个序号只有一个事件」「同一 report_id 只产生一个事件」。
// 事件本身不可更新（append-only），补偿只能追加新 seq 的事件。
type StreamEvent struct {
	ID                 int64  `db:"id"`              // 自增主键（发布器按此升序投递）
	EventID            string `db:"event_id"`        // 事件唯一 ID（ULID，消费方去重锚点）
	StreamID           string `db:"stream_id"`       // 流 ID
	RoomID             int64  `db:"room_id"`         // 房间引用
	SessionID          int64  `db:"session_id"`      // 场次引用
	Seq                int64  `db:"seq"`             // 该流单调递增序号
	FromState          int32  `db:"from_state"`      // 迁移前状态
	ToState            int32  `db:"to_state"`        // 迁移后状态
	NodeID             string `db:"node_id"`         // 关联节点
	InterruptionID     int64  `db:"interruption_id"` // 关联断流记录，0 表示无
	InterruptedSeconds int64  `db:"interrupted_seconds"`
	StopReason         int32  `db:"stop_reason"` // 停流原因，非停流事件为 0
	ReportID           string `db:"report_id"`   // 上报幂等键；内部事件为空串（列可 NULL）
	Source             string `db:"source"`      // 见 EventSource*
	Reason             string `db:"reason"`      // 原因摘要（不含明文密钥）
	OccurredAt         int64  `db:"occurred_at"` // 事件发生时间（Unix 秒）
	TraceID            string `db:"trace_id"`    // 链路追踪 ID
	Ctime              int64  `db:"ctime"`       // 落库时间（Unix 秒）
}

// StreamEventModel live_stream_event 表查询与写入接口（append-only）。
type StreamEventModel interface {
	// Insert 在业务事务内写入事件。uniq_event_id / uniq_stream_seq / uniq_report_id
	// 任一冲突都表示重复投递，调用方用 IsDuplicate 判定后按重放返回首次结果。
	Insert(ctx context.Context, tx sqlx.Session, e *StreamEvent) (int64, error)
	// FindByEventID 按 event_id 查询；不存在返回 (nil, nil)。
	FindByEventID(ctx context.Context, eventID string) (*StreamEvent, error)
	// FindByReportID 按上报幂等键查询（ReportStreamState 重放路径）；不存在返回 (nil, nil)。
	FindByReportID(ctx context.Context, reportID string) (*StreamEvent, error)
	// FindByStreamSeq 按 (stream_id, seq) 精确回查（同态 no-op 时回放该序号的事件），
	// 不存在返回 (nil, nil)。
	FindByStreamSeq(ctx context.Context, streamID string, seq int64) (*StreamEvent, error)
	// MaxSeq 返回该流当前最大 seq（无事件时 0），用于对账与 seq 起点校验。
	MaxSeq(ctx context.Context, streamID string) (int64, error)
	// ListAfterSeq 按 seq 游标拉取事件（keyset 分页），limit 强制生效。
	ListAfterSeq(ctx context.Context, streamID string, afterSeq int64, limit int32, desc bool) ([]*StreamEvent, error)
	// ListByEventIDs 批量按 event_id 查询（Outbox 位点样本回显），最多 200 个。
	ListByEventIDs(ctx context.Context, eventIDs []string) ([]*StreamEvent, error)
	// Prune 归档终态流的旧事件（由 services/cron 调用），返回影响行数。
	Prune(ctx context.Context, before int64, limit int32) (int64, error)
}

type defaultStreamEventModel struct {
	conn sqlx.SqlConn
}

// NewStreamEventModel 创建 StreamEventModel 实现。
func NewStreamEventModel(conn sqlx.SqlConn) StreamEventModel {
	return &defaultStreamEventModel{conn: conn}
}

func (m *defaultStreamEventModel) Insert(ctx context.Context, tx sqlx.Session, e *StreamEvent) (int64, error) {
	session := pickSession(m.conn, tx)
	if e.Ctime == 0 {
		e.Ctime = nowUnix()
	}
	if e.OccurredAt == 0 {
		e.OccurredAt = e.Ctime
	}
	res, err := session.ExecCtx(ctx,
		"INSERT INTO live_stream_event (event_id, stream_id, room_id, session_id, seq, from_state, to_state, "+
			"node_id, interruption_id, interrupted_seconds, stop_reason, report_id, source, reason, occurred_at, trace_id, ctime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		e.EventID, e.StreamID, e.RoomID, e.SessionID, e.Seq, e.FromState, e.ToState,
		e.NodeID, e.InterruptionID, e.InterruptedSeconds, e.StopReason, nullableString(e.ReportID),
		e.Source, truncate(e.Reason, 255), e.OccurredAt, e.TraceID, e.Ctime)
	if err != nil {
		return 0, fmt.Errorf("live_stream_event Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_stream_event Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultStreamEventModel) FindByEventID(ctx context.Context, eventID string) (*StreamEvent, error) {
	var e StreamEvent
	query := "SELECT " + streamEventColumns + " FROM live_stream_event WHERE event_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &e, query, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_event FindByEventID: %w", err)
	}
	return &e, nil
}

func (m *defaultStreamEventModel) FindByReportID(ctx context.Context, reportID string) (*StreamEvent, error) {
	if reportID == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	var e StreamEvent
	query := "SELECT " + streamEventColumns + " FROM live_stream_event WHERE report_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &e, query, reportID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_event FindByReportID: %w", err)
	}
	return &e, nil
}

func (m *defaultStreamEventModel) FindByStreamSeq(ctx context.Context, streamID string, seq int64) (*StreamEvent, error) {
	if streamID == "" || seq <= 0 {
		return nil, nil
	}
	var e StreamEvent
	query := "SELECT " + streamEventColumns + " FROM live_stream_event WHERE stream_id = ? AND seq = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &e, query, streamID, seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_event FindByStreamSeq: %w", err)
	}
	return &e, nil
}

func (m *defaultStreamEventModel) MaxSeq(ctx context.Context, streamID string) (int64, error) {
	var seq sql.NullInt64
	err := m.conn.QueryRowCtx(ctx, &seq,
		"SELECT MAX(seq) FROM live_stream_event WHERE stream_id = ? LIMIT 1", streamID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_stream_event MaxSeq: %w", err)
	}
	if !seq.Valid {
		return 0, nil
	}
	return seq.Int64, nil
}

func (m *defaultStreamEventModel) ListAfterSeq(
	ctx context.Context,
	streamID string,
	afterSeq int64,
	limit int32,
	desc bool,
) ([]*StreamEvent, error) {
	order := "ASC"
	if desc {
		order = "DESC"
	}
	query := "SELECT " + streamEventColumns + " FROM live_stream_event WHERE stream_id = ? AND seq > ? " +
		"ORDER BY seq " + order + " LIMIT ?"
	var rows []*StreamEvent
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, streamID, afterSeq, clampLimit(limit, 500)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_event ListAfterSeq: %w", err)
	}
	return rows, nil
}

func (m *defaultStreamEventModel) ListByEventIDs(ctx context.Context, eventIDs []string) ([]*StreamEvent, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}
	if len(eventIDs) > 200 {
		eventIDs = eventIDs[:200]
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventIDs)), ",")
	query := "SELECT " + streamEventColumns + " FROM live_stream_event WHERE event_id IN (" + placeholders +
		") ORDER BY id ASC LIMIT ?"
	args := append(stringArgs(eventIDs), len(eventIDs))

	var rows []*StreamEvent
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_event ListByEventIDs: %w", err)
	}
	return rows, nil
}

func (m *defaultStreamEventModel) Prune(ctx context.Context, before int64, limit int32) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM live_stream_event WHERE occurred_at < ? ORDER BY id ASC LIMIT ?",
		before, clampLimit(limit, 1000))
	if err != nil {
		return 0, fmt.Errorf("live_stream_event Prune: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_stream_event Prune RowsAffected: %w", err)
	}
	return affected, nil
}

// nullableString 把空串映射为 NULL：MySQL 唯一索引允许多个 NULL，
// 因此「内部事件（无 report_id）」之间不会互相冲突，
// 而任何非空 report_id 仍然严格唯一。这是上报幂等的最终防线。
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
