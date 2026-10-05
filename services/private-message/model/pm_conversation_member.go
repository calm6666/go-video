package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 成员侧隐藏状态，与 pm_conversation_member.hide_state 一致。
const (
	// HideStateNormal 正常展示。
	HideStateNormal int32 = 0
	// HideStateHidden 本方隐藏（不影响对方，也不删除消息）。
	HideStateHidden int32 = 1
)

// ConversationMember 会话成员行（pm_conversation_member 表）：一行一个（会话，用户）。
//
// 已读语义（取舍见 services/private-message/README.md）：
//   - read_seq 是“读到哪”的游标，而不是逐条已读状态表。逐条回执的行数级为
//     会话成员数 × 消息数，单聊高频用户会放大到亿级；游标方案固定 2 行/会话。
//   - unread_count 是投影列：发送时 +1、读到即清零、撤回未读时 -1，
//     可由 pm_message 用 RebuildProjection 完全重建，不作为唯一事实源。
//
// 展示列（last_*）同样是投影：与会话主体在同一事务内更新，
// 目的是让会话列表只扫本表、不 JOIN pm_message。
type ConversationMember struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// ConversationID 会话 ID
	ConversationID int64 `db:"conversation_id"`
	// Mid 成员 mid
	Mid int64 `db:"mid"`
	// PeerMid 对方 mid（列表投影直接展示用，来源为 social-graph 的用户主键）
	PeerMid int64 `db:"peer_mid"`
	// ReadSeq 已读游标：本方看到过的最大 seq，只前进不回退
	ReadSeq int64 `db:"read_seq"`
	// UnreadCount 未读数投影
	UnreadCount int64 `db:"unread_count"`
	// LastSeq 最后消息 seq 快照
	LastSeq int64 `db:"last_seq"`
	// LastMsgID 最后消息 ID 快照
	LastMsgID int64 `db:"last_msg_id"`
	// LastMsgType 最后消息载体类型快照
	LastMsgType int32 `db:"last_msg_type"`
	// LastPreview 最后消息脱敏摘要（不含正文原文）
	LastPreview string `db:"last_preview"`
	// LastMsgTime 最后消息时间（Unix 秒，会话列表游标）
	LastMsgTime int64 `db:"last_msg_time"`
	// HideState 本方隐藏状态
	HideState int32 `db:"hide_state"`
	// CreatedAt 创建时间（Unix 秒）
	CreatedAt int64 `db:"created_at"`
	// UpdatedAt 最近更新时间（Unix 秒）
	UpdatedAt int64 `db:"updated_at"`
}

// Unread 返回投影未读数（防御性下限 0，避免历史脏数据把角标算成负数）。
func (m *ConversationMember) Unread() int64 {
	if m == nil || m.UnreadCount <= 0 {
		return 0
	}
	return m.UnreadCount
}

// MemberProjection 是重建成员展示投影所需的输入（由 logic 从 pm_message 聚合得到）。
type MemberProjection struct {
	LastSeq     int64
	LastMsgID   int64
	LastMsgType int32
	LastPreview string
	LastMsgTime int64
	UnreadCount int64
	ReadSeq     int64
}

// ListMembersOptions 会话列表查询参数（黑名单/风控过滤由 logic 在结果集上完成，
// 因为黑名单真值在 social-graph，本表不存对方关系）。
type ListMembersOptions struct {
	Mid           int64
	CursorTime    int64 // 上一页末条的 last_msg_time
	CursorID      int64 // 上一页末条的 id
	PageSize      int32
	OnlyUnread    bool
	IncludeHidden bool
}

