package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// PendingDelivery 投递台账 / Outbox 行（ec_pending_delivery 表）。
//
// 语义（AGENTS.md §5）：接收事务只写「批次 + 事件台账 + 本表」，MQ 发送由 dispatcher
// 在事务外完成，因此本表是 Outbox——事件与投递意图同事务落库，进程崩溃也不会丢投递。
//
// 幂等：uniq_event_topic(event_id, topic) 保证同一事件对同一 topic 只有一行；
// 重放/回填不会在 MQ 前堆积重复行（下游仍按 event_id 去重，见 spm）。
// 并发：ClaimDue 用租约（lease_owner + lease_until）而非「查到就发」，
// 多个 cron 实例并行推进时同一行只会被一个持有者取走；worker 崩溃后租约到期可被接管。
type PendingDelivery struct {
	ID int64 `db:"id"`
	// EventID 台账事件 ID（ec_event_record.event_id）
	EventID string `db:"event_id"`
	// BatchID 所属批次（死信排查时回溯上报方）
	BatchID string `db:"batch_id"`
	// Topic 投递目标 topic，如 behavior.play.v1
	Topic string `db:"topic"`
	// EnvelopeEventID 信封 event_id（与入参 event_id 分开记账，下游按它去重）
	EnvelopeEventID string `db:"envelope_event_id"`
	// PayloadDigest 正文摘要（原文不入库，重放时按 event_id 回到对象存储取件）
	PayloadDigest string `db:"payload_digest"`
	// State 见 DeliveryState*（PENDING/RETRYING/SENT/DEAD）
	State int32 `db:"state"`
	// Attempts 已尝试次数
	Attempts int32 `db:"attempts"`
	// NextRetryAt 下次可投递时间（Unix 秒）
	NextRetryAt int64 `db:"next_retry_at"`
	// LeaseOwner 当前租约持有者（worker 标识），空表示未被占用
	LeaseOwner string `db:"lease_owner"`
	// LeaseUntil 租约到期时刻（Unix 秒），0 表示未占用
	LeaseUntil int64 `db:"lease_until"`
	// SentAt 投递成功时刻（Unix 秒，0 = 未成功）
	SentAt int64 `db:"sent_at"`
	// LastError 最近一次失败摘要（已脱敏）
	LastError string `db:"last_error"`
	Ctime     int64  `db:"ctime"`
	Mtime     int64  `db:"mtime"`
}

// TopicStat 单 topic 投递积压视图（GetCollectorHealth 的数据源之一）。
type TopicStat struct {
	Topic              string `db:"topic"`
	Pending            int64  `db:"pending"`
	Retrying           int64  `db:"retrying"`
	SentLastHour       int64  `db:"sent_last_hour"`
	OldestPendingCtime int64  `db:"oldest_pending_ctime"`
}

// PendingDeliveryModel ec_pending_delivery 读写接口。
type PendingDeliveryModel interface {
	// InsertIgnore 幂等入队（dispatcher 与 ReplayDeadLetter 共用）；已存在返回 false。
	InsertIgnore(ctx context.Context, session sqlx.Session, p *PendingDelivery) (created bool, err error)
	// InsertIgnoreMany 批量入队，返回新增行数。
	InsertIgnoreMany(ctx context.Context, session sqlx.Session, rows []*PendingDelivery) (int64, error)
	// ClaimDue 按租约领取到期行：把 state IN (PENDING,RETRYING) 且 next_retry_at <= now
	// 且租约空闲/过期的行标记为本 worker 持有（lease_until = until），返回这些行。
	ClaimDue(ctx context.Context, topic string, now, until int64, worker string, limit int32) ([]*PendingDelivery, error)
	// MarkSent 投递成功：state=SENT、释放租约；applied=false 表示租约已被接管。
	MarkSent(ctx context.Context, id int64, worker string, sentAt int64) (applied bool, err error)
	// MarkRetry 投递失败但还可重试：写 attempts/next_retry_at/退避中的 state。
	MarkRetry(ctx context.Context, id int64, worker string, attempts int32, nextRetryAt int64,
		lastError string) (applied bool, err error)
	// MarkDead 超过重试上限，等待调用方写 ec_dead_letter 后转 DEAD。
	MarkDead(ctx context.Context, id int64, worker string, attempts int32, lastError string) (applied bool, err error)
	// ReleaseLease 本轮无法继续投递时（如 dispatcher 未接线）把租约交还，
	// 不写 attempts、不改 state：未真正尝试过就不能消耗重试预算。
	ReleaseLease(ctx context.Context, id int64, worker string) (applied bool, err error)
	// Requeue 死信重放入队：只把 state=DEAD 的行放回 PENDING（清空 attempts/退避/租约），
	// applied=false 表示该行不在 DEAD（已在队列里或已发送），由调用方按「不重复入队」处理。
	// 刻意保留 envelope_event_id：下游按信封 event_id 去重，同一条事件重放仍归同一去重键，
	// 因此「已投递成功过的事件」不会被重放成第二条事实（AGENTS.md §5 幂等）。
	Requeue(ctx context.Context, session sqlx.Session, eventID, topic, lastError string) (applied bool, err error)
	// ReapExpiredLeases 回收过期租约（worker 崩溃后把 RETRYING 行放回可领取状态），返回行数。
	ReapExpiredLeases(ctx context.Context, now int64, limit int32) (int64, error)
	// TopicStats 按 topic 聚合积压与近一小时成功量。
	TopicStats(ctx context.Context, since int64) ([]TopicStat, error)
	// CountOpen 统计仍在途（PENDING/RETRYING）的行数。
	CountOpen(ctx context.Context) (int64, error)
	// ListByEventID 查某事件的全部投递行（排障：一条事件可能投多个 topic）。
	ListByEventID(ctx context.Context, eventID string) ([]*PendingDelivery, error)
	// DeleteSentBefore 清理已成功的历史投递行（台账瘦身，不影响死信与事件台账）。
	DeleteSentBefore(ctx context.Context, ctimeBefore int64, limit int32) (int64, error)
	// WithSession 绑定事务句柄。
	WithSession(session sqlx.Session) PendingDeliveryModel
}

