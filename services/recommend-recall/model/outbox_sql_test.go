package model

// outbox_sql_test.go 是 recall_outbox 的 SQL 形状测试。
//
// 这里用的是「记录型假连接」，不是「让测试变绿的假实现」：本文件不断言任何库行为
//（没有库可断言），只断言 model 真正发给驱动的那条语句与实参。之所以必须有这层：
// Outbox 的读侧判据（哪些行算候选）和写侧守卫（什么状态才允许改）是发布器的全部前提，
// 而它们一旦写歪，症状是「事件永远投不出去」或「判死行每轮被重新投递」，
// 都不会让任何一层报错。internal/logic 的单测把整张表换成了内存替身，覆盖不到这些 SQL。
//
// 先例见 services/live-media/model/live_media_outbox_sql_test.go 与
// services/coin/model/model_rules_test.go（同一手法）。
//
// 断言里 int 与 int32 不能混用：OutboxStatePending 等常量带 int32 类型，
// 而 WHERE 段传的是未类型化常量（默认 int）。interface 比较按动态类型判等，
// 所以实参要和常量原样比：加转换会让用例红在自己的断言上，而不是红在 SQL 上。

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

type obxCall struct {
	query string
	args  []any
}

type obxResult struct {
	n   int64
	err error
}

func (r obxResult) LastInsertId() (int64, error) { return 0, nil }
func (r obxResult) RowsAffected() (int64, error) { return r.n, r.err }

// obxBrokenResult 模拟「驱动不支持 RowsAffected」：model 必须报错而不是塌成 0 行。
type obxBrokenResult struct{}

func (obxBrokenResult) LastInsertId() (int64, error) { return 0, nil }
func (obxBrokenResult) RowsAffected() (int64, error) {
	return 0, errors.New("driver 不支持 RowsAffected")
}

// obxSQL 只实现本表用到的入口，其余一律 panic：
// 「用例其实什么都没断言」必须炸，不能安静通过。
type obxSQL struct {
	calls       []obxCall
	execErr     error
	affected    int64
	affectedErr error
	rowsErr     error
	rowErr      error
	count       int64
}

var _ sqlx.SqlConn = (*obxSQL)(nil)

func (f *obxSQL) ExecCtx(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.calls = append(f.calls, obxCall{query: query, args: args})
	if f.execErr != nil {
		return nil, f.execErr
	}
	if f.affectedErr != nil {
		return obxBrokenResult{}, nil
	}
	return obxResult{n: f.affected}, nil
}

func (f *obxSQL) one(t *testing.T) obxCall {
	t.Helper()
	if len(f.calls) != 1 {
		t.Fatalf("期望恰好发出 1 条 SQL，实际 %d：%+v", len(f.calls), f.calls)
	}
	return f.calls[0]
}

func (f *obxSQL) none(t *testing.T) {
	t.Helper()
	if len(f.calls) != 0 {
		t.Fatalf("校验必须在 SQL 之前拦住，却发出了 %d 条 SQL：%+v", len(f.calls), f.calls)
	}
}

func (f *obxSQL) Exec(query string, args ...any) (sql.Result, error) {
	panic("model 必须走 Ctx 版本：" + query)
}

func (f *obxSQL) Prepare(query string) (sqlx.StmtSession, error) {
	panic("outbox 不使用预编译语句")
}

func (f *obxSQL) PrepareCtx(ctx context.Context, query string) (sqlx.StmtSession, error) {
	panic("outbox 不使用预编译语句")
}

func (f *obxSQL) QueryRowCtx(_ context.Context, v any, query string, args ...any) error {
	f.calls = append(f.calls, obxCall{query: query, args: args})
	if f.rowErr != nil {
		return f.rowErr
	}
	// 本表只有 CountPending / CountStuck 用单行查询，且只取一个计数。
	if p, ok := v.(*int64); ok {
		*p = f.count
		return nil
	}
	panic("outbox 的单行查询只用于 COUNT(*)，其余列必须走 QueryRowsCtx：" + query)
}

func (f *obxSQL) QueryRow(v any, query string, args ...any) error {
	panic("outbox 发布路径不做单行查询：" + query)
}

func (f *obxSQL) QueryRowPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("outbox 不使用 partial 查询")
}

func (f *obxSQL) QueryRowPartial(v any, query string, args ...any) error {
	panic("outbox 不使用 partial 查询")
}

