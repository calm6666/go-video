package logic

// 本文件提供 coin logic 单测用的「内存版 model」与「假事务」。
//
// 为什么可以这样注入（以及为什么这不是伪装，AGENTS.md §9）：
//   - svc.ServiceContext 的四个 model 字段是接口类型（AccountModel/DailyTossModel/
//     TossModel/FlowModel），测试可以直接赋值，不需要连 MySQL；
//   - 假实现复刻的是**语义**而不是锁：唯一键命中即 created=false / 插入报 1062、
//     条件 UPDATE（balance >= ?、tossed + ? <= ?、count + ? <= ?、state = ?）不命中即
//     返回 false、回退用 GREATEST 夹底到 0。logic 面对这些返回值的裁决才是本轮
//     可在无库条件下证明的部分；真库的行锁与并发交错留给容器化集成测试（README §7.7）；
//   - 余额写口（DeductForTossTx/RefundForCancelTx/ApplyGrantTx/Flows.InsertTx）在
//     session 为 nil 时当场 panic：硬币余额只能通过「本服务事务内的写接口」变动，
//     这条数据所有权边界（AGENTS.md §5）必须在单元测试里就是响的；
//   - 每个假实现只嵌入接口并覆写被测路径用到的方法，其余方法由内嵌的 nil 接口提升而来，
//     一旦测试走到未实现的分支会立刻 panic —— 失败不会被写成「通过」。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"go-video/services/coin/internal/config"
	"go-video/services/coin/internal/svc"
	"go-video/services/coin/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// --- 内存态 ---

type dailyKey struct {
	mid  int64
	date int32
}

// failure 是一次注入的读/写故障。left<0 表示长期生效，>0 表示只失败剩余次数。
type failure struct {
	err  error
	left int
}

type fakeDB struct {
	accounts map[int64]*model.Account
	daily    map[dailyKey]*model.DailyToss
	tosses   map[int64]*model.Toss // 按 id 存，(mid,target_aid) 唯一性另判
	flows    map[int64]*model.Flow // 按 id 存，request_id 唯一性另判
	nextToss int64
	nextFlow int64

	// calls 是有序调用日志。它**不参与**快照/回滚：事务回滚后仍要能证明锁序与「有没有白扣币」。
	calls []string
	// balanceWrites 记录每次余额改动的入口名（数据所有权断言用）。同样不参与回滚。
	balanceWrites []string
	failures      map[string]*failure
	txRuns        int
	txCommits     int
	txRollbacks   int

	// concurrentFlow 复刻「本事务快路径读过后，另一个事务提交了同 request_id 流水」：
	// 事务外（session==nil）读不到，事务内读得到；本事务回滚时它才作为已提交数据被采纳。
	concurrentFlow       *model.Flow
	concurrentVisible    bool
	keepConcurrentHidden bool
	// concurrentBalanceAfter 是并发方提交后该账户的**权威**余额（流水 BalanceAfter 同源）。
	// 不回滚它：本事务回滚后，当前真值就是「别人那一次生效了」。
	concurrentBalanceAfter int64

	// concurrentToss 复刻「快检之后另一个事务改了这条投币记录」：只有事务内锁行才看得到。
	concurrentToss *model.Toss
	// hideTossInTx 让事务内锁行读不到记录（快检却有），用于验证「记录消失」的结论分支。
	hideTossInTx bool
}

// seedConcurrentFlow 放一条「别人正在写」的流水，用来触发 errTossReplay /
// errCancelReplay / errGrantReplay 这类事务内发现的并发重放。
func (db *fakeDB) seedConcurrentFlow(f *model.Flow) {
	db.concurrentFlow = f
	db.concurrentVisible = false
}

// seedConcurrentCommit 除了放一条并发流水，还声明它对余额的改动已经生效：
// 用于证明「并发重放只生效一次」时台账与余额仍然自洽。
func (db *fakeDB) seedConcurrentCommit(f *model.Flow, balanceAfter int64) {
	db.seedConcurrentFlow(f)
	db.concurrentBalanceAfter = balanceAfter
}

// seedConcurrentToss 放一条「别人已经改过」的投币行：只在事务内锁行时可见。
func (db *fakeDB) seedConcurrentToss(toss *model.Toss) {
	db.concurrentToss = toss
}

func (db *fakeDB) restore(s *fakeDB) {
	db.accounts = s.accounts
	db.daily = s.daily
	db.tosses = s.tosses
	db.flows = s.flows
	db.nextToss = s.nextToss
	db.nextFlow = s.nextFlow
	if db.concurrentFlow != nil && !db.keepConcurrentHidden {
		c := *db.concurrentFlow
		db.concurrentFlow = nil
		db.seedFlowOnly(&c)
		if db.concurrentBalanceAfter != 0 {
			// 并发方对余额的改动是已提交事实，回滚不能把它一起抹掉。
			if acc, ok := db.accounts[c.Mid]; ok {
				acc.Balance = db.concurrentBalanceAfter
				acc.Version++
			}
			db.concurrentBalanceAfter = 0
		}
	}
}

