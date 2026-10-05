package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// taskColumns 是 op_admin_task 的列清单，必须与迁移 SQL 一致。
const taskColumns = "task_id, task_type, params, state, request_id, progress, total, succeeded, failed," +
	" operator, trace_id, ctime, mtime, started_at, finished_at"

// stepColumns 是 op_admin_task_step 的列清单。
const stepColumns = "id, task_id, step_no, target_type, target_id, state, result, err_msg, ctime, mtime"

// ErrTaskExists 表示 request_id 已存在（唯一索引冲突），用于幂等提交。
var ErrTaskExists = errors.New("operation: admin task already exists for request_id")

// AdminTask 对应 op_admin_task 表：批量运营任务。
// 任务只描述“要编排哪些下游动作”，真实写入由 video/catalog/rights/moderation
// 的 RPC 完成（AGENTS.md §5）；本服务不内置分发 worker，推进入口是 RunAdminTask RPC，
// 由 services/cron 或人工触发。
type AdminTask struct {
	// TaskID 任务 ID，主键。
	TaskID int64 `db:"task_id"`
	// TaskType 任务类型，见 TaskType* 常量。
	TaskType string `db:"task_type"`
	// Params 任务级参数（JSON 文本，如 {"reason":"版权到期"}）。
	Params string `db:"params"`
	// State 任务状态：pending/running/succeeded/partial/failed/canceled。
	State string `db:"state"`
	// RequestID 幂等键，唯一索引。
	RequestID string `db:"request_id"`
	// Progress 已执行步数（succeeded + failed）。
	Progress int32 `db:"progress"`
	// Total 步骤总数。
	Total int32 `db:"total"`
	// Succeeded 成功步数。
	Succeeded int32 `db:"succeeded"`
	// Failed 失败步数。
	Failed int32 `db:"failed"`
	// Operator 提交人 admin_id。
	Operator int64 `db:"operator"`
	// TraceID 提交时的链路 ID。
	TraceID string `db:"trace_id"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
	// StartedAt 首次进入 running 的时间（Unix 秒），0 表示未开始。
	StartedAt int64 `db:"started_at"`
	// FinishedAt 进入终态的时间（Unix 秒），0 表示未结束。
	FinishedAt int64 `db:"finished_at"`
}

// AdminTaskStep 对应 op_admin_task_step 表：任务步骤（一行一个目标聚合）。
type AdminTaskStep struct {
	// ID 自增主键。
	ID int64 `db:"id"`
	// TaskID 所属任务。
	TaskID int64 `db:"task_id"`
	// StepNo 步骤序号，任务内唯一。
	StepNo int32 `db:"step_no"`
	// TargetType 目标类型：submission/episode/rights_window/moderation_appeal。
	TargetType string `db:"target_type"`
	// TargetID 目标聚合 ID（字符串，兼容不同主键形态）。
	TargetID string `db:"target_id"`
	// State 步骤状态：pending/running/succeeded/failed/canceled。
	State string `db:"state"`
	// Result 下游返回摘要。
	Result string `db:"result"`
	// ErrMsg 失败原因（脱敏，不含堆栈与密钥）。
	ErrMsg string `db:"err_msg"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// AdminTaskModel 抽象 op_admin_task 表。
type AdminTaskModel interface {
	// Insert 新建任务；request_id 冲突时返回 ErrTaskExists（幂等重放由 repository 处理）。
	Insert(ctx context.Context, t *AdminTask) (int64, error)
	// FindOne 按 ID 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, taskID int64) (*AdminTask, error)
	// FindByRequestID 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*AdminTask, error)
	// TransitionState 受控状态迁移：仅当库中 state == from 时写入 to。
	// 返回 (是否迁移成功, error)；非法状态由调用方的 CanTaskTransition 预先拦截。
	TransitionState(ctx context.Context, taskID int64, from, to string, startedAt, finishedAt int64) (bool, error)
	// AddCounters 累加成功/失败步数与进度（并发安全：SQL 侧自增）。
	AddCounters(ctx context.Context, taskID int64, succeededDelta, failedDelta int32) error
	// List 分页查询任务；state/taskType 为空表示不过滤，operator=0 表示不过滤。
	List(ctx context.Context, state, taskType string, operator int64, pn, ps int32) ([]*AdminTask, int64, error)
}

// AdminTaskStepModel 抽象 op_admin_task_step 表。
type AdminTaskStepModel interface {
	// InsertBatch 批量写入步骤（单事务）。
	InsertBatch(ctx context.Context, steps []*AdminTaskStep) error
	// ListByTask 按任务列出全部步骤（step_no 升序）。
	ListByTask(ctx context.Context, taskID int64) ([]*AdminTaskStep, error)
	// ListExecutable 按任务列出待执行步骤（state=pending），最多 limit 条。
	ListExecutable(ctx context.Context, taskID int64, limit int32) ([]*AdminTaskStep, error)
	// MarkStep 受控状态迁移：仅当库中 state == from 时写入 to 及结果。
	MarkStep(ctx context.Context, stepID int64, from, to, result, errMsg string) (bool, error)
	// CancelPending 把任务下所有 pending 步骤置为 canceled，返回影响行数。
	CancelPending(ctx context.Context, taskID int64) (int64, error)
	// CountByTask 统计任务下各状态步骤数。
	CountByTask(ctx context.Context, taskID int64) (map[string]int32, error)
}

type defaultAdminTaskModel struct {
	conn sqlx.SqlConn
}

// NewAdminTaskModel 构造 op_admin_task 的 sqlx 实现。
func NewAdminTaskModel(conn sqlx.SqlConn) AdminTaskModel {
	return &defaultAdminTaskModel{conn: conn}
}

func (m *defaultAdminTaskModel) Insert(ctx context.Context, t *AdminTask) (int64, error) {
	now := nowUnix()
	if t.Ctime == 0 {
		t.Ctime = now
	}
	t.Mtime = now
	if t.State == "" {
		t.State = TaskStatePending
	}
	// uniq_request_id 命中时 ON DUPLICATE KEY UPDATE 为刻意空更新：
	// RowsAffected == 0 即判定为重复提交，避免依赖 driver 专有错误类型。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_admin_task ("+taskColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		t.TaskID, t.TaskType, t.Params, t.State, t.RequestID, t.Progress, t.Total,
		t.Succeeded, t.Failed, t.Operator, t.TraceID, t.Ctime, t.Mtime, t.StartedAt, t.FinishedAt)
	if err != nil {
		return 0, fmt.Errorf("op_admin_task Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_admin_task Insert RowsAffected: %w", err)
	}
	if aff == 0 {
		return 0, ErrTaskExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("op_admin_task Insert LastInsertId: %w", err)
	}
	t.TaskID = id
	return id, nil
}

