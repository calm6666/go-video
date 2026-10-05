package model

// live_media_outbox 的 SQL 形状测试。
//
// 这里用的是「记录型假连接」，不是「让测试变绿的假实现」：本文件不断言任何库行为
// （没有库可断言），只断言 model 真正发给驱动的那条语句与实参。之所以必须有这层：
// Outbox 的读侧判据（哪些行算候选）和写侧守卫（什么状态才允许改）是发布器的全部前提，
// 而它们一旦写歪，症状是「事件永远投不出去」或「判死行每轮被重新投递」，
// 都不会让任何一层报错。logic 层的单测把整张表换成了内存替身，覆盖不到这些 SQL。
//
// 先例见 services/coin/model/model_rules_test.go（同一手法）。
//
// 断言里 int 与 int32 不能混用：本表 SET 段的状态值显式写了 int32(...)，
// 而 WHERE 段传的是未类型化常量（默认 int）。两者驱动侧都能正确绑定，
// 但 interface 比较按动态类型判等，所以 WHERE 的实参要和 OutboxStatePending 原样比：
// 加了 int32() 转换会让用例红在自己的断言上，而不是红在 SQL 上。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// --- 记录型 sqlx.SqlConn（不 import 驱动、不连库）---

type recordedCall struct {
	query string
	args  []any
}

type fakeAffected struct{ n int64 }

func (r fakeAffected) LastInsertId() (int64, error) { return 0, nil }
func (r fakeAffected) RowsAffected() (int64, error) { return r.n, nil }

// outboxSQL 只实现本表用到的三个入口，其余一律 panic：
// 「用例其实什么都没断言」必须炸，不能安静通过。
type outboxSQL struct {
	calls      []recordedCall
	execErr    error
	affected   int64
	affectedEr error
	rowsErr    error
	rowErr     error
	count      int64
}

var _ sqlx.SqlConn = (*outboxSQL)(nil)

func (f *outboxSQL) ExecCtx(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.calls = append(f.calls, recordedCall{query: query, args: args})
	if f.execErr != nil {
		return nil, f.execErr
	}
	return fakeResultWith(f.affected, f.affectedEr), nil
}

func fakeResultWith(n int64, err error) sql.Result {
	if err != nil {
		return brokenResult{}
	}
	return fakeAffected{n: n}
}

type brokenResult struct{}

func (brokenResult) LastInsertId() (int64, error) { return 0, nil }
func (brokenResult) RowsAffected() (int64, error) {
	return 0, errors.New("driver 不支持 RowsAffected")
}

func (f *outboxSQL) one(t *testing.T) recordedCall {
	t.Helper()
	if len(f.calls) != 1 {
		t.Fatalf("期望恰好发出 1 条 SQL，实际 %d：%+v", len(f.calls), f.calls)
	}
	return f.calls[0]
}

func (f *outboxSQL) none(t *testing.T) {
	t.Helper()
	if len(f.calls) != 0 {
		t.Fatalf("校验必须在 SQL 之前拦住，却发出了 %d 条 SQL：%+v", len(f.calls), f.calls)
	}
}

func (f *outboxSQL) Exec(query string, args ...any) (sql.Result, error) {
	panic("model 必须走 Ctx 版本：" + query)
}

func (f *outboxSQL) Prepare(query string) (sqlx.StmtSession, error) {
	panic("outbox 不使用预编译语句")
}

func (f *outboxSQL) PrepareCtx(ctx context.Context, query string) (sqlx.StmtSession, error) {
	panic("outbox 不使用预编译语句")
}

func (f *outboxSQL) QueryRowCtx(_ context.Context, v any, query string, args ...any) error {
	f.calls = append(f.calls, recordedCall{query: query, args: args})
	if f.rowErr != nil {
		return f.rowErr
	}
	// 本表只有 CountPending 用单行查询，且只取一个计数。
	if p, ok := v.(*int64); ok {
		*p = f.count
		return nil
	}
	panic("outbox 的单行查询只用于 COUNT(*)，其余列必须走 QueryRowsCtx：" + query)
}

func (f *outboxSQL) QueryRow(v any, query string, args ...any) error {
	panic("outbox 发布路径不做单行查询：" + query)
}

func (f *outboxSQL) QueryRowPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("outbox 不使用 partial 查询")
}

func (f *outboxSQL) QueryRowPartial(v any, query string, args ...any) error {
	panic("outbox 不使用 partial 查询")
}

