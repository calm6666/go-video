package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// DeadLetter 投递死信（ec_dead_letter 表）。
//
// 何时产生：ec_pending_delivery.attempts 达到策略 deliver_max_attempts，
// dispatcher 把行转 DEAD 并写本表；死信只存摘要与失败原因分类，事件原文不入库。
//
// 处置语义（proto ReplayDeadLetterReq 注释）：
//   - state=open 才能重放；replayed/discarded 是终态，重复重放计入 skipped；
//   - operator + replay_key + replay_reason 三列共同构成审计证据：谁、凭哪次请求、为什么重放；
//   - uniq_event_topic(event_id, topic) 使「同一条死信被重放多次」最多留一行，
//     重放的实际幂等由 replay_key + 重新入队 ec_pending_delivery 的唯一键保证。
type DeadLetter struct {
	ID int64 `db:"id"`
	// EventID 台账事件 ID
	EventID string `db:"event_id"`
	// BatchID 所属批次
	BatchID string `db:"batch_id"`
	// EventType 事件类型
	EventType string `db:"event_type"`
	// Topic 目标 topic
	Topic string `db:"topic"`
	// PayloadDigest 正文摘要（原文不入库）
	PayloadDigest string `db:"payload_digest"`
	// Reason 失败原因分类（稳定枚举串，如 mq_timeout / mq_auth / payload_oversize）
	Reason string `db:"reason"`
	// ReasonDetail 已脱敏的错误摘要
	ReasonDetail string `db:"reason_detail"`
	// Attempts 死信前的投递尝试次数
	Attempts int32 `db:"attempts"`
	// State open / replayed / discarded
	State string `db:"state"`
	// CreatedAt 死信生成时刻（Unix 秒）
	CreatedAt int64 `db:"created_at"`
	// HandledAt 处置时刻（Unix 秒，0 = 未处置）
	HandledAt int64 `db:"handled_at"`
	// Operator 处置人（服务账号或运营 ID）
	Operator string `db:"operator"`
	// ReplayKey 重放请求的 idempotency_key
	ReplayKey string `db:"replay_key"`
	// ReplayReason 重放/废弃理由（必填，proto ReplayDeadLetterReq.reason）
	ReplayReason string `db:"replay_reason"`
	Ctime        int64  `db:"ctime"`
	Mtime        int64  `db:"mtime"`
}

// DeadLetterModel ec_dead_letter 读写接口。
type DeadLetterModel interface {
	// InsertIgnore 写入死信；uniq_event_topic 已存在返回 false（重复死信不覆盖首次原因）。
	InsertIgnore(ctx context.Context, session sqlx.Session, d *DeadLetter) (created bool, err error)
	// FindByID 查询死信；不存在返回 ErrDeadLetterNotFound。
	FindByID(ctx context.Context, id int64) (*DeadLetter, error)
	// ListByIDs 批量回查死信（重放路径需要知道每行的 state/topic/event_id 才能逐条入队）。
	// 返回顺序按 id 升序，缺失的 ID 不会出现在结果里，由调用方按「不存在」计数。
	ListByIDs(ctx context.Context, session sqlx.Session, ids []int64) ([]*DeadLetter, error)
	// MarkHandled 把 open 死信迁移到终态（replayed/discarded），返回实际迁移行数；
	// 已处置的行不会被覆盖（WHERE state=open），因此 skipped = len(ids) - 返回行数。
	MarkHandled(ctx context.Context, session sqlx.Session, ids []int64, to, operator, replayKey,
		reason string, handledAt int64) (applied int64, err error)
	// List 按 (ctime, id) 倒序游标翻页；state 空表示全部。
	List(ctx context.Context, topic, state string, ctimeFrom, ctimeTo int64, cur Cursor,
		ps int32) ([]*DeadLetter, error)
	// Count 同条件计数。
	Count(ctx context.Context, topic, state string, ctimeFrom, ctimeTo int64) (int64, error)
	// CountOpenByTopic 统计各 topic 未处置死信数（GetCollectorHealth.dead_open）。
	CountOpenByTopic(ctx context.Context) (map[string]int64, error)
	// DeleteBefore 按死信保留天数清理（只清理已处置终态行，open 行必须人工处置）。
	DeleteBefore(ctx context.Context, ctimeBefore int64, limit int32) (int64, error)
	// WithSession 绑定事务句柄。
	WithSession(session sqlx.Session) DeadLetterModel
}

