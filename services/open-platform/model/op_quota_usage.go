package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// QuotaUsage 配额用量投影（op_quota_usage 表）：应用 × 接口 × 窗口起点 → 已用次数。
//
// 为什么是投影而不是事实源：
//   - 真值是 op_api_call_log 的调用流水（append-only），本表只是把「按窗口计数」这件事
//     预聚合，避免每次网关鉴权都去扫流水。
//   - 计数丢失或漂移的后果只是「短时放宽/收紧限额」，不会破坏任何业务数据；
//     RecomputeQuota 随时可以从流水重算（RecomputeOverwrite）。
//   - 因此本表可以安全地按保留期清理旧窗口（PurgeOlderThan），也不需要备份级严格性。
//
// 窗口取齐：window_start = AlignWindow(ts, window_seconds)，全局同一规则，重算与扣减共用。
type QuotaUsage struct {
	// UsageID 自增主键
	UsageID int64 `db:"usage_id"`
	// AppID 应用 ID（全局默认规则用 0 记账，与 op_quota_policy.app_id 对齐）
	AppID int64 `db:"app_id"`
	// APICode 接口标识（"*" 表示合并口径）
	APICode string `db:"api_code"`
	// WindowSeconds 窗口长度（秒）
	WindowSeconds int64 `db:"window_seconds"`
	// WindowStart 窗口起点（Unix 秒，已取齐）
	WindowStart int64 `db:"window_start"`
	// WindowEnd 窗口终点（Unix 秒 = window_start + window_seconds）。
	// 单独存列的原因：清理任务要按「窗口是否已结束」扫描，
	// 写成 window_start + window_seconds < ? 会让索引失效，变成全表扫。
	WindowEnd int64 `db:"window_end"`
	// Used 已用次数
	Used int64 `db:"used"`
	// LimitSnapshot 记账时的限额快照（解释“为什么被拒”，不代表当前生效限额）
	LimitSnapshot int64 `db:"limit_snapshot"`
	// UpdatedAt 最近一次累加时间（Unix 秒）
	UpdatedAt int64 `db:"updated_at"`
	// Ctime 窗口首次记账时间（Unix 秒）
	Ctime int64 `db:"ctime"`
}

// Remaining 计算剩余可用次数（负数按 0 处理，限额被下调时不返回负数）。
func (u *QuotaUsage) Remaining() int64 {
	if u == nil {
		return 0
	}
	r := u.LimitSnapshot - u.Used
	if r < 0 {
		return 0
	}
	return r
}

// Exceeded 判断是否超限（limit<=0 表示禁用，任何调用都算超限）。
func (u *QuotaUsage) Exceeded() bool {
	if u == nil {
		return false
	}
	return u.LimitSnapshot <= 0 || u.Used > u.LimitSnapshot
}

// ResetAt 返回窗口结束时刻（Unix 秒），用于 quota_remaining 的 window_reset_at。
func (u *QuotaUsage) ResetAt() int64 {
	if u == nil {
		return 0
	}
	if u.WindowEnd > 0 {
		return u.WindowEnd
	}
	return u.WindowStart + u.WindowSeconds
}

// windowEndOf 计算窗口终点，与 ResetAt 保持同一口径（写入路径共用，避免两处漂移）。
func windowEndOf(windowStart, windowSeconds int64) int64 {
	return windowStart + windowSeconds
}

// QuotaUsageModel op_quota_usage 表读写接口。
type QuotaUsageModel interface {
	// Add 原子累加并回读当前用量。delta 通常为 1（每次真实扣减），重算路径不用它。
	// 说明：累加在 SQL 内原子完成（不会丢更新），回读值是高并发下的“下界”，
	// 判定超限时以此为准可能偶尔放行 1 次，属于配额投影可接受误差；
	// 真正的“同一 request_id 不重复扣配额”由 op_api_call_log.uniq_request_id 保证。
	Add(ctx context.Context, appID int64, apiCode string, windowSeconds, windowStart, delta,
		limitSnapshot int64) (*QuotaUsage, error)
	// Peek 只读窗口用量（不扣减），不存在返回 (nil, nil)。
	Peek(ctx context.Context, appID int64, apiCode string, windowSeconds, windowStart int64) (*QuotaUsage, error)
	// ListByApp 查询某应用某接口（apiCode 为空表示全部）在指定窗口起点的用量。
	// windowStart=0 表示「每个窗口长度各取当前窗口」，由调用方传入已对齐的时间戳。
	ListByApp(ctx context.Context, appID int64, apiCode string, windowStart int64) ([]*QuotaUsage, error)
	// ListWindowsInRange 列出区间内已存在的窗口行（重算时用于找出“流水有记录但投影缺失”的窗口）。
	ListWindowsInRange(ctx context.Context, appID int64, apiCode string, windowSeconds,
		from, to int64) ([]*QuotaUsage, error)
	// RecomputeOverwrite 用流水真值覆盖窗口计数，返回修正量 delta（新值-旧值，负数表示之前多计）。
	// 唯一键 upsert，cron 可反复执行且结果收敛。
	RecomputeOverwrite(ctx context.Context, appID int64, apiCode string, windowSeconds, windowStart,
		used, limitSnapshot int64) (delta int64, err error)
	// PurgeOlderThan 清理过期窗口投影行（可重建，不影响真值）。
	PurgeOlderThan(ctx context.Context, before int64, limit int32) (int64, error)
}

type defaultQuotaUsageModel struct {
	conn sqlx.SqlConn
}

// NewQuotaUsageModel 创建 QuotaUsageModel 实现。
func NewQuotaUsageModel(conn sqlx.SqlConn) QuotaUsageModel {
	return &defaultQuotaUsageModel{conn: conn}
}

