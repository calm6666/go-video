package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RecallRequestLog 一次在线召回请求的审计摘要（recall_request_log 表）。
//
// 存在这张表的意义：候选必须可追溯到"读了哪些池的哪个版本、每路出了多少条、是否降级"。
// 排序服务与运营排障只靠 request_id / snapshot_id 就能完整回放一次召回，
// 不需要访问任何其它服务的库（AGENTS.md §5）。
//
// 隐私：不落设备号、IP 等原文（连摘要都不落，日志按 mid + 时间维度查询已足够排障），
// 与 search-query 的查询日志保持同一脱敏口径。
type RecallRequestLog struct {
	ID               int64  `db:"id"`                // 自增主键
	RequestID        string `db:"request_id"`        // 幂等与审计键（唯一）
	SnapshotID       string `db:"snapshot_id"`       // 本次召回快照 ID（唯一，供排序回指）
	Mid              int64  `db:"mid"`               // 用户 ID，0 表示游客
	Scene            string `db:"scene"`             // 场景稳定 key
	Platform         int32  `db:"platform"`          // 客户端平台，参见 rpc Platform
	AppVersion       string `db:"app_version"`       // 客户端版本
	Region           string `db:"region"`            // 地区代码
	RequestedSources string `db:"requested_sources"` // csv 升序去重的召回路编号
	PerSource        string `db:"per_source"`        // 每路统计 JSON 数组（排障展示）
	CandidateCount   int32  `db:"candidate_count"`   // 合并去重后的候选条数
	ReturnedCount    int32  `db:"returned_count"`    // 实际下发条数
	Degraded         int32  `db:"degraded"`          // 0 未降级、1 已降级
	DegradeReason    string `db:"degrade_reason"`    // 受控 key，参见 DegradeReason*
	DroppedSources   string `db:"dropped_sources"`   // 被丢弃的召回路（csv）
	VersionsDigest   string `db:"versions_digest"`   // sha256(读取到的 source:version 序列)
	CostMs           int32  `db:"cost_ms"`           // 本次召回耗时
	TraceID          string `db:"trace_id"`          // 调用方透传 trace_id
	Ctime            int64  `db:"ctime"`             // 创建时间（Unix 秒）
}

// RequestLogQuery 审计日志的分页查询条件。
type RequestLogQuery struct {
	Mid    int64  // 0 表示不按用户过滤
	Scene  string // 空表示不过滤
	From   int64  // Unix 秒，含
	To     int64  // Unix 秒，含
	Offset int
	Limit  int
}

// RecallRequestLogModel recall_request_log 表读写接口。
type RecallRequestLogModel interface {
	// Insert 写入审计行；request_id 或 snapshot_id 为空返回 ErrRequestLogRequired；
	// 两列任一重复返回 ErrRequestLogExists（幂等命中，调用方应回放旧结果而不是再算一次）。
	Insert(ctx context.Context, l *RecallRequestLog) error
	// FindByRequestID 按 request_id 查询；不存在返回 ErrRequestNotFound。
	FindByRequestID(ctx context.Context, requestID string) (*RecallRequestLog, error)
	// FindBySnapshotID 按 snapshot_id 查询；不存在返回 ErrRequestNotFound。
	FindBySnapshotID(ctx context.Context, snapshotID string) (*RecallRequestLog, error)
	// List 分页查询（ctime DESC，供运营/排障）。
	// q.Limit 必须落在 (0, MaxRequestLogPageSize]；q.Offset 超深返回 ErrPageTooDeep。
	List(ctx context.Context, q RequestLogQuery) ([]*RecallRequestLog, error)
	// Count 与 List 同条件的总数（分页是否有下一页由调用方按 pn/ps 判定）。
	Count(ctx context.Context, q RequestLogQuery) (int64, error)
	// DeleteBefore 按 ctime 截断历史（保留期治理由 services/cron 调用），maxRows 限制单次行数。
	DeleteBefore(ctx context.Context, before, maxRows int64) (int64, error)
}

type defaultRecallRequestLogModel struct {
	conn sqlx.SqlConn
}

// NewRecallRequestLogModel 创建 RecallRequestLogModel 实现。
func NewRecallRequestLogModel(conn sqlx.SqlConn) RecallRequestLogModel {
	return &defaultRecallRequestLogModel{conn: conn}
}

const requestLogSelect = "SELECT id, request_id, snapshot_id, mid, scene, platform, app_version, region, " +
	"requested_sources, per_source, candidate_count, returned_count, degraded, degrade_reason, dropped_sources, " +
	"versions_digest, cost_ms, trace_id, ctime FROM recall_request_log"

const requestLogInsertColumns = "request_id, snapshot_id, mid, scene, platform, app_version, region, " +
	"requested_sources, per_source, candidate_count, returned_count, degraded, degrade_reason, dropped_sources, " +
	"versions_digest, cost_ms, trace_id, ctime"

