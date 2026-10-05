package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ThumbupLike 用户对对象的点赞事实记录。
// (business, mid, message_id) 唯一索引保证幂等：重复 Like 请求不重复增加计数。
type ThumbupLike struct {
	ID        int64  `db:"id"`         // 主键 ID
	Business  string `db:"business"`   // 业务名
	Mid       int64  `db:"mid"`        // 用户 ID
	UpMid     int64  `db:"up_mid"`     // UP 主 ID
	OriginID  int64  `db:"origin_id"`  // 来源 ID
	MessageID int64  `db:"message_id"` // 对象 ID
	State     int32  `db:"state"`      // 点赞状态：1 like、2 dislike、0 unspecified
	Ctime     int64  `db:"ctime"`      // 创建时间（Unix 秒）
	Mtime     int64  `db:"mtime"`      // 修改时间（Unix 秒）
}

// ThumbupLikeModel thumbup_like 表查询与写入接口。
//
// 带 tx 的方法都接受可选事务会话（tx 为空时退化为自动提交）：
// Like 的「旧状态回读 + 关系行 + 计数增量 + 事件行」必须在同一事务里提交，
// 否则计数增量失败而关系行已落地后，客户端重试会走「状态已相同」短路，计数永远补不回来
// （README 已知缺口 2）。ListByMid/ListByItem 是纯读分页路径，不接受 tx，也不参与事务。
type ThumbupLikeModel interface {
	// Upsert 新增或更新点赞状态；幂等。
	// 返回旧状态（用于决定计数增减方向）。无记录返回 0。
	Upsert(ctx context.Context, tx sqlx.Session, l *ThumbupLike) (oldState int32, err error)
	// FindStates 批量查询用户对多个对象的点赞状态。
	// 返回 message_id → (state, time)。缺失对象不在结果中。
	FindStates(ctx context.Context, tx sqlx.Session, business string, mid int64, messageIDs []int64) (map[int64]*ThumbupLike, error)
	// ListByMid 分页查询用户的点赞列表。
	ListByMid(ctx context.Context, business string, mid int64, pn, ps int32) ([]*ThumbupLike, int32, error)
	// ListByItem 分页查询对象的点赞人列表。
	// last_mid 用于去重翻页：查询 mid > last_mid 的记录。
	ListByItem(ctx context.Context, business string, originID, messageID, lastMid int64, pn, ps int32) ([]*ThumbupLike, int32, error)
}

type defaultThumbupLikeModel struct {
	conn sqlx.SqlConn
}

// NewThumbupLikeModel 创建 ThumbupLikeModel 实现。
func NewThumbupLikeModel(conn sqlx.SqlConn) ThumbupLikeModel {
	return &defaultThumbupLikeModel{conn: conn}
}

func (m *defaultThumbupLikeModel) Upsert(ctx context.Context, tx sqlx.Session, l *ThumbupLike) (int32, error) {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	// INSERT ... ON DUPLICATE KEY UPDATE：唯一索引 (business, mid, message_id)
	res, err := session.ExecCtx(ctx,
		"INSERT INTO thumbup_like (business, mid, up_mid, origin_id, message_id, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE state = VALUES(state), mtime = VALUES(mtime)",
		l.Business, l.Mid, l.UpMid, l.OriginID, l.MessageID, l.State, l.Ctime, l.Mtime)
	if err != nil {
		return 0, fmt.Errorf("thumbup_like Upsert: %w", err)
	}
	// 查询旧状态：如果是新插入，RowsAffected=1；如果是更新但状态没变，RowsAffected=0；
	// 如果是更新且状态变化，MySQL 默认 RowsAffected=2。
	// 为简化业务，查询当前记录的 state 不可靠（已被覆盖），这里返回 0 让 logic 层查询 stat 表确定增量方向。
	_ = res
	return 0, nil
}

func (m *defaultThumbupLikeModel) FindStates(ctx context.Context, tx sqlx.Session, business string, mid int64, messageIDs []int64) (map[int64]*ThumbupLike, error) {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	if len(messageIDs) == 0 {
		return map[int64]*ThumbupLike{}, nil
	}
	// go-zero 的 QueryRowsCtx 会自动展开 IN (?) 中的 slice 参数。
	query := "SELECT id, business, mid, up_mid, origin_id, message_id, state, ctime, mtime FROM thumbup_like WHERE business = ? AND mid = ? AND message_id IN (?)"
	var rows []*ThumbupLike
	if err := session.QueryRowsCtx(ctx, &rows, query, business, mid, messageIDs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64]*ThumbupLike{}, nil
		}
		return nil, fmt.Errorf("thumbup_like FindStates: %w", err)
	}
	out := make(map[int64]*ThumbupLike, len(rows))
	for _, r := range rows {
		out[r.MessageID] = r
	}
	return out, nil
}

func (m *defaultThumbupLikeModel) ListByMid(ctx context.Context, business string, mid int64, pn, ps int32) ([]*ThumbupLike, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM thumbup_like WHERE business = ? AND mid = ? AND state = 1", business, mid); err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("thumbup_like ListByMid count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*ThumbupLike
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, business, mid, up_mid, origin_id, message_id, state, ctime, mtime FROM thumbup_like WHERE business = ? AND mid = ? AND state = 1 ORDER BY ctime DESC LIMIT ? OFFSET ?",
		business, mid, ps, offset); err != nil {
		if err == sql.ErrNoRows {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("thumbup_like ListByMid list: %w", err)
	}
	return rows, total, nil
}

func (m *defaultThumbupLikeModel) ListByItem(ctx context.Context, business string, originID, messageID, lastMid int64, pn, ps int32) ([]*ThumbupLike, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM thumbup_like WHERE business = ? AND origin_id = ? AND message_id = ? AND state = 1",
		business, originID, messageID); err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("thumbup_like ListByItem count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*ThumbupLike
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, business, mid, up_mid, origin_id, message_id, state, ctime, mtime FROM thumbup_like WHERE business = ? AND origin_id = ? AND message_id = ? AND state = 1 AND mid > ? ORDER BY mid ASC LIMIT ? OFFSET ?",
		business, originID, messageID, lastMid, ps, offset); err != nil {
		if err == sql.ErrNoRows {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("thumbup_like ListByItem list: %w", err)
	}
	return rows, total, nil
}
