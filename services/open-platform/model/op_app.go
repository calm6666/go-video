package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 应用状态机，与 op_app.status、rpc.AppStatus 一致。
const (
	// AppStatusPendingReview 待运营审核。
	AppStatusPendingReview int32 = 1
	// AppStatusActive 正常可用。
	AppStatusActive int32 = 2
	// AppStatusSuspended 已停用（违规/风控）：token、签名与配额全部拒绝。
	AppStatusSuspended int32 = 3
	// AppStatusRejected 审核驳回。
	AppStatusRejected int32 = 4
	// AppStatusOffline 已下线（终态）。
	AppStatusOffline int32 = 5
)

// ValidAppStatus 判断状态取值落在已定义区间。
func ValidAppStatus(v int32) bool { return v >= AppStatusPendingReview && v <= AppStatusOffline }

// CanTransitionAppStatus 定义应用状态机的合法迁移（logic 与 model 双层护栏）。
//
//	PENDING_REVIEW → ACTIVE | REJECTED | OFFLINE
//	ACTIVE         → SUSPENDED | OFFLINE
//	SUSPENDED      → ACTIVE | OFFLINE
//	REJECTED       → PENDING_REVIEW（重新提交）| OFFLINE
//	OFFLINE        → 终态，无任何出边
//
// 两条附加规则：
//  1. from/to 任一不在 ValidAppStatus 定义域内一律拒绝。否则 (0,0)、(99,99) 这类
//     未定义值会因「同态即放行」被误接受，0 是 proto3 默认值，绝不能被写进库。
//  2. 同状态重复提交（from == to）视为幂等空操作放行，但终态 OFFLINE 不放行：
//     已下线应用没有「再下线一次」的语义，重新接入必须重新注册拿到新 app_id，
//     避免复用带着旧 secret / 旧 scope / 旧 token 的历史身份。
func CanTransitionAppStatus(from, to int32) bool {
	if !ValidAppStatus(from) || !ValidAppStatus(to) {
		return false
	}
	if from == to {
		return from != AppStatusOffline
	}
	switch from {
	case AppStatusPendingReview:
		return to == AppStatusActive || to == AppStatusRejected || to == AppStatusOffline
	case AppStatusActive:
		return to == AppStatusSuspended || to == AppStatusOffline
	case AppStatusSuspended:
		return to == AppStatusActive || to == AppStatusOffline
	case AppStatusRejected:
		return to == AppStatusPendingReview || to == AppStatusOffline
	default:
		return false
	}
}

