package logic

// fakes_test.go 是 risk-control logic 测试的内存替身集合。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造 repository.New 只吃 *redis.Redis，离线跑必然 panic。因此本包用例统一走
// repository.NewWithDeps(内存 Redis 原语, 内存 6 个 model) 组装**真实 Repository**
// （真实 Cache + 真实 Counter + 真实 localRateGuard + 真实桶数学），再按
// svc.NewServiceContext 的口径把**真实 policy.Engine** 装到 ServiceContext 上。
// Repository 与 Engine 都不被 mock——否则「黑名单→处罚→白名单→兜底限流→规则→降级」
// 整条链就在被测路径之外了，logic 测试也就什么都证明不了。
//
// 四条替身纪律（catalog / rights / inbox 几轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 侧对返回结构的写回（如
//     repository.UpsertDeviceProfile 末尾的 saved.RelatedMidCount = count）不得污染库存行。
//  2. 写入按真实 SQL 的口径处理主键与 affected 行数：Insert 忽略入参 ID、自增分配并回填；
//     ON DUPLICATE KEY 的 1/2/0 三态、INSERT IGNORE 的静默重复都不做美化。
//     生产 SQL 不写的列（如 risk_list 更新时的 ctime、risk_device_profile 更新时的
//     related_mid_count）替身同样不写，避免「替身自动补齐」掩盖真实缺口。
//  3. 副作用按**顺序**记录（callLog，格式 <pkg>.<method>:<key>），断言序列而不是只断言次数：
//     risk-control 要紧的结论是「规则缓存命中还是回源、UpsertRule 之后有没有失效缓存、
//     Redis 计数读失败后有没有继续走 DB、降级路径有没有跳过 risk_check_log」。
//     时间戳/自增主键/桶号这类非确定性值不进精确序列，改用 wantCount + 读回断言。
//  4. 布数据一律走**不记 callLog** 的静默入口（seedRule/seedPunishment/warmJSON…），
//     所以轨迹断言可以直接从 0 开始数。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**（uniq_name / uniq_idempotency_key /
//     uniq(list_type,target_type,target_value) / uniq_device_hash / uniq_device_mid /
//     uniq_request_id、LEAST/GREATEST、INSERT IGNORE、ORDER BY、LIMIT/OFFSET），
//     不证明 SQL 文本与列名本身，那部分由 deploy/migrations/risk-control/*.sql 与集成环境负责；
//   - Redis 由 fakeRedis 按同语义复刻（MGET 缺失位返回空串、GET miss 报 redis.Nil、
//     SETNXEX 已存在返回 false、INCRBY+EXPIRE 同批），不验证真实网络故障形态。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/services/risk-control/internal/config"
	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/internal/repository"
	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"

	"github.com/zeromicro/go-zero/core/stores/redis"
)

// --- 断言小工具（与 catalog / rights / inbox 同口径） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：%v", label, err)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵（logic/repository 用 %w 包装下游错误是允许的）。
// 注入的错误一律先具名（boom := errors.New(...)）再断言，不拿 Unwrap 结果当期望值。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want errors.Is(..., %v)", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(..., %v)", label, err, want)
	}
}

// wantErrNil 断言确实没有错误（用于「错误不得被吞成 nil」的反向断言）。
func wantErrNil(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：不应出错，实际 %v", label, err)
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

func wantInt64s(t *testing.T, label, field string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：%s = %v, want %v", label, field, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s：%s[%d] = %d, want %d（完整 %v）", label, field, i, got[i], want[i], got)
		}
	}
}

