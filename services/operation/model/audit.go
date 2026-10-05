package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// auditColumns 是 op_audit_index 的列清单，必须与迁移 SQL 一致。
const auditColumns = "id, admin_id, action, resource_type, resource_id, result, ip_hash, user_agent," +
	" trace_id, request_id, ctime"

// AuditIndex 对应 op_audit_index 表：管理操作审计**索引**。
// 只回答“谁在何时对哪个聚合做了什么、结果如何”，并带 trace_id/request_id 便于回溯；
// 请求正文、前后快照、审核证据由被操作的领域服务与后续 services/audit 保留。
// IP 以哈希落库（不落明文），UA 截断存储。
type AuditIndex struct {
	// ID 自增主键。
	ID int64 `db:"id"`
	// AdminID 操作管理员。
	AdminID int64 `db:"admin_id"`
	// Action 动作标识，如 admin_user.create、ops_config.save、admin_task.run。
	Action string `db:"action"`
	// ResourceType 目标聚合类型，如 admin_user、ops_config、video:submission。
	ResourceType string `db:"resource_type"`
	// ResourceID 目标聚合 ID（字符串，兼容不同主键形态）。
	ResourceID string `db:"resource_id"`
	// Result 结果：ok/denied/error。
	Result string `db:"result"`
	// IPHash 来源 IP 哈希。
	IPHash string `db:"ip_hash"`
	// UserAgent 客户端 UA（截断）。
	UserAgent string `db:"user_agent"`
	// TraceID 链路 ID。
	TraceID string `db:"trace_id"`
	// RequestID 幂等/请求 ID。
	RequestID string `db:"request_id"`
	// Ctime 发生时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Username 仅由关联查询填充（展示用），不是 op_audit_index 的列。
	Username string `db:"username"`
}

// AuditFilter 是审计索引查询条件；零值表示不过滤。
type AuditFilter struct {
	AdminID      int64
	Action       string
	ResourceType string
	ResourceID   string
	StartAt      int64
	EndAt        int64
	Pn           int32
	Ps           int32
}

// AuditIndexModel 抽象 op_audit_index 表。
// 本表只追加、不更新：不提供任何 UPDATE/DELETE 方法。
type AuditIndexModel interface {
	// Insert 追加一条审计索引。
	Insert(ctx context.Context, a *AuditIndex) (int64, error)
	// List 按条件分页查询，时间倒序；返回行含关联出的管理员用户名。
	List(ctx context.Context, f AuditFilter) ([]*AuditIndex, int64, error)
}

type defaultAuditIndexModel struct {
	conn sqlx.SqlConn
}

// NewAuditIndexModel 构造 op_audit_index 的 sqlx 实现。
func NewAuditIndexModel(conn sqlx.SqlConn) AuditIndexModel {
	return &defaultAuditIndexModel{conn: conn}
}

func (m *defaultAuditIndexModel) Insert(ctx context.Context, a *AuditIndex) (int64, error) {
	if a.Ctime == 0 {
		a.Ctime = nowUnix()
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_audit_index ("+auditColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.AdminID, a.Action, a.ResourceType, a.ResourceID, a.Result,
		a.IPHash, a.UserAgent, a.TraceID, a.RequestID, a.Ctime)
	if err != nil {
		return 0, fmt.Errorf("op_audit_index Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("op_audit_index Insert LastInsertId: %w", err)
	}
	a.ID = id
	return id, nil
}

func (m *defaultAuditIndexModel) List(ctx context.Context, f AuditFilter) ([]*AuditIndex, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 6)
	if f.AdminID > 0 {
		where += " AND a.admin_id = ?"
		args = append(args, f.AdminID)
	}
	if f.Action != "" {
		where += " AND a.action = ?"
		args = append(args, f.Action)
	}
	if f.ResourceType != "" {
		where += " AND a.resource_type = ?"
		args = append(args, f.ResourceType)
	}
	if f.ResourceID != "" {
		where += " AND a.resource_id = ?"
		args = append(args, f.ResourceID)
	}
	if f.StartAt > 0 {
		where += " AND a.ctime >= ?"
		args = append(args, f.StartAt)
	}
	if f.EndAt > 0 {
		where += " AND a.ctime < ?"
		args = append(args, f.EndAt)
	}

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM op_audit_index a "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("op_audit_index List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*AuditIndex
	query := "SELECT a.id, a.admin_id, a.action, a.resource_type, a.resource_id, a.result, a.ip_hash," +
		" a.user_agent, a.trace_id, a.request_id, a.ctime, IFNULL(u.username, '') AS username" +
		" FROM op_audit_index a LEFT JOIN op_admin_user u ON u.admin_id = a.admin_id" +
		" " + where + " ORDER BY a.id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("op_audit_index List: %w", err)
	}
	return rows, total, nil
}
