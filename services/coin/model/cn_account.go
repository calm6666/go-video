package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// accountColumns 与 deploy/migrations/coin/000001_create_coin_tables.sql 的 cn_account 逐列对应。
const accountColumns = "mid, balance, total_tossed, version, ctime, mtime"

// Account 硬币账户行（cn_account 投影，对应 rpc.CoinAccountInfo 的账户部分）。
//
// 本服务是硬币余额的唯一写入口（AGENTS.md §5）：payment 的现金余额在另一套账、
// 另一张库，两边既不互换也不互相换算；mid 直接沿用 account 域的用户主键，
// 不建外键、不复制任何用户资料。
//
// 并发控制口径：balance 的一切变更都必须走「条件 UPDATE + RowsAffected 判定」，
// 禁止「先 SELECT 再无条件写回」——那种写法在并发投币下会丢扣减（凭空多币）。
// version 只是给读侧和排障用的变更计数，不当乐观锁用（写门禁由 WHERE 条件承担）。
type Account struct {
	Mid         int64 `db:"mid"`          // 用户 ID（主键，非自增：由调用方指明归属）
	Balance     int64 `db:"balance"`      // 当前可用硬币，恒 >= 0
	TotalTossed int64 `db:"total_tossed"` // 历史累计投出（取消不回退，见 DeductForTossTx/RefundForCancelTx 注释）
	Version     int64 `db:"version"`      // 余额变更次数（每次 balance 变化 +1）
	Ctime       int64 `db:"ctime"`        // 建仓时间（Unix 秒）
	Mtime       int64 `db:"mtime"`        // 最后一次余额变更时间（Unix 秒）
}

// AccountModel cn_account 表读写接口。
type AccountModel interface {
	// FindOne 读账户；从未建过账户返回 (nil, nil)——这是「查无此账户」而不是失败，
	// GetCoinAccount 据此回 found=false，绝不返回一个凭空造出的 0 余额账户冒充存在。
	FindOne(ctx context.Context, mid int64) (*Account, error)
	// EnsureTx 懒建仓：主键冲突时不做任何修改。
	// initial 是新建时的初始余额（config.Coin.InitialBalance），只在真正新建那一行时生效；
	// 返回 created=true 时调用方必须补写一条发放流水，否则「余额 = 流水之和」的对账不变式会破。
	// session 为 nil 时退化为自动提交。
	EnsureTx(ctx context.Context, session sqlx.Session, mid, initial int64) (created bool, err error)
	// LockForUpdateTx 在事务内取账户行并加行锁，是同一用户所有写操作的串行点：
	// 投币、取消、发放都先锁这里，再依次动 cn_toss / cn_daily_toss / cn_flow，
	// 因此同一 request_id 的并发重试不会双扣。行缺失返回 ErrAccountNotFound。
	LockForUpdateTx(ctx context.Context, session sqlx.Session, mid int64) (*Account, error)
	// DeductForTossTx 投币扣减：balance -= amount 且 total_tossed += amount，
	// 条件是 balance >= atLeast（atLeast = max(amount, Coin.MinBalanceToToss)）。
	// 返回 false 表示余额不足（调用方转 reason=INSUFFICIENT_BALANCE），不是错误。
	DeductForTossTx(ctx context.Context, session sqlx.Session, mid, amount, atLeast int64) (bool, error)
	// RefundForCancelTx 取消投币退回：balance += amount，但 total_tossed 不回退
	// ——proto 把 total_tossed 定义为历史口径（曾经投出去过多少枚），退款只影响余额。
	RefundForCancelTx(ctx context.Context, session sqlx.Session, mid, amount int64) (bool, error)
	// ApplyGrantTx 发放/扣回：balance += delta，条件 balance + delta >= 0，
	// 保证余额不可能为负。返回 false 即「扣回金额超过当前余额」，事务必须整体回滚。
	ApplyGrantTx(ctx context.Context, session sqlx.Session, mid, delta int64) (bool, error)
}

type defaultAccountModel struct {
	conn sqlx.SqlConn
}

// NewAccountModel 构造 cn_account 的 sqlx 实现。
func NewAccountModel(conn sqlx.SqlConn) AccountModel {
	return &defaultAccountModel{conn: conn}
}

