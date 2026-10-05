package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/search-query/model"
)

// 联想候选来源。
const (
	SourceHistory = "history"    // 用户自己的搜索历史
	SourceDict    = "redis"      // Redis ZSET 词典（离线聚合维护）
	SourceEngine  = "opensearch" // 引擎前缀查询（冷启动兜底）
)

// SuggestParams 联想查询条件（Prefix 已规范化）。
type SuggestParams struct {
	Prefix   string
	Limit    int32
	Mid      int64    // 0 表示游客，不合并历史
	DocTypes []string // 引擎回源时限定的 doc_type
}

// SuggestCandidate 一条联想候选。
type SuggestCandidate struct {
	Keyword string
	Weight  float64
	Source  string
}

// Suggest 输入前缀联想。
//
// 选型：主链路用 Redis ZSET 词典（key sq:sug:v1），而不是 OpenSearch completion。
// 理由：
//  1. 联想是最高频、最低容忍延迟的接口（每次击键都可能触发），ZSET 取 Top-N
//     不占引擎查询配额，也不会因为索引重建/别名切换出现抖动；
//  2. completion suggester 的词典要随索引版本重建，索引切换窗口内联想会退化，
//     而词典由离线聚合刷新，独立于索引生命周期；
//  3. 代价：前缀过滤发生在取回的候选窗口内（SuggestCandidateWindow），
//     窗口外的低频词不会命中 —— 因此保留引擎 prefix 回源作为冷启动兜底。
//
// 失败语义：词典与引擎回源都不可用时返回 model.ErrSearchUnavailable；
// 只要有一侧成功就返回真实候选（可能少于 limit），不返回伪造词。
func (r *Repository) Suggest(ctx context.Context, p SuggestParams) ([]SuggestCandidate, error) {
	if p.Prefix == "" {
		return nil, model.ErrInvalidKeyword
	}
	if p.Limit <= 0 {
		p.Limit = r.cfg.SuggestLimit
	}
	if p.Limit <= 0 {
		p.Limit = 10
	}

	var (
		cands     []SuggestCandidate
		dictErr   error
		engineErr error
	)

	// 1) 用户历史（登录态优先展示，权重用最近搜索时间，保证“越近越靠前”）。
	if p.Mid > 0 {
		rows, err := r.historyMd.ListByPrefix(ctx, p.Mid, p.Prefix, p.Limit)
		if err != nil {
			// 历史读失败不影响联想整体可用性，记录后继续。
			logx.Errorf("search-query/suggest: history prefix mid=%d err=%v", p.Mid, err)
		}
		for _, row := range rows {
			cands = append(cands, SuggestCandidate{Keyword: row.Keyword, Weight: float64(row.Mtime), Source: SourceHistory})
		}
	}

	// 2) Redis 词典。
	window := r.cfg.SuggestCandidateWindow
	if window <= 0 {
		window = 500
	}
	var pairs []redis.FloatPair
	if r.cache != nil {
		pairs, dictErr = r.cache.SuggestDict(ctx, window)
	} else {
		dictErr = errCacheUnavailable
	}
	if dictErr != nil {
		logx.Errorf("search-query/suggest: read dict err=%v", dictErr)
	} else {
		for _, kv := range pairs {
			if !matchPrefix(kv.Key, p.Prefix) {
				continue
			}
			cands = append(cands, SuggestCandidate{Keyword: kv.Key, Weight: kv.Score, Source: SourceDict})
		}
	}

	// 3) 候选不足时回源引擎前缀查询（冷启动/低频词）。
	if int32(len(dedupeCandidates(cands))) < p.Limit && p.Prefix != "" {
		fromEngine, err := r.suggestFromEngine(ctx, p)
		if err != nil {
			engineErr = err
			logx.Errorf("search-query/suggest: engine fallback prefix=%q err=%v", p.Prefix, err)
		} else {
			cands = append(cands, fromEngine...)
		}
	}

	out := dedupeCandidates(cands)
	if len(out) == 0 && dictErr != nil && engineErr != nil {
		// 两个候选来源都不可用：明确降级，不返回“空但成功”。
		return nil, fmt.Errorf("%w: dict=%v engine=%v", model.ErrSearchUnavailable, dictErr, engineErr)
	}

	// 4) 出口屏蔽过滤 + 截断。
	blocked := r.blockedWordSet(ctx)
	filtered := make([]SuggestCandidate, 0, len(out))
	for _, c := range out {
		if _, hit := blocked[c.Keyword]; hit {
			continue
		}
		filtered = append(filtered, c)
	}
	if int32(len(filtered)) > p.Limit {
		filtered = filtered[:p.Limit]
	}
	return filtered, nil
}

// errCacheUnavailable 缓存层未初始化（配置缺失或测试场景），联想会转用引擎回源。
var errCacheUnavailable = errors.New("search-query: cache unavailable")

// suggestFromEngine 用 title.keyword 前缀查询补齐联想候选。
func (r *Repository) suggestFromEngine(ctx context.Context, p SuggestParams) ([]SuggestCandidate, error) {
	if !r.EngineAvailable() {
		return nil, model.ErrSearchUnavailable
	}
	size := p.Limit*2 + 5 // 多取一些，去重后仍可能不足
	if size > 50 {
		size = 50
	}
	body, err := BuildPrefixBody(p.Prefix, p.DocTypes, size)
	if err != nil {
		return nil, err
	}
	resp, err := r.es.Search(ctx, body)
	if err != nil {
		return nil, MapEngineError(err)
	}
	out := make([]SuggestCandidate, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		kw := NormalizeKeyword(hit.Source.Title)
		if kw == "" {
			continue
		}
		out = append(out, SuggestCandidate{Keyword: kw, Weight: hit.Source.HotScore, Source: SourceEngine})
	}
	return out, nil
}

// matchPrefix 前缀匹配（大小写不敏感，按 rune 比较）。
func matchPrefix(candidate, prefix string) bool {
	if prefix == "" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(prefix))
}

// dedupeCandidates 按“规范化后的小写形式”去重，保留权重更高的一条；
// 来源优先级：history > redis > opensearch（同分时按此顺序）。
func dedupeCandidates(in []SuggestCandidate) []SuggestCandidate {
	if len(in) == 0 {
		return nil
	}
	rank := map[string]int{SourceHistory: 0, SourceDict: 1, SourceEngine: 2}
	best := make(map[string]SuggestCandidate, len(in))
	for _, c := range in {
		key := strings.ToLower(c.Keyword)
		if key == "" {
			continue
		}
		cur, ok := best[key]
		if !ok {
			best[key] = c
			continue
		}
		if c.Weight > cur.Weight || (c.Weight == cur.Weight && rank[c.Source] < rank[cur.Source]) {
			best[key] = c
		}
	}
	out := make([]SuggestCandidate, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return rank[out[i].Source] < rank[out[j].Source]
	})
	return out
}