func (f *outboxSQL) QueryRowsCtx(_ context.Context, _ any, query string, args ...any) error {
	f.calls = append(f.calls, recordedCall{query: query, args: args})
	return f.rowsErr
}

func (f *outboxSQL) QueryRows(v any, query string, args ...any) error {
	panic("model 必须走 Ctx 版本：" + query)
}

func (f *outboxSQL) QueryRowsPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("outbox 不使用 partial 查询")
}

func (f *outboxSQL) QueryRowsPartial(v any, query string, args ...any) error {
	panic("outbox 不使用 partial 查询")
}

func (f *outboxSQL) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	panic("model 层不得自行开事务：Outbox 的事务边界由 logic 的 Transact 决定")
}

func (f *outboxSQL) Transact(fn func(sqlx.Session) error) error {
	panic("model 层不得自行开事务（且必须走 Ctx 版本）")
}

func (f *outboxSQL) RawDB() (*sql.DB, error) {
	return nil, errors.New("outbox 测试不接触真实连接池")
}

func newOutboxModel(conn sqlx.SqlConn) LiveMediaOutboxModel { return NewLiveMediaOutboxModel(conn) }

func validRow() *LiveMediaOutbox {
	return &LiveMediaOutbox{
		EventId: "01ABCDEF0123456789ABCDEF01", EventType: EventTypeRecordStopped,
		SchemaVersion: EventSchemaVersion, AggregateType: AggregateRecordTask,
		AggregateId: "4242", RoomId: 17, Payload: `{"x":1}`,
		State: OutboxStatePending, TraceId: "trace-1",
	}
}

// --- 读侧：候选集只有待发布态 ---

// TestListPendingSelectsOnlyPendingDueRows 钉住死信不会被重新投递。
//
// 这条判据的原始写法是 `state IN (待发布, 失败)`，后果不是「多投一次」而是「死信等于不存在」：
// 判死的行每一轮都回到候选集，同一条坏事件被无限重试，而 state=2 这个「已转人工」的结论
// 在轮询里被反复推翻。MarkRetry 把行留在待发布态并写 next_retry_at，
// 所以「退避中的行仍会被取到」由 next_retry_at 条件负责，不需要靠放宽 state 实现。
func TestListPendingSelectsOnlyPendingDueRows(t *testing.T) {
	db := &outboxSQL{}
	m := newOutboxModel(db)
	if _, err := m.ListPending(context.Background(), 1_700_000_000, 50); err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	call := db.one(t)
	q := strings.Join(strings.Fields(call.query), " ")
	for _, want := range []string{"FROM live_media_outbox", "WHERE state = ? AND next_retry_at <= ? ORDER BY id ASC LIMIT ?"} {
		if !strings.Contains(q, want) {
			t.Errorf("SQL 缺少 %q：%s", want, q)
		}
	}
	if strings.Contains(q, "state IN") {
		t.Errorf("SQL 又把死信纳入候选集：%s", q)
	}
	if len(call.args) != 3 || call.args[0] != OutboxStatePending ||
		call.args[1] != int64(1_700_000_000) || call.args[2] != int32(50) {
		t.Errorf("实参=%v，期望 [0 1700000000 50]（state 只传待发布）", call.args)
	}
}

func TestListPendingClampsNonPositiveLimitAndMapsNoRows(t *testing.T) {
	db := &outboxSQL{}
	if _, err := newOutboxModel(db).ListPending(context.Background(), 1, 0); err != nil {
		t.Fatalf("limit=0 应回落默认值而不是报错：%v", err)
	}
	if got := db.one(t).args[2]; got != int32(100) {
		t.Errorf("limit=%v，期望默认 100", got)
	}

	empty := &outboxSQL{rowsErr: sql.ErrNoRows}
	rows, err := newOutboxModel(empty).ListPending(context.Background(), 1, 10)
	if err != nil || rows != nil {
		t.Errorf("空结果应折叠成 (nil,nil)：(%v,%v)", rows, err)
	}

	broken := &outboxSQL{rowsErr: errors.New("connection reset by peer")}
	if _, err := newOutboxModel(broken).ListPending(context.Background(), 1, 10); err == nil ||
		!strings.Contains(err.Error(), "live_media_outbox ListPending") ||
		!strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("读库错误必须点名表名并保留底层错误：%v", err)
	}
}

