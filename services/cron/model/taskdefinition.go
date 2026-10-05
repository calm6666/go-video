package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// taskDefinitionColumns 必须与
// deploy/migrations/cron/000001_create_cron_task_tables.sql 完全一致。
const taskDefinitionColumns = "id, task_key, name, handler, task_group, schedule_type, cron_expr, " +
	"interval_seconds, timezone, timeout_seconds, max_attempts, retry_base_seconds, retry_max_seconds, " +
	"concurrency_limit, lease_ttl_seconds, misfire_policy, misfire_backfill_limit, params, secret_refs, " +
	"state, next_fire_at, last_fire_at, last_success_at, last_error, version, owner, operator, ctime, mtime"

// TaskDefinition 任务定义（cron_task_definition 表）。
//
// 它是「调度事实源」而不是业务事实源：任务能不能跑、跑多久、失败怎么退避都在这里，
// 业务状态一律由处理器通过领域 RPC 推进（AGENTS.md §5、§8）。
//
// 幂等身份：task_key 唯一。注册表（internal/registry）用 handler 名把进程内实现
// 与这里的行为定义绑起来；进程启动时若 DB 有定义而注册表没有对应 handler，
// 该任务判 ErrHandlerNotRegistered 失败，绝不静默跳过（AGENTS.md §9）。
type TaskDefinition struct {
	ID                 int64  `db:"id"`                     // 自增主键
	TaskKey            string `db:"task_key"`               // 任务唯一键，如 rights.expire_scan
	Name               string `db:"name"`                   // 展示名
	Handler            string `db:"handler"`                // 进程内注册表键
	TaskGroup          string `db:"task_group"`             // 分组，用于批量暂停与健康统计
	ScheduleType       int32  `db:"schedule_type"`          // 1 cron / 2 interval / 3 manual
	CronExpr           string `db:"cron_expr"`              // schedule_type=1
	IntervalSeconds    int32  `db:"interval_seconds"`       // schedule_type=2
	Timezone           string `db:"timezone"`               // cron 表达式时区
	TimeoutSeconds     int32  `db:"timeout_seconds"`        // 单次执行超时
	MaxAttempts        int32  `db:"max_attempts"`           // 含首次的最大尝试次数
	RetryBaseSeconds   int32  `db:"retry_base_seconds"`     // 退避基数
	RetryMaxSeconds    int32  `db:"retry_max_seconds"`      // 退避上限
	ConcurrencyLimit   int32  `db:"concurrency_limit"`      // 同任务并行上限，1 表示串行
	LeaseTTLSeconds    int32  `db:"lease_ttl_seconds"`      // 租约 TTL，过期可被抢占
	MisfirePolicy      int32  `db:"misfire_policy"`         // 过期计划点处理策略
	MisfireBackfillLim int32  `db:"misfire_backfill_limit"` // FIRE_ALL 时单轮最多补齐的计划点数
	Params             string `db:"params"`                 // 处理器参数 JSON 文本（不含密钥）
	SecretRefs         string `db:"secret_refs"`            // 逗号分隔的环境变量名，不存密钥值
	State              int32  `db:"state"`                  // 1 enabled / 2 paused / 3 disabled
	NextFireAt         int64  `db:"next_fire_at"`           // 下一个计划时刻（Unix 秒），0 表示不参与到期扫描
	LastFireAt         int64  `db:"last_fire_at"`           // 最近一次产生执行记录的计划时刻
	LastSuccessAt      int64  `db:"last_success_at"`        // 最近一次成功完成时间
	LastError          string `db:"last_error"`             // 最近一次失败摘要
	Version            int64  `db:"version"`                // 乐观锁版本
	Owner              string `db:"owner"`                  // 责任团队/服务
	Operator           string `db:"operator"`               // 最近一次变更操作人
	Ctime              int64  `db:"ctime"`
	Mtime              int64  `db:"mtime"`
}

