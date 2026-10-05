// Package repository 是 transcode 服务的数据访问层。
// 组合 transcode_task 与 transcode_template 两个 model，
// 为 logic 层提供统一数据访问入口。
// 缓存策略：任务详情短 TTL（60s），变更时刷新；模板详情长 TTL（600s），创建时刷新。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/transcode/model"
)

// Cache 封装 transcode 的 Redis 缓存操作。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixTask     = "tc:task:%d" // task_id → 任务详情 JSON
	prefixTemplate = "tc:tpl:%d"  // template_id → 模板详情 JSON

	cacheTTLTask     = 60  // 任务详情短缓存
	cacheTTLTemplate = 600 // 模板详情长缓存
)

func keyTask(taskID int64) string         { return fmt.Sprintf(prefixTask, taskID) }
func keyTemplate(templateID int64) string { return fmt.Sprintf(prefixTemplate, templateID) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("transcode/cache: redis ping failed")
}

// --- 任务缓存 ---

// GetTask 读取任务详情缓存。返回 (payload, hit, err)：hit=false 表示 miss。
func (c *Cache) GetTask(ctx context.Context, taskID int64) (string, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyTask(taskID))
	if err != nil {
		if err == redis.Nil {
			return "", false, nil
		}
		return "", false, err
	}
	return bs, true, nil
}

// SetTask 写入任务详情缓存。
func (c *Cache) SetTask(ctx context.Context, taskID int64, payload string) error {
	return c.rds.SetexCtx(ctx, keyTask(taskID), payload, cacheTTLTask)
}

// DelTask 删除任务详情缓存。
func (c *Cache) DelTask(ctx context.Context, taskID int64) error {
	_, err := c.rds.DelCtx(ctx, keyTask(taskID))
	return err
}

// --- 模板缓存 ---

// GetTemplate 读取模板详情缓存。返回 (payload, hit, err)。
func (c *Cache) GetTemplate(ctx context.Context, templateID int64) (string, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyTemplate(templateID))
	if err != nil {
		if err == redis.Nil {
			return "", false, nil
		}
		return "", false, err
	}
	return bs, true, nil
}

// SetTemplate 写入模板详情缓存。
func (c *Cache) SetTemplate(ctx context.Context, templateID int64, payload string) error {
	return c.rds.SetexCtx(ctx, keyTemplate(templateID), payload, cacheTTLTemplate)
}

// DelTemplate 删除模板详情缓存。
func (c *Cache) DelTemplate(ctx context.Context, templateID int64) error {
	_, err := c.rds.DelCtx(ctx, keyTemplate(templateID))
	return err
}

// --- Repository ---

// Cacher 是 Repository 对缓存层的最小依赖面（生产由 *Cache 实现）。
//
// 为什么要有这个接口：Repository.cache 原先是具体类型 *Cache，而 ServiceContext.Repository
// 又是具体类型 *repository.Repository，logic 单测无处塞替身，只能连真 Redis。抽出接口后，
// 测试用 NewWithDeps(内存缓存, fakeConn, 内存 taskMd/tplMd) 组装**真实的 Repository**，
// 缓存读穿/回填、UpdateProgress 后的失效顺序、终态判定读的是缓存还是库这些结论，
// 仍然整条在被测路径上（口径同 comment/rights/playback 的 Cacher）。
// 只声明 Repository 实际调用的方法：Cache.DelTemplate 全仓零调用（模板不可改，只靠 TTL 过期），
// 故不进入接口，也不被任何用例覆盖。
// 语义约定与 *Cache 一致：miss 返回 hit=false 且 err=nil，不视为错误。
type Cacher interface {
	Ping(ctx context.Context) error
	GetTask(ctx context.Context, taskID int64) (string, bool, error)
	SetTask(ctx context.Context, taskID int64, payload string) error
	DelTask(ctx context.Context, taskID int64) error
	GetTemplate(ctx context.Context, templateID int64) (string, bool, error)
	SetTemplate(ctx context.Context, templateID int64, payload string) error
}

var _ Cacher = (*Cache)(nil)

// Repository 是 transcode 服务的数据访问入口。
type Repository struct {
	cache  Cacher
	conn   sqlx.SqlConn
	taskMd model.TranscodeTaskModel
	tplMd  model.TranscodeTemplateModel
}

