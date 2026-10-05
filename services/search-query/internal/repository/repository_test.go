package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-video/services/search-query/internal/config"
	"go-video/services/search-query/internal/esclient"
	"go-video/services/search-query/model"
)

// 诚实降级是本期最重要的行为约束：引擎故障必须表现为错误，
// 而不是“成功 + 空 hits”，否则网关与客户端无法区分“没结果”和“搜索挂了”。

func searchParams() SearchParams {
	p := SearchParams{
		Keyword:    "关键词",
		DocTypes:   []string{model.DocTypeVideo},
		SortFields: []SortField{{Field: "_score", Desc: true}},
		Size:       30,
	}
	p.Fingerprint = QueryFingerprint(p)
	return p
}

func repoWith(eng engine, cache CacheStore) (*Repository, config.Config) {
	c := testConfig()
	return newTestRepo(nil, eng, cache, c, modelsOf(&fakeHistory{}, &fakeLog{inserted: true}, &fakeHot{}, &fakeBlock{words: map[string]struct{}{}}, &fakeOutbox{})), c
}

func TestSearchWithoutEngineFailsExplicitly(t *testing.T) {
	repo, _ := repoWith(nil, newFakeCache())
	if repo.EngineAvailable() {
		t.Fatal("EngineAvailable must be false when the engine is not configured")
	}
	out, err := repo.Search(context.Background(), searchParams())
	if !errors.Is(err, model.ErrSearchUnavailable) {
		t.Fatalf("err = %v, want ErrSearchUnavailable", err)
	}
	if out != nil {
		t.Fatalf("no outcome must be returned together with an error, got %+v", out)
	}
}

func TestEngineFailureIsNotReportedAsEmptySuccess(t *testing.T) {
	eng := &fakeEngine{searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return nil, esclient.ErrUnavailable
	}}
	cache := newFakeCache()
	repo, _ := repoWith(eng, cache)

	if _, err := repo.Search(context.Background(), searchParams()); !errors.Is(err, model.ErrSearchUnavailable) {
		t.Fatalf("err = %v, want ErrSearchUnavailable", err)
	}
	if cache.setCalls != 0 {
		t.Fatalf("failed query must not be cached, setCalls=%d", cache.setCalls)
	}
}

// TestSearchServesUnexpiredCacheOnly 覆盖降级兜底：引擎已配置但熔断打开时，
// 允许返回仍在 TTL 内的缓存，且必须打上 Degraded 标记
// （网关据此不写客户端缓存、监控据此报警）。
func TestSearchServesUnexpiredCacheOnly(t *testing.T) {
	eng := &fakeEngine{circuitOpen: true, searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return nil, esclient.ErrCircuitOpen
	}}
	p := searchParams()
	cache := newFakeCache()
	cache.seededResult(p.Fingerprint, 0, 12, searchResponseWith(3, "eq", "甲", "乙", "丙"))
	repo, _ := repoWith(eng, cache)

	out, err := repo.Search(context.Background(), p)
	if err != nil {
		t.Fatalf("degrade path must succeed from unexpired cache: %v", err)
	}
	if !out.Degraded || !out.CacheHit {
		t.Fatalf("outcome flags = degraded=%v cache_hit=%v, want both true", out.Degraded, out.CacheHit)
	}
	if len(out.Hits) != 3 || out.Total != 3 {
		t.Fatalf("degraded result must carry the cached hits verbatim, got total=%d hits=%d", out.Total, len(out.Hits))
	}
	if out.CacheTTL != 12 {
		t.Errorf("CacheTTL = %d, want remaining ttl 12", out.CacheTTL)
	}
	// 熔断打开 + 缓存命中：不再消耗引擎配额（恢复靠缓存自然过期后的未命中请求）。
	if len(eng.searchBodies) != 0 {
		t.Errorf("engine must not be queried while the breaker is open, calls=%d", len(eng.searchBodies))
	}
	if cache.setCalls != 0 {
		t.Errorf("degraded result must not refresh/extend the cache, setCalls=%d", cache.setCalls)
	}
}