// ConversationMemberModel pm_conversation_member 表读写接口。
type ConversationMemberModel interface {
	// Ensure 幂等补齐会话成员行（建会话时同事务写两行）。
	// 唯一键 uniq_conv_mid(conversation_id, mid) 保证重复调用不产生第二行；
	// (mid, peer_mid) 只有普通索引 idx_mid_peer（供 FindByPeer 定位），它的单聊唯一性
	// 由 pm_conversation.uniq_pair_key 间接保证——同一对用户只可能有一行会话。
	Ensure(ctx context.Context, session sqlx.Session, rows []*ConversationMember) error
	// Find 查询某会话中某成员的行；不存在返回 (nil, nil)。
	Find(ctx context.Context, conversationID, mid int64) (*ConversationMember, error)
	// FindByPeer 按 (mid, peer_mid) 定位单聊会话成员行（幂等入口，避免先查会话）。
	FindByPeer(ctx context.Context, mid, peerMid int64) (*ConversationMember, error)
	// ListByMid 会话列表：按 (last_msg_time, id) 倒序游标翻页，一次只扫本表。
	ListByMid(ctx context.Context, opt ListMembersOptions) ([]*ConversationMember, error)
	// ListPeersByMid 会话列表用的 (mid, peer_mid) 批量定位，供过滤前探测已存在会话。
	ListPeersByMid(ctx context.Context, mid int64, peerMids []int64) (map[int64]*ConversationMember, error)
	// ListByConversation 查询会话全部成员行（撤回/重算需要扇出到双方）。
	ListByConversation(ctx context.Context, conversationID int64) ([]*ConversationMember, error)
	// ApplyIncoming 事务内落一条新消息的成员投影：
	// 接收方 unread_count +1；发送方把自己的 read_seq 顶到该 seq 且未读保持 0。
	ApplyIncoming(ctx context.Context, session sqlx.Session, conversationID, senderMid, receiverMid, msgID, seq int64, msgType int32, preview string, msgTime int64) error
	// MarkRead 前移已读游标并把未读清零；游标回退时 applied=false（幂等）。
	MarkRead(ctx context.Context, conversationID, mid, readSeq int64) (applied bool, current *ConversationMember, err error)
	// SetHidden 更新本方隐藏位。
	SetHidden(ctx context.Context, conversationID, mid int64, hidden bool) error
	// DecrementUnreadIfUnread 撤回时调用：对“该 seq 尚未读到”的成员扣减未读投影。
	DecrementUnreadIfUnread(ctx context.Context, session sqlx.Session, conversationID, seq int64) error
	// RefreshPreview 刷新成员行的摘要快照（撤回后把 last_preview 换成占位文案）。
	RefreshPreview(ctx context.Context, session sqlx.Session, conversationID, msgID int64, preview string) error
	// RebuildProjection 用事实表重算的结果覆盖成员投影（cron/运营修复入口）。
	RebuildProjection(ctx context.Context, conversationID, mid int64, p MemberProjection) error
	// SumUnread 汇总未读消息数与有未读的会话数（投影口径）。
	SumUnread(ctx context.Context, mid int64, includeHidden bool) (unreadTotal, unreadConversations int64, err error)
}

type defaultConversationMemberModel struct {
	conn sqlx.SqlConn
}

// NewConversationMemberModel 创建 ConversationMemberModel 实现。
func NewConversationMemberModel(conn sqlx.SqlConn) ConversationMemberModel {
	return &defaultConversationMemberModel{conn: conn}
}

const memberColumns = `id, conversation_id, mid, peer_mid, read_seq, unread_count, last_seq, last_msg_id,
	last_msg_type, last_preview, last_msg_time, hide_state, created_at, updated_at`

func (m *defaultConversationMemberModel) Ensure(ctx context.Context, session sqlx.Session, rows []*ConversationMember) error {
	if len(rows) == 0 {
		return nil
	}
	stmt := "INSERT INTO pm_conversation_member (conversation_id, mid, peer_mid, read_seq, unread_count, last_seq, " +
		"last_msg_id, last_msg_type, last_preview, last_msg_time, hide_state, created_at, updated_at) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE updated_at = VALUES(updated_at)"
	db := pick(session, m.conn)
	for _, r := range rows {
		if r == nil {
			return errors.New("pm_conversation_member Ensure: nil row")
		}
		now := nowUnix()
		if _, err := db.ExecCtx(ctx, stmt, r.ConversationID, r.Mid, r.PeerMid, r.ReadSeq, r.UnreadCount,
			r.LastSeq, r.LastMsgID, r.LastMsgType, r.LastPreview, r.LastMsgTime, r.HideState, now, now); err != nil {
			return fmt.Errorf("pm_conversation_member Ensure: %w", err)
		}
	}
	return nil
}

func (m *defaultConversationMemberModel) Find(ctx context.Context, conversationID, mid int64) (*ConversationMember, error) {
	var r ConversationMember
	err := m.conn.QueryRowCtx(ctx, &r,
		"SELECT "+memberColumns+" FROM pm_conversation_member WHERE conversation_id = ? AND mid = ? LIMIT 1",
		conversationID, mid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_conversation_member Find: %w", err)
	}
	return &r, nil
}

