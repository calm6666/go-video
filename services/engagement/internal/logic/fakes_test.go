package logic

// fakes_test.go 是 engagement logic 测试的内存替身集合。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, 假事务连接, 6 个内存 model) 组装**真实的 Repository**，
// 只把它的 8 个依赖（缓存、事务连接、6 个 model）换成替身。这样「幂等判定、计数增减方向、
// 缓存读穿/回填、软删兜底、事件装配与事务边界」整条链路都在被测路径上，而不是把 Repository 整个 mock 掉。
// 见 internal/repository 的 Cacher 注释。engagement 的 6 个 model 本来就是接口
// （model 包已定义），所以本文件不需要新增生产接口。
//
// 四条替身纪律（catalog / rights / playback 三轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 里的写回不得污染库存行，否则
//     「计数有没有真的落库」这类断言会被共享指针掩盖。
//  2. 副作用按**顺序**记录（callLog），断言序列而不是只数次数：engagement 最要紧的结论是
//     「重复点赞有没有再走 Upsert / 再加一次计数」，只数次数会漏掉次序与跳过分支。
//  3. 错误注入按方法粒度（faultInjector.failWith("FindStates", err)），不用一个全局 err。
//  4. 布数据走替身的**静默写入路径**（seedLike / seedStat / seedFavItem / seedFolder /
//     seedShare / warmFolders / warmIsFavored → 直接写 map，不记轨迹），
//     所以所有序列断言都从 0 开始数。被测代码走的才是公开口径（Upsert/Add/…）。
//
// 唯一键口径（对着 deploy/migrations/engagement/*.sql 复刻，不是凭空约定）：
//   - thumbup_like：UNIQUE (business, mid, message_id) → fakeLikeModel 按该三元组寻址；
//   - thumbup_stat：UNIQUE (business, origin_id, message_id) → fakeStatModel 同上；
//   - favorite_item：UNIQUE (mid, oid, tp) → fakeFavItemModel 同上（fid 不在唯一键里，
//     所以「换收藏夹」是 UPDATE fid 而不是插第二行）；
//   - share_log：UNIQUE (oid, mid, tp, day) → fakeShareModel 同上（**按天**幂等）。
//
// 覆盖边界（如实声明，别把替身当 SQL 用）：
//   - 替身只复刻 model 层 SQL 的**语义**（ON DUPLICATE KEY UPDATE 的字段子集、INSERT IGNORE、
//     UPDATE 未命中 0 行、state=1 过滤、LIMIT/OFFSET 切片、ORDER BY），不证明 SQL 与列名本身；
//   - 因此**发现不了 schema↔model 的列名漂移**：本包落地时 `share_log` 迁移里的列名是
//     `tp`，而 model/share_log.go 写的 SQL 是 `type`（`AddShare` 在真实 schema 下必然报错），
//     替身两边都跟着 model 走所以全绿 —— 该漂移已于 2026-09-23 改 model 侧为 `tp` 修掉，
//     登记在 README 已知缺口 1；机器门禁（model↔DDL 逐列比对）见 model 包的说明。
//   - 事务由 fakeConn 提供，且**真的回滚**：TransactCtx 报错时按各替身的快照还原行集，
//     与 InnoDB 语义一致（缺陷 #1 修好后，「Upsert 落地而 Incr 失败」不再留下半截状态）。
//     因此失败用例同时断言两件事：调用轨迹记到哪儿（事务体执行过哪些语句）与库里剩什么（回滚后
//     什么都没多）。注意轨迹本身不回滚，所以它比库更啰嗦，这是有意的；
//   - 会话不是真会话：假事务只保证「同一个 txSession 对象」，不保证隔离级别与可见性，
//     所以「Incr 之后必须用同一会话回读计数」这件事只能靠调用轨迹里的 #tN 后缀断言
//     （见 sessTag），不能靠替身的可见性；
//   - Redis key 前缀（eng:）留在未导出的 *Cache 里，Cacher 接口按结构化入参划分，
//     本文件里的 keyXxx 只是替身的内部寻址串，不用于断言真实 Redis 键空间；
//   - 墙钟：model.NowUnix()/time.Now() 不可注入，因此布景一律给显式 ctime/mtime，
//     涉及 model 内部取 now 的字段只做「非零」级断言（见 isRecentUnix）。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/engagement/internal/config"
	"go-video/services/engagement/internal/repository"
	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
)

// --- 断言小工具（本包共享） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵（model/repository 用 %w 包装下游错误是允许的）。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %v", label, want)
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

// wantFail 断言「错误按哨兵透出，且响应体为 nil」——
// 出错时还带回半截 reply 是最容易被客户端当成功的形态，守卫和失败传播用例都锁这条。
// 参数 got 用 any 包一层，才能区分 (*rpc.X)(nil)（合法）与真有值（非法）。
func wantFail[R any](t *testing.T, label string, got R, err, sentinel error) {
	t.Helper()
	wantErrIs(t, label, err, sentinel)
	v := reflect.ValueOf(any(got))
	if v.Kind() == reflect.Ptr && !v.IsNil() {
		t.Errorf("%s：出错时响应体 = %#v, want nil", label, got)
	}
}

// wantStringsEQ 比较字符串切片（wantEQ 受 comparable 约束，切片只能另走这条）。
func wantStringsEQ(t *testing.T, label, field string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
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

// wantOpsUnordered 用于 map 遍历顺序本身不确定的用例（MultiStats 按业务分组）。
// 只放宽**顺序**，不放宽**集合与重数**：少一次调用、多一次调用、调错对象都会红。
func wantOpsUnordered(t *testing.T, label string, got, want []string) {
	t.Helper()
	g, w := slices.Clone(got), slices.Clone(want)
	sort.Strings(g)
	sort.Strings(w)
	if !slices.Equal(g, w) {
		t.Errorf("%s：调用集合 = %v, want %v（原序列 [%s]）", label, g, w, strings.Join(got, " → "))
	}
}

// wantCount 断言某类调用发生的次数（配合 wantOps 做「这类写操作一次都不许发生」）。
// 布数据走静默路径（见纪律 4），所以这里数的一定是被测代码干的。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整序列 [%s]）", label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

// itoa 拼调用轨迹里的整数段：轨迹由替身用 %d 格式化，用例期望值必须同源，
// 否则 strconv 与 fmt 的差别会变成一条永远对不上的断言。
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// joinIDs 拼 IN (?) 的 id 列表段，顺序与入参一致（替身不改序）。
func joinIDs(ids []int64) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, itoa(id))
	}
	return strings.Join(out, "|")
}

