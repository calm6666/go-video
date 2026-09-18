# common/counter

线程安全计数器原语，提供原子计数、gauge 与滑动窗口三种实现，并支持按 key 分组。

## 职责

- `Counter` 接口：`Add(int64)`、`Value() int64`、`Reset()`。
- `NewAtomic()`：基于 `sync/atomic` 的原子计数器，单调累加（可传负数回退）。
- `NewGauge()`：gauge 计数器，语义上表示可上下波动的瞬时值，实现与原子计数器一致。
- `NewRolling(window, buckets)`：滑动窗口计数器，仅统计窗口内有效桶的累计值，过期桶自动重置。
- `CounterGroup`：按 key 分组的计数器集合，首次访问 key 时通过 `New` 构造计数器。

## 依赖

- 仅标准库：`sync`、`sync/atomic`、`time`。

## API

| 符号 | 说明 |
|---|---|
| `type Counter interface` | `Add(int64)` / `Value() int64` / `Reset()` |
| `func NewAtomic() Counter` | 原子计数器 |
| `func NewGauge() Counter` | gauge 计数器 |
| `func NewRolling(window time.Duration, winBuckets int) Counter` | 滑动窗口；`window` 为整体跨度，`winBuckets` 为桶数 |
| `type CounterGroup struct` | `New func() Counter` 字段用于指定构造函数 |
| `func (g *CounterGroup) Add(key string, value int64)` | 按 key 累加；key 不存在则新建 |
| `func (g *CounterGroup) Value(key string) int64` | 按 key 取当前值；不存在返回 0 |
| `func (g *CounterGroup) Reset(key string)` | 按 key 重置 |

## 使用示例

### 原子计数

```go
import "go-video/common/counter"

c := counter.NewAtomic()
c.Add(1)
c.Add(2)
_ = c.Value() // 3
c.Reset()
_ = c.Value() // 0
```

### 滑动窗口（最近 1 秒计数）

```go
import (
    "time"
    "go-video/common/counter"
)

c := counter.NewRolling(time.Second, 10) // 10 桶，每桶 100ms
c.Add(1)
_ = c.Value() // 窗口内累计值
```

### 按业务 key 分组

```go
g := &counter.CounterGroup{
    New: func() counter.Counter { return counter.NewRolling(time.Second, 10) },
}
g.Add("video.play", 1)
g.Add("video.play", 1)
g.Add("video.like", 1)
_ = g.Value("video.play") // 2
_ = g.Value("video.like") // 1
g.Reset("video.play")
_ = g.Value("video.play") // 0
```

## 实现约定

- 全部实现线程安全；原子计数器与 gauge 使用 `sync/atomic`，滑动窗口使用 `sync.RWMutex`。
- 滑动窗口的桶为环形链表；`Add` 时按当前时间推进游标并重置过期桶，`Value` 跳过已过期桶。
- `CounterGroup` 使用读多写少的双检锁模式：读路径用 `RLock`，仅新建 key 时升为写锁。
- `CounterGroup.New` 必须由调用方在构造时设置，否则 `Add` 未知 key 时会 panic（与原实现一致）。
- 全部代码注释与文档使用中文。
