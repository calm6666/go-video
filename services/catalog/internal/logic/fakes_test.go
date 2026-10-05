package logic

// fakes_test.go 是 catalog logic 层按方法单测的依赖替身集合。
//
// 为什么需要这一层：catalog 的 12 个 RPC 方法全部经 svcCtx.Repository 读写
// catalog_work / catalog_season / catalog_episode / catalog_zone / catalog_tag，
// 并按配置调用 rights / asset 两个只读下游。此前本包只有 guard_test.go（纯函数）
// 与 statemachine_test.go（转换表），没有任何一条用例构造过 <X>Logic，
// 于是「守卫有没有在触库前拦住」「校验不通过时有没有落库」「状态机是否真的生效」
// 全都没有证据。本文件把 repository.Repository 的 6 个依赖（5 个 model + 缓存）
// 换成内存实现，让 logic 的判定链可断言。
//
// 三条替身纪律（改本文件时保持）：
//  1. 读接口返回行的**值拷贝**，写接口入库前也存拷贝。真实 DB 每次查询返回新对象；
//     若替身共享指针，logic 里 `e.State = EpStateOnline` 这类收尾赋值会反向污染库存行，
//     「没调 UpdateState 却变成已上架」这种缺陷会被自己掩盖掉（seed 辅助函数返回的
//     是指向库内行的指针，用例要改库内状态必须显式走 rows  map，别改返回值）。
//  2. 所有方法把调用记进同一条 callLog（有序），既断言次数，也断言先后次序。
//  3. 错误按方法名注入（faultInjector），不整体替换 fake。
//     一条用例里「读成功、写失败」是常见判定链，全局 err 字段会把读路径一起打挂，
//     测试就会因为错误的原因通过。
//
// 覆盖边界（如实声明）：替身只证明 logic 的判定与投影，**不证明 SQL**。
// catalog 的 model 层仍无单测，也没有迁移↔model 的自动对账门禁，见 README 已知缺口。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"go-video/services/catalog/internal/config"
	"go-video/services/catalog/internal/repository"
	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
)

// --- 断言小工具（本包共享，write/read 两个测试文件都用） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵（logic 用 %w 包装下游错误是允许的）。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %v", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

// wantNoCall 断言 from 之后没有发生任何依赖调用（守卫必须在触库之前）。
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

// opsEpisodeRead 是「读一集」在替身轨迹上的完整形状：Repository.GetEpisode 先查缓存，
// miss 才查库，拿到行才回填。写侧用例的期望序列都以它开头（校验只在读到行之后发生），
// 8 个用例各抄三行容易改一处漏七处，故收敛到这里。
func opsEpisodeRead(epid int64) []string {
	return []string{
		fmt.Sprintf("cache.Get:"+cacheKeyEpisode, epid),
		fmt.Sprintf("ep.FindOne:%d", epid),
		fmt.Sprintf("cache.Set:"+cacheKeyEpisode, epid),
	}
}

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