type defaultDeadLetterModel struct {
	conn sqlx.SqlConn
}

// NewDeadLetterModel 创建 DeadLetterModel 实现。
func NewDeadLetterModel(conn sqlx.SqlConn) DeadLetterModel {
	return &defaultDeadLetterModel{conn: conn}
}

func (m *defaultDeadLetterModel) WithSession(session sqlx.Session) DeadLetterModel {
	return &defaultDeadLetterModel{conn: pick(session, m.conn)}
}

const deadLetterColumns = `id, event_id, batch_id, event_type, topic, payload_digest, reason, reason_detail,
	attempts, state, created_at, handled_at, operator, replay_key, replay_reason, ctime, mtime`

// deadLetterInsertValues 是 InsertIgnore 的列数（不含自增主键 id）。
const deadLetterInsertValues = 16

func (m *defaultDeadLetterModel) InsertIgnore(ctx context.Context, session sqlx.Session,
	d *DeadLetter) (bool, error) {
	if d == nil || d.EventID == "" || d.Topic == "" {
		return false, ErrEventIDRequired
	}
	db := pick(session, m.conn)
	now := nowUnix()
	if d.State == "" {
		d.State = DeadLetterOpen
	}
	if !ValidDeadLetterState(d.State) {
		return false, ErrInvalidStateTransition
	}
	if d.CreatedAt == 0 {
		d.CreatedAt = now
	}
	if d.Ctime == 0 {
		d.Ctime, d.Mtime = now, now
	}
	res, err := db.ExecCtx(ctx,
		"INSERT IGNORE INTO ec_dead_letter (event_id, batch_id, event_type, topic, payload_digest, reason, "+
			"reason_detail, attempts, state, created_at, handled_at, operator, replay_key, replay_reason, ctime, mtime) "+
			"VALUES ("+placeholders(deadLetterInsertValues)+")",
		d.EventID, d.BatchID, d.EventType, d.Topic, d.PayloadDigest, truncate(d.Reason, 64),
		truncate(d.ReasonDetail, 512), d.Attempts, d.State, d.CreatedAt, d.HandledAt, truncate(d.Operator, 64),
		truncate(d.ReplayKey, 128), truncate(d.ReplayReason, 512), d.Ctime, d.Mtime)
	if err != nil {
		return false, fmt.Errorf("ec_dead_letter InsertIgnore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_dead_letter InsertIgnore RowsAffected: %w", err)
	}
	return n > 0, nil
}

func (m *defaultDeadLetterModel) FindByID(ctx context.Context, id int64) (*DeadLetter, error) {
	if id <= 0 {
		return nil, ErrDeadLetterNotFound
	}
	var row DeadLetter
	err := m.conn.QueryRowCtx(ctx, &row,
		"SELECT "+deadLetterColumns+" FROM ec_dead_letter WHERE id = ? LIMIT 1", id)
	if err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, ErrDeadLetterNotFound
		}
		return nil, fmt.Errorf("ec_dead_letter FindByID: %w", err)
	}
	return &row, nil
}

func (m *defaultDeadLetterModel) ListByIDs(ctx context.Context, session sqlx.Session,
	ids []int64) ([]*DeadLetter, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > maxIDList {
		return nil, ErrBatchLimitTooLarge
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, ErrDeadLetterNotFound
		}
		args = append(args, id)
	}
	db := pick(session, m.conn)
	var rows []*DeadLetter
	query := "SELECT " + deadLetterColumns + " FROM ec_dead_letter WHERE id IN (" +
		placeholders(len(ids)) + ") ORDER BY id ASC"
	if err := db.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ec_dead_letter ListByIDs: %w", err)
	}
	return rows, nil
}

