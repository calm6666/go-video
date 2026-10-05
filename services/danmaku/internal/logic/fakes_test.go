package logic

// fakes_test.go 是 danmaku logic 测试的内存替身集合。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, 内存 conn, 6 个内存 model, Options) 组装**真实的 Repository**，
// 只把它的 8 个依赖换成替身——这样「段缓存读穿/空段回填、屏蔽词降级、分钟窗口计数、
// 幂等键回查、主表 CAS + op_log 同事务、段计数派生」整条判定链都在被测路径上，
// 而不是把 Repository 也 mock 掉。见 internal/repository/cache.go 的 Cacher 注释。
//
// 四条替身纪律（rights / playback 两轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic 里的写回（如 PostDanmaku 的 `d.Dmid = dmid`、
//     ApplyStateTransition 的 `log.Dmid = ...`）不得污染库存行。
//  2. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：danmaku 要紧的结论是
//     「op_log 与主表 CAS 是不是同一个事务、失效缓存有没有排在事务之后、
//     重复投递有没有二次加段计数」。
//  3. 错误注入按方法粒度（faultInjector.failWith("FindOne", err)），不用一个全局 err。
//  4. 布数据走替身的**静默写入路径**（put/warm，不记 callLog）：布景不算被测调用，
//     因此轨迹断言可以直接从 0 开始数。用公开方法布景必须先在布景后取 st.log.snapshot()。
//
// 与 rights / playback 多出来的两条纪律：
//   - 分钟窗口 key 含 `now.Unix()/60`（logic 传的是真 time.Now()，不可注入），
//     所以布防用 `cache.freezeWindows()` 把桶冻成常量并返回该常量；
//     否则跨分钟边界会让密度用例随机变绿/变红。桶算法本身属于 *Cache 实现，
//     不在 logic 断言范围内（见 README 覆盖边界）。
//   - 自增主键按表分段起算（danmaku 从 101 起、blockword 从 501 起 …），
//     这样「新写入那一行的 ID」可以出现在精确序列期望里，不必回读。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**（唯一索引冲突、CAS 门槛、GREATEST 不为负、
//     ORDER BY + LIMIT/OFFSET 分页、空段命中），不证明 SQL 与列名本身；
//   - Redis 的 SETEX/INCR/TTL 与 JSON 编解码由真实 *Cache 承担，logic 用例只断言
//     「读/写/失效发生了什么顺序」，不验证 TTL 秒数；
//   - 事务原子性由 MySQL 保证，替身只断言「主表 CAS 与 op_log 拿到同一个非 nil 事务会话、
//     且只开一个事务、失败时确实回滚」；
//   - `errStateCasMiss` 在 repository 里是 `err ==` 身份比较，依赖 go-zero TransactCtx
//     不包装 fn 返回值；替身按不包装实现（与 go-zero 口径一致）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/ratelimit"
	"go-video/services/danmaku/internal/config"
	"go-video/services/danmaku/internal/policy"
	"go-video/services/danmaku/internal/repository"
	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

// --- 断言小工具（本包共享；与 rights/playback 同名但各包独立） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵（repository 用 %w 包装下游错误是允许的）。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want errors.Is(%v)", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

// wantErrContains 断言错误文案带着维度/参数说明（ErrRateLimited 会被 logic 拼上触发的维度）。
func wantErrContains(t *testing.T, label string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want 含 %q", label, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s：错误 = %v, want 含 %q", label, err, want)
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

// wantInt32sEQ 比较分段号切片。
func wantInt32sEQ(t *testing.T, label string, got, want []int32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}

