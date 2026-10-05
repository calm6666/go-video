// Package repository 是 operation 服务的数据访问与领域编排层。
// 组合本服务的 MySQL model、Redis 缓存和下游领域服务的 RPC 客户端：
//   - 权限判定结果、运营配置、菜单树走短缓存（写后立即失效）；
//   - 管理员口令只做 PBKDF2 校验，任何出参都不带散列；
//   - 稿件/目录/版权/审核的写动作只通过下游 RPC 发起，本服务不直连他人库表。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 缓存 key 约定（全部带 op: 前缀，与 account 的用户会话 key 完全隔离）。
const (
	keyRBACVersion  = "op:rbac:ver"     // 权限快照全局版本号（自增即整体失效）
	keyRBACSnapshot = "op:rbac:%d:%d"   // <version>:<admin_id> → 角色+权限快照
	keySession      = "op:sess:%s"      // <token> → 会话
	keyConfig       = "op:cfg:%s:%s"    // <scope>:<cfg_key> → 运营配置
	keyMenuVersion  = "op:menu:ver"     // 菜单树全局版本号
	keyMenuTree     = "op:menu:%d"      // <version> → 全量菜单节点
	versionKeyTTL   = 30 * 24 * 60 * 60 // 版本 key 保留 30 天
	defaultMissTTL  = 30                // 空值哨兵 TTL（防穿透）
	missPlaceholder = `{"miss":1}`      // 空值哨兵内容
)

// Store 抽象 Redis 的最小读写面，使权限/配置缓存逻辑可在单测中用内存实现覆盖，
// 不需要真实 Redis（AGENTS.md §9：测试不依赖不可用的外部服务）。
//
// 接口与方法是导出的（历史名称是 store/get/setex/del/incr）：Go 规定带非导出方法的
// 接口只能被同包类型实现，非导出时 logic 层的单测就无法注入内存实现，
// 只能把 Cache 一起 mock 掉——那会让「缓存里到底存了什么、key 怎么派生」脱离被测路径。
// 导出面后 logic 单测注入的是**真实 Cache + 内存 Store**，key 派生与 JSON 编解码仍被覆盖。
type Store interface {
	// Get 读取字符串；miss 返回 ("", false, nil)。
	Get(ctx context.Context, key string) (string, bool, error)
	// Setex 写入并设置 TTL（秒）。
	Setex(ctx context.Context, key string, val string, ttl int) error
	// Delete 删除若干 key。
	Delete(ctx context.Context, keys ...string) error
	// Incr 自增并在首次创建时设置 TTL，返回自增后的值。
	Incr(ctx context.Context, key string, ttl int) (int64, error)
}

// redisStore 是 Store 的生产实现。
type redisStore struct {
	rds *redis.Redis
}

func (s *redisStore) Get(ctx context.Context, key string) (string, bool, error) {
	if s.rds == nil {
		return "", false, errors.New("operation/cache: redis not configured")
	}
	val, err := s.rds.GetCtx(ctx, key)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, err
	}
	if val == "" {
		return "", false, nil
	}
	return val, true, nil
}

func (s *redisStore) Setex(ctx context.Context, key, val string, ttl int) error {
	if s.rds == nil {
		return errors.New("operation/cache: redis not configured")
	}
	if ttl <= 0 {
		ttl = defaultMissTTL
	}
	return s.rds.SetexCtx(ctx, key, val, ttl)
}

func (s *redisStore) Delete(ctx context.Context, keys ...string) error {
	if s.rds == nil {
		return errors.New("operation/cache: redis not configured")
	}
	if len(keys) == 0 {
		return nil
	}
	_, err := s.rds.DelCtx(ctx, keys...)
	return err
}

func (s *redisStore) Incr(ctx context.Context, key string, ttl int) (int64, error) {
	if s.rds == nil {
		return 0, errors.New("operation/cache: redis not configured")
	}
	n, err := s.rds.IncrCtx(ctx, key)
	if err != nil {
		return 0, err
	}
	if ttl > 0 {
		if err := s.rds.ExpireCtx(ctx, key, ttl); err != nil {
			return n, err
		}
	}
	return n, nil
}