func (m *defaultRecallRequestLogModel) Insert(ctx context.Context, l *RecallRequestLog) error {
	if strings.TrimSpace(l.RequestID) == "" || strings.TrimSpace(l.SnapshotID) == "" {
		return ErrRequestLogRequired
	}
	if l.DegradeReason != "" && !ValidDegradeReason(l.DegradeReason) {
		return ErrInvalidDegradeReason
	}
	if l.Platform != 0 && !ValidPlatform(l.Platform) {
		return ErrInvalidPlatform
	}
	if l.Ctime == 0 {
		l.Ctime = nowUnix()
	}
	query := "INSERT INTO recall_request_log (" + requestLogInsertColumns + ") VALUES (" +
		inPlaceholders(18) + ")"
	if _, err := m.conn.ExecCtx(ctx, query,
		l.RequestID, l.SnapshotID, l.Mid, l.Scene, l.Platform, l.AppVersion, l.Region,
		l.RequestedSources, l.PerSource, l.CandidateCount, l.ReturnedCount, l.Degraded, l.DegradeReason,
		l.DroppedSources, l.VersionsDigest, l.CostMs, l.TraceID, l.Ctime); err != nil {
		if isDuplicateErr(err) {
			return ErrRequestLogExists
		}
		return fmt.Errorf("recall_request_log Insert: %w", err)
	}
	return nil
}

func (m *defaultRecallRequestLogModel) FindByRequestID(ctx context.Context, requestID string) (*RecallRequestLog, error) {
	return m.find(ctx, "request_id", requestID)
}

func (m *defaultRecallRequestLogModel) FindBySnapshotID(ctx context.Context, snapshotID string) (*RecallRequestLog, error) {
	return m.find(ctx, "snapshot_id", snapshotID)
}

func (m *defaultRecallRequestLogModel) find(ctx context.Context, column, value string) (*RecallRequestLog, error) {
	var row RecallRequestLog
	// column 只取内部两个字面量（request_id / snapshot_id），不接受调用方输入，无注入面。
	// 两列都有唯一索引，LIMIT 1 是防御性下界而不是性能手段。
	query := requestLogSelect + " WHERE " + column + " = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRequestNotFound
		}
		return nil, fmt.Errorf("recall_request_log find(%s): %w", column, err)
	}
	return &row, nil
}

func (m *defaultRecallRequestLogModel) List(ctx context.Context, q RequestLogQuery) ([]*RecallRequestLog, error) {
	if err := checkRequestLogQuery(q); err != nil {
		return nil, err
	}
	where, args := requestLogWhere(q)
	query := requestLogSelect + where + " ORDER BY ctime DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Limit, q.Offset)
	var rows []*RecallRequestLog
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_request_log List: %w", err)
	}
	return rows, nil
}

func (m *defaultRecallRequestLogModel) Count(ctx context.Context, q RequestLogQuery) (int64, error) {
	if q.From > 0 && q.To > 0 && q.From > q.To {
		return 0, ErrPageTooDeep
	}
	where, args := requestLogWhere(q)
	var count int64
	query := "SELECT COUNT(1) FROM recall_request_log" + where
	if err := m.conn.QueryRowCtx(ctx, &count, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("recall_request_log Count: %w", err)
	}
	return count, nil
}

// requestLogWhere 返回 "<WHERE ...>" 片段与参数（条件文本只来自本包字面量）。
// List 与 Count 共用它，保证分页总数与当页口径一致。
func requestLogWhere(q RequestLogQuery) (string, []interface{}) {
	var conditions []string
	var args []interface{}
	if q.Mid > 0 {
		conditions = append(conditions, "mid = ?")
		args = append(args, q.Mid)
	}
	if q.Scene != "" {
		conditions = append(conditions, "scene = ?")
		args = append(args, q.Scene)
	}
	if q.From > 0 {
		conditions = append(conditions, "ctime >= ?")
		args = append(args, q.From)
	}
	if q.To > 0 {
		conditions = append(conditions, "ctime <= ?")
		args = append(args, q.To)
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + joinAnd(conditions), args
}

// checkRequestLogQuery 是日志读路径的边界校验：条数、偏移与时间区间都必须有界。
func checkRequestLogQuery(q RequestLogQuery) error {
	if err := CheckLimit(q.Limit, MaxRequestLogPageSize); err != nil {
		return err
	}
	if q.Offset > MaxRequestLogOffset {
		return ErrPageTooDeep
	}
	if q.From > 0 && q.To > 0 && q.From > q.To {
		return ErrPageTooDeep
	}
	return nil
}

func (m *defaultRecallRequestLogModel) DeleteBefore(ctx context.Context, before, maxRows int64) (int64, error) {
	if before <= 0 {
		return 0, ErrInvalidLimit
	}
	if err := CheckInt64Limit(maxRows, MaxDeleteRows); err != nil {
		return 0, err
	}
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM recall_request_log WHERE ctime < ? LIMIT ?", before, maxRows)
	if err != nil {
		return 0, fmt.Errorf("recall_request_log DeleteBefore: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recall_request_log DeleteBefore RowsAffected: %w", err)
	}
	return deleted, nil
}

// joinAnd 用 AND 连接已构造好的条件（条件文本只来自本包字面量）。
func joinAnd(conditions []string) string {
	out := conditions[0]
	for _, c := range conditions[1:] {
		out += " AND " + c
	}
	return out
}
