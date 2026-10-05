package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveRecordTask 录制任务（live_record_task）。
// 一条记录 = 「某场次的一次录制」，切片明细在 live_record_segment。
// last_seq 是断点续录的唯一锚点：Worker 重启后从 last_seq+1 继续，缺口必须显式登记 MISSING。
type LiveRecordTask struct {
	RecordId         int64  `db:"record_id"`
	RoomId           int64  `db:"room_id"`
	LiveSession      int64  `db:"live_session_id"`
	SourceTaskId     int64  `db:"source_task_id"`       // 录制源（转码任务）；0 表示原画源
	State            int32  `db:"state"`                // RecordState*
	StartAt          int64  `db:"start_at"`             // 期望起点（Unix 秒）
	EndAt            int64  `db:"end_at"`               // 期望终点（Unix 秒）
	RecordStartAt    int64  `db:"record_start_at"`      // 实际开始（Unix 秒）
	RecordEndAt      int64  `db:"record_end_at"`        // 实际结束（Unix 秒）
	SegmentSeconds   int32  `db:"segment_seconds"`      // 分片时长
	LastSeq          int64  `db:"last_seq"`             // 已登记的最大切片序号
	SegmentCount     int64  `db:"segment_count"`        // 已登记切片数（含缺口）
	GapCount         int64  `db:"gap_count"`            // 缺口（MISSING/CORRUPT）数
	RecordedDuration int64  `db:"recorded_duration_ms"` // 有效录制时长（毫秒，VERIFIED 求和）
	OutputBucket     string `db:"output_bucket"`
	OutputPrefix     string `db:"output_prefix"`
	HeartbeatAt      int64  `db:"heartbeat_at"`
	TimeoutAt        int64  `db:"timeout_at"`
	Version          int64  `db:"version"`
	Reason           int32  `db:"reason"`
	Errno            int32  `db:"errno"`
	ErrMsg           string `db:"err_msg"`
	RequestId        string `db:"request_id"`
	TraceId          string `db:"trace_id"`
	Ctime            int64  `db:"ctime"`
	Mtime            int64  `db:"mtime"`
}

// RecordTaskFilter List 过滤条件。
type RecordTaskFilter struct {
	RoomId      int64
	SessionId   int64
	State       int32
	Pn          int32
	Ps          int32
	MaxPageSize int32
}

// RecordPatch 状态推进时随带写入的字段；nil 表示不更新。
type RecordPatch struct {
	State         int32 // 目标状态（必填）
	LastSeq       *int64
	EndAt         *int64 // 期望终点快照（StopLiveRecord 下发停止指令时写入；与实际的 record_end_at 分离）
	HeartbeatAt   *int64
	TimeoutAt     *int64
	RecordStartAt *int64
	RecordEndAt   *int64
	Reason        *int32
	Errno         *int32
	ErrMsg        *string
	TraceID       *string
}

func (p RecordPatch) sets() []columnValue {
	sets := make([]columnValue, 0, 9)
	if p.LastSeq != nil {
		// GREATEST 保证 last_seq 单调：乱序/重放的上报不会把续录锚点往回退。
		sets = append(sets, columnValue{"last_seq", rawExpr{"GREATEST(last_seq, ?)", []any{*p.LastSeq}}})
	}
	if p.EndAt != nil {
		// 只覆盖「期望终点」：实际结束时刻 record_end_at 仍由 Worker 在最后一片落库后上报，
		// 两者混写会让回放误判区间已完整而尾部还在写。
		sets = append(sets, columnValue{"end_at", *p.EndAt})
	}
	if p.HeartbeatAt != nil {
		sets = append(sets, columnValue{"heartbeat_at", *p.HeartbeatAt})
	}
	if p.TimeoutAt != nil {
		sets = append(sets, columnValue{"timeout_at", *p.TimeoutAt})
	}
	if p.RecordStartAt != nil {
		sets = append(sets, columnValue{"record_start_at", *p.RecordStartAt})
	}
	if p.RecordEndAt != nil {
		sets = append(sets, columnValue{"record_end_at", *p.RecordEndAt})
	}
	if p.Reason != nil {
		sets = append(sets, columnValue{"reason", *p.Reason})
	}
	if p.Errno != nil {
		sets = append(sets, columnValue{"errno", *p.Errno})
	}
	if p.ErrMsg != nil {
		sets = append(sets, columnValue{"err_msg", *p.ErrMsg})
	}
	if p.TraceID != nil {
		sets = append(sets, columnValue{"trace_id", *p.TraceID})
	}
	return sets
}

