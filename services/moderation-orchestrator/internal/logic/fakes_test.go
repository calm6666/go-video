package logic

// fakes_test.go 是 moderation-orchestrator logic 测试的内存替身集合。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, 内存 taskMd/ruleMd/resultMd/appealMd) 组装**真实的 Repository**，
// 只把它的 5 个依赖换成替身——这样「缓存读穿/回填/失效、状态机 CAS（UpdateState 的
// fromStates 门槛）、结论 Upsert 的唯一键覆盖、申诉 state=0 门槛」整条链路都在被测路径上，
// 而不是把 Repository 也 mock 掉。见 internal/repository/repository.go 的 Cacher 注释。
//
// 四条替身纪律（catalog / rights / playback / comment 四轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 里 `t.State = ...`、`a.ID = appealID` 这类写回
//     不得污染库存行，否则「状态到底有没有落库」会被共享指针掩盖。
//  2. 副作用按真实 SQL 的口径处理：Insert 忽略入参主键、自增分配并返回；
//     moderation_task 的 uniq_business_submission 命中时走 `ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)`
//     语义（只回既有 id，**一列都不改**）；moderation_result 的 uniq_task 命中时整行覆盖；
//     appeal.Update 带 `AND state = 0`，未命中即 ErrAppealAlreadyHandled；
//     task.UpdateState 是 CAS，旧态不在 fromStates 内 ⇒ RowsAffected=0 ⇒ ErrInvalidStateTransition。
//  3. 副作用按**顺序**记录（callLog，格式 `<pkg>.<method>:<key>`），断言序列而不是只断言次数；
//     顺带记 ctx 传播（lostCtx）：替身收到的 ctx 若丢了用例塞进的标记值，就是「换成 Background 打库」。
//  4. 布数据走替身的**静默写入路径**（put / warm，不记 callLog）：布景不算被测调用，
//     因此轨迹断言可以直接从 0 开始数。
//
// Redis key 与 model SQL 语义的对齐方式：keyTask/keyResult/keyAppeal 与 repository 未导出常量
// 逐字复刻（`mod:task:%d`、`mod:result:%d`、`mod:appeal:%d`），Set/Del/Get 任一处键漂移即红；
// List 的 `pn<1→1`、`ps<1||ps>50→20`、`state/content_type/mid > 0` 才过滤、`ORDER BY id DESC`、
// `total==0 ⇒ 不发第二条 SELECT` 与 model/moderationmodel.go 同语义。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**，不证明 SQL 与列名本身；`model/*.go` 无单测，
//     仓库里也没有 moderation 的迁移↔model 列级对账门禁（见 README 已知缺口）；
//   - 唯一键冲突按「代码意图」模拟（fakeTaskModel.Insert 命中唯一键时不新增行），
//     真实 MySQL 的 `LAST_INSERT_ID(id)` 回读、`ON DUPLICATE` 是否保留 ctime 只能真机验证；
//   - 本服务**没有 Outbox、没有 consumer、没有事件生产**，所以「业务写与事件写是否同一事务」
//     这一项的结论是「根本没有事件写」（README 缺口 #1）；两步写的原子性只能断言残留的半截数据；
//   - `Cache` 真身（SetexCtx/Del/Get）没被测，TTL 60s 的实际行为不在断言范围内。

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

	"go-video/services/moderation-orchestrator/internal/config"
	"go-video/services/moderation-orchestrator/internal/repository"
	"go-video/services/moderation-orchestrator/internal/svc"
	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
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
		t.Fatalf("%s：错误 = nil, want %v", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

// wantErrMessage 断言错误的**文案**逐字等于 want。用于「两种完全不同的失败被压成同一个哨兵」
// 这类口径缺陷：errors.Is 分不出「申诉不存在」和「申诉已被别人处理」，文案才是唯一的线索。
func wantErrMessage(t *testing.T, label string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %q", label, want)
	}
	if err.Error() != want {
		t.Errorf("%s：错误 = %q, want %q", label, err.Error(), want)
	}
}

// wantNoErr 断言没有错误。
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

