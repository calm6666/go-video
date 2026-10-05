// Package model 是 private-message 服务的数据库访问层，只操作 go_video_private_message 库
// 自身的表（AGENTS.md §5：服务只能写自己的 schema，跨服务只存对方主键引用）。
//
// 表清单与 deploy/migrations/private-message/*.sql 严格一致：
//
//	pm_conversation        单聊会话主体：pair_key 唯一键 + 会话内 seq 分配锚点
//	pm_conversation_member 会话成员行：已读游标、本方隐藏位与列表展示投影
//	pm_message             私信消息：密文正文、会话内 seq、幂等键与状态机
//	pm_user_setting        用户反骚扰偏好（接收范围、陌生人门槛等）
//	pm_report              私信举报事实（审核结论由 moderation-orchestrator 写）
//	pm_withdraw_log        撤回审计流水（只追加，不改写）
//
// 与 inbox（站内信/系统消息）分库分表，绝不共表：会话与私信是用户之间的数据，
// 系统消息是平台对用户的投递，两者的留存、加密和审计口径不同
// （services/private-message/README.md 约束、AGENTS.md §5）。
package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 会话状态，与 pm_conversation.state、rpc.ConversationState 一致。
const (
	// ConversationStateNormal 正常。
	ConversationStateNormal int32 = 1
	// ConversationStateFrozen 风控冻结（禁止新发送，历史仍可读；真值在 risk-control）。
	ConversationStateFrozen int32 = 2
)

// Conversation 单聊会话主体（pm_conversation 表）。
//
// 唯一性：pair_key = 双方 mid 升序拼接（见 PairKey），(a,b) 与 (b,a) 命中同一行，
// 因此“建会话”天然是幂等操作，不依赖分布式锁。
// seq 锚点：last_seq 是会话内已分配的最大序列号，发送时在事务内以行锁递增，
// 使消息顺序与分页游标只由数据库保证，不依赖 Redis（容量与一致性取舍见 README）。
type Conversation struct {
	// ConversationID 会话 ID（主键）
	ConversationID int64 `db:"conversation_id"`
	// PairKey 规范化双方键
	PairKey string `db:"pair_key"`
	// UserA mid 较小的一方
	UserA int64 `db:"user_a"`
	// UserB mid 较大的一方
	UserB int64 `db:"user_b"`
	// State 会话状态，见 ConversationState*
	State int32 `db:"state"`
	// LastSeq 会话内已分配的最大序列号
	LastSeq int64 `db:"last_seq"`
	// LastMsgID 最后一条消息 ID
	LastMsgID int64 `db:"last_msg_id"`
	// LastMsgTime 最后消息时间（Unix 秒）
	LastMsgTime int64 `db:"last_msg_time"`
	// CreatedAt 创建时间（Unix 秒）
	CreatedAt int64 `db:"created_at"`
	// UpdatedAt 最近更新时间（Unix 秒）
	UpdatedAt int64 `db:"updated_at"`
}

// ConversationModel pm_conversation 表读写接口。
type ConversationModel interface {
	// FindOrCreate 按 pair_key 定位会话；不存在时创建会话主体并返回 created=true。
	// 并发下依赖 uniq_pair_key 兜底：写入撞唯一键后回查一次，按已存在返回，
	// 保证同一对用户只会有一行会话。
	FindOrCreate(ctx context.Context, pairKey string, userA, userB int64) (conv *Conversation, created bool, err error)
	// FindOrCreateInTx 与 FindOrCreate 同语义，但参与调用方事务：
	// 「建档 + 补齐成员行」必须原子，否则崩溃窗口会留下没有成员行的会话
	// （成员行是越权判定的依据，缺行的会话任何人都读不到，只能靠再次建档自愈）。
	FindOrCreateInTx(ctx context.Context, session sqlx.Session, pairKey string, userA, userB int64) (conv *Conversation, created bool, err error)
	// FindByID 查询会话；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, conversationID int64) (*Conversation, error)
	// FindByPairKey 按规范化双方键定位会话；不存在返回 (nil, nil)。
	// 发送路径先用它判断「这是不是新建会话」，从而在建档前就把陌生人日配额挡住
	// （配额是拒收理由，不能靠「先建再删」实现——会话行与 seq 锚点删不掉）。
	FindByPairKey(ctx context.Context, pairKey string) (*Conversation, error)
	// FindByIDs 批量查询会话，返回 conversation_id -> 会话；用于会话列表补状态。
	FindByIDs(ctx context.Context, ids []int64) (map[int64]*Conversation, error)
	// AllocateSeq 在事务内为会话分配下一个 seq（行锁串行化，返回 0 表示会话不存在）。
	AllocateSeq(ctx context.Context, session sqlx.Session, conversationID int64) (int64, error)
	// TouchLastMessage 事务内把会话的 last_msg 游标推进到给定消息（seq 回退时忽略）。
	TouchLastMessage(ctx context.Context, session sqlx.Session, conversationID, msgID, seq, msgTime int64) error
	// UpdateState 更新会话状态（风控冻结/解冻，operator 由 logic 侧审计落日志）。
	UpdateState(ctx context.Context, conversationID int64, state int32) (*Conversation, error)
}

