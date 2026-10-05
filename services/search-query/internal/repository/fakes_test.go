package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/search-query/internal/config"
	"go-video/services/search-query/internal/esclient"
	"go-video/services/search-query/model"
)

// 本文件是仓库层单测的内存替身：不连接任何 MySQL / Redis / OpenSearch。

// ---------------------------------------------------------------- 引擎替身

type fakeEngine struct {
	mu sync.Mutex

	alias  string
	window int64

	// circuitOpen 模拟熔断器处于打开状态（*esclient.Client.Healthy 的对应信号）。
	// 置为 true 时 Repository 认为引擎处于故障观察期，缓存命中要按降级返回。
	circuitOpen bool

	searchFn func(ctx context.Context, body []byte) (*esclient.SearchResponse, error)
	countFn  func(ctx context.Context, body []byte) (int64, error)

	searchBodies []string
	countCalls   int
}

func (e *fakeEngine) Available() bool { return true }

// Healthy 与生产实现口径一致：已配置且熔断器未打开才算健康。
func (e *fakeEngine) Healthy() bool { return e.Available() && !e.circuitOpen }

func (e *fakeEngine) Alias() string {
	if e.alias == "" {
		return "go_video_search_read"
	}
	return e.alias
}

func (e *fakeEngine) MaxResultWindow() int64 {
	if e.window == 0 {
		return 10000
	}
	return e.window
}

func (e *fakeEngine) Search(ctx context.Context, body []byte) (*esclient.SearchResponse, error) {
	e.mu.Lock()
	e.searchBodies = append(e.searchBodies, string(body))
	fn := e.searchFn
	n := len(e.searchBodies)
	e.mu.Unlock()
	if fn == nil {
		return emptySearchResponse(), nil
	}
	_ = n
	return fn(ctx, body)
}

func (e *fakeEngine) Count(ctx context.Context, body []byte) (int64, error) {
	e.mu.Lock()
	e.countCalls++
	fn := e.countFn
	e.mu.Unlock()
	if fn == nil {
		return 0, nil
	}
	return fn(ctx, body)
}

// searchResponseWith 构造 n 条命中的引擎响应。
func searchResponseWith(total int64, relation string, titles ...string) *esclient.SearchResponse {
	resp := &esclient.SearchResponse{Took: 7}
	resp.Hits.Total = esclient.TotalBlock{Value: total, Relation: relation}
	for i, title := range titles {
		resp.Hits.Hits = append(resp.Hits.Hits, esclient.Hit{
			Index: "go_video_search_v1",
			ID:    []byte(fmt.Sprintf("%q", fmt.Sprintf("doc-%d", i))),
			Score: float64(10 - i),
			Source: esclient.SourceDoc{
				DocType: model.DocTypeVideo,
				DocID:   int64(1000 + i),
				Title:   title,
			},
		})
	}
	return resp
}

func emptySearchResponse() *esclient.SearchResponse {
	return searchResponseWith(0, "eq")
}

// ---------------------------------------------------------------- 缓存替身

type fakeCache struct {
	mu sync.Mutex

	results      map[string]*cachedResult
	ttls         map[string]int
	hot          map[string][]*model.SearchHotKeyword
	hotHit       bool
	blockSet     map[string]struct{}
	blockHit     bool
	dict         []redis.FloatPair
	counters     []string
	setCalls     int
	getCalls     int
	pingErr      error
	getResultErr error
	dictErr      error

	// onMiss 在首次未命中时被回调一次（回调期间不持锁），用于模拟
	// “引擎调用期间有并发请求写入了结果副本”这一竞态。
	onMiss func()
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		results:  map[string]*cachedResult{},
		ttls:     map[string]int{},
		hot:      map[string][]*model.SearchHotKeyword{},
		blockSet: map[string]struct{}{},
	}
}

func (c *fakeCache) Ping(ctx context.Context) error { return c.pingErr }

func (c *fakeCache) GetResult(ctx context.Context, fingerprint string, offset int64) (*cachedResult, int, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCalls++
	key := ResultKey(fingerprint, offset)
	if c.getResultErr != nil {
		return nil, 0, false, c.getResultErr
	}
	v, ok := c.results[key]
	if !ok {
		hook := c.onMiss
		c.onMiss = nil
		if hook != nil {
			c.mu.Unlock()
			hook()
			c.mu.Lock()
		}
		return nil, 0, false, nil
	}
	return v, c.ttls[key], true, nil
}

