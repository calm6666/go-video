package model

// 本文件覆盖 coin model 里「不需要数据库就能定死」的判定口径。
// 迁移对照见 migration_parity_test.go；这里管的是运行期折叠规则：
//
//  1. 校验必须发生在 SQL 之前。把非法入参交给驱动，最坏结果不是「报个错」，
//     而是 1406/1366 让整笔已判定成功的扣币事务回滚（logic 侧就再也说不清发生过什么）；
//     用例对每个写入函数都断言「返回哨兵错误且一条 SQL 都没发出去」。
//  2. RowsAffected / ErrNoRows 的折叠口径是 logic 全部结论的真值来源：
//     取不到 RowsAffected 就当 0 会把一次成功的扣减误判成「余额不足」；
//     把读失败塌成 (nil, nil) 会把「库不通」冒充成「这人余额 0」——两种都必须在 model 层挡掉。
//  3. INSERT 的实参顺序必须与列清单逐列对应。错一位就是把 balance 写进 total_tossed，
//     这种 bug 编译期发现不了，假实现也发现不了，只能逐列比对实参。
//  4. 行锁语句（FOR UPDATE）只许在事务里跑：自动提交下的 SELECT ... FOR UPDATE 不加锁，
//     「同一用户串行化」的前提就没了。
//
// 约定：fakeSQL 同时充当 sqlx.SqlConn 与 sqlx.Session，未预期的调用一律 panic（宁炸不默）。

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// --- 可编程的 sqlx 替身（不 import 驱动、不连库） ---

type sqlCall struct {
	query string
	args  []any
}

type fakeResult struct {
	affected    int64
	affectedErr error
	lastID      int64
	lastIDErr   error
}

func (r fakeResult) LastInsertId() (int64, error) { return r.lastID, r.lastIDErr }
func (r fakeResult) RowsAffected() (int64, error) { return r.affected, r.affectedErr }

// fakeSQL 记录每条 SQL 与实参。三个读回调按需覆盖，没覆盖的一律 panic，
// 避免出现「用例其实什么都没断言」的静默通过。
type fakeSQL struct {
	calls   []sqlCall
	execRes sql.Result
	execErr error
	rowFn   func(v any, query string, args []any) error
	rowsFn  func(v any, query string, args []any) error
}

func (f *fakeSQL) record(query string, args []any) {
	f.calls = append(f.calls, sqlCall{query: query, args: args})
}

// one 断言「恰好发了一条 SQL」并返回它。
func (f *fakeSQL) one(t *testing.T) sqlCall {
	t.Helper()
	if len(f.calls) != 1 {
		t.Fatalf("期望恰好发出 1 条 SQL，实际 %d：%+v", len(f.calls), f.calls)
	}
	return f.calls[0]
}

func (f *fakeSQL) ExecCtx(ctx context.Context, query string, args ...any) (sql.Result, error) {
	f.record(query, args)
	if f.execErr != nil {
		return nil, f.execErr
	}
	if f.execRes != nil {
		return f.execRes, nil
	}
	return fakeResult{affected: 1, lastID: 9001}, nil
}

func (f *fakeSQL) Exec(query string, args ...any) (sql.Result, error) {
	panic("coin model 必须走 Ctx 版本：" + query)
}

func (f *fakeSQL) Prepare(query string) (sqlx.StmtSession, error) {
	panic("coin model 不使用预编译语句")
}

func (f *fakeSQL) PrepareCtx(ctx context.Context, query string) (sqlx.StmtSession, error) {
	panic("coin model 不使用预编译语句")
}

func (f *fakeSQL) QueryRowCtx(ctx context.Context, v any, query string, args ...any) error {
	f.record(query, args)
	if f.rowFn != nil {
		return f.rowFn(v, query, args)
	}
	panic("未设 rowFn 却发生了单行查询：" + query)
}

func (f *fakeSQL) QueryRow(v any, query string, args ...any) error {
	panic("coin model 必须走 Ctx 版本：" + query)
}

func (f *fakeSQL) QueryRowPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("coin model 不使用 partial 查询（列清单必须与结构体逐列一致）")
}

func (f *fakeSQL) QueryRowPartial(v any, query string, args ...any) error {
	panic("coin model 不使用 partial 查询")
}

func (f *fakeSQL) QueryRowsCtx(ctx context.Context, v any, query string, args ...any) error {
	f.record(query, args)
	if f.rowsFn != nil {
		return f.rowsFn(v, query, args)
	}
	panic("未设 rowsFn 却发生了多行查询：" + query)
}

func (f *fakeSQL) QueryRows(v any, query string, args ...any) error {
	panic("coin model 必须走 Ctx 版本：" + query)
}

func (f *fakeSQL) QueryRowsPartialCtx(ctx context.Context, v any, query string, args ...any) error {
	panic("coin model 不使用 partial 查询")
}

func (f *fakeSQL) QueryRowsPartial(v any, query string, args ...any) error {
	panic("coin model 不使用 partial 查询")
}

func (f *fakeSQL) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	panic("model 层不得自行开事务：事务边界由 logic 决定，否则余额与流水会落在两个事务里")
}

func (f *fakeSQL) Transact(fn func(sqlx.Session) error) error {
	panic("model 层不得自行开事务（且必须走 Ctx 版本）")
}

func (f *fakeSQL) RawDB() (*sql.DB, error) {
	return nil, errors.New("coin model 测试不接触真实连接池")
}

func newModels(conn sqlx.SqlConn) (AccountModel, DailyTossModel, TossModel, FlowModel) {
	return NewAccountModel(conn), NewDailyTossModel(conn), NewTossModel(conn), NewFlowModel(conn)
}

