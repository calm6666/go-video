# 终端面 · `/x/passport-login`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 登录路由（参考仓库 passport-login 接口层 /x/passport-login） | 免鉴权 | 9 |
| passport 增量：密码与登录记录 | 免鉴权 | 4 |

合计 **13** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 登录路由（参考仓库 passport-login 接口层 /x/passport-login）（免鉴权，9 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/x/passport-login/key` | 获取密码加密 RSA 公钥 | `loginKey` | `loginkeylogic.go` |
| POST | `/x/passport-login/web/login` | 登录（密码/验证码，login_type 区分） | `login` | `loginlogic.go` |
| POST | `/x/passport-login/web/captcha/send` | 发送登录验证码 | `loginCaptureSend` | `logincapturesendlogic.go` |
| POST | `/x/passport-login/web/captcha/check` | 校验登录验证码 | `loginCaptureCheck` | `logincapturechecklogic.go` |
| POST | `/x/passport-login/web/register` | 注册（用户名+密码 或 手机/邮箱+验证码+密码） | `register` | `registerlogic.go` |
| POST | `/x/passport-login/exit` | 登出 | `logout` | `logoutlogic.go` |
| POST | `/x/passport-login/token/renew` | 刷新 token | `renewToken` | `renewtokenlogic.go` |
| GET | `/x/passport-login/web/cookie/info` | 查询 cookie 会话登录态 | `cookieInfo` | `cookieinfologic.go` |
| GET | `/x/passport-login/web/token/info` | 查询 token 登录态 | `tokenInfo` | `tokeninfologic.go` |

### GET `/x/passport-login/key` — 获取密码加密 RSA 公钥

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/loginkeyhandler.go`
- 业务实现：`gateway/app/internal/logic/loginkeylogic.go`

请求：无参数体。

响应：`KeyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `KeyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/passport-login/web/login` — 登录（密码/验证码，login_type 区分）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/loginhandler.go`
- 业务实现：`gateway/app/internal/logic/loginlogic.go`

请求：`ParamLogin`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Account` | `account` | form | `string` | 是 | — | — |
| `Password` | `password` | form | `string` | 是 | — | — |
| `LoginType` | `login_type` | form | `int32` | 是 | default=1 | — |
| `CaptureCode` | `capture_code` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |
| `Device` | `device` | form | `string` | 是 | — | — |
| `Buvid` | `buvid` | form | `string` | 是 | — | — |

响应：`LoginResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LoginData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/passport-login/web/captcha/send` — 发送登录验证码

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/logincapturesendhandler.go`
- 业务实现：`gateway/app/internal/logic/logincapturesendlogic.go`

请求：`ParamCaptureSend`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Target` | `target` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/passport-login/web/captcha/check` — 校验登录验证码

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/logincapturecheckhandler.go`
- 业务实现：`gateway/app/internal/logic/logincapturechecklogic.go`

请求：`ParamCaptureVerify`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Target` | `target` | form | `string` | 是 | — | — |
| `CaptureCode` | `capture_code` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/passport-login/web/register` — 注册（用户名+密码 或 手机/邮箱+验证码+密码）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/registerhandler.go`
- 业务实现：`gateway/app/internal/logic/registerlogic.go`

请求：`ParamRegister`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Account` | `account` | form | `string` | 是 | — | — |
| `Password` | `password` | form | `string` | 是 | — | — |
| `CaptureCode` | `capture_code` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`LoginResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LoginData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/passport-login/exit` — 登出

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/logouthandler.go`
- 业务实现：`gateway/app/internal/logic/logoutlogic.go`

请求：`ParamToken`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Token` | `token` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/passport-login/token/renew` — 刷新 token

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/renewtokenhandler.go`
- 业务实现：`gateway/app/internal/logic/renewtokenlogic.go`

请求：`ParamRenew`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RefreshToken` | `refresh_token` | form | `string` | 是 | — | — |

响应：`RenewResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RenewData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/passport-login/web/cookie/info` — 查询 cookie 会话登录态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/cookieinfohandler.go`
- 业务实现：`gateway/app/internal/logic/cookieinfologic.go`

请求：`ParamCookie`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Cookie` | `cookie` | form | `string` | 是 | — | — |

响应：`SessionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SessionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/passport-login/web/token/info` — 查询 token 登录态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/tokeninfohandler.go`
- 业务实现：`gateway/app/internal/logic/tokeninfologic.go`

请求：`ParamToken`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Token` | `token` | form | `string` | 是 | — | — |

响应：`SessionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SessionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## passport 增量：密码与登录记录（免鉴权，4 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/x/passport-login/password/set` | 设置/修改密码（RSA 密文传输） | `setPassword` | `setpasswordlogic.go` |
| POST | `/x/passport-login/password/reset` | 验证码校验后找回密码 | `resetPassword` | `resetpasswordlogic.go` |
| GET | `/x/passport-login/password/history/check` | 历史密码重复校验 | `checkHistoryPassword` | `checkhistorypasswordlogic.go` |
| GET | `/x/passport-login/login/logs` | 本人登录记录 | `loginLogs` | `loginlogslogic.go` |

### POST `/x/passport-login/password/set` — 设置/修改密码（RSA 密文传输）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/setpasswordhandler.go`
- 业务实现：`gateway/app/internal/logic/setpasswordlogic.go`

