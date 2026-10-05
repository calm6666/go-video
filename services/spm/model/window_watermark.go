package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// WindowWatermark 窗口闭合水位行（spm_window_watermark 表投影）。
//
// 契约里有三个读方法的 window_start 允许传 0，含义都是「最近一个已闭合窗口」
// （GetMetricReq / BatchGetMetricsReq / ListHotSubjectsReq）。没有水位，每次读都要对
// spm_metric_window 做一次 max(window_start) 扫描；本表把它压成一次唯一键点查，
// 并顺带承载 GetUserInterestReply.stale 与榜「数据停在几点」的观测时刻。
//
// 一行 = 一个「主体类型 × 口径版本 × 窗口粒度」的水位，只前进不回退：
//   - 回填链路的职责是补写历史窗口，不是把水位拉回过去；
//   - 如果允许回退，一个跑错的旧批次会让所有实时读接口瞬间改读老窗口，
//     而响应里没有任何字段能解释「为什么榜倒退了十分钟」。
//
// 本表是投影：对 spm_metric_window 按 (口径版本, 粒度) 取 max(window_start) 即可整体重建，
// 不作为任何服务的事实源（AGENTS.md §5、docs/data-design.md §5）。
type WindowWatermark struct {
	ID              int64  `db:"id"`                // 主键 ID
	SubjectType     int32  `db:"subject_type"`      // 主体类型（0 = 跨主体汇总水位，供全站/分区榜）
	MetricKey       string `db:"metric_key"`        // 指标键
	MetricVersion   int32  `db:"metric_version"`    // 口径版本
	WindowType      int32  `db:"window_type"`       // 窗口粒度（水位不跨粒度共享）
	LastClosedStart int64  `db:"last_closed_start"` // 最近一个已闭合窗口的左边界（Unix 秒，已规整）
	LastEventTime   int64  `db:"last_event_time"`   // 该水位对应数据的推进时刻（Unix 秒，stale 判定）
	RowsWritten     int64  `db:"rows_written"`      // 该闭合窗口写入的主体行数（0 即聚合器漏算）
	UpdateRequestID string `db:"update_request_id"` // 最近一次推进水位的幂等键
	Ctime           int64  `db:"ctime"`             // 创建时间（Unix 秒）
	Mtime           int64  `db:"mtime"`             // 修改时间（Unix 秒）
}

// WatermarkFilter 水位列表过滤条件。零值表示不限。
type WatermarkFilter struct {
	SubjectType   int32
	MetricKey     string
	MetricVersion int32
	WindowType    int32
	Offset        int32
	Limit         int32
}

// WindowWatermarkModel spm_window_watermark 表读写接口（可从 spm_metric_window 重算的投影）。
type WindowWatermarkModel interface {
	// Advance 单调推进水位：只在 w.LastClosedStart 不早于既有水位时生效。
	// 返回 applied=false 表示这是一次「旧水位回退」尝试（幂等拒绝，不是错误），
	// 首次建立该水位行时 applied=true。
	Advance(ctx context.Context, w *WindowWatermark) (bool, error)
	// Find 取一个「主体类型 × 口径版本 × 粒度」的水位；不存在返回 nil
	// （调用方按「该口径还没有闭合窗口」处理，不得伪造 window_start=now）。
	Find(
		ctx context.Context, subjectType int32, metricKey string,
		version, windowType int32,
	) (*WindowWatermark, error)
	// ListByMetricKeys 取一批口径在同一粒度下的水位（BatchGetMetrics 解析 window_start=0）。
	// keys 的上限与 MetricWindowModel.ListByKeyWindows 同源（口径数不能由调用方决定 SQL 大小）。
	ListByMetricKeys(
		ctx context.Context, subjectType int32, windowType int32, keys []MetricKeyVersion,
	) ([]*WindowWatermark, error)
	// List 分页查询水位（观测「哪个口径的消费/聚合停在哪」）。
	List(ctx context.Context, f WatermarkFilter) ([]*WindowWatermark, error)
	// Count 同条件计数（分页 total）。
	Count(ctx context.Context, f WatermarkFilter) (int64, error)
	// DeleteByMetricVersion 删除某口径版本的全部水位（口径退役后的清理，单批 limit 行）。
	// 水位本身可由 spm_metric_window 重建，因此删除是安全的；反过来不行——
	// 只删指标不清水位，读取会继续停在已被重算掉的窗口上。
	DeleteByMetricVersion(ctx context.Context, metricKey string, version int32, limit int32) (int64, error)
	// WithSession 绑定事务句柄：水位必须与它描述的那批窗口写入在同一事务里推进，
	// 否则会出现「水位已前进、对应窗口还没落库」的空窗口读。
	WithSession(session sqlx.Session) WindowWatermarkModel
}

type defaultWindowWatermarkModel struct {
	// session 而不是 conn：sqlx.SqlConn 本身实现 sqlx.Session，
	// WithSession 之后读写都落在调用方的事务连接上（事务内不能再拿新连接）。
	session sqlx.Session
}

