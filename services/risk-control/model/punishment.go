package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RiskPunishment 处罚记录（risk_punishment 表）。
// 状态机：ACTIVE → LIFTED（运营解除）／EXPIRED（到期，读时惰性推进 + cron 归档）。
// 幂等：idempotency_key 唯一索引；同一 (mid, scope) 同时只允许一条 ACTIVE，
// 由 ApplyPunishment 在事务外先查后写 + 唯一索引兜底，重叠处罚会让裁决无法解释。
type RiskPunishment struct {
	PunishmentID   int64  `db:"punishment_id"`   // 处罚 ID
	Mid            int64  `db:"mid"`             // 被处罚账号
	Scope          int32  `db:"scope"`           // 生效动作范围，0 表示全域
	Decision       int32  `db:"decision"`        // 生效裁决（CHALLENGE/BLOCK/REVIEW）
	Reason         string `db:"reason"`          // 运营内部说明，禁止下发终端
	ReasonCode     string `db:"reason_code"`     // 面向端的稳定原因码
	Operator       int64  `db:"operator"`        // 下发人（运营 ID）
	StartAt        int64  `db:"start_at"`        // 生效时间（Unix 秒）
	EndAt          int64  `db:"end_at"`          // 到期时间（Unix 秒），0 表示永久
	State          int32  `db:"state"`           // 1 生效、2 已解除、3 已过期
	IdempotencyKey string `db:"idempotency_key"` // 幂等键（唯一）
	LiftOperator   int64  `db:"lift_operator"`   // 解除人（0 表示未解除或系统过期）
	LiftReason     string `db:"lift_reason"`     // 解除说明
	Ctime          int64  `db:"ctime"`           // 创建时间（Unix 秒）
	Mtime          int64  `db:"mtime"`           // 修改时间（Unix 秒）
}

const punishmentColumns = "punishment_id, mid, scope, decision, reason, reason_code, operator, " +
	"start_at, end_at, state, idempotency_key, lift_operator, lift_reason, ctime, mtime"

// Effective 判定在 now 时刻处罚是否仍在生效。
// end_at == 0 表示永久；start_at 允许运营设定未来生效（时间边界必须可单测）。
func (p *RiskPunishment) Effective(now int64) bool {
	if p == nil || p.State != PunishmentStateActive {
		return false
	}
	if p.StartAt > now {
		return false
	}
	return p.EndAt == 0 || p.EndAt > now
}

// RemainingSeconds 返回剩余处罚秒数；永久或已结束时为 0。
func (p *RiskPunishment) RemainingSeconds(now int64) int64 {
	if !p.Effective(now) || p.EndAt == 0 {
		return 0
	}
	return p.EndAt - now
}

// Covers 判定处罚范围是否覆盖给定动作：scope 为 0（全域）覆盖所有动作。
func (p *RiskPunishment) Covers(action int32) bool {
	if p == nil {
		return false
	}
	return p.Scope == ActionAll || p.Scope == action
}

// MatchesScope 判定处罚是否正好落在 (mid, scope) 这一维度上。
// 用于运营解除：解除某个动作的处罚绝不能顺带解除全域处罚，
// 因此这里按 scope 精确匹配，不做「全域覆盖具体动作」的宽松判断。
func (p *RiskPunishment) MatchesScope(mid int64, scope int32) bool {
	return p != nil && p.Mid == mid && p.Scope == scope
}

// RiskPunishmentModel risk_punishment 表读写接口。
type RiskPunishmentModel interface {
	// Insert 新建处罚；idempotency_key 冲突时返回既有记录 ID 且 created=false。
	Insert(ctx context.Context, p *RiskPunishment) (id int64, created bool, err error)
	// FindOne 按主键查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, punishmentID int64) (*RiskPunishment, error)
	// FindByIDempotencyKey 按幂等键查询；不存在返回 (nil, nil)。
	FindByIDempotencyKey(ctx context.Context, key string) (*RiskPunishment, error)
	// ListActiveByMid 查询某账号当前生效中的处罚（含全域处罚）。
	ListActiveByMid(ctx context.Context, mid, now int64) ([]*RiskPunishment, error)
	// Lift 生效 → 已解除；非 ACTIVE 时返回 ErrPunishmentAlreadyFinished。
	Lift(ctx context.Context, punishmentID, operator int64, reason string) error
	// ExpireStale 把该账号已到期的 ACTIVE 处罚推进为 EXPIRED，返回推进条数。
	ExpireStale(ctx context.Context, mid, now int64) (int64, error)
	// List 分页查询；mid=0、scope=ActionAll、state=0 表示对应维度不过滤，
	// onlyActive 为真时叠加「当前时间生效中」条件。
	List(ctx context.Context, mid int64, scope, state int32, onlyActive bool, now int64, offset, limit int) ([]*RiskPunishment, int32, error)
}

