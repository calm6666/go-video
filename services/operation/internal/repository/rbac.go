package repository

// 本文件实现 RBAC 判定：管理员 → 角色（多对多）→ 权限点（多对多），
// 权限判定取“角色并集”。判定结果按 (RBAC 版本, admin_id) 缓存，
// 任何影响授权关系的写操作都会递增版本 key（op:rbac:ver），
// 从而整体失效，避免对 Redis 做 KEYS/SCAN 扫描。

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/operation/model"
)

// 通配符约定（写入 op_permission.resource / op_permission.action）：
//
//	"*"          匹配全部
//	"video:*"    匹配以 "video:" 开头的资源（域内通配）
//	"*:read"     匹配以 ":read" 结尾的动作（跨域只读）
//
// 通配只用于减少权限点膨胀，判定始终是“存在一条覆盖即放行”。
const (
	wildcardAll    = "*"
	wildcardSuffix = ":*"
	wildcardPrefix = "*:"
)

// rbacSnapshot 是按角色分组的授权快照，也是 Redis 缓存载体。
// 只包含角色名与权限点，绝不含口令、token 等敏感字段。
type rbacSnapshot struct {
	AdminID     int64              `json:"admin_id"`
	Grants      []*model.RoleGrant `json:"grants"`
	CachedAt    int64              `json:"cached_at"`
	State       int32              `json:"state"`    // 冗余的账号状态，命中缓存也能拦截已禁用账号
	Username    string             `json:"username"` // 冗余，便于审计与日志
	LockedUntil int64              `json:"locked_until"`
}

// matchToken 判断授权侧模式 granted 是否覆盖请求侧 actual。
func matchToken(granted, actual string) bool {
	granted = strings.TrimSpace(granted)
	if granted == "" {
		return false
	}
	if granted == wildcardAll || granted == actual {
		return true
	}
	if strings.HasSuffix(granted, wildcardSuffix) {
		return strings.HasPrefix(actual, strings.TrimSuffix(granted, wildcardAll))
	}
	if strings.HasPrefix(granted, wildcardPrefix) {
		return strings.HasSuffix(actual, strings.TrimPrefix(granted, wildcardAll))
	}
	return false
}

// permissionMatches 判断单个权限点是否覆盖 (resource, action)。
func permissionMatches(p *model.Permission, resource, action string) bool {
	if p == nil {
		return false
	}
	return matchToken(p.Resource, resource) && matchToken(p.Action, action)
}

// allows 返回是否放行以及命中的角色名（按角色并集判定）。
func (s *rbacSnapshot) allows(resource, action string) (bool, []string) {
	if s == nil || resource == "" || action == "" {
		return false, nil
	}
	var matched []string
	for _, g := range s.Grants {
		if g == nil || g.Role == nil {
			continue
		}
		for _, p := range g.Permissions {
			if permissionMatches(p, resource, action) {
				matched = append(matched, g.Role.Name)
				break
			}
		}
	}
	return len(matched) > 0, matched
}

// unionPerms 计算角色并集后的权限点集合（按 permission_id 去重，保持角色顺序）。
func unionPerms(grants []*model.RoleGrant) []*model.Permission {
	seen := make(map[int64]struct{})
	out := make([]*model.Permission, 0, 8)
	for _, g := range grants {
		if g == nil {
			continue
		}
		for _, p := range g.Permissions {
			if p == nil {
				continue
			}
			if _, ok := seen[p.PermissionID]; ok {
				continue
			}
			seen[p.PermissionID] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// roleNames 返回快照中的角色名列表（登录响应与判定回传复用）。
func (s *rbacSnapshot) roleNames() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.Grants))
	for _, g := range s.Grants {
		if g != nil && g.Role != nil {
			names = append(names, g.Role.Name)
		}
	}
	return names
}

