package repository

// RBAC 判定的纯逻辑用例：角色并集、通配匹配、拒绝语义（不连数据库）。

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/services/operation/model"
)

func perm(id int64, resource, action string) *model.Permission {
	return &model.Permission{PermissionID: id, Resource: resource, Action: action}
}

func grant(roleName string, perms ...*model.Permission) *model.RoleGrant {
	return &model.RoleGrant{
		Role:        &model.Role{RoleID: int64(len(roleName)), Name: roleName, State: model.StateEnable},
		Permissions: perms,
	}
}

func TestMatchToken(t *testing.T) {
	cases := []struct {
		granted string
		actual  string
		want    bool
	}{
		{"*", "video:submission", true},
		{"video:submission", "video:submission", true},
		{"video:*", "video:submission", true},
		{"video:*", "catalog:episode", false},
		{"video:submission", "video:episode", false},
		{"*:read", "video:submission:read", true},
		{"*:read", "video:submission:write", false},
		{"", "video:submission", false},   // 空模式不放行任何请求
		{"  ", "video:submission", false}, // 只有空白也算空模式
		{"video:submission", "", false},
	}
	for _, c := range cases {
		if got := matchToken(c.granted, c.actual); got != c.want {
			t.Fatalf("matchToken(%q, %q) = %v, want %v", c.granted, c.actual, got, c.want)
		}
	}
}

func TestPermissionMatches(t *testing.T) {
	if permissionMatches(nil, "video:submission", "offline") {
		t.Fatal("nil permission must not match")
	}
	p := perm(1, "video:*", "offline")
	if !permissionMatches(p, "video:submission", "offline") {
		t.Fatal("resource wildcard + exact action must match")
	}
	if permissionMatches(p, "video:submission", "delete") {
		t.Fatal("action mismatch must not match")
	}
}

func TestAllowsRoleUnion(t *testing.T) {
	s := &rbacSnapshot{Grants: []*model.RoleGrant{
		grant("content_ops", perm(1, "video:submission", "offline")),
		grant("read_all", perm(2, "catalog:episode", "read"), perm(3, "*", "read")),
	}}

	// 单角色覆盖即放行，并回传命中角色名（后台要能解释“为什么可以”）。
	ok, roles := s.allows("video:submission", "offline")
	if !ok || !reflect.DeepEqual(roles, []string{"content_ops"}) {
		t.Fatalf("allows(submission.offline) = %v/%v", ok, roles)
	}
	// 并集：另一个角色的权限同样生效，且两个角色都覆盖时全部回传。
	ok, roles = s.allows("catalog:episode", "read")
	if !ok || !reflect.DeepEqual(roles, []string{"read_all"}) {
		t.Fatalf("allows(episode.read) = %v/%v", ok, roles)
	}
	// 跨域只读：resource="*" 覆盖任意资源（action 仍是精确值）。
	ok, _ = s.allows("rights:window", "read")
	if !ok {
		t.Fatal("resource wildcard must cover rights:window.read")
	}
	// 没有任何角色覆盖 → 拒绝，且不给角色线索。
	ok, roles = s.allows("admin_user", "create")
	if ok || roles != nil {
		t.Fatalf("uncovered request must be denied, got %v/%v", ok, roles)
	}
	// 空 resource/action 是非法请求，不是“查全部权限”。
	if ok, _ := s.allows("", "read"); ok {
		t.Fatal("empty resource must be denied")
	}
}

func TestAllowsIgnoresMalformedGrants(t *testing.T) {
	s := &rbacSnapshot{Grants: []*model.RoleGrant{
		nil,
		{Role: nil, Permissions: []*model.Permission{perm(9, "*", "*")}},
		grant("empty_role"),
		grant("ok", perm(10, "menu", "read")),
	}}
	ok, roles := s.allows("menu", "read")
	if !ok || !reflect.DeepEqual(roles, []string{"ok"}) {
		t.Fatalf("allows = %v/%v, want ok/[ok] despite malformed rows", ok, roles)
	}
	var nilSnapshot *rbacSnapshot
	if ok, _ := nilSnapshot.allows("menu", "read"); ok {
		t.Fatal("nil snapshot must deny (fail-closed)")
	}
}

