# 运营面 · `/admin/transcode`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| transcode 域运营路由 | 免鉴权 | 3 |
| transcode 域写入口（受 AdminPermission 保护） | AdminPermission | 1 |

合计 **4** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## transcode 域运营路由（免鉴权，3 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/transcode/tasks` | 分页查询转码任务（按 asset_id/state 过滤） | `listTranscodeTasks` | `listtranscodetaskslogic.go` |
| GET | `/admin/transcode/tasks/:task_id` | 查询转码任务详情 | `getTranscodeTask` | `gettranscodetasklogic.go` |
| GET | `/admin/transcode/templates` | 分页查询转码模板 | `listTranscodeTemplates` | `listtranscodetemplateslogic.go` |

### GET `/admin/transcode/tasks` — 分页查询转码任务（按 asset_id/state 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listtranscodetaskshandler.go`
- 业务实现：`gateway/admin/internal/logic/listtranscodetaskslogic.go`

请求：`ParamListTranscodeTasks`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AssetId` | `asset_id` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`TranscodeTasksResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `TranscodeTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/transcode/tasks/:task_id` — 查询转码任务详情

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/gettranscodetaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/gettranscodetasklogic.go`

请求：`ParamTranscodeTaskId`

（该类型无字段：空请求 / 空响应。）

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | path | `int64` | 是 | — | — |

响应：`TranscodeTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `TranscodeTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/transcode/templates` — 分页查询转码模板

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listtranscodetemplateshandler.go`
- 业务实现：`gateway/admin/internal/logic/listtranscodetemplateslogic.go`

请求：`ParamListTranscodeTemplates`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`TranscodeTemplatesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `TranscodeTemplatesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## transcode 域写入口（受 AdminPermission 保护）（AdminPermission，1 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/transcode/templates` | 运营创建转码模板 | `transcode:template` / `create` | `createTranscodeTemplate` | `createtranscodetemplatelogic.go` |

### POST `/admin/transcode/templates` — 运营创建转码模板

- 权限口径：AdminPermission · 权限点 `transcode:template` / `create`
- goctl 入口：`gateway/admin/internal/handler/createtranscodetemplatehandler.go`
- 业务实现：`gateway/admin/internal/logic/createtranscodetemplatelogic.go`

请求：`ParamCreateTranscodeTemplate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Name` | `name` | json | `string` | 是 | — | — |
| `Codec` | `codec` | json | `string` | 是 | — | — |
| `Width` | `width` | json | `int32` | 是 | — | — |
| `Height` | `height` | json | `int32` | 是 | — | — |
| `Bitrate` | `bitrate` | json | `int32` | 是 | — | — |
| `Fps` | `fps` | json | `int32` | 是 | — | — |
| `SegmentSeconds` | `segment_seconds` | json | `int32` | 是 | — | — |

响应：`TranscodeTemplateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `TranscodeTemplateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamListTranscodeTasks`

> transcode 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AssetId` | `asset_id` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `TranscodeTasksResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `TranscodeTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamTranscodeTaskId`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | path | `int64` | 是 | — | — |

### `TranscodeTaskResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `TranscodeTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListTranscodeTemplates`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `TranscodeTemplatesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `TranscodeTemplatesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCreateTranscodeTemplate`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Name` | `name` | json | `string` | 是 | — | — |
| `Codec` | `codec` | json | `string` | 是 | — | — |
| `Width` | `width` | json | `int32` | 是 | — | — |
| `Height` | `height` | json | `int32` | 是 | — | — |
| `Bitrate` | `bitrate` | json | `int32` | 是 | — | — |
| `Fps` | `fps` | json | `int32` | 是 | — | — |
| `SegmentSeconds` | `segment_seconds` | json | `int32` | 是 | — | — |

### `TranscodeTemplateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `TranscodeTemplateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `TranscodeTasksData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Tasks` | `tasks` | json | `[]TranscodeTaskItem` | 是 | — | — |

### `TranscodeTaskData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `TranscodeTaskItem` | 是 | — | — |

### `TranscodeTemplatesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Templates` | `templates` | json | `[]TranscodeTemplateItem` | 是 | — | — |

### `TranscodeTemplateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Template` | `template` | json | `TranscodeTemplateItem` | 是 | — | — |

### `TranscodeTaskItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `AssetId` | `asset_id` | json | `int64` | 是 | — | — |
| `TemplateId` | `template_id` | json | `int64` | 是 | — | — |
| `InputBucket` | `input_bucket` | json | `string` | 是 | — | — |
| `InputKey` | `input_key` | json | `string` | 是 | — | — |
| `OutputBucket` | `output_bucket` | json | `string` | 是 | — | — |
| `OutputKey` | `output_key` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Progress` | `progress` | json | `int32` | 是 | — | — |
| `Errno` | `errno` | json | `int32` | 是 | — | — |
| `ErrMsg` | `err_msg` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `TranscodeTemplateItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TemplateId` | `template_id` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Codec` | `codec` | json | `string` | 是 | — | — |
| `Width` | `width` | json | `int32` | 是 | — | — |
| `Height` | `height` | json | `int32` | 是 | — | — |
| `Bitrate` | `bitrate` | json | `int32` | 是 | — | — |
| `Fps` | `fps` | json | `int32` | 是 | — | — |
| `SegmentSeconds` | `segment_seconds` | json | `int32` | 是 | — | — |


<!-- file: docs/api/http/admin/08-admin-transcode.md -->
