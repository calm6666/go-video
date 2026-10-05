// Package repository 是 search-query 服务的数据访问层。
//
// 组合三类依赖：
//   - OpenSearch 只读客户端（internal/esclient，查询索引投影，不写索引）；
//   - Redis（结果短缓存、热词缓存、联想词典、计数器）；
//   - MySQL model（搜索历史、查询日志、热词快照、屏蔽词、事件 Outbox）。
//
// 边界（AGENTS.md §5）：索引文档与热度事实由 search-indexer / 离线聚合产生，
// 本层对它们只读；写入范围仅限本服务拥有的 5 张表。
//
// 可测试性：引擎与缓存分别通过 engine / CacheStore 接口注入，
// 单测使用内存替身，不需要（也不允许）连接真实 OpenSearch / Redis / MySQL。
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/search-query/internal/config"
	"go-video/services/search-query/internal/esclient"
	"go-video/services/search-query/model"
)

// blockWordSetLimit 屏蔽词集合一次加载的上限；超出记录告警（词表过大需改分片字典）。
const blockWordSetLimit = 2000

// engine 抽象 OpenSearch 只读调用（*esclient.Client 是其唯一生产实现）。
//
// Available 只表示“引擎已配置”；Healthy 额外反映熔断器的当前状态，
// 是 Search 判定“缓存命中算不算降级”的依据。
type engine interface {
	Available() bool
	Healthy() bool
	Alias() string
	MaxResultWindow() int64
	Search(ctx context.Context, body []byte) (*esclient.SearchResponse, error)
	Count(ctx context.Context, body []byte) (int64, error)
}

// CacheStore 抽象缓存依赖，便于单测注入替身（生产实现为 *Cache）。
type CacheStore interface {
	Ping(ctx context.Context) error
	GetResult(ctx context.Context, fingerprint string, offset int64) (*cachedResult, int, bool, error)
	SetResult(ctx context.Context, fingerprint string, offset int64, v *cachedResult, ttlSeconds int) error
	GetHot(ctx context.Context, scope string) ([]*model.SearchHotKeyword, bool, error)
	SetHot(ctx context.Context, scope string, rows []*model.SearchHotKeyword, ttlSeconds int) error
	GetBlockSet(ctx context.Context) (map[string]struct{}, bool, error)
	SetBlockSet(ctx context.Context, words []string, ttlSeconds int) error
	SuggestDict(ctx context.Context, window int) ([]redis.FloatPair, error)
	IncrQueryCounters(ctx context.Context, keywordHash, day string) error
}

// Models 聚合本服务的 model 依赖（测试注入替身使用）。
type Models struct {
	History model.SearchHistoryModel
	Log     model.SearchQueryLogModel
	Hot     model.SearchHotKeywordModel
	Block   model.SearchBlockWordModel
	Outbox  model.SearchOutboxModel
}

// Repository 是 search-query 服务的数据访问入口。
type Repository struct {
	cfg           config.SearchConf
	engineTimeout time.Duration
	es            engine // nil 表示引擎未配置
	cache         CacheStore
	conn          sqlx.SqlConn

	historyMd model.SearchHistoryModel
	logMd     model.SearchQueryLogModel
	hotMd     model.SearchHotKeywordModel
	blockMd   model.SearchBlockWordModel
	outboxMd  model.SearchOutboxModel
}

// New 构造生产用 Repository。esCli 为 nil 或不可用时，查询走降级分支。
func New(rds *redis.Redis, conn sqlx.SqlConn, esCli *esclient.Client, c config.Config) *Repository {
	var eng engine
	if esCli.Available() {
		eng = esCli
	}
	return NewWithDeps(conn, eng, NewCache(rds), c, defaultModels(conn))
}

// NewWithDeps 依赖注入构造（单测与替换后端使用；conn 仍用于事务）。
func NewWithDeps(conn sqlx.SqlConn, eng engine, cache CacheStore, c config.Config, m Models) *Repository {
	return &Repository{
		cfg:           c.Search,
		engineTimeout: time.Duration(c.OpenSearch.TimeoutMs) * time.Millisecond,
		es:            eng,
		cache:         cache,
		conn:          conn,
		historyMd:     m.History,
		logMd:         m.Log,
		hotMd:         m.Hot,
		blockMd:       m.Block,
		outboxMd:      m.Outbox,
	}
}

// defaultModels 从连接构造默认 model 集合。
func defaultModels(conn sqlx.SqlConn) Models {
	return Models{
		History: model.NewSearchHistoryModel(conn),
		Log:     model.NewSearchQueryLogModel(conn),
		Hot:     model.NewSearchHotKeywordModel(conn),
		Block:   model.NewSearchBlockWordModel(conn),
		Outbox:  model.NewSearchOutboxModel(conn),
	}
}

