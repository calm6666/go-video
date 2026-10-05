package model

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strconv"
	"strings"
)

// 隐私脱敏纯函数（AGENTS.md §7、proto 头部硬约束）。
//
// 本服务库里绝不允许出现明文 IP、设备号、手机号：入口处只保留
// HMAC-SHA256(盐, 盐版本||原值) 与 IP 段。原值仅在请求生命周期内存在，
// 也不写日志。所有函数都是纯函数、可直接单测，且不返回可反推原值的中间量。

// hashPrefix 标识哈希算法，便于日后换代（sha256 → 加强 KDF）时区分历史数据。
const hashPrefix = "h1:"

// SaltedHash 计算加盐哈希：hex(HMAC-SHA256(salt, "v<saltVersion>|<value>"))，带算法前缀。
//
//   - 盐版本进摘要，轮换后同一原值在不同版本下得到不同结果，旧数据不可逆推；
//   - salt 为空即返回 ErrSaltMissing，不允许退化成「对原值直接 sha256」——
//     设备号/手机号空间有限，无盐哈希可被彩虹表还原（等于明文入库）。
//   - value 为空返回空串：未登录且无设备号的事件由 logic 判 REJECT_MISSING_SUBJECT。
func SaltedHash(salt string, saltVersion int32, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	if salt == "" {
		return "", ErrSaltMissing
	}
	if saltVersion <= 0 {
		return "", ErrSaltMissing
	}
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte("v" + strconv.FormatInt(int64(saltVersion), 10) + "|" + value))
	return hashPrefix + hex.EncodeToString(mac.Sum(nil)), nil
}

// SaltedHashEqual 恒定时间比较两个哈希，避免按前缀命中差异做枚举。
func SaltedHashEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// DefaultIPSegmentBits IPv4 默认脱敏前缀长度（/24，proto 注释里的默认口径）。
const DefaultIPSegmentBits = 24

// MaxIPv4SegmentBits IPv4 允许的最大前缀长度：/32 等于把出口 IP 原样入库，禁止。
const MaxIPv4SegmentBits = 31

// DefaultIPv6SegmentBits IPv6 默认脱敏前缀长度（同一 NAT 出口下的常见粒度）。
const DefaultIPv6SegmentBits = 64

// MaxIPv6SegmentBits IPv6 允许的最大前缀长度：/128 同样等于明文地址。
const MaxIPv6SegmentBits = 64

// IPSegment 把出口 IP 收敛为脱敏网段字符串，如 "203.0.113.0/24"。
//
// - 只保留网络地址 + 前缀长度，主机位一律清零，无法回溯到具体设备；
// - IPv6 默认 /64（同一 NAT 出口下的常见粒度）；
// - 前缀越界（IPv4 >= /32、IPv6 > /64）一律回落到默认值，绝不产出等于明文的段；
// - 非法 IP 返回空串，调用方按「无 IP 维度」处理，不得回退成原始字符串。
func IPSegment(ip string, bits int) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if v4 := parsed.To4(); v4 != nil {
		if bits <= 0 || bits > MaxIPv4SegmentBits {
			bits = DefaultIPSegmentBits
		}
		return maskSegment(v4, bits, 32)
	}
	prefix := DefaultIPv6SegmentBits
	if bits > 0 && bits <= MaxIPv6SegmentBits {
		prefix = bits
	}
	return maskSegment(parsed.To16(), prefix, 128)
}

func maskSegment(addr net.IP, bits, width int) string {
	masked := make(net.IP, len(addr))
	mask := net.CIDRMask(bits, width)
	for i := range masked {
		masked[i] = addr[i] & mask[i]
	}
	return masked.String() + "/" + strconv.Itoa(bits)
}

// DigestPrefix payload 摘要前缀，写入 ec_event_record.payload_digest。
const DigestPrefix = "sha256:"

// PayloadDigest 计算事件正文摘要与字节数：原文不入库，只留摘要（proto EventRecord.payload_digest）。
// 摘要用于死信排查时比对「重放的是不是同一条内容」，不用于还原内容。
func PayloadDigest(payload string) (string, int32) {
	if payload == "" {
		return "", 0
	}
	sum := sha256.Sum256([]byte(payload))
	return DigestPrefix + hex.EncodeToString(sum[:]), int32(len(payload))
}

// KeywordDigest 搜索词只存摘要与长度（AGENTS.md §7：行为台账不留可还原的明文）。
// 明文搜索词只在投递信封里出现一次，由 spm 侧按自身隐私策略处理。
func KeywordDigest(keyword string, maxRunes int) (digest string, runes int32, truncated bool) {
	rs := []rune(keyword)
	if len(rs) == 0 {
		return "", 0, false
	}
	if maxRunes > 0 && len(rs) > maxRunes {
		rs = rs[:maxRunes]
		truncated = true
	}
	sum := sha256.Sum256([]byte(string(rs)))
	return DigestPrefix + hex.EncodeToString(sum[:]), int32(len(rs)), truncated
}
