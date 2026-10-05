// 本文件是 spm logic 包的测试夹具：10 个 model 接口的内存假实现 + 事务/限流假件 +
// ServiceContext 装配。
//
// 刻意不连 MySQL、不起 gRPC、不用 time.Sleep：本服务的核心承诺（窗口幂等覆盖、
// 水位单调、口径 CAS、分页边界、限流显式失败、投影逐字段）全都是纯判定，
// 能在内存里钉死的东西没有理由推到集成测试。
//
// 假实现遵循的是 model 层注释里写明的 SQL 语义（uniq_metric 六列覆盖、
// ON DUPLICATE 的 event_time 守卫、水位 `last_closed_start <= ?` 条件推进、
// UpdateState 的 `WHERE state = fromState` CAS），而不是「永远成功」：
// 语义偏离真库的假件只会把 logic 的 bug 一起放行。
package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"go-video/common/ratelimit"
	"go-video/services/spm/internal/config"
	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// --- 固定入参 ---

// baseTS 取 2026-03-05T00:00:00Z 之后的整点，且刻意远离当前时间：
// 迟到判定、水位落后与 stale 的基准都是 time.Now()， fixture 用「相对 now」表达，
// 绝对常量只作为「远早于任何已闭合窗口」的历史起点。
const (
	dayUnix   int64 = 86400
	hourUnix  int64 = 3600
	minute5   int64 = 300
	tolerSecs int64 = 120
)

// testConfig 与 etc 示例配置同值（Validate 必须通过：假 svcCtx 不能代表一个非法服务端）。
func testConfig() config.Config {
	c := config.Config{
		DataSource: "spm:spm@tcp(127.0.0.1:3306)/go_video_spm?charset=utf8mb4",
		Spm: config.SpmConf{
			PageSize:                  20,
			MaxPageSize:               100,
			MaxMetricKeysPerRequest:   50,
			MaxWindowCount:            30,
			MaxWindowsPerJob:          2880,
			MaxWritePoints:            500,
			MaxRetentionDay:           90,
			InterestTopN:              20,
			MaxInterestTopN:           100,
			RealtimeWindowType:        int32(model.WindowType5Min),
			LateToleranceSeconds:      tolerSecs,
			InterestStaleAfterSeconds: 86400,
			WatermarkLagSeconds:       900,
			BehaviorRetentionDays:     180,
			ProjectionRetentionDays:   400,
			DeleteBatchSize:           2000,
			JobLeaseSeconds:           300,
			JobMaxRetry:               3,
			MaxRetryIntervalSeconds:   600,
			DeadLetterPreviewBytes:    512,
			ReadQps:                   2000,
			WriteQps:                  200,
			WriteBurst:                100,
		},
	}
	if err := c.Validate(); err != nil {
		panic("testConfig 不是合法的 spm 配置: " + err.Error())
	}
	return c
}

// errStub 是注入用的库错误（与 model 哨兵区分开，保证断言的是「错误被原样上抛」）。
var errStub = errors.New("spm/test: 模拟库故障")

// --- 假 ServiceContext ---

// deps 装配一套假依赖。Cache 保持 nil：shortCacheTTL 于是为 0，
// 缓存路径整体关闭，真值判定全部走内存 model（README「契约缺口」第 7 条的「配 0 即关闭」）。
type deps struct {
	ctx   *svc.ServiceContext
	db    *fakeDB
	read  *fakeLimiter
	write *fakeLimiter

	defs   *fakeDefinitions
	wins   *fakeWindows
	water  *fakeWatermarks
	ints   *fakeInterests
	rets   *fakeRetention
	jobs   *fakeJobs
	offs   *fakeOffsets
	dls    *fakeDeadLetters
	events *fakeEvents
	conts  *fakeContents
}

func newDeps(t *testing.T) *deps {
	t.Helper()
	db := newFakeDB()
	defs := &fakeDefinitions{db: db}
	wins := &fakeWindows{db: db, winState: &winState{}}
	water := &fakeWatermarks{db: db, wmState: &wmState{}}
	ints := &fakeInterests{db: db}
	rets := &fakeRetention{db: db}
	jobs := &fakeJobs{db: db}
	offs := &fakeOffsets{db: db}
	dls := &fakeDeadLetters{db: db}
	events := &fakeEvents{db: db}
	conts := &fakeContents{db: db}
	read := &fakeLimiter{}
	write := &fakeLimiter{}
	s := &svc.ServiceContext{
		Config:       testConfig(),
		DB:           &fakeConn{db: db},
		Events:       events,
		Definitions:  defs,
		Windows:      wins,
		Watermarks:   water,
		Interests:    ints,
		Retention:    rets,
		Jobs:         jobs,
		Contents:     conts,
		Offsets:      offs,
		DeadLetters:  dls,
		ReadLimiter:  read,
		WriteLimiter: write,
	}
	return &deps{
		ctx: s, db: db, read: read, write: write,
		defs: defs, wins: wins, water: water, ints: ints, rets: rets,
		jobs: jobs, offs: offs, dls: dls, events: events, conts: conts,
	}
}