// seedFlowOnly 把一条流水按「别人已提交」写入表：id 从回滚后的计数器继续分配，
// 因此不会与本事务之前播过种的行撞号。
func (db *fakeDB) seedFlowOnly(f *model.Flow) {
	c := *f
	db.nextFlow++
	c.ID = db.nextFlow
	db.flows[c.ID] = &c
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		accounts: map[int64]*model.Account{},
		daily:    map[dailyKey]*model.DailyToss{},
		tosses:   map[int64]*model.Toss{},
		flows:    map[int64]*model.Flow{},
		failures: map[string]*failure{},
	}
}

// Fail 让名为 op 的 model 方法长期返回 err；FailOnce 只作用一次。
// op 形如 "Accounts.FindOne"、"Flows.FindByRequestID"。
func (db *fakeDB) Fail(op string, err error) { db.failures[op] = &failure{err: err, left: -1} }
func (db *fakeDB) FailOnce(op string, err error) {
	db.failures[op] = &failure{err: err, left: 1}
}

// step 记一次调用并返回该处注入的故障（无故障返回 nil）。
func (db *fakeDB) step(op string) error {
	db.calls = append(db.calls, op)
	f, ok := db.failures[op]
	if !ok {
		return nil
	}
	if f.left > 0 {
		f.left--
		if f.left == 0 {
			delete(db.failures, op)
		}
	}
	return f.err
}

func (db *fakeDB) snapshot() *fakeDB {
	cp := &fakeDB{
		accounts:  map[int64]*model.Account{},
		daily:     map[dailyKey]*model.DailyToss{},
		tosses:    map[int64]*model.Toss{},
		flows:     map[int64]*model.Flow{},
		nextToss:  db.nextToss,
		nextFlow:  db.nextFlow,
		txRuns:    db.txRuns,
		txCommits: db.txCommits,
	}
	for k, v := range db.accounts {
		c := *v
		cp.accounts[k] = &c
	}
	for k, v := range db.daily {
		c := *v
		cp.daily[k] = &c
	}
	for k, v := range db.tosses {
		c := *v
		cp.tosses[k] = &c
	}
	for k, v := range db.flows {
		c := *v
		cp.flows[k] = &c
	}
	return cp
}

// --- 断言辅助 ---

func (db *fakeDB) countCall(op string) int {
	n := 0
	for _, c := range db.calls {
		if c == op {
			n++
		}
	}
	return n
}

// wantCallOrder 断言给定方法名按顺序（可不连续）出现在调用日志里。
// 用来钉住 README §3 的锁序：账户行锁 → request_id 复核 → cn_toss 行锁 → 日额度 → 扣减 → 台账。
func (db *fakeDB) wantCallOrder(t *testing.T, ops ...string) {
	t.Helper()
	i := 0
	for _, line := range db.calls {
		if i < len(ops) && line == ops[i] {
			i++
		}
	}
	if i != len(ops) {
		var rest []string
		if i < len(ops) {
			rest = ops[i:]
		}
		t.Fatalf("调用顺序不符：还期待 %v，实际日志为\n  %s", rest, strings.Join(db.calls, " -> "))
	}
}

func (db *fakeDB) wantNoCall(t *testing.T, ops ...string) {
	t.Helper()
	for _, op := range ops {
		if n := db.countCall(op); n != 0 {
			t.Fatalf("不该发生 %s（实际 %d 次），调用日志为\n  %s", op, n, strings.Join(db.calls, " -> "))
		}
	}
}

