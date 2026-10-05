// 本文件是 recommend-recall logic 包的测试替身集合（不产出任何生产代码）。
//
// 装配口径（与 services/user-profile、services/private-message 同一套做法）：
//   - logic 测试装的是**真实的** internal/repository.Repository 形状，只把最外层 IO
//     （六张自有表的 model、FeatureSource/VisibilitySource、sqlx.SqlConn）换成内存替身；
//   - 替身必须复刻真实 SQL 语义：唯一键冲突、ORDER BY 口径、条件 UPDATE 的影响行数、
//     参数校验哨兵，否则"通过"只证明替身自洽；
//   - 未覆盖的路径一律**显式失败**（返回 errNotWired 并记进调用序列），
//     不返回零值冒充成功 —— 零值会让用例假绿。
//
// 2026-09 增补（Publish/Rollback/Upsert/Prune 的写路径用例）：事务不再是禁区。
// TransactCtx 现在按"进入事务前快照、回调失败即还原"的方式给出**可回滚的**内存事务，
// 六个 model 的 session 版写方法逐条复刻生产 SQL 的 WHERE 条件与 RowsAffected 口径，
// 因此"失败后库里留下什么形态""CAS 未命中不得谎报成功"这两类断言测的是生产编排代码。
// 边界仍然要如实说：替身没有行锁与隔离级别，真并发无法在这里复现，
// 只能在生产代码两次读取之间注入一次"对手已提交"的外部变更（store.commitExternally），
// 由它检验复核/CAS 是否真的挡住；该变更不受本事务回滚影响（与真实 MySQL 一致）。
package logic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/idempotency"
	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

// 事务不再是禁区（Publish/Rollback/Upsert/Prune 的写路径用例依赖它）：
// TransactCtx 按"进入事务前快照、回调失败即还原"给出可回滚的内存事务，
// 因此"整事务回滚后库里留下什么形态"这类断言测的是生产编排代码，不是替身自洽。
var (
	// errNotWired 取代身未实现的方法。
	errNotWired = errors.New("test fake: method not wired for this case")
)

// --- 调用序列 ---

// callLog 记录一次用例里所有依赖调用（含关键入参）。
// 断言分两类：门禁拒绝时必须 total()==0；边界行为必须比对 from=0 起的全序列。
type callLog struct {
	entries []string
}

func (c *callLog) record(op string, args ...interface{}) {
	if len(args) == 0 {
		c.entries = append(c.entries, op)
		return
	}
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	c.entries = append(c.entries, op+"("+strings.Join(parts, ",")+")")
}

func (c *callLog) total() int { return len(c.entries) }

// ops 返回剥掉入参后的操作名序列（全序列断言用它，避免把入参细节抄进期望值）。
func (c *callLog) ops() []string {
	out := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		if i := strings.IndexByte(e, '('); i >= 0 {
			e = e[:i]
		}
		out = append(out, e)
	}
	return out
}

func (c *callLog) countOf(op string) int {
	n := 0
	for _, e := range c.ops() {
		if e == op {
			n++
		}
	}
	return n
}

func (c *callLog) has(op string) bool { return c.countOf(op) > 0 }

// assertOps 比对从 from 起的完整调用序列。
func assertOps(t *testing.T, c *callLog, from int, want ...string) {
	t.Helper()
	ops := c.ops()
	if from > len(ops) {
		t.Fatalf("调用序列只有 %d 项，无法从 %d 起比对: %v", len(ops), from, ops)
	}
	got := ops[from:]
	if len(got) != len(want) {
		t.Fatalf("调用序列长度=%d %v，期望 %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("调用序列第 %d 项=%q，期望 %q（完整序列 %v）", i, got[i], want[i], got)
		}
	}
}

// entryAt 取调用序列第 i 项的完整文本（含关键入参）。
// 上限类断言（"limit 传下去到底是多少""查的是哪个池"）靠它，而不是只数调用次数。
func entryAt(c *callLog, i int) string {
	if i >= len(c.entries) {
		return fmt.Sprintf("<没有第 %d 次调用，总次数=%d>", i, len(c.entries))
	}
	return c.entries[i]
}

// requireErrIs 断言错误链上带着指定哨兵（错误文本必须原样透传，不允许被吞）。
func requireErrIs(t *testing.T, err error, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误 %v，实际 nil", want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("期望错误链包含 %v，实际错误为 %v", want, err)
	}
}

// --- 内存库 ---

type rowKey struct {
	source  int32
	poolKey string
	version int64
}

type ptrKey struct {
	source  int32
	poolKey string
}

type claimKey struct {
	scope string
	key   string
}

// store 是六张自有表的内存状态。字段语义与 deploy/migrations/recommend-recall 一致。
type store struct {
	calls *callLog

	items    map[rowKey][]model.RecallPool
	versions map[rowKey]*model.RecallPoolVersion
	pointers map[ptrKey]*model.RecallPoolCurrent
	logs     []*model.RecallRequestLog
	claims   map[claimKey]*model.RecallIdempotency
	outbox   []*model.RecallOutbox

	nextID int64
	// writes 是累计写入类调用次数（AGENTS.md §5：召回读路径不得改任何计数/他域主数据）。
	// 断言一律按 baseline+delta，不假设初值为 0。
	writes int

	// 故障注入位（未设即正常路径）。
	errTopByVersion   error
	errListByVersion  error
	errVersionFindOne error
	errPointerFindOne error
	errPointerList    error
	errLogInsert      error
	errLogFind        error
	errClaim          error
	// errTopByKey 按 (source, pool_key) 注入单池故障：真实故障总是先打在某个池上，
	// 全局位会把所有路一起打倒，测不出"只丢这一路"的归因。
	errTopByKey map[readKey]error

	// 最近一次批量读入参（断言"读了哪些池"而不是只数调用次数）。
	lastListBySourcesPools []model.PoolKey
	lastListByRefs         []model.PoolVersionRef

	// 事务内的故障与并发注入位（键是替身方法名，如 "Current.Switch"）。
	// errOn 模拟"这条语句在存储层失败"；hooks 模拟"对手事务恰好在这一步提交"。
	errOn map[string]error
	hooks map[string]func(args ...interface{})
	// external 是事务进行中由 commitExternally 施加的**已提交**外部变更：
	// 回滚必须重放它们（MySQL 里别人已提交的行不会被我的 ROLLBACK 抹掉）。
	external []func()
}

func newStore() *store {
	return &store{
		calls:    &callLog{},
		items:    make(map[rowKey][]model.RecallPool),
		versions: make(map[rowKey]*model.RecallPoolVersion),
		pointers: make(map[ptrKey]*model.RecallPoolCurrent),
		claims:   make(map[claimKey]*model.RecallIdempotency),
		errOn:    make(map[string]error),
		hooks:    make(map[string]func(args ...interface{})),
	}
}

// failOn 让指定替身方法返回一次存储层故障（用例名即注入点，便于失败时定位）。
func (s *store) failOn(op string, err error) { s.errOn[op] = err }

