// Package repository 是 search-indexer 的数据访问层。
//
// 它同时封装两类存储：
//  1. MySQL（本服务自有的四张投影/流水表，见 deploy/migrations/search-indexer）；
//  2. OpenSearch（索引投影本身，通过 internal/esclient 的接口注入）。
//
// 边界（AGENTS.md §5）：本包只读写 search-indexer 自己的表，绝不连接
// video/catalog/user-profile 等上游库表或 Redis key；文档字段一律由事件
// payload 或上游 RPC 推送进来。
package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/model"
)

// 缓存 key 与 TTL。
// 说明：消费幂等与重建互斥都靠 MySQL 条件更新（uniq_event_id / state CAS）保证，
// 不需要分布式锁，因此这里只有「写入索引解析缓存」和「别名切换互斥」两个 key。
const (
	keyActiveIndex = "si:act:%s" // alias → 当前写入索引
	keyAliasLock   = "si:sw:%s"  // alias → 别名切换互斥锁

	ttlActiveIndex = 30 // 写入索引解析缓存 30 秒
	ttlAliasLock   = 60
)

// Options 索引命名与重建切片参数。
type Options struct {
	// IndexPrefix 别名前缀，同时是默认查询别名（search-query 读同一个别名）。
	IndexPrefix string
	// SchemaVersion 当前 mapping 版本，如 v1。
	SchemaVersion string
	// SliceSpan 重建时 content_id 区间宽度。
	SliceSpan int64
	// SliceSize 单次 _reindex 服务端批大小。
	SliceSize int
	// StopAfterEmptySlices 连续多少个空切片后判定重建完成。
	StopAfterEmptySlices int
	// RetryOnConflict 部分更新的服务端冲突重试次数。
	RetryOnConflict int
}

// normalize 补齐默认值并做基础校验。
func (o *Options) normalize() error {
	o.IndexPrefix = strings.ToLower(strings.TrimSpace(o.IndexPrefix))
	if o.IndexPrefix == "" {
		return model.ErrIndexPrefixEmpty
	}
	if o.SchemaVersion == "" {
		o.SchemaVersion = esclient.DefaultSchemaVersion
	}
	o.SchemaVersion = strings.ToLower(strings.TrimSpace(o.SchemaVersion))
	if o.SliceSpan <= 0 {
		o.SliceSpan = 100000
	}
	if o.SliceSize <= 0 {
		o.SliceSize = 2000
	}
	if o.StopAfterEmptySlices <= 0 {
		o.StopAfterEmptySlices = 3
	}
	if o.RetryOnConflict < 0 {
		o.RetryOnConflict = 0
	}
	return nil
}

// DefaultAlias 返回默认查询别名。
func (o Options) DefaultAlias() string { return o.IndexPrefix }

// Alias 把调用方传入的别名归一化：空串表示默认别名。
func (o Options) Alias(alias string) string {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" {
		return o.IndexPrefix
	}
	return alias
}

// NewIndexName 生成物理索引名：<alias>_<schema>_<unix秒>。
// OpenSearch 索引名必须小写，且带时间戳以保证重建可反复执行而不撞名。
func (o Options) NewIndexName(alias string) string {
	return fmt.Sprintf("%s_%s_%d", o.Alias(alias), o.SchemaVersion, time.Now().Unix())
}

// Cache 封装 Redis 缓存与互斥锁。
// rds 为 nil 时所有方法退化为「无缓存/不抢锁」，方便离线单测构造 Repository。
type Cache struct {
	rds *redis.Redis
}

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	if rds == nil {
		return nil
	}
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c == nil || c.rds == nil {
		return errors.New("search-indexer/cache: redis 未配置")
	}
	if c.rds.Ping() {
		return nil
	}
	return errors.New("search-indexer/cache: redis ping failed")
}

// GetActiveIndex 读取写入索引缓存。
func (c *Cache) GetActiveIndex(ctx context.Context, alias string) (string, bool) {
	if c == nil {
		return "", false
	}
	v, err := c.rds.GetCtx(ctx, fmt.Sprintf(keyActiveIndex, alias))
	if err != nil || v == "" {
		return "", false
	}
	return v, true
}

// SetActiveIndex 写入写入索引缓存。
func (c *Cache) SetActiveIndex(ctx context.Context, alias, index string) {
	if c == nil {
		return
	}
	_ = c.rds.SetexCtx(ctx, fmt.Sprintf(keyActiveIndex, alias), index, ttlActiveIndex)
}

// DelActiveIndex 失效写入索引缓存（别名切换后必须立刻失效，否则继续写老索引）。
func (c *Cache) DelActiveIndex(ctx context.Context, alias string) {
	if c == nil {
		return
	}
	_, _ = c.rds.DelCtx(ctx, fmt.Sprintf(keyActiveIndex, alias))
}

// AcquireLock 抢占互斥锁；返回 false 表示已被占用。无 Redis 时视为抢到（单实例）。
func (c *Cache) AcquireLock(ctx context.Context, key string, ttl int) (bool, error) {
	if c == nil {
		return true, nil
	}
	return c.rds.SetnxExCtx(ctx, key, "1", ttl)
}