// TestCountPendingDeliberatelyIncludesDeadRows 钉住统计口径与候选集口径**刻意不同**。
// 判死的行发布器不再取，但运维必须看得见它堆了多少；
// 两个查询将来若被合并成一份（「顺手复用同一个 WHERE」），本用例即红。
func TestCountPendingDeliberatelyIncludesDeadRows(t *testing.T) {
	db := &outboxSQL{count: 7}
	got, err := newOutboxModel(db).CountPending(context.Background(), 1_700_000_000)
	if err != nil || got != 7 {
		t.Fatalf("CountPending=(%d,%v)，期望 (7,nil)", got, err)
	}
	call := db.one(t)
	if !strings.Contains(call.query, "state IN (?, ?)") {
		t.Errorf("统计必须同时数待发布与死信：%s", call.query)
	}
	if len(call.args) != 3 || call.args[0] != OutboxStatePending ||
		call.args[1] != OutboxStateFailed || call.args[2] != int64(1_700_000_000) {
		t.Errorf("实参=%v，期望 [0 2 1700000000]", call.args)
	}
	// 与候选集判据的差异由 TestListPendingSelectsOnlyPendingDueRows 的另一条断言钉住：
	// 那里的 SQL 里绝不能出现 state IN。
	// ErrNoRows 折叠成 0 是刻意的：COUNT(*) 没有行只可能是驱动异常，报 0 比让指标接口整体失败更可用。
	noRow := &outboxSQL{rowErr: sql.ErrNoRows}
	if n, err := newOutboxModel(noRow).CountPending(context.Background(), 1); err != nil || n != 0 {
		t.Errorf("ErrNoRows 应塌成 (0,nil)：(%d,%v)", n, err)
	}
	broken := &outboxSQL{rowErr: errors.New("read timeout")}
	if _, err := newOutboxModel(broken).CountPending(context.Background(), 1); err == nil ||
		!strings.Contains(err.Error(), "live_media_outbox CountPending") {
		t.Errorf("读库错误必须点名表名：%v", err)
	}
}

// --- 写侧：三个状态推进都有守卫 ---

func TestMarkPublishedWritesPositionAndKeepsStateGuard(t *testing.T) {
	restore := SetClock(func() time.Time { return time.Unix(1_700_000_500, 0) })
	defer restore()

	db := &outboxSQL{affected: 1}
	aff, err := newOutboxModel(db).MarkPublished(context.Background(), 77, 0)
	if err != nil || aff != 1 {
		t.Fatalf("MarkPublished=(%d,%v)", aff, err)
	}
	call := db.one(t)
	if !strings.Contains(call.query, "state = ?") || !strings.Contains(call.query, "published_at = ?") {
		t.Errorf("SQL 缺列：%s", call.query)
	}
	// publishedAt<=0 时用注入时钟兜底，而不是写 0（0 会被 PurgePublished 的 published_at>0 排除）。
	if call.args[1] != int64(1_700_000_500) {
		t.Errorf("published_at=%v，期望注入时钟的 1700000500", call.args[1])
	}
	// WHERE 里必须同时有 id 与「待发布或已判死」两个守卫：
	// 少了 state 守卫就能把已发布的行改回已发布并刷新位点，等于伪造投递时间。
	if got := call.args[len(call.args)-3:]; len(got) != 3 ||
		got[0] != int64(77) || got[1] != OutboxStatePending || got[2] != OutboxStateFailed {
		t.Errorf("WHERE 实参=%v，期望 [77 0 2]", got)
	}
}

func TestMarkPublishedReportsZeroRowsAsZeroNotError(t *testing.T) {
	db := &outboxSQL{affected: 0}
	aff, err := newOutboxModel(db).MarkPublished(context.Background(), 5, 123)
	if err != nil || aff != 0 {
		t.Fatalf("0 行是业务结论不是错误：(%d,%v)", aff, err)
	}
}

func TestMarkPublishedPropagatesDriverAndExecErrors(t *testing.T) {
	broken := &outboxSQL{affected: 1, affectedEr: errors.New("no rows")}
	if _, err := newOutboxModel(broken).MarkPublished(context.Background(), 5, 1); err == nil ||
		!strings.Contains(err.Error(), "RowsAffected") {
		t.Errorf("拿不到 RowsAffected 必须报错，不能塌成 0 行：%v", err)
	}
	exec := &outboxSQL{execErr: errors.New("1406 Data too long")}
	if _, err := newOutboxModel(exec).MarkPublished(context.Background(), 5, 1); err == nil ||
		!strings.Contains(err.Error(), "1406") {
		t.Errorf("写库错误必须原样冒泡：%v", err)
	}
}

