package logic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"go-video/services/search-query/internal/esclient"
	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

// searchReq 给一条「所有守卫都能过」的基准请求（各用例只改自己要的那一项）。
func searchReq() *rpc.SearchReq {
	return &rpc.SearchReq{
		Keyword:    "开源软件",
		SearchType: rpc.SearchType_SEARCH_TYPE_VIDEO,
		Sort:       rpc.SortMode_SORT_COMPREHENSIVE,
		Pn:         1,
		Ps:         20,
		Platform:   "android",
	}
}

func doSearch(t *testing.T, st *store, in *rpc.SearchReq) (*rpc.SearchReply, error) {
	t.Helper()
	return NewSearchLogic(context.Background(), st.svcCtx()).Search(in)
}

// TestSearchRejectsInvalidRequestsBeforeAnyDependency 参数守卫表：
// 每个坏输入都必须被拒，且**一次依赖调用都不许发生**（不打引擎、不打缓存、不打库）。
func TestSearchRejectsInvalidRequestsBeforeAnyDependency(t *testing.T) {
	longKeyword := strings.Repeat("词", repository.MaxKeywordRunes+1) // 规范化后仍超长
	ok := searchReq()

	cases := []struct {
		name  string
		req   *rpc.SearchReq
		want  error
		cause string
	}{
		{"空请求", nil, model.ErrInvalidKeyword, "in==nil 与空关键词同码，网关据此回参数错误"},
		{"空关键词", bad(ok, func(r *rpc.SearchReq) { r.Keyword = "" }), model.ErrInvalidKeyword, "关键词必填"},
		{"纯空白关键词", bad(ok, func(r *rpc.SearchReq) { r.Keyword = "  \t\n " }), model.ErrInvalidKeyword, "规范化后为空"},
		{"纯控制字符关键词", bad(ok, func(r *rpc.SearchReq) { r.Keyword = "\x01\x02\x7f" }), model.ErrInvalidKeyword, "清洗后为空，不允许把控制字符送进 DSL"},
		{"超长关键词", bad(ok, func(r *rpc.SearchReq) { r.Keyword = longKeyword }), model.ErrInvalidKeyword, "KeywordMaxLen/MaxKeywordRunes 兜底"},
		{"未知端", bad(ok, func(r *rpc.SearchReq) { r.Platform = "windows-phone" }), model.ErrInvalidPlatform, "端白名单（AGENTS.md §6）"},
		{"未知搜索类型", bad(ok, func(r *rpc.SearchReq) { r.SearchType = rpc.SearchType(99) }), model.ErrInvalidSearchType, "不猜语义"},
		{"未知时长档", bad(ok, func(r *rpc.SearchReq) { r.Duration = rpc.DurationBucket(99) }), model.ErrInvalidPage, "时长枚举封闭"},
		{"负发布时间下界", bad(ok, func(r *rpc.SearchReq) { r.PublishedAfter = -1 }), model.ErrInvalidPage, "时间戳不得为负"},
		{"负发布时间上界", bad(ok, func(r *rpc.SearchReq) { r.PublishedBefore = -5 }), model.ErrInvalidPage, "时间戳不得为负"},
		{"时间窗倒置", bad(ok, func(r *rpc.SearchReq) {
			r.PublishedAfter, r.PublishedBefore = 2_000_000_000, 1_900_000_000
		}), model.ErrInvalidPage, "after>before 必然零命中，直接拒"},
		{"未知排序", bad(ok, func(r *rpc.SearchReq) { r.Sort = rpc.SortMode(99) }), model.ErrInvalidSort, "排序枚举封闭"},
		{"粉丝排序用于非用户", bad(ok, func(r *rpc.SearchReq) {
			r.SearchType = rpc.SearchType_SEARCH_TYPE_VIDEO
			r.Sort = rpc.SortMode_SORT_MOST_FANS
		}), model.ErrInvalidSort, "静默退化排序会让用户以为筛过"},
		{"游标不是合法编码", bad(ok, func(r *rpc.SearchReq) {
			r.Cursor = "not-a-cursor!!"
		}), model.ErrInvalidCursor, "base64url/JSON 解不开"},
		{"游标属于别的查询", bad(ok, func(r *rpc.SearchReq) {
			r.Cursor = mustEncodeCursor(t, 20, "ffffffffffffffff") // 指纹对不上
		}), model.ErrCursorMismatch, "翻页改条件必须报错而不是串页"},
		{"游标版本不兼容", bad(ok, func(r *rpc.SearchReq) {
			r.Cursor = rawCursor(t, 9)
		}), repository.ErrUnsupportedCursorVersion, "滚动发布期的新旧版本"},
		{"游标 offset 为负", bad(ok, func(r *rpc.SearchReq) {
			r.Cursor = mustEncodeCursor(t, -1, "")
		}), model.ErrInvalidCursor, "负 offset 不是合法翻页"},
		{"负页码", bad(ok, func(r *rpc.SearchReq) { r.Pn = -1 }), model.ErrInvalidPage, "pn 从 1 开始"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.OpenSearch.MaxResultWindow = 10000
			st := newStore(t, cfg)
			reply, err := doSearch(t, st, tc.req)
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：被拒的请求不应返回响应体，实际 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, st, 0)
		})
	}
}

// TestSearchDeepPageRejectionDoesNotConsumeCacheOrEngine 深分页拒绝发生在读写缓存与引擎之前，
// 但仍会先做一次屏蔽词判定（实现事实：Search 的屏蔽词检查在 repo.Search 之前）。
func TestSearchDeepPageRejectionDoesNotConsumeCacheOrEngine(t *testing.T) {
	cfg := testConfig()
	cfg.Search.MaxOffset = 900
	st := newStore(t, cfg)

	req := searchReq()
	req.Pn = 32 // offset = 31*30 = 930 > MaxOffset=900
	req.Ps = 30
	reply, err := doSearch(t, st, req)
	wantErrIs(t, "深分页", err, model.ErrDeepPage)
	if reply != nil {
		t.Errorf("深分页：被拒的请求不应返回响应体，实际 %+v", reply)
	}
	wantOps(t, "深分页调用序列", st.log.ops, []string{"block.IsBlocked:开源软件"})
	wantNoOpsWith(t, "深分页不得消耗缓存/引擎配额", st.log, 0, "cache.", "es.")
}

