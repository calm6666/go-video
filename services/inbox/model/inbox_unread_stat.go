package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UnreadStat 未读数快照行（inbox_unread_stat 表投影）。
// 该表是 Redis 与明细表之间的回落层：值始终由 CountUnreadByCategory 重算写入，
// 因此可以随时整表重建，不作为业务真值来源。
type UnreadStat struct {
	Mid      int64 `db:"mid"`      // 用户 mid
	Category int32 `db:"category"` // 分类
	Unread   int64 `db:"unread"`   // 未读数快照
	Mtime    int64 `db:"mtime"`    // 快照更新时间（Unix 秒）
}

// UnreadStatModel inbox_unread_stat 表读写接口。
type UnreadStatModel interface {
	// ReplaceByMid 用重算结果整体替换某用户的分类快照（含 0 值分类）。
	ReplaceByMid(ctx context.Context, session sqlx.Session, mid int64, counts map[int32]int64) error
	// IncrBy 投递路径的增量更新（与收件行插入同事务）；不存在则建行。
	// delta 可为负，结果用 GREATEST 兜底到 0，避免异常写入产生负数快照。
	IncrBy(ctx context.Context, session sqlx.Session, mid int64, category int32, delta int64) error
	// ListByMid 读取某用户的分类快照；无记录返回空切片。
	ListByMid(ctx context.Context, mid int64) ([]UnreadStat, error)
	// DeleteByMid 删除某用户的快照（明细表已无未读时用于回收）。
	DeleteByMid(ctx context.Context, mid int64) error
}

type defaultUnreadStatModel struct {
	conn sqlx.SqlConn
}

// NewUnreadStatModel 创建 UnreadStatModel 实现。
func NewUnreadStatModel(conn sqlx.SqlConn) UnreadStatModel {
	return &defaultUnreadStatModel{conn: conn}
}

func (m *defaultUnreadStatModel) ReplaceByMid(
	ctx context.Context, session sqlx.Session, mid int64, counts map[int32]int64,
) error {
	now := nowUnix()
	stmt := "INSERT INTO inbox_unread_stat (mid, category, unread, mtime) VALUES (?, ?, ?, ?)" +
		" ON DUPLICATE KEY UPDATE unread = VALUES(unread), mtime = VALUES(mtime)"
	for _, category := range AllCategories() {
		// 四个分类全部写入（缺失记 0），保证快照自洽、读取端无需补默认值。
		if _, err := pick(m.conn, session).ExecCtx(ctx, stmt, mid, category, counts[category], now); err != nil {
			return fmt.Errorf("inbox_unread_stat ReplaceByMid category=%d: %w", category, err)
		}
	}
	return nil
}

func (m *defaultUnreadStatModel) IncrBy(
	ctx context.Context, session sqlx.Session, mid int64, category int32, delta int64,
) error {
	if delta == 0 {
		return nil
	}
	now := nowUnix()
	_, err := pick(m.conn, session).ExecCtx(ctx,
		"INSERT INTO inbox_unread_stat (mid, category, unread, mtime) VALUES (?, ?, ?, ?)"+
			" ON DUPLICATE KEY UPDATE unread = GREATEST(unread + ?, 0), mtime = VALUES(mtime)",
		mid, category, max64(delta, 0), now, delta)
	if err != nil {
		return fmt.Errorf("inbox_unread_stat IncrBy: %w", err)
	}
	return nil
}

func (m *defaultUnreadStatModel) ListByMid(ctx context.Context, mid int64) ([]UnreadStat, error) {
	var rows []UnreadStat
	query := "SELECT mid, category, unread, mtime FROM inbox_unread_stat WHERE mid = ? ORDER BY category ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("inbox_unread_stat ListByMid: %w", err)
	}
	return rows, nil
}

func (m *defaultUnreadStatModel) DeleteByMid(ctx context.Context, mid int64) error {
	if _, err := m.conn.ExecCtx(ctx, "DELETE FROM inbox_unread_stat WHERE mid = ?", mid); err != nil {
		return fmt.Errorf("inbox_unread_stat DeleteByMid: %w", err)
	}
	return nil
}

// Snapshot 把明细表的分组计数结果规范化为「四个分类都有值」的快照。
// 未出现的分类补 0，非法分类忽略，保证快照与 rpc 的 by_category 输出一致。
func Snapshot(rows []CategoryCount) map[int32]int64 {
	out := make(map[int32]int64, len(AllCategories()))
	for _, c := range AllCategories() {
		out[c] = 0
	}
	for _, r := range rows {
		if ValidCategory(r.Category) {
			out[r.Category] = r.Unread
		}
	}
	return out
}

// SnapshotTotal 返回快照中各分类未读之和。
func SnapshotTotal(snapshot map[int32]int64) int64 {
	var total int64
	for _, c := range AllCategories() {
		if n, ok := snapshot[c]; ok && n > 0 {
			total += n
		}
	}
	return total
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