// TestMarkRetryIncrementsInSQL 钉住 retry_count 是 SQL 侧自增，不是「覆盖成引擎算出来的值」。
// 适配器因此可以安全地忽略引擎传入的次数；若这里改成写死值，那个前提就不成立。
func TestMarkRetryIncrementsInSQL(t *testing.T) {
	restore := SetClock(func() time.Time { return time.Unix(1_700_000_600, 0) })
	defer restore()

	db := &outboxSQL{affected: 1}
	aff, err := newOutboxModel(db).MarkRetry(context.Background(), 9, 1_700_000_900, "broker down")
	if err != nil || aff != 1 {
		t.Fatalf("MarkRetry=(%d,%v)", aff, err)
	}
	call := db.one(t)
	if !strings.Contains(call.query, "retry_count = retry_count + 1") {
		t.Errorf("retry_count 必须是 SQL 自增（多副本下不能互相覆盖计数）：%s", call.query)
	}
	// SET 段的实参只有 state、next_retry_at、last_error（retry_count 走 SQL 表达式，不占位），
	// 位置错了就是把 last_error 写进 next_retry_at，而 ListPending 会把这行当成「立即可投」。
	if len(call.args) != 7 {
		t.Fatalf("实参个数=%d，期望 7（3 个 SET + mtime + 3 个 WHERE）：%v", len(call.args), call.args)
	}
	if call.args[0] != int32(OutboxStatePending) || call.args[1] != int64(1_700_000_900) ||
		call.args[2] != "broker down" {
		t.Errorf("SET 实参=%v，期望 [state=0, next_retry_at=1700000900, last_error=broker down]", call.args)
	}
	if got := call.args[len(call.args)-3:]; got[0] != int64(9) ||
		got[1] != OutboxStatePending || got[2] != OutboxStateFailed {
		t.Errorf("WHERE=%v，期望 [9 0 2]（保留 state ∈ 待发布/失败 的守卫）", got)
	}
}

func TestMarkRetryDefaultsNextRetryAtToNow(t *testing.T) {
	restore := SetClock(func() time.Time { return time.Unix(1_700_000_700, 0) })
	defer restore()
	db := &outboxSQL{affected: 1}
	if _, err := newOutboxModel(db).MarkRetry(context.Background(), 1, 0, "err"); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
	// next_retry_at=0 会被 ListPending 当作「立即到期」，退避直接失效，所以必须兜底成当前时间。
	if got := db.one(t).args[1]; got != int64(1_700_000_700) {
		t.Errorf("next_retry_at=%v，期望 1700000700", got)
	}
}