// TestSearchProjectsEveryFieldAndCarriesNormalizedKeyword 正常路径逐字段投影：
// 命中列表、总数、耗时、翻页信息、DSL 请求体与关键词规范化一次锁死。
func TestSearchProjectsEveryFieldAndCarriesNormalizedKeyword(t *testing.T) {
	st := newStore(t, testConfig())
	videoSrc := esclient.SourceDoc{
		DocType: model.DocTypeVideo, DocID: 10001, Title: "标题A", Intro: "简介A",
		AuthorMid: 2002, AuthorName: "UP主A", ZoneID: 17, ZoneName: "科技",
		CoverURL: "https://cover.example/a.jpg", ViewCount: 11, LikeCount: 12,
		DanmakuCount: 13, FansCount: 14, DurationSec: 150, PubTime: 1_700_000_000,
		HotScore: 3.5, State: "published", Tags: []string{"t1", "t2"},
	}
	userSrc := esclient.SourceDoc{
		DocType: model.DocTypeUser, DocID: 3003, Title: "UP主B", Intro: "个人签名",
		AuthorMid: 3003, AuthorName: "UP主B", FansCount: 999, State: "published",
	}
	st.eng.resp = respWith(1234, "eq", 27,
		hitOf("engine-phys-id-A", 9.5, videoSrc, map[string][]string{
			repository.FieldTitle: {"<em>开源</em>软件"},
			repository.FieldIntro: {"片断1", "片断2"},
			repository.FieldTags:  {"t1", "t2"},
			"empty_field":         {},
		}),
		hitOf("engine-phys-id-B", 8.5, userSrc, nil),
	)

	req := searchReq()
	req.Keyword = "  开源   软件 \x07 " // 前后空白 + 内部多空格 + 控制字符
	req.SearchType = rpc.SearchType_SEARCH_TYPE_ALL
	req.Pn = 3
	req.Ps = 0 // 未传 -> PsDefault
	req.AppVersion = "1.2.3"
	reply, err := doSearch(t, st, req)
	wantNoErr(t, "正常搜索", err)

	wantEQ(t, "总数", "Total", reply.Total, int64(1234))
	wantEQ(t, "页大小", "Ps", reply.Ps, int32(30))
	wantEQ(t, "页码（offset 60/ps 30 反解）", "Pn", reply.Pn, int32(3))
	wantEQ(t, "非缓存命中", "CacheHit", reply.CacheHit, false)
	wantEQ(t, "未降级时用服务端缓存窗口", "Ttl", reply.Ttl, int32(30))
	wantEQ(t, "未命中屏蔽词", "SafeFiltered", reply.SafeFiltered, false)
	wantEQ(t, "命中条数", "len(Hits)", len(reply.Hits), 2)

	h0 := reply.Hits[0]
	wantEQ(t, "首条", "DocType", h0.DocType, model.DocTypeVideo)
	wantEQ(t, "首条", "DocId", h0.DocId, int64(10001)) // 取 source.doc_id，不是引擎物理 _id
	wantEQ(t, "首条", "Title", h0.Title, "<em>开源</em>软件")
	wantEQ(t, "首条", "ContentSnippet", h0.ContentSnippet, "片断1 … 片断2")
	wantEQ(t, "首条", "AuthorMid", h0.AuthorMid, int64(2002))
	wantEQ(t, "首条", "AuthorName", h0.AuthorName, "UP主A")
	wantEQ(t, "首条", "ZoneId", h0.ZoneId, int32(17))
	wantEQ(t, "首条", "CoverUrl", h0.CoverUrl, "https://cover.example/a.jpg")
	wantEQ(t, "首条", "ViewCount", h0.ViewCount, int64(11))
	wantEQ(t, "首条", "LikeCount", h0.LikeCount, int64(12))
	wantEQ(t, "首条", "DanmakuCount", h0.DanmakuCount, int64(13))
	wantEQ(t, "首条", "FansCount", h0.FansCount, int64(14))
	wantEQ(t, "首条", "DurationSec", h0.DurationSec, int32(150))
	wantEQ(t, "首条", "PubTime", h0.PubTime, int64(1_700_000_000))
	wantEQ(t, "首条", "Score", h0.Score, 9.5)
	wantEQ(t, "首条", "State", h0.State, "published")
	// 内部字段不外泄：highlights 只保留 title/intro 之外的字段，空片段被丢掉。
	wantEQ(t, "首条", "len(Highlights)", len(h0.Highlights), 1)
	wantEQ(t, "首条", "Highlights[tags]", h0.Highlights[repository.FieldTags], "t1 … t2")
	if _, leaked := h0.Highlights[repository.FieldTitle]; leaked {
		t.Errorf("首条：title 不得重复出现在 highlights 里（已映射到 Title）")
	}
	if _, leaked := h0.Highlights[repository.FieldIntro]; leaked {
		t.Errorf("首条：intro 不得重复出现在 highlights 里（已映射到 ContentSnippet）")
	}
	if _, leaked := h0.Highlights["empty_field"]; leaked {
		t.Errorf("首条：空片段字段不得下发")
	}
	if strings.Contains(jsonDump(t, reply), "engine-phys-id") {
		t.Errorf("投影：引擎物理文档 ID 不该出现在响应里（客户端只见 doc_id）")
	}
	h1 := reply.Hits[1]
	wantEQ(t, "次条", "DocType", h1.DocType, model.DocTypeUser)
	wantEQ(t, "次条", "DocId", h1.DocId, int64(3003))
	wantEQ(t, "次条", "Title（无高亮时原样）", h1.Title, "UP主B")
	wantEQ(t, "次条", "ContentSnippet（取 intro）", h1.ContentSnippet, "个人签名")
	wantEQ(t, "次条", "Score", h1.Score, 8.5)
	wantEQ(t, "次条", "len(Highlights)", len(h1.Highlights), 0)
	wantEQ(t, "末页无游标", "NextCursor", reply.NextCursor, "")
	wantEQ(t, "末页", "HasMore", reply.HasMore, false)

	// DSL 请求体：规范化后的关键词只出现在 value 位置，筛选/投影/超时按配置拼接。
	body := st.eng.body(t, 1)
	wantJSONNum(t, "DSL", "from", body["from"], 60)
	wantJSONNum(t, "DSL", "size（PsDefault）", body["size"], 30)
	wantJSONNum(t, "DSL", "track_total_hits（引擎窗口）", body["track_total_hits"], 10000)
	wantEQ(t, "DSL", "timeout（HTTP 超时的 8 成）", body["timeout"], "1200ms")
	wantEQ(t, "DSL", "query.bool.must[0].multi_match.query",
		nested(t, body, "query", "bool", "must").([]any)[0].(map[string]any)["multi_match"].(map[string]any)["query"],
		"开源 软件")
	wantStringsEQ(t, "DSL", "_source", toStringSlice(body["_source"]), repository.DefaultSourceFields)
	filters := nested(t, body, "query", "bool").(map[string]any)["filter"].([]any)
	wantEQ(t, "DSL", "len(filter)", len(filters), 1) // ALL 类型只带 doc_type 过滤
	terms := filters[0].(map[string]any)["terms"].(map[string]any)[repository.FieldDocType].([]any)
	wantStringsEQ(t, "DSL", "doc_type terms", toStringSlice(terms),
		[]string{model.DocTypeVideo, model.DocTypeUser, model.DocTypePGC})
	hl := nested(t, body, "highlight").(map[string]any)
	wantStringsEQ(t, "DSL", "highlight.pre_tags", toStringSlice(hl["pre_tags"]), []string{"<em>"})
	wantStringsEQ(t, "DSL", "highlight.post_tags", toStringSlice(hl["post_tags"]), []string{"</em>"})
	// 别名决定查哪个索引：请求体里不得出现裸索引名（别名切换期会读到半成品索引）。
	if strings.Contains(st.eng.bodies[0], "go_video_search_v1") {
		t.Errorf("DSL：请求体不得写死物理索引名，应只用别名 %s", st.eng.alias)
	}

	// 屏蔽词判定与缓存写入都使用规范化后的关键词/折算后的 offset。
	key := wantResultCacheOps(t, st, 60)
	wantEQ(t, "结果缓存命名空间", "key 前缀", strings.HasPrefix(key, "sq:res:v1:"), true)
}

