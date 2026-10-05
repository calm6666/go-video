# 运营面 · `/admin/cron`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| cron 域运营路由（只读面） | 免鉴权 | 10 |
| cron 域运营路由（受 AdminPermission 保护） | AdminPermission | 8 |

合计 **18** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## cron 域运营路由（只读面）（免鉴权，10 条）

> 任务定义、执行记录、游标、租约与变更审计的读取，以及调度健康度快照。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/cron/task/list` | 游标分页任务定义（state/task_group/handler 过滤） | `cronListTasks` | `cronlisttaskslogic.go` |
| POST | `/admin/cron/task/get` | 单个任务定义（含 next_fire_at/last_error 与乐观锁 version） | `cronGetTask` | `crongettasklogic.go` |
| POST | `/admin/cron/run/list` | 游标分页执行记录（按 planned_at,run_id 倒序） | `cronListTaskRuns` | `cronlisttaskrunslogic.go` |
| POST | `/admin/cron/run/get` | 单条执行记录（attempt/fence_token/lease_owner 全可见） | `cronGetTaskRun` | `crongettaskrunlogic.go` |
| POST | `/admin/cron/checkpoint/list` | 游标分页增量游标（按 task_key,scope_key 升序） | `cronListCheckpoints` | `cronlistcheckpointslogic.go` |
| POST | `/admin/cron/checkpoint/get` | 单个游标（未推进过返回 found=false 而不是错误） | `cronGetCheckpoint` | `crongetcheckpointlogic.go` |
| POST | `/admin/cron/lease/list` | 游标分页租约（only_expired=true 排查实例崩溃） | `cronListLeases` | `cronlistleaseslogic.go` |
| POST | `/admin/cron/lease/get` | 单个任务租约（fence_token 与 takeover_count 是抢占证据） | `cronGetLease` | `crongetleaselogic.go` |
| POST | `/admin/cron/audit/list` | 游标分页任务变更审计（register/update/pause/resume/disable/trigger/retry/replay） | `cronListTaskAudits` | `cronlisttaskauditslogic.go` |
| POST | `/admin/cron/health/get` | 调度健康度：积压、运行中、退避、近一小时失败、过期租约 | `cronSchedulerHealth` | `cronschedulerhealthlogic.go` |

### POST `/admin/cron/task/list` — 游标分页任务定义（state/task_group/handler 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/cronlisttaskshandler.go`
- 业务实现：`gateway/admin/internal/logic/cronlisttaskslogic.go`

请求：`ParamCronListTasks`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | 0 全部；1 启用、2 暂停、3 停用 |
| `TaskGroup` | `task_group` | json | `string` | 否 | — | — |
| `Handler` | `handler` | json | `string` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | 0 用 cron 的默认值；越界由 cron 回 ErrInvalidPageLimit |

响应：`CronTasksResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/task/get` — 单个任务定义（含 next_fire_at/last_error 与乐观锁 version）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/crongettaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/crongettasklogic.go`

请求：`ParamCronGetTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |

响应：`CronTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/run/list` — 游标分页执行记录（按 planned_at,run_id 倒序）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/cronlisttaskrunshandler.go`
- 业务实现：`gateway/admin/internal/logic/cronlisttaskrunslogic.go`

请求：`ParamCronListTaskRuns`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | 0 全部；1 PENDING…8 SKIPPED |
| `PlannedFrom` | `planned_from` | json | `int64` | 否 | — | — |
| `PlannedTo` | `planned_to` | json | `int64` | 否 | — | 0 表示不限 |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

响应：`CronRunsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronRunsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/run/get` — 单条执行记录（attempt/fence_token/lease_owner 全可见）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/crongettaskrunhandler.go`
- 业务实现：`gateway/admin/internal/logic/crongettaskrunlogic.go`

请求：`ParamCronGetTaskRun`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RunId` | `run_id` | json | `int64` | 是 | — | — |

响应：`CronRunResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronRunData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/checkpoint/list` — 游标分页增量游标（按 task_key,scope_key 升序）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/cronlistcheckpointshandler.go`
- 业务实现：`gateway/admin/internal/logic/cronlistcheckpointslogic.go`

