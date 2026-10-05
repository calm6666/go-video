package logic

// fakes_test.go 是 content-fingerprint logic 测试的内存替身集合。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, fakeConn, 内存 taskMd, 内存 recMd) 组装**真实的 Repository**，
// 只把它的 4 个依赖换成替身——这样「缓存读穿/回填/失效、task_id 回填 SQL、
// 状态推进后写指纹事实」整条判定链都在被测路径上，而不是把 Repository 也 mock 掉。
// 见 internal/repository/repository.go 的 Cacher 注释。
//
// 四条替身纪律（catalog / rights / playback 三轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 里对返回值的写回不得污染库存行，
//     否则「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 副作用按真实 SQL 的口径处理主键与唯一键：Insert 忽略入参 ID、自增分配并返回，
//     并复刻 fingerprint_task 的 uniq_asset_fptype / uniq_task_id（task_id 默认 0 也占唯一索引槽）
//     与 fingerprint_record 的 ON DUPLICATE KEY UPDATE（换 key/hash/ctime、不换主键）。
//  3. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：本域要紧的结论是
//     「先读缓存还是先查库」「状态推进前后有没有写指纹事实」「提交任务后有没有回填 task_id」。
//  4. 布数据走替身的**静默写入路径**（seedTask / seedRecord / cache.warm → put 而非公开的
//     Insert / Upsert）：布景不算被测调用，因此轨迹断言可以直接从 0 开始数。
//     如果哪天改成用公开方法布景，就必须先在布景之后取 before := st.log.snapshot()。
//
// 错误注入按方法粒度（failWith("Upsert", err)），不用一个全局 err。
//
// 覆盖边界（如实声明，四条）：
//   - 替身只复刻 model 层 SQL 的**语义**（过滤条件、状态门槛、唯一键冲突、分页裁剪），
//     不证明 SQL 文本与列名本身；`model/fingerprintmodel.go` 没有列级单测，仓库里也没有
//     本服务的迁移↔model 对账门禁。
//   - model 层的状态机校验（PENDING → SUCCEEDED/FAILED）被替身复刻，因此本包断言的是
//     「logic/Repository 有没有如实把 ErrIllegalState 传出、有没有在失败后留下半截数据」，
//     不是「状态机本身写对没有」。
//   - `fakeRecordModel.FindByKey` 默认严格复刻生产占位实现（不查库、恒返回空）；
//     只有显式 `st.rec.searchEnabled = true` 才按 (key, fp_type) 召回候选，且仅用于锁定
//     logic 的**投影与顺序契约**——生产当前不可能返回这些行，接上检索引擎后这组用例才代表现网。
//     替身不做相似度打分：rpc.MatchItem.Score 由 recordToItem 恒置 0，用例把 0 钉死。
//   - fakeConn 只认 SubmitTask 里那条 task_id 回填 UPDATE 的**逐字 SQL**；出现其它直连 SQL
//     或未实现的 sqlx 方法会 panic，等价于「logic 测试路径上不允许越界访问数据库」。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/content-fingerprint/internal/config"
	"go-video/services/content-fingerprint/internal/repository"
	"go-video/services/content-fingerprint/internal/svc"
	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"
)

// --- 断言小工具（本包共享；与其它包同名不同文件，互不影响） ---

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

// wantErrContains 断言错误文本含子串：用于没有哨兵、只能靠包装文本定位的分支
// （例如 Repository.GetTask 的缓存解码失败）。
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

// wantCount 断言某类调用发生的次数（前缀统计）。布数据走静默写入路径，
// 因此这里数的是**被测代码**的调用。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整序列 [%s]）", label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

