# common/timeutil

时间与 Duration 工具，提供 MySQL timestamp 扫描和上下文超时截断能力。

## 职责

- `Time`：MySQL `DATETIME`/`TIMESTAMP` 与 Unix 秒的双向转换，实现 `sql.Scanner` 和 `driver.Valuer`。
- `Duration`：YAML/TOML 字符串解析的 duration（如 `"500ms"`、`"1s"`），并支持按上下文 deadline 截断。
- `Now`/`NowMilli`：可被测试替换的当前时间获取器。
- `ParseRFC3339`/`FormatRFC3339`：事件时间戳统一格式。

## API

| 类型/函数 | 说明 |
|---|---|
| `type Time int64` | Unix 秒时间，配合 `database/sql` 使用 |
| `func (t *Time) Scan(src any) error` | 支持 `time.Time`、`string`、`[]byte`、`int64` 输入 |
| `func (t Time) Value() (driver.Value, error)` | 输出 `time.Time` |
| `func (t Time) Time() time.Time` | 转 `time.Time` |
| `type Duration time.Duration` | 可从字符串反序列化 |
| `func (d *Duration) UnmarshalText(text []byte) error` | 解析 `"1s"`/`"500ms"` |
| `func (d Duration) Shrink(ctx) (Duration, context.Context, context.CancelFunc)` | 取 `min(d, ctx剩余时间)`，避免子调用超时父调用 |
| `func Now() time.Time` / `func NowMilli() int64` | 当前时间，可被 `SetNowFunc` 替换 |
| `func SetNowFunc(f func() time.Time)` | 测试钩子；传入 `nil` 还原默认 |
| `func ParseRFC3339(s string) (time.Time, error)` | 严格 RFC3339 解析 |
| `func FormatRFC3339(t time.Time) string` | UTC RFC3339 输出 |

## 使用示例

```go
import (
    "context"
    "time"
    "go-video/common/timeutil"
)

var d timeutil.Duration
_ = d.UnmarshalText([]byte("500ms"))

ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
defer cancel()
shrink, sctx, scancel := d.Shrink(ctx)
defer scancel()
// shrink <= 100ms
_ = shrink
```

## 实现约定

- 默认时区为 UTC，业务侧需要本地时区时显式转换。
- `SetNowFunc` 仅供测试使用，生产路径禁止调用。
- 仅使用标准库（`context`、`database/sql/driver`、`time`）。