// Application 应用主体（op_app 表）。
//
// RedirectURIs 以逗号分隔的规范化 URL 存储（logic 负责校验 https 与内网地址）。
// Version 是乐观锁位点：任何资料/状态变更都要求携带读到的版本，冲突返回 ErrConcurrentUpdate，
// 满足 AGENTS.md §5「所有写接口要设计幂等键、状态版本或唯一约束」。
type Application struct {
	// AppID 应用 ID（跨服务唯一引用，主键）
	AppID int64 `db:"app_id"`
	// AppKey 公开标识，签发后不可变
	AppKey string `db:"app_key"`
	// Name 应用名（同一 owner 下唯一）
	Name string `db:"name"`
	// Description 简介
	Description string `db:"description"`
	// OwnerMid 归属开发者 mid（真值在 account/user-profile，本表只存主键）
	OwnerMid int64 `db:"owner_mid"`
	// Status 状态，见 AppStatus*
	Status int32 `db:"status"`
	// RedirectURIs 回调白名单，逗号分隔
	RedirectURIs string `db:"redirect_uris"`
	// Version 乐观锁版本
	Version int32 `db:"version"`
	// RegisterToken 注册幂等键（客户端生成，唯一索引）
	RegisterToken string `db:"register_token"`
	// LastOperator 最近一次状态变更的操作者（运营 mid）
	LastOperator int64 `db:"last_operator"`
	// StatusReason 状态变更原因（审计，脱敏）
	StatusReason string `db:"status_reason"`
	// OfflineAt 下线时间（Unix 秒，0 表示未下线）
	OfflineAt int64 `db:"offline_at"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// IsActive 判断应用是否可对外提供服务。
func (a *Application) IsActive() bool { return a != nil && a.Status == AppStatusActive }

// ApplicationModel op_app 表读写接口。
type ApplicationModel interface {
	// Insert 创建应用；命中 uniq_register_token 时返回既有 app_id 且 created=false（幂等重放）。
	Insert(ctx context.Context, app *Application) (appID int64, created bool, err error)
	// InsertTx 事务内创建应用：注册流程必须同时写 op_app 与 op_app_secret，
	// 两句话同生共死，否则留下「有应用无密钥」的半成品身份（密钥行缺失会让应用永远不可用）。
	InsertTx(ctx context.Context, session sqlx.Session, app *Application) (appID int64, created bool, err error)
	// FindByID 查询应用；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, appID int64) (*Application, error)
	// FindByAppKey 按公开标识查询（签名模式入口）。
	FindByAppKey(ctx context.Context, appKey string) (*Application, error)
	// FindByRegisterToken 按幂等键查询。
	FindByRegisterToken(ctx context.Context, token string) (*Application, error)
	// ListByOwner 开发者侧分页：cursor 为 (mtime, app_id) 倒序位点。
	// ownerMid 用 int64：mid 在本仓一律是 int64（Application.OwnerMid / Grant.Mid 同列宽），
	// 声明成 int32 会让 logic 侧不得不窄化取值，等于给自己埋一个 mid 溢出。
	ListByOwner(ctx context.Context, ownerMid int64, status int32, cursorTime, cursorID int64,
		ps int32) ([]*Application, error)
	// ListAll 运营侧全量分页（status=0 表示不过滤）。
	ListAll(ctx context.Context, status int32, cursorTime, cursorID int64, ps int32) ([]*Application, error)
	// UpdateProfile CAS 更新资料（只改 name/description/redirect_uris），版本自增。
	UpdateProfile(ctx context.Context, appID int64, name, description, redirectURIs string, expectedVersion int32) error
	// UpdateStatus CAS 推进状态机（运营），版本自增并记录操作者与原因。
	UpdateStatus(ctx context.Context, appID int64, from, to int32, operator int64, reason string, offlineAt int64) error
	// NextVersion 只做版本自增（scope 授予等外部变更需要让 token 校验侧看到新版本）。
	NextVersion(ctx context.Context, appID int64) (int32, error)
}

type defaultApplicationModel struct {
	conn sqlx.SqlConn
}

// NewApplicationModel 创建 ApplicationModel 实现。
func NewApplicationModel(conn sqlx.SqlConn) ApplicationModel {
	return &defaultApplicationModel{conn: conn}
}

const appColumns = `app_id, app_key, name, description, owner_mid, status, redirect_uris, version,
	register_token, last_operator, status_reason, offline_at, ctime, mtime`

func (m *defaultApplicationModel) Insert(ctx context.Context, app *Application) (int64, bool, error) {
	return m.insert(ctx, nil, app)
}

func (m *defaultApplicationModel) InsertTx(ctx context.Context, session sqlx.Session, app *Application) (int64, bool, error) {
	return m.insert(ctx, session, app)
}

func (m *defaultApplicationModel) insert(ctx context.Context, session sqlx.Session, app *Application) (int64, bool, error) {
	if app.RegisterToken == "" {
		return 0, false, ErrClientTokenRequired
	}
	now := nowUnix()
	if app.Ctime == 0 {
		app.Ctime = now
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"INSERT INTO op_app (app_key, name, description, owner_mid, status, redirect_uris, version, "+
			"register_token, last_operator, status_reason, offline_at, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, 1, ?, 0, '', 0, ?, ?)",
		app.AppKey, app.Name, app.Description, app.OwnerMid, app.Status, app.RedirectURIs,
		app.RegisterToken, app.Ctime, app.Ctime)
	if err != nil {
		// (owner_mid, name) 唯一键冲突要单独暴露：注册幂等不能掩盖重名。
		if isDuplicateKeyErr(err) && strings.Contains(err.Error(), "uniq_owner_name") {
			return 0, false, ErrDuplicateAppName
		}
		if old, qerr := m.FindByRegisterToken(ctx, app.RegisterToken); qerr == nil && old != nil {
			return old.AppID, false, nil
		}
		return 0, false, fmt.Errorf("op_app Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("op_app Insert LastInsertId: %w", err)
	}
	return id, true, nil
}

func (m *defaultApplicationModel) FindByID(ctx context.Context, appID int64) (*Application, error) {
	var a Application
	err := m.conn.QueryRowCtx(ctx, &a,
		"SELECT "+appColumns+" FROM op_app WHERE app_id = ? LIMIT 1", appID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_app FindByID: %w", err)
	}
	return &a, nil
}

func (m *defaultApplicationModel) FindByAppKey(ctx context.Context, appKey string) (*Application, error) {
	if appKey == "" {
		return nil, ErrAppKeyRequired
	}
	var a Application
	err := m.conn.QueryRowCtx(ctx, &a,
		"SELECT "+appColumns+" FROM op_app WHERE app_key = ? LIMIT 1", appKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_app FindByAppKey: %w", err)
	}
	return &a, nil
}

func (m *defaultApplicationModel) FindByRegisterToken(ctx context.Context, token string) (*Application, error) {
	if token == "" {
		return nil, ErrClientTokenRequired
	}
	var a Application
	err := m.conn.QueryRowCtx(ctx, &a,
		"SELECT "+appColumns+" FROM op_app WHERE register_token = ? LIMIT 1", token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_app FindByRegisterToken: %w", err)
	}
	return &a, nil
}

func (m *defaultApplicationModel) ListByOwner(ctx context.Context, ownerMid int64, status int32,
	cursorTime, cursorID int64, ps int32) ([]*Application, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	conds := []string{"owner_mid = ?"}
	args := []any{ownerMid}
	if status > 0 {
		conds = append(conds, "status = ?")
		args = append(args, status)
	}
	return m.list(ctx, conds, args, cursorTime, cursorID, ps)
}

func (m *defaultApplicationModel) ListAll(ctx context.Context, status int32, cursorTime, cursorID int64, ps int32) ([]*Application, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	conds := []string{"1 = 1"}
	var args []any
	if status > 0 {
		conds = append(conds, "status = ?")
		args = append(args, status)
	}
	return m.list(ctx, conds, args, cursorTime, cursorID, ps)
}

func (m *defaultApplicationModel) list(ctx context.Context, conds []string, args []any,
	cursorTime, cursorID int64, ps int32) ([]*Application, error) {
	if cursorTime > 0 {
		conds = append(conds, "(mtime < ? OR (mtime = ? AND app_id < ?))")
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	query := "SELECT " + appColumns + " FROM op_app WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY mtime DESC, app_id DESC LIMIT ?"
	args = append(args, ps)

	var rows []*Application
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_app list: %w", err)
	}
	return rows, nil
}

func (m *defaultApplicationModel) UpdateProfile(ctx context.Context, appID int64, name, description,
	redirectURIs string, expectedVersion int32) error {
	sets := []string{"version = version + 1", "mtime = ?"}
	args := []any{nowUnix()}
	if name != "" {
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if description != "" {
		sets = append(sets, "description = ?")
		args = append(args, description)
	}
	if redirectURIs != "" {
		sets = append(sets, "redirect_uris = ?")
		args = append(args, redirectURIs)
	}
	if len(sets) == 2 {
		return nil // 没有任何变更，避免无谓的版本自增
	}
	args = append(args, appID, expectedVersion)
	query := "UPDATE op_app SET " + strings.Join(sets, ", ") + " WHERE app_id = ? AND version = ?"
	return m.casUpdate(ctx, query, args, "op_app UpdateProfile")
}

func (m *defaultApplicationModel) UpdateStatus(ctx context.Context, appID int64, from, to int32,
	operator int64, reason string, offlineAt int64) error {
	if !CanTransitionAppStatus(from, to) {
		return ErrInvalidStateTransition
	}
	if from == to {
		return nil
	}
	if offlineAt == 0 && to == AppStatusOffline {
		offlineAt = nowUnix()
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_app SET status = ?, version = version + 1, last_operator = ?, status_reason = ?, "+
			"offline_at = ?, mtime = ? WHERE app_id = ? AND status = ?",
		to, operator, reason, offlineAt, nowUnix(), appID, from)
	if err != nil {
		return fmt.Errorf("op_app UpdateStatus: %w", err)
	}
	return checkAffected(res, "op_app UpdateStatus")
}

func (m *defaultApplicationModel) NextVersion(ctx context.Context, appID int64) (int32, error) {
	if _, err := m.conn.ExecCtx(ctx, "UPDATE op_app SET version = version + 1, mtime = ? WHERE app_id = ?",
		nowUnix(), appID); err != nil {
		return 0, fmt.Errorf("op_app NextVersion: %w", err)
	}
	app, err := m.FindByID(ctx, appID)
	if err != nil {
		return 0, err
	}
	if app == nil {
		return 0, ErrAppNotFound
	}
	return app.Version, nil
}

func (m *defaultApplicationModel) casUpdate(ctx context.Context, query string, args []any, op string) error {
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return checkAffected(res, op)
}

// checkAffected CAS 更新 0 行即视为版本冲突（乐观锁失败）。
func checkAffected(res sql.Result, op string) error {
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	if aff == 0 {
		return ErrConcurrentUpdate
	}
	return nil
}

// isDuplicateKeyErr 判断是否唯一键冲突（MySQL 1062）。
// go-zero 会把驱动错误包进 fmt 链，这里只按错误码文本识别，避免引入 mysql 驱动依赖。
func isDuplicateKeyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}
