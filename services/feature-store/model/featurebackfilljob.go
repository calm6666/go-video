package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// featureBackfillJobColumns 是 feature_backfill_job 的列清单（含自增主键）。
const featureBackfillJobColumns = "job_id, feature_key, version, entity_scope, source, state," +
	" window_from, window_to, entity_ids, entities_truncated, entities_total, entities_done," +
	" entities_failed, progress_seq, cursor_entity_id, cursor_entity_str, lease_owner," +
	" lease_expire_at, auto_switch, from_version, request_id, operator, reason, last_error," +
	" trace_id, ctime, mtime, started_at, finished_at"

// featureBackfillJobInsertColumns 是写入列（不含自增主键，共 28 列）。
const featureBackfillJobInsertColumns = "feature_key, version, entity_scope, source, state," +
	" window_from, window_to, entity_ids, entities_truncated, entities_total, entities_done," +
	" entities_failed, progress_seq, cursor_entity_id, cursor_entity_str, lease_owner," +
	" lease_expire_at, auto_switch, from_version, request_id, operator, reason, last_error," +
	" trace_id, ctime, mtime, started_at, finished_at"

// featureBackfillJobInsertArgs 是每行写入的参数个数。
const featureBackfillJobInsertArgs = 28

// 回填作业租约默认值（配置 Backfill.LeaseSeconds / Backfill.BatchRows 可覆盖，
// 但 model 里保留一份兜底，避免配置缺失时租约退化成「永久持有」）。
const (
	defaultJobLeaseSeconds int64 = 120
	defaultJobBatchRows    int32 = 1000
)

// BackfillJob 对应 feature_backfill_job 表：一次「给某个版本补历史值」的作业与它的断点。
//
// 为什么是作业而不是同步 RPC：给一个版本补 30 天历史可能要处理上百万主体，
// 同步调用一定会被超时打断并留下「补了一半」的版本 —— 而半补的版本被切成 ACTIVE
// 就是线上事故。作业化后可以限批推进、可续跑、可取消，进度与失败数还可解释。
//
// 认领用「租约 + 条件更新」而不是分布式锁：两个 cron 实例同时 Claim 时只有一个能拿到
// （UPDATE ... WHERE state=pending 或 state=running AND lease_expire_at<now），
// 原持有者崩溃后租约到期即可被接管，不需要额外的锁服务与锁泄漏治理。
type BackfillJob struct {
	// JobID 自增主键。
	JobID int64 `db:"job_id"`
	// FeatureKey 目标特征键。
	FeatureKey string `db:"feature_key"`
	// Version 目标版本：必须是 DRAFT（见 ErrBackfillTargetNotDraft）。
	Version int32 `db:"version"`
	// EntityScope 主体类型（与定义一致，冗余存一份便于按维度分片扫描）。
	EntityScope int32 `db:"entity_scope"`
	// Source 取数来源（与定义一致，防止「用兴趣口径去补热度特征」这类串链路）。
	Source int32 `db:"source"`
	// State 见 ValidBackfillStateTransition。
	State int32 `db:"state"`
	// WindowFrom 回填数据的时间范围起点（Unix 秒，必填）。
	WindowFrom int64 `db:"window_from"`
	// WindowTo 时间范围终点（Unix 秒），0 = 提交时刻。
	WindowTo int64 `db:"window_to"`
	// EntityIDs 显式主体列表（逗号分隔，升序），空串 = 全量扫描。
	// 有长度上限（MaxBackfillEntityIDsBytes）：条数一样但摘要更长的列表
	// 不该有完全不同的内存上界。
	EntityIDs string `db:"entity_ids"`
	// EntitiesTruncated 1 = 列表因超过上限被截断（此时作业必须 failed，不允许「补一半」）。
	EntitiesTruncated int32 `db:"entities_truncated"`
	// EntitiesTotal 计划处理主体数（扫描前为 0，仅当进度分母用，允许近似）。
	EntitiesTotal int64 `db:"entities_total"`
	// EntitiesDone 已成功处理主体数（AddProgress 按租约持有者累加，不重复计数）。
	EntitiesDone int64 `db:"entities_done"`
	// EntitiesFailed 失败主体数。done + failed <= total，超出即数据坏了，作业转 failed。
	EntitiesFailed int64 `db:"entities_failed"`
	// ProgressSeq 推进次数计数器：每次 AddProgress 无条件 +1。
	// 它存在的理由是 RowsAffected 的语义 —— go-sql-driver 默认返回「实际改变的行数」，
	// 一次「done=0、failed=0、游标没动」的空心跳会改不到任何列而返回 0，
	// 于是租约仍完好的 worker 被误判成「已被接管」。有了这一列，命中即改变，
	// RowsAffected == 0 就只剩「不是当前持有者」这一个含义；
	// 顺带它也是识别僵尸 worker 重复心跳的证据。
	ProgressSeq int64 `db:"progress_seq"`
	// CursorEntityID 断点游标：最近处理的数值型主体 ID（mid/aid/zone/item）。
	// 哈希/搜索词维度的主体 ID 不是数字，这一列保持 0，实际游标看 CursorEntityStr。
	CursorEntityID int64 `db:"cursor_entity_id"`
	// CursorEntityStr 断点游标：最近处理的 entity_id 原文（受控标识，不是明文 PII）。
	// 扫描统一按 entity_id 升序，因此两种游标可以共存且都单调。
	CursorEntityStr string `db:"cursor_entity_str"`
	// LeaseOwner 当前认领者（<pod 名>#<worker id>），非持有者的推进写入会被拒绝。
	LeaseOwner string `db:"lease_owner"`
	// LeaseExpireAt 租约到期时间（Unix 秒）：过期后作业可被别的 worker 接管。
	LeaseExpireAt int64 `db:"lease_expire_at"`
	// AutoSwitch 回填成功后是否自动切 ACTIVE（要求 FromVersion 作为乐观基线）。
	AutoSwitch bool `db:"auto_switch"`
	// FromVersion auto_switch 的期望当前生效版本（0 = 从无到有）。
	// 有这一列，「回填期间有人手工切过版本」不会被 auto_switch 悄悄覆盖。
	FromVersion int32 `db:"from_version"`
	// RequestID 提交幂等键（唯一索引）。
	RequestID string `db:"request_id"`
	// Operator 提交人。
	Operator string `db:"operator"`
	// Reason 提交理由（评估结论、补数工单号）。
	Reason string `db:"reason"`
	// LastError 最近一次失败原因（截断保存，不含 SQL 与特征值原文）。
	LastError string `db:"last_error"`
	// TraceID 提交时的链路 ID。
	TraceID string `db:"trace_id"`
	// Ctime 提交时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后更新时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
	// StartedAt 首次进入 RUNNING 的时间。
	StartedAt int64 `db:"started_at"`
	// FinishedAt 进入终态的时间。
	FinishedAt int64 `db:"finished_at"`
}

