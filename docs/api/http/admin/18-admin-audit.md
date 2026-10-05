# 运营面 · `/admin/audit`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| audit 域运营路由（services/audit/rpc/audit.proto） | 免鉴权 | 7 |
| audit 域运营路由（services/audit/rpc/audit.proto） | AdminPermission | 4 |

合计 **11** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## audit 域运营路由（services/audit/rpc/audit.proto）（免鉴权，7 条）

> 审计检索与自证是只读能力：查询维度、时间跨度、分页上限全部由 audit 服务判定，
> 网关只做主体存在性与分页归一，并把每次读取按 audit 侧的 data_access 自审计留痕。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/audit/entry/list` | 分页检索审计条目（时间范围 + 至少一个收窄维度，由 audit 强制） | `auditListEntries` | `auditlistentrieslogic.go` |
| POST | `/admin/audit/entry/get` | 按 entry_id 或 event_id 取单条审计（found=false 表示不存在，不报 NotFound） | `auditGetEntry` | `auditgetentrylogic.go` |
| POST | `/admin/audit/chain/verify` | 哈希链完整性自证（按 chain_key + seq 区间重放，可增量续验） | `auditVerifyChain` | `auditverifychainlogic.go` |
| POST | `/admin/audit/export/get` | 查询导出任务（含短期签名下载地址与到期时间） | `auditGetExport` | `auditgetexportlogic.go` |
| POST | `/admin/audit/export/list` | 分页查询导出任务（operator/state/创建时间过滤） | `auditListExports` | `auditlistexportslogic.go` |
| POST | `/admin/audit/retention/list` | 分页查询保留期策略（state=0 表示全部） | `auditListRetention` | `auditlistretentionlogic.go` |
| POST | `/admin/audit/archive/list` | 分页查询归档批次（chain_key/state/时间过滤） | `auditListArchives` | `auditlistarchiveslogic.go` |

### POST `/admin/audit/entry/list` — 分页检索审计条目（时间范围 + 至少一个收窄维度，由 audit 强制）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/auditlistentrieshandler.go`
- 业务实现：`gateway/admin/internal/logic/auditlistentrieslogic.go`

请求：`ParamAuditListEntries`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `ActorType` | `actor_type` | json | `int32` | 否 | — | — |
| `ActorId` | `actor_id` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `string` | 否 | — | — |
| `ActionDomain` | `action_domain` | json | `string` | 否 | — | — |
| `TargetType` | `target_type` | json | `string` | 否 | — | — |
| `TargetId` | `target_id` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `Result` | `result` | json | `int32` | 否 | — | — |
| `SourceApp` | `source_app` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`AuditEntriesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditEntriesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/audit/entry/get` — 按 entry_id 或 event_id 取单条审计（found=false 表示不存在，不报 NotFound）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/auditgetentryhandler.go`
- 业务实现：`gateway/admin/internal/logic/auditgetentrylogic.go`

请求：`ParamAuditGetEntry`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `EntryId` | `entry_id` | json | `int64` | 否 | — | — |
| `EventId` | `event_id` | json | `string` | 否 | — | — |

响应：`AuditEntryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditEntryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/audit/chain/verify` — 哈希链完整性自证（按 chain_key + seq 区间重放，可增量续验）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/auditverifychainhandler.go`
- 业务实现：`gateway/admin/internal/logic/auditverifychainlogic.go`

请求：`ParamAuditVerifyChain`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `ChainKey` | `chain_key` | json | `string` | 是 | — | — |
| `FromSeq` | `from_seq` | json | `int64` | 否 | — | — |
| `ToSeq` | `to_seq` | json | `int64` | 否 | — | — |
| `MaxEntries` | `max_entries` | json | `int32` | 否 | — | — |

响应：`AuditChainVerifyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditChainVerifyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/audit/export/get` — 查询导出任务（含短期签名下载地址与到期时间）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/auditgetexporthandler.go`
- 业务实现：`gateway/admin/internal/logic/auditgetexportlogic.go`

请求：`ParamAuditGetExport`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

响应：`AuditExportDetailResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditExportDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/audit/export/list` — 分页查询导出任务（operator/state/创建时间过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/auditlistexportshandler.go`
- 业务实现：`gateway/admin/internal/logic/auditlistexportslogic.go`

