package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// DeadLetter 死信留档行（spm_dead_letter 表投影）。
//
// 隐私留档形态：只存 sha256 摘要与「脱敏前缀」，绝不存事件原文（AGENTS.md §6、§7）。
// 需要重放时，靠 event_id 回上游（event-collector 的接收库/ Kafka topic）取原文，
// 而不是在本服务里保有一份可回放的事件副本。
type DeadLetter struct {
	ID             int64  `db:"id"`              // 主键 ID
	EventID        string `db:"event_id"`        // 信封 event_id；信封不可解析时为空串
	EventType      string `db:"event_type"`      // 事件类型（不可解析时为空串）
	Topic          string `db:"topic"`           // 来源 topic
	PartitionNo    int32  `db:"partition_no"`    // Kafka 分区
	MsgOffset      int64  `db:"msg_offset"`      // Kafka 位点（重放定位）
	PayloadDigest  string `db:"payload_digest"`  // sha256:<hex>，比对内容是否同一条
	PayloadPreview string `db:"payload_preview"` // 脱敏前缀，不含行为原文与标识符
	Reason         string `db:"reason"`          // 判死原因（稳定错误名，不做人读拼接）
	ErrorCount     int32  `db:"error_count"`     // 累计失败次数
	State          string `db:"state"`           // open/replayed/ignored
	Operator       string `db:"operator"`        // 处理人（重放/忽略留痕）
	ProcessedAt    int64  `db:"processed_at"`    // 人工处理时间（Unix 秒）
	Ctime          int64  `db:"ctime"`           // 创建时间（Unix 秒）
	Mtime          int64  `db:"mtime"`           // 修改时间（Unix 秒）
}

// DeadLetterFilter 死信列表过滤条件。零值表示不限。
type DeadLetterFilter struct {
	Topic  string
	State  string
	Since  int64
	Offset int32
	Limit  int32
}

// DeadLetterModel spm_dead_letter 表读写接口。
type DeadLetterModel interface {
	// InsertIfAbsent 按 uniq_event_id 留档。返回 false 表示该事件已有死信记录。
	// event_id 为空（信封不可解析）时退化成按 payload_digest 去重，避免同一坏消息刷出多行。
	InsertIfAbsent(ctx context.Context, d *DeadLetter) (bool, error)
	// FindByEventID 按事件 ID 查询；不存在返回 nil。
	FindByEventID(ctx context.Context, eventID string) (*DeadLetter, error)
	// MarkState 推进留档状态（open -> replayed/ignored），带操作人留痕。
	MarkState(ctx context.Context, id int64, state, operator string) (int64, error)
	// List 分页查询死信。
	List(ctx context.Context, f DeadLetterFilter) ([]*DeadLetter, error)
	// Count 统计过滤条件下的死信数（分页 total）。
	Count(ctx context.Context, f DeadLetterFilter) (int64, error)
	// CountOpen 统计待处理死信数（健康检查与告警阈值）。
	CountOpen(ctx context.Context, since int64) (int64, error)
}

type defaultDeadLetterModel struct {
	conn sqlx.SqlConn
}

// NewDeadLetterModel 创建 DeadLetterModel 实现。
func NewDeadLetterModel(conn sqlx.SqlConn) DeadLetterModel {
	return &defaultDeadLetterModel{conn: conn}
}

// deadLetterColumns 里 event_id 必须 COALESCE：该列在信封不可解析时存 NULL
// （NULL 不参与唯一约束，见 InsertIfAbsent），而结构体字段是非空 string，
// 直接 SELECT 会在扫描阶段报「converting NULL to string」，让整条列表查询失败。
const deadLetterColumns = "id, COALESCE(event_id, '') AS event_id, event_type, topic," +
	" partition_no, msg_offset, payload_digest, payload_preview, reason, error_count, state," +
	" operator, processed_at, ctime, mtime"

// ValidDeadLetterState 判断死信状态是否属于集合。
func ValidDeadLetterState(s string) bool {
	switch s {
	case DeadLetterStateOpen, DeadLetterStateReplayed, DeadLetterStateIgnored:
		return true
	default:
		return false
	}
}

