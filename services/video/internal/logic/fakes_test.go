package logic

// fakes_test.go 是 video logic 用例的内存替身集合。
//
// 覆盖范围：internal/logic 的全部 8 个 RPC 方法都已有用例，按文件分工：
//   - createsubmission_logic_test.go —— CreateSubmission
//   - read_logic_test.go —— GetSubmission / ListSubmissions / ListByState
//   - update_submission_logic_test.go —— UpdateSubmission
//   - delete_submission_logic_test.go —— DeleteSubmission
//   - transition_state_logic_test.go —— TransitionState，并直接以 statemachine.go 的
//     legalTransitions 为期望值遍历 14×14 全部状态对（不手抄枚举）
//   - get_playable_source_logic_test.go —— GetPlayableSource
//   - content_event_logic_test.go 跨 TransitionState 与 DeleteSubmission 两条路径，
//     专测 video_outbox 那一行：哪些状态转换产事件、payload 字段口径、事件行与审计行是否同源同事务
//
// 为什么需要注入缝：video 的 ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, 假连接, 内存 model) 组装**真实的 Repository**，
// 只把它的 6 个依赖换成替身：「状态推进是否一个事务内写状态+审计+事件行」
// 「哪些状态转换该产 content.published.v1」「失效缓存发生在落库之后还是之前」
// 「读是否走 model」这些判定仍然整条在被测路径上（与 comment/playback 同一纪律）。
//
// 五条替身纪律：
//  1. **读取返回拷贝**：每个 Find*/List* 都返回值副本，logic 若就地改字段不会污染库存行，
//     否则「有没有真的落库」这类断言会被共享指针掩盖。
//  2. **写侧像真实 SQL 一样写**：Insert 自己按 max+1 分配主键（video_submission.aid 在迁移里
//     是 AUTO_INCREMENT），只写生产语句出现的列，生产 SQL 不写的列一律不补；
//     UpdateFields/UpdateState 恒把 mtime 刷成 nowUnix()（与 model 一致），
//     UpdateState/审计 Insert/事件 Insert 只接受非 nil 事务会话，绕过事务直接失败。
//     未实现的写方法返回 errUnexpectedDependency（不返回假成功），所以「声称没走某条 SQL」
//     一定会被 callLog 抓到。布数据走静默路径（seedSub/seedVersion/seedAudit），不写 callLog。
//  3. **有序 callLog**（`<表>.<方法>[:<键>]`）：既数次数也断顺序。本服务要紧的结论全是顺序类的：
//     守卫拒绝后一次依赖调用都不许发生、List 的 COUNT 先于 SELECT 且同口径、
//     TransitionState 是「读→事务(读+状态+审计+事件行)→失效缓存→复读」、GetPlayableSource 未发布
//     时不许去碰 video_version。
//  4. **错误按方法粒度注入**（st.fail("submission.UpdateFields", err)），并且替身像 model 一样
//     用 `%w` 包装，因此用例只能用 errors.Is 断言原样透传——任何把错误换成别的哨兵或
//     吞掉错误的写法都会红。
//  5. **事务不回滚**：fakeConn.TransactCtx 直接调 fn，中途失败时前面已写的行**留在库里**
//     （真实 MySQL 会回滚）。因此失败用例一律断言「失败后库里到底还剩什么」，
//     绝不写「应当已回滚」这种假结论。
//
// 假件的 SQL 语义逐条对齐 model/submissionmodel.go 的真实语句（WHERE 片段、ORDER BY aid DESC、
// LIMIT/OFFSET、COUNT 先判 0 再取行、pn/ps 夹紧、RowsAffected==0 → ErrSubmissionNotFound）。
//
// 如实声明两处**刻意**与真库不同、且都已在 services/video/README.md 的「已知缺口」登记：
//   - RowsAffected：本服务 DSN（etc/video.v1.yaml:14）未开 clientFoundRows，真实 MySQL 返回的是
//     **changed rows**，即「把 title 更新成它已有的值」会得到 0 行、被判 ErrSubmissionNotFound。
//     替身默认按「WHERE 命中行数」实现（等价于开了 clientFoundRows），
//     并额外提供 changedRowsOnly 开关复现真实行为，
//     见 update_submission_logic_test.go 的 TestUpdateSubmissionNoOpUpdateDivergence。
//   - 并发：TransitionState 的「二次校验」读的是 r.subMd.FindOne（**不带 session**，另一条连接），
//     UPDATE 也没有 `WHERE state = from` 的 CAS 条件，所以纯内存用例只能靠 race hook 复现
//     「校验后被人插队」的丢失更新，见 transition_state_logic_test.go 的
//     TestTransitionStateLostUpdateIsNotDetected（对照：插队发生在二次校验之前会被检出，
//     见同文件 TestTransitionStateLostUpdateBeforeTxCheckIsDetected）。
//
// Redis 只被 repository.Cache 用作稿件详情失效（video:sub:<aid>），本文件替身复刻 key 口径；
// GetSubmission 生产上根本不读缓存（repository.GetSubmission 直接回源），用例锁住这一点，
// 不伪造「缓存已生效」。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/video/internal/repository"
	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