// TestSearchDegradeDisabledSurfacesError 关闭兜底策略时，引擎故障必须原样抛错，
// 即使缓存里还有未过期的副本（宁可失败，不可伪装）。
func TestSearchDegradeDisabledSurfacesError(t *testing.T) {
	eng := &fakeEngine{circuitOpen: true, searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return nil, esclient.ErrUnavailable
	}}
	p := searchParams()
	cache := newFakeCache()
	cache.seededResult(p.Fingerprint, 0, 20, searchResponseWith(1, "eq", "甲"))
	c := testConfig()
	c.Search.DegradeEnabled = false
	repo := newTestRepo(nil, eng, cache, c, modelsOf(&fakeHistory{}, &fakeLog{}, &fakeHot{}, &fakeBlock{}, &fakeOutbox{}))

	if _, err := repo.Search(context.Background(), p); !errors.Is(err, model.ErrSearchUnavailable) {
		t.Fatalf("err = %v, want ErrSearchUnavailable with DegradeEnabled=false", err)
	}
	if len(eng.searchBodies) != 0 {
		t.Errorf("engine must not be queried while the breaker is open, calls=%d", len(eng.searchBodies))
	}
}

// TestSearchEngineNotConfiguredNeverServesCache 区分“配置缺失”和“临时故障”：
// 未配置引擎时即使缓存里有副本也必须报错（与 VerifyAdminPermission 的显式失败风格一致）。
func TestSearchEngineNotConfiguredNeverServesCache(t *testing.T) {
	p := searchParams()
	cache := newFakeCache()
	cache.seededResult(p.Fingerprint, 0, 30, searchResponseWith(2, "eq", "甲", "乙"))
	repo, _ := repoWith(nil, cache)

	if _, err := repo.Search(context.Background(), p); !errors.Is(err, model.ErrSearchUnavailable) {
		t.Fatalf("err = %v, want ErrSearchUnavailable without a configured engine", err)
	}
	if cache.getCalls != 0 {
		t.Errorf("no cache read is expected before the configuration check, getCalls=%d", cache.getCalls)
	}
}

// TestSearchDegradesWhenEngineFailsAfterCacheMiss 覆盖第二条降级路径：
// 请求开始时缓存未命中，引擎随后失败，而并发请求已经写入了未过期副本。
func TestSearchDegradesWhenEngineFailsAfterCacheMiss(t *testing.T) {
	eng := &fakeEngine{searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return nil, esclient.ErrUnavailable
	}}
	p := searchParams()
	cache := newFakeCache()
	cache.onMiss = func() {
		cache.seededResult(p.Fingerprint, 0, 9, searchResponseWith(1, "eq", "甲"))
	}
	repo, _ := repoWith(eng, cache)

	out, err := repo.Search(context.Background(), p)
	if err != nil {
		t.Fatalf("engine failure after a concurrent cache write must degrade: %v", err)
	}
	if !out.Degraded || !out.CacheHit {
		t.Fatalf("flags = degraded=%v cache_hit=%v, want both true", out.Degraded, out.CacheHit)
	}
	if len(eng.searchBodies) != 1 {
		t.Errorf("engine calls = %d, want 1", len(eng.searchBodies))
	}
	if cache.setCalls != 0 {
		t.Errorf("failed query must not be cached, setCalls=%d", cache.setCalls)
	}
}

