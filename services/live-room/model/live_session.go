package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// liveSessionColumns 与 000004_create_live_session.sql 逐列对应。
const liveSessionColumns = "session_id, room_id, mid, state, title_snapshot, area_id_snapshot, " +
	"stream_id, started_at, ended_at, duration_seconds, end_reason, last_stream_seq, " +
	"replay_state, record_id, record_asset_id, record_aid, moderation_task_id, trace_id, ctime, mtime"

// LiveSession 直播场次行（live_session 表投影，对应 rpc.SessionInfo）。
//
// 快照列（title_snapshot / area_id_snapshot）：开播那一刻的标题与分区，
// 之后改房间资料不得影响历史记录（AGENTS.md §8 审计留存）。
//
// 回放引用（record_id / record_asset_id / record_aid）：只存 live-media / asset / video
// 的业务主键，不复制媒资元数据，也不回查对方的库（AGENTS.md §5）。
// 因此本服务不需要独立的回放表：一场直播最多一条回放引用，随场次同生命周期。
type LiveSession struct {
	SessionID        int64  `db:"session_id"`         // 场次 ID（主键）
	RoomID           int64  `db:"room_id"`            // 房间 ID
	Mid              int64  `db:"mid"`                // 开播主播 ID
	State            int32  `db:"state"`              // 场次状态，见 SessionState* 常量
	TitleSnapshot    string `db:"title_snapshot"`     // 开播时标题快照
	AreaIDSnapshot   int64  `db:"area_id_snapshot"`   // 开播时分区快照
	StreamID         string `db:"stream_id"`          // live-ingest 推流标识引用（本服务不校验）
	StartedAt        int64  `db:"started_at"`         // 实际开播时间（Unix 秒），0 表示未开播
	EndedAt          int64  `db:"ended_at"`           // 结束时间（Unix 秒），0 表示进行中
	DurationSeconds  int64  `db:"duration_seconds"`   // 直播时长（秒）
	EndReason        int32  `db:"end_reason"`         // 终止原因，见 EndReason* 常量
	LastStreamSeq    int64  `db:"last_stream_seq"`    // 已应用的最大流事件序号（乱序守卫）
	ReplayState      int32  `db:"replay_state"`       // 回放状态，见 ReplayState* 常量
	RecordID         int64  `db:"record_id"`          // live-media 录制记录 ID 引用
	RecordAssetID    int64  `db:"record_asset_id"`    // 回放媒资 asset_id 引用
	RecordAid        int64  `db:"record_aid"`         // 回放稿件 aid 引用
	ModerationTaskID int64  `db:"moderation_task_id"` // 开播送审任务 ID，0 表示未送审
	TraceID          string `db:"trace_id"`           // 链路追踪 ID
	Ctime            int64  `db:"ctime"`              // 创建时间（Unix 秒）
	Mtime            int64  `db:"mtime"`              // 修改时间（Unix 秒）
}

// ActiveSessionStates 是「进行中场次」的状态集合（PENDING 已建档等推流、LIVING 直播中）。
// 房间是否 LIVING、CloseRoom/BanRoom 是否要强制终止场次，都以该集合为准。
var ActiveSessionStates = []int32{SessionStatePending, SessionStateLiving}

// SessionListModel 是 ListSessions 的查询条件。
type SessionListModel struct {
	RoomID          int64
	Mid             int64
	State           int32 // SessionStateUnspecified 表示不过滤
	BeforeSessionID int64 // cursor 解码后的游标，0 表示从头（最近一场）开始
	Limit           int32
}

