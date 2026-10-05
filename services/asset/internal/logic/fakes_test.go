package logic

// fakes_test.go 是 asset logic 测试的内存替身集合与装配缝。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造 repository.New 会 redis.MustNewRedis + sqlx.NewMysql（连不上的 MySQL/Redis
// 直接 panic），logic 测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, 4 个内存 model) 组装**真实的 Repository**，
// 只把它的 5 个依赖换成替身——于是「先读缓存还是先查库、命中要不要回填、
// 空结果要不要占位、列表走不走缓存、下游错误原样上抛还是换哨兵」整条判定链
// 都留在被测路径上，而不是把 Repository 一起 mock 掉。
// 缝的定义见 internal/repository/repository.go 的 Cacher / NewWithDeps 注释。
//
// 五条替身纪律（catalog / rights / content-fingerprint 几轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 里对返回值的改写不得污染库存行，
//     否则「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 副作用按真实 SQL 的口径处理主键与唯一键：Insert 忽略入参 asset_id、自增分配并返回，
//     asset_subtitle 复刻 uniq_asset_lang（同媒资同语言重复插入报 1062），
//     UpdateMeta/UpdateState 按 RowsAffected==0 返回**未包装**的 model.ErrAssetNotFound
//     ——logic 里用的是 `err == 哨兵`，包一层就会静默走错分支。
//  3. 排序/分页期望**不手抄字面量**：model 的 ORDER BY 文本被 modelsql_test.go 抽出来
//     与 clauseLess 登记表对账，用例的期望序列由 orderIDs(clause) 现算。SQL 一改，
//     对账先红，期望值随后自动跟着新口径，不会出现「测试替 model 说谎」。
//  4. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：本域要紧的结论是
//     「守卫拒绝后一次依赖都不许碰」「命中缓存时不许查库」「回填发生在读库之后」。
//  5. 布数据走替身的**静默写入路径**（put/warm，不记轨迹），所以轨迹断言一律 from=0
//     直接数完整序列。哪天改成用公开方法布景，就必须先取 before := st.log.snapshot()。
//
// 错误注入按方法粒度（failWith("FindOne", err)），不用一个全局 err。
//
// 覆盖边界（如实声明，详见 README「测试口径」）：
//   - 替身只复刻 model 层 SQL 的**语义**（WHERE 过滤、total==0 时省掉第二条 SELECT、
//     ORDER BY 方向、LIMIT/OFFSET 裁剪、1062 冲突），不证明 SQL 文本与列名本身。
//   - 生产的 repository.Cache（真 Redis 客户端）被替身整体换掉，所以「key 拼成
//     asset:a:<id>、TTL=60s、空串按 miss 处理」这三条只能靠 modelsql_test.go 的
//     **文本对账**钉住，不是行为验证。
//   - UpdateMeta/UpdateState 的「未找到」按**匹配到的行数**建模（缺行才返回
//     model.ErrAssetNotFound）。真实驱动默认上报**改变**的行数，同值重放会被判成未找到，
//     这条差异在内存用例里不可见，只能靠写侧用例里的源码对账 + README 缺口留痕。
//   - 写侧 6 个方法（RegisterAsset/UpdateAssetMeta/TransitionState/AddCover/
//     AddSubtitle/AddScreenshot）的用例落在同目录 6 个 *_test.go 里，用的就是这里
//     已实现的 Insert/Update 语义，没有再改缝。
//   - 本服务不碰对象存储与 CDN：没有预签名 URL 生成点，也就没有过期口径可测
//     （应答面由 TestAssetRepliesExposeOSSReferenceButNoSignedURL 兜底）。

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

	"go-video/services/asset/internal/config"
	"go-video/services/asset/internal/repository"
	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

// --- 断言小工具（本包共享） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
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

// wantErrIdentity 断言错误**就是那个哨兵实例本身**，不是 %w 包装后的新错误。
// getassetlogic.go:34 / updateassetmetalogic.go:34 写的是 `err == model.ErrAssetNotFound`，
// 一旦上游改成包装，这个分支只会静默少写一条日志（返回值不变），单靠 errors.Is 测不出来，
// 所以这里用身份等式把它钉住。
func wantErrIdentity(t *testing.T, label string, err, want error) {
	t.Helper()
	if err != want {
		t.Fatalf("%s：错误 = %v (%T), want 同一个哨兵实例（logic 用的是 == 比较）%v", label, err, err, want)
	}
}

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

