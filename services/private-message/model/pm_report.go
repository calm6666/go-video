package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 举报处理状态，与 pm_report.state、rpc.ReportState 一致。
const (
	// ReportStatePending 待处理。
	ReportStatePending int32 = 1
	// ReportStateHandled 已处理（撤回/处罚/送审完成）。
	ReportStateHandled int32 = 2
	// ReportStateDismissed 已驳回。
	ReportStateDismissed int32 = 3
)

// Report 私信举报记录（pm_report 表）。
//
// 依据 AGENTS.md §5：本表只记录“谁举报了哪条消息”，不直连 moderation 库；
// 送审通过 moderation-orchestrator RPC 拿 task_id，审核结论只由
// ApplyModerationVerdict（moderation-orchestrator 侧）推进本域状态。
// 幂等：唯一键 (msg_id, reporter_mid) 保证同一举报人对同一消息只留一条。
// 处置幂等：handle_idempotency_key 可空唯一索引（MySQL 下 NULL 不参与唯一性判定），
// 未处置行为 NULL，处置后写入调用方幂等键，重复提交直接回放首次结果。
type Report struct {
	// ReportID 举报 ID（主键）
	ReportID int64 `db:"report_id"`
	// ConversationID 会话 ID（冗余快照，运营侧免 JOIN）
	ConversationID int64 `db:"conversation_id"`
	// MsgID 被举报消息 ID
	MsgID int64 `db:"msg_id"`
	// ReporterMid 举报者
	ReporterMid int64 `db:"reporter_mid"`
	// TargetMid 被举报人（通常是消息发送者）
	TargetMid int64 `db:"target_mid"`
	// Reason 举报原因码
	Reason int32 `db:"reason"`
	// Description 补充说明（不含私信正文）
	Description string `db:"description"`
	// State 处理状态
	State int32 `db:"state"`
	// AuditTaskID 关联的审核任务 ID
	AuditTaskID int64 `db:"audit_task_id"`
	// Handler 处理人（0 表示未处理）
	Handler int64 `db:"handler"`
	// HandleNote 处置备注（审计）
	HandleNote string `db:"handle_note"`
	// HandleIdempotencyKey 处置幂等键，未处置为 NULL
	HandleIdempotencyKey sql.NullString `db:"handle_idempotency_key"`
	// TraceID 举报链路 ID
	TraceID string `db:"trace_id"`
	// Ctime 举报时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// ReportModel pm_report 表读写接口。
type ReportModel interface {
	// Insert 写入举报；命中 (msg_id, reporter_mid) 唯一键时返回已有 ID 且 created=false。
	Insert(ctx context.Context, r *Report) (reportID int64, created bool, err error)
	// FindByID 查询举报；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, reportID int64) (*Report, error)
	// FindByHandleKey 按处置幂等键查询（HandleReport 回放路径）。
	FindByHandleKey(ctx context.Context, key string) (*Report, error)
	// ListPending 按游标（report_id 倒序）分页拉取待处理举报，
	// 供 moderation-orchestrator 侧批量取件（不含正文，只给主键与原因）。
	ListPending(ctx context.Context, ps int32) ([]*Report, error)
	// ListByCursor 运营侧分页：state/targetMid 为 0 表示不过滤。
	ListByCursor(ctx context.Context, state, targetMid, cursorID int64, ps int32) ([]*Report, error)
	// MarkHandled 处置举报：CAS 要求当前状态为 ReportStatePending，
	// 幂等键冲突（同一 key 已处置）时返回 ErrConcurrentUpdate 由 logic 回查回放。
	MarkHandled(ctx context.Context, reportID int64, state int32, handler int64, note, idempotencyKey string) error
	// MarkHandledInTx 与 MarkHandled 同一 CAS 语义，但可在调用方事务内执行。
	// 举报处置与连带撤回必须同事务（否则出现「举报已处理、违规消息仍可见」的空档），
	// 因此事务版不再回查（回查走的是连接而非事务快照，只会看到别人的已提交行），
	// 命中 0 行统一返回 ErrConcurrentUpdate，由 logic 在事务外按幂等键回放。
	MarkHandledInTx(ctx context.Context, session sqlx.Session, reportID int64, state int32, handler int64,
		note, idempotencyKey string) error
	// BindAuditTask 回写送审任务 ID。
	BindAuditTask(ctx context.Context, reportID, taskID int64) error
}

type defaultReportModel struct {
	conn sqlx.SqlConn
}

// NewReportModel 创建 ReportModel 实现。
func NewReportModel(conn sqlx.SqlConn) ReportModel {
	return &defaultReportModel{conn: conn}
}

const reportColumns = `report_id, conversation_id, msg_id, reporter_mid, target_mid, reason, description,
	state, audit_task_id, handler, handle_note, handle_idempotency_key, trace_id, ctime, mtime`

func (m *defaultReportModel) Insert(ctx context.Context, r *Report) (int64, bool, error) {
	if r.MsgID <= 0 || r.ReporterMid <= 0 {
		return 0, false, ErrInvalidMid
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO pm_report (conversation_id, msg_id, reporter_mid, target_mid, reason, description, state, "+
			"audit_task_id, handler, handle_note, handle_idempotency_key, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, '', NULL, ?, ?, ?)",
		r.ConversationID, r.MsgID, r.ReporterMid, r.TargetMid, r.Reason, r.Description, ReportStatePending,
		r.AuditTaskID, r.TraceID, now, now)
	if err != nil {
		// 重复举报按幂等重放返回既有记录。
		if old, qerr := m.findByMsgReporter(ctx, r.MsgID, r.ReporterMid); qerr == nil && old != nil {
			return old.ReportID, false, nil
		}
		return 0, false, fmt.Errorf("pm_report Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("pm_report Insert LastInsertId: %w", err)
	}
	return id, true, nil
}

func (m *defaultReportModel) FindByID(ctx context.Context, reportID int64) (*Report, error) {
	var r Report
	err := m.conn.QueryRowCtx(ctx, &r,
		"SELECT "+reportColumns+" FROM pm_report WHERE report_id = ? LIMIT 1", reportID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_report FindByID: %w", err)
	}
	return &r, nil
}

func (m *defaultReportModel) findByMsgReporter(ctx context.Context, msgID, reporterMid int64) (*Report, error) {
	var r Report
	err := m.conn.QueryRowCtx(ctx, &r,
		"SELECT "+reportColumns+" FROM pm_report WHERE msg_id = ? AND reporter_mid = ? LIMIT 1", msgID, reporterMid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_report findByMsgReporter: %w", err)
	}
	return &r, nil
}

func (m *defaultReportModel) FindByHandleKey(ctx context.Context, key string) (*Report, error) {
	if key == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	var r Report
	err := m.conn.QueryRowCtx(ctx, &r,
		"SELECT "+reportColumns+" FROM pm_report WHERE handle_idempotency_key = ? LIMIT 1", key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_report FindByHandleKey: %w", err)
	}
	return &r, nil
}

func (m *defaultReportModel) ListPending(ctx context.Context, ps int32) ([]*Report, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	var rows []*Report
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+reportColumns+" FROM pm_report WHERE state = ? ORDER BY report_id ASC LIMIT ?",
		ReportStatePending, ps)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_report ListPending: %w", err)
	}
	return rows, nil
}

func (m *defaultReportModel) ListByCursor(ctx context.Context, state, targetMid, cursorID int64, ps int32) ([]*Report, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	conds := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if state > 0 {
		conds = append(conds, "state = ?")
		args = append(args, state)
	}
	if targetMid > 0 {
		conds = append(conds, "target_mid = ?")
		args = append(args, targetMid)
	}
	if cursorID > 0 {
		conds = append(conds, "report_id < ?")
		args = append(args, cursorID)
	}
	query := "SELECT " + reportColumns + " FROM pm_report WHERE " + joinAnd(conds) + " ORDER BY report_id DESC LIMIT ?"
	args = append(args, ps)

	var rows []*Report
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_report ListByCursor: %w", err)
	}
	return rows, nil
}

