package model

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SignUp 高能联盟签约信息（参考仓库 sign_up 表）。
type SignUp struct {
	Mid       int64 `db:"mid"`        // 签约 UP 主 ID
	State     int32 `db:"state"`      // 签约状态
	BeginDate int64 `db:"begin_date"` // 签约开始时间（Unix 秒）
	EndDate   int64 `db:"end_date"`   // 签约结束时间（Unix 秒）
}

// SignUpModel sign_up 表查询接口。
type SignUpModel interface {
	// FindMany 批量查询签约信息；缺失的 mid 不在结果中。
	FindMany(ctx context.Context, mids []int64) (map[int64]*SignUp, error)
}

type defaultSignUpModel struct {
	conn sqlx.SqlConn
}

// NewSignUpModel 创建 SignUpModel 实现。
func NewSignUpModel(conn sqlx.SqlConn) SignUpModel {
	return &defaultSignUpModel{conn: conn}
}

func (m *defaultSignUpModel) FindMany(ctx context.Context, mids []int64) (map[int64]*SignUp, error) {
	if len(mids) == 0 {
		return map[int64]*SignUp{}, nil
	}
	// go-zero sqlx 自动展开切片到 IN (?) 占位符
	query := `SELECT mid, state, begin_date, end_date FROM sign_up WHERE mid IN (?)`
	var rows []*SignUp
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mids); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64]*SignUp{}, nil
		}
		return nil, err
	}
	out := make(map[int64]*SignUp, len(rows))
	for _, r := range rows {
		out[r.Mid] = r
	}
	return out, nil
}