// wantIntsEQ 比较 int64 切片（返回顺序、分页集合类断言）。
func wantIntsEQ(t *testing.T, label, field string, got, want []int64) {
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

// wantCount 断言某类调用发生的次数（布数据走静默路径，所以数的是被测代码的调用）。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整序列 [%s]）", label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

// wantCtxCarried 断言每次依赖调用拿到的都是用例传入的 ctx（没被换成 context.Background()）。
func wantCtxCarried(t *testing.T, label string, log *callLog) {
	t.Helper()
	if len(log.lostCtx) != 0 {
		t.Errorf("%s：以下依赖调用丢失请求 ctx：%v", label, log.lostCtx)
	}
}

// assertAround 断言墙钟派生值落在期望附近：model 的 ctime/mtime 由未导出的 nowUnix()（time.Now）
// 生成且不可注入，跨秒抖动不可避免；需要精确相等的地方一律用 wantEQ。
func assertAround(t *testing.T, label, field string, got, want, slack int64) {
	t.Helper()
	if got < want-slack || got > want+slack {
		t.Errorf("%s：%s = %d, want %d±%d", label, field, got, want, slack)
	}
}

func nowUnix() int64 { return time.Now().Unix() }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func itoa32(n int32) string { return strconv.FormatInt(int64(n), 10) }

// --- 调用轨迹 ---

type ctxProbeKey struct{}

type callLog struct {
	ops     []string
	lostCtx []string
}

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

// call 记一次调用顺序，同时检查 ctx 是否还带着用例塞进去的标记值。
func (c *callLog) call(ctx context.Context, format string, args ...any) {
	op := fmt.Sprintf(format, args...)
	c.ops = append(c.ops, op)
	if ctx.Value(ctxProbeKey{}) == nil {
		c.lostCtx = append(c.lostCtx, op)
	}
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

// --- 错误注入（按方法粒度，见纪律 3） ---

type faultInjector struct{ by map[string]error }

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

func (f *faultInjector) fail(method string) error { return f.by[method] }

// --- 缓存替身 ---

// 与 repository 未导出常量逐字对齐；漂移时「失效了哪个键」的用例即红。
func keyTask(taskID int64) string     { return fmt.Sprintf("mod:task:%d", taskID) }
func keyResult(taskID int64) string   { return fmt.Sprintf("mod:result:%d", taskID) }
func keyAppeal(appealID int64) string { return fmt.Sprintf("mod:appeal:%d", appealID) }

type fakeCache struct {
	log *callLog
	faultInjector
	data map[string][]byte
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{log: log, data: map[string][]byte{}}
}

func (f *fakeCache) Ping(ctx context.Context) error {
	f.log.call(ctx, "cache.Ping")
	return f.fail("Ping")
}

func (f *fakeCache) get(ctx context.Context, op, key string) ([]byte, bool, error) {
	f.log.call(ctx, "cache.%s:%s", op, key)
	if err := f.fail(op); err != nil {
		return nil, false, err
	}
	bs, ok := f.data[key]
	if !ok {
		return nil, false, nil // 与真实 Cache 处理 redis.Nil 同口径：miss 返回 hit=false 且 err=nil
	}
	return slices.Clone(bs), true, nil
}

func (f *fakeCache) set(ctx context.Context, op, key string, payload []byte) error {
	f.log.call(ctx, "cache.%s:%s", op, key)
	if err := f.fail(op); err != nil {
		return err
	}
	f.data[key] = slices.Clone(payload)
	return nil
}

func (f *fakeCache) del(ctx context.Context, op, key string) error {
	f.log.call(ctx, "cache.%s:%s", op, key)
	if err := f.fail(op); err != nil {
		return err
	}
	delete(f.data, key)
	return nil
}

func (f *fakeCache) GetTask(ctx context.Context, taskID int64) ([]byte, bool, error) {
	return f.get(ctx, "GetTask", keyTask(taskID))
}
func (f *fakeCache) SetTask(ctx context.Context, taskID int64, payload []byte) error {
	return f.set(ctx, "SetTask", keyTask(taskID), payload)
}
func (f *fakeCache) DelTask(ctx context.Context, taskID int64) error {
	return f.del(ctx, "DelTask", keyTask(taskID))
}
func (f *fakeCache) GetResult(ctx context.Context, taskID int64) ([]byte, bool, error) {
	return f.get(ctx, "GetResult", keyResult(taskID))
}
func (f *fakeCache) SetResult(ctx context.Context, taskID int64, payload []byte) error {
	return f.set(ctx, "SetResult", keyResult(taskID), payload)
}
func (f *fakeCache) DelResult(ctx context.Context, taskID int64) error {
	return f.del(ctx, "DelResult", keyResult(taskID))
}
func (f *fakeCache) GetAppeal(ctx context.Context, appealID int64) ([]byte, bool, error) {
	return f.get(ctx, "GetAppeal", keyAppeal(appealID))
}
func (f *fakeCache) SetAppeal(ctx context.Context, appealID int64, payload []byte) error {
	return f.set(ctx, "SetAppeal", keyAppeal(appealID), payload)
}
func (f *fakeCache) DelAppeal(ctx context.Context, appealID int64) error {
	return f.del(ctx, "DelAppeal", keyAppeal(appealID))
}

// --- 静默布景（纪律 4：不记 callLog） ---

// warm 直接放一条缓存值，绕过被测的 Set 路径。
func (f *fakeCache) warm(key string, payload []byte) { f.data[key] = payload }

// warmTask / warmResult / warmAppeal 按 repository 的编码方式（jsonMustMarshal(model 结构)）布缓存。
func (f *fakeCache) warmTask(t *model.ModerationTask) { f.warm(keyTask(t.ID), mustJSON(t)) }
func (f *fakeCache) warmResult(r *model.ModerationResult) {
	f.warm(keyResult(r.TaskID), mustJSON(r))
}
func (f *fakeCache) warmAppeal(a *model.ModerationAppeal) {
	f.warm(keyAppeal(a.ID), mustJSON(a))
}

func (f *fakeCache) has(key string) bool { _, ok := f.data[key]; return ok }

func (f *fakeCache) raw(key string) []byte { return f.data[key] }

// mustJSON 复刻 repository.jsonMustMarshal 的编码口径（model 结构无 json tag ⇒ 键名取字段名）。
func mustJSON(v any) []byte {
	bs, err := json.Marshal(v)
	if err != nil {
		panic("mustJSON: " + err.Error())
	}
	return bs
}

// decodeTask 读缓存里实际存的任务，用于「回填的是不是刚查出来的那一行」。
func decodeTask(payload []byte) (model.ModerationTask, error) {
	var t model.ModerationTask
	err := json.Unmarshal(payload, &t)
	return t, err
}

func decodeResult(payload []byte) (model.ModerationResult, error) {
	var r model.ModerationResult
	err := json.Unmarshal(payload, &r)
	return r, err
}

func decodeAppeal(payload []byte) (model.ModerationAppeal, error) {
	var a model.ModerationAppeal
	err := json.Unmarshal(payload, &a)
	return a, err
}

// --- moderation_task 替身 ---

type fakeTaskModel struct {
	log *callLog
	faultInjector
	rows     map[int64]*model.ModerationTask
	next     int64
	advances []string // 成功推进的迁移 "<id>:<from>-><to>"，用于「只推进一次」断言
}

func (f *fakeTaskModel) Insert(ctx context.Context, t *model.ModerationTask) (int64, error) {
	f.log.call(ctx, "task.Insert:%s/%d", t.Business, t.SubmissionID)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	now := nowUnix()
	t.Ctime, t.Mtime = now, now // 真 model 在 Exec 之前无条件改写形参
	// uniq_business_submission 命中：ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)
	// ⇒ 只回既有主键，**不覆盖任何列**（含 state / ctime）。
	for _, id := range f.sortedIDs() {
		row := f.rows[id]
		if row.Business == t.Business && row.SubmissionID == t.SubmissionID {
			return row.ID, nil
		}
	}
	f.next++
	cp := *t
	cp.ID = f.next
	f.rows[cp.ID] = &cp
	return cp.ID, nil
}

func (f *fakeTaskModel) FindOne(ctx context.Context, taskID int64) (*model.ModerationTask, error) {
	f.log.call(ctx, "task.FindOne:%d", taskID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[taskID]
	if !ok {
		return nil, nil // 与 model 一致：sql.ErrNoRows ⇒ (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (f *fakeTaskModel) FindBySubmission(ctx context.Context, business string, submissionID int64) (*model.ModerationTask, error) {
	f.log.call(ctx, "task.FindBySubmission:%s/%d", business, submissionID)
	if err := f.fail("FindBySubmission"); err != nil {
		return nil, err
	}
	var best *model.ModerationTask
	for _, id := range f.descIDs() { // ORDER BY id DESC LIMIT 1
		row := f.rows[id]
		if row.Business != business || row.SubmissionID != submissionID {
			continue
		}
		if best == nil {
			cp := *row
			best = &cp
		}
	}
	return best, nil
}

// UpdateState 复刻 CAS 语义：旧态不在 fromStates 内（或任务不存在）⇒ RowsAffected=0
// ⇒ ErrInvalidStateTransition；空 fromStates 在打库前就被 model 直接拒。
func (f *fakeTaskModel) UpdateState(ctx context.Context, taskID int64, toState int32, fromStates ...int32) error {
	f.log.call(ctx, "task.UpdateState:%d->%d", taskID, toState)
	if len(fromStates) == 0 {
		return fmt.Errorf("moderation_task UpdateState: %w", model.ErrInvalidStateTransition)
	}
	if err := f.fail("UpdateState"); err != nil {
		return err
	}
	row, ok := f.rows[taskID]
	if !ok || !slices.Contains(fromStates, row.State) {
		return fmt.Errorf("moderation_task UpdateState: %w", model.ErrInvalidStateTransition)
	}
	f.advances = append(f.advances, fmt.Sprintf("%d:%d->%d", taskID, row.State, toState))
	row.State = toState
	row.Mtime = nowUnix()
	return nil
}

// List 复刻 model.List：pn/ps 归一、>0 才过滤、COUNT 与 SELECT 两跳、total==0 时不发 SELECT。
func (f *fakeTaskModel) List(ctx context.Context, mid int64, contentType, state, pn, ps int32) ([]*model.ModerationTask, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	args := fmt.Sprintf("%d/%d/%d/%d/%d", mid, contentType, state, pn, ps)
	f.log.call(ctx, "task.ListCount:%s", args)
	if err := f.fail("ListCount"); err != nil {
		return nil, 0, fmt.Errorf("moderation_task List count: %w", err)
	}
	var matched []*model.ModerationTask
	for _, id := range f.descIDs() { // ORDER BY id DESC
		row := f.rows[id]
		if mid > 0 && row.Mid != mid {
			continue
		}
		if contentType > 0 && row.ContentType != contentType {
			continue
		}
		if state > 0 && row.State != state {
			continue
		}
		matched = append(matched, row)
	}
	total := int32(len(matched))
	if total == 0 {
		return nil, 0, nil
	}
	f.log.call(ctx, "task.ListRows:%s", args)
	if err := f.fail("ListRows"); err != nil {
		return nil, 0, fmt.Errorf("moderation_task List list: %w", err)
	}
	offset := int((pn - 1) * ps)
	if offset >= len(matched) {
		return nil, total, nil
	}
	end := offset + int(ps)
	if end > len(matched) {
		end = len(matched)
	}
	out := make([]*model.ModerationTask, 0, end-offset)
	for _, row := range matched[offset:end] {
		cp := *row
		out = append(out, &cp)
	}
	return out, total, nil
}

// descIDs 主键降序，对齐 SQL 的 ORDER BY id DESC。
func (f *fakeTaskModel) descIDs() []int64 {
	ids := f.sortedIDs()
	slices.Reverse(ids)
	return ids
}

// sortedIDs 主键升序（map 遍历顺序不确定，替身必须先归一化再按 SQL 方向取）。
func (f *fakeTaskModel) sortedIDs() []int64 {
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// put 静默落一行（布数据用，不记 callLog、不分配主键）。
func (f *fakeTaskModel) put(t *model.ModerationTask) *model.ModerationTask {
	cp := *t
	f.rows[cp.ID] = &cp
	if cp.ID >= f.next {
		f.next = cp.ID
	}
	return &cp
}

// state 读库内当前状态（直接读库存，不记 callLog）。
func (f *fakeTaskModel) state(taskID int64) int32 {
	if row, ok := f.rows[taskID]; ok {
		return row.State
	}
	return -1
}

func (f *fakeTaskModel) count() int { return len(f.rows) }

// --- moderation_rule 替身（本服务没有任何 logic 路径读规则，见 README 缺口 #8） ---

type fakeRuleModel struct {
	log *callLog
	faultInjector
	rows   map[int64]*model.ModerationRule
	nextID int64
}

func (f *fakeRuleModel) Insert(ctx context.Context, r *model.ModerationRule) (int64, error) {
	f.log.call(ctx, "rule.Insert:%s", r.Name)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	f.nextID++
	r.ID = f.nextID
	cp := *r
	f.rows[cp.ID] = &cp
	return cp.ID, nil
}

func (f *fakeRuleModel) FindOne(ctx context.Context, ruleID int64) (*model.ModerationRule, error) {
	f.log.call(ctx, "rule.FindOne:%d", ruleID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[ruleID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeRuleModel) ListEnabled(ctx context.Context) ([]*model.ModerationRule, error) {
	f.log.call(ctx, "rule.ListEnabled")
	if err := f.fail("ListEnabled"); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	// WHERE state = 1 ORDER BY priority DESC, id ASC
	sort.Slice(ids, func(i, j int) bool {
		a, b := f.rows[ids[i]], f.rows[ids[j]]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return ids[i] < ids[j]
	})
	var out []*model.ModerationRule
	for _, id := range ids {
		if f.rows[id].State != model.RuleStateEnabled {
			continue
		}
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, nil
}

// --- moderation_result 替身 ---

type fakeResultModel struct {
	log *callLog
	faultInjector
	rows    map[int64]*model.ModerationResult // 按 task_id 索引（uniq_task）
	byPK    map[int64]*model.ModerationResult
	next    int64
	upserts []string // 每次 Upsert 的 "<taskID>:<verdict>"，用于「结论被覆盖几次」断言
}

// Upsert 复刻 INSERT ... ON DUPLICATE KEY UPDATE：命中 uniq_task 时整行覆盖
// verdict/reason/worker_id/reviewer/ctime，主键与 task_id 不变。
func (f *fakeResultModel) Upsert(ctx context.Context, r *model.ModerationResult) error {
	f.log.call(ctx, "result.Upsert:%d:%d", r.TaskID, r.Verdict)
	if err := f.fail("Upsert"); err != nil {
		return err
	}
	f.upserts = append(f.upserts, fmt.Sprintf("%d:%d", r.TaskID, r.Verdict))
	r.Ctime = nowUnix() // 真 model 无条件改写形参
	if row, ok := f.rows[r.TaskID]; ok {
		row.Verdict, row.Reason, row.WorkerID, row.Reviewer, row.Ctime =
			r.Verdict, r.Reason, r.WorkerID, r.Reviewer, r.Ctime
		return nil
	}
	f.next++
	cp := *r
	cp.ID = f.next
	f.rows[cp.TaskID] = &cp
	f.byPK[cp.ID] = &cp
	return nil
}

func (f *fakeResultModel) FindOne(ctx context.Context, taskID int64) (*model.ModerationResult, error) {
	f.log.call(ctx, "result.FindOne:%d", taskID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[taskID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// put 静默落一行结论（按 task_id 唯一定位）。
func (f *fakeResultModel) put(r *model.ModerationResult) *model.ModerationResult {
	cp := *r
	f.rows[cp.TaskID] = &cp
	f.byPK[cp.ID] = &cp
	if cp.ID >= f.next {
		f.next = cp.ID
	}
	return &cp
}

func (f *fakeResultModel) count(taskID int64) int {
	n := 0
	for _, row := range f.rows {
		if row.TaskID == taskID {
			n++
		}
	}
	return n
}

// --- moderation_appeal 替身 ---

type fakeAppealModel struct {
	log *callLog
	faultInjector
	rows map[int64]*model.ModerationAppeal
	next int64
}

func (f *fakeAppealModel) Insert(ctx context.Context, a *model.ModerationAppeal) (int64, error) {
	f.log.call(ctx, "appeal.Insert:%d", a.TaskID)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	now := nowUnix()
	a.Ctime, a.Mtime = now, now
	f.next++
	cp := *a
	cp.ID = f.next
	f.rows[cp.ID] = &cp
	return cp.ID, nil
}

func (f *fakeAppealModel) FindOne(ctx context.Context, appealID int64) (*model.ModerationAppeal, error) {
	f.log.call(ctx, "appeal.FindOne:%d", appealID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[appealID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// Update 复刻 `UPDATE ... SET state = 1 WHERE id = ? AND state = 0`：
// 申诉不存在或已处理 ⇒ RowsAffected=0 ⇒ ErrAppealAlreadyHandled（model 返回的是裸哨兵）。
func (f *fakeAppealModel) Update(ctx context.Context, appealID, handler int64, finalVerdict int32, finalReason string) error {
	f.log.call(ctx, "appeal.Update:%d", appealID)
	if err := f.fail("Update"); err != nil {
		return err
	}
	row, ok := f.rows[appealID]
	if !ok || row.State != appealStatePending {
		return model.ErrAppealAlreadyHandled
	}
	row.FinalVerdict, row.FinalReason, row.Handler = finalVerdict, finalReason, handler
	row.State = appealStateHandled
	row.Mtime = nowUnix()
	return nil
}

// put 静默落一行申诉。
func (f *fakeAppealModel) put(a *model.ModerationAppeal) *model.ModerationAppeal {
	cp := *a
	f.rows[cp.ID] = &cp
	if cp.ID >= f.next {
		f.next = cp.ID
	}
	return &cp
}

func (f *fakeAppealModel) countForTask(taskID int64) int {
	n := 0
	for _, row := range f.rows {
		if row.TaskID == taskID {
			n++
		}
	}
	return n
}

// --- 装配 ---

type store struct {
	log    *callLog
	cache  *fakeCache
	task   *fakeTaskModel
	rule   *fakeRuleModel
	result *fakeResultModel
	appeal *fakeAppealModel
	repo   *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	st := &store{
		log:   log,
		cache: newFakeCache(log),
		// 自增主键从 500（task）/ 700（appeal）/ 800（result 主键）起分配，
		// 所以布景用的 5xx/6xx/7xx 段 ID 不会和生成式用例拿到的主键撞上，轨迹里的 ID 才是确定的。
		task:   &fakeTaskModel{log: log, rows: map[int64]*model.ModerationTask{}, next: 500},
		rule:   &fakeRuleModel{log: log, rows: map[int64]*model.ModerationRule{}},
		result: &fakeResultModel{log: log, rows: map[int64]*model.ModerationResult{}, byPK: map[int64]*model.ModerationResult{}, next: 800},
		appeal: &fakeAppealModel{log: log, rows: map[int64]*model.ModerationAppeal{}, next: 700},
	}
	st.repo = repository.NewWithDeps(st.cache, st.task, st.rule, st.result, st.appeal)
	return st
}

// newTestSvc 装配只带内存依赖的 ServiceContext；与生产的差别仅在 Repository 的 5 个依赖是替身。
// 本服务 ServiceContext 没有下游 RPC/MQ 字段（worker 派发是占位，见 README 缺口 #2），故无需额外替身。
func newTestSvc(st *store) *svc.ServiceContext {
	return &svc.ServiceContext{Config: config.Config{}, Repository: st.repo}
}

// testCtx 带标记值的 ctx，供 wantCtxCarried 检查传播。
func testCtx() context.Context {
	return context.WithValue(context.Background(), ctxProbeKey{}, "probe")
}

// --- 布景（静默路径，见纪律 4） ---

// 状态桥接：model 侧是 int32 常量，rpc 侧是枚举类型；用例里两边都用名字，不写裸数字。
const (
	stUnspecified = int32(rpc.TaskState_TASK_STATE_UNSPECIFIED)
	stPending     = int32(rpc.TaskState_TASK_STATE_PENDING)
	stProcessing  = int32(rpc.TaskState_TASK_STATE_PROCESSING)
	stDone        = int32(rpc.TaskState_TASK_STATE_DONE)
	stAppealed    = int32(rpc.TaskState_TASK_STATE_APPEALED)
	stAppealDone  = int32(rpc.TaskState_TASK_STATE_APPEAL_DONE)
	stCanceled    = int32(rpc.TaskState_TASK_STATE_CANCELED)
	stGhost       = int32(7) // 枚举里没有的脏值（DB 列是 TINYINT，无 CHECK 约束）

	vUnspecified = int32(rpc.Verdict_VERDICT_UNSPECIFIED)
	vPass        = int32(rpc.Verdict_VERDICT_PASS)
	vReview      = int32(rpc.Verdict_VERDICT_REVIEW)
	vReject      = int32(rpc.Verdict_VERDICT_REJECT)
	vGhost       = int32(99)

	appealStatePending = int32(0)
	appealStateHandled = int32(1)

	ctVideo    = int32(rpc.ContentType_CONTENT_TYPE_VIDEO)
	ctComment  = int32(rpc.ContentType_CONTENT_TYPE_COMMENT)
	ctDanmaku  = int32(rpc.ContentType_CONTENT_TYPE_DANMAKU)
	ctCatalog  = int32(rpc.ContentType_CONTENT_TYPE_CATALOG)
	ctLive     = int32(rpc.ContentType_CONTENT_TYPE_LIVE)
	ctUnspec   = int32(rpc.ContentType_CONTENT_TYPE_UNSPECIFIED)
	ctGhost    = int32(42) // 枚举里没有的内容类型
	bizVideo   = "video.ugc"
	bizComment = "comment.reply"
	midAlice   = int64(30001) // 提交人 / 申诉人
	midBob     = int64(30002) // 另一个普通用户（越权申诉用）
	upCarol    = int64(40002) // UP 主
	opAdminDan = int64(9001)  // 运营（处理申诉）
	opAdminEve = int64(9002)  // 另一个运营
	workerOne  = int64(77)    // 机审实例 A
	workerTwo  = int64(88)    // 机审实例 B（越权回写用）
	subAlice   = int64(777001)
)

// seedTask 落一行任务，绕开 SubmitForReview 的守卫与唯一键检查；tune 用于逐用例改个别字段。
func seedTask(st *store, taskID int64, state int32, tune func(*model.ModerationTask)) *model.ModerationTask {
	row := &model.ModerationTask{
		ID:           taskID,
		SubmissionID: subAlice,
		ContentType:  ctVideo,
		Mid:          midAlice,
		UpMid:        upCarol,
		Business:     bizVideo,
		Reason:       "首发内容自审",
		State:        state,
		Operator:     0,
		Ctime:        1_700_000_000,
		Mtime:        1_700_000_060,
	}
	if tune != nil {
		tune(row)
	}
	st.task.put(row)
	return st.task.rows[taskID]
}

// seedResult 落一行审核结论（task_id 唯一）。
func seedResult(st *store, taskID int64, verdict int32, tune func(*model.ModerationResult)) *model.ModerationResult {
	row := &model.ModerationResult{
		ID:       taskID + 100,
		TaskID:   taskID,
		Verdict:  verdict,
		Reason:   "机审命中关键词表 v3",
		WorkerID: workerOne,
		Reviewer: 0,
		Ctime:    1_700_000_120,
	}
	if tune != nil {
		tune(row)
	}
	st.result.put(row)
	return st.result.rows[taskID]
}

// seedAppeal 落一行申诉。默认未处理（state=0）。
func seedAppeal(st *store, appealID, taskID int64, tune func(*model.ModerationAppeal)) *model.ModerationAppeal {
	row := &model.ModerationAppeal{
		ID:           appealID,
		TaskID:       taskID,
		Mid:          midAlice,
		Content:      "被误判为搬运，原创证明见附件",
		FinalVerdict: vUnspecified,
		FinalReason:  "",
		Handler:      0,
		State:        appealStatePending,
		Ctime:        1_700_000_200,
		Mtime:        1_700_000_200,
	}
	if tune != nil {
		tune(row)
	}
	st.appeal.put(row)
	return st.appeal.rows[appealID]
}

// --- 投影断言 helper（逐字段，辨识度值由 seed* 给定） ---

func wantTaskFields(t *testing.T, label string, got, want *rpc.Task) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s：Task = nil, want 非空", label)
	}
	wantEQ(t, label, "task_id", got.TaskId, want.TaskId)
	wantEQ(t, label, "submission_id", got.SubmissionId, want.SubmissionId)
	wantEQ(t, label, "content_type", got.ContentType, want.ContentType)
	wantEQ(t, label, "mid", got.Mid, want.Mid)
	wantEQ(t, label, "up_mid", got.UpMid, want.UpMid)
	wantEQ(t, label, "business", got.Business, want.Business)
	wantEQ(t, label, "reason", got.Reason, want.Reason)
	wantEQ(t, label, "state", got.State, want.State)
	wantEQ(t, label, "ctime", got.Ctime, want.Ctime)
	wantEQ(t, label, "mtime", got.Mtime, want.Mtime)
	wantEQ(t, label, "operator", got.Operator, want.Operator)
}

func wantResultFields(t *testing.T, label string, got, want *rpc.Result) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s：Result = nil, want 非空", label)
	}
	wantEQ(t, label, "task_id", got.TaskId, want.TaskId)
	wantEQ(t, label, "verdict", got.Verdict, want.Verdict)
	wantEQ(t, label, "reason", got.Reason, want.Reason)
	wantEQ(t, label, "worker_id", got.WorkerId, want.WorkerId)
	wantEQ(t, label, "reviewer", got.Reviewer, want.Reviewer)
	wantEQ(t, label, "ctime", got.Ctime, want.Ctime)
}

func wantAppealFields(t *testing.T, label string, got, want *rpc.Appeal) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s：Appeal = nil, want 非空", label)
	}
	wantEQ(t, label, "appeal_id", got.AppealId, want.AppealId)
	wantEQ(t, label, "task_id", got.TaskId, want.TaskId)
	wantEQ(t, label, "mid", got.Mid, want.Mid)
	wantEQ(t, label, "content", got.Content, want.Content)
	wantEQ(t, label, "final_verdict", got.FinalVerdict, want.FinalVerdict)
	wantEQ(t, label, "final_reason", got.FinalReason, want.FinalReason)
	wantEQ(t, label, "handler", got.Handler, want.Handler)
	wantEQ(t, label, "ctime", got.Ctime, want.Ctime)
	wantEQ(t, label, "mtime", got.Mtime, want.Mtime)
}

// taskRPCOf 把库存行按 convert.go 的口径映射成期望值，避免用例里手抄字段名时漏一项。
func taskRPCOf(row *model.ModerationTask) *rpc.Task {
	return &rpc.Task{
		TaskId: row.ID, SubmissionId: row.SubmissionID, ContentType: rpc.ContentType(row.ContentType),
		Mid: row.Mid, UpMid: row.UpMid, Business: row.Business, Reason: row.Reason,
		State: rpc.TaskState(row.State), Ctime: row.Ctime, Mtime: row.Mtime, Operator: row.Operator,
	}
}

func resultRPCOf(row *model.ModerationResult) *rpc.Result {
	return &rpc.Result{
		TaskId: row.TaskID, Verdict: rpc.Verdict(row.Verdict), Reason: row.Reason,
		WorkerId: row.WorkerID, Reviewer: row.Reviewer, Ctime: row.Ctime,
	}
}

func appealRPCOf(row *model.ModerationAppeal) *rpc.Appeal {
	return &rpc.Appeal{
		AppealId: row.ID, TaskId: row.TaskID, Mid: row.Mid, Content: row.Content,
		FinalVerdict: rpc.Verdict(row.FinalVerdict), FinalReason: row.FinalReason,
		Handler: row.Handler, Ctime: row.Ctime, Mtime: row.Mtime,
	}
}

// taskIDs 取响应里的任务主键顺序，用于排序/分页断言。
func taskIDs(tasks []*rpc.Task) []int64 {
	out := make([]int64, 0, len(tasks))
	for _, x := range tasks {
		out = append(out, x.TaskId)
	}
	return out
}