// --- 1. 校验先于 SQL ---

func TestInvalidRowsRejectedBeforeAnySQL(t *testing.T) {
	ctx := context.Background()
	f := &fakeSQL{}
	accounts, daily, tosses, flows := newModels(f)
	s := sqlx.Session(f)

	cases := []struct {
		name  string
		run   func() error
		wants error
	}{
		{"建仓 mid 非法", func() error { _, err := accounts.EnsureTx(ctx, s, 0, 5); return err }, ErrInvalidMid},
		{"投币记录 mid 非法", func() error {
			_, err := tosses.InsertTx(ctx, s, &Toss{TargetAid: 8, Count: 1})
			return err
		}, ErrInvalidMid},
		{"投币记录 aid 非法", func() error { _, err := tosses.InsertTx(ctx, s, &Toss{Mid: 7, Count: 1}); return err }, ErrInvalidTargetAid},
		{"投币枚数非法", func() error { _, err := tosses.InsertTx(ctx, s, &Toss{Mid: 7, TargetAid: 8}); return err }, ErrInvalidTossCount},
		{"流水 mid 非法", func() error { _, err := flows.InsertTx(ctx, s, &Flow{RequestID: "R1", Delta: 1}); return err }, ErrInvalidMid},
		{"流水缺幂等键", func() error { _, err := flows.InsertTx(ctx, s, &Flow{Mid: 7, Delta: -1}); return err }, ErrRequestIDRequired},
		{"流水零额", func() error {
			_, err := flows.InsertTx(ctx, s, &Flow{Mid: 7, Delta: 0, RequestID: "R1"})
			return err
		}, ErrGrantDeltaInvalid},
		{"幂等键为空不做无界查询", func() error { _, err := flows.FindByRequestID(ctx, s, ""); return err }, ErrRequestIDRequired},
		{"扣减枚数非法", func() error { _, err := accounts.DeductForTossTx(ctx, s, 7, 0, 0); return err }, ErrInvalidTossCount},
		{"退回枚数非法", func() error { _, err := accounts.RefundForCancelTx(ctx, s, 7, -3); return err }, ErrInvalidTossCount},
		{"发放 delta 为 0", func() error { _, err := accounts.ApplyGrantTx(ctx, s, 7, 0); return err }, ErrGrantDeltaInvalid},
		{"日额度累加枚数非法", func() error { _, err := daily.AccumulateTx(ctx, s, 7, 20260305, 0, 50); return err }, ErrInvalidTossCount},
		{"日额度回退枚数非法", func() error { return daily.RollbackTx(ctx, s, 7, 20260305, 0) }, ErrInvalidTossCount},
		{"投币累计枚数非法", func() error {
			_, err := tosses.AccumulateTx(ctx, s, 3, 0, 20, 1, 20260305, "R1", 1, "t")
			return err
		}, ErrInvalidTossCount},
		{"重投枚数非法", func() error {
			_, err := tosses.ReactivateTx(ctx, s, 3, -1, 1, 20260305, "R1", 1, "t")
			return err
		}, ErrInvalidTossCount},
	}
	for _, tc := range cases {
		before := len(f.calls)
		err := tc.run()
		if !errors.Is(err, tc.wants) {
			t.Errorf("%s：应得 %v，实际 %v", tc.name, tc.wants, err)
		}
		if len(f.calls) != before {
			t.Errorf("%s：校验必须在发 SQL 之前挡住，实际发出了 %+v", tc.name, f.calls[before:])
		}
	}
}

// --- 2. 行锁只在事务内 ---

