package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveRetentionTask 回收任务（live_retention_task）。
//
// 回收是「先登记意图、再执行、最后留计数证据」的三步流程（AGENTS.md §8 审计证据）：
// 禁止边查边删——那样一旦删错就无法回答「谁在什么时候为什么删了哪些对象」。
// purge=false 时本表只记录标记动作（deleted 恒为 0），真删对象存储由 Worker 调
// internal/repository 的 Storage 接口完成（本轮为显式 stub，见 README 已知缺口）。
type LiveRetentionTask struct {
	RetentionId  int64  `db:"retention_id"`
	TargetKind   int32  `db:"target_kind"`   // RetentionTarget*
	RoomId       int64  `db:"room_id"`       // 0 表示全局扫描
	TargetId     int64  `db:"target_id"`     // 指定回收对象主键（0 表示按 expire_before 批量）
	ExpireBefore int64  `db:"expire_before"` // 只回收该时刻（Unix 秒）之前到期的对象
	Purge        int32  `db:"purge"`         // 0 只登记标记、1 真删产物
	BatchLimit   int32  `db:"batch_limit"`   // 单次处理上限（服务端夹取）
	State        int32  `db:"state"`         // RetentionState*
	Scanned      int32  `db:"scanned"`       // 扫描命中行数
	Deleted      int32  `db:"deleted"`       // 实际删除行数（purge=0 时恒为 0）
	Skipped      int32  `db:"skipped"`       // 跳过行数（仍被引用/状态不允许）
	Reason       string `db:"reason"`        // 回收原因（审计必填：超期/切片被回放吸收/房间删除）
	Operator     string `db:"operator"`      // 提交者（system/运营账号）
	Version      int64  `db:"version"`
	FailReason   int32  `db:"fail_reason"` // rpc.FailureReason
	Errno        int32  `db:"errno"`
	ErrMsg       string `db:"err_msg"`
	RequestId    string `db:"request_id"` // 幂等键（唯一索引）
	TraceId      string `db:"trace_id"`
	Ctime        int64  `db:"ctime"`
	Mtime        int64  `db:"mtime"`
}

// RetentionFilter List 过滤条件。
type RetentionFilter struct {
	TargetKind  int32
	State       int32
	RoomId      int64
	Pn          int32
	Ps          int32
	MaxPageSize int32
}

// RetentionPatch 状态推进时随带写入的字段；nil 表示不更新。
// 计数用「覆盖写」而不是累加：Worker 每次上报的是本次执行的完整结果，
// 累加会让重放把 deleted 翻倍，覆盖写天然幂等。
type RetentionPatch struct {
	State      int32 // 目标状态（必填）
	Scanned    *int32
	Deleted    *int32
	Skipped    *int32
	FailReason *int32
	Errno      *int32
	ErrMsg     *string
	TraceID    *string
}

