package repository

// AdminLogin 的防爆破与会话签发用例（内存 model，不连数据库）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go-video/services/operation/model"
)

const testAdminPassword = "Correct-Horse-Battery-Staple"

// newLoginRepository 装配一个可登录的 Repository：账号存在、未启用二次校验。
func newLoginRepository(t *testing.T) (*Repository, *fakeAdminUserModel, *fakeSessionModel, *fakeAuditModel) {
	t.Helper()
	hash, algo, err := hashAdminPasswordWithIterations(testAdminPassword, minIterations)
	if err != nil {
		t.Fatalf("seed hash: %v", err)
	}
	adminMd := &fakeAdminUserModel{user: &model.AdminUser{
		AdminID:      42,
		Username:     "ops_lead",
		PasswordHash: hash,
		PwdAlgo:      algo,
		State:        model.AdminStateNormal,
	}}
	sessionMd := &fakeSessionModel{}
	auditMd := &fakeAuditModel{}
	r := newTestRepository()
	r.adminMd = adminMd
	r.sessionMd = sessionMd
	r.auditMd = auditMd
	r.roleMd = &fakeRoleModel{grants: []*model.RoleGrant{{
		Role: &model.Role{RoleID: 1, Name: "content_ops", State: model.StateEnable},
	}}}
	return r, adminMd, sessionMd, auditMd
}

func loginInput() LoginInput {
	return LoginInput{
		Username: "ops_lead",
		Password: testAdminPassword,
		Actor: Actor{
			IP:        "203.0.113.9",
			UserAgent: "qoder-admin-console/1.0",
			TraceID:   "trace-abc",
			RequestID: "req-001",
		},
	}
}

