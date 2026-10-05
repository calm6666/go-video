package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 留存分桶类型，与 rpc.GetRetentionReq.CohortType 逐值对齐。
const (
	CohortTypeUnspecified  int32 = 0
	CohortTypeRegisterDay  int32 = 1 // 按注册日分桶（依赖 account 的注册时间，见 README 契约缺口）
	CohortTypeFirstPlayDay int32 = 2 // 按首次播放日分桶（spm 自有事实可算）
)

// ValidCohortType 判断分桶类型是否合法。
func ValidCohortType(t int32) bool { return t == CohortTypeRegisterDay || t == CohortTypeFirstPlayDay }

// RetentionCohort 留存投影行（spm_retention_cohort 表投影）。
//
// 一行 = 「某分桶日的一批用户在第 N 日仍活跃」，rate 恒等于 retained/cohort_size：
// 只存比率而不在读取时重算的话，cohort_size 修正（例如事实补到迟到数据）之后
// 曲线就会和分子分母对不上。本表可从 spm_behavior_event 全量重算。
type RetentionCohort struct {
	ID            int64   `db:"id"`             // 主键 ID
	CohortType    int32   `db:"cohort_type"`    // 1 注册日、2 首次播放日
	CohortDate    int64   `db:"cohort_date"`    // 分桶日（UTC 零点，Unix 秒）
	ZoneID        int64   `db:"zone_id"`        // 0 = 全站
	MetricVersion int32   `db:"metric_version"` // 留存口径版本
	DayOffset     int32   `db:"day_offset"`     // 第 N 日（0 = 分桶当日）
	CohortSize    int64   `db:"cohort_size"`    // 分桶规模
	Retained      int64   `db:"retained"`       // 第 N 日仍活跃数
	Rate          float64 `db:"rate"`           // retained / cohort_size
	EventTime     int64   `db:"event_time"`     // 最近一次计算时间（Unix 秒）
	Ctime         int64   `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64   `db:"mtime"`          // 修改时间（Unix 秒）
}

// RetentionCohortModel spm_retention_cohort 表读写接口（可从事实重算的投影）。
type RetentionCohortModel interface {
	// UpsertBatch 幂等覆盖写入一批留存点（同 uniq_cohort 整行替换，绝不累加）。
	// 写入侧不变量：0 <= retained <= cohort_size 且 rate == retained/cohort_size，
	// 违反即拒绝整批——比率一旦落库就没有还原余地（见 rateTolerance）。
	UpsertBatch(ctx context.Context, rows []*RetentionCohort) (int64, error)
	// ListCurve 取某分桶日在 [0, maxDay] 的留存曲线。
	ListCurve(
		ctx context.Context, cohortType int32, cohortDate, zoneID int64,
		metricVersion, maxDay int32,
	) ([]*RetentionCohort, error)
	// FindOne 取单个留存点；不存在返回 nil。
	FindOne(
		ctx context.Context, cohortType int32, cohortDate, zoneID int64,
		metricVersion, dayOffset int32,
	) (*RetentionCohort, error)
	// DeleteExpired 删除分桶日早于 before 的留存行（留存曲线的保留期由配置决定）。
	DeleteExpired(ctx context.Context, before int64, limit int32) (int64, error)
}

type defaultRetentionCohortModel struct {
	conn sqlx.SqlConn
}

// NewRetentionCohortModel 创建 RetentionCohortModel 实现。
func NewRetentionCohortModel(conn sqlx.SqlConn) RetentionCohortModel {
	return &defaultRetentionCohortModel{conn: conn}
}

const retentionCohortColumns = "id, cohort_type, cohort_date, zone_id, metric_version," +
	" day_offset, cohort_size, retained, rate, event_time, ctime, mtime"

// retentionRowLen 是单个留存点的列数，与 retentionCohortInsertCols 对应。
const retentionRowLen = 11

const retentionCohortInsertCols = "cohort_type, cohort_date, zone_id, metric_version, day_offset," +
	" cohort_size, retained, rate, event_time, ctime, mtime"

func (m *defaultRetentionCohortModel) UpsertBatch(
	ctx context.Context, rows []*RetentionCohort,
) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	// 一批最多 1000 个留存点：一个 cohort 日 × 全站 + 各分区 × D0..D90 是 91×分区数，
	// 再多就该由调用方拆批，而不是把一条巨型的 INSERT ... ON DUPLICATE 交给 MySQL。
	if len(rows) > maxRetentionRowsPerBatch {
		return 0, ErrTooManyPoints
	}
	now := nowUnix()
	args := make([]any, 0, len(rows)*retentionRowLen)
	for _, r := range rows {
		if !ValidCohortType(r.CohortType) {
			return 0, ErrInvalidCohort
		}
		if r.MetricVersion <= 0 {
			return 0, ErrMetricVersionRequired
		}
		if r.CohortDate <= 0 {
			// cohort_date=0 在 dayStartUnix 下是个合法值，但它表示 1970-01-01 的分桶，
			// 落库后会永远留在曲线最左边并被当成「留存极好的老 cohort」。
			return 0, ErrInvalidCohort
		}
		if r.DayOffset < 0 || r.DayOffset > maxRetentionDay {
			return 0, ErrMaxDayTooLarge
		}
		// 落库不变量：retained 不可能大于 cohort_size，rate 恒等于 retained/cohort_size。
		// 不在这里守住，曲线上就会出现「第 30 日留存 120%」这种图，
		// 而读接口只能照抄——比率错了以后没有任何线索能追回原因。
		if r.CohortSize < 0 || r.Retained < 0 || r.Retained > r.CohortSize {
			return 0, ErrInvalidCohort
		}
		if r.CohortSize > 0 && math.Abs(r.Rate-float64(r.Retained)/float64(r.CohortSize)) > rateTolerance {
			return 0, ErrInvalidCohort
		}
		// 分桶日必须在 model 层再对齐一次：跨时区部署时未对齐的 cohort_date
		// 会把同一天的人拆进两个桶，曲线就此失真。
		r.CohortDate = dayStartUnix(r.CohortDate)
		// event_time 是「本次计算时刻」而不是事件时刻，缺省补 now 不改变任何口径语义。
		if r.EventTime <= 0 {
			r.EventTime = now
		}
		args = append(args,
			r.CohortType, r.CohortDate, r.ZoneID, r.MetricVersion, r.DayOffset,
			r.CohortSize, r.Retained, r.Rate, r.EventTime, now, now)
	}
	query := "INSERT INTO spm_retention_cohort (" + retentionCohortInsertCols +
		") VALUES " + rowPlaceholders(retentionRowLen, len(rows)) +
		" ON DUPLICATE KEY UPDATE cohort_size = VALUES(cohort_size)," +
		" retained = VALUES(retained), rate = VALUES(rate), event_time = VALUES(event_time)," +
		" mtime = VALUES(mtime)"
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("spm_retention_cohort UpsertBatch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_retention_cohort UpsertBatch RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultRetentionCohortModel) ListCurve(
	ctx context.Context, cohortType int32, cohortDate, zoneID int64,
	metricVersion, maxDay int32,
) ([]*RetentionCohort, error) {
	if !ValidCohortType(cohortType) {
		return nil, ErrInvalidCohort
	}
	if metricVersion <= 0 {
		return nil, ErrMetricVersionRequired
	}
	if maxDay <= 0 {
		maxDay = maxRetentionDay
	}
	if maxDay > maxRetentionDay {
		return nil, ErrMaxDayTooLarge
	}
	query := "SELECT " + retentionCohortColumns +
		" FROM spm_retention_cohort WHERE cohort_type = ? AND cohort_date = ? AND zone_id = ?" +
		" AND metric_version = ? AND day_offset BETWEEN 0 AND ?" +
		" ORDER BY day_offset ASC"
	var rows []*RetentionCohort
	err := m.conn.QueryRowsCtx(ctx, &rows, query,
		cohortType, dayStartUnix(cohortDate), zoneID, metricVersion, maxDay)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_retention_cohort ListCurve: %w", err)
	}
	return rows, nil
}

func (m *defaultRetentionCohortModel) FindOne(
	ctx context.Context, cohortType int32, cohortDate, zoneID int64,
	metricVersion, dayOffset int32,
) (*RetentionCohort, error) {
	var row RetentionCohort
	query := "SELECT " + retentionCohortColumns + " FROM spm_retention_cohort" +
		" WHERE cohort_type = ? AND cohort_date = ? AND zone_id = ?" +
		" AND metric_version = ? AND day_offset = ?"
	err := m.conn.QueryRowCtx(ctx, &row, query,
		cohortType, dayStartUnix(cohortDate), zoneID, metricVersion, dayOffset)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_retention_cohort FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultRetentionCohortModel) DeleteExpired(
	ctx context.Context, before int64, limit int32,
) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM spm_retention_cohort WHERE cohort_date < ? ORDER BY id ASC LIMIT ?",
		dayStartUnix(before), clampBatch(limit))
	if err != nil {
		return 0, fmt.Errorf("spm_retention_cohort DeleteExpired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_retention_cohort DeleteExpired RowsAffected: %w", err)
	}
	return n, nil
}

// maxRetentionDay 是留存曲线的天数上限，与契约里 max_day 的取值区间一致。
// 改动它等价于口径变更：D30 之外的点是否可信取决于事实保留期，不能悄悄放宽。
const maxRetentionDay = 90

// maxRetentionRowsPerBatch 是 UpsertBatch 单次提交的行数上限，
// 即「单条 INSERT 的最大行数，避免超长 SQL 与锁持有时间」。
// 取 1000：一个 cohort 日 × 全站 + 各分区 × D0..D90（maxRetentionDay=90）约 91×分区数量级，
// 1000 行 × retentionRowLen(11) = 1.1 万个占位符已经贴近 maxAllowedPacket 与
// InnoDB 行锁的舒适区，再大就该由调用方拆批而不是让 MySQL 一次吞下。
const maxRetentionRowsPerBatch = 1000

// rateTolerance 是「rate == retained/cohort_size」的浮点容差。
// 比率落库后是 DOUBLE，读写两侧的表示误差在 1e-16 量级；给 1e-9 既能放过
// 正常计算，又能拦住「把百分比 42.0 当成比率 0.42 写错方向」这类真实错误。
const rateTolerance = 1e-9
