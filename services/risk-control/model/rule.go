package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RiskRule 风控规则（risk_rule 表）。
// 可解释性要求：规则带 version，决策日志记录命中时的 rule_id + version，
// 规则被改后仍能还原「当时为什么这么判」。
type RiskRule struct {
	RuleID        int64  `db:"rule_id"`        // 规则 ID
	Name          string `db:"name"`           // 规则名（全局唯一）
	ActionType    int32  `db:"action_type"`    // 适用动作，0 表示全部动作
	Metric        string `db:"metric"`         // 指标名，见 SupportedMetrics
	Op            int32  `db:"op"`             // 比较符，见 OpGT..OpEQ
	Threshold     int64  `db:"threshold"`      // 阈值
	WindowSeconds int64  `db:"window_seconds"` // 统计窗口（秒）
	Decision      int32  `db:"decision"`       // 命中后的裁决
	Priority      int32  `db:"priority"`       // 优先级（越大越先出现在 hit_rule_ids）
	State         int32  `db:"state"`          // 0 禁用、1 启用
	Version       int32  `db:"version"`        // 规则版本，评估字段变更即 +1
	Operator      int64  `db:"operator"`       // 最近变更的运营 ID（审计必填）
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// ruleColumns 是显式列清单，避免 SELECT *。
const ruleColumns = "rule_id, name, action_type, metric, op, threshold, window_seconds, " +
	"decision, priority, state, version, operator, ctime, mtime"

// Validate 校验规则字段。指标未注册或裁决写成 ALLOW 都会被拒绝：
// 前者会永久无法观测，后者不具备任何处罚语义，都会让决策解释失真。
func (r *RiskRule) Validate() error {
	if r.Name == "" || len(r.Name) > 128 {
		return fmt.Errorf("%w: name required and <=128", ErrInvalidRule)
	}
	if !ValidRuleAction(r.ActionType) {
		return fmt.Errorf("%w: action_type=%d", ErrInvalidRule, r.ActionType)
	}
	if !ValidMetric(r.Metric) {
		return fmt.Errorf("%w: metric=%q not implemented", ErrInvalidRule, r.Metric)
	}
	if !ValidOp(r.Op) {
		return fmt.Errorf("%w: op=%d", ErrInvalidRule, r.Op)
	}
	if r.WindowSeconds <= 0 || r.WindowSeconds > 86400 {
		return fmt.Errorf("%w: window_seconds=%d must be in (0,86400]", ErrInvalidRule, r.WindowSeconds)
	}
	if r.Threshold < 0 {
		return fmt.Errorf("%w: threshold must be >= 0", ErrInvalidRule)
	}
	// 方向性校验：计数类指标只在大值时才有风险，允许 LT/LTE/EQ 供设备分/关联数使用，
	// 但阈值必须可达（LT/LTE 至少为 1），否则规则恒不命中会掩盖配置错误。
	if (r.Op == OpLT || r.Op == OpLTE) && r.Threshold <= 0 {
		return fmt.Errorf("%w: threshold must be > 0 when op is LT/LTE", ErrInvalidRule)
	}
	if r.Decision != DecisionChallenge && r.Decision != DecisionBlock && r.Decision != DecisionReview {
		return fmt.Errorf("%w: decision=%d must be CHALLENGE/BLOCK/REVIEW", ErrInvalidRule, r.Decision)
	}
	if r.State != StateDisabled && r.State != StateEnabled {
		return fmt.Errorf("%w: state=%d", ErrInvalidRule, r.State)
	}
	return nil
}

// Hit 判定给定观测值是否命中本规则。
func (r *RiskRule) Hit(observed int64) bool { return Evaluate(r.Op, observed, r.Threshold) }

// RiskRuleModel risk_rule 表读写接口。
type RiskRuleModel interface {
	// Insert 新建规则，返回自增 rule_id。
	Insert(ctx context.Context, r *RiskRule) (int64, error)
	// Update 按 rule_id 更新评估字段（含 version、operator、mtime）。
	Update(ctx context.Context, r *RiskRule) error
	// FindOne 按主键查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, ruleID int64) (*RiskRule, error)
	// FindByName 按规则名查询（唯一约束用）；不存在返回 (nil, nil)。
	FindByName(ctx context.Context, name string) (*RiskRule, error)
	// ListActiveByAction 查询对 action 生效的启用规则（action_type IN (0, action)），
	// 按 priority DESC、rule_id ASC 排序，保证 hit_rule_ids 顺序可复现。
	ListActiveByAction(ctx context.Context, action int32) ([]*RiskRule, error)
	// List 分页查询规则；action/metric 为空、state 为 -1 表示不过滤。
	List(ctx context.Context, action int32, metric string, state int32, offset, limit int) ([]*RiskRule, int32, error)
	// CountAll 返回启用规则数，用于启动自检日志。
	CountAll(ctx context.Context) (int64, error)
}

type defaultRiskRuleModel struct {
	conn sqlx.SqlConn
}

// NewRiskRuleModel 构造 RiskRuleModel 实现。
func NewRiskRuleModel(conn sqlx.SqlConn) RiskRuleModel {
	return &defaultRiskRuleModel{conn: conn}
}

func (m *defaultRiskRuleModel) Insert(ctx context.Context, r *RiskRule) (int64, error) {
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO risk_rule (name, action_type, metric, op, threshold, window_seconds, decision, priority, state, version, operator, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		r.Name, r.ActionType, r.Metric, r.Op, r.Threshold, r.WindowSeconds, r.Decision,
		r.Priority, r.State, r.Version, r.Operator, now, now)
	if err != nil {
		if isDuplicateEntry(err) {
			return 0, fmt.Errorf("%w: %s", ErrRuleNameDuplicated, r.Name)
		}
		return 0, fmt.Errorf("risk_rule Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("risk_rule Insert LastInsertId: %w", err)
	}
	r.RuleID, r.Ctime, r.Mtime = id, now, now
	return id, nil
}

func (m *defaultRiskRuleModel) Update(ctx context.Context, r *RiskRule) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE risk_rule SET action_type = ?, metric = ?, op = ?, threshold = ?, window_seconds = ?, "+
			"decision = ?, priority = ?, state = ?, version = ?, operator = ?, mtime = ? "+
			"WHERE rule_id = ?",
		r.ActionType, r.Metric, r.Op, r.Threshold, r.WindowSeconds, r.Decision, r.Priority,
		r.State, r.Version, r.Operator, nowUnix(), r.RuleID)
	if err != nil {
		return fmt.Errorf("risk_rule Update: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("risk_rule Update RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrRuleNotFound
	}
	return nil
}

func (m *defaultRiskRuleModel) FindOne(ctx context.Context, ruleID int64) (*RiskRule, error) {
	var r RiskRule
	query := "SELECT " + ruleColumns + " FROM risk_rule WHERE rule_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, ruleID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("risk_rule FindOne: %w", err)
	}
	return &r, nil
}

