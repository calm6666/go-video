package logic

// fakes_test.go 是 creator 8 个 RPC logic 用例的内存替身集合。
//
// 为什么需要注入缝：creator 的 ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），logic 单测无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, 6 个内存 model) 组装**真实的 Repository**，只把它的依赖换成
// 替身——这样「缓存读穿/回填/失效顺序、空标记 vs 真 miss、脏缓存、IN 列表口径」整条判定链
// 都在被测路径上，而不是把 Repository 也 mock 掉。见 internal/repository/repository.go 的 Cacher 注释。
//
// 四条替身纪律（catalog / rights / playback / comment 几轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 若就地改字段不得污染库存行，否则
//     「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 副作用按**顺序**记录（callLog，条目 `<表>.<方法>:<键>`，缓存条目用真实 Redis key 当键），
//     断言序列而不只断次数：creator 要紧的结论全是顺序与口径类的
//     （写必须是 Upsert→DelSwitch 而不是反过来、批量不得碰缓存、守卫拒绝后零依赖调用）。
//  3. 错误注入按方法粒度，且能指定**第几次调用**发作（failWith 每次都发作 / failOn 只在第 n 次）：
//     「第二次读才炸」这类用例是缓存回填与回源支路唯一的可证伪写法。
//  4. 布数据（seedXxx / warmXxx）走**静默路径**，不写 callLog：序列断言里出现的每一项
//     都是被测代码的调用。
//
// 替身的 SQL / 缓存语义逐条对齐源文件（标了 file:line）：
//   - model/*.go 的 WHERE、ORDER BY、LIMIT/OFFSET、`sql.ErrNoRows` 翻译；
//   - internal/repository/cache.go 的 key 拼装（up:spec/up:attr/up:sw/up:groups/up:gm）、
//     emptyMark="{}" 的「空标记＝命中但值为空」语义、TTL 常量。
//
// 如实声明无法覆盖的路径：
//   - 本服务 8 个方法**没有任何跨服务下游**（svc.ServiceContext 只带 Repository），
//     所以「未接入下游必须走降级、错误要原样 errors.Is 透传」这条在本服务只体现为
//     「model/缓存错误不被伪装成查无此人」，见各 *DBFailure / *CorruptCache 用例；
//     README 声明依赖的 account/user-profile/moderation-orchestrator 尚未接入本服务代码。
//   - 假件只复刻 model 层 SQL 的**语义**，不证明 SQL 与列名本身；真库上的
//     `INSERT ... ON DUPLICATE KEY UPDATE`（up_switch）与 `mid IN (?)` 展开由
//     deploy/migrations/creator/*.sql 与 DSN 决定，内存用例无法证伪。
//   - 真实 Redis 的 SETEX/DEL/EXISTS 与序列化由 go-zero 承担，这里只按 cache.go 的分支等价复刻。
//
// 缺陷登记（都是**当前真实行为**，对应用例按现状断言，注释里写明「pin」；未修的理由一并给出）：
//
//	D1 UpSwitch 的回填存在「读到旧值→写缓存」竞态窗口，可把已被并发写覆盖的开关再次钉成旧值，
//	   最长 24h（TTL 见 repository/cache.go:28）。
//	   位置：services/creator/internal/repository/repository.go:222-230（先 FindOne 后 SetSwitch，
//	   中间无版本判定）；后果：见 TestUpSwitchStaleBackfillRacePinsOldValue。
//	   未修：修法不唯一（版本号 / 写穿透 / 缩短 TTL 各有取舍），先 pin。
//	D2 SetUpSwitch 在「DB 已写成功、缓存失效失败」时整体返回错误：调用方看到失败，
//	   但库里已经是新值。位置：repository.go:238-241（DelSwitch 的 error 被原样返回）。
//	   后果与复现：TestSetUpSwitchCacheInvalidateFailureStillWritesDB。
//	   未修：幂等重试即可自愈（Upsert 覆盖同值），改成吞掉失效错误反而会掩盖 Redis 故障。
//	D3 UpSpecial 没有 mid>0 守卫（同包的 UpAttr/UpSwitch/SetUpSwitch 都有），mid=0/负数会
//	   照常触库并写进缓存键 `up:spec:0`。位置：services/creator/internal/logic/upspeciallogic.go:28-37
//	   （函数体第一句就是 Repository 调用）；后果：非法 mid 得到「空分组」成功应答而非
//	   errInvalidMid，且网关侧不做该校验（gateway/app/internal/logic/upspeciallogic.go:38 直接透传）。
//	   未修：应答本身无害（mid=0 不可能是真用户），但要改就是契约变更，先 pin 并留用例锁死。
//	D4 GetHighAllyUps 对 mids 长度**无上限**（UpsSpecial 有 100 的上限），一次请求可以把任意长的
//	   IN 列表打到 MySQL。位置：services/creator/internal/logic/gethighallyupslogic.go:29-36
//	   （只判了 len==0）；后果：单请求放大成慢查询/连接占用。
//	   未修：上限取多少属容量决策（100？500？），需与调用方一起定，先 pin 当前无界行为。
//	D5 分组成员缓存（up:gm:*，TTL 60s）**没有任何失效路径**：本服务不写 up_special，
//	   分组归属由运营面直接改表，所以改完最长 60s 才可见。位置：repository/cache.go:30、
//	   repository.go:156-185（UpGroupMids 只回填不失效）；后果：UpGroupMids 有最多 60s 的成员滞后。
//	   未修：需要跨服务的失效入口（本服务没有写 up_special 的 RPC），先 pin。
//	D6 UpGroupMids 把分页归一化结果**写回入参消息** in.Pn/in.Ps（upgroupmidslogic.go:32-42）。
//	   后果：gRPC 侧调用方复用自己的 request 对象时会被悄悄改页码。未修：行为无歧义（值就是
//	   实际生效的页），只是副作用可见性差，用例 TestUpGroupMidsNormalizesPageInPlace 锁住现状。
//
// 已修的生产缺陷（回归锁在 TestSetUpSwitchKeepsFullInt64Mid）：
// SetUpSwitch 链路把 int64 mid 截断成 int32（旧 logic/setupswitchlogic.go:38 的 `int32(in.Mid)`、
// 旧 repository.SetUpSwitch 与旧 model.UpSwitchModel.Upsert 的 `mid int32`，
// 现分别为 repository.go:237 与 model/up_switch.go:27 的 int64），而 up_switch.mid 是 BIGINT UNSIGNED
// （deploy/migrations/creator/000004_create_up_switch.sql:16）。mid > 2^31-1 时：写进**另一个号**的
// 行、失效**另一个号**的缓存，而读侧 UpSwitch 用的是完整 int64 → 用户改完开关读回旧值，
// 且真的改动了别人的开关行。修法唯一（与同表 FindOne 的 int64 对齐），故直接改生产代码。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go-video/services/creator/internal/config"
	"go-video/services/creator/internal/repository"
	"go-video/services/creator/internal/svc"
	"go-video/services/creator/model"
	"go-video/services/creator/rpc"
)