// wantNoCall 断言 from 之后没有发生任何依赖调用（守卫必须发生在触库/触缓存之前）。
func wantNoCall(t *testing.T, label string, st *store, from int) {
	t.Helper()
	if ops := st.log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

// wantRangeInt64 用于时间边界这类只能给区间的断言（本服务时间戳来自真实时钟，不可注入）。
func wantRangeInt64(t *testing.T, label, field string, got, lo, hi int64) {
	t.Helper()
	if got < lo || got > hi {
		t.Errorf("%s：%s = %d, want ∈ [%d,%d]", label, field, got, lo, hi)
	}
}

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

func (c *callLog) snapshot() int             { return len(c.ops) }
func (c *callLog) opsFrom(from int) []string { return c.ops[from:] }
func (c *callLog) all() []string             { return c.ops }

func (c *callLog) countPrefix(prefix string) int {
	n := 0
	for _, o := range c.ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

// countPrefixFrom 只统计 from 之后的调用。同一 store 上先后跑两个入口时（例如
// 下发完再裁决），用它把断言限定在后一段轨迹上，否则前一段的同类调用会混进来。
func (c *callLog) countPrefixFrom(from int, prefix string) int {
	n := 0
	for _, o := range c.ops[from:] {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

// --- 错误注入（按方法粒度） ---

type faultInjector struct {
	by    map[string]error
	fromN map[string]int // 第几次调用起注入故障（1 起；缺省 1 = 每次都失败）
	calls map[string]int
}

func (f *faultInjector) init() {
	if f.by == nil {
		f.by = map[string]error{}
	}
	if f.fromN == nil {
		f.fromN = map[string]int{}
	}
	if f.calls == nil {
		f.calls = map[string]int{}
	}
}

func (f *faultInjector) failWith(method string, err error) {
	f.arm(method, 1, err)
}

// failFrom 从该方法第 from 次调用（含）起才返回故障。
// 用于「同一方法既做前置读又做写后读回」的入口：只有按调用序号注入，
// 才能单独钉住「UPDATE 已经落库、读回失败」这一段爆炸半径。
func (f *faultInjector) failFrom(method string, from int, err error) {
	f.arm(method, from, err)
}

func (f *faultInjector) arm(method string, from int, err error) {
	f.init()
	f.by[method] = err
	f.fromN[method] = from
}

func (f *faultInjector) fail(method string) error {
	err := f.by[method]
	if err == nil {
		return nil
	}
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[method]++
	if f.calls[method] < f.fromN[method] {
		return nil
	}
	return err
}

// clear 撤掉一个已注入的故障，用于「故障恢复后同一入口再次调用必须走通」这类用例。
func (f *faultInjector) clear(method string) {
	delete(f.by, method)
	delete(f.fromN, method)
}

// --- Redis 原语替身（同时实现 repository 的 cacheBackend 与 counterBackend） ---

// testWindowBuckets / testTiers 是本包用例统一的计数器配置，必须与 newStore 里
// 传给 repository.Config 的值一致；桶大小 tier/buckets 的算法与 repository.bucketSize 同式。
const (
	testWindowBuckets  = 6
	testDecisionCacheS = 60
	testRuleCacheS     = 30
)

func testTiers() []int64 { return []int64{60, 600, 3600} }

// testBucketSize 与 repository.bucketSize 逐字同式（tier/桶数，至少 1 秒）。
func testBucketSize(tier int64) int64 {
	if bs := tier / int64(testWindowBuckets); bs > 0 {
		return bs
	}
	return 1
}

// testCounterKey 与 repository 未导出的 counterKey/keyPrefixCounter 逐字对齐：
// 前缀或段序一旦漂移，MGET 就会读到 0 而不是布好的值，用例会立刻变红。
func testCounterKey(metric, subject string, action int32, tier, bucket int64) string {
	return "rc:c:" + metric + ":" + subject + ":" +
		strconv.FormatInt(int64(action), 10) + ":" +
		strconv.FormatInt(tier, 10) + ":" +
		strconv.FormatInt(bucket, 10)
}

// fakeRedis 只复刻 Redis 原语，不掺任何风控判断：档位选择、桶跨度、TTL 计算
// 全部由被测侧的真实 repository.Counter 完成。
type fakeRedis struct {
	log *callLog
	faultInjector

	kv         map[string]string // 缓存 JSON 串
	cacheTTL   map[string]int
	counters   map[string]int64 // INCRBY 语义
	counterTTL map[string]int
	events     map[string]bool // SETNXEX 去重标记
	eventTTL   map[string]int
}

func newFakeRedis(log *callLog) *fakeRedis {
	return &fakeRedis{
		log:        log,
		kv:         map[string]string{},
		cacheTTL:   map[string]int{},
		counters:   map[string]int64{},
		counterTTL: map[string]int{},
		events:     map[string]bool{},
		eventTTL:   map[string]int{},
	}
}

func (f *fakeRedis) Get(_ context.Context, key string) (string, error) {
	f.log.add("redis.Get:%s", key)
	if err := f.fail("Get"); err != nil {
		return "", err
	}
	v, ok := f.kv[key]
	if !ok {
		return "", redis.Nil // 与真实客户端同口径：miss 是 redis.Nil 而不是空串+nil
	}
	return v, nil
}

func (f *fakeRedis) Setex(_ context.Context, key, value string, ttlSeconds int) error {
	f.log.add("redis.Setex:%s", key)
	if err := f.fail("Setex"); err != nil {
		return err
	}
	f.kv[key] = value
	f.cacheTTL[key] = ttlSeconds
	return nil
}

func (f *fakeRedis) Del(_ context.Context, keys ...string) (int, error) {
	f.log.add("redis.Del:%s", strings.Join(keys, "+"))
	if err := f.fail("Del"); err != nil {
		return 0, err
	}
	var n int
	for _, k := range keys {
		if _, ok := f.kv[k]; ok {
			delete(f.kv, k)
			n++
		}
	}
	return n, nil
}

func (f *fakeRedis) IncrByBatch(_ context.Context, keys []string, delta int64, ttls []int) error {
	f.log.add("redis.IncrByBatch:%d", len(keys))
	if err := f.fail("IncrByBatch"); err != nil {
		return err
	}
	for i, k := range keys {
		f.counters[k] += delta
		f.counterTTL[k] = ttls[i]
	}
	return nil
}

func (f *fakeRedis) Mget(_ context.Context, keys ...string) ([]string, error) {
	f.log.add("redis.Mget:%d", len(keys))
	if err := f.fail("Mget"); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := f.counters[k]; ok {
			out = append(out, strconv.FormatInt(v, 10))
			continue
		}
		out = append(out, "") // Redis MGET：缺失位是 nil/空串，不是错误
	}
	return out, nil
}

func (f *fakeRedis) Setnxex(_ context.Context, key, value string, ttlSeconds int) (bool, error) {
	f.log.add("redis.Setnxex:%s", key)
	if err := f.fail("Setnxex"); err != nil {
		return false, err
	}
	if f.events[key] {
		return false, nil // 已存在：SETNX 失败但不算错误
	}
	f.events[key] = true
	f.eventTTL[key] = ttlSeconds
	return true, nil
}

func (f *fakeRedis) Ping() bool {
	f.log.add("redis.Ping")
	return f.fail("Ping") == nil
}

// --- 静默布数据（不记 callLog） ---

// warm 直接写缓存值：布景不算被测调用。
func (f *fakeRedis) warm(key, value string) { f.kv[key] = value }

// seedCounter 把 v 落到「当下」这个桶。真实 Counter 的读取跨度必然覆盖它：
// 窗口 >= 档位里 tier 的跨度，而桶号只会随时间前进，不会退出已算出的跨度。
func (f *fakeRedis) seedCounter(metric, subject string, action int32, tier, v int64) {
	bucket := time.Now().Unix() / testBucketSize(tier)
	f.counters[testCounterKey(metric, subject, action, tier, bucket)] = v
}

// counterSum 读回某 (metric,subject,action,tier) 下所有桶的合计（非确定性桶号不进序列）。
func (f *fakeRedis) counterSum(metric, subject string, action int32, tier int64) int64 {
	prefix := "rc:c:" + metric + ":" + subject + ":" + strconv.FormatInt(int64(action), 10) + ":" +
		strconv.FormatInt(tier, 10) + ":"
	var total int64
	for k, v := range f.counters {
		if strings.HasPrefix(k, prefix) {
			total += v
		}
	}
	return total
}

// --- model 替身：risk_rule ---

type fakeRuleModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.RiskRule
	names map[string]int64 // uniq_name
	next  int64
}

func newFakeRuleModel(log *callLog) *fakeRuleModel {
	return &fakeRuleModel{log: log, rows: map[int64]*model.RiskRule{}, names: map[string]int64{}}
}

func (f *fakeRuleModel) Insert(_ context.Context, r *model.RiskRule) (int64, error) {
	f.log.add("rule.Insert:%s", r.Name)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	if _, ok := f.names[r.Name]; ok { // 真实唯一索引冲突
		return 0, fmt.Errorf("%w: %s", model.ErrRuleNameDuplicated, r.Name)
	}
	f.next++
	cp := *r
	cp.RuleID = f.next
	now := time.Now().Unix()
	cp.Ctime, cp.Mtime = now, now // 真实 Insert 用 nowUnix()，不吃入参 ctime
	f.rows[f.next] = &cp
	f.names[cp.Name] = f.next
	*r = cp // 真实实现会把 LastInsertId 与时间回填到入参指针
	return f.next, nil
}

func (f *fakeRuleModel) Update(_ context.Context, r *model.RiskRule) error {
	f.log.add("rule.Update:%d", r.RuleID)
	if err := f.fail("Update"); err != nil {
		return err
	}
	row, ok := f.rows[r.RuleID]
	if !ok {
		return model.ErrRuleNotFound // 真实实现：RowsAffected==0 → ErrRuleNotFound
	}
	// UPDATE 语句不含 name/ctime：规则名与创建时间不可变更。
	row.ActionType, row.Metric, row.Op, row.Threshold = r.ActionType, r.Metric, r.Op, r.Threshold
	row.WindowSeconds, row.Decision, row.Priority, row.State = r.WindowSeconds, r.Decision, r.Priority, r.State
	row.Version, row.Operator = r.Version, r.Operator
	row.Mtime = time.Now().Unix()
	return nil
}

func (f *fakeRuleModel) FindOne(_ context.Context, ruleID int64) (*model.RiskRule, error) {
	f.log.add("rule.FindOne:%d", ruleID)
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

func (f *fakeRuleModel) FindByName(_ context.Context, name string) (*model.RiskRule, error) {
	f.log.add("rule.FindByName:%s", name)
	if err := f.fail("FindByName"); err != nil {
		return nil, err
	}
	id, ok := f.names[name]
	if !ok {
		return nil, nil
	}
	cp := *f.rows[id]
	return &cp, nil
}

// ListActiveByAction 复刻 WHERE state=1 AND action_type IN (0,action)
// ORDER BY priority DESC, rule_id ASC。
func (f *fakeRuleModel) ListActiveByAction(_ context.Context, action int32) ([]*model.RiskRule, error) {
	f.log.add("rule.ListActive:%d", action)
	if err := f.fail("ListActiveByAction"); err != nil {
		return nil, err
	}
	var out []*model.RiskRule
	for _, row := range f.rows {
		if row.State != model.StateEnabled || (row.ActionType != model.ActionAll && row.ActionType != action) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out, nil
}

func (f *fakeRuleModel) List(_ context.Context, action int32, metric string, state int32, offset, limit int) ([]*model.RiskRule, int32, error) {
	f.log.add("rule.List:%d/%s/%d/%d/%d", action, metric, state, offset, limit)
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	var ids []int64
	for id, row := range f.rows {
		if action > 0 && row.ActionType != model.ActionAll && row.ActionType != action {
			continue
		}
		if metric != "" && row.Metric != metric {
			continue
		}
		if (state == model.StateDisabled || state == model.StateEnabled) && row.State != state {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] }) // rule_id DESC
	total := int32(len(ids))
	if total == 0 {
		return nil, 0, nil // 真实实现：total=0 时不发第二条 SQL
	}
	var out []*model.RiskRule
	for _, id := range pageSlice(ids, offset, limit) {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, total, nil
}

func (f *fakeRuleModel) CountAll(_ context.Context) (int64, error) {
	f.log.add("rule.CountAll")
	if err := f.fail("CountAll"); err != nil {
		return 0, err
	}
	var n int64
	for _, row := range f.rows {
		if row.State == model.StateEnabled {
			n++
		}
	}
	return n, nil
}

// --- model 替身：risk_punishment ---

type fakePunishmentModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.RiskPunishment
	next int64
}

func newFakePunishmentModel(log *callLog) *fakePunishmentModel {
	return &fakePunishmentModel{log: log, rows: map[int64]*model.RiskPunishment{}}
}

// rowByKey 按 uniq_idempotency_key 扫描库存行（真实表就是这个唯一索引，没有第二份映射）：
// 主键升序保证同 key 多行时结果确定。
func (f *fakePunishmentModel) rowByKey(key string) (*model.RiskPunishment, bool) {
	var ids []int64
	for id, row := range f.rows {
		if row.IdempotencyKey == key {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, false
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return f.rows[ids[0]], true
}

// Insert 复刻 ON DUPLICATE KEY UPDATE punishment_id = LAST_INSERT_ID(punishment_id)：
// 命中幂等键唯一索引时返回既有行 ID、created=false（affected=0）。
// 与真实 SQL 一致：state 由入参决定，替身**不做** DEFAULT 1 的自动补齐 ——
// 生产 INSERT 显式绑定 p.State，所以 state 只能由 repository.ApplyPunishment 归一。
func (f *fakePunishmentModel) Insert(_ context.Context, p *model.RiskPunishment) (int64, bool, error) {
	f.log.add("punish.Insert:%s", p.IdempotencyKey)
	if err := f.fail("Insert"); err != nil {
		return 0, false, err
	}
	now := time.Now().Unix()
	p.Ctime, p.Mtime = now, now
	if existed, ok := f.rowByKey(p.IdempotencyKey); ok {
		p.PunishmentID = existed.PunishmentID
		return existed.PunishmentID, false, nil
	}
	f.next++
	cp := *p
	cp.PunishmentID = f.next
	f.rows[f.next] = &cp
	p.PunishmentID = f.next
	return f.next, true, nil
}

func (f *fakePunishmentModel) FindOne(_ context.Context, punishmentID int64) (*model.RiskPunishment, error) {
	f.log.add("punish.FindOne:%d", punishmentID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[punishmentID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakePunishmentModel) FindByIDempotencyKey(_ context.Context, key string) (*model.RiskPunishment, error) {
	f.log.add("punish.FindByKey:%s", key)
	if err := f.fail("FindByIDempotencyKey"); err != nil {
		return nil, err
	}
	row, ok := f.rowByKey(key)
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// ListActiveByMid 复刻 state=ACTIVE AND start_at<=now AND (end_at=0 OR end_at>now)
// ORDER BY scope ASC, punishment_id DESC。
func (f *fakePunishmentModel) ListActiveByMid(_ context.Context, mid, now int64) ([]*model.RiskPunishment, error) {
	f.log.add("punish.ListActive:%d", mid)
	if err := f.fail("ListActiveByMid"); err != nil {
		return nil, err
	}
	var out []*model.RiskPunishment
	for _, row := range f.rows {
		if row.Mid != mid || !activeAt(row, now) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].PunishmentID > out[j].PunishmentID
	})
	return out, nil
}

func activeAt(p *model.RiskPunishment, now int64) bool {
	return p.State == model.PunishmentStateActive && p.StartAt <= now && (p.EndAt == 0 || p.EndAt > now)
}

// Lift 复刻 UPDATE ... WHERE punishment_id=? AND state=ACTIVE：0 行 → 终态哨兵。
// 注意 repository 侧是 `err == model.ErrPunishmentAlreadyFinished` 直接比较，
// 所以替身必须返回未包装的哨兵，与真实 model 一致。
func (f *fakePunishmentModel) Lift(_ context.Context, punishmentID, operator int64, reason string) error {
	f.log.add("punish.Lift:%d", punishmentID)
	if err := f.fail("Lift"); err != nil {
		return err
	}
	row, ok := f.rows[punishmentID]
	if !ok || row.State != model.PunishmentStateActive {
		return model.ErrPunishmentAlreadyFinished
	}
	row.State = model.PunishmentStateLifted
	row.LiftOperator = operator
	row.LiftReason = reason
	row.Mtime = time.Now().Unix()
	return nil
}

// ExpireStale 复刻 UPDATE ... WHERE mid=? AND state=ACTIVE AND end_at>0 AND end_at<=now。
func (f *fakePunishmentModel) ExpireStale(_ context.Context, mid, now int64) (int64, error) {
	f.log.add("punish.ExpireStale:%d", mid)
	if err := f.fail("ExpireStale"); err != nil {
		return 0, err
	}
	var n int64
	for _, row := range f.rows {
		if row.Mid == mid && row.State == model.PunishmentStateActive && row.EndAt > 0 && row.EndAt <= now {
			row.State = model.PunishmentStateExpired
			row.Mtime = now
			n++
		}
	}
	return n, nil
}

func (f *fakePunishmentModel) List(_ context.Context, mid int64, scope, state int32, onlyActive bool, now int64, offset, limit int) ([]*model.RiskPunishment, int32, error) {
	f.log.add("punish.List:%d/%d/%d/%t/%d/%d", mid, scope, state, onlyActive, offset, limit)
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	var ids []int64
	for id, row := range f.rows {
		if mid > 0 && row.Mid != mid {
			continue
		}
		if scope > model.ActionAll && row.Scope != scope {
			continue
		}
		if state > 0 && row.State != state {
			continue
		}
		if onlyActive && !activeAt(row, now) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] }) // punishment_id DESC
	total := int32(len(ids))
	if total == 0 {
		return nil, 0, nil
	}
	var out []*model.RiskPunishment
	for _, id := range pageSlice(ids, offset, limit) {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, total, nil
}

// --- model 替身：risk_list ---

type fakeListModel struct {
	faultInjector
	log  *callLog
	rows map[string]*model.RiskList
	next int64
}

func newFakeListModel(log *callLog) *fakeListModel {
	return &fakeListModel{log: log, rows: map[string]*model.RiskList{}}
}

func listKey(listType, targetType int32, targetValue string) string {
	return strconv.FormatInt(int64(listType), 10) + ":" + strconv.FormatInt(int64(targetType), 10) + ":" + targetValue
}

func (f *fakeListModel) Upsert(_ context.Context, l *model.RiskList) (*model.RiskList, bool, error) {
	f.log.add("list.Upsert:%d/%d/%s", l.ListType, l.TargetType, l.TargetValue)
	if err := f.fail("Upsert"); err != nil {
		return nil, false, err
	}
	if err := l.Validate(); err != nil { // 真实 Upsert 第一步就是 Validate
		return nil, false, err
	}
	now := time.Now().Unix()
	key := listKey(l.ListType, l.TargetType, l.TargetValue)
	created := false
	row, ok := f.rows[key]
	if !ok {
		f.next++
		cp := *l
		cp.ID = f.next
		cp.Ctime = now
		cp.Mtime = now
		row = &cp
		f.rows[key] = row
		created = true
	} else {
		// ON DUPLICATE KEY UPDATE 只改 reason/operator/expire_at/state/mtime：ctime 与 id 保留。
		row.Reason, row.Operator, row.ExpireAt, row.State = l.Reason, l.Operator, l.ExpireAt, l.State
		row.Mtime = now
	}
	cp := *row
	return &cp, created, nil
}

func (f *fakeListModel) FindOne(_ context.Context, listType, targetType int32, targetValue string) (*model.RiskList, error) {
	f.log.add("list.FindOne:%d/%d/%s", listType, targetType, targetValue)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[listKey(listType, targetType, targetValue)]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// FindActive 复刻 state=1 AND (expire_at=0 OR expire_at>now) AND 命中给定 (type,value) 集合。
func (f *fakeListModel) FindActive(_ context.Context, now int64, pairs []model.TargetPair) ([]*model.RiskList, error) {
	f.log.add("list.FindActive:%d", len(pairs))
	if err := f.fail("FindActive"); err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	var out []*model.RiskList
	for _, row := range f.rows {
		if row.State != model.StateEnabled || (row.ExpireAt != 0 && row.ExpireAt <= now) {
			continue
		}
		for _, p := range pairs {
			if p.TargetType == row.TargetType && p.TargetValue == row.TargetValue {
				cp := *row
				out = append(out, &cp)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID }) // 稳定顺序，便于断言分流
	return out, nil
}

func (f *fakeListModel) List(_ context.Context, listType, targetType int32, targetValue string, state int32, offset, limit int) ([]*model.RiskList, int32, error) {
	f.log.add("list.List:%d/%d/%s/%d/%d/%d", listType, targetType, targetValue, state, offset, limit)
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	var ids []int64
	for _, row := range f.rows {
		if listType > 0 && row.ListType != listType {
			continue
		}
		if targetType > 0 && row.TargetType != targetType {
			continue
		}
		if targetValue != "" && row.TargetValue != targetValue {
			continue
		}
		if (state == model.StateDisabled || state == model.StateEnabled) && row.State != state {
			continue
		}
		ids = append(ids, row.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] }) // id DESC
	total := int32(len(ids))
	if total == 0 {
		return nil, 0, nil
	}
	var out []*model.RiskList
	for _, id := range pageSlice(ids, offset, limit) {
		for _, row := range f.rows {
			if row.ID == id {
				cp := *row
				out = append(out, &cp)
			}
		}
	}
	return out, total, nil
}

// --- model 替身：risk_device_profile / risk_device_mid ---

type fakeDeviceModel struct {
	faultInjector
	log  *callLog
	rows map[string]*model.RiskDeviceProfile
	next int64
}

func newFakeDeviceModel(log *callLog) *fakeDeviceModel {
	return &fakeDeviceModel{log: log, rows: map[string]*model.RiskDeviceProfile{}}
}

// Upsert 复刻 ON DUPLICATE KEY UPDATE labels/first_seen=LEAST/last_seen=GREATEST/
// risk_score/source/operator/mtime —— **不含 related_mid_count**，替身也不写它。
func (f *fakeDeviceModel) Upsert(_ context.Context, d *model.RiskDeviceProfile) (*model.RiskDeviceProfile, bool, error) {
	f.log.add("device.Upsert:%s", d.DeviceHash)
	if err := f.fail("Upsert"); err != nil {
		return nil, false, err
	}
	now := time.Now().Unix()
	if d.FirstSeen == 0 {
		d.FirstSeen = now
	}
	d.LastSeen, d.Ctime, d.Mtime = now, now, now
	created := false
	row, ok := f.rows[d.DeviceHash]
	if !ok {
		f.next++
		cp := *d
		cp.ID = f.next
		row = &cp
		f.rows[d.DeviceHash] = row
		created = true
	} else {
		row.Labels = d.Labels
		if d.FirstSeen < row.FirstSeen {
			row.FirstSeen = d.FirstSeen
		}
		if d.LastSeen > row.LastSeen {
			row.LastSeen = d.LastSeen
		}
		row.RiskScore, row.Source, row.Operator, row.Mtime = d.RiskScore, d.Source, d.Operator, d.Mtime
	}
	cp := *row
	*d = cp // 真实实现：*d = *fresh，返回的是行的**副本**，调用方改它不落库
	return d, created, nil
}

func (f *fakeDeviceModel) FindOne(_ context.Context, deviceHash string) (*model.RiskDeviceProfile, error) {
	f.log.add("device.FindOne:%s", deviceHash)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[deviceHash]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeDeviceModel) UpdateRelatedCount(_ context.Context, deviceHash string, count int64) error {
	f.log.add("device.UpdateRelatedCount:%s=%d", deviceHash, count)
	if err := f.fail("UpdateRelatedCount"); err != nil {
		return err
	}
	if row, ok := f.rows[deviceHash]; ok { // 行不存在：UPDATE 影响 0 行，真实实现也不报错
		row.RelatedMidCount = count
		row.Mtime = time.Now().Unix()
	}
	return nil
}

type fakeDeviceMidModel struct {
	faultInjector
	log  *callLog
	rels map[string]map[int64]bool // uniq_device_mid
}

func newFakeDeviceMidModel(log *callLog) *fakeDeviceMidModel {
	return &fakeDeviceMidModel{log: log, rels: map[string]map[int64]bool{}}
}

// AddRelation 复刻 INSERT IGNORE：已存在则影响 0 行、只刷 last_seen，返回 false。
func (f *fakeDeviceMidModel) AddRelation(_ context.Context, deviceHash string, mid int64) (bool, error) {
	f.log.add("mid.AddRelation:%s=%d", deviceHash, mid)
	if err := f.fail("AddRelation"); err != nil {
		return false, err
	}
	set := f.rels[deviceHash]
	if set == nil {
		set = map[int64]bool{}
		f.rels[deviceHash] = set
	}
	if set[mid] {
		return false, nil
	}
	set[mid] = true
	return true, nil
}

func (f *fakeDeviceMidModel) CountByDevice(_ context.Context, deviceHash string) (int64, error) {
	f.log.add("mid.CountByDevice:%s", deviceHash)
	if err := f.fail("CountByDevice"); err != nil {
		return 0, err
	}
	return int64(len(f.rels[deviceHash])), nil
}

// --- model 替身：risk_check_log（只写不读的审计表） ---

type fakeCheckLogModel struct {
	faultInjector
	log   *callLog
	rows  map[string]*model.RiskCheckLog
	order []string
	next  int64
}

func newFakeCheckLogModel(log *callLog) *fakeCheckLogModel {
	return &fakeCheckLogModel{log: log, rows: map[string]*model.RiskCheckLog{}}
}

// Insert 复刻 INSERT IGNORE INTO risk_check_log …（uniq_request_id）：
// 重复 request_id 静默保留首次记录，不报错、不覆盖。
func (f *fakeCheckLogModel) Insert(_ context.Context, l *model.RiskCheckLog) error {
	f.log.add("log.Insert:%s", l.RequestID)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	if _, ok := f.rows[l.RequestID]; ok {
		return nil
	}
	f.next++
	cp := *l
	cp.ID = f.next
	cp.Ctime = time.Now().Unix()
	f.rows[cp.RequestID] = &cp
	f.order = append(f.order, cp.RequestID)
	l.Ctime = cp.Ctime
	return nil
}

// --- 分页裁剪（真实 SQL 是 LIMIT ? OFFSET ?） ---

func pageSlice(ids []int64, offset, limit int) []int64 {
	if offset >= len(ids) {
		return nil
	}
	end := offset + limit
	if limit <= 0 || end > len(ids) {
		end = len(ids)
	}
	return ids[offset:end]
}

// --- 装配 ---

type storeOpts struct {
	localFallbackLimit int64
	challengeTTL       int64
	maxHits            int
	highRisk           []int32
	onDBFailureBlock   bool
	defaultWindow      int64
	ruleCacheSeconds   int64
	decisionCache      int64
}

type store struct {
	log      *callLog
	redis    *fakeRedis
	rule     *fakeRuleModel
	punish   *fakePunishmentModel
	device   *fakeDeviceModel
	mid      *fakeDeviceMidModel
	list     *fakeListModel
	checkLog *fakeCheckLogModel
	repo     *repository.Repository
	svcCtx   *svc.ServiceContext
	cfg      config.Config
}

type storeOption func(*storeOpts)

// withLocalFallback 打开进程内兜底限流（Redis 计数不可读时才会参与裁决）。
func withLocalFallback(limit int64) storeOption {
	return func(o *storeOpts) { o.localFallbackLimit = limit }
}

func withChallengeTTL(sec int64) storeOption {
	return func(o *storeOpts) { o.challengeTTL = sec }
}

func withMaxHits(n int) storeOption { return func(o *storeOpts) { o.maxHits = n } }

func withHighRiskActions(a ...int32) storeOption {
	return func(o *storeOpts) { o.highRisk = a }
}

// withOnDBFailureBlock 把非高危动作也切成 BLOCK-on-error。
func withOnDBFailureBlock() storeOption { return func(o *storeOpts) { o.onDBFailureBlock = true } }

func withDefaultWindow(sec int64) storeOption {
	return func(o *storeOpts) {
		o.defaultWindow = sec
	}
}

// newStore 用内存依赖组装**真实 Repository + 真实 Engine**，口径与 svc.NewServiceContext 一致。
func newStore(t *testing.T, opts ...storeOption) *store {
	t.Helper()
	o := storeOpts{
		localFallbackLimit: 0, // 默认关闭兜底限流，避免它干扰其它用例
		challengeTTL:       300,
		maxHits:            20,
		defaultWindow:      60,
		ruleCacheSeconds:   testRuleCacheS,
		decisionCache:      testDecisionCacheS,
	}
	for _, fn := range opts {
		fn(&o)
	}

	log := &callLog{}
	st := &store{
		log:      log,
		redis:    newFakeRedis(log),
		rule:     newFakeRuleModel(log),
		punish:   newFakePunishmentModel(log),
		device:   newFakeDeviceModel(log),
		mid:      newFakeDeviceMidModel(log),
		list:     newFakeListModel(log),
		checkLog: newFakeCheckLogModel(log),
	}
	st.repo = repository.NewWithDeps(st.redis, st.redis, repository.Config{
		CounterTiers:         testTiers(),
		WindowBuckets:        testWindowBuckets,
		DefaultWindowSeconds: o.defaultWindow,
		DecisionCacheSeconds: o.decisionCache,
		RuleCacheSeconds:     o.ruleCacheSeconds,
		LocalFallbackLimit:   o.localFallbackLimit,
	}, st.rule, st.punish, st.device, st.mid, st.list, st.checkLog)

	degrade := policy.DegradePolicy{
		HighRiskActions:    o.highRisk, // 空 → 引擎回落到代码默认（1/5/6/7）
		HighRiskDecision:   model.DecisionBlock,
		DefaultDecision:    model.DecisionAllow,
		LocalFallbackLimit: o.localFallbackLimit,
	}
	if o.onDBFailureBlock {
		degrade.DefaultDecision = model.DecisionBlock
	}
	// svc.policyConfig 的映射逐条复刻：ChallengeTTLSeconds / MaxHitRulesPerDecision / Degrade。
	engine := policy.NewEngine(st.repo, policy.Config{
		ChallengeTTLSeconds: o.challengeTTL,
		MaxHitsPerDecision:  o.maxHits,
		Degrade:             degrade,
	})
	st.svcCtx = &svc.ServiceContext{Repository: st.repo, Engine: engine}
	return st
}

// --- 静默布数据入口（不记 callLog，见文件头纪律 4） ---

func (st *store) seedRule(r model.RiskRule) int64 {
	st.rule.next++
	cp := r
	cp.RuleID = st.rule.next
	st.rule.rows[cp.RuleID] = &cp
	st.rule.names[cp.Name] = cp.RuleID
	return cp.RuleID
}

func (st *store) seedPunishment(p model.RiskPunishment) int64 {
	st.punish.next++
	cp := p
	cp.PunishmentID = st.punish.next
	if cp.State == 0 {
		cp.State = model.PunishmentStateActive
	}
	st.punish.rows[cp.PunishmentID] = &cp
	return cp.PunishmentID
}

func (st *store) seedListEntry(e model.RiskList) int64 {
	st.list.next++
	cp := e
	cp.ID = st.list.next
	st.list.rows[listKey(cp.ListType, cp.TargetType, cp.TargetValue)] = &cp
	return cp.ID
}

func (st *store) seedDevice(d model.RiskDeviceProfile) {
	cp := d
	if cp.ID == 0 {
		st.device.next++
		cp.ID = st.device.next
	}
	st.device.rows[cp.DeviceHash] = &cp
}

func (st *store) seedRelation(deviceHash string, mids ...int64) {
	set := st.mid.rels[deviceHash]
	if set == nil {
		set = map[int64]bool{}
		st.mid.rels[deviceHash] = set
	}
	for _, m := range mids {
		set[m] = true
	}
}

// punishmentRow / ruleState / checkLogRow 读回库内当前值（读的是库存行，不是副本）。
func (st *store) ruleState(ruleID int64) (int32, bool) {
	row, ok := st.rule.rows[ruleID]
	if !ok {
		return -1, false
	}
	return row.State, true
}

func (st *store) punishmentRow(id int64) model.RiskPunishment {
	if row, ok := st.punish.rows[id]; ok {
		return *row
	}
	return model.RiskPunishment{}
}

func (st *store) checkLogRow(requestID string) (model.RiskCheckLog, bool) {
	row, ok := st.checkLog.rows[requestID]
	if !ok {
		return model.RiskCheckLog{}, false
	}
	return *row, true
}

// --- 本包用例常用的受控标识 ---

const (
	// rawDeviceSN 是设备号**原文**：任何用例都不允许它出现在日志列、响应或缓存值里。
	rawDeviceSN = "RAW-DEVICE-SN-9527"
	// rawIPv4 / rawIPv6 只用于「必须被拒绝」的用例。
	rawIPv4 = "203.0.113.77"
)

// devHash / ipHashOf 是测试侧算好的受控标识。
func devHash() string { return model.DeviceHash(rawDeviceSN) }

// ipHashHex 是一个合法的 64 位十六进制预哈希值（不是裸 IP）。
const ipHashHex = "666f6f00000000000000000000000000000000000000000000000000deadbeef"

// subject 拼计数器主体标识，与 repository 未导出的 subjectMid/subjectDevice/subjectIP 同式。
func subjectOfMid(mid int64) string      { return "mid:" + strconv.FormatInt(mid, 10) }
func subjectOfDevice(hash string) string { return "dev:" + hash }
func subjectOfIP(hash string) string     { return "ip:" + hash }
