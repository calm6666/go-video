package esclient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// indexBodyMap 解析指定分词策略的请求体；结构断言统一走这个入口，避免每个用例重复解码。
func indexBodyMap(t *testing.T, schemaVersion string, a Analyzer) map[string]interface{} {
	t.Helper()
	raw, err := IndexBody(schemaVersion, a)
	if err != nil {
		t.Fatalf("IndexBody(%s, %+v) 失败: %v", schemaVersion, a, err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("索引请求体不是合法 JSON: %v", err)
	}
	return body
}

func analysisOf(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	settings, ok := body["settings"].(map[string]interface{})
	if !ok {
		t.Fatal("请求体缺少 settings")
	}
	analysis, ok := settings["analysis"].(map[string]interface{})
	if !ok {
		t.Fatal("settings 缺少 analysis")
	}
	return analysis
}

func analyzerOf(t *testing.T, body map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	analyzers, ok := analysisOf(t, body)["analyzer"].(map[string]interface{})
	if !ok {
		t.Fatal("analysis 缺少 analyzer")
	}
	a, ok := analyzers[name].(map[string]interface{})
	if !ok {
		t.Fatalf("analysis.analyzer 缺少 %s", name)
	}
	return a
}

func filtersOf(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	f, _ := analysisOf(t, body)["filter"].(map[string]interface{})
	return f
}

// TestIndexBodyDefaultAnalyzerNeedsNoPlugin 锁死默认策略：不配插件也必须能中文召回。
// 这条断言的意义在于「开箱可用」——本地/新集群第一次建索引不应该因为缺插件失败。
func TestIndexBodyDefaultAnalyzerNeedsNoPlugin(t *testing.T) {
	body := indexBodyMap(t, "", Analyzer{})

	for _, name := range []string{IndexAnalyzerName, SearchAnalyzerName} {
		a := analyzerOf(t, body, name)
		if a["tokenizer"] != "standard" {
			t.Errorf("%s tokenizer = %v, want standard", name, a["tokenizer"])
		}
		if !strings.Contains(string(mustJSON(t, a)), cjkFilterName) {
			t.Errorf("%s 未挂 cjk bigram filter，中文检索会退化成整串匹配: %s", name, mustJSON(t, a))
		}
	}
	if filtersOf(t, body)[cjkFilterName].(map[string]interface{})["type"] != "cjk" {
		t.Errorf("filter.%s 应为内置 cjk 类型", cjkFilterName)
	}
	if got := analyzerMeta(t, body)["analyzer"]; got != string(AnalyzerCJK) {
		t.Errorf("_meta.analyzer = %v, want %s", got, AnalyzerCJK)
	}
}

// TestIndexBodyTextFieldsReferenceAnalyzers 保证需要中文切分的字段都引用同一对分析器名：
// 查询侧（search-query 的 multi_match）不显式传 analyzer，全靠 mapping 决定查询分词。
func TestIndexBodyTextFieldsReferenceAnalyzers(t *testing.T) {
	props := mappingsProperties(t, indexBodyMap(t, "", DefaultAnalyzer()))
	for _, f := range []string{"title", "description", "author_name"} {
		m, ok := props[f].(map[string]interface{})
		if !ok {
			t.Fatalf("字段 %s 缺失", f)
		}
		if m["type"] != "text" || m["analyzer"] != IndexAnalyzerName || m["search_analyzer"] != SearchAnalyzerName {
			t.Errorf("字段 %s 未引用中文分析器: %s", f, mustJSON(t, m))
		}
	}
	// keyword 子字段用于精确匹配与排序，不能因为加了分词就被丢掉。
	for _, f := range []string{"title", "author_name"} {
		m := props[f].(map[string]interface{})
		fields, ok := m["fields"].(map[string]interface{})
		if !ok {
			t.Fatalf("字段 %s 缺少 keyword 子字段", f)
		}
		if _, ok := fields["keyword"].(map[string]interface{}); !ok {
			t.Errorf("字段 %s 缺少 fields.keyword", f)
		}
	}
}

// TestIndexBodyAnalyzerKinds 逐个校验四种策略的 tokenizer 组合。
// ik/smartcn 依赖集群插件，因此必须能在 _meta 里看清用了哪一族，排障时不必反推索引名。
func TestIndexBodyAnalyzerKinds(t *testing.T) {
	cases := []struct {
		kind          AnalyzerKind
		wantIndexTok  string
		wantSearchTok string
	}{
		{AnalyzerStandard, "standard", "standard"},
		{AnalyzerCJK, "standard", "standard"},
		{AnalyzerIK, "ik_max_word", "ik_smart"},
		{AnalyzerSmartCN, "smartcn", "smartcn"},
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			body := indexBodyMap(t, "v1", Analyzer{Kind: c.kind})
			if got := analyzerOf(t, body, IndexAnalyzerName)["tokenizer"]; got != c.wantIndexTok {
				t.Errorf("写入 tokenizer = %v, want %s", got, c.wantIndexTok)
			}
			if got := analyzerOf(t, body, SearchAnalyzerName)["tokenizer"]; got != c.wantSearchTok {
				t.Errorf("查询 tokenizer = %v, want %s", got, c.wantSearchTok)
			}
			if got := analyzerMeta(t, body)["analyzer"]; got != string(c.kind) {
				t.Errorf("_meta.analyzer = %v, want %s", got, c.kind)
			}
			hasCJK := strings.Contains(string(mustJSON(t, filtersOf(t, body))), `"type":"cjk"`)
			if got := c.kind == AnalyzerCJK; got != hasCJK {
				t.Errorf("cjk filter 存在性 = %v, want %v", hasCJK, got)
			}
		})
	}
}