func (m *defaultDeadLetterModel) InsertIfAbsent(
	ctx context.Context, d *DeadLetter,
) (bool, error) {
	if strings.TrimSpace(d.PayloadDigest) == "" {
		return false, ErrEmptyPayload
	}
	if !ValidDeadLetterState(d.State) {
		// 新留档一律 open：判死即代表需要有人看，自动忽略等于把故障吞掉。
		d.State = DeadLetterStateOpen
	}
	now := nowUnix()
	// uniq_event_id 与 uniq_digest 两条唯一键配合：信封可解析时按 event_id 去重，
	// 不可解析（event_id 空串）时按内容摘要去重。MySQL 的 NULL 不参与唯一约束，
	// 因此空 event_id 必须写 NULL 才不会互相撞键。
	// 判空看的是 trim 之后的值：只含空白的 event_id 若按空串写进去，
	// 第二条同样的坏消息就会撞 uniq_event_id 而丢档。
	eventIDArg := any(nil)
	if eventID := strings.TrimSpace(d.EventID); eventID != "" {
		eventIDArg = eventID
	}
	query := "INSERT INTO spm_dead_letter (event_id, event_type, topic, partition_no, msg_offset," +
		" payload_digest, payload_preview, reason, error_count, state, operator, processed_at," +
		" ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', 0, ?, ?)" +
		" ON DUPLICATE KEY UPDATE error_count = error_count + 1, mtime = VALUES(mtime)"
	res, err := m.conn.ExecCtx(ctx, query,
		eventIDArg, truncate(d.EventType, 64), truncate(d.Topic, 128), d.PartitionNo, d.MsgOffset,
		truncate(d.PayloadDigest, 80), truncate(d.PayloadPreview, 512), truncate(d.Reason, 512),
		d.ErrorCount, d.State, now, now)
	if err != nil {
		return false, fmt.Errorf("spm_dead_letter InsertIfAbsent: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("spm_dead_letter InsertIfAbsent RowsAffected: %w", err)
	}
	// affected=1 新插入；=2 命中唯一键并累加了 error_count（同一坏消息被反复投递）。
	// d.State 不出现在这里：入口已把非法状态归一成 open，回读自己赋值是给 vet
	// 报「self-assignment」留把柄，也会让调用方以为落库值和入参不同。
	d.Ctime, d.Mtime = now, now
	return affected == 1, nil
}

func (m *defaultDeadLetterModel) FindByEventID(
	ctx context.Context, eventID string,
) (*DeadLetter, error) {
	var row DeadLetter
	query := "SELECT " + deadLetterColumns + " FROM spm_dead_letter WHERE event_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_dead_letter FindByEventID: %w", err)
	}
	return &row, nil
}

func (m *defaultDeadLetterModel) MarkState(
	ctx context.Context, id int64, state, operator string,
) (int64, error) {
	if !ValidDeadLetterState(state) || state == DeadLetterStateOpen {
		return 0, ErrInvalidDeadLetterState
	}
	if strings.TrimSpace(operator) == "" {
		return 0, ErrOperatorRequired
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE spm_dead_letter SET state = ?, operator = ?, processed_at = ?, mtime = ?"+
			" WHERE id = ? AND state = ?",
		state, truncate(operator, 64), nowUnix(), nowUnix(), id, DeadLetterStateOpen)
	if err != nil {
		return 0, fmt.Errorf("spm_dead_letter MarkState: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_dead_letter MarkState RowsAffected: %w", err)
	}
	return n, nil
}

func buildDeadLetterQuery(f DeadLetterFilter) (string, []any) {
	query := "SELECT " + deadLetterColumns + " FROM spm_dead_letter WHERE 1 = 1"
	var args []any
	if f.Topic != "" {
		query += " AND topic = ?"
		args = append(args, f.Topic)
	}
	if f.State != "" {
		query += " AND state = ?"
		args = append(args, f.State)
	}
	if f.Since > 0 {
		query += " AND ctime >= ?"
		args = append(args, f.Since)
	}
	return query, args
}

func (m *defaultDeadLetterModel) List(
	ctx context.Context, f DeadLetterFilter,
) ([]*DeadLetter, error) {
	query, args := buildDeadLetterQuery(f)
	query += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, clampLimit(f.Limit), clampOffset(f.Offset))
	var rows []*DeadLetter
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_dead_letter List: %w", err)
	}
	return rows, nil
}

func (m *defaultDeadLetterModel) Count(ctx context.Context, f DeadLetterFilter) (int64, error) {
	query, args := buildDeadLetterQuery(f)
	query = strings.Replace(query, "SELECT "+deadLetterColumns, "SELECT COUNT(*)", 1)
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		return 0, fmt.Errorf("spm_dead_letter Count: %w", err)
	}
	return n, nil
}

func (m *defaultDeadLetterModel) CountOpen(ctx context.Context, since int64) (int64, error) {
	query := "SELECT COUNT(*) FROM spm_dead_letter WHERE state = ?"
	args := []any{DeadLetterStateOpen}
	if since > 0 {
		query += " AND ctime >= ?"
		args = append(args, since)
	}
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		return 0, fmt.Errorf("spm_dead_letter CountOpen: %w", err)
	}
	return n, nil
}
