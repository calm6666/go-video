package logic

// 本文件是 logic 包的测试替身。它存在的前提是：本服务的可测性设计
// （model 全部是接口、事务入口只有 sqlx.SqlConn.TransactCtx、缓存只有 CacheKV 三方法），
// 于是「乐观锁 CAS / 幂等回放 / 事务回滚不留幽灵行 / 缓存键空间边界」这些真正的风险点
// 都能在**不连 MySQL、不起 gRPC、不依赖 Redis、不 sleep** 的情况下被证明。
//
// 关键手法：
//   - fakeDB 一次实现全部 8 个 model 接口（读写都走内存 map），写路径复刻真 model 的
//     唯一键与 RowsAffected 语义（重复 (config_id,version) → ErrVersionExists，
//     CAS 条件不满足 → false，唯一键冲突 → ErrConfigExists/ErrTopicSlugConflict…）。
//   - 读写字段值一律**深拷贝**（这些结构体全是值类型），因此 logic 若改写返回出去的指针，
//     库里的行不会跟着变——「回滚不改写历史」才是可证的。
//   - fakeConn.TransactCtx 先快照、失败即整库还原，等价于 MySQL 的事务回滚，
//     于是「CAS 输了不能留下版本行」可断言。
//   - 没实现的组合一律 panic（不是返回 nil），避免用例悄悄走到假成功。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	auditrpc "go-video/services/audit/rpc"
	"go-video/services/ops-config/internal/config"
	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"google.golang.org/grpc"
)

// fakeDB 是 8 张表的内存实现。
type fakeDB struct {
	items      map[int64]*model.ConfigItem
	versions   map[int64]*model.ConfigVersion
	rules      map[int64]*model.RolloutRule
	topics     map[int64]*model.Topic
	topicItems map[int64]*model.TopicItem
	slots      map[int64]*model.RecommendSlot
	slotItems  map[int64]*model.SlotItem
	switches   map[int64]*model.ClientSwitch

	next int64

	// 注入点：模拟「别人抢先推进了指针」这类只在并发下出现的分支。
	failAdvanceRelease bool
	// 注入点：让版本行写入返回指定错误（真库里是并发撞 uniq_config_version ⇒ ErrVersionExists，
	// 内存版没有 AUTO_INCREMENT 竞争，只能显式注入，否则 logic 的转换分支永远走不到）。
	conflictOnInsert error
	// 注入点：让事务内某一步失败，验证整事务回滚。
	failInTx string // "insert_version" / "upsert_rule" / "set_state"
	txCalls  int

	// readErrs 注入点：让某个**读**方法返回错误，用来证明「依赖故障原样上抛、
	// 不退化成 Found=false 或空列表这种别的语义」。key 与 calls 里的刻度同名。
	// 注意：必须在 seeding 之后再设置——seeding 走的是写方法刻度，两者不冲突。
	readErrs map[string]error

	calls map[string]int
}

// failRead 返回该刻度的注入错误，并按真 model 的方式包一层表名/方法名
// （生产 model 全部是 fmt.Errorf("<table> <Method>: %w", err)，
// 包这一层才能同时断言「错误链没被 logic 掐掉」与「错误信息指向真凶」）。
func (db *fakeDB) failRead(name string) error {
	if err := db.readErrs[name]; err != nil {
		return fmt.Errorf("fake %s: %w", name, err)
	}
	return nil
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		items:      map[int64]*model.ConfigItem{},
		versions:   map[int64]*model.ConfigVersion{},
		rules:      map[int64]*model.RolloutRule{},
		topics:     map[int64]*model.Topic{},
		topicItems: map[int64]*model.TopicItem{},
		slots:      map[int64]*model.RecommendSlot{},
		slotItems:  map[int64]*model.SlotItem{},
		switches:   map[int64]*model.ClientSwitch{},
		readErrs:   map[string]error{},
		next:       1000,
		calls:      map[string]int{},
	}
}

func (db *fakeDB) tick(name string) {
	if db.calls == nil {
		db.calls = map[string]int{}
	}
	db.calls[name]++
}

func (db *fakeDB) id() int64 { db.next++; return db.next }

// --- 快照/还原（事务回滚的等价物） ---

type dbSnapshot struct {
	items      map[int64]*model.ConfigItem
	versions   map[int64]*model.ConfigVersion
	rules      map[int64]*model.RolloutRule
	topics     map[int64]*model.Topic
	topicItems map[int64]*model.TopicItem
	slots      map[int64]*model.RecommendSlot
	slotItems  map[int64]*model.SlotItem
	switches   map[int64]*model.ClientSwitch
	next       int64
}

func copyMap[V any](src map[int64]V) map[int64]V {
	dst := make(map[int64]V, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (db *fakeDB) snapshot() dbSnapshot {
	return dbSnapshot{
		items: copyMap(db.items), versions: copyMap(db.versions), rules: copyMap(db.rules),
		topics: copyMap(db.topics), topicItems: copyMap(db.topicItems), slots: copyMap(db.slots),
		slotItems: copyMap(db.slotItems), switches: copyMap(db.switches), next: db.next,
	}
}

func (db *fakeDB) restore(s dbSnapshot) {
	db.items, db.versions, db.rules = s.items, s.versions, s.rules
	db.topics, db.topicItems, db.slots = s.topics, s.topicItems, s.slots
	db.slotItems, db.switches, db.next = s.slotItems, s.switches, s.next
}

// fakeConn 只实现 TransactCtx：成功即提交（内存里已经写好了），失败整库还原。
type fakeConn struct {
	sqlx.SqlConn
	db *fakeDB
}

func (c *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.db.txCalls++
	snap := c.db.snapshot()
	if err := fn(ctx, &fakeSession{db: c.db}); err != nil {
		c.db.restore(snap)
		return err
	}
	return nil
}

// fakeSession 只是占位：fakeDB 的事务内方法与自动提交方法写的是同一个内存库。
type fakeSession struct {
	sqlx.Session
	db *fakeDB
}

// fakeCache 记录被读/写/删过的键，用于验证「只删自己的键空间」（AGENTS.md §5）。
type fakeCache struct {
	data    map[string]string
	gets    []string
	sets    []string
	dels    []string
	delErr  error
	getErr  error
	setErr  error
	hits    int
	misses  int
	setTTLs map[string]int
}

func newFakeCache() *fakeCache {
	return &fakeCache{data: map[string]string{}, setTTLs: map[string]int{}}
}

func (c *fakeCache) Get(_ context.Context, key string) (string, error) {
	c.gets = append(c.gets, key)
	if c.getErr != nil {
		return "", c.getErr
	}
	if v, ok := c.data[key]; ok {
		c.hits++
		return v, nil
	}
	c.misses++
	return "", nil
}

func (c *fakeCache) Setex(_ context.Context, key, value string, seconds int) error {
	if seconds <= 0 {
		return nil
	}
	c.sets = append(c.sets, key)
	c.setTTLs[key] = seconds
	if c.setErr != nil {
		// 记录「尝试写过」但不落数据：等价于 Redis 写失败，投影仍然是旧的/没有。
		return c.setErr
	}
	c.data[key] = value
	return nil
}

func (c *fakeCache) Del(_ context.Context, keys ...string) (int64, error) {
	if c.delErr != nil {
		return 0, c.delErr
	}
	var n int64
	for _, k := range keys {
		c.dels = append(c.dels, k)
		if _, ok := c.data[k]; ok {
			delete(c.data, k)
			n++
		}
	}
	return n, nil
}

func (c *fakeCache) seed(key, value string) { c.data[key] = value }

// fakeAudit 只实现用到的那一个方法；其余方法被调用即 panic（接口是 nil）。
type fakeAudit struct {
	auditrpc.AuditClient
	reqs    []*auditrpc.AppendAuditReq
	entryID int64
	err     error
}

func (f *fakeAudit) AppendAudit(_ context.Context, in *auditrpc.AppendAuditReq,
	_ ...grpc.CallOption) (*auditrpc.AppendAuditReply, error) {
	f.reqs = append(f.reqs, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.entryID <= 0 {
		return &auditrpc.AppendAuditReply{}, nil
	}
	return &auditrpc.AppendAuditReply{Entry: &auditrpc.AuditEntryView{EntryId: f.entryID}}, nil
}

// fakeLogWriter 把 logx 的输出同时接一份到内存里。
//
// 它存在的理由：本服务有一类契约**只能从日志证明** —— 审计写不进去时不阻塞业务，
// 但必须把「待补偿的 event_id」打进 Error 日志（rpc/opsconfig.proto 文件头、
// internal/svc 对 Audit 字段的注释）。回参里没有 entry_id 只说明「没写成」，
// 说明不了「补偿任务找得到活」。用 AddWriter 挂进链，不顶掉默认输出，
// 这样日志既能在测试里被断言，也能在 go test 的输出里被人看见。
type fakeLogWriter struct {
	mu   sync.Mutex
	text []string
}

func (w *fakeLogWriter) Alert(v any) {
	w.add("severe", v)
}

func (w *fakeLogWriter) Close() error { return nil }

func (w *fakeLogWriter) Debug(v any, fields ...logx.LogField) { w.add("debug", v, fields...) }
func (w *fakeLogWriter) Error(v any, fields ...logx.LogField) { w.add("error", v, fields...) }
func (w *fakeLogWriter) Info(v any, fields ...logx.LogField)  { w.add("info", v, fields...) }
func (w *fakeLogWriter) Severe(v any)                         { w.add("severe", v) }
func (w *fakeLogWriter) Slow(v any, fields ...logx.LogField)  { w.add("slow", v, fields...) }
func (w *fakeLogWriter) Stack(v any)                          { w.add("error", v) }
func (w *fakeLogWriter) Stat(v any, fields ...logx.LogField)  { w.add("stat", v, fields...) }

// fieldsText 把结构化字段并成 `key=value` 一起留痕：logx 把 Infow 的字段作为可变参数
// 传给每个 writer，忽略它们就等于让按字段值做的断言全部失效（正向永假、隐私反向永真）。
func fieldsText(fields []logx.LogField) string {
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, " %s=%v", f.Key, f.Value)
	}
	return b.String()
}

func (w *fakeLogWriter) add(kind string, v any, fields ...logx.LogField) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.text = append(w.text, kind+" "+fmt.Sprint(v)+fieldsText(fields))
}

