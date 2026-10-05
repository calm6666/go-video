package logic

// fakes_test.go 是 social-graph logic 测试的内存替身集合。
//
// 为什么需要注入缝：social-graph 的 ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, 内存 followMd/blackMd/statMd/specialMd) 组装**真实的 Repository**，
// 只把它的 5 个依赖换成替身——这样「幂等判定 → 计数增量方向」「缓存读穿/回填」「计数器 miss 回刷」
// 整条判定链都在被测路径上，而不是把 Repository 也 mock 掉。
// 见 internal/repository/repository.go 的 Cacher 注释。
//
// 四条替身纪律（catalog / rights / playback / comment 四轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 里的写回不得污染库存行，否则
//     「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：关系服务要紧的结论是
//     「第二次关注有没有真的再发 INSERT」「取关时 -1 落在谁的计数上」「拉黑时先写黑名单还是先取关」。
//  3. 错误注入按**语句**粒度（faultInjector.failWith("UpsertSelect", err)）：本服务每个 Upsert/Delete
//     都是「SELECT 旧状态 + 条件写」两条语句，只给一个开关就分不出「读了没写」和「写了半截」。
//  4. 布数据走替身的**静默写入路径**（seedFollow / seedStat / cache.warmFollowing）：
//     布景不得记进 callLog，否则有序轨迹断言会把布景调用一起数进去。
//
// 与 model SQL 的对齐方式：relation_follow/relation_black/relation_special 都以迁移 SQL 里的
// UNIQUE KEY (mid, …) 为键存行；Upsert 复刻「先 SELECT state，oldState==newState 直接返回不发
// INSERT，否则 INSERT ... ON DUPLICATE KEY UPDATE state,mtime（**不含 ctime**）」；Delete 复刻
// 「SELECT → 无行返回 -1 → 已是 1 不再 UPDATE → 否则 UPDATE state=1」；列表复刻
// 「pn<1→1、ps∉[1,50]→20、COUNT 为 0 时不发第二条查询、ORDER BY ctime DESC LIMIT/OFFSET」。
// Redis 侧复刻 SISMEMBER+EXISTS 的 hit 口径、SREM 清空即删 key、INCRBY 建键、SETEX 覆盖。
//
// 覆盖边界（如实声明）：替身只复刻 model 层 SQL 的**语义**，不证明 SQL 与列名本身；
// 真实 *Cache（SismemberCtx/ExistsCtx/IncrbyCtx/SetexCtx/ZaddCtx 与其 TTL）没有被测，
// 所以 SETEX 的 3600s TTL、ZSet member 的十进制编码、parseInt64 遇到脏值的报错路径都不在断言范围内；
// 唯一索引冲突由 MySQL 保证（本替身按迁移 SQL 的 UNIQUE KEY 直接以二元组为键，无法证明约束存在）；
// relation_stat 的 UPDATE 在「行存在但 delta 为 0/0」时受影响行数为 0，会走 INSERT 分支，
// 该分支本服务不可达（Follow/Unfollow 的 delta 永远非 0），故未复刻，见 README 已知缺口。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go-video/services/social-graph/internal/config"
	"go-video/services/social-graph/internal/repository"
	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	rpc "go-video/services/social-graph/rpc"
)

// --- 状态常量（桥接 model 的未导出语义，与迁移 SQL 的 state 注释一致） ---

const (
	followNormal  = int32(model.FollowStateNormal)  // 0 正常关注
	followGone    = int32(model.FollowStateDeleted) // 1 已取关
	blackNormal   = int32(model.BlackStateNormal)
	blackGone     = int32(model.BlackStateDeleted)
	specialNormal = int32(model.SpecialStateNormal)
	specialGone   = int32(model.SpecialStateDeleted)
)

// --- 断言小工具（本包共享；与其它服务同包名不同文件，互不影响） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantStringsEQ 比较字符串切片（wantEQ 受 comparable 约束，切片只能另走这条）。
func wantStringsEQ(t *testing.T, label, field string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantInt64sEQ 比较 int64 切片（列表项的 mid/ctime 列断言用）。
func wantInt64sEQ(t *testing.T, label, field string, got, want []int64) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantInt32sEQ 比较 int32 切片（列表项的 attr 列断言用）。
func wantInt32sEQ(t *testing.T, label, field string, got, want []int32) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵（model 层用 %w 包装下游错误是允许的）。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %v", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

// wantErrMessage 断言未导出内联错误的整串消息（logic 返回的是 model 包里的哨兵变量，
// 但这些哨兵的文案同样要钉住，改文案必须有人察觉）。
func wantErrMessage(t *testing.T, label string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %q", label, want)
	}
	if err.Error() != want {
		t.Fatalf("%s：错误 = %q, want %q", label, err.Error(), want)
	}
}

func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：%v", label, err)
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

// wantCount 断言某类调用发生的次数（布数据走静默路径，所以数的是被测代码的调用）。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整序列 [%s]）", label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

