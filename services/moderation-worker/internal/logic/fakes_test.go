package logic

// fakes_test.go 是 moderation-worker logic 测试的内存替身集合。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, nil, 内存 workerMd) 组装**真实的 Repository**，
// 只把它的 2 个依赖换成替身——这样「建任务 → 写终态 → 失效结果缓存」整条链路
// （含 FinishTask 里那条被吞掉的缓存错误、UpdateResult 的 RowsAffected==0 语义）
// 都在被测路径上，而不是把 Repository 也 mock 掉。见 internal/repository 的 Cacher 注释。
//
// 四条替身纪律（rights / playback 两轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic 里的写回不得污染库存行，否则「有没有真的落库」
//     这类断言会被共享指针掩盖。
//  2. 副作用按真实 SQL 的口径处理：Upsert 的 ON DUPLICATE KEY UPDATE 只改 mtime，
//     UpdateResult 的 WHERE 未命中要返回 ErrTaskNotFound（与 model 的 RowsAffected 判定一致）。
//  3. 调用按**顺序**记录（callLog，格式 `<pkg>.<method>:<key>`），断言序列而不是只数次数：
//     本服务要紧的结论是「建任务在前、写终态在后、终态写失败时一次都不许碰缓存」。
//  4. 布数据走替身的**静默写入路径**（seedTask → put、fakeCache.warm），不记轨迹，
//     因此轨迹断言可以从 0 开始数。改用公开方法布景时必须先取 before := st.log.snapshot()。
//
// 另加两条本服务特有的纪律：
//  5. worker_task_id 在 Run* 里可能是 ULID（不可预测），因此只有**布景给定的** ID
//     才允许出现在精确序列期望里；生成式用例一律用 wantCount + 回读库存断言。
//  6. 每个替身方法额外记录「拿到的是请求 ctx 还是 context.Background()」，
//     用来钉住超时/取消传播（见 TestRunCreateTaskIgnoresRequestContext）。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**（主键冲突只改 mtime、RowsAffected、mtime DESC 取最新），
//     不证明 SQL 与列名本身；deploy/migrations/moderation-worker 与本包替身之间没有列级对账门禁。
//   - 本服务**没有外部算法/引擎依赖**（本期占位实现，ServiceContext 里没有引擎字段），
//     所以「引擎超时/返回未知结论」这类下游故障在 worker 侧只能落到两个下游上复现：
//     worker_task 写入失败、结果缓存失效失败。用例按这两个下游逐个注入，不虚构引擎替身。
//   - 替身不做回滚：CreateTask 与 FinishTask 是两次独立写入、无事务，
//     因此「第二步失败 ⇒ 第一步一起消失」在这里根本不成立，用例只能断言残留的半截 PENDING 行（缺陷 #1）。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"go-video/services/moderation-worker/internal/config"
	"go-video/services/moderation-worker/internal/repository"
	"go-video/services/moderation-worker/internal/svc"
	"go-video/services/moderation-worker/model"
)

// --- 断言小工具（本包共享） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵（repository/logic 用 %w 包装下游错误是允许的）。
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

