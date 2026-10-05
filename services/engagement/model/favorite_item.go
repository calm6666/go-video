package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// FavoriteItem 收藏项（用户对资源的收藏关系）。
// (mid, oid, tp) 唯一索引保证幂等。
type FavoriteItem struct {
	ID    int64 `db:"id"`    // 主键 ID
	Oid   int64 `db:"oid"`   // 目标 ID
	Mid   int64 `db:"mid"`   // 用户 ID
	Fid   int64 `db:"fid"`   // 收藏夹 ID（0 默认夹）
	Tp    int32 `db:"tp"`    // 收藏类型
	Otype int32 `db:"otype"` // 目标子类型
	State int32 `db:"state"` // 0 正常、1 删除
	Ctime int64 `db:"ctime"` // 收藏时间（Unix 秒）
	Mtime int64 `db:"mtime"` // 修改时间（Unix 秒）
}

// FavoriteItemModel favorite_item 表查询与写入接口。
// Add/Del/IsFavored/CountByOid 接受可选事务会话（为空时退化为自动提交）：
// 收藏写、写前的「原本是否已收藏」判定、以及「收藏数绝对快照」必须同事务同会话，
// 否则事件里的 favorite_count 会漏掉这一次操作，或者重复收藏被当成新收藏发出去。
type FavoriteItemModel interface {
	// Add 新增收藏；幂等（已存在则 state 改回 0）。
	Add(ctx context.Context, tx sqlx.Session, f *FavoriteItem) error
	// Del 删除收藏（软删除 state=1）。
	Del(ctx context.Context, tx sqlx.Session, mid, oid int64, tp int32, fid int64) error
	// IsFavored 查询是否已收藏。
	IsFavored(ctx context.Context, tx sqlx.Session, mid, oid int64, tp int32) (bool, error)
	// IsFavoreds 批量查询是否已收藏。
	IsFavoreds(ctx context.Context, mid int64, oids []int64, tp int32) (map[int64]bool, error)
	// CountByOid 查询对象当前被收藏的次数（只计 state=0 的正常行）。
	CountByOid(ctx context.Context, tx sqlx.Session, oid int64, tp int32) (int64, error)
}

type defaultFavoriteItemModel struct {
	conn sqlx.SqlConn
}

// NewFavoriteItemModel 创建 FavoriteItemModel 实现。
func NewFavoriteItemModel(conn sqlx.SqlConn) FavoriteItemModel {
	return &defaultFavoriteItemModel{conn: conn}
}

func (m *defaultFavoriteItemModel) Add(ctx context.Context, tx sqlx.Session, f *FavoriteItem) error {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	now := nowUnix()
	_, err := session.ExecCtx(ctx,
		"INSERT INTO favorite_item (oid, mid, fid, tp, otype, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, 0, ?, ?) ON DUPLICATE KEY UPDATE state = 0, fid = VALUES(fid), mtime = VALUES(mtime)",
		f.Oid, f.Mid, f.Fid, f.Tp, f.Otype, now, now)
	if err != nil {
		return fmt.Errorf("favorite_item Add: %w", err)
	}
	return nil
}

func (m *defaultFavoriteItemModel) Del(ctx context.Context, tx sqlx.Session, mid, oid int64, tp int32, fid int64) error {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	res, err := session.ExecCtx(ctx,
		"UPDATE favorite_item SET state = 1, mtime = ? WHERE mid = ? AND oid = ? AND tp = ? AND fid = ? AND state = 0",
		nowUnix(), mid, oid, tp, fid)
	if err != nil {
		return fmt.Errorf("favorite_item Del: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("favorite_item Del RowsAffected: %w", err)
	}
	if aff == 0 {
		// 尝试不限定 fid（兼容默认夹）
		res, err = session.ExecCtx(ctx,
			"UPDATE favorite_item SET state = 1, mtime = ? WHERE mid = ? AND oid = ? AND tp = ? AND state = 0",
			nowUnix(), mid, oid, tp)
		if err != nil {
			return fmt.Errorf("favorite_item Del fallback: %w", err)
		}
	}
	return nil
}

func (m *defaultFavoriteItemModel) IsFavored(ctx context.Context, tx sqlx.Session, mid, oid int64, tp int32) (bool, error) {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	var state int32
	err := session.QueryRowCtx(ctx, &state,
		"SELECT state FROM favorite_item WHERE mid = ? AND oid = ? AND tp = ?",
		mid, oid, tp)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("favorite_item IsFavored: %w", err)
	}
	return state == 0, nil
}

func (m *defaultFavoriteItemModel) IsFavoreds(ctx context.Context, mid int64, oids []int64, tp int32) (map[int64]bool, error) {
	if len(oids) == 0 {
		return map[int64]bool{}, nil
	}
	// go-zero 的 QueryRowsCtx 会自动展开 IN (?) 中的 slice 参数。
	query := "SELECT oid, state FROM favorite_item WHERE mid = ? AND tp = ? AND oid IN (?)"
	type row struct {
		Oid   int64 `db:"oid"`
		State int32 `db:"state"`
	}
	var rows []row
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, tp, oids); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64]bool{}, nil
		}
		return nil, fmt.Errorf("favorite_item IsFavoreds: %w", err)
	}
	out := make(map[int64]bool, len(oids))
	for _, r := range rows {
		out[r.Oid] = (r.State == 0)
	}
	return out, nil
}

// CountByOid 走 idx_oid_tp (oid, tp)：这是该索引的第一条真实查询，
// 之前它登记在 migration_parity_test.go 的 deferredIndexes 豁免里，本方法落地后豁免同步删除。
// 带 state = 0 是因为软删行仍留在表里（Del 只置位），不过滤会把已取消的收藏算进快照。
func (m *defaultFavoriteItemModel) CountByOid(ctx context.Context, tx sqlx.Session, oid int64, tp int32) (int64, error) {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	var count int64
	err := session.QueryRowCtx(ctx, &count,
		"SELECT COUNT(*) FROM favorite_item WHERE oid = ? AND tp = ? AND state = 0", oid, tp)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("favorite_item CountByOid: %w", err)
	}
	return count, nil
}
