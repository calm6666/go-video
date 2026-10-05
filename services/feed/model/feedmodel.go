package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// FeedOutbox 个人发件箱：作者发布的动态本体。
// (mid, id) 是粉丝收件箱引用的对象；state 软删除。
type FeedOutbox struct {
	ID        int64  `db:"id"`         // 动态 ID
	Mid       int64  `db:"mid"`        // 发布者 ID
	Oid       int64  `db:"oid"`        // 对象 ID（如视频稿件 ID）
	Otype     int32  `db:"otype"`      // 对象类型：1 UGC 视频、2 PGC 番剧、3 直播、4 专栏
	Action    int32  `db:"action"`     // 动作类型：1 发布、2 转发、3 修改
	Ctime     int64  `db:"ctime"`      // 发布时间（Unix 秒）
	Mtime     int64  `db:"mtime"`      // 修改时间（Unix 秒）
	State     int32  `db:"state"`      // 0 正常、1 已修改、2 已删除
	Title     string `db:"title"`      // 标题
	Cover     string `db:"cover"`      // 封面 URL
	Uri       string `db:"uri"`        // 跳转 URI
	ForwardID int64  `db:"forward_id"` // 转发的源动态 ID（0 表示原创）
}

// FeedOutboxModel feed_outbox 表查询与写入接口。
type FeedOutboxModel interface {
	// Insert 新建动态；返回新动态 ID。
	Insert(ctx context.Context, f *FeedOutbox) (int64, error)
	// FindOne 查询单条动态（含已删除）。
	FindOne(ctx context.Context, id int64) (*FeedOutbox, error)
	// FindMany 按 ID 列表批量查询；缺失项不在结果中。
	FindMany(ctx context.Context, ids []int64) (map[int64]*FeedOutbox, error)
	// SoftDelete 把动态标记为已删除（state=2）。
	SoftDelete(ctx context.Context, id, mid int64) error
}

type defaultFeedOutboxModel struct {
	conn sqlx.SqlConn
}

// NewFeedOutboxModel 创建 FeedOutboxModel 实现。
func NewFeedOutboxModel(conn sqlx.SqlConn) FeedOutboxModel {
	return &defaultFeedOutboxModel{conn: conn}
}

func (m *defaultFeedOutboxModel) Insert(ctx context.Context, f *FeedOutbox) (int64, error) {
	now := nowUnix()
	if f.Ctime == 0 {
		f.Ctime = now
	}
	f.Mtime = now
	if f.State == 0 {
		f.State = FeedStateNormal
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO feed_outbox (mid, oid, otype, action, ctime, mtime, state, title, cover, uri, forward_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		f.Mid, f.Oid, f.Otype, f.Action, f.Ctime, f.Mtime, f.State, f.Title, f.Cover, f.Uri, f.ForwardID)
	if err != nil {
		return 0, fmt.Errorf("feed_outbox Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("feed_outbox Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultFeedOutboxModel) FindOne(ctx context.Context, id int64) (*FeedOutbox, error) {
	var f FeedOutbox
	query := "SELECT id, mid, oid, otype, action, ctime, mtime, state, title, cover, uri, forward_id FROM feed_outbox WHERE id = ?"
	if err := m.conn.QueryRowCtx(ctx, &f, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feed_outbox FindOne: %w", err)
	}
	return &f, nil
}

func (m *defaultFeedOutboxModel) FindMany(ctx context.Context, ids []int64) (map[int64]*FeedOutbox, error) {
	if len(ids) == 0 {
		return map[int64]*FeedOutbox{}, nil
	}
	// go-zero 的 QueryRowsCtx 会自动展开 IN (?) 中的 slice 参数。
	query := "SELECT id, mid, oid, otype, action, ctime, mtime, state, title, cover, uri, forward_id FROM feed_outbox WHERE id IN (?)"
	var rows []*FeedOutbox
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, ids); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64]*FeedOutbox{}, nil
		}
		return nil, fmt.Errorf("feed_outbox FindMany: %w", err)
	}
	out := make(map[int64]*FeedOutbox, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

func (m *defaultFeedOutboxModel) SoftDelete(ctx context.Context, id, mid int64) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feed_outbox SET state = ?, mtime = ? WHERE id = ? AND mid = ?",
		FeedStateDeleted, nowUnix(), id, mid)
	if err != nil {
		return fmt.Errorf("feed_outbox SoftDelete: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("feed_outbox SoftDelete RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrFeedNotFound
	}
	return nil
}

