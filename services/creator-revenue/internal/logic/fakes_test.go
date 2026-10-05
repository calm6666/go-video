package logic

// 本文件提供 creator-revenue logic 单测用的「内存版 cr_* 表」。
//
// 为什么必须做到 SQL 这一层（以及为什么这不是伪装）：
//   - 本服务的写侧口径全部押在一个事务里（applyMetricInTx / applySettlementInTx /
//     applyRuleDraftInTx / applyRuleStateChange / applyEnrollmentTransition），
//     事务内用 model.NewXxxModel(sqlx.NewSqlConnFromSession(tx)) 绑定会话，
//     所以只替换 svc.ServiceContext 的 model 字段无法进入这些被测函数；
//   - 于是这里实现 sqlx.Session：按语句签名把请求落到内存表上，并且**复刻真库的判定语义**：
//     uniq_mid / uniq_rule_code / uniq_metric_key / uniq_active_period_mid /
//     uniq_request_id / uniq_no_source 命中即回可被 model.IsDuplicateErr 识别的 1062 错误；
//     CAS 语句按真实 WHERE 条件求值，不命中即 RowsAffected=0（DSN 禁止 clientFoundRows，
//     logic 就是把「0 行」当成并发失败来裁决的，见 model.rowsAffected）；
//     cr_enrollment.Transition 的 IF(? > 0, ?, 原值) 三态时间戳语义照抄；
//     cr_settlement.Void 的 void_seq = settlement_id 照抄（释放槽位而不是删行）；
//   - 未实现的语句一律 panic，绝不返回空结果冒充「查不到」；
//     TransactCtx 在回调报错时整体回滚快照，用来断言「主表与台账要么都在要么都不在」；
//   - 复刻的是**语义**而不是锁：真实并发正确性由 InnoDB 的 next-key 锁保证，
//     单元测试无法也不该复刻锁，这里断言的是 logic 面对上述返回值的裁决。

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go-video/services/creator-revenue/internal/config"
	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// fakeNow 与 model.NowUnix 同源（真实时钟）；用例只用「相对现在的偏移」。
func fakeNow() int64 { return time.Now().Unix() }

// dupErr 复刻 MySQL 1062 文案，保证 model.IsDuplicateErr 能识别。
func dupErr(key, value string) error {
	return fmt.Errorf("Error 1062 (23000): Duplicate entry '%s' for key '%s'", value, key)
}

// fakeDB 是一次测试的全部内存态（七张 cr_* 表）。
type fakeDB struct {
	rules       []*model.RevenueRule
	ruleLogs    []*model.RuleChangeLog
	enrollments []*model.Enrollment
	metrics     []*model.RevenueMetric
	metricLogs  []*model.MetricChangeLog
	settlements []*model.Settlement
	items       []*model.SettlementItem

	seq map[string]int64

	// noRowsFor 让用例把某类 CAS 强制判成「条件不命中」（模拟并发抢先）。
	noRowsFor map[string]bool
	// failOn 让用例注入「真 DB 故障」（区别于唯一键冲突）。
	failOn map[string]error
	// hideForRead 让按唯一键的 SELECT 看不到某些行，而 INSERT 的唯一键检查照样看得到。
	// 用来复刻「另一会话绕过 SELECT ... FOR UPDATE 抢先落行」这个真库才有的窗口。
	hideForRead map[int64]bool
	// missNext 让某个读签名「接下来 n 次」返回空，之后恢复正常。
	// 用来表达「事务外快照读看不到、但唯一键看得见」这类只发生一次的并发窗口。
	missNext map[string]int
	// calls 记录执行过的语句签名，供「写了哪几张表」的断言使用。
	calls []string

	// reads / windows / readFailOn 是本批次读侧用例**新增**的观测面，与 db.calls 严格分开：
	// db.calls 只装 SQL 语句签名，已有用例用 `len(db.calls)==0` 断言
	// 「入参闸门未过就一条语句都不执行」（settlement_entry_test.go:124、:441），
	// 把「事务外 fake model 的内存读」记进 calls 会改写那些用例的口径。
	//   - reads：按调用顺序记 fake model 方法名（前缀 fake:）；
	//   - windows：记某个方法最后一次用到的分页窗口，断言「回显的 page/size 之外，
	//     真的按折好的 offset/limit 去查」；
	//   - readFailOn：把某个事务外读判成真库故障（logic 必须上抛，不得折叠成空结果）。
	reads      []string
	windows    map[string]pageWindow
	readFailOn map[string]error
}

// pageWindow 是一次列表读实际使用的物理分页位。
type pageWindow struct{ offset, limit int64 }

func newFakeDB() *fakeDB {
	return &fakeDB{
		seq:         map[string]int64{},
		noRowsFor:   map[string]bool{},
		failOn:      map[string]error{},
		hideForRead: map[int64]bool{},
		missNext:    map[string]int{},
		windows:     map[string]pageWindow{},
		readFailOn:  map[string]error{},
	}
}

// read 记一次「事务外读」并回注入的故障（未注入时回 nil）。
func (db *fakeDB) read(op string) error {
	db.reads = append(db.reads, op)
	return db.readFailOn[op]
}

// readPage 记一次带分页窗口的列表读。
func (db *fakeDB) readPage(op string, offset, limit int64) error {
	db.reads = append(db.reads, op)
	db.windows[op] = pageWindow{offset: offset, limit: limit}
	return db.readFailOn[op]
}

// window 取某列表读最后一次用的分页位。
func (db *fakeDB) window(op string) pageWindow { return db.windows[op] }

// assertReads 断言「第 from 条读之后」的读轨迹与 want **逐项等长等值**。
// 只数前缀会漏掉「多读了一张表」这类越权/多余访问，所以序列必须整体比。
func (db *fakeDB) assertReads(t *testing.T, from int, want ...string) {
	t.Helper()
	got := db.reads[from:]
	if len(got) != len(want) {
		t.Fatalf("读轨迹长度不符：期望 %d 项 %v，实际 %d 项 %v", len(want), want, len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项读不符：期望 %s，实际 %s（完整轨迹 %v）", i, want[i], got[i], got)
		}
	}
}