// --- 断言小工具（本包共享） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

func wantInt64sEQ(t *testing.T, label, field string, got, want []int64) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

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

// wantErrContains 锁定分支归属：repository 用 `fmt.Errorf("Xxx: %w", err)` 包一层，
// 只看哨兵不足以分辨「是哪一条语句报的错」。
func wantErrContains(t *testing.T, label string, err error, frag string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want 含 %q", label, frag)
	}
	if !strings.Contains(err.Error(), frag) {
		t.Fatalf("%s：错误 = %v, want 含 %q", label, err, frag)
	}
}

// wantNoCallAfter 断言 from 之后没有任何依赖调用：入参守卫必须发生在触库/触缓存之前。
func wantNoCallAfter(t *testing.T, label string, log *callLog, from int) {
	t.Helper()
	if ops := log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

// wantSeq 断言 from 之后的完整调用序列（顺序本身就是要锁的结论时用这条）。
func wantSeq(t *testing.T, label string, log *callLog, from int, want ...string) {
	t.Helper()
	got := log.opsFrom(from)
	if !slices.Equal(got, want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label,
			strings.Join(got, " → "), strings.Join(want, " → "))
	}
}

// wantMethodCount 按方法名精确计数（op 去掉 `:键` 后整体相等）。
// 锁「没走某条 SQL / 没碰某个缓存」必须用这条：`up_special.FindOne` 与
// `up_special.FindMidsByGroup` 共享同一张表名，前缀匹配会把断言写成假通过。
func wantMethodCount(t *testing.T, label string, log *callLog, method string, want int) {
	t.Helper()
	if got := log.countMethod(method); got != want {
		t.Errorf("%s：%s 调用次数 = %d, want %d（完整轨迹 [%s]）",
			label, method, got, want, strings.Join(log.ops, " → "))
	}
}

// wantCacheUntouched 锁「批量路径不经过缓存」这类结论（缺陷 D4/D5 的对照面）。
func wantCacheUntouched(t *testing.T, label string, log *callLog) {
	t.Helper()
	var hits []string
	for _, o := range log.ops {
		if strings.HasPrefix(o, "cache.") {
			hits = append(hits, o)
		}
	}
	if len(hits) != 0 {
		t.Fatalf("%s：路径上出现了缓存调用 %v", label, hits)
	}
}

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

func (c *callLog) countMethod(method string) int {
	n := 0
	for _, o := range c.ops {
		if strings.SplitN(o, ":", 2)[0] == method {
			n++
		}
	}
	return n
}

func (c *callLog) snapshot() int             { return len(c.ops) }
func (c *callLog) opsFrom(from int) []string { return c.ops[from:] }

// --- 并发钩子：在两次被调方法之间插入改库动作 ---

// raceHooks 模拟「读到值之后、写之前，别的入口把这一行/这个 key 改了」这类窗口，
// 不需要真起 goroutine：每个键是一次性钩子，在对应方法**执行之前**触发一次。
// 键与 callLog 同形（`<表>.<方法>` 或 `cache.<方法>`，不带 `:键` 部分）。
type raceHooks struct{ before map[string]func() }

func (r *raceHooks) arm(method string, fn func()) {
	if r.before == nil {
		r.before = map[string]func(){}
	}
	r.before[method] = fn
}

