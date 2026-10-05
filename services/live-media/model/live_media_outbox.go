package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveMediaOutbox 领域事件 Outbox（live_media_outbox）。
//
// 遵循 AGENTS.md §5：业务写操作与事件记录在同一事务内提交（tx 传入本表方法），
// 由 internal/publisher 按 id 升序投递到 MQ，消费者按 event_id 幂等去重。
// 投递只发生在 Outbox 表里：logic 一律通过 appendOutboxEvent 在同事务写事件行，
// 不得在事务外直接改 state（否则「业务已提交但事件没了结论」无法归因）。
//
// payload 只放事实（主键、状态、时间），禁止写入拉流地址、签名参数或对象存储凭据。
type LiveMediaOutbox struct {
	// ID 自增主键（发布器按此升序保证同一聚合的顺序）
	Id int64 `db:"id"`
	// EventId 事件唯一 ID（ULID，唯一索引，消费者据此幂等）
	EventId string `db:"event_id"`
	// EventType 事件类型，见 EventType* 常量
	EventType string `db:"event_type"`
	// SchemaVersion 事件 schema 版本（Topic = EventType + ".v" + SchemaVersion）
	SchemaVersion int32 `db:"schema_version"`
	// AggregateType 聚合根类型：transcode_task / record_task / record_segment / replay_task / replay_asset_ref / retention_task
	AggregateType string `db:"aggregate_type"`
	// AggregateId 聚合根主键的字符串形式
	AggregateId string `db:"aggregate_id"`
	// RoomId 冗余房间维度：排障时按房间查事件流水，不必解析 payload
	RoomId int64 `db:"room_id"`
	// Payload 事件信封完整 JSON（common/eventenvelope.Envelope）
	Payload string `db:"payload"`
	// State 发布状态：OutboxStatePending / OutboxStatePublished / OutboxStateFailed
	State int32 `db:"state"`
	// RetryCount 已重试次数（指数退避由发布器计算）
	RetryCount int32 `db:"retry_count"`
	// NextRetryAt 下次重试时间（Unix 秒，0 表示可立即投递）
	NextRetryAt int64 `db:"next_retry_at"`
	// LastError 最近一次投递错误（脱敏）
	LastError string `db:"last_error"`
	// OccurredAt 事件发生时间（Unix 秒，与信封 occurred_at 对应）
	OccurredAt int64 `db:"occurred_at"`
	// PublishedAt 投递成功时间（Unix 秒，0 表示未投递）
	PublishedAt int64 `db:"published_at"`
	// TraceId 产生事件的调用 trace_id
	TraceId string `db:"trace_id"`
	Ctime   int64  `db:"ctime"`
	Mtime   int64  `db:"mtime"`
}

// 事件类型常量。Topic 由 common/eventenvelope.Topic() 逐事件推导（类型 + ".v" + schema_version），
// 9 个 topic 已登记进 docs/api-and-events.md §5 并写进 etc/livemedia.v1.yaml 的 PublishTopics。
// 命名必须只含小写字母、数字和点号（eventenvelope.New 会拒绝并让整笔事务回滚），
// 全仓由 common/eventenvelope/event_type_gate_test.go 拦住违规名。
const (
	// EventTypeTranscodeStateChanged 转码任务状态变更
	EventTypeTranscodeStateChanged = "livemedia.transcode.state.changed"
	// EventTypeRecordStateChanged 录制任务生命周期推进（登记 PENDING、停止指令 STOPPING）。
	// 与 record.stopped 的分工：本事件只表达「状态推进了」（审计/排障），
	// record.stopped 才表达「切片清单就绪、回放可拼接」（下游触发点）。
	EventTypeRecordStateChanged = "livemedia.record.state.changed"
	// EventTypeRecordStopped 录制停止（切片清单就绪，回放可拼接）
	EventTypeRecordStopped = "livemedia.record.stopped"
	// EventTypeRecordGapDetected 录制检测到切片缺口
	EventTypeRecordGapDetected = "livemedia.record.gap.detected"
	// EventTypeStreamOutputOnline 分发档位上线
	EventTypeStreamOutputOnline = "livemedia.stream.output.online"
	// EventTypeStreamOutputOffline 分发档位下线（断流/到期/人工）
	EventTypeStreamOutputOffline = "livemedia.stream.output.offline"
	// EventTypeReplayReviewSubmitted 回放已送入普通审核链路（等待 video 结论）
	EventTypeReplayReviewSubmitted = "livemedia.replay.review.submitted"
	// EventTypeReplayContentStateChanged video 侧投影变更（发布/下架/删除）
	EventTypeReplayContentStateChanged = "livemedia.replay.content.state.changed"
	// EventTypeRetentionFinished 回收任务结束（含计数证据）
	EventTypeRetentionFinished = "livemedia.retention.finished"
)

