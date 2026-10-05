package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 应用 scope 审批状态，与 op_app_scope.state 一致。
const (
	// AppScopePending 已申请，待运营审批。
	AppScopePending int32 = 1
	// AppScopeGranted 已获批。
	AppScopeGranted int32 = 2
	// AppScopeRevoked 已回收（保留行以便审计）。
	AppScopeRevoked int32 = 3
)

// AppScope 应用与权限点的审批关系（op_app_scope 表）。
// 唯一键 (app_id, scope)：授予与回收都是同一行的状态迁移，历史原因留在本行，
// 详细问责流水由 op_app 的 last_operator/status_reason 与（后续批次的）audit 服务承担。
type AppScope struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// AppID 应用 ID
	AppID int64 `db:"app_id"`
	// Scope 权限点标识
	Scope string `db:"scope"`
	// State 审批状态
	State int32 `db:"state"`
	// RequestedBy 申请人（开发者 mid）
	RequestedBy int64 `db:"requested_by"`
	// Operator 审批人（运营 mid，0 表示未审批）
	Operator int64 `db:"operator"`
	// Reason 审批/回收原因（审计，脱敏）
	Reason string `db:"reason"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// AppScopeModel op_app_scope 表读写接口。
type AppScopeModel interface {
	// Request 登记开发者申请的 scope（已存在则按幂等处理，不覆盖审批结论）。
	Request(ctx context.Context, appID int64, scopes []string, requestedBy int64) error
	// Grant 事务内把 scope 置为获批（行不存在时补建）。
	Grant(ctx context.Context, session sqlx.Session, appID int64, scopes []string, operator int64, reason string) error
	// Revoke 事务内把 scope 置为回收（保留行）。
	Revoke(ctx context.Context, session sqlx.Session, appID int64, scopes []string, operator int64, reason string) error
	// ListByApp 返回 app_id -> scope -> 关系行（含待审批与已回收）。
	ListByApp(ctx context.Context, appID int64) (map[string]*AppScope, error)
	// ListGranted 返回该应用已获批的 scope 列表。
	ListGranted(ctx context.Context, appID int64) ([]string, error)
	// FindGrantedScopes 从给定候选集中挑出已获批的 scope。
	FindGrantedScopes(ctx context.Context, appID int64, candidates []string) ([]string, error)
	// GrantedByApps 批量返回多个应用已获批的 scope（列表投影用，避免逐应用 ListGranted 的 N+1）。
	GrantedByApps(ctx context.Context, appIDs []int64) (map[int64][]string, error)
}

type defaultAppScopeModel struct {
	conn sqlx.SqlConn
}

// NewAppScopeModel 创建 AppScopeModel 实现。
func NewAppScopeModel(conn sqlx.SqlConn) AppScopeModel {
	return &defaultAppScopeModel{conn: conn}
}

const appScopeColumns = `id, app_id, scope, state, requested_by, operator, reason, ctime, mtime`

func (m *defaultAppScopeModel) Request(ctx context.Context, appID int64, scopes []string, requestedBy int64) error {
	if appID <= 0 {
		return ErrInvalidAppID
	}
	if len(scopes) == 0 {
		return nil
	}
	const stmt = "INSERT INTO op_app_scope (app_id, scope, state, requested_by, operator, reason, ctime, mtime) " +
		"VALUES (?, ?, ?, ?, 0, '', ?, ?) ON DUPLICATE KEY UPDATE requested_by = VALUES(requested_by), mtime = VALUES(mtime)"
	now := nowUnix()
	for _, s := range scopes {
		if _, err := m.conn.ExecCtx(ctx, stmt, appID, s, AppScopePending, requestedBy, now, now); err != nil {
			return fmt.Errorf("op_app_scope Request: %w", err)
		}
	}
	return nil
}

func (m *defaultAppScopeModel) Grant(ctx context.Context, session sqlx.Session, appID int64, scopes []string,
	operator int64, reason string) error {
	return m.transition(ctx, session, appID, scopes, operator, reason, AppScopeGranted)
}

func (m *defaultAppScopeModel) Revoke(ctx context.Context, session sqlx.Session, appID int64, scopes []string,
	operator int64, reason string) error {
	return m.transition(ctx, session, appID, scopes, operator, reason, AppScopeRevoked)
}

func (m *defaultAppScopeModel) transition(ctx context.Context, session sqlx.Session, appID int64, scopes []string,
	operator int64, reason string, state int32) error {
	if appID <= 0 {
		return ErrInvalidAppID
	}
	if len(scopes) == 0 {
		return nil
	}
	// 审批与回收都落到唯一键 (app_id, scope) 的同一行，重复调用幂等。
	const stmt = "INSERT INTO op_app_scope (app_id, scope, state, requested_by, operator, reason, ctime, mtime) " +
		"VALUES (?, ?, ?, 0, ?, ?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE state = VALUES(state), operator = VALUES(operator), " +
		"reason = VALUES(reason), mtime = VALUES(mtime)"
	db := pick(session, m.conn)
	now := nowUnix()
	for _, s := range scopes {
		if _, err := db.ExecCtx(ctx, stmt, appID, s, state, operator, reason, now, now); err != nil {
			return fmt.Errorf("op_app_scope transition: %w", err)
		}
	}
	return nil
}

func (m *defaultAppScopeModel) ListByApp(ctx context.Context, appID int64) (map[string]*AppScope, error) {
	out := make(map[string]*AppScope)
	var rows []*AppScope
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+appScopeColumns+" FROM op_app_scope WHERE app_id = ? ORDER BY scope ASC", appID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("op_app_scope ListByApp: %w", err)
	}
	for _, r := range rows {
		out[r.Scope] = r
	}
	return out, nil
}

func (m *defaultAppScopeModel) ListGranted(ctx context.Context, appID int64) ([]string, error) {
	var rows []string
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT scope FROM op_app_scope WHERE app_id = ? AND state = ? ORDER BY scope ASC", appID, AppScopeGranted)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_app_scope ListGranted: %w", err)
	}
	return rows, nil
}

func (m *defaultAppScopeModel) FindGrantedScopes(ctx context.Context, appID int64, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(candidates)+2)
	args = append(args, appID, AppScopeGranted)
	for _, c := range candidates {
		args = append(args, c)
	}
	query := "SELECT scope FROM op_app_scope WHERE app_id = ? AND state = ? AND scope IN (?" +
		strings.Repeat(",?", len(candidates)-1) + ") ORDER BY scope ASC"
	var rows []string
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_app_scope FindGrantedScopes: %w", err)
	}
	return rows, nil
}

// pick 选择执行载体：session 非空走事务，否则走连接。
// scope 授予要同时改 op_app_scope、op_grant 与 op_app.version，因此这些方法都接受 session。
func pick(session sqlx.Session, conn sqlx.SqlConn) sqlx.Session {
	if session != nil {
		return session
	}
	return conn
}

// appScopePair 批量读取的 (app_id, scope) 行。
type appScopePair struct {
	AppID int64  `db:"app_id"`
	Scope string `db:"scope"`
}

func (m *defaultAppScopeModel) GrantedByApps(ctx context.Context, appIDs []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(appIDs)+1)
	args = append(args, AppScopeGranted)
	for _, id := range appIDs {
		args = append(args, id)
	}
	query := "SELECT app_id, scope FROM op_app_scope WHERE state = ? AND app_id IN (?" +
		strings.Repeat(",?", len(appIDs)-1) + ") ORDER BY app_id ASC, scope ASC"
	var rows []*appScopePair
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("op_app_scope GrantedByApps: %w", err)
	}
	for _, r := range rows {
		out[r.AppID] = append(out[r.AppID], r.Scope)
	}
	return out, nil
}