// Cache 暴露缓存句柄（健康检查与测试使用）。
func (r *Repository) Cache() CacheStore { return r.cache }

// EngineAvailable 报告 OpenSearch 是否已配置（未配置时 false，Search 直接返回
// model.ErrSearchUnavailable，不做缓存兜底）。
func (r *Repository) EngineAvailable() bool { return r.es != nil && r.es.Available() }

// EngineHealthy 报告引擎此刻能否承接常规查询：已配置且熔断器未打开。
//
// false 表示处于故障观察期，此时结果缓存命中必须按降级返回（Degraded=true），
// 而不是伪装成正常命中；判定顺序见 Search 的文档注释。
func (r *Repository) EngineHealthy() bool { return r.EngineAvailable() && r.es.Healthy() }

// EngineAlias 返回当前查询别名（日志与排查看用）。
func (r *Repository) EngineAlias() string {
	if r.es == nil {
		return ""
	}
	return r.es.Alias()
}

// Conf 返回查询侧配置（logic 需要 PsLimit/MaxOffset 等限制）。
func (r *Repository) Conf() config.SearchConf { return r.cfg }

// Ping 检查 Redis 连通性（健康探测）。
func (r *Repository) Ping(ctx context.Context) error {
	if r.cache == nil {
		return errors.New("search-query/repository: cache not initialized")
	}
	return r.cache.Ping(ctx)
}

// dslOptions 由配置推导引擎查询参数。
func (r *Repository) dslOptions() DSLOptions {
	pre, post := r.cfg.HighlightTags()
	return DSLOptions{
		MatchFields:     DefaultMatchFields,
		HighlightFields: DefaultHighlightFields,
		PreTag:          pre,
		PostTag:         post,
		TrackTotalCap:   r.engineWindow(),
		SourceFields:    DefaultSourceFields,
		EngineTimeout:   r.engineTimeoutClause(),
	}
}

// engineWindow 返回引擎 max_result_window（无引擎时 0 表示不限制，由 Search.MaxOffset 兜底）。
func (r *Repository) engineWindow() int64 {
	if r.es == nil {
		return 0
	}
	return r.es.MaxResultWindow()
}

// engineTimeoutClause 引擎侧超时（"Nms"）。
// 取 HTTP 客户端超时的 8 成，让引擎先返回超时信号，从而把“可解释的引擎超时”
// 与“连接被掐断”区分开；未配置时返回空串（不写入 DSL）。
func (r *Repository) engineTimeoutClause() string {
	if r.engineTimeout <= 0 {
		return ""
	}
	ms := r.engineTimeout.Milliseconds() * 80 / 100
	if ms <= 0 {
		ms = 1
	}
	return fmt.Sprintf("%dms", ms)
}

// SearchOutcome 一次查询的结果与元信息。
type SearchOutcome struct {
	Hits        []esclient.Hit
	Total       int64
	Approximate bool // relation == "gte"：总数只是下界（达到 track_total_hits 上限）
	TookMs      int64
	CacheHit    bool
	CacheTTL    int  // 命中缓存时的剩余秒数（网关可据此设置响应 ttl）
	Degraded    bool // 引擎不可用/失败，用未过期缓存兜底成功（必须显式标记）
}

