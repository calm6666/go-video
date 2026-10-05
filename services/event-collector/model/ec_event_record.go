package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// EventRecord 单条事件的接收与投递台账（ec_event_record 表）。
//
// 定位：这是「采集台账」，不是行为事实表——下游分析必须消费 MQ topic，
// 本表只用于排障、去重判定、投递推进与审计（proto 注释同义）。
//
// 幂等：uniq_event_id(event_id) 是逐条去重的唯一落点。同一 event_id 第二次上报
// 只累加批次计数并把本条判 DUPLICATED，不新增行、不重复投递。
// 隐私：payload 只存 payload_digest + payload_bytes；搜索词只存 keyword_digest + 长度；
// 超大事件正文归档到对象存储时也只存 blob_bucket/blob_object_key 引用（不含密钥）。
// 明文 IP/设备号/手机号一律不落库，只有加盐哈希与 /24 段。
type EventRecord struct {
	ID int64 `db:"id"`
	// EventID 事件全局唯一键（客户端生成，服务端按其去重）
	EventID string `db:"event_id"`
	// BatchID 所属批次（ec_ingest_batch.batch_id）
	BatchID string `db:"batch_id"`
	// EventType 归一化后的事件类型，如 behavior.play
	EventType string `db:"event_type"`
	// Category 行为类别（rpc.BehaviorCategory 取值）
	Category int32 `db:"category"`
	// SchemaVersion 服务端实际采用的事件结构版本
	SchemaVersion int32 `db:"schema_version"`
	// OccurredAt 客户端上报的事件发生时间（Unix 秒）
	OccurredAt int64 `db:"occurred_at"`
	// ReceivedAt 服务端接收时间（Unix 秒）
	ReceivedAt int64 `db:"received_at"`
	// ClockSkewSeconds occurred_at 与服务器时间的偏差（有符号），识别时钟漂移
	ClockSkewSeconds int64 `db:"clock_skew_seconds"`
	// Decision 处理结论，见 Decision*
	Decision int32 `db:"decision"`
	// Reason 拒绝/丢弃原因码（rpc.RejectReason 取值，1 = 无问题）
	Reason int32 `db:"reason"`
	// ReasonDetail 已脱敏的补充说明（禁止写 payload 原文）
	ReasonDetail string `db:"reason_detail"`
	// DeliveryState 投递状态，见 DeliveryState*
	DeliveryState int32 `db:"delivery_state"`
	// Topic 投递目标 topic（未投递为空）
	Topic string `db:"topic"`
	// EnvelopeEventID 投递信封的 event_id（与入参 event_id 分开记账）
	EnvelopeEventID string `db:"envelope_event_id"`
	// DeliveryAttempts 已尝试投递次数
	DeliveryAttempts int32 `db:"delivery_attempts"`
	// NextRetryAt 下次重试时间（Unix 秒，0 = 不需要）
	NextRetryAt int64 `db:"next_retry_at"`
	// LastError 最近一次投递失败的脱敏摘要
	LastError string `db:"last_error"`
	// Mid 归属用户（0 = 未登录）
	Mid int64 `db:"mid"`
	// DeviceHash 加盐设备哈希（非明文）
	DeviceHash string `db:"device_hash"`
	// IPSegment 脱敏 IP 段（非明文）
	IPSegment string `db:"ip_segment"`
	// SaltVersion 本次哈希使用的盐版本
	SaltVersion int32 `db:"salt_version"`
	// ContentType 内容主类型（ugc/pgc/live/keyword...）
	ContentType string `db:"content_type"`
	// ContentID 内容主键（catalog 作品/集 ID）
	ContentID int64 `db:"content_id"`
	// Aid 稿件 aid（UGC 场景）
	Aid int64 `db:"aid"`
	// Vid 稿件 vid（UGC 场景）
	Vid string `db:"vid"`
	// SessionID 播放/会话主键
	SessionID string `db:"session_id"`
	// TargetMid 行为对象用户（关注/分享），0 = 无
	TargetMid int64 `db:"target_mid"`
	// KeywordDigest 搜索词摘要（sha256:<hex>），原文不入库
	KeywordDigest string `db:"keyword_digest"`
	// KeywordRunes 截断后搜索词长度（rune 数）
	KeywordRunes int32 `db:"keyword_runes"`
	// PayloadDigest 事件正文摘要（sha256:<hex>）
	PayloadDigest string `db:"payload_digest"`
	// PayloadBytes 事件正文字节数
	PayloadBytes int32 `db:"payload_bytes"`
	// BlobBucket 超大正文归档的对象存储桶（仅引用；空 = 未归档）
	BlobBucket string `db:"blob_bucket"`
	// BlobObjectKey 对象存储 object key（不含任何凭据、不是可公开访问 URL）
	BlobObjectKey string `db:"blob_object_key"`
	// SanitizeVersion 实际生效的脱敏规则版本
	SanitizeVersion string `db:"sanitize_version"`
	// PolicyVersion 实际生效的采样/脱敏策略版本（归因依据）
	PolicyVersion string `db:"policy_version"`
	// TraceID 链路 ID
	TraceID string `db:"trace_id"`
	Ctime   int64  `db:"ctime"`
	Mtime   int64  `db:"mtime"`
}