func TestLockStatementsRefuseToRunOutsideTransaction(t *testing.T) {
	ctx := context.Background()
	f := &fakeSQL{}
	accounts, _, tosses, _ := newModels(f)

	acc, err := accounts.LockForUpdateTx(ctx, nil, 7)
	if err == nil || acc != nil {
		t.Fatalf("session 为 nil 时禁止取行锁（自动提交下的 FOR UPDATE 不加分离锁）：%v %v", acc, err)
	}
	if !strings.Contains(err.Error(), "requires a transaction session") {
		t.Errorf("错误要能自证原因，实际：%v", err)
	}
	row, err := tosses.LockByTargetTx(ctx, nil, 7, 8)
	if err == nil || row != nil {
		t.Fatalf("LockByTargetTx 同理：%v %v", row, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("挡住之前不该发出任何 SQL：%+v", f.calls)
	}
}

func TestLockStatementsUseForUpdateAndBindKeys(t *testing.T) {
	ctx := context.Background()
	f := &fakeSQL{rowFn: func(v any, query string, args []any) error {
		v.(*Account).Balance = 42
		return nil
	}}
	accounts, _, tosses, _ := newModels(f)
	s := sqlx.Session(f)

	acc, err := accounts.LockForUpdateTx(ctx, s, 7)
	if err != nil || acc == nil || acc.Balance != 42 {
		t.Fatalf("LockForUpdateTx=%+v err=%v", acc, err)
	}
	call := f.one(t)
	if !strings.Contains(call.query, "FROM cn_account") || !strings.Contains(call.query, "FOR UPDATE") {
		t.Errorf("账户读必须锁 cn_account 行：%s", call.query)
	}
	if len(call.args) != 1 || call.args[0] != int64(7) {
		t.Errorf("mid 必须走占位符：%+v", call.args)
	}

	f.calls = nil
	f.rowFn = func(v any, query string, args []any) error {
		v.(*Toss).Count = 3
		return nil
	}
	row, err := tosses.LockByTargetTx(ctx, s, 7, 8)
	if err != nil || row == nil || row.Count != 3 {
		t.Fatalf("LockByTargetTx=%+v err=%v", row, err)
	}
	call = f.one(t)
	if !strings.Contains(call.query, "FROM cn_toss") || !strings.Contains(call.query, "FOR UPDATE") {
		t.Errorf("取消前必须锁投币行：%s", call.query)
	}
	if len(call.args) != 2 || call.args[0] != int64(7) || call.args[1] != int64(8) {
		t.Errorf("(mid, target_aid) 绑定不符：%+v", call.args)
	}
}

// --- 3. 「无行」与「读失败」是两件事 ---

func TestMissingRowAndReadFailureNeverCollapse(t *testing.T) {
	ctx := context.Background()
	dbErr := errors.New("dial tcp 10.0.0.1:3306: connect refused")

	scenario(t, "无行", func(f *fakeSQL) {
		accounts, _, tosses, flows := newModels(f)
		f.rowFn = func(v any, query string, args []any) error { return sql.ErrNoRows }

		// 无行 = 查无此账户/此记录：(nil, nil)，logic 据此回 found=false。
		if acc, err := accounts.FindOne(ctx, 7); acc != nil || err != nil {
			t.Errorf("FindOne 无行应回 (nil,nil)，实际 %+v %v", acc, err)
		}
		if row, err := tosses.FindOne(ctx, 7, 8); row != nil || err != nil {
			t.Errorf("Toss.FindOne 无行应回 (nil,nil)，实际 %+v %v", row, err)
		}
		if fl, err := flows.FindByRequestID(ctx, nil, "R1"); fl != nil || err != nil {
			t.Errorf("幂等键不存在应回 (nil,nil)（这就是「首次受理」），实际 %+v %v", fl, err)
		}
	})

	scenario(t, "读失败", func(f *fakeSQL) {
		accounts, _, tosses, flows := newModels(f)
		f.rowFn = func(v any, query string, args []any) error { return dbErr }

		// 读失败必须原样上抛：塌成 (nil,nil) 等于把「库不通」冒充成「这人没有账户/没投过币」，
		// 客户端会据此提示「余额不足」，运营会以为账户丢了。
		if acc, err := accounts.FindOne(ctx, 7); acc != nil || !errors.Is(err, dbErr) {
			t.Errorf("FindOne 读失败必须上抛，实际 %+v %v", acc, err)
		}
		if row, err := tosses.FindOne(ctx, 7, 8); row != nil || !errors.Is(err, dbErr) {
			t.Errorf("Toss.FindOne 读失败必须上抛，实际 %+v %v", row, err)
		}
		if fl, err := flows.FindByRequestID(ctx, nil, "R1"); fl != nil || !errors.Is(err, dbErr) {
			t.Errorf("幂等判定读失败绝不能当成「没有重放」，实际 %+v %v", fl, err)
		}
		if fl, err := flows.FindLatestByTarget(ctx, 7, 8, FlowTypeCancelToss); fl != nil || !errors.Is(err, dbErr) {
			t.Errorf("FindLatestByTarget 读失败必须上抛，实际 %+v %v", fl, err)
		}
	})

	// 事务内锁不到账户行是「顺序被改坏」而不是「用户不存在」：必须是 ErrAccountNotFound。
	scenario(t, "事务内行缺失", func(f *fakeSQL) {
		accounts, _, _, _ := newModels(f)
		f.rowFn = func(v any, query string, args []any) error { return sql.ErrNoRows }
		if _, err := accounts.LockForUpdateTx(ctx, sqlx.Session(f), 7); !errors.Is(err, ErrAccountNotFound) {
			t.Errorf("事务内账户行缺失要报 ErrAccountNotFound，实际 %v", err)
		}
	})
}

func TestEmptyListIsNotNilButFailureIsNotEmptyList(t *testing.T) {
	ctx := context.Background()
	dbErr := errors.New("broken pipe")

	f := &fakeSQL{rowsFn: func(v any, query string, args []any) error { return sql.ErrNoRows }}
	_, _, tosses, flows := newModels(f)

	rows, err := flows.List(ctx, FlowFilter{Mid: 7}, 0, 20)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Errorf("无流水要回非 nil 空切片（客户端拿到 [] 而不是 null），实际 %+v %v", rows, err)
	}
	tossRows, err := tosses.ListByMid(ctx, 7, 0, 0, 20)
	if err != nil || tossRows == nil || len(tossRows) != 0 {
		t.Errorf("投币列表同理，实际 %+v %v", tossRows, err)
	}
	aggs, err := tosses.SummarizeTargets(ctx, []int64{1, 2})
	if err != nil || aggs == nil || len(aggs) != 0 {
		t.Errorf("无投币时要回空 map 而不是 nil，实际 %+v %v", aggs, err)
	}

	f2 := &fakeSQL{rowsFn: func(v any, query string, args []any) error { return dbErr }}
	_, _, tosses2, flows2 := newModels(f2)
	if rows, err := flows2.List(ctx, FlowFilter{Mid: 7}, 0, 20); rows != nil || !errors.Is(err, dbErr) {
		t.Errorf("台账读失败绝不能变成空列表，实际 %+v %v", rows, err)
	}
	if rows, err := tosses2.ListByMid(ctx, 7, 0, 0, 20); rows != nil || !errors.Is(err, dbErr) {
		t.Errorf("我的投币同理，实际 %+v %v", rows, err)
	}
	if m, err := tosses2.SummarizeTargets(ctx, []int64{1, 2}); m != nil || !errors.Is(err, dbErr) {
		t.Errorf("批量聚合同理（缺键会被补成 0，等于把故障伪装成「没人投过币」），实际 %+v %v", m, err)
	}
	// Count 走单行查询：读失败绝不能报 0 条（页码会算错，前端以为到底了）。
	f3 := &fakeSQL{rowFn: func(v any, query string, args []any) error { return dbErr }}
	_, _, _, flows3 := newModels(f3)
	if n, err := flows3.Count(ctx, FlowFilter{Mid: 7}); n != 0 || !errors.Is(err, dbErr) {
		t.Errorf("Count 读失败必须上抛，实际 %d %v", n, err)
	}
}