// paginate 复刻 model 各 List 末尾的 `LIMIT ? OFFSET ?`，
// 含「limit<=0 时 model 兜成 50、offset<0 夹到 0」的口径（见 cr_metric.go:262 等）。
func paginate[T any](rows []*T, offset, limit int64) []*T {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	out := make([]*T, 0, int(min64(limit, int64(len(rows)))))
	for i := offset; i < int64(len(rows)) && int64(len(out)) < limit; i++ {
		c := *rows[i]
		out = append(out, &c)
	}
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// takeMiss 消耗一次「读空」额度。
func (db *fakeDB) takeMiss(key string) bool {
	if db.missNext[key] <= 0 {
		return false
	}
	db.missNext[key]--
	return true
}

func (db *fakeDB) nextID(table string) int64 {
	db.seq[table]++
	return db.seq[table]
}

func (db *fakeDB) record(op string) {
	db.calls = append(db.calls, op)
	if err, ok := db.failOn[op]; ok {
		panic(fakeSQLFail{err})
	}
}

// injected 是「事务外假 model 方法」的故障注入口：那条路径上没有 session 的 recover，
// 所以直接把错误回给调用方而不是 panic。
func (db *fakeDB) injected(op string) error {
	db.calls = append(db.calls, op)
	return db.failOn[op]
}

// fakeSQLFail 把「注入的 DB 故障」从 session 层带到 sqlx 包装层的 error 通道。
type fakeSQLFail struct{ err error }

func (f fakeSQLFail) Error() string { return f.err.Error() }

// snapshot / restore 只为事务回滚断言服务。
// 必须逐行深拷贝：真库回滚撤掉的是「行内的值」，只拷切片头会让 UPDATE 改过的字段
// 在 restore 之后仍然是脏的（回滚就变成假的）。
func (db *fakeDB) snapshot() *fakeDB {
	cp := &fakeDB{seq: map[string]int64{}, noRowsFor: db.noRowsFor, failOn: db.failOn, hideForRead: db.hideForRead}
	for k, v := range db.seq {
		cp.seq[k] = v
	}
	cp.rules = copyRows(db.rules)
	cp.ruleLogs = copyRows(db.ruleLogs)
	cp.enrollments = copyRows(db.enrollments)
	cp.metrics = copyRows(db.metrics)
	cp.metricLogs = copyRows(db.metricLogs)
	cp.settlements = copyRows(db.settlements)
	cp.items = copyRows(db.items)
	return cp
}

func (db *fakeDB) restore(s *fakeDB) {
	db.rules, db.ruleLogs, db.enrollments = s.rules, s.ruleLogs, s.enrollments
	db.metrics, db.metricLogs, db.settlements, db.items = s.metrics, s.metricLogs, s.settlements, s.items
	db.seq = s.seq
}

// copyRows 复制每一行本身（七张表的行都是纯值结构体，没有嵌套指针）。
func copyRows[T any](in []*T) []*T {
	out := make([]*T, 0, len(in))
	for _, p := range in {
		c := *p
		out = append(out, &c)
	}
	return out
}

// ---------------------------------------------------------------- 种子数据

func (db *fakeDB) addRule(r *model.RevenueRule) *model.RevenueRule {
	row := *r
	if row.RuleId == 0 {
		row.RuleId = db.nextID("cr_revenue_rule")
	}
	if row.Version == 0 {
		row.Version = 1
	}
	db.rules = append(db.rules, &row)
	return &row
}

func (db *fakeDB) addEnrollment(e *model.Enrollment) *model.Enrollment {
	row := *e
	if row.EnrollmentId == 0 {
		row.EnrollmentId = db.nextID("cr_enrollment")
	}
	db.enrollments = append(db.enrollments, &row)
	return &row
}

func (db *fakeDB) addMetric(m *model.RevenueMetric) *model.RevenueMetric {
	row := *m
	if row.MetricId == 0 {
		row.MetricId = db.nextID("cr_metric")
	}
	db.metrics = append(db.metrics, &row)
	return &row
}

func (db *fakeDB) addSettlement(s *model.Settlement) *model.Settlement {
	row := *s
	if row.SettlementId == 0 {
		row.SettlementId = db.nextID("cr_settlement")
	}
	db.settlements = append(db.settlements, &row)
	return &row
}

// ruleByCode / enrollmentByMid / settlementByNo / metricsOf 是断言用的只读视图。
func (db *fakeDB) ruleByCode(code string) *model.RevenueRule {
	for _, r := range db.rules {
		if r.RuleCode == code {
			c := *r
			return &c
		}
	}
	return nil
}

func (db *fakeDB) enrollmentByMid(mid int64) *model.Enrollment {
	for _, e := range db.enrollments {
		if e.Mid == mid {
			c := *e
			return &c
		}
	}
	return nil
}

func (db *fakeDB) settlementByNo(no string) *model.Settlement {
	for _, s := range db.settlements {
		if s.SettlementNo == no {
			c := *s
			return &c
		}
	}
	return nil
}

func (db *fakeDB) liveSettlement(period string, mid int64) *model.Settlement {
	for _, s := range db.settlements {
		if s.Period == period && s.Mid == mid && s.VoidSeq == 0 {
			c := *s
			return &c
		}
	}
	return nil
}

func (db *fakeDB) metricsOf(period string, mid int64, sourceType int32) []*model.RevenueMetric {
	var out []*model.RevenueMetric
	for _, m := range db.metrics {
		if m.Period == period && m.Mid == mid && (sourceType == 0 || m.SourceType == sourceType) {
			c := *m
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Aid != out[j].Aid {
			return out[i].Aid < out[j].Aid
		}
		return out[i].MetricId < out[j].MetricId
	})
	return out
}

func (db *fakeDB) itemsOf(no string) []*model.SettlementItem {
	var out []*model.SettlementItem
	for _, it := range db.items {
		if it.SettlementNo == no {
			c := *it
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceType < out[j].SourceType })
	return out
}

func (db *fakeDB) countCalls(prefix string) int {
	n := 0
	for _, c := range db.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// countCallsAfter 只数「第 from 条之后」的语句，用来断言某一次调用本身写了哪几张表。
func (db *fakeDB) countCallsAfter(from int, prefix string) int {
	n := 0
	for i := from; i < len(db.calls); i++ {
		if strings.HasPrefix(db.calls[i], prefix) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- 结果包装

type fakeResult struct {
	affected int64
	lastID   int64
}

func (r fakeResult) LastInsertId() (int64, error) { return r.lastID, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.affected, nil }

// fill 把内存行按类型装进 model 侧传出来的目标（*Struct / *[]*Struct / *int64 / *string）。
func fill(v any, src any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		panic("fake: 目标必须是非 nil 指针")
	}
	dst := rv.Elem()
	s := reflect.ValueOf(src)
	if !s.IsValid() {
		dst.Set(reflect.Zero(dst.Type()))
		return
	}
	// model 侧用 &Struct 承接单行，假实现内部存的是 *Struct：先折成值再比类型。
	if s.Kind() == reflect.Pointer && dst.Kind() != reflect.Pointer && dst.Type() == s.Type().Elem() {
		s = s.Elem()
	}
	switch {
	case dst.Type() == s.Type():
		dst.Set(s)
	case dst.Kind() == reflect.Slice && s.Kind() == reflect.Slice &&
		dst.Type().Elem().Kind() == reflect.Pointer &&
		dst.Type().Elem() == s.Type().Elem():
		out := reflect.MakeSlice(dst.Type(), s.Len(), s.Len())
		for i := 0; i < s.Len(); i++ {
			p := reflect.New(dst.Type().Elem().Elem())
			p.Elem().Set(s.Index(i).Elem())
			out.Index(i).Set(p)
		}
		dst.Set(out)
	case dst.Kind() == reflect.Slice && s.Kind() == reflect.Slice &&
		dst.Type().Elem() == s.Type().Elem():
		out := reflect.MakeSlice(dst.Type(), s.Len(), s.Len())
		reflect.Copy(out, s)
		dst.Set(out)
	default:
		panic(fmt.Sprintf("fake: 无法把 %s 装进 %s", s.Type(), dst.Type()))
	}
}

// ---------------------------------------------------------------- sqlx.Session

// fakeSession 实现本服务 model 用到的 sqlx.Session 子集，其余语句一律 panic。
type fakeSession struct{ db *fakeDB }

var _ sqlx.Session = (*fakeSession)(nil)

func (s *fakeSession) Exec(query string, args ...any) (sql.Result, error) {
	return s.ExecCtx(context.Background(), query, args...)
}

func (s *fakeSession) ExecCtx(_ context.Context, query string, args ...any) (res sql.Result, err error) {
	op := classify(query)
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(fakeSQLFail)
			if !ok {
				panic(r)
			}
			err = f.err
		}
	}()
	s.db.record(op)
	return s.exec(op, query, args)
}

func (s *fakeSession) Prepare(string) (sqlx.StmtSession, error) {
	panic("fake: Prepare 未实现")
}

func (s *fakeSession) PrepareCtx(_ context.Context, _ string) (sqlx.StmtSession, error) {
	panic("fake: PrepareCtx 未实现")
}

func (s *fakeSession) QueryRow(v any, query string, args ...any) error {
	return s.QueryRowCtx(context.Background(), v, query, args...)
}

func (s *fakeSession) QueryRowPartial(v any, _ string, _ ...any) error {
	panic("fake: QueryRowPartial 未实现")
}

func (s *fakeSession) QueryRowPartialCtx(_ context.Context, _ any, _ string, _ ...any) error {
	panic("fake: QueryRowPartialCtx 未实现")
}

func (s *fakeSession) QueryRows(v any, query string, args ...any) error {
	return s.QueryRowsCtx(context.Background(), v, query, args...)
}

func (s *fakeSession) QueryRowsPartial(v any, _ string, _ ...any) error {
	panic("fake: QueryRowsPartial 未实现")
}

func (s *fakeSession) QueryRowsPartialCtx(_ context.Context, _ any, _ string, _ ...any) error {
	panic("fake: QueryRowsPartialCtx 未实现")
}

func (s *fakeSession) QueryRowCtx(_ context.Context, v any, query string, args ...any) (err error) {
	op := classify(query)
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(fakeSQLFail)
			if !ok {
				panic(r)
			}
			err = f.err
		}
	}()
	s.db.record(op)
	row, found := s.queryRow(op, args)
	if !found {
		return sql.ErrNoRows
	}
	fill(v, row)
	return nil
}

