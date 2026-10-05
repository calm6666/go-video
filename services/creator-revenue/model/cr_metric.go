package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 收益来源类型常量，与 cr_metric.source_type / cr_revenue_rule.source_type
// 和 rpc.RevenueSourceType 取值严格一致。禁止重排（会破坏已落库台账的语义）。
const (
	SourceTypeUnspecified int32 = 0
	SourceTypeVipWatch    int32 = 1 // 会员有效观看时长折算
	SourceTypeCoin        int32 = 2 // 收到的投币折算
	SourceTypeInteraction int32 = 3 // 有效互动（点赞/收藏/分享）折算
	SourceTypeActivity    int32 = 4 // 运营活动激励（手工回填，必须有 reason）
)

// SourceTypeValid 判定来源类型是否落在枚举内。
func SourceTypeValid(v int32) bool { return v >= SourceTypeVipWatch && v <= SourceTypeActivity }

// metricColumns 是 cr_metric 的完整列清单，
// 与 deploy/migrations/creator-revenue/000001_create_creator_revenue_tables.sql 一一对应。
const metricColumns = "metric_id, period, mid, aid, source_type, rule_code, rule_version, " +
	"quantity, unit, amount_minor, capped_amount_minor, threshold_blocked, source_detail, " +
	"corrected, ctime, mtime"

// RevenueMetric 计量台账行（DB 投影）。
//
// 一条 = 某周期、某内容、某来源的折算结果。唯一键 (period, mid, aid, source_type)
// 是「重复上报视为更正」的判定依据；更正前的旧值进 cr_metric_change_log，不静默覆盖。
//
// corrected 只在 DB 侧存在（rpc.RevenueMetricInfo 没有该字段），
// 因为契约未给读侧暴露「这行被更正过」的位置——已记入交付报告的契约缺口。
type RevenueMetric struct {
	MetricId          int64  `db:"metric_id"`
	Period            string `db:"period"`
	Mid               int64  `db:"mid"`
	Aid               int64  `db:"aid"`
	SourceType        int32  `db:"source_type"`
	RuleCode          string `db:"rule_code"`
	RuleVersion       int64  `db:"rule_version"`
	Quantity          int64  `db:"quantity"`
	Unit              string `db:"unit"`
	AmountMinor       int64  `db:"amount_minor"`
	CappedAmountMinor int64  `db:"capped_amount_minor"`
	ThresholdBlocked  int32  `db:"threshold_blocked"`
	SourceDetail      string `db:"source_detail"`
	Corrected         int32  `db:"corrected"`
	Ctime             int64  `db:"ctime"`
	Mtime             int64  `db:"mtime"`
}

// MetricSourceSum 按来源聚合的台账合计（出单与概览的输入）。
type MetricSourceSum struct {
	SourceType        int32  `db:"source_type"`
	RuleCode          string `db:"rule_code"`
	Quantity          int64  `db:"quantity"`
	AmountMinor       int64  `db:"amount_minor"`
	CappedAmountMinor int64  `db:"capped_amount_minor"`
	MetricCount       int64  `db:"metric_count"`
}