请求：`ParamAuditListExports`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `string` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`AuditExportsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditExportsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/audit/retention/list` — 分页查询保留期策略（state=0 表示全部）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/auditlistretentionhandler.go`
- 业务实现：`gateway/admin/internal/logic/auditlistretentionlogic.go`

请求：`ParamAuditListRetention`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | 0 表示全部 |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`AuditRetentionPoliciesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditRetentionPoliciesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/audit/archive/list` — 分页查询归档批次（chain_key/state/时间过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/auditlistarchiveshandler.go`
- 业务实现：`gateway/admin/internal/logic/auditlistarchiveslogic.go`

请求：`ParamAuditListArchives`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `ChainKey` | `chain_key` | json | `string` | 否 | — | — |
| `State` | `state` | json | `string` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`AuditArchivesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditArchivesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## audit 域运营路由（services/audit/rpc/audit.proto）（AdminPermission，4 条）

> 审计写操作：导出申请、导出推进、保留策略变更、归档触发。
> request_id 必填（audit 用它做写入幂等），operator_id 以中间件会话身份为准。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/audit/export/create` | 提交审计导出任务（request_id 幂等，导出不走同步大查询） | `audit:export` / `create` | `auditCreateExport` | `auditcreateexportlogic.go` |
| POST | `/admin/audit/export/run` | 手动推进一个导出任务（正常由 services/cron 驱动，这里是运营兜底） | `audit:export` / `run` | `auditRunExport` | `auditrunexportlogic.go` |
| POST | `/admin/audit/retention/save` | 新建/更新保留期策略（expect_version 乐观锁，0 表示新建） | `audit:retention` / `update` | `auditSaveRetention` | `auditsaveretentionlogic.go` |
| POST | `/admin/audit/archive/run` | 归档一条哈希链的指定区间（先落 manifest 再标记热表，不物理删除） | `audit:archive` / `create` | `auditArchive` | `auditarchivelogic.go` |

### POST `/admin/audit/export/create` — 提交审计导出任务（request_id 幂等，导出不走同步大查询）

- 权限口径：AdminPermission · 权限点 `audit:export` / `create`
- goctl 入口：`gateway/admin/internal/handler/auditcreateexporthandler.go`
- 业务实现：`gateway/admin/internal/logic/auditcreateexportlogic.go`

请求：`ParamAuditCreateExport`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `ActorType` | `actor_type` | json | `int32` | 否 | — | — |
| `ActorId` | `actor_id` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `string` | 否 | — | — |
| `ActionDomain` | `action_domain` | json | `string` | 否 | — | — |
| `TargetType` | `target_type` | json | `string` | 否 | — | — |
| `TargetId` | `target_id` | json | `string` | 否 | — | — |
| `Format` | `format` | json | `string` | 否 | — | 空由 audit 默认 csv |
| `Reason` | `reason` | json | `string` | 是 | — | 导出动机，进 filter_json 与任务本体 |

响应：`AuditExportTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditExportTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/audit/export/run` — 手动推进一个导出任务（正常由 services/cron 驱动，这里是运营兜底）

- 权限口径：AdminPermission · 权限点 `audit:export` / `run`
- goctl 入口：`gateway/admin/internal/handler/auditrunexporthandler.go`
- 业务实现：`gateway/admin/internal/logic/auditrunexportlogic.go`

请求：`ParamAuditRunExport`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `BatchRows` | `batch_rows` | json | `int32` | 否 | — | <=0 由 audit 按 AuditExport.BatchRows 默认 |

响应：`AuditExportRunResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditExportRunData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/audit/retention/save` — 新建/更新保留期策略（expect_version 乐观锁，0 表示新建）

- 权限口径：AdminPermission · 权限点 `audit:retention` / `update`
- goctl 入口：`gateway/admin/internal/handler/auditsaveretentionhandler.go`
- 业务实现：`gateway/admin/internal/logic/auditsaveretentionlogic.go`