// LiveRecordTaskModel live_record_task 表接口。
type LiveRecordTaskModel interface {
	Insert(ctx context.Context, t *LiveRecordTask) (int64, error)
	// InsertTx 与 Insert 同语义，但写入跑在调用方事务里（任务行 + Outbox 同事务，AGENTS.md §5）。
	InsertTx(ctx context.Context, sess sqlx.Session, t *LiveRecordTask) (int64, error)
	FindOne(ctx context.Context, recordID int64) (*LiveRecordTask, error)
	FindByRequestID(ctx context.Context, requestID string) (*LiveRecordTask, error)
	// FindActiveBySession 查某场次仍在录制中的任务（重复开播时复用，避免同场次双录）。
	FindActiveBySession(ctx context.Context, roomID, sessionID int64) (*LiveRecordTask, error)
	List(ctx context.Context, f RecordTaskFilter) ([]*LiveRecordTask, int32, error)
	// UpdateState 条件 UPDATE 推进状态（fromStates + expectedVersion），返回受影响行数。
	UpdateState(ctx context.Context, recordID int64, fromStates []int32, expectedVersion int64,
		patch RecordPatch) (int64, error)
	// UpdateStateTx 与 UpdateState 同语义，但条件 UPDATE 跑在调用方事务里。
	UpdateStateTx(ctx context.Context, sess sqlx.Session, recordID int64, fromStates []int32,
		expectedVersion int64, patch RecordPatch) (int64, error)
	// RefreshStats 用切片表重算 segment_count / gap_count / recorded_duration_ms，
	// 返回受影响行数。计数是派生值，不靠增量累加（重放与并发都会双计）。
	// last_seq 同步取 GREATEST(现值, 切片表 MAX(seq))：只随切片前进，不回退续录锚点。
	RefreshStats(ctx context.Context, recordID int64) (int64, error)
	// RefreshStatsTx 与 RefreshStats 同语义，但重算跑在调用方事务里（切片登记 + 计数收敛同提交）。
	RefreshStatsTx(ctx context.Context, sess sqlx.Session, recordID int64) (int64, error)
	// ListTimedOut 扫描超过 timeout_at 仍未终态的录制任务。
	ListTimedOut(ctx context.Context, now int64, limit int32) ([]*LiveRecordTask, error)
}

const liveRecordTaskColumns = "SELECT record_id, room_id, live_session_id, source_task_id, state, start_at, end_at, " +
	"record_start_at, record_end_at, segment_seconds, last_seq, segment_count, gap_count, recorded_duration_ms, " +
	"output_bucket, output_prefix, heartbeat_at, timeout_at, version, reason, errno, err_msg, request_id, trace_id, ctime, mtime"

type defaultLiveRecordTaskModel struct {
	conn sqlx.SqlConn
}

// NewLiveRecordTaskModel 构造 live_record_task 的 model。
func NewLiveRecordTaskModel(conn sqlx.SqlConn) LiveRecordTaskModel {
	return &defaultLiveRecordTaskModel{conn: conn}
}

func (m *defaultLiveRecordTaskModel) Insert(ctx context.Context, t *LiveRecordTask) (int64, error) {
	return m.InsertTx(ctx, m.conn, t)
}

