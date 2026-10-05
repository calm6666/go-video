package repository

// 本文件实现后台菜单树的读取与保存。
//
// 可见性依据是“角色并集 + 节点 required_permission”，而不是前端硬编码：
// 前端只拿到该管理员真正可见的节点，避免用猜路由的方式探测后台功能。
// 全量节点按菜单版本号缓存，权限过滤在内存里按管理员做，
// 这样节点变更（SaveMenu）只需 bump 版本，无需按 admin 维度清理缓存。

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/operation/model"
)

// requiredPermissionSep 分隔 required_permission 的 resource 与 action。
// 资源自身含冒号（如 video:submission），因此用 "#" 而非 ":"。
const requiredPermissionSep = "#"

// MenuInput SaveMenu 入参（MenuID=0 表示新建）。
type MenuInput struct {
	MenuID             int64
	ParentID           int64
	Name               string
	Path               string
	Icon               string
	Sort               int32
	RequiredPermission string
	State              int32
}

// MenuView 菜单节点投影。
type MenuView struct {
	MenuID             int64
	ParentID           int64
	Name               string
	Path               string
	Icon               string
	Sort               int32
	RequiredPermission string
	State              int32
	Ctime              int64
	Mtime              int64
}

// parseRequiredPermission 解析 "resource#action"。
// 空串表示“登录即可见”，返回 ok=true 且 needPerm=false。
func parseRequiredPermission(s string) (resource, action string, needPerm bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false, nil
	}
	if strings.Contains(s, ":") && !strings.Contains(s, requiredPermissionSep) {
		return "", "", false, fmt.Errorf(
			"operation: required_permission %q must be formatted as resource%saction (e.g. video:submission%soffline)",
			s, requiredPermissionSep, requiredPermissionSep)
	}
	parts := strings.SplitN(s, requiredPermissionSep, 2)
	if len(parts) != 2 {
		return "", "", false, fmt.Errorf("operation: required_permission %q missing %q separator", s, requiredPermissionSep)
	}
	resource = strings.ToLower(strings.TrimSpace(parts[0]))
	action = strings.ToLower(strings.TrimSpace(parts[1]))
	if !isPermissionToken(resource) || !isPermissionToken(action) {
		return "", "", false, model.ErrPermissionInvalid
	}
	return resource, action, true, nil
}

// menuVersion 读取（或首次分配）菜单版本号。
func (r *Repository) menuVersion(ctx context.Context) int64 {
	ver := r.cache.version(ctx, keyMenuVersion)
	if ver > 0 {
		return ver
	}
	// 首次访问时把版本 key 建起来，后续 bump 才能从 1 递增而不是始终为 0。
	r.cache.bumpVersion(ctx, keyMenuVersion)
	return r.cache.version(ctx, keyMenuVersion)
}

// bumpMenuVersion 递增菜单版本，使全部缓存树立即失效。
func (r *Repository) bumpMenuVersion(ctx context.Context) {
	r.cache.bumpVersion(ctx, keyMenuVersion)
}

// loadAllMenus 读取全量菜单节点（含隐藏），优先缓存。
func (r *Repository) loadAllMenus(ctx context.Context) ([]*model.Menu, bool, error) {
	ver := r.menuVersion(ctx)
	key := fmt.Sprintf(keyMenuTree, ver)
	var cached []*model.Menu
	if r.cache.getJSON(ctx, key, &cached) {
		return cached, true, nil
	}
	rows, err := r.menuMd.ListAll(ctx)
	if err != nil {
		return nil, false, err
	}
	r.cache.setJSON(ctx, key, rows, r.cfg.MenuTTL)
	return rows, false, nil
}

// GetMenu 返回该管理员可见的菜单节点（按角色并集过滤）与可缓存秒数。
func (r *Repository) GetMenu(ctx context.Context, adminID int64) ([]*MenuView, int, error) {
	if adminID <= 0 {
		return nil, 0, model.ErrInvalidOperator
	}
	snapshot, err := r.AdminPermissions(ctx, adminID)
	if err != nil {
		return nil, 0, err
	}
	if snapshot.State != model.AdminStateNormal {
		return nil, 0, model.ErrAdminDisabled
	}
	nodes, _, err := r.loadAllMenus(ctx)
	if err != nil {
		return nil, 0, err
	}
	views := make([]*MenuView, 0, len(nodes))
	for _, n := range nodes {
		if n.State != model.StateEnable {
			continue
		}
		resource, action, need, err := parseRequiredPermission(n.RequiredPermission)
		if err != nil {
			// 配置写错的节点跳过而不是整体失败，避免一个坏节点让后台菜单全白。
			continue
		}
		if need {
			allowed, _ := snapshot.allows(resource, action)
			if !allowed {
				continue
			}
		}
		views = append(views, menuViewOf(n))
	}
	return views, r.cfg.MenuTTL, nil
}