// fire 触发并摘掉钩子（一次性）；没布钩子时什么都不做。
// 记账顺序固定为「先 fire 再 log.add」：钩子代表别的入口在这一瞬间真的改了数据，
// 所以序列里必须排在本次副作用落地之前——否则轨迹会自相矛盾
// （例：D1 里回填的 SetSwitch 必须出现在并发写的 Upsert/DelSwitch 之后，才和「缓存留下旧值」的终态一致）。
func (r *raceHooks) fire(method string) {
	if r == nil || r.before == nil {
		return
	}
	if fn, ok := r.before[method]; ok {
		delete(r.before, method)
		fn()
	}
}

// assertFired 断言所有布下的钩子都被触发过：钩子没触发说明被测代码根本没走到那条语句，
// 用例的并发前提就成了空转（这类自证式失效必须抓出来）。
func (r *raceHooks) assertFired(t *testing.T, label string) {
	t.Helper()
	if len(r.before) != 0 {
		keys := make([]string, 0, len(r.before))
		for k := range r.before {
			keys = append(keys, k)
		}
		t.Errorf("%s：这些并发钩子从未被触发（对应语句没执行）%v", label, keys)
	}
}

// --- 错误注入（方法粒度 + 可指定第几次发作） ---

type nthFault struct {
	n   int
	err error
}

type faultInjector struct {
	who   string
	log   *callLog
	race  *raceHooks
	by    map[string]error
	nth   map[string]nthFault
	calls map[string]int
}

func newFault(who string, log *callLog, race *raceHooks) faultInjector {
	return faultInjector{who: who, log: log, race: race}
}

// failWith 让某方法每次调用都返回指定错误。
func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

// failOn 只让第 n 次调用（从 1 计）发作——回填/回源支路的可证伪写法。
func (f *faultInjector) failOn(method string, n int, err error) {
	if f.nth == nil {
		f.nth = map[string]nthFault{}
	}
	f.nth[method] = nthFault{n: n, err: err}
}

// dbErr 记账一次调用并按注入规则返回错误；未注入时返回 nil。
// 记账发生在**副作用之前**：真库里没写成的那一行，callLog 也照实留痕（与真实驱动一致）。
func (f *faultInjector) dbErr(method string) error {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[method]++
	if by := f.by[method]; by != nil {
		return by
	}
	if nf, ok := f.nth[method]; ok && f.calls[method] == nf.n {
		return nf.err
	}
	return nil
}

func (f *faultInjector) fireRace(method string) { f.race.fire(method) }

// unexpected 是「本用例不允许触达的依赖调用」的统一出口：
// 返回错误而不是 panic，失败信息里直接有方法名，且这次调用进了 callLog（序列断言抓得到）。
func (f *faultInjector) unexpected(method string) error {
	f.log.add("%s.%s:UNEXPECTED", f.who, method)
	return fmt.Errorf("%w: %s.%s 未在替身里实现（该路径本不该被触达，或请在 fakes_test.go 补真实语义）",
		errUnexpectedDependency, f.who, method)
}

var (
	errUnexpectedDependency = errors.New("creator/logic/test: unexpected dependency call")

	// 注入用的哨兵：用例断言错误**原样透传**（errors.Is），不伪装成「查无此人」。
	errFakeDB    = errors.New("test: mysql unavailable")
	errFakeRedis = errors.New("test: redis unavailable")
)

// --- 缓存 key / TTL 复刻（repository/cache.go:17-35） ---

const (
	fakeEmptyMark = "{}" // cache.go:34 防击穿空标记

	fakeTTLSpecial  = 3600  // cache.go:26
	fakeTTLAttr     = 3600  // cache.go:27
	fakeTTLSwitch   = 86400 // cache.go:28 —— 缺陷 D1 的滞后上限
	fakeTTLGroups   = 300   // cache.go:29
	fakeTTLGroupMem = 60    // cache.go:30 —— 缺陷 D5 的滞后上限
)

func fakeKeySpecial(mid int64) string            { return fmt.Sprintf("up:spec:%d", mid) }
func fakeKeyAttr(mid int64, from int32) string   { return fmt.Sprintf("up:attr:%d:%d", mid, from) }
func fakeKeySwitch(mid int64, from int32) string { return fmt.Sprintf("up:sw:%d:%d", mid, from) }
func fakeKeyGroups() string                      { return "up:groups" }
func fakeKeyGroupMem(gid int64, pn, ps int32) string {
	return fmt.Sprintf("up:gm:%d:%d:%d", gid, pn, ps)
}

// --- 缓存替身（实现 repository.Cacher，逐分支复刻 cache.go） ---

type fakeCache struct {
	faultInjector
	raw map[string]string
	ttl map[string]int
}

func newFakeCache(log *callLog, race *raceHooks) *fakeCache {
	return &fakeCache{
		faultInjector: newFault("cache", log, race),
		raw:           map[string]string{},
		ttl:           map[string]int{},
	}
}

var _ repository.Cacher = (*fakeCache)(nil)

func (f *fakeCache) setex(key, val string, ttl int) {
	f.raw[key] = val
	f.ttl[key] = ttl
}

// warm 是布景写入（静默：不写 callLog、不吃错误注入）。
func (f *fakeCache) warm(key, val string) {
	f.raw[key] = val
	f.ttl[key] = -1 // 布景不自报 TTL，避免把「代码选的 TTL」和「布景 TTL」混成一件事
}

func (f *fakeCache) Ping(context.Context) error {
	f.fireRace("cache.Ping")
	f.log.add("cache.Ping")
	return f.dbErr("Ping")
}

