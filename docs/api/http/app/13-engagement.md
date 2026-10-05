# 终端面 · `/engagement`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| engagement 域聚合（services/engagement/rpc/engagement.proto） | 免鉴权 | 5 |
| engagement 增量：收藏状态、收藏夹与分享 | 免鉴权 | 7 |

合计 **12** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## engagement 域聚合（services/engagement/rpc/engagement.proto）（免鉴权，5 条）

> engagement 域路由

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/engagement/like` | 点赞/取消点赞/点踩（幂等） | `like` | `likelogic.go` |
| GET | `/engagement/stats` | 批量查询对象计数与当前用户状态 | `engagementStats` | `engagementstatslogic.go` |
| POST | `/engagement/fav` | 添加收藏 | `addFav` | `addfavlogic.go` |
| POST | `/engagement/unfav` | 删除收藏 | `delFav` | `delfavlogic.go` |
| GET | `/engagement/folders` | 用户收藏夹列表 | `userFolders` | `userfolderslogic.go` |

### POST `/engagement/like` — 点赞/取消点赞/点踩（幂等）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/likehandler.go`
- 业务实现：`gateway/app/internal/logic/likelogic.go`

请求：`ParamLike`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Business` | `business` | form | `string` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `UpMid` | `up_mid` | form | `int64` | 是 | — | — |
| `OriginId` | `origin_id` | form | `int64` | 是 | — | — |
| `MessageId` | `message_id` | form | `int64` | 是 | — | — |
| `Action` | `action` | form | `int32` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EngagementLikeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementLikeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/engagement/stats` — 批量查询对象计数与当前用户状态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/engagementstatshandler.go`
- 业务实现：`gateway/app/internal/logic/engagementstatslogic.go`

请求：`ParamEngagementStats`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Business` | `business` | form | `string` | 是 | — | — |
| `OriginId` | `origin_id` | form | `int64` | 是 | — | — |
| `MessageIds` | `message_ids` | form | `[]int64` | 是 | split | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EngagementStatsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementStatsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/engagement/fav` — 添加收藏

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/addfavhandler.go`
- 业务实现：`gateway/app/internal/logic/addfavlogic.go`

请求：`ParamAddFav`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Fid` | `fid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Otype` | `otype` | form | `int32` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/engagement/unfav` — 删除收藏

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/delfavhandler.go`
- 业务实现：`gateway/app/internal/logic/delfavlogic.go`

请求：`ParamDelFav`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Fid` | `fid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Otype` | `otype` | form | `int32` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/engagement/folders` — 用户收藏夹列表

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/userfoldershandler.go`
- 业务实现：`gateway/app/internal/logic/userfolderslogic.go`

请求：`ParamUserFolders`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Vmid` | `vmid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `AllCount` | `all_count` | form | `bool` | 是 | — | — |
| `Otype` | `otype` | form | `int32` | 是 | — | — |

响应：`EngagementFoldersResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementFoldersData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## engagement 增量：收藏状态、收藏夹与分享（免鉴权，7 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/engagement/fav/state` | 单对象收藏状态 | `isFavored` | `isfavoredlogic.go` |
| GET | `/engagement/fav/states` | 批量收藏状态（最多 100） | `isFavoreds` | `isfavoredslogic.go` |
| POST | `/engagement/folder/add` | 新建收藏夹 | `addFolder` | `addfolderlogic.go` |
| POST | `/engagement/folder/del` | 删除收藏夹（软删） | `delFolder` | `delfolderlogic.go` |
| POST | `/engagement/share` | 上报分享并返回最新分享数 | `addShare` | `addsharelogic.go` |
| GET | `/engagement/has_like` | 批量查询当前用户点赞状态 | `hasLike` | `haslikelogic.go` |
| GET | `/engagement/user_likes` | 当前用户的点赞历史 | `userLikes` | `userlikeslogic.go` |

### GET `/engagement/fav/state` — 单对象收藏状态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/isfavoredhandler.go`
- 业务实现：`gateway/app/internal/logic/isfavoredlogic.go`

请求：`ParamIsFavored`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |

响应：`EngagementFavStateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementFavStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/engagement/fav/states` — 批量收藏状态（最多 100）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/isfavoredshandler.go`
- 业务实现：`gateway/app/internal/logic/isfavoredslogic.go`

请求：`ParamIsFavoreds`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Oids` | `oids` | form | `[]int64` | 是 | split | — |

响应：`EngagementFavStatesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementFavStatesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/engagement/folder/add` — 新建收藏夹

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/addfolderhandler.go`
- 业务实现：`gateway/app/internal/logic/addfolderlogic.go`

请求：`ParamAddFolder`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Name` | `name` | form | `string` | 是 | — | — |
| `Description` | `description` | form | `string` | 否 | — | — |
| `Cover` | `cover` | form | `string` | 否 | — | — |
| `Public` | `public` | form | `int32` | 否 | — | — |

响应：`EngagementFolderOpResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementFolderOpData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/engagement/folder/del` — 删除收藏夹（软删）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/delfolderhandler.go`
- 业务实现：`gateway/app/internal/logic/delfolderlogic.go`

请求：`ParamDelFolder`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Fid` | `fid` | form | `int64` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/engagement/share` — 上报分享并返回最新分享数

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/addsharehandler.go`
- 业务实现：`gateway/app/internal/logic/addsharelogic.go`

