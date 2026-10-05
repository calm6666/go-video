package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// AggregationJob 聚合作业行（spm_aggregation_job 表投影）。
//
// 它是「实时窗口聚合 / 离线回填 / 从事实重算」三条链路的统一任务面：
// 作业本身可重放（claim + 租约 + 进度计数），因此崩溃后另一个实例能接手，
// 而不是把窗口指标写花。租约列 claimed_by/lease_until 由 ClaimPending 维护。
type AggregationJob struct {
	ID              int64  `db:"id"`                // 主键 ID（job_id）
	JobType         int32  `db:"job_type"`          // 1 实时、2 离线回填、3 重算
	State           int32  `db:"state"`             // 1 PENDING、2 RUNNING、3 SUCCEEDED、4 FAILED、5 CANCELLED
	SubjectType     int32  `db:"subject_type"`      // 0 = 全部主体
	SubjectID       int64  `db:"subject_id"`        // 0 = 不限主体
	MetricKey       string `db:"metric_key"`        // 空 = 该作业类型下全部指标
	MetricVersion   int32  `db:"metric_version"`    // 重算/回填必须显式版本
	WindowType      int32  `db:"window_type"`       // 窗口粒度
	WindowStartFrom int64  `db:"window_start_from"` // 起始窗口（含）
	WindowStartTo   int64  `db:"window_start_to"`   // 结束窗口（含）
	WindowsTotal    int32  `db:"windows_total"`     // 计划窗口数
	WindowsDone     int32  `db:"windows_done"`      // 已完成窗口数
	WindowsFailed   int32  `db:"windows_failed"`    // 失败窗口数
	ClaimedBy       string `db:"claimed_by"`        // 当前持有租约的实例标识/领取令牌
	LeaseUntil      int64  `db:"lease_until"`       // 租约到期时间（Unix 秒），过期可被接手
	RequestID       string `db:"request_id"`        // 提交幂等键
	Operator        string `db:"operator"`          // 触发者（system/cron/admin:<id>）
	Reason          string `db:"reason"`            // 触发原因（回填范围、修复单号）
	LastError       string `db:"last_error"`        // 最近失败原因（脱敏截断，不含堆栈与 SQL）
	FinishedAt      int64  `db:"finished_at"`       // 终态时间（Unix 秒，0 = 未结束）
	Ctime           int64  `db:"ctime"`             // 创建时间（Unix 秒）
	Mtime           int64  `db:"mtime"`             // 修改时间（Unix 秒）
}

// JobFilter 作业列表过滤条件。零值表示不限。
type JobFilter struct {
	JobType int32
	State   int32
	Since   int64
	Offset  int32
	Limit   int32
}