func wantStringsEQ(t *testing.T, label, field string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantSeq 比较一条调用/结果序列（读侧用例几乎都在比序列）。
func wantSeq(t *testing.T, label string, got, want []string) {
	t.Helper()
	wantStringsEQ(t, label, "序列", got, want)
}

// wantNoCall 断言 from 之后没有发生任何依赖调用（守卫必须发生在触库/触缓存之前）。
func wantNoCall(t *testing.T, label string, st *store, from int) {
	t.Helper()
	if ops := st.log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

// wantUnixWindow 断言 got 落在 [from,to]（含端点）内。
// logic/model 用 time.Now().Unix() 生成 ctime/mtime，无法预知确切值，只能按窗口判定；
// 窗口外即说明这个字段不是本次调用写的（例如残留的是布景里的 fakeCtime）。
func wantUnixWindow(t *testing.T, label, field string, got, from, to int64) {
	t.Helper()
	if got < from || got > to {
		t.Errorf("%s：%s = %d, want ∈ [%d,%d]（本次调用的时间窗）", label, field, got, from, to)
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

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

// raw 追加一条**已拼好**的轨迹（配合下面的 op* 构造器）：
// 走 add() 等于把外部字符串再交给 Sprintf，含 % 的入参（如 lang、object_key）会拼出假词条。
func (c *callLog) raw(op string) { c.ops = append(c.ops, op) }

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

// --- 轨迹词条（替身与用例期望的**唯一**事实源） ---
//
// 替身的每一条形调用都用这些构造器落轨迹，用例的期望序列也只用它们拼，
// 于是「期望值手抄错格式」这类假红/假绿在编译期就不可能发生。
// 读侧旧用例仍然内联字面量：那是一段有意的交叉校验——两边对不上就立刻红。

func opCachePing() string             { return "cache.Ping" }
func opCacheGet(assetID int64) string { return "cache.Get:" + keyAsset(assetID) }
func opCacheSet(assetID int64) string { return "cache.Set:" + keyAsset(assetID) }
func opCacheDel(assetID int64) string { return "cache.Del:" + keyAsset(assetID) }
func opMetaInsert(uploadID, mid int64) string {
	return "meta.Insert:upload=" + itoa(uploadID) + "/mid=" + itoa(mid)
}
func opMetaFindOne(assetID int64) string { return "meta.FindOne:" + itoa(assetID) }
func opMetaCount(mid int64, state int32) string {
	return "meta.Count:mid=" + itoa(mid) + "/state=" + itoa(int64(state))
}
func opMetaUpdateMeta(assetID int64) string { return "meta.UpdateMeta:" + itoa(assetID) }
func opMetaUpdateState(assetID int64, to int32) string {
	return "meta.UpdateState:" + itoa(assetID) + "->" + itoa(int64(to))
}
func opCoverInsert(assetID int64) string { return "cover.Insert:asset=" + itoa(assetID) }
func opCoverList(assetID int64) string   { return "cover.List:" + itoa(assetID) }
func opSubInsert(assetID int64, lang string) string {
	return "sub.Insert:asset=" + itoa(assetID) + "/lang=" + lang
}
func opSubList(assetID int64) string    { return "sub.List:" + itoa(assetID) }
func opShotInsert(assetID int64) string { return "shot.Insert:asset=" + itoa(assetID) }

// TestTrajectoryVocabularyMatchesFakes 是对账不是行为验证：替身**实际**落下的每一条轨迹，
// 必须与用例用来拼期望的 op* 构造器逐字一致。构造器和替身里的 Sprintf 是两份字面量，
// 谁改了格式另一边就会红，写侧用例就不会把「期望值抄错」误判成「生产行为变了」。
// 唯一不被构造器覆盖的词条是 meta.Select（读侧分页专用），这里内联字面量一起对账。
func TestTrajectoryVocabularyMatchesFakes(t *testing.T) {
	st := newStore()
	ctx := context.Background()
	const probeID = int64(4242)

	_ = st.cache.Ping(ctx)
	seedAsset(t, st, &model.AssetMeta{AssetID: probeID, UploadID: fakeUploadID, Mid: fakeMid})
	_, _, _ = st.cache.GetAsset(ctx, probeID)
	_ = st.cache.SetAsset(ctx, probeID, "{}")
	_ = st.cache.DelAsset(ctx, probeID)

	// 走公开写入路径（会记轨迹），与上面的静默布景区分开。
	newID, err := st.meta.Insert(ctx, &model.AssetMeta{UploadID: fakeUploadID, Mid: fakeMid})
	wantNoErr(t, "对账布景 meta.Insert", err)
	if _, err := st.meta.FindOne(ctx, newID); err != nil {
		t.Fatalf("对账布景 meta.FindOne：%v", err)
	}
	if _, _, err := st.meta.List(ctx, fakeMid, 0, 1, 20); err != nil {
		t.Fatalf("对账布景 meta.List：%v", err)
	}
	wantNoErr(t, "对账布景 meta.UpdateMeta", st.meta.UpdateMeta(ctx, newID, 1000, 640, 360, "h264"))
	wantNoErr(t, "对账布景 meta.UpdateState", st.meta.UpdateState(ctx, newID, model.StateScanned))

	coverID, err := st.cover.Insert(ctx, &model.AssetCover{AssetID: probeID})
	wantNoErr(t, "对账布景 cover.Insert", err)
	if _, err := st.cover.ListByAsset(ctx, probeID); err != nil {
		t.Fatalf("对账布景 cover.ListByAsset：%v", err)
	}
	subID, err := st.sub.Insert(ctx, &model.AssetSubtitle{AssetID: probeID, Lang: "zh-CN"})
	wantNoErr(t, "对账布景 sub.Insert", err)
	if _, err := st.sub.ListByAsset(ctx, probeID); err != nil {
		t.Fatalf("对账布景 sub.ListByAsset：%v", err)
	}
	shotID, err := st.shot.Insert(ctx, &model.AssetScreenshot{AssetID: probeID})
	wantNoErr(t, "对账布景 shot.Insert", err)
	if coverID == 0 || subID == 0 || shotID == 0 {
		t.Fatalf("替身没分配主键：cover=%d sub=%d shot=%d", coverID, subID, shotID)
	}

	wantOps(t, "轨迹词条对账", st.log.opsFrom(0), []string{
		opCachePing(),
		opCacheGet(probeID), opCacheSet(probeID), opCacheDel(probeID),
		opMetaInsert(fakeUploadID, fakeMid),
		opMetaFindOne(newID),
		opMetaCount(fakeMid, 0),
		"meta.Select:mid=" + itoa(fakeMid) + "/state=0/pn=1/ps=20",
		opMetaUpdateMeta(newID),
		opMetaUpdateState(newID, model.StateScanned),
		opCoverInsert(probeID),
		opCoverList(probeID),
		opSubInsert(probeID, "zh-CN"),
		opSubList(probeID),
		opShotInsert(probeID),
	})
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

// errBoom 是注入用的通用下游故障（文本刻意不像任何本域哨兵）。
var errBoom = errors.New("boom-ut-fake-dependency")

// --- 缓存 key 复刻（与 repository 未导出常量逐字对齐；文本对账见 modelsql_test.go） ---

func keyAsset(assetID int64) string { return fmt.Sprintf("asset:a:%d", assetID) }

// --- 排序口径登记表（唯一事实源：model/assetmodel.go 里的 ORDER BY 文本） ---

const (
	orderByAssetsList   = "asset_id DESC" // asset_meta List：新媒资在前
	orderByCoverList    = "cover_id ASC"  // asset_cover ListByAsset：登记顺序
	orderBySubtitleList = "sub_id ASC"    // asset_subtitle ListByAsset：登记顺序
)

// clauseLess 把 ORDER BY 子句映射成「ID a 是否排在 ID b 之前」。
// 登记表里必须**恰好**是 model 出现过的 ORDER BY 子句，多一条少一条都由
// TestModelOrderByClausesStillMatchFakes 判红。
var clauseLess = map[string]func(a, b int64) bool{
	orderByAssetsList:   func(a, b int64) bool { return a > b },
	orderByCoverList:    func(a, b int64) bool { return a < b },
	orderBySubtitleList: func(a, b int64) bool { return a < b },
}

func lessByClause(clause string) func(a, b int64) bool {
	f, ok := clauseLess[clause]
	if !ok {
		panic("未登记的 ORDER BY 子句：" + clause)
	}
	return f
}

// orderIDs 按 clause 排一组 ID。用例的期望序列一律由它现算，不手抄字面量。
func orderIDs(t *testing.T, clause string, ids []int64) []int64 {
	t.Helper()
	out := slices.Clone(ids)
	less := lessByClause(clause)
	sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

// --- 缓存替身 ---

// fakeCache 实现 repository.Cacher。「值为空串」与「key 不存在」都返回 hit=false，
// 与真实 Cache.GetAsset 的 `return bs, bs != "", nil` 同口径（该文本由对账用例钉住）。
type fakeCache struct {
	faultInjector
	log   *callLog
	items map[string]string
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{log: log, items: map[string]string{}}
}

func (f *fakeCache) Ping(context.Context) error { f.log.add("cache.Ping"); return nil }

func (f *fakeCache) GetAsset(_ context.Context, assetID int64) (string, bool, error) {
	key := keyAsset(assetID)
	f.log.add("cache.Get:%s", key)
	if err := f.fail("GetAsset"); err != nil {
		return "", false, err
	}
	return f.items[key], f.items[key] != "", nil
}

func (f *fakeCache) SetAsset(_ context.Context, assetID int64, payload string) error {
	key := keyAsset(assetID)
	f.log.add("cache.Set:%s", key)
	if err := f.fail("SetAsset"); err != nil {
		return err
	}
	f.items[key] = payload
	return nil
}

func (f *fakeCache) DelAsset(_ context.Context, assetID int64) error {
	key := keyAsset(assetID)
	f.log.add("cache.Del:%s", key)
	if err := f.fail("DelAsset"); err != nil {
		return err
	}
	delete(f.items, key)
	return nil
}

// warm 静默预热详情缓存（布数据不记轨迹：命中用例要区分「读穿回填」和「本来就命中」）。
func (f *fakeCache) warm(assetID int64, payload string) { f.items[keyAsset(assetID)] = payload }

// warmAsset 按 Repository 的编码方式（json.Marshal(*model.AssetMeta)）预热一条详情。
func (f *fakeCache) warmAsset(m *model.AssetMeta) { f.warm(m.AssetID, mustMarshalAsset(m)) }

func mustMarshalAsset(m *model.AssetMeta) string {
	bs, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("布景编码媒资缓存失败：%v", err))
	}
	return string(bs)
}

// stored 读缓存里实际存了什么 JSON；第二返回值是 key 是否存在（区别于「存了空串」）。
func (f *fakeCache) stored(assetID int64) (string, bool) {
	p, ok := f.items[keyAsset(assetID)]
	return p, ok
}

// storedAsset 解码缓存里存的媒资行；miss 或解码失败返回 nil。
// 单独解码一次，才能断言「回填的是哪一行、字段全不全」。
func (f *fakeCache) storedAsset(assetID int64) *model.AssetMeta {
	payload, ok := f.stored(assetID)
	if !ok {
		return nil
	}
	var m model.AssetMeta
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return nil
	}
	return &m
}

// --- asset_meta 替身 ---

// fakeMetaModel 实现 model.AssetMetaModel，逐条复刻 model/assetmodel.go 的 SQL 语义。
type fakeMetaModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.AssetMeta
	next int64
}

func newFakeMetaModel(log *callLog) *fakeMetaModel {
	return &fakeMetaModel{log: log, rows: map[int64]*model.AssetMeta{}}
}

// put 是静默写入路径（纪律 5）：显式给了 asset_id 就按它落库，撞主键直接 panic（布景错要立刻炸）。
func (f *fakeMetaModel) put(in *model.AssetMeta) *model.AssetMeta {
	cp := *in
	if cp.AssetID == 0 {
		panic("布景必须显式给 asset_id；自增只走 Insert")
	}
	if _, dup := f.rows[cp.AssetID]; dup {
		panic(fmt.Sprintf("布景重复 asset_id=%d", cp.AssetID))
	}
	f.rows[cp.AssetID] = &cp
	if cp.AssetID > f.next {
		f.next = cp.AssetID
	}
	out := cp
	return &out
}

// Insert 复刻真实 SQL：asset_id 走 AUTO_INCREMENT（忽略入参 ID），ctime/mtime 用入参。
func (f *fakeMetaModel) Insert(_ context.Context, a *model.AssetMeta) (int64, error) {
	f.log.add("meta.Insert:upload=%s/mid=%s", itoa(a.UploadID), itoa(a.Mid))
	if err := f.fail("Insert"); err != nil {
		return 0, fmt.Errorf("asset_meta Insert: %w", err)
	}
	cp := *a
	f.next++
	cp.AssetID = f.next
	f.rows[cp.AssetID] = &cp
	return cp.AssetID, nil
}

// FindOne 复刻 WHERE asset_id = ?；查无此行返回 (nil, nil)（真实实现把 sql.ErrNoRows 翻译成 nil）。
func (f *fakeMetaModel) FindOne(_ context.Context, assetID int64) (*model.AssetMeta, error) {
	f.log.add("meta.FindOne:%s", itoa(assetID))
	if err := f.fail("FindOne"); err != nil {
		return nil, fmt.Errorf("asset_meta FindOne: %w", err)
	}
	row, ok := f.rows[assetID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// List 复刻 model 的 SQL 语义：pn<1→1、ps∉[1,50]→20、mid>0/state>0 才过滤、
// 先 COUNT(*)，total==0 时**不发**第二条 SELECT，否则 ORDER BY asset_id DESC + LIMIT/OFFSET 裁剪。
// 这里断言的是「logic 透传了什么 + Repository 有没有如实搬运结果」，不证明 SQL 文本。
func (f *fakeMetaModel) List(_ context.Context, mid int64, state, pn, ps int32) ([]*model.AssetMeta, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	f.log.add("meta.Count:mid=%s/state=%s", itoa(mid), itoa(int64(state)))
	if err := f.fail("ListCount"); err != nil {
		return nil, 0, fmt.Errorf("asset_meta List count: %w", err)
	}
	var matched []int64
	for id, row := range f.rows {
		if mid > 0 && row.Mid != mid {
			continue
		}
		if state > 0 && row.State != state {
			continue
		}
		matched = append(matched, id)
	}
	total := int32(len(matched))
	if total == 0 {
		return nil, 0, nil
	}
	sort.Slice(matched, func(i, j int) bool { return lessByClause(orderByAssetsList)(matched[i], matched[j]) })

	f.log.add("meta.Select:mid=%s/state=%s/pn=%d/ps=%d", itoa(mid), itoa(int64(state)), pn, ps)
	if err := f.fail("ListRows"); err != nil {
		return nil, 0, fmt.Errorf("asset_meta List list: %w", err)
	}
	offset := int((pn - 1) * ps)
	if offset >= len(matched) {
		// 真实 SQL：LIMIT ? OFFSET ? 越界返回空集，total 仍是全量过滤计数。
		return nil, total, nil
	}
	page := matched[offset:]
	if len(page) > int(ps) {
		page = page[:ps]
	}
	out := make([]*model.AssetMeta, 0, len(page))
	for _, id := range page {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, total, nil
}

// UpdateMeta 复刻 UPDATE ... WHERE asset_id = ?：RowsAffected==0 → **未包装**的 ErrAssetNotFound。
func (f *fakeMetaModel) UpdateMeta(_ context.Context, assetID int64, duration int64, width, height int32, codec string) error {
	f.log.add("meta.UpdateMeta:%s", itoa(assetID))
	if err := f.fail("UpdateMeta"); err != nil {
		return fmt.Errorf("asset_meta UpdateMeta: %w", err)
	}
	row, ok := f.rows[assetID]
	if !ok {
		return model.ErrAssetNotFound
	}
	row.Duration, row.Width, row.Height, row.Codec = duration, width, height, codec
	row.Mtime = time.Now().Unix()
	return nil
}

// UpdateState 复刻 UPDATE state WHERE asset_id = ?：同样不校验当前状态（SQL 里没有 CAS 条件）。
func (f *fakeMetaModel) UpdateState(_ context.Context, assetID int64, toState int32) error {
	f.log.add("meta.UpdateState:%s->%d", itoa(assetID), toState)
	if err := f.fail("UpdateState"); err != nil {
		return fmt.Errorf("asset_meta UpdateState: %w", err)
	}
	row, ok := f.rows[assetID]
	if !ok {
		return model.ErrAssetNotFound
	}
	row.State = toState
	row.Mtime = time.Now().Unix()
	return nil
}

// row 读库内某媒资行（值读）；不存在返回 nil。
func (f *fakeMetaModel) row(assetID int64) *model.AssetMeta {
	r, ok := f.rows[assetID]
	if !ok {
		return nil
	}
	cp := *r
	return &cp
}

// snapshotByPK 返回全部库存行的值拷贝，按主键升序。
// 只用于「一次调用结束后库里到底留下什么形态」这类残留断言（写侧用例反复用到），
// **不代表任何 SQL 的排序口径**：读侧顺序一律以 clauseLess 登记的 ORDER BY 为准。
func (f *fakeMetaModel) snapshotByPK() []*model.AssetMeta {
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]*model.AssetMeta, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out
}

// --- asset_cover 替身 ---

type fakeCoverModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.AssetCover
	next int64
}

func newFakeCoverModel(log *callLog) *fakeCoverModel {
	return &fakeCoverModel{log: log, rows: map[int64]*model.AssetCover{}}
}

func (f *fakeCoverModel) put(in *model.AssetCover) *model.AssetCover {
	cp := *in
	if cp.CoverID == 0 {
		panic("布景必须显式给 cover_id")
	}
	if _, dup := f.rows[cp.CoverID]; dup {
		panic(fmt.Sprintf("布景重复 cover_id=%d", cp.CoverID))
	}
	f.rows[cp.CoverID] = &cp
	if cp.CoverID > f.next {
		f.next = cp.CoverID
	}
	out := cp
	return &out
}

func (f *fakeCoverModel) Insert(_ context.Context, c *model.AssetCover) (int64, error) {
	f.log.add("cover.Insert:asset=%s", itoa(c.AssetID))
	if err := f.fail("Insert"); err != nil {
		return 0, fmt.Errorf("asset_cover Insert: %w", err)
	}
	cp := *c
	f.next++
	cp.CoverID = f.next
	f.rows[cp.CoverID] = &cp
	return cp.CoverID, nil
}

// row 读某条封面（值读）；不存在返回 nil。
func (f *fakeCoverModel) row(coverID int64) *model.AssetCover {
	r, ok := f.rows[coverID]
	if !ok {
		return nil
	}
	cp := *r
	return &cp
}

// snapshotByPK 返回全部库存封面行的值拷贝，按 cover_id 升序（仅用于残留形态断言）。
func (f *fakeCoverModel) snapshotByPK() []*model.AssetCover {
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]*model.AssetCover, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out
}

// ListByAsset 复刻 WHERE asset_id = ? ORDER BY cover_id ASC（无 LIMIT）：
// 不校验媒资是否存在，无命中返回空切片。
func (f *fakeCoverModel) ListByAsset(_ context.Context, assetID int64) ([]*model.AssetCover, error) {
	f.log.add("cover.List:%s", itoa(assetID))
	if err := f.fail("ListByAsset"); err != nil {
		return nil, fmt.Errorf("asset_cover ListByAsset: %w", err)
	}
	var ids []int64
	for id, r := range f.rows {
		if r.AssetID == assetID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return lessByClause(orderByCoverList)(ids[i], ids[j]) })
	out := make([]*model.AssetCover, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, nil
}

// --- asset_subtitle 替身 ---

type fakeSubtitleModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.AssetSubtitle
	byUK map[string]int64 // uniq_asset_lang：asset_id/lang → sub_id
	next int64
}

func newFakeSubtitleModel(log *callLog) *fakeSubtitleModel {
	return &fakeSubtitleModel{log: log, rows: map[int64]*model.AssetSubtitle{}, byUK: map[string]int64{}}
}

func ukAssetLang(assetID int64, lang string) string { return fmt.Sprintf("%d/%s", assetID, lang) }

// errDuplicateKey 文本对齐 MySQL 1062；调用方只按 errors.Is 判定「撞了唯一键」。
var errDuplicateKey = errors.New("Error 1062: Duplicate entry")

func (f *fakeSubtitleModel) put(in *model.AssetSubtitle) (*model.AssetSubtitle, error) {
	cp := *in
	if cp.SubID == 0 {
		panic("布景必须显式给 sub_id")
	}
	if _, dup := f.rows[cp.SubID]; dup {
		return nil, fmt.Errorf("%w for key 'PRIMARY': sub_id=%d", errDuplicateKey, cp.SubID)
	}
	if other, dup := f.byUK[ukAssetLang(cp.AssetID, cp.Lang)]; dup && other != cp.SubID {
		return nil, fmt.Errorf("%w for key 'uniq_asset_lang': %d/%s（与 sub_id=%d 冲突）",
			errDuplicateKey, cp.AssetID, cp.Lang, other)
	}
	f.rows[cp.SubID] = &cp
	f.byUK[ukAssetLang(cp.AssetID, cp.Lang)] = cp.SubID
	if cp.SubID > f.next {
		f.next = cp.SubID
	}
	out := cp
	return &out, nil
}

// Insert 复刻 INSERT INTO asset_subtitle：撞 uniq_asset_lang 时把 1062 原样上抛（本服务不去重）。
func (f *fakeSubtitleModel) Insert(_ context.Context, s *model.AssetSubtitle) (int64, error) {
	f.log.add("sub.Insert:asset=%s/lang=%s", itoa(s.AssetID), s.Lang)
	if err := f.fail("Insert"); err != nil {
		return 0, fmt.Errorf("asset_subtitle Insert: %w", err)
	}
	cp := *s
	f.next++
	cp.SubID = f.next
	saved, err := f.put(&cp)
	if err != nil {
		return 0, fmt.Errorf("asset_subtitle Insert: %w", err)
	}
	return saved.SubID, nil
}

// row 读某条字幕（值读）；不存在返回 nil。
func (f *fakeSubtitleModel) row(subID int64) *model.AssetSubtitle {
	r, ok := f.rows[subID]
	if !ok {
		return nil
	}
	cp := *r
	return &cp
}

// rowByLang 按 uniq_asset_lang 的业务键 (asset_id, lang) 读行。
// 注意：替身按**字节**比较 lang，而 MySQL 的 utf8mb4 默认排序规则大小写不敏感，
// 所以「zh-CN 与 ZH-CN 是否算同一行」在这里不可测（见 README 缺口 8 的补充说明）。
func (f *fakeSubtitleModel) rowByLang(assetID int64, lang string) *model.AssetSubtitle {
	id, ok := f.byUK[ukAssetLang(assetID, lang)]
	if !ok {
		return nil
	}
	return f.row(id)
}

// snapshotByPK 返回全部库存字幕行的值拷贝，按 sub_id 升序（仅用于残留形态断言）。
func (f *fakeSubtitleModel) snapshotByPK() []*model.AssetSubtitle {
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]*model.AssetSubtitle, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out
}