// --- 断言小工具（本包共享） ---

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

func wantEq[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantSeq 断言 from 之后的完整调用序列（顺序本身就是要锁的结论时用这条）。
// want 为空表示「此后一次依赖调用都不许发生」。
func wantSeq(t *testing.T, label string, log *callLog, from int, want ...string) {
	t.Helper()
	got := log.opsFrom(from)
	if !slices.Equal(got, want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label,
			strings.Join(got, " → "), strings.Join(want, " → "))
	}
}

// wantNoCallAfter 断言 from 之后没有任何依赖调用：入参守卫必须发生在触库之前。
func wantNoCallAfter(t *testing.T, label string, log *callLog, from int) {
	t.Helper()
	if ops := log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

// wantCount 断言某前缀的调用次数（seed 静默，所以数出来的都是被测代码的调用）。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整轨迹 [%s]）",
			label, prefix, got, want, log.trace())
	}
}

// wantNilReply / wantReply 少用；本包直接判 err == nil。

// --- 有序调用轨迹 + 错误注入 + 并发钩子 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

func (c *callLog) snapshot() int             { return len(c.ops) }
func (c *callLog) opsFrom(from int) []string { return slices.Clone(c.ops[from:]) }
func (c *callLog) trace() string             { return strings.Join(c.ops, " → ") }