请求：`ParamAddShare`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Type` | `type` | form | `int32` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EngagementFolderOpResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementFolderOpData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/engagement/has_like` — 批量查询当前用户点赞状态

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/haslikehandler.go`
- 业务实现：`gateway/app/internal/logic/haslikelogic.go`

请求：`ParamHasLike`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Business` | `business` | form | `string` | 是 | — | — |
| `MessageIds` | `message_ids` | form | `[]int64` | 是 | split | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EngagementHasLikeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementHasLikeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/engagement/user_likes` — 当前用户的点赞历史

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/userlikeshandler.go`
- 业务实现：`gateway/app/internal/logic/userlikeslogic.go`

请求：`ParamUserLikes`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Business` | `business` | form | `string` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EngagementUserLikesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementUserLikesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamLike`

> engagement 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Business` | `business` | form | `string` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `UpMid` | `up_mid` | form | `int64` | 是 | — | — |
| `OriginId` | `origin_id` | form | `int64` | 是 | — | — |
| `MessageId` | `message_id` | form | `int64` | 是 | — | — |
| `Action` | `action` | form | `int32` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `EngagementLikeResponse`

> engagement 域响应信封

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementLikeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamEngagementStats`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Business` | `business` | form | `string` | 是 | — | — |
| `OriginId` | `origin_id` | form | `int64` | 是 | — | — |
| `MessageIds` | `message_ids` | form | `[]int64` | 是 | split | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `EngagementStatsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementStatsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAddFav`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Fid` | `fid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Otype` | `otype` | form | `int32` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamDelFav`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Fid` | `fid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Otype` | `otype` | form | `int32` | 是 | — | — |

### `ParamUserFolders`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Vmid` | `vmid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `AllCount` | `all_count` | form | `bool` | 是 | — | — |
| `Otype` | `otype` | form | `int32` | 是 | — | — |

### `EngagementFoldersResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementFoldersData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamIsFavored`

> 高级收藏夹管理（移动/复制/排序）属运营面，本期不在终端网关暴露（AGENTS.md §1）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Oid` | `oid` | form | `int64` | 是 | — | — |

### `EngagementFavStateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementFavStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamIsFavoreds`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Oids` | `oids` | form | `[]int64` | 是 | split | — |

### `EngagementFavStatesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementFavStatesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAddFolder`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Name` | `name` | form | `string` | 是 | — | — |
| `Description` | `description` | form | `string` | 否 | — | — |
| `Cover` | `cover` | form | `string` | 否 | — | — |
| `Public` | `public` | form | `int32` | 否 | — | — |

### `EngagementFolderOpResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementFolderOpData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamDelFolder`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tp` | `tp` | form | `int32` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Fid` | `fid` | form | `int64` | 是 | — | — |

### `ParamAddShare`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Type` | `type` | form | `int32` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamHasLike`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Business` | `business` | form | `string` | 是 | — | — |
| `MessageIds` | `message_ids` | form | `[]int64` | 是 | split | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `EngagementHasLikeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementHasLikeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUserLikes`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Business` | `business` | form | `string` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `EngagementUserLikesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EngagementUserLikesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `EngagementLikeData`

> engagement 域响应数据载荷

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OriginId` | `origin_id` | json | `int64` | 是 | — | — |
| `MessageId` | `message_id` | json | `int64` | 是 | — | — |
| `LikeNumber` | `like_number` | json | `int64` | 是 | — | — |
| `DislikeNumber` | `dislike_number` | json | `int64` | 是 | — | — |

### `EngagementStatsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Stats` | `stats` | json | `map[int64]EngagementStatState` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `EngagementFoldersData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Folders` | `folders` | json | `[]EngagementFolder` | 是 | — | — |

### `EngagementFavStateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Faved` | `faved` | json | `bool` | 是 | — | — |

### `EngagementFavStatesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Faveds` | `faveds` | json | `map[int64]bool` | 是 | — | — |

### `EngagementFolderOpData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Fid` | `fid` | json | `int64` | 是 | — | — |
| `Shares` | `shares` | json | `int64` | 是 | — | — |

### `EngagementHasLikeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `States` | `states` | json | `map[int64]EngagementLikeStateData` | 是 | — | — |

### `EngagementUserLikesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Items` | `items` | json | `[]EngagementLikeItem` | 是 | — | — |

### `EngagementStatState`

> 点赞计数与状态（对应 engagement.StatState）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OriginId` | `origin_id` | json | `int64` | 是 | — | — |
| `MessageId` | `message_id` | json | `int64` | 是 | — | — |
| `LikeNumber` | `like_number` | json | `int64` | 是 | — | — |
| `DislikeNumber` | `dislike_number` | json | `int64` | 是 | — | — |
| `LikeState` | `like_state` | json | `int32` | 是 | — | — |

### `EngagementFolder`

> 收藏夹（对应 engagement.Folder）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Fid` | `fid` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `Cover` | `cover` | json | `string` | 是 | — | — |
| `Public` | `public` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `Count` | `count` | json | `int32` | 是 | — | — |

### `EngagementLikeStateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Time` | `time` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |

### `EngagementLikeItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MessageId` | `message_id` | json | `int64` | 是 | — | — |
| `Time` | `time` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/13-engagement.md -->