func (f *obxSQL) QueryRowsCtx(_ context.Context, _ any, query string, args ...any) error {
	f.calls = append(f.calls, obxCall{query: query, args: args})
	return f.rowsErr
}

func (f *obxSQL) QueryRows(v any, query string, args ...any) error {
	panic("model 必须走 Ctx 版本：" + query)
}

func (f *obxSQL) QueryRowsPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("outbox 不使用 partial 查询")
}

func (f *obxSQL) QueryRowsPartial(v any, query string, args ...any) error {
	panic("outbox 不使用 partial 查询")
}

func (f *obxSQL) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	panic("model 层不得自行开事务：Outbox 的事务边界由 logic 的 Transact 决定")
}

func (f *obxSQL) Transact(fn func(sqlx.Session) error) error {
	panic("model 层不得自行开事务（且必须走 Ctx 版本）")
}

func (f *obxSQL) RawDB() (*sql.DB, error) {
	return nil, errors.New("outbox 测试不接触真实连接池")
}

// obxTx 是事务 session 的记录端：只有 ExecCtx 可用。
// 单独一个类型是为了能把「Insert 落在全局 conn 上」和「Insert 落在调用方事务上」
// 分开放进两个记录器比对，而不是靠 ctx 里的暗号。
type obxTx struct {
	calls []obxCall
}

var _ sqlx.Session = (*obxTx)(nil)

func (t *obxTx) ExecCtx(_ context.Context, query string, args ...any) (sql.Result, error) {
	t.calls = append(t.calls, obxCall{query: query, args: args})
	return obxResult{n: 1}, nil
}

func (t *obxTx) Exec(query string, args ...any) (sql.Result, error) {
	panic("model 必须走 Ctx 版本：" + query)
}

func (t *obxTx) Prepare(query string) (sqlx.StmtSession, error) {
	panic("事务登记不使用预编译语句")
}

func (t *obxTx) PrepareCtx(ctx context.Context, query string) (sqlx.StmtSession, error) {
	panic("事务登记不使用预编译语句")
}

func (t *obxTx) QueryRow(v any, query string, args ...any) error {
	panic("事务登记不读库：" + query)
}

func (t *obxTx) QueryRowCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("事务登记不读库")
}

func (t *obxTx) QueryRowPartial(v any, query string, args ...any) error {
	panic("事务登记不读库")
}

func (t *obxTx) QueryRowPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("事务登记不读库")
}

func (t *obxTx) QueryRows(v any, query string, args ...any) error {
	panic("事务登记不读库")
}

func (t *obxTx) QueryRowsCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("事务登记不读库")
}

func (t *obxTx) QueryRowsPartial(v any, query string, args ...any) error {
	panic("事务登记不读库")
}

func (t *obxTx) QueryRowsPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("事务登记不读库")
}

func newOutbox(conn sqlx.SqlConn) RecallOutboxModel { return NewRecallOutboxModel(conn) }

func validOutboxRow() *RecallOutbox {
	return &RecallOutbox{
		EventID: "01ABCDEF0123456789ABCDEF01", EventType: EventPoolPublished,
		SchemaVersion: PoolVersionSchemaVersion, AggregateType: AggregateTypePool,
		AggregateID: "1:hot:7", Payload: `{"x":1}`, State: OutboxStatePending,
	}
}

// --- 读侧：候选集只有待发布态 ---

// TestListPendingSelectsOnlyPendingDueRows 钉住死信不会被重新投递。
//
// 这条判据此前的写法是 `state IN (待发布, 失败)` 加「失败行也取」，
// 后果不是「多投一次」而是「死信等于不存在」：判死的行每一轮都回到候选集，
// 同一条坏事件被无限重试，而 state=2 这个「已转人工」的结论在轮询里被反复推翻
// （本轮接线要修的就是这一条）。MarkRetry 把行留在待发布态并写 next_retry_at，
// 所以「退避中的行仍会被取到」由 next_retry_at 条件负责，不需要靠放宽 state 实现。
func TestListPendingSelectsOnlyPendingDueRows(t *testing.T) {
	db := &obxSQL{}
	if _, err := newOutbox(db).ListPending(context.Background(), 1_700_000_000, 50); err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	call := db.one(t)
	q := strings.Join(strings.Fields(call.query), " ")
	for _, want := range []string{"FROM recall_outbox", "WHERE state = ? AND next_retry_at <= ? ORDER BY id ASC LIMIT ?"} {
		if !strings.Contains(q, want) {
			t.Errorf("SQL 缺少 %q：%s", want, q)
		}
	}
	if strings.Contains(q, "state IN") {
		t.Errorf("SQL 又把死信纳入候选集：%s", q)
	}
	if !strings.Contains(q, "ORDER BY id ASC") {
		t.Errorf("SQL 丢了 id 升序：同池事件会乱序投递（消费方按 event_id 去重但无从判序）：%s", q)
	}
	if len(call.args) != 3 || call.args[0] != OutboxStatePending ||
		call.args[1] != int64(1_700_000_000) || call.args[2] != int32(50) {
		t.Errorf("实参=%v，期望 [0 1700000000 50]（state 只传待发布）", call.args)
	}
}

