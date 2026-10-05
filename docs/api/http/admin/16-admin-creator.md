# 运营面 · `/admin/creator`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| creator 域运营路由（services/creator/rpc/creator.proto） | 免鉴权 | 3 |

合计 **3** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## creator 域运营路由（services/creator/rpc/creator.proto）（免鉴权，3 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/creator/groups` | 查询全部 UP 主特殊分组（按分组 ID 升序投影） | `adminUpGroups` | `adminupgroupslogic.go` |
| GET | `/admin/creator/group/mids` | 分页查询分组下的 UP 主 | `adminUpGroupMids` | `adminupgroupmidslogic.go` |
| GET | `/admin/creator/high-ally-ups` | 查询高能联盟 UP 主签约信息 | `adminHighAllyUps` | `adminhighallyupslogic.go` |

### GET `/admin/creator/groups` — 查询全部 UP 主特殊分组（按分组 ID 升序投影）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/adminupgroupshandler.go`
- 业务实现：`gateway/admin/internal/logic/adminupgroupslogic.go`

请求：无参数体。

响应：`AdminUpGroupsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminUpGroupsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/creator/group/mids` — 分页查询分组下的 UP 主

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/adminupgroupmidshandler.go`
- 业务实现：`gateway/admin/internal/logic/adminupgroupmidslogic.go`

请求：`ParamAdminUpGroupMids`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `GroupId` | `group_id` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=50 | — |

响应：`AdminUpGroupMidsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminUpGroupMidsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/creator/high-ally-ups` — 查询高能联盟 UP 主签约信息

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/adminhighallyupshandler.go`
- 业务实现：`gateway/admin/internal/logic/adminhighallyupslogic.go`

请求：`ParamAdminHighAllyUps`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

响应：`AdminHighAllyUpsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminHighAllyUpsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `AdminUpGroupsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminUpGroupsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAdminUpGroupMids`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `GroupId` | `group_id` | form | `int64` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=50 | — |

### `AdminUpGroupMidsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminUpGroupMidsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAdminHighAllyUps`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | form | `[]int64` | 是 | split | — |

### `AdminHighAllyUpsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminHighAllyUpsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `AdminUpGroupsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Groups` | `groups` | json | `[]AdminUpGroup` | 是 | — | — |

### `AdminUpGroupMidsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mids` | `mids` | json | `[]int64` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `AdminHighAllyUpsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ups` | `ups` | json | `[]AdminSignUpInfo` | 是 | — | — |

### `AdminUpGroup`

> 特殊分组字典、分组名册与高能联盟签约是运营配置； / UP 主自助的关注弹窗开关与身份查询在 gateway/app 暴露。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Tag` | `tag` | json | `string` | 是 | — | — |
| `ShortTag` | `short_tag` | json | `string` | 是 | — | — |
| `FontColor` | `font_color` | json | `string` | 是 | — | — |
| `BgColor` | `bg_color` | json | `string` | 是 | — | — |
| `Note` | `note` | json | `string` | 是 | — | — |

### `AdminSignUpInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `BeginDate` | `begin_date` | json | `int64` | 是 | — | — |
| `EndDate` | `end_date` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/16-admin-creator.md -->
