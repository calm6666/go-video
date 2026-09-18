# common/pipeline

合并写入管道，按 Split 分片聚批，达到 `MaxSize` 或 `Interval` 超时后批量回调处理。适用于批量写库、批量上报、批量刷缓存等场景。

## 职责

- `NewPipeline(config)` 创建一组分片 channel 与 worker。
- `Add(ctx, key, value)` 异步投递，channel 满返回 `ErrFull`。
- `SyncAdd(ctx, key, value)` 同步阻塞投递。
- `Start` 启动所有 worker；worker 按 `Split` 分片聚批，达到 `MaxSize` 或 `Interval` 超时后调用 `Do` 回调批量处理。
- `Close` 向所有 channel 发送结束信号并等待 worker 退出。
- `Smooth` 模式错开各 worker 的首次 ticker，避免批量回调同时打满下游。

## 依赖

- 标准库（`context`、`errors`、`sync`、`time`）。
- [common/timeutil](../timeutil)：`Duration` 类型用于 `Interval` 配置。

## API

| 符号 | 说明 |
|---|---|
| `var ErrFull = errors.New("channel full")` | channel 满时返回的哨兵错误 |
| `type Config struct` | 配置：`MaxSize`/`Interval`/`Buffer`/`Worker`/`Smooth` |
| `type Pipeline struct` | 合并管道，需设置 `Do` 与 `Split` 字段 |
| `func NewPipeline(config *Config) *Pipeline` | 创建管道并初始化分片 channel |
| `func (p *Pipeline) Start()` | 启动所有 worker；`Do`/`Split` 为 nil 时 panic |
| `func (p *Pipeline) Add(ctx, key, value) error` | 异步投递，满返回 `ErrFull` |
| `func (p *Pipeline) SyncAdd(ctx, key, value)` | 同步阻塞投递 |
| `func (p *Pipeline) Close() error` | 关闭并等待所有 worker 退出 |

### Pipeline 字段

| 字段 | 说明 |
|---|---|
| `Do func(ctx, index, values map[string][]interface{})` | 批量处理回调，`index` 为分片号 |
| `Split func(key string) int` | 分片函数，决定 key 落入哪个 worker |

### Config 字段

| 字段 | 默认值 | 说明 |
|---|---|---|
| `MaxSize` | 1000 | 单批最大条数，达到即触发 `Do` |
| `Interval` | 1s | 聚批超时，超时即触发 `Do` |
| `Buffer` | 1000 | 单个分片 channel 容量 |
| `Worker` | 10 | 分片（worker）数量 |
| `Smooth` | false | 是否错开各 worker 首次 ticker |

## 使用示例

```go
package demo

import (
    "context"
    "strconv"

    "go-video/common/pipeline"
    "go-video/common/timeutil"
)

func newBatcher() *pipeline.Pipeline {
    cfg := &pipeline.Config{
        MaxSize:  100,
        Interval: timeutil.Duration(500 * 1e6), // 500ms
        Buffer:   1000,
        Worker:   8,
        Smooth:   true,
    }
    p := pipeline.NewPipeline(cfg)
    p.Do = func(ctx context.Context, index int, values map[string][]interface{}) {
        // 批量写库或上报...
        // values: key -> []value
    }
    p.Split = func(key string) int {
        n, _ := strconv.Atoi(key)
        return n
    }
    p.Start()
    return p
}

func write(p *pipeline.Pipeline, key string, val any) {
    _ = p.Add(context.Background(), key, val)
}

func shutdown(p *pipeline.Pipeline) {
    _ = p.Close()
}
```

## 实现约定

- `Add` 通过 `Split(key) % Worker` 选择分片 channel；满则返回 `ErrFull`，不阻塞。
- `SyncAdd` 直接向分片 channel 阻塞写入，channel 满时阻塞直到 worker 消费。
- `Close` 向每个 channel 发送 `nil` 哨兵，worker 收到后处理剩余聚批并退出。
- `Smooth=true` 时，第 `i` 个 worker 的首个 ticker 间隔为 `i * (Interval/Worker)`，触发一次后切换为标准 `Interval`，从而错开各 worker 的回调时机。
- `Do` 回调收到的 `values` 为单次聚批结果，按 key 聚合为 `[]value`。
- `Do` 与 `Split` 必须在 `Start` 前设置，否则 `Start` panic。
