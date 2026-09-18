# common/dsn

DSN（数据源名称）解析与 struct 绑定工具，把类似 URI 的连接串解析为字段化的配置结构。

## 职责

- `Parse(rawdsn) (*DSN, error)` 把 DSN 字符串解析为 `*DSN`，底层复用 `net/url.URL`。
- `DSN.Bind(v) (url.Values, error)` 把内置字段与 query 参数绑定到 struct，并执行 validator 校验。
- `DSN.Addresses()` 按 `,` 切分 host；unix 系协议返回 `Path` 单地址。
- 绑定后未被消费的 query 参数通过返回的 `url.Values` 透出，便于调用方处理额外参数。

## DSN 格式

```
network:[//[username[:password]@]address[:port][,address[:port]]][/path][?query][#fragment]
```

- `network`：等价于 `net` 包中的 network，如 `tcp`/`udp`/`unix`/`unixgram`/`unixpacket`。
- `address`：支持多个，用 `,` 分割；当 network 为 unix 系时使用 `Path` 字段且仅一个。
- `username`/`password`：来自 `userinfo`。

## 内置绑定键

| 键 | 字段类型 | 说明 |
|---|---|---|
| `network` | `string` | DSN scheme，如 `tcp` |
| `username` | `string` | userinfo 用户名 |
| `password` | `string` | userinfo 密码 |
| `address` | `string` 或 `[]string` | `string` 取首个地址；`[]string` 取全部 |

## query 绑定

- 通过 `dsn:"query.<name>"` 绑定 query 参数。
- 数组通过同一 key 多值传递，例如 `?arr=1&arr=2` 绑定到 `[]int`。
- 嵌套 struct 通过 `query.<prefix>` 前缀展开，子字段以 `<prefix>.<name>` 查找。
- 默认值：`dsn:"query.<name>,<default>"`；切片默认值用 `,` 分割，如 `dsn:"query.tags,a,b,c"`。
- 实现 `encoding.TextUnmarshaler` 的字段可自定义解析逻辑。
- `dsn:"-"` 表示跳过该字段。

## 依赖

- [`github.com/go-playground/validator/v10`](https://github.com/go-playground/validator)：struct 字段校验，通过 `validate` tag 声明规则。
- 标准库：`net/url`、`reflect`、`strconv`、`strings`、`encoding`、`runtime`。

## API

| 符号 | 说明 |
|---|---|
| `func Parse(rawdsn string) (*DSN, error)` | 解析 DSN 字符串 |
| `func (*DSN) Bind(v any) (url.Values, error)` | 绑定到 struct 并执行 validator 校验 |
| `func (*DSN) Addresses() []string` | 返回地址列表，unix 系协议返回 `[Path]` |
| `type DSN struct{ *url.URL }` | 解析结果包装 |
| `type InvalidBindError struct{ Type reflect.Type }` | 非 pointer 或 nil 参数错误 |
| `type BindTypeError struct{ Value, Type }` | 类型不匹配错误 |

## 使用示例

```go
package demo

import (
    "log"

    "go-video/common/dsn"
    "go-video/common/timeutil"
)

type Config struct {
    Network  string           `dsn:"network" validate:"required"`
    Username string           `dsn:"username" validate:"required"`
    Password string           `dsn:"password" validate:"required"`
    Address  []string         `dsn:"address" validate:"required"`
    Timeout  timeutil.Duration `dsn:"query.timeout,1s"`
    Offset   int              `dsn:"query.offset" validate:"gte=0"`
}

func load() {
    cfg := &Config{}
    d, err := dsn.Parse("tcp://root:toor@127.0.0.1:3306,127.0.0.1:3307?timeout=10s&offset=10")
    if err != nil {
        log.Fatal(err)
    }
    if _, err := d.Bind(cfg); err != nil {
        log.Fatal(err)
    }
    _ = cfg
}
```

## 实现约定

- struct 字段必须通过 `dsn` tag 声明绑定键；未声明或 `-` 的字段不被消费。
- 内置键（`network`/`username`/`password`/`address`）优先于 query 绑定。
- `Bind` 返回的 `url.Values` 是 query 中未被内置键或 `query.*` 字段消费的部分，可用于透传上游自定义参数。
- `validator.Validate` 在包 `init()` 阶段构造，使用默认配置；调用方在 struct 上添加 `validate` tag 即可启用校验。