// NewWindowWatermarkModel 创建 WindowWatermarkModel 实现。
func NewWindowWatermarkModel(conn sqlx.SqlConn) WindowWatermarkModel {
	return &defaultWindowWatermarkModel{session: conn}
}

func (m *defaultWindowWatermarkModel) WithSession(session sqlx.Session) WindowWatermarkModel {
	if session == nil {
		return m
	}
	return &defaultWindowWatermarkModel{session: session}
}

const windowWatermarkColumns = "id, subject_type, metric_key, metric_version, window_type," +
	" last_closed_start, last_event_time, rows_written, update_request_id, ctime, mtime"

// validWatermarkSubject 允许 0（跨主体汇总水位）与全部合法主体类型。
// 汇总水位是给「全站榜 / 分区榜」用的：这些榜的主体维度是请求参数，不是口径属性。
func validWatermarkSubject(t int32) bool { return t == SubjectTypeUnspecified || ValidSubjectType(t) }

// checkWatermark 是写入前的入参校验（一律先于 SQL）：
// 水位是唯一键定位 + 单调推进的游标，一个未规整的 window_start 会让
// 「最近闭合窗口」指向一个根本不存在的窗口，读接口于是稳定返回空数据。
func checkWatermark(w *WindowWatermark) error {
	if w == nil {
		return ErrInvalidWindow
	}
	if strings.TrimSpace(w.MetricKey) == "" {
		return ErrMetricKeyEmpty
	}
	if w.MetricVersion <= 0 {
		return ErrMetricVersionRequired
	}
	if !validWatermarkSubject(w.SubjectType) {
		return ErrInvalidSubject
	}
	if !ValidWindowType(w.WindowType) || w.WindowType == WindowTypeTotal {
		// TOTAL 窗口的 window_start 恒为 0，「最近闭合」对它没有意义，
		// 建水位只会让读到 0 与「还没有水位」两种语义混在一起。
		return ErrInvalidWindow
	}
	if w.LastClosedStart < 0 {
		return ErrInvalidWindow
	}
	if w.LastClosedStart != AlignWindow(w.LastClosedStart, w.WindowType) {
		return ErrInvalidWindow
	}
	if w.LastEventTime <= 0 {
		// 与 MetricWindow.UpsertBatch 同一条线：event_time 是 stale 判定与
		// 「数据停在哪」的唯一依据，0 会让水位看起来永远严重过期。
		return ErrEventTimeRequired
	}
	if w.RowsWritten < 0 {
		return ErrNegativeMetricPoint
	}
	return nil
}

func (m *defaultWindowWatermarkModel) Advance(ctx context.Context, w *WindowWatermark) (bool, error) {
	if err := checkWatermark(w); err != nil {
		return false, err
	}
	now := nowUnix()
	// 推进用「条件 UPDATE」而不是 IF(...) 多列守卫：MySQL 的
	// ON DUPLICATE KEY UPDATE 从左到右求值，先更新 last_closed_start 之后，
	// 后续列的判定读到的就是新值，回退保护会静默失效。条件写进 WHERE 才是真守卫。
	res, err := m.session.ExecCtx(ctx,
		"UPDATE spm_window_watermark SET last_closed_start = ?, last_event_time = ?,"+
			" rows_written = ?, update_request_id = ?, mtime = ?"+
			" WHERE subject_type = ? AND metric_key = ? AND metric_version = ?"+
			" AND window_type = ? AND last_closed_start <= ?",
		w.LastClosedStart, w.LastEventTime, w.RowsWritten, truncate(w.UpdateRequestID, 128), now,
		w.SubjectType, truncate(w.MetricKey, 100), w.MetricVersion, w.WindowType, w.LastClosedStart)
	if err != nil {
		return false, fmt.Errorf("spm_window_watermark Advance: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("spm_window_watermark Advance RowsAffected: %w", err)
	}
	if affected > 0 {
		w.Ctime, w.Mtime = 0, now
		return true, nil
	}
	// 未命中：要么这一行还不存在（首次建立），要么是旧水位回退（幂等拒绝）。
	exists, err := m.Find(ctx, w.SubjectType, w.MetricKey, w.MetricVersion, w.WindowType)
	if err != nil {
		return false, err
	}
	if exists != nil {
		return false, nil
	}
	// 自赋值让并发首次插入的胜者拿到 affected=1、败者 affected=0，
	// 败者随后走上面的 Find 分支，得到与胜者一致的「已存在」结论。
	if _, err := m.session.ExecCtx(ctx,
		"INSERT INTO spm_window_watermark (subject_type, metric_key, metric_version,"+
			" window_type, last_closed_start, last_event_time, rows_written,"+
			" update_request_id, ctime, mtime)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"+
			" ON DUPLICATE KEY UPDATE metric_key = metric_key",
		w.SubjectType, truncate(w.MetricKey, 100), w.MetricVersion, w.WindowType,
		w.LastClosedStart, w.LastEventTime, w.RowsWritten, truncate(w.UpdateRequestID, 128),
		now, now); err != nil {
		return false, fmt.Errorf("spm_window_watermark Advance insert: %w", err)
	}
	w.Ctime, w.Mtime = now, now
	return true, nil
}

func (m *defaultWindowWatermarkModel) Find(
	ctx context.Context, subjectType int32, metricKey string, version, windowType int32,
) (*WindowWatermark, error) {
	if strings.TrimSpace(metricKey) == "" {
		return nil, ErrMetricKeyEmpty
	}
	if version <= 0 {
		return nil, ErrMetricVersionRequired
	}
	if !validWatermarkSubject(subjectType) || !ValidWindowType(windowType) {
		return nil, ErrInvalidWindow
	}
	var row WindowWatermark
	query := "SELECT " + windowWatermarkColumns + " FROM spm_window_watermark" +
		" WHERE subject_type = ? AND metric_key = ? AND metric_version = ? AND window_type = ?"
	if err := m.session.QueryRowCtx(ctx, &row, query, subjectType, metricKey, version, windowType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_window_watermark Find: %w", err)
	}
	return &row, nil
}

func (m *defaultWindowWatermarkModel) ListByMetricKeys(
	ctx context.Context, subjectType int32, windowType int32, keys []MetricKeyVersion,
) ([]*WindowWatermark, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > maxMetricKeysPerRequest {
		return nil, ErrTooManyKeys
	}
	if !validWatermarkSubject(subjectType) || !ValidWindowType(windowType) {
		return nil, ErrInvalidWindow
	}
	pairs := make([]string, 0, len(keys))
	args := []any{subjectType, windowType}
	for _, k := range keys {
		if strings.TrimSpace(k.MetricKey) == "" || k.Version <= 0 {
			return nil, ErrMetricVersionRequired
		}
		pairs = append(pairs, "(metric_key = ? AND metric_version = ?)")
		args = append(args, k.MetricKey, k.Version)
	}
	query := "SELECT " + windowWatermarkColumns + " FROM spm_window_watermark" +
		" WHERE subject_type = ? AND window_type = ? AND (" +
		strings.Join(pairs, " OR ") + ")" +
		" ORDER BY metric_key ASC, metric_version ASC"
	var rows []*WindowWatermark
	if err := m.session.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_window_watermark ListByMetricKeys: %w", err)
	}
	return rows, nil
}

