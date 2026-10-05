package model

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"strings"
)

// 凭证哈希与观测摘要的纯函数集合（不连库、不读配置，logic 与单测共用同一算式）。
//
// 两类哈希口径刻意不同，读路径必须与之严格对齐（见 README「哈希口径」）：
//   - 需要「由明文反查唯一键」的凭证（授权码 / access / refresh）用 HashCredential，
//     算式 HMAC(pepper, 明文)：随机 salt 掺进算式就无法定位 uniq_code_hash / uniq_access_hash。
//   - 按 app_id 定位的 client_secret 用 HashSecret，算式 HMAC(pepper, salt||明文)：
//     同一把明文落在两行里哈希不同，抗跨行比对。
//
// pepper 为空一律返回 ErrSecretVerificationUnavailable：宁可整条链路不可用，
// 也不退化成「无 pepper 哈希」或「跳过比对放行」（fail closed，AGENTS.md §9）。

// HashCredential 计算「按值定位」凭证（授权码、access、refresh）的哈希。
func HashCredential(pepper, plaintext string) (string, error) {
	if pepper == "" {
		return "", ErrSecretVerificationUnavailable
	}
	if plaintext == "" {
		return "", ErrTokenInvalid
	}
	return hexHMAC(pepper, plaintext), nil
}

// HashSecret 计算 client_secret 的哈希：HMAC(pepper, salt||secret)。
func HashSecret(pepper, salt, secret string) (string, error) {
	if pepper == "" {
		return "", ErrSecretVerificationUnavailable
	}
	if salt == "" || secret == "" {
		return "", ErrSecretNotConfigured
	}
	return hexHMAC(pepper, salt+secret), nil
}

// SecretHashMatches 恒定时间比对密钥哈希。任一入参为空即不匹配，绝不「空值放行」。
func SecretHashMatches(wantHash, gotHash string) bool {
	if wantHash == "" || gotHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(wantHash)),
		[]byte(strings.ToLower(gotHash))) == 1
}

// RandomHex 返回 n 字节的密码学随机数的 hex 表示（生成明文凭证与 salt）。
func RandomHex(nBytes int) (string, error) {
	if nBytes <= 0 || nBytes > 128 {
		return "", ErrInvalidCursor
	}
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// DigestPayload 计算正文摘要，格式 sha256:<hex>。响应侧只回摘要，不回正文。
func DigestPayload(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// MaskClientIP 把来源 IP 脱敏成可入库的观测值：
// IPv4 抹掉末段、IPv6 只保留前两组、无法解析一律 "unknown"。
// 流水与日志只存本函数产物，绝不存明文 IP（AGENTS.md §7 隐私最小化）。
func MaskClientIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "unknown"
	}
	if v4 := parsed.To4(); v4 != nil {
		return "v4:" + itoa(int(v4[0])) + "." + itoa(int(v4[1])) + ".x.x"
	}
	return "v6:" + itoa(int(parsed[0])) + ":" + itoa(int(parsed[1])) + "::"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := make([]byte, 0, 3)
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

// IsPublicHostAddr 判断解析出的地址是否可作为外呼目标（SSRF 护栏）。
// 回环、私网、链路本地、接口本地、组播、未指定地址与 100.64/10（CGNAT）一律拒绝。
func IsPublicHostAddr(addr net.IP) bool {
	if addr == nil {
		return false
	}
	switch {
	case addr.IsLoopback(), addr.IsPrivate(), addr.IsLinkLocalUnicast(),
		addr.IsLinkLocalMulticast(), addr.IsInterfaceLocalMulticast(),
		addr.IsMulticast(), !addr.IsGlobalUnicast():
		return false
	}
	// 100.64.0.0/10 是运营商级 NAT，不是私网段但同样不可公网投递。
	if v4 := addr.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return true
}

// CanonicalRequest 定死开放接口签名的规范化串（跨语言客户端唯一口径）：
//
//	METHOD \n path \n timestamp \n nonce \n api_code \n body_digest
//
// 归一规则：字段为空用 "-" 占位（保证字段数恒定，拼接不可伪造歧义）；
// method 取大写并去空白；path 只保留路径本身（大小写敏感，不做百分号解码）；
// body_digest 归一到 sha256:<小写 hex> 或 "-"。
//
// 注意：本服务当前只有签发侧的规范化实现，验签侧因 client_secret 只存单向哈希
// 而无法重算 HMAC（见 README「已知缺口」），因此该方法暂供投递与客户端 SDK 对齐使用。
func CanonicalRequest(method, path string, timestamp int64, nonce, apiCode, bodyDigest string) string {
	fields := []string{
		canonicalField(strings.ToUpper(method)),
		canonicalField(path),
		itoa64(timestamp),
		canonicalField(nonce),
		canonicalField(apiCode),
		NormalizeBodyDigest(bodyDigest),
	}
	return strings.Join(fields, "\n")
}

// NormalizeBodyDigest 把客户端传来的摘要归一成 sha256:<hex>；空值回 "-"。
func NormalizeBodyDigest(digest string) string {
	d := canonicalField(digest)
	if d == placeholderField {
		return placeholderField
	}
	d = strings.ToLower(strings.TrimPrefix(d, "sha256:"))
	if len(d) != sha256HexLen || !isHex(d) {
		// 非法摘要保留可辨识前缀，比对必然失败，但不panic也不猜测。
		return "sha256:invalid"
	}
	return "sha256:" + d
}

const (
	placeholderField = "-"
	sha256HexLen     = 64
)

func canonicalField(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return placeholderField
	}
	return v
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	digits := make([]byte, 0, 20)
	for v > 0 {
		digits = append([]byte{byte('0' + byte(v%10))}, digits...)
		v /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

func hexHMAC(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}