// GetSpecial 复刻 cache.go:95-113：真 miss→(nil,nil)；空标记→([]int64{},nil)；脏值→error。
func (f *fakeCache) GetSpecial(_ context.Context, mid int64) ([]int64, error) {
	key := fakeKeySpecial(mid)
	f.fireRace("cache.GetSpecial")
	f.log.add("cache.GetSpecial:%s", key)
	if err := f.dbErr("GetSpecial"); err != nil {
		return nil, err
	}
	bs, ok := f.raw[key]
	if !ok {
		return nil, nil // redis.Nil：getJSON 不报错，ids 仍是 nil，再被 ExistsCtx 判成真 miss
	}
	if bs == "" || bs == fakeEmptyMark {
		return []int64{}, nil // 空标记：命中，值为空列表
	}
	var ids []int64
	if err := json.Unmarshal([]byte(bs), &ids); err != nil {
		return nil, err // cache.go:74 的解码错误原样上抛
	}
	return ids, nil
}

// SetSpecial 复刻 cache.go:116-121：ids==nil 走空标记，否则写 JSON。
func (f *fakeCache) SetSpecial(_ context.Context, mid int64, ids []int64) error {
	key := fakeKeySpecial(mid)
	f.fireRace("cache.SetSpecial")
	f.log.add("cache.SetSpecial:%s", key)
	if err := f.dbErr("SetSpecial"); err != nil {
		return err
	}
	if ids == nil {
		f.setex(key, fakeEmptyMark, fakeTTLSpecial)
		return nil
	}
	bs, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	f.setex(key, string(bs), fakeTTLSpecial)
	return nil
}

// GetGroups / SetGroups 复刻 cache.go:197-211（key up:groups，payload 原样存取）。
func (f *fakeCache) GetGroups(_ context.Context) (string, error) {
	key := fakeKeyGroups()
	f.fireRace("cache.GetGroups")
	f.log.add("cache.GetGroups:%s", key)
	if err := f.dbErr("GetGroups"); err != nil {
		return "", err
	}
	return f.raw[key], nil
}

func (f *fakeCache) SetGroups(_ context.Context, payload string) error {
	key := fakeKeyGroups()
	f.fireRace("cache.SetGroups")
	f.log.add("cache.SetGroups:%s", key)
	if err := f.dbErr("SetGroups"); err != nil {
		return err
	}
	f.setex(key, payload, fakeTTLGroups)
	return nil
}

// GetGroupMids / SetGroupMids 复刻 cache.go:216-230（key up:gm:gid:pn:ps）。
func (f *fakeCache) GetGroupMids(_ context.Context, gid int64, pn, ps int32) (string, error) {
	key := fakeKeyGroupMem(gid, pn, ps)
	f.fireRace("cache.GetGroupMids")
	f.log.add("cache.GetGroupMids:%s", key)
	if err := f.dbErr("GetGroupMids"); err != nil {
		return "", err
	}
	return f.raw[key], nil
}

func (f *fakeCache) SetGroupMids(_ context.Context, gid int64, pn, ps int32, payload string) error {
	key := fakeKeyGroupMem(gid, pn, ps)
	f.fireRace("cache.SetGroupMids")
	f.log.add("cache.SetGroupMids:%s", key)
	if err := f.dbErr("SetGroupMids"); err != nil {
		return err
	}
	f.setex(key, payload, fakeTTLGroupMem)
	return nil
}

// GetAttr 复刻 cache.go:132-148：命中口径是 (state, hit)，空标记算 hit。
func (f *fakeCache) GetAttr(_ context.Context, mid int64, from int32) (int32, bool, error) {
	key := fakeKeyAttr(mid, from)
	f.fireRace("cache.GetAttr")
	f.log.add("cache.GetAttr:%s", key)
	if err := f.dbErr("GetAttr"); err != nil {
		return 0, false, err
	}
	bs, ok := f.raw[key]
	if !ok {
		return 0, false, nil
	}
	if bs == "" || bs == fakeEmptyMark {
		return 0, true, nil // 空标记：已缓存的「查无此人」
	}
	v, err := strconv.Atoi(bs)
	if err != nil {
		return 0, false, err
	}
	return int32(v), true, nil
}

func (f *fakeCache) SetAttr(_ context.Context, mid int64, from, state int32) error {
	key := fakeKeyAttr(mid, from)
	f.fireRace("cache.SetAttr")
	f.log.add("cache.SetAttr:%s", key)
	if err := f.dbErr("SetAttr"); err != nil {
		return err
	}
	f.setex(key, strconv.Itoa(int(state)), fakeTTLAttr)
	return nil
}

func (f *fakeCache) SetAttrEmpty(_ context.Context, mid int64, from int32) error {
	key := fakeKeyAttr(mid, from)
	f.fireRace("cache.SetAttrEmpty")
	f.log.add("cache.SetAttrEmpty:%s", key)
	if err := f.dbErr("SetAttrEmpty"); err != nil {
		return err
	}
	f.setex(key, fakeEmptyMark, fakeTTLAttr)
	return nil
}

