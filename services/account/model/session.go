package model

// 本文件定义登录会话（account_session 表）实体与查询。
// 参考 passport-auth 的 token/cookie/refresh 存储；本实现单表 + Redis 缓存。

import (
	"context"
	"database/sql"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 会话状态常量。
const (
	// SessionStatusActive 有效。
	SessionStatusActive int8 = 0
	// SessionStatusRevoked 已吊销（登出/改密后）。
	SessionStatusRevoked int8 = 1
)

// AccountSession 对应数据库 account_session 表，记录登录会话。
type AccountSession struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// Token access token（hex）
	Token string `db:"token"`
	// RefreshToken 刷新令牌（hex）
	RefreshToken string `db:"refresh_token"`
	// Mid 用户 ID
	Mid int64 `db:"mid"`
	// Csrf CSRF token（hex）
	Csrf string `db:"csrf"`
	// Expires token 过期时间（Unix 秒）
	Expires int64 `db:"expires"`
	// RefreshExpires 刷新令牌过期时间（Unix 秒）
	RefreshExpires int64 `db:"refresh_expires"`
	// Status 会话状态：0 有效、1 已吊销
	Status int8 `db:"status"`
	// CreateIP 签发来源 IP
	CreateIP string `db:"create_ip"`
	// Device 设备标识
	Device string `db:"device"`
	// Buvid 设备 BUVID
	Buvid string `db:"buvid"`
	// CTime 创建时间（Unix 秒）
	CTime int64 `db:"ctime"`
	// MTime 最近更新时间（Unix 秒）
	MTime int64 `db:"mtime"`
}

// AccountSessionModel 抽象 account_session 表的查询接口。
type AccountSessionModel interface {
	// FindByToken 按 token 查询会话；不存在返回 nil。
	FindByToken(ctx context.Context, token string) (*AccountSession, error)
	// FindByRefresh 按 refresh token 查询会话；不存在返回 nil。
	FindByRefresh(ctx context.Context, refreshToken string) (*AccountSession, error)
	// Insert 新增会话。
	Insert(ctx context.Context, session *AccountSession) error
	// Revoke 按 token 吊销会话。
	Revoke(ctx context.Context, token string) error
	// RevokeAll 吊销某用户全部会话（改密/封禁时调用）。
	RevokeAll(ctx context.Context, mid int64) error
	// UpdateToken 刷新会话：轮换 token 并延长过期时间。
	UpdateToken(ctx context.Context, id int64, token, csrf string, expires int64) error
}

type defaultAccountSessionModel struct {
	conn sqlx.SqlConn
}

// NewAccountSessionModel 创建基于 sqlx 的 AccountSessionModel 实现。
func NewAccountSessionModel(conn sqlx.SqlConn) AccountSessionModel {
	return &defaultAccountSessionModel{conn: conn}
}

func (m *defaultAccountSessionModel) FindByToken(ctx context.Context, token string) (*AccountSession, error) {
	var s AccountSession
	query := `SELECT id, token, refresh_token, mid, csrf, expires, refresh_expires, status, create_ip, device, buvid, ctime, mtime
		FROM account_session WHERE token = ? LIMIT 1`
	if err := m.conn.QueryRowCtx(ctx, &s, query, token); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (m *defaultAccountSessionModel) FindByRefresh(ctx context.Context, refreshToken string) (*AccountSession, error) {
	var s AccountSession
	query := `SELECT id, token, refresh_token, mid, csrf, expires, refresh_expires, status, create_ip, device, buvid, ctime, mtime
		FROM account_session WHERE refresh_token = ? LIMIT 1`
	if err := m.conn.QueryRowCtx(ctx, &s, query, refreshToken); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (m *defaultAccountSessionModel) Insert(ctx context.Context, session *AccountSession) error {
	query := `INSERT INTO account_session
		(token, refresh_token, mid, csrf, expires, refresh_expires, status, create_ip, device, buvid, ctime, mtime)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	now := time.Now().Unix()
	if session.CTime == 0 {
		session.CTime = now
	}
	session.MTime = now
	res, err := m.conn.ExecCtx(ctx, query, session.Token, session.RefreshToken, session.Mid, session.Csrf,
		session.Expires, session.RefreshExpires, session.Status, session.CreateIP, session.Device, session.Buvid,
		session.CTime, session.MTime)
	if err != nil {
		return err
	}
	session.ID, err = res.LastInsertId()
	return err
}

func (m *defaultAccountSessionModel) Revoke(ctx context.Context, token string) error {
	query := `UPDATE account_session SET status = 1, mtime = ? WHERE token = ? AND status = 0`
	_, err := m.conn.ExecCtx(ctx, query, time.Now().Unix(), token)
	return err
}

func (m *defaultAccountSessionModel) RevokeAll(ctx context.Context, mid int64) error {
	query := `UPDATE account_session SET status = 1, mtime = ? WHERE mid = ? AND status = 0`
	_, err := m.conn.ExecCtx(ctx, query, time.Now().Unix(), mid)
	return err
}

func (m *defaultAccountSessionModel) UpdateToken(ctx context.Context, id int64, token, csrf string, expires int64) error {
	query := `UPDATE account_session SET token = ?, csrf = ?, expires = ?, mtime = ? WHERE id = ?`
	_, err := m.conn.ExecCtx(ctx, query, token, csrf, expires, time.Now().Unix(), id)
	return err
}
