package repository

// 本文件覆盖登录域的纯函数：密码哈希、mid 生成、凭证类型推断、验证码与
// token 生成，规则与参考仓库 passport/passport-auth 保持一致。

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// rsaGenKey 生成测试用 RSA 私钥。
func rsaGenKey() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
}

// rsaPEMs 导出测试用 PEM 公钥/私钥。
func rsaPEMs(t *testing.T, priv *rsa.PrivateKey) (string, string) {
	t.Helper()
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	prv := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}))
	return pub, prv
}

// TestSaltPwd 覆盖密码哈希算法（与参考仓库 getSaltPwd 一致）。
func TestSaltPwd(t *testing.T) {
	salt := "aabbccdd"
	hash1 := saltPwd("pwd123", salt)
	hash2 := saltPwd("pwd123", salt)
	if hash1 != hash2 {
		t.Fatal("same pwd+salt must produce same hash")
	}
	if len(hash1) != 32 {
		t.Errorf("hash length = %d, want 32", len(hash1))
	}
	if saltPwd("pwd123", "other") == hash1 {
		t.Error("different salt must produce different hash")
	}
	if saltPwd("pwd124", salt) == hash1 {
		t.Error("different pwd must produce different hash")
	}
}

// TestGenMid 覆盖 ULID 风格 mid 生成：正值、唯一、时间有序。
func TestGenMid(t *testing.T) {
	seen := map[int64]bool{}
	var last int64
	for i := 0; i < 1000; i++ {
		mid, err := genMid()
		if err != nil {
			t.Fatal(err)
		}
		if mid <= 0 {
			t.Fatalf("mid = %d, want positive", mid)
		}
		if seen[mid] {
			t.Fatalf("duplicate mid %d", mid)
		}
		seen[mid] = true
		if mid <= last {
			t.Fatalf("mid %d not time-ordered after %d", mid, last)
		}
		last = mid
	}
}

// TestCredentialTypeOf 覆盖注册标识形态推断。
func TestCredentialTypeOf(t *testing.T) {
	cases := map[string]int8{
		"13800138000":  2, // 手机（CredentialTypePhone）
		"a@b.com":      3, // 邮箱（CredentialTypeEmail）
		"some_user_01": 1, // 用户名（CredentialTypeUsername）
	}
	for in, want := range cases {
		if got := credentialTypeOf(in); got != want {
			t.Errorf("credentialTypeOf(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestRandomHex 覆盖随机 hex 生成长度与随机性。
func TestRandomHex(t *testing.T) {
	a := randomHex(32)
	b := randomHex(32)
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("randomHex len = %d/%d, want 64", len(a), len(b))
	}
	if a == b {
		t.Fatal("randomHex should produce distinct values")
	}
}

// TestDecryptPasswordPlaintextMode 覆盖未配置 RSA 密钥时按明文处理（开发模式）。
func TestDecryptPasswordPlaintextMode(t *testing.T) {
	r := &Repository{}
	got, err := r.decryptPassword("plain-password")
	if err != nil || got != "plain-password" {
		t.Errorf("decryptPassword plain = %q err=%v, want passthrough", got, err)
	}
}

// TestPassportRSARoundtrip 覆盖登录密码 RSA 加解密往返。
func TestPassportRSARoundtrip(t *testing.T) {
	priv, err := rsaGenKey()
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, privPEM := rsaPEMs(t, priv)
	c := NewPassportRSA(pubPEM, privPEM)
	plain := "my-secret-pwd"
	enc, err := c.CardEncrypt([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if string(enc) == plain {
		t.Fatal("ciphertext equals plaintext")
	}
	dec, err := c.CardDecrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if string(dec) != plain {
		t.Fatalf("decrypted %q, want %q", dec, plain)
	}

	r := &Repository{passportRSA: c}
	got, err := r.decryptPassword(string(enc))
	if err != nil || got != plain {
		t.Errorf("decryptPassword = %q err=%v, want %q", got, err, plain)
	}
}

// TestCookieInfoParse 覆盖 SESSDATA cookie 解析。
func TestCookieInfoParse(t *testing.T) {
	cookie := "buvid3=xxx; SESSDATA=deadbeef; sid=abcd"
	if got := parseSESSDATA(cookie); got != "deadbeef" {
		t.Errorf("parseSESSDATA = %q, want deadbeef", got)
	}
	if got := parseSESSDATA("no-cookie-here"); got != "" {
		t.Errorf("parseSESSDATA = %q, want empty", got)
	}
}

// parseSESSDATA 从 cookie 原文提取 SESSDATA 值（与 CookieInfo 内联逻辑一致）。
func parseSESSDATA(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && kv[0] == "SESSDATA" {
			return kv[1]
		}
	}
	return ""
}
