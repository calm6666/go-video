package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 密钥状态，与 op_app_secret.status 一致（沿用 account_secret 的“生效位 + 历史行”形态）。
const (
	// SecretStatusActive 当前生效。
	SecretStatusActive int8 = 0
	// SecretStatusHistory 历史（轮换或吊销后保留，供审计回溯）。
	SecretStatusHistory int8 = 1
)

// AppSecret client_secret 凭证行（op_app_secret 表）。
//
// 存储方式与 services/account 的凭证表同形：一行一把密钥，salt 与 hash 分列，
// status 作生效位，轮换时旧行置历史而不是删除。差异（在 README 中显式记录）：
// account 的密码哈希沿用历史 MD5(pwd+pepper+salt) 以兼容 passport 客户端，
// 本域没有历史包袱，使用 HMAC-SHA256(pepper, salt || secret)，pepper 来自 Secret/Vault、
// 既不入库也不入仓库；因此数据库泄露也无法离线还原或伪造密钥。
//
// 明文 secret 只在 RegisterApplication / RotateApplicationSecret 响应中出现一次；
// 本表的 Salt/Hash 字段禁止出现在任何 RPC 响应、日志与事件里。
type AppSecret struct {
	// SecretID 密钥行 ID（主键）
	SecretID int64 `db:"secret_id"`
	// AppID 应用 ID
	AppID int64 `db:"app_id"`
	// Salt 随机盐（hex），每把密钥独立生成
	Salt string `db:"salt"`
	// Hash HMAC-SHA256(pepper, salt || secret) 的 hex
	Hash string `db:"hash"`
	// Status 生效位：0 生效、1 历史
	Status int8 `db:"status"`
	// ExpiresAt 失效时间（Unix 秒，0 表示按配置长期有效；宽限期用该列表达）
	ExpiresAt int64 `db:"expires_at"`
	// LastUsedAt 最近一次签名校验通过时间（用于发现长期不用的密钥）
	LastUsedAt int64 `db:"last_used_at"`
	// RotateReason 轮换/吊销原因（审计，脱敏）
	RotateReason string `db:"rotate_reason"`
	// OperatorMid 触发该变更的 mid（owner 或运营）
	OperatorMid int64 `db:"operator_mid"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// Usable 判断该密钥行当前是否可用于验签。
func (s *AppSecret) Usable(now int64) bool {
	if s == nil || s.Status != SecretStatusActive {
		return false
	}
	return s.ExpiresAt == 0 || s.ExpiresAt > now
}

// AppSecretModel op_app_secret 表读写接口。
type AppSecretModel interface {
	// Insert 新增一把生效密钥。
	Insert(ctx context.Context, s *AppSecret) (int64, error)
	// InsertTx 事务内新增密钥：注册流程要与 op_app 同事务提交。
	InsertTx(ctx context.Context, session sqlx.Session, s *AppSecret) (int64, error)
	// FindByID 查询密钥行；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, secretID int64) (*AppSecret, error)
	// FindActive 查询当前可验签的密钥（status=0 且未过期），按创建时间倒序返回，
	// 宽限期内可能同时存在两把，logic 逐个尝试比对并记录命中的 secret_id。
	FindActive(ctx context.Context, appID int64, now int64) ([]*AppSecret, error)
	// MarkHistory 把指定密钥置为历史；expiresAt>0 时保留生效位到该时间（宽限期）。
	MarkHistory(ctx context.Context, secretID int64, expiresAt int64) error
	// MarkHistoryTx 事务内把指定密钥置为历史（轮换要与新密钥同事务生效）。
	MarkHistoryTx(ctx context.Context, session sqlx.Session, secretID int64, expiresAt int64) error
	// MarkAllHistory 吊销该应用全部密钥（紧急下架）。返回受影响行数。
	MarkAllHistory(ctx context.Context, appID int64, operator int64, reason string) (int64, error)
	// TouchUsed 记录密钥使用时间（限频写入由调用方负责，避免每次请求都写库）。
	TouchUsed(ctx context.Context, secretID int64, ts int64) error
	// CountActive 统计生效密钥数（应用投影里的 SecretState 用）。
	CountActive(ctx context.Context, appID int64, now int64) (int64, error)
	// SummariesByApps 批量统计多个应用的密钥概览（列表投影用，避免逐应用 CountActive 的 N+1）。
	// 只读非敏感列：salt/hash 一律不进内存，列表响应也永远不含密钥材料。
	SummariesByApps(ctx context.Context, appIDs []int64, now int64) (map[int64]*SecretSummary, error)
}

// SecretSummary 单个应用的密钥概览（不含任何密钥材料）。
type SecretSummary struct {
	// Total 密钥行数（含历史）
	Total int64
	// Active 当前可签发的生效行数
	Active int64
	// LastCtime 最近一把密钥的创建时间（列表里作为 secret_rotated_at 展示）
	LastCtime int64
}

type defaultAppSecretModel struct {
	conn sqlx.SqlConn
}

// NewAppSecretModel 创建 AppSecretModel 实现。
func NewAppSecretModel(conn sqlx.SqlConn) AppSecretModel {
	return &defaultAppSecretModel{conn: conn}
}

const appSecretColumns = `secret_id, app_id, salt, hash, status, expires_at, last_used_at, rotate_reason, operator_mid, ctime, mtime`

func (m *defaultAppSecretModel) Insert(ctx context.Context, s *AppSecret) (int64, error) {
	return m.insert(ctx, nil, s)
}

func (m *defaultAppSecretModel) InsertTx(ctx context.Context, session sqlx.Session, s *AppSecret) (int64, error) {
	return m.insert(ctx, session, s)
}

func (m *defaultAppSecretModel) insert(ctx context.Context, session sqlx.Session, s *AppSecret) (int64, error) {
	if s.AppID <= 0 {
		return 0, ErrInvalidAppID
	}
	if s.Salt == "" || s.Hash == "" {
		return 0, ErrSecretNotConfigured
	}
	now := nowUnix()
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"INSERT INTO op_app_secret (app_id, salt, hash, status, expires_at, last_used_at, rotate_reason, "+
			"operator_mid, ctime, mtime) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?)",
		s.AppID, s.Salt, s.Hash, SecretStatusActive, s.ExpiresAt, s.RotateReason, s.OperatorMid, now, now)
	if err != nil {
		return 0, fmt.Errorf("op_app_secret Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("op_app_secret Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultAppSecretModel) FindByID(ctx context.Context, secretID int64) (*AppSecret, error) {
	var s AppSecret
	err := m.conn.QueryRowCtx(ctx, &s,
		"SELECT "+appSecretColumns+" FROM op_app_secret WHERE secret_id = ? LIMIT 1", secretID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_app_secret FindByID: %w", err)
	}
	return &s, nil
}

func (m *defaultAppSecretModel) FindActive(ctx context.Context, appID int64, now int64) ([]*AppSecret, error) {
	var rows []*AppSecret
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+appSecretColumns+" FROM op_app_secret WHERE app_id = ? AND status = ? "+
			"AND (expires_at = 0 OR expires_at > ?) ORDER BY secret_id DESC",
		appID, SecretStatusActive, now)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_app_secret FindActive: %w", err)
	}
	return rows, nil
}

func (m *defaultAppSecretModel) MarkHistory(ctx context.Context, secretID int64, expiresAt int64) error {
	return m.markHistory(ctx, nil, secretID, expiresAt)
}

func (m *defaultAppSecretModel) MarkHistoryTx(ctx context.Context, session sqlx.Session, secretID int64,
	expiresAt int64) error {
	return m.markHistory(ctx, session, secretID, expiresAt)
}

func (m *defaultAppSecretModel) markHistory(ctx context.Context, session sqlx.Session, secretID int64,
	expiresAt int64) error {
	// expiresAt>0 表示宽限期：保持生效位但到期即失效；0 表示立即置历史。
	query := "UPDATE op_app_secret SET status = ?, mtime = ? WHERE secret_id = ? AND status = ?"
	args := []any{SecretStatusHistory, nowUnix(), secretID, SecretStatusActive}
	if expiresAt > 0 {
		query = "UPDATE op_app_secret SET expires_at = ?, mtime = ? WHERE secret_id = ? AND status = ?"
		args = []any{expiresAt, nowUnix(), secretID, SecretStatusActive}
	}
	if _, err := pick(session, m.conn).ExecCtx(ctx, query, args...); err != nil {
		return fmt.Errorf("op_app_secret MarkHistory: %w", err)
	}
	return nil
}

func (m *defaultAppSecretModel) MarkAllHistory(ctx context.Context, appID int64, operator int64, reason string) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_app_secret SET status = ?, expires_at = 0, rotate_reason = ?, operator_mid = ?, mtime = ? "+
			"WHERE app_id = ? AND status = ?",
		SecretStatusHistory, reason, operator, nowUnix(), appID, SecretStatusActive)
	if err != nil {
		return 0, fmt.Errorf("op_app_secret MarkAllHistory: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_app_secret MarkAllHistory RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultAppSecretModel) TouchUsed(ctx context.Context, secretID int64, ts int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE op_app_secret SET last_used_at = ? WHERE secret_id = ? AND last_used_at < ?",
		ts, secretID, ts); err != nil {
		return fmt.Errorf("op_app_secret TouchUsed: %w", err)
	}
	return nil
}

func (m *defaultAppSecretModel) CountActive(ctx context.Context, appID int64, now int64) (int64, error) {
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM op_app_secret WHERE app_id = ? AND status = ? AND (expires_at = 0 OR expires_at > ?)",
		appID, SecretStatusActive, now)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("op_app_secret CountActive: %w", err)
	}
	return cnt, nil
}

// secretSummaryRow 概览查询的中转行：刻意不含 salt/hash 列，
// 密钥材料在本方法的路径上根本没有离开数据库的可能。
type secretSummaryRow struct {
	AppID     int64 `db:"app_id"`
	Status    int8  `db:"status"`
	ExpiresAt int64 `db:"expires_at"`
	Ctime     int64 `db:"ctime"`
}

func (m *defaultAppSecretModel) SummariesByApps(ctx context.Context, appIDs []int64,
	now int64) (map[int64]*SecretSummary, error) {
	out := make(map[int64]*SecretSummary, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(appIDs))
	for _, id := range appIDs {
		args = append(args, id)
	}
	query := "SELECT app_id, status, expires_at, ctime FROM op_app_secret WHERE app_id IN (?" +
		strings.Repeat(",?", len(appIDs)-1) + ")"
	var rows []*secretSummaryRow
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("op_app_secret SummariesByApps: %w", err)
	}
	for _, r := range rows {
		s := out[r.AppID]
		if s == nil {
			s = &SecretSummary{}
			out[r.AppID] = s
		}
		s.Total++
		if r.Status == SecretStatusActive && (r.ExpiresAt == 0 || r.ExpiresAt > now) {
			s.Active++
		}
		if r.Ctime > s.LastCtime {
			s.LastCtime = r.Ctime
		}
	}
	return out, nil
}
