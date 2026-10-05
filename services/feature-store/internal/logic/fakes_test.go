// 本文件是 feature-store logic 层的测试替身：六个 model.*Model 接口的内存实现，
// 加上一个记录事务边界的 fakeDB。全程不接 MySQL / Redis / 网络（AGENTS.md §9）。
//
// 三条让替身「不说好话」的纪律：
//  1. 每个写方法在入口重复调用 model 包里同一份校验/摘要函数，而不是另写一套宽松规则
//     —— 否则「测试绿了、真 SQL 会报错」这类假阳性排除不掉；
//  2. TransactCtx 在回调返回 error 时按真事务语义还原全部六张表
//     —— 「审计写了、指针没动」这种中间态只要没回滚就会被断言抓出来；
//  3. 记录的是副作用（方法名、写入行、CAS 结果、回源键集合），断言因此打在副作用上
//     而不只是返回值上。
//
// svcCtx.Cache 恒为 nil（建 Redis 客户端就是走网络）：在线读的「主存层整体不可用」
// 由 Read.DBFallbackEnabled=false 这条演练开关表达，「DB 回源失败」由 fakeValues 的
// 错误注入表达 —— 两条降级路径都能在离线条件下被真实触发。
package logic

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"go-video/services/feature-store/internal/config"
	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 固定时钟与测试用主体标识。
const (
	// testNow 是注入给 model.NowUnix 的「服务端当前时间」。
	testNow = int64(1_700_000_000)
	// testDeviceID 是 40 位十六进制设备摘要（ValidEntityID 允许的三档之一）。
	testDeviceID = "0123456789abcdef0123456789abcdef01234567"
	// testIPHash 是 64 位十六进制 IP 摘要。
	testIPHash = "6d796f7574756265303132333435363738393031323334353637383930313233"
	// testMid 是十进制主键串。
	testMid = "10086"
	// plaintextDeviceSerial 是明文设备号：含非 hex 字符，必须被隐私闸门挡在 SQL 之前。
	plaintextDeviceSerial = "RMX3700_ABCDE12345"
	// rawIPv4 是原始 IP：点分十进制不是十六进制摘要。
	rawIPv4 = "192.168.5.13"
	// testPhone 是明文手机号，任何入参里都必须被拒。
	testPhone = "13800138000"
	// testSourceMetricKey 是行为类特征的口径追溯键（NewFeatureValue 强制要求）。
	testSourceMetricKey = "u_play_finish_7d@v1"
)

// rollbackable 由每个替身实现，供 fakeDB 做「回滚 = 还原到事务开始前的表内容」。
type rollbackable interface {
	snapshot() tableState
}

// tableState 是一份可还原的表内容。
type tableState func()

// fixture 是一次测试的全部装配。
type fixture struct {
	t   *testing.T
	ctx context.Context
	*svc.ServiceContext

	db        *fakeDB
	defs      *fakeDefinitions
	pointers  *fakeActiveVersions
	values    *fakeValues
	switches  *fakeSwitches
	backfills *fakeBackfillJobs
	receipts  *fakeReceipts

	now int64
}

// newFixture 装配「配置合法 + 依赖全为替身 + 时钟固定」的上下文。
//
// 这里刻意调用真实的 Config.Validate：测试若跑在一个启动期就会被自己拦死的配置上，
// 等于什么都没测。CacheRedis.Host 填占位值只为过校验，Cache 字段仍留 nil。
func newFixture(t *testing.T) *fixture {
	t.Helper()

	var c config.Config
	c.Name = "feature-store"
	c.Timeout = 1000
	c.DataSource = "fs:fs@tcp(127.0.0.1:3306)/" + config.DatabaseName +
		"?charset=utf8mb4&parseTime=true"
	c.CacheRedis.Host = "127.0.0.1:6379" // 仅用于通过 Config.Validate；下面不建客户端
	c.Read = config.ReadConf{
		MaxBatchResponseBytes:     model.MaxBatchResponseBytes,
		CacheJitterRatio:          0.2,
		ActivePointerCacheSeconds: 10,
		DBFallbackEnabled:         true,
	}
	c.Write = config.WriteConf{
		ReceiptLeaseSeconds:     60,
		ReceiptRetentionSeconds: 604800,
		MaxPurgeRowsPerCall:     model.MaxPurgeRows,
	}
	c.Backfill = config.BackfillConf{LeaseSeconds: 120, BatchRows: 1000, WorkerEnabled: false}
	c.Privacy = config.PrivacyConf{
		OperatorPrefixes:      []string{"privacy-job:"},
		ExportMaxPrivacyLevel: model.PrivacyUserProfile,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("fixture config must pass Config.Validate: %v", err)
	}

	f := &fixture{
		t:         t,
		ctx:       context.Background(),
		defs:      &fakeDefinitions{rows: map[model.DefinitionKey]model.FeatureDefinition{}},
		pointers:  &fakeActiveVersions{rows: map[string]model.ActiveVersion{}},
		values:    &fakeValues{rows: map[model.ValueKey]model.FeatureValue{}},
		switches:  &fakeSwitches{rows: map[int64]model.VersionSwitch{}},
		backfills: &fakeBackfillJobs{rows: map[int64]model.BackfillJob{}},
		receipts:  &fakeReceipts{rows: map[string]model.WriteReceipt{}},
		now:       testNow,
	}
	// 值替身要按定义的隐私级别与 ACTIVE 指针收窄视图，因此反向引用这两张表。
	f.values.defs = f.defs
	f.values.pointers = f.pointers
	f.db = &fakeDB{fx: f}
	f.ServiceContext = &svc.ServiceContext{
		Config:         c,
		DB:             f.db,
		Cache:          nil, // 见文件头：不建 Redis 客户端
		Definitions:    f.defs,
		ActiveVersions: f.pointers,
		Values:         f.values,
		Switches:       f.switches,
		Backfills:      f.backfills,
		Receipts:       f.receipts,
	}
	f.now = testNow
	restore := model.SetClockForTest(func() time.Time { return time.Unix(f.now, 0) })
	t.Cleanup(restore)
	return f
}

// --- 配置微调（都在调用 logic 之前用）---

func (f *fixture) withDBFallback(on bool) *fixture {
	f.Config.Read.DBFallbackEnabled = on
	return f
}

func (f *fixture) withMaxResponseBytes(n int) *fixture {
	f.Config.Read.MaxBatchResponseBytes = n
	return f
}

func (f *fixture) withPurgeLimit(n int32) *fixture {
	f.Config.Write.MaxPurgeRowsPerCall = n
	return f
}

func (f *fixture) withPrivacyPrefixes(prefixes ...string) *fixture {
	f.Config.Privacy.OperatorPrefixes = prefixes
	return f
}

func (f *fixture) withExportMaxPrivacy(level int32) *fixture {
	f.Config.Privacy.ExportMaxPrivacyLevel = level
	return f
}

func (f *fixture) withReceiptLease(seconds int64) *fixture {
	f.Config.Write.ReceiptLeaseSeconds = seconds
	return f
}

func (f *fixture) withActivePointerTTLOff() *fixture {
	f.Config.Read.ActivePointerCacheSeconds = 0
	return f
}

// advance 把服务端时钟往前推，用于跨 TTL / 租约到期边界的场景。
// 时钟用闭包读 f.now，所以推完之后 model.NowUnix 与 f.nowUnix 同步生效。
func (f *fixture) advance(seconds int64) *fixture {
	f.now += seconds
	return f
}

func (f *fixture) nowUnix() int64 { return f.now }

// --- 种子数据 ---

type defOpt func(*model.FeatureDefinition)

func withScope(scope int32) defOpt { return func(d *model.FeatureDefinition) { d.EntityScope = scope } }

func withPrivacy(p int32) defOpt {
	return func(d *model.FeatureDefinition) { d.PrivacyLevel = p }
}

func withState(s int32) defOpt { return func(d *model.FeatureDefinition) { d.State = s } }

func withSource(s int32) defOpt { return func(d *model.FeatureDefinition) { d.Source = s } }

func withTTL(seconds int64) defOpt {
	return func(d *model.FeatureDefinition) { d.TTLSeconds = seconds }
}

func withDefault(raw string) defOpt {
	return func(d *model.FeatureDefinition) { d.DefaultValue = raw }
}

func withScalar(vt int32) defOpt {
	return func(d *model.FeatureDefinition) { d.ValueType, d.Dimension = vt, 0 }
}

func withInt64List(dimension int32, def string) defOpt {
	return func(d *model.FeatureDefinition) {
		d.ValueType, d.Dimension, d.DefaultValue = model.ValueTypeInt64List, dimension, def
	}
}

func withWindow(seconds int64) defOpt {
	return func(d *model.FeatureDefinition) { d.WindowSeconds = seconds }
}

