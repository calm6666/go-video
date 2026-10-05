// 本文件是 model 包的手写扩展，不是 goctl 生成产物。
//
// 它做两件事：
//  1. 为既有单表方法补上「可加入外部事务」的 *Tx 变体，让 logic 能把
//     「claim 执行记录 + 抢占租约 + 推进调度指针」写进同一个事务（AGENTS.md §5）；
//     SQL 仍然只在 model，*Tx 变体只是把模型实例指向事务会话，不复制任何语句。
//  2. 补分页 total、按 run_id 反查租约与分组健康度的统计查询（RPC 契约需要）。

package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// inTx 返回绑定到事务会话的模型副本；tx 为 nil 时返回自身（自动提交）。
//
// sqlx.NewSqlConnFromSession 把一个 Session 适配成 SqlConn：由于 Session 没有
// Begin 能力，适配后的 TransactCtx 只能原地执行回调，因此像 TaskLease.Acquire
// 这种「自带事务」的方法加入外部事务后不会开嵌套事务，而是复用同一个事务。

func (m *defaultTaskDefinitionModel) inTx(tx sqlx.Session) *defaultTaskDefinitionModel {
	if tx == nil {
		return m
	}
	return &defaultTaskDefinitionModel{conn: sqlx.NewSqlConnFromSession(tx)}
}

func (m *defaultTaskRunModel) inTx(tx sqlx.Session) *defaultTaskRunModel {
	if tx == nil {
		return m
	}
	return &defaultTaskRunModel{conn: sqlx.NewSqlConnFromSession(tx)}
}

func (m *defaultTaskLeaseModel) inTx(tx sqlx.Session) *defaultTaskLeaseModel {
	if tx == nil {
		return m
	}
	return &defaultTaskLeaseModel{conn: sqlx.NewSqlConnFromSession(tx)}
}

func (m *defaultTaskCheckpointModel) inTx(tx sqlx.Session) *defaultTaskCheckpointModel {
	if tx == nil {
		return m
	}
	return &defaultTaskCheckpointModel{conn: sqlx.NewSqlConnFromSession(tx)}
}

// --- cron_task_definition ---

func (m *defaultTaskDefinitionModel) InsertTx(
	ctx context.Context, tx sqlx.Session, d *TaskDefinition,
) (bool, error) {
	return m.inTx(tx).Insert(ctx, d)
}

func (m *defaultTaskDefinitionModel) UpdateMutableTx(
	ctx context.Context, tx sqlx.Session, d *TaskDefinition, expectedVersion int64,
) (bool, error) {
	return m.inTx(tx).UpdateMutable(ctx, d, expectedVersion)
}

func (m *defaultTaskDefinitionModel) SetStateTx(
	ctx context.Context, tx sqlx.Session, taskKey string, fromState, toState int32,
	expectedVersion int64, operator string,
) (bool, error) {
	return m.inTx(tx).SetState(ctx, taskKey, fromState, toState, expectedVersion, operator)
}

func (m *defaultTaskDefinitionModel) MarkFiredTx(
	ctx context.Context, tx sqlx.Session, taskKey string, plannedAt, nextFireAt int64,
) error {
	return m.inTx(tx).MarkFired(ctx, taskKey, plannedAt, nextFireAt)
}

func (m *defaultTaskDefinitionModel) MarkResultTx(
	ctx context.Context, tx sqlx.Session, taskKey string, successAt int64, lastError string,
) error {
	return m.inTx(tx).MarkResult(ctx, taskKey, successAt, lastError)
}

func (m *defaultTaskDefinitionModel) FindOneTx(
	ctx context.Context, tx sqlx.Session, taskKey string,
) (*TaskDefinition, error) {
	return m.inTx(tx).FindOne(ctx, taskKey)
}

// SetNextFireAtTx 单独推进调度指针。
//
// 为什么需要：UpdateMutable 不碰 next_fire_at（避免人工改配置时误改计划），
// SetState 只在「暂停」时清空指针。于是 ResumeTask 重算指针、DisableTask
// 置零指针这两条路都没有落点，只能靠这里补上。它不带状态守卫，
// 调用方（logic）必须在同一事务里先用 SetState/UpdateMutable 的乐观锁命中。
func (m *defaultTaskDefinitionModel) SetNextFireAtTx(
	ctx context.Context, tx sqlx.Session, taskKey string, nextFireAt int64,
) error {
	if taskKey == "" {
		return ErrTaskKeyEmpty
	}
	if nextFireAt < 0 {
		nextFireAt = 0
	}
	_, err := m.inTx(tx).conn.ExecCtx(ctx,
		"UPDATE cron_task_definition SET next_fire_at = ?, mtime = ? WHERE task_key = ?",
		nextFireAt, nowUnix(), taskKey)
	if err != nil {
		return fmt.Errorf("cron_task_definition SetNextFireAt(%s): %w", taskKey, err)
	}
	return nil
}