// noopStore 是未配置 Redis 时的兜底实现：读永远 miss、写丢弃。
// 缓存只是加速手段，判定与配置的正确性依据始终是库，因此降级安全。
type noopStore struct{}

func (noopStore) Get(context.Context, string) (string, bool, error) { return "", false, nil }

func (noopStore) Setex(context.Context, string, string, int) error { return nil }

func (noopStore) Delete(context.Context, ...string) error { return nil }

func (noopStore) Incr(context.Context, string, int) (int64, error) { return 0, nil }

// Cache 封装 operation 的 Redis 读写（JSON 编解码 + 版本失效）。
type Cache struct {
	st Store
}

// NewCache 构造基于 Redis 的 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{st: &redisStore{rds: rds}}
}

// NewCacheWithStore 用显式 Store 构造 Cache（内存实现供 logic 层单测注入，
// 使真实 Cache 的 key 派生、JSON 编解码与版本号失效语义仍留在被测路径上）。
func NewCacheWithStore(st Store) *Cache {
	return &Cache{st: st}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c == nil || c.st == nil {
		return errors.New("operation/cache: redis not configured")
	}
	_, ok, err := c.st.Get(ctx, keyRBACVersion)
	if err != nil {
		return err
	}
	_ = ok
	return nil
}

// getJSON 读取并反序列化；miss 返回 hit=false。
// 反序列化失败视为 miss（缓存不应放大成故障）。
func (c *Cache) getJSON(ctx context.Context, key string, v any) (hit bool) {
	if c == nil || c.st == nil {
		return false
	}
	raw, ok, err := c.st.Get(ctx, key)
	if err != nil {
		logx.Errorf("operation/cache get %s: %v", key, err)
		return false
	}
	if !ok || raw == "" || raw == missPlaceholder {
		return false
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		logx.Errorf("operation/cache unmarshal %s: %v", key, err)
		return false
	}
	return true
}

// setJSON 序列化并写入，ttl <= 0 时使用默认 TTL。
func (c *Cache) setJSON(ctx context.Context, key string, v any, ttl int) {
	if c == nil || c.st == nil {
		return
	}
	bs, err := json.Marshal(v)
	if err != nil {
		logx.Errorf("operation/cache marshal %s: %v", key, err)
		return
	}
	if err := c.st.Setex(ctx, key, string(bs), ttl); err != nil {
		logx.Errorf("operation/cache set %s: %v", key, err)
	}
}

// setMiss 写入空值哨兵，避免不存在的 key 反复穿透到 DB。
func (c *Cache) setMiss(ctx context.Context, key string, ttl int) {
	if c == nil || c.st == nil {
		return
	}
	if err := c.st.Setex(ctx, key, missPlaceholder, ttl); err != nil {
		logx.Errorf("operation/cache miss-guard %s: %v", key, err)
	}
}

// del 删除 key（写操作后失效）。
func (c *Cache) del(ctx context.Context, keys ...string) {
	if c == nil || c.st == nil {
		return
	}
	if err := c.st.Delete(ctx, keys...); err != nil {
		logx.Errorf("operation/cache del %v: %v", keys, err)
	}
}

// version 读取版本 key；缺失或异常时返回 0（等价于“全部缓存作废”的安全侧）。
func (c *Cache) version(ctx context.Context, key string) int64 {
	if c == nil || c.st == nil {
		return 0
	}
	raw, ok, err := c.st.Get(ctx, key)
	if err != nil || !ok {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// bumpVersion 递增版本号，使以该版本为前缀的缓存整体失效。
// 采用版本号而不是 KEYS/SCAN 批量删除，避免生产 Redis 上的键扫描。
func (c *Cache) bumpVersion(ctx context.Context, key string) {
	if c == nil || c.st == nil {
		return
	}
	if _, err := c.st.Incr(ctx, key, versionKeyTTL); err != nil {
		logx.Errorf("operation/cache bump version %s: %v", key, err)
	}
}

// nowUnix 供本包统一取时间（便于阅读，不含业务语义）。
func nowUnix() int64 { return time.Now().Unix() }