// --- 限流假件 ---

// fakeLimiter 计数并可选择拒绝。拒绝时必须返回错误（logic 会归一成 ErrRateLimited），
// 而不是返回一个「之后什么也不做」的 done，否则限流形同虚设也测不出来。
type fakeLimiter struct {
	calls  int
	denies int // >0 时前 denies 次拒绝
}

func (l *fakeLimiter) Allow(ctx context.Context) (func(ratelimit.Op), error) {
	l.calls++
	if l.denies > 0 {
		l.denies--
		return nil, ratelimit.ErrLimitExceed
	}
	return func(ratelimit.Op) {}, nil
}

// --- 事务假件 ---

// fakeConn 只实现被测的 TransactCtx：成功即提交（内存库已写好），失败整库还原，
// 等价于 MySQL 的事务回滚。其余 sqlx 方法由内嵌 nil 接口提升，谁想用谁 panic。
type fakeConn struct {
	sqlx.SqlConn
	db *fakeDB
}

func (c *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.db.txRuns++
	snap := c.db.snapshot()
	if err := fn(ctx, &fakeSession{db: c.db}); err != nil {
		c.db.restore(snap)
		c.db.txRollbacks++
		return err
	}
	return nil
}

type fakeSession struct {
	sqlx.Session
	db *fakeDB
}

// --- 内存库 ---

type fakeDB struct {
	defs        map[string]*model.MetricDefinition // metric_key@vN
	windows     map[string]*model.MetricWindow     // uniq_metric 六列
	water       map[string]*model.WindowWatermark  // uniq_watermark 四列
	jobs        map[string]*model.AggregationJob   // uniq_request_id
	nextDef     int64
	nextWin     int64
	nextWm      int64
	nextJob     int64
	txRuns      int
	txRollbacks int
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		defs:    map[string]*model.MetricDefinition{},
		windows: map[string]*model.MetricWindow{},
		water:   map[string]*model.WindowWatermark{},
		jobs:    map[string]*model.AggregationJob{},
	}
}

type dbSnapshot struct {
	defs                              map[string]*model.MetricDefinition
	windows                           map[string]*model.MetricWindow
	water                             map[string]*model.WindowWatermark
	jobs                              map[string]*model.AggregationJob
	nextDef, nextWin, nextWm, nextJob int64
	txRuns, txRollbacks               int
}

func copyMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (db *fakeDB) snapshot() dbSnapshot {
	return dbSnapshot{
		defs: copyMap(db.defs), windows: copyMap(db.windows),
		water: copyMap(db.water), jobs: copyMap(db.jobs),
		nextDef: db.nextDef, nextWin: db.nextWin, nextWm: db.nextWm, nextJob: db.nextJob,
		txRuns: db.txRuns, txRollbacks: db.txRollbacks,
	}
}

func (db *fakeDB) restore(s dbSnapshot) {
	db.defs, db.windows, db.water, db.jobs = s.defs, s.windows, s.water, s.jobs
	db.nextDef, db.nextWin, db.nextWm, db.nextJob = s.nextDef, s.nextWin, s.nextWm, s.nextJob
	db.txRuns, db.txRollbacks = s.txRuns, s.txRollbacks
}

func defKey(metricKey string, version int32) string {
	return fmt.Sprintf("%s@v%d", metricKey, version)
}

func winKey(subjectType int32, subjectID int64, metricKey string, version, windowType int32,
	start int64) string {
	return fmt.Sprintf("%d:%d:%s:%d:%d:%d", subjectType, subjectID, metricKey, version,
		windowType, start)
}

func wmKey(subjectType int32, metricKey string, version, windowType int32) string {
	return fmt.Sprintf("%d:%s:%d:%d", subjectType, metricKey, version, windowType)
}

// --- 口径注册表 ---

// countStep 脚本化 CountActive：同一次状态迁移会在「迁移前 / 迁移后」各数一遍 ACTIVE，
// 并发赢家只在第二眼出现，所以两步必须能给出不同的值（或直接失败）。
type countStep struct {
	n   int64
	err error
}

type fakeDefinitions struct {
	model.MetricDefinitionModel
	db *fakeDB

	insertCalls     int
	updateCalls     []string // "key@vN:from->to"
	findActiveErr   error
	findKeyCalls    int              // FindByKeyVersion（显式版本点查）
	findActiveCalls int              // FindActive（ACTIVE 指针点查）
	countActive     map[string]int64 // 覆盖真实计数（模拟并发双 ACTIVE）
	countSeq        []countStep      // 按调用顺序给出 CountActive 结果/错误
	countCalls      int
	onUpdateState   func() // CAS 之前触发：模拟读后写之间被并发改掉状态
	findBlind       int    // 前 N 次 FindByKeyVersion 返回 nil（模拟并发窗口内读不到）
	listRows        []*model.MetricDefinition
	listTotal       *int64
	listCalls       int
	seenFts         []model.MetricDefinitionFilter
	errOn           string // "insert"/"update"/"list"/"count"/"countlist"/"find"
}

