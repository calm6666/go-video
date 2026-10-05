package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 消息状态机，与 pm_message.state、rpc.MsgState 一致（取值不可变更）。
const (
	// MsgStateNormal 正常：会话双方可见。
	MsgStateNormal int32 = 1
	// MsgStatePendingReview 待审核：仅发送者本人可见。
	MsgStatePendingReview int32 = 2
	// MsgStateWithdrawn 已撤回：正文对外替换为占位文案，行与审计保留。
	MsgStateWithdrawn int32 = 3
	// MsgStateRejected 审核驳回：双方不可见正文。
	MsgStateRejected int32 = 4
	// MsgStateDeleted 运营/司法处置删除：保留行与处置记录。
	MsgStateDeleted int32 = 5
)

// 载体类型，与 pm_message.msg_type、rpc.MsgType 一致。
const (
	// MsgTypeText 纯文本。
	MsgTypeText int32 = 1
	// MsgTypeImage 图片（media_ref 存 asset 主键）。
	MsgTypeImage int32 = 2
	// MsgTypeAudio 语音。
	MsgTypeAudio int32 = 3
	// MsgTypeVideo 短视频。
	MsgTypeVideo int32 = 4
	// MsgTypeShare 分享卡片。
	MsgTypeShare int32 = 5
)

// ValidMsgType 判断载体类型是否落在已开放枚举内。
func ValidMsgType(t int32) bool { return t >= MsgTypeText && t <= MsgTypeShare }

// 正文清理标记，与 pm_message.content_purged 一致。
const (
	// ContentPurgedNo 正文仍在留存期内。
	ContentPurgedNo int8 = 0
	// ContentPurgedYes 正文已按留存策略物理清除（只剩摘要与审计）。
	ContentPurgedYes int8 = 1
)