// wantBalanceWritesVia 断言本次测试里所有余额改动都只经过指定的写入口。
func (db *fakeDB) wantBalanceWritesVia(t *testing.T, allowed ...string) {
	t.Helper()
	if len(db.balanceWrites) == 0 {
		t.Fatalf("本次没有任何余额变动，断言无意义（调用日志 %v）", db.calls)
	}
	for _, w := range db.balanceWrites {
		ok := false
		for _, a := range allowed {
			if w == a {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("余额经由 %s 被改动，只允许 %v（AGENTS.md §5：硬币余额唯一写入口）", w, allowed)
		}
	}
}

func (db *fakeDB) account(t *testing.T, mid int64) *model.Account {
	t.Helper()
	acc, ok := db.accounts[mid]
	if !ok {
		t.Fatalf("没有 mid=%d 的账户行", mid)
	}
	c := *acc
	return &c
}

// sumDelta 复刻 model.SumDelta：balance 的独立算法，对账不变式的另一半。
func (db *fakeDB) sumDelta(mid int64) int64 {
	var sum int64
	for _, f := range db.flows {
		if f.Mid == mid {
			sum += f.Delta
		}
	}
	return sum
}

// wantLedgerParity 钉住 README §1 的记账不变式：balance == SUM(cn_flow.delta)。
func (db *fakeDB) wantLedgerParity(t *testing.T, mid int64) {
	t.Helper()
	acc := db.account(t, mid)
	if got := db.sumDelta(mid); got != acc.Balance {
		t.Fatalf("对账不变式破裂：balance=%d 而 SUM(flow.delta)=%d；流水 %s",
			acc.Balance, got, db.flowDump(mid))
	}
}

func (db *fakeDB) flowDump(mid int64) string {
	var parts []string
	for _, f := range db.sortedFlows(mid) {
		parts = append(parts, fmt.Sprintf("{id=%d type=%d delta=%d after=%d rid=%s}",
			f.ID, f.FlowType, f.Delta, f.BalanceAfter, f.RequestID))
	}
	return strings.Join(parts, " ")
}

func (db *fakeDB) sortedFlows(mid int64) []*model.Flow {
	var out []*model.Flow
	for _, f := range db.flows {
		if mid == 0 || f.Mid == mid {
			c := *f
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (db *fakeDB) flowByRequestID(t *testing.T, requestID string) *model.Flow {
	t.Helper()
	for _, f := range db.flows {
		if f.RequestID == requestID {
			c := *f
			return &c
		}
	}
	t.Fatalf("没有 request_id=%s 的流水，现有流水：%s", requestID, db.flowDump(0))
	return nil
}

// wantFlowBalanceAfter 断言流水的 balance_after 就是落库后的余额（对账口径的另一半：
// 台账里回显的余额必须与 cn_account 当前值同源于同一笔变更，否则排障时两边对不上）。
func (db *fakeDB) wantFlowBalanceAfter(t *testing.T, requestID string, balanceAfter int64) *model.Flow {
	t.Helper()
	f := db.flowByRequestID(t, requestID)
	if f.BalanceAfter != balanceAfter {
		t.Fatalf("流水 %s 的 balance_after=%d，期待 %d；台账：%s", requestID, f.BalanceAfter, balanceAfter, db.flowDump(f.Mid))
	}
	return f
}

func (db *fakeDB) toss(t *testing.T, mid, aid int64) *model.Toss {
	t.Helper()
	for _, r := range db.tosses {
		if r.Mid == mid && r.TargetAid == aid {
			c := *r
			return &c
		}
	}
	t.Fatalf("没有 (mid=%d, aid=%d) 的投币记录", mid, aid)
	return nil
}

// --- 假事务 ---

// fakeTx 只是「此刻在事务里」的凭证：logic 从不自发 SQL，model 假实现只用它区分
// 事务内/事务外。未实现的方法由内嵌 nil 接口提升，走到即 panic。
type fakeTx struct {
	sqlx.Session
}

type fakeConn struct {
	sqlx.SqlConn
	db *fakeDB
}

func (c fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.db.txRuns++
	snap := c.db.snapshot()
	if err := fn(ctx, fakeTx{}); err != nil {
		c.db.restore(snap)
		c.db.txRollbacks++
		return err
	}
	c.db.txCommits++
	return nil
}

// outsideTxPanic 是「余额写口在事务外被调用」的当场失败：这类调用在真库里就是一次
// 无事务保护的余额改动（可能扣了币没记流水），绝不能被测试默默放过。
func outsideTxPanic(op string) error {
	return fmt.Errorf("coin: %s 必须在事务内调用（余额变动与流水必须同事务，README §3）", op)
}

// --- cn_account ---

type fakeAccounts struct {
	model.AccountModel
	db *fakeDB
}

func (f fakeAccounts) FindOne(_ context.Context, mid int64) (*model.Account, error) {
	if err := f.db.step("Accounts.FindOne"); err != nil {
		return nil, err
	}
	acc, ok := f.db.accounts[mid]
	if !ok {
		return nil, nil // 与真 model 一致：查无此账户是 (nil, nil)，不是错误
	}
	c := *acc
	return &c, nil
}

func (f fakeAccounts) EnsureTx(_ context.Context, _ sqlx.Session, mid, initial int64) (bool, error) {
	if err := f.db.step("Accounts.EnsureTx"); err != nil {
		return false, err
	}
	if mid <= 0 {
		return false, model.ErrInvalidMid
	}
	if initial < 0 {
		initial = 0
	}
	if _, ok := f.db.accounts[mid]; ok {
		return false, nil // ON DUPLICATE KEY UPDATE mid=mid ⇒ affected=0
	}
	now := model.NowUnix()
	f.db.accounts[mid] = &model.Account{Mid: mid, Balance: initial, Ctime: now, Mtime: now}
	return true, nil
}

func (f fakeAccounts) LockForUpdateTx(_ context.Context, session sqlx.Session, mid int64) (*model.Account, error) {
	if err := f.db.step("Accounts.LockForUpdateTx"); err != nil {
		return nil, err
	}
	if session == nil {
		panic(outsideTxPanic("Accounts.LockForUpdateTx"))
	}
	acc, ok := f.db.accounts[mid]
	if !ok {
		return nil, model.ErrAccountNotFound
	}
	c := *acc
	return &c, nil
}

func (f fakeAccounts) DeductForTossTx(_ context.Context, session sqlx.Session, mid, amount, atLeast int64) (bool, error) {
	if err := f.db.step("Accounts.DeductForTossTx"); err != nil {
		return false, err
	}
	if session == nil {
		panic(outsideTxPanic("Accounts.DeductForTossTx"))
	}
	if amount <= 0 {
		return false, model.ErrInvalidTossCount
	}
	acc, ok := f.db.accounts[mid]
	if !ok {
		return false, model.ErrAccountNotFound
	}
	if atLeast < amount {
		atLeast = amount
	}
	if acc.Balance < atLeast {
		return false, nil // WHERE balance >= ? 未命中
	}
	acc.Balance -= amount
	acc.TotalTossed += amount
	acc.Version++
	acc.Mtime = model.NowUnix()
	f.db.balanceWrites = append(f.db.balanceWrites, "DeductForTossTx")
	return true, nil
}

func (f fakeAccounts) RefundForCancelTx(_ context.Context, session sqlx.Session, mid, amount int64) (bool, error) {
	if err := f.db.step("Accounts.RefundForCancelTx"); err != nil {
		return false, err
	}
	if session == nil {
		panic(outsideTxPanic("Accounts.RefundForCancelTx"))
	}
	if amount <= 0 {
		return false, model.ErrInvalidTossCount
	}
	acc, ok := f.db.accounts[mid]
	if !ok {
		return false, model.ErrAccountNotFound
	}
	acc.Balance += amount // total_tossed 不回退（proto 锁定的历史口径）
	acc.Version++
	acc.Mtime = model.NowUnix()
	f.db.balanceWrites = append(f.db.balanceWrites, "RefundForCancelTx")
	return true, nil
}

func (f fakeAccounts) ApplyGrantTx(_ context.Context, session sqlx.Session, mid, delta int64) (bool, error) {
	if err := f.db.step("Accounts.ApplyGrantTx"); err != nil {
		return false, err
	}
	if session == nil {
		panic(outsideTxPanic("Accounts.ApplyGrantTx"))
	}
	if delta == 0 {
		return false, model.ErrGrantDeltaInvalid
	}
	acc, ok := f.db.accounts[mid]
	if !ok {
		return false, model.ErrAccountNotFound
	}
	if acc.Balance+delta < 0 {
		return false, nil // WHERE balance + ? >= 0 未命中：任何路径都不得把余额扣成负数
	}
	acc.Balance += delta
	acc.Version++
	acc.Mtime = model.NowUnix()
	f.db.balanceWrites = append(f.db.balanceWrites, "ApplyGrantTx")
	return true, nil
}

// --- cn_daily_toss ---

type fakeDaily struct {
	model.DailyTossModel
	db *fakeDB
}

func (f fakeDaily) FindOne(_ context.Context, mid int64, date int32) (*model.DailyToss, error) {
	if err := f.db.step("Daily.FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.db.daily[dailyKey{mid, date}]
	if !ok {
		return nil, nil // 缺行即「当日 0 枚」，跨日天然重置
	}
	c := *row
	return &c, nil
}

func (f fakeDaily) EnsureTx(_ context.Context, _ sqlx.Session, mid int64, date int32) error {
	if err := f.db.step("Daily.EnsureTx"); err != nil {
		return err
	}
	key := dailyKey{mid, date}
	if _, ok := f.db.daily[key]; ok {
		return nil
	}
	now := model.NowUnix()
	f.db.daily[key] = &model.DailyToss{ID: int64(len(f.db.daily) + 1), Mid: mid, Date: date, Ctime: now, Mtime: now}
	return nil
}

func (f fakeDaily) AccumulateTx(_ context.Context, session sqlx.Session, mid int64, date, delta, dailyLimit int32) (bool, error) {
	if err := f.db.step("Daily.AccumulateTx"); err != nil {
		return false, err
	}
	if session == nil {
		panic(outsideTxPanic("Daily.AccumulateTx"))
	}
	if delta <= 0 {
		return false, model.ErrInvalidTossCount
	}
	if dailyLimit < 0 {
		dailyLimit = 0
	}
	row, ok := f.db.daily[dailyKey{mid, date}]
	if !ok {
		return false, nil // 条件更新无行可命中
	}
	if row.Tossed+delta > dailyLimit {
		return false, nil
	}
	row.Tossed += delta
	row.Mtime = model.NowUnix()
	return true, nil
}

func (f fakeDaily) RollbackTx(_ context.Context, session sqlx.Session, mid int64, date, delta int32) error {
	if err := f.db.step("Daily.RollbackTx"); err != nil {
		return err
	}
	if session == nil {
		panic(outsideTxPanic("Daily.RollbackTx"))
	}
	if delta <= 0 {
		return model.ErrInvalidTossCount
	}
	row, ok := f.db.daily[dailyKey{mid, date}]
	if !ok {
		return nil // 缺行等价于 0，不报错
	}
	if row.Tossed-delta < 0 {
		row.Tossed = 0 // GREATEST(tossed - ?, 0) 夹底：宁可少给额度也不多给
	} else {
		row.Tossed -= delta
	}
	row.Mtime = model.NowUnix()
	return nil
}

// --- cn_toss ---

type fakeTosses struct {
	model.TossModel
	db *fakeDB
}

func (f fakeTosses) findRow(mid, targetAid int64) *model.Toss {
	for _, r := range f.db.tosses {
		if r.Mid == mid && r.TargetAid == targetAid {
			c := *r
			return &c
		}
	}
	return nil
}

func (f fakeTosses) FindOne(_ context.Context, mid, targetAid int64) (*model.Toss, error) {
	if err := f.db.step("Tosses.FindOne"); err != nil {
		return nil, err
	}
	return f.findRow(mid, targetAid), nil
}

func (f fakeTosses) LockByTargetTx(_ context.Context, session sqlx.Session, mid, targetAid int64) (*model.Toss, error) {
	if err := f.db.step("Tosses.LockByTargetTx"); err != nil {
		return nil, err
	}
	if session == nil {
		panic(outsideTxPanic("Tosses.LockByTargetTx"))
	}
	if f.db.hideTossInTx {
		return nil, nil // SELECT ... FOR UPDATE 无行
	}
	if ct := f.db.concurrentToss; ct != nil && ct.Mid == mid && ct.TargetAid == targetAid {
		c := *ct
		return &c, nil // 并发方已提交的新版本此刻才对本事务可见
	}
	return f.findRow(mid, targetAid), nil
}

func (f fakeTosses) InsertTx(_ context.Context, session sqlx.Session, t *model.Toss) (int64, error) {
	if err := f.db.step("Tosses.InsertTx"); err != nil {
		return 0, err
	}
	if session == nil {
		panic(outsideTxPanic("Tosses.InsertTx"))
	}
	if t.Mid <= 0 {
		return 0, model.ErrInvalidMid
	}
	if t.TargetAid <= 0 {
		return 0, model.ErrInvalidTargetAid
	}
	if t.Count <= 0 {
		return 0, model.ErrInvalidTossCount
	}
	if f.findRow(t.Mid, t.TargetAid) != nil {
		return 0, fmt.Errorf("Error 1062: Duplicate entry for key 'cn_toss.uniq_mid_target'")
	}
	stored := *t
	if stored.State == 0 {
		stored.State = model.TossStateActive
	}
	if stored.Ctime == 0 {
		stored.Ctime = model.NowUnix()
	}
	stored.ID = f.db.nextToss + 1
	f.db.nextToss = stored.ID
	f.db.tosses[stored.ID] = &stored
	t.ID = stored.ID
	c := stored
	return c.ID, nil
}

func (f fakeTosses) byID(id int64) *model.Toss {
	for _, r := range f.db.tosses {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (f fakeTosses) AccumulateTx(_ context.Context, session sqlx.Session, id int64, delta, perTargetLimit int32,
	now int64, date int32, requestID string, platform int32, traceID string) (bool, error) {
	if err := f.db.step("Tosses.AccumulateTx"); err != nil {
		return false, err
	}
	if session == nil {
		panic(outsideTxPanic("Tosses.AccumulateTx"))
	}
	if delta <= 0 {
		return false, model.ErrInvalidTossCount
	}
	row := f.byID(id)
	if row == nil || row.State != model.TossStateActive || row.Count+delta > perTargetLimit {
		return false, nil // WHERE state=ACTIVE AND `count` + ? <= ?
	}
	row.Count += delta
	row.LastTossedAt = now
	row.LastTossDate = date
	row.LastRequestID = requestID
	row.Platform = platform
	row.TraceID = traceID
	row.Mtime = now
	return true, nil
}

func (f fakeTosses) ReactivateTx(_ context.Context, session sqlx.Session, id int64, count int32,
	now int64, date int32, requestID string, platform int32, traceID string) (bool, error) {
	if err := f.db.step("Tosses.ReactivateTx"); err != nil {
		return false, err
	}
	if session == nil {
		panic(outsideTxPanic("Tosses.ReactivateTx"))
	}
	if count <= 0 {
		return false, model.ErrInvalidTossCount
	}
	row := f.byID(id)
	if row == nil || row.State != model.TossStateCancelled {
		return false, nil
	}
	row.State = model.TossStateActive
	row.Count = count // 覆盖而非累加：上次已退回的枚数不能再算成「现在投出去的」
	row.LastTossedAt = now
	row.LastTossDate = date
	row.CancelledAt = 0
	row.LastRequestID = requestID
	row.Platform = platform
	row.TraceID = traceID
	row.Mtime = now
	return true, nil
}

func (f fakeTosses) CancelTx(_ context.Context, session sqlx.Session, id, now int64) (bool, error) {
	if err := f.db.step("Tosses.CancelTx"); err != nil {
		return false, err
	}
	if session == nil {
		panic(outsideTxPanic("Tosses.CancelTx"))
	}
	row := f.byID(id)
	if row == nil || row.State != model.TossStateActive {
		return false, nil // WHERE id = ? AND state = ACTIVE
	}
	row.State = model.TossStateCancelled
	row.CancelledAt = now
	row.Mtime = now
	return true, nil
}

func (f fakeTosses) match(mid, targetAid int64, state int32) []*model.Toss {
	var out []*model.Toss
	for _, r := range f.db.tosses {
		if mid > 0 && r.Mid != mid {
			continue
		}
		if targetAid > 0 && r.TargetAid != targetAid {
			continue
		}
		if state != 0 && r.State != state {
			continue
		}
		c := *r
		out = append(out, &c)
	}
	// ORDER BY last_tossed_at DESC, id DESC
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastTossedAt != out[j].LastTossedAt {
			return out[i].LastTossedAt > out[j].LastTossedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func pageRows[T any](rows []T, offset int64, limit int) []T {
	if offset < 0 {
		offset = 0
	}
	if int64(len(rows)) <= offset {
		return []T{}
	}
	out := rows[offset:]
	if limit >= 0 && int64(len(out)) > int64(limit) {
		out = out[:limit]
	}
	return out
}

func (f fakeTosses) ListByMid(_ context.Context, mid int64, state int32, offset int64, limit int) ([]*model.Toss, error) {
	if err := f.db.step("Tosses.ListByMid"); err != nil {
		return nil, err
	}
	return pageRows(f.match(mid, 0, state), offset, limit), nil
}

func (f fakeTosses) CountByMid(_ context.Context, mid int64, state int32) (int64, error) {
	if err := f.db.step("Tosses.CountByMid"); err != nil {
		return 0, err
	}
	return int64(len(f.match(mid, 0, state))), nil
}

func (f fakeTosses) ListActiveByTarget(_ context.Context, targetAid int64, offset int64, limit int) ([]*model.Toss, error) {
	if err := f.db.step("Tosses.ListActiveByTarget"); err != nil {
		return nil, err
	}
	return pageRows(f.match(0, targetAid, model.TossStateActive), offset, limit), nil
}

func (f fakeTosses) CountActiveByTarget(_ context.Context, targetAid int64) (int64, error) {
	if err := f.db.step("Tosses.CountActiveByTarget"); err != nil {
		return 0, err
	}
	return int64(len(f.match(0, targetAid, model.TossStateActive))), nil
}

func (f fakeTosses) SummarizeTargets(_ context.Context, aids []int64) (map[int64]model.TargetAgg, error) {
	if err := f.db.step("Tosses.SummarizeTargets"); err != nil {
		return nil, err
	}
	out := map[int64]model.TargetAgg{}
	if len(aids) == 0 {
		return out, nil
	}
	wanted := map[int64]struct{}{}
	for _, a := range aids {
		wanted[a] = struct{}{}
	}
	for _, r := range f.db.tosses {
		if r.State != model.TossStateActive {
			continue
		}
		if _, ok := wanted[r.TargetAid]; !ok {
			continue
		}
		agg := out[r.TargetAid]
		agg.Aid = r.TargetAid
		agg.CoinCount += int64(r.Count)
		agg.CoinUserCount++
		out[r.TargetAid] = agg
	}
	return out, nil
}

// --- cn_flow ---

type fakeFlows struct {
	model.FlowModel
	db *fakeDB
}

func (f fakeFlows) byRequestID(requestID string) *model.Flow {
	for _, r := range f.db.flows {
		if r.RequestID == requestID {
			c := *r
			return &c
		}
	}
	return nil
}

func (f fakeFlows) InsertTx(_ context.Context, session sqlx.Session, fl *model.Flow) (int64, error) {
	if err := f.db.step("Flows.InsertTx"); err != nil {
		return 0, err
	}
	if session == nil {
		panic(outsideTxPanic("Flows.InsertTx"))
	}
	if fl.Mid <= 0 {
		return 0, model.ErrInvalidMid
	}
	if fl.RequestID == "" {
		return 0, model.ErrRequestIDRequired
	}
	if fl.Delta == 0 {
		return 0, model.ErrGrantDeltaInvalid
	}
	if dup := f.byRequestID(fl.RequestID); dup != nil {
		return 0, fmt.Errorf("Error 1062: Duplicate entry %q for key 'cn_flow.uniq_request_id'", dup.RequestID)
	}
	stored := *fl
	if stored.Ctime == 0 {
		stored.Ctime = model.NowUnix()
	}
	stored.ID = f.db.nextFlow + 1
	f.db.nextFlow = stored.ID
	f.db.flows[stored.ID] = &stored
	fl.ID = stored.ID
	return stored.ID, nil
}

func (f fakeFlows) FindByRequestID(_ context.Context, session sqlx.Session, requestID string) (*model.Flow, error) {
	if err := f.db.step("Flows.FindByRequestID"); err != nil {
		return nil, err
	}
	if requestID == "" {
		return nil, model.ErrRequestIDRequired
	}
	if cf := f.db.concurrentFlow; cf != nil && cf.RequestID == requestID {
		if session == nil {
			return nil, nil // 事务外还看不见：并发方尚未对本事务可见
		}
		f.db.concurrentVisible = true
		c := *cf
		return &c, nil
	}
	return f.byRequestID(requestID), nil
}

func (f fakeFlows) FindLatestByTarget(_ context.Context, mid, targetAid int64, flowType int32) (*model.Flow, error) {
	if err := f.db.step("Flows.FindLatestByTarget"); err != nil {
		return nil, err
	}
	var latest *model.Flow
	for _, r := range f.db.flows {
		if r.Mid != mid || r.TargetAid != targetAid || r.FlowType != flowType {
			continue
		}
		if latest == nil || r.ID > latest.ID {
			c := *r
			latest = &c
		}
	}
	return latest, nil
}

func (f fakeFlows) List(_ context.Context, f1 model.FlowFilter, offset int64, limit int) ([]*model.Flow, error) {
	if err := f.db.step("Flows.List"); err != nil {
		return nil, err
	}
	if !f1.Bounded() {
		// 与真 model 的 flowWhere 一致：无界扫描在 model 层就被拒绝。
		return nil, model.ErrUnboundedLedgerQuery
	}
	var out []*model.Flow
	for _, r := range f.db.flows {
		if !flowMatch(r, f1) {
			continue
		}
		c := *r
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ctime != out[j].Ctime {
			return out[i].Ctime > out[j].Ctime
		}
		return out[i].ID > out[j].ID
	})
	return pageRows(out, offset, limit), nil
}

func (f fakeFlows) Count(_ context.Context, flt model.FlowFilter) (int64, error) {
	if err := f.db.step("Flows.Count"); err != nil {
		return 0, err
	}
	if !flt.Bounded() {
		return 0, model.ErrUnboundedLedgerQuery
	}
	var n int64
	for _, r := range f.db.flows {
		if flowMatch(r, flt) {
			n++
		}
	}
	return n, nil
}

func (f fakeFlows) SumDelta(_ context.Context, mid int64) (int64, error) {
	if err := f.db.step("Flows.SumDelta"); err != nil {
		return 0, err
	}
	return f.db.sumDelta(mid), nil
}

func flowMatch(r *model.Flow, flt model.FlowFilter) bool {
	if flt.Mid > 0 && r.Mid != flt.Mid {
		return false
	}
	if flt.FlowType > 0 && r.FlowType != flt.FlowType {
		return false
	}
	if flt.BizNo != "" && r.BizNo != flt.BizNo {
		return false
	}
	if flt.FromTs > 0 && r.Ctime < flt.FromTs {
		return false
	}
	if flt.ToTs > 0 && r.Ctime > flt.ToTs {
		return false
	}
	return true
}

// --- 装配 ---

// testCoinConf 与 etc/coin.v1.yaml 的显式取值一致（config_load_test.go 已在管另一侧的漂移）。
// 单个用例要改限额时直接覆写字段，不要改这份基线，否则「默认值行为」就没人证明了。
func testCoinConf() config.CoinConf {
	c := config.CoinConf{
		InitialBalance:               5,
		DailyLimit:                   10,
		PerTargetLimit:               2,
		CancelWindowSeconds:          86400,
		MinBalanceToToss:             1,
		MaxGrantDelta:                1000,
		DefaultPageSize:              20,
		MaxPageSize:                  100,
		MaxBatchAids:                 50,
		TargetSummaryCacheTTLSeconds: 10,
	}
	c.Sanitize()
	return c
}

// newTestSvc 只换掉数据访问，其余（Coin()/PageSize/Offset/限额判定路径）全是被测代码。
// Cache 恒为 nil：本测试不连 Redis，缓存路径单独在 conv_test.go 里按「未启用即回源」证明。
func newTestSvc(t *testing.T) (*svc.ServiceContext, *fakeDB) {
	t.Helper()
	db := newFakeDB()
	return &svc.ServiceContext{
		Config:   config.Config{Coin: testCoinConf()},
		DB:       fakeConn{db: db},
		Accounts: fakeAccounts{db: db},
		Daily:    fakeDaily{db: db},
		Tosses:   fakeTosses{db: db},
		Flows:    fakeFlows{db: db},
	}, db
}

// newTestSvcWith 允许用例改写生效限额（改后重跑 Sanitize，与 svc 装配路径一致）。
func newTestSvcWith(t *testing.T, mutate func(*config.CoinConf)) (*svc.ServiceContext, *fakeDB) {
	t.Helper()
	sc, db := newTestSvc(t)
	mutate(&sc.Config.Coin)
	sc.Config.Coin.Sanitize()
	return sc, db
}

// --- 播种 ---

func seedAccount(db *fakeDB, mid, balance int64) *model.Account {
	now := model.NowUnix()
	acc := &model.Account{Mid: mid, Balance: balance, Ctime: now, Mtime: now}
	db.accounts[mid] = acc
	return acc
}

// seedAccountWithLedger 建一笔「余额与流水自洽」的账户：先给余额再补一条发放流水，
// 让对账不变式在断言前就成立，避免把夹具本身的不一致当成被测代码的结论。
func seedAccountWithLedger(db *fakeDB, mid, balance int64) *model.Account {
	acc := seedAccount(db, mid, balance)
	if balance > 0 {
		seedFlow(db, &model.Flow{
			Mid: mid, FlowType: model.FlowTypeAdminGrant, Delta: balance, BalanceAfter: balance,
			BizNo: "SEED", Operator: "seed", RequestID: fmt.Sprintf("seed:grant:%d", mid),
		})
	}
	return acc
}

func seedDaily(db *fakeDB, mid int64, date, tossed int32) *model.DailyToss {
	key := dailyKey{mid, date}
	row := &model.DailyToss{ID: int64(len(db.daily) + 1), Mid: mid, Date: date, Tossed: tossed,
		Ctime: model.NowUnix(), Mtime: model.NowUnix()}
	db.daily[key] = row
	return row
}

func seedToss(db *fakeDB, toss *model.Toss) *model.Toss {
	stored := *toss
	if stored.State == 0 {
		stored.State = model.TossStateActive
	}
	stored.ID = db.nextToss + 1
	db.nextToss = stored.ID
	db.tosses[stored.ID] = &stored
	c := stored
	return &c
}

func seedFlow(db *fakeDB, fl *model.Flow) *model.Flow {
	stored := *fl
	if stored.Ctime == 0 {
		stored.Ctime = model.NowUnix()
	}
	stored.ID = db.nextFlow + 1
	db.nextFlow = stored.ID
	db.flows[stored.ID] = &stored
	c := stored
	return &c
}

// --- 时钟与错误断言 ---

// fixedClock 注入固定时钟（model.SetClockForTest），用于取消窗口与跨日边界，
// 不需要 time.Sleep（AGENTS.md §9 不许靠挂钟猜）。
func fixedClock(t *testing.T, at time.Time) {
	t.Helper()
	restore := model.SetClockForTest(func() time.Time { return at })
	t.Cleanup(restore)
}

// clockAt 返回固定时刻，用例据此推算 last_tossed_at。
func clockAt(sec int) time.Time {
	return time.Date(2026, 3, 5, 10, 0, 0, 0, time.Local).Add(time.Duration(sec) * time.Second)
}

var errBoom = errors.New("boom: 依赖故障")

// wantErrIs 断言 err 是（或包装了）target，并且错误文本里带上关键上下文。
func wantErrIs(t *testing.T, err, target error) {
	t.Helper()
	if err == nil {
		t.Fatalf("期待 %v，实际 err=nil", target)
	}
	if !errors.Is(err, target) {
		t.Fatalf("期待错误 %v，实际 %v", target, err)
	}
}

// wantErrContains 断言错误文本里含某片段（错误码之外的可诊断信息同样重要）。
func wantErrContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期待错误含 %q，实际 err=nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("错误 %q 不含 %q", err.Error(), substr)
	}
}
