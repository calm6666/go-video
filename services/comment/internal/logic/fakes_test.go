package logic

// fakes_test.go 是 comment logic 测试的内存替身集合。
//
// 为什么需要注入缝：comment 的 ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, 内存 commentMd, 内存 reportMd) 组装**真实的 Repository**，
// 只把它的 3 个依赖换成替身——这样「缓存读穿/回填/失效、软删门槛、计数口径」整条判定链
// 都在被测路径上，而不是把 Repository 也 mock 掉。见 internal/repository/repository.go 的 Cacher 注释。
//
// 四条替身纪律（catalog / rights / playback 三轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 里的写回不得污染库存行，否则
//     「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：comment 要紧的结论是
//     「发布后失效了哪些 key」「拒绝删除时有没有留下半截失效」「置顶清掉了谁的置顶」。
//  3. 错误注入按方法粒度（faultInjector.failWith("FindOne", err)），不用一个全局 err。
//  4. 布数据走替身的**静默写入路径**（seedComment / cache.warmList / cache.warmStats）：
//     布景不得记进 callLog，否则序列断言要把布景调用一起数进去。
//
// Redis key 与 model SQL 语义的对齐方式：keyList/keyOne/keyStats/invalidatePattern 与 repository
// 未导出的常量逐字复刻，不一致时缓存命中类用例即红；ListRoots/ListReplies/SoftDelete/SetPinned/
// CountByTarget 的过滤条件、排序、状态门槛、LIMIT/OFFSET 与 model/*.go 的 SQL 同语义复刻。
//
// 覆盖边界（如实声明）：替身只复刻 model 层 SQL 的**语义**，不证明 SQL 与列名本身；
// `services/comment/model/*.go` 的占位符、`floor`/`pinned` 两个未进入 model 结构的列，
// 以及「迁移声明 rpid 无 AUTO_INCREMENT、而 model.Insert 依赖 LastInsertId」这一冲突，
// 都无法在纯内存用例里证伪——替身按代码意图自增分配 rpid（见 fakeCommentModel.Insert 注释），
// 该冲突登记在 README 已知缺口。仓库里也没有 comment 的迁移↔model 列级对账门禁。

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

	"go-video/services/comment/internal/config"
	"go-video/services/comment/internal/repository"
	"go-video/services/comment/internal/svc"
	"go-video/services/comment/model"
	rpc "go-video/services/comment/rpc"
)

// --- 状态常量（model 的 state 列是 int32，SQL 里写的是字面量；这里桥接 rpc 枚举名） ---

const (
	stateNormal   = int32(rpc.CommentState_STATE_NORMAL)   // 0 正常
	stateFolded   = int32(rpc.CommentState_STATE_FOLDED)   // 1 折叠
	stateDeleted  = int32(rpc.CommentState_STATE_DELETED)  // 2 已删除
	statePinned   = int32(rpc.CommentState_STATE_PINNED)   // 3 置顶
	statePending  = int32(rpc.CommentState_STATE_PENDING)  // 4 待审核
	stateRejected = int32(rpc.CommentState_STATE_REJECTED) // 5 审核驳回
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

// wantErrIs 断言错误链里有 want 这个哨兵（logic/repository 用 %w 包装下游错误是允许的）。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %v", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

// wantErrMessage 断言内联 errors.New 的整串消息：comment 有 5 处守卫返回的是未导出哨兵的
// 内联错误（model 包里没有对应 error 变量），只能按文本锁定，防止改文案时无人察觉。
func wantErrMessage(t *testing.T, label string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %q", label, want)
	}
	if err.Error() != want {
		t.Fatalf("%s：错误 = %q, want %q", label, err.Error(), want)
	}
}

