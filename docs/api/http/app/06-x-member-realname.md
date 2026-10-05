# 终端面 · `/x/member/realname`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| user-profile 客户端路由（参考仓库 account-interface /x/member/*） | 免鉴权 | 8 |

合计 **8** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## user-profile 客户端路由（参考仓库 account-interface /x/member/*）（免鉴权，8 条）

> 实名认证路由（参考仓库 /x/member/realname/*）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/x/member/realname/status` | 查询实名认证状态 | `realnameStatus` | `realnamestatuslogic.go` |
| GET | `/x/member/realname/info` | 查询实名简要信息 | `realnameInfo` | `realnameinfologic.go` |
| GET | `/x/member/realname/apply/status` | 查询实名申请流程状态 | `realnameApplyStatus` | `realnameapplystatuslogic.go` |
| POST | `/x/member/realname/tel/capture` | 发送实名手机验证码 | `realnameTelCapture` | `realnametelcapturelogic.go` |
| GET | `/x/member/realname/tel/capture/check` | 校验实名手机验证码 | `realnameCaptureCheck` | `realnamecapturechecklogic.go` |
| POST | `/x/member/realname/apply` | 提交实名认证申请 | `realnameApply` | `realnameapplylogic.go` |
| GET | `/x/member/realname/adult` | 查询实名成年状态 | `realnameAdult` | `realnameadultlogic.go` |
| GET | `/x/member/realname/check` | 校验实名证件号 | `realnameCheck` | `realnamechecklogic.go` |

### GET `/x/member/realname/status` — 查询实名认证状态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/realnamestatushandler.go`
- 业务实现：`gateway/app/internal/logic/realnamestatuslogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`RealnameStatusResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameStatusData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/realname/info` — 查询实名简要信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/realnameinfohandler.go`
- 业务实现：`gateway/app/internal/logic/realnameinfologic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`RealnameBriefResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameBriefData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/realname/apply/status` — 查询实名申请流程状态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/realnameapplystatushandler.go`
- 业务实现：`gateway/app/internal/logic/realnameapplystatuslogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`RealnameApplyStatusResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameApplyStatusData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/realname/tel/capture` — 发送实名手机验证码

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/realnametelcapturehandler.go`
- 业务实现：`gateway/app/internal/logic/realnametelcapturelogic.go`

请求：`ParamMid`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/realname/tel/capture/check` — 校验实名手机验证码

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/realnamecapturecheckhandler.go`
- 业务实现：`gateway/app/internal/logic/realnamecapturechecklogic.go`

请求：`ParamCaptureCheck`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Capture` | `capture` | form | `int` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/realname/apply` — 提交实名认证申请

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/realnameapplyhandler.go`
- 业务实现：`gateway/app/internal/logic/realnameapplylogic.go`

请求：`ParamRealnameApply`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Capture` | `capture` | form | `int64` | 是 | — | — |
| `RealName` | `real_name` | form | `string` | 是 | — | — |
| `CardType` | `card_type` | form | `int64` | 是 | — | — |
| `CardNum` | `card_num` | form | `string` | 是 | — | — |
| `Country` | `country` | form | `int64` | 是 | — | — |
| `IMG1Token` | `img1_token` | form | `string` | 是 | — | — |
| `IMG2Token` | `img2_token` | form | `string` | 是 | — | — |
| `IMG3Token` | `img3_token` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/realname/adult` — 查询实名成年状态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/realnameadulthandler.go`
- 业务实现：`gateway/app/internal/logic/realnameadultlogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`RealnameAdultResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameAdultData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/realname/check` — 校验实名证件号

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/realnamecheckhandler.go`
- 业务实现：`gateway/app/internal/logic/realnamechecklogic.go`

请求：`ParamRealnameCheck`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `CardType` | `card_type` | form | `int8` | 是 | — | — |
| `CardCode` | `card_code` | form | `string` | 是 | — | — |

响应：`RealnameCheckResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameCheckData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamMid`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `RealnameStatusResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameStatusData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `RealnameBriefResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameBriefData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `RealnameApplyStatusResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameApplyStatusData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCaptureCheck`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Capture` | `capture` | form | `int` | 是 | — | — |

### `ParamRealnameApply`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Capture` | `capture` | form | `int64` | 是 | — | — |
| `RealName` | `real_name` | form | `string` | 是 | — | — |
| `CardType` | `card_type` | form | `int64` | 是 | — | — |
| `CardNum` | `card_num` | form | `string` | 是 | — | — |
| `Country` | `country` | form | `int64` | 是 | — | — |
| `IMG1Token` | `img1_token` | form | `string` | 是 | — | — |
| `IMG2Token` | `img2_token` | form | `string` | 是 | — | — |
| `IMG3Token` | `img3_token` | form | `string` | 是 | — | — |

### `RealnameAdultResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameAdultData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRealnameCheck`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `CardType` | `card_type` | form | `int8` | 是 | — | — |
| `CardCode` | `card_code` | form | `string` | 是 | — | — |

### `RealnameCheckResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameCheckData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `RealnameStatusData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Status` | `status` | json | `int8` | 是 | — | — |

### `RealnameBriefData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RealnameBrief` | `realname_brief` | json | `RealnameBrief` | 是 | — | — |

### `RealnameApplyStatusData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RealnameApplyStatus` | `realname_apply_status` | json | `RealnameApplyStatus` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `RealnameAdultData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Type` | `type` | json | `int8` | 是 | — | — |

### `RealnameCheckData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Result` | `result` | json | `bool` | 是 | — | — |

### `RealnameBrief`

> 实名简要信息（对应 user-profile RealnameBrief）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Realname` | `realname` | json | `string` | 是 | — | — |
| `Card` | `card` | json | `string` | 是 | — | — |
| `CardType` | `card_type` | json | `int` | 是 | — | — |
| `Status` | `status` | json | `int8` | 是 | — | — |

### `RealnameApplyStatus`

> 实名申请流程状态（参考 RealnameApplyStatusInfo）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Status` | `status` | json | `int8` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 是 | — | — |
| `Realname` | `realname` | json | `string` | 是 | — | — |
| `Card` | `card` | json | `string` | 是 | — | — |


<!-- file: docs/api/http/app/06-x-member-realname.md -->
