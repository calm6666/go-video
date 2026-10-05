package logic

// 本文件提供 trade-order logic 单测用的「内存版 model」「假事务」与「假下游 RPC 客户端」。
//
// 为什么可以这样注入（以及为什么这不是伪装）：
//   - svc.ServiceContext 的 Orders / OrderEvents 是接口类型，Conn 只被用来开事务，
//     Membership / Payment / Coin 是 goctl 生成的 client 接口，测试可以直接赋值；
//   - 真正的并发正确性由 MySQL 的 uniq_order_no / uniq_request_id 与
//     「UPDATE ... WHERE state=? AND version=?」的 RowsAffected 保证（DSN 禁 clientFoundRows）。
//     单测不复刻行锁，复刻的是**语义**：唯一键命中即 ErrDuplicateRequest、
//     CAS 条件不命中即 (false, nil)、非法迁移即报错、事务回调报错即整体回滚；
//   - 每个假实现只嵌入接口并覆写被测路径用到的方法，其余方法由内嵌的 nil 接口提升，
//     一旦测试走到未实现的分支会立刻 panic —— 失败是响的，不会被写成「通过」。
//
// OrderUpdate 的可写列集合在 model 里是私有白名单，本文件用「只读反射」取出
// SET 片段并按 model 的 SQL 语义施加到内存行上；遇到未知片段直接 panic，
// 于是「给 OrderUpdate 加了新列却没同步假实现」会以测试失败暴露，而不是静默丢写入。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"google.golang.org/grpc"

	coinrpc "go-video/services/coin/rpc"
	memberrpc "go-video/services/membership/rpc"
	paymentrpc "go-video/services/payment/rpc"
	"go-video/services/trade-order/internal/config"
	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
)

// fakeNow 与 model.NowUnix 同源（真实时钟）。用例一律用「相对现在的偏移」构造时间。
func fakeNow() int64 { return time.Now().Unix() }

// ---------------------------------------------------------------- 内存数据库 ----

type fakeDB struct {
	mu sync.Mutex

	orders      map[string]*model.Order
	orderSeq    map[string]int64 // 自增 id 分配（ListByFilter 的 ORDER BY id DESC 依赖）
	nextOrderID int64
	events      []*model.OrderEvent
	eventSeq    int64
	transitions []recordedTransition // 本用例发生过的全部 CAS 迁移（含失败的），用于状态机审计

	txRuns    int
	txRolls   int
	insertErr error // 注入：InsertTx 直接失败（模拟台账与主表同事务回滚）
	failOn    *failOn
	// findRequestErr 注入：幂等回查本身失败（不能当「没有历史单」处理，否则会重复建单）。
	findRequestErr error
	// hideRequestIDLookups > 0 时让前 N 次 FindByRequestID 查不到（模拟「读时还没提交、
	// 写时才撞唯一键」的并发建单），随后的读才回真值。
	hideRequestIDLookups int
	// findOrderErr / listErr / stuckErr / listEventsErr 注入读路径故障：
	// 读接口必须把错误回出去，不能伪装成「没找到 / 空列表」。
	findOrderErr  error
	listErr       error
	stuckErr      error
	listEventsErr error
	// lastFilter / lastStuck* / lastEvents* 记录读接口的实参，让用例能断言
	// 「分页换算、时间窗、归属条件到底传没传下去」，而不是只看返回值。
	lastFilter      *model.OrderFilter
	lastStuckStates []int32
	lastStuckBefore int64
	lastStuckLimit  int64
	lastEventsOff   int64
	lastEventsLimit int64
	// calls 按顺序记录本用例真正触达 model 的方法名。读侧用例用它钉住
	// 「入参守卫发生在触库之前」：守卫失效时这里会凭空多出一条 ListByFilter，
	// 只看返回值是看不出来的（返回值可能恰好也对）。
	calls []string
	// raceHook 注入：在 CAS 比较之前改动内存行，模拟「另一个 writer 刚推了一步」。
	// 用例按 (from,to) 自己过滤要拦哪条边。
	// 真实实现里这就是 UPDATE ... WHERE order_no=? AND state=? AND version=? 的
	// RowsAffected==0（DSN 禁 clientFoundRows，所以匹配行数才是真相），
	// 假实现必须能表达「0 行 → 并发失败」，否则 CAS 相关分支全都测不到。
	raceHook func(o *model.Order, from, to int32)
	// externalWrites 是「别人已提交的外部写」流水：raceHook 的改动属于另一笔事务，
	// 不能被本事务的回滚带走，所以 restore 之后要重放一遍。
	// 登记的闭包必须是幂等赋值（不要写 version++ 这类累加）。
	externalWrites []func()
}

type recordedTransition struct {
	OrderNo     string
	From, To    int32
	Version     int64
	Matched     bool
	Err         error
	FulfillCols map[string]any
}

type failOn struct {
	statePair string // "PAID->FULFILLING"
	attempts  int    // 第 N 次命中时让事务回调失败
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		orders:   map[string]*model.Order{},
		orderSeq: map[string]int64{},
	}
}