请求：`ParamCronListCheckpoints`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

响应：`CronCheckpointsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronCheckpointsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/checkpoint/get` — 单个游标（未推进过返回 found=false 而不是错误）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/crongetcheckpointhandler.go`
- 业务实现：`gateway/admin/internal/logic/crongetcheckpointlogic.go`

请求：`ParamCronGetCheckpoint`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `ScopeKey` | `scope_key` | json | `string` | 否 | — | — |

响应：`CronCheckpointResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronCheckpointData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/lease/list` — 游标分页租约（only_expired=true 排查实例崩溃）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/cronlistleaseshandler.go`
- 业务实现：`gateway/admin/internal/logic/cronlistleaseslogic.go`

请求：`ParamCronListLeases`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 否 | — | — |
| `OnlyExpired` | `only_expired` | json | `bool` | 否 | — | true 只看已过期可抢占的租约（实例崩溃排查） |
| `Now` | `now` | json | `int64` | 否 | — | 0 由 cron 用服务端当前时间，避免后台时钟不一致得出不同结论 |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

响应：`CronLeasesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronLeasesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/lease/get` — 单个任务租约（fence_token 与 takeover_count 是抢占证据）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/crongetleasehandler.go`
- 业务实现：`gateway/admin/internal/logic/crongetleaselogic.go`

请求：`ParamCronGetLease`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |

响应：`CronLeaseResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronLeaseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/audit/list` — 游标分页任务变更审计（register/update/pause/resume/disable/trigger/retry/replay）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/cronlisttaskauditshandler.go`
- 业务实现：`gateway/admin/internal/logic/cronlisttaskauditslogic.go`

请求：`ParamCronListTaskAudits`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 否 | — | — |
| `Action` | `action` | json | `string` | 否 | — | register/update/pause/resume/disable/trigger/retry/replay |
| `CtimeFrom` | `ctime_from` | json | `int64` | 否 | — | — |
| `CtimeTo` | `ctime_to` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

响应：`CronTaskAuditsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTaskAuditsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/health/get` — 调度健康度：积压、运行中、退避、近一小时失败、过期租约

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/cronschedulerhealthhandler.go`
- 业务实现：`gateway/admin/internal/logic/cronschedulerhealthlogic.go`

请求：`ParamCronSchedulerHealth`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Now` | `now` | json | `int64` | 否 | — | 0 由 cron 用服务端当前时间 |
| `TaskGroup` | `task_group` | json | `string` | 否 | — | 空表示全部分组 |

响应：`CronHealthResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronHealthData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## cron 域运营路由（受 AdminPermission 保护）（AdminPermission，8 条）

> 全部要求 idempotency_key；operator 由会话身份渲染，不接受客户端声明。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/cron/task/register` | 注册任务定义（task_key 唯一；重复注册幂等返回 created=false） | `cron:task` / `create` | `cronRegisterTask` | `cronregistertasklogic.go` |
| POST | `/admin/cron/task/update` | 修改任务定义（expected_version 乐观锁；state 不在此处改） | `cron:task` / `update` | `cronUpdateTask` | `cronupdatetasklogic.go` |
| POST | `/admin/cron/task/pause` | 暂停任务（可恢复；重复暂停 changed=false） | `cron:task` / `pause` | `cronPauseTask` | `cronpausetasklogic.go` |
| POST | `/admin/cron/task/resume` | 恢复任务（暂停期间过期点按 MisfirePolicy 处理） | `cron:task` / `resume` | `cronResumeTask` | `cronresumetasklogic.go` |
| POST | `/admin/cron/task/disable` | 停用任务（终态，保留历史，只能重新注册恢复） | `cron:task` / `disable` | `cronDisableTask` | `crondisabletasklogic.go` |
| POST | `/admin/cron/task/trigger` | 立即触发一次执行，或补跑指定计划时刻 | `cron:task` / `trigger` | `cronTriggerTask` | `crontriggertasklogic.go` |
| POST | `/admin/cron/run/retry` | 人工重试已终结执行（同计划时刻追加 attempt，保持幂等上下文） | `cron:run` / `retry` | `cronRetryRun` | `cronretryrunlogic.go` |
| POST | `/admin/cron/checkpoint/save` | 独立推进增量游标（expected_version CAS；值单调性由处理器保证） | `cron:checkpoint` / `update` | `cronSaveCheckpoint` | `cronsavecheckpointlogic.go` |