// TaskDefinitionModel cron_task_definition 表读写接口。
type TaskDefinitionModel interface {
	// Insert 注册任务定义；命中 uniq_task_key 时不覆盖既有行，返回 existed=true。
	Insert(ctx context.Context, d *TaskDefinition) (existed bool, err error)
	// FindOne 按 task_key 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, taskKey string) (*TaskDefinition, error)
	// ListByCursor 按 id 升序游标分页（cursorID 为上一页最后一行 id，0 表示从头）。
	// state 为 TaskStateUnspecified 表示不过滤。返回行、下一页游标（无更多为 0）。
	ListByCursor(ctx context.Context, state int32, group, handler string, cursorID int64, limit int) ([]*TaskDefinition, int64, error)
	// UpdateMutable 更新可调度属性（不含 state/version/时间戳），expectedVersion 不一致返回 false。
	// 成功时 version 自动 +1。
	UpdateMutable(ctx context.Context, d *TaskDefinition, expectedVersion int64) (bool, error)
	// SetState 以乐观锁推进任务状态（enabled/paused/disabled）。
	// fromState 作为额外条件（TaskStateUnspecified 表示不校验来源状态），
	// 避免并发暂停/恢复互相覆盖；返回 false 表示未命中（需重读后重试）。
	SetState(ctx context.Context, taskKey string, fromState, toState int32, expectedVersion int64, operator string) (bool, error)
	// ListDue 返回到期可执行的启用任务：state=ENABLED 且 0 < next_fire_at <= now+lookahead。
	// group 为空表示全部分组。结果按 next_fire_at 升序，优先处理积压最久的任务。
	ListDue(ctx context.Context, now, lookaheadSeconds int64, group string, limit int) ([]*TaskDefinition, error)
	// MarkFired 推进调度指针：last_fire_at=plannedAt、next_fire_at=nextFireAt（可为 0 表示手动任务）。
	// 只有 state=ENABLED 的任务会被推进，防止暂停中的任务被补跑改写计划。
	MarkFired(ctx context.Context, taskKey string, plannedAt, nextFireAt int64) error
	// MarkResult 回写任务级最近结果（成功时间/最后错误），供运营列表直接读，不做聚合。
	MarkResult(ctx context.Context, taskKey string, successAt int64, lastError string) error
	// CountByGroupState 统计各 (task_group, state) 的任务数，供 GetSchedulerHealth 使用。
	CountByGroupState(ctx context.Context) ([]GroupStateCount, error)

	// --- 事务变体与统计（实现见 tx.go：SQL 不复制，只是把模型指向事务会话）---
	// logic 需要把「写定义 + 写审计」放进同一个事务，审计失败必须整体回滚。

	InsertTx(ctx context.Context, tx sqlx.Session, d *TaskDefinition) (bool, error)
	UpdateMutableTx(ctx context.Context, tx sqlx.Session, d *TaskDefinition, expectedVersion int64) (bool, error)
	SetStateTx(ctx context.Context, tx sqlx.Session, taskKey string, fromState, toState int32,
		expectedVersion int64, operator string) (bool, error)
	MarkFiredTx(ctx context.Context, tx sqlx.Session, taskKey string, plannedAt, nextFireAt int64) error
	MarkResultTx(ctx context.Context, tx sqlx.Session, taskKey string, successAt int64, lastError string) error
	FindOneTx(ctx context.Context, tx sqlx.Session, taskKey string) (*TaskDefinition, error)
	CountByFilter(ctx context.Context, state int32, group, handler string) (int64, error)
	// SetNextFireAtTx 单独写调度指针（恢复任务重算 / 禁用任务置零时用），
	// 不带状态守卫，调用方必须在同一事务内先用乐观锁命中。
	SetNextFireAtTx(ctx context.Context, tx sqlx.Session, taskKey string, nextFireAt int64) error
}

// GroupStateCount 一个分组在某状态下的任务数量。
type GroupStateCount struct {
	TaskGroup string `db:"task_group"`
	State     int32  `db:"state"`
	Count     int64  `db:"total"`
}

type defaultTaskDefinitionModel struct {
	conn sqlx.SqlConn
}

// NewTaskDefinitionModel 创建 TaskDefinitionModel 实现。
func NewTaskDefinitionModel(conn sqlx.SqlConn) TaskDefinitionModel {
	return &defaultTaskDefinitionModel{conn: conn}
}