// wantUnixInRange 断言墙钟派生值落在用例起止时间内：model 用 time.Now()（不可注入），
// 因此只断言区间，不依赖秒级边界。
func wantUnixInRange(t *testing.T, label, field string, got, from, to int64) {
	t.Helper()
	if got < from || got > to {
		t.Errorf("%s：%s = %d, want 落在 [%d, %d]", label, field, got, from, to)
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

// itoa 拼调用轨迹里的整数段：轨迹由替身用 %d 格式化，用例期望值必须同源，
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

func keyTask(taskID int64) string { return fmt.Sprintf("fp:task:%d", taskID) }

// 唯一键冲突的替身错误：文本对齐 MySQL 1062，用例只断言 errors.Is(err, errDuplicateKey)
// 以及**哪个索引**撞了（用于区分「同媒资同类型重复提交」与「task_id 未回填导致撞 0 槽」）。
var errDuplicateKey = errors.New("Error 1062: Duplicate entry")

func dupKey(index string, detail string) error {
	return fmt.Errorf("%w for key '%s': %s", errDuplicateKey, index, detail)
}

func ukAssetFp(assetID int64, fpType int32) string { return fmt.Sprintf("%d/%d", assetID, fpType) }

// --- 缓存替身 ---

// fakeCache 实现 repository.Cacher。miss 返回 hit=false 且 err=nil，与真实 Cache
// 处理 redis.Nil 的口径一致。
//
// 这里**故意只实现 Cacher 的 4 个方法**：repository.Cache 还带着 GetMatchKey/SetMatchKey/
// GetMatchAsset/SetMatchAsset 四个 Match 缓存方法，但 Repository 里一个调用点都没有
// （fp:match:* 是死键，见 README 已知缺口）。替身不去实现接口外的方法，
// 否则「缓存调用次数」的断言会被替身自己的死代码污染。
type fakeCache struct {
	faultInjector
	log   *callLog
	tasks map[string]string
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{log: log, tasks: map[string]string{}}
}

func (f *fakeCache) Ping(context.Context) error { f.log.add("cache.Ping"); return nil }

func (f *fakeCache) GetTask(_ context.Context, taskID int64) (string, bool, error) {
	key := keyTask(taskID)
	f.log.add("cache.Get:%s", key)
	if err := f.fail("GetTask"); err != nil {
		return "", false, err
	}
	payload, ok := f.tasks[key]
	return payload, ok, nil
}

func (f *fakeCache) SetTask(_ context.Context, taskID int64, payload string) error {
	key := keyTask(taskID)
	f.log.add("cache.Set:%s", key)
	if err := f.fail("SetTask"); err != nil {
		return err
	}
	f.tasks[key] = payload
	return nil
}

func (f *fakeCache) DelTask(_ context.Context, taskID int64) error {
	key := keyTask(taskID)
	f.log.add("cache.Del:%s", key)
	if err := f.fail("DelTask"); err != nil {
		return err
	}
	delete(f.tasks, key)
	return nil
}

// warm 直接预热任务缓存（布数据，不写轨迹：命中用例要区分「读穿回填」和「本来就命中」）。
func (f *fakeCache) warm(taskID int64, payload string) {
	f.tasks[keyTask(taskID)] = payload
}

// warmTask 按 Repository 的编码方式（json.Marshal(model.FingerprintTask)）预热一条任务缓存。
func (f *fakeCache) warmTask(task *model.FingerprintTask) {
	f.warm(task.TaskID, mustMarshalTask(task))
}

// mustMarshalTask 与 Repository.GetTask 回填时用的是同一次编码，
// 因此「缓存命中」用例喂进去的 JSON 字段名必须和 model 结构体一致。
func mustMarshalTask(task *model.FingerprintTask) string {
	bs, err := json.Marshal(task)
	if err != nil {
		panic(fmt.Sprintf("布景编码任务缓存失败：%v", err))
	}
	return string(bs)
}

// storedTask 解码缓存里存的任务；miss 或解码失败返回 nil。
// 单独解码一次，才能断言「回填的是哪一行、是不是完整字段」。
func (f *fakeCache) storedTask(taskID int64) *model.FingerprintTask {
	payload, ok := f.stored(taskID)
	if !ok {
		return nil
	}
	var t model.FingerprintTask
	if err := json.Unmarshal([]byte(payload), &t); err != nil {
		return nil
	}
	return &t
}

// stored 读缓存里实际存了什么 JSON。
func (f *fakeCache) stored(taskID int64) (string, bool) {
	p, ok := f.tasks[keyTask(taskID)]
	return p, ok
}

// --- 任务 model 替身 ---

// 静默写入用 put；被测路径用 Insert/FindOne/List/UpdateResult（记轨迹）。
type fakeTaskModel struct {
	faultInjector
	log       *callLog
	rows      map[int64]*model.FingerprintTask // key = id
	byTask    map[int64]int64                  // uniq_task_id：task_id → id（0 也占槽）
	byAssetFp map[string]int64                 // uniq_asset_fptype：asset_id/fp_type → id
	next      int64
}

func newFakeTaskModel(log *callLog) *fakeTaskModel {
	return &fakeTaskModel{
		log:       log,
		rows:      map[int64]*model.FingerprintTask{},
		byTask:    map[int64]int64{},
		byAssetFp: map[string]int64{},
	}
}

// put 是静默写入路径（纪律 4），同时是真实唯一键口径的实现处。
// 入参不被改写（真实 model 只改写 Ctime/Mtime/State，见 Insert）。
func (f *fakeTaskModel) put(in *model.FingerprintTask) (*model.FingerprintTask, error) {
	cp := *in
	if cp.ID == 0 {
		f.next++
		cp.ID = f.next
	}
	if _, ok := f.rows[cp.ID]; ok {
		return nil, dupKey("PRIMARY", fmt.Sprintf("id=%d", cp.ID))
	}
	if other, ok := f.byAssetFp[ukAssetFp(cp.AssetID, cp.FpType)]; ok {
		return nil, dupKey("uniq_asset_fptype", fmt.Sprintf("asset_id=%d,fp_type=%d（与 id=%d 冲突）", cp.AssetID, cp.FpType, other))
	}
	if other, ok := f.byTask[cp.TaskID]; ok && other != cp.ID {
		return nil, dupKey("uniq_task_id", fmt.Sprintf("task_id=%d（与 id=%d 冲突）", cp.TaskID, other))
	}
	f.rows[cp.ID] = &cp
	f.byAssetFp[ukAssetFp(cp.AssetID, cp.FpType)] = cp.ID
	f.byTask[cp.TaskID] = cp.ID
	out := cp
	return &out, nil
}

// Insert 复刻真实 SQL：主键自增、ctime/mtime 取 now、state 为 0 时回落 PENDING。
func (f *fakeTaskModel) Insert(_ context.Context, t *model.FingerprintTask) (int64, error) {
	f.log.add("task.Insert:%d/%d", t.AssetID, t.FpType)
	if err := f.fail("Insert"); err != nil {
		return 0, fmt.Errorf("fingerprint_task Insert: %w", err)
	}
	now := time.Now().Unix()
	t.Ctime = now
	t.Mtime = now
	if t.State == 0 {
		t.State = model.TaskStatePending
	}
	saved, err := f.put(t)
	if err != nil {
		return 0, fmt.Errorf("fingerprint_task Insert: %w", err)
	}
	return saved.ID, nil
}

// FindOne 复刻 WHERE task_id = ?（不是 WHERE id = ?）；查无此行返回 (nil, nil)。
func (f *fakeTaskModel) FindOne(_ context.Context, taskID int64) (*model.FingerprintTask, error) {
	f.log.add("task.FindOne:%d", taskID)
	if err := f.fail("FindOne"); err != nil {
		return nil, fmt.Errorf("fingerprint_task FindOne: %w", err)
	}
	id, ok := f.byTask[taskID]
	if !ok {
		return nil, nil
	}
	cp := *f.rows[id]
	return &cp, nil
}

// List 复刻 model 的 SQL 语义：可选 asset_id / state 过滤、ORDER BY ctime DESC、LIMIT/OFFSET 裁剪。
// pn/ps 的钳制（pn<1→1、ps∉[1,50]→20）也来自 model，所以这里的断言是「透传值 + 复刻钳制」，
// 不证明 SQL 文本；total=0 时 model 返回 (nil, 0, nil)，本替身同口径。
func (f *fakeTaskModel) List(_ context.Context, assetID int64, state, pn, ps int32) ([]*model.FingerprintTask, int32, error) {
	f.log.add("task.List:%d/%d/%d/%d", assetID, state, pn, ps)
	if err := f.fail("List"); err != nil {
		return nil, 0, fmt.Errorf("fingerprint_task List count: %w", err)
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	ids := make([]int64, 0, len(f.rows))
	for id, row := range f.rows {
		if assetID > 0 && row.AssetID != assetID {
			continue
		}
		if state > 0 && row.State != state {
			continue
		}
		ids = append(ids, id)
	}
	// 真实 SQL 在 ctime 相等时顺序不确定，因此布景必须给不同 ctime（见 seedTask）。
	sort.Slice(ids, func(i, j int) bool {
		a, b := f.rows[ids[i]], f.rows[ids[j]]
		if a.Ctime != b.Ctime {
			return a.Ctime > b.Ctime
		}
		return ids[i] > ids[j]
	})
	total := int32(len(ids))
	if total == 0 {
		return nil, 0, nil
	}
	offset := int(pn-1) * int(ps)
	if offset >= len(ids) {
		return nil, total, nil
	}
	end := offset + int(ps)
	if end > len(ids) {
		end = len(ids)
	}
	out := make([]*model.FingerprintTask, 0, end-offset)
	for _, id := range ids[offset:end] {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, total, nil
}

// UpdateResult 复刻 model 的实现：先读旧行校验状态机，再 UPDATE ... WHERE task_id=? AND state=PENDING，
// 最后回读。错误文本与哨兵（ErrTaskNotFound / ErrIllegalState）逐字对齐。
func (f *fakeTaskModel) UpdateResult(ctx context.Context, taskID int64, state int32, videoKey, audioKey string) (*model.FingerprintTask, error) {
	old, err := f.FindOne(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, model.ErrTaskNotFound
	}
	if old.State != model.TaskStatePending {
		return nil, fmt.Errorf("%w: task %d state=%d, expect PENDING", model.ErrIllegalState, taskID, old.State)
	}
	if state != model.TaskStateSucceeded && state != model.TaskStateFailed {
		return nil, fmt.Errorf("%w: target state=%d, expect SUCCEEDED/FAILED", model.ErrIllegalState, state)
	}
	f.log.add("task.Update:%d/%d", taskID, state)
	if err := f.fail("Update"); err != nil {
		return nil, fmt.Errorf("fingerprint_task UpdateResult: %w", err)
	}
	f.rows[old.ID].State = state
	f.rows[old.ID].VideoKey = videoKey
	f.rows[old.ID].AudioKey = audioKey
	f.rows[old.ID].Mtime = time.Now().Unix()
	return f.FindOne(ctx, taskID)
}

// backfillTaskID 复刻 "UPDATE fingerprint_task SET task_id = id WHERE id = ? AND task_id = 0"。
// WHERE 未命中（行不存在或 task_id 已非 0）时受影响 0 行，且不算错误。
func (f *fakeTaskModel) backfillTaskID(id int64) int64 {
	row, ok := f.rows[id]
	if !ok || row.TaskID != 0 {
		return 0
	}
	delete(f.byTask, row.TaskID)
	row.TaskID = id
	f.byTask[id] = id
	return 1
}

// row 读库内当前行（值读，避免用例误改库存行）；按 id 取，与 task_id 无关。
func (f *fakeTaskModel) row(id int64) *model.FingerprintTask {
	row, ok := f.rows[id]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// count 库里任务行数。
func (f *fakeTaskModel) count() int { return len(f.rows) }

// --- 指纹事实 model 替身 ---

type fakeRecordModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.FingerprintRecord
	byUK map[string]int64 // uniq_asset_fptype：asset_id/fp_type → id
	next int64
	// searchEnabled=false 时 FindByKey 严格复刻生产占位（不查库、恒返回空）。
	searchEnabled bool
}

func newFakeRecordModel(log *callLog) *fakeRecordModel {
	return &fakeRecordModel{log: log, rows: map[int64]*model.FingerprintRecord{}, byUK: map[string]int64{}}
}

// put 静默写入 + Upsert 的 ON DUPLICATE KEY UPDATE 口径：命中 (asset_id, fp_type)
// 时替换 key/hash/ctime 但**不换主键**，否则插入新行。返回是否新建。
func (f *fakeRecordModel) put(in *model.FingerprintRecord) (created bool) {
	cp := *in
	if id, ok := f.byUK[ukAssetFp(cp.AssetID, cp.FpType)]; ok {
		row := f.rows[id]
		row.Key = cp.Key
		row.Hash = cp.Hash
		row.Ctime = cp.Ctime
		return false
	}
	if cp.ID == 0 {
		f.next++
		cp.ID = f.next
	}
	f.rows[cp.ID] = &cp
	f.byUK[ukAssetFp(cp.AssetID, cp.FpType)] = cp.ID
	return true
}

func (f *fakeRecordModel) Upsert(_ context.Context, r *model.FingerprintRecord) error {
	f.log.add("rec.Upsert:%d/%d/%s", r.AssetID, r.FpType, r.Key)
	r.Ctime = time.Now().Unix()
	if err := f.fail("Upsert"); err != nil {
		return fmt.Errorf("fingerprint_record Upsert: %w", err)
	}
	f.put(r)
	return nil
}

// FindByAsset 复刻 WHERE asset_id = ? [AND fp_type = ?]（fpType<=0 不限定类型）。
// 真实 SQL 没有 ORDER BY，同 asset 最多 2 行；替身按主键升序返回，用例只断言
// 「逻辑不改顺序」，不把顺序当契约。
func (f *fakeRecordModel) FindByAsset(_ context.Context, assetID int64, fpType int32) ([]*model.FingerprintRecord, error) {
	f.log.add("rec.FindByAsset:%d/%d", assetID, fpType)
	if err := f.fail("FindByAsset"); err != nil {
		return nil, fmt.Errorf("fingerprint_record FindByAsset: %w", err)
	}
	return f.match(func(r *model.FingerprintRecord) bool {
		return r.AssetID == assetID && (fpType <= 0 || r.FpType == fpType)
	}), nil
}

// FindByKey 生产实现是占位（不查库、恒返回空）。默认逐字复刻该行为；
// searchEnabled 之后按 (key, fp_type) + topN 召回，只用于锁定 logic 的投影契约。
func (f *fakeRecordModel) FindByKey(_ context.Context, fpKey string, fpType int32, topN int32) ([]*model.FingerprintRecord, error) {
	f.log.add("rec.FindByKey:%s/%d/%d", fpKey, fpType, topN)
	if err := f.fail("FindByKey"); err != nil {
		return nil, fmt.Errorf("fingerprint_record FindByKey: %w", err)
	}
	if !f.searchEnabled {
		return nil, nil
	}
	rows := f.match(func(r *model.FingerprintRecord) bool {
		return r.Key == fpKey && (fpType <= 0 || r.FpType == fpType)
	})
	if topN > 0 && int32(len(rows)) > topN {
		rows = rows[:topN]
	}
	return rows, nil
}

func (f *fakeRecordModel) match(keep func(*model.FingerprintRecord) bool) []*model.FingerprintRecord {
	ids := make([]int64, 0, len(f.rows))
	for id, row := range f.rows {
		if keep(row) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*model.FingerprintRecord, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out
}

// row 读库内某 (asset_id, fp_type) 的指纹事实（值读）；不存在返回 nil。
func (f *fakeRecordModel) row(assetID int64, fpType int32) *model.FingerprintRecord {
	id, ok := f.byUK[ukAssetFp(assetID, fpType)]
	if !ok {
		return nil
	}
	cp := *f.rows[id]
	return &cp
}

func (f *fakeRecordModel) count() int { return len(f.rows) }

// keys 列出库存指纹事实（含主键），便于断言「重复上报后只剩一条、主键没换、key/hash 被替换」。
func (f *fakeRecordModel) keys() []string {
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		r := f.rows[id]
		out = append(out, fmt.Sprintf("id=%d/asset=%d/type=%d/key=%s/hash=%s", id, r.AssetID, r.FpType, r.Key, r.Hash))
	}
	return out
}

// --- 事务/直连 SQL 替身 ---

// sqlBackfillTaskID 与 repository.SubmitTask 里那条语句逐字对齐；文本变了这里就 panic，
// 逼着改动人同步更新用例（回填 task_id 是跨服务引用的前提）。
const sqlBackfillTaskID = "UPDATE fingerprint_task SET task_id = id WHERE id = ? AND task_id = 0"

type fakeResult struct {
	lastID   int64
	affected int64
}

func (r fakeResult) LastInsertId() (int64, error) { return r.lastID, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.affected, nil }

// fakeConn 只实现 Repository 真正用到的 ExecCtx（task_id 回填）。
// 其它方法落在嵌入的 nil 接口上会直接 panic，等价于「logic 测试路径上不允许出现别的直连 SQL」。
type fakeConn struct {
	sqlx.SqlConn
	log  *callLog
	task *fakeTaskModel
	faultInjector
}

func (f *fakeConn) ExecCtx(_ context.Context, query string, args ...any) (sql.Result, error) {
	if query != sqlBackfillTaskID {
		panic("content-fingerprint 测试路径上出现未预期的直连 SQL: " + query)
	}
	id, ok := args[0].(int64)
	if !ok {
		panic(fmt.Sprintf("task_id 回填的 id 参数类型意外：%T", args[0]))
	}
	f.log.add("conn.BackfillTaskID:%d", id)
	if err := f.fail("BackfillTaskID"); err != nil {
		return nil, fmt.Errorf("fingerprint_task backfill task_id: %w", err)
	}
	return fakeResult{lastID: id, affected: f.task.backfillTaskID(id)}, nil
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

// --- 装配 ---

type store struct {
	log   *callLog
	cache *fakeCache
	task  *fakeTaskModel
	rec   *fakeRecordModel
	conn  *fakeConn
	repo  *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	st := &store{log: log, cache: newFakeCache(log)}
	st.task = newFakeTaskModel(log)
	st.conn = &fakeConn{log: log, task: st.task}
	st.rec = newFakeRecordModel(log)
	st.repo = repository.NewWithDeps(st.cache, st.conn, st.task, st.rec)
	return st
}

// newTestSvc 装配只带内存依赖的 ServiceContext。与生产 ServiceContext 的差别仅在
// Repository 的 4 个依赖是替身；Config 留零值（这 6 个 logic 只读 Repository）。
func newTestSvc(st *store) *svc.ServiceContext {
	return &svc.ServiceContext{Config: config.Config{}, Repository: st.repo}
}

// --- 布数据（静默路径，不写轨迹） ---

// seedTask 布一行任务；返回库内行的副本。TaskID 为 0 时按生产终态对齐到 id，
// ctime 必须由用例显式给出（List 的排序契约依赖它）。
func seedTask(t *testing.T, st *store, task *model.FingerprintTask) *model.FingerprintTask {
	t.Helper()
	if task.State == 0 {
		task.State = model.TaskStatePending
	}
	saved, err := st.task.put(task)
	if err != nil {
		t.Fatalf("布景写入任务失败：%v", err)
	}
	if saved.TaskID == 0 {
		st.task.backfillTaskID(saved.ID)
	}
	return st.task.row(saved.ID)
}

// seedRecord 布一行指纹事实。
func seedRecord(t *testing.T, st *store, rec *model.FingerprintRecord) *model.FingerprintRecord {
	t.Helper()
	if rec.Ctime == 0 {
		rec.Ctime = time.Now().Unix()
	}
	st.rec.put(rec)
	return st.rec.row(rec.AssetID, rec.FpType)
}

// --- 投影断言辅助 ---

// itemLine 把一条 MatchItem 摊成可比较文本：辨识度值（asset_id/fp_type/key/hash/score）逐字段进断言。
func itemLine(i *rpc.MatchItem) string {
	return fmt.Sprintf("asset=%d/type=%v/key=%s/hash=%s/score=%v",
		i.GetAssetId(), i.GetFpType(), i.GetFpKey(), i.GetFpHash(), i.GetScore())
}

func itemLines(items []*rpc.MatchItem) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, itemLine(i))
	}
	return out
}

// taskLine 把库存任务行摊成文本（不含 ctime/mtime：那是墙钟派生值，单独用区间断言）。
func taskLine(t *model.FingerprintTask) string {
	if t == nil {
		return "<nil>"
	}
	return fmt.Sprintf("task=%d/asset=%d/type=%d/vkey=%s/akey=%s/state=%d",
		t.TaskID, t.AssetID, t.FpType, t.VideoKey, t.AudioKey, t.State)
}

// replyLine 把 TaskReply 摊成**与 taskLine 同形**的文本，于是「RPC 投影 == 库存行」
// 就是一条可比对的等式；枚举按编号摊开，避免 rpc 枚举与 model 常量的偏移被字符串掩盖。
func replyLine(r *rpc.TaskReply) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("task=%d/asset=%d/type=%d/vkey=%s/akey=%s/state=%d",
		r.GetTaskId(), r.GetAssetId(), int32(r.GetFpType()), r.GetVideoKey(), r.GetAudioKey(), int32(r.GetState()))
}

func replyLines(rs []*rpc.TaskReply) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, replyLine(r))
	}
	return out
}
