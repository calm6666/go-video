package chinese

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

// resetSingletonsForTest 重置包级单例，仅供本包测试使用。
func resetSingletonsForTest() {
	simplifiedOnce = sync.Once{}
	simplifiedConv = nil
	simplifiedErr = nil
	traditionalOnce = sync.Once{}
	traditionalConv = nil
	traditionalErr = nil
}

// memoryLoader 把字典文件名映射到内存内容。
type memoryLoader struct {
	data map[string]string
}

func (m *memoryLoader) load(file string) ([]byte, error) {
	if v, ok := m.data[file]; ok {
		return []byte(v), nil
	}
	return nil, errNotFound(file)
}

type notFoundError struct{ file string }

func (e *notFoundError) Error() string { return "not found: " + e.file }

func errNotFound(file string) error { return &notFoundError{file: file} }

// tinyDictData 提供 s2t/t2s/s2twp/tw2sp 等方案所需的最小字典数据。
// 每行格式：key<TAB>value。
func tinyDictData() map[string]string {
	return map[string]string{
		"STCharacters.txt":         "国\t國\n简\t簡\n体\t體\n",
		"STPhrases.txt":            "计算机\t計電腦\n",
		"TSCharacters.txt":         "國\t国\n簡\t简\n體\t体\n",
		"TSPhrases.txt":            "計電腦\t计算机\n",
		"TWPhrases.txt":            "",
		"TWVariants.txt":           "",
		"TWPhrasesRev.txt":         "",
		"TWVariantsRev.txt":        "",
		"TWVariantsRevPhrases.txt": "",
		"HKVariants.txt":           "",
		"HKVariantsPhrases.txt":    "",
		"HKVariantsRev.txt":        "",
		"HKVariantsRevPhrases.txt": "",
	}
}

// withLoader 安装指定 loader 并返回还原函数。
// 调用方必须 defer 调用还原函数，避免污染其他测试。
func withLoader(fn func(file string) ([]byte, error)) func() {
	original := loaderPtr.Load().(func(string) ([]byte, error))
	SetDictLoader(fn)
	return func() { SetDictLoader(original) }
}

// withTinyLoader 安装 tiny 字典加载器并返回还原函数。
func withTinyLoader() func() {
	ml := &memoryLoader{data: tinyDictData()}
	return withLoader(ml.load)
}

// TestNewConverterScheme 验证按方案名构造 Converter 成功且字段被填充。
func TestNewConverterScheme(t *testing.T) {
	restore := withTinyLoader()
	defer restore()
	cases := []string{"s2t", "t2s", "s2twp", "tw2sp", "s2tw", "tw2s", "s2hk", "hk2s", "t2tw", "t2hk"}
	for _, scheme := range cases {
		c, err := NewConverter(scheme)
		if err != nil {
			t.Errorf("NewConverter(%q) error: %v", scheme, err)
			continue
		}
		if c == nil {
			t.Errorf("NewConverter(%q) returned nil", scheme)
			continue
		}
		if len(c.DictGroup) == 0 {
			t.Errorf("NewConverter(%q) DictGroup empty", scheme)
		}
		if c.Description() == "" {
			t.Errorf("NewConverter(%q) Description empty", scheme)
		}
	}
}

// TestNewConverterUnknownScheme 验证未知方案且非文件路径返回错误。
func TestNewConverterUnknownScheme(t *testing.T) {
	restore := withTinyLoader()
	defer restore()
	_, err := NewConverter("no-such-scheme-and-not-a-file.json")
	if err == nil {
		t.Fatal("expect error, got nil")
	}
}