type defaultPendingDeliveryModel struct {
	conn sqlx.SqlConn
}

// NewPendingDeliveryModel 创建 PendingDeliveryModel 实现。
func NewPendingDeliveryModel(conn sqlx.SqlConn) PendingDeliveryModel {
	return &defaultPendingDeliveryModel{conn: conn}
}

func (m *defaultPendingDeliveryModel) WithSession(session sqlx.Session) PendingDeliveryModel {
	return &defaultPendingDeliveryModel{conn: pick(session, m.conn)}
}

const pendingDeliveryColumns = `id, event_id, batch_id, topic, envelope_event_id, payload_digest, state, attempts,
	next_retry_at, lease_owner, lease_until, sent_at, last_error, ctime, mtime`

const pendingDeliveryInsertColumns = `event_id, batch_id, topic, envelope_event_id, payload_digest, state,
	attempts, next_retry_at, lease_owner, lease_until, sent_at, last_error, ctime, mtime`

// pendingDeliveryInsertValues 是入队 INSERT 的列数（不含自增主键 id）。
const pendingDeliveryInsertValues = 14

func (m *defaultPendingDeliveryModel) InsertIgnore(ctx context.Context, session sqlx.Session,
	p *PendingDelivery) (bool, error) {
	n, err := m.InsertIgnoreMany(ctx, session, []*PendingDelivery{p})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (m *defaultPendingDeliveryModel) InsertIgnoreMany(ctx context.Context, session sqlx.Session,
	rows []*PendingDelivery) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if len(rows) > maxStringIDList {
		return 0, ErrBatchTooLarge
	}
	db := pick(session, m.conn)
	now := nowUnix()
	args := make([]any, 0, len(rows)*pendingDeliveryInsertValues)
	values := make([]string, 0, len(rows))
	group := "(" + placeholders(pendingDeliveryInsertValues) + ")"
	for _, p := range rows {
		if p == nil || p.EventID == "" || p.Topic == "" {
			return 0, ErrEventIDRequired
		}
		if p.State == 0 {
			p.State = DeliveryStatePending
		}
		if !DeliveryStateOpen(p.State) && p.State != DeliveryStateSent && p.State != DeliveryStateDead {
			return 0, ErrInvalidStateTransition
		}
		if p.Ctime == 0 {
			p.Ctime, p.Mtime = now, now
		}
		args = append(args, p.EventID, p.BatchID, p.Topic, p.EnvelopeEventID, p.PayloadDigest, p.State,
			p.Attempts, p.NextRetryAt, p.LeaseOwner, p.LeaseUntil, p.SentAt, truncate(p.LastError, 512),
			p.Ctime, p.Mtime)
		values = append(values, group)
	}
	query := "INSERT IGNORE INTO ec_pending_delivery (" + pendingDeliveryInsertColumns + ") VALUES " +
		strings.Join(values, ",")
	res, err := db.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("ec_pending_delivery InsertIgnoreMany: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ec_pending_delivery InsertIgnoreMany RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultPendingDeliveryModel) ClaimDue(ctx context.Context, topic string, now, until int64,
	worker string, limit int32) ([]*PendingDelivery, error) {
	if worker == "" {
		return nil, ErrOperatorRequired
	}
	if limit <= 0 {
		return nil, ErrBatchLimitTooLarge
	}
	if limit > maxIDList {
		limit = maxIDList
	}
	if until <= now {
		// 租约必须比当前时间更晚，否则同一条会被并发的第二个 worker 立刻抢走。
		return nil, ErrInvalidStateTransition
	}
	extra := ""
	// 参数顺序严格对应 SQL 里的 ?：SET(3) → state IN(2) → next_retry_at → lease_until → [topic] → LIMIT。
	args := []any{worker, until, now, DeliveryStatePending, DeliveryStateRetrying, now, now, limit}
	if topic != "" {
		extra = " AND topic = ?"
		args = []any{worker, until, now, DeliveryStatePending, DeliveryStateRetrying, now, now, topic, limit}
	}
	// 第一步：抢占租约。WHERE 里的 (lease_until = 0 OR lease_until <= ?) 让崩溃 worker
	// 的行可被接管；affected 行可能少于 limit（有已被别人占的），属正常。
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE ec_pending_delivery SET lease_owner = ?, lease_until = ?, mtime = ? "+
			"WHERE state IN (?, ?) AND next_retry_at <= ? AND (lease_until = 0 OR lease_until <= ?)"+extra+
			" ORDER BY next_retry_at ASC, id ASC LIMIT ?", args...)
	if err != nil {
		return nil, fmt.Errorf("ec_pending_delivery ClaimDue update: %w", err)
	}
	// 第二步：把本 worker 刚拿到的行读出来（同一 lease_until 值即同一轮）。
	rows := make([]*PendingDelivery, 0, limit)
	query := "SELECT " + pendingDeliveryColumns + " FROM ec_pending_delivery WHERE lease_owner = ? AND lease_until = ?" +
		" AND state IN (?, ?)"
	if topic != "" {
		query += " AND topic = ?"
		if err := m.conn.QueryRowsCtx(ctx, &rows, query+" ORDER BY next_retry_at ASC, id ASC LIMIT ?",
			worker, until, DeliveryStatePending, DeliveryStateRetrying, topic, limit); err != nil {
			return nil, fmt.Errorf("ec_pending_delivery ClaimDue select: %w", err)
		}
		return rows, nil
	}
	if err := m.conn.QueryRowsCtx(ctx, &rows, query+" ORDER BY next_retry_at ASC, id ASC LIMIT ?",
		worker, until, DeliveryStatePending, DeliveryStateRetrying, limit); err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ec_pending_delivery ClaimDue select: %w", err)
	}
	return rows, nil
}