// --- 4. RowsAffected 折叠 ---

func TestRowsAffectedFailureIsNotCollapsedToRejected(t *testing.T) {
	ctx := context.Background()
	affectedErr := errors.New("driver: RowsAffected unsupported")

	cases := []struct {
		name    string
		res     fakeResult
		wantOK  bool
		wantErr error
	}{
		{"命中 1 行", fakeResult{affected: 1}, true, nil},
		{"命中 0 行（余额/额度/上限/状态门禁）", fakeResult{affected: 0}, false, nil},
		{"取不到 affected", fakeResult{affectedErr: affectedErr}, false, affectedErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSQL{execRes: tc.res}
			accounts, daily, tosses, _ := newModels(f)
			s := sqlx.Session(f)
			checks := []struct {
				op  string
				got func() (bool, error)
			}{
				{"DeductForTossTx", func() (bool, error) { return accounts.DeductForTossTx(ctx, s, 7, 5, 5) }},
				{"RefundForCancelTx", func() (bool, error) { return accounts.RefundForCancelTx(ctx, s, 7, 5) }},
				{"ApplyGrantTx", func() (bool, error) { return accounts.ApplyGrantTx(ctx, s, 7, 5) }},
				{"daily.AccumulateTx", func() (bool, error) { return daily.AccumulateTx(ctx, s, 7, 20260305, 5, 50) }},
				{"tosses.AccumulateTx", func() (bool, error) {
					return tosses.AccumulateTx(ctx, s, 3, 5, 20, 1, 20260305, "R1", 1, "t")
				}},
				{"tosses.ReactivateTx", func() (bool, error) {
					return tosses.ReactivateTx(ctx, s, 3, 5, 1, 20260305, "R1", 1, "t")
				}},
				{"tosses.CancelTx", func() (bool, error) { return tosses.CancelTx(ctx, s, 3, 1) }},
			}
			for _, c := range checks {
				ok, err := c.got()
				if tc.wantErr != nil {
					// 拿不到 affected 时把结论当「未命中」= 把一次成功的扣减误判成余额不足，
					// 事务会带着错误结论回滚，用户重试还会再撞一次。必须报错。
					if err == nil || ok {
						t.Errorf("%s：RowsAffected 取不到必须上抛，实际 ok=%v err=%v", c.op, ok, err)
					}
					continue
				}
				if err != nil {
					t.Errorf("%s：%v", c.op, err)
					continue
				}
				if ok != tc.wantOK {
					t.Errorf("%s：affected 折叠成条件结论错了，期望 %v 实际 %v", c.op, tc.wantOK, ok)
				}
			}
		})
	}

	// 执行本身失败时同理：不能吞成「条件没命中」。
	f := &fakeSQL{execErr: errors.New("Error 1062: Duplicate entry 'req-1' for key 'uniq_request_id'")}
	accounts, daily, tosses, _ := newModels(f)
	s := sqlx.Session(f)
	if ok, err := accounts.ApplyGrantTx(ctx, s, 7, 5); err == nil || ok {
		t.Errorf("Exec 失败必须上抛（重复键绝不能被当成扣减未命中），实际 %v %v", ok, err)
	}
	if err := daily.RollbackTx(ctx, s, 7, 20260305, 3); err == nil {
		t.Error("日额度回退失败必须上抛，否则取消事务会带着未回退的额度提交")
	}
	if ok, err := tosses.CancelTx(ctx, s, 3, 1); err == nil || ok {
		t.Error("取消写失败必须上抛")
	}
}

// --- 5. INSERT 语句与实参逐列对应 ---

func TestFlowInsertBindsEveryColumnInOrder(t *testing.T) {
	ctx := context.Background()
	f := &fakeSQL{}
	_, _, _, flows := newModels(f)
	row := &Flow{
		Mid: 1001, FlowType: FlowTypeOrderPack, Delta: 10, BalanceAfter: 11,
		TargetAid: 2002, BizNo: "ORDER-1", Operator: "trade-order",
		RequestID: "req-1", Remark: "摘要", TraceID: "trace-1", Ctime: 1772670000,
	}
	if _, err := flows.InsertTx(ctx, sqlx.Session(f), row); err != nil {
		t.Fatal(err)
	}
	call := f.one(t)
	const wantSQL = "INSERT INTO cn_flow (mid, flow_type, delta, balance_after, target_aid, biz_no," +
		" operator, request_id, remark, trace_id, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	if call.query != wantSQL {
		t.Errorf("台账写入语句变了：\n got %s\nwant %s", call.query, wantSQL)
	}
	wantArgs := []any{row.Mid, row.FlowType, row.Delta, row.BalanceAfter, row.TargetAid, row.BizNo,
		row.Operator, row.RequestID, row.Remark, row.TraceID, row.Ctime}
	if !reflect.DeepEqual(wantArgs, call.args) {
		t.Errorf("实参与列清单不对齐（位置敏感）：\n got %+v\nwant %+v", call.args, wantArgs)
	}
	// balance_after 必须入实参：重放时据此回显「当时落库的余额」，不必再猜。
	if call.args[3] != int64(11) {
		t.Errorf("balance_after 实参=%v", call.args[3])
	}
}

