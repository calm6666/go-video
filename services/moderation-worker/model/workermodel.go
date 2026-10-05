package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// WorkerTask 识别任务执行记录。
// worker_task_id 是幂等键（外部传入或 worker 生成），task_id 关联 orchestrator 任务。
// 同一 (task_id, capability) 唯一：相同任务和能力只能执行一次。
type WorkerTask struct {
	WorkerTaskID     string `db:"worker_task_id"`    // worker 内部任务 ID（幂等键）
	TaskID           string `db:"task_id"`           // orchestrator 任务 ID
	Capability       int32  `db:"capability"`        // 识别能力：1 OCR、2 ASR、3 Image、4 Audio
	MediaURI         string `db:"media_uri"`         // 媒体访问 URI
	DurationMs       int64  `db:"duration_ms"`       // 媒体时长（毫秒）
	ParamsJSON       string `db:"params_json"`       // 算法参数 JSON
	TimeoutMs        int64  `db:"timeout_ms"`        // 单任务超时（毫秒）
	TraceID          string `db:"trace_id"`          // 调用方 trace_id
	State            int32  `db:"state"`             // 任务状态：见 TaskState* 常量
	AlgorithmVersion string `db:"algorithm_version"` // 算法版本
	ElapsedMs        int64  `db:"elapsed_ms"`        // 执行耗时（毫秒）
	ResultJSON       string `db:"result_json"`       // 结构化结果片段 JSON
	ErrorMessage     string `db:"error_message"`     // 失败原因
	Ctime            int64  `db:"ctime"`             // 创建时间（Unix 秒）
	Mtime            int64  `db:"mtime"`             // 修改时间（Unix 秒）
}

// WorkerTaskModel worker_task 表查询与写入接口。
type WorkerTaskModel interface {
	// Upsert 新增或按主键 worker_task_id 覆盖任务。
	// 已存在且状态为 RUNNING/SUCCEEDED 时返回 ErrTaskAlreadyRunning，保证幂等。
	Upsert(ctx context.Context, t *WorkerTask) error
	// UpdateResult 写入执行结果并更新状态与耗时。
	UpdateResult(ctx context.Context, workerTaskID string, state int32, algorithmVersion string, elapsedMs int64, resultJSON, errorMessage string) error
	// FindOne 按 worker_task_id 查询单条任务。
	FindOne(ctx context.Context, workerTaskID string) (*WorkerTask, error)
	// FindByTaskID 按 orchestrator task_id 查询任务（同一 task_id 可能有多个能力，返回最新一条）。
	FindByTaskID(ctx context.Context, taskID string) (*WorkerTask, error)
}

type defaultWorkerTaskModel struct {
	conn sqlx.SqlConn
}

// NewWorkerTaskModel 创建 WorkerTaskModel 实现。
func NewWorkerTaskModel(conn sqlx.SqlConn) WorkerTaskModel {
	return &defaultWorkerTaskModel{conn: conn}
}

func (m *defaultWorkerTaskModel) Upsert(ctx context.Context, t *WorkerTask) error {
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO worker_task (worker_task_id, task_id, capability, media_uri, duration_ms, params_json, timeout_ms, trace_id, state, algorithm_version, elapsed_ms, result_json, error_message, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE mtime = VALUES(mtime)",
		t.WorkerTaskID, t.TaskID, t.Capability, t.MediaURI, t.DurationMs, t.ParamsJSON, t.TimeoutMs, t.TraceID, t.State, t.AlgorithmVersion, t.ElapsedMs, t.ResultJSON, t.ErrorMessage, t.Ctime, t.Mtime)
	if err != nil {
		return fmt.Errorf("worker_task Upsert: %w", err)
	}
	return nil
}

func (m *defaultWorkerTaskModel) UpdateResult(ctx context.Context, workerTaskID string, state int32, algorithmVersion string, elapsedMs int64, resultJSON, errorMessage string) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE worker_task SET state = ?, algorithm_version = ?, elapsed_ms = ?, result_json = ?, error_message = ?, mtime = ? WHERE worker_task_id = ?",
		state, algorithmVersion, elapsedMs, resultJSON, errorMessage, NowUnix(), workerTaskID)
	if err != nil {
		return fmt.Errorf("worker_task UpdateResult: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("worker_task UpdateResult RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrTaskNotFound
	}
	return nil
}

func (m *defaultWorkerTaskModel) FindOne(ctx context.Context, workerTaskID string) (*WorkerTask, error) {
	var t WorkerTask
	query := "SELECT worker_task_id, task_id, capability, media_uri, duration_ms, params_json, timeout_ms, trace_id, state, algorithm_version, elapsed_ms, result_json, error_message, ctime, mtime FROM worker_task WHERE worker_task_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, workerTaskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("worker_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultWorkerTaskModel) FindByTaskID(ctx context.Context, taskID string) (*WorkerTask, error) {
	var t WorkerTask
	query := "SELECT worker_task_id, task_id, capability, media_uri, duration_ms, params_json, timeout_ms, trace_id, state, algorithm_version, elapsed_ms, result_json, error_message, ctime, mtime FROM worker_task WHERE task_id = ? ORDER BY mtime DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &t, query, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("worker_task FindByTaskID: %w", err)
	}
	return &t, nil
}