### POST `/admin/cron/task/register` — 注册任务定义（task_key 唯一；重复注册幂等返回 created=false）

- 权限口径：AdminPermission · 权限点 `cron:task` / `create`
- goctl 入口：`gateway/admin/internal/handler/cronregistertaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/cronregistertasklogic.go`

请求：`ParamCronRegisterTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `CronTaskDefinition` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | 必填：同一次注册动作唯一 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`CronRegisterTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronRegisterTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/task/update` — 修改任务定义（expected_version 乐观锁；state 不在此处改）

- 权限口径：AdminPermission · 权限点 `cron:task` / `update`
- goctl 入口：`gateway/admin/internal/handler/cronupdatetaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/cronupdatetasklogic.go`

请求：`ParamCronUpdateTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Definition` | `definition` | json | `CronTaskDefinition` | 是 | — | 只有 name/handler/task_group/schedule/timeout/retry/concurrency/lease_ttl/misfire/params/secret_refs/owner 生效 |
| `ExpectedVersion` | `expected_version` | json | `int64` | 是 | — | 乐观锁：回传读到的 version，冲突不静默覆盖 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`CronTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/task/pause` — 暂停任务（可恢复；重复暂停 changed=false）

- 权限口径：AdminPermission · 权限点 `cron:task` / `pause`
- goctl 入口：`gateway/admin/internal/handler/cronpausetaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/cronpausetasklogic.go`

请求：`ParamCronPauseTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | 必填：审计要求可追溯 |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`CronTaskOperationResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTaskOperationData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/task/resume` — 恢复任务（暂停期间过期点按 MisfirePolicy 处理）

- 权限口径：AdminPermission · 权限点 `cron:task` / `resume`
- goctl 入口：`gateway/admin/internal/handler/cronresumetaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/cronresumetasklogic.go`

请求：`ParamCronResumeTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`CronTaskOperationResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTaskOperationData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/task/disable` — 停用任务（终态，保留历史，只能重新注册恢复）

- 权限口径：AdminPermission · 权限点 `cron:task` / `disable`
- goctl 入口：`gateway/admin/internal/handler/crondisabletaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/crondisabletasklogic.go`

请求：`ParamCronDisableTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | 必填：终态操作必须留下原因 |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`CronTaskOperationResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTaskOperationData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/task/trigger` — 立即触发一次执行，或补跑指定计划时刻

- 权限口径：AdminPermission · 权限点 `cron:task` / `trigger`
- goctl 入口：`gateway/admin/internal/handler/crontriggertaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/crontriggertasklogic.go`

请求：`ParamCronTriggerTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Params` | `params` | json | `string` | 否 | — | 覆盖本次执行的参数，空表示用定义里的 params |
| `PlannedAt` | `planned_at` | json | `int64` | 否 | — | 0 表示服务端当前时间；显式传入即补跑某个计划时刻 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`CronTriggerResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTriggerData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/run/retry` — 人工重试已终结执行（同计划时刻追加 attempt，保持幂等上下文）

- 权限口径：AdminPermission · 权限点 `cron:run` / `retry`
- goctl 入口：`gateway/admin/internal/handler/cronretryrunhandler.go`
- 业务实现：`gateway/admin/internal/logic/cronretryrunlogic.go`

