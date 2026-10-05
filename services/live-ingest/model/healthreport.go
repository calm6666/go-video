package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// healthReportColumns 是 live_stream_health_report 的列清单，必须与
// deploy/migrations/live-ingest/000002_create_live_stream_tables.sql 完全一致。
const healthReportColumns = "id, report_id, stream_id, node_id, video_bitrate_bps, audio_bitrate_bps, " +
	"fps_x100, packet_loss_ppm, rtt_ms, sample_window_seconds, health_state, occurred_at, trace_id, ctime"

// StreamHealthReport 健康采样点（live_stream_health_report 表投影）。
//
// 这是排障与「流健康检查」的原始事实：live_stream 上只保留最新一帧采样，
// 窗口聚合（均值/最低值/最大丢包）由本表按 (stream_id, occurred_at) 索引算出。
// 增长最快，由 services/cron 按 occurred_at 归档（保留窗口见服务 README）。
type StreamHealthReport struct {
	ID                  int64  `db:"id"`                    // 自增主键
	ReportID            string `db:"report_id"`             // 上报幂等键（唯一索引，内部采样可 NULL）
	StreamID            string `db:"stream_id"`             // 所属流
	NodeID              string `db:"node_id"`               // 采样来源节点
	VideoBitrateBps     int64  `db:"video_bitrate_bps"`     // 视频码率（bps）
	AudioBitrateBps     int64  `db:"audio_bitrate_bps"`     // 音频码率（bps）
	FpsX100             int32  `db:"fps_x100"`              // 帧率 ×100
	PacketLossPpm       int32  `db:"packet_loss_ppm"`       // 丢包率（百万分比）
	RttMs               int64  `db:"rtt_ms"`                // 往返时延（毫秒）
	SampleWindowSeconds int32  `db:"sample_window_seconds"` // 采样窗口（秒）
	HealthState         int32  `db:"health_state"`          // 该采样点判定，见 HealthState*
	OccurredAt          int64  `db:"occurred_at"`           // 采样时间（Unix 秒）
	TraceID             string `db:"trace_id"`              // 链路追踪 ID
	Ctime               int64  `db:"ctime"`                 // 落库时间（Unix 秒）
}

// HealthAggregate 窗口聚合结果（GetStreamHealth 的数据来源）。
type HealthAggregate struct {
	SampleCount      int32 `db:"sample_count"`
	AvgVideoBitrate  int64 `db:"avg_video_bitrate"`
	MinVideoBitrate  int64 `db:"min_video_bitrate"`
	MaxPacketLossPpm int32 `db:"max_packet_loss_ppm"`
	LatestReportedAt int64 `db:"latest_reported_at"`
}

// StreamHealthReportModel live_stream_health_report 表查询与写入接口。
type StreamHealthReportModel interface {
	// Insert 写入采样点并返回自增 ID；uniq_report_id 冲突表示上报重放。
	Insert(ctx context.Context, tx sqlx.Session, r *StreamHealthReport) (int64, error)
	// FindByReportID 按上报幂等键查询；不存在返回 (nil, nil)。
	FindByReportID(ctx context.Context, reportID string) (*StreamHealthReport, error)
	// ListRecent 返回某流最近 limit 条采样（按 occurred_at 升序，便于直接画折线）。
	ListRecent(ctx context.Context, streamID string, since int64, limit int32) ([]*StreamHealthReport, error)
	// Aggregate 计算 [since, now] 窗口的聚合值；无采样时 SampleCount=0。
	Aggregate(ctx context.Context, streamID string, since int64) (*HealthAggregate, error)
	// Prune 归档窗口采样（由 services/cron 调用），返回影响行数。
	Prune(ctx context.Context, before int64, limit int32) (int64, error)
}

type defaultStreamHealthReportModel struct {
	conn sqlx.SqlConn
}

// NewStreamHealthReportModel 创建 StreamHealthReportModel 实现。
func NewStreamHealthReportModel(conn sqlx.SqlConn) StreamHealthReportModel {
	return &defaultStreamHealthReportModel{conn: conn}
}

func (m *defaultStreamHealthReportModel) Insert(ctx context.Context, tx sqlx.Session, r *StreamHealthReport) (int64, error) {
	session := pickSession(m.conn, tx)
	if r.Ctime == 0 {
		r.Ctime = nowUnix()
	}
	if r.OccurredAt == 0 {
		r.OccurredAt = r.Ctime
	}
	res, err := session.ExecCtx(ctx,
		"INSERT INTO live_stream_health_report (report_id, stream_id, node_id, video_bitrate_bps, audio_bitrate_bps, "+
			"fps_x100, packet_loss_ppm, rtt_ms, sample_window_seconds, health_state, occurred_at, trace_id, ctime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		nullableString(r.ReportID), r.StreamID, r.NodeID, r.VideoBitrateBps, r.AudioBitrateBps,
		r.FpsX100, r.PacketLossPpm, r.RttMs, r.SampleWindowSeconds, r.HealthState, r.OccurredAt, r.TraceID, r.Ctime)
	if err != nil {
		return 0, fmt.Errorf("live_stream_health_report Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_stream_health_report Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultStreamHealthReportModel) FindByReportID(ctx context.Context, reportID string) (*StreamHealthReport, error) {
	if reportID == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	var r StreamHealthReport
	query := "SELECT " + healthReportColumns + " FROM live_stream_health_report WHERE report_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &r, query, reportID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_health_report FindByReportID: %w", err)
	}
	return &r, nil
}

func (m *defaultStreamHealthReportModel) ListRecent(ctx context.Context, streamID string, since int64, limit int32) ([]*StreamHealthReport, error) {
	max := clampLimit(limit, 300)
	conds := []string{"stream_id = ?"}
	args := []interface{}{streamID}
	if since > 0 {
		conds = append(conds, "occurred_at >= ?")
		args = append(args, since)
	}
	// 先取最近 max 条（DESC），再在调用方按升序返回，保证「最近的点」不被截掉。
	query := "SELECT " + healthReportColumns + " FROM live_stream_health_report WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY occurred_at DESC, id DESC LIMIT ?"
	args = append(args, max)

	var rows []*StreamHealthReport
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_health_report ListRecent: %w", err)
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

func (m *defaultStreamHealthReportModel) Aggregate(ctx context.Context, streamID string, since int64) (*HealthAggregate, error) {
	agg := &HealthAggregate{}
	query := "SELECT COUNT(*) AS sample_count, " +
		"COALESCE(AVG(video_bitrate_bps), 0) AS avg_video_bitrate, " +
		"COALESCE(MIN(video_bitrate_bps), 0) AS min_video_bitrate, " +
		"COALESCE(MAX(packet_loss_ppm), 0) AS max_packet_loss_ppm, " +
		"COALESCE(MAX(occurred_at), 0) AS latest_reported_at " +
		"FROM live_stream_health_report WHERE stream_id = ? AND occurred_at >= ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, agg, query, streamID, since); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &HealthAggregate{}, nil
		}
		return nil, fmt.Errorf("live_stream_health_report Aggregate: %w", err)
	}
	return agg, nil
}

func (m *defaultStreamHealthReportModel) Prune(ctx context.Context, before int64, limit int32) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM live_stream_health_report WHERE occurred_at < ? ORDER BY id ASC LIMIT ?",
		before, clampLimit(limit, 2000))
	if err != nil {
		return 0, fmt.Errorf("live_stream_health_report Prune: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_stream_health_report Prune RowsAffected: %w", err)
	}
	return affected, nil
}