// RevenueMetricModel cr_metric 表读写接口。
type RevenueMetricModel interface {
	// Insert 写入台账行，返回自增 metric_id。
	// uniq_metric_key 命中时返回可被 IsDuplicateErr 识别的错误（走更正分支）。
	Insert(ctx context.Context, m *RevenueMetric) (int64, error)
	// FindByKey 按唯一键查询；不存在返回 (nil, nil)。
	FindByKey(ctx context.Context, period string, mid, aid int64, sourceType int32) (*RevenueMetric, error)
	// LockByKey 按唯一键加行锁读取（更正路径需构造在事务会话上）。
	LockByKey(ctx context.Context, period string, mid, aid int64, sourceType int32) (*RevenueMetric, error)
	// UpdateCorrection 就地更正：写新数量/新金额并置 corrected=1。
	// 带 metric_id + 旧 quantity 的 CAS，未命中返回 false（并发更正，让调用方重试）。
	UpdateCorrection(ctx context.Context, m *RevenueMetric, oldQuantity int64) (bool, error)
	// SetCappedAmount 重算某行的封顶后金额（同组封顶分配重算用，不改原始数量）。
	SetCappedAmount(ctx context.Context, metricID, cappedAmount int64) error
	// ListGroupForUpdate 按 (period, mid, source_type) 取同组全部台账行并加锁，
	// 供月度封顶重算在同一事务里遍历；按 aid、metric_id 升序，保证分配顺序确定。
	ListGroupForUpdate(ctx context.Context, period string, mid int64, sourceType int32) ([]*RevenueMetric, error)
	// List 多条件分页（period 非空或 mid 非零至少要有一个，避免全表扫）。
	List(ctx context.Context, period string, mid, aid int64, sourceType int32, offset, limit int64) ([]*RevenueMetric, error)
	// Count 同 List 条件总数。
	Count(ctx context.Context, period string, mid, aid int64, sourceType int32) (int64, error)
	// SumByPeriodMid 按来源聚合某作者某周期的台账合计（出单输入）。
	SumByPeriodMid(ctx context.Context, period string, mid int64) ([]MetricSourceSum, error)
	// SumCappedByPeriod 某作者某周期封顶后应计合计（GetRevenueSummary 的本月预估）。
	SumCappedByPeriod(ctx context.Context, period string, mid int64) (int64, error)
	// ListSettleableMids 返回该周期有台账且已 ENROLLED 的作者 mid（升序、限量）。
	ListSettleableMids(ctx context.Context, period string, limit int64) ([]int64, error)
}

// ComputeAmountMinor 按规则把计量数量折算成应计金额（分）。
//
// 公式：amount = quantity * unit_price_per_1000_minor / 1000，整数除法。
// 取整方向：quantity 与单价都非负，所以 Go 的整除就是向下取整（向零取整 == 向下），
//
//	余数（< 1 分）被丢弃且**不向后续周期追溯补偿**——这是刻意选择：
//	单条台账的误差 < 1 分，方向恒为「少算」，即误差有利于平台而不是作者，
//	绝不会出现「平台欠账被抹掉、作者多领」的资金风险。
//	若要改取整方向（四舍五入或余数结转），必须先改本函数注释与 README 口径，
//	并同步历史台账的重算方案，不能让某次改动悄悄把往月金额算高。
//
// 负 quantity / 负单价一律拒绝（负数会把「应付」算成「倒扣」，属于脏数据入口）。
// 溢出保护：quantity * 单价 可能撑爆 int64（异常量级），先做除法前置判定再报错，
// 不返回回绕后的负数。
func ComputeAmountMinor(quantity, unitPricePer1000 int64) (int64, error) {
	if quantity < 0 {
		return 0, fmt.Errorf("%w: quantity=%d", ErrNegativeQuantity, quantity)
	}
	if unitPricePer1000 < 0 {
		return 0, fmt.Errorf("%w: unit_price_per_1000_minor=%d", ErrNegativeUnitPrice, unitPricePer1000)
	}
	if quantity > 0 && unitPricePer1000 > math.MaxInt64/int64(quantity) {
		return 0, fmt.Errorf("%w: quantity=%d unit_price_per_1000_minor=%d",
			ErrAmountOverflow, quantity, unitPricePer1000)
	}
	return quantity * unitPricePer1000 / 1000, nil
}

// ApplyThreshold 防刷门槛判定：quantity 低于 min_quantity 时整条不结算。
// 返回 (参与封顶的金额, 是否被门槛拦掉)。被拦掉时 amount_minor 仍保留公式算出的应计
// （「这条本该有多少」是复核信息），capped_amount_minor 置 0 并在 source_detail 说明；
// 同时把 threshold_blocked 置 1，让月度封顶重算能确定性地把它排除在额度分配之外，
// 不依赖「规则当时的门槛值」这种历史推断。
func ApplyThreshold(quantity, minQuantity, amountMinor int64) (int64, bool) {
	if minQuantity > 0 && quantity < minQuantity {
		return 0, true
	}
	return amountMinor, false
}