// TestNewConverterCustomConfig 验证按本地 JSON 配置路径构造 Converter。
func TestNewConverterCustomConfig(t *testing.T) {
	customCfg := `{
		"name": "test custom config",
		"conversion_chain": [{
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "STCharacters.txt"
				}]
			}
		}]
	}`
	called := false
	ml := &memoryLoader{data: tinyDictData()}
	restore := withLoader(func(file string) ([]byte, error) {
		if file == "custom.json" {
			called = true
			return []byte(customCfg), nil
		}
		return ml.load(file)
	})
	defer restore()
	c, err := NewConverter("custom.json")
	if err != nil {
		t.Fatalf("NewConverter custom: %v", err)
	}
	if !called {
		t.Fatal("custom config not loaded")
	}
	if c.Description() != "test custom config" {
		t.Errorf("description = %q, want %q", c.Description(), "test custom config")
	}
}

// TestConvertS2T 验证 s2t 方案把 "中国" 转为 "中國"。
func TestConvertS2T(t *testing.T) {
	restore := withTinyLoader()
	defer restore()
	c, err := NewConverter("s2t")
	if err != nil {
		t.Fatalf("NewConverter(s2t): %v", err)
	}
	if got := c.Convert("中国"); got != "中國" {
		t.Errorf("s2t Convert(中国) = %q, want %q", got, "中國")
	}
}

// TestConvertS2TPhrase 验证短语字典优先于字符字典的匹配。
func TestConvertS2TPhrase(t *testing.T) {
	restore := withTinyLoader()
	defer restore()
	c, err := NewConverter("s2t")
	if err != nil {
		t.Fatalf("NewConverter(s2t): %v", err)
	}
	if got := c.Convert("计算机"); got != "計電腦" {
		t.Errorf("s2t Convert(计算机) = %q, want %q", got, "計電腦")
	}
}

// TestConvertT2S 验证 t2s 方案把 "中國" 转为 "中国"。
func TestConvertT2S(t *testing.T) {
	restore := withTinyLoader()
	defer restore()
	c, err := NewConverter("t2s")
	if err != nil {
		t.Fatalf("NewConverter(t2s): %v", err)
	}
	if got := c.Convert("中國"); got != "中国" {
		t.Errorf("t2s Convert(中國) = %q, want %q", got, "中国")
	}
}

// TestConvertEmptyInput 验证空字符串输入返回空字符串。
func TestConvertEmptyInput(t *testing.T) {
	restore := withTinyLoader()
	defer restore()
	c, err := NewConverter("s2t")
	if err != nil {
		t.Fatalf("NewConverter(s2t): %v", err)
	}
	if got := c.Convert(""); got != "" {
		t.Errorf("Convert(\"\") = %q, want empty", got)
	}
}

// TestConvertNoMatch 验证字典无匹配时字符原样透传。
func TestConvertNoMatch(t *testing.T) {
	customCfg := `{
		"name": "empty dict",
		"conversion_chain": [{
			"dict": {
				"type": "txt",
				"file": "empty.txt"
			}
		}]
	}`
	restore := withLoader(func(file string) ([]byte, error) {
		if file == "custom-empty.json" {
			return []byte(customCfg), nil
		}
		if file == "empty.txt" {
			return []byte(""), nil
		}
		return nil, errNotFound(file)
	})
	defer restore()
	c, err := NewConverter("custom-empty.json")
	if err != nil {
		t.Fatalf("NewConverter: %v", err)
	}
	in := "abc中文"
	if got := c.Convert(in); got != in {
		t.Errorf("Convert(%q) = %q, want %q", in, got, in)
	}
}

// TestToTraditionalSimplified 验证包级单例 ToSimplified/ToTraditional。
// 单例仅在首次调用时构造，依赖当时 Loader 提供的字典数据。
func TestToTraditionalSimplified(t *testing.T) {
	resetSingletonsForTest()
	restore := withTinyLoader()
	defer restore()
	got := ToTraditional("中国")
	if got != "中國" {
		t.Errorf("ToTraditional(中国) = %q, want %q", got, "中國")
	}
	got = ToSimplified("中國")
	if got != "中国" {
		t.Errorf("ToSimplified(中國) = %q, want %q", got, "中国")
	}
}