func (m *defaultRiskRuleModel) FindByName(ctx context.Context, name string) (*RiskRule, error) {
	var r RiskRule
	query := "SELECT " + ruleColumns + " FROM risk_rule WHERE name = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &r, query, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("risk_rule FindByName: %w", err)
	}
	return &r, nil
}

func (m *defaultRiskRuleModel) ListActiveByAction(ctx context.Context, action int32) ([]*RiskRule, error) {
	var rows []*RiskRule
	query := "SELECT " + ruleColumns + " FROM risk_rule WHERE state = ? AND action_type IN (?, ?) ORDER BY priority DESC, rule_id ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, StateEnabled, ActionAll, action); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("risk_rule ListActiveByAction: %w", err)
	}
	return rows, nil
}

func (m *defaultRiskRuleModel) List(ctx context.Context, action int32, metric string, state int32, offset, limit int) ([]*RiskRule, int32, error) {
	where := "WHERE 1=1"
	args := make([]any, 0, 3)
	if action > 0 {
		where += " AND action_type IN (?, ?)"
		args = append(args, ActionAll, action)
	}
	if metric != "" {
		where += " AND metric = ?"
		args = append(args, metric)
	}
	if state == StateDisabled || state == StateEnabled {
		where += " AND state = ?"
		args = append(args, state)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM risk_rule "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("risk_rule List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*RiskRule
	query := "SELECT " + ruleColumns + " FROM risk_rule " + where + " ORDER BY rule_id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("risk_rule List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultRiskRuleModel) CountAll(ctx context.Context) (int64, error) {
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, "SELECT COUNT(*) FROM risk_rule WHERE state = ?", StateEnabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("risk_rule CountAll: %w", err)
	}
	return n, nil
}

// isDuplicateEntry 判定 MySQL 唯一索引冲突（错误码 1062）。
// 这里不引入 go-sql-driver 的具体错误类型，按驱动错误文案匹配，
// 避免为一个分支新增直接依赖。
func isDuplicateEntry(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}