// TestSearchFiltersAndSortReachEngine 筛选/排序枚举到 DSL 的映射（逐档一次，锁死区间语义）。
func TestSearchFiltersAndSortReachEngine(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(r *rpc.SearchReq)
		wantOps []string
		assert  func(t *testing.T, body map[string]any)
	}{
		{
			name:   "时长档 1-10min 用 gte+lt",
			mutate: func(r *rpc.SearchReq) { r.Duration = rpc.DurationBucket_DURATION_1_10MIN },
			assert: func(t *testing.T, body map[string]any) {
				dr := rangeOf(t, body, repository.FieldDuration)
				wantJSONNum(t, "时长", "gte", dr["gte"], 60)
				wantJSONNum(t, "时长", "lt", dr["lt"], 600)
			},
		},
		{
			name:   "时长档 >60min 只有下界",
			mutate: func(r *rpc.SearchReq) { r.Duration = rpc.DurationBucket_DURATION_GT_60MIN },
			assert: func(t *testing.T, body map[string]any) {
				dr := rangeOf(t, body, repository.FieldDuration)
				wantJSONNum(t, "时长", "gte", dr["gte"], 3600)
				if _, has := dr["lt"]; has {
					t.Errorf("时长：无上界时不得写 lt")
				}
			},
		},
		{
			name:   "发布时间窗用 gte+lte（含端点）",
			mutate: func(r *rpc.SearchReq) { r.PublishedAfter, r.PublishedBefore = 1_700_000_000, 1_800_000_000 },
			assert: func(t *testing.T, body map[string]any) {
				pr := rangeOf(t, body, repository.FieldPubTime)
				wantJSONNum(t, "发布", "gte", pr["gte"], 1_700_000_000)
				wantJSONNum(t, "发布", "lte", pr["lte"], 1_800_000_000)
			},
		},
		{
			name:   "分区筛选写进 term",
			mutate: func(r *rpc.SearchReq) { r.ZoneId = 158 },
			assert: func(t *testing.T, body map[string]any) {
				term := termOf(t, body, repository.FieldZoneID)
				wantJSONNum(t, "分区", "zone_id", term, 158)
			},
		},
		{
			name:   "最新排序只有 pub_time 倒序",
			mutate: func(r *rpc.SearchReq) { r.Sort = rpc.SortMode_SORT_LATEST },
			assert: func(t *testing.T, body map[string]any) {
				sorts := nested(t, body, "sort").([]any)
				wantEQ(t, "排序", "len(sort)", len(sorts), 1)
				wantEQ(t, "排序", "pub_time.order",
					sorts[0].(map[string]any)[repository.FieldPubTime].(map[string]any)["order"], "desc")
			},
		},
		{
			name:   "综合排序先相关性再时间",
			mutate: func(r *rpc.SearchReq) { r.Sort = rpc.SortMode_SORT_COMPREHENSIVE },
			assert: func(t *testing.T, body map[string]any) {
				sorts := nested(t, body, "sort").([]any)
				wantEQ(t, "排序", "len(sort)", len(sorts), 2)
				if _, ok := sorts[0].(map[string]any)["_score"]; !ok {
					t.Errorf("排序：第 1 项应为 _score，实际 %v", sorts[0])
				}
				if _, ok := sorts[1].(map[string]any)[repository.FieldPubTime]; !ok {
					t.Errorf("排序：第 2 项应为 pub_time，实际 %v", sorts[1])
				}
			},
		},
		{
			name: "粉丝排序只作用于用户搜索",
			mutate: func(r *rpc.SearchReq) {
				r.SearchType = rpc.SearchType_SEARCH_TYPE_USER
				r.Sort = rpc.SortMode_SORT_MOST_FANS
			},
			assert: func(t *testing.T, body map[string]any) {
				sorts := nested(t, body, "sort").([]any)
				wantEQ(t, "排序", "len(sort)", len(sorts), 1)
				if _, ok := sorts[0].(map[string]any)[repository.FieldFansCount]; !ok {
					t.Errorf("排序：应为 fans_count，实际 %v", sorts[0])
				}
			},
		},
		{
			name:   "未传排序回落到配置默认（热度分）",
			mutate: func(r *rpc.SearchReq) { r.Sort = rpc.SortMode_SORT_UNSPECIFIED },
			assert: func(t *testing.T, body map[string]any) {
				sorts := nested(t, body, "sort").([]any)
				if _, ok := sorts[0].(map[string]any)[repository.FieldHotScore]; !ok {
					t.Errorf("排序：默认值应取 cfg.DefaultSort=HOT_SCORE，实际 %v", sorts[0])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Search.DefaultSort = int32(rpc.SortMode_SORT_HOT_SCORE)
			st := newStore(t, cfg)
			req := searchReq()
			tc.mutate(req)
			_, err := doSearch(t, st, req)
			wantNoErr(t, tc.name, err)
			tc.assert(t, st.eng.body(t, 1))
		})
	}
}

// TestSearchCursorPaginationAndWindowCeiling 翻页游标：只在下发窗口内给游标，
// 且游标内嵌指纹（改筛选条件即 ErrCursorMismatch），pn 由 offset 反解。
func TestSearchCursorPaginationAndWindowCeiling(t *testing.T) {
	st := newStore(t, testConfig())
	doc := func(i int) esclient.Hit {
		return hitOf(fmt.Sprintf("e-%d", i), float64(10-i), esclient.SourceDoc{
			DocType: model.DocTypeVideo, DocID: int64(2000 + i), Title: fmt.Sprintf("片头%d", i),
		}, nil)
	}
	st.eng.resp = respWith(5, "eq", 9, doc(0), doc(1))

	req := searchReq()
	req.Ps = 2
	req.Pn = 1
	page1, err := doSearch(t, st, req)
	wantNoErr(t, "第一页", err)
	wantEQ(t, "第一页", "len(Hits)", len(page1.Hits), 2)
	wantEQ(t, "第一页", "HasMore（hits==ps 且 next<total）", page1.HasMore, true)
	wantEQ(t, "第一页", "Pn", page1.Pn, int32(1))
	if page1.NextCursor == "" {
		t.Fatalf("第一页：应下发下一页游标")
	}
	fp := st.cache.lastFingerprint(t)
	off, derr := repository.DecodeOffsetCursor(page1.NextCursor, fp)
	wantNoErr(t, "游标可解码", derr)
	wantEQ(t, "游标 offset 段", "offset", off, int64(2))

	// 用返回的游标继续翻：引擎收到 from=2，pn 反解为 2。
	before := st.log.snapshot()
	req.Cursor = page1.NextCursor
	req.Pn = 99 // cursor 优先：pn 被忽略
	page2, err := doSearch(t, st, req)
	wantNoErr(t, "第二页", err)
	wantEQ(t, "第二页", "Pn（由游标反解）", page2.Pn, int32(2))
	wantOpsFrom(t, "第二页调用序列", st.log, before, []string{
		"block.IsBlocked:开源软件",
		"cache.GetResult:" + repository.ResultKey(fp, 2),
		"es.Search:2/2",
		"cache.SetResult:" + repository.ResultKey(fp, 2) + "/30",
	})
	wantEQ(t, "第二页", "HasMore（total=5、next=4<5、hits 满页）", page2.HasMore, true)

	// 翻页途中改筛选条件：指纹变化，必须报错而不是串页。
	_, err = doSearch(t, st, func() *rpc.SearchReq {
		r := searchReq()
		r.Ps = 2
		r.Cursor = page1.NextCursor
		r.ZoneId = 158
		return r
	}())
	wantErrIs(t, "游标与新条件", err, model.ErrCursorMismatch)

	// 改页大小同样算改条件：游标里绑的是「关键词+筛选+排序+ps」指纹。
	_, err = doSearch(t, st, func() *rpc.SearchReq {
		r := searchReq()
		r.Ps = 3
		r.Cursor = page1.NextCursor
		return r
	}())
	wantErrIs(t, "游标与页大小", err, model.ErrCursorMismatch)

	// 最后一页：hits 不足 ps -> 不给游标（用新 offset 4，避免被第二页写下的缓存命中）。
	st.eng.resp = respWith(5, "eq", 9, doc(0))
	before = st.log.snapshot()
	last, err := doSearch(t, st, func() *rpc.SearchReq {
		r := searchReq()
		r.Ps = 2
		r.Pn = 3 // offset = 4
		return r
	}())
	wantNoErr(t, "末页", err)
	wantEQ(t, "末页", "HasMore", last.HasMore, false)
	wantEQ(t, "末页", "NextCursor", last.NextCursor, "")
	wantEQ(t, "末页", "Pn", last.Pn, int32(3))
	wantOpsFrom(t, "末页调用序列", st.log, before, []string{
		"block.IsBlocked:开源软件",
		"cache.GetResult:" + repository.ResultKey(fp, 4),
		"es.Search:4/2",
		"cache.SetResult:" + repository.ResultKey(fp, 4) + "/30",
	})

	// offset 达到 MaxOffset 边界：这一页仍可服务，但不再下发会落进拒绝区的游标。
	full := make([]esclient.Hit, 0, 50)
	for i := 0; i < 50; i++ {
		full = append(full, doc(i))
	}
	st.eng.resp = respWith(100_000, "eq", 9, full...)
	edge, err := doSearch(t, st, func() *rpc.SearchReq {
		r := searchReq()
		r.Ps = 50 // = PsLimit
		r.Pn = 19 // offset = 18*50 = 900 = MaxOffset，仍合法
		return r
	}())
	wantNoErr(t, "MaxOffset 边界页", err)
	wantEQ(t, "MaxOffset 边界页", "Pn", edge.Pn, int32(19))
	wantEQ(t, "MaxOffset 边界页：next=950 已越界", "HasMore（不引导客户端翻到拒绝区）", edge.HasMore, false)
	wantEQ(t, "MaxOffset 边界页", "NextCursor", edge.NextCursor, "")
	wantEQ(t, "MaxOffset 边界页", "len(Hits)", len(edge.Hits), 50)
}

