package policy

import (
	"sort"
	"strings"
	"unicode"
)

// 屏蔽词归一化规则：
//  1. 全角字母/数字/标点折叠为半角（FF01~FF5E → 0021~007E），全角空格丢弃；
//  2. 统一小写，规避 ABC / abc 变体；
//  3. 丢弃空白、零宽字符与各类分隔填充标点（含全角折叠后的 ASCII 标点），
//     规避「敏・感・词」「abc!!!」式绕过。
//
// 归一化只用于匹配判定，落库 content 保持原文，不改写用户输入。
// 简繁转换未接入（common/chinese 需要外置 OpenCC 字典文件），见 README 缺口。
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if folded, ok := foldFullWidth(r); ok {
			r = folded
		}
		if isIgnorable(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// foldFullWidth 把全角可打印字符折叠为半角等价字符。
func foldFullWidth(r rune) (rune, bool) {
	switch {
	case r >= 0xFF01 && r <= 0xFF5E:
		return r - 0xFEE0, true
	default:
		return r, false
	}
}

// isIgnorable 判断字符是否在匹配时被丢弃。
func isIgnorable(r rune) bool {
	if r == 0xFEFF || r == 0x200B || r == 0x200C || r == 0x200D {
		return true // BOM 与零宽字符
	}
	if unicode.IsSpace(r) || r == 0x3000 {
		return true
	}
	switch r {
	case '*', '.', ',', ';', ':', '-', '_', '~', '·', '・', '•', '|', '\\', '/', '"', '\'',
		'、', '。', '，', '；', '：', '！', '？', '（', '）', '(', ')', '[', ']', '【', '】', '<', '>',
		'!', '?', '#', '$', '%', '&', '@', '+', '=', '^', '{', '}':
		return true // 常见分隔填充符，含半角间隔号、片假名中点与 ASCII 标点（全角折叠后落到这一组）
	default:
		return false
	}
}

// pattern 是一条已归一化的屏蔽词。
type pattern struct {
	raw   string
	runes []rune
}

// BlockFilter 是发送侧/读取侧共用的屏蔽词匹配器。
// 按首字符分桶后做前缀比对：词表规模在千级时足以支撑弹幕写入 QPS，
// 若词表膨胀到万级应替换为 AC 自动机（README 已记为后续工作）。
type BlockFilter struct {
	buckets map[rune][]pattern
	size    int
}

// NewBlockFilter 用词表构造匹配器；空词与归一化后为空的词被忽略。
func NewBlockFilter(words []string) *BlockFilter {
	f := &BlockFilter{buckets: make(map[rune][]pattern)}
	for _, w := range words {
		nw := Normalize(w)
		if nw == "" {
			continue
		}
		rs := []rune(nw)
		head := rs[0]
		dup := false
		for _, p := range f.buckets[head] {
			if p.raw == nw {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		f.buckets[head] = append(f.buckets[head], pattern{raw: nw, runes: rs})
		f.size++
	}
	// 同首字符的词条按长度降序，优先命中更具体的词。
	for head := range f.buckets {
		sort.SliceStable(f.buckets[head], func(i, j int) bool {
			return len(f.buckets[head][i].runes) > len(f.buckets[head][j].runes)
		})
	}
	return f
}

// Len 返回生效词条数。
func (f *BlockFilter) Len() int {
	if f == nil {
		return 0
	}
	return f.size
}

// Match 返回文本命中的第一个屏蔽词（已归一化形式）。
func (f *BlockFilter) Match(text string) (string, bool) {
	if f == nil || f.size == 0 {
		return "", false
	}
	rs := []rune(Normalize(text))
	if len(rs) == 0 {
		return "", false
	}
	for i := range rs {
		cands := f.buckets[rs[i]]
		if len(cands) == 0 {
			continue
		}
		for _, p := range cands {
			if matchAt(rs, p.runes, i) {
				return p.raw, true
			}
		}
	}
	return "", false
}

// matchAt 判断 word 是否从 text 的 at 位置开始整体匹配。
func matchAt(text, word []rune, at int) bool {
	if at+len(word) > len(text) {
		return false
	}
	for i, w := range word {
		if text[at+i] != w {
			return false
		}
	}
	return true
}
