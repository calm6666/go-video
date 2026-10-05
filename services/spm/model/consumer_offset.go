package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ConsumerOffset 事件消费状态行（spm_consumer_offset 表投影）。
//
// 状态机：received -> processing -> succeeded
//
//	-> retry(退避) -> processing -> ... -> dead_letter
//
// 两个职责合在一张表里（docs/api-and-events.md §6）：
//  1. event_id 唯一键 = 幂等真值。重复、乱序、迟到消息只能读到既有状态，
//     绝不会把 succeeded 改回 processing，也就不会让事实表被写两遍；
//  2. topic/partition/offset = 已处理位点。回放与「消费到哪了」的可观测都靠它，
//     这是 spm 能重放的证据链（AGENTS.md §7 第 1 条）。
//
// 与 spm_behavior_event 的分工：本表记录「投递是否处理过」，事实表记录「脱敏后的事实」；
// 成功行的 payload 会被清空，因此本表不会长期堆积事件原文。
type ConsumerOffset struct {
	ID          int64  `db:"id"`            // 主键 ID
	EventID     string `db:"event_id"`      // 事件唯一 ID
	EventType   string `db:"event_type"`    // 事件类型
	Topic       string `db:"topic"`         // 来源 topic（含版本后缀）
	PartitionNo int32  `db:"partition_no"`  // Kafka 分区（kq 不上报时为 0）
	MsgOffset   int64  `db:"msg_offset"`    // Kafka 位点（kq 不上报时为 0）
	State       string `db:"state"`         // received/processing/succeeded/retry/dead_letter
	RetryCount  int32  `db:"retry_count"`   // 已失败次数
	NextRetryAt int64  `db:"next_retry_at"` // 退避到期时间（Unix 秒）
	LastError   string `db:"last_error"`    // 最近失败原因（脱敏截断）
	// Payload 只在失败路径写入（retry/dead_letter），成功行由 MarkSucceeded 清空：
	// 它让退避重投不依赖 Kafka 是否重投同一条消息，同时避免长期堆积原文。
	Payload     string `db:"payload"`      // 原始事件信封 JSON（可为空）
	OccurredAt  int64  `db:"occurred_at"`  // 事件发生时间（Unix 秒）
	ConsumeFrom string `db:"consume_from"` // 首次启动起点（first/last），位点可观测用
	Ctime       int64  `db:"ctime"`        // 首次收到时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 状态变更时间（Unix 秒）
}

// ClaimOutcome 领取事件的结果。
type ClaimOutcome int

const (
	// ClaimAcquired 获得处理权：首次投递、退避到期重投或崩溃后回收。
	ClaimAcquired ClaimOutcome = iota
	// ClaimDuplicate 事件已终结（succeeded/dead_letter）或类型不在处理范围：直接确认。
	ClaimDuplicate
	// ClaimDeferred 退避未到或有别的处理器正在执行：返回错误让 MQ 重投。
	ClaimDeferred
)

// String 便于日志与测试断言。
func (o ClaimOutcome) String() string {
	switch o {
	case ClaimAcquired:
		return "acquired"
	case ClaimDuplicate:
		return "duplicate"
	case ClaimDeferred:
		return "deferred"
	default:
		return "unknown"
	}
}

// ConsumerSummary 按 topic × 状态汇总的消费视图（ListConsumerState 的数据源）。
type ConsumerSummary struct {
	Topic         string `db:"topic"`
	State         string `db:"state"`
	RowCount      int64  `db:"row_count"`
	OldestCtime   int64  `db:"oldest_ctime"`
	LastMsgOffset int64  `db:"last_msg_offset"`
	LastEventTime int64  `db:"last_event_time"`
}

