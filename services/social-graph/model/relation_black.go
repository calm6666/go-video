package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RelationBlack 用户拉黑关系事实记录。
// (mid, black_mid) 唯一索引保证幂等。
// 语义：mid 拉黑 black_mid（即 black_mid 不在 mid 的可见范围内）。
type RelationBlack struct {
	ID       int64 `db:"id"`        // 主键 ID
	Mid      int64 `db:"mid"`       // 拉黑发起方用户 ID
	BlackMid int64 `db:"black_mid"` // 被拉黑者用户 ID
	State    int32 `db:"state"`     // 0 正常、1 已取消拉黑（软删除）
	Ctime    int64 `db:"ctime"`     // 拉黑时间（Unix 秒）
	Mtime    int64 `db:"mtime"`     // 修改时间（Unix 秒）
}

// RelationBlackModel relation_black 表查询与写入接口。
type RelationBlackModel interface {
	// Upsert 拉黑/恢复；幂等。返回 oldState：无记录返回 -1。
	Upsert(ctx context.Context, b *RelationBlack, newState int32) (oldState int32, err error)
	// Delete 取消拉黑（软删除 state=1）；幂等。
	Delete(ctx context.Context, mid, blackMid int64) (oldState int32, err error)
	// FindOne 查询 mid 是否拉黑 black_mid，仅返回 state=0 的记录。
	FindOne(ctx context.Context, mid, blackMid int64) (*RelationBlack, error)
	// FindBlacks 批量查询 mid 拉黑了 mids 中的哪些。
	// 返回 blackMid → 是否被拉黑；口径与 FindOne 一致（只看 state=0）。
	FindBlacks(ctx context.Context, mid int64, mids []int64) (map[int64]bool, error)
	// ListByMid 分页查询 mid 的黑名单列表。
	ListByMid(ctx context.Context, mid int64, pn, ps int32) ([]*RelationBlack, int32, error)
}

type defaultRelationBlackModel struct {
	conn sqlx.SqlConn
}

// NewRelationBlackModel 创建 RelationBlackModel 实现。
func NewRelationBlackModel(conn sqlx.SqlConn) RelationBlackModel {
	return &defaultRelationBlackModel{conn: conn}
}

func (m *defaultRelationBlackModel) Upsert(ctx context.Context, b *RelationBlack, newState int32) (int32, error) {
	var oldState int32 = -1
	var cur int32
	err := m.conn.QueryRowCtx(ctx, &cur,
		"SELECT state FROM relation_black WHERE mid = ? AND black_mid = ?",
		b.Mid, b.BlackMid)
	if err == nil {
		oldState = cur
	} else if !errors.Is(err, sql.ErrNoRows) {
		return -1, fmt.Errorf("relation_black Upsert select: %w", err)
	}

	if oldState == newState {
		return oldState, nil
	}

	now := nowUnix()
	if _, err := m.conn.ExecCtx(ctx,
		"INSERT INTO relation_black (mid, black_mid, state, ctime, mtime) VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE state = VALUES(state), mtime = VALUES(mtime)",
		b.Mid, b.BlackMid, newState, now, now); err != nil {
		return -1, fmt.Errorf("relation_black Upsert: %w", err)
	}
	return oldState, nil
}

func (m *defaultRelationBlackModel) Delete(ctx context.Context, mid, blackMid int64) (int32, error) {
	var cur int32
	err := m.conn.QueryRowCtx(ctx, &cur,
		"SELECT state FROM relation_black WHERE mid = ? AND black_mid = ?",
		mid, blackMid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -1, nil
		}
		return -1, fmt.Errorf("relation_black Delete select: %w", err)
	}
	if cur == BlackStateDeleted {
		return cur, nil
	}
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE relation_black SET state = ?, mtime = ? WHERE mid = ? AND black_mid = ? AND state = ?",
		BlackStateDeleted, nowUnix(), mid, blackMid, BlackStateNormal); err != nil {
		return -1, fmt.Errorf("relation_black Delete: %w", err)
	}
	return cur, nil
}

func (m *defaultRelationBlackModel) FindOne(ctx context.Context, mid, blackMid int64) (*RelationBlack, error) {
	var b RelationBlack
	query := "SELECT id, mid, black_mid, state, ctime, mtime FROM relation_black WHERE mid = ? AND black_mid = ? AND state = ?"
	if err := m.conn.QueryRowCtx(ctx, &b, query, mid, blackMid, BlackStateNormal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("relation_black FindOne: %w", err)
	}
	return &b, nil
}

// FindBlacks 复刻 WHERE mid=? AND state=? AND black_mid IN (?)，走 uniq_mid_black (mid, black_mid)。
func (m *defaultRelationBlackModel) FindBlacks(ctx context.Context, mid int64, mids []int64) (map[int64]bool, error) {
	if len(mids) == 0 {
		return map[int64]bool{}, nil
	}
	type row struct {
		BlackMid int64 `db:"black_mid"`
	}
	var rows []row
	query := "SELECT black_mid FROM relation_black WHERE mid = ? AND state = ? AND black_mid IN (?)"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, BlackStateNormal, mids); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64]bool{}, nil
		}
		return nil, fmt.Errorf("relation_black FindBlacks: %w", err)
	}
	out := make(map[int64]bool, len(mids))
	for _, blackMid := range mids {
		out[blackMid] = false
	}
	for _, r := range rows {
		out[r.BlackMid] = true
	}
	return out, nil
}

func (m *defaultRelationBlackModel) ListByMid(ctx context.Context, mid int64, pn, ps int32) ([]*RelationBlack, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM relation_black WHERE mid = ? AND state = ?", mid, BlackStateNormal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("relation_black ListByMid count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*RelationBlack
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, mid, black_mid, state, ctime, mtime FROM relation_black WHERE mid = ? AND state = ? ORDER BY ctime DESC LIMIT ? OFFSET ?",
		mid, BlackStateNormal, ps, offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("relation_black ListByMid list: %w", err)
	}
	return rows, total, nil
}
