package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// retentionColumns 是 audit_retention_policy 的列清单。
const retentionColumns = "policy_id, action_domain, hot_days, archive_after_days, delete_after_days," +
	" state, version, operator, remark, ctime, mtime"

// RetentionPolicy 对应 audit_retention_policy 表：按动作域的保留期策略。
//
// 三个天数的关系必须满足 0 < archive_after_days <= hot_days，
// 且 delete_after_days == 0（永久保留）或 delete_after_days >= archive_after_days，
// 否则会出现「还没归档就要删」或「归档线晚于热表清理线」的自相矛盾配置。
// 校验在 model.CanPolicyDays 里做单点实现，repository 与 logic 共用。
type RetentionPolicy struct {
	// PolicyID 自增主键。
	PolicyID int64 `db:"policy_id"`
	// ActionDomain 动作域（唯一键）。"default" 是未匹配域时的回退策略。
	ActionDomain string `db:"action_domain"`
	// HotDays 热表目标保留天数（在线查询覆盖的窗口）。
	HotDays int32 `db:"hot_days"`
	// ArchiveAfterDays 超过该天数的条目可被归档作业处理。
	ArchiveAfterDays int32 `db:"archive_after_days"`
	// DeleteAfterDays 超过该天数**且已有校验通过的归档清单**才允许物理清理；
	// 0 表示永久保留（默认，宁可占盘不可丢证）。
	DeleteAfterDays int32 `db:"delete_after_days"`
	// State 1 启用、2 停用。停用后归档作业跳过该域。
	State int32 `db:"state"`
	// Version 乐观锁版本，SaveRetentionPolicy 的 expect_version 依据。
	Version int64 `db:"version"`
	// Operator 最后修改人 admin_id（引用 operation，不复制）。
	Operator int64 `db:"operator"`
	// Remark 变更说明。
	Remark string `db:"remark"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// RetentionPolicyModel 抽象 audit_retention_policy 表。
type RetentionPolicyModel interface {
	// Insert 新建策略；action_domain 冲突时返回 ErrPolicyExists。
	Insert(ctx context.Context, p *RetentionPolicy) (int64, error)
	// FindOne 按动作域查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, actionDomain string) (*RetentionPolicy, error)
	// FindEffective 取生效策略：优先精确匹配 action_domain，其次 "default"。
	FindEffective(ctx context.Context, actionDomain string) (*RetentionPolicy, error)
	// UpdateWithVersion 乐观锁更新：仅当库中 version == expectVersion 时写入并把 version 置为 +1。
	// 未命中时 updated=false 且不报错，由 repository 转成 ErrPolicyVersionConflict。
	UpdateWithVersion(ctx context.Context, p *RetentionPolicy, expectVersion int64) (bool, error)
	// List 分页查询；state 为 0 时返回全部。
	List(ctx context.Context, state int32, pn, ps int32) ([]*RetentionPolicy, int64, error)
}

type defaultRetentionPolicyModel struct {
	conn sqlx.SqlConn
}

// NewRetentionPolicyModel 构造 audit_retention_policy 的 sqlx 实现。
func NewRetentionPolicyModel(conn sqlx.SqlConn) RetentionPolicyModel {
	return &defaultRetentionPolicyModel{conn: conn}
}

func (m *defaultRetentionPolicyModel) Insert(ctx context.Context, p *RetentionPolicy) (int64, error) {
	if p.ActionDomain == "" {
		return 0, ErrActionRequired
	}
	if !CanPolicyDays(p.HotDays, p.ArchiveAfterDays, p.DeleteAfterDays) {
		return 0, ErrPolicyDaysInvalid
	}
	if p.Ctime == 0 {
		p.Ctime = nowUnix()
	}
	if p.State == 0 {
		p.State = StateEnable
	}
	p.Version = 1
	// 依赖 uniq_action_domain：冲突时 mtime 自等 → RowsAffected == 0 → ErrPolicyExists，
	// 不依赖驱动专有错误码（AGENTS.md §5 幂等写入要求）。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO audit_retention_policy ("+retentionColumns+") VALUES ("+placeholders(11)+")"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		p.PolicyID, p.ActionDomain, p.HotDays, p.ArchiveAfterDays, p.DeleteAfterDays,
		p.State, p.Version, p.Operator, p.Remark, p.Ctime, p.Ctime)
	if err != nil {
		return 0, fmt.Errorf("audit_retention_policy Insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("audit_retention_policy Insert RowsAffected: %w", err)
	}
	if n == 0 {
		return 0, ErrPolicyExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("audit_retention_policy Insert LastInsertId: %w", err)
	}
	p.PolicyID = id
	return id, nil
}

const retentionSelect = "SELECT " + retentionColumns + " FROM audit_retention_policy"

func (m *defaultRetentionPolicyModel) FindOne(ctx context.Context, actionDomain string) (*RetentionPolicy, error) {
	if actionDomain == "" {
		return nil, nil
	}
	var row RetentionPolicy
	err := m.conn.QueryRowCtx(ctx, &row, retentionSelect+" WHERE action_domain = ? LIMIT 1", actionDomain)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit_retention_policy FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultRetentionPolicyModel) FindEffective(ctx context.Context, actionDomain string) (*RetentionPolicy, error) {
	p, err := m.FindOne(ctx, actionDomain)
	if err != nil {
		return nil, err
	}
	if p != nil && p.State == StateEnable {
		return p, nil
	}
	fallback, err := m.FindOne(ctx, DefaultPolicyDomain)
	if err != nil {
		return nil, err
	}
	if fallback == nil {
		return nil, ErrPolicyNotFound
	}
	return fallback, nil
}

func (m *defaultRetentionPolicyModel) UpdateWithVersion(ctx context.Context, p *RetentionPolicy,
	expectVersion int64) (bool, error) {
	if !CanPolicyDays(p.HotDays, p.ArchiveAfterDays, p.DeleteAfterDays) {
		return false, ErrPolicyDaysInvalid
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE audit_retention_policy SET hot_days = ?, archive_after_days = ?, delete_after_days = ?,"+
			" state = ?, version = version + 1, operator = ?, remark = ?, mtime = ?"+
			" WHERE action_domain = ? AND version = ?",
		p.HotDays, p.ArchiveAfterDays, p.DeleteAfterDays, p.State, p.Operator, p.Remark, nowUnix(),
		p.ActionDomain, expectVersion)
	if err != nil {
		return false, fmt.Errorf("audit_retention_policy UpdateWithVersion: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("audit_retention_policy UpdateWithVersion RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultRetentionPolicyModel) List(ctx context.Context, state int32, pn, ps int32) ([]*RetentionPolicy, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 3)
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM audit_retention_policy "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("audit_retention_policy List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*RetentionPolicy
	query := retentionSelect + where + " ORDER BY action_domain ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("audit_retention_policy List: %w", err)
	}
	return rows, total, nil
}

// CanPolicyDays 校验三个天数的自洽性。
// hot_days 允许 0（表示该域不设热表窗口，全部即时可归档），
// 但一旦 > 0 就必须不小于 archive_after_days。
func CanPolicyDays(hotDays, archiveAfter, deleteAfter int32) bool {
	if archiveAfter <= 0 {
		return false
	}
	if hotDays > 0 && hotDays < archiveAfter {
		return false
	}
	if deleteAfter < 0 {
		return false
	}
	return deleteAfter == 0 || deleteAfter >= archiveAfter
}
