package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// archiveBatchColumns 是 audit_archive_batch 的列清单。
const archiveBatchColumns = "batch_id, request_id, chain_key, from_seq, to_seq, row_count, bucket," +
	" object_key, manifest_hash, last_entry_hash, state, operator_id, trace_id, err_msg," +
	" ctime, mtime, finished_at"

// ArchiveBatch 对应 audit_archive_batch 表：归档批次证据。
//
// 它存在的唯一理由：审计条目离开热表之前必须先留下一份「这段链被完整搬走了」的凭证。
// manifest_hash 是归档清单文件（每行 entry_id/seq/entry_hash）的 sha256hex，
// 校验顺序固定为 writing → verified（重算清单哈希并与链尾对齐）→ purged（标记账号），
// 任一环节失败都停在 failed，绝不跳过 verified 直接清热表。
type ArchiveBatch struct {
	// BatchID 自增主键。
	BatchID int64 `db:"batch_id"`
	// RequestID 幂等键（唯一索引）。
	RequestID string `db:"request_id"`
	// ChainKey 被归档的链。
	ChainKey string `db:"chain_key"`
	// FromSeq 起始序号（含）。
	FromSeq int64 `db:"from_seq"`
	// ToSeq 结束序号（含）。
	ToSeq int64 `db:"to_seq"`
	// RowCount 本批条数，必须等于 ToSeq - FromSeq + 1，否则即为截断。
	RowCount int64 `db:"row_count"`
	// Bucket 归档对象桶（只存引用）。
	Bucket string `db:"bucket"`
	// ObjectKey 归档对象键（清单 + 全量条目文件）。
	ObjectKey string `db:"object_key"`
	// ManifestHash 清单文件 sha256hex。
	ManifestHash string `db:"manifest_hash"`
	// LastEntryHash 批次尾条目的 entry_hash，用于续验（下一批的 prev_hash 必须等于它）。
	LastEntryHash string `db:"last_entry_hash"`
	// State 见 CanBatchTransition。
	State string `db:"state"`
	// OperatorID 触发人 admin_id；定时触发为 0（配合 ActorType=SYSTEM 的审计条目）。
	OperatorID int64 `db:"operator_id"`
	// TraceID 链路 ID。
	TraceID string `db:"trace_id"`
	// ErrMsg 失败原因（脱敏）。
	ErrMsg string `db:"err_msg"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后更新时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
	// FinishedAt 进入 verified/purged/failed 的时间。
	FinishedAt int64 `db:"finished_at"`
}

// ArchiveBatchFilter 归档批次列表条件。
type ArchiveBatchFilter struct {
	ChainKey string
	State    string
	StartAt  int64
	EndAt    int64
	Pn       int32
	Ps       int32
}

// ArchiveBatchModel 抽象 audit_archive_batch 表。
type ArchiveBatchModel interface {
	// Insert 创建批次；request_id 冲突返回 ErrTaskExists（同一幂等键复用同一批次）。
	Insert(ctx context.Context, b *ArchiveBatch) (int64, error)
	// FindOne 按主键查询。
	FindOne(ctx context.Context, batchID int64) (*ArchiveBatch, error)
	// FindByRequestID 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*ArchiveBatch, error)
	// FindCovering 查找已覆盖 [from,to] 且状态为 verified/purged 的批次，
	// 用于「同一段链不被重复归档」的预检。
	FindCovering(ctx context.Context, chainKey string, fromSeq, toSeq int64) (*ArchiveBatch, error)
	// TransitionState 带合法性校验与 from 条件的状态推进（并发推进只有一人成功）。
	TransitionState(ctx context.Context, batchID int64, fromState, toState string, manifestHash, lastEntryHash string, errMsg string) (bool, error)
	// List 分页查询。
	List(ctx context.Context, f ArchiveBatchFilter) ([]*ArchiveBatch, int64, error)
}

type defaultArchiveBatchModel struct {
	conn sqlx.SqlConn
}

// NewArchiveBatchModel 构造 audit_archive_batch 的 sqlx 实现。
func NewArchiveBatchModel(conn sqlx.SqlConn) ArchiveBatchModel {
	return &defaultArchiveBatchModel{conn: conn}
}

func (m *defaultArchiveBatchModel) Insert(ctx context.Context, b *ArchiveBatch) (int64, error) {
	if b.RequestID == "" {
		return 0, ErrRequestIDRequired
	}
	if b.ChainKey == "" {
		return 0, ErrChainKeyRequired
	}
	if b.Ctime == 0 {
		b.Ctime = nowUnix()
	}
	if b.State == "" {
		b.State = BatchStatePending
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO audit_archive_batch ("+archiveBatchColumns+") VALUES ("+placeholders(17)+")"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		b.BatchID, b.RequestID, b.ChainKey, b.FromSeq, b.ToSeq, b.RowCount, b.Bucket, b.ObjectKey,
		b.ManifestHash, b.LastEntryHash, b.State, b.OperatorID, b.TraceID, b.ErrMsg,
		b.Ctime, b.Ctime, b.FinishedAt)
	if err != nil {
		return 0, fmt.Errorf("audit_archive_batch Insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("audit_archive_batch Insert RowsAffected: %w", err)
	}
	if n == 0 {
		return 0, ErrTaskExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("audit_archive_batch Insert LastInsertId: %w", err)
	}
	b.BatchID = id
	return id, nil
}

const archiveSelect = "SELECT " + archiveBatchColumns + " FROM audit_archive_batch"

func (m *defaultArchiveBatchModel) FindOne(ctx context.Context, batchID int64) (*ArchiveBatch, error) {
	var row ArchiveBatch
	err := m.conn.QueryRowCtx(ctx, &row, archiveSelect+" WHERE batch_id = ? LIMIT 1", batchID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBatchNotFound
		}
		return nil, fmt.Errorf("audit_archive_batch FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultArchiveBatchModel) FindByRequestID(ctx context.Context, requestID string) (*ArchiveBatch, error) {
	if requestID == "" {
		return nil, ErrRequestIDRequired
	}
	var row ArchiveBatch
	err := m.conn.QueryRowCtx(ctx, &row, archiveSelect+" WHERE request_id = ? LIMIT 1", requestID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit_archive_batch FindByRequestID: %w", err)
	}
	return &row, nil
}

func (m *defaultArchiveBatchModel) FindCovering(ctx context.Context, chainKey string, fromSeq, toSeq int64) (*ArchiveBatch, error) {
	if chainKey == "" {
		return nil, ErrChainKeyRequired
	}
	var row ArchiveBatch
	query := archiveSelect + " WHERE chain_key = ? AND from_seq <= ? AND to_seq >= ?" +
		" AND state IN (?, ?) ORDER BY batch_id DESC LIMIT 1"
	err := m.conn.QueryRowCtx(ctx, &row, query, chainKey, fromSeq, toSeq, BatchStateVerified, BatchStatePurged)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit_archive_batch FindCovering: %w", err)
	}
	return &row, nil
}

func (m *defaultArchiveBatchModel) TransitionState(ctx context.Context, batchID int64, fromState, toState,
	manifestHash, lastEntryHash, errMsg string) (bool, error) {
	if !CanBatchTransition(fromState, toState) {
		return false, ErrTaskBadTransition
	}
	now := nowUnix()
	// manifest_hash / last_entry_hash 只在本批次写完成时落一次；之后不可改。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE audit_archive_batch SET state = ?, err_msg = ?, mtime = ?,"+
			" manifest_hash = IF(manifest_hash = '', ?, manifest_hash),"+
			" last_entry_hash = IF(last_entry_hash = '', ?, last_entry_hash),"+
			" finished_at = IF(? IN (?, ?, ?), ?, finished_at)"+
			" WHERE batch_id = ? AND state = ?",
		toState, errMsg, now, manifestHash, lastEntryHash,
		toState, BatchStateVerified, BatchStatePurged, BatchStateFailed, now, batchID, fromState)
	if err != nil {
		return false, fmt.Errorf("audit_archive_batch TransitionState: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("audit_archive_batch TransitionState RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultArchiveBatchModel) List(ctx context.Context, f ArchiveBatchFilter) ([]*ArchiveBatch, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 5)
	if f.ChainKey != "" {
		where += " AND chain_key = ?"
		args = append(args, f.ChainKey)
	}
	if f.State != "" {
		where += " AND state = ?"
		args = append(args, f.State)
	}
	if f.StartAt > 0 {
		where += " AND ctime >= ?"
		args = append(args, f.StartAt)
	}
	if f.EndAt > 0 {
		where += " AND ctime < ?"
		args = append(args, f.EndAt)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM audit_archive_batch "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("audit_archive_batch List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*ArchiveBatch
	query := archiveSelect + where + " ORDER BY batch_id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("audit_archive_batch List: %w", err)
	}
	return rows, total, nil
}
