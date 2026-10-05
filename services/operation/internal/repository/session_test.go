package repository

// 后台会话 token 的签发与本地校验用例（纯内存，不查库）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-video/services/operation/model"
)

func TestSessionConfRefusesToSignWithoutSecret(t *testing.T) {
	s := SessionConf{TokenTTL: 7200, Issuer: "op"} // 未注入密钥
	if _, err := s.newToken(); !errors.Is(err, model.ErrTokenSecretMissing) {
		t.Fatalf("newToken err = %v, want ErrTokenSecretMissing", err)
	}
	// 也不接受任何“看起来像”的 token：没有密钥就没有可校验的签发方。
	if _, ok := s.parseToken("adm_op_deadbeef"); ok {
		t.Fatal("unsigned token must be rejected when the secret is missing")
	}
	if _, ok := s.parseToken("adm_op_deadbeef_cafe"); ok {
		t.Fatal("any token must be rejected when the secret is missing")
	}
}

func TestTokenSignAndParseRoundTrip(t *testing.T) {
	s := SessionConf{TokenTTL: 7200, Issuer: "op", Secret: []byte("secret-A")}
	token, err := s.newToken()
	if err != nil {
		t.Fatalf("newToken: %v", err)
	}
	parts := strings.Split(token, "_")
	if len(parts) != 4 || parts[0] != "adm" || parts[1] != "op" {
		t.Fatalf("token %q must be adm_<issuer>_<random>_<sig>", token)
	}
	body, ok := s.parseToken(token)
	if !ok || body != parts[2] {
		t.Fatalf("parseToken = %q/%v, want %q/true", body, ok, parts[2])
	}

	// 摘要被改一个字符 → 拒绝（爆破/扫描流量在打库前就被挡下）。
	tampered := token[:len(token)-1]
	if last := token[len(token)-1]; last == 'a' {
		tampered += "b"
	} else {
		tampered += "a"
	}
	if _, ok := s.parseToken(tampered); ok {
		t.Fatal("tampered signature accepted")
	}

	// 换一把密钥 → 之前签发的 token 全部失效（密钥轮换的预期语义）。
	other := SessionConf{TokenTTL: 7200, Issuer: "op", Secret: []byte("secret-B")}
	if _, ok := other.parseToken(token); ok {
		t.Fatal("token signed by another secret accepted")
	}

	// 其它签发方（多环境隔离）不接受。
	otherIssuer := SessionConf{TokenTTL: 7200, Issuer: "stage", Secret: []byte("secret-A")}
	if _, ok := otherIssuer.parseToken(token); ok {
		t.Fatal("token from another issuer accepted")
	}

	for _, bad := range []string{"", "adm_op", "adm_op_short", "adm_op_a_b_c", "usr_op_" + parts[2] + "_" + parts[3]} {
		if _, ok := s.parseToken(bad); ok {
			t.Fatalf("malformed/foreign token accepted: %q", bad)
		}
	}
}

func TestTokenBodiesAreUnique(t *testing.T) {
	s := SessionConf{TokenTTL: 7200, Issuer: "op", Secret: []byte("secret-A")}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok, err := s.newToken()
		if err != nil {
			t.Fatalf("newToken: %v", err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token generated: %s", tok)
		}
		seen[tok] = true
	}
}

func TestIssuerNormalization(t *testing.T) {
	// issuer 参与 token 分段，绝不能含下划线（会破坏 4 段解析）。
	cases := map[string]string{"": "op", "  ": "op", "a_b": "a-b", " Prod ": "Prod"}
	for in, want := range cases {
		if got := (SessionConf{Issuer: in}).issuer(); got != want {
			t.Fatalf("issuer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionTTLFallback(t *testing.T) {
	if got := (SessionConf{}).ttl(); got != 7200 {
		t.Fatalf("ttl = %d, want the 7200s admin default", got)
	}
	if got := (SessionConf{TokenTTL: 60}).ttl(); got != 60 {
		t.Fatalf("ttl = %d, want 60", got)
	}
}

func TestSessionByTokenRejectsUnknownAndRevoked(t *testing.T) {
	ctx := context.Background()
	r := newTestRepository()
	r.sessionMd = &fakeSessionModel{}
	if _, err := r.sessionByToken(ctx, "adm_op_deadbeef_deadbeef"); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("err = %v, want ErrSessionInvalid", err)
	}
	// 明显非法的格式不应触达 model（未实现的方法被调用即 panic）。
	if _, err := r.sessionByToken(ctx, "garbage"); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("err = %v, want ErrSessionInvalid", err)
	}
}

func TestResolveAdminIDPrefersToken(t *testing.T) {
	ctx := context.Background()
	sess := &model.AdminSession{Token: "adm_op_" + strings.Repeat("a", 48) + "_" + strings.Repeat("b", 16), AdminID: 42, Expires: 9_999_999_999, State: model.SessionStateActive}
	r := newTestRepository()
	// 该 token 的摘要不属于本 Repository 的密钥，因此不会命中格式校验：
	// 必须走 ErrSessionInvalid，而不是退回 admin_id（否则伪造 token + 猜 admin_id 即可冒充）。
	if _, err := r.ResolveAdminID(ctx, sess.Token, 7); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("err = %v, want ErrSessionInvalid", err)
	}
	// 没有 token 时才接受显式 admin_id（网关已鉴权的内部调用）。
	got, err := r.ResolveAdminID(ctx, "", 7)
	if err != nil || got != 7 {
		t.Fatalf("ResolveAdminID = %d/%v, want 7/nil", got, err)
	}
	if _, err := r.ResolveAdminID(ctx, "  ", 0); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("err = %v, want ErrSessionInvalid", err)
	}
}

func TestCachedSessionNeverLeaksCredentials(t *testing.T) {
	// 会话缓存载体是 JSON：token 本身是凭证，只允许出现在会话表/会话缓存里，
	// 权限快照（rbacSnapshot）里绝不能出现 token 或口令。
	snapshot := rbacSnapshot{
		AdminID:  1,
		Username: "ops_lead",
		State:    model.AdminStateNormal,
		Grants:   []*model.RoleGrant{{Role: &model.Role{Name: "content_ops"}}},
	}
	bs, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"adm_op_", "password", "PasswordHash", "token"} {
		if strings.Contains(strings.ToLower(string(bs)), strings.ToLower(forbidden)) {
			t.Fatalf("rbac snapshot leaks %q: %s", forbidden, bs)
		}
	}
}