func (m *defaultTaskDefinitionModel) CountByFilter(
	ctx context.Context, state int32, group, handler string,
) (int64, error) {
	where := "1 = 1"
	var args []any
	if state != TaskStateUnspecified {
		where += " AND state = ?"
		args = append(args, state)
	}
	if group != "" {
		where += " AND task_group = ?"
		args = append(args, group)
	}
	if handler != "" {
		where += " AND handler = ?"
		args = append(args, handler)
	}
	var total int64
	query := "SELECT COUNT(*) FROM cron_task_definition WHERE " + where
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cron_task_definition CountByFilter: %w", err)
	}
	return total, nil
}

// --- cron_task_run ---

func (m *defaultTaskRunModel) FindOneTx(ctx context.Context, tx sqlx.Session, runID int64) (*TaskRun, error) {
	return m.inTx(tx).FindOne(ctx, runID)
}

func (m *defaultTaskRunModel) FindByFireTx(
	ctx context.Context, tx sqlx.Session, taskKey string, plannedAt int64, attempt int32,
) (*TaskRun, error) {
	return m.inTx(tx).FindByFire(ctx, taskKey, plannedAt, attempt)
}

func (m *defaultTaskRunModel) MaxAttemptTx(
	ctx context.Context, tx sqlx.Session, taskKey string, plannedAt int64,
) (int32, error) {
	return m.inTx(tx).MaxAttempt(ctx, taskKey, plannedAt)
}

func (m *defaultTaskRunModel) StartTx(
	ctx context.Context, tx sqlx.Session, runID int64, owner string, fenceToken, ttlSeconds int64,
) (bool, error) {
	return m.inTx(tx).Start(ctx, runID, owner, fenceToken, ttlSeconds)
}

func (m *defaultTaskRunModel) RenewTx(
	ctx context.Context, tx sqlx.Session, runID int64, owner string, fenceToken, ttlSeconds int64,
) (bool, error) {
	return m.inTx(tx).Renew(ctx, runID, owner, fenceToken, ttlSeconds)
}

func (m *defaultTaskRunModel) ReportTx(
	ctx context.Context, tx sqlx.Session, runID int64, owner string, fenceToken int64,
	fromState, toState int32, resultSummary, lastError string, nextRetryAt, durationMs int64,
) (bool, error) {
	return m.inTx(tx).Report(ctx, runID, owner, fenceToken, fromState, toState,
		resultSummary, lastError, nextRetryAt, durationMs)
}

func (m *defaultTaskRunModel) CountRunningTx(ctx context.Context, tx sqlx.Session, taskKey string) (int64, error) {
	return m.inTx(tx).CountRunning(ctx, taskKey)
}

// ClaimForFire 抢占「某计划时刻的第 N 次尝试」并返回入库后的行（含 run_id）。
//
// 为什么需要：AcquireLease 必须先插入 PENDING 执行记录再抢租约，而 InsertTx
// 只返回 existed 不给 id；并发下两个实例可能同时插同一 (task_key, planned_at,
// attempt)，落败方必须读到赢方写入的那一行（而不是自己的入参），否则会给
// 不存在的 run_id 抢租约。命中唯一键时用 SELECT ... FOR UPDATE 读最新行：
// REPEATABLE READ 下普通 SELECT 可能返回事务快照里的「不存在」，加行锁才安全。
func (m *defaultTaskRunModel) ClaimForFire(
	ctx context.Context, tx sqlx.Session, r *TaskRun,
) (*TaskRun, bool, error) {
	mm := m.inTx(tx)
	existed, err := mm.InsertTx(ctx, tx, r)
	if err != nil {
		return nil, false, err
	}
	if !existed {
		stored, err := mm.FindByFire(ctx, r.TaskKey, r.PlannedAt, r.Attempt)
		if err != nil {
			return nil, false, err
		}
		if stored == nil {
			return nil, false, ErrRunNotFound
		}
		return stored, true, nil
	}
	stored, err := mm.lockByFire(ctx, tx, r.TaskKey, r.PlannedAt, r.Attempt)
	if err != nil {
		return nil, false, err
	}
	if stored == nil {
		return nil, false, ErrRunNotFound
	}
	return stored, false, nil
}

// lockByFire 在事务内按幂等身份加行锁读取，行不存在返回 (nil, nil)。
func (m *defaultTaskRunModel) lockByFire(
	ctx context.Context, tx sqlx.Session, taskKey string, plannedAt int64, attempt int32,
) (*TaskRun, error) {
	sess := tx
	if sess == nil {
		sess = m.conn
	}
	var r TaskRun
	query := "SELECT " + taskRunColumns + " FROM cron_task_run " +
		"WHERE task_key = ? AND planned_at = ? AND attempt = ? FOR UPDATE"
	if err := sess.QueryRowCtx(ctx, &r, query, taskKey, plannedAt, attempt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_run lockByFire(%s,%d,%d): %w", taskKey, plannedAt, attempt, err)
	}
	return &r, nil
}

