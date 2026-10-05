package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SearchIndexTask 索引重建任务（search_index_task 表）。
// 幂等：uniq_request_id 唯一索引，同一 request_id 重复提交返回同一任务；
// 状态推进：pending → running → succeeded/failed，canceled 由运维置入。
// cursor_value 保存续跑游标（content_id 区间下界），进程重启后可从断点继续。
type SearchIndexTask struct {
	ID          int64  `db:"id"`           // 自增主键
	TaskID      string `db:"task_id"`      // 任务 ID（ULID，对外暴露）
	Scope       string `db:"scope"`        // 重建范围：full/partition/content_type
	ScopeValue  string `db:"scope_value"`  // 范围取值：分区区间或内容类型名
	State       string `db:"state"`        // 任务状态：pending/running/succeeded/failed/canceled
	CursorValue string `db:"cursor_value"` // 续跑游标（content_id 区间下界，十进制字符串）
	Total       int64  `db:"total"`        // 预计总量（首个切片估算，0 表示未知）
	Processed   int64  `db:"processed"`    // 已处理文档数
	Failed      int64  `db:"failed"`       // 失败文档数
	TargetIndex string `db:"target_index"` // 目标物理索引（重建写入新索引后切别名）
	Alias       string `db:"alias"`        // 查询别名
	Operator    string `db:"operator"`     // 提交人（管理员或 cron job 名）
	RequestID   string `db:"request_id"`   // 幂等键
	LastError   string `db:"last_error"`   // 最近一次失败原因（脱敏，不含堆栈）
	Ctime       int64  `db:"ctime"`        // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 修改时间（Unix 秒）
	StartedAt   int64  `db:"started_at"`   // 开始执行时间（Unix 秒）
	FinishedAt  int64  `db:"finished_at"`  // 结束时间（Unix 秒）
}

// SearchIndexTaskModel search_index_task 表查询与写入接口。
type SearchIndexTaskModel interface {
	// Insert 新建任务；命中 uniq_request_id 时不覆盖，返回 existed=false。
	Insert(ctx context.Context, t *SearchIndexTask) (existed bool, err error)
	// FindOne 按 task_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, taskID string) (*SearchIndexTask, error)
	// FindByRequestID 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*SearchIndexTask, error)
	// ClaimNext 抢占一个 pending 任务并置为 running（多实例安全，CAS）。
	// 无可抢占任务时返回 (nil, nil)。
	ClaimNext(ctx context.Context, now int64) (*SearchIndexTask, error)
	// UpdateProgress 推进游标与计数（仅 running 状态可更新，避免终态回退）。
	UpdateProgress(ctx context.Context, taskID, cursorValue string, processed, failed, total int64, now int64) error
	// Finish 写入终态（succeeded/failed/canceled）与失败原因。
	Finish(ctx context.Context, taskID, state, lastError string, now int64) error
	// List 按 cursor 分页查询（cursor 为上一页最小 id，0 表示从头）；limit<=0 用默认。
	List(ctx context.Context, state, cursor string, limit int) ([]*SearchIndexTask, string, error)
}

type defaultTaskModel struct {
	conn sqlx.SqlConn
}

// NewSearchIndexTaskModel 创建 SearchIndexTaskModel 实现。
func NewSearchIndexTaskModel(conn sqlx.SqlConn) SearchIndexTaskModel {
	return &defaultTaskModel{conn: conn}
}

const taskColumns = "id, task_id, scope, scope_value, state, cursor_value, total, processed, failed, target_index, alias, operator, request_id, last_error, ctime, mtime, started_at, finished_at"

