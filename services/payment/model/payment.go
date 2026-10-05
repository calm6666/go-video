package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 支付单状态，与 rpc.PaymentState、pm_payment.state 取值严格一致，禁止重排。
const (
	// PaymentStatePending 已受理未支付。本服务的两条受理路径都是同步终态
	// （BALANCE 即时扣减、SANDBOX_CHANNEL 即时成功），因此该状态在沙箱下不会产生，
	// 保留它是为了 ClosePayment 的语义完整与未来接入真实渠道时的中间态。
	PaymentStatePending int32 = 1
	// PaymentStatePaid 已支付成功。
	PaymentStatePaid int32 = 2
	// PaymentStateFailed 受理失败。
	PaymentStateFailed int32 = 3
	// PaymentStateClosed 未支付即关闭。
	PaymentStateClosed int32 = 4
	// PaymentStateRefunded 全额退款。
	PaymentStateRefunded int32 = 5
	// PaymentStatePartiallyRefunded 部分退款，refunded_minor 记录已退累计额。
	PaymentStatePartiallyRefunded int32 = 6
)

// 支付方式，与 rpc.PayMethod 一致。
const (
	// MethodBalance 扣本服务余额。
	MethodBalance int32 = 1
	// MethodSandboxChannel 沙箱收单，受理即成功，不动余额、不写余额流水。
	MethodSandboxChannel int32 = 2
)

// DestinationBalance 退款去向，与 rpc.RefundInfo.destination 取值一致。
// 本项目只有这一个合法值：原路退回真实渠道没有配置。
const DestinationBalance = "BALANCE"

// paymentColumns 与迁移 SQL 的 pm_payment 定义一一对应。
const paymentColumns = "id, payment_no, biz_order_no, request_id, mid, amount_minor, refunded_minor, " +
	"currency, method, state, subject, operator, paid_at, expire_at, last_request_id, remark, ctime, mtime"

// Payment 支付单行。
//
// biz_order_no 上有唯一索引（一单一支付）；operator / last_request_id / remark
// 是台账审计列，契约里不回显。
type Payment struct {
	Id            int64  `db:"id"`
	PaymentNo     string `db:"payment_no"`
	BizOrderNo    string `db:"biz_order_no"`
	RequestId     string `db:"request_id"`
	Mid           int64  `db:"mid"`
	AmountMinor   int64  `db:"amount_minor"`
	RefundedMinor int64  `db:"refunded_minor"`
	Currency      string `db:"currency"`
	Method        int32  `db:"method"`
	State         int32  `db:"state"`
	Subject       string `db:"subject"`
	Operator      string `db:"operator"`
	PaidAt        int64  `db:"paid_at"`
	ExpireAt      int64  `db:"expire_at"`
	LastRequestId string `db:"last_request_id"`
	Remark        string `db:"remark"`
	Ctime         int64  `db:"ctime"`
	Mtime         int64  `db:"mtime"`
}

// RefundableMinor 剩余可退金额。
func (p *Payment) RefundableMinor() int64 {
	left := p.AmountMinor - p.RefundedMinor
	if left < 0 {
		return 0
	}
	return left
}

// PaymentListQuery 支付台账分页条件。Mid=0 表示跨用户；State/Method 为 0 表示不限。
type PaymentListQuery struct {
	Mid    int64
	State  int32
	Method int32
	FromTs int64
	ToTs   int64
	Page   ListPage
}

