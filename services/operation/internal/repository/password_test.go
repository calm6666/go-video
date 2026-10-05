package repository

// 口令散列与防爆破计数的纯逻辑单测（不连 MySQL/Redis）。
//
// 关键原则：PBKDF2 的期望值来自独立实现（OpenSSL 的 hashlib.pbkdf2_hmac('sha256', ...)），
// 不是拿本包代码算一遍再和自己比——那样实现错了也测不出来（AGENTS.md §9）。

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go-video/services/operation/model"
)

func TestPBKDF2SHA256FixedVectors(t *testing.T) {
	cases := []struct {
		name  string
		pwd   string
		salt  string
		iter  int
		dkLen int
		want  string
	}{
		{"c=1", "password", "salt", 1, sha256SizeHex, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"c=2", "password", "salt", 2, sha256SizeHex, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"c=4096", "passwd", "salt", 4096, sha256SizeHex, "21943fd5b7a10905c38fad60157ff498e1e81df1e03254325682a74dca3b2be8"},
		// 40 字节派生密钥需要两个 HMAC 块，覆盖 numBlocks 拼接分支
		{"multi-block", "passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pbkdf2SHA256([]byte(c.pwd), []byte(c.salt), c.iter, c.dkLen)
			if len(got) != c.dkLen {
				t.Fatalf("derived key length = %d, want %d", len(got), c.dkLen)
			}
			if hex.EncodeToString(got) != c.want {
				t.Fatalf("dk = %s, want %s", hex.EncodeToString(got), c.want)
			}
		})
	}
}

// sha256SizeHex 是 SHA-256 输出按 hex 表示时的字节数（dkLen=32 时 hex 长度 64）。
const sha256SizeHex = 32

