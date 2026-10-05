package logic

// 本文件提供 payment logic 单测用的「内存版资金台账」与「假事务」。
//
// 为什么可以这样注入（以及为什么这不是伪装成功）：
//   - svc.ServiceContext.Models 由 model.NewModels(sqlx.SqlConn) 构造，五个表字段都是接口类型，
//     测试可以先用假连接构造、再逐字段换成内存实现；
//   - 真实并发正确性由 MySQL 的 uniq_* 唯一索引与 UPDATE ... WHERE 守卫保证，单测无法也不该
//     复刻行锁；这里复刻的是**语义**：唯一键命中即返回 1062、CAS 条件不命中即 0 行、
//     余额不足即 0 行、事务回调报错则整体回滚。断言的是 logic 面对这些返回值时的裁决，
//     也就是无库条件下唯一可证明的部分；
//   - 每个假实现只覆写被测路径用到的方法，其余方法落到「以 fakeConn 构造的真实 model」上，
//     而 fakeConn 只实现 TransactCtx，其余 sqlx 方法都是对 nil 接口发起调用 —— 走到即 panic，
//     失败是响的，不会被写成通过；
//   - IsDuplicate 复用生产实现（default*Model.IsDuplicate 不碰连接），因此测试里的
//     「唯一键冲突」判定不会与线上漂移。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/payment/internal/config"
	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeNow 与 model.nowUnix 同源（真实时钟）。用例一律按「相对现在的偏移」构造时间。
func fakeNow() int64 { return time.Now().Unix() }

// dupErr 复刻 MySQL 1062 文本形态（model.isDuplicateErr 就是按这个子串判定的）。
func dupErr(key, val string) error {
	return fmt.Errorf("Error 1062: Duplicate entry '%s' for key '%s'", val, key)
}

// fakeDB 是一次测试的全部内存台账。TransactCtx 在入口快照、回调报错时整体回滚，
// 因此「已扣余额却没落流水」这种半成品会被当场测出来。
type fakeDB struct {
	wallets   map[int64]*model.Wallet
	recharges map[string]*model.Recharge // key: recharge_no
	payments  map[string]*model.Payment  // key: payment_no
	refunds   map[string]*model.Refund   // key: refund_no
	flows     []*model.Flow              // append-only，按写入顺序

	nextWallet   int64
	nextRecharge int64
	nextPayment  int64
	nextRefund   int64
	nextFlow     int64

	// --- 故障注入：唯一键冲突（模拟并发对手先提交）---
	rechargeDup bool
	paymentDup  bool
	refundDup   bool
	flowDup     bool

	// --- 故障注入：底层报错（非唯一键）---
	rechargeErr error
	paymentErr  error
	refundErr   error
	flowErr     error

	// --- CAS 条件不命中注入（模拟并发把状态先推进了）---
	settleMiss    bool
	cancelMiss    bool
	closeMiss     bool
	refundCasMiss bool

	// settleSteal 模拟「另一个事务在本次事务执行期间先结算并提交了」：
	// CAS 返回 0 行，对手的写入在**本事务回滚之后**才落进内存 ——
	// 因为对手是独立事务，本事务回滚不该抹掉它的已提交结果。
	settleSteal func(db *fakeDB)
	// paymentSteal 同理，但对手撞的是唯一键（并发下先插入了同一笔支付单）：
	// 本事务拿到 1062 并回滚，回查时必须能看见对手已提交的那一行。
	paymentSteal func(db *fakeDB)
	// refundSteal / flowSteal 同上，用于验证「唯一键冲突后回读成重放」的分支：
	// 回滚后必须能读到对手那笔已提交的退款单/流水，否则结论是「号被复用」。
	refundSteal func(db *fakeDB)
	flowSteal   func(db *fakeDB)
	// cancelSteal / closeSteal 同理，但取消/关单路径本身不开事务，写入直接生效。
	cancelSteal func(db *fakeDB)
	closeSteal  func(db *fakeDB)

	afterRollback func(db *fakeDB)

	txRuns    int
	rollbacks int
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		wallets:   map[int64]*model.Wallet{},
		recharges: map[string]*model.Recharge{},
		payments:  map[string]*model.Payment{},
		refunds:   map[string]*model.Refund{},
	}
}

