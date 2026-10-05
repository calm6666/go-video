package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// menuColumns 是 op_menu 的列清单，必须与
// deploy/migrations/operation/000001_create_operation_rbac_tables.sql 一致。
const menuColumns = "menu_id, parent_id, name, path, icon, sort, required_permission, state, ctime, mtime"

// Menu 对应 op_menu 表：后台菜单节点（仅 Web 后台使用，项目不支持小程序）。
// 树形用 parent_id 表达，本期不做无限层级校验，只禁止自引用与缺失父级。
type Menu struct {
	// MenuID 菜单 ID，主键。
	MenuID int64 `db:"menu_id"`
	// ParentID 父节点 ID，0 表示根节点。
	ParentID int64 `db:"parent_id"`
	// Name 菜单名。
	Name string `db:"name"`
	// Path 前端路由路径。
	Path string `db:"path"`
	// Icon 图标标识。
	Icon string `db:"icon"`
	// Sort 同级排序，小者在前。
	Sort int32 `db:"sort"`
	// RequiredPermission 可见所需权限，格式 "resource#action"；空表示登录即可见。
	RequiredPermission string `db:"required_permission"`
	// State 1 显示、2 隐藏。
	State int32 `db:"state"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// MenuModel 抽象 op_menu 表。
type MenuModel interface {
	// Insert 新建菜单节点，返回自增 menu_id。
	Insert(ctx context.Context, m *Menu) (int64, error)
	// Update 更新菜单节点（按 menu_id）。
	Update(ctx context.Context, m *Menu) error
	// FindOne 按 ID 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, menuID int64) (*Menu, error)
	// ListAll 列出全部节点（含隐藏，按 parent_id + sort 升序），菜单树组装在内存完成。
	ListAll(ctx context.Context) ([]*Menu, error)
	// CountChildren 统计子节点数量。
	CountChildren(ctx context.Context, menuID int64) (int64, error)
}

type defaultMenuModel struct {
	conn sqlx.SqlConn
}

// NewMenuModel 构造 op_menu 的 sqlx 实现。
func NewMenuModel(conn sqlx.SqlConn) MenuModel {
	return &defaultMenuModel{conn: conn}
}

func (m *defaultMenuModel) Insert(ctx context.Context, node *Menu) (int64, error) {
	now := nowUnix()
	if node.Ctime == 0 {
		node.Ctime = now
	}
	node.Mtime = now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_menu ("+menuColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		node.MenuID, node.ParentID, node.Name, node.Path, node.Icon, node.Sort,
		node.RequiredPermission, node.State, node.Ctime, node.Mtime)
	if err != nil {
		return 0, fmt.Errorf("op_menu Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("op_menu Insert LastInsertId: %w", err)
	}
	node.MenuID = id
	return id, nil
}

func (m *defaultMenuModel) Update(ctx context.Context, node *Menu) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE op_menu SET parent_id = ?, name = ?, path = ?, icon = ?, sort = ?, required_permission = ?, state = ?, mtime = ? WHERE menu_id = ?",
		node.ParentID, node.Name, node.Path, node.Icon, node.Sort,
		node.RequiredPermission, node.State, nowUnix(), node.MenuID)
	if err != nil {
		return fmt.Errorf("op_menu Update: %w", err)
	}
	return nil
}

func (m *defaultMenuModel) FindOne(ctx context.Context, menuID int64) (*Menu, error) {
	var node Menu
	query := "SELECT " + menuColumns + " FROM op_menu WHERE menu_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &node, query, menuID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_menu FindOne: %w", err)
	}
	return &node, nil
}

func (m *defaultMenuModel) ListAll(ctx context.Context) ([]*Menu, error) {
	var rows []*Menu
	query := "SELECT " + menuColumns + " FROM op_menu ORDER BY parent_id ASC, sort ASC, menu_id ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_menu ListAll: %w", err)
	}
	return rows, nil
}

func (m *defaultMenuModel) CountChildren(ctx context.Context, menuID int64) (int64, error) {
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, "SELECT COUNT(*) FROM op_menu WHERE parent_id = ?", menuID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("op_menu CountChildren: %w", err)
	}
	return n, nil
}
