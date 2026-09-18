package repository

// 本文件覆盖实名认证的身份证解析、成年判断、证件号哈希与 RSA 加解密。
// 规则与参考仓库 member 服务 service/realname.go、service/crypto 保持一致。

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

// TestParseIdentity 覆盖 15/18 位身份证的生日与性别解析（参考 ParseIdentity）。
func TestParseIdentity(t *testing.T) {
	cases := []struct {
		id       string
		birthday time.Time
		gender   string
	}{
		// 18 位：1990-01-02，序号第 17 位为 3（奇数→男）
		{"110101199001020031", time.Date(1990, 1, 2, 0, 0, 0, 0, time.Local), genderMale},
		// 18 位：2000-12-31，序号第 17 位为 4（偶数→女）
		{"110101200012310042", time.Date(2000, 12, 31, 0, 0, 0, 0, time.Local), genderFemale},
		// 15 位：1995-06-15，第 15 位为 7（奇数→男）
		{"110101950615071", time.Date(1995, 6, 15, 0, 0, 0, 0, time.Local), genderMale},
		// 15 位：1999-09-09，第 15 位为 2（偶数→女）
		{"110101990909022", time.Date(1999, 9, 9, 0, 0, 0, 0, time.Local), genderFemale},
	}
	for _, c := range cases {
		birthday, gender, err := parseIdentity(c.id)
		if err != nil {
			t.Errorf("parseIdentity(%q) err=%v", c.id, err)
			continue
		}
		if !birthday.Equal(c.birthday) || gender != c.gender {
			t.Errorf("parseIdentity(%q) = (%v,%s), want (%v,%s)", c.id, birthday, gender, c.birthday, c.gender)
		}
	}
	if _, _, err := parseIdentity("bad"); err == nil {
		t.Error("parseIdentity(\"bad\") should fail")
	}
}

// TestIsAdult 覆盖成年判断边界（参考 isAdult）。
func TestIsAdult(t *testing.T) {
	anchor := time.Date(2024, 1, 1, 0, 0, 0, 0, time.Local)
	cases := []struct {
		birthday time.Time
		want     bool
	}{
		// 恰好 18 岁（2006-01-01）
		{time.Date(2006, 1, 1, 0, 0, 0, 0, time.Local), true},
		// 差一天 18 岁
		{time.Date(2006, 1, 2, 0, 0, 0, 0, time.Local), false},
		// 超过 18 岁
		{time.Date(1990, 1, 1, 0, 0, 0, 0, time.Local), true},
	}
	for _, c := range cases {
		got, err := isAdult(c.birthday, anchor)
		if err != nil {
			t.Errorf("isAdult(%v) err=%v", c.birthday, err)
			continue
		}
		if got != c.want {
			t.Errorf("isAdult(%v) = %v, want %v", c.birthday, got, c.want)
		}
	}
	if _, err := isAdult(anchor.AddDate(1, 0, 0), anchor); err == nil {
		t.Error("future birthday should fail")
	}
}

// TestIsIDCard 覆盖身份证格式校验（参考 isIDCard）。
// 注意：与参考实现一致，18 位身份证末位校验码只接受小写 x（大写 X 需客户端先转小写）。
func TestIsIDCard(t *testing.T) {
	valid := []string{"110101199001020031", "11010119900102003x", "110101900102031"}
	for _, id := range valid {
		if !isIDCard(id) {
			t.Errorf("isIDCard(%q) = false, want true", id)
		}
	}
	invalid := []string{"", "123", "11010119900102003", "11010119900102003X", "abcdefghijklmnopqrst"}
	for _, id := range invalid {
		if isIDCard(id) {
			t.Errorf("isIDCard(%q) = true, want false", id)
		}
	}
}

// TestCardMD5 覆盖证件号哈希的盐与大小写归一（参考 cardMD5）。
func TestCardMD5(t *testing.T) {
	a := cardMD5("110101199001020031", RealnameCardTypeIdentitySafe, 0)
	b := cardMD5("11010119900102003X", 0, 0)
	if a == b {
		t.Error("different cards produce same md5")
	}
	// 小写归一：同一证件号大小写不同应得到相同哈希
	if cardMD5("abc123", 0, 0) != cardMD5("ABC123", 0, 0) {
		t.Error("cardMD5 should normalize case")
	}
	if len(a) != 32 {
		t.Errorf("md5 length = %d, want 32", len(a))
	}
}

// RealnameCardTypeIdentitySafe 与 model.RealnameCardTypeIdentity 一致（避免测试依赖 model 包外常量拼写）。
const RealnameCardTypeIdentitySafe = 0

// TestCardCryptorRoundtrip 覆盖证件号 RSA 加解密往返。
func TestCardCryptorRoundtrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustMarshalPKIX(t, &priv.PublicKey)})
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	c := NewCardCryptor(string(pubPEM), string(privPEM))
	plain := "110101199001020031"
	enc, err := c.CardEncrypt([]byte(plain))
	if err != nil {
		t.Fatalf("CardEncrypt err=%v", err)
	}
	if string(enc) == plain {
		t.Fatal("ciphertext equals plaintext")
	}
	dec, err := c.CardDecrypt(enc)
	if err != nil {
		t.Fatalf("CardDecrypt err=%v", err)
	}
	if string(dec) != plain {
		t.Fatalf("decrypted %q, want %q", dec, plain)
	}

	// 空输入往返
	if enc, err = c.CardEncrypt(nil); err != nil || len(enc) != 0 {
		t.Errorf("CardEncrypt(empty) = %q err=%v, want empty", enc, err)
	}
}

func mustMarshalPKIX(t *testing.T, pub *rsa.PublicKey) []byte {
	t.Helper()
	bs, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

// TestCardCryptorBadKey 覆盖密钥缺失/错误场景。
func TestCardCryptorBadKey(t *testing.T) {
	c := NewCardCryptor("", "")
	if _, err := c.CardEncrypt([]byte("x")); err == nil {
		t.Error("encrypt with empty key should fail")
	}
	if _, err := c.CardDecrypt([]byte("x")); err == nil {
		t.Error("decrypt with empty key should fail")
	}
}
