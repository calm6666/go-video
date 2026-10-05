package signurl

import (
	"crypto/md5" // #nosec G501 -- 与被测实现同一算法，用于对账期望值
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const (
	testKey     = "unit-test-private-key"
	testURI     = "/ugc/12/34/56.m3u8"
	testRand    = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	fixedNow    = int64(1_800_000_000)
	fixedExpire = int64(1_800_001_800) // fixedNow + 1800
	testKeyID   = "k1"
	testBaseURL = "https://play.example.com"
	testHexLen  = 32
)

// newSigner 构造一个启用签名的测试签名器。
func newSigner(t *testing.T, privateKey string) *Signer {
	t.Helper()
	s, err := New(Config{
		BaseURL:       testBaseURL,
		PrivateKey:    privateKey,
		KeyID:         testKeyID,
		TokenTTL:      1800,
		EnableAuthKey: true,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s
}

// TestHashMatchesProtocol 断言 md5hash = MD5("URI-ts-rand-uid-PrivateKey")，
// 即 CDN 侧配置的计算口径（固定输入 → 固定摘要）。
func TestHashMatchesProtocol(t *testing.T) {
	raw := testURI + "-" + "1800001800" + "-" + testRand + "-" + "42" + "-" + testKey
	sum := md5.Sum([]byte(raw)) // #nosec G401
	want := hex.EncodeToString(sum[:])

	got := Hash(testURI, 1800001800, testRand, "42", testKey)
	if got != want {
		t.Fatalf("Hash() = %q, want %q", got, want)
	}
	if len(got) != testHexLen {
		t.Fatalf("Hash() length = %d, want %d", len(got), testHexLen)
	}
	// 私钥不同必须得到不同摘要（防盗链的唯一防线）。
	if Hash(testURI, 1800001800, testRand, "42", "other-key") == got {
		t.Fatal("Hash() must depend on the private key")
	}
}

func TestSignIsDeterministic(t *testing.T) {
	s := newSigner(t, testKey)
	got, err := s.Sign(testURI, "42", fixedExpire, testRand)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	want := strings.Join([]string{
		"1800001800", testRand, "42", Hash(testURI, fixedExpire, testRand, "42", testKey),
	}, "-")
	if got != want {
		t.Fatalf("Sign() = %q, want %q", got, want)
	}
	// 同参数重复调用结果一致（幂等重放会走这条路径）。
	again, err := s.Sign(testURI, "42", fixedExpire, testRand)
	if err != nil || again != got {
		t.Fatalf("Sign() replay = %q, %v; want %q", again, err, got)
	}
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	s := newSigner(t, testKey)
	authKey, err := s.Sign(testURI, "7", fixedExpire, testRand)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	tok, err := s.Verify(testURI, authKey, fixedNow)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if tok.ExpireAt != fixedExpire || tok.Rand != testRand || tok.UID != "7" {
		t.Fatalf("Verify() token = %+v, want expire=%d rand=%s uid=7", tok, fixedExpire, testRand)
	}
}

func TestVerifyRejections(t *testing.T) {
	s := newSigner(t, testKey)
	authKey, err := s.Sign(testURI, "7", fixedExpire, testRand)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}

	cases := []struct {
		name    string
		uri     string
		authKey string
		now     int64
		want    error
	}{
		{"过期", testURI, authKey, fixedExpire, ErrAuthKeyExpired},
		{"刚过期一秒", testURI, authKey, fixedExpire + 1, ErrAuthKeyExpired},
		{"URI 被换", "/ugc/other.m3u8", authKey, fixedNow, ErrSignatureMismatch},
		{"格式非法-段数不对", testURI, "1-2-3", fixedNow, ErrMalformedAuthKey},
		{"格式非法-ts 非数字", testURI, "abc-a-b-c", fixedNow, ErrMalformedAuthKey},
		{"格式非法-ts 为 0", testURI, "0-a-b-c", fixedNow, ErrMalformedAuthKey},
		{"格式非法-空摘要", testURI, "1800001800-a-b-", fixedNow, ErrMalformedAuthKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Verify(tc.uri, tc.authKey, tc.now); !errors.Is(err, tc.want) {
				t.Fatalf("Verify() error = %v, want %v", err, tc.want)
			}
		})
	}

	// 段数不足（只有三段）必须报格式错误。
	if _, err := s.Verify(testURI, "1800001800-a-b", fixedNow); !errors.Is(err, ErrMalformedAuthKey) {
		t.Fatalf("Verify() short key error = %v, want %v", err, ErrMalformedAuthKey)
	}
}

func TestSignRequiresPrivateKey(t *testing.T) {
	s := newSigner(t, "")
	if _, err := s.Sign(testURI, "1", fixedExpire, testRand); !errors.Is(err, ErrPrivateKeyRequired) {
		t.Fatalf("Sign() error = %v, want %v", err, ErrPrivateKeyRequired)
	}
	if _, err := s.Verify(testURI, "1800001800-a-b-c", fixedNow); !errors.Is(err, ErrPrivateKeyRequired) {
		t.Fatalf("Verify() error = %v, want %v", err, ErrPrivateKeyRequired)
	}
}

// TestNewRejectsUnsignedOutsideDev 保证生产模式下关闭防盗链会直接构造失败，
// 服务不会退化成公共地址分发（AGENTS.md §6）。
func TestNewRejectsUnsignedOutsideDev(t *testing.T) {
	if _, err := New(Config{EnableAuthKey: false, AllowUnsigned: false}); !errors.Is(err, ErrAuthKeyDisabled) {
		t.Fatalf("New() error = %v, want %v", err, ErrAuthKeyDisabled)
	}
	s, err := New(Config{BaseURL: testBaseURL, EnableAuthKey: false, AllowUnsigned: true})
	if err != nil {
		t.Fatalf("New(dev) error = %v", err)
	}
	if s.Signed() {
		t.Fatal("Signed() = true, want false when EnableAuthKey=false")
	}
	key, err := s.Sign(testURI, "1", fixedExpire, testRand)
	if err != nil || key != "" {
		t.Fatalf("Sign() = %q, %v; want empty auth_key in dev", key, err)
	}
	// dev 模式下 Verify 显式报错，避免调用方误以为已校验通过。
	if _, err := s.Verify(testURI, "", fixedNow); !errors.Is(err, ErrAuthKeyDisabled) {
		t.Fatalf("Verify() error = %v, want %v", err, ErrAuthKeyDisabled)
	}
}

func TestTTLDefaultAndExplicit(t *testing.T) {
	def, err := New(Config{EnableAuthKey: true, PrivateKey: testKey})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if def.TTL() != DefaultTTL {
		t.Fatalf("TTL() = %d, want default %d", def.TTL(), DefaultTTL)
	}
	custom, err := New(Config{EnableAuthKey: true, PrivateKey: testKey, TokenTTL: 60})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if custom.TTL() != 60 {
		t.Fatalf("TTL() = %d, want 60", custom.TTL())
	}
	// expireAt 非正数属于策略计算错误，必须拒绝而不是签出一个立刻失效的地址。
	if _, err := custom.Sign(testURI, "1", 0, testRand); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("Sign() error = %v, want %v", err, ErrInvalidTTL)
	}
}