func TestTossInsertBindsEveryColumnInOrder(t *testing.T) {
	ctx := context.Background()
	f := &fakeSQL{}
	_, _, tosses, _ := newModels(f)
	row := &Toss{
		Mid: 1001, TargetAid: 2002, Count: 3, State: TossStateActive,
		FirstTossedAt: 1772660000, LastTossedAt: 1772670000,
		LastRequestID: "req-2", Platform: 2, TraceID: "trace-2",
		LastTossDate: 20260305, Ctime: 1772660000, Mtime: 1772670000,
	}
	if _, err := tosses.InsertTx(ctx, sqlx.Session(f), row); err != nil {
		t.Fatal(err)
	}
	call := f.one(t)
	const wantSQL = "INSERT INTO cn_toss (mid, target_aid, `count`, state, first_tossed_at, last_tossed_at," +
		" cancelled_at, last_request_id, platform, trace_id, last_toss_date, ctime, mtime)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	if call.query != wantSQL {
		t.Errorf("投币记录写入语句变了：\n got %s\nwant %s", call.query, wantSQL)
	}
	wantArgs := []any{row.Mid, row.TargetAid, row.Count, row.State, row.FirstTossedAt, row.LastTossedAt,
		row.CancelledAt, row.LastRequestID, row.Platform, row.TraceID, row.LastTossDate, row.Ctime, row.Mtime}
	if !reflect.DeepEqual(wantArgs, call.args) {
		t.Errorf("实参与列清单不对齐：\n got %+v\nwant %+v", call.args, wantArgs)
	}
	if call.args[6] != int64(0) {
		t.Errorf("新建记录的 cancelled_at 必须是 0（非零会被当成已取消）：%v", call.args[6])
	}
}

func TestDailyBucketInsertIsIdempotentUpsert(t *testing.T) {
	ctx := context.Background()
	restore := SetClockForTest(fixedClock(time.Date(2026, 3, 5, 10, 0, 0, 0, time.Local)))
	defer restore()
	f := &fakeSQL{}
	_, daily, _, _ := newModels(f)
	if err := daily.EnsureTx(ctx, sqlx.Session(f), 1001, 20260305); err != nil {
		t.Fatal(err)
	}
	call := f.one(t)
	const wantSQL = "INSERT INTO cn_daily_toss (mid, date, tossed, ctime, mtime) VALUES (?, ?, 0, ?, ?)" +
		" ON DUPLICATE KEY UPDATE mtime = mtime"
	if call.query != wantSQL {
		t.Errorf("日桶建仓语句变了：\n got %s\nwant %s", call.query, wantSQL)
	}
	if !reflect.DeepEqual([]any{int64(1001), int32(20260305), NowUnix(), NowUnix()}, call.args) {
		t.Errorf("实参=%+v", call.args)
	}
	// 建仓计数必须从 0 起（写死在 SQL 里），否则「已有行」的重复 Ensure 会凭空加额。
	if !strings.Contains(call.query, "VALUES (?, ?, 0, ?, ?)") {
		t.Error("tossed 只能是字面量 0")
	}
}

// --- 6. 建仓与派生时间戳 ---

func TestLazyAccountUsesUniqueKeyUpsert(t *testing.T) {
	ctx := context.Background()
	restore := SetClockForTest(fixedClock(time.Date(2026, 3, 5, 10, 0, 0, 0, time.Local)))
	defer restore()

	for _, tc := range []struct {
		name       string
		affected   int64
		initial    int64
		wantCreate bool
		wantBound  int64
	}{
		{"真正新建", 1, 5, true, 5},
		{"已存在（affected=0）", 0, 5, false, 5},
		{"配置配错也不能写负余额", 1, -7, true, 0},
	} {
		f := &fakeSQL{execRes: fakeResult{affected: tc.affected}}
		accounts, _, _, _ := newModels(f)
		created, err := accounts.EnsureTx(ctx, sqlx.Session(f), 1001, tc.initial)
		if err != nil {
			t.Fatalf("%s：%v", tc.name, err)
		}
		if created != tc.wantCreate {
			t.Errorf("%s：created=%v 期望 %v", tc.name, created, tc.wantCreate)
		}
		call := f.one(t)
		// 靠主键冲突 + ON DUPLICATE KEY UPDATE mid=mid 区分新建/已存在，不依赖驱动专有错误码。
		if !strings.Contains(call.query, "ON DUPLICATE KEY UPDATE mid = mid") {
			t.Errorf("%s：建仓必须靠主键冲突吞掉重复：%s", tc.name, call.query)
		}
		if !strings.Contains(call.query, "VALUES (?, ?, 0, 0, ?, ?)") {
			t.Errorf("%s：total_tossed/version 必须从 0 起（历史口径不能凭空有数）：%s", tc.name, call.query)
		}
		if call.args[0] != int64(1001) {
			t.Errorf("%s：mid 实参=%v", tc.name, call.args[0])
		}
		if call.args[1] != tc.wantBound {
			t.Errorf("%s：初始余额实参=%v 期望 %v", tc.name, call.args[1], tc.wantBound)
		}
		// 同一次写入的 ctime/mtime 必须同源（同一秒），否则「建仓时间」会自相矛盾。
		if call.args[2] != call.args[3] {
			t.Errorf("%s：ctime/mtime 不同源：%v vs %v", tc.name, call.args[2], call.args[3])
		}
	}
}