func (c *fakeCache) SetResult(ctx context.Context, fingerprint string, offset int64, v *cachedResult, ttlSeconds int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCalls++
	key := ResultKey(fingerprint, offset)
	c.results[key] = v
	c.ttls[key] = ttlSeconds
	return nil
}

func (c *fakeCache) GetHot(ctx context.Context, scope string) ([]*model.SearchHotKeyword, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows, ok := c.hot[scope]
	if !ok || !c.hotHit {
		return nil, false, nil
	}
	return rows, true, nil
}

func (c *fakeCache) SetHot(ctx context.Context, scope string, rows []*model.SearchHotKeyword, ttlSeconds int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hot[scope] = rows
	c.hotHit = true
	return nil
}

func (c *fakeCache) GetBlockSet(ctx context.Context) (map[string]struct{}, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.blockHit {
		return nil, false, nil
	}
	return c.blockSet, true, nil
}

func (c *fakeCache) SetBlockSet(ctx context.Context, words []string, ttlSeconds int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := make(map[string]struct{}, len(words))
	for _, w := range words {
		set[w] = struct{}{}
	}
	c.blockSet = set
	c.blockHit = true
	return nil
}

func (c *fakeCache) SuggestDict(ctx context.Context, window int) ([]redis.FloatPair, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dictErr != nil {
		return nil, c.dictErr
	}
	if window > 0 && len(c.dict) > window {
		return c.dict[:window], nil
	}
	return c.dict, nil
}

func (c *fakeCache) IncrQueryCounters(ctx context.Context, keywordHash, day string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counters = append(c.counters, keywordHash+"|"+day)
	return nil
}

// seededResult 预置一条结果缓存（模拟“上一次成功查询留下的副本”）。
func (c *fakeCache) seededResult(fp string, offset int64, ttl int, resp *esclient.SearchResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := ResultKey(fp, offset)
	c.results[key] = &cachedResult{Hits: resp.Hits.Hits, Total: resp.Hits.Total.Value, Relation: resp.Hits.Total.Relation, TookMs: resp.Took}
	c.ttls[key] = ttl
}

func (c *fakeCache) cachedHits(fp string, offset int64) (*cachedResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.results[ResultKey(fp, offset)]
	return v, ok
}

// ---------------------------------------------------------------- model 替身

type fakeHistory struct {
	mu sync.Mutex

	rows       []*model.SearchHistory
	nextID     int64
	upserts    []*model.SearchHistory
	pruneKeep  int64
	prefixRows []*model.SearchHistory

	upsertErr error
	listErr   error
	deleteErr error
}

func (h *fakeHistory) Upsert(ctx context.Context, row *model.SearchHistory) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.upsertErr != nil {
		return h.upsertErr
	}
	h.upserts = append(h.upserts, row)
	return nil
}

