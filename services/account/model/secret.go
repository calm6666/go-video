package model

// 本文件定义登录密码密钥（account_secret 表）实体与查询。
// 哈希算法与参考仓库 passport 服务一致：MD5(pwd + ">>BiLiSaLt<<" + salt)。

import (
	"context"
	"database/sql"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 密钥类型常量。
const (
	// SecretTypePassword 登录密码。
	SecretTypePassword int8 = 1
)

// 密钥状态常量。
const (
	// SecretStatusActive 当前生效。
	SecretStatusActive int8 = 0
	// SecretStatusHistory 历史（改密后旧行置此状态，供历史密码校验）。
	SecretStatusHistory int8 = 1
)

// AccountSecret 对应数据库 account_secret 表，记录登录密码哈希与历史密码。
type AccountSecret struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// Mid 用户 ID
	Mid int64 `db:"mid"`
	// SecretType 密钥类型：1 登录密码
	SecretType int8 `db:"secret_type"`
	// Salt 随机盐（hex）
	Salt string `db:"salt"`
	// Hash 哈希值 hex
	Hash string `db:"hash"`
	// Status 状态：0 当前生效、1 历史
	Status int8 `db:"status"`
	// CTime 创建时间（Unix 秒）
	CTime int64 `db:"ctime"`
	// MTime 最近更新时间（Unix 秒）
	MTime int64 `db:"mtime"`
}

// AccountSecretModel 抽象 account_secret 表的查询接口。
type AccountSecretModel interface {
	// FindActive 查询当前生效密钥；不存在返回 nil。
	FindActive(ctx context.Context, mid int64, secretType int8) (*AccountSecret, error)
	// FindAll 查询全部密钥（含历史，供历史密码校验）；不存在返回空。
	FindAll(ctx context.Context, mid int64, secretType int8) ([]*AccountSecret, error)
	// Insert 新增密钥行（新密码，status=0；调用方须先事务内把旧行置为历史）。
	Insert(ctx context.Context, tx sqlx.Session, secret *AccountSecret) error
	// MarkHistory 事务内把当前生效行置为历史（改密时调用）。
	MarkHistory(ctx context.Context, tx sqlx.Session, mid int64, secretType int8) error
}

type defaultAccountSecretModel struct {
	conn sqlx.SqlConn
}

// NewAccountSecretModel 创建基于 sqlx 的 AccountSecretModel 实现。
func NewAccountSecretModel(conn sqlx.SqlConn) AccountSecretModel {
	return &defaultAccountSecretModel{conn: conn}
}

func (m *defaultAccountSecretModel) FindActive(ctx context.Context, mid int64, secretType int8) (*AccountSecret, error) {
	var s AccountSecret
	query := `SELECT id, mid, secret_type, salt, hash, status, ctime, mtime FROM account_secret
		WHERE mid = ? AND secret_type = ? AND status = 0 LIMIT 1`
	if err := m.conn.QueryRowCtx(ctx, &s, query, mid, secretType); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (m *defaultAccountSecretModel) FindAll(ctx context.Context, mid int64, secretType int8) ([]*AccountSecret, error) {
	query := `SELECT id, mid, secret_type, salt, hash, status, ctime, mtime FROM account_secret
		WHERE mid = ? AND secret_type = ? ORDER BY id DESC`
	var rows []*AccountSecret
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, secretType); err != nil {
		return nil, err
	}
	return rows, nil
}

func (m *defaultAccountSecretModel) Insert(ctx context.Context, tx sqlx.Session, secret *AccountSecret) error {
	query := `INSERT INTO account_secret (mid, secret_type, salt, hash, status, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?)`
	res, err := tx.ExecCtx(ctx, query, secret.Mid, secret.SecretType, secret.Salt, secret.Hash, secret.Status, secret.CTime, secret.MTime)
	if err != nil {
		return err
	}
	secret.ID, err = res.LastInsertId()
	return err
}

func (m *defaultAccountSecretModel) MarkHistory(ctx context.Context, tx sqlx.Session, mid int64, secretType int8) error {
	query := `UPDATE account_secret SET status = 1, mtime = ? WHERE mid = ? AND secret_type = ? AND status = 0`
	_, err := tx.ExecCtx(ctx, query, time.Now().Unix(), mid, secretType)
	return err
}