// Message 私信消息（pm_message 表）。
//
// 加密与隐私级别：
//   - ContentCipher 是 AES-GCM 密文（nonce || ciphertext），明文永不落库；
//     加密由 logic/repository 层完成，密钥 ID/版本记在 KeyVersion，主密钥在 Secret/Vault。
//   - ContentHash 是明文的服务端 keyed hash（HMAC-SHA256 + pepper），只用于风控查重与
//     重复骚扰识别，不可反推原文；pepper 不入库。
//   - Preview 是脱敏摘要（例如“[图片]”“[语音]”或截断文本），用于会话列表，
//     任何日志与事件只能带 Preview，不能带正文。
//   - 隐私级别：P4（私密通信内容）。查看明文必须通过 ListMessages/单条读取路径，
//     且调用者必须是会话成员；留存到期由 PurgeByIDs 清空密文。
//
// 幂等：唯一键 (sender_mid, client_msg_id) 保证客户端重试不产生第二条消息；
// 顺序与分页：唯一键 (conversation_id, seq) 既保证 seq 不重复，也是分页索引。
type Message struct {
	// MsgID 消息 ID（主键）
	MsgID int64 `db:"msg_id"`
	// ConversationID 会话 ID
	ConversationID int64 `db:"conversation_id"`
	// Seq 会话内单调递增序列号
	Seq int64 `db:"seq"`
	// SenderMid 发送者
	SenderMid int64 `db:"sender_mid"`
	// ReceiverMid 接收者（单聊快照，避免读时再解析 pair_key）
	ReceiverMid int64 `db:"receiver_mid"`
	// MsgType 载体类型
	MsgType int32 `db:"msg_type"`
	// ContentCipher 密文正文（AES-GCM：nonce || ciphertext）
	ContentCipher []byte `db:"content_cipher"`
	// KeyVersion 加密密钥版本（轮换用，0 表示未加密=已清除）
	KeyVersion int32 `db:"key_version"`
	// ContentHash 明文的 keyed hash（风控查重，不含 pepper）
	ContentHash string `db:"content_hash"`
	// MediaRef 媒体引用（asset 主键字符串，不含对象存储地址）
	MediaRef string `db:"media_ref"`
	// Preview 脱敏摘要（会话列表与日志唯一可见的内容表示）
	Preview string `db:"preview"`
	// State 消息状态，见 MsgState*
	State int32 `db:"state"`
	// AuditTaskID 机审任务 ID（0 表示未送审）
	AuditTaskID int64 `db:"audit_task_id"`
	// AuditEventID 最近一次被应用的审核结论 event_id（重复投递识别）
	AuditEventID string `db:"audit_event_id"`
	// ClientMsgID 客户端消息 ID（幂等键）
	ClientMsgID string `db:"client_msg_id"`
	// ContentPurged 正文是否已按留存策略清除
	ContentPurged int8 `db:"content_purged"`
	// WithdrawTime 撤回/处置生效时间（0 表示未撤回）
	WithdrawTime int64 `db:"withdraw_time"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// HasPlainContent 判断是否还能取到密文正文（清理后返回 false）。
func (m *Message) HasPlainContent() bool {
	return m != nil && m.ContentPurged == ContentPurgedNo && len(m.ContentCipher) > 0
}

// MessageModel pm_message 表读写接口。
type MessageModel interface {
	// Insert 写入消息；session 非空时参与调用方事务（发送路径必须与会话/成员游标同事务）。
	// 命中 uniq_sender_client 时返回 (已存在 ID, false, nil)，供 logic 按“幂等重放”处理。
	Insert(ctx context.Context, session sqlx.Session, msg *Message) (msgID int64, created bool, err error)
	// FindByID 查询消息；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, msgID int64) (*Message, error)
	// FindByClientMsgID 按幂等键查询（重试回放路径）。
	FindByClientMsgID(ctx context.Context, senderMid int64, clientMsgID string) (*Message, error)
	// FindByIDs 批量查询（举报列表与运营投影）。
	FindByIDs(ctx context.Context, ids []int64) (map[int64]*Message, error)
	// ListBeforeSeq 会话内按 seq 倒序翻页；cursorSeq<=0 表示从最新开始。
	// 走 uniq_conv_seq 索引，不做 offset。
	ListBeforeSeq(ctx context.Context, conversationID, cursorSeq int64, ps int32) ([]*Message, error)
	// LastVisible 会话内最后一条“未进终态”的消息，用于重建成员展示投影。
	LastVisible(ctx context.Context, conversationID int64) (*Message, error)
	// CountVisibleAfterSeq 统计 seq 之后仍可见的消息数（未读重算）。
	CountVisibleAfterSeq(ctx context.Context, conversationID, seq int64) (int64, error)
	// HasAnyFromSender 判断该发送者在本会话内是否已有历史消息（「陌生人首条消息」门槛的事实依据）。
	// 只回 EXISTS，不加载密文：判定「是不是第一次找这个人」不需要任何正文。
	HasAnyFromSender(ctx context.Context, conversationID, senderMid int64) (bool, error)
	// BindAuditEvent 登记送审事件（不改状态）：VERDICT_REVIEW 转人审时刷新 task_id/event_id。
	// 只作用于 MsgStatePendingReview 行——已落定的消息不接受「再刷新一次结论」，
	// 返回 applied=false 让 logic 按重复/迟到投递处理，而不是把终态行改回去。
	BindAuditEvent(ctx context.Context, session sqlx.Session, msgID, taskID int64, eventID string) (applied bool, err error)
	// MarkState 状态机迁移：只允许 from 集合内的当前态改到 to，返回 applied=false 表示
	// 当前态不在集合内（重复投递或非法迁移），不抛错以便调用方按幂等处理。
	MarkState(ctx context.Context, session sqlx.Session, msgID int64, from []int32, to int32,
		withdrawTime int64, auditEventID string) (applied bool, err error)
	// BindAuditTask 回写机审任务 ID（送审成功后调用）。
	BindAuditTask(ctx context.Context, session sqlx.Session, msgID, taskID int64) error
	// ListPurgeCandidates 列出留存到期消息 ID（不加载密文）。
	ListPurgeCandidates(ctx context.Context, beforeTime int64, limit int32) ([]int64, error)
	// CountPurgeCandidates 统计到期消息数（dry_run 与漂移观测）。
	CountPurgeCandidates(ctx context.Context, beforeTime int64) (int64, error)
	// PurgeByIDs 清空到期消息的密文与媒体引用，保留状态、摘要与审计字段。
	PurgeByIDs(ctx context.Context, ids []int64) (int64, error)
}

type defaultMessageModel struct {
	conn sqlx.SqlConn
}

// NewMessageModel 创建 MessageModel 实现。
func NewMessageModel(conn sqlx.SqlConn) MessageModel {
	return &defaultMessageModel{conn: conn}
}

const messageColumns = `msg_id, conversation_id, seq, sender_mid, receiver_mid, msg_type, content_cipher,
	key_version, content_hash, media_ref, preview, state, audit_task_id, audit_event_id, client_msg_id,
	content_purged, withdraw_time, ctime, mtime`

func (m *defaultMessageModel) Insert(ctx context.Context, session sqlx.Session, msg *Message) (int64, bool, error) {
	if msg.ConversationID <= 0 || msg.Seq <= 0 {
		return 0, false, ErrConversationNotFound
	}
	// client_msg_id 必填：唯一键 (sender_mid, client_msg_id) 是发送幂等的落点，
	// 留空会让不同消息撞在同一个空串键上，因此在本层直接拒绝（logic 不得补随机值绕过）。
	if msg.ClientMsgID == "" {
		return 0, false, ErrClientMsgIDRequired
	}
	db := pick(session, m.conn)
	now := nowUnix()
	res, err := db.ExecCtx(ctx,
		"INSERT INTO pm_message (conversation_id, seq, sender_mid, receiver_mid, msg_type, content_cipher, "+
			"key_version, content_hash, media_ref, preview, state, audit_task_id, audit_event_id, client_msg_id, "+
			"content_purged, withdraw_time, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		msg.ConversationID, msg.Seq, msg.SenderMid, msg.ReceiverMid, msg.MsgType, msg.ContentCipher,
		msg.KeyVersion, msg.ContentHash, msg.MediaRef, msg.Preview, msg.State, msg.AuditTaskID, msg.AuditEventID,
		msg.ClientMsgID, ContentPurgedNo, int64(0), now, now)
	if err != nil {
		// 唯一键冲突按幂等重放处理：回查首次写入的行。
		if old, qerr := m.FindByClientMsgID(ctx, msg.SenderMid, msg.ClientMsgID); qerr == nil && old != nil {
			return old.MsgID, false, nil
		}
		return 0, false, fmt.Errorf("pm_message Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("pm_message Insert LastInsertId: %w", err)
	}
	return id, true, nil
}

func (m *defaultMessageModel) FindByID(ctx context.Context, msgID int64) (*Message, error) {
	var msg Message
	err := m.conn.QueryRowCtx(ctx, &msg,
		"SELECT "+messageColumns+" FROM pm_message WHERE msg_id = ? LIMIT 1", msgID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_message FindByID: %w", err)
	}
	return &msg, nil
}

func (m *defaultMessageModel) FindByClientMsgID(ctx context.Context, senderMid int64, clientMsgID string) (*Message, error) {
	if clientMsgID == "" {
		return nil, ErrClientMsgIDRequired
	}
	var msg Message
	err := m.conn.QueryRowCtx(ctx, &msg,
		"SELECT "+messageColumns+" FROM pm_message WHERE sender_mid = ? AND client_msg_id = ? LIMIT 1",
		senderMid, clientMsgID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_message FindByClientMsgID: %w", err)
	}
	return &msg, nil
}

func (m *defaultMessageModel) FindByIDs(ctx context.Context, ids []int64) (map[int64]*Message, error) {
	out := make(map[int64]*Message, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	query := "SELECT " + messageColumns + " FROM pm_message WHERE msg_id IN (?" + strings.Repeat(",?", len(ids)-1) + ")"
	var rows []*Message
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("pm_message FindByIDs: %w", err)
	}
	for _, r := range rows {
		out[r.MsgID] = r
	}
	return out, nil
}

func (m *defaultMessageModel) ListBeforeSeq(ctx context.Context, conversationID, cursorSeq int64, ps int32) ([]*Message, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	conds := []string{"conversation_id = ?"}
	args := []any{conversationID}
	if cursorSeq > 0 {
		conds = append(conds, "seq < ?")
		args = append(args, cursorSeq)
	}
	query := "SELECT " + messageColumns + " FROM pm_message WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY seq DESC LIMIT ?"
	args = append(args, ps)

	var rows []*Message
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_message ListBeforeSeq: %w", err)
	}
	return rows, nil
}

func (m *defaultMessageModel) LastVisible(ctx context.Context, conversationID int64) (*Message, error) {
	var msg Message
	err := m.conn.QueryRowCtx(ctx, &msg,
		"SELECT "+messageColumns+" FROM pm_message WHERE conversation_id = ? AND state <> ? ORDER BY seq DESC LIMIT 1",
		conversationID, MsgStateRejected)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_message LastVisible: %w", err)
	}
	return &msg, nil
}

func (m *defaultMessageModel) CountVisibleAfterSeq(ctx context.Context, conversationID, seq int64) (int64, error) {
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM pm_message WHERE conversation_id = ? AND seq > ? AND state = ?",
		conversationID, seq, MsgStateNormal)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("pm_message CountVisibleAfterSeq: %w", err)
	}
	return cnt, nil
}

// HasAnyFromSender 走 uniq_conv_seq(conversation_id, seq) 前缀 + sender_mid 过滤的 EXISTS 查询，
// 命中一条即返回，不做 COUNT(*)（陌生人门槛只关心「有没有」，不关心「多少条」）。
func (m *defaultMessageModel) HasAnyFromSender(ctx context.Context, conversationID, senderMid int64) (bool, error) {
	if conversationID <= 0 {
		return false, ErrConversationNotFound
	}
	if senderMid <= 0 {
		return false, ErrInvalidMid
	}
	var hit int64
	err := m.conn.QueryRowCtx(ctx, &hit,
		"SELECT 1 FROM pm_message WHERE conversation_id = ? AND sender_mid = ? LIMIT 1", conversationID, senderMid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("pm_message HasAnyFromSender: %w", err)
	}
	return true, nil
}

// BindAuditEvent 只刷新送审登记，不触碰 state/withdraw_time：
// 审核链路里「转人审」是等待而非结论，把 state 改写就等于替人审给出结论（AGENTS.md §8）。
// WHERE 带 state = PENDING_REVIEW，终态行改不到（applied=false），历史结论不会被后到的投递覆盖。
func (m *defaultMessageModel) BindAuditEvent(ctx context.Context, session sqlx.Session, msgID, taskID int64,
	eventID string) (bool, error) {
	if msgID <= 0 {
		return false, ErrMessageNotFound
	}
	if eventID == "" {
		return false, ErrIdempotencyKeyRequired
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE pm_message SET audit_task_id = ?, audit_event_id = ?, mtime = ? WHERE msg_id = ? AND state = ?",
		taskID, eventID, nowUnix(), msgID, MsgStatePendingReview)
	if err != nil {
		return false, fmt.Errorf("pm_message BindAuditEvent: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("pm_message BindAuditEvent RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultMessageModel) MarkState(ctx context.Context, session sqlx.Session, msgID int64, from []int32,
	to int32, withdrawTime int64, auditEventID string) (bool, error) {
	if len(from) == 0 {
		return false, ErrInvalidStateTransition
	}
	cur, err := m.FindByID(ctx, msgID)
	if err != nil {
		return false, err
	}
	if cur == nil {
		return false, ErrMessageNotFound
	}
	allowed := false
	for _, s := range from {
		if s == cur.State {
			allowed = true
			break
		}
	}
	if !allowed {
		// 当前态不在允许集合内：重复投递或非法迁移，交由 logic 判断是幂等还是报错。
		return false, nil
	}
	if withdrawTime <= 0 {
		withdrawTime = cur.WithdrawTime
	}
	if auditEventID == "" {
		auditEventID = cur.AuditEventID
	}
	// CAS：WHERE 带读取到的旧 state，避免与并发的另一次状态推进互相覆盖。
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE pm_message SET state = ?, withdraw_time = ?, audit_event_id = ?, mtime = ? "+
			"WHERE msg_id = ? AND state = ?",
		to, withdrawTime, auditEventID, nowUnix(), msgID, cur.State)
	if err != nil {
		return false, fmt.Errorf("pm_message MarkState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("pm_message MarkState RowsAffected: %w", err)
	}
	if aff == 0 {
		return false, ErrConcurrentUpdate
	}
	return true, nil
}

func (m *defaultMessageModel) BindAuditTask(ctx context.Context, session sqlx.Session, msgID, taskID int64) error {
	_, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE pm_message SET audit_task_id = ?, mtime = ? WHERE msg_id = ? AND audit_task_id = 0",
		taskID, nowUnix(), msgID)
	if err != nil {
		return fmt.Errorf("pm_message BindAuditTask: %w", err)
	}
	return nil
}

func (m *defaultMessageModel) ListPurgeCandidates(ctx context.Context, beforeTime int64, limit int32) ([]int64, error) {
	if limit <= 0 {
		return nil, ErrInvalidPage
	}
	// 只取主键：清理路径不需要（也不应）把密文读进内存。
	var ids []int64
	err := m.conn.QueryRowsCtx(ctx, &ids,
		"SELECT msg_id FROM pm_message WHERE ctime < ? AND content_purged = ? ORDER BY ctime ASC LIMIT ?",
		beforeTime, ContentPurgedNo, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_message ListPurgeCandidates: %w", err)
	}
	return ids, nil
}

func (m *defaultMessageModel) CountPurgeCandidates(ctx context.Context, beforeTime int64) (int64, error) {
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM pm_message WHERE ctime < ? AND content_purged = ?", beforeTime, ContentPurgedNo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("pm_message CountPurgeCandidates: %w", err)
	}
	return cnt, nil
}

func (m *defaultMessageModel) PurgeByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	query := "UPDATE pm_message SET content_cipher = ?, key_version = ?, content_purged = ?, media_ref = '', mtime = ? " +
		"WHERE content_purged = ? AND msg_id IN (?" + strings.Repeat(",?", len(ids)-1) + ")"
	// 明文与媒体引用一起丢弃；preview、状态与审计字段保留（合规证据链优先于正文留存）。
	args := make([]any, 0, len(ids)+6)
	args = append(args, "", 0, ContentPurgedYes, nowUnix(), ContentPurgedNo)
	for _, id := range ids {
		args = append(args, id)
	}

	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("pm_message PurgeByIDs: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pm_message PurgeByIDs RowsAffected: %w", err)
	}
	return aff, nil
}
