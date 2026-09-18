package model

import (
	"context"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UserMonitor 对应数据库 user_monitor 表，记录受监控用户名单。
// 移植自参考仓库 user_monitor 表：受监控用户的资料变更自动进入审核流程。
type UserMonitor struct {
	// Mid 用户 ID，主键
	Mid int64 `db:"mid"`
	// Operator 添加人
	Operator string `db:"operator"`
	// Remark 备注
	Remark string `db:"remark"`
	// IsDeleted 软删除：0 在监控中、1 已移出
	IsDeleted int8 `db:"is_deleted"`
}

// UserMonitorModel 抽象 user_monitor 表的查询接口。
type UserMonitorModel interface {
	// InMonitor 判断 mid 是否在监控名单。
	InMonitor(ctx context.Context, mid int64) (bool, error)
	// Add 添加/恢复监控（UPSERT，重复添加更新操作人与备注并清除软删除）。
	Add(ctx context.Context, mid int64, operator, remark string) error
}

type defaultUserMonitorModel struct {
	conn sqlx.SqlConn
}

// NewUserMonitorModel 创建基于 sqlx 的 UserMonitorModel 实现。
func NewUserMonitorModel(conn sqlx.SqlConn) UserMonitorModel {
	return &defaultUserMonitorModel{conn: conn}
}

func (m *defaultUserMonitorModel) InMonitor(ctx context.Context, mid int64) (bool, error) {
	var count int
	query := `SELECT COUNT(1) FROM user_monitor WHERE mid = ? AND is_deleted = 0`
	if err := m.conn.QueryRowCtx(ctx, &count, query, mid); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (m *defaultUserMonitorModel) Add(ctx context.Context, mid int64, operator, remark string) error {
	query := `INSERT INTO user_monitor (mid, operator, remark) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE operator = VALUES(operator), remark = VALUES(remark), is_deleted = 0`
	_, err := m.conn.ExecCtx(ctx, query, mid, operator, remark)
	return err
}

// UserPropertyReview 对应数据库 user_property_review 表，记录用户资料属性
// （头像/签名/昵称）变更审核记录。移植自参考仓库 user_property_review 表。
type UserPropertyReview struct {
	// Mid 用户 ID
	Mid int64 `db:"mid"`
	// Old 变更前的值
	Old string `db:"old"`
	// New 变更后的值
	New string `db:"new"`
	// State 审核状态：0 待审核、1 通过、2 驳回、3 已归档、10 自动审核中
	State int8 `db:"state"`
	// Property 审核属性：1 头像、2 签名、3 昵称
	Property int8 `db:"property"`
	// IsMonitor 提交时用户是否在监控名单
	IsMonitor bool `db:"is_monitor"`
	// Extra 审核扩展信息 JSON
	Extra string `db:"extra"`
	// Operator 归档操作人（审核通过/驳回时回填）
	Operator string `db:"operator"`
	// Remark 归档备注（审核通过/驳回时回填）
	Remark string `db:"remark"`
}

// UserPropertyReviewModel 抽象 user_property_review 表的查询接口。
type UserPropertyReviewModel interface {
	// Add 新增一条属性变更审核记录。
	Add(ctx context.Context, review *UserPropertyReview) error
	// Archive 归档：把 mid 同一属性的待审核记录标记为已归档并回填操作人与备注。
	Archive(ctx context.Context, mid int64, property int8, operator, remark string) error
}

type defaultUserPropertyReviewModel struct {
	conn sqlx.SqlConn
}

// NewUserPropertyReviewModel 创建基于 sqlx 的 UserPropertyReviewModel 实现。
func NewUserPropertyReviewModel(conn sqlx.SqlConn) UserPropertyReviewModel {
	return &defaultUserPropertyReviewModel{conn: conn}
}

func (m *defaultUserPropertyReviewModel) Add(ctx context.Context, review *UserPropertyReview) error {
	query := `INSERT INTO user_property_review (mid, old, new, state, property, is_monitor, extra)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := m.conn.ExecCtx(ctx, query, review.Mid, review.Old, review.New, review.State,
		review.Property, review.IsMonitor, review.Extra)
	return err
}

func (m *defaultUserPropertyReviewModel) Archive(ctx context.Context, mid int64, property int8, operator, remark string) error {
	query := `UPDATE user_property_review SET state = ?, operator = ?, remark = ? WHERE mid = ? AND property = ? AND state = 0`
	_, err := m.conn.ExecCtx(ctx, query, ReviewStateArchived, operator, remark, mid, property)
	return err
}