// TestListPendingLimitBoundaries 钉住批次上限的两侧：
// 上限本身放行、超限拒绝且不发 SQL。超限若被放行，MySQL 会把 LIMIT 当合法值照单全收，
// 发布器一次锁住整张表；若上限被拒，运维把批次配到最大值反而每轮报错。
func TestListPendingLimitBoundaries(t *testing.T) {
	db := &obxSQL{}
	if _, err := newOutbox(db).ListPending(context.Background(), 1, int32(MaxOutboxBatch)); err != nil {
		t.Errorf("limit=%d（等于上限）应放行：%v", MaxOutboxBatch, err)
	}

	for _, limit := range []int32{0, -1, int32(MaxOutboxBatch) + 1} {
		bounded := &obxSQL{}
		_, err := newOutbox(bounded).ListPending(context.Background(), 1, limit)
		if err == nil {
			t.Fatalf("limit=%d 必须被拒", limit)
		}
		if limit > int32(MaxOutboxBatch) {
			if !errors.Is(err, ErrLimitTooLarge) {
				t.Errorf("超限应返回 ErrLimitTooLarge：%v", err)
			}
		} else if !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("非正 limit 应返回 ErrInvalidLimit：%v", err)
		}
		bounded.none(t)
	}
}

func TestListPendingMapsNoRowsAndWrapsReadError(t *testing.T) {
	empty := &obxSQL{rowsErr: sql.ErrNoRows}
	rows, err := newOutbox(empty).ListPending(context.Background(), 1, 10)
	if err != nil || rows != nil {
		t.Errorf("空结果应折叠成 (nil,nil)：(%v,%v)", rows, err)
	}

	broken := &obxSQL{rowsErr: errors.New("connection reset by peer")}
	if _, err := newOutbox(broken).ListPending(context.Background(), 1, 10); err == nil ||
		!strings.Contains(err.Error(), "recall_outbox ListPending") ||
		!strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("读库错误必须点名表名并保留底层错误：%v", err)
	}
}

// --- 写侧：三个状态推进都有守卫 ---

// TestMarkPublishedWritesMtimeAndKeepsStateGuard 钉住本表的发布位点。
//
// recall_outbox 没有 published_at 列（迁移 000005 的列清单里没有），
// 所以发布时间只能落在 mtime 上；publishedAt<=0 时用真实时钟兜底而不是写 0，
// 否则 DeleteSentBefore / 排障看到的「什么时候发的」会退化成 1970。
// WHERE 必须同时有 id 与「待发布或已判死」两个守卫：
// 少了 state 守卫，拿到一个 id 就能把已发布的行改回已发布并刷新位点，等于伪造投递时间。
func TestMarkPublishedWritesMtimeAndKeepsStateGuard(t *testing.T) {
	db := &obxSQL{affected: 1}
	before := time.Now().Unix()
	aff, err := newOutbox(db).MarkPublished(context.Background(), 77, 0)
	after := time.Now().Unix()
	if err != nil || aff != 1 {
		t.Fatalf("MarkPublished=(%d,%v)", aff, err)
	}
	call := db.one(t)
	q := strings.Join(strings.Fields(call.query), " ")
	for _, want := range []string{"UPDATE recall_outbox SET state = ?, last_error = '', mtime = ? WHERE id = ? AND state IN (?, ?)"} {
		if !strings.Contains(q, want) {
			t.Errorf("SQL 缺少 %q：%s", want, q)
		}
	}
	if len(call.args) != 5 {
		t.Fatalf("实参数=%d，期望 5（2 个 SET + 3 个 WHERE）：%v", len(call.args), call.args)
	}
	if call.args[0] != OutboxStateSent {
		t.Errorf("SET state=%v，期望已发布态 1", call.args[0])
	}
	if got := call.args[1].(int64); got < before || got > after {
		t.Errorf("mtime=%d，期望落在 [%d,%d]（publishedAt=0 必须兜底成当前时间）", got, before, after)
	}
	if call.args[2] != int64(77) || call.args[3] != OutboxStatePending || call.args[4] != OutboxStateFailed {
		t.Errorf("WHERE=%v，期望 [77 0 2]", call.args[2:])
	}
	// 显式传位点时不得被当前时间覆盖：调用方（发布引擎）给的时刻才是投递时刻。
	fixed := &obxSQL{affected: 1}
	if _, err := newOutbox(fixed).MarkPublished(context.Background(), 5, 1234); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	if got := fixed.one(t).args[1]; got != int64(1234) {
		t.Errorf("mtime=%v，期望原样透传 1234", got)
	}
}

