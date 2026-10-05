package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 充值单状态，与 rpc.RechargeState、pm_recharge.state 取值严格一致，禁止重排。
const (
	// RechargeStatePending 已开单、未入账（唯一可取消的状态）。
	RechargeStatePending int32 = 1
	// RechargeStateSuccess 已结算入账，余额与流水都已落地。
	RechargeStateSuccess int32 = 2
	// RechargeStateCancelled 未结算即取消，钱从未进账。
	RechargeStateCancelled int32 = 3
	// RechargeStateFailed 受理失败。沙箱不会自动产生该状态，仅作为台账终态保留。
	RechargeStateFailed int32 = 4
)

// ChannelSandbox 充值渠道，与 rpc.PayChannel_PAY_CHANNEL_SANDBOX 一致。
// 本项目唯一存在的渠道：只改本地台账，不请求任何第三方支付。
const ChannelSandbox int32 = 1

// rechargeColumns 与迁移 SQL 的 pm_recharge 定义一一对应。
const rechargeColumns = "id, recharge_no, request_id, mid, amount_minor, currency, channel, state, " +
	"operator, reason, settled_at, client_trace_id, ctime, mtime"

// Recharge 充值单行。
type Recharge struct {
	Id            int64  `db:"id"`
	RechargeNo    string `db:"recharge_no"`
	RequestId     string `db:"request_id"`
	Mid           int64  `db:"mid"`
	AmountMinor   int64  `db:"amount_minor"`
	Currency      string `db:"currency"`
	Channel       int32  `db:"channel"`
	State         int32  `db:"state"`
	Operator      string `db:"operator"`
	Reason        string `db:"reason"`
	SettledAt     int64  `db:"settled_at"`
	ClientTraceId string `db:"client_trace_id"`
	Ctime         int64  `db:"ctime"`
	Mtime         int64  `db:"mtime"`
}

// RechargeListQuery 充值台账分页条件。
// Mid=0 表示跨用户（运营面）；State=0 表示不限状态。
type RechargeListQuery struct {
	Mid    int64
	State  int32
	FromTs int64
	ToTs   int64
	Page   ListPage
}

// RechargeModel pm_recharge 读写接口。
type RechargeModel interface {
	// Insert 写入充值单，返回自增 id。recharge_no / request_id 上有唯一索引，
	// 并发重复写入会返回错误，调用方需回查并按重放处理（IsDuplicate 判定）。
	Insert(ctx context.Context, r *Recharge) (int64, error)
	// FindOne 按 recharge_no 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, rechargeNo string) (*Recharge, error)
	// FindOneTx 在事务内按 recharge_no 查询；不存在返回 (nil, nil)。
	FindOneTx(ctx context.Context, session sqlx.Session, rechargeNo string) (*Recharge, error)
	// FindByRequestID 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*Recharge, error)
	// List 按条件分页；空结果返回空切片。
	List(ctx context.Context, q RechargeListQuery) ([]*Recharge, error)
	// Count 同条件总数。
	Count(ctx context.Context, q RechargeListQuery) (int64, error)
	// SettleTx 以 CAS 方式把 PENDING 推进为 SUCCESS，并回填结算主体/理由/时间。
	// 返回 false 表示状态已被并发推进，调用方需回读并按重放处理。
	SettleTx(ctx context.Context, session sqlx.Session, rechargeNo, operator, reason string, settledAt int64) (bool, error)
	// CancelTx 以 CAS 方式把 PENDING 推进为 CANCELLED。
	CancelTx(ctx context.Context, session sqlx.Session, rechargeNo, operator, reason string) (bool, error)
	// IsDuplicate 判定错误是否为唯一索引冲突。
	IsDuplicate(err error) bool
}

type defaultRechargeModel struct {
	conn sqlx.SqlConn
}

// NewRechargeModel 创建 RechargeModel 实现。
func NewRechargeModel(conn sqlx.SqlConn) RechargeModel {
	return &defaultRechargeModel{conn: conn}
}