func (m *defaultAdminTaskModel) FindOne(ctx context.Context, taskID int64) (*AdminTask, error) {
	var t AdminTask
	query := "SELECT " + taskColumns + " FROM op_admin_task WHERE task_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultAdminTaskModel) FindByRequestID(ctx context.Context, requestID string) (*AdminTask, error) {
	var t AdminTask
	query := "SELECT " + taskColumns + " FROM op_admin_task WHERE request_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_task FindByRequestID: %w", err)
	}
	return &t, nil
}

func (m *defaultAdminTaskModel) TransitionState(ctx context.Context, taskID int64, from, to string, startedAt, finishedAt int64) (bool, error) {
	var setSQL = "state = ?, mtime = ?"
	args := []any{to, nowUnix()}
	if startedAt > 0 {
		setSQL += ", started_at = ?"
		args = append(args, startedAt)
	}
	if finishedAt > 0 {
		setSQL += ", finished_at = ?"
		args = append(args, finishedAt)
	}
	args = append(args, taskID, from)
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_admin_task SET "+setSQL+" WHERE task_id = ? AND state = ?", args...)
	if err != nil {
		return false, fmt.Errorf("op_admin_task TransitionState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("op_admin_task TransitionState RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultAdminTaskModel) AddCounters(ctx context.Context, taskID int64, succeededDelta, failedDelta int32) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE op_admin_task SET succeeded = succeeded + ?, failed = failed + ?, progress = progress + ?, mtime = ? WHERE task_id = ?",
		succeededDelta, failedDelta, succeededDelta+failedDelta, nowUnix(), taskID)
	return err
}

