// Package model 定义 account 服务的数据库实体和查询。
// 本包只持有 account 自有的表，不复制 user-profile、social-graph 等其他服务的数据。
package model

import (
	"context"
	"database/sql"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Account 对应数据库 account 表，记录账号主信息。
type Account struct {
	// Mid 用户 ID，主键，由 idgen 全局生成
	Mid int64 `db:"mid"`
	// Status 账号状态：0 正常、1 封禁、2 注销中
	Status int32 `db:"status"`
	// IsTourist 是否游客账号：0 否、1 是
	IsTourist int32 `db:"is_tourist"`
	// CreatedAt 注册时间（Unix 秒）
	CreatedAt int64 `db:"created_at"`
	// UpdatedAt 最近更新时间（Unix 秒）
	UpdatedAt int64 `db:"updated_at"`
	// RegIP 注册时的客户端 IP
	RegIP string `db:"reg_ip"`
}

// AccountModel 抽象 account 表的查询接口，便于测试替换。
type AccountModel interface {
	// FindOne 根据 mid 查询账号主表。
	FindOne(ctx context.Context, mid int64) (*Account, error)
	// FindMany 批量查询账号主表，按 mid 顺序返回，缺失的 mid 不在结果中。
	FindMany(ctx context.Context, mids []int64) (map[int64]*Account, error)
	// Insert 新建账号记录；mid 必须由调用方通过 idgen 预先生成。
	Insert(ctx context.Context, data *Account) error
	// UpdateStatus 更新账号状态。
	UpdateStatus(ctx context.Context, mid int64, status int32) error
}

type defaultAccountModel struct {
	conn sqlx.SqlConn
}

// NewAccountModel 创建基于 sqlx 的 AccountModel 实现。
func NewAccountModel(conn sqlx.SqlConn) AccountModel {
	return &defaultAccountModel{conn: conn}
}

func (m *defaultAccountModel) FindOne(ctx context.Context, mid int64) (*Account, error) {
	var acc Account
	query := `SELECT mid, status, is_tourist, created_at, updated_at, reg_ip FROM account WHERE mid = ?`
	if err := m.conn.QueryRowCtx(ctx, &acc, query, mid); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &acc, nil
}

func (m *defaultAccountModel) FindMany(ctx context.Context, mids []int64) (map[int64]*Account, error) {
	if len(mids) == 0 {
		return map[int64]*Account{}, nil
	}
	query := `SELECT mid, status, is_tourist, created_at, updated_at, reg_ip FROM account WHERE mid IN (?)`
	var rows []*Account
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mids); err != nil {
		return nil, err
	}
	result := make(map[int64]*Account, len(rows))
	for _, r := range rows {
		result[r.Mid] = r
	}
	return result, nil
}

func (m *defaultAccountModel) Insert(ctx context.Context, data *Account) error {
	now := time.Now().Unix()
	if data.CreatedAt == 0 {
		data.CreatedAt = now
	}
	data.UpdatedAt = now
	query := `INSERT INTO account (mid, status, is_tourist, created_at, updated_at, reg_ip) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := m.conn.ExecCtx(ctx, query, data.Mid, data.Status, data.IsTourist, data.CreatedAt, data.UpdatedAt, data.RegIP)
	return err
}

func (m *defaultAccountModel) UpdateStatus(ctx context.Context, mid int64, status int32) error {
	query := `UPDATE account SET status = ?, updated_at = ? WHERE mid = ?`
	_, err := m.conn.ExecCtx(ctx, query, status, time.Now().Unix(), mid)
	return err
}
