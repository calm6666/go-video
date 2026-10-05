// Package repository 是 moderation-worker 服务的数据访问层。
// 组合 worker_task model，为 logic 层提供统一数据访问入口。
// 任务幂等：worker_task_id 主键唯一；(task_id, capability) 联合唯一约束防重入。
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/moderation-worker/model"
)

// Cache 封装 moderation-worker 的 Redis 缓存与幂等锁操作。
// 任务运行中通过 SETNX 抢锁防止并发重入；结果片段短缓存加速 GetTaskResult。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixTaskLock   = "mw:lock:%s" // worker_task_id → 运行中锁
	prefixTaskResult = "mw:res:%s"  // worker_task_id → 结果 JSON 短缓存

	cacheTTLTaskLock   = 600 // 锁 TTL 秒，需大于最大任务耗时；占位用 10 分钟
	cacheTTLTaskResult = 60
)

func keyTaskLock(workerTaskID string) string   { return fmt.Sprintf(prefixTaskLock, workerTaskID) }
func keyTaskResult(workerTaskID string) string { return fmt.Sprintf(prefixTaskResult, workerTaskID) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("moderation-worker/cache: redis ping failed")
}

// AcquireTaskLock 抢占任务运行锁。返回 true 表示抢占成功。
func (c *Cache) AcquireTaskLock(ctx context.Context, workerTaskID string) (bool, error) {
	ok, err := c.rds.SetnxExCtx(ctx, keyTaskLock(workerTaskID), "1", cacheTTLTaskLock)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// ReleaseTaskLock 释放任务运行锁。
func (c *Cache) ReleaseTaskLock(ctx context.Context, workerTaskID string) error {
	_, err := c.rds.DelCtx(ctx, keyTaskLock(workerTaskID))
	return err
}

// GetTaskResult 读取任务结果缓存。
// 返回 (payload, hit, err)：hit=false 表示 miss。
func (c *Cache) GetTaskResult(ctx context.Context, workerTaskID string) (string, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyTaskResult(workerTaskID))
	if err != nil {
		if err == redis.Nil {
			return "", false, nil
		}
		return "", false, err
	}
	return bs, true, nil
}

// SetTaskResult 写入任务结果缓存。
func (c *Cache) SetTaskResult(ctx context.Context, workerTaskID, payload string) error {
	return c.rds.SetexCtx(ctx, keyTaskResult(workerTaskID), payload, cacheTTLTaskResult)
}

// DelTaskResult 删除任务结果缓存。
func (c *Cache) DelTaskResult(ctx context.Context, workerTaskID string) error {
	_, err := c.rds.DelCtx(ctx, keyTaskResult(workerTaskID))
	return err
}

// Cacher 是 Repository 对缓存的**依赖面**（生产实现是上面的 *Cache）。
// 只声明 Repository 实际调用的 4 个方法：Cache 的 GetTaskResult/SetTaskResult 目前
// 没有任何 Repository/logic 调用方（见 README 已知缺口 #2），因此不进接口，
// 免得接口把「结果短缓存已接通」这个尚未成立的事实固化成契约。
// 跨包实现本接口不算破坏封装：「终态结果必须同时失效缓存」本身就是本服务的承诺。
type Cacher interface {
	Ping(ctx context.Context) error
	AcquireTaskLock(ctx context.Context, workerTaskID string) (bool, error)
	ReleaseTaskLock(ctx context.Context, workerTaskID string) error
	DelTaskResult(ctx context.Context, workerTaskID string) error
}

var _ Cacher = (*Cache)(nil)

// --- Repository ---

// Repository 是 moderation-worker 服务的数据访问入口。
type Repository struct {
	cache    Cacher
	conn     sqlx.SqlConn
	workerMd model.WorkerTaskModel
}

// New 构造 Repository（生产路径）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(NewCache(rds), conn, model.NewWorkerTaskModel(conn))
}

// NewWithDeps 用显式依赖构造 Repository；只服务于测试注入，生产代码一律走 New。
// conn 在本服务不被任何 Repository 方法使用（worker_task 只有单表写入，无跨表事务），
// 测试传 nil 即可；保留该字段是为了不让注入缝改动生产装配。
func NewWithDeps(cache Cacher, conn sqlx.SqlConn, workerMd model.WorkerTaskModel) *Repository {
	return &Repository{
		cache:    cache,
		conn:     conn,
		workerMd: workerMd,
	}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// CreateTask 创建识别任务记录（state=PENDING）。
func (r *Repository) CreateTask(ctx context.Context, t *model.WorkerTask) error {
	return r.workerMd.Upsert(ctx, t)
}

// FinishTask 写入任务执行结果并更新状态。
func (r *Repository) FinishTask(ctx context.Context, workerTaskID string, state int32, algorithmVersion string, elapsedMs int64, resultJSON, errorMessage string) error {
	if err := r.workerMd.UpdateResult(ctx, workerTaskID, state, algorithmVersion, elapsedMs, resultJSON, errorMessage); err != nil {
		return err
	}
	_ = r.cache.DelTaskResult(ctx, workerTaskID)
	return nil
}

// AcquireRunLock 抢占任务运行锁（防止并发重入）。
func (r *Repository) AcquireRunLock(ctx context.Context, workerTaskID string) (bool, error) {
	return r.cache.AcquireTaskLock(ctx, workerTaskID)
}

// ReleaseRunLock 释放任务运行锁。
func (r *Repository) ReleaseRunLock(ctx context.Context, workerTaskID string) error {
	return r.cache.ReleaseTaskLock(ctx, workerTaskID)
}

// GetTask 按 worker_task_id 查询任务。
func (r *Repository) GetTask(ctx context.Context, workerTaskID string) (*model.WorkerTask, error) {
	return r.workerMd.FindOne(ctx, workerTaskID)
}

// GetTaskByOrchestrator 按 orchestrator task_id 查询最新任务。
func (r *Repository) GetTaskByOrchestrator(ctx context.Context, taskID string) (*model.WorkerTask, error) {
	return r.workerMd.FindByTaskID(ctx, taskID)
}
