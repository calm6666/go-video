package model

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UpGroupMember 表示 up_special 表作为"分组 → 成员"反向索引时的行。
// 复用 up_special 表，只是查询方向不同：按 group_id 查 mids。
type UpGroupMemberModel interface {
	// FindMidsByGroup 分页查询某分组下的 mid 列表。
	// pn 从 1 开始，ps 为每页大小（调用方限制上限）。
	FindMidsByGroup(ctx context.Context, groupID int64, pn, ps int32) ([]int64, int32, error)
}

type defaultUpGroupMemberModel struct {
	conn sqlx.SqlConn
}

// NewUpGroupMemberModel 创建 UpGroupMemberModel 实现。
func NewUpGroupMemberModel(conn sqlx.SqlConn) UpGroupMemberModel {
	return &defaultUpGroupMemberModel{conn: conn}
}

func (m *defaultUpGroupMemberModel) FindMidsByGroup(ctx context.Context, groupID int64, pn, ps int32) ([]int64, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	countQuery := `SELECT COUNT(*) FROM up_special WHERE group_id = ?`
	if err := m.conn.QueryRowCtx(ctx, &total, countQuery, groupID); err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("up_group_member count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var mids []int64
	query := `SELECT mid FROM up_special WHERE group_id = ? ORDER BY mid LIMIT ? OFFSET ?`
	if err := m.conn.QueryRowsCtx(ctx, &mids, query, groupID, ps, offset); err != nil {
		if err == sql.ErrNoRows {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("up_group_member list: %w", err)
	}
	return mids, total, nil
}
