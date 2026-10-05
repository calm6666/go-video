# 运营面 · `/admin/account`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| account 缓存运营路由 | AdminPermission | 2 |

合计 **2** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## account 缓存运营路由（AdminPermission，2 条）

> account 缓存运营路由（调用 account DelCache RPC）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| GET | `/admin/account/cache/del` | 失效指定用户的缓存 | `account:cache` / `invalidate` | `cacheDel` | `cachedellogic.go` |
| POST | `/admin/account/cache/clear` | 接收资料变更通知，失效缓存 | `account:cache` / `invalidate` | `cacheClear` | `cacheclearlogic.go` |

### GET `/admin/account/cache/del` — 失效指定用户的缓存

- 权限口径：AdminPermission · 权限点 `account:cache` / `invalidate`
- goctl 入口：`gateway/admin/internal/handler/cachedelhandler.go`
- 业务实现：`gateway/admin/internal/logic/cachedellogic.go`

请求：`ParamModify`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ModifiedAttr` | `modifiedAttr` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/account/cache/clear` — 接收资料变更通知，失效缓存

- 权限口径：AdminPermission · 权限点 `account:cache` / `invalidate`
- goctl 入口：`gateway/admin/internal/handler/cacheclearhandler.go`
- 业务实现：`gateway/admin/internal/logic/cacheclearlogic.go`

请求：`ParamMsg`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Msg` | `msg` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamModify`

> account 缓存失效（参考 /cache/del、/cache/clear 的参数）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ModifiedAttr` | `modifiedAttr` | form | `string` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMsg`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Msg` | `msg` | form | `string` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）


<!-- file: docs/api/http/admin/02-admin-account.md -->