// ListByAsset 复刻 WHERE asset_id = ? ORDER BY sub_id ASC。
func (f *fakeSubtitleModel) ListByAsset(_ context.Context, assetID int64) ([]*model.AssetSubtitle, error) {
	f.log.add("sub.List:%s", itoa(assetID))
	if err := f.fail("ListByAsset"); err != nil {
		return nil, fmt.Errorf("asset_subtitle ListByAsset: %w", err)
	}
	var ids []int64
	for id, r := range f.rows {
		if r.AssetID == assetID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return lessByClause(orderBySubtitleList)(ids[i], ids[j]) })
	out := make([]*model.AssetSubtitle, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, nil
}

// --- asset_screenshot 替身（model 接口只有 Insert：本服务没有截图查询 RPC） ---

type fakeScreenshotModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.AssetScreenshot
	next int64
}

func newFakeScreenshotModel(log *callLog) *fakeScreenshotModel {
	return &fakeScreenshotModel{log: log, rows: map[int64]*model.AssetScreenshot{}}
}

func (f *fakeScreenshotModel) put(in *model.AssetScreenshot) *model.AssetScreenshot {
	cp := *in
	if cp.ShotID == 0 {
		panic("布景必须显式给 shot_id")
	}
	f.rows[cp.ShotID] = &cp
	if cp.ShotID > f.next {
		f.next = cp.ShotID
	}
	out := cp
	return &out
}