// errors 只回 Error/Severe 级内容：断言「缺口在日志里可见」不该被 info 噪声影响。
func (w *fakeLogWriter) errors() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, line := range w.text {
		if strings.HasPrefix(line, "error ") || strings.HasPrefix(line, "severe ") {
			out = append(out, line)
		}
	}
	return out
}

func (w *fakeLogWriter) joined() string { return strings.Join(w.errors(), "\n") }

// --- 测试脚手架 ---

func testConfig() config.Config {
	var c config.Config
	c.Cache.KeyPrefix = "govideo:opsconfig:test"
	c.Cache.DeleteOnBump = true
	c.Cache.MaxTTLSeconds = 3600
	c.Query.MaxPageSize = 100
	c.Query.CountTotal = true
	c.OpsValue.MaxBytes = 8192
	c.OpsValue.MaxKeyLen = 64
	c.OpsValue.MaxReasonLen = 500
	c.OpsValue.MaxOperatorNameLen = 64
	c.OpsTopic.MaxItems = 500
	c.OpsTopic.MaxRefIDs = 64
	c.OpsTopic.DefaultItemLimit = 100
	c.OpsTopic.ListTTLSeconds = 60
	c.OpsSlot.MaxCapacity = 200
	c.OpsSlot.MaxItems = 200
	c.OpsSlot.DefaultTTLSeconds = 30
	c.Rollout.MaxWhitelistMids = 200
	c.Rollout.MaxRulesPerConfig = 50
	c.Resolve.MaxBatchKeys = 50
	c.Resolve.DefaultTTLSeconds = 60
	c.Resolve.DefaultTopicTTLSeconds = 120
	return c
}

type testEnv struct {
	t      *testing.T
	svc    *svc.ServiceContext
	db     *fakeDB
	cache  *fakeCache
	audit  *fakeAudit
	limits limits
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, nil)
}

// newTestEnvWith 在默认配置上开一个改动口子：限额类契约（total 是否可信、
// TTL 是否被夹住、ps 上限）只能靠换配置来证，复制整份 testConfig 会让默认值漂移时
// 两处一起错，看不出差异。
func newTestEnvWith(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()
	cfg := testConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	db := newFakeDB()
	cache := newFakeCache()
	audit := &fakeAudit{entryID: 777}
	svcCtx := &svc.ServiceContext{
		Config: cfg,
		Conn:   &fakeConn{db: db},
		Cache:  cache,
		Models: svc.Models{
			ConfigItem:    itemModel{db},
			ConfigVersion: versionModel{db},
			RolloutRule:   ruleModel{db},
			Topic:         topicModel{db},
			TopicItem:     topicItemModel{db},
			Slot:          slotModel{db},
			SlotItem:      slotItemModel{db},
			ClientSwitch:  switchModel{db},
		},
		Audit: audit,
	}
	return &testEnv{t: t, svc: svcCtx, db: db, cache: cache, audit: audit, limits: newLimits(cfg)}
}

func (e *testEnv) fatalOn(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("意外错误: %v", err)
	}
}

// callCtx 是写接口的身份三件套。
func callCtx(requestID string) *rpc.CallContext {
	return &rpc.CallContext{RequestId: requestID, OperatorId: 42, OperatorName: "运营甲"}
}

func rolloutCtx(mid int64, platform rpc.ClientPlatform, appVersion string) *rpc.TargetContext {
	return &rpc.TargetContext{Mid: mid, Platform: platform, AppVersion: appVersion}
}

// --- ConfigItemModel ---

func (db *fakeDB) Insert(_ context.Context, item *model.ConfigItem) (int64, error) {
	db.tick("ConfigItem.Insert")
	if !model.ValidCfgKey(item.CfgKey) {
		return 0, model.ErrConfigKeyInvalid
	}
	if !model.ValidScope(item.Scope) {
		return 0, model.ErrScopeUnknown
	}
	if !model.ValidValueType(item.ValueType) {
		return 0, model.ErrValueTypeUnsupported
	}
	for _, row := range db.items {
		if row.CfgKey == item.CfgKey && row.Scope == item.Scope {
			return 0, model.ErrConfigExists // uniq_key_scope：空更新 + RowsAffected==0
		}
	}
	if item.Ctime == 0 {
		item.Ctime = model.NowUnix()
	}
	item.Mtime = item.Ctime // 与真 model 同：首次写入 mtime 等于 ctime
	if item.State == 0 {
		item.State = model.StateOn // 默认启用：新建即「刚发布就不可读」不是任何人想要的语义
	}
	cp := *item
	if cp.ConfigID == 0 {
		cp.ConfigID = db.id()
	}
	db.items[cp.ConfigID] = &cp
	// 真 Insert 是**就地**修改入参再落库，并用 LastInsertId 回填主键。
	item.ConfigID = cp.ConfigID
	return cp.ConfigID, nil
}

