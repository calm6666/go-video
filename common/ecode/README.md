# common/ecode

业务错误码注册表，与 [common/httpresponse](../httpresponse) 联动输出四字段响应信封。

## 职责

- 全局唯一业务错误码注册：`Register(map[int]string)`、`New(int) Code`。
- `Code` 类型实现 `error`，提供 `Code()`/`Message()`，可在 logic 层被返回并由 `httpresponse.ErrorHandler` 转换。
- `Cause(error) Code` 从 `error` 链中提取首个 `*ecode.Error`。
- `EqualError(code, err) bool` 用于判断错误是否对应某个码。

## API

| 符号 | 说明 |
|---|---|
| `var OK = Code(0)` | 成功码 |
| `var ServerErr = Code(50000)` | 服务器错误兜底码（与 [httpresponse.CodeInternalError](../httpresponse/response.go) 对齐） |
| `func Register(m map[int]string)` | 注册错误码到消息映射；重复调用覆盖前一次注册 |
| `func New(e int) Code` | 申请唯一业务码（必须 > 0），重复注册 panic |
| `func Int(i int) Code` | 直接构造码，跳过唯一性检查 |
| `func String(s string) Code` | 从错误字符串解析码 |
| `func Cause(err error) Code` | 从 error 链提取 Code |
| `func Equal(a, b Code) bool` | 码值相等比较 |
| `func EqualError(code Code, err error) bool` | 等价于 `Equal(code, Cause(err))` |
| `type Code int` | 实现 `error`/`Code()`/`Message()` |
| `type Error struct` | 携带码与可选详情的错误类型，`Unwrap()` 支持链式 Cause |

## 使用示例

```go
package demo

import (
    "errors"
    "go-video/common/ecode"
)

var (
    // 在服务初始化时调用一次。
    _ = ecode.Register(map[int]string{
        10001: "user not found",
        10002: "password mismatch",
    })
    UserNotFound = ecode.New(10001)
)

func login(account, password string) error {
    if account == "" {
        return UserNotFound // *ecode.Code 实现 error
    }
    return errors.New("other failure")
}
```

在 `svc.ServiceContext` 启动时调用 `ecode.Register`，由 logic 层返回 `Code`，再由
`httpresponse.ErrorHandler` 输出统一信封。

## 与 httpresponse 的协作

`httpresponse` 已有数字常量（`CodeOK`/`CodeBadRequest`/...）和默认错误码到 HTTP 状态的映射。`ecode` 提供的是**带消息的业务码注册体系**，二者关系：

- `httpresponse` 是协议层（四字段信封、HTTP 状态码）。
- `ecode` 是语义层（业务码到可读消息、错误链提取）。
- handler 应保持原状（goctl 生成）；logic 层返回 `ecode.Code`，由公共 `httpresponse.ErrorHandler` 统一序列化。

## 实现约定

- `Code` 是值类型 `int`，可比较、可做 map key。
- `New` 在重复注册时 panic，避免运行时静默吞码。
- 仅使用标准库（`errors`、`fmt`、`strconv`、`sync`）。
