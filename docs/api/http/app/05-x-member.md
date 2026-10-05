# 终端面 · `/x/member`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| user-profile 客户端路由（参考仓库 account-interface /x/member/*） | 免鉴权 | 10 |
| user-profile 客户端路由（参考仓库 account-interface /x/member/*） | 免鉴权 | 6 |

合计 **16** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## user-profile 客户端路由（参考仓库 account-interface /x/member/*）（免鉴权，10 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/x/member/web/account` | 查询我的全量资料（基础+等级+官方认证） | `memberAccount` | `memberaccountlogic.go` |
| GET | `/x/member/base` | 查询单个用户基础资料 | `memberBase` | `memberbaselogic.go` |
| GET | `/x/member/batchBase` | 批量查询用户基础资料 | `memberBatchBase` | `memberbatchbaselogic.go` |
| GET | `/x/member/moral` | 查询节操值 | `memberMoral` | `membermorallogic.go` |
| GET | `/x/member/web/moral/log` | 查询节操值变更日志 | `moralLog` | `moralloglogic.go` |
| GET | `/x/member/exp` | 查询经验等级信息（含当前经验） | `memberExp` | `memberexplogic.go` |
| GET | `/x/member/level` | 查询等级信息（不含当前经验） | `memberLevel` | `memberlevellogic.go` |
| GET | `/x/member/official` | 查询生效官方认证信息 | `memberOfficial` | `memberofficiallogic.go` |
| GET | `/x/member/web/exp/log` | 查询经验变更日志 | `expLog` | `exploglogic.go` |
| GET | `/x/member/web/exp/reward` | 查询当日经验奖励统计 | `expReward` | `exprewardlogic.go` |

### GET `/x/member/web/account` — 查询我的全量资料（基础+等级+官方认证）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/memberaccounthandler.go`
- 业务实现：`gateway/app/internal/logic/memberaccountlogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`MemberFullResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MemberFullData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/base` — 查询单个用户基础资料

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/memberbasehandler.go`
- 业务实现：`gateway/app/internal/logic/memberbaselogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`MemberBaseResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MemberBaseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/batchBase` — 批量查询用户基础资料

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/memberbatchbasehandler.go`
- 业务实现：`gateway/app/internal/logic/memberbatchbaselogic.go`

请求：`ParamMids`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

响应：`BatchBaseResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `BatchBaseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/moral` — 查询节操值

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/membermoralhandler.go`
- 业务实现：`gateway/app/internal/logic/membermorallogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`MoralResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MoralData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/web/moral/log` — 查询节操值变更日志

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/moralloghandler.go`
- 业务实现：`gateway/app/internal/logic/moralloglogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`MoralLogResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MoralLogData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/exp` — 查询经验等级信息（含当前经验）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/memberexphandler.go`
- 业务实现：`gateway/app/internal/logic/memberexplogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`LevelResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LevelData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/level` — 查询等级信息（不含当前经验）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/memberlevelhandler.go`
- 业务实现：`gateway/app/internal/logic/memberlevellogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`LevelResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LevelData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/official` — 查询生效官方认证信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/memberofficialhandler.go`
- 业务实现：`gateway/app/internal/logic/memberofficiallogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`OfficialInfoResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OfficialInfoData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/web/exp/log` — 查询经验变更日志

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/exploghandler.go`
- 业务实现：`gateway/app/internal/logic/exploglogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`MoralLogResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MoralLogData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/web/exp/reward` — 查询当日经验奖励统计

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/exprewardhandler.go`
- 业务实现：`gateway/app/internal/logic/exprewardlogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`ExpRewardResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ExpRewardData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## user-profile 客户端路由（参考仓库 account-interface /x/member/*）（免鉴权，6 条）

> 资料编辑路由（参考仓库 /x/member/app/* 与 /x/member/web/update）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/x/member/app/uname/update` | 更新用户昵称 | `updateUname` | `updateunamelogic.go` |
| POST | `/x/member/app/sign/update` | 更新用户签名 | `updateSign` | `updatesignlogic.go` |
| POST | `/x/member/app/sex/update` | 更新用户性别 | `updateSex` | `updatesexlogic.go` |
| POST | `/x/member/app/birthday/update` | 更新用户生日 | `updateBirthday` | `updatebirthdaylogic.go` |
| POST | `/x/member/app/face/update` | 更新用户头像 | `updateFace` | `updatefacelogic.go` |
| POST | `/x/member/web/update` | 整体更新用户基础资料 | `updateBaseAll` | `updatebasealllogic.go` |

### POST `/x/member/app/uname/update` — 更新用户昵称

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updateunamehandler.go`
- 业务实现：`gateway/app/internal/logic/updateunamelogic.go`

请求：`ParamUname`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Name` | `name` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/app/sign/update` — 更新用户签名

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updatesignhandler.go`
- 业务实现：`gateway/app/internal/logic/updatesignlogic.go`

请求：`ParamUserSign`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `UserSign` | `user_sign` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/app/sex/update` — 更新用户性别

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updatesexhandler.go`
- 业务实现：`gateway/app/internal/logic/updatesexlogic.go`

请求：`ParamSex`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Sex` | `sex` | form | `int64` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/app/birthday/update` — 更新用户生日

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updatebirthdayhandler.go`
- 业务实现：`gateway/app/internal/logic/updatebirthdaylogic.go`

请求：`ParamBirthday`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Birthday` | `birthday` | form | `int64` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/app/face/update` — 更新用户头像

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updatefacehandler.go`
- 业务实现：`gateway/app/internal/logic/updatefacelogic.go`

请求：`ParamFace`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Face` | `face` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/web/update` — 整体更新用户基础资料

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updatebaseallhandler.go`
- 业务实现：`gateway/app/internal/logic/updatebasealllogic.go`

请求：`ParamBaseAll`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Name` | `name` | form | `string` | 是 | — | — |
| `Sex` | `sex` | form | `int64` | 是 | — | — |
| `Face` | `face` | form | `string` | 是 | — | — |
| `UserSign` | `user_sign` | form | `string` | 是 | — | — |
| `Rank` | `rank` | form | `int64` | 是 | — | — |
| `Birthday` | `birthday` | form | `int64` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamMid`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `MemberFullResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MemberFullData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `MemberBaseResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MemberBaseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMids`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

### `BatchBaseResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `BatchBaseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `MoralResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MoralData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `MoralLogResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MoralLogData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `LevelResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LevelData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `OfficialInfoResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OfficialInfoData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ExpRewardResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ExpRewardData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUname`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Name` | `name` | form | `string` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUserSign`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `UserSign` | `user_sign` | form | `string` | 是 | — | — |

### `ParamSex`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Sex` | `sex` | form | `int64` | 是 | — | — |

### `ParamBirthday`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Birthday` | `birthday` | form | `int64` | 是 | — | — |

### `ParamFace`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Face` | `face` | form | `string` | 是 | — | — |

### `ParamBaseAll`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Name` | `name` | form | `string` | 是 | — | — |
| `Sex` | `sex` | form | `int64` | 是 | — | — |
| `Face` | `face` | form | `string` | 是 | — | — |
| `UserSign` | `user_sign` | form | `string` | 是 | — | — |
| `Rank` | `rank` | form | `int64` | 是 | — | — |
| `Birthday` | `birthday` | form | `int64` | 是 | — | — |

### `MemberFullData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MemberInfo` | `member_info` | json | `MemberFullInfo` | 是 | — | — |

### `MemberBaseData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BaseInfo` | `base_info` | json | `MemberBaseInfo` | 是 | — | — |

### `BatchBaseData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BaseInfos` | `base_infos` | json | `map[int64]MemberBaseInfo` | 是 | — | — |

### `MoralData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Moral` | `moral` | json | `MoralInfo` | 是 | — | — |

### `MoralLogData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UserLogs` | `user_logs` | json | `[]UserLog` | 是 | — | — |

### `LevelData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `LevelInfo` | `level_info` | json | `LevelInfo` | 是 | — | — |

### `OfficialInfoData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OfficialInfo` | `official_info` | json | `OfficialInfo` | 是 | — | — |

### `ExpRewardData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ExpStat` | `exp_stat` | json | `ExpStat` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `MemberFullInfo`

> 用户全量信息（对应 user-profile MemberInfoReply）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BaseInfo` | `base_info` | json | `MemberBaseInfo` | 是 | — | — |
| `LevelInfo` | `level_info` | json | `LevelInfo` | 是 | — | — |
| `OfficialInfo` | `official_info` | json | `OfficialInfo` | 是 | — | — |

### `MemberBaseInfo`

> 基础资料（对应 user-profile BaseInfoReply）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Sex` | `sex` | json | `int64` | 是 | — | — |
| `Face` | `face` | json | `string` | 是 | — | — |
| `Sign` | `sign` | json | `string` | 是 | — | — |
| `Rank` | `rank` | json | `int64` | 是 | — | — |
| `Birthday` | `birthday` | json | `int64` | 是 | — | — |

### `MoralInfo`

> 节操值（对应 user-profile MoralReply）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Moral` | `moral` | json | `int64` | 是 | — | — |
| `Added` | `added` | json | `int64` | 是 | — | — |
| `Deducted` | `deducted` | json | `int64` | 是 | — | — |
| `LastRecoverDate` | `last_recover_date` | json | `int64` | 是 | — | — |

### `UserLog`

> 用户变更日志（对应 user-profile UserLogReply）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `IP` | `ip` | json | `string` | 是 | — | — |
| `TS` | `ts` | json | `int64` | 是 | — | — |
| `LogID` | `log_id` | json | `string` | 是 | — | — |
| `Content` | `content` | json | `map[string]string` | 是 | — | — |

### `LevelInfo`

> 等级信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Cur` | `current_level` | json | `int32` | 是 | — | — |
| `Min` | `current_min` | json | `int32` | 是 | — | — |
| `NowExp` | `current_exp` | json | `int32` | 是 | — | — |
| `NextExp` | `next_exp` | json | `int32` | 是 | — | — |

### `OfficialInfo`

> 官方认证信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Role` | `role` | json | `int32` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Desc` | `desc` | json | `string` | 是 | — | — |

### `ExpStat`

> 当日经验奖励统计（对应 user-profile ExpStatReply）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Login` | `login` | json | `bool` | 是 | — | — |
| `Watch` | `watch_av` | json | `bool` | 是 | — | — |
| `Coin` | `coins_av` | json | `int64` | 是 | — | — |
| `Share` | `share_av` | json | `bool` | 是 | — | — |


<!-- file: docs/api/http/app/05-x-member.md -->
