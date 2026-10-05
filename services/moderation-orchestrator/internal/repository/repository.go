// Package repository 是 moderation-orchestrator 服务的数据访问层。
// 组合 moderation_task/moderation_rule/moderation_result/moderation_appeal 4 个 model，
// 为 logic 层提供统一数据访问入口，并通过 Redis 缓存降低热点查询压力。
//
// 状态机约束（AGENTS.md §8）：
//   - SubmitForReview 只接受未审核任务（无现有任务或现有任务已终态）；
//   - worker 回调 SubmitWorkerResult 只能推进 PENDING/PROCESSING → DONE，
//     不能直接置 APPROVED/PUBLISHED；
//   - 申诉处理只能推进 APPEALED → APPEAL_DONE。
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/moderation-orchestrator/model"
)

// Cache 封装 moderation-orchestrator 的 Redis 缓存操作。
// 任务详情、审核结论、申诉记录短缓存；列表查询不缓存（运营后台流量小）。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixTask   = "mod:task:%d"   // task_id → 任务详情 JSON
	prefixResult = "mod:result:%d" // task_id → 审核结论 JSON
	prefixAppeal = "mod:appeal:%d" // appeal_id → 申诉详情 JSON

	cacheTTLTask   = 60
	cacheTTLResult = 60
	cacheTTLAppeal = 60
)

func keyTask(taskID int64) string     { return fmt.Sprintf(prefixTask, taskID) }
func keyResult(taskID int64) string   { return fmt.Sprintf(prefixResult, taskID) }
func keyAppeal(appealID int64) string { return fmt.Sprintf(prefixAppeal, appealID) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("moderation/cache: redis ping failed")
}

// --- 任务详情缓存 ---

// GetTask 读取任务详情缓存。
// 返回 (payload, hit, err)：hit=false 表示缓存 miss。
func (c *Cache) GetTask(ctx context.Context, taskID int64) ([]byte, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyTask(taskID))
	if err != nil {
		if err == redis.Nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	return []byte(bs), true, nil
}

// SetTask 写入任务详情缓存。
func (c *Cache) SetTask(ctx context.Context, taskID int64, payload []byte) error {
	return c.rds.SetexCtx(ctx, keyTask(taskID), string(payload), cacheTTLTask)
}

// DelTask 删除任务详情缓存（任务状态变更时失效）。
func (c *Cache) DelTask(ctx context.Context, taskID int64) error {
	_, err := c.rds.DelCtx(ctx, keyTask(taskID))
	return err
}

// --- 审核结论缓存 ---

// GetResult 读取审核结论缓存。
func (c *Cache) GetResult(ctx context.Context, taskID int64) ([]byte, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyResult(taskID))
	if err != nil {
		if err == redis.Nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	return []byte(bs), true, nil
}

// SetResult 写入审核结论缓存。
func (c *Cache) SetResult(ctx context.Context, taskID int64, payload []byte) error {
	return c.rds.SetexCtx(ctx, keyResult(taskID), string(payload), cacheTTLResult)
}

// DelResult 删除审核结论缓存（结论回写时失效，避免脏读）。
func (c *Cache) DelResult(ctx context.Context, taskID int64) error {
	_, err := c.rds.DelCtx(ctx, keyResult(taskID))
	return err
}

// --- 申诉缓存 ---

// GetAppeal 读取申诉详情缓存。
func (c *Cache) GetAppeal(ctx context.Context, appealID int64) ([]byte, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyAppeal(appealID))
	if err != nil {
		if err == redis.Nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	return []byte(bs), true, nil
}

// SetAppeal 写入申诉详情缓存。
func (c *Cache) SetAppeal(ctx context.Context, appealID int64, payload []byte) error {
	return c.rds.SetexCtx(ctx, keyAppeal(appealID), string(payload), cacheTTLAppeal)
}

// DelAppeal 删除申诉详情缓存（处理时失效）。
func (c *Cache) DelAppeal(ctx context.Context, appealID int64) error {
	_, err := c.rds.DelCtx(ctx, keyAppeal(appealID))
	return err
}

// --- Repository ---

// 编译期确认生产缓存实现满足注入面。
var _ Cacher = (*Cache)(nil)