func TestUnionPermsDedupsAcrossRoles(t *testing.T) {
	shared := perm(1, "video:submission", "offline")
	grants := []*model.RoleGrant{
		grant("a", shared, perm(2, "video:submission", "delete")),
		grant("b", shared, perm(3, "catalog:episode", "read")),
		nil,
	}
	got := unionPerms(grants)
	var ids []int64
	for _, p := range got {
		ids = append(ids, p.PermissionID)
	}
	if want := []int64{1, 2, 3}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("union ids = %v, want %v (dedup by permission_id, role order kept)", ids, want)
	}
}

func TestRoleNamesFromSnapshot(t *testing.T) {
	s := &rbacSnapshot{Grants: []*model.RoleGrant{grant("a"), {Role: nil}, nil, grant("b")}}
	if got := s.roleNames(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("roleNames = %v", got)
	}
	var nilSnapshot *rbacSnapshot
	if nilSnapshot.roleNames() != nil {
		t.Fatal("nil snapshot must return no roles")
	}
}

func TestVerifyPermissionRejectsNonNormalAccount(t *testing.T) {
	ctx := context.Background()
	r := newTestRepository()
	// 快照里权限覆盖，但账号已被禁用：仍然拒绝（旧 token 不能越过禁用动作）。
	r.adminMd = &fakeAdminUserModel{user: &model.AdminUser{
		AdminID: 7, Username: "fired_ops", State: model.AdminStateDisable,
	}}
	r.roleMd = &fakeRoleModel{grants: []*model.RoleGrant{grant("super", perm(1, "*", "*"))}}

	got, err := r.VerifyPermission(ctx, 7, "admin_user", "create")
	if err != nil {
		t.Fatalf("VerifyPermission: %v", err)
	}
	if got.Allowed || got.Reason != DenyReasonAdminNotNormal {
		t.Fatalf("decision = %+v, want denied with %s", got, DenyReasonAdminNotNormal)
	}
	if got.Username != "fired_ops" {
		t.Fatalf("username = %q", got.Username)
	}
	// TTL 回传给网关，作为其本地缓存秒数（写入侧已即时失效服务端快照）。
	if got.TTL != int32(defaultPermissionTTL) {
		t.Fatalf("ttl = %d, want %d", got.TTL, defaultPermissionTTL)
	}
}

func TestVerifyPermissionAllowsByUnion(t *testing.T) {
	ctx := context.Background()
	r := newTestRepository()
	r.adminMd = &fakeAdminUserModel{user: &model.AdminUser{AdminID: 7, Username: "ops_lead", State: model.AdminStateNormal}}
	r.roleMd = &fakeRoleModel{grants: []*model.RoleGrant{
		grant("content_ops", perm(1, "video:submission", "offline")),
	}}

	got, err := r.VerifyPermission(ctx, 7, "video:submission", "offline")
	if err != nil {
		t.Fatalf("VerifyPermission: %v", err)
	}
	if !got.Allowed || got.Reason != "" {
		t.Fatalf("decision = %+v, want allowed", got)
	}
	// 同一条判定被缓存命中（第二次不再回源，验证版本 key 生效路径可用）。
	if _, err := r.VerifyPermission(ctx, 7, "video:submission", "offline"); err != nil {
		t.Fatalf("second VerifyPermission: %v", err)
	}
}

func TestVerifyPermissionInputGuards(t *testing.T) {
	ctx := context.Background()
	r := newTestRepository()
	if _, err := r.VerifyPermission(ctx, 0, "a", "b"); !errors.Is(err, model.ErrInvalidOperator) {
		t.Fatalf("err = %v, want ErrInvalidOperator", err)
	}
	if _, err := r.VerifyPermission(ctx, 1, "  ", "read"); !errors.Is(err, model.ErrPermissionInvalid) {
		t.Fatalf("err = %v, want ErrPermissionInvalid", err)
	}
}