func (m *defaultReportModel) MarkHandled(ctx context.Context, reportID int64, state int32, handler int64,
	note, idempotencyKey string) error {
	if idempotencyKey == "" {
		return ErrIdempotencyKeyRequired
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE pm_report SET state = ?, handler = ?, handle_note = ?, handle_idempotency_key = ?, mtime = ? "+
			"WHERE report_id = ? AND state = ?",
		state, handler, note, idempotencyKey, nowUnix(), reportID, ReportStatePending)
	if err != nil {
		return fmt.Errorf("pm_report MarkHandled: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("pm_report MarkHandled RowsAffected: %w", err)
	}
	if aff == 0 {
		// 区分“记录不存在”与“已被处置”：前者报错，后者由 logic 用 FindByHandleKey 回放。
		cur, qerr := m.FindByID(ctx, reportID)
		if qerr != nil {
			return qerr
		}
		if cur == nil {
			return ErrReportNotFound
		}
		return ErrConcurrentUpdate
	}
	return nil
}

// MarkHandledInTx 事务内版本：CAS 条件与列写入与 MarkHandled 完全一致，
// 只有「命中 0 行」的归因不同（不回查，统一 ErrConcurrentUpdate）。
func (m *defaultReportModel) MarkHandledInTx(ctx context.Context, session sqlx.Session, reportID int64,
	state int32, handler int64, note, idempotencyKey string) error {
	if idempotencyKey == "" {
		return ErrIdempotencyKeyRequired
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE pm_report SET state = ?, handler = ?, handle_note = ?, handle_idempotency_key = ?, mtime = ? "+
			"WHERE report_id = ? AND state = ?",
		state, handler, note, idempotencyKey, nowUnix(), reportID, ReportStatePending)
	if err != nil {
		return fmt.Errorf("pm_report MarkHandledInTx: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("pm_report MarkHandledInTx RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrConcurrentUpdate
	}
	return nil
}

func (m *defaultReportModel) BindAuditTask(ctx context.Context, reportID, taskID int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE pm_report SET audit_task_id = ?, mtime = ? WHERE report_id = ? AND audit_task_id = 0",
		taskID, nowUnix(), reportID)
	if err != nil {
		return fmt.Errorf("pm_report BindAuditTask: %w", err)
	}
	return nil
}

// joinAnd 把过滤条件拼成 AND 串。
func joinAnd(conds []string) string {
	return strings.Join(conds, " AND ")
}