// TakeoverTx 抢占「租约已过期的 RUNNING 执行」：把它就地交给新持有者。
//
// 为什么不能复用 Start：Start 只接受 state=PENDING，而实例崩溃后遗留的行是
// RUNNING，不放开这条路过期执行永远无法被回收。条件里带 fence_token=旧值，
// 保证同一过期行只有一个抢占者成功；旧实例后续上报会因栅栏不一致拿到
// ErrLeaseLost（AGENTS.md §5 互斥不伪成功）。
// reason 写入 last_error，保留「被谁因为什么接管」的排障线索。
func (m *defaultTaskRunModel) TakeoverTx(
	ctx context.Context, tx sqlx.Session, runID int64, owner string,
	oldFence, newFence, ttlSeconds int64, reason string,
) (bool, error) {
	if owner == "" {
		return false, ErrLeaseOwnerRequired
	}
	if newFence <= oldFence {
		return false, fmt.Errorf("%w: old=%d new=%d", ErrStateTransition, oldFence, newFence)
	}
	now := nowUnix()
	res, err := m.inTx(tx).conn.ExecCtx(ctx,
		"UPDATE cron_task_run SET lease_owner = ?, lease_expire_at = ?, fence_token = ?, started_at = ?, "+
			"last_error = ?, mtime = ? "+
			"WHERE id = ? AND state = ? AND fence_token = ? AND lease_expire_at > 0 AND lease_expire_at <= ?",
		owner, now+ttlSeconds, newFence, now, truncate(reason, MaxLastErrorBytes), now,
		runID, RunStateRunning, oldFence, now)
	if err != nil {
		return false, fmt.Errorf("cron_task_run Takeover(%d): %w", runID, err)
	}
	return rowsAffected(res)
}

func (m *defaultTaskRunModel) CountByFilter(
	ctx context.Context, taskKey string, state int32, plannedFrom, plannedTo int64,
) (int64, error) {
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
	var total int64
	query := "SELECT COUNT(*) FROM cron_task_run WHERE " + where
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cron_task_run CountByFilter: %w", err)
	}
	return total, nil
}

// --- cron_task_lease ---

func (m *defaultTaskLeaseModel) AcquireTx(
	ctx context.Context, tx sqlx.Session, leaseKey, taskKey, scope, owner string,
	ttlSeconds, runID int64,
) (*LeaseAcquire, error) {
	return m.inTx(tx).Acquire(ctx, leaseKey, taskKey, scope, owner, ttlSeconds, runID)
}

func (m *defaultTaskLeaseModel) RenewTx(
	ctx context.Context, tx sqlx.Session, leaseKey, owner string, fenceToken, ttlSeconds int64,
) (bool, error) {
	return m.inTx(tx).Renew(ctx, leaseKey, owner, fenceToken, ttlSeconds)
}

func (m *defaultTaskLeaseModel) ReleaseTx(
	ctx context.Context, tx sqlx.Session, leaseKey, owner string, fenceToken int64,
) (bool, error) {
	return m.inTx(tx).Release(ctx, leaseKey, owner, fenceToken)
}

func (m *defaultTaskLeaseModel) FindOneTx(
	ctx context.Context, tx sqlx.Session, leaseKey string,
) (*TaskLease, error) {
	return m.inTx(tx).FindOne(ctx, leaseKey)
}

// FindByRun 按当前持有租约的 run_id 反查租约行。
//
// 为什么需要：RenewLease/ReleaseLease/ReportTaskResult 的请求里只有 run_id，
// 带 scope 的任务其 lease_key 是 task_key + "/" + scope，而请求没有 scope 字段，
// 只能靠 cron_task_lease.run_id 反查定位（这是契约缺口的兜底，见 README「契约缺口」）。
func (m *defaultTaskLeaseModel) FindByRun(ctx context.Context, runID int64) (*TaskLease, error) {
	if runID <= 0 {
		return nil, ErrRunNotFound
	}
	var l TaskLease
	query := "SELECT " + taskLeaseColumns + " FROM cron_task_lease WHERE run_id = ? ORDER BY id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &l, query, runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_lease FindByRun(%d): %w", runID, err)
	}
	return &l, nil
}