func (s *fakeSession) QueryRowsCtx(_ context.Context, v any, query string, args ...any) (err error) {
	op := classify(query)
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(fakeSQLFail)
			if !ok {
				panic(r)
			}
			err = f.err
		}
	}()
	s.db.record(op)
	fill(v, s.queryRows(op, args))
	return nil
}

// noRows 复刻「CAS 条件不命中」：真库回 RowsAffected=0，model.rowsAffected 判 false。
func (db *fakeDB) noRows(op string) bool { return db.noRowsFor[op] }

// argI / argF 取参数并断言数值口径，越界即 panic（宁可炸给测试看）。
func argI(args []any, i int) int64 {
	switch v := args[i].(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	}
	panic(fmt.Sprintf("fake: 第 %d 个参数不是整数：%T", i, args[i]))
}

func argS(args []any, i int) string {
	v, ok := args[i].(string)
	if !ok {
		panic(fmt.Sprintf("fake: 第 %d 个参数不是字符串：%T", i, args[i]))
	}
	return v
}

// classify 把语句折成签名。签名不在表内的一律 panic（见 notSupported）。
func classify(query string) string {
	q := strings.Join(strings.Fields(query), " ")
	for _, sig := range querySignatures {
		if strings.Contains(q, sig[1]) {
			return sig[0]
		}
	}
	panic("fake: 未预置的 SQL 语句：" + q)
}

// querySignatures 是「签名 ← 语句特征」对照表，特征串逐字抄自 model/*.go。
// 新写一条 SQL 就会当场 panic，逼着测试同步扩展假实现（而不是静默走空结果）。
var querySignatures = [][2]string{
	{"ins:cr_enrollment", "INSERT INTO cr_enrollment (mid, state, agreed_rule_version"},
	{"upd:cr_enrollment.transition", "UPDATE cr_enrollment SET state = ?, agreed_rule_version = IF(? > 0"},
	{"upd:cr_enrollment.agreed", "UPDATE cr_enrollment SET agreed_rule_version = ?"},
	{"sel:cr_enrollment.lockByMid", "FROM cr_enrollment WHERE mid = ? LIMIT 1 FOR UPDATE"},
	{"sel:cr_enrollment.byMid", "FROM cr_enrollment WHERE mid = ? LIMIT 1"},

	{"ins:cr_revenue_rule", "INSERT INTO cr_revenue_rule (rule_code, source_type"},
	{"upd:cr_revenue_rule.draft", "UPDATE cr_revenue_rule SET source_type = ?"},
	{"upd:cr_revenue_rule.state", "UPDATE cr_revenue_rule SET state = ?, version = version + 1"},
	{"sel:cr_revenue_rule.byCodeLock", "FROM cr_revenue_rule WHERE rule_code = ? LIMIT 1 FOR UPDATE"},
	{"sel:cr_revenue_rule.byCode", "FROM cr_revenue_rule WHERE rule_code = ? LIMIT 1"},
	{"sel:cr_revenue_rule.bySourceLock", "FROM cr_revenue_rule WHERE source_type = ? ORDER BY rule_id ASC FOR UPDATE"},
	{"sel:cr_revenue_rule.byId", "FROM cr_revenue_rule WHERE rule_id = ? LIMIT 1"},

	{"ins:cr_rule_change_log", "INSERT INTO cr_rule_change_log (rule_code, source_type, action"},
	{"sel:cr_rule_change_log.byRequest", "FROM cr_rule_change_log WHERE request_id = ? LIMIT 1"},

	{"ins:cr_metric", "INSERT INTO cr_metric (period, mid, aid, source_type"},
	{"upd:cr_metric.correction", "UPDATE cr_metric SET rule_code = ?"},
	{"upd:cr_metric.capped", "UPDATE cr_metric SET capped_amount_minor = ?"},
	{"sel:cr_metric.byKeyLock", "FROM cr_metric WHERE period = ? AND mid = ? AND aid = ? AND source_type = ? LIMIT 1 FOR UPDATE"},
	{"sel:cr_metric.byKey", "FROM cr_metric WHERE period = ? AND mid = ? AND aid = ? AND source_type = ? LIMIT 1"},
	{"sel:cr_metric.groupLock", "FROM cr_metric WHERE period = ? AND mid = ? AND source_type = ? ORDER BY aid ASC, metric_id ASC FOR UPDATE"},
	{"sel:cr_metric.settleable", "SELECT DISTINCT m.mid FROM cr_metric m"},
	{"cnt:cr_metric.sumCapped", "SELECT SUM(capped_amount_minor) FROM cr_metric WHERE period = ? AND mid = ?"},

	{"ins:cr_metric_change_log", "INSERT INTO cr_metric_change_log (period, mid, aid, source_type"},

	{"ins:cr_settlement", "INSERT INTO cr_settlement (settlement_no, period, mid, amount_minor"},
	{"upd:cr_settlement.draftAmounts", "UPDATE cr_settlement SET amount_minor = ?"},
	{"upd:cr_settlement.confirm", "UPDATE cr_settlement SET state = ?, confirmed_by = ?"},
	{"upd:cr_settlement.void", "UPDATE cr_settlement SET state = ?, void_reason = ?"},
	{"sel:cr_settlement.activeLock", "FROM cr_settlement WHERE period = ? AND mid = ? AND void_seq = ? LIMIT 1 FOR UPDATE"},
	{"sel:cr_settlement.active", "FROM cr_settlement WHERE period = ? AND mid = ? AND void_seq = ? LIMIT 1"},
	{"sel:cr_settlement.byNo", "FROM cr_settlement WHERE settlement_no = ? LIMIT 1"},
	{"sel:cr_settlement.byRequest", "FROM cr_settlement WHERE request_id = ? LIMIT 1"},
	{"cnt:cr_settlement.byPeriodMid", "SELECT COUNT(*) FROM cr_settlement WHERE period = ? AND mid = ?"},

	{"ins:cr_settlement_item", "INSERT INTO cr_settlement_item (settlement_no, source_type"},
	{"del:cr_settlement_item.byNo", "DELETE FROM cr_settlement_item WHERE settlement_no = ?"},
}

