# 运营面 · `/admin/asset`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| asset 域运营路由 | 免鉴权 | 2 |

合计 **2** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## asset 域运营路由（免鉴权，2 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/asset/list` | 分页查询媒资列表（按 mid/state 过滤） | `listAssets` | `listassetslogic.go` |
| GET | `/admin/asset/:asset_id` | 查询单个媒资元数据 | `getAsset` | `getassetlogic.go` |

### GET `/admin/asset/list` — 分页查询媒资列表（按 mid/state 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listassetshandler.go`
- 业务实现：`gateway/admin/internal/logic/listassetslogic.go`

请求：`ParamListAssets`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`AssetsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AssetsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/asset/:asset_id` — 查询单个媒资元数据

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getassethandler.go`
- 业务实现：`gateway/admin/internal/logic/getassetlogic.go`

请求：`ParamAssetId`

（该类型无字段：空请求 / 空响应。）

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AssetId` | `asset_id` | path | `int64` | 是 | — | — |

响应：`AssetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AssetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamListAssets`

> asset 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `AssetsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AssetsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAssetId`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AssetId` | `asset_id` | path | `int64` | 是 | — | — |

### `AssetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AssetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `AssetsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]AssetItem` | 是 | — | — |

### `AssetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Asset` | `asset` | json | `AssetItem` | 是 | — | — |

### `AssetItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AssetId` | `asset_id` | json | `int64` | 是 | — | — |
| `UploadId` | `upload_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Bucket` | `bucket` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |
| `Md5` | `md5` | json | `string` | 是 | — | — |
| `Duration` | `duration` | json | `int64` | 是 | — | — |
| `Width` | `width` | json | `int32` | 是 | — | — |
| `Height` | `height` | json | `int32` | 是 | — | — |
| `Codec` | `codec` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/09-admin-asset.md -->
