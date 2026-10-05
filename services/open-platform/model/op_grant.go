package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 授权关系状态，与 op_grant.status 一致。
const (
	// GrantStatusActive 用户已授权且未撤销。
	GrantStatusActive int32 = 1
	// GrantStatusRevoked 已撤销（位点 revoked_at 为真值，status 仅作展示与索引）。
	GrantStatusRevoked int32 = 2
)

// Grant 用户对第三方应用的授权关系（op_grant 表），同时是「撤销位点」。
//
// 唯一键 (app_id, mid)：同一用户对同一应用只有一条授权关系，重新授权复用同一行并覆盖
// scope 快照，旧的 access/refresh token 在轮换链上自然失效，避免“多份并行授权”导致
// 撤销漏网（撤销一条 grant 即可让用户对该应用的全部 token 立即失效）。
//
// 撤销位点语义：RevokedAt 是「撤销发生的 Unix 秒」，token 校验时若
// token.Ctime < grant.RevokedAt（或 token 自身已被逐条标记）即拒绝。
// 这是「撤销立即生效」的第二道保险：即使某条 op_token 行因为并发没被标记，
// 位点比对也会拒绝；反过来，签发时间晚于位点的新 token 不受影响。
type Grant struct {
	// GrantID 授权关系 ID（跨服务引用主键）
	GrantID int64 `db:"grant_id"`
	// AppID 应用 ID
	AppID int64 `db:"app_id"`
	// Mid 授权用户 mid
	Mid int64 `db:"mid"`
	// Scope 用户同意的 scope 快照（逗号分隔、升序；token 只继承不扩大）
	Scope string `db:"scope"`
	// Status 状态，见 GrantStatus*
	Status int32 `db:"status"`
	// ConsentGiven 用户显式同意的标记（0 表示从未同意，签发授权码时强制为 1）
	ConsentGiven int8 `db:"consent_given"`
	// ConsentAt 最近一次同意时间（Unix 秒）
	ConsentAt int64 `db:"consent_at"`
	// CurrentTokenID 当前有效的 token 链头（轮换时前移；0 表示从未换出 token）
	CurrentTokenID int64 `db:"current_token_id"`
	// RotateSeq 轮换代数（观测 refresh 是否被异常高频轮换）
	RotateSeq int64 `db:"rotate_seq"`
	// LastCodeID 最近一次消费的授权码（审计链路：grant ← code ← token）
	LastCodeID int64 `db:"last_code_id"`
	// RevokedAt 撤销位点（0 表示未撤销）
	RevokedAt int64 `db:"revoked_at"`
	// RevokeReason 撤销原因（审计，脱敏）
	RevokeReason string `db:"revoke_reason"`
	// RevokeOperator 撤销触发者：用户本人 mid 或运营 mid
	RevokeOperator int64 `db:"revoke_operator"`
	// Ctime 首次授权时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// Granted 判断在给定时刻是否处于有效授权（未撤销）。
func (g *Grant) Granted(now int64) bool {
	return g != nil && g.RevokedAt == 0 && g.Status == GrantStatusActive
}

// TokenAccepted 判断某个 token（签发时间 issuedAt）是否晚于撤销位点。
// 撤销立即生效的核心比对：位点为 0 时全部通过。
func (g *Grant) TokenAccepted(issuedAt int64) bool {
	return g != nil && (g.RevokedAt == 0 || issuedAt > g.RevokedAt)
}

// GrantModel op_grant 表读写接口。
type GrantModel interface {
	// FindOrCreate 用户同意授权后取得/刷新授权关系：命中唯一键则更新 scope 快照、
	// 同意时间并清空撤销位点（重新授权必须能覆盖历史撤销）。session 非空走事务，
	// 以便与 op_app_scope / op_auth_code 同事务提交。
	FindOrCreate(ctx context.Context, session sqlx.Session, appID, mid int64, scopes []string,
		consentGiven bool) (grantID int64, created bool, err error)
	// FindByID 主键查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, grantID int64) (*Grant, error)
	// FindByAppMid 唯一键查询（签发授权码与撤销的入口）。
	FindByAppMid(ctx context.Context, appID, mid int64) (*Grant, error)
	// ListByMid 用户侧「已授权的第三方应用」分页：cursor 为 (mtime, grant_id) 倒序位点。
	ListByMid(ctx context.Context, mid int64, cursorTime, cursorID int64, ps int32) ([]*Grant, error)
	// ListByApp 运营侧查看某应用获得的授权（只返回未撤销的，游标同上）。
	ListByApp(ctx context.Context, appID int64, cursorTime, cursorID int64, ps int32) ([]*Grant, error)
	// MarkRevoked 撤销单条 grant：写位点 + 置状态，条件 revoked_at=0（幂等，已撤销返回 false）。
	MarkRevoked(ctx context.Context, session sqlx.Session, grantID, operator int64,
		reason string, now int64) (applied bool, err error)
	// RevokeByMid 撤销用户全部第三方授权（改密/风控一键下线场景）。返回受影响行数。
	RevokeByMid(ctx context.Context, session sqlx.Session, mid, operator int64, reason string,
		now int64) (int64, error)
	// TouchTokenHead 轮换时前移链头并累加轮换代数（CAS：仅当当前链头等于 fromTokenID 才推进，
	// 防止并发轮换把旧链接回去）。
	TouchTokenHead(ctx context.Context, session sqlx.Session, grantID, fromTokenID, toTokenID int64,
		now int64) (applied bool, err error)
	// CountActiveByApp 统计应用有效授权用户数（运营视图）。
	CountActiveByApp(ctx context.Context, appID int64) (int64, error)
	// RevokeByAppWithScopes 回收 scope 时定点撤销授权关系：只命中 scope 快照包含被回收集合的 grant。
	// 与 TokenModel.RevokeByAppWithScopes 同一集合化口径，避免 logic 分页遍历 grant 造成的
	// 未定义规模扫描（撤销的安全属性不能随数据量退化）。
	RevokeByAppWithScopes(ctx context.Context, session sqlx.Session, appID int64, scopes []string,
		operator int64, reason string, now int64) (int64, error)
	// TouchLastCode 回填最近一次消费的授权码（审计链路 grant ← code ← token）。
	// session 非空以与 MarkUsed/TouchTokenHead 同事务提交。
	TouchLastCode(ctx context.Context, session sqlx.Session, grantID, codeID, now int64) error
	// ScopeOf 读取 grant 的 scope 快照列表（logic 判定时避免整行载入）。
	ScopeOf(ctx context.Context, grantID int64) ([]string, error)
}

type defaultGrantModel struct {
	conn sqlx.SqlConn
}

// NewGrantModel 创建 GrantModel 实现。
func NewGrantModel(conn sqlx.SqlConn) GrantModel {
	return &defaultGrantModel{conn: conn}
}

const grantColumns = `grant_id, app_id, mid, scope, status, consent_given, consent_at, current_token_id,
	rotate_seq, last_code_id, revoked_at, revoke_reason, revoke_operator, ctime, mtime`

func (m *defaultGrantModel) FindOrCreate(ctx context.Context, session sqlx.Session, appID, mid int64,
	scopes []string, consentGiven bool) (int64, bool, error) {
	if appID <= 0 {
		return 0, false, ErrInvalidAppID
	}
	if mid <= 0 {
		return 0, false, ErrConsentRequired
	}
	if !consentGiven {
		// 服务端不接受“隐式同意”，这是 scope 最小权限的入口护栏。
		return 0, false, ErrConsentRequired
	}
	now := nowUnix()
	scopeStr := JoinScopes(scopes)
	// 唯一键 (app_id, mid)：重新授权覆盖 scope 快照并清空撤销位点。
	// 注意 created 判定用 ROW_COUNT 语义：MySQL 对 INSERT 返回 1、对 ON DUPLICATE UPDATE 返回 2，
	// 未变更返回 0，因此这里再用 affected==1 判新建（与 pm_conversation 同一手法）。
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"INSERT INTO op_grant (app_id, mid, scope, status, consent_given, consent_at, current_token_id, "+
			"rotate_seq, last_code_id, revoked_at, revoke_reason, revoke_operator, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, 1, ?, 0, 0, 0, 0, '', 0, ?, ?) "+
			"ON DUPLICATE KEY UPDATE scope = VALUES(scope), status = VALUES(status), consent_given = 1, "+
			"consent_at = VALUES(consent_at), revoked_at = 0, revoke_reason = '', revoke_operator = 0, "+
			"mtime = VALUES(mtime)",
		appID, mid, scopeStr, GrantStatusActive, now, now, now)
	if err != nil {
		return 0, false, fmt.Errorf("op_grant FindOrCreate: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("op_grant FindOrCreate RowsAffected: %w", err)
	}
	created := affected == 1
	// 回读必须走同一个 session：事务内刚插入/更新的行对连接池上的另一条连接不可见，
	// 用 m.conn 读会在「与授权码同事务」的签发路径上稳定读到空行，把成功事务判成失败。
	grant, err := m.findByAppMid(ctx, session, appID, mid)
	if err != nil {
		return 0, false, err
	}
	if grant == nil {
		return 0, false, ErrGrantRevoked
	}
	return grant.GrantID, created, nil
}

func (m *defaultGrantModel) FindByID(ctx context.Context, grantID int64) (*Grant, error) {
	var g Grant
	err := m.conn.QueryRowCtx(ctx, &g,
		"SELECT "+grantColumns+" FROM op_grant WHERE grant_id = ? LIMIT 1", grantID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_grant FindByID: %w", err)
	}
	return &g, nil
}

func (m *defaultGrantModel) FindByAppMid(ctx context.Context, appID, mid int64) (*Grant, error) {
	if appID <= 0 || mid <= 0 {
		return nil, ErrInvalidAppID
	}
	return m.findByAppMid(ctx, nil, appID, mid)
}

// findByAppMid 唯一键查询的内部实现：session 非空时走同一会话（事务），
// 以便 FindOrCreate 在事务内读到自己刚插入的行。
func (m *defaultGrantModel) findByAppMid(ctx context.Context, session sqlx.Session,
	appID, mid int64) (*Grant, error) {
	var g Grant
	err := pick(session, m.conn).QueryRowCtx(ctx, &g,
		"SELECT "+grantColumns+" FROM op_grant WHERE app_id = ? AND mid = ? LIMIT 1", appID, mid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_grant FindByAppMid: %w", err)
	}
	return &g, nil
}

func (m *defaultGrantModel) ListByMid(ctx context.Context, mid int64, cursorTime, cursorID int64,
	ps int32) ([]*Grant, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	if mid <= 0 {
		return nil, ErrConsentRequired
	}
	return m.list(ctx, []string{"mid = ?", "revoked_at = 0"}, []any{mid}, cursorTime, cursorID, ps)
}

func (m *defaultGrantModel) ListByApp(ctx context.Context, appID int64, cursorTime, cursorID int64,
	ps int32) ([]*Grant, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	if appID <= 0 {
		return nil, ErrInvalidAppID
	}
	return m.list(ctx, []string{"app_id = ?", "revoked_at = 0"}, []any{appID}, cursorTime, cursorID, ps)
}

func (m *defaultGrantModel) list(ctx context.Context, conds []string, args []any, cursorTime, cursorID int64,
	ps int32) ([]*Grant, error) {
	if cursorTime > 0 {
		conds = append(conds, "(mtime < ? OR (mtime = ? AND grant_id < ?))")
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	query := "SELECT " + grantColumns + " FROM op_grant WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY mtime DESC, grant_id DESC LIMIT ?"
	args = append(args, ps)

	var rows []*Grant
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_grant list: %w", err)
	}
	return rows, nil
}

func (m *defaultGrantModel) MarkRevoked(ctx context.Context, session sqlx.Session, grantID, operator int64,
	reason string, now int64) (bool, error) {
	if grantID <= 0 {
		return false, ErrInvalidAppID
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE op_grant SET status = ?, revoked_at = ?, revoke_reason = ?, revoke_operator = ?, mtime = ? "+
			"WHERE grant_id = ? AND revoked_at = 0",
		GrantStatusRevoked, now, reason, operator, now, grantID)
	if err != nil {
		return false, fmt.Errorf("op_grant MarkRevoked: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("op_grant MarkRevoked RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultGrantModel) RevokeByMid(ctx context.Context, session sqlx.Session, mid, operator int64,
	reason string, now int64) (int64, error) {
	if mid <= 0 {
		return 0, ErrConsentRequired
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE op_grant SET status = ?, revoked_at = ?, revoke_reason = ?, revoke_operator = ?, mtime = ? "+
			"WHERE mid = ? AND revoked_at = 0",
		GrantStatusRevoked, now, reason, operator, now, mid)
	if err != nil {
		return 0, fmt.Errorf("op_grant RevokeByMid: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_grant RevokeByMid RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultGrantModel) TouchTokenHead(ctx context.Context, session sqlx.Session, grantID, fromTokenID,
	toTokenID int64, now int64) (bool, error) {
	if grantID <= 0 {
		return false, ErrInvalidAppID
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE op_grant SET current_token_id = ?, rotate_seq = rotate_seq + 1, mtime = ? "+
			"WHERE grant_id = ? AND current_token_id = ?",
		toTokenID, now, grantID, fromTokenID)
	if err != nil {
		return false, fmt.Errorf("op_grant TouchTokenHead: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("op_grant TouchTokenHead RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultGrantModel) RevokeByAppWithScopes(ctx context.Context, session sqlx.Session, appID int64,
	scopes []string, operator int64, reason string, now int64) (int64, error) {
	if appID <= 0 {
		return 0, ErrInvalidAppID
	}
	if len(scopes) == 0 {
		return 0, nil
	}
	conds := make([]string, 0, len(scopes))
	args := []any{GrantStatusRevoked, now, reason, operator, now, appID}
	for _, s := range scopes {
		conds = append(conds, "FIND_IN_SET(?, scope)")
		args = append(args, s)
	}
	query := "UPDATE op_grant SET status = ?, revoked_at = ?, revoke_reason = ?, revoke_operator = ?, " +
		"mtime = ? WHERE app_id = ? AND revoked_at = 0 AND (" + strings.Join(conds, " OR ") + ")"
	res, err := pick(session, m.conn).ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("op_grant RevokeByAppWithScopes: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_grant RevokeByAppWithScopes RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultGrantModel) TouchLastCode(ctx context.Context, session sqlx.Session, grantID, codeID,
	now int64) error {
	if grantID <= 0 || codeID <= 0 {
		return ErrInvalidAppID
	}
	if _, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE op_grant SET last_code_id = ?, mtime = ? WHERE grant_id = ?", codeID, now, grantID); err != nil {
		return fmt.Errorf("op_grant TouchLastCode: %w", err)
	}
	return nil
}

func (m *defaultGrantModel) CountActiveByApp(ctx context.Context, appID int64) (int64, error) {
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM op_grant WHERE app_id = ? AND revoked_at = 0", appID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("op_grant CountActiveByApp: %w", err)
	}
	return cnt, nil
}

func (m *defaultGrantModel) ScopeOf(ctx context.Context, grantID int64) ([]string, error) {
	var raw string
	err := m.conn.QueryRowCtx(ctx, &raw, "SELECT scope FROM op_grant WHERE grant_id = ? LIMIT 1", grantID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_grant ScopeOf: %w", err)
	}
	return SplitScopes(raw), nil
}
