# 运营面 · `/x/member`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| user-profile 运营路由 | AdminPermission | 9 |

合计 **9** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## user-profile 运营路由（AdminPermission，9 条）

> user-profile 运营路由（调用 user-profile RPC）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/x/member/morals/update` | 批量变更节操值 | `member:moral` / `update` | `moralsUpdate` | `moralsupdatelogic.go` |
| POST | `/x/member/moral/update` | 变更单个用户节操值 | `member:moral` / `update` | `moralUpdate` | `moralupdatelogic.go` |
| POST | `/x/member/moral/undo` | 撤销节操值变更 | `member:moral` / `undo` | `moralUndo` | `moralundologic.go` |
| POST | `/x/member/exp/set` | 设置经验值（仅运营） | `member:exp` / `set` | `expSet` | `expsetlogic.go` |
| POST | `/x/member/exp/update` | 增加经验值 | `member:exp` / `update` | `expUpdate` | `expupdatelogic.go` |
| POST | `/x/member/property/review/add` | 添加用户属性变更审核 | `member:property-review` / `create` | `propertyReview` | `propertyreviewlogic.go` |
| GET | `/x/member/realname/stripped/info` | 查询脱敏实名信息 | `member:realname` / `read` | `realnameStripped` | `realnamestrippedlogic.go` |
| GET | `/x/member/realname/mid/by/card` | 按证件号批量查询 mid | `member:realname` / `reverse-lookup` | `realnameMidByCard` | `realnamemidbycardlogic.go` |
| GET | `/x/member/web/login/log` | 查询用户登录日志 | `member:login-log` / `read` | `loginLog` | `loginloglogic.go` |

### POST `/x/member/morals/update` — 批量变更节操值

- 权限口径：AdminPermission · 权限点 `member:moral` / `update`
- goctl 入口：`gateway/admin/internal/handler/moralsupdatehandler.go`
- 业务实现：`gateway/admin/internal/logic/moralsupdatelogic.go`

请求：`ParamUpdateMorals`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |
| `Delta` | `delta` | form | `int64` | 是 | — | — |
| `Origin` | `origin` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |
| `ReasonType` | `reason_type` | form | `int64` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `Remark` | `remark` | form | `string` | 是 | — | — |
| `Status` | `status` | form | `int64` | 是 | — | — |
| `IsNotify` | `is_notify` | form | `bool` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`MoralsUpdateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MoralsUpdateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/moral/update` — 变更单个用户节操值

- 权限口径：AdminPermission · 权限点 `member:moral` / `update`
- goctl 入口：`gateway/admin/internal/handler/moralupdatehandler.go`
- 业务实现：`gateway/admin/internal/logic/moralupdatelogic.go`

请求：`ParamUpdateMoral`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Delta` | `delta` | form | `int64` | 是 | — | — |
| `Origin` | `origin` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |
| `ReasonType` | `reason_type` | form | `int64` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `Remark` | `remark` | form | `string` | 是 | — | — |
| `Status` | `status` | form | `int64` | 是 | — | — |
| `IsNotify` | `is_notify` | form | `bool` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`MoralsUpdateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MoralsUpdateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/moral/undo` — 撤销节操值变更

- 权限口径：AdminPermission · 权限点 `member:moral` / `undo`
- goctl 入口：`gateway/admin/internal/handler/moralundohandler.go`
- 业务实现：`gateway/admin/internal/logic/moralundologic.go`

请求：`ParamUndo`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `LogID` | `log_id` | form | `string` | 是 | — | — |
| `Remark` | `remark` | form | `string` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/exp/set` — 设置经验值（仅运营）

- 权限口径：AdminPermission · 权限点 `member:exp` / `set`
- goctl 入口：`gateway/admin/internal/handler/expsethandler.go`
- 业务实现：`gateway/admin/internal/logic/expsetlogic.go`

请求：`ParamExp`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Count` | `count` | form | `float64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |
| `Operate` | `operate` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/exp/update` — 增加经验值

- 权限口径：AdminPermission · 权限点 `member:exp` / `update`
- goctl 入口：`gateway/admin/internal/handler/expupdatehandler.go`
- 业务实现：`gateway/admin/internal/logic/expupdatelogic.go`

请求：`ParamExp`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Count` | `count` | form | `float64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |
| `Operate` | `operate` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/x/member/property/review/add` — 添加用户属性变更审核

- 权限口径：AdminPermission · 权限点 `member:property-review` / `create`
- goctl 入口：`gateway/admin/internal/handler/propertyreviewhandler.go`
- 业务实现：`gateway/admin/internal/logic/propertyreviewlogic.go`

请求：`ParamPropertyReview`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `New` | `new` | form | `string` | 是 | — | — |
| `State` | `state` | form | `int8` | 是 | — | — |
| `Property` | `property` | form | `int8` | 是 | — | — |
| `Extra` | `extra` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/realname/stripped/info` — 查询脱敏实名信息