// injected 读取注入的故障；包一层方法名前缀，与真实驱动的 %w 包装同形。
func (s *store) injected(op string) error {
	if err := s.errOn[op]; err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// runHook 在替身方法**改动内存库之前**触发钩子，用来施加一次"对手事务已提交"的外部变更。
// 位置很关键：生产代码是"读 → 判断 → 带条件的写"，只有把变更塞在读与写之间，
// 才能检验 CAS/复核是否真的挡住，而不是检验替身恰好返回了什么。
func (s *store) runHook(op string, args ...interface{}) {
	if fn := s.hooks[op]; fn != nil {
		fn(args...)
	}
}

// on 注册一个事务内钩子。
func (s *store) on(op string, fn func(args ...interface{})) { s.hooks[op] = fn }

// commitExternally 立刻施加一份变更并登记为"外部已提交"：
// 它不参与本事务的回滚（与真实 MySQL 一致），因此能区分
// "本事务的写入被回滚干净了"与"外部已提交的写入必须留下"。
func (s *store) commitExternally(apply func()) {
	apply()
	s.external = append(s.external, apply)
}

// storeSnapshot 是进入事务前各表行值的深拷贝（回滚目标）。
type storeSnapshot struct {
	items    map[rowKey][]model.RecallPool
	versions map[rowKey]model.RecallPoolVersion
	pointers map[ptrKey]model.RecallPoolCurrent
	logs     []model.RecallRequestLog
	claims   map[claimKey]model.RecallIdempotency
	outbox   []model.RecallOutbox
}

// snapshot 深拷贝行数据。刻意**不**拷贝 calls/writes/nextID：
// 写语句确实执行过（MySQL 回滚也不回收 AUTO_INCREMENT），调用序列是证据而不是状态。
func (s *store) snapshot() *storeSnapshot {
	snap := &storeSnapshot{
		items:    make(map[rowKey][]model.RecallPool, len(s.items)),
		versions: make(map[rowKey]model.RecallPoolVersion, len(s.versions)),
		pointers: make(map[ptrKey]model.RecallPoolCurrent, len(s.pointers)),
		claims:   make(map[claimKey]model.RecallIdempotency, len(s.claims)),
	}
	for k, rows := range s.items {
		cp := make([]model.RecallPool, len(rows))
		copy(cp, rows)
		snap.items[k] = cp
	}
	for k, v := range s.versions {
		snap.versions[k] = *v
	}
	for k, p := range s.pointers {
		snap.pointers[k] = *p
	}
	for k, c := range s.claims {
		snap.claims[k] = *c
	}
	snap.logs = make([]model.RecallRequestLog, 0, len(s.logs))
	for _, l := range s.logs {
		snap.logs = append(snap.logs, *l)
	}
	snap.outbox = make([]model.RecallOutbox, 0, len(s.outbox))
	for _, o := range s.outbox {
		snap.outbox = append(snap.outbox, *o)
	}
	return snap
}

// restore 把行数据还原到快照，且**原地覆写**已有行对象而不是换新指针：
// 用例常常持有 seedVersion/seedPointer 返回的指针做前后比对。
func (s *store) restore(snap *storeSnapshot) {
	for k := range s.items {
		if _, ok := snap.items[k]; !ok {
			delete(s.items, k)
		}
	}
	for k, want := range snap.items {
		cp := make([]model.RecallPool, len(want))
		copy(cp, want)
		s.items[k] = cp
	}
	restoreMap(s.versions, snap.versions)
	restoreMap(s.pointers, snap.pointers)
	restoreMap(s.claims, snap.claims)
	s.logs = restoreSlice(s.logs, snap.logs)
	s.outbox = restoreSlice(s.outbox, snap.outbox)
}

// restoreMap 还原一张"主键 -> 行指针"的表：
// 已有键就地覆写行值（保住用例持有的指针），快照里没有的键删掉，
// 事务里新增的键移除。
func restoreMap[K comparable, V any](cur map[K]*V, snap map[K]V) {
	for k := range cur {
		if _, ok := snap[k]; !ok {
			delete(cur, k)
		}
	}
	for k, v := range snap {
		if row, ok := cur[k]; ok {
			*row = v
			continue
		}
		cp := v
		cur[k] = &cp
	}
}

// restoreSlice 还原追加型表（recall_request_log / recall_outbox）：截断到快照长度后逐行覆写。
func restoreSlice[T any](cur []*T, snap []T) []*T {
	if len(cur) > len(snap) {
		cur = cur[:len(snap)]
	}
	for i, v := range snap {
		if i < len(cur) {
			*cur[i] = v
			continue
		}
		cp := v
		cur = append(cur, &cp)
	}
	return cur
}

// --- 回读断言件 ---
//
// 这些辅助一律返回**值**而不是指针：用例若拿着指针断言，回滚后读到的是同一份内存，
// 断言就会因为"看的是同一个对象"而恒真。

func (s *store) pointerOf(source int32, poolKey string) (model.RecallPoolCurrent, bool) {
	ptr, ok := s.pointers[ptrKey{source: source, poolKey: poolKey}]
	if !ok {
		return model.RecallPoolCurrent{}, false
	}
	return *ptr, true
}

func (s *store) versionOf(source int32, poolKey string, version int64) (model.RecallPoolVersion, bool) {
	row, ok := s.versions[rowKey{source: source, poolKey: poolKey, version: version}]
	if !ok {
		return model.RecallPoolVersion{}, false
	}
	return *row, true
}

func (s *store) versionRowOfID(id int64) (model.RecallPoolVersion, bool) {
	for _, row := range s.versions {
		if row.ID == id {
			return *row, true
		}
	}
	return model.RecallPoolVersion{}, false
}

// itemAids 返回某版本条目 aid（升序），断言"这一版到底写进去哪几条"。
func (s *store) itemAids(source int32, poolKey string, version int64) []int64 {
	rows := s.items[rowKey{source: source, poolKey: poolKey, version: version}]
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Aid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// itemCountOfVersion 是某版本条目行数（与 CountByVersion 同一口径）。
func (s *store) itemCountOfVersion(source int32, poolKey string, version int64) int {
	return len(s.items[rowKey{source: source, poolKey: poolKey, version: version}])
}

// totalItemRows 是全表条目行数：清理用例用它断言"没顺手删掉别的版本"。
func (s *store) totalItemRows() int {
	n := 0
	for _, rows := range s.items {
		n += len(rows)
	}
	return n
}

// versionRowCount 是全表登记行数（清理用例断言"只删了点名的那一行"）。
func (s *store) versionRowCount() int { return len(s.versions) }

func (s *store) claimOf(scope, key string) (model.RecallIdempotency, bool) {
	row, ok := s.claims[claimKey{scope: scope, key: key}]
	if !ok {
		return model.RecallIdempotency{}, false
	}
	return *row, true
}

func (s *store) logCount() int { return len(s.logs) }

func (s *store) outboxOf() []model.RecallOutbox {
	out := make([]model.RecallOutbox, 0, len(s.outbox))
	for _, row := range s.outbox {
		out = append(out, *row)
	}
	return out
}

// item 是一条池条目入参。
type item struct {
	aid   int64
	score float64
}

// seedItems 播种 recall_pool 条目。**入参顺序刻意不参与排序**：
// 读取顺序由替身按真实 SQL 的 ORDER BY score DESC, aid ASC 重算，
// 用例把入参顺序打乱即可证明排序不是"按插入顺序返回"。
func (s *store) seedItems(source int32, poolKey string, version int64, items ...item) {
	key := rowKey{source: source, poolKey: poolKey, version: version}
	for i, it := range items {
		s.items[key] = append(s.items[key], model.RecallPool{
			ID: s.nextRowID(), Source: source, PoolKey: poolKey, Version: version,
			Aid: it.aid, Score: it.score, Ctime: 1700000000 + int64(i), Mtime: 1700000000 + int64(i),
		})
	}
}

// seedVersion 播种 recall_pool_version 登记行。itemCount 刻意与条目数解耦：
// GetPoolSnapshot 的 has_more 读的是登记值（已发布版本不可变，登记值即事实），
// 用条目数替代会掩盖"登记值没回填"这类缺陷。
func (s *store) seedVersion(source int32, poolKey string, version int64, batchID string,
	state int32, itemCount int64) *model.RecallPoolVersion {
	row := &model.RecallPoolVersion{
		ID: s.nextRowID(), Source: source, PoolKey: poolKey, Version: version,
		BatchID: batchID, Generator: "fake-job", SchemaVersion: model.PoolVersionSchemaVersion,
		ItemCount: itemCount, State: state, PublishedAt: 0,
		Operator: "fake-operator", Note: "fake note", Ctime: 1700000000, Mtime: 1700000000,
	}
	s.versions[rowKey{source: source, poolKey: poolKey, version: version}] = row
	return row
}

// seedPointer 播种 recall_pool_current 指针行（在线读的唯一权威）。
func (s *store) seedPointer(source int32, poolKey string, version int64, batchID string,
	publishedAt int64) *model.RecallPoolCurrent {
	ptr := &model.RecallPoolCurrent{
		Source: source, PoolKey: poolKey, Version: version, BatchID: batchID,
		PreviousVersion: 0, SwitchCount: 1, Operator: "fake-operator", Note: "fake switch",
		PublishedAt: publishedAt, Ctime: 1700000000, Mtime: 1700000000,
	}
	s.pointers[ptrKey{source: source, poolKey: poolKey}] = ptr
	return ptr
}

// seedRequestLog 播种一行召回审计（读用例的前置数据）。
func (s *store) seedRequestLog(l *model.RecallRequestLog) *model.RecallRequestLog {
	row := *l
	if row.ID == 0 {
		row.ID = s.nextRowID()
	}
	s.logs = append(s.logs, &row)
	return &row
}

func (s *store) nextRowID() int64 {
	s.nextID++
	return s.nextID
}

func (s *store) trap(op string) error {
	s.calls.record(op, "TRAP")
	return fmt.Errorf("%s: %w", op, errNotWired)
}

// sortedPoolRows 复刻 recall_pool 的读取口径：score DESC, aid ASC。
func sortedPoolRows(rows []model.RecallPool) []model.RecallPool {
	out := make([]model.RecallPool, len(rows))
	copy(out, rows)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Aid < out[j].Aid
	})
	return out
}

// checkPoolReadArgs 复刻 model.checkPoolQueryArgs（本包只能用到导出的校验件）。
func checkPoolReadArgs(source int32, poolKey string, version int64, limit int) error {
	if !model.ValidSource(source) {
		return model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return err
	}
	if version <= 0 {
		return model.ErrInvalidVersion
	}
	return model.CheckLimit(limit, model.MaxPoolQueryLimit)
}

// --- recall_pool 替身 ---

type fakePool struct{ s *store }

func (f *fakePool) TopByVersion(ctx context.Context, source int32, poolKey string,
	version int64, limit int) ([]*model.RecallPool, error) {
	f.s.calls.record("Pool.TopByVersion", fmt.Sprintf("%d|%s|%d|%d", source, poolKey, version, limit))
	if err := checkPoolReadArgs(source, poolKey, version, limit); err != nil {
		return nil, err
	}
	if err := f.s.errTopByVersion; err != nil {
		return nil, fmt.Errorf("recall_pool TopByVersion: %w", err)
	}
	if err := f.s.errTopByKey[readKey{source: source, poolKey: poolKey}]; err != nil {
		return nil, fmt.Errorf("recall_pool TopByVersion: %w", err)
	}
	rows := sortedPoolRows(f.s.items[rowKey{source: source, poolKey: poolKey, version: version}])
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]*model.RecallPool, 0, len(rows))
	for i := range rows {
		row := rows[i]
		out = append(out, &row)
	}
	return out, nil
}

func (f *fakePool) ListByVersion(ctx context.Context, source int32, poolKey string,
	version int64, offset, limit int) ([]*model.RecallPool, error) {
	f.s.calls.record("Pool.ListByVersion", fmt.Sprintf("%d|%s|%d|%d|%d", source, poolKey, version, offset, limit))
	if err := checkPoolReadArgs(source, poolKey, version, limit); err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > model.MaxPoolSnapshotOffset {
		return nil, model.ErrPageTooDeep
	}
	if err := f.s.errListByVersion; err != nil {
		return nil, fmt.Errorf("recall_pool ListByVersion: %w", err)
	}
	rows := sortedPoolRows(f.s.items[rowKey{source: source, poolKey: poolKey, version: version}])
	if offset >= len(rows) {
		return nil, nil
	}
	rows = rows[offset:]
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]*model.RecallPool, 0, len(rows))
	for i := range rows {
		row := rows[i]
		out = append(out, &row)
	}
	return out, nil
}

func (f *fakePool) CountByVersion(ctx context.Context, source int32, poolKey string, version int64) (int64, error) {
	f.s.calls.record("Pool.CountByVersion", fmt.Sprintf("%d|%s|%d", source, poolKey, version))
	if !model.ValidSource(source) {
		return 0, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return 0, err
	}
	if version <= 0 {
		return 0, model.ErrInvalidVersion
	}
	return int64(len(f.s.items[rowKey{source: source, poolKey: poolKey, version: version}])), nil
}

func (f *fakePool) BatchUpsert(context.Context, int32, string, int64, []model.PoolItemInput) (int64, error) {
	return 0, f.s.trap("Pool.BatchUpsert")
}

// checkPoolWriteArgs 复刻 model.batchUpsert/countByVersion 的校验前缀（无 limit 一栏）。
func checkPoolWriteArgs(source int32, poolKey string, version int64) error {
	if !model.ValidSource(source) {
		return model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return err
	}
	if version <= 0 {
		return model.ErrInvalidVersion
	}
	return nil
}

// requireTxSession 断言写语句确实落在事务里。
// 生产模型对 nil session 会退回全局连接自动提交（这正是"版本已封版、条目还在追加"的来源），
// 替身对它的真实后果没有更忠实的模拟，只能拒绝执行：logic 给 InTx 方法传 nil 时必须红。
func requireTxSession(op string, session sqlx.Session) error {
	if session == nil {
		return fmt.Errorf("%s: 写语句落在事务外（session=nil）: %w", op, errNotWired)
	}
	return nil
}