// Search 执行一次查询（结果短缓存 + 诚实降级）。
//
// 判定顺序（三条路径互斥，Degraded 分支真实可达）：
//
//	a) 引擎未配置（EngineAvailable=false，例如 Endpoints/Alias 缺失）：
//	   一律返回 model.ErrSearchUnavailable，即使缓存里有副本也不返回。
//	   配置缺失是运维错误，用历史缓存撑着会把故障藏起来。
//	b) 引擎已配置但熔断打开（EngineHealthy=false）且结果缓存未过期：
//	   DegradeEnabled=true 时返回缓存并置 Degraded=true（网关据此 ttl=0 不写客户端缓存、
//	   监控据此告警）；DegradeEnabled=false 时返回引擎错误，绝不兜底。
//	c) 引擎健康：缓存命中直接返回（Degraded=false，不消耗引擎配额）；未命中才请求引擎，
//	   引擎失败时若缓存刚好被并发请求写入且仍未过期，同样按 b 的降级规则处理，
//	   否则原样返回引擎错误。
//
// 关键红线：引擎挂了绝不表现为“空 hits + total=0”的伪成功，客户端必须能区分
// “真的没结果”和“搜索挂了”。降级返回一定带 Degraded 标记。
//
// 恢复保证：命中缓存不会给条目续期（只有引擎成功才写缓存），所以故障期最多持续
// CacheTTLSeconds 就会回到未命中分支并再次调用引擎，熔断器的半开探活不会被饿死。
func (r *Repository) Search(ctx context.Context, p SearchParams) (*SearchOutcome, error) {
	if p.Keyword == "" {
		return nil, model.ErrInvalidKeyword
	}
	if p.Fingerprint == "" {
		p.Fingerprint = QueryFingerprint(p)
	}
	// 深分页保护在读写缓存之前执行：拒绝的翻页不应消耗缓存或引擎配额。
	if err := r.checkPaging(p); err != nil {
		return nil, err
	}
	// a) 未配置引擎：明确报错，不做缓存兜底。
	if !r.EngineAvailable() {
		return nil, model.ErrSearchUnavailable
	}

	cached, left, hit := r.readResultCache(ctx, p)
	if hit {
		if r.EngineHealthy() {
			return outcomeFromCache(cached, left), nil
		}
		// b) 熔断打开：缓存命中是“带标记的降级”，不是正常命中。
		return r.degradeOutcome(p, cached, left, errEngineBreakerOpen())
	}

	outcome, engineErr := r.searchEngine(ctx, p)
	if engineErr != nil {
		// 缓存此前未命中；复查一次以覆盖并发请求刚写入副本的情况（同时校验是否已过期）。
		if cached, left, hit := r.readResultCache(ctx, p); hit {
			return r.degradeOutcome(p, cached, left, engineErr)
		}
		return nil, engineErr
	}
	r.writeResultCache(ctx, p, outcome)
	return outcome, nil
}

// readResultCache 读取结果短缓存（关闭、未初始化或读失败时按未命中处理）。
// 缓存故障不阻断查询：可用性优先，但绝不会把读失败算成命中。
func (r *Repository) readResultCache(ctx context.Context, p SearchParams) (*cachedResult, int, bool) {
	if r.cfg.CacheTTLSeconds <= 0 || r.cache == nil {
		return nil, 0, false
	}
	cached, left, hit, err := r.cache.GetResult(ctx, p.Fingerprint, p.From)
	if err != nil {
		logx.Errorf("search-query/repository: read result cache fp=%s err=%v", p.Fingerprint, err)
		return nil, 0, false
	}
	return cached, left, hit && cached != nil
}

// writeResultCache 写入结果短缓存（仅引擎成功路径调用）。
func (r *Repository) writeResultCache(ctx context.Context, p SearchParams, o *SearchOutcome) {
	if r.cfg.CacheTTLSeconds <= 0 || r.cache == nil || o == nil || o.Degraded {
		return
	}
	payload := &cachedResult{Hits: o.Hits, Total: o.Total, TookMs: o.TookMs}
	if o.Approximate {
		// 保留“总数只是下界”的语义，否则缓存命中会把 gte 伪装成精确总数。
		payload.Relation = "gte"
	}
	if err := r.cache.SetResult(ctx, p.Fingerprint, p.From, payload, r.cfg.CacheTTLSeconds); err != nil {
		logx.Errorf("search-query/repository: write result cache fp=%s err=%v", p.Fingerprint, err)
	}
}

// degradeOutcome 用未过期缓存构造降级结果；策略不允许兜底时返回引擎错误。
func (r *Repository) degradeOutcome(p SearchParams, cached *cachedResult, left int, cause error) (*SearchOutcome, error) {
	if !r.cfg.DegradeEnabled || cached == nil {
		return nil, cause
	}
	out := outcomeFromCache(cached, left)
	out.Degraded = true
	logx.Errorf("search-query/repository: engine degraded, served unexpired cache fp=%s ttl_left=%ds cause=%v",
		p.Fingerprint, left, cause)
	return out, nil
}

// errEngineBreakerOpen 熔断打开时的引擎错误（未配置走 a) 分支，不会到这里）。
func errEngineBreakerOpen() error {
	return fmt.Errorf("%w: circuit breaker open", model.ErrSearchUnavailable)
}

// outcomeFromCache 把缓存载荷转成查询结果（标记 cache_hit，供网关设置响应 ttl）。
func outcomeFromCache(v *cachedResult, ttlLeft int) *SearchOutcome {
	return &SearchOutcome{
		Hits:        v.Hits,
		Total:       v.Total,
		Approximate: v.Relation == "gte",
		TookMs:      v.TookMs,
		CacheHit:    true,
		CacheTTL:    ttlLeft,
	}
}