请求：`ParamSetPassword`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OldPassword` | `old_password` | form | `string` | 否 | — | 首次设置可为空 |
| `NewPassword` | `new_password` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/passport-login/password/reset` — 验证码校验后找回密码

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/resetpasswordhandler.go`
- 业务实现：`gateway/app/internal/logic/resetpasswordlogic.go`

请求：`ParamResetPassword`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Account` | `account` | form | `string` | 是 | — | — |
| `CaptureCode` | `capture_code` | form | `string` | 是 | — | — |
| `NewPassword` | `new_password` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/passport-login/password/history/check` — 历史密码重复校验

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/checkhistorypasswordhandler.go`
- 业务实现：`gateway/app/internal/logic/checkhistorypasswordlogic.go`

请求：`ParamCheckHistoryPassword`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Password` | `password` | form | `string` | 是 | — | 可逗号分隔多个 |

响应：`PassportCheckHistoryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PassportCheckHistoryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/passport-login/login/logs` — 本人登录记录

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/loginlogshandler.go`
- 业务实现：`gateway/app/internal/logic/loginlogslogic.go`

请求：`ParamLoginLogs`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Limit` | `limit` | form | `int32` | 是 | default=20 | — |

响应：`PassportLoginLogsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PassportLoginLogsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `KeyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `KeyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLogin`

> 登录请求参数（密码为 RSA 公钥加密后的 base64，登录方式：1 密码、2 验证码）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Account` | `account` | form | `string` | 是 | — | — |
| `Password` | `password` | form | `string` | 是 | — | — |
| `LoginType` | `login_type` | form | `int32` | 是 | default=1 | — |
| `CaptureCode` | `capture_code` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |
| `Device` | `device` | form | `string` | 是 | — | — |
| `Buvid` | `buvid` | form | `string` | 是 | — | — |

### `LoginResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LoginData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCaptureSend`

> 验证码发送/校验请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Target` | `target` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCaptureVerify`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Target` | `target` | form | `string` | 是 | — | — |
| `CaptureCode` | `capture_code` | form | `string` | 是 | — | — |

### `ParamRegister`

> 注册请求参数（手机/邮箱注册带验证码，用户名注册可无）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Account` | `account` | form | `string` | 是 | — | — |
| `Password` | `password` | form | `string` | 是 | — | — |
| `CaptureCode` | `capture_code` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamToken`

> 登出/校验请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Token` | `token` | form | `string` | 是 | — | — |

### `ParamRenew`

> 刷新 token 请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RefreshToken` | `refresh_token` | form | `string` | 是 | — | — |

### `RenewResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RenewData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCookie`

> cookie 会话校验请求参数（客户端回传 Cookie 原文）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Cookie` | `cookie` | form | `string` | 是 | — | — |

### `SessionResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SessionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSetPassword`

> 密码一律由客户端用 /x/passport-login/key 的 RSA 公钥加密后传输， / 网关不落明文（AGENTS.md §6）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `OldPassword` | `old_password` | form | `string` | 否 | — | 首次设置可为空 |
| `NewPassword` | `new_password` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamResetPassword`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Account` | `account` | form | `string` | 是 | — | — |
| `CaptureCode` | `capture_code` | form | `string` | 是 | — | — |
| `NewPassword` | `new_password` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamCheckHistoryPassword`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Password` | `password` | form | `string` | 是 | — | 可逗号分隔多个 |

### `PassportCheckHistoryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PassportCheckHistoryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLoginLogs`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Limit` | `limit` | form | `int32` | 是 | default=20 | — |

### `PassportLoginLogsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PassportLoginLogsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `KeyData`

> RSA 公钥响应（参考 passport-login /key）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PublicKey` | `public_key` | json | `string` | 是 | — | — |
| `Hash` | `hash` | json | `string` | 是 | — | — |

### `LoginData`

> 登录态数据（token/refresh/csrf）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Token` | `token` | json | `string` | 是 | — | — |
| `RefreshToken` | `refresh_token` | json | `string` | 是 | — | — |
| `Csrf` | `csrf` | json | `string` | 是 | — | — |
| `Expires` | `expires` | json | `int64` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `RenewData`

> 刷新 token 响应数据

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Token` | `token` | json | `string` | 是 | — | — |
| `Csrf` | `csrf` | json | `string` | 是 | — | — |
| `Expires` | `expires` | json | `int64` | 是 | — | — |

### `SessionData`

> 会话校验数据（is_login/mid/csrf/expires）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `IsLogin` | `is_login` | json | `bool` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Csrf` | `csrf` | json | `string` | 是 | — | — |
| `Expires` | `expires` | json | `int64` | 是 | — | — |

### `PassportCheckHistoryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Result` | `result` | json | `string` | 是 | — | — |

### `PassportLoginLogsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Logs` | `logs` | json | `[]PassportLoginLog` | 是 | — | — |

### `PassportLoginLog`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `IP` | `ip` | json | `string` | 是 | — | — |
| `Ts` | `ts` | json | `int64` | 是 | — | — |
| `LoginType` | `login_type` | json | `int32` | 是 | — | — |
| `Status` | `status` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Device` | `device` | json | `string` | 是 | — | — |


<!-- file: docs/api/http/app/07-x-passport-login.md -->