// TestSearchHasMoreDecision 「还有下一页」的四个判定分支各用一把新锁存器，
// 避免上一页写下的结果缓存掩盖真实分支。
func TestSearchHasMoreDecision(t *testing.T) {
	doc := func(i int) esclient.Hit {
		return hitOf(fmt.Sprintf("e-%d", i), float64(10-i), esclient.SourceDoc{
			DocType: model.DocTypeVideo, DocID: int64(2000 + i), Title: fmt.Sprintf("片头%d", i),
		}, nil)
	}

	cases := []struct {
		name       string
		resp       *esclient.SearchResponse
		ps, pn     int32
		wantMore   bool
		wantOffset int64 // 期望写进游标的下一页 offset（wantMore=false 时忽略）
	}{
		{"满页且 next<total -> 给游标", respWith(5, "eq", 9, doc(0), doc(1)), 2, 1, true, 2},
		{"满页但 next>=total -> 不给", respWith(5, "eq", 9, doc(0), doc(1)), 2, 3, false, 0},
		{"缺页（hits<ps）-> 不给", respWith(5, "eq", 9, doc(0)), 2, 2, false, 0},
		{"引擎 total=0 却回满页 -> 仍给游标（不信任 total 的兜底分支）",
			respWith(0, "eq", 9, doc(0), doc(1)), 2, 1, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			st.eng.resp = tc.resp
			reply, err := doSearch(t, st, func() *rpc.SearchReq {
				r := searchReq()
				r.Ps = tc.ps
				r.Pn = tc.pn
				return r
			}())
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "HasMore", reply.HasMore, tc.wantMore)
			if !tc.wantMore {
				wantEQ(t, tc.name, "NextCursor", reply.NextCursor, "")
				return
			}
			if reply.NextCursor == "" {
				t.Fatalf("%s：应下发下一页游标", tc.name)
			}
			off, derr := repository.DecodeOffsetCursor(reply.NextCursor, st.cache.lastFingerprint(t))
			wantNoErr(t, tc.name+"：游标可解码", derr)
			wantEQ(t, tc.name, "游标 offset 段", off, tc.wantOffset)
			wantEQ(t, tc.name, "游标可回读同一页", reply.Pn, tc.pn)
		})
	}
}

// TestSearchPagingCeilings 两条深分页上限各自生效，且都在读写缓存与引擎之前完成
// （docs/api-and-events.md §2）。另外锁死 ps 的归一化：超限截断、未传取默认。
func TestSearchPagingCeilings(t *testing.T) {
	t.Run("业务上限 MaxOffset 拒绝越界翻页", func(t *testing.T) {
		cfg := testConfig()
		cfg.Search.MaxOffset = 900
		st := newStore(t, cfg)
		_, err := doSearch(t, st, func() *rpc.SearchReq {
			r := searchReq()
			r.Ps = 30
			r.Pn = 32 // offset = 31*30 = 930 > 900
			return r
		}())
		wantErrIs(t, "MaxOffset", err, model.ErrDeepPage)
		wantOps(t, "MaxOffset 拒绝的调用序列", st.log.ops, []string{"block.IsBlocked:开源软件"})
	})

	t.Run("引擎窗口在 MaxOffset 关闭时兜底", func(t *testing.T) {
		cfg := testConfig()
		cfg.Search.MaxOffset = 0 // 业务侧不限，只靠引擎窗口
		st := newStore(t, cfg)
		st.eng.window = 10000
		_, err := doSearch(t, st, func() *rpc.SearchReq {
			r := searchReq()
			r.Ps = 30
			r.Pn = 334 // offset = 9990, 9990+30 > 10000
			return r
		}())
		wantErrIs(t, "引擎窗口", err, model.ErrDeepPage)
		wantOps(t, "引擎窗口拒绝的调用序列", st.log.ops, []string{"block.IsBlocked:开源软件"})
	})

	t.Run("两条上限都关闭时深翻页只受引擎约束", func(t *testing.T) {
		cfg := testConfig()
		cfg.Search.MaxOffset = 0
		st := newStore(t, cfg)
		st.eng.window = 0 // 引擎未报告窗口：不裁剪，DSL 也不写 track_total_hits
		reply, err := doSearch(t, st, func() *rpc.SearchReq {
			r := searchReq()
			r.Ps = 30
			r.Pn = 334 // offset = 9990
			return r
		}())
		wantNoErr(t, "无窗口上限的深翻页", err)
		wantEQ(t, "无窗口上限", "Pn", reply.Pn, int32(334))
		wantOps(t, "调用序列", st.log.ops, []string{
			"block.IsBlocked:开源软件",
			"cache.GetResult:" + repository.ResultKey(st.cache.lastFingerprint(t), 9990),
			"es.Search:9990/30",
			"cache.SetResult:" + repository.ResultKey(st.cache.lastFingerprint(t), 9990) + "/30",
		})
		wantEQ(t, "无窗口上限", "track_total_hits", st.eng.body(t, 1)["track_total_hits"], false)
	})

	t.Run("页大小归一化", func(t *testing.T) {
		st := newStore(t, testConfig())
		reply, err := doSearch(t, st, func() *rpc.SearchReq {
			r := searchReq()
			r.Ps = 500 // 超 PsLimit=50 -> 截断，不是报错
			r.Pn = 2
			return r
		}())
		wantNoErr(t, "ps 超限", err)
		wantEQ(t, "ps 超限", "Ps（截断到 PsLimit）", reply.Ps, int32(50))
		wantEQ(t, "ps 超限", "Pn（offset 50/ps 50）", reply.Pn, int32(2))
		wantJSONNum(t, "ps 超限", "引擎 size", st.eng.body(t, 1)["size"], 50)

		st2 := newStore(t, testConfig())
		reply2, err := doSearch(t, st2, func() *rpc.SearchReq {
			r := searchReq()
			r.Ps = -1 // 非法值不报错，按未传处理取 PsDefault
			r.Pn = 0  // 未传按 1
			return r
		}())
		wantNoErr(t, "ps 为负", err)
		wantEQ(t, "ps 为负", "Ps（回落 PsDefault）", reply2.Ps, int32(30))
		wantEQ(t, "ps 为负", "Pn（pn=0 视为 1）", reply2.Pn, int32(1))
	})
}