func (m *defaultAdminTaskModel) List(ctx context.Context, state, taskType string, operator int64, pn, ps int32) ([]*AdminTask, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 3)
	if state != "" {
		where += " AND state = ?"
		args = append(args, state)
	}
	if taskType != "" {
		where += " AND task_type = ?"
		args = append(args, taskType)
	}
	if operator > 0 {
		where += " AND operator = ?"
		args = append(args, operator)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM op_admin_task "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("op_admin_task List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*AdminTask
	query := "SELECT " + taskColumns + " FROM op_admin_task " + where + " ORDER BY task_id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("op_admin_task List: %w", err)
	}
	return rows, total, nil
}

type defaultAdminTaskStepModel struct {
	conn sqlx.SqlConn
}

// NewAdminTaskStepModel 构造 op_admin_task_step 的 sqlx 实现。
func NewAdminTaskStepModel(conn sqlx.SqlConn) AdminTaskStepModel {
	return &defaultAdminTaskStepModel{conn: conn}
}

func (m *defaultAdminTaskStepModel) InsertBatch(ctx context.Context, steps []*AdminTaskStep) error {
	if len(steps) == 0 {
		return nil
	}
	return m.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		now := nowUnix()
		for _, s := range steps {
			if s.Ctime == 0 {
				s.Ctime = now
			}
			s.Mtime = now
			if s.State == "" {
				s.State = StepStatePending
			}
			res, err := session.ExecCtx(ctx,
				"INSERT INTO op_admin_task_step ("+stepColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				s.ID, s.TaskID, s.StepNo, s.TargetType, s.TargetID, s.State, s.Result, s.ErrMsg, s.Ctime, s.Mtime)
			if err != nil {
				return fmt.Errorf("op_admin_task_step insert: %w", err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("op_admin_task_step LastInsertId: %w", err)
			}
			s.ID = id
		}
		return nil
	})
}

func (m *defaultAdminTaskStepModel) ListByTask(ctx context.Context, taskID int64) ([]*AdminTaskStep, error) {
	var rows []*AdminTaskStep
	query := "SELECT " + stepColumns + " FROM op_admin_task_step WHERE task_id = ? ORDER BY step_no ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_task_step ListByTask: %w", err)
	}
	return rows, nil
}

func (m *defaultAdminTaskStepModel) ListExecutable(ctx context.Context, taskID int64, limit int32) ([]*AdminTaskStep, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []*AdminTaskStep
	query := "SELECT " + stepColumns + " FROM op_admin_task_step WHERE task_id = ? AND state = ? ORDER BY step_no ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, taskID, StepStatePending, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_task_step ListExecutable: %w", err)
	}
	return rows, nil
}

func (m *defaultAdminTaskStepModel) MarkStep(ctx context.Context, stepID int64, from, to, result, errMsg string) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_admin_task_step SET state = ?, result = ?, err_msg = ?, mtime = ? WHERE id = ? AND state = ?",
		to, result, errMsg, nowUnix(), stepID, from)
	if err != nil {
		return false, fmt.Errorf("op_admin_task_step MarkStep: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("op_admin_task_step MarkStep RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultAdminTaskStepModel) CancelPending(ctx context.Context, taskID int64) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_admin_task_step SET state = ?, mtime = ? WHERE task_id = ? AND state = ?",
		StepStateCanceled, nowUnix(), taskID, StepStatePending)
	if err != nil {
		return 0, fmt.Errorf("op_admin_task_step CancelPending: %w", err)
	}
	return res.RowsAffected()
}

func (m *defaultAdminTaskStepModel) CountByTask(ctx context.Context, taskID int64) (map[string]int32, error) {
	type row struct {
		State string `db:"state"`
		Cnt   int32  `db:"cnt"`
	}
	var rows []*row
	query := "SELECT state, COUNT(*) AS cnt FROM op_admin_task_step WHERE task_id = ? GROUP BY state"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]int32{}, nil
		}
		return nil, fmt.Errorf("op_admin_task_step CountByTask: %w", err)
	}
	out := make(map[string]int32, len(rows))
	for _, r := range rows {
		out[r.State] = r.Cnt
	}
	return out, nil
}