func (h *fakeHistory) ListByKeyset(ctx context.Context, mid int64, beforeMtime, beforeID int64, limit int32) ([]*model.SearchHistory, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listErr != nil {
		return nil, h.listErr
	}
	var out []*model.SearchHistory
	for _, row := range h.rows {
		if beforeMtime > 0 && !(row.Mtime < beforeMtime || (row.Mtime == beforeMtime && row.Id < beforeID)) {
			continue
		}
		out = append(out, row)
		if int32(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

func (h *fakeHistory) ListByPrefix(ctx context.Context, mid int64, prefix string, limit int32) ([]*model.SearchHistory, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listErr != nil {
		return nil, h.listErr
	}
	if h.prefixRows != nil {
		return h.prefixRows, nil
	}
	var out []*model.SearchHistory
	for _, row := range h.rows {
		if strings.HasPrefix(row.Keyword, prefix) {
			out = append(out, row)
		}
		if int32(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

func (h *fakeHistory) FindByKeyword(ctx context.Context, mid int64, keyword string) (*model.SearchHistory, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, row := range h.rows {
		if row.Keyword == keyword {
			return row, nil
		}
	}
	return nil, nil
}

func (h *fakeHistory) DeleteKeyword(ctx context.Context, mid int64, keyword string) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.deleteErr != nil {
		return 0, h.deleteErr
	}
	for i, row := range h.rows {
		if row.Keyword == keyword {
			h.rows = append(h.rows[:i], h.rows[i+1:]...)
			return 1, nil
		}
	}
	return 0, nil
}

func (h *fakeHistory) DeleteAll(ctx context.Context, mid int64) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.deleteErr != nil {
		return 0, h.deleteErr
	}
	n := int64(len(h.rows))
	h.rows = nil
	return n, nil
}

func (h *fakeHistory) Prune(ctx context.Context, mid int64, keep int64) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneKeep = keep
	return 0, nil
}

type fakeHot struct {
	mu sync.Mutex

	byScope map[string][]*model.SearchHotKeyword
	err     error
	calls   []string
}

func (s *fakeHot) ListByScope(ctx context.Context, scope string, limit int32) ([]*model.SearchHotKeyword, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, fmt.Sprintf("%s:%d", scope, limit))
	if s.err != nil {
		return nil, s.err
	}
	rows := s.byScope[scope]
	if limit > 0 && int32(len(rows)) > limit {
		rows = rows[:limit]
	}
	out := make([]*model.SearchHotKeyword, len(rows))
	copy(out, rows)
	return out, nil
}

func (s *fakeHot) LatestSnapshotAt(ctx context.Context, scope string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ts int64
	for _, row := range s.byScope[scope] {
		if row.SnapshotAt > ts {
			ts = row.SnapshotAt
		}
	}
	return ts, nil
}

func (s *fakeHot) UpsertSnapshot(ctx context.Context, h *model.SearchHotKeyword) error {
	return errors.New("fakeHot: snapshot writes must not happen on the query path")
}

func (s *fakeHot) PruneStale(ctx context.Context, scope string, before int64) (int64, error) {
	return 0, errors.New("fakeHot: prune must not happen on the query path")
}

type fakeBlock struct {
	mu sync.Mutex

	words     map[string]struct{} // 生效词
	active    []*model.SearchBlockWord
	blockErr  error
	listCalls int
}

func (b *fakeBlock) IsBlocked(ctx context.Context, keyword string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blockErr != nil {
		return false, b.blockErr
	}
	_, hit := b.words[keyword]
	return hit, nil
}

func (b *fakeBlock) ListActive(ctx context.Context, afterId int64, limit int32) ([]*model.SearchBlockWord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listCalls++
	if b.blockErr != nil {
		return nil, b.blockErr
	}
	out := b.active
	if limit > 0 && int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

type fakeLog struct {
	mu sync.Mutex

	inserted  bool
	calls     []*model.SearchQueryLog
	sessionTx []sqlx.Session
	err       error
}

func (l *fakeLog) InsertIgnore(ctx context.Context, tx sqlx.Session, row *model.SearchQueryLog) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, row)
	l.sessionTx = append(l.sessionTx, tx)
	if l.err != nil {
		return false, l.err
	}
	return l.inserted, nil
}

func (l *fakeLog) FindByQueryId(ctx context.Context, queryId string) (*model.SearchQueryLog, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) == 0 {
		return nil, nil
	}
	return l.calls[0], nil
}

type fakeOutbox struct {
	mu sync.Mutex

	inserted   []*model.SearchOutbox
	sessionTx  []sqlx.Session
	eventID    string
	err        error
	lookupErr  error
	lookupCall int
}

func (o *fakeOutbox) Insert(ctx context.Context, tx sqlx.Session, out *model.SearchOutbox) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	o.inserted = append(o.inserted, out)
	o.sessionTx = append(o.sessionTx, tx)
	return nil
}

func (o *fakeOutbox) FindEventIdByAggregate(ctx context.Context, aggregateType, aggregateId string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lookupCall++
	if o.lookupErr != nil {
		return "", o.lookupErr
	}
	return o.eventID, nil
}

// ---------------------------------------------------------------- 其它替身

// fakeResult 实现 sql.Result（fakeConn 的 ExecCtx 返回值）。
type fakeResult struct{ affected int64 }