func (m *defaultPendingDeliveryModel) MarkSent(ctx context.Context, id int64, worker string,
	sentAt int64) (bool, error) {
	return m.finish(ctx, id, worker, DeliveryStateSent, 0, 0, sentAt, "")
}

func (m *defaultPendingDeliveryModel) MarkRetry(ctx context.Context, id int64, worker string, attempts int32,
	nextRetryAt int64, lastError string) (bool, error) {
	if nextRetryAt <= 0 {
		return false, ErrInvalidStateTransition
	}
	return m.finish(ctx, id, worker, DeliveryStateRetrying, attempts, nextRetryAt, 0, lastError)
}

func (m *defaultPendingDeliveryModel) MarkDead(ctx context.Context, id int64, worker string, attempts int32,
	lastError string) (bool, error) {
	return m.finish(ctx, id, worker, DeliveryStateDead, attempts, 0, 0, lastError)
}

// finish 统一的投递结果回写：owner 条件保证「只有当前租约持有者能改这行」，
// 租约被接管后旧 worker 的写入 applied=false，不会把已接管行覆盖成过期状态。
func (m *defaultPendingDeliveryModel) finish(ctx context.Context, id int64, worker string, state int32,
	attempts int32, nextRetryAt, sentAt int64, lastError string) (bool, error) {
	if id <= 0 || worker == "" {
		return false, ErrPendingNotFound
	}
	if !ValidDeliveryState(state) {
		return false, ErrInvalidStateTransition
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE ec_pending_delivery SET state = ?, attempts = ?, next_retry_at = ?, sent_at = ?, last_error = ?, "+
			"lease_owner = '', lease_until = 0, mtime = ? WHERE id = ? AND lease_owner = ?",
		state, attempts, nextRetryAt, sentAt, truncate(lastError, 512), nowUnix(), id, worker)
	if err != nil {
		return false, fmt.Errorf("ec_pending_delivery finish: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_pending_delivery finish RowsAffected: %w", err)
	}
	return n > 0, nil
}

