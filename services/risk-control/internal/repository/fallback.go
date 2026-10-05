package repository

import (
	"strconv"
	"time"

	"go-video/common/counter"
)

// localRateGuard 是 Redis 滑窗不可用时的进程内兜底限流器。
//
// 为什么需要它：计数器全部落在 Redis，一旦 Redis 故障，规则里的
// action_count/device_action_count 等指标会变成「不可观测」，
// 引擎会跳过这些规则 —— 攻击者正好可以在这个窗口里放大动作。
// 因此这里用 common/counter 的滑动窗口对每个受保护动作做一次
// 进程内影子计数（常态只记不判），仅在 Redis 不可用时参与裁决。
//
// key 维度是动作枚举（<=7 个），不随 mid/device 增长，内存上界可控。
type localRateGuard struct {
	group    *counter.CounterGroup
	interval time.Duration
	buckets  int
	limit    int64
	enabled  bool
}

// newLocalRateGuard 构造兜底限流器。
// windowSeconds <= 0 或 limit <= 0 时返回禁用态（enabled=false），行为退化为 no-op。
func newLocalRateGuard(windowSeconds int64, buckets int, limit int64) *localRateGuard {
	if buckets <= 0 {
		buckets = 6
	}
	if windowSeconds <= 0 || limit <= 0 {
		return &localRateGuard{interval: time.Duration(windowSeconds) * time.Second, buckets: buckets, limit: limit, enabled: false}
	}
	g := &localRateGuard{
		interval: time.Duration(windowSeconds) * time.Second,
		buckets:  buckets,
		limit:    limit,
		enabled:  true,
	}
	g.group = &counter.CounterGroup{New: func() counter.Counter {
		return counter.NewRolling(g.interval, g.buckets)
	}}
	return g
}

// observe 记录一次动作（常态调用，开销是一次内存原子加）。
func (g *localRateGuard) observe(action int32) {
	if g == nil || !g.enabled {
		return
	}
	g.group.Add(localKey(action), 1)
}

// limited 判定该动作在本实例的窗口内是否已超兜底阈值。
// 仅在 Redis 不可用时由 LoadFacts 调用，常态不参与裁决。
func (g *localRateGuard) limited(action int32) bool {
	if g == nil || !g.enabled {
		return false
	}
	return g.group.Value(localKey(action)) > g.limit
}

// current 返回窗口内累计值（测试与观测用）。
func (g *localRateGuard) current(action int32) int64 {
	if g == nil || !g.enabled {
		return 0
	}
	return g.group.Value(localKey(action))
}

// Enabled 报告兜底限流是否开启。
func (g *localRateGuard) Enabled() bool { return g != nil && g.enabled }

// localKey 返回兜底限流的进程内 key（按动作维度，数量有界）。
func localKey(action int32) string { return "action:" + strconv.FormatInt(int64(action), 10) }
