# 运营面 · `/admin/search`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| search 域运营路由 | 免鉴权 | 3 |
| search 域写入口（受 AdminPermission 保护） | AdminPermission | 2 |

合计 **5** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## search 域运营路由（免鉴权，3 条）

> 索引重建与别名切换是零停机重建的关键步骤（AGENTS.md §5：索引是投影，不是事实源）。
> 网关不写索引、不直连 OpenSearch，只转发到 search-indexer RPC。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/search/rebuild/:task_id` | 查询单个重建任务进度 | `getRebuildTask` | `getrebuildtasklogic.go` |
| GET | `/admin/search/rebuilds` | 分页查询重建任务（cursor + state 过滤，limit 上限 100） | `listRebuildTasks` | `listrebuildtaskslogic.go` |
| GET | `/admin/search/health` | 查询索引/别名健康与重试、死信积压 | `getIndexHealth` | `getindexhealthlogic.go` |

### GET `/admin/search/rebuild/:task_id` — 查询单个重建任务进度

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getrebuildtaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/getrebuildtasklogic.go`

请求：`ParamRebuildTaskId`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | path | `string` | 是 | — | — |

响应：`SearchRebuildTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchRebuildTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/search/rebuilds` — 分页查询重建任务（cursor + state 过滤，limit 上限 100）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listrebuildtaskshandler.go`
- 业务实现：`gateway/admin/internal/logic/listrebuildtaskslogic.go`

请求：`ParamListRebuildTasks`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | form | `string` | 否 | — | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `Limit` | `limit` | form | `int32` | 是 | default=20 | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

响应：`SearchRebuildTasksResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchRebuildTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/search/health` — 查询索引/别名健康与重试、死信积压

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getindexhealthhandler.go`
- 业务实现：`gateway/admin/internal/logic/getindexhealthlogic.go`

请求：`ParamIndexHealth`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Alias` | `alias` | form | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

响应：`SearchIndexHealthResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchIndexHealthData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## search 域写入口（受 AdminPermission 保护）（AdminPermission，2 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/search/rebuild` | 提交索引重建任务（request_id 幂等，scope full/partition/content_type） | `search:index` / `rebuild` | `submitRebuildTask` | `submitrebuildtasklogic.go` |
| POST | `/admin/search/alias/switch` | 切换查询别名到新版本索引（expected_current 乐观校验） | `search:alias` / `switch` | `switchAlias` | `switchaliaslogic.go` |

### POST `/admin/search/rebuild` — 提交索引重建任务（request_id 幂等，scope full/partition/content_type）

- 权限口径：AdminPermission · 权限点 `search:index` / `rebuild`
- goctl 入口：`gateway/admin/internal/handler/submitrebuildtaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/submitrebuildtasklogic.go`

请求：`ParamSubmitRebuildTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | json | `string` | 是 | — | — |
| `ScopeValue` | `scope_value` | json | `string` | 否 | — | — |
| `Alias` | `alias` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |

响应：`SearchRebuildSubmitResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchRebuildSubmitData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/search/alias/switch` — 切换查询别名到新版本索引（expected_current 乐观校验）

- 权限口径：AdminPermission · 权限点 `search:alias` / `switch`
- goctl 入口：`gateway/admin/internal/handler/switchaliashandler.go`
- 业务实现：`gateway/admin/internal/logic/switchaliaslogic.go`

请求：`ParamSwitchAlias`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Alias` | `alias` | json | `string` | 否 | — | — |
| `TargetIndex` | `target_index` | json | `string` | 是 | — | — |
| `ExpectedCurrent` | `expected_current` | json | `string` | 否 | — | — |
| `SkipHealthCheck` | `skip_health_check` | json | `bool` | 否 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |

响应：`SearchSwitchAliasResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchSwitchAliasData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamRebuildTaskId`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | path | `string` | 是 | — | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

### `SearchRebuildTaskResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchRebuildTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListRebuildTasks`

> limit 上限 100、缺省 20（与 search-indexer 任务表查询口径一致）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | form | `string` | 否 | — | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `Limit` | `limit` | form | `int32` | 是 | default=20 | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

### `SearchRebuildTasksResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchRebuildTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamIndexHealth`

