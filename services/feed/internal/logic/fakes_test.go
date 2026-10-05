package logic

// fakes_test.go 是 feed logic 用例的内存替身集合。
//
// 覆盖范围：feed.v1.rpc 的全部 8 个方法（PushFeed / PullFeed / ListUserFeed /
// PinFeed / UnpinFeed / GetUnreadCount / ClearUnread / DeleteFeed）。
// **8 个方法全部是已实现用例，没有未实现桩**：proto 里没有一个方法返回
// codes.Unimplemented，internal/logic 与 internal/repository 里也没有
// `ErrNotImplemented`/`Unimplemented` 字样（已用 Grep 核实），因此本轮不存在
// 「为桩写暴露桩状态的断言」这一类用例；桩清单为空。
//
// 各用例里引用的缺陷编号 D1…D23 在 services/feed/README.md 的《已知缺陷》表里逐条给了
// 「位置 + 后果」；本包文件头只写与该文件直接相关的那几条，编号全局唯一，不重复登记。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），logic 包无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, nil conn, 4 个内存 model) 组装**真实的 Repository**，
// 只把它的 5 个依赖换成替身——这样「写扩散 fan-out、ZSet 游标翻页、缓存 miss 回源与预热、
// 未读计数器与 DB 双写、置顶集合读写配对、删除时的收件箱清理」整条判定链都在被测路径上，
// 而不是把 Repository 也 mock 掉。见 internal/repository/repository.go 的 Cacher 注释。
//
// 五条替身纪律：
//  1. **每次读都返回值拷贝**：logic/repository 拿到的行是副本，就地改字段不会污染库存行；
//     否则「ZSet score 用的是哪一份 ctime」「软删有没有真的落库」会被共享指针掩盖。
//  2. **写侧照抄生产 SQL 的语义**：列集合、ON DUPLICATE 分支、WHERE 片段、GREATEST 不为负、
//     ORDER BY/LIMIT、以及「FindOne/FindMany 不过滤 state」「DeleteByFeedID 不看 RowsAffected」
//     逐条对齐 model/feedmodel.go（来源行号写在方法注释里）。未复刻的分支一律返回
//     errUnexpectedDependency，不返回假成功。
//  3. **有序 callLog**（`<表|cache>.<方法>:<键>`）：既数次数也断顺序。feed 要紧的结论全是
//     顺序与口径类的：fan-out 每人是否恰好「ZADD→投影→计数」四条、游标翻页是否只读一次、
//     守卫拒绝后一次依赖调用都不许发生、UnpinFeed 失败时缓存是否被跳过。
//     键**只放调用方可见的入参**（mid/feed_id/cursor/limit/delta），替身自己分配的自增主键
//     不进键；新号的正确性一律用「读回库里那一行」断言（见 st.outbox.lastID）。
//  4. **错误注入可指定第几次调用发作**（faultInjector.failOn(method, nth, err)）：
//     PushFeed 的 fan-out 循环里「只有第 2 个粉丝写失败」这种爆炸半径只能靠它复现。
//  5. **并发窗口用钩子**（st.raceBefore / st.checkRaces）：在两次被调方法之间插入改库动作，
//     模拟别的入口插队；钩子没触发就判失败，避免用例前提空转。
//
// 覆盖边界（如实声明，不假装覆盖）：
//   - 替身复刻的是 model 层 SQL 的**语义**，不证明 SQL、列名与索引命中；
//     那部分由 deploy/migrations/feed/000001_create_feed_tables.sql 与人工评审承担。
//   - **Redis 键格式与 TTL 不在本文件断言范围内**：键在真实 *Cache 内部拼
//     （repository.go:38-42 的 keyInbox/keyOutbox/keyPin/keyUnread/keyFollowers），
//     缝在 Cacher 之下，logic 用例只能看到 (mid, feedID) 级身份。键空间与「无 TTL」
//     这两条改由 internal/repository/keys_logic_test.go 白盒锁定。
//   - `feed:followers:{mid}` 的**只读性**由 Cacher 契约不含写方法在编译期锁死，
//     无需运行时断言；真实 Cache 对集合成员做 ParseInt 并跳过非数字（repository.go:80-85），
//     该分支属于 *Cache 实现，从 logic 侧不可达。
//   - `Cache.GetUnread` 用 `err == redis.Nil` 身份比较区分 miss（repository.go:230），
//     真实驱动是否总返回该哨兵不由本文件证明。
//   - model.FeedOutboxModel.Insert 走 LastInsertId，替身按「表内 max+1」分配；
//     自增主键从 101 起，只为可读性，用例一律读回而不是硬编码。
//   - go-sql-driver 默认（feed DSN 未开 clientFoundRows）RowsAffected 是 **changed rows**，
//     所以「同一秒内重复软删同一条动态」真实库会得 0 行→ErrFeedNotFound；
//     替身按 matched rows 实现，这一差异不模拟（见 feed_outbox.SoftDelete 注释）。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/services/feed/internal/repository"
	"go-video/services/feed/internal/svc"
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"
)

