package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// exportTaskColumns 是 audit_export_task 的列清单。
const exportTaskColumns = "task_id, request_id, operator_id, caller_service, filter_json, format, state," +
	" row_count, object_size, expire_at, file_hash, err_msg, trace_id, last_seq, bucket, object_key," +
	" ctime, mtime, started_at, finished_at"

// ExportTask 对应 audit_export_task 表：审计导出任务。
//
// 为什么是任务而不是同步查询：一次合规导出可能覆盖数十万条、要翻页很久，
// 同步 RPC 会长时间占住连接与内存，还会被 gRPC 超时打断留下半成品。
// 因此导出走「提交任务 → 由 services/cron 推进（RunAuditExportTask）→ 结果落对象存储 →
// 客户端凭 task_id 换取短期签名地址」，本服务不内置 worker（AGENTS.md §3）。
type ExportTask struct {
	// TaskID 自增主键。
	TaskID int64 `db:"task_id"`
	// RequestID 提交幂等键（唯一索引）；同一 request_id 只会存在一个任务。
	RequestID string `db:"request_id"`
	// OperatorID 申请人 admin_id（引用 operation，不复制）。
	OperatorID int64 `db:"operator_id"`
	// CallerService 申请来源服务。
	CallerService string `db:"caller_service"`
	// FilterJSON 查询条件快照（JSON 文本）。写入前经 PII 扫描，
	// 任何疑似明文手机号的过滤值都会被拒绝，导出条件本身也不能成为泄露面。
	FilterJSON string `db:"filter_json"`
	// Format csv / json。
	Format string `db:"format"`
	// State 见 CanExportTransition。
	State string `db:"state"`
	// RowCount 已导出行数（推进时累加，完成后即为总数）。
	RowCount int64 `db:"row_count"`
	// ObjectSize 导出文件字节数。
	ObjectSize int64 `db:"object_size"`
	// ExpireAt 对象计划删除时间（Unix 秒）；到期后任务流转到 expired。
	ExpireAt int64 `db:"expire_at"`
	// FileHash 导出文件 sha256hex，供下载方自证文件未被替换。
	FileHash string `db:"file_hash"`
	// ErrMsg 失败原因（脱敏，不含堆栈与连接串）。
	ErrMsg string `db:"err_msg"`
	// TraceID 提交时的链路 ID。
	TraceID string `db:"trace_id"`
	// LastSeq 断点游标：已导出的最大 entry_id（按 entry_id 升序分批扫热表）。
	// 推进者崩溃后从该位置续传，避免同一任务重复写文件。
	LastSeq int64 `db:"last_seq"`
	// Bucket 对象存储桶（只存引用，密钥进 Secret/Vault）。
	Bucket string `db:"bucket"`
	// ObjectKey 对象键（只存引用）。
	ObjectKey string `db:"object_key"`
	// Ctime 提交时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后更新时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
	// StartedAt 首次进入 running 的时间。
	StartedAt int64 `db:"started_at"`
	// FinishedAt 进入 succeeded/failed 的时间。
	FinishedAt int64 `db:"finished_at"`
}

// ExportTaskFilter 导出任务列表条件。
type ExportTaskFilter struct {
	OperatorID int64
	State      string
	StartAt    int64
	EndAt      int64
	Pn         int32
	Ps         int32
}

// ExportTaskModel 抽象 audit_export_task 表。
type ExportTaskModel interface {
	// Insert 提交任务；request_id 冲突时返回 ErrTaskExists（repository 回查并 reused=true）。
	Insert(ctx context.Context, t *ExportTask) (int64, error)
	// FindOne 按主键查询。
	FindOne(ctx context.Context, taskID int64) (*ExportTask, error)
	// FindByRequestID 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*ExportTask, error)
	// Claim 把任务从 expectState 原子推进到 running（返回 updated=false 表示被别的推进者抢占）。
	// 这是「同一任务不被两个 cron 实例同时导出」的关键：条件更新代替分布式锁。
	Claim(ctx context.Context, taskID int64, expectState string) (bool, error)
	// AddProgress 累加已导出行数与游标，不改状态（推进中途心跳）。
	AddProgress(ctx context.Context, taskID int64, rows, lastSeq int64) error
	// Finish 结束任务：写终态、对象引用与文件摘要。
	// WHERE 带 state = 'running'，因此重复结束不会覆盖已定的终态。
	Finish(ctx context.Context, taskID int64, toState string, obj ObjectRef, errMsg string) (bool, error)
	// TransitionState 通用状态迁移（如 succeeded → expired），带合法性与乐观条件。
	TransitionState(ctx context.Context, taskID int64, fromState, toState string) (bool, error)
	// List 分页查询。
	List(ctx context.Context, f ExportTaskFilter) ([]*ExportTask, int64, error)
}

// ObjectRef 对象存储引用。只带定位信息，不带任何凭据。
type ObjectRef struct {
	Bucket     string
	ObjectKey  string
	Size       int64
	FileHash   string
	ExpireAt   int64
	TotalRows  int64
	FinishedAt int64
}

