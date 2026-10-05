package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// metricChangeLogColumns 是 cr_metric_change_log 的完整列清单，
// 与 deploy/migrations/creator-revenue/000001_create_creator_revenue_tables.sql 一一对应。
const metricChangeLogColumns = "log_id, period, mid, aid, source_type, rule_code, " +
	"old_rule_version, new_rule_version, old_quantity, new_quantity, " +
	"old_amount_minor, new_amount_minor, old_capped_amount_minor, new_capped_amount_minor, " +
	"operator, reason, request_id, ctime"

// MetricChangeLog 计量更正台账。
//
// 为什么必须存在：cr_metric 的唯一键让「重复上报」变成就地覆盖，如果只留新值，
// 「原来算多少、谁在什么时候依据什么改的」就永久丢失，结算争议无法复核。
// 本表只追加、不修改，是与主表同事务写入的审计证据（AGENTS.md §5、§8）。
type MetricChangeLog struct {
	LogId                int64  `db:"log_id"`
	Period               string `db:"period"`
	Mid                  int64  `db:"mid"`
	Aid                  int64  `db:"aid"`
	SourceType           int32  `db:"source_type"`
	RuleCode             string `db:"rule_code"`
	OldRuleVersion       int64  `db:"old_rule_version"`
	NewRuleVersion       int64  `db:"new_rule_version"`
	OldQuantity          int64  `db:"old_quantity"`
	NewQuantity          int64  `db:"new_quantity"`
	OldAmountMinor       int64  `db:"old_amount_minor"`
	NewAmountMinor       int64  `db:"new_amount_minor"`
	OldCappedAmountMinor int64  `db:"old_capped_amount_minor"`
	NewCappedAmountMinor int64  `db:"new_capped_amount_minor"`
	Operator             string `db:"operator"`
	Reason               string `db:"reason"`
	RequestId            string `db:"request_id"`
	Ctime                int64  `db:"ctime"`
}

// MetricChangeLogModel cr_metric_change_log 表写入接口（只追加）。
type MetricChangeLogModel interface {
	// Insert 追加一条更正台账。与 cr_metric 的就地更正同事务提交。
	Insert(ctx context.Context, l *MetricChangeLog) (int64, error)
}

type defaultMetricChangeLogModel struct {
	conn sqlx.SqlConn
}

// NewMetricChangeLogModel 创建 cr_metric_change_log 的数据访问对象。
func NewMetricChangeLogModel(conn sqlx.SqlConn) MetricChangeLogModel {
	return &defaultMetricChangeLogModel{conn: conn}
}

func (m *defaultMetricChangeLogModel) Insert(ctx context.Context, l *MetricChangeLog) (int64, error) {
	if l.Ctime == 0 {
		l.Ctime = nowUnix()
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO cr_metric_change_log (period, mid, aid, source_type, rule_code, "+
			"old_rule_version, new_rule_version, old_quantity, new_quantity, "+
			"old_amount_minor, new_amount_minor, old_capped_amount_minor, new_capped_amount_minor, "+
			"operator, reason, request_id, ctime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		l.Period, l.Mid, l.Aid, l.SourceType, l.RuleCode,
		l.OldRuleVersion, l.NewRuleVersion, l.OldQuantity, l.NewQuantity,
		l.OldAmountMinor, l.NewAmountMinor, l.OldCappedAmountMinor, l.NewCappedAmountMinor,
		l.Operator, l.Reason, l.RequestId, l.Ctime)
	if err != nil {
		return 0, fmt.Errorf("cr_metric_change_log Insert(%s,%d,%d,%d): %w",
			l.Period, l.Mid, l.Aid, l.SourceType, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("cr_metric_change_log LastInsertId(%s,%d): %w", l.Period, l.Mid, err)
	}
	return id, nil
}