func (m *defaultPendingDeliveryModel) ReleaseLease(ctx context.Context, id int64, worker string) (bool, error) {
	if id <= 0 || worker == "" {
		return false, ErrPendingNotFound
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE ec_pending_delivery SET lease_owner = '', lease_until = 0, mtime = ? "+
			"WHERE id = ? AND lease_owner = ? AND state IN (?, ?)",
		nowUnix(), id, worker, DeliveryStatePending, DeliveryStateRetrying)
	if err != nil {
		return false, fmt.Errorf("ec_pending_delivery ReleaseLease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_pending_delivery ReleaseLease RowsAffected: %w", err)
	}
	return n > 0, nil
}

// Requeue 死信重放入队：CAS 语义由 state=DEAD 条件保证 ——
// 两个操作员同时重放同一条时，只有一个能把行放回 PENDING，另一个 applied=false，
// 调用方据此计 skipped，不会把同一事件注入两次（AGENTS.md §5 幂等）。
// 不写 lease：重放入队后任何 worker 都可按 idx_claim 领取。
func (m *defaultPendingDeliveryModel) Requeue(ctx context.Context, session sqlx.Session,
	eventID, topic, lastError string) (bool, error) {
	if strings.TrimSpace(eventID) == "" || strings.TrimSpace(topic) == "" {
		return false, ErrEventIDRequired
	}
	db := pick(session, m.conn)
	res, err := db.ExecCtx(ctx,
		"UPDATE ec_pending_delivery SET state = ?, attempts = 0, next_retry_at = 0, lease_owner = '', "+
			"lease_until = 0, sent_at = 0, last_error = ?, mtime = ? "+
			"WHERE event_id = ? AND topic = ? AND state = ?",
		DeliveryStatePending, truncate(lastError, 512), nowUnix(), eventID, topic, DeliveryStateDead)
	if err != nil {
		return false, fmt.Errorf("ec_pending_delivery Requeue: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_pending_delivery Requeue RowsAffected: %w", err)
	}
	return n > 0, nil
}

func (m *defaultPendingDeliveryModel) ReapExpiredLeases(ctx context.Context, now int64, limit int32) (int64, error) {
	if now <= 0 {
		return 0, ErrInvalidPage
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > maxIDList {
		return 0, ErrBatchLimitTooLarge
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE ec_pending_delivery SET lease_owner = '', lease_until = 0, mtime = ? "+
			"WHERE lease_until <> 0 AND lease_until < ? AND state IN (?, ?) LIMIT ?",
		now, now, DeliveryStatePending, DeliveryStateRetrying, limit)
	if err != nil {
		return 0, fmt.Errorf("ec_pending_delivery ReapExpiredLeases: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ec_pending_delivery ReapExpiredLeases RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultPendingDeliveryModel) TopicStats(ctx context.Context, since int64) ([]TopicStat, error) {
	var rows []TopicStat
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT topic, SUM(state = ?) AS pending, SUM(state = ?) AS retrying, "+
			"SUM(state = ? AND sent_at >= ?) AS sent_last_hour, COALESCE(MIN(CASE WHEN state IN (?, ?) THEN ctime END), 0) AS oldest_pending_ctime "+
			"FROM ec_pending_delivery GROUP BY topic ORDER BY topic ASC",
		DeliveryStatePending, DeliveryStateRetrying, DeliveryStateSent, since,
		DeliveryStatePending, DeliveryStateRetrying)
	if err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ec_pending_delivery TopicStats: %w", err)
	}
	return rows, nil
}

func (m *defaultPendingDeliveryModel) CountOpen(ctx context.Context) (int64, error) {
	var total int64
	err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(1) FROM ec_pending_delivery WHERE state IN (?, ?)",
		DeliveryStatePending, DeliveryStateRetrying)
	if err != nil {
		return 0, fmt.Errorf("ec_pending_delivery CountOpen: %w", err)
	}
	return total, nil
}

func (m *defaultPendingDeliveryModel) ListByEventID(ctx context.Context, eventID string) ([]*PendingDelivery, error) {
	if eventID == "" {
		return nil, ErrEventIDRequired
	}
	var rows []*PendingDelivery
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+pendingDeliveryColumns+" FROM ec_pending_delivery WHERE event_id = ? ORDER BY id ASC LIMIT 50",
		eventID)
	if err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ec_pending_delivery ListByEventID: %w", err)
	}
	return rows, nil
}

func (m *defaultPendingDeliveryModel) DeleteSentBefore(ctx context.Context, ctimeBefore int64,
	limit int32) (int64, error) {
	if ctimeBefore <= 0 {
		return 0, ErrInvalidPage
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > maxIDList {
		return 0, ErrBatchLimitTooLarge
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM ec_pending_delivery WHERE state = ? AND ctime < ? LIMIT ?",
		DeliveryStateSent, ctimeBefore, limit)
	if err != nil {
		return 0, fmt.Errorf("ec_pending_delivery DeleteSentBefore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ec_pending_delivery DeleteSentBefore RowsAffected: %w", err)
	}
	return n, nil
}