// --- feed_inbox ---

// FeedInbox 关注收件箱投影（持久化用，热读走 Redis ZSet）。
// (mid, feed_id) 唯一索引保证幂等。
type FeedInbox struct {
	ID        int64 `db:"id"`         // 主键 ID
	Mid       int64 `db:"mid"`        // 粉丝（收件人）ID
	FeedID    int64 `db:"feed_id"`    // 动态 ID（对应 feed_outbox.id）
	AuthorMid int64 `db:"author_mid"` // 发布者 ID
	Ctime     int64 `db:"ctime"`      // 收件时间（Unix 秒）
	State     int32 `db:"state"`      // 0 正常、2 已删除
}

// FeedInboxModel feed_inbox 表查询与写入接口。
type FeedInboxModel interface {
	// Add 投递一条动态到指定用户的收件箱；幂等（已存在则改回 state=0）。
	Add(ctx context.Context, in *FeedInbox) error
	// ListByMid 按用户拉取收件箱（ctime 倒序），用于 Redis miss 时回查。
	ListByMid(ctx context.Context, mid int64, cursor int64, limit int32) ([]*FeedInbox, error)
	// DeleteByFeedID 软删除指定动态在所有用户的收件箱投影。
	DeleteByFeedID(ctx context.Context, feedID int64) error
}

type defaultFeedInboxModel struct {
	conn sqlx.SqlConn
}

// NewFeedInboxModel 创建 FeedInboxModel 实现。
func NewFeedInboxModel(conn sqlx.SqlConn) FeedInboxModel {
	return &defaultFeedInboxModel{conn: conn}
}

func (m *defaultFeedInboxModel) Add(ctx context.Context, in *FeedInbox) error {
	if in.Ctime == 0 {
		in.Ctime = nowUnix()
	}
	if in.State == 0 {
		in.State = FeedStateNormal
	}
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO feed_inbox (mid, feed_id, author_mid, ctime, state) VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE state = 0, ctime = VALUES(ctime), author_mid = VALUES(author_mid)",
		in.Mid, in.FeedID, in.AuthorMid, in.Ctime, in.State)
	if err != nil {
		return fmt.Errorf("feed_inbox Add: %w", err)
	}
	return nil
}

func (m *defaultFeedInboxModel) ListByMid(ctx context.Context, mid int64, cursor int64, limit int32) ([]*FeedInbox, error) {
	query := "SELECT id, mid, feed_id, author_mid, ctime, state FROM feed_inbox WHERE mid = ? AND state = 0 AND ctime < ? ORDER BY ctime DESC LIMIT ?"
	args := []interface{}{mid, cursor, limit}
	if cursor == 0 {
		query = "SELECT id, mid, feed_id, author_mid, ctime, state FROM feed_inbox WHERE mid = ? AND state = 0 ORDER BY ctime DESC LIMIT ?"
		args = []interface{}{mid, limit}
	}
	var rows []*FeedInbox
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feed_inbox ListByMid: %w", err)
	}
	return rows, nil
}

func (m *defaultFeedInboxModel) DeleteByFeedID(ctx context.Context, feedID int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE feed_inbox SET state = ? WHERE feed_id = ?",
		FeedStateDeleted, feedID)
	if err != nil {
		return fmt.Errorf("feed_inbox DeleteByFeedID: %w", err)
	}
	return nil
}

// --- feed_pin ---

// FeedPin 用户空间置顶动态。
// (mid, feed_id) 唯一索引。
type FeedPin struct {
	ID     int64 `db:"id"`      // 主键 ID
	Mid    int64 `db:"mid"`     // 用户 ID
	FeedID int64 `db:"feed_id"` // 动态 ID
	Ctime  int64 `db:"ctime"`   // 置顶时间（Unix 秒）
	Mtime  int64 `db:"mtime"`   // 修改时间（Unix 秒）
	State  int32 `db:"state"`   // 0 正常、1 删除
}

