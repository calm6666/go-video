package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// interruptionColumns 是 live_stream_interruption 的列清单，必须与
// deploy/migrations/live-ingest/000002_create_live_stream_tables.sql 完全一致。
const interruptionColumns = "interruption_id, stream_id, room_id, episode_no, node_id, started_at, ended_at, " +
	"duration_seconds, end_reason, reconnect_attempts, start_event_id, end_event_id, reason, ctime, mtime"

// StreamInterruption 断流与重连记录（live_stream_interruption 表投影）。
//
// 一行 = 一次「进入 INTERRUPTED → 离开 INTERRUPTED」的区间：
//   - 开启：INTERRUPTED 迁移成功后由同一事务 Insert（start_event_id 唯一，重放不重复开）；
//   - 关闭：INTERRUPTED → PUBLISHING/STOPPED 时 Close（end_reason 区分重连成功/超时/主动停流）；
//   - ended_at = 0 表示仍在中断中，宽限期耗尽由后台扫描器补偿关闭。
type StreamInterruption struct {
	InterruptionID    int64  `db:"interruption_id"`    // 自增主键
	StreamID          string `db:"stream_id"`          // 所属流
	RoomID            int64  `db:"room_id"`            // 房间引用
	EpisodeNo         int32  `db:"episode_no"`         // 该流第几次断流，从 1 递增
	NodeID            string `db:"node_id"`            // 断流时的接入节点
	StartedAt         int64  `db:"started_at"`         // 断流开始（Unix 秒）
	EndedAt           int64  `db:"ended_at"`           // 断流结束（Unix 秒），0 表示进行中
	DurationSeconds   int64  `db:"duration_seconds"`   // 本次中断时长（秒）
	EndReason         int32  `db:"end_reason"`         // 结束原因，见 InterruptionEnd*
	ReconnectAttempts int32  `db:"reconnect_attempts"` // 期间重连尝试次数
	StartEventID      string `db:"start_event_id"`     // 开启记录的 live.state.v1 event_id
	EndEventID        string `db:"end_event_id"`       // 关闭记录的 event_id，空串表示未关闭
	Reason            string `db:"reason"`             // 原因摘要
	Ctime             int64  `db:"ctime"`              // 创建时间（Unix 秒）
	Mtime             int64  `db:"mtime"`              // 修改时间（Unix 秒）
}

// InterruptionFilter 断流记录查询条件；零值表示不过滤。
type InterruptionFilter struct {
	StreamID   string
	RoomID     int64
	OnlyOpen   bool
	StartTime  int64
	EndTime    int64
	MaxResults int32
}

// StreamInterruptionModel live_stream_interruption 表查询与写入接口。
type StreamInterruptionModel interface {
	// Insert 开启一条断流记录并返回 ID；uniq_start_event 冲突表示该事件已开过区间。
	Insert(ctx context.Context, tx sqlx.Session, in *StreamInterruption) (int64, error)
	// FindOpenByStream 返回该流当前未结束的断流记录；不存在返回 (nil, nil)。
	FindOpenByStream(ctx context.Context, streamID string) (*StreamInterruption, error)
	// FindByID 按记录 ID 查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, id int64) (*StreamInterruption, error)
	// NextEpisodeNo 返回该流的下一个断流序号（现有最大值 + 1）。
	NextEpisodeNo(ctx context.Context, tx sqlx.Session, streamID string) (int32, error)
	// Close 结束区间：仅当记录仍未结束时生效（CAS on ended_at = 0）。
	Close(ctx context.Context, tx sqlx.Session, id, endedAt, durationSeconds, endReason int64, endEventID, reason string) (bool, error)
	// IncrReconnectAttempts 累加重连尝试次数（鉴权重试但未成功时调用）。
	IncrReconnectAttempts(ctx context.Context, tx sqlx.Session, streamID string) (bool, error)
	// ListByFilter 分页查询断流记录，按 started_at 升序，limit 强制生效。
	ListByFilter(ctx context.Context, f InterruptionFilter) ([]*StreamInterruption, error)
	// CountByFilter 统计条件命中行数（total，上限 CountHardLimit，超出返回 -1）。
	CountByFilter(ctx context.Context, f InterruptionFilter, hardLimit int64) (int64, error)
}

type defaultStreamInterruptionModel struct {
	conn sqlx.SqlConn
}

// NewStreamInterruptionModel 创建 StreamInterruptionModel 实现。
func NewStreamInterruptionModel(conn sqlx.SqlConn) StreamInterruptionModel {
	return &defaultStreamInterruptionModel{conn: conn}
}

