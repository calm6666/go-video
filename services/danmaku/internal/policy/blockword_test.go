package policy

import "testing"

// 绕过样本一律用 “反斜杠 + u” 转义书写：BOM(U+FEFF) 等零宽字符直接写进源码
// 会触发 Go 词法错误（illegal byte order mark），转义后语义同样清晰。
const (
	zwsp = "\u200B" // 零宽空格
	bom  = "\uFEFF" // 零宽 BOM
	mdot = "\u00B7" // 半角间隔号
	kdot = "\u30FB" // 片假名中点
)

// TestNormalize 校验归一化规则：全角折叠、大小写统一、
// 空白/零宽/填充标点丢弃；这些是屏蔽词绕过的主要手段。
func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"全角字母数字折叠", "ＡＢＣ１２３", "abc123"},
		{"大小写统一", "MiXeD CaSe", "mixedcase"},
		{"丢弃半角空格", "敏 感 词", "敏感词"},
		{"丢弃全角空格", "敏　感　词", "敏感词"},
		{"丢弃零宽字符", "敏" + zwsp + "感" + bom + "词", "敏感词"},
		{"丢弃分隔填充符", "敏" + mdot + "感-词。！", "敏感词"},
		{"全角标点折叠后丢弃", "敏ａｂ（感）", "敏ab感"},
		{"空串", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Normalize(c.in); got != c.want {
				t.Fatalf("Normalize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestBlockFilterMatch 校验基本包含匹配与大小写/全半角/零宽/标点变体命中。
func TestBlockFilterMatch(t *testing.T) {
	f := NewBlockFilter([]string{"敏感词", "spam"})

	cases := []struct {
		name string
		text string
		want string // 空表示不应命中
	}{
		{"原文命中", "这是一句敏感词而已", "敏感词"},
		{"大小写变体", "SPAM here", "spam"},
		{"全角变体", "ｓｐａｍ", "spam"},
		{"零宽字符插入绕过", "spa" + zwsp + "m", "spam"},
		{"标点填充绕过", "敏" + mdot + "感" + kdot + "词", "敏感词"},
		{"开头命中", "敏感词开头", "敏感词"},
		{"结尾命中", "结尾敏感词", "敏感词"},
		{"不命中", "正常弹幕内容", ""},
		{"空文本", "", ""},
		{"归一化后为空", "。。。", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := f.Match(c.text)
			if c.want == "" {
				if ok {
					t.Fatalf("Match(%q) 不应命中，实际命中 %q", c.text, got)
				}
				return
			}
			if !ok {
				t.Fatalf("Match(%q) 应命中 %q，实际未命中", c.text, c.want)
			}
			if got != c.want {
				t.Fatalf("Match(%q) 命中 %q, want %q", c.text, got, c.want)
			}
		})
	}
}

// TestBlockFilterPrefersLongestWord 同首字符的词条按长度降序尝试，
// 保证「敏感词」优先于「敏感」，便于日志与人工复核看到最具体的命中。
func TestBlockFilterPrefersLongestWord(t *testing.T) {
	f := NewBlockFilter([]string{"敏感", "敏感词"})
	got, ok := f.Match("这句话是敏感词汇")
	if !ok {
		t.Fatal("应命中屏蔽词")
	}
	if got != "敏感词" {
		t.Fatalf("应优先命中最长词条 敏感词，实际 %q", got)
	}
}

// TestBlockFilterIgnoresUnusableWords 空词与归一化后为空的词不入库，
// 否则任何弹幕都会被空模式命中。
func TestBlockFilterIgnoresUnusableWords(t *testing.T) {
	f := NewBlockFilter([]string{"", "   ", "。。。", mdot + "-", "有效"})
	if f.Len() != 1 {
		t.Fatalf("应只保留 1 个可用词条，实际 %d", f.Len())
	}
	if _, ok := f.Match("随便一句"); ok {
		t.Fatal("标点词条不得命中普通弹幕")
	}
	if _, ok := f.Match("这里有有效内容"); !ok {
		t.Fatal("可用词条应命中")
	}
}

// TestBlockFilterDedupesNormalizedVariants 全半角/大小写变体归一化后视为同一条目。
func TestBlockFilterDedupesNormalizedVariants(t *testing.T) {
	f := NewBlockFilter([]string{"abc", "ＡＢＣ", "ABC", "a b c"})
	if f.Len() != 1 {
		t.Fatalf("变体应去重为 1 条，实际 %d", f.Len())
	}
}

// TestNilBlockFilter 零值/nil 匹配器必须表现为「词库为空」，
// 让 Redis 词库缓存不可用时的行为可预期（不误伤正常弹幕）。
func TestNilBlockFilter(t *testing.T) {
	var f *BlockFilter
	if f.Len() != 0 {
		t.Fatalf("nil 匹配器 Len 应为 0，实际 %d", f.Len())
	}
	if _, ok := f.Match("任何内容"); ok {
		t.Fatal("nil 匹配器不应命中")
	}
	empty := NewBlockFilter(nil)
	if _, ok := empty.Match("任何内容"); ok {
		t.Fatal("空词库不应命中")
	}
}
