# common/errgroup

带并发限制与 panic 恢复的 errgroup，用于一组子任务并行执行时的同步、错误传播与 Context 取消。

## 职责

- `Group` 聚合并行子任务，`Wait` 等待全部完成后返回首个非 nil 错误。
- `GOMAXPROCS(n)` 限制同时运行的 worker 数量，避免瞬时 goroutine 雪崩。
- 每个 worker 执行任务时做 panic recover，将 panic 包装为 error 返回，避免整个进程崩溃。
- `WithContext` 派生 Context，首个错误或 `Wait` 返回时取消该 Context，便于下游任务提前退出。

## 依赖

- 仅使用标准库（`context`、`fmt`、`runtime`、`sync`）。

## API

| 符号 | 说明 |
|---|---|
| `type Group struct` | 零值可用，零值 Group 不在出错时取消 |
| `func WithContext(ctx context.Context) (*Group, context.Context)` | 派生 Context，首个错误或 Wait 返回时取消 |
| `func (g *Group) GOMAXPROCS(n int)` | 限制 worker 数，n 必须 > 0；只能在 Go 之前调用一次 |
| `func (g *Group) Go(f func() error)` | 提交一个子任务；未启用 GOMAXPROCS 时直接新起 goroutine |
| `func (g *Group) Wait() error` | 等待全部子任务结束，返回首个非 nil 错误 |

## 使用示例

```go
package demo

import (
    "context"
    "fmt"

    "go-video/common/errgroup"
)

func search(ctx context.Context, kind string) (string, error) {
    return kind + " result", nil
}

func Parallel(ctx context.Context, query string) ([]string, error) {
    g, ctx := errgroup.WithContext(ctx)
    kinds := []string{"web", "image", "video"}
    results := make([]string, len(kinds))
    for i, kind := range kinds {
        i, kind := i, kind
        g.Go(func() error {
            r, err := search(ctx, kind)
            if err == nil {
                results[i] = r
            }
            return err
        })
    }
    if err := g.Wait(); err != nil {
        return nil, err
    }
    return results, nil
}

func main() {
    res, err := Parallel(context.Background(), "golang")
    fmt.Println(res, err)
}
```

## 实现约定

- `GOMAXPROCS` 通过 `workerOnce` 保证只生效一次；重复调用会被忽略。
- 启用 `GOMAXPROCS` 后，`Go` 优先将任务投递到 worker channel，投递失败时暂存到本地切片，由 `Wait` 在返回前补投，确保任务不丢失。
- panic recover 捕获 64KB 栈并包装为 `errgroup: panic recovered: <r>\n<stack>` 形式的 error。
- `Wait` 在所有任务完成后关闭 worker channel，并触发 Context 取消。
- 错误只保留首个非 nil 错误，通过 `errOnce` 保证只赋值一次。
