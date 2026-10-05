package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// AuthCode OAuth 授权码行（op_auth_code 表）。
//
// 只存哈希：Salt + Hash = HMAC-SHA256(pepper, salt || code)，与 client_secret 同一套
// 存储口径（对齐 services/account 的凭证表形态）。明文 code 只在签发响应里出现一次，
// 经 gateway 302 回跳传递，绝不入库、不入日志。
//
// 一次性消费：used_at 从 0 翻到非 0 是 CAS，保证同一 code 只能换出一次 token。
// 重放（used_at 已非 0）返回 ErrAuthCodeUsed，并保留 first/second 次调用方信息用于告警。
type AuthCode struct {
	// CodeID 授权码行 ID（主键）
	CodeID int64 `db:"code_id"`
	// AppID 应用 ID
	AppID int64 `db:"app_id"`
	// Mid 授权用户 mid（真值在 account/user-profile，本表只存主键）
	Mid int64 `db:"mid"`
	// Salt 随机盐（hex）
	Salt string `db:"salt"`
	// Hash 授权码哈希（hex），唯一索引，换码入口
	Hash string `db:"hash"`
	// Scope 用户已同意的 scope 快照（逗号分隔，升序；换码时直接继承，不再询问用户）
	Scope string `db:"scope"`
	// RedirectURI 签发时的回调地址（换码需一致，防授权码被转移到别的回调地址）
	RedirectURI string `db:"redirect_uri"`
	// State 客户端 state，仅用于回显与排障
	State string `db:"state"`
	// GrantID 预建的授权关系 ID（换码前即确定，便于撤销位点连贯）
	GrantID int64 `db:"grant_id"`
	// ExpiresAt 过期时间（Unix 秒，AuthCodeTTLSeconds 默认 60s）
	ExpiresAt int64 `db:"expires_at"`
	// UsedAt 消费时间（0 表示未消费）
	UsedAt int64 `db:"used_at"`
	// ConsumedByTokenID 消费该 code 签发出的 token 行 ID（0 表示未消费）
	ConsumedByTokenID int64 `db:"consumed_by_token_id"`
	// ReplayCount 重放尝试次数（>0 说明 code 可能泄露，需要告警）
	ReplayCount int32 `db:"replay_count"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// Expired 判断授权码是否在给定时间已过期（过期与未过期都走同一 CAS 消费路径，
// logic 需先判过期以返回准确错误码）。
func (a *AuthCode) Expired(now int64) bool {
	return a != nil && a.ExpiresAt > 0 && a.ExpiresAt <= now
}

// Usable 判断能否消费。
func (a *AuthCode) Usable(now int64) bool {
	return a != nil && a.UsedAt == 0 && !a.Expired(now)
}

// AuthCodeModel op_auth_code 表读写接口。
type AuthCodeModel interface {
	// Insert 签发授权码；命中 uniq_hash 时返回既有行 ID 且 created=false（极小概率碰撞，
	// 调用方应重新生成明文再签，不复用同一 code_id）。
	Insert(ctx context.Context, a *AuthCode) (codeID int64, created bool, err error)
	// InsertTx 事务内签发授权码：要与 op_grant 的 FindOrCreate 同事务提交，
	// 否则会出现「有 code 无 grant」的孤儿凭证（撤销位点无从比对）。
	InsertTx(ctx context.Context, session sqlx.Session, a *AuthCode) (codeID int64, created bool, err error)
	// FindByID 主键查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, codeID int64) (*AuthCode, error)
	// FindByHash 按哈希查询（换码入口）；不存在返回 (nil, nil)。
	FindByHash(ctx context.Context, hash string) (*AuthCode, error)
	// MarkUsed CAS 消费：仅当 used_at=0 时成功，返回 applied=false 表示已被消费（重放）。
	// 成功的同时把 grant 的 current token 指过去，因此接受 session。
	MarkUsed(ctx context.Context, session sqlx.Session, codeID, tokenID, now int64) (applied bool, err error)
	// IncrReplay 记录一次重放尝试（错误路径也留痕，供风控与告警）。
	IncrReplay(ctx context.Context, codeID int64) error
	// CountRecentByMid 统计用户近 since 秒内的签发次数（授权端点限频，防批量刷码）。
	CountRecentByMid(ctx context.Context, mid, since int64) (int64, error)
	// ListExpiredBefore 返回可清理的过期 code_id（清理任务用，上限由调用方控制）。
	ListExpiredBefore(ctx context.Context, before int64, limit int32) ([]int64, error)
	// PurgeByIDs 物理删除授权码行（短期凭证无需保留正文哈希，审计只留 grant/token）。
	PurgeByIDs(ctx context.Context, ids []int64) (int64, error)
}

type defaultAuthCodeModel struct {
	conn sqlx.SqlConn
}

// NewAuthCodeModel 创建 AuthCodeModel 实现。
func NewAuthCodeModel(conn sqlx.SqlConn) AuthCodeModel {
	return &defaultAuthCodeModel{conn: conn}
}

const authCodeColumns = `code_id, app_id, mid, salt, hash, scope, redirect_uri, state, grant_id,
	expires_at, used_at, consumed_by_token_id, replay_count, ctime, mtime`

func (m *defaultAuthCodeModel) Insert(ctx context.Context, a *AuthCode) (int64, bool, error) {
	return m.insert(ctx, nil, a)
}

func (m *defaultAuthCodeModel) InsertTx(ctx context.Context, session sqlx.Session,
	a *AuthCode) (int64, bool, error) {
	return m.insert(ctx, session, a)
}

func (m *defaultAuthCodeModel) insert(ctx context.Context, session sqlx.Session,
	a *AuthCode) (int64, bool, error) {
	if a.AppID <= 0 {
		return 0, false, ErrInvalidAppID
	}
	if a.Mid <= 0 {
		return 0, false, ErrConsentRequired
	}
	if a.Hash == "" || a.Salt == "" {
		return 0, false, ErrAuthCodeInvalid
	}
	now := nowUnix()
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"INSERT INTO op_auth_code (app_id, mid, salt, hash, scope, redirect_uri, state, grant_id, "+
			"expires_at, used_at, consumed_by_token_id, replay_count, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?, ?)",
		a.AppID, a.Mid, a.Salt, a.Hash, a.Scope, a.RedirectURI, a.State, a.GrantID, a.ExpiresAt, now, now)
	if err != nil {
		// hash 唯一：极小概率碰撞时不返回旧行内容，只报告未创建，由调用方重新生成。
		// 回读走同一 session：事务内用连接池另开一条读，会读到「别人已提交的同哈希行」，
		// 把 created=false 的既有 ID 交给 logic 就等于允许消费一条不属于自己的授权码。
		if isDuplicateKeyErr(err) {
			var old AuthCode
			qerr := pick(session, m.conn).QueryRowCtx(ctx, &old,
				"SELECT "+authCodeColumns+" FROM op_auth_code WHERE hash = ? LIMIT 1", a.Hash)
			if qerr == nil {
				return old.CodeID, false, nil
			}
		}
		return 0, false, fmt.Errorf("op_auth_code Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("op_auth_code Insert LastInsertId: %w", err)
	}
	return id, true, nil
}

func (m *defaultAuthCodeModel) FindByID(ctx context.Context, codeID int64) (*AuthCode, error) {
	var a AuthCode
	err := m.conn.QueryRowCtx(ctx, &a,
		"SELECT "+authCodeColumns+" FROM op_auth_code WHERE code_id = ? LIMIT 1", codeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_auth_code FindByID: %w", err)
	}
	return &a, nil
}

func (m *defaultAuthCodeModel) FindByHash(ctx context.Context, hash string) (*AuthCode, error) {
	if hash == "" {
		return nil, ErrAuthCodeInvalid
	}
	var a AuthCode
	err := m.conn.QueryRowCtx(ctx, &a,
		"SELECT "+authCodeColumns+" FROM op_auth_code WHERE hash = ? LIMIT 1", hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_auth_code FindByHash: %w", err)
	}
	return &a, nil
}

func (m *defaultAuthCodeModel) MarkUsed(ctx context.Context, session sqlx.Session, codeID, tokenID,
	now int64) (bool, error) {
	if codeID <= 0 {
		return false, ErrAuthCodeInvalid
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE op_auth_code SET used_at = ?, consumed_by_token_id = ?, mtime = ? "+
			"WHERE code_id = ? AND used_at = 0",
		now, tokenID, now, codeID)
	if err != nil {
		return false, fmt.Errorf("op_auth_code MarkUsed: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("op_auth_code MarkUsed RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultAuthCodeModel) IncrReplay(ctx context.Context, codeID int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE op_auth_code SET replay_count = replay_count + 1, mtime = ? WHERE code_id = ?",
		nowUnix(), codeID); err != nil {
		return fmt.Errorf("op_auth_code IncrReplay: %w", err)
	}
	return nil
}

func (m *defaultAuthCodeModel) CountRecentByMid(ctx context.Context, mid, since int64) (int64, error) {
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM op_auth_code WHERE mid = ? AND ctime >= ?", mid, since)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("op_auth_code CountRecentByMid: %w", err)
	}
	return cnt, nil
}

func (m *defaultAuthCodeModel) ListExpiredBefore(ctx context.Context, before int64, limit int32) ([]int64, error) {
	if limit <= 0 {
		return nil, ErrInvalidPage
	}
	var ids []int64
	// 清理路径只取主键，禁止把哈希列读进内存再丢弃。
	err := m.conn.QueryRowsCtx(ctx, &ids,
		"SELECT code_id FROM op_auth_code WHERE expires_at < ? LIMIT ?", before, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_auth_code ListExpiredBefore: %w", err)
	}
	return ids, nil
}

func (m *defaultAuthCodeModel) PurgeByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(ids))
	query := "DELETE FROM op_auth_code WHERE code_id IN (?"
	for _, id := range ids {
		args = append(args, id)
		query += ",?"
	}
	query += ")"
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("op_auth_code PurgeByIDs: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_auth_code PurgeByIDs RowsAffected: %w", err)
	}
	return aff, nil
}