// LiveSessionModel live_session 表读写接口。
type LiveSessionModel interface {
	// Insert 新建场次并返回 session_id。调用方必须把 state 置为 SessionStatePending、
	// replay_state 置为 ReplayStateNone、last_stream_seq 置 0。
	Insert(ctx context.Context, s *LiveSession) (int64, error)
	// InsertTx 在事务内新建场次，供「建档 + 房间投影 + 状态迁移 + 审计日志」同事务提交：
	// 场次离开事务就是孤儿行，会把该房间后续开播永久卡在「已有非终态场次」上。
	InsertTx(ctx context.Context, session sqlx.Session, s *LiveSession) (int64, error)
	// FindOne 按 session_id 读场次；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, sessionID int64) (*LiveSession, error)
	// FindActiveByRoom 读房间当前进行中场次（PENDING/LIVING），按 session_id 倒序取一条；
	// 无进行中场次返回 (nil, nil)。理论上最多一条，多条属数据异常，由 logic 记 Error 日志。
	FindActiveByRoom(ctx context.Context, roomID int64) (*LiveSession, error)
	// ListActiveByRoom 读房间进行中场次（最多取 2 条用于发现重复异常的自检）。
	ListActiveByRoom(ctx context.Context, roomID int64, limit int32) ([]*LiveSession, error)
	// FindLatest 取房间最近第 offset+1 场（session_id 倒序），offset 用于
	// GetSession 的「最近 N 场之一」；不存在返回 (nil, nil)。
	FindLatest(ctx context.Context, roomID int64, offset int32) (*LiveSession, error)
	// List 游标分页拉历史场次（session_id 倒序，Limit 截断）。
	List(ctx context.Context, q SessionListModel) ([]*LiveSession, error)
	// Transition 条件迁移场次状态：WHERE session_id=? AND state=from。
	// 进入终态时同一条 UPDATE 写 ended_at / end_reason / duration_seconds
	// （时长由 SQL 侧按 ended_at - started_at 计算），保证「时长簿记」与「终态」
	// 不分裂成两次写入；endedAt<=0 时取当前时间。迁移到 LIVING 用 GREATEST 补写
	// started_at，断流重连不会刷新开播时间。
	// 返回 false 表示 state 已被并发推进，调用方必须重读，不得当作成功。
	Transition(ctx context.Context, sessionID int64, from, to int32, endReason int32, endedAt int64) (bool, error)
	// TransitionTx 在事务内做同样的条件迁移，供与 live_room 状态迁移同事务提交。
	TransitionTx(ctx context.Context, session sqlx.Session, sessionID int64, from, to int32, endReason int32, endedAt int64) (bool, error)
	// SetStreamID 回填推流标识引用：仅当当前为空时写入（不覆盖已登记的流，
	// 避免重连事件把 stream_id 漂到另一条流上）。
	SetStreamID(ctx context.Context, sessionID int64, streamID string) (bool, error)
	// AdvanceStreamSeq 以「seq 严格大于已应用最大值」为条件写入场次状态迁移。
	// 返回 false 有两种含义（调用方需回查区分）：seq 陈旧，或 state 已不是 from。
	// 这是 live.state.v1 乱序守卫的唯一落库点。
	AdvanceStreamSeq(ctx context.Context, sessionID, seq int64, from, to int32, endReason int32, endedAt int64) (bool, error)
	// AdvanceStreamSeqTx 在事务内做同样的 seq 守卫迁移，供与 live_room 状态、
	// live_room_state_log 同事务提交（房间与场次投影必须原子变化）。
	AdvanceStreamSeqTx(ctx context.Context, session sqlx.Session, sessionID, seq int64,
		from, to int32, endReason int32, endedAt int64) (bool, error)
	// BumpStreamSeqTx 只推进 last_stream_seq、不改场次状态：用于「观测型」流事件
	// （推流心跳、未超宽限的中断、Idle）。条件仍是 last_stream_seq < seq，
	// 返回 false 表示该事件比已应用的序号更旧，调用方必须按乱序丢弃处理。
	BumpStreamSeqTx(ctx context.Context, session sqlx.Session, sessionID, seq int64) (bool, error)
	// AttachReplay 写入回放引用与目标回放状态：要求场次属于 room_id 且已终态，
	// 且 replay_state 满足 CanReplayTransition(from, to)。返回 false 表示条件未命中。
	AttachReplay(ctx context.Context, sessionID, roomID int64, fromReplay, toReplay int32,
		recordID, recordAssetID, recordAid int64) (bool, error)
	// SetModerationTaskID 回填开播送审任务 ID（仅当当前为 0 时写入）。
	SetModerationTaskID(ctx context.Context, sessionID, taskID int64) error
	// CountActive 统计全库进行中场次数（观测/配额用，非列表接口，仍带 LIMIT 语义）。
	CountActive(ctx context.Context) (int64, error)
}

type defaultLiveSessionModel struct {
	conn sqlx.SqlConn
}

// NewLiveSessionModel 创建 LiveSessionModel 实现。
func NewLiveSessionModel(conn sqlx.SqlConn) LiveSessionModel {
	return &defaultLiveSessionModel{conn: conn}
}

