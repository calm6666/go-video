package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/inbox/model"
)

// 未读计数缓存 key：一个 mid 一个 hash，字段为分类。
// 只作为加速层——任何写操作后都直接失效，读回源 DB 快照再回填。
const (
	prefixUnreadHash = "inbox:unread:%d" // mid -> hash(category -> count)
	invalidChunk     = 100               // 单次 DEL 的 key 数上限
)

func unreadKey(mid int64) string {
	return fmt.Sprintf(prefixUnreadHash, mid)
}

// UnreadCache 是未读计数加速层的抽象，便于单测替换（不连真实 Redis）。
type UnreadCache interface {
	// Get 读取快照；miss 或出错都返回 ok=false（Redis 不可用不得影响读接口）。
	Get(ctx context.Context, mid int64) (map[int32]int64, bool)
	// Set 回填快照；TTL 到期后自动回源 DB。
	Set(ctx context.Context, mid int64, snapshot map[int32]int64)
	// Invalidate 在写事务提交后清除受影响用户的快照。
	Invalidate(ctx context.Context, mids ...int64)
	// Ping 检查 Redis 连通性，供健康检查使用。
	Ping(ctx context.Context) error
}

// Cache 是基于 go-zero Redis 的 UnreadCache 实现。
type Cache struct {
	rds *redis.Redis
	ttl int
}

// NewCache 构造 Cache；ttlSeconds<=0 时使用 1800 秒。
func NewCache(rds *redis.Redis, ttlSeconds int) *Cache {
	if ttlSeconds <= 0 {
		ttlSeconds = 1800
	}
	return &Cache{rds: rds, ttl: ttlSeconds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds == nil {
		return errors.New("inbox/cache: redis not configured")
	}
	if c.rds.PingCtx(ctx) {
		return nil
	}
	return errors.New("inbox/cache: redis ping failed")
}

// Get 用 HGETALL 一次取回四个分类的未读数。
func (c *Cache) Get(ctx context.Context, mid int64) (map[int32]int64, bool) {
	if c.rds == nil {
		return nil, false
	}
	fields, err := c.rds.HgetallCtx(ctx, unreadKey(mid))
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			logx.WithContext(ctx).Errorf("inbox/cache Get mid=%d err=%v", mid, err)
		}
		return nil, false
	}
	if len(fields) == 0 {
		return nil, false
	}
	out := make(map[int32]int64, len(fields))
	for field, value := range fields {
		category, err := strconv.Atoi(field)
		if err != nil {
			logx.WithContext(ctx).Errorf("inbox/cache Get mid=%d bad field=%q", mid, field)
			return nil, false
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			logx.WithContext(ctx).Errorf("inbox/cache Get mid=%d bad value=%q", mid, value)
			return nil, false
		}
		out[int32(category)] = n
	}
	// 缺任一分类都视为快照不完整，整体回源，避免 total 少算。
	for _, category := range model.AllCategories() {
		if _, ok := out[category]; !ok {
			return nil, false
		}
	}
	return out, true
}

// Set 用 HMSET + EXPIRE 回填快照；失败只记日志，读路径自然回源。
func (c *Cache) Set(ctx context.Context, mid int64, snapshot map[int32]int64) {
	if c.rds == nil || len(snapshot) == 0 {
		return
	}
	fields := make(map[string]string, len(snapshot))
	for category, n := range snapshot {
		if !model.ValidCategory(category) {
			continue
		}
		fields[strconv.Itoa(int(category))] = strconv.FormatInt(n, 10)
	}
	key := unreadKey(mid)
	if err := c.rds.HmsetCtx(ctx, key, fields); err != nil {
		logx.WithContext(ctx).Errorf("inbox/cache Set mid=%d hmset err=%v", mid, err)
		return
	}
	if err := c.rds.ExpireCtx(ctx, key, c.ttl); err != nil {
		logx.WithContext(ctx).Errorf("inbox/cache Set mid=%d expire err=%v", mid, err)
	}
}

// Invalidate 批量删除快照，按 invalidChunk 分片，避免一次性 DEL 过多 key。
func (c *Cache) Invalidate(ctx context.Context, mids ...int64) {
	if c.rds == nil || len(mids) == 0 {
		return
	}
	for start := 0; start < len(mids); start += invalidChunk {
		end := start + invalidChunk
		if end > len(mids) {
			end = len(mids)
		}
		keys := make([]string, 0, end-start)
		for _, mid := range mids[start:end] {
			keys = append(keys, unreadKey(mid))
		}
		if _, err := c.rds.DelCtx(ctx, keys...); err != nil {
			logx.WithContext(ctx).Errorf("inbox/cache Invalidate mids=%v err=%v", keys, err)
		}
	}
}