// isRecentUnix 断言某个由 model 内部 time.Now() 落下的时间戳落在用例运行窗口内。
// 只用于无法注入墙钟的字段（favorite_item.ctime、thumbup_stat.ctime）；
// 能显式布景的时间戳一律用 wantEQ 精确比较。
func isRecentUnix(t *testing.T, label, field string, got int64) {
	t.Helper()
	now := time.Now().Unix()
	if got < now-120 || got > now+5 {
		t.Errorf("%s：%s = %d, want 接近当前时间 %d（±120s）", label, field, got, now)
	}
}

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

// countPrefix 统计以 prefix 开头的调用数。
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

// --- 错误注入 ---

type faultInjector struct{ by map[string]error }

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

func (f *faultInjector) fail(method string) error { return f.by[method] }

// --- 事务会话 ---

// txSession 是 fakeConn 发的那张「事务会话」凭证。tag 形如 t1，会被 sessTag 拼进调用轨迹，
// 用例据此断言某一步确实走的是本次事务的会话，而不是只断言「非空」。
type txSession struct {
	sqlx.Session
	tag string
}

// sessTag 把会话翻译成轨迹后缀：nil 读路径无后缀，事务内会话带 #t<序号>。
// 为什么值得为它改所有写路径的期望串：Like 的「Incr 之后回读计数」一旦退回自动提交连接，
// 读到的就是事务前的旧值，事件快照会少算这次互动，而调用序列（顺序）完全一样、抓不出来。
func sessTag(tx sqlx.Session) string {
	if tx == nil {
		return ""
	}
	s, ok := tx.(*txSession)
	if !ok {
		return "#not-a-fake-session"
	}
	return "#" + s.tag
}

// requireTx 给「只应出现在事务体内」的写方法用：传进 nil 就是生产代码回归，
// 直接报错让用例红，而不是默默用自动提交连接把写做完（那正是缺陷 #1 的原形）。
func requireTx(tx sqlx.Session, what string) error {
	if tx == nil {
		return fmt.Errorf("fake: %s 必须在事务会话内调用（AGENTS.md §5 的 Outbox 口径）", what)
	}
	if _, ok := tx.(*txSession); !ok {
		return fmt.Errorf("fake: %s 拿到的会话不是 fakeConn 发的会话：%T", what, tx)
	}
	return nil
}

// undo 是一个还原闭包：把某个替身的行集恢复到事务开始前的样子。
type undo func()

