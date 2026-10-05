package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RelationFollow 用户关注关系事实记录。
// (mid, follower_mid) 唯一索引保证幂等：重复关注不重复增加计数。
// 注意语义：mid 是关注发起方，follower_mid 是被关注者。
type RelationFollow struct {
	ID          int64 `db:"id"`           // 主键 ID
	Mid         int64 `db:"mid"`          // 关注发起方用户 ID
	FollowerMid int64 `db:"follower_mid"` // 被关注者用户 ID
	Attr        int32 `db:"attr"`         // 关系属性位（保留）
	State       int32 `db:"state"`        // 0 正常、1 已取关（软删除）
	Ctime       int64 `db:"ctime"`        // 关注时间（Unix 秒）
	Mtime       int64 `db:"mtime"`        // 修改时间（Unix 秒）
}

// RelationFollowModel relation_follow 表查询与写入接口。
type RelationFollowModel interface {
	// Upsert 关注/取关（软删除恢复）；幂等。
	// 返回 oldState：原记录状态（用于决定计数增量方向）。无记录返回 -1。
	Upsert(ctx context.Context, f *RelationFollow, newState int32) (oldState int32, err error)
	// Delete 取关（软删除 state=1）；幂等（已取关不报错）。
	// 返回 oldState：原记录状态（1=已取关，0=正常关注），-1 表示无记录。
	Delete(ctx context.Context, mid, followerMid int64) (oldState int32, err error)
	// FindOne 查询单条关注关系，仅返回 state=0 的记录。
	FindOne(ctx context.Context, mid, followerMid int64) (*RelationFollow, error)
	// FindFollowings 批量查询 mid 是否关注 owners 中的每一个。
	// 返回 owner → 是否关注。
	FindFollowings(ctx context.Context, mid int64, owners []int64) (map[int64]bool, error)
	// FindFollowers 反方向批量查询：mids 中有哪些关注 owner。
	// 返回 mid → 是否关注 owner。与 FindFollowings 是同一张表的两条不同 WHERE，
	// 不能用一次查询代替（正向按 mid 定位、反向按 follower_mid 定位）。
	FindFollowers(ctx context.Context, owner int64, mids []int64) (map[int64]bool, error)
	// ListFollowings 分页查询 mid 的关注列表。
	ListFollowings(ctx context.Context, mid int64, pn, ps int32) ([]*RelationFollow, int32, error)
	// ListFollowers 分页查询 mid 的粉丝列表（即 follower_mid=mid 的关注记录）。
	ListFollowers(ctx context.Context, mid int64, pn, ps int32) ([]*RelationFollow, int32, error)
	// DeleteByMid 删除 mid 发起的全部关注记录（拉黑场景下批量取关）。
	DeleteByMid(ctx context.Context, mid, followerMid int64) error
}

type defaultRelationFollowModel struct {
	conn sqlx.SqlConn
}

// NewRelationFollowModel 创建 RelationFollowModel 实现。
func NewRelationFollowModel(conn sqlx.SqlConn) RelationFollowModel {
	return &defaultRelationFollowModel{conn: conn}
}

func (m *defaultRelationFollowModel) Upsert(ctx context.Context, f *RelationFollow, newState int32) (int32, error) {
	// 先查旧状态，Upsert 之后会被新值覆盖，无法可靠获取。
	var oldState int32 = -1
	var cur int32
	err := m.conn.QueryRowCtx(ctx, &cur,
		"SELECT state FROM relation_follow WHERE mid = ? AND follower_mid = ?",
		f.Mid, f.FollowerMid)
	if err == nil {
		oldState = cur
	} else if !errors.Is(err, sql.ErrNoRows) {
		return -1, fmt.Errorf("relation_follow Upsert select: %w", err)
	}

	// 幂等：状态相同则不再 UPDATE，避免 mtime 抖动。
	if oldState == newState {
		return oldState, nil
	}

	now := nowUnix()
	if _, err := m.conn.ExecCtx(ctx,
		"INSERT INTO relation_follow (mid, follower_mid, attr, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE state = VALUES(state), mtime = VALUES(mtime)",
		f.Mid, f.FollowerMid, f.Attr, newState, now, now); err != nil {
		return -1, fmt.Errorf("relation_follow Upsert: %w", err)
	}
	return oldState, nil
}