func TestAdminLoginIssuesSignedToken(t *testing.T) {
	ctx := context.Background()
	r, adminMd, sessionMd, auditMd := newLoginRepository(t)

	got, err := r.AdminLogin(ctx, loginInput())
	if err != nil {
		t.Fatalf("AdminLogin: %v", err)
	}
	if got.AdminID != 42 || got.Username != "ops_lead" {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if got.TTL != 7200 || got.ExpiresAt <= time.Now().Unix() || got.ExpiresAt > time.Now().Unix()+7200 {
		t.Fatalf("ttl/expires invalid: %+v", got)
	}
	if strings.Join(got.Roles, ",") != "content_ops" {
		t.Fatalf("roles = %v, want [content_ops]", got.Roles)
	}
	// token 形如 adm_<issuer>_<random>_<sig>：四段且摘要可本地校验。
	if _, ok := r.session.parseToken(got.Token); !ok {
		t.Fatalf("issued token %q fails its own signature check", got.Token)
	}
	if len(sessionMd.issued) != 1 {
		t.Fatalf("sessions inserted = %d, want 1", len(sessionMd.issued))
	}
	sess := sessionMd.issued[0]
	if sess.State != model.SessionStateActive || sess.AdminID != 42 {
		t.Fatalf("session invalid: %+v", sess)
	}
	if sess.IPHash == "" || strings.Contains(sess.IPHash, "203.0.113.9") {
		t.Fatalf("session must store a hashed IP, got %q", sess.IPHash)
	}
	if len(adminMd.touchTimes) != 1 {
		t.Fatalf("TouchLogin calls = %d, want 1", len(adminMd.touchTimes))
	}
	if adminMd.user.FailCount != 0 || adminMd.user.State != model.AdminStateNormal {
		t.Fatalf("successful login must clear the guard, got %+v", adminMd.user)
	}
	if len(auditMd.rows) != 1 || auditMd.rows[0].Result != model.AuditResultOK {
		t.Fatalf("audit rows = %+v, want one ok", auditMd.rows)
	}
	// 审计与会话行都不得携带口令原文。
	assertNoSecret(t, auditMd.rows[0], testAdminPassword)
}

func TestAdminLoginWrongPasswordCountsFailure(t *testing.T) {
	ctx := context.Background()
	r, adminMd, sessionMd, auditMd := newLoginRepository(t)

	in := loginInput()
	in.Password = "wrong-password-totally"
	if _, err := r.AdminLogin(ctx, in); !errors.Is(err, model.ErrAdminPasswordWrong) {
		t.Fatalf("err = %v, want ErrAdminPasswordWrong", err)
	}
	if len(adminMd.guards) != 1 {
		t.Fatalf("guard writes = %d, want 1", len(adminMd.guards))
	}
	g := adminMd.guards[0]
	if g.failCount != 1 || g.lockedUntil != 0 || g.state != model.AdminStateNormal {
		t.Fatalf("first failure guard = %+v, want (1, 0, normal)", g)
	}
	if len(sessionMd.issued) != 0 {
		t.Fatal("must not issue a session on wrong password")
	}
	if len(adminMd.touchTimes) != 0 {
		t.Fatal("must not touch login time on wrong password")
	}
	if len(auditMd.rows) != 1 || auditMd.rows[0].Result != model.AuditResultDenied {
		t.Fatalf("audit = %+v, want one denied row", auditMd.rows)
	}
	assertNoSecret(t, auditMd.rows[0], in.Password)
}

func TestAdminLoginLocksAtThreshold(t *testing.T) {
	ctx := context.Background()
	r, adminMd, sessionMd, _ := newLoginRepository(t)
	// 已失败 4 次（默认阈值 5），下一次失败即锁定。
	adminMd.user.FailCount = defaultMaxFail - 1

	in := loginInput()
	in.Password = "wrong-password-totally"
	if _, err := r.AdminLogin(ctx, in); !errors.Is(err, model.ErrAdminPasswordWrong) {
		t.Fatalf("err = %v, want ErrAdminPasswordWrong", err)
	}
	g := adminMd.guards[len(adminMd.guards)-1]
	if g.failCount != defaultMaxFail {
		t.Fatalf("fail_count = %d, want %d", g.failCount, defaultMaxFail)
	}
	if g.state != model.AdminStateLock || g.lockedUntil <= 0 {
		t.Fatalf("account must be locked with a deadline, got %+v", g)
	}
	if len(sessionMd.issued) != 0 {
		t.Fatal("locked attempt must not issue a session")
	}

	// 锁定期内即使口令正确也拒绝：锁定是运维介入信号，不能被“猜对了”绕过。
	if _, err := r.AdminLogin(ctx, loginInput()); !errors.Is(err, model.ErrAdminLocked) {
		t.Fatalf("err during lock = %v, want ErrAdminLocked", err)
	}
}

func TestAdminLoginRejectsDisabledAccount(t *testing.T) {
	ctx := context.Background()
	r, adminMd, sessionMd, _ := newLoginRepository(t)
	adminMd.user.State = model.AdminStateDisable

	if _, err := r.AdminLogin(ctx, loginInput()); !errors.Is(err, model.ErrAdminDisabled) {
		t.Fatalf("err = %v, want ErrAdminDisabled", err)
	}
	if len(adminMd.guards) != 0 {
		t.Fatalf("disabled account must not be counted as brute force: %+v", adminMd.guards)
	}
	if len(sessionMd.issued) != 0 {
		t.Fatal("disabled account must not get a session")
	}
}

func TestAdminLoginUnknownUsernameSameError(t *testing.T) {
	ctx := context.Background()
	r, adminMd, sessionMd, auditMd := newLoginRepository(t)
	adminMd.user = nil // 库里没有这个账号

	in := loginInput()
	in.Username = "nobody_ops"
	in.Password = "whatever-password"
	if _, err := r.AdminLogin(ctx, in); !errors.Is(err, model.ErrAdminPasswordWrong) {
		t.Fatalf("err = %v, want ErrAdminPasswordWrong (no user enumeration)", err)
	}
	if len(sessionMd.issued) != 0 || len(adminMd.guards) != 0 {
		t.Fatal("unknown account must not create sessions or guard rows")
	}
	// 仍然留一条 denied 审计，便于发现枚举尝试。
	if len(auditMd.rows) != 1 || auditMd.rows[0].Result != model.AuditResultDenied {
		t.Fatalf("audit = %+v, want one denied row", auditMd.rows)
	}
}

func TestAdminLoginRequiresConfiguredTokenSecret(t *testing.T) {
	ctx := context.Background()
	r, adminMd, sessionMd, _ := newLoginRepository(t)
	// 密钥只能来自配置指向的环境变量；缺失时拒绝签发，而不是发无摘要弱 token。
	r.session.Secret = nil

	if _, err := r.AdminLogin(ctx, loginInput()); !errors.Is(err, model.ErrTokenSecretMissing) {
		t.Fatalf("err = %v, want ErrTokenSecretMissing", err)
	}
	if len(sessionMd.issued) != 0 {
		t.Fatal("no session may be issued without the signing secret")
	}
	if len(adminMd.touchTimes) != 0 {
		t.Fatal("must not mark the account as logged-in when signing is unavailable")
	}
}

func TestAdminLoginSecondFactorFailsClosed(t *testing.T) {
	ctx := context.Background()
	r, adminMd, sessionMd, _ := newLoginRepository(t)
	adminMd.user.TwoFactorTarget = "13800000000"
	r.downstream = nil // 未配置 account RPC

	// 未带校验码 → 明确要求补码。
	if _, err := r.AdminLogin(ctx, loginInput()); !errors.Is(err, model.ErrSecondFactorRequired) {
		t.Fatalf("err = %v, want ErrSecondFactorRequired", err)
	}
	// 带了校验码但通道不可用 → fail-closed，绝不跳过第二因子。
	in := loginInput()
	in.SecondFactor = "123456"
	if _, err := r.AdminLogin(ctx, in); !errors.Is(err, model.ErrDownstreamUnavailable) {
		t.Fatalf("err = %v, want ErrDownstreamUnavailable", err)
	}
	if len(sessionMd.issued) != 0 {
		t.Fatal("second factor unavailable must not issue a session")
	}
}

func TestAdminLoginEmptyPasswordRejected(t *testing.T) {
	ctx := context.Background()
	r, _, sessionMd, _ := newLoginRepository(t)
	in := loginInput()
	in.Password = ""
	if _, err := r.AdminLogin(ctx, in); !errors.Is(err, model.ErrAdminPasswordEmpty) {
		t.Fatalf("err = %v, want ErrAdminPasswordEmpty", err)
	}
	if len(sessionMd.issued) != 0 {
		t.Fatal("empty password must not issue a session")
	}
}

func TestCreateAdminUserRejectsWeakPasswordBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	r, adminMd, _, _ := newLoginRepository(t)
	actor := Actor{AdminID: 42, Username: "ops_lead"}

	if _, err := r.CreateAdminUser(ctx, actor, CreateAdminUserInput{
		Username: "new_ops",
		Password: "short1",
	}); !errors.Is(err, model.ErrAdminPasswordWeak) {
		t.Fatalf("err = %v, want ErrAdminPasswordWeak", err)
	}
	if len(adminMd.inserted) != 0 {
		t.Fatalf("weak password must not reach the model: %+v", adminMd.inserted)
	}
}

// assertNoSecret 断言序列化后的记录不含明文口令/密钥（脱敏的最后防线）。
func assertNoSecret(t *testing.T, row any, secret string) {
	t.Helper()
	bs, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(bs), secret) {
		t.Fatalf("record leaks the plaintext credential: %s", bs)
	}
}