func (f *fakeDefinitions) InsertIfAbsent(_ context.Context, d *model.MetricDefinition) (bool, error) {
	f.insertCalls++
	if f.errOn == "insert" {
		return false, errStub
	}
	k := defKey(d.MetricKey, d.MetricVersion)
	if _, ok := f.db.defs[k]; ok {
		return false, nil
	}
	f.db.nextDef++
	row := *d
	row.ID = f.db.nextDef
	row.Ctime, row.Mtime = nowForTest(), nowForTest()
	f.db.defs[k] = &row
	return true, nil
}

func (f *fakeDefinitions) FindByKeyVersion(_ context.Context, metricKey string,
	version int32) (*model.MetricDefinition, error) {
	f.findKeyCalls++
	if f.errOn == "find" {
		return nil, errStub
	}
	if f.findBlind > 0 {
		f.findBlind--
		return nil, nil
	}
	row, ok := f.db.defs[defKey(metricKey, version)]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeDefinitions) FindActive(_ context.Context, metricKey string) (*model.MetricDefinition, error) {
	f.findActiveCalls++
	if f.findActiveErr != nil {
		return nil, f.findActiveErr
	}
	return f.activeRow(metricKey), nil
}

func (f *fakeDefinitions) activeRow(metricKey string) *model.MetricDefinition {
	var best *model.MetricDefinition
	for _, row := range f.db.defs {
		if row.MetricKey != metricKey || row.State != model.DefinitionStateActive {
			continue
		}
		if best == nil || row.MetricVersion < best.MetricVersion {
			cp := *row
			best = &cp
		}
	}
	return best
}