// AllocateCap 把月度封顶分配到单条台账行。
//
// room 是「同 (period, mid, source_type) 组内本行之前已占用的额度」剩余量，
// 由调用方按 aid、metric_id 升序累计得出，保证同一批数据在任何写入顺序下
// 得到相同的分配结果（重算/更正可复现，不会出现「同一份台账两次算出不同金额」）。
// 返回 (封顶后金额, 被封顶扣掉的额度)。
func AllocateCap(amount, room int64) (int64, int64) {
	if amount <= 0 {
		return 0, 0
	}
	if room < 0 {
		room = 0
	}
	if amount <= room {
		return amount, 0
	}
	return room, amount - room
}

type defaultRevenueMetricModel struct {
	conn sqlx.SqlConn
}

// NewRevenueMetricModel 创建 cr_metric 的数据访问对象。
func NewRevenueMetricModel(conn sqlx.SqlConn) RevenueMetricModel {
	return &defaultRevenueMetricModel{conn: conn}
}

func (m *defaultRevenueMetricModel) Insert(ctx context.Context, row *RevenueMetric) (int64, error) {
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO cr_metric (period, mid, aid, source_type, rule_code, rule_version, quantity, unit, "+
			"amount_minor, capped_amount_minor, threshold_blocked, source_detail, corrected, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		row.Period, row.Mid, row.Aid, row.SourceType, row.RuleCode, row.RuleVersion, row.Quantity, row.Unit,
		row.AmountMinor, row.CappedAmountMinor, row.ThresholdBlocked, row.SourceDetail, row.Corrected, now, now)
	if err != nil {
		return 0, fmt.Errorf("cr_metric Insert(%s,%d,%d,%d): %w",
			row.Period, row.Mid, row.Aid, row.SourceType, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("cr_metric LastInsertId(%s,%d): %w", row.Period, row.Mid, err)
	}
	return id, nil
}

func (m *defaultRevenueMetricModel) FindByKey(
	ctx context.Context, period string, mid, aid int64, sourceType int32,
) (*RevenueMetric, error) {
	var row RevenueMetric
	query := "SELECT " + metricColumns + " FROM cr_metric " +
		"WHERE period = ? AND mid = ? AND aid = ? AND source_type = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, period, mid, aid, sourceType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_metric FindByKey(%s,%d,%d,%d): %w", period, mid, aid, sourceType, err)
	}
	return &row, nil
}

func (m *defaultRevenueMetricModel) LockByKey(
	ctx context.Context, period string, mid, aid int64, sourceType int32,
) (*RevenueMetric, error) {
	var row RevenueMetric
	query := "SELECT " + metricColumns + " FROM cr_metric " +
		"WHERE period = ? AND mid = ? AND aid = ? AND source_type = ? LIMIT 1 FOR UPDATE"
	if err := m.conn.QueryRowCtx(ctx, &row, query, period, mid, aid, sourceType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_metric LockByKey(%s,%d,%d,%d): %w", period, mid, aid, sourceType, err)
	}
	return &row, nil
}

func (m *defaultRevenueMetricModel) UpdateCorrection(
	ctx context.Context, row *RevenueMetric, oldQuantity int64,
) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cr_metric SET rule_code = ?, rule_version = ?, quantity = ?, unit = ?, "+
			"amount_minor = ?, capped_amount_minor = ?, threshold_blocked = ?, source_detail = ?, "+
			"corrected = 1, mtime = ? "+
			"WHERE metric_id = ? AND quantity = ?",
		row.RuleCode, row.RuleVersion, row.Quantity, row.Unit, row.AmountMinor, row.CappedAmountMinor,
		row.ThresholdBlocked, row.SourceDetail, nowUnix(), row.MetricId, oldQuantity)
	if err != nil {
		return false, fmt.Errorf("cr_metric UpdateCorrection(%d): %w", row.MetricId, err)
	}
	return rowsAffected(res)
}

func (m *defaultRevenueMetricModel) SetCappedAmount(
	ctx context.Context, metricID, cappedAmount int64,
) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE cr_metric SET capped_amount_minor = ?, mtime = ? WHERE metric_id = ?",
		cappedAmount, nowUnix(), metricID)
	if err != nil {
		return fmt.Errorf("cr_metric SetCappedAmount(%d): %w", metricID, err)
	}
	return nil
}