// fakeConn 只实现 engagement 用到的 TransactCtx。其它方法落在内嵌的 nil 接口上会直接 panic，
// 等价于「logic 单测路径上不允许出现任何绕过 model 的直连 SQL」，真出现了就是越界。
//
// 事务体返回错误时逐个执行快照还原，语义与 InnoDB 回滚一致：
// 这样「计数增量失败后关系行不许留下」这类断言才在替身上成立（缺陷 #1 的修法）。
// 调用轨迹不回滚：它记录「执行到哪儿」，与库里剩什么是两个独立的证据。
type fakeConn struct {
	sqlx.SqlConn
	log          *callLog
	txSeq        int
	transactions int
	rolledBack   int
	// snapshots 由 newStore 装配：每个替身提供一个「拍下现在，返回还原闭包」的函数。
	snapshots []func() undo
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

func (f *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	f.transactions++
	f.txSeq++
	tag := "t" + strconv.Itoa(f.txSeq)
	f.log.add("db.TransactCtx:%s", tag)
	undos := make([]undo, 0, len(f.snapshots))
	for _, snap := range f.snapshots {
		undos = append(undos, snap())
	}
	if err := fn(ctx, &txSession{tag: tag}); err != nil {
		f.rolledBack++
		for _, u := range undos {
			u()
		}
		return err
	}
	return nil
}

// txCount 断言事务次数：一次互动应当且仅当一个事务。
func (f *fakeConn) txCount() int { return f.transactions }

// --- 缓存替身 ---

// 替身内部寻址串。真实键前缀在 repository 的未导出常量里，接口不传键，所以这里
// 只为 fake 自己的 map 寻址，不与生产 Redis 对齐（见文件头覆盖边界）。
func addrLikeCnt(business string, originID, messageID int64) string {
	return fmt.Sprintf("lc:%s:%d:%d", business, originID, messageID)
}
func addrDislikeCnt(business string, originID, messageID int64) string {
	return fmt.Sprintf("dc:%s:%d:%d", business, originID, messageID)
}
func addrFolders(mid int64) string { return fmt.Sprintf("fl:%d", mid) }
func addrIsFavored(mid, oid int64, tp int32) string {
	return fmt.Sprintf("fav:%d:%d:%d", mid, oid, tp)
}

// fakeCache 实现 repository.Cacher。
// miss 口径与真实 Cache 一致：GetFolders 返回 ("", nil) 表示 miss，
// GetIsFavored 返回 (false, false, nil) 表示 miss。
// 计数器初值是 0，且**不**做「计数器 = DB 值 + Redis 增量」的合成 ——
// 因为真实 Repository 也没有做（它只是把 Redis 当影子计数器），用例断言的是
// 「有没有被调、delta 是多少」，不依赖这里的具体累计值。
type fakeCache struct {
	faultInjector
	log     *callLog
	likeCnt map[string]int64
	disCnt  map[string]int64
	folders map[string]string // mid → 收藏夹列表 JSON 载荷
	isFav   map[string]string // "1"/"0"
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{
		log:     log,
		likeCnt: map[string]int64{},
		disCnt:  map[string]int64{},
		folders: map[string]string{},
		isFav:   map[string]string{},
	}
}

func (f *fakeCache) Ping(context.Context) error { f.log.add("cache.Ping"); return nil }

func (f *fakeCache) IncrLikeCount(_ context.Context, business string, originID, messageID, delta int64) error {
	key := addrLikeCnt(business, originID, messageID)
	f.log.add("cache.IncrLike:%s:%d", key, delta)
	if err := f.fail("IncrLikeCount"); err != nil {
		return err
	}
	f.likeCnt[key] += delta
	return nil
}

func (f *fakeCache) IncrDislikeCount(_ context.Context, business string, originID, messageID, delta int64) error {
	key := addrDislikeCnt(business, originID, messageID)
	f.log.add("cache.IncrDislike:%s:%d", key, delta)
	if err := f.fail("IncrDislikeCount"); err != nil {
		return err
	}
	f.disCnt[key] += delta
	return nil
}

func (f *fakeCache) GetFolders(_ context.Context, mid int64) (string, error) {
	key := addrFolders(mid)
	f.log.add("cache.GetFolders:%d", mid)
	if err := f.fail("GetFolders"); err != nil {
		return "", err
	}
	return f.folders[key], nil
}

func (f *fakeCache) SetFolders(_ context.Context, mid int64, payload string) error {
	f.log.add("cache.SetFolders:%d", mid)
	if err := f.fail("SetFolders"); err != nil {
		return err
	}
	f.folders[addrFolders(mid)] = payload
	return nil
}

func (f *fakeCache) DelFolders(_ context.Context, mid int64) error {
	f.log.add("cache.DelFolders:%d", mid)
	if err := f.fail("DelFolders"); err != nil {
		return err
	}
	delete(f.folders, addrFolders(mid))
	return nil
}

func (f *fakeCache) GetIsFavored(_ context.Context, mid, oid int64, tp int32) (bool, bool, error) {
	key := addrIsFavored(mid, oid, tp)
	f.log.add("cache.GetIsFavored:%d:%d:%d", mid, oid, tp)
	if err := f.fail("GetIsFavored"); err != nil {
		return false, false, err
	}
	v, ok := f.isFav[key]
	if !ok {
		return false, false, nil
	}
	return v == "1", true, nil
}

func (f *fakeCache) SetIsFavored(_ context.Context, mid, oid int64, tp int32, faved bool) error {
	f.log.add("cache.SetIsFavored:%d:%d:%d:%d", mid, oid, tp, b2i(faved))
	if err := f.fail("SetIsFavored"); err != nil {
		return err
	}
	v := "0"
	if faved {
		v = "1"
	}
	f.isFav[addrIsFavored(mid, oid, tp)] = v
	return nil
}

func (f *fakeCache) DelIsFavored(_ context.Context, mid, oid int64, tp int32) error {
	f.log.add("cache.DelIsFavored:%d:%d:%d", mid, oid, tp)
	if err := f.fail("DelIsFavored"); err != nil {
		return err
	}
	delete(f.isFav, addrIsFavored(mid, oid, tp))
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// foldersPayload 读缓存里实际存的载荷（断言「回填的是哪个 JSON」）。
func (f *fakeCache) foldersPayload(mid int64) string { return f.folders[addrFolders(mid)] }

// isFavoredCached 读缓存里的收藏标记；hit=false 表示没有该 key。
func (f *fakeCache) isFavoredCached(mid, oid int64, tp int32) (bool, bool) {
	v, ok := f.isFav[addrIsFavored(mid, oid, tp)]
	if !ok {
		return false, false
	}
	return v == "1", true
}

// warmFolders 静默预热收藏夹列表缓存（不记轨迹，纪律 4）。
func (f *fakeCache) warmFolders(mid int64, payload string) {
	f.folders[addrFolders(mid)] = payload
}

// warmIsFavored 静默预热收藏标记（不记轨迹）。
func (f *fakeCache) warmIsFavored(mid, oid int64, tp int32, faved bool) {
	v := "0"
	if faved {
		v = "1"
	}
	f.isFav[addrIsFavored(mid, oid, tp)] = v
}

func (f *fakeCache) likeCounter(business string, originID, messageID int64) int64 {
	return f.likeCnt[addrLikeCnt(business, originID, messageID)]
}

func (f *fakeCache) dislikeCounter(business string, originID, messageID int64) int64 {
	return f.disCnt[addrDislikeCnt(business, originID, messageID)]
}

// --- thumbup_like 替身（UNIQUE (business, mid, message_id)） ---

type likeKey struct {
	business  string
	mid       int64
	messageID int64
}

type fakeLikeModel struct {
	faultInjector
	log  *callLog
	rows map[likeKey]*model.ThumbupLike
	next int64
}

func newFakeLikeModel(log *callLog) *fakeLikeModel {
	return &fakeLikeModel{log: log, rows: map[likeKey]*model.ThumbupLike{}}
}

// Upsert 复刻 INSERT ... ON DUPLICATE KEY UPDATE state = VALUES(state), mtime = VALUES(mtime)：
// 冲突时只有 state / mtime 被覆盖，ctime、up_mid、origin_id 保持原行（这正是
// 「换收藏夹式更新」在点赞表上的对应物）。真实 model 恒返回 oldState=0（它不查旧状态，
// 由 Repository 先 FindStates 判定），这里同样返回 0。
func (f *fakeLikeModel) Upsert(_ context.Context, tx sqlx.Session, l *model.ThumbupLike) (int32, error) {
	f.log.add("like.Upsert:%s:%d:%d%s", l.Business, l.Mid, l.MessageID, sessTag(tx))
	if err := requireTx(tx, "like.Upsert"); err != nil {
		return 0, err
	}
	if err := f.fail("Upsert"); err != nil {
		return 0, err
	}
	k := likeKey{l.Business, l.Mid, l.MessageID}
	row, ok := f.rows[k]
	if !ok {
		f.next++
		cp := *l
		cp.ID = f.next
		f.rows[k] = &cp
		return 0, nil
	}
	row.State = l.State
	row.Mtime = l.Mtime
	return 0, nil
}

// FindStates 复刻 WHERE business=? AND mid=? AND message_id IN (?)。
// 注意：**没有 state 过滤**——已取消（state=0）的历史行照样返回，
// 所以 HasLike 会把「取消过」如实报成 STATE_UNSPECIFIED，而不是当作「没点过」。
// 双用途方法：Like 的事务内首读带 #tN，HasLike/Stats 的纯读路径不带后缀。
func (f *fakeLikeModel) FindStates(_ context.Context, tx sqlx.Session, business string, mid int64, messageIDs []int64) (map[int64]*model.ThumbupLike, error) {
	f.log.add("like.FindStates:%s:%d:%s%s", business, mid, joinIDs(messageIDs), sessTag(tx))
	if err := f.fail("FindStates"); err != nil {
		return nil, err
	}
	if len(messageIDs) == 0 {
		return map[int64]*model.ThumbupLike{}, nil
	}
	out := make(map[int64]*model.ThumbupLike)
	for _, id := range messageIDs {
		if row, ok := f.rows[likeKey{business, mid, id}]; ok {
			cp := *row
			out[id] = &cp
		}
	}
	return out, nil
}

// ListByMid 复刻「先 COUNT(*) WHERE state=1，total=0 直接返回空；
// 再 ORDER BY ctime DESC LIMIT ps OFFSET (pn-1)*ps」。ps<1||ps>50 钳到 20 也照搬。
// ctime 相同行 MySQL 排序不确定，因此布景必须给不同 ctime。
func (f *fakeLikeModel) ListByMid(_ context.Context, business string, mid int64, pn, ps int32) ([]*model.ThumbupLike, int32, error) {
	f.log.add("like.ListByMid:%s:%d:%d/%d", business, mid, pn, ps)
	if err := f.fail("ListByMid"); err != nil {
		return nil, 0, err
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	matched := f.matchSorted(func(r *model.ThumbupLike) bool {
		return r.Business == business && r.Mid == mid && r.State == model.LikeStateLike
	}, func(a, b *model.ThumbupLike) bool { return a.Ctime > b.Ctime })
	total := int32(len(matched))
	if total == 0 {
		return nil, 0, nil
	}
	return pageOf(matched, pn, ps), total, nil
}

// ListByItem 复刻 COUNT(*)（**不带 last_mid 条件**，所以 total 是「全部点赞人数」，
// 不是「剩余可翻页数」）+ WHERE state=1 AND mid > last_mid ORDER BY mid ASC。
func (f *fakeLikeModel) ListByItem(_ context.Context, business string, originID, messageID, lastMid int64, pn, ps int32) ([]*model.ThumbupLike, int32, error) {
	f.log.add("like.ListByItem:%s:%d:%d:%d:%d/%d", business, originID, messageID, lastMid, pn, ps)
	if err := f.fail("ListByItem"); err != nil {
		return nil, 0, err
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	total := int32(len(f.matchSorted(func(r *model.ThumbupLike) bool {
		return r.Business == business && r.OriginID == originID && r.MessageID == messageID && r.State == model.LikeStateLike
	}, func(a, b *model.ThumbupLike) bool { return a.Mid < b.Mid })))
	if total == 0 {
		return nil, 0, nil
	}
	filtered := f.matchSorted(func(r *model.ThumbupLike) bool {
		return r.Business == business && r.OriginID == originID && r.MessageID == messageID &&
			r.State == model.LikeStateLike && r.Mid > lastMid
	}, func(a, b *model.ThumbupLike) bool { return a.Mid < b.Mid })
	return pageOf(filtered, pn, ps), total, nil
}

// matchSorted 按条件取行并按 less 排序，返回值拷贝。
func (f *fakeLikeModel) matchSorted(pred func(*model.ThumbupLike) bool, less func(a, b *model.ThumbupLike) bool) []*model.ThumbupLike {
	var hits []*model.ThumbupLike
	for _, row := range f.rows {
		if pred(row) {
			cp := *row
			hits = append(hits, &cp)
		}
	}
	slices.SortFunc(hits, func(a, b *model.ThumbupLike) int {
		switch {
		case less(a, b):
			return -1
		case less(b, a):
			return 1
		default:
			return 0
		}
	})
	return hits
}

// get 读库内一行（值拷贝）。
func (f *fakeLikeModel) get(business string, mid, messageID int64) *model.ThumbupLike {
	row, ok := f.rows[likeKey{business, mid, messageID}]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// rowsOf 返回该 business 下全部行（值拷贝，按 mid+messageID 定序，便于断言行数）。
func (f *fakeLikeModel) rowsOf(business string) []*model.ThumbupLike {
	var out []*model.ThumbupLike
	for _, row := range f.rows {
		if row.Business == business {
			cp := *row
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *model.ThumbupLike) int {
		if a.Mid != b.Mid {
			return int(a.Mid - b.Mid)
		}
		return int(a.MessageID - b.MessageID)
	})
	return out
}

// snapshot 拍下当前行集与自增值，返回还原闭包（fakeConn 的回滚用它）。
// 必须连行结构体一起拷：替身是原地改字段（row.State = 某值），只换 map 等于没还原。
func (f *fakeLikeModel) snapshot() undo {
	rows := make(map[likeKey]*model.ThumbupLike, len(f.rows))
	for k, v := range f.rows {
		cp := *v
		rows[k] = &cp
	}
	next := f.next
	return func() {
		f.rows = rows
		f.next = next
	}
}

// --- thumbup_stat 替身（UNIQUE (business, origin_id, message_id)） ---

type statKey struct {
	business  string
	originID  int64
	messageID int64
}

type fakeStatModel struct {
	faultInjector
	log  *callLog
	rows map[statKey]*model.ThumbupStat
	next int64
}

func newFakeStatModel(log *callLog) *fakeStatModel {
	return &fakeStatModel{log: log, rows: map[statKey]*model.ThumbupStat{}}
}

// FindOne 双用途：Like 的事务内回读带 #tN，RawStat 的纯读路径不带后缀。
func (f *fakeStatModel) FindOne(_ context.Context, tx sqlx.Session, business string, originID, messageID int64) (*model.ThumbupStat, error) {
	f.log.add("stat.FindOne:%s:%d:%d%s", business, originID, messageID, sessTag(tx))
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[statKey{business, originID, messageID}]
	if !ok {
		return nil, nil // 与 model 一致：查无此行返回 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (f *fakeStatModel) FindMany(_ context.Context, business string, originID int64, messageIDs []int64) (map[int64]*model.ThumbupStat, error) {
	f.log.add("stat.FindMany:%s:%d:%s", business, originID, joinIDs(messageIDs))
	if err := f.fail("FindMany"); err != nil {
		return nil, err
	}
	if len(messageIDs) == 0 {
		return map[int64]*model.ThumbupStat{}, nil
	}
	out := make(map[int64]*model.ThumbupStat)
	for _, id := range messageIDs {
		if row, ok := f.rows[statKey{business, originID, id}]; ok {
			cp := *row
			out[id] = &cp
		}
	}
	return out, nil
}

// Incr 复刻 INSERT ... ON DUPLICATE KEY UPDATE
// like_number = like_number + VALUES(like_number), dislike_number 同理, mtime = VALUES(mtime)。
// 首次插入时 like_number 直接等于 delta —— 所以 delta 为负且行不存在会得到**负计数**
// （model 的 SQL 没有 GREATEST(0, …) 兜底，这是本服务的真实行为，见 likelogic 用例）。
func (f *fakeStatModel) Incr(_ context.Context, tx sqlx.Session, business string, originID, messageID, likeDelta, dislikeDelta int64) error {
	f.log.add("stat.Incr:%s:%d:%d:%d/%d%s", business, originID, messageID, likeDelta, dislikeDelta, sessTag(tx))
	if err := requireTx(tx, "stat.Incr"); err != nil {
		return err
	}
	if err := f.fail("Incr"); err != nil {
		return err
	}
	now := time.Now().Unix()
	k := statKey{business, originID, messageID}
	row, ok := f.rows[k]
	if !ok {
		f.next++
		f.rows[k] = &model.ThumbupStat{
			ID: f.next, Business: business, OriginID: originID, MessageID: messageID,
			LikeNumber: likeDelta, DislikeNumber: dislikeDelta,
			LikeChange: 0, DislikeChange: 0, Ctime: now, Mtime: now,
		}
		return nil
	}
	row.LikeNumber += likeDelta
	row.DislikeNumber += dislikeDelta
	row.Mtime = now
	return nil
}

// UpdateChange 复刻 UPDATE ... SET like_change = like_change + ?, dislike_change = dislike_change + ?
// ——**只有 UPDATE，没有 INSERT**：行不存在时受影响 0 行且不报错（model 忽略了 RowsAffected），
// 所以运营修正会静默丢失。见 updatecountlogic 用例登记的缺陷 #3。
// 真实 SQL 也不更新 mtime，这里同样不更新。
func (f *fakeStatModel) UpdateChange(_ context.Context, business string, originID, messageID, likeChange, dislikeChange int64) error {
	f.log.add("stat.UpdateChange:%s:%d:%d:%d/%d", business, originID, messageID, likeChange, dislikeChange)
	if err := f.fail("UpdateChange"); err != nil {
		return err
	}
	if row, ok := f.rows[statKey{business, originID, messageID}]; ok {
		row.LikeChange += likeChange
		row.DislikeChange += dislikeChange
	}
	return nil
}

func (f *fakeStatModel) get(business string, originID, messageID int64) *model.ThumbupStat {
	row, ok := f.rows[statKey{business, originID, messageID}]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeStatModel) rowCount() int { return len(f.rows) }

// snapshot 拍下当前行集与自增值，返回还原闭包（见 fakeLikeModel.snapshot 的拷贝口径）。
func (f *fakeStatModel) snapshot() undo {
	rows := make(map[statKey]*model.ThumbupStat, len(f.rows))
	for k, v := range f.rows {
		cp := *v
		rows[k] = &cp
	}
	next := f.next
	return func() {
		f.rows = rows
		f.next = next
	}
}

// --- favorite_item 替身（UNIQUE (mid, oid, tp)，fid 不在唯一键内） ---

type favKey struct {
	mid int64
	oid int64
	tp  int32
}

type fakeFavItemModel struct {
	faultInjector
	log       *callLog
	rows      map[favKey]*model.FavoriteItem
	next      int64
	fallbacks int // Del 走了「不带 fid 的兜底 UPDATE」的次数
}

func newFakeFavItemModel(log *callLog) *fakeFavItemModel {
	return &fakeFavItemModel{log: log, rows: map[favKey]*model.FavoriteItem{}}
}

// Add 复刻 INSERT ... ON DUPLICATE KEY UPDATE state = 0, fid = VALUES(fid), mtime = VALUES(mtime)。
// ctime 与 otype 用 model 内部 now 和插入时的 otype，冲突时 otype 不会被更新（真实 SQL 就没带它）。
func (f *fakeFavItemModel) Add(_ context.Context, tx sqlx.Session, item *model.FavoriteItem) error {
	f.log.add("favItem.Add:%d:%d:%d:%d%s", item.Mid, item.Oid, item.Tp, item.Fid, sessTag(tx))
	if err := requireTx(tx, "favItem.Add"); err != nil {
		return err
	}
	if err := f.fail("Add"); err != nil {
		return err
	}
	now := time.Now().Unix()
	k := favKey{item.Mid, item.Oid, item.Tp}
	row, ok := f.rows[k]
	if !ok {
		f.next++
		cp := *item
		cp.ID = f.next
		cp.State = 0
		cp.Ctime = now
		cp.Mtime = now
		f.rows[k] = &cp
		return nil
	}
	row.State = 0
	row.Fid = item.Fid
	row.Mtime = now
	return nil
}

// Del 复刻两段式软删：先按 (mid,oid,tp,fid) 且 state=0 更新，受影响 0 行时
// **再发一次不带 fid 的 UPDATE**（兼容默认夹），两次都 0 行也返回 nil（幂等取消收藏）。
// 由于本替身实现的是 model 接口，兜底那条 SQL 不会单独进 callLog；
// 「有没有走兜底」通过 fallbacks 计数断言。
func (f *fakeFavItemModel) Del(_ context.Context, tx sqlx.Session, mid, oid int64, tp int32, fid int64) error {
	f.log.add("favItem.Del:%d:%d:%d:%d%s", mid, oid, tp, fid, sessTag(tx))
	if err := requireTx(tx, "favItem.Del"); err != nil {
		return err
	}
	if err := f.fail("Del"); err != nil {
		return err
	}
	now := time.Now().Unix()
	k := favKey{mid, oid, tp}
	row, ok := f.rows[k]
	if ok && row.Fid == fid && row.State == 0 {
		row.State = 1
		row.Mtime = now
		return nil
	}
	f.fallbacks++
	if ok && row.State == 0 {
		row.State = 1
		row.Mtime = now
	}
	return nil
}

// IsFavored 复刻 SELECT state FROM favorite_item WHERE mid,oid,tp：无行 → false,nil；
// 有行且 state=0 → true。
// 双用途方法：AddFav/DelFav 的事务内前置判定带 #tN（漏了它就是缺陷 #1 的复现），
// Repository.IsFavored 的纯读路径不带后缀。
func (f *fakeFavItemModel) IsFavored(_ context.Context, tx sqlx.Session, mid, oid int64, tp int32) (bool, error) {
	f.log.add("favItem.IsFavored:%d:%d:%d%s", mid, oid, tp, sessTag(tx))
	if err := f.fail("IsFavored"); err != nil {
		return false, err
	}
	row, ok := f.rows[favKey{mid, oid, tp}]
	if !ok {
		return false, nil
	}
	return row.State == 0, nil
}

// CountByOid 复刻 SELECT COUNT(*) ... WHERE oid=? AND tp=? AND state = 0：
// 软删行留在表里（Del 只置位），不过滤会把已取消的收藏算进事件快照。
// 只应在事务内调用（它给事件提供收藏数绝对快照），因此要求会话。
func (f *fakeFavItemModel) CountByOid(_ context.Context, tx sqlx.Session, oid int64, tp int32) (int64, error) {
	f.log.add("favItem.CountByOid:%d:%d%s", oid, tp, sessTag(tx))
	if err := requireTx(tx, "favItem.CountByOid"); err != nil {
		return 0, err
	}
	if err := f.fail("CountByOid"); err != nil {
		return 0, err
	}
	var n int64
	for _, row := range f.rows {
		if row.Oid == oid && row.Tp == tp && row.State == 0 {
			n++
		}
	}
	return n, nil
}

// IsFavoreds 复刻 WHERE mid=? AND tp=? AND oid IN (?)：查不到的 oid 不出现在结果 map 里。
func (f *fakeFavItemModel) IsFavoreds(_ context.Context, mid int64, oids []int64, tp int32) (map[int64]bool, error) {
	f.log.add("favItem.IsFavoreds:%d:%d:%s", mid, tp, joinIDs(oids))
	if err := f.fail("IsFavoreds"); err != nil {
		return nil, err
	}
	if len(oids) == 0 {
		return map[int64]bool{}, nil
	}
	out := make(map[int64]bool)
	for _, oid := range oids {
		if row, ok := f.rows[favKey{mid, oid, tp}]; ok {
			out[oid] = row.State == 0
		}
	}
	return out, nil
}

func (f *fakeFavItemModel) get(mid, oid int64, tp int32) *model.FavoriteItem {
	row, ok := f.rows[favKey{mid, oid, tp}]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeFavItemModel) rowCount() int { return len(f.rows) }

// snapshot 拍下当前行集、自增值与兜底次数，返回还原闭包。
// fallbacks 也要还原：它记录「Del 走了不带 fid 的第二条 UPDATE」，
// 回滚后若留着，失败用例就会误判成「兜底语句真的执行并落库了」。
func (f *fakeFavItemModel) snapshot() undo {
	rows := make(map[favKey]*model.FavoriteItem, len(f.rows))
	for k, v := range f.rows {
		cp := *v
		rows[k] = &cp
	}
	next := f.next
	fallbacks := f.fallbacks
	return func() {
		f.rows = rows
		f.next = next
		f.fallbacks = fallbacks
	}
}

// --- favorite_folder 替身（PRIMARY KEY (fid)，state 软删） ---

type fakeFolderModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.FavoriteFolder
	next int64
}

func newFakeFolderModel(log *callLog) *fakeFolderModel {
	return &fakeFolderModel{log: log, rows: map[int64]*model.FavoriteFolder{}}
}

// Add 复刻 INSERT INTO favorite_folder (mid,name,description,cover,public,state,ctime,mtime,count)
// VALUES (?,?,?,?,?,0,?,?,0) —— SQL 字面量把 state 和 count 钉成 0，入参给什么都不算，
// 所以替身也无条件写 0（这条断言能抓住「以后有人把 state 改成透传」）。
func (f *fakeFolderModel) Add(_ context.Context, fld *model.FavoriteFolder) (int64, error) {
	f.log.add("folder.Add:%d:%s", fld.Mid, fld.Name)
	if err := f.fail("Add"); err != nil {
		return 0, err
	}
	f.next++
	cp := *fld
	cp.Fid = f.next
	cp.State = model.FolderStateNormal
	cp.Count = 0
	f.rows[cp.Fid] = &cp
	return cp.Fid, nil
}

// Del 复刻 UPDATE ... SET state=1, mtime=? WHERE fid=? AND mid=?：
// WHERE **不含 state=0**，所以重复删除同一夹仍算命中；夹子不存在或不属于该 mid
// 时受影响 0 行 → ErrFolderNotFoundOrForbidden。
func (f *fakeFolderModel) Del(_ context.Context, fid, mid int64) error {
	f.log.add("folder.Del:%d:%d", fid, mid)
	if err := f.fail("Del"); err != nil {
		return err
	}
	row, ok := f.rows[fid]
	if !ok || row.Mid != mid {
		return model.ErrFolderNotFoundOrForbidden
	}
	row.State = model.FolderStateDeleted
	row.Mtime = time.Now().Unix()
	return nil
}

// ListByUser 复刻 WHERE mid = vmid AND state = 0（+ mid != vmid 时 public = 1）
// ORDER BY fid ASC。注意过滤用的是 vmid：mid 只用来决定要不要加公开条件。
func (f *fakeFolderModel) ListByUser(_ context.Context, mid, vmid int64) ([]*model.FavoriteFolder, error) {
	f.log.add("folder.ListByUser:%d:%d", mid, vmid)
	if err := f.fail("ListByUser"); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []*model.FavoriteFolder
	for _, id := range ids {
		row := f.rows[id]
		if row.Mid != vmid || row.State != model.FolderStateNormal {
			continue
		}
		if mid != vmid && row.Public != 1 {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	return out, nil
}

// FindOne 目前不被 Repository 使用（见 README 已知缺口），实现它是为了满足 model 接口。
func (f *fakeFolderModel) FindOne(_ context.Context, fid int64) (*model.FavoriteFolder, error) {
	f.log.add("folder.FindOne:%d", fid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[fid]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeFolderModel) get(fid int64) *model.FavoriteFolder {
	row, ok := f.rows[fid]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// --- share_log 替身（UNIQUE (oid, mid, tp, day)，列名与 model 一致为 tp） ---

type shareKey struct {
	oid int64
	mid int64
	tp  int32
	day int32
}

type fakeShareModel struct {
	faultInjector
	log     *callLog
	rows    map[shareKey]*model.ShareLog
	next    int64
	dayLock int32 // 非 0 时钉住「今天」，用于确定性复现按天幂等；0 表示走真实日期
}

func newFakeShareModel(log *callLog) *fakeShareModel {
	return &fakeShareModel{log: log, rows: map[shareKey]*model.ShareLog{}}
}

// today 复刻 model.currentDayYyyymmdd()（未导出且不可注入，故替身同式）。
func (f *fakeShareModel) today() int32 {
	if f.dayLock != 0 {
		return f.dayLock
	}
	now := time.Now()
	return int32(now.Year()*10000 + int(now.Month())*100 + now.Day())
}

// AddIfNotExists 复刻 INSERT IGNORE：命中唯一键时受影响 0 行、不报错，added=false。
func (f *fakeShareModel) AddIfNotExists(_ context.Context, tx sqlx.Session, oid, mid int64, tp int32) (bool, error) {
	f.log.add("share.AddIfNotExists:%d:%d:%d%s", oid, mid, tp, sessTag(tx))
	if err := requireTx(tx, "share.AddIfNotExists"); err != nil {
		return false, err
	}
	if err := f.fail("AddIfNotExists"); err != nil {
		return false, err
	}
	k := shareKey{oid: oid, mid: mid, tp: tp, day: f.today()}
	if _, ok := f.rows[k]; ok {
		return false, nil
	}
	f.next++
	f.rows[k] = &model.ShareLog{ID: f.next, Oid: oid, Mid: mid, Type: tp, Day: k.day, Ctime: time.Now().Unix()}
	return true, nil
}

// CountByOid 复刻 SELECT COUNT(*) FROM share_log WHERE oid=? AND tp=?
// —— 按天幂等只保证「同人同物同天一条」，跨天会各计一条，所以 count 是累计值。
// 必须与 AddIfNotExists 同会话，否则事件里的 share_count 读不到刚插入的这行。
func (f *fakeShareModel) CountByOid(_ context.Context, tx sqlx.Session, oid int64, tp int32) (int64, error) {
	f.log.add("share.CountByOid:%d:%d%s", oid, tp, sessTag(tx))
	if err := requireTx(tx, "share.CountByOid"); err != nil {
		return 0, err
	}
	if err := f.fail("CountByOid"); err != nil {
		return 0, err
	}
	var n int64
	for _, row := range f.rows {
		if row.Oid == oid && row.Type == tp {
			n++
		}
	}
	return n, nil
}

func (f *fakeShareModel) rowCount() int { return len(f.rows) }

// snapshot 拍下当前行集与自增值，返回还原闭包。
func (f *fakeShareModel) snapshot() undo {
	rows := make(map[shareKey]*model.ShareLog, len(f.rows))
	for k, v := range f.rows {
		cp := *v
		rows[k] = &cp
	}
	next := f.next
	return func() {
		f.rows = rows
		f.next = next
	}
}

func (f *fakeShareModel) existsToday(oid, mid int64, tp int32) bool {
	_, ok := f.rows[shareKey{oid: oid, mid: mid, tp: tp, day: f.today()}]
	return ok
}

// --- engagement_outbox 替身（UNIQUE (event_id)，自增 id 是发布顺序） ---

// fakeOutboxModel 实现 model.EngagementOutboxModel。
//
// 本包只测 logic→repository 的写入侧，因此只有 Insert 是「真依赖」；
// ListPending 与三个 Mark* 属于 internal/publisher（自有 outbox_store_test.go 覆盖），
// 在 logic 路径上被调用就是越界，所以这里返回明确的「不该被调用」错误而不是空实现，
// 调用轨迹照样记录，让序列断言能指出越界发生的那一步。
//
// 复刻的真实行为：
//   - ctime 为 0 时取 nowUnix，mtime 恒等于 ctime（model/engagementoutboxmodel.go Insert），
//     occurred_at 为 0 时同样落到 ctime；
//   - event_id 唯一：重复写同一 event_id 报错（对应 uniq_event_id），
//     这样「装配漏字段导致两个事件同 ID」不会被替身悄悄吞掉；
//   - id 由自增主键分配，发布顺序即 id 升序。
type fakeOutboxModel struct {
	faultInjector
	log   *callLog
	rows  map[string]*model.EngagementOutbox // event_id → 行
	order []int64                            // 插入顺序（id 升序 = 真实投递顺序）
	next  int64
}

func newFakeOutboxModel(log *callLog) *fakeOutboxModel {
	return &fakeOutboxModel{log: log, rows: map[string]*model.EngagementOutbox{}}
}

var _ model.EngagementOutboxModel = (*fakeOutboxModel)(nil)

func (f *fakeOutboxModel) Insert(_ context.Context, tx sqlx.Session, out *model.EngagementOutbox) error {
	f.log.add("outbox.Insert:%s:%d%s", out.AggregateID, out.SchemaVersion, sessTag(tx))
	if err := requireTx(tx, "outbox.Insert"); err != nil {
		return err
	}
	if err := f.fail("Insert"); err != nil {
		return err
	}
	if _, dup := f.rows[out.EventID]; dup {
		return fmt.Errorf("fake: engagement_outbox 撞 uniq_event_id（event_id=%s）", out.EventID)
	}
	if out.Ctime == 0 {
		out.Ctime = time.Now().Unix()
	}
	out.Mtime = out.Ctime
	if out.OccurredAt == 0 {
		out.OccurredAt = out.Ctime
	}
	cp := *out
	f.next++
	cp.ID = f.next
	cp.State = model.OutboxStatePending
	f.rows[cp.EventID] = &cp
	f.order = append(f.order, cp.ID)
	return nil
}

// errNotForLogicPath 是 publisher 侧方法被 logic 路径调用时的错误。
var errNotForLogicPath = errors.New("engagement-test: engagement_outbox 的读取/标记只属于 internal/publisher，不该出现在 logic 路径")

func (f *fakeOutboxModel) ListPending(_ context.Context, now int64, limit int32) ([]*model.EngagementOutbox, error) {
	f.log.add("outbox.ListPending:%d:%d", now, limit)
	return nil, errNotForLogicPath
}

func (f *fakeOutboxModel) MarkPublished(_ context.Context, id, publishedAt int64) error {
	f.log.add("outbox.MarkPublished:%d", id)
	return errNotForLogicPath
}

func (f *fakeOutboxModel) MarkRetry(_ context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	f.log.add("outbox.MarkRetry:%d", id)
	return errNotForLogicPath
}

func (f *fakeOutboxModel) MarkFailed(_ context.Context, id int64, lastError string) error {
	f.log.add("outbox.MarkFailed:%d", id)
	return errNotForLogicPath
}

// rowCount 断言本轮写出了几行事件（回滚后应为 0）。
func (f *fakeOutboxModel) rowCount() int { return len(f.rows) }

// lastRow 返回最后插入的一行（值拷贝）；没有行时 nil。
// 用例普遍只写一个事件，因此「最后一行」就是「那一行」。
func (f *fakeOutboxModel) lastRow() *model.EngagementOutbox {
	if len(f.order) == 0 {
		return nil
	}
	for _, row := range f.rows {
		if row.ID == f.order[len(f.order)-1] {
			cp := *row
			return &cp
		}
	}
	return nil
}

// rowsInOrder 按 id 升序返回值拷贝，用于断言同一对象的多个事件顺序。
func (f *fakeOutboxModel) rowsInOrder() []*model.EngagementOutbox {
	out := make([]*model.EngagementOutbox, 0, len(f.order))
	for _, id := range f.order {
		for _, row := range f.rows {
			if row.ID == id {
				cp := *row
				out = append(out, &cp)
				break
			}
		}
	}
	return out
}

// snapshot 拍下当前行集、顺序与自增值，返回还原闭包。
// 回滚必须连 order 一起还原：否则「事务失败后 outbox 一行都不留」会被残留的顺序表掩盖。
func (f *fakeOutboxModel) snapshot() undo {
	rows := make(map[string]*model.EngagementOutbox, len(f.rows))
	for k, v := range f.rows {
		cp := *v
		rows[k] = &cp
	}
	order := slices.Clone(f.order)
	next := f.next
	return func() {
		f.rows = rows
		f.order = order
		f.next = next
	}
}

// --- 分页切片（复刻 LIMIT ? OFFSET ?） ---

func pageOf[T any](hits []T, pn, ps int32) []T {
	offset := int((pn - 1) * ps)
	if offset >= len(hits) {
		return nil
	}
	end := offset + int(ps)
	if end > len(hits) {
		end = len(hits)
	}
	return hits[offset:end]
}

// --- 装配 ---

type store struct {
	log     *callLog
	cache   *fakeCache
	conn    *fakeConn
	like    *fakeLikeModel
	stat    *fakeStatModel
	favItem *fakeFavItemModel
	folder  *fakeFolderModel
	share   *fakeShareModel
	outbox  *fakeOutboxModel
	repo    *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	st := &store{
		log:     log,
		cache:   newFakeCache(log),
		conn:    &fakeConn{log: log},
		like:    newFakeLikeModel(log),
		stat:    newFakeStatModel(log),
		favItem: newFakeFavItemModel(log),
		folder:  newFakeFolderModel(log),
		share:   newFakeShareModel(log),
		outbox:  newFakeOutboxModel(log),
	}
	// 只登记事务能碰到的 5 个替身；favorite_folder 的三个方法都不带会话，
	// 它不参与事务，登记快照只会多一份永远不会执行的闭包。
	st.conn.snapshots = []func() undo{st.like.snapshot, st.stat.snapshot, st.favItem.snapshot, st.share.snapshot, st.outbox.snapshot}
	st.repo = repository.NewWithDeps(st.cache, st.conn, st.like, st.stat, st.favItem, st.folder, st.share, st.outbox)
	return st
}

// newTestSvc 装配只带内存依赖的 ServiceContext。与生产的差别仅在 Repository 的 8 个依赖是替身；
// Config 留零值即可：16 个 logic 只读 Repository 一个字段。
func newTestSvc(st *store) *svc.ServiceContext {
	return &svc.ServiceContext{Config: config.Config{}, Repository: st.repo}
}

// --- 静默布数据（纪律 4：不写 callLog） ---

// seedLike 布一行点赞关系（state：0 取消、1 点赞、2 点踩）。
func seedLike(st *store, business string, mid, upMid, originID, messageID int64, state int32, ctime int64) *model.ThumbupLike {
	st.like.next++
	k := likeKey{business, mid, messageID}
	st.like.rows[k] = &model.ThumbupLike{
		ID: st.like.next, Business: business, Mid: mid, UpMid: upMid,
		OriginID: originID, MessageID: messageID, State: state, Ctime: ctime, Mtime: ctime,
	}
	return st.like.get(business, mid, messageID)
}

// seedStat 布一行计数（含运营修正增量）。
func seedStat(st *store, business string, originID, messageID, likeNum, dislikeNum, likeChg, dislikeChg int64) *model.ThumbupStat {
	st.stat.next++
	k := statKey{business, originID, messageID}
	st.stat.rows[k] = &model.ThumbupStat{
		ID: st.stat.next, Business: business, OriginID: originID, MessageID: messageID,
		LikeNumber: likeNum, DislikeNumber: dislikeNum, LikeChange: likeChg, DislikeChange: dislikeChg,
		Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	}
	return st.stat.get(business, originID, messageID)
}

// seedFavItem 布一行收藏项（state：0 正常、1 已取消）。
func seedFavItem(st *store, mid, oid, fid int64, tp, otype, state int32) *model.FavoriteItem {
	st.favItem.next++
	k := favKey{mid, oid, tp}
	st.favItem.rows[k] = &model.FavoriteItem{
		ID: st.favItem.next, Oid: oid, Mid: mid, Fid: fid, Tp: tp, Otype: otype, State: state,
		Ctime: 1_700_000_100, Mtime: 1_700_000_100,
	}
	return st.favItem.get(mid, oid, tp)
}

// seedFolder 布一行收藏夹（fid 由用例给，便于写精确序列断言）。
func seedFolder(st *store, fid, mid int64, name string, public, state int32, count int32) *model.FavoriteFolder {
	if fid > st.folder.next {
		st.folder.next = fid
	}
	st.folder.rows[fid] = &model.FavoriteFolder{
		Fid: fid, Mid: mid, Name: name, Description: name + "-desc", Cover: "https://cover/" + itoa(fid),
		Public: public, State: state, Count: count, Ctime: 1_700_000_200, Mtime: 1_700_000_200,
	}
	return st.folder.get(fid)
}

// seedShare 布一行分享日志（day 由用例给，绕开墙钟）。
func seedShare(st *store, oid, mid int64, tp, day int32) {
	st.share.next++
	st.share.rows[shareKey{oid: oid, mid: mid, tp: tp, day: day}] = &model.ShareLog{
		ID: st.share.next, Oid: oid, Mid: mid, Type: tp, Day: day, Ctime: 1_700_000_300,
	}
}

// errBoom 是本包错误注入用的具体哨兵：断言一律 errors.Is(err, errBoom)，
// 不用「有错就算过」的宽松判定。
var errBoom = errors.New("engagement-test: downstream boom")

// errOther 用于「多依赖各自注入时不能串味」的对照。
var errOther = errors.New("engagement-test: another boom")