func (m *defaultStreamInterruptionModel) Insert(ctx context.Context, tx sqlx.Session, in *StreamInterruption) (int64, error) {
	session := pickSession(m.conn, tx)
	if in.Ctime == 0 {
		in.Ctime = nowUnix()
	}
	in.Mtime = in.Ctime
	res, err := session.ExecCtx(ctx,
		"INSERT INTO live_stream_interruption (stream_id, room_id, episode_no, node_id, started_at, ended_at, "+
			"duration_seconds, end_reason, reconnect_attempts, start_event_id, end_event_id, reason, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, 0, 0, 0, ?, ?, '', ?, ?, ?)",
		in.StreamID, in.RoomID, in.EpisodeNo, in.NodeID, in.StartedAt, in.ReconnectAttempts,
		in.StartEventID, truncate(in.Reason, 255), in.Ctime, in.Mtime)
	if err != nil {
		return 0, fmt.Errorf("live_stream_interruption Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_stream_interruption Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultStreamInterruptionModel) FindOpenByStream(ctx context.Context, streamID string) (*StreamInterruption, error) {
	var in StreamInterruption
	query := "SELECT " + interruptionColumns + " FROM live_stream_interruption " +
		"WHERE stream_id = ? AND ended_at = 0 ORDER BY started_at DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &in, query, streamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_interruption FindOpenByStream: %w", err)
	}
	return &in, nil
}

func (m *defaultStreamInterruptionModel) FindByID(ctx context.Context, id int64) (*StreamInterruption, error) {
	var in StreamInterruption
	query := "SELECT " + interruptionColumns + " FROM live_stream_interruption WHERE interruption_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &in, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_interruption FindByID: %w", err)
	}
	return &in, nil
}

func (m *defaultStreamInterruptionModel) NextEpisodeNo(ctx context.Context, tx sqlx.Session, streamID string) (int32, error) {
	session := pickSession(m.conn, tx)
	var maxNo sql.NullInt64
	err := session.QueryRowCtx(ctx, &maxNo,
		"SELECT MAX(episode_no) FROM live_stream_interruption WHERE stream_id = ? LIMIT 1", streamID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 1, nil
		}
		return 0, fmt.Errorf("live_stream_interruption NextEpisodeNo: %w", err)
	}
	if !maxNo.Valid {
		return 1, nil
	}
	return int32(maxNo.Int64 + 1), nil
}

func (m *defaultStreamInterruptionModel) Close(
	ctx context.Context,
	tx sqlx.Session,
	id, endedAt, durationSeconds, endReason int64,
	endEventID, reason string,
) (bool, error) {
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_stream_interruption SET ended_at = ?, duration_seconds = ?, end_reason = ?, "+
			"end_event_id = ?, reason = CONCAT(reason, IF(reason = '', '', ' | '), ?), mtime = ? "+
			"WHERE interruption_id = ? AND ended_at = 0",
		endedAt, durationSeconds, endReason, endEventID, truncate(reason, 128), nowUnix(), id)
	if err != nil {
		return false, fmt.Errorf("live_stream_interruption Close: %w", err)
	}
	return rowsAffected(res, "live_stream_interruption Close")
}

func (m *defaultStreamInterruptionModel) IncrReconnectAttempts(ctx context.Context, tx sqlx.Session, streamID string) (bool, error) {
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_stream_interruption SET reconnect_attempts = reconnect_attempts + 1, mtime = ? "+
			"WHERE stream_id = ? AND ended_at = 0 ORDER BY started_at DESC LIMIT 1",
		nowUnix(), streamID)
	if err != nil {
		return false, fmt.Errorf("live_stream_interruption IncrReconnectAttempts: %w", err)
	}
	return rowsAffected(res, "live_stream_interruption IncrReconnectAttempts")
}

func (m *defaultStreamInterruptionModel) ListByFilter(ctx context.Context, f InterruptionFilter) ([]*StreamInterruption, error) {
	where, args := f.build()
	query := "SELECT " + interruptionColumns + " FROM live_stream_interruption WHERE " + where +
		" ORDER BY started_at ASC LIMIT ?"
	args = append(args, clampLimit(f.MaxResults, 500))

	var rows []*StreamInterruption
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_interruption ListByFilter: %w", err)
	}
	return rows, nil
}

func (m *defaultStreamInterruptionModel) CountByFilter(ctx context.Context, f InterruptionFilter, hardLimit int64) (int64, error) {
	where, args := f.build()
	if hardLimit <= 0 {
		hardLimit = 10000
	}
	var cnt int64
	query := "SELECT COUNT(*) FROM (SELECT interruption_id FROM live_stream_interruption WHERE " + where +
		" ORDER BY interruption_id ASC LIMIT ?) t"
	args = append(args, hardLimit)
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_stream_interruption CountByFilter: %w", err)
	}
	if cnt >= hardLimit {
		// 达到硬上限时不谎报精确值，调用方按「至少这么多」处理。
		return -1, nil
	}
	return cnt, nil
}

// build 组装 WHERE 片段与参数；恒真条件用 episode_no <> 0 占位。
func (f InterruptionFilter) build() (string, []interface{}) {
	conds := []string{"episode_no <> 0"}
	args := make([]interface{}, 0, 5)
	if f.StreamID != "" {
		conds = append(conds, "stream_id = ?")
		args = append(args, f.StreamID)
	}
	if f.RoomID > 0 {
		conds = append(conds, "room_id = ?")
		args = append(args, f.RoomID)
	}
	if f.OnlyOpen {
		conds = append(conds, "ended_at = 0")
	}
	if f.StartTime > 0 {
		conds = append(conds, "started_at >= ?")
		args = append(args, f.StartTime)
	}
	if f.EndTime > 0 {
		conds = append(conds, "started_at <= ?")
		args = append(args, f.EndTime)
	}
	return strings.Join(conds, " AND "), args
}