// 聚合根类型常量。
const (
	AggregateTranscodeTask  = "live_transcode_task"
	AggregateStreamOutput   = "live_stream_output"
	AggregateRecordTask     = "live_record_task"
	AggregateRecordSegment  = "live_record_segment"
	AggregateReplayTask     = "live_replay_task"
	AggregateReplayAssetRef = "live_replay_asset_ref"
	AggregateRetentionTask  = "live_retention_task"
)

// LiveMediaOutboxModel live_media_outbox 表接口。
type LiveMediaOutboxModel interface {
	// Insert 写入事件；tx 非空时在同一事务内执行（与业务变更同事务提交），
	// 为 nil 时退化为自动提交。event_id 冲突返回 ErrRequestIdDuplicated 包装错误。
	Insert(ctx context.Context, tx sqlx.Session, e *LiveMediaOutbox) error
	// FindByEventID 按事件 ID 查询（发布器投递前查重），不存在返回 (nil, nil)。
	FindByEventID(ctx context.Context, eventID string) (*LiveMediaOutbox, error)
	// ListPending 到期可投递事件：**只取待发布态**（state = OutboxStatePending）且 next_retry_at 已到，
	// 按 id 升序。失败态（判死）不在此列：判死就是「不再自动投」，把它放回候选集等于没有死信，
	// 每一轮都会重新投递同一条已知坏事件；判死行的处置入口是人工，不是轮询条件。
	// 注意 MarkRetry 会把行留在待发布态并写 next_retry_at，因此重试中的行仍会被本方法取到。
	ListPending(ctx context.Context, now int64, limit int32) ([]*LiveMediaOutbox, error)
	// MarkPublished 标记已发布：条件 UPDATE（state ∈ 待发布/失败），重复标记返回 0 行。
	MarkPublished(ctx context.Context, id, publishedAt int64) (int64, error)
	// MarkRetry 记录本次失败并安排下次重试（仍留在待发布态，retry_count 自增）。
	MarkRetry(ctx context.Context, id, nextRetryAt int64, lastError string) (int64, error)
	// MarkFailed 超过最大重试后转失败（人工处理）：条件 UPDATE，返回受影响行数。
	MarkFailed(ctx context.Context, id int64, lastError string) (int64, error)
	// CountPending 未投递事件数（监控指标）：待发布且已到投递时间，**加上**已判死的行。
	// 与 ListPending 的候选集刻意不同：判死的行发布器不再取，但运维必须看得见它堆了多少。
	// 本方法当前没有调用方（README 已知缺口 4：没有清扫/上报的调度方）。
	CountPending(ctx context.Context, now int64) (int64, error)
	// PurgePublished 清理早于 before（Unix 秒）的已发布事件，返回删除行数。
	PurgePublished(ctx context.Context, before int64, limit int32) (int64, error)
}

const liveMediaOutboxColumns = "SELECT id, event_id, event_type, schema_version, aggregate_type, aggregate_id, " +
	"room_id, payload, state, retry_count, next_retry_at, last_error, occurred_at, published_at, trace_id, ctime, mtime"

type defaultLiveMediaOutboxModel struct {
	conn sqlx.SqlConn
}

// NewLiveMediaOutboxModel 构造 live_media_outbox 的 model。
func NewLiveMediaOutboxModel(conn sqlx.SqlConn) LiveMediaOutboxModel {
	return &defaultLiveMediaOutboxModel{conn: conn}
}

func (m *defaultLiveMediaOutboxModel) Insert(ctx context.Context, tx sqlx.Session, e *LiveMediaOutbox) error {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	if e.EventId == "" || e.EventType == "" || e.AggregateType == "" || e.AggregateId == "" {
		return fmt.Errorf("live_media_outbox Insert: event_id=%q event_type=%q aggregate=%s/%s %w",
			e.EventId, e.EventType, e.AggregateType, e.AggregateId, ErrEmptyEventPayload)
	}
	if e.SchemaVersion <= 0 {
		e.SchemaVersion = 1
	}
	now := nowUnix()
	if e.Ctime == 0 {
		e.Ctime = now
	}
	e.Mtime = e.Ctime
	if e.OccurredAt == 0 {
		e.OccurredAt = e.Ctime
	}
	_, err := session.ExecCtx(ctx,
		"INSERT INTO live_media_outbox (event_id, event_type, schema_version, aggregate_type, aggregate_id, "+
			"room_id, payload, state, retry_count, next_retry_at, last_error, occurred_at, published_at, trace_id, "+
			"ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		e.EventId, e.EventType, e.SchemaVersion, e.AggregateType, e.AggregateId, e.RoomId, e.Payload,
		e.State, e.RetryCount, e.NextRetryAt, e.LastError, e.OccurredAt, e.PublishedAt, e.TraceId,
		e.Ctime, e.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			return fmt.Errorf("event_id=%s: %w", e.EventId, ErrRequestIdDuplicated)
		}
		return fmt.Errorf("live_media_outbox Insert: %w", err)
	}
	return nil
}