// assertAround 断言墙钟派生值落在期望附近：repository/model 内部用 time.Now()，不可注入。
// 需要精确相等的地方一律用 wantEQ，不用这条。
func assertAround(t *testing.T, label, field string, got, want, slack int64) {
	t.Helper()
	if got < want-slack || got > want+slack {
		t.Errorf("%s：%s = %d, want %d±%d", label, field, got, want, slack)
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

// opsWith 返回含 frag 的调用，用于「这一类调用一次都没发生」的断言。
func (c *callLog) opsWith(frag string) []string {
	var out []string
	for _, o := range c.ops {
		if strings.Contains(o, frag) {
			out = append(out, o)
		}
	}
	return out
}

// --- 错误注入 ---

type faultInjector struct{ by map[string]error }

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

func (f *faultInjector) fail(method string) error { return f.by[method] }

// errWrap 复刻 model 层的 fmt.Errorf("table Method: %w", err) 包装口径，
// 让用例既能用 errors.Is 断言原哨兵，也能钉住包装文案。
func errWrap(op string, err error) error { return fmt.Errorf("%s: %w", op, err) }

// --- 缓存替身（复刻 repository.Cache 的 Redis 语义） ---

type fakeCache struct {
	faultInjector
	log        *callLog
	sets       map[int64]map[int64]bool // mid → 关注集合（key 不存在 = Redis key 不存在）
	zfollowers map[int64]map[int64]int64
	following  map[int64]int64 // 关注数计数器（键不存在 = miss）
	follower   map[int64]int64 // 粉丝数计数器
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{
		log: log, sets: map[int64]map[int64]bool{}, zfollowers: map[int64]map[int64]int64{},
		following: map[int64]int64{}, follower: map[int64]int64{},
	}
}

func (f *fakeCache) Ping(context.Context) error { f.log.add("cache.Ping"); return nil }

// IsFollowing 复刻「SISMEMBER 后再 EXISTS 判定 key 是否存在」：
// key 不存在一律 miss（无法区分「没关注」与「集合没建过」），存在才给 hit=true。
func (f *fakeCache) IsFollowing(_ context.Context, mid, followerMid int64) (bool, bool, error) {
	f.log.add("cache.IsFollowing:%d>%d", mid, followerMid)
	if err := f.fail("IsFollowing"); err != nil {
		return false, false, err
	}
	set, ok := f.sets[mid]
	if !ok {
		return false, false, nil
	}
	return set[followerMid], true, nil
}

func (f *fakeCache) AddFollowing(_ context.Context, mid, followerMid int64) error {
	f.log.add("cache.AddFollowing:%d>%d", mid, followerMid)
	if err := f.fail("AddFollowing"); err != nil {
		return err
	}
	if f.sets[mid] == nil {
		f.sets[mid] = map[int64]bool{}
	}
	f.sets[mid][followerMid] = true
	return nil
}

// DelFollowing 复刻 SREM：集合被搬空后 Redis 会自动删 key，于是下一次读是 miss 而不是 hit=false。
func (f *fakeCache) DelFollowing(_ context.Context, mid, followerMid int64) error {
	f.log.add("cache.DelFollowing:%d>%d", mid, followerMid)
	if err := f.fail("DelFollowing"); err != nil {
		return err
	}
	set, ok := f.sets[mid]
	if !ok {
		return nil
	}
	delete(set, followerMid)
	if len(set) == 0 {
		delete(f.sets, mid)
	}
	return nil
}

// AddFollower 复刻 ZADD（score=ctime）；member 在真实实现里是十进制字符串。
func (f *fakeCache) AddFollower(_ context.Context, mid, followerMid, ctime int64) error {
	f.log.add("cache.AddFollower:%d>%d", mid, followerMid)
	if err := f.fail("AddFollower"); err != nil {
		return err
	}
	if f.zfollowers[mid] == nil {
		f.zfollowers[mid] = map[int64]int64{}
	}
	f.zfollowers[mid][followerMid] = ctime
	return nil
}

func (f *fakeCache) DelFollower(_ context.Context, mid, followerMid int64) error {
	f.log.add("cache.DelFollower:%d>%d", mid, followerMid)
	if err := f.fail("DelFollower"); err != nil {
		return err
	}
	z, ok := f.zfollowers[mid]
	if !ok {
		return nil
	}
	delete(z, followerMid)
	if len(z) == 0 {
		delete(f.zfollowers, mid)
	}
	return nil
}

func (f *fakeCache) IncrFollowingCount(_ context.Context, mid, delta int64) error {
	f.log.add("cache.IncrFollowingCount:%d/%+d", mid, delta)
	if err := f.fail("IncrFollowingCount"); err != nil {
		return err
	}
	f.following[mid] += delta // INCRBY 不存在即建键
	return nil
}

func (f *fakeCache) IncrFollowerCount(_ context.Context, mid, delta int64) error {
	f.log.add("cache.IncrFollowerCount:%d/%+d", mid, delta)
	if err := f.fail("IncrFollowerCount"); err != nil {
		return err
	}
	f.follower[mid] += delta
	return nil
}

func (f *fakeCache) GetFollowingCount(_ context.Context, mid int64) (int64, bool, error) {
	f.log.add("cache.GetFollowingCount:%d", mid)
	if err := f.fail("GetFollowingCount"); err != nil {
		return 0, false, err
	}
	v, ok := f.following[mid]
	return v, ok, nil
}

func (f *fakeCache) GetFollowerCount(_ context.Context, mid int64) (int64, bool, error) {
	f.log.add("cache.GetFollowerCount:%d", mid)
	if err := f.fail("GetFollowerCount"); err != nil {
		return 0, false, err
	}
	v, ok := f.follower[mid]
	return v, ok, nil
}

// SetFollowingCount 复刻 SETEX（TTL=cacheTTLStat）；TTL 不在断言范围内。
func (f *fakeCache) SetFollowingCount(_ context.Context, mid, val int64) error {
	f.log.add("cache.SetFollowingCount:%d", mid)
	if err := f.fail("SetFollowingCount"); err != nil {
		return err
	}
	f.following[mid] = val
	return nil
}

func (f *fakeCache) SetFollowerCount(_ context.Context, mid, val int64) error {
	f.log.add("cache.SetFollowerCount:%d", mid)
	if err := f.fail("SetFollowerCount"); err != nil {
		return err
	}
	f.follower[mid] = val
	return nil
}

// --- 缓存读回与静默布景（纪律 4：不写 callLog） ---

func (f *fakeCache) warmFollowing(mid, followerMid int64) {
	if f.sets[mid] == nil {
		f.sets[mid] = map[int64]bool{}
	}
	f.sets[mid][followerMid] = true
}

func (f *fakeCache) warmCounts(mid, following, follower int64) {
	f.following[mid] = following
	f.follower[mid] = follower
}

func (f *fakeCache) cachedFollowing(mid int64) (int64, bool) {
	v, ok := f.following[mid]
	return v, ok
}

func (f *fakeCache) cachedFollower(mid int64) (int64, bool) {
	v, ok := f.follower[mid]
	return v, ok
}

func (f *fakeCache) cachedIsFollowing(mid, followerMid int64) bool {
	set, ok := f.sets[mid]
	return ok && set[followerMid]
}

func (f *fakeCache) followSetExists(mid int64) bool {
	_, ok := f.sets[mid]
	return ok
}

func (f *fakeCache) followerZScore(mid, member int64) (int64, bool) {
	z, ok := f.zfollowers[mid]
	if !ok {
		return 0, false
	}
	v, ok := z[member]
	return v, ok
}

// zsetHas 只问「粉丝时间轴里有没有这个 member」，不关心 score。
func (f *fakeCache) zsetHas(mid, member int64) bool {
	_, ok := f.zfollowers[mid][member]
	return ok
}

// --- 关注 model 替身（复刻 model/relation_follow.go 的 SQL 语义） ---

// pairKey 对应迁移 SQL 的 UNIQUE KEY uniq_mid_follower (mid, follower_mid)。
type pairKey struct{ a, b int64 }

type fakeFollowModel struct {
	faultInjector
	log   *callLog
	rows  map[pairKey]*model.RelationFollow
	next  int64 // AUTO_INCREMENT 游标
	calls []string
}

func newFakeFollowModel(log *callLog) *fakeFollowModel {
	// next 从 900 起：第一张自增行是 901，期望轨迹里的 id 才是确定的。
	return &fakeFollowModel{log: log, rows: map[pairKey]*model.RelationFollow{}, next: 900}
}

// Upsert 复刻「SELECT state → oldState==newState 直接返回（不发 INSERT，避免 mtime 抖动）
// → 否则 INSERT ... ON DUPLICATE KEY UPDATE state,mtime」。
// 关键语义：ON DUPLICATE 分支**不写 ctime、不写 attr**，所以取关后重新关注的行仍按老 ctime 排序。
func (f *fakeFollowModel) Upsert(_ context.Context, fow *model.RelationFollow, newState int32) (int32, error) {
	key := pairKey{fow.Mid, fow.FollowerMid}
	f.log.add("follow.Upsert:select:%d>%d", fow.Mid, fow.FollowerMid)
	if err := f.fail("UpsertSelect"); err != nil {
		return -1, errWrap("relation_follow Upsert select", err)
	}
	var oldState int32 = -1
	if row, ok := f.rows[key]; ok {
		oldState = row.State
	}
	if oldState == newState {
		return oldState, nil // 幂等：一条 UPDATE 都不发
	}
	f.log.add("follow.Upsert:write:%d>%d/%d", fow.Mid, fow.FollowerMid, newState)
	if err := f.fail("Upsert"); err != nil {
		return -1, errWrap("relation_follow Upsert", err)
	}
	now := time.Now().Unix()
	if row, ok := f.rows[key]; ok {
		row.State = newState
		row.Mtime = now // ctime / attr 保持原值（ON DUPLICATE 列表里没有它们）
		return oldState, nil
	}
	f.next++
	f.rows[key] = &model.RelationFollow{
		ID: f.next, Mid: fow.Mid, FollowerMid: fow.FollowerMid,
		Attr: fow.Attr, State: newState, Ctime: now, Mtime: now,
	}
	return oldState, nil
}

// Delete 复刻「SELECT state → 无行返回 -1（不报错）→ 已是 1 不再 UPDATE → 否则 UPDATE state=1」。
func (f *fakeFollowModel) Delete(_ context.Context, mid, followerMid int64) (int32, error) {
	f.log.add("follow.Delete:select:%d>%d", mid, followerMid)
	if err := f.fail("DeleteSelect"); err != nil {
		return -1, errWrap("relation_follow Delete select", err)
	}
	row, ok := f.rows[pairKey{mid, followerMid}]
	if !ok {
		return -1, nil
	}
	if row.State == followGone {
		return row.State, nil
	}
	f.log.add("follow.Delete:write:%d>%d", mid, followerMid)
	if err := f.fail("Delete"); err != nil {
		return -1, errWrap("relation_follow Delete", err)
	}
	old := row.State
	row.State = followGone
	row.Mtime = time.Now().Unix()
	return old, nil
}

// FindOne 复刻 WHERE mid=? AND follower_mid=? AND state=0；查无返回 (nil, nil)。
func (f *fakeFollowModel) FindOne(_ context.Context, mid, followerMid int64) (*model.RelationFollow, error) {
	f.log.add("follow.FindOne:%d>%d", mid, followerMid)
	if err := f.fail("FindOne"); err != nil {
		return nil, errWrap("relation_follow FindOne", err)
	}
	row, ok := f.rows[pairKey{mid, followerMid}]
	if !ok || row.State != followNormal {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// FindFollowings 复刻 WHERE mid=? AND state=0 AND follower_mid IN (?)，
// 先把 owners 全置 false 再按命中置 true ⇒ 返回 map 的键集合恒等于 owners 的去集。
func (f *fakeFollowModel) FindFollowings(_ context.Context, mid int64, owners []int64) (map[int64]bool, error) {
	f.log.add("follow.FindFollowings:%d/%v", mid, owners)
	f.calls = append(f.calls, fmt.Sprintf("%d/%v", mid, owners))
	if len(owners) == 0 {
		return map[int64]bool{}, nil
	}
	if err := f.fail("FindFollowings"); err != nil {
		return nil, errWrap("relation_follow FindFollowings", err)
	}
	out := make(map[int64]bool, len(owners))
	for _, o := range owners {
		out[o] = false
	}
	for _, o := range owners {
		if row, ok := f.rows[pairKey{mid, o}]; ok && row.State == followNormal {
			out[o] = true
		}
	}
	return out, nil
}

// FindFollowers 复刻 WHERE follower_mid=? AND state=0 AND mid IN (?)（即 mids 里谁关注了 owner）。
// 与 FindFollowings 的差别只在定位列，所以两条用例必须分别布数据才能证明方向没写反。
func (f *fakeFollowModel) FindFollowers(_ context.Context, owner int64, mids []int64) (map[int64]bool, error) {
	f.log.add("follow.FindFollowers:%d/%v", owner, mids)
	f.calls = append(f.calls, fmt.Sprintf("rev:%d/%v", owner, mids))
	if len(mids) == 0 {
		return map[int64]bool{}, nil
	}
	if err := f.fail("FindFollowers"); err != nil {
		return nil, errWrap("relation_follow FindFollowers", err)
	}
	out := make(map[int64]bool, len(mids))
	for _, mid := range mids {
		out[mid] = false
	}
	for _, mid := range mids {
		if row, ok := f.rows[pairKey{mid, owner}]; ok && row.State == followNormal {
			out[mid] = true
		}
	}
	return out, nil
}

// ListFollowings 复刻「WHERE mid=? AND state=0 ORDER BY ctime DESC LIMIT/OFFSET」。
// 轨迹里 count 带**原始** pn/ps（证明 logic 没钳制），rows 带**钳制后**的值（证明钳制在 model）。
func (f *fakeFollowModel) ListFollowings(_ context.Context, mid int64, pn, ps int32) ([]*model.RelationFollow, int32, error) {
	return f.list("ListFollowings", mid, pn, ps, func(r *model.RelationFollow) bool { return r.Mid == mid })
}

// ListFollowers 复刻「WHERE follower_mid=? AND state=0 …」（即谁关注了 mid）。
func (f *fakeFollowModel) ListFollowers(_ context.Context, mid int64, pn, ps int32) ([]*model.RelationFollow, int32, error) {
	return f.list("ListFollowers", mid, pn, ps, func(r *model.RelationFollow) bool { return r.FollowerMid == mid })
}

func (f *fakeFollowModel) list(which string, mid int64, pn, ps int32, match func(*model.RelationFollow) bool) ([]*model.RelationFollow, int32, error) {
	f.log.add("follow.%s:count:%d/%d/%d", which, mid, pn, ps)
	if err := f.fail(which + "Count"); err != nil {
		return nil, 0, errWrap("relation_follow "+which+" count", err)
	}
	matched := f.selectRows(func(r *model.RelationFollow) bool {
		return match(r) && r.State == followNormal
	})
	total := int32(len(matched))
	if total == 0 {
		return nil, 0, nil // 与 SQL 一致：count 为 0 时不再发第二条查询
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	f.log.add("follow.%s:rows:%d/%d/%d", which, mid, pn, ps)
	if err := f.fail(which + "Rows"); err != nil {
		return nil, 0, errWrap("relation_follow "+which+" list", err)
	}
	// ORDER BY ctime DESC；ctime 相同 SQL 顺序未定义，故用 id DESC 兜底，
	// 用例必须把 ctime 布成互不相同（见 distinctFollow）。
	slices.SortFunc(matched, func(a, b *model.RelationFollow) int {
		if c := cmpInt64(b.Ctime, a.Ctime); c != 0 {
			return c
		}
		return cmpInt64(b.ID, a.ID)
	})
	return pageRelations(matched, pn, ps), total, nil
}

// DeleteByMid 复刻 UPDATE ... WHERE mid=? AND follower_mid=? AND state=0。
// 本方法在 Repository 里**零调用方**（拉黑只走 Unfollow），用例只用它断言「没被调用」。
func (f *fakeFollowModel) DeleteByMid(_ context.Context, mid, followerMid int64) error {
	f.log.add("follow.DeleteByMid:%d>%d", mid, followerMid)
	if err := f.fail("DeleteByMid"); err != nil {
		return errWrap("relation_follow DeleteByMid", err)
	}
	if row, ok := f.rows[pairKey{mid, followerMid}]; ok && row.State == followNormal {
		row.State = followGone
		row.Mtime = time.Now().Unix()
	}
	return nil
}

func (f *fakeFollowModel) selectRows(pred func(*model.RelationFollow) bool) []*model.RelationFollow {
	var out []*model.RelationFollow
	for _, row := range f.rows {
		if !pred(row) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// rowsOf 返回 mid 发起的全部关注行（含软删），按 follower_mid 升序，便于「行没被误删」断言。
func (f *fakeFollowModel) rowsOf(mid int64) []*model.RelationFollow {
	out := f.selectRows(func(r *model.RelationFollow) bool { return r.Mid == mid })
	slices.SortFunc(out, func(a, b *model.RelationFollow) int { return cmpInt64(a.FollowerMid, b.FollowerMid) })
	return out
}

func (f *fakeFollowModel) row(mid, followerMid int64) *model.RelationFollow {
	row, ok := f.rows[pairKey{mid, followerMid}]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// stateOfFollow 读库内当前状态；查无返回 -1（与 Upsert/Delete 的「无记录」口径同值）。
func (f *fakeFollowModel) stateOfFollow(mid, followerMid int64) int32 {
	if row, ok := f.rows[pairKey{mid, followerMid}]; ok {
		return row.State
	}
	return -1
}

func (f *fakeFollowModel) countRows() int { return len(f.rows) }

// put 是静默写入（布数据用，不记轨迹）：主键取入参 ID，0 则自增。
func (f *fakeFollowModel) put(r *model.RelationFollow) *model.RelationFollow {
	stored := *r
	if stored.ID == 0 {
		f.next++
		stored.ID = f.next
	}
	if stored.ID > f.next {
		f.next = stored.ID
	}
	f.rows[pairKey{stored.Mid, stored.FollowerMid}] = &stored
	cp := stored
	return &cp
}

// --- 黑名单 model 替身（复刻 model/relation_black.go） ---

type fakeBlackModel struct {
	faultInjector
	log  *callLog
	rows map[pairKey]*model.RelationBlack
	next int64
}

func newFakeBlackModel(log *callLog) *fakeBlackModel {
	return &fakeBlackModel{log: log, rows: map[pairKey]*model.RelationBlack{}, next: 600}
}

func (f *fakeBlackModel) Upsert(_ context.Context, b *model.RelationBlack, newState int32) (int32, error) {
	key := pairKey{b.Mid, b.BlackMid}
	f.log.add("black.Upsert:select:%d>%d", b.Mid, b.BlackMid)
	if err := f.fail("UpsertSelect"); err != nil {
		return -1, errWrap("relation_black Upsert select", err)
	}
	var oldState int32 = -1
	if row, ok := f.rows[key]; ok {
		oldState = row.State
	}
	if oldState == newState {
		return oldState, nil
	}
	f.log.add("black.Upsert:write:%d>%d/%d", b.Mid, b.BlackMid, newState)
	if err := f.fail("Upsert"); err != nil {
		return -1, errWrap("relation_black Upsert", err)
	}
	now := time.Now().Unix()
	if row, ok := f.rows[key]; ok {
		row.State = newState
		row.Mtime = now
		return oldState, nil
	}
	f.next++
	f.rows[key] = &model.RelationBlack{ID: f.next, Mid: b.Mid, BlackMid: b.BlackMid, State: newState, Ctime: now, Mtime: now}
	return oldState, nil
}

func (f *fakeBlackModel) Delete(_ context.Context, mid, blackMid int64) (int32, error) {
	f.log.add("black.Delete:select:%d>%d", mid, blackMid)
	if err := f.fail("DeleteSelect"); err != nil {
		return -1, errWrap("relation_black Delete select", err)
	}
	row, ok := f.rows[pairKey{mid, blackMid}]
	if !ok {
		return -1, nil
	}
	if row.State == blackGone {
		return row.State, nil
	}
	f.log.add("black.Delete:write:%d>%d", mid, blackMid)
	if err := f.fail("Delete"); err != nil {
		return -1, errWrap("relation_black Delete", err)
	}
	row.State = blackGone
	row.Mtime = time.Now().Unix()
	return blackNormal, nil
}

func (f *fakeBlackModel) FindOne(_ context.Context, mid, blackMid int64) (*model.RelationBlack, error) {
	f.log.add("black.FindOne:%d>%d", mid, blackMid)
	if err := f.fail("FindOne"); err != nil {
		return nil, errWrap("relation_black FindOne", err)
	}
	row, ok := f.rows[pairKey{mid, blackMid}]
	if !ok || row.State != blackNormal {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// FindBlacks 复刻 WHERE mid=? AND state=0 AND black_mid IN (?)。
func (f *fakeBlackModel) FindBlacks(_ context.Context, mid int64, mids []int64) (map[int64]bool, error) {
	f.log.add("black.FindBlacks:%d/%v", mid, mids)
	if len(mids) == 0 {
		return map[int64]bool{}, nil
	}
	if err := f.fail("FindBlacks"); err != nil {
		return nil, errWrap("relation_black FindBlacks", err)
	}
	out := make(map[int64]bool, len(mids))
	for _, blackMid := range mids {
		out[blackMid] = false
	}
	for _, blackMid := range mids {
		if row, ok := f.rows[pairKey{mid, blackMid}]; ok && row.State == blackNormal {
			out[blackMid] = true
		}
	}
	return out, nil
}

func (f *fakeBlackModel) ListByMid(_ context.Context, mid int64, pn, ps int32) ([]*model.RelationBlack, int32, error) {
	f.log.add("black.ListByMid:count:%d/%d/%d", mid, pn, ps)
	if err := f.fail("ListByMidCount"); err != nil {
		return nil, 0, errWrap("relation_black ListByMid count", err)
	}
	matched := f.selectRows(func(r *model.RelationBlack) bool { return r.Mid == mid && r.State == blackNormal })
	total := int32(len(matched))
	if total == 0 {
		return nil, 0, nil
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	f.log.add("black.ListByMid:rows:%d/%d/%d", mid, pn, ps)
	if err := f.fail("ListByMidRows"); err != nil {
		return nil, 0, errWrap("relation_black ListByMid list", err)
	}
	return pageRelations(matched, pn, ps), total, nil
}

func (f *fakeBlackModel) selectRows(pred func(*model.RelationBlack) bool) []*model.RelationBlack {
	var out []*model.RelationBlack
	for _, row := range f.rows {
		if !pred(row) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	// ORDER BY ctime DESC；ctime 相同时 SQL 顺序未定义，故用 id DESC 兜底，
	// 用例必须把 ctime 布成互不相同（见 distinctBlack）。
	slices.SortFunc(out, func(a, b *model.RelationBlack) int {
		if c := cmpInt64(b.Ctime, a.Ctime); c != 0 {
			return c
		}
		return cmpInt64(b.ID, a.ID)
	})
	return out
}

func (f *fakeBlackModel) row(mid, blackMid int64) *model.RelationBlack {
	row, ok := f.rows[pairKey{mid, blackMid}]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeBlackModel) stateOf(mid, blackMid int64) int32 {
	if row, ok := f.rows[pairKey{mid, blackMid}]; ok {
		return row.State
	}
	return -1
}

func (f *fakeBlackModel) countRows() int { return len(f.rows) }

func (f *fakeBlackModel) put(b *model.RelationBlack) *model.RelationBlack {
	stored := *b
	if stored.ID == 0 {
		f.next++
		stored.ID = f.next
	}
	if stored.ID > f.next {
		f.next = stored.ID
	}
	f.rows[pairKey{stored.Mid, stored.BlackMid}] = &stored
	cp := stored
	return &cp
}

// --- 特别关注 model 替身（复刻 model/relation_special.go） ---

type fakeSpecialModel struct {
	faultInjector
	log  *callLog
	rows map[pairKey]*model.RelationSpecial
	next int64
}

func newFakeSpecialModel(log *callLog) *fakeSpecialModel {
	return &fakeSpecialModel{log: log, rows: map[pairKey]*model.RelationSpecial{}, next: 400}
}

func (f *fakeSpecialModel) Upsert(_ context.Context, s *model.RelationSpecial, newState int32) (int32, error) {
	key := pairKey{s.Mid, s.SpecialMid}
	f.log.add("special.Upsert:select:%d>%d", s.Mid, s.SpecialMid)
	if err := f.fail("UpsertSelect"); err != nil {
		return -1, errWrap("relation_special Upsert select", err)
	}
	var oldState int32 = -1
	if row, ok := f.rows[key]; ok {
		oldState = row.State
	}
	if oldState == newState {
		return oldState, nil
	}
	f.log.add("special.Upsert:write:%d>%d/%d", s.Mid, s.SpecialMid, newState)
	if err := f.fail("Upsert"); err != nil {
		return -1, errWrap("relation_special Upsert", err)
	}
	now := time.Now().Unix()
	if row, ok := f.rows[key]; ok {
		row.State = newState
		row.Mtime = now
		return oldState, nil
	}
	f.next++
	f.rows[key] = &model.RelationSpecial{ID: f.next, Mid: s.Mid, SpecialMid: s.SpecialMid, State: newState, Ctime: now, Mtime: now}
	return oldState, nil
}

func (f *fakeSpecialModel) Delete(_ context.Context, mid, specialMid int64) (int32, error) {
	f.log.add("special.Delete:select:%d>%d", mid, specialMid)
	if err := f.fail("DeleteSelect"); err != nil {
		return -1, errWrap("relation_special Delete select", err)
	}
	row, ok := f.rows[pairKey{mid, specialMid}]
	if !ok {
		return -1, nil
	}
	if row.State == specialGone {
		return row.State, nil
	}
	f.log.add("special.Delete:write:%d>%d", mid, specialMid)
	if err := f.fail("Delete"); err != nil {
		return -1, errWrap("relation_special Delete", err)
	}
	row.State = specialGone
	row.Mtime = time.Now().Unix()
	return specialNormal, nil
}

// FindOne 复刻 WHERE mid=? AND special_mid=? AND state=0。
// 与 DeleteByMid 一样，Repository 的写侧路径里没有调用方（本服务仍没有单条「是否特别关注」RPC）；
// 2026-10-04 起 relation_special 的读入口是 FindSpecials（供 RichRelations 用）。
func (f *fakeSpecialModel) FindOne(_ context.Context, mid, specialMid int64) (*model.RelationSpecial, error) {
	f.log.add("special.FindOne:%d>%d", mid, specialMid)
	if err := f.fail("FindOne"); err != nil {
		return nil, errWrap("relation_special FindOne", err)
	}
	row, ok := f.rows[pairKey{mid, specialMid}]
	if !ok || row.State != specialNormal {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// FindSpecials 复刻 WHERE mid=? AND state=0 AND special_mid IN (?)。
func (f *fakeSpecialModel) FindSpecials(_ context.Context, mid int64, mids []int64) (map[int64]bool, error) {
	f.log.add("special.FindSpecials:%d/%v", mid, mids)
	if len(mids) == 0 {
		return map[int64]bool{}, nil
	}
	if err := f.fail("FindSpecials"); err != nil {
		return nil, errWrap("relation_special FindSpecials", err)
	}
	out := make(map[int64]bool, len(mids))
	for _, specialMid := range mids {
		out[specialMid] = false
	}
	for _, specialMid := range mids {
		if row, ok := f.rows[pairKey{mid, specialMid}]; ok && row.State == specialNormal {
			out[specialMid] = true
		}
	}
	return out, nil
}

func (f *fakeSpecialModel) row(mid, specialMid int64) *model.RelationSpecial {
	row, ok := f.rows[pairKey{mid, specialMid}]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeSpecialModel) stateOf(mid, specialMid int64) int32 {
	if row, ok := f.rows[pairKey{mid, specialMid}]; ok {
		return row.State
	}
	return -1
}

func (f *fakeSpecialModel) countRows() int { return len(f.rows) }

func (f *fakeSpecialModel) put(s *model.RelationSpecial) *model.RelationSpecial {
	stored := *s
	if stored.ID == 0 {
		f.next++
		stored.ID = f.next
	}
	if stored.ID > f.next {
		f.next = stored.ID
	}
	f.rows[pairKey{stored.Mid, stored.SpecialMid}] = &stored
	cp := stored
	return &cp
}

// --- 计数 model 替身（复刻 model/relation_stat.go） ---

type fakeStatModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.RelationStat
	next int64
}

func newFakeStatModel(log *callLog) *fakeStatModel {
	return &fakeStatModel{log: log, rows: map[int64]*model.RelationStat{}, next: 300}
}

func (f *fakeStatModel) Find(_ context.Context, mid int64) (*model.RelationStat, error) {
	f.log.add("stat.Find:%d", mid)
	if err := f.fail("Find"); err != nil {
		return nil, errWrap("relation_stat Find", err)
	}
	row, ok := f.rows[mid]
	if !ok {
		return nil, nil // model 把 sql.ErrNoRows 折成 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

// Incr 复刻「先 UPDATE … following=following+?, follower=follower+? → 受影响 0 行时
// INSERT … VALUES(mid, max(fd,0), max(rd,0), …) ON DUPLICATE KEY UPDATE 累加」。
// 负 delta 在 INSERT 分支被 max() 截成 0，所以缺快照的用户取关不会得到负计数。
func (f *fakeStatModel) Incr(_ context.Context, mid, followingDelta, followerDelta int64) error {
	f.log.add("stat.Incr:update:%d/%+d/%+d", mid, followingDelta, followerDelta)
	if err := f.fail("IncrUpdate"); err != nil {
		return errWrap("relation_stat Incr update", err)
	}
	if row, ok := f.rows[mid]; ok {
		row.Following += followingDelta
		row.Follower += followerDelta
		row.Mtime = time.Now().Unix()
		return nil
	}
	f.log.add("stat.Incr:insert:%d/%+d/%+d", mid, followingDelta, followerDelta)
	if err := f.fail("IncrInsert"); err != nil {
		return errWrap("relation_stat Incr insert", err)
	}
	f.next++
	f.rows[mid] = &model.RelationStat{
		ID: f.next, Mid: mid,
		Following: max(followingDelta, 0), Follower: max(followerDelta, 0),
		Ctime: time.Now().Unix(), Mtime: time.Now().Unix(),
	}
	return nil
}

func (f *fakeStatModel) row(mid int64) *model.RelationStat {
	row, ok := f.rows[mid]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeStatModel) countRows() int { return len(f.rows) }

func (f *fakeStatModel) put(s *model.RelationStat) *model.RelationStat {
	stored := *s
	if stored.ID == 0 {
		f.next++
		stored.ID = f.next
	}
	if stored.ID > f.next {
		f.next = stored.ID
	}
	f.rows[stored.Mid] = &stored
	cp := stored
	return &cp
}

// --- 排序与分页 helper ---

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// pageRelations 复刻 LIMIT/OFFSET（rows 必须已按 ctime DESC 排好）。
func pageRelations[T any](rows []T, pn, ps int32) []T {
	offset := int((pn - 1) * ps)
	if offset >= len(rows) {
		return nil
	}
	return rows[offset:min(offset+int(ps), len(rows))]
}

// --- 装配 ---

type store struct {
	log      *callLog
	cache    *fakeCache
	follows  *fakeFollowModel
	blacks   *fakeBlackModel
	stats    *fakeStatModel
	specials *fakeSpecialModel
	repo     *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	st := &store{
		log:      log,
		cache:    newFakeCache(log),
		follows:  newFakeFollowModel(log),
		blacks:   newFakeBlackModel(log),
		stats:    newFakeStatModel(log),
		specials: newFakeSpecialModel(log),
	}
	st.repo = repository.NewWithDeps(st.cache, st.follows, st.blacks, st.stats, st.specials)
	return st
}

// env 是一次用例的完整运行时：真实 Repository（5 个依赖全为替身）。
// Config 留零值即可：14 个 logic 只读 Repository 一个字段（无 MQ/事件出口，见 README）。
type env struct {
	t      *testing.T
	st     *store
	svcCtx *svc.ServiceContext
}

func newEnv(t *testing.T) *env {
	st := newStore()
	return &env{t: t, st: st, svcCtx: &svc.ServiceContext{Config: config.Config{}, Repository: st.repo}}
}

// --- 静默布景（纪律 4：不写 callLog） ---

func seedFollow(st *store, r *model.RelationFollow) *model.RelationFollow {
	return st.follows.put(r)
}

func seedBlack(st *store, r *model.RelationBlack) *model.RelationBlack { return st.blacks.put(r) }

func seedSpecial(st *store, r *model.RelationSpecial) *model.RelationSpecial {
	return st.specials.put(r)
}

func seedStat(st *store, r *model.RelationStat) *model.RelationStat { return st.stats.put(r) }

// --- 布景样板：字段值互不相同，方便逐字段核对投影 ---

// distinctFollow 给一行「谁(mid)关注了谁(follower_mid)」，各字段两两不同。
// ctime 由入参决定（列表按 ctime DESC 排序，辨识度全靠它）。
func distinctFollow(id, mid, followerMid, ctime int64) *model.RelationFollow {
	return &model.RelationFollow{
		ID: id, Mid: mid, FollowerMid: followerMid, Attr: 0,
		State: followNormal, Ctime: ctime, Mtime: ctime + 11,
	}
}

// distinctBlack 给一行「mid 拉黑了 blackMid」。
func distinctBlack(id, mid, blackMid, ctime int64) *model.RelationBlack {
	return &model.RelationBlack{
		ID: id, Mid: mid, BlackMid: blackMid, State: blackNormal, Ctime: ctime, Mtime: ctime + 23,
	}
}

// distinctSpecial 给一行「mid 特别关注 specialMid」。
func distinctSpecial(id, mid, specialMid, ctime int64) *model.RelationSpecial {
	return &model.RelationSpecial{
		ID: id, Mid: mid, SpecialMid: specialMid, State: specialNormal, Ctime: ctime, Mtime: ctime + 31,
	}
}

// assertFollowRow 逐字段核对库存的 relation_follow 行（7 列全覆盖）。
func assertFollowRow(t *testing.T, label string, got, src *model.RelationFollow) {
	t.Helper()
	if got == nil || src == nil {
		t.Fatalf("%s：行缺失 got=%v src=%v", label, got, src)
	}
	wantEQ(t, label, "id", got.ID, src.ID)
	wantEQ(t, label, "mid", got.Mid, src.Mid)
	wantEQ(t, label, "follower_mid", got.FollowerMid, src.FollowerMid)
	wantEQ(t, label, "attr", got.Attr, src.Attr)
	wantEQ(t, label, "state", got.State, src.State)
	wantEQ(t, label, "ctime", got.Ctime, src.Ctime)
	wantEQ(t, label, "mtime", got.Mtime, src.Mtime)
}

// itemMids / itemCtimes / itemAttrs 把 rpc 列表项拆成三列，便于「排序 + 无重叠 + attr 硬编码」断言。
func itemMids(items []*rpc.RelationItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.GetMid())
	}
	return out
}

func itemCtimes(items []*rpc.RelationItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.GetCtime())
	}
	return out
}

func itemAttrs(items []*rpc.RelationItem) []int32 {
	out := make([]int32, 0, len(items))
	for _, it := range items {
		out = append(out, it.GetAttr())
	}
	return out
}

// --- 本包共享的错误注入哨兵 ---
//
// 用例注入的是这两个**具体变量**，断言一律走 errors.Is，不用「任何非 nil 错误」蒙过。

var (
	errStore = errors.New("social-graph-test: store unavailable")
	errCache = errors.New("social-graph-test: cache unavailable")
)

// wantGuardRejected 是 14 个方法守卫表的统一断言：
// 返回指定错误（哨兵 + 整串文案双钉）**且**在打缓存/打库之前就被拒（调用轨迹零增长）。
func wantGuardRejected(t *testing.T, st *store, label string, want error, call func() error) {
	t.Helper()
	before := st.log.snapshot()
	err := call()
	wantErrIs(t, label, err, want)
	wantErrMessage(t, label, err, want.Error())
	wantNoCall(t, label, st, before)
}
