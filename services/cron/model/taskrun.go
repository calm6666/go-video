package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// taskRunColumns 必须与
// deploy/migrations/cron/000002_create_cron_run_lease_checkpoint_tables.sql 一致。
const taskRunColumns = "id, task_key, planned_at, attempt, trigger_type, state, lease_owner, " +
	"lease_expire_at, fence_token, started_at, finished_at, duration_ms, result_summary, last_error, " +
	"next_retry_at, operator, trace_id, ctime, mtime"

// TaskRun 执行记录（cron_task_run 表）。
//
// 幂等身份：UNIQUE(task_key, planned_at, attempt)。
//   - 一次「计划触发」= (task_key, planned_at)。并发 claim 时只有一个实例能插入
//     attempt=1 成功，其余得到 existed=true，因此同一计划时刻不会重复产生副作用；
//   - 退避重试与人工重试都是「同一 (task_key, planned_at) 下的新 attempt 行」，
//     绝不复用旧行、也不把终态改回运行中，历史轨迹不可篡改；
//   - 处理器必须以 (task_key, planned_at) 作为自己的幂等上下文（报表/归档类任务
//     再用 cron_task_checkpoint 的 CAS 游标记录已处理水位）。重放同一计划时刻时，
//     游标已推进的部分会被跳过，只有未完成部分继续向前收敛。
//
// 租约语义：state=RUNNING 的行由 lease_owner + lease_expire_at + fence_token 三元组守护。
//   - TTL 到期即视为实例崩溃，任何实例都可抢占（fence_token 单调递增）；
//   - 被抢占的旧实例再上报时 fence_token 不一致，得到 ErrLeaseLost，
//     必须立刻停止向下游写入，避免「两个实例同时推进同一任务」造成重复副作用。
type TaskRun struct {
	ID            int64  `db:"id"`              // run_id
	TaskKey       string `db:"task_key"`        // 任务键
	PlannedAt     int64  `db:"planned_at"`      // 计划时刻（Unix 秒），幂等身份的一部分
	Attempt       int32  `db:"attempt"`         // 第几次尝试，从 1 开始
	TriggerType   int32  `db:"trigger_type"`    // 1 定时 / 2 手动 / 3 重试 / 4 重放
	State         int32  `db:"state"`           // RunState*
	LeaseOwner    string `db:"lease_owner"`     // 持有租约的实例
	LeaseExpireAt int64  `db:"lease_expire_at"` // 租约到期时间，过期即可被抢占
	FenceToken    int64  `db:"fence_token"`     // 栅栏令牌，抢占时递增
	StartedAt     int64  `db:"started_at"`
	FinishedAt    int64  `db:"finished_at"`
	DurationMs    int64  `db:"duration_ms"`
	ResultSummary string `db:"result_summary"` // 处理器回填的结果摘要
	LastError     string `db:"last_error"`
	NextRetryAt   int64  `db:"next_retry_at"` // RETRYING 的下次可执行时间
	Operator      string `db:"operator"`      // 手动触发/重试的操作人
	TraceID       string `db:"trace_id"`
	Ctime         int64  `db:"ctime"`
	Mtime         int64  `db:"mtime"`
}

