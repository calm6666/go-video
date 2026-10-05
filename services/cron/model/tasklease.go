package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// taskLeaseColumns 必须与
// deploy/migrations/cron/000002_create_cron_run_lease_checkpoint_tables.sql 一致。
const taskLeaseColumns = "id, lease_key, task_key, scope, owner_instance, fence_token, acquired_at, " +
	"renewed_at, expire_at, takeover_count, run_id, ctime, mtime"

// TaskLease 任务级互斥租约（cron_task_lease 表）。
//
// 为什么需要它：cron_task_run 的租约只守护「某一次执行」，而报表/清理/重建类任务
// 需要的是「同一时刻整个任务只允许一个实例在跑」（不同计划时刻也不能并行），
// 由这张表按 lease_key 提供互斥。lease_key 默认等于 task_key，带分片时为
// task_key + "/" + scope（如 archive.pgc/shard=7）。
//
// 可抢占 + TTL：expire_at 到期即视为持有者崩溃，任何实例都能抢占；抢占走
// SELECT ... FOR UPDATE 的事务路径（Acquire），同时把 fence_token 递增。
// 旧持有者随后的心跳/上报会因 fence_token 不一致拿到 ErrLeaseLost，
// 从而停止向下游 RPC 写入 —— 这是「不会有两个实例同时推进同一任务」的最后一道闸。
type TaskLease struct {
	ID            int64  `db:"id"`
	LeaseKey      string `db:"lease_key"`      // 唯一键
	TaskKey       string `db:"task_key"`       // 冗余任务键，便于按任务查询
	Scope         string `db:"scope"`          // 分片键，空表示任务级全局互斥
	OwnerInstance string `db:"owner_instance"` // 当前持有实例，空表示无人持有
	FenceToken    int64  `db:"fence_token"`    // 每次抢占 +1
	AcquiredAt    int64  `db:"acquired_at"`    // 本次持有的开始时间
	RenewedAt     int64  `db:"renewed_at"`     // 最近一次续租时间
	ExpireAt      int64  `db:"expire_at"`      // 到期时间，<=now 即可被抢占
	TakeoverCount int32  `db:"takeover_count"` // 因过期被抢占的次数（实例稳定性信号）
	RunID         int64  `db:"run_id"`         // 当前持有租约对应的执行记录
	Ctime         int64  `db:"ctime"`
	Mtime         int64  `db:"mtime"`
}

// LeaseAcquire 抢占租约的结果。
type LeaseAcquire struct {
	// Acquired 表示本次调用是否取得（或本就持有）租约。
	Acquired bool
	// TakenOver 表示本次抢占顶掉了一个已过期的其它实例持有者（fence_token 已递增）。
	TakenOver bool
	// Lease 是抢占后的最终租约快照，调用方把 FenceToken 带给处理器。
	Lease *TaskLease
}

// TaskLeaseModel cron_task_lease 表读写接口。
type TaskLeaseModel interface {
	// Acquire 在事务内以 SELECT ... FOR UPDATE 抢占租约：
	// 无人持有 / 已过期 / 自己已持有 三种情况都会成功，其余返回 Acquired=false。
	// 抢占他人租约时 fence_token+1 并累加 takeover_count。
	// ttlSeconds 必须为正；调用方（AcquireLease logic）用任务定义的 lease_ttl_seconds 兜底。
	Acquire(ctx context.Context, leaseKey, taskKey, scope, owner string, ttlSeconds int64, runID int64) (*LeaseAcquire, error)
	// Renew 续租：owner 与 fence_token 都必须匹配，否则返回 false（租约已丢失）。
	Renew(ctx context.Context, leaseKey, owner string, fenceToken int64, ttlSeconds int64) (bool, error)
	// Release 释放并交回租约：只有当前持有者能释放，fence_token 保持不回退。
	Release(ctx context.Context, leaseKey, owner string, fenceToken int64) (bool, error)
	// FindOne 按 lease_key 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, leaseKey string) (*TaskLease, error)
	// ListByCursor 按 expire_at 升序游标分页（cursorID 为上一页最后一行 id，0 表示从头）。
	// onlyExpired 为 true 时只返回已过期可抢占的租约。
	ListByCursor(ctx context.Context, taskKey string, onlyExpired bool, now, cursorID int64, limit int) ([]*TaskLease, int64, error)
	// CountExpired 统计已过期未释放的租约数（GetSchedulerHealth 用）。
	CountExpired(ctx context.Context, now int64) (int64, error)

	// --- 事务变体与按 run 反查（实现见 tx.go）---

	AcquireTx(ctx context.Context, tx sqlx.Session, leaseKey, taskKey, scope, owner string,
		ttlSeconds, runID int64) (*LeaseAcquire, error)
	RenewTx(ctx context.Context, tx sqlx.Session, leaseKey, owner string, fenceToken, ttlSeconds int64) (bool, error)
	ReleaseTx(ctx context.Context, tx sqlx.Session, leaseKey, owner string, fenceToken int64) (bool, error)
	FindOneTx(ctx context.Context, tx sqlx.Session, leaseKey string) (*TaskLease, error)
	// FindByRun 按 run_id 反查租约：RenewLease/ReleaseLease 请求里只有 run_id，
	// 而带 scope 的 lease_key 无法从请求推出（契约缺口，见 README）。
	FindByRun(ctx context.Context, runID int64) (*TaskLease, error)
	// FindByTask 按 (task_key, owner, fence_token) 定位租约，用于 run_id 已被接管实例改写的情形。
	FindByTask(ctx context.Context, taskKey, owner string, fenceToken int64) (*TaskLease, error)
	CountByFilter(ctx context.Context, taskKey string, onlyExpired bool, now int64) (int64, error)
	// CountExpiredByGroup 按分组统计已过期未释放的租约数（分组健康度）。
	CountExpiredByGroup(ctx context.Context, now int64) ([]GroupCount, error)
}