// GetSwitch / SetSwitch / DelSwitch 复刻 cache.go:164-191（key up:sw:mid:from）。
func (f *fakeCache) GetSwitch(_ context.Context, mid int64, from int32) (int32, bool, error) {
	key := fakeKeySwitch(mid, from)
	f.fireRace("cache.GetSwitch")
	f.log.add("cache.GetSwitch:%s", key)
	if err := f.dbErr("GetSwitch"); err != nil {
		return 0, false, err
	}
	bs, ok := f.raw[key]
	if !ok {
		return 0, false, nil
	}
	if bs == "" || bs == fakeEmptyMark {
		return 0, true, nil
	}
	v, err := strconv.Atoi(bs)
	if err != nil {
		return 0, false, err
	}
	return int32(v), true, nil
}

func (f *fakeCache) SetSwitch(_ context.Context, mid int64, from, state int32) error {
	key := fakeKeySwitch(mid, from)
	f.fireRace("cache.SetSwitch") // 缺陷 D1 的插队点就在这里（回填之前被并发写覆盖）
	f.log.add("cache.SetSwitch:%s", key)
	if err := f.dbErr("SetSwitch"); err != nil {
		return err
	}
	f.setex(key, strconv.Itoa(int(state)), fakeTTLSwitch)
	return nil
}

func (f *fakeCache) DelSwitch(_ context.Context, mid int64, from int32) error {
	key := fakeKeySwitch(mid, from)
	f.fireRace("cache.DelSwitch")
	f.log.add("cache.DelSwitch:%s", key)
	if err := f.dbErr("DelSwitch"); err != nil {
		return err
	}
	delete(f.raw, key)
	delete(f.ttl, key)
	return nil
}

// --- up_special 替身（同一张表被两个 model 接口读，见 model/up_group_member.go:11-12） ---

type upSpecialRow struct {
	mid     int64
	groupID int64
}

type fakeUpSpecialModel struct {
	faultInjector
	rows []upSpecialRow

	findOneCalls  []int64
	findManyCalls [][]int64
}

func newFakeUpSpecialModel(log *callLog, race *raceHooks) *fakeUpSpecialModel {
	return &fakeUpSpecialModel{faultInjector: newFault("up_special", log, race)}
}

var _ model.UpSpecialModel = (*fakeUpSpecialModel)(nil)

// FindOne 复刻 model/up_special.go:35-45：`SELECT group_id FROM up_special WHERE mid = ?`，
// 无 ORDER BY（返回行序＝插入序），无记录时 model 交回 nil 切片。
func (f *fakeUpSpecialModel) FindOne(_ context.Context, mid int64) ([]int64, error) {
	f.fireRace("up_special.FindOne")
	f.log.add("up_special.FindOne:%d", mid)
	f.findOneCalls = append(f.findOneCalls, mid)
	if err := f.dbErr("FindOne"); err != nil {
		return nil, err
	}
	var out []int64
	for _, r := range f.rows {
		if r.mid == mid {
			out = append(out, r.groupID)
		}
	}
	return out, nil
}

// FindMany 复刻 model/up_special.go:47-68：`WHERE mid IN (?)`。
// IN 是集合语义，所以请求里重复的 mid **不会**让结果翻倍；查不到的 mid 不进 map（缺陷 D3 的对照面）。
func (f *fakeUpSpecialModel) FindMany(_ context.Context, mids []int64) (map[int64][]int64, error) {
	f.fireRace("up_special.FindMany")
	f.log.add("up_special.FindMany:n=%d", len(mids))
	f.findManyCalls = append(f.findManyCalls, slices.Clone(mids))
	if err := f.dbErr("FindMany"); err != nil {
		return nil, err
	}
	out := make(map[int64][]int64, len(mids))
	if len(mids) == 0 {
		return out, nil // model/up_special.go:48-50
	}
	for _, r := range f.rows {
		if slices.Contains(mids, r.mid) {
			out[r.mid] = append(out[r.mid], r.groupID)
		}
	}
	return out, nil
}

// --- up_group 替身 ---

type fakeUpGroupModel struct {
	faultInjector
	rows    []*model.UpGroup
	allCall int
}

func newFakeUpGroupModel(log *callLog, race *raceHooks) *fakeUpGroupModel {
	return &fakeUpGroupModel{faultInjector: newFault("up_group", log, race)}
}

var _ model.UpGroupModel = (*fakeUpGroupModel)(nil)

// All 复刻 model/up_group.go:37-51：全表扫，按 id 索引；返回的是行副本。
func (f *fakeUpGroupModel) All(context.Context) (map[int64]*model.UpGroup, error) {
	f.fireRace("up_group.All")
	f.log.add("up_group.All")
	f.allCall++
	if err := f.dbErr("All"); err != nil {
		return nil, err
	}
	out := make(map[int64]*model.UpGroup, len(f.rows))
	for _, g := range f.rows {
		cp := *g
		out[cp.ID] = &cp
	}
	return out, nil
}

// --- up_special 反向索引（分组 → 成员）替身，读的是 fakeUpSpecialModel 的同一份行 ---

type fakeUpGroupMemberModel struct {
	faultInjector
	special *fakeUpSpecialModel

	midsCalls []groupMidsCall
}

type groupMidsCall struct {
	gid    int64
	pn, ps int32
}

func newFakeUpGroupMemberModel(log *callLog, race *raceHooks, special *fakeUpSpecialModel) *fakeUpGroupMemberModel {
	return &fakeUpGroupMemberModel{
		faultInjector: newFault("up_group_member", log, race),
		special:       special,
	}
}

var _ model.UpGroupMemberModel = (*fakeUpGroupMemberModel)(nil)

