package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 退款单状态，与 rpc.RefundState、pm_refund.state 取值一致。
const (
	// RefundStateSucceeded 退余额是同步完成的（沙箱台账内一步落账）。
	RefundStateSucceeded int32 = 1
	// RefundStateFailed 退款失败。本服务的失败一律以错误返回并回滚事务，
	// 因此该状态保留给未来渠道异步退款回执。
	RefundStateFailed int32 = 2
)

// refundColumns 与迁移 SQL 的 pm_refund 定义一一对应。
const refundColumns = "id, refund_no, request_id, payment_no, biz_order_no, mid, amount_minor, " +
	"currency, state, destination, operator, reason, ctime"

// Refund 退款单行。
type Refund struct {
	Id          int64  `db:"id"`
	RefundNo    string `db:"refund_no"`
	RequestId   string `db:"request_id"`
	PaymentNo   string `db:"payment_no"`
	BizOrderNo  string `db:"biz_order_no"`
	Mid         int64  `db:"mid"`
	AmountMinor int64  `db:"amount_minor"`
	Currency    string `db:"currency"`
	State       int32  `db:"state"`
	Destination string `db:"destination"`
	Operator    string `db:"operator"`
	Reason      string `db:"reason"`
	Ctime       int64  `db:"ctime"`
}

// RefundListQuery 退款台账分页条件。Mid=0 表示跨用户。
type RefundListQuery struct {
	Mid       int64
	PaymentNo string
	FromTs    int64
	ToTs      int64
	Page      ListPage
}

// RefundModel pm_refund 读写接口。退款单一经写入即为终态，故无更新方法。
type RefundModel interface {
	// InsertTx 在事务内写入退款单；refund_no / request_id 唯一索引兜住并发重放。
	InsertTx(ctx context.Context, session sqlx.Session, r *Refund) (int64, error)
	// FindOne 按 refund_no 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, refundNo string) (*Refund, error)
	// FindByRequestID 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*Refund, error)
	// List 按条件分页；空结果返回空切片。
	List(ctx context.Context, q RefundListQuery) ([]*Refund, error)
	// Count 同条件总数。
	Count(ctx context.Context, q RefundListQuery) (int64, error)
	// IsDuplicate 判定错误是否为唯一索引冲突。
	IsDuplicate(err error) bool
}

type defaultRefundModel struct {
	conn sqlx.SqlConn
}

// NewRefundModel 创建 RefundModel 实现。
func NewRefundModel(conn sqlx.SqlConn) RefundModel {
	return &defaultRefundModel{conn: conn}
}

func (m *defaultRefundModel) InsertTx(ctx context.Context, session sqlx.Session, r *Refund) (int64, error) {
	if r.Ctime == 0 {
		r.Ctime = nowUnix()
	}
	res, err := pick(m.conn, session).ExecCtx(ctx,
		"INSERT INTO pm_refund (refund_no, request_id, payment_no, biz_order_no, mid, amount_minor, "+
			"currency, state, destination, operator, reason, ctime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		r.RefundNo, r.RequestId, r.PaymentNo, r.BizOrderNo, r.Mid, r.AmountMinor,
		r.Currency, r.State, r.Destination, r.Operator, r.Reason, r.Ctime)
	if err != nil {
		return 0, fmt.Errorf("pm_refund InsertTx: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("pm_refund InsertTx LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultRefundModel) FindOne(ctx context.Context, refundNo string) (*Refund, error) {
	var r Refund
	query := "SELECT " + refundColumns + " FROM pm_refund WHERE refund_no = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &r, query, refundNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_refund FindOne: %w", err)
	}
	return &r, nil
}

func (m *defaultRefundModel) FindByRequestID(ctx context.Context, requestID string) (*Refund, error) {
	var r Refund
	query := "SELECT " + refundColumns + " FROM pm_refund WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &r, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_refund FindByRequestID: %w", err)
	}
	return &r, nil
}

func (m *defaultRefundModel) List(ctx context.Context, q RefundListQuery) ([]*Refund, error) {
	where, args := refundWhere(q)
	query := "SELECT " + refundColumns + " FROM pm_refund WHERE 1=1" + where +
		" ORDER BY ctime DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Page.Limit, q.Page.Offset)

	var rows []*Refund
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []*Refund{}, nil
		}
		return nil, fmt.Errorf("pm_refund List: %w", err)
	}
	if rows == nil {
		rows = []*Refund{}
	}
	return rows, nil
}

func (m *defaultRefundModel) Count(ctx context.Context, q RefundListQuery) (int64, error) {
	where, args := refundWhere(q)
	var cnt int64
	query := "SELECT COUNT(*) FROM pm_refund WHERE 1=1" + where
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("pm_refund Count: %w", err)
	}
	return cnt, nil
}

func (m *defaultRefundModel) IsDuplicate(err error) bool { return isDuplicateErr(err) }

// refundWhere 组装过滤条件。契约里没有状态过滤位（ListRefundsReq 只有 mid/payment_no），
// 因此这里也不按 state 过滤——沙箱退款只会落终态行。
func refundWhere(q RefundListQuery) (string, []any) {
	clause := ""
	var args []any
	if q.Mid > 0 {
		clause += " AND mid = ?"
		args = append(args, q.Mid)
	}
	if q.PaymentNo != "" {
		clause += " AND payment_no = ?"
		args = append(args, q.PaymentNo)
	}
	win, winArgs := windowClause(q.FromTs, q.ToTs)
	return clause + win, append(args, winArgs...)
}