func (m *defaultConversationMemberModel) FindByPeer(ctx context.Context, mid, peerMid int64) (*ConversationMember, error) {
	var r ConversationMember
	err := m.conn.QueryRowCtx(ctx, &r,
		"SELECT "+memberColumns+" FROM pm_conversation_member WHERE mid = ? AND peer_mid = ? LIMIT 1",
		mid, peerMid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_conversation_member FindByPeer: %w", err)
	}
	return &r, nil
}

func (m *defaultConversationMemberModel) ListByMid(ctx context.Context, opt ListMembersOptions) ([]*ConversationMember, error) {
	if opt.PageSize <= 0 {
		return nil, ErrInvalidPage
	}
	var (
		conds = []string{"mid = ?"}
		args  = []any{opt.Mid}
	)
	if !opt.IncludeHidden {
		conds = append(conds, "hide_state = ?")
		args = append(args, HideStateNormal)
	}
	if opt.OnlyUnread {
		conds = append(conds, "unread_count > 0")
	}
	if opt.CursorTime > 0 {
		// (time, id) 双列游标：同一秒内的多条会话也能稳定翻页。
		conds = append(conds, "(last_msg_time < ? OR (last_msg_time = ? AND id < ?))")
		args = append(args, opt.CursorTime, opt.CursorTime, opt.CursorID)
	}
	query := "SELECT " + memberColumns + " FROM pm_conversation_member WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY last_msg_time DESC, id DESC LIMIT ?"
	args = append(args, opt.PageSize)

	var rows []*ConversationMember
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_conversation_member ListByMid: %w", err)
	}
	return rows, nil
}

func (m *defaultConversationMemberModel) ListPeersByMid(ctx context.Context, mid int64, peerMids []int64) (map[int64]*ConversationMember, error) {
	out := make(map[int64]*ConversationMember, len(peerMids))
	if len(peerMids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(peerMids)+1)
	args = append(args, mid)
	for _, p := range peerMids {
		args = append(args, p)
	}
	query := "SELECT " + memberColumns + " FROM pm_conversation_member WHERE mid = ? AND peer_mid IN (?" +
		strings.Repeat(",?", len(peerMids)-1) + ")"
	var rows []*ConversationMember
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("pm_conversation_member ListPeersByMid: %w", err)
	}
	for _, r := range rows {
		out[r.PeerMid] = r
	}
	return out, nil
}

func (m *defaultConversationMemberModel) ListByConversation(ctx context.Context, conversationID int64) ([]*ConversationMember, error) {
	var rows []*ConversationMember
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+memberColumns+" FROM pm_conversation_member WHERE conversation_id = ? ORDER BY mid ASC",
		conversationID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_conversation_member ListByConversation: %w", err)
	}
	return rows, nil
}

func (m *defaultConversationMemberModel) ApplyIncoming(ctx context.Context, session sqlx.Session,
	conversationID, senderMid, receiverMid, msgID, seq int64, msgType int32, preview string, msgTime int64) error {
	db := pick(session, m.conn)
	now := nowUnix()

	// 接收方：投影前移 + 未读 +1（GREATEST 防并发下界）。
	if _, err := db.ExecCtx(ctx,
		"UPDATE pm_conversation_member SET last_seq = ?, last_msg_id = ?, last_msg_type = ?, last_preview = ?, "+
			"last_msg_time = ?, unread_count = GREATEST(unread_count + 1, 1), hide_state = ?, updated_at = ? "+
			"WHERE conversation_id = ? AND mid = ? AND last_seq < ?",
		seq, msgID, msgType, preview, msgTime, HideStateNormal, now, conversationID, receiverMid, seq); err != nil {
		return fmt.Errorf("pm_conversation_member ApplyIncoming receiver: %w", err)
	}
	// 发送方：自己的消息视为已读，只前移投影，不加未读。
	if _, err := db.ExecCtx(ctx,
		"UPDATE pm_conversation_member SET last_seq = ?, last_msg_id = ?, last_msg_type = ?, last_preview = ?, "+
			"last_msg_time = ?, read_seq = ?, unread_count = 0, hide_state = ?, updated_at = ? "+
			"WHERE conversation_id = ? AND mid = ? AND last_seq < ?",
		seq, msgID, msgType, preview, msgTime, seq, HideStateNormal, now, conversationID, senderMid, seq); err != nil {
		return fmt.Errorf("pm_conversation_member ApplyIncoming sender: %w", err)
	}
	return nil
}