// FindMidsByGroup 复刻 model/up_group_member.go:28-57 的两条 SQL：
//  1. `COUNT(*) ... WHERE group_id = ?`，total==0 直接 (nil, 0, nil)（不再发第二条）；
//  2. `SELECT mid ... ORDER BY mid LIMIT ? OFFSET ?`，offset 越界 → (nil, total, nil)。
//
// 兜底：pn<1 归 1、ps<1 归 20（model 自己的守卫，logic 另有 1/1000 的归一化，两层都在被测路径上）。
func (f *fakeUpGroupMemberModel) FindMidsByGroup(_ context.Context, gid int64, pn, ps int32) ([]int64, int32, error) {
	f.fireRace("up_special.FindMidsByGroup")
	f.log.add("up_special.FindMidsByGroup:%d/%d/%d", gid, pn, ps)
	f.midsCalls = append(f.midsCalls, groupMidsCall{gid: gid, pn: pn, ps: ps})
	if err := f.dbErr("FindMidsByGroup"); err != nil {
		return nil, 0, err
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = 20
	}
	var all []int64
	for _, r := range f.special.rows {
		if r.groupID == gid {
			all = append(all, r.mid)
		}
	}
	slices.Sort(all) // ORDER BY mid
	total := int32(len(all))
	if total == 0 {
		return nil, 0, nil
	}
	start := int(pn-1) * int(ps)
	if start >= len(all) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], total, nil
}

// --- up_attr 替身 ---

type fakeUpAttrModel struct {
	faultInjector
	rows []*model.UpAttr

	findOneCalls []attrKey
}

type attrKey struct {
	mid  int64
	from int32
}

func newFakeUpAttrModel(log *callLog, race *raceHooks) *fakeUpAttrModel {
	return &fakeUpAttrModel{faultInjector: newFault("up_attr", log, race)}
}

var _ model.UpAttrModel = (*fakeUpAttrModel)(nil)

// FindOne 复刻 model/up_attr.go:33-42：`WHERE mid = ? AND from = ?`，无记录 (nil, nil)。
func (f *fakeUpAttrModel) FindOne(_ context.Context, mid int64, from int32) (*model.UpAttr, error) {
	f.fireRace("up_attr.FindOne")
	f.log.add("up_attr.FindOne:%d/%d", mid, from)
	f.findOneCalls = append(f.findOneCalls, attrKey{mid: mid, from: from})
	if err := f.dbErr("FindOne"); err != nil {
		return nil, err
	}
	for _, a := range f.rows {
		if a.Mid == mid && a.From == from {
			cp := *a
			return &cp, nil
		}
	}
	return nil, nil
}

// --- up_switch 替身（本包唯一的写入口） ---

type fakeUpSwitchModel struct {
	faultInjector
	rows []*model.UpSwitch

	findOneCalls []switchKey
	upsertCalls  []switchKey
}

type switchKey struct {
	mid  int64
	from int32
}

func newFakeUpSwitchModel(log *callLog, race *raceHooks) *fakeUpSwitchModel {
	return &fakeUpSwitchModel{faultInjector: newFault("up_switch", log, race)}
}

var _ model.UpSwitchModel = (*fakeUpSwitchModel)(nil)

// FindOne 复刻 model/up_switch.go:39-49：无记录 (nil, nil)。
func (f *fakeUpSwitchModel) FindOne(_ context.Context, mid int64, from int32) (*model.UpSwitch, error) {
	f.fireRace("up_switch.FindOne")
	f.log.add("up_switch.FindOne:%d/%d", mid, from)
	f.findOneCalls = append(f.findOneCalls, switchKey{mid: mid, from: from})
	if err := f.dbErr("FindOne"); err != nil {
		return nil, err
	}
	if row := f.byKey(mid, from); row != nil {
		cp := *row
		return &cp, nil
	}
	return nil, nil
}

// Upsert 复刻 model/up_switch.go:51-55 的
// `INSERT ... ON DUPLICATE KEY UPDATE state = VALUES(state)`：冲突键是 PRIMARY KEY(mid, from)。
// 关键事实——**只写这三列**，没有 mtime/ctime 可保，也不校验 mid 是否「已存在」，
// 所以「给任意合法 mid 凭空插一行」是本方法的真实语义（守卫只能靠 logic）。
func (f *fakeUpSwitchModel) Upsert(_ context.Context, mid int64, from, state int32) error {
	f.fireRace("up_switch.Upsert")
	f.log.add("up_switch.Upsert:%d/%d/%d", mid, from, state)
	f.upsertCalls = append(f.upsertCalls, switchKey{mid: mid, from: from})
	if err := f.dbErr("Upsert"); err != nil {
		return err
	}
	if row := f.byKey(mid, from); row != nil {
		row.State = state // 更新分支：只覆盖 state
		return nil
	}
	f.rows = append(f.rows, &model.UpSwitch{Mid: mid, From: from, State: state})
	return nil
}

func (f *fakeUpSwitchModel) byKey(mid int64, from int32) *model.UpSwitch {
	for _, s := range f.rows {
		if s.Mid == mid && s.From == from {
			return s
		}
	}
	return nil
}

// --- sign_up 替身 ---

type fakeSignUpModel struct {
	faultInjector
	rows []*model.SignUp

	findManyCalls [][]int64
}

