package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// CommentReport 评论举报记录。
// 举报写入 moderation-orchestrator 待审队列，本表只保留本地审计快照。
type CommentReport struct {
	ReportID    int64  `db:"report_id"`    // 举报记录 ID
	Rpid        int64  `db:"rpid"`         // 被举报评论 ID
	ReporterMid int64  `db:"reporter_mid"` // 举报者用户 ID
	Reason      int32  `db:"reason"`       // 举报原因码
	Content     string `db:"content"`      // 举报理由补充说明
	TraceID     string `db:"trace_id"`     // 透传 trace_id
	Ctime       int64  `db:"ctime"`        // 举报时间（Unix 秒）
}

// CommentReportModel comment_report 表查询与写入接口。
type CommentReportModel interface {
	// Insert 记录举报；不直接修改评论状态（由 moderation 审核结论回调推进）。
	Insert(ctx context.Context, r *CommentReport) (int64, error)
}

type defaultCommentReportModel struct {
	conn sqlx.SqlConn
}

// NewCommentReportModel 创建 CommentReportModel 实现。
func NewCommentReportModel(conn sqlx.SqlConn) CommentReportModel {
	return &defaultCommentReportModel{conn: conn}
}

func (m *defaultCommentReportModel) Insert(ctx context.Context, r *CommentReport) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO comment_report (rpid, reporter_mid, reason, content, trace_id, ctime) VALUES (?, ?, ?, ?, ?, ?)",
		r.Rpid, r.ReporterMid, r.Reason, r.Content, r.TraceID, r.Ctime)
	if err != nil {
		return 0, fmt.Errorf("comment_report Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("comment_report Insert LastInsertId: %w", err)
	}
	return id, nil
}
