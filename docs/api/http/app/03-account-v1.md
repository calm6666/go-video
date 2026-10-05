# 终端面 · `/account/v1`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| v1 老客户端兼容路由 | 免鉴权 | 4 |

合计 **4** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## v1 老客户端兼容路由（免鉴权，4 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/account/v1/info` | v1 老客户端：查询用户基础信息 | `v1Info` | `v1infologic.go` |
| GET | `/account/v1/infos` | v1 老客户端：批量查询用户基础信息 | `v1Infos` | `v1infoslogic.go` |
| GET | `/account/v1/card` | v1 老客户端：查询用户名片 | `v1Card` | `v1cardlogic.go` |
| GET | `/account/v1/vip` | v1 老客户端：查询用户会员信息 | `v1Vip` | `v1viplogic.go` |

### GET `/account/v1/info` — v1 老客户端：查询用户基础信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/v1infohandler.go`
- 业务实现：`gateway/app/internal/logic/v1infologic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`V1InfoResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `V1Info` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/v1/infos` — v1 老客户端：批量查询用户基础信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/v1infoshandler.go`
- 业务实现：`gateway/app/internal/logic/v1infoslogic.go`

请求：`ParamMids`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

响应：`V1InfosResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `map[string]V1Info` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/v1/card` — v1 老客户端：查询用户名片

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/v1cardhandler.go`
- 业务实现：`gateway/app/internal/logic/v1cardlogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`V1CardResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `V1Card` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/v1/vip` — v1 老客户端：查询用户会员信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/v1viphandler.go`
- 业务实现：`gateway/app/internal/logic/v1viplogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`V1VipResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `V1Vip` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamMid`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `V1InfoResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `V1Info` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMids`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

### `V1InfosResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `map[string]V1Info` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `V1CardResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `V1Card` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `V1VipResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `V1Vip` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `V1Info`

> v1 用户基础信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `string` | 是 | — | — |
| `Name` | `uname` | json | `string` | 是 | — | — |
| `Sex` | `sex` | json | `string` | 是 | — | — |
| `Sign` | `sign` | json | `string` | 是 | — | — |
| `Avatar` | `avatar` | json | `string` | 是 | — | — |
| `Rank` | `rank` | json | `string` | 是 | — | — |
| `DisplayRank` | `DisplayRank` | json | `string` | 是 | — | — |
| `LevelInfo` | `level_info` | json | `V1LevelInfo` | 是 | — | — |
| `Pendant` | `pendant` | json | `PendantInfo` | 是 | — | — |
| `Nameplate` | `nameplate` | json | `NameplateInfo` | 是 | — | — |
| `OfficialVerify` | `official_verify` | json | `V1OfficialVerify` | 是 | — | — |
| `Vip` | `vip` | json | `V1VipDetail` | 是 | — | — |

### `V1Card`

> v1 用户名片

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `string` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Approve` | `approve` | json | `bool` | 是 | — | — |
| `Sex` | `sex` | json | `string` | 是 | — | — |
| `Rank` | `rank` | json | `string` | 是 | — | — |
| `Face` | `face` | json | `string` | 是 | — | — |
| `DisplayRank` | `DisplayRank` | json | `string` | 是 | — | — |
| `Regtime` | `regtime` | json | `int64` | 是 | — | — |
| `Spacesta` | `spacesta` | json | `int` | 是 | — | — |
| `Birthday` | `birthday` | json | `string` | 是 | — | — |
| `Place` | `place` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `Article` | `article` | json | `int` | 是 | — | — |
| `Attentions` | `attentions` | json | `[]int64` | 是 | — | — |
| `Fans` | `fans` | json | `int` | 是 | — | — |
| `Friend` | `friend` | json | `int` | 是 | — | — |
| `Attention` | `attention` | json | `int` | 是 | — | — |
| `Sign` | `sign` | json | `string` | 是 | — | — |
| `LevelInfo` | `level_info` | json | `V1LevelInfo` | 是 | — | — |
| `Pendant` | `pendant` | json | `PendantInfo` | 是 | — | — |
| `Nameplate` | `nameplate` | json | `NameplateInfo` | 是 | — | — |
| `OfficialVerify` | `official_verify` | json | `V1OfficialVerify` | 是 | — | — |
| `Vip` | `vip` | json | `V1VipDetail` | 是 | — | — |

### `V1Vip`

> v1 会员信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Type` | `vipType` | json | `int` | 是 | — | — |
| `DueDate` | `vipDueDate` | json | `int64` | 是 | — | — |
| `DueRemark` | `dueRemark` | json | `string` | 是 | — | — |
| `AccessStatus` | `accessStatus` | json | `int` | 是 | — | — |
| `VipStatus` | `vipStatus` | json | `int` | 是 | — | — |
| `VipStatusWarn` | `vipStatusWarn` | json | `string` | 是 | — | — |

### `V1LevelInfo`

> v1 等级信息：next_exp 在满级时为字符串 "--"

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Cur` | `current_level` | json | `int` | 是 | — | — |
| `Min` | `current_min` | json | `int` | 是 | — | — |
| `NowExp` | `current_exp` | json | `int` | 是 | — | — |
| `NextExp` | `next_exp` | json | `interface{}` | 是 | — | — |

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


<!-- file: docs/api/http/app/03-account-v1.md -->