请求：`ParamAuditSaveRetention`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `ActionDomain` | `action_domain` | json | `string` | 是 | — | — |
| `HotDays` | `hot_days` | json | `int32` | 是 | — | — |
| `ArchiveAfterDays` | `archive_after_days` | json | `int32` | 是 | — | — |
| `DeleteAfterDays` | `delete_after_days` | json | `int32` | 是 | — | 0 表示永久保留 |
| `State` | `state` | json | `int32` | 否 | — | 0 由 audit 视为 1 |
| `ExpectVersion` | `expect_version` | json | `int64` | 是 | — | 0 新建，非 0 乐观锁更新 |
| `Remark` | `remark` | json | `string` | 是 | — | — |

响应：`AuditRetentionPolicyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditRetentionPolicyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/audit/archive/run` — 归档一条哈希链的指定区间（先落 manifest 再标记热表，不物理删除）

- 权限口径：AdminPermission · 权限点 `audit:archive` / `create`
- goctl 入口：`gateway/admin/internal/handler/auditarchivehandler.go`
- 业务实现：`gateway/admin/internal/logic/auditarchivelogic.go`

请求：`ParamAuditArchive`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `ChainKey` | `chain_key` | json | `string` | 是 | — | — |
| `FromSeq` | `from_seq` | json | `int64` | 否 | — | — |
| `ToSeq` | `to_seq` | json | `int64` | 否 | — | — |
| `PurgeHot` | `purge_hot` | json | `bool` | 否 | — | 只标记 archived_at，不做物理删除 |

响应：`AuditArchiveBatchResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditArchiveBatchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamAuditListEntries`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `ActorType` | `actor_type` | json | `int32` | 否 | — | — |
| `ActorId` | `actor_id` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `string` | 否 | — | — |
| `ActionDomain` | `action_domain` | json | `string` | 否 | — | — |
| `TargetType` | `target_type` | json | `string` | 否 | — | — |
| `TargetId` | `target_id` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `Result` | `result` | json | `int32` | 否 | — | — |
| `SourceApp` | `source_app` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `AuditEntriesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditEntriesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditGetEntry`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `EntryId` | `entry_id` | json | `int64` | 否 | — | — |
| `EventId` | `event_id` | json | `string` | 否 | — | — |

### `AuditEntryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditEntryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditVerifyChain`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `ChainKey` | `chain_key` | json | `string` | 是 | — | — |
| `FromSeq` | `from_seq` | json | `int64` | 否 | — | — |
| `ToSeq` | `to_seq` | json | `int64` | 否 | — | — |
| `MaxEntries` | `max_entries` | json | `int32` | 否 | — | — |

### `AuditChainVerifyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditChainVerifyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditGetExport`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

### `AuditExportDetailResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditExportDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditListExports`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `string` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `AuditExportsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditExportsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditListRetention`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | 0 表示全部 |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `AuditRetentionPoliciesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditRetentionPoliciesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditListArchives`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `ChainKey` | `chain_key` | json | `string` | 否 | — | — |
| `State` | `state` | json | `string` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `AuditArchivesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditArchivesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditCreateExport`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `ActorType` | `actor_type` | json | `int32` | 否 | — | — |
| `ActorId` | `actor_id` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `string` | 否 | — | — |
| `ActionDomain` | `action_domain` | json | `string` | 否 | — | — |
| `TargetType` | `target_type` | json | `string` | 否 | — | — |
| `TargetId` | `target_id` | json | `string` | 否 | — | — |
| `Format` | `format` | json | `string` | 否 | — | 空由 audit 默认 csv |
| `Reason` | `reason` | json | `string` | 是 | — | 导出动机，进 filter_json 与任务本体 |

### `AuditExportTaskResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditExportTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditRunExport`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `BatchRows` | `batch_rows` | json | `int32` | 否 | — | <=0 由 audit 按 AuditExport.BatchRows 默认 |

### `AuditExportRunResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditExportRunData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditSaveRetention`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `ActionDomain` | `action_domain` | json | `string` | 是 | — | — |
| `HotDays` | `hot_days` | json | `int32` | 是 | — | — |
| `ArchiveAfterDays` | `archive_after_days` | json | `int32` | 是 | — | — |
| `DeleteAfterDays` | `delete_after_days` | json | `int32` | 是 | — | 0 表示永久保留 |
| `State` | `state` | json | `int32` | 否 | — | 0 由 audit 视为 1 |
| `ExpectVersion` | `expect_version` | json | `int64` | 是 | — | 0 新建，非 0 乐观锁更新 |
| `Remark` | `remark` | json | `string` | 是 | — | — |