func TestMarkPublishedReportsZeroRowsAsZeroNotError(t *testing.T) {
	db := &obxSQL{affected: 0}
	aff, err := newOutbox(db).MarkPublished(context.Background(), 5, 123)
	if err != nil || aff != 0 {
		t.Fatalf("0 行是业务结论不是错误：(%d,%v)", aff, err)
	}
}

func TestMarkPublishedPropagatesDriverAndExecErrors(t *testing.T) {
	broken := &obxSQL{affected: 1, affectedErr: errors.New("no rows")}
	if _, err := newOutbox(broken).MarkPublished(context.Background(), 5, 1); err == nil ||
		!strings.Contains(err.Error(), "RowsAffected") {
		t.Errorf("拿不到 RowsAffected 必须报错，不能塌成 0 行：%v", err)
	}
	exec := &obxSQL{execErr: errors.New("1406 Data too long")}
	if _, err := newOutbox(exec).MarkPublished(context.Background(), 5, 1); err == nil ||
		!strings.Contains(err.Error(), "recall_outbox MarkPublished") ||
		!strings.Contains(err.Error(), "1406") {
		t.Errorf("写库错误必须点名表名与原样冒泡：%v", err)
	}
}

// TestMarkRetryKeepsRowPendingAndIncrementsInSQL 钉住两件事：
//  1. SET 的 state 仍是待发布（否则一次失败就把行踢出候选集，退避形同判死）；
//  2. retry_count 是 SQL 侧自增，不是「覆盖成引擎算出来的值」。
//     发布器适配层因此可以安全地忽略引擎传入的次数；若这里改成写死值，那个前提就不成立。
func TestMarkRetryKeepsRowPendingAndIncrementsInSQL(t *testing.T) {
	db := &obxSQL{affected: 1}
	aff, err := newOutbox(db).MarkRetry(context.Background(), 9, 1_700_000_900, "broker down")
	if err != nil || aff != 1 {
		t.Fatalf("MarkRetry=(%d,%v)", aff, err)
	}
	call := db.one(t)
	if !strings.Contains(call.query, "retry_count = retry_count + 1") {
		t.Errorf("retry_count 必须是 SQL 自增（多副本下不能互相覆盖计数）：%s", call.query)
	}
	// SET 段的实参只有 state、next_retry_at、last_error、mtime（retry_count 走 SQL 表达式，不占位），
	// 位置错了就是把 last_error 写进 next_retry_at，而 ListPending 会把这行当成「立即可投」。
	if len(call.args) != 7 {
		t.Fatalf("实参数=%d，期望 7（4 个 SET + 3 个 WHERE）：%v", len(call.args), call.args)
	}
	if call.args[0] != OutboxStatePending || call.args[1] != int64(1_700_000_900) ||
		call.args[2] != "broker down" {
		t.Errorf("SET 实参=%v，期望 [state=0, next_retry_at=1700000900, last_error=broker down]", call.args[:3])
	}
	if got := call.args[len(call.args)-3:]; got[0] != int64(9) ||
		got[1] != OutboxStatePending || got[2] != OutboxStateFailed {
		t.Errorf("WHERE=%v，期望 [9 0 2]（保留 state ∈ 待发布/失败 的守卫）", got)
	}
}

