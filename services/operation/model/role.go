package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// roleFields 是 op_role 的列清单。
const roleFields = "role_id, name, title, state, operator, ctime, mtime"

// Role 对应 op_role 表：后台角色。
type Role struct {
	// RoleID 角色 ID，主键。
	RoleID int64 `db:"role_id"`
	// Name 角色标识，全局唯一（如 super_admin、content_ops）。
	Name string `db:"name"`
	// Title 展示名。
	Title string `db:"title"`
	// State 1 启用、2 停用；停用角色不参与权限判定。
	State int32 `db:"state"`
	// Operator 最后修改人 admin_id。
	Operator int64 `db:"operator"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// AdminRBAC 是某管理员的角色 + 权限并集快照（一次 JOIN 载入，供判定与缓存）。
type AdminRBAC struct {
	// Roles 生效中的角色（按 role_id 升序，含未绑定权限的角色）。
	Roles []*Role
	// Permissions 角色并集后的权限点（已按 permission_id 去重）。
	Permissions []*Permission
}

// RoleGrant 是按角色分组的权限绑定：一个启用角色 + 它的权限点。
// 分组形态让 VerifyAdminPermission 能回传“命中了哪些角色”，
// 而 AdminRBAC 的并集形态用于菜单过滤与列表展示。
type RoleGrant struct {
	// Role 生效中的角色。
	Role *Role
	// Permissions 该角色绑定的权限点（可能为空）。
	Permissions []*Permission
}

// RoleModel 抽象 op_role 及其两张关系表 op_role_permission、op_admin_role。
type RoleModel interface {
	// Insert 新建角色，返回自增 role_id。
	Insert(ctx context.Context, r *Role) (int64, error)
	// FindOne 按 ID 查询角色；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, roleID int64) (*Role, error)
	// FindByName 按标识查询角色；不存在返回 (nil, nil)。
	FindByName(ctx context.Context, name string) (*Role, error)
	// List 分页查询角色；state=0 表示不过滤。
	List(ctx context.Context, state int32, keyword string, pn, ps int32) ([]*Role, int64, error)
	// Delete 删除角色及其权限绑定（调用方必须先确认无成员）。
	Delete(ctx context.Context, roleID int64) error

	// GrantPermissions 全量覆盖角色的权限点绑定。
	GrantPermissions(ctx context.Context, roleID int64, permissionIDs []int64) error
	// ListPermissionIDs 查询角色已绑定的权限点 ID。
	ListPermissionIDs(ctx context.Context, roleID int64) ([]int64, error)
	// CountMembers 统计角色成员数（删除保护）。
	CountMembers(ctx context.Context, roleID int64) (int64, error)

	// ReplaceAdminRoles 全量覆盖管理员的角色绑定。
	ReplaceAdminRoles(ctx context.Context, adminID int64, roleIDs []int64) error
	// ListRoleIDsByAdmin 查询管理员已绑定的角色 ID。
	ListRoleIDsByAdmin(ctx context.Context, adminID int64) ([]int64, error)
	// ListAdminsByRole 查询某角色下的全部管理员 ID（批量通知/回收权限用）。
	ListAdminsByRole(ctx context.Context, roleID int64) ([]int64, error)
	// ListRoleBindings 批量查询多个管理员的角色绑定（列表页一次性取回，避免 N+1）。
	ListRoleBindings(ctx context.Context, adminIDs []int64) ([]*AdminRolePair, error)
	// LoadAdminGrants 按角色分组载入管理员的生效权限（只统计 state=1 的角色；
	// 未绑定权限的角色也会返回，Permissions 为空）。
	LoadAdminGrants(ctx context.Context, adminID int64) ([]*RoleGrant, error)
}

// AdminRolePair 是“管理员 → 角色”的一行绑定，用于列表页批量取回角色名。
type AdminRolePair struct {
	AdminID  int64  `db:"admin_id"`
	RoleID   int64  `db:"role_id"`
	RoleName string `db:"role_name"`
}

type defaultRoleModel struct {
	conn sqlx.SqlConn
}

// NewRoleModel 构造 op_role 的 sqlx 实现。
func NewRoleModel(conn sqlx.SqlConn) RoleModel {
	return &defaultRoleModel{conn: conn}
}

func (m *defaultRoleModel) Insert(ctx context.Context, r *Role) (int64, error) {
	now := nowUnix()
	if r.Ctime == 0 {
		r.Ctime = now
	}
	r.Mtime = now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_role ("+roleFields+") VALUES (?, ?, ?, ?, ?, ?, ?)",
		r.RoleID, r.Name, r.Title, r.State, r.Operator, r.Ctime, r.Mtime)
	if err != nil {
		return 0, fmt.Errorf("op_role Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("op_role Insert LastInsertId: %w", err)
	}
	r.RoleID = id
	return id, nil
}

func (m *defaultRoleModel) FindOne(ctx context.Context, roleID int64) (*Role, error) {
	var r Role
	query := "SELECT " + roleFields + " FROM op_role WHERE role_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, roleID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_role FindOne: %w", err)
	}
	return &r, nil
}

func (m *defaultRoleModel) FindByName(ctx context.Context, name string) (*Role, error) {
	var r Role
	query := "SELECT " + roleFields + " FROM op_role WHERE name = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, name); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_role FindByName: %w", err)
	}
	return &r, nil
}

func (m *defaultRoleModel) List(ctx context.Context, state int32, keyword string, pn, ps int32) ([]*Role, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 2)
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}
	if keyword != "" {
		where += " AND (name LIKE ? OR title LIKE ?)"
		args = append(args, "%"+keyword+"%", "%"+keyword+"%")
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM op_role "+where, args...); err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("op_role List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*Role
	query := "SELECT " + roleFields + " FROM op_role " + where + " ORDER BY role_id ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if err == sql.ErrNoRows {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("op_role List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultRoleModel) Delete(ctx context.Context, roleID int64) error {
	return m.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		if _, err := session.ExecCtx(ctx, "DELETE FROM op_role_permission WHERE role_id = ?", roleID); err != nil {
			return fmt.Errorf("op_role_permission delete: %w", err)
		}
		if _, err := session.ExecCtx(ctx, "DELETE FROM op_admin_role WHERE role_id = ?", roleID); err != nil {
			return fmt.Errorf("op_admin_role delete: %w", err)
		}
		if _, err := session.ExecCtx(ctx, "DELETE FROM op_role WHERE role_id = ?", roleID); err != nil {
			return fmt.Errorf("op_role delete: %w", err)
		}
		return nil
	})
}

func (m *defaultRoleModel) GrantPermissions(ctx context.Context, roleID int64, permissionIDs []int64) error {
	return m.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		if _, err := session.ExecCtx(ctx, "DELETE FROM op_role_permission WHERE role_id = ?", roleID); err != nil {
			return fmt.Errorf("op_role_permission clear: %w", err)
		}
		for _, pid := range permissionIDs {
			if pid <= 0 {
				return errors.New("op_role_permission: invalid permission_id")
			}
			if _, err := session.ExecCtx(ctx,
				"INSERT INTO op_role_permission (role_id, permission_id, ctime) VALUES (?, ?, ?)",
				roleID, pid, nowUnix()); err != nil {
				return fmt.Errorf("op_role_permission insert: %w", err)
			}
		}
		return nil
	})
}

func (m *defaultRoleModel) ListPermissionIDs(ctx context.Context, roleID int64) ([]int64, error) {
	var ids []int64
	query := "SELECT permission_id FROM op_role_permission WHERE role_id = ? ORDER BY permission_id ASC"
	if err := m.conn.QueryRowsCtx(ctx, &ids, query, roleID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_role_permission ListPermissionIDs: %w", err)
	}
	return ids, nil
}

func (m *defaultRoleModel) CountMembers(ctx context.Context, roleID int64) (int64, error) {
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, "SELECT COUNT(*) FROM op_admin_role WHERE role_id = ?", roleID); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("op_admin_role CountMembers: %w", err)
	}
	return n, nil
}

func (m *defaultRoleModel) ReplaceAdminRoles(ctx context.Context, adminID int64, roleIDs []int64) error {
	return m.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		if _, err := session.ExecCtx(ctx, "DELETE FROM op_admin_role WHERE admin_id = ?", adminID); err != nil {
			return fmt.Errorf("op_admin_role clear: %w", err)
		}
		seen := make(map[int64]struct{}, len(roleIDs))
		for _, rid := range roleIDs {
			if rid <= 0 {
				return errors.New("op_admin_role: invalid role_id")
			}
			if _, ok := seen[rid]; ok {
				continue // 入参去重，避免唯一索引冲突
			}
			seen[rid] = struct{}{}
			if _, err := session.ExecCtx(ctx,
				"INSERT INTO op_admin_role (admin_id, role_id, ctime) VALUES (?, ?, ?)",
				adminID, rid, nowUnix()); err != nil {
				return fmt.Errorf("op_admin_role insert: %w", err)
			}
		}
		return nil
	})
}

func (m *defaultRoleModel) ListRoleIDsByAdmin(ctx context.Context, adminID int64) ([]int64, error) {
	var ids []int64
	if err := m.conn.QueryRowsCtx(ctx, &ids,
		"SELECT role_id FROM op_admin_role WHERE admin_id = ? ORDER BY role_id ASC", adminID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_role ListRoleIDsByAdmin: %w", err)
	}
	return ids, nil
}

func (m *defaultRoleModel) ListAdminsByRole(ctx context.Context, roleID int64) ([]int64, error) {
	var ids []int64
	if err := m.conn.QueryRowsCtx(ctx, &ids,
		"SELECT admin_id FROM op_admin_role WHERE role_id = ? ORDER BY admin_id ASC", roleID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_role ListAdminsByRole: %w", err)
	}
	return ids, nil
}

func (m *defaultRoleModel) ListRoleBindings(ctx context.Context, adminIDs []int64) ([]*AdminRolePair, error) {
	if len(adminIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(adminIDs))
	for _, id := range adminIDs {
		args = append(args, id)
	}
	query := "SELECT ar.admin_id, ar.role_id, r.name AS role_name FROM op_admin_role ar" +
		" JOIN op_role r ON r.role_id = ar.role_id" +
		" WHERE ar.admin_id IN (" + placeholders(len(adminIDs)) + ") ORDER BY ar.admin_id ASC, ar.role_id ASC"
	var rows []*AdminRolePair
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_role ListRoleBindings: %w", err)
	}
	return rows, nil
}

// rbacRow 是“管理员 → 角色 → 权限点”的扁平查询行，一次 JOIN 载入按角色分组的数据。
type rbacRow struct {
	RoleID       int64  `db:"role_id"`
	RoleName     string `db:"role_name"`
	RoleTitle    string `db:"role_title"`
	Operator     int64  `db:"operator"`
	State        int32  `db:"state"`
	RoleCtime    int64  `db:"role_ctime"`
	RoleMtime    int64  `db:"role_mtime"`
	PermissionID int64  `db:"permission_id"`
	Resource     string `db:"resource"`
	Action       string `db:"action"`
	Domain       string `db:"domain"`
	Description  string `db:"description"`
	PermCtime    int64  `db:"perm_ctime"`
}

func (m *defaultRoleModel) LoadAdminGrants(ctx context.Context, adminID int64) ([]*RoleGrant, error) {
	// LEFT JOIN + IFNULL：未绑定权限的角色同样返回，避免“角色不可见”造成的排障困惑。
	query := "SELECT r.role_id, r.name AS role_name, r.title AS role_title, r.operator, r.state," +
		" r.ctime AS role_ctime, r.mtime AS role_mtime," +
		" IFNULL(p.permission_id, 0) AS permission_id, IFNULL(p.resource, '') AS resource," +
		" IFNULL(p.action, '') AS action, IFNULL(p.domain, '') AS domain," +
		" IFNULL(p.description, '') AS description, IFNULL(p.ctime, 0) AS perm_ctime" +
		" FROM op_admin_role ar" +
		" JOIN op_role r ON r.role_id = ar.role_id AND r.state = ?" +
		" LEFT JOIN op_role_permission rp ON rp.role_id = ar.role_id" +
		" LEFT JOIN op_permission p ON p.permission_id = rp.permission_id" +
		" WHERE ar.admin_id = ?" +
		" ORDER BY r.role_id ASC, p.permission_id ASC"
	var rows []*rbacRow
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, StateEnable, adminID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_role LoadAdminGrants: %w", err)
	}

	grants := make([]*RoleGrant, 0, 4)
	index := make(map[int64]*RoleGrant, 4)
	for _, row := range rows {
		g, ok := index[row.RoleID]
		if !ok {
			g = &RoleGrant{Role: &Role{
				RoleID:   row.RoleID,
				Name:     row.RoleName,
				Title:    row.RoleTitle,
				State:    row.State,
				Operator: row.Operator,
				Ctime:    row.RoleCtime,
				Mtime:    row.RoleMtime,
			}}
			index[row.RoleID] = g
			grants = append(grants, g)
		}
		if row.PermissionID > 0 {
			g.Permissions = append(g.Permissions, &Permission{
				PermissionID: row.PermissionID,
				Resource:     row.Resource,
				Action:       row.Action,
				Domain:       row.Domain,
				Description:  row.Description,
				Ctime:        row.PermCtime,
			})
		}
	}
	return grants, nil
}
