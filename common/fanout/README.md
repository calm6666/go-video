# common/fanout

异步任务执行器，用固定数量的 worker 消费带缓冲 channel 中的任务，用于解耦主链路与旁路处理（如缓存写入、日志上报、指标记录）。

## 职责

- `New(name, opts...)` 创建带 worker 和 buffer 的 fanout，启动后台 worker 协程。
- `Do(ctx, f)` 异步投递任务到 channel，满时返回 `ErrFull`，避免 OOM。
- `Close()` 取消内部 Context 并等待所有 worker 退出，优雅关闭。
- worker 执行任务时做 panic recover，panic 通过 `logx.Error` 记录，不崩溃。

## 依赖

- 标准库（`context`、`errors`、`runtime`、`sync`）。
- [`github.com/zeromicro/go-zero/core/logx`](https://github.com/zeromicro/go-zero)：panic 恢复后的错误日志输出。

## API

| 符号 | 说明 |
|---|---|
| `var ErrFull = errors.New("fanout: chan full")` | channel 满时返回的哨兵错误 |
| `type Option func(*options)` | 配置选项函数 |
| `func Worker(n int) Option` | 设置 worker 数，必须 > 0 |
| `func Buffer(n int) Option` | 设置 channel 缓冲长度，必须 > 0 |
| `type Fanout struct` | 异步任务执行器 |
| `func New(name string, opts ...Option) *Fanout` | 创建 fanout 并启动 worker；name 为空时回退为 `"fanout"` |
| `func (c *Fanout) Do(ctx context.Context, f func(ctx context.Context)) error` | 异步投递任务；已关闭或满返回错误 |
| `func (c *Fanout) Close() error` | 关闭 fanout，等待 worker 退出 |

## 使用示例

```go
package demo

import (
    "context"

    "go-video/common/fanout"
)

func addCache(ctx context.Context, id, value int) {
    // 写缓存...
}

var cache = fanout.New("cache", fanout.Worker(1), fanout.Buffer(1024))

func Handle(id, value int) {
    // 异步执行；传入的 ctx 仅用于任务闭包，超时不会被继承到 worker。
    _ = cache.Do(context.Background(), func(ctx context.Context) {
        addCache(ctx, id, value)
    })
}

func Shutdown() {
    // 程序退出时关闭，等待后台 worker 完成已投递任务。
    _ = cache.Close()
}
```

## 实现约定

- 默认 `Worker=1`、`Buffer=1024`。
- `Do` 在内部 Context 已取消（Close 已调用）时直接返回该 Context 错误。
- worker 从 channel 取任务执行；`Close` 通过取消内部 Context 通知 worker 退出当前阻塞的 select，`WaitGroup` 等待所有 worker 完成。
- panic 恢复后捕获 64KB 栈，通过 `logx.Error` 记录错误信息和栈，任务不重新投递。
- 传入 `Do` 的 Context 直接透传给任务函数，不在 worker 侧派生新 Context，调用方负责其生命周期。
