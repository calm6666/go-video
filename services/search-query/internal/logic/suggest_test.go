package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/search-query/internal/esclient"
	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

func doSuggest(t *testing.T, st *store, in *rpc.SuggestReq) (*rpc.SuggestReply, error) {
	t.Helper()
	return NewSuggestLogic(context.Background(), st.svcCtx()).Suggest(in)
}

// suggestReq 一条所有守卫都能过的基准联想请求。
func suggestReq() *rpc.SuggestReq {
	return &rpc.SuggestReq{
		Keyword:    "开源",
		Limit:      3,
		Platform:   "android",
		SearchType: rpc.SearchType_SEARCH_TYPE_VIDEO,
	}
}

// TestSuggestRejectsInvalidRequestsBeforeAnyDependency 参数守卫表：
// 全部坏输入必须在读词典/打引擎/查历史之前被拒。
func TestSuggestRejectsInvalidRequestsBeforeAnyDependency(t *testing.T) {
	cfg := testConfig() // KeywordMaxLen=64，与 etc/searchquery.v1.yaml 一致
	ok := suggestReq()
	cases := []struct {
		name string
		req  *rpc.SuggestReq
		want error
	}{
		{"空请求", nil, model.ErrInvalidKeyword},
		{"空前缀", bad(ok, func(r *rpc.SuggestReq) { r.Keyword = "" }), model.ErrInvalidKeyword},
		{"纯空白前缀", bad(ok, func(r *rpc.SuggestReq) { r.Keyword = " \t\n " }), model.ErrInvalidKeyword},
		{"只含被清洗字符", bad(ok, func(r *rpc.SuggestReq) { r.Keyword = "<>" }), model.ErrInvalidKeyword},
		{"超 KeywordMaxLen", bad(ok, func(r *rpc.SuggestReq) {
			r.Keyword = strings.Repeat("词", int(cfg.Search.KeywordMaxLen)+1)
		}), model.ErrInvalidKeyword},
		{"未知端", bad(ok, func(r *rpc.SuggestReq) { r.Platform = "nokia" }), model.ErrInvalidPlatform},
		{"未知搜索类型", bad(ok, func(r *rpc.SuggestReq) { r.SearchType = rpc.SearchType(77) }), model.ErrInvalidSearchType},
		{"limit 为负", bad(ok, func(r *rpc.SuggestReq) { r.Limit = -1 }), model.ErrInvalidPage},
		{"limit 超上限（联想不静默截断）", bad(ok, func(r *rpc.SuggestReq) { r.Limit = 11 }), model.ErrInvalidPage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, cfg)
			reply, err := doSuggest(t, st, tc.req)
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：被拒的请求不应返回响应体，实际 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, st, 0)
		})
	}

	// 边界：恰好 KeywordMaxLen 个 rune 合法，且 limit=SuggestLimit 的上限值本身合法。
	st := newStore(t, cfg)
	st.cache.warmDict(redis.FloatPair{Key: "开源", Score: 10})
	atLimit := bad(ok, func(r *rpc.SuggestReq) {
		r.Keyword = strings.Repeat("词", int(cfg.Search.KeywordMaxLen))
		r.Limit = cfg.Search.SuggestLimit
	})
	reply, err := doSuggest(t, st, atLimit)
	wantNoErr(t, "边界值放行", err)
	wantEQ(t, "边界值放行", "len(Items)", len(reply.Items), 0)
	wantOps(t, "边界值调用序列", st.log.ops, []string{
		"block.IsBlocked:" + atLimit.Keyword,
		"cache.SuggestDict:500",
		"es.Search:0/25", // limit=10 -> size=10*2+5
		"cache.GetBlockSet",
		"block.ListActive:0/2000",
		"cache.SetBlockSet:0/60",
	})
}