// --------------------------------------------------------------- 单行查询

func (s *fakeSession) queryRow(op string, args []any) (any, bool) {
	db := s.db
	switch op {
	case "sel:cr_enrollment.byMid", "sel:cr_enrollment.lockByMid":
		mid := argI(args, 0)
		for _, e := range db.enrollments {
			if e.Mid == mid {
				c := *e
				return &c, true
			}
		}
		return nil, false

	case "sel:cr_revenue_rule.byCode", "sel:cr_revenue_rule.byCodeLock":
		code := argS(args, 0)
		for _, r := range db.rules {
			if r.RuleCode == code {
				c := *r
				return &c, true
			}
		}
		return nil, false

	case "sel:cr_revenue_rule.byId":
		id := argI(args, 0)
		for _, r := range db.rules {
			if r.RuleId == id {
				c := *r
				return &c, true
			}
		}
		return nil, false

	case "sel:cr_rule_change_log.byRequest":
		req := argS(args, 0)
		for _, l := range db.ruleLogs {
			if l.RequestId == req {
				c := *l
				return &c, true
			}
		}
		return nil, false

	case "sel:cr_metric.byKey", "sel:cr_metric.byKeyLock":
		period, mid, aid, st := argS(args, 0), argI(args, 1), argI(args, 2), int32(argI(args, 3))
		for _, m := range db.metrics {
			if m.Period == period && m.Mid == mid && m.Aid == aid && m.SourceType == st {
				if db.hideForRead[m.MetricId] {
					return nil, false // 另一会话绕锁写的行：本会话的 SELECT 看不见，INSERT 才撞唯一键
				}
				c := *m
				return &c, true
			}
		}
		return nil, false

	case "sel:cr_settlement.active", "sel:cr_settlement.activeLock":
		period, mid, voidSeq := argS(args, 0), argI(args, 1), argI(args, 2)
		for _, x := range db.settlements {
			if x.Period == period && x.Mid == mid && x.VoidSeq == voidSeq {
				c := *x
				return &c, true
			}
		}
		return nil, false

	case "sel:cr_settlement.byNo":
		no := argS(args, 0)
		for _, x := range db.settlements {
			if x.SettlementNo == no {
				c := *x
				return &c, true
			}
		}
		return nil, false

	case "sel:cr_settlement.byRequest":
		req := argS(args, 0)
		for _, x := range db.settlements {
			if x.RequestId == req {
				c := *x
				return &c, true
			}
		}
		return nil, false

	case "cnt:cr_settlement.byPeriodMid":
		period, mid := argS(args, 0), argI(args, 1)
		var n int64
		for _, x := range db.settlements {
			if x.Period == period && x.Mid == mid {
				n++
			}
		}
		return n, true

	case "cnt:cr_metric.sumCapped":
		period, mid := argS(args, 0), argI(args, 1)
		var sum int64
		for _, m := range db.metrics {
			if m.Period == period && m.Mid == mid {
				sum += m.CappedAmountMinor
			}
		}
		return sql.NullInt64{Int64: sum, Valid: true}, true
	}
	panic("fake: 未预置的单行查询 " + op)
}

// --------------------------------------------------------------- 多行查询

func (s *fakeSession) queryRows(op string, args []any) any {
	db := s.db
	switch op {
	case "sel:cr_revenue_rule.bySourceLock":
		st := int32(argI(args, 0))
		var rows []*model.RevenueRule
		for _, r := range db.rules {
			if r.SourceType == st {
				c := *r
				rows = append(rows, &c)
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].RuleId < rows[j].RuleId })
		return rows

	case "sel:cr_metric.groupLock":
		period, mid, st := argS(args, 0), argI(args, 1), int32(argI(args, 2))
		return db.metricsOf(period, mid, st)

	case "sel:cr_metric.settleable":
		period := argS(args, 0)
		seen := map[int64]bool{}
		var mids []int64
		for _, m := range db.metrics {
			if m.Period != period || seen[m.Mid] {
				continue
			}
			for _, e := range db.enrollments {
				if e.Mid == m.Mid && e.State == model.EnrollmentStateEnrolled {
					seen[m.Mid] = true
					mids = append(mids, m.Mid)
				}
			}
		}
		sort.Slice(mids, func(i, j int) bool { return mids[i] < mids[j] })
		limit := int(argI(args, 1))
		if limit > 0 && len(mids) > limit {
			mids = mids[:limit]
		}
		return mids
	}
	panic("fake: 未预置的多行查询 " + op)
}

// --------------------------------------------------------------- 写语句

