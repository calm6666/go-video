package repository

import (
	"encoding/json"
	"fmt"
	"testing"

	"go-video/services/search-query/model"
)

// DSL 构造测试：不连接任何引擎，只断言请求体结构与安全性。

func dslOpts() DSLOptions {
	return DSLOptions{
		MatchFields:     DefaultMatchFields,
		HighlightFields: DefaultHighlightFields,
		PreTag:          "<em>",
		PostTag:         "</em>",
		TrackTotalCap:   10000,
		SourceFields:    DefaultSourceFields,
		EngineTimeout:   "1200ms",
	}
}

// decodeBody 解析请求体（失败即 fatal）。
func decodeBody(t *testing.T, bs []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(bs, &body); err != nil {
		t.Fatalf("body is not valid json: %v\n%s", err, string(bs))
	}
	return body
}

// boolOf 取出 query.bool 子句。
func boolOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	q, ok := body["query"].(map[string]any)
	if !ok {
		t.Fatalf("missing query object: %v", body["query"])
	}
	b, ok := q["bool"].(map[string]any)
	if !ok {
		t.Fatalf("missing query.bool: %v", q)
	}
	return b
}

// filtersOf 取出 filter 数组。
func filtersOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	f, ok := boolOf(t, body)["filter"]
	if !ok {
		return nil
	}
	arr, ok := f.([]any)
	if !ok {
		t.Fatalf("filter is not an array: %T", f)
	}
	return arr
}

// findFilter 在 filter 数组里找第一个包含 key 的子句（如 terms/term/range）。
func findFilter(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	for _, f := range filtersOf(t, body) {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := m[key]; ok {
			inner, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("filter %s payload is not an object: %T", key, v)
			}
			return inner
		}
	}
	return nil
}

// rangeFor 取出指定字段的 range 子句。
// 每个字段是独立的 {"range": {field: {...}}} 子句（filter 语境下等价且更易读），
// 所以必须遍历所有 range 子句，而不是只看第一个。
func rangeFor(t *testing.T, body map[string]any, field string) map[string]any {
	t.Helper()
	for _, f := range filtersOf(t, body) {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		rng, ok := m["range"].(map[string]any)
		if !ok {
			continue
		}
		if clause, ok := rng[field].(map[string]any); ok {
			return clause
		}
	}
	return nil
}

func TestBuildSearchBodyRequiresKeyword(t *testing.T) {
	if _, err := BuildSearchBody(SearchParams{Size: 10}, dslOpts()); err != model.ErrInvalidKeyword {
		t.Fatalf("empty keyword err = %v, want ErrInvalidKeyword", err)
	}
	if _, err := BuildSearchBody(SearchParams{Keyword: "x", Size: 0}, dslOpts()); err == nil {
		t.Fatal("size=0 must be rejected instead of falling back to engine default")
	}
	if _, err := BuildSearchBody(SearchParams{Keyword: "x", Size: 10, From: -1}, dslOpts()); err == nil {
		t.Fatal("negative from must be rejected")
	}
}

func TestBuildSearchBodyPagingAndProjection(t *testing.T) {
	bs, err := BuildSearchBody(SearchParams{Keyword: "原神", Size: 30, From: 60}, dslOpts())
	if err != nil {
		t.Fatalf("BuildSearchBody: %v", err)
	}
	body := decodeBody(t, bs)
	if int64(body["from"].(float64)) != 60 {
		t.Errorf("from = %v, want 60", body["from"])
	}
	if int32(body["size"].(float64)) != 30 {
		t.Errorf("size = %v, want 30", body["size"])
	}
	if int64(body["track_total_hits"].(float64)) != 10000 {
		t.Errorf("track_total_hits = %v, want engine window cap 10000", body["track_total_hits"])
	}
	if body["timeout"] != "1200ms" {
		t.Errorf("timeout = %v, want 1200ms", body["timeout"])
	}
	src, ok := body["_source"].([]any)
	if !ok || len(src) != len(DefaultSourceFields) {
		t.Fatalf("_source = %v, want the projection field list", body["_source"])
	}
	if src[0] != FieldDocType {
		t.Errorf("_source[0] = %v, want %s", src[0], FieldDocType)
	}
}

