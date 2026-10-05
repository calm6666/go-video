package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// InboxUserMessage 收件明细行（inbox_user_message 表投影）。
// uniq(mid, msg_id) 保证同一消息对同一收件人只有一行。
type InboxUserMessage struct {
	ID        int64 `db:"id"`         // 主键 ID（列表游标第 2 列）
	Mid       int64 `db:"mid"`        // 收件人 mid
	MsgID     int64 `db:"msg_id"`     // 消息 ID
	Category  int32 `db:"category"`   // 分类快照（不可变）
	ReadState int32 `db:"read_state"` // 1 未读、2 已读
	DelState  int32 `db:"del_state"`  // 0 正常、1 本人已删除
	Ctime     int64 `db:"ctime"`      // 投递时间（Unix 秒，列表游标第 1 列）
	Mtime     int64 `db:"mtime"`      // 状态变更时间（Unix 秒）
}

// MessageRow 是收件明细与消息主体 JOIN 后的读模型。
type MessageRow struct {
	ID        int64  `db:"id"`
	MsgID     int64  `db:"msg_id"`
	Mid       int64  `db:"mid"`
	Category  int32  `db:"category"`
	MsgType   int32  `db:"msg_type"`
	Title     string `db:"title"`
	Content   string `db:"content"`
	SenderMid int64  `db:"sender_mid"`
	BizType   string `db:"biz_type"`
	BizID     string `db:"biz_id"`
	Extra     string `db:"extra"`
	ReadState int32  `db:"read_state"`
	DelState  int32  `db:"del_state"`
	Ctime     int64  `db:"ctime"`
}

// CategoryCount 单个分类的未读数量。
type CategoryCount struct {
	Category int32 `db:"category"`
	Unread   int64 `db:"unread"`
}

// ListFilter 收件箱列表查询条件。
type ListFilter struct {
	Mid        int64 // 收件人
	Category   int32 // 0 表示不限分类
	UnreadOnly bool  // true 只看未读
	CursorTime int64 // 游标：上一页最后一条的 ctime（0 表示第一页）
	CursorID   int64 // 游标：上一页最后一条的 id（同一秒内稳定排序）
	Limit      int32 // 返回条数上限（调用方传 ps+1 以判断 has_more）
}

// InboxUserMessageModel inbox_user_message 表读写接口。
type InboxUserMessageModel interface {
	// InsertIdempotent 幂等投递一个收件人（uniq(mid,msg_id) 冲突时忽略）。
	// 返回 inserted=false 表示该收件人此前已收到同一消息。
	InsertIdempotent(ctx context.Context, session sqlx.Session, um *InboxUserMessage) (inserted bool, err error)
	// List 按 (ctime, id) 倒序游标分页拉取收件箱。
	List(ctx context.Context, f ListFilter) ([]*MessageRow, error)
	// MarkReadBatch 批量把未读消息置为已读；返回真实发生变更的行数
	// （重复调用返回 0，天然幂等）。
	MarkReadBatch(ctx context.Context, session sqlx.Session, mid int64, msgIDs []int64) (int64, error)
	// MarkAllReadBatch 按分类（0 全部）批量置为已读；返回变更行数。
	MarkAllReadBatch(ctx context.Context, session sqlx.Session, mid int64, category int32) (int64, error)
	// SoftDeleteBatch 用户侧批量软删除；返回真实发生变更的行数。
	SoftDeleteBatch(ctx context.Context, session sqlx.Session, mid int64, msgIDs []int64) (int64, error)
	// CountOwned 统计这些消息中属于该收件人的行数，用于越权与存在性校验。
	CountOwned(ctx context.Context, mid int64, msgIDs []int64) (int64, error)
	// CountUnreadByCategory 从明细表重算各分类未读数（未删除且未读）。
	CountUnreadByCategory(ctx context.Context, mid int64) ([]CategoryCount, error)
}

type defaultInboxUserMessageModel struct {
	conn sqlx.SqlConn
}

// NewInboxUserMessageModel 创建 InboxUserMessageModel 实现。
func NewInboxUserMessageModel(conn sqlx.SqlConn) InboxUserMessageModel {
	return &defaultInboxUserMessageModel{conn: conn}
}

// insertSQL 用 INSERT IGNORE 而不是 ODKU：收件行没有可变字段需要覆盖。
var insertSQL = "INSERT IGNORE INTO inbox_user_message (mid, msg_id, category, read_state, del_state, ctime, mtime)" +
	" VALUES (?, ?, ?, ?, ?, ?, ?)"

func (m *defaultInboxUserMessageModel) InsertIdempotent(
	ctx context.Context, session sqlx.Session, um *InboxUserMessage,
) (bool, error) {
	now := nowUnix()
	if um.Ctime == 0 {
		um.Ctime = now
	}
	um.Mtime = now
	if um.ReadState == 0 {
		um.ReadState = ReadStateUnread
	}
	res, err := pick(m.conn, session).ExecCtx(ctx, insertSQL,
		um.Mid, um.MsgID, um.Category, um.ReadState, um.DelState, um.Ctime, um.Mtime)
	if err != nil {
		return false, fmt.Errorf("inbox_user_message InsertIdempotent: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inbox_user_message InsertIdempotent RowsAffected: %w", err)
	}
	return affected == 1, nil
}

const messageRowColumns = "um.id, um.msg_id, um.mid, um.category, m.msg_type, m.title, m.content, " +
	"m.sender_mid, m.biz_type, m.biz_id, m.extra, um.read_state, um.del_state, um.ctime"

