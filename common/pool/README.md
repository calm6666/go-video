# common/pool

通用对象池，统一管理可复用资源（如连接、客户端）的获取、归还、空闲清理与关闭，避免频繁创建销毁。

## 职责

- `Pool` 接口抽象对象池：`Get` 取对象、`Put` 归还对象、`Close` 关闭池。
- `Config` 控制池行为：`Active`（活跃上限）、`Idle`（空闲上限）、`IdleTimeout`（空闲超时清理）、`WaitTimeout`（等待超时）、`Wait`（是否阻塞等待）。
- `NewSlice` 基于切片实现，支持异步 opener 与等待请求队列，适合高并发短任务。
- `NewList` 基于双向链表实现，支持 cond 信号唤醒等待者，结构更轻量。
- 两种实现均支持空闲对象超时清理 goroutine。

## 依赖

- 标准库（`container/list`、`context`、`errors`、`io`、`sync`、`time`）。
- [common/timeutil](../timeutil)：`Duration` 类型用于配置项，支持从字符串解析并按 Context deadline 截断。

## API

| 符号 | 说明 |
|---|---|
| `var ErrPoolExhausted` | 活跃对象耗尽且不允许等待时返回 |
| `var ErrPoolClosed` | 池已关闭时返回 |
| `type Config struct` | 池配置：`Active`/`Idle`/`IdleTimeout`/`WaitTimeout`/`Wait` |
| `type Pool interface` | `Get(ctx)`/`Put(ctx, c, forceClose)`/`Close()` |
| `func NewSlice(c *Config) *Slice` | 创建切片池；要求 `Active >= Idle` |
| `func NewList(c *Config) *List` | 创建链表池；要求 `Active >= Idle` |
| `func (p *Slice) Reload(c *Config) error` | 热更新切片池配置 |
| `func (p *List) Reload(c *Config) error` | 热更新链表池配置 |

### Slice 字段

| 符号 | 说明 |
|---|---|
| `Slice.New func(ctx) (io.Closer, error)` | 应用提供的对象构造函数 |

### List 字段

| 符号 | 说明 |
|---|---|
| `List.New func(ctx) (io.Closer, error)` | 应用提供的对象构造函数 |

## 使用示例

```go
package demo

import (
    "context"
    "io"

    "go-video/common/pool"
    "go-video/common/timeutil"
)

type conn struct{}

func (c *conn) Close() error { return nil }

func newPool() *pool.List {
    cfg := &pool.Config{
        Active:      10,
        Idle:        5,
        IdleTimeout: timeutil.Duration(90 * 1e9), // 90s，等价于 90 * time.Second
        WaitTimeout: timeutil.Duration(100 * 1e6), // 100ms
        Wait:        true,
    }
    p := pool.NewList(cfg)
    p.New = func(ctx context.Context) (io.Closer, error) {
        return &conn{}, nil
    }
    return p
}

func handle(p *pool.List) error {
    ctx := context.Background()
    c, err := p.Get(ctx)
    if err != nil {
        return err
    }
    defer p.Put(ctx, c, false)
    // 使用 c...
    return nil
}
```

`timeutil.Duration` 的零值为 0，等价于 `time.Duration(0)`；配置可直接用 `timeutil.Duration(90 * time.Second)` 表达。

## 实现约定

- `New`/`NewList` 在 `Active < Idle` 或配置为 nil 时 panic，避免静默配置错误。
- `IdleTimeout <= 0` 时禁用空闲清理 goroutine；`Active == 0` 表示不限活跃数。
- `Wait=false` 且未设 `WaitTimeout` 时，活跃耗尽直接返回 `ErrPoolExhausted`。
- `WaitTimeout > 0` 时取 `min(WaitTimeout, ctx剩余时间)` 作为等待上限。
- `Put` 的 `forceClose=true` 时强制关闭对象并释放活跃计数。
- 空闲对象超过 `Idle` 上限时按 LRU 关闭尾部对象。
- `Slice` 用 `itemRequests` map + `openerCh` 实现异步 opener，`List` 用 channel 信号唤醒等待者。