// PaymentModel pm_payment 读写接口。
type PaymentModel interface {
	// InsertTx 在事务内写入支付单并返回自增 id。
	// payment_no / biz_order_no / request_id 三个唯一索引是并发下的最终防线。
	InsertTx(ctx context.Context, session sqlx.Session, p *Payment) (int64, error)
	// FindOne 按 payment_no 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, paymentNo string) (*Payment, error)
	// FindOneTx 在事务内按 payment_no 查询；不存在返回 (nil, nil)。
	FindOneTx(ctx context.Context, session sqlx.Session, paymentNo string) (*Payment, error)
	// FindByBizOrderNo 按订单号查询（一单一支付）；不存在返回 (nil, nil)。
	FindByBizOrderNo(ctx context.Context, bizOrderNo string) (*Payment, error)
	// FindByRequestID 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*Payment, error)
	// List 按条件分页；空结果返回空切片。
	List(ctx context.Context, q PaymentListQuery) ([]*Payment, error)
	// Count 同条件总数。
	Count(ctx context.Context, q PaymentListQuery) (int64, error)
	// CloseTx 以 CAS 方式把 PENDING 推进为 CLOSED，并记录关闭主体与理由。
	CloseTx(ctx context.Context, session sqlx.Session, paymentNo, operator, requestID, reason string) (bool, error)
	// RefundTx 累加 refunded_minor 并同步推进 state（退满＝REFUNDED，否则 PARTIALLY_REFUNDED）。
	// SQL 自带累计退款不超过 amount_minor 的守卫，返回 false 表示本次退款不成立。
	RefundTx(ctx context.Context, session sqlx.Session, paymentNo string, refundMinor int64, operator, requestID, remark string) (bool, error)
	// IsDuplicate 判定错误是否为唯一索引冲突。
	IsDuplicate(err error) bool
}

type defaultPaymentModel struct {
	conn sqlx.SqlConn
}

// NewPaymentModel 创建 PaymentModel 实现。
func NewPaymentModel(conn sqlx.SqlConn) PaymentModel {
	return &defaultPaymentModel{conn: conn}
}

func (m *defaultPaymentModel) InsertTx(ctx context.Context, session sqlx.Session, p *Payment) (int64, error) {
	now := nowUnix()
	if p.Ctime == 0 {
		p.Ctime = now
	}
	p.Mtime = p.Ctime
	res, err := pick(m.conn, session).ExecCtx(ctx,
		"INSERT INTO pm_payment (payment_no, biz_order_no, request_id, mid, amount_minor, refunded_minor, "+
			"currency, method, state, subject, operator, paid_at, expire_at, last_request_id, remark, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		p.PaymentNo, p.BizOrderNo, p.RequestId, p.Mid, p.AmountMinor, p.RefundedMinor,
		p.Currency, p.Method, p.State, p.Subject, p.Operator, p.PaidAt, p.ExpireAt,
		p.RequestId, "", p.Ctime, p.Mtime)
	if err != nil {
		return 0, fmt.Errorf("pm_payment InsertTx: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("pm_payment InsertTx LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultPaymentModel) FindOne(ctx context.Context, paymentNo string) (*Payment, error) {
	return scanPayment(ctx, m.conn, paymentNo)
}

func (m *defaultPaymentModel) FindOneTx(ctx context.Context, session sqlx.Session, paymentNo string) (*Payment, error) {
	return scanPayment(ctx, pick(m.conn, session), paymentNo)
}

func (m *defaultPaymentModel) FindByBizOrderNo(ctx context.Context, bizOrderNo string) (*Payment, error) {
	var p Payment
	query := "SELECT " + paymentColumns + " FROM pm_payment WHERE biz_order_no = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &p, query, bizOrderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_payment FindByBizOrderNo: %w", err)
	}
	return &p, nil
}

func (m *defaultPaymentModel) FindByRequestID(ctx context.Context, requestID string) (*Payment, error) {
	var p Payment
	query := "SELECT " + paymentColumns + " FROM pm_payment WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &p, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_payment FindByRequestID: %w", err)
	}
	return &p, nil
}

func (m *defaultPaymentModel) List(ctx context.Context, q PaymentListQuery) ([]*Payment, error) {
	where, args := paymentWhere(q)
	query := "SELECT " + paymentColumns + " FROM pm_payment WHERE 1=1" + where +
		" ORDER BY ctime DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Page.Limit, q.Page.Offset)

	var rows []*Payment
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []*Payment{}, nil
		}
		return nil, fmt.Errorf("pm_payment List: %w", err)
	}
	if rows == nil {
		rows = []*Payment{}
	}
	return rows, nil
}