// count 统计完全相等的调用条数。
func (c *callLog) count(op string) int {
	n := 0
	for _, o := range c.ops {
		if o == op {
			n++
		}
	}
	return n
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

// --- 错误注入 ---

// faultInjector 按 model 方法名注入错误；未注入时返回 nil。
type faultInjector struct{ by map[string]error }

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

func (f *faultInjector) fail(method string) error { return f.by[method] }

// --- 缓存替身 ---

// fakeCache 实现 repository.Cacher：按 key 存 payload 并记录读/写/删。
// miss 返回 ("", nil)，与真实 Cache 处理 redis.Nil 的口径一致；
// fail 非 nil 时所有读操作返回该错误（模拟 Redis 故障，Repository 会把它上抛）。
type fakeCache struct {
	log  *callLog
	data map[string]string
	gets map[string]int
	fail error
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{log: log, data: map[string]string{}, gets: map[string]int{}}
}

// 缓存 key 与 repository 包内未导出的常量逐一对齐；不一致时下面的读穿/回填用例会红。
const (
	zonesKey        = "cat:zones"
	cacheKeyWork    = "cat:w:%d"
	cacheKeySeason  = "cat:s:%d"
	cacheKeyEpisode = "cat:e:%d"
)

func (f *fakeCache) Ping(context.Context) error { f.log.add("cache.Ping"); return nil }

func (f *fakeCache) get(key string) (string, error) {
	f.log.add("cache.Get:%s", key)
	f.gets[key]++
	if f.fail != nil {
		return "", f.fail
	}
	return f.data[key], nil
}

func (f *fakeCache) set(key, payload string) error {
	f.log.add("cache.Set:%s", key)
	f.data[key] = payload
	return nil
}

func (f *fakeCache) del(key string) error {
	f.log.add("cache.Del:%s", key)
	delete(f.data, key)
	return nil
}

func (f *fakeCache) GetWork(_ context.Context, id int64) (string, error) {
	return f.get(fmt.Sprintf(cacheKeyWork, id))
}
func (f *fakeCache) SetWork(_ context.Context, id int64, payload string) error {
	return f.set(fmt.Sprintf(cacheKeyWork, id), payload)
}
func (f *fakeCache) DelWork(_ context.Context, id int64) error {
	return f.del(fmt.Sprintf(cacheKeyWork, id))
}

func (f *fakeCache) GetSeason(_ context.Context, id int64) (string, error) {
	return f.get(fmt.Sprintf(cacheKeySeason, id))
}
func (f *fakeCache) SetSeason(_ context.Context, id int64, payload string) error {
	return f.set(fmt.Sprintf(cacheKeySeason, id), payload)
}
func (f *fakeCache) DelSeason(_ context.Context, id int64) error {
	return f.del(fmt.Sprintf(cacheKeySeason, id))
}

func (f *fakeCache) GetEpisode(_ context.Context, id int64) (string, error) {
	return f.get(fmt.Sprintf(cacheKeyEpisode, id))
}
func (f *fakeCache) SetEpisode(_ context.Context, id int64, payload string) error {
	return f.set(fmt.Sprintf(cacheKeyEpisode, id), payload)
}
func (f *fakeCache) DelEpisode(_ context.Context, id int64) error {
	return f.del(fmt.Sprintf(cacheKeyEpisode, id))
}

func (f *fakeCache) GetZones(context.Context) (string, error) { return f.get(zonesKey) }
func (f *fakeCache) SetZones(_ context.Context, payload string) error {
	return f.set(zonesKey, payload)
}

// hits 返回某 key 的读取次数（断言「第二次读走缓存，没再查库」）。
func (f *fakeCache) hits(key string) int { return f.gets[key] }

// --- Work ---

type fakeWorkModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.Work
	next  int64
	calls []string // 记录 List 的归一化入参，便于断言分页/过滤透传
}

func (f *fakeWorkModel) Insert(_ context.Context, w *model.Work) (int64, error) {
	f.log.add("work.Insert")
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	cp := *w
	f.next++
	cp.SeasonID = f.next
	stored := cp
	f.rows[cp.SeasonID] = &stored
	return cp.SeasonID, nil
}

func (f *fakeWorkModel) FindOne(_ context.Context, seasonID int64) (*model.Work, error) {
	f.log.add("work.FindOne:%d", seasonID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[seasonID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeWorkModel) List(_ context.Context, typeid, state, pn, ps int32) ([]*model.Work, int32, error) {
	f.log.add("work.List:%d/%d/%d/%d", typeid, state, pn, ps)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%d/%d", typeid, state, pn, ps))
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] }) // 与 model 的 ORDER BY season_id DESC 同向
	var out []*model.Work
	var total int32
	for _, id := range ids {
		row := f.rows[id]
		if typeid > 0 && row.TypeID != typeid {
			continue
		}
		if state >= 0 && row.State != state {
			continue
		}
		total++
		cp := *row
		out = append(out, &cp)
	}
	return out, total, nil
}