- 权限口径：AdminPermission · 权限点 `member:realname` / `read`
- goctl 入口：`gateway/admin/internal/handler/realnamestrippedhandler.go`
- 业务实现：`gateway/admin/internal/logic/realnamestrippedlogic.go`

请求：`ParamModify`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ModifiedAttr` | `modifiedAttr` | form | `string` | 是 | — | — |

响应：`RealnameStrippedResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameStrippedData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/realname/mid/by/card` — 按证件号批量查询 mid

- 权限口径：AdminPermission · 权限点 `member:realname` / `reverse-lookup`
- goctl 入口：`gateway/admin/internal/handler/realnamemidbycardhandler.go`
- 业务实现：`gateway/admin/internal/logic/realnamemidbycardlogic.go`

请求：`ParamMidByCard`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `CardCode` | `card_code` | form | `[]string` | 是 | split | — |
| `Country` | `country` | form | `int32` | 是 | — | — |
| `CardType` | `card_type` | form | `int32` | 是 | — | — |

响应：`MidByCardResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MidByCardData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/x/member/web/login/log` — 查询用户登录日志

- 权限口径：AdminPermission · 权限点 `member:login-log` / `read`
- goctl 入口：`gateway/admin/internal/handler/loginloghandler.go`
- 业务实现：`gateway/admin/internal/logic/loginloglogic.go`

请求：`ParamLoginLog`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Limit` | `limit` | form | `int32` | 是 | default=20 | — |

响应：`LoginLogsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LoginLogsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamUpdateMorals`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |
| `Delta` | `delta` | form | `int64` | 是 | — | — |
| `Origin` | `origin` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |
| `ReasonType` | `reason_type` | form | `int64` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `Remark` | `remark` | form | `string` | 是 | — | — |
| `Status` | `status` | form | `int64` | 是 | — | — |
| `IsNotify` | `is_notify` | form | `bool` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `MoralsUpdateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MoralsUpdateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUpdateMoral`

> user-profile 节操变更（参考 /morals/update、/moral/update 的参数）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Delta` | `delta` | form | `int64` | 是 | — | — |
| `Origin` | `origin` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |
| `ReasonType` | `reason_type` | form | `int64` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `Remark` | `remark` | form | `string` | 是 | — | — |
| `Status` | `status` | form | `int64` | 是 | — | — |
| `IsNotify` | `is_notify` | form | `bool` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamUndo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `LogID` | `log_id` | form | `string` | 是 | — | — |
| `Remark` | `remark` | form | `string` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamExp`

> user-profile 经验值操作（参考 /exp/set、/exp/update 的参数）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Count` | `count` | form | `float64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |
| `Operate` | `operate` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamPropertyReview`

> user-profile 属性审核（参考 /property/review/add 的参数）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `New` | `new` | form | `string` | 是 | — | — |
| `State` | `state` | form | `int8` | 是 | — | — |
| `Property` | `property` | form | `int8` | 是 | — | — |
| `Extra` | `extra` | form | `string` | 是 | — | — |

### `ParamModify`

> account 缓存失效（参考 /cache/del、/cache/clear 的参数）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ModifiedAttr` | `modifiedAttr` | form | `string` | 是 | — | — |

### `RealnameStrippedResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RealnameStrippedData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMidByCard`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `CardCode` | `card_code` | form | `[]string` | 是 | split | — |
| `Country` | `country` | form | `int32` | 是 | — | — |
| `CardType` | `card_type` | form | `int32` | 是 | — | — |

### `MidByCardResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MidByCardData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamLoginLog`

> 登录日志查询参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Limit` | `limit` | form | `int32` | 是 | default=20 | — |

### `LoginLogsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `LoginLogsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `MoralsUpdateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AfterMorals` | `after_morals` | json | `map[int64]int64` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `RealnameStrippedData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Status` | `status` | json | `int8` | 是 | — | — |
| `Channel` | `channel` | json | `int8` | 是 | — | — |
| `Country` | `country` | json | `int16` | 是 | — | — |
| `CardType` | `card_type` | json | `int8` | 是 | — | — |
| `AdultType` | `adult_type` | json | `int8` | 是 | — | — |

### `MidByCardData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `CodeToMid` | `code_to_mid` | json | `map[string]int64` | 是 | — | — |

### `LoginLogsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Logs` | `logs` | json | `[]LoginLogItem` | 是 | — | — |

### `LoginLogItem`

> 单条登录日志

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `IP` | `ip` | json | `string` | 是 | — | — |
| `TS` | `ts` | json | `int64` | 是 | — | — |
| `LoginType` | `login_type` | json | `int32` | 是 | — | — |
| `Status` | `status` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Device` | `device` | json | `string` | 是 | — | — |


<!-- file: docs/api/http/admin/03-x-member.md -->
