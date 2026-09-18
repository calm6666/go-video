package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/account/rpc"
	"google.golang.org/protobuf/proto"
)

// 缓存 key 前缀，沿用参考仓库约定，便于跨语言联调。
const (
	prefixInfo    = "i3_"
	prefixCard    = "c3_"
	prefixVip     = "v3_"
	prefixProfile = "p3_"

	cacheExpireSeconds = 3600 // 缓存默认过期时间 1 小时
)

func keyInfo(mid int64) string    { return prefixInfo + strconv.FormatInt(mid, 10) }
func keyCard(mid int64) string    { return prefixCard + strconv.FormatInt(mid, 10) }
func keyVip(mid int64) string     { return prefixVip + strconv.FormatInt(mid, 10) }
func keyProfile(mid int64) string { return prefixProfile + strconv.FormatInt(mid, 10) }

// Cache 封装 account 服务的 Redis 缓存操作。
// proto 消息使用 protobuf 二进制存储，比 JSON 体积小且解析快。
type Cache struct {
	rds    *redis.Redis
	expire time.Duration
}

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{
		rds:    rds,
		expire: time.Duration(cacheExpireSeconds) * time.Second,
	}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("account/cache: redis ping failed")
}

// ==================== Info ====================

// CacheInfo 从缓存读取单个 Info。
func (c *Cache) CacheInfo(ctx context.Context, mid int64) (*rpc.Info, error) {
	bs, err := c.rds.GetCtx(ctx, keyInfo(mid))
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		logx.Errorf("account/cache: get info mid=%d err=%v", mid, err)
		return nil, nil
	}
	if len(bs) == 0 {
		return nil, nil
	}
	info := &rpc.Info{}
	if err := proto.Unmarshal([]byte(bs), info); err != nil {
		logx.Errorf("account/cache: unmarshal info mid=%d err=%v", mid, err)
		return nil, nil
	}
	return info, nil
}

// AddCacheInfo 写入单个 Info 缓存。
func (c *Cache) AddCacheInfo(ctx context.Context, mid int64, info *rpc.Info) {
	if info == nil {
		return
	}
	bs, err := proto.Marshal(info)
	if err != nil {
		logx.Errorf("account/cache: marshal info mid=%d err=%v", mid, err)
		return
	}
	if err := c.rds.SetexCtx(ctx, keyInfo(mid), string(bs), int(cacheExpireSeconds)); err != nil {
		logx.Errorf("account/cache: set info mid=%d err=%v", mid, err)
	}
}

// AddCacheInfos 批量写入 Info 缓存。
func (c *Cache) AddCacheInfos(ctx context.Context, infos map[int64]*rpc.Info) {
	for mid, info := range infos {
		c.AddCacheInfo(ctx, mid, info)
	}
}

// ==================== Card ====================

// CacheCard 从缓存读取单个 Card。
func (c *Cache) CacheCard(ctx context.Context, mid int64) (*rpc.Card, error) {
	bs, err := c.rds.GetCtx(ctx, keyCard(mid))
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		logx.Errorf("account/cache: get card mid=%d err=%v", mid, err)
		return nil, nil
	}
	if len(bs) == 0 {
		return nil, nil
	}
	card := &rpc.Card{}
	if err := proto.Unmarshal([]byte(bs), card); err != nil {
		logx.Errorf("account/cache: unmarshal card mid=%d err=%v", mid, err)
		return nil, nil
	}
	return card, nil
}

// AddCacheCard 写入单个 Card 缓存。
func (c *Cache) AddCacheCard(ctx context.Context, mid int64, card *rpc.Card) {
	if card == nil {
		return
	}
	bs, err := proto.Marshal(card)
	if err != nil {
		logx.Errorf("account/cache: marshal card mid=%d err=%v", mid, err)
		return
	}
	if err := c.rds.SetexCtx(ctx, keyCard(mid), string(bs), int(cacheExpireSeconds)); err != nil {
		logx.Errorf("account/cache: set card mid=%d err=%v", mid, err)
	}
}

// AddCacheCards 批量写入 Card 缓存。
func (c *Cache) AddCacheCards(ctx context.Context, cards map[int64]*rpc.Card) {
	for mid, card := range cards {
		c.AddCacheCard(ctx, mid, card)
	}
}

// ==================== Profile ====================

// CacheProfile 从缓存读取单个 Profile。
func (c *Cache) CacheProfile(ctx context.Context, mid int64) (*rpc.Profile, error) {
	bs, err := c.rds.GetCtx(ctx, keyProfile(mid))
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		logx.Errorf("account/cache: get profile mid=%d err=%v", mid, err)
		return nil, nil
	}
	if len(bs) == 0 {
		return nil, nil
	}
	p := &rpc.Profile{}
	if err := proto.Unmarshal([]byte(bs), p); err != nil {
		logx.Errorf("account/cache: unmarshal profile mid=%d err=%v", mid, err)
		return nil, nil
	}
	return p, nil
}

