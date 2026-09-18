package model

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UpSpecial 表示某 mid 所属的特殊用户组列表（参考仓库 up_special 表）。
// 一对多关系：一个 mid 可同时归属多个特殊分组。
type UpSpecial struct {
	Mid     int64 `db:"mid"`      // 用户 ID
	GroupID int64 `db:"group_id"` // 分组 ID
}

// UpSpecialModel up_special 表查询接口。
type UpSpecialModel interface {
	// FindOne 查询单个 mid 的所有特殊分组 ID；不存在返回空切片。
	FindOne(ctx context.Context, mid int64) ([]int64, error)
	// FindMany 批量查询；缺失的 mid 对应空切片（不在结果中）。
	FindMany(ctx context.Context, mids []int64) (map[int64][]int64, error)
}

type defaultUpSpecialModel struct {
	conn sqlx.SqlConn
}

// NewUpSpecialModel 创建 UpSpecialModel 实现。
func NewUpSpecialModel(conn sqlx.SqlConn) UpSpecialModel {
	return &defaultUpSpecialModel{conn: conn}
}

func (m *defaultUpSpecialModel) FindOne(ctx context.Context, mid int64) ([]int64, error) {
	var rows []int64
	query := `SELECT group_id FROM up_special WHERE mid = ?`
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

func (m *defaultUpSpecialModel) FindMany(ctx context.Context, mids []int64) (map[int64][]int64, error) {
	if len(mids) == 0 {
		return map[int64][]int64{}, nil
	}
	// go-zero sqlx 自动展开切片到 IN (?) 占位符
	query := `SELECT mid, group_id FROM up_special WHERE mid IN (?)`
	type row struct {
		Mid     int64 `db:"mid"`
		GroupID int64 `db:"group_id"`
	}
	var rows []row
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mids); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64][]int64{}, nil
		}
		return nil, err
	}
	out := make(map[int64][]int64, len(mids))
	for _, r := range rows {
		out[r.Mid] = append(out[r.Mid], r.GroupID)
	}
	return out, nil
}