// SaveMenu 新建或更新菜单节点。
func (r *Repository) SaveMenu(ctx context.Context, actor Actor, in MenuInput) (*MenuView, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, fmt.Errorf("operation: menu name required and must be <= 64 chars")
	}
	path := strings.TrimSpace(in.Path)
	if len([]rune(path)) > 255 {
		return nil, fmt.Errorf("operation: menu path too long (max 255)")
	}
	if len([]rune(in.Icon)) > 64 {
		return nil, fmt.Errorf("operation: menu icon too long (max 64)")
	}
	required := strings.ToLower(strings.TrimSpace(in.RequiredPermission))
	if _, _, _, err := parseRequiredPermission(required); err != nil {
		return nil, err
	}
	state := in.State
	if state == 0 {
		state = model.StateEnable
	}
	if state != model.StateEnable && state != model.StateDisable {
		return nil, fmt.Errorf("operation: invalid menu state %d (1 show, 2 hide)", state)
	}
	if in.ParentID < 0 {
		return nil, fmt.Errorf("operation: invalid parent_id %d", in.ParentID)
	}

	if in.MenuID == 0 {
		node := &model.Menu{
			ParentID:           in.ParentID,
			Name:               name,
			Path:               path,
			Icon:               in.Icon,
			Sort:               in.Sort,
			RequiredPermission: required,
			State:              state,
		}
		id, err := r.menuMd.Insert(ctx, node)
		if err != nil {
			return nil, err
		}
		r.bumpMenuVersion(ctx)
		if err := r.writeAudit(ctx, actor, actionMenuSave, "admin_menu", fmt.Sprintf("%d", id), model.AuditResultOK); err != nil {
			return nil, err
		}
		return menuViewOf(node), nil
	}

	existing, err := r.menuMd.FindOne(ctx, in.MenuID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, model.ErrMenuNotFound
	}
	if existing.MenuID == in.ParentID {
		return nil, model.ErrMenuParentSelf
	}
	if in.ParentID > 0 {
		parent, err := r.menuMd.FindOne(ctx, in.ParentID)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			return nil, model.ErrMenuNotFound
		}
		if err := r.assertNoMenuCycle(ctx, in.MenuID, in.ParentID); err != nil {
			return nil, err
		}
	}
	node := &model.Menu{
		MenuID:             in.MenuID,
		ParentID:           in.ParentID,
		Name:               name,
		Path:               path,
		Icon:               in.Icon,
		Sort:               in.Sort,
		RequiredPermission: required,
		State:              state,
	}
	if err := r.menuMd.Update(ctx, node); err != nil {
		return nil, err
	}
	r.bumpMenuVersion(ctx)
	if err := r.writeAudit(ctx, actor, actionMenuSave, "admin_menu", fmt.Sprintf("%d", in.MenuID), model.AuditResultOK); err != nil {
		return nil, err
	}
	return menuViewOf(node), nil
}

// assertNoMenuCycle 沿父链上溯，防止把节点挂到自己的后代下形成环。
// 层级很浅（后台菜单一般 <= 3 层），因此只做多跳保护与访问计数上界。
func (r *Repository) assertNoMenuCycle(ctx context.Context, menuID, parentID int64) error {
	seen := map[int64]struct{}{menuID: {}}
	cur := parentID
	for i := 0; cur > 0 && i < maxMenuDepth; i++ {
		if _, ok := seen[cur]; ok {
			return model.ErrMenuParentSelf
		}
		seen[cur] = struct{}{}
		p, err := r.menuMd.FindOne(ctx, cur)
		if err != nil {
			return err
		}
		if p == nil {
			return model.ErrMenuNotFound
		}
		cur = p.ParentID
	}
	if cur > 0 {
		return fmt.Errorf("operation: menu hierarchy deeper than %d levels", maxMenuDepth)
	}
	return nil
}

// maxMenuDepth 菜单允许的最大父链长度。
const maxMenuDepth = 8

// menuViewOf 把 model 节点转为投影。
func menuViewOf(n *model.Menu) *MenuView {
	if n == nil {
		return nil
	}
	return &MenuView{
		MenuID:             n.MenuID,
		ParentID:           n.ParentID,
		Name:               n.Name,
		Path:               n.Path,
		Icon:               n.Icon,
		Sort:               n.Sort,
		RequiredPermission: n.RequiredPermission,
		State:              n.State,
		Ctime:              n.Ctime,
		Mtime:              n.Mtime,
	}
}
