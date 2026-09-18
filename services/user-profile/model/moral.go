package model

import (
	"context"
	"database/sql"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UserMoral 对应数据库 user_moral 表，记录用户节操值。
// 移植自参考仓库 user_moral 表，字段与语义保持一致：
// 节操值以 1/100 为单位（70.00 → 7000），初始 7000、上限 10000。
type UserMoral struct {
	// Mid 用户 ID，主键
	Mid int64 `db:"mid"`
	// Moral 当前节操值（1/100 单位）
	Moral int64 `db:"moral"`
	// Added 累计增加值（1/100 单位）
	Added int64 `db:"added"`
	// Deducted 累计扣减值（1/100 单位）
	Deducted int64 `db:"deducted"`
	// LastRecoverDate 上次节操低于基准值（7000）时的恢复时间（Unix 秒）
	LastRecoverDate int64 `db:"last_recover_date"`
}

// UserMoralModel 抽象 user_moral 表的查询接口。
type UserMoralModel interface {
	// FindOne 查询节操值；不存在返回 nil（调用方按 DefaultMoral 处理）。
	FindOne(ctx context.Context, mid int64) (*UserMoral, error)
	// TxFindOne 事务内查询节操值。
	TxFindOne(ctx context.Context, tx sqlx.Session, mid int64) (*UserMoral, error)
	// TxInit 事务内初始化节操记录（INSERT IGNORE）。
	TxInit(ctx context.Context, tx sqlx.Session, mid, moral, added, deducted, lastRecoverDate int64) error
	// TxUpdate 事务内变更节操值（moral=+Δ、added=+Δ加、deducted=+Δ扣）。
	TxUpdate(ctx context.Context, tx sqlx.Session, mid, moral, added, deducted int64) error
	// TxUpdateRecoverDate 事务内更新恢复时间。
	TxUpdateRecoverDate(ctx context.Context, tx sqlx.Session, mid int64, recoverDate int64) error
}

type defaultUserMoralModel struct {
	conn sqlx.SqlConn
}

// NewUserMoralModel 创建基于 sqlx 的 UserMoralModel 实现。
func NewUserMoralModel(conn sqlx.SqlConn) UserMoralModel {
	return &defaultUserMoralModel{conn: conn}
}

func (m *defaultUserMoralModel) FindOne(ctx context.Context, mid int64) (*UserMoral, error) {
	return m.TxFindOne(ctx, m.conn, mid)
}

func (m *defaultUserMoralModel) TxFindOne(ctx context.Context, tx sqlx.Session, mid int64) (*UserMoral, error) {
	var moral UserMoral
	query := `SELECT mid, moral, added, deducted, last_recover_date FROM user_moral WHERE mid = ?`
	if err := tx.QueryRowCtx(ctx, &moral, query, mid); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &moral, nil
}

func (m *defaultUserMoralModel) TxInit(ctx context.Context, tx sqlx.Session, mid, moral, added, deducted, lastRecoverDate int64) error {
	query := `INSERT IGNORE INTO user_moral (mid, moral, added, deducted, last_recover_date) VALUES (?, ?, ?, ?, ?)`
	_, err := tx.ExecCtx(ctx, query, mid, moral, added, deducted, lastRecoverDate)
	return err
}

func (m *defaultUserMoralModel) TxUpdate(ctx context.Context, tx sqlx.Session, mid, moral, added, deducted int64) error {
	query := `UPDATE user_moral SET moral = moral + ?, added = added + ?, deducted = deducted + ? WHERE mid = ?`
	_, err := tx.ExecCtx(ctx, query, moral, added, deducted, mid)
	return err
}

func (m *defaultUserMoralModel) TxUpdateRecoverDate(ctx context.Context, tx sqlx.Session, mid int64, recoverDate int64) error {
	query := `UPDATE user_moral SET last_recover_date = ? WHERE mid = ?`
	_, err := tx.ExecCtx(ctx, query, recoverDate, mid)
	return err
}
