package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 操作者角色，与 danmaku_op_log.operator_role 列一致。
const (
	// RoleSelf 弹幕发送者本人。
	RoleSelf int32 = 1
	// RoleAdmin 运营/管理员。
	RoleAdmin int32 = 2
	// RoleSystem 系统（机审、事件消费）。
	RoleSystem int32 = 3
)

// 审计动作，与 danmaku_op_log.action 列一致。
const (
	// ActionPost 发送落库。
	ActionPost = "post"
	// ActionDelete 删除。
	ActionDelete = "delete"
	// ActionModeration 审核结论回写。
	ActionModeration = "moderation"
	// ActionReport 举报。
	ActionReport = "report"
)

// OpLog 弹幕操作留痕（danmaku_op_log 表）。
// 依据 AGENTS.md §8，删除/下架必须保留审计证据，因此弹幕主表只做软删除，
// 所有状态推进都在同一事务内写一条 op_log。
// event_id 用于 moderation.result.v1 消费去重：非事件驱动的写操作为 NULL，
// MySQL 唯一索引允许多个 NULL，因此不会互相冲突。
type OpLog struct {
	LogID        int64  `db:"log_id"`        // 自增主键
	Dmid         int64  `db:"dmid"`          // 弹幕 ID
	Action       string `db:"action"`        // 动作
	FromState    int32  `db:"from_state"`    // 迁移前状态
	ToState      int32  `db:"to_state"`      // 迁移后状态
	OperatorMid  int64  `db:"operator_mid"`  // 操作者
	OperatorRole int32  `db:"operator_role"` // 操作者角色
	Reason       string `db:"reason"`        // 原因说明
	EventID      string `db:"event_id"`      // 事件 ID（可空）
	TraceID      string `db:"trace_id"`      // 链路追踪 ID
	Ctime        int64  `db:"ctime"`         // 创建时间（Unix 秒）
}

// OpLogModel danmaku_op_log 表查询与写入接口。
type OpLogModel interface {
	// Insert 写入留痕，返回 log_id。
	Insert(ctx context.Context, l *OpLog) (int64, error)
	// InsertTx 在事务内写入留痕；event_id 重复会返回错误用于消费去重。
	InsertTx(ctx context.Context, session sqlx.Session, l *OpLog) (int64, error)
	// ExistsEvent 判断事件是否已消费（去重快查）。
	ExistsEvent(ctx context.Context, eventID string) (bool, error)
	// ListByDmid 按时间倒序查询某条弹幕的操作留痕。
	ListByDmid(ctx context.Context, dmid int64, limit int32) ([]*OpLog, error)
}

type defaultOpLogModel struct {
	conn sqlx.SqlConn
}

// NewOpLogModel 创建 OpLogModel 实现。
func NewOpLogModel(conn sqlx.SqlConn) OpLogModel {
	return &defaultOpLogModel{conn: conn}
}

const opLogInsert = "INSERT INTO danmaku_op_log (dmid, action, from_state, to_state, operator_mid, operator_role, reason, event_id, trace_id, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

func (m *defaultOpLogModel) Insert(ctx context.Context, l *OpLog) (int64, error) {
	return m.insert(ctx, m.conn, l)
}

func (m *defaultOpLogModel) InsertTx(ctx context.Context, session sqlx.Session, l *OpLog) (int64, error) {
	if session == nil {
		return m.insert(ctx, m.conn, l)
	}
	return m.insert(ctx, session, l)
}

// execer 抽象 sqlx.SqlConn 与 sqlx.Session 的 ExecCtx，避免复制插入逻辑。
type execer interface {
	ExecCtx(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func (m *defaultOpLogModel) insert(ctx context.Context, ex execer, l *OpLog) (int64, error) {
	var eventArg interface{}
	if l.EventID != "" {
		eventArg = l.EventID // 空串写 NULL，唯一索引不冲突
	}
	if l.Ctime == 0 {
		l.Ctime = nowUnix()
	}
	res, err := ex.ExecCtx(ctx, opLogInsert,
		l.Dmid, l.Action, l.FromState, l.ToState, l.OperatorMid, l.OperatorRole, l.Reason, eventArg, l.TraceID, l.Ctime)
	if err != nil {
		return 0, fmt.Errorf("danmaku_op_log Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("danmaku_op_log Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultOpLogModel) ExistsEvent(ctx context.Context, eventID string) (bool, error) {
	if eventID == "" {
		return false, nil
	}
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt, "SELECT COUNT(1) FROM danmaku_op_log WHERE event_id = ? LIMIT 1", eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("danmaku_op_log ExistsEvent: %w", err)
	}
	return cnt > 0, nil
}

func (m *defaultOpLogModel) ListByDmid(ctx context.Context, dmid int64, limit int32) ([]*OpLog, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var rows []*OpLog
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT log_id, dmid, action, from_state, to_state, operator_mid, operator_role, reason, IFNULL(event_id, '') AS event_id, trace_id, ctime FROM danmaku_op_log WHERE dmid = ? ORDER BY log_id DESC LIMIT ?",
		dmid, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku_op_log ListByDmid: %w", err)
	}
	return rows, nil
}