type defaultConversationModel struct {
	conn sqlx.SqlConn
}

// NewConversationModel 创建 ConversationModel 实现。
func NewConversationModel(conn sqlx.SqlConn) ConversationModel {
	return &defaultConversationModel{conn: conn}
}

const conversationColumns = `conversation_id, pair_key, user_a, user_b, state, last_seq, last_msg_id, last_msg_time, created_at, updated_at`

func (m *defaultConversationModel) FindOrCreate(ctx context.Context, pairKey string, userA, userB int64) (*Conversation, bool, error) {
	return m.findOrCreate(ctx, nil, pairKey, userA, userB)
}

func (m *defaultConversationModel) FindOrCreateInTx(ctx context.Context, session sqlx.Session,
	pairKey string, userA, userB int64) (*Conversation, bool, error) {
	return m.findOrCreate(ctx, session, pairKey, userA, userB)
}

func (m *defaultConversationModel) findOrCreate(ctx context.Context, session sqlx.Session,
	pairKey string, userA, userB int64) (*Conversation, bool, error) {
	now := nowUnix()
	db := pick(session, m.conn)
	res, err := db.ExecCtx(ctx,
		"INSERT INTO pm_conversation (pair_key, user_a, user_b, state, last_seq, last_msg_id, last_msg_time, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 0, 0, ?, ?, ?) ON DUPLICATE KEY UPDATE conversation_id = conversation_id",
		pairKey, userA, userB, ConversationStateNormal, now, now, now)
	if err != nil {
		return nil, false, fmt.Errorf("pm_conversation FindOrCreate: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("pm_conversation FindOrCreate RowsAffected: %w", err)
	}
	lastID, err := res.LastInsertId()
	if err != nil {
		return nil, false, fmt.Errorf("pm_conversation FindOrCreate LastInsertId: %w", err)
	}

	conv, err := m.findByKeyOrID(ctx, session, pairKey, lastID, affected == 1)
	if err != nil {
		return nil, false, err
	}
	if conv == nil {
		return nil, false, ErrConversationNotFound
	}
	// MySQL 对 ON DUPLICATE KEY UPDATE 的语义：新插入 affected=1，命中唯一键 affected=0
	// （赋值表达式未改变任何列）。created 只在该判断成立时为真。
	return conv, affected == 1, nil
}

// findByKeyOrID 优先按 pair_key 回查；只有确认是新插入行时才用自增 ID 直读，
// 避免 LastInsertId 在 ON DUPLICATE KEY 语义下指向错误行。
func (m *defaultConversationModel) findByKeyOrID(ctx context.Context, session sqlx.Session,
	pairKey string, lastID int64, isNew bool) (*Conversation, error) {
	var c Conversation
	db := pick(session, m.conn)
	err := db.QueryRowCtx(ctx, &c,
		"SELECT "+conversationColumns+" FROM pm_conversation WHERE pair_key = ? LIMIT 1", pairKey)
	if err == nil {
		return &c, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("pm_conversation FindByPairKey: %w", err)
	}
	if !isNew {
		return nil, nil
	}
	err = db.QueryRowCtx(ctx, &c,
		"SELECT "+conversationColumns+" FROM pm_conversation WHERE conversation_id = ? LIMIT 1", lastID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_conversation FindByID: %w", err)
	}
	return &c, nil
}

func (m *defaultConversationModel) FindByID(ctx context.Context, conversationID int64) (*Conversation, error) {
	var c Conversation
	err := m.conn.QueryRowCtx(ctx, &c,
		"SELECT "+conversationColumns+" FROM pm_conversation WHERE conversation_id = ? LIMIT 1", conversationID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_conversation FindByID: %w", err)
	}
	return &c, nil
}

func (m *defaultConversationModel) FindByPairKey(ctx context.Context, pairKey string) (*Conversation, error) {
	if pairKey == "" {
		return nil, ErrConversationNotFound
	}
	var c Conversation
	err := m.conn.QueryRowCtx(ctx, &c,
		"SELECT "+conversationColumns+" FROM pm_conversation WHERE pair_key = ? LIMIT 1", pairKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_conversation FindByPairKey: %w", err)
	}
	return &c, nil
}

func (m *defaultConversationModel) FindByIDs(ctx context.Context, ids []int64) (map[int64]*Conversation, error) {
	out := make(map[int64]*Conversation, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	query := "SELECT " + conversationColumns + " FROM pm_conversation WHERE conversation_id IN (?" +
		strings.Repeat(",?", len(ids)-1) + ")"
	var rows []*Conversation
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("pm_conversation FindByIDs: %w", err)
	}
	for _, r := range rows {
		out[r.ConversationID] = r
	}
	return out, nil
}

func (m *defaultConversationModel) AllocateSeq(ctx context.Context, session sqlx.Session, conversationID int64) (int64, error) {
	if session == nil {
		return 0, errors.New("pm_conversation AllocateSeq: session required (must run in transaction)")
	}
	var cur int64
	err := session.QueryRowCtx(ctx, &cur,
		"SELECT last_seq FROM pm_conversation WHERE conversation_id = ? FOR UPDATE", conversationID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrConversationNotFound
		}
		return 0, fmt.Errorf("pm_conversation AllocateSeq lock: %w", err)
	}
	next := cur + 1
	if _, err := session.ExecCtx(ctx,
		"UPDATE pm_conversation SET last_seq = ?, updated_at = ? WHERE conversation_id = ? AND last_seq = ?",
		next, nowUnix(), conversationID, cur); err != nil {
		return 0, fmt.Errorf("pm_conversation AllocateSeq update: %w", err)
	}
	return next, nil
}