func (m *defaultLiveSessionModel) Insert(ctx context.Context, s *LiveSession) (int64, error) {
	return m.insert(ctx, m.conn, s)
}

func (m *defaultLiveSessionModel) InsertTx(ctx context.Context, session sqlx.Session, s *LiveSession) (int64, error) {
	if session == nil {
		return m.Insert(ctx, s)
	}
	return m.insert(ctx, session, s)
}

// insert 是场次建档的唯一 SQL 实现，execer 可以是连接或事务句柄。
func (m *defaultLiveSessionModel) insert(ctx context.Context, execer sqlx.Session, s *LiveSession) (int64, error) {
	if s.RoomID <= 0 {
		return 0, ErrInvalidRoomID
	}
	if s.Mid <= 0 {
		return 0, ErrInvalidMid
	}
	const query = "INSERT INTO live_session (room_id, mid, state, title_snapshot, area_id_snapshot, " +
		"stream_id, started_at, ended_at, duration_seconds, end_reason, last_stream_seq, " +
		"replay_state, record_id, record_asset_id, record_aid, moderation_task_id, trace_id, ctime, mtime) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	now := nowUnix()
	if s.Ctime == 0 {
		s.Ctime = now
	}
	s.Mtime = now
	res, err := execer.ExecCtx(ctx, query,
		s.RoomID, s.Mid, s.State, s.TitleSnapshot, s.AreaIDSnapshot,
		s.StreamID, s.StartedAt, s.EndedAt, s.DurationSeconds, s.EndReason, s.LastStreamSeq,
		s.ReplayState, s.RecordID, s.RecordAssetID, s.RecordAid, s.ModerationTaskID, s.TraceID,
		s.Ctime, s.Mtime)
	if err != nil {
		return 0, fmt.Errorf("live_session Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_session Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveSessionModel) FindOne(ctx context.Context, sessionID int64) (*LiveSession, error) {
	var s LiveSession
	query := "SELECT " + liveSessionColumns + " FROM live_session WHERE session_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &s, query, sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_session FindOne: %w", err)
	}
	return &s, nil
}