func TestHashAdminPasswordFormat(t *testing.T) {
	const pwd = "S3cret-Admin-Pwd"
	hash, algo, err := hashAdminPasswordWithIterations(pwd, minIterations)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if algo != PwdAlgo {
		t.Fatalf("algo = %q, want %q", algo, PwdAlgo)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 4 {
		t.Fatalf("hash %q must be algo$iterations$salt$dk", hash)
	}
	if parts[0] != PwdAlgo || parts[1] != "1000" {
		t.Fatalf("unexpected head segments: %v", parts[:2])
	}
	if strings.Contains(hash, pwd) {
		t.Fatal("hash must not contain the plaintext password")
	}
	// 同一口令两次散列必须不同（随机盐），否则彩虹表可用。
	other, _, err := hashAdminPasswordWithIterations(pwd, minIterations)
	if err != nil {
		t.Fatalf("hash second time: %v", err)
	}
	if other == hash {
		t.Fatal("salt must be random per hash")
	}
}

func TestVerifyAdminPassword(t *testing.T) {
	const pwd = "Correct-Horse-Battery"
	hash, _, err := hashAdminPasswordWithIterations(pwd, minIterations)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyAdminPassword(pwd, hash)
	if err != nil || !ok {
		t.Fatalf("verify right password: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyAdminPassword(pwd+"x", hash)
	if err != nil || ok {
		t.Fatalf("verify wrong password: ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestVerifyAdminPasswordRejectsMalformedHash(t *testing.T) {
	// 数据损坏必须报错，而不是被调用当成“口令错”（否则账号会静默登不上）。
	cases := map[string]string{
		"too few segments": "pbkdf2_sha256$1000$aabb",
		"bad iterations":   "pbkdf2_sha256$0$aabb$ccdd",
		"weak iterations":  "pbkdf2_sha256$10$aabb$ccdd",
		"bad salt hex":     "pbkdf2_sha256$1000$zzzz$ccdd",
		"empty dk":         "pbkdf2_sha256$1000$aabb$",
		"unsupported algo": "bcrypt$10$aabb$ccdd",
	}
	for name, stored := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyAdminPassword("whatever", stored)
			if err == nil {
				t.Fatalf("want error for %q, got ok=%v", stored, ok)
			}
			if ok {
				t.Fatalf("want ok=false for %q", stored)
			}
		})
	}
}

func TestPasswordStrengthOK(t *testing.T) {
	cases := []struct {
		pwd  string
		want bool
	}{
		{strings.Repeat("a", MinPasswordLength-1), false}, // 9 位太短
		{strings.Repeat("a", MinPasswordLength), true},
		{strings.Repeat("a", MaxPasswordLength), true},
		{strings.Repeat("a", MaxPasswordLength+1), false}, // 上限保护 PBKDF2 输入规模
		{" " + strings.Repeat("a", 12), false},            // 首尾空白多半是复制粘贴产物
		{strings.Repeat("a", 12) + " ", false},
		{"", false},
		// 中文按 rune 计数：10 个汉字合法
		{"口令口令口令口令口令口令", true},
	}
	for _, c := range cases {
		if got := passwordStrengthOK(c.pwd); got != c.want {
			t.Fatalf("passwordStrengthOK(%q) = %v, want %v", c.pwd, got, c.want)
		}
	}
}

func TestHashAdminPasswordRejectsWeakIterations(t *testing.T) {
	if _, _, err := hashAdminPasswordWithIterations("abcdefghij", minIterations-1); err == nil {
		t.Fatal("iterations below the minimum must be refused")
	}
	// 对外入口使用默认迭代数，散列里必须能读出来（便于后续升级算法）。
	hash, _, err := HashAdminPassword("Correct-Horse-Battery!")
	if err != nil {
		t.Fatalf("HashAdminPassword: %v", err)
	}
	if !strings.HasPrefix(hash, PwdAlgo+"$") {
		t.Fatalf("hash %q must start with the algo identifier", hash)
	}
	parts := strings.Split(hash, "$")
	if parts[1] != "210000" {
		t.Fatalf("iterations = %s, want the OWASP-level default 210000", parts[1])
	}
}

func TestCheckAdminUsername(t *testing.T) {
	// 账号名统一小写归一化后校验：3-32 位、小写字母开头。
	if _, err := checkAdminUsername("ab"); err == nil {
		t.Fatal("username shorter than 3 chars must be refused")
	}
	if _, err := checkAdminUsername("Ops_Lead"); err != nil {
		t.Fatalf("valid username refused: %v", err)
	}
	name, err := checkAdminUsername("  Ops_Lead.2 ")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if name != "ops_lead.2" {
		t.Fatalf("normalized username = %q, want lowercase/trimmed", name)
	}
}

func TestNormalizeSecondFactorTarget(t *testing.T) {
	// 空串=不修改，"-"=关闭，其余按长度校验。
	if v, changed, err := normalizeSecondFactorTarget(""); err != nil || changed || v != "" {
		t.Fatalf("empty: got %q/%v/%v", v, changed, err)
	}
	if v, changed, err := normalizeSecondFactorTarget(secondFactorDisableToken); err != nil || !changed || v != "" {
		t.Fatalf("disable token: got %q/%v/%v", v, changed, err)
	}
	if _, _, err := normalizeSecondFactorTarget(strings.Repeat("1", secondFactorTargetMaxLen+1)); err == nil {
		t.Fatal("over-long target must be refused")
	}
	if _, _, err := normalizeSecondFactorTarget(" 13800000000 "); err != nil {
		t.Fatalf("valid target refused: %v", err)
	}
}

func TestEvalLoginFailureFixedVectors(t *testing.T) {
	const now = int64(1_700_000_000)
	cases := []struct {
		name      string
		failCount int32
		cfg       LoginConf
		wantFail  int32
		wantLock  int64
		wantState int32
	}{
		{"first failure", 0, LoginConf{MaxFail: 5, LockMinutes: 15}, 1, 0, model.AdminStateNormal},
		{"one below threshold", 3, LoginConf{MaxFail: 5, LockMinutes: 15}, 4, 0, model.AdminStateNormal},
		{"reaches threshold", 4, LoginConf{MaxFail: 5, LockMinutes: 15}, 5, now + 15*60, model.AdminStateLock},
		{"already beyond threshold", 9, LoginConf{MaxFail: 5, LockMinutes: 15}, 10, now + 15*60, model.AdminStateLock},
		{"custom threshold", 1, LoginConf{MaxFail: 2, LockMinutes: 1}, 2, now + 60, model.AdminStateLock},
		// 未配置时用缺省值（5 次 / 15 分钟），不能退化成“永不锁定”。
		{"defaults apply when unset", 4, LoginConf{}, 5, now + defaultLockMinutes*60, model.AdminStateLock},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := &model.AdminUser{FailCount: c.failCount}
			fail, lockedUntil, state := evalLoginFailure(u, now, c.cfg)
			if fail != c.wantFail || lockedUntil != c.wantLock || state != c.wantState {
				t.Fatalf("evalLoginFailure = (%d,%d,%d), want (%d,%d,%d)",
					fail, lockedUntil, state, c.wantFail, c.wantLock, c.wantState)
			}
		})
	}
}

