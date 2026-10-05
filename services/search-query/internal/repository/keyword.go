package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"go-video/services/search-query/model"
)

// 关键词与查询身份的规范化。
//
// 为什么需要：同一个词在不同端可能有全角/多余空格/控制字符差异，规范化后
// 才能作为缓存 key、历史唯一键与热词聚合分组；同时把不可打印字符挡在
// 引擎查询之外，避免把异常输入写进 DSL。

// MaxKeywordRunes 兜底上限：即使配置项写得很宽，也不允许超过该长度，
// 防止超长关键词把缓存 key 与 DB 索引撑爆（search_history.keyword 为 varchar(128)）。
const MaxKeywordRunes = 128

// StripControl 清洗关键词字符：
//   - 控制字符（含换行/制表符/DEL）直接移除，避免污染日志与 DSL；
//   - 尖括号替换为空格：引擎返回的 _highlight 会把命中片段包在 <em> 标签里，
//     若关键词本身能带标签字符，客户端按富文本渲染时会被注入自定义标记。
//     服务端在入口就把它挡掉（真正的渲染安全仍由客户端负责，见 AGENTS.md §6）。
func StripControl(s string) string {
	if s == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0x7f || unicode.IsControl(r):
			return -1
		case r == '<' || r == '>':
			return ' '
		default:
			return r
		}
	}, s)
}

// NormalizeKeyword 规范化关键词：清洗字符、折叠连续空白为单个空格、两端裁剪。
func NormalizeKeyword(s string) string {
	s = strings.ReplaceAll(StripControl(s), "\u00a0", " ")
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			inSpace = true
			continue
		}
		if inSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// ValidateKeyword 校验并规范化关键词。
// maxLen <= 0 时使用 MaxKeywordRunes 兜底；空串、纯空白、超长都返回
// model.ErrInvalidKeyword（包装原因，便于日志定位）。
func ValidateKeyword(s string, maxLen int) (string, error) {
	if maxLen <= 0 || maxLen > MaxKeywordRunes {
		maxLen = MaxKeywordRunes
	}
	kw := NormalizeKeyword(s)
	if kw == "" {
		return "", fmt.Errorf("%w: keyword is empty after normalization", model.ErrInvalidKeyword)
	}
	if n := len([]rune(kw)); n > maxLen {
		return "", fmt.Errorf("%w: keyword length %d exceeds limit %d", model.ErrInvalidKeyword, n, maxLen)
	}
	return kw, nil
}

// shortHash 取 sha256 前 16 字节十六进制，用于缓存 key / 游标指纹。
// 截断只影响碰撞概率（2^64 量级），不用于安全场景。
func shortHash(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0x1f}) // 分隔符，避免 "ab"+"c" 与 "a"+"bc" 同哈希
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// QueryFingerprint 计算“同一查询”的身份指纹：关键词 + 全部筛选条件 + 排序 + 页大小。
// 结果用于：结果缓存 key、游标一致性校验（翻页时改条件应报错而不是串页）。
func QueryFingerprint(p SearchParams) string {
	parts := []string{
		"v1",
		p.Keyword,
		strings.Join(p.DocTypes, ","),
		strconv.FormatInt(int64(p.ZoneID), 10),
		strconv.FormatInt(int64(p.DurationMin), 10),
		strconv.FormatInt(int64(p.DurationMax), 10),
		strconv.FormatInt(p.PublishedAfter, 10),
		strconv.FormatInt(p.PublishedBefore, 10),
		encodeSortSpecs(p.SortFields),
		strconv.FormatInt(int64(p.Size), 10),
	}
	return shortHash(parts...)
}

// encodeSortSpecs 把排序规格编成稳定字符串（参与指纹计算）。
func encodeSortSpecs(fields []SortField) string {
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		dir := "asc"
		if f.Desc {
			dir = "desc"
		}
		parts = append(parts, f.Field+":"+dir)
	}
	return strings.Join(parts, "|")
}