func (m *defaultLiveSessionModel) FindActiveByRoom(ctx context.Context, roomID int64) (*LiveSession, error) {
	list, err := m.ListActiveByRoom(ctx, roomID, 1)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

func (m *defaultLiveSessionModel) ListActiveByRoom(ctx context.Context, roomID int64, limit int32) ([]*LiveSession, error) {
	if roomID <= 0 {
		return nil, ErrInvalidRoomID
	}
	if limit <= 0 {
		limit = 1
	}
	query := "SELECT " + liveSessionColumns + " FROM live_session " +
		"WHERE room_id = ? AND state IN (" + placeholders(len(ActiveSessionStates)) + ") " +
		"ORDER BY session_id DESC LIMIT ?"
	args := []interface{}{roomID}
	for _, st := range ActiveSessionStates {
		args = append(args, st)
	}
	args = append(args, limit)

	var rows []*LiveSession
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_session ListActiveByRoom: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveSessionModel) FindLatest(ctx context.Context, roomID int64, offset int32) (*LiveSession, error) {
	if roomID <= 0 {
		return nil, ErrInvalidRoomID
	}
	if offset < 0 {
		offset = 0
	}
	query := "SELECT " + liveSessionColumns + " FROM live_session WHERE room_id = ? " +
		"ORDER BY session_id DESC LIMIT 1 OFFSET ?"
	var s LiveSession
	if err := m.conn.QueryRowCtx(ctx, &s, query, roomID, offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_session FindLatest: %w", err)
	}
	return &s, nil
}

func (m *defaultLiveSessionModel) List(ctx context.Context, q SessionListModel) ([]*LiveSession, error) {
	if q.RoomID <= 0 {
		return nil, ErrInvalidRoomID
	}
	if q.Limit <= 0 {
		q.Limit = defaultListLimit
	}
	var (
		sb   strings.Builder
		args []interface{}
	)
	sb.WriteString("room_id = ?")
	args = append(args, q.RoomID)
	if q.Mid > 0 {
		sb.WriteString(" AND mid = ?")
		args = append(args, q.Mid)
	}
	if q.State != SessionStateUnspecified {
		sb.WriteString(" AND state = ?")
		args = append(args, q.State)
	}
	if q.BeforeSessionID > 0 {
		sb.WriteString(" AND session_id < ?")
		args = append(args, q.BeforeSessionID)
	}
	query := "SELECT " + liveSessionColumns + " FROM live_session WHERE " + sb.String() +
		" ORDER BY session_id DESC LIMIT ?"
	args = append(args, q.Limit)

	var rows []*LiveSession
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_session List: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveSessionModel) Transition(ctx context.Context, sessionID int64, from, to int32, endReason int32, endedAt int64) (bool, error) {
	return m.transition(ctx, m.conn, sessionID, from, to, endReason, endedAt)
}

func (m *defaultLiveSessionModel) TransitionTx(ctx context.Context, session sqlx.Session, sessionID int64, from, to int32, endReason int32, endedAt int64) (bool, error) {
	if session == nil {
		return m.Transition(ctx, sessionID, from, to, endReason, endedAt)
	}
	return m.transition(ctx, session, sessionID, from, to, endReason, endedAt)
}

// transition 是场次状态迁移的唯一 SQL 实现，execer 可以是连接或事务句柄。
func (m *defaultLiveSessionModel) transition(ctx context.Context, execer sqlx.Session, sessionID int64, from, to int32, endReason int32, endedAt int64) (bool, error) {
	if !CanSessionTransition(from, to) {
		return false, ErrInvalidSessionTransition
	}
	set := []string{"state = ?", "mtime = ?"}
	args := []interface{}{to, nowUnix()}

	switch {
	case SessionStateIsTerminal(to):
		// 终态必须带终止原因：ENDED/TERMINATED 而无原因会让审计无法解释这一场怎么没的。
		if endReason == EndReasonUnspecified {
			return false, ErrEndReasonInvalid
		}
		if endedAt <= 0 {
			endedAt = nowUnix()
		}
		// 时长在 SQL 侧算（ended_at - started_at）：应用实例时钟有漂移时，
		// 由调用方传 duration 会写出「started_at/ended_at 与 duration 三者不自洽」的行。
		set = append(set, "ended_at = ?", "end_reason = ?", "duration_seconds = GREATEST(? - started_at, 0)")
		args = append(args, endedAt, endReason, endedAt)
	case to == SessionStateLiving:
		// PENDING→LIVING：只在 started_at 还没写过（=0）时落开播时间，
		// 断流重连回到 LIVING 不得刷新开播时间，否则时长被算短。
		set = append(set, "started_at = GREATEST(started_at, ?)")
		args = append(args, nowUnix())
	}

	query := "UPDATE live_session SET " + strings.Join(set, ", ") + " WHERE session_id = ? AND state = ?"
	args = append(args, sessionID, from)

	res, err := execer.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_session Transition: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_session Transition RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveSessionModel) SetStreamID(ctx context.Context, sessionID int64, streamID string) (bool, error) {
	if streamID == "" {
		return false, ErrStreamRefMismatch
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_session SET stream_id = ?, mtime = ? WHERE session_id = ? AND stream_id = ''",
		streamID, nowUnix(), sessionID)
	if err != nil {
		return false, fmt.Errorf("live_session SetStreamID: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_session SetStreamID RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveSessionModel) AdvanceStreamSeq(ctx context.Context, sessionID, seq int64,
	from, to int32, endReason int32, endedAt int64) (bool, error) {
	return m.advanceStreamSeq(ctx, m.conn, sessionID, seq, from, to, endReason, endedAt)
}

func (m *defaultLiveSessionModel) AdvanceStreamSeqTx(ctx context.Context, session sqlx.Session,
	sessionID, seq int64, from, to int32, endReason int32, endedAt int64) (bool, error) {
	if session == nil {
		return m.AdvanceStreamSeq(ctx, sessionID, seq, from, to, endReason, endedAt)
	}
	return m.advanceStreamSeq(ctx, session, sessionID, seq, from, to, endReason, endedAt)
}

// advanceStreamSeq 是 seq 守卫迁移的唯一 SQL 实现，execer 可以是连接或事务句柄。
func (m *defaultLiveSessionModel) advanceStreamSeq(ctx context.Context, execer sqlx.Session,
	sessionID, seq int64, from, to int32, endReason int32, endedAt int64) (bool, error) {
	if !CanSessionTransition(from, to) {
		return false, ErrInvalidSessionTransition
	}
	if seq <= 0 {
		return false, ErrStreamSeqStale
	}
	set := []string{"last_stream_seq = ?", "state = ?", "mtime = ?"}
	args := []interface{}{seq, to, nowUnix()}
	if SessionStateIsTerminal(to) {
		if endReason == EndReasonUnspecified {
			return false, ErrEndReasonInvalid
		}
		if endedAt <= 0 {
			endedAt = nowUnix()
		}
		set = append(set, "ended_at = ?", "end_reason = ?", "duration_seconds = GREATEST(? - started_at, 0)")
		args = append(args, endedAt, endReason, endedAt)
	}
	query := "UPDATE live_session SET " + strings.Join(set, ", ") +
		" WHERE session_id = ? AND state = ? AND last_stream_seq < ?"
	args = append(args, sessionID, from, seq)

	res, err := execer.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_session AdvanceStreamSeq: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_session AdvanceStreamSeq RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveSessionModel) BumpStreamSeqTx(ctx context.Context, session sqlx.Session,
	sessionID, seq int64) (bool, error) {
	if sessionID <= 0 {
		return false, ErrInvalidSessionID
	}
	if seq <= 0 {
		return false, ErrStreamSeqStale
	}
	execer := sqlx.Session(session)
	if execer == nil {
		execer = m.conn
	}
	res, err := execer.ExecCtx(ctx,
		"UPDATE live_session SET last_stream_seq = ?, mtime = ? WHERE session_id = ? AND last_stream_seq < ?",
		seq, nowUnix(), sessionID, seq)
	if err != nil {
		return false, fmt.Errorf("live_session BumpStreamSeq: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_session BumpStreamSeq RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveSessionModel) AttachReplay(ctx context.Context, sessionID, roomID int64, fromReplay, toReplay int32,
	recordID, recordAssetID, recordAid int64) (bool, error) {
	if sessionID <= 0 {
		return false, ErrInvalidSessionID
	}
	if roomID <= 0 {
		return false, ErrInvalidRoomID
	}
	if !CanReplayTransition(fromReplay, toReplay) {
		return false, ErrInvalidReplayTransition
	}
	set := []string{"replay_state = ?", "mtime = ?"}
	args := []interface{}{toReplay, nowUnix()}
	// 引用列 0 表示「不修改」：回放是逐步补全的（先 record_id，再 asset_id，最后 aid）。
	if recordID > 0 {
		set = append(set, "record_id = ?")
		args = append(args, recordID)
	}
	if recordAssetID > 0 {
		set = append(set, "record_asset_id = ?")
		args = append(args, recordAssetID)
	}
	if recordAid > 0 {
		set = append(set, "record_aid = ?")
		args = append(args, recordAid)
	}
	// 归属校验写在 WHERE 里（room_id + 终态 + 当前回放状态），
	// 不靠「先读再判」，否则跨房间串改在并发下能穿过检查。
	query := "UPDATE live_session SET " + strings.Join(set, ", ") +
		" WHERE session_id = ? AND room_id = ? AND state IN (" + placeholders(len(terminalSessionStates)) + ")" +
		" AND replay_state = ?"
	args = append(args, sessionID, roomID)
	for _, st := range terminalSessionStates {
		args = append(args, st)
	}
	args = append(args, fromReplay)

	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_session AttachReplay: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_session AttachReplay RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveSessionModel) SetModerationTaskID(ctx context.Context, sessionID, taskID int64) error {
	if taskID <= 0 {
		return ErrTaskMismatch
	}
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE live_session SET moderation_task_id = ?, mtime = ? WHERE session_id = ? AND moderation_task_id = 0",
		taskID, nowUnix(), sessionID)
	if err != nil {
		return fmt.Errorf("live_session SetModerationTaskID: %w", err)
	}
	return nil
}

func (m *defaultLiveSessionModel) CountActive(ctx context.Context) (int64, error) {
	query := "SELECT COUNT(*) FROM live_session WHERE state IN (" + placeholders(len(ActiveSessionStates)) + ")"
	args := make([]interface{}, 0, len(ActiveSessionStates))
	for _, st := range ActiveSessionStates {
		args = append(args, st)
	}
	var cnt int64
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_session CountActive: %w", err)
	}
	return cnt, nil
}

// terminalSessionStates 是场次终态集合（AttachReplay 的前置条件）。
var terminalSessionStates = []int32{SessionStateEnded, SessionStateTerminated}