// newDef 造一条自洽的定义并填好摘要（摘要算法走生产代码，不在此复刻）。
func newDef(key string, version int32, opts ...defOpt) *model.FeatureDefinition {
	d := &model.FeatureDefinition{
		FeatureKey:    key,
		Version:       version,
		Name:          key,
		ValueType:     model.ValueTypeInt64,
		EntityScope:   model.EntityScopeMid,
		Source:        model.SourceSpmMetric,
		PrivacyLevel:  model.PrivacyPseudonymous,
		WindowSeconds: 7 * 24 * 3600,
		TTLSeconds:    3600,
		DefaultValue:  "7",
		State:         model.FeatureStateActive,
		Description:   "近 7 日完播率分子",
		ChangeNote:    "首版",
		CreatedBy:     "admin:tester",
	}
	for _, o := range opts {
		o(d)
	}
	if err := model.ValidateFeatureDefinition(d); err != nil {
		panic("test definition violates its own contract: " + err.Error())
	}
	d.FillDigests()
	return d
}

// putDef 把定义放入替身表（同键覆盖），返回入库副本指针。
func (f *fixture) putDef(d *model.FeatureDefinition) *model.FeatureDefinition {
	f.t.Helper()
	f.defs.rows[model.DefinitionKey{FeatureKey: d.FeatureKey, Version: d.Version}] = *d
	out := *d
	return &out
}

// putPointer 放一行 ACTIVE 指针（保留已有 pointer_id / last_switch_id）。
func (f *fixture) putPointer(key string, active, previous int32) *model.ActiveVersion {
	f.t.Helper()
	row := model.ActiveVersion{
		FeatureKey: key, ActiveVersion: active, PreviousVersion: previous,
		Ctime: f.now, Mtime: f.now,
	}
	if existing, ok := f.pointers.rows[key]; ok {
		row.PointerID = existing.PointerID
		row.LastSwitchID = existing.LastSwitchID
	} else {
		f.pointers.nextID++
		row.PointerID = f.pointers.nextID
	}
	f.pointers.rows[key] = row
	out := row
	return &out
}

// registerActive 是一条最常见的种子：一个 ACTIVE 定义 + 指向它的指针。
func (f *fixture) registerActive(d *model.FeatureDefinition) *model.FeatureDefinition {
	f.putDef(d)
	f.putPointer(d.FeatureKey, d.Version, 0)
	return d
}

// putValue 用生产的 NewFeatureValue 造一行值（编码/TTL/追溯由 model 判）再入库。
// eventTime<=0 表示「按当前时间」。
func (f *fixture) putValue(def *model.FeatureDefinition, version int32, entityID string,
	p model.ValuePayload, eventTime int64) *model.FeatureValue {
	f.t.Helper()
	key := model.ValueKey{FeatureKey: def.FeatureKey, Version: version,
		EntityScope: def.EntityScope, EntityID: entityID}
	v, err := model.NewFeatureValue(def, key, p, eventTime, testSourceMetricKey,
		"seed-req", "system:seeder", 0)
	if err != nil {
		f.t.Fatalf("seed value %s: %v", key.CacheKey(), err)
	}
	return f.storeValue(v)
}

// putInt64 是标量 int64 值的便捷种子。
func (f *fixture) putInt64(def *model.FeatureDefinition, version int32, entityID string,
	val, eventTime int64) *model.FeatureValue {
	return f.putValue(def, version, entityID,
		model.ValuePayload{ValueType: model.ValueTypeInt64, Int64Value: val}, eventTime)
}

// putRawValue 直接入库一行（绕过 NewFeatureValue），只为造脏数据：
// expire_at=0、RETIRED 版本上的残值（契约规定 RETIRED 不接受写入，
// NewFeatureValue 因此拒造这种行）、与定义 value_type 不符的列组合等。
func (f *fixture) putRawValue(v *model.FeatureValue) *model.FeatureValue {
	f.t.Helper()
	return f.storeValue(v)
}

func (f *fixture) storeValue(v *model.FeatureValue) *model.FeatureValue {
	if v.ValueID == 0 {
		f.values.nextID++
		v.ValueID = f.values.nextID
	} else if v.ValueID > f.values.nextID {
		f.values.nextID = v.ValueID
	}
	out := *v
	f.values.rows[v.Key()] = out
	return &out
}

// valueRow 读回替身表里的行（断言副作用用）。
func (f *fixture) valueRow(key model.ValueKey) (model.FeatureValue, bool) {
	v, ok := f.values.rows[key]
	return v, ok
}

// --- fakeDB：只实现 logic 用到的三个方法，其余留给内嵌接口（被调用即 panic，
// 而 logic 不会走到，因此不会掩盖任何真实行为）---

type fakeDB struct {
	sqlx.SqlConn
	fx *fixture
	// boundaries 记录事务边界，供断言「副作用确实发生在事务里」。
	boundaries []string
	// failCommit 让回调成功返回后仍强制回滚：测「提交阶段失败也不能留下半成品」。
	failCommit bool
}

type fakeSession struct{ sqlx.Session }

func (d *fakeDB) Transact(fn func(sqlx.Session) error) error {
	return d.TransactCtx(context.Background(), func(_ context.Context, s sqlx.Session) error {
		return fn(s)
	})
}

func (d *fakeDB) TransactCtx(ctx context.Context,
	fn func(context.Context, sqlx.Session) error) error {
	snaps := d.fx.takeSnaps()
	d.boundaries = append(d.boundaries, "begin")
	err := fn(ctx, fakeSession{})
	if d.failCommit && err == nil {
		err = context.DeadlineExceeded
	}
	if err != nil {
		d.fx.applySnaps(snaps)
		d.boundaries = append(d.boundaries, "rollback")
		return err
	}
	d.boundaries = append(d.boundaries, "commit")
	return nil
}

// pairs 汇总事务边界，断言「begin 与 commit/rollback 一一对应」。
func (d *fakeDB) pairs() (begins, commits, rollbacks int) {
	for _, b := range d.boundaries {
		switch b {
		case "begin":
			begins++
		case "commit":
			commits++
		case "rollback":
			rollbacks++
		}
	}
	return
}

func (f *fixture) takeSnaps() []tableState {
	parts := []rollbackable{f.defs, f.pointers, f.values, f.switches, f.backfills, f.receipts}
	out := make([]tableState, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.snapshot())
	}
	return out
}

func (f *fixture) applySnaps(snaps []tableState) {
	for _, s := range snaps {
		s()
	}
}

func (f *fixture) txBegins() int {
	b, _, _ := f.db.pairs()
	return b
}

func (f *fixture) txCommits() int {
	_, c, _ := f.db.pairs()
	return c
}

func (f *fixture) txRollbacks() int {
	_, _, r := f.db.pairs()
	return r
}

// record 统一记录调用名，断言「有没有走到这一层」时不必给每个替身写一套。
func record(calls *[]string, name string) { *calls = append(*calls, name) }

func called(calls []string, name string) bool {
	for _, c := range calls {
		if c == name {
			return true
		}
	}
	return false
}