// TaskRunModel cron_task_run 表读写接口。
type TaskRunModel interface {
	// InsertTx 在给定事务内插入执行记录（tx 为 nil 时退化为自动提交）。
	// 命中 uniq_fire_attempt 时返回 existed=true 且不覆盖既有行，调用方读回原行即可。
	InsertTx(ctx context.Context, tx sqlx.Session, r *TaskRun) (existed bool, err error)
	// FindOne 按 run_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, runID int64) (*TaskRun, error)
	// FindByFire 按幂等身份 (task_key, planned_at, attempt) 查询；不存在返回 (nil, nil)。
	FindByFire(ctx context.Context, taskKey string, plannedAt int64, attempt int32) (*TaskRun, error)
	// MaxAttempt 返回某计划时刻已存在的最大 attempt（无记录时为 0）。
	MaxAttempt(ctx context.Context, taskKey string, plannedAt int64) (int32, error)
	// Start 把 PENDING 推进到 RUNNING 并写入租约三元组（CAS：state + attempt 条件）。
	// 返回 false 表示已被其它实例推进或抢占。
	Start(ctx context.Context, runID int64, owner string, fenceToken int64, ttlSeconds int64) (bool, error)
	// Renew 心跳续租：owner 与 fence_token 都必须匹配，否则返回 false（租约已丢失）。
	Renew(ctx context.Context, runID int64, owner string, fenceToken int64, ttlSeconds int64) (bool, error)
	// Report 以 fence_token 为条件推进执行状态并写结果。
	// fromState/toState 必须满足 CanTransition；非法迁移返回 ErrStateTransition，
	// 栅栏不一致返回 false（调用方按 ErrLeaseLost 处理）。
	Report(ctx context.Context, runID int64, owner string, fenceToken int64, fromState, toState int32,
		resultSummary, lastError string, nextRetryAt int64, durationMs int64) (bool, error)
	// ListByCursor 按 (planned_at, id) 倒序游标分页；cursorID 为上一页最后一行 id（0 表示从头）。
	ListByCursor(ctx context.Context, taskKey string, state int32, plannedFrom, plannedTo, cursorID int64,
		limit int) ([]*TaskRun, int64, error)
	// CountRunning 统计某任务当前 RUNNING 的执行数，用于并发上限判定。
	CountRunning(ctx context.Context, taskKey string) (int64, error)
	// ListExpiredRunning 列出 RUNNING 但租约已过期的执行（实例崩溃遗留，可被抢占/回收）。
	ListExpiredRunning(ctx context.Context, now int64, limit int) ([]*TaskRun, error)
	// ListDueRetrying 列出退避到期、等待新 attempt 的执行。
	ListDueRetrying(ctx context.Context, now int64, limit int) ([]*TaskRun, error)
	// CountByStates 统计 since 之后各状态的执行数，供 GetSchedulerHealth 使用。
	CountByStates(ctx context.Context, since int64) ([]RunStateCount, error)
	// DeleteBefore 按 ctime 清理历史执行记录（清理任务专用，limit 限制单轮删除行数避免长事务）。
	DeleteBefore(ctx context.Context, before int64, limit int64) (int64, error)

	// --- 事务变体与统计（实现见 tx.go）---
	// claim/续租/上报都要和租约、游标同事务，因此每个写方法都要有 tx 版本。

	FindOneTx(ctx context.Context, tx sqlx.Session, runID int64) (*TaskRun, error)
	FindByFireTx(ctx context.Context, tx sqlx.Session, taskKey string, plannedAt int64,
		attempt int32) (*TaskRun, error)
	MaxAttemptTx(ctx context.Context, tx sqlx.Session, taskKey string, plannedAt int64) (int32, error)
	StartTx(ctx context.Context, tx sqlx.Session, runID int64, owner string, fenceToken, ttlSeconds int64) (bool, error)
	RenewTx(ctx context.Context, tx sqlx.Session, runID int64, owner string, fenceToken, ttlSeconds int64) (bool, error)
	ReportTx(ctx context.Context, tx sqlx.Session, runID int64, owner string, fenceToken int64,
		fromState, toState int32, resultSummary, lastError string, nextRetryAt, durationMs int64) (bool, error)
	CountRunningTx(ctx context.Context, tx sqlx.Session, taskKey string) (int64, error)
	CountByFilter(ctx context.Context, taskKey string, state int32, plannedFrom, plannedTo int64) (int64, error)
	// ClaimForFire 插入（或幂等读回）某计划时刻的一次尝试，并返回入库后的行（含 run_id）。
	// created=false 表示同一 (task_key, planned_at, attempt) 已存在，调用方按重复触发处理。
	ClaimForFire(ctx context.Context, tx sqlx.Session, r *TaskRun) (stored *TaskRun, created bool, err error)
	// TakeoverTx 就地接管「租约已过期的 RUNNING 执行」：CAS 条件带旧 fence_token，
	// 只有一个抢占者能命中；旧实例随后的续租/上报必然得到 ErrLeaseLost。
	TakeoverTx(ctx context.Context, tx sqlx.Session, runID int64, owner string,
		oldFence, newFence, ttlSeconds int64, reason string) (bool, error)
	// CountByGroupStates 按 (分组, 状态) 聚合 since 之后的执行数，供分组健康度使用。
	CountByGroupStates(ctx context.Context, since int64) ([]GroupRunStateCount, error)
}

// RunStateCount 某状态在执行窗口内的数量。
type RunStateCount struct {
	State int32 `db:"state"`
	Total int64 `db:"total"`
}

type defaultTaskRunModel struct {
	conn sqlx.SqlConn
}

// NewTaskRunModel 创建 TaskRunModel 实现。
func NewTaskRunModel(conn sqlx.SqlConn) TaskRunModel {
	return &defaultTaskRunModel{conn: conn}
}

func (m *defaultTaskRunModel) session(tx sqlx.Session) sqlx.Session {
	if tx != nil {
		return tx
	}
	return m.conn
}