func (s *fakeSession) exec(op, query string, args []any) (sql.Result, error) {
	db := s.db
	switch op {
	case "ins:cr_enrollment":
		// uniq_mid
		mid := argI(args, 0)
		if db.enrollmentByMid(mid) != nil {
			return nil, dupErr("uniq_mid", fmt.Sprint(mid))
		}
		e := &model.Enrollment{
			Mid: mid, State: int32(argI(args, 1)), AgreedRuleVersion: argI(args, 2),
			EnrolledAt: argI(args, 3), LeftAt: argI(args, 4),
			Operator: argS(args, 5), Remark: argS(args, 6),
			Ctime: argI(args, 7), Mtime: argI(args, 8),
		}
		e.EnrollmentId = db.nextID("cr_enrollment")
		db.enrollments = append(db.enrollments, e)
		return fakeResult{affected: 1, lastID: e.EnrollmentId}, nil

	case "upd:cr_enrollment.transition":
		// WHERE mid = ? AND state = ?（末两位）；时间戳三态 0 保持 / >0 写入 / <0 清零。
		mid, fromState := argI(args, 12), int32(argI(args, 13))
		cur := db.enrollmentByMidRaw(mid)
		if cur == nil || cur.State != fromState || db.noRows(op) {
			return fakeResult{affected: 0}, nil
		}
		toState := int32(argI(args, 0))
		if arv := argI(args, 1); arv > 0 {
			cur.AgreedRuleVersion = arv
		}
		if ea := argI(args, 3); ea < 0 {
			cur.EnrolledAt = 0
		} else if ea > 0 {
			cur.EnrolledAt = ea
		}
		if la := argI(args, 6); la < 0 {
			cur.LeftAt = 0
		} else if la > 0 {
			cur.LeftAt = la
		}
		cur.State = toState
		cur.Operator, cur.Remark, cur.Mtime = argS(args, 9), argS(args, 10), argI(args, 11)
		return fakeResult{affected: 1}, nil

	case "upd:cr_enrollment.agreed":
		// WHERE mid = ? AND state = ENROLLED AND agreed_rule_version < ?
		arv := argI(args, 0)
		mid, state, lt := argI(args, 3), int32(argI(args, 4)), argI(args, 5)
		cur := db.enrollmentByMidRaw(mid)
		if cur == nil || cur.State != state || !(cur.AgreedRuleVersion < lt) || db.noRows(op) {
			return fakeResult{affected: 0}, nil
		}
		cur.AgreedRuleVersion, cur.Operator, cur.Mtime = arv, argS(args, 1), argI(args, 2)
		return fakeResult{affected: 1}, nil

	case "ins:cr_revenue_rule":
		code := argS(args, 0)
		if db.ruleByCode(code) != nil {
			return nil, dupErr("uniq_rule_code", code)
		}
		r := &model.RevenueRule{
			RuleCode: code, SourceType: int32(argI(args, 1)), Name: argS(args, 2),
			Description: argS(args, 3), UnitPricePer1000: argI(args, 4), Currency: argS(args, 5),
			Unit: argS(args, 6), MinQuantity: argI(args, 7), MonthlyCapMinor: argI(args, 8),
			State: int32(argI(args, 9)), EffectiveFrom: argI(args, 10), Version: argI(args, 11),
			CreatedBy: argS(args, 12), UpdatedBy: argS(args, 13), Ctime: argI(args, 14), Mtime: argI(args, 15),
		}
		r.RuleId = db.nextID("cr_revenue_rule")
		db.rules = append(db.rules, r)
		return fakeResult{affected: 1, lastID: r.RuleId}, nil

	case "upd:cr_revenue_rule.draft":
		// WHERE rule_id = ? AND version = ? AND state = DRAFT
		id, ver, state := argI(args, 11), argI(args, 12), int32(argI(args, 13))
		cur := db.ruleByID(id)
		if cur == nil || cur.Version != ver || cur.State != state || db.noRows(op) {
			return fakeResult{affected: 0}, nil
		}
		cur.SourceType, cur.Name, cur.Description = int32(argI(args, 0)), argS(args, 1), argS(args, 2)
		cur.UnitPricePer1000, cur.Currency, cur.Unit = argI(args, 3), argS(args, 4), argS(args, 5)
		cur.MinQuantity, cur.MonthlyCapMinor, cur.EffectiveFrom = argI(args, 6), argI(args, 7), argI(args, 8)
		cur.UpdatedBy, cur.Mtime = argS(args, 9), argI(args, 10)
		cur.Version++
		return fakeResult{affected: 1}, nil

	case "upd:cr_revenue_rule.state":
		// WHERE rule_id = ? AND version = ? AND state = fromState
		id, ver, fromState := argI(args, 3), argI(args, 4), int32(argI(args, 5))
		cur := db.ruleByID(id)
		if cur == nil || cur.Version != ver || cur.State != fromState || db.noRows(op) {
			return fakeResult{affected: 0}, nil
		}
		cur.State, cur.UpdatedBy, cur.Mtime = int32(argI(args, 0)), argS(args, 1), argI(args, 2)
		cur.Version++
		return fakeResult{affected: 1}, nil

	case "ins:cr_rule_change_log":
		req := argS(args, 11)
		// uniq_request_id：幂等兜底，同键第二次必须冲突而不是静默追加。
		for _, l := range db.ruleLogs {
			if l.RequestId == req {
				return nil, dupErr("uniq_request_id", req)
			}
		}
		l := &model.RuleChangeLog{
			RuleCode: argS(args, 0), SourceType: int32(argI(args, 1)), Action: argS(args, 2),
			FromState: int32(argI(args, 3)), ToState: int32(argI(args, 4)),
			FromUnitPrice: argI(args, 5), ToUnitPrice: argI(args, 6),
			FromVersion: argI(args, 7), ToVersion: argI(args, 8),
			Operator: argS(args, 9), Reason: argS(args, 10), RequestId: req, Ctime: argI(args, 12),
		}
		l.LogId = db.nextID("cr_rule_change_log")
		db.ruleLogs = append(db.ruleLogs, l)
		return fakeResult{affected: 1, lastID: l.LogId}, nil

	case "ins:cr_metric":
		period, mid, aid, st := argS(args, 0), argI(args, 1), argI(args, 2), int32(argI(args, 3))
		for _, m := range db.metrics {
			if m.Period == period && m.Mid == mid && m.Aid == aid && m.SourceType == st {
				return nil, dupErr("uniq_metric_key", fmt.Sprintf("%s-%d-%d-%d", period, mid, aid, st))
			}
		}
		m := &model.RevenueMetric{
			Period: period, Mid: mid, Aid: aid, SourceType: st,
			RuleCode: argS(args, 4), RuleVersion: argI(args, 5),
			Quantity: argI(args, 6), Unit: argS(args, 7),
			AmountMinor: argI(args, 8), CappedAmountMinor: argI(args, 9),
			ThresholdBlocked: int32(argI(args, 10)), SourceDetail: argS(args, 11),
			Corrected: int32(argI(args, 12)), Ctime: argI(args, 13), Mtime: argI(args, 14),
		}
		m.MetricId = db.nextID("cr_metric")
		db.metrics = append(db.metrics, m)
		return fakeResult{affected: 1, lastID: m.MetricId}, nil

	case "upd:cr_metric.correction":
		// WHERE metric_id = ? AND quantity = ?（旧数量做 CAS 条件）
		id, oldQty := argI(args, 9), argI(args, 10)
		cur := db.metricByID(id)
		if cur == nil || cur.Quantity != oldQty || db.noRows(op) {
			return fakeResult{affected: 0}, nil
		}
		cur.RuleCode, cur.RuleVersion = argS(args, 0), argI(args, 1)
		cur.Quantity, cur.Unit = argI(args, 2), argS(args, 3)
		cur.AmountMinor, cur.CappedAmountMinor = argI(args, 4), argI(args, 5)
		cur.ThresholdBlocked, cur.SourceDetail = int32(argI(args, 6)), argS(args, 7)
		cur.Corrected, cur.Mtime = 1, argI(args, 8)
		return fakeResult{affected: 1}, nil

	case "upd:cr_metric.capped":
		id := argI(args, 2)
		cur := db.metricByID(id)
		if cur == nil || db.noRows(op) {
			return fakeResult{affected: 0}, nil
		}
		cur.CappedAmountMinor, cur.Mtime = argI(args, 0), argI(args, 1)
		return fakeResult{affected: 1}, nil

	case "ins:cr_metric_change_log":
		l := &model.MetricChangeLog{
			Period: argS(args, 0), Mid: argI(args, 1), Aid: argI(args, 2),
			SourceType: int32(argI(args, 3)), RuleCode: argS(args, 4),
			OldRuleVersion: argI(args, 5), NewRuleVersion: argI(args, 6),
			OldQuantity: argI(args, 7), NewQuantity: argI(args, 8),
			OldAmountMinor: argI(args, 9), NewAmountMinor: argI(args, 10),
			OldCappedAmountMinor: argI(args, 11), NewCappedAmountMinor: argI(args, 12),
			Operator: argS(args, 13), Reason: argS(args, 14), RequestId: argS(args, 15), Ctime: argI(args, 16),
		}
		l.LogId = db.nextID("cr_metric_change_log")
		db.metricLogs = append(db.metricLogs, l)
		return fakeResult{affected: 1, lastID: l.LogId}, nil

	case "ins:cr_settlement":
		period, mid, voidSeq := argS(args, 1), argI(args, 2), argI(args, 13)
		no, req := argS(args, 0), argS(args, 12)
		for _, x := range db.settlements {
			if x.SettlementNo == no {
				return nil, dupErr("uniq_settlement_no", no)
			}
			if x.Period == period && x.Mid == mid && x.VoidSeq == voidSeq && voidSeq == 0 {
				return nil, dupErr("uniq_active_period_mid", fmt.Sprintf("%s-%d-0", period, mid))
			}
			if x.RequestId == req {
				return nil, dupErr("uniq_request_id", req)
			}
		}
		x := &model.Settlement{
			SettlementNo: no, Period: period, Mid: mid,
			AmountMinor: argI(args, 3), CapAppliedMinor: argI(args, 4), Currency: argS(args, 5),
			MetricCount: argI(args, 6), State: int32(argI(args, 7)), PayoutState: int32(argI(args, 8)),
			ConfirmedBy: argS(args, 9), ConfirmedAt: argI(args, 10), VoidReason: argS(args, 11),
			RequestId: req, VoidSeq: voidSeq, Ctime: argI(args, 14), Mtime: argI(args, 15),
		}
		x.SettlementId = db.nextID("cr_settlement")
		db.settlements = append(db.settlements, x)
		return fakeResult{affected: 1, lastID: x.SettlementId}, nil

	case "upd:cr_settlement.draftAmounts":
		// WHERE settlement_no = ? AND state = DRAFT AND void_seq = 0
		no, state, voidSeq := argS(args, 6), int32(argI(args, 7)), argI(args, 8)
		cur := db.settlementByNoRaw(no)
		if cur == nil || cur.State != state || cur.VoidSeq != voidSeq || db.noRows(op) {
			return fakeResult{affected: 0}, nil
		}
		cur.AmountMinor, cur.CapAppliedMinor, cur.MetricCount = argI(args, 0), argI(args, 1), argI(args, 2)
		cur.Currency, cur.RequestId, cur.Mtime = argS(args, 3), argS(args, 4), argI(args, 5)
		return fakeResult{affected: 1}, nil

	case "upd:cr_settlement.confirm":
		no, state, voidSeq := argS(args, 4), int32(argI(args, 5)), argI(args, 6)
		cur := db.settlementByNoRaw(no)
		if cur == nil || cur.State != state || cur.VoidSeq != voidSeq || db.noRows(op) {
			return fakeResult{affected: 0}, nil
		}
		cur.State, cur.ConfirmedBy, cur.ConfirmedAt, cur.Mtime =
			int32(argI(args, 0)), argS(args, 1), argI(args, 2), argI(args, 3)
		return fakeResult{affected: 1}, nil

	case "upd:cr_settlement.void":
		// SET state=VOIDED, void_reason=?, void_seq = settlement_id
		no, state, voidSeq := argS(args, 3), int32(argI(args, 4)), argI(args, 5)
		cur := db.settlementByNoRaw(no)
		if cur == nil || cur.State != state || cur.VoidSeq != voidSeq || db.noRows(op) {
			return fakeResult{affected: 0}, nil
		}
		cur.State, cur.VoidReason, cur.Mtime = int32(argI(args, 0)), argS(args, 1), argI(args, 2)
		cur.VoidSeq = cur.SettlementId
		return fakeResult{affected: 1}, nil

	case "ins:cr_settlement_item":
		// 单条多值 INSERT：每 6 个参数一行。
		if len(args)%6 != 0 {
			panic(fmt.Sprintf("fake: cr_settlement_item 参数数 %d 不是 6 的倍数", len(args)))
		}
		for i := 0; i+5 < len(args); i += 6 {
			no := argS(args, i)
			st := int32(argI(args, i+1))
			for _, it := range db.items {
				if it.SettlementNo == no && it.SourceType == st {
					return nil, dupErr("uniq_no_source", fmt.Sprintf("%s-%d", no, st))
				}
			}
			db.items = append(db.items, &model.SettlementItem{
				ItemId: db.nextID("cr_settlement_item"), SettlementNo: no, SourceType: st,
				RuleCode: argS(args, i+2), Quantity: argI(args, i+3),
				AmountMinor: argI(args, i+4), Ctime: argI(args, i+5),
			})
		}
		return fakeResult{affected: int64(len(args) / 6)}, nil

	case "del:cr_settlement_item.byNo":
		no := argS(args, 0)
		kept := db.items[:0]
		for _, it := range db.items {
			if it.SettlementNo != no {
				kept = append(kept, it)
			}
		}
		db.items = kept
		return fakeResult{affected: 1}, nil
	}
	_ = query
	panic("fake: 未预置的写语句 " + op)
}