// TestSearchWritesAndReusesResultCache 校验缓存 key 生成与命中路径。
func TestSearchWritesAndReusesResultCache(t *testing.T) {
	eng := &fakeEngine{searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return searchResponseWith(2, "eq", "甲", "乙"), nil
	}}
	cache := newFakeCache()
	repo, c := repoWith(eng, cache)
	p := searchParams()

	out, err := repo.Search(context.Background(), p)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if out.CacheHit {
		t.Error("first query must not report a cache hit")
	}
	stored, ok := cache.cachedHits(p.Fingerprint, 0)
	if !ok {
		t.Fatalf("result must be cached under %s", ResultKey(p.Fingerprint, 0))
	}
	if stored.Total != 2 || len(stored.Hits) != 2 {
		t.Fatalf("cached payload = %+v", stored)
	}
	if cache.ttls[ResultKey(p.Fingerprint, 0)] != c.Search.CacheTTLSeconds {
		t.Errorf("cache ttl = %d, want configured %d",
			cache.ttls[ResultKey(p.Fingerprint, 0)], c.Search.CacheTTLSeconds)
	}

	second, err := repo.Search(context.Background(), p)
	if err != nil {
		t.Fatalf("second Search: %v", err)
	}
	if !second.CacheHit {
		t.Error("second identical query must hit the cache")
	}
	if len(eng.searchBodies) != 1 {
		t.Errorf("engine must be queried once, got %d", len(eng.searchBodies))
	}

	// 不同 offset 必须是不同 key（翻页不能复用上一页的副本）。
	if _, ok := cache.cachedHits(p.Fingerprint, 30); ok {
		t.Error("offset 30 must not reuse the offset 0 cache entry")
	}
	// 不同关键词（指纹不同）也不能复用。
	other := p
	other.Keyword = "另一个词"
	other.Fingerprint = QueryFingerprint(other)
	if _, ok := cache.cachedHits(other.Fingerprint, 0); ok {
		t.Error("a different query fingerprint must not reuse the cache entry")
	}
}

func TestResultKeyIsOffsetScopedAndFingerprintScoped(t *testing.T) {
	if got, want := ResultKey("fp1", 0), "sq:res:v1:fp1:0"; got != want {
		t.Fatalf("ResultKey = %q, want %q (service-owned namespace)", got, want)
	}
	a, b, c := ResultKey("fp1", 0), ResultKey("fp1", 30), ResultKey("fp2", 0)
	if a == b || a == c || b == c {
		t.Fatalf("keys must differ per fingerprint and offset: %s %s %s", a, b, c)
	}
}

// TestCacheReadFailureFallsBackToEngine：缓存故障不能阻断搜索（可用性优先），
// 但也不允许把缓存故障伪装成命中。
func TestCacheReadFailureFallsBackToEngine(t *testing.T) {
	eng := &fakeEngine{searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return searchResponseWith(1, "eq", "甲"), nil
	}}
	cache := newFakeCache()
	cache.getResultErr = errors.New("redis: connection refused")
	repo, _ := repoWith(eng, cache)

	out, err := repo.Search(context.Background(), searchParams())
	if err != nil {
		t.Fatalf("cache failure must not fail the query: %v", err)
	}
	if out.CacheHit {
		t.Error("cache read failure must not be reported as a hit")
	}
	if len(eng.searchBodies) != 1 {
		t.Errorf("engine must still be queried, calls=%d", len(eng.searchBodies))
	}
}

func TestSearchCachingDisabledSkipsCache(t *testing.T) {
	eng := &fakeEngine{searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return searchResponseWith(1, "eq", "甲"), nil
	}}
	cache := newFakeCache()
	c := testConfig()
	c.Search.CacheTTLSeconds = 0
	repo := newTestRepo(nil, eng, cache, c, modelsOf(&fakeHistory{}, &fakeLog{}, &fakeHot{}, &fakeBlock{}, &fakeOutbox{}))

	if _, err := repo.Search(context.Background(), searchParams()); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if cache.getCalls != 0 || cache.setCalls != 0 {
		t.Errorf("cache must be bypassed when ttl=0, got get=%d set=%d", cache.getCalls, cache.setCalls)
	}
}

