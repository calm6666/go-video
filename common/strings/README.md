# common/strings

通用字符串切片工具，零依赖。仅提供与标准库 `strconv`/`strings` 互补的纯函数工具，不包含业务语义。

## 职责

- 整型切片与逗号分隔字符串的互转。
- 字符串切片的去重、包含判断、按 rune 截断等通用操作。

## API

| 函数 | 说明 |
|---|---|
| `JoinInts(is []int64) string` | 将 `[]int64` 用逗号拼接为 `n1,n2,n3`，空切片返回空串 |
| `SplitInts(s string) ([]int64, error)` | 将 `n1,n2,n3` 解析为 `[]int64`；空串返回 `nil, nil`；非法字符返回错误 |
| `JoinStrings(ss []string, sep string) string` | 通用字符串切片拼接，跳过空元素 |
| `DedupStrings(ss []string) []string` | 保持顺序去重 |
| `ContainsString(ss []string, target string) bool` | 线性查找，适用于小切片 |
| `Truncate(s string, max int) string` | 按 rune 截断，避免中文截半 |

## 使用示例

```go
import "go-video/common/strings"

ids, _ := strings.SplitInts("1,2,3")  // []int64{1,2,3}
joined := strings.JoinInts(ids)       // "1,2,3"
```

## 实现约定

- 全部函数为纯函数，不读取全局状态、不触发 IO。
- 输入切片为 `nil` 时返回零值，不 panic。
- 仅使用标准库（`strconv`、`strings`）。
