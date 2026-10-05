package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// taskCheckpointColumns 必须与
// deploy/migrations/cron/000002_create_cron_run_lease_checkpoint_tables.sql 一致。
const taskCheckpointColumns = "id, task_key, scope_key, value, value_str, version, operator, ctime, mtime"

// TaskCheckpoint 增量任务游标（cron_task_checkpoint 表）。
//
// 它是 cron「重放不重复产生副作用」的落点：报表、归档、索引重建、死信重放这类任务
// 按主键或时间水位分段推进，每段完成后 CAS 前进游标。同一计划时刻被重放时，
// 已推进的区间会被游标挡住，只有未完成部分继续向前收敛（README「重放语义」）。
//
// version 是 CAS 版本：Save(expected_version=0) 要求「游标尚不存在」，
// Save(expected_version=N) 要求当前版本恰为 N；不一致返回 ErrCheckpointConflict，
// 由处理器决定是重读还是放弃本轮，杜绝两个实例各写一半。
type TaskCheckpoint struct {
	ID       int64  `db:"id"`
	TaskKey  string `db:"task_key"`  // 任务键
	ScopeKey string `db:"scope_key"` // 同一任务内的分片/维度游标，空表示默认游标
	Value    int64  `db:"value"`     // 数值游标（已处理到的主键或时间水位）
	ValueStr string `db:"value_str"` // 字符串游标（索引别名、分区名等）
	Version  int64  `db:"version"`   // CAS 版本，每次成功写入 +1
	Operator string `db:"operator"`  // 最近一次推进者（实例名或操作人）
	Ctime    int64  `db:"ctime"`
	Mtime    int64  `db:"mtime"`
}

// TaskCheckpointModel cron_task_checkpoint 表读写接口。
type TaskCheckpointModel interface {
	// Save 以 CAS 方式写入游标：expectedVersion=0 要求行尚不存在（并发首写只有一个赢家），
	// >0 要求当前版本匹配。返回 advanced=false 表示版本冲突，调用方应重读后再决定。
	Save(ctx context.Context, c *TaskCheckpoint, expectedVersion int64) (advanced bool, err error)
	// FindOne 查询游标；不存在返回 (nil, nil)，logic 层用 found=false 表达「从未推进」。
	FindOne(ctx context.Context, taskKey, scopeKey string) (*TaskCheckpoint, error)
	// ListByCursor 按 (task_key, scope_key) 升序游标分页；cursorKey 为上一页最后一行的
	// task_key + "\x1f" + scope_key 复合键（空表示从头）。
	ListByCursor(ctx context.Context, taskKey, cursorKey string, limit int) ([]*TaskCheckpoint, string, error)
	// DeleteBefore 清理长期未更新（mtime 早于 cutoff）的游标，供清理任务调用；
	// 是否允许清理由 logic 层先校验任务已停用，模型只负责按时间界限删。
	DeleteBefore(ctx context.Context, before int64, limit int64) (int64, error)

	// --- 事务变体与统计（实现见 tx.go）---

	SaveTx(ctx context.Context, tx sqlx.Session, c *TaskCheckpoint, expectedVersion int64) (bool, error)
	FindOneTx(ctx context.Context, tx sqlx.Session, taskKey, scopeKey string) (*TaskCheckpoint, error)
	CountByTask(ctx context.Context, taskKey string) (int64, error)
}

// CompositeCursor 生成 (task_key, scope_key) 复合游标，与 ListByCursor 的解析规则配对。
func CompositeCursor(taskKey, scopeKey string) string {
	return taskKey + cursorSeparator + scopeKey
}

// SplitCompositeCursor 校验并解析复合游标，把「格式不对」统一表达为 ErrInvalidCursor，
// 让 logic 不必重复实现分隔符规则。
func SplitCompositeCursor(cursor string) (string, string, error) {
	taskKey, scopeKey, err := splitCursorKey(cursor)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return taskKey, scopeKey, nil
}

type defaultTaskCheckpointModel struct {
	conn sqlx.SqlConn
}

// NewTaskCheckpointModel 创建 TaskCheckpointModel 实现。
func NewTaskCheckpointModel(conn sqlx.SqlConn) TaskCheckpointModel {
	return &defaultTaskCheckpointModel{conn: conn}
}

