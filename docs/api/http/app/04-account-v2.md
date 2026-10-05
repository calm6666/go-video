# 终端面 · `/account/v2`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| v2 兼容路由 | 免鉴权 | 2 |

合计 **2** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## v2 兼容路由（免鉴权，2 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/account/v2/myinfo` | v2 查询我的资料 | `v2MyInfo` | `v2myinfologic.go` |
| GET | `/account/v2/userinfo` | v2 查询我的资料（userinfo 别名） | `v2UserInfo` | `v2userinfologic.go` |

### GET `/account/v2/myinfo` — v2 查询我的资料

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/v2myinfohandler.go`
- 业务实现：`gateway/app/internal/logic/v2myinfologic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`V2MyInfoResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `V2MyInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/v2/userinfo` — v2 查询我的资料（userinfo 别名）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/v2userinfohandler.go`
- 业务实现：`gateway/app/internal/logic/v2userinfologic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`V2MyInfoResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `V2MyInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamMid`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `V2MyInfoResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `V2MyInfo` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `V2MyInfo`

> v2 我的资料（myinfo/userinfo 共用）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Name` | `uname` | json | `string` | 是 | — | — |
| `Face` | `face` | json | `string` | 是 | — | — |
| `Rank` | `rank` | json | `int32` | 是 | — | — |
| `Scores` | `scores` | json | `int32` | 是 | — | — |
| `Coins` | `coins` | json | `float64` | 是 | — | — |
| `Sex` | `sex` | json | `int32` | 是 | — | — |
| `Sign` | `sign` | json | `string` | 是 | — | — |
| `JoinTime` | `jointime` | json | `int32` | 是 | — | — |
| `Spacesta` | `spacesta` | json | `int32` | 是 | — | — |
| `Active` | `active` | json | `int32` | 是 | — | — |
| `Silence` | `silence` | json | `int32` | 是 | — | — |
| `EmailStatus` | `email_status` | json | `int32` | 是 | — | — |
| `TelStatus` | `tel_status` | json | `int32` | 是 | — | — |
| `Identification` | `identification` | json | `int32` | 是 | — | — |
| `Moral` | `moral` | json | `int32` | 是 | — | — |
| `Birthday` | `birthday` | json | `string` | 是 | — | — |
| `Telephone` | `telephone` | json | `string` | 是 | — | — |
| `LevelInfo` | `level_info` | json | `LevelInfo` | 是 | — | — |
| `Pendant` | `pendant` | json | `PendantInfo` | 是 | — | — |
| `Nameplate` | `nameplate` | json | `NameplateInfo` | 是 | — | — |
| `OfficialVerify` | `official_verify` | json | `V1OfficialVerify` | 是 | — | — |
| `Vip` | `vip` | json | `V1VipDetail` | 是 | — | — |

### `LevelInfo`

> 等级信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Cur` | `current_level` | json | `int32` | 是 | — | — |
| `Min` | `current_min` | json | `int32` | 是 | — | — |
| `NowExp` | `current_exp` | json | `int32` | 是 | — | — |
| `NextExp` | `next_exp` | json | `int32` | 是 | — | — |

### `PendantInfo`

> 头像挂件信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pid` | `pid` | json | `int32` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Image` | `image` | json | `string` | 是 | — | — |
| `Expire` | `expire` | json | `int64` | 是 | — | — |

### `NameplateInfo`

> 勋章信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Nid` | `nid` | json | `int32` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Image` | `image` | json | `string` | 是 | — | — |
| `ImageSmall` | `image_small` | json | `string` | 是 | — | — |
| `Level` | `level` | json | `string` | 是 | — | — |
| `Condition` | `condition` | json | `string` | 是 | — | — |

### `V1OfficialVerify`

> v1 官方认证（旧结构：type + desc）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Type` | `type` | json | `int8` | 是 | — | — |
| `Desc` | `desc` | json | `string` | 是 | — | — |

### `V1VipDetail`

> v1 会员信息明细（老客户端字段名）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Type` | `vipType` | json | `int` | 是 | — | — |
| `DueDate` | `vipDueDate` | json | `int64` | 是 | — | — |
| `DueRemark` | `dueRemark` | json | `string` | 是 | — | — |
| `AccessStatus` | `accessStatus` | json | `int` | 是 | — | — |
| `VipStatus` | `vipStatus` | json | `int` | 是 | — | — |
| `VipStatusWarn` | `vipStatusWarn` | json | `string` | 是 | — | — |


<!-- file: docs/api/http/app/04-account-v2.md -->