func (m *defaultConversationModel) TouchLastMessage(ctx context.Context, session sqlx.Session, conversationID, msgID, seq, msgTime int64) error {
	// 只前进不后退：并发发送时低 seq 的迟到更新会被 WHERE 过滤掉。
	_, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE pm_conversation SET last_seq = ?, last_msg_id = ?, last_msg_time = ?, updated_at = ? "+
			"WHERE conversation_id = ? AND last_seq < ?",
		seq, msgID, msgTime, nowUnix(), conversationID, seq)
	if err != nil {
		return fmt.Errorf("pm_conversation TouchLastMessage: %w", err)
	}
	return nil
}

func (m *defaultConversationModel) UpdateState(ctx context.Context, conversationID int64, state int32) (*Conversation, error) {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE pm_conversation SET state = ?, updated_at = ? WHERE conversation_id = ?",
		state, nowUnix(), conversationID)
	if err != nil {
		return nil, fmt.Errorf("pm_conversation UpdateState: %w", err)
	}
	return m.FindByID(ctx, conversationID)
}

// pick 选择执行载体：session 非 nil 时走事务，否则走连接。
// 私信的发送路径必须同事务写消息与会话/成员游标，因此多数写方法都接受 session。
func pick(session sqlx.Session, conn sqlx.SqlConn) sqlx.Session {
	if session != nil {
		return session
	}
	return conn
}