// EventRecordModel ec_event_record 读写接口。
type EventRecordModel interface {
	// Insert 写入单条台账；命中 uniq_event_id 返回 (首行 ID, false, nil) 供上层判 DUPLICATED。
	Insert(ctx context.Context, session sqlx.Session, r *EventRecord) (id int64, created bool, err error)
	// InsertIgnoreMany 多行幂等写入，返回实际新增行数（已存在的行静默跳过）。
	InsertIgnoreMany(ctx context.Context, session sqlx.Session, rows []*EventRecord) (inserted int64, err error)
	// FindByEventID 查询台账；不存在返回 ErrRecordNotFound。
	FindByEventID(ctx context.Context, eventID string) (*EventRecord, error)
	// ListByEventIDs 批量回查（批次去重预检，单次上限 maxStringIDList）。
	ListByEventIDs(ctx context.Context, eventIDs []string) (map[string]*EventRecord, error)
	// MarkDelivery 投递状态迁移（台账投影；真值在 ec_pending_delivery）。
	MarkDelivery(ctx context.Context, session sqlx.Session, eventID string, from []int32, to int32,
		topic string, attempts int32, nextRetryAt int64, lastError string) (applied bool, err error)
	// List 按 (ctime, id) 倒序游标翻页。
	List(ctx context.Context, f EventRecordFilter, cur Cursor, ps int32) ([]*EventRecord, error)
	// Count 同条件计数。
	Count(ctx context.Context, f EventRecordFilter) (int64, error)
	// CountReasonSince 统计时间窗内某原因码条数（限流量、拒绝分布，供健康度）。
	CountReasonSince(ctx context.Context, reason int32, since int64) (int64, error)
	// CountOpenSince 统计某 topic 仍开放（待投递/退避）的台账条数与最早时间。
	CountOpenSince(ctx context.Context, since int64) (int64, error)
	// CountOpenByBatch 统计批次内仍未收敛（待投递/退避）的台账条数，
	// 供「批次是否已全部投递」判定用：走 idx_batch_ctime 前缀，
	// 不扫 ec_pending_delivery（该表没有 batch_id 索引，按批次过滤会退化成全表扫）。
	CountOpenByBatch(ctx context.Context, batchID string) (int64, error)
	// DeleteBefore 按留存策略清理台账。
	DeleteBefore(ctx context.Context, ctimeBefore int64, limit int32) (int64, error)
	// WithSession 绑定事务句柄。
	WithSession(session sqlx.Session) EventRecordModel
}

// EventRecordFilter 台账翻页条件；零值字段表示不加该条件。
type EventRecordFilter struct {
	BatchID       string
	EventType     string
	Category      int32
	Decision      int32
	Reason        int32
	DeliveryState int32
	Topic         string
	Mid           int64
	DeviceHash    string
	CtimeFrom     int64
	CtimeTo       int64
}