func pairName(from, to int32) string {
	return model.StateName(from) + "->" + model.StateName(to)
}

func (db *fakeDB) shouldFailAt(pair string) bool {
	if db.failOn == nil || db.failOn.statePair != pair {
		return false
	}
	return db.txRuns == db.failOn.attempts
}

func (db *fakeDB) snapshot() *fakeDB {
	cp := &fakeDB{
		orders:      make(map[string]*model.Order, len(db.orders)),
		orderSeq:    make(map[string]int64, len(db.orderSeq)),
		nextOrderID: db.nextOrderID,
		txRuns:      db.txRuns,
		txRolls:     db.txRolls,
		eventSeq:    db.eventSeq,
	}
	for k, v := range db.orders {
		c := *v
		cp.orders[k] = &c
	}
	for k, v := range db.orderSeq {
		cp.orderSeq[k] = v
	}
	cp.events = make([]*model.OrderEvent, len(db.events))
	for i, e := range db.events {
		c := *e
		cp.events[i] = &c
	}
	cp.transitions = append([]recordedTransition(nil), db.transitions...)
	return cp
}

func (db *fakeDB) restore(s *fakeDB) {
	db.orders = s.orders
	db.orderSeq = s.orderSeq
	db.nextOrderID = s.nextOrderID
	db.events = s.events
	db.eventSeq = s.eventSeq
	// transitions 是「尝试过哪些迁移」的审计流水，回滚也保留：
	// assertOnlyLegalTransitions 要断的正是这些被拒/被回滚的尝试。
	db.txRolls++
	// 外部已提交的并发写不属于本事务，回滚后必须重放（见 externalWrites 注释）。
	for _, w := range db.externalWrites {
		w()
	}
}

// recordExternalWrite 把 raceHook 对本行的改动登记为外部已提交写。
func (db *fakeDB) recordExternalWrite(orderNo string, from, to int32, fn func(*model.Order, int32, int32)) {
	db.externalWrites = append(db.externalWrites, func() {
		if cur, ok := db.orders[orderNo]; ok {
			fn(cur, from, to)
		}
	})
}

// --- 装配 ---

type fakeConn struct {
	sqlx.SqlConn
	db *fakeDB
}

func (c fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.db.mu.Lock()
	c.db.txRuns++
	c.db.mu.Unlock()
	snap := c.db.snapshot()
	// 事务里的 model 调用在真实实现里带 *sql.Tx；假实现忽略 session 句柄本身，
	// 因为回滚语义由 snapshot/restore 提供（logic 关心的只是「一起成功或一起不留下」）。
	if err := fn(ctx, nil); err != nil {
		c.db.restore(snap)
		return err
	}
	return nil
}

func (c fakeConn) QueryRowCtx(ctx context.Context, v any, query string, args ...any) error {
	return errors.New("trade-order fake conn: logic 不应直接执行 SQL（SQL 属于 model）")
}

// newTestSvc 构造一份「只换掉数据访问与下游客户端、其余用真实实现」的 ServiceContext。
// Transact 走真实现（它检查 Conn 非空后委派给 fakeConn），领域参数用 etc 的默认口径。
func newTestSvc(t *testing.T) (*svc.ServiceContext, *fakeDB) {
	t.Helper()
	db := newFakeDB()
	ctx := &svc.ServiceContext{
		Config: config.Config{TradeOrder: config.TradeOrderConf{
			MaxQuantityPerOrder:           10,
			OrderExpireSeconds:            1800,
			StuckScanMaxLimit:             200,
			MaxListWindowSeconds:          7776000,
			MaxPageSize:                   100,
			FulfillMaxAttempts:            5,
			MinSecondsBetweenFulfillRetry: 30,
			DefaultCurrency:               "CNY",
		}},
		Orders:      &fakeOrderModel{db: db},
		OrderEvents: &fakeEventModel{db: db},
		Conn:        fakeConn{db: db},
	}
	return ctx, db
}

// defaultPlan 是会员单/硬币包单常用的一份「在售、双端可见、有价」套餐。
func defaultPlan(planID int64, vip memberrpc.VipType) *memberrpc.PlanInfo {
	p := &memberrpc.PlanInfo{
		PlanId:       planID,
		PlanCode:     fmt.Sprintf("code_%d", planID),
		Name:         "大会员月卡",
		VipType:      vip,
		DurationDays: 31,
		UnitCount:    1,
		PriceMinor:   3000,
		Currency:     "cny",
		Platforms:    []memberrpc.PlanPlatform{memberrpc.PlanPlatform_PLAN_PLATFORM_ANDROID, memberrpc.PlanPlatform_PLAN_PLATFORM_IOS},
		State:        memberrpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE,
	}
	return p
}

// ---------------------------------------------------------------- 假 model ----

type fakeOrderModel struct {
	db *fakeDB
	model.OrderModel
}

