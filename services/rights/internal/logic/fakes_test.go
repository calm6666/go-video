package logic

// fakes_test.go 是 rights logic 测试的内存替身集合。
//
// 为什么需要注入缝：rights 的 ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包的用例统一用
// repository.NewWithDeps(内存缓存, 内存 contractMd, 内存 windowMd) 组装**真实的 Repository**，
// 只把它的 3 个依赖换成替身——这样判定链（缓存/DB/状态推进）整条都在被测路径上，
// 而不是把 Repository 也 mock 掉。见 internal/repository/repository.go 的 Cacher 注释。
//
// 四条替身纪律（catalog 同包已踩过坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic 里 `w.State = ...` 之类的写回不得污染库存行，
//     否则「有没有真的调用 UpdateState」这类断言会被共享指针掩盖。
//  2. 写入按真实 SQL 的口径处理主键：Insert 忽略入参 ID、自增分配并返回，
//     与 model 的 INSERT（列表不含主键）+ LastInsertId 一致。
//  3. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：
//     rights 最要紧的结论是「先查谁、回填了什么、有没有推进状态」，只数次数会漏掉次序错误。
//  4. 错误注入按方法粒度（faultInjector.failWith("FindActive...", err)），
//     不用一个全局 err：读侧用例会因此互相污染，也定位不了是哪条分支。
//
// 覆盖边界（如实声明）：替身只复刻 model 层 SQL 的**语义**（过滤条件、排序、状态门槛），
// 不证明 SQL 本身。`services/rights/model/rightsmodel.go` 的列名/占位符/分页仍无单测，
// 仓库里也没有 rights 的迁移↔model 列级对账门禁，见 README 已知缺口。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go-video/services/rights/internal/config"
	"go-video/services/rights/internal/repository"
	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
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

// wantNoErr 断言没有错误（CheckPlayable 的「判定为不可播」必须走这条，不能报错）。
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

func (c *callLog) snapshot() int { return len(c.ops) }
func (c *callLog) opsFrom(from int) []string {
	return c.ops[from:]
}

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

// --- 缓存替身 ---

// chkEntry 是一条 CheckPlayable 缓存值（真实实现把它编码成 "playable:window_id:end_time"）。
type chkEntry struct {
	playable bool
	windowID int64
	endTime  int64
}

// keyCheck 与 repository 未导出的 keyCheckPlayable 逐字对齐；不一致时读穿/回填用例即红。
func keyCheck(contentID int64, contentType int32, region string) string {
	return fmt.Sprintf("rights:chk:%d:%d:%s", contentID, contentType, region)
}

// fakeCache 实现 repository.Cacher。miss 返回 hit=false 且 err=nil，与真实 Cache
// 处理 redis.Nil 的口径一致。
type fakeCache struct {
	log     *callLog
	data    map[string]chkEntry
	gets    map[string]int
	failGet error
	failSet error
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{log: log, data: map[string]chkEntry{}, gets: map[string]int{}}
}

func (f *fakeCache) Ping(context.Context) error { f.log.add("cache.Ping"); return nil }

func (f *fakeCache) GetCheckPlayable(_ context.Context, contentID int64, contentType int32, region string) (bool, int64, int64, bool, error) {
	key := keyCheck(contentID, contentType, region)
	f.log.add("cache.Get:%s", key)
	f.gets[key]++
	if f.failGet != nil {
		return false, 0, 0, false, f.failGet
	}
	e, ok := f.data[key]
	if !ok {
		return false, 0, 0, false, nil
	}
	return e.playable, e.windowID, e.endTime, true, nil
}

func (f *fakeCache) SetCheckPlayable(_ context.Context, contentID int64, contentType int32, region string, playable bool, windowID, endTime int64) error {
	key := keyCheck(contentID, contentType, region)
	f.log.add("cache.Set:%s", key)
	if f.failSet != nil {
		return f.failSet
	}
	f.data[key] = chkEntry{playable: playable, windowID: windowID, endTime: endTime}
	return nil
}