func (f *fakeScreenshotModel) Insert(_ context.Context, s *model.AssetScreenshot) (int64, error) {
	f.log.add("shot.Insert:asset=%s", itoa(s.AssetID))
	if err := f.fail("Insert"); err != nil {
		return 0, fmt.Errorf("asset_screenshot Insert: %w", err)
	}
	cp := *s
	f.next++
	cp.ShotID = f.next
	f.rows[cp.ShotID] = &cp
	return cp.ShotID, nil
}

// row 读某张截图（值读）；不存在返回 nil。
// 本服务**没有**截图读接口（rpc/asset.proto 里没有 ListScreenshots），
// 所以截图落库后的形态只能这样从替身里读出来——这正是 README 缺口 9 说的事实。
func (f *fakeScreenshotModel) row(shotID int64) *model.AssetScreenshot {
	r, ok := f.rows[shotID]
	if !ok {
		return nil
	}
	cp := *r
	return &cp
}

// snapshotByPK 返回全部库存截图行的值拷贝，按 shot_id 升序（仅用于残留形态断言）。
func (f *fakeScreenshotModel) snapshotByPK() []*model.AssetScreenshot {
	ids := make([]int64, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]*model.AssetScreenshot, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out
}

// --- 装配 ---

type store struct {
	log   *callLog
	cache *fakeCache
	meta  *fakeMetaModel
	cover *fakeCoverModel
	sub   *fakeSubtitleModel
	shot  *fakeScreenshotModel
	repo  *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	st := &store{log: log, cache: newFakeCache(log)}
	st.meta = newFakeMetaModel(log)
	st.cover = newFakeCoverModel(log)
	st.sub = newFakeSubtitleModel(log)
	st.shot = newFakeScreenshotModel(log)
	st.repo = repository.NewWithDeps(st.cache, st.meta, st.cover, st.sub, st.shot)
	return st
}