// ConsumerOffsetModel spm_consumer_offset 表读写接口。
type ConsumerOffsetModel interface {
	// Claim 按 event_id 领取处理权。staleSeconds 回收进程崩溃后停留在 processing 的事件。
	// 返回的 offset 带当前 retry_count，调用方据此决定继续退避还是判死。
	Claim(ctx context.Context, ev *ConsumerOffset, staleSeconds int64) (ClaimOutcome, *ConsumerOffset, error)
	// MarkSucceeded 标记处理完成并清空暂存的 payload。
	MarkSucceeded(ctx context.Context, eventID string) error
	// MarkRetry 标记失败并写入退避到期时间、原因与供重投的 payload。
	MarkRetry(ctx context.Context, eventID string, nextRetryAt int64, reason, payload string) error
	// MarkDeadLetter 标记判死（配合 spm_dead_letter 留档），保留 payload 以便人工重放。
	MarkDeadLetter(ctx context.Context, eventID, reason, payload string) error
	// FindOne 查询事件状态（不含 payload）；不存在返回 nil。
	FindOne(ctx context.Context, eventID string) (*ConsumerOffset, error)
	// FindWithPayload 查询事件状态并带出 payload（重放路径专用）。
	FindWithPayload(ctx context.Context, eventID string) (*ConsumerOffset, error)
	// ListOverdueRetry 列出退避到期待重投的事件（state=retry 且 next_retry_at<=now），含 payload。
	ListOverdueRetry(ctx context.Context, now int64, limit int32) ([]*ConsumerOffset, error)
	// Summarize 按 topic × 状态汇总消费情况，供 ListConsumerState 使用。
	// since>0 是 ctime 下界：GROUP BY 要回表取 msg_offset/ctime（这两个列不在
	// idx_topic_state 里），没有下界就等于对整个状态表做一次扫描。
	// 调用方给的下界应当不晚于 DeleteSettledBefore 的清理水位，否则会把仍在库里的
	// 堆积算没（见 README「数据保留策略」）。
	Summarize(ctx context.Context, topic string, states []string, since int64,
		offset, limit int32) ([]*ConsumerSummary, error)
	// CountByState 按 topic × 状态统计条数（Summarize 的 total），与 Summarize 同下界。
	CountByState(ctx context.Context, topic string, states []string, since int64) (int64, error)
	// DeleteSettledBefore 清理 ctime 早于 before 且已终结（succeeded）的记录，单批 limit 行。
	// 只清成功行：retry/dead_letter 是待处理与留档证据，位点也还没稳定。
	DeleteSettledBefore(ctx context.Context, before int64, limit int32) (int64, error)
}

type defaultConsumerOffsetModel struct {
	conn sqlx.SqlConn
}

// NewConsumerOffsetModel 创建 ConsumerOffsetModel 实现。
func NewConsumerOffsetModel(conn sqlx.SqlConn) ConsumerOffsetModel {
	return &defaultConsumerOffsetModel{conn: conn}
}

// consumerOffsetColumns 是状态判定路径需要的轻量列（不含 payload）。
const consumerOffsetColumns = "id, event_id, event_type, topic, partition_no, msg_offset, state," +
	" retry_count, next_retry_at, last_error, occurred_at, consume_from, ctime, mtime"

// consumerOffsetFullColumns 额外带出 payload，只在重投/重放路径使用。
// COALESCE 兜住 NULL，让结构体直接映射成 string，不必引入 sql.NullString。
const consumerOffsetFullColumns = consumerOffsetColumns + ", COALESCE(payload, '') AS payload"

// maxRetryBatch 是单轮退避扫描读取的 payload 条数上限（见 ListOverdueRetry）。
const maxRetryBatch int32 = 500

// nullPayload 把空串写成 NULL，避免成功/首发路径在 TEXT 列里堆积空值。
func nullPayload(payload string) any {
	if payload == "" {
		return nil
	}
	return payload
}

// ValidConsumerState 判断状态字符串是否属于状态机集合。
func ValidConsumerState(s string) bool {
	switch s {
	case ConsumerStateReceived, ConsumerStateProcessing, ConsumerStateSucceeded,
		ConsumerStateRetry, ConsumerStateDeadLetter:
		return true
	default:
		return false
	}
}