func newFakeSignUpModel(log *callLog, race *raceHooks) *fakeSignUpModel {
	return &fakeSignUpModel{faultInjector: newFault("sign_up", log, race)}
}

var _ model.SignUpModel = (*fakeSignUpModel)(nil)

// FindMany 复刻 model/sign_up.go:34-52：`WHERE mid IN (?)`，缺失的 mid 不进 map；
// 返回的指针指向库存行本体，所以这里逐个副本交回。
func (f *fakeSignUpModel) FindMany(_ context.Context, mids []int64) (map[int64]*model.SignUp, error) {
	f.fireRace("sign_up.FindMany")
	f.log.add("sign_up.FindMany:n=%d", len(mids))
	f.findManyCalls = append(f.findManyCalls, slices.Clone(mids))
	if err := f.dbErr("FindMany"); err != nil {
		return nil, err
	}
	out := make(map[int64]*model.SignUp, len(mids))
	if len(mids) == 0 {
		return out, nil // model/sign_up.go:35-37
	}
	for _, r := range f.rows {
		if slices.Contains(mids, r.Mid) {
			cp := *r
			out[cp.Mid] = &cp
		}
	}
	return out, nil
}

// --- 装配 ---

// store 汇总缓存替身、6 个 model 替身、共享调用轨迹与**真实 Repository**。
type store struct {
	log  *callLog
	race *raceHooks

	cache    *fakeCache
	special  *fakeUpSpecialModel
	group    *fakeUpGroupModel
	groupMem *fakeUpGroupMemberModel
	attr     *fakeUpAttrModel
	sw       *fakeUpSwitchModel
	sign     *fakeSignUpModel

	repo *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	race := &raceHooks{}
	st := &store{
		log:     log,
		race:    race,
		cache:   newFakeCache(log, race),
		special: newFakeUpSpecialModel(log, race),
		group:   newFakeUpGroupModel(log, race),
		attr:    newFakeUpAttrModel(log, race),
		sw:      newFakeUpSwitchModel(log, race),
		sign:    newFakeSignUpModel(log, race),
	}
	st.groupMem = newFakeUpGroupMemberModel(log, race, st.special)
	st.repo = repository.NewWithDeps(st.cache, st.special, st.group, st.groupMem,
		st.attr, st.sw, st.sign)
	return st
}

// svcCtx 装配一个真实 ServiceContext：Config 留零值即可，8 个 logic 只读 Repository 一个字段。
func (st *store) svcCtx() *svc.ServiceContext {
	return &svc.ServiceContext{Config: config.Config{}, Repository: st.repo}
}

// raceBefore 布一个一次性并发钩子：在第 method 这次调用执行之前改库，模拟别的入口插队。
// 钩子若始终没触发，checkRaces 会让用例失败（前提没成立＝断言在空转）。
func (st *store) raceBefore(method string, fn func()) { st.race.arm(method, fn) }

func (st *store) checkRaces(t *testing.T) {
	t.Helper()
	st.race.assertFired(t, t.Name())
}

// --- 读回库存与缓存（静默：不经 model/cache 接口、不写 callLog） ---

func (st *store) switchRow(t *testing.T, mid int64, from int32) *model.UpSwitch {
	t.Helper()
	row := st.sw.byKey(mid, from)
	if row == nil {
		t.Fatalf("读回 up_switch(%d,%d)：库里没有这一行（现有 %d 行）", mid, from, len(st.sw.rows))
	}
	cp := *row
	return &cp
}

func (st *store) cacheRaw(t *testing.T, key string) string {
	t.Helper()
	bs, ok := st.cache.raw[key]
	if !ok {
		t.Fatalf("读回缓存 key %q：不存在（现有 %v）", key, st.cache.keysSorted())
	}
	return bs
}

func (st *store) cacheHas(key string) bool {
	_, ok := st.cache.raw[key]
	return ok
}

func (st *store) cacheTTL(t *testing.T, key string) int {
	t.Helper()
	ttl, ok := st.cache.ttl[key]
	if !ok {
		t.Fatalf("读回缓存 key %q 的 TTL：key 不存在", key)
	}
	return ttl
}

