package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 死信处置状态：与 rpc/notification.proto 的 DeadLetterState 一致。
const (
	// DeadLetterStatePending 待运营处理。
	DeadLetterStatePending int32 = 1
	// DeadLetterStateRetried 已被重投（终态）。
	DeadLetterStateRetried int32 = 2
	// DeadLetterStateDiscarded 已丢弃（终态）。
	DeadLetterStateDiscarded int32 = 3
)

// 死信来源。
const (
	// DeadLetterSourceEvent MQ 事件死信：原始报文只留摘要，重投需由生产者按 event_id 重放 Outbox。
	DeadLetterSourceEvent = "event"
	// DeadLetterSourceDelivery 投递任务死信：任务与渲染变量仍在 notification_delivery，可直接重投。
	DeadLetterSourceDelivery = "delivery"
)

// NotificationDeadLetter 死信留档（notification_dead_letter 表）。
// 隐私：不存原始报文，只存 payload_digest 与错误原因，避免敏感内容长期留档。
type NotificationDeadLetter struct {
	Id            int64  `db:"id"`             // 自增主键
	EventId       string `db:"event_id"`       // 事件 ID（来源为事件时必填）
	EventType     string `db:"event_type"`     // 事件类型
	Topic         string `db:"topic"`          // 来源 topic；RPC 直投写 "rpc:send"
	Source        string `db:"source"`         // 来源：event|delivery
	DeliveryId    string `db:"delivery_id"`    // 关联投递任务 ID（source=delivery 时必填）
	PayloadDigest string `db:"payload_digest"` // 原始报文/渲染结果摘要
	Reason        string `db:"reason"`         // 死信原因（脱敏）
	State         int32  `db:"state"`          // 处置状态，见 DeadLetterState* 常量
	Operator      string `db:"operator"`       // 处置人
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// DeadLetterFilter 死信列表过滤条件。
type DeadLetterFilter struct {
	EventId string
	Topic   string
	Source  string
	State   int32
}

// NotificationDeadLetterModel notification_dead_letter 表读写接口。
type NotificationDeadLetterModel interface {
	// InsertIfAbsent 幂等登记死信；返回 false 表示同一来源死信已登记。
	InsertIfAbsent(ctx context.Context, d *NotificationDeadLetter) (bool, error)
	// FindOne 按主键查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, id int64) (*NotificationDeadLetter, error)
	// FindByDelivery 查询某投递任务的死信记录；不存在返回 (nil, nil)。
	FindByDelivery(ctx context.Context, deliveryId string) (*NotificationDeadLetter, error)
	// MarkState 带源状态守卫地更新处置状态。
	MarkState(ctx context.Context, id int64, to int32, operator string, from []int32) (bool, error)
	// List 分页查询死信，按 ctime 降序。
	List(ctx context.Context, f DeadLetterFilter, pn, ps int32) ([]*NotificationDeadLetter, int64, error)
}

type defaultNotificationDeadLetterModel struct {
	conn sqlx.SqlConn
}

// NewNotificationDeadLetterModel 创建 NotificationDeadLetterModel 实现。
func NewNotificationDeadLetterModel(conn sqlx.SqlConn) NotificationDeadLetterModel {
	return &defaultNotificationDeadLetterModel{conn: conn}
}

const deadLetterCols = "id, event_id, event_type, topic, source, delivery_id, payload_digest, reason, state, operator, ctime, mtime"

func (m *defaultNotificationDeadLetterModel) InsertIfAbsent(ctx context.Context, d *NotificationDeadLetter) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT IGNORE INTO notification_dead_letter (event_id, event_type, topic, source, delivery_id, payload_digest, reason, state, operator, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		d.EventId, d.EventType, d.Topic, d.Source, d.DeliveryId, d.PayloadDigest, d.Reason, d.State, d.Operator, d.Ctime, d.Mtime)
	if err != nil {
		return false, fmt.Errorf("notification_dead_letter InsertIfAbsent: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("notification_dead_letter InsertIfAbsent RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultNotificationDeadLetterModel) FindOne(ctx context.Context, id int64) (*NotificationDeadLetter, error) {
	var d NotificationDeadLetter
	err := m.conn.QueryRowCtx(ctx, &d, "SELECT "+deadLetterCols+" FROM notification_dead_letter WHERE id = ? LIMIT 1", id)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("notification_dead_letter FindOne: %w", err)
	}
	return &d, nil
}

func (m *defaultNotificationDeadLetterModel) FindByDelivery(ctx context.Context, deliveryId string) (*NotificationDeadLetter, error) {
	if deliveryId == "" {
		return nil, nil
	}
	var d NotificationDeadLetter
	err := m.conn.QueryRowCtx(ctx, &d,
		"SELECT "+deadLetterCols+" FROM notification_dead_letter WHERE delivery_id = ? AND source = ? ORDER BY id DESC LIMIT 1",
		deliveryId, DeadLetterSourceDelivery)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("notification_dead_letter FindByDelivery: %w", err)
	}
	return &d, nil
}

func (m *defaultNotificationDeadLetterModel) MarkState(ctx context.Context, id int64, to int32, operator string, from []int32) (bool, error) {
	if len(from) == 0 {
		return false, ErrIllegalStateTransition
	}
	q := "UPDATE notification_dead_letter SET state = ?, operator = ?, mtime = ? WHERE id = ? AND state IN (" + placeholders(len(from)) + ")"
	args := append([]any{to, operator, nowUnix(), id}, int32Args(from)...)
	res, err := m.conn.ExecCtx(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("notification_dead_letter MarkState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("notification_dead_letter MarkState RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultNotificationDeadLetterModel) List(ctx context.Context, f DeadLetterFilter, pn, ps int32) ([]*NotificationDeadLetter, int64, error) {
	if ps > 100 {
		return nil, 0, ErrPsTooLarge
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = 20
	}
	where := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if f.EventId != "" {
		where = append(where, "event_id = ?")
		args = append(args, f.EventId)
	}
	if f.Topic != "" {
		where = append(where, "topic = ?")
		args = append(args, f.Topic)
	}
	if f.Source != "" {
		where = append(where, "source = ?")
		args = append(args, f.Source)
	}
	if f.State != 0 {
		where = append(where, "state = ?")
		args = append(args, f.State)
	}
	cond := joinAnd(where)

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM notification_dead_letter WHERE "+cond, args...); err != nil {
		return nil, 0, fmt.Errorf("notification_dead_letter List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*NotificationDeadLetter
	q := "SELECT " + deadLetterCols + " FROM notification_dead_letter WHERE " + cond + " ORDER BY ctime DESC, id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, q, listArgs...); err != nil {
		if isNoRows(err) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("notification_dead_letter List: %w", err)
	}
	return rows, total, nil
}