func TestMarkRetryDefaultsNextRetryAtToNow(t *testing.T) {
	db := &obxSQL{affected: 1}
	before := time.Now().Unix()
	if _, err := newOutbox(db).MarkRetry(context.Background(), 1, 0, "err"); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
	after := time.Now().Unix()
	// next_retry_at=0 会被 ListPending 当作「立即到期」，退避直接失效，所以必须兜底成当前时间。
	if got := db.one(t).args[1].(int64); got < before || got > after {
		t.Errorf("next_retry_at=%d，期望落在 [%d,%d]", got, before, after)
	}
}

// TestMarkFailedOnlyFromPending 钉住死信单向：已发布的行不能被判死改写，
// 第二次判死也不能覆盖第一次的 last_error（人工排障要看到的是判死那一次的错误）。
func TestMarkFailedOnlyFromPending(t *testing.T) {
	db := &obxSQL{affected: 1}
	if _, err := newOutbox(db).MarkFailed(context.Background(), 3, "exhausted"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	call := db.one(t)
	if !strings.Contains(call.query, "state = ?") || strings.Contains(call.query, "state IN") {
		t.Errorf("判死必须只从待发布态出发：%s", call.query)
	}
	if len(call.args) != 5 || call.args[0] != OutboxStateFailed || call.args[1] != "exhausted" {
		t.Errorf("SET 实参=%v，期望 [2, exhausted, ...]", call.args[:2])
	}
	if got := call.args[len(call.args)-2:]; got[0] != int64(3) || got[1] != OutboxStatePending {
		t.Errorf("WHERE=%v，期望 [3 0]", got)
	}
}

// --- 统计与清理：口径与候选集刻意不同 ---

// TestCountPendingDeliberatelyIncludesDeadRows 钉住统计口径与候选集口径**刻意不同**。
// 判死的行发布器不再取，但运维必须看得见它堆了多少；
// 两个查询将来若被合并成一份（「顺手复用同一个 WHERE」），本用例即红。
func TestCountPendingDeliberatelyIncludesDeadRows(t *testing.T) {
	db := &obxSQL{count: 7}
	got, err := newOutbox(db).CountPending(context.Background())
	if err != nil || got != 7 {
		t.Fatalf("CountPending=(%d,%v)，期望 (7,nil)", got, err)
	}
	call := db.one(t)
	if !strings.Contains(call.query, "state IN (?, ?)") {
		t.Errorf("统计必须同时数待发布与死信：%s", call.query)
	}
	if len(call.args) != 2 || call.args[0] != OutboxStatePending || call.args[1] != OutboxStateFailed {
		t.Errorf("实参=%v，期望 [0 2]", call.args)
	}
	// ErrNoRows 折叠成 0 是刻意的：COUNT(*) 没有行只可能是驱动异常，
	// 报 0 比让指标接口整体失败更可用。
	noRow := &obxSQL{rowErr: sql.ErrNoRows}
	if n, err := newOutbox(noRow).CountPending(context.Background()); err != nil || n != 0 {
		t.Errorf("ErrNoRows 应塌成 (0,nil)：(%d,%v)", n, err)
	}
	broken := &obxSQL{rowErr: errors.New("read timeout")}
	if _, err := newOutbox(broken).CountPending(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "recall_outbox CountPending") {
		t.Errorf("读库错误必须点名表名：%v", err)
	}
}

// TestCountStuckCoversDeadRowsToo 钉住滞留告警同样看得见死信：
// 判死等于「停止自动投递」，从告警视角它与从未投出去的积压是同一件事。
func TestCountStuckCoversDeadRowsToo(t *testing.T) {
	db := &obxSQL{count: 3}
	got, err := newOutbox(db).CountStuck(context.Background(), 1_700_000_000)
	if err != nil || got != 3 {
		t.Fatalf("CountStuck=(%d,%v)，期望 (3,nil)", got, err)
	}
	call := db.one(t)
	if !strings.Contains(call.query, "occurred_at < ?") {
		t.Errorf("滞留判定必须按事件产生时间而不是 mtime（退避会不停刷 mtime）：%s", call.query)
	}
	if len(call.args) != 3 || call.args[0] != OutboxStatePending ||
		call.args[1] != OutboxStateFailed || call.args[2] != int64(1_700_000_000) {
		t.Errorf("实参=%v，期望 [0 2 1700000000]", call.args)
	}
	bad := &obxSQL{count: 9}
	if _, err := newOutbox(bad).CountStuck(context.Background(), 0); !errors.Is(err, ErrInvalidLimit) {
		t.Errorf("before=0 会让统计变成全表计数，必须拒掉：%v", err)
	}
	bad.none(t)
}

// TestDeleteSentBeforeOnlyTouchesPublishedRows 钉住清理不销毁证据：
// 待发布与死信行必须保留，否则「这条事件为什么没投出去」在库里查不到任何痕迹。
func TestDeleteSentBeforeOnlyTouchesPublishedRows(t *testing.T) {
	db := &obxSQL{affected: 12}
	deleted, err := newOutbox(db).DeleteSentBefore(context.Background(), 1_700_000_000, 500)
	if err != nil || deleted != 12 {
		t.Fatalf("DeleteSentBefore=(%d,%v)，期望 (12,nil)", deleted, err)
	}
	call := db.one(t)
	if !strings.HasPrefix(call.query, "DELETE FROM recall_outbox WHERE state = ?") {
		t.Errorf("删除必须带 state 条件：%s", call.query)
	}
	if strings.Contains(call.query, "state IN") {
		t.Errorf("删除不得波及待发布/死信行：%s", call.query)
	}
	if len(call.args) != 3 || call.args[0] != OutboxStateSent ||
		call.args[1] != int64(1_700_000_000) || call.args[2] != int64(500) {
		t.Errorf("实参=%v，期望 [1 1700000000 500]", call.args)
	}

	limit := &obxSQL{}
	_, err = newOutbox(limit).DeleteSentBefore(context.Background(), 1, int64(MaxDeleteRows)+1)
	if !errors.Is(err, ErrLimitTooLarge) {
		t.Errorf("无上限 DELETE 会长时间持锁并放大主从延迟，必须拒绝：%v", err)
	}
	limit.none(t)
	if _, err := newOutbox(limit).DeleteSentBefore(context.Background(), 0, 10); !errors.Is(err, ErrInvalidLimit) {
		t.Errorf("before=0 等于删除全部已发布行，必须拒掉：%v", err)
	}
	limit.none(t)
}

// --- last_error 夹取：字节数与 UTF-8 边界 ---

// TestTrimLastErrorNeverSplitsRune 钉住夹取按字符边界收口。
// 非法序列写进 utf8mb4 列会让 MarkRetry/MarkFailed 自己失败，
// 而发布引擎把状态写库失败当作整批中断，故障面从一行放大到一批。
func TestTrimLastErrorNeverSplitsRune(t *testing.T) {
	short := "recommend-recall: 投递失败"
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
	db := &obxSQL{affected: 1}
	if _, err := newOutbox(db).MarkFailed(context.Background(), 1, straddling); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	stored := db.one(t).args[1].(string)
	if !utf8.ValidString(stored) || len(stored) > 512 {
		t.Errorf("落库的 last_error 不合格：len=%d valid=%v", len(stored), utf8.ValidString(stored))
	}
}

// --- 登记侧：校验先于 SQL，列序与实参逐位对应，事务归属可查 ---

func TestInsertRejectsIncompleteRowBeforeSQL(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RecallOutbox)
	}{
		{"缺 event_id", func(e *RecallOutbox) { e.EventID = "" }},
		{"event_id 只有空白", func(e *RecallOutbox) { e.EventID = "  " }},
		{"缺 event_type", func(e *RecallOutbox) { e.EventType = "" }},
		{"event_type 只有空白", func(e *RecallOutbox) { e.EventType = " " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := validOutboxRow()
			tc.mutate(row)
			db := &obxSQL{}
			err := newOutbox(db).Insert(context.Background(), nil, row)
			if !errors.Is(err, ErrEventRequired) {
				t.Errorf("应返回 ErrEventRequired：%v", err)
			}
			db.none(t)
		})
	}
}