func (f *fakeWorkModel) UpdateState(_ context.Context, seasonID int64, state int32) error {
	f.log.add("work.UpdateState:%d->%d", seasonID, state)
	if err := f.fail("UpdateState"); err != nil {
		return err
	}
	row, ok := f.rows[seasonID]
	if !ok {
		return model.ErrWorkNotFound
	}
	row.State = state
	return nil
}

// --- Season ---

type fakeSeasonModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.Season
	next int64
}

func (f *fakeSeasonModel) Insert(_ context.Context, s *model.Season) (int64, error) {
	f.log.add("season.Insert")
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	cp := *s
	f.next++
	cp.SeasonID = f.next
	stored := cp
	f.rows[cp.SeasonID] = &stored
	return cp.SeasonID, nil
}

func (f *fakeSeasonModel) FindOne(_ context.Context, seasonID int64) (*model.Season, error) {
	f.log.add("season.FindOne:%d", seasonID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[seasonID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeSeasonModel) ListByWork(_ context.Context, workID int64) ([]*model.Season, error) {
	f.log.add("season.ListByWork:%d", workID)
	if err := f.fail("ListByWork"); err != nil {
		return nil, err
	}
	var ids []int64
	for id, row := range f.rows {
		if row.WorkID == workID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return f.rows[ids[i]].SeasonNo < f.rows[ids[j]].SeasonNo })
	out := make([]*model.Season, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, nil
}

// --- Episode ---

type fakeEpisodeModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.Episode
	next int64
}

func (f *fakeEpisodeModel) Insert(_ context.Context, e *model.Episode) (int64, error) {
	f.log.add("ep.Insert")
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	cp := *e
	f.next++
	cp.Epid = f.next
	stored := cp
	f.rows[cp.Epid] = &stored
	return cp.Epid, nil
}

func (f *fakeEpisodeModel) FindOne(_ context.Context, epid int64) (*model.Episode, error) {
	f.log.add("ep.FindOne:%d", epid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[epid]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeEpisodeModel) ListBySeason(_ context.Context, seasonID int64) ([]*model.Episode, error) {
	f.log.add("ep.ListBySeason:%d", seasonID)
	if err := f.fail("ListBySeason"); err != nil {
		return nil, err
	}
	var ids []int64
	for id, row := range f.rows {
		if row.SeasonID == seasonID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return f.rows[ids[i]].EpNo < f.rows[ids[j]].EpNo })
	out := make([]*model.Episode, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeEpisodeModel) UpdateState(_ context.Context, epid int64, state int32) error {
	f.log.add("ep.UpdateState:%d->%d", epid, state)
	if err := f.fail("UpdateState"); err != nil {
		return err
	}
	row, ok := f.rows[epid]
	if !ok {
		return model.ErrEpisodeNotFound
	}
	row.State = state
	return nil
}

// state 读库内当前状态（值拷贝，避免用例误改库存行）。
func (f *fakeEpisodeModel) state(epid int64) int32 {
	if row, ok := f.rows[epid]; ok {
		return row.State
	}
	return -1
}

// --- Zone / Tag ---

type fakeZoneModel struct {
	faultInjector
	log  *callLog
	rows []*model.Zone
}

func (f *fakeZoneModel) ListAll(context.Context) ([]*model.Zone, error) {
	f.log.add("zone.ListAll")
	if err := f.fail("ListAll"); err != nil {
		return nil, err
	}
	out := make([]*model.Zone, 0, len(f.rows))
	for _, z := range f.rows {
		cp := *z
		out = append(out, &cp)
	}
	return out, nil
}

type fakeTagModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.Tag
	calls []string
}

func (f *fakeTagModel) FindByIDs(_ context.Context, ids []int64) ([]*model.Tag, error) {
	f.log.add("tag.FindByIDs:%d", len(ids))
	f.calls = append(f.calls, fmt.Sprintf("ids:%d", len(ids)))
	if err := f.fail("FindByIDs"); err != nil {
		return nil, err
	}
	out := make([]*model.Tag, 0, len(ids))
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeTagModel) SearchByName(_ context.Context, name string, pn, ps int32) ([]*model.Tag, error) {
	f.log.add("tag.SearchByName:%s/%d/%d", name, pn, ps)
	f.calls = append(f.calls, fmt.Sprintf("name:%s/%d/%d", name, pn, ps))
	if err := f.fail("SearchByName"); err != nil {
		return nil, err
	}
	var ids []int64
	for id, row := range f.rows {
		if strings.Contains(row.Name, name) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*model.Tag, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, nil
}

// --- 装配 ---

// store 汇总一条用例的全部内存依赖，便于断言「一次都没调用」。
type store struct {
	log    *callLog
	cache  *fakeCache
	work   *fakeWorkModel
	season *fakeSeasonModel
	ep     *fakeEpisodeModel
	zone   *fakeZoneModel
	tag    *fakeTagModel
	repo   *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	st := &store{
		log:    log,
		cache:  newFakeCache(log),
		work:   &fakeWorkModel{log: log, rows: map[int64]*model.Work{}},
		season: &fakeSeasonModel{log: log, rows: map[int64]*model.Season{}},
		ep:     &fakeEpisodeModel{log: log, rows: map[int64]*model.Episode{}},
		zone:   &fakeZoneModel{log: log},
		tag:    &fakeTagModel{log: log, rows: map[int64]*model.Tag{}},
	}
	st.repo = repository.NewWithDeps(st.cache, st.work, st.season, st.ep, st.zone, st.tag)
	return st
}

// newTestSvc 装配只带内存依赖与 fake 下游的 ServiceContext。
// 与生产 ServiceContext 的差别仅在 Repository 的 6 个依赖是替身、Rights/Asset 是
// 调用方可控的 fake；Config 由用例显式给出（etc/*.yaml 的加载由 config 包的用例负责）。
func newTestSvc(cfg config.Config, st *store, rights repository.RightsClient,
	asset repository.AssetClient) *svc.ServiceContext {
	return &svc.ServiceContext{Config: cfg, Repository: st.repo, Rights: rights, Asset: asset}
}

// 三个默认配置：带缺省地区 / 不带地区（地区必须显式传） / 关闭版权校验。
var (
	cfgCN         = config.Config{DefaultRegion: "CN"}
	cfgNoRegion   = config.Config{}
	cfgNoRights   = config.Config{DisableRightsCheck: true}
	cfgNoAssetChk = config.Config{DefaultRegion: "CN", DisableAssetCheck: true}
)

// seed* 写入测试数据并返回**指向库内行**的指针：用例可用它拿主键，
// 但改状态必须显式走 rows map（见本文件头纪律 1）。
func seedWork(st *store, state, typeid int32, title string) *model.Work {
	id, _ := st.work.Insert(context.Background(), &model.Work{
		Title: title, TypeID: typeid, State: state, Cover: "cover", Intro: "intro",
	})
	return st.work.rows[id]
}

func seedSeason(st *store, workID int64, seasonNo, state int32) *model.Season {
	id, _ := st.season.Insert(context.Background(), &model.Season{
		WorkID: workID, SeasonNo: seasonNo, State: state, Title: "season", Cover: "season-cover",
	})
	return st.season.rows[id]
}

func seedEpisode(st *store, seasonID int64, epNo, state int32, assetID int64) *model.Episode {
	id, _ := st.ep.Insert(context.Background(), &model.Episode{
		SeasonID: seasonID, EpNo: epNo, State: state, AssetID: assetID,
		Title: "ep", Duration: 1200,
	})
	return st.ep.rows[id]
}

func seedTag(st *store, tagID int64, name string) {
	st.tag.rows[tagID] = &model.Tag{TagID: tagID, Name: name}
}