func (db *fakeDB) snapshot() *fakeDB {
	cp := &fakeDB{
		wallets:      map[int64]*model.Wallet{},
		recharges:    map[string]*model.Recharge{},
		payments:     map[string]*model.Payment{},
		refunds:      map[string]*model.Refund{},
		nextWallet:   db.nextWallet,
		nextRecharge: db.nextRecharge,
		nextPayment:  db.nextPayment,
		nextRefund:   db.nextRefund,
		nextFlow:     db.nextFlow,
		txRuns:       db.txRuns,
		rollbacks:    db.rollbacks,
	}
	for k, v := range db.wallets {
		c := *v
		cp.wallets[k] = &c
	}
	for k, v := range db.recharges {
		c := *v
		cp.recharges[k] = &c
	}
	for k, v := range db.payments {
		c := *v
		cp.payments[k] = &c
	}
	for k, v := range db.refunds {
		c := *v
		cp.refunds[k] = &c
	}
	for _, f := range db.flows {
		c := *f
		cp.flows = append(cp.flows, &c)
	}
	return cp
}

func (db *fakeDB) restore(s *fakeDB) {
	db.wallets = s.wallets
	db.recharges = s.recharges
	db.payments = s.payments
	db.refunds = s.refunds
	db.flows = s.flows
	db.nextWallet = s.nextWallet
	db.nextRecharge = s.nextRecharge
	db.nextPayment = s.nextPayment
	db.nextRefund = s.nextRefund
	db.nextFlow = s.nextFlow
	db.rollbacks++
	if hook := db.afterRollback; hook != nil {
		db.afterRollback = nil
		hook(db)
	}
}

// --- 假连接：只实现真正被测的 TransactCtx ---

type fakeConn struct {
	sqlx.SqlConn
	db *fakeDB
}

func (c fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.db.txRuns++
	snap := c.db.snapshot()
	if err := fn(ctx, nil); err != nil {
		c.db.restore(snap)
		return err
	}
	return nil
}

// --- 装配 ---

// defaultPaymentConf 与 etc/payment.v1.yaml 的发布默认值一致（不读文件，避免测试依赖部署物）。
func defaultPaymentConf() config.PaymentConf {
	return config.PaymentConf{
		DefaultCurrency:      "CNY",
		MinRechargeMinor:     1,
		MaxRechargeMinor:     200000,
		MaxAdjustMinor:       1000000,
		MaxPageSize:          100,
		MaxListWindowSeconds: 2592000,
		MaxListOffset:        10000,
		AllowedChannels:      []string{config.ChannelSandbox},
	}
}

// newTestSvc 构造「只替换数据访问、其余全用真实实现」的 ServiceContext。
// 币种/金额上下限/分页窗口/渠道门禁都是被测代码，不能用替身污染。
func newTestSvc(t *testing.T, cfg config.PaymentConf) (*svc.ServiceContext, *fakeDB) {
	t.Helper()
	db := newFakeDB()
	conn := fakeConn{db: db}
	models := model.NewModels(conn)
	// NewModels 里的真实模型会以 fakeConn 兜底（除 TransactCtx 外一律 panic），
	// 这里逐个换成内存实现，IsDuplicate 仍走生产判定逻辑。
	models.Wallet = fakeWallet{WalletModel: model.NewWalletModel(conn), db: db}
	models.Recharge = fakeRecharge{RechargeModel: model.NewRechargeModel(conn), db: db}
	models.Payment = fakePayment{PaymentModel: model.NewPaymentModel(conn), db: db}
	models.Refund = fakeRefund{RefundModel: model.NewRefundModel(conn), db: db}
	models.Flow = fakeFlow{FlowModel: model.NewFlowModel(conn), db: db}
	return &svc.ServiceContext{
		Config: config.Config{Payment: cfg},
		Models: models,
	}, db
}

// --- pm_wallet ---

type fakeWallet struct {
	model.WalletModel
	db *fakeDB
}

func (f fakeWallet) FindOne(_ context.Context, mid int64) (*model.Wallet, error) {
	return f.find(mid), nil
}

func (f fakeWallet) FindOneTx(_ context.Context, _ sqlx.Session, mid int64) (*model.Wallet, error) {
	return f.find(mid), nil
}

func (f fakeWallet) find(mid int64) *model.Wallet {
	w, ok := f.db.wallets[mid]
	if !ok {
		return nil
	}
	c := *w
	return &c
}