// TestSuggestMergesHistoryDictAndEngineInWeightOrder 正常路径逐字段投影：
// 三个候选源合并、按规范化小写键去重、权重降序、来源标记与 from_history 一致。
func TestSuggestMergesHistoryDictAndEngineInWeightOrder(t *testing.T) {
	st := newStore(t, testConfig())
	// 用户历史：权重是最近搜索时间（越近越靠前），因此必然排在词典候选之前。
	recent := nowMinus(10)
	st.history.seed(&model.SearchHistory{Mid: 88, Keyword: "开源软件", State: model.HistoryStateNormal, Mtime: recent})
	// 已停用状态的历史不得下发。
	st.history.seed(&model.SearchHistory{Mid: 88, Keyword: "开源 tombstone", State: model.HistoryStateTombstone, Mtime: recent + 5})
	st.cache.warmBlockSet() // 屏蔽集走缓存命中：本用例只考察候选合并，不混入 DB 回源轨迹
	st.cache.warmDict(
		redis.FloatPair{Key: "开源软件", Score: 90},
		redis.FloatPair{Key: "开源硬件", Score: 80},
		redis.FloatPair{Key: "闭环开源", Score: 70}, // 前缀不匹配
		redis.FloatPair{Key: "开源社区", Score: 60},
	)

	req := suggestReq()
	req.ViewerMid = 88
	reply, err := doSuggest(t, st, req)
	wantNoErr(t, "联想合并", err)
	wantEQ(t, "联想合并", "Ttl（跟随结果缓存窗口）", reply.Ttl, int32(30))
	wantEQ(t, "联想合并", "len(Items)（limit=3，够数就不回源）", len(reply.Items), 3)
	wantStringsEQ(t, "联想合并", "候选词按权重降序", keywordsOf(reply.Items), []string{"开源软件", "开源硬件", "开源社区"})
	wantStringsEQ(t, "联想合并", "候选来源", sourcesOf(reply.Items),
		[]string{repository.SourceHistory, repository.SourceDict, repository.SourceDict})
	wantSliceEQ(t, "联想合并", "from_history 标记", fromHistoryOf(reply.Items), []bool{true, false, false})
	wantEQ(t, "联想合并", "历史权重=最近搜索时间", reply.Items[0].Weight, float64(recent))
	wantEQ(t, "联想合并", "词典权重=ZSET 分值", reply.Items[1].Weight, float64(80))
	// 上面那条精确列表本身就是 tombstone / 前缀不匹配两道过滤的断言：
	// 「开源 tombstone」的 mtime 比唯一的history 候选更大，若状态过滤失效应排第 1；
	// 「闭环开源」分值 70 落在 80 与 60 之间，若前缀过滤失效应插在第 3 位。

	// 顺序：历史 -> 词典 -> 出口屏蔽集；候选已够 limit，引擎一次都没被打。
	wantOps(t, "联想调用序列", st.log.ops, []string{
		"block.IsBlocked:开源",
		"history.ListByPrefix:88/开源/3",
		"cache.SuggestDict:500",
		"cache.GetBlockSet",
	})
	wantCount(t, "候选足够时不回源", st.log, "es.", 0)
}