func (m *defaultTaskModel) Insert(ctx context.Context, t *SearchIndexTask) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT IGNORE INTO search_index_task (task_id, scope, scope_value, state, cursor_value, total, processed, failed, target_index, alias, operator, request_id, last_error, ctime, mtime, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.TaskID, t.Scope, t.ScopeValue, t.State, t.CursorValue, t.Total, t.Processed, t.Failed,
		t.TargetIndex, t.Alias, t.Operator, t.RequestID, t.LastError, t.Ctime, t.Mtime, t.StartedAt, t.FinishedAt)
	if err != nil {
		return false, fmt.Errorf("search_index_task Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("search_index_task Insert RowsAffected: %w", err)
	}
	// 影响 0 行说明命中 uniq_request_id，属于幂等重复提交。
	return aff == 0, nil
}

func (m *defaultTaskModel) FindOne(ctx context.Context, taskID string) (*SearchIndexTask, error) {
	var t SearchIndexTask
	query := "SELECT " + taskColumns + " FROM search_index_task WHERE task_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_index_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultTaskModel) FindByRequestID(ctx context.Context, requestID string) (*SearchIndexTask, error) {
	var t SearchIndexTask
	query := "SELECT " + taskColumns + " FROM search_index_task WHERE request_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_index_task FindByRequestID: %w", err)
	}
	return &t, nil
}

func (m *defaultTaskModel) ClaimNext(ctx context.Context, now int64) (*SearchIndexTask, error) {
	var t SearchIndexTask
	// 先取候选，再用 state 条件做 CAS 更新；两个实例同时抢同一行时只有一个 RowsAffected=1。
	query := "SELECT " + taskColumns + " FROM search_index_task WHERE state = ? ORDER BY id ASC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &t, query, TaskStatePending); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_index_task ClaimNext select: %w", err)
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE search_index_task SET state = ?, started_at = ?, mtime = ? WHERE task_id = ? AND state = ?",
		TaskStateRunning, now, now, t.TaskID, TaskStatePending)
	if err != nil {
		return nil, fmt.Errorf("search_index_task ClaimNext update: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("search_index_task ClaimNext RowsAffected: %w", err)
	}
	if aff == 0 {
		// 被其它实例抢走，本轮视为无任务。
		return nil, nil
	}
	t.State = TaskStateRunning
	t.StartedAt = now
	t.Mtime = now
	return &t, nil
}

func (m *defaultTaskModel) UpdateProgress(ctx context.Context, taskID, cursorValue string, processed, failed, total int64, now int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE search_index_task SET cursor_value = ?, processed = ?, failed = ?, total = ?, mtime = ? WHERE task_id = ? AND state = ?",
		cursorValue, processed, failed, total, now, taskID, TaskStateRunning)
	if err != nil {
		return fmt.Errorf("search_index_task UpdateProgress: %w", err)
	}
	return nil
}

func (m *defaultTaskModel) Finish(ctx context.Context, taskID, state, lastError string, now int64) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE search_index_task SET state = ?, last_error = ?, finished_at = ?, mtime = ? WHERE task_id = ? AND state = ?",
		state, lastError, now, now, taskID, TaskStateRunning)
	if err != nil {
		return fmt.Errorf("search_index_task Finish: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("search_index_task Finish RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrTaskNotFound
	}
	return nil
}

func (m *defaultTaskModel) List(ctx context.Context, state, cursor string, limit int) ([]*SearchIndexTask, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	// cursor 是上一页最后一行的自增 id，转成 id 条件避免深分页。
	var (
		lastID int64
		rows   []*SearchIndexTask
	)
	if cursor != "" {
		if _, err := fmt.Sscanf(cursor, "%d", &lastID); err != nil {
			return nil, "", fmt.Errorf("search_index_task List cursor %q: %w", cursor, err)
		}
	}

	where := "1 = 1"
	var args []interface{}
	if state != "" {
		where += " AND state = ?"
		args = append(args, state)
	}
	if lastID > 0 {
		where += " AND id < ?"
		args = append(args, lastID)
	}

	query := "SELECT " + taskColumns + " FROM search_index_task WHERE " + where +
		" ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("search_index_task List: %w", err)
	}
	next := ""
	if len(rows) == limit {
		next = fmt.Sprintf("%d", rows[len(rows)-1].ID)
	}
	return rows, next, nil
}
