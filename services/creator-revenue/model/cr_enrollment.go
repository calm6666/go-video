package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 参与状态常量，与 cr_enrollment.state 列和 rpc.EnrollmentState 取值严格一致。
const (
	// EnrollmentStateUnspecified 未指定（查询语境表示「不过滤」；也用于「从未参加」的投影）。
	EnrollmentStateUnspecified int32 = 0
	// EnrollmentStateEnrolled 在计划内，产生收益计量与结算。
	EnrollmentStateEnrolled int32 = 1
	// EnrollmentStateLeft 已退出；历史台账保留，不再出单。
	EnrollmentStateLeft int32 = 2
	// EnrollmentStateSuspended 违规暂停：收益不结算（出单判定必须排除）。
	EnrollmentStateSuspended int32 = 3
)

// enrollmentColumns 是 cr_enrollment 的完整列清单，
// 与 deploy/migrations/creator-revenue/000001_create_creator_revenue_tables.sql 一一对应。
const enrollmentColumns = "enrollment_id, mid, state, agreed_rule_version, enrolled_at, left_at, " +
	"operator, remark, ctime, mtime"

// Enrollment 参与关系行（一人一行，mid 唯一）。
//
// agreed_rule_version 是「参与者确认时看到的规则版本」，必须可回溯：
// 它是 cr_revenue_rule.version 的快照值，事后争议靠 cr_rule_change_log 还原当时的单价口径。
type Enrollment struct {
	EnrollmentId      int64  `db:"enrollment_id"`
	Mid               int64  `db:"mid"`
	State             int32  `db:"state"`
	AgreedRuleVersion int64  `db:"agreed_rule_version"`
	EnrolledAt        int64  `db:"enrolled_at"`
	LeftAt            int64  `db:"left_at"`
	Operator          string `db:"operator"`
	Remark            string `db:"remark"`
	Ctime             int64  `db:"ctime"`
	Mtime             int64  `db:"mtime"`
}

// EnrollmentModel cr_enrollment 表读写接口。
type EnrollmentModel interface {
	// Insert 首次参加时建行（uniq_mid 命中返回可被 IsDuplicateErr 识别的错误）。
	Insert(ctx context.Context, e *Enrollment) (int64, error)
	// FindOne 按 mid 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, mid int64) (*Enrollment, error)
	// LockByMid 按 mid 加行锁读取（状态迁移前定位，需构造在事务会话上）。
	LockByMid(ctx context.Context, mid int64) (*Enrollment, error)
	// Transition 以 fromState 为条件推进状态（CAS）。
	//
	// enrolledAt/leftAt 语义：传 0 表示「保持原值」，传正值表示写入该时间戳，
	// 传 -1 表示清空该列为 0（重新参加时清掉上一次 left_at）。
	// 返回 false 表示状态已被并发修改，调用方需重新读取。
	Transition(
		ctx context.Context, mid int64, fromState, toState int32,
		agreedRuleVersion int64, enrolledAt, leftAt int64, operator, remark string,
	) (bool, error)
	// UpdateAgreedVersion 在已 ENROLLED 的情况下前移确认的规则版本（创作者重新确认新条款）。
	// 带 state=ENROLLED 守卫；返回 false 表示状态已变。
	UpdateAgreedVersion(ctx context.Context, mid, agreedRuleVersion int64, operator string) (bool, error)
	// List 按状态分页列出（state=0 不过滤），按 mid 升序稳定遍历。
	List(ctx context.Context, state int32, offset, limit int64) ([]*Enrollment, error)
	// Count 同 List 条件的总数。
	Count(ctx context.Context, state int32) (int64, error)
}

type defaultEnrollmentModel struct {
	conn sqlx.SqlConn
}

// NewEnrollmentModel 创建 cr_enrollment 的数据访问对象。
func NewEnrollmentModel(conn sqlx.SqlConn) EnrollmentModel {
	return &defaultEnrollmentModel{conn: conn}
}