// TestIndexBodyStopwordsAndSynonymsPlacement 固定词典挂载位置：
// 停用词改变索引内容所以挂在写入侧；同义词只挂查询侧且必须 updateable，
// 否则集群会拒绝 file-based 同义词，或要求为改一行同义词就重建整个索引。
func TestIndexBodyStopwordsAndSynonymsPlacement(t *testing.T) {
	body := indexBodyMap(t, "v1", Analyzer{
		Kind:          AnalyzerCJK,
		StopwordsPath: "/usr/share/opensearch/config/analysis/go_video_stopwords.txt",
		SynonymsPath:  "/usr/share/opensearch/config/analysis/go_video_synonyms.txt",
	})

	filters := filtersOf(t, body)
	stop, ok := filters[stopwordFilterName].(map[string]interface{})
	if !ok {
		t.Fatalf("缺少停用词 filter: %s", mustJSON(t, filters))
	}
	if stop["type"] != "stop" || !strings.HasSuffix(stop["stopwords_path"].(string), "go_video_stopwords.txt") {
		t.Errorf("停用词 filter 配置异常: %s", mustJSON(t, stop))
	}
	syn, ok := filters[synonymFilterName].(map[string]interface{})
	if !ok {
		t.Fatalf("缺少同义词 filter: %s", mustJSON(t, filters))
	}
	if syn["type"] != "synonym_graph" || syn["updateable"] != true {
		t.Errorf("同义词 filter 必须是 updateable 的 synonym_graph: %s", mustJSON(t, syn))
	}

	idxChain := filterChain(t, analyzerOf(t, body, IndexAnalyzerName))
	searchChain := filterChain(t, analyzerOf(t, body, SearchAnalyzerName))
	for _, want := range []string{"lowercase", cjkFilterName, stopwordFilterName} {
		if !containsStr(idxChain, want) {
			t.Errorf("写入分析器 filter 链缺少 %s，实际 %v", want, idxChain)
		}
	}
	if containsStr(idxChain, synonymFilterName) {
		t.Errorf("同义词不能挂在写入分析器（会让改词典必须重建索引）: %v", idxChain)
	}
	if !containsStr(searchChain, synonymFilterName) {
		t.Errorf("查询分析器缺少同义词 filter: %v", searchChain)
	}
}

// TestIndexBodyNoDictionariesOmitsFilters 未配置词典时不能留下悬空引用，
// 否则集群会因为找不到文件直接拒绝建索引。
func TestIndexBodyNoDictionariesOmitsFilters(t *testing.T) {
	body := indexBodyMap(t, "v1", Analyzer{Kind: AnalyzerStandard})
	if filtersOf(t, body) != nil {
		t.Errorf("未配置词典时不应生成 filter 段: %s", mustJSON(t, filtersOf(t, body)))
	}
	for _, name := range []string{IndexAnalyzerName, SearchAnalyzerName} {
		if got := filterChain(t, analyzerOf(t, body, name)); !reflect.DeepEqual(got, []string{"lowercase"}) {
			t.Errorf("%s filter 链 = %v, want [lowercase]", name, got)
		}
	}
	meta := analyzerMeta(t, body)
	if _, ok := meta["stopwords_path"]; ok {
		t.Errorf("_meta 不应记录未配置的 stopwords_path: %s", mustJSON(t, meta))
	}
}

// TestIndexBodyRejectsUnknownAnalyzerKind 非法分词族必须报错，不能悄悄退回默认值：
// 配置写错却建出一个「看起来正常」的索引，后续只会以召回异常的形式暴露。
func TestIndexBodyRejectsUnknownAnalyzerKind(t *testing.T) {
	if _, err := IndexBody("v1", Analyzer{Kind: "jieba"}); err == nil {
		t.Fatal("未知 Analyzer.Kind 应返回错误")
	} else if !strings.Contains(err.Error(), "jieba") {
		t.Errorf("错误信息应包含非法取值: %v", err)
	}
	_, err := New(Options{Endpoints: []string{"http://127.0.0.1:9200"}, Password: "p", Analyzer: Analyzer{Kind: "pk"}})
	if err == nil || !strings.Contains(err.Error(), "pk") {
		t.Errorf("New 应在启动阶段拒绝非法分词配置，实际 err=%v", err)
	}
}

// TestNewNormalizesAnalyzerKind 空 Kind 归一化为 cjk，避免进程与脚本对同一份配置
// 生成不同的 _meta.analyzer 值。
func TestNewNormalizesAnalyzerKind(t *testing.T) {
	c, err := New(Options{Endpoints: []string{"http://127.0.0.1:9200"}, Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	hc, ok := c.(*httpClient)
	if !ok {
		t.Fatalf("New 返回了未知实现 %T", c)
	}
	if hc.opts.Analyzer.Kind != AnalyzerCJK {
		t.Errorf("空 Kind 应归一化为 %s，实际 %s", AnalyzerCJK, hc.opts.Analyzer.Kind)
	}
}

func analyzerMeta(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	mappings, ok := body["mappings"].(map[string]interface{})
	if !ok {
		t.Fatal("请求体缺少 mappings")
	}
	meta, ok := mappings["_meta"].(map[string]interface{})
	if !ok {
		t.Fatal("mappings 缺少 _meta")
	}
	return meta
}

func filterChain(t *testing.T, analyzer map[string]interface{}) []string {
	t.Helper()
	raw, ok := analyzer["filter"].([]interface{})
	if !ok {
		t.Fatalf("analyzer 缺少 filter 链: %s", mustJSON(t, analyzer))
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("filter 链含非字符串元素: %v", v)
		}
		out = append(out, s)
	}
	return out
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
