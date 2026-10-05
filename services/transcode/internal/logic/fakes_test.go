package logic

// fakes_test.go 是 transcode logic 测试的内存替身集合。
//
// 为什么需要注入缝：transcode 的 ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, fakeConn, 内存 taskMd/tplMd) 组装**真实的 Repository**，
// 只把它的 4 个依赖换成替身——这样「缓存读穿/回填/失效、终态判定读的是缓存还是库、
// UPDATE 有没有 CAS」整条链路都在被测路径上，而不是把 Repository 也 mock 掉。
// 见 internal/repository/repository.go 的 Cacher 注释。
//
// 五条替身纪律（catalog / rights / playback / comment 四轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 里的写回不得污染库存行，否则
//     「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：transcode 要紧的结论是
//     「拒绝推进时有没有留下半截写入」「UPDATE 之后先删缓存还是先回源」「COUNT 为 0 时
//     还发不发第二条 SELECT」——这些都只有序列能表达。
//  3. 错误注入按**依赖.方法**粒度（st.fail("transcode_task.UpdateProgress", err)），
//     不用一个全局 err。
//  4. 布数据走替身的**静默写入路径**（seedTask / seedTemplate / warmTaskCache）：布景不记轨迹，
//     因此序列断言可以直接从 0 开始数（wantSeq(..., 0, ...)）。
//  5. 并发窗口用 before(key, 第 n 次调用, fn) 钩子复现（生产没有 session 级校验读、
//     UPDATE 也没有 `AND state = ?` 条件，纯内存只能靠插队钩子钉住），用例结尾必须
//     checkHooks 确认钩子真的触发过，否则断言在空转。
//
// Redis key 与 model SQL 语义的对齐方式：keyTask/keyTemplate 与 repository 未导出常量逐字复刻，
// 不一致时缓存命中类用例即红；List 的 `pn<1→1`、`ps<1||ps>50→20` 夹紧、`WHERE 1=1` 动态过滤、
// 任务的 `ORDER BY task_id DESC` 与模板的 `ORDER BY template_id ASC`（两条方向**相反**，
// 所以任一条写反都会被对照用例抓到）、`COUNT` 先判 0 再取行、`RowsAffected==0 → ErrTaskNotFound`、
// `transcode_template.uniq_name` 冲突，均按 model/*.go 的 SQL 同语义复刻。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**，不证明 SQL 与列名本身（要证明得引入 sqlmock，超本轮范围）；
//   - 不做 LIMIT/OFFSET 切片的**正确性**验证：fake 里的 `offset=(pn-1)*ps` 是把
//     `model/transcodemodel.go:85,208` 的算式抄了一遍，所以这条算式写错用例不会变红，
//     翻页「不重不漏」也只能证明到「与本文件同一套算术自洽」；
//   - MySQL 的 changed-rows 口径（DSN 未开 clientFoundRows，见 etc/transcode.v1.yaml:14）
//     替身按 matched-rows 复刻，所以「同一秒内重复上报完全相同的进度 → RowsAffected=0 →
//     被判 ErrTaskNotFound」这条只在 README 登记，纯内存用例证明不了；
//   - 真 Redis 的 SETEX/DEL 与 TTL 编码不在断言范围内（TTL 常量藏在 *Cache 里，
//     Cacher 接口签名不带 TTL），模板缓存的失效路径（Cache.DelTemplate）零调用方，也没有用例。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/transcode/internal/repository"
	"go-video/services/transcode/internal/svc"
	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"
)

// --- 断言小工具（本包共享；与其它服务同包名不同文件，互不影响） ---

// wantEq 比较两个可比值（字段级投影断言用这条，不做reflect.DeepEqual，避免漏掉新字段的语义）。
func wantEq[T comparable](t *testing.T, label, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, what, got, want)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵（repository/model 用 %w 包装下游错误是允许的）。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want errors.Is(%v)", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

// wantErrContains 锁定分支归属：model 用 `fmt.Errorf("transcode_task FindOne: %w", err)` 包一层，
// 只看「有错误」不足以分辨是哪一条语句报的。
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

