# 终端面 · `/catalog`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| catalog 域聚合（services/catalog/rpc/catalog.proto） | 免鉴权 | 5 |

合计 **5** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## catalog 域聚合（services/catalog/rpc/catalog.proto）（免鉴权，5 条）

> catalog 域路由

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/catalog/works/:work_id` | 查询作品详情 | `getWork` | `getworklogic.go` |
| GET | `/catalog/works` | 分页查询作品 | `listWorks` | `listworkslogic.go` |
| GET | `/catalog/seasons/:season_id/episodes` | 查询某季的集列表 | `listEpisodes` | `listepisodeslogic.go` |
| GET | `/catalog/episodes/:epid` | 查询集详情 | `getEpisode` | `getepisodelogic.go` |
| GET | `/catalog/zones` | 分区树（扁平列表） | `listZones` | `listzoneslogic.go` |

### GET `/catalog/works/:work_id` — 查询作品详情

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getworkhandler.go`
- 业务实现：`gateway/app/internal/logic/getworklogic.go`

请求：`ParamCatalogWorkId`

（该类型无字段：空请求 / 空响应。）

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `WorkId` | `work_id` | path | `int64` | 是 | — | — |

响应：`CatalogWorkResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogWorkData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/catalog/works` — 分页查询作品

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listworkshandler.go`
- 业务实现：`gateway/app/internal/logic/listworkslogic.go`

请求：`ParamListWorks`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | default=-1 | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`CatalogWorksResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogWorksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/catalog/seasons/:season_id/episodes` — 查询某季的集列表

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listepisodeshandler.go`
- 业务实现：`gateway/app/internal/logic/listepisodeslogic.go`

请求：`ParamCatalogSeasonId`

（该类型无字段：空请求 / 空响应。）

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SeasonId` | `season_id` | path | `int64` | 是 | — | — |

响应：`CatalogEpisodesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogEpisodesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/catalog/episodes/:epid` — 查询集详情

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getepisodehandler.go`
- 业务实现：`gateway/app/internal/logic/getepisodelogic.go`

请求：`ParamCatalogEpid`

（该类型无字段：空请求 / 空响应。）

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Epid` | `epid` | path | `int64` | 是 | — | — |

响应：`CatalogEpisodeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogEpisodeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/catalog/zones` — 分区树（扁平列表）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listzoneshandler.go`
- 业务实现：`gateway/app/internal/logic/listzoneslogic.go`

请求：无参数体。

响应：`CatalogZonesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogZonesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamCatalogWorkId`

> catalog 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `WorkId` | `work_id` | path | `int64` | 是 | — | — |

### `CatalogWorkResponse`

> catalog 域响应信封

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogWorkData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListWorks`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | default=-1 | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `CatalogWorksResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogWorksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCatalogSeasonId`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SeasonId` | `season_id` | path | `int64` | 是 | — | — |

### `CatalogEpisodesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogEpisodesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCatalogEpid`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Epid` | `epid` | path | `int64` | 是 | — | — |

### `CatalogEpisodeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogEpisodeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CatalogZonesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CatalogZonesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CatalogWorkData`

> catalog 域响应数据载荷

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Work` | `work` | json | `CatalogWork` | 是 | — | — |

### `CatalogWorksData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Works` | `works` | json | `[]CatalogWork` | 是 | — | — |

### `CatalogEpisodesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Episodes` | `episodes` | json | `[]CatalogEpisode` | 是 | — | — |

### `CatalogEpisodeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Episode` | `episode` | json | `CatalogEpisode` | 是 | — | — |

### `CatalogZonesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Zones` | `zones` | json | `[]CatalogZone` | 是 | — | — |

### `CatalogWork`

> 作品（对应 catalog.WorkReply）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SeasonId` | `season_id` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Cover` | `cover` | json | `string` | 是 | — | — |
| `Typeid` | `typeid` | json | `int32` | 是 | — | — |
| `Intro` | `intro` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |

### `CatalogEpisode`

> 集（对应 catalog.EpisodeReply）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Epid` | `epid` | json | `int64` | 是 | — | — |
| `SeasonId` | `season_id` | json | `int64` | 是 | — | — |
| `EpNo` | `ep_no` | json | `int32` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `AssetId` | `asset_id` | json | `int64` | 是 | — | — |
| `Duration` | `duration` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |

### `CatalogZone`

> 分区（对应 catalog.ZoneReply）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Zoneid` | `zoneid` | json | `int32` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Parent` | `parent` | json | `int32` | 是 | — | — |


<!-- file: docs/api/http/app/12-catalog.md -->
