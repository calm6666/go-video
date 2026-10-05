package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// liveRoomStateLogColumns 与 000007_create_live_room_state_log.sql 逐列对应。
const liveRoomStateLogColumns = "log_id, room_id, session_id, state_type, from_state, to_state, " +
	"operator_mid, source, request_id, event_id, reason, trace_id, ctime"

// 状态迁移来源（live_room_state_log.source）：稳定字符串，供审计与看板归因。
// 新增来源要同时更新 README 的状态机矩阵说明，不允许写自由文本。
const (
	// SourceRPCClient 由终端用户经 gateway 触发的 RPC。
	SourceRPCClient = "rpc_client"
	// SourceRPCAdmin 由运营后台触发的 RPC。
	SourceRPCAdmin = "rpc_admin"
	// SourceStreamEvent 由 live.state.v1（live-ingest）事件推进。
	SourceStreamEvent = "stream_event"
	// SourceModerationResult 由 moderation.result.v1 结论推进。
	SourceModerationResult = "moderation_result"
	// SourceCron 由 services/cron 的到期补偿任务推进（禁播到期、断流超时兜底）。
	SourceCron = "cron"
)

// LiveRoomStateLog 状态流转日志行（live_room_state_log 表投影）。
//
// 本表是 append-only 的审计证据：任何房间/资料/场次/回放状态迁移都要留一行，
// 与状态迁移在**同一事务**内写入（AGENTS.md §8「删除、下架和版权撤回要保留审计证据」）。
// 不做物理删除，只由 services/cron 按 ctime 归档。
type LiveRoomStateLog struct {
	LogID       int64  `db:"log_id"`       // 日志 ID（主键）
	RoomID      int64  `db:"room_id"`      // 房间 ID
	SessionID   int64  `db:"session_id"`   // 关联场次 ID，0 表示与场次无关
	StateType   int32  `db:"state_type"`   // 见 LogType* 常量
	FromState   int32  `db:"from_state"`   // 迁移前状态值
	ToState     int32  `db:"to_state"`     // 迁移后状态值
	OperatorMid int64  `db:"operator_mid"` // 操作人，0 表示系统
	Source      string `db:"source"`       // 来源，见 Source* 常量
	RequestID   string `db:"request_id"`   // 触发本次迁移的幂等键
	EventID     string `db:"event_id"`     // 触发本次迁移的事件 ID
	Reason      string `db:"reason"`       // 迁移原因（运营内部说明，不下发终端）
	TraceID     string `db:"trace_id"`     // 链路追踪 ID
	Ctime       int64  `db:"ctime"`        // 记录时间（Unix 秒）
}

// LiveRoomStateLogModel live_room_state_log 表读写接口。
type LiveRoomStateLogModel interface {
	// Insert 追加一条流转日志，返回 log_id。
	Insert(ctx context.Context, l *LiveRoomStateLog) (int64, error)
	// InsertTx 在事务内追加日志，供与状态迁移同事务提交。
	InsertTx(ctx context.Context, session sqlx.Session, l *LiveRoomStateLog) (int64, error)
	// ListByRoom 读房间的状态流转历史（log_id 倒序，limit 截断），运营审计用。
	ListByRoom(ctx context.Context, roomID int64, stateType int32, limit int32) ([]*LiveRoomStateLog, error)
	// ListByTimeRange 按时间窗读全量流转日志（运营排查批量异常，limit 截断）。
	// 必须给时间范围：本表是高频写入的追加表，无界扫描会拖垮主库。
	ListByTimeRange(ctx context.Context, from, to int64, source string, limit int32) ([]*LiveRoomStateLog, error)
}

type defaultLiveRoomStateLogModel struct {
	conn sqlx.SqlConn
}

// NewLiveRoomStateLogModel 创建 LiveRoomStateLogModel 实现。
func NewLiveRoomStateLogModel(conn sqlx.SqlConn) LiveRoomStateLogModel {
	return &defaultLiveRoomStateLogModel{conn: conn}
}

func (m *defaultLiveRoomStateLogModel) Insert(ctx context.Context, l *LiveRoomStateLog) (int64, error) {
	return m.insert(ctx, m.conn, l)
}

func (m *defaultLiveRoomStateLogModel) InsertTx(ctx context.Context, session sqlx.Session, l *LiveRoomStateLog) (int64, error) {
	if session == nil {
		return m.Insert(ctx, l)
	}
	return m.insert(ctx, session, l)
}

func (m *defaultLiveRoomStateLogModel) insert(ctx context.Context, execer sqlx.Session, l *LiveRoomStateLog) (int64, error) {
	if l.RoomID <= 0 {
		return 0, ErrInvalidRoomID
	}
	switch l.StateType {
	case LogTypeRoomState, LogTypeVerifyState, LogTypeSessionState, LogTypeReplayState:
	default:
		return 0, ErrStateTypeInvalid
	}
	const query = "INSERT INTO live_room_state_log (room_id, session_id, state_type, from_state, to_state, " +
		"operator_mid, source, request_id, event_id, reason, trace_id, ctime) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	if l.Ctime == 0 {
		l.Ctime = nowUnix()
	}
	res, err := execer.ExecCtx(ctx, query,
		l.RoomID, l.SessionID, l.StateType, l.FromState, l.ToState,
		l.OperatorMid, l.Source, l.RequestID, l.EventID, l.Reason, l.TraceID, l.Ctime)
	if err != nil {
		return 0, fmt.Errorf("live_room_state_log Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_room_state_log Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveRoomStateLogModel) ListByRoom(ctx context.Context, roomID int64, stateType int32, limit int32) ([]*LiveRoomStateLog, error) {
	if roomID <= 0 {
		return nil, ErrInvalidRoomID
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	query := "SELECT " + liveRoomStateLogColumns + " FROM live_room_state_log WHERE room_id = ?"
	args := []interface{}{roomID}
	if stateType != 0 {
		query += " AND state_type = ?"
		args = append(args, stateType)
	}
	query += " ORDER BY log_id DESC LIMIT ?"
	args = append(args, limit)

	var rows []*LiveRoomStateLog
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("live_room_state_log ListByRoom: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveRoomStateLogModel) ListByTimeRange(ctx context.Context, from, to int64, source string, limit int32) ([]*LiveRoomStateLog, error) {
	if from <= 0 || to <= 0 || from > to {
		return nil, ErrQueryRangeRequired
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	var (
		sb   strings.Builder
		args []interface{}
	)
	sb.WriteString("ctime >= ? AND ctime <= ?")
	args = append(args, from, to)
	if source != "" {
		sb.WriteString(" AND source = ?")
		args = append(args, source)
	}
	query := "SELECT " + liveRoomStateLogColumns + " FROM live_room_state_log WHERE " + sb.String() +
		" ORDER BY log_id DESC LIMIT ?"
	args = append(args, limit)

	var rows []*LiveRoomStateLog
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("live_room_state_log ListByTimeRange: %w", err)
	}
	return rows, nil
}
