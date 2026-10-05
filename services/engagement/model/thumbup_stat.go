package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ThumbupStat 对象的点赞/点踩计数表。
// (business, origin_id, message_id) 唯一索引。
// like_number/dislike_number 是修正后的展示值；like_change/dislike_change 是修正增量。
type ThumbupStat struct {
	ID            int64  `db:"id"`             // 主键 ID
	Business      string `db:"business"`       // 业务名
	OriginID      int64  `db:"origin_id"`      // 来源 ID
	MessageID     int64  `db:"message_id"`     // 对象 ID
	LikeNumber    int64  `db:"like_number"`    // 点赞数
	DislikeNumber int64  `db:"dislike_number"` // 点踩数
	LikeChange    int64  `db:"like_change"`    // 点赞修正增量
	DislikeChange int64  `db:"dislike_change"` // 点踩修正增量
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// ThumbupStatModel thumbup_stat 表查询与更新接口。
// FindOne/Incr 接受可选事务会话（为空时退化为自动提交）：Like 事务内要先在**同一会话**上
// 回读计数，才能拿到「本次增量之后」的绝对快照放进事件 payload；
// 用独立连接读会在事务提交前看到旧值，快照就会比事件描述的这次互动少一个。
type ThumbupStatModel interface {
	// FindOne 查询单个对象计数；不存在返回 nil。
	FindOne(ctx context.Context, tx sqlx.Session, business string, originID, messageID int64) (*ThumbupStat, error)
	// FindMany 批量查询；缺失对象不在结果中。
	FindMany(ctx context.Context, business string, originID int64, messageIDs []int64) (map[int64]*ThumbupStat, error)
	// Incr 增量更新计数；不存在则 INSERT。
	Incr(ctx context.Context, tx sqlx.Session, business string, originID, messageID int64, likeDelta, dislikeDelta int64) error
	// UpdateChange 运营修正增量。
	UpdateChange(ctx context.Context, business string, originID, messageID int64, likeChange, dislikeChange int64) error
}

type defaultThumbupStatModel struct {
	conn sqlx.SqlConn
}

// NewThumbupStatModel 创建 ThumbupStatModel 实现。
func NewThumbupStatModel(conn sqlx.SqlConn) ThumbupStatModel {
	return &defaultThumbupStatModel{conn: conn}
}

func (m *defaultThumbupStatModel) FindOne(ctx context.Context, tx sqlx.Session, business string, originID, messageID int64) (*ThumbupStat, error) {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	var s ThumbupStat
	query := "SELECT id, business, origin_id, message_id, like_number, dislike_number, like_change, dislike_change, ctime, mtime FROM thumbup_stat WHERE business = ? AND origin_id = ? AND message_id = ?"
	if err := session.QueryRowCtx(ctx, &s, query, business, originID, messageID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("thumbup_stat FindOne: %w", err)
	}
	return &s, nil
}

func (m *defaultThumbupStatModel) FindMany(ctx context.Context, business string, originID int64, messageIDs []int64) (map[int64]*ThumbupStat, error) {
	if len(messageIDs) == 0 {
		return map[int64]*ThumbupStat{}, nil
	}
	// go-zero 的 QueryRowsCtx 会自动展开 IN (?) 中的 slice 参数。
	query := "SELECT id, business, origin_id, message_id, like_number, dislike_number, like_change, dislike_change, ctime, mtime FROM thumbup_stat WHERE business = ? AND origin_id = ? AND message_id IN (?)"
	var rows []*ThumbupStat
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, business, originID, messageIDs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64]*ThumbupStat{}, nil
		}
		return nil, fmt.Errorf("thumbup_stat FindMany: %w", err)
	}
	out := make(map[int64]*ThumbupStat, len(rows))
	for _, r := range rows {
		out[r.MessageID] = r
	}
	return out, nil
}

func (m *defaultThumbupStatModel) Incr(ctx context.Context, tx sqlx.Session, business string, originID, messageID int64, likeDelta, dislikeDelta int64) error {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	now := nowUnix()
	_, err := session.ExecCtx(ctx,
		"INSERT INTO thumbup_stat (business, origin_id, message_id, like_number, dislike_number, like_change, dislike_change, ctime, mtime) VALUES (?, ?, ?, ?, ?, 0, 0, ?, ?) ON DUPLICATE KEY UPDATE like_number = like_number + VALUES(like_number), dislike_number = dislike_number + VALUES(dislike_number), mtime = VALUES(mtime)",
		business, originID, messageID, likeDelta, dislikeDelta, now, now)
	if err != nil {
		return fmt.Errorf("thumbup_stat Incr: %w", err)
	}
	return nil
}

func (m *defaultThumbupStatModel) UpdateChange(ctx context.Context, business string, originID, messageID int64, likeChange, dislikeChange int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE thumbup_stat SET like_change = like_change + ?, dislike_change = dislike_change + ? WHERE business = ? AND origin_id = ? AND message_id = ?",
		likeChange, dislikeChange, business, originID, messageID)
	if err != nil {
		return fmt.Errorf("thumbup_stat UpdateChange: %w", err)
	}
	return nil
}