// wantInt64sEQ 比较主键切片（「到底返回/改动了哪几行」）。
func wantInt64sEQ(t *testing.T, label string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", label, got, want)
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

// wantCount 断言某前缀的调用次数（用于自增 ID 不可预知的写侧断言）。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整序列 [%s]）",
			label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

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

// countIn 统计片段序列里某前缀的出现次数。wantCount 作用于整条轨迹，
// 一个用例里连续发多次被测调用时必须按片段数，否则会被前面的成功调用污染。
func countIn(ops []string, prefix string) int {
	n := 0
	for _, o := range ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

// --- 错误注入 ---

type faultInjector struct {
	by map[string]error
	// oneShot 是「只失败一次」的注入位（不叫 next：各 model 已有 next int64 自增主键字段，
	// 同名会造成 f.next 逐层遮蔽）。
	oneShot map[string]error
}

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

// failNext 只让该方法的**下一次**调用失败，之后自动恢复正常。
// 用于「同一方法在一次用例里被调用两次、必须让第二次失败」的路径，
// 例如 ApplyModerationResult 在 CAS 未命中后重读弹幕时下游又报错。
func (f *faultInjector) failNext(method string, err error) {
	if f.oneShot == nil {
		f.oneShot = map[string]error{}
	}
	f.oneShot[method] = err
}

func (f *faultInjector) fail(method string) error {
	if err, ok := f.oneShot[method]; ok {
		delete(f.oneShot, method)
		return err
	}
	return f.by[method]
}

// --- 事务回滚补偿 ---

// undoStack 是替身的事务回滚口径。
//
// 各 model 替身直接改内存 map，没有 MySQL 的 MVCC 快照，所以 TransactCtx 失败时必须
// 把本事务里做过的写逐条撤销；否则「CAS 未命中 ⇒ op_log 不留痕」「op_log 写失败 ⇒ 主表不脏」
// 这类断言会被替身自己的半成品数据带红，更糟的是把替身缺陷当成生产行为断言下去。
// 只有 *Tx 路径登记撤销项，非事务方法按自动提交处理（与真实 SQL 一致）。
type undoStack struct{ fns []func() }

func (u *undoStack) push(fn func()) {
	if u != nil {
		u.fns = append(u.fns, fn)
	}
}

func (u *undoStack) rollback() {
	if u == nil {
		return
	}
	for i := len(u.fns) - 1; i >= 0; i-- {
		u.fns[i]()
	}
	u.fns = u.fns[:0]
}

func (u *undoStack) commit() {
	if u != nil {
		u.fns = u.fns[:0]
	}
}

// --- Redis key 复刻（与 repository/cache.go 未导出常量逐字对齐；不一致时读穿用例即红） ---

const (
	keySegList   = "dm:seg:%d:%d"
	keySegCount  = "dm:cnt:%d:%d"
	keyBlockWord = "dm:bw:%d"
	keyUserBlock = "dm:ub:%d"
	keyRateMid   = "dm:rl:mid:%d:%d"
	keyRateOid   = "dm:rl:oid:%d:%d"
)

// --- 小工具 ---

// segLabel 复刻替身记轨迹时的分段列表格式（"[0 1]"），保证期望值与实现同源格式化规则。
func segLabel(segs []int32) string { return fmt.Sprintf("%v", segs) }

// goldenIdem 独立复刻幂等键派生规则：sha256hex("<oid>:<mid>:<客户端键>")。
// 这里**不调用** common/idempotency，否则就是「拿实现算期望值」的永真断言；
// 用例用它锁定键空间只由 (oid, mid, 客户端键) 决定。
func goldenIdem(oid, mid int64, clientKey string) string {
	raw := strconv.FormatInt(oid, 10) + ":" + strconv.FormatInt(mid, 10) + ":" + clientKey
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// errDB 是注入用的下游错误样本（每个方法各注入一次，断言 errors.Is 命中）。
var errDB = errors.New("inject: db down")

// dupEventErr / dupIdemErr 复刻 MySQL 1062 报文，repository.isDuplicateErr 按字符串判定。
func dupEventErr(eventID string) error {
	return fmt.Errorf("Error 1062: Duplicate entry '%s' for key 'danmaku_op_log.uniq_event'", eventID)
}

func dupIdemErr(key string) error {
	return fmt.Errorf("Error 1062: Duplicate entry '%s' for key 'danmaku.uniq_idempotency'", key)
}

// --- 缓存替身 ---

type fakeCache struct {
	faultInjector
	log *callLog

	segs       map[string][]*model.Danmaku   // 段列表；key 存在即为命中（空段也算命中，防击穿）
	counts     map[string]int32              // 段计数；key 存在即为命中
	cntHas     map[string]bool               // 0 计数也是有效命中，单独记存在位
	blockWords map[string][]string           // 屏蔽词库
	userBlocks map[string][]*model.UserBlock // 用户屏蔽项
	windows    map[string]int32              // 分钟窗口计数
	pinned     int64                         // 冻结后的分钟桶；pinnedOK 为真时启用
	pinnedOK   bool
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{
		log:        log,
		segs:       map[string][]*model.Danmaku{},
		counts:     map[string]int32{},
		cntHas:     map[string]bool{},
		blockWords: map[string][]string{},
		userBlocks: map[string][]*model.UserBlock{},
		windows:    map[string]int32{},
	}
}

// freezeWindows 把分钟桶冻在当前分钟并返回该桶号，消除跨分钟边界的随机翻红。
func (f *fakeCache) freezeWindows() int64 {
	f.pinned = time.Now().Unix() / 60
	f.pinnedOK = true
	return f.pinned
}

func (f *fakeCache) bucket(now time.Time) int64 {
	if f.pinnedOK {
		return f.pinned
	}
	return now.Unix() / 60
}

func (f *fakeCache) Ping(context.Context) error { f.log.add("cache.Ping"); return nil }

func (f *fakeCache) IncrMidWindow(_ context.Context, mid int64, now time.Time) (int32, error) {
	key := fmt.Sprintf(keyRateMid, mid, f.bucket(now))
	f.log.add("cache.IncrMid:%s", key)
	if err := f.fail("IncrMidWindow"); err != nil {
		return 0, err
	}
	f.windows[key]++
	return f.windows[key], nil
}

func (f *fakeCache) IncrOidWindow(_ context.Context, oid int64, now time.Time) (int32, error) {
	key := fmt.Sprintf(keyRateOid, oid, f.bucket(now))
	f.log.add("cache.IncrOid:%s", key)
	if err := f.fail("IncrOidWindow"); err != nil {
		return 0, err
	}
	f.windows[key]++
	return f.windows[key], nil
}

// window 读某个维度当前桶内累计值（断言「被拒绝的请求有没有照样把计数推上去」）。
func (f *fakeCache) window(fmtKey string, id, bucket int64) int32 {
	return f.windows[fmt.Sprintf(fmtKey, id, bucket)]
}

func (f *fakeCache) GetSegment(_ context.Context, oid int64, segNo int32) ([]*model.Danmaku, bool, error) {
	key := fmt.Sprintf(keySegList, oid, segNo)
	f.log.add("cache.GetSeg:%s", key)
	if err := f.fail("GetSegment"); err != nil {
		return nil, false, err
	}
	rows, ok := f.segs[key]
	if !ok {
		return nil, false, nil
	}
	return copyDanmaku(rows), true, nil
}

func (f *fakeCache) SetSegment(_ context.Context, oid int64, segNo int32, rows []*model.Danmaku) error {
	key := fmt.Sprintf(keySegList, oid, segNo)
	f.log.add("cache.SetSeg:%s=%d", key, len(rows))
	if err := f.fail("SetSegment"); err != nil {
		return err
	}
	f.segs[key] = copyDanmaku(rows)
	return nil
}

func (f *fakeCache) DelSegment(_ context.Context, oid int64, segNo int32) error {
	key := fmt.Sprintf(keySegList, oid, segNo)
	f.log.add("cache.DelSeg:%s", key)
	if err := f.fail("DelSegment"); err != nil {
		return err
	}
	delete(f.segs, key)
	return nil
}

func (f *fakeCache) GetSegmentCounts(_ context.Context, oid int64, segs []int32) (map[int32]int32, error) {
	out := map[int32]int32{}
	for _, s := range segs {
		key := fmt.Sprintf(keySegCount, oid, s)
		f.log.add("cache.GetCnt:%s", key)
		if err := f.fail("GetSegmentCounts"); err != nil {
			return out, err
		}
		if f.cntHas[key] {
			out[s] = f.counts[key]
		}
	}
	return out, nil
}

func (f *fakeCache) SetSegmentCount(_ context.Context, oid int64, segNo, count int32) error {
	key := fmt.Sprintf(keySegCount, oid, segNo)
	f.log.add("cache.SetCnt:%s=%d", key, count)
	if err := f.fail("SetSegmentCount"); err != nil {
		return err
	}
	f.counts[key] = count
	f.cntHas[key] = true
	return nil
}

func (f *fakeCache) IncrSegmentCount(_ context.Context, oid int64, segNo, delta int32) error {
	key := fmt.Sprintf(keySegCount, oid, segNo)
	f.log.add("cache.IncrCnt:%s=%+d", key, delta)
	if err := f.fail("IncrSegmentCount"); err != nil {
		return err
	}
	if !f.cntHas[key] {
		f.cntHas[key] = true
	}
	f.counts[key] += delta
	if f.counts[key] <= 0 {
		// 与真实 Cache 同口径：回删到 0 以下就清掉，让下一次读回源 DB 修正。
		delete(f.counts, key)
		delete(f.cntHas, key)
	}
	return nil
}

func (f *fakeCache) GetBlockWords(_ context.Context, oid int64) ([]string, bool, error) {
	key := fmt.Sprintf(keyBlockWord, oid)
	f.log.add("cache.GetBW:%s", key)
	if err := f.fail("GetBlockWords"); err != nil {
		return nil, false, err
	}
	w, ok := f.blockWords[key]
	if !ok {
		return nil, false, nil
	}
	return slices.Clone(w), true, nil
}

func (f *fakeCache) SetBlockWords(_ context.Context, oid int64, words []string) error {
	key := fmt.Sprintf(keyBlockWord, oid)
	f.log.add("cache.SetBW:%s=%d", key, len(words))
	if err := f.fail("SetBlockWords"); err != nil {
		return err
	}
	if words == nil {
		words = []string{}
	}
	f.blockWords[key] = slices.Clone(words)
	return nil
}

func (f *fakeCache) DelBlockWords(_ context.Context, oid int64) error {
	keys := []string{fmt.Sprintf(keyBlockWord, 0)}
	if oid > 0 {
		keys = append(keys, fmt.Sprintf(keyBlockWord, oid))
	}
	f.log.add("cache.DelBW:%s", strings.Join(keys, ","))
	if err := f.fail("DelBlockWords"); err != nil {
		return err
	}
	for _, k := range keys {
		delete(f.blockWords, k)
	}
	return nil
}

func (f *fakeCache) GetUserBlocks(_ context.Context, mid int64) ([]*model.UserBlock, bool, error) {
	key := fmt.Sprintf(keyUserBlock, mid)
	f.log.add("cache.GetUB:%s", key)
	if err := f.fail("GetUserBlocks"); err != nil {
		return nil, false, err
	}
	if mid <= 0 {
		return nil, false, nil
	}
	rows, ok := f.userBlocks[key]
	if !ok {
		return nil, false, nil
	}
	return copyUserBlocks(rows), true, nil
}

func (f *fakeCache) SetUserBlocks(_ context.Context, mid int64, rows []*model.UserBlock) error {
	key := fmt.Sprintf(keyUserBlock, mid)
	f.log.add("cache.SetUB:%s=%d", key, len(rows))
	if err := f.fail("SetUserBlocks"); err != nil {
		return err
	}
	if rows == nil {
		rows = []*model.UserBlock{}
	}
	f.userBlocks[key] = copyUserBlocks(rows)
	return nil
}

func (f *fakeCache) DelUserBlocks(_ context.Context, mid int64) error {
	key := fmt.Sprintf(keyUserBlock, mid)
	f.log.add("cache.DelUB:%s", key)
	if err := f.fail("DelUserBlocks"); err != nil {
		return err
	}
	delete(f.userBlocks, key)
	return nil
}

// --- 静默布景（纪律 4：不记 callLog） ---

func (f *fakeCache) warmSegment(oid int64, segNo int32, rows ...*model.Danmaku) {
	f.segs[fmt.Sprintf(keySegList, oid, segNo)] = copyDanmaku(rows)
}

func (f *fakeCache) warmSegmentCount(oid int64, segNo, count int32) {
	key := fmt.Sprintf(keySegCount, oid, segNo)
	f.counts[key] = count
	f.cntHas[key] = true
}

func (f *fakeCache) warmBlockWords(oid int64, words ...string) {
	f.blockWords[fmt.Sprintf(keyBlockWord, oid)] = slices.Clone(words)
}

func (f *fakeCache) warmUserBlocks(mid int64, rows ...*model.UserBlock) {
	f.userBlocks[fmt.Sprintf(keyUserBlock, mid)] = copyUserBlocks(rows)
}

func (f *fakeCache) segmentRows(oid int64, segNo int32) ([]*model.Danmaku, bool) {
	key := fmt.Sprintf(keySegList, oid, segNo)
	rows, ok := f.segs[key]
	return copyDanmaku(rows), ok
}

// blockWordSet 读替身里的屏蔽词库快照（断言用，不记轨迹）。
// 名字不能叫 blockWords —— 那会和结构体的 map 字段同名。
func (f *fakeCache) blockWordSet(oid int64) ([]string, bool) {
	key := fmt.Sprintf(keyBlockWord, oid)
	w, ok := f.blockWords[key]
	return slices.Clone(w), ok
}

// userBlockSet 读替身里某用户的屏蔽视图缓存（断言「失效清干净没有」用，不记轨迹）。
func (f *fakeCache) userBlockSet(mid int64) ([]*model.UserBlock, bool) {
	key := fmt.Sprintf(keyUserBlock, mid)
	rows, ok := f.userBlocks[key]
	return copyUserBlocks(rows), ok
}

func (f *fakeCache) count(oid int64, segNo int32) (int32, bool) {
	key := fmt.Sprintf(keySegCount, oid, segNo)
	return f.counts[key], f.cntHas[key]
}

// --- 弹幕主表 model 替身 ---

type fakeDanmakuModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.Danmaku
	byKey map[string]int64
	next  int64
	// pendingReveal 模拟并发重复投递：下一次 Insert 让这行落地并返回唯一键冲突，
	// 从而把「logic 预查 miss → 唯一索引兜底 → 回查命中」这条路径完整跑出来。
	pendingReveal *model.Danmaku
	// flipDmid/flipTo/flipOK 模拟「logic 读到某状态 → 并发消费者抢先改了它 → 本次 CAS 影响 0 行」：
	// 下一次 cas 之前先把该行状态改成 flipTo。
	flipDmid int64
	flipTo   int32
	flipOK   bool
	// undo 由 newStore 注入，事务失败时撤销本事务在主表上做过的写。
	undo *undoStack
	// sawTxSession / txNilSession 记录事务方法拿到的会话凭证：
	// 用于断言「主表 CAS 与 op_log 是否真在同一个事务里」。
	txSawSession bool
	txNilSession bool
	txCalls      int
}

// newFakeDanmakuModel 的自增起点取 100：第一条新落库弹幕的 dmid 恒为 101，
// 可以直接写进调用序列期望，不必回读。
func newFakeDanmakuModel(log *callLog) *fakeDanmakuModel {
	return &fakeDanmakuModel{log: log, rows: map[int64]*model.Danmaku{}, byKey: map[string]int64{}, next: 100}
}

func (f *fakeDanmakuModel) Insert(_ context.Context, d *model.Danmaku) (int64, error) {
	f.log.add("danmaku.Insert:%s", d.IdempotencyKey)
	if err := f.fail("Insert"); err != nil {
		return 0, fmt.Errorf("danmaku Insert: %w", err)
	}
	if f.pendingReveal != nil {
		p := f.pendingReveal
		f.pendingReveal = nil
		_, _ = f.put(p)
		return 0, dupIdemErr(d.IdempotencyKey)
	}
	id, err := f.put(d)
	if err != nil {
		return 0, fmt.Errorf("danmaku Insert: %w", err)
	}
	return id, nil
}

// put 是唯一约束的实际实现；seedDanmaku 直接调它布数据（不记轨迹）。
func (f *fakeDanmakuModel) put(d *model.Danmaku) (int64, error) {
	if d.IdempotencyKey != "" {
		if _, ok := f.byKey[d.IdempotencyKey]; ok {
			return 0, dupIdemErr(d.IdempotencyKey)
		}
	}
	f.next++
	cp := *d
	cp.Dmid = f.next
	f.rows[cp.Dmid] = &cp
	if cp.IdempotencyKey != "" {
		f.byKey[cp.IdempotencyKey] = cp.Dmid
	}
	return cp.Dmid, nil
}

func (f *fakeDanmakuModel) FindOne(_ context.Context, dmid int64) (*model.Danmaku, error) {
	f.log.add("danmaku.FindOne:%d", dmid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[dmid]
	if !ok {
		return nil, nil // 与 model 一致：查无此行返回 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (f *fakeDanmakuModel) FindByIdempotencyKey(_ context.Context, key string) (*model.Danmaku, error) {
	f.log.add("danmaku.FindByKey:%s", key)
	if err := f.fail("FindByIdempotencyKey"); err != nil {
		return nil, err
	}
	id, ok := f.byKey[key]
	if !ok {
		return nil, nil
	}
	cp := *f.rows[id]
	return &cp, nil
}

// ListVisibleBySegs 复刻 WHERE oid=? AND seg_no IN (...) AND state=0 AND pool=1
// ORDER BY progress_ms ASC LIMIT ?。真实 SQL 同 progress_ms 时不确定，
// 替身再按 dmid 升序稳定化（与 repository 的兜底排序同向）。
func (f *fakeDanmakuModel) ListVisibleBySegs(_ context.Context, oid int64, segs []int32, limit int32) ([]*model.Danmaku, error) {
	f.log.add("danmaku.ListVisible:%d/%s/%d", oid, segLabel(segs), limit)
	if err := f.fail("ListVisibleBySegs"); err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	want := map[int32]bool{}
	for _, s := range segs {
		want[s] = true
	}
	var hits []*model.Danmaku
	for _, row := range f.rows {
		if row.Oid != oid || !want[row.SegNo] {
			continue
		}
		if row.State != model.StateNormal || row.Pool != model.PoolNormal {
			continue
		}
		cp := *row
		hits = append(hits, &cp)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].ProgressMs != hits[j].ProgressMs {
			return hits[i].ProgressMs < hits[j].ProgressMs
		}
		return hits[i].Dmid < hits[j].Dmid
	})
	if int32(len(hits)) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// ListMineBySegs 复刻 state IN (pending, folded, rejected) 且 mid 限定的回显查询。
func (f *fakeDanmakuModel) ListMineBySegs(_ context.Context, oid, mid int64, segs []int32) ([]*model.Danmaku, error) {
	f.log.add("danmaku.ListMine:%d/%d/%s", oid, mid, segLabel(segs))
	if err := f.fail("ListMineBySegs"); err != nil {
		return nil, err
	}
	if len(segs) == 0 || mid <= 0 {
		return nil, nil
	}
	want := map[int32]bool{}
	for _, s := range segs {
		want[s] = true
	}
	var hits []*model.Danmaku
	for _, row := range f.rows {
		if row.Oid != oid || !want[row.SegNo] || row.Mid != mid {
			continue
		}
		if row.State != model.StatePending && row.State != model.StateFolded && row.State != model.StateRejected {
			continue
		}
		cp := *row
		hits = append(hits, &cp)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].ProgressMs < hits[j].ProgressMs })
	return hits, nil
}

func (f *fakeDanmakuModel) TransitionState(_ context.Context, dmid int64, from, to, toPool int32) (bool, error) {
	f.log.add("danmaku.Transition:%d/%d->%d/%d", dmid, from, to, toPool)
	if err := f.fail("TransitionState"); err != nil {
		return false, err
	}
	return f.cas(dmid, from, to, toPool, false)
}

func (f *fakeDanmakuModel) TransitionStateTx(_ context.Context, session sqlx.Session, dmid int64, from, to, toPool int32) (bool, error) {
	f.log.add("danmaku.TransitionTx:%d/%d->%d/%d", dmid, from, to, toPool)
	f.txCalls++
	if session == nil {
		f.txNilSession = true // 本该在事务内却拿到 nil 会话，等价于绕开事务
	} else {
		f.txSawSession = true
	}
	if err := f.fail("TransitionStateTx"); err != nil {
		return false, err
	}
	return f.cas(dmid, from, to, toPool, true)
}

// cas 复刻 UPDATE danmaku SET state=?, pool=?, mtime=? WHERE dmid=? AND state=?
// （model/danmaku.go:184-217）：CAS 条件不满足（含行不存在）时受影响 0 行，
// 返回 (false, nil) 而不是错误。inTx 为真时把这一行的改前值登记进事务撤销栈，
// 于是 TransactCtx 失败可以和 MySQL 一样把它还原。
func (f *fakeDanmakuModel) cas(dmid int64, from, to, toPool int32, inTx bool) (bool, error) {
	row, ok := f.rows[dmid]
	if !ok {
		return false, nil
	}
	if f.flipOK && f.flipDmid == dmid {
		f.flipOK = false
		row.State = f.flipTo // 并发消费者在「读到行」与「执行 UPDATE」之间改了状态
	}
	if row.State != from {
		return false, nil
	}
	if inTx {
		before := *row
		f.undo.push(func() { *row = before })
	}
	row.State, row.Pool, row.Mtime = to, toPool, time.Now().Unix()
	return true, nil
}

// advance 静默把某行推进到指定状态/池，模拟**并发消费者已提交**的结果
// （不记轨迹、不登记撤销：那是别人的事务，回滚我们的事务不该把它撤掉）。
func (f *fakeDanmakuModel) advance(dmid int64, state, pool int32) {
	if row, ok := f.rows[dmid]; ok {
		row.State, row.Pool, row.Mtime = state, pool, time.Now().Unix()
	}
}

// flipBeforeCas 武装一次性的「CAS 前被并发改状态」，用于测 ErrConcurrentUpdate 分支。
func (f *fakeDanmakuModel) flipBeforeCas(dmid int64, to int32) {
	f.flipDmid, f.flipTo, f.flipOK = dmid, to, true
}

func (f *fakeDanmakuModel) SetModerationTaskID(_ context.Context, dmid, taskID int64) error {
	f.log.add("danmaku.SetTask:%d/%d", dmid, taskID)
	if err := f.fail("SetModerationTaskID"); err != nil {
		return err
	}
	f.setTask(dmid, taskID)
	return nil
}

func (f *fakeDanmakuModel) SetModerationTaskIDTx(_ context.Context, _ sqlx.Session, dmid, taskID int64) error {
	f.log.add("danmaku.SetTaskTx:%d/%d", dmid, taskID)
	if err := f.fail("SetModerationTaskIDTx"); err != nil {
		return err
	}
	if row, ok := f.rows[dmid]; ok && row.ModerationTaskId == 0 {
		before := row.ModerationTaskId
		f.undo.push(func() { row.ModerationTaskId = before })
		row.ModerationTaskId = taskID
	}
	return nil
}

// setTask 复刻 UPDATE ... WHERE dmid=? AND moderation_task_id=0：只填首个任务号。
func (f *fakeDanmakuModel) setTask(dmid, taskID int64) {
	if row, ok := f.rows[dmid]; ok && row.ModerationTaskId == 0 {
		row.ModerationTaskId = taskID
	}
}

func (f *fakeDanmakuModel) CountVisibleByOid(_ context.Context, oid int64) (int64, error) {
	f.log.add("danmaku.CountVisible:%d", oid)
	if err := f.fail("CountVisibleByOid"); err != nil {
		return 0, err
	}
	var n int64
	for _, row := range f.rows {
		if row.Oid == oid && row.State == model.StateNormal && row.Pool == model.PoolNormal {
			n++
		}
	}
	return n, nil
}

func (f *fakeDanmakuModel) get(dmid int64) *model.Danmaku {
	row, ok := f.rows[dmid]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeDanmakuModel) countRows() int { return len(f.rows) }

func (f *fakeDanmakuModel) only() *model.Danmaku {
	if len(f.rows) != 1 {
		panic(fmt.Sprintf("期望 danmaku 只有 1 行，实际 %d 行", len(f.rows)))
	}
	for _, row := range f.rows {
		cp := *row
		return &cp
	}
	panic("unreachable")
}

// --- 段计数派生表替身 ---

type fakeSegmentModel struct {
	faultInjector
	log   *callLog
	rows  map[string]*model.DanmakuSegment
	next  int64
	calls []string
}

func newFakeSegmentModel(log *callLog) *fakeSegmentModel {
	return &fakeSegmentModel{log: log, rows: map[string]*model.DanmakuSegment{}, next: 400}
}

func segKey(oid int64, segNo int32) string { return fmt.Sprintf("%d/%d", oid, segNo) }

// Incr 复刻 INSERT ... ON DUPLICATE KEY UPDATE count = GREATEST(count + VALUES(count), 0)。
func (f *fakeSegmentModel) Incr(_ context.Context, oid int64, segNo, delta int32) error {
	f.log.add("segment.Incr:%d/%d/%+d", oid, segNo, delta)
	f.calls = append(f.calls, segKey(oid, segNo))
	if err := f.fail("Incr"); err != nil {
		return fmt.Errorf("danmaku_segment Incr: %w", err)
	}
	key := segKey(oid, segNo)
	row, ok := f.rows[key]
	if !ok {
		f.next++
		row = &model.DanmakuSegment{ID: f.next, Oid: oid, SegNo: segNo}
		f.rows[key] = row
	}
	if row.Count+delta < 0 {
		row.Count = 0
	} else {
		row.Count += delta
	}
	row.Mtime = time.Now().Unix()
	return nil
}

// ListBySegs 缺失分段不返回行（与 model 一致）。
func (f *fakeSegmentModel) ListBySegs(_ context.Context, oid int64, segs []int32) ([]*model.DanmakuSegment, error) {
	f.log.add("segment.ListBySegs:%d/%s", oid, segLabel(segs))
	if err := f.fail("ListBySegs"); err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, nil
	}
	var out []*model.DanmakuSegment
	for _, s := range segs {
		if row, ok := f.rows[segKey(oid, s)]; ok {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeSegmentModel) DeleteByOid(_ context.Context, oid int64) error {
	f.log.add("segment.DeleteByOid:%d", oid)
	if err := f.fail("DeleteByOid"); err != nil {
		return err
	}
	for k, row := range f.rows {
		if row.Oid == oid {
			delete(f.rows, k)
		}
	}
	return nil
}

func (f *fakeSegmentModel) countAt(oid int64, segNo int32) int32 {
	if row, ok := f.rows[segKey(oid, segNo)]; ok {
		return row.Count
	}
	return -1
}

// --- 屏蔽词表替身 ---

// vanishOnce 让下一次 Disable/Delete 报「受影响 0 行」并让该行消失：
// 模拟 FindOne 读到词条之后、执行 UPDATE/DELETE 之前，另一个运营把这行物理删了。
// 没有这个臂就测不到「预查有行、写时撞空」这条并发路径（repository 只按 ok 判定）。
type fakeBlockWordModel struct {
	faultInjector
	log        *callLog
	rows       map[string]*model.BlockWord // 唯一索引 uniq_word
	byID       map[int64]string
	next       int64
	calls      []string
	vanishOnce bool
}

func newFakeBlockWordModel(log *callLog) *fakeBlockWordModel {
	return &fakeBlockWordModel{log: log, rows: map[string]*model.BlockWord{}, byID: map[int64]string{}, next: 500}
}

// vanishBeforeWrite 武装一次「写之前那行被并发改没了」。
func (f *fakeBlockWordModel) vanishBeforeWrite() { f.vanishOnce = true }

// Upsert 复刻 ON DUPLICATE KEY UPDATE scope/oid/state/operator：重新启用不产生新行。
func (f *fakeBlockWordModel) Upsert(_ context.Context, w *model.BlockWord) (int64, error) {
	f.log.add("blockword.Upsert:%s", w.Word)
	f.calls = append(f.calls, w.Word)
	if err := f.fail("Upsert"); err != nil {
		return 0, fmt.Errorf("danmaku_blockword Upsert: %w", err)
	}
	now := time.Now().Unix()
	if row, ok := f.rows[w.Word]; ok {
		row.Scope, row.Oid, row.State, row.Operator, row.Mtime = w.Scope, w.Oid, w.State, w.Operator, now
		return row.WordID, nil
	}
	f.next++
	stored := *w
	stored.WordID = f.next
	stored.Ctime, stored.Mtime = now, now
	f.rows[stored.Word] = &stored
	f.byID[stored.WordID] = stored.Word
	return stored.WordID, nil
}

// Disable 复刻 UPDATE ... SET state=0, operator=?, mtime=? WHERE word=?
// （model/danmaku_blockword.go:87-99）：词不存在时受影响 0 行，返回 (false, nil)。
// 这条语句不在事务里（repository 直接调用），因此**不登记撤销项**——与 MySQL 自动提交一致。
func (f *fakeBlockWordModel) Disable(_ context.Context, word string, operator int64) (bool, error) {
	f.log.add("blockword.Disable:%s", word)
	if err := f.fail("Disable"); err != nil {
		return false, fmt.Errorf("danmaku_blockword Disable: %w", err)
	}
	row, ok := f.rows[word]
	if !ok {
		return false, nil
	}
	if f.vanishOnce {
		f.consumeVanish(word, row)
		return false, nil
	}
	row.State = model.BlockWordDisabled
	row.Operator = operator
	row.Mtime = time.Now().Unix()
	return true, nil
}

// Delete 复刻 DELETE FROM danmaku_blockword WHERE word=?：物理删行，不存在返回 (false, nil)。
// 同样是自动提交语句，不登记撤销项。
func (f *fakeBlockWordModel) Delete(_ context.Context, word string) (bool, error) {
	f.log.add("blockword.Delete:%s", word)
	if err := f.fail("Delete"); err != nil {
		return false, fmt.Errorf("danmaku_blockword Delete: %w", err)
	}
	row, ok := f.rows[word]
	if !ok {
		return false, nil
	}
	if f.vanishOnce {
		f.consumeVanish(word, row)
		return false, nil
	}
	delete(f.rows, word)
	delete(f.byID, row.WordID)
	return true, nil
}

// consumeVanish 落实「并发删除已发生」：那行归别人所有，本会话的写受影响 0 行。
func (f *fakeBlockWordModel) consumeVanish(word string, row *model.BlockWord) {
	f.vanishOnce = false
	delete(f.rows, word)
	delete(f.byID, row.WordID)
}

func (f *fakeBlockWordModel) FindOne(_ context.Context, word string) (*model.BlockWord, error) {
	f.log.add("blockword.FindOne:%s", word)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[word]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// ListEnabled 复刻 oid>0 时「全局词 + 本分区词」、oid<=0 时只取全局词，且都要求生效。
func (f *fakeBlockWordModel) ListEnabled(_ context.Context, oid int64) ([]*model.BlockWord, error) {
	f.log.add("blockword.ListEnabled:%d", oid)
	f.calls = append(f.calls, fmt.Sprintf("enabled:%d", oid))
	if err := f.fail("ListEnabled"); err != nil {
		return nil, err
	}
	var out []*model.BlockWord
	words := make([]string, 0, len(f.rows))
	for w := range f.rows {
		words = append(words, w)
	}
	sort.Strings(words) // map 遍历无序，排序让断言确定
	for _, w := range words {
		row := f.rows[w]
		if row.State != model.BlockWordEnabled {
			continue
		}
		if row.Scope == model.ScopeGlobal || (oid > 0 && row.Scope == model.ScopeOid && row.Oid == oid) {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out, nil
}

// List 复刻运营侧分页：pn<1→1、ps 越界→20、ORDER BY word_id DESC、total 为过滤后总数。
func (f *fakeBlockWordModel) List(_ context.Context, scope int32, oid int64, onlyEnabled bool, pn, ps int32) ([]*model.BlockWord, int32, error) {
	f.log.add("blockword.List:%d/%d/%t/%d/%d", scope, oid, onlyEnabled, pn, ps)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%t/%d/%d", scope, oid, onlyEnabled, pn, ps))
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 100 {
		ps = 20
	}
	ids := make([]int64, 0, len(f.rows))
	for id := range f.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	var matched []*model.BlockWord
	for _, id := range ids {
		row := f.rows[f.byID[id]]
		if scope > 0 && row.Scope != scope {
			continue
		}
		if oid > 0 && row.Oid != oid {
			continue
		}
		if onlyEnabled && row.State != model.BlockWordEnabled {
			continue
		}
		cp := *row
		matched = append(matched, &cp)
	}
	total := int32(len(matched))
	if total == 0 {
		return nil, 0, nil
	}
	start := int((pn - 1) * ps)
	if start >= len(matched) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(matched) {
		end = len(matched)
	}
	return matched[start:end], total, nil
}

func (f *fakeBlockWordModel) state(word string) int32 {
	if row, ok := f.rows[word]; ok {
		return row.State
	}
	return -1
}

// --- 用户屏蔽表替身 ---

type fakeUserBlockModel struct {
	faultInjector
	log   *callLog
	rows  map[string]*model.UserBlock // 唯一索引 uniq_mid_target
	next  int64
	calls []string
}

func newFakeUserBlockModel(log *callLog) *fakeUserBlockModel {
	return &fakeUserBlockModel{log: log, rows: map[string]*model.UserBlock{}, next: 700}
}

func ubKey(mid, blockedMid int64, keyword string) string {
	return fmt.Sprintf("%d/%d/%s", mid, blockedMid, keyword)
}

// Upsert 复刻 uniq_mid_target 的 ON DUPLICATE KEY UPDATE type/state/mtime。
func (f *fakeUserBlockModel) Upsert(_ context.Context, b *model.UserBlock) (int64, error) {
	f.log.add("userblock.Upsert:%s", ubKey(b.Mid, b.BlockedMid, b.Keyword))
	f.calls = append(f.calls, ubKey(b.Mid, b.BlockedMid, b.Keyword))
	if err := f.fail("Upsert"); err != nil {
		return 0, fmt.Errorf("danmaku_user_block Upsert: %w", err)
	}
	now := time.Now().Unix()
	key := ubKey(b.Mid, b.BlockedMid, b.Keyword)
	if row, ok := f.rows[key]; ok {
		row.Type, row.State, row.Mtime = b.Type, b.State, now
		return row.ID, nil
	}
	f.next++
	stored := *b
	stored.ID = f.next
	stored.Ctime, stored.Mtime = now, now
	f.rows[key] = &stored
	return stored.ID, nil
}

func (f *fakeUserBlockModel) ListEnabled(_ context.Context, mid int64) ([]*model.UserBlock, error) {
	f.log.add("userblock.ListEnabled:%d", mid)
	if err := f.fail("ListEnabled"); err != nil {
		return nil, err
	}
	if mid <= 0 {
		return nil, nil
	}
	var out []*model.UserBlock
	for _, k := range f.idsAsc() { // ORDER BY id ASC
		if row := f.rows[k]; row.Mid == mid && row.State == model.UserBlockOn {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out, nil
}

// List 复刻「total 含已解除项、列表只返回生效项、ORDER BY id DESC + LIMIT/OFFSET」。
func (f *fakeUserBlockModel) List(_ context.Context, mid int64, blockType int32, pn, ps int32) ([]*model.UserBlock, int32, error) {
	f.log.add("userblock.List:%d/%d/%d/%d", mid, blockType, pn, ps)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%d/%d", mid, blockType, pn, ps))
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	if mid <= 0 {
		return nil, 0, model.ErrInvalidMid
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 100 {
		ps = 20
	}
	var all, on []*model.UserBlock
	for _, k := range f.keysByIDDesc() { // ORDER BY id DESC
		row := f.rows[k]
		if row.Mid != mid {
			continue
		}
		if blockType > 0 && row.Type != blockType {
			continue
		}
		cp := *row
		all = append(all, &cp) // total 口径：含已解除
		if row.State == model.UserBlockOn {
			on = append(on, &cp)
		}
	}
	total := int32(len(all))
	if total == 0 {
		return nil, 0, nil
	}
	start := int((pn - 1) * ps)
	if start >= len(on) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(on) {
		end = len(on)
	}
	return on[start:end], total, nil
}

// keysByIDDesc 按主键降序列出唯一键，让 List 的排序口径可断言。
func (f *fakeUserBlockModel) keysByIDDesc() []string {
	keys := make([]string, 0, len(f.rows))
	for k := range f.rows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return f.rows[keys[i]].ID > f.rows[keys[j]].ID })
	return keys
}

// idsAsc 按主键升序列出唯一键，让 ListEnabled 的排序口径可断言。
func (f *fakeUserBlockModel) idsAsc() []string {
	keys := make([]string, 0, len(f.rows))
	for k := range f.rows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return f.rows[keys[i]].ID < f.rows[keys[j]].ID })
	return keys
}

func (f *fakeUserBlockModel) stateAt(mid, blockedMid int64, keyword string) int32 {
	if row, ok := f.rows[ubKey(mid, blockedMid, keyword)]; ok {
		return row.State
	}
	return -1
}

// --- 举报表替身 ---

type fakeReportModel struct {
	faultInjector
	log      *callLog
	rows     map[int64]*model.Report
	byTarget map[string]int64
	next     int64
	// reveal 模拟并发重复举报：本次 INSERT 撞 uniq_dmid_reporter，
	// 落地的是「另一个消费者刚提交的那行」，于是走真实实现里的回查重放分支。
	reveal *model.Report
}

func newFakeReportModel(log *callLog) *fakeReportModel {
	return &fakeReportModel{log: log, rows: map[int64]*model.Report{}, byTarget: map[string]int64{}, next: 900}
}

// Insert 复刻真实实现（model/danmaku_report.go:65-90）：
// 先查唯一索引锚点，命中即重放；未命中再写；写失败（并发撞 uniq_dmid_reporter）
// 时**回查一次**，查到就按重放返回、查不到才把错误上抛。
// 回查这一步必须留在替身里，否则「并发重复举报不报 500」这条路径在测试里根本走不到。
func (f *fakeReportModel) Insert(_ context.Context, r *model.Report) (int64, bool, error) {
	f.log.add("report.FindByTarget:%d/%d", r.Dmid, r.ReporterMid)
	if err := f.fail("FindByTarget"); err != nil {
		return 0, false, err
	}
	if id, ok := f.byTarget[reportKey(r.Dmid, r.ReporterMid)]; ok {
		return id, false, nil
	}
	f.log.add("report.Insert:%d/%d", r.Dmid, r.ReporterMid)
	execErr := f.fail("Insert")
	if execErr == nil && f.reveal != nil {
		p := f.reveal
		f.reveal = nil
		// 别人的事务在我们 Exec 之前提交了同一行；本行的落地必然成功，失败即替身自身缺陷，直接炸出来。
		if _, _, err := f.land(p); err != nil {
			return 0, false, err
		}
		execErr = dupReportErr(r.Dmid, r.ReporterMid)
	}
	if execErr != nil {
		f.log.add("report.FindByTarget:%d/%d", r.Dmid, r.ReporterMid)
		if qerr := f.fail("FindByTargetAfterDup"); qerr != nil {
			return 0, false, qerr
		}
		if id, ok := f.byTarget[reportKey(r.Dmid, r.ReporterMid)]; ok {
			return id, false, nil
		}
		return 0, false, fmt.Errorf("danmaku_report Insert: %w", execErr)
	}
	return f.land(r)
}

// land 是唯一约束的实际落地动作（不记轨迹）。
func (f *fakeReportModel) land(r *model.Report) (int64, bool, error) {
	key := reportKey(r.Dmid, r.ReporterMid)
	if id, ok := f.byTarget[key]; ok {
		return id, false, nil
	}
	f.next++
	stored := *r
	stored.ReportID = f.next
	stored.Mtime = stored.Ctime
	f.rows[stored.ReportID] = &stored
	f.byTarget[key] = stored.ReportID
	return stored.ReportID, true, nil
}

// armConcurrentReplay 武装一次「本次写入撞上并发已提交的同一举报」。
func (f *fakeReportModel) armConcurrentReplay(r *model.Report) { f.reveal = r }

func dupReportErr(dmid, reporterMid int64) error {
	return fmt.Errorf("Error 1062: Duplicate entry '%d-%d' for key 'danmaku_report.uniq_dmid_reporter'", dmid, reporterMid)
}

func reportKey(dmid, reporterMid int64) string { return fmt.Sprintf("%d/%d", dmid, reporterMid) }

func (f *fakeReportModel) FindByTarget(_ context.Context, dmid, reporterMid int64) (*model.Report, error) {
	f.log.add("report.FindByTarget:%d/%d", dmid, reporterMid)
	if err := f.fail("FindByTarget"); err != nil {
		return nil, err
	}
	id, ok := f.byTarget[reportKey(dmid, reporterMid)]
	if !ok {
		return nil, nil
	}
	cp := *f.rows[id]
	return &cp, nil
}

func (f *fakeReportModel) ListPending(_ context.Context, pn, ps int32) ([]*model.Report, int32, error) {
	f.log.add("report.ListPending:%d/%d", pn, ps)
	if err := f.fail("ListPending"); err != nil {
		return nil, 0, err
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 100 {
		ps = 20
	}
	var pending []*model.Report
	for _, id := range sortedIDs(f.rows) {
		row := f.rows[id]
		if row.State == model.ReportPending {
			cp := *row
			pending = append(pending, &cp)
		}
	}
	total := int32(len(pending))
	if total == 0 {
		return nil, 0, nil
	}
	start := int((pn - 1) * ps)
	if start >= len(pending) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(pending) {
		end = len(pending)
	}
	return pending[start:end], total, nil
}

func (f *fakeReportModel) MarkHandled(_ context.Context, reportID int64, state int32) error {
	f.log.add("report.MarkHandled:%d/%d", reportID, state)
	if err := f.fail("MarkHandled"); err != nil {
		return err
	}
	row, ok := f.rows[reportID]
	if !ok {
		return model.ErrReportNotFound
	}
	row.State = state
	return nil
}

func (f *fakeReportModel) get(reportID int64) *model.Report {
	row, ok := f.rows[reportID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func sortedIDs(rows map[int64]*model.Report) []int64 {
	ids := make([]int64, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// --- 操作留痕表替身 ---

type fakeOpLogModel struct {
	faultInjector
	log          *callLog
	rows         []*model.OpLog
	byEvent      map[string]bool
	next         int64
	txSawSession bool
	txNilSession bool
	// dedupeBlind 让 ExistsEvent 无视 byEvent 恒返回「未消费」，
	// 用于跑通「去重快查读到 miss → 真正写留痕时才撞 uniq_event」的竞态窗口。
	dedupeBlind bool
	// undo 由 newStore 注入，事务失败时撤销本事务写下的留痕行与事件占位。
	undo *undoStack
}

func newFakeOpLogModel(log *callLog) *fakeOpLogModel {
	return &fakeOpLogModel{log: log, byEvent: map[string]bool{}, next: 300}
}

func (f *fakeOpLogModel) Insert(_ context.Context, l *model.OpLog) (int64, error) {
	f.log.add("oplog.Insert:%d/%s", l.Dmid, l.Action)
	return f.insert(l, false)
}

func (f *fakeOpLogModel) InsertTx(_ context.Context, session sqlx.Session, l *model.OpLog) (int64, error) {
	f.log.add("oplog.InsertTx:%d/%s/%s", l.Dmid, l.Action, eventLabel(l.EventID))
	if session == nil {
		f.txNilSession = true
	} else {
		f.txSawSession = true
	}
	return f.insert(l, true)
}

func eventLabel(id string) string {
	if id == "" {
		return "-"
	}
	return id
}

// insert 复刻 uniq_event 唯一索引：event_id 非空且已存在时返回 1062 报文，
// 让 repository.isDuplicateErr 走真实判定路径。NULL（空串）不参与唯一约束。
func (f *fakeOpLogModel) insert(l *model.OpLog, txPath bool) (int64, error) {
	fault := "Insert"
	if txPath {
		fault = "InsertTx"
	}
	if err := f.fail(fault); err != nil {
		return 0, fmt.Errorf("danmaku_op_log Insert: %w", err)
	}
	if l.EventID != "" {
		if f.byEvent[l.EventID] {
			return 0, dupEventErr(l.EventID)
		}
		f.byEvent[l.EventID] = true
	}
	if l.Ctime == 0 {
		l.Ctime = time.Now().Unix()
	}
	f.next++
	cp := *l
	cp.LogID = f.next
	f.rows = append(f.rows, &cp)
	if txPath {
		idx, event := len(f.rows)-1, l.EventID
		f.undo.push(func() {
			if idx < len(f.rows) && f.rows[idx].LogID == cp.LogID {
				f.rows = append(f.rows[:idx], f.rows[idx+1:]...)
			}
			if event != "" {
				delete(f.byEvent, event)
			}
		})
	}
	return cp.LogID, nil
}

func (f *fakeOpLogModel) ExistsEvent(_ context.Context, eventID string) (bool, error) {
	f.log.add("oplog.ExistsEvent:%s", eventID)
	if err := f.fail("ExistsEvent"); err != nil {
		return false, fmt.Errorf("danmaku_op_log ExistsEvent: %w", err)
	}
	if eventID == "" {
		return false, nil
	}
	if f.dedupeBlind {
		return false, nil
	}
	return f.byEvent[eventID], nil
}

func (f *fakeOpLogModel) ListByDmid(_ context.Context, dmid int64, limit int32) ([]*model.OpLog, error) {
	f.log.add("oplog.ListByDmid:%d/%d", dmid, limit)
	if err := f.fail("ListByDmid"); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var out []*model.OpLog
	for i := len(f.rows) - 1; i >= 0; i-- { // ORDER BY log_id DESC
		if f.rows[i].Dmid == dmid {
			cp := *f.rows[i]
			out = append(out, &cp)
		}
	}
	if int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeOpLogModel) actions() []string {
	out := make([]string, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, fmt.Sprintf("%s:%d->%d/%s", r.Action, r.FromState, r.ToState, eventLabel(r.EventID)))
	}
	return out
}

// --- 事务连接替身 ---

// txSession 只是「拿到了事务会话」的凭证；fake models 只判断它非 nil。
type txSession struct{ sqlx.Session }

// fakeConn 只实现 danmaku 用到的 TransactCtx。其它方法落在嵌入的 nil 接口上会直接 panic，
// 等价于「logic 单测路径上不允许出现任何直连 SQL」——真出现了就是越界，让它炸出来。
type fakeConn struct {
	sqlx.SqlConn
	log  *callLog
	undo *undoStack
	// onRollback 在回滚之后执行，用于模拟「我们的事务失败的同一瞬间，并发消费者提交了」。
	onRollback   func()
	transactions int
	rolledBack   int
	committed    int
}

func (f *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	f.transactions++
	f.log.add("tx.Begin")
	if err := fn(ctx, &txSession{}); err != nil {
		f.rolledBack++
		f.log.add("tx.Rollback")
		f.undo.rollback()
		if f.onRollback != nil {
			f.onRollback()
		}
		return err
	}
	f.committed++
	f.log.add("tx.Commit")
	f.undo.commit()
	return nil
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

// --- 进程级限流替身 ---

// fakeLimiter 实现 ratelimit.Limiter。done 也进 callLog，
// 因为「Allow 成功后必须回调 done」本身就是 common/ratelimit 的契约。
type fakeLimiter struct {
	log     *callLog
	err     error
	doneOps []ratelimit.Op
}

func (f *fakeLimiter) Allow(context.Context) (func(ratelimit.Op), error) {
	f.log.add("limiter.Allow")
	if f.err != nil {
		return nil, f.err
	}
	return func(op ratelimit.Op) {
		f.doneOps = append(f.doneOps, op)
		f.log.add("limiter.Done:%d", op)
	}, nil
}

// --- 审核下游替身 ---

// fakeModeration 实现 repository.ModerationClient。
// 未布景 taskID 时**必须报错**：否则「送审失败不能伪造已送审」这类断言会被静默放过。
type fakeModeration struct {
	log    *callLog
	calls  []string
	taskID int64
	err    error
}

func (f *fakeModeration) SubmitForReview(_ context.Context, dmid, mid int64, reason string) (int64, error) {
	f.log.add("moderation.Submit:%d/%d/%s", dmid, mid, reason)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%s", dmid, mid, reason))
	if f.err != nil {
		return 0, f.err
	}
	if f.taskID == 0 {
		return 0, errors.New("fakeModeration: 用例未布景 taskID，拒绝返回伪成功")
	}
	return f.taskID, nil
}

// --- 装配 ---

type store struct {
	log       *callLog
	cache     *fakeCache
	danmaku   *fakeDanmakuModel
	segment   *fakeSegmentModel
	blockWord *fakeBlockWordModel
	userBlock *fakeUserBlockModel
	report    *fakeReportModel
	opLog     *fakeOpLogModel
	conn      *fakeConn
	repo      *repository.Repository
}

func newStore(opt repository.Options) *store {
	log := &callLog{}
	u := &undoStack{}
	st := &store{
		log:       log,
		cache:     newFakeCache(log),
		danmaku:   newFakeDanmakuModel(log),
		segment:   newFakeSegmentModel(log),
		blockWord: newFakeBlockWordModel(log),
		userBlock: newFakeUserBlockModel(log),
		report:    newFakeReportModel(log),
		opLog:     newFakeOpLogModel(log),
		conn:      &fakeConn{log: log},
	}
	// 事务回滚补偿：主表与留痕表共用一个撤销栈，TransactCtx 失败时按登记的逆序撤销。
	st.danmaku.undo = u
	st.opLog.undo = u
	st.conn.undo = u
	st.repo = repository.NewWithDeps(st.cache, st.conn,
		st.danmaku, st.segment, st.blockWord, st.userBlock, st.report, st.opLog, opt)
	return st
}

// defaultConf 是 etc/danmaku.v1.yaml 的缺省口径（机审 + 屏蔽词双门禁全开）。
func defaultConf() config.DanmakuConf {
	return config.DanmakuConf{
		SegmentSeconds:            6,
		MaxContentLength:          100,
		MaxPerUserPerMinute:       20,
		MaxPerOidPerMinute:        6000,
		PostQps:                   800,
		PostBurst:                 200,
		SensitiveWordCheckEnabled: true,
		MachineReviewEnabled:      true,
		SegmentCacheTTLSeconds:    15,
		SegmentCountTTLSeconds:    60,
		BlockWordCacheTTLSeconds:  300,
		UserBlockCacheTTLSeconds:  120,
		BlockWordCacheMaxWords:    2000,
		MaxSegWindow:              60,
		MaxListLimit:              3000,
	}
}

// env 是一次用例的完整运行时：真实 Repository（8 个依赖为替身）+ 限流/审核替身。
//
// Options 由同一份 DanmakuConf 派生，与生产 svc 的映射保持一致：
// 若哪天只改一边（配置加了窗口上限、repository 仍拿旧值），本装配会让用例立刻红。
type env struct {
	t      *testing.T
	st     *store
	lim    *fakeLimiter
	mod    *fakeModeration
	svcCtx *svc.ServiceContext
	conf   config.DanmakuConf
}

func newEnv(t *testing.T) *env { return newEnvConf(t, defaultConf()) }

// newEnvConf 按给定配置装配；mutate 由用例通过 newEnvConf(t, cfg) 事先改好的 cfg 承载。
func newEnvConf(t *testing.T, cfg config.DanmakuConf) *env {
	t.Helper()
	st := newStore(repository.Options{
		Cache: repository.CacheOptions{
			SegmentTTLSeconds:      cfg.SegmentCacheTTLSeconds,
			SegmentCountTTLSeconds: cfg.SegmentCountTTLSeconds,
			BlockWordTTLSeconds:    cfg.BlockWordCacheTTLSeconds,
			UserBlockTTLSeconds:    cfg.UserBlockCacheTTLSeconds,
		},
		MaxSegWindow:           cfg.MaxSegWindow,
		MaxListLimit:           cfg.MaxListLimit,
		BlockWordCacheMaxWords: cfg.BlockWordCacheMaxWords,
	})
	lim := &fakeLimiter{log: st.log}
	mod := &fakeModeration{log: st.log}
	return &env{
		t:   t,
		st:  st,
		lim: lim,
		mod: mod,
		svcCtx: &svc.ServiceContext{
			Config:         config.Config{Danmaku: cfg},
			Repository:     st.repo,
			Moderation:     mod,
			PostLimiter:    lim,
			SegmentSeconds: policy.SegmentSeconds(cfg.SegmentSeconds),
		},
		conf: cfg,
	}
}

// ops 取被测调用序列（布景不写轨迹，见纪律 4，因此可从 0 开始断言）。
func (e *env) ops() []string { return e.st.log.ops }

// --- 静默布景（纪律 4） ---

// seedDanmaku 布一行弹幕（走 put，不记轨迹），返回库里那一行的副本。
// 入参 Dmid 会被忽略并按自增分配（与真实 INSERT 同口径，见纪律 2 之延伸）。
func seedDanmaku(t *testing.T, st *store, d *model.Danmaku) *model.Danmaku {
	t.Helper()
	if _, err := st.danmaku.put(d); err != nil {
		t.Fatalf("布景写入弹幕失败：%v", err)
	}
	return st.danmaku.get(st.danmaku.next)
}

func seedBlockWord(t *testing.T, st *store, w *model.BlockWord) *model.BlockWord {
	t.Helper()
	before := st.log.snapshot()
	id, err := st.blockWord.Upsert(context.Background(), w)
	st.log.ops = st.log.ops[:before] // 布景不算被测调用（纪律 4）
	if err != nil {
		t.Fatalf("布景写入屏蔽词失败：%v", err)
	}
	st.blockWord.calls = st.blockWord.calls[:len(st.blockWord.calls)-1]
	row := *st.blockWord.rows[w.Word]
	row.WordID = id
	return &row
}

func seedUserBlock(t *testing.T, st *store, b *model.UserBlock) *model.UserBlock {
	t.Helper()
	before := st.log.snapshot()
	if _, err := st.userBlock.Upsert(context.Background(), b); err != nil {
		t.Fatalf("布景写入用户屏蔽失败：%v", err)
	}
	st.log.ops = st.log.ops[:before]
	st.userBlock.calls = st.userBlock.calls[:len(st.userBlock.calls)-1]
	return st.userBlock.rows[ubKey(b.Mid, b.BlockedMid, b.Keyword)]
}

func seedSegment(t *testing.T, st *store, oid int64, segNo, count int32) {
	t.Helper()
	before := st.log.snapshot()
	if err := st.segment.Incr(context.Background(), oid, segNo, count); err != nil {
		t.Fatalf("布景写入段计数失败：%v", err)
	}
	st.log.ops = st.log.ops[:before]
	st.segment.calls = st.segment.calls[:len(st.segment.calls)-1]
}

func seedReport(t *testing.T, st *store, r *model.Report) *model.Report {
	t.Helper()
	// 走 land（唯一约束的实际落地动作），既不计轨迹也不消费故障注入位。
	id, _, err := st.report.land(r)
	if err != nil {
		t.Fatalf("布景写入举报失败：%v", err)
	}
	return st.report.get(id)
}

func seedOpLog(t *testing.T, st *store, l *model.OpLog) *model.OpLog {
	t.Helper()
	before := st.log.snapshot()
	if _, err := st.opLog.Insert(context.Background(), l); err != nil {
		t.Fatalf("布景写入留痕失败：%v", err)
	}
	st.log.ops = st.log.ops[:before]
	return st.opLog.rows[len(st.opLog.rows)-1]
}

// --- 值拷贝辅助（纪律 1） ---

func copyDanmaku(rows []*model.Danmaku) []*model.Danmaku {
	out := make([]*model.Danmaku, 0, len(rows))
	for _, r := range rows {
		cp := *r
		out = append(out, &cp)
	}
	return out
}

func copyUserBlocks(rows []*model.UserBlock) []*model.UserBlock {
	out := make([]*model.UserBlock, 0, len(rows))
	for _, r := range rows {
		cp := *r
		out = append(out, &cp)
	}
	return out
}

// dmids 取弹幕主键序列（逐字段断言之外的「到底返回了哪几行」）。
func dmids(rows []*model.Danmaku) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Dmid)
	}
	return out
}

func rpcDmids(rows []*rpc.DanmakuInfo) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetDmid())
	}
	return out
}