func (m *defaultTaskRunModel) InsertTx(ctx context.Context, tx sqlx.Session, r *TaskRun) (bool, error) {
	if r.TaskKey == "" {
		return false, ErrTaskKeyEmpty
	}
	if r.PlannedAt <= 0 {
		return false, errors.New("cron: planned_at is required for a fire identity")
	}
	if r.Attempt <= 0 {
		r.Attempt = 1
	}
	now := nowUnix()
	if r.Ctime == 0 {
		r.Ctime = now
	}
	r.Mtime = now
	if r.State == RunStateUnspecified {
		r.State = RunStatePending
	}
	res, err := m.session(tx).ExecCtx(ctx,
		"INSERT INTO cron_task_run (task_key, planned_at, attempt, trigger_type, state, lease_owner, "+
			"lease_expire_at, fence_token, started_at, finished_at, duration_ms, result_summary, last_error, "+
			"next_retry_at, operator, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE id = id",
		r.TaskKey, r.PlannedAt, r.Attempt, r.TriggerType, r.State, r.LeaseOwner,
		r.LeaseExpireAt, r.FenceToken, r.StartedAt, r.FinishedAt, r.DurationMs, r.ResultSummary,
		r.LastError, r.NextRetryAt, r.Operator, r.TraceID, r.Ctime, r.Mtime)
	if err != nil {
		return false, fmt.Errorf("cron_task_run InsertTx: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cron_task_run InsertTx RowsAffected: %w", err)
	}
	return affected == 0, nil
}

func (m *defaultTaskRunModel) FindOne(ctx context.Context, runID int64) (*TaskRun, error) {
	var r TaskRun
	query := "SELECT " + taskRunColumns + " FROM cron_task_run WHERE id = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_run FindOne(%d): %w", runID, err)
	}
	return &r, nil
}

func (m *defaultTaskRunModel) FindByFire(
	ctx context.Context, taskKey string, plannedAt int64, attempt int32,
) (*TaskRun, error) {
	var r TaskRun
	query := "SELECT " + taskRunColumns + " FROM cron_task_run WHERE task_key = ? AND planned_at = ? AND attempt = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, taskKey, plannedAt, attempt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_run FindByFire(%s,%d,%d): %w", taskKey, plannedAt, attempt, err)
	}
	return &r, nil
}

func (m *defaultTaskRunModel) MaxAttempt(ctx context.Context, taskKey string, plannedAt int64) (int32, error) {
	var attempt sql.NullInt64
	query := "SELECT MAX(attempt) FROM cron_task_run WHERE task_key = ? AND planned_at = ?"
	if err := m.conn.QueryRowCtx(ctx, &attempt, query, taskKey, plannedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cron_task_run MaxAttempt(%s,%d): %w", taskKey, plannedAt, err)
	}
	return int32(attempt.Int64), nil
}

func (m *defaultTaskRunModel) Start(
	ctx context.Context, runID int64, owner string, fenceToken int64, ttlSeconds int64,
) (bool, error) {
	if owner == "" {
		return false, errors.New("cron: lease owner is required")
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cron_task_run SET state = ?, lease_owner = ?, lease_expire_at = ?, fence_token = ?, "+
			"started_at = ?, mtime = ? WHERE id = ? AND state = ?",
		RunStateRunning, owner, now+ttlSeconds, fenceToken, now, now, runID, RunStatePending)
	if err != nil {
		return false, fmt.Errorf("cron_task_run Start(%d): %w", runID, err)
	}
	return rowsAffected(res)
}

func (m *defaultTaskRunModel) Renew(
	ctx context.Context, runID int64, owner string, fenceToken int64, ttlSeconds int64,
) (bool, error) {
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cron_task_run SET lease_expire_at = ?, mtime = ? "+
			"WHERE id = ? AND state = ? AND lease_owner = ? AND fence_token = ?",
		now+ttlSeconds, now, runID, RunStateRunning, owner, fenceToken)
	if err != nil {
		return false, fmt.Errorf("cron_task_run Renew(%d): %w", runID, err)
	}
	return rowsAffected(res)
}