type defaultEventRecordModel struct {
	conn sqlx.SqlConn
}

// NewEventRecordModel 创建 EventRecordModel 实现。
func NewEventRecordModel(conn sqlx.SqlConn) EventRecordModel {
	return &defaultEventRecordModel{conn: conn}
}

func (m *defaultEventRecordModel) WithSession(session sqlx.Session) EventRecordModel {
	return &defaultEventRecordModel{conn: pick(session, m.conn)}
}

const eventRecordColumns = `id, event_id, batch_id, event_type, category, schema_version, occurred_at, received_at,
	clock_skew_seconds, decision, reason, reason_detail, delivery_state, topic, envelope_event_id, delivery_attempts,
	next_retry_at, last_error, mid, device_hash, ip_segment, salt_version, content_type, content_id, aid, vid,
	session_id, target_mid, keyword_digest, keyword_runes, payload_digest, payload_bytes, blob_bucket, blob_object_key,
	sanitize_version, policy_version, trace_id, ctime, mtime`

const eventRecordPlaceholdersPerRow = 38

func (m *defaultEventRecordModel) Insert(ctx context.Context, session sqlx.Session, r *EventRecord) (int64, bool, error) {
	if r == nil || r.EventID == "" || len(r.EventID) > 64 {
		return 0, false, ErrEventIDRequired
	}
	if r.BatchID == "" {
		return 0, false, ErrBatchIDRequired
	}
	db := pick(session, m.conn)
	one, err := m.InsertIgnoreMany(ctx, db, []*EventRecord{r})
	if err != nil {
		return 0, false, err
	}
	if one == 0 {
		exist, qerr := m.FindByEventID(ctx, r.EventID)
		if qerr != nil {
			return 0, false, qerr
		}
		return exist.ID, false, nil
	}
	var id int64
	err = db.QueryRowCtx(ctx, &id, "SELECT id FROM ec_event_record WHERE event_id = ? LIMIT 1", r.EventID)
	if err != nil {
		return 0, false, fmt.Errorf("ec_event_record Insert LastInsertId: %w", err)
	}
	return id, true, nil
}

func (m *defaultEventRecordModel) InsertIgnoreMany(ctx context.Context, session sqlx.Session,
	rows []*EventRecord) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if len(rows) > maxStringIDList {
		return 0, ErrBatchTooLarge
	}
	for _, r := range rows {
		if r == nil || r.EventID == "" {
			return 0, ErrEventIDRequired
		}
		if r.BatchID == "" {
			return 0, ErrBatchIDRequired
		}
	}
	db := pick(session, m.conn)
	now := nowUnix()
	args := make([]any, 0, len(rows)*eventRecordPlaceholdersPerRow)
	values := make([]string, 0, len(rows))
	group := "(" + placeholders(eventRecordPlaceholdersPerRow) + ")"
	for _, r := range rows {
		if r.Ctime == 0 {
			r.Ctime, r.Mtime = now, now
		}
		if r.ReceivedAt == 0 {
			r.ReceivedAt = now
		}
		args = append(args,
			r.EventID, r.BatchID, r.EventType, r.Category, r.SchemaVersion, r.OccurredAt, r.ReceivedAt,
			r.ClockSkewSeconds, r.Decision, r.Reason, truncate(r.ReasonDetail, 512), r.DeliveryState, r.Topic,
			r.EnvelopeEventID, r.DeliveryAttempts, r.NextRetryAt, truncate(r.LastError, 512), r.Mid, r.DeviceHash,
			r.IPSegment, r.SaltVersion, r.ContentType, r.ContentID, r.Aid, r.Vid, r.SessionID, r.TargetMid,
			r.KeywordDigest, r.KeywordRunes, r.PayloadDigest, r.PayloadBytes, r.BlobBucket, r.BlobObjectKey,
			r.SanitizeVersion, r.PolicyVersion, truncate(r.TraceID, 64), r.Ctime, r.Mtime)
		values = append(values, group)
	}
	query := "INSERT IGNORE INTO ec_event_record (event_id, batch_id, event_type, category, schema_version, " +
		"occurred_at, received_at, clock_skew_seconds, decision, reason, reason_detail, delivery_state, topic, " +
		"envelope_event_id, delivery_attempts, next_retry_at, last_error, mid, device_hash, ip_segment, salt_version, " +
		"content_type, content_id, aid, vid, session_id, target_mid, keyword_digest, keyword_runes, payload_digest, " +
		"payload_bytes, blob_bucket, blob_object_key, sanitize_version, policy_version, trace_id, ctime, mtime) VALUES " +
		strings.Join(values, ",")
	res, err := db.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("ec_event_record InsertIgnoreMany: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ec_event_record InsertIgnoreMany RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultEventRecordModel) FindByEventID(ctx context.Context, eventID string) (*EventRecord, error) {
	if eventID == "" {
		return nil, ErrEventIDRequired
	}
	var row EventRecord
	err := m.conn.QueryRowCtx(ctx, &row,
		"SELECT "+eventRecordColumns+" FROM ec_event_record WHERE event_id = ? LIMIT 1", eventID)
	if err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, ErrRecordNotFound
		}
		return nil, fmt.Errorf("ec_event_record FindByEventID: %w", err)
	}
	return &row, nil
}