// BatchUpsertInTx 逐条复刻 model.batchUpsert 的 ON DUPLICATE KEY UPDATE 语义。
//
// RowsAffected 口径是 MySQL 的而非直觉的：新插入=1、值完全未变=0、更新=2，
// 所以"重放同一批"返回 0 或 2*n 而不是"失败"，调用方不得据此判定写入失败；
// 本替身如实算这个数，用例才能钉住"logic 有没有把影响行数当条数用"。
func (f *fakePool) BatchUpsertInTx(ctx context.Context, session sqlx.Session, source int32,
	poolKey string, version int64, items []model.PoolItemInput) (int64, error) {
	f.s.calls.record("Pool.BatchUpsertInTx", fmt.Sprintf("%d|%s|%d|%d", source, poolKey, version, len(items)))
	if err := checkPoolWriteArgs(source, poolKey, version); err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	if len(items) > model.MaxPoolItemBatch {
		return 0, model.ErrTooManyItems
	}
	for _, it := range items {
		if it.Aid <= 0 {
			return 0, model.ErrInvalidAid
		}
	}
	if err := requireTxSession("Pool.BatchUpsertInTx", session); err != nil {
		return 0, err
	}
	if err := f.s.injected("Pool.BatchUpsertInTx"); err != nil {
		return 0, err
	}
	key := rowKey{source: source, poolKey: poolKey, version: version}
	now := time.Now().Unix()
	var affected int64
	for _, it := range items {
		exists := false
		for i := range f.s.items[key] {
			if f.s.items[key][i].Aid != it.Aid {
				continue
			}
			exists = true
			row := &f.s.items[key][i]
			switch {
			case row.Score == it.Score:
				// 值完全未变：ODKU 命中但不改数据，RowsAffected=0。
			default:
				row.Score, row.Mtime = it.Score, now
				affected += 2
			}
			break
		}
		if !exists {
			f.s.items[key] = append(f.s.items[key], model.RecallPool{
				ID: f.s.nextRowID(), Source: source, PoolKey: poolKey, Version: version,
				Aid: it.Aid, Score: it.Score, Ctime: now, Mtime: now,
			})
			affected++
		}
	}
	f.s.writes++
	return affected, nil
}

// CountByVersionInTx 与 CountByVersion 同口径，但必须在事务内：
// 用事务外连接读会漏掉本批刚写的条目，于是 item_count 登记成假事实。
func (f *fakePool) CountByVersionInTx(ctx context.Context, session sqlx.Session, source int32,
	poolKey string, version int64) (int64, error) {
	f.s.calls.record("Pool.CountByVersionInTx", fmt.Sprintf("%d|%s|%d", source, poolKey, version))
	if err := checkPoolWriteArgs(source, poolKey, version); err != nil {
		return 0, err
	}
	if err := requireTxSession("Pool.CountByVersionInTx", session); err != nil {
		return 0, err
	}
	if err := f.s.injected("Pool.CountByVersionInTx"); err != nil {
		return 0, err
	}
	return int64(len(f.s.items[rowKey{source: source, poolKey: poolKey, version: version}])), nil
}

func (f *fakePool) DeleteByVersions(context.Context, int32, string, []int64, int64) (int64, bool, error) {
	return 0, false, f.s.trap("Pool.DeleteByVersions")
}

// DeleteByVersionsInTx 复刻 "DELETE ... WHERE version IN (...) LIMIT ?"：
// 真实 MySQL 的 LIMIT 不保证删的是哪几行，但保证"至多 maxRows 行、删满即可能还有剩余"。
// 替身按 (version, id) 升序取前 maxRows 行，把不确定性收敛成可断言的确定形态；
// has_more 用 deleted>=maxRows 判定，与生产同一式。
func (f *fakePool) DeleteByVersionsInTx(ctx context.Context, session sqlx.Session, source int32,
	poolKey string, versions []int64, maxRows int64) (int64, bool, error) {
	f.s.calls.record("Pool.DeleteByVersionsInTx", fmt.Sprintf("%d|%s|%v|%d", source, poolKey, versions, maxRows))
	if len(versions) == 0 {
		return 0, false, nil
	}
	if err := checkPoolWriteArgs0(source, poolKey); err != nil {
		return 0, false, err
	}
	for _, v := range versions {
		if v <= 0 {
			return 0, false, model.ErrInvalidVersion
		}
	}
	if err := model.CheckInt64Limit(maxRows, model.MaxDeleteRows); err != nil {
		return 0, false, err
	}
	if err := requireTxSession("Pool.DeleteByVersionsInTx", session); err != nil {
		return 0, false, err
	}
	f.s.runHook("Pool.DeleteByVersionsInTx", source, poolKey, versions, maxRows)
	if err := f.s.injected("Pool.DeleteByVersionsInTx"); err != nil {
		return 0, false, err
	}
	type target struct {
		version int64
		id      int64
	}
	var rows []target
	for _, v := range versions {
		for _, row := range f.s.items[rowKey{source: source, poolKey: poolKey, version: v}] {
			rows = append(rows, target{version: v, id: row.ID})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].version != rows[j].version {
			return rows[i].version < rows[j].version
		}
		return rows[i].id < rows[j].id
	})
	if int64(len(rows)) > maxRows {
		rows = rows[:maxRows]
	}
	remove := make(map[int64]struct{}, len(rows))
	for _, t := range rows {
		remove[t.id] = struct{}{}
	}
	for _, v := range versions {
		key := rowKey{source: source, poolKey: poolKey, version: v}
		kept := make([]model.RecallPool, 0, len(f.s.items[key]))
		for _, row := range f.s.items[key] {
			if _, drop := remove[row.ID]; drop {
				continue
			}
			kept = append(kept, row)
		}
		if len(kept) == 0 {
			delete(f.s.items, key)
			continue
		}
		f.s.items[key] = kept
	}
	f.s.writes++
	deleted := int64(len(rows))
	return deleted, deleted >= maxRows, nil
}

// checkPoolWriteArgs0 是不带版本校验的写入参数检查（DeleteByVersions 只校验 source/pool_key）。
func checkPoolWriteArgs0(source int32, poolKey string) error {
	if !model.ValidSource(source) {
		return model.ErrInvalidSource
	}
	return model.ValidatePoolKey(source, poolKey)
}

// --- recall_pool_version 替身 ---

type fakePoolVersion struct{ s *store }

