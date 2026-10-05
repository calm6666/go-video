package esclient

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// AnalyzerKind 分词插件族。索引结构里的 analyzer 一旦上线不可原地改，
// 换分词族必须递增 schema_version 并走「建新索引 + 回填 + 切别名」流程。
type AnalyzerKind string

const (
	// AnalyzerStandard 只做标准切分：仅适合纯英文/ID 调试，中文检索会退化为整串匹配。
	AnalyzerStandard AnalyzerKind = "standard"
	// AnalyzerCJK 内置 standard tokenizer + cjk bigram filter，不需要集群插件，是默认值。
	AnalyzerCJK AnalyzerKind = "cjk"
	// AnalyzerIK analysis-ik 插件：index=ik_max_word、search=ik_smart，召回与精度平衡最好。
	AnalyzerIK AnalyzerKind = "ik"
	// AnalyzerSmartCN analysis-smartcn 插件：OpenSearch 自带的中文分析插件族。
	AnalyzerSmartCN AnalyzerKind = "smartcn"
)

// 分析器与 filter 名称。mapping 只按名字引用，避免同一段配置在多处内联漂移。
const (
	// IndexAnalyzerName 写入分析器名（title/description/author_name 共用）。
	IndexAnalyzerName = "go_video_title"
	// SearchAnalyzerName 查询分析器名；search-query 侧不显式传 analyzer，
	// 由这里的 mapping 决定查询分词，保证读写同源。
	SearchAnalyzerName = "go_video_search"

	stopwordFilterName = "go_video_stopwords"
	synonymFilterName  = "go_video_synonyms"
	cjkFilterName      = "go_video_cjk_bigram"
)

// Analyzer 分词配置（对应配置段 OpenSearch.Analyzer）。
//
// 单一来源约束：索引 mapping 只由 IndexBody 生成；deploy/opensearch 下的脚本
// 通过 `go run ./services/search-indexer/cmd/esmapping` 取同一份 JSON，
// 不允许在 shell/JSON 文件里另写一份 mapping 定义。
// StopwordsPath/SynonymsPath 是集群容器内的绝对路径，文件由 compose 挂载
// deploy/opensearch/analysis 提供；改词典内容需要重建索引才对写入侧生效。
type Analyzer struct {
	Kind          AnalyzerKind
	StopwordsPath string
	SynonymsPath  string
}

// DefaultAnalyzer 未配置时的分词策略：内置 cjk，保证零插件依赖也能中文召回。
func DefaultAnalyzer() Analyzer {
	return Analyzer{Kind: AnalyzerCJK}
}

// normalize 补齐默认值并校验取值；非法配置返回错误而不是静默降级。
func (a Analyzer) normalize() (Analyzer, error) {
	out := a
	if out.Kind == "" {
		out.Kind = AnalyzerCJK
	}
	switch out.Kind {
	case AnalyzerStandard, AnalyzerCJK, AnalyzerIK, AnalyzerSmartCN:
	default:
		return Analyzer{}, fmt.Errorf("esclient: 未知 OpenSearch.Analyzer.Kind %q（可选 standard|cjk|ik|smartcn）", out.Kind)
	}
	return out, nil
}

// buildAnalysis 生成 settings.analysis（analyzer + filter）。
//
// 停用词挂在写入侧（改变索引内容，改词典必须重建索引）；同义词只挂查询侧并标
// updateable，这样改同义词表只需 reload，不必重建索引 —— 这是 ES/OpenSearch
// 对 file-based synonym 的唯一合法用法。
func (a Analyzer) buildAnalysis() map[string]interface{} {
	filters := map[string]interface{}{}
	indexFilters := []string{"lowercase"}
	searchFilters := []string{"lowercase"}

	if a.Kind == AnalyzerCJK {
		filters[cjkFilterName] = map[string]interface{}{"type": "cjk"}
		indexFilters = append(indexFilters, cjkFilterName)
		searchFilters = append(searchFilters, cjkFilterName)
	}
	if a.StopwordsPath != "" {
		filters[stopwordFilterName] = map[string]interface{}{
			"type":           "stop",
			"stopwords_path": a.StopwordsPath,
		}
		indexFilters = append(indexFilters, stopwordFilterName)
		searchFilters = append(searchFilters, stopwordFilterName)
	}
	if a.SynonymsPath != "" {
		filters[synonymFilterName] = map[string]interface{}{
			"type":          "synonym_graph",
			"synonyms_path": a.SynonymsPath,
			// updateable 只允许出现在查询分析器引用的 filter 上。
			"updateable": true,
		}
		searchFilters = append(searchFilters, synonymFilterName)
	}

	custom := func(tokenizer string, chain []string) map[string]interface{} {
		return map[string]interface{}{"type": "custom", "tokenizer": tokenizer, "filter": chain}
	}

	analyzers := map[string]interface{}{}
	switch a.Kind {
	case AnalyzerIK:
		// 写入用 ik_max_word 穷举细粒度切分，查询用 ik_smart 减少误召回。
		// 插件缺失时集群会在建索引阶段直接报错，es-init.ps1 已提前探测并给出提示。
		analyzers[IndexAnalyzerName] = custom("ik_max_word", indexFilters)
		analyzers[SearchAnalyzerName] = custom("ik_smart", searchFilters)
	case AnalyzerSmartCN:
		analyzers[IndexAnalyzerName] = custom("smartcn", indexFilters)
		analyzers[SearchAnalyzerName] = custom("smartcn", searchFilters)
	default: // AnalyzerStandard / AnalyzerCJK
		analyzers[IndexAnalyzerName] = custom("standard", indexFilters)
		analyzers[SearchAnalyzerName] = custom("standard", searchFilters)
	}

	analysis := map[string]interface{}{"analyzer": analyzers}
	if len(filters) > 0 {
		analysis["filter"] = filters
	}
	return analysis
}