func (m *defaultLiveRecordTaskModel) InsertTx(ctx context.Context, sess sqlx.Session,
	t *LiveRecordTask) (int64, error) {
	now := nowUnix()
	if t.Ctime == 0 {
		t.Ctime = now
	}
	if t.Mtime == 0 {
		t.Mtime = now
	}
	if t.Version == 0 {
		t.Version = 1
	}
	res, err := sess.ExecCtx(ctx,
		"INSERT INTO live_record_task (room_id, live_session_id, source_task_id, state, start_at, end_at, "+
			"record_start_at, record_end_at, segment_seconds, last_seq, segment_count, gap_count, "+
			"recorded_duration_ms, output_bucket, output_prefix, heartbeat_at, timeout_at, version, reason, errno, "+
			"err_msg, request_id, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0, 0, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)",
		t.RoomId, t.LiveSession, t.SourceTaskId, t.State, t.StartAt, t.EndAt, t.RecordStartAt, t.RecordEndAt,
		t.SegmentSeconds, t.OutputBucket, t.OutputPrefix, t.HeartbeatAt, t.TimeoutAt, t.Reason, t.Errno,
		t.ErrMsg, t.RequestId, t.TraceId, t.Ctime, t.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			return 0, fmt.Errorf("request_id=%s: %w", t.RequestId, ErrRequestIdDuplicated)
		}
		return 0, fmt.Errorf("live_record_task Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_record_task Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveRecordTaskModel) FindOne(ctx context.Context, recordID int64) (*LiveRecordTask, error) {
	var t LiveRecordTask
	if err := m.conn.QueryRowCtx(ctx, &t, liveRecordTaskColumns+" FROM live_record_task WHERE record_id = ?", recordID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_record_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveRecordTaskModel) FindByRequestID(ctx context.Context, requestID string) (*LiveRecordTask, error) {
	var t LiveRecordTask
	if err := m.conn.QueryRowCtx(ctx, &t, liveRecordTaskColumns+" FROM live_record_task WHERE request_id = ?", requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_record_task FindByRequestID: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveRecordTaskModel) FindActiveBySession(ctx context.Context, roomID, sessionID int64) (*LiveRecordTask, error) {
	var t LiveRecordTask
	query := liveRecordTaskColumns + " FROM live_record_task WHERE room_id = ? AND live_session_id = ? " +
		"AND state IN (?, ?, ?) ORDER BY record_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &t, query, roomID, sessionID,
		RecordStatePending, RecordStateRecording, RecordStateStopping); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_record_task FindActiveBySession: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveRecordTaskModel) List(ctx context.Context, f RecordTaskFilter) ([]*LiveRecordTask, int32, error) {
	where, args := buildWhere(
		whereFragment{"room_id = ?", []any{}}.when(f.RoomId > 0, f.RoomId),
		whereFragment{"live_session_id = ?", []any{}}.when(f.SessionId > 0, f.SessionId),
		whereFragment{"state = ?", []any{}}.when(f.State > 0, f.State),
	)
	limit, offset := clampPage(f.Pn, f.Ps, f.MaxPageSize)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_record_task "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_record_task List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveRecordTask
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveRecordTaskColumns+" FROM live_record_task "+where+" ORDER BY record_id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_record_task List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultLiveRecordTaskModel) UpdateState(ctx context.Context, recordID int64, fromStates []int32,
	expectedVersion int64, patch RecordPatch) (int64, error) {
	return m.UpdateStateTx(ctx, m.conn, recordID, fromStates, expectedVersion, patch)
}

func (m *defaultLiveRecordTaskModel) UpdateStateTx(ctx context.Context, sess sqlx.Session, recordID int64,
	fromStates []int32, expectedVersion int64, patch RecordPatch) (int64, error) {
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_record_task UpdateState: empty fromStates %w", ErrInvalidTransition)
	}
	sets := append(patch.sets(), columnValue{"state", patch.State})
	wheres := []whereFragment{
		{"record_id = ?", []any{recordID}},
		stateInFragment(fromStates),
	}
	if expectedVersion > 0 {
		wheres = append(wheres, whereFragment{"version = ?", []any{expectedVersion}})
	}
	return conditionalUpdate(ctx, sess, "live_record_task", sets, true, wheres)
}

func (m *defaultLiveRecordTaskModel) RefreshStats(ctx context.Context, recordID int64) (int64, error) {
	return m.RefreshStatsTx(ctx, m.conn, recordID)
}

func (m *defaultLiveRecordTaskModel) RefreshStatsTx(ctx context.Context, sess sqlx.Session,
	recordID int64) (int64, error) {
	res, err := sess.ExecCtx(ctx,
		"UPDATE live_record_task t SET "+
			"t.segment_count = (SELECT COUNT(*) FROM live_record_segment s WHERE s.record_id = t.record_id), "+
			"t.gap_count = (SELECT COUNT(*) FROM live_record_segment s WHERE s.record_id = t.record_id AND s.state IN (?, ?)), "+
			"t.recorded_duration_ms = (SELECT COALESCE(SUM(s.duration_ms), 0) FROM live_record_segment s WHERE s.record_id = t.record_id AND s.state = ?), "+
			// last_seq 与切片表同源，但只允许前进：Worker 的进度上报可能已把水位推到比
			// 已登记切片更大（切片异步落库中），重算不得把断点续录锚点往回退。
			"t.last_seq = GREATEST(t.last_seq, (SELECT COALESCE(MAX(s.seq), 0) FROM live_record_segment s WHERE s.record_id = t.record_id)), "+
			"t.mtime = ? WHERE t.record_id = ?",
		SegmentStateMissing, SegmentStateCorrupt, SegmentStateVerified, nowUnix(), recordID)
	if err != nil {
		return 0, fmt.Errorf("live_record_task RefreshStats: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_record_task RefreshStats RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultLiveRecordTaskModel) ListTimedOut(ctx context.Context, now int64, limit int32) ([]*LiveRecordTask, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []*LiveRecordTask
	query := liveRecordTaskColumns + " FROM live_record_task WHERE state IN (?, ?) AND timeout_at > 0 AND timeout_at < ? " +
		"ORDER BY timeout_at ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, RecordStateRecording, RecordStateStopping, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_record_task ListTimedOut: %w", err)
	}
	return rows, nil
}