func (r fakeResult) LastInsertId() (int64, error) { return 1, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.affected, nil }

// fakeConn 记录事务内的 SQL 调用；不会真的连接数据库（也不 import driver）。
type fakeConn struct {
	mu sync.Mutex

	stmts []string
	inTx  bool

	execErr error
}

func (c *fakeConn) record(query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = append(c.stmts, query)
}

func (c *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.mu.Lock()
	c.inTx = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inTx = false
		c.mu.Unlock()
	}()
	return fn(ctx, c)
}

func (c *fakeConn) Transact(fn func(sqlx.Session) error) error {
	return errors.New("fakeConn: Transact without ctx unused")
}

func (c *fakeConn) RawDB() (*sql.DB, error) {
	return nil, errors.New("fakeConn: RawDB disabled in tests")
}

func (c *fakeConn) ExecCtx(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.record(query)
	if c.execErr != nil {
		return nil, c.execErr
	}
	return fakeResult{affected: 1}, nil
}

func (c *fakeConn) Exec(query string, args ...any) (sql.Result, error) {
	return c.ExecCtx(context.Background(), query, args...)
}

func (c *fakeConn) Prepare(query string) (sqlx.StmtSession, error) {
	return nil, errors.New("fakeConn: Prepare unused")
}

func (c *fakeConn) PrepareCtx(ctx context.Context, query string) (sqlx.StmtSession, error) {
	return nil, errors.New("fakeConn: PrepareCtx unused")
}

func (c *fakeConn) QueryRowCtx(ctx context.Context, v any, query string, args ...any) error {
	c.record(query)
	return sql.ErrNoRows
}

func (c *fakeConn) QueryRow(v any, query string, args ...any) error {
	return sql.ErrNoRows
}

func (c *fakeConn) QueryRowPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	return sql.ErrNoRows
}

func (c *fakeConn) QueryRowPartial(v any, query string, args ...any) error { return sql.ErrNoRows }

func (c *fakeConn) QueryRowsCtx(ctx context.Context, v any, query string, args ...any) error {
	c.record(query)
	return sql.ErrNoRows
}

func (c *fakeConn) QueryRows(v any, query string, args ...any) error { return sql.ErrNoRows }

func (c *fakeConn) QueryRowsPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	return sql.ErrNoRows
}

func (c *fakeConn) QueryRowsPartial(v any, query string, args ...any) error { return sql.ErrNoRows }

// ---------------------------------------------------------------- 构造 helper

// testConfig 生成一份带明确限制的测试配置（默认值刻意偏小，便于断言边界）。
func testConfig() config.Config {
	var c config.Config
	c.Search = config.SearchConf{
		PsDefault:                30,
		PsLimit:                  50,
		MaxOffset:                900,
		KeywordMaxLen:            64,
		CacheTTLSeconds:          30,
		DegradeEnabled:           true,
		SuggestLimit:             10,
		SuggestCandidateWindow:   500,
		HotKeywordLimit:          20,
		HistoryLimit:             30,
		HistoryEnabled:           true,
		BlockWordCacheTTLSeconds: 60,
		DefaultSort:              1,
	}
	c.OpenSearch.TimeoutMs = 1500
	c.OpenSearch.MaxResultWindow = 10000
	return c
}

// newTestRepo 组装可注入替身的 Repository。
func newTestRepo(conn sqlx.SqlConn, eng engine, cache CacheStore, c config.Config, m Models) *Repository {
	return NewWithDeps(conn, eng, cache, c, m)
}

// modelsOf 把各替身打包成 Models。
func modelsOf(h model.SearchHistoryModel, l model.SearchQueryLogModel, hot model.SearchHotKeywordModel,
	b model.SearchBlockWordModel, o model.SearchOutboxModel) Models {
	return Models{History: h, Log: l, Hot: hot, Block: b, Outbox: o}
}

// mustFingerprint 计算与 Search 一致的指纹（测试断言缓存 key 时使用）。
func mustFingerprint(t *testing.T, p SearchParams) string {
	t.Helper()
	if p.Fingerprint == "" {
		p.Fingerprint = QueryFingerprint(p)
	}
	return p.Fingerprint
}
