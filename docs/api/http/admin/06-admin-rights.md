# 运营面 · `/admin/rights`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| rights 域运营路由 | 免鉴权 | 2 |
| rights 域写入口（受 AdminPermission 保护） | AdminPermission | 3 |

合计 **5** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## rights 域运营路由（免鉴权，2 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/rights/contracts` | 分页查询版权合同 | `listRightsContracts` | `listrightscontractslogic.go` |
| GET | `/admin/rights/windows` | 分页查询播放窗口 | `listRightsWindows` | `listrightswindowslogic.go` |

### GET `/admin/rights/contracts` — 分页查询版权合同

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listrightscontractshandler.go`
- 业务实现：`gateway/admin/internal/logic/listrightscontractslogic.go`

请求：`ParamListContracts`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerId` | `owner_id` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`RightsContractsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RightsContractsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/rights/windows` — 分页查询播放窗口

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listrightswindowshandler.go`
- 业务实现：`gateway/admin/internal/logic/listrightswindowslogic.go`

请求：`ParamListWindows`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ContentId` | `content_id` | form | `int64` | 是 | — | — |
| `ContractId` | `contract_id` | form | `int64` | 是 | — | — |
| `ContentType` | `content_type` | form | `int32` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`RightsWindowsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RightsWindowsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## rights 域写入口（受 AdminPermission 保护）（AdminPermission，3 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/rights/contracts` | 运营创建版权合同 | `rights:contract` / `create` | `createRightsContract` | `createrightscontractlogic.go` |
| POST | `/admin/rights/windows` | 运营创建播放窗口 | `rights:window` / `create` | `createRightsWindow` | `createrightswindowlogic.go` |
| POST | `/admin/rights/windows/:window_id/expire` | 手动过期播放窗口 | `rights:window` / `expire` | `expireRightsWindow` | `expirerightswindowlogic.go` |

### POST `/admin/rights/contracts` — 运营创建版权合同

- 权限口径：AdminPermission · 权限点 `rights:contract` / `create`
- goctl 入口：`gateway/admin/internal/handler/createrightscontracthandler.go`
- 业务实现：`gateway/admin/internal/logic/createrightscontractlogic.go`

请求：`ParamCreateContract`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerId` | `owner_id` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `SignDate` | `sign_date` | json | `int64` | 是 | — | — |
| `StartDate` | `start_date` | json | `int64` | 是 | — | — |
| `EndDate` | `end_date` | json | `int64` | 是 | — | — |
| `Regions` | `regions` | json | `[]string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |

响应：`RightsContractResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RightsContractData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/rights/windows` — 运营创建播放窗口

- 权限口径：AdminPermission · 权限点 `rights:window` / `create`
- goctl 入口：`gateway/admin/internal/handler/createrightswindowhandler.go`
- 业务实现：`gateway/admin/internal/logic/createrightswindowlogic.go`

请求：`ParamCreateWindow`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ContractId` | `contract_id` | json | `int64` | 是 | — | — |
| `ContentId` | `content_id` | json | `int64` | 是 | — | — |
| `ContentType` | `content_type` | json | `int32` | 是 | — | — |
| `Region` | `region` | json | `string` | 是 | — | — |
| `StartTime` | `start_time` | json | `int64` | 是 | — | — |
| `EndTime` | `end_time` | json | `int64` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |

响应：`RightsWindowResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RightsWindowData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/rights/windows/:window_id/expire` — 手动过期播放窗口

- 权限口径：AdminPermission · 权限点 `rights:window` / `expire`
- goctl 入口：`gateway/admin/internal/handler/expirerightswindowhandler.go`
- 业务实现：`gateway/admin/internal/logic/expirerightswindowlogic.go`

请求：`ParamRightsWindowId`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `WindowId` | `window_id` | path | `int64` | 是 | — | — |

响应：`RightsWindowResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RightsWindowData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamListContracts`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerId` | `owner_id` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `RightsContractsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RightsContractsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListWindows`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ContentId` | `content_id` | form | `int64` | 是 | — | — |
| `ContractId` | `contract_id` | form | `int64` | 是 | — | — |
| `ContentType` | `content_type` | form | `int32` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `RightsWindowsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RightsWindowsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCreateContract`

> rights 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OwnerId` | `owner_id` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `SignDate` | `sign_date` | json | `int64` | 是 | — | — |
| `StartDate` | `start_date` | json | `int64` | 是 | — | — |
| `EndDate` | `end_date` | json | `int64` | 是 | — | — |
| `Regions` | `regions` | json | `[]string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |

### `RightsContractResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RightsContractData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCreateWindow`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ContractId` | `contract_id` | json | `int64` | 是 | — | — |
| `ContentId` | `content_id` | json | `int64` | 是 | — | — |
| `ContentType` | `content_type` | json | `int32` | 是 | — | — |
| `Region` | `region` | json | `string` | 是 | — | — |
| `StartTime` | `start_time` | json | `int64` | 是 | — | — |
| `EndTime` | `end_time` | json | `int64` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |

### `RightsWindowResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RightsWindowData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRightsWindowId`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `WindowId` | `window_id` | path | `int64` | 是 | — | — |

### `RightsContractsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Contracts` | `contracts` | json | `[]RightsContractItem` | 是 | — | — |

### `RightsWindowsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Windows` | `windows` | json | `[]RightsWindowItem` | 是 | — | — |

### `RightsContractData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Contract` | `contract` | json | `RightsContractItem` | 是 | — | — |

### `RightsWindowData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Window` | `window` | json | `RightsWindowItem` | 是 | — | — |

### `RightsContractItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ContractId` | `contract_id` | json | `int64` | 是 | — | — |
| `OwnerId` | `owner_id` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `SignDate` | `sign_date` | json | `int64` | 是 | — | — |
| `StartDate` | `start_date` | json | `int64` | 是 | — | — |
| `EndDate` | `end_date` | json | `int64` | 是 | — | — |
| `Regions` | `regions` | json | `[]string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `RightsWindowItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `WindowId` | `window_id` | json | `int64` | 是 | — | — |
| `ContractId` | `contract_id` | json | `int64` | 是 | — | — |
| `ContentId` | `content_id` | json | `int64` | 是 | — | — |
| `ContentType` | `content_type` | json | `int32` | 是 | — | — |
| `Region` | `region` | json | `string` | 是 | — | — |
| `StartTime` | `start_time` | json | `int64` | 是 | — | — |
| `EndTime` | `end_time` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/06-admin-rights.md -->
