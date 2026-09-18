# common/httpresponse

HTTP 统一响应信封与错误处理器，对齐 [AGENTS.md §6](../../AGENTS.md) 的四字段响应信封要求。

## 职责

- 定义统一响应信封 `Envelope`：`code`/`message`/`data`/`ttl` 四字段。
- 提供进程级 `ErrorHandler`，将 go-zero 处理流程中的 error 转换为统一信封。
- 提供常用业务码常量（`CodeOK`/`CodeBadRequest`/`CodeUnauthorized`/...）。
- 通过 `Install` 注册到 go-zero `httpx`，供所有 API 服务复用。

## 依赖

- `github.com/zeromicro/go-zero/rest/httpx`：注册错误处理器。

## API

| 符号 | 说明 |
|---|---|
| `type Envelope struct` | 统一响应信封，`Code`/`Message`/`Data`/`TTL` 四字段 |
| `func ErrorHandler(ctx, err) (int, any)` | 将 error 转换为信封；保留 HTTP 状态码语义 |
| `func Install()` | 注册进程级 go-zero 错误处理器 |
| `const CodeOK = 0` | 成功码 |
| `const CodeBadRequest = 40000` | 请求参数错误 |
| `const CodeUnauthorized = 40100` | 未认证 |
| `const CodeForbidden = 40300` | 无权限 |
| `const CodeNotFound = 40400` | 资源不存在 |
| `const CodeConflict = 40900` | 冲突（如幂等命中 pending） |
| `const CodeInternalError = 50000` | 服务端错误兜底 |

## 响应信封规范

| 场景 | `code` | `message` | `data` | `ttl` |
|---|---|---|---|---|
| 成功 | `0` | `"ok"` | 具体类型，由 .api 生成 | 缓存秒数，默认 `0` |
| 业务错误 | 业务码 | 已注册消息 | `{}` | `0` |
| 无数据成功 | `0` | `"ok"` | `{}` | `0` |

HTTP 状态码仍按协议表达鉴权、限流和服务错误，不能只依赖业务 `code`。

## 使用示例

在服务入口注册一次：

```go
package main

import (
    "go-video/common/httpresponse"
)

func main() {
    httpresponse.Install()
    // 启动 go-zero server...
}
```

handler 保持 goctl 生成原状；logic 层返回 error，由 `ErrorHandler` 统一序列化：

```go
// logic 层
if err != nil {
    return httpresponse.CodeConflict // 或 ecode.Code
}
```

## 与 ecode 的协作

- `httpresponse` 是协议层：四字段信封、HTTP 状态码映射。
- `ecode` 是语义层：业务码到可读消息的注册表、错误链提取。
- handler 不写业务规则；logic 层返回 `ecode.Code`，由 `httpresponse.ErrorHandler` 输出统一信封。

## 实现约定

- 信封四字段名（`code`/`message`/`data`/`ttl`）一旦发布即冻结，变更需全客户端协同。
- 业务码常量区间避开各服务自定义码段；服务自定义码通过 `ecode.New` 申请。
- 不在 handler 中手拼 JSON；成功响应类型由 `.api` 生成。
- HTTP 状态码：成功 `200`；客户端取消 `499`；超时 `504`；其他错误默认 `400`。
