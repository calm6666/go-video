package repository

// 本文件实现角色与权限点管理。
//
// RBAC 模型（README 有完整说明）：
//   管理员 ── op_admin_role ──> 角色 ── op_role_permission ──> 权限点(resource, action)
//   判定取角色并集，权限点支持通配（见 rbac.go）。
//
// 每一次影响授权关系的写入都会递增 RBAC 版本（invalidateRBAC），
// 使全部快照立即失效；角色停用/删除不删绑定数据，只让判定不再计入。

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"go-video/services/operation/model"
)

// rolePattern 角色标识：小写字母开头，允许小写字母/数字/下划线，总长 2~32。
var rolePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// CreateRoleInput 创建角色入参。
type CreateRoleInput struct {
	// Name 角色标识（唯一）。
	Name string
	// Title 展示名。
	Title string
	// PermissionIDs 初始权限点。
	PermissionIDs []int64
}

// RoleView 角色投影（含成员数与已绑定权限点）。
type RoleView struct {
	// RoleID 角色 ID。
	RoleID int64
	// Name 标识。
	Name string
	// Title 展示名。
	Title string
	// State 1 启用、2 停用。
	State int32
	// MemberCount 成员数。
	MemberCount int64
	// PermissionIDs 绑定的权限点 ID。
	PermissionIDs []int64
	// Ctime 创建时间。
	Ctime int64
	// Mtime 修改时间。
	Mtime int64
}

// PermissionView 权限点投影。
type PermissionView struct {
	// PermissionID 权限点 ID。
	PermissionID int64
	// Resource 资源标识。
	Resource string
	// Action 动作标识。
	Action string
	// Domain 所属域。
	Domain string
	// Description 说明。
	Description string
	// Ctime 创建时间。
	Ctime int64
}

// CreateRole 新建角色并绑定权限点。
func (r *Repository) CreateRole(ctx context.Context, actor Actor, in CreateRoleInput) (*RoleView, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, err
	}
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !rolePattern.MatchString(name) {
		return nil, fmt.Errorf("operation: invalid role name %q (need 2-32 chars, lowercase letter first, [a-z0-9_])", name)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = name
	}
	if len([]rune(title)) > 64 {
		return nil, fmt.Errorf("operation: role title too long (max 64)")
	}
	if err := r.validatePermissionIDs(ctx, in.PermissionIDs); err != nil {
		return nil, err
	}
	existed, err := r.roleMd.FindByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if existed != nil {
		return nil, model.ErrRoleExists
	}
	role := &model.Role{Name: name, Title: title, State: model.StateEnable, Operator: actor.AdminID}
	roleID, err := r.roleMd.Insert(ctx, role)
	if err != nil {
		return nil, err
	}
	if len(in.PermissionIDs) > 0 {
		if err := r.roleMd.GrantPermissions(ctx, roleID, in.PermissionIDs); err != nil {
			return nil, fmt.Errorf("operation: grant permissions for role %d: %w", roleID, err)
		}
	}
	r.invalidateRBAC(ctx)
	if err := r.writeAudit(ctx, actor, actionRoleCreate, "admin_role", fmt.Sprintf("%d", roleID), model.AuditResultOK); err != nil {
		return nil, err
	}
	return r.RoleView(ctx, roleID)
}

// RoleView 组装单个角色投影。
func (r *Repository) RoleView(ctx context.Context, roleID int64) (*RoleView, error) {
	role, err := r.roleMd.FindOne(ctx, roleID)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, model.ErrRoleNotFound
	}
	perms, err := r.roleMd.ListPermissionIDs(ctx, roleID)
	if err != nil {
		return nil, err
	}
	members, err := r.roleMd.CountMembers(ctx, roleID)
	if err != nil {
		return nil, err
	}
	return &RoleView{
		RoleID:        role.RoleID,
		Name:          role.Name,
		Title:         role.Title,
		State:         role.State,
		MemberCount:   members,
		PermissionIDs: perms,
		Ctime:         role.Ctime,
		Mtime:         role.Mtime,
	}, nil
}

