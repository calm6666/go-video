package model

// 本文件定义登录日志（account_login_log 表）与验证码发送记录
// （account_capture_log 表）实体与查询。

import (
	"context"
	"database/sql"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 登录方式常量。
const (
	// LoginTypePassword 密码登录。
	LoginTypePassword int8 = 1
	// LoginTypeCapture 验证码登录。
	LoginTypeCapture int8 = 2
	// LoginTypeRegister 注册。
	LoginTypeRegister int8 = 3
)

// 登录结果常量。
const (
	// LoginStatusOK 成功。
	LoginStatusOK int8 = 0
	// LoginStatusFail 失败。
	LoginStatusFail int8 = 1
)

// 验证码业务类型常量。
const (
	// CaptureBizLogin 登录。
	CaptureBizLogin int8 = 1
	// CaptureBizRegister 注册。
	CaptureBizRegister int8 = 2
	// CaptureBizRecovery 账号找回。
	CaptureBizRecovery int8 = 3
)

// AccountLoginLog 对应数据库 account_login_log 表，记录登录/注册行为。
type AccountLoginLog struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// Mid 用户 ID（注册失败等场景为 0）
	Mid int64 `db:"mid"`
	// LoginType 登录方式：1 密码、2 验证码、3 注册
	LoginType int8 `db:"login_type"`
	// Status 结果：0 成功、1 失败
	Status int8 `db:"status"`
	// Reason 失败原因
	Reason string `db:"reason"`
	// IP 登录来源 IP
	IP string `db:"ip"`
	// Device 设备标识
	Device string `db:"device"`
	// Buvid 设备 BUVID
	Buvid string `db:"buvid"`
	// TS 登录时间（Unix 秒）
	TS int64 `db:"ts"`
	// CTime 写入时间（Unix 秒）
	CTime int64 `db:"ctime"`
}

// AccountLoginLogModel 抽象 account_login_log 表的查询接口。
type AccountLoginLogModel interface {
	// Add 新增登录日志。
	Add(ctx context.Context, log *AccountLoginLog) error
	// FindByMid 查询某用户最近 N 条登录日志（时间倒序）。
	FindByMid(ctx context.Context, mid int64, limit int32) ([]*AccountLoginLog, error)
}

type defaultAccountLoginLogModel struct {
	conn sqlx.SqlConn
}

// NewAccountLoginLogModel 创建基于 sqlx 的 AccountLoginLogModel 实现。
func NewAccountLoginLogModel(conn sqlx.SqlConn) AccountLoginLogModel {
	return &defaultAccountLoginLogModel{conn: conn}
}

func (m *defaultAccountLoginLogModel) Add(ctx context.Context, log *AccountLoginLog) error {
	query := `INSERT INTO account_login_log (mid, login_type, status, reason, ip, device, buvid, ts, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	now := time.Now().Unix()
	if log.TS == 0 {
		log.TS = now
	}
	log.CTime = now
	res, err := m.conn.ExecCtx(ctx, query, log.Mid, log.LoginType, log.Status, log.Reason, log.IP, log.Device, log.Buvid, log.TS, log.CTime)
	if err != nil {
		return err
	}
	log.ID, err = res.LastInsertId()
	return err
}

func (m *defaultAccountLoginLogModel) FindByMid(ctx context.Context, mid int64, limit int32) ([]*AccountLoginLog, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	query := `SELECT id, mid, login_type, status, reason, ip, device, buvid, ts, ctime FROM account_login_log
		WHERE mid = ? ORDER BY ctime DESC, id DESC LIMIT ?`
	var rows []*AccountLoginLog
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, limit); err != nil {
		if err == sql.ErrNoRows {
			return []*AccountLoginLog{}, nil
		}
		return nil, err
	}
	return rows, nil
}

// AccountCaptureLog 对应数据库 account_capture_log 表，记录验证码发送行为。
type AccountCaptureLog struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// Biz 业务类型：1 登录、2 注册、3 账号找回
	Biz int8 `db:"biz"`
	// Target 接收方（手机号）
	Target string `db:"target"`
	// IP 发送请求来源 IP
	IP string `db:"ip"`
	// Status 发送结果：0 成功、1 失败
	Status int8 `db:"status"`
	// Reason 失败原因
	Reason string `db:"reason"`
	// CTime 创建时间（Unix 秒）
	CTime int64 `db:"ctime"`
}

// AccountCaptureLogModel 抽象 account_capture_log 表的查询接口。
type AccountCaptureLogModel interface {
	// Add 新增发送记录。
	Add(ctx context.Context, log *AccountCaptureLog) error
}

type defaultAccountCaptureLogModel struct {
	conn sqlx.SqlConn
}

// NewAccountCaptureLogModel 创建基于 sqlx 的 AccountCaptureLogModel 实现。
func NewAccountCaptureLogModel(conn sqlx.SqlConn) AccountCaptureLogModel {
	return &defaultAccountCaptureLogModel{conn: conn}
}

func (m *defaultAccountCaptureLogModel) Add(ctx context.Context, log *AccountCaptureLog) error {
	query := `INSERT INTO account_capture_log (biz, target, ip, status, reason, ctime) VALUES (?, ?, ?, ?, ?, ?)`
	log.CTime = time.Now().Unix()
	_, err := m.conn.ExecCtx(ctx, query, log.Biz, log.Target, log.IP, log.Status, log.Reason, log.CTime)
	return err
}