### `AuditRetentionPolicyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditRetentionPolicyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAuditArchive`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `AuditCallContext` | 是 | — | — |
| `ChainKey` | `chain_key` | json | `string` | 是 | — | — |
| `FromSeq` | `from_seq` | json | `int64` | 否 | — | — |
| `ToSeq` | `to_seq` | json | `int64` | 否 | — | — |
| `PurgeHot` | `purge_hot` | json | `bool` | 否 | — | 只标记 archived_at，不做物理删除 |

### `AuditArchiveBatchResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AuditArchiveBatchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `AuditCallContext`

> audit 是「追加式不可抵赖存证」（哈希链 + 归档批次），与 operation.op_audit_index 的 / 后台轻量索引分工不同：前者由拥有业务动作的领域服务写入并长期保存，后者只回答 / 「哪个管理员在后台点了什么」。字段口径逐项对齐 audit.v1.*，网关不发明 audit 没有的方法： /   * AppendAudit/BatchAppendAudit 不在后台开放——proto 明确「同一个 event_id 只允许一个所有者写」， /     后台补写审计会破坏不可抵赖性； /   * AuditEntryView 只回传 ip_hash/device_hash，明文 IP 与设备号不出审计服务也不进网关； /   * 查询的时间跨度上限（AuditQuery.MaxRangeDays）与「必须带收窄维度」由 audit 判定， /     网关只回读 max_range_seconds 供前端自我修正，不另算一套阈值（AGENTS.md §5）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `CallerService` | `caller_service` | json | `string` | 否 | — | 留空由网关固定为 gateway/admin |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Ip` | `ip` | json | `string` | 否 | — | — |
| `UserAgent` | `user_agent` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | 写接口必填（audit 侧幂等与审计关联键） |

### `AuditEntriesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Entries` | `entries` | json | `[]AuditEntryItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |
| `MaxRangeSeconds` | `max_range_seconds` | json | `int64` | 是 | — | — |

### `AuditEntryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Entry` | `entry` | json | `AuditEntryItem` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |

### `AuditChainVerifyData`

> 哈希链校验结果（broken_reason: seq_gap/prev_hash_mismatch/entry_hash_mismatch/truncated； / truncated=true 表示本次没验完，需要用 last_entry_hash 续验）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Intact` | `intact` | json | `bool` | 是 | — | — |
| `Checked` | `checked` | json | `int64` | 是 | — | — |
| `FirstBrokenSeq` | `first_broken_seq` | json | `int64` | 是 | — | — |
| `FirstBrokenEntryId` | `first_broken_entry_id` | json | `int64` | 是 | — | — |
| `BrokenReason` | `broken_reason` | json | `string` | 是 | — | — |
| `LastEntryHash` | `last_entry_hash` | json | `string` | 是 | — | — |
| `Truncated` | `truncated` | json | `bool` | 是 | — | — |

### `AuditExportDetailData`

> download_url 仅在 state=succeeded 且未过期时由 audit 签发短期地址，否则为空串； / 网关不缓存也不改写它，客户端要按 url_expire_at 自行重新申请（AGENTS.md §6）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `AuditExportTaskItem` | 是 | — | — |
| `DownloadUrl` | `download_url` | json | `string` | 是 | — | — |
| `UrlExpireAt` | `url_expire_at` | json | `int64` | 是 | — | — |

### `AuditExportsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]AuditExportTaskItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `AuditRetentionPoliciesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]AuditRetentionPolicyItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `AuditArchivesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]AuditArchiveBatchItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `AuditExportTaskData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `AuditExportTaskItem` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |

### `AuditExportRunData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `AuditExportTaskItem` | 是 | — | — |
| `ExportedRows` | `exported_rows` | json | `int64` | 是 | — | — |
| `Finished` | `finished` | json | `bool` | 是 | — | — |

### `AuditRetentionPolicyData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Policy` | `policy` | json | `AuditRetentionPolicyItem` | 是 | — | — |