// wantErrContains 断言错误消息含片段（用于 repository 包装的下游错误，如缓存解码失败）。
func wantErrContains(t *testing.T, label string, err error, frag string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want 含 %q", label, frag)
	}
	if !strings.Contains(err.Error(), frag) {
		t.Fatalf("%s：错误 = %v, want 含 %q", label, err, frag)
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

// assertAround 断言墙钟派生值落在期望附近：logic 内部用 time.Now()，跨秒抖动不可避免。
// 需要精确相等的地方（如 reply.ctime == 库存 ctime）一律用 wantEQ，不用这条。
func assertAround(t *testing.T, label, field string, got, want, slack int64) {
	t.Helper()
	if got < want-slack || got > want+slack {
		t.Errorf("%s：%s = %d, want %d±%d", label, field, got, want, slack)
	}
}

// pickOps 从完整轨迹里挑出以任一 prefix 开头的调用，用于「这一类调用一次都没发生」的断言。
func pickOps(log *callLog, prefixes ...string) []string {
	var out []string
	for _, o := range log.ops {
		for _, p := range prefixes {
			if strings.HasPrefix(o, p) {
				out = append(out, o)
				break
			}
		}
	}
	return out
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

// itoa 拼调用轨迹与期望值里的整数段：轨迹由替身用 %d 格式化，用例期望值必须同源，
// 否则 strconv 与 fmt 的差别会变成一条永远对不上的断言。
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// --- 错误注入 ---

type faultInjector struct{ by map[string]error }

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

func (f *faultInjector) fail(method string) error { return f.by[method] }

// --- Redis key 复刻（与 repository 未导出的常量逐字对齐；不一致时读穿用例即红） ---

func keyList(oid int64, tp int32, sort string, pn, ps int32) string {
	return fmt.Sprintf("cmt:list:%d:%d:%s:%d:%d", oid, tp, sort, pn, ps)
}
func keyOne(rpid int64) string { return fmt.Sprintf("cmt:1:%d", rpid) }
func keyStats(oid int64, tp int32) string {
	return fmt.Sprintf("cmt:st:%d:%d", oid, tp)
}
func invalidatePattern(oid int64, tp int32) string {
	return fmt.Sprintf("cmt:list:%d:%d:*", oid, tp)
}

// --- 缓存替身 ---

// statsEntry 是一条 CommentStats 缓存值（真实实现编码成 {"total":..,"root_total":..}）。
type statsEntry struct{ total, rootTotal int64 }

// fakeCache 实现 repository.Cacher。miss 返回零值 + hit=false + err=nil，
// 与真实 Cache 处理 redis.Nil 的口径一致。
type fakeCache struct {
	faultInjector
	log   *callLog
	lists map[string]string        // 列表缓存原始 payload
	ones  map[int64]*model.Comment // 评论主体缓存（Repository 只写不读，仍存下来以便断言写了什么）
	stats map[string]statsEntry
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{log: log, lists: map[string]string{}, ones: map[int64]*model.Comment{}, stats: map[string]statsEntry{}}
}

func (f *fakeCache) Ping(context.Context) error { f.log.add("cache.Ping"); return nil }

func (f *fakeCache) GetList(_ context.Context, oid int64, tp int32, sort string, pn, ps int32) (string, error) {
	key := keyList(oid, tp, sort, pn, ps)
	f.log.add("cache.GetList:%s", key)
	if err := f.fail("GetList"); err != nil {
		return "", err
	}
	return f.lists[key], nil
}

func (f *fakeCache) SetList(_ context.Context, oid int64, tp int32, sort string, pn, ps int32, payload string) error {
	key := keyList(oid, tp, sort, pn, ps)
	f.log.add("cache.SetList:%s", key)
	if err := f.fail("SetList"); err != nil {
		return err
	}
	f.lists[key] = payload
	return nil
}

func (f *fakeCache) SetOne(_ context.Context, cm *model.Comment) error {
	f.log.add("cache.SetOne:%s", keyOne(cm.Rpid))
	if err := f.fail("SetOne"); err != nil {
		return err
	}
	cp := *cm
	f.ones[cm.Rpid] = &cp
	return nil
}

func (f *fakeCache) DelOne(_ context.Context, rpid int64) error {
	f.log.add("cache.DelOne:%s", keyOne(rpid))
	if err := f.fail("DelOne"); err != nil {
		return err
	}
	delete(f.ones, rpid)
	return nil
}

func (f *fakeCache) GetStats(_ context.Context, oid int64, tp int32) (int64, int64, bool, error) {
	key := keyStats(oid, tp)
	f.log.add("cache.GetStats:%s", key)
	if err := f.fail("GetStats"); err != nil {
		return 0, 0, false, err
	}
	e, ok := f.stats[key]
	if !ok {
		return 0, 0, false, nil
	}
	return e.total, e.rootTotal, true, nil
}

func (f *fakeCache) SetStats(_ context.Context, oid int64, tp int32, total, rootTotal int64) error {
	key := keyStats(oid, tp)
	f.log.add("cache.SetStats:%s", key)
	if err := f.fail("SetStats"); err != nil {
		return err
	}
	f.stats[key] = statsEntry{total: total, rootTotal: rootTotal}
	return nil
}

// InvalidateListByOid 复刻真实实现的 Keys 通配 + Del：只删与该 pattern 同前缀的 key。
// 正因如此，Repository.PinComment 传 tp=0 时删不到 tp=1 的列表 key——
// 用例 TestPinCommentDoesNotInvalidateListCacheForItsTarget 把这条缺陷钉住。
func (f *fakeCache) InvalidateListByOid(_ context.Context, oid int64, tp int32) error {
	pattern := invalidatePattern(oid, tp)
	f.log.add("cache.Invalidate:%s", pattern)
	if err := f.fail("InvalidateListByOid"); err != nil {
		return err
	}
	prefix := fmt.Sprintf("cmt:list:%d:%d:", oid, tp)
	for k := range f.lists {
		if strings.HasPrefix(k, prefix) {
			delete(f.lists, k)
		}
	}
	return nil
}

func (f *fakeCache) DelStats(_ context.Context, oid int64, tp int32) error {
	key := keyStats(oid, tp)
	f.log.add("cache.DelStats:%s", key)
	if err := f.fail("DelStats"); err != nil {
		return err
	}
	delete(f.stats, key)
	return nil
}

// --- 缓存读回与静默布景（纪律 4：不写 callLog） ---

func (f *fakeCache) listPayload(oid int64, tp int32, sort string, pn, ps int32) string {
	return f.lists[keyList(oid, tp, sort, pn, ps)]
}

func (f *fakeCache) statsOf(oid int64, tp int32) (statsEntry, bool) {
	e, ok := f.stats[keyStats(oid, tp)]
	return e, ok
}

func (f *fakeCache) oneOf(rpid int64) *model.Comment {
	cm, ok := f.ones[rpid]
	if !ok {
		return nil
	}
	cp := *cm
	return &cp
}

// warmList 直接把一段 payload 放进列表缓存（模拟「上一次读已回填」或「上一个进程写入的旧值」）。
func (f *fakeCache) warmList(oid int64, tp int32, sort string, pn, ps int32, payload string) {
	f.lists[keyList(oid, tp, sort, pn, ps)] = payload
}

// warmStats 直接放一条计数快照。
func (f *fakeCache) warmStats(oid int64, tp int32, total, rootTotal int64) {
	f.stats[keyStats(oid, tp)] = statsEntry{total: total, rootTotal: rootTotal}
}

// listCachePayload 按 repository.ListComments 的编码口径生成列表缓存 payload。
// 编码格式若变化，本 helper 与「缓存命中」「缓存被污染」两类用例一起变红——这是刻意的。
func listCachePayload(t *testing.T, comments []*model.Comment, total int32) string {
	t.Helper()
	bs, err := json.Marshal(struct {
		Comments []*model.Comment `json:"comments"`
		Total    int32            `json:"total"`
	}{Comments: comments, Total: total})
	if err != nil {
		t.Fatalf("编码列表缓存失败：%v", err)
	}
	return string(bs)
}

// --- 评论 model 替身（复刻 model/comment.go 的 SQL 语义） ---

type fakeCommentModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.Comment
	next  int64
	calls []string // 归一化入参，便于断言过滤/分页/排序透传
}

func newFakeCommentModel(log *callLog) *fakeCommentModel {
	// next 从 500 起：第一张自增行是 501，用例里的期望轨迹因此完全可预测。
	return &fakeCommentModel{log: log, rows: map[int64]*model.Comment{}, next: 500}
}

// Insert 复刻 INSERT ... VALUES(..., 0, 0)：忽略入参主键与计数快照，自增分配并返回
// （LastInsertId 口径），ctime 原样回传（SQL 里就是占位符传进来的 c.Ctime）。
// 注意：真实迁移里 comment.rpid 没有 AUTO_INCREMENT（见 README 已知缺口 #1），
// 替身按代码**意图**自增，因此本包用例证明不了该冲突是否存在。
func (f *fakeCommentModel) Insert(_ context.Context, c *model.Comment) (int64, int64, error) {
	f.log.add("comment.Insert")
	if err := f.fail("Insert"); err != nil {
		return 0, 0, err
	}
	f.next++
	stored := *c
	stored.Rpid = f.next
	stored.LikeCount = 0  // SQL 写死 0
	stored.ReplyCount = 0 // SQL 写死 0
	f.rows[stored.Rpid] = &stored
	return stored.Rpid, c.Ctime, nil
}

// FindOne 复刻 WHERE rpid=?：查无此行返回 (nil, nil)（model 把 sql.ErrNoRows 折成 nil）。
func (f *fakeCommentModel) FindOne(_ context.Context, rpid int64) (*model.Comment, error) {
	f.log.add("comment.FindOne:%d", rpid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[rpid]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// SoftDelete 复刻两条 UPDATE 的门槛差异：
//   - admin：WHERE rpid=?（不看 mid，也不检查受影响行数——真库里删不存在的行同样返回 nil）；
//   - 本人：WHERE rpid=? AND mid=?，0 行受影响返回 ErrCommentNotFoundOrForbidden；
//   - 两条都把 mtime 写成传入的 mid（model/comment.go 的实参错位，见 README 已知缺口 #2）。
func (f *fakeCommentModel) SoftDelete(_ context.Context, rpid, mid int64, admin bool) error {
	f.log.add("comment.SoftDelete:%d/%v", rpid, admin)
	if err := f.fail("SoftDelete"); err != nil {
		return err
	}
	row, ok := f.rows[rpid]
	if !ok {
		if admin {
			return nil // 0 行受影响，不报错
		}
		return model.ErrCommentNotFoundOrForbidden
	}
	if !admin && row.Mid != mid {
		return model.ErrCommentNotFoundOrForbidden
	}
	row.State = stateDeleted
	row.Mtime = mid // 复刻生产 SQL：写进 mtime 的是操作者 mid，不是时间戳
	return nil
}

// ListRoots 复刻 WHERE oid=? AND tp=? AND root=0 AND state NOT IN (2,5)
// ORDER BY state=3 DESC, <like_count|ctime> DESC LIMIT ? OFFSET ?，
// 以及 pn<1→1、ps∉[1,49]→20 的钳制和 total==0 的提前返回。
func (f *fakeCommentModel) ListRoots(_ context.Context, oid int64, tp int32, sortMode string, pn, ps int32) ([]*model.Comment, int32, error) {
	f.log.add("comment.ListRoots:%d/%d/%s/%d/%d", oid, tp, sortMode, pn, ps)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%s/%d/%d", oid, tp, sortMode, pn, ps))
	if err := f.fail("ListRoots"); err != nil {
		return nil, 0, err
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 49 {
		ps = 20
	}
	matched := f.selectRows(func(r *model.Comment) bool {
		return r.Oid == oid && r.Tp == tp && r.Root == 0 && rootVisibleState(r.State)
	})
	total := int32(len(matched))
	if total == 0 {
		return nil, 0, nil // 与 SQL 一致：count=0 时不再发第二条查询
	}
	sortRows(matched, sortMode, "DESC")
	return page(matched, pn, ps), total, nil
}

// ListReplies 复刻 WHERE root=? AND state NOT IN (2,5) ORDER BY ctime ASC LIMIT ? OFFSET ?。
func (f *fakeCommentModel) ListReplies(_ context.Context, root int64, pn, ps int32) ([]*model.Comment, int32, error) {
	f.log.add("comment.ListReplies:%d/%d/%d", root, pn, ps)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%d", root, pn, ps))
	if err := f.fail("ListReplies"); err != nil {
		return nil, 0, err
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 49 {
		ps = 20
	}
	matched := f.selectRows(func(r *model.Comment) bool {
		return r.Root == root && rootVisibleState(r.State)
	})
	total := int32(len(matched))
	if total == 0 {
		return nil, 0, nil
	}
	sortRows(matched, "time", "ASC")
	return page(matched, pn, ps), total, nil
}

// SetPinned 复刻三段 UPDATE。注意两条真实语义：
//   - 清旧置顶只按 oid，不看 tp、也不看被置顶行是否属于该 oid；
//   - 设新置顶只按 rpid，不校验 oid：传别的稿件的 rpid 也会被改成 state=3（可见）。
func (f *fakeCommentModel) SetPinned(_ context.Context, rpid, oid int64, pin bool) error {
	f.log.add("comment.SetPinned:%d/%d/%v", rpid, oid, pin)
	if err := f.fail("SetPinned"); err != nil {
		return err
	}
	if pin {
		for _, row := range f.rows {
			if row.Oid == oid && row.State == statePinned {
				row.State = stateNormal
			}
		}
		if row, ok := f.rows[rpid]; ok {
			row.State = statePinned
		}
		return nil
	}
	if row, ok := f.rows[rpid]; ok && row.State == statePinned {
		row.State = stateNormal
	}
	return nil
}

// CountByTarget 复刻两条 COUNT(*)：total 含回复，rootTotal 只数 root=0；
// 两者的可见口径都是 state NOT IN (2,5)。
func (f *fakeCommentModel) CountByTarget(_ context.Context, oid int64, tp int32) (int64, int64, error) {
	f.log.add("comment.CountByTarget:%d/%d", oid, tp)
	f.calls = append(f.calls, fmt.Sprintf("count:%d/%d", oid, tp))
	if err := f.fail("CountByTarget"); err != nil {
		return 0, 0, err
	}
	var total, rootTotal int64
	for _, row := range f.rows {
		if row.Oid != oid || row.Tp != tp || !rootVisibleState(row.State) {
			continue
		}
		total++
		if row.Root == 0 {
			rootTotal++
		}
	}
	return total, rootTotal, nil
}

func (f *fakeCommentModel) IncrLikeCount(_ context.Context, rpid int64, delta int32) error {
	f.log.add("comment.IncrLike:%d/%d", rpid, delta)
	if err := f.fail("IncrLikeCount"); err != nil {
		return err
	}
	if row, ok := f.rows[rpid]; ok {
		row.LikeCount += delta
	}
	return nil
}

func (f *fakeCommentModel) IncrReplyCount(_ context.Context, rpid int64, delta int32) error {
	f.log.add("comment.IncrReply:%d/%d", rpid, delta)
	if err := f.fail("IncrReplyCount"); err != nil {
		return err
	}
	if row, ok := f.rows[rpid]; ok {
		row.ReplyCount += delta
	}
	return nil
}

// rootVisibleState 是 ListRoots/ListReplies/CountByTarget 三处 SQL 共用的可见口径。
func rootVisibleState(state int32) bool {
	return state != stateDeleted && state != stateRejected
}

// selectRows 按条件取库存行的**副本**（纪律 1：用例改返回值不得污染库存）。
func (f *fakeCommentModel) selectRows(pred func(*model.Comment) bool) []*model.Comment {
	var out []*model.Comment
	for _, row := range f.rows {
		if !pred(row) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// sortRows 复刻 ORDER BY state=3 DESC + 主排序键。同键时按 rpid 兜底，
// 真实 SQL 这种情况下顺序不确定，所以用例必须布出可区分的 like_count/ctime。
func sortRows(rows []*model.Comment, sortMode, dir string) {
	desc := dir == "DESC"
	by := func(x, y int64) bool { return desc == (x > y) }
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		aPinned, bPinned := a.State == statePinned, b.State == statePinned
		if aPinned != bPinned {
			return aPinned // ORDER BY state = 3 DESC
		}
		if sortMode == "time" {
			if a.Ctime != b.Ctime {
				return by(a.Ctime, b.Ctime)
			}
		} else if a.LikeCount != b.LikeCount {
			if by(int64(a.LikeCount), int64(b.LikeCount)) {
				return true
			}
		}
		return a.Rpid > b.Rpid
	})
}

// page 复刻 LIMIT/OFFSET。
func page(rows []*model.Comment, pn, ps int32) []*model.Comment {
	offset := int((pn - 1) * ps)
	if offset >= len(rows) {
		return nil
	}
	end := offset + int(ps)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

// row 读库内当前行（副本）；查无返回 nil。
func (f *fakeCommentModel) row(rpid int64) *model.Comment {
	row, ok := f.rows[rpid]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// state 读库内当前状态（值读，避免用例误改库存行）。
func (f *fakeCommentModel) state(rpid int64) int32 {
	if row, ok := f.rows[rpid]; ok {
		return row.State
	}
	return -1
}

func (f *fakeCommentModel) countRows() int { return len(f.rows) }

func (f *fakeCommentModel) rpidList() []string {
	var out []string
	for id := range f.rows {
		out = append(out, fmt.Sprintf("%d", id))
	}
	sort.Strings(out)
	return out
}

// put 是静默写入（布数据用，不记轨迹）：主键取入参 Rpid，0 则自增。
func (f *fakeCommentModel) put(c *model.Comment) int64 {
	stored := *c
	if stored.Rpid == 0 {
		f.next++
		stored.Rpid = f.next
	}
	if stored.Rpid > f.next {
		f.next = stored.Rpid
	}
	f.rows[stored.Rpid] = &stored
	return stored.Rpid
}

// --- 举报 model 替身（复刻 comment_report 的 AUTO_INCREMENT 口径） ---

type fakeReportModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.CommentReport
	next  int64
	calls []string
}

func newFakeReportModel(log *callLog) *fakeReportModel {
	return &fakeReportModel{log: log, rows: map[int64]*model.CommentReport{}, next: 700}
}

func (f *fakeReportModel) Insert(_ context.Context, r *model.CommentReport) (int64, error) {
	f.log.add("report.Insert:%d/%d", r.Rpid, r.ReporterMid)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	f.next++
	stored := *r
	stored.ReportID = f.next
	f.rows[stored.ReportID] = &stored
	return stored.ReportID, nil
}

func (f *fakeReportModel) row(reportID int64) *model.CommentReport {
	row, ok := f.rows[reportID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeReportModel) only(t *testing.T) *model.CommentReport {
	t.Helper()
	if len(f.rows) != 1 {
		t.Fatalf("期望 comment_report 只有 1 行，实际 %d 行", len(f.rows))
	}
	for _, row := range f.rows {
		cp := *row
		return &cp
	}
	panic("unreachable")
}

func (f *fakeReportModel) countRows() int { return len(f.rows) }

// --- 装配 ---

type store struct {
	log      *callLog
	cache    *fakeCache
	comments *fakeCommentModel
	reports  *fakeReportModel
	repo     *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	st := &store{
		log:      log,
		cache:    newFakeCache(log),
		comments: newFakeCommentModel(log),
		reports:  newFakeReportModel(log),
	}
	st.repo = repository.NewWithDeps(st.cache, st.comments, st.reports)
	return st
}

// env 是一次用例的完整运行时：真实 Repository（依赖为替身）。
// Config 留零值即可：7 个 logic 只读 Repository 一个字段（审核/风控/RPC 客户端尚未接入，见 README）。
type env struct {
	t      *testing.T
	st     *store
	svcCtx *svc.ServiceContext
}

func newEnv(t *testing.T) *env {
	st := newStore()
	return &env{t: t, st: st, svcCtx: &svc.ServiceContext{Config: config.Config{}, Repository: st.repo}}
}

// seedComment 静默布一行评论（不写轨迹，见纪律 4），返回库里那一行的副本。
func seedComment(t *testing.T, st *store, c *model.Comment) *model.Comment {
	t.Helper()
	return st.comments.row(st.comments.put(c))
}

// nowUnix 与 logic 内部同源（time.Now()，不可注入），布景只留安全余量。
func nowUnix() int64 { return time.Now().Unix() }

// --- 布景样板：一条字段值互不相同的根评论，方便逐字段核对投影 ---

// distinctComment 给一条每个字段都不同的评论。相同值会掩盖「串字段」类缺陷，
// 因此任何投影断言都以本函数为基准。
func distinctComment(rpid, oid int64) *model.Comment {
	return &model.Comment{
		Rpid:       rpid,
		Oid:        oid,
		Tp:         1,
		Root:       0,
		Parent:     0,
		Mid:        7000 + rpid%100,
		Content:    fmt.Sprintf("第 %d 楼的内容", rpid),
		State:      stateNormal,
		Ctime:      1_700_000_000 + rpid,
		Mtime:      1_800_000_000 + rpid,
		LikeCount:  int32(rpid % 97),
		ReplyCount: int32(rpid % 13),
	}
}

// assertCommentProjected 逐字段核对 model.Comment → rpc.CommentInfo 的投影（12 个字段全覆盖）。
func assertCommentProjected(t *testing.T, label string, got *rpc.CommentInfo, src *model.Comment) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s：投影结果为 nil", label)
	}
	wantEQ(t, label, "rpid", got.GetRpid(), src.Rpid)
	wantEQ(t, label, "oid", got.GetOid(), src.Oid)
	wantEQ(t, label, "tp", got.GetTp(), src.Tp)
	wantEQ(t, label, "root", got.GetRoot(), src.Root)
	wantEQ(t, label, "parent", got.GetParent(), src.Parent)
	wantEQ(t, label, "mid", got.GetMid(), src.Mid)
	wantEQ(t, label, "content", got.GetContent(), src.Content)
	wantEQ(t, label, "state", got.GetState(), src.State)
	wantEQ(t, label, "ctime", got.GetCtime(), src.Ctime)
	wantEQ(t, label, "mtime", got.GetMtime(), src.Mtime)
	wantEQ(t, label, "like_count", got.GetLikeCount(), src.LikeCount)
	wantEQ(t, label, "reply_count", got.GetReplyCount(), src.ReplyCount)
}
