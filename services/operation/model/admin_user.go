// Package model 定义 operation 服务的数据库实体与查询。
// 本包只持有 operation 自有的表（库 go_video_operation：op_admin_user、op_role、
// op_role_permission、op_admin_role、op_permission、op_menu、op_config、
// op_admin_task、op_admin_task_step、op_audit_index、op_admin_session），
// 不引用 video/catalog/rights/moderation/account 的任何表（AGENTS.md §5）。
package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// adminUserFields 是 op_admin_user 的列清单，查询语句统一引用，避免列序漂移。
const adminUserFields = "admin_id, username, password_hash, pwd_algo, state, fail_count, " +
	"locked_until, last_login_at, remark, two_factor_target, operator, ctime, mtime"

// AdminUser 对应 op_admin_user 表：后台管理员账号。
// 口令只以散列形式落库（password_hash + pwd_algo），任何查询接口都不返回该字段。
type AdminUser struct {
	// AdminID 管理员 ID，主键，自增。
	AdminID int64 `db:"admin_id"`
	// Username 登录账号名，全局唯一。
	Username string `db:"username"`
	// PasswordHash 口令散列，格式 "pbkdf2_sha256$迭代数$盐$派生密钥(hex)"。
	PasswordHash string `db:"password_hash"`
	// PwdAlgo 口令算法标识（便于后续平滑升级算法）。
	PwdAlgo string `db:"pwd_algo"`
	// State 账号状态：1 正常、2 禁用、3 锁定，参见 AdminState*。
	State int32 `db:"state"`
	// FailCount 连续登录失败次数，成功后清零（防爆破）。
	FailCount int32 `db:"fail_count"`
	// LockedUntil 锁定截止时间（Unix 秒），0 表示未锁定。
	LockedUntil int64 `db:"locked_until"`
	// LastLoginAt 最近一次成功登录时间（Unix 秒）。
	LastLoginAt int64 `db:"last_login_at"`
	// Remark 备注（岗位、责任范围）。
	Remark string `db:"remark"`
	// TwoFactorTarget 二次校验目标（手机号）。空表示该账号未启用二次校验；
	// 任何出参都不返回该字段，只返回“是否启用”布尔值（隐私最小化）。
	TwoFactorTarget string `db:"two_factor_target"`
	// Operator 最后修改该账号的管理员 ID（0 表示系统初始化）。
	Operator int64 `db:"operator"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// AdminUserModel 抽象 op_admin_user 表，便于测试替换。
type AdminUserModel interface {
	// Insert 新建管理员账号，返回自增 admin_id。
	Insert(ctx context.Context, u *AdminUser) (int64, error)
	// FindOne 按 admin_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, adminID int64) (*AdminUser, error)
	// FindByUsername 按账号名查询；不存在返回 (nil, nil)。
	FindByUsername(ctx context.Context, username string) (*AdminUser, error)
	// FindMany 批量查询，返回 admin_id → 账号 的映射。
	FindMany(ctx context.Context, adminIDs []int64) (map[int64]*AdminUser, error)
	// UpdateProfile 按补丁更新账号字段（nil 字段不修改），返回是否有行被更新。
	UpdateProfile(ctx context.Context, adminID int64, patch *AdminUserPatch) (bool, error)
	// UpdatePassword 更新口令散列并清零失败计数、解除锁定。
	UpdatePassword(ctx context.Context, adminID int64, hash, algo string, operator int64) error
	// UpdateLoginGuard 更新失败计数/锁定时间/状态（防爆破写回）。
	UpdateLoginGuard(ctx context.Context, adminID int64, failCount int32, lockedUntil int64, state int32) error
	// TouchLogin 记录登录成功时间并清零失败计数。
	TouchLogin(ctx context.Context, adminID int64, at int64) error
	// List 分页查询账号：state=0 表示不过滤，keyword 为用户名模糊匹配。
	List(ctx context.Context, state int32, keyword string, pn, ps int32) ([]*AdminUser, int64, error)
	// CountByUsername 统计同名账号数（用于唯一性预检）。
	CountByUsername(ctx context.Context, username string) (int64, error)
}

type defaultAdminUserModel struct {
	conn sqlx.SqlConn
}

// NewAdminUserModel 构造 op_admin_user 的 sqlx 实现。
func NewAdminUserModel(conn sqlx.SqlConn) AdminUserModel {
	return &defaultAdminUserModel{conn: conn}
}

func (m *defaultAdminUserModel) Insert(ctx context.Context, u *AdminUser) (int64, error) {
	now := nowUnix()
	if u.Ctime == 0 {
		u.Ctime = now
	}
	u.Mtime = now
	// uniq_username 命中时 ON DUPLICATE KEY UPDATE 为刻意空更新，
	// RowsAffected == 0 即判定为重名，不依赖 driver 专有错误类型。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_admin_user ("+adminUserFields+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		u.AdminID, u.Username, u.PasswordHash, u.PwdAlgo, u.State, u.FailCount,
		u.LockedUntil, u.LastLoginAt, u.Remark, u.TwoFactorTarget, u.Operator, u.Ctime, u.Mtime)
	if err != nil {
		return 0, fmt.Errorf("op_admin_user Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_admin_user Insert RowsAffected: %w", err)
	}
	if aff == 0 {
		return 0, ErrAdminExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("op_admin_user Insert LastInsertId: %w", err)
	}
	u.AdminID = id
	return id, nil
}

func (m *defaultAdminUserModel) FindOne(ctx context.Context, adminID int64) (*AdminUser, error) {
	var u AdminUser
	query := "SELECT " + adminUserFields + " FROM op_admin_user WHERE admin_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &u, query, adminID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_user FindOne: %w", err)
	}
	return &u, nil
}

func (m *defaultAdminUserModel) FindByUsername(ctx context.Context, username string) (*AdminUser, error) {
	var u AdminUser
	query := "SELECT " + adminUserFields + " FROM op_admin_user WHERE username = ?"
	if err := m.conn.QueryRowCtx(ctx, &u, query, username); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("op_admin_user FindByUsername: %w", err)
	}
	return &u, nil
}

func (m *defaultAdminUserModel) FindMany(ctx context.Context, adminIDs []int64) (map[int64]*AdminUser, error) {
	result := make(map[int64]*AdminUser, len(adminIDs))
	if len(adminIDs) == 0 {
		return result, nil
	}
	args := make([]any, 0, len(adminIDs))
	for _, id := range adminIDs {
		args = append(args, id)
	}
	query := "SELECT " + adminUserFields + " FROM op_admin_user WHERE admin_id IN (" + placeholders(len(adminIDs)) + ")"
	var rows []*AdminUser
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if err == sql.ErrNoRows {
			return result, nil
		}
		return nil, fmt.Errorf("op_admin_user FindMany: %w", err)
	}
	for _, r := range rows {
		result[r.AdminID] = r
	}
	return result, nil
}

// AdminUserPatch 是 op_admin_user 的局部更新补丁：nil 字段表示不修改。
// 用指针区分“没传”和“传了空串”，否则无法清空备注或关闭二次校验。
type AdminUserPatch struct {
	Remark          *string
	TwoFactorTarget *string
	State           *int32
	Operator        int64
}

func (m *defaultAdminUserModel) UpdateProfile(ctx context.Context, adminID int64, patch *AdminUserPatch) (bool, error) {
	if patch == nil {
		return false, nil
	}
	set := []string{"mtime = ?"}
	args := []any{nowUnix()}
	if patch.Remark != nil {
		set = append(set, "remark = ?")
		args = append(args, *patch.Remark)
	}
	if patch.TwoFactorTarget != nil {
		set = append(set, "two_factor_target = ?")
		args = append(args, *patch.TwoFactorTarget)
	}
	if patch.State != nil {
		set = append(set, "state = ?")
		args = append(args, *patch.State)
		// 人工恢复为正常状态时同时清除失败计数与锁定，避免运营改完仍登不进。
		if *patch.State == AdminStateNormal {
			set = append(set, "fail_count = 0", "locked_until = 0")
		}
	}
	set = append(set, "operator = ?")
	args = append(args, patch.Operator)
	args = append(args, adminID)

	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_admin_user SET "+strings.Join(set, ", ")+" WHERE admin_id = ?", args...)
	if err != nil {
		return false, fmt.Errorf("op_admin_user UpdateProfile: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("op_admin_user UpdateProfile RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultAdminUserModel) UpdatePassword(ctx context.Context, adminID int64, hash, algo string, operator int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE op_admin_user SET password_hash = ?, pwd_algo = ?, operator = ?, mtime = ?, fail_count = 0, locked_until = 0 WHERE admin_id = ?",
		hash, algo, operator, nowUnix(), adminID)
	return err
}

func (m *defaultAdminUserModel) UpdateLoginGuard(ctx context.Context, adminID int64, failCount int32, lockedUntil int64, state int32) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE op_admin_user SET fail_count = ?, locked_until = ?, state = ?, mtime = ? WHERE admin_id = ?",
		failCount, lockedUntil, state, nowUnix(), adminID)
	return err
}

func (m *defaultAdminUserModel) TouchLogin(ctx context.Context, adminID int64, at int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE op_admin_user SET last_login_at = ?, fail_count = 0, locked_until = 0, state = ?, mtime = ? WHERE admin_id = ?",
		at, AdminStateNormal, nowUnix(), adminID)
	return err
}

func (m *defaultAdminUserModel) List(ctx context.Context, state int32, keyword string, pn, ps int32) ([]*AdminUser, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 3)
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}
	if keyword != "" {
		where += " AND username LIKE ?"
		args = append(args, "%"+keyword+"%")
	}

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM op_admin_user "+where, args...); err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("op_admin_user List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	query := "SELECT " + adminUserFields + " FROM op_admin_user " + where + " ORDER BY admin_id DESC LIMIT ? OFFSET ?"
	var rows []*AdminUser
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if err == sql.ErrNoRows {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("op_admin_user List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultAdminUserModel) CountByUsername(ctx context.Context, username string) (int64, error) {
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, "SELECT COUNT(*) FROM op_admin_user WHERE username = ?", username); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("op_admin_user CountByUsername: %w", err)
	}
	return n, nil
}
