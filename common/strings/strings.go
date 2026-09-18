// Package strings 提供纯字符串切片工具，补充标准库 strings 的不足。
//
// 所有函数都是纯函数：不读取全局状态、不进行 IO。传入 nil 切片视为空切片，
// 不会触发 panic。
package strings

import (
	"strconv"
	"strings"
)

// JoinInts 将 int64 切片用逗号拼接成 "n1,n2,n3" 形式。
// 输入为空或 nil 时返回空字符串。
func JoinInts(is []int64) string {
	if len(is) == 0 {
		return ""
	}
	if len(is) == 1 {
		return strconv.FormatInt(is[0], 10)
	}
	var b strings.Builder
	b.Grow(len(is) * 4)
	for i, v := range is {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(v, 10))
	}
	return b.String()
}

// SplitInts 将 "n1,n2,n3" 解析为 []int64。
// 输入为空字符串时返回 (nil, nil)；任意非数字 token 会原样返回底层 strconv 错误。
func SplitInts(s string) ([]int64, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// JoinStrings 用 sep 拼接非空字符串，保留原始顺序。
func JoinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	var b strings.Builder
	first := true
	for _, s := range ss {
		if s == "" {
			continue
		}
		if !first {
			b.WriteString(sep)
		}
		b.WriteString(s)
		first = false
	}
	return b.String()
}

// DedupStrings 返回去重后的新切片，保留首次出现的位置。
// 输入为 nil 时返回 nil。
func DedupStrings(ss []string) []string {
	if ss == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// ContainsString 判断 target 是否出现在 ss 中。
// 仅适用于小切片；大集合查找应使用 map。
func ContainsString(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

// Truncate 将 s 截断到最多 max 个 rune。
// max <= 0 时返回空字符串。截断时不附加省略号，调用方可自行组合。
// 基于 rune 的截断可避免切到多字节字符（如中文）的中间。
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
