package repository

import (
	"context"
	"errors"
	"strconv"

	"go-video/services/risk-control/model"

	"github.com/zeromicro/go-zero/core/stores/redis"
)

// counterBackend 是 Counter 对 Redis 计数原语的最小依赖面（与 cache.go 的 cacheBackend
// 同口径的注入缝：只为 internal/logic 的单测组装**真实 Counter** 而存在，
// 使档位选择、桶数学、TTL 计算、MGET 求和这些生产语义仍留在被测路径上。
// 生产路径仍然只走 New → newCounter(newRedisCounterBackend(rds), ...)。
type counterBackend interface {
	// IncrByBatch 在**一次** pipeline 内对每个 key INCRBY delta 并 EXPIRE 对应 TTL；
	// 真实实现逐条转调 PipelinedCtx，命令序列与抽取前逐字一致。
	IncrByBatch(ctx context.Context, keys []string, delta int64, ttls []int) error
	// Mget 一次读取多个 key，缺失的 key 返回空串（Redis MGET 语义）。
	Mget(ctx context.Context, keys ...string) ([]string, error)
	// Setnxex 仅当 key 不存在时写入并设过期；返回 false 表示 key 已存在（重复上报）。
	Setnxex(ctx context.Context, key, value string, ttlSeconds int) (bool, error)
	// Ping 连通性探测。
	Ping() bool
}

// redisCounterBackend 是 counterBackend 的真实实现。
type redisCounterBackend struct{ rds *redis.Redis }

// newRedisCounterBackend 包装真实客户端；rds 为 nil 时返回 nil 接口。
func newRedisCounterBackend(rds *redis.Redis) counterBackend {
	if rds == nil {
		return nil
	}
	return redisCounterBackend{rds: rds}
}

func (b redisCounterBackend) IncrByBatch(ctx context.Context, keys []string, delta int64, ttls []int) error {
	return b.rds.PipelinedCtx(ctx, func(pipe redis.Pipeliner) error {
		for i, key := range keys {
			pipe.IncrBy(ctx, key, delta)
			pipe.Expire(ctx, key, seconds(ttls[i]))
		}
		return nil
	})
}

func (b redisCounterBackend) Mget(ctx context.Context, keys ...string) ([]string, error) {
	return b.rds.MgetCtx(ctx, keys...)
}

func (b redisCounterBackend) Setnxex(ctx context.Context, key, value string, ttlSeconds int) (bool, error) {
	return b.rds.SetnxExCtx(ctx, key, value, ttlSeconds)
}

func (b redisCounterBackend) Ping() bool { return b.rds.Ping() }

// Counter 是 Redis 多档滑动窗口计数器。
//
// 写入：一次动作对每个档位各 +1（key 带上档位与桶号，天然滚动），
// 读取：按规则窗口挑档位、算桶区间、MGET 求和。
// 这样单条规则不需要维护 ZSET 明细，读放大固定为 span 次 GET（合批一次 MGET）。
type Counter struct {
	rds     counterBackend
	tiers   []int64
	buckets int
	// defaultWindow 是 ReportAction 未指定窗口时使用的窗口（也是回包里的 window_seconds）。
	defaultWindow int64
}

// newCounter 构造计数器。tiers 为空时回落到 DefaultCounterTiers。
func newCounter(rds counterBackend, tiers []int64, buckets int, defaultWindow int64) *Counter {
	if buckets <= 0 {
		buckets = 6
	}
	if defaultWindow <= 0 {
		defaultWindow = 60
	}
	return &Counter{
		rds:           rds,
		tiers:         normalizeTiers(tiers),
		buckets:       buckets,
		defaultWindow: defaultWindow,
	}
}

// Ping 检查 Redis 连通性。
func (c *Counter) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("risk-control/cache: redis ping failed")
}

// Subject 是一个计数主体（已经过脱敏的受控标识）。
type Subject struct {
	Kind  string // mid / device / ip
	Value string // 受控值；device/ip 均为 hash
}

// SubjectKey 拼装计数器主体标识。返回空串表示该主体不可用（跳过该维度计数）。
func (s Subject) SubjectKey() string {
	switch s.Kind {
	case "mid":
		if s.Value == "" || s.Value == "0" {
			return ""
		}
		n, err := strconv.ParseInt(s.Value, 10, 64)
		if err != nil || n <= 0 {
			return ""
		}
		return subjectMid(n)
	case "device":
		if s.Value == "" {
			return ""
		}
		return subjectDevice(s.Value)
	case "ip":
		if s.Value == "" {
			return ""
		}
		return subjectIP(s.Value)
	default:
		return ""
	}
}

// IncrRequest 是一次计数写入。
type IncrRequest struct {
	Action     int32
	Subjects   []Subject
	Count      int64
	OccurredAt int64
}

// Incr 把动作计入所有档位的当前桶。
// 单次 pipeline 完成，返回 error 表示 Redis 写入失败（调用方需记录降级，不得伪造成功）。
func (c *Counter) Incr(ctx context.Context, req IncrRequest) error {
	if !model.ValidAction(req.Action) {
		return model.ErrInvalidTarget
	}
	count := req.Count
	if count <= 0 {
		count = 1
	}
	now := req.OccurredAt
	if now <= 0 {
		now = nowUnix()
	}

	keys := make([]string, 0, len(c.tiers)*len(req.Subjects))
	ttls := make([]int, 0, len(keys))
	for _, s := range req.Subjects {
		sk := s.SubjectKey()
		if sk == "" {
			continue
		}
		for _, tier := range c.tiers {
			bs := bucketSize(tier, c.buckets)
			key := counterKey(metricForSubject(s.Kind), sk, req.Action, tier, bucketIndex(now, bs))
			keys = append(keys, key)
			// TTL 取「窗口 + 一个桶」，保证窗口内最老的桶不会被提前清理。
			ttls = append(ttls, clampInt(tier+bs))
		}
	}
	if len(keys) == 0 {
		return nil
	}

	return c.rds.IncrByBatch(ctx, keys, count, ttls)
}

// metricForSubject 把主体种类映射到指标名，供 Sum 反查使用。
func metricForSubject(kind string) string {
	switch kind {
	case "device":
		return model.MetricDeviceActionCount
	case "ip":
		return model.MetricIpActionCount
	default:
		return model.MetricActionCount
	}
}

// Sum 返回某主体在给定窗口内的累计动作数。
// ok=false 表示窗口超出已配置档位（不可观测），调用方必须把该规则标记为 skipped。
func (c *Counter) Sum(ctx context.Context, metric, subjectKey string, action int32, window, now int64) (int64, bool, error) {
	if subjectKey == "" {
		return 0, false, nil
	}
	keys, _, _, ok := counterKeys(metric, subjectKey, action, c.tiers, c.buckets, window, now)
	if !ok {
		return 0, false, nil
	}
	values, err := c.rds.Mget(ctx, keys...)
	if err != nil {
		return 0, true, err
	}
	return parseCounts(values), true, nil
}

// MarkEventOnce 登记行为上报的幂等标记，返回 false 表示此前已登记过（重复上报）。
// ttl 由 dedupTTLSeconds 给出；eventID 为空时视为不去重，返回 true。
func (c *Counter) MarkEventOnce(ctx context.Context, eventID string, ttlSeconds int) (bool, error) {
	key := eventKey(eventID)
	if key == "" {
		return true, nil
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 120
	}
	ok, err := c.rds.Setnxex(ctx, key, "1", ttlSeconds)
	if err != nil {
		return false, err
	}
	return ok, nil
}