// 下面几个 xxxID/Raw 取的是内存里的**活指针**，写语句必须改它；
// ruleByCode / enrollmentByMid / settlementByNo 是断言用的只读视图（回副本）。
func (db *fakeDB) ruleByID(id int64) *model.RevenueRule {
	for _, r := range db.rules {
		if r.RuleId == id {
			return r
		}
	}
	return nil
}

func (db *fakeDB) enrollmentByMidRaw(mid int64) *model.Enrollment {
	for _, e := range db.enrollments {
		if e.Mid == mid {
			return e
		}
	}
	return nil
}

func (db *fakeDB) metricByID(id int64) *model.RevenueMetric {
	for _, m := range db.metrics {
		if m.MetricId == id {
			return m
		}
	}
	return nil
}

func (db *fakeDB) settlementByNoRaw(no string) *model.Settlement {
	for _, s := range db.settlements {
		if s.SettlementNo == no {
			return s
		}
	}
	return nil
}

// ---------------------------------------------------------------- sqlx.SqlConn

// fakeConn 只实现被测代码真正用到的 TransactCtx，其余 sqlx 方法由内嵌 nil 接口提升，
// 一旦有人想用真连接会当场 panic。回调整体报错时恢复快照，用来证明「跨表写要么都在要么都不在」。
type fakeConn struct {
	sqlx.SqlConn
	db *fakeDB
}

func (c fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	snap := c.db.snapshot()
	if err := fn(ctx, &fakeSession{db: c.db}); err != nil {
		c.db.restore(snap)
		return err
	}
	return nil
}

// ---------------------------------------------------------------- ServiceContext

// 以下假 model 只覆写「事务外」被调到的方法（ServiceContext 字段路径）；
// 事务内路径走 fakeSession，两者共享同一份 fakeDB，所以断言看到的是同一份状态。

type fakeRules struct {
	model.RevenueRuleModel
	db *fakeDB
}

func (f fakeRules) FindOne(_ context.Context, ruleID int64) (*model.RevenueRule, error) {
	if err := f.db.read("fake:rules.FindOne"); err != nil {
		return nil, err
	}
	r := f.db.ruleByID(ruleID)
	if r == nil {
		return nil, nil
	}
	c := *r
	return &c, nil
}

func (f fakeRules) FindByCode(_ context.Context, code string) (*model.RevenueRule, error) {
	if err := f.db.read("fake:rules.FindByCode"); err != nil {
		return nil, err
	}
	r := f.db.ruleByCode(code)
	if r == nil {
		return nil, nil
	}
	c := *r
	return &c, nil
}

// List 复刻 defaultRevenueRuleModel.List：ruleFilter 的两个可选条件 + ORDER BY rule_id ASC
// + LIMIT/OFFSET（model/cr_revenue_rule.go:184，排序事实源在同文件 :196）。
func (f fakeRules) List(
	_ context.Context, state, sourceType int32, offset, limit int64,
) ([]*model.RevenueRule, error) {
	if err := f.db.readPage("fake:rules.List", offset, limit); err != nil {
		return nil, err
	}
	rows := make([]*model.RevenueRule, 0, len(f.db.rules))
	for _, r := range f.db.rules {
		if state != model.RuleStateUnspecified && r.State != state {
			continue
		}
		if sourceType != 0 && r.SourceType != sourceType {
			continue
		}
		c := *r
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RuleId < rows[j].RuleId })
	return paginate(rows, offset, limit), nil
}