func (c *callLog) countPrefix(prefix string) int {
	n := 0
	for _, o := range c.ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

type stub struct {
	log       *callLog
	faults    map[string]error
	nthFaults map[string]map[int]error
	counts    map[string]int
	hooks     map[string][]func()
	fired     map[string]int
}

func newStub() *stub {
	return &stub{
		log: &callLog{}, faults: map[string]error{},
		nthFaults: map[string]map[int]error{}, counts: map[string]int{},
		hooks: map[string][]func(){}, fired: map[string]int{},
	}
}

// enter 在每个依赖调用入口做三件事：触发并发钩子、记轨迹、返回注入的错误。
// key 是 `<依赖>.<方法>`（错误注入粒度），opFormat 是写进 callLog 的 `<表>.<方法>:<键>`。
func (s *stub) enter(key, opFormat string, args ...any) error {
	if hs := s.hooks[key]; len(hs) > 0 {
		for _, fn := range hs {
			fn()
		}
		s.fired[key] += len(hs)
		s.hooks[key] = nil
	}
	s.log.add(opFormat, args...)
	s.counts[key]++
	if err := s.faults[key]; err != nil {
		return err
	}
	if errs := s.nthFaults[key]; errs != nil {
		if err := errs[s.counts[key]]; err != nil {
			return err
		}
	}
	return nil
}

// fail 让第 key 个依赖**每次**都返回错误；替身会像 model 一样用 %w 包装后再抛出。
func (s *stub) fail(key string, err error) { s.faults[key] = err }

// failNth 让第 key 个依赖的**第 n 次**调用（从 1 计数）返回错误。
// 用于「复读/事务内二次校验」这类同名调用多次出现的位置。
func (s *stub) failNth(key string, n int, err error) {
	if s.nthFaults[key] == nil {
		s.nthFaults[key] = map[int]error{}
	}
	s.nthFaults[key][n] = err
}

// before 布一个一次性钩子：在第 key 这次依赖调用**之前**执行 fn，模拟别的入口插队。
func (s *stub) before(key string, fn func()) { s.hooks[key] = append(s.hooks[key], fn) }

// checkHooks 在用例末尾确认布下的并发钩子都真的触发过（没触发＝断言在空转）。
func (s *stub) checkHooks(t *testing.T) {
	t.Helper()
	for key, hs := range s.hooks {
		if len(hs) > 0 {
			t.Errorf("并发钩子 %s 始终没触发，用例在空转（轨迹 [%s]）", key, s.log.trace())
		}
	}
}

// wrapf 复刻 model 的错误包装口径（`fmt.Errorf("video_submission Insert: %w", err)`）。
func wrapf(table, method string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", table, method, err)
}

// errUnexpectedDependency：本用例声称不该走到的依赖被走到了。
var errUnexpectedDependency = errors.New("fake: 不该被调用的依赖")

// nextFakePK 模拟 AUTO_INCREMENT：表内已有号最大值 +1。
func nextFakePK[T any](rows []T, idOf func(T) int64) int64 {
	var hi int64
	for _, r := range rows {
		if v := idOf(r); v > hi {
			hi = v
		}
	}
	return hi + 1
}

// --- video_submission 替身（语义逐条对齐 model/submissionmodel.go:48-208） ---

type fakeSubmissionModel struct {
	s    *stub
	rows []*model.VideoSubmission
	// changedRowsOnly=true 时复现真实 MySQL（DSN 未开 clientFoundRows）的 changed-rows 行为：
	// 更新值与旧值全同 → RowsAffected()==0 → model 判 ErrSubmissionNotFound。
	changedRowsOnly bool
}

var _ model.VideoSubmissionModel = (*fakeSubmissionModel)(nil)

func (f *fakeSubmissionModel) find(aid int64) *model.VideoSubmission {
	for _, r := range f.rows {
		if r.Aid == aid {
			return r
		}
	}
	return nil
}

func (f *fakeSubmissionModel) Insert(ctx context.Context, s *model.VideoSubmission) (int64, error) {
	if err := f.s.enter("submission.Insert", "video_submission.Insert:m%d/s%d", s.Mid, s.State); err != nil {
		return 0, wrapf("video_submission", "Insert", err)
	}
	// 生产 INSERT 只写 mid/title/desc/cover/typeid/tag/state/ctime/mtime，aid 自增（迁移 000001）。
	id := nextFakePK(f.rows, func(r *model.VideoSubmission) int64 { return r.Aid })
	cp := *s
	cp.Aid = id
	f.rows = append(f.rows, &cp)
	return id, nil
}

func (f *fakeSubmissionModel) FindOne(ctx context.Context, aid int64) (*model.VideoSubmission, error) {
	if err := f.s.enter("submission.FindOne", "video_submission.FindOne:%d", aid); err != nil {
		return nil, wrapf("video_submission", "FindOne", err)
	}
	cur := f.find(aid)
	if cur == nil {
		return nil, nil // model: sql.ErrNoRows → (nil, nil)
	}
	cp := *cur
	return &cp, nil
}

func (f *fakeSubmissionModel) List(ctx context.Context, mid int64, typeid int32, pn, ps int32) ([]*model.VideoSubmission, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	key := fmt.Sprintf("%d/%d/%d/%d", mid, typeid, pn, ps)
	if err := f.s.enter("submission.List.count", "video_submission.List.count:%s", key); err != nil {
		return nil, 0, wrapf("video_submission", "List count", err)
	}
	matched := f.filter(func(r *model.VideoSubmission) bool {
		if r.State == model.StateDeleted { // WHERE state <> StateDeleted
			return false
		}
		if mid > 0 && r.Mid != mid {
			return false
		}
		if typeid > 0 && r.Typeid != typeid {
			return false
		}
		return true
	})
	if len(matched) == 0 {
		return nil, 0, nil // total==0 时 model 直接返回，不再发 SELECT
	}
	if err := f.s.enter("submission.List.rows", "video_submission.List.rows:%s", key); err != nil {
		return nil, 0, wrapf("video_submission", "List", err)
	}
	return paginate(matched, pn, ps), int32(len(matched)), nil
}

func (f *fakeSubmissionModel) ListByState(ctx context.Context, state int32, pn, ps int32) ([]*model.VideoSubmission, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	key := fmt.Sprintf("%d/%d/%d", state, pn, ps)
	if err := f.s.enter("submission.ListByState.count", "video_submission.ListByState.count:%s", key); err != nil {
		return nil, 0, wrapf("video_submission", "ListByState count", err)
	}
	matched := f.filter(func(r *model.VideoSubmission) bool { return r.State == state })
	if len(matched) == 0 {
		return nil, 0, nil
	}
	if err := f.s.enter("submission.ListByState.rows", "video_submission.ListByState.rows:%s", key); err != nil {
		return nil, 0, wrapf("video_submission", "ListByState", err)
	}
	return paginate(matched, pn, ps), int32(len(matched)), nil
}

// filter 返回命中行的**倒序（aid DESC）副本切片**，对齐 model 的 ORDER BY aid DESC。
func (f *fakeSubmissionModel) filter(keep func(*model.VideoSubmission) bool) []*model.VideoSubmission {
	out := make([]*model.VideoSubmission, 0, len(f.rows))
	for _, r := range f.rows {
		if keep(r) {
			cp := *r
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *model.VideoSubmission) int {
		switch {
		case a.Aid > b.Aid:
			return -1
		case a.Aid < b.Aid:
			return 1
		default:
			return 0
		}
	})
	return out
}

// paginate 按 model 的 LIMIT ? OFFSET ?（pn/ps 已在调用方夹紧）取一页。
func paginate(rows []*model.VideoSubmission, pn, ps int32) []*model.VideoSubmission {
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

func (f *fakeSubmissionModel) UpdateFields(ctx context.Context, aid int64,
	title, desc, cover string, typeid int32, tag string) error {
	if err := f.s.enter("submission.UpdateFields", "video_submission.UpdateFields:%d", aid); err != nil {
		return wrapf("video_submission", "UpdateFields", err)
	}
	cur := f.find(aid)
	if cur == nil {
		return model.ErrSubmissionNotFound // RowsAffected()==0
	}
	changed := cur.Title != title || cur.Desc != desc || cur.Cover != cover ||
		cur.Typeid != typeid || cur.Tag != tag
	if f.changedRowsOnly && !changed {
		return model.ErrSubmissionNotFound // changed rows == 0
	}
	cur.Title, cur.Desc, cur.Cover, cur.Typeid, cur.Tag = title, desc, cover, typeid, tag
	cur.Mtime = time.Now().Unix() // 生产 SQL 恒写 mtime = nowUnix()
	return nil
}

func (f *fakeSubmissionModel) UpdateState(ctx context.Context, tx sqlx.Session, aid int64, state int32) error {
	if err := f.s.enter("submission.UpdateState", "video_submission.UpdateState:%d/%d", aid, state); err != nil {
		return wrapf("video_submission", "UpdateState", err)
	}
	if tx == nil {
		return errors.New("fake: video_submission.UpdateState 必须带事务会话（生产签名要求）")
	}
	cur := f.find(aid)
	if cur == nil {
		return model.ErrSubmissionNotFound
	}
	if f.changedRowsOnly && cur.State == state {
		return model.ErrSubmissionNotFound
	}
	cur.State = state
	cur.Mtime = time.Now().Unix()
	return nil
}

// --- video_version 替身（对齐 model/submissionmodel.go:211-247） ---

type fakeVersionModel struct {
	s    *stub
	rows []*model.VideoVersion
}

var _ model.VideoVersionModel = (*fakeVersionModel)(nil)

func (f *fakeVersionModel) Insert(ctx context.Context, v *model.VideoVersion) error {
	err := f.s.enter("version.Insert", "video_version.Insert:%d/%d", v.Aid, v.Version)
	if err == nil {
		// 本期 8 个方法都不该写版次表：真被调用就报错，不返回假成功。
		err = errUnexpectedDependency
	}
	return wrapf("video_version", "Insert", err)
}

func (f *fakeVersionModel) ListByAid(ctx context.Context, aid int64) ([]*model.VideoVersion, error) {
	if err := f.s.enter("version.ListByAid", "video_version.ListByAid:%d", aid); err != nil {
		return nil, wrapf("video_version", "ListByAid", err)
	}
	out := make([]*model.VideoVersion, 0, len(f.rows))
	for _, r := range f.rows {
		if r.Aid == aid {
			cp := *r
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *model.VideoVersion) int { // ORDER BY version DESC
		switch {
		case a.Version > b.Version:
			return -1
		case a.Version < b.Version:
			return 1
		default:
			return 0
		}
	})
	return out, nil
}

// --- video_audit_log 替身（对齐 model/submissionmodel.go:250-286） ---

type fakeAuditModel struct {
	s    *stub
	rows []*model.VideoAuditLog
}

var _ model.VideoAuditLogModel = (*fakeAuditModel)(nil)

func (f *fakeAuditModel) Insert(ctx context.Context, tx sqlx.Session, log *model.VideoAuditLog) error {
	if err := f.s.enter("audit.Insert", "video_audit_log.Insert:%d:%d->%d", log.Aid, log.FromState, log.ToState); err != nil {
		return wrapf("video_audit_log", "Insert", err)
	}
	if tx == nil {
		return errors.New("fake: video_audit_log.Insert 必须与状态更新同事务（AGENTS.md §8）")
	}
	cp := *log
	cp.ID = nextFakePK(f.rows, func(r *model.VideoAuditLog) int64 { return r.ID })
	f.rows = append(f.rows, &cp)
	return nil
}

func (f *fakeAuditModel) ListByAid(ctx context.Context, aid int64) ([]*model.VideoAuditLog, error) {
	if err := f.s.enter("audit.ListByAid", "video_audit_log.ListByAid:%d", aid); err != nil {
		return nil, wrapf("video_audit_log", "ListByAid", err)
	}
	out := make([]*model.VideoAuditLog, 0, len(f.rows))
	for _, r := range f.rows {
		if r.Aid == aid {
			cp := *r
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *model.VideoAuditLog) int { return int(a.ID - b.ID) })
	return out, nil
}

// --- video_outbox 替身（对齐 model/videooutboxmodel.go:77-144） ---

// fakeOutboxModel 只实现 logic 路径上的 Insert：
// 读侧与位点方法（ListPending/MarkPublished/MarkRetry/MarkFailed）由 internal/publisher
// 用自己的替身覆盖，本包用例声称「logic 不碰它们」，所以真被调用就报错。
type fakeOutboxModel struct {
	s    *stub
	rows []*model.VideoOutbox
}

var _ model.VideoOutboxModel = (*fakeOutboxModel)(nil)

func (f *fakeOutboxModel) Insert(ctx context.Context, tx sqlx.Session, out *model.VideoOutbox) error {
	if err := f.s.enter("outbox.Insert", "video_outbox.Insert:%s", out.AggregateID); err != nil {
		return wrapf("video_outbox", "Insert", err)
	}
	if tx == nil {
		return errors.New("fake: video_outbox.Insert 必须与状态更新同事务（AGENTS.md §5）")
	}
	cp := *out
	cp.ID = nextFakePK(f.rows, func(r *model.VideoOutbox) int64 { return r.ID })
	// 与生产 INSERT 前的补默认值一致（videooutboxmodel.go:82-88）：
	// repository 不传 ctime/occurred_at，落库值由 model 现取。
	if cp.Ctime == 0 {
		cp.Ctime = time.Now().Unix()
	}
	cp.Mtime = cp.Ctime
	if cp.OccurredAt == 0 {
		cp.OccurredAt = cp.Ctime
	}
	f.rows = append(f.rows, &cp)
	return nil
}

func (f *fakeOutboxModel) ListPending(ctx context.Context, now int64, limit int32) ([]*model.VideoOutbox, error) {
	return nil, wrapf("video_outbox", "ListPending", errUnexpectedDependency)
}

func (f *fakeOutboxModel) MarkPublished(ctx context.Context, id, publishedAt int64) error {
	return wrapf("video_outbox", "MarkPublished", errUnexpectedDependency)
}

func (f *fakeOutboxModel) MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	return wrapf("video_outbox", "MarkRetry", errUnexpectedDependency)
}

func (f *fakeOutboxModel) MarkFailed(ctx context.Context, id int64, lastError string) error {
	return wrapf("video_outbox", "MarkFailed", errUnexpectedDependency)
}

// --- 缓存与事务连接替身 ---

// fakeCache 实现 repository.Cacher，只记录「失效了哪些 aid」。
// key 口径与 repository.keySubmission 一致（video:sub:%d），本替身把 key 存进 map，
// 以便断言失效确实作用在稿件详情键上而不是随手删了别的 key。
type fakeCache struct {
	s       *stub
	keys    map[string]int64 // 缓存 key → aid（只为证明 key 口径）
	dels    []int64
	pingErr error
}

var _ repository.Cacher = (*fakeCache)(nil)

func newFakeCache(s *stub) *fakeCache {
	return &fakeCache{s: s, keys: map[string]int64{}}
}

func cacheKeySubmission(aid int64) string { return fmt.Sprintf("video:sub:%d", aid) }

func (c *fakeCache) Ping(ctx context.Context) error {
	c.s.log.add("cache.Ping")
	return c.pingErr
}

func (c *fakeCache) DelSubmission(ctx context.Context, aid int64) error {
	if err := c.s.enter("cache.DelSubmission", "cache.DelSubmission:%d", aid); err != nil {
		return err
	}
	key := cacheKeySubmission(aid)
	c.keys[key] = aid
	c.dels = append(c.dels, aid)
	return nil
}

// txSession 只是「拿到了事务会话」的凭证；fake models 只判断它非 nil。
type txSession struct{ sqlx.Session }

// fakeConn 只实现 video 用到的 TransactCtx。其它方法落在嵌入的 nil 接口上会直接 panic，
// 等价于「logic 单测路径上不允许出现任何绕过 model 的直连 SQL」——真出现了就是越界。
type fakeConn struct {
	sqlx.SqlConn
	s            *stub
	transactions int
	rolledBack   int
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

func (f *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	f.transactions++
	// enter 让用例能在「事务即将开始」这一刻插队改库，复现校验与事务之间的并发窗口。
	if err := f.s.enter("db.TransactCtx", "db.TransactCtx"); err != nil {
		f.rolledBack++
		return err // 事务体没跑就被判定失败（复现「开启事务即报错」）
	}
	if err := fn(ctx, &txSession{}); err != nil {
		// 注意：**不回滚**。真实 MySQL 会回滚，本替身保留已写入的行，
		// 用来暴露「副作用跨语句分裂」；失败用例按「库里还剩什么」断言。
		f.rolledBack++
		return err
	}
	return nil
}

// --- 装配 ---

type store struct {
	s      *stub
	subs   *fakeSubmissionModel
	vers   *fakeVersionModel
	audits *fakeAuditModel
	outbox *fakeOutboxModel
	cache  *fakeCache
	conn   *fakeConn
	repo   *repository.Repository

	svcCtx *svc.ServiceContext
}

func newStore() *store {
	s := newStub()
	st := &store{
		s:      s,
		subs:   &fakeSubmissionModel{s: s},
		vers:   &fakeVersionModel{s: s},
		audits: &fakeAuditModel{s: s},
		outbox: &fakeOutboxModel{s: s},
		cache:  newFakeCache(s),
		conn:   &fakeConn{s: s},
	}
	st.repo = repository.NewWithDeps(st.cache, st.conn, st.subs, st.vers, st.audits, st.outbox)
	st.svcCtx = &svc.ServiceContext{Repository: st.repo}
	return st
}

// errBoom 是注入依赖故障用的哨兵错误：替身会像 model 一样把它 %w 包装后再抛出，
// 所以用例一律用 errors.Is 断言「原样透传」，不接受被换成别的哨兵。
var errBoom = errors.New("boom: 依赖故障")

// log 便捷取用。
func (st *store) log() *callLog { return st.s.log }

// fail 让某个依赖返回 errBoom（按 `<依赖>.<方法>` 粒度）。
func (st *store) fail(key string) { st.s.fail(key, errBoom) }

// failNth 让某个依赖的第 n 次调用返回 errBoom（1 起计数）。
func (st *store) failNth(key string, n int) { st.s.failNth(key, n, errBoom) }

// before 在某个依赖调用前插队改库，模拟并发写者。
func (st *store) before(key string, fn func()) { st.s.before(key, fn) }

// txCount 断言事务次数：一次状态推进应当且仅当一个事务。
func (st *store) txCount() int { return st.conn.transactions }

// dropSub 静默删掉一行稿件（复现「校验后稿件被别的入口删了」）。
func (st *store) dropSub(aid int64) {
	st.subs.rows = slices.DeleteFunc(st.subs.rows, func(r *model.VideoSubmission) bool { return r.Aid == aid })
}

// setState 静默改状态，模拟别的入口（如 media worker）在同一瞬间推进稿件。
func (st *store) setState(aid int64, state int32) {
	if cur := st.subs.find(aid); cur != nil {
		cur.State = state
	}
}

// setTitle 静默改正文，模拟别的入口在同一瞬间改了标题：
// 用来分辨事件取的是「事务内那次读」还是「logic 更早的首读」。
func (st *store) setTitle(aid int64, title string) {
	if cur := st.subs.find(aid); cur != nil {
		cur.Title = title
	}
}

// markChangedRows 打开真实 MySQL 的 changed-rows 行为（缺陷哨兵用例用）。
func (st *store) markChangedRows() { st.subs.changedRowsOnly = true }

// seedSub 静默布一行稿件（不写 callLog），aid 由自增分配后回写。
func (st *store) seedSub(s *model.VideoSubmission) *model.VideoSubmission {
	cp := *s
	if cp.Aid == 0 {
		cp.Aid = nextFakePK(st.subs.rows, func(r *model.VideoSubmission) int64 { return r.Aid })
	}
	st.subs.rows = append(st.subs.rows, &cp)
	out := cp
	return &out
}

// seedDraft 布一行常用稿件：title/desc/tag 非空、ctime 与 mtime 都是很久以前的 100，
// 这样「mtime 有没有被刷新」「ctime 有没有被误改」才有可断言的差异。
func (st *store) seedDraft(aid, mid int64, state int32) *model.VideoSubmission {
	return st.seedSub(&model.VideoSubmission{
		Aid: aid, Mid: mid, Title: fmt.Sprintf("t-%d", aid), Desc: fmt.Sprintf("d-%d", aid),
		Cover: fmt.Sprintf("c-%d", aid), Typeid: 11, Tag: fmt.Sprintf("g-%d", aid),
		State: state, Ctime: 100, Mtime: 100,
	})
}

// seedVersion 静默布一行版次。
func (st *store) seedVersion(v *model.VideoVersion) {
	cp := *v
	st.vers.rows = append(st.vers.rows, &cp)
}

// sub 读回库里那一行（副本），nil 表示不存在。
func (st *store) sub(aid int64) *model.VideoSubmission {
	cur := st.subs.find(aid)
	if cur == nil {
		return nil
	}
	cp := *cur
	return &cp
}

// auditRows 读回某稿件的全部审计行（按 id 升序）。
func (st *store) auditRows(aid int64) []*model.VideoAuditLog {
	out := make([]*model.VideoAuditLog, 0, len(st.audits.rows))
	for _, r := range st.audits.rows {
		if r.Aid == aid {
			cp := *r
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *model.VideoAuditLog) int { return int(a.ID - b.ID) })
	return out
}

// auditCount 全表审计行数（用来断「这次拒绝一个字节都没写」）。
func (st *store) auditCount() int { return len(st.audits.rows) }

// outboxRows 读回某稿件的全部事件行（按 id 升序）。
// 匹配键是 aggregate_id（信封里的 aid 十进制串），与 outbox 表的真实查询口径一致。
func (st *store) outboxRows(aid int64) []*model.VideoOutbox {
	key := strconv.FormatInt(aid, 10)
	out := make([]*model.VideoOutbox, 0, len(st.outbox.rows))
	for _, r := range st.outbox.rows {
		if r.AggregateID == key {
			cp := *r
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *model.VideoOutbox) int { return int(a.ID - b.ID) })
	return out
}

// outboxCount 全表事件行数。
func (st *store) outboxCount() int { return len(st.outbox.rows) }

// onlyOutboxRow 取某稿件唯一一行事件；行数不是 1 直接判失败，
// 免得后面的字段断言在 nil 上跑成假绿。
func (st *store) onlyOutboxRow(t *testing.T, aid int64) *model.VideoOutbox {
	t.Helper()
	rows := st.outboxRows(aid)
	if len(rows) != 1 {
		t.Fatalf("aid=%d 的事件行数 = %d, want 1（%+v）", aid, len(rows), rows)
	}
	return rows[0]
}

// --- rpc 投影小工具 ---

func submissionIDs(rows []*rpc.Submission) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetAid())
	}
	return out
}

// staleTime 是 seed 里的历史时间戳；任何被生产 SQL 刷新的时间字段都应 > 它。
const staleTime int64 = 100