func (p RetentionPatch) sets() []columnValue {
	sets := make([]columnValue, 0, 7)
	if p.Scanned != nil {
		sets = append(sets, columnValue{"scanned", *p.Scanned})
	}
	if p.Deleted != nil {
		sets = append(sets, columnValue{"deleted", *p.Deleted})
	}
	if p.Skipped != nil {
		sets = append(sets, columnValue{"skipped", *p.Skipped})
	}
	if p.FailReason != nil {
		sets = append(sets, columnValue{"fail_reason", *p.FailReason})
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

// LiveRetentionTaskModel live_retention_task 表接口。
type LiveRetentionTaskModel interface {
	Insert(ctx context.Context, t *LiveRetentionTask) (int64, error)
	// InsertTx 与 Insert 同语义，但写入跑在调用方事务里（回收意图 + Outbox 同事务，AGENTS.md §5/§8）。
	InsertTx(ctx context.Context, sess sqlx.Session, t *LiveRetentionTask) (int64, error)
	FindOne(ctx context.Context, retentionID int64) (*LiveRetentionTask, error)
	FindByRequestID(ctx context.Context, requestID string) (*LiveRetentionTask, error)
	// FindUnfinishedByTarget 同一对象已有未完成的回收任务时复用它，避免排队两个删除任务。
	// targetID<=0 的批量任务不参与该判定（它们的集合语义可以重叠）。不存在返回 (nil, nil)。
	FindUnfinishedByTarget(ctx context.Context, targetKind, targetID int64) (*LiveRetentionTask, error)
	List(ctx context.Context, f RetentionFilter) ([]*LiveRetentionTask, int32, error)
	// ListByState 按状态取待执行任务（Worker 领取队列），按 retention_id 升序保证先登记先执行。
	ListByState(ctx context.Context, state int32, limit int32) ([]*LiveRetentionTask, error)
	// UpdateState 条件 UPDATE 推进状态与结果计数，返回受影响行数。
	UpdateState(ctx context.Context, retentionID int64, fromStates []int32, expectedVersion int64,
		patch RetentionPatch) (int64, error)
	// UpdateStateTx 与 UpdateState 同语义，但条件 UPDATE 跑在调用方事务里。
	UpdateStateTx(ctx context.Context, sess sqlx.Session, retentionID int64, fromStates []int32,
		expectedVersion int64, patch RetentionPatch) (int64, error)
}

const liveRetentionTaskColumns = "SELECT retention_id, target_kind, room_id, target_id, expire_before, purge, " +
	"batch_limit, state, scanned, deleted, skipped, reason, operator, version, fail_reason, errno, err_msg, " +
	"request_id, trace_id, ctime, mtime"

type defaultLiveRetentionTaskModel struct {
	conn sqlx.SqlConn
}

// NewLiveRetentionTaskModel 构造 live_retention_task 的 model。
func NewLiveRetentionTaskModel(conn sqlx.SqlConn) LiveRetentionTaskModel {
	return &defaultLiveRetentionTaskModel{conn: conn}
}

func (m *defaultLiveRetentionTaskModel) Insert(ctx context.Context, t *LiveRetentionTask) (int64, error) {
	return m.InsertTx(ctx, m.conn, t)
}

func (m *defaultLiveRetentionTaskModel) InsertTx(ctx context.Context, sess sqlx.Session,
	t *LiveRetentionTask) (int64, error) {
	if !ValidRetentionTarget(t.TargetKind) {
		return 0, fmt.Errorf("live_retention_task Insert: target_kind=%d %w", t.TargetKind, ErrInvalidTransition)
	}
	if t.TargetId <= 0 && t.ExpireBefore <= 0 {
		return 0, ErrRetentionTargetRequired
	}
	if t.Reason == "" {
		return 0, ErrRetentionReasonRequired
	}
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
	if t.State == 0 {
		t.State = RetentionStatePending
	}
	purge := int32(0)
	if t.Purge != 0 {
		purge = 1
	}
	res, err := sess.ExecCtx(ctx,
		"INSERT INTO live_retention_task (target_kind, room_id, target_id, expire_before, purge, batch_limit, "+
			"state, scanned, deleted, skipped, reason, operator, version, fail_reason, errno, err_msg, request_id, "+
			"trace_id, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.TargetKind, t.RoomId, t.TargetId, t.ExpireBefore, purge, t.BatchLimit, t.State, t.Reason, t.Operator,
		t.Version, t.FailReason, t.Errno, t.ErrMsg, t.RequestId, t.TraceId, t.Ctime, t.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			return 0, fmt.Errorf("request_id=%s: %w", t.RequestId, ErrRequestIdDuplicated)
		}
		return 0, fmt.Errorf("live_retention_task Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_retention_task Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveRetentionTaskModel) FindOne(ctx context.Context, retentionID int64) (*LiveRetentionTask, error) {
	var t LiveRetentionTask
	query := liveRetentionTaskColumns + " FROM live_retention_task WHERE retention_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, retentionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_retention_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveRetentionTaskModel) FindByRequestID(ctx context.Context, requestID string) (*LiveRetentionTask, error) {
	var t LiveRetentionTask
	query := liveRetentionTaskColumns + " FROM live_retention_task WHERE request_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_retention_task FindByRequestID: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveRetentionTaskModel) FindUnfinishedByTarget(ctx context.Context, targetKind, targetID int64) (*LiveRetentionTask, error) {
	if targetID <= 0 {
		return nil, nil
	}
	var t LiveRetentionTask
	query := liveRetentionTaskColumns + " FROM live_retention_task WHERE target_kind = ? AND target_id = ? " +
		"AND state IN (?, ?) ORDER BY retention_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &t, query, targetKind, targetID, RetentionStatePending, RetentionStateRunning); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_retention_task FindUnfinishedByTarget: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveRetentionTaskModel) List(ctx context.Context, f RetentionFilter) ([]*LiveRetentionTask, int32, error) {
	where, args := buildWhere(
		whereFragment{"target_kind = ?", []any{}}.when(f.TargetKind > 0, f.TargetKind),
		whereFragment{"state = ?", []any{}}.when(f.State > 0, f.State),
		whereFragment{"room_id = ?", []any{}}.when(f.RoomId > 0, f.RoomId),
	)
	limit, offset := clampPage(f.Pn, f.Ps, f.MaxPageSize)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_retention_task "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_retention_task List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveRetentionTask
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveRetentionTaskColumns+" FROM live_retention_task "+where+" ORDER BY retention_id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_retention_task List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultLiveRetentionTaskModel) ListByState(ctx context.Context, state int32, limit int32) ([]*LiveRetentionTask, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []*LiveRetentionTask
	query := liveRetentionTaskColumns + " FROM live_retention_task WHERE state = ? ORDER BY retention_id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, state, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_retention_task ListByState: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveRetentionTaskModel) UpdateState(ctx context.Context, retentionID int64, fromStates []int32,
	expectedVersion int64, patch RetentionPatch) (int64, error) {
	return m.UpdateStateTx(ctx, m.conn, retentionID, fromStates, expectedVersion, patch)
}

func (m *defaultLiveRetentionTaskModel) UpdateStateTx(ctx context.Context, sess sqlx.Session, retentionID int64,
	fromStates []int32, expectedVersion int64, patch RetentionPatch) (int64, error) {
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_retention_task UpdateState: empty fromStates %w", ErrInvalidTransition)
	}
	sets := append(patch.sets(), columnValue{"state", patch.State})
	wheres := []whereFragment{
		{"retention_id = ?", []any{retentionID}},
		stateInFragment(fromStates),
	}
	if expectedVersion > 0 {
		wheres = append(wheres, whereFragment{"version = ?", []any{expectedVersion}})
	}
	return conditionalUpdate(ctx, sess, "live_retention_task", sets, true, wheres)
}