func (m *defaultPaymentModel) Count(ctx context.Context, q PaymentListQuery) (int64, error) {
	where, args := paymentWhere(q)
	var cnt int64
	query := "SELECT COUNT(*) FROM pm_payment WHERE 1=1" + where
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("pm_payment Count: %w", err)
	}
	return cnt, nil
}

func (m *defaultPaymentModel) CloseTx(ctx context.Context, session sqlx.Session,
	paymentNo, operator, requestID, reason string) (bool, error) {
	const query = "UPDATE pm_payment SET state = ?, operator = ?, last_request_id = ?, remark = ?, mtime = ? " +
		"WHERE payment_no = ? AND state = ?"
	args := []any{PaymentStateClosed, operator, requestID, reason, nowUnix(), paymentNo, PaymentStatePending}
	return paymentExec(ctx, m.conn, session, query, args, "pm_payment CloseTx")
}

// RefundTx 累加退款额并推进状态。
//
// 依赖 MySQL 的 SET 列表求值顺序（自左向右，后面的表达式看到前面已更新的列值）：
// state 判定用的 refunded_minor 是累加之后的新值，因此不需要先查后改。
// 守卫 `refunded_minor + ? <= amount_minor AND state IN (PAID, PARTIALLY_REFUNDED)`
// 写在 WHERE 里，影响行数 0 即代表「累计超退」或「状态不可退」，整体回滚。
func (m *defaultPaymentModel) RefundTx(ctx context.Context, session sqlx.Session,
	paymentNo string, refundMinor int64, operator, requestID, remark string) (bool, error) {
	const query = "UPDATE pm_payment SET refunded_minor = refunded_minor + ?, " +
		"state = IF(refunded_minor >= amount_minor, 5, 6), " + // 5 REFUNDED / 6 PARTIALLY_REFUNDED
		"operator = ?, last_request_id = ?, remark = ?, mtime = ? " +
		"WHERE payment_no = ? AND state IN (2, 6) AND refunded_minor + ? <= amount_minor"
	args := []any{refundMinor, operator, requestID, remark, nowUnix(), paymentNo, refundMinor}
	return paymentExec(ctx, m.conn, session, query, args, "pm_payment RefundTx")
}

func (m *defaultPaymentModel) IsDuplicate(err error) bool { return isDuplicateErr(err) }

// paymentExec 执行带状态守卫的更新并回报是否命中。
func paymentExec(ctx context.Context, conn sqlx.SqlConn, session sqlx.Session,
	query string, args []any, op string) (bool, error) {
	res, err := pick(conn, session).ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return aff > 0, nil
}

// paymentWhere 组装过滤条件；所有值都走占位符。
func paymentWhere(q PaymentListQuery) (string, []any) {
	clause := ""
	var args []any
	if q.Mid > 0 {
		clause += " AND mid = ?"
		args = append(args, q.Mid)
	}
	if q.State != 0 {
		clause += " AND state = ?"
		args = append(args, q.State)
	}
	if q.Method != 0 {
		clause += " AND method = ?"
		args = append(args, q.Method)
	}
	win, winArgs := windowClause(q.FromTs, q.ToTs)
	return clause + win, append(args, winArgs...)
}

// scanPayment 按支付单号读单；不存在返回 (nil, nil)。
func scanPayment(ctx context.Context, e execer, paymentNo string) (*Payment, error) {
	var p Payment
	query := "SELECT " + paymentColumns + " FROM pm_payment WHERE payment_no = ? LIMIT 1"
	if err := e.QueryRowCtx(ctx, &p, query, paymentNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_payment FindOne: %w", err)
	}
	return &p, nil
}