func (f *fakeCache) DelCheckPlayable(_ context.Context, contentID int64, contentType int32, region string) error {
	key := keyCheck(contentID, contentType, region)
	f.log.add("cache.Del:%s", key)
	delete(f.data, key)
	return nil
}

// entry 读缓存里实际存了什么（断言「回填的是正缓存还是负缓存」）。
func (f *fakeCache) entry(contentID int64, contentType int32, region string) (chkEntry, bool) {
	e, ok := f.data[keyCheck(contentID, contentType, region)]
	return e, ok
}

func (f *fakeCache) hits(contentID int64, contentType int32, region string) int {
	return f.gets[keyCheck(contentID, contentType, region)]
}

// --- 合同 model 替身 ---

type fakeContractModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.RightsContract
	next  int64
	calls []string // List 的归一化入参，便于断言过滤/分页透传
}

func (f *fakeContractModel) Insert(_ context.Context, c *model.RightsContract) (int64, error) {
	f.log.add("contract.Insert")
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	cp := *c
	f.next++
	cp.ContractID = f.next
	stored := cp
	f.rows[cp.ContractID] = &stored
	return cp.ContractID, nil
}

func (f *fakeContractModel) FindOne(_ context.Context, contractID int64) (*model.RightsContract, error) {
	f.log.add("contract.FindOne:%d", contractID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[contractID]
	if !ok {
		return nil, nil // 与 model 一致：查无此行返回 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (f *fakeContractModel) List(_ context.Context, ownerID int64, state, pn, ps int32) ([]*model.RightsContract, int32, error) {
	f.log.add("contract.List:%d/%d/%d/%d", ownerID, state, pn, ps)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%d/%d", ownerID, state, pn, ps))
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] }) // 与 model 的 ORDER BY contract_id DESC 同向
	var out []*model.RightsContract
	var total int32
	for _, id := range ids {
		row := f.rows[id]
		if ownerID > 0 && row.OwnerID != ownerID {
			continue
		}
		if state > 0 && row.State != state {
			continue
		}
		total++
		cp := *row
		out = append(out, &cp)
	}
	return out, total, nil
}

// --- 窗口 model 替身 ---

type fakeWindowModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.RightsWindow
	next  int64
	calls []string
}

func (f *fakeWindowModel) Insert(_ context.Context, w *model.RightsWindow) (int64, error) {
	f.log.add("win.Insert")
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	cp := *w
	f.next++
	cp.WindowID = f.next
	stored := cp
	f.rows[cp.WindowID] = &stored
	return cp.WindowID, nil
}