### `AuditArchiveBatchData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Batch` | `batch` | json | `AuditArchiveBatchItem` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |

### `AuditEntryItem`

> 审计条目投影（对齐 audit.v1.AuditEntryView，27 字段全量透传； / actor_type/result/source_app 是 proto 枚举的数值，前端按 audit.proto 注释解释）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EntryId` | `entry_id` | json | `int64` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `SchemaVersion` | `schema_version` | json | `int32` | 是 | — | — |
| `ChainKey` | `chain_key` | json | `string` | 是 | — | — |
| `Seq` | `seq` | json | `int64` | 是 | — | — |
| `ActorType` | `actor_type` | json | `int32` | 是 | — | — |
| `ActorId` | `actor_id` | json | `int64` | 是 | — | — |
| `ActorName` | `actor_name` | json | `string` | 是 | — | — |
| `Action` | `action` | json | `string` | 是 | — | — |
| `ActionDomain` | `action_domain` | json | `string` | 是 | — | — |
| `TargetType` | `target_type` | json | `string` | 是 | — | — |
| `TargetId` | `target_id` | json | `string` | 是 | — | — |
| `Result` | `result` | json | `int32` | 是 | — | — |
| `BeforeDigest` | `before_digest` | json | `string` | 是 | — | — |
| `AfterDigest` | `after_digest` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `SourceApp` | `source_app` | json | `int32` | 是 | — | — |
| `IpHash` | `ip_hash` | json | `string` | 是 | — | — |
| `DeviceHash` | `device_hash` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `OccurredAt` | `occurred_at` | json | `int64` | 是 | — | — |
| `PrevHash` | `prev_hash` | json | `string` | 是 | — | — |
| `EntryHash` | `entry_hash` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `ArchivedAt` | `archived_at` | json | `int64` | 是 | — | — |
| `CallerService` | `caller_service` | json | `string` | 是 | — | — |

### `AuditExportTaskItem`

> 导出任务投影（对齐 audit.v1.AuditExportTask；state: / pending/running/succeeded/failed/expired/canceled，bucket/object_key 只是对象存储引用）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `CallerService` | `caller_service` | json | `string` | 是 | — | — |
| `FilterJson` | `filter_json` | json | `string` | 是 | — | — |
| `Format` | `format` | json | `string` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |
| `RowCount` | `row_count` | json | `int64` | 是 | — | — |
| `Bucket` | `bucket` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |
| `ObjectSize` | `object_size` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `FileHash` | `file_hash` | json | `string` | 是 | — | — |
| `ErrMsg` | `err_msg` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `StartedAt` | `started_at` | json | `int64` | 是 | — | — |
| `FinishedAt` | `finished_at` | json | `int64` | 是 | — | — |

### `AuditRetentionPolicyItem`

> 保留期策略投影（对齐 audit.v1.RetentionPolicy；"default" 是未匹配域的回退策略， / delete_after_days=0 表示永久保留；version 是乐观锁版本）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PolicyId` | `policy_id` | json | `int64` | 是 | — | — |
| `ActionDomain` | `action_domain` | json | `string` | 是 | — | — |
| `HotDays` | `hot_days` | json | `int32` | 是 | — | — |
| `ArchiveAfterDays` | `archive_after_days` | json | `int32` | 是 | — | — |
| `DeleteAfterDays` | `delete_after_days` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `AuditArchiveBatchItem`

> 归档批次投影（对齐 audit.v1.ArchiveBatch；state: / pending/writing/verified/purged/failed，manifest_hash 覆盖离开热表前的逐行证据）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BatchId` | `batch_id` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `ChainKey` | `chain_key` | json | `string` | 是 | — | — |
| `FromSeq` | `from_seq` | json | `int64` | 是 | — | — |
| `ToSeq` | `to_seq` | json | `int64` | 是 | — | — |
| `RowCount` | `row_count` | json | `int64` | 是 | — | — |
| `Bucket` | `bucket` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |
| `ManifestHash` | `manifest_hash` | json | `string` | 是 | — | — |
| `LastEntryHash` | `last_entry_hash` | json | `string` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `ErrMsg` | `err_msg` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `FinishedAt` | `finished_at` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/18-admin-audit.md -->
