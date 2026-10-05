# 终端面 · `/social`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| social-graph 域聚合（services/social-graph/rpc/socialgraph.proto） | 免鉴权 | 6 |
| social-graph 增量：黑名单与特别关注 | 免鉴权 | 6 |

合计 **12** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## social-graph 域聚合（services/social-graph/rpc/socialgraph.proto）（免鉴权，6 条）

> social 域路由

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/social/follow` | 关注（幂等：重复不重复计数） | `follow` | `followlogic.go` |
| POST | `/social/unfollow` | 取关（幂等） | `unfollow` | `unfollowlogic.go` |
| GET | `/social/is_following` | 查询 mid 是否关注 owner | `isFollowing` | `isfollowinglogic.go` |
| GET | `/social/following` | mid 的关注列表（分页） | `listFollowing` | `listfollowinglogic.go` |
| GET | `/social/follower` | mid 的粉丝列表（分页） | `listFollower` | `listfollowerlogic.go` |
| GET | `/social/stat` | 查询关注数与粉丝数 | `socialStat` | `socialstatlogic.go` |

### POST `/social/follow` — 关注（幂等：重复不重复计数）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/followhandler.go`
- 业务实现：`gateway/app/internal/logic/followlogic.go`

请求：`ParamFollow`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `FollowerMid` | `follower_mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/social/unfollow` — 取关（幂等）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/unfollowhandler.go`
- 业务实现：`gateway/app/internal/logic/unfollowlogic.go`

请求：`ParamFollow`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `FollowerMid` | `follower_mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/social/is_following` — 查询 mid 是否关注 owner

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/isfollowinghandler.go`
- 业务实现：`gateway/app/internal/logic/isfollowinglogic.go`

请求：`ParamRelation`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Owner` | `owner` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`SocialIsFollowingResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialIsFollowingData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/social/following` — mid 的关注列表（分页）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listfollowinghandler.go`
- 业务实现：`gateway/app/internal/logic/listfollowinglogic.go`

请求：`ParamSocialList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`SocialFollowingResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialFollowingData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/social/follower` — mid 的粉丝列表（分页）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listfollowerhandler.go`
- 业务实现：`gateway/app/internal/logic/listfollowerlogic.go`

请求：`ParamSocialList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`SocialFollowerResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialFollowerData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/social/stat` — 查询关注数与粉丝数

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/socialstathandler.go`
- 业务实现：`gateway/app/internal/logic/socialstatlogic.go`

请求：`ParamSocialStat`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`SocialStatResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialStatData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## social-graph 增量：黑名单与特别关注（免鉴权，6 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/social/black/add` | 拉黑用户（已关注时自动取关） | `addBlack` | `addblacklogic.go` |
| POST | `/social/black/del` | 取消拉黑（幂等） | `delBlack` | `delblacklogic.go` |
| GET | `/social/black/check` | 是否已拉黑 owner | `isBlacked` | `isblackedlogic.go` |
| GET | `/social/black/list` | 本人黑名单列表 | `listBlacks` | `listblackslogic.go` |
| POST | `/social/special/add` | 设为特别关注（须先关注） | `addSpecial` | `addspeciallogic.go` |
| POST | `/social/special/del` | 取消特别关注（幂等） | `delSpecial` | `delspeciallogic.go` |

### POST `/social/black/add` — 拉黑用户（已关注时自动取关）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/addblackhandler.go`
- 业务实现：`gateway/app/internal/logic/addblacklogic.go`

请求：`ParamAddBlack`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `BlackMid` | `black_mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/social/black/del` — 取消拉黑（幂等）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/delblackhandler.go`
- 业务实现：`gateway/app/internal/logic/delblacklogic.go`

请求：`ParamAddBlack`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `BlackMid` | `black_mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/social/black/check` — 是否已拉黑 owner

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/isblackedhandler.go`
- 业务实现：`gateway/app/internal/logic/isblackedlogic.go`

请求：`ParamRelation`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Owner` | `owner` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`SocialIsFollowingResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialIsFollowingData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/social/black/list` — 本人黑名单列表

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listblackshandler.go`
- 业务实现：`gateway/app/internal/logic/listblackslogic.go`

请求：`ParamListBlacks`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`SocialBlacksResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialBlacksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/social/special/add` — 设为特别关注（须先关注）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/addspecialhandler.go`
- 业务实现：`gateway/app/internal/logic/addspeciallogic.go`

请求：`ParamAddSpecial`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `SpecialMid` | `special_mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/social/special/del` — 取消特别关注（幂等）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/delspecialhandler.go`
- 业务实现：`gateway/app/internal/logic/delspeciallogic.go`

请求：`ParamAddSpecial`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `SpecialMid` | `special_mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamFollow`

> social 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `FollowerMid` | `follower_mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRelation`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Owner` | `owner` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `SocialIsFollowingResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialIsFollowingData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSocialList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `SocialFollowingResponse`

> social 域响应信封

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialFollowingData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `SocialFollowerResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialFollowerData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSocialStat`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `SocialStatResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialStatData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAddBlack`

> 拉黑会自动取关（social-graph 语义）；特别关注必须先关注，否则服务侧返回 / ErrSpecialNeedFollow。黑名单可见性影响动态/评论，规则在服务侧落地。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `BlackMid` | `black_mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamListBlacks`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `SocialBlacksResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SocialBlacksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAddSpecial`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `SpecialMid` | `special_mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `SocialIsFollowingData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Following` | `following` | json | `bool` | 是 | — | — |

### `SocialFollowingData`

> social 域响应数据载荷

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Items` | `items` | json | `[]SocialRelationItem` | 是 | — | — |

### `SocialFollowerData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Items` | `items` | json | `[]SocialRelationItem` | 是 | — | — |

### `SocialStatData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Stat` | `stat` | json | `SocialStat` | 是 | — | — |

### `SocialBlacksData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Items` | `items` | json | `[]SocialRelationItem` | 是 | — | — |

### `SocialRelationItem`

> 关系列表项（对应 socialgraph.RelationItem）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Attr` | `attr` | json | `int32` | 是 | — | — |

### `SocialStat`

> 关系统计（对应 socialgraph.StatReply）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Following` | `following` | json | `int64` | 是 | — | — |
| `Follower` | `follower` | json | `int64` | 是 | — | — |
| `Whisper` | `whisper` | json | `int32` | 是 | — | — |


<!-- file: docs/api/http/app/09-social.md -->