func countCalled(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

// --- feature_definition ---

type fakeDefinitions struct {
	rows  map[model.DefinitionKey]model.FeatureDefinition
	calls []string
	// lastFilter 供断言分页/过滤参数是否按契约下传。
	lastFilter model.FeatureDefinitionFilter
	// listByKeysSeen 记录每次批量取的键，供「一批一次读而不是 N 次点查」断言。
	listByKeysSeen [][]model.DefinitionKey
	// omitFromListByKeys 让「库里确实有这一行，但批量取时读不到」可测。
	// 真库对应的是一次读与另一次读之间的并发删除，或索引/数据不一致；
	// logic 里那条「有值却无定义」的防御分支在其余替身语义下不可达
	// （entityRows 与 ListByKeys 读的是同一张 map），只有这一处注入能触达它。
	omitFromListByKeys map[model.DefinitionKey]bool
	// raceWinner 让「本实例查不到行、提交时才发现对手已经插了同一版本」这一条并发分支可测：
	// 它在重复键检测之前把对方的行放进表里，随后的 uniq_key_version 冲突因此是真实的。
	raceWinner *model.FeatureDefinition

	errInsert        error
	errFindOne       error
	errList          error
	errListByKeys    error
	errUpdateState   error
	errUpdatePrivacy error
	// lostUpdate 与 fakeActiveVersions 上的同名字段同一个用途：在「logic 读到旧值」与
	// 「条件 UPDATE 比较」之间制造一次并发提交，让 CAS 分支可测。
	lostUpdate func(featureKey string, version int32, column string)
}

func (m *fakeDefinitions) snapshot() tableState {
	cp := make(map[model.DefinitionKey]model.FeatureDefinition, len(m.rows))
	for k, v := range m.rows {
		cp[k] = v
	}
	return func() { m.rows = cp }
}

func (m *fakeDefinitions) Insert(ctx context.Context, d *model.FeatureDefinition) error {
	record(&m.calls, "definitions.Insert")
	if err := model.ValidateFeatureDefinition(d); err != nil {
		return err
	}
	key := model.DefinitionKey{FeatureKey: d.FeatureKey, Version: d.Version}
	if m.raceWinner != nil {
		// 只把「对手那一事务刚提交的行」放进表，不改动本次要写的 d。
		winner := *m.raceWinner
		m.rows[model.DefinitionKey{FeatureKey: winner.FeatureKey, Version: winner.Version}] = winner
		m.raceWinner = nil
	}
	if _, ok := m.rows[key]; ok {
		return model.ErrFeatureVersionExists
	}
	if m.errInsert != nil {
		return m.errInsert
	}
	row := *d
	if row.Ctime == 0 {
		row.Ctime = model.NowUnix()
	}
	row.Mtime = row.Ctime
	m.rows[key] = row
	return nil
}

func (m *fakeDefinitions) FindOne(ctx context.Context, featureKey string,
	version int32) (*model.FeatureDefinition, error) {
	record(&m.calls, "definitions.FindOne")
	if m.errFindOne != nil {
		return nil, m.errFindOne
	}
	row, ok := m.rows[model.DefinitionKey{FeatureKey: featureKey, Version: version}]
	if !ok {
		return nil, model.ErrFeatureNotFound
	}
	out := row
	return &out, nil
}

func (m *fakeDefinitions) ListByKey(ctx context.Context,
	featureKey string) ([]*model.FeatureDefinition, error) {
	record(&m.calls, "definitions.ListByKey")
	out := m.sorted(func(*model.FeatureDefinition) bool { return true })
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

func (m *fakeDefinitions) List(ctx context.Context,
	f model.FeatureDefinitionFilter) ([]*model.FeatureDefinition, int64, error) {
	record(&m.calls, "definitions.List")
	m.lastFilter = f
	if m.errList != nil {
		return nil, 0, m.errList
	}
	f.Normalize()
	all := m.sorted(func(d *model.FeatureDefinition) bool {
		if f.KeyPrefix != "" && !strings.HasPrefix(d.FeatureKey, f.KeyPrefix) {
			return false
		}
		if f.EntityScope != model.EntityScopeUnspecified && d.EntityScope != f.EntityScope {
			return false
		}
		if f.Source != model.SourceUnspecified && d.Source != f.Source {
			return false
		}
		if f.State != model.FeatureStateUnspecified && d.State != f.State {
			return false
		}
		if f.MaxPrivacyLevel != 0 && d.PrivacyLevel > f.MaxPrivacyLevel {
			return false
		}
		return true
	})
	total := int64(len(all))
	if total == 0 {
		return nil, 0, nil
	}
	start := int(f.Pn-1) * int(f.Ps)
	if start >= len(all) {
		return nil, total, nil
	}
	return all[start:min(start+int(f.Ps), len(all))], total, nil
}

func (m *fakeDefinitions) ListByKeys(ctx context.Context,
	keys []model.DefinitionKey) (map[model.DefinitionKey]*model.FeatureDefinition, error) {
	record(&m.calls, "definitions.ListByKeys")
	m.listByKeysSeen = append(m.listByKeysSeen, append([]model.DefinitionKey(nil), keys...))
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > model.MaxBatchFeatures {
		return nil, model.ErrTooManyEntries
	}
	if m.errListByKeys != nil {
		return nil, m.errListByKeys
	}
	out := make(map[model.DefinitionKey]*model.FeatureDefinition, len(keys))
	for _, k := range keys {
		if strings.TrimSpace(k.FeatureKey) == "" || k.Version < 1 {
			return nil, model.ErrFeatureVersionRequired
		}
		if row, ok := m.rows[k]; ok && !m.omitFromListByKeys[k] {
			v := row
			out[k] = &v
		}
	}
	return out, nil
}

func (m *fakeDefinitions) UpdateState(ctx context.Context, featureKey string, version,
	fromState, toState int32) (bool, error) {
	record(&m.calls, "definitions.UpdateState")
	if m.errUpdateState != nil {
		return false, m.errUpdateState
	}
	if !model.ValidFeatureStateTransition(fromState, toState) {
		return false, model.ErrFeatureStateTransition
	}
	key := model.DefinitionKey{FeatureKey: featureKey, Version: version}
	if m.lostUpdate != nil {
		m.lostUpdate(featureKey, version, "state")
	}
	row, ok := m.rows[key]
	if !ok || row.State != fromState {
		return false, nil
	}
	row.State = toState
	row.Mtime = model.NowUnix()
	m.rows[key] = row
	return true, nil
}

func (m *fakeDefinitions) UpdatePrivacy(ctx context.Context, featureKey string,
	version, fromLevel, toLevel int32) (bool, error) {
	record(&m.calls, "definitions.UpdatePrivacy")
	if m.errUpdatePrivacy != nil {
		return false, m.errUpdatePrivacy
	}
	if !model.ValidPrivacyLevel(fromLevel) || !model.ValidPrivacyLevel(toLevel) {
		return false, model.ErrPrivacyUnsetNotAllowed
	}
	if fromLevel == toLevel {
		return false, nil
	}
	key := model.DefinitionKey{FeatureKey: featureKey, Version: version}
	if m.lostUpdate != nil {
		m.lostUpdate(featureKey, version, "privacy_level")
	}
	row, ok := m.rows[key]
	if !ok || row.PrivacyLevel != fromLevel {
		return false, nil
	}
	row.PrivacyLevel = toLevel
	row.Mtime = model.NowUnix()
	m.rows[key] = row
	return true, nil
}

func (m *fakeDefinitions) CountByState(ctx context.Context, featureKey string,
	state int32) (int64, error) {
	record(&m.calls, "definitions.CountByState")
	if !model.ValidFeatureState(state) {
		return 0, model.ErrFeatureStateTransition
	}
	var n int64
	for _, d := range m.rows {
		if d.FeatureKey == featureKey && d.State == state {
			n++
		}
	}
	return n, nil
}

func (m *fakeDefinitions) sorted(keep func(*model.FeatureDefinition) bool) []*model.FeatureDefinition {
	out := make([]*model.FeatureDefinition, 0, len(m.rows))
	for _, r := range m.rows {
		v := r
		if keep(&v) {
			out = append(out, &v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FeatureKey != out[j].FeatureKey {
			return out[i].FeatureKey < out[j].FeatureKey
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// --- feature_active_version ---

type fakeActiveVersions struct {
	rows     map[string]model.ActiveVersion
	nextID   int64
	calls    []string
	switched []string // 每次成功 CAS 的 "key:from>to"
	listSeen [][]string

	errEnsure        error
	errFindOne       error
	errListByKeys    error
	errLockForUpdate error
	errSwitch        error
	// lostUpdate 在「FOR UPDATE 已读到行」与「条件更新比较」之间被调用。
	// 它只用来 *制造* 一次并发提交（真库里 FOR UPDATE 串行化失效、或跨连接读到旧快照的
	// 那一类竞态），让 CAS 分支可测；它不放宽任何校验，校验该失败照旧失败。
	lostUpdate func(featureKey string)
}

func (m *fakeActiveVersions) snapshot() tableState {
	cp := make(map[string]model.ActiveVersion, len(m.rows))
	for k, v := range m.rows {
		cp[k] = v
	}
	id := m.nextID
	return func() { m.rows, m.nextID = cp, id }
}

func (m *fakeActiveVersions) Ensure(ctx context.Context, session sqlx.Session,
	featureKey string) error {
	record(&m.calls, "activeVersions.Ensure")
	if strings.TrimSpace(featureKey) == "" {
		return model.ErrFeatureKeyRequired
	}
	if m.errEnsure != nil {
		return m.errEnsure
	}
	if _, ok := m.rows[featureKey]; ok {
		return nil // ON DUPLICATE KEY UPDATE mtime = mtime：不覆盖已有指针
	}
	m.nextID++
	m.rows[featureKey] = model.ActiveVersion{
		PointerID: m.nextID, FeatureKey: featureKey,
		ActiveVersion: model.NoActiveVersion, PreviousVersion: model.NoActiveVersion,
		Ctime: model.NowUnix(), Mtime: model.NowUnix(),
	}
	return nil
}

func (m *fakeActiveVersions) FindOne(ctx context.Context,
	featureKey string) (*model.ActiveVersion, error) {
	record(&m.calls, "activeVersions.FindOne")
	if m.errFindOne != nil {
		return nil, m.errFindOne
	}
	row, ok := m.rows[featureKey]
	if !ok {
		return nil, nil // 与真实实现一致：未建指针行不是错误
	}
	out := row
	return &out, nil
}

func (m *fakeActiveVersions) ListByKeys(ctx context.Context,
	featureKeys []string) (map[string]*model.ActiveVersion, error) {
	record(&m.calls, "activeVersions.ListByKeys")
	m.listSeen = append(m.listSeen, append([]string(nil), featureKeys...))
	if len(featureKeys) == 0 {
		return nil, nil
	}
	if len(featureKeys) > model.MaxBatchFeatures {
		return nil, model.ErrTooManyEntries
	}
	if m.errListByKeys != nil {
		return nil, m.errListByKeys
	}
	out := map[string]*model.ActiveVersion{}
	for _, k := range featureKeys {
		if row, ok := m.rows[k]; ok {
			v := row
			out[k] = &v
		}
	}
	return out, nil
}

func (m *fakeActiveVersions) LockForUpdate(ctx context.Context, session sqlx.Session,
	featureKey string) (*model.ActiveVersion, error) {
	record(&m.calls, "activeVersions.LockForUpdate")
	if session == nil {
		return nil, model.ErrTransactionRequired
	}
	if m.errLockForUpdate != nil {
		return nil, m.errLockForUpdate
	}
	row, ok := m.rows[featureKey]
	if !ok {
		return nil, model.ErrActiveVersionMissing
	}
	out := row
	return &out, nil
}

func (m *fakeActiveVersions) Switch(ctx context.Context, session sqlx.Session, featureKey string,
	expectFrom, toVersion int32, switchID int64) (bool, error) {
	record(&m.calls, "activeVersions.Switch")
	if session == nil {
		return false, model.ErrTransactionRequired
	}
	if toVersion < 1 {
		return false, model.ErrFeatureVersionRequired
	}
	if expectFrom == toVersion {
		return false, model.ErrVersionConflict
	}
	if m.errSwitch != nil {
		return false, m.errSwitch
	}
	if m.lostUpdate != nil {
		m.lostUpdate(featureKey)
	}
	row, ok := m.rows[featureKey]
	if !ok || row.ActiveVersion != expectFrom {
		return false, nil // 指针已被并发切走
	}
	row.ActiveVersion = toVersion
	if expectFrom != model.NoActiveVersion {
		row.PreviousVersion = expectFrom
	}
	row.LastSwitchID = switchID
	row.Mtime = model.NowUnix()
	m.rows[featureKey] = row
	m.switched = append(m.switched, featureKey+"|"+int32Text(expectFrom)+"→"+int32Text(toVersion))
	return true, nil
}

func (m *fakeActiveVersions) RetireActive(ctx context.Context, session sqlx.Session,
	featureKey string, expectActive int32, switchID int64) (bool, error) {
	record(&m.calls, "activeVersions.RetireActive")
	if session == nil {
		return false, model.ErrTransactionRequired
	}
	if expectActive < 1 {
		return false, model.ErrFeatureVersionRequired
	}
	if m.lostUpdate != nil {
		m.lostUpdate(featureKey)
	}
	row, ok := m.rows[featureKey]
	if !ok || row.ActiveVersion != expectActive {
		return false, nil
	}
	row.ActiveVersion = model.NoActiveVersion
	row.PreviousVersion = expectActive
	row.LastSwitchID = switchID
	row.Mtime = model.NowUnix()
	m.rows[featureKey] = row
	return true, nil
}

func int32Text(v int32) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --- feature_value ---

type fakeValues struct {
	rows      map[model.ValueKey]model.FeatureValue
	nextID    int64
	calls     []string
	entries   [][]model.ValueKey // 每次 FindEntries 的键，供「回源键集合」断言
	upserts   []*model.FeatureValue
	deletes   []int64
	eraseSee  []int32 // ListForErase 收到的 limit
	expireSee []int32 // ListExpiredForPurge 收到的 limit（PurgeExpired 是夹取而不是拒绝，必须能断言）
	// expireBeforeSeen 记录 ListExpiredForPurge 收到的截止时间：before<=0 时 logic
	// 用服务端当前时间补位，这一列是「补的是哪一刻」的唯一可观测点。
	expireBeforeSeen []int64
	// deleteShortfall 让「DELETE ... IN 的 RowsAffected 小于传进去的 ID 数」可测。
	// 真库里这是并发清理（另一轮 purge 或另一起擦除工单）先一步删掉了部分行的结果：
	// 行确实没了，但本次语句的影响行数更少，所以 logic 从 RowsAffected 得到的计数
	// 与它自己「选出来的行」不是同一个集合。删除计数是否如实，只能在这一档上判别。
	deleteShortfall int64
	// listByEntitySeen / listForEraseSeen 记录两条按主体读/擦的完整入参。
	// 隐私下限与导出上限在真 SQL 里是两条彼此独立的列条件（privacyClause /
	// privacyMaxClause），只记方法名无法判别「谁覆盖谁」，也无法证明上限确实下传了。
	listByEntitySeen []listByEntityCall
	listForEraseSeen []listForEraseCall

	// 反向引用：导出视图要按定义隐私级别与 ACTIVE 指针收窄（与真 SQL 的 JOIN 同构）。
	defs     *fakeDefinitions
	pointers *fakeActiveVersions

	errBatchUpsert      error
	errFindEntries      error
	errListByEntity     error
	errListForErase     error
	errDeleteByIDs      error
	errListExpiredPurge error
	errCountExpired     error
	errCountByVersion   error
}

// listByEntityCall 是一次 values.ListByEntity 的入参快照。
type listByEntityCall struct {
	entityScope int32
	entityID    string
	minPrivacy  int32
	maxPrivacy  int32
	pn, ps      int32
}

// listForEraseCall 是一次 values.ListForErase 的入参快照。
type listForEraseCall struct {
	entityScope int32
	entityID    string
	minPrivacy  int32
	limit       int32
}

func (m *fakeValues) snapshot() tableState {
	cp := make(map[model.ValueKey]model.FeatureValue, len(m.rows))
	for k, v := range m.rows {
		cp[k] = v
	}
	id := m.nextID
	return func() { m.rows, m.nextID = cp, id }
}

func (m *fakeValues) BatchUpsert(ctx context.Context, session sqlx.Session,
	rows []*model.FeatureValue, opts model.UpsertOptions) ([]model.UpsertOutcome, error) {
	record(&m.calls, "values.BatchUpsert")
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) > model.MaxBatchWriteRows {
		return nil, model.ErrTooManyRows
	}
	outcomes := make([]model.UpsertOutcome, len(rows))
	for i, row := range rows {
		outcomes[i].Key, outcomes[i].OK = row.Key(), true
	}
	writable := make([]*model.FeatureValue, 0, len(rows))
	seen := map[model.ValueKey]int{}
	for i, row := range rows {
		if err := validateRowShape(row); err != nil {
			outcomes[i].OK, outcomes[i].Err = false, err
			continue
		}
		if prev, dup := seen[row.Key()]; dup {
			outcomes[prev].OK, outcomes[prev].Err = false, model.ErrDuplicateRowInBatch
		}
		seen[row.Key()] = i
		writable = append(writable, row)
	}
	if m.errBatchUpsert != nil {
		return nil, m.errBatchUpsert
	}
	for _, row := range writable {
		k := row.Key()
		cur, exists := m.rows[k]
		if !exists {
			m.nextID++
			v := *row
			v.ValueID = m.nextID
			v.Ctime = model.NowUnix()
			v.Mtime = v.Ctime
			m.rows[k] = v
			m.upserts = append(m.upserts, row)
			continue
		}
		if !opts.AllowStaleOverwrite && row.EventTime < cur.EventTime {
			outcomes[seen[k]].OK, outcomes[seen[k]].Err = false, model.ErrStaleEventTime
			continue
		}
		if valueColumnsEqual(row, &cur) {
			continue // 幂等重放：同值重写计成功、不改动任何列（对应 RowsAffected=0 分支）
		}
		v := *row
		v.ValueID, v.Ctime = cur.ValueID, cur.Ctime
		v.Mtime = model.NowUnix()
		m.rows[k] = v
		m.upserts = append(m.upserts, row)
	}
	return outcomes, nil
}

func (m *fakeValues) FindOne(ctx context.Context,
	key model.ValueKey) (*model.FeatureValue, error) {
	record(&m.calls, "values.FindOne")
	row, ok := m.rows[key]
	if !ok {
		return nil, nil
	}
	out := row
	return &out, nil
}

func (m *fakeValues) FindEntries(ctx context.Context,
	keys []model.ValueKey) (map[model.ValueKey]*model.FeatureValue, error) {
	record(&m.calls, "values.FindEntries")
	m.entries = append(m.entries, append([]model.ValueKey(nil), keys...))
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > model.MaxBatchReadEntries {
		return nil, model.ErrTooManyEntries
	}
	if m.errFindEntries != nil {
		return nil, m.errFindEntries
	}
	out := make(map[model.ValueKey]*model.FeatureValue, len(keys))
	for _, k := range keys {
		if row, ok := m.rows[k]; ok {
			v := row
			out[k] = &v
		}
	}
	return out, nil
}

func (m *fakeValues) ListByEntity(ctx context.Context, entityScope int32, entityID string,
	minPrivacyLevel, maxPrivacyLevel int32, pn, ps int32) ([]*model.FeatureValue, int64, error) {
	record(&m.calls, "values.ListByEntity")
	m.listByEntitySeen = append(m.listByEntitySeen, listByEntityCall{
		entityScope: entityScope, entityID: entityID, minPrivacy: minPrivacyLevel,
		maxPrivacy: maxPrivacyLevel, pn: pn, ps: ps,
	})
	if !model.ValidEntityScope(entityScope) {
		return nil, 0, model.ErrEntityScopeRequired
	}
	if !model.ValidEntityID(entityScope, entityID) {
		return nil, 0, model.ErrEntityIDInvalid
	}
	if err := model.ValidatePageSize(pn, ps); err != nil {
		return nil, 0, err
	}
	if m.errListByEntity != nil {
		return nil, 0, m.errListByEntity
	}
	all := m.entityRows(entityScope, entityID, minPrivacyLevel, maxPrivacyLevel, true)
	total := int64(len(all))
	if total == 0 {
		return nil, 0, nil
	}
	start := int(pn-1) * int(ps)
	if start >= len(all) {
		return nil, total, nil
	}
	return all[start:min(start+int(ps), len(all))], total, nil
}

func (m *fakeValues) ListForErase(ctx context.Context, entityScope int32, entityID string,
	minPrivacyLevel, limit int32) ([]*model.FeatureValue, error) {
	record(&m.calls, "values.ListForErase")
	m.eraseSee = append(m.eraseSee, limit)
	m.listForEraseSeen = append(m.listForEraseSeen, listForEraseCall{
		entityScope: entityScope, entityID: entityID, minPrivacy: minPrivacyLevel, limit: limit,
	})
	if !model.ValidEntityScope(entityScope) {
		return nil, model.ErrEntityScopeRequired
	}
	if !model.ValidEntityID(entityScope, entityID) {
		return nil, model.ErrEntityIDInvalid
	}
	if limit <= 0 || limit > model.MaxPurgeRows {
		return nil, model.ErrLimitTooLarge
	}
	if m.errListForErase != nil {
		return nil, m.errListForErase
	}
	all := m.entityRows(entityScope, entityID, minPrivacyLevel, 0, false)
	if int32(len(all)) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (m *fakeValues) CountByEntity(ctx context.Context, entityScope int32, entityID string,
	minPrivacyLevel int32) (int64, error) {
	record(&m.calls, "values.CountByEntity")
	return int64(len(m.entityRows(entityScope, entityID, minPrivacyLevel, 0, true))), nil
}

func (m *fakeValues) DeleteByEntity(ctx context.Context, entityScope int32, entityID string,
	minPrivacyLevel, limit int32) (int64, error) {
	record(&m.calls, "values.DeleteByEntity")
	rows := m.entityRows(entityScope, entityID, minPrivacyLevel, 0, false)
	var n int64
	for _, r := range rows {
		if limit > 0 && n >= int64(limit) {
			break
		}
		delete(m.rows, r.Key())
		n++
	}
	return n, nil
}

func (m *fakeValues) SelectExpiredIDs(ctx context.Context, before int64,
	limit int32) ([]int64, error) {
	record(&m.calls, "values.SelectExpiredIDs")
	rows := m.expiredRows(before, limit)
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ValueID)
	}
	return ids, nil
}

func (m *fakeValues) ListExpiredForPurge(ctx context.Context, before int64,
	limit int32) ([]*model.FeatureValue, error) {
	record(&m.calls, "values.ListExpiredForPurge")
	m.expireSee = append(m.expireSee, limit)
	m.expireBeforeSeen = append(m.expireBeforeSeen, before)
	if limit <= 0 || limit > model.MaxPurgeRows {
		return nil, model.ErrLimitTooLarge
	}
	if m.errListExpiredPurge != nil {
		return nil, m.errListExpiredPurge
	}
	return m.expiredRows(before, limit), nil
}

func (m *fakeValues) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	record(&m.calls, "values.DeleteByIDs")
	m.deletes = append(m.deletes, ids...)
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > model.MaxPurgeRows {
		return 0, model.ErrLimitTooLarge
	}
	if m.errDeleteByIDs != nil {
		return 0, m.errDeleteByIDs
	}
	want := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	var n int64
	for k, row := range m.rows {
		if _, ok := want[row.ValueID]; ok {
			delete(m.rows, k)
			n++
		}
	}
	if n > m.deleteShortfall {
		n -= m.deleteShortfall
	} else {
		n = 0
	}
	return n, nil
}

func (m *fakeValues) CountExpired(ctx context.Context, before int64) (int64, error) {
	record(&m.calls, "values.CountExpired")
	if m.errCountExpired != nil {
		return 0, m.errCountExpired
	}
	return int64(len(m.expiredRows(before, 0))), nil
}

func (m *fakeValues) CountByVersion(ctx context.Context, featureKey string,
	version int32) (int64, error) {
	record(&m.calls, "values.CountByVersion")
	if m.errCountByVersion != nil {
		return 0, m.errCountByVersion
	}
	var n int64
	for _, r := range m.rows {
		if r.FeatureKey == featureKey && r.Version == version {
			n++
		}
	}
	return n, nil
}

func (m *fakeValues) ScanKeys(ctx context.Context, featureKey string, version int32,
	afterEntityID string, limit int32) ([]model.ValueKey, error) {
	record(&m.calls, "values.ScanKeys")
	var out []model.ValueKey
	for _, k := range m.sortedKeys() {
		if k.FeatureKey != featureKey || k.Version != version || k.EntityID <= afterEntityID {
			continue
		}
		if limit > 0 && int32(len(out)) >= limit {
			break
		}
		out = append(out, k)
	}
	return out, nil
}

func (m *fakeValues) DeleteByVersion(ctx context.Context, featureKey string, version,
	limit int32) (int64, error) {
	record(&m.calls, "values.DeleteByVersion")
	var n int64
	for _, k := range m.sortedKeys() {
		if k.FeatureKey != featureKey || k.Version != version {
			continue
		}
		if limit > 0 && n >= int64(limit) {
			break
		}
		delete(m.rows, k)
		n++
	}
	return n, nil
}

// entityRows 取某主体的行。
//
// onlyActive=true 对应导出视图的 SQL：JOIN feature_definition（隐私级别取自定义列）
// + JOIN feature_active_version(active_version = v.version) + d.state = ACTIVE。
// onlyActive=false 对应擦除路径：只 JOIN 定义，历史版本与非 ACTIVE 状态的残值都要删。
// 两种情况下「定义不存在」的行都不可见（内连接）。
func (m *fakeValues) entityRows(scope int32, entityID string, minPrivacy, maxPrivacy int32,
	onlyActive bool) []*model.FeatureValue {
	var out []*model.FeatureValue
	for _, k := range m.sortedKeys() {
		if k.EntityScope != scope || k.EntityID != entityID {
			continue
		}
		row := m.rows[k]
		def, ok := m.defs.rows[model.DefinitionKey{FeatureKey: k.FeatureKey, Version: k.Version}]
		if !ok {
			continue
		}
		if minPrivacy > 0 && def.PrivacyLevel < minPrivacy {
			continue
		}
		if maxPrivacy > 0 && def.PrivacyLevel > maxPrivacy {
			continue
		}
		if onlyActive {
			if def.State != model.FeatureStateActive {
				continue
			}
			ptr, has := m.pointers.rows[k.FeatureKey]
			if !has || ptr.ActiveVersion != k.Version {
				continue
			}
		}
		v := row
		out = append(out, &v)
	}
	if onlyActive {
		sort.Slice(out, func(i, j int) bool {
			if out[i].FeatureKey != out[j].FeatureKey {
				return out[i].FeatureKey < out[j].FeatureKey
			}
			return out[i].Version < out[j].Version
		})
		return out
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ValueID < out[j].ValueID })
	return out
}

func (m *fakeValues) expiredRows(before int64, limit int32) []*model.FeatureValue {
	if before <= 0 {
		before = model.NowUnix()
	}
	var out []*model.FeatureValue
	for _, k := range m.sortedKeys() {
		r := m.rows[k]
		if r.ExpireAt <= 0 || r.ExpireAt >= before {
			continue
		}
		v := r
		out = append(out, &v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpireAt != out[j].ExpireAt {
			return out[i].ExpireAt < out[j].ExpireAt
		}
		return out[i].ValueID < out[j].ValueID
	})
	if limit > 0 && int32(len(out)) > limit {
		out = out[:limit]
	}
	return out
}

func (m *fakeValues) sortedKeys() []model.ValueKey {
	out := make([]model.ValueKey, 0, len(m.rows))
	for k := range m.rows {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.FeatureKey != b.FeatureKey {
			return a.FeatureKey < b.FeatureKey
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.EntityScope != b.EntityScope {
			return a.EntityScope < b.EntityScope
		}
		return a.EntityID < b.EntityID
	})
	return out
}

// validateRowShape 与 model.validateFeatureValueRow 同一条纪律（那个函数未导出，
// 这里逐项对齐，避免替身比真 SQL 宽松）。
func validateRowShape(row *model.FeatureValue) error {
	if row == nil {
		return model.ErrMalformedValue
	}
	switch {
	case strings.TrimSpace(row.FeatureKey) == "":
		return model.ErrFeatureKeyRequired
	case row.Version < 1:
		return model.ErrFeatureVersionRequired
	case !model.ValidEntityScope(row.EntityScope):
		return model.ErrEntityScopeRequired
	case !model.ValidEntityID(row.EntityScope, row.EntityID):
		return model.ErrEntityIDInvalid
	}
	if len(row.StringValue) > model.MaxStringValueLen {
		return model.ErrMalformedValue
	}
	if len(row.ListValues) > model.MaxListValueBytes {
		return model.ErrDimensionExceeded
	}
	if row.EventTime <= 0 {
		return model.ErrMalformedValue
	}
	if row.ExpireAt <= 0 {
		return model.ErrTTLRequired
	}
	if len(row.SourceMetricKey) > model.MaxSourceMetricKeyLen {
		return model.ErrMalformedValue
	}
	return nil
}

func valueColumnsEqual(a, b *model.FeatureValue) bool {
	return a.ValueType == b.ValueType && a.Int64Value == b.Int64Value &&
		a.DoubleValue == b.DoubleValue && a.BoolValue == b.BoolValue &&
		a.StringValue == b.StringValue && a.ListValues == b.ListValues &&
		a.EventTime == b.EventTime && a.ExpireAt == b.ExpireAt
}

// --- feature_version_switch ---

type fakeSwitches struct {
	rows     map[int64]model.VersionSwitch
	nextID   int64
	calls    []string
	appended []*model.VersionSwitch
	// listSeen 记录每次 List 收到的过滤条件：审计列表是否把 key/since/分页下传给 SQL、
	// 有没有顺手把 switch_type 也带上（契约里根本没有这一列），全靠它判别。
	listSeen []model.VersionSwitchFilter

	errAppend  error
	errFindOne error
	errList    error
}

func (m *fakeSwitches) snapshot() tableState {
	cp := make(map[int64]model.VersionSwitch, len(m.rows))
	for k, v := range m.rows {
		cp[k] = v
	}
	id := m.nextID
	return func() { m.rows, m.nextID = cp, id }
}

func (m *fakeSwitches) Append(ctx context.Context, session sqlx.Session,
	rec *model.VersionSwitch) (int64, error) {
	record(&m.calls, "switches.Append")
	if rec == nil || strings.TrimSpace(rec.FeatureKey) == "" {
		return 0, model.ErrFeatureKeyRequired
	}
	if !model.ValidSwitchType(rec.SwitchType) {
		return 0, model.ErrFeatureStateTransition
	}
	if strings.TrimSpace(rec.Operator) == "" {
		return 0, model.ErrOperatorRequired
	}
	if strings.TrimSpace(rec.RequestID) == "" {
		return 0, model.ErrRequestIdRequired
	}
	if m.errAppend != nil {
		return 0, m.errAppend
	}
	// uniq_request_switch：同一 request_id + 同一审计类型只允许落一条。
	for _, r := range m.rows {
		if r.RequestID == rec.RequestID && r.SwitchType == rec.SwitchType {
			return 0, model.ErrRequestIdReused
		}
	}
	if rec.Ctime == 0 {
		rec.Ctime = model.NowUnix()
	}
	m.nextID++
	rec.SwitchID = m.nextID
	row := *rec
	m.rows[m.nextID] = row
	m.appended = append(m.appended, &row)
	return m.nextID, nil
}

func (m *fakeSwitches) FindOne(ctx context.Context,
	switchID int64) (*model.VersionSwitch, error) {
	record(&m.calls, "switches.FindOne")
	if m.errFindOne != nil {
		return nil, m.errFindOne
	}
	if switchID <= 0 {
		return nil, model.ErrLimitTooLarge
	}
	row, ok := m.rows[switchID]
	if !ok {
		return nil, model.ErrSwitchNotFound
	}
	out := row
	return &out, nil
}

func (m *fakeSwitches) List(ctx context.Context,
	f model.VersionSwitchFilter) ([]*model.VersionSwitch, int64, error) {
	record(&m.calls, "switches.List")
	m.listSeen = append(m.listSeen, f)
	if m.errList != nil {
		return nil, 0, m.errList
	}
	if f.SwitchType != "" && !model.ValidSwitchType(f.SwitchType) {
		return nil, 0, model.ErrFeatureStateTransition
	}
	all := m.filtered(f)
	total := int64(len(all))
	if total == 0 {
		return nil, 0, nil
	}
	pn, ps := f.Pn, f.Ps
	if pn < 1 {
		pn = 1
	}
	if ps <= 0 || ps > model.MaxListPageSize {
		ps = model.MaxListPageSize
	}
	start := int(pn-1) * int(ps)
	if start >= len(all) {
		return nil, total, nil
	}
	return all[start:min(start+int(ps), len(all))], total, nil
}

func (m *fakeSwitches) LatestByKey(ctx context.Context,
	featureKey string) (*model.VersionSwitch, error) {
	record(&m.calls, "switches.LatestByKey")
	rows := m.sortedDesc(func(r *model.VersionSwitch) bool { return r.FeatureKey == featureKey })
	if len(rows) == 0 {
		return nil, model.ErrSwitchNotFound
	}
	return rows[0], nil
}

func (m *fakeSwitches) filtered(f model.VersionSwitchFilter) []*model.VersionSwitch {
	return m.sortedDesc(func(r *model.VersionSwitch) bool {
		if key := strings.TrimSpace(f.FeatureKey); key != "" && r.FeatureKey != key {
			return false
		}
		if f.SwitchType != "" && r.SwitchType != f.SwitchType {
			return false
		}
		if f.Since > 0 && r.Ctime < f.Since {
			return false
		}
		if f.Until > 0 && r.Ctime >= f.Until {
			return false
		}
		return true
	})
}

func (m *fakeSwitches) sortedDesc(keep func(*model.VersionSwitch) bool) []*model.VersionSwitch {
	out := make([]*model.VersionSwitch, 0, len(m.rows))
	for _, r := range m.rows {
		v := r
		if keep(&v) {
			out = append(out, &v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SwitchID > out[j].SwitchID })
	return out
}

// --- feature_backfill_job ---

type fakeBackfillJobs struct {
	rows   map[int64]model.BackfillJob
	nextID int64
	calls  []string
	// listSeen 记录每次 List 收到的完整过滤条件。分页与状态过滤是否真的下传到 SQL、
	// 还是被 logic 在本地重新切了一遍，只有这一列能判别。
	listSeen []model.BackfillJobFilter

	errInsert          error
	errFindOne         error
	errFindByRequest   error
	errList            error
	errCountUnfinished error
}

func (m *fakeBackfillJobs) snapshot() tableState {
	cp := make(map[int64]model.BackfillJob, len(m.rows))
	for k, v := range m.rows {
		cp[k] = v
	}
	id := m.nextID
	return func() { m.rows, m.nextID = cp, id }
}

func (m *fakeBackfillJobs) Insert(ctx context.Context, j *model.BackfillJob) (int64, error) {
	record(&m.calls, "backfills.Insert")
	if err := model.ValidateBackfillJob(j); err != nil {
		return 0, err
	}
	if m.errInsert != nil {
		return 0, m.errInsert
	}
	for _, r := range m.rows {
		if r.RequestID == j.RequestID {
			return 0, model.ErrJobExists
		}
	}
	m.nextID++
	j.JobID = m.nextID
	// 与 defaultBackfillJobModel.Insert 同一侧写：状态缺省、ctime 补当前时刻、mtime 跟随 ctime
	// 都发生在入参对象上，所以 logic 拿到的「入参指针」与「落库行」必须同形，
	// 否则 SubmitBackfillJob 的响应一致性测试钉住的是替身产物而不是 SQL 行为。
	if j.State == model.BackfillStateUnspecified {
		j.State = model.BackfillStatePending
	}
	if j.Ctime == 0 {
		j.Ctime = model.NowUnix()
	}
	j.Mtime = j.Ctime
	row := *j
	m.rows[m.nextID] = row
	return m.nextID, nil
}

func (m *fakeBackfillJobs) FindOne(ctx context.Context, jobID int64) (*model.BackfillJob, error) {
	record(&m.calls, "backfills.FindOne")
	if m.errFindOne != nil {
		return nil, m.errFindOne
	}
	row, ok := m.rows[jobID]
	if !ok {
		return nil, model.ErrJobNotFound
	}
	out := row
	return &out, nil
}

func (m *fakeBackfillJobs) FindByRequestID(ctx context.Context,
	requestID string) (*model.BackfillJob, error) {
	record(&m.calls, "backfills.FindByRequestID")
	if strings.TrimSpace(requestID) == "" {
		return nil, model.ErrRequestIdRequired
	}
	if m.errFindByRequest != nil {
		return nil, m.errFindByRequest
	}
	for _, r := range m.rows {
		if r.RequestID == requestID {
			v := r
			return &v, nil
		}
	}
	return nil, nil
}

func (m *fakeBackfillJobs) List(ctx context.Context,
	f model.BackfillJobFilter) ([]*model.BackfillJob, int64, error) {
	record(&m.calls, "backfills.List")
	m.listSeen = append(m.listSeen, f)
	if m.errList != nil {
		return nil, 0, m.errList
	}
	if f.State != model.BackfillStateUnspecified && !model.ValidBackfillState(f.State) {
		return nil, 0, model.ErrJobStateInvalid
	}
	all := m.filtered(f)
	total := int64(len(all))
	if total == 0 {
		return nil, 0, nil
	}
	pn, ps := f.Pn, f.Ps
	if pn < 1 {
		pn = 1
	}
	if ps <= 0 || ps > model.MaxListPageSize {
		ps = model.MaxListPageSize
	}
	start := int(pn-1) * int(ps)
	if start >= len(all) {
		return nil, total, nil
	}
	return all[start:min(start+int(ps), len(all))], total, nil
}

func (m *fakeBackfillJobs) Claim(ctx context.Context, jobID int64, owner string,
	leaseSeconds int64) (bool, error) {
	record(&m.calls, "backfills.Claim")
	return m.acquire(jobID, owner, leaseSeconds)
}

func (m *fakeBackfillJobs) ListClaimable(ctx context.Context,
	limit int32) ([]*model.BackfillJob, error) {
	record(&m.calls, "backfills.ListClaimable")
	now := model.NowUnix()
	var out []*model.BackfillJob
	for _, r := range m.sortedAsc() {
		if r.State == model.BackfillStatePending ||
			(r.State == model.BackfillStateRunning && r.LeaseExpireAt <= now) {
			v := *r
			out = append(out, &v)
			if limit > 0 && int32(len(out)) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *fakeBackfillJobs) AddProgress(ctx context.Context, jobID int64, owner string,
	done, failed, cursorEntityID int64, cursorEntityStr string,
	leaseSeconds int64) (bool, error) {
	record(&m.calls, "backfills.AddProgress")
	if done < 0 || failed < 0 {
		return false, model.ErrJobStateInvalid
	}
	row, ok := m.rows[jobID]
	if !ok {
		return false, model.ErrJobNotFound
	}
	if row.State != model.BackfillStateRunning || row.LeaseOwner != owner ||
		row.LeaseExpireAt <= model.NowUnix() {
		return false, nil // 租约已被接管
	}
	if cursorEntityID < row.CursorEntityID {
		return false, nil // 游标必须单调不回退
	}
	row.EntitiesDone += done
	row.EntitiesFailed += failed
	row.CursorEntityID, row.CursorEntityStr = cursorEntityID, cursorEntityStr
	row.ProgressSeq++
	row.LeaseExpireAt = model.NowUnix() + leaseSeconds
	m.rows[jobID] = row
	return true, nil
}

func (m *fakeBackfillJobs) Finish(ctx context.Context, jobID int64, owner, toState,
	lastError string) (bool, error) {
	record(&m.calls, "backfills.Finish")
	row, ok := m.rows[jobID]
	if !ok {
		return false, model.ErrJobNotFound
	}
	state, err := backfillStateOf(toState)
	if err != nil {
		return false, err
	}
	if row.IsTerminal() || !model.ValidBackfillTransition(row.State, state) ||
		row.LeaseOwner != owner || row.LeaseExpireAt <= model.NowUnix() {
		return false, nil
	}
	row.State, row.LastError, row.FinishedAt = state, lastError, model.NowUnix()
	m.rows[jobID] = row
	return true, nil
}

func (m *fakeBackfillJobs) Cancel(ctx context.Context, jobID int64, operator,
	reason string) (bool, error) {
	record(&m.calls, "backfills.Cancel")
	row, ok := m.rows[jobID]
	if !ok {
		return false, model.ErrJobNotFound
	}
	if row.IsTerminal() {
		return false, model.ErrJobAlreadyTerminal
	}
	row.State, row.FinishedAt, row.LastError = model.BackfillStateCancelled, model.NowUnix(), reason
	m.rows[jobID] = row
	return true, nil
}

func (m *fakeBackfillJobs) CountUnfinished(ctx context.Context, featureKey string,
	version int32) (int64, error) {
	record(&m.calls, "backfills.CountUnfinished")
	if m.errCountUnfinished != nil {
		return 0, m.errCountUnfinished
	}
	return m.count(featureKey, version, func(s int32) bool {
		return s == model.BackfillStatePending || s == model.BackfillStateRunning
	}), nil
}

func (m *fakeBackfillJobs) CountSucceeded(ctx context.Context, featureKey string,
	version int32) (int64, error) {
	record(&m.calls, "backfills.CountSucceeded")
	return m.count(featureKey, version,
		func(s int32) bool { return s == model.BackfillStateSucceeded }), nil
}

// acquire 实现「PENDING 可认领 / RUNNING 但租约过期可接管 / 持有者可续约」。
func (m *fakeBackfillJobs) acquire(jobID int64, owner string, leaseSeconds int64) (bool, error) {
	if strings.TrimSpace(owner) == "" {
		return false, model.ErrOperatorRequired
	}
	row, ok := m.rows[jobID]
	if !ok {
		return false, model.ErrJobNotFound
	}
	now := model.NowUnix()
	switch {
	case row.State == model.BackfillStatePending:
	case row.State == model.BackfillStateRunning && row.LeaseExpireAt <= now:
	case row.State == model.BackfillStateRunning && row.LeaseOwner == owner:
	default:
		return false, nil
	}
	row.State = model.BackfillStateRunning
	row.LeaseOwner = owner
	row.LeaseExpireAt = now + leaseSeconds
	if row.StartedAt == 0 {
		row.StartedAt = now
	}
	row.Mtime = now
	m.rows[jobID] = row
	return true, nil
}

func (m *fakeBackfillJobs) count(featureKey string, version int32,
	included func(int32) bool) int64 {
	var n int64
	for _, r := range m.rows {
		if r.FeatureKey == featureKey && r.Version == version && included(r.State) {
			n++
		}
	}
	return n
}

func (m *fakeBackfillJobs) filtered(f model.BackfillJobFilter) []*model.BackfillJob {
	out := m.sortedDesc()
	filtered := make([]*model.BackfillJob, 0, len(out))
	for _, r := range out {
		if f.FeatureKey != "" && r.FeatureKey != f.FeatureKey {
			continue
		}
		if f.State != model.BackfillStateUnspecified && r.State != f.State {
			continue
		}
		if f.Since > 0 && r.Ctime < f.Since {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

func (m *fakeBackfillJobs) sortedDesc() []*model.BackfillJob {
	out := m.sortedAsc()
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (m *fakeBackfillJobs) sortedAsc() []*model.BackfillJob {
	out := make([]*model.BackfillJob, 0, len(m.rows))
	for _, r := range m.rows {
		v := r
		out = append(out, &v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out
}

// backfillStateOf 把 Finish 的字符串入参转成枚举（model 内部同名函数未导出）。
func backfillStateOf(name string) (int32, error) {
	switch name {
	case "running":
		return model.BackfillStateRunning, nil
	case "succeeded":
		return model.BackfillStateSucceeded, nil
	case "failed":
		return model.BackfillStateFailed, nil
	case "cancelled":
		return model.BackfillStateCancelled, nil
	default:
		return 0, model.ErrJobStateInvalid
	}
}

// --- feature_write_receipt ---

type fakeReceipts struct {
	rows   map[string]model.WriteReceipt // key: requestID + 分隔符 + opType
	calls  []string
	begun  []*model.WriteReceipt
	done   []string
	failed []string

	errBegin      error
	errMarkDone   error
	errMarkFailed error
	errFind       error
}

func (m *fakeReceipts) snapshot() tableState {
	cp := make(map[string]model.WriteReceipt, len(m.rows))
	for k, v := range m.rows {
		cp[k] = v
	}
	return func() { m.rows = cp }
}

func receiptKey(requestID, opType string) string { return requestID + "\x1f" + opType }

// Begin 复刻真实状态机：新请求取得执行权；已完成回放；同键不同内容冲突；
// 执行中且租约存活则拒绝二次执行；failed 或租约过期则接管。
func (m *fakeReceipts) Begin(ctx context.Context, r *model.WriteReceipt,
	leaseSeconds int64) (model.ReceiptBeginResult, error) {
	record(&m.calls, "receipts.Begin")
	if err := model.ValidateWriteReceipt(r); err != nil {
		return model.ReceiptBeginResult{}, err
	}
	if strings.TrimSpace(r.LeaseOwner) == "" {
		return model.ReceiptBeginResult{}, model.ErrOperatorRequired
	}
	if leaseSeconds <= 0 {
		leaseSeconds = model.MaxReceiptLeaseSeconds / 5
	}
	if leaseSeconds > model.MaxReceiptLeaseSeconds {
		leaseSeconds = model.MaxReceiptLeaseSeconds
	}
	if m.errBegin != nil {
		return model.ReceiptBeginResult{}, m.errBegin
	}
	now := model.NowUnix()
	digest := r.RequestDigestOf()
	key := receiptKey(r.RequestID, r.OpType)
	if existing, ok := m.rows[key]; ok {
		if existing.RequestDigest != digest {
			return model.ReceiptBeginResult{}, model.ErrRequestIdReused
		}
		taken := existing
		switch existing.State {
		case model.ReceiptStateDone:
			return model.ReceiptBeginResult{Receipt: &taken}, nil
		case model.ReceiptStateFailed:
			taken.State = model.ReceiptStateInProgress
			taken.LeaseOwner = r.LeaseOwner
			taken.LeaseExpireAt = now + leaseSeconds
			taken.ResultJSON, taken.ResultDigest, taken.DetailKept = "", "", 0
			taken.AffectedRows, taken.ErrorCode, taken.FinishedAt = 0, "", 0
			m.rows[key] = taken
			return model.ReceiptBeginResult{Receipt: &taken, Execute: true}, nil
		case model.ReceiptStateInProgress:
			if taken.LeaseExpireAt > now {
				return model.ReceiptBeginResult{Receipt: &taken}, model.ErrReceiptInProgress
			}
			taken.LeaseOwner = r.LeaseOwner
			taken.LeaseExpireAt = now + leaseSeconds
			m.rows[key] = taken
			return model.ReceiptBeginResult{Receipt: &taken, Execute: true}, nil
		default:
			return model.ReceiptBeginResult{}, model.ErrReceiptStateInvalid
		}
	}
	fresh := *r
	fresh.RequestDigest = digest
	fresh.DigestVer = model.ReceiptDigestVersion
	fresh.State = model.ReceiptStateInProgress
	fresh.LeaseExpireAt = now + leaseSeconds
	fresh.Ctime, fresh.Mtime, fresh.FinishedAt = now, now, 0
	m.rows[key] = fresh
	m.begun = append(m.begun, &fresh)
	return model.ReceiptBeginResult{Receipt: &fresh, Execute: true}, nil
}

func (m *fakeReceipts) MarkDone(ctx context.Context, requestID, opType, leaseOwner string,
	snap model.ReceiptSnapshot) (bool, error) {
	record(&m.calls, "receipts.MarkDone")
	if m.errMarkDone != nil {
		return false, m.errMarkDone
	}
	if err := checkReceiptKey(requestID, opType, leaseOwner); err != nil {
		return false, err
	}
	if len(snap.ResultJSON) > model.MaxReceiptResultBytes {
		return false, model.ErrReceiptResultTooLarge
	}
	ok := m.guardedWrite(requestID, opType, leaseOwner, func(row *model.WriteReceipt) {
		row.State = model.ReceiptStateDone
		row.ResultJSON = snap.ResultJSON
		row.ResultDigest = snap.ResultDigest
		row.AffectedRows = snap.AffectedRows
		row.DetailKept = 0
		if snap.DetailKept {
			row.DetailKept = 1
		}
		row.ErrorCode = ""
	})
	if ok {
		m.done = append(m.done, requestID+"/"+opType)
	}
	return ok, nil
}

func (m *fakeReceipts) MarkFailed(ctx context.Context, requestID, opType, leaseOwner,
	errorCode string) (bool, error) {
	record(&m.calls, "receipts.MarkFailed")
	if m.errMarkFailed != nil {
		return false, m.errMarkFailed
	}
	if err := checkReceiptKey(requestID, opType, leaseOwner); err != nil {
		return false, err
	}
	code := strings.TrimSpace(errorCode)
	if code == "" {
		return false, model.ErrReceiptOpRequired
	}
	ok := m.guardedWrite(requestID, opType, leaseOwner, func(row *model.WriteReceipt) {
		row.State = model.ReceiptStateFailed
		row.ErrorCode = code
		row.LeaseExpireAt = model.NowUnix()
	})
	if ok {
		m.failed = append(m.failed, requestID+"/"+opType+":"+code)
	}
	return ok, nil
}

func checkReceiptKey(requestID, opType, leaseOwner string) error {
	if strings.TrimSpace(requestID) == "" || strings.TrimSpace(opType) == "" ||
		strings.TrimSpace(leaseOwner) == "" {
		return model.ErrReceiptOpRequired
	}
	return nil
}

// guardedWrite 只在「in_progress + 自己是持有者 + 租约未过期」时改写，
// 与真 SQL 的 WHERE 条件一致；返回 false 就是执行权已被接管。
func (m *fakeReceipts) guardedWrite(requestID, opType, leaseOwner string,
	mutate func(*model.WriteReceipt)) bool {
	key := receiptKey(requestID, opType)
	row, ok := m.rows[key]
	if !ok || row.State != model.ReceiptStateInProgress || row.LeaseOwner != leaseOwner ||
		row.LeaseExpireAt <= model.NowUnix() {
		return false
	}
	now := model.NowUnix()
	row.FinishedAt, row.Mtime = now, now
	mutate(&row)
	m.rows[key] = row
	return true
}

func (m *fakeReceipts) Find(ctx context.Context, requestID,
	opType string) (*model.WriteReceipt, error) {
	record(&m.calls, "receipts.Find")
	if m.errFind != nil {
		return nil, m.errFind
	}
	row, ok := m.rows[receiptKey(requestID, opType)]
	if !ok {
		return nil, nil
	}
	out := row
	return &out, nil
}

func (m *fakeReceipts) CountStuck(ctx context.Context, before int64) (int64, error) {
	record(&m.calls, "receipts.CountStuck")
	var n int64
	for _, r := range m.rows {
		if r.State == model.ReceiptStateInProgress && r.LeaseExpireAt < before {
			n++
		}
	}
	return n, nil
}

func (m *fakeReceipts) SelectAgedIDs(ctx context.Context, before int64,
	limit int32) ([]int64, error) {
	record(&m.calls, "receipts.SelectAgedIDs")
	var ids []int64
	for _, r := range m.rows {
		if model.IsReceiptTerminal(r.State) && r.Mtime < before {
			ids = append(ids, r.ReceiptID)
		}
	}
	if limit > 0 && int32(len(ids)) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func (m *fakeReceipts) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	record(&m.calls, "receipts.DeleteByIDs")
	return int64(len(ids)), nil
}

// --- 断言小工具 ---

// receiptRow 取出回执行内容，断言「收尾后状态与快照确实落库」。
func (f *fixture) receiptRow(requestID, opType string) (model.WriteReceipt, bool) {
	v, ok := f.receipts.rows[receiptKey(requestID, opType)]
	return v, ok
}

// fallbackCacheKeys 把替身记下的回源键换成 Redis 主读键集合：
// 不建真缓存也能验证「回源读了哪些键」与「写后要 DEL 哪些键」是否按方案对齐。
func fallbackCacheKeys(seen [][]model.ValueKey) map[string]struct{} {
	out := map[string]struct{}{}
	for _, chunk := range seen {
		for _, k := range chunk {
			out[k.CacheKey()] = struct{}{}
		}
	}
	return out
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