func TestNormalizeURI(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"补前导斜杠", "ugc/1/2.m3u8", "/ugc/1/2.m3u8", false},
		{"已有斜杠", "/ugc/1/2.m3u8", "/ugc/1/2.m3u8", false},
		{"折叠重复斜杠", "/ugc//1///2.m3u8", "/ugc/1/2.m3u8", false},
		{"去掉首尾空白", "  /ugc/1.m3u8  ", "/ugc/1.m3u8", false},
		{"空串", "", "", true},
		{"绝对 URL", "https://oss.example.com/a.m3u8", "", true},
		{"带 query", "/a.m3u8?x=1", "", true},
		{"带 fragment", "/a.m3u8#p", "", true},
		{"反斜杠", "C:\\a\\b", "", true},
		{"路径穿越", "/ugc/../../etc/passwd", "", true},
		{"单点段", "/ugc/./1.m3u8", "/ugc/1.m3u8", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeURI(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidObjectKey) {
					t.Fatalf("NormalizeURI(%q) error = %v, want %v", tc.in, err, ErrInvalidObjectKey)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("NormalizeURI(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestBuildURL(t *testing.T) {
	s := newSigner(t, testKey)
	if got := s.BuildURL(testURI, ""); got != testBaseURL+testURI {
		t.Fatalf("BuildURL(unsigned) = %q", got)
	}
	got := s.BuildURL(testURI, "1-a-b-c")
	if got != testBaseURL+testURI+"?auth_key=1-a-b-c" {
		t.Fatalf("BuildURL() = %q", got)
	}
	// BaseURL 结尾斜杠不应产生双斜杠。
	s2, err := New(Config{BaseURL: testBaseURL + "/", PrivateKey: testKey, EnableAuthKey: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := s2.BuildURL(testURI, "1-a-b-c"); strings.Contains(got, "com//") {
		t.Fatalf("BuildURL() = %q, want single slash", got)
	}
}

func TestSignGeneratesRandomWhenEmpty(t *testing.T) {
	s := newSigner(t, testKey)
	a, err := s.Sign(testURI, "", fixedExpire, "")
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	b, err := s.Sign(testURI, "", fixedExpire, "")
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if a == b {
		t.Fatal("two signatures with empty rand must differ (random suffix)")
	}
	tok, err := Parse(a)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if tok.UID != "0" {
		t.Fatalf("Parse() uid = %q, want 0 for empty uid", tok.UID)
	}
	// 签名后必须能通过校验（uid 由 Sign 内部补 0，Verify 用串里的 uid）。
	if _, err := s.Verify(testURI, a, fixedNow); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestRandomString(t *testing.T) {
	a, err := RandomString(8)
	if err != nil {
		t.Fatalf("RandomString() error = %v", err)
	}
	if len(a) != 16 {
		t.Fatalf("RandomString(8) length = %d, want 16 hex chars", len(a))
	}
	if _, err := RandomString(0); err == nil {
		t.Fatal("RandomString(0) must error")
	}
}