// ApplyDeltaTx 复刻生产 SQL 的三条语义：
//  1. INSERT ... ON DUPLICATE KEY UPDATE id=id 保证账户行存在（建行只发生在要动钱的写入里）；
//  2. 扣减条件写在 WHERE 里（balance_minor >= -delta），0 行即余额不足，不做先查后改；
//  3. 币种参与 WHERE，不匹配即 0 行 → ErrUnsupportedCurrency，绝不隐式换汇。
//
// 结果余额若为负，说明本假实现自己写错了守卫（生产还有 CHECK 兜底），这里直接 panic。
func (f fakeWallet) ApplyDeltaTx(_ context.Context, _ sqlx.Session,
	mid int64, currency string, delta int64) (*model.Wallet, error) {
	if mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if delta == 0 {
		return nil, model.ErrAdjustDeltaZero
	}
	w, ok := f.db.wallets[mid]
	if !ok {
		f.db.nextWallet++
		now := fakeNow()
		w = &model.Wallet{Id: f.db.nextWallet, Mid: mid, Currency: currency, Ctime: now, Mtime: now}
		f.db.wallets[mid] = w
	}
	if w.Currency != currency {
		return nil, model.ErrUnsupportedCurrency
	}
	if delta < 0 && w.BalanceMinor < -delta {
		return nil, model.ErrInsufficientBalance
	}
	w.BalanceMinor += delta
	if w.BalanceMinor < 0 {
		panic(fmt.Sprintf("fakeDB 守卫失效：mid=%d 余额被扣成 %d", mid, w.BalanceMinor))
	}
	w.Version++
	w.Mtime = fakeNow()
	c := *w
	return &c, nil
}

// --- pm_recharge ---

type fakeRecharge struct {
	model.RechargeModel
	db *fakeDB
}

func (f fakeRecharge) Insert(_ context.Context, r *model.Recharge) (int64, error) {
	if f.db.rechargeErr != nil {
		return 0, f.db.rechargeErr
	}
	if f.db.rechargeDup {
		return 0, dupErr("uniq_request_id", r.RequestId)
	}
	if _, ok := f.db.recharges[r.RechargeNo]; ok {
		return 0, dupErr("uniq_recharge_no", r.RechargeNo)
	}
	for _, ex := range f.db.recharges {
		if ex.RequestId == r.RequestId {
			return 0, dupErr("uniq_request_id", r.RequestId)
		}
	}
	f.db.nextRecharge++
	// 与生产一致：模型回填调用方结构体的时间列（OpenRecharge 的响应投影就取自它）。
	if r.Ctime == 0 {
		r.Ctime = fakeNow()
	}
	r.Mtime = r.Ctime
	stored := *r
	stored.Id = f.db.nextRecharge
	f.db.recharges[stored.RechargeNo] = &stored
	return f.db.nextRecharge, nil
}

func (f fakeRecharge) FindOne(_ context.Context, rechargeNo string) (*model.Recharge, error) {
	return f.find(rechargeNo), nil
}

func (f fakeRecharge) FindOneTx(_ context.Context, _ sqlx.Session, rechargeNo string) (*model.Recharge, error) {
	return f.find(rechargeNo), nil
}

func (f fakeRecharge) find(rechargeNo string) *model.Recharge {
	r, ok := f.db.recharges[rechargeNo]
	if !ok {
		return nil
	}
	c := *r
	return &c
}

func (f fakeRecharge) FindByRequestID(_ context.Context, requestID string) (*model.Recharge, error) {
	for _, r := range f.db.recharges {
		if r.RequestId == requestID {
			c := *r
			return &c, nil
		}
	}
	return nil, nil
}

// SettleTx 复刻 CAS：WHERE recharge_no = ? AND state = PENDING。
func (f fakeRecharge) SettleTx(_ context.Context, _ sqlx.Session,
	rechargeNo, operator, reason string, settledAt int64) (bool, error) {
	if f.db.settleSteal != nil {
		steal := f.db.settleSteal
		f.db.settleSteal = nil
		// 对手是独立事务：本事务回滚后再落它的已提交结果。
		f.db.afterRollback = func(db *fakeDB) { steal(db) }
		return false, nil
	}
	r, ok := f.db.recharges[rechargeNo]
	if !ok || f.db.settleMiss {
		if f.db.settleMiss {
			f.db.settleMiss = false
		}
		return false, nil
	}
	if r.State != model.RechargeStatePending {
		return false, nil
	}
	now := fakeNow()
	r.State = model.RechargeStateSuccess
	r.Operator, r.Reason = operator, reason
	if settledAt > 0 {
		r.SettledAt = settledAt
	} else {
		r.SettledAt = now
	}
	r.Mtime = now
	return true, nil
}