func (m *defaultRevenueMetricModel) ListGroupForUpdate(
	ctx context.Context, period string, mid int64, sourceType int32,
) ([]*RevenueMetric, error) {
	var rows []*RevenueMetric
	query := "SELECT " + metricColumns + " FROM cr_metric " +
		"WHERE period = ? AND mid = ? AND source_type = ? ORDER BY aid ASC, metric_id ASC FOR UPDATE"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, period, mid, sourceType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_metric ListGroupForUpdate(%s,%d,%d): %w", period, mid, sourceType, err)
	}
	return rows, nil
}

func (m *defaultRevenueMetricModel) List(
	ctx context.Context, period string, mid, aid int64, sourceType int32, offset, limit int64,
) ([]*RevenueMetric, error) {
	where, args := metricFilter(period, mid, aid, sourceType)
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []*RevenueMetric
	query := "SELECT " + metricColumns + " FROM cr_metric WHERE " + where +
		" ORDER BY period ASC, mid ASC, aid ASC, source_type ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_metric List: %w", err)
	}
	return rows, nil
}

func (m *defaultRevenueMetricModel) Count(
	ctx context.Context, period string, mid, aid int64, sourceType int32,
) (int64, error) {
	where, args := metricFilter(period, mid, aid, sourceType)
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM cr_metric WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cr_metric Count: %w", err)
	}
	return total, nil
}

func (m *defaultRevenueMetricModel) SumByPeriodMid(
	ctx context.Context, period string, mid int64,
) ([]MetricSourceSum, error) {
	var rows []MetricSourceSum
	query := "SELECT source_type, rule_code, SUM(quantity) AS quantity, " +
		"SUM(amount_minor) AS amount_minor, SUM(capped_amount_minor) AS capped_amount_minor, " +
		"COUNT(*) AS metric_count FROM cr_metric WHERE period = ? AND mid = ? " +
		"GROUP BY source_type, rule_code " +
		"ORDER BY source_type ASC, capped_amount_minor DESC, rule_code ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, period, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_metric SumByPeriodMid(%s,%d): %w", period, mid, err)
	}
	return rows, nil
}

func (m *defaultRevenueMetricModel) SumCappedByPeriod(
	ctx context.Context, period string, mid int64,
) (int64, error) {
	var total sql.NullInt64
	query := "SELECT SUM(capped_amount_minor) FROM cr_metric WHERE period = ? AND mid = ?"
	if err := m.conn.QueryRowCtx(ctx, &total, query, period, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cr_metric SumCappedByPeriod(%s,%d): %w", period, mid, err)
	}
	if !total.Valid {
		return 0, nil // 无行时 SUM 返回 NULL，这是「0 应计」而不是查询失败
	}
	return total.Int64, nil
}

func (m *defaultRevenueMetricModel) ListSettleableMids(
	ctx context.Context, period string, limit int64,
) ([]int64, error) {
	if limit <= 0 {
		limit = 500
	}
	var mids []int64
	// 只 JOIN 本服务自己的两张表（AGENTS.md §5）：作者资料在 user-profile/creator，
	// 本服务只认 mid，不复制可变主资料。
	query := "SELECT DISTINCT m.mid FROM cr_metric m " +
		"JOIN cr_enrollment e ON e.mid = m.mid AND e.state = ? " +
		"WHERE m.period = ? ORDER BY m.mid ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &mids, query, EnrollmentStateEnrolled, period, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_metric ListSettleableMids(%s): %w", period, err)
	}
	return mids, nil
}

// metricFilter 构造台账列表/计数的共用 WHERE。至少带 period 或 mid 之一，
// 否则 cr_metric 是亿级表，无界扫描会拖垮主库（logic 侧已先拒，这里再兜一层）。
func metricFilter(period string, mid, aid int64, sourceType int32) (string, []any) {
	conds := make([]string, 0, 4)
	var args []any
	if period != "" {
		conds = append(conds, "period = ?")
		args = append(args, period)
	}
	if mid != 0 {
		conds = append(conds, "mid = ?")
		args = append(args, mid)
	}
	if aid != 0 {
		conds = append(conds, "aid = ?")
		args = append(args, aid)
	}
	if sourceType != 0 {
		conds = append(conds, "source_type = ?")
		args = append(args, sourceType)
	}
	if len(conds) == 0 {
		return "1 = 1", args
	}
	return strings.Join(conds, " AND "), args
}
