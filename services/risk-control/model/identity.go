package model

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
)

// 本文件落实 AGENTS.md §5/§7 的敏感信息约束：
// 设备号只以 SHA-256 摘要形式落库（受控 ID），IP 只接受调用方预哈希后的摘要，
// 服务端永远不接收、不保存、不打印明文 IP/手机号/身份证。

// DeviceHash 计算设备受控 ID：sha256(小写去空格后的设备标识) 的十六进制。
// 返回空串表示输入不可用。
func DeviceHash(deviceID string) string {
	trimmed := strings.TrimSpace(deviceID)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(trimmed)))
	return hex.EncodeToString(sum[:])
}

// LooksLikeRawIP 判定字符串是否是裸 IP 地址（v4/v6）。
// 用于拒绝调用方把明文 IP 当作 ip_hash 或名单 target_value 传入。
func LooksLikeRawIP(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if net.ParseIP(value) != nil {
		return true
	}
	// 带端口的 host:port 或 X-Forwarded-For 链同样视为裸 IP。
	if host, _, err := net.SplitHostPort(value); err == nil {
		return net.ParseIP(host) != nil
	}
	if idx := strings.IndexByte(value, ','); idx >= 0 {
		return net.ParseIP(strings.TrimSpace(value[:idx])) != nil
	}
	return false
}

// NormalizeIPHash 校验并规范化调用方传入的 ip_hash。
// 规则：非空、十六进制、长度 8-64、且不得是裸 IP；返回小写十六进制。
// 不满足时返回空串，由调用方按「未提供 ip_hash」处理（记录降级说明，不伪造数据）。
func NormalizeIPHash(ipHash string) string {
	v := strings.ToLower(strings.TrimSpace(ipHash))
	if v == "" || LooksLikeRawIP(v) {
		return ""
	}
	if len(v) < 8 || len(v) > 64 {
		return ""
	}
	if _, err := hex.DecodeString(v); err != nil {
		return ""
	}
	return v
}

// NormalizeTargetValue 规范化名单 target_value：
// 设备/IP 类必须是小写十六进制摘要，账号类是十进制字符串。
// 返回空串表示非法输入。
func NormalizeTargetValue(targetType int32, value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	switch targetType {
	case TargetTypeMid:
		for _, r := range v {
			if r < '0' || r > '9' {
				return ""
			}
		}
		// 归一到最简十进制。裁决读侧（repository.activeListEntries）用 strconv.FormatInt(mid,10)
		// 组 target_value，若放过 "042" 这种写法，同一条封禁会按写法存成两行，
		// 而带前导零的那一行永远命中不了 —— 黑名单是安全控制，不能静默失效（fail-open）。
		if trimmed := strings.TrimLeft(v, "0"); trimmed != "" {
			return trimmed
		}
		return "0" // 全零：规范化成单个 0（mid<=0 不会被查询，条目本身仍是惰性的）
	case TargetTypeDevice, TargetTypeIpHash:
		return NormalizeIPHash(v)
	default:
		return ""
	}
}

// TruncateHash 取摘要前 n 个字符用于日志展示（不落全量摘要，避免日志成为反查索引）。
func TruncateHash(hash string, n int) string {
	if hash == "" {
		return ""
	}
	if n <= 0 || len(hash) <= n {
		return hash
	}
	return hash[:n]
}