func (m *defaultDeadLetterModel) MarkHandled(ctx context.Context, session sqlx.Session, ids []int64, to, operator,
	replayKey, reason string, handledAt int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > maxIDList {
		return 0, ErrBatchLimitTooLarge
	}
	// 终态只能从 open 迁移；且必须留下操作人与理由，否则审计链断（proto 要求 reason 必填）。
	if !ValidDeadLetterState(to) || to == DeadLetterOpen {
		return 0, ErrInvalidStateTransition
	}
	if strings.TrimSpace(operator) == "" || strings.TrimSpace(reason) == "" {
		return 0, ErrOperatorRequired
	}
	db := pick(session, m.conn)
	args := make([]any, 0, len(ids)+6)
	args = append(args, to, truncate(operator, 64), truncate(replayKey, 128), truncate(reason, 512),
		handledAt, nowUnix())
	query := "UPDATE ec_dead_letter SET state = ?, operator = ?, replay_key = ?, replay_reason = ?, handled_at = ?, " +
		"mtime = ? WHERE state = ? AND id IN (" + placeholders(len(ids)) + ")"
	args = append(args, DeadLetterOpen)
	for _, id := range ids {
		if id <= 0 {
			return 0, ErrDeadLetterNotFound
		}
		args = append(args, id)
	}
	res, err := db.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("ec_dead_letter MarkHandled: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ec_dead_letter MarkHandled RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultDeadLetterModel) where(topic, state string, ctimeFrom, ctimeTo int64) (string, []any) {
	conds := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if topic != "" {
		conds = append(conds, "topic = ?")
		args = append(args, topic)
	}
	if state != "" {
		if !ValidDeadLetterState(state) {
			return "", nil
		}
		conds = append(conds, "state = ?")
		args = append(args, state)
	}
	if ctimeFrom > 0 {
		conds = append(conds, "ctime >= ?")
		args = append(args, ctimeFrom)
	}
	if ctimeTo > 0 {
		conds = append(conds, "ctime <= ?")
		args = append(args, ctimeTo)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (m *defaultDeadLetterModel) List(ctx context.Context, topic, state string, ctimeFrom, ctimeTo int64,
	cur Cursor, ps int32) ([]*DeadLetter, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	where, args := m.where(topic, state, ctimeFrom, ctimeTo)
	if args == nil && (state != "" && !ValidDeadLetterState(state)) {
		return nil, ErrInvalidStateTransition
	}
	if cur.ID > 0 {
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += "(ctime < ? OR (ctime = ? AND id < ?))"
		args = append(args, cur.Ctime, cur.Ctime, cur.ID)
	}
	var rows []*DeadLetter
	query := "SELECT " + deadLetterColumns + " FROM ec_dead_letter" + where + " ORDER BY ctime DESC, id DESC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, append(args, ps)...); err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ec_dead_letter List: %w", err)
	}
	return rows, nil
}

func (m *defaultDeadLetterModel) Count(ctx context.Context, topic, state string, ctimeFrom, ctimeTo int64) (int64, error) {
	where, args := m.where(topic, state, ctimeFrom, ctimeTo)
	if args == nil && state != "" {
		return 0, ErrInvalidStateTransition
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(1) FROM ec_dead_letter"+where, args...); err != nil {
		return 0, fmt.Errorf("ec_dead_letter Count: %w", err)
	}
	return total, nil
}

func (m *defaultDeadLetterModel) CountOpenByTopic(ctx context.Context) (map[string]int64, error) {
	type row struct {
		Topic string `db:"topic"`
		Total int64  `db:"total"`
	}
	var rows []*row
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT topic, COUNT(1) AS total FROM ec_dead_letter WHERE state = ? GROUP BY topic", DeadLetterOpen)
	if err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return map[string]int64{}, nil
		}
		return nil, fmt.Errorf("ec_dead_letter CountOpenByTopic: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Topic] = r.Total
	}
	return out, nil
}

func (m *defaultDeadLetterModel) DeleteBefore(ctx context.Context, ctimeBefore int64, limit int32) (int64, error) {
	if ctimeBefore <= 0 {
		return 0, ErrInvalidPage
	}
	if limit <= 0 {
		limit = 500
	}
	if limit > maxIDList {
		return 0, ErrBatchLimitTooLarge
	}
	// 只删已处置终态：open 死信必须人工 Replay/Discard 后才可清理，保留审计证据（AGENTS.md §8）。
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM ec_dead_letter WHERE state <> ? AND ctime < ? LIMIT ?", DeadLetterOpen, ctimeBefore, limit)
	if err != nil {
		return 0, fmt.Errorf("ec_dead_letter DeleteBefore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ec_dead_letter DeleteBefore RowsAffected: %w", err)
	}
	return n, nil
}