// ValidateBackfillJob 校验提交请求（不含唯一性与定义自洽性）。
func ValidateBackfillJob(j *BackfillJob) error {
	if j == nil {
		return ErrFeatureKeyRequired
	}
	if strings.TrimSpace(j.FeatureKey) == "" {
		return ErrFeatureKeyRequired
	}
	if j.Version < 1 {
		return ErrFeatureVersionRequired
	}
	if !ValidEntityScope(j.EntityScope) {
		return ErrEntityScopeRequired
	}
	if !ValidSource(j.Source) {
		return ErrSourceRequired
	}
	if j.WindowFrom <= 0 {
		return ErrBackfillWindowInvalid
	}
	if j.WindowTo != 0 && j.WindowTo < j.WindowFrom {
		return ErrBackfillWindowInvalid
	}
	if j.WindowTo > 0 && j.WindowTo-j.WindowFrom > maxBackfillWindowSeconds {
		// 窗口跨度上限存在的原因：一次补两年历史既没有可解释的评估意义，
		// 也会把「回填」变成事实上的全表重算，占满在线读的 IO。
		return fmt.Errorf("%w: window span exceeds %ds", ErrBackfillWindowInvalid, maxBackfillWindowSeconds)
	}
	if strings.TrimSpace(j.Operator) == "" {
		return ErrOperatorRequired
	}
	if strings.TrimSpace(j.Reason) == "" {
		return ErrReasonRequired
	}
	if strings.TrimSpace(j.RequestID) == "" {
		return ErrRequestIdRequired
	}
	if _, err := j.EntityIDList(); err != nil {
		return err
	}
	return nil
}

// maxBackfillWindowSeconds 是单次回填允许的最大时间跨度（92 天）。
// 与「补更久历史」对应的做法是按窗口提交多个作业，每个都可独立重放与回滚。
const maxBackfillWindowSeconds = 92 * 24 * 3600