func (m *defaultInboxUserMessageModel) List(ctx context.Context, f ListFilter) ([]*MessageRow, error) {
	var (
		conds []string
		args  []any
	)
	// 只展示未撤回的消息主体；用户侧删除不进列表。
	conds = append(conds, "um.mid = ?", "um.del_state = ?", "m.state = ?")
	args = append(args, f.Mid, DelStateNormal, MessageStateNormal)
	if ValidCategory(f.Category) {
		conds = append(conds, "um.category = ?")
		args = append(args, f.Category)
	}
	if f.UnreadOnly {
		conds = append(conds, "um.read_state = ?")
		args = append(args, ReadStateUnread)
	}
	if f.CursorTime > 0 {
		// 同一秒投递的多条消息用 id 兜底，保证分页不重不漏。
		conds = append(conds, "(um.ctime < ? OR (um.ctime = ? AND um.id < ?))")
		args = append(args, f.CursorTime, f.CursorTime, f.CursorID)
	}
	query := "SELECT " + messageRowColumns +
		" FROM inbox_user_message um JOIN inbox_message m ON m.msg_id = um.msg_id" +
		" WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY um.ctime DESC, um.id DESC LIMIT ?"
	args = append(args, f.Limit)

	var rows []*MessageRow
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("inbox_user_message List: %w", err)
	}
	return rows, nil
}

func (m *defaultInboxUserMessageModel) MarkReadBatch(
	ctx context.Context, session sqlx.Session, mid int64, msgIDs []int64,
) (int64, error) {
	if len(msgIDs) == 0 {
		return 0, nil
	}
	args := idArgs([]any{ReadStateRead, nowUnix(), mid}, msgIDs)
	args = append(args, ReadStateUnread, DelStateNormal)
	query := "UPDATE inbox_user_message SET read_state = ?, mtime = ? WHERE mid = ? AND msg_id IN (" +
		placeholders(len(msgIDs)) + ") AND read_state = ? AND del_state = ?"
	return affected(ctx, pick(m.conn, session), query, args, "inbox_user_message MarkReadBatch")
}

func (m *defaultInboxUserMessageModel) MarkAllReadBatch(
	ctx context.Context, session sqlx.Session, mid int64, category int32,
) (int64, error) {
	query := "UPDATE inbox_user_message SET read_state = ?, mtime = ? WHERE mid = ? AND read_state = ? AND del_state = ?"
	args := []any{ReadStateRead, nowUnix(), mid, ReadStateUnread, DelStateNormal}
	if ValidCategory(category) {
		query = "UPDATE inbox_user_message SET read_state = ?, mtime = ? WHERE mid = ? AND category = ?" +
			" AND read_state = ? AND del_state = ?"
		args = []any{ReadStateRead, nowUnix(), mid, category, ReadStateUnread, DelStateNormal}
	}
	return affected(ctx, pick(m.conn, session), query, args, "inbox_user_message MarkAllReadBatch")
}

func (m *defaultInboxUserMessageModel) SoftDeleteBatch(
	ctx context.Context, session sqlx.Session, mid int64, msgIDs []int64,
) (int64, error) {
	if len(msgIDs) == 0 {
		return 0, nil
	}
	// 删除即视为不再提醒：一并置为已读，未读快照由调用方在同一事务内重算。
	args := idArgs([]any{DelStateDeleted, ReadStateRead, nowUnix(), mid}, msgIDs)
	args = append(args, DelStateNormal)
	query := "UPDATE inbox_user_message SET del_state = ?, read_state = ?, mtime = ?" +
		" WHERE mid = ? AND msg_id IN (" + placeholders(len(msgIDs)) + ") AND del_state = ?"
	return affected(ctx, pick(m.conn, session), query, args, "inbox_user_message SoftDeleteBatch")
}

func (m *defaultInboxUserMessageModel) CountOwned(ctx context.Context, mid int64, msgIDs []int64) (int64, error) {
	if len(msgIDs) == 0 {
		return 0, nil
	}
	args := idArgs([]any{mid}, msgIDs)
	query := "SELECT COUNT(*) FROM inbox_user_message WHERE mid = ? AND msg_id IN (" +
		placeholders(len(msgIDs)) + ")"
	var count int64
	if err := m.conn.QueryRowCtx(ctx, &count, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("inbox_user_message CountOwned: %w", err)
	}
	return count, nil
}

func (m *defaultInboxUserMessageModel) CountUnreadByCategory(ctx context.Context, mid int64) ([]CategoryCount, error) {
	query := "SELECT category, COUNT(*) AS unread FROM inbox_user_message" +
		" WHERE mid = ? AND read_state = ? AND del_state = ? GROUP BY category"
	var rows []CategoryCount
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, ReadStateUnread, DelStateNormal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("inbox_user_message CountUnreadByCategory: %w", err)
	}
	return rows, nil
}

// idArgs 把 []int64 追加成 ? 占位参数序列（Go 不允许把 []int64 直接 append 进 []any）。
func idArgs(args []any, ids []int64) []any {
	for _, id := range ids {
		args = append(args, id)
	}
	return args
}

// placeholders 生成 "?,?,?" 形式的 IN 列表占位符。
// go-zero 的 sqlx 不会展开切片参数，因此按 id 个数显式拼占位符。
func placeholders(n int) string {
	if n <= 0 {
		return "?"
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
	}
	return b.String()
}

// affected 执行写语句并返回受影响行数，统一错误包装。
func affected(ctx context.Context, ex execer, query string, args []any, op string) (int64, error) {
	res, err := ex.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return n, nil
}