// TestDeepPagingRejections 深分页保护：业务 MaxOffset 与引擎 max_result_window 双重生效，
// 且拒绝发生在读写缓存与访问引擎之前。
func TestDeepPagingRejections(t *testing.T) {
	cases := []struct {
		name   string
		from   int64
		size   int32
		engine bool
		want   error
	}{
		{name: "beyond max offset", from: 901, size: 30, want: model.ErrDeepPage},
		{name: "far beyond max offset", from: 5000, size: 30, want: model.ErrDeepPage},
		{name: "zero size", from: 0, size: 0, want: model.ErrInvalidPage},
		{name: "negative offset", from: -1, size: 30, want: model.ErrInvalidPage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &fakeEngine{searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
				return searchResponseWith(0, "eq"), nil
			}}
			p := searchParams()
			p.From, p.Size = tc.from, tc.size
			p.Fingerprint = QueryFingerprint(p)
			cache := newFakeCache()
			cache.seededResult(p.Fingerprint, tc.from, 10, searchResponseWith(1, "eq", "甲"))
			repo, _ := repoWith(eng, cache)

			_, err := repo.Search(context.Background(), p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(eng.searchBodies) != 0 {
				t.Error("rejected paging must not reach the engine")
			}
			if cache.getCalls != 0 || cache.setCalls != 0 {
				t.Errorf("rejected paging must not touch the cache, get=%d set=%d", cache.getCalls, cache.setCalls)
			}
		})
	}
}

// 恰好落在 MaxOffset 上应放行（边界值），而 from+size 超过引擎窗口要拒绝。
func TestPagingBoundaries(t *testing.T) {
	eng := &fakeEngine{window: 1000, searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return searchResponseWith(0, "eq"), nil
	}}
	repo, _ := repoWith(eng, newFakeCache())

	ok := searchParams()
	ok.From, ok.Size, ok.Fingerprint = 900, 30, ""
	ok.Fingerprint = QueryFingerprint(ok)
	if _, err := repo.Search(context.Background(), ok); err != nil {
		t.Errorf("offset == MaxOffset must be allowed, got %v", err)
	}

	tightRepo, _ := repoWith(&fakeEngine{window: 100, searchFn: eng.searchFn}, newFakeCache())
	over := searchParams()
	over.From, over.Size = 90, 20 // 90+20 > 引擎窗口 100
	over.Fingerprint = QueryFingerprint(over)
	if _, err := tightRepo.Search(context.Background(), over); !errors.Is(err, model.ErrDeepPage) {
		t.Errorf("from+size beyond engine window: err = %v, want ErrDeepPage", err)
	}
}

func TestSearchRejectsEmptyKeyword(t *testing.T) {
	repo, _ := repoWith(&fakeEngine{}, newFakeCache())
	if _, err := repo.Search(context.Background(), SearchParams{Size: 10}); !errors.Is(err, model.ErrInvalidKeyword) {
		t.Fatalf("err = %v, want ErrInvalidKeyword", err)
	}
}

// TestEngineTimeoutIsReportedAsFailure：引擎自报 timed_out 时结果可能不完整，
// 按不可用处理而不是返回半份数据。
func TestEngineTimeoutIsReportedAsFailure(t *testing.T) {
	eng := &fakeEngine{searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		resp := searchResponseWith(1, "eq", "甲")
		resp.TimedOut = true
		return resp, nil
	}}
	repo, _ := repoWith(eng, newFakeCache())
	if _, err := repo.Search(context.Background(), searchParams()); !errors.Is(err, model.ErrSearchUnavailable) {
		t.Fatalf("err = %v, want ErrSearchUnavailable", err)
	}
}

func TestApproximateTotalFromGteRelation(t *testing.T) {
	eng := &fakeEngine{searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return searchResponseWith(10000, "gte", "甲"), nil
	}}
	repo, _ := repoWith(eng, newFakeCache())
	out, err := repo.Search(context.Background(), searchParams())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !out.Approximate {
		t.Error("relation=gte must surface as an approximate total")
	}
}