func TestLoginBlocked(t *testing.T) {
	const now = int64(1_700_000_000)
	cases := []struct {
		name string
		u    *model.AdminUser
		want bool
	}{
		{"nil", nil, false},
		{"normal", &model.AdminUser{State: model.AdminStateNormal}, false},
		{"disabled forever", &model.AdminUser{State: model.AdminStateDisable}, true},
		{"lock still active", &model.AdminUser{State: model.AdminStateLock, LockedUntil: now + 60}, true},
		// 锁定期已过即可重试：不能要求人工解锁，否则爆破者反而把账号当成 DoS 靶子。
		{"lock expired", &model.AdminUser{State: model.AdminStateLock, LockedUntil: now}, false},
		{"lock expired in past", &model.AdminUser{State: model.AdminStateLock, LockedUntil: now - 1}, false},
	}
	for _, c := range cases {
		if got := loginBlocked(c.u, now); got != c.want {
			t.Fatalf("%s: loginBlocked = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNormalizeOptionsFillsDefaults(t *testing.T) {
	got := normalizeOptions(Options{})
	if got.PermissionTTL != defaultPermissionTTL || got.ConfigTTL != defaultConfigTTL || got.MenuTTL != defaultMenuTTL {
		t.Fatalf("TTL defaults wrong: %+v", got)
	}
	if got.RunSteps != defaultRunSteps || got.MaxFail != defaultMaxFail || got.LockMinutes != defaultLockMinutes {
		t.Fatalf("batch/login defaults wrong: %+v", got)
	}
	// 推进批量超过任务步骤上限时收敛到上限，避免一次 RPC 打爆下游。
	if big := normalizeOptions(Options{RunSteps: maxTaskSteps + 10}); big.RunSteps != maxTaskSteps {
		t.Fatalf("RunSteps = %d, want capped at %d", big.RunSteps, maxTaskSteps)
	}
}

func TestPagePair(t *testing.T) {
	cases := []struct{ pn, ps, wantPN, wantPS int32 }{
		{0, 0, 1, defaultPageSize},
		{-3, -1, 1, defaultPageSize},
		{2, 50, 2, 50},
		{1, maxPageSize + 1, 1, maxPageSize},
	}
	for _, c := range cases {
		pn, ps := pagePair(c.pn, c.ps)
		if pn != c.wantPN || ps != c.wantPS {
			t.Fatalf("pagePair(%d,%d) = (%d,%d), want (%d,%d)", c.pn, c.ps, pn, ps, c.wantPN, c.wantPS)
		}
	}
}

func TestRequireOperator(t *testing.T) {
	r := newTestRepository()
	if err := r.requireOperator(Actor{}); !errors.Is(err, model.ErrInvalidOperator) {
		t.Fatalf("missing operator: err = %v, want ErrInvalidOperator", err)
	}
	if err := r.requireOperator(Actor{AdminID: 1}); err != nil {
		t.Fatalf("valid operator: %v", err)
	}
}