// TestSearchKeywordNormalizationReachesEverySide 关键词规范化的口径必须一致：
// 送去屏蔽词判定、进 DSL、落历史用的必须是同一个字符串（否则缓存/历史/安全三者会分叉）。
func TestSearchKeywordNormalizationReachesEverySide(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"折叠内部空白", "开源   软件", "开源 软件"},
		{"裁剪两端", "\t 开源软件 \n", "开源软件"},
		{"尖括号换成空格（阻断富文本注入）", "开源<em>软件</em>", "开源 em 软件 /em"},
		{"控制字符直接删除", "开源\x00\x0b\x1f软件", "开源软件"},
		{"不换行空格当普通空格", "\u00a0开源\u00a0\u00a0软件\u00a0", "开源 软件"},
		{"全角符号保留", "＜开源＞", "＜开源＞"},
		// 换行/制表是控制字符：被删除而不是折叠成空格（与「两端裁剪」那条不冲突，
		// 那里两端的 \t\n 是被裁剪掉的空白）。
		{"词中换行直接粘连", "a\n\n\nb", "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			st.eng.resp = respWith(1, "eq", 3, hitOf("e1", 2, esclient.SourceDoc{
				DocType: model.DocTypeVideo, DocID: 7, Title: "T"}, nil))
			req := searchReq()
			req.Keyword = tc.in
			req.ViewerMid = 88
			reply, err := doSearch(t, st, req)
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "Ps 未被规范化影响", reply.Ps, int32(20))

			body := st.eng.body(t, 1)
			mm := nested(t, body, "query", "bool", "must").([]any)[0].(map[string]any)["multi_match"].(map[string]any)
			wantEQ(t, tc.name, "DSL multi_match.query", mm["query"], any(tc.want))
			wantStringsEQ(t, tc.name, "DSL multi_match.fields", toStringSlice(mm["fields"]), repository.DefaultMatchFields)
			wantEQ(t, tc.name, "DSL multi_match.type", mm["type"], "best_fields")

			// 用户输入只能作为 JSON 字符串值进入请求体：顶层键集合不得被撑出新字段。
			wantSliceEQ(t, tc.name, "DSL 顶层键", sortedKeys(body),
				[]string{"_source", "from", "highlight", "query", "size", "sort", "timeout", "track_total_hits"})

			wantOps(t, tc.name+"：规范化词贯穿三处", st.log.ops, []string{
				"block.IsBlocked:" + tc.want,
				"cache.GetResult:" + repository.ResultKey(st.cache.lastFingerprint(t), 0),
				"es.Search:0/20",
				"cache.SetResult:" + repository.ResultKey(st.cache.lastFingerprint(t), 0) + "/30",
				"history.Upsert:88/" + tc.want,
				"history.Prune:88/300",
			})
			row, ok := st.history.get(88, tc.want)
			if !ok {
				t.Fatalf("历史未按规范化词入库（现有 %v）", st.history.rows)
			}
			wantEQ(t, tc.name, "历史 keyword_hash 对应规范化词", row.KeywordHash, model.KeywordHash(tc.want))
		})
	}
}

// TestSearchBlockedKeywordIsTheOnlySuccessfulZeroResult 命中屏蔽词：不查引擎、不写缓存，
// 返回 safe_filtered=true 且 ttl=0（唯一「成功且零结果」的场景）。
func TestSearchBlockedKeywordIsTheOnlySuccessfulZeroResult(t *testing.T) {
	newSt := func() *store {
		st := newStore(t, testConfig())
		st.block.seedWord("敏感词", model.BlockWordStateActive)
		st.block.seedWord("已停用词", model.BlockWordStateInactive)
		st.eng.resp = respWith(7, "eq", 3,
			hitOf("e1", 1, esclient.SourceDoc{DocType: model.DocTypeVideo, DocID: 1, Title: "T"}, nil))
		return st
	}

	// 原词与被规范化后的词都必须命中同一道安全闸（判定用的是规范化词）。
	for _, in := range []string{"敏感词", "  敏感词\x07 ", "\u00a0敏感词\u00a0"} {
		st := newSt()
		reply, err := doSearch(t, st, func() *rpc.SearchReq {
			r := searchReq()
			r.Keyword = in
			r.ViewerMid = 88
			return r
		}())
		wantNoErr(t, "屏蔽词 "+in, err)
		wantEQ(t, "屏蔽词", "SafeFiltered", reply.SafeFiltered, true)
		wantEQ(t, "屏蔽词", "Total（不暴露命中数量）", reply.Total, int64(0))
		wantEQ(t, "屏蔽词", "len(Hits)", len(reply.Hits), 0)
		wantEQ(t, "屏蔽词", "Ttl", reply.Ttl, int32(0))
		wantEQ(t, "屏蔽词", "CacheHit", reply.CacheHit, false)
		wantEQ(t, "屏蔽词", "HasMore", reply.HasMore, false)
		wantEQ(t, "屏蔽词", "NextCursor", reply.NextCursor, "")
		wantEQ(t, "屏蔽词", "Pn", reply.Pn, int32(1))
		wantEQ(t, "屏蔽词", "Ps", reply.Ps, int32(20))
		// 顺序：只有一次屏蔽词判定，之后什么都不许发生（缓存/引擎/历史全零）。
		wantOps(t, "屏蔽词调用序列", st.log.ops, []string{"block.IsBlocked:敏感词"})
	}

	// state 门槛：inactive 的词不拦，走正常链路（证明拦不拦看 state，不是看词表非空）。
	st := newSt()
	okReply, err := doSearch(t, st, func() *rpc.SearchReq {
		r := searchReq()
		r.Keyword = "已停用词"
		r.ViewerMid = 88
		return r
	}())
	wantNoErr(t, "停用词", err)
	wantEQ(t, "停用词", "SafeFiltered", okReply.SafeFiltered, false)
	wantEQ(t, "停用词", "Total", okReply.Total, int64(7))
	wantEQ(t, "停用词", "Ttl", okReply.Ttl, int32(30))
	wantOps(t, "停用词调用序列", st.log.ops, []string{
		"block.IsBlocked:已停用词",
		"cache.GetResult:" + repository.ResultKey(st.cache.lastFingerprint(t), 0),
		"es.Search:0/20",
		"cache.SetResult:" + repository.ResultKey(st.cache.lastFingerprint(t), 0) + "/30",
		"history.Upsert:88/已停用词",
		"history.Prune:88/300",
	})
}

// TestSearchBlockCheckFailureIsHardError 屏蔽词表读不到时必须硬失败：
// 安全判定无法完成时不允许放行查询（README「缓存与降级」补充事实）。
func TestSearchBlockCheckFailureIsHardError(t *testing.T) {
	st := newStore(t, testConfig())
	boom := errors.New("block word db down")
	st.block.failWith("IsBlocked", boom)

	reply, err := doSearch(t, st, searchReq())
	if reply != nil {
		t.Errorf("屏蔽判定失败：不应返回结果，实际 %+v", reply)
	}
	wantErrIs(t, "屏蔽判定失败", err, boom)
	wantEQ(t, "屏蔽判定失败", "错误未被改写成哨兵", errors.Is(err, model.ErrSearchUnavailable), false)
	wantCount(t, "屏蔽判定失败不得继续查询", st.log, "es.", 0)
	wantCount(t, "屏蔽判定失败不得读缓存", st.log, "cache.", 0)
	wantOps(t, "调用序列", st.log.ops, []string{"block.IsBlocked:开源软件"})
}