func TestTossInsertFillsDerivedTimestamps(t *testing.T) {
	ctx := context.Background()
	restore := SetClockForTest(fixedClock(time.Date(2026, 3, 5, 10, 0, 0, 0, time.Local)))
	defer restore()

	f := &fakeSQL{execRes: fakeResult{affected: 1, lastID: 555}}
	_, _, tosses, _ := newModels(f)
	row := &Toss{Mid: 1, TargetAid: 2, Count: 1} // 只给业务字段，其余靠默认口径
	id, err := tosses.InsertTx(ctx, sqlx.Session(f), row)
	if err != nil {
		t.Fatal(err)
	}
	if id != 555 || row.ID != 555 {
		t.Errorf("要回 LastInsertId 供 rpc 回显 toss_id，实际 %d/%d", id, row.ID)
	}
	if row.State != TossStateActive {
		t.Errorf("新记录必须落 ACTIVE，实际 %d", row.State)
	}
	want := NowUnix()
	if row.Ctime != want || row.Mtime != want || row.FirstTossedAt != want || row.LastTossedAt != want {
		t.Errorf("同一次写入的时间戳必须同源 %d：%+v", want, row)
	}

	// 反向：调用方显式给过的时间不得被覆盖（取消后重投要保留 first_tossed_at）。
	f2 := &fakeSQL{}
	_, _, tosses2, _ := newModels(f2)
	earlier := want - 100
	row2 := &Toss{Mid: 1, TargetAid: 2, Count: 1, Ctime: earlier, FirstTossedAt: earlier}
	if _, err := tosses2.InsertTx(ctx, sqlx.Session(f2), row2); err != nil {
		t.Fatal(err)
	}
	if row2.Ctime != earlier || row2.FirstTossedAt != earlier {
		t.Errorf("已填的时间戳不得被覆盖：%+v", row2)
	}
	if row2.LastTossedAt != earlier {
		t.Errorf("last_tossed_at 缺省要跟随 first_tossed_at（取消窗口的起算点），实际 %d", row2.LastTossedAt)
	}
}

func TestFlowInsertKeepsGivenCtime(t *testing.T) {
	ctx := context.Background()
	restore := SetClockForTest(fixedClock(time.Date(2026, 3, 5, 10, 0, 0, 0, time.Local)))
	defer restore()

	f := &fakeSQL{}
	_, _, _, flows := newModels(f)
	row := &Flow{Mid: 1, FlowType: FlowTypeToss, Delta: -2, BalanceAfter: 3, RequestID: "R", Ctime: 111}
	if _, err := flows.InsertTx(ctx, sqlx.Session(f), row); err != nil {
		t.Fatal(err)
	}
	if row.Ctime != 111 || f.one(t).args[10] != int64(111) {
		t.Errorf("台账时间由调用方给定（与余额变更同刻），不得改写：%d", row.Ctime)
	}

	// ctime 缺失才补当前时间：append-only 台账不允许出现 ctime=0 的行，
	// 那种行会被时间窗查询永久漏掉，对账时「流水比余额少一条」。
	f2 := &fakeSQL{}
	_, _, _, flows2 := newModels(f2)
	row2 := &Flow{Mid: 1, FlowType: FlowTypeToss, Delta: -2, RequestID: "R2"}
	if _, err := flows2.InsertTx(ctx, sqlx.Session(f2), row2); err != nil {
		t.Fatal(err)
	}
	if row2.Ctime != NowUnix() {
		t.Errorf("ctime 缺省应补当前时间，实际 %d", row2.Ctime)
	}
}

// --- 7. 条件更新的实参绑定 ---

func TestConditionalGuardsBindTheRightThreshold(t *testing.T) {
	ctx := context.Background()
	restore := SetClockForTest(fixedClock(time.Date(2026, 3, 5, 10, 0, 0, 0, time.Local)))
	defer restore()

	f := &fakeSQL{}
	accounts, daily, tosses, _ := newModels(f)
	s := sqlx.Session(f)

	// atLeast < amount 时必须抬到 amount：否则「最低保留额」形同虚设，余额会被扣穿。
	if _, err := accounts.DeductForTossTx(ctx, s, 7, 5, 2); err != nil {
		t.Fatal(err)
	}
	call := f.one(t)
	if call.args[len(call.args)-1] != int64(5) {
		t.Errorf("扣减条件应绑抬升后的 atLeast=5，实际 %+v", call.args)
	}
	if call.args[0] != int64(5) || call.args[1] != int64(5) {
		t.Errorf("扣减与历史累计必须是同一枚数：%+v", call.args)
	}
	if !strings.Contains(call.query, "total_tossed = total_tossed + ?") {
		t.Errorf("扣减要同事务累计历史口径：%s", call.query)
	}

	f.calls = nil
	if _, err := accounts.DeductForTossTx(ctx, s, 7, 5, 9); err != nil {
		t.Fatal(err)
	}
	if call = f.one(t); call.args[len(call.args)-1] != int64(9) {
		t.Errorf("显式最低保留额不得被下调：%+v", call.args)
	}

	// 日额度上限来自配置而不是枚数。
	f.calls = nil
	if _, err := daily.AccumulateTx(ctx, s, 7, 20260305, 3, 50); err != nil {
		t.Fatal(err)
	}
	call = f.one(t)
	if call.args[len(call.args)-1] != int32(50) {
		t.Errorf("日限额实参末位=%v(%T)，期望 int32(50)", call.args[len(call.args)-1], call.args[len(call.args)-1])
	}
	if call.args[0] != int32(3) || call.args[4] != int32(3) {
		t.Errorf("累加枚数与条件里的枚数必须是同一个值：%+v", call.args)
	}

	// 取消退回不动 total_tossed（历史口径）；扣回必须带非负门禁。
	f.calls = nil
	if _, err := accounts.RefundForCancelTx(ctx, s, 7, 3); err != nil {
		t.Fatal(err)
	}
	if call = f.one(t); strings.Contains(call.query, "total_tossed") {
		t.Errorf("退回不得回退历史累计：%s", call.query)
	}
	f.calls = nil
	if _, err := accounts.ApplyGrantTx(ctx, s, 7, -3); err != nil {
		t.Fatal(err)
	}
	call = f.one(t)
	if !strings.Contains(call.query, "balance + ? >= 0") {
		t.Errorf("扣回必须带非负门禁：%s", call.query)
	}
	if call.args[0] != int64(-3) || call.args[2] != int64(7) || call.args[3] != int64(-3) {
		t.Errorf("delta/mid 绑定不符（条件里漏了 delta 就等于没有门禁）：%+v", call.args)
	}

	// 取消只作用于 ACTIVE 行：状态条件必须是 ACTIVE。
	f.calls = nil
	if _, err := tosses.CancelTx(ctx, s, 42, 1772670000); err != nil {
		t.Fatal(err)
	}
	call = f.one(t)
	if call.args[len(call.args)-1] != TossStateActive {
		t.Errorf("取消的状态条件=%v，期望 ACTIVE", call.args[len(call.args)-1])
	}
	// 重投的覆盖式更新只认 CANCELLED 行。
	f.calls = nil
	if _, err := tosses.ReactivateTx(ctx, s, 42, 3, 1772670000, 20260305, "R", 1, "t"); err != nil {
		t.Fatal(err)
	}
	call = f.one(t)
	if call.args[len(call.args)-1] != TossStateCancelled {
		t.Errorf("重投的状态条件=%v，期望 CANCELLED", call.args[len(call.args)-1])
	}
}

