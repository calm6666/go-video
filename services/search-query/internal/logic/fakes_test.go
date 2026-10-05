package logic

// fakes_test.go 是 search-query logic 测试的内存替身集合。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL + 真 OpenSearch），测试无处塞替身。
// 因此本包用例统一用 repository.NewWithDeps(内存引擎, 内存缓存, fakeConn, cfg, 5 个 model 替身)
// 组装**真实的 Repository**，只把它的依赖换成替身——这样关键词规范化、指纹与游标、深分页保护、
// DSL 构造、结果缓存/降级判定、屏蔽词出口过滤、历史幂等写、上报同事务这些判定链
// 全都落在被测路径上，而不是把 Repository 也 mock 掉。
// 唯一为测试新增的生产侧声明是 repository.CachedResult（cachedResult 的导出别名）：
// CacheStore 的签名带这个未导出载荷类型，外部包无法书写它，见 cache.go 注释。
//
// 四条替身纪律（catalog / rights / playback 三轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**（含缓存载荷与 model 行）：logic 里的写回不得污染库存行，
//     否则「有没有真的落库/真的写缓存」这类断言会被共享指针掩盖。
//  2. 写入按真实 SQL 的口径处理主键与唯一键：Insert/Upsert 忽略入参 ID、自增分配，
//     (mid, keyword) / query_id / event_id 冲突按各自 SQL 的 ON DUPLICATE / INSERT IGNORE 语义，
//     查无此行返回 (nil, nil)（与 model 一致，不返回 ErrNoRows）。
//  3. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：本服务要紧的结论是
//     「先拦屏蔽词还是先打引擎」「查询日志与事件是否同一事务」「降级时有几次引擎调用」。
//  4. 布数据走替身的**静默写入路径**（seed* / warm* / put，不记轨迹）：布景不算被测调用，
//     因此轨迹断言可以直接从 0 开始数；需要在多次调用之间比较时显式取 before := st.log.snapshot()。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**（state 门槛、keyset 条件、ORDER BY 方向、LIMIT 截断、
//     INSERT IGNORE 的 0 行受影响），不证明 SQL、列名与占位符本身；仓库没有 search-query 的
//     迁移↔model 列级对账门禁，见 README 已知缺口；
//   - 引擎侧只断言到「请求体结构 + 命中投影」，不验证 OpenSearch 真实召回与分词；
//   - 熔断器（esclient 的 breaker）与 Redis ZSET 由替身按同语义模拟，不验证真实实现；
//   - 事务只断言「两次写入拿到同一个非 nil 会话、且以失败收场时整体回滚」，原子性由 MySQL 保证。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/search-query/internal/config"
	"go-video/services/search-query/internal/esclient"
	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/internal/svc"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

// --- 断言小工具（本包共享） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵（logic/repository 用 %w 包装下游错误是允许的）。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want errors.Is(%v)", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：%v", label, err)
	}
}