// TestMarkFailedOnlyFromPending 钉住死信单向：已发布的行不能被判死改写。
func TestMarkFailedOnlyFromPending(t *testing.T) {
	db := &outboxSQL{affected: 1}
	if _, err := newOutboxModel(db).MarkFailed(context.Background(), 3, "exhausted"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	call := db.one(t)
	if !strings.Contains(call.query, "state = ?") || strings.Contains(call.query, "state IN") {
		t.Errorf("判死必须只从待发布态出发：%s", call.query)
	}
	if got := call.args[len(call.args)-2:]; got[0] != int64(3) || got[1] != OutboxStatePending {
		t.Errorf("WHERE=%v，期望 [3 0]", got)
	}
}

// --- last_error 夹取：字节数与 UTF-8 边界 ---

// TestTrimLastErrorNeverSplitsRune 钉住夹取按字符边界收口。
// 非法序列写进 utf8mb4 列会让 MarkRetry/MarkFailed 自己失败，
// 而发布引擎把状态写库失败当作整批中断，故障面从一行放大到一批。
func TestTrimLastErrorNeverSplitsRune(t *testing.T) {
	short := "livemedia: 投递失败"
	if got := trimLastError(short); got != short {
		t.Errorf("未超限的串被改写了：%q", got)
	}

	ascii := strings.Repeat("a", 600)
	if got := trimLastError(ascii); len(got) != 512 {
		t.Errorf("ASCII 夹取长度=%d，期望 512", len(got))
	}

	// 3 字节汉字：把边界正好压在字符中间（511 个 ASCII + 一个汉字 = 514 字节）。
	straddling := strings.Repeat("a", 511) + "汉" + strings.Repeat("b", 20)
	got := trimLastError(straddling)
	if !utf8.ValidString(got) {
		t.Fatalf("夹取结果不是合法 UTF-8：%q", got)
	}
	if len(got) != 511 || strings.HasSuffix(got, "汉") {
		t.Errorf("长度=%d 结尾=%q，期望退到 511 字节并整字符丢弃「汉」", len(got), got[len(got)-3:])
	}

	// 全中文串：512 字节落在第 171 个汉字中间。
	allChinese := strings.Repeat("播", 300)
	got = trimLastError(allChinese)
	if !utf8.ValidString(got) || len(got) != 510 {
		t.Errorf("全中文夹取=(len=%d,valid=%v)，期望 510 字节且合法", len(got), utf8.ValidString(got))
	}

	// MarkRetry/MarkFailed 必须真的走这条夹取（长度与合法性都落到 SQL 实参上）。
	db := &outboxSQL{affected: 1}
	if _, err := newOutboxModel(db).MarkFailed(context.Background(), 1, straddling); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	stored := db.one(t).args[1].(string)
	if !utf8.ValidString(stored) || len(stored) > 512 {
		t.Errorf("落库的 last_error 不合格：len=%d valid=%v", len(stored), utf8.ValidString(stored))
	}
}

// --- 登记侧：校验先于 SQL，列序与实参逐位对应 ---

func TestInsertRejectsIncompleteRowBeforeSQL(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*LiveMediaOutbox)
	}{
		{"缺 event_id", func(e *LiveMediaOutbox) { e.EventId = "" }},
		{"缺 event_type", func(e *LiveMediaOutbox) { e.EventType = "" }},
		{"缺聚合类型", func(e *LiveMediaOutbox) { e.AggregateType = "" }},
		{"缺聚合主键", func(e *LiveMediaOutbox) { e.AggregateId = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := validRow()
			tc.mutate(row)
			db := &outboxSQL{}
			err := newOutboxModel(db).Insert(context.Background(), nil, row)
			if !errors.Is(err, ErrEmptyEventPayload) {
				t.Errorf("应返回 ErrEmptyEventPayload：%v", err)
			}
			db.none(t)
		})
	}
}

func TestInsertColumnOrderMatchesArgs(t *testing.T) {
	db := &outboxSQL{}
	row := validRow()
	if err := newOutboxModel(db).Insert(context.Background(), nil, row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	call := db.one(t)
	in := strings.Index(call.query, ") VALUES (")
	cols := strings.Split(call.query[strings.Index(call.query, "INSERT INTO live_media_outbox (")+len("INSERT INTO live_media_outbox ("):in], ", ")
	if len(cols) != len(call.args) {
		t.Fatalf("列数 %d != 实参数 %d：%s", len(cols), len(call.args), call.query)
	}
	// 逐位比对：错一位就是把 payload 写进 aggregate_id，编译期和内存替身都发现不了。
	want := map[string]any{
		"event_id": row.EventId, "event_type": row.EventType, "schema_version": int32(EventSchemaVersion),
		"aggregate_type": row.AggregateType, "aggregate_id": row.AggregateId, "room_id": row.RoomId,
		"payload": row.Payload, "state": int32(OutboxStatePending), "retry_count": int32(0),
		"next_retry_at": int64(0), "last_error": "", "trace_id": row.TraceId,
	}
	for i, col := range cols {
		exp, ok := want[col]
		if !ok {
			continue
		}
		if call.args[i] != exp {
			t.Errorf("列 %s 的第 %d 个实参=%v，期望 %v", col, i, call.args[i], exp)
		}
	}
	// occurred_at / published_at / ctime / mtime 由 model 兜底，必须都落在时间列上。
	for i, col := range cols {
		switch col {
		case "occurred_at", "ctime", "mtime":
			if v, ok := call.args[i].(int64); !ok || v == 0 {
				t.Errorf("列 %s 实参=%v，期望兜底成非零 Unix 秒", col, call.args[i])
			}
		case "published_at":
			if call.args[i] != int64(0) {
				t.Errorf("新登记的事件 published_at=%v，期望 0（未投递）", call.args[i])
			}
		}
	}
}

func TestInsertMapsDuplicateToErrRequestIdDuplicated(t *testing.T) {
	db := &outboxSQL{execErr: errors.New("Error 1062 (23000): Duplicate entry 'x' for key 'uniq_event_id'")}
	err := newOutboxModel(db).Insert(context.Background(), nil, validRow())
	if !errors.Is(err, ErrRequestIdDuplicated) {
		t.Errorf("event_id 冲突应映射成 ErrRequestIdDuplicated（logic 据此判幂等）：%v", err)
	}
}