func (db *fakeDB) FindOne(_ context.Context, cfgKey, scope string) (*model.ConfigItem, error) {
	db.tick("ConfigItem.FindOne")
	if err := db.failRead("ConfigItem.FindOne"); err != nil {
		return nil, err
	}
	if cfgKey == "" {
		return nil, model.ErrConfigKeyRequired
	}
	for _, row := range db.items {
		if row.CfgKey == cfgKey && row.Scope == scope {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (db *fakeDB) FindByID(_ context.Context, configID int64) (*model.ConfigItem, error) {
	db.tick("ConfigItem.FindByID")
	row, ok := db.items[configID]
	if !ok {
		// 与真 model 一致：ops_config_item.FindByID 缺行返回 ErrConfigNotFound 而不是 (nil,nil)。
		// 若这里回 (nil,nil)，logic 里的「找不到」分支会以另一种形态被覆盖，
		// 测试就证不到生产实际走的那条错误路径。
		return nil, model.ErrConfigNotFound
	}
	cp := *row
	return &cp, nil
}

func (db *fakeDB) FindByKeys(_ context.Context, cfgKeys []string, scope string) (map[string]*model.ConfigItem, error) {
	db.tick("ConfigItem.FindByKeys")
	want := map[string]struct{}{}
	for _, k := range cfgKeys {
		want[k] = struct{}{}
	}
	out := map[string]*model.ConfigItem{}
	for _, row := range db.items {
		if _, ok := want[row.CfgKey]; !ok || row.Scope != scope {
			continue
		}
		cp := *row
		out[row.CfgKey] = &cp
	}
	return out, nil
}

func (db *fakeDB) AdvanceRelease(ctx context.Context, configID, expectLatest, newLatest, operatorID, ts int64) (bool, error) {
	// 独立计数：非事务版会「自动提交」半条发布，正常路径不该用到它。
	db.tick("ConfigItem.AdvanceRelease.autocommit")
	return db.advanceRelease(ctx, configID, expectLatest, newLatest, operatorID, ts)
}

func (db *fakeDB) AdvanceReleaseTx(_ context.Context, _ sqlx.Session, configID, expectLatest,
	newLatest, operatorID, ts int64) (bool, error) {
	return db.advanceRelease(context.Background(), configID, expectLatest, newLatest, operatorID, ts)
}

func (db *fakeDB) advanceRelease(_ context.Context, configID, expectLatest, newLatest, operatorID, ts int64) (bool, error) {
	db.tick("ConfigItem.AdvanceRelease")
	if db.failAdvanceRelease {
		return false, nil // 别人已经推进过：条件 UPDATE 影响 0 行
	}
	if newLatest <= expectLatest {
		return false, fmt.Errorf("ops_config_item AdvanceRelease: %w", model.ErrVersionConflict)
	}
	row, ok := db.items[configID]
	if !ok || row.LatestVersion != expectLatest {
		return false, nil
	}
	cp := *row
	cp.LatestVersion = newLatest
	cp.Epoch++
	cp.OperatorID = operatorID
	cp.Mtime = ts
	db.items[configID] = &cp
	return true, nil
}

func (db *fakeDB) BumpEpoch(_ context.Context, configIDs []int64, ts int64) (int64, error) {
	db.tick("ConfigItem.BumpEpoch")
	var n int64
	for _, id := range configIDs {
		row, ok := db.items[id]
		if !ok {
			continue
		}
		cp := *row
		cp.Epoch++
		cp.Mtime = ts
		db.items[id] = &cp
		n++
	}
	return n, nil
}

func (db *fakeDB) BumpAllEpoch(_ context.Context, state int32, ts int64) (int64, error) {
	db.tick("ConfigItem.BumpAllEpoch")
	var n int64
	for id, row := range db.items {
		if state != 0 && row.State != state {
			continue
		}
		cp := *row
		cp.Epoch++
		cp.Mtime = ts
		db.items[id] = &cp
		n++
	}
	return n, nil
}

func (db *fakeDB) List(_ context.Context, f model.ConfigItemFilter) ([]*model.ConfigItem, int64, error) {
	db.tick("ConfigItem.List")
	var all []*model.ConfigItem
	for _, row := range db.items {
		if f.Scope != "" && row.Scope != f.Scope {
			continue
		}
		if f.State != 0 && row.State != f.State {
			continue
		}
		if f.Keyword != "" && !strings.Contains(row.CfgKey, f.Keyword) && !strings.Contains(row.Title, f.Keyword) {
			continue
		}
		cp := *row
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CfgKey != all[j].CfgKey {
			return all[i].CfgKey < all[j].CfgKey
		}
		return all[i].Scope < all[j].Scope // ORDER BY cfg_key ASC, scope ASC
	})
	total := int64(len(all))
	return pageSlice(all, f.Pn, f.Ps), total, nil
}

// --- ConfigVersionModel ---

func (db *fakeDB) insertVersion(v *model.ConfigVersion) (int64, error) {
	if db.failInTx == "insert_version" {
		return 0, errors.New("注入：事务内写版本行失败")
	}
	if db.conflictOnInsert != nil {
		return 0, db.conflictOnInsert
	}
	if v.ConfigID <= 0 {
		return 0, model.ErrConfigNotFound
	}
	if v.Version <= 0 {
		return 0, model.ErrVersionNotFound
	}
	if v.RequestID == "" {
		return 0, model.ErrRequestIDRequired
	}
	if v.Reason == "" {
		return 0, model.ErrReasonRequired
	}
	switch v.ChangeType {
	case model.ChangeTypeCreate, model.ChangeTypePublish, model.ChangeTypeRollback:
	default:
		return 0, model.ErrChangeTypeInvalid
	}
	if !model.ValidValueType(v.ValueType) {
		return 0, model.ErrValueTypeUnsupported
	}
	for _, row := range db.versions {
		// 两条唯一键各自独立判定，与真 SQL 的 ON DUPLICATE KEY 一致。
		if row.ConfigID == v.ConfigID && row.Version == v.Version {
			return 0, model.ErrVersionExists
		}
		if row.RequestID == v.RequestID {
			return 0, model.ErrVersionExists
		}
	}
	cp := *v
	if cp.VersionID == 0 {
		cp.VersionID = db.id() // 自增主键的替身：真库由 AUTO_INCREMENT 分配
	}
	if cp.PublishedAt == 0 {
		cp.PublishedAt = model.NowUnix()
	}
	if cp.Ctime == 0 {
		cp.Ctime = cp.PublishedAt
	}
	v.PublishedAt, v.Ctime = cp.PublishedAt, cp.Ctime // 真 insertVersion 也回写入参
	db.versions[cp.VersionID] = &cp
	v.VersionID = cp.VersionID // 真 model 也回填：调用方要用它去 backfill 审计
	return cp.VersionID, nil
}

func (db *fakeDB) InsertVersion(ctx context.Context, v *model.ConfigVersion) (int64, error) {
	db.tick("ConfigVersion.Insert")
	return db.insertVersion(v)
}

func (db *fakeDB) InsertVersionTx(_ context.Context, _ sqlx.Session, v *model.ConfigVersion) (int64, error) {
	db.tick("ConfigVersion.InsertTx")
	return db.insertVersion(v)
}

func (db *fakeDB) FindVersion(_ context.Context, configID, version int64) (*model.ConfigVersion, error) {
	db.tick("ConfigVersion.FindOne")
	for _, row := range db.versions {
		if row.ConfigID == configID && row.Version == version {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (db *fakeDB) FindVersionByRequestID(_ context.Context, requestID string) (*model.ConfigVersion, error) {
	db.tick("ConfigVersion.FindByRequestID")
	if requestID == "" {
		return nil, model.ErrRequestIDRequired
	}
	for _, row := range db.versions {
		if row.RequestID == requestID {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (db *fakeDB) FindLatestVersion(ctx context.Context, configID int64) (*model.ConfigVersion, error) {
	db.tick("ConfigVersion.FindLatest")
	maxVer, err := db.MaxVersion(ctx, configID)
	if err != nil || maxVer == 0 {
		return nil, err
	}
	return db.FindVersion(ctx, configID, maxVer)
}

func (db *fakeDB) FindVersions(_ context.Context, configID int64, versions []int64) (map[int64]*model.ConfigVersion, error) {
	db.tick("ConfigVersion.FindVersions")
	want := map[int64]struct{}{}
	for _, v := range versions {
		want[v] = struct{}{}
	}
	out := map[int64]*model.ConfigVersion{}
	for _, row := range db.versions {
		if row.ConfigID != configID {
			continue
		}
		if _, ok := want[row.Version]; !ok {
			continue
		}
		cp := *row
		out[row.Version] = &cp
	}
	return out, nil
}

func (db *fakeDB) ListVersions(_ context.Context, configID int64, pn, ps int32) ([]*model.ConfigVersion, int64, error) {
	db.tick("ConfigVersion.ListByConfig")
	if err := db.failRead("ConfigVersion.ListByConfig"); err != nil {
		return nil, 0, err
	}
	var all []*model.ConfigVersion
	for _, row := range db.versions {
		if row.ConfigID != configID {
			continue
		}
		cp := *row
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Version > all[j].Version })
	total := int64(len(all))
	return pageSlice(all, pn, ps), total, nil
}

func (db *fakeDB) MaxVersion(_ context.Context, configID int64) (int64, error) {
	db.tick("ConfigVersion.MaxVersion")
	var maxVer int64
	for _, row := range db.versions {
		if row.ConfigID == configID && row.Version > maxVer {
			maxVer = row.Version
		}
	}
	return maxVer, nil
}

func (db *fakeDB) SetAuditEntry(_ context.Context, versionID, entryID int64) (bool, error) {
	db.tick("ConfigVersion.SetAuditEntry")
	// 与真 SQL 同：只当库里当前 audit_entry_id = 0 时写入，重复补写回 false，
	// 让调用方能分辨「这次补上的」与「早就有存证」。
	if versionID <= 0 {
		return false, model.ErrVersionNotFound
	}
	if entryID <= 0 {
		return false, model.ErrAuditRefAlreadySet
	}
	row, ok := db.versions[versionID]
	if !ok || row.AuditEntryID != 0 {
		return false, nil
	}
	cp := *row
	cp.AuditEntryID = entryID
	db.versions[versionID] = &cp
	return true, nil
}

// --- RolloutRuleModel ---

func (db *fakeDB) upsertRule(rule *model.RolloutRule) (int64, bool, error) {
	if db.failInTx == "upsert_rule" {
		return 0, false, errors.New("注入：事务内写规则失败")
	}
	// 校验顺序与真 upsertRule 一致：先 name，再形态（形态里含 version<=0 → ErrVersionNotFound）。
	if rule.Name == "" {
		return 0, false, model.ErrRuleNameRequired
	}
	if err := model.ValidateRuleShape(rule, model.MaxWhitelistMids); err != nil {
		return 0, false, err
	}
	suffixes, err := model.NormalizeMidSuffixes(rule.MidSuffixes)
	if err != nil {
		return 0, false, err
	}
	rule.MidSuffixes = suffixes
	if rule.State == 0 {
		rule.State = model.StateOff // 默认停用：写规则与开闸是两步
	}
	if rule.Priority == 0 {
		rule.Priority = 100 // 真 model 的默认名次，缺了它「首个命中」的次序就不同
	}
	if rule.Ctime == 0 {
		rule.Ctime = model.NowUnix()
	}
	rule.Mtime = rule.Ctime
	for id, row := range db.rules {
		if row.ConfigID == rule.ConfigID && row.Version == rule.Version && row.Name == rule.Name {
			cp := *rule
			cp.RuleID = id // 幂等覆盖：同一 (config_id, version, name) 只有一行
			cp.Ctime = row.Ctime
			db.rules[id] = &cp
			rule.RuleID = id
			return id, false, nil
		}
	}
	cp := *rule
	if cp.RuleID == 0 {
		cp.RuleID = db.id()
	}
	db.rules[cp.RuleID] = &cp
	rule.RuleID = cp.RuleID
	return cp.RuleID, true, nil
}

func (db *fakeDB) UpsertRule(ctx context.Context, rule *model.RolloutRule) (int64, bool, error) {
	db.tick("RolloutRule.Upsert")
	return db.upsertRule(rule)
}

func (db *fakeDB) UpsertRuleTx(_ context.Context, _ sqlx.Session, rule *model.RolloutRule) (int64, bool, error) {
	db.tick("RolloutRule.UpsertTx")
	return db.upsertRule(rule)
}

func (db *fakeDB) FindRuleByID(_ context.Context, ruleID int64) (*model.RolloutRule, error) {
	db.tick("RolloutRule.FindByID")
	row, ok := db.rules[ruleID]
	if !ok {
		// 接口注释写明：按主键查询不存在返回 ErrRuleNotFound，而不是 (nil,nil)。
		return nil, model.ErrRuleNotFound
	}
	cp := *row
	return &cp, nil
}

func (db *fakeDB) FindRuleByName(_ context.Context, configID, version int64, name string) (*model.RolloutRule, error) {
	db.tick("RolloutRule.FindByName")
	for _, row := range db.rules {
		if row.ConfigID == configID && row.Version == version && row.Name == name {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (db *fakeDB) ListCandidates(_ context.Context, configID, ts int64, limit int) ([]*model.RolloutRule, error) {
	db.tick("RolloutRule.ListCandidates")
	// 与真 SQL 同：configID 非法直接报错（少这一步，logic 的守卫分支就永远测不到），
	// limit 缺省回落到硬上限，避免一次解析扫全表。
	if configID <= 0 {
		return nil, model.ErrConfigNotFound
	}
	if limit <= 0 {
		limit = model.MaxRolloutCandidates
	}
	var out []*model.RolloutRule
	for _, row := range db.rules {
		if row.ConfigID != configID || row.State != model.StateOn {
			continue
		}
		// 时间窗口下推到 SQL 的等价物：0 表示该侧不限。
		if row.StartAt != 0 && row.StartAt > ts {
			continue
		}
		if row.EndAt != 0 && row.EndAt <= ts {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	// 顺序即优先级：与真 SQL 的 ORDER BY priority ASC, rule_id ASC 保持一致。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].RuleID < out[j].RuleID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (db *fakeDB) ListRules(_ context.Context, f model.RolloutRuleFilter) ([]*model.RolloutRule, int64, error) {
	db.tick("RolloutRule.List")
	if err := db.failRead("RolloutRule.List"); err != nil {
		return nil, 0, err
	}
	var all []*model.RolloutRule
	for _, row := range db.rules {
		if f.ConfigID != 0 && row.ConfigID != f.ConfigID {
			continue
		}
		if f.Version != 0 && row.Version != f.Version {
			continue
		}
		if f.State != 0 && row.State != f.State {
			continue
		}
		if f.Mode != 0 && row.Mode != f.Mode {
			continue
		}
		cp := *row
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ConfigID != all[j].ConfigID {
			return all[i].ConfigID < all[j].ConfigID // ORDER BY config_id, priority, rule_id
		}
		if all[i].Priority != all[j].Priority {
			return all[i].Priority < all[j].Priority
		}
		return all[i].RuleID < all[j].RuleID
	})
	total := int64(len(all))
	return pageSlice(all, f.Pn, f.Ps), total, nil
}

func (db *fakeDB) setRuleState(ruleID int64, state, expectState int32, operatorID, ts int64) (bool, error) {
	if db.failInTx == "set_state" {
		return false, errors.New("注入：事务内写规则状态失败")
	}
	// 真 SQL 在进 UPDATE 之前先拒绝非法状态与非正主键：少了这两步，
	// logic 的「state 非法」与「规则不存在」就会在测试里走成 updated=false。
	if !model.ValidState(state) {
		return false, model.ErrRuleStateInvalid
	}
	if ruleID <= 0 {
		return false, model.ErrRuleNotFound
	}
	row, ok := db.rules[ruleID]
	if !ok {
		return false, nil // WHERE rule_id = ? 未命中 = 0 行
	}
	if expectState != 0 && row.State != expectState {
		return false, nil // 条件 UPDATE 未命中 = 别人改过
	}
	cp := *row
	cp.State = state
	cp.OperatorID = operatorID
	cp.Mtime = ts
	db.rules[ruleID] = &cp
	return true, nil
}

func (db *fakeDB) SetRuleState(_ context.Context, ruleID int64, state, expectState int32, operatorID, ts int64) (bool, error) {
	db.tick("RolloutRule.SetState")
	return db.setRuleState(ruleID, state, expectState, operatorID, ts)
}

func (db *fakeDB) SetRuleStateTx(_ context.Context, _ sqlx.Session, ruleID int64, state,
	expectState int32, operatorID, ts int64) (bool, error) {
	db.tick("RolloutRule.SetStateTx")
	return db.setRuleState(ruleID, state, expectState, operatorID, ts)
}

func (db *fakeDB) CountByVersion(_ context.Context, configID, version int64, state int32) (int64, error) {
	db.tick("RolloutRule.CountByVersion")
	var n int64
	for _, row := range db.rules {
		if row.ConfigID != configID || row.Version != version {
			continue
		}
		if state != 0 && row.State != state {
			continue
		}
		n++
	}
	return n, nil
}

// --- TopicModel ---

func (db *fakeDB) InsertTopic(_ context.Context, topic *model.Topic) (int64, error) {
	db.tick("Topic.Insert")
	if err := checkTopicForWrite(topic); err != nil {
		return 0, err
	}
	for _, row := range db.topics {
		if row.Slug == topic.Slug {
			return 0, model.ErrTopicSlugConflict // uniq_slug 的空更新语义
		}
	}
	cp := *topic
	if cp.TopicID == 0 {
		cp.TopicID = db.id()
	}
	if cp.State == 0 {
		cp.State = model.StateOff // 默认下架：条目还没挂就先上线，端上会看到空专题
	}
	if cp.Version == 0 {
		cp.Version = 1
	}
	db.topics[cp.TopicID] = &cp
	topic.TopicID = cp.TopicID // 真 Insert 也回写 LastInsertId
	return cp.TopicID, nil
}

// checkTopicForWrite 复刻真 model 在 Insert 与 UpdateWithVersion **两条入口**里
// 都做的那串前置校验。它刻意不写默认值：更新路径不补 State，
// 否则「logic 漏传 state 却测试通过」这种分叉会被 fake 掩盖掉。
func checkTopicForWrite(topic *model.Topic) error {
	if !model.ValidTopicSlug(topic.Slug) {
		return model.ErrTopicSlugInvalid
	}
	if topic.Title == "" {
		return model.ErrTopicTitleRequired
	}
	if topic.EndAt > 0 && topic.EndAt <= topic.StartAt {
		return model.ErrTopicTimeRangeInvalid
	}
	if _, err := model.ValidateIDList(topic.ZoneIDList(), model.MaxTopicRefIDs); err != nil {
		return err
	}
	if _, err := model.ValidateIDList(topic.TagIDList(), model.MaxTopicRefIDs); err != nil {
		return err
	}
	if topic.Ctime == 0 {
		topic.Ctime = model.NowUnix()
	}
	topic.Mtime = topic.Ctime
	return nil
}

func (db *fakeDB) FindTopicByID(_ context.Context, topicID int64) (*model.Topic, error) {
	db.tick("Topic.FindByID")
	if err := db.failRead("Topic.FindByID"); err != nil {
		return nil, err
	}
	row, ok := db.topics[topicID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (db *fakeDB) FindTopicBySlug(_ context.Context, slug string) (*model.Topic, error) {
	db.tick("Topic.FindBySlug")
	if err := db.failRead("Topic.FindBySlug"); err != nil {
		return nil, err
	}
	if slug == "" {
		return nil, model.ErrTopicSlugRequired
	}
	for _, row := range db.topics {
		if row.Slug == slug {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (db *fakeDB) UpdateTopicWithVersion(_ context.Context, topic *model.Topic, expectVersion, ts int64) (bool, error) {
	db.tick("Topic.UpdateWithVersion")
	if topic.TopicID <= 0 {
		return false, model.ErrTopicNotFound
	}
	// 复刻真实现：先做与 Insert 同一套校验，再探测 slug 是否被**别的**专题占用，
	// 好把「冲突」与「版本被抢先」区分开。缺了探测这两类失败在测试里就分不出来。
	if err := checkTopicForWrite(topic); err != nil {
		return false, err
	}
	for id, row := range db.topics {
		if row.Slug == topic.Slug && id != topic.TopicID {
			return false, model.ErrTopicSlugConflict
		}
	}
	row, ok := db.topics[topic.TopicID]
	if !ok || row.Version != expectVersion {
		return false, nil // WHERE topic_id=? AND version=? 未命中
	}
	cp := *topic
	cp.Version = expectVersion + 1
	cp.Mtime = ts
	db.topics[cp.TopicID] = &cp
	// 真实现同样回写入参，logic 直接投影新值而不再回查。
	topic.Version, topic.Mtime = cp.Version, cp.Mtime
	return true, nil
}

func (db *fakeDB) ListTopics(_ context.Context, f model.TopicFilter) ([]*model.Topic, int64, error) {
	db.tick("Topic.List")
	if err := db.failRead("Topic.List"); err != nil {
		return nil, 0, err
	}
	var all []*model.Topic
	for _, row := range db.topics {
		// 每个 if 对应 TopicFilter.build() 里的一段 WHERE，口径必须一致：
		// 少一段，「后台筛出来的」与「运行时命中的」就不是同一批数据。
		if f.State > 0 && row.State != f.State {
			continue
		}
		if f.ZoneID > 0 && !model.IDListContains(row.ZoneIDs, f.ZoneID) {
			continue
		}
		if f.TagID > 0 && !model.IDListContains(row.TagIDs, f.TagID) {
			continue
		}
		if f.OnlineOnly && (row.State != model.StateOn || !row.InWindow(f.Now)) {
			continue
		}
		if f.Keyword != "" && !strings.Contains(row.Slug, f.Keyword) &&
			!strings.Contains(row.Title, f.Keyword) {
			continue
		}
		cp := *row
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Sort != all[j].Sort {
			return all[i].Sort < all[j].Sort
		}
		return all[i].TopicID < all[j].TopicID
	})
	total := int64(len(all))
	return pageSlice(all, f.Pn, f.Ps), total, nil
}

func (db *fakeDB) ListOnlineTopics(ctx context.Context, ts int64, limit int) ([]*model.Topic, error) {
	if limit <= 0 {
		limit = model.DefaultTopicListLimit // 真实现同样回落服务端默认，而不是扫全表
	}
	// 必须走 OnlineOnly：生效窗口只有在 OnlineOnly 分支里才被过滤。
	// 早先这里传 State=ON + Now 而没打开 OnlineOnly，于是「过期专题仍被列出」
	// 这一整类错误在测试里根本不可能出现。
	rows, _, err := db.ListTopics(ctx, model.TopicFilter{OnlineOnly: true, Now: ts, Pn: 1, Ps: int32(limit)})
	return rows, err
}

func (db *fakeDB) TouchTopicMtime(_ context.Context, topicID, ts int64) (int64, error) {
	db.tick("Topic.TouchMtime")
	row, ok := db.topics[topicID]
	if !ok {
		return 0, nil
	}
	cp := *row
	cp.Mtime = ts
	db.topics[topicID] = &cp
	return 1, nil
}

// --- TopicItemModel ---

func (db *fakeDB) ReplaceTopicItems(_ context.Context, topicID int64, items []*model.TopicItem,
	operatorID, ts int64, maxItems int) (int64, error) {
	db.tick("TopicItem.ReplaceAll")
	if err := model.ValidateTopicItems(topicID, items, maxItems); err != nil {
		return 0, err
	}
	for id := range db.topicItems {
		if db.topicItems[id].TopicID == topicID {
			delete(db.topicItems, id)
		}
	}
	var n int64
	for _, it := range items {
		cp := *it
		cp.TopicID = topicID
		if cp.ID == 0 {
			cp.ID = db.id()
		}
		cp.OperatorID = operatorID
		cp.Ctime, cp.Mtime = ts, ts
		if cp.State == 0 {
			cp.State = model.StateOn
		}
		db.topicItems[cp.ID] = &cp
		n++
	}
	return n, nil
}

func (db *fakeDB) ListTopicItems(_ context.Context, topicID int64, state int32, limit int) ([]*model.TopicItem, error) {
	db.tick("TopicItem.ListByTopic")
	if err := db.failRead("TopicItem.ListByTopic"); err != nil {
		return nil, err
	}
	if topicID <= 0 {
		return nil, model.ErrTopicNotFound
	}
	if limit <= 0 {
		limit = model.DefaultTopicListLimit
	}
	var out []*model.TopicItem
	for _, row := range db.topicItems {
		if row.TopicID != topicID || (state != 0 && row.State != state) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (db *fakeDB) CountTopicItems(_ context.Context, topicID int64, state int32) (int64, error) {
	db.tick("TopicItem.CountByTopic")
	var n int64
	for _, row := range db.topicItems {
		if row.TopicID == topicID && (state == 0 || row.State == state) {
			n++
		}
	}
	return n, nil
}

func (db *fakeDB) FindTopicItemsByRef(_ context.Context, itemType, itemID string, limit int) ([]*model.TopicItem, error) {
	db.tick("TopicItem.FindByItemRef")
	if itemType == "" || itemID == "" {
		return nil, model.ErrItemRefRequired
	}
	if limit <= 0 {
		limit = model.DefaultTopicListLimit
	}
	var out []*model.TopicItem
	for _, row := range db.topicItems {
		// state=ON 是真 SQL 的硬条件：反查的用途是「内容下架时定位**当前**受影响的运营位」，
		// 把已移除的行也捞回来会让调用方以为还有地方在展示它。
		if row.ItemType != itemType || row.ItemID != itemID || row.State != model.StateOn {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TopicID != out[j].TopicID {
			return out[i].TopicID < out[j].TopicID
		}
		return out[i].Position < out[j].Position
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- RecommendSlotModel ---

func (db *fakeDB) InsertSlot(_ context.Context, slot *model.RecommendSlot) (int64, error) {
	db.tick("Slot.Insert")
	if slot.Code == "" {
		return 0, model.ErrSlotCodeRequired
	}
	if err := checkSlotForWrite(slot); err != nil {
		return 0, err
	}
	for _, row := range db.slots {
		if row.Code == slot.Code {
			return 0, model.ErrSlotCodeConflict
		}
	}
	cp := *slot
	if cp.SlotID == 0 {
		cp.SlotID = db.id()
	}
	if cp.State == 0 {
		cp.State = model.StateOff
	}
	if cp.Version == 0 {
		cp.Version = 1
	}
	db.slots[cp.SlotID] = &cp
	slot.SlotID = cp.SlotID
	return cp.SlotID, nil
}

// checkSlotForWrite / checkSwitchForWrite 复刻真 model 里内联的 normalizeSlotForWrite /
// normalizeSwitchForWrite（那两个函数不导出，测试包只能按同一顺序重述一遍）。
// 顺序必须一致：先编码、再容量、再端列表，否则「容量非法」的用例
// 会在测试里被编码错误抢先，断言到的就不是同一条不变式。
func checkSlotForWrite(slot *model.RecommendSlot) error {
	if !model.ValidSlotCode(slot.Code) {
		return model.ErrSlotCodeInvalid
	}
	if !model.ValidSlotCapacity(slot.Capacity) {
		return model.ErrSlotCapacityInvalid
	}
	if _, err := model.ValidatePlatformList(slot.Platforms, 4); err != nil {
		return err
	}
	if slot.Ctime == 0 {
		slot.Ctime = model.NowUnix()
	}
	slot.Mtime = slot.Ctime
	return nil
}

func (db *fakeDB) FindSlotByID(_ context.Context, slotID int64) (*model.RecommendSlot, error) {
	db.tick("Slot.FindByID")
	row, ok := db.slots[slotID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (db *fakeDB) FindSlotByCode(_ context.Context, code string) (*model.RecommendSlot, error) {
	db.tick("Slot.FindByCode")
	if err := db.failRead("Slot.FindByCode"); err != nil {
		return nil, err
	}
	if code == "" {
		return nil, model.ErrSlotCodeRequired
	}
	for _, row := range db.slots {
		if row.Code == code {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (db *fakeDB) UpdateSlotWithVersion(_ context.Context, slot *model.RecommendSlot, expectVersion, ts int64) (bool, error) {
	db.tick("Slot.UpdateWithVersion")
	if slot.SlotID <= 0 {
		return false, model.ErrSlotNotFound
	}
	if err := checkSlotForWrite(slot); err != nil {
		return false, err
	}
	for id, row := range db.slots {
		if row.Code == slot.Code && id != slot.SlotID {
			return false, model.ErrSlotCodeConflict
		}
	}
	row, ok := db.slots[slot.SlotID]
	if !ok || row.Version != expectVersion {
		return false, nil
	}
	cp := *slot
	cp.Version = expectVersion + 1
	cp.Mtime = ts
	db.slots[cp.SlotID] = &cp
	slot.Version, slot.Mtime = cp.Version, cp.Mtime
	return true, nil
}

func (db *fakeDB) ListSlots(_ context.Context, f model.SlotFilter) ([]*model.RecommendSlot, int64, error) {
	db.tick("Slot.List")
	if err := db.failRead("Slot.List"); err != nil {
		return nil, 0, err
	}
	var all []*model.RecommendSlot
	for _, row := range db.slots {
		if f.Page != "" && row.Page != f.Page {
			continue
		}
		if f.State != 0 && row.State != f.State {
			continue
		}
		if f.Platform != 0 && !model.PlatformMatchesAny(row.Platforms, f.Platform) {
			continue
		}
		cp := *row
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Code < all[j].Code })
	total := int64(len(all))
	return pageSlice(all, f.Pn, f.Ps), total, nil
}

func (db *fakeDB) ListEnabledSlots(_ context.Context, limit int) ([]*model.RecommendSlot, error) {
	db.tick("Slot.ListEnabled")
	if limit <= 0 {
		limit = model.DefaultTopicListLimit
	}
	var out []*model.RecommendSlot
	for _, row := range db.slots {
		if row.State != model.StateOn {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- SlotItemModel ---

func (db *fakeDB) ReplaceSlotItems(_ context.Context, slotID int64, capacity int32, items []*model.SlotItem,
	operatorID, ts int64, maxItems int) (int64, error) {
	db.tick("SlotItem.ReplaceAll")
	if err := model.ValidateSlotItems(slotID, capacity, items, maxItems); err != nil {
		return 0, err
	}
	for id := range db.slotItems {
		if db.slotItems[id].SlotID == slotID {
			delete(db.slotItems, id)
		}
	}
	var n int64
	for _, it := range items {
		cp := *it
		cp.SlotID = slotID
		if cp.ID == 0 {
			cp.ID = db.id()
		}
		cp.OperatorID = operatorID
		cp.Ctime, cp.Mtime = ts, ts
		if cp.State == 0 {
			cp.State = model.StateOn
		}
		db.slotItems[cp.ID] = &cp
		n++
	}
	return n, nil
}

func (db *fakeDB) ListEffectiveSlotItems(_ context.Context, slotID, at int64, limit int) ([]*model.SlotItem, error) {
	db.tick("SlotItem.ListEffective")
	if err := db.failRead("SlotItem.ListEffective"); err != nil {
		return nil, err
	}
	if slotID <= 0 {
		return nil, model.ErrSlotNotFound
	}
	if limit <= 0 {
		limit = model.DefaultSlotCapacity // 真 SQL 的 LIMIT 兜底
	}
	var out []*model.SlotItem
	for _, row := range db.slotItems {
		if row.SlotID != slotID || row.State != model.StateOn {
			continue
		}
		if !row.InWindow(at) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (db *fakeDB) ListAllSlotItems(_ context.Context, slotID int64, limit int) ([]*model.SlotItem, error) {
	db.tick("SlotItem.ListAll")
	if slotID <= 0 {
		return nil, model.ErrSlotNotFound
	}
	if limit <= 0 {
		limit = model.MaxSlotItems
	}
	// 后台视图：含停用与过期排期，因此**不**过 state、也**不**过时间窗，
	// 但排序口径与 ListEffective 一致（position ASC, weight DESC, id ASC）。
	var all []*model.SlotItem
	for _, row := range db.slotItems {
		if row.SlotID != slotID {
			continue
		}
		cp := *row
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Position != all[j].Position {
			return all[i].Position < all[j].Position
		}
		if all[i].Weight != all[j].Weight {
			return all[i].Weight > all[j].Weight
		}
		return all[i].ID < all[j].ID
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (db *fakeDB) CountSlotItems(_ context.Context, slotID int64, state int32) (int64, error) {
	db.tick("SlotItem.CountBySlot")
	var n int64
	for _, row := range db.slotItems {
		if row.SlotID == slotID && (state == 0 || row.State == state) {
			n++
		}
	}
	return n, nil
}

func (db *fakeDB) FindSlotItemsByRef(_ context.Context, itemType, itemID string, limit int) ([]*model.SlotItem, error) {
	db.tick("SlotItem.FindByItemRef")
	if itemType == "" || itemID == "" {
		return nil, model.ErrItemRefRequired
	}
	if limit <= 0 {
		limit = model.DefaultSlotCapacity
	}
	var out []*model.SlotItem
	for _, row := range db.slotItems {
		if row.ItemType != itemType || row.ItemID != itemID || row.State != model.StateOn {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SlotID != out[j].SlotID {
			return out[i].SlotID < out[j].SlotID
		}
		return out[i].Position < out[j].Position
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- ClientSwitchModel ---

func (db *fakeDB) InsertSwitch(_ context.Context, sw *model.ClientSwitch) (int64, error) {
	db.tick("ClientSwitch.Insert")
	if err := checkSwitchForWrite(sw); err != nil {
		return 0, err
	}
	for _, row := range db.switches {
		if row.SwitchKey == sw.SwitchKey && row.Platform == sw.Platform {
			return 0, model.ErrSwitchConflict // uniq_key_platform
		}
	}
	cp := *sw
	if cp.SwitchID == 0 {
		cp.SwitchID = db.id()
	}
	db.switches[cp.SwitchID] = &cp
	sw.SwitchID = cp.SwitchID
	return cp.SwitchID, nil
}

// checkSwitchForWrite 复刻真 normalizeSwitchForWrite 的校验与默认值：
// 键格式、端必填、版本区间（点分版本只能按段比，见 ClientSwitch 注释）、
// Enabled 默认关、Version 默认 1。
func checkSwitchForWrite(sw *model.ClientSwitch) error {
	if sw.SwitchKey == "" || !model.ValidSwitchKey(sw.SwitchKey) {
		return model.ErrSwitchKeyRequired
	}
	if !model.ValidPlatform(sw.Platform) {
		return model.ErrPlatformRequired
	}
	for _, v := range []string{sw.MinVersion, sw.MaxVersion} {
		if v == "" {
			continue
		}
		if len([]rune(v)) > model.MaxAppVersionChars {
			return model.ErrAppVersionTooLong
		}
		if _, ok := model.CompareAppVersion(v, v); !ok {
			return model.ErrAppVersionRangeInvalid
		}
	}
	if sw.MinVersion != "" && sw.MaxVersion != "" {
		if c, ok := model.CompareAppVersion(sw.MinVersion, sw.MaxVersion); !ok || c > 0 {
			return model.ErrAppVersionRangeInvalid
		}
	}
	if sw.Enabled == 0 {
		sw.Enabled = model.StateOff // 默认关：新建即放量是最典型的误操作
	}
	if !model.ValidState(sw.Enabled) {
		return model.ErrRuleStateInvalid
	}
	if sw.Ctime == 0 {
		sw.Ctime = model.NowUnix()
	}
	sw.Mtime = sw.Ctime
	if sw.Version == 0 {
		sw.Version = 1
	}
	return nil
}

func (db *fakeDB) FindSwitchByID(_ context.Context, switchID int64) (*model.ClientSwitch, error) {
	db.tick("ClientSwitch.FindByID")
	row, ok := db.switches[switchID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (db *fakeDB) FindByKeyPlatform(_ context.Context, switchKey string, platform int32) (*model.ClientSwitch, error) {
	db.tick("ClientSwitch.FindByKeyPlatform")
	if switchKey == "" {
		return nil, model.ErrSwitchKeyRequired
	}
	if !model.ValidPlatform(platform) {
		return nil, model.ErrPlatformRequired
	}
	for _, row := range db.switches {
		if row.SwitchKey == switchKey && row.Platform == platform {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (db *fakeDB) UpdateSwitchWithVersion(_ context.Context, sw *model.ClientSwitch, expectVersion, ts int64) (bool, error) {
	db.tick("ClientSwitch.UpdateWithVersion")
	if sw.SwitchID <= 0 {
		return false, model.ErrSwitchNotFound
	}
	if err := checkSwitchForWrite(sw); err != nil {
		return false, err
	}
	// (switch_key, platform) 是唯一键：改键或改端可能撞到别人已有的行，
	// 真实现先探测一次，为的是把「冲突」与「版本被抢先」分开。
	for id, row := range db.switches {
		if row.SwitchKey == sw.SwitchKey && row.Platform == sw.Platform && id != sw.SwitchID {
			return false, model.ErrSwitchConflict
		}
	}
	row, ok := db.switches[sw.SwitchID]
	if !ok || row.Version != expectVersion {
		return false, nil
	}
	cp := *sw
	cp.Version = expectVersion + 1
	cp.Mtime = ts
	db.switches[cp.SwitchID] = &cp
	sw.Version, sw.Mtime = cp.Version, cp.Mtime
	return true, nil
}

func (db *fakeDB) ListSwitches(_ context.Context, f model.ClientSwitchFilter) ([]*model.ClientSwitch, int64, error) {
	db.tick("ClientSwitch.List")
	if err := db.failRead("ClientSwitch.List"); err != nil {
		return nil, 0, err
	}
	var all []*model.ClientSwitch
	for _, row := range db.switches {
		// SwitchKey 在真 SQL 里是 LIKE '%kw%' 而不是等值：后台搜索框要能前缀/中缀匹配。
		if f.Platform != 0 && row.Platform != f.Platform {
			continue
		}
		if f.SwitchKey != "" && !strings.Contains(row.SwitchKey, f.SwitchKey) {
			continue
		}
		if f.Enabled != 0 && row.Enabled != f.Enabled {
			continue
		}
		if f.ConfigID != 0 && row.ConfigID != f.ConfigID {
			continue
		}
		cp := *row
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].SwitchKey != all[j].SwitchKey {
			return all[i].SwitchKey < all[j].SwitchKey
		}
		return all[i].Platform < all[j].Platform // ORDER BY switch_key, platform
	})
	total := int64(len(all))
	return pageSlice(all, f.Pn, f.Ps), total, nil
}

func (db *fakeDB) ListForPlatform(_ context.Context, platform int32, limit int) ([]*model.ClientSwitch, error) {
	db.tick("ClientSwitch.ListForPlatform")
	if !model.ValidPlatform(platform) {
		return nil, model.ErrPlatformRequired
	}
	if limit <= 0 || limit > model.MaxSwitchListLimit {
		limit = model.MaxSwitchListLimit // 真 SQL 的 LIMIT 兜底，缺了就是一次无界扫描
	}
	out, _, err := db.ListSwitches(context.Background(), model.ClientSwitchFilter{
		Platform: platform, Pn: 1, Ps: int32(limit),
	})
	return out, err
}

func (db *fakeDB) BoundConfig(_ context.Context, switchID, configID, ts int64) (int64, error) {
	db.tick("ClientSwitch.BoundConfig")
	row, ok := db.switches[switchID]
	if !ok {
		return 0, nil
	}
	cp := *row
	cp.ConfigID = configID
	cp.Mtime = ts
	db.switches[switchID] = &cp
	return 1, nil
}

// --- 通用小工具 ---

func pageSlice[T any](all []T, pn, ps int32) []T {
	if ps <= 0 || pn <= 0 {
		return all
	}
	start := int(pn-1) * int(ps)
	if start >= len(all) {
		return nil
	}
	end := start + int(ps)
	if end > len(all) {
		end = len(all)
	}
	return all[start:end]
}

// --- 8 个 model 接口的适配器 --------------------------------------------
// fakeDB 用「唯一方法名」存数据，避免同一类型上的方法签名冲突；
// 下面的薄适配器只负责把接口方法名转发过去，断言逻辑一律留在 fakeDB 里。

type itemModel struct{ db *fakeDB }

func (m itemModel) Insert(ctx context.Context, item *model.ConfigItem) (int64, error) {
	return m.db.Insert(ctx, item)
}
func (m itemModel) FindOne(ctx context.Context, cfgKey, scope string) (*model.ConfigItem, error) {
	return m.db.FindOne(ctx, cfgKey, scope)
}
func (m itemModel) FindByID(ctx context.Context, configID int64) (*model.ConfigItem, error) {
	return m.db.FindByID(ctx, configID)
}
func (m itemModel) FindByKeys(ctx context.Context, cfgKeys []string, scope string) (map[string]*model.ConfigItem, error) {
	return m.db.FindByKeys(ctx, cfgKeys, scope)
}
func (m itemModel) AdvanceRelease(ctx context.Context, configID, expectLatest, newLatest, operatorID, ts int64) (bool, error) {
	return m.db.AdvanceRelease(ctx, configID, expectLatest, newLatest, operatorID, ts)
}
func (m itemModel) AdvanceReleaseTx(ctx context.Context, s sqlx.Session, configID, expectLatest,
	newLatest, operatorID, ts int64) (bool, error) {
	return m.db.AdvanceReleaseTx(ctx, s, configID, expectLatest, newLatest, operatorID, ts)
}
func (m itemModel) BumpEpoch(ctx context.Context, configIDs []int64, ts int64) (int64, error) {
	return m.db.BumpEpoch(ctx, configIDs, ts)
}
func (m itemModel) BumpAllEpoch(ctx context.Context, state int32, ts int64) (int64, error) {
	return m.db.BumpAllEpoch(ctx, state, ts)
}
func (m itemModel) List(ctx context.Context, f model.ConfigItemFilter) ([]*model.ConfigItem, int64, error) {
	return m.db.List(ctx, f)
}

type versionModel struct{ db *fakeDB }

func (m versionModel) Insert(ctx context.Context, v *model.ConfigVersion) (int64, error) {
	return m.db.InsertVersion(ctx, v)
}
func (m versionModel) InsertTx(ctx context.Context, s sqlx.Session, v *model.ConfigVersion) (int64, error) {
	return m.db.InsertVersionTx(ctx, s, v)
}
func (m versionModel) FindOne(ctx context.Context, configID, version int64) (*model.ConfigVersion, error) {
	return m.db.FindVersion(ctx, configID, version)
}
func (m versionModel) FindByRequestID(ctx context.Context, requestID string) (*model.ConfigVersion, error) {
	return m.db.FindVersionByRequestID(ctx, requestID)
}
func (m versionModel) FindLatest(ctx context.Context, configID int64) (*model.ConfigVersion, error) {
	return m.db.FindLatestVersion(ctx, configID)
}
func (m versionModel) FindVersions(ctx context.Context, configID int64, versions []int64) (map[int64]*model.ConfigVersion, error) {
	return m.db.FindVersions(ctx, configID, versions)
}
func (m versionModel) ListByConfig(ctx context.Context, configID int64, pn, ps int32) ([]*model.ConfigVersion, int64, error) {
	return m.db.ListVersions(ctx, configID, pn, ps)
}
func (m versionModel) MaxVersion(ctx context.Context, configID int64) (int64, error) {
	return m.db.MaxVersion(ctx, configID)
}
func (m versionModel) SetAuditEntry(ctx context.Context, versionID, entryID int64) (bool, error) {
	return m.db.SetAuditEntry(ctx, versionID, entryID)
}

type ruleModel struct{ db *fakeDB }

func (m ruleModel) Upsert(ctx context.Context, rule *model.RolloutRule) (int64, bool, error) {
	return m.db.UpsertRule(ctx, rule)
}
func (m ruleModel) UpsertTx(ctx context.Context, s sqlx.Session, rule *model.RolloutRule) (int64, bool, error) {
	return m.db.UpsertRuleTx(ctx, s, rule)
}
func (m ruleModel) FindByID(ctx context.Context, ruleID int64) (*model.RolloutRule, error) {
	return m.db.FindRuleByID(ctx, ruleID)
}
func (m ruleModel) FindByName(ctx context.Context, configID, version int64, name string) (*model.RolloutRule, error) {
	return m.db.FindRuleByName(ctx, configID, version, name)
}
func (m ruleModel) ListCandidates(ctx context.Context, configID, ts int64, limit int) ([]*model.RolloutRule, error) {
	return m.db.ListCandidates(ctx, configID, ts, limit)
}
func (m ruleModel) List(ctx context.Context, f model.RolloutRuleFilter) ([]*model.RolloutRule, int64, error) {
	return m.db.ListRules(ctx, f)
}
func (m ruleModel) SetState(ctx context.Context, ruleID int64, state, expectState int32, operatorID, ts int64) (bool, error) {
	return m.db.SetRuleState(ctx, ruleID, state, expectState, operatorID, ts)
}
func (m ruleModel) SetStateTx(ctx context.Context, s sqlx.Session, ruleID int64, state,
	expectState int32, operatorID, ts int64) (bool, error) {
	return m.db.SetRuleStateTx(ctx, s, ruleID, state, expectState, operatorID, ts)
}
func (m ruleModel) CountByVersion(ctx context.Context, configID, version int64, state int32) (int64, error) {
	return m.db.CountByVersion(ctx, configID, version, state)
}

type topicModel struct{ db *fakeDB }

func (m topicModel) Insert(ctx context.Context, topic *model.Topic) (int64, error) {
	return m.db.InsertTopic(ctx, topic)
}
func (m topicModel) FindByID(ctx context.Context, topicID int64) (*model.Topic, error) {
	return m.db.FindTopicByID(ctx, topicID)
}
func (m topicModel) FindBySlug(ctx context.Context, slug string) (*model.Topic, error) {
	return m.db.FindTopicBySlug(ctx, slug)
}
func (m topicModel) UpdateWithVersion(ctx context.Context, topic *model.Topic, expectVersion, ts int64) (bool, error) {
	return m.db.UpdateTopicWithVersion(ctx, topic, expectVersion, ts)
}
func (m topicModel) List(ctx context.Context, f model.TopicFilter) ([]*model.Topic, int64, error) {
	return m.db.ListTopics(ctx, f)
}
func (m topicModel) ListOnline(ctx context.Context, ts int64, limit int) ([]*model.Topic, error) {
	return m.db.ListOnlineTopics(ctx, ts, limit)
}
func (m topicModel) TouchMtime(ctx context.Context, topicID, ts int64) (int64, error) {
	return m.db.TouchTopicMtime(ctx, topicID, ts)
}

type topicItemModel struct{ db *fakeDB }

func (m topicItemModel) ReplaceAll(ctx context.Context, topicID int64, items []*model.TopicItem,
	operatorID, ts int64, maxItems int) (int64, error) {
	return m.db.ReplaceTopicItems(ctx, topicID, items, operatorID, ts, maxItems)
}
func (m topicItemModel) ListByTopic(ctx context.Context, topicID int64, state int32, limit int) ([]*model.TopicItem, error) {
	return m.db.ListTopicItems(ctx, topicID, state, limit)
}
func (m topicItemModel) CountByTopic(ctx context.Context, topicID int64, state int32) (int64, error) {
	return m.db.CountTopicItems(ctx, topicID, state)
}
func (m topicItemModel) FindByItemRef(ctx context.Context, itemType, itemID string, limit int) ([]*model.TopicItem, error) {
	return m.db.FindTopicItemsByRef(ctx, itemType, itemID, limit)
}

type slotModel struct{ db *fakeDB }

func (m slotModel) Insert(ctx context.Context, slot *model.RecommendSlot) (int64, error) {
	return m.db.InsertSlot(ctx, slot)
}
func (m slotModel) FindByID(ctx context.Context, slotID int64) (*model.RecommendSlot, error) {
	return m.db.FindSlotByID(ctx, slotID)
}
func (m slotModel) FindByCode(ctx context.Context, code string) (*model.RecommendSlot, error) {
	return m.db.FindSlotByCode(ctx, code)
}
func (m slotModel) UpdateWithVersion(ctx context.Context, slot *model.RecommendSlot, expectVersion, ts int64) (bool, error) {
	return m.db.UpdateSlotWithVersion(ctx, slot, expectVersion, ts)
}
func (m slotModel) List(ctx context.Context, f model.SlotFilter) ([]*model.RecommendSlot, int64, error) {
	return m.db.ListSlots(ctx, f)
}
func (m slotModel) ListEnabled(ctx context.Context, limit int) ([]*model.RecommendSlot, error) {
	return m.db.ListEnabledSlots(ctx, limit)
}

type slotItemModel struct{ db *fakeDB }

func (m slotItemModel) ReplaceAll(ctx context.Context, slotID int64, capacity int32, items []*model.SlotItem,
	operatorID, ts int64, maxItems int) (int64, error) {
	return m.db.ReplaceSlotItems(ctx, slotID, capacity, items, operatorID, ts, maxItems)
}
func (m slotItemModel) ListEffective(ctx context.Context, slotID, at int64, limit int) ([]*model.SlotItem, error) {
	return m.db.ListEffectiveSlotItems(ctx, slotID, at, limit)
}
func (m slotItemModel) ListAll(ctx context.Context, slotID int64, limit int) ([]*model.SlotItem, error) {
	return m.db.ListAllSlotItems(ctx, slotID, limit)
}
func (m slotItemModel) CountBySlot(ctx context.Context, slotID int64, state int32) (int64, error) {
	return m.db.CountSlotItems(ctx, slotID, state)
}
func (m slotItemModel) FindByItemRef(ctx context.Context, itemType, itemID string, limit int) ([]*model.SlotItem, error) {
	return m.db.FindSlotItemsByRef(ctx, itemType, itemID, limit)
}

type switchModel struct{ db *fakeDB }

func (m switchModel) Insert(ctx context.Context, sw *model.ClientSwitch) (int64, error) {
	return m.db.InsertSwitch(ctx, sw)
}
func (m switchModel) FindByID(ctx context.Context, switchID int64) (*model.ClientSwitch, error) {
	return m.db.FindSwitchByID(ctx, switchID)
}
func (m switchModel) FindByKeyPlatform(ctx context.Context, switchKey string, platform int32) (*model.ClientSwitch, error) {
	return m.db.FindByKeyPlatform(ctx, switchKey, platform)
}
func (m switchModel) UpdateWithVersion(ctx context.Context, sw *model.ClientSwitch, expectVersion, ts int64) (bool, error) {
	return m.db.UpdateSwitchWithVersion(ctx, sw, expectVersion, ts)
}
func (m switchModel) List(ctx context.Context, f model.ClientSwitchFilter) ([]*model.ClientSwitch, int64, error) {
	return m.db.ListSwitches(ctx, f)
}
func (m switchModel) ListForPlatform(ctx context.Context, platform int32, limit int) ([]*model.ClientSwitch, error) {
	return m.db.ListForPlatform(ctx, platform, limit)
}
func (m switchModel) BoundConfig(ctx context.Context, switchID, configID, ts int64) (int64, error) {
	return m.db.BoundConfig(ctx, switchID, configID, ts)
}

// 编译期钉死：每个适配器都完整实现对应接口，接口方法增减会直接编译失败。
var (
	_ model.ConfigItemModel    = itemModel{}
	_ model.ConfigVersionModel = versionModel{}
	_ model.RolloutRuleModel   = ruleModel{}
	_ model.TopicModel         = topicModel{}
	_ model.TopicItemModel     = topicItemModel{}
	_ model.RecommendSlotModel = slotModel{}
	_ model.SlotItemModel      = slotItemModel{}
	_ model.ClientSwitchModel  = switchModel{}
	_ svc.CacheKV              = (*fakeCache)(nil)
)