type defaultRiskPunishmentModel struct {
	conn sqlx.SqlConn
}

// NewRiskPunishmentModel 构造 RiskPunishmentModel 实现。
func NewRiskPunishmentModel(conn sqlx.SqlConn) RiskPunishmentModel {
	return &defaultRiskPunishmentModel{conn: conn}
}

func (m *defaultRiskPunishmentModel) Insert(ctx context.Context, p *RiskPunishment) (int64, bool, error) {
	now := nowUnix()
	p.Ctime, p.Mtime = now, now
	// idempotency_key 唯一索引 + ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)：
	// 重复下发返回既有行 ID，不新增第二条处罚。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO risk_punishment (mid, scope, decision, reason, reason_code, operator, start_at, end_at, state, idempotency_key, lift_operator, lift_reason, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE punishment_id = LAST_INSERT_ID(punishment_id)",
		p.Mid, p.Scope, p.Decision, p.Reason, p.ReasonCode, p.Operator, p.StartAt, p.EndAt,
		p.State, p.IdempotencyKey, p.LiftOperator, p.LiftReason, p.Ctime, p.Mtime)
	if err != nil {
		return 0, false, fmt.Errorf("risk_punishment Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("risk_punishment Insert LastInsertId: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("risk_punishment Insert RowsAffected: %w", err)
	}
	p.PunishmentID = id
	// MySQL 语义：1 = 新插入，0 = 命中唯一键且无字段变化（本语句恒为 0 或 1）。
	return id, aff == 1, nil
}

func (m *defaultRiskPunishmentModel) FindOne(ctx context.Context, punishmentID int64) (*RiskPunishment, error) {
	var p RiskPunishment
	query := "SELECT " + punishmentColumns + " FROM risk_punishment WHERE punishment_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &p, query, punishmentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("risk_punishment FindOne: %w", err)
	}
	return &p, nil
}

func (m *defaultRiskPunishmentModel) FindByIDempotencyKey(ctx context.Context, key string) (*RiskPunishment, error) {
	var p RiskPunishment
	query := "SELECT " + punishmentColumns + " FROM risk_punishment WHERE idempotency_key = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &p, query, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("risk_punishment FindByIDempotencyKey: %w", err)
	}
	return &p, nil
}

func (m *defaultRiskPunishmentModel) ListActiveByMid(ctx context.Context, mid, now int64) ([]*RiskPunishment, error) {
	var rows []*RiskPunishment
	query := "SELECT " + punishmentColumns + " FROM risk_punishment " +
		"WHERE mid = ? AND state = ? AND start_at <= ? AND (end_at = 0 OR end_at > ?) ORDER BY scope ASC, punishment_id DESC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, PunishmentStateActive, now, now); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("risk_punishment ListActiveByMid: %w", err)
	}
	return rows, nil
}

func (m *defaultRiskPunishmentModel) Lift(ctx context.Context, punishmentID, operator int64, reason string) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE risk_punishment SET state = ?, lift_operator = ?, lift_reason = ?, mtime = ? WHERE punishment_id = ? AND state = ?",
		PunishmentStateLifted, operator, reason, nowUnix(), punishmentID, PunishmentStateActive)
	if err != nil {
		return fmt.Errorf("risk_punishment Lift: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("risk_punishment Lift RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrPunishmentAlreadyFinished
	}
	return nil
}

func (m *defaultRiskPunishmentModel) ExpireStale(ctx context.Context, mid, now int64) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE risk_punishment SET state = ?, mtime = ? WHERE mid = ? AND state = ? AND end_at > 0 AND end_at <= ?",
		PunishmentStateExpired, now, mid, PunishmentStateActive, now)
	if err != nil {
		return 0, fmt.Errorf("risk_punishment ExpireStale: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("risk_punishment ExpireStale RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultRiskPunishmentModel) List(ctx context.Context, mid int64, scope, state int32, onlyActive bool, now int64, offset, limit int) ([]*RiskPunishment, int32, error) {
	where := "WHERE 1=1"
	args := make([]any, 0, 4)
	if mid > 0 {
		where += " AND mid = ?"
		args = append(args, mid)
	}
	if scope > ActionAll {
		where += " AND scope = ?"
		args = append(args, scope)
	}
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}
	if onlyActive {
		where += " AND state = ? AND start_at <= ? AND (end_at = 0 OR end_at > ?)"
		args = append(args, PunishmentStateActive, now, now)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM risk_punishment "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("risk_punishment List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*RiskPunishment
	query := "SELECT " + punishmentColumns + " FROM risk_punishment " + where + " ORDER BY punishment_id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("risk_punishment List: %w", err)
	}
	return rows, total, nil
}