// TestToTraditionalFallback 验证 Loader 不可用时单例回退返回原字符串。
func TestToTraditionalFallback(t *testing.T) {
	resetSingletonsForTest()
	restore := withLoader(func(file string) ([]byte, error) {
		return nil, errNotFound(file)
	})
	defer restore()
	in := "中国"
	if got := ToTraditional(in); got != in {
		t.Errorf("ToTraditional with broken loader = %q, want %q", got, in)
	}
	resetSingletonsForTest()
	in2 := "中國"
	if got := ToSimplified(in2); got != in2 {
		t.Errorf("ToSimplified with broken loader = %q, want %q", got, in2)
	}
}

// TestPrefixMatchEmptyTrie 验证 Trie 为 nil 时 prefixMatch 返回错误。
func TestPrefixMatchEmptyTrie(t *testing.T) {
	d := &dict{Trie: nil}
	if _, err := d.prefixMatch("any"); err == nil {
		t.Fatal("expect error when Trie is nil")
	}
}

// TestSchemesList 验证所有内置方案常量已注册到 schemes。
func TestSchemesList(t *testing.T) {
	want := []string{"hk2s", "s2hk", "s2t", "s2tw", "s2twp", "t2hk", "t2s", "t2tw", "tw2s", "tw2sp"}
	for _, s := range want {
		if _, ok := schemes[s]; !ok {
			t.Errorf("scheme %q not registered", s)
			continue
		}
		if !strings.Contains(schemes[s], "conversion_chain") {
			t.Errorf("scheme %q config missing conversion_chain", s)
		}
	}
}

// TestGroupString 验证 Group.String 输出文件列表。
func TestGroupString(t *testing.T) {
	g := &Group{Files: []string{"a.txt", "b.txt"}}
	s := g.String()
	if !strings.Contains(s, "a.txt") || !strings.Contains(s, "b.txt") {
		t.Errorf("Group.String() = %q, want files listed", s)
	}
}

// TestConvertIdempotent 验证 Convert 多次调用返回相同结果，确保无内部状态。
func TestConvertIdempotent(t *testing.T) {
	restore := withTinyLoader()
	defer restore()
	c, err := NewConverter("s2t")
	if err != nil {
		t.Fatalf("NewConverter: %v", err)
	}
	first := c.Convert("中国简体")
	second := c.Convert("中国简体")
	if first != second {
		t.Errorf("Convert not idempotent: first=%q second=%q", first, second)
	}
}

// TestBuildFromFileFormat 验证 buildFromFile 处理 TAB 分隔的多列与单列回退。
func TestBuildFromFileFormat(t *testing.T) {
	restore := withLoader(func(file string) ([]byte, error) {
		if file != "fmt.txt" {
			return nil, errNotFound(file)
		}
		// 第 1 行：单 TAB 分隔，value 单列 -> Fields 切分
		// 第 2 行：多 TAB 分隔，多列值
		// 第 3 行：少于 2 列，跳过
		return []byte("a\tA1\nb\tB1\tB2\nbadline\n"), nil
	})
	defer restore()
	d, err := buildFromFile("fmt.txt")
	if err != nil {
		t.Fatalf("buildFromFile: %v", err)
	}
	if d.Trie == nil {
		t.Fatal("Trie is nil")
	}
	if len(d.Values) != 2 {
		t.Fatalf("Values len = %d, want 2", len(d.Values))
	}
	if !reflect.DeepEqual(d.Values[0], []string{"A1"}) {
		t.Errorf("Values[0] = %v, want [A1]", d.Values[0])
	}
	if !reflect.DeepEqual(d.Values[1], []string{"B1", "B2"}) {
		t.Errorf("Values[1] = %v, want [B1 B2]", d.Values[1])
	}
}

// TestSetDictRootEmpty 验证 SetDictRoot 传入空字符串时回退到默认 opencc。
func TestSetDictRootEmpty(t *testing.T) {
	SetDictRoot("")
	if got := dictRootPtr.Load().(string); got != "opencc" {
		t.Errorf("SetDictRoot(\"\") = %q, want %q", got, "opencc")
	}
}