// checkPaging 深分页保护（docs/api-and-events.md §2：限制 page size，避免深分页）。
//
// 两条上限同时生效：
//   - Search.MaxOffset：起始 offset 上限（业务侧限流，通常远小于引擎窗口）；
//   - 引擎 index.max_result_window：from+size 上限（超过会被引擎直接拒绝）。
//
// 超限时返回 model.ErrDeepPage，调用方应引导客户端改用更窄的筛选条件而不是继续翻页。
func (r *Repository) checkPaging(p SearchParams) error {
	if p.Size <= 0 {
		return fmt.Errorf("%w: size=%d", model.ErrInvalidPage, p.Size)
	}
	if p.From < 0 {
		return fmt.Errorf("%w: from=%d", model.ErrInvalidPage, p.From)
	}
	if r.cfg.MaxOffset > 0 && p.From > int64(r.cfg.MaxOffset) {
		return fmt.Errorf("%w: offset=%d max_offset=%d", model.ErrDeepPage, p.From, r.cfg.MaxOffset)
	}
	if w := r.engineWindow(); w > 0 && p.From+int64(p.Size) > w {
		return fmt.Errorf("%w: offset=%d size=%d engine_window=%d", model.ErrDeepPage, p.From, p.Size, w)
	}
	return nil
}

// searchEngine 直接请求引擎（不读写缓存）。
func (r *Repository) searchEngine(ctx context.Context, p SearchParams) (*SearchOutcome, error) {
	if !r.EngineAvailable() {
		return nil, model.ErrSearchUnavailable
	}
	body, err := BuildSearchBody(p, r.dslOptions())
	if err != nil {
		return nil, err
	}
	resp, err := r.es.Search(ctx, body)
	if err != nil {
		return nil, MapEngineError(err)
	}
	if resp.TimedOut {
		// 引擎自报超时：结果可能不完整，按不可用处理而不是返回半份数据。
		return nil, fmt.Errorf("%w: engine reported timed_out", model.ErrSearchUnavailable)
	}
	return &SearchOutcome{
		Hits:        resp.Hits.Hits,
		Total:       resp.Hits.Total.Value,
		Approximate: resp.Hits.Total.Relation == "gte",
		TookMs:      resp.Took,
	}, nil
}

// MapEngineError 把引擎层错误映射为领域错误码（保留原始原因供日志，不吞错误）。
func MapEngineError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, esclient.ErrNotConfigured),
		errors.Is(err, esclient.ErrUnavailable),
		errors.Is(err, esclient.ErrCircuitOpen):
		return fmt.Errorf("%w: %v", model.ErrSearchUnavailable, err)
	case errors.Is(err, esclient.ErrAliasMissing):
		return fmt.Errorf("%w: %v", model.ErrAliasMissing, err)
	case errors.Is(err, esclient.ErrBadResponse), errors.Is(err, esclient.ErrBadStatus):
		return fmt.Errorf("%w: %v", model.ErrQueryRejected, err)
	default:
		return fmt.Errorf("%w: %v", model.ErrSearchUnavailable, err)
	}
}

// IsBlockedKeyword 判定关键词是否命中生效屏蔽词。
//
// 屏蔽词表读取失败时返回错误：安全判定无法完成时不允许放行查询。
func (r *Repository) IsBlockedKeyword(ctx context.Context, keyword string) (bool, error) {
	return r.blockMd.IsBlocked(ctx, keyword)
}

// blockedWordSet 加载生效屏蔽词集合（Redis 短缓存 + DB 回源）。
// 用于联想与热词结果的出口过滤，避免已屏蔽词经其它入口重新暴露。
func (r *Repository) blockedWordSet(ctx context.Context) map[string]struct{} {
	if r.cache != nil {
		set, hit, err := r.cache.GetBlockSet(ctx)
		if err != nil {
			logx.Errorf("search-query/repository: read block set cache err=%v", err)
		}
		if hit {
			return set
		}
	}
	rows, err := r.blockMd.ListActive(ctx, 0, blockWordSetLimit)
	if err != nil {
		// 无法取得词表时不做“猜测式屏蔽”：记录错误并在返回值里表现为不过滤，
		// 引擎主链路的 IsBlockedKeyword 仍会硬失败，安全边界不因此放宽。
		logx.Errorf("search-query/repository: load block words from db err=%v", err)
		return map[string]struct{}{}
	}
	if len(rows) >= blockWordSetLimit {
		logx.Errorf("search-query/repository: block word set truncated at %d, split the dictionary before it grows", blockWordSetLimit)
	}
	words := make([]string, 0, len(rows))
	set := make(map[string]struct{}, len(rows))
	for _, w := range rows {
		words = append(words, w.Word)
		set[w.Word] = struct{}{}
	}
	if r.cache != nil {
		if err := r.cache.SetBlockSet(ctx, words, r.cfg.BlockWordCacheTTLSeconds); err != nil {
			logx.Errorf("search-query/repository: write block set cache err=%v", err)
		}
	}
	return set
}