// TestSuggestEngineFallbackBuildsPrefixQuery 候选不足时才回源引擎，
// 且回源用的是 title.keyword 的 prefix 子句（不解析查询语法）。
func TestSuggestEngineFallbackBuildsPrefixQuery(t *testing.T) {
	st := newStore(t, testConfig())
	st.cache.warmBlockSet()
	st.cache.warmDict(
		redis.FloatPair{Key: "开源软件", Score: 90},
		redis.FloatPair{Key: "开源硬件", Score: 80},
		redis.FloatPair{Key: "开源社区", Score: 60},
	)
	st.eng.resp = respWith(3, "eq", 4,
		hitOf("e1", 0, esclient.SourceDoc{DocType: model.DocTypeVideo, DocID: 1, Title: "开源  世界", HotScore: 50}, nil),
		hitOf("e2", 0, esclient.SourceDoc{DocType: model.DocTypeVideo, DocID: 2, Title: "   ", HotScore: 40}, nil),
		hitOf("e3", 0, esclient.SourceDoc{DocType: model.DocTypeVideo, DocID: 3, Title: "开源软件", HotScore: 55}, nil),
	)

	req := suggestReq()
	req.Limit = 4 // 词典只 3 条 -> 触发回源
	reply, err := doSuggest(t, st, req)
	wantNoErr(t, "回源补齐", err)
	wantStringsEQ(t, "回源补齐", "候选词", keywordsOf(reply.Items),
		[]string{"开源软件", "开源硬件", "开源社区", "开源 世界"})
	wantStringsEQ(t, "回源补齐", "候选来源", sourcesOf(reply.Items),
		[]string{repository.SourceDict, repository.SourceDict, repository.SourceDict, repository.SourceEngine})
	// 空标题被丢弃；与词典重名的引擎候选按「同权重比来源优先级」保留词典那条（权重 90 > 55）。
	wantEQ(t, "回源补齐", "len(Items)", len(reply.Items), 4)

	// 回源请求体：prefix 值 + doc_type 过滤 + 热度排序 + 不统计总数。
	body := st.eng.body(t, 1)
	wantJSONNum(t, "回源 DSL", "size（limit*2+5）", body["size"], 13)
	wantJSONNum(t, "回源 DSL", "from", body["from"], 0)
	wantEQ(t, "回源 DSL", "track_total_hits", body["track_total_hits"], false)
	prefix := nested(t, body, "query", "bool", "must").([]any)[0].(map[string]any)["prefix"].(map[string]any)
	wantEQ(t, "回源 DSL", "prefix 字段", any(sortedKeys(prefix)[0]), repository.FieldTitleKeyword)
	wantEQ(t, "回源 DSL", "prefix.value", prefix[repository.FieldTitleKeyword].(map[string]any)["value"], any("开源"))
	wantEQ(t, "回源 DSL", "prefix.case_insensitive",
		prefix[repository.FieldTitleKeyword].(map[string]any)["case_insensitive"], any(true))
	wantStringsEQ(t, "回源 DSL", "_source", toStringSlice(body["_source"]),
		[]string{repository.FieldTitle, repository.FieldHotScore, repository.FieldDocType})
	filters := nested(t, body, "query", "bool").(map[string]any)["filter"].([]any)
	wantEQ(t, "回源 DSL", "len(filter)", len(filters), 1)
	terms := filters[0].(map[string]any)["terms"].(map[string]any)
	wantStringsEQ(t, "回源 DSL", "terms 字段", sortedKeys(terms), []string{repository.FieldDocType})
	wantStringsEQ(t, "回源 DSL", "doc_type 集合（按搜索类型收窄）",
		toStringSlice(terms[repository.FieldDocType]), []string{model.DocTypeVideo})
	sorts := nested(t, body, "sort").([]any)
	wantEQ(t, "回源 DSL", "len(sort)", len(sorts), 2)
	if _, ok := sorts[0].(map[string]any)[repository.FieldHotScore]; !ok {
		t.Errorf("回源 DSL：第 1 项应为 hot_score，实际 %v", sorts[0])
	}
	wantStringsEQ(t, "回源 DSL", "第 2 项为 _score tie-break",
		sortedKeys(sorts[1].(map[string]any)), []string{"_score"})
	wantOps(t, "回源调用序列", st.log.ops, []string{
		"block.IsBlocked:开源",
		"cache.SuggestDict:500",
		"es.Search:0/13",
		"cache.GetBlockSet",
	})
}

// TestSuggestBlockedPrefixIsZeroCostSuccess 命中屏蔽前缀：不读词典、不回源、不读屏蔽集，
// 返回空列表成功（与「联想挂了」区分开：这是安全策略生效）。
func TestSuggestBlockedPrefixIsZeroCostSuccess(t *testing.T) {
	cases := []struct{ name, input string }{
		{"原文命中", "敏感前缀"},
		{"首尾空白归一后命中", "  敏感前缀 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每个用例一份全新 store：callLog 从 0 开始，「零成本」才是真断言。
			st := newStore(t, testConfig())
			st.block.seedWord("敏感前缀", model.BlockWordStateActive)
			st.cache.warmDict(redis.FloatPair{Key: "敏感前缀什么", Score: 99})

			req := suggestReq()
			req.Keyword = tc.input
			req.ViewerMid = 88
			reply, err := doSuggest(t, st, req)
			wantNoErr(t, "屏蔽前缀", err)
			wantEQ(t, "屏蔽前缀", "len(Items)（不透露词典内容）", len(reply.Items), 0)
			wantEQ(t, "屏蔽前缀", "Ttl（安全结果不缓存）", reply.Ttl, int32(0))
			wantOps(t, "屏蔽前缀调用序列", st.log.ops, []string{"block.IsBlocked:敏感前缀"})
		})
	}
}

