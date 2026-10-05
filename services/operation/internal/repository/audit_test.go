package repository

// 审计索引脱敏用例：落库行不得含明文 IP、口令或 token（AGENTS.md §7）。

import (
	"encoding/json"
	"strings"
	"testing"

	"go-video/services/operation/model"
)

func TestNewAuditIndexRedactsActor(t *testing.T) {
	const (
		plainIP   = "203.0.113.24:54321"
		bearerTok = "adm_op_deadbeefdeadbeefdeadbeefdeadbeef"
		pwd       = "S3cret-Admin-Pwd-Plain"
	)
	actor := Actor{
		AdminID:  7,
		Username: "ops_lead",
		IP:       plainIP,
		// UA 里出现口令/token 是真实场景（前端把表单塞进 UA 头部调试），必须被截断而非结构化落库。
		UserAgent: "Mozilla/5.0 (dev) token=" + bearerTok + " pwd=" + pwd + " " + strings.Repeat("u", 400),
		TraceID:   strings.Repeat("t", 100),
		RequestID: strings.Repeat("r", 100),
	}

	row := newAuditIndex(actor, actionAdminLogin, "admin_user", "7", model.AuditResultDenied)

	if row.IPHash == plainIP || row.IPHash == "" {
		t.Fatalf("ip_hash must be a non-empty irreversible digest, got %q", row.IPHash)
	}
	if len(row.IPHash) != 32 {
		t.Fatalf("ip_hash length = %d, want 32 hex chars", len(row.IPHash))
	}
	if n := len([]rune(row.UserAgent)); n > maxUserAgentLen {
		t.Fatalf("user_agent runes = %d, want <= %d", n, maxUserAgentLen)
	}
	if n := len([]rune(row.TraceID)); n != 64 {
		t.Fatalf("trace_id runes = %d, want truncated to 64", n)
	}
	if n := len([]rune(row.RequestID)); n != 64 {
		t.Fatalf("request_id runes = %d, want truncated to 64", n)
	}
	// 结构化字段里绝不出现凭证原文（口令、token、明文 IP）。
	// 凭证原文必须整段丢弃（见 redactUserAgent），审计表不能变成凭证的第二份副本。
	if strings.Contains(row.UserAgent, bearerTok) || strings.Contains(row.UserAgent, pwd) {
		t.Fatalf("UA leaked a credential: %q", row.UserAgent)
	}
	if !strings.Contains(row.UserAgent, uaRedactedMark) {
		t.Fatalf("UA must keep a visible redaction marker for triage, got %q", row.UserAgent)
	}
	assertNoSecretInJSON(t, row, plainIP, pwd)
	if row.AdminID != 7 || row.Action != actionAdminLogin || row.Result != model.AuditResultDenied {
		t.Fatalf("row identity lost: %+v", row)
	}
	if row.Ctime == 0 {
		t.Fatal("ctime must be filled")
	}
}

func TestIPHashIsDeterministicButNotPlain(t *testing.T) {
	a := ipHash("203.0.113.9")
	b := ipHash("203.0.113.9")
	if a != b {
		t.Fatal("same IP must hash the same, otherwise「同源异常登录」查不出来")
	}
	if a == ipHash("203.0.113.10") {
		t.Fatal("different IPs must not collide")
	}
	if strings.Contains(a, "203") || len(a) != 32 {
		t.Fatalf("hash leaks the IP or has the wrong width: %q", a)
	}
	if ipHash("  ") != "" || ipHash("") != "" {
		t.Fatal("blank IP must hash to the empty string, not a digest of whitespace")
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	got := truncate("删除受版权保护整片的运营批量操作", 5)
	if got != "删除受版权" {
		t.Fatalf("truncate = %q, want 5 runes", got)
	}
	if n := len([]rune(truncate(strings.Repeat("汉", 300), maxUserAgentLen))); n != maxUserAgentLen {
		t.Fatalf("truncated rune count = %d, want %d", n, maxUserAgentLen)
	}
	if truncate("abc", 0) != "" {
		t.Fatal("non-positive max must yield empty string instead of an oversized value")
	}
	if truncate("abc", 10) != "abc" {
		t.Fatal("short input must be returned unchanged")
	}
}

func TestRedactUserAgent(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		wantS string // 期望结果（不含脱敏标记时按原样比较前缀）
		leak  string // 绝不许出现的原文
	}{
		{"plain ua keeps content", "Mozilla/5.0 (X11; Linux x86_64)", "Mozilla/5.0 (X11; Linux x86_64)", ""},
		{"token=", "admin-console/1.0 token=adm_op_abcdef", "admin-console/1.0", "adm_op_abcdef"},
		{"bearer", "curl/8.0 Authorization: Bearer abc123", "curl/8.0", "abc123"},
		{"pwd:", "tool/1 pwd:hunter2", "tool/1", "hunter2"},
		{"case-insensitive", "UA TOKEN=x", "UA", "TOKEN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactUserAgent(c.in)
			if !strings.HasPrefix(got, c.wantS) {
				t.Fatalf("redactUserAgent(%q) = %q, want prefix %q", c.in, got, c.wantS)
			}
			if c.leak != "" && strings.Contains(got, c.leak) {
				t.Fatalf("redacted UA still leaks %q: %q", c.leak, got)
			}
			if n := len([]rune(got)); n > maxUserAgentLen {
				t.Fatalf("length = %d, want <= %d", n, maxUserAgentLen)
			}
		})
	}
	// 普通 UA 不应被误伤（否则排障时看不到客户端版本）。
	plain := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/126.0"
	if redactUserAgent(plain) != plain {
		t.Fatalf("plain UA altered: %q", redactUserAgent(plain))
	}
}

func TestAuditActionNamingConvention(t *testing.T) {
	// action 必须是 <对象>.<动作>：列表按 action 精确过滤，命名漂移会让历史索引查不出来。
	actions := []string{
		actionAdminLogin, actionAdminLogout, actionAdminUserCreate, actionAdminUserUpdate,
		actionAdminUserDisable, actionAdminUserResetPwd, actionRoleCreate, actionRoleDelete,
		actionRoleGrant, actionAdminAssignRoles, actionPermissionCreate, actionMenuSave,
		actionConfigSave, actionTaskSubmit, actionTaskCancel, actionTaskRun,
	}
	seen := map[string]bool{}
	for _, a := range actions {
		if seen[a] {
			t.Fatalf("duplicate audit action %q", a)
		}
		seen[a] = true
		parts := strings.Split(a, ".")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			t.Fatalf("audit action %q must be formatted as <object>.<action>", a)
		}
		if strings.ToLower(a) != a {
			t.Fatalf("audit action %q must be lowercase", a)
		}
	}
}

// assertNoSecretInJSON 检查序列化后的记录不含任何给定原文凭证。
func assertNoSecretInJSON(t *testing.T, row any, secrets ...string) {
	t.Helper()
	bs, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(string(bs), s) {
			t.Fatalf("audit record leaks %q: %s", s, bs)
		}
	}
}
