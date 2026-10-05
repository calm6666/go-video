package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// orderEventColumns 是 to_order_event 的完整列清单，
// 与 deploy/migrations/trade-order/000001_create_trade_order_tables.sql 一一对应。
const orderEventColumns = "event_id, order_no, from_state, to_state, operator, reason, request_id, ctime"

// OrderEvent 是订单状态流转台账行（to_order_event 投影）。
//
// 每次状态迁移一行，与主表更新同事务提交（AGENTS.md §5「事务内写业务数据和台账」）：
// 订单状态是可以被覆盖的当前值，只有这张表能回答「谁在什么时候把它从 A 推到 B、为什么」。
// 因此任何拒绝伪成功的路径（例如已取消订单收到绑款）也要留一行同态台账作为人工介入证据。
type OrderEvent struct {
	EventID   int64  `db:"event_id"`   // 自增主键
	OrderNo   string `db:"order_no"`   // 订单号（跨服务只存主键引用，不建外键）
	FromState int32  `db:"from_state"` // 迁移前状态；建单行为 0（UNSPECIFIED）
	ToState   int32  `db:"to_state"`   // 迁移后状态
	Operator  string `db:"operator"`   // "user" / 运营工号 / "cron" / "system" / "trade-order"
	Reason    string `db:"reason"`     // 台账摘要：不含 PII 与凭据
	RequestID string `db:"request_id"` // 触发本次迁移的幂等键
	Ctime     int64  `db:"ctime"`      // Unix 秒
}

// OrderEventModel to_order_event 表读写接口。
type OrderEventModel interface {
	// InsertTx 在事务内追加一行台账（与主表 CAS 同事务）。
	InsertTx(ctx context.Context, session sqlx.Session, e *OrderEvent) (int64, error)
	// ListByOrderNo 按时间正序分页读取台账，并同时回总数。
	ListByOrderNo(ctx context.Context, orderNo string, offset, limit int64) ([]*OrderEvent, int64, error)
	// FindByRequestAndState 按 (order_no, request_id, to_state) 查最近一行，
	// 用于写接口的幂等重放判定：命中说明这次动作已经生效过，返回 duplicated=true。
	// 注意该列组合上没有唯一索引（建单一次请求会写多行），所以这里是查询不是约束。
	FindByRequestAndState(ctx context.Context, orderNo, requestID string, toState int32) (*OrderEvent, error)
	// FindLastTransitionTo 取最后一次进入某状态的台账行。
	// RejectRefund 需要据此把订单「回到申请前的原状态」，而不是猜一个。
	FindLastTransitionTo(ctx context.Context, orderNo string, toState int32) (*OrderEvent, error)
}

type defaultOrderEventModel struct {
	conn sqlx.SqlConn
}

// NewOrderEventModel 创建 OrderEventModel 实现。
func NewOrderEventModel(conn sqlx.SqlConn) OrderEventModel {
	return &defaultOrderEventModel{conn: conn}
}

const insertOrderEventSQL = "INSERT INTO to_order_event (order_no, from_state, to_state, operator, reason, " +
	"request_id, ctime) VALUES (?, ?, ?, ?, ?, ?, ?)"

func (m *defaultOrderEventModel) InsertTx(ctx context.Context, session sqlx.Session, e *OrderEvent) (int64, error) {
	if e.OrderNo == "" {
		return 0, ErrOrderNoRequired
	}
	// from_state=0 是「建单」这一行的合法取值（此前不存在状态），
	// to_state 则必须是已定义状态：写进 0 会让台账出现无法解释的行。
	if !ValidState(e.ToState) {
		return 0, fmt.Errorf("%w: to_state=%d", ErrInvalidStateTransition, e.ToState)
	}
	if e.FromState != 0 && !ValidState(e.FromState) {
		return 0, fmt.Errorf("%w: from_state=%d", ErrInvalidStateTransition, e.FromState)
	}
	if e.Ctime == 0 {
		e.Ctime = nowUnix()
	}
	res, err := pickConn(session, m.conn).ExecCtx(ctx, insertOrderEventSQL,
		e.OrderNo, e.FromState, e.ToState, e.Operator, e.Reason, e.RequestID, e.Ctime)
	if err != nil {
		return 0, fmt.Errorf("to_order_event InsertTx: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("to_order_event InsertTx LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultOrderEventModel) ListByOrderNo(ctx context.Context, orderNo string,
	offset, limit int64,
) ([]*OrderEvent, int64, error) {
	if orderNo == "" {
		return nil, 0, ErrOrderNoRequired
	}
	if limit <= 0 {
		return nil, 0, ErrInvalidPage
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM to_order_event WHERE order_no = ?", orderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			total = 0
		} else {
			return nil, 0, fmt.Errorf("to_order_event count: %w", err)
		}
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*OrderEvent
	query := "SELECT " + orderEventColumns + " FROM to_order_event WHERE order_no = ? " +
		"ORDER BY ctime ASC, event_id ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, orderNo, limit, offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("to_order_event list: %w", err)
	}
	return rows, total, nil
}

func (m *defaultOrderEventModel) FindByRequestAndState(ctx context.Context, orderNo, requestID string,
	toState int32,
) (*OrderEvent, error) {
	if orderNo == "" || requestID == "" {
		return nil, nil
	}
	var e OrderEvent
	query := "SELECT " + orderEventColumns + " FROM to_order_event " +
		"WHERE order_no = ? AND request_id = ? AND to_state = ? ORDER BY event_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &e, query, orderNo, requestID, toState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("to_order_event FindByRequestAndState: %w", err)
	}
	return &e, nil
}

func (m *defaultOrderEventModel) FindLastTransitionTo(ctx context.Context, orderNo string,
	toState int32,
) (*OrderEvent, error) {
	if orderNo == "" {
		return nil, ErrOrderNoRequired
	}
	var e OrderEvent
	query := "SELECT " + orderEventColumns + " FROM to_order_event " +
		"WHERE order_no = ? AND to_state = ? ORDER BY event_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &e, query, orderNo, toState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("to_order_event FindLastTransitionTo: %w", err)
	}
	return &e, nil
}