func buildWatermarkQuery(f WatermarkFilter) (string, []any) {
	query := "SELECT " + windowWatermarkColumns + " FROM spm_window_watermark WHERE 1 = 1"
	var args []any
	if f.MetricKey != "" {
		query += " AND metric_key = ?"
		args = append(args, f.MetricKey)
	}
	if validWatermarkSubject(f.SubjectType) && f.SubjectType != SubjectTypeUnspecified {
		query += " AND subject_type = ?"
		args = append(args, f.SubjectType)
	}
	if f.MetricVersion != 0 {
		query += " AND metric_version = ?"
		args = append(args, f.MetricVersion)
	}
	if f.WindowType != WindowTypeUnspecified {
		query += " AND window_type = ?"
		args = append(args, f.WindowType)
	}
	return query, args
}

func (m *defaultWindowWatermarkModel) List(
	ctx context.Context, f WatermarkFilter,
) ([]*WindowWatermark, error) {
	query, args := buildWatermarkQuery(f)
	query += " ORDER BY metric_key ASC, metric_version ASC, subject_type ASC, window_type ASC" +
		" LIMIT ? OFFSET ?"
	args = append(args, clampLimit(f.Limit), clampOffset(f.Offset))
	var rows []*WindowWatermark
	if err := m.session.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_window_watermark List: %w", err)
	}
	return rows, nil
}

func (m *defaultWindowWatermarkModel) Count(ctx context.Context, f WatermarkFilter) (int64, error) {
	query, args := buildWatermarkQuery(f)
	query = strings.Replace(query, "SELECT "+windowWatermarkColumns, "SELECT COUNT(*)", 1)
	var n int64
	if err := m.session.QueryRowCtx(ctx, &n, query, args...); err != nil {
		return 0, fmt.Errorf("spm_window_watermark Count: %w", err)
	}
	return n, nil
}

func (m *defaultWindowWatermarkModel) DeleteByMetricVersion(
	ctx context.Context, metricKey string, version int32, limit int32,
) (int64, error) {
	if strings.TrimSpace(metricKey) == "" {
		return 0, ErrMetricKeyEmpty
	}
	if version <= 0 {
		return 0, ErrMetricVersionRequired
	}
	res, err := m.session.ExecCtx(ctx,
		"DELETE FROM spm_window_watermark WHERE metric_key = ? AND metric_version = ?"+
			" ORDER BY id ASC LIMIT ?",
		metricKey, version, clampBatch(limit))
	if err != nil {
		return 0, fmt.Errorf("spm_window_watermark DeleteByMetricVersion: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_window_watermark DeleteByMetricVersion RowsAffected: %w", err)
	}
	return n, nil
}