// wantStringsEQ 比较字符串切片（wantEQ 受 comparable 约束，切片只能另走这条）。
func wantStringsEQ(t *testing.T, label, field string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantSliceEQ 比较任意 comparable 元素切片（枚举切片如 []rpc.SortMode）。
func wantSliceEQ[T comparable](t *testing.T, label, field string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantNoCall 断言 from 之后没有发生任何依赖调用（守卫必须发生在触引擎/触缓存/触库之前）。
func wantNoCall(t *testing.T, label string, st *store, from int) {
	t.Helper()
	if ops := st.log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

func wantOps(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label, strings.Join(got, " → "), strings.Join(want, " → "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s：第 %d 次调用 = %s, want %s（完整序列 [%s]）",
				label, i+1, got[i], want[i], strings.Join(got, " → "))
		}
	}
}

// wantOpsFrom 断言第 from 次调用之后的子序列（多次调用之间的比较用 snapshot 取 from）。
func wantOpsFrom(t *testing.T, label string, log *callLog, from int, want []string) {
	t.Helper()
	wantOps(t, label, log.opsFrom(from), want)
}

// wantOpsAt 断言第 idx 次调用（0 起）恰好是 want：只关心序列中某一个位置、
// 后续调用又不想全列出来时用这条（越界即失败，不静默跳过）。
func wantOpsAt(t *testing.T, label string, st *store, idx int, want string) {
	t.Helper()
	if idx >= len(st.log.ops) {
		t.Fatalf("%s：总共只有 %d 次调用，取不到第 %d 次（完整序列 [%s]）",
			label, len(st.log.ops), idx, strings.Join(st.log.ops, " → "))
	}
	wantEQ(t, label, "第 "+itoa(int64(idx+1))+" 次调用", st.log.ops[idx], want)
}

// wantNoOpsWith 断言 from 之后没有任何以 prefixes 之一的调用（「这一类下游一次都没碰」）。
func wantNoOpsWith(t *testing.T, label string, log *callLog, from int, prefixes ...string) {
	t.Helper()
	for _, op := range log.opsFrom(from) {
		for _, p := range prefixes {
			if strings.HasPrefix(op, p) {
				t.Errorf("%s：不应发生 %s 调用，实际 %s（完整序列 [%s]）",
					label, p, op, strings.Join(log.opsFrom(from), " → "))
			}
		}
	}
}

// wantCount 断言某类调用发生的次数（与 wantOps 互补：只关心「一次都没发生」时用这条）。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整序列 [%s]）", label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

// itoa 拼调用轨迹里的整数段：轨迹由替身用 %d 格式化，期望值必须同源。
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

func (c *callLog) countPrefix(prefix string) int {
	n := 0
	for _, o := range c.ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

func (c *callLog) snapshot() int             { return len(c.ops) }
func (c *callLog) opsFrom(from int) []string { return c.ops[from:] }

// --- 错误注入（按方法粒度） ---

type faultInjector struct{ by map[string]error }

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

func (f *faultInjector) fail(method string) error { return f.by[method] }

// --- 引擎替身（实现 repository 的 engine 接口） ---

// fakeEngine 复刻 *esclient.Client 的可观察契约：Available 只看是否配置、
// Healthy 额外反映熔断状态、Search 收到的是已构造好的 DSL 请求体。
type fakeEngine struct {
	faultInjector
	log         *callLog
	available   bool
	circuitOpen bool
	alias       string
	window      int64

	// resp 是默认响应；searchFn 非 nil 时按调用序号自定义返回（分页/联想回源用）。
	resp     *esclient.SearchResponse
	searchFn func(seq int, body []byte) (*esclient.SearchResponse, error)
	bodies   []string
}

func (e *fakeEngine) Available() bool { return e.available }

func (e *fakeEngine) Healthy() bool { return e.available && !e.circuitOpen }

func (e *fakeEngine) Alias() string { return e.alias }

func (e *fakeEngine) MaxResultWindow() int64 { return e.window }

func (e *fakeEngine) Search(_ context.Context, body []byte) (*esclient.SearchResponse, error) {
	var p struct {
		From int64 `json:"from"`
		Size int32 `json:"size"`
	}
	_ = json.Unmarshal(body, &p)
	e.log.add("es.Search:%d/%d", p.From, p.Size)
	e.bodies = append(e.bodies, string(body))
	// 与真熔断器同口径：打开状态下快速失败，不再发 HTTP 请求。
	if e.circuitOpen {
		return nil, esclient.ErrCircuitOpen
	}
	if err := e.fail("Search"); err != nil {
		return nil, err
	}
	seq := len(e.bodies)
	if e.searchFn != nil {
		return e.searchFn(seq, body)
	}
	if e.resp == nil {
		return respWith(0, "eq", 0), nil
	}
	return e.resp, nil
}

func (e *fakeEngine) Count(_ context.Context, _ []byte) (int64, error) {
	e.log.add("es.Count")
	return 0, e.fail("Count")
}

// body 取第 n 次引擎请求体（1 起）。
func (e *fakeEngine) body(t *testing.T, n int) map[string]any {
	t.Helper()
	if n < 1 || n > len(e.bodies) {
		t.Fatalf("引擎只被调用 %d 次，取不到第 %d 次请求体", len(e.bodies), n)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(e.bodies[n-1]), &m); err != nil {
		t.Fatalf("引擎请求体不是合法 JSON：%v", err)
	}
	return m
}

// respWith 构造引擎响应。
func respWith(total int64, relation string, took int64, hits ...esclient.Hit) *esclient.SearchResponse {
	return &esclient.SearchResponse{
		Took: took,
		Hits: esclient.HitsBlock{
			Total: esclient.TotalBlock{Value: total, Relation: relation},
			Hits:  hits,
		},
	}
}

// hitOf 构造一条命中。engineID 刻意与 source.doc_id 不同：投影必须取文档字段，
// 不能把引擎物理 _id（别名切换期的文档主键）当业务主键下发给客户端。
func hitOf(engineID string, score float64, src esclient.SourceDoc, highlights map[string][]string) esclient.Hit {
	return esclient.Hit{
		Index:      "go_video_search_v1",
		ID:         json.RawMessage(strconv.Quote(engineID)),
		Score:      score,
		Source:     src,
		Highlights: highlights,
	}
}

// --- 缓存替身（实现 repository.CacheStore） ---

// fakeCache 复刻 *Cache 的口径：miss 返回 hit=false 且 err=nil；SuggestDict 按窗口截断；
// 结果缓存 key 复用 repository.ResultKey（同源，避免用例与实现各算一套 key）。
type fakeCache struct {
	faultInjector
	log      *callLog
	results  map[string]*repository.CachedResult
	ttls     map[string]int
	hot      map[string][]*model.SearchHotKeyword
	hotHit   map[string]bool
	block    []string
	blockOn  bool
	dict     []redis.FloatPair
	counters []string

	// lastFP 记录最近一次结果缓存读/写用的查询指纹：用例据此复原缓存 key 与游标，
	// 避免在用例里重算一套指纹（那会变成「断言实现等于实现自己」）。
	lastFP string
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{
		log:     log,
		results: map[string]*repository.CachedResult{},
		ttls:    map[string]int{},
		hot:     map[string][]*model.SearchHotKeyword{},
		hotHit:  map[string]bool{},
	}
}

func (f *fakeCache) Ping(context.Context) error {
	f.log.add("cache.Ping")
	return f.fail("Ping")
}

func (f *fakeCache) GetResult(_ context.Context, fingerprint string, offset int64) (*repository.CachedResult, int, bool, error) {
	f.lastFP = fingerprint
	key := repository.ResultKey(fingerprint, offset)
	f.log.add("cache.GetResult:%s", key)
	if err := f.fail("GetResult"); err != nil {
		return nil, 0, false, err
	}
	v, ok := f.results[key]
	if !ok {
		return nil, 0, false, nil
	}
	cp := *v
	cp.Hits = append([]esclient.Hit(nil), v.Hits...)
	return &cp, f.ttls[key], true, nil
}

func (f *fakeCache) SetResult(_ context.Context, fingerprint string, offset int64, v *repository.CachedResult, ttlSeconds int) error {
	f.lastFP = fingerprint
	key := repository.ResultKey(fingerprint, offset)
	// 真实 Cache 在 ttl<=0 时直接返回（缓存被关闭），不发 Setex。
	if ttlSeconds <= 0 {
		return nil
	}
	if err := f.fail("SetResult"); err != nil {
		return err
	}
	f.log.add("cache.SetResult:%s/%d", key, ttlSeconds)
	cp := *v
	cp.Hits = append([]esclient.Hit(nil), v.Hits...)
	f.results[key] = &cp
	f.ttls[key] = ttlSeconds
	return nil
}

func (f *fakeCache) GetHot(_ context.Context, scope string) ([]*model.SearchHotKeyword, bool, error) {
	f.log.add("cache.GetHot:%s", scope)
	if err := f.fail("GetHot"); err != nil {
		return nil, false, err
	}
	if !f.hotHit[scope] {
		return nil, false, nil
	}
	return cloneHotRows(f.hot[scope]), true, nil
}

func (f *fakeCache) SetHot(_ context.Context, scope string, rows []*model.SearchHotKeyword, ttlSeconds int) error {
	// 与真实 Cache 同口径：ttl<=0 表示缓存关闭，不发任何 Redis 命令、也不留下可读到的副本，
	// 因此注入的故障同样不该触发（真实现代到不了 Redis 客户端）。
	if ttlSeconds <= 0 {
		return nil
	}
	if err := f.fail("SetHot"); err != nil {
		return err
	}
	f.log.add("cache.SetHot:%s/%d", scope, ttlSeconds)
	f.hot[scope] = cloneHotRows(rows)
	f.hotHit[scope] = true
	return nil
}

func (f *fakeCache) GetBlockSet(context.Context) (map[string]struct{}, bool, error) {
	f.log.add("cache.GetBlockSet")
	if err := f.fail("GetBlockSet"); err != nil {
		return nil, false, err
	}
	if !f.blockOn {
		return nil, false, nil
	}
	set := make(map[string]struct{}, len(f.block))
	for _, w := range f.block {
		set[w] = struct{}{}
	}
	return set, true, nil
}

func (f *fakeCache) SetBlockSet(_ context.Context, words []string, ttlSeconds int) error {
	// 同 SetHot：ttl<=0 时真实 Cache 不发命令。
	if ttlSeconds <= 0 {
		return nil
	}
	if err := f.fail("SetBlockSet"); err != nil {
		return err
	}
	f.log.add("cache.SetBlockSet:%d/%d", len(words), ttlSeconds)
	f.block = append([]string(nil), words...)
	f.blockOn = true
	return nil
}

func (f *fakeCache) SuggestDict(_ context.Context, window int) ([]redis.FloatPair, error) {
	f.log.add("cache.SuggestDict:%d", window)
	if err := f.fail("SuggestDict"); err != nil {
		return nil, err
	}
	if window <= 0 {
		return nil, nil // 与真实 Cache 一致：window<=0 不发命令
	}
	if len(f.dict) > window {
		return append([]redis.FloatPair(nil), f.dict[:window]...), nil
	}
	return append([]redis.FloatPair(nil), f.dict...), nil
}

func (f *fakeCache) IncrQueryCounters(_ context.Context, keywordHash, day string) error {
	f.log.add("cache.IncrCounters:%s/%s", keywordHash, day)
	return f.fail("IncrQueryCounters")
}

// warmResult 静默写入结果缓存（纪律 4：布景不记轨迹）。
func (f *fakeCache) warmResult(fingerprint string, offset int64, ttl int, resp *esclient.SearchResponse) {
	key := repository.ResultKey(fingerprint, offset)
	f.results[key] = &repository.CachedResult{
		Hits:     append([]esclient.Hit(nil), resp.Hits.Hits...),
		Total:    resp.Hits.Total.Value,
		Relation: resp.Hits.Total.Relation,
		TookMs:   resp.Took,
	}
	f.ttls[key] = ttl
}

// warmHot 静默写入某 scope 的热词缓存。
func (f *fakeCache) warmHot(scope string, rows ...*model.SearchHotKeyword) {
	f.hot[scope] = cloneHotRows(rows)
	f.hotHit[scope] = true
}

// warmBlockSet 静默写入屏蔽词集合缓存（出口过滤用，不等于 IsBlocked 的判定源）。
func (f *fakeCache) warmBlockSet(words ...string) {
	f.block = append([]string(nil), words...)
	f.blockOn = true
}

// warmDict 静默写入联想词典。
func (f *fakeCache) warmDict(pairs ...redis.FloatPair) {
	f.dict = append([]redis.FloatPair(nil), pairs...)
}

// resultTTLOf 读缓存里实际存的 TTL（断言「只由引擎成功路径写入、命中不续期」）。
func (f *fakeCache) resultTTLOf(t *testing.T, offset int64) int {
	t.Helper()
	if len(f.ttls) != 1 {
		t.Fatalf("结果缓存条目数 = %d, want 1", len(f.ttls))
	}
	for key, ttl := range f.ttls {
		if !strings.HasSuffix(key, ":"+itoa(offset)) {
			t.Fatalf("缓存 key %s 的 offset 段不是 %d", key, offset)
		}
		return ttl
	}
	panic("unreachable")
}

// onlyResultKey 返回唯一那条结果缓存 key。
func (f *fakeCache) onlyResultKey(t *testing.T) string {
	t.Helper()
	if len(f.results) != 1 {
		t.Fatalf("结果缓存条目数 = %d, want 1", len(f.results))
	}
	for key := range f.results {
		return key
	}
	panic("unreachable")
}

// --- 搜索历史 model 替身 ---

type fakeHistory struct {
	faultInjector
	log    *callLog
	rows   []*model.SearchHistory
	nextID int64
}

func newFakeHistory(log *callLog) *fakeHistory {
	return &fakeHistory{log: log}
}

// seed 静默布一行历史（纪律 4）。返回库内那一行的指针，只用于取主键。
func (f *fakeHistory) seed(h *model.SearchHistory) *model.SearchHistory {
	f.nextID++
	cp := *h
	cp.Id = f.nextID
	if cp.KeywordHash == "" {
		cp.KeywordHash = model.KeywordHash(cp.Keyword)
	}
	f.rows = append(f.rows, &cp)
	return &cp
}

func (f *fakeHistory) Upsert(_ context.Context, h *model.SearchHistory) error {
	f.log.add("history.Upsert:%d/%s", h.Mid, h.Keyword)
	if err := f.fail("Upsert"); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, row := range f.rows {
		if row.Mid == h.Mid && row.Keyword == h.Keyword { // 唯一索引 (mid, keyword)
			row.Platform = h.Platform
			row.State = h.State
			row.Mtime = now
			return nil
		}
	}
	f.nextID++
	stored := *h
	stored.Id = f.nextID
	if stored.KeywordHash == "" {
		stored.KeywordHash = model.KeywordHash(stored.Keyword)
	}
	if stored.Ctime == 0 {
		stored.Ctime = now
	}
	stored.Mtime = now
	f.rows = append(f.rows, &stored)
	return nil
}

// ListByKeyset 复刻 WHERE mid=? AND state=normal AND (?=0 OR mtime<? OR (mtime=? AND id<?))
// ORDER BY mtime DESC, id DESC LIMIT ?
func (f *fakeHistory) ListByKeyset(_ context.Context, mid, beforeMtime, beforeID int64, limit int32) ([]*model.SearchHistory, error) {
	f.log.add("history.ListByKeyset:%d/%d/%d/%d", mid, beforeMtime, beforeID, limit)
	if err := f.fail("ListByKeyset"); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, model.ErrInvalidPage
	}
	matched := f.sortedBy(func(a, b *model.SearchHistory) bool {
		if a.Mtime != b.Mtime {
			return a.Mtime > b.Mtime
		}
		return a.Id > b.Id
	}, func(row *model.SearchHistory) bool {
		if row.Mid != mid || row.State != model.HistoryStateNormal {
			return false
		}
		if beforeMtime == 0 {
			return true
		}
		return row.Mtime < beforeMtime || (row.Mtime == beforeMtime && row.Id < beforeID)
	})
	return clipRows(matched, limit), nil
}

// ListByPrefix 复刻 WHERE mid=? AND state=normal AND keyword LIKE 'prefix%' ORDER BY mtime DESC, id DESC。
func (f *fakeHistory) ListByPrefix(_ context.Context, mid int64, prefix string, limit int32) ([]*model.SearchHistory, error) {
	f.log.add("history.ListByPrefix:%d/%s/%d", mid, prefix, limit)
	if err := f.fail("ListByPrefix"); err != nil {
		return nil, err
	}
	if prefix == "" || limit <= 0 {
		return nil, nil // 与 model 一致：空前缀不发查询
	}
	matched := f.sortedBy(func(a, b *model.SearchHistory) bool {
		if a.Mtime != b.Mtime {
			return a.Mtime > b.Mtime
		}
		return a.Id > b.Id
	}, func(row *model.SearchHistory) bool {
		return row.Mid == mid && row.State == model.HistoryStateNormal && strings.HasPrefix(row.Keyword, prefix)
	})
	return clipRows(matched, limit), nil
}

// FindByKeyword 与 model 一样按 keyword_hash 定位；查无此行返回 (nil, nil)。
func (f *fakeHistory) FindByKeyword(_ context.Context, mid int64, keyword string) (*model.SearchHistory, error) {
	f.log.add("history.FindByKeyword:%d/%s", mid, keyword)
	if err := f.fail("FindByKeyword"); err != nil {
		return nil, err
	}
	hash := model.KeywordHash(keyword)
	for _, row := range f.rows {
		if row.Mid == mid && row.KeywordHash == hash {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

// DeleteKeyword 复刻 DELETE ... WHERE mid=? AND keyword_hash=?（按哈希精确定位，返回受影响行数）。
func (f *fakeHistory) DeleteKeyword(_ context.Context, mid int64, keyword string) (int64, error) {
	f.log.add("history.DeleteKeyword:%d/%s", mid, keyword)
	if err := f.fail("DeleteKeyword"); err != nil {
		return 0, err
	}
	hash := model.KeywordHash(keyword)
	var n int64
	kept := f.rows[:0]
	for _, row := range f.rows {
		if row.Mid == mid && row.KeywordHash == hash {
			n++
			continue
		}
		kept = append(kept, row)
	}
	f.rows = kept
	return n, nil
}

func (f *fakeHistory) DeleteAll(_ context.Context, mid int64) (int64, error) {
	f.log.add("history.DeleteAll:%d", mid)
	if err := f.fail("DeleteAll"); err != nil {
		return 0, err
	}
	var n int64
	kept := f.rows[:0]
	for _, row := range f.rows {
		if row.Mid == mid {
			n++
			continue
		}
		kept = append(kept, row)
	}
	f.rows = kept
	return n, nil
}

func (f *fakeHistory) Prune(_ context.Context, mid int64, keep int64) (int64, error) {
	f.log.add("history.Prune:%d/%d", mid, keep)
	if err := f.fail("Prune"); err != nil {
		return 0, err
	}
	if keep <= 0 {
		return 0, nil
	}
	rows := f.sortedBy(func(a, b *model.SearchHistory) bool {
		if a.Mtime != b.Mtime {
			return a.Mtime > b.Mtime
		}
		return a.Id > b.Id
	}, func(row *model.SearchHistory) bool { return row.Mid == mid })
	if int64(len(rows)) <= keep {
		return 0, nil
	}
	var n int64
	for _, row := range rows[keep:] {
		for i, all := range f.rows {
			if all.Id == row.Id {
				f.rows = append(f.rows[:i], f.rows[i+1:]...)
				n++
				break
			}
		}
	}
	return n, nil
}

// sortedBy 返回满足过滤条件并按 less 排序的**行副本**切片（纪律 1）。
func (f *fakeHistory) sortedBy(less func(a, b *model.SearchHistory) bool, keep func(*model.SearchHistory) bool) []*model.SearchHistory {
	var ids []*model.SearchHistory
	for _, row := range f.rows {
		if keep(row) {
			cp := *row
			ids = append(ids, &cp)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return less(ids[i], ids[j]) })
	return ids
}

// seedRow 静默布一行搜索历史（纪律 4），只给业务字段：
// 主键自增、keyword_hash 由 seed 按规范化词补齐。历史三个接口的用例共用。
func (f *fakeHistory) seedRow(mid int64, keyword string, state int32, platform string, ctime, mtime int64) *model.SearchHistory {
	return f.seed(&model.SearchHistory{
		Mid: mid, Keyword: keyword, State: state, Platform: platform, Ctime: ctime, Mtime: mtime,
	})
}

// get 读库内某词的当前行（值读）。
func (f *fakeHistory) get(mid int64, keyword string) (*model.SearchHistory, bool) {
	for _, row := range f.rows {
		if row.Mid == mid && row.Keyword == keyword {
			cp := *row
			return &cp, true
		}
	}
	return nil, false
}

func (f *fakeHistory) countBy(mid int64) int {
	n := 0
	for _, row := range f.rows {
		if row.Mid == mid {
			n++
		}
	}
	return n
}

func clipRows[T any](rows []T, limit int32) []T {
	if limit > 0 && int32(len(rows)) > limit {
		return rows[:limit]
	}
	return rows
}

// --- 查询日志 model 替身 ---

type fakeQueryLog struct {
	faultInjector
	log    *callLog
	rows   []*model.SearchQueryLog
	gotTx  map[string]bool // query_id -> InsertIgnore 是否拿到事务会话
	nextID int64
}

func newFakeQueryLog(log *callLog) *fakeQueryLog {
	return &fakeQueryLog{log: log, gotTx: map[string]bool{}}
}

// seed 静默布一行（模拟「上一次上报已占住 query_id 唯一索引」）。
func (f *fakeQueryLog) seed(l *model.SearchQueryLog) *model.SearchQueryLog {
	f.nextID++
	cp := *l
	cp.Id = f.nextID
	f.rows = append(f.rows, &cp)
	return &cp
}

// InsertIgnore 复刻 INSERT IGNORE：query_id 已存在时 0 行受影响 -> inserted=false。
func (f *fakeQueryLog) InsertIgnore(_ context.Context, tx sqlx.Session, l *model.SearchQueryLog) (bool, error) {
	f.log.add("log.InsertIgnore:%s", l.QueryId)
	if err := f.fail("InsertIgnore"); err != nil {
		return false, err
	}
	if l.QueryId == "" {
		return false, model.ErrInvalidQueryID
	}
	if !model.IsValidResultState(l.ResultState) {
		return false, fmt.Errorf("search_query_log InsertIgnore: invalid result_state %q", l.ResultState)
	}
	f.gotTx[l.QueryId] = tx != nil
	for _, row := range f.rows {
		if row.QueryId == l.QueryId {
			return false, nil // 唯一索引冲突，静默跳过
		}
	}
	f.nextID++
	stored := *l
	stored.Id = f.nextID
	if stored.KeywordHash == "" {
		stored.KeywordHash = model.KeywordHash(stored.Keyword)
	}
	f.rows = append(f.rows, &stored)
	return true, nil
}

func (f *fakeQueryLog) FindByQueryId(_ context.Context, queryID string) (*model.SearchQueryLog, error) {
	f.log.add("log.FindByQueryId:%s", queryID)
	if err := f.fail("FindByQueryId"); err != nil {
		return nil, err
	}
	for _, row := range f.rows {
		if row.QueryId == queryID {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

// only 返回库里唯一那行（多行时立即失败，避免断言写歪）。
func (f *fakeQueryLog) only(t *testing.T) *model.SearchQueryLog {
	t.Helper()
	if len(f.rows) != 1 {
		t.Fatalf("search_query_log 行数 = %d, want 1", len(f.rows))
	}
	cp := *f.rows[0]
	return &cp
}

// --- 热词快照 model 替身 ---

type fakeHot struct {
	faultInjector
	log    *callLog
	rows   []*model.SearchHotKeyword
	nextID int64
}

func newFakeHot(log *callLog) *fakeHot { return &fakeHot{log: log} }

// seed 静默布一行快照。
func (f *fakeHot) seed(scope, keyword string, score float64, snapshotAt int64) *model.SearchHotKeyword {
	f.nextID++
	row := &model.SearchHotKeyword{Id: f.nextID, Scope: scope, Keyword: keyword, Score: score, SnapshotAt: snapshotAt}
	f.rows = append(f.rows, row)
	return row
}

// ListByScope 复刻 WHERE scope=? ORDER BY score DESC, id ASC LIMIT ?
func (f *fakeHot) ListByScope(_ context.Context, scope string, limit int32) ([]*model.SearchHotKeyword, error) {
	f.log.add("hot.ListByScope:%s/%d", scope, limit)
	if err := f.fail("ListByScope"); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	matched := cloneHotRows(f.rows)
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Score != matched[j].Score {
			return matched[i].Score > matched[j].Score
		}
		return matched[i].Id < matched[j].Id
	})
	var out []*model.SearchHotKeyword
	for _, row := range matched {
		if row.Scope != scope {
			continue
		}
		out = append(out, row)
		if int32(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeHot) LatestSnapshotAt(_ context.Context, scope string) (int64, error) {
	f.log.add("hot.LatestSnapshotAt:%s", scope)
	var ts int64
	for _, row := range f.rows {
		if row.Scope == scope && row.SnapshotAt > ts {
			ts = row.SnapshotAt
		}
	}
	return ts, nil
}

// UpsertSnapshot / PruneStale 属于聚合任务写侧：查询链路调用它们就是越界，直接失败暴露。
func (f *fakeHot) UpsertSnapshot(context.Context, *model.SearchHotKeyword) error {
	f.log.add("hot.UpsertSnapshot")
	return fmt.Errorf("fakeHot: 查询链路不允许写快照")
}

func (f *fakeHot) PruneStale(context.Context, string, int64) (int64, error) {
	f.log.add("hot.PruneStale")
	return 0, fmt.Errorf("fakeHot: 查询链路不允许清理快照")
}

func cloneHotRows(rows []*model.SearchHotKeyword) []*model.SearchHotKeyword {
	out := make([]*model.SearchHotKeyword, 0, len(rows))
	for _, r := range rows {
		cp := *r
		out = append(out, &cp)
	}
	return out
}

// --- 屏蔽词 model 替身 ---

type fakeBlock struct {
	faultInjector
	log  *callLog
	rows []*model.SearchBlockWord
	next int64
}

func newFakeBlock(log *callLog) *fakeBlock { return &fakeBlock{log: log} }

// seedWord 静默布一个屏蔽词（state=model.BlockWordState*）。
func (f *fakeBlock) seedWord(word string, state int32) *model.SearchBlockWord {
	f.next++
	row := &model.SearchBlockWord{Id: f.next, Word: word, State: state, Operator: "test"}
	f.rows = append(f.rows, row)
	return row
}

// IsBlocked 复刻 SELECT id ... WHERE word=? AND state=active LIMIT 1。
func (f *fakeBlock) IsBlocked(_ context.Context, keyword string) (bool, error) {
	f.log.add("block.IsBlocked:%s", keyword)
	if err := f.fail("IsBlocked"); err != nil {
		return false, err
	}
	if keyword == "" {
		return false, model.ErrInvalidKeyword
	}
	for _, row := range f.rows {
		if row.Word == keyword && row.State == model.BlockWordStateActive {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeBlock) ListActive(_ context.Context, afterID int64, limit int32) ([]*model.SearchBlockWord, error) {
	f.log.add("block.ListActive:%d/%d", afterID, limit)
	if err := f.fail("ListActive"); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	var out []*model.SearchBlockWord
	for _, row := range f.rows {
		if row.State != model.BlockWordStateActive || row.Id <= afterID {
			continue
		}
		cp := *row
		out = append(out, &cp)
		if int32(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

// --- Outbox model 替身 ---

type fakeOutbox struct {
	faultInjector
	log    *callLog
	rows   []*model.SearchOutbox
	byAgg  map[string]string // "aggregate_type/aggregate_id" -> event_id
	gotTx  map[string]bool
	nextID int64
}

func newFakeOutbox(log *callLog) *fakeOutbox {
	return &fakeOutbox{log: log, byAgg: map[string]string{}, gotTx: map[string]bool{}}
}

// seedEvent 静默布一行事件（模拟「首次上报已产生 event_id」，用于幂等回填断言）。
func (f *fakeOutbox) seedEvent(eventID, aggregateType, aggregateID string) {
	f.nextID++
	f.rows = append(f.rows, &model.SearchOutbox{
		Id: f.nextID, EventId: eventID, EventType: model.EventQueryReported,
		AggregateType: aggregateType, AggregateId: aggregateID, State: model.OutboxStatePending,
	})
	f.byAgg[aggregateType+"/"+aggregateID] = eventID
}

func (f *fakeOutbox) Insert(_ context.Context, tx sqlx.Session, out *model.SearchOutbox) error {
	f.log.add("outbox.Insert:%s/%s", out.EventType, out.AggregateId)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	if out.EventId == "" || out.EventType == "" {
		return fmt.Errorf("fakeOutbox: event_id/event_type 不得为空")
	}
	f.gotTx[out.EventId] = tx != nil
	for _, row := range f.rows {
		if row.EventId == out.EventId {
			return fmt.Errorf("fakeOutbox: event_id 唯一索引冲突 %s", out.EventId)
		}
	}
	f.nextID++
	cp := *out
	cp.Id = f.nextID
	f.rows = append(f.rows, &cp)
	f.byAgg[out.AggregateType+"/"+out.AggregateId] = out.EventId
	return nil
}

func (f *fakeOutbox) FindEventIdByAggregate(_ context.Context, aggregateType, aggregateID string) (string, error) {
	f.log.add("outbox.FindEventId:%s/%s", aggregateType, aggregateID)
	if err := f.fail("FindEventIdByAggregate"); err != nil {
		return "", err
	}
	return f.byAgg[aggregateType+"/"+aggregateID], nil
}

func (f *fakeOutbox) only(t *testing.T) *model.SearchOutbox {
	t.Helper()
	if len(f.rows) != 1 {
		t.Fatalf("search_outbox 行数 = %d, want 1", len(f.rows))
	}
	cp := *f.rows[0]
	return &cp
}

// --- 事务连接替身 ---

// fakeConn 只实现 logic 单测需要的 TransactCtx；其余方法落在嵌入的 nil 接口上会直接 panic，
// 等价于「logic 单测路径上不允许出现任何直连 SQL」——真出现了就是越界，让它炸出来。
type fakeConn struct {
	sqlx.SqlConn
	log        *callLog
	transact   int
	rolledBack int
}

func (c *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.log.add("tx.Begin")
	c.transact++
	if err := fn(ctx, c); err != nil {
		c.log.add("tx.Rollback")
		c.rolledBack++
		return err
	}
	c.log.add("tx.Commit")
	return nil
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

// --- 装配 ---

type store struct {
	cfg      config.Config
	log      *callLog
	eng      *fakeEngine
	cache    *fakeCache
	history  *fakeHistory
	queryLog *fakeQueryLog
	hot      *fakeHot
	block    *fakeBlock
	outbox   *fakeOutbox
	conn     *fakeConn
	repo     *repository.Repository
}

// testConfig 显式给出查询侧限制（不依赖 json:",default" 标签，那只在配置加载时生效）。
// 取值刻意与 etc/searchquery.v1.yaml 一致，边界断言因此对生产配置有效。
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
		DefaultSort:              int32(rpc.SortMode_SORT_COMPREHENSIVE),
	}
	c.OpenSearch.TimeoutMs = 1500
	c.OpenSearch.MaxResultWindow = 10000
	return c
}

// newStore 装配只带内存依赖的 ServiceContext：真实 Repository，依赖全为替身。
// 引擎默认「已配置且健康」，用例按需改 st.eng.available / circuitOpen / failWith。
func newStore(t *testing.T, cfg config.Config) *store {
	t.Helper()
	log := &callLog{}
	st := &store{
		cfg:      cfg,
		log:      log,
		eng:      &fakeEngine{log: log, available: true, alias: "go_video_content", window: cfg.OpenSearch.MaxResultWindow},
		cache:    newFakeCache(log),
		history:  newFakeHistory(log),
		queryLog: newFakeQueryLog(log),
		hot:      newFakeHot(log),
		block:    newFakeBlock(log),
		outbox:   newFakeOutbox(log),
		conn:     &fakeConn{log: log},
	}
	st.repo = repository.NewWithDeps(st.conn, st.eng, st.cache, cfg, repository.Models{
		History: st.history,
		Log:     st.queryLog,
		Hot:     st.hot,
		Block:   st.block,
		Outbox:  st.outbox,
	})
	return st
}

// svcCtx 返回可直接交给 New*Logic 的上下文。Engine 字段留 nil：8 个 logic 都不读它，
// 引擎能力一律通过 Repository 暴露（EngineAvailable/EngineHealthy）。
func (st *store) svcCtx() *svc.ServiceContext {
	return &svc.ServiceContext{Config: st.cfg, Repository: st.repo}
}

// --- 时间布景 ---

// nowMinus 相对真实 now 往前偏移：logic/repository 内部用 time.Now()（不可注入），
// 布景只留安全余量，不依赖秒级边界。
func nowMinus(delta int64) int64 { return time.Now().Unix() - delta }

// --- 跨文件复用的取值小工具 ---

// searchSearchReplyPs 取上一次 Search 真正发给引擎的每页条数（DSL 的 size 段）。
// 调用方拿不到已被丢弃的 reply 时用这条：值来自实现实际发出的请求体，
// 不在用例里重算 normalizePageSize（那等于把实现抄一遍，断言永远不会红）。
func searchSearchReplyPs(t *testing.T, st *store) int32 {
	t.Helper()
	n := len(st.eng.bodies)
	if n == 0 {
		t.Fatalf("引擎一次都没被调用，取不到实际页大小")
	}
	size, ok := st.eng.body(t, n)["size"].(float64)
	if !ok {
		t.Fatalf("第 %d 次引擎请求体没有数值 size 字段：%s", n, st.eng.bodies[n-1])
	}
	return int32(size)
}