// AggregationJobModel spm_aggregation_job 表读写接口。
type AggregationJobModel interface {
	// InsertIfAbsent 按 uniq_request_id 幂等提交作业。返回 false 表示同 request_id 已提交，
	// 调用方应回读既有作业并复用（绝不重复起一个改写同一段窗口的作业）。
	InsertIfAbsent(ctx context.Context, j *AggregationJob) (bool, error)
	// FindByID 按主键查询；不存在返回 nil。
	FindByID(ctx context.Context, id int64) (*AggregationJob, error)
	// FindByRequestID 按幂等键查询（重复提交回放首次结果）；不存在返回 nil。
	FindByRequestID(ctx context.Context, requestID string) (*AggregationJob, error)
	// ClaimPending 原子领取一个可执行作业（PENDING，或租约过期的 RUNNING）。
	// token 是调用方生成的唯一领取令牌：领取后靠它回读自己被派到的作业，
	// 避免「UPDATE ... LIMIT 1」拿不到受影响主键的问题。无可领取作业时返回 nil。
	// token 必须「每次领取」唯一（不是每个进程唯一）：同一实例连续两次领取若落在同一秒，
	// 回读条件 (claimed_by, state, lease_until) 会命中两行，只能拿到 id 最小的一条，
	// 另一条就被静默丢掉。令牌生成约束见 README「作业与租约语义」。
	ClaimPending(ctx context.Context, token string, leaseSeconds int32, jobTypes []int32) (*AggregationJob, error)
	// RenewLease 续租：只有当前令牌持有者能续，返回 0 表示已被他人接手。
	RenewLease(ctx context.Context, id int64, token string, leaseSeconds int32) (int64, error)
	// UpdateProgress 累加完成/失败窗口数并记录最近错误（进度只单调前进）。
	UpdateProgress(ctx context.Context, id int64, token string, doneDelta, failedDelta int32, lastError string) error
	// MarkFinished 落终态（SUCCEEDED/FAILED/CANCELLED）。终态不可回退，返回 0 表示未改动。
	MarkFinished(ctx context.Context, id int64, token string, state int32, lastError string) (int64, error)
	// CancelPending 取消尚未开始的作业（PENDING -> CANCELLED）。operator/reason 都为必填留痕。
	// 注意：这里把 operator 覆写成「取消人」而不是提交人——运维真正要问的是谁按下的取消，
	// 提交人可从 request_id 关联的上游留痕回查。
	CancelPending(ctx context.Context, id int64, operator, reason string) (int64, error)
	// List 分页查询作业。
	List(ctx context.Context, f JobFilter) ([]*AggregationJob, error)
	// Count 统计过滤条件下的作业数（分页 total）。
	Count(ctx context.Context, f JobFilter) (int64, error)
}

type defaultAggregationJobModel struct {
	conn sqlx.SqlConn
}

// NewAggregationJobModel 创建 AggregationJobModel 实现。
func NewAggregationJobModel(conn sqlx.SqlConn) AggregationJobModel {
	return &defaultAggregationJobModel{conn: conn}
}

const aggregationJobColumns = "id, job_type, state, subject_type, subject_id, metric_key," +
	" metric_version, window_type, window_start_from, window_start_to, windows_total," +
	" windows_done, windows_failed, claimed_by, lease_until, request_id, operator, reason," +
	" last_error, finished_at, ctime, mtime"

func (m *defaultAggregationJobModel) InsertIfAbsent(
	ctx context.Context, j *AggregationJob,
) (bool, error) {
	if !ValidJobType(j.JobType) {
		return false, ErrInvalidJobType
	}
	if strings.TrimSpace(j.RequestID) == "" {
		return false, ErrRequestIdRequired
	}
	if !ValidWindowType(j.WindowType) {
		return false, ErrInvalidWindow
	}
	// 回填与重算必须钉住口径版本：拿「当前 ACTIVE」去改写历史窗口，
	// 事后没人能解释这批数据是按哪版口径写的。
	if j.JobType != JobTypeRealtime && j.MetricKey != "" && j.MetricVersion <= 0 {
		return false, ErrMetricVersionRequired
	}
	if j.State == JobStateUnspecified {
		j.State = JobStatePending
	}
	j.WindowStartFrom = AlignWindow(j.WindowStartFrom, j.WindowType)
	now := nowUnix()
	query := "INSERT INTO spm_aggregation_job (job_type, state, subject_type, subject_id," +
		" metric_key, metric_version, window_type, window_start_from, window_start_to," +
		" windows_total, windows_done, windows_failed, request_id, operator, reason, last_error," +
		" finished_at, ctime, mtime)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?, '', 0, ?, ?)" +
		" ON DUPLICATE KEY UPDATE request_id = request_id"
	res, err := m.conn.ExecCtx(ctx, query,
		j.JobType, j.State, j.SubjectType, j.SubjectID, truncate(j.MetricKey, 100),
		j.MetricVersion, j.WindowType, j.WindowStartFrom, j.WindowStartTo, j.WindowsTotal,
		truncate(j.RequestID, 128), truncate(j.Operator, 64), truncate(j.Reason, 500), now, now)
	if err != nil {
		return false, fmt.Errorf("spm_aggregation_job InsertIfAbsent: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("spm_aggregation_job InsertIfAbsent RowsAffected: %w", err)
	}
	if affected == 0 {
		return false, nil
	}
	j.Ctime, j.Mtime, j.State = now, now, JobStatePending
	return true, nil
}

func (m *defaultAggregationJobModel) FindByID(ctx context.Context, id int64) (*AggregationJob, error) {
	var row AggregationJob
	query := "SELECT " + aggregationJobColumns + " FROM spm_aggregation_job WHERE id = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_aggregation_job FindByID: %w", err)
	}
	return &row, nil
}

