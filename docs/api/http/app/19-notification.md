# 终端面 · `/notification`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| notification 域终端偏好（services/notification/rpc/notification.proto） | 免鉴权 | 2 |

合计 **2** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## notification 域终端偏好（services/notification/rpc/notification.proto）（免鉴权，2 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/notification/dnd` | 查询本人通道偏好与免打扰设置 | `getDndPreference` | `getdndpreferencelogic.go` |
| POST | `/notification/dnd/update` | 更新本人通道偏好与免打扰设置（全量覆盖） | `updateDndPreference` | `updatedndpreferencelogic.go` |

### GET `/notification/dnd` — 查询本人通道偏好与免打扰设置

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getdndpreferencehandler.go`
- 业务实现：`gateway/app/internal/logic/getdndpreferencelogic.go`

请求：`ParamGetDnd`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

响应：`NotifyDndResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyDndData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/notification/dnd/update` — 更新本人通道偏好与免打扰设置（全量覆盖）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updatedndpreferencehandler.go`
- 业务实现：`gateway/app/internal/logic/updatedndpreferencelogic.go`

请求：`ParamUpdateDnd`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `MutedChannels` | `muted_channels` | form | `[]int32` | 否 | — | 全量覆盖语义：留空表示全部允许 |
| `QuietStart` | `quiet_start` | form | `string` | 否 | — | HH:MM，空串表示不设时段 |
| `QuietEnd` | `quiet_end` | form | `string` | 否 | — | — |
| `Timezone` | `timezone` | form | `string` | 否 | — | IANA 时区名 |
| `Enabled` | `enabled` | form | `bool` | 是 | — | — |

响应：`NotifyDndResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyDndData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamGetDnd`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |

### `NotifyDndResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyDndData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUpdateDnd`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `MutedChannels` | `muted_channels` | form | `[]int32` | 否 | — | 全量覆盖语义：留空表示全部允许 |
| `QuietStart` | `quiet_start` | form | `string` | 否 | — | HH:MM，空串表示不设时段 |
| `QuietEnd` | `quiet_end` | form | `string` | 否 | — | — |
| `Timezone` | `timezone` | form | `string` | 否 | — | IANA 时区名 |
| `Enabled` | `enabled` | form | `bool` | 是 | — | — |

### `NotifyDndData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Preference` | `preference` | json | `NotifyDndPreference` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |

### `NotifyDndPreference`

> 终端只暴露"本人通道偏好与免打扰设置"；模板管理、投递记录、死信重投属运营面， / 在 gateway/admin 暴露（AGENTS.md §3/§6）。明文手机号/邮箱不经过本网关。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `MutedChannels` | `muted_channels` | json | `[]int32` | 是 | — | — |
| `QuietStart` | `quiet_start` | json | `string` | 是 | — | — |
| `QuietEnd` | `quiet_end` | json | `string` | 是 | — | — |
| `Timezone` | `timezone` | json | `string` | 是 | — | — |
| `Enabled` | `enabled` | json | `bool` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/19-notification.md -->