// CancelTx 复刻 CAS：WHERE recharge_no = ? AND state = PENDING。
func (f fakeRecharge) CancelTx(_ context.Context, _ sqlx.Session,
	rechargeNo, operator, reason string) (bool, error) {
	if f.db.cancelSteal != nil {
		steal := f.db.cancelSteal
		f.db.cancelSteal = nil
		steal(f.db) // 取消路径不开事务，对手的写入当场可见
	}
	r, ok := f.db.recharges[rechargeNo]
	if !ok || f.db.cancelMiss {
		if f.db.cancelMiss {
			f.db.cancelMiss = false
		}
		return false, nil
	}
	if r.State != model.RechargeStatePending {
		return false, nil
	}
	r.State = model.RechargeStateCancelled
	r.Operator, r.Reason, r.Mtime = operator, reason, fakeNow()
	return true, nil
}

func (f fakeRecharge) List(_ context.Context, q model.RechargeListQuery) ([]*model.Recharge, error) {
	var all []*model.Recharge
	for _, r := range f.db.recharges {
		if !rechargeMatches(r, q) {
			continue
		}
		c := *r
		all = append(all, &c)
	}
	return paginate(all, q.Page, func(r *model.Recharge) int64 { return r.Id }), nil
}

func (f fakeRecharge) Count(_ context.Context, q model.RechargeListQuery) (int64, error) {
	var n int64
	for _, r := range f.db.recharges {
		if rechargeMatches(r, q) {
			n++
		}
	}
	return n, nil
}

func rechargeMatches(r *model.Recharge, q model.RechargeListQuery) bool {
	if q.Mid != 0 && r.Mid != q.Mid {
		return false
	}
	return q.State == 0 || r.State == q.State
}

// --- pm_payment ---

type fakePayment struct {
	model.PaymentModel
	db *fakeDB
}

func (f fakePayment) InsertTx(_ context.Context, _ sqlx.Session, p *model.Payment) (int64, error) {
	if f.db.paymentErr != nil {
		return 0, f.db.paymentErr
	}
	if f.db.paymentDup {
		return 0, dupErr("uniq_request_id", p.RequestId)
	}
	if f.db.paymentSteal != nil {
		steal := f.db.paymentSteal
		f.db.paymentSteal = nil
		// 对手是独立事务：本事务回滚后才看得见它已提交的行。
		f.db.afterRollback = func(db *fakeDB) { steal(db) }
		return 0, dupErr("uniq_request_id", p.RequestId)
	}
	if _, ok := f.db.payments[p.PaymentNo]; ok {
		return 0, dupErr("uniq_payment_no", p.PaymentNo)
	}
	for _, ex := range f.db.payments {
		if ex.BizOrderNo == p.BizOrderNo {
			return 0, dupErr("uniq_biz_order_no", p.BizOrderNo)
		}
		if ex.RequestId == p.RequestId {
			return 0, dupErr("uniq_request_id", p.RequestId)
		}
	}
	f.db.nextPayment++
	if p.Ctime == 0 {
		p.Ctime = fakeNow()
	}
	p.Mtime = p.Ctime
	stored := *p
	// 与生产 INSERT 的列绑定保持一致：last_request_id 写 request_id、remark 写空串。
	stored.LastRequestId = p.RequestId
	stored.Remark = ""
	stored.Id = f.db.nextPayment
	f.db.payments[stored.PaymentNo] = &stored
	return f.db.nextPayment, nil
}

func (f fakePayment) FindOne(_ context.Context, paymentNo string) (*model.Payment, error) {
	return f.find(paymentNo), nil
}

func (f fakePayment) FindOneTx(_ context.Context, _ sqlx.Session, paymentNo string) (*model.Payment, error) {
	return f.find(paymentNo), nil
}

func (f fakePayment) find(paymentNo string) *model.Payment {
	p, ok := f.db.payments[paymentNo]
	if !ok {
		return nil
	}
	c := *p
	return &c
}