func (m *defaultAggregationJobModel) FindByRequestID(
	ctx context.Context, requestID string,
) (*AggregationJob, error) {
	var row AggregationJob
	query := "SELECT " + aggregationJobColumns + " FROM spm_aggregation_job WHERE request_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_aggregation_job FindByRequestID: %w", err)
	}
	return &row, nil
}

func (m *defaultAggregationJobModel) ClaimPending(
	ctx context.Context, token string, leaseSeconds int32, jobTypes []int32,
) (*AggregationJob, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrOperatorRequired
	}
	if leaseSeconds <= 0 {
		leaseSeconds = 600
	}
	now := nowUnix()
	leaseUntil := now + int64(leaseSeconds)
	// 可领取 = PENDING，或 RUNNING 但租约已过期（持有者崩溃/长 GC，作业必须能被接手）。
	// 括号不能省：MySQL 里 AND 比 OR 结合得更紧，少了括号下面追加的 job_type 过滤
	// 只会作用在「过期接手」那一支上，PENDING 分支会把别的作业类型也抢走。
	query := "UPDATE spm_aggregation_job SET state = ?, claimed_by = ?, lease_until = ?, mtime = ?" +
		" WHERE (state = ? OR (state = ? AND lease_until < ?))"
	args := []any{JobStateRunning, token, leaseUntil, now, JobStatePending, JobStateRunning, now}
	if len(jobTypes) > 0 {
		holders := make([]string, 0, len(jobTypes))
		for range jobTypes {
			holders = append(holders, "?")
		}
		query += " AND job_type IN (" + strings.Join(holders, ",") + ")"
		for _, t := range jobTypes {
			args = append(args, t)
		}
	}
	// 单条 UPDATE ... LIMIT 1 保证同一作业不会被两个实例同时接手；
	// 受影响行数用 0/1 判断，再按令牌回读拿主键。
	query += " ORDER BY id ASC LIMIT 1"
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("spm_aggregation_job ClaimPending: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("spm_aggregation_job ClaimPending RowsAffected: %w", err)
	}
	if affected == 0 {
		return nil, nil
	}
	var row AggregationJob
	err = m.conn.QueryRowCtx(ctx, &row,
		"SELECT "+aggregationJobColumns+
			" FROM spm_aggregation_job WHERE claimed_by = ? AND state = ? AND lease_until = ?"+
			" ORDER BY id ASC LIMIT 1",
		token, JobStateRunning, leaseUntil)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_aggregation_job ClaimPending readback: %w", err)
	}
	return &row, nil
}