func TestMapEngineErrorCodes(t *testing.T) {
	cases := []struct {
		in   error
		want error
	}{
		{nil, nil},
		{esclient.ErrNotConfigured, model.ErrSearchUnavailable},
		{esclient.ErrUnavailable, model.ErrSearchUnavailable},
		{esclient.ErrCircuitOpen, model.ErrSearchUnavailable},
		{esclient.ErrAliasMissing, model.ErrAliasMissing},
		{esclient.ErrBadStatus, model.ErrQueryRejected},
		{esclient.ErrBadResponse, model.ErrQueryRejected},
		{errors.New("boom"), model.ErrSearchUnavailable},
	}
	for _, tc := range cases {
		got := MapEngineError(tc.in)
		if tc.want == nil {
			if got != nil {
				t.Errorf("MapEngineError(nil) = %v", got)
			}
			continue
		}
		if !errors.Is(got, tc.want) {
			t.Errorf("MapEngineError(%v) = %v, want %v", tc.in, got, tc.want)
		}
		// 原始原因必须保留，便于日志定位（不允许只留领域码）。
		if tc.in != nil && !strings.Contains(got.Error(), tc.in.Error()) {
			t.Errorf("wrapped error lost the cause: %v", got)
		}
	}
}

// TestSearchBodyCarriesConfiguredDialect 校验仓库把配置翻译进 DSL：
// 投影字段、高亮标签、引擎超时与 track_total_hits（= 引擎窗口）。
func TestSearchBodyCarriesConfiguredDialect(t *testing.T) {
	eng := &fakeEngine{window: 5000, searchFn: func(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
		return emptySearchResponse(), nil
	}}
	c := testConfig()
	c.OpenSearch.TimeoutMs = 1000
	c.Search.HighlightPreTag = ""
	c.Search.HighlightPostTag = ""
	repo := newTestRepo(nil, eng, newFakeCache(), c,
		modelsOf(&fakeHistory{}, &fakeLog{}, &fakeHot{}, &fakeBlock{}, &fakeOutbox{}))

	if _, err := repo.Search(context.Background(), searchParams()); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(eng.searchBodies) != 1 {
		t.Fatalf("engine calls = %d", len(eng.searchBodies))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(eng.searchBodies[0]), &body); err != nil {
		t.Fatalf("engine body is not valid json: %v", err)
	}
	if int64(body["track_total_hits"].(float64)) != 5000 {
		t.Errorf("track_total_hits = %v, want engine window 5000", body["track_total_hits"])
	}
	// HTTP 超时 1000ms -> 引擎侧 timeout 取 8 成 = 800ms。
	if body["timeout"] != "800ms" {
		t.Errorf("timeout = %v, want 800ms", body["timeout"])
	}
	hl := body["highlight"].(map[string]any)
	if hl["pre_tags"].([]any)[0] != "<em>" {
		t.Errorf("pre_tags = %v, want default <em>", hl["pre_tags"])
	}
	src := body["_source"].([]any)
	if len(src) != len(DefaultSourceFields) {
		t.Errorf("_source = %v, want projection fields", src)
	}
}

func TestEngineTimeoutClauseEdgeCases(t *testing.T) {
	c := testConfig()
	c.OpenSearch.TimeoutMs = 0
	repo := newTestRepo(nil, nil, nil, c, Models{})
	if got := repo.engineTimeoutClause(); got != "" {
		t.Errorf("timeout clause = %q, want empty when timeout not configured", got)
	}
	c.OpenSearch.TimeoutMs = 1
	repo = newTestRepo(nil, nil, nil, c, Models{})
	if got := repo.engineTimeoutClause(); got != "1ms" {
		t.Errorf("timeout clause = %q, want clamped 1ms", got)
	}
}