const quotaUsageColumns = `usage_id, app_id, api_code, window_seconds, window_start, window_end, used,
	limit_snapshot, updated_at, ctime`

func (m *defaultQuotaUsageModel) Add(ctx context.Context, appID int64, apiCode string, windowSeconds,
	windowStart, delta, limitSnapshot int64) (*QuotaUsage, error) {
	if windowSeconds <= 0 {
		return nil, ErrWindowInvalid
	}
	if apiCode == "" {
		return nil, ErrQuotaPolicyNotFound
	}
	if delta <= 0 {
		delta = 1
	}
	now := nowUnix()
	// used = used + VALUES(used)：MySQL 的 ON DUPLICATE KEY UPDATE 写法，
	// 单语句内完成“建行或累加”，不需要先查后改（避免并发丢更新）。
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_quota_usage (app_id, api_code, window_seconds, window_start, window_end, used, "+
			"limit_snapshot, updated_at, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE used = used + VALUES(used), limit_snapshot = VALUES(limit_snapshot), "+
			"updated_at = VALUES(updated_at)",
		appID, apiCode, windowSeconds, windowStart, windowEndOf(windowStart, windowSeconds), delta,
		limitSnapshot, now, now)
	if err != nil {
		return nil, fmt.Errorf("op_quota_usage Add: %w", err)
	}
	return m.Peek(ctx, appID, apiCode, windowSeconds, windowStart)
}

func (m *defaultQuotaUsageModel) Peek(ctx context.Context, appID int64, apiCode string, windowSeconds,
	windowStart int64) (*QuotaUsage, error) {
	var u QuotaUsage
	err := m.conn.QueryRowCtx(ctx, &u,
		"SELECT "+quotaUsageColumns+" FROM op_quota_usage WHERE app_id = ? AND api_code = ? AND "+
			"window_seconds = ? AND window_start = ? LIMIT 1",
		appID, apiCode, windowSeconds, windowStart)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_quota_usage Peek: %w", err)
	}
	return &u, nil
}

func (m *defaultQuotaUsageModel) ListByApp(ctx context.Context, appID int64, apiCode string,
	windowStart int64) ([]*QuotaUsage, error) {
	conds := []string{"app_id = ?"}
	args := []any{appID}
	if apiCode != "" {
		conds = append(conds, "api_code = ?")
		args = append(args, apiCode)
	}
	if windowStart > 0 {
		conds = append(conds, "window_start = ?")
		args = append(args, windowStart)
	}
	query := "SELECT " + quotaUsageColumns + " FROM op_quota_usage WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY window_start DESC, api_code ASC, window_seconds ASC"
	var rows []*QuotaUsage
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_quota_usage ListByApp: %w", err)
	}
	return rows, nil
}

func (m *defaultQuotaUsageModel) ListWindowsInRange(ctx context.Context, appID int64, apiCode string,
	windowSeconds, from, to int64) ([]*QuotaUsage, error) {
	if windowSeconds <= 0 {
		return nil, ErrWindowInvalid
	}
	if to <= from {
		return nil, ErrWindowInvalid
	}
	conds := []string{"window_seconds = ?", "window_start >= ?", "window_start < ?"}
	args := []any{windowSeconds, from, to}
	if appID > 0 {
		conds = append(conds, "app_id = ?")
		args = append(args, appID)
	}
	if apiCode != "" {
		conds = append(conds, "api_code = ?")
		args = append(args, apiCode)
	}
	query := "SELECT " + quotaUsageColumns + " FROM op_quota_usage WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY window_start ASC"
	var rows []*QuotaUsage
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_quota_usage ListWindowsInRange: %w", err)
	}
	return rows, nil
}

func (m *defaultQuotaUsageModel) RecomputeOverwrite(ctx context.Context, appID int64, apiCode string,
	windowSeconds, windowStart, used, limitSnapshot int64) (int64, error) {
	if windowSeconds <= 0 {
		return 0, ErrWindowInvalid
	}
	if used < 0 {
		used = 0
	}
	now := nowUnix()
	old, err := m.Peek(ctx, appID, apiCode, windowSeconds, windowStart)
	if err != nil {
		return 0, err
	}
	oldUsed := int64(0)
	if old != nil {
		oldUsed = old.Used
	}
	// 重算直接覆盖窗口计数：并发下真实请求仍在累加，因此重算结果只是把投影拉回真值附近，
	// 最终以 op_api_call_log 流水为准（下一次重算继续收敛）。
	if _, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_quota_usage (app_id, api_code, window_seconds, window_start, window_end, used, "+
			"limit_snapshot, updated_at, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE used = VALUES(used), limit_snapshot = VALUES(limit_snapshot), "+
			"updated_at = VALUES(updated_at)",
		appID, apiCode, windowSeconds, windowStart, windowEndOf(windowStart, windowSeconds), used,
		limitSnapshot, now, now); err != nil {
		return 0, fmt.Errorf("op_quota_usage RecomputeOverwrite: %w", err)
	}
	return used - oldUsed, nil
}

func (m *defaultQuotaUsageModel) PurgeOlderThan(ctx context.Context, before int64, limit int32) (int64, error) {
	if limit <= 0 {
		return 0, ErrInvalidPage
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM op_quota_usage WHERE window_end < ? LIMIT ?", before, limit)
	if err != nil {
		return 0, fmt.Errorf("op_quota_usage PurgeOlderThan: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_quota_usage PurgeOlderThan RowsAffected: %w", err)
	}
	return aff, nil
}