// ListRoles 分页查询角色。
func (r *Repository) ListRoles(ctx context.Context, state int32, keyword string, pn, ps int32) ([]*RoleView, int64, error) {
	pn, ps = pagePair(pn, ps)
	rows, total, err := r.roleMd.List(ctx, state, strings.TrimSpace(keyword), pn, ps)
	if err != nil {
		return nil, 0, err
	}
	views := make([]*RoleView, 0, len(rows))
	for _, row := range rows {
		perms, err := r.roleMd.ListPermissionIDs(ctx, row.RoleID)
		if err != nil {
			return nil, 0, err
		}
		members, err := r.roleMd.CountMembers(ctx, row.RoleID)
		if err != nil {
			return nil, 0, err
		}
		views = append(views, &RoleView{
			RoleID:        row.RoleID,
			Name:          row.Name,
			Title:         row.Title,
			State:         row.State,
			MemberCount:   members,
			PermissionIDs: perms,
			Ctime:         row.Ctime,
			Mtime:         row.Mtime,
		})
	}
	return views, total, nil
}

// DeleteRole 删除角色。仍有成员时拒绝，避免“删角色导致一批管理员静默失权”。
// 需要先 AssignRoles 迁移成员。
func (r *Repository) DeleteRole(ctx context.Context, actor Actor, roleID int64) error {
	if err := r.requireOperator(actor); err != nil {
		return err
	}
	if roleID <= 0 {
		return fmt.Errorf("operation: invalid role_id %d", roleID)
	}
	role, err := r.roleMd.FindOne(ctx, roleID)
	if err != nil {
		return err
	}
	if role == nil {
		return model.ErrRoleNotFound
	}
	members, err := r.roleMd.CountMembers(ctx, roleID)
	if err != nil {
		return err
	}
	if members > 0 {
		return model.ErrRoleHasMembers
	}
	if err := r.roleMd.Delete(ctx, roleID); err != nil {
		return err
	}
	r.invalidateRBAC(ctx)
	return r.writeAudit(ctx, actor, actionRoleDelete, "admin_role", fmt.Sprintf("%d", roleID), model.AuditResultOK)
}

// AssignRoles 全量覆盖管理员角色；空集合表示清空权限。
func (r *Repository) AssignRoles(ctx context.Context, actor Actor, adminID int64, roleIDs []int64) ([]int64, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, err
	}
	if adminID <= 0 {
		return nil, model.ErrInvalidOperator
	}
	u, err := r.adminMd.FindOne(ctx, adminID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, model.ErrAdminNotFound
	}
	if err := r.validateRoleIDs(ctx, roleIDs); err != nil {
		return nil, err
	}
	if err := r.roleMd.ReplaceAdminRoles(ctx, adminID, roleIDs); err != nil {
		return nil, err
	}
	// 授权关系变了：作废权限快照，并吊销该账号会话缓存（网关侧 ttl 兜底窗口见 README）。
	r.invalidateRBAC(ctx)
	if err := r.writeAudit(ctx, actor, actionAdminAssignRoles, "admin_user", fmt.Sprintf("%d", adminID), model.AuditResultOK); err != nil {
		return nil, err
	}
	return r.roleMd.ListRoleIDsByAdmin(ctx, adminID)
}

// GrantRolePermissions 全量覆盖角色的权限点绑定（本期由 CreateRole 使用，
// 单独的 UpdateRole RPC 留待后续批次，见 README 已知缺口）。
func (r *Repository) GrantRolePermissions(ctx context.Context, actor Actor, roleID int64, permissionIDs []int64) error {
	if err := r.requireOperator(actor); err != nil {
		return err
	}
	role, err := r.roleMd.FindOne(ctx, roleID)
	if err != nil {
		return err
	}
	if role == nil {
		return model.ErrRoleNotFound
	}
	if err := r.validatePermissionIDs(ctx, permissionIDs); err != nil {
		return err
	}
	if err := r.roleMd.GrantPermissions(ctx, roleID, permissionIDs); err != nil {
		return err
	}
	r.invalidateRBAC(ctx)
	return r.writeAudit(ctx, actor, actionRoleGrant, "admin_role", fmt.Sprintf("%d", roleID), model.AuditResultOK)
}