// ReleaseLock 释放互斥锁。
func (c *Cache) ReleaseLock(ctx context.Context, key string) {
	if c == nil {
		return
	}
	_, _ = c.rds.DelCtx(ctx, key)
}

// Configured 报告背后是否真的有 Redis 连接（无 Redis 时 Repository 的
// PingRedis 退化为「不检查」，见其注释）。
func (c *Cache) Configured() bool { return c != nil && c.rds != nil }

// Cacher 是 Repository 用到的缓存能力面，由本包的 *Cache 实现。
//
// 抽接口只是**注入缝**（AGENTS.md §4：自定义代码只能落在明确标记的手写扩展点）：
// 生产构造路径 New / NewWithModels 仍然得到同一个 *Cache，行为逐字节不变；
// 目的是让离线单测能覆盖三条「只有真配了 Redis 才走得到」的生产分支——
// 写入索引缓存命中/回填、别名切换互斥抢锁失败、切换后立刻失效缓存。
// 实现方必须保持与 *Cache 一致的退化语义：无缓存时读 miss、抢锁放行。
type Cacher interface {
	GetActiveIndex(ctx context.Context, alias string) (string, bool)
	SetActiveIndex(ctx context.Context, alias, index string)
	DelActiveIndex(ctx context.Context, alias string)
	AcquireLock(ctx context.Context, key string, ttl int) (bool, error)
	ReleaseLock(ctx context.Context, key string)
	Configured() bool
	Ping(ctx context.Context) error
}

// newCacheStore 把 *Cache 转成接口值。
//
// rds 未配置时 NewCache 返回 nil *Cache，这里**故意**包装成类型化 nil 指针而不是 nil 接口：
// *Cache 的每个方法都做了 `c == nil` 判空，因此 Repository 里的 r.cache.X() 调用
// 不会 panic（改造前 cache 字段就是 *Cache，同样依赖这点）。
// 而「是否真的配了 Redis」由 Configured() 表达，PingRedis 不再依赖 r.cache == nil，
// 于是类型化 nil 与非 nil 的判定结果与改造前完全一致。
func newCacheStore(c *Cache) Cacher { return c }

// Repository 是 search-indexer 的数据访问入口。
type Repository struct {
	cache     Cacher
	es        esclient.Client
	taskMd    model.SearchIndexTaskModel
	versionMd model.SearchIndexVersionModel
	offsetMd  model.SearchConsumerOffsetModel
	dlqMd     model.SearchDeadLetterModel
	opts      Options
}

// New 构造生产 Repository（模型由 sqlx 连接创建）。
func New(rds *redis.Redis, conn sqlx.SqlConn, es esclient.Client, opts Options) (*Repository, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	if es == nil {
		return nil, esclient.ErrEmptyEndpoints
	}
	return &Repository{
		cache:     newCacheStore(NewCache(rds)),
		es:        es,
		taskMd:    model.NewSearchIndexTaskModel(conn),
		versionMd: model.NewSearchIndexVersionModel(conn),
		offsetMd:  model.NewSearchConsumerOffsetModel(conn),
		dlqMd:     model.NewSearchDeadLetterModel(conn),
		opts:      opts,
	}, nil
}

// NewWithModels 用显式 model 实现构造 Repository，供单测注入 fake
// （禁止用「永不失败」的假实现糊测试：fake 必须按用例返回真实错误）。
// 缓存仍由 rds 决定（传 nil 即「无 Redis」的退化语义）；要替换缓存本身用 NewWithDeps。
func NewWithModels(rds *redis.Redis, es esclient.Client, opts Options,
	taskMd model.SearchIndexTaskModel, versionMd model.SearchIndexVersionModel,
	offsetMd model.SearchConsumerOffsetModel, dlqMd model.SearchDeadLetterModel) (*Repository, error) {
	return NewWithDeps(newCacheStore(NewCache(rds)), es, opts, taskMd, versionMd, offsetMd, dlqMd)
}

// NewWithDeps 在 NewWithModels 之上再开放缓存注入缝（仅单测使用，见 Cacher 注释）。
// cache 传 nil 时退化为「无 Redis」：读永远 miss、抢锁总是放行、PingRedis 返回 nil。
func NewWithDeps(cache Cacher, es esclient.Client, opts Options,
	taskMd model.SearchIndexTaskModel, versionMd model.SearchIndexVersionModel,
	offsetMd model.SearchConsumerOffsetModel, dlqMd model.SearchDeadLetterModel) (*Repository, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	if es == nil {
		return nil, esclient.ErrEmptyEndpoints
	}
	if cache == nil {
		cache = newCacheStore(nil)
	}
	return &Repository{
		cache:     cache,
		es:        es,
		taskMd:    taskMd,
		versionMd: versionMd,
		offsetMd:  offsetMd,
		dlqMd:     dlqMd,
		opts:      opts,
	}, nil
}

// Opts 返回归一化后的配置。
func (r *Repository) Opts() Options { return r.opts }

// PingRedis 检查 Redis 连通性（未配置缓存时返回 nil）。
func (r *Repository) PingRedis(ctx context.Context) error {
	if !r.cache.Configured() {
		return nil
	}
	return r.cache.Ping(ctx)
}
