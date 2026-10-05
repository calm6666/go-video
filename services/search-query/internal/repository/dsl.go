package repository

import (
	"encoding/json"
	"fmt"

	"go-video/services/search-query/model"
)

// 查询 DSL 构造。
//
// 安全说明：所有用户输入都以 JSON 字符串值的形式进入请求体（encoding/json 负责转义），
// 不使用 query_string / simple_query_string 这类“值即语法”的查询，因此
// `+ - = && || > < ! ( ) { } [ ] ^ " ~ * ? : \` 等 Lucene 操作符不会被引擎解析成语法，
// 用户也无法闭合括号注入额外子句。仓库层再叠加一层关键词清洗（keyword.go）。

// 引擎字段名。
// ⚠ 这批名字来自 openbilibili 时代的搜索索引词汇，与 `services/search-indexer` 现在生成的
// mapping 并不一致（对方是 content_type/content_id/description/typeid/publish_at/heat.*，
// 且没有 fans_count 与 user 文档）。真实联调前必须先做读/写两侧契约对齐，
// 见本服务 README「索引契约」与 docs/roadmap.md 的搜索待办；在此之前不要声称搜索可用。
const (
	FieldDocType      = "doc_type"
	FieldDocID        = "doc_id"
	FieldTitle        = "title"
	FieldTitleKeyword = "title.keyword"
	FieldIntro        = "intro"
	FieldAuthorMid    = "author_mid"
	FieldAuthorName   = "author_name"
	FieldZoneID       = "zone_id"
	FieldCoverURL     = "cover_url"
	FieldViewCount    = "view_count"
	FieldLikeCount    = "like_count"
	FieldDanmaku      = "danmaku_count"
	FieldFansCount    = "fans_count"
	FieldDuration     = "duration_sec"
	FieldPubTime      = "pub_time"
	FieldHotScore     = "hot_score"
	FieldState        = "state"
	FieldTags         = "tags"
)

// 排序方向常量（引擎侧）。
const (
	OrderAsc  = "asc"
	OrderDesc = "desc"
)

// SortField 一个引擎排序子句。
type SortField struct {
	Field string // 字段名；_score 表示相关性
	Desc  bool
}

// scoreSort 相关性排序（_score 只支持降序）。
func scoreSort() SortField { return SortField{Field: "_score", Desc: true} }

// SearchParams 经 logic 校验后的查询条件，字段已是引擎可直接使用的形态。
//
// 区间语义：DurationMin 含下界（0 表示不限制）、DurationMax 不含上界（0 表示不限制）；
// PublishedAfter/PublishedBefore 均为 Unix 秒，0 表示不限制。
type SearchParams struct {
	Keyword         string
	DocTypes        []string
	ZoneID          int32
	DurationMin     int32
	DurationMax     int32
	PublishedAfter  int64
	PublishedBefore int64
	SortFields      []SortField
	From            int64
	Size            int32
	Fingerprint     string
}

// DSLOptions 构造请求体所需的静态配置（来自 SearchConf/OpenSearchConf）。
type DSLOptions struct {
	// MatchFields multi_match 字段列表，可带 boost（例 "title^3"）。
	MatchFields []string
	// HighlightFields 需要高亮的字段（title/intro 等）。
	HighlightFields []string
	// PreTag/PostTag 高亮标签。
	PreTag  string
	PostTag string
	// TrackTotalCap 统计总数的上限（对应 track_total_hits），0 表示不返回精确总数。
	TrackTotalCap int64
	// SourceFields 返回的投影字段；为空表示返回全部 _source。
	SourceFields []string
	// EngineTimeout 引擎侧超时（例 "1s"），0 表示不设。
	EngineTimeout string
}

// DefaultSourceFields 查询侧需要的投影字段（避免把整个文档拖回网关）。
var DefaultSourceFields = []string{
	FieldDocType, FieldDocID, FieldTitle, FieldIntro, FieldAuthorMid, FieldAuthorName,
	FieldZoneID, FieldCoverURL, FieldViewCount, FieldLikeCount, FieldDanmaku, FieldFansCount,
	FieldDuration, FieldPubTime, FieldHotScore, FieldState, FieldTags,
}

// DefaultMatchFields 默认检索字段与权重：标题 > 标签/UP 主 > 简介。
var DefaultMatchFields = []string{
	FieldTitle + "^3", FieldTags + "^2", FieldAuthorName + "^2", FieldIntro,
}

// DefaultHighlightFields 默认高亮字段。
var DefaultHighlightFields = []string{FieldTitle, FieldIntro}

// BuildSearchBody 构造 _search 请求体。
// p.Keyword 为空视为调用方错误（返回 ErrInvalidKeyword），不允许退化成 match_all，
// 避免把“未带关键词”的请求当成全库浏览返回随机结果。
func BuildSearchBody(p SearchParams, opt DSLOptions) ([]byte, error) {
	if p.Keyword == "" {
		return nil, model.ErrInvalidKeyword
	}
	if p.Size <= 0 {
		return nil, fmt.Errorf("%w: size must be positive, got %d", model.ErrInvalidPage, p.Size)
	}
	if p.From < 0 {
		return nil, fmt.Errorf("%w: from must not be negative, got %d", model.ErrInvalidPage, p.From)
	}

	must := []any{
		map[string]any{
			"multi_match": map[string]any{
				"query":  p.Keyword,
				"fields": matchFields(opt),
				"type":   "best_fields",
			},
		},
	}

	filters := buildFilters(p)
	boolQuery := map[string]any{"must": must}
	if len(filters) > 0 {
		boolQuery["filter"] = filters
	}

	body := map[string]any{
		"from":  p.From,
		"size":  p.Size,
		"query": map[string]any{"bool": boolQuery},
	}
	if opt.TrackTotalCap > 0 {
		body["track_total_hits"] = opt.TrackTotalCap
	} else {
		body["track_total_hits"] = false
	}
	if fields := sourceFields(opt); len(fields) > 0 {
		body["_source"] = fields
	}
	if len(p.SortFields) > 0 {
		body["sort"] = buildSort(p.SortFields)
	}
	if hf := opt.HighlightFields; len(hf) > 0 {
		body["highlight"] = buildHighlight(hf, opt)
	}
	if opt.EngineTimeout != "" {
		body["timeout"] = opt.EngineTimeout
	}
	return json.Marshal(body)
}