func TestBuildSearchBodyFilters(t *testing.T) {
	bs, err := BuildSearchBody(SearchParams{
		Keyword:         "测试",
		DocTypes:        []string{model.DocTypeVideo, model.DocTypePGC},
		ZoneID:          7,
		DurationMin:     60,
		DurationMax:     600,
		PublishedAfter:  1700000000,
		PublishedBefore: 1700003600,
		Size:            20,
	}, dslOpts())
	if err != nil {
		t.Fatalf("BuildSearchBody: %v", err)
	}
	body := decodeBody(t, bs)

	terms := findFilter(t, body, "terms")
	if terms == nil {
		t.Fatal("missing terms filter for doc_type")
	}
	dt, ok := terms[FieldDocType].([]any)
	if !ok || len(dt) != 2 || dt[0] != model.DocTypeVideo || dt[1] != model.DocTypePGC {
		t.Fatalf("doc_type terms = %v, want [video pgc]", terms[FieldDocType])
	}

	term := findFilter(t, body, "term")
	if term == nil || term[FieldZoneID] == nil {
		t.Fatalf("missing term filter for zone, filters=%v", filtersOf(t, body))
	}

	rng := findFilter(t, body, "range")
	if rng == nil {
		t.Fatal("missing range filters")
	}
	dur := rangeFor(t, body, FieldDuration)
	if dur == nil || dur["gte"] == nil || dur["lt"] == nil {
		t.Fatalf("duration range = %v, want gte+lt (upper bound exclusive)", rng[FieldDuration])
	}
	if dur["lte"] != nil {
		t.Errorf("duration upper bound must stay exclusive (lt), got %v", dur)
	}
	pub := rangeFor(t, body, FieldPubTime)
	if pub == nil || pub["gte"] == nil || pub["lte"] == nil {
		t.Fatalf("pub_time range = %v, want gte+lte (inclusive upper)", pub)
	}
	if pub["lt"] != nil {
		t.Errorf("pub_time upper bound must be inclusive (lte), got %v", pub)
	}
	if pub["gte"] != float64(1700000000) || pub["lte"] != float64(1700003600) {
		t.Errorf("pub_time bounds = %v, want the request's unix seconds verbatim", pub)
	}
	if dur["gte"] != float64(60) || dur["lt"] != float64(600) {
		t.Errorf("duration bounds = %v, want 60/600", dur)
	}
}

func TestBuildSearchBodyOmitsEmptyFilters(t *testing.T) {
	bs, err := BuildSearchBody(SearchParams{Keyword: "only keyword", Size: 10}, dslOpts())
	if err != nil {
		t.Fatalf("BuildSearchBody: %v", err)
	}
	if f := filtersOf(t, decodeBody(t, bs)); len(f) != 0 {
		t.Fatalf("filters must be absent when no narrowing condition, got %v", f)
	}
	must, ok := boolOf(t, decodeBody(t, bs))["must"].([]any)
	if !ok || len(must) != 1 {
		t.Fatalf("must clause = %v, want exactly the multi_match", boolOf(t, decodeBody(t, bs))["must"])
	}
	mm, ok := must[0].(map[string]any)["multi_match"].(map[string]any)
	if !ok {
		t.Fatalf("must[0] is not multi_match: %v", must[0])
	}
	if mm["query"] != "only keyword" {
		t.Errorf("multi_match.query = %v", mm["query"])
	}
	if mm["type"] != "best_fields" {
		t.Errorf("multi_match.type = %v, want best_fields", mm["type"])
	}
	fields, ok := mm["fields"].([]any)
	if !ok || len(fields) != len(DefaultMatchFields) {
		t.Fatalf("multi_match.fields = %v", mm["fields"])
	}
	if fields[0] != FieldTitle+"^3" {
		t.Errorf("title must carry the highest boost, got %v", fields[0])
	}
}

