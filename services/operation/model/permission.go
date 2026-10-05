package model

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// permissionFields 是 op_permission 的列清单。
const permissionFields = "permission_id, resource, action, domain, description, ctime"

// Permission 对应 op_permission 表：权限点（resource + action + 所属域）。
// domain 用于分组展示（video/catalog/rights/moderation/operation/system），
// 权限判定只依赖 resource + action。
type Permission struct {
	// PermissionID 权限点 ID，主键。
	PermissionID int64 `db:"permission_id"`
	// Resource 资源标识，如 video:submission、catalog:episode、ops:config；"*" 表示全部资源。
	Resource string `db:"resource"`
	// Action 动作标识，如 read、offline、approve；"*" 表示全部动作。
	Action string `db:"action"`
	// Domain 所属业务域。
	Domain string `db:"domain"`
	// Description 说明。
	Description string `db:"description"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
}

// PermissionModel 抽象 op_permission 表。
type PermissionModel interface {
	// Insert 新建权限点；(resource, action) 唯一。
	Insert(ctx context.Context, p *Permission) (int64, error)
	// FindOne 按 ID 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, permissionID int64) (*Permission, error)
	// FindByResourceAction 按 (resource, action) 查询；不存在返回 (nil, nil)。
	FindByResourceAction(ctx context.Context, resource, action string) (*Permission, error)
	// FindMany 批量查询，返回 ID → 权限点。
	FindMany(ctx context.Context, ids []int64) (map[int64]*Permission, error)
	// List 分页查询；domain 为空表示全部域。
	List(ctx context.Context, domain string, pn, ps int32) ([]*Permission, int64, error)
}

type defaultPermissionModel struct {
	conn sqlx.SqlConn
}

// NewPermissionModel 构造 op_permission 的 sqlx 实现。
func NewPermissionModel(conn sqlx.SqlConn) PermissionModel {
	return &defaultPermissionModel{conn: conn}
}

func (m *defaultPermissionModel) Insert(ctx context.Context, p *Permission) (int64, error) {
	if p.Ctime == 0 {
		p.Ctime = nowUnix()
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_permission ("+permissionFields+") VALUES (?, ?, ?, ?, ?, ?)",
		p.PermissionID, p.Resource, p.Action, p.Domain, p.Description, p.Ctime)
	if err != nil {
		return 0, fmt.Errorf("op_permission Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("op_permission Insert LastInsertId: %w", err)
	}
	p.PermissionID = id
	return id, nil
}

func (m *defaultPermissionModel) FindOne(ctx context.Context, permissionID int64) (*Permission, error) {
	var p Permission
	query := "SELECT " + permissionFields + " FROM op_permission WHERE permission_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &p, query, permissionID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_permission FindOne: %w", err)
	}
	return &p, nil
}

func (m *defaultPermissionModel) FindByResourceAction(ctx context.Context, resource, action string) (*Permission, error) {
	var p Permission
	query := "SELECT " + permissionFields + " FROM op_permission WHERE resource = ? AND action = ?"
	if err := m.conn.QueryRowCtx(ctx, &p, query, resource, action); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_permission FindByResourceAction: %w", err)
	}
	return &p, nil
}

func (m *defaultPermissionModel) FindMany(ctx context.Context, ids []int64) (map[int64]*Permission, error) {
	result := make(map[int64]*Permission, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	query := "SELECT " + permissionFields + " FROM op_permission WHERE permission_id IN (" + placeholders(len(ids)) + ")"
	var rows []*Permission
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if err == sql.ErrNoRows {
			return result, nil
		}
		return nil, fmt.Errorf("op_permission FindMany: %w", err)
	}
	for _, r := range rows {
		result[r.PermissionID] = r
	}
	return result, nil
}

func (m *defaultPermissionModel) List(ctx context.Context, domain string, pn, ps int32) ([]*Permission, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 2)
	if domain != "" {
		where += " AND domain = ?"
		args = append(args, domain)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM op_permission "+where, args...); err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("op_permission List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*Permission
	query := "SELECT " + permissionFields + " FROM op_permission " + where + " ORDER BY domain ASC, resource ASC, action ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if err == sql.ErrNoRows {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("op_permission List: %w", err)
	}
	return rows, total, nil
}
