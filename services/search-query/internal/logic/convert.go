package logic

// 本文件是 rpc 枚举与引擎查询条件之间的映射（手写扩展，不属于 goctl 生成边界）。
//
// 原则：本服务只按索引投影字段检索，不猜测上游数据；doc_type 与字段名
// 由 search-indexer 的 mapping 约定决定（见服务 README）。

import (
	"fmt"
	"strings"

	"go-video/services/search-query/internal/config"
	"go-video/services/search-query/internal/esclient"
	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

// normalizePageSize 归一化每页大小：未传取默认值，超过上限直接截断
// （截断而不是报错，兼容历史客户端；上限本身仍受引擎窗口约束）。
func normalizePageSize(in int32, cfg config.SearchConf) int32 {
	ps := in
	if ps <= 0 {
		ps = cfg.PsDefault
	}
	if ps <= 0 {
		ps = 30
	}
	if cfg.PsLimit > 0 && ps > cfg.PsLimit {
		ps = cfg.PsLimit
	}
	return ps
}

// normalizeLimit 归一化列表条数（cursor 分页接口的 limit 与 page size 分开处理：
// limit 超过上限视为调用方错误，避免被当成合法的截断请求）。
func normalizeLimit(in, def, max int32) (int32, error) {
	if in < 0 {
		return 0, fmt.Errorf("%w: limit must not be negative", model.ErrInvalidPage)
	}
	n := in
	if n == 0 {
		n = def
	}
	if n <= 0 {
		n = 10
	}
	if max > 0 && n > max {
		return 0, fmt.Errorf("%w: limit %d exceeds max %d", model.ErrInvalidPage, n, max)
	}
	return n, nil
}

// docTypesOf 把搜索类型翻译成索引 doc_type 过滤集合。
func docTypesOf(t rpc.SearchType) ([]string, error) {
	switch t {
	case rpc.SearchType_SEARCH_TYPE_UNSPECIFIED, rpc.SearchType_SEARCH_TYPE_ALL:
		return []string{model.DocTypeVideo, model.DocTypeUser, model.DocTypePGC}, nil
	case rpc.SearchType_SEARCH_TYPE_VIDEO:
		return []string{model.DocTypeVideo}, nil
	case rpc.SearchType_SEARCH_TYPE_USER:
		return []string{model.DocTypeUser}, nil
	case rpc.SearchType_SEARCH_TYPE_PGC:
		// PGC 结果来自 catalog/rights 事件驱动的索引投影（doc_type=pgc），
		// 本服务不查 catalog 库，也不合成版权窗口信息。
		return []string{model.DocTypePGC}, nil
	default:
		return nil, fmt.Errorf("%w: %v", model.ErrInvalidSearchType, t)
	}
}

// resolveSort 解析排序枚举（UNSPECIFIED 回落到服务端默认配置）。
func resolveSort(in rpc.SortMode, defaultSort int32) rpc.SortMode {
	if in == rpc.SortMode_SORT_UNSPECIFIED {
		return rpc.SortMode(defaultSort)
	}
	return in
}

// sortFieldsOf 把排序枚举翻译成引擎 sort 子句，并校验与搜索类型的组合是否合法。
func sortFieldsOf(t rpc.SearchType, s rpc.SortMode) ([]repository.SortField, error) {
	latest := repository.SortField{Field: repository.FieldPubTime, Desc: true}
	switch s {
	case rpc.SortMode_SORT_COMPREHENSIVE:
		return []repository.SortField{
			{Field: "_score", Desc: true},
			latest,
		}, nil
	case rpc.SortMode_SORT_LATEST:
		return []repository.SortField{latest}, nil
	case rpc.SortMode_SORT_MOST_VIEW:
		return []repository.SortField{
			{Field: repository.FieldViewCount, Desc: true},
			latest,
		}, nil
	case rpc.SortMode_SORT_MOST_FANS:
		// 粉丝数只在 user 文档上有意义，其它类型直接拒绝，避免静默退化排序。
		if t != rpc.SearchType_SEARCH_TYPE_USER {
			return nil, fmt.Errorf("%w: fans sort requires user search", model.ErrInvalidSort)
		}
		return []repository.SortField{{Field: repository.FieldFansCount, Desc: true}}, nil
	case rpc.SortMode_SORT_HOT_SCORE:
		return []repository.SortField{
			{Field: repository.FieldHotScore, Desc: true},
			latest,
		}, nil
	default:
		return nil, fmt.Errorf("%w: %v", model.ErrInvalidSort, s)
	}
}

// durationRangeOf 时长筛选 -> [min, max) 秒。max=0 表示无上界。
func durationRangeOf(b rpc.DurationBucket) (int32, int32, error) {
	switch b {
	case rpc.DurationBucket_DURATION_UNSPECIFIED:
		return 0, 0, nil
	case rpc.DurationBucket_DURATION_LT_1MIN:
		return 0, 60, nil
	case rpc.DurationBucket_DURATION_1_10MIN:
		return 60, 600, nil
	case rpc.DurationBucket_DURATION_10_30MIN:
		return 600, 1800, nil
	case rpc.DurationBucket_DURATION_30_60MIN:
		return 1800, 3600, nil
	case rpc.DurationBucket_DURATION_GT_60MIN:
		return 3600, 0, nil
	default:
		return 0, 0, fmt.Errorf("%w: duration bucket %v", model.ErrInvalidPage, b)
	}
}

// toSearchHits 把引擎命中翻译成 rpc 投影。
func toSearchHits(hits []esclient.Hit) []*rpc.SearchHit {
	if len(hits) == 0 {
		return nil
	}
	out := make([]*rpc.SearchHit, 0, len(hits))
	for _, h := range hits {
		src := h.Source
		item := &rpc.SearchHit{
			DocType:        src.DocType,
			DocId:          src.DocID,
			Title:          src.Title,
			ContentSnippet: src.Intro,
			AuthorMid:      src.AuthorMid,
			AuthorName:     src.AuthorName,
			ZoneId:         src.ZoneID,
			CoverUrl:       src.CoverURL,
			ViewCount:      src.ViewCount,
			LikeCount:      src.LikeCount,
			DanmakuCount:   src.DanmakuCount,
			FansCount:      src.FansCount,
			DurationSec:    src.DurationSec,
			PubTime:        src.PubTime,
			Score:          h.Score,
			State:          src.State,
			Highlights:     flattenHighlights(h.Highlights),
		}
		// 标题高亮优先返回给 title，保持与旧客户端的字段兼容。
		if frags, ok := h.Highlights[repository.FieldTitle]; ok && len(frags) > 0 {
			item.Title = frags[0]
		}
		if frags, ok := h.Highlights[repository.FieldIntro]; ok && len(frags) > 0 {
			item.ContentSnippet = strings.Join(frags, " … ")
		}
		out = append(out, item)
	}
	return out
}

// flattenHighlights 把引擎的多片段高亮压成 字段->文本；其它字段透传（不含 title/intro）。
func flattenHighlights(in map[string][]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if k == repository.FieldTitle || k == repository.FieldIntro {
			continue
		}
		if len(v) == 0 {
			continue
		}
		out[k] = strings.Join(v, " … ")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resultStateOf 把 rpc 枚举翻译成 DB 存储的字符串（与 search_query_log.result_state 一致）。
func resultStateOf(s rpc.QueryResultState) (string, error) {
	switch s {
	case rpc.QueryResultState_RESULT_STATE_OK:
		return model.ResultStateOK, nil
	case rpc.QueryResultState_RESULT_STATE_EMPTY:
		return model.ResultStateEmpty, nil
	case rpc.QueryResultState_RESULT_STATE_DEGRADED:
		return model.ResultStateDegraded, nil
	case rpc.QueryResultState_RESULT_STATE_BLOCKED:
		return model.ResultStateBlocked, nil
	case rpc.QueryResultState_RESULT_STATE_UNSPECIFIED:
		return "", fmt.Errorf("%w: result_state is required", model.ErrInvalidPage)
	default:
		return "", fmt.Errorf("%w: unknown result_state %v", model.ErrInvalidPage, s)
	}
}

// truncate 按 rune 安全截断（DB 列长度保护，不切断多字节字符）。
func truncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes])
}

// isSupportedPlatform 端标识白名单（Android/iOS/HarmonyOS/桌面）。
// 仅用于校验与统计分桶，不用于任何 UI 行为分支。
func isSupportedPlatform(p string) bool {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "android", "ios", "harmony", "harmonyos", "desktop", "web", "":
		return true
	default:
		return false
	}
}
