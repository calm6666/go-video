# 运营面 · `/admin/moderation`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| moderation 域运营路由 | 免鉴权 | 3 |
| moderation 域写入口（受 AdminPermission 保护） | AdminPermission | 1 |

合计 **4** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## moderation 域运营路由（免鉴权，3 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/moderation/tasks` | 分页查询审核任务 | `listModerationTasks` | `listmoderationtaskslogic.go` |
| GET | `/admin/moderation/tasks/:task_id` | 查询审核任务详情 | `getModerationTask` | `getmoderationtasklogic.go` |
| GET | `/admin/moderation/results/:task_id` | 查询审核结论（按任务 ID） | `getModerationResult` | `getmoderationresultlogic.go` |

### GET `/admin/moderation/tasks` — 分页查询审核任务

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listmoderationtaskshandler.go`
- 业务实现：`gateway/admin/internal/logic/listmoderationtaskslogic.go`

请求：`ParamListModerationTasks`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ContentType` | `content_type` | form | `int32` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`ModerationTasksResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/moderation/tasks/:task_id` — 查询审核任务详情

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getmoderationtaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/getmoderationtasklogic.go`

请求：`ParamModerationTaskId`

（该类型无字段：空请求 / 空响应。）

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | path | `int64` | 是 | — | — |

响应：`ModerationTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/moderation/results/:task_id` — 查询审核结论（按任务 ID）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getmoderationresulthandler.go`
- 业务实现：`gateway/admin/internal/logic/getmoderationresultlogic.go`

请求：`ParamModerationTaskId`

（该类型无字段：空请求 / 空响应。）

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | path | `int64` | 是 | — | — |

响应：`ModerationResultResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationResultData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## moderation 域写入口（受 AdminPermission 保护）（AdminPermission，1 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/moderation/appeals` | 处理申诉 | `moderation:appeal` / `handle` | `processModerationAppeal` | `processmoderationappeallogic.go` |

### POST `/admin/moderation/appeals` — 处理申诉

- 权限口径：AdminPermission · 权限点 `moderation:appeal` / `handle`
- goctl 入口：`gateway/admin/internal/handler/processmoderationappealhandler.go`
- 业务实现：`gateway/admin/internal/logic/processmoderationappeallogic.go`

请求：`ParamProcessAppeal`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppealId` | `appeal_id` | json | `int64` | 是 | — | — |
| `Handler` | `handler` | json | `int64` | 是 | — | — |
| `FinalVerdict` | `final_verdict` | json | `int32` | 是 | — | — |
| `FinalReason` | `final_reason` | json | `string` | 是 | — | — |

响应：`ModerationAppealResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationAppealData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamListModerationTasks`

> moderation 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ContentType` | `content_type` | form | `int32` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `ModerationTasksResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamModerationTaskId`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | path | `int64` | 是 | — | — |

### `ModerationTaskResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ModerationResultResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationResultData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamProcessAppeal`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppealId` | `appeal_id` | json | `int64` | 是 | — | — |
| `Handler` | `handler` | json | `int64` | 是 | — | — |
| `FinalVerdict` | `final_verdict` | json | `int32` | 是 | — | — |
| `FinalReason` | `final_reason` | json | `string` | 是 | — | — |

### `ModerationAppealResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationAppealData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ModerationTasksData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Tasks` | `tasks` | json | `[]ModerationTaskItem` | 是 | — | — |

### `ModerationTaskData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `ModerationTaskItem` | 是 | — | — |

### `ModerationResultData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Result` | `result` | json | `ModerationResultItem` | 是 | — | — |

### `ModerationAppealData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Appeal` | `appeal` | json | `ModerationAppealItem` | 是 | — | — |

### `ModerationTaskItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `SubmissionId` | `submission_id` | json | `int64` | 是 | — | — |
| `ContentType` | `content_type` | json | `int32` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `UpMid` | `up_mid` | json | `int64` | 是 | — | — |
| `Business` | `business` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | — |

### `ModerationResultItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `Verdict` | `verdict` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `WorkerId` | `worker_id` | json | `int64` | 是 | — | — |
| `Reviewer` | `reviewer` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `ModerationAppealItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppealId` | `appeal_id` | json | `int64` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Content` | `content` | json | `string` | 是 | — | — |
| `FinalVerdict` | `final_verdict` | json | `int32` | 是 | — | — |
| `FinalReason` | `final_reason` | json | `string` | 是 | — | — |
| `Handler` | `handler` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/07-admin-moderation.md -->
