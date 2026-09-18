# common/flagvar

自定义 `flag.Value` 实现，把命令行参数解析为切片等结构化类型。

## 职责

- 提供 `StringVars` 类型实现 `flag.Value` 接口（`String()`/`Set(val)`），支持把多次出现的命令行 flag 累积为 `[]string`。
- 与标准库 `flag` 包无缝衔接：直接传给 `flag.Var` 即可注册。

## 依赖

- 标准库：`flag`、`strings`。

## API

| 符号 | 说明 |
|---|---|
| `type StringVars []string` | 实现 `flag.Value`，多次 `-flag value` 累积为切片 |
| `func (s StringVars) String() string` | 返回 `,` 分隔的扁平字符串，用于 flag 帮助与默认值显示 |
| `func (s *StringVars) Set(val string) error` | 追加单个值到切片，永不返回错误 |

## 使用示例

```go
package main

import (
    "flag"
    "fmt"

    "go-video/common/flagvar"
)

func main() {
    var groups flagvar.StringVars
    flag.Var(&groups, "group", "group names, repeatable")
    flag.Parse()
    fmt.Println(groups) // [g1 g2 g3] when -group g1 -group g2 -group g3
}
```

## 实现约定

- `Set` 永远追加，不去重；多次同名 flag 会按命令行出现顺序入列。
- `String` 用 `,` 连接，与 `flag` 包默认值显示约定一致；空切片返回空字符串。
- 接收者类型：`String()` 使用值接收者便于 `flag.Var` 直接传 `*StringVars`；`Set` 使用指针接收者以修改底层数组。
