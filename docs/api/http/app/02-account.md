# 终端面 · `/account`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| account 聚合查询路由 | 免鉴权 | 9 |
| /privacy 仅允许白名单 appkey 访问 | AppkeyVerify | 1 |

合计 **10** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## account 聚合查询路由（免鉴权，9 条）

> account 聚合查询路由（BFF：调用 account gRPC）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/account/info` | 查询单个用户基础信息 | `info` | `infologic.go` |
| GET | `/account/infos` | 批量查询用户基础信息 | `infos` | `infoslogic.go` |
| GET | `/account/info/by/name` | 按用户名批量查询用户基础信息 | `infoByName` | `infobynamelogic.go` |
| GET | `/account/card` | 查询单个用户名片 | `card` | `cardlogic.go` |
| GET | `/account/cards` | 批量查询用户名片 | `cards` | `cardslogic.go` |
| GET | `/account/vip` | 查询单个用户会员信息 | `vip` | `viplogic.go` |
| GET | `/account/vips` | 批量查询用户会员信息 | `vips` | `vipslogic.go` |
| GET | `/account/profile` | 查询用户完整资料 | `profile` | `profilelogic.go` |
| GET | `/account/profile/stat` | 查询带统计的资料 | `profileWithStat` | `profilewithstatlogic.go` |

### GET `/account/info` — 查询单个用户基础信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/infohandler.go`
- 业务实现：`gateway/app/internal/logic/infologic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`InfoResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InfoData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/infos` — 批量查询用户基础信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/infoshandler.go`
- 业务实现：`gateway/app/internal/logic/infoslogic.go`

请求：`ParamMids`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

响应：`InfosResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InfosData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/info/by/name` — 按用户名批量查询用户基础信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/infobynamehandler.go`
- 业务实现：`gateway/app/internal/logic/infobynamelogic.go`

请求：`ParamNames`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Names` | `names` | form | `[]string` | 是 | split | — |

响应：`InfosResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InfosData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/card` — 查询单个用户名片

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/cardhandler.go`
- 业务实现：`gateway/app/internal/logic/cardlogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`CardResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CardData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/cards` — 批量查询用户名片

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/cardshandler.go`
- 业务实现：`gateway/app/internal/logic/cardslogic.go`

请求：`ParamMids`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

响应：`CardsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CardsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/vip` — 查询单个用户会员信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/viphandler.go`
- 业务实现：`gateway/app/internal/logic/viplogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`VipResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VipData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/vips` — 批量查询用户会员信息

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/vipshandler.go`
- 业务实现：`gateway/app/internal/logic/vipslogic.go`

请求：`ParamMids`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

响应：`VipsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VipsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/profile` — 查询用户完整资料

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/profilehandler.go`
- 业务实现：`gateway/app/internal/logic/profilelogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`ProfileResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ProfileData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/account/profile/stat` — 查询带统计的资料

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/profilewithstathandler.go`
- 业务实现：`gateway/app/internal/logic/profilewithstatlogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`ProfileStatResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ProfileStatData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## /privacy 仅允许白名单 appkey 访问（AppkeyVerify，1 条）

> /privacy 仅允许白名单 appkey 访问（从 account 服务迁移的 filterByAppkey 语义）

鉴权：`AppkeyVerify`：query 参数 `appkey` 必须在白名单内，否则 403

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/account/privacy` | 查询用户隐私信息（需白名单 appkey） | `privacy` | `privacylogic.go` |

### GET `/account/privacy` — 查询用户隐私信息（需白名单 appkey）

- 权限口径：AppkeyVerify · query `appkey` 白名单
- goctl 入口：`gateway/app/internal/handler/privacyhandler.go`
- 业务实现：`gateway/app/internal/logic/privacylogic.go`

请求：`ParamMid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`PrivacyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PrivacyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamMid`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `InfoResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InfoData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMids`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

### `InfosResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `InfosData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamNames`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Names` | `names` | form | `[]string` | 是 | split | — |

### `CardResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CardData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CardsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CardsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `VipResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VipData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `VipsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VipsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ProfileResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ProfileData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ProfileStatResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ProfileStatData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `PrivacyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PrivacyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `InfoData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Info` | `info` | json | `Info` | 是 | — | — |

### `InfosData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Infos` | `infos` | json | `map[int64]Info` | 是 | — | — |

### `CardData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Card` | `card` | json | `Card` | 是 | — | — |

### `CardsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Cards` | `cards` | json | `map[int64]Card` | 是 | — | — |

### `VipData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Vip` | `vip` | json | `VipInfo` | 是 | — | — |

### `VipsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Vips` | `vips` | json | `map[int64]VipInfo` | 是 | — | — |

### `ProfileData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Profile` | `profile` | json | `Profile` | 是 | — | — |

### `ProfileStatData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ProfileStat` | `profile_stat` | json | `ProfileStat` | 是 | — | — |

### `PrivacyData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Privacy` | `privacy` | json | `Privacy` | 是 | — | — |

### `Info`

> 用户基础信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Sex` | `sex` | json | `string` | 是 | — | — |
| `Face` | `face` | json | `string` | 是 | — | — |
| `Sign` | `sign` | json | `string` | 是 | — | — |
| `Rank` | `rank` | json | `int32` | 是 | — | — |

