package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveReplayTask 回放拼接任务（live_replay_task）。
//
// 关键边界（README 硬约束 1）：本表记录的是「拼接产物 + 它引用了哪些主键」，
// 不是稿件本身。COMPLETED 只表示「从 video 投影得知回放已可用」，
// 本服务不提供任何把回放或稿件写成已发布的入口：
// PENDING→…→REVIEW_SUBMITTED 由 Worker 上报，REVIEW_SUBMITTED→COMPLETED 只能由
// ApplyReplayContentState（video 事实投影）驱动。
// 产物只存引用：output_bucket/output_key，大文件不进 MySQL。
type LiveReplayTask struct {
	ReplayId     int64  `db:"replay_id"`
	RoomId       int64  `db:"room_id"`
	LiveSession  int64  `db:"live_session_id"`
	RecordId     int64  `db:"record_id"`     // 回放素材来源（录制任务）
	State        int32  `db:"state"`         // ReplayState*
	FromSeq      int64  `db:"from_seq"`      // 拼接区间起始切片序号
	ToSeq        int64  `db:"to_seq"`        // 拼接区间结束切片序号
	SegmentCount int64  `db:"segment_count"` // 实际参与拼接的有效切片数
	GapCount     int64  `db:"gap_count"`     // 区间内缺口数（allow_gaps=true 时才可能 > 0）
	StartAt      int64  `db:"start_at"`      // 回放覆盖区间起点（Unix 秒）
	EndAt        int64  `db:"end_at"`        // 终点（Unix 秒）
	DurationMs   int64  `db:"duration_ms"`   // 拼接后时长（毫秒）
	AllowGaps    int32  `db:"allow_gaps"`    // 0 不允许、1 允许缺口（tinyint）
	OutputBucket string `db:"output_bucket"`
	OutputKey    string `db:"output_key"`
	AssetId      int64  `db:"asset_id"`   // asset 主键引用（0 表示未登记）
	Aid          int64  `db:"aid"`        // video 稿件主键引用（0 表示未建稿）
	Bvid         string `db:"bvid"`       // 冗余展示字段，事实源仍是 video
	AnchorMid    int64  `db:"anchor_mid"` // 回放稿件归属主播 mid
	Title        string `db:"title"`
	Description  string `db:"description"`
	Version      int64  `db:"version"`
	Reason       int32  `db:"reason"` // rpc.FailureReason
	Errno        int32  `db:"errno"`
	ErrMsg       string `db:"err_msg"`
	RequestId    string `db:"request_id"` // 幂等键（唯一索引）
	TraceId      string `db:"trace_id"`
	Ctime        int64  `db:"ctime"`
	Mtime        int64  `db:"mtime"`
}

// ReplayTaskFilter List 过滤条件。
type ReplayTaskFilter struct {
	RoomId      int64
	SessionId   int64
	RecordId    int64
	State       int32
	AnchorMid   int64
	Pn          int32
	Ps          int32
	MaxPageSize int32
}

// ReplayPatch 状态推进时随带写入的字段；nil 表示不更新。
type ReplayPatch struct {
	State        int32 // 目标状态（必填）
	SegmentCount *int64
	GapCount     *int64
	DurationMs   *int64
	StartAt      *int64
	EndAt        *int64
	OutputBucket *string
	OutputKey    *string
	AssetId      *int64
	Aid          *int64
	Bvid         *string
	Reason       *int32
	Errno        *int32
	ErrMsg       *string
	TraceID      *string
}

