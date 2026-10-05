// Package model 是 payment（资金域）的数据库访问层，只操作 go_video_payment 库
// 自身的 pm_* 表（AGENTS.md §5：服务只能写自己的 schema）。
//
// 表清单与 deploy/migrations/payment/000001_create_payment_tables.sql 严格一致：
//
//	pm_wallet    余额账户（mid 唯一，现金余额的唯一真值）
//	pm_recharge  充值单（recharge_no / request_id 唯一）
//	pm_payment   支付单（payment_no 唯一，biz_order_no 唯一＝一单一支付）
//	pm_refund    退款单（refund_no / request_id 唯一，payment_no 索引）
//	pm_flow      资金流水（append-only，request_id 唯一＝入账幂等的最终防线）
//
// 资金不变式（本包的职责边界，logic 层不得绕过）：
//  1. 余额变更只能是条件更新：`UPDATE pm_wallet SET balance_minor = balance_minor - ?
//     WHERE mid = ? AND balance_minor >= ?`，影响行数 0 即余额不足；
//     严禁「先查后改」，那会在并发下超扣。
//  2. 「改单据 + 改余额 + 写流水」必须在同一个 TransactCtx 里，任何一步失败整体回滚，
//     不允许出现已扣款无支付单或已入账无流水。
//  3. 金额一律 int64 最小货币单位（分），禁止浮点；币种随行走。
package model

import (
	"context"
	"database/sql"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Models 聚合本服务的表模型，并暴露事务执行器。
// logic 侧统一通过 Models.Tx 把多表写入串成一个事务。
type Models struct {
	conn sqlx.SqlConn

	Wallet   WalletModel
	Recharge RechargeModel
	Payment  PaymentModel
	Refund   RefundModel
	Flow     FlowModel
}

// NewModels 基于给定连接构造全部模型。
func NewModels(conn sqlx.SqlConn) *Models {
	return &Models{
		conn:     conn,
		Wallet:   NewWalletModel(conn),
		Recharge: NewRechargeModel(conn),
		Payment:  NewPaymentModel(conn),
		Refund:   NewRefundModel(conn),
		Flow:     NewFlowModel(conn),
	}
}

// Conn 返回底层连接，供只读探测与测试替换。
func (m *Models) Conn() sqlx.SqlConn { return m.conn }

// Tx 在单个数据库事务内执行 fn；fn 报错即整体回滚。
// fn 收到的 session 必须传给各模型的 *Tx 方法，否则该步会落到事务之外。
func (m *Models) Tx(ctx context.Context, fn func(ctx context.Context, session sqlx.Session) error) error {
	return m.conn.TransactCtx(ctx, fn)
}

// execer 抽象 sqlx.SqlConn 与 sqlx.Session，使同一条 SQL 可在事务内外复用；
// session 传 nil 表示走连接自身（非事务）。
type execer interface {
	ExecCtx(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowCtx(ctx context.Context, v any, query string, args ...any) error
	QueryRowsCtx(ctx context.Context, v any, query string, args ...any) error
}

// pick 选择执行载体。
func pick(conn sqlx.SqlConn, session sqlx.Session) execer {
	if session != nil {
		return session
	}
	return conn
}

// ListPage 是分页参数（偏移 + 条数），由各 List 方法在 SQL 末端使用。
type ListPage struct {
	Offset int64
	Limit  int64
}

// windowClause 生成 ` AND ctime >= ? AND ctime <= ?` 片段与参数；
// 两个边界都可选，0 表示该侧不限。
func windowClause(fromTs, toTs int64) (string, []any) {
	var (
		clause = ""
		args   []any
	)
	if fromTs > 0 {
		clause += " AND ctime >= ?"
		args = append(args, fromTs)
	}
	if toTs > 0 {
		clause += " AND ctime <= ?"
		args = append(args, toTs)
	}
	return clause, args
}
