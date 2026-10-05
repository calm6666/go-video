package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 举报处理状态，与 danmaku_report.state 列一致。
const (
	// ReportPending 待 moderation 拉取处理。
	ReportPending int32 = 0
	// ReportHandled 已处理（审核结论已回写）。
	ReportHandled int32 = 1
	// ReportDismissed 已驳回（不处理）。
	ReportDismissed int32 = 2
)

// Report 弹幕举报记录（danmaku_report 表）。
// 依据 AGENTS.md §5，本表只记录举报事实，不直连 moderation 库；
// 审核域通过 ListPendingReports 拉取，再由 ApplyModerationResult 回写结论。
// (dmid, reporter_mid) 唯一索引保证同一人对同一条弹幕重复举报只留一条记录。
type Report struct {
	ReportID    int64  `db:"report_id"`    // 举报记录 ID
	Dmid        int64  `db:"dmid"`         // 被举报弹幕 ID
	ReporterMid int64  `db:"reporter_mid"` // 举报者用户 ID
	Reason      int32  `db:"reason"`       // 举报原因码
	Content     string `db:"content"`      // 举报补充说明
	State       int32  `db:"state"`        // 处理状态
	TraceID     string `db:"trace_id"`     // 链路追踪 ID
	Ctime       int64  `db:"ctime"`        // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 修改时间（Unix 秒）
}

// ReportModel danmaku_report 表查询与写入接口。
type ReportModel interface {
	// Insert 写入举报，返回 report_id；同一 (dmid, reporter_mid) 已存在时
	// 返回已有 report_id 且 created=false（幂等重放）。
	Insert(ctx context.Context, r *Report) (reportID int64, created bool, err error)
	// FindByTarget 查询某人对某条弹幕的举报记录；不存在返回 (nil, nil)。
	FindByTarget(ctx context.Context, dmid, reporterMid int64) (*Report, error)
	// ListPending 分页拉取待处理举报（供 moderation-orchestrator 侧调用）。
	ListPending(ctx context.Context, pn, ps int32) ([]*Report, int32, error)
	// MarkHandled 更新举报处理状态。
	MarkHandled(ctx context.Context, reportID int64, state int32) error
}

type defaultReportModel struct {
	conn sqlx.SqlConn
}

// NewReportModel 创建 ReportModel 实现。
func NewReportModel(conn sqlx.SqlConn) ReportModel {
	return &defaultReportModel{conn: conn}
}

func (m *defaultReportModel) Insert(ctx context.Context, r *Report) (int64, bool, error) {
	now := nowUnix()
	// 先查唯一索引锚点，命中即视为重放；未命中再写，
	// 并发下唯一索引兜底，写失败时回查一次按重放返回。
	old, err := m.FindByTarget(ctx, r.Dmid, r.ReporterMid)
	if err != nil {
		return 0, false, err
	}
	if old != nil {
		return old.ReportID, false, nil
	}

	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO danmaku_report (dmid, reporter_mid, reason, content, state, trace_id, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		r.Dmid, r.ReporterMid, r.Reason, r.Content, ReportPending, r.TraceID, now, now)
	if err != nil {
		dup, qerr := m.FindByTarget(ctx, r.Dmid, r.ReporterMid)
		if qerr != nil {
			return 0, false, qerr
		}
		if dup != nil {
			return dup.ReportID, false, nil
		}
		return 0, false, fmt.Errorf("danmaku_report Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("danmaku_report Insert LastInsertId: %w", err)
	}
	return id, true, nil
}

func (m *defaultReportModel) FindByTarget(ctx context.Context, dmid, reporterMid int64) (*Report, error) {
	var r Report
	err := m.conn.QueryRowCtx(ctx, &r,
		"SELECT report_id, dmid, reporter_mid, reason, content, state, trace_id, ctime, mtime FROM danmaku_report WHERE dmid = ? AND reporter_mid = ? LIMIT 1",
		dmid, reporterMid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku_report FindByTarget: %w", err)
	}
	return &r, nil
}

func (m *defaultReportModel) ListPending(ctx context.Context, pn, ps int32) ([]*Report, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 100 {
		ps = 20
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM danmaku_report WHERE state = ?", ReportPending); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("danmaku_report ListPending count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*Report
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT report_id, dmid, reporter_mid, reason, content, state, trace_id, ctime, mtime FROM danmaku_report WHERE state = ? ORDER BY report_id ASC LIMIT ? OFFSET ?",
		ReportPending, ps, (pn-1)*ps)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("danmaku_report ListPending: %w", err)
	}
	return rows, total, nil
}

func (m *defaultReportModel) MarkHandled(ctx context.Context, reportID int64, state int32) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE danmaku_report SET state = ?, mtime = ? WHERE report_id = ?", state, nowUnix(), reportID)
	if err != nil {
		return fmt.Errorf("danmaku_report MarkHandled: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("danmaku_report MarkHandled RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrReportNotFound
	}
	return nil
}