func (m *defaultLiveMediaOutboxModel) FindByEventID(ctx context.Context, eventID string) (*LiveMediaOutbox, error) {
	var e LiveMediaOutbox
	query := liveMediaOutboxColumns + " FROM live_media_outbox WHERE event_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &e, query, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_media_outbox FindByEventID: %w", err)
	}
	return &e, nil
}

func (m *defaultLiveMediaOutboxModel) ListPending(ctx context.Context, now int64, limit int32) ([]*LiveMediaOutbox, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []*LiveMediaOutbox
	query := liveMediaOutboxColumns + " FROM live_media_outbox " +
		"WHERE state = ? AND next_retry_at <= ? ORDER BY id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, OutboxStatePending, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_media_outbox ListPending: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveMediaOutboxModel) MarkPublished(ctx context.Context, id, publishedAt int64) (int64, error) {
	if publishedAt <= 0 {
		publishedAt = nowUnix()
	}
	return conditionalUpdate(ctx, m.conn, "live_media_outbox",
		[]columnValue{
			{"state", int32(OutboxStatePublished)},
			{"published_at", publishedAt},
			{"last_error", ""},
		}, false, []whereFragment{
			{"id = ?", []any{id}},
			{"state IN (?, ?)", []any{OutboxStatePending, OutboxStateFailed}},
		})
}

func (m *defaultLiveMediaOutboxModel) MarkRetry(ctx context.Context, id, nextRetryAt int64, lastError string) (int64, error) {
	if nextRetryAt <= 0 {
		nextRetryAt = nowUnix()
	}
	return conditionalUpdate(ctx, m.conn, "live_media_outbox",
		[]columnValue{
			{"state", int32(OutboxStatePending)},
			{"retry_count", rawExpr{"retry_count + 1", nil}},
			{"next_retry_at", nextRetryAt},
			{"last_error", trimLastError(lastError)},
		}, false, []whereFragment{
			{"id = ?", []any{id}},
			{"state IN (?, ?)", []any{OutboxStatePending, OutboxStateFailed}},
		})
}

func (m *defaultLiveMediaOutboxModel) MarkFailed(ctx context.Context, id int64, lastError string) (int64, error) {
	return conditionalUpdate(ctx, m.conn, "live_media_outbox",
		[]columnValue{
			{"state", int32(OutboxStateFailed)},
			{"last_error", trimLastError(lastError)},
		}, false, []whereFragment{
			{"id = ?", []any{id}},
			{"state = ?", []any{OutboxStatePending}},
		})
}

// trimLastError 把投递错误夹进 last_error 列（VARCHAR(512)，MySQL 按字符计数，
// 因此夹到 512 字节必然不越界），并且绝不切断 UTF-8 序列。
//
// 为什么这条不能省：本仓库的发布错误串是中文（publisher 的 label 与包装文案都是中文），
// 按字节硬切会在多字节字符中间留下半个序列，而 utf8mb4 列在严格模式下直接拒绝非法序列。
// 后果不是「日志少几个字」：MarkRetry/MarkFailed 本身报错 → 发布引擎中断整批，
// 同一批次里其余本来正常的行也拿不到结论，故障面从「一条事件投递失败」放大成「这一轮全无进展」。
func trimLastError(s string) string {
	const maxBytes = 512
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	// 0x80..0xBF 是 UTF-8 续字节：从切点往前退到首个非续字节，保证 s[:cut] 以完整字符结尾。
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func (m *defaultLiveMediaOutboxModel) CountPending(ctx context.Context, now int64) (int64, error) {
	var n int64
	query := "SELECT COUNT(*) FROM live_media_outbox WHERE state IN (?, ?) AND next_retry_at <= ?"
	if err := m.conn.QueryRowCtx(ctx, &n, query, OutboxStatePending, OutboxStateFailed, now); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_media_outbox CountPending: %w", err)
	}
	return n, nil
}

func (m *defaultLiveMediaOutboxModel) PurgePublished(ctx context.Context, before int64, limit int32) (int64, error) {
	if limit <= 0 {
		limit = 500
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM live_media_outbox WHERE state = ? AND published_at > 0 AND published_at < ? LIMIT ?",
		OutboxStatePublished, before, limit)
	if err != nil {
		return 0, fmt.Errorf("live_media_outbox PurgePublished: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_media_outbox PurgePublished RowsAffected: %w", err)
	}
	return aff, nil
}