// wantSeq 断言 from 之后**完整**的调用序列（顺序本身就是结论时用这条，默认 from=0）。
func wantSeq(t *testing.T, label string, log *callLog, from int, want ...string) {
	t.Helper()
	got := log.opsFrom(from)
	if !slices.Equal(got, want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label,
			strings.Join(got, " → "), strings.Join(want, " → "))
	}
}

// wantNoCall 断言 from 之后没有发生任何依赖调用：入参守卫必须发生在触库/触缓存之前。
func wantNoCall(t *testing.T, label string, log *callLog, from int) {
	t.Helper()
	if ops := log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

// wantCount 断言某类调用发生的次数（布数据走静默路径，所以数的是被测代码的调用）。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整序列 [%s]）",
			label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

// assertAround 断言墙钟派生值落在期望附近：logic 内部用 time.Now()，跨秒抖动不可避免。
// 需要精确相等的地方（如 ctime == mtime）一律用 wantEq，不用这条。
func assertAround(t *testing.T, label, what string, got, want, slack int64) {
	t.Helper()
	if got < want-slack || got > want+slack {
		t.Errorf("%s：%s = %d, want %d±%d", label, what, got, want, slack)
	}
}

// wantTaskReplyMatchesRow 逐字段（12 个）比对 rpc.TaskReply 与库存行。
// 用逐字段而不是 DeepEqual：新增 proto 字段时必须显式决定「回不回填」，
// DeepEqual 会把「忘了投影」掩盖成一次结构体比较失败。
func wantTaskReplyMatchesRow(t *testing.T, label string, got *rpc.TaskReply, row *model.TranscodeTask) {
	t.Helper()
	if got == nil || row == nil {
		t.Fatalf("%s：应答或库存行为 nil（got=%v row=%v）", label, got, row)
	}
	wantEq(t, label, "task_id", got.GetTaskId(), row.TaskId)
	wantEq(t, label, "asset_id", got.GetAssetId(), row.AssetId)
	wantEq(t, label, "template_id", got.GetTemplateId(), row.TemplateId)
	wantEq(t, label, "input_bucket", got.GetInputBucket(), row.InputBucket)
	wantEq(t, label, "input_key", got.GetInputKey(), row.InputKey)
	wantEq(t, label, "output_bucket", got.GetOutputBucket(), row.OutputBucket)
	wantEq(t, label, "output_key", got.GetOutputKey(), row.OutputKey)
	wantEq(t, label, "state", int32(got.GetState()), row.State)
	wantEq(t, label, "progress", got.GetProgress(), row.Progress)
	wantEq(t, label, "errno", got.GetErrno(), row.Errno)
	wantEq(t, label, "err_msg", got.GetErrMsg(), row.ErrMsg)
	wantEq(t, label, "ctime", got.GetCtime(), row.Ctime)
	wantEq(t, label, "mtime", got.GetMtime(), row.Mtime)
}

// wantTemplateReplyMatchesRow 逐字段比对 rpc.TemplateReply 与库存行。
// proto 的 TemplateReply 只有 8 个字段（无 ctime/mtime），所以这里刻意不比对时间列，
// 并由 CreateTemplate 用例单独断言「库存有时间、应答没有时间」。
func wantTemplateReplyMatchesRow(t *testing.T, label string, got *rpc.TemplateReply, row *model.TranscodeTemplate) {
	t.Helper()
	if got == nil || row == nil {
		t.Fatalf("%s：应答或库存行为 nil（got=%v row=%v）", label, got, row)
	}
	wantEq(t, label, "template_id", got.GetTemplateId(), row.TemplateId)
	wantEq(t, label, "name", got.GetName(), row.Name)
	wantEq(t, label, "codec", got.GetCodec(), row.Codec)
	wantEq(t, label, "width", got.GetWidth(), row.Width)
	wantEq(t, label, "height", got.GetHeight(), row.Height)
	wantEq(t, label, "bitrate", got.GetBitrate(), row.Bitrate)
	wantEq(t, label, "fps", got.GetFps(), row.Fps)
	wantEq(t, label, "segment_seconds", got.GetSegmentSeconds(), row.SegmentSeconds)
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
func (c *callLog) trace() string             { return strings.Join(c.ops, " → ") }

// --- 依赖调用的公共骨架：轨迹 + 错误注入 + 并发插队钩子 ---

type hook struct {
	at int // 1-based：在该依赖的第 at 次调用前触发
	fn func()
}

// stub 是所有替身共用的入口骨架。key 是 `<表>.<方法>`（错误注入与钩子粒度），
// opFormat 是写进 callLog 的 `<表>.<方法>:<键>`。
type stub struct {
	log    *callLog
	faults map[string]error
	hooks  map[string][]hook
	fired  map[string][]int // key → 已触发的 at 列表
	calls  map[string]int
}

func newStub(log *callLog) *stub {
	return &stub{
		log:    log,
		faults: map[string]error{},
		hooks:  map[string][]hook{},
		fired:  map[string][]int{},
		calls:  map[string]int{},
	}
}

// enter 在每个依赖调用入口做三件事：触发该次的并发钩子、记轨迹、返回注入的错误。
func (s *stub) enter(key, opFormat string, args ...any) error {
	s.calls[key]++
	at := s.calls[key]
	for _, h := range s.hooks[key] {
		if h.at == at {
			h.fn()
			s.fired[key] = append(s.fired[key], h.at)
		}
	}
	s.log.add(opFormat, args...)
	return s.faults[key]
}

// fail 让依赖 key 返回错误；包装口径由调用方按 model 的 fmt.Errorf("%w") 复刻（见 wrapf）。
func (s *stub) fail(key string, err error) { s.faults[key] = err }

// before 布一个一次性钩子：在依赖 key 的第 n 次调用**之前**执行 fn，模拟别的入口插队。
func (s *stub) before(key string, n int, fn func()) {
	s.hooks[key] = append(s.hooks[key], hook{at: n, fn: fn})
}

// checkHooks 在用例末尾确认布下的钩子都真的触发过（没触发＝断言在空转）。
func (s *stub) checkHooks(t *testing.T) {
	t.Helper()
	for key, hs := range s.hooks {
		for _, h := range hs {
			if !slices.Contains(s.fired[key], h.at) {
				t.Errorf("并发钩子 %s#第%d次 始终没触发，用例在空转（轨迹 [%s]）", key, h.at, s.log.trace())
			}
		}
	}
}

// wrapf 复刻 model 的错误包装口径（`fmt.Errorf("transcode_task Insert: %w", err)`）。
func wrapf(table, method string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", table, method, err)
}

// --- Redis key 复刻（与 repository 未导出常量逐字对齐；漂移即缓存命中类用例全红） ---

func keyTask(taskID int64) string         { return fmt.Sprintf("tc:task:%d", taskID) }
func keyTemplate(templateID int64) string { return fmt.Sprintf("tc:tpl:%d", templateID) }

// --- 缓存替身 ---

// fakeCache 实现 repository.Cacher。miss 返回 (\"\", false, nil)，与真实 Cache 处理 redis.Nil 一致。
// 载荷按真实链路存 JSON 字符串（不是结构体），所以「回填的到底是什么」可被断言。
type fakeCache struct {
	*stub
	kv map[string]string
}

func newFakeCache(s *stub) *fakeCache {
	return &fakeCache{stub: s, kv: map[string]string{}}
}

var _ repository.Cacher = (*fakeCache)(nil)
var _ model.TranscodeTaskModel = (*fakeTaskModel)(nil)
var _ model.TranscodeTemplateModel = (*fakeTemplateModel)(nil)

func (f *fakeCache) Ping(context.Context) error {
	return f.enter("cache.Ping", "cache.Ping")
}

// Cacher 的五个业务方法：键拼装走 keyTask/keyTemplate，与 repository 常量逐字对齐。
func (f *fakeCache) GetTask(_ context.Context, taskID int64) (string, bool, error) {
	return f.getWith("GetTask", keyTask(taskID))
}

func (f *fakeCache) SetTask(_ context.Context, taskID int64, payload string) error {
	return f.setWith("SetTask", keyTask(taskID), payload)
}

func (f *fakeCache) DelTask(_ context.Context, taskID int64) error {
	return f.delWith("DelTask", keyTask(taskID))
}

func (f *fakeCache) GetTemplate(_ context.Context, templateID int64) (string, bool, error) {
	return f.getWith("GetTemplate", keyTemplate(templateID))
}

func (f *fakeCache) SetTemplate(_ context.Context, templateID int64, payload string) error {
	return f.setWith("SetTemplate", keyTemplate(templateID), payload)
}

// getWith/setWith/delWith 把真实 Redis 的键名写进轨迹（`cache.GetTask:tc:task:101`），
// 这样键拼装本身也是被断言的对象。miss 返回 ("", false, nil)，与真实 Cache 处理 redis.Nil 一致。
func (f *fakeCache) getWith(method, key string) (string, bool, error) {
	if err := f.enter("cache."+method, "cache.%s:%s", method, key); err != nil {
		return "", false, err
	}
	v, ok := f.kv[key]
	return v, ok, nil
}

func (f *fakeCache) setWith(method, key, payload string) error {
	if err := f.enter("cache."+method, "cache.%s:%s", method, key); err != nil {
		return err
	}
	f.kv[key] = payload
	return nil
}

func (f *fakeCache) delWith(method, key string) error {
	if err := f.enter("cache."+method, "cache.%s:%s", method, key); err != nil {
		return err
	}
	delete(f.kv, key)
	return nil
}

// warmTask / warmTemplate 静默预热（布数据不写轨迹：读穿用例要区分「回填」和「本来就命中」）。
func (f *fakeCache) warmTask(task *model.TranscodeTask) {
	f.kv[keyTask(task.TaskId)] = string(mustMarshal(task))
}

func (f *fakeCache) warmTemplate(tpl *model.TranscodeTemplate) {
	f.kv[keyTemplate(tpl.TemplateId)] = string(mustMarshal(tpl))
}

// raw 读缓存里的原始载荷（用例据此断言「回填的 JSON 就是库存那一行」）。
func (f *fakeCache) raw(key string) (string, bool) { return f.kv[key], f.kv[key] != "" }

func mustMarshal(v any) []byte {
	bs, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("布景序列化失败：%v", err))
	}
	return bs
}

