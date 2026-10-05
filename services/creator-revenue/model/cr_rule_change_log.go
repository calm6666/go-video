package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 规则变更动作，与 cr_rule_change_log.action 列一致（只用于台账解读，不进 RPC 契约）。
const (
	// RuleActionCreate 首次落规则（无历史版本）。
	RuleActionCreate = "CREATE"
	// RuleActionUpdate 修改 DRAFT 规则的字段/单价。
	RuleActionUpdate = "UPDATE"
	// RuleActionActivate DRAFT→ACTIVE。
	RuleActionActivate = "ACTIVATE"
	// RuleActionArchive 置为 ARCHIVED（人工归档）。
	RuleActionArchive = "ARCHIVE"
	// RuleActionAutoArchive 因「同一来源至多一条 ACTIVE」被系统自动归档的旧规则。
	RuleActionAutoArchive = "AUTO_ARCHIVE"
)

// ruleChangeLogColumns 是 cr_rule_change_log 的完整列清单，
// 与 deploy/migrations/creator-revenue/000001_create_creator_revenue_tables.sql 一一对应。
const ruleChangeLogColumns = "log_id, rule_code, source_type, action, from_state, to_state, " +
	"from_unit_price_per_1000_minor, to_unit_price_per_1000_minor, from_version, to_version, " +
	"operator, reason, request_id, ctime"

// RuleChangeLog 规则变更台账行。
//
// 为什么必须存在：单价与状态的每次变化都会改写历史周期「本应计多少」，
// 出现结算争议时唯一可复核的证据就是这里（AGENTS.md §8 的「保留审计证据」）。
// rule_code 唯一、主表就地更新，所以历史版本只在本表按 from_/to_ 成对留存。
type RuleChangeLog struct {
	LogId         int64  `db:"log_id"`
	RuleCode      string `db:"rule_code"`
	SourceType    int32  `db:"source_type"`
	Action        string `db:"action"`
	FromState     int32  `db:"from_state"`
	ToState       int32  `db:"to_state"`
	FromUnitPrice int64  `db:"from_unit_price_per_1000_minor"`
	ToUnitPrice   int64  `db:"to_unit_price_per_1000_minor"`
	FromVersion   int64  `db:"from_version"`
	ToVersion     int64  `db:"to_version"`
	Operator      string `db:"operator"`
	Reason        string `db:"reason"`
	RequestId     string `db:"request_id"`
	Ctime         int64  `db:"ctime"`
}

// RuleChangeLogModel cr_rule_change_log 表读写接口。
type RuleChangeLogModel interface {
	// Insert 追加一条变更台账；uniq_request_id 命中时返回可被 IsDuplicateErr 识别的错误。
	Insert(ctx context.Context, l *RuleChangeLog) (int64, error)
	// FindByRequest 按幂等键反查（判定 request_id 重放）。不存在返回 (nil, nil)。
	FindByRequest(ctx context.Context, requestID string) (*RuleChangeLog, error)
	// FirstChangeFrom 返回该规则「from_version >= version」的首条变更，
	// 即 version 这一版生效期间的单价与状态取值。不存在返回 (nil, nil)。
	FirstChangeFrom(ctx context.Context, ruleCode string, version int64) (*RuleChangeLog, error)
	// ListByRule 按版本倒序返回该规则的变更台账（复核面板/争议定位用）。
	ListByRule(ctx context.Context, ruleCode string, limit int64) ([]*RuleChangeLog, error)
}

type defaultRuleChangeLogModel struct {
	conn sqlx.SqlConn
}

// NewRuleChangeLogModel 创建 cr_rule_change_log 的数据访问对象。
func NewRuleChangeLogModel(conn sqlx.SqlConn) RuleChangeLogModel {
	return &defaultRuleChangeLogModel{conn: conn}
}

func (m *defaultRuleChangeLogModel) Insert(ctx context.Context, l *RuleChangeLog) (int64, error) {
	if l.Ctime == 0 {
		l.Ctime = nowUnix()
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO cr_rule_change_log (rule_code, source_type, action, from_state, to_state, "+
			"from_unit_price_per_1000_minor, to_unit_price_per_1000_minor, from_version, to_version, "+
			"operator, reason, request_id, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		l.RuleCode, l.SourceType, l.Action, l.FromState, l.ToState,
		l.FromUnitPrice, l.ToUnitPrice, l.FromVersion, l.ToVersion,
		l.Operator, l.Reason, l.RequestId, l.Ctime)
	if err != nil {
		return 0, fmt.Errorf("cr_rule_change_log Insert(%s,%s): %w", l.RuleCode, l.Action, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("cr_rule_change_log LastInsertId(%s): %w", l.RuleCode, err)
	}
	return id, nil
}

func (m *defaultRuleChangeLogModel) FindByRequest(
	ctx context.Context, requestID string,
) (*RuleChangeLog, error) {
	var l RuleChangeLog
	query := "SELECT " + ruleChangeLogColumns + " FROM cr_rule_change_log WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &l, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_rule_change_log FindByRequest(%s): %w", requestID, err)
	}
	return &l, nil
}

func (m *defaultRuleChangeLogModel) FirstChangeFrom(
	ctx context.Context, ruleCode string, version int64,
) (*RuleChangeLog, error) {
	var l RuleChangeLog
	query := "SELECT " + ruleChangeLogColumns + " FROM cr_rule_change_log " +
		"WHERE rule_code = ? AND from_version >= ? ORDER BY from_version ASC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &l, query, ruleCode, version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_rule_change_log FirstChangeFrom(%s,%d): %w", ruleCode, version, err)
	}
	return &l, nil
}

func (m *defaultRuleChangeLogModel) ListByRule(
	ctx context.Context, ruleCode string, limit int64,
) ([]*RuleChangeLog, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []*RuleChangeLog
	query := "SELECT " + ruleChangeLogColumns + " FROM cr_rule_change_log " +
		"WHERE rule_code = ? ORDER BY to_version DESC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, ruleCode, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_rule_change_log ListByRule(%s): %w", ruleCode, err)
	}
	return rows, nil
}