// bannedClauseKeys 是“值即语法”或可携带原生查询的子句名：
// 只要它们作为 JSON 结构里的 key 出现，就说明用户输入被拼进了 DSL。
var bannedClauseKeys = []string{
	"query_string", "simple_query_string", "match_all", "wrapper",
	"script", "script_fields", "regexp", "fuzzy", "span_first",
}

// collectKeys 递归收集请求体里所有 JSON 对象的 key（结构检查，不用子串匹配）。
func collectKeys(v any, out map[string]int) {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			out[k]++
			collectKeys(child, out)
		}
	case []any:
		for _, child := range node {
			collectKeys(child, out)
		}
	}
}

// collectStringPaths 递归找出等于 want 的字符串值所在的位置（JSON path）。
func collectStringPaths(v any, want, path string, out *[]string) {
	switch node := v.(type) {
	case string:
		if node == want {
			*out = append(*out, path)
		}
	case map[string]any:
		for k, child := range node {
			collectStringPaths(child, want, path+"."+k, out)
		}
	case []any:
		for i, child := range node {
			collectStringPaths(child, want, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// TestBuildSearchBodyNoQueryStringInjection 是防注入的核心断言：
// 关键词里的引号/花括号/Lucene 操作符只能作为字符串值出现，
// 不能变成额外子句，也不能让请求体失去合法性。
//
// 注意：关键词原文一定会出现在请求体里（multi_match.query 的字符串值，JSON 已转义），
// 所以“正文含关键词”不是注入；注入的判据是它变成了 DSL 结构。
// 因此这里用反序列化后的结构检查，而不是子串匹配。
func TestBuildSearchBodyNoQueryStringInjection(t *testing.T) {
	hostile := `{"bool":{"must":{"match_all":{}}}} && title:* !() ~* ?:`
	bs, err := BuildSearchBody(SearchParams{Keyword: hostile, DocTypes: []string{model.DocTypeVideo}, Size: 10}, dslOpts())
	if err != nil {
		t.Fatalf("BuildSearchBody: %v", err)
	}
	// 能解析成合法 JSON = 用户没能闭合引号把请求体写坏。
	body := decodeBody(t, bs)

	// 1) 结构里不允许出现任何“值即语法”或可携带原生查询的子句 key。
	keys := map[string]int{}
	collectKeys(body, keys)
	for _, banned := range bannedClauseKeys {
		if n := keys[banned]; n != 0 {
			t.Fatalf("injected clause %q reached the DSL as a structural key (%d times): %s", banned, n, bs)
		}
	}

	// 2) 关键词只能落在 multi_match.query 的字符串值位置，且逐字保留。
	var paths []string
	collectStringPaths(body, hostile, "", &paths)
	if len(paths) != 1 {
		t.Fatalf("keyword must appear exactly once as a string value, got paths %v in %s", paths, bs)
	}
	if paths[0] != ".query.bool.must[0].multi_match.query" {
		t.Fatalf("keyword value path = %s, want only the multi_match query value slot", paths[0])
	}

	// 3) bool 子句仍只有 must(+filter)，没有被注入出 should/must_not/must 多条目。
	bq := boolOf(t, body)
	if len(bq) != 2 {
		t.Fatalf("bool clause keys = %v, want only must+filter", bq)
	}
	must, ok := bq["must"].([]any)
	if !ok || len(must) != 1 {
		t.Fatalf("must clause = %v, want exactly the multi_match", bq["must"])
	}
	if _, isMM := must[0].(map[string]any)["multi_match"]; !isMM {
		t.Fatalf("must[0] changed type: %v", must[0])
	}
	if mm := must[0].(map[string]any)["multi_match"].(map[string]any); mm["query"] != hostile {
		t.Errorf("keyword must survive verbatim as a string value, got %v", mm["query"])
	}

	// 4) filter 只能是白名单子句（terms/term/range），注入的 terms/match 之类不得混入。
	for i, f := range filtersOf(t, body) {
		m, ok := f.(map[string]any)
		if !ok {
			t.Fatalf("filter[%d] is not an object: %v", i, f)
		}
		for k := range m {
			if k != "terms" && k != "term" && k != "range" {
				t.Fatalf("unexpected filter clause %q at index %d: %v", k, i, m)
			}
		}
	}
}

func TestBuildSearchBodySortAndHighlight(t *testing.T) {
	bs, err := BuildSearchBody(SearchParams{
		Keyword: "x",
		Size:    10,
		SortFields: []SortField{
			{Field: "_score", Desc: true},
			{Field: FieldViewCount, Desc: true},
		},
	}, dslOpts())
	if err != nil {
		t.Fatalf("BuildSearchBody: %v", err)
	}
	body := decodeBody(t, bs)

	sortArr, ok := body["sort"].([]any)
	if !ok || len(sortArr) != 2 {
		t.Fatalf("sort = %v, want 2 clauses", body["sort"])
	}
	first := sortArr[0].(map[string]any)
	if sc, ok := first["_score"].(map[string]any); !ok || sc["order"] != "desc" {
		t.Errorf("_score sort = %v", first)
	} else if _, hasMissing := sc["missing"]; hasMissing {
		t.Errorf("_score sort must not carry missing:_last: %v", sc)
	}
	second := sortArr[1].(map[string]any)[FieldViewCount].(map[string]any)
	if second["order"] != "desc" || second["missing"] != "_last" {
		t.Errorf("view_count sort clause = %v, want desc + missing:_last", second)
	}

	hl, ok := body["highlight"].(map[string]any)
	if !ok {
		t.Fatal("missing highlight block")
	}
	if got := hl["pre_tags"].([]any)[0]; got != "<em>" {
		t.Errorf("pre_tags = %v", hl["pre_tags"])
	}
	if got := hl["post_tags"].([]any)[0]; got != "</em>" {
		t.Errorf("post_tags = %v", hl["post_tags"])
	}
	hf := hl["fields"].(map[string]any)
	if _, ok := hf[FieldTitle].(map[string]any); !ok {
		t.Errorf("title highlight config missing: %v", hf)
	}
	if n := hf[FieldTitle].(map[string]any)["number_of_fragments"]; n != float64(0) {
		t.Errorf("title number_of_fragments = %v, want 0 (whole title, single fragment)", n)
	}
	intro := hf[FieldIntro].(map[string]any)
	if intro["number_of_fragments"] != float64(2) || intro["fragment_size"] != float64(120) {
		t.Errorf("intro highlight config = %v", intro)
	}
}

// TestBuildSearchBodyHighlightTagsFromConfig 确认高亮标签由配置驱动
// （服务端不写死渲染，只返回标记文本）。
func TestBuildSearchBodyHighlightTagsFromConfig(t *testing.T) {
	opt := dslOpts()
	opt.PreTag, opt.PostTag = "[", "]"
	opt.HighlightFields = nil
	opt.TrackTotalCap = 0
	bs, err := BuildSearchBody(SearchParams{Keyword: "x", Size: 10}, opt)
	if err != nil {
		t.Fatalf("BuildSearchBody: %v", err)
	}
	body := decodeBody(t, bs)
	if _, ok := body["highlight"]; ok {
		t.Error("highlight must be omitted when no field configured")
	}
	if body["track_total_hits"] != false {
		t.Errorf("track_total_hits = %v, want false when cap is 0", body["track_total_hits"])
	}
	// 标签为空的兜底路径
	opt2 := dslOpts()
	opt2.PreTag, opt2.PostTag = "", ""
	bs2, err := BuildSearchBody(SearchParams{Keyword: "x", Size: 10}, opt2)
	if err != nil {
		t.Fatalf("BuildSearchBody: %v", err)
	}
	hl := decodeBody(t, bs2)["highlight"].(map[string]any)
	if hl["pre_tags"].([]any)[0] != "<em>" {
		t.Errorf("default pre tag must be <em>, got %v", hl["pre_tags"])
	}
}

func TestBuildPrefixBody(t *testing.T) {
	bs, err := BuildPrefixBody("原神", []string{model.DocTypeVideo}, 15)
	if err != nil {
		t.Fatalf("BuildPrefixBody: %v", err)
	}
	body := decodeBody(t, bs)
	if body["track_total_hits"] != false {
		t.Errorf("prefix query must not count totals: %v", body["track_total_hits"])
	}
	if int32(body["size"].(float64)) != 15 {
		t.Errorf("size = %v", body["size"])
	}
	must := boolOf(t, body)["must"].([]any)
	prefix, ok := must[0].(map[string]any)["prefix"].(map[string]any)
	if !ok {
		t.Fatalf("must[0] is not a prefix clause: %v", must[0])
	}
	spec, ok := prefix[FieldTitleKeyword].(map[string]any)
	if !ok {
		t.Fatalf("prefix must run on the keyword field (not analyzed), got %v", prefix)
	}
	if spec["value"] != "原神" || spec["case_insensitive"] != true {
		t.Errorf("prefix spec = %v", spec)
	}
	if terms := findFilter(t, body, "terms"); terms == nil {
		t.Error("missing doc_type filter in prefix query")
	}
	if _, err := BuildPrefixBody("", nil, 10); err != model.ErrInvalidKeyword {
		t.Errorf("empty prefix err = %v", err)
	}
	// size<=0 时兜底为 10，而不是把 0 传给引擎（那会返回 0 命中并冒充“无联想”）。
	bs2, err := BuildPrefixBody("a", nil, 0)
	if err != nil {
		t.Fatalf("BuildPrefixBody: %v", err)
	}
	if int32(decodeBody(t, bs2)["size"].(float64)) != 10 {
		t.Errorf("size fallback = %v, want 10", decodeBody(t, bs2)["size"])
	}
}

// TestQueryFingerprintStability 指纹必须对“同一查询”稳定、对筛选条件敏感：
// 它同时是缓存 key 与游标一致性校验的依据。
func TestQueryFingerprintStability(t *testing.T) {
	base := SearchParams{
		Keyword:    "关键词",
		DocTypes:   []string{model.DocTypeVideo},
		SortFields: []SortField{{Field: FieldPubTime, Desc: true}},
		Size:       30,
	}
	first := QueryFingerprint(base)
	if first == "" {
		t.Fatal("fingerprint must not be empty")
	}
	if second := QueryFingerprint(base); second != first {
		t.Fatalf("fingerprint not stable: %s vs %s", first, second)
	}
	if len(first) != 16 {
		t.Errorf("fingerprint length = %d, want 16 hex chars for cache keys", len(first))
	}

	variants := map[string]func(p *SearchParams){
		"keyword":     func(p *SearchParams) { p.Keyword = "另一个词" },
		"doctype":     func(p *SearchParams) { p.DocTypes = []string{model.DocTypeUser} },
		"zone":        func(p *SearchParams) { p.ZoneID = 9 },
		"duration":    func(p *SearchParams) { p.DurationMax = 600 },
		"publishedat": func(p *SearchParams) { p.PublishedAfter = 1 },
		"sort":        func(p *SearchParams) { p.SortFields = []SortField{{Field: FieldPubTime, Desc: false}} },
		"size":        func(p *SearchParams) { p.Size = 20 },
	}
	for name, mutate := range variants {
		p := base
		p.DocTypes = append([]string(nil), base.DocTypes...)
		p.SortFields = append([]SortField(nil), base.SortFields...)
		mutate(&p)
		if got := QueryFingerprint(p); got == first {
			t.Errorf("changing %s must change the fingerprint", name)
		}
	}

	// offset 不参与指纹：翻页时条件不变，游标才校验得过。
	withOffset := base
	withOffset.From = 900
	if QueryFingerprint(withOffset) != first {
		t.Error("offset must not affect the fingerprint (paging keeps the same query)")
	}
}