// wantStringsEQ 比较字符串切片（wantEQ 受 comparable 约束，切片只能另走这条）。
func wantStringsEQ(t *testing.T, label, field string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantInt32sEQ 比较 int32 切片（枚举序列断言用，wantEQ 受 comparable 约束）。
func wantInt32sEQ(t *testing.T, label, field string, got, want []int32) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantNil 断言指针为空（用来区分「无片段」与「空片段」，protobuf 字段同理）。
func wantNil[T any](t *testing.T, label, field string, got *T) {
	t.Helper()
	if got != nil {
		t.Errorf("%s：%s = %#v, want nil", label, field, *got)
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

// wantCount 断言某类调用发生的次数（用于 worker_task_id 不可预测的写侧断言）。
// 布数据走静默路径（见 seedTask），因此这里数的是**被测代码**的调用。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整序列 [%s]）",
			label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

// assertAround 断言墙钟派生值落在期望附近：logic 内部用 model.NowUnix()（不可注入），
// 跨秒抖动不可避免，因此只允许 ±slack 的偏差；状态/计数类断言必须用精确相等。
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

// countPrefix 统计以 prefix 开头的调用数，用于「这类操作一次都没发生」的断言。
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

// --- ctx 探针（纪律 6） ---

type ctxMarkerKey struct{ name string }

// reqCtxMarker 只用于测试：带标记的 ctx 表示「这是 gRPC 请求 ctx」，
// 替身据此区分被测代码传进来的是 l.ctx 还是 context.Background()。
var reqCtxMarker = ctxMarkerKey{"mw-request"}

// newReqCtx 返回一个带请求标记的 ctx。
func newReqCtx() context.Context {
	return context.WithValue(context.Background(), reqCtxMarker, "req")
}

// ctxProbe 记录每个方法每次调用是否拿到带标记的请求 ctx。
type ctxProbe struct{ reqByMethod map[string][]bool }

func newCtxProbe() *ctxProbe { return &ctxProbe{reqByMethod: map[string][]bool{}} }

func (p *ctxProbe) record(method string, ctx context.Context) {
	p.reqByMethod[method] = append(p.reqByMethod[method], ctx.Value(reqCtxMarker) != nil)
}

// got 读第 n 次（从 0 数）调用拿到的 ctx 是否为请求 ctx；没有该调用则直接失败。
func (p *ctxProbe) got(t *testing.T, method string, n int) bool {
	t.Helper()
	seen := p.reqByMethod[method]
	if n >= len(seen) {
		t.Fatalf("探针：方法 %s 只有 %d 次调用，取不到第 %d 次的 ctx", method, len(seen), n+1)
	}
	return seen[n]
}

// --- Redis key 复刻（与 repository 未导出常量逐字对齐；不一致时失效用例即红） ---

func keyTaskLock(workerTaskID string) string   { return fmt.Sprintf("mw:lock:%s", workerTaskID) }
func keyTaskResult(workerTaskID string) string { return fmt.Sprintf("mw:res:%s", workerTaskID) }

// --- 缓存替身（实现 repository.Cacher） ---

// fakeCache 复刻真实 Cache 的口径：miss 返回 (nil, false, nil)，AcquireTaskLock 是 SETNX，
// DelTaskResult 真的把 key 删掉。结果缓存的 Get/Set 不属于 Cacher 接口
// （生产代码里没有任何调用方，见 README 已知缺口 #2），所以这里只保留一个可读写的
// results map 供 warm/回读。
type fakeCache struct {
	faultInjector
	log     *callLog
	results map[string]string
	locks   map[string]int
	probe   *ctxProbe
}

func newFakeCache(log *callLog, probe *ctxProbe) *fakeCache {
	return &fakeCache{log: log, results: map[string]string{}, locks: map[string]int{}, probe: probe}
}

func (f *fakeCache) Ping(ctx context.Context) error {
	f.probe.record("cache.Ping", ctx)
	f.log.add("cache.Ping")
	return f.fail("Ping")
}

func (f *fakeCache) AcquireTaskLock(ctx context.Context, workerTaskID string) (bool, error) {
	key := keyTaskLock(workerTaskID)
	f.probe.record("cache.AcquireTaskLock", ctx)
	f.log.add("cache.AcquireTaskLock:%s", key)
	if err := f.fail("AcquireTaskLock"); err != nil {
		return false, err
	}
	if f.locks[key] > 0 {
		return false, nil // SETNX：已存在即抢锁失败
	}
	f.locks[key] = 1
	return true, nil
}

func (f *fakeCache) ReleaseTaskLock(ctx context.Context, workerTaskID string) error {
	key := keyTaskLock(workerTaskID)
	f.probe.record("cache.ReleaseTaskLock", ctx)
	f.log.add("cache.ReleaseTaskLock:%s", key)
	if err := f.fail("ReleaseTaskLock"); err != nil {
		return err
	}
	delete(f.locks, key)
	return nil
}

func (f *fakeCache) DelTaskResult(ctx context.Context, workerTaskID string) error {
	key := keyTaskResult(workerTaskID)
	f.probe.record("cache.DelTaskResult", ctx)
	f.log.add("cache.DelTaskResult:%s", key)
	if err := f.fail("DelTaskResult"); err != nil {
		return err // 真实实现会把 Redis 错误传出去；Repository.FinishTask 吞掉它（缺陷 #2）
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	delete(f.results, key)
	return nil
}

// warm 静默预热一条结果缓存（布数据，不记轨迹）：用来证明「终态写入后缓存确实被删了」。
func (f *fakeCache) warm(workerTaskID, payload string) {
	f.results[keyTaskResult(workerTaskID)] = payload
}

// cached 读缓存里剩下的结果（miss 返回 ""、false）。
func (f *fakeCache) cached(workerTaskID string) (string, bool) {
	v, ok := f.results[keyTaskResult(workerTaskID)]
	return v, ok
}

func (f *fakeCache) locked(workerTaskID string) bool {
	return f.locks[keyTaskLock(workerTaskID)] > 0
}

// --- worker_task model 替身（实现 model.WorkerTaskModel） ---

type fakeWorkerModel struct {
	faultInjector
	log   *callLog
	rows  map[string]*model.WorkerTask
	order []string // 插入顺序：FindByTaskID 的 mtime 并列时用后写入者，保证替身可复现
	// writtenStates 记录被测代码**要求写入**的状态（含注入失败的那次），
	// 用于「终态只能落在合法枚举里」这条不变量。
	writtenStates []int32
	probe         *ctxProbe
}

func newFakeWorkerModel(log *callLog, probe *ctxProbe) *fakeWorkerModel {
	return &fakeWorkerModel{log: log, rows: map[string]*model.WorkerTask{}, probe: probe}
}

// ctxErr 复刻真实驱动的行为：go-zero 的 ExecCtx/QueryRowCtx/SetexCtx 都吃 ctx，
// 请求被取消时写入必然失败。替身不 honour 取消就等于「永不失败的替身」，
// 也就看不到「取消的请求留下一条 PENDING 行」这个真实后果（见缺陷 #3 用例）。
func ctxErr(ctx context.Context) error { return ctx.Err() }

// Upsert 复刻真实 SQL 的 ON DUPLICATE KEY UPDATE mtime = VALUES(mtime)：
// 主键已存在时**只改 mtime**，其余列保持库存值（含 state、media_uri、task_id）。
// 真实 model 在驱动报错时以 %w 包装，这里直接返回注入的错误原文（logic 再包一层）。
func (f *fakeWorkerModel) Upsert(ctx context.Context, task *model.WorkerTask) error {
	f.probe.record("Upsert", ctx)
	f.log.add("worker.Upsert:%s/%s", task.WorkerTaskID, task.TaskID)
	if err := f.fail("Upsert"); err != nil {
		return err
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	f.put(task)
	return nil
}

// put 是 Upsert 的实际写入实现；seedTask 直接调它布数据（不记轨迹，见纪律 4）。
func (f *fakeWorkerModel) put(task *model.WorkerTask) {
	if row, ok := f.rows[task.WorkerTaskID]; ok {
		row.Mtime = task.Mtime // 与 ON DUPLICATE 同口径：只有 mtime 被覆盖
		return
	}
	cp := *task
	f.rows[cp.WorkerTaskID] = &cp
	f.order = append(f.order, cp.WorkerTaskID)
}

// UpdateResult 复刻 UPDATE ... WHERE worker_task_id=?：未命中即 RowsAffected==0 ⇒ ErrTaskNotFound。
func (f *fakeWorkerModel) UpdateResult(ctx context.Context, workerTaskID string, state int32,
	algorithmVersion string, elapsedMs int64, resultJSON, errorMessage string) error {
	f.probe.record("UpdateResult", ctx)
	f.log.add("worker.UpdateResult:%s:%d", workerTaskID, state)
	f.writtenStates = append(f.writtenStates, state)
	if err := f.fail("UpdateResult"); err != nil {
		return err
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	row, ok := f.rows[workerTaskID]
	if !ok {
		return model.ErrTaskNotFound
	}
	row.State = state
	row.AlgorithmVersion = algorithmVersion
	row.ElapsedMs = elapsedMs
	row.ResultJSON = resultJSON
	row.ErrorMessage = errorMessage
	row.Mtime = model.NowUnix()
	return nil
}

func (f *fakeWorkerModel) FindOne(ctx context.Context, workerTaskID string) (*model.WorkerTask, error) {
	f.probe.record("FindOne", ctx)
	f.log.add("worker.FindOne:%s", workerTaskID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	row, ok := f.rows[workerTaskID]
	if !ok {
		return nil, nil // 与 model 一致：sql.ErrNoRows → (nil, nil)
	}
	cp := *row
	return &cp, nil
}

// FindByTaskID 复刻 WHERE task_id=? ORDER BY mtime DESC LIMIT 1。
// 真实 SQL 在 mtime 相等时不确定，因此涉及本方法的用例布景必须给出不同 mtime；
// 替身把并列情况判给后写入者，保证用例可复现。
func (f *fakeWorkerModel) FindByTaskID(ctx context.Context, taskID string) (*model.WorkerTask, error) {
	f.probe.record("FindByTaskID", ctx)
	f.log.add("worker.FindByTaskID:%s", taskID)
	if err := f.fail("FindByTaskID"); err != nil {
		return nil, err
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	var best *model.WorkerTask
	for _, id := range f.order {
		row := f.rows[id]
		if row.TaskID != taskID {
			continue
		}
		if best == nil || row.Mtime >= best.Mtime {
			cp := *row
			best = &cp
		}
	}
	return best, nil
}

// --- 库存回读（一律给副本，见纪律 1） ---

func (f *fakeWorkerModel) snapshot(workerTaskID string) *model.WorkerTask {
	row, ok := f.rows[workerTaskID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeWorkerModel) count() int { return len(f.rows) }

// states 返回库存里所有行的 state（排序后），用于「没多出半截行/没写脏状态」的断言。
func (f *fakeWorkerModel) states() []int32 {
	out := make([]int32, 0, len(f.rows))
	for _, id := range f.rows {
		out = append(out, id.State)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- 装配 ---

type store struct {
	log    *callLog
	probe  *ctxProbe
	cache  *fakeCache
	worker *fakeWorkerModel
	repo   *repository.Repository
	svcCtx *svc.ServiceContext
}

// newStore 组装真实 Repository（依赖为替身）。defaultTimeoutMs 是 config 的默认超时，
// etc/moderation.worker.v1.yaml 生产示例值为 60000。
func newStore(defaultTimeoutMs int64) *store {
	log := &callLog{}
	probe := newCtxProbe()
	st := &store{
		log:    log,
		probe:  probe,
		cache:  newFakeCache(log, probe),
		worker: newFakeWorkerModel(log, probe),
	}
	// conn 传 nil：本服务没有任何 Repository 方法用到 sqlx.SqlConn（单表写入、无跨表事务）。
	st.repo = repository.NewWithDeps(st.cache, nil, st.worker)
	st.svcCtx = &svc.ServiceContext{
		Config:     config.Config{DefaultTimeoutMs: defaultTimeoutMs},
		Repository: st.repo,
	}
	return st
}

func newEnv() *store { return newStore(60000) }

// seedTask 静默布一行 worker_task（不记轨迹，纪律 4），返回库内那一行的副本。
func seedTask(t *testing.T, st *store, task *model.WorkerTask) *model.WorkerTask {
	t.Helper()
	if task.WorkerTaskID == "" {
		t.Fatal("布景必须给定 worker_task_id，否则无法回读那一行")
	}
	st.worker.put(task)
	return st.worker.snapshot(task.WorkerTaskID)
}

// legalTaskStates 是与 proto TaskState 对齐的合法状态集合（不含 0=UNSPECIFIED）。
func legalTaskStates() []int32 {
	return []int32{
		model.TaskStatePending, model.TaskStateRunning, model.TaskStateSucceeded,
		model.TaskStateFailed, model.TaskStateTimeout,
	}
}

// wantLegalTaskState 断言一个 state 落在合法枚举里。
// 0（UNSPECIFIED，DB 列默认值）与任何越界值都不算合法：它会让 orchestrator 读到一个
// 既不是「执行中」也不是「终态」的任务，只能靠人工兜底。
func wantLegalTaskState(t *testing.T, label string, state int32) {
	t.Helper()
	if !slices.Contains(legalTaskStates(), state) {
		t.Errorf("%s：状态 %d 不在合法 TaskState %v 内", label, state, legalTaskStates())
	}
}

// errEngineTimeout / errEngineUnknown 是本服务里「下游审核引擎」在现阶段的最佳替身：
// worker 唯一的两个下游是 worker_task 写入与结果缓存，占位实现不调用任何算法客户端
// （见 README 约束与 internal/svc）。因此这两个错误只用于注入**写库/失效缓存**失败，
// 名字保留 engine 语义是为了让「失败必须如实传出、不得伪装成审核通过」这条断言
// 在未来接入真算法后仍然指向同一个方向。
var (
	errEngineTimeout    = errors.New("injected downstream timeout")
	errCacheUnavailable = errors.New("injected cache unavailable")
)
