package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
)

// nowUnix 返回当前 Unix 秒；本服务所有时间列均为秒。
func nowUnix() int64 { return time.Now().Unix() }

// seconds 把秒数转成 time.Duration（pipeline Expire 需要）。
func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// errRedisNil 归一化 Redis 未命中判断。
func isRedisNil(err error) bool { return errors.Is(err, redis.Nil) }

// cacheBackend 是 Cache 对 Redis 的最小依赖面：只暴露 key→string 三条原语。
//
// 抽这个接口的唯一目的是给 internal/logic 的单测留注入缝（见 repository.go 的 NewWithDeps）：
// 用例用内存替身组装**真实 Cache + 真实 Repository**，让
// 「redis.Nil 归一化成 miss / JSON 损坏按 miss 处理 / 日志 key 截断脱敏」这些生产口径
// 仍留在被测路径上，而不是把 Cache 一起 mock 掉。生产路径仍然只走 New。
//
// 逐条转调 *redis.Redis 的同名方法，不改写返回值、不吞错误、不重试。
type cacheBackend interface {
	// Get 读取值；key 不存在时返回 redis.Nil 错误（与 *redis.Redis.GetCtx 同口径）。
	Get(ctx context.Context, key string) (string, error)
	// Setex 写入值并设置过期秒数。
	Setex(ctx context.Context, key, value string, ttlSeconds int) error
	// Del 删除 key，返回删除个数。
	Del(ctx context.Context, keys ...string) (int, error)
}

// redisCacheBackend 是 cacheBackend 的真实实现。
type redisCacheBackend struct{ rds *redis.Redis }

// newRedisCacheBackend 包装真实客户端；rds 为 nil 时返回 nil 接口，
// 使 Cache 原有的「c.rds == nil 即 no-op」判断结论不变。
func newRedisCacheBackend(rds *redis.Redis) cacheBackend {
	if rds == nil {
		return nil
	}
	return redisCacheBackend{rds: rds}
}

func (b redisCacheBackend) Get(ctx context.Context, key string) (string, error) {
	return b.rds.GetCtx(ctx, key)
}

func (b redisCacheBackend) Setex(ctx context.Context, key, value string, ttlSeconds int) error {
	return b.rds.SetexCtx(ctx, key, value, ttlSeconds)
}

func (b redisCacheBackend) Del(ctx context.Context, keys ...string) (int, error) {
	return b.rds.DelCtx(ctx, keys...)
}

// Cache 封装 risk-control 的 Redis JSON 缓存：
// 裁决回放（保证同 request_id 幂等）与启用规则集合（降低 CheckAction 的 DB 读放大）。
type Cache struct {
	rds cacheBackend
}

// newCache 构造 Cache。
func newCache(rds *redis.Redis) *Cache { return &Cache{rds: newRedisCacheBackend(rds)} }

// GetJSON 读取并反序列化缓存值；未命中返回 (false, nil)。
// 反序列化失败同样按未命中处理：缓存损坏不应让裁决失败，回源 DB 即可。
func (c *Cache) GetJSON(ctx context.Context, key string, dst any) (bool, error) {
	if key == "" || c == nil || c.rds == nil {
		return false, nil
	}
	bs, err := c.rds.Get(ctx, key)
	if err != nil {
		if isRedisNil(err) {
			return false, nil
		}
		return false, fmt.Errorf("risk-control/cache get %s: %w", sanitizeKey(key), err)
	}
	if bs == "" {
		return false, nil
	}
	if err := json.Unmarshal([]byte(bs), dst); err != nil {
		return false, nil
	}
	return true, nil
}

// SetJSON 序列化并写入缓存值。
func (c *Cache) SetJSON(ctx context.Context, key string, v any, ttlSeconds int) error {
	if key == "" || c == nil || c.rds == nil {
		return nil
	}
	bs, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("risk-control/cache marshal %s: %w", sanitizeKey(key), err)
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 60
	}
	return c.rds.Setex(ctx, key, string(bs), ttlSeconds)
}

// Del 删除缓存 key。
func (c *Cache) Del(ctx context.Context, key string) error {
	if key == "" || c == nil || c.rds == nil {
		return nil
	}
	_, err := c.rds.Del(ctx, key)
	return err
}

// sanitizeKey 截断日志用的 key，避免把完整设备/IP 摘要写进日志
// （日志本身可能被抓取，摘要前缀足够定位问题）。
func sanitizeKey(key string) string {
	if len(key) > 32 {
		return key[:32]
	}
	return key
}