// TestSuggestAppliesBlockSetAtExit 词典里已生效的屏蔽词必须在出口被剔掉：
// 屏蔽主链路（IsBlocked）只判整词，联想按前缀出词，两者口径不同，需各自设闸。
func TestSuggestAppliesBlockSetAtExit(t *testing.T) {
	st := newStore(t, testConfig())
	st.cache.warmDict(
		redis.FloatPair{Key: "开源软件", Score: 90},
		redis.FloatPair{Key: "开源违规词", Score: 85},
		redis.FloatPair{Key: "开源硬件", Score: 80},
	)
	st.cache.warmBlockSet("开源违规词")
	// 屏蔽集命中时不该再有 DB 回源。
	st.eng.resp = respWith(0, "eq", 1)

	req := suggestReq()
	reply, err := doSuggest(t, st, req)
	wantNoErr(t, "出口过滤", err)
	wantStringsEQ(t, "出口过滤", "候选词", keywordsOf(reply.Items), []string{"开源软件", "开源硬件"})
	// 注意：候选是否够 limit 的判定发生在出口过滤之前，因此屏蔽掉一个词不会触发回源补齐
	// —— 联想结果可以少于 limit，这是代码事实（本用例同时钉住「不多出词」与「不额外回源」）。
	wantOps(t, "屏蔽集命中时的调用序列", st.log.ops, []string{
		"block.IsBlocked:开源",
		"cache.SuggestDict:500",
		"cache.GetBlockSet",
	})
	wantCount(t, "候选已够 limit 时不回源", st.log, "es.", 0)

	// 屏蔽集未命中缓存：回源 DB 并写回缓存（TTL 用 BlockWordCacheTTLSeconds）。
	st2 := newStore(t, testConfig())
	st2.cache.warmDict(redis.FloatPair{Key: "开源软件", Score: 90})
	st2.block.seedWord("开源违规词", model.BlockWordStateActive)
	st2.block.seedWord("已放开的词", model.BlockWordStateInactive)
	st2.eng.resp = respWith(0, "eq", 1)
	_, err = doSuggest(t, st2, suggestReq())
	wantNoErr(t, "屏蔽集回源", err)
	wantOps(t, "屏蔽集回源调用序列", st2.log.ops, []string{
		"block.IsBlocked:开源",
		"cache.SuggestDict:500",
		"es.Search:0/11",
		"cache.GetBlockSet",
		"block.ListActive:0/2000",
		"cache.SetBlockSet:1/60", // 只装 active 词，inactive 不进缓存
	})
}

// TestSuggestFailureSemantics 词典/引擎/历史三侧的失败口径。
//
// 判断依据（钉代码事实）：repository.Suggest 只在「候选为空 且 词典与引擎都报错」时
// 返回 model.ErrSearchUnavailable（suggest.go 的 `len(out)==0 && dictErr!=nil && engineErr!=nil`）；
// 历史读失败只 logx（不进错误链）；logic 侧对 repo.Suggest 的错误一律 `return nil, err`。
// 因此「词典挂了但引擎有词」是部分成功，「两侧都挂」必须是错误，绝不返回空列表冒充成功。
func TestSuggestFailureSemantics(t *testing.T) {
	boom := errors.New("redis dict down")
	esBoom := errors.New("opensearch connection refused")

	cases := []struct {
		name      string
		dictFail  bool
		esFail    bool
		histFail  bool
		withHist  bool
		wantErr   error
		wantWords []string
	}{
		{"两侧都挂：明确错误", true, true, false, false, model.ErrSearchUnavailable, nil},
		{"词典挂、引擎可用：部分成功", true, false, false, false, nil, []string{"开源 世界"}},
		{"引擎挂、词典可用：部分成功", false, true, false, false, nil, []string{"开源软件"}},
		{"两侧都挂但历史可用：按历史返回", true, true, false, true, nil, []string{"开源历史词"}},
		{"历史读失败不影响联想", false, true, true, true, nil, []string{"开源软件"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			if !tc.dictFail {
				st.cache.warmDict(redis.FloatPair{Key: "开源软件", Score: 90})
			} else {
				st.cache.failWith("SuggestDict", boom)
			}
			if tc.esFail {
				st.eng.failWith("Search", esBoom)
			} else {
				st.eng.resp = respWith(1, "eq", 2, hitOf("e1", 0,
					esclient.SourceDoc{DocType: model.DocTypeVideo, DocID: 5, Title: "开源 世界", HotScore: 50}, nil))
			}
			if tc.histFail {
				st.history.failWith("ListByPrefix", errors.New("history db down"))
			}
			if tc.withHist {
				st.history.seed(&model.SearchHistory{Mid: 88, Keyword: "开源历史词", State: model.HistoryStateNormal, Mtime: 123})
			}

			req := suggestReq()
			req.Limit = 5 // 保证一定尝试回源
			req.ViewerMid = 88
			reply, err := doSuggest(t, st, req)
			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				wantCount(t, tc.name+"：失败不得返回半截列表", st.log, "cache.SetBlockSet", 0)
				return
			}
			wantNoErr(t, tc.name, err)
			wantStringsEQ(t, tc.name, "候选词", keywordsOf(reply.Items), tc.wantWords)
			wantEQ(t, tc.name, "Ttl 仍给配置窗口（成功就是成功）", reply.Ttl, int32(30))
		})
	}

	// 两侧都挂时的顺序：先读词典，再回源引擎，两处都失败才放弃（不回写屏蔽集）。
	st := newStore(t, testConfig())
	st.cache.failWith("SuggestDict", boom)
	st.eng.failWith("Search", esBoom)
	_, err := doSuggest(t, st, bad(suggestReq(), func(r *rpc.SuggestReq) { r.Limit = 5 }))
	wantErrIs(t, "两侧都挂", err, model.ErrSearchUnavailable)
	wantOps(t, "两侧都挂调用序列", st.log.ops, []string{
		"block.IsBlocked:开源",
		"cache.SuggestDict:500",
		"es.Search:0/15",
	})
}