func (m *defaultEnrollmentModel) Insert(ctx context.Context, e *Enrollment) (int64, error) {
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO cr_enrollment (mid, state, agreed_rule_version, enrolled_at, left_at, "+
			"operator, remark, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		e.Mid, e.State, e.AgreedRuleVersion, e.EnrolledAt, e.LeftAt, e.Operator, e.Remark, now, now)
	if err != nil {
		return 0, fmt.Errorf("cr_enrollment Insert(%d): %w", e.Mid, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("cr_enrollment LastInsertId(%d): %w", e.Mid, err)
	}
	return id, nil
}

func (m *defaultEnrollmentModel) FindOne(ctx context.Context, mid int64) (*Enrollment, error) {
	var e Enrollment
	query := "SELECT " + enrollmentColumns + " FROM cr_enrollment WHERE mid = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &e, query, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_enrollment FindOne(%d): %w", mid, err)
	}
	return &e, nil
}

func (m *defaultEnrollmentModel) LockByMid(ctx context.Context, mid int64) (*Enrollment, error) {
	var e Enrollment
	query := "SELECT " + enrollmentColumns + " FROM cr_enrollment WHERE mid = ? LIMIT 1 FOR UPDATE"
	if err := m.conn.QueryRowCtx(ctx, &e, query, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_enrollment LockByMid(%d): %w", mid, err)
	}
	return &e, nil
}

func (m *defaultEnrollmentModel) Transition(
	ctx context.Context, mid int64, fromState, toState int32,
	agreedRuleVersion int64, enrolledAt, leftAt int64, operator, remark string,
) (bool, error) {
	// 时间戳三态：0 保持、>0 写入、-1 清零。用 IF 表达，避免 logic 先读后写造成丢更新。
	const query = "UPDATE cr_enrollment SET state = ?, " +
		"agreed_rule_version = IF(? > 0, ?, agreed_rule_version), " +
		"enrolled_at = IF(? < 0, 0, IF(? > 0, ?, enrolled_at)), " +
		"left_at = IF(? < 0, 0, IF(? > 0, ?, left_at)), " +
		"operator = ?, remark = ?, mtime = ? " +
		"WHERE mid = ? AND state = ?"
	res, err := m.conn.ExecCtx(ctx, query,
		toState,
		agreedRuleVersion, agreedRuleVersion,
		enrolledAt, enrolledAt, enrolledAt,
		leftAt, leftAt, leftAt,
		operator, remark, nowUnix(), mid, fromState)
	if err != nil {
		return false, fmt.Errorf("cr_enrollment Transition(%d %d->%d): %w", mid, fromState, toState, err)
	}
	return rowsAffected(res)
}

func (m *defaultEnrollmentModel) UpdateAgreedVersion(
	ctx context.Context, mid, agreedRuleVersion int64, operator string,
) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cr_enrollment SET agreed_rule_version = ?, operator = ?, mtime = ? "+
			"WHERE mid = ? AND state = ? AND agreed_rule_version < ?",
		agreedRuleVersion, operator, nowUnix(), mid, EnrollmentStateEnrolled, agreedRuleVersion)
	if err != nil {
		return false, fmt.Errorf("cr_enrollment UpdateAgreedVersion(%d): %w", mid, err)
	}
	return rowsAffected(res)
}

func (m *defaultEnrollmentModel) List(
	ctx context.Context, state int32, offset, limit int64,
) ([]*Enrollment, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []*Enrollment
	where, args := "1 = 1", []any{}
	if state != EnrollmentStateUnspecified {
		where, args = "state = ?", []any{state}
	}
	query := "SELECT " + enrollmentColumns + " FROM cr_enrollment WHERE " + where +
		" ORDER BY mid ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_enrollment List(state=%d): %w", state, err)
	}
	return rows, nil
}

func (m *defaultEnrollmentModel) Count(ctx context.Context, state int32) (int64, error) {
	var total int64
	query := "SELECT COUNT(*) FROM cr_enrollment"
	args := []any{}
	if state != EnrollmentStateUnspecified {
		query += " WHERE state = ?"
		args = append(args, state)
	}
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cr_enrollment Count(state=%d): %w", state, err)
	}
	return total, nil
}