// Insert 用 ON DUPLICATE KEY UPDATE id=id 区分「首次注册」与「重复注册」：
// 重复键时 affected=0，调用方据此返回幂等结果而不是覆盖线上调度。
func (m *defaultTaskDefinitionModel) Insert(ctx context.Context, d *TaskDefinition) (bool, error) {
	if d.TaskKey == "" {
		return false, ErrTaskKeyEmpty
	}
	now := nowUnix()
	if d.Ctime == 0 {
		d.Ctime = now
	}
	d.Mtime = now
	if d.Version == 0 {
		d.Version = 1
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO cron_task_definition (task_key, name, handler, task_group, schedule_type, cron_expr, "+
			"interval_seconds, timezone, timeout_seconds, max_attempts, retry_base_seconds, retry_max_seconds, "+
			"concurrency_limit, lease_ttl_seconds, misfire_policy, misfire_backfill_limit, params, secret_refs, "+
			"state, next_fire_at, last_fire_at, last_success_at, last_error, version, owner, operator, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE id = id",
		d.TaskKey, d.Name, d.Handler, d.TaskGroup, d.ScheduleType, d.CronExpr,
		d.IntervalSeconds, d.Timezone, d.TimeoutSeconds, d.MaxAttempts, d.RetryBaseSeconds, d.RetryMaxSeconds,
		d.ConcurrencyLimit, d.LeaseTTLSeconds, d.MisfirePolicy, d.MisfireBackfillLim, d.Params, d.SecretRefs,
		d.State, d.NextFireAt, d.LastFireAt, d.LastSuccessAt, d.LastError, d.Version, d.Owner, d.Operator,
		d.Ctime, d.Mtime)
	if err != nil {
		return false, fmt.Errorf("cron_task_definition Insert: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cron_task_definition Insert RowsAffected: %w", err)
	}
	return affected == 0, nil
}

func (m *defaultTaskDefinitionModel) FindOne(ctx context.Context, taskKey string) (*TaskDefinition, error) {
	if taskKey == "" {
		return nil, ErrTaskKeyEmpty
	}
	var d TaskDefinition
	query := "SELECT " + taskDefinitionColumns + " FROM cron_task_definition WHERE task_key = ?"
	if err := m.conn.QueryRowCtx(ctx, &d, query, taskKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_definition FindOne(%s): %w", taskKey, err)
	}
	return &d, nil
}

func (m *defaultTaskDefinitionModel) ListByCursor(
	ctx context.Context, state int32, group, handler string, cursorID int64, limit int,
) ([]*TaskDefinition, int64, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	where := "1 = 1"
	var args []any
	if state != TaskStateUnspecified {
		where += " AND state = ?"
		args = append(args, state)
	}
	if group != "" {
		where += " AND task_group = ?"
		args = append(args, group)
	}
	if handler != "" {
		where += " AND handler = ?"
		args = append(args, handler)
	}
	if cursorID > 0 {
		where += " AND id > ?"
		args = append(args, cursorID)
	}
	args = append(args, limit)

	var rows []*TaskDefinition
	query := "SELECT " + taskDefinitionColumns + " FROM cron_task_definition WHERE " + where +
		" ORDER BY id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("cron_task_definition ListByCursor: %w", err)
	}
	next := int64(0)
	if len(rows) == limit {
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}

func (m *defaultTaskDefinitionModel) UpdateMutable(
	ctx context.Context, d *TaskDefinition, expectedVersion int64,
) (bool, error) {
	if d.TaskKey == "" {
		return false, ErrTaskKeyEmpty
	}
	if expectedVersion <= 0 {
		return false, ErrVersionConflict
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cron_task_definition SET name = ?, handler = ?, task_group = ?, schedule_type = ?, "+
			"cron_expr = ?, interval_seconds = ?, timezone = ?, timeout_seconds = ?, max_attempts = ?, "+
			"retry_base_seconds = ?, retry_max_seconds = ?, concurrency_limit = ?, lease_ttl_seconds = ?, "+
			"misfire_policy = ?, misfire_backfill_limit = ?, params = ?, secret_refs = ?, owner = ?, "+
			"operator = ?, version = version + 1, mtime = ? "+
			"WHERE task_key = ? AND version = ?",
		d.Name, d.Handler, d.TaskGroup, d.ScheduleType, d.CronExpr, d.IntervalSeconds, d.Timezone,
		d.TimeoutSeconds, d.MaxAttempts, d.RetryBaseSeconds, d.RetryMaxSeconds, d.ConcurrencyLimit,
		d.LeaseTTLSeconds, d.MisfirePolicy, d.MisfireBackfillLim, d.Params, d.SecretRefs, d.Owner,
		d.Operator, nowUnix(), d.TaskKey, expectedVersion)
	if err != nil {
		return false, fmt.Errorf("cron_task_definition UpdateMutable(%s): %w", d.TaskKey, err)
	}
	return rowsAffected(res)
}

func (m *defaultTaskDefinitionModel) SetState(
	ctx context.Context, taskKey string, fromState, toState int32, expectedVersion int64, operator string,
) (bool, error) {
	if taskKey == "" {
		return false, ErrTaskKeyEmpty
	}
	// 暂停/恢复时同步调度指针：暂停即清空 next_fire_at（不再产生计划），
	// 恢复时保持 next_fire_at 不变，由 logic 层按 MisfirePolicy 决定是否补跑。
	query := "UPDATE cron_task_definition SET state = ?, " +
		"next_fire_at = CASE WHEN ? = ? THEN 0 ELSE next_fire_at END, " +
		"operator = ?, version = version + 1, mtime = ? " +
		"WHERE task_key = ? AND version = ?"
	args := []any{toState, toState, TaskStatePaused, operator, nowUnix(), taskKey, expectedVersion}
	if fromState != TaskStateUnspecified {
		query += " AND state = ?"
		args = append(args, fromState)
	}
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("cron_task_definition SetState(%s): %w", taskKey, err)
	}
	return rowsAffected(res)
}

