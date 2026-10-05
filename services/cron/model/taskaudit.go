package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// taskAuditColumns 必须与
// deploy/migrations/cron/000001_create_cron_task_tables.sql 一致。
const taskAuditColumns = "id, task_key, action, from_state, to_state, operator, detail, trace_id, ctime"

// TaskAudit 任务变更审计（cron_task_audit 表）。
//
// AGENTS.md §4/§8 要求「暂停/恢复/停用/人工重试」留痕可追溯：
// 这里只记录调度面的变更（谁、什么时候、把哪个任务从什么状态改到什么状态、为什么），
// 不复制业务数据，也不记录任务输出（任务输出在 cron_task_run.result_summary，摘要有上限）。
//
// 表只追加、不更新：from_state/to_state 存可读文本（enabled/paused/disabled/…），
// 便于人工核对，不依赖枚举数值。
type TaskAudit struct {
	ID        int64  `db:"id"`
	TaskKey   string `db:"task_key"`
	Action    string `db:"action"`     // AuditAction*
	FromState string `db:"from_state"` // 变更前的可读状态，新建为空串
	ToState   string `db:"to_state"`   // 变更后的可读状态
	Operator  string `db:"operator"`   // 操作人（管理员 mid 字符串或实例名）
	Detail    string `db:"detail"`     // 变更摘要 JSON 文本，不含密钥与事件正文
	TraceID   string `db:"trace_id"`
	Ctime     int64  `db:"ctime"`
}

// TaskAuditModel cron_task_audit 表读写接口。
type TaskAuditModel interface {
	// Insert 追加一条审计。审计写入失败必须让业务操作整体失败（同事务），
	// 不允许「操作成功但无痕迹」。
	Insert(ctx context.Context, tx sqlx.Session, a *TaskAudit) error
	// ListByCursor 按 id 倒序游标分页（cursorID 为上一页最后一行 id，0 表示从头）。
	ListByCursor(ctx context.Context, taskKey, action string, ctimeFrom, ctimeTo, cursorID int64,
		limit int) ([]*TaskAudit, int64, error)
	// DeleteBefore 按 ctime 清理超期审计（清理任务专用，须先完成归档）。
	DeleteBefore(ctx context.Context, before int64, limit int64) (int64, error)
	// CountByFilter 统计过滤条件下的审计条数（列表页 total，走 idx_task_ctime/idx_action_ctime）。
	CountByFilter(ctx context.Context, taskKey, action string, ctimeFrom, ctimeTo int64) (int64, error)
}

type defaultTaskAuditModel struct {
	conn sqlx.SqlConn
}

// NewTaskAuditModel 创建 TaskAuditModel 实现。
func NewTaskAuditModel(conn sqlx.SqlConn) TaskAuditModel {
	return &defaultTaskAuditModel{conn: conn}
}

func (m *defaultTaskAuditModel) Insert(ctx context.Context, tx sqlx.Session, a *TaskAudit) error {
	if a.TaskKey == "" {
		return ErrTaskKeyEmpty
	}
	if a.Action == "" {
		return errors.New("cron: audit action is required")
	}
	if a.Ctime == 0 {
		a.Ctime = nowUnix()
	}
	session := sqlx.Session(m.conn)
	if tx != nil {
		session = tx
	}
	res, err := session.ExecCtx(ctx,
		"INSERT INTO cron_task_audit (task_key, action, from_state, to_state, operator, detail, trace_id, ctime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		a.TaskKey, a.Action, a.FromState, a.ToState, a.Operator,
		truncate(a.Detail, MaxResultSummaryBytes*2), a.TraceID, a.Ctime)
	if err != nil {
		return fmt.Errorf("cron_task_audit Insert: %w", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		a.ID = id
	}
	return nil
}

func (m *defaultTaskAuditModel) ListByCursor(
	ctx context.Context, taskKey, action string, ctimeFrom, ctimeTo, cursorID int64, limit int,
) ([]*TaskAudit, int64, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
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
	if cursorID > 0 {
		where += " AND id < ?"
		args = append(args, cursorID)
	}
	args = append(args, limit)

	var rows []*TaskAudit
	query := "SELECT " + taskAuditColumns + " FROM cron_task_audit WHERE " + where +
		" ORDER BY id DESC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("cron_task_audit ListByCursor: %w", err)
	}
	next := int64(0)
	if len(rows) == limit {
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}

func (m *defaultTaskAuditModel) DeleteBefore(
	ctx context.Context, before int64, limit int64,
) (int64, error) {
	if before <= 0 {
		return 0, errors.New("cron: delete cutoff must be positive")
	}
	if limit <= 0 {
		limit = 1000
	}
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM cron_task_audit WHERE ctime < ? LIMIT ?", before, limit)
	if err != nil {
		return 0, fmt.Errorf("cron_task_audit DeleteBefore: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cron_task_audit DeleteBefore RowsAffected: %w", err)
	}
	return affected, nil
}