// TestSuggestBlockCheckFailureIsHardError 屏蔽判定失败不得放行联想：
// 与 Search 同口径（安全判定未完成时不允许降级为「不过滤」）。
func TestSuggestBlockCheckFailureIsHardError(t *testing.T) {
	st := newStore(t, testConfig())
	boom := errors.New("block word db down")
	st.block.failWith("IsBlocked", boom)
	st.cache.warmDict(redis.FloatPair{Key: "开源软件", Score: 90})

	reply, err := doSuggest(t, st, suggestReq())
	wantErrIs(t, "屏蔽判定失败", err, boom)
	if reply != nil {
		t.Errorf("屏蔽判定失败：不应返回候选，实际 %+v", reply.Items)
	}
	wantOps(t, "调用序列", st.log.ops, []string{"block.IsBlocked:开源"})
}

// TestSuggestTTLFollowsCacheSwitch 关闭结果缓存时联想 ttl 归 0（客户端不该本地缓存）。
func TestSuggestTTLFollowsCacheSwitch(t *testing.T) {
	cfg := testConfig()
	cfg.Search.CacheTTLSeconds = 0
	st := newStore(t, cfg)
	st.cache.warmBlockSet()
	st.cache.warmDict(redis.FloatPair{Key: "开源软件", Score: 90})
	reply, err := doSuggest(t, st, suggestReq())
	wantNoErr(t, "关闭缓存的联想", err)
	wantEQ(t, "关闭缓存的联想", "Ttl", reply.Ttl, int32(0))
	wantEQ(t, "关闭缓存的联想", "候选仍返回", len(reply.Items), 1)
	// CacheTTLSeconds 只影响「结果缓存 + 下发给客户端的 ttl」：词典与屏蔽集是各自的缓存，
	// 关结果缓存不会让词典读不到（那由 SuggestCandidateWindow 决定），也不会跳过关词表。
	wantOps(t, "关闭结果缓存的调用序列", st.log.ops, []string{
		"block.IsBlocked:开源",
		"cache.SuggestDict:500",
		"es.Search:0/11",
		"cache.GetBlockSet",
	})
	wantNoOpsWith(t, "关闭缓存的联想", st.log, 0, "cache.GetResult", "cache.SetResult")
}

// --- 本文件专用小工具 ---

func keywordsOf(items []*rpc.SuggestItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Keyword)
	}
	return out
}

func sourcesOf(items []*rpc.SuggestItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Source)
	}
	return out
}

func fromHistoryOf(items []*rpc.SuggestItem) []bool {
	out := make([]bool, 0, len(items))
	for _, it := range items {
		out = append(out, it.FromHistory)
	}
	return out
}