// FindByTask 按 (task_key, owner_instance, fence_token) 定位租约行，
// 用于 run 已被接管实例改写（lease.run_id 不再指向本 run）时仍能找到自己那把锁。
func (m *defaultTaskLeaseModel) FindByTask(
	ctx context.Context, taskKey, owner string, fenceToken int64,
) (*TaskLease, error) {
	if taskKey == "" {
		return nil, ErrTaskKeyEmpty
	}
	var l TaskLease
	query := "SELECT " + taskLeaseColumns + " FROM cron_task_lease " +
		"WHERE task_key = ? AND owner_instance = ? AND fence_token = ? ORDER BY id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &l, query, taskKey, owner, fenceToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_lease FindByTask(%s): %w", taskKey, err)
	}
	return &l, nil
}

func (m *defaultTaskLeaseModel) CountByFilter(
	ctx context.Context, taskKey string, onlyExpired bool, now int64,
) (int64, error) {
	where := "1 = 1"
	var args []any
	if taskKey != "" {
		where += " AND task_key = ?"
		args = append(args, taskKey)
	}
	if onlyExpired {
		where += " AND expire_at > 0 AND expire_at <= ?"
		args = append(args, now)
	}
	var total int64
	query := "SELECT COUNT(*) FROM cron_task_lease WHERE " + where
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cron_task_lease CountByFilter: %w", err)
	}
	return total, nil
}

// --- 健康度聚合（只 JOIN cron 自己的两张表，AGENTS.md §5 数据所有权）---

// GroupRunStateCount 某分组在某执行状态下的执行数量。
// 说明：cron_task_run 只存 task_key，分组归属由本服务的任务定义给出；
// 定义被删除后其历史执行不再计入分组健康度（README「留存与回收」）。
type GroupRunStateCount struct {
	TaskGroup string `db:"task_group"`
	State     int32  `db:"state"`
	Total     int64  `db:"total"`
}

// GroupCount 一个分组的单值计数。
type GroupCount struct {
	TaskGroup string `db:"task_group"`
	Total     int64  `db:"total"`
}

func (m *defaultTaskRunModel) CountByGroupStates(ctx context.Context, since int64) ([]GroupRunStateCount, error) {
	var rows []GroupRunStateCount
	query := "SELECT d.task_group, r.state, COUNT(*) AS total FROM cron_task_run r " +
		"JOIN cron_task_definition d ON d.task_key = r.task_key WHERE r.ctime >= ? " +
		"GROUP BY d.task_group, r.state ORDER BY d.task_group ASC, r.state ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, since); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_run CountByGroupStates: %w", err)
	}
	return rows, nil
}

func (m *defaultTaskLeaseModel) CountExpiredByGroup(ctx context.Context, now int64) ([]GroupCount, error) {
	var rows []GroupCount
	query := "SELECT d.task_group, COUNT(*) AS total FROM cron_task_lease l " +
		"JOIN cron_task_definition d ON d.task_key = l.task_key " +
		"WHERE l.owner_instance <> '' AND l.expire_at > 0 AND l.expire_at <= ? " +
		"GROUP BY d.task_group ORDER BY d.task_group ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, now); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_lease CountExpiredByGroup: %w", err)
	}
	return rows, nil
}

// --- cron_task_checkpoint ---

func (m *defaultTaskCheckpointModel) SaveTx(
	ctx context.Context, tx sqlx.Session, c *TaskCheckpoint, expectedVersion int64,
) (bool, error) {
	return m.inTx(tx).Save(ctx, c, expectedVersion)
}

func (m *defaultTaskCheckpointModel) FindOneTx(
	ctx context.Context, tx sqlx.Session, taskKey, scopeKey string,
) (*TaskCheckpoint, error) {
	return m.inTx(tx).FindOne(ctx, taskKey, scopeKey)
}

func (m *defaultTaskCheckpointModel) CountByTask(ctx context.Context, taskKey string) (int64, error) {
	where := "1 = 1"
	var args []any
	if taskKey != "" {
		where += " AND task_key = ?"
		args = append(args, taskKey)
	}
	var total int64
	query := "SELECT COUNT(*) FROM cron_task_checkpoint WHERE " + where
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cron_task_checkpoint CountByTask: %w", err)
	}
	return total, nil
}

// --- cron_task_audit ---

func (m *defaultTaskAuditModel) CountByFilter(
	ctx context.Context, taskKey, action string, ctimeFrom, ctimeTo int64,
) (int64, error) {
	where := "1 = 1"
	var args []any
	if taskKey != "" {
		where += " AND task_key = ?"
		args = append(args, taskKey)
	}
	if action != "" {
		where += " AND action = ?"
		args = append(args, action)
	}
	if ctimeFrom > 0 {
		where += " AND ctime >= ?"
		args = append(args, ctimeFrom)
	}
	if ctimeTo > 0 {
		where += " AND ctime <= ?"
		args = append(args, ctimeTo)
	}
	var total int64
	query := "SELECT COUNT(*) FROM cron_task_audit WHERE " + where
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cron_task_audit CountByFilter: %w", err)
	}
	return total, nil
}