// newTestSvc 装配只带内存依赖的 ServiceContext。与生产 ServiceContext 的差别仅在
// Repository 的 5 个依赖是替身；Config 留零值（读侧 4 个 logic 只读 Repository）。
func newTestSvc(st *store) *svc.ServiceContext {
	return &svc.ServiceContext{Config: config.Config{}, Repository: st.repo}
}

// --- 布数据（静默路径，不写轨迹） ---

// 明显假的测试值：不取真实桶名/真实对象键格式，mid/upload_id 都用 9/7 开头的保留段。
const (
	fakeBucket    = "ut-fake-bucket"
	fakeMd5       = "0123456789abcdef0123456789abcdef"
	fakeMid       = 900001
	fakeMidOther  = 900002
	fakeUploadID  = 770001
	fakeCtime     = 1700000000
	fakeOriginKey = "ugc-ut/fake/asset-origin.mp4"
	fakeAssetID   = 910001 // 附属（封面/字幕）用例挂靠的媒资 ID
)

func seedAsset(t *testing.T, st *store, m *model.AssetMeta) *model.AssetMeta {
	t.Helper()
	if m.Ctime == 0 {
		m.Ctime = fakeCtime
	}
	if m.Mtime == 0 {
		m.Mtime = fakeCtime
	}
	saved := st.meta.put(m)
	return st.meta.row(saved.AssetID)
}