// Cacher 是 Repository 真正调用到的缓存依赖面（AGENTS.md §4：接口只收实际使用的方法）。
// 生产由 *Cache 实现；单测用内存替身经 NewWithDeps 注入，从而让「读穿/回填/失效、
// 状态机 CAS、结论 Upsert」整条链路仍在被测路径上，而不是把 Repository 整个 mock 掉。
// 装配语义未变：生产路径仍只走 New(rds, conn)。
type Cacher interface {
	Ping(ctx context.Context) error

	GetTask(ctx context.Context, taskID int64) ([]byte, bool, error)
	SetTask(ctx context.Context, taskID int64, payload []byte) error
	DelTask(ctx context.Context, taskID int64) error

	GetResult(ctx context.Context, taskID int64) ([]byte, bool, error)
	SetResult(ctx context.Context, taskID int64, payload []byte) error
	DelResult(ctx context.Context, taskID int64) error

	GetAppeal(ctx context.Context, appealID int64) ([]byte, bool, error)
	SetAppeal(ctx context.Context, appealID int64, payload []byte) error
	DelAppeal(ctx context.Context, appealID int64) error
}

// Repository 是 moderation-orchestrator 服务的数据访问入口。
type Repository struct {
	cache    Cacher
	taskMd   model.ModerationTaskModel
	ruleMd   model.ModerationRuleModel
	resultMd model.ModerationResultModel
	appealMd model.ModerationAppealModel
}

// New 构造 Repository（生产入口）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(
		NewCache(rds),
		model.NewModerationTaskModel(conn),
		model.NewModerationRuleModel(conn),
		model.NewModerationResultModel(conn),
		model.NewModerationAppealModel(conn),
	)
}

