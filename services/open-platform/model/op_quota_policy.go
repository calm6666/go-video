package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 配额通配常量。
const (
	// GlobalAppID app_id=0 表示全局默认规则。
	GlobalAppID int64 = 0
	// AnyAPICode api_code="*" 表示该应用（或全局）的全部接口。
	AnyAPICode = "*"
)

// QuotaPolicy 配额规则行（op_quota_policy 表）：应用 × 接口 × 时间窗。
//
// limit 用 quota_limit 列名：LIMIT 是 MySQL 保留字，禁止用裸 limit 建列。
// 同一 (app_id, api_code, window_seconds) 只有一行（唯一键），upsert 即改限额。
//
// 多窗口并存是有意设计：例如同时配 (60s → 100 次) 与 (86400s → 50000 次)，
// AuthorizeRequest 会对「生效层级」内的每一条规则都做一次窗口扣减，任一超限即拒
// （见 NarrowPolicies 与 op_quota_usage 的窗口维度计数）。
type QuotaPolicy struct {
	// PolicyID 规则 ID（主键）
	PolicyID int64 `db:"policy_id"`
	// AppID 应用 ID，0 表示全局默认
	AppID int64 `db:"app_id"`
	// APICode 接口标识，"*" 表示全部接口
	APICode string `db:"api_code"`
	// WindowSeconds 时间窗长度（秒，必须 > 0）
	WindowSeconds int64 `db:"window_seconds"`
	// QuotaLimit 窗口内允许次数；0 或负数表示禁用该接口
	QuotaLimit int64 `db:"quota_limit"`
	// Enabled 是否生效
	Enabled int8 `db:"enabled"`
	// Operator 最近一次修改的运营 mid
	Operator int64 `db:"operator_mid"`
	// Reason 变更原因（审计）
	Reason string `db:"reason"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// Denied 判断该规则是否等价于“禁止调用”。
func (p *QuotaPolicy) Denied() bool { return p != nil && p.Enabled == 1 && p.QuotaLimit <= 0 }

// QuotaPolicyModel op_quota_policy 表读写接口。
type QuotaPolicyModel interface {
	// Upsert 按唯一键 (app_id, api_code, window_seconds) 写入或更新规则。
	// created=false 表示命中既有行并更新。
	Upsert(ctx context.Context, p *QuotaPolicy) (policyID int64, created bool, err error)
	// FindByID 主键查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, policyID int64) (*QuotaPolicy, error)
	// ListCandidates 返回 (app_id ∈ {app, 0}) × (api_code ∈ {code, "*"}) 四条象限中
	// 所有生效规则，交由 NarrowPolicies 选出生效层级（一次查询取回，避免 N+1）。
	ListCandidates(ctx context.Context, appID int64, apiCode string) ([]*QuotaPolicy, error)
	// ListByApp 运营侧分页：appID=0 时返回全局规则；cursor 为 (mtime, policy_id) 倒序。
	ListByApp(ctx context.Context, appID int64, apiCode string, cursorTime, cursorID int64,
		ps int32) ([]*QuotaPolicy, error)
	// Disable 停用规则（保留行以便审计与重算解释）。
	Disable(ctx context.Context, policyID, operator int64, reason string) (bool, error)
}

type defaultQuotaPolicyModel struct {
	conn sqlx.SqlConn
}

// NewQuotaPolicyModel 创建 QuotaPolicyModel 实现。
func NewQuotaPolicyModel(conn sqlx.SqlConn) QuotaPolicyModel {
	return &defaultQuotaPolicyModel{conn: conn}
}

const quotaPolicyColumns = `policy_id, app_id, api_code, window_seconds, quota_limit, enabled,
	operator_mid, reason, ctime, mtime`

func (m *defaultQuotaPolicyModel) Upsert(ctx context.Context, p *QuotaPolicy) (int64, bool, error) {
	if p.AppID < 0 {
		return 0, false, ErrInvalidAppID
	}
	if strings.TrimSpace(p.APICode) == "" {
		return 0, false, ErrQuotaPolicyNotFound
	}
	if p.WindowSeconds <= 0 {
		return 0, false, ErrWindowInvalid
	}
	if p.Operator <= 0 {
		return 0, false, ErrOperatorRequired
	}
	// 配额规则同样受商业化红线约束：api_code 命中未开放类目直接拒绝落库。
	if IsForbiddenScopeCategory(p.APICode) {
		return 0, false, ErrForbiddenScopeCategory
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_quota_policy (app_id, api_code, window_seconds, quota_limit, enabled, operator_mid, "+
			"reason, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE quota_limit = VALUES(quota_limit), enabled = VALUES(enabled), "+
			"operator_mid = VALUES(operator_mid), reason = VALUES(reason), mtime = VALUES(mtime)",
		p.AppID, p.APICode, p.WindowSeconds, p.QuotaLimit, p.Enabled, p.Operator, p.Reason, now, now)
	if err != nil {
		return 0, false, fmt.Errorf("op_quota_policy Upsert: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("op_quota_policy Upsert RowsAffected: %w", err)
	}
	created := affected == 1
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("op_quota_policy Upsert LastInsertId: %w", err)
	}
	if created && id > 0 {
		return id, true, nil
	}
	// 命中唯一键时 LastInsertId 不可靠（MySQL 只在插入时消耗自增值），回查取真实 ID。
	row, err := m.find(ctx, p.AppID, p.APICode, p.WindowSeconds)
	if err != nil {
		return 0, false, err
	}
	if row == nil {
		return 0, false, ErrQuotaPolicyNotFound
	}
	return row.PolicyID, false, nil
}

func (m *defaultQuotaPolicyModel) find(ctx context.Context, appID int64, apiCode string,
	windowSeconds int64) (*QuotaPolicy, error) {
	var p QuotaPolicy
	err := m.conn.QueryRowCtx(ctx, &p,
		"SELECT "+quotaPolicyColumns+" FROM op_quota_policy WHERE app_id = ? AND api_code = ? AND "+
			"window_seconds = ? LIMIT 1", appID, apiCode, windowSeconds)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_quota_policy find: %w", err)
	}
	return &p, nil
}

func (m *defaultQuotaPolicyModel) FindByID(ctx context.Context, policyID int64) (*QuotaPolicy, error) {
	var p QuotaPolicy
	err := m.conn.QueryRowCtx(ctx, &p,
		"SELECT "+quotaPolicyColumns+" FROM op_quota_policy WHERE policy_id = ? LIMIT 1", policyID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_quota_policy FindByID: %w", err)
	}
	return &p, nil
}

func (m *defaultQuotaPolicyModel) ListCandidates(ctx context.Context, appID int64, apiCode string) ([]*QuotaPolicy, error) {
	if apiCode == "" {
		return nil, ErrQuotaPolicyNotFound
	}
	var rows []*QuotaPolicy
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+quotaPolicyColumns+" FROM op_quota_policy WHERE enabled = 1 AND app_id IN (?, ?) "+
			"AND api_code IN (?, ?) ORDER BY app_id DESC, api_code DESC, window_seconds ASC",
		appID, GlobalAppID, apiCode, AnyAPICode)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_quota_policy ListCandidates: %w", err)
	}
	return rows, nil
}

func (m *defaultQuotaPolicyModel) ListByApp(ctx context.Context, appID int64, apiCode string,
	cursorTime, cursorID int64, ps int32) ([]*QuotaPolicy, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	conds := []string{"app_id = ?"}
	args := []any{appID}
	if apiCode != "" {
		conds = append(conds, "api_code = ?")
		args = append(args, apiCode)
	}
	if cursorTime > 0 {
		conds = append(conds, "(mtime < ? OR (mtime = ? AND policy_id < ?))")
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	query := "SELECT " + quotaPolicyColumns + " FROM op_quota_policy WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY mtime DESC, policy_id DESC LIMIT ?"
	args = append(args, ps)

	var rows []*QuotaPolicy
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_quota_policy ListByApp: %w", err)
	}
	return rows, nil
}

func (m *defaultQuotaPolicyModel) Disable(ctx context.Context, policyID, operator int64,
	reason string) (bool, error) {
	if policyID <= 0 {
		return false, ErrQuotaPolicyNotFound
	}
	if operator <= 0 {
		return false, ErrOperatorRequired
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_quota_policy SET enabled = 0, operator_mid = ?, reason = ?, mtime = ? "+
			"WHERE policy_id = ? AND enabled = 1",
		operator, reason, nowUnix(), policyID)
	if err != nil {
		return false, fmt.Errorf("op_quota_policy Disable: %w", err)
	}
	return rowsPositive(res, "op_quota_policy Disable")
}

// NarrowPolicies 从候选规则集中挑出生效层级（tier），纯函数便于单测。
//
// 层级从高到低：
//  1. 本应用 + 精确 api_code
//  2. 本应用 + "*"
//  3. 全局默认 + 精确 api_code
//  4. 全局默认 + "*"
//
// 取第一个非空层级，并保留该层级的全部窗口（多窗同时限流）。
// 不做“跨层级相加”，否则运营给单个应用放开的限额会被全局默认再次收紧，行为不可解释。
func NarrowPolicies(cands []*QuotaPolicy, appID int64, apiCode string) []*QuotaPolicy {
	tiers := make([][]*QuotaPolicy, 4)
	for _, p := range cands {
		if p == nil {
			continue
		}
		appMatch := p.AppID == appID && appID > GlobalAppID
		globalMatch := p.AppID == GlobalAppID
		exactAPI := p.APICode == apiCode
		wildAPI := p.APICode == AnyAPICode
		switch {
		case appMatch && exactAPI:
			tiers[0] = append(tiers[0], p)
		case appMatch && wildAPI:
			tiers[1] = append(tiers[1], p)
		case globalMatch && exactAPI:
			tiers[2] = append(tiers[2], p)
		case globalMatch && wildAPI:
			tiers[3] = append(tiers[3], p)
		}
	}
	for _, t := range tiers {
		if len(t) > 0 {
			return t
		}
	}
	return nil
}