// CreatePermission 新建权限点；(resource, action) 唯一，重复即视为已存在。
func (r *Repository) CreatePermission(ctx context.Context, actor Actor, resource, action, domain, description string) (*PermissionView, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, err
	}
	res := strings.ToLower(strings.TrimSpace(resource))
	act := strings.ToLower(strings.TrimSpace(action))
	if !isPermissionToken(res) || !isPermissionToken(act) {
		return nil, model.ErrPermissionInvalid
	}
	if strings.Contains(res, " ") || strings.Contains(act, " ") {
		return nil, model.ErrPermissionInvalid
	}
	dom := strings.ToLower(strings.TrimSpace(domain))
	if dom == "" {
		// 未指定域时取资源前缀，保证列表可按域筛选。
		dom = strings.SplitN(res, ":", 2)[0]
	}
	if len([]rune(description)) > maxRemarkLen {
		return nil, fmt.Errorf("operation: permission description too long (max %d)", maxRemarkLen)
	}
	existed, err := r.permMd.FindByResourceAction(ctx, res, act)
	if err != nil {
		return nil, err
	}
	if existed != nil {
		return nil, model.ErrPermissionExists
	}
	row := &model.Permission{Resource: res, Action: act, Domain: dom, Description: strings.TrimSpace(description)}
	id, err := r.permMd.Insert(ctx, row)
	if err != nil {
		return nil, err
	}
	// 新增权限点本身不改变既有绑定，但菜单可见性可能引用它，故一并作废菜单缓存。
	r.invalidateRBAC(ctx)
	r.bumpMenuVersion(ctx)
	if err := r.writeAudit(ctx, actor, actionPermissionCreate, "admin_permission", fmt.Sprintf("%d", id), model.AuditResultOK); err != nil {
		return nil, err
	}
	return &PermissionView{
		PermissionID: id,
		Resource:     res,
		Action:       act,
		Domain:       dom,
		Description:  row.Description,
		Ctime:        row.Ctime,
	}, nil
}

// ListPermissions 分页查询权限点。
func (r *Repository) ListPermissions(ctx context.Context, domain string, pn, ps int32) ([]*PermissionView, int64, error) {
	pn, ps = pagePair(pn, ps)
	rows, total, err := r.permMd.List(ctx, strings.TrimSpace(domain), pn, ps)
	if err != nil {
		return nil, 0, err
	}
	views := make([]*PermissionView, 0, len(rows))
	for _, row := range rows {
		views = append(views, &PermissionView{
			PermissionID: row.PermissionID,
			Resource:     row.Resource,
			Action:       row.Action,
			Domain:       row.Domain,
			Description:  row.Description,
			Ctime:        row.Ctime,
		})
	}
	return views, total, nil
}

// validatePermissionIDs 校验权限点存在（拒绝悬空 ID，防止授权表被写入无效引用）。
func (r *Repository) validatePermissionIDs(ctx context.Context, ids []int64) error {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return model.ErrPermissionInvalid
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		p, err := r.permMd.FindOne(ctx, id)
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("%w: permission_id %d", model.ErrPermissionNotFound, id)
		}
	}
	return nil
}

// isPermissionToken 资源/动作片段校验：小写字母或 * 开头，允许 a-z0-9_*: ，长度 1~64。
func isPermissionToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == ':' || r == '*':
		default:
			return false
		}
	}
	// "*" 合法；但 "x*" 这类混合形态容易与 ":*" 前缀语义混淆，直接拒绝。
	if strings.Contains(s, "*") && s != wildcardAll && !strings.HasSuffix(s, wildcardSuffix) && !strings.HasPrefix(s, wildcardPrefix) {
		return false
	}
	return true
}
