package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 撤回来源，与 pm_withdraw_log.source、rpc.WithdrawSource 一致。
const (
	// WithdrawSourceSender 发送者本人限时撤回。
	WithdrawSourceSender int32 = 1
	// WithdrawSourceReceiver 接收方撤回自己收到的这条消息。
	WithdrawSourceReceiver int32 = 2
	// WithdrawSourceModeration 审核结论驱动的系统撤回。
	WithdrawSourceModeration int32 = 3
	// WithdrawSourceAdmin 运营/管理员处置撤回。
	WithdrawSourceAdmin int32 = 4
)

// ValidWithdrawSource 判断撤回来源合法。
func ValidWithdrawSource(v int32) bool { return v >= WithdrawSourceSender && v <= WithdrawSourceAdmin }

// WithdrawLog 撤回审计流水（pm_withdraw_log 表）。
//
// 依据 AGENTS.md §8：删除、下架与撤回必须保留审计证据。本表只追加、不改写、不删除，
// 因此即使 pm_message 的正文按留存策略被物理清除，撤回处置链依旧可追溯。
// Reason 只允许写脱敏描述（命中规则、举报单号），不得写私信正文。
type WithdrawLog struct {
	// LogID 流水 ID（主键）
	LogID int64 `db:"log_id"`
	// MsgID 被撤回消息 ID
	MsgID int64 `db:"msg_id"`
	// ConversationID 会话 ID
	ConversationID int64 `db:"conversation_id"`
	// Seq 消息会话内序列号（快照，便于按会话回放时间线）
	Seq int64 `db:"seq"`
	// SenderMid 原发送者
	SenderMid int64 `db:"sender_mid"`
	// OperatorMid 操作者 mid（系统撤回为 0）
	OperatorMid int64 `db:"operator_mid"`
	// Source 撤回来源
	Source int32 `db:"source"`
	// Reason 脱敏原因
	Reason string `db:"reason"`
	// AuditTaskID 关联审核任务 ID（source=MODERATION 时非 0）
	AuditTaskID int64 `db:"audit_task_id"`
	// ReportID 关联举报单 ID（由举报处置触发时非 0）
	ReportID int64 `db:"report_id"`
	// Ctime 生效时间（Unix 秒）
	Ctime int64 `db:"ctime"`
}

// WithdrawLogModel pm_withdraw_log 表读写接口。
type WithdrawLogModel interface {
	// Insert 追加一条撤回审计（session 非空时与状态迁移同事务，保证“改了状态必有流水”）。
	Insert(ctx context.Context, session sqlx.Session, log *WithdrawLog) error
	// ListByMsgID 查询某消息的撤回流水（审计回查，通常 0~1 条）。
	ListByMsgID(ctx context.Context, msgID int64) ([]*WithdrawLog, error)
	// ListByOperator 查询某操作者的处置流水（运营自查与问责）。
	ListByOperator(ctx context.Context, operatorMid int64, ps int32) ([]*WithdrawLog, error)
}

type defaultWithdrawLogModel struct {
	conn sqlx.SqlConn
}

// NewWithdrawLogModel 创建 WithdrawLogModel 实现。
func NewWithdrawLogModel(conn sqlx.SqlConn) WithdrawLogModel {
	return &defaultWithdrawLogModel{conn: conn}
}

const withdrawLogColumns = `log_id, msg_id, conversation_id, seq, sender_mid, operator_mid, source, reason,
	audit_task_id, report_id, ctime`

func (m *defaultWithdrawLogModel) Insert(ctx context.Context, session sqlx.Session, l *WithdrawLog) error {
	if l == nil || l.MsgID <= 0 {
		return ErrMessageNotFound
	}
	if !ValidWithdrawSource(l.Source) {
		return ErrInvalidStateTransition
	}
	now := nowUnix()
	if l.Ctime == 0 {
		l.Ctime = now
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"INSERT INTO pm_withdraw_log (msg_id, conversation_id, seq, sender_mid, operator_mid, source, reason, "+
			"audit_task_id, report_id, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		l.MsgID, l.ConversationID, l.Seq, l.SenderMid, l.OperatorMid, l.Source, l.Reason,
		l.AuditTaskID, l.ReportID, l.Ctime)
	if err != nil {
		return fmt.Errorf("pm_withdraw_log Insert: %w", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		l.LogID = id
	}
	return nil
}

func (m *defaultWithdrawLogModel) ListByMsgID(ctx context.Context, msgID int64) ([]*WithdrawLog, error) {
	var rows []*WithdrawLog
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+withdrawLogColumns+" FROM pm_withdraw_log WHERE msg_id = ? ORDER BY log_id ASC", msgID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_withdraw_log ListByMsgID: %w", err)
	}
	return rows, nil
}

func (m *defaultWithdrawLogModel) ListByOperator(ctx context.Context, operatorMid int64, ps int32) ([]*WithdrawLog, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	var rows []*WithdrawLog
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+withdrawLogColumns+" FROM pm_withdraw_log WHERE operator_mid = ? ORDER BY log_id DESC LIMIT ?",
		operatorMid, ps)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_withdraw_log ListByOperator: %w", err)
	}
	return rows, nil
}