// resolveRBAC 读取管理员授权快照：优先缓存，miss 时回源并回填。
// 返回 (快照, 是否命中缓存, error)。缓存不可用时降级直连 DB，不影响判定正确性。
func (r *Repository) resolveRBAC(ctx context.Context, adminID int64) (*rbacSnapshot, bool, error) {
	version := r.cache.version(ctx, keyRBACVersion)
	key := fmt.Sprintf(keyRBACSnapshot, version, adminID)

	var cached rbacSnapshot
	if r.cache.getJSON(ctx, key, &cached) {
		return &cached, true, nil
	}

	user, err := r.adminMd.FindOne(ctx, adminID)
	if err != nil {
		return nil, false, err
	}
	if user == nil {
		return nil, false, model.ErrAdminNotFound
	}
	grants, err := r.roleMd.LoadAdminGrants(ctx, adminID)
	if err != nil {
		return nil, false, err
	}
	snapshot := &rbacSnapshot{
		AdminID:     adminID,
		Grants:      grants,
		CachedAt:    nowUnix(),
		State:       user.State,
		Username:    user.Username,
		LockedUntil: user.LockedUntil,
	}
	r.cache.setJSON(ctx, key, snapshot, r.cfg.PermissionTTL)
	return snapshot, false, nil
}

// invalidateRBAC 递增 RBAC 版本，使全部授权快照立即失效。
// 由 AssignRoles / CreateRole / DeleteRole / CreatePermission / 角色授权变更 /
// 账号禁用等写操作调用（网关侧缓存另有 ttl 秒的兜底窗口，见 README）。
func (r *Repository) invalidateRBAC(ctx context.Context) {
	r.cache.bumpVersion(ctx, keyRBACVersion)
}

// PermissionDecision 是一次权限判定的完整结果，供 logic 直接映射为 RPC 响应。
type PermissionDecision struct {
	// AdminID 判定所用的管理员 ID。
	AdminID int64
	// Username 管理员账号名（审计与日志用）。
	Username string
	// Allowed 是否放行。
	Allowed bool
	// MatchedRoles 命中的角色名（并集判定结果）。
	MatchedRoles []string
	// CacheHit 是否命中权限快照缓存（排障用）。
	CacheHit bool
	// TTL 建议调用方（gateway/admin）缓存本次判定的秒数。
	TTL int32
	// Reason 拒绝原因（Allowed=true 时为空）：区分“权限不足”和“账号非正常态”，
	// 便于后台给出可执行提示（补权限 vs 恢复账号）。
	Reason string
}

// 拒绝原因（对外稳定字符串，与 logic 层常量一致）。
const (
	DenyReasonPermissionDenied = "permission_denied"
	DenyReasonAdminNotNormal   = "admin_not_normal"
)

// VerifyPermission 校验管理员是否具备 resource+action。
// 会话有效性由调用方（AdminID 来自 VerifyAdminPermission 的 token 解析）保证；
// 这里额外拦截已禁用账号，避免旧 token 在被禁用后仍能通过网关缓存放行。
func (r *Repository) VerifyPermission(ctx context.Context, adminID int64, resource, action string) (*PermissionDecision, error) {
	if adminID <= 0 {
		return nil, model.ErrInvalidOperator
	}
	if strings.TrimSpace(resource) == "" || strings.TrimSpace(action) == "" {
		return nil, model.ErrPermissionInvalid
	}
	snapshot, hit, err := r.resolveRBAC(ctx, adminID)
	if err != nil {
		return nil, err
	}
	allowed, matched := snapshot.allows(resource, action)
	reason := ""
	switch {
	case !allowed:
		reason = DenyReasonPermissionDenied
	case snapshot.State != model.AdminStateNormal:
		// 账号被禁用或锁定时无条件拒绝，即使角色仍覆盖该权限。
		allowed, matched, reason = false, nil, DenyReasonAdminNotNormal
	}
	return &PermissionDecision{
		AdminID:      adminID,
		Username:     snapshot.Username,
		Allowed:      allowed,
		MatchedRoles: matched,
		CacheHit:     hit,
		TTL:          int32(r.cfg.PermissionTTL),
		Reason:       reason,
	}, nil
}

// AdminPermissions 返回管理员的角色并集（菜单过滤与账号详情用）。
func (r *Repository) AdminPermissions(ctx context.Context, adminID int64) (*rbacSnapshot, error) {
	snapshot, _, err := r.resolveRBAC(ctx, adminID)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}
