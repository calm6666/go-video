package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 来源，与 rpc.Source、ec_ingest_batch.source 一致。
const (
	// SourceClient 客户端 SDK 批量上报（可采样）。
	SourceClient int32 = 1
	// SourceServer 服务端内部埋点（不采样、trace_id 必填）。
	SourceServer int32 = 2
)

// ValidSource 判定来源取值合法（0 = UNSPECIFIED 由 logic 归一化成 SourceClient）。
func ValidSource(s int32) bool { return s == SourceClient || s == SourceServer }

// IngestBatch 一次批量上报的接收台账（ec_ingest_batch 表）。
//
// 幂等：uniq_batch_id(batch_id) 是采集幂等的唯一落点。重复上报不新增行，
// logic 回读首行的计数与结论直接回放，避免「客户端重试 = 事件翻倍」。
// 隐私：只存 device_hash / ip_segment / salt_version，绝不含明文设备号或出口 IP。
// 计数列（total/accepted/.../dispatched/dead）都是投影：
// 与 ec_event_record 同事务维护，并可用 RecountFromRecords 全量重算，不作为唯一事实源。
type IngestBatch struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// BatchID 客户端/调用方生成的批次幂等键
	BatchID string `db:"batch_id"`
	// Source 来源：1 客户端、2 服务端
	Source int32 `db:"source"`
	// CallerService 服务端来源的服务名（信封 producer）
	CallerService string `db:"caller_service"`
	// IdempotencyKey 服务端来源的动作幂等键（客户端来源为空）
	IdempotencyKey string `db:"idempotency_key"`
	// Platform 客户端平台
	Platform int32 `db:"platform"`
	// AppID 应用标识
	AppID string `db:"app_id"`
	// AppVersion 客户端版本
	AppVersion string `db:"app_version"`
	// SdkVersion 埋点 SDK 版本
	SdkVersion string `db:"sdk_version"`
	// Mid 登录用户（0 = 未登录）
	Mid int64 `db:"mid"`
	// DeviceHash 加盐哈希后的设备标识（非明文）
	DeviceHash string `db:"device_hash"`
	// IPSegment 脱敏 IP 段（非明文）
	IPSegment string `db:"ip_segment"`
	// SaltVersion 本次使用的盐版本
	SaltVersion int32 `db:"salt_version"`
	// PolicyVersion 本次采用的采样/脱敏策略版本（事后归因依据）
	PolicyVersion string `db:"policy_version"`
	// Total 批次内事件条数
	Total int32 `db:"total"`
	// Accepted 通过校验并落库的条数（投影，可重算）
	Accepted int32 `db:"accepted"`
	// Duplicated event_id 已存在的条数（投影，可重算）
	Duplicated int32 `db:"duplicated"`
	// Rejected 校验被拒的条数（投影，可重算）
	Rejected int32 `db:"rejected"`
	// SampledOut 命中采样丢弃的条数（投影，可重算）
	SampledOut int32 `db:"sampled_out"`
	// Dispatched 已投递到 MQ 的条数（投影，可重算）
	Dispatched int32 `db:"dispatched"`
	// Dead 转死信的条数（投影，可重算）
	Dead int32 `db:"dead"`
	// RequestBytes 请求体字节数（容量与攻击面观测）
	RequestBytes int64 `db:"request_bytes"`
	// State 批次状态，见 BatchState*
	State int32 `db:"state"`
	// TopReason 整批最主要的拒绝原因（1 = 无问题）
	TopReason int32 `db:"top_reason"`
	// ClientSeq 客户端自增序号（识别乱序/重发批次）
	ClientSeq int32 `db:"client_seq"`
	// RequestID 传输层请求 ID（排障）
	RequestID string `db:"request_id"`
	// LastError 最近一次失败的脱敏摘要
	LastError string `db:"last_error"`
	// TraceID 批次级链路 ID
	TraceID string `db:"trace_id"`
	// ReceivedAt 接收时刻（Unix 秒）
	ReceivedAt int64 `db:"received_at"`
	// FinishedAt 投递收敛时刻（Unix 秒，0 = 未收敛）
	FinishedAt int64 `db:"finished_at"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// IngestBatchModel ec_ingest_batch 读写接口。
type IngestBatchModel interface {
	// Insert 写入批次行；命中 uniq_batch_id 时返回 (首行 ID, false, nil)，供 logic 回放首次结果。
	Insert(ctx context.Context, session sqlx.Session, b *IngestBatch) (id int64, created bool, err error)
	// FindByBatchID 按幂等键查询；不存在返回 ErrBatchNotFound。
	FindByBatchID(ctx context.Context, batchID string) (*IngestBatch, error)
	// Accumulate 累加校验结论计数（同事务调用，避免整批覆盖导致并发丢更新）。
	Accumulate(ctx context.Context, session sqlx.Session, batchID string,
		total, accepted, duplicated, rejected, sampledOut int32) error
	// UpdateAttribution 刷新整批被拒后重驱动时的归因列（策略版本/盐版本/请求字节）。
	// 只在 logic 重驱动 state=REJECTED 批次时同事务调用：一次上报实际生效的策略与盐版本
	// 必须落在行上，否则事后按旧策略版本解释这批数据会得出相反结论。
	UpdateAttribution(ctx context.Context, session sqlx.Session, batchID, policyVersion string,
		saltVersion int32, requestBytes int64) error
	// MarkState 状态机迁移：当前态必须在 from 集合内；applied=false 表示并发或非法迁移。
	MarkState(ctx context.Context, session sqlx.Session, batchID string,
		from []int32, to int32, topReason int32, lastError string) (applied bool, err error)
	// MarkDispatchProgress 累加投递投影（dispatcher 与 RetryPendingDelivery 共用）。
	MarkDispatchProgress(ctx context.Context, session sqlx.Session, batchID string,
		dispatchedDelta, deadDelta int32) (applied bool, err error)
	// Finish 批次收敛：仅当已无待投递事件时写 finished_at。
	Finish(ctx context.Context, session sqlx.Session, batchID string) (applied bool, err error)
	// RecountFromRecords 从 ec_event_record 重算批次计数投影（运维/对账任务）。
	RecountFromRecords(ctx context.Context, session sqlx.Session, batchID string) error
	// List 按 (ctime, id) 倒序游标翻页。
	List(ctx context.Context, f IngestBatchFilter, cur Cursor, ps int32) ([]*IngestBatch, error)
	// Count 同条件计数（分页 total，允许轻微滞后）。
	Count(ctx context.Context, f IngestBatchFilter) (int64, error)
	// CountRejectedSince 统计时间窗内整批被拒数量（GetCollectorHealth）。
	CountRejectedSince(ctx context.Context, since int64) (int64, error)
	// CountTopReasonSince 统计时间窗内以某原因码整批被拒的数量
	// （GetCollectorHealth.rate_limited_last_hour：整批限流不写事件台账，只能从批次台账取）。
	CountTopReasonSince(ctx context.Context, topReason int32, since int64) (int64, error)
	// DeleteBefore 按留存策略清理批次台账，返回删除行数。
	DeleteBefore(ctx context.Context, ctimeBefore int64, limit int32) (int64, error)
	// WithSession 绑定事务句柄。
	WithSession(session sqlx.Session) IngestBatchModel
}

// IngestBatchFilter 批次翻页条件；零值字段表示不加该条件。
type IngestBatchFilter struct {
	Source     int32
	State      int32
	Mid        int64
	DeviceHash string
	IPSegment  string
	CtimeFrom  int64
	CtimeTo    int64
}

type defaultIngestBatchModel struct {
	conn sqlx.SqlConn
}

// NewIngestBatchModel 创建 IngestBatchModel 实现。
func NewIngestBatchModel(conn sqlx.SqlConn) IngestBatchModel {
	return &defaultIngestBatchModel{conn: conn}
}

func (m *defaultIngestBatchModel) WithSession(session sqlx.Session) IngestBatchModel {
	return &defaultIngestBatchModel{conn: pick(session, m.conn)}
}

const ingestBatchColumns = `id, batch_id, source, caller_service, idempotency_key, platform, app_id, app_version,
	sdk_version, mid, device_hash, ip_segment, salt_version, policy_version, total, accepted, duplicated, rejected,
	sampled_out, dispatched, dead, request_bytes, state, top_reason, client_seq, request_id, last_error, trace_id,
	received_at, finished_at, ctime, mtime`

// ingestBatchInsertValues 是 Insert 的列数（不含自增主键 id），与列清单逐列对应。
const ingestBatchInsertValues = 31

func (m *defaultIngestBatchModel) Insert(ctx context.Context, session sqlx.Session, b *IngestBatch) (int64, bool, error) {
	// 入参校验先于 SQL：batch_id 是唯一幂等键，留空会让不同批次撞在同一空串行上。
	if b == nil {
		return 0, false, ErrBatchIDRequired
	}
	if b.BatchID == "" || len(b.BatchID) > 64 {
		return 0, false, ErrBatchIDRequired
	}
	if b.Source == 0 {
		b.Source = SourceClient
	}
	if !ValidSource(b.Source) {
		return 0, false, ErrSourceNotAllowed(b.Source)
	}
	db := pick(session, m.conn)
	now := nowUnix()
	if b.Ctime == 0 {
		b.Ctime, b.Mtime = now, now
	}
	if b.ReceivedAt == 0 {
		b.ReceivedAt = now
	}
	if b.State == 0 {
		b.State = BatchStateReceived
	}
	res, err := db.ExecCtx(ctx,
		"INSERT IGNORE INTO ec_ingest_batch (batch_id, source, caller_service, idempotency_key, platform, app_id, "+
			"app_version, sdk_version, mid, device_hash, ip_segment, salt_version, policy_version, total, accepted, "+
			"duplicated, rejected, sampled_out, dispatched, dead, request_bytes, state, top_reason, client_seq, "+
			"request_id, last_error, trace_id, received_at, finished_at, ctime, mtime) "+
			"VALUES ("+placeholders(ingestBatchInsertValues)+")",
		b.BatchID, b.Source, b.CallerService, b.IdempotencyKey, b.Platform, b.AppID, b.AppVersion, b.SdkVersion,
		b.Mid, b.DeviceHash, b.IPSegment, b.SaltVersion, b.PolicyVersion, b.Total, b.Accepted, b.Duplicated,
		b.Rejected, b.SampledOut, b.Dispatched, b.Dead, b.RequestBytes, b.State, b.TopReason, b.ClientSeq,
		b.RequestID, b.LastError, b.TraceID, b.ReceivedAt, b.FinishedAt, b.Ctime, b.Mtime)
	if err != nil {
		if IsDuplicate(err) {
			// 并发重试：另一路已建行，回查首行按幂等重放处理。
			exist, qerr := m.FindByBatchID(ctx, b.BatchID)
			if qerr == nil && exist != nil {
				return exist.ID, false, nil
			}
			return 0, false, qerr
		}
		return 0, false, fmt.Errorf("ec_ingest_batch Insert: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("ec_ingest_batch Insert RowsAffected: %w", err)
	}
	if affected == 0 {
		exist, qerr := m.FindByBatchID(ctx, b.BatchID)
		if qerr != nil {
			return 0, false, qerr
		}
		return exist.ID, false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("ec_ingest_batch Insert LastInsertId: %w", err)
	}
	return id, true, nil
}

func (m *defaultIngestBatchModel) FindByBatchID(ctx context.Context, batchID string) (*IngestBatch, error) {
	if batchID == "" {
		return nil, ErrBatchIDRequired
	}
	var row IngestBatch
	err := m.conn.QueryRowCtx(ctx, &row,
		"SELECT "+ingestBatchColumns+" FROM ec_ingest_batch WHERE batch_id = ? LIMIT 1", batchID)
	if err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, ErrBatchNotFound
		}
		return nil, fmt.Errorf("ec_ingest_batch FindByBatchID: %w", err)
	}
	return &row, nil
}

func (m *defaultIngestBatchModel) Accumulate(ctx context.Context, session sqlx.Session, batchID string,
	total, accepted, duplicated, rejected, sampledOut int32) error {
	if batchID == "" {
		return ErrBatchIDRequired
	}
	db := pick(session, m.conn)
	_, err := db.ExecCtx(ctx,
		"UPDATE ec_ingest_batch SET total = total + ?, accepted = accepted + ?, duplicated = duplicated + ?, "+
			"rejected = rejected + ?, sampled_out = sampled_out + ?, mtime = ? WHERE batch_id = ?",
		total, accepted, duplicated, rejected, sampledOut, nowUnix(), batchID)
	if err != nil {
		return fmt.Errorf("ec_ingest_batch Accumulate: %w", err)
	}
	return nil
}

func (m *defaultIngestBatchModel) UpdateAttribution(ctx context.Context, session sqlx.Session,
	batchID, policyVersion string, saltVersion int32, requestBytes int64) error {
	if batchID == "" {
		return ErrBatchIDRequired
	}
	db := pick(session, m.conn)
	_, err := db.ExecCtx(ctx,
		"UPDATE ec_ingest_batch SET policy_version = ?, salt_version = ?, request_bytes = ?, mtime = ? "+
			"WHERE batch_id = ?", truncate(policyVersion, 64), saltVersion, requestBytes, nowUnix(), batchID)
	if err != nil {
		return fmt.Errorf("ec_ingest_batch UpdateAttribution: %w", err)
	}
	return nil
}

func (m *defaultIngestBatchModel) MarkState(ctx context.Context, session sqlx.Session, batchID string,
	from []int32, to int32, topReason int32, lastError string) (bool, error) {
	if batchID == "" {
		return false, ErrBatchIDRequired
	}
	if !ValidBatchState(to) {
		return false, ErrInvalidStateTransition
	}
	if len(from) == 0 {
		return false, ErrInvalidStateTransition
	}
	db := pick(session, m.conn)
	args := make([]any, 0, len(from)+4)
	args = append(args, to, topReason, truncate(lastError, 512), nowUnix())
	query := "UPDATE ec_ingest_batch SET state = ?, top_reason = ?, last_error = ?, mtime = ? WHERE batch_id = ? AND state IN (" +
		placeholders(len(from)) + ")"
	for _, f := range from {
		args = append(args, f)
	}
	args = append(args, batchID)
	res, err := db.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("ec_ingest_batch MarkState: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_ingest_batch MarkState RowsAffected: %w", err)
	}
	return affected > 0, nil
}

func (m *defaultIngestBatchModel) MarkDispatchProgress(ctx context.Context, session sqlx.Session, batchID string,
	dispatchedDelta, deadDelta int32) (bool, error) {
	if batchID == "" {
		return false, ErrBatchIDRequired
	}
	db := pick(session, m.conn)
	res, err := db.ExecCtx(ctx,
		"UPDATE ec_ingest_batch SET dispatched = dispatched + ?, dead = dead + ?, mtime = ? WHERE batch_id = ?",
		dispatchedDelta, deadDelta, nowUnix(), batchID)
	if err != nil {
		return false, fmt.Errorf("ec_ingest_batch MarkDispatchProgress: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_ingest_batch MarkDispatchProgress RowsAffected: %w", err)
	}
	return affected > 0, nil
}

func (m *defaultIngestBatchModel) Finish(ctx context.Context, session sqlx.Session, batchID string) (bool, error) {
	if batchID == "" {
		return false, ErrBatchIDRequired
	}
	db := pick(session, m.conn)
	// finished_at 只写一次（COALESCE/IF 判空），重复收敛不改写首值，保证排障时序可信。
	res, err := db.ExecCtx(ctx,
		"UPDATE ec_ingest_batch SET finished_at = ?, state = ?, mtime = ? WHERE batch_id = ? AND finished_at = 0",
		nowUnix(), BatchStateDispatched, nowUnix(), batchID)
	if err != nil {
		return false, fmt.Errorf("ec_ingest_batch Finish: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_ingest_batch Finish RowsAffected: %w", err)
	}
	return affected > 0, nil
}

func (m *defaultIngestBatchModel) RecountFromRecords(ctx context.Context, session sqlx.Session, batchID string) error {
	if batchID == "" {
		return ErrBatchIDRequired
	}
	db := pick(session, m.conn)
	_, err := db.ExecCtx(ctx,
		"UPDATE ec_ingest_batch b SET b.total = ("+
			"SELECT COUNT(1) FROM ec_event_record r WHERE r.batch_id = b.batch_id), "+
			"b.accepted = (SELECT COUNT(1) FROM ec_event_record r WHERE r.batch_id = b.batch_id AND r.decision = 1), "+
			"b.duplicated = (SELECT COUNT(1) FROM ec_event_record r WHERE r.batch_id = b.batch_id AND r.decision = 2), "+
			"b.rejected = (SELECT COUNT(1) FROM ec_event_record r WHERE r.batch_id = b.batch_id AND r.decision = 3), "+
			"b.sampled_out = (SELECT COUNT(1) FROM ec_event_record r WHERE r.batch_id = b.batch_id AND r.decision = 4), "+
			"b.dispatched = (SELECT COUNT(1) FROM ec_event_record r WHERE r.batch_id = b.batch_id AND r.delivery_state = 3), "+
			"b.dead = (SELECT COUNT(1) FROM ec_event_record r WHERE r.batch_id = b.batch_id AND r.delivery_state = 5), "+
			"b.mtime = ? WHERE b.batch_id = ?", nowUnix(), batchID)
	if err != nil {
		return fmt.Errorf("ec_ingest_batch RecountFromRecords: %w", err)
	}
	return nil
}

func (m *defaultIngestBatchModel) where(f IngestBatchFilter) (string, []any) {
	conds := make([]string, 0, 7)
	args := make([]any, 0, 7)
	if f.Source != 0 {
		conds = append(conds, "source = ?")
		args = append(args, f.Source)
	}
	if f.State != 0 {
		conds = append(conds, "state = ?")
		args = append(args, f.State)
	}
	if f.Mid != 0 {
		conds = append(conds, "mid = ?")
		args = append(args, f.Mid)
	}
	if f.DeviceHash != "" {
		conds = append(conds, "device_hash = ?")
		args = append(args, f.DeviceHash)
	}
	if f.IPSegment != "" {
		conds = append(conds, "ip_segment = ?")
		args = append(args, f.IPSegment)
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

func (m *defaultIngestBatchModel) List(ctx context.Context, f IngestBatchFilter, cur Cursor, ps int32) ([]*IngestBatch, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	where, args := m.where(f)
	if cur.ID > 0 {
		// (ctime, id) 倒序游标：用行比较避免 offset 翻页在高频写入下漏行/重复。
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += "(ctime < ? OR (ctime = ? AND id < ?))"
		args = append(args, cur.Ctime, cur.Ctime, cur.ID)
	}
	var rows []*IngestBatch
	query := "SELECT " + ingestBatchColumns + " FROM ec_ingest_batch" + where +
		" ORDER BY ctime DESC, id DESC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, append(args, ps)...); err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ec_ingest_batch List: %w", err)
	}
	return rows, nil
}

func (m *defaultIngestBatchModel) Count(ctx context.Context, f IngestBatchFilter) (int64, error) {
	where, args := m.where(f)
	var total int64
	query := "SELECT COUNT(1) FROM ec_ingest_batch" + where
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		return 0, fmt.Errorf("ec_ingest_batch Count: %w", err)
	}
	return total, nil
}

func (m *defaultIngestBatchModel) CountRejectedSince(ctx context.Context, since int64) (int64, error) {
	var total int64
	err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(1) FROM ec_ingest_batch WHERE state = ? AND ctime >= ?", BatchStateRejected, since)
	if err != nil {
		return 0, fmt.Errorf("ec_ingest_batch CountRejectedSince: %w", err)
	}
	return total, nil
}

func (m *defaultIngestBatchModel) CountTopReasonSince(ctx context.Context, topReason int32, since int64) (int64, error) {
	// top_reason 与 state 一起才构成「整批被拒」的完整语义：只按 top_reason 计数会把
	// 「个别事件被拒但整批已投递」的批次也算进去，健康度于是虚高。
	if !ValidReason(topReason) {
		return 0, ErrInvalidStateTransition
	}
	var total int64
	err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(1) FROM ec_ingest_batch WHERE state = ? AND top_reason = ? AND ctime >= ?",
		BatchStateRejected, topReason, since)
	if err != nil {
		return 0, fmt.Errorf("ec_ingest_batch CountTopReasonSince: %w", err)
	}
	return total, nil
}

func (m *defaultIngestBatchModel) DeleteBefore(ctx context.Context, ctimeBefore int64, limit int32) (int64, error) {
	if ctimeBefore <= 0 {
		return 0, ErrInvalidPage
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > maxIDList {
		return 0, ErrBatchLimitTooLarge
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM ec_ingest_batch WHERE ctime < ? LIMIT ?", ctimeBefore, limit)
	if err != nil {
		return 0, fmt.Errorf("ec_ingest_batch DeleteBefore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ec_ingest_batch DeleteBefore RowsAffected: %w", err)
	}
	return n, nil
}

// ErrSourceNotAllowed 来源与方法不匹配（REJECT_SOURCE_NOT_ALLOWED），携带具体取值。
func ErrSourceNotAllowed(source int32) error {
	return fmt.Errorf("event-collector: source %d not allowed for this method", source)
}

// truncate 把外部文本（错误信息、payload 摘要）裁到列宽，避免 MySQL 截断警告。
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}
