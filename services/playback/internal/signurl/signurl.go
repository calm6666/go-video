// Package signurl 实现 CDN A 型防盗链签名的生成与校验（playback 领域策略，手写文件）。
//
// 规则（对齐阿里云/腾讯云 CDN Type A）：
//
//	auth_key = ts-rand-uid-md5hash
//	md5hash  = MD5("URI-ts-rand-uid-PrivateKey")   // 32 位小写十六进制
//
// 其中：
//   - URI  是被保护资源的访问路径（以 / 开头，不含 query），签名与路径绑定，换路径即失效；
//   - ts   是签名的**过期时间点**（Unix 秒），不是签发时间，CDN 在 now > ts 时拒绝回源；
//   - rand 是随机串（通常是 UUID），保证同一秒签发的两个地址不同，避免碰撞与重放；
//   - uid  是访问者标识（本项目使用播放会话所属 mid，游客为 0），仅用于 CDN 日志排查；
//   - PrivateKey 只存在于服务端配置（Secret/Vault），绝不下发给客户端（AGENTS.md §6）。
//
// 本包只做纯计算，不访问数据库、Redis 或配置中心，便于确定性单元测试。
package signurl

import (
	"crypto/md5" // #nosec G501 -- CDN Type A 协议固定使用 MD5，非用于口令散列
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultTTL 是未配置 TokenTTL 时的默认签名有效期（秒）。
const DefaultTTL int64 = 1800

// 签名相关错误：调用方（logic）据此返回明确错误码，不得伪造成功。
var (
	// ErrPrivateKeyRequired 私钥未配置。生产必须从 Secret/Vault 注入。
	ErrPrivateKeyRequired = errors.New("playback: cdn private key is not configured")
	// ErrAuthKeyDisabled 关闭了防盗链签名，但当前运行模式不允许（仅 dev/test 允许）。
	ErrAuthKeyDisabled = errors.New("playback: cdn auth key signing is disabled in this mode")
	// ErrMalformedAuthKey auth_key 串格式不合法。
	ErrMalformedAuthKey = errors.New("playback: malformed auth_key")
	// ErrSignatureMismatch 签名不匹配（URI 被篡改、私钥轮换或串伪造）。
	ErrSignatureMismatch = errors.New("playback: auth_key signature mismatch")
	// ErrAuthKeyExpired auth_key 已过期。
	ErrAuthKeyExpired = errors.New("playback: auth_key expired")
	// ErrInvalidTTL TokenTTL 非正数。
	ErrInvalidTTL = errors.New("playback: sign token ttl must be positive")
	// ErrInvalidObjectKey object_key 非法（空、绝对 URL、路径穿越或带 query）。
	ErrInvalidObjectKey = errors.New("playback: invalid object key")
)

// Token 是解析后的 auth_key。
type Token struct {
	// ExpireAt 是签名过期时间点（Unix 秒，A 型的 ts 字段）。
	ExpireAt int64
	// Rand 是随机串。
	Rand string
	// UID 是签发时写入的访问者标识。
	UID string
	// Hash 是签名摘要。
	Hash string
}

// Config 是 Signer 的构造配置。
type Config struct {
	// BaseURL CDN 访问域名，如 https://play.example.com（不带结尾斜杠）。
	BaseURL string
	// PrivateKey 防盗链私钥。
	PrivateKey string
	// KeyID 当前密钥标识，用于轮换排障。
	KeyID string
	// TokenTTL 签名有效期（秒）；<=0 时取 DefaultTTL。
	TokenTTL int64
	// EnableAuthKey 是否启用签名。false 时只有 AllowUnsigned=true 才可用。
	EnableAuthKey bool
	// AllowUnsigned 由调用方按运行模式（dev/test）设置，生产必须为 false。
	AllowUnsigned bool
}

// Signer 生成与校验 CDN 防盗链签名。构造后只读，可并发使用。
type Signer struct {
	baseURL     string
	privateKey  string
	keyID       string
	ttl         int64
	enableAuthK bool
}

// New 按配置构造 Signer。
// 启用签名时允许私钥为空（服务仍可对外提供 GetSession 等只读方法），
// 但 Sign/Verify 会返回 ErrPrivateKeyRequired，绝不签发无签名地址；
// 关闭签名时（EnableAuthKey=false）必须 AllowUnsigned=true，否则构造失败，
// 避免生产配置误写导致整个服务退化成公共地址分发。
func New(cfg Config) (*Signer, error) {
	ttl := cfg.TokenTTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if !cfg.EnableAuthKey && !cfg.AllowUnsigned {
		return nil, ErrAuthKeyDisabled
	}
	return &Signer{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		privateKey:  cfg.PrivateKey,
		keyID:       cfg.KeyID,
		ttl:         ttl,
		enableAuthK: cfg.EnableAuthKey,
	}, nil
}

// Signed 返回是否启用 CDN 鉴权串。
func (s *Signer) Signed() bool { return s.enableAuthK }

// TTL 返回签名有效期（秒）。
func (s *Signer) TTL() int64 { return s.ttl }

// KeyID 返回当前签名密钥标识。
func (s *Signer) KeyID() string { return s.keyID }

// BaseURL 返回 CDN 访问域名。
func (s *Signer) BaseURL() string { return s.baseURL }

// NormalizeURI 把媒资 object_key 规范成签名使用的 URI 路径。
// 规则：拒绝空串、绝对 URL、query/fragment、反斜杠与 "."/".." 路径穿越；
// 统一以单个 "/" 开头，折叠重复斜杠。
func NormalizeURI(objectKey string) (string, error) {
	k := strings.TrimSpace(objectKey)
	if k == "" {
		return "", ErrInvalidObjectKey
	}
	if strings.Contains(k, "://") || strings.Contains(k, "?") ||
		strings.Contains(k, "#") || strings.Contains(k, "\\") {
		return "", ErrInvalidObjectKey
	}
	parts := strings.Split(k, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			return "", ErrInvalidObjectKey
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return "", ErrInvalidObjectKey
	}
	return "/" + strings.Join(out, "/"), nil
}

// Hash 计算 A 型签名的 md5hash：MD5("URI-ts-rand-uid-PrivateKey")。
// 导出以便测试和 CDN 配置对账使用。
func Hash(uri string, ts int64, random, uid, privateKey string) string {
	raw := fmt.Sprintf("%s-%d-%s-%s-%s", uri, ts, random, uid, privateKey)
	sum := md5.Sum([]byte(raw)) // #nosec G401 -- CDN Type A 协议要求 MD5
	return hex.EncodeToString(sum[:])
}

// Sign 为 uri 在指定过期时间点生成 auth_key。
//
// expireAt 由调用方（logic）按 TTL 与版权窗口上限计算后传入，本包不做策略判断；
// random 为空时自动生成 16 字节随机十六进制串；uid 传播放会话所属 mid 的十进制字符串。
// 未启用签名时返回空 auth_key（logic 会记录降级日志）；私钥缺失时返回明确错误，
// 绝不签发无签名地址（AGENTS.md §6）。
func (s *Signer) Sign(uri, uid string, expireAt int64, random string) (string, error) {
	if !s.enableAuthK {
		return "", nil
	}
	if s.privateKey == "" {
		return "", ErrPrivateKeyRequired
	}
	if expireAt <= 0 {
		return "", ErrInvalidTTL
	}
	if random == "" {
		var err error
		if random, err = RandomString(16); err != nil {
			return "", err
		}
	}
	if uid == "" {
		uid = "0"
	}
	return fmt.Sprintf("%d-%s-%s-%s",
		expireAt, random, uid, Hash(uri, expireAt, random, uid, s.privateKey)), nil
}

// BuildURL 拼接最终播放地址。authKey 为空（未启用签名）时返回裸地址。
func (s *Signer) BuildURL(uri, authKey string) string {
	if authKey == "" {
		return s.baseURL + uri
	}
	return fmt.Sprintf("%s%s?auth_key=%s", s.baseURL, uri, authKey)
}

// Parse 拆解 auth_key 为 ts-rand-uid-md5hash 四段。
func Parse(authKey string) (*Token, error) {
	seg := strings.Split(strings.TrimSpace(authKey), "-")
	if len(seg) != 4 {
		return nil, ErrMalformedAuthKey
	}
	ts, err := strconv.ParseInt(seg[0], 10, 64)
	if err != nil || ts <= 0 {
		return nil, ErrMalformedAuthKey
	}
	if seg[1] == "" || seg[3] == "" {
		return nil, ErrMalformedAuthKey
	}
	return &Token{ExpireAt: ts, Rand: seg[1], UID: seg[2], Hash: seg[3]}, nil
}

// Verify 校验 auth_key 是否与 uri 匹配且未过期，返回解析后的 Token。
// 只做协议层校验；会话状态（撤销/过期）由 logic 结合 playback_session 判定。
func (s *Signer) Verify(uri, authKey string, now int64) (*Token, error) {
	if !s.enableAuthK {
		return nil, ErrAuthKeyDisabled
	}
	if s.privateKey == "" {
		return nil, ErrPrivateKeyRequired
	}
	t, err := Parse(authKey)
	if err != nil {
		return nil, err
	}
	if t.ExpireAt <= now {
		return nil, ErrAuthKeyExpired
	}
	if Hash(uri, t.ExpireAt, t.Rand, t.UID, s.privateKey) != t.Hash {
		return nil, ErrSignatureMismatch
	}
	return t, nil
}

// RandomString 返回 n 字节随机数据的十六进制表示，用于 auth_key 的 rand 字段。
func RandomString(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("signurl: RandomString requires n > 0, got %d", n)
	}
	b := make([]byte, n)
	if _, err := cryptorand.Read(b); err != nil {
		return "", fmt.Errorf("signurl: read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