// NewWithDeps 用显式依赖装配 Repository，仅供单测注入内存替身；
// New 是它的生产包装。原 `conn sqlx.SqlConn` 字段是死重（本服务无跨表事务，
// Repository 从不读它），随门缝一并去掉。
func NewWithDeps(
	cache Cacher,
	taskMd model.ModerationTaskModel,
	ruleMd model.ModerationRuleModel,
	resultMd model.ModerationResultModel,
	appealMd model.ModerationAppealModel,
) *Repository {
	return &Repository{
		cache:    cache,
		taskMd:   taskMd,
		ruleMd:   ruleMd,
		resultMd: resultMd,
		appealMd: appealMd,
	}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- 任务 ---

// SubmitForReview 创建审核任务。
// 状态机约束：同一 (business, submission_id) 已存在 PENDING/PROCESSING 任务时拒绝重复提交。
// 当前实现占位：不实际调用 moderation-worker，仅持久化 task 入 PENDING 状态，
// 后续由 MQ 消费者或定时任务派发给 worker。
func (r *Repository) SubmitForReview(ctx context.Context, t *model.ModerationTask) (int64, error) {
	exist, err := r.taskMd.FindBySubmission(ctx, t.Business, t.SubmissionID)
	if err != nil {
		return 0, err
	}
	if exist != nil && (exist.State == model.TaskStatePending || exist.State == model.TaskStateProcessing) {
		return 0, model.ErrDuplicateTask
	}
	t.State = model.TaskStatePending
	id, err := r.taskMd.Insert(ctx, t)
	if err != nil {
		return 0, err
	}
	t.ID = id
	// 写入任务缓存，便于后续 GetTask 直接命中。
	_ = r.cache.SetTask(ctx, id, jsonMustMarshal(t))
	return id, nil
}

// GetTask 查询任务详情；优先读缓存。
func (r *Repository) GetTask(ctx context.Context, taskID int64) (*model.ModerationTask, error) {
	if payload, hit, err := r.cache.GetTask(ctx, taskID); err == nil && hit {
		var t model.ModerationTask
		if err := jsonUnmarshal(payload, &t); err == nil && t.ID > 0 {
			return &t, nil
		}
	}
	t, err := r.taskMd.FindOne(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, model.ErrTaskNotFound
	}
	_ = r.cache.SetTask(ctx, taskID, jsonMustMarshal(t))
	return t, nil
}

// ListTasks 分页查询任务列表。
func (r *Repository) ListTasks(ctx context.Context, mid int64, contentType, state, pn, ps int32) ([]*model.ModerationTask, int32, error) {
	return r.taskMd.List(ctx, mid, contentType, state, pn, ps)
}

// UpdateTaskState 更新任务状态（带状态机校验）。
// 旧状态必须命中 fromStates，否则返回 ErrInvalidStateTransition。
func (r *Repository) UpdateTaskState(ctx context.Context, taskID int64, toState int32, fromStates ...int32) error {
	if err := r.taskMd.UpdateState(ctx, taskID, toState, fromStates...); err != nil {
		return err
	}
	// 任务状态变更后失效缓存。
	_ = r.cache.DelTask(ctx, taskID)
	return nil
}

// --- 结论 ---

// GetResult 查询审核结论；优先读缓存。
func (r *Repository) GetResult(ctx context.Context, taskID int64) (*model.ModerationResult, error) {
	if payload, hit, err := r.cache.GetResult(ctx, taskID); err == nil && hit {
		var res model.ModerationResult
		if err := jsonUnmarshal(payload, &res); err == nil && res.TaskID > 0 {
			return &res, nil
		}
	}
	res, err := r.resultMd.FindOne(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, model.ErrResultNotFound
	}
	_ = r.cache.SetResult(ctx, taskID, jsonMustMarshal(res))
	return res, nil
}

// SubmitWorkerResult 回写 worker 识别结果，并推进任务状态 PENDING/PROCESSING → DONE。
// 依据 AGENTS.md §8：worker 回调不能直接置 APPROVED/PUBLISHED；
// 由内容所有者消费 moderation.result.v1 推进合法状态。
func (r *Repository) SubmitWorkerResult(ctx context.Context, res *model.ModerationResult) error {
	if err := r.resultMd.Upsert(ctx, res); err != nil {
		return err
	}
	if err := r.taskMd.UpdateState(ctx, res.TaskID, model.TaskStateDone,
		model.TaskStatePending, model.TaskStateProcessing); err != nil {
		// 任务已 DONE 或不存在；结论已写入，幂等返回。
		if errors.Is(err, model.ErrInvalidStateTransition) {
			return nil
		}
		return err
	}
	// 失效任务和结论缓存，让后续 GetTask/GetResult 拿到最新值。
	_ = r.cache.DelTask(ctx, res.TaskID)
	_ = r.cache.DelResult(ctx, res.TaskID)
	return nil
}

// --- 申诉 ---

// SubmitAppeal 创建申诉，并推进任务状态 DONE → APPEALED。
func (r *Repository) SubmitAppeal(ctx context.Context, a *model.ModerationAppeal) (int64, error) {
	appealID, err := r.appealMd.Insert(ctx, a)
	if err != nil {
		return 0, err
	}
	// DONE → APPEALED；若任务已处于 APPEALED（重复申诉）幂等返回。
	if err := r.taskMd.UpdateState(ctx, a.TaskID, model.TaskStateAppealed,
		model.TaskStateDone); err != nil {
		if !errors.Is(err, model.ErrInvalidStateTransition) {
			return 0, err
		}
	}
	_ = r.cache.DelTask(ctx, a.TaskID)
	_ = r.cache.SetAppeal(ctx, appealID, jsonMustMarshal(a))
	return appealID, nil
}

// GetAppeal 查询申诉详情；优先读缓存。
func (r *Repository) GetAppeal(ctx context.Context, appealID int64) (*model.ModerationAppeal, error) {
	if payload, hit, err := r.cache.GetAppeal(ctx, appealID); err == nil && hit {
		var a model.ModerationAppeal
		if err := jsonUnmarshal(payload, &a); err == nil && a.ID > 0 {
			return &a, nil
		}
	}
	a, err := r.appealMd.FindOne(ctx, appealID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, model.ErrAppealNotFound
	}
	_ = r.cache.SetAppeal(ctx, appealID, jsonMustMarshal(a))
	return a, nil
}

// ProcessAppeal 处理申诉，并推进任务状态 APPEALED → APPEAL_DONE。
func (r *Repository) ProcessAppeal(ctx context.Context, appealID, handler int64, finalVerdict int32, finalReason string) error {
	if err := r.appealMd.Update(ctx, appealID, handler, finalVerdict, finalReason); err != nil {
		return err
	}
	a, err := r.appealMd.FindOne(ctx, appealID)
	if err != nil {
		return err
	}
	if a != nil {
		// APPEALED → APPEAL_DONE；若任务已 APPEAL_DONE（重复处理）幂等返回。
		if err := r.taskMd.UpdateState(ctx, a.TaskID, model.TaskStateAppealDone,
			model.TaskStateAppealed); err != nil {
			if !errors.Is(err, model.ErrInvalidStateTransition) {
				return err
			}
		}
		_ = r.cache.DelTask(ctx, a.TaskID)
	}
	_ = r.cache.DelAppeal(ctx, appealID)
	return nil
}
