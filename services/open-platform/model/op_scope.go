package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// scope 读写属性，与 op_scope.access、rpc.ScopeAccess 一致。
const (
	// ScopeAccessRead 只读权限。
	ScopeAccessRead int32 = 1
	// ScopeAccessWrite 写权限（必须要求用户显式同意）。
	ScopeAccessWrite int32 = 2
)

// scope 风险级别，与 op_scope.risk_level、rpc.ScopeRiskLevel 一致。
const (
	// ScopeRiskLow 低：可默认勾选。
	ScopeRiskLow int32 = 1
	// ScopeRiskMedium 中：需用户显式确认。
	ScopeRiskMedium int32 = 2
	// ScopeRiskHigh 高：需运营审批，不可批量授予。
	ScopeRiskHigh int32 = 3
)

// 商业化禁用词根（AGENTS.md §1）。scope 与 api_code 命中任一即拒绝注册/授予，
// 避免“先建表位、后面再放开”导致范围漂移。
var forbiddenScopeTokens = []string{
	"member", "vip", "order", "pay", "payment", "charge", "coin",
	"divide", "revenue", "ads", "ad_", "advert", "sponsor",
}

// IsForbiddenScopeCategory 判断权限点/接口标识是否落在未开放类目。
func IsForbiddenScopeCategory(v string) bool {
	v = strings.ToLower(v)
	for _, bad := range forbiddenScopeTokens {
		if strings.Contains(v, bad) {
			return true
		}
	}
	return false
}

// Scope 权限点目录行（op_scope 表）。
//
// 目录内容由迁移 seed 维护（deploy/migrations/open-platform/000001_*.sql），
// 本期不开放管理接口：新增能力必须先经范围评审再新增 seed 迁移，
// 保证「只开放明确的非商业化能力」有可审计的变更记录。
type Scope struct {
	// Scope 权限点标识，如 video.publish
	Scope string `db:"scope"`
	// DisplayName 授权页展示名
	DisplayName string `db:"display_name"`
	// Description 授权页说明（用户看得懂的用途描述）
	Description string `db:"description"`
	// Access 读/写声明
	Access int32 `db:"access"`
	// RiskLevel 风险级别
	RiskLevel int32 `db:"risk_level"`
	// RequiresUserConsent 是否需要用户逐次同意（写 scope 强制为 1）
	RequiresUserConsent int8 `db:"requires_user_consent"`
	// Enabled 是否对外开放
	Enabled int8 `db:"enabled"`
	// DisableReason 停用原因
	DisableReason string `db:"disable_reason"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// IsWrite 判断是否写权限。
func (s *Scope) IsWrite() bool { return s != nil && s.Access == ScopeAccessWrite }

// ScopeModel op_scope 表读接口。
type ScopeModel interface {
	// List 返回目录全部或部分 scope。
	List(ctx context.Context, onlyEnabled bool) ([]*Scope, error)
	// FindByScope 查询单个 scope；不存在返回 (nil, nil)。
	FindByScope(ctx context.Context, scope string) (*Scope, error)
	// FindByScopes 批量查询，返回 scope -> 定义（缺失的 key 不在 map 中，供“未知 scope”判定）。
	FindByScopes(ctx context.Context, scopes []string) (map[string]*Scope, error)
	// Upsert 写入/更新目录项（仅迁移脚本与运营应急使用；调用方必须过 IsForbiddenScopeCategory 检查）。
	Upsert(ctx context.Context, s *Scope) error
}

type defaultScopeModel struct {
	conn sqlx.SqlConn
}

// NewScopeModel 创建 ScopeModel 实现。
func NewScopeModel(conn sqlx.SqlConn) ScopeModel {
	return &defaultScopeModel{conn: conn}
}

const scopeColumns = `scope, display_name, description, access, risk_level, requires_user_consent, enabled,
	disable_reason, ctime, mtime`

func (m *defaultScopeModel) List(ctx context.Context, onlyEnabled bool) ([]*Scope, error) {
	conds := []string{"1 = 1"}
	if onlyEnabled {
		conds = append(conds, "enabled = 1")
	}
	var rows []*Scope
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+scopeColumns+" FROM op_scope WHERE "+strings.Join(conds, " AND ")+" ORDER BY scope ASC")
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_scope List: %w", err)
	}
	return rows, nil
}

func (m *defaultScopeModel) FindByScope(ctx context.Context, scope string) (*Scope, error) {
	if scope == "" {
		return nil, ErrScopeUnknown
	}
	var s Scope
	err := m.conn.QueryRowCtx(ctx, &s,
		"SELECT "+scopeColumns+" FROM op_scope WHERE scope = ? LIMIT 1", scope)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_scope FindByScope: %w", err)
	}
	return &s, nil
}

func (m *defaultScopeModel) FindByScopes(ctx context.Context, scopes []string) (map[string]*Scope, error) {
	out := make(map[string]*Scope, len(scopes))
	if len(scopes) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(scopes))
	for _, s := range scopes {
		args = append(args, s)
	}
	query := "SELECT " + scopeColumns + " FROM op_scope WHERE scope IN (?" + strings.Repeat(",?", len(scopes)-1) + ")"
	var rows []*Scope
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("op_scope FindByScopes: %w", err)
	}
	for _, r := range rows {
		out[r.Scope] = r
	}
	return out, nil
}

func (m *defaultScopeModel) Upsert(ctx context.Context, s *Scope) error {
	if s == nil || s.Scope == "" {
		return ErrScopeUnknown
	}
	if IsForbiddenScopeCategory(s.Scope) {
		return ErrForbiddenScopeCategory
	}
	if s.Access != ScopeAccessRead && s.Access != ScopeAccessWrite {
		return ErrScopeUnknown
	}
	now := nowUnix()
	consent := s.RequiresUserConsent
	if s.Access == ScopeAccessWrite {
		// 写权限强制要求用户同意，不给调用方留关闭口子。
		consent = 1
	}
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_scope (scope, display_name, description, access, risk_level, requires_user_consent, "+
			"enabled, disable_reason, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE display_name = VALUES(display_name), description = VALUES(description), "+
			"access = VALUES(access), risk_level = VALUES(risk_level), requires_user_consent = VALUES(requires_user_consent), "+
			"enabled = VALUES(enabled), disable_reason = VALUES(disable_reason), mtime = VALUES(mtime)",
		s.Scope, s.DisplayName, s.Description, s.Access, s.RiskLevel, consent, s.Enabled, s.DisableReason, now, now)
	if err != nil {
		return fmt.Errorf("op_scope Upsert: %w", err)
	}
	return nil
}
