package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/search-query/internal/esclient"
	"go-video/services/search-query/model"
)

// Redis key 约定（前缀 sq: = search-query）。
// 这些 key 只属于本服务，其他服务不得读写（AGENTS.md §5）。
const (
	keyResultCache = "sq:res:v1:%s:%d"  // 查询指纹:offset → 结果短缓存
	keyHotKeywords = "sq:hot:v1:%s"     // scope → 热词快照
	keyBlockSet    = "sq:bw:v1"         // 生效屏蔽词集合（用于联想/热词过滤）
	keySuggestDict = "sq:sug:v1"        // 联想词典 ZSET（离线聚合维护，本服务只读）
	keyQueryCount  = "sq:cnt:kw:v1:%s"  // 关键词计数（keyword_hash）
	keyDayCount    = "sq:cnt:day:v1:%s" // 当日查询总量（yyyymmdd）

	// ttlQueryCount 计数器保留 7 天：热词/联想词典由离线聚合从 query_log 重建，
	// Redis 计数只用于实时大盘与临时词典，不作为事实源。
	ttlQueryCount = 7 * 24 * 3600
)

// cachedResult 结果短缓存载荷。
// 缓存的是引擎返回的投影，不含用户态数据，因此不同 mid 共享同一 key；
// 若后续引入个性化（黑名单过滤等），必须把个性化维度并入指纹，不能复用此 key。
type cachedResult struct {
	Hits     []esclient.Hit `json:"hits"`
	Total    int64          `json:"total"`
	Relation string         `json:"relation,omitempty"`
	TookMs   int64          `json:"took_ms"`
}

// CachedResult 是 cachedResult 的导出别名（注入缝，不是新类型）。
//
// 为什么需要：CacheStore.GetResult/SetResult 的签名带这个载荷类型，而 Go 不允许在其它包里
// 书写未导出类型名，因此 internal/logic 的单测无法实现 CacheStore 给 Repository 注入内存缓存
// （Repository 又只有 NewWithDeps 这一条注入路径）。别名保持类型标识不变，包内引用与生产
// 路径（New → NewWithDeps）零改动，只是让外部替身可书写。
type CachedResult = cachedResult

// Cache 封装 search-query 的 Redis 访问。
type Cache struct {
	rds *redis.Redis
}

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性（供健康检查使用）。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds == nil {
		return errors.New("search-query/cache: redis not initialized")
	}
	if c.rds.PingCtx(ctx) {
		return nil
	}
	return errors.New("search-query/cache: redis ping failed")
}

// ResultKey 返回结果缓存 key（导出给测试与运维排查脚本使用）。
func ResultKey(fingerprint string, offset int64) string {
	return fmt.Sprintf(keyResultCache, fingerprint, offset)
}

// GetResult 读取结果缓存。返回 (payload, 剩余 TTL 秒, 是否命中, error)。
// Redis 自身错误会返回 error（调用方决定降级策略）；未命中返回 hit=false 且 err=nil。
func (c *Cache) GetResult(ctx context.Context, fingerprint string, offset int64) (*cachedResult, int, bool, error) {
	key := ResultKey(fingerprint, offset)
	bs, err := c.rds.GetCtx(ctx, key)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("search-query/cache GetResult: %w", err)
	}
	if bs == "" {
		return nil, 0, false, nil
	}
	var v cachedResult
	if err := json.Unmarshal([]byte(bs), &v); err != nil {
		// 结构不兼容（滚动发布期间的旧版本）：删掉脏 key，按未命中处理。
		_, _ = c.rds.DelCtx(ctx, key)
		return nil, 0, false, nil
	}
	ttl, err := c.rds.TtlCtx(ctx, key)
	if err != nil {
		ttl = 0
	}
	return &v, ttl, true, nil
}

// SetResult 写入结果缓存；ttlSeconds<=0 时跳过（缓存被关闭）。
func (c *Cache) SetResult(ctx context.Context, fingerprint string, offset int64, v *cachedResult, ttlSeconds int) error {
	if ttlSeconds <= 0 {
		return nil
	}
	bs, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("search-query/cache SetResult marshal: %w", err)
	}
	if err := c.rds.SetexCtx(ctx, ResultKey(fingerprint, offset), string(bs), ttlSeconds); err != nil {
		return fmt.Errorf("search-query/cache SetResult: %w", err)
	}
	return nil
}

