package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// walletColumns 与 deploy/migrations/payment/000001_create_payment_tables.sql
// 的 pm_wallet 定义一一对应。
const walletColumns = "id, mid, balance_minor, frozen_minor, currency, version, ctime, mtime"

// Wallet 余额账户行（现金台账，本服务唯一写入口）。
//
// frozen_minor 是契约预留位（payment.proto 注明本项目无预授权流程），
// 本服务只写 0，任何扣减都只看 balance_minor。
type Wallet struct {
	Id           int64  `db:"id"`
	Mid          int64  `db:"mid"`
	BalanceMinor int64  `db:"balance_minor"`
	FrozenMinor  int64  `db:"frozen_minor"`
	Currency     string `db:"currency"`
	Version      int64  `db:"version"`
	Ctime        int64  `db:"ctime"`
	Mtime        int64  `db:"mtime"`
}

// WalletModel pm_wallet 读写接口。
type WalletModel interface {
	// FindOne 按 mid 读账户；账户不存在返回 (nil, nil)，调用方按 0 余额语义处理，
	// 读取路径不建行（建行只发生在真正要动钱的写入里）。
	FindOne(ctx context.Context, mid int64) (*Wallet, error)
	// FindOneTx 在事务内读账户，用于资金变更后回读余额快照；不存在返回 (nil, nil)。
	FindOneTx(ctx context.Context, session sqlx.Session, mid int64) (*Wallet, error)
	// ApplyDeltaTx 在事务内变更余额并返回变更后的账户。
	//
	// 这里是防超扣的唯一关卡：扣减走条件更新
	// `WHERE mid = ? AND currency = ? AND balance_minor >= ?`，
	// 影响行数为 0 就直接判定余额不足，不做「先查后改」。
	// delta<0 且余额不足返回 ErrInsufficientBalance；
	// 账户币种与请求不一致返回 ErrUnsupportedCurrency。
	ApplyDeltaTx(ctx context.Context, session sqlx.Session, mid int64, currency string, delta int64) (*Wallet, error)
}

type defaultWalletModel struct {
	conn sqlx.SqlConn
}

// NewWalletModel 创建 WalletModel 实现。
func NewWalletModel(conn sqlx.SqlConn) WalletModel {
	return &defaultWalletModel{conn: conn}
}

func (m *defaultWalletModel) FindOne(ctx context.Context, mid int64) (*Wallet, error) {
	return scanWallet(ctx, m.conn, mid)
}

func (m *defaultWalletModel) FindOneTx(ctx context.Context, session sqlx.Session, mid int64) (*Wallet, error) {
	return scanWallet(ctx, pick(m.conn, session), mid)
}

func (m *defaultWalletModel) ApplyDeltaTx(ctx context.Context, session sqlx.Session, mid int64, currency string, delta int64) (*Wallet, error) {
	if mid <= 0 {
		return nil, ErrInvalidMid
	}
	if delta == 0 {
		return nil, ErrAdjustDeltaZero
	}
	e := pick(m.conn, session)
	now := nowUnix()

	// 安全建行：并发/重复都不得报错。ON DUPLICATE KEY UPDATE id=id 是无操作写入，
	// 但 MySQL 会对既有行加排他锁，等价于「取到这只账户的所有权」。
	if _, err := e.ExecCtx(ctx,
		"INSERT INTO pm_wallet (mid, balance_minor, frozen_minor, currency, version, ctime, mtime) "+
			"VALUES (?, 0, 0, ?, 0, ?, ?) ON DUPLICATE KEY UPDATE id = id",
		mid, currency, now, now); err != nil {
		return nil, fmt.Errorf("pm_wallet ensure: %w", err)
	}

	var (
		res sql.Result
		err error
	)
	if delta < 0 {
		// 条件扣减：守卫写在 WHERE 里，影响行数 0 即余额不足。
		res, err = e.ExecCtx(ctx,
			"UPDATE pm_wallet SET balance_minor = balance_minor + ?, version = version + 1, mtime = ? "+
				"WHERE mid = ? AND currency = ? AND balance_minor >= ?",
			delta, now, mid, currency, -delta)
	} else {
		res, err = e.ExecCtx(ctx,
			"UPDATE pm_wallet SET balance_minor = balance_minor + ?, version = version + 1, mtime = ? "+
				"WHERE mid = ? AND currency = ?",
			delta, now, mid, currency)
	}
	if err != nil {
		return nil, fmt.Errorf("pm_wallet apply delta mid=%d: %w", mid, err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("pm_wallet apply delta RowsAffected: %w", err)
	}
	if aff == 0 {
		// 条件没命中：区分「币种不匹配」与「余额不足」，两者都不能当成成功。
		cur, err := scanWallet(ctx, e, mid)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, ErrConcurrentUpdate
		}
		if cur.Currency != currency {
			return nil, ErrUnsupportedCurrency
		}
		return nil, ErrInsufficientBalance
	}

	wallet, err := scanWallet(ctx, e, mid)
	if err != nil {
		return nil, err
	}
	if wallet == nil {
		return nil, ErrConcurrentUpdate
	}
	return wallet, nil
}

// scanWallet 读单只账户；不存在返回 (nil, nil)。
func scanWallet(ctx context.Context, e execer, mid int64) (*Wallet, error) {
	var w Wallet
	query := "SELECT " + walletColumns + " FROM pm_wallet WHERE mid = ? LIMIT 1"
	if err := e.QueryRowCtx(ctx, &w, query, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_wallet FindOne mid=%d: %w", mid, err)
	}
	return &w, nil
}