func (f *fakeOrderModel) InsertTx(_ context.Context, _ sqlx.Session, o *model.Order) (int64, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	if f.db.insertErr != nil {
		return 0, f.db.insertErr
	}
	if o.OrderNo == "" {
		return 0, model.ErrOrderNoRequired
	}
	if o.RequestID == "" {
		return 0, model.ErrRequestIdRequired
	}
	if _, dup := f.db.orders[o.OrderNo]; dup {
		return 0, model.ErrOrderNoCollision
	}
	if f.requestIDTaken(o.RequestID) {
		return 0, model.ErrDuplicateRequest
	}
	f.db.nextOrderID++
	stored := *o
	stored.ID = f.db.nextOrderID
	// 与真实现一致：缺省时间/首版号由 model 补齐并回写到调用方手里的行。
	if o.CreatedAt == 0 {
		o.CreatedAt = fakeNow()
	}
	if o.UpdatedAt == 0 {
		o.UpdatedAt = fakeNow()
	}
	if o.Version == 0 {
		o.Version = 1
	}
	stored.CreatedAt, stored.UpdatedAt, stored.Version = o.CreatedAt, o.UpdatedAt, o.Version
	f.db.orders[stored.OrderNo] = &stored
	f.db.orderSeq[stored.OrderNo] = stored.ID
	return stored.ID, nil
}

func (f *fakeOrderModel) requestIDTaken(requestID string) bool {
	for _, o := range f.db.orders {
		if o.RequestID == requestID {
			return true
		}
	}
	return false
}

func (f *fakeOrderModel) FindByOrderNo(_ context.Context, orderNo string) (*model.Order, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	f.db.calls = append(f.db.calls, "FindByOrderNo")
	if f.db.findOrderErr != nil {
		return nil, f.db.findOrderErr
	}
	return f.findLocked(orderNo), nil
}

func (f *fakeOrderModel) FindByOrderNoTx(ctx context.Context, _ sqlx.Session, orderNo string) (*model.Order, error) {
	return f.FindByOrderNo(ctx, orderNo)
}

func (f *fakeOrderModel) findLocked(orderNo string) *model.Order {
	o, ok := f.db.orders[orderNo]
	if !ok {
		return nil
	}
	c := *o
	return &c
}

func (f *fakeOrderModel) FindByRequestID(_ context.Context, requestID string) (*model.Order, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	if requestID == "" {
		return nil, nil
	}
	if f.db.findRequestErr != nil {
		return nil, f.db.findRequestErr
	}
	if f.db.hideRequestIDLookups > 0 {
		f.db.hideRequestIDLookups--
		return nil, nil
	}
	for _, o := range f.db.orders {
		if o.RequestID == requestID {
			c := *o
			return &c, nil
		}
	}
	return nil, nil
}

// TransitionTx 复刻真实现的三道关卡：CanTransition 把关、CAS 条件（state+version）、
// RowsAffected==0 即 (false, nil)。缺任一关卡，logic 的并发结论就无从证明。
func (f *fakeOrderModel) TransitionTx(_ context.Context, _ sqlx.Session, orderNo string,
	from, to int32, expectedVersion int64, upd *model.OrderUpdate,
) (bool, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()

	rec := recordedTransition{OrderNo: orderNo, From: from, To: to, Version: expectedVersion}
	defer func() { f.db.transitions = append(f.db.transitions, rec) }()

	if orderNo == "" {
		rec.Err = model.ErrOrderNoRequired
		return false, model.ErrOrderNoRequired
	}
	if !model.CanTransition(from, to) {
		err := fmt.Errorf("%w: %s -> %s", model.ErrInvalidStateTransition, model.StateName(from), model.StateName(to))
		rec.Err = err
		return false, err
	}
	if expectedVersion <= 0 {
		rec.Err = model.ErrExpectedVersionRequired
		return false, model.ErrExpectedVersionRequired
	}
	o, ok := f.db.orders[orderNo]
	if !ok {
		rec.Matched = false
		return false, nil
	}
	if f.db.raceHook != nil {
		f.db.raceHook(o, from, to)
		f.db.recordExternalWrite(orderNo, from, to, f.db.raceHook)
	}
	if o.State != from || o.Version != expectedVersion {
		rec.Matched = false
		return false, nil
	}
	if f.db.shouldFailAt(pairName(from, to)) {
		err := errors.New("injected tx failure")
		rec.Err = err
		return false, err
	}
	o.State = to
	o.Version = expectedVersion + 1
	o.UpdatedAt = fakeNow()
	applyOrderUpdate(o, upd)
	rec.Matched = true
	return true, nil
}

func (f *fakeOrderModel) AnnotateTx(_ context.Context, _ sqlx.Session, orderNo string,
	state int32, expectedVersion int64, detail string,
) (bool, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()

	if !model.ValidState(state) {
		return false, fmt.Errorf("%w: state=%d", model.ErrInvalidStateTransition, state)
	}
	if expectedVersion <= 0 {
		return false, model.ErrExpectedVersionRequired
	}
	o, ok := f.db.orders[orderNo]
	if !ok {
		return false, nil
	}
	if f.db.raceHook != nil {
		f.db.raceHook(o, state, state)
		f.db.recordExternalWrite(orderNo, state, state, f.db.raceHook)
	}
	if o.State != state || o.Version != expectedVersion {
		return false, nil
	}
	if f.db.shouldFailAt(pairName(state, state)) {
		return false, errors.New("injected tx failure")
	}
	o.FulfillDetail = truncateRunes(detail, 500)
	o.Version = expectedVersion + 1
	o.UpdatedAt = fakeNow()
	return true, nil
}

