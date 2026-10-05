package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// planChangeLogColumns 是 mb_plan_change_log 的完整列清单，
// 与 deploy/migrations/membership/000001_create_membership_tables.sql 一一对应。
const planChangeLogColumns = "log_id, plan_id, change_type, from_state, to_state, from_price_minor, " +
	"to_price_minor, from_prom_price_minor, to_prom_price_minor, operator, reason, request_id, " +
	"params_fingerprint, ctime"

// PlanChangeLog 套餐变更台账（上下架 / 改价必须可回溯到人和理由）。
//
// request_id 上有唯一索引，既是写接口幂等键，也是「同 request_id 换参数」的判定入口：
// 命中冲突时返回 ErrRequestIdReused，而不是按新参数再改一次。
type PlanChangeLog struct {
	LogID              int64  `db:"log_id"`                // 自增主键
	PlanID             int64  `db:"plan_id"`               // 套餐 ID
	ChangeType         string `db:"change_type"`           // UPSERT / STATE
	FromState          int32  `db:"from_state"`            // 变更前状态
	ToState            int32  `db:"to_state"`              // 变更后状态
	FromPriceMinor     int64  `db:"from_price_minor"`      // 变更前原价（分）
	ToPriceMinor       int64  `db:"to_price_minor"`        // 变更后原价（分）
	FromPromPriceMinor int64  `db:"from_prom_price_minor"` // 变更前促销价（分）
	ToPromPriceMinor   int64  `db:"to_prom_price_minor"`   // 变更后促销价（分）
	Operator           string `db:"operator"`              // 操作者（网关按会话渲染）
	Reason             string `db:"reason"`                // 变更理由（不得写入凭据）
	RequestID          string `db:"request_id"`            // 幂等键（唯一）
	ParamsFingerprint  string `db:"params_fingerprint"`    // 关键参数指纹
	Ctime              int64  `db:"ctime"`                 // 创建时间（Unix 秒）
}

// PlanChangeLogModel mb_plan_change_log 表接口。
type PlanChangeLogModel interface {
	// InsertTx 写入台账；request_id 冲突返回可被 IsDuplicate 识别的错误。
	InsertTx(ctx context.Context, session sqlx.Session, l *PlanChangeLog) (int64, error)
	// FindByRequestID 幂等重放回查；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*PlanChangeLog, error)
	// ListByPlan 按时间倒序查某套餐的变更台账（运营审计页）。
	ListByPlan(ctx context.Context, planID int64, limit int32) ([]*PlanChangeLog, error)
	// IsDuplicate 暴露唯一索引冲突判定，供 logic 区分「重放」与「真的写失败」。
	IsDuplicate(err error) bool
}

type defaultPlanChangeLogModel struct {
	conn sqlx.SqlConn
}

// NewPlanChangeLogModel 创建 PlanChangeLogModel 实现。
func NewPlanChangeLogModel(conn sqlx.SqlConn) PlanChangeLogModel {
	return &defaultPlanChangeLogModel{conn: conn}
}

func (m *defaultPlanChangeLogModel) InsertTx(ctx context.Context, session sqlx.Session, l *PlanChangeLog) (int64, error) {
	const query = "INSERT INTO mb_plan_change_log (plan_id, change_type, from_state, to_state, from_price_minor, " +
		"to_price_minor, from_prom_price_minor, to_prom_price_minor, operator, reason, request_id, " +
		"params_fingerprint, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	if l.Ctime == 0 {
		l.Ctime = nowUnix()
	}
	res, err := execerOr(session, m.conn).ExecCtx(ctx, query,
		l.PlanID, l.ChangeType, l.FromState, l.ToState, l.FromPriceMinor, l.ToPriceMinor,
		l.FromPromPriceMinor, l.ToPromPriceMinor, l.Operator, l.Reason, l.RequestID,
		l.ParamsFingerprint, l.Ctime)
	if err != nil {
		return 0, fmt.Errorf("mb_plan_change_log Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("mb_plan_change_log Insert LastInsertId: %w", err)
	}
	l.LogID = id
	return id, nil
}

func (m *defaultPlanChangeLogModel) FindByRequestID(ctx context.Context, requestID string) (*PlanChangeLog, error) {
	var l PlanChangeLog
	query := "SELECT " + planChangeLogColumns + " FROM mb_plan_change_log WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &l, query, requestID); err != nil {
		return noRowsAsNil(&l, err, "mb_plan_change_log FindByRequestID")
	}
	return &l, nil
}

func (m *defaultPlanChangeLogModel) ListByPlan(ctx context.Context, planID int64, limit int32) ([]*PlanChangeLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []*PlanChangeLog
	query := "SELECT " + planChangeLogColumns + " FROM mb_plan_change_log WHERE plan_id = ? ORDER BY log_id DESC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, planID, limit); err != nil {
		return nil, fmt.Errorf("mb_plan_change_log ListByPlan: %w", err)
	}
	return rows, nil
}

func (m *defaultPlanChangeLogModel) IsDuplicate(err error) bool { return isDuplicateErr(err) }