// --- 8. 台账边界与批量聚合 ---

func TestUnboundedLedgerQueryRejectedBeforeSQL(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		filter  FlowFilter
		bounded bool
	}{
		{"全空条件（跨用户全表扫描）", FlowFilter{}, false},
		{"只给类型", FlowFilter{FlowType: FlowTypeToss}, false},
		{"只给结束时间", FlowFilter{ToTs: 100}, false},
		{"时间窗倒置", FlowFilter{FromTs: 200, ToTs: 100}, false},
		{"mid 为负", FlowFilter{Mid: -1}, false},
		{"给到 mid", FlowFilter{Mid: 7}, true},
		{"给到订单号", FlowFilter{BizNo: "ORDER-1"}, true},
		{"合法时间窗", FlowFilter{FromTs: 100, ToTs: 200}, true},
		{"时间窗首尾同刻", FlowFilter{FromTs: 100, ToTs: 100}, true},
	}
	for _, tc := range cases {
		if got := tc.filter.Bounded(); got != tc.bounded {
			t.Errorf("%s：Bounded=%v 期望 %v", tc.name, got, tc.bounded)
		}
	}

	f := &fakeSQL{}
	_, _, _, flows := newModels(f)
	for _, tc := range cases {
		if tc.bounded {
			continue
		}
		rows, err := flows.List(ctx, tc.filter, 0, 20)
		if !errors.Is(err, ErrUnboundedLedgerQuery) || rows != nil {
			t.Errorf("%s：List 必须挡住，实际 %+v %v", tc.name, rows, err)
		}
		if n, err := flows.Count(ctx, tc.filter); n != 0 || !errors.Is(err, ErrUnboundedLedgerQuery) {
			t.Errorf("%s：Count 必须与 List 同口径（否则有总数没列表，页码算错）", tc.name)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("挡住之前不该发 SQL：%+v", f.calls)
	}

	// 正向：条件按 Bounded 出现，且值一律走占位符。
	f2 := &fakeSQL{rowsFn: func(v any, query string, args []any) error { return nil }}
	_, _, _, flows2 := newModels(f2)
	if _, err := flows2.List(ctx, FlowFilter{Mid: 7, FlowType: FlowTypeToss, BizNo: "B", FromTs: 1, ToTs: 2}, 20, 10); err != nil {
		t.Fatal(err)
	}
	call := f2.one(t)
	for _, want := range []string{"mid = ?", "flow_type = ?", "biz_no = ?", "ctime >= ?", "ctime <= ?",
		"ORDER BY ctime DESC, id DESC LIMIT ? OFFSET ?"} {
		if !strings.Contains(call.query, want) {
			t.Errorf("台账查询缺少 %q：%s", want, call.query)
		}
	}
	if !reflect.DeepEqual([]any{int64(7), FlowTypeToss, "B", int64(1), int64(2), 10, int64(20)}, call.args) {
		t.Errorf("台账查询实参=%+v", call.args)
	}
	// 恒真谓词会让「条件为空」变成全表扫描的伪装。
	if strings.Contains(call.query, "1 = 1") {
		t.Error("不允许拼恒真谓词")
	}
}