func (f *fakeDefinitions) FindByRequestID(_ context.Context, requestID string) (*model.MetricDefinition, error) {
	for _, row := range f.db.defs {
		if row.RequestID == requestID {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeDefinitions) CountActive(_ context.Context, metricKey string) (int64, error) {
	f.countCalls++
	if f.errOn == "count" {
		return 0, errStub
	}
	// 脚本化：同一键在一次调用的「迁移前 / 迁移后」被数两遍，并发赢家只在第二眼出现。
	if len(f.countSeq) > 0 {
		step := f.countSeq[0]
		f.countSeq = f.countSeq[1:]
		if step.err != nil {
			return 0, step.err
		}
		return step.n, nil
	}
	if v, ok := f.countActive[metricKey]; ok {
		return v, nil
	}
	var n int64
	for _, row := range f.db.defs {
		if row.MetricKey == metricKey && row.State == model.DefinitionStateActive {
			n++
		}
	}
	return n, nil
}

// UpdateState 复刻真库的 CAS：WHERE state = fromState。命中才写 toState 与 reason。
func (f *fakeDefinitions) UpdateState(_ context.Context, metricKey string,
	version, fromState, toState int32, reason string) (int64, error) {
	f.updateCalls = append(f.updateCalls,
		fmt.Sprintf("%s@v%d:%d->%d", metricKey, version, fromState, toState))
	if f.errOn == "update" {
		return 0, errStub
	}
	// 并发赢家的写入点：CAS 的条件判断仍然按真库语义执行，钩子只负责在条件之前改数据。
	if f.onUpdateState != nil {
		f.onUpdateState()
	}
	row, ok := f.db.defs[defKey(metricKey, version)]
	if !ok || row.State != fromState {
		return 0, nil
	}
	row.State = toState
	row.Description = reason
	row.Mtime = nowForTest()
	return 1, nil
}

func (f *fakeDefinitions) List(_ context.Context, ftr model.MetricDefinitionFilter) ([]*model.MetricDefinition, error) {
	f.listCalls++
	f.seenFts = append(f.seenFts, ftr)
	if f.errOn == "list" {
		return nil, errStub
	}
	return f.listRows, nil
}

func (f *fakeDefinitions) Count(_ context.Context, ftr model.MetricDefinitionFilter) (int64, error) {
	f.listCalls++
	f.seenFts = append(f.seenFts, ftr)
	if f.errOn == "countlist" {
		return 0, errStub
	}
	if f.listTotal != nil {
		return *f.listTotal, nil
	}
	return int64(len(f.listRows)), nil
}

// --- 窗口指标投影 ---

// winState 收集窗口假件的全部观测点与旋钮。WithSession 派生出的实例只多了一个
// 「我在事务内」的事实（对应 MySQL 的 session），计数必须记在同一份状态上——
// 否则事务内 upsert/advance 在测试里读不到，假件会把「没走事务」的 bug 一起放行。
type winState struct {
	upsertCalls   int
	upsertLate    []bool
	upsertRows    [][]*model.MetricWindow
	findOneCalls  int
	listHotCalls  int
	countHotCalls int
	latestCalls   int
	naturalCalls  int
	listKeysCalls int
	outsideTx     int // UpsertBatch 未走 WithSession 的调用次数（真契约只允许事务内写）

	findOneErr error
	upsertErr  error
	// listByNatErr / hotErr 对「回放判重」与「整段榜单查询（COUNT+LIST）」两条路径同时生效；
	// listHotErr 只影响 ListHot，用来钉「COUNT 成功、列表失败时不得回一张空榜」。
	listByNatErr error
	hotErr       error
	listHotErr   error
	listKeysErr  error
	latestStart  *int64 // 覆盖「兜底极值」返回值
	latestErr    error
	latestSeen   []int64 // 下传的 closedBefore（「只认已闭合窗口」的唯一证据）
	hotRows      []*model.HotSubject
	hotTotal     *int64
	countHotSeen []string
	listKeysRows []*model.MetricWindow
	listKeysSeen []string
}

type fakeWindows struct {
	model.MetricWindowModel
	*winState
	db      *fakeDB
	inTx    bool
	session sqlx.Session
}

func (f *fakeWindows) WithSession(session sqlx.Session) model.MetricWindowModel {
	if session == nil {
		return f
	}
	return &fakeWindows{winState: f.winState, db: f.db, inTx: true, session: session}
}

// UpsertBatch 复刻真库语义：入参校验逐条对齐 model.MetricWindowModel.UpsertBatch，
// 命中 uniq_metric 时整行覆盖（绝不累加），allowLate=false 时
// 只有「新 event_time 不早于既有值」才覆盖指标列。
func (f *fakeWindows) UpsertBatch(_ context.Context, rows []*model.MetricWindow, allowLate bool) (int64, error) {
	f.upsertCalls++
	f.upsertLate = append(f.upsertLate, allowLate)
	f.upsertRows = append(f.upsertRows, rows)
	if f.upsertErr != nil {
		return 0, f.upsertErr
	}
	if len(rows) > 500 {
		return 0, model.ErrTooManyPoints
	}
	seen := map[string]struct{}{}
	var affected int64
	for _, r := range rows {
		if !model.ValidSubjectType(r.SubjectType) || r.SubjectID <= 0 {
			return 0, model.ErrInvalidSubject
		}
		if strings.TrimSpace(r.MetricKey) == "" || r.MetricVersion <= 0 {
			return 0, model.ErrMetricVersionRequired
		}
		if !model.ValidWindowType(r.WindowType) {
			return 0, model.ErrInvalidWindow
		}
		if !model.ValidMetricSource(r.Source) {
			return 0, model.ErrInvalidMetricSource
		}
		if r.Numerator < 0 || r.Denominator < 0 || r.SampleCount < 0 {
			return 0, model.ErrNegativeMetricPoint
		}
		if r.EventTime <= 0 {
			return 0, model.ErrEventTimeRequired
		}
		r.WindowStart = model.AlignWindow(r.WindowStart, r.WindowType)
		k := winKey(r.SubjectType, r.SubjectID, r.MetricKey, r.MetricVersion, r.WindowType,
			r.WindowStart)
		if _, dup := seen[k]; dup {
			return 0, model.ErrDuplicatePointInBatch
		}
		seen[k] = struct{}{}
		if !f.inTx {
			// 真契约里指标行必须与水位同事务提交（Windows.WithSession）；
			// 事务外写一次就记一笔，供断言「没有绕过事务」。
			f.outsideTx++
		}
		existing, ok := f.db.windows[k]
		if !ok {
			f.db.nextWin++
			row := *r
			row.ID = f.db.nextWin
			row.Ctime = nowForTest()
			f.db.windows[k] = &row
			affected++
			continue
		}
		// ON DUPLICATE：write_request_id/mtime 无条件更新，指标列受 event_time 守卫。
		if allowLate || existing.EventTime <= r.EventTime {
			existing.MetricValue = r.MetricValue
			existing.Numerator = r.Numerator
			existing.Denominator = r.Denominator
			existing.SampleCount = r.SampleCount
			existing.Source = r.Source
			existing.EventTime = r.EventTime
		}
		existing.WriteReqID = r.WriteReqID
		existing.Mtime = nowForTest()
		affected++
	}
	return affected, nil
}

func (f *fakeWindows) FindOne(_ context.Context, subjectType int32, subjectID int64,
	metricKey string, version, windowType int32, start int64) (*model.MetricWindow, error) {
	f.findOneCalls++
	if f.findOneErr != nil {
		return nil, f.findOneErr
	}
	row, ok := f.db.windows[winKey(subjectType, subjectID, metricKey, version, windowType,
		model.AlignWindow(start, windowType))]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeWindows) ListByNaturalKeys(_ context.Context, rows []*model.MetricWindow) ([]*model.MetricWindow, error) {
	f.naturalCalls++
	if f.listByNatErr != nil {
		return nil, f.listByNatErr
	}
	var out []*model.MetricWindow
	for _, r := range rows {
		if row, ok := f.db.windows[winKey(r.SubjectType, r.SubjectID, r.MetricKey,
			r.MetricVersion, r.WindowType, model.AlignWindow(r.WindowStart, r.WindowType))]; ok {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeWindows) ListByKeyWindows(_ context.Context, subjectType int32, subjectID int64,
	windowType int32, keys []model.MetricKeyVersion, starts []int64) ([]*model.MetricWindow, error) {
	f.listKeysCalls++
	f.listKeysSeen = append(f.listKeysSeen, fmt.Sprintf("%d:%d:%d:%v:%v", subjectType, subjectID,
		windowType, keys, starts))
	if f.listKeysErr != nil {
		return nil, f.listKeysErr
	}
	if len(f.listKeysRows) > 0 {
		return f.listKeysRows, nil
	}
	var out []*model.MetricWindow
	for _, k := range keys {
		for _, s := range starts {
			if row, ok := f.db.windows[winKey(subjectType, subjectID, k.MetricKey, k.Version,
				windowType, model.AlignWindow(s, windowType))]; ok {
				cp := *row
				out = append(out, &cp)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WindowStart != out[j].WindowStart {
			return out[i].WindowStart < out[j].WindowStart
		}
		return out[i].MetricKey < out[j].MetricKey
	})
	return out, nil
}

func (f *fakeWindows) FindLatestWindowStart(_ context.Context, subjectType int32,
	metricKey string, version, windowType int32, closedBefore int64) (int64, error) {
	f.latestCalls++
	f.latestSeen = append(f.latestSeen, closedBefore)
	if f.latestErr != nil {
		return 0, f.latestErr
	}
	if f.latestStart != nil {
		return *f.latestStart, nil
	}
	var best int64
	for _, row := range f.db.windows {
		if row.SubjectType != subjectType || row.MetricKey != metricKey ||
			row.MetricVersion != version || row.WindowType != windowType {
			continue
		}
		if closedBefore > 0 && row.WindowStart > closedBefore {
			continue
		}
		if row.WindowStart > best {
			best = row.WindowStart
		}
	}
	return best, nil
}

func (f *fakeWindows) CountHot(_ context.Context, subjectType int32, metricKey string,
	version, windowType int32, windowStart, zoneID int64) (int64, error) {
	f.countHotCalls++
	f.countHotSeen = append(f.countHotSeen, fmt.Sprintf("%d:%s@v%d:%d:%d:zone=%d",
		subjectType, metricKey, version, windowType, windowStart, zoneID))
	if f.hotErr != nil {
		return 0, f.hotErr
	}
	if f.hotTotal != nil {
		return *f.hotTotal, nil
	}
	return int64(len(f.hotRows)), nil
}

func (f *fakeWindows) ListHot(_ context.Context, subjectType int32, metricKey string,
	version, windowType int32, windowStart, zoneID int64, offset, limit int32) ([]*model.HotSubject, error) {
	f.listHotCalls++
	if f.listHotErr != nil {
		return nil, f.listHotErr
	}
	if f.hotErr != nil {
		return nil, f.hotErr
	}
	if int64(offset) >= int64(len(f.hotRows)) {
		return nil, nil
	}
	end := int(offset) + int(limit)
	if end > len(f.hotRows) {
		end = len(f.hotRows)
	}
	out := make([]*model.HotSubject, 0, end-int(offset))
	for _, r := range f.hotRows[int(offset):end] {
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

// --- 水位 ---

// wmState 同 winState：事务内派生实例的推进记录必须与基座共享。
type wmState struct {
	advanceCalls   int
	advanceApplied []bool
	findCalls      int
	listKeysCalls  int

	findErr     error
	listKeysErr error // 只作用于 ListByMetricKeys（批量水位查询）
	advErr      error
	findSeed    map[string]*model.WindowWatermark // 覆盖库里没有的行（模拟并发/脏数据）
	listKeys    []*model.WindowWatermark
}

type fakeWatermarks struct {
	model.WindowWatermarkModel
	*wmState
	db      *fakeDB
	inTx    bool
	session sqlx.Session
}

func (f *fakeWatermarks) WithSession(session sqlx.Session) model.WindowWatermarkModel {
	if session == nil {
		return f
	}
	return &fakeWatermarks{wmState: f.wmState, db: f.db, inTx: true, session: session}
}

// Advance 复刻真库单调推进：只在 last_closed_start <= 新值时生效，
// 回退返回 applied=false（幂等拒绝，不是错误）；TOTAL 与未规整边界直接报错。
func (f *fakeWatermarks) Advance(_ context.Context, w *model.WindowWatermark) (bool, error) {
	f.advanceCalls++
	if w == nil {
		return false, model.ErrInvalidWindow
	}
	if strings.TrimSpace(w.MetricKey) == "" {
		return false, model.ErrMetricKeyEmpty
	}
	if w.MetricVersion <= 0 {
		return false, model.ErrMetricVersionRequired
	}
	if !model.ValidWindowType(w.WindowType) || w.WindowType == model.WindowTypeTotal {
		return false, model.ErrInvalidWindow
	}
	if w.LastClosedStart != model.AlignWindow(w.LastClosedStart, w.WindowType) {
		return false, model.ErrInvalidWindow
	}
	if w.LastEventTime <= 0 {
		return false, model.ErrEventTimeRequired
	}
	if f.advErr != nil {
		return false, f.advErr
	}
	k := wmKey(w.SubjectType, w.MetricKey, w.MetricVersion, w.WindowType)
	existing, ok := f.db.water[k]
	if seed, seeded := f.findSeed[k]; seeded {
		existing, ok = seed, true
	}
	if ok {
		if existing.LastClosedStart > w.LastClosedStart {
			f.advanceApplied = append(f.advanceApplied, false)
			return false, nil
		}
		cp := *w
		existing.LastClosedStart = cp.LastClosedStart
		existing.LastEventTime = cp.LastEventTime
		existing.RowsWritten = cp.RowsWritten
		existing.UpdateRequestID = cp.UpdateRequestID
		existing.Mtime = nowForTest()
		f.advanceApplied = append(f.advanceApplied, true)
		return true, nil
	}
	f.db.nextWm++
	row := *w
	row.ID = f.db.nextWm
	row.Ctime, row.Mtime = nowForTest(), nowForTest()
	f.db.water[k] = &row
	f.advanceApplied = append(f.advanceApplied, true)
	return true, nil
}

func (f *fakeWatermarks) Find(_ context.Context, subjectType int32, metricKey string,
	version, windowType int32) (*model.WindowWatermark, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	k := wmKey(subjectType, metricKey, version, windowType)
	if seed, ok := f.findSeed[k]; ok {
		cp := *seed
		return &cp, nil
	}
	row, ok := f.db.water[k]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeWatermarks) ListByMetricKeys(_ context.Context, subjectType int32,
	windowType int32, keys []model.MetricKeyVersion) ([]*model.WindowWatermark, error) {
	f.listKeysCalls++
	if f.listKeysErr != nil {
		return nil, f.listKeysErr
	}
	if len(f.listKeys) > 0 {
		return f.listKeys, nil
	}
	var out []*model.WindowWatermark
	for _, k := range keys {
		if row, ok := f.db.water[wmKey(subjectType, k.MetricKey, k.Version, windowType)]; ok {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out, nil
}

// --- 兴趣 / 留存 ---

type fakeInterests struct {
	model.UserInterestModel
	db *fakeDB

	listTopCalls int
	rows         []*model.UserInterest
	err          error
	seenTopN     []int32
}

func (f *fakeInterests) ListTop(_ context.Context, mid int64, version, topN int32) ([]*model.UserInterest, error) {
	f.listTopCalls++
	f.seenTopN = append(f.seenTopN, topN)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*model.UserInterest, 0, len(f.rows))
	for _, r := range f.rows {
		cp := *r
		cp.Mid, cp.MetricVersion = mid, version
		out = append(out, &cp)
	}
	return out, nil
}

type fakeRetention struct {
	model.RetentionCohortModel
	db *fakeDB

	listCurveCalls int
	rows           []*model.RetentionCohort
	err            error
	seenMaxDay     []int32
	seenDates      []int64
}

func (f *fakeRetention) ListCurve(_ context.Context, cohortType int32, cohortDate, zoneID int64,
	version, maxDay int32) ([]*model.RetentionCohort, error) {
	f.listCurveCalls++
	f.seenMaxDay = append(f.seenMaxDay, maxDay)
	f.seenDates = append(f.seenDates, cohortDate)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*model.RetentionCohort, 0, len(f.rows))
	for _, r := range f.rows {
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

// --- 作业 ---

type fakeJobs struct {
	model.AggregationJobModel
	db *fakeDB

	insertCalls int
	insertSeen  []*model.AggregationJob
	created     *bool // 覆盖 InsertIfAbsent 的返回值（模拟「插成功却读不到」）
	readBackNil bool
	errOnInsert bool
	errOnFind   bool
	listRows    []*model.AggregationJob
	listTotal   *int64
	listCalls   int
	seenFts     []model.JobFilter
	errOnList   bool
	// 两个定位入口各记一次：GetAggregationJob 必须能被 job_id 或 request_id 单独驱动，
	// 计数是「走的是哪条索引」的唯一证据（真库分别是主键与 uniq_request_id）。
	findByIDCalls  int
	findByReqCalls int
}

// InsertIfAbsent 复刻真库语义：uniq_request_id 命中时 affected=0 返回 created=false，
// 并且只回填 ctime/mtime/state —— 主键 id 不回填（真库 INSERT ... ON DUPLICATE 之后
// 不去取 LastInsertId），所以 logic 必须回读一次才能拿到 job_id。
func (f *fakeJobs) InsertIfAbsent(_ context.Context, j *model.AggregationJob) (bool, error) {
	f.insertCalls++
	cp := *j
	f.insertSeen = append(f.insertSeen, &cp)
	if f.errOnInsert {
		return false, errStub
	}
	if !model.ValidJobType(j.JobType) {
		return false, model.ErrInvalidJobType
	}
	if strings.TrimSpace(j.RequestID) == "" {
		return false, model.ErrRequestIdRequired
	}
	if !model.ValidWindowType(j.WindowType) {
		return false, model.ErrInvalidWindow
	}
	if j.JobType != model.JobTypeRealtime && j.MetricKey != "" && j.MetricVersion <= 0 {
		return false, model.ErrMetricVersionRequired
	}
	if _, ok := f.db.jobs[j.RequestID]; ok {
		if f.created != nil {
			return *f.created, nil
		}
		return false, nil
	}
	f.db.nextJob++
	row := *j
	row.ID = f.db.nextJob
	now := nowForTest()
	row.Ctime, row.Mtime, row.State = now, now, model.JobStatePending
	f.db.jobs[row.RequestID] = &row
	// 与真库一致：调用方结构体拿到时间戳，但拿不到主键。
	j.Ctime, j.Mtime, j.State = now, now, model.JobStatePending
	return true, nil
}

func (f *fakeJobs) FindByRequestID(_ context.Context, requestID string) (*model.AggregationJob, error) {
	f.findByReqCalls++
	if f.errOnFind {
		return nil, errStub
	}
	if f.readBackNil {
		return nil, nil
	}
	row, ok := f.db.jobs[requestID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeJobs) FindByID(_ context.Context, id int64) (*model.AggregationJob, error) {
	f.findByIDCalls++
	if f.errOnFind {
		return nil, errStub
	}
	for _, row := range f.db.jobs {
		if row.ID == id {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeJobs) List(_ context.Context, ft model.JobFilter) ([]*model.AggregationJob, error) {
	f.listCalls++
	f.seenFts = append(f.seenFts, ft)
	if f.errOnList {
		return nil, errStub
	}
	return f.listRows, nil
}

func (f *fakeJobs) Count(_ context.Context, ft model.JobFilter) (int64, error) {
	f.listCalls++
	f.seenFts = append(f.seenFts, ft)
	if f.errOnList {
		return 0, errStub
	}
	if f.listTotal != nil {
		return *f.listTotal, nil
	}
	return int64(len(f.listRows)), nil
}

// --- 消费状态 / 死信 ---

type fakeOffsets struct {
	model.ConsumerOffsetModel
	db *fakeDB

	summarizeCalls int
	countCalls     int
	rows           []*model.ConsumerSummary
	total          *int64
	err            error
	seenSince      []int64
	seenStates     []string
	seenTopic      []string // 下传的 topic；COUNT 与 SUMMARIZE 各记一次，用来证明两条 SQL 同条件
	seenOffset     []int32  // 下传的分页偏移
	seenLimit      []int32  // 下传的 LIMIT（有界聚合的界）
}

func (f *fakeOffsets) Summarize(_ context.Context, topic string, states []string, since int64,
	offset, limit int32) ([]*model.ConsumerSummary, error) {
	f.summarizeCalls++
	f.seenTopic = append(f.seenTopic, topic)
	f.seenSince = append(f.seenSince, since)
	f.seenStates = append(f.seenStates, strings.Join(states, ","))
	f.seenOffset = append(f.seenOffset, offset)
	f.seenLimit = append(f.seenLimit, limit)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeOffsets) CountByState(_ context.Context, topic string, states []string,
	since int64) (int64, error) {
	f.countCalls++
	f.seenTopic = append(f.seenTopic, topic)
	f.seenSince = append(f.seenSince, since)
	f.seenStates = append(f.seenStates, strings.Join(states, ","))
	if f.err != nil {
		return 0, f.err
	}
	if f.total != nil {
		return *f.total, nil
	}
	return int64(len(f.rows)), nil
}

type fakeDeadLetters struct {
	model.DeadLetterModel
	db *fakeDB

	listCalls int
	rows      []*model.DeadLetter
	total     *int64
	err       error
	seen      []model.DeadLetterFilter
}

func (f *fakeDeadLetters) List(_ context.Context, ft model.DeadLetterFilter) ([]*model.DeadLetter, error) {
	f.listCalls++
	f.seen = append(f.seen, ft)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeDeadLetters) Count(_ context.Context, ft model.DeadLetterFilter) (int64, error) {
	f.listCalls++
	f.seen = append(f.seen, ft)
	if f.err != nil {
		return 0, f.err
	}
	if f.total != nil {
		return *f.total, nil
	}
	return int64(len(f.rows)), nil
}

// --- 未被任何 logic 调用的两个 model（存在即必须是 nil 接口，调用即 panic）---

type fakeEvents struct {
	model.BehaviorEventModel
	db *fakeDB
}

type fakeContents struct {
	model.ContentProjectionModel
	db *fakeDB
}

// --- 断言辅助 ---

// timeNow 与 logic 内部用的是同一个时钟源（logic 直接调 time.Now，本包不假装能注入）。
var timeNow = time.Now

// nowForTest 只提供「时间已经推进」的单调戳记，logic 的判定基准仍是它自己的 time.Now()。
func nowForTest() int64 {
	return timeNow().Unix()
}

// seedDef 直接往内存库放一行指定状态的口径（绕过 logic 的登记入口），
// 用于状态机与读路径测试：要测「RETIRED 不能复活」，就得先有一行 RETIRED。
func seedDef(t *testing.T, d *deps, key string, version, state int32) *model.MetricDefinition {
	t.Helper()
	return seedDefSpec(t, d, key, version, state, "1,2,3", "behavior.play")
}

func seedDefSpec(t *testing.T, d *deps, key string, version, state int32,
	windows, events string) *model.MetricDefinition {
	t.Helper()
	created, err := d.defs.InsertIfAbsent(context.Background(), &model.MetricDefinition{
		MetricKey: key, MetricVersion: version, Name: key + " 展示名",
		Formula: "count(distinct event_id)", Unit: "count", SupportedWindows: windows,
		SourceEventTypes: events, State: state, Description: "预置",
		CreatedBy: "tester", RequestID: "req-seed-" + defKey(key, version),
	})
	if err != nil || !created {
		t.Fatalf("预置口径 %s@v%d: %v %v", key, version, created, err)
	}
	cp := *d.db.defs[defKey(key, version)]
	return &cp
}

func mustActiveDef(t *testing.T, d *deps, key string, version int32, windows string) *model.MetricDefinition {
	t.Helper()
	seedDefSpec(t, d, key, version, model.DefinitionStateDraft, windows, "behavior.play")
	if _, err := d.defs.UpdateState(context.Background(), key, version,
		model.DefinitionStateDraft, model.DefinitionStateActive, "评审通过"); err != nil {
		t.Fatalf("预置 ACTIVE 失败: %v", err)
	}
	cp := *d.db.defs[defKey(key, version)]
	if cp.State != model.DefinitionStateActive {
		t.Fatalf("预置后 state=%d，不是 ACTIVE", cp.State)
	}
	return &cp
}

// seedWindow 直接往内存库放一行已落库的窗口投影（绕过 logic，只测读路径）。
func seedWindow(d *deps, subjectType int32, subjectID int64, metricKey string,
	version, windowType int32, start int64, value float64, reqID string) *model.MetricWindow {
	k := winKey(subjectType, subjectID, metricKey, version, windowType,
		model.AlignWindow(start, windowType))
	d.db.nextWin++
	row := &model.MetricWindow{
		ID: d.db.nextWin, SubjectType: subjectType, SubjectID: subjectID, MetricKey: metricKey,
		MetricVersion: version, WindowType: windowType, WindowStart: model.AlignWindow(start, windowType),
		MetricValue: value, Numerator: int64(value), Denominator: int64(value) * 2,
		SampleCount: int64(value), Source: model.MetricSourceRealtime,
		EventTime: nowForTest(), WriteReqID: reqID, Ctime: nowForTest(), Mtime: nowForTest(),
	}
	d.db.windows[k] = row
	return row
}

func seedWatermark(d *deps, subjectType int32, metricKey string, version, windowType int32,
	lastClosed, lastEvent int64) {
	d.db.nextWm++
	d.db.water[wmKey(subjectType, metricKey, version, windowType)] = &model.WindowWatermark{
		ID: d.db.nextWm, SubjectType: subjectType, MetricKey: metricKey, MetricVersion: version,
		WindowType: windowType, LastClosedStart: lastClosed, LastEventTime: lastEvent,
		RowsWritten: 1, UpdateRequestID: "seed", Ctime: nowForTest(), Mtime: nowForTest(),
	}
}

func storedWindow(t *testing.T, d *deps, subjectType int32, subjectID int64, metricKey string,
	version, windowType int32, start int64) *model.MetricWindow {
	t.Helper()
	row, ok := d.db.windows[winKey(subjectType, subjectID, metricKey, version, windowType,
		model.AlignWindow(start, windowType))]
	if !ok {
		t.Fatalf("窗口未落库: %s", winKey(subjectType, subjectID, metricKey, version, windowType, start))
	}
	return row
}

// validPoint 构造一条完全合法的写入点（版本/主体/粒度/事件时间都给全）。
func validPoint(metricKey string, version int32, windowType int32, start int64) *rpc.MetricPoint {
	return &rpc.MetricPoint{
		MetricKey: metricKey, MetricVersion: version,
		SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, SubjectId: 101,
		WindowType: rpc.WindowType(windowType), WindowStart: start,
		Value: 0.8, Numerator: 8, Denominator: 10, SampleCount: 10,
		EventTime: start + 1,
	}
}

// alignedStart5Min 返回「相对 now 的第 n 个已闭合 5 分钟窗口」左边界。
func alignedStart5Min(offsetWindows int64) int64 {
	now := timeNow().Unix()
	return model.AlignWindow(now-minute5, model.WindowType5Min) + offsetWindows*minute5
}

func requireErrIs(t *testing.T, err, target error, msg string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s：期望 %v，实得 %v", msg, target, err)
	}
}