func (m *defaultRechargeModel) Insert(ctx context.Context, r *Recharge) (int64, error) {
	now := nowUnix()
	if r.Ctime == 0 {
		r.Ctime = now
	}
	r.Mtime = r.Ctime
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO pm_recharge (recharge_no, request_id, mid, amount_minor, currency, channel, state, "+
			"operator, reason, settled_at, client_trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		r.RechargeNo, r.RequestId, r.Mid, r.AmountMinor, r.Currency, r.Channel, r.State,
		r.Operator, r.Reason, r.SettledAt, r.ClientTraceId, r.Ctime, r.Mtime)
	if err != nil {
		return 0, fmt.Errorf("pm_recharge Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("pm_recharge Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultRechargeModel) FindOne(ctx context.Context, rechargeNo string) (*Recharge, error) {
	return scanRecharge(ctx, m.conn, rechargeNo)
}

func (m *defaultRechargeModel) FindOneTx(ctx context.Context, session sqlx.Session, rechargeNo string) (*Recharge, error) {
	return scanRecharge(ctx, pick(m.conn, session), rechargeNo)
}

func (m *defaultRechargeModel) FindByRequestID(ctx context.Context, requestID string) (*Recharge, error) {
	var r Recharge
	query := "SELECT " + rechargeColumns + " FROM pm_recharge WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &r, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_recharge FindByRequestID: %w", err)
	}
	return &r, nil
}

func (m *defaultRechargeModel) List(ctx context.Context, q RechargeListQuery) ([]*Recharge, error) {
	where, args := rechargeWhere(q)
	query := "SELECT " + rechargeColumns + " FROM pm_recharge WHERE 1=1" + where +
		" ORDER BY ctime DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Page.Limit, q.Page.Offset)

	var rows []*Recharge
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []*Recharge{}, nil
		}
		return nil, fmt.Errorf("pm_recharge List: %w", err)
	}
	if rows == nil {
		rows = []*Recharge{}
	}
	return rows, nil
}

func (m *defaultRechargeModel) Count(ctx context.Context, q RechargeListQuery) (int64, error) {
	where, args := rechargeWhere(q)
	var cnt int64
	query := "SELECT COUNT(*) FROM pm_recharge WHERE 1=1" + where
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("pm_recharge Count: %w", err)
	}
	return cnt, nil
}

// SettleTx 以 CAS 方式把 PENDING 推进为 SUCCESS 并回填结算主体/理由/时间。
// settledAt <= 0 表示由本层取当前时间，保证「状态推进」与「settled_at」同时落地。
func (m *defaultRechargeModel) SettleTx(ctx context.Context, session sqlx.Session, rechargeNo, operator, reason string, settledAt int64) (bool, error) {
	return rechargeCas(ctx, m.conn, session,
		"UPDATE pm_recharge SET state = ?, operator = ?, reason = ?, settled_at = ?, mtime = ? "+
			"WHERE recharge_no = ? AND state = ?",
		[]any{RechargeStateSuccess, operator, reason, settledAtOrNow(settledAt), nowUnix(), rechargeNo, RechargeStatePending},
		"pm_recharge SettleTx")
}

func (m *defaultRechargeModel) CancelTx(ctx context.Context, session sqlx.Session, rechargeNo, operator, reason string) (bool, error) {
	return rechargeCas(ctx, m.conn, session,
		"UPDATE pm_recharge SET state = ?, operator = ?, reason = ?, mtime = ? "+
			"WHERE recharge_no = ? AND state = ?",
		[]any{RechargeStateCancelled, operator, reason, nowUnix(), rechargeNo, RechargeStatePending},
		"pm_recharge CancelTx")
}

func (m *defaultRechargeModel) IsDuplicate(err error) bool { return isDuplicateErr(err) }

// rechargeCas 执行带状态守卫（WHERE state = PENDING）的 CAS 更新。
func rechargeCas(ctx context.Context, conn sqlx.SqlConn, session sqlx.Session,
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

// rechargeWhere 组装过滤条件；所有值都走占位符，不拼用户输入。
func rechargeWhere(q RechargeListQuery) (string, []any) {
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
	win, winArgs := windowClause(q.FromTs, q.ToTs)
	return clause + win, append(args, winArgs...)
}

// scanRecharge 按单号读充值单；不存在返回 (nil, nil)。
func scanRecharge(ctx context.Context, e execer, rechargeNo string) (*Recharge, error) {
	var r Recharge
	query := "SELECT " + rechargeColumns + " FROM pm_recharge WHERE recharge_no = ? LIMIT 1"
	if err := e.QueryRowCtx(ctx, &r, query, rechargeNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_recharge FindOne: %w", err)
	}
	return &r, nil
}