func (m *defaultTaskCheckpointModel) Save(
	ctx context.Context, c *TaskCheckpoint, expectedVersion int64,
) (bool, error) {
	if c.TaskKey == "" {
		return false, ErrTaskKeyEmpty
	}
	now := nowUnix()
	if expectedVersion == 0 {
		// 首写：靠 uniq_task_scope 兜并发，冲突时不覆盖既有游标。
		res, err := m.conn.ExecCtx(ctx,
			"INSERT INTO cron_task_checkpoint (task_key, scope_key, value, value_str, version, operator, ctime, mtime) "+
				"VALUES (?, ?, ?, ?, 1, ?, ?, ?) ON DUPLICATE KEY UPDATE id = id",
			c.TaskKey, c.ScopeKey, c.Value, c.ValueStr, c.Operator, now, now)
		if err != nil {
			return false, fmt.Errorf("cron_task_checkpoint Save first-write(%s/%s): %w", c.TaskKey, c.ScopeKey, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("cron_task_checkpoint Save RowsAffected: %w", err)
		}
		if affected == 0 {
			return false, ErrCheckpointConflict
		}
		c.ID, c.Version, c.Ctime, c.Mtime = 0, 1, now, now
		return true, nil
	}

	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cron_task_checkpoint SET value = ?, value_str = ?, version = version + 1, operator = ?, mtime = ? "+
			"WHERE task_key = ? AND scope_key = ? AND version = ?",
		c.Value, c.ValueStr, c.Operator, now, c.TaskKey, c.ScopeKey, expectedVersion)
	if err != nil {
		return false, fmt.Errorf("cron_task_checkpoint Save cas(%s/%s): %w", c.TaskKey, c.ScopeKey, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cron_task_checkpoint Save RowsAffected: %w", err)
	}
	if affected == 0 {
		return false, ErrCheckpointConflict
	}
	c.Version = expectedVersion + 1
	c.Mtime = now
	return true, nil
}

func (m *defaultTaskCheckpointModel) FindOne(
	ctx context.Context, taskKey, scopeKey string,
) (*TaskCheckpoint, error) {
	if taskKey == "" {
		return nil, ErrTaskKeyEmpty
	}
	var c TaskCheckpoint
	query := "SELECT " + taskCheckpointColumns + " FROM cron_task_checkpoint WHERE task_key = ? AND scope_key = ?"
	if err := m.conn.QueryRowCtx(ctx, &c, query, taskKey, scopeKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_checkpoint FindOne(%s/%s): %w", taskKey, scopeKey, err)
	}
	return &c, nil
}

func (m *defaultTaskCheckpointModel) ListByCursor(
	ctx context.Context, taskKey, cursorKey string, limit int,
) ([]*TaskCheckpoint, string, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	where := "1 = 1"
	var args []any
	if taskKey != "" {
		where += " AND task_key = ?"
		args = append(args, taskKey)
	}
	if cursorKey != "" {
		// 复合键游标：(task_key, scope_key) 严格大于上一页最后一行，避免同 task 下漏读。
		ct, cs, err := splitCursorKey(cursorKey)
		if err != nil {
			return nil, "", err
		}
		where += " AND (task_key > ? OR (task_key = ? AND scope_key > ?))"
		args = append(args, ct, ct, cs)
	}
	args = append(args, limit)

	var rows []*TaskCheckpoint
	query := "SELECT " + taskCheckpointColumns + " FROM cron_task_checkpoint WHERE " + where +
		" ORDER BY task_key ASC, scope_key ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("cron_task_checkpoint ListByCursor: %w", err)
	}
	next := ""
	if len(rows) == limit {
		last := rows[len(rows)-1]
		next = last.TaskKey + cursorSeparator + last.ScopeKey
	}
	return rows, next, nil
}

func (m *defaultTaskCheckpointModel) DeleteBefore(
	ctx context.Context, before int64, limit int64,
) (int64, error) {
	if before <= 0 {
		return 0, errors.New("cron: delete cutoff must be positive")
	}
	if limit <= 0 {
		limit = 1000
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM cron_task_checkpoint WHERE mtime < ? LIMIT ?", before, limit)
	if err != nil {
		return 0, fmt.Errorf("cron_task_checkpoint DeleteBefore: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cron_task_checkpoint DeleteBefore RowsAffected: %w", err)
	}
	return affected, nil
}

// cursorSeparator 是复合游标里分隔两个字段的不可见字符，
// 选 \x1f（Unit Separator）是为了避免任务键/分区名里出现普通可打印字符导致歧义。
const cursorSeparator = "\x1f"

// splitCursorKey 解析 ListByCursor 的复合游标。
func splitCursorKey(cursor string) (string, string, error) {
	for i := 0; i < len(cursor); i++ {
		if cursor[i] == cursorSeparator[0] {
			return cursor[:i], cursor[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("cron: invalid composite cursor %q", cursor)
}
