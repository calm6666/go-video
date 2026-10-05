package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// token 状态，与 op_token.state、rpc.TokenState 一致。
const (
	// TokenStateActive 有效（仍需比对 grant 撤销位点与应用状态）。
	TokenStateActive int32 = 1
	// TokenStateRotated 已被轮换替代（旧 access/refresh）。
	TokenStateRotated int32 = 2
	// TokenStateRevoked 已撤销。
	TokenStateRevoked int32 = 3
	// TokenStateExpired 已过期（惰性归档，仅观测用，判定一律以时间戳为准）。
	TokenStateExpired int32 = 4
)

// 授权换取方式，与 op_token.grant_type、rpc.GrantType 一致。
const (
	// GrantTypeAuthorizationCode 授权码换 token。
	GrantTypeAuthorizationCode int32 = 1
	// GrantTypeRefreshToken refresh 轮换出的新 token。
	GrantTypeRefreshToken int32 = 2
)

// Token 令牌行（op_token 表）：一行 = 一次签发的 access+refresh 组合。
//
// 只存哈希：access/refresh 各自 salt+hash（HMAC-SHA256(pepper, salt||token)），
// 与 client_secret 同口径。明文只在签发响应中出现一次，禁止入库、日志与事件。
//
// 轮换链：RefreshAccessToken 先把旧行 MarkRotated（CAS state=ACTIVE→ROTATED），
// 再插入新行并把 grant.current_token_id 前移过去，parent_token_id 记录来源。
// 若旧 refresh 值再次出现（FindActiveByRefreshHash 查不到、但哈希命中且 state=ROTATED），
// 判定为重放：logic 必须撤销整条 grant（保守失效）并返回 ErrRefreshReused。
type Token struct {
	// TokenID token 行 ID（主键，Introspect 可按此查询）
	TokenID int64 `db:"token_id"`
	// GrantID 所属授权关系（撤销位点锚点）
	GrantID int64 `db:"grant_id"`
	// AppID 应用 ID（冗余存储，避免每次校验回查 grant）
	AppID int64 `db:"app_id"`
	// Mid 授权用户 mid（冗余存储；0 预留给未来的应用级凭证，本期不签发）
	Mid int64 `db:"mid"`
	// GrantType 签发方式，见 GrantType*
	GrantType int32 `db:"grant_type"`
	// AccessSalt access token 盐（hex）
	AccessSalt string `db:"access_salt"`
	// AccessHash access token 哈希（hex），唯一索引，校验入口
	AccessHash string `db:"access_hash"`
	// RefreshSalt refresh token 盐（hex）
	RefreshSalt string `db:"refresh_salt"`
	// RefreshHash refresh token 哈希（hex），唯一索引，轮换入口
	RefreshHash string `db:"refresh_hash"`
	// Scope 实际授予 scope 快照（可能小于申请值；刷新只允许收窄）
	Scope string `db:"scope"`
	// AccessExpiresAt access 过期时间（Unix 秒）
	AccessExpiresAt int64 `db:"access_expires_at"`
	// RefreshExpiresAt refresh 过期时间（Unix 秒）
	RefreshExpiresAt int64 `db:"refresh_expires_at"`
	// State 状态，见 TokenState*
	State int32 `db:"state"`
	// ParentTokenID 轮换来源行（0 表示由授权码首发）
	ParentTokenID int64 `db:"parent_token_id"`
	// RotatedAt 被轮换时间（Unix 秒，0 表示未轮换）
	RotatedAt int64 `db:"rotated_at"`
	// RevokedAt 被撤销时间（Unix 秒，0 表示未撤销）
	RevokedAt int64 `db:"revoked_at"`
	// RevokeReason 撤销/轮换原因（审计，脱敏）
	RevokeReason string `db:"revoke_reason"`
	// LastUsedAt 最近一次校验通过时间（限频写，见 AppSecretModel.TouchUsed）
	LastUsedAt int64 `db:"last_used_at"`
	// Ctime 签发时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// AccessUsable 判断 access 在该时刻是否可用（状态 + 时间；grant 位点与应用状态由 logic 叠加）。
func (t *Token) AccessUsable(now int64) bool {
	if t == nil || t.State != TokenStateActive {
		return false
	}
	return t.AccessExpiresAt > now
}

// RefreshUsable 判断 refresh 能否用于轮换。
func (t *Token) RefreshUsable(now int64) bool {
	if t == nil {
		return false
	}
	return t.State == TokenStateActive && t.RefreshExpiresAt > now
}

// ScopeList 返回 scope 快照列表。
func (t *Token) ScopeList() []string { return SplitScopes(t.Scope) }

// TokenModel op_token 表读写接口。
type TokenModel interface {
	// Insert 签发一行 token；哈希为空直接拒绝，禁止把明文透传到本层。
	Insert(ctx context.Context, t *Token) (int64, error)
	// InsertTx 事务内签发：换码与轮换都要让「新行落库」和「旧行作废/授权码消费」原子生效，
	// 否则会留下「旧的已作废、新的没签发」的用户不可用状态。
	InsertTx(ctx context.Context, session sqlx.Session, t *Token) (int64, error)
	// FindByID 主键查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, tokenID int64) (*Token, error)
	// FindByAccessHash 按 access 哈希查询（校验入口）。
	FindByAccessHash(ctx context.Context, hash string) (*Token, error)
	// FindByRefreshHash 按 refresh 哈希查询（轮换入口；命中 ROTATED 即重放线索）。
	FindByRefreshHash(ctx context.Context, hash string) (*Token, error)
	// FindAnyByHash access 或 refresh 任一命中即返回（RevokeAuthorization 传 token_hint 时用）。
	FindAnyByHash(ctx context.Context, hash string) (*Token, error)
	// MarkRotated CAS 置为已轮换（仅 state=ACTIVE 时成功）。
	MarkRotated(ctx context.Context, session sqlx.Session, tokenID, now int64, reason string) (bool, error)
	// MarkRevoked 撤销单行 token（state 为 ACTIVE/ROTATED 时成功）。
	MarkRevoked(ctx context.Context, session sqlx.Session, tokenID, now int64, reason string) (bool, error)
	// RevokeByGrant 撤销整条 grant 下未过期 token（撤销授权主路径）。返回受影响行数。
	RevokeByGrant(ctx context.Context, session sqlx.Session, grantID, now int64, reason string) (int64, error)
	// RevokeByMid 撤销用户全部第三方 token（改密/风控一键下线）。返回受影响行数。
	RevokeByMid(ctx context.Context, session sqlx.Session, mid, now int64, reason string) (int64, error)
	// RevokeByApp 停用应用时撤销其全部 token（应用下线/违规处置）。返回受影响行数。
	RevokeByApp(ctx context.Context, session sqlx.Session, appID, now int64, reason string) (int64, error)
	// RevokeByAppWithScopes 回收 scope 时定点撤销：只作废 scope 快照命中被回收集合的 token。
	// 集合化 UPDATE 而不是让 logic 分页遍历 grant 列表——遍历会产生未定义规模的扫描，
	// 也会让「已回收但仍可用」的时间窗随分页深度变长（安全属性必须与数据规模无关）。
	// scopes 必须是 model.JoinScopes 规范化后的存储口径（升序、逗号分隔、无空格）。
	RevokeByAppWithScopes(ctx context.Context, session sqlx.Session, appID int64, scopes []string,
		now int64, reason string) (int64, error)
	// TouchUsed 记录校验通过时间（限频写，避免每请求写库）。
	TouchUsed(ctx context.Context, tokenID, ts int64) error
	// ListByGrant 返回该授权关系的签发历史（倒序，观测轮换链）。
	ListByGrant(ctx context.Context, grantID int64, ps int32) ([]*Token, error)
	// ArchiveExpiredBefore 把已过期的 ACTIVE 行惰性归档为 EXPIRED（cron 用），返回行数。
	ArchiveExpiredBefore(ctx context.Context, now int64, limit int32) (int64, error)
	// PurgeByIDs 物理删除到期的令牌哈希（保留期由配置决定，README 记录口径）。
	PurgeByIDs(ctx context.Context, ids []int64) (int64, error)
}

type defaultTokenModel struct {
	conn sqlx.SqlConn
}

// NewTokenModel 创建 TokenModel 实现。
func NewTokenModel(conn sqlx.SqlConn) TokenModel {
	return &defaultTokenModel{conn: conn}
}

const tokenColumns = `token_id, grant_id, app_id, mid, grant_type, access_salt, access_hash,
	refresh_salt, refresh_hash, scope, access_expires_at, refresh_expires_at, state, parent_token_id,
	rotated_at, revoked_at, revoke_reason, last_used_at, ctime, mtime`

func (m *defaultTokenModel) Insert(ctx context.Context, t *Token) (int64, error) {
	return m.insert(ctx, nil, t)
}

func (m *defaultTokenModel) InsertTx(ctx context.Context, session sqlx.Session, t *Token) (int64, error) {
	return m.insert(ctx, session, t)
}

func (m *defaultTokenModel) insert(ctx context.Context, session sqlx.Session, t *Token) (int64, error) {
	if t.GrantID <= 0 || t.AppID <= 0 {
		return 0, ErrInvalidAppID
	}
	if t.AccessHash == "" || t.AccessSalt == "" || t.RefreshHash == "" || t.RefreshSalt == "" {
		return 0, ErrTokenInvalid
	}
	if t.GrantType != GrantTypeAuthorizationCode && t.GrantType != GrantTypeRefreshToken {
		return 0, ErrInvalidGrantType
	}
	now := nowUnix()
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"INSERT INTO op_token (grant_id, app_id, mid, grant_type, access_salt, access_hash, refresh_salt, "+
			"refresh_hash, scope, access_expires_at, refresh_expires_at, state, parent_token_id, rotated_at, "+
			"revoked_at, revoke_reason, last_used_at, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, '', 0, ?, ?)",
		t.GrantID, t.AppID, t.Mid, t.GrantType, t.AccessSalt, t.AccessHash, t.RefreshSalt, t.RefreshHash,
		t.Scope, t.AccessExpiresAt, t.RefreshExpiresAt, TokenStateActive, t.ParentTokenID, now, now)
	if err != nil {
		return 0, fmt.Errorf("op_token Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("op_token Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultTokenModel) FindByID(ctx context.Context, tokenID int64) (*Token, error) {
	var t Token
	err := m.conn.QueryRowCtx(ctx, &t,
		"SELECT "+tokenColumns+" FROM op_token WHERE token_id = ? LIMIT 1", tokenID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_token FindByID: %w", err)
	}
	return &t, nil
}

func (m *defaultTokenModel) FindByAccessHash(ctx context.Context, hash string) (*Token, error) {
	if hash == "" {
		return nil, ErrTokenInvalid
	}
	var t Token
	err := m.conn.QueryRowCtx(ctx, &t,
		"SELECT "+tokenColumns+" FROM op_token WHERE access_hash = ? LIMIT 1", hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_token FindByAccessHash: %w", err)
	}
	return &t, nil
}

func (m *defaultTokenModel) FindByRefreshHash(ctx context.Context, hash string) (*Token, error) {
	if hash == "" {
		return nil, ErrTokenInvalid
	}
	var t Token
	err := m.conn.QueryRowCtx(ctx, &t,
		"SELECT "+tokenColumns+" FROM op_token WHERE refresh_hash = ? LIMIT 1", hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_token FindByRefreshHash: %w", err)
	}
	return &t, nil
}

func (m *defaultTokenModel) FindAnyByHash(ctx context.Context, hash string) (*Token, error) {
	if hash == "" {
		return nil, ErrTokenInvalid
	}
	var t Token
	err := m.conn.QueryRowCtx(ctx, &t,
		"SELECT "+tokenColumns+" FROM op_token WHERE access_hash = ? OR refresh_hash = ? LIMIT 1", hash, hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_token FindAnyByHash: %w", err)
	}
	return &t, nil
}

func (m *defaultTokenModel) MarkRotated(ctx context.Context, session sqlx.Session, tokenID, now int64,
	reason string) (bool, error) {
	if tokenID <= 0 {
		return false, ErrTokenInvalid
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE op_token SET state = ?, rotated_at = ?, revoke_reason = ?, mtime = ? "+
			"WHERE token_id = ? AND state = ?",
		TokenStateRotated, now, reason, now, tokenID, TokenStateActive)
	if err != nil {
		return false, fmt.Errorf("op_token MarkRotated: %w", err)
	}
	return rowsPositive(res, "op_token MarkRotated")
}

func (m *defaultTokenModel) MarkRevoked(ctx context.Context, session sqlx.Session, tokenID, now int64,
	reason string) (bool, error) {
	if tokenID <= 0 {
		return false, ErrTokenInvalid
	}
	// ACTIVE 与 ROTATED 都可置为 REVOKED：轮换链上的旧 refresh 一旦被重放也必须能立刻作废。
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE op_token SET state = ?, revoked_at = ?, revoke_reason = ?, mtime = ? "+
			"WHERE token_id = ? AND state IN (?, ?)",
		TokenStateRevoked, now, reason, now, tokenID, TokenStateActive, TokenStateRotated)
	if err != nil {
		return false, fmt.Errorf("op_token MarkRevoked: %w", err)
	}
	return rowsPositive(res, "op_token MarkRevoked")
}

func (m *defaultTokenModel) RevokeByGrant(ctx context.Context, session sqlx.Session, grantID, now int64,
	reason string) (int64, error) {
	if grantID <= 0 {
		return 0, ErrInvalidAppID
	}
	return m.revoke(ctx, session, "grant_id = ?", []any{TokenStateRevoked, now, reason, now, grantID},
		"op_token RevokeByGrant")
}

func (m *defaultTokenModel) RevokeByMid(ctx context.Context, session sqlx.Session, mid, now int64,
	reason string) (int64, error) {
	if mid <= 0 {
		return 0, ErrConsentRequired
	}
	return m.revoke(ctx, session, "mid = ?", []any{TokenStateRevoked, now, reason, now, mid},
		"op_token RevokeByMid")
}

func (m *defaultTokenModel) RevokeByApp(ctx context.Context, session sqlx.Session, appID, now int64,
	reason string) (int64, error) {
	if appID <= 0 {
		return 0, ErrInvalidAppID
	}
	return m.revoke(ctx, session, "app_id = ?", []any{TokenStateRevoked, now, reason, now, appID},
		"op_token RevokeByApp")
}

func (m *defaultTokenModel) RevokeByAppWithScopes(ctx context.Context, session sqlx.Session, appID int64,
	scopes []string, now int64, reason string) (int64, error) {
	if appID <= 0 {
		return 0, ErrInvalidAppID
	}
	if len(scopes) == 0 {
		return 0, nil
	}
	conds := make([]string, 0, len(scopes))
	args := []any{TokenStateRevoked, now, reason, now, appID}
	for _, s := range scopes {
		conds = append(conds, "FIND_IN_SET(?, scope)")
		args = append(args, s)
	}
	cond := "app_id = ? AND (" + strings.Join(conds, " OR ") + ")"
	return m.revoke(ctx, session, cond, args, "op_token RevokeByAppWithScopes")
}

func (m *defaultTokenModel) revoke(ctx context.Context, session sqlx.Session, cond string, args []any,
	op string) (int64, error) {
	// args 顺序 = SET 子句占位符 + cond 占位符；末尾再补 state IN (ACTIVE, ROTATED) 两个值。
	query := "UPDATE op_token SET state = ?, revoked_at = ?, revoke_reason = ?, mtime = ? WHERE " +
		cond + " AND state IN (?, ?)"
	res, err := pick(session, m.conn).ExecCtx(ctx, query, append(args, TokenStateActive, TokenStateRotated)...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return aff, nil
}

func (m *defaultTokenModel) TouchUsed(ctx context.Context, tokenID, ts int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE op_token SET last_used_at = ? WHERE token_id = ? AND last_used_at < ?", ts, tokenID, ts); err != nil {
		return fmt.Errorf("op_token TouchUsed: %w", err)
	}
	return nil
}

func (m *defaultTokenModel) ListByGrant(ctx context.Context, grantID int64, ps int32) ([]*Token, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	var rows []*Token
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+tokenColumns+" FROM op_token WHERE grant_id = ? ORDER BY token_id DESC LIMIT ?",
		grantID, ps)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_token ListByGrant: %w", err)
	}
	return rows, nil
}

func (m *defaultTokenModel) ArchiveExpiredBefore(ctx context.Context, now int64, limit int32) (int64, error) {
	if limit <= 0 {
		return 0, ErrInvalidPage
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_token SET state = ?, mtime = ? WHERE state = ? AND access_expires_at < ? LIMIT ?",
		TokenStateExpired, now, TokenStateActive, now, limit)
	if err != nil {
		return 0, fmt.Errorf("op_token ArchiveExpiredBefore: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_token ArchiveExpiredBefore RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultTokenModel) PurgeByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(ids))
	query := "DELETE FROM op_token WHERE token_id IN (?"
	for _, id := range ids {
		args = append(args, id)
		query += ",?"
	}
	query += ")"
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("op_token PurgeByIDs: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_token PurgeByIDs RowsAffected: %w", err)
	}
	return aff, nil
}

// rowsPositive 把 RowsAffected 转成“是否真的改到了行”，CAS 判定用。
func rowsPositive(res sql.Result, op string) (bool, error) {
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return aff > 0, nil
}