func (m *defaultConsumerOffsetModel) Claim(
	ctx context.Context, ev *ConsumerOffset, staleSeconds int64,
) (ClaimOutcome, *ConsumerOffset, error) {
	if strings.TrimSpace(ev.EventID) == "" {
		return ClaimDeferred, nil, ErrEventIDEmpty
	}
	if staleSeconds <= 0 {
		staleSeconds = 300
	}
	now := nowUnix()

	// 第一次投递：直接以 processing 落库。ON DUPLICATE KEY UPDATE 的自赋值
	// 让重复键场景 affected=0，从而区分「新事件」与「已存在事件」。
	// payload 在首次插入就写入：否则进程在 claim 与回写之间被杀掉，
	// 这条事件就没有可重放的原文了（失败后由 MarkSucceeded 清空）。
	query := "INSERT INTO spm_consumer_offset (event_id, event_type, topic, partition_no," +
		" msg_offset, state, retry_count, next_retry_at, last_error, payload, occurred_at," +
		" consume_from, ctime, mtime)" +
		" VALUES (?, ?, ?, ?, ?, ?, 0, 0, '', ?, ?, ?, ?, ?)" +
		" ON DUPLICATE KEY UPDATE event_id = event_id"
	res, err := m.conn.ExecCtx(ctx, query,
		ev.EventID, ev.EventType, ev.Topic, ev.PartitionNo, ev.MsgOffset,
		ConsumerStateProcessing, nullPayload(ev.Payload), ev.OccurredAt,
		truncate(ev.ConsumeFrom, 16), now, now)
	if err != nil {
		return ClaimDeferred, nil, fmt.Errorf("spm_consumer_offset Claim insert: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ClaimDeferred, nil, fmt.Errorf("spm_consumer_offset Claim RowsAffected: %w", err)
	}
	if affected == 1 {
		ev.State = ConsumerStateProcessing
		ev.Ctime, ev.Mtime = now, now
		return ClaimAcquired, ev, nil
	}

	// 已存在：只有「退避到期的 received/retry」或「崩溃遗留的过期 processing」可被抢占。
	// 条件写进 WHERE 而不是先读后写，避免两个处理器同时执行同一事件。
	res, err = m.conn.ExecCtx(ctx,
		"UPDATE spm_consumer_offset SET state = ?, mtime = ?, partition_no = ?, msg_offset = ?"+
			" WHERE event_id = ?"+
			" AND ((state IN (?, ?) AND next_retry_at <= ?) OR (state = ? AND mtime < ?))",
		ConsumerStateProcessing, now, ev.PartitionNo, ev.MsgOffset, ev.EventID,
		ConsumerStateReceived, ConsumerStateRetry, now,
		ConsumerStateProcessing, now-staleSeconds)
	if err != nil {
		return ClaimDeferred, nil, fmt.Errorf("spm_consumer_offset Claim reclaim: %w", err)
	}
	affected, err = res.RowsAffected()
	if err != nil {
		return ClaimDeferred, nil, fmt.Errorf("spm_consumer_offset Claim reclaim RowsAffected: %w", err)
	}
	if affected == 1 {
		row, err := m.FindOne(ctx, ev.EventID)
		if err != nil {
			return ClaimDeferred, nil, err
		}
		return ClaimAcquired, row, nil
	}

	// 抢占失败：按既有状态判定是重复还是延后。
	row, err := m.FindOne(ctx, ev.EventID)
	if err != nil {
		return ClaimDeferred, nil, err
	}
	if row == nil {
		// 极端情况：被并发清理任务删除，视为需要重投。
		return ClaimDeferred, nil, ErrConflictProcessing
	}
	switch row.State {
	case ConsumerStateSucceeded, ConsumerStateDeadLetter:
		return ClaimDuplicate, row, nil
	default:
		return ClaimDeferred, row, nil
	}
}

func (m *defaultConsumerOffsetModel) MarkSucceeded(ctx context.Context, eventID string) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE spm_consumer_offset SET state = ?, next_retry_at = 0, last_error = '',"+
			" payload = NULL, mtime = ? WHERE event_id = ?",
		ConsumerStateSucceeded, nowUnix(), eventID)
	if err != nil {
		return fmt.Errorf("spm_consumer_offset MarkSucceeded: %w", err)
	}
	return nil
}

func (m *defaultConsumerOffsetModel) MarkRetry(
	ctx context.Context, eventID string, nextRetryAt int64, reason, payload string,
) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE spm_consumer_offset SET state = ?, retry_count = retry_count + 1,"+
			" next_retry_at = ?, last_error = ?, payload = ?, mtime = ? WHERE event_id = ?",
		ConsumerStateRetry, nextRetryAt, truncate(reason, 512), nullPayload(payload), nowUnix(),
		eventID)
	if err != nil {
		return fmt.Errorf("spm_consumer_offset MarkRetry: %w", err)
	}
	return nil
}

func (m *defaultConsumerOffsetModel) MarkDeadLetter(
	ctx context.Context, eventID, reason, payload string,
) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE spm_consumer_offset SET state = ?, last_error = ?, payload = ?, mtime = ?"+
			" WHERE event_id = ?",
		ConsumerStateDeadLetter, truncate(reason, 512), nullPayload(payload), nowUnix(), eventID)
	if err != nil {
		return fmt.Errorf("spm_consumer_offset MarkDeadLetter: %w", err)
	}
	return nil
}

