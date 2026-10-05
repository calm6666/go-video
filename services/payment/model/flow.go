package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 流水业务类型，与 rpc.FlowBizType、pm_flow.biz_type 取值一致。
const (
	// FlowBizRecharge 充值入账（+）。
	FlowBizRecharge int32 = 1
	// FlowBizPayment 消费出账（-）。
	FlowBizPayment int32 = 2
	// FlowBizRefund 退款入账（+）。
	FlowBizRefund int32 = 3
	// FlowBizAdminAdjust 运营调整（正负皆可，必须有 reason）。
	FlowBizAdminAdjust int32 = 4
)

// flowColumns 与迁移 SQL 的 pm_flow 定义一一对应。
const flowColumns = "flow_id, mid, biz_type, biz_no, delta_minor, balance_after_minor, currency, " +
	"remark, operator, request_id, ctime"

// Flow 资金流水行。
//
// append-only：本模型不提供任何更新或删除方法。流水是台账的审计证据，
// 需要「修正」时只能再记一条反向流水（ADMIN_ADJUST），不能改写历史行。
type Flow struct {
	FlowId            int64  `db:"flow_id"`
	Mid               int64  `db:"mid"`
	BizType           int32  `db:"biz_type"`
	BizNo             string `db:"biz_no"`
	DeltaMinor        int64  `db:"delta_minor"`
	BalanceAfterMinor int64  `db:"balance_after_minor"`
	Currency          string `db:"currency"`
	Remark            string `db:"remark"`
	Operator          string `db:"operator"`
	RequestId         string `db:"request_id"`
	Ctime             int64  `db:"ctime"`
}

// FlowListQuery 流水台账分页条件。Mid=0 表示跨用户（运营面）。
type FlowListQuery struct {
	Mid     int64
	BizType int32
	BizNo   string
	FromTs  int64
	ToTs    int64
	Page    ListPage
}

// FlowModel pm_flow 读写接口。
type FlowModel interface {
	// InsertTx 在事务内写流水并返回 flow_id。request_id 唯一索引是「同一请求不重复入账」
	// 的最终防线：与单据唯一索引一起，构成并发下的双重保险。
	InsertTx(ctx context.Context, session sqlx.Session, f *Flow) (int64, error)
	// FindByRequestID 按幂等键查流水；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*Flow, error)
	// FindByBizNo 按 (biz_type, biz_no) 查流水；不存在返回 (nil, nil)。
	FindByBizNo(ctx context.Context, bizType int32, bizNo string) (*Flow, error)
	// List 按条件分页；空结果返回空切片。
	List(ctx context.Context, q FlowListQuery) ([]*Flow, error)
	// Count 同条件总数。
	Count(ctx context.Context, q FlowListQuery) (int64, error)
	// IsDuplicate 判定错误是否为唯一索引冲突。
	IsDuplicate(err error) bool
}

type defaultFlowModel struct {
	conn sqlx.SqlConn
}

// NewFlowModel 创建 FlowModel 实现。
func NewFlowModel(conn sqlx.SqlConn) FlowModel {
	return &defaultFlowModel{conn: conn}
}

func (m *defaultFlowModel) InsertTx(ctx context.Context, session sqlx.Session, f *Flow) (int64, error) {
	if f.Ctime == 0 {
		f.Ctime = nowUnix()
	}
	res, err := pick(m.conn, session).ExecCtx(ctx,
		"INSERT INTO pm_flow (mid, biz_type, biz_no, delta_minor, balance_after_minor, currency, "+
			"remark, operator, request_id, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		f.Mid, f.BizType, f.BizNo, f.DeltaMinor, f.BalanceAfterMinor, f.Currency,
		f.Remark, f.Operator, f.RequestId, f.Ctime)
	if err != nil {
		return 0, fmt.Errorf("pm_flow InsertTx: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("pm_flow InsertTx LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultFlowModel) FindByRequestID(ctx context.Context, requestID string) (*Flow, error) {
	var f Flow
	query := "SELECT " + flowColumns + " FROM pm_flow WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &f, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_flow FindByRequestID: %w", err)
	}
	return &f, nil
}

func (m *defaultFlowModel) FindByBizNo(ctx context.Context, bizType int32, bizNo string) (*Flow, error) {
	var f Flow
	query := "SELECT " + flowColumns + " FROM pm_flow WHERE biz_type = ? AND biz_no = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &f, query, bizType, bizNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_flow FindByBizNo: %w", err)
	}
	return &f, nil
}

func (m *defaultFlowModel) List(ctx context.Context, q FlowListQuery) ([]*Flow, error) {
	where, args := flowWhere(q)
	query := "SELECT " + flowColumns + " FROM pm_flow WHERE 1=1" + where +
		" ORDER BY ctime DESC, flow_id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Page.Limit, q.Page.Offset)

	var rows []*Flow
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []*Flow{}, nil
		}
		return nil, fmt.Errorf("pm_flow List: %w", err)
	}
	if rows == nil {
		rows = []*Flow{}
	}
	return rows, nil
}

func (m *defaultFlowModel) Count(ctx context.Context, q FlowListQuery) (int64, error) {
	where, args := flowWhere(q)
	var cnt int64
	query := "SELECT COUNT(*) FROM pm_flow WHERE 1=1" + where
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("pm_flow Count: %w", err)
	}
	return cnt, nil
}

func (m *defaultFlowModel) IsDuplicate(err error) bool { return isDuplicateErr(err) }

// flowWhere 组装过滤条件；所有值都走占位符。
func flowWhere(q FlowListQuery) (string, []any) {
	clause := ""
	var args []any
	if q.Mid > 0 {
		clause += " AND mid = ?"
		args = append(args, q.Mid)
	}
	if q.BizType != 0 {
		clause += " AND biz_type = ?"
		args = append(args, q.BizType)
	}
	if q.BizNo != "" {
		clause += " AND biz_no = ?"
		args = append(args, q.BizNo)
	}
	win, winArgs := windowClause(q.FromTs, q.ToTs)
	return clause + win, append(args, winArgs...)
}