// --- 任务 model 替身 ---

type fakeTaskModel struct {
	*stub
	rows   map[int64]*model.TranscodeTask
	nextID int64
}

func newFakeTaskModel(s *stub) *fakeTaskModel {
	// nextID 从 900 起分配，布景一律用 1xx/2xx 段，轨迹里的 task_id 因此是确定的。
	return &fakeTaskModel{stub: s, rows: map[int64]*model.TranscodeTask{}, nextID: 900}
}

// Insert 复刻 INSERT ... （不写 task_id 列，靠 AUTO_INCREMENT）并返回 LastInsertId。
// transcode_task 除主键外无唯一约束（deploy/migrations/transcode/000001:22-25 只有三个普通 KEY），
// 所以这里不做冲突判定：同一请求重放必然再插一行。
// 自增只在成功时消耗（失败轨迹里记的是「本来会拿到的 id」），这样多个用例叠加错误注入时
// 后续 id 仍然确定，不会被前一次失败推动。
func (f *fakeTaskModel) Insert(_ context.Context, t *model.TranscodeTask) (int64, error) {
	newID := f.nextID + 1
	if err := f.enter("transcode_task.Insert", "transcode_task.Insert:%d", newID); err != nil {
		return 0, wrapf("transcode_task", "Insert", err)
	}
	f.nextID = newID
	cp := *t
	cp.TaskId = newID
	f.rows[newID] = &cp
	return newID, nil
}

