# 运营面 · `/admin/private-message`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 私信运营面（举报台账 / 处置 / 留存清理） | 免鉴权 | 1 |
| 私信运营面（举报台账 / 处置 / 留存清理） | AdminPermission | 2 |

合计 **3** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 私信运营面（举报台账 / 处置 / 留存清理）（免鉴权，1 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/private-message/report/list` | 举报台账游标翻页（状态/被举报人过滤；读取主体由 operator_mid 承载） | `privateMessageReportList` | `privatemessagereportlistlogic.go` |

### POST `/admin/private-message/report/list` — 举报台账游标翻页（状态/被举报人过滤；读取主体由 operator_mid 承载）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/privatemessagereportlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/privatemessagereportlistlogic.go`

请求：`ParamPrivateMessageReportList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | 0 = 全部状态 |
| `TargetMid` | `target_mid` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | private-message 以此作读取主体，必须 > 0 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`PrivateMessageReportListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PrivateMessageReportListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 私信运营面（举报台账 / 处置 / 留存清理）（AdminPermission，2 条）

> 两条写入口都要求会话身份 + 幂等/审计位：处置靠 idempotency_key 防重复执行，
> 留存清理靠 dry_run 先看清影响面。举报状态机、可处置动作集合、留存窗口下限、
> 批处理上限与「哪些行还不能删」全部由 private-message 判定，网关只转达入参并投影结论。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/private-message/report/handle` | 处置举报（驳回/撤回/转处罚/升级人审；重复提交回首次结论，replayed=true） | `pm:report` / `handle` | `privateMessageReportHandle` | `privatemessagereporthandlelogic.go` |
| POST | `/admin/private-message/retention/purge` | 留存到期清理（先 dry_run 看影响面，purge=true 才真删正文，审计行保留） | `pm:retention` / `purge` | `privateMessageRetentionPurge` | `privatemessageretentionpurgelogic.go` |

### POST `/admin/private-message/report/handle` — 处置举报（驳回/撤回/转处罚/升级人审；重复提交回首次结论，replayed=true）

- 权限口径：AdminPermission · 权限点 `pm:report` / `handle`
- goctl 入口：`gateway/admin/internal/handler/privatemessagereporthandlehandler.go`
- 业务实现：`gateway/admin/internal/logic/privatemessagereporthandlelogic.go`

请求：`ParamPrivateMessageReportHandle`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReportId` | `report_id` | json | `int64` | 是 | — | — |
| `Action` | `action` | json | `int32` | 是 | — | ReportAction：1 驳回 2 撤回 3 转处罚 4 升级人审 |
| `Handler` | `handler` | json | `int64` | 是 | — | — |
| `Note` | `note` | json | `string` | 否 | — | — |
| `WithdrawMessage` | `withdraw_message` | json | `bool` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`PrivateMessageReportHandleResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PrivateMessageReportHandleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/private-message/retention/purge` — 留存到期清理（先 dry_run 看影响面，purge=true 才真删正文，审计行保留）

- 权限口径：AdminPermission · 权限点 `pm:retention` / `purge`
- goctl 入口：`gateway/admin/internal/handler/privatemessageretentionpurgehandler.go`
- 业务实现：`gateway/admin/internal/logic/privatemessageretentionpurgelogic.go`

请求：`ParamPrivateMessagePurge`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BeforeTime` | `before_time` | json | `int64` | 否 | — | 0 = 由服务按留存窗口推算 |
| `BatchLimit` | `batch_limit` | json | `int32` | 否 | — | 0 = 服务端默认值 |
| `DryRun` | `dry_run` | json | `bool` | 否 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 触发者 mid，必须 > 0（cron 走服务侧调用，不经本路由） |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`PrivateMessagePurgeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PrivateMessagePurgeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamPrivateMessageReportList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | 0 = 全部状态 |
| `TargetMid` | `target_mid` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | private-message 以此作读取主体，必须 > 0 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `PrivateMessageReportListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PrivateMessageReportListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPrivateMessageReportHandle`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReportId` | `report_id` | json | `int64` | 是 | — | — |
| `Action` | `action` | json | `int32` | 是 | — | ReportAction：1 驳回 2 撤回 3 转处罚 4 升级人审 |
| `Handler` | `handler` | json | `int64` | 是 | — | — |
| `Note` | `note` | json | `string` | 否 | — | — |
| `WithdrawMessage` | `withdraw_message` | json | `bool` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `PrivateMessageReportHandleResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PrivateMessageReportHandleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPrivateMessagePurge`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BeforeTime` | `before_time` | json | `int64` | 否 | — | 0 = 由服务按留存窗口推算 |
| `BatchLimit` | `batch_limit` | json | `int32` | 否 | — | 0 = 服务端默认值 |
| `DryRun` | `dry_run` | json | `bool` | 否 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 触发者 mid，必须 > 0（cron 走服务侧调用，不经本路由） |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `PrivateMessagePurgeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `PrivateMessagePurgeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `PrivateMessageReportListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]PrivateMessageReport` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |

### `PrivateMessageReportHandleData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReportId` | `report_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | true = 命中幂等键，回的是首次结论 |
| `WithdrawMsgId` | `withdraw_msg_id` | json | `int64` | 是 | — | 0 表示本次没有连带撤回 |

### `PrivateMessagePurgeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ExpiredBefore` | `expired_before` | json | `int64` | 是 | — | 本次生效的留存截止点 |
| `Scanned` | `scanned` | json | `int64` | 是 | — | — |
| `Purged` | `purged` | json | `int64` | 是 | — | dry_run 时恒为 0 |
| `Remaining` | `remaining` | json | `int64` | 是 | — | — |

### `PrivateMessageReport`

> 契约来源 services/private-message/rpc/privatemessage.proto 的四个运营方法。 / 刻意**不开** /verdict：ApplyModerationVerdict 的调用方是 moderation-orchestrator 或 / moderation.result.v1 消费者，人审结论一旦能从后台点出来，就等于多了一条「不用过审核」的 / 结论写入口（与 live-media 的 ReportRetentionResult 同口径：回调只从服务侧来，不给按钮）。 /  / operator_mid / handler 的口径与 live-room/live-media 一致：admin_id 是 op_admin_user 主键、 / operator_mid 是用户 mid，**不是同一个编号空间**，网关不用前者覆盖后者（覆盖等于把处置记到 / 无关用户头上）。两条主体都进网关日志：既能追「谁点的按钮」，也能对上下游台账。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReportId` | `report_id` | json | `int64` | 是 | — | — |
| `ConversationId` | `conversation_id` | json | `int64` | 是 | — | — |
| `MsgId` | `msg_id` | json | `int64` | 是 | — | — |
| `ReporterMid` | `reporter_mid` | json | `int64` | 是 | — | — |
| `TargetMid` | `target_mid` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | 原因码（枚举真值在 private-message） |
| `Description` | `description` | json | `string` | 是 | — | 举报者补充说明 |
| `State` | `state` | json | `int32` | 是 | — | ReportState：1 PENDING 2 HANDLED 3 DISMISSED |
| `AuditTaskId` | `audit_task_id` | json | `int64` | 是 | — | 关联审核任务（0 表示未送审） |
| `Handler` | `handler` | json | `int64` | 是 | — | 处理人（0 表示未处理） |
| `HandleNote` | `handle_note` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/24-admin-private-message.md -->