// analyzedText 需要中文分词的 text 字段。
func analyzedText(extra map[string]interface{}) map[string]interface{} {
	field := map[string]interface{}{
		"type":            "text",
		"analyzer":        IndexAnalyzerName,
		"search_analyzer": SearchAnalyzerName,
	}
	for k, v := range extra {
		field[k] = v
	}
	return field
}

// IndexBody 生成创建物理索引的请求体（settings + mappings）。
//
// 字段来源约束（AGENTS.md §5）：这里声明的都是 search-indexer 自有的投影字段，
// 事实字段由 video/catalog/live-room 通过事件或 RPC 推送，本服务不自行补齐。
// schemaVersion 预留 v2 之类的结构升级：变更字段类型或分词族时必须递增版本并走
// 「建新索引 + 回填 + 切别名」流程，禁止原地改已上线索引的 mapping。
// analyzer 传零值表示用 DefaultAnalyzer 的 cjk 策略。
func IndexBody(schemaVersion string, analyzer Analyzer) ([]byte, error) {
	a, err := analyzer.normalize()
	if err != nil {
		return nil, err
	}
	if schemaVersion == "" {
		schemaVersion = DefaultSchemaVersion
	}
	keywordSub := func(ignoreAbove int) map[string]interface{} {
		return map[string]interface{}{"fields": map[string]interface{}{"keyword": map[string]interface{}{"type": "keyword", "ignore_above": ignoreAbove}}}
	}

	meta := map[string]interface{}{
		"schema_version": schemaVersion,
		"analyzer":       string(a.Kind),
		"owner":          "search-indexer",
	}
	if a.StopwordsPath != "" {
		meta["stopwords_path"] = a.StopwordsPath
	}
	if a.SynonymsPath != "" {
		meta["synonyms_path"] = a.SynonymsPath
	}

	body := map[string]interface{}{
		"settings": map[string]interface{}{
			"index": map[string]interface{}{
				// 写入不逐条 refresh，由查询侧接受近实时（1s）延迟；
				// 重建收尾会显式调用一次 _refresh。
				"refresh_interval":   "1s",
				"number_of_shards":   "1",
				"number_of_replicas": "0",
			},
			"analysis": a.buildAnalysis(),
		},
		"mappings": map[string]interface{}{
			// _meta 把结构版本与分词族随索引一起落地：排障与重建校验时无需反推索引名
			// 即可判定该索引是哪一版结构建的（登记表 search_index_version.schema_version
			// 同源；scripts/es-init.ps1 用它比对进程配置与集群现状）。
			"_meta": meta,
			"properties": map[string]interface{}{
				"content_id":       map[string]interface{}{"type": "long"},
				"content_type":     map[string]interface{}{"type": "integer"},
				"title":            analyzedText(keywordSub(256)),
				"description":      analyzedText(nil),
				"cover_url":        map[string]interface{}{"type": "keyword", "index": false},
				"author_mid":       map[string]interface{}{"type": "long"},
				"author_name":      analyzedText(keywordSub(128)),
				"typeid":           map[string]interface{}{"type": "integer"},
				"type_name":        map[string]interface{}{"type": "keyword"},
				"tags":             map[string]interface{}{"type": "keyword"},
				"duration_sec":     map[string]interface{}{"type": "long"},
				"publish_at":       map[string]interface{}{"type": "long"},
				"ctime":            map[string]interface{}{"type": "long"},
				"state":            map[string]interface{}{"type": "integer"},
				"doc_revision":     map[string]interface{}{"type": "long"},
				"rights_expire_at": map[string]interface{}{"type": "long"},
				"language":         map[string]interface{}{"type": "keyword"},
				"subtitle_langs":   map[string]interface{}{"type": "keyword"},
				"sensitive":        map[string]interface{}{"type": "boolean"},
				"schema_version":   map[string]interface{}{"type": "integer"},
				"heat": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"view_count":     map[string]interface{}{"type": "long"},
						"like_count":     map[string]interface{}{"type": "long"},
						"favorite_count": map[string]interface{}{"type": "long"},
						"share_count":    map[string]interface{}{"type": "long"},
						"comment_count":  map[string]interface{}{"type": "long"},
						"danmaku_count":  map[string]interface{}{"type": "long"},
						"heat_score":     map[string]interface{}{"type": "integer"},
						"heat_revision":  map[string]interface{}{"type": "long"},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// 该结构全部由本函数构造，序列化不可能失败；panic 比静默返回空 body 更安全。
	if err := enc.Encode(body); err != nil {
		panic(fmt.Sprintf("esclient: marshal index body: %v", err))
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