func (m *defaultConsumerOffsetModel) FindOne(ctx context.Context, eventID string) (*ConsumerOffset, error) {
	var row ConsumerOffset
	query := "SELECT " + consumerOffsetColumns + " FROM spm_consumer_offset WHERE event_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_consumer_offset FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultConsumerOffsetModel) FindWithPayload(
	ctx context.Context, eventID string,
) (*ConsumerOffset, error) {
	var row ConsumerOffset
	query := "SELECT " + consumerOffsetFullColumns + " FROM spm_consumer_offset WHERE event_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_consumer_offset FindWithPayload: %w", err)
	}
	return &row, nil
}

func (m *defaultConsumerOffsetModel) ListOverdueRetry(
	ctx context.Context, now int64, limit int32,
) ([]*ConsumerOffset, error) {
	// 重投队列按退避到期时间先进先出。批大小单独设上限而不用 clampBatch：
	// 这一行的 LIMIT 限制的是「读进内存的 payload 总量」（含事件原文），
	// 2000 条原文一次进堆会直接把消费者打爆，500 条已经够一轮退避扫描。
	query := "SELECT " + consumerOffsetFullColumns +
		" FROM spm_consumer_offset WHERE state = ? AND next_retry_at <= ?" +
		" ORDER BY next_retry_at ASC LIMIT ?"
	if limit <= 0 || limit > maxRetryBatch {
		limit = maxRetryBatch
	}
	var rows []*ConsumerOffset
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, ConsumerStateRetry, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_consumer_offset ListOverdueRetry: %w", err)
	}
	return rows, nil
}

// buildSummaryFilter 组装汇总条件：按 topic、状态与 ctime 下界过滤。
// 下界是必需的而不是优化：汇总要回表读 msg_offset/occurred_at，无下界的 GROUP BY
// 会扫过整张状态表（含已成功但尚未清理的历史行）。
func buildSummaryFilter(topic string, states []string, since int64) (string, []any) {
	clause := " WHERE 1 = 1"
	var args []any
	if topic != "" {
		clause += " AND topic = ?"
		args = append(args, topic)
	}
	if len(states) > 0 {
		holders := make([]string, 0, len(states))
		for range states {
			holders = append(holders, "?")
		}
		clause += " AND state IN (" + strings.Join(holders, ",") + ")"
		for _, s := range states {
			args = append(args, s)
		}
	}
	if since > 0 {
		clause += " AND ctime >= ?"
		args = append(args, since)
	}
	return clause, args
}

func (m *defaultConsumerOffsetModel) Summarize(
	ctx context.Context, topic string, states []string, since int64, offset, limit int32,
) ([]*ConsumerSummary, error) {
	clause, args := buildSummaryFilter(topic, states, since)
	query := "SELECT topic, state, COUNT(*) AS row_count, MIN(ctime) AS oldest_ctime," +
		" MAX(msg_offset) AS last_msg_offset, MAX(occurred_at) AS last_event_time" +
		" FROM spm_consumer_offset" + clause +
		" GROUP BY topic, state ORDER BY topic ASC, state ASC LIMIT ? OFFSET ?"
	args = append(args, clampLimit(limit), clampOffset(offset))
	var rows []*ConsumerSummary
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_consumer_offset Summarize: %w", err)
	}
	return rows, nil
}

func (m *defaultConsumerOffsetModel) CountByState(
	ctx context.Context, topic string, states []string, since int64,
) (int64, error) {
	clause, args := buildSummaryFilter(topic, states, since)
	var n int64
	// total 是「topic × 状态」组合数而不是事件条数：分页面向的是汇总行。
	query := "SELECT COUNT(*) FROM (SELECT 1 FROM spm_consumer_offset" + clause +
		" GROUP BY topic, state) t"
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		return 0, fmt.Errorf("spm_consumer_offset CountByState: %w", err)
	}
	return n, nil
}

func (m *defaultConsumerOffsetModel) DeleteSettledBefore(
	ctx context.Context, before int64, limit int32,
) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM spm_consumer_offset WHERE state = ? AND ctime < ? ORDER BY id ASC LIMIT ?",
		ConsumerStateSucceeded, before, clampBatch(limit))
	if err != nil {
		return 0, fmt.Errorf("spm_consumer_offset DeleteSettledBefore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_consumer_offset DeleteSettledBefore RowsAffected: %w", err)
	}
	return n, nil
}