func seedCover(t *testing.T, st *store, c *model.AssetCover) *model.AssetCover {
	t.Helper()
	if c.Ctime == 0 {
		c.Ctime = fakeCtime
	}
	st.cover.put(c)
	cp := *c
	return &cp
}

func seedSubtitle(t *testing.T, st *store, s *model.AssetSubtitle) *model.AssetSubtitle {
	t.Helper()
	if s.Ctime == 0 {
		s.Ctime = fakeCtime
	}
	saved, err := st.sub.put(s)
	if err != nil {
		t.Fatalf("布景写入字幕失败：%v", err)
	}
	cp := *saved
	return &cp
}

// --- 投影摊平（「RPC 应答 == 库存行」要能一条等式比完） ---

func assetLine(m *model.AssetMeta) string {
	if m == nil {
		return "<nil>"
	}
	return fmt.Sprintf("asset=%d/upload=%d/mid=%d/bucket=%s/key=%s/size=%d/md5=%s/dur=%d/w=%d/h=%d/codec=%s/state=%d",
		m.AssetID, m.UploadID, m.Mid, m.Bucket, m.ObjectKey, m.Size, m.Md5, m.Duration, m.Width, m.Height, m.Codec, m.State)
}

func replyAssetLine(r *rpc.AssetReply) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("asset=%d/upload=%d/mid=%d/bucket=%s/key=%s/size=%d/md5=%s/dur=%d/w=%d/h=%d/codec=%s/state=%d",
		r.GetAssetId(), r.GetUploadId(), r.GetMid(), r.GetBucket(), r.GetObjectKey(), r.GetSize(), r.GetMd5(),
		r.GetDuration(), r.GetWidth(), r.GetHeight(), r.GetCodec(), int32(r.GetState()))
}