// TestSearchEngineFailureNeverLooksLikeEmptySuccess 下游失败的传播与降级口径。
//
// 判断依据（钉代码事实）：repository.searchEngine 对「引擎 error」和「引擎自报 timed_out」
// 都返回 error；logic 只在 err != nil 时 `return nil, err`。因此引擎故障**只能**表现为错误，
// 不可能退化成「空 hits + total=0 的成功」。唯一返回结果的兜底路径是 degradeOutcome，
// 它必然带 Degraded=true，并被 replyTTL 归 0（见 TestSearchCacheHitAndDegradeTTL）。
func TestSearchEngineFailureNeverLooksLikeEmptySuccess(t *testing.T) {
	cases := []struct {
		name      string
		failErr   error
		timedOut  bool
		available bool
		want      error
	}{
		{"连接失败", esclient.ErrUnavailable, false, true, model.ErrSearchUnavailable},
		{"熔断打开", esclient.ErrCircuitOpen, false, true, model.ErrSearchUnavailable},
		{"别名不存在", esclient.ErrAliasMissing, false, true, model.ErrAliasMissing},
		{"引擎拒绝 DSL", esclient.ErrBadStatus, false, true, model.ErrQueryRejected},
		{"未归类错误", errors.New("dial tcp: no route to host"), false, true, model.ErrSearchUnavailable},
		{"引擎自报超时", nil, true, true, model.ErrSearchUnavailable},
		{"引擎未配置", nil, false, false, model.ErrSearchUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			st.eng.available = tc.available
			if tc.failErr != nil {
				st.eng.failWith("Search", tc.failErr)
			}
			if tc.timedOut {
				resp := respWith(9, "eq", 5, hitOf("e1", 1, esclient.SourceDoc{DocID: 1, Title: "T"}, nil))
				resp.TimedOut = true
				st.eng.resp = resp
			}
			reply, err := doSearch(t, st, searchReq())
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：失败不得返回响应体（不得伪装成 0 条命中），实际 total=%d hits=%d",
					tc.name, reply.Total, len(reply.Hits))
			}
			wantCount(t, tc.name+"：失败不得写缓存", st.log, "cache.SetResult", 0)
			wantCount(t, tc.name+"：失败不得写历史", st.log, "history.", 0)
		})
	}
}

// TestSearchTrueEmptyResultIsStillSuccess 与上一条对照：引擎正常但零命中，
// 这才是「搜到了 0 条」，返回成功且 ttl>0，且错误为 nil。
func TestSearchTrueEmptyResultIsStillSuccess(t *testing.T) {
	st := newStore(t, testConfig())
	st.eng.resp = respWith(0, "eq", 4)

	reply, err := doSearch(t, st, func() *rpc.SearchReq {
		r := searchReq()
		r.ViewerMid = 88
		return r
	}())
	wantNoErr(t, "零命中", err)
	wantEQ(t, "零命中", "Total", reply.Total, int64(0))
	wantEQ(t, "零命中", "len(Hits)", len(reply.Hits), 0)
	wantEQ(t, "零命中", "HasMore", reply.HasMore, false)
	wantEQ(t, "零命中", "SafeFiltered（不是被屏蔽）", reply.SafeFiltered, false)
	wantEQ(t, "零命中", "Ttl", reply.Ttl, int32(30))
	wantEQ(t, "零命中", "CacheHit", reply.CacheHit, false)
}

// TestSearchCacheHitAndDegradeTTL 结果缓存的口径：命中不消耗引擎配额、命中不续期、
// 降级必须 ttl=0、引擎未配置时不用缓存掩盖、关兜底时直接报错。
//
// 判断依据（钉代码事实）：repository.Search 的判定顺序是
// 「checkPaging -> EngineAvailable -> 读缓存（命中：EngineHealthy ? 正常 : 降级）->
// searchEngine（失败：复查缓存，命中则降级否则报错）-> 只在引擎成功时写缓存」。
// logic 侧只有 replyTTL 会把 Degraded 归 0，其余字段一律照抄。
func TestSearchCacheHitAndDegradeTTL(t *testing.T) {
	st := newStore(t, testConfig())
	st.eng.resp = respWith(3, "eq", 6,
		hitOf("e1", 1, esclient.SourceDoc{DocType: model.DocTypeVideo, DocID: 501, Title: "缓存里的标题"}, nil))

	req := searchReq()
	first, err := doSearch(t, st, req)
	wantNoErr(t, "首次查询", err)
	wantEQ(t, "首次", "CacheHit", first.CacheHit, false)
	fp := st.cache.lastFingerprint(t)
	wantEQ(t, "首次", "Ttl（引擎成功：用配置窗口）", first.Ttl, int32(30))
	wantEQ(t, "缓存 TTL", "写入秒数", st.cache.resultTTLOf(t, 0), 30)
	wantOps(t, "首次调用序列", st.log.ops, []string{
		"block.IsBlocked:开源软件",
		"cache.GetResult:" + repository.ResultKey(fp, 0),
		"es.Search:0/20",
		"cache.SetResult:" + repository.ResultKey(fp, 0) + "/30",
	})

	// 第二次：命中缓存，引擎不再被调用，ttl 用剩余秒数。
	before := st.log.snapshot()
	second, err := doSearch(t, st, req)
	wantNoErr(t, "第二次查询", err)
	wantEQ(t, "第二次", "CacheHit", second.CacheHit, true)
	wantEQ(t, "第二次", "Ttl（剩余秒数）", second.Ttl, int32(30))
	wantEQ(t, "第二次", "Total 与首次一致", second.Total, first.Total)
	wantEQ(t, "第二次", "标题来自缓存副本", second.Hits[0].Title, "缓存里的标题")
	wantOpsFrom(t, "第二次调用序列", st.log, before, []string{
		"block.IsBlocked:开源软件",
		"cache.GetResult:" + repository.ResultKey(fp, 0),
	})
	wantCount(t, "命中缓存不续期（也不给未命中路径多写一次）", st.log, "cache.SetResult", 1)

	// 熔断打开：结果仍来自缓存，但必须 ttl=0（降级不伪装成正常命中，网关不写客户端缓存）。
	st.eng.circuitOpen = true
	before = st.log.snapshot()
	third, err := doSearch(t, st, req)
	wantNoErr(t, "降级查询", err)
	wantEQ(t, "降级", "仍返回缓存内容", len(third.Hits), 1)
	wantEQ(t, "降级", "Ttl 归 0", third.Ttl, int32(0))
	wantEQ(t, "降级", "CacheHit", third.CacheHit, true)
	wantEQ(t, "降级", "Total", third.Total, int64(3))
	wantOpsFrom(t, "降级调用序列", st.log, before, []string{
		"block.IsBlocked:开源软件",
		"cache.GetResult:" + repository.ResultKey(fp, 0),
	})
	wantCount(t, "降级期间不得再打引擎", st.log, "es.", 1)
	wantCount(t, "降级期间不得写缓存", st.log, "cache.SetResult", 1)

	// 关掉兜底策略：引擎不可用直接报错，绝不返回陈旧副本。
	cfg := testConfig()
	cfg.Search.DegradeEnabled = false
	st2 := newStore(t, cfg)
	st2.eng.circuitOpen = true
	st2.eng.resp = respWith(3, "eq", 6, hitOf("e1", 1, esclient.SourceDoc{DocID: 501, Title: "T"}, nil))
	_, err = doSearch(t, st2, req) // 缓存为空 -> 熔断打开 -> 无副本可兜底
	wantErrIs(t, "关闭兜底", err, model.ErrSearchUnavailable)
	wantOps(t, "关闭兜底的调用序列", st2.log.ops, []string{
		"block.IsBlocked:开源软件",
		"cache.GetResult:" + repository.ResultKey(st2.cache.lastFingerprint(t), 0),
		"es.Search:0/20",
		"cache.GetResult:" + repository.ResultKey(st2.cache.lastFingerprint(t), 0), // 失败后复查一次
	})

	// 引擎未配置：即使缓存里已有副本也不返回（配置错误不得被缓存掩盖），
	// 而且门禁在读写缓存之前——连一次 Redis 都不该碰。
	st3 := newStore(t, testConfig())
	st3.eng.resp = respWith(3, "eq", 6,
		hitOf("e1", 1, esclient.SourceDoc{DocID: 501, Title: "缓存里的标题"}, nil))
	if _, err := doSearch(t, st3, req); err != nil {
		t.Fatalf("预热结果缓存失败：%v", err)
	}
	before = st3.log.snapshot()
	st3.eng.available = false
	reply, err := doSearch(t, st3, req)
	wantErrIs(t, "未配置引擎不得用缓存掩盖", err, model.ErrSearchUnavailable)
	if reply != nil {
		t.Errorf("未配置引擎：不应返回结果，实际 %+v", reply)
	}
	wantOpsFrom(t, "未配置引擎的调用序列", st3.log, before, []string{"block.IsBlocked:开源软件"})
	wantNoOpsWith(t, "未配置引擎", st3.log, before, "cache.", "es.")
}