func (m *defaultEventRecordModel) ListByEventIDs(ctx context.Context, eventIDs []string) (map[string]*EventRecord, error) {
	out := make(map[string]*EventRecord, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}
	if len(eventIDs) > maxStringIDList {
		return nil, ErrBatchTooLarge
	}
	args := make([]any, 0, len(eventIDs))
	for _, id := range eventIDs {
		if id == "" {
			return nil, ErrEventIDRequired
		}
		args = append(args, id)
	}
	query := "SELECT " + eventRecordColumns + " FROM ec_event_record WHERE event_id IN (" +
		placeholders(len(eventIDs)) + ")"
	var rows []*EventRecord
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return out, nil
		}
		return nil, fmt.Errorf("ec_event_record ListByEventIDs: %w", err)
	}
	for _, r := range rows {
		out[r.EventID] = r
	}
	return out, nil
}

func (m *defaultEventRecordModel) MarkDelivery(ctx context.Context, session sqlx.Session, eventID string,
	from []int32, to int32, topic string, attempts int32, nextRetryAt int64, lastError string) (bool, error) {
	if eventID == "" {
		return false, ErrEventIDRequired
	}
	if !ValidDeliveryState(to) {
		return false, ErrInvalidStateTransition
	}
	if len(from) == 0 {
		return false, ErrInvalidStateTransition
	}
	db := pick(session, m.conn)
	args := make([]any, 0, len(from)+6)
	args = append(args, to, truncate(topic, 128), attempts, nextRetryAt, truncate(lastError, 512), nowUnix())
	query := "UPDATE ec_event_record SET delivery_state = ?, topic = ?, delivery_attempts = ?, next_retry_at = ?, " +
		"last_error = ?, mtime = ? WHERE event_id = ? AND delivery_state IN (" + placeholders(len(from)) + ")"
	for _, f := range from {
		args = append(args, f)
	}
	args = append(args, eventID)
	res, err := db.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("ec_event_record MarkDelivery: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_event_record MarkDelivery RowsAffected: %w", err)
	}
	return n > 0, nil
}