func (m *defaultConversationMemberModel) MarkRead(ctx context.Context, conversationID, mid, readSeq int64) (bool, *ConversationMember, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE pm_conversation_member SET read_seq = ?, unread_count = 0, updated_at = ? "+
			"WHERE conversation_id = ? AND mid = ? AND read_seq < ?",
		readSeq, nowUnix(), conversationID, mid, readSeq)
	if err != nil {
		return false, nil, fmt.Errorf("pm_conversation_member MarkRead: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("pm_conversation_member MarkRead RowsAffected: %w", err)
	}
	cur, err := m.Find(ctx, conversationID, mid)
	if err != nil {
		return false, nil, err
	}
	return aff > 0, cur, nil
}

func (m *defaultConversationMemberModel) SetHidden(ctx context.Context, conversationID, mid int64, hidden bool) error {
	state := HideStateNormal
	if hidden {
		state = HideStateHidden
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE pm_conversation_member SET hide_state = ?, updated_at = ? WHERE conversation_id = ? AND mid = ?",
		state, nowUnix(), conversationID, mid)
	if err != nil {
		return fmt.Errorf("pm_conversation_member SetHidden: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("pm_conversation_member SetHidden RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrNotConversationMember
	}
	return nil
}

func (m *defaultConversationMemberModel) DecrementUnreadIfUnread(ctx context.Context, session sqlx.Session,
	conversationID, seq int64) error {
	_, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE pm_conversation_member SET unread_count = GREATEST(unread_count - 1, 0), updated_at = ? "+
			"WHERE conversation_id = ? AND read_seq < ? AND unread_count > 0",
		nowUnix(), conversationID, seq)
	if err != nil {
		return fmt.Errorf("pm_conversation_member DecrementUnreadIfUnread: %w", err)
	}
	return nil
}

func (m *defaultConversationMemberModel) RefreshPreview(ctx context.Context, session sqlx.Session,
	conversationID, msgID int64, preview string) error {
	_, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE pm_conversation_member SET last_preview = ?, updated_at = ? WHERE conversation_id = ? AND last_msg_id = ?",
		preview, nowUnix(), conversationID, msgID)
	if err != nil {
		return fmt.Errorf("pm_conversation_member RefreshPreview: %w", err)
	}
	return nil
}

func (m *defaultConversationMemberModel) RebuildProjection(ctx context.Context, conversationID, mid int64,
	p MemberProjection) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE pm_conversation_member SET read_seq = ?, unread_count = ?, last_seq = ?, last_msg_id = ?, "+
			"last_msg_type = ?, last_preview = ?, last_msg_time = ?, updated_at = ? "+
			"WHERE conversation_id = ? AND mid = ?",
		p.ReadSeq, p.UnreadCount, p.LastSeq, p.LastMsgID, p.LastMsgType, p.LastPreview, p.LastMsgTime,
		nowUnix(), conversationID, mid)
	if err != nil {
		return fmt.Errorf("pm_conversation_member RebuildProjection: %w", err)
	}
	return nil
}

func (m *defaultConversationMemberModel) SumUnread(ctx context.Context, mid int64, includeHidden bool) (int64, int64, error) {
	conds := []string{"mid = ?", "unread_count > 0"}
	args := []any{mid}
	if !includeHidden {
		conds = append(conds, "hide_state = ?")
		args = append(args, HideStateNormal)
	}
	query := "SELECT COALESCE(SUM(unread_count), 0) AS unread_total, COUNT(*) AS unread_conversations " +
		"FROM pm_conversation_member WHERE " + strings.Join(conds, " AND ")

	var row unreadSummaryRow
	if err := m.conn.QueryRowCtx(ctx, &row, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("pm_conversation_member SumUnread: %w", err)
	}
	return row.UnreadTotal, row.UnreadConversations, nil
}

// unreadSummaryRow 承载未读汇总的双列结果（列名与 SQL 别名一一对应）。
type unreadSummaryRow struct {
	UnreadTotal         int64 `db:"unread_total"`
	UnreadConversations int64 `db:"unread_conversations"`
}
