package model

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// AdminSession 对应 op_admin_session 表：后台管理会话。
// 签发方案与 account 的用户会话一致（随机 token + 落库 + Redis 缓存），
// 但表、key 前缀与 token 命名空间独立（adm_ 前缀），两端 token 互不通用。
type AdminSession struct {
	// Token 后台会话 token，主键。
	Token string `db:"token"`
	// AdminID 会话所属管理员。
	AdminID int64 `db:"admin_id"`
	// Expires 过期时间（Unix 秒）。
	Expires int64 `db:"expires"`
	// State 1 有效、2 已吊销。
	State int32 `db:"state"`
	// IPHash 登录来源 IP 的哈希（不落明文，隐私最小化，见 docs/data-design.md §6）。
	IPHash string `db:"ip_hash"`
	// UserAgent 登录 UA（截断存储）。
	UserAgent string `db:"user_agent"`
	// Ctime 签发时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
}

// AdminSessionModel 抽象 op_admin_session 表。
type AdminSessionModel interface {
	// Insert 签发会话。
	Insert(ctx context.Context, s *AdminSession) error
	// FindByToken 按 token 查询；不存在返回 (nil, nil)。
	FindByToken(ctx context.Context, token string) (*AdminSession, error)
	// Revoke 吊销单个会话。
	Revoke(ctx context.Context, token string) error
	// RevokeAllByAdmin 吊销某管理员的全部会话（禁用账号/重置口令时调用）。
	RevokeAllByAdmin(ctx context.Context, adminID int64) error
	// PurgeExpired 清理过期会话，返回删除行数（运维/cron 用）。
	PurgeExpired(ctx context.Context, before int64) (int64, error)
}

type defaultAdminSessionModel struct {
	conn sqlx.SqlConn
}

// NewAdminSessionModel 构造 op_admin_session 的 sqlx 实现。
func NewAdminSessionModel(conn sqlx.SqlConn) AdminSessionModel {
	return &defaultAdminSessionModel{conn: conn}
}

func (m *defaultAdminSessionModel) Insert(ctx context.Context, s *AdminSession) error {
	if s.Ctime == 0 {
		s.Ctime = nowUnix()
	}
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_admin_session (token, admin_id, expires, state, ip_hash, user_agent, ctime) VALUES (?, ?, ?, ?, ?, ?, ?)",
		s.Token, s.AdminID, s.Expires, s.State, s.IPHash, s.UserAgent, s.Ctime)
	if err != nil {
		return fmt.Errorf("op_admin_session Insert: %w", err)
	}
	return nil
}

func (m *defaultAdminSessionModel) FindByToken(ctx context.Context, token string) (*AdminSession, error) {
	var s AdminSession
	query := "SELECT token, admin_id, expires, state, ip_hash, user_agent, ctime FROM op_admin_session WHERE token = ?"
	if err := m.conn.QueryRowCtx(ctx, &s, query, token); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_session FindByToken: %w", err)
	}
	return &s, nil
}

func (m *defaultAdminSessionModel) Revoke(ctx context.Context, token string) error {
	_, err := m.conn.ExecCtx(ctx, "UPDATE op_admin_session SET state = ? WHERE token = ?", SessionStateRevoked, token)
	return err
}

func (m *defaultAdminSessionModel) RevokeAllByAdmin(ctx context.Context, adminID int64) error {
	_, err := m.conn.ExecCtx(ctx, "UPDATE op_admin_session SET state = ? WHERE admin_id = ? AND state = ?",
		SessionStateRevoked, adminID, SessionStateActive)
	return err
}

func (m *defaultAdminSessionModel) PurgeExpired(ctx context.Context, before int64) (int64, error) {
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM op_admin_session WHERE expires < ? LIMIT 1000", before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