func coverLine(c *model.AssetCover) string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf("cover=%d/asset=%d/bucket=%s/key=%s/w=%d/h=%d/ctime=%d",
		c.CoverID, c.AssetID, c.Bucket, c.ObjectKey, c.Width, c.Height, c.Ctime)
}

func replyCoverLine(r *rpc.CoverReply) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("cover=%d/asset=%d/bucket=%s/key=%s/w=%d/h=%d/ctime=%d",
		r.GetCoverId(), r.GetAssetId(), r.GetBucket(), r.GetObjectKey(), r.GetWidth(), r.GetHeight(), r.GetCtime())
}

func subtitleLine(s *model.AssetSubtitle) string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("sub=%d/asset=%d/lang=%s/bucket=%s/key=%s/ctime=%d",
		s.SubID, s.AssetID, s.Lang, s.Bucket, s.ObjectKey, s.Ctime)
}

func replySubtitleLine(r *rpc.SubtitleReply) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("sub=%d/asset=%d/lang=%s/bucket=%s/key=%s/ctime=%d",
		r.GetSubId(), r.GetAssetId(), r.GetLang(), r.GetBucket(), r.GetObjectKey(), r.GetCtime())
}

func shotLine(s *model.AssetScreenshot) string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("shot=%d/asset=%d/ts=%d/bucket=%s/key=%s/ctime=%d",
		s.ShotID, s.AssetID, s.Timestamp, s.Bucket, s.ObjectKey, s.Ctime)
}

func replyShotLine(r *rpc.ScreenshotReply) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("shot=%d/asset=%d/ts=%d/bucket=%s/key=%s/ctime=%d",
		r.GetShotId(), r.GetAssetId(), r.GetTimestamp(), r.GetBucket(), r.GetObjectKey(), r.GetCtime())
}

func lines[T any](items []T, f func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, f(it))
	}
	return out
}