func (m *defaultTaskDefinitionModel) ListDue(
	ctx context.Context, now, lookaheadSeconds int64, group string, limit int,
) ([]*TaskDefinition, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if lookaheadSeconds < 0 {
		lookaheadSeconds = 0
	}
	where := "state = ? AND next_fire_at > 0 AND next_fire_at <= ?"
	args := []any{TaskStateEnabled, now + lookaheadSeconds}
	if group != "" {
		where += " AND task_group = ?"
		args = append(args, group)
	}
	args = append(args, limit)

	var rows []*TaskDefinition
	query := "SELECT " + taskDefinitionColumns + " FROM cron_task_definition WHERE " + where +
		" ORDER BY next_fire_at ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_definition ListDue: %w", err)
	}
	return rows, nil
}

func (m *defaultTaskDefinitionModel) MarkFired(
	ctx context.Context, taskKey string, plannedAt, nextFireAt int64,
) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE cron_task_definition SET last_fire_at = ?, next_fire_at = ?, mtime = ? "+
			"WHERE task_key = ? AND state = ? AND last_fire_at <= ?",
		plannedAt, nextFireAt, nowUnix(), taskKey, TaskStateEnabled, plannedAt)
	if err != nil {
		return fmt.Errorf("cron_task_definition MarkFired(%s): %w", taskKey, err)
	}
	return nil
}

func (m *defaultTaskDefinitionModel) MarkResult(
	ctx context.Context, taskKey string, successAt int64, lastError string,
) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE cron_task_definition SET last_success_at = GREATEST(last_success_at, ?), "+
			"last_error = ?, mtime = ? WHERE task_key = ?",
		successAt, truncate(lastError, MaxLastErrorBytes), nowUnix(), taskKey)
	if err != nil {
		return fmt.Errorf("cron_task_definition MarkResult(%s): %w", taskKey, err)
	}
	return nil
}

func (m *defaultTaskDefinitionModel) CountByGroupState(ctx context.Context) ([]GroupStateCount, error) {
	var rows []GroupStateCount
	query := "SELECT task_group, state, COUNT(*) AS total FROM cron_task_definition " +
		"GROUP BY task_group, state ORDER BY task_group ASC, state ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_definition CountByGroupState: %w", err)
	}
	return rows, nil
}

// rowsAffected 把 Exec 结果转成 CAS 命中与否，统一错误包装。
// 调用方已处理 ExecCtx 自身的 error，这里只负责 RowsAffected 的读取。
func rowsAffected(res sql.Result) (bool, error) {
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cron: RowsAffected: %w", err)
	}
	return affected > 0, nil
}

// truncate 按字节截断长文本，保证入库字段不撑爆列宽（错误原因常有堆栈风险）。
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
