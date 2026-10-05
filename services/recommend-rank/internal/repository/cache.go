package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// Cache 是排序在线面的只读快照缓存。
//
// 定位（重要）：缓存不是任何事实的来源，只是 MySQL 读放大的保护。
//   - 配置类快照（ACTIVE 模型版本、RUNNING 变体集合、运行时参数）过期后回源 DB，
//     所以「激活模型」最迟 RuntimeConfigCacheTTLSeconds 秒后在全集群生效；
//     写接口成功后调用 InvalidateModelConfig 收敛本实例，跨实例靠 TTL 收敛，
//     不为此引入广播通道（配置生效秒级延迟可接受，多一条通道多一份故障面）；
//   - 排序决策摘要（rank_decision_log）不整条进缓存，只缓存 request_id -> decision_id
//     的幂等回放指针，回放内容仍以 DB 为准；
//   - Redis 故障一律降级成「未命中」并记日志：排序必须在无缓存下继续工作，
//     只有落库失败才算存储故障（对应 model.DegradeReasonStoreUnavailable）。
//
// rds 为 nil 时整层视为关闭（未配 CacheRedis 的实例与单测），行为等价于「全部未命中」，
// 不影响正确性，只影响延迟。
type Cache struct {
	rds *redis.Redis
}

// NewCache 构造缓存层。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// enabled 判定缓存是否可用。
func (c *Cache) enabled() bool { return c != nil && c.rds != nil }

// 缓存 key 前缀。带服务与版本段：换口径时改 version 即整体自然失效，
// 不需要跑清 key 脚本，也不会和同实例其他服务的 key 撞名。
const keyVersion = "v1"

const (
	keyPrefixRuntimeConfig   = "rk:" + keyVersion + ":rtcfg:"
	keyPrefixActiveModel     = "rk:" + keyVersion + ":activemodel:"
	keyPrefixRunningExp      = "rk:" + keyVersion + ":runexp:"
	keyPrefixDecisionPointer = "rk:" + keyVersion + ":decision:"
	keyPrefixAssignment      = "rk:" + keyVersion + ":assign:"
)

// RuntimeConfigKey 是 GetRankRuntimeConfig 的快照 key（按 model_key 维度）。
func RuntimeConfigKey(modelKey string) string { return keyPrefixRuntimeConfig + modelKey }

// ActiveModelKey 是某 model_key 的 ACTIVE 版本快照 key。
func ActiveModelKey(modelKey string) string { return keyPrefixActiveModel + modelKey }

// RunningExperimentsKey 是 RUNNING 变体集合的快照 key。
// 按分钟分片：这样「新增一个变体」不需要清缓存，最多影响下一分钟，
// 避免写路径依赖清 key 的正确性（漏清就等于配置永远不生效）。
func RunningExperimentsKey(at int64) string {
	if at < 0 {
		at = 0
	}
	return fmt.Sprintf("%s%d", keyPrefixRunningExp, at/60)
}

// DecisionPointerKey 是 request_id -> decision_id 的幂等回放指针 key。
func DecisionPointerKey(requestID string) string { return keyPrefixDecisionPointer + requestID }

// AssignmentKey 是主体在实验当前哈希盐下的分桶快照 key。
// 含 hash_seed：换盐后旧 key 自然失效，不会把旧分组回灌给新分桶。
func AssignmentKey(expKey string, subjectType int32, subjectID, hashSeed string) string {
	return fmt.Sprintf("%s%s:%d:%s:%s", keyPrefixAssignment, expKey, subjectType, subjectID, hashSeed)
}

// Ping 检查 Redis 可用性；未启用缓存时视为可用（关闭不等于故障）。
func (c *Cache) Ping(ctx context.Context) error {
	if !c.enabled() {
		return nil
	}
	if c.rds.PingCtx(ctx) {
		return nil
	}
	return errors.New("recommend-rank/cache: redis ping failed")
}

// SetSnapshot 写入 JSON 快照；ttlSeconds<=0 表示「不缓存」（配置里 0 就是这个语义）。
// 失败只记日志：写缓存失败不该让一次排序失败。
func (c *Cache) SetSnapshot(ctx context.Context, key string, value interface{}, ttlSeconds int64) {
	if !c.enabled() || ttlSeconds <= 0 {
		return
	}
	payload, err := json.Marshal(value)
	if err != nil {
		logx.WithContext(ctx).Errorf("recommend-rank/cache marshal key=%s err=%v", key, err)
		return
	}
	if err := c.rds.SetexCtx(ctx, key, string(payload), int(ttlSeconds)); err != nil {
		logx.WithContext(ctx).Errorf("recommend-rank/cache setex key=%s err=%v", key, err)
	}
}

// GetSnapshot 读取 JSON 快照；未命中、反序列化失败或 Redis 故障都返回 false，
// 调用方回源 DB —— 宁可多读一次库，也不返回半截数据。
func (c *Cache) GetSnapshot(ctx context.Context, key string, dst interface{}) bool {
	if !c.enabled() {
		return false
	}
	raw, err := c.rds.GetCtx(ctx, key)
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			logx.WithContext(ctx).Errorf("recommend-rank/cache get key=%s err=%v", key, err)
		}
		return false
	}
	if raw == "" {
		return false
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		logx.WithContext(ctx).Errorf("recommend-rank/cache unmarshal key=%s err=%v", key, err)
		return false
	}
	return true
}

// DelKeys 批量失效 key（写接口在本地收敛配置缓存时调用）。
func (c *Cache) DelKeys(ctx context.Context, keys ...string) {
	if !c.enabled() || len(keys) == 0 {
		return
	}
	if _, err := c.rds.DelCtx(ctx, keys...); err != nil {
		logx.WithContext(ctx).Errorf("recommend-rank/cache del keys=%v err=%v", keys, err)
	}
}

// InvalidateModelConfig 失效某 model_key 的运行时配置与 ACTIVE 快照。
// SetModelVersionState 成功后调用，避免本实例在 TTL 内继续用旧版本打分。
func (c *Cache) InvalidateModelConfig(ctx context.Context, modelKey string) {
	c.DelKeys(ctx, RuntimeConfigKey(modelKey), ActiveModelKey(modelKey))
}

// SetDecisionPointer 记录 request_id -> decision_id 的幂等回放指针。
func (c *Cache) SetDecisionPointer(ctx context.Context, requestID, decisionID string, ttlSeconds int64) {
	if !c.enabled() || requestID == "" || decisionID == "" || ttlSeconds <= 0 {
		return
	}
	if err := c.rds.SetexCtx(ctx, DecisionPointerKey(requestID), decisionID, int(ttlSeconds)); err != nil {
		logx.WithContext(ctx).Errorf("recommend-rank/cache decision pointer req=%s err=%v", requestID, err)
	}
}

// DecisionPointer 读取回放指针；未命中返回空串（调用方回源 DB）。
func (c *Cache) DecisionPointer(ctx context.Context, requestID string) string {
	if !c.enabled() || requestID == "" {
		return ""
	}
	value, err := c.rds.GetCtx(ctx, DecisionPointerKey(requestID))
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			logx.WithContext(ctx).Errorf("recommend-rank/cache decision pointer get req=%s err=%v", requestID, err)
		}
		return ""
	}
	return value
}