// Count 与 List 共用 ruleFilter（model/cr_revenue_rule.go:250），条件必须同源。
func (f fakeRules) Count(_ context.Context, state, sourceType int32) (int64, error) {
	if err := f.db.read("fake:rules.Count"); err != nil {
		return 0, err
	}
	var n int64
	for _, r := range f.db.rules {
		if state != model.RuleStateUnspecified && r.State != state {
			continue
		}
		if sourceType != 0 && r.SourceType != sourceType {
			continue
		}
		n++
	}
	return n, nil
}

type fakeRuleLogs struct {
	model.RuleChangeLogModel
	db *fakeDB
}

func (f fakeRuleLogs) FindByRequest(_ context.Context, requestID string) (*model.RuleChangeLog, error) {
	if err := f.db.read("fake:ruleChanges.FindByRequest"); err != nil {
		return nil, err
	}
	for _, l := range f.db.ruleLogs {
		if l.RequestId == requestID {
			c := *l
			return &c, nil
		}
	}
	return nil, nil
}

// FirstChangeFrom 复刻 model/cr_rule_change_log.go:111：
// WHERE rule_code = ? AND from_version >= ? ORDER BY from_version ASC LIMIT 1。
// 「首条」是历史单价的判定要点：同一版本区间有多条变更时只能取最早那条的 from_。
func (f fakeRuleLogs) FirstChangeFrom(
	_ context.Context, ruleCode string, version int64,
) (*model.RuleChangeLog, error) {
	if err := f.db.read("fake:ruleChanges.FirstChangeFrom"); err != nil {
		return nil, err
	}
	var cand []*model.RuleChangeLog
	for _, l := range f.db.ruleLogs {
		if l.RuleCode == ruleCode && l.FromVersion >= version {
			c := *l
			cand = append(cand, &c)
		}
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].FromVersion < cand[j].FromVersion })
	if len(cand) == 0 {
		return nil, nil
	}
	return cand[0], nil
}

func (f fakeRuleLogs) Insert(_ context.Context, l *model.RuleChangeLog) (int64, error) {
	for _, e := range f.db.ruleLogs {
		if e.RequestId == l.RequestId {
			return 0, dupErr("uniq_request_id", l.RequestId)
		}
	}
	row := *l
	row.LogId = f.db.nextID("cr_rule_change_log")
	f.db.ruleLogs = append(f.db.ruleLogs, &row)
	return row.LogId, nil
}

type fakeEnrollments struct {
	model.EnrollmentModel
	db *fakeDB
}

func (f fakeEnrollments) FindOne(_ context.Context, mid int64) (*model.Enrollment, error) {
	if err := f.db.read("fake:enrollments.FindOne"); err != nil {
		return nil, err
	}
	if f.db.takeMiss("fake:enrollments.FindOne") {
		return nil, nil // 事务外的快照读看不到那一行，但 uniq_mid 看得见
	}
	e := f.db.enrollmentByMid(mid)
	if e == nil {
		return nil, nil
	}
	c := *e
	return &c, nil
}

// List 复刻 defaultEnrollmentModel.List：state=0 不过滤 + ORDER BY mid ASC
// + LIMIT/OFFSET（model/cr_enrollment.go:157，排序事实源在同文件 :172）。
func (f fakeEnrollments) List(
	_ context.Context, state int32, offset, limit int64,
) ([]*model.Enrollment, error) {
	if err := f.db.readPage("fake:enrollments.List", offset, limit); err != nil {
		return nil, err
	}
	rows := make([]*model.Enrollment, 0, len(f.db.enrollments))
	for _, e := range f.db.enrollments {
		if state == model.EnrollmentStateUnspecified || e.State == state {
			c := *e
			rows = append(rows, &c)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Mid < rows[j].Mid })
	return paginate(rows, offset, limit), nil
}

// Count 与 List 同一个 state 条件（model/cr_enrollment.go:183）。
func (f fakeEnrollments) Count(_ context.Context, state int32) (int64, error) {
	if err := f.db.read("fake:enrollments.Count"); err != nil {
		return 0, err
	}
	var n int64
	for _, e := range f.db.enrollments {
		if state == model.EnrollmentStateUnspecified || e.State == state {
			n++
		}
	}
	return n, nil
}

func (f fakeEnrollments) Insert(_ context.Context, e *model.Enrollment) (int64, error) {
	if err := f.db.injected("fake:cr_enrollment.Insert"); err != nil {
		return 0, err
	}
	if f.db.enrollmentByMid(e.Mid) != nil {
		return 0, dupErr("uniq_mid", fmt.Sprint(e.Mid))
	}
	row := *e
	if row.EnrolledAt == 0 {
		row.EnrolledAt = fakeNow()
	}
	row.EnrollmentId = f.db.nextID("cr_enrollment")
	f.db.enrollments = append(f.db.enrollments, &row)
	return row.EnrollmentId, nil
}

type fakeMetrics struct {
	model.RevenueMetricModel
	db    *fakeDB
	mids  []int64
	midsE error
	// limitSeen 记录最后一次 ListSettleableMids 收到的 limit，
	// 用来断言「多取一条只为判定还有没有」的探测位（GenerateMaxBatch+1）。
	limitSeen int64
}

func (f *fakeMetrics) ListSettleableMids(_ context.Context, _ string, limit int64) ([]int64, error) {
	f.limitSeen = limit
	if f.midsE != nil {
		return nil, f.midsE
	}
	if limit > 0 && int64(len(f.mids)) > limit {
		return append([]int64(nil), f.mids[:limit]...), nil
	}
	return append([]int64(nil), f.mids...), nil
}

// settleable 把 ServiceContext 上的 Metrics 换成「可出单候选人可控」的版本。
func settleable(ctx *svc.ServiceContext, db *fakeDB, mids ...int64) *fakeMetrics {
	f := &fakeMetrics{db: db, mids: mids}
	ctx.Metrics = f
	return f
}

func (f fakeMetrics) FindByKey(_ context.Context, period string, mid, aid int64, st int32) (*model.RevenueMetric, error) {
	if err := f.db.read("fake:metrics.FindByKey"); err != nil {
		return nil, err
	}
	for _, m := range f.db.metrics {
		if m.Period == period && m.Mid == mid && m.Aid == aid && m.SourceType == st {
			c := *m
			return &c, nil
		}
	}
	return nil, nil
}

// List 复刻 defaultRevenueMetricModel.List：metricFilter 的四个可选条件
// + ORDER BY period ASC, mid ASC, aid ASC, source_type ASC
// （model/cr_metric.go:258，排序事实源在同文件 :270，WHERE 条件在同文件 :353）。
func (f fakeMetrics) List(
	_ context.Context, period string, mid, aid int64, sourceType int32, offset, limit int64,
) ([]*model.RevenueMetric, error) {
	if err := f.db.readPage("fake:metrics.List", offset, limit); err != nil {
		return nil, err
	}
	rows := f.db.metricsMatching(period, mid, aid, sourceType)
	return paginate(rows, offset, limit), nil
}

// Count 与 List 共用 metricFilter（model/cr_metric.go:281）。
func (f fakeMetrics) Count(
	_ context.Context, period string, mid, aid int64, sourceType int32,
) (int64, error) {
	if err := f.db.read("fake:metrics.Count"); err != nil {
		return 0, err
	}
	return int64(len(f.db.metricsMatching(period, mid, aid, sourceType))), nil
}