// EntityIDList 解析显式主体列表；空列表 = 全量扫描。
//
// 这里同时做条数与编码长度两道校验（见 ErrTooManyEntities / ErrBackfillEntityIDsTooLarge），
// 并对每个 ID 复验形态：作业里的主体必须是能落库的形态，
// 否则要等到 worker 跑到那一行才发现整批白跑。
func (j *BackfillJob) EntityIDList() ([]string, error) {
	raw := strings.TrimSpace(j.EntityIDs)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > MaxBackfillEntityIDsBytes {
		return nil, fmt.Errorf("%w: %d bytes > %d", ErrBackfillEntityIDsTooLarge,
			len(raw), MaxBackfillEntityIDsBytes)
	}
	parts := strings.Split(raw, listSeparator)
	if len(parts) > MaxBackfillEntityIDs {
		return nil, fmt.Errorf("%w: %d ids > %d", ErrTooManyEntities, len(parts), MaxBackfillEntityIDs)
	}
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		if !ValidEntityID(j.EntityScope, id) {
			return nil, ErrEntityIDInvalid
		}
		if _, dup := seen[id]; dup {
			return nil, ErrDuplicateRowInBatch
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// FormatEntityIDs 校验并规范化显式主体列表（去重后升序，逗号连接）。
// 排序是必需的：断点游标按 entity_id 升序推进，乱序列表会导致续跑时漏掉前半段。
func FormatEntityIDs(scope int32, ids []string) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	if len(ids) > MaxBackfillEntityIDs {
		return "", fmt.Errorf("%w: %d ids > %d", ErrTooManyEntities, len(ids), MaxBackfillEntityIDs)
	}
	seen := make(map[string]struct{}, len(ids))
	cleaned := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if !ValidEntityID(scope, id) {
			return "", ErrEntityIDInvalid
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		cleaned = append(cleaned, id)
	}
	out := strings.Join(cleaned, listSeparator)
	if len(out) > MaxBackfillEntityIDsBytes {
		return "", fmt.Errorf("%w: %d bytes > %d", ErrBackfillEntityIDsTooLarge,
			len(out), MaxBackfillEntityIDsBytes)
	}
	return out, nil
}

// IsTerminal 判断作业是否终态。
func (j *BackfillJob) IsTerminal() bool { return j != nil && IsBackfillTerminal(j.State) }

// Progress 返回已完成比例（0..1），total 为 0 时返回 0，避免除零与「100% 的空作业」。
func (j *BackfillJob) Progress() float64 {
	if j == nil || j.EntitiesTotal <= 0 {
		return 0
	}
	done := float64(j.EntitiesDone + j.EntitiesFailed)
	if done > float64(j.EntitiesTotal) {
		return 1
	}
	return done / float64(j.EntitiesTotal)
}

// LeaseHeld 判断给定 owner 是否仍持有租约。
func (j *BackfillJob) LeaseHeld(owner string, now int64) bool {
	return j != nil && owner != "" && j.LeaseOwner == owner && j.LeaseExpireAt > now
}

// BackfillJobFilter 作业列表条件。
type BackfillJobFilter struct {
	FeatureKey string
	State      int32 // BackfillStateUnspecified = 全部
	Since      int64
	Pn         int32
	Ps         int32
}

// Normalize 夹取分页参数。
func (f *BackfillJobFilter) Normalize() {
	if f.Pn < 1 {
		f.Pn = 1
	}
	if f.Ps <= 0 || f.Ps > MaxListPageSize {
		f.Ps = MaxListPageSize
	}
}

// BackfillJobModel 抽象 feature_backfill_job 表。
type BackfillJobModel interface {
	// Insert 提交作业；命中 uniq_request_id 返回 ErrJobExists（logic 回查后 reused=true）。
	Insert(ctx context.Context, j *BackfillJob) (int64, error)
	// FindOne 按主键查询；未命中返回 ErrJobNotFound。
	FindOne(ctx context.Context, jobID int64) (*BackfillJob, error)
	// FindByRequestID 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*BackfillJob, error)
	// List 分页查询，job_id 倒序。
	List(ctx context.Context, f BackfillJobFilter) ([]*BackfillJob, int64, error)
	// Claim 认领一批可跑作业（PENDING，或 RUNNING 但租约已过期）并写入 owner 与到期时间。
	// 返回 false 表示被别的 worker 抢先。这是「同一作业不被两个 worker 同时推进」的关键。
	Claim(ctx context.Context, jobID int64, owner string, leaseSeconds int64) (bool, error)
	// ListClaimable 列出可认领作业（cron 巡检用），按 job_id 升序保证先到先得、不互相插队。
	ListClaimable(ctx context.Context, limit int32) ([]*BackfillJob, error)
	// AddProgress 心跳式推进：只有当前持有租约的 owner 才能写，游标必须单调不回退。
	// 心跳同时把租约续到 now+leaseSeconds（与 Claim 用同一个配置值，否则一次心跳就把
	// 长批次作业的租约缩回默认值，跑到一半被接管）。
	// 返回 false = 租约已被接管，worker 必须停止本作业而不是继续写值。
	AddProgress(ctx context.Context, jobID int64, owner string, done, failed int64,
		cursorEntityID int64, cursorEntityStr string, leaseSeconds int64) (bool, error)
	// Finish 收尾：写终态与最近错误。state=RUNNING + 租约持有双条件，重复收尾不覆盖终态。
	Finish(ctx context.Context, jobID int64, owner, toState, lastError string) (bool, error)
	// Cancel 由提交方取消（PENDING/RUNNING → CANCELLED）；终态返回 ErrJobAlreadyTerminal。
	Cancel(ctx context.Context, jobID int64, operator, reason string) (bool, error)
	// CountUnfinished 统计某 (key, version) 上 PENDING/RUNNING 的作业数。
	// UpdateFeatureState 在把版本切 ACTIVE 前必须看到 0：
	// 边回填边对外读 = 读到的值在无人复核的情况下持续变化。
	CountUnfinished(ctx context.Context, featureKey string, version int32) (int64, error)
	// CountSucceeded 统计某 (key, version) 已成功完成的作业数（「补过历史」的证据）。
	CountSucceeded(ctx context.Context, featureKey string, version int32) (int64, error)
}

type defaultBackfillJobModel struct {
	conn sqlx.SqlConn
}

// NewBackfillJobModel 构造 feature_backfill_job 的 sqlx 实现。
func NewBackfillJobModel(conn sqlx.SqlConn) BackfillJobModel {
	return &defaultBackfillJobModel{conn: conn}
}

const backfillJobSelect = "SELECT " + featureBackfillJobColumns + " FROM feature_backfill_job"

func (m *defaultBackfillJobModel) Insert(ctx context.Context, j *BackfillJob) (int64, error) {
	if err := ValidateBackfillJob(j); err != nil {
		return 0, err
	}
	if j.State == BackfillStateUnspecified {
		j.State = BackfillStatePending
	}
	now := nowUnix()
	if j.Ctime == 0 {
		j.Ctime = now
	}
	j.Mtime = j.Ctime
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO feature_backfill_job ("+featureBackfillJobInsertColumns+") VALUES ("+
			placeholders(featureBackfillJobInsertArgs)+") ON DUPLICATE KEY UPDATE mtime = mtime",
		j.FeatureKey, j.Version, j.EntityScope, j.Source, j.State,
		j.WindowFrom, j.WindowTo, j.EntityIDs, j.EntitiesTruncated, j.EntitiesTotal, j.EntitiesDone,
		j.EntitiesFailed, j.ProgressSeq, j.CursorEntityID, j.CursorEntityStr, j.LeaseOwner, j.LeaseExpireAt,
		j.AutoSwitch, j.FromVersion, j.RequestID, j.Operator, j.Reason, j.LastError, j.TraceID,
		j.Ctime, j.Mtime, j.StartedAt, j.FinishedAt)
	if err != nil {
		return 0, fmt.Errorf("feature_backfill_job Insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("feature_backfill_job Insert RowsAffected: %w", err)
	}
	if n == 0 {
		return 0, ErrJobExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("feature_backfill_job Insert LastInsertId: %w", err)
	}
	j.JobID = id
	return id, nil
}

func (m *defaultBackfillJobModel) FindOne(ctx context.Context, jobID int64) (*BackfillJob, error) {
	if jobID <= 0 {
		return nil, ErrJobNotFound
	}
	var row BackfillJob
	err := m.conn.QueryRowCtx(ctx, &row, backfillJobSelect+" WHERE job_id = ? LIMIT 1", jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, fmt.Errorf("feature_backfill_job FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultBackfillJobModel) FindByRequestID(ctx context.Context, requestID string) (*BackfillJob, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, ErrRequestIdRequired
	}
	var row BackfillJob
	err := m.conn.QueryRowCtx(ctx, &row, backfillJobSelect+" WHERE request_id = ? LIMIT 1", requestID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_backfill_job FindByRequestID: %w", err)
	}
	return &row, nil
}

func (m *defaultBackfillJobModel) List(ctx context.Context, f BackfillJobFilter) ([]*BackfillJob, int64, error) {
	f.Normalize()
	where := "WHERE 1 = 1"
	args := make([]any, 0, 3)
	if key := strings.TrimSpace(f.FeatureKey); key != "" {
		where += " AND feature_key = ?"
		args = append(args, key)
	}
	if f.State != BackfillStateUnspecified {
		if !ValidBackfillState(f.State) {
			return nil, 0, ErrJobStateInvalid
		}
		where += " AND state = ?"
		args = append(args, f.State)
	}
	if f.Since > 0 {
		where += " AND ctime >= ?"
		args = append(args, f.Since)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM feature_backfill_job "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("feature_backfill_job List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*BackfillJob
	query := backfillJobSelect + where + " ORDER BY job_id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("feature_backfill_job List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultBackfillJobModel) Claim(ctx context.Context, jobID int64, owner string,
	leaseSeconds int64) (bool, error) {
	if strings.TrimSpace(owner) == "" {
		return false, ErrOperatorRequired
	}
	if leaseSeconds <= 0 {
		leaseSeconds = defaultJobLeaseSeconds
	}
	now := nowUnix()
	// 一条条件更新覆盖两种可认领情形：新作业（PENDING）与租约过期的僵尸作业（RUNNING）。
	// 「僵尸作业能被接管」是回填能在 pod 漂移后继续跑完的前提。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feature_backfill_job SET state = ?, lease_owner = ?, lease_expire_at = ?,"+
			" started_at = IF(started_at = 0, ?, started_at), mtime = ?"+
			" WHERE job_id = ? AND (state = ? OR (state = ? AND lease_expire_at < ?))",
		BackfillStateRunning, owner, now+leaseSeconds, now, now, jobID,
		BackfillStatePending, BackfillStateRunning, now)
	if err != nil {
		return false, fmt.Errorf("feature_backfill_job Claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("feature_backfill_job Claim RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultBackfillJobModel) ListClaimable(ctx context.Context, limit int32) ([]*BackfillJob, error) {
	if limit <= 0 || limit > defaultJobBatchRows {
		limit = defaultJobBatchRows
	}
	now := nowUnix()
	var rows []*BackfillJob
	err := m.conn.QueryRowsCtx(ctx, &rows,
		backfillJobSelect+" WHERE state = ? OR (state = ? AND lease_expire_at < ?)"+
			" ORDER BY job_id ASC LIMIT ?",
		BackfillStatePending, BackfillStateRunning, now, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_backfill_job ListClaimable: %w", err)
	}
	return rows, nil
}

func (m *defaultBackfillJobModel) AddProgress(ctx context.Context, jobID int64, owner string,
	done, failed int64, cursorEntityID int64, cursorEntityStr string, leaseSeconds int64) (bool, error) {
	if strings.TrimSpace(owner) == "" {
		return false, ErrOperatorRequired
	}
	if done < 0 || failed < 0 {
		return false, ErrMalformedValue
	}
	if leaseSeconds <= 0 {
		leaseSeconds = defaultJobLeaseSeconds
	}
	now := nowUnix()
	// 心跳同时是租约续期：不续租的作业会在长批次跑到一半时被别人接管，两半合起来
	// 就是「同一主体被写了两遍不同口径的值」。
	// 游标用条件表达式保证单调：慢 worker 的旧游标不能把快 worker 的新游标拽回去。
	// progress_seq 无条件 +1 保证「命中即改变」，RowsAffected 才是可信的持有者判据
	// （见 ProgressSeq 字段注释）。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feature_backfill_job SET entities_done = entities_done + ?,"+
			" entities_failed = entities_failed + ?, progress_seq = progress_seq + 1,"+
			" lease_expire_at = ?, cursor_entity_id = IF(? > cursor_entity_id, ?, cursor_entity_id),"+
			" cursor_entity_str = IF(? > cursor_entity_str, ?, cursor_entity_str), mtime = ?"+
			" WHERE job_id = ? AND state = ? AND lease_owner = ? AND lease_expire_at > ?",
		done, failed, now+leaseSeconds, cursorEntityID, cursorEntityID, cursorEntityStr,
		cursorEntityStr, now, jobID, BackfillStateRunning, owner, now)
	if err != nil {
		return false, fmt.Errorf("feature_backfill_job AddProgress: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("feature_backfill_job AddProgress RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultBackfillJobModel) Finish(ctx context.Context, jobID int64, owner, toStateStr,
	lastError string) (bool, error) {
	toState, err := backfillStateFromName(toStateStr)
	if err != nil {
		return false, err
	}
	if !IsBackfillTerminal(toState) {
		return false, ErrJobBadTransition
	}
	if strings.TrimSpace(owner) == "" {
		return false, ErrOperatorRequired
	}
	if len(lastError) > maxJobErrorLen {
		lastError = truncateRunes(lastError, maxJobErrorLen)
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feature_backfill_job SET state = ?, last_error = ?, finished_at = ?, mtime = ?,"+
			" lease_owner = '', lease_expire_at = ?"+
			" WHERE job_id = ? AND state = ? AND lease_owner = ? AND lease_expire_at > ?",
		toState, lastError, now, now, now, jobID, BackfillStateRunning, owner, now)
	if err != nil {
		return false, fmt.Errorf("feature_backfill_job Finish: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("feature_backfill_job Finish RowsAffected: %w", err)
	}
	return n == 1, nil
}

const maxJobErrorLen = 512

func (m *defaultBackfillJobModel) Cancel(ctx context.Context, jobID int64, operator, reason string) (bool, error) {
	if strings.TrimSpace(operator) == "" {
		return false, ErrOperatorRequired
	}
	if strings.TrimSpace(reason) == "" {
		return false, ErrReasonRequired
	}
	job, err := m.FindOne(ctx, jobID)
	if err != nil {
		return false, err
	}
	if job.IsTerminal() {
		return false, ErrJobAlreadyTerminal
	}
	if !ValidBackfillTransition(job.State, BackfillStateCancelled) {
		return false, ErrJobBadTransition
	}
	now := nowUnix()
	if len(reason) > maxJobErrorLen {
		reason = truncateRunes(reason, maxJobErrorLen)
	}
	// 取消不要求租约：提交方（运营）中止一个正在跑的作业是合法诉求；
	// 但必须带 operator + reason，且只作用于 PENDING/RUNNING。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feature_backfill_job SET state = ?, finished_at = ?, mtime = ?, last_error = ?,"+
			" lease_owner = '', lease_expire_at = ?"+
			" WHERE job_id = ? AND state IN (?, ?)",
		BackfillStateCancelled, now, now, "cancelled by "+operator+": "+reason, now, jobID,
		BackfillStatePending, BackfillStateRunning)
	if err != nil {
		return false, fmt.Errorf("feature_backfill_job Cancel: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("feature_backfill_job Cancel RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultBackfillJobModel) CountUnfinished(ctx context.Context, featureKey string, version int32) (int64, error) {
	return m.countByState(ctx, featureKey, version, []any{BackfillStatePending, BackfillStateRunning})
}

func (m *defaultBackfillJobModel) CountSucceeded(ctx context.Context, featureKey string, version int32) (int64, error) {
	return m.countByState(ctx, featureKey, version, []any{BackfillStateSucceeded})
}

func (m *defaultBackfillJobModel) countByState(ctx context.Context, featureKey string, version int32,
	states []any) (int64, error) {
	if strings.TrimSpace(featureKey) == "" {
		return 0, ErrFeatureKeyRequired
	}
	if version < 1 {
		return 0, ErrFeatureVersionRequired
	}
	args := append([]any{featureKey, version}, states...)
	var count int64
	err := m.conn.QueryRowCtx(ctx, &count,
		"SELECT COUNT(*) FROM feature_backfill_job WHERE feature_key = ? AND version = ? AND state IN ("+
			placeholders(len(states))+") LIMIT 1", args...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("feature_backfill_job countByState: %w", err)
	}
	return count, nil
}

// backfillStateFromName 把 worker 传入的终态名映射为枚举值（避免把枚举数字散在调用方）。
func backfillStateFromName(name string) (int32, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "succeeded", strconv.Itoa(int(BackfillStateSucceeded)):
		return BackfillStateSucceeded, nil
	case "failed", strconv.Itoa(int(BackfillStateFailed)):
		return BackfillStateFailed, nil
	case "cancelled", "canceled", strconv.Itoa(int(BackfillStateCancelled)):
		return BackfillStateCancelled, nil
	default:
		return 0, ErrJobStateInvalid
	}
}

// truncateRunes 按 rune 截断，避免把中文错误信息切成半个字（AGENTS.md：文档与配置统一 UTF-8）。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
