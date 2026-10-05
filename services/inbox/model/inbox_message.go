package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// InboxMessage 消息主体行（inbox_message 表投影）。
// 一条消息一行；“谁收到、读没读”属于收件人视角，见 InboxUserMessage。
type InboxMessage struct {
	MsgID          int64  `db:"msg_id"`          // 消息 ID
	Category       int32  `db:"category"`        // 分类 1..4
	MsgType        int32  `db:"msg_type"`        // 载体类型
	Title          string `db:"title"`           // 标题
	Content        string `db:"content"`         // 正文
	SenderMid      int64  `db:"sender_mid"`      // 发送方 mid，0 表示系统
	BizType        string `db:"biz_type"`        // 业务类型
	BizID          string `db:"biz_id"`          // 业务主键
	Extra          string `db:"extra"`           // 扩展 JSON 文本
	State          int32  `db:"state"`           // 0 正常、1 已撤回
	IdempotencyKey string `db:"idempotency_key"` // 幂等键（唯一索引）
	Operator       int64  `db:"operator"`        // 运营管理员 mid（审计）
	Ctime          int64  `db:"ctime"`           // 创建时间（Unix 秒）
	Mtime          int64  `db:"mtime"`           // 修改时间（Unix 秒）
}

// InboxMessageModel inbox_message 表读写接口。
type InboxMessageModel interface {
	// InsertIdempotent 按 idempotency_key 幂等插入消息主体。
	// 命中已存在的键时不写新行，created=false 且返回首次那条的 msg_id。
	// session 非 nil 时在调用方事务内执行。
	InsertIdempotent(ctx context.Context, session sqlx.Session, msg *InboxMessage) (msgID int64, created bool, err error)
	// FindOne 查询消息主体；不存在返回 nil。
	FindOne(ctx context.Context, msgID int64) (*InboxMessage, error)
	// Withdraw 撤回消息主体（state=1，对所有收件人不再展示）。
	Withdraw(ctx context.Context, msgID int64) error
}

// 查询列与插入列分开维护，插入时 msg_id 由自增生成。
const (
	inboxMessageSelectColumns = "msg_id, category, msg_type, title, content, sender_mid, " +
		"biz_type, biz_id, extra, state, idempotency_key, operator, ctime, mtime"
	inboxMessageInsertColumns = "category, msg_type, title, content, sender_mid, " +
		"biz_type, biz_id, extra, state, idempotency_key, operator, ctime, mtime"
)

// insertIdempotentSQL 依赖 MySQL 的 LAST_INSERT_ID(expr) 技巧：重复键时把已存在的
// msg_id 写进会话的 last_insert_id，驱动返回的 LastInsertId() 即首次那条消息的 ID，
// 无需二次查询。affected=1 表示本次真的插入，0 表示命中幂等键。
var insertIdempotentSQL = "INSERT INTO inbox_message (" + inboxMessageInsertColumns +
	") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)" +
	" ON DUPLICATE KEY UPDATE msg_id = LAST_INSERT_ID(msg_id)"

type defaultInboxMessageModel struct {
	conn sqlx.SqlConn
}

// NewInboxMessageModel 创建 InboxMessageModel 实现。
func NewInboxMessageModel(conn sqlx.SqlConn) InboxMessageModel {
	return &defaultInboxMessageModel{conn: conn}
}

func (m *defaultInboxMessageModel) InsertIdempotent(
	ctx context.Context, session sqlx.Session, msg *InboxMessage,
) (int64, bool, error) {
	if msg.IdempotencyKey == "" {
		return 0, false, ErrInvalidIdempotencyKey
	}
	now := nowUnix()
	if msg.Ctime == 0 {
		msg.Ctime = now
	}
	msg.Mtime = now

	res, err := pick(m.conn, session).ExecCtx(ctx, insertIdempotentSQL,
		msg.Category, msg.MsgType, msg.Title, msg.Content, msg.SenderMid,
		msg.BizType, msg.BizID, msg.Extra, MessageStateNormal, msg.IdempotencyKey, msg.Operator,
		msg.Ctime, msg.Mtime)
	if err != nil {
		return 0, false, fmt.Errorf("inbox_message InsertIdempotent: %w", err)
	}
	msgID, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("inbox_message InsertIdempotent LastInsertId: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("inbox_message InsertIdempotent RowsAffected: %w", err)
	}
	msg.MsgID = msgID
	msg.State = MessageStateNormal
	return msgID, affected == 1, nil
}

func (m *defaultInboxMessageModel) FindOne(ctx context.Context, msgID int64) (*InboxMessage, error) {
	var row InboxMessage
	query := "SELECT " + inboxMessageSelectColumns + " FROM inbox_message WHERE msg_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, msgID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("inbox_message FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultInboxMessageModel) Withdraw(ctx context.Context, msgID int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE inbox_message SET state = ?, mtime = ? WHERE msg_id = ?",
		MessageStateWithdrawn, nowUnix(), msgID); err != nil {
		return fmt.Errorf("inbox_message Withdraw: %w", err)
	}
	return nil
}
