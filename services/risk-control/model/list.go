package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RiskList 名单条目（risk_list 表）。
// target_value 只允许账号 ID 十进制串、设备受控 ID 或 IP 摘要（见 NormalizeTargetValue），
// 禁止写入明文 IP/手机号，保证名单可审计且不成为敏感信息二次存储点。
type RiskList struct {
	ID          int64  `db:"id"`           // 主键 ID
	ListType    int32  `db:"list_type"`    // 1 黑名单、2 白名单
	TargetType  int32  `db:"target_type"`  // 1 mid、2 device_hash、3 ip_hash
	TargetValue string `db:"target_value"` // 目标受控值
	Reason      string `db:"reason"`       // 运营内部说明
	Operator    int64  `db:"operator"`     // 写入人（运营 ID，必填）
	ExpireAt    int64  `db:"expire_at"`    // 到期时间（Unix 秒），0 表示永久
	State       int32  `db:"state"`        // 0 停用、1 生效
	Ctime       int64  `db:"ctime"`        // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 修改时间（Unix 秒）
}

const listColumns = "id, list_type, target_type, target_value, reason, operator, expire_at, state, ctime, mtime"

// Active 判定条目在 now 时刻是否生效。
func (l *RiskList) Active(now int64) bool {
	if l == nil || l.State != StateEnabled {
		return false
	}
	return l.ExpireAt == 0 || l.ExpireAt > now
}

// Validate 校验名单条目字段。
func (l *RiskList) Validate() error {
	if !ValidListType(l.ListType) {
		return fmt.Errorf("%w: list_type=%d", ErrInvalidListEntry, l.ListType)
	}
	if !ValidTargetType(l.TargetType) {
		return fmt.Errorf("%w: target_type=%d", ErrInvalidListEntry, l.TargetType)
	}
	if NormalizeTargetValue(l.TargetType, l.TargetValue) == "" {
		return fmt.Errorf("%w: target_value invalid or raw ip rejected", ErrInvalidListEntry)
	}
	if l.ExpireAt < 0 {
		return fmt.Errorf("%w: expire_at must be >= 0", ErrInvalidListEntry)
	}
	if l.State != StateDisabled && l.State != StateEnabled {
		return fmt.Errorf("%w: state=%d", ErrInvalidListEntry, l.State)
	}
	if l.Operator <= 0 {
		return ErrOperatorRequired
	}
	return nil
}

// RiskListModel risk_list 表读写接口。
type RiskListModel interface {
	// Upsert 按 (list_type, target_type, target_value) 唯一键插入或覆盖；
	// 返回落库后的行与是否新建。
	Upsert(ctx context.Context, l *RiskList) (*RiskList, bool, error)
	// FindOne 按唯一键查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, listType, targetType int32, targetValue string) (*RiskList, error)
	// FindActive 查询 now 时刻生效、且命中给定 (targetType, targetValue) 集合的条目。
	// 单次查询同时取黑/白名单，由调用方按类型分流。
	FindActive(ctx context.Context, now int64, pairs []TargetPair) ([]*RiskList, error)
	// List 分页查询；listType/targetType 为 0、targetValue 为空表示不过滤，state 为 -1 不过滤。
	List(ctx context.Context, listType, targetType int32, targetValue string, state int32, offset, limit int) ([]*RiskList, int32, error)
}

// TargetPair 是名单查询的一个候选目标（类型 + 受控值）。
type TargetPair struct {
	TargetType  int32
	TargetValue string
}

type defaultRiskListModel struct {
	conn sqlx.SqlConn
}

// NewRiskListModel 构造 RiskListModel 实现。
func NewRiskListModel(conn sqlx.SqlConn) RiskListModel {
	return &defaultRiskListModel{conn: conn}
}

func (m *defaultRiskListModel) Upsert(ctx context.Context, l *RiskList) (*RiskList, bool, error) {
	if err := l.Validate(); err != nil {
		return nil, false, err
	}
	now := nowUnix()
	l.Ctime, l.Mtime = now, now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO risk_list (list_type, target_type, target_value, reason, operator, expire_at, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE reason = VALUES(reason), operator = VALUES(operator), expire_at = VALUES(expire_at), state = VALUES(state), mtime = VALUES(mtime)",
		l.ListType, l.TargetType, l.TargetValue, l.Reason, l.Operator, l.ExpireAt, l.State, l.Ctime, l.Mtime)
	if err != nil {
		return nil, false, fmt.Errorf("risk_list Upsert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("risk_list Upsert RowsAffected: %w", err)
	}
	created := aff == 1
	fresh, err := m.FindOne(ctx, l.ListType, l.TargetType, l.TargetValue)
	if err != nil {
		return nil, created, err
	}
	if fresh == nil {
		return nil, created, fmt.Errorf("risk_list Upsert: %w", ErrListEntryNotFound)
	}
	return fresh, created, nil
}

func (m *defaultRiskListModel) FindOne(ctx context.Context, listType, targetType int32, targetValue string) (*RiskList, error) {
	var l RiskList
	query := "SELECT " + listColumns + " FROM risk_list WHERE list_type = ? AND target_type = ? AND target_value = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &l, query, listType, targetType, targetValue); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("risk_list FindOne: %w", err)
	}
	return &l, nil
}

func (m *defaultRiskListModel) FindActive(ctx context.Context, now int64, pairs []TargetPair) ([]*RiskList, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	args := make([]any, 0, len(pairs)*2+1)
	args = append(args, StateEnabled, now)
	for i, p := range pairs {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		sb.WriteString("(target_type = ? AND target_value = ?)")
		args = append(args, p.TargetType, p.TargetValue)
	}
	query := "SELECT " + listColumns + " FROM risk_list WHERE state = ? AND (expire_at = 0 OR expire_at > ?) AND (" + sb.String() + ")"

	var rows []*RiskList
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("risk_list FindActive: %w", err)
	}
	return rows, nil
}

func (m *defaultRiskListModel) List(ctx context.Context, listType, targetType int32, targetValue string, state int32, offset, limit int) ([]*RiskList, int32, error) {
	where := "WHERE 1=1"
	args := make([]any, 0, 4)
	if listType > 0 {
		where += " AND list_type = ?"
		args = append(args, listType)
	}
	if targetType > 0 {
		where += " AND target_type = ?"
		args = append(args, targetType)
	}
	if targetValue != "" {
		where += " AND target_value = ?"
		args = append(args, targetValue)
	}
	if state == StateDisabled || state == StateEnabled {
		where += " AND state = ?"
		args = append(args, state)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM risk_list "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("risk_list List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*RiskList
	query := "SELECT " + listColumns + " FROM risk_list " + where + " ORDER BY id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("risk_list List: %w", err)
	}
	return rows, total, nil
}