type defaultTaskLeaseModel struct {
	conn sqlx.SqlConn
}

// NewTaskLeaseModel 创建 TaskLeaseModel 实现。
func NewTaskLeaseModel(conn sqlx.SqlConn) TaskLeaseModel {
	return &defaultTaskLeaseModel{conn: conn}
}

func (m *defaultTaskLeaseModel) Acquire(
	ctx context.Context, leaseKey, taskKey, scope, owner string, ttlSeconds int64, runID int64,
) (*LeaseAcquire, error) {
	if leaseKey == "" {
		return nil, errors.New("cron: lease_key is required")
	}
	if owner == "" {
		return nil, errors.New("cron: lease owner is required")
	}
	if ttlSeconds < MinLeaseTTLSeconds {
		return nil, fmt.Errorf("%w: ttl=%d, minimum=%d", ErrInvalidLeaseTTL, ttlSeconds, MinLeaseTTLSeconds)
	}
	now := nowUnix()
	out := &LeaseAcquire{}
	err := m.conn.TransactCtx(ctx, func(ctx context.Context, tx sqlx.Session) error {
		var lease TaskLease
		query := "SELECT " + taskLeaseColumns + " FROM cron_task_lease WHERE lease_key = ? FOR UPDATE"
		err := tx.QueryRowCtx(ctx, &lease, query, leaseKey)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// 首次持有：fence_token 从 1 起。
			insert := "INSERT INTO cron_task_lease (lease_key, task_key, scope, owner_instance, fence_token, " +
				"acquired_at, renewed_at, expire_at, takeover_count, run_id, ctime, mtime) " +
				"VALUES (?, ?, ?, ?, 1, ?, ?, ?, 0, ?, ?, ?)"
			if _, err := tx.ExecCtx(ctx, insert, leaseKey, taskKey, scope, owner,
				now, now, now+ttlSeconds, runID, now, now); err != nil {
				return fmt.Errorf("cron_task_lease Acquire insert(%s): %w", leaseKey, err)
			}
			lease = TaskLease{
				LeaseKey: leaseKey, TaskKey: taskKey, Scope: scope, OwnerInstance: owner,
				FenceToken: 1, AcquiredAt: now, RenewedAt: now, ExpireAt: now + ttlSeconds, RunID: runID,
			}
			out.Acquired = true
			out.Lease = &lease
			return nil
		case err != nil:
			return fmt.Errorf("cron_task_lease Acquire select(%s): %w", leaseKey, err)
		}

		switch {
		case lease.OwnerInstance == owner && lease.FenceToken > 0 && lease.ExpireAt > now:
			// 自己已持有：只续期，不递增栅栏，保证 AcquireLease 可安全重入。
			if _, err := tx.ExecCtx(ctx,
				"UPDATE cron_task_lease SET renewed_at = ?, expire_at = ?, run_id = ?, mtime = ? "+
					"WHERE lease_key = ? AND fence_token = ?",
				now, now+ttlSeconds, runID, now, leaseKey, lease.FenceToken); err != nil {
				return fmt.Errorf("cron_task_lease Acquire renew-self(%s): %w", leaseKey, err)
			}
			lease.RenewedAt = now
			lease.ExpireAt = now + ttlSeconds
			lease.RunID = runID
			out.Acquired = true
			out.Lease = &lease
			return nil
		case lease.ExpireAt <= now:
			// 过期即可抢占：fence_token 递增让旧持有者的上报必然失败。
			newFence := lease.FenceToken + 1
			if _, err := tx.ExecCtx(ctx,
				"UPDATE cron_task_lease SET owner_instance = ?, fence_token = ?, acquired_at = ?, "+
					"renewed_at = ?, expire_at = ?, takeover_count = takeover_count + 1, run_id = ?, mtime = ? "+
					"WHERE lease_key = ? AND fence_token = ? AND expire_at <= ?",
				owner, newFence, now, now, now+ttlSeconds, runID, now, leaseKey, lease.FenceToken, now); err != nil {
				return fmt.Errorf("cron_task_lease Acquire takeover(%s): %w", leaseKey, err)
			}
			lease.OwnerInstance = owner
			lease.FenceToken = newFence
			lease.AcquiredAt = now
			lease.RenewedAt = now
			lease.ExpireAt = now + ttlSeconds
			lease.TakeoverCount++
			lease.RunID = runID
			out.Acquired = true
			out.TakenOver = true
			out.Lease = &lease
			return nil
		default:
			// 其它实例仍在有效期内持有：不递增栅栏、不改任何列。
			out.Acquired = false
			out.Lease = &lease
			return nil
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (m *defaultTaskLeaseModel) Renew(
	ctx context.Context, leaseKey, owner string, fenceToken int64, ttlSeconds int64,
) (bool, error) {
	if ttlSeconds <= 0 {
		return false, ErrInvalidLeaseTTL
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cron_task_lease SET renewed_at = ?, expire_at = ?, mtime = ? "+
			"WHERE lease_key = ? AND owner_instance = ? AND fence_token = ? AND expire_at > ?",
		now, now+ttlSeconds, now, leaseKey, owner, fenceToken, now)
	if err != nil {
		return false, fmt.Errorf("cron_task_lease Renew(%s): %w", leaseKey, err)
	}
	return rowsAffected(res)
}

func (m *defaultTaskLeaseModel) Release(
	ctx context.Context, leaseKey, owner string, fenceToken int64,
) (bool, error) {
	now := nowUnix()
	// 释放不重置 fence_token：单调递增的令牌一旦发出就不能回收，
	// 否则被抢占前的旧实例可能在令牌复用上「复活」。expire_at 归零即表示无人持有。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cron_task_lease SET owner_instance = '', expire_at = 0, run_id = 0, mtime = ? "+
			"WHERE lease_key = ? AND owner_instance = ? AND fence_token = ?",
		now, leaseKey, owner, fenceToken)
	if err != nil {
		return false, fmt.Errorf("cron_task_lease Release(%s): %w", leaseKey, err)
	}
	return rowsAffected(res)
}

func (m *defaultTaskLeaseModel) FindOne(ctx context.Context, leaseKey string) (*TaskLease, error) {
	var l TaskLease
	query := "SELECT " + taskLeaseColumns + " FROM cron_task_lease WHERE lease_key = ?"
	if err := m.conn.QueryRowCtx(ctx, &l, query, leaseKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cron_task_lease FindOne(%s): %w", leaseKey, err)
	}
	return &l, nil
}

func (m *defaultTaskLeaseModel) ListByCursor(
	ctx context.Context, taskKey string, onlyExpired bool, now, cursorID int64, limit int,
) ([]*TaskLease, int64, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	where := "1 = 1"
	var args []any
	if taskKey != "" {
		where += " AND task_key = ?"
		args = append(args, taskKey)
	}
	if onlyExpired {
		where += " AND expire_at > 0 AND expire_at <= ?"
		args = append(args, now)
	}
	if cursorID > 0 {
		where += " AND id > ?"
		args = append(args, cursorID)
	}
	args = append(args, limit)

	var rows []*TaskLease
	query := "SELECT " + taskLeaseColumns + " FROM cron_task_lease WHERE " + where +
		" ORDER BY expire_at ASC, id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("cron_task_lease ListByCursor: %w", err)
	}
	next := int64(0)
	if len(rows) == limit {
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}

func (m *defaultTaskLeaseModel) CountExpired(ctx context.Context, now int64) (int64, error) {
	var total int64
	query := "SELECT COUNT(*) FROM cron_task_lease WHERE owner_instance <> '' AND expire_at > 0 AND expire_at <= ?"
	if err := m.conn.QueryRowCtx(ctx, &total, query, now); err != nil {
		return 0, fmt.Errorf("cron_task_lease CountExpired: %w", err)
	}
	return total, nil
}