> alias 为空表示返回全部已登记别名

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Alias` | `alias` | form | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | form | `int64` | 是 | — | — |

### `SearchIndexHealthResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchIndexHealthData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSubmitRebuildTask`

> search 域请求参数 / scope=partition 时 scope_value 是 typeid 区间 "1000-1999"； / scope=content_type 时 scope_value 是内容类型枚举值 1 UGC / 2 PGC 剧集 / 3 直播间； / alias 为空表示使用服务端默认查询别名；request_id 是幂等键（必填，重复提交返回同一 task_id）； / operator 是写入任务表的提交人字符串，operator_id 是管理后台登录账号 ID（网关审计主体）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | json | `string` | 是 | — | — |
| `ScopeValue` | `scope_value` | json | `string` | 否 | — | — |
| `Alias` | `alias` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |

### `SearchRebuildSubmitResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchRebuildSubmitData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSwitchAlias`

> expected_current 为空表示首次挂载别名；skip_health_check 仅供紧急回滚使用

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Alias` | `alias` | json | `string` | 否 | — | — |
| `TargetIndex` | `target_index` | json | `string` | 是 | — | — |
| `ExpectedCurrent` | `expected_current` | json | `string` | 否 | — | — |
| `SkipHealthCheck` | `skip_health_check` | json | `bool` | 否 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |

### `SearchSwitchAliasResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchSwitchAliasData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `SearchRebuildTaskData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `SearchRebuildTaskItem` | 是 | — | — |

### `SearchRebuildTasksData`

> 重建任务列表：cursor 风格分页，next_cursor 为空表示已到末页

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Tasks` | `tasks` | json | `[]SearchRebuildTaskItem` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |

### `SearchIndexHealthData`

> 索引健康总览：overall_state ok / degraded / down

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aliases` | `aliases` | json | `[]SearchAliasStatusItem` | 是 | — | — |
| `RetryPending` | `retry_pending` | json | `int64` | 是 | — | — |
| `DeadLetter` | `dead_letter` | json | `int64` | 是 | — | — |
| `OverallState` | `overall_state` | json | `string` | 是 | — | — |

### `SearchRebuildSubmitData`

> 提交重建任务结果：duplicated=true 表示命中 request_id 幂等，返回的是既有任务

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `string` | 是 | — | — |
| `TargetIndex` | `target_index` | json | `string` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |

### `SearchSwitchAliasData`

> 别名切换结果（与 searchindexer.v1.SwitchAliasReply 对齐） / record_state：active / history

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Alias` | `alias` | json | `string` | 是 | — | — |
| `PreviousIndex` | `previous_index` | json | `string` | 是 | — | — |
| `CurrentIndex` | `current_index` | json | `string` | 是 | — | — |
| `DocCount` | `doc_count` | json | `int64` | 是 | — | — |
| `RecordState` | `record_state` | json | `string` | 是 | — | — |

### `SearchRebuildTaskItem`

> 重建任务（与 searchindexer.v1.RebuildTask 对齐） / scope：full / partition / content_type； / state：pending / running / succeeded / failed / canceled

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 是 | — | — |
| `ScopeValue` | `scope_value` | json | `string` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |
| `CursorValue` | `cursor_value` | json | `string` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Processed` | `processed` | json | `int64` | 是 | — | — |
| `Failed` | `failed` | json | `int64` | 是 | — | — |
| `TargetIndex` | `target_index` | json | `string` | 是 | — | — |
| `Alias` | `alias` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `StartedAt` | `started_at` | json | `int64` | 是 | — | — |
| `FinishedAt` | `finished_at` | json | `int64` | 是 | — | — |
| `LastError` | `last_error` | json | `string` | 是 | — | — |
| `DlqCount` | `dlq_count` | json | `int64` | 是 | — | — |

### `SearchAliasStatusItem`

> 单个别名健康状态（与 searchindexer.v1.AliasStatus 对齐） / health：green / yellow / red / missing；state：active / retiring / history； / doc_count 读不到时为 -1

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Alias` | `alias` | json | `string` | 是 | — | — |
| `ActiveIndex` | `active_index` | json | `string` | 是 | — | — |
| `SchemaVersion` | `schema_version` | json | `string` | 是 | — | — |
| `DocCount` | `doc_count` | json | `int64` | 是 | — | — |
| `IndexExists` | `index_exists` | json | `bool` | 是 | — | — |
| `Health` | `health` | json | `string` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |


<!-- file: docs/api/http/admin/11-admin-search.md -->