func (f *fakeOrderModel) ListByFilter(_ context.Context, f2 *model.OrderFilter) ([]*model.Order, int64, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	f.db.calls = append(f.db.calls, "ListByFilter")
	// 记下实参（存副本：logic 复用的 filter 指针之后可能被别处改动）。
	f.db.lastFilter = nil
	if f2 != nil {
		cp := *f2
		f.db.lastFilter = &cp
	}
	if f.db.listErr != nil {
		return nil, 0, f.db.listErr
	}
	if f2 == nil {
		return nil, 0, model.ErrFilterRequired
	}
	if f2.Limit <= 0 {
		return nil, 0, model.ErrInvalidPage
	}
	rows := make([]*model.Order, 0, len(f.db.orders))
	for _, o := range f.db.orders {
		if orderMatches(o, f2) {
			c := *o
			rows = append(rows, &c)
		}
	}
	// 与真 SQL 一致：ORDER BY created_at DESC, id DESC
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && orderAfter(rows[j], rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	total := int64(len(rows))
	if total == 0 {
		return nil, 0, nil
	}
	start := int(f2.Offset)
	if start > len(rows) {
		start = len(rows)
	}
	end := start + int(f2.Limit)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], total, nil
}

func orderAfter(a, b *model.Order) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID > b.ID
}

