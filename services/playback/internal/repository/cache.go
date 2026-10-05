// Package repository 是 playback 服务的数据访问层。
// 组合 MySQL 模型、Redis 计数/缓存与 rights 服务的 RPC 客户端，为 logic 层提供
// 统一入口；业务规则仍留在 internal/logic（AGENTS.md §4）。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/playback/model"
)

// Redis key 约定（本服务独占前缀 pb:，不与其它服务共用；AGENTS.md §5）。
const (
	keySession   = "pb:s:%s"     // 播放会话短缓存：session_id
	keyVerified  = "pb:v:%s"     // 会话首次放行标记：session_id
	keyPlayCount = "pb:pc:%d:%d" // 内容播放计数：content_type:content_id

	cacheTTLSessionMax = 300   // 会话缓存最长 5 分钟，且不超过 expire_at
	cacheTTLSessionMin = 10    // 剩余时间不足时的最小缓存秒数
	counterTTL         = 86400 // 播放计数 key 的兜底 TTL（1 天）
)

// Cache 封装 playback 的 Redis 操作。
type Cache struct {
	rds *redis.Redis
}

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("playback/cache: redis ping failed")
}

// --- 会话缓存 ---

// GetSession 读取会话缓存；miss 返回 (nil, nil)。
func (c *Cache) GetSession(ctx context.Context, sessionId string) (*model.PlaybackSession, error) {
	bs, err := c.rds.GetCtx(ctx, fmt.Sprintf(keySession, sessionId))
	if err != nil {
		return nil, err
	}
	if bs == "" {
		return nil, nil
	}
	var s model.PlaybackSession
	if err := json.Unmarshal([]byte(bs), &s); err != nil {
		return nil, fmt.Errorf("playback/cache GetSession unmarshal: %w", err)
	}
	return &s, nil
}

// SetSession 写入会话缓存，TTL 取剩余有效期（限制在 [cacheTTLSessionMin, cacheTTLSessionMax]）。
func (c *Cache) SetSession(ctx context.Context, s *model.PlaybackSession, now int64) error {
	bs, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("playback/cache SetSession marshal: %w", err)
	}
	return c.rds.SetexCtx(ctx, fmt.Sprintf(keySession, s.SessionId), string(bs), sessionTTL(s.ExpireAt, now))
}

// DelSession 失效会话缓存（状态变更后调用）。
func (c *Cache) DelSession(ctx context.Context, sessionId string) error {
	_, err := c.rds.DelCtx(ctx, fmt.Sprintf(keySession, sessionId))
	return err
}

// sessionTTL 计算会话缓存秒数：不超过授权剩余时间，避免缓存比授权活得更久。
func sessionTTL(expireAt, now int64) int {
	remain := expireAt - now
	if remain <= 0 {
		return cacheTTLSessionMin
	}
	if remain > cacheTTLSessionMax {
		return cacheTTLSessionMax
	}
	if remain < cacheTTLSessionMin {
		return cacheTTLSessionMin
	}
	return int(remain)
}

// --- 回源放行与播放计数 ---

// MarkVerifiedOnce 尝试为会话打上"首次放行"标记（SETNX + TTL）。
// 返回 true 表示本次是首次放行，调用方应当累加播放计数；
// CDN 分片回源会多次校验同一会话，靠该标记保证计数不重复。
func (c *Cache) MarkVerifiedOnce(ctx context.Context, sessionId string, ttl int) (bool, error) {
	return c.rds.SetnxExCtx(ctx, fmt.Sprintf(keyVerified, sessionId), "1", ttl)
}

// IncrPlayCount 累加内容播放计数并返回累加后的值。
// 播放量是典型高并发计数：先写行为事实（本方法 + 会话表），再由 SPM 异步聚合，
// 不在请求线程同步更新其它服务的投影（AGENTS.md §5、docs/data-design.md §5）。
func (c *Cache) IncrPlayCount(ctx context.Context, contentType, contentId int64) (int64, error) {
	key := fmt.Sprintf(keyPlayCount, contentType, contentId)
	val, err := c.rds.IncrbyCtx(ctx, key, 1)
	if err != nil {
		return 0, err
	}
	// 兜底 TTL：计数 key 不做永久驻留，真实播放量以 event-collector/spm 聚合结果为准。
	if err := c.rds.ExpireCtx(ctx, key, counterTTL); err != nil {
		return val, fmt.Errorf("playback/cache IncrPlayCount expire: %w", err)
	}
	return val, nil
}

// PlayCount 读取当前播放计数；不存在返回 0。
func (c *Cache) PlayCount(ctx context.Context, contentType, contentId int64) (int64, error) {
	val, err := c.rds.GetCtx(ctx, fmt.Sprintf(keyPlayCount, contentType, contentId))
	if err != nil {
		return 0, err
	}
	if val == "" {
		return 0, nil
	}
	var n int64
	if _, err := fmt.Sscanf(val, "%d", &n); err != nil {
		return 0, fmt.Errorf("playback/cache PlayCount parse %q: %w", val, err)
	}
	return n, nil
}

// Cacher 是 Repository 对缓存的依赖面。生产实现是上面的 *Cache（Redis），
// logic/repository 的单元测试注入内存替身；跨包实现本接口不算破坏封装，
// 因为「一个会话只计一次播放」和「缓存不比授权活得更久」本身就是本服务的对外承诺。
type Cacher interface {
	Ping(ctx context.Context) error
	GetSession(ctx context.Context, sessionId string) (*model.PlaybackSession, error)
	SetSession(ctx context.Context, s *model.PlaybackSession, now int64) error
	DelSession(ctx context.Context, sessionId string) error
	// MarkVerifiedOnce 返回 true 表示该会话首次放行，调用方据此累加播放计数。
	MarkVerifiedOnce(ctx context.Context, sessionId string, ttl int) (bool, error)
	IncrPlayCount(ctx context.Context, contentType, contentId int64) (int64, error)
	PlayCount(ctx context.Context, contentType, contentId int64) (int64, error)
}

var _ Cacher = (*Cache)(nil)