func (m *defaultEventRecordModel) where(f EventRecordFilter) (string, []any) {
	conds := make([]string, 0, 11)
	args := make([]any, 0, 11)
	if f.BatchID != "" {
		conds = append(conds, "batch_id = ?")
		args = append(args, f.BatchID)
	}
	if f.EventType != "" {
		conds = append(conds, "event_type = ?")
		args = append(args, f.EventType)
	}
	if f.Category != 0 {
		conds = append(conds, "category = ?")
		args = append(args, f.Category)
	}
	if f.Decision != 0 {
		conds = append(conds, "decision = ?")
		args = append(args, f.Decision)
	}
	if f.Reason != 0 {
		conds = append(conds, "reason = ?")
		args = append(args, f.Reason)
	}
	if f.DeliveryState != 0 {
		conds = append(conds, "delivery_state = ?")
		args = append(args, f.DeliveryState)
	}
	if f.Topic != "" {
		conds = append(conds, "topic = ?")
		args = append(args, f.Topic)
	}
	if f.Mid != 0 {
		conds = append(conds, "mid = ?")
		args = append(args, f.Mid)
	}
	if f.DeviceHash != "" {
		conds = append(conds, "device_hash = ?")
		args = append(args, f.DeviceHash)
	}
	if f.CtimeFrom > 0 {
		conds = append(conds, "ctime >= ?")
		args = append(args, f.CtimeFrom)
	}
	if f.CtimeTo > 0 {
		conds = append(conds, "ctime <= ?")
		args = append(args, f.CtimeTo)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (m *defaultEventRecordModel) List(ctx context.Context, f EventRecordFilter, cur Cursor,
	ps int32) ([]*EventRecord, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	where, args := m.where(f)
	if cur.ID > 0 {
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += "(ctime < ? OR (ctime = ? AND id < ?))"
		args = append(args, cur.Ctime, cur.Ctime, cur.ID)
	}
	var rows []*EventRecord
	query := "SELECT " + eventRecordColumns + " FROM ec_event_record" + where +
		" ORDER BY ctime DESC, id DESC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, append(args, ps)...); err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ec_event_record List: %w", err)
	}
	return rows, nil
}

func (m *defaultEventRecordModel) Count(ctx context.Context, f EventRecordFilter) (int64, error) {
	where, args := m.where(f)
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(1) FROM ec_event_record"+where, args...); err != nil {
		return 0, fmt.Errorf("ec_event_record Count: %w", err)
	}
	return total, nil
}

func (m *defaultEventRecordModel) CountReasonSince(ctx context.Context, reason int32, since int64) (int64, error) {
	if !ValidReason(reason) {
		return 0, ErrInvalidStateTransition
	}
	var total int64
	err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(1) FROM ec_event_record WHERE reason = ? AND ctime >= ?", reason, since)
	if err != nil {
		return 0, fmt.Errorf("ec_event_record CountReasonSince: %w", err)
	}
	return total, nil
}

func (m *defaultEventRecordModel) CountOpenSince(ctx context.Context, since int64) (int64, error) {
	var total int64
	err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(1) FROM ec_event_record WHERE delivery_state IN (?, ?) AND ctime >= ?",
		DeliveryStatePending, DeliveryStateRetrying, since)
	if err != nil {
		return 0, fmt.Errorf("ec_event_record CountOpenSince: %w", err)
	}
	return total, nil
}

func (m *defaultEventRecordModel) CountOpenByBatch(ctx context.Context, batchID string) (int64, error) {
	if batchID == "" {
		return 0, ErrBatchIDRequired
	}
	var total int64
	err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(1) FROM ec_event_record WHERE batch_id = ? AND delivery_state IN (?, ?)",
		batchID, DeliveryStatePending, DeliveryStateRetrying)
	if err != nil {
		return 0, fmt.Errorf("ec_event_record CountOpenByBatch: %w", err)
	}
	return total, nil
}

func (m *defaultEventRecordModel) DeleteBefore(ctx context.Context, ctimeBefore int64, limit int32) (int64, error) {
	if ctimeBefore <= 0 {
		return 0, ErrInvalidPage
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > maxIDList {
		return 0, ErrBatchLimitTooLarge
	}
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM ec_event_record WHERE ctime < ? LIMIT ?", ctimeBefore, limit)
	if err != nil {
		return 0, fmt.Errorf("ec_event_record DeleteBefore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ec_event_record DeleteBefore RowsAffected: %w", err)
	}
	return n, nil
}