func (p ReplayPatch) sets() []columnValue {
	sets := make([]columnValue, 0, 14)
	if p.SegmentCount != nil {
		sets = append(sets, columnValue{"segment_count", *p.SegmentCount})
	}
	if p.GapCount != nil {
		sets = append(sets, columnValue{"gap_count", *p.GapCount})
	}
	if p.DurationMs != nil {
		sets = append(sets, columnValue{"duration_ms", *p.DurationMs})
	}
	if p.StartAt != nil {
		sets = append(sets, columnValue{"start_at", *p.StartAt})
	}
	if p.EndAt != nil {
		sets = append(sets, columnValue{"end_at", *p.EndAt})
	}
	if p.OutputBucket != nil && *p.OutputBucket != "" {
		sets = append(sets, columnValue{"output_bucket", *p.OutputBucket})
	}
	if p.OutputKey != nil && *p.OutputKey != "" {
		sets = append(sets, columnValue{"output_key", *p.OutputKey})
	}
	if p.AssetId != nil {
		sets = append(sets, columnValue{"asset_id", *p.AssetId})
	}
	if p.Aid != nil {
		sets = append(sets, columnValue{"aid", *p.Aid})
	}
	if p.Bvid != nil && *p.Bvid != "" {
		sets = append(sets, columnValue{"bvid", *p.Bvid})
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
	if p.TraceID != nil && *p.TraceID != "" {
		sets = append(sets, columnValue{"trace_id", *p.TraceID})
	}
	return sets
}

// LiveReplayTaskModel live_replay_task 表接口。
type LiveReplayTaskModel interface {
	Insert(ctx context.Context, t *LiveReplayTask) (int64, error)
	// InsertTx 与 Insert 同语义，但写入跑在调用方事务里（任务行 + Outbox 同事务，AGENTS.md §5）。
	InsertTx(ctx context.Context, sess sqlx.Session, t *LiveReplayTask) (int64, error)
	FindOne(ctx context.Context, replayID int64) (*LiveReplayTask, error)
	FindByRequestID(ctx context.Context, requestID string) (*LiveReplayTask, error)
	// FindByRecordAndRange 查同一录制区间未完成的任务，供 SubmitReplayTask 复用，
	// 避免同一区间产出两份稿件。不存在返回 (nil, nil)。
	FindByRecordAndRange(ctx context.Context, recordID, fromSeq, toSeq int64) (*LiveReplayTask, error)
	// FindByAssetID 按媒资引用反查回放任务（投影同步与回收排障入口），不存在返回 (nil, nil)。
	FindByAssetID(ctx context.Context, assetID int64) (*LiveReplayTask, error)
	List(ctx context.Context, f ReplayTaskFilter) ([]*LiveReplayTask, int32, error)
	// UpdateState 条件 UPDATE 推进状态，返回受影响行数（0 行由调用方翻译成 NotFound/Conflict）。
	// 与 live_replay_asset_ref 的写入不带事务：BindReplayAsset 两步都靠唯一键幂等，
	// 中途失败由同一 request_id 重放收敛（README「已知缺口」有记录）。
	UpdateState(ctx context.Context, replayID int64, fromStates []int32, expectedVersion int64,
		patch ReplayPatch) (int64, error)
	// UpdateStateTx 与 UpdateState 同语义，但条件 UPDATE 跑在调用方事务里。
	UpdateStateTx(ctx context.Context, sess sqlx.Session, replayID int64, fromStates []int32,
		expectedVersion int64, patch ReplayPatch) (int64, error)
}

const liveReplayTaskColumns = "SELECT replay_id, room_id, live_session_id, record_id, state, from_seq, to_seq, " +
	"segment_count, gap_count, start_at, end_at, duration_ms, allow_gaps, output_bucket, output_key, asset_id, aid, " +
	"bvid, anchor_mid, title, description, version, reason, errno, err_msg, request_id, trace_id, ctime, mtime"

type defaultLiveReplayTaskModel struct {
	conn sqlx.SqlConn
}

// NewLiveReplayTaskModel 构造 live_replay_task 的 model。
func NewLiveReplayTaskModel(conn sqlx.SqlConn) LiveReplayTaskModel {
	return &defaultLiveReplayTaskModel{conn: conn}
}

func (m *defaultLiveReplayTaskModel) Insert(ctx context.Context, t *LiveReplayTask) (int64, error) {
	return m.InsertTx(ctx, m.conn, t)
}

func (m *defaultLiveReplayTaskModel) InsertTx(ctx context.Context, sess sqlx.Session,
	t *LiveReplayTask) (int64, error) {
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
	allow := int32(0)
	if t.AllowGaps != 0 {
		allow = 1
	}
	res, err := sess.ExecCtx(ctx,
		"INSERT INTO live_replay_task (room_id, live_session_id, record_id, state, from_seq, to_seq, "+
			"segment_count, gap_count, start_at, end_at, duration_ms, allow_gaps, output_bucket, output_key, "+
			"asset_id, aid, bvid, anchor_mid, title, description, version, reason, errno, err_msg, request_id, "+
			"trace_id, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, "+
			"?, ?, ?, ?, ?)",
		t.RoomId, t.LiveSession, t.RecordId, t.State, t.FromSeq, t.ToSeq, t.SegmentCount, t.GapCount,
		t.StartAt, t.EndAt, t.DurationMs, allow, t.OutputBucket, t.OutputKey, t.AssetId, t.Aid, t.Bvid,
		t.AnchorMid, t.Title, t.Description, t.Version, t.Reason, t.Errno, t.ErrMsg, t.RequestId, t.TraceId,
		t.Ctime, t.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			return 0, fmt.Errorf("request_id=%s: %w", t.RequestId, ErrRequestIdDuplicated)
		}
		return 0, fmt.Errorf("live_replay_task Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_replay_task Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveReplayTaskModel) FindOne(ctx context.Context, replayID int64) (*LiveReplayTask, error) {
	var t LiveReplayTask
	query := liveReplayTaskColumns + " FROM live_replay_task WHERE replay_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, replayID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_replay_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveReplayTaskModel) FindByRequestID(ctx context.Context, requestID string) (*LiveReplayTask, error) {
	var t LiveReplayTask
	query := liveReplayTaskColumns + " FROM live_replay_task WHERE request_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_replay_task FindByRequestID: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveReplayTaskModel) FindByRecordAndRange(ctx context.Context, recordID, fromSeq, toSeq int64) (*LiveReplayTask, error) {
	var t LiveReplayTask
	// 只匹配未终态任务：FAILED/CANCELLED 的区间允许重新提交（产出新任务与新稿件）。
	query := liveReplayTaskColumns + " FROM live_replay_task WHERE record_id = ? AND from_seq = ? AND to_seq = ? " +
		"AND state NOT IN (?, ?, ?) ORDER BY replay_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &t, query, recordID, fromSeq, toSeq,
		ReplayStateCompleted, ReplayStateFailed, ReplayStateCancelled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_replay_task FindByRecordAndRange: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveReplayTaskModel) FindByAssetID(ctx context.Context, assetID int64) (*LiveReplayTask, error) {
	var t LiveReplayTask
	// asset_id=0 是「尚未登记媒资」的占位值，不能当反查条件，否则会命中所有未登记任务。
	if assetID <= 0 {
		return nil, ErrInvalidAssetID
	}
	query := liveReplayTaskColumns + " FROM live_replay_task WHERE asset_id = ? ORDER BY replay_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &t, query, assetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_replay_task FindByAssetID: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveReplayTaskModel) List(ctx context.Context, f ReplayTaskFilter) ([]*LiveReplayTask, int32, error) {
	where, args := buildWhere(
		whereFragment{"room_id = ?", []any{}}.when(f.RoomId > 0, f.RoomId),
		whereFragment{"live_session_id = ?", []any{}}.when(f.SessionId > 0, f.SessionId),
		whereFragment{"record_id = ?", []any{}}.when(f.RecordId > 0, f.RecordId),
		whereFragment{"state = ?", []any{}}.when(f.State > 0, f.State),
		whereFragment{"anchor_mid = ?", []any{}}.when(f.AnchorMid > 0, f.AnchorMid),
	)
	limit, offset := clampPage(f.Pn, f.Ps, f.MaxPageSize)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_replay_task "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_replay_task List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveReplayTask
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveReplayTaskColumns+" FROM live_replay_task "+where+" ORDER BY replay_id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_replay_task List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultLiveReplayTaskModel) UpdateState(ctx context.Context, replayID int64, fromStates []int32,
	expectedVersion int64, patch ReplayPatch) (int64, error) {
	return m.UpdateStateTx(ctx, m.conn, replayID, fromStates, expectedVersion, patch)
}

func (m *defaultLiveReplayTaskModel) UpdateStateTx(ctx context.Context, sess sqlx.Session, replayID int64,
	fromStates []int32, expectedVersion int64, patch ReplayPatch) (int64, error) {
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_replay_task UpdateState: empty fromStates %w", ErrInvalidTransition)
	}
	sets := append(patch.sets(), columnValue{"state", patch.State})
	wheres := []whereFragment{
		{"replay_id = ?", []any{replayID}},
		stateInFragment(fromStates),
	}
	if expectedVersion > 0 {
		wheres = append(wheres, whereFragment{"version = ?", []any{expectedVersion}})
	}
	return conditionalUpdate(ctx, sess, "live_replay_task", sets, true, wheres)
}