func (f *fakeCache) keysSorted() []string {
	keys := make([]string, 0, len(f.raw))
	for k := range f.raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// storeCounts 是「五张表各有多少行」的静默快照：失败用例靠它断言**残留形态与爆炸半径**。
type storeCounts struct {
	special, group, attr, sw, sign int
}

func (st *store) counts() storeCounts {
	return storeCounts{
		special: len(st.special.rows),
		group:   len(st.group.rows),
		attr:    len(st.attr.rows),
		sw:      len(st.sw.rows),
		sign:    len(st.sign.rows),
	}
}

// --- 布数据（静默，且必须布成「生产写得出来的行」） ---

// seedSpecial 布 up_special 行（PRIMARY KEY(mid, group_id)，见 000002_create_up_special.sql:21）。
func (st *store) seedSpecial(mid int64, groupIDs ...int64) {
	if mid <= 0 {
		panic("seedSpecial: mid 是 BIGINT UNSIGNED，必须 > 0")
	}
	for _, gid := range groupIDs {
		if gid <= 0 {
			panic(fmt.Sprintf("seedSpecial: group_id=%d 非法（BIGINT UNSIGNED）", gid))
		}
		for _, r := range st.special.rows {
			if r.mid == mid && r.groupID == gid {
				panic(fmt.Sprintf("seedSpecial: (%d,%d) 重复（PRIMARY KEY(mid,group_id)）", mid, gid))
			}
		}
		st.special.rows = append(st.special.rows, upSpecialRow{mid: mid, groupID: gid})
	}
}

func (st *store) seedGroup(g *model.UpGroup) {
	switch {
	case g.ID <= 0:
		panic("seedGroup: id 是 AUTO_INCREMENT 主键，布数据必须显式给")
	case g.Name == "":
		panic("seedGroup: name 非空（迁移 DEFAULT ''，但运营面写入必有组名）")
	}
	for _, exist := range st.group.rows {
		if exist.ID == g.ID {
			panic(fmt.Sprintf("seedGroup: id=%d 重复", g.ID))
		}
	}
	cp := *g
	st.group.rows = append(st.group.rows, &cp)
}

func (st *store) seedAttr(mid int64, from, isAuthor int32) {
	switch {
	case mid <= 0:
		panic("seedAttr: mid 必须 > 0")
	case from < 0 || from > 3:
		panic(fmt.Sprintf("seedAttr: from=%d 生产写不出来（0..3，见 000003_create_up_attr.sql:20）", from))
	case isAuthor != 0 && isAuthor != 1:
		panic(fmt.Sprintf("seedAttr: is_author=%d 非法（0/1）", isAuthor))
	}
	for _, a := range st.attr.rows {
		if a.Mid == mid && a.From == from {
			panic(fmt.Sprintf("seedAttr: (%d,%d) 重复（PRIMARY KEY(mid,from)）", mid, from))
		}
	}
	st.attr.rows = append(st.attr.rows, &model.UpAttr{Mid: mid, From: from, IsAuthor: isAuthor})
}

func (st *store) seedSwitch(mid int64, from, state int32) {
	switch {
	case mid <= 0:
		panic("seedSwitch: mid 必须 > 0")
	case from != 0 && from != 1:
		panic(fmt.Sprintf("seedSwitch: from=%d 生产写不出来（0/1，见 000004_create_up_switch.sql:19）", from))
	case state != 0 && state != 1:
		panic(fmt.Sprintf("seedSwitch: state=%d 非法（0/1）", state))
	}
	for _, s := range st.sw.rows {
		if s.Mid == mid && s.From == from {
			panic(fmt.Sprintf("seedSwitch: (%d,%d) 重复（PRIMARY KEY(mid,from)）", mid, from))
		}
	}
	st.sw.rows = append(st.sw.rows, &model.UpSwitch{Mid: mid, From: from, State: state})
}

func (st *store) seedSignUp(mid int64, state int32, begin, end int64) {
	switch {
	case mid <= 0:
		panic("seedSignUp: mid 是主键，必须 > 0")
	case state <= 0:
		panic(fmt.Sprintf("seedSignUp: state=%d 生产写不出来（DEFAULT 1，业务定义 1 生效/2 到期/3 终止）", state))
	case begin < 0 || end < 0:
		panic("seedSignUp: begin_date/end_date 是 BIGINT UNSIGNED，不能为负")
	case begin > 0 && end > 0 && end < begin:
		panic("seedSignUp: end_date < begin_date 不是生产写得出来的窗口")
	}
	for _, r := range st.sign.rows {
		if r.Mid == mid {
			panic(fmt.Sprintf("seedSignUp: mid=%d 重复（PRIMARY KEY(mid)）", mid))
		}
	}
	st.sign.rows = append(st.sign.rows, &model.SignUp{Mid: mid, State: state, BeginDate: begin, EndDate: end})
}

// warmSwitch / warmSpecial / warmAttr / warmGroups / warmGroupMids 是缓存布景：
// 传的是**原始串**（与 Redis 里真正躺着的字节一致），所以能布出「脏值」这类形态。
func (st *store) warmSwitch(mid int64, from int32, raw string) {
	st.cache.warm(fakeKeySwitch(mid, from), raw)
}

func (st *store) warmSpecial(mid int64, raw string) { st.cache.warm(fakeKeySpecial(mid), raw) }

func (st *store) warmAttr(mid int64, from int32, raw string) {
	st.cache.warm(fakeKeyAttr(mid, from), raw)
}

func (st *store) warmGroups(raw string) { st.cache.warm(fakeKeyGroups(), raw) }

func (st *store) warmGroupMids(gid int64, pn, ps int32, raw string) {
	st.cache.warm(fakeKeyGroupMem(gid, pn, ps), raw)
}

// mustSetUpSwitch 在钩子里跑一次**完整的生产写路径**（缺陷 D1 的插队方就是这个 RPC 本身）。
// 注意它复用同一个 store，所以会把 up_switch.Upsert / cache.DelSwitch 记进同一条 callLog：
// 用例要断言的正是这条轨迹里「谁先谁后」。
func (st *store) mustSetUpSwitch(t *testing.T, mid int64, from, state int32) {
	t.Helper()
	in := &rpc.UpSwitchReq{Mid: mid, From: from, State: state}
	if _, err := NewSetUpSwitchLogic(context.Background(), st.svcCtx()).SetUpSwitch(in); err != nil {
		t.Fatalf("钩子里的并发 SetUpSwitch 失败：%v", err)
	}
}