请求：`ParamCronRetryRun`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RunId` | `run_id` | json | `int64` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | 必填：人工重试属高危，原因进 cron_task_audit |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`CronRetryRunResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronRetryRunData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/cron/checkpoint/save` — 独立推进增量游标（expected_version CAS；值单调性由处理器保证）

- 权限口径：AdminPermission · 权限点 `cron:checkpoint` / `update`
- goctl 入口：`gateway/admin/internal/handler/cronsavecheckpointhandler.go`
- 业务实现：`gateway/admin/internal/logic/cronsavecheckpointlogic.go`

请求：`ParamCronSaveCheckpoint`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Checkpoint` | `checkpoint` | json | `CronCheckpoint` | 是 | — | 必填 task_key；scope_key 空串表示默认游标；version 由服务端维护 |
| `ExpectedVersion` | `expected_version` | json | `int64` | 是 | — | 0 要求「尚不存在」，>0 走 CAS |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`CronSaveCheckpointResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronSaveCheckpointData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamCronListTasks`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | 0 全部；1 启用、2 暂停、3 停用 |
| `TaskGroup` | `task_group` | json | `string` | 否 | — | — |
| `Handler` | `handler` | json | `string` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | 0 用 cron 的默认值；越界由 cron 回 ErrInvalidPageLimit |

### `CronTasksResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronGetTask`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |

### `CronTaskResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronListTaskRuns`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | 0 全部；1 PENDING…8 SKIPPED |
| `PlannedFrom` | `planned_from` | json | `int64` | 否 | — | — |
| `PlannedTo` | `planned_to` | json | `int64` | 否 | — | 0 表示不限 |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

### `CronRunsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronRunsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronGetTaskRun`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RunId` | `run_id` | json | `int64` | 是 | — | — |

### `CronRunResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronRunData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronListCheckpoints`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

### `CronCheckpointsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronCheckpointsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronGetCheckpoint`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `ScopeKey` | `scope_key` | json | `string` | 否 | — | — |

### `CronCheckpointResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronCheckpointData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronListLeases`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 否 | — | — |
| `OnlyExpired` | `only_expired` | json | `bool` | 否 | — | true 只看已过期可抢占的租约（实例崩溃排查） |
| `Now` | `now` | json | `int64` | 否 | — | 0 由 cron 用服务端当前时间，避免后台时钟不一致得出不同结论 |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

### `CronLeasesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronLeasesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronGetLease`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |

### `CronLeaseResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronLeaseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronListTaskAudits`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 否 | — | — |
| `Action` | `action` | json | `string` | 否 | — | register/update/pause/resume/disable/trigger/retry/replay |
| `CtimeFrom` | `ctime_from` | json | `int64` | 否 | — | — |
| `CtimeTo` | `ctime_to` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

### `CronTaskAuditsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTaskAuditsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronSchedulerHealth`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Now` | `now` | json | `int64` | 否 | — | 0 由 cron 用服务端当前时间 |
| `TaskGroup` | `task_group` | json | `string` | 否 | — | 空表示全部分组 |

### `CronHealthResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronHealthData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronRegisterTask`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `CronTaskDefinition` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | 必填：同一次注册动作唯一 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `CronRegisterTaskResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronRegisterTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronUpdateTask`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Definition` | `definition` | json | `CronTaskDefinition` | 是 | — | 只有 name/handler/task_group/schedule/timeout/retry/concurrency/lease_ttl/misfire/params/secret_refs/owner 生效 |
| `ExpectedVersion` | `expected_version` | json | `int64` | 是 | — | 乐观锁：回传读到的 version，冲突不静默覆盖 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `ParamCronPauseTask`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | 必填：审计要求可追溯 |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `CronTaskOperationResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTaskOperationData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronResumeTask`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `ParamCronDisableTask`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | 必填：终态操作必须留下原因 |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `ParamCronTriggerTask`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Params` | `params` | json | `string` | 否 | — | 覆盖本次执行的参数，空表示用定义里的 params |
| `PlannedAt` | `planned_at` | json | `int64` | 否 | — | 0 表示服务端当前时间；显式传入即补跑某个计划时刻 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `CronTriggerResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronTriggerData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronRetryRun`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RunId` | `run_id` | json | `int64` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | 必填：人工重试属高危，原因进 cron_task_audit |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `CronRetryRunResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronRetryRunData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCronSaveCheckpoint`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Checkpoint` | `checkpoint` | json | `CronCheckpoint` | 是 | — | 必填 task_key；scope_key 空串表示默认游标；version 由服务端维护 |
| `ExpectedVersion` | `expected_version` | json | `int64` | 是 | — | 0 要求「尚不存在」，>0 走 CAS |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `CronSaveCheckpointResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CronSaveCheckpointData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CronTasksData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CronTaskDefinition` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `CronTaskData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `CronTaskDefinition` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | 网关按下游 definition==nil 派生，未命中不是错误 |