func TestRepositoryConfAndAlias(t *testing.T) {
	eng := &fakeEngine{alias: "go_video_search_read"}
	repo, c := repoWith(eng, newFakeCache())
	if repo.EngineAlias() != "go_video_search_read" {
		t.Errorf("alias = %q", repo.EngineAlias())
	}
	if repo.Conf().PsLimit != c.Search.PsLimit {
		t.Errorf("Conf() must expose the search limits")
	}
	// 无引擎时别名读取要安全返回空串（日志不能 panic）。
	noEngine, _ := repoWith(nil, nil)
	if noEngine.EngineAlias() != "" {
		t.Errorf("alias without engine = %q, want empty", noEngine.EngineAlias())
	}
	if err := noEngine.Ping(context.Background()); err == nil {
		t.Error("Ping without a cache must report an error instead of pretending health")
	}
}

func TestIsBlockedKeywordPropagatesFailure(t *testing.T) {
	// 屏蔽词判定失败时不允许“猜测式放行”：错误必须原样返回给 logic。
	c := testConfig()
	block := &fakeBlock{blockErr: errors.New("db down")}
	repo := newTestRepo(nil, nil, newFakeCache(), c,
		modelsOf(&fakeHistory{}, &fakeLog{}, &fakeHot{}, block, &fakeOutbox{}))
	if _, err := repo.IsBlockedKeyword(context.Background(), "词"); err == nil {
		t.Fatal("block word failure must be returned")
	}

	okBlock := &fakeBlock{words: map[string]struct{}{"敏感词": {}}}
	repo = newTestRepo(nil, nil, newFakeCache(), c,
		modelsOf(&fakeHistory{}, &fakeLog{}, &fakeHot{}, okBlock, &fakeOutbox{}))
	hit, err := repo.IsBlockedKeyword(context.Background(), "敏感词")
	if err != nil || !hit {
		t.Fatalf("hit=%v err=%v, want blocked", hit, err)
	}
	miss, err := repo.IsBlockedKeyword(context.Background(), "普通词")
	if err != nil || miss {
		t.Fatalf("hit=%v err=%v, want allowed", miss, err)
	}
}

// TestBlockedWordSetFailureDoesNotPanic 词表加载失败时出口过滤退化为“不过滤”，
// 但主链路 IsBlockedKeyword 仍会硬失败（安全边界不放宽）。
func TestBlockedWordSetFailureDoesNotPanic(t *testing.T) {
	c := testConfig()
	repo := newTestRepo(nil, nil, newFakeCache(), c,
		modelsOf(&fakeHistory{}, &fakeLog{}, &fakeHot{}, &fakeBlock{blockErr: errors.New("db down")}, &fakeOutbox{}))
	set := repo.blockedWordSet(context.Background())
	if len(set) != 0 {
		t.Errorf("set = %v, want empty on load failure", set)
	}
}

func TestBlockedWordSetCachedAndSourcedFromDB(t *testing.T) {
	c := testConfig()
	cache := newFakeCache()
	block := &fakeBlock{active: []*model.SearchBlockWord{{Word: "敏感词"}, {Word: "另一个词"}}}
	repo := newTestRepo(nil, nil, cache, c, modelsOf(&fakeHistory{}, &fakeLog{}, &fakeHot{}, block, &fakeOutbox{}))

	ctx := context.Background()
	if set := repo.blockedWordSet(ctx); len(set) != 2 {
		t.Fatalf("set = %v, want both active words", set)
	}
	if block.listCalls != 1 {
		t.Errorf("db reads = %d, want 1", block.listCalls)
	}
	if !cache.blockHit {
		t.Error("block set must be written back to the cache")
	}
	// 第二次直接命中缓存，不再回源 DB（联想/热词每次出口过滤都会走这里）。
	if set := repo.blockedWordSet(ctx); len(set) != 2 {
		t.Fatalf("cached set = %v, want both words", set)
	}
	if block.listCalls != 1 {
		t.Errorf("db reads after cache hit = %d, want 1", block.listCalls)
	}
}