func (f *fakeTaskModel) FindOne(_ context.Context, taskID int64) (*model.TranscodeTask, error) {
	if err := f.enter("transcode_task.FindOne", "transcode_task.FindOne:%d", taskID); err != nil {
		return nil, wrapf("transcode_task", "FindOne", err)
	}
	row, ok := f.rows[taskID]
	if !ok {
		return nil, nil // 与 model 一致：查无此行返回 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

// List 复刻 model/transcodemodel.go:78-122：先夹紧 pn/ps，再 COUNT，total==0 直接返回不再发 SELECT，
// 有行则按 task_id DESC + LIMIT/OFFSET 取页。返回 (行, total, err)。
// 轨迹里的 `<asset>/<state>` 记的是**实际进入 WHERE 的过滤值**（<=0 一律记 0，表示该条件不存在），
// 因为真实 SQL 绑的就是「加不加这一段 WHERE」，而不是调用方传的负数本身。
func (f *fakeTaskModel) List(_ context.Context, assetID int64, state int32, pn, ps int32) ([]*model.TranscodeTask, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps
	logAsset, logState := assetID, state // <=0 在这两张表里都表示「不加这个条件」，直接原样记
	if logAsset < 0 {
		logAsset = 0
	}
	if logState < 0 {
		logState = 0
	}

	matched := f.filter(assetID, state) // 与 `WHERE 1=1 [AND asset_id=?] [AND state=?]` 同语义
	total := int32(len(matched))
	if err := f.enter("transcode_task.Count", "transcode_task.Count:%d/%d", logAsset, logState); err != nil {
		return nil, 0, wrapf("transcode_task", "List count", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	if err := f.enter("transcode_task.Select", "transcode_task.Select:%d/%d/%d/%d/%d",
		logAsset, logState, pn, ps, offset); err != nil {
		return nil, 0, wrapf("transcode_task", "List", err)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].TaskId > matched[j].TaskId }) // ORDER BY task_id DESC
	page := slicePage(matched, ps, offset)
	out := make([]*model.TranscodeTask, 0, len(page))
	for _, row := range page {
		cp := *row
		out = append(out, &cp)
	}
	return out, total, nil
}

// UpdateProgress 复刻 `UPDATE transcode_task SET progress=?,state=?,errno=?,err_msg=?,mtime=?
// WHERE task_id=?`：五个列**无条件**覆盖，且 WHERE 只有主键、没有 `AND state = ?` 的 CAS 条件。
// 影响 0 行（行不存在）返回 ErrTaskNotFound。
func (f *fakeTaskModel) UpdateProgress(_ context.Context, taskID int64, progress, state, errno int32, errMsg string, mtime int64) error {
	if err := f.enter("transcode_task.UpdateProgress", "transcode_task.UpdateProgress:%d/%d/%d",
		taskID, state, progress); err != nil {
		return wrapf("transcode_task", "UpdateProgress", err)
	}
	row, ok := f.rows[taskID]
	if !ok {
		return model.ErrTaskNotFound // RowsAffected()==0
	}
	row.Progress, row.State, row.Errno, row.ErrMsg, row.Mtime = progress, state, errno, errMsg, mtime
	return nil
}

func (f *fakeTaskModel) filter(assetID int64, state int32) []*model.TranscodeTask {
	var out []*model.TranscodeTask
	for _, row := range f.rows {
		if assetID > 0 && row.AssetId != assetID {
			continue
		}
		if state > 0 && row.State != state {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// put 是静默布景路径（不记轨迹）。
func (f *fakeTaskModel) put(t *model.TranscodeTask) {
	cp := *t
	f.rows[t.TaskId] = &cp
}

// poke 给并发钩子用：模拟「别的 Worker 在这之前改了同一行」（不记轨迹）。
func (f *fakeTaskModel) poke(taskID int64, fn func(*model.TranscodeTask)) {
	if row, ok := f.rows[taskID]; ok {
		fn(row)
	}
}

// remove 给并发钩子用：模拟「这行在两个语句之间被删掉了」。
func (f *fakeTaskModel) remove(taskID int64) { delete(f.rows, taskID) }

func (f *fakeTaskModel) get(taskID int64) *model.TranscodeTask {
	row, ok := f.rows[taskID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// --- 模板 model 替身 ---

type fakeTemplateModel struct {
	*stub
	rows   map[int64]*model.TranscodeTemplate
	nextID int64
}

func newFakeTemplateModel(s *stub) *fakeTemplateModel {
	return &fakeTemplateModel{stub: s, rows: map[int64]*model.TranscodeTemplate{}, nextID: 900}
}

// Insert 复刻 model/transcodemodel.go:174-187：ctime/mtime 由 **model 自己**取 time.Now()
// （与任务表「由调用方传」的口径不同），并吃 `uniq_name` 唯一键（000001_create_transcode_tables.sql:43）。
// 自增消耗的口径与任务表**不同**，两条都要留意：
//   - 连接级失败（注入的 fault）发生在语句之前 ⇒ 不消耗号；
//   - `uniq_name` 冲突是 InnoDB 分配完自增值、写索引时才发现的 ⇒ 号已消耗，下一次成功插入会跳号
//     （MySQL 明确不回收已分配的 AUTO_INCREMENT）。因此重名用例的轨迹是 901 → 902(失败) → 903。
func (f *fakeTemplateModel) Insert(_ context.Context, t *model.TranscodeTemplate) (int64, error) {
	newID := f.nextID + 1
	if err := f.enter("transcode_template.Insert", "transcode_template.Insert:%d", newID); err != nil {
		return 0, wrapf("transcode_template", "Insert", err)
	}
	for _, row := range f.rows {
		if row.Name == t.Name {
			f.nextID = newID // 冲突前 InnoDB 已把号分配出去
			return 0, wrapf("transcode_template", "Insert",
				fmt.Errorf("Error 1062: Duplicate entry '%s' for key 'transcode_template.uniq_name'", t.Name))
		}
	}
	f.nextID = newID
	now := time.Now().Unix()
	cp := *t
	cp.TemplateId, cp.Ctime, cp.Mtime = newID, now, now
	f.rows[newID] = &cp
	return newID, nil
}

func (f *fakeTemplateModel) FindOne(_ context.Context, templateID int64) (*model.TranscodeTemplate, error) {
	if err := f.enter("transcode_template.FindOne", "transcode_template.FindOne:%d", templateID); err != nil {
		return nil, wrapf("transcode_template", "FindOne", err)
	}
	row, ok := f.rows[templateID]
	if !ok {
		return nil, nil // 与 model 一致
	}
	cp := *row
	return &cp, nil
}

// List 复刻 model/transcodemodel.go:201-232：无 WHERE、COUNT 先判 0、ORDER BY template_id **ASC**。
func (f *fakeTemplateModel) List(_ context.Context, pn, ps int32) ([]*model.TranscodeTemplate, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	all := make([]*model.TranscodeTemplate, 0, len(f.rows))
	for _, row := range f.rows {
		cp := *row
		all = append(all, &cp)
	}
	total := int32(len(all))
	if err := f.enter("transcode_template.Count", "transcode_template.Count"); err != nil {
		return nil, 0, wrapf("transcode_template", "List count", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	if err := f.enter("transcode_template.Select", "transcode_template.Select:%d/%d/%d", pn, ps, offset); err != nil {
		return nil, 0, wrapf("transcode_template", "List", err)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].TemplateId < all[j].TemplateId }) // ORDER BY template_id ASC
	page := slicePageTpl(all, ps, offset)
	out := make([]*model.TranscodeTemplate, 0, len(page))
	for _, row := range page {
		cp := *row
		out = append(out, &cp)
	}
	return out, total, nil
}

func (f *fakeTemplateModel) put(t *model.TranscodeTemplate) {
	cp := *t
	f.rows[t.TemplateId] = &cp
}

// poke 静默改库存一行，模拟「模板表被人工改库/重建」（本服务没有改模板的 RPC，
// 但 transcode_template 是运营表，人工 UPDATE/DELETE 是真实存在的脏数据来源）。
func (f *fakeTemplateModel) poke(templateID int64, fn func(*model.TranscodeTemplate)) {
	if row, ok := f.rows[templateID]; ok {
		fn(row)
	}
}

func (f *fakeTemplateModel) get(templateID int64) *model.TranscodeTemplate {
	row, ok := f.rows[templateID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// --- 分页切片（与 model 同一套算术，见文件头「覆盖边界」） ---

func slicePage(rows []*model.TranscodeTask, ps, offset int32) []*model.TranscodeTask {
	if int(offset) >= len(rows) {
		return nil
	}
	end := int(offset) + int(ps)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

func slicePageTpl(rows []*model.TranscodeTemplate, ps, offset int32) []*model.TranscodeTemplate {
	if int(offset) >= len(rows) {
		return nil
	}
	end := int(offset) + int(ps)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

// --- 事务连接替身 ---

// fakeConn 不实现任何方法：transcode 的 Repository 不走事务，也不该走。
// 任何直连 SQL 落在嵌入的 nil 接口上会直接 panic，等价于「logic 单测路径上不允许出现裸 SQL」。
type fakeConn struct{ sqlx.SqlConn }

var _ sqlx.SqlConn = (*fakeConn)(nil)

// --- 装配 ---

type store struct {
	log   *callLog
	s     *stub // 三个替身共用的注入/钩子骨架（本包用例用 st.s.before / st.s.fail）
	cache *fakeCache
	tasks *fakeTaskModel
	tpls  *fakeTemplateModel
	conn  *fakeConn
	repo  *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	s := newStub(log)
	st := &store{
		log:   log,
		s:     s,
		cache: newFakeCache(s),
		tasks: newFakeTaskModel(s),
		tpls:  newFakeTemplateModel(s),
		conn:  &fakeConn{},
	}
	st.repo = repository.NewWithDeps(st.cache, st.conn, st.tasks, st.tpls)
	return st
}

// task / tpl 读库存当前值（副本，用例改它不会影响库存）；missing 返回 nil。
func (st *store) task(taskID int64) *model.TranscodeTask { return st.tasks.get(taskID) }
func (st *store) tpl(templateID int64) *model.TranscodeTemplate {
	return st.tpls.get(templateID)
}

// taskRows / tplRows 是「库里实际还剩什么」的断言口径（拒绝类用例必须数到 0）。
func (st *store) taskRows() int { return len(st.tasks.rows) }
func (st *store) tplRows() int  { return len(st.tpls.rows) }

// env 是一次用例的完整运行时：真实 Repository（依赖为替身）+ ServiceContext。
// Config 留零值即可：7 个 logic 只读 svcCtx.Repository。
type env struct {
	t      *testing.T
	st     *store
	svcCtx *svc.ServiceContext
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := newStore()
	return &env{t: t, st: st, svcCtx: &svc.ServiceContext{Repository: st.repo}}
}

// --- 布景（全部走静默路径，不写轨迹） ---

// seedTask 布一行任务；返回库存副本。
func seedTask(t *testing.T, st *store, task *model.TranscodeTask) *model.TranscodeTask {
	t.Helper()
	if task.TaskId <= 0 {
		t.Fatal("布景必须显式给 TaskId（避免与替身自增的 901+ 段撞号）")
	}
	st.tasks.put(task)
	return st.tasks.get(task.TaskId)
}

// seedTemplate 布一行模板。
func seedTemplate(t *testing.T, st *store, tpl *model.TranscodeTemplate) *model.TranscodeTemplate {
	t.Helper()
	if tpl.TemplateId <= 0 {
		t.Fatal("布景必须显式给 TemplateId")
	}
	st.tpls.put(tpl)
	return st.tpls.get(tpl.TemplateId)
}

// warmTaskCache 预热任务缓存（读穿用例用）；payload 口径与 repository 一致：整行 JSON。
func warmTaskCache(t *testing.T, st *store, task *model.TranscodeTask) {
	t.Helper()
	st.cache.warmTask(task)
}

// warmTemplateCache 预热模板缓存。
func warmTemplateCache(t *testing.T, st *store, tpl *model.TranscodeTemplate) {
	t.Helper()
	st.cache.warmTemplate(tpl)
}

// --- 常用样例数据 ---

// pendingTask 是一条典型的 PENDING 任务（SubmitTask 落库后的样子）。
func pendingTask(taskID, assetID, templateID int64, now int64) *model.TranscodeTask {
	return &model.TranscodeTask{
		TaskId:       taskID,
		AssetId:      assetID,
		TemplateId:   templateID,
		InputBucket:  "in-bucket",
		InputKey:     "raw/2026/09/23/mid-100.mp4",
		OutputBucket: "out-bucket",
		OutputKey:    fmt.Sprintf("hls/%d/1080p.m3u8", taskID),
		State:        model.TaskStatePending,
		Progress:     0,
		Errno:        0,
		ErrMsg:       "",
		Ctime:        now,
		Mtime:        now,
	}
}

// h264_1080p 是一条典型的转码模板。
func h264_1080p(id int64, name string) *model.TranscodeTemplate {
	return &model.TranscodeTemplate{
		TemplateId:     id,
		Name:           name,
		Codec:          "h264",
		Width:          1920,
		Height:         1080,
		Bitrate:        6000,
		Fps:            30,
		SegmentSeconds: 6,
		Ctime:          1700000000,
		Mtime:          1700000000,
	}
}

// nowUnix 是布景与断言共用的墙钟基准。
func nowUnix() int64 { return time.Now().Unix() }