func (m *defaultAccountModel) FindOne(ctx context.Context, mid int64) (*Account, error) {
	var row Account
	query := "SELECT " + accountColumns + " FROM cn_account WHERE mid = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cn_account FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultAccountModel) EnsureTx(ctx context.Context, session sqlx.Session, mid, initial int64) (bool, error) {
	if mid <= 0 {
		return false, ErrInvalidMid
	}
	if initial < 0 {
		initial = 0
	}
	now := nowUnix()
	// ON DUPLICATE KEY UPDATE mid = mid 让「已存在」返回 RowsAffected=0，
	// 不依赖驱动专有错误码就能区分新建/已存在（与 audit 的幂等锚点同一手法）。
	// 并发下第二个事务会阻塞到前者提交，因此 created=false 时前者的行必然已可见。
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"INSERT INTO cn_account (mid, balance, total_tossed, version, ctime, mtime)"+
			" VALUES (?, ?, 0, 0, ?, ?) ON DUPLICATE KEY UPDATE mid = mid",
		mid, initial, now, now)
	if err != nil {
		return false, fmt.Errorf("cn_account EnsureTx: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cn_account EnsureTx RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultAccountModel) LockForUpdateTx(ctx context.Context, session sqlx.Session, mid int64) (*Account, error) {
	if session == nil {
		return nil, errors.New("coin: LockForUpdateTx requires a transaction session")
	}
	var row Account
	query := "SELECT " + accountColumns + " FROM cn_account WHERE mid = ? FOR UPDATE"
	if err := session.QueryRowCtx(ctx, &row, query, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("cn_account LockForUpdateTx: %w", err)
	}
	return &row, nil
}

func (m *defaultAccountModel) DeductForTossTx(ctx context.Context, session sqlx.Session, mid, amount, atLeast int64) (bool, error) {
	if amount <= 0 {
		return false, ErrInvalidTossCount
	}
	if atLeast < amount {
		atLeast = amount
	}
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"UPDATE cn_account SET balance = balance - ?, total_tossed = total_tossed + ?,"+
			" version = version + 1, mtime = ? WHERE mid = ? AND balance >= ?",
		amount, amount, nowUnix(), mid, atLeast)
	if err != nil {
		return false, fmt.Errorf("cn_account DeductForTossTx: %w", err)
	}
	return rowsAffectedOne(res, "cn_account DeductForTossTx")
}

func (m *defaultAccountModel) RefundForCancelTx(ctx context.Context, session sqlx.Session, mid, amount int64) (bool, error) {
	if amount <= 0 {
		return false, ErrInvalidTossCount
	}
	// 退回不需要门禁：本行已被 LockForUpdateTx 锁住，amount 来自库里读到的 count，
	// balance 只会变大，不存在穿底风险。amount > 0 保证值必然变化，RowsAffected 恒为 1。
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"UPDATE cn_account SET balance = balance + ?, version = version + 1, mtime = ?"+
			" WHERE mid = ?",
		amount, nowUnix(), mid)
	if err != nil {
		return false, fmt.Errorf("cn_account RefundForCancelTx: %w", err)
	}
	return rowsAffectedOne(res, "cn_account RefundForCancelTx")
}

func (m *defaultAccountModel) ApplyGrantTx(ctx context.Context, session sqlx.Session, mid, delta int64) (bool, error) {
	if delta == 0 {
		return false, ErrGrantDeltaInvalid
	}
	// balance + ? >= 0 这一个条件同时覆盖两件事：扣回不穿底、发放恒非负。
	// delta > 0 时条件必然成立（balance 非负），不需要额外分支。
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"UPDATE cn_account SET balance = balance + ?, version = version + 1, mtime = ?"+
			" WHERE mid = ? AND balance + ? >= 0",
		delta, nowUnix(), mid, delta)
	if err != nil {
		return false, fmt.Errorf("cn_account ApplyGrantTx: %w", err)
	}
	return rowsAffectedOne(res, "cn_account ApplyGrantTx")
}

// executor 让同一份 SQL 既能在事务里跑（session 非 nil）也能自动提交（session 为 nil）：
// go-zero 的 sqlx.SqlConn 本身实现了 Session，所以两者可以统一成一个入口，
// 避免出现「事务版和自动版各写一遍 SQL、改一处漏一处」的双份真相。
func executor(session sqlx.Session, conn sqlx.SqlConn) sqlx.Session {
	if session != nil {
		return session
	}
	return conn
}

// rowsAffectedOne 把 Exec 结果折叠成「条件是否命中」。
// 取不到 RowsAffected 必须报错：把它当 0 会把一次成功的扣减误判成余额不足。
func rowsAffectedOne(res sql.Result, op string) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return n == 1, nil
}