// AddCacheProfile 写入单个 Profile 缓存。
func (c *Cache) AddCacheProfile(ctx context.Context, mid int64, p *rpc.Profile) {
	if p == nil {
		return
	}
	bs, err := proto.Marshal(p)
	if err != nil {
		logx.Errorf("account/cache: marshal profile mid=%d err=%v", mid, err)
		return
	}
	if err := c.rds.SetexCtx(ctx, keyProfile(mid), string(bs), int(cacheExpireSeconds)); err != nil {
		logx.Errorf("account/cache: set profile mid=%d err=%v", mid, err)
	}
}

// ==================== Vip ====================

// CacheVip 从缓存读取单个 VipInfo。
func (c *Cache) CacheVip(ctx context.Context, mid int64) (*rpc.VipInfo, error) {
	bs, err := c.rds.GetCtx(ctx, keyVip(mid))
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		logx.Errorf("account/cache: get vip mid=%d err=%v", mid, err)
		return nil, nil
	}
	if len(bs) == 0 {
		return nil, nil
	}
	v := &rpc.VipInfo{}
	if err := proto.Unmarshal([]byte(bs), v); err != nil {
		logx.Errorf("account/cache: unmarshal vip mid=%d err=%v", mid, err)
		return nil, nil
	}
	return v, nil
}

// AddCacheVip 写入单个 VipInfo 缓存。
func (c *Cache) AddCacheVip(ctx context.Context, mid int64, v *rpc.VipInfo) {
	if v == nil {
		return
	}
	bs, err := proto.Marshal(v)
	if err != nil {
		logx.Errorf("account/cache: marshal vip mid=%d err=%v", mid, err)
		return
	}
	if err := c.rds.SetexCtx(ctx, keyVip(mid), string(bs), int(cacheExpireSeconds)); err != nil {
		logx.Errorf("account/cache: set vip mid=%d err=%v", mid, err)
	}
}

// AddCacheVips 批量写入 VipInfo 缓存。
func (c *Cache) AddCacheVips(ctx context.Context, vips map[int64]*rpc.VipInfo) {
	for mid, v := range vips {
		c.AddCacheVip(ctx, mid, v)
	}
}

// ==================== Del ====================

// DelCache 删除指定 mid 的全部缓存（Info/Card/Profile/Vip）。
// 返回所有删除操作中发生的错误。
func (c *Cache) DelCache(ctx context.Context, mid int64) []error {
	keys := []string{keyInfo(mid), keyCard(mid), keyVip(mid), keyProfile(mid)}
	errs := make([]error, 0, len(keys))
	for _, key := range keys {
		if _, err := c.rds.DelCtx(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// ==================== 登录域通用缓存 ====================

// getJSON 读取并反序列化 JSON 缓存；miss 返回 nil 错误且目标保持零值。
func (c *Cache) getJSON(ctx context.Context, key string, v any) error {
	bs, err := c.rds.GetCtx(ctx, key)
	if err != nil || len(bs) == 0 {
		return err
	}
	return json.Unmarshal([]byte(bs), v)
}

// setJSON 写入 JSON 缓存（带过期时间）。
func (c *Cache) setJSON(ctx context.Context, key string, v any, ttlSeconds int) {
	bs, err := json.Marshal(v)
	if err != nil {
		logx.Errorf("account/cache: marshal %s err=%v", key, err)
		return
	}
	if err := c.rds.SetexCtx(ctx, key, string(bs), ttlSeconds); err != nil {
		logx.Errorf("account/cache: set %s err=%v", key, err)
	}
}

// del 删除缓存 key（miss 不视为错误）。
func (c *Cache) del(ctx context.Context, key string) error {
	_, err := c.rds.DelCtx(ctx, key)
	if err != nil && err != redis.Nil {
		return err
	}
	return nil
}

// getInt 读取整数缓存；miss 返回 (0, false)。
func (c *Cache) getInt(ctx context.Context, key string) (int64, bool) {
	bs, err := c.rds.GetCtx(ctx, key)
	if err != nil || len(bs) == 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(string(bs), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// setInt 写入整数缓存（带过期时间）。
func (c *Cache) setInt(ctx context.Context, key string, v int64, ttlSeconds int) error {
	return c.rds.SetexCtx(ctx, key, strconv.FormatInt(v, 10), ttlSeconds)
}

// incrCaptureTimes 验证码发送次数 +1（首次写入补 TTL）。
func (c *Cache) incrCaptureTimes(ctx context.Context, key string) {
	if _, err := c.rds.IncrCtx(ctx, key); err != nil {
		logx.Errorf("account/cache: incr %s err=%v", key, err)
		return
	}
	if v, ok := c.getInt(ctx, key); ok && v <= 1 {
		_ = c.rds.ExpireCtx(ctx, key, captureTimesTTL)
	}
}

// incrCaptureErrTimes 验证码错误次数 +1（首次写入补 TTL）。
func (c *Cache) incrCaptureErrTimes(ctx context.Context, key string) {
	if _, err := c.rds.IncrCtx(ctx, key); err != nil {
		logx.Errorf("account/cache: incr %s err=%v", key, err)
		return
	}
	if v, ok := c.getInt(ctx, key); ok && v <= 1 {
		_ = c.rds.ExpireCtx(ctx, key, captureErrTTL)
	}
}