// --- 断言小工具（本包共享） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantErrIs 断言错误链里就是那个哨兵/被注入的错误本身（errors.Is 原样透传）。
// 第二层含义是「没有被换成另一种语义」：ErrFeedNotFound 不得变成 ErrInvalidMid。
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

// wantInt64sEQ 比较 id 序列（「到底返回/改动了哪几行」）。
func wantInt64sEQ(t *testing.T, label, field string, got, want []int64) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantOps 断言完整调用序列（顺序 + 内容）。
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

// wantNoCall 断言 from 之后没有发生任何依赖调用（守卫必须发生在触库/触缓存之前）。
func wantNoCall(t *testing.T, label string, st *store, from int) {
	t.Helper()
	if ops := st.log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

// countIn 统计片段序列里某前缀的出现次数（wantOps 数整条轨迹，
// 一个用例里连发多次被测调用时必须按片段数）。
func countIn(ops []string, prefix string) int {
	n := 0
	for _, o := range ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

// wantCountIn 断言片段内某前缀的调用次数（爆炸半径：3 个粉丝该写 3 次）。
func wantCountIn(t *testing.T, label string, ops []string, prefix string, want int) {
	t.Helper()
	if got := countIn(ops, prefix); got != want {
		t.Errorf("%s：%s* 次数 = %d, want %d（片段 [%s]）", label, prefix, got, want, strings.Join(ops, " → "))
	}
}

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

func (c *callLog) snapshot() int             { return len(c.ops) }
func (c *callLog) opsFrom(from int) []string { return c.ops[from:] }
func (c *callLog) reset()                    { c.ops = nil }

// --- 并发钩子（live-room 同款口径：一次性、执行前触发、未触发即判失败） ---

type raceHooks struct{ before map[string]func() }

func (r *raceHooks) arm(method string, fn func()) {
	if r.before == nil {
		r.before = map[string]func(){}
	}
	r.before[method] = fn
}

func (r *raceHooks) fire(method string) {
	if r == nil || r.before == nil {
		return
	}
	if fn, ok := r.before[method]; ok {
		delete(r.before, method)
		fn()
	}
}

// assertFired 断言布下的钩子都被触发过：钩子没触发说明被测代码根本没走到那条调用，
// 用例的并发前提就成了空转（这类自证式失效必须抓出来）。
func (r *raceHooks) assertFired(t *testing.T, label string) {
	t.Helper()
	if len(r.before) != 0 {
		keys := make([]string, 0, len(r.before))
		for k := range r.before {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Errorf("%s：raceBefore 这些并发钩子从未被触发（对应调用没执行）%v", label, keys)
	}
}

// --- 错误注入（方法粒度 + 可指定第几次发作） ---

type faultInjector struct {
	race    *raceHooks
	by      map[string]error    // 持续失败
	oneShot map[string]error    // 只失败下一次
	atN     map[string]nthFault // 指定第 n 次失败
	nth     map[string]int      // 每个方法的已调用次数
}

type nthFault struct {
	n   int
	err error
}

func newFault(race *raceHooks) *faultInjector {
	return &faultInjector{race: race, nth: map[string]int{}}
}

// failWith 让该方法之后每次调用都失败。
func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

// failNext 只让该方法的**下一次**调用失败，之后自动恢复正常。
func (f *faultInjector) failNext(method string, err error) {
	if f.oneShot == nil {
		f.oneShot = map[string]error{}
	}
	f.oneShot[method] = err
}

// failOn 让该方法的第 nth 次（1 起）调用失败。用于「fan-out 循环里只有第 2 个粉丝写坏」。
func (f *faultInjector) failOn(method string, nth int, err error) {
	if f.atN == nil {
		f.atN = map[string]nthFault{}
	}
	f.atN[method] = nthFault{n: nth, err: err}
}

// probe 是每个替身方法的唯一入口：记轨迹 → 触发并发钩子 → 计数并判定注入错误。
// 顺序与真实时序一致（钩子落在真正读写之前，等价于「别的插队在上一句 SQL 与这句之间」）。
func (f *faultInjector) probe(method string, log *callLog, format string, args ...any) error {
	log.add(format, args...)
	f.nth[method]++
	f.race.fire(method)
	if err, ok := f.oneShot[method]; ok {
		delete(f.oneShot, method)
		return err
	}
	if nf, ok := f.atN[method]; ok && f.nth[method] == nf.n {
		return nf.err
	}
	return f.by[method]
}

// errInjected 是注入用的下游故障样本（断言 errors.Is 原样透传到 logic 返回值）。
var (
	errInjected  = errors.New("inject: mysql down")
	errCacheDown = errors.New("inject: redis down")
)

// --- 缓存替身 ---

// fakeCache 实现 repository.Cacher。
//
// 内部键沿用生产前缀（repository.go:31-35），这样轨迹里的键与线上 Redis 一致可读；
// 但**缝在 Cacher 之下，键格式本身不是被测对象**（见文件头覆盖边界）。
// ZSet 语义按 go-zero/Redis 复刻：
//   - ZADD 同 member 改分（幂等重投不产生第二条成员）；
//   - ZREVRANGEBYSCORE key 0 max LIMIT 0 n：分数 ∈ [0, max]（**min 固定 0**，
//     所以负 ctime 的成员永远拉不到），同分按 member 十进制字符串逆序；
//   - SREM 删空集合后键消失（真实 Redis 行为），因此 ListPins 的 hit/miss 才有意义。
type fakeCache struct {
	log   *callLog
	fault *faultInjector

	zsets    map[string]map[int64]int64 // inbox/outbox：feedID → score(ctime)
	pins     map[string]map[int64]bool  // 置顶集合；外层键存在即 EXISTS=1
	counters map[string]int64           // 未读计数器；键存在即命中（0 也算命中）
	// followers 只有 seed 写入口，没有 Cacher 写方法：真实系统里由 social-graph 关注事件写入。
	followers map[int64][]int64
}

func newFakeCache(log *callLog, fault *faultInjector) *fakeCache {
	return &fakeCache{
		log:       log,
		fault:     fault,
		zsets:     map[string]map[int64]int64{},
		pins:      map[string]map[int64]bool{},
		counters:  map[string]int64{},
		followers: map[int64][]int64{},
	}
}

func inboxKey(mid int64) string  { return fmt.Sprintf("feed:inbox:%d", mid) }
func outboxKey(mid int64) string { return fmt.Sprintf("feed:outbox:%d", mid) }
func pinKey(mid int64) string    { return fmt.Sprintf("feed:pin:%d", mid) }
func unreadKey(mid int64) string { return fmt.Sprintf("feed:unread:%d", mid) }

// --- 缓存替身方法（逐字对应 repository.Cacher 的方法集） ---
// 注意：契约里**没有**写粉丝集合的方法，因此下面只有 seedFollowers 这个测试侧布防入口。

func (c *fakeCache) Ping(ctx context.Context) error {
	return c.fault.probe("cache.Ping", c.log, "cache.Ping")
}

func (c *fakeCache) GetFollowers(_ context.Context, authorMid int64) ([]int64, bool, error) {
	if err := c.fault.probe("cache.GetFollowers", c.log, "cache.GetFollowers:%d", authorMid); err != nil {
		return nil, false, err
	}
	mids, ok := c.followers[authorMid]
	if !ok {
		return nil, false, nil
	}
	out := make([]int64, len(mids))
	copy(out, mids)
	return out, true, nil
}

func (c *fakeCache) AddInbox(_ context.Context, mid, feedID, ctime int64) error {
	key := inboxKey(mid)
	if err := c.fault.probe("cache.AddInbox", c.log, "cache.AddInbox:mid=%d:fid=%d", mid, feedID); err != nil {
		return err
	}
	c.zadd(key, feedID, ctime)
	return nil
}

func (c *fakeCache) RemInbox(_ context.Context, mid, feedID int64) error {
	key := inboxKey(mid)
	if err := c.fault.probe("cache.RemInbox", c.log, "cache.RemInbox:mid=%d:fid=%d", mid, feedID); err != nil {
		return err
	}
	if z, ok := c.zsets[key]; ok {
		delete(z, feedID)
		if len(z) == 0 {
			delete(c.zsets, key)
		}
	}
	return nil
}

func (c *fakeCache) RangeInbox(_ context.Context, mid, cursor int64, limit int32) ([]int64, []int64, error) {
	if err := c.fault.probe("cache.RangeInbox", c.log,
		"cache.RangeInbox:mid=%d:cursor=%d:limit=%d", mid, cursor, limit); err != nil {
		return nil, nil, err
	}
	return c.orderedMembers(inboxKey(mid), cursor, limit), c.rangeScores(inboxKey(mid), cursor, limit), nil
}

func (c *fakeCache) AddOutbox(_ context.Context, mid, feedID, ctime int64) error {
	key := outboxKey(mid)
	if err := c.fault.probe("cache.AddOutbox", c.log, "cache.AddOutbox:mid=%d:fid=%d", mid, feedID); err != nil {
		return err
	}
	c.zadd(key, feedID, ctime)
	return nil
}

func (c *fakeCache) RemOutbox(_ context.Context, mid, feedID int64) error {
	key := outboxKey(mid)
	if err := c.fault.probe("cache.RemOutbox", c.log, "cache.RemOutbox:mid=%d:fid=%d", mid, feedID); err != nil {
		return err
	}
	if z, ok := c.zsets[key]; ok {
		delete(z, feedID)
		if len(z) == 0 {
			delete(c.zsets, key)
		}
	}
	return nil
}

func (c *fakeCache) RangeOutbox(_ context.Context, mid, cursor int64, limit int32) ([]int64, []int64, error) {
	if err := c.fault.probe("cache.RangeOutbox", c.log,
		"cache.RangeOutbox:mid=%d:cursor=%d:limit=%d", mid, cursor, limit); err != nil {
		return nil, nil, err
	}
	return c.orderedMembers(outboxKey(mid), cursor, limit), c.rangeScores(outboxKey(mid), cursor, limit), nil
}

func (c *fakeCache) AddPin(_ context.Context, mid, feedID int64) error {
	key := pinKey(mid)
	if err := c.fault.probe("cache.AddPin", c.log, "cache.AddPin:mid=%d:fid=%d", mid, feedID); err != nil {
		return err
	}
	if c.pins[key] == nil {
		c.pins[key] = map[int64]bool{}
	}
	c.pins[key][feedID] = true
	return nil
}

func (c *fakeCache) RemPin(_ context.Context, mid, feedID int64) error {
	key := pinKey(mid)
	if err := c.fault.probe("cache.RemPin", c.log, "cache.RemPin:mid=%d:fid=%d", mid, feedID); err != nil {
		return err
	}
	if s, ok := c.pins[key]; ok {
		delete(s, feedID)
		if len(s) == 0 {
			delete(c.pins, key) // 删空即键消失（Redis SREM 语义）
		}
	}
	return nil
}

func (c *fakeCache) ListPins(_ context.Context, mid int64) ([]int64, bool, error) {
	key := pinKey(mid)
	if err := c.fault.probe("cache.ListPins", c.log, "cache.ListPins:%d", mid); err != nil {
		return nil, false, err
	}
	s, ok := c.pins[key]
	if !ok {
		return nil, false, nil
	}
	ids := make([]int64, 0, len(s))
	for id := range s {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	return ids, true, nil
}

func (c *fakeCache) IncrUnread(_ context.Context, mid int64, delta int64) error {
	key := unreadKey(mid)
	if err := c.fault.probe("cache.IncrUnread", c.log, "cache.IncrUnread:mid=%d:%+d", mid, delta); err != nil {
		return err
	}
	c.counters[key] += delta // INCRBY 语义：键不存在按 0 起算并创建
	return nil
}

func (c *fakeCache) GetUnread(_ context.Context, mid int64) (int64, bool, error) {
	key := unreadKey(mid)
	if err := c.fault.probe("cache.GetUnread", c.log, "cache.GetUnread:%d", mid); err != nil {
		return 0, false, err
	}
	v, ok := c.counters[key]
	if !ok {
		return 0, false, nil
	}
	return v, true, nil
}

func (c *fakeCache) SetUnread(_ context.Context, mid int64, n int64) error {
	key := unreadKey(mid)
	if err := c.fault.probe("cache.SetUnread", c.log, "cache.SetUnread:%d=%d", mid, n); err != nil {
		return err
	}
	c.counters[key] = n
	return nil
}

func (c *fakeCache) ClearUnread(_ context.Context, mid int64) error {
	key := unreadKey(mid)
	if err := c.fault.probe("cache.ClearUnread", c.log, "cache.ClearUnread:%d", mid); err != nil {
		return err
	}
	c.counters[key] = 0 // SET key 0：键仍存在且值为 0（与生产 SetCtx 一致）
	return nil
}

// --- 缓存静默布防与状态读取（不记 callLog） ---

// seedFollowers 布作者的粉丝集合（生产由 social-graph 关注事件写入，本服务只读）。
func (c *fakeCache) seedFollowers(author int64, mids ...int64) {
	if c.followers == nil {
		c.followers = map[int64][]int64{}
	}
	c.followers[author] = mids
}

func (c *fakeCache) zadd(key string, member, score int64) {
	if c.zsets[key] == nil {
		c.zsets[key] = map[int64]int64{}
	}
	c.zsets[key][member] = score
}

// zscore 读回某个 ZSet 成员的分数（断言「ZSet score 用的是落库 ctime」）。
func (c *fakeCache) zscore(key string, member int64) (int64, bool) {
	v, ok := c.zsets[key][member]
	return v, ok
}

func (c *fakeCache) zmembers(key string) []int64 {
	ids := make([]int64, 0, len(c.zsets[key]))
	for id := range c.zsets[key] {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	return ids
}

func (c *fakeCache) unreadValue(mid int64) (int64, bool) {
	v, ok := c.counters[unreadKey(mid)]
	return v, ok
}

// pinMembers 读回置顶集合成员（键不存在返回 nil，等价于 Redis EXISTS=0）。
func (c *fakeCache) pinMembers(mid int64) []int64 {
	s, ok := c.pins[pinKey(mid)]
	if !ok {
		return nil
	}
	ids := make([]int64, 0, len(s))
	for id := range s {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	return ids
}

// seedPin 静默布一个置顶成员（真实 Redis 里键可能根本不存在，此时不该布）。
func (c *fakeCache) seedPin(mid, feedID int64) {
	if c.pins[pinKey(mid)] == nil {
		c.pins[pinKey(mid)] = map[int64]bool{}
	}
	c.pins[pinKey(mid)][feedID] = true
}

// seedUnread 静默布未读计数器（键存在，值可为 0）。
func (c *fakeCache) seedUnread(mid, n int64) { c.counters[unreadKey(mid)] = n }

// seedZSet 静默布 ZSet 成员。
func (c *fakeCache) seedZSet(key string, feedID, ctime int64) { c.zadd(key, feedID, ctime) }

// dropUnread 模拟缓存键被驱逐/清理（真实环境里是 flushdb、maxmemory 逐出或人工清理），
// 之后 GetUnread 会 miss 并回源 DB。不记 callLog：它是环境事件而不是被测调用。
func (c *fakeCache) dropUnread(mid int64) { delete(c.counters, unreadKey(mid)) }

// dropFollower 模拟 social-graph 的取关事件在读取之前生效（粉丝集合被别的入口改掉）。
func (c *fakeCache) dropFollower(author, follower int64) {
	mids := c.followers[author]
	kept := mids[:0]
	for _, m := range mids {
		if m != follower {
			kept = append(kept, m)
		}
	}
	c.followers[author] = kept
}

// --- ZSet 区间读取复刻（repository.go:110-133 / 151-174 同一套排序口径） ---

// orderedMembers 返回 score ∈ [0, max] 的成员，按 (score 降序, member 十进制字符串降序)。
func (c *fakeCache) orderedMembers(key string, cursor int64, limit int32) []int64 {
	var max int64 = math.MaxInt64
	if cursor > 0 {
		max = cursor - 1
	}
	type pair struct {
		id    int64
		score int64
	}
	var got []pair
	for id, score := range c.zsets[key] {
		if score < 0 || score > max {
			continue // 生产 min 固定 0：负分成员永远不可见
		}
		got = append(got, pair{id, score})
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].score != got[j].score {
			return got[i].score > got[j].score
		}
		return strconv.FormatInt(got[i].id, 10) > strconv.FormatInt(got[j].id, 10)
	})
	if limit >= 0 && int(limit) < len(got) {
		got = got[:limit]
	}
	out := make([]int64, 0, len(got))
	for _, p := range got {
		out = append(out, p.id)
	}
	return out
}

func (c *fakeCache) rangeScores(key string, cursor int64, limit int32) []int64 {
	ids := c.orderedMembers(key, cursor, limit)
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.zsets[key][id])
	}
	return out
}

// --- feed_outbox 替身（model/feedmodel.go:50-119） ---

type fakeOutboxModel struct {
	log   *callLog
	fault *faultInjector
	rows  map[int64]*model.FeedOutbox
	next  int64
}

func newFakeOutboxModel(log *callLog, fault *faultInjector) *fakeOutboxModel {
	return &fakeOutboxModel{log: log, fault: fault, rows: map[int64]*model.FeedOutbox{}, next: 101}
}

// Insert 镜像 model/feedmodel.go:50-70：Ctime==0 → now、Mtime=now、State==0 → 正常，
// 并且**写回调用方传入的结构体**（生产就是 f.Ctime = now）。
// 这个副作用是被测路径的一部分：repository 之后用 f.Ctime 当 ZSet score。
func (m *fakeOutboxModel) Insert(_ context.Context, f *model.FeedOutbox) (int64, error) {
	if err := m.fault.probe("feed_outbox.Insert", m.log, "feed_outbox.Insert:mid=%d:oid=%d", f.Mid, f.Oid); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	if f.Ctime == 0 {
		f.Ctime = now
	}
	f.Mtime = now
	if f.State == 0 {
		f.State = model.FeedStateNormal
	}
	id := m.next
	m.next++
	cp := *f
	cp.ID = id // 生产不写回 f.ID，只返回 LastInsertId
	m.rows[id] = &cp
	return id, nil
}

func (m *fakeOutboxModel) FindOne(_ context.Context, id int64) (*model.FeedOutbox, error) {
	if err := m.fault.probe("feed_outbox.FindOne", m.log, "feed_outbox.FindOne:%d", id); err != nil {
		return nil, err
	}
	row, ok := m.rows[id]
	if !ok {
		return nil, nil // 生产：sql.ErrNoRows → (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (m *fakeOutboxModel) FindMany(_ context.Context, ids []int64) (map[int64]*model.FeedOutbox, error) {
	if len(ids) == 0 {
		return map[int64]*model.FeedOutbox{}, nil // 生产 model/feedmodel.go:85-87 提前返回，不发 SQL
	}
	if err := m.fault.probe("feed_outbox.FindMany", m.log, "feed_outbox.FindMany:%s", joinInt64(ids)); err != nil {
		return nil, err
	}
	out := make(map[int64]*model.FeedOutbox, len(ids))
	for _, id := range ids {
		if row, ok := m.rows[id]; ok { // WHERE id IN (?)：不过滤 state，缺失项不在结果里
			cp := *row
			out[id] = &cp
		}
	}
	return out, nil
}

// SoftDelete 镜像 model/feedmodel.go:104-119（WHERE id AND mid；0 行 → ErrFeedNotFound）。
// 按 matched rows 计数，不模拟真实驱动 changed rows 的同秒重复删除差异。
func (m *fakeOutboxModel) SoftDelete(_ context.Context, id, mid int64) error {
	if err := m.fault.probe("feed_outbox.SoftDelete", m.log, "feed_outbox.SoftDelete:%d@%d", id, mid); err != nil {
		return err
	}
	row, ok := m.rows[id]
	if !ok || row.Mid != mid {
		return model.ErrFeedNotFound
	}
	row.State = model.FeedStateDeleted
	row.Mtime = time.Now().Unix()
	return nil
}

// --- 静默布防 ---

// put 直接塞一行（不记 callLog）；行必须布成「生产写得出来的形态」。
func (m *fakeOutboxModel) put(f *model.FeedOutbox) *model.FeedOutbox {
	if f.ID == 0 {
		f.ID = m.next
		m.next++
	}
	if f.ID >= m.next {
		m.next = f.ID + 1
	}
	cp := *f
	m.rows[cp.ID] = &cp
	return &cp
}

func (m *fakeOutboxModel) seed(mid, oid, ctime int64) *model.FeedOutbox {
	return m.put(&model.FeedOutbox{
		Mid: mid, Oid: oid, Otype: int32(rpc.OType_OTYPE_UGC_VIDEO),
		Action: int32(rpc.Action_ACTION_PUBLISH), Ctime: ctime, State: model.FeedStateNormal,
		Title: fmt.Sprintf("t%d", oid), Uri: fmt.Sprintf("bilibili://video/%d", oid),
	})
}

func (m *fakeOutboxModel) lastID() int64 {
	max := int64(0)
	for id := range m.rows {
		if id > max {
			max = id
		}
	}
	return max
}

func (m *fakeOutboxModel) stateOf(id int64) (int32, bool) {
	row, ok := m.rows[id]
	if !ok {
		return 0, false
	}
	return row.State, true
}

// --- feed_inbox 替身（model/feedmodel.go:153-194） ---

type fakeInboxModel struct {
	log   *callLog
	fault *faultInjector
	rows  []*model.FeedInbox
	next  int64
}

func newFakeInboxModel(log *callLog, fault *faultInjector) *fakeInboxModel {
	return &fakeInboxModel{log: log, fault: fault, next: 201}
}

// Add 镜像 uniq(mid, feed_id) 的 ON DUPLICATE 分支：已存在则 state=0、ctime/author_mid 覆盖。
func (m *fakeInboxModel) Add(_ context.Context, in *model.FeedInbox) error {
	if err := m.fault.probe("feed_inbox.Add", m.log, "feed_inbox.Add:mid=%d:fid=%d", in.Mid, in.FeedID); err != nil {
		return err
	}
	if in.Ctime == 0 {
		in.Ctime = time.Now().Unix()
	}
	if in.State == 0 {
		in.State = model.FeedStateNormal
	}
	for _, row := range m.rows {
		if row.Mid == in.Mid && row.FeedID == in.FeedID {
			row.State = model.FeedStateNormal
			row.Ctime = in.Ctime
			row.AuthorMid = in.AuthorMid
			return nil
		}
	}
	cp := *in
	cp.ID = m.next
	m.next++
	m.rows = append(m.rows, &cp)
	return nil
}

// ListByMid 镜像 model/feedmodel.go:169-184：cursor>0 走 `ctime < cursor`，cursor==0 走无过滤分支；
// ORDER BY ctime DESC LIMIT n。同分顺序 MySQL 不保证，替身取 feed_id 升序（确定序）。
func (m *fakeInboxModel) ListByMid(_ context.Context, mid int64, cursor int64, limit int32) ([]*model.FeedInbox, error) {
	if err := m.fault.probe("feed_inbox.ListByMid", m.log,
		"feed_inbox.ListByMid:mid=%d:cursor=%d:limit=%d", mid, cursor, limit); err != nil {
		return nil, err
	}
	var got []*model.FeedInbox
	for _, row := range m.rows {
		if row.Mid != mid || row.State != model.FeedStateNormal {
			continue
		}
		if cursor > 0 && row.Ctime >= cursor {
			continue
		}
		got = append(got, row)
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].Ctime != got[j].Ctime {
			return got[i].Ctime > got[j].Ctime
		}
		return got[i].FeedID < got[j].FeedID
	})
	if int(limit) < len(got) {
		got = got[:limit]
	}
	out := make([]*model.FeedInbox, 0, len(got))
	for _, row := range got {
		cp := *row
		out = append(out, &cp)
	}
	return out, nil
}

// DeleteByFeedID 镜像 model/feedmodel.go:186-194：不看 RowsAffected，0 行也算成功。
func (m *fakeInboxModel) DeleteByFeedID(_ context.Context, feedID int64) error {
	if err := m.fault.probe("feed_inbox.DeleteByFeedID", m.log, "feed_inbox.DeleteByFeedID:%d", feedID); err != nil {
		return err
	}
	for _, row := range m.rows {
		if row.FeedID == feedID {
			row.State = model.FeedStateDeleted
		}
	}
	return nil
}

func (m *fakeInboxModel) seed(mid, feedID, authorMid, ctime int64) {
	m.rows = append(m.rows, &model.FeedInbox{
		ID: m.next, Mid: mid, FeedID: feedID, AuthorMid: authorMid, Ctime: ctime, State: model.FeedStateNormal,
	})
	m.next++
}

// rowsOf 读回某个用户当前可见（state=0）的收件行数——爆炸半径的「行数差」就用它锁。
func (m *fakeInboxModel) rowsOf(mid int64) []int64 {
	var ids []int64
	for _, row := range m.rows {
		if row.Mid == mid && row.State == model.FeedStateNormal {
			ids = append(ids, row.FeedID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	return ids
}

func (m *fakeInboxModel) stateOf(mid, feedID int64) (int32, bool) {
	for _, row := range m.rows {
		if row.Mid == mid && row.FeedID == feedID {
			return row.State, true
		}
	}
	return 0, false
}

// --- feed_pin 替身（model/feedmodel.go:228-273） ---

type fakePinModel struct {
	log   *callLog
	fault *faultInjector
	rows  []*model.FeedPin
	next  int64
}

func newFakePinModel(log *callLog, fault *faultInjector) *fakePinModel {
	return &fakePinModel{log: log, fault: fault, next: 301}
}

func (m *fakePinModel) Add(_ context.Context, p *model.FeedPin) error {
	if err := m.fault.probe("feed_pin.Add", m.log, "feed_pin.Add:mid=%d:fid=%d", p.Mid, p.FeedID); err != nil {
		return err
	}
	now := time.Now().Unix()
	if p.Ctime == 0 {
		p.Ctime = now
	}
	for _, row := range m.rows {
		if row.Mid == p.Mid && row.FeedID == p.FeedID {
			row.State = model.PinStateNormal // ON DUPLICATE UPDATE state = 0
			row.Mtime = now
			return nil
		}
	}
	cp := *p
	cp.ID = m.next
	m.next++
	cp.Mtime = now
	if cp.State == 0 {
		cp.State = model.PinStateNormal
	}
	m.rows = append(m.rows, &cp)
	return nil
}

// Del 镜像 model/feedmodel.go:246-261：WHERE mid AND feed_id AND state=0，0 行 → ErrPinNotFound。
func (m *fakePinModel) Del(_ context.Context, mid, feedID int64) error {
	if err := m.fault.probe("feed_pin.Del", m.log, "feed_pin.Del:mid=%d:fid=%d", mid, feedID); err != nil {
		return err
	}
	for _, row := range m.rows {
		if row.Mid == mid && row.FeedID == feedID && row.State == model.PinStateNormal {
			row.State = model.PinStateDeleted
			row.Mtime = time.Now().Unix()
			return nil
		}
	}
	return model.ErrPinNotFound
}

func (m *fakePinModel) ListByMid(_ context.Context, mid int64) ([]int64, error) {
	if err := m.fault.probe("feed_pin.ListByMid", m.log, "feed_pin.ListByMid:%d", mid); err != nil {
		return nil, err
	}
	type row struct {
		id, ctime int64
	}
	var got []row
	for _, r := range m.rows {
		if r.Mid == mid && r.State == model.PinStateNormal {
			got = append(got, row{r.FeedID, r.Ctime})
		}
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].ctime != got[j].ctime {
			return got[i].ctime < got[j].ctime // ORDER BY ctime ASC
		}
		return got[i].id < got[j].id
	})
	out := make([]int64, 0, len(got))
	for _, r := range got {
		out = append(out, r.id)
	}
	return out, nil
}

func (m *fakePinModel) seed(mid, feedID int64) {
	m.rows = append(m.rows, &model.FeedPin{
		ID: m.next, Mid: mid, FeedID: feedID, Ctime: 1_700, Mtime: 1_700, State: model.PinStateNormal,
	})
	m.next++
}

func (m *fakePinModel) stateOf(mid, feedID int64) (int32, bool) {
	for _, row := range m.rows {
		if row.Mid == mid && row.FeedID == feedID {
			return row.State, true
		}
	}
	return 0, false
}

func (m *fakePinModel) normalCount() int {
	n := 0
	for _, row := range m.rows {
		if row.State == model.PinStateNormal {
			n++
		}
	}
	return n
}

// --- feed_unread 替身（model/feedmodel.go:304-336） ---

type fakeUnreadModel struct {
	log   *callLog
	fault *faultInjector
	rows  map[int64]*model.FeedUnread
}

func newFakeUnreadModel(log *callLog, fault *faultInjector) *fakeUnreadModel {
	return &fakeUnreadModel{log: log, fault: fault, rows: map[int64]*model.FeedUnread{}}
}

// IncrBy 镜像 model/feedmodel.go:304-313：INSERT 分支直接写 delta（可以为负，没有 GREATEST），
// UPDATE 分支才做 GREATEST(unread+delta, 0)。
func (m *fakeUnreadModel) IncrBy(_ context.Context, mid int64, delta int64) error {
	if err := m.fault.probe("feed_unread.IncrBy", m.log, "feed_unread.IncrBy:mid=%d:%+d", mid, delta); err != nil {
		return err
	}
	now := time.Now().Unix()
	row, ok := m.rows[mid]
	if !ok {
		m.rows[mid] = &model.FeedUnread{Mid: mid, Unread: delta, Mtime: now}
		return nil
	}
	n := row.Unread + delta
	if n < 0 {
		n = 0
	}
	row.Unread = n
	row.Mtime = now
	return nil
}

func (m *fakeUnreadModel) Get(_ context.Context, mid int64) (int64, error) {
	if err := m.fault.probe("feed_unread.Get", m.log, "feed_unread.Get:%d", mid); err != nil {
		return 0, err
	}
	row, ok := m.rows[mid]
	if !ok {
		return 0, nil // sql.ErrNoRows → 0
	}
	return row.Unread, nil
}

func (m *fakeUnreadModel) Clear(_ context.Context, mid int64) error {
	if err := m.fault.probe("feed_unread.Clear", m.log, "feed_unread.Clear:%d", mid); err != nil {
		return err
	}
	m.rows[mid] = &model.FeedUnread{Mid: mid, Unread: 0, Mtime: time.Now().Unix()}
	return nil
}

func (m *fakeUnreadModel) seed(mid, unread int64) {
	m.rows[mid] = &model.FeedUnread{Mid: mid, Unread: unread, Mtime: 1_700}
}

func (m *fakeUnreadModel) value(mid int64) int64 { return m.rows[mid].Unread }

// --- 小工具 ---

func joinInt64(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func feedIDs(items []*rpc.FeedItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.Id)
	}
	return out
}

// --- 装配：真实 Repository + 替身依赖 ---

type store struct {
	log    *callLog
	race   *raceHooks
	cache  *fakeCache
	outbox *fakeOutboxModel
	inbox  *fakeInboxModel
	pin    *fakePinModel
	unread *fakeUnreadModel
	repo   *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	race := &raceHooks{}
	fault := newFault(race)
	st := &store{log: log, race: race}
	st.cache = newFakeCache(log, fault)
	st.outbox = newFakeOutboxModel(log, fault)
	st.inbox = newFakeInboxModel(log, fault)
	st.pin = newFakePinModel(log, fault)
	st.unread = newFakeUnreadModel(log, fault)
	// conn 传 nil：feed Repository 不直接用 r.conn（无事务路径），真用了就是 nil panic，用例即红。
	st.repo = repository.NewWithDeps(st.cache, nil, st.outbox, st.inbox, st.pin, st.unread)
	return st
}

// cacheFault / modelFault 给用例一个注入错误的入口（同一份 faultInjector）。
func (st *store) fault() *faultInjector { return st.cache.fault }

func (st *store) svcCtx() *svc.ServiceContext {
	return &svc.ServiceContext{Repository: st.repo}
}

// raceBefore 布一个一次性并发钩子：在第 method 这次调用执行之前改库，模拟别的入口插队。
func (st *store) raceBefore(method string, fn func()) { st.race.arm(method, fn) }

// checkRaces 在用例末尾调用，确认布下的并发钩子都被触发过。
func (st *store) checkRaces(t *testing.T) {
	t.Helper()
	st.race.assertFired(t, t.Name())
}

// 编译期确认替身满足生产接口（签名漂移在这里立刻炸出，而不是留到装配处）。
var (
	_ repository.Cacher     = (*fakeCache)(nil)
	_ model.FeedOutboxModel = (*fakeOutboxModel)(nil)
	_ model.FeedInboxModel  = (*fakeInboxModel)(nil)
	_ model.FeedPinModel    = (*fakePinModel)(nil)
	_ model.FeedUnreadModel = (*fakeUnreadModel)(nil)
)