// GetHot 读取热词快照缓存。
func (c *Cache) GetHot(ctx context.Context, scope string) ([]*model.SearchHotKeyword, bool, error) {
	bs, err := c.rds.GetCtx(ctx, fmt.Sprintf(keyHotKeywords, scope))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("search-query/cache GetHot: %w", err)
	}
	if bs == "" {
		return nil, false, nil
	}
	var rows []*model.SearchHotKeyword
	if err := json.Unmarshal([]byte(bs), &rows); err != nil {
		_, _ = c.rds.DelCtx(ctx, fmt.Sprintf(keyHotKeywords, scope))
		return nil, false, nil
	}
	return rows, true, nil
}

// SetHot 写入热词快照缓存。
func (c *Cache) SetHot(ctx context.Context, scope string, rows []*model.SearchHotKeyword, ttlSeconds int) error {
	if ttlSeconds <= 0 {
		return nil
	}
	bs, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("search-query/cache SetHot marshal: %w", err)
	}
	if err := c.rds.SetexCtx(ctx, fmt.Sprintf(keyHotKeywords, scope), string(bs), ttlSeconds); err != nil {
		return fmt.Errorf("search-query/cache SetHot: %w", err)
	}
	return nil
}

// GetBlockSet 读取生效屏蔽词集合缓存。
func (c *Cache) GetBlockSet(ctx context.Context) (map[string]struct{}, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyBlockSet)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("search-query/cache GetBlockSet: %w", err)
	}
	if bs == "" {
		return nil, false, nil
	}
	var words []string
	if err := json.Unmarshal([]byte(bs), &words); err != nil {
		_, _ = c.rds.DelCtx(ctx, keyBlockSet)
		return nil, false, nil
	}
	set := make(map[string]struct{}, len(words))
	for _, w := range words {
		set[w] = struct{}{}
	}
	return set, true, nil
}

// SetBlockSet 写入生效屏蔽词集合缓存。
func (c *Cache) SetBlockSet(ctx context.Context, words []string, ttlSeconds int) error {
	if ttlSeconds <= 0 {
		return nil
	}
	bs, err := json.Marshal(words)
	if err != nil {
		return fmt.Errorf("search-query/cache SetBlockSet marshal: %w", err)
	}
	if err := c.rds.SetexCtx(ctx, keyBlockSet, string(bs), ttlSeconds); err != nil {
		return fmt.Errorf("search-query/cache SetBlockSet: %w", err)
	}
	return nil
}

// SuggestDict 取回联想词典候选（按权重倒序，最多 window 条）。
// 词典由离线聚合任务通过 ZADD 维护，本服务只读。
func (c *Cache) SuggestDict(ctx context.Context, window int) ([]redis.FloatPair, error) {
	if window <= 0 {
		return nil, nil
	}
	pairs, err := c.rds.ZrevrangeWithScoresByFloatCtx(ctx, keySuggestDict, 0, int64(window-1))
	if err != nil {
		return nil, fmt.Errorf("search-query/cache SuggestDict: %w", err)
	}
	return pairs, nil
}

// IncrQueryCounters 递增关键词与当日查询计数（供实时大盘/临时词典使用）。
// 计数是投影，允许从 search_query_log 重算，不作为事实源。
func (c *Cache) IncrQueryCounters(ctx context.Context, keywordHash, day string) error {
	if _, err := c.rds.IncrbyCtx(ctx, fmt.Sprintf(keyQueryCount, keywordHash), 1); err != nil {
		return fmt.Errorf("search-query/cache IncrKeyword: %w", err)
	}
	_ = c.rds.ExpireCtx(ctx, fmt.Sprintf(keyQueryCount, keywordHash), ttlQueryCount)
	if _, err := c.rds.IncrbyCtx(ctx, fmt.Sprintf(keyDayCount, day), 1); err != nil {
		return fmt.Errorf("search-query/cache IncrDay: %w", err)
	}
	_ = c.rds.ExpireCtx(ctx, fmt.Sprintf(keyDayCount, day), ttlQueryCount)
	return nil
}

// todayKey 返回当日分桶（yyyymmdd，UTC 由调用方时间决定）。
func todayKey(t time.Time) string {
	return t.UTC().Format("20060102")
}
