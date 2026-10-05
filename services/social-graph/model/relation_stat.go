package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RelationStat 关系计数快照表，每用户一行。
// 计数异步聚合：写入关系时由 repository 同步 Redis 计数器，定时任务回刷本表。
type RelationStat struct {
	ID        int64 `db:"id"`        // 主键 ID
	Mid       int64 `db:"mid"`       // 用户 ID
	Following int64 `db:"following"` // 关注数
	Follower  int64 `db:"follower"`  // 粉丝数
	Whisper   int64 `db:"whisper"`   // 悄悄关注数（保留，本期固定 0）
	Ctime     int64 `db:"ctime"`     // 创建时间（Unix 秒）
	Mtime     int64 `db:"mtime"`     // 修改时间（Unix 秒）
}

// RelationStatModel relation_stat 表查询与写入接口。
type RelationStatModel interface {
	// Find 查询用户计数；不存在返回 nil（调用方降级到 Redis 计数器）。
	Find(ctx context.Context, mid int64) (*RelationStat, error)
	// Incr 增减计数（delta 可为负），不存在则先初始化为 0 再增减。
	Incr(ctx context.Context, mid int64, followingDelta, followerDelta int64) error
}

type defaultRelationStatModel struct {
	conn sqlx.SqlConn
}

// NewRelationStatModel 创建 RelationStatModel 实现。
func NewRelationStatModel(conn sqlx.SqlConn) RelationStatModel {
	return &defaultRelationStatModel{conn: conn}
}

func (m *defaultRelationStatModel) Find(ctx context.Context, mid int64) (*RelationStat, error) {
	var s RelationStat
	query := "SELECT id, mid, following, follower, whisper, ctime, mtime FROM relation_stat WHERE mid = ?"
	if err := m.conn.QueryRowCtx(ctx, &s, query, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("relation_stat Find: %w", err)
	}
	return &s, nil
}

func (m *defaultRelationStatModel) Incr(ctx context.Context, mid int64, followingDelta, followerDelta int64) error {
	now := nowUnix()
	// 先尝试 UPDATE；行不存在时 INSERT。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE relation_stat SET following = following + ?, follower = follower + ?, mtime = ? WHERE mid = ?",
		followingDelta, followerDelta, now, mid)
	if err != nil {
		return fmt.Errorf("relation_stat Incr update: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("relation_stat Incr RowsAffected: %w", err)
	}
	if aff == 0 {
		// 初始化：将 delta 作为初始值（取绝对值确保非负）。
		// 调用方一般先 +1/-1，初始化时取 max(delta, 0) 防止负数。
		following := followingDelta
		if following < 0 {
			following = 0
		}
		follower := followerDelta
		if follower < 0 {
			follower = 0
		}
		if _, err := m.conn.ExecCtx(ctx,
			"INSERT INTO relation_stat (mid, following, follower, whisper, ctime, mtime) VALUES (?, ?, ?, 0, ?, ?) ON DUPLICATE KEY UPDATE following = following + ?, follower = follower + ?, mtime = VALUES(mtime)",
			mid, following, follower, now, now, followingDelta, followerDelta); err != nil {
			return fmt.Errorf("relation_stat Incr insert: %w", err)
		}
	}
	return nil
}