func (m *defaultTaskRunModel) Report(
	ctx context.Context, runID int64, owner string, fenceToken int64, fromState, toState int32,
	resultSummary, lastError string, nextRetryAt int64, durationMs int64,
) (bool, error) {
	if !CanTransition(fromState, toState) {
		return false, fmt.Errorf("%w: %d -> %d", ErrStateTransition, fromState, toState)
	}
	now := nowUnix()
	// 终态写 finished_at 并清空租约有效期（保留 lease_owner 供审计「谁跑的」）。
	query := "UPDATE cron_task_run SET state = ?, result_summary = ?, last_error = ?, next_retry_at = ?, " +
		"duration_ms = CASE WHEN ? > 0 THEN ? ELSE duration_ms END, " +
		"finished_at = CASE WHEN ? IN (?, ?, ?, ?, ?) THEN ? ELSE finished_at END, " +
		"lease_expire_at = CASE WHEN ? IN (?, ?, ?, ?, ?) THEN 0 ELSE lease_expire_at END, mtime = ? " +
		"WHERE id = ? AND state = ? AND fence_token = ?"
	args := []any{
		toState, truncate(resultSummary, MaxResultSummaryBytes), truncate(lastError, MaxLastErrorBytes),
		nextRetryAt, durationMs, durationMs,
		toState, RunStateSucceeded, RunStateFailed, RunStateTimeout, RunStateCanceled, RunStateSkipped, now,
		toState, RunStateSucceeded, RunStateFailed, RunStateTimeout, RunStateCanceled, RunStateSkipped,
		now, runID, fromState, fenceToken,
	}
	if owner != "" {
		query += " AND lease_owner = ?"
		args = append(args, owner)
	}
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("cron_task_run Report(%d): %w", runID, err)
	}
	return rowsAffected(res)
}

func (m *defaultTaskRunModel) ListByCursor(
	ctx context.Context, taskKey string, state int32, plannedFrom, plannedTo, cursorID int64, limit int,
) ([]*TaskRun, int64, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	where := "1 = 1"
	var args []any
	if taskKey != "" {
		where += " AND task_key = ?"
		args = append(args, taskKey)
	}
	if state != RunStateUnspecified {
		where += " AND state = ?"
		args = append(args, state)
	}
	if plannedFrom > 0 {
		where += " AND planned_at >= ?"
		args = append(args, plannedFrom)
	}
	if plannedTo > 0 {
		where += " AND planned_at <= ?"
		args = append(args, plannedTo)
	}
	if cursorID > 0 {
		// 倒序游标：上一页最后一条的 planned_at 由 id 反查，避免同秒多行时漏读。
		where += " AND id < ?"
		args = append(args, cursorID)
	}
	args = append(args, limit)

	var rows []*TaskRun
	query := "SELECT " + taskRunColumns + " FROM cron_task_run WHERE " + where +
		" ORDER BY planned_at DESC, id DESC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("cron_task_run ListByCursor: %w", err)
	}
	next := int64(0)
	if len(rows) == limit {
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}

func (m *defaultTaskRunModel) CountRunning(ctx context.Context, taskKey string) (int64, error) {
	var total int64
	query := "SELECT COUNT(*) FROM cron_task_run WHERE state = ? AND task_key = ? AND lease_expire_at > ?"
	if err := m.conn.QueryRowCtx(ctx, &total, query, RunStateRunning, taskKey, nowUnix()); err != nil {
		return 0, fmt.Errorf("cron_task_run CountRunning(%s): %w", taskKey, err)
	}
	return total, nil
}

func (m *defaultTaskRunModel) ListExpiredRunning(ctx context.Context, now int64, limit int) ([]*TaskRun, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	var rows []*TaskRun
	query := "SELECT " + taskRunColumns + " FROM cron_task_run " +
		"WHERE state = ? AND lease_expire_at > 0 AND lease_expire_at <= ? ORDER BY lease_expire_at ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, RunStateRunning, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_run ListExpiredRunning: %w", err)
	}
	return rows, nil
}

func (m *defaultTaskRunModel) ListDueRetrying(ctx context.Context, now int64, limit int) ([]*TaskRun, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	var rows []*TaskRun
	query := "SELECT " + taskRunColumns + " FROM cron_task_run " +
		"WHERE state = ? AND next_retry_at <= ? ORDER BY next_retry_at ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, RunStateRetrying, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_run ListDueRetrying: %w", err)
	}
	return rows, nil
}

func (m *defaultTaskRunModel) CountByStates(ctx context.Context, since int64) ([]RunStateCount, error) {
	var rows []RunStateCount
	query := "SELECT state, COUNT(*) AS total FROM cron_task_run WHERE ctime >= ? GROUP BY state ORDER BY state ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, since); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_run CountByStates: %w", err)
	}
	return rows, nil
}

func (m *defaultTaskRunModel) DeleteBefore(ctx context.Context, before int64, limit int64) (int64, error) {
	if before <= 0 {
		return 0, errors.New("cron: delete cutoff must be positive")
	}
	if limit <= 0 {
		limit = 1000
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM cron_task_run WHERE ctime < ? AND state IN (?, ?, ?, ?, ?) LIMIT ?",
		before, RunStateSucceeded, RunStateFailed, RunStateTimeout, RunStateCanceled, RunStateSkipped, limit)
	if err != nil {
		return 0, fmt.Errorf("cron_task_run DeleteBefore: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cron_task_run DeleteBefore RowsAffected: %w", err)
	}
	return affected, nil
}