func TestSummarizeTargetsBindsAidsAsPlaceholders(t *testing.T) {
	ctx := context.Background()
	f := &fakeSQL{rowsFn: func(v any, query string, args []any) error {
		*v.(*[]TargetAgg) = []TargetAgg{{Aid: 11, CoinCount: 4, CoinUserCount: 2}}
		return nil
	}}
	_, _, tosses, _ := newModels(f)
	got, err := tosses.SummarizeTargets(ctx, []int64{11, 12})
	if err != nil {
		t.Fatal(err)
	}
	call := f.one(t)
	if n := strings.Count(call.query, "?"); n != len(call.args) {
		t.Errorf("占位符 %d 个 / 实参 %d 个：%s", n, len(call.args), call.query)
	}
	if !strings.Contains(call.query, "state = ?") || call.args[0] != TossStateActive {
		t.Errorf("聚合只许统计 ACTIVE 行（否则取消掉的币仍算投币数）：%s / %+v", call.query, call.args)
	}
	if agg := got[11]; agg.CoinCount != 4 || agg.CoinUserCount != 2 {
		t.Errorf("结果未按 aid 归位：%+v", got)
	}
	if _, ok := got[12]; ok {
		t.Error("没有投币的 aid 不得凭空出现在结果里（缺键由调用方补零行，两种口径要能区分）")
	}

	// 空集合必须在发 SQL 前返回：`IN ()` 是语法错误。
	f2 := &fakeSQL{}
	_, _, tosses2, _ := newModels(f2)
	m, err := tosses2.SummarizeTargets(ctx, nil)
	if err != nil || m == nil || len(m) != 0 {
		t.Fatalf("空 aids 应回空 map，实际 %+v %v", m, err)
	}
	if len(f2.calls) != 0 {
		t.Errorf("空 aids 不该发 SQL：%+v", f2.calls)
	}
}

func TestPlaceholdersShape(t *testing.T) {
	if got := placeholders(3); got != "?, ?, ?" {
		t.Errorf("placeholders(3)=%q", got)
	}
	if got := placeholders(1); got != "?" {
		t.Errorf("placeholders(1)=%q", got)
	}
	// n<=0 兜底成单个占位符而不是空串：空串会拼出 `IN ()` 这种语法错误，报错点离原因更远。
	for _, n := range []int{0, -1} {
		if got := placeholders(n); got != "?" {
			t.Errorf("placeholders(%d)=%q，必须是单个占位符兜底", n, got)
		}
	}
}

// --- 9. 日桶与幂等键 ---

func TestDayNoTracksLocalMidnight(t *testing.T) {
	loc := time.Local
	cases := []struct {
		name string
		at   time.Time
		want int32
	}{
		{"当天最后一秒", time.Date(2026, 3, 5, 23, 59, 59, 999999999, loc), 20260305},
		{"过零点一秒", time.Date(2026, 3, 6, 0, 0, 1, 0, loc), 20260306},
		{"跨年", time.Date(2027, 1, 1, 0, 0, 0, 0, loc), 20270101},
		{"月日补零", time.Date(2026, 1, 9, 12, 0, 0, 0, loc), 20260109},
	}
	for _, tc := range cases {
		if got := DayNo(tc.at); got != tc.want {
			t.Errorf("%s：DayNo=%d 期望 %d", tc.name, got, tc.want)
		}
	}
	// 注入时钟后 TodayDayNo 必须跟着走：取消窗口与日额度边界的用例都靠它，不能读真实时间。
	restore := SetClockForTest(fixedClock(time.Date(2026, 2, 28, 23, 30, 0, 0, loc)))
	if got := TodayDayNo(); got != 20260228 {
		t.Errorf("TodayDayNo=%d", got)
	}
	if got, want := NowUnix(), time.Date(2026, 2, 28, 23, 30, 0, 0, loc).Unix(); got != want {
		t.Errorf("NowUnix 未走注入时钟：%d 期望 %d", got, want)
	}
	restore()
	// restore 之后必须回到真实时钟（否则别的用例会继承固定时间）。
	if TodayDayNo() == 20260228 && time.Now().Year() != 2026 {
		t.Error("SetClockForTest 的 restore 没生效")
	}
	// DayNo 取的是给定时刻自身的年月日，不做隐式时区换算：
	// 调用方（logic）一律传本地时区时刻，与 DSN 的 loc=Local 同源。
	if got := DayNo(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); got != 20260301 {
		t.Errorf("DayNo 不该换算时区，实际 %d", got)
	}
}

func TestInitialGrantRequestIDIsDeterministicAndFitsColumn(t *testing.T) {
	seen := map[string]int64{}
	for _, mid := range []int64{1, 2, 1 << 40, int64(1<<62) + 12345} {
		id := InitialGrantRequestID(mid)
		if again := InitialGrantRequestID(mid); again != id {
			t.Fatalf("同一 mid 的幂等键必须稳定：%q vs %q", id, again)
		}
		if prev, ok := seen[id]; ok {
			t.Fatalf("mid=%d 与 mid=%d 撞键，建仓发放会被误判成重放", prev, mid)
		}
		seen[id] = mid
		if !strings.HasPrefix(id, InitialGrantRequestIDPrefix) {
			t.Errorf("前缀要能一眼认出是建仓发放：%q", id)
		}
		if len([]rune(id)) > MaxIDChars {
			t.Errorf("%q 超出 request_id 列宽 %d", id, MaxIDChars)
		}
	}
	// 建仓流水的 biz_no/operator 同样要放进 VARCHAR(64)：超宽会在建仓那一笔就报 1406。
	for name, v := range map[string]string{"InitialGrantBizNo": InitialGrantBizNo, "InitialGrantOperator": InitialGrantOperator} {
		if len([]rune(v)) > MaxIDChars {
			t.Errorf("%s=%q 超出列宽 %d", name, v, MaxIDChars)
		}
	}
}

// --- 测试辅助 ---

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// scenario 给每个场景一份全新的替身，避免用例之间互相看到对方的 SQL。
func scenario(t *testing.T, name string, fn func(f *fakeSQL)) {
	t.Helper()
	t.Run(name, func(t *testing.T) { fn(&fakeSQL{}) })
}
