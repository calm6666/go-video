package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// KeywordHash 返回关键词的 sha256 十六进制（64 字符）。
// 用于唯一索引定位与聚合分组：避免 utf8mb4 排序规则差异导致的等值判定漂移，
// 也避免在索引里长期保存完整关键词之外的额外副本。
func KeywordHash(keyword string) string {
	sum := sha256.Sum256([]byte(keyword))
	return hex.EncodeToString(sum[:])
}

// EscapeLikePrefix 转义 LIKE 前缀匹配中的 % 、_ 与 \，防止用户输入被当作通配符。
// 调用方拼接为 `keyword LIKE ? ` + ` ESCAPE '\'`（本仓库 model 内统一使用）。
func EscapeLikePrefix(prefix string) string {
	var b strings.Builder
	b.Grow(len(prefix) + 8)
	for _, r := range prefix {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NormalizeScope 校验并归一化热词作用域：
//
//	""            -> global
//	"global"      -> global
//	"zone:16"     -> zone:16（zone_id 必须为正整数）
//
// 其它输入返回 ErrInvalidScope，避免脏 scope 进入唯一索引。
func NormalizeScope(scope string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(scope))
	switch {
	case s == "":
		return ScopeGlobal, nil
	case s == ScopeGlobal:
		return ScopeGlobal, nil
	case strings.HasPrefix(s, "zone:"):
		idStr := strings.TrimPrefix(s, "zone:")
		id, err := strconv.ParseInt(idStr, 10, 32)
		if err != nil || id <= 0 {
			return "", fmt.Errorf("%w: %q", ErrInvalidScope, scope)
		}
		return fmt.Sprintf("zone:%d", id), nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidScope, scope)
	}
}

// ScopeZoneID 解析 zone scope；非 zone scope 返回 (0, false)。
func ScopeZoneID(scope string) (int32, bool) {
	if !strings.HasPrefix(scope, "zone:") {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(scope, "zone:"), 10, 32)
	if err != nil || id <= 0 {
		return 0, false
	}
	return int32(id), true
}

// IsValidResultState 判定查询日志 result_state 是否合法。
func IsValidResultState(s string) bool {
	switch s {
	case ResultStateOK, ResultStateEmpty, ResultStateDegraded, ResultStateBlocked:
		return true
	default:
		return false
	}
}