func orderMatches(o *model.Order, f *model.OrderFilter) bool {
	if f.Mid > 0 && o.Mid != f.Mid {
		return false
	}
	if len(f.States) > 0 {
		ok := false
		for _, s := range f.States {
			if s == o.State {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	} else if f.State > 0 && o.State != f.State {
		return false
	}
	if f.BizType > 0 && o.BizType != f.BizType {
		return false
	}
	if f.PayMethod > 0 && o.PayMethod != f.PayMethod {
		return false
	}
	if f.OrderNo != "" && o.OrderNo != f.OrderNo {
		return false
	}
	if f.PaymentNo != "" && o.PaymentNo != f.PaymentNo {
		return false
	}
	if f.FromTs > 0 && o.CreatedAt < f.FromTs {
		return false
	}
	if f.ToTs > 0 && o.CreatedAt > f.ToTs {
		return false
	}
	return len(f.States) > 0 || f.State > 0 || f.Mid > 0 || f.OrderNo != "" || f.PaymentNo != "" ||
		f.BizType > 0 || f.PayMethod > 0 || f.FromTs > 0 || f.ToTs > 0
}

func (f *fakeOrderModel) ListStuck(_ context.Context, states []int32, updatedBefore, limit int64) ([]*model.Order, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	f.db.calls = append(f.db.calls, "ListStuck")
	f.db.lastStuckStates = append([]int32(nil), states...)
	f.db.lastStuckBefore, f.db.lastStuckLimit = updatedBefore, limit
	if f.db.stuckErr != nil {
		return nil, f.db.stuckErr
	}
	if len(states) == 0 || limit <= 0 {
		return nil, nil
	}
	want := map[int32]bool{}
	for _, s := range states {
		want[s] = true
	}
	rows := make([]*model.Order, 0, 4)
	for _, o := range f.db.orders {
		if want[o.State] && o.UpdatedAt < updatedBefore {
			c := *o
			rows = append(rows, &c)
		}
	}
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].UpdatedAt < rows[j-1].UpdatedAt; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	if int64(len(rows)) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// --- 台账 ---

type fakeEventModel struct {
	db *fakeDB
	model.OrderEventModel
}

func (f *fakeEventModel) InsertTx(_ context.Context, _ sqlx.Session, e *model.OrderEvent) (int64, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	if e.OrderNo == "" {
		return 0, model.ErrOrderNoRequired
	}
	if !model.ValidState(e.ToState) {
		return 0, fmt.Errorf("%w: to_state=%d", model.ErrInvalidStateTransition, e.ToState)
	}
	if e.FromState != 0 && !model.ValidState(e.FromState) {
		return 0, fmt.Errorf("%w: from_state=%d", model.ErrInvalidStateTransition, e.FromState)
	}
	if f.db.shouldFailAt(pairName(e.FromState, e.ToState)) {
		return 0, errors.New("injected ledger failure")
	}
	f.db.eventSeq++
	stored := *e
	stored.EventID = f.db.eventSeq
	if stored.Ctime == 0 {
		stored.Ctime = fakeNow()
	}
	f.db.events = append(f.db.events, &stored)
	return stored.EventID, nil
}

func (f *fakeEventModel) ListByOrderNo(_ context.Context, orderNo string, offset, limit int64) ([]*model.OrderEvent, int64, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	f.db.calls = append(f.db.calls, "ListOrderEvents")
	f.db.lastEventsOff, f.db.lastEventsLimit = offset, limit
	if f.db.listEventsErr != nil {
		return nil, 0, f.db.listEventsErr
	}
	if orderNo == "" {
		return nil, 0, model.ErrOrderNoRequired
	}
	if limit <= 0 {
		return nil, 0, model.ErrInvalidPage
	}
	rows := f.forOrder(orderNo)
	// 与真 SQL 一致：ORDER BY ctime ASC, event_id ASC（model/to_order_event.go 的
	// ListByOrderNo）。台账必须读成正史，所以假实现也排序而不是沿用插入顺序；
	// forOrder 返回的是拷贝，排它不会影响 db.events 本身。
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && eventBefore(rows[j], rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	total := int64(len(rows))
	if total == 0 {
		return nil, 0, nil
	}
	start := int(offset)
	if start > len(rows) {
		start = len(rows)
	}
	end := start + int(limit)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], total, nil
}

// eventBefore 判定台账行的先后：ctime 优先，同秒按自增主键（与 SQL 的排序键一致）。
func eventBefore(a, b *model.OrderEvent) bool {
	if a.Ctime != b.Ctime {
		return a.Ctime < b.Ctime
	}
	return a.EventID < b.EventID
}

func (f *fakeEventModel) forOrder(orderNo string) []*model.OrderEvent {
	out := make([]*model.OrderEvent, 0, 4)
	for _, e := range f.db.events {
		if e.OrderNo == orderNo {
			c := *e
			out = append(out, &c)
		}
	}
	return out
}

func (f *fakeEventModel) FindByRequestAndState(_ context.Context, orderNo, requestID string,
	toState int32,
) (*model.OrderEvent, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	if orderNo == "" || requestID == "" {
		return nil, nil
	}
	var last *model.OrderEvent
	for _, e := range f.db.events {
		if e.OrderNo == orderNo && e.RequestID == requestID && e.ToState == toState {
			c := *e
			last = &c
		}
	}
	return last, nil
}

func (f *fakeEventModel) FindLastTransitionTo(_ context.Context, orderNo string, toState int32) (*model.OrderEvent, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	if orderNo == "" {
		return nil, model.ErrOrderNoRequired
	}
	var last *model.OrderEvent
	for _, e := range f.db.events {
		if e.OrderNo == orderNo && e.ToState == toState {
			c := *e
			last = &c
		}
	}
	return last, nil
}

// ------------------------------------------------- OrderUpdate 施加（只读反射） ----

// applyOrderUpdate 按 model/to_order.go 里那批硬编码 SET 片段更新内存行。
// 列集合与片段文本必须一一对应；出现未知片段直接 panic（见文件头说明）。
func applyOrderUpdate(o *model.Order, upd *model.OrderUpdate) {
	if upd == nil {
		return
	}
	sets, args := readUpdateFields(upd)
	argIdx := 0
	for _, clause := range sets {
		switch clause {
		case "fulfill_attempts = fulfill_attempts + 1":
			o.FulfillAttempts++
			continue
		case "refunded_minor = refunded_minor + ?":
			o.RefundedMinor += args[argIdx].i
			argIdx++
			continue
		}
		col, ok := strings.CutSuffix(clause, " = ?")
		if !ok {
			panic(fmt.Sprintf("trade-order fake model: 未预期的 OrderUpdate SET 片段 %q —— "+
				"model 里新增了可写列，假实现必须同步（否则测试会漏掉该列的断言）", clause))
		}
		a := args[argIdx]
		argIdx++
		switch col {
		case "payment_no":
			o.PaymentNo = a.s
		case "grant_ref":
			o.GrantRef = a.s
		case "fulfill_detail":
			o.FulfillDetail = a.s
		case "paid_at":
			o.PaidAt = a.i
		case "fulfilled_at":
			o.FulfilledAt = a.i
		case "closed_at":
			o.ClosedAt = a.i
		case "fulfill_state":
			o.FulfillState = int32(a.i)
		default:
			panic(fmt.Sprintf("trade-order fake model: SET 列 %q 未实现，请补上", col))
		}
	}
}

type fakeArg struct {
	i int64
	s string
}

// readUpdateFields 只读地取出 OrderUpdate 的私有 sets/args（不写、不用 unsafe）。
func readUpdateFields(upd *model.OrderUpdate) ([]string, []fakeArg) {
	v := reflect.ValueOf(upd).Elem()
	setF, argsF := v.Field(0), v.Field(1)
	if setF.Kind() != reflect.Slice || argsF.Kind() != reflect.Slice {
		panic("trade-order fake model: OrderUpdate 的字段布局变了，请同步本文件")
	}
	sets := make([]string, 0, setF.Len())
	for i := 0; i < setF.Len(); i++ {
		sets = append(sets, setF.Index(i).String())
	}
	args := make([]fakeArg, 0, argsF.Len())
	for i := 0; i < argsF.Len(); i++ {
		e := argsF.Index(i).Elem()
		switch e.Kind() {
		case reflect.String:
			args = append(args, fakeArg{s: e.String()})
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			args = append(args, fakeArg{i: e.Int()})
		default:
			panic(fmt.Sprintf("trade-order fake model: SET 参数类型 %s 未支持", e.Kind()))
		}
	}
	return sets, args
}

// truncateRunes 与 model/logic 的同名工具同语义（假实现里用于 AnnotateTx 的列宽保护）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

// ------------------------------------------------------------- 种子与断言 ----

// seedOrder 放一张订单（返回内存里的指针拷贝）。
func seedOrder(t *testing.T, db *fakeDB, o *model.Order) *model.Order {
	t.Helper()
	if o.OrderNo == "" {
		t.Fatal("seedOrder 必须给 order_no")
	}
	if o.State == 0 {
		o.State = model.StatePaid
	}
	if o.Version == 0 {
		o.Version = 1
	}
	if o.RequestID == "" {
		o.RequestID = "req_" + o.OrderNo
	}
	if o.CreatedAt == 0 {
		o.CreatedAt = fakeNow() - 60
	}
	if o.UpdatedAt == 0 {
		o.UpdatedAt = fakeNow() - 60
	}
	if id, ok := db.orderSeq[o.OrderNo]; ok {
		o.ID = id
	} else {
		db.nextOrderID++
		db.orderSeq[o.OrderNo] = db.nextOrderID
		o.ID = db.nextOrderID
	}
	stored := *o
	db.orders[o.OrderNo] = &stored
	c := stored
	return &c
}

// seedLedger 放一行台账（RejectRefund 反查原状态、幂等重放判定都用它）。
func seedLedger(t *testing.T, db *fakeDB, e *model.OrderEvent) {
	t.Helper()
	if e.Ctime == 0 {
		e.Ctime = fakeNow()
	}
	db.eventSeq++
	e.EventID = db.eventSeq
	stored := *e
	db.events = append(db.events, &stored)
}

func orderState(t *testing.T, db *fakeDB, orderNo string) int32 {
	t.Helper()
	o, ok := db.orders[orderNo]
	if !ok {
		t.Fatalf("订单 %s 不存在", orderNo)
	}
	return o.State
}

func mustOrder(t *testing.T, db *fakeDB, orderNo string) *model.Order {
	t.Helper()
	o, ok := db.orders[orderNo]
	if !ok {
		t.Fatalf("订单 %s 不存在", orderNo)
	}
	c := *o
	return &c
}

func ledgerOf(db *fakeDB, orderNo string) []*model.OrderEvent {
	out := make([]*model.OrderEvent, 0, len(db.events))
	for _, e := range db.events {
		if e.OrderNo == orderNo {
			c := *e
			out = append(out, &c)
		}
	}
	return out
}

func ledgerPairs(db *fakeDB, orderNo string) [][2]string {
	rows := ledgerOf(db, orderNo)
	out := make([][2]string, 0, len(rows))
	for _, e := range rows {
		out = append(out, [2]string{model.StateName(e.FromState), model.StateName(e.ToState)})
	}
	return out
}

// assertOnlyLegalTransitions 是本服务数据所有权（AGENTS.md §5）的可执行表达：
// 订单状态只能经 model.CanTransition 认可的边推进，且终态一旦被记录就不得再有出边。
// 每个流程用例收尾都调它，这样「顺手加一条迁移」会在多个用例里同时炸。
func assertOnlyLegalTransitions(t *testing.T, db *fakeDB) {
	t.Helper()
	for _, tr := range db.transitions {
		if tr.Err != nil {
			// 被拒的迁移是断言对象（非法前态必须被拒），只要它确实没生效就行。
			if tr.Matched {
				t.Errorf("迁移 %s 报错却仍生效：%v", pairName(tr.From, tr.To), tr.Err)
			}
			continue
		}
		if !model.CanTransition(tr.From, tr.To) {
			t.Errorf("发生了 CanTransition 未授权的迁移 %s（order=%s）", pairName(tr.From, tr.To), tr.OrderNo)
		}
		switch tr.From {
		case model.StateCancelled, model.StateFailed, model.StateRefunded, model.StateRefundRejected:
			t.Errorf("终态 %s 出现了出边 -> %s（order=%s）", model.StateName(tr.From), model.StateName(tr.To), tr.OrderNo)
		}
	}
}

func quietLogger() logx.Logger { return logx.WithContext(context.Background()) }

// --------------------------------------------------- 触库序列与只读断言 ----

// assertCalls 断言本用例到目前为止触达 model 的方法序列。
// 读侧用例用它钉「入参守卫发生在触库之前」：守卫写反成「先查再判」时，
// 返回值可能恰好还对，但序列会多出一条 ListByFilter/FindByOrderNo。
func assertCalls(t *testing.T, db *fakeDB, want ...string) {
	t.Helper()
	got := append([]string(nil), db.calls...)
	if len(got) != len(want) {
		t.Fatalf("触库序列 = %v，期望 %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("触库序列 = %v，期望 %v", got, want)
		}
	}
}

func countCalls(db *fakeDB, name string) int {
	n := 0
	for _, c := range db.calls {
		if c == name {
			n++
		}
	}
	return n
}

// writeMark 记录一次「写副作用基线」。用例自己 seed 进替身的行不算被测代码写的，
// 所以断言只比较标记之后的增量。
type writeMark struct {
	txRuns      int
	transitions int
	events      int
}

func (db *fakeDB) markWrites() writeMark {
	return writeMark{txRuns: db.txRuns, transitions: len(db.transitions), events: len(db.events)}
}

// assertNoWritesAfter 断言从标记到现在没有任何写：没有事务、没有 CAS、没有台账行。
func assertNoWritesAfter(t *testing.T, db *fakeDB, m writeMark) {
	t.Helper()
	if db.txRuns != m.txRuns {
		t.Errorf("读路径开启了 %d 次写事务", db.txRuns-m.txRuns)
	}
	if len(db.transitions) != m.transitions {
		t.Errorf("读路径记录了 %d 次状态迁移", len(db.transitions)-m.transitions)
	}
	if len(db.events) != m.events {
		t.Errorf("读路径写了 %d 行台账", len(db.events)-m.events)
	}
}

// assertReadOnly 钉住「这次调用一个字节都没写」：没有事务、没有 CAS、没有台账行。
// 读接口顺手改状态（尤其卡单扫描「顺便补偿一下」）是本服务最危险的越权形态。
// 用例自己 seed 过数据时用 assertNoWritesAfter（基线不是零）。
func assertReadOnly(t *testing.T, db *fakeDB) {
	t.Helper()
	assertNoWritesAfter(t, db, writeMark{})
}

// ---------------------------------------------------------------- 假下游 RPC ----
//
// 只实现被调用的方法；其余方法由内嵌 nil 接口提升，误调即 panic。

type fakeMembership struct {
	memberrpc.MembershipClient

	plans      map[int64]*memberrpc.PlanInfo
	byCode     map[string]*memberrpc.PlanInfo
	getPlanErr error
	grantErr   error
	revokeErr  error

	planCalls   int
	grantCalls  []*memberrpc.GrantMembershipReq
	revokeCalls []*memberrpc.RevokeMembershipReq
	grantSeq    int64
	duplicated  bool
}

func newFakeMembership() *fakeMembership {
	return &fakeMembership{plans: map[int64]*memberrpc.PlanInfo{}, byCode: map[string]*memberrpc.PlanInfo{}}
}

func (m *fakeMembership) withPlan(p *memberrpc.PlanInfo) *fakeMembership {
	m.plans[p.GetPlanId()] = p
	m.byCode[p.GetPlanCode()] = p
	return m
}

func (m *fakeMembership) GetPlan(_ context.Context, in *memberrpc.GetPlanReq,
	_ ...grpc.CallOption,
) (*memberrpc.GetPlanReply, error) {
	m.planCalls++
	if m.getPlanErr != nil {
		return nil, m.getPlanErr
	}
	if in.GetPlanId() > 0 {
		if p, ok := m.plans[in.GetPlanId()]; ok {
			return &memberrpc.GetPlanReply{Found: true, Plan: p}, nil
		}
		return &memberrpc.GetPlanReply{Found: false}, nil
	}
	if p, ok := m.byCode[strings.TrimSpace(in.GetPlanCode())]; ok {
		return &memberrpc.GetPlanReply{Found: true, Plan: p}, nil
	}
	return &memberrpc.GetPlanReply{Found: false}, nil
}

func (m *fakeMembership) GrantMembership(_ context.Context, in *memberrpc.GrantMembershipReq,
	_ ...grpc.CallOption,
) (*memberrpc.GrantMembershipReply, error) {
	m.grantCalls = append(m.grantCalls, in)
	if m.grantErr != nil {
		return nil, m.grantErr
	}
	m.grantSeq++
	return &memberrpc.GrantMembershipReply{
		GrantId:    7000 + m.grantSeq,
		Duplicated: m.duplicated,
	}, nil
}

func (m *fakeMembership) RevokeMembership(_ context.Context, in *memberrpc.RevokeMembershipReq,
	_ ...grpc.CallOption,
) (*memberrpc.RevokeMembershipReply, error) {
	m.revokeCalls = append(m.revokeCalls, in)
	if m.revokeErr != nil {
		return nil, m.revokeErr
	}
	m.grantSeq++
	return &memberrpc.RevokeMembershipReply{GrantId: 9000 + m.grantSeq, Duplicated: m.duplicated}, nil
}

type fakePayment struct {
	paymentrpc.PaymentClient

	createErr     error
	createState   paymentrpc.PaymentState
	createAmount  int64 // 0 表示回显订单金额
	emptyPayment  bool  // 注入：下游受理成功却不回支付记录（契约违约）
	getErr        error
	getFound      bool
	getState      paymentrpc.PaymentState
	closeErr      error
	refundErr     error
	refundState   paymentrpc.RefundState
	refundAmount  int64
	refundCur     string
	duplicated    bool
	refundErrText string

	createCalls []*paymentrpc.CreatePaymentReq
	getCalls    []*paymentrpc.GetPaymentReq
	closeCalls  []*paymentrpc.ClosePaymentReq
	refundCalls []*paymentrpc.RefundPaymentReq
	listCalls   int
	listRefunds []*paymentrpc.RefundInfo
}

func newFakePayment() *fakePayment {
	return &fakePayment{createState: paymentrpc.PaymentState_PAYMENT_STATE_PAID}
}

func (p *fakePayment) CreatePayment(_ context.Context, in *paymentrpc.CreatePaymentReq,
	_ ...grpc.CallOption,
) (*paymentrpc.CreatePaymentReply, error) {
	p.createCalls = append(p.createCalls, in)
	if p.createErr != nil {
		return nil, p.createErr
	}
	if p.emptyPayment {
		return &paymentrpc.CreatePaymentReply{}, nil
	}
	amount := p.createAmount
	if amount == 0 {
		amount = in.GetAmountMinor()
	}
	return &paymentrpc.CreatePaymentReply{Payment: &paymentrpc.PaymentInfo{
		PaymentNo:   "pay_" + in.GetBizOrderNo(),
		BizOrderNo:  in.GetBizOrderNo(),
		Mid:         in.GetMid(),
		AmountMinor: amount,
		Currency:    in.GetCurrency(),
		Method:      in.GetMethod(),
		State:       p.createState,
		RequestId:   in.GetRequestId(),
	}}, nil
}

func (p *fakePayment) GetPayment(_ context.Context, in *paymentrpc.GetPaymentReq,
	_ ...grpc.CallOption,
) (*paymentrpc.GetPaymentReply, error) {
	p.getCalls = append(p.getCalls, in)
	if p.getErr != nil {
		return nil, p.getErr
	}
	if !p.getFound {
		return &paymentrpc.GetPaymentReply{Found: false}, nil
	}
	return &paymentrpc.GetPaymentReply{Found: true, Payment: &paymentrpc.PaymentInfo{
		PaymentNo:  "pay_" + in.GetBizOrderNo(),
		BizOrderNo: in.GetBizOrderNo(),
		State:      p.getState,
	}}, nil
}

func (p *fakePayment) ClosePayment(_ context.Context, in *paymentrpc.ClosePaymentReq,
	_ ...grpc.CallOption,
) (*paymentrpc.ClosePaymentReply, error) {
	p.closeCalls = append(p.closeCalls, in)
	if p.closeErr != nil {
		return nil, p.closeErr
	}
	return &paymentrpc.ClosePaymentReply{Payment: &paymentrpc.PaymentInfo{PaymentNo: in.GetPaymentNo()}}, nil
}

func (p *fakePayment) RefundPayment(_ context.Context, in *paymentrpc.RefundPaymentReq,
	_ ...grpc.CallOption,
) (*paymentrpc.RefundPaymentReply, error) {
	p.refundCalls = append(p.refundCalls, in)
	if p.refundErr != nil {
		return nil, p.refundErr
	}
	if p.refundErrText != "" {
		return nil, errors.New(p.refundErrText)
	}
	amount := p.refundAmount
	if amount == 0 {
		amount = in.GetAmountMinor()
	}
	state := p.refundState
	if state == paymentrpc.RefundState_REFUND_STATE_UNSPECIFIED {
		state = paymentrpc.RefundState_REFUND_STATE_SUCCEEDED
	}
	cur := p.refundCur
	if cur == "" {
		cur = "CNY"
	}
	return &paymentrpc.RefundPaymentReply{
		Duplicated: p.duplicated,
		Refund: &paymentrpc.RefundInfo{
			RefundNo:    "rf_" + in.GetPaymentNo(),
			PaymentNo:   in.GetPaymentNo(),
			AmountMinor: amount,
			Currency:    cur,
			State:       state,
			RequestId:   in.GetRequestId(),
			Destination: "BALANCE",
		},
	}, nil
}

func (p *fakePayment) ListRefunds(_ context.Context, _ *paymentrpc.ListRefundsReq,
	_ ...grpc.CallOption,
) (*paymentrpc.ListRefundsReply, error) {
	p.listCalls++
	return &paymentrpc.ListRefundsReply{Refunds: p.listRefunds}, nil
}

type fakeCoin struct {
	coinrpc.CoinClient

	grantErr   error
	duplicated bool

	grantCalls []*coinrpc.GrantCoinReq
	grantSeq   int64
}

func (c *fakeCoin) GrantCoin(_ context.Context, in *coinrpc.GrantCoinReq,
	_ ...grpc.CallOption,
) (*coinrpc.GrantCoinReply, error) {
	c.grantCalls = append(c.grantCalls, in)
	if c.grantErr != nil {
		return nil, c.grantErr
	}
	c.grantSeq++
	return &coinrpc.GrantCoinReply{FlowId: 5000 + c.grantSeq, Duplicated: c.duplicated}, nil
}

// wireDownstream 把三个假客户端装到 ServiceContext 上。
func wireDownstream(s *svc.ServiceContext, m *fakeMembership, p *fakePayment, c *fakeCoin) {
	if m != nil {
		s.Membership = m
	}
	if p != nil {
		s.Payment = p
	}
	if c != nil {
		s.Coin = c
	}
}