// New 构造 Repository（生产路径唯一入口）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(NewCache(rds), conn,
		model.NewTranscodeTaskModel(conn), model.NewTranscodeTemplateModel(conn))
}

// NewWithDeps 用显式依赖组装 Repository。生产代码只应走 New；本函数为 logic 单测提供注入缝
// （见上方 Cacher 注释）。conn 只在 New 里用于装配 model，Repository 本身不发起事务，
// 因此注入缝下它仍然是原样保留的字段（与加缝前的生产语义逐字一致）。
func NewWithDeps(cache Cacher, conn sqlx.SqlConn,
	taskMd model.TranscodeTaskModel, tplMd model.TranscodeTemplateModel) *Repository {
	return &Repository{cache: cache, conn: conn, taskMd: taskMd, tplMd: tplMd}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- 任务 ---

// SubmitTask 创建转码任务，返回新 task_id。
// 本期占位：仅写库，不调用 FFmpeg；实际转码由 Worker 消费 MQ 实现。
func (r *Repository) SubmitTask(ctx context.Context, t *model.TranscodeTask) (int64, error) {
	taskID, err := r.taskMd.Insert(ctx, t)
	if err != nil {
		return 0, err
	}
	return taskID, nil
}

// GetTask 查询任务详情（cache-aside）。
func (r *Repository) GetTask(ctx context.Context, taskID int64) (*model.TranscodeTask, error) {
	if payload, hit, err := r.cache.GetTask(ctx, taskID); err != nil {
		return nil, err
	} else if hit {
		var t model.TranscodeTask
		if err := json.Unmarshal([]byte(payload), &t); err != nil {
			return nil, fmt.Errorf("transcode/GetTask unmarshal cache: %w", err)
		}
		return &t, nil
	}
	t, err := r.taskMd.FindOne(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, model.ErrTaskNotFound
	}
	if bs, err := json.Marshal(t); err == nil {
		_ = r.cache.SetTask(ctx, taskID, string(bs))
	}
	return t, nil
}

// ListTasks 分页查询任务。
func (r *Repository) ListTasks(ctx context.Context, assetID int64, state int32, pn, ps int32) ([]*model.TranscodeTask, int32, error) {
	return r.taskMd.List(ctx, assetID, state, pn, ps)
}

// UpdateProgress 更新任务进度与状态，刷新缓存，返回最新任务详情。
// 调用前 logic 层应已校验状态机合法性（model.IsValidTransition）。
func (r *Repository) UpdateProgress(ctx context.Context, taskID int64, progress, state, errno int32, errMsg string, mtime int64) (*model.TranscodeTask, error) {
	if err := r.taskMd.UpdateProgress(ctx, taskID, progress, state, errno, errMsg, mtime); err != nil {
		return nil, err
	}
	_ = r.cache.DelTask(ctx, taskID)
	return r.taskMd.FindOne(ctx, taskID)
}

// --- 模板 ---

// ListTemplates 分页查询模板列表。
func (r *Repository) ListTemplates(ctx context.Context, pn, ps int32) ([]*model.TranscodeTemplate, int32, error) {
	return r.tplMd.List(ctx, pn, ps)
}

// GetTemplate 查询模板详情（cache-aside）。
func (r *Repository) GetTemplate(ctx context.Context, templateID int64) (*model.TranscodeTemplate, error) {
	if payload, hit, err := r.cache.GetTemplate(ctx, templateID); err != nil {
		return nil, err
	} else if hit {
		var t model.TranscodeTemplate
		if err := json.Unmarshal([]byte(payload), &t); err != nil {
			return nil, fmt.Errorf("transcode/GetTemplate unmarshal cache: %w", err)
		}
		return &t, nil
	}
	t, err := r.tplMd.FindOne(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, model.ErrTemplateNotFound
	}
	if bs, err := json.Marshal(t); err == nil {
		_ = r.cache.SetTemplate(ctx, templateID, string(bs))
	}
	return t, nil
}

// CreateTemplate 创建模板，返回新 template_id。
func (r *Repository) CreateTemplate(ctx context.Context, t *model.TranscodeTemplate) (int64, error) {
	templateID, err := r.tplMd.Insert(ctx, t)
	if err != nil {
		return 0, err
	}
	return templateID, nil
}