// TestSearchEngineFailsAfterConcurrentCacheWrite 引擎健康、缓存首读未命中、引擎失败，
// 但失败后复查缓存时并发请求刚写入副本：这条路径必须按「降级」返回而不是报错。
func TestSearchEngineFailsAfterConcurrentCacheWrite(t *testing.T) {
	st := newStore(t, testConfig())
	st.eng.searchFn = func(seq int, _ []byte) (*esclient.SearchResponse, error) {
		if seq == 1 {
			// 模拟另一个请求在同期完成回源并写入结果缓存。
			st.cache.warmResult(st.cache.lastFP, 0, 30, respWith(2, "eq", 7,
				hitOf("e9", 3, esclient.SourceDoc{DocType: model.DocTypeVideo, DocID: 901, Title: "并发副本"}, nil)))
		}
		return nil, esclient.ErrUnavailable
	}

	reply, err := doSearch(t, st, searchReq())
	wantNoErr(t, "并发副本兜底", err)
	wantEQ(t, "并发副本", "CacheHit", reply.CacheHit, true)
	wantEQ(t, "并发副本", "Ttl 归 0（降级不得被客户端缓存）", reply.Ttl, int32(0))
	wantEQ(t, "并发副本", "Total", reply.Total, int64(2))
	wantEQ(t, "并发副本", "Hits 来自副本", reply.Hits[0].Title, "并发副本")
	wantOps(t, "并发副本调用序列", st.log.ops, []string{
		"block.IsBlocked:开源软件",
		"cache.GetResult:" + ck(t, st, 0),
		"es.Search:0/20",
		"cache.GetResult:" + ck(t, st, 0),
	})
}

// TestSearchDegradeIsIndistinguishableFromExpiredCacheHit 已知口径缺口（登记用）：
// SearchReply 没有 degraded 字段，降级结果与「剩余 TTL 恰好为 0 的正常缓存命中」
// 在响应上完全同形（cache_hit=true && ttl=0）。本用例把这一事实钉住：
// 生产若要区分，必须新增字段，届时这条断言会变红。
func TestSearchDegradeIsIndistinguishableFromExpiredCacheHit(t *testing.T) {
	// 场景 A：引擎健康，但缓存条目剩余 TTL 为 0（Redis 秒级精度边界）。
	stA := newStore(t, testConfig())
	stA.eng.resp = respWith(4, "eq", 5,
		hitOf("e1", 1, esclient.SourceDoc{DocType: model.DocTypeVideo, DocID: 11, Title: "同一个副本"}, nil))
	if _, err := doSearch(t, stA, searchReq()); err != nil {
		t.Fatalf("预热失败：%v", err)
	}
	key := ck(t, stA, 0)
	stA.cache.ttls[key] = 0 // 直接把剩余秒数改 0，等价于 TTL 边界
	normalHit, err := doSearch(t, stA, searchReq())
	wantNoErr(t, "TTL 归零的正常命中", err)
	wantEQ(t, "TTL 归零的正常命中", "CacheHit", normalHit.CacheHit, true)
	wantEQ(t, "TTL 归零的正常命中", "Ttl", normalHit.Ttl, int32(0))

	// 场景 B：熔断打开走降级，命中同一份副本。
	stA.eng.circuitOpen = true
	degraded, err := doSearch(t, stA, searchReq())
	wantNoErr(t, "降级", err)

	wantEQ(t, "缺口：降级与 TTL 归零命中同形", "CacheHit", degraded.CacheHit, normalHit.CacheHit)
	wantEQ(t, "缺口：降级与 TTL 归零命中同形", "Ttl", degraded.Ttl, normalHit.Ttl)
	wantEQ(t, "缺口：降级与 TTL 归零命中同形", "Total", degraded.Total, normalHit.Total)
	wantEQ(t, "缺口：降级与 TTL 归零命中同形", "len(Hits)", len(degraded.Hits), len(normalHit.Hits))
	// 唯一可观察的差异只在调用序列上：降级时引擎一次都没被调用。
	wantCount(t, "两条路径都没打第二次引擎", stA.log, "es.", 1)
}

// TestSearchCacheDisabledNeverTouchesRedis 关闭结果缓存（CacheTTLSeconds=0）时不得访问 Redis。
func TestSearchCacheDisabledNeverTouchesRedis(t *testing.T) {
	cfg := testConfig()
	cfg.Search.CacheTTLSeconds = 0
	st := newStore(t, cfg)
	st.eng.resp = respWith(1, "eq", 2, hitOf("e1", 1, esclient.SourceDoc{DocID: 7, Title: "T"}, nil))

	reply, err := doSearch(t, st, searchReq())
	wantNoErr(t, "关闭缓存", err)
	wantEQ(t, "关闭缓存", "len(Hits)", len(reply.Hits), 1)
	wantEQ(t, "关闭缓存", "Ttl 建议不缓存", reply.Ttl, int32(0))
	wantEQ(t, "关闭缓存", "CacheHit", reply.CacheHit, false)
	wantCount(t, "关闭缓存", st.log, "cache.", 0)
	wantOps(t, "调用序列", st.log.ops, []string{"block.IsBlocked:开源软件", "es.Search:0/20"})
}