type defaultExportTaskModel struct {
	conn sqlx.SqlConn
}

// NewExportTaskModel 构造 audit_export_task 的 sqlx 实现。
func NewExportTaskModel(conn sqlx.SqlConn) ExportTaskModel {
	return &defaultExportTaskModel{conn: conn}
}

func (m *defaultExportTaskModel) Insert(ctx context.Context, t *ExportTask) (int64, error) {
	if t.RequestID == "" {
		return 0, ErrRequestIDRequired
	}
	if t.Ctime == 0 {
		t.Ctime = nowUnix()
	}
	if t.State == "" {
		t.State = ExportStatePending
	}
	// 依赖 uniq_request_id：冲突时 mtime 自等，RowsAffected == 0，
	// 从而不依赖驱动专有错误码就能识别「已存在」。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO audit_export_task ("+exportTaskColumns+") VALUES ("+placeholders(20)+")"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		t.TaskID, t.RequestID, t.OperatorID, t.CallerService, t.FilterJSON, t.Format, t.State,
		t.RowCount, t.ObjectSize, t.ExpireAt, t.FileHash, t.ErrMsg, t.TraceID, t.LastSeq,
		t.Bucket, t.ObjectKey, t.Ctime, t.Ctime, t.StartedAt, t.FinishedAt)
	if err != nil {
		return 0, fmt.Errorf("audit_export_task Insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("audit_export_task Insert RowsAffected: %w", err)
	}
	if n == 0 {
		return 0, ErrTaskExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("audit_export_task Insert LastInsertId: %w", err)
	}
	t.TaskID = id
	return id, nil
}

const exportSelect = "SELECT " + exportTaskColumns + " FROM audit_export_task"

func (m *defaultExportTaskModel) FindOne(ctx context.Context, taskID int64) (*ExportTask, error) {
	var row ExportTask
	err := m.conn.QueryRowCtx(ctx, &row, exportSelect+" WHERE task_id = ? LIMIT 1", taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("audit_export_task FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultExportTaskModel) FindByRequestID(ctx context.Context, requestID string) (*ExportTask, error) {
	if requestID == "" {
		return nil, ErrRequestIDRequired
	}
	var row ExportTask
	err := m.conn.QueryRowCtx(ctx, &row, exportSelect+" WHERE request_id = ? LIMIT 1", requestID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit_export_task FindByRequestID: %w", err)
	}
	return &row, nil
}

func (m *defaultExportTaskModel) Claim(ctx context.Context, taskID int64, expectState string) (bool, error) {
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE audit_export_task SET state = ?, started_at = IF(started_at = 0, ?, started_at), mtime = ?"+
			" WHERE task_id = ? AND state = ?",
		ExportStateRunning, now, now, taskID, expectState)
	if err != nil {
		return false, fmt.Errorf("audit_export_task Claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("audit_export_task Claim RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultExportTaskModel) AddProgress(ctx context.Context, taskID int64, rows, lastSeq int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE audit_export_task SET row_count = row_count + ?, last_seq = ?, mtime = ?"+
			" WHERE task_id = ? AND state = ?",
		rows, lastSeq, nowUnix(), taskID, ExportStateRunning)
	if err != nil {
		return fmt.Errorf("audit_export_task AddProgress: %w", err)
	}
	return nil
}

func (m *defaultExportTaskModel) Finish(ctx context.Context, taskID int64, toState string,
	obj ObjectRef, errMsg string) (bool, error) {
	if !CanExportTransition(ExportStateRunning, toState) {
		return false, ErrTaskBadTransition
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE audit_export_task SET state = ?, row_count = ?, bucket = ?, object_key = ?, object_size = ?,"+
			" file_hash = ?, expire_at = ?, err_msg = ?, mtime = ?, finished_at = ?"+
			" WHERE task_id = ? AND state = ?",
		toState, obj.TotalRows, obj.Bucket, obj.ObjectKey, obj.Size, obj.FileHash, obj.ExpireAt,
		errMsg, now, now, taskID, ExportStateRunning)
	if err != nil {
		return false, fmt.Errorf("audit_export_task Finish: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("audit_export_task Finish RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultExportTaskModel) TransitionState(ctx context.Context, taskID int64, fromState, toState string) (bool, error) {
	if !CanExportTransition(fromState, toState) {
		return false, ErrTaskBadTransition
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE audit_export_task SET state = ?, mtime = ? WHERE task_id = ? AND state = ?",
		toState, nowUnix(), taskID, fromState)
	if err != nil {
		return false, fmt.Errorf("audit_export_task TransitionState: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("audit_export_task TransitionState RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultExportTaskModel) List(ctx context.Context, f ExportTaskFilter) ([]*ExportTask, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 4)
	if f.OperatorID > 0 {
		where += " AND operator_id = ?"
		args = append(args, f.OperatorID)
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
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM audit_export_task "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("audit_export_task List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*ExportTask
	query := exportSelect + where + " ORDER BY task_id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("audit_export_task List: %w", err)
	}
	return rows, total, nil
}