func (f *fakeWindowModel) FindOne(_ context.Context, windowID int64) (*model.RightsWindow, error) {
	f.log.add("win.FindOne:%d", windowID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[windowID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeWindowModel) List(_ context.Context, contentID, contractID int64, contentType, state, pn, ps int32) ([]*model.RightsWindow, int32, error) {
	f.log.add("win.List:%d/%d/%d/%d/%d/%d", contentID, contractID, contentType, state, pn, ps)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%d/%d/%d/%d", contentID, contractID, contentType, state, pn, ps))
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	var out []*model.RightsWindow
	var total int32
	for _, id := range ids {
		row := f.rows[id]
		if contentID > 0 && row.ContentID != contentID {
			continue
		}
		if contractID > 0 && row.ContractID != contractID {
			continue
		}
		if contentType > 0 && row.ContentType != contentType {
			continue
		}
		if state > 0 && row.State != state {
			continue
		}
		total++
		cp := *row
		out = append(out, &cp)
	}
	return out, total, nil
}

// FindActiveByContentRegion 复刻 model 的 SQL 语义：只要 state=active，
// 多条命中取 end_time 最大的一条，无命中返回 (nil, nil)。
func (f *fakeWindowModel) FindActiveByContentRegion(_ context.Context, contentID int64, contentType int32, region string) (*model.RightsWindow, error) {
	f.log.add("win.FindActive:%d/%d/%s", contentID, contentType, region)
	f.calls = append(f.calls, fmt.Sprintf("active:%d/%d/%s", contentID, contentType, region))
	if err := f.fail("FindActiveByContentRegion"); err != nil {
		return nil, err
	}
	var best *model.RightsWindow
	for _, row := range f.rows {
		if row.ContentID != contentID || row.ContentType != contentType ||
			row.Region != region || row.State != model.WindowStateActive {
			continue
		}
		if best == nil || row.EndTime > best.EndTime {
			cp := *row
			best = &cp
		}
	}
	return best, nil
}

func (f *fakeWindowModel) UpdateState(_ context.Context, windowID int64, state int32) (int64, error) {
	f.log.add("win.UpdateState:%d->%d", windowID, state)
	if err := f.fail("UpdateState"); err != nil {
		return 0, err
	}
	row, ok := f.rows[windowID]
	if !ok {
		return 0, nil // UPDATE 未命中：受影响 0 行
	}
	row.State = state
	return 1, nil
}

// ListExpiring 复刻 model 的 SQL 语义：state=active 且 end_time <= now+within，按 end_time 升序。
func (f *fakeWindowModel) ListExpiring(_ context.Context, withinSeconds, pn, ps int32) ([]*model.RightsWindow, int32, error) {
	f.log.add("win.ListExpiring:%d/%d/%d", withinSeconds, pn, ps)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%d", withinSeconds, pn, ps))
	if err := f.fail("ListExpiring"); err != nil {
		return nil, 0, err
	}
	threshold := model.NowUnix() + int64(withinSeconds)
	var ids []int64
	for id, row := range f.rows {
		if row.State == model.WindowStateActive && row.EndTime <= threshold {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return f.rows[ids[i]].EndTime < f.rows[ids[j]].EndTime })
	out := make([]*model.RightsWindow, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, int32(len(out)), nil
}

// state 读库内当前状态（值读，避免用例误改库存行）。
func (f *fakeWindowModel) state(windowID int64) int32 {
	if row, ok := f.rows[windowID]; ok {
		return row.State
	}
	return -1
}

// --- 装配 ---

type store struct {
	log      *callLog
	cache    *fakeCache
	contract *fakeContractModel
	window   *fakeWindowModel
	repo     *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	st := &store{
		log:      log,
		cache:    newFakeCache(log),
		contract: &fakeContractModel{log: log, rows: map[int64]*model.RightsContract{}},
		window:   &fakeWindowModel{log: log, rows: map[int64]*model.RightsWindow{}},
	}
	st.repo = repository.NewWithDeps(st.cache, st.contract, st.window)
	return st
}

// newTestSvc 装配只带内存依赖的 ServiceContext。与生产 ServiceContext 的差别仅在
// Repository 的 3 个依赖是替身；Config 由用例显式给出（etc/*.yaml 的加载由 config 包用例负责）。
func newTestSvc(st *store) *svc.ServiceContext {
	return &svc.ServiceContext{Config: config.Config{}, Repository: st.repo}
}

// seed* 写入测试数据并返回**指向库内行**的指针：用例可用它拿主键，
// 改状态必须显式走 rows map（见本文件头纪律 1）。
func seedContract(st *store, state int32, regions ...string) *model.RightsContract {
	id, _ := st.contract.Insert(context.Background(), &model.RightsContract{
		OwnerID:    7001,
		Title:      "某番剧授权合同",
		StartDate:  1_700_000_000,
		EndDate:    1_900_000_000,
		RegionsCSV: model.RegionsToCSV(regions),
		State:      state,
	})
	return st.contract.rows[id]
}

// seedWindow 直接落一行窗口（绕开 CreateWindow 的守卫），用于读侧与判定侧布景。
func seedWindow(st *store, contentID int64, contentType int32, region string, start, end int64, state int32) *model.RightsWindow {
	id, _ := st.window.Insert(context.Background(), &model.RightsWindow{
		ContractID: 1, ContentID: contentID, ContentType: contentType,
		Region: region, StartTime: start, EndTime: end, State: state,
	})
	return st.window.rows[id]
}

// nowPlus 相对真实 now 偏移若干秒：rights 的时间判定用的是 model.NowUnix()（不可注入），
// 因此布景只留安全余量，不依赖秒级边界。
func nowPlus(delta int64) int64 { return model.NowUnix() + delta }