// TestSearchHistoryIsBestEffortAndGated 搜索历史的三条不变量：游客不建、开关关了不建、
// 写失败不得把已成功的搜索变成错误。
func TestSearchHistoryIsBestEffortAndGated(t *testing.T) {
	t.Run("登录用户成功后写历史并裁剪规模", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.eng.resp = respWith(1, "eq", 2, hitOf("e1", 1, esclient.SourceDoc{DocID: 7, Title: "T"}, nil))
		req := searchReq()
		req.Keyword = "开 源"
		req.ViewerMid = 88
		req.Platform = "ios"
		reply, err := doSearch(t, st, req)
		wantNoErr(t, "写历史", err)
		wantEQ(t, "写历史", "len(Hits)", len(reply.Hits), 1)

		key := repository.ResultKey(st.cache.lastFingerprint(t), 0)
		wantOps(t, "调用序列", st.log.ops, []string{
			"block.IsBlocked:开 源",
			"cache.GetResult:" + key,
			"es.Search:0/20",
			"cache.SetResult:" + key + "/30",
			"history.Upsert:88/开 源",
			"history.Prune:88/300", // repository.historyKeepRows=300，改动即红（有意漂移告警）
		})
		row, ok := st.history.get(88, "开 源")
		if !ok {
			t.Fatalf("历史未落库")
		}
		wantEQ(t, "历史", "Mid", row.Mid, int64(88))
		wantEQ(t, "历史", "Keyword（规范化后入库）", row.Keyword, "开 源")
		wantEQ(t, "历史", "Platform", row.Platform, "ios")
		wantEQ(t, "历史", "State", row.State, int32(model.HistoryStateNormal))
		wantEQ(t, "历史", "KeywordHash 与规范化词对齐", row.KeywordHash, model.KeywordHash("开 源"))
	})

	t.Run("游客不建历史", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.eng.resp = respWith(1, "eq", 2, hitOf("e1", 1, esclient.SourceDoc{DocID: 7, Title: "T"}, nil))
		_, err := doSearch(t, st, searchReq()) // ViewerMid 默认 0
		wantNoErr(t, "游客", err)
		wantCount(t, "游客", st.log, "history.", 0)
	})

	t.Run("配置关闭历史时不写", func(t *testing.T) {
		cfg := testConfig()
		cfg.Search.HistoryEnabled = false
		st := newStore(t, cfg)
		st.eng.resp = respWith(1, "eq", 2, hitOf("e1", 1, esclient.SourceDoc{DocID: 7, Title: "T"}, nil))
		req := searchReq()
		req.ViewerMid = 88
		_, err := doSearch(t, st, req)
		wantNoErr(t, "关闭历史", err)
		wantCount(t, "关闭历史", st.log, "history.", 0)
	})

	t.Run("历史写失败不影响已成功的搜索", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.eng.resp = respWith(1, "eq", 2, hitOf("e1", 1, esclient.SourceDoc{DocID: 7, Title: "T"}, nil))
		st.history.failWith("Upsert", errors.New("duplicate entry on primary key"))
		req := searchReq()
		req.ViewerMid = 88
		reply, err := doSearch(t, st, req)
		wantNoErr(t, "历史写失败", err)
		wantEQ(t, "历史写失败", "搜索仍返回命中", len(reply.Hits), 1)
		wantEQ(t, "历史写失败", "Ttl 不受影响", reply.Ttl, int32(30))
		wantCount(t, "历史写失败后不再裁剪", st.log, "history.Prune", 0)
	})
}

// --- 本文件专用小工具 ---

// bad 复制请求并改动一个字段（表驱动用例的可读性来源）。泛型让每个 logic 的请求类型共用同一份。
func bad[T any](base *T, mutate func(*T)) *T {
	cp := *base
	mutate(&cp)
	return &cp
}

// mustEncodeCursor 编一个 offset 游标（指纹可显式给错，用于 mismatch 用例）。
func mustEncodeCursor(t *testing.T, offset int64, fingerprint string) string {
	t.Helper()
	s, err := repository.EncodeOffsetCursor(offset, fingerprint)
	wantNoErr(t, "构造游标", err)
	return s
}

// rawCursor 构造指定版本的游标（验证版本门禁）。
func rawCursor(t *testing.T, version int) string {
	t.Helper()
	bs, err := json.Marshal(map[string]any{"v": version, "o": 20, "f": "x"})
	wantNoErr(t, "构造原始游标", err)
	return base64.RawURLEncoding.EncodeToString(bs)
}

// wantResultCacheOps 断言读/写结果缓存用同一个键，且键的 offset 段等于期望值，返回该键。
func wantResultCacheOps(t *testing.T, st *store, wantOffset int64) string {
	t.Helper()
	gets, sets := 0, 0
	var key string
	for _, op := range st.log.ops {
		switch {
		case strings.HasPrefix(op, "cache.GetResult:"):
			gets++
			key = strings.TrimPrefix(op, "cache.GetResult:")
		case strings.HasPrefix(op, "cache.SetResult:"):
			sets++
			k := strings.SplitN(strings.TrimPrefix(op, "cache.SetResult:"), "/", 2)[0]
			if key == "" {
				key = k
			}
			if k != key {
				t.Errorf("结果缓存读写键不一致：Get %s, Set %s", key, k)
			}
		}
	}
	if gets != 1 || sets != 1 {
		t.Fatalf("结果缓存读/写次数 = %d/%d, want 1/1（序列 %v）", gets, sets, st.log.ops)
	}
	if !strings.HasSuffix(key, ":"+itoa(wantOffset)) {
		t.Errorf("结果缓存 key %s 的 offset 段应为 %d", key, wantOffset)
	}
	return key
}

// lastFingerprint 取替身实际用于结果缓存的查询指纹（避免用例重算一套指纹）。
func (f *fakeCache) lastFingerprint(t *testing.T) string {
	t.Helper()
	if f.lastFP == "" {
		t.Fatalf("替身还没记录过查询指纹")
	}
	return f.lastFP
}

// wantJSONNum 断言 JSON 里的数值字段：encoding/json 把所有数字都读成 float64，
// 期望值写整数更易读；类型不对时立即失败（避免用 any==any 静默漏断言）。
func wantJSONNum(t *testing.T, label, field string, got any, want float64) {
	t.Helper()
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("%s：%s = %#v (%T), want 数值 %v", label, field, got, got, want)
	}
	if f != want {
		t.Errorf("%s：%s = %v, want %v", label, field, f, want)
	}
}

// ck 复原「本用例这次查询」的结果缓存键：指纹取替身实际用过的值，
// 用例不自算指纹（那等于把实现的哈希抄一遍，断言就永远不会红）。
func ck(t *testing.T, st *store, offset int64) string {
	t.Helper()
	return repository.ResultKey(st.cache.lastFingerprint(t), offset)
}

// sortedKeys 返回 JSON 对象的键名升序列表（结构断言用，map 遍历无序所以必须排序）。
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func nested(t *testing.T, body map[string]any, path ...string) any {
	t.Helper()
	var cur any = body
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("DSL 路径 %v 在 %s 处不是对象：%T", path, p, cur)
		}
		cur, ok = m[p]
		if !ok {
			t.Fatalf("DSL 缺少字段 %s（路径 %v）", p, path)
		}
	}
	return cur
}

// rangeOf 取某个字段的 range 子句。
func rangeOf(t *testing.T, body map[string]any, field string) map[string]any {
	t.Helper()
	for _, item := range nested(t, body, "query", "bool").(map[string]any)["filter"].([]any) {
		clause, ok := item.(map[string]any)["range"].(map[string]any)
		if !ok {
			continue // 非 range 子句（terms/term）
		}
		if v, ok := clause[field].(map[string]any); ok {
			return v
		}
	}
	t.Fatalf("DSL 没有 %s 的 range 子句：%s", field, stringDump(body))
	panic("unreachable")
}

// termOf 取某个字段的 term 子句值。
func termOf(t *testing.T, body map[string]any, field string) any {
	t.Helper()
	for _, item := range nested(t, body, "query", "bool").(map[string]any)["filter"].([]any) {
		clause, ok := item.(map[string]any)["term"].(map[string]any)
		if !ok {
			continue // 非 term 子句（terms/range）
		}
		if v, has := clause[field]; has {
			return v
		}
	}
	t.Fatalf("DSL 没有 %s 的 term 子句：%s", field, stringDump(body))
	panic("unreachable")
}

func toStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

func stringDump(v any) string {
	bs, _ := json.Marshal(v)
	return string(bs)
}

func jsonDump(t *testing.T, v any) string {
	t.Helper()
	bs, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	return string(bs)
}