func (f *fakePoolVersion) FindOne(ctx context.Context, source int32, poolKey string,
	version int64) (*model.RecallPoolVersion, error) {
	f.s.calls.record("PoolVersion.FindOne", fmt.Sprintf("%d|%s|%d", source, poolKey, version))
	if !model.ValidSource(source) {
		return nil, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if version <= 0 {
		return nil, model.ErrInvalidVersion
	}
	if err := f.s.errVersionFindOne; err != nil {
		return nil, fmt.Errorf("recall_pool_version FindOne: %w", err)
	}
	row, ok := f.s.versions[rowKey{source: source, poolKey: poolKey, version: version}]
	if !ok {
		return nil, model.ErrVersionNotFound
	}
	cp := *row
	return &cp, nil
}

// ListByPool 复刻真实语义：state<>RETIRED 的过滤在读取侧做，排序是 version DESC。
func (f *fakePoolVersion) ListByPool(ctx context.Context, source int32, poolKey string,
	includeRetired bool, limit int) ([]*model.RecallPoolVersion, error) {
	f.s.calls.record("PoolVersion.ListByPool", fmt.Sprintf("%d|%s|%t|%d", source, poolKey, includeRetired, limit))
	if !model.ValidSource(source) {
		return nil, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if err := model.CheckLimit(limit, model.MaxVersionListLimit); err != nil {
		return nil, err
	}
	var rows []*model.RecallPoolVersion
	for _, v := range f.s.versions {
		if v.Source != source || v.PoolKey != poolKey {
			continue
		}
		if !includeRetired && v.State == model.VersionStateRetired {
			continue
		}
		cp := *v
		rows = append(rows, &cp)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Version > rows[j].Version })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (f *fakePoolVersion) ListByRefs(ctx context.Context, refs []model.PoolVersionRef,
	limit int) ([]*model.RecallPoolVersion, error) {
	f.s.calls.record("PoolVersion.ListByRefs", len(refs))
	f.s.lastListByRefs = append([]model.PoolVersionRef(nil), refs...)
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > model.MaxPoolRefsPerQuery {
		return nil, model.ErrTooManyPools
	}
	if err := model.CheckLimit(limit, model.MaxPoolRefsPerQuery); err != nil {
		return nil, err
	}
	out := make([]*model.RecallPoolVersion, 0, len(refs))
	for _, r := range refs {
		row, ok := f.s.versions[rowKey{source: r.Source, poolKey: r.PoolKey, version: r.Version}]
		if !ok {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	return out, nil
}

// PrunableBefore 复刻真实 SQL：ORDER BY version DESC LIMIT 1 OFFSET keep-1，
// 且刻意不过滤状态（状态保护在 ListPrunable 与 logic 的复核里）。
func (f *fakePoolVersion) PrunableBefore(ctx context.Context, source int32, poolKey string,
	keepVersions int32) (int64, error) {
	f.s.calls.record("PoolVersion.PrunableBefore", fmt.Sprintf("%d|%s|%d", source, poolKey, keepVersions))
	if keepVersions < 1 {
		return 0, model.ErrKeepVersionsTooSmall
	}
	if !model.ValidSource(source) {
		return 0, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return 0, err
	}
	versions := make([]int64, 0, 8)
	for k, v := range f.s.versions {
		if k.source == source && k.poolKey == poolKey {
			versions = append(versions, v.Version)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] > versions[j] })
	at := int(keepVersions) - 1
	if at >= len(versions) {
		return 0, nil
	}
	return versions[at], nil
}

func (f *fakePoolVersion) ListPrunable(ctx context.Context, source int32, poolKey string,
	belowVersion int64, limit int) ([]*model.RecallPoolVersion, error) {
	f.s.calls.record("PoolVersion.ListPrunable", fmt.Sprintf("%d|%s|%d|%d", source, poolKey, belowVersion, limit))
	if !model.ValidSource(source) {
		return nil, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if belowVersion <= 0 {
		return nil, nil
	}
	if err := model.CheckLimit(limit, model.MaxVersionListLimit); err != nil {
		return nil, err
	}
	var rows []*model.RecallPoolVersion
	for _, v := range f.s.versions {
		if v.Source != source || v.PoolKey != poolKey {
			continue
		}
		if v.Version >= belowVersion {
			continue
		}
		if v.State != model.VersionStateRetired && v.State != model.VersionStateFailed {
			continue
		}
		cp := *v
		rows = append(rows, &cp)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Version < rows[j].Version })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// Register 复刻 model.Register 的校验顺序与 ODKU 列集：
// 只刷新 generator/schema_version/operator/note/mtime，**不**覆盖已登记的 batch_id/state/item_count
// —— 批次归属是本表的核心不变量。登记后再读回比对批次，不同即 ErrVersionReuseBlocked。
// 真实实现的 INSERT 是自动提交还是随事务提交取决于 session，这里同样尊重 session。
func (f *fakePoolVersion) Register(ctx context.Context, session sqlx.Session, v *model.RecallPoolVersion) error {
	f.s.calls.record("PoolVersion.Register", fmt.Sprintf("%d|%s|%d|%s|%d",
		v.Source, v.PoolKey, v.Version, v.BatchID, v.State))
	if err := checkVersionRegisterArgs(v); err != nil {
		return err
	}
	if err := requireTxSession("PoolVersion.Register", session); err != nil {
		return err
	}
	if err := f.s.injected("PoolVersion.Register"); err != nil {
		return err
	}
	key := rowKey{source: v.Source, poolKey: v.PoolKey, version: v.Version}
	now := time.Now().Unix()
	f.s.writes++
	if exist, ok := f.s.versions[key]; ok {
		// ON DUPLICATE KEY UPDATE：只改可变元数据。
		exist.Generator = v.Generator
		exist.SchemaVersion = v.SchemaVersion
		exist.Operator = v.Operator
		exist.Note = v.Note
		exist.Mtime = now
		if exist.BatchID != v.BatchID {
			return model.ErrVersionReuseBlocked
		}
		return nil
	}
	row := *v
	row.ID = f.s.nextRowID()
	row.Ctime, row.Mtime = now, now
	f.s.versions[key] = &row
	return nil
}

// checkVersionRegisterArgs 复刻 model.Register 的入参检查（含两处默认值补齐）。
func checkVersionRegisterArgs(v *model.RecallPoolVersion) error {
	if !model.ValidSource(v.Source) {
		return model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(v.Source, v.PoolKey); err != nil {
		return err
	}
	if v.Version <= 0 {
		return model.ErrInvalidVersion
	}
	if strings.TrimSpace(v.BatchID) == "" {
		return model.ErrBatchIDRequired
	}
	if v.State == 0 {
		v.State = model.VersionStateBuilding
	}
	if !model.ValidVersionState(v.State) {
		return model.ErrInvalidVersionState
	}
	if v.SchemaVersion == 0 {
		v.SchemaVersion = model.PoolVersionSchemaVersion
	}
	return nil
}

// FindForUpdate 是发布/封版路径的版本行读取：脱离事务一律拒绝（生产同款）。
// 钩子跑在返回之前，用例借此把"对手已把这一行推到别的状态"塞进读与判断之间。
func (f *fakePoolVersion) FindForUpdate(ctx context.Context, session sqlx.Session, source int32,
	poolKey string, version int64) (*model.RecallPoolVersion, error) {
	f.s.calls.record("PoolVersion.FindForUpdate", fmt.Sprintf("%d|%s|%d", source, poolKey, version))
	if session == nil {
		return nil, fmt.Errorf("recall_pool_version FindForUpdate: %w", model.ErrSwitchConflict)
	}
	if err := checkPoolWriteArgs(source, poolKey, version); err != nil {
		return nil, err
	}
	f.s.runHook("PoolVersion.FindForUpdate", source, poolKey, version)
	if err := f.s.injected("PoolVersion.FindForUpdate"); err != nil {
		return nil, err
	}
	row, ok := f.s.versions[rowKey{source: source, poolKey: poolKey, version: version}]
	if !ok {
		return nil, model.ErrVersionNotFound
	}
	cp := *row
	return &cp, nil
}

// FindCurrent 读 state=CURRENT 的镜像行；没有则 ErrPoolNotFound（生产同款）。
func (f *fakePoolVersion) FindCurrent(ctx context.Context, source int32,
	poolKey string) (*model.RecallPoolVersion, error) {
	f.s.calls.record("PoolVersion.FindCurrent", fmt.Sprintf("%d|%s", source, poolKey))
	if !model.ValidSource(source) {
		return nil, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	row := f.currentMirror(source, poolKey)
	if row == nil {
		return nil, model.ErrPoolNotFound
	}
	cp := *row
	return &cp, nil
}

// FindCurrentForUpdate 与 FindCurrent 的差别在生产里写得很清楚：
// 没有 CURRENT 行返回 (nil, nil) 而不是 ErrPoolNotFound —— 首次上线没有旧版本可退役。
func (f *fakePoolVersion) FindCurrentForUpdate(ctx context.Context, session sqlx.Session, source int32,
	poolKey string) (*model.RecallPoolVersion, error) {
	f.s.calls.record("PoolVersion.FindCurrentForUpdate", fmt.Sprintf("%d|%s", source, poolKey))
	if session == nil {
		return nil, fmt.Errorf("recall_pool_version FindCurrentForUpdate: %w", model.ErrSwitchConflict)
	}
	if !model.ValidSource(source) {
		return nil, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	f.s.runHook("PoolVersion.FindCurrentForUpdate", source, poolKey)
	row := f.currentMirror(source, poolKey)
	if row == nil {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// currentMirror 取 state=CURRENT 且 version 最大的登记行（ORDER BY version DESC LIMIT 1）。
func (f *fakePoolVersion) currentMirror(source int32, poolKey string) *model.RecallPoolVersion {
	var best *model.RecallPoolVersion
	for _, v := range f.s.versions {
		if v.Source != source || v.PoolKey != poolKey || v.State != model.VersionStateCurrent {
			continue
		}
		if best == nil || v.Version > best.Version {
			best = v
		}
	}
	return best
}

func (f *fakePoolVersion) FindByID(ctx context.Context, id int64) (*model.RecallPoolVersion, error) {
	f.s.calls.record("PoolVersion.FindByID", id)
	if id <= 0 {
		return nil, model.ErrInvalidVersion
	}
	for _, row := range f.s.versions {
		if row.ID != id {
			continue
		}
		cp := *row
		return &cp, nil
	}
	return nil, model.ErrVersionNotFound
}

func (f *fakePoolVersion) FindByBatch(ctx context.Context, source int32, poolKey,
	batchID string) (*model.RecallPoolVersion, error) {
	f.s.calls.record("PoolVersion.FindByBatch", fmt.Sprintf("%d|%s|%s", source, poolKey, batchID))
	if !model.ValidSource(source) {
		return nil, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if strings.TrimSpace(batchID) == "" {
		return nil, model.ErrBatchIDRequired
	}
	for _, row := range f.s.versions {
		if row.Source == source && row.PoolKey == poolKey && row.BatchID == batchID {
			cp := *row
			return &cp, nil
		}
	}
	return nil, model.ErrVersionNotFound
}

func (f *fakePoolVersion) MaxVersion(ctx context.Context, source int32, poolKey string) (int64, error) {
	f.s.calls.record("PoolVersion.MaxVersion", fmt.Sprintf("%d|%s", source, poolKey))
	if !model.ValidSource(source) {
		return 0, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return 0, err
	}
	var max int64
	for _, v := range f.s.versions {
		if v.Source == source && v.PoolKey == poolKey && v.Version > max {
			max = v.Version
		}
	}
	return max, nil
}

// UpdateState 是"条件 UPDATE + RowsAffected"的忠实复刻：
// 只有 id 命中且当前 state 落在 fromStates 内才算改动，否则返回 (false, nil)。
// false 不是错误，是"状态已被别人推进"，调用方必须重读后决策 —— 因此这里绝不把
// 未命中包装成 error，也就逼着用例去断言 logic 有没有把 false 当成成功。
func (f *fakePoolVersion) UpdateState(ctx context.Context, session sqlx.Session, id int64, toState int32,
	fromStates []int32, operator, note string) (bool, error) {
	f.s.calls.record("PoolVersion.UpdateState", fmt.Sprintf("%d|%d|%v", id, toState, fromStates))
	if id <= 0 {
		return false, model.ErrInvalidVersion
	}
	if !model.ValidVersionState(toState) {
		return false, model.ErrInvalidVersionState
	}
	if len(fromStates) == 0 {
		return false, model.ErrInvalidVersionState
	}
	for _, s := range fromStates {
		if !model.ValidVersionState(s) {
			return false, model.ErrInvalidVersionState
		}
	}
	if err := requireTxSession("PoolVersion.UpdateState", session); err != nil {
		return false, err
	}
	f.s.runHook("PoolVersion.UpdateState", id, toState, fromStates)
	if err := f.s.injected("PoolVersion.UpdateState"); err != nil {
		return false, err
	}
	row := f.byID(id)
	if row == nil {
		return false, nil // WHERE id=? 没匹配到行：RowsAffected=0
	}
	matched := false
	for _, s := range fromStates {
		if row.State == s {
			matched = true
			break
		}
	}
	if !matched {
		return false, nil
	}
	f.s.writes++
	now := time.Now().Unix()
	row.State, row.Mtime, row.Operator, row.Note = toState, now, operator, note
	return true, nil
}

// SetPublishedAt 置 CURRENT 并记录生效时间：只有 READY/RETIRED 行能被点亮。
// 返回 false 同样"不是错误"，语义是"这一行不在可点亮状态"。
func (f *fakePoolVersion) SetPublishedAt(ctx context.Context, session sqlx.Session, id,
	publishedAt int64, operator string) (bool, error) {
	f.s.calls.record("PoolVersion.SetPublishedAt", fmt.Sprintf("%d|%d|%s", id, publishedAt, operator))
	if id <= 0 || publishedAt <= 0 {
		return false, model.ErrInvalidVersion
	}
	if strings.TrimSpace(operator) == "" {
		return false, model.ErrOperatorRequired
	}
	if err := requireTxSession("PoolVersion.SetPublishedAt", session); err != nil {
		return false, err
	}
	f.s.runHook("PoolVersion.SetPublishedAt", id, publishedAt)
	if err := f.s.injected("PoolVersion.SetPublishedAt"); err != nil {
		return false, err
	}
	row := f.byID(id)
	if row == nil {
		return false, nil
	}
	if row.State != model.VersionStateReady && row.State != model.VersionStateRetired {
		return false, nil
	}
	f.s.writes++
	row.State, row.PublishedAt, row.Operator, row.Mtime = model.VersionStateCurrent, publishedAt, operator, publishedAt
	return true, nil
}

func (f *fakePoolVersion) UpdateItemCount(ctx context.Context, session sqlx.Session,
	id, itemCount int64) error {
	f.s.calls.record("PoolVersion.UpdateItemCount", fmt.Sprintf("%d|%d", id, itemCount))
	if id <= 0 || itemCount < 0 {
		return model.ErrInvalidVersion
	}
	if err := requireTxSession("PoolVersion.UpdateItemCount", session); err != nil {
		return err
	}
	if err := f.s.injected("PoolVersion.UpdateItemCount"); err != nil {
		return err
	}
	row := f.byID(id)
	if row == nil {
		return nil // WHERE id=? 未命中：语句成功、影响 0 行
	}
	f.s.writes++
	row.ItemCount = itemCount
	row.Mtime = time.Now().Unix()
	return nil
}

// Delete 复刻 "DELETE ... WHERE state IN (RETIRED,FAILED) AND id IN (...)"：
// 状态保护写在 SQL 里，误传 CURRENT/READY 的 id 也删不掉 —— 这正是"绝不清掉在线版本"
// 的最后一道防线，用例要能证明即使 logic 点错名，在线版本依然健在。
func (f *fakePoolVersion) Delete(ctx context.Context, session sqlx.Session, ids []int64) (int64, error) {
	f.s.calls.record("PoolVersion.Delete", fmt.Sprintf("%v", ids))
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > model.MaxVersionListLimit {
		return 0, model.ErrTooManyItems
	}
	if err := requireTxSession("PoolVersion.Delete", session); err != nil {
		return 0, err
	}
	f.s.runHook("PoolVersion.Delete", ids)
	if err := f.s.injected("PoolVersion.Delete"); err != nil {
		return 0, err
	}
	want := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	var deleted int64
	for k, row := range f.s.versions {
		if _, ok := want[row.ID]; !ok {
			continue
		}
		if row.State != model.VersionStateRetired && row.State != model.VersionStateFailed {
			continue
		}
		delete(f.s.versions, k)
		deleted++
	}
	f.s.writes++
	return deleted, nil
}

// byID 按自增 id 找行指针（仅替身内部用）。
func (f *fakePoolVersion) byID(id int64) *model.RecallPoolVersion {
	for _, row := range f.s.versions {
		if row.ID == id {
			return row
		}
	}
	return nil
}

// --- recall_pool_current 替身 ---

type fakeCurrent struct{ s *store }

func (f *fakeCurrent) FindOne(ctx context.Context, source int32, poolKey string) (*model.RecallPoolCurrent, error) {
	f.s.calls.record("Current.FindOne", fmt.Sprintf("%d|%s", source, poolKey))
	if !model.ValidSource(source) {
		return nil, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if err := f.s.errPointerFindOne; err != nil {
		return nil, fmt.Errorf("recall_pool_current FindOne: %w", err)
	}
	ptr, ok := f.s.pointers[ptrKey{source: source, poolKey: poolKey}]
	if !ok {
		return nil, model.ErrPoolNotFound
	}
	cp := *ptr
	return &cp, nil
}

// ListBySources 复刻元组 IN 的语义：表里没有的池不会出现在结果里（不是错误）。
func (f *fakeCurrent) ListBySources(ctx context.Context, pools []model.PoolKey) ([]*model.RecallPoolCurrent, error) {
	f.s.calls.record("Current.ListBySources", len(pools))
	f.s.lastListBySourcesPools = append([]model.PoolKey(nil), pools...)
	if len(pools) == 0 {
		return nil, nil
	}
	if len(pools) > model.MaxPoolRefsPerQuery {
		return nil, model.ErrTooManyPools
	}
	if err := f.s.errPointerList; err != nil {
		return nil, fmt.Errorf("recall_pool_current ListBySources: %w", err)
	}
	out := make([]*model.RecallPoolCurrent, 0, len(pools))
	for _, p := range pools {
		if !model.ValidSource(p.Source) {
			return nil, model.ErrInvalidSource
		}
		if err := model.ValidatePoolKey(p.Source, p.PoolKey); err != nil {
			return nil, err
		}
		ptr, ok := f.s.pointers[ptrKey{source: p.Source, poolKey: p.PoolKey}]
		if !ok {
			continue
		}
		cp := *ptr
		out = append(out, &cp)
	}
	return out, nil
}

// ListCurrent 复刻真实 SQL：WHERE version>0 ORDER BY published_at DESC, source ASC LIMIT ?
func (f *fakeCurrent) ListCurrent(ctx context.Context, limit int) ([]*model.RecallPoolCurrent, error) {
	f.s.calls.record("Current.ListCurrent", limit)
	if err := model.CheckLimit(limit, model.MaxPoolRefsPerQuery); err != nil {
		return nil, err
	}
	var rows []*model.RecallPoolCurrent
	for _, ptr := range f.s.pointers {
		if ptr.Version <= 0 {
			continue
		}
		cp := *ptr
		rows = append(rows, &cp)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].PublishedAt != rows[j].PublishedAt {
			return rows[i].PublishedAt > rows[j].PublishedAt
		}
		return rows[i].Source < rows[j].Source
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// FindOneForUpdate 脱离事务直接返回 ErrSwitchConflict（生产同款：
// 没有事务的 FOR UPDATE 会长期持有行锁，比冲突更危险）。
func (f *fakeCurrent) FindOneForUpdate(ctx context.Context, session sqlx.Session, source int32,
	poolKey string) (*model.RecallPoolCurrent, error) {
	f.s.calls.record("Current.FindOneForUpdate", fmt.Sprintf("%d|%s", source, poolKey))
	if session == nil {
		return nil, fmt.Errorf("recall_pool_current FindOneForUpdate: %w", model.ErrSwitchConflict)
	}
	if !model.ValidSource(source) {
		return nil, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	f.s.runHook("Current.FindOneForUpdate", source, poolKey)
	if err := f.s.injected("Current.FindOneForUpdate"); err != nil {
		return nil, err
	}
	ptr, ok := f.s.pointers[ptrKey{source: source, poolKey: poolKey}]
	if !ok {
		return nil, model.ErrPoolNotFound
	}
	cp := *ptr
	return &cp, nil
}

// EnsureRow 幂等建 version=0 的空壳行（ODKU 是显式 no-op）。
func (f *fakeCurrent) EnsureRow(ctx context.Context, session sqlx.Session, source int32,
	poolKey string) error {
	f.s.calls.record("Current.EnsureRow", fmt.Sprintf("%d|%s", source, poolKey))
	if !model.ValidSource(source) {
		return model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(source, poolKey); err != nil {
		return err
	}
	if err := requireTxSession("Current.EnsureRow", session); err != nil {
		return err
	}
	if err := f.s.injected("Current.EnsureRow"); err != nil {
		return err
	}
	key := ptrKey{source: source, poolKey: poolKey}
	if _, ok := f.s.pointers[key]; ok {
		return nil // 值未变：RowsAffected=0，但语句成功
	}
	f.s.writes++
	now := time.Now().Unix()
	f.s.pointers[key] = &model.RecallPoolCurrent{
		Source: source, PoolKey: poolKey, Version: 0, Ctime: now, Mtime: now,
	}
	return nil
}

// Switch 是"指针切换只有一个开关"的复刻，也是本轮最要紧的一处替身：
//   - CAS 命中（指针当前值==expectVersion）才移动，Switched=true，Previous=expectVersion；
//   - CAS 未命中但指针已在目标版本 → Switched=false（并发对手切到同一目标，属幂等重放，不重复计数）；
//   - CAS 未命中且指针在别的版本 → ErrSwitchConflict，绝不静默覆盖。
//
// switch_count 在真实 SQL 里是 `switch_count = switch_count + 1`（不在 Go 读改写），
// 未切换的分支一律不碰它。
func (f *fakeCurrent) Switch(ctx context.Context, session sqlx.Session, in *model.RecallPoolCurrent,
	expectVersion int64) (*model.PoolSwitchOutcome, error) {
	f.s.calls.record("Current.Switch", fmt.Sprintf("%d|%s|%d|expect=%d", in.Source, in.PoolKey, in.Version, expectVersion))
	if !model.ValidSource(in.Source) {
		return nil, model.ErrInvalidSource
	}
	if err := model.ValidatePoolKey(in.Source, in.PoolKey); err != nil {
		return nil, err
	}
	if in.Version <= 0 {
		return nil, model.ErrInvalidVersion
	}
	if expectVersion < 0 {
		return nil, model.ErrInvalidVersion
	}
	if session == nil {
		return nil, fmt.Errorf("recall_pool_current Switch: %w", model.ErrSwitchConflict)
	}
	if strings.TrimSpace(in.Operator) == "" {
		return nil, model.ErrOperatorRequired
	}
	if strings.TrimSpace(in.Note) == "" {
		return nil, model.ErrReasonRequired
	}
	f.s.runHook("Current.Switch", in, expectVersion)
	if err := f.s.injected("Current.Switch"); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if in.PublishedAt == 0 {
		in.PublishedAt = now
	}
	key := ptrKey{source: in.Source, poolKey: in.PoolKey}
	ptr, ok := f.s.pointers[key]
	if !ok {
		// UPDATE 未匹配到行 → RowsAffected=0 → 事务内重读 → ErrPoolNotFound。
		return nil, model.ErrPoolNotFound
	}
	if ptr.Version != expectVersion {
		if ptr.Version == in.Version {
			return &model.PoolSwitchOutcome{Switched: false, Previous: ptr.Version,
				Current: ptr.Version, SwitchCount: ptr.SwitchCount}, nil
		}
		return nil, model.ErrSwitchConflict
	}
	f.s.writes++
	ptr.PreviousVersion = ptr.Version
	ptr.Version = in.Version
	ptr.BatchID = in.BatchID
	ptr.SwitchCount++
	ptr.Operator = in.Operator
	ptr.Note = in.Note
	ptr.PublishedAt = in.PublishedAt
	ptr.Mtime = now
	return &model.PoolSwitchOutcome{Switched: true, Previous: expectVersion, Current: in.Version}, nil
}

// --- recall_request_log 替身 ---

type fakeRequestLog struct{ s *store }

// Insert 复刻两个唯一键（uniq_request_id / uniq_snapshot_id）与列侧枚举校验：
// 审计行的唯一约束是"一次召回只有一份现场"的唯一保证，替身不校验就等于放开重复写。
func (f *fakeRequestLog) Insert(ctx context.Context, l *model.RecallRequestLog) error {
	f.s.calls.record("RequestLog.Insert", l.RequestID)
	f.s.writes++
	if strings.TrimSpace(l.RequestID) == "" || strings.TrimSpace(l.SnapshotID) == "" {
		return model.ErrRequestLogRequired
	}
	if l.DegradeReason != "" && !model.ValidDegradeReason(l.DegradeReason) {
		return model.ErrInvalidDegradeReason
	}
	if l.Platform != 0 && !model.ValidPlatform(l.Platform) {
		return model.ErrInvalidPlatform
	}
	if err := f.s.errLogInsert; err != nil {
		return fmt.Errorf("recall_request_log Insert: %w", err)
	}
	for _, exist := range f.s.logs {
		if exist.RequestID == l.RequestID || exist.SnapshotID == l.SnapshotID {
			return model.ErrRequestLogExists
		}
	}
	if l.Ctime == 0 {
		l.Ctime = time.Now().Unix()
	}
	cp := *l
	cp.ID = f.s.nextRowID()
	f.s.logs = append(f.s.logs, &cp)
	return nil
}

func (f *fakeRequestLog) FindByRequestID(ctx context.Context, requestID string) (*model.RecallRequestLog, error) {
	return f.find("RequestLog.FindByRequestID", "request_id", requestID)
}

func (f *fakeRequestLog) FindBySnapshotID(ctx context.Context, snapshotID string) (*model.RecallRequestLog, error) {
	return f.find("RequestLog.FindBySnapshotID", "snapshot_id", snapshotID)
}

func (f *fakeRequestLog) find(op, column, value string) (*model.RecallRequestLog, error) {
	f.s.calls.record(op, value)
	if err := f.s.errLogFind; err != nil {
		// 真实存储故障与"没查到"是两件事：这里包成非 ErrRequestNotFound 的错误，
		// 用来证明读路径不会把故障吞成一次未命中。
		return nil, fmt.Errorf("recall_request_log %s: %w", op, err)
	}
	for _, l := range f.s.logs {
		v := l.RequestID
		if column == "snapshot_id" {
			v = l.SnapshotID
		}
		if v == value {
			cp := *l
			return &cp, nil
		}
	}
	return nil, model.ErrRequestNotFound
}

// List 复刻 requestLogWhere + ORDER BY ctime DESC, id DESC + LIMIT/OFFSET + 边界校验。
func (f *fakeRequestLog) List(ctx context.Context, q model.RequestLogQuery) ([]*model.RecallRequestLog, error) {
	f.s.calls.record("RequestLog.List", fmt.Sprintf("mid=%d scene=%s from=%d to=%d off=%d lim=%d",
		q.Mid, q.Scene, q.From, q.To, q.Offset, q.Limit))
	if err := checkRequestLogQuery(q); err != nil {
		return nil, err
	}
	if err := f.s.injected("RequestLog.List"); err != nil {
		return nil, err
	}
	rows := f.match(q)
	if q.Offset >= len(rows) {
		return nil, nil
	}
	rows = rows[q.Offset:]
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return rows, nil
}

func (f *fakeRequestLog) Count(ctx context.Context, q model.RequestLogQuery) (int64, error) {
	f.s.calls.record("RequestLog.Count", fmt.Sprintf("mid=%d scene=%s from=%d to=%d", q.Mid, q.Scene, q.From, q.To))
	if q.From > 0 && q.To > 0 && q.From > q.To {
		return 0, model.ErrPageTooDeep
	}
	if err := f.s.injected("RequestLog.Count"); err != nil {
		return 0, err
	}
	return int64(len(f.match(q))), nil
}

func (f *fakeRequestLog) match(q model.RequestLogQuery) []*model.RecallRequestLog {
	var rows []*model.RecallRequestLog
	for _, l := range f.s.logs {
		cp := *l
		if q.Mid > 0 && cp.Mid != q.Mid {
			continue
		}
		if q.Scene != "" && cp.Scene != q.Scene {
			continue
		}
		if q.From > 0 && cp.Ctime < q.From {
			continue
		}
		if q.To > 0 && cp.Ctime > q.To {
			continue
		}
		rows = append(rows, &cp)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Ctime != rows[j].Ctime {
			return rows[i].Ctime > rows[j].Ctime
		}
		return rows[i].ID > rows[j].ID
	})
	return rows
}

func (f *fakeRequestLog) DeleteBefore(context.Context, int64, int64) (int64, error) {
	return 0, f.s.trap("RequestLog.DeleteBefore")
}

// checkRequestLogQuery 复刻 model.checkRequestLogQuery（logic 与 model 两道闸门都要在替身里生效）。
func checkRequestLogQuery(q model.RequestLogQuery) error {
	if err := model.CheckLimit(q.Limit, model.MaxRequestLogPageSize); err != nil {
		return err
	}
	if q.Offset > model.MaxRequestLogOffset {
		return model.ErrPageTooDeep
	}
	if q.From > 0 && q.To > 0 && q.From > q.To {
		return model.ErrPageTooDeep
	}
	return nil
}

// --- recall_idempotency 替身 ---

type fakeIdempotency struct{ s *store }

// Claim 复刻真实语义：唯一键插入影响行数判定首认领；
// 同键不同指纹硬失败；SUCCEEDED/未过期 PENDING 回放；过期 PENDING 与 FAILED 重新认领。
func (f *fakeIdempotency) Claim(ctx context.Context, session sqlx.Session, in *model.RecallIdempotency,
	now int64) (*model.IdempotencyClaim, error) {
	f.s.calls.record("Idempotency.Claim", fmt.Sprintf("%s|%s", in.Scope, in.IdempotencyKey))
	f.s.writes++
	if !model.ValidIdempotencyScope(in.Scope) {
		return nil, model.ErrIdempotencyStateInvalid
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" {
		return nil, model.ErrIdempotencyKeyRequired
	}
	if len(key) > model.MaxIdempotencyKeyLen {
		return nil, model.ErrIdempotencyKeyTooLong
	}
	if len(in.RequestHash) != model.RequestHashLen {
		return nil, model.ErrIdempotencyFingerprintMismatch
	}
	if strings.TrimSpace(in.Operator) == "" {
		return nil, model.ErrOperatorRequired
	}
	if !model.ValidIdempotencyState(in.State) {
		return nil, model.ErrIdempotencyStateInvalid
	}
	if err := f.s.errClaim; err != nil {
		return nil, fmt.Errorf("recall_idempotency Claim: %w", err)
	}
	ck := claimKey{scope: in.Scope, key: key}
	exist, ok := f.s.claims[ck]
	if !ok {
		cp := *in
		cp.IdempotencyKey = key
		cp.ID = f.s.nextRowID()
		if cp.Ctime == 0 {
			cp.Ctime = now
		}
		cp.ExecutionCount = 1
		f.s.claims[ck] = &cp
		return &model.IdempotencyClaim{First: true, Existing: &cp}, nil
	}
	if exist.RequestHash != in.RequestHash {
		return nil, model.ErrIdempotencyFingerprintMismatch
	}
	switch {
	case exist.State == string(idempotency.StateSucceeded):
		return &model.IdempotencyClaim{Held: true, Existing: exist}, nil
	case exist.State == string(idempotency.StatePending) && exist.LeaseExpireAt > now:
		return &model.IdempotencyClaim{Held: true, Existing: exist}, nil
	}
	// 过期 PENDING / FAILED：条件更新只放一个赢家。
	exist.State = string(idempotency.StatePending)
	exist.LeaseExpireAt = in.LeaseExpireAt
	exist.RequestHash = in.RequestHash
	exist.Operator = in.Operator
	exist.ExecutionCount++
	exist.LastError = ""
	return &model.IdempotencyClaim{First: true, Existing: exist}, nil
}

func (f *fakeIdempotency) MarkFailed(ctx context.Context, scope, key, lastError string) (bool, error) {
	f.s.calls.record("Idempotency.MarkFailed", fmt.Sprintf("%s|%s", scope, key))
	f.s.writes++
	if !model.ValidIdempotencyScope(scope) {
		return false, model.ErrIdempotencyStateInvalid
	}
	if strings.TrimSpace(key) == "" {
		return false, model.ErrIdempotencyKeyRequired
	}
	if len(lastError) > 512 {
		lastError = lastError[:512]
	}
	row, ok := f.s.claims[claimKey{scope: scope, key: key}]
	if !ok || row.State == string(idempotency.StateSucceeded) {
		// 真实 SQL 的 WHERE state<>SUCCEEDED 影响 0 行：不是错误，是"没改动"。
		return false, nil
	}
	row.State = string(idempotency.StateFailed)
	row.LeaseExpireAt = 0
	row.LastError = lastError
	return true, nil
}

func (f *fakeIdempotency) Find(ctx context.Context, scope, key string) (*model.RecallIdempotency, error) {
	f.s.calls.record("Idempotency.Find", fmt.Sprintf("%s|%s", scope, key))
	row, ok := f.s.claims[claimKey{scope: scope, key: key}]
	if !ok {
		return nil, model.ErrNotFound
	}
	cp := *row
	return &cp, nil
}

// seedClaim 预置一条幂等记录（重放用例的前置数据）。
func (s *store) seedClaim(scope, key, hash, state, payload string, leaseExpireAt, expireAt int64) *model.RecallIdempotency {
	row := &model.RecallIdempotency{
		ID: s.nextRowID(), Scope: scope, IdempotencyKey: key, RequestHash: hash, State: state,
		ResultPayload: payload, Operator: "fake-operator", LeaseExpireAt: leaseExpireAt,
		ExpireAt: expireAt, ExecutionCount: 1, Ctime: 1700000000, Mtime: 1700000000,
	}
	s.claims[claimKey{scope: scope, key: key}] = row
	return row
}

// MarkSucceeded 把幂等记录置为 SUCCEEDED 并回填回放载荷/事件号，必须与业务写同事务。
//
// 真实 SQL 用的是 rowChanged（"值完全相同"时 MySQL 返回 0），所以 false 不代表失败：
// 记录不存在时 UPDATE 影响 0 行同样返回 (false, nil)。这里如实复刻——
// logic 若把 false 当成功，或把"记录不存在"当成功，用例就能红。
func (f *fakeIdempotency) MarkSucceeded(ctx context.Context, session sqlx.Session, scope, key,
	resultPayload, eventID string) (bool, error) {
	f.s.calls.record("Idempotency.MarkSucceeded", fmt.Sprintf("%s|%s|%s", scope, key, eventID))
	if !model.ValidIdempotencyScope(scope) {
		return false, model.ErrIdempotencyStateInvalid
	}
	if strings.TrimSpace(key) == "" {
		return false, model.ErrIdempotencyKeyRequired
	}
	if len(resultPayload) > model.MaxIdempotencyPayloadLen {
		return false, model.ErrTooManyItems
	}
	if err := requireTxSession("Idempotency.MarkSucceeded", session); err != nil {
		return false, err
	}
	f.s.runHook("Idempotency.MarkSucceeded", scope, key)
	if err := f.s.injected("Idempotency.MarkSucceeded"); err != nil {
		return false, err
	}
	row, ok := f.s.claims[claimKey{scope: scope, key: key}]
	if !ok {
		return false, nil
	}
	f.s.writes++
	// UPDATE 总会在匹配到行时把 mtime 推到"现在"，因此 changed 只可能因同秒重复赋值而为 false。
	changed := row.State != string(idempotency.StateSucceeded) || row.ResultPayload != resultPayload ||
		row.EventID != eventID || row.LeaseExpireAt != 0 || row.LastError != ""
	now := time.Now().Unix()
	row.State = string(idempotency.StateSucceeded)
	row.ResultPayload = resultPayload
	row.EventID = eventID
	row.LeaseExpireAt = 0
	row.LastError = ""
	row.Mtime = now
	return changed, nil
}

func (f *fakeIdempotency) ListExpired(context.Context, int64, int) ([]int64, error) {
	return nil, f.s.trap("Idempotency.ListExpired")
}
func (f *fakeIdempotency) DeleteExpired(context.Context, int64, int64) (int64, error) {
	return 0, f.s.trap("Idempotency.DeleteExpired")
}

// --- recall_outbox 替身 ---

type fakeOutbox struct{ s *store }

// Insert 复刻 event_id 唯一键与列默认值：事件是"业务写与事件必须同提交"的那一半，
// 事务内落库、重复 event_id 报 ErrEventExists（生产者重放的幂等命中）。
func (f *fakeOutbox) Insert(ctx context.Context, session sqlx.Session, e *model.RecallOutbox) error {
	f.s.calls.record("Outbox.Insert", fmt.Sprintf("%s|%s|%s", e.EventID, e.EventType, e.AggregateID))
	if strings.TrimSpace(e.EventID) == "" || strings.TrimSpace(e.EventType) == "" {
		return model.ErrEventRequired
	}
	if err := requireTxSession("Outbox.Insert", session); err != nil {
		return err
	}
	if err := f.s.injected("Outbox.Insert"); err != nil {
		return err
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = model.PoolVersionSchemaVersion
	}
	if e.Ctime == 0 {
		e.Ctime = time.Now().Unix()
	}
	e.Mtime = e.Ctime
	if e.OccurredAt == 0 {
		e.OccurredAt = e.Ctime
	}
	for _, exist := range f.s.outbox {
		if exist.EventID == e.EventID {
			return model.ErrEventExists
		}
	}
	f.s.writes++
	cp := *e
	cp.ID = f.s.nextRowID()
	f.s.outbox = append(f.s.outbox, &cp)
	return nil
}
func (f *fakeOutbox) ListPending(context.Context, int64, int32) ([]*model.RecallOutbox, error) {
	return nil, f.s.trap("Outbox.ListPending")
}
func (f *fakeOutbox) MarkPublished(context.Context, int64, int64) (int64, error) {
	return 0, f.s.trap("Outbox.MarkPublished")
}
func (f *fakeOutbox) MarkRetry(context.Context, int64, int64, string) (int64, error) {
	return 0, f.s.trap("Outbox.MarkRetry")
}
func (f *fakeOutbox) MarkFailed(context.Context, int64, string) (int64, error) {
	return 0, f.s.trap("Outbox.MarkFailed")
}
func (f *fakeOutbox) CountPending(context.Context) (int64, error) {
	return 0, f.s.trap("Outbox.CountPending")
}
func (f *fakeOutbox) CountStuck(context.Context, int64) (int64, error) {
	return 0, f.s.trap("Outbox.CountStuck")
}
func (f *fakeOutbox) DeleteSentBefore(context.Context, int64, int64) (int64, error) {
	return 0, f.s.trap("Outbox.DeleteSentBefore")
}

// --- 下游数据源替身 ---

type fakeFeatures struct {
	calls *callLog

	interestTags []repository.InterestTag
	interestErr  error
	signals      *repository.BehaviorSignals
	signalsErr   error
	vectors      []model.RecallPool
	vectorErr    error

	lastInterestLimit int
	lastSignalsLimit  int
	lastSignalsWindow time.Duration
	lastSignalsMid    int64
	lastVectorLimit   int
}

func (f *fakeFeatures) GetUserInterest(ctx context.Context, mid int64, limit int) ([]repository.InterestTag, error) {
	f.calls.record("Features.GetUserInterest", fmt.Sprintf("%d|%d", mid, limit))
	f.lastInterestLimit = limit
	if f.interestErr != nil {
		return nil, f.interestErr
	}
	return f.interestTags, nil
}

func (f *fakeFeatures) GetBehaviorSignals(ctx context.Context, mid int64, within time.Duration,
	limit int) (*repository.BehaviorSignals, error) {
	f.calls.record("Features.GetBehaviorSignals", fmt.Sprintf("%d|%d|%d", mid, within, limit))
	f.lastSignalsLimit, f.lastSignalsWindow, f.lastSignalsMid = limit, within, mid
	if f.signalsErr != nil {
		return nil, f.signalsErr
	}
	return f.signals, nil
}

func (f *fakeFeatures) ListHotSubjects(context.Context, int64, time.Duration,
	int) ([]repository.HotSubject, error) {
	f.calls.record("Features.ListHotSubjects", "TRAP")
	return nil, fmt.Errorf("Features.ListHotSubjects: %w", errNotWired)
}

func (f *fakeFeatures) GetVectorCandidates(ctx context.Context, mid int64,
	limit int) ([]model.RecallPool, error) {
	f.calls.record("Features.GetVectorCandidates", fmt.Sprintf("%d|%d", mid, limit))
	f.lastVectorLimit = limit
	if f.vectorErr != nil {
		return nil, f.vectorErr
	}
	return f.vectors, nil
}

// fakeVisibility 是可见性下游替身。
// visible==nil 表示"全部入参可见"；visible 为非 nil 空切片表示"一条都不许下发"。
type fakeVisibility struct {
	calls   *callLog
	visible []int64
	err     error

	lastAids  []int64
	callCount int
}

func (f *fakeVisibility) FilterVisible(ctx context.Context, aids []int64) ([]int64, error) {
	f.calls.record("Visibility.FilterVisible", len(aids))
	f.lastAids = append([]int64(nil), aids...)
	f.callCount++
	if f.err != nil {
		return nil, f.err
	}
	if f.visible != nil {
		return f.visible, nil
	}
	return aids, nil
}

func (f *fakeVisibility) FollowingUps(context.Context, int64, int) ([]int64, error) {
	f.calls.record("Visibility.FollowingUps", "TRAP")
	return nil, fmt.Errorf("Visibility.FollowingUps: %w", errNotWired)
}

func (f *fakeVisibility) BlockedUps(context.Context, int64, int) ([]int64, error) {
	f.calls.record("Visibility.BlockedUps", "TRAP")
	return nil, fmt.Errorf("Visibility.BlockedUps: %w", errNotWired)
}

// --- SQL 连接替身 ---

// trapConn 只满足 sqlx.SqlConn：不建任何连接，裸语句一律显式失败。
//
// TransactCtx 给出一个**可回滚的**内存事务：进入回调前对六张表做快照，
// 回调返回错误即还原行数据（并保留事务期间外部已提交的变更），返回 nil 即提交。
// 这样"业务写了一半失败后库里是什么形态""事件行有没有跟着回滚"才是被测的生产编排代码。
// 边界：没有行锁与隔离级别，真并发只能靠 store.commitExternally 注入一份对手变更。
type trapConn struct{ s *store }

func (c *trapConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.s.calls.record("Transact")
	if err := c.s.injected("Transact"); err != nil {
		return err
	}
	snap := c.s.snapshot()
	c.s.external = nil
	err := fn(ctx, &txSession{calls: c.s.calls})
	if err == nil {
		c.s.external = nil
		c.s.calls.record("Transact.Commit")
		return nil
	}
	c.s.restore(snap)
	// 回滚不能撤销别人已提交的变更：按发生顺序重放。
	for _, apply := range c.s.external {
		apply()
	}
	c.s.external = nil
	c.s.calls.record("Transact.Rollback")
	return err
}

func (c *trapConn) Transact(fn func(sqlx.Session) error) error {
	return c.TransactCtx(context.Background(), func(_ context.Context, s sqlx.Session) error {
		return fn(s)
	})
}

func (c *trapConn) RawDB() (*sql.DB, error) { return nil, errNotWired }
func (c *trapConn) ExecCtx(context.Context, string, ...interface{}) (sql.Result, error) {
	c.s.calls.record("Conn.ExecCtx", "TRAP")
	return nil, errNotWired
}
func (c *trapConn) Exec(string, ...interface{}) (sql.Result, error) {
	c.s.calls.record("Conn.Exec", "TRAP")
	return nil, errNotWired
}
func (c *trapConn) Prepare(string) (sqlx.StmtSession, error) { return nil, errNotWired }
func (c *trapConn) PrepareCtx(context.Context, string) (sqlx.StmtSession, error) {
	return nil, errNotWired
}
func (c *trapConn) QueryRowCtx(context.Context, any, string, ...any) error {
	c.s.calls.record("Conn.QueryRowCtx", "TRAP")
	return errNotWired
}
func (c *trapConn) QueryRow(any, string, ...any) error { return errNotWired }
func (c *trapConn) QueryRowPartialCtx(context.Context, any, string, ...any) error {
	return errNotWired
}
func (c *trapConn) QueryRowPartial(any, string, ...any) error { return errNotWired }
func (c *trapConn) QueryRowsCtx(context.Context, any, string, ...any) error {
	c.s.calls.record("Conn.QueryRowsCtx", "TRAP")
	return errNotWired
}
func (c *trapConn) QueryRows(any, string, ...any) error { return errNotWired }
func (c *trapConn) QueryRowsPartialCtx(context.Context, any, string, ...any) error {
	return errNotWired
}
func (c *trapConn) QueryRowsPartial(any, string, ...any) error { return errNotWired }

// txSession 是事务里的手写 SQL 通道。六个 model 的 InTx 方法在真实实现里走 ExecCtx/QueryRowCtx，
// 但本包的替身已经按语义复刻了它们的数据效果，因此**没有**一条语句该落到这里。
// 一旦有语句进来就记进调用序列并显式失败：这同时证明"写路径全在替身覆盖的 model 方法里"。
type txSession struct{ calls *callLog }

func (t *txSession) fail(op string, args ...interface{}) error {
	t.calls.record(op, "SQL")
	return fmt.Errorf("%s: 事务内出现未建模的手写 SQL: %w", op, errNotWired)
}

func (t *txSession) Exec(string, ...interface{}) (sql.Result, error) {
	return nil, t.fail("Tx.Exec")
}
func (t *txSession) ExecCtx(context.Context, string, ...interface{}) (sql.Result, error) {
	return nil, t.fail("Tx.ExecCtx")
}
func (t *txSession) Prepare(string) (sqlx.StmtSession, error) { return nil, t.fail("Tx.Prepare") }
func (t *txSession) PrepareCtx(context.Context, string) (sqlx.StmtSession, error) {
	return nil, t.fail("Tx.PrepareCtx")
}
func (t *txSession) QueryRow(any, string, ...any) error { return t.fail("Tx.QueryRow") }
func (t *txSession) QueryRowCtx(context.Context, any, string, ...any) error {
	return t.fail("Tx.QueryRowCtx")
}
func (t *txSession) QueryRowPartial(any, string, ...any) error { return t.fail("Tx.QueryRowPartial") }
func (t *txSession) QueryRowPartialCtx(context.Context, any, string, ...any) error {
	return t.fail("Tx.QueryRowPartialCtx")
}
func (t *txSession) QueryRows(any, string, ...any) error { return t.fail("Tx.QueryRows") }
func (t *txSession) QueryRowsCtx(context.Context, any, string, ...any) error {
	return t.fail("Tx.QueryRowsCtx")
}
func (t *txSession) QueryRowsPartial(any, string, ...any) error { return t.fail("Tx.QueryRowsPartial") }
func (t *txSession) QueryRowsPartialCtx(context.Context, any, string, ...any) error {
	return t.fail("Tx.QueryRowsPartialCtx")
}

// --- 装配 ---

// 召回路编号：config.RecallConf 用 int64、model.Source* 是 int32，
// 这里显式换算，保证测试里的编号与 model 常量同一真值（不自造数字）。
const (
	srcHot     = int64(model.SourceHot)
	srcFollow  = int64(model.SourceFollow)
	srcTag     = int64(model.SourceTag)
	srcCollab  = int64(model.SourceCollab)
	srcVector  = int64(model.SourceVector)
	srcCold    = int64(model.SourceCold)
	srcHot32   = model.SourceHot
	srcFollow3 = model.SourceFollow
	srcTag32   = model.SourceTag
	srcCold32  = model.SourceCold
)

// testRecallConf 与 etc/recommendrecall.v1.yaml 的召回参数逐项对应
// （启用热门/关注/标签/冷启动，默认组合热门+关注+冷启动，兜底热门，冷启动走 6，
// 预算 80ms，TTL 30s）。用例改配置一律走 newEnvWithRecall，不另起一份常量。
func testRecallConf() config.RecallConf {
	return config.RecallConf{
		MaxCandidates:               400,
		DefaultLimit:                120,
		PerSourceMax:                120,
		EnabledSources:              []int64{srcHot, srcFollow, srcTag, srcCold},
		DefaultSources:              []int64{srcHot, srcFollow, srcCold},
		FallbackSource:              srcHot,
		ColdStartSource:             srcCold,
		DegradeEnabled:              true,
		MaxSeedAids:                 20,
		MaxSeedTags:                 20,
		MaxExcludeAids:              500,
		BudgetMillis:                80,
		TTLSeconds:                  30,
		PoolStaleSeconds:            1800,
		MaxReadyPools:               50,
		MaxBatchItems:               1000,
		MaxVersionList:              100,
		MaxPoolSnapshotPage:         200,
		MaxRequestLogPage:           100,
		MinKeepVersions:             2,
		PruneMaxRows:                2000,
		RequestLogRetentionSeconds:  604800,
		IdempotencyLeaseSeconds:     300,
		IdempotencyRetentionSeconds: 86400,
	}
}

// optionsFromRecall 与 internal/svc.ServiceContext 的映射逐字一致：
// 配置到运行参数的换算只有一处真值，测试不允许自己发明第二份。
func optionsFromRecall(rc config.RecallConf) repository.Options {
	return repository.Options{
		MaxCandidates:               rc.MaxCandidates,
		DefaultLimit:                rc.DefaultLimit,
		PerSourceMax:                rc.PerSourceMax,
		MaxSeedAids:                 rc.MaxSeedAids,
		MaxSeedTags:                 rc.MaxSeedTags,
		MaxExcludeAids:              rc.MaxExcludeAids,
		MaxBatchItems:               rc.MaxBatchItems,
		MaxVersionList:              rc.MaxVersionList,
		MaxPoolSnapshotPage:         rc.MaxPoolSnapshotPage,
		MaxRequestLogPage:           rc.MaxRequestLogPage,
		MinKeepVersions:             rc.MinKeepVersions,
		PoolStaleSeconds:            rc.PoolStaleSeconds,
		IdempotencyLeaseSeconds:     rc.IdempotencyLeaseSeconds,
		IdempotencyRetentionSeconds: rc.IdempotencyRetentionSeconds,
	}
}

// env 是一次用例的装配结果：真实 Repository 形状 + 内存替身。
type env struct {
	t      *testing.T
	svcCtx *svc.ServiceContext
	repo   *repository.Repository
	rc     config.RecallConf
	st     *store
	feat   *fakeFeatures
	vis    *fakeVisibility

	// depsFeatures/depsVisibility 覆盖默认替身，用于「按生产口径装显式 stub」的用例。
	// 覆盖时 e.feat/e.vis 置 nil —— 一旦 logic 仍去调用被替换掉的对象就是 panic，
	// 而不是悄悄拿到底层替身的数据。
	depsFeatures   repository.FeatureSource
	depsVisibility repository.VisibilitySource
}

// deps 组装 repository.Deps：六个 model 全部走内存替身，
// Conn 走 trapConn（裸语句显式失败，事务可回滚），Cache 恒为 nil。
//
// Cache 为 nil 是本轮的覆盖边界而不是偷懒：redis.Redis 是具体类型而非接口，
// 进程内没有可替换的假缓存，因此所有池读取都会带 store_unavailable、
// 正常结果的 ttl 恒为 0。该边界与它的后果都写进 README。
func (e *env) deps() repository.Deps {
	d := repository.Deps{
		Conn:        &trapConn{s: e.st},
		Pool:        &fakePool{s: e.st},
		PoolVersion: &fakePoolVersion{s: e.st},
		Current:     &fakeCurrent{s: e.st},
		RequestLog:  &fakeRequestLog{s: e.st},
		Outbox:      &fakeOutbox{s: e.st},
		Idempotency: &fakeIdempotency{s: e.st},
		Features:    e.feat,
		Visibility:  e.vis,
	}
	if e.depsFeatures != nil {
		d.Features = e.depsFeatures
	}
	if e.depsVisibility != nil {
		d.Visibility = e.depsVisibility
	}
	return d
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil, nil) }

func newEnvWithRecall(t *testing.T, mutate func(*config.RecallConf)) *env {
	e := newEnvWith(t, nil, nil)
	mutate(&e.rc)
	// 改过配置必须重新装配，否则 Repository.Options() 还是旧值（logic 只读它）。
	repo, err := repository.NewWithDeps(optionsFromRecall(e.rc), e.deps())
	if err != nil {
		t.Fatalf("NewWithDeps 失败: %v", err)
	}
	e.repo = repo
	e.svcCtx.Repository = repo
	e.svcCtx.Config.Recall = e.rc
	return e
}

// newEnvWith 允许替换下游数据源：传 repository 的显式 stub 即为生产口径。
func newEnvWith(t *testing.T, features repository.FeatureSource, visibility repository.VisibilitySource) *env {
	t.Helper()
	st := newStore()
	e := &env{t: t, rc: testRecallConf(), st: st}
	e.feat = &fakeFeatures{calls: st.calls}
	e.vis = &fakeVisibility{calls: st.calls}
	if features != nil {
		e.feat = nil
		e.depsFeatures = features
	}
	if visibility != nil {
		e.vis = nil
		e.depsVisibility = visibility
	}
	if err := e.rc.Validate(); err != nil {
		t.Fatalf("测试配置必须本身可启动（RecallConf.Validate）: %v", err)
	}
	repo, err := repository.NewWithDeps(optionsFromRecall(e.rc), e.deps())
	if err != nil {
		t.Fatalf("NewWithDeps 失败: %v", err)
	}
	e.repo = repo
	e.svcCtx = &svc.ServiceContext{Config: config.Config{}, Repository: repo}
	e.svcCtx.Config.Recall = e.rc
	return e
}

// ctx 返回与 env 配套的 context（logic 构造器的 ctx 参数）。
func (e *env) ctx() context.Context { return context.Background() }

// calls 便捷取调用序列。
func (e *env) calls() *callLog { return e.st.calls }

// testPoolRef 构造池寻址入参。(source, pool_key) 的合法组合语法由 model.ValidatePoolKey
// 单独管辖，本文件不自造"看起来像池键"的字符串：测试里出现的每个池键都必须能通过校验，
// 除非该用例就是在钉拒绝行为。
func testPoolRef(source int32, key string) *rpc.PoolRef {
	return &rpc.PoolRef{Source: rpc.Source(source), PoolKey: key}
}

// requestLogRow 给一行召回审计的默认值（明显假数据），用例只覆写自己关心的列。
// 默认未降级、per_source 为受控空数组，这样"读路径把降级读成正常"这类问题
// 必须由用例显式构造而不是靠默认值蒙对。
func requestLogRow(requestID, snapshotID string) *model.RecallRequestLog {
	return &model.RecallRequestLog{
		RequestID: requestID, SnapshotID: snapshotID,
		Mid: 900000001, Scene: "home.feed", Platform: model.PlatformAndroid,
		AppVersion: "9.9.9", Region: "cn-test",
		RequestedSources: encodeSourcesCSV([]int32{model.SourceHot, model.SourceCold}),
		PerSource:        "[]",
		CandidateCount:   3, ReturnedCount: 3,
		Degraded: 0, DegradeReason: model.DegradeReasonNone, DroppedSources: "",
		VersionsDigest: strings.Repeat("0", model.RequestHashLen),
		CostMs:         7, TraceID: "trace-fake-0001", Ctime: 1700000100,
	}
}

// statsJSON 渲染 per_source 列（用生产同一份编码器，避免测试自己发明第二套 JSON 形状）。
func statsJSON(t *testing.T, stats ...*rpc.SourceStat) string {
	t.Helper()
	raw, err := encodeSourceStats(stats)
	if err != nil {
		t.Fatalf("构造 per_source 失败: %v", err)
	}
	return raw
}

var (
	_ model.RecallPoolModel        = (*fakePool)(nil)
	_ model.RecallPoolVersionModel = (*fakePoolVersion)(nil)
	_ model.RecallPoolCurrentModel = (*fakeCurrent)(nil)
	_ model.RecallRequestLogModel  = (*fakeRequestLog)(nil)
	_ model.RecallIdempotencyModel = (*fakeIdempotency)(nil)
	_ model.RecallOutboxModel      = (*fakeOutbox)(nil)
	_ repository.FeatureSource     = (*fakeFeatures)(nil)
	_ repository.VisibilitySource  = (*fakeVisibility)(nil)
	_ sqlx.SqlConn                 = (*trapConn)(nil)
	_ sqlx.Session                 = (*txSession)(nil)
)