// buildFilters 组装 filter 子句（不贡献打分，只裁剪候选集）。
func buildFilters(p SearchParams) []any {
	filters := make([]any, 0, 5)
	if len(p.DocTypes) > 0 {
		terms := make([]any, 0, len(p.DocTypes))
		for _, d := range p.DocTypes {
			terms = append(terms, d)
		}
		filters = append(filters, map[string]any{"terms": map[string]any{FieldDocType: terms}})
	}
	if p.ZoneID > 0 {
		filters = append(filters, map[string]any{"term": map[string]any{FieldZoneID: p.ZoneID}})
	}
	if dr := buildRange(FieldDuration, int64(p.DurationMin), int64(p.DurationMax), true); dr != nil {
		filters = append(filters, dr)
	}
	if pr := buildRange(FieldPubTime, p.PublishedAfter, p.PublishedBefore, false); pr != nil {
		filters = append(filters, pr)
	}
	return filters
}

// buildRange 生成 {"range": {field: {gte: min, lt: max}}}；两端都为 0 时返回 nil。
// exclusiveUpper=true 时上界用 lt，否则用 lte（发布时间含当天末秒更直观）。
func buildRange(field string, min, max int64, exclusiveUpper bool) any {
	if min <= 0 && max <= 0 {
		return nil
	}
	clause := map[string]any{}
	if min > 0 {
		clause["gte"] = min
	}
	if max > 0 {
		if exclusiveUpper {
			clause["lt"] = max
		} else {
			clause["lte"] = max
		}
	}
	return map[string]any{"range": map[string]any{field: clause}}
}

// buildSort 生成 sort 数组。_score 必须以字符串 "desc" 形式给出，其它字段用对象形式。
func buildSort(fields []SortField) []any {
	out := make([]any, 0, len(fields))
	for _, f := range fields {
		order := OrderAsc
		if f.Desc {
			order = OrderDesc
		}
		if f.Field == "_score" {
			out = append(out, map[string]any{"_score": map[string]any{"order": "desc"}})
			continue
		}
		out = append(out, map[string]any{f.Field: map[string]any{"order": order, "missing": "_last"}})
	}
	return out
}

// buildHighlight 高亮配置：标题整段返回（fragment 0），简介切片段。
func buildHighlight(fields []string, opt DSLOptions) map[string]any {
	pre, post := opt.PreTag, opt.PostTag
	if pre == "" {
		pre = "<em>"
	}
	if post == "" {
		post = "</em>"
	}
	hf := make(map[string]any, len(fields))
	for _, f := range fields {
		if f == FieldIntro {
			hf[f] = map[string]any{"fragment_size": 120, "number_of_fragments": 2}
			continue
		}
		hf[f] = map[string]any{"number_of_fragments": 0}
	}
	return map[string]any{
		"pre_tags":  []string{pre},
		"post_tags": []string{post},
		"fields":    hf,
	}
}

// matchFields 返回检索字段列表，未配置时使用默认值。
func matchFields(opt DSLOptions) []string {
	if len(opt.MatchFields) > 0 {
		return opt.MatchFields
	}
	return DefaultMatchFields
}

// sourceFields 返回投影字段列表。
func sourceFields(opt DSLOptions) []string {
	if opt.SourceFields != nil {
		return opt.SourceFields
	}
	return DefaultSourceFields
}

// BuildPrefixBody 构造联想冷启动用的前缀查询（Redis ZSET 词典未覆盖低频词时回源引擎）。
// 使用 title.keyword 的 prefix 子句 + 热度排序；prefix 的 value 同样是 JSON 字符串值，
// 不解析查询语法。
func BuildPrefixBody(prefix string, docTypes []string, size int32) ([]byte, error) {
	if prefix == "" {
		return nil, model.ErrInvalidKeyword
	}
	if size <= 0 {
		size = 10
	}
	filters := make([]any, 0, 1)
	if len(docTypes) > 0 {
		terms := make([]any, 0, len(docTypes))
		for _, d := range docTypes {
			terms = append(terms, d)
		}
		filters = append(filters, map[string]any{"terms": map[string]any{FieldDocType: terms}})
	}
	boolQuery := map[string]any{
		"must": []any{map[string]any{"prefix": map[string]any{FieldTitleKeyword: map[string]any{"value": prefix, "case_insensitive": true}}}},
	}
	if len(filters) > 0 {
		boolQuery["filter"] = filters
	}
	body := map[string]any{
		"from":             0,
		"size":             size,
		"query":            map[string]any{"bool": boolQuery},
		"track_total_hits": false,
		"_source":          []string{FieldTitle, FieldHotScore, FieldDocType},
		"sort":             buildSort([]SortField{{Field: FieldHotScore, Desc: true}, scoreSort()}),
	}
	return json.Marshal(body)
}