func (m *defaultAggregationJobModel) RenewLease(
	ctx context.Context, id int64, token string, leaseSeconds int32,
) (int64, error) {
	if leaseSeconds <= 0 {
		leaseSeconds = 600
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE spm_aggregation_job SET lease_until = ?, mtime = ?"+
			" WHERE id = ? AND claimed_by = ? AND state = ?",
		nowUnix()+int64(leaseSeconds), nowUnix(), id, token, JobStateRunning)
	if err != nil {
		return 0, fmt.Errorf("spm_aggregation_job RenewLease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_aggregation_job RenewLease RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultAggregationJobModel) UpdateProgress(
	ctx context.Context, id int64, token string, doneDelta, failedDelta int32, lastError string,
) error {
	if doneDelta < 0 || failedDelta < 0 {
		return ErrNegativeProgress
	}
	if doneDelta == 0 && failedDelta == 0 && lastError == "" {
		return nil
	}
	// 进度只在持有令牌且租约未过期时前进：过期实例的迟到写回必须被丢弃，
	// 否则接手者已经重算的窗口会被旧进度二次累加。
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE spm_aggregation_job SET windows_done = windows_done + ?,"+
			" windows_failed = windows_failed + ?, last_error = ?, mtime = ?"+
			" WHERE id = ? AND claimed_by = ? AND state = ? AND lease_until > ?",
		doneDelta, failedDelta, truncate(lastError, 512), nowUnix(),
		id, token, JobStateRunning, nowUnix())
	if err != nil {
		return fmt.Errorf("spm_aggregation_job UpdateProgress: %w", err)
	}
	return nil
}

func (m *defaultAggregationJobModel) MarkFinished(
	ctx context.Context, id int64, token string, state int32, lastError string,
) (int64, error) {
	if state != JobStateSucceeded && state != JobStateFailed && state != JobStateCancelled {
		return 0, ErrInvalidJobState
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE spm_aggregation_job SET state = ?, finished_at = ?, last_error = ?,"+
			" claimed_by = '', lease_until = 0, mtime = ?"+
			" WHERE id = ? AND state = ? AND claimed_by = ?",
		state, now, truncate(lastError, 512), now, id, JobStateRunning, token)
	if err != nil {
		return 0, fmt.Errorf("spm_aggregation_job MarkFinished: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_aggregation_job MarkFinished RowsAffected: %w", err)
	}
	if n == 0 {
		// 区分「作业不存在」与「状态不允许推进」：前者返回 0 让调用方按 found=false 处理。
		row, ferr := m.FindByID(ctx, id)
		if ferr != nil {
			return 0, ferr
		}
		if row != nil && isTerminalJobState(row.State) {
			return 0, ErrJobAlreadyTerminal
		}
	}
	return n, nil
}

func (m *defaultAggregationJobModel) CancelPending(
	ctx context.Context, id int64, operator, reason string,
) (int64, error) {
	if strings.TrimSpace(operator) == "" {
		return 0, ErrOperatorRequired
	}
	if strings.TrimSpace(reason) == "" {
		return 0, ErrReasonRequired
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE spm_aggregation_job SET state = ?, finished_at = ?, operator = ?,"+
			" reason = ?, mtime = ? WHERE id = ? AND state = ?",
		JobStateCancelled, now, truncate(operator, 64), truncate(reason, 500), now,
		id, JobStatePending)
	if err != nil {
		return 0, fmt.Errorf("spm_aggregation_job CancelPending: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_aggregation_job CancelPending RowsAffected: %w", err)
	}
	return n, nil
}

func isTerminalJobState(state int32) bool {
	return state == JobStateSucceeded || state == JobStateFailed || state == JobStateCancelled
}

func buildJobQuery(f JobFilter) (string, []any) {
	query := "SELECT " + aggregationJobColumns + " FROM spm_aggregation_job WHERE 1 = 1"
	var args []any
	if f.JobType != JobTypeUnspecified {
		query += " AND job_type = ?"
		args = append(args, f.JobType)
	}
	if f.State != JobStateUnspecified {
		query += " AND state = ?"
		args = append(args, f.State)
	}
	if f.Since > 0 {
		query += " AND ctime >= ?"
		args = append(args, f.Since)
	}
	return query, args
}

func (m *defaultAggregationJobModel) List(ctx context.Context, f JobFilter) ([]*AggregationJob, error) {
	query, args := buildJobQuery(f)
	query += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, clampLimit(f.Limit), clampOffset(f.Offset))
	var rows []*AggregationJob
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_aggregation_job List: %w", err)
	}
	return rows, nil
}

func (m *defaultAggregationJobModel) Count(ctx context.Context, f JobFilter) (int64, error) {
	query, args := buildJobQuery(f)
	query = strings.Replace(query, "SELECT "+aggregationJobColumns, "SELECT COUNT(*)", 1)
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		return 0, fmt.Errorf("spm_aggregation_job Count: %w", err)
	}
	return n, nil
}