func (m *defaultRelationFollowModel) Delete(ctx context.Context, mid, followerMid int64) (int32, error) {
	var cur int32
	err := m.conn.QueryRowCtx(ctx, &cur,
		"SELECT state FROM relation_follow WHERE mid = ? AND follower_mid = ?",
		mid, followerMid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -1, nil
		}
		return -1, fmt.Errorf("relation_follow Delete select: %w", err)
	}
	if cur == FollowStateDeleted {
		return cur, nil
	}
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE relation_follow SET state = ?, mtime = ? WHERE mid = ? AND follower_mid = ? AND state = ?",
		FollowStateDeleted, nowUnix(), mid, followerMid, FollowStateNormal); err != nil {
		return -1, fmt.Errorf("relation_follow Delete: %w", err)
	}
	return cur, nil
}

func (m *defaultRelationFollowModel) FindOne(ctx context.Context, mid, followerMid int64) (*RelationFollow, error) {
	var f RelationFollow
	query := "SELECT id, mid, follower_mid, attr, state, ctime, mtime FROM relation_follow WHERE mid = ? AND follower_mid = ? AND state = ?"
	if err := m.conn.QueryRowCtx(ctx, &f, query, mid, followerMid, FollowStateNormal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("relation_follow FindOne: %w", err)
	}
	return &f, nil
}

func (m *defaultRelationFollowModel) FindFollowings(ctx context.Context, mid int64, owners []int64) (map[int64]bool, error) {
	if len(owners) == 0 {
		return map[int64]bool{}, nil
	}
	type row struct {
		FollowerMid int64 `db:"follower_mid"`
	}
	var rows []row
	query := "SELECT follower_mid FROM relation_follow WHERE mid = ? AND state = ? AND follower_mid IN (?)"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, FollowStateNormal, owners); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64]bool{}, nil
		}
		return nil, fmt.Errorf("relation_follow FindFollowings: %w", err)
	}
	out := make(map[int64]bool, len(owners))
	for _, o := range owners {
		out[o] = false
	}
	for _, r := range rows {
		out[r.FollowerMid] = true
	}
	return out, nil
}

// FindFollowers 复刻 WHERE follower_mid=? AND state=? AND mid IN (?)。
// 走迁移 SQL 的 idx_follower_state_ctime (follower_mid, state, ctime) 前缀，
// 与 FindFollowings（走 uniq_mid_follower/idx_mid_state_ctime）是两条不同的索引路径。
func (m *defaultRelationFollowModel) FindFollowers(ctx context.Context, owner int64, mids []int64) (map[int64]bool, error) {
	if len(mids) == 0 {
		return map[int64]bool{}, nil
	}
	type row struct {
		Mid int64 `db:"mid"`
	}
	var rows []row
	query := "SELECT mid FROM relation_follow WHERE follower_mid = ? AND state = ? AND mid IN (?)"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, owner, FollowStateNormal, mids); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64]bool{}, nil
		}
		return nil, fmt.Errorf("relation_follow FindFollowers: %w", err)
	}
	out := make(map[int64]bool, len(mids))
	for _, mid := range mids {
		out[mid] = false
	}
	for _, r := range rows {
		out[r.Mid] = true
	}
	return out, nil
}

func (m *defaultRelationFollowModel) ListFollowings(ctx context.Context, mid int64, pn, ps int32) ([]*RelationFollow, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM relation_follow WHERE mid = ? AND state = ?", mid, FollowStateNormal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("relation_follow ListFollowings count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*RelationFollow
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, mid, follower_mid, attr, state, ctime, mtime FROM relation_follow WHERE mid = ? AND state = ? ORDER BY ctime DESC LIMIT ? OFFSET ?",
		mid, FollowStateNormal, ps, offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("relation_follow ListFollowings list: %w", err)
	}
	return rows, total, nil
}

func (m *defaultRelationFollowModel) ListFollowers(ctx context.Context, mid int64, pn, ps int32) ([]*RelationFollow, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM relation_follow WHERE follower_mid = ? AND state = ?", mid, FollowStateNormal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("relation_follow ListFollowers count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*RelationFollow
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, mid, follower_mid, attr, state, ctime, mtime FROM relation_follow WHERE follower_mid = ? AND state = ? ORDER BY ctime DESC LIMIT ? OFFSET ?",
		mid, FollowStateNormal, ps, offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("relation_follow ListFollowers list: %w", err)
	}
	return rows, total, nil
}

func (m *defaultRelationFollowModel) DeleteByMid(ctx context.Context, mid, followerMid int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE relation_follow SET state = ?, mtime = ? WHERE mid = ? AND follower_mid = ? AND state = ?",
		FollowStateDeleted, nowUnix(), mid, followerMid, FollowStateNormal); err != nil {
		return fmt.Errorf("relation_follow DeleteByMid: %w", err)
	}
	return nil
}