### `CronRunsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CronTaskRun` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `CronRunData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Run` | `run` | json | `CronTaskRun` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |

### `CronCheckpointsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CronCheckpoint` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `CronCheckpointData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Checkpoint` | `checkpoint` | json | `CronCheckpoint` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | false 表示从未推进过游标，checkpoint 字段无效 |

### `CronLeasesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CronLease` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `CronLeaseData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Lease` | `lease` | json | `CronLease` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |

### `CronTaskAuditsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CronTaskAudit` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `CronHealthData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ServerTime` | `server_time` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `string` | 是 | — | cron 构建版本，便于排障定位 |
| `Groups` | `groups` | json | `[]CronGroupHealth` | 是 | — | — |

### `CronTaskDefinition`

>  / 口径说明（与 audit/ops-config 两组的差异）： /   - cron 的列表接口是**游标分页**（cursor + page_size，服务端返回 next_cursor/has_more）， /     不是 pn/ps；page_size 的上限与越界判定由 cron 自己的 svcCtx.PageSize 负责并回 /     ErrInvalidPageLimit，网关只做非负校验并原样透传，避免「网关静默截断、服务端以为你要 500 条」 /     这种两边各说一半的情况（AGENTS.md §5）。 /   - cron 契约里没有 CallContext，写接口用 operator(string) + idempotency_key + trace_id： /     operator 由网关用 AdminPermission 会话身份渲染成 "gateway/admin:<admin_id>"， /     不接受客户端声明；idempotency_key 是 proto 注释的必填项，网关拦住空值。 /   - 执行器语义的 5 个方法（ListDueTasks/AcquireLease/RenewLease/ReleaseLease/ReportTaskResult） /     不属于运营面，故意不开 HTTP 入口：它们要求租约所有权与栅栏令牌，由 worker 进程直接调用。 / CronTaskDefinition 是 cron.v1.TaskDefinition 的后台投影；调度参数是否自洽 / （cron/interval/manual 与重试、租约 TTL 下限）由 cron 的 ValidateTaskDefinition 判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Handler` | `handler` | json | `string` | 是 | — | — |
| `TaskGroup` | `task_group` | json | `string` | 否 | — | — |
| `ScheduleType` | `schedule_type` | json | `int32` | 是 | — | — |
| `CronExpr` | `cron_expr` | json | `string` | 否 | — | — |
| `IntervalSeconds` | `interval_seconds` | json | `int32` | 否 | — | — |
| `Timezone` | `timezone` | json | `string` | 否 | — | — |
| `TimeoutSeconds` | `timeout_seconds` | json | `int32` | 否 | — | — |
| `MaxAttempts` | `max_attempts` | json | `int32` | 否 | — | — |
| `RetryBaseSeconds` | `retry_base_seconds` | json | `int32` | 否 | — | — |
| `RetryMaxSeconds` | `retry_max_seconds` | json | `int32` | 否 | — | — |
| `ConcurrencyLimit` | `concurrency_limit` | json | `int32` | 否 | — | — |
| `LeaseTtlSeconds` | `lease_ttl_seconds` | json | `int32` | 否 | — | — |
| `MisfirePolicy` | `misfire_policy` | json | `int32` | 否 | — | — |
| `MisfireBackfillLimit` | `misfire_backfill_limit` | json | `int32` | 否 | — | — |
| `Params` | `params` | json | `string` | 否 | — | — |
| `SecretRefs` | `secret_refs` | json | `string` | 否 | — | 逗号分隔的环境变量名，库里不存密钥值 |
| `State` | `state` | json | `int32` | 否 | — | — |
| `NextFireAt` | `next_fire_at` | json | `int64` | 是 | — | — |
| `LastFireAt` | `last_fire_at` | json | `int64` | 是 | — | — |
| `LastSuccessAt` | `last_success_at` | json | `int64` | 是 | — | — |
| `LastError` | `last_error` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Owner` | `owner` | json | `string` | 否 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `CronRegisterTaskData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `CronTaskDefinition` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | false 表示 task_key 已存在且定义一致（幂等重入） |
| `DedupeReason` | `dedupe_reason` | json | `string` | 是 | — | — |

### `CronTaskOperationData`

> CronTaskOperationData 对应 cron.v1.TaskOperationReply：changed=false 表示目标状态已达成。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `CronTaskDefinition` | 是 | — | — |
| `Changed` | `changed` | json | `bool` | 是 | — | — |
| `AuditId` | `audit_id` | json | `int64` | 是 | — | — |

### `CronTriggerData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Run` | `run` | json | `CronTaskRun` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | — |

### `CronRetryRunData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Run` | `run` | json | `CronTaskRun` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | — |

### `CronCheckpoint`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `ScopeKey` | `scope_key` | json | `string` | 是 | — | — |
| `Value` | `value` | json | `int64` | 是 | — | — |
| `ValueStr` | `value_str` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `CronSaveCheckpointData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Checkpoint` | `checkpoint` | json | `CronCheckpoint` | 是 | — | — |
| `Advanced` | `advanced` | json | `bool` | 是 | — | — |

### `CronTaskRun`

> CronTaskRun 是 cron.v1.RunRecord 的投影。(task_key, planned_at) 是执行身份， / attempt 与 fence_token 必须可见：缺了栅栏令牌就无法判断「被抢占后的僵尸写」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RunId` | `run_id` | json | `int64` | 是 | — | — |
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `PlannedAt` | `planned_at` | json | `int64` | 是 | — | — |
| `Attempt` | `attempt` | json | `int32` | 是 | — | — |
| `TriggerType` | `trigger_type` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `LeaseOwner` | `lease_owner` | json | `string` | 是 | — | — |
| `LeaseExpireAt` | `lease_expire_at` | json | `int64` | 是 | — | — |
| `FenceToken` | `fence_token` | json | `int64` | 是 | — | — |
| `StartedAt` | `started_at` | json | `int64` | 是 | — | — |
| `FinishedAt` | `finished_at` | json | `int64` | 是 | — | — |
| `DurationMs` | `duration_ms` | json | `int64` | 是 | — | — |
| `ResultSummary` | `result_summary` | json | `string` | 是 | — | — |
| `LastError` | `last_error` | json | `string` | 是 | — | — |
| `NextRetryAt` | `next_retry_at` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `CronLease`