// SumCappedByPeriod 复刻 model/cr_metric.go:313：
// SELECT SUM(capped_amount_minor) WHERE period = ? AND mid = ?
// —— 封顶后金额，不是原始 amount_minor，也不按 threshold_blocked 过滤。
func (f fakeMetrics) SumCappedByPeriod(_ context.Context, period string, mid int64) (int64, error) {
	if err := f.db.read("fake:metrics.SumCappedByPeriod"); err != nil {
		return 0, err
	}
	var sum int64
	for _, m := range f.db.metrics {
		if m.Period == period && m.Mid == mid {
			sum += m.CappedAmountMinor
		}
	}
	return sum, nil
}

// metricsMatching 按 metricFilter 的四个可选条件取行，再按其 ORDER BY 排序。
func (db *fakeDB) metricsMatching(period string, mid, aid int64, sourceType int32) []*model.RevenueMetric {
	var rows []*model.RevenueMetric
	for _, m := range db.metrics {
		if period != "" && m.Period != period {
			continue
		}
		if mid != 0 && m.Mid != mid {
			continue
		}
		if aid != 0 && m.Aid != aid {
			continue
		}
		if sourceType != 0 && m.SourceType != sourceType {
			continue
		}
		c := *m
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Period != rows[j].Period {
			return rows[i].Period < rows[j].Period
		}
		if rows[i].Mid != rows[j].Mid {
			return rows[i].Mid < rows[j].Mid
		}
		if rows[i].Aid != rows[j].Aid {
			return rows[i].Aid < rows[j].Aid
		}
		return rows[i].SourceType < rows[j].SourceType
	})
	return rows
}

type fakeSettlements struct {
	model.SettlementModel
	db *fakeDB
}

func (f fakeSettlements) FindByRequest(_ context.Context, key string) (*model.Settlement, error) {
	if err := f.db.read("fake:settlements.FindByRequest"); err != nil {
		return nil, err
	}
	for _, s := range f.db.settlements {
		if s.RequestId == key {
			c := *s
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakeSettlements) FindActive(_ context.Context, period string, mid int64) (*model.Settlement, error) {
	if err := f.db.read("fake:settlements.FindActive"); err != nil {
		return nil, err
	}
	s := f.db.liveSettlement(period, mid)
	if s == nil {
		return nil, nil
	}
	c := *s
	return &c, nil
}

// FindOneByNo 复刻 model/cr_settlement.go:170：按 settlement_no 取一行，
// **不带 void_seq 条件**，所以已作废的单照样查得到（作废是审计证据，不是删除）。
func (f fakeSettlements) FindOneByNo(_ context.Context, no string) (*model.Settlement, error) {
	if err := f.db.read("fake:settlements.FindOneByNo"); err != nil {
		return nil, err
	}
	s := f.db.settlementByNo(no)
	if s == nil {
		return nil, nil
	}
	c := *s
	return &c, nil
}

// List 复刻 defaultSettlementModel.List：settlementFilter(period, mid, state)
// + ORDER BY settlement_id DESC（新单优先，model/cr_settlement.go:235，排序事实源在 :247）。
func (f fakeSettlements) List(
	_ context.Context, period string, mid int64, state int32, offset, limit int64,
) ([]*model.Settlement, error) {
	if err := f.db.readPage("fake:settlements.List", offset, limit); err != nil {
		return nil, err
	}
	return paginate(f.db.settlementsMatching(period, mid, state), offset, limit), nil
}

// Count 与 List 共用 settlementFilter（model/cr_settlement.go:321）。
func (f fakeSettlements) Count(
	_ context.Context, period string, mid int64, state int32,
) (int64, error) {
	if err := f.db.read("fake:settlements.Count"); err != nil {
		return 0, err
	}
	return int64(len(f.db.settlementsMatching(period, mid, state))), nil
}

// SumConfirmed 复刻 model/cr_settlement.go:272：
// SUM(amount_minor) WHERE mid = ? AND state = CONFIRMED [AND period >= cutoff]。
// cutoff 为空串表示不开窗（全历史）；VOIDED / DRAFT 一律不计。
func (f fakeSettlements) SumConfirmed(_ context.Context, mid int64, cutoff string) (int64, error) {
	if err := f.db.read("fake:settlements.SumConfirmed"); err != nil {
		return 0, err
	}
	var sum int64
	for _, s := range f.db.settlements {
		if s.Mid != mid || s.State != model.SettlementStateConfirmed {
			continue
		}
		if cutoff != "" && s.Period < cutoff {
			continue
		}
		sum += s.AmountMinor
	}
	return sum, nil
}

// LatestActivePeriod 复刻 model/cr_settlement.go:292：
// WHERE mid = ? AND void_seq = 0 ORDER BY period DESC LIMIT 1；无行回空串。
// 注意过滤位是 void_seq 而不是 state —— 作废路径会把 void_seq 改成本行 id，
// 所以「VOIDED 但 void_seq 仍是 0」的脏行也会被算进在效单，这里是照抄而不是纠偏。
func (f fakeSettlements) LatestActivePeriod(_ context.Context, mid int64) (string, error) {
	if err := f.db.read("fake:settlements.LatestActivePeriod"); err != nil {
		return "", err
	}
	best := ""
	for _, s := range f.db.settlements {
		if s.Mid == mid && s.VoidSeq == 0 && s.Period > best {
			best = s.Period
		}
	}
	return best, nil
}

// settlementsMatching 按 settlementFilter 的三个可选条件取行，再按 settlement_id 倒序。
func (db *fakeDB) settlementsMatching(period string, mid int64, state int32) []*model.Settlement {
	var rows []*model.Settlement
	for _, s := range db.settlements {
		if period != "" && s.Period != period {
			continue
		}
		if mid != 0 && s.Mid != mid {
			continue
		}
		if state != model.SettlementStateUnspecified && s.State != state {
			continue
		}
		c := *s
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].SettlementId > rows[j].SettlementId })
	return rows
}

type fakeItems struct {
	model.SettlementItemModel
	db *fakeDB
}

func (f fakeItems) ListByNo(_ context.Context, no string) ([]*model.SettlementItem, error) {
	if err := f.db.read("fake:settlementItems.ListByNo"); err != nil {
		return nil, err
	}
	return f.db.itemsOf(no), nil
}

type fakeMetricLogs struct {
	model.MetricChangeLogModel
	db *fakeDB
}

// newTestSvc 构造「只换掉数据访问、其余用真实实现」的 ServiceContext。
// 配置里的护栏取值由用例自己给，避免和 etc 默认值耦合。
func newTestSvc(t *testing.T) (*svc.ServiceContext, *fakeDB) {
	t.Helper()
	db := newFakeDB()
	ctx := &svc.ServiceContext{
		Config: config.Config{CreatorRevenue: config.CreatorRevenueConf{
			DefaultCurrency:              "CNY",
			MaxPageSize:                  100,
			MaxRuleUnitPricePer1000Minor: 1_000_000,
			MaxMonthlyCapMinor:           50_000_000,
			GenerateMaxBatch:             500,
			SummaryRecentPeriods:         12,
		}},
		Conn:            fakeConn{db: db},
		Rules:           fakeRules{db: db},
		RuleChanges:     fakeRuleLogs{db: db},
		Enrollments:     fakeEnrollments{db: db},
		Metrics:         &fakeMetrics{db: db},
		MetricChanges:   fakeMetricLogs{db: db},
		Settlements:     fakeSettlements{db: db},
		SettlementItems: fakeItems{db: db},
	}
	return ctx, db
}

// txOf 给「直接调事务内函数」的用例用：返回一个绑在 fakeDB 上的 sqlx.Session。
func txOf(db *fakeDB) sqlx.Session { return &fakeSession{db: db} }