// FeedPinModel feed_pin 表查询与写入接口。
type FeedPinModel interface {
	// Add 置顶动态；幂等（已存在则改回 state=0）。
	Add(ctx context.Context, p *FeedPin) error
	// Del 取消置顶（软删除）。
	Del(ctx context.Context, mid, feedID int64) error
	// ListByMid 查询用户置顶动态 ID 列表。
	ListByMid(ctx context.Context, mid int64) ([]int64, error)
}

type defaultFeedPinModel struct {
	conn sqlx.SqlConn
}

// NewFeedPinModel 创建 FeedPinModel 实现。
func NewFeedPinModel(conn sqlx.SqlConn) FeedPinModel {
	return &defaultFeedPinModel{conn: conn}
}

func (m *defaultFeedPinModel) Add(ctx context.Context, p *FeedPin) error {
	now := nowUnix()
	if p.Ctime == 0 {
		p.Ctime = now
	}
	p.Mtime = now
	if p.State == 0 {
		p.State = PinStateNormal
	}
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO feed_pin (mid, feed_id, ctime, mtime, state) VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE state = 0, mtime = VALUES(mtime)",
		p.Mid, p.FeedID, p.Ctime, p.Mtime, p.State)
	if err != nil {
		return fmt.Errorf("feed_pin Add: %w", err)
	}
	return nil
}

func (m *defaultFeedPinModel) Del(ctx context.Context, mid, feedID int64) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feed_pin SET state = ?, mtime = ? WHERE mid = ? AND feed_id = ? AND state = 0",
		PinStateDeleted, nowUnix(), mid, feedID)
	if err != nil {
		return fmt.Errorf("feed_pin Del: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("feed_pin Del RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrPinNotFound
	}
	return nil
}

func (m *defaultFeedPinModel) ListByMid(ctx context.Context, mid int64) ([]int64, error) {
	query := "SELECT feed_id FROM feed_pin WHERE mid = ? AND state = 0 ORDER BY ctime ASC"
	var ids []int64
	if err := m.conn.QueryRowsCtx(ctx, &ids, query, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feed_pin ListByMid: %w", err)
	}
	return ids, nil
}

// --- feed_unread ---

// FeedUnread 用户未读动态计数。
// 主键 mid；Redis 计数器是热读，DB 是持久化。
type FeedUnread struct {
	Mid    int64 `db:"mid"`    // 用户 ID
	Unread int64 `db:"unread"` // 未读数
	Mtime  int64 `db:"mtime"`  // 修改时间（Unix 秒）
}

// FeedUnreadModel feed_unread 表查询与写入接口。
type FeedUnreadModel interface {
	// IncrBy 增量更新未读数；不存在则 INSERT。
	IncrBy(ctx context.Context, mid int64, delta int64) error
	// Get 查询未读数；不存在返回 0。
	Get(ctx context.Context, mid int64) (int64, error)
	// Clear 清零未读数。
	Clear(ctx context.Context, mid int64) error
}

type defaultFeedUnreadModel struct {
	conn sqlx.SqlConn
}

// NewFeedUnreadModel 创建 FeedUnreadModel 实现。
func NewFeedUnreadModel(conn sqlx.SqlConn) FeedUnreadModel {
	return &defaultFeedUnreadModel{conn: conn}
}

func (m *defaultFeedUnreadModel) IncrBy(ctx context.Context, mid int64, delta int64) error {
	now := nowUnix()
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO feed_unread (mid, unread, mtime) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE unread = GREATEST(unread + ?, 0), mtime = VALUES(mtime)",
		mid, delta, now, delta)
	if err != nil {
		return fmt.Errorf("feed_unread IncrBy: %w", err)
	}
	return nil
}

func (m *defaultFeedUnreadModel) Get(ctx context.Context, mid int64) (int64, error) {
	var u FeedUnread
	err := m.conn.QueryRowCtx(ctx, &u,
		"SELECT mid, unread, mtime FROM feed_unread WHERE mid = ?", mid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("feed_unread Get: %w", err)
	}
	return u.Unread, nil
}

func (m *defaultFeedUnreadModel) Clear(ctx context.Context, mid int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO feed_unread (mid, unread, mtime) VALUES (?, 0, ?) ON DUPLICATE KEY UPDATE unread = 0, mtime = VALUES(mtime)",
		mid, nowUnix())
	if err != nil {
		return fmt.Errorf("feed_unread Clear: %w", err)
	}
	return nil
}