func TestInsertColumnOrderMatchesArgs(t *testing.T) {
	db := &obxSQL{}
	row := validOutboxRow()
	row.OccurredAt = 1_700_000_000
	if err := newOutbox(db).Insert(context.Background(), nil, row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	call := db.one(t)
	prefix := "INSERT INTO recall_outbox ("
	in := strings.Index(call.query, ") VALUES (")
	cols := strings.Split(call.query[strings.Index(call.query, prefix)+len(prefix):in], ", ")
	if len(cols) != len(call.args) {
		t.Fatalf("列数 %d != 实参数 %d：%s", len(cols), len(call.args), call.query)
	}
	// 逐位比对：错一位就是把 payload 写进 aggregate_id，编译期和内存替身都发现不了。
	want := map[string]any{
		"event_id": row.EventID, "event_type": row.EventType,
		"schema_version": int32(PoolVersionSchemaVersion), "aggregate_type": row.AggregateType,
		"aggregate_id": row.AggregateID, "payload": row.Payload,
		"state": OutboxStatePending, "retry_count": int32(0), "next_retry_at": int64(0),
		"occurred_at": int64(1_700_000_000), "last_error": "",
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
	// ctime / mtime 由 model 兜底成同一刻：两者相差一轮的话，
	// 「登记时间」与「最后修改时间」会让排障误判成「这行被改过」。
	for i, col := range cols {
		switch col {
		case "ctime", "mtime":
			v, ok := call.args[i].(int64)
			if !ok || v == 0 {
				t.Errorf("列 %s 实参=%v，期望兜底成非零 Unix 秒", col, call.args[i])
			}
		}
	}
	// 未登记的三个状态位必须全是初始值：state 非 0 会让这行永远进不了候选集。
	if call.args[indexOfCol(cols, "next_retry_at")] != int64(0) {
		t.Error("新登记事件的 next_retry_at 必须为 0（立即可投）")
	}
}

// TestInsertDefaultsSchemaVersion 钉住 schema_version=0 回落成当前版本：
// 落 0 的话 topic 会拼成 recall.pool.published.v0（eventenvelope.Topic 返回空串），
// 生产侧照样提交，发布侧整类事件判死。
func TestInsertDefaultsSchemaVersion(t *testing.T) {
	db := &obxSQL{}
	row := validOutboxRow()
	row.SchemaVersion = 0
	if err := newOutbox(db).Insert(context.Background(), nil, row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got := db.one(t).args[2]; got != int32(PoolVersionSchemaVersion) {
		t.Errorf("schema_version=%v，期望兜底成 %d", got, PoolVersionSchemaVersion)
	}
}

// TestInsertUsesCallerTransaction 钉住事务归属：
// 传 session 时 INSERT 必须发在调用方事务上、全局连接一条都不发。
// 这是 Outbox 的成立前提（AGENTS.md §5）：写在事务外会出现
// 「池版本已切换但事件没登记」或「事件登记了但切换回滚」两种都对不上的状态。
func TestInsertUsesCallerTransaction(t *testing.T) {
	conn := &obxSQL{}
	tx := &obxTx{}
	if err := newOutbox(conn).Insert(context.Background(), tx, validOutboxRow()); err != nil {
		t.Fatalf("Insert(事务): %v", err)
	}
	conn.none(t)
	if len(tx.calls) != 1 {
		t.Fatalf("事务上应恰好发 1 条 SQL，实际 %d", len(tx.calls))
	}
	if !strings.HasPrefix(tx.calls[0].query, "INSERT INTO recall_outbox (") {
		t.Errorf("事务里的语句不是登记事件：%s", tx.calls[0].query)
	}

	// session 为 nil 是补偿脚本的入口，仍要能写：走全局连接（自动提交）。
	standalone := &obxSQL{affected: 1}
	if err := newOutbox(standalone).Insert(context.Background(), nil, validOutboxRow()); err != nil {
		t.Fatalf("Insert(自动提交): %v", err)
	}
	if got := standalone.one(t).query; !strings.HasPrefix(got, "INSERT INTO recall_outbox (") {
		t.Errorf("自动提交路径语句错：%s", got)
	}
}

func TestInsertMapsDuplicateToErrEventExists(t *testing.T) {
	db := &obxSQL{execErr: errors.New("Error 1062 (23000): Duplicate entry 'x' for key 'uniq_event_id'")}
	err := newOutbox(db).Insert(context.Background(), nil, validOutboxRow())
	if !errors.Is(err, ErrEventExists) {
		t.Errorf("event_id 冲突应映射成 ErrEventExists（生产者重试据此判幂等）：%v", err)
	}
	other := &obxSQL{execErr: errors.New("1146 Table go_video_recommend_recall.recall_outbox doesn't exist")}
	if err := newOutbox(other).Insert(context.Background(), nil, validOutboxRow()); err == nil ||
		strings.Contains(err.Error(), "1062") ||
		!strings.Contains(err.Error(), "recall_outbox Insert") {
		t.Errorf("非冲突错误应点名表名原样冒泡：%v", err)
	}
}

// indexOfCol 返回列名在清单里的下标（找不到直接红，避免断言悄悄跳过）。
func indexOfCol(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}