> CronLease 是 cron.v1.LeaseInfo 的投影：takeover_count 是实例不稳定性的直接线索。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `LeaseKey` | `lease_key` | json | `string` | 是 | — | — |
| `Owner` | `owner` | json | `string` | 是 | — | — |
| `FenceToken` | `fence_token` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `AcquiredAt` | `acquired_at` | json | `int64` | 是 | — | — |
| `TakeoverCount` | `takeover_count` | json | `int32` | 是 | — | — |

### `CronTaskAudit`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `TaskKey` | `task_key` | json | `string` | 是 | — | — |
| `Action` | `action` | json | `string` | 是 | — | — |
| `FromState` | `from_state` | json | `string` | 是 | — | — |
| `ToState` | `to_state` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Detail` | `detail` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `CronGroupHealth`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskGroup` | `task_group` | json | `string` | 是 | — | — |
| `EnabledTasks` | `enabled_tasks` | json | `int32` | 是 | — | — |
| `PausedTasks` | `paused_tasks` | json | `int32` | 是 | — | — |
| `DueBacklog` | `due_backlog` | json | `int32` | 是 | — | — |
| `Running` | `running` | json | `int32` | 是 | — | — |
| `Retrying` | `retrying` | json | `int32` | 是 | — | — |
| `FailedLastHour` | `failed_last_hour` | json | `int32` | 是 | — | — |
| `ExpiredLeases` | `expired_leases` | json | `int32` | 是 | — | — |
| `OldestDuePlannedAt` | `oldest_due_planned_at` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/20-admin-cron.md -->
