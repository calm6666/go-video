# common/validation

通用输入校验工具，覆盖分页、字符串长度、枚举、cursor 等 [AGENTS.md §6](../../AGENTS.md) 要求的“限制大小、格式、分页和超时”场景。

## 职责

- 分页参数归一化与边界限制（`page`/`page_size`、`cursor`/`limit`）。
- 字符串长度、字节数、枚举值校验。
- 切片长度、map 大小校验。
- 提供可重用的错误消息前缀，避免业务侧手拼字符串。

## API

| 符号 | 说明 |
|---|---|
| `type Page struct` | `Page int`/`PageSize int`，调用 `Normalize` 后保证在合法范围 |
| `func NormalizePage(page, pageSize, maxPageSize int) Page` | 把 page<=0 归 1，pageSize<=0 或 >max 归 max |
| `func (p Page) Offset() int` | `(page-1) * pageSize` |
| `type Cursor struct` | `Value string`/`Limit int`，cursor 风格分页 |
| `func NormalizeCursor(value string, limit, maxLimit int) Cursor` | 限制 limit 上限，空 cursor 视为首页 |
| `func ValidateStringLength(s string, minLen, maxLen int) error` | 按 rune 计长度 |
| `func ValidateByteLength(s string, minLen, maxLen int) error` | 按字节计长度 |
| `func ValidateEnum(value string, allowed []string) error` | 大小写敏感的枚举校验 |
| `func ValidateSliceLength(s any, minLen, maxLen int) error` | 切片长度范围 |
| `func ValidateMapSize(m map[string]any, minSize, maxSize int) error` | map 大小范围 |
| `func ValidateNonEmptyString(field, value string) error` | 字段非空校验，错误消息含字段名 |
| `func ValidatePositiveInt(field string, value int64) error` | 正整数校验 |
| `func ValidateRange(field string, value, min, max int64) error` | 整数范围校验 |

## 使用示例

```go
import "go-video/common/validation"

page := validation.NormalizePage(req.Page, req.PageSize, 50)
if err := validation.ValidateStringLength(req.Title, 1, 200); err != nil {
    return err
}
if err := validation.ValidateEnum(req.Sort, []string{"newest", "oldest", "hottest"}); err != nil {
    return err
}
```

## 实现约定

- 所有错误都包含具体字段名或值，便于日志排查。
- 不依赖业务错误码体系（`ecode`），返回标准 `error`；业务侧可包装为 `ecode`。
- 仅使用标准库（`fmt`、`reflect`）。
- 不做格式校验（如 email、URL），这些由具体业务规则负责。

## 分页约束

| 场景 | 默认 | 上限 | 说明 |
|---|---|---|---|
| `page` | 1 | — | page <= 0 视为 1 |
| `page_size` | 20 | 50 | 超过 maxPageSize 截断 |
| `cursor.limit` | 20 | 100 | 超过 maxLimit 截断 |

调用方应使用 `NormalizePage` 而非手拼，避免业务侧各自维护阈值。
