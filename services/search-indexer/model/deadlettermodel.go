package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SearchDeadLetter 死信登记（search_dead_letter 表）。
// 事件退避重试用尽、或信封格式错误无法解析时写入本表，等待人工/工具重放。
// 只保存 payload 摘要（sha256 前 16 字节 hex）而不是原文，避免长期堆积业务数据；
// 需要重放时按 event_id 回到 Kafka topic 指定 offset 重投（README 记录运维步骤）。
// 保留策略：state=open 的记录不得自动删除；超过 DLQRetentionDays 的终态记录
// 由 services/cron 归档后清理，清理动作必须可审计（AGENTS.md §8）。
type SearchDeadLetter struct {
	ID            int64  `db:"id"`             // 自增主键
	EventID       string `db:"event_id"`       // 事件 ID
	EventType     string `db:"event_type"`     // 事件类型
	Topic         string `db:"topic"`          // 来源 topic
	PayloadDigest string `db:"payload_digest"` // payload 摘要（sha256 hex 前 32 字符）
	Reason        string `db:"reason"`         // 死信原因（脱敏，不含堆栈与密钥）
	State         string `db:"state"`          // open/replayed/discarded
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// SearchDeadLetterModel search_dead_letter 表查询与写入接口。
type SearchDeadLetterModel interface {
	// Insert 登记死信；同一 event_id 已存在时不覆盖（uniq_event_id），返回 existed=false。
	Insert(ctx context.Context, d *SearchDeadLetter) (existed bool, err error)
	// FindByEventID 按 event_id 查询；不存在返回 (nil, nil)。
	FindByEventID(ctx context.Context, eventID string) (*SearchDeadLetter, error)
	// ListOpen 分页列出待处理死信（按 id 升序，运维重放顺序）。
	ListOpen(ctx context.Context, limit int) ([]*SearchDeadLetter, error)
	// UpdateState 更新死信处理状态（replayed/discarded）。
	UpdateState(ctx context.Context, eventID, state string, now int64) error
	// Count 统计死信总数；state 为空表示全部。
	Count(ctx context.Context, state string) (int64, error)
}

type defaultDeadLetterModel struct {
	conn sqlx.SqlConn
}

// NewSearchDeadLetterModel 创建 SearchDeadLetterModel 实现。
func NewSearchDeadLetterModel(conn sqlx.SqlConn) SearchDeadLetterModel {
	return &defaultDeadLetterModel{conn: conn}
}

const deadLetterColumns = "id, event_id, event_type, topic, payload_digest, reason, state, ctime, mtime"

func (m *defaultDeadLetterModel) Insert(ctx context.Context, d *SearchDeadLetter) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT IGNORE INTO search_dead_letter (event_id, event_type, topic, payload_digest, reason, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		d.EventID, d.EventType, d.Topic, d.PayloadDigest, d.Reason, d.State, d.Ctime, d.Mtime)
	if err != nil {
		return false, fmt.Errorf("search_dead_letter Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("search_dead_letter Insert RowsAffected: %w", err)
	}
	return aff == 0, nil
}

func (m *defaultDeadLetterModel) FindByEventID(ctx context.Context, eventID string) (*SearchDeadLetter, error) {
	var d SearchDeadLetter
	query := "SELECT " + deadLetterColumns + " FROM search_dead_letter WHERE event_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &d, query, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_dead_letter FindByEventID: %w", err)
	}
	return &d, nil
}

func (m *defaultDeadLetterModel) ListOpen(ctx context.Context, limit int) ([]*SearchDeadLetter, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var rows []*SearchDeadLetter
	query := "SELECT " + deadLetterColumns + " FROM search_dead_letter WHERE state = ? ORDER BY id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, DLQStateOpen, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_dead_letter ListOpen: %w", err)
	}
	return rows, nil
}

func (m *defaultDeadLetterModel) UpdateState(ctx context.Context, eventID, state string, now int64) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE search_dead_letter SET state = ?, mtime = ? WHERE event_id = ?", state, now, eventID)
	if err != nil {
		return fmt.Errorf("search_dead_letter UpdateState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("search_dead_letter UpdateState RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrEventNotFound
	}
	return nil
}

func (m *defaultDeadLetterModel) Count(ctx context.Context, state string) (int64, error) {
	var (
		total int64
		err   error
	)
	if state == "" {
		err = m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM search_dead_letter")
	} else {
		err = m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM search_dead_letter WHERE state = ?", state)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("search_dead_letter Count: %w", err)
	}
	return total, nil
}
