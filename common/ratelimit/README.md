# common/ratelimit

限流工具包，提供自适应限流与令牌桶限流两种实现，统一 `Limiter` 接口。

## 职责

- `Limiter` 接口：`Allow(ctx) (func(Op), error)`，调用方在收到 `nil` error 后必须在请求结束时调用返回的 `done` 反馈结果。
- `Op` 类型：`Success`/`Ignore`/`Drop`，用于反馈请求处理结果，参与自适应调节。
- CoDel 受控延迟队列：在队列停留时间超过目标时按算法丢弃请求，保护后端。
- Vegas 自适应限流器：基于 TCP Vegas 拥塞控制思路，依据 RTT 采样动态调整并发上限。
- 自适应限流器：组合 Vegas 与 CoDel，超过当前并发上限的请求进入 CoDel 队列等待。
- 令牌桶限流器：基于 `golang.org/x/time/rate` 的标准令牌桶，适用于固定 QPS 场景。

## 依赖

- 标准库：`context`、`errors`、`math`、`math/rand`、`sync`、`sync/atomic`、`time`。
- 外部高性能库：`golang.org/x/time/rate`（业界标准令牌桶实现）。
- 不依赖 `go-common/library/log`、`ecode`、`net/trace` 等内部包；日志由调用方通过 `Stat()` 自行采集。

## API

### 通用

| 符号 | 说明 |
|---|---|
| `type Op int` | 请求结果反馈类型 |
| `const Success Op` | 请求成功完成，参与 RTT 采样与限流调节 |
| `const Ignore Op` | 不计入统计（如客户端取消、上游错误），不更新 RTT |
| `const Drop Op` | 请求被显式丢弃，触发限流器降低窗口 |
| `type Limiter interface` | `Allow(ctx) (func(Op), error)` |
| `var ErrLimitExceed` | 超过当前限流窗口或队列已满 |
| `var ErrDeadline` | 请求在等待被允许前已超过截止时间 |

### CoDel 队列

| 符号 | 说明 |
|---|---|
| `type CoDelConfig struct` | `Target`/`Internal`（毫秒） |
| `type CoDelStat struct` | `Dropping`/`FaTime`/`DropNext`/`Packets` |
| `func DefaultCoDelQueue() *CoDelQueue` | 使用默认配置（Target 50ms，Internal 500ms） |
| `func NewCoDelQueue(*CoDelConfig) *CoDelQueue` | 创建队列，nil 配置使用默认值 |
| `func (q *CoDelQueue) Push(ctx) error` | 入队并等待被 `Pop` 唤醒或超时；返回 `nil` 表示通过 |
| `func (q *CoDelQueue) Pop()` | 取出一个报文并通知其等待方 |
| `func (q *CoDelQueue) Stat() CoDelStat` | 当前统计快照 |
| `func (q *CoDelQueue) Reload(*CoDelConfig)` | 热更新配置；忽略非法值 |

### Vegas 自适应限流器

| 符号 | 说明 |
|---|---|
| `type VegasStat struct` | `Limit`/`InFlight`/`MinRTT`/`LastRTT` |
| `func NewVegas() *Vegas` | 创建 Vegas 限流器 |
| `func (v *Vegas) Acquire() (func(time.Time, Op), bool)` | 尝试获取槽位；返回的 `done` 必须在请求结束时调用，`bool` 为 true 表示直接通过 |
| `func (v *Vegas) Stat() VegasStat` | 当前统计快照 |
| `func (v *Vegas) Reset()` | 恢复到初始状态 |

### 自适应限流器

| 符号 | 说明 |
|---|---|
| `type AdaptiveStat struct` | 合并 `Vegas` 与 `CoDel` 统计 |
| `func NewAdaptiveLimiter(*CoDelConfig) *AdaptiveLimiter` | 创建自适应限流器 |
| `func (l *AdaptiveLimiter) Allow(ctx) (func(Op), error)` | 实现 `Limiter` |
| `func (l *AdaptiveLimiter) Stat() AdaptiveStat` | 合并统计 |

### 令牌桶限流器

| 符号 | 说明 |
|---|---|
| `func NewTokenBucket(ratePerSec, burst int) *TokenBucket` | 创建令牌桶；`ratePerSec` 每秒填充令牌数，`burst` 桶容量 |
| `func (t *TokenBucket) Allow(ctx) (func(Op), error)` | 实现 `Limiter`；无令牌时立即返回 `ErrLimitExceed` |

## 使用示例

### 自适应限流（gateway 鉴权后保护下游）

```go
import (
    "context"
    "go-video/common/ratelimit"
)

limiter := ratelimit.NewAdaptiveLimiter(nil)

func handle(ctx context.Context) error {
    done, err := limiter.Allow(ctx)
    if err != nil {
        return err
    }
    defer done(ratelimit.Success)
    // 业务处理
    return nil
}
```

### 固定 QPS 令牌桶

```go
limiter := ratelimit.NewTokenBucket(1000, 50) // 1000 QPS，突发 50

done, err := limiter.Allow(ctx)
if err != nil {
    // 限流，返回 429
}
defer done(ratelimit.Success)
```

### 采集统计

```go
stat := limiter.Stat()
// stat.Vegas.Limit、stat.CoDel.Packets 等可写入日志或 Prometheus
```

## 实现约定

- `Limiter.Allow` 返回非 `nil` error 时，调用方无需调用 `done`；返回 `nil` error 时必须调用 `done`，否则自适应统计会失真。
- Vegas 的 `done` 第二参数 `Op` 仅 `Success` 与 `Drop` 参与 RTT 采样；`Ignore` 仅归还 inflight 计数。
- CoDel 队列容量固定为 2048，超过容量立即返回 `ErrLimitExceed`，不阻塞。
- 令牌桶不阻塞等待，无令牌立即拒绝；需要平滑等待的场景由调用方自行重试或退避。
- 自适应限流器不再内置后台日志 goroutine，统计通过 `Stat()` 按需采集，由调用方决定输出方式。
- Vegas 限流上下限为 8 ~ 2048，采样窗口 500ms ~ 2000ms，由算法自动选择。
- 全部代码注释与文档使用中文。
