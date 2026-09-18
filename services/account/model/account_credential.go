package model

import (
	"context"
	"database/sql"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 凭证类型常量，对应 account_credential.credential_type 字段。
const (
	// CredentialTypeUsername 用户名凭证
	CredentialTypeUsername int8 = 1
	// CredentialTypePhone 手机号凭证
	CredentialTypePhone int8 = 2
	// CredentialTypeEmail 邮箱凭证
	CredentialTypeEmail int8 = 3
)

// AccountCredential 对应数据库 account_credential 表，记录用户的登录标识。
// 一个 mid 可以有多条凭证记录（用户名、手机、邮箱各一条）。
type AccountCredential struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// Mid 用户 ID，外键关联 account.mid
	Mid int64 `db:"mid"`
	// CredentialType 凭证类型：1 用户名、2 手机号、3 邮箱
	CredentialType int8 `db:"credential_type"`
	// Identifier 凭证值：用户名/手机号/邮箱
	Identifier string `db:"identifier"`
	// Status 凭证状态：0 正常、1 已解绑
	Status int8 `db:"status"`
	// CreatedAt 创建时间（Unix 秒）
	CreatedAt int64 `db:"created_at"`
	// UpdatedAt 最近更新时间（Unix 秒）
	UpdatedAt int64 `db:"updated_at"`
}

// AccountCredentialModel 抽象 account_credential 表的查询接口。
type AccountCredentialModel interface {
	// FindByMid 查询某 mid 的全部凭证。
	FindByMid(ctx context.Context, mid int64) ([]*AccountCredential, error)
	// FindByMids 批量查询多个 mid 的凭证。
	FindByMids(ctx context.Context, mids []int64) (map[int64][]*AccountCredential, error)
	// FindMidsByIdentifiers 按凭证值列表查询 mid 列表。
	// 仅返回 identifier 命中且 credential_type 匹配的记录。
	FindMidsByIdentifiers(ctx context.Context, credType int8, identifiers []string) (map[string]int64, error)
	// Insert 新增凭证记录。
	Insert(ctx context.Context, data *AccountCredential) error
}

type defaultAccountCredentialModel struct {
	conn sqlx.SqlConn
}

// NewAccountCredentialModel 创建基于 sqlx 的 AccountCredentialModel 实现。
func NewAccountCredentialModel(conn sqlx.SqlConn) AccountCredentialModel {
	return &defaultAccountCredentialModel{conn: conn}
}

func (m *defaultAccountCredentialModel) FindByMid(ctx context.Context, mid int64) ([]*AccountCredential, error) {
	var rows []*AccountCredential
	query := `SELECT id, mid, credential_type, identifier, status, created_at, updated_at FROM account_credential WHERE mid = ?`
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid); err != nil {
		return nil, err
	}
	return rows, nil
}

func (m *defaultAccountCredentialModel) FindByMids(ctx context.Context, mids []int64) (map[int64][]*AccountCredential, error) {
	if len(mids) == 0 {
		return map[int64][]*AccountCredential{}, nil
	}
	var rows []*AccountCredential
	query := `SELECT id, mid, credential_type, identifier, status, created_at, updated_at FROM account_credential WHERE mid IN (?)`
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mids); err != nil {
		return nil, err
	}
	result := make(map[int64][]*AccountCredential, len(mids))
	for _, r := range rows {
		result[r.Mid] = append(result[r.Mid], r)
	}
	return result, nil
}

func (m *defaultAccountCredentialModel) FindMidsByIdentifiers(ctx context.Context, credType int8, identifiers []string) (map[string]int64, error) {
	if len(identifiers) == 0 {
		return map[string]int64{}, nil
	}
	// 去重并转小写，避免大小写导致重复
	seen := make(map[string]struct{}, len(identifiers))
	uniq := make([]string, 0, len(identifiers))
	for _, id := range identifiers {
		lower := strings.ToLower(id)
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		uniq = append(uniq, lower)
	}
	var rows []*AccountCredential
	query := `SELECT id, mid, credential_type, identifier, status, created_at, updated_at FROM account_credential WHERE credential_type = ? AND LOWER(identifier) IN (?)`
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, credType, uniq); err != nil {
		if err == sql.ErrNoRows {
			return map[string]int64{}, nil
		}
		return nil, err
	}
	result := make(map[string]int64, len(rows))
	for _, r := range rows {
		result[strings.ToLower(r.Identifier)] = r.Mid
	}
	return result, nil
}

func (m *defaultAccountCredentialModel) Insert(ctx context.Context, data *AccountCredential) error {
	query := `INSERT INTO account_credential (mid, credential_type, identifier, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := m.conn.ExecCtx(ctx, query, data.Mid, data.CredentialType, data.Identifier, data.Status, data.CreatedAt, data.UpdatedAt)
	return err
}