func (f fakePayment) FindByBizOrderNo(_ context.Context, bizOrderNo string) (*model.Payment, error) {
	for _, p := range f.db.payments {
		if p.BizOrderNo == bizOrderNo {
			c := *p
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakePayment) FindByRequestID(_ context.Context, requestID string) (*model.Payment, error) {
	for _, p := range f.db.payments {
		if p.RequestId == requestID {
			c := *p
			return &c, nil
		}
	}
	return nil, nil
}

// CloseTx 复刻 CAS：WHERE payment_no = ? AND state = PENDING。
func (f fakePayment) CloseTx(_ context.Context, _ sqlx.Session,
	paymentNo, operator, requestID, reason string) (bool, error) {
	if f.db.closeSteal != nil {
		steal := f.db.closeSteal
		f.db.closeSteal = nil
		steal(f.db)
	}
	p, ok := f.db.payments[paymentNo]
	if !ok || f.db.closeMiss {
		if f.db.closeMiss {
			f.db.closeMiss = false
		}
		return false, nil
	}
	if p.State != model.PaymentStatePending {
		return false, nil
	}
	now := fakeNow()
	p.State = model.PaymentStateClosed
	p.Operator, p.LastRequestId, p.Remark, p.Mtime = operator, requestID, reason, now
	return true, nil
}

// RefundTx 复刻生产 SQL 的守卫与状态推进：
//
//	WHERE payment_no = ? AND state IN (PAID, PARTIALLY_REFUNDED) AND refunded_minor + ? <= amount_minor
//	SET state = IF(refunded_minor >= amount_minor, REFUNDED, PARTIALLY_REFUNDED)  -- 用累加后的新值
//
// 0 行即「累计超退」或「状态不可退」，由 logic 判定整笔回滚。
func (f fakePayment) RefundTx(_ context.Context, _ sqlx.Session, paymentNo string, refundMinor int64,
	operator, requestID, remark string) (bool, error) {
	p, ok := f.db.payments[paymentNo]
	if !ok || f.db.refundCasMiss {
		if f.db.refundCasMiss {
			f.db.refundCasMiss = false
		}
		return false, nil
	}
	if p.State != model.PaymentStatePaid && p.State != model.PaymentStatePartiallyRefunded {
		return false, nil
	}
	if p.RefundedMinor+refundMinor > p.AmountMinor {
		return false, nil
	}
	p.RefundedMinor += refundMinor
	if p.RefundedMinor >= p.AmountMinor {
		p.State = model.PaymentStateRefunded
	} else {
		p.State = model.PaymentStatePartiallyRefunded
	}
	p.Operator, p.LastRequestId, p.Remark, p.Mtime = operator, requestID, remark, fakeNow()
	return true, nil
}

func (f fakePayment) List(_ context.Context, q model.PaymentListQuery) ([]*model.Payment, error) {
	var all []*model.Payment
	for _, p := range f.db.payments {
		if !paymentMatches(p, q) {
			continue
		}
		c := *p
		all = append(all, &c)
	}
	return paginate(all, q.Page, func(p *model.Payment) int64 { return p.Id }), nil
}

func (f fakePayment) Count(_ context.Context, q model.PaymentListQuery) (int64, error) {
	var n int64
	for _, p := range f.db.payments {
		if paymentMatches(p, q) {
			n++
		}
	}
	return n, nil
}

func paymentMatches(p *model.Payment, q model.PaymentListQuery) bool {
	if q.Mid != 0 && p.Mid != q.Mid {
		return false
	}
	if q.State != 0 && p.State != q.State {
		return false
	}
	if q.Method != 0 && p.Method != q.Method {
		return false
	}
	return true
}

// --- pm_refund ---

type fakeRefund struct {
	model.RefundModel
	db *fakeDB
}

func (f fakeRefund) InsertTx(_ context.Context, _ sqlx.Session, r *model.Refund) (int64, error) {
	if f.db.refundErr != nil {
		return 0, f.db.refundErr
	}
	if f.db.refundDup {
		return 0, dupErr("uniq_request_id", r.RequestId)
	}
	if f.db.refundSteal != nil {
		steal := f.db.refundSteal
		f.db.refundSteal = nil
		f.db.afterRollback = func(db *fakeDB) { steal(db) }
		return 0, dupErr("uniq_request_id", r.RequestId)
	}
	if _, ok := f.db.refunds[r.RefundNo]; ok {
		return 0, dupErr("uniq_refund_no", r.RefundNo)
	}
	for _, ex := range f.db.refunds {
		if ex.RequestId == r.RequestId {
			return 0, dupErr("uniq_request_id", r.RequestId)
		}
	}
	f.db.nextRefund++
	if r.Ctime == 0 {
		r.Ctime = fakeNow()
	}
	stored := *r
	stored.Id = f.db.nextRefund
	f.db.refunds[stored.RefundNo] = &stored
	return f.db.nextRefund, nil
}

func (f fakeRefund) FindOne(_ context.Context, refundNo string) (*model.Refund, error) {
	r, ok := f.db.refunds[refundNo]
	if !ok {
		return nil, nil
	}
	c := *r
	return &c, nil
}

func (f fakeRefund) FindByRequestID(_ context.Context, requestID string) (*model.Refund, error) {
	for _, r := range f.db.refunds {
		if r.RequestId == requestID {
			c := *r
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakeRefund) List(_ context.Context, q model.RefundListQuery) ([]*model.Refund, error) {
	var all []*model.Refund
	for _, r := range f.db.refunds {
		if q.Mid != 0 && r.Mid != q.Mid {
			continue
		}
		if q.PaymentNo != "" && r.PaymentNo != q.PaymentNo {
			continue
		}
		c := *r
		all = append(all, &c)
	}
	return paginate(all, q.Page, func(r *model.Refund) int64 { return r.Id }), nil
}

func (f fakeRefund) Count(_ context.Context, q model.RefundListQuery) (int64, error) {
	var n int64
	for _, r := range f.db.refunds {
		if (q.Mid == 0 || r.Mid == q.Mid) && (q.PaymentNo == "" || r.PaymentNo == q.PaymentNo) {
			n++
		}
	}
	return n, nil
}

// --- pm_flow ---

type fakeFlow struct {
	model.FlowModel
	db *fakeDB
}

func (f fakeFlow) InsertTx(_ context.Context, _ sqlx.Session, fl *model.Flow) (int64, error) {
	if f.db.flowErr != nil {
		return 0, f.db.flowErr
	}
	if f.db.flowDup {
		f.db.flowDup = false
		return 0, dupErr("uniq_request_id", fl.RequestId)
	}
	if f.db.flowSteal != nil {
		steal := f.db.flowSteal
		f.db.flowSteal = nil
		f.db.afterRollback = func(db *fakeDB) { steal(db) }
		return 0, dupErr("uniq_request_id", fl.RequestId)
	}
	for _, ex := range f.db.flows {
		if ex.RequestId == fl.RequestId {
			return 0, dupErr("uniq_request_id", fl.RequestId)
		}
		if ex.BizType == fl.BizType && ex.BizNo == fl.BizNo {
			return 0, dupErr("uniq_biz_type_biz_no", fl.BizNo)
		}
	}
	f.db.nextFlow++
	stored := *fl
	stored.FlowId = f.db.nextFlow
	if stored.Ctime == 0 {
		stored.Ctime = fakeNow()
	}
	f.db.flows = append(f.db.flows, &stored)
	return f.db.nextFlow, nil
}

func (f fakeFlow) FindByRequestID(_ context.Context, requestID string) (*model.Flow, error) {
	for _, fl := range f.db.flows {
		if fl.RequestId == requestID {
			c := *fl
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakeFlow) FindByBizNo(_ context.Context, bizType int32, bizNo string) (*model.Flow, error) {
	for _, fl := range f.db.flows {
		if fl.BizType == bizType && fl.BizNo == bizNo {
			c := *fl
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakeFlow) List(_ context.Context, q model.FlowListQuery) ([]*model.Flow, error) {
	var all []*model.Flow
	for _, fl := range f.db.flows {
		if !flowMatches(fl, q) {
			continue
		}
		c := *fl
		all = append(all, &c)
	}
	return paginate(all, q.Page, func(fl *model.Flow) int64 { return fl.FlowId }), nil
}

func (f fakeFlow) Count(_ context.Context, q model.FlowListQuery) (int64, error) {
	var n int64
	for _, fl := range f.db.flows {
		if flowMatches(fl, q) {
			n++
		}
	}
	return n, nil
}

func flowMatches(fl *model.Flow, q model.FlowListQuery) bool {
	if q.Mid != 0 && fl.Mid != q.Mid {
		return false
	}
	if q.BizType != 0 && fl.BizType != q.BizType {
		return false
	}
	if q.BizNo != "" && fl.BizNo != q.BizNo {
		return false
	}
	return true
}

// paginate 按 id 降序（与生产的 ORDER BY ctime DESC, id DESC 同序，测试内同秒）取一页。
func paginate[T any](rows []T, page model.ListPage, id func(T) int64) []T {
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if id(rows[j]) > id(rows[i]) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if page.Offset >= int64(len(rows)) {
		return []T{}
	}
	end := page.Offset + page.Limit
	if end > int64(len(rows)) {
		end = int64(len(rows))
	}
	return rows[page.Offset:end]
}

// --- 种子数据 ---

func (db *fakeDB) seedWallet(mid int64, balance int64, currency string) *model.Wallet {
	now := fakeNow()
	db.nextWallet++
	w := &model.Wallet{
		Id: db.nextWallet, Mid: mid, BalanceMinor: balance, Currency: currency,
		Ctime: now, Mtime: now,
	}
	db.wallets[mid] = w
	return w
}

func (db *fakeDB) seedRecharge(r *model.Recharge) *model.Recharge {
	if r.Id == 0 {
		db.nextRecharge++
		r.Id = db.nextRecharge
	}
	if r.Ctime == 0 {
		r.Ctime = fakeNow()
	}
	r.Mtime = r.Ctime
	db.recharges[r.RechargeNo] = r
	return r
}

func (db *fakeDB) seedPayment(p *model.Payment) *model.Payment {
	if p.Id == 0 {
		db.nextPayment++
		p.Id = db.nextPayment
	}
	if p.Ctime == 0 {
		p.Ctime = fakeNow()
	}
	p.Mtime = p.Ctime
	db.payments[p.PaymentNo] = p
	return p
}

func (db *fakeDB) seedRefund(r *model.Refund) *model.Refund {
	if r.Id == 0 {
		db.nextRefund++
		r.Id = db.nextRefund
	}
	if r.Ctime == 0 {
		r.Ctime = fakeNow()
	}
	db.refunds[r.RefundNo] = r
	return r
}

func (db *fakeDB) seedFlow(fl *model.Flow) *model.Flow {
	db.nextFlow++
	fl.FlowId = db.nextFlow
	if fl.Ctime == 0 {
		fl.Ctime = fakeNow()
	}
	db.flows = append(db.flows, fl)
	return fl
}

// --- 断言助手 ---

// requireStatus 断言错误是期望的 gRPC code 且消息含 wantMsg。
// 哨兵错误带完整契约语义，ErrDetail 会在其后追加上下文，所以按子串判定。
func requireStatus(t *testing.T, err error, want codes.Code, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 code=%s 的错误，实际返回成功", want)
	}
	if got := status.Code(err); got != want {
		t.Fatalf("gRPC code = %s，期望 %s（err=%v）", got, want, err)
	}
	if wantMsg != "" && !strings.Contains(err.Error(), wantMsg) {
		t.Fatalf("错误消息 = %q，期望包含 %q", err.Error(), wantMsg)
	}
}

func requireSentinel(t *testing.T, err error, sentinel error, want codes.Code) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误 = %v，期望哨兵 %v", err, sentinel)
	}
	if got := status.Code(err); got != want {
		t.Fatalf("gRPC code = %s，期望 %s", got, want)
	}
}

// requireBalance 断言内存台账里的余额；账户不存在时按 0 判定前先要求它存在。
func requireBalance(t *testing.T, db *fakeDB, mid, want int64) {
	t.Helper()
	w, ok := db.wallets[mid]
	if !ok {
		if want == 0 {
			return
		}
		t.Fatalf("mid=%d 没有账户，期望余额 %d", mid, want)
	}
	if w.BalanceMinor != want {
		t.Fatalf("mid=%d 余额 = %d，期望 %d", mid, w.BalanceMinor, want)
	}
}

func requireFlows(t *testing.T, db *fakeDB, want int) {
	t.Helper()
	if len(db.flows) != want {
		description := make([]string, 0, len(db.flows))
		for _, f := range db.flows {
			description = append(description, fmt.Sprintf("type=%d biz_no=%s delta=%d", f.BizType, f.BizNo, f.DeltaMinor))
		}
		t.Fatalf("流水条数 = %d，期望 %d：%v", len(db.flows), want, description)
	}
}
