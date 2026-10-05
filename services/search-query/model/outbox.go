package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SearchOutbox 领域事件 Outbox（search_outbox 表）。
//
// 按 AGENTS.md §5：查询日志写入与事件记录在同一事务内提交，事件由独立发布器投递。
// 消费者按 event_id 去重，失败进入退避重试，超限置 state=Failed 转人工处理。
//
// 注意：本阶段仓库没有可用的消息队列客户端依赖（禁止为此新增 go.mod 依赖），
// 因此只落地“事件已可靠产生”的持久化事实，行停留在 state=待发布。
// 发布器接入前不得声称事件已投递，详见服务 README「已知缺口」。
type SearchOutbox struct {
	Id            int64  `db:"id"`             // 自增主键（发布顺序）
	EventId       string `db:"event_id"`       // 事件唯一 ID（ULID，唯一索引）
	EventType     string `db:"event_type"`     // 事件类型（search.query）
	SchemaVersion int32  `db:"schema_version"` // payload schema 版本
	AggregateType string `db:"aggregate_type"` // 聚合根类型（search_query）
	AggregateId   string `db:"aggregate_id"`   // 聚合根 ID（query_id）
	Payload       string `db:"payload"`        // 事件信封完整 JSON（common/eventenvelope）
	State         int32  `db:"state"`          // 0 待发布、1 已发布、2 失败
	RetryCount    int32  `db:"retry_count"`    // 已重试次数
	NextRetryAt   int64  `db:"next_retry_at"`  // 下次重试时间（Unix 秒，0 立即可投）
	OccurredAt    int64  `db:"occurred_at"`    // 事件发生时间（Unix 秒）
	LastError     string `db:"last_error"`     // 最近一次投递错误
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// SearchOutboxModel search_outbox 表访问接口。
type SearchOutboxModel interface {
	// Insert 在业务事务内写入事件（tx 为 nil 时退化为单连接写入，仅限非事务场景）。
	Insert(ctx context.Context, tx sqlx.Session, out *SearchOutbox) error
	// FindEventIdByAggregate 按聚合根查询已产生的 event_id；不存在返回 ("", nil)。
	// 用于 query_id 重复上报时回填事件 ID，保证调用方幂等可见。
	FindEventIdByAggregate(ctx context.Context, aggregateType, aggregateId string) (string, error)
}

type defaultSearchOutboxModel struct {
	conn sqlx.SqlConn
}

// NewSearchOutboxModel 创建 SearchOutboxModel 实现。
func NewSearchOutboxModel(conn sqlx.SqlConn) SearchOutboxModel {
	return &defaultSearchOutboxModel{conn: conn}
}

const outboxInsert = "INSERT INTO search_outbox (event_id, event_type, schema_version, aggregate_type, aggregate_id, payload, state, retry_count, next_retry_at, occurred_at, last_error, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

func (m *defaultSearchOutboxModel) Insert(ctx context.Context, tx sqlx.Session, out *SearchOutbox) error {
	if out.EventId == "" || out.EventType == "" {
		return fmt.Errorf("search_outbox Insert: empty event_id/event_type")
	}
	now := nowUnix()
	if out.Ctime == 0 {
		out.Ctime = now
	}
	out.Mtime = out.Ctime
	if out.SchemaVersion <= 0 {
		out.SchemaVersion = EventSchemaVersion
	}
	if out.OccurredAt == 0 {
		out.OccurredAt = out.Ctime
	}
	var (
		err     error
		res     sql.Result
		session = tx
	)
	if session == nil {
		session = m.conn
	}
	res, err = session.ExecCtx(ctx, outboxInsert,
		out.EventId, out.EventType, out.SchemaVersion, out.AggregateType, out.AggregateId,
		out.Payload, out.State, out.RetryCount, out.NextRetryAt, out.OccurredAt, out.LastError, out.Ctime, out.Mtime)
	if err != nil {
		return fmt.Errorf("search_outbox Insert: %w", err)
	}
	if out.Id == 0 {
		if id, idErr := res.LastInsertId(); idErr == nil {
			out.Id = id
		}
	}
	return nil
}

func (m *defaultSearchOutboxModel) FindEventIdByAggregate(ctx context.Context, aggregateType, aggregateId string) (string, error) {
	var eventId string
	err := m.conn.QueryRowCtx(ctx, &eventId,
		"SELECT event_id FROM search_outbox WHERE aggregate_type = ? AND aggregate_id = ? ORDER BY id ASC LIMIT 1",
		aggregateType, aggregateId)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("search_outbox FindEventIdByAggregate: %w", err)
	}
	return eventId, nil
}