### `Card`

> 用户名片

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Sex` | `sex` | json | `string` | 是 | — | — |
| `Face` | `face` | json | `string` | 是 | — | — |
| `Sign` | `sign` | json | `string` | 是 | — | — |
| `Rank` | `rank` | json | `int32` | 是 | — | — |
| `Level` | `level` | json | `int32` | 是 | — | — |
| `Silence` | `silence` | json | `int32` | 是 | — | — |
| `Vip` | `vip` | json | `VipInfo` | 是 | — | — |
| `Pendant` | `pendant` | json | `PendantInfo` | 是 | — | — |
| `Nameplate` | `nameplate` | json | `NameplateInfo` | 是 | — | — |
| `Official` | `official` | json | `OfficialInfo` | 是 | — | — |

### `VipInfo`

> 会员信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Type` | `type` | json | `int32` | 是 | — | — |
| `Status` | `status` | json | `int32` | 是 | — | — |
| `DueDate` | `due_date` | json | `int64` | 是 | — | — |
| `VipPayType` | `vip_pay_type` | json | `int32` | 是 | — | — |

### `Profile`

> 用户完整资料

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Sex` | `sex` | json | `string` | 是 | — | — |
| `Face` | `face` | json | `string` | 是 | — | — |
| `Sign` | `sign` | json | `string` | 是 | — | — |
| `Rank` | `rank` | json | `int32` | 是 | — | — |
| `Level` | `level` | json | `int32` | 是 | — | — |
| `JoinTime` | `jointime` | json | `int32` | 是 | — | — |
| `Moral` | `moral` | json | `int32` | 是 | — | — |
| `Silence` | `silence` | json | `int32` | 是 | — | — |
| `EmailStatus` | `email_status` | json | `int32` | 是 | — | — |
| `TelStatus` | `tel_status` | json | `int32` | 是 | — | — |
| `Identification` | `identification` | json | `int32` | 是 | — | — |
| `Vip` | `vip` | json | `VipInfo` | 是 | — | — |
| `Pendant` | `pendant` | json | `PendantInfo` | 是 | — | — |
| `Nameplate` | `nameplate` | json | `NameplateInfo` | 是 | — | — |
| `Official` | `official` | json | `OfficialInfo` | 是 | — | — |
| `Birthday` | `birthday` | json | `int64` | 是 | — | — |
| `IsTourist` | `is_tourist` | json | `int32` | 是 | — | — |

### `ProfileStat`

> 带统计的资料

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Profile` | `profile` | json | `Profile` | 是 | — | — |
| `LevelExp` | `level_exp` | json | `LevelInfo` | 是 | — | — |
| `Coins` | `coins` | json | `float64` | 是 | — | — |
| `Following` | `following` | json | `int64` | 是 | — | — |
| `Follower` | `follower` | json | `int64` | 是 | — | — |

### `Privacy`

> 隐私信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Realname` | `realname` | json | `string` | 是 | — | — |
| `IdentityCard` | `identity_card` | json | `string` | 是 | — | — |
| `IdentitySex` | `identity_sex` | json | `string` | 是 | — | — |
| `Tel` | `tel` | json | `string` | 是 | — | — |
| `RegIP` | `reg_ip` | json | `string` | 是 | — | — |
| `RegTS` | `reg_ts` | json | `int64` | 是 | — | — |
| `HandIMG` | `hand_img` | json | `string` | 是 | — | — |

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

### `OfficialInfo`

> 官方认证信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Role` | `role` | json | `int32` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Desc` | `desc` | json | `string` | 是 | — | — |

### `LevelInfo`

> 等级信息

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Cur` | `current_level` | json | `int32` | 是 | — | — |
| `Min` | `current_min` | json | `int32` | 是 | — | — |
| `NowExp` | `current_exp` | json | `int32` | 是 | — | — |
| `NextExp` | `next_exp` | json | `int32` | 是 | — | — |


<!-- file: docs/api/http/app/02-account.md -->
