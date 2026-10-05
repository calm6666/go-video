# 运营面 · `/admin/collector`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| event-collector 域运营路由（只读面） | 免鉴权 | 9 |
| event-collector 域运营路由（受 AdminPermission 保护） | AdminPermission | 4 |

合计 **13** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## event-collector 域运营路由（只读面）（免鉴权，9 条）

> 与 audit / ops-config / cron / live / recommend 同一口径：批次与事件台账、死信列表、
> 策略读取与健康度走免中间件路由组——排障页每次刷新都会打一次 RPC，全量挂判定会把
> operation 变成读放大瓶颈；这些读取本身不产生任何写入，也没有需要留痕的处置动作。
> /schema/validate 同组：它是干跑（不落库、不投递、不写台账）。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/collector/schema/validate` | 单事件干跑校验：回缺失字段与归一化 event_type/topic，不落库不投递 | `collectorValidateSchema` | `collectorvalidateschemalogic.go` |
| GET | `/admin/collector/batch` | 按 batch_id 精读接收批次（计数、状态、整批首要拒绝原因） | `collectorBatchGet` | `collectorbatchgetlogic.go` |
| POST | `/admin/collector/batch/list` | 批次台账游标翻页（来源/状态/mid/设备摘要/IP 段/时间窗） | `collectorBatchList` | `collectorbatchlistlogic.go` |
| GET | `/admin/collector/event` | 按 event_id 精读单条事件（投递状态以 Outbox 真值覆盖） | `collectorEventGet` | `collectoreventgetlogic.go` |
| POST | `/admin/collector/event/list` | 事件台账游标翻页（校验结论与投递状态是两个独立维度） | `collectorEventList` | `collectoreventlistlogic.go` |
| POST | `/admin/collector/dead-letter/list` | 投递死信游标翻页（topic/state/时间窗；reason 是稳定枚举） | `collectorDeadLetterList` | `collectordeadletterlistlogic.go` |
| GET | `/admin/collector/policy/active` | 当前生效的采样与脱敏策略（无 ACTIVE 时由服务明确报错，不回空策略） | `collectorPolicyActive` | `collectorpolicyactivelogic.go` |
| GET | `/admin/collector/policy/list` | 策略版本游标翻页（含 ARCHIVED：历史批次的归因依据） | `collectorPolicyList` | `collectorpolicylistlogic.go` |
| GET | `/admin/collector/health` | 采集与投递健康度：积压、限流、盐可用性与生效策略版本 | `collectorHealth` | `collectorhealthlogic.go` |

### POST `/admin/collector/schema/validate` — 单事件干跑校验：回缺失字段与归一化 event_type/topic，不落库不投递

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/collectorvalidateschemahandler.go`
- 业务实现：`gateway/admin/internal/logic/collectorvalidateschemalogic.go`

请求：`ParamCollectorSchemaValidate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | json | `int32` | 否 | — | 0 按 SOURCE_CLIENT 判定，与采集入口同一口径 |
| `EventId` | `event_id` | json | `string` | 否 | — | — |
| `EventType` | `event_type` | json | `string` | 否 | — | — |
| `SchemaVersion` | `schema_version` | json | `int32` | 否 | — | — |
| `OccurredAt` | `occurred_at` | json | `int64` | 否 | — | — |
| `ReportedAt` | `reported_at` | json | `int64` | 否 | — | — |
| `Category` | `category` | json | `int32` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `ContentType` | `content_type` | json | `string` | 否 | — | — |
| `ContentId` | `content_id` | json | `int64` | 否 | — | — |
| `Aid` | `aid` | json | `int64` | 否 | — | — |
| `Vid` | `vid` | json | `string` | 否 | — | — |
| `TargetMid` | `target_mid` | json | `int64` | 否 | — | — |
| `SessionId` | `session_id` | json | `string` | 否 | — | — |
| `PositionMs` | `position_ms` | json | `int64` | 否 | — | — |
| `DurationMs` | `duration_ms` | json | `int64` | 否 | — | — |
| `BufferCount` | `buffer_count` | json | `int32` | 否 | — | — |
| `FirstFrameMs` | `first_frame_ms` | json | `int64` | 否 | — | — |
| `AvgBitrate` | `avg_bitrate` | json | `int32` | 否 | — | — |
| `ErrorCode` | `error_code` | json | `string` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `ResultIndex` | `result_index` | json | `int32` | 否 | — | — |
| `TargetUrl` | `target_url` | json | `string` | 否 | — | — |
| `Payload` | `payload` | json | `string` | 否 | — | 扩展 JSON 文本原文（干跑不回带） |
| `CtxMid` | `ctx_mid` | json | `int64` | 否 | — | — |
| `CtxDeviceId` | `ctx_device_id` | json | `string` | 否 | — | — |
| `CtxDeviceType` | `ctx_device_type` | json | `string` | 否 | — | — |
| `CtxIp` | `ctx_ip` | json | `string` | 否 | — | — |
| `CtxPlatform` | `ctx_platform` | json | `int32` | 否 | — | — |
| `CtxAppId` | `ctx_app_id` | json | `string` | 否 | — | — |
| `CtxAppVersion` | `ctx_app_version` | json | `string` | 否 | — | — |
| `CtxSdkVersion` | `ctx_sdk_version` | json | `string` | 否 | — | — |
| `CtxOsVersion` | `ctx_os_version` | json | `string` | 否 | — | — |
| `CtxNetworkType` | `ctx_network_type` | json | `string` | 否 | — | — |
| `CtxModel` | `ctx_model` | json | `string` | 否 | — | — |
| `CtxRegion` | `ctx_region` | json | `string` | 否 | — | — |
| `CtxSessionId` | `ctx_session_id` | json | `string` | 否 | — | — |
| `CtxPage` | `ctx_page` | json | `string` | 否 | — | — |
| `CtxSpm` | `ctx_spm` | json | `string` | 否 | — | 行为链路标识，非广告位参数（AGENTS.md §7） |

响应：`CollectorSchemaValidateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorSchemaValidateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/collector/batch` — 按 batch_id 精读接收批次（计数、状态、整批首要拒绝原因）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/collectorbatchgethandler.go`
- 业务实现：`gateway/admin/internal/logic/collectorbatchgetlogic.go`

请求：`ParamCollectorBatchGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BatchId` | `batch_id` | form | `string` | 是 | — | — |

响应：`CollectorBatchResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorBatchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/collector/batch/list` — 批次台账游标翻页（来源/状态/mid/设备摘要/IP 段/时间窗）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/collectorbatchlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/collectorbatchlistlogic.go`

请求：`ParamCollectorBatchList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | 0 表示不按 mid 过滤（游客上报的批次 mid 就是 0） |
| `DeviceHash` | `device_hash` | json | `string` | 否 | — | — |
| `IpSegment` | `ip_segment` | json | `string` | 否 | — | 脱敏段，如 203.0.113.0/24 |
| `CtimeFrom` | `ctime_from` | json | `int64` | 否 | — | — |
| `CtimeTo` | `ctime_to` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

响应：`CollectorBatchListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorBatchListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/collector/event` — 按 event_id 精读单条事件（投递状态以 Outbox 真值覆盖）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/collectoreventgethandler.go`
- 业务实现：`gateway/admin/internal/logic/collectoreventgetlogic.go`

请求：`ParamCollectorEventGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventId` | `event_id` | form | `string` | 是 | — | — |

响应：`CollectorEventResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorEventData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/collector/event/list` — 事件台账游标翻页（校验结论与投递状态是两个独立维度）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/collectoreventlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/collectoreventlistlogic.go`

请求：`ParamCollectorEventList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BatchId` | `batch_id` | json | `string` | 否 | — | — |
| `EventType` | `event_type` | json | `string` | 否 | — | — |
| `Category` | `category` | json | `int32` | 否 | — | — |
| `Decision` | `decision` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 否 | — | — |
| `DeliveryState` | `delivery_state` | json | `int32` | 否 | — | — |
| `Topic` | `topic` | json | `string` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `DeviceHash` | `device_hash` | json | `string` | 否 | — | — |
| `CtimeFrom` | `ctime_from` | json | `int64` | 否 | — | — |
| `CtimeTo` | `ctime_to` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

响应：`CollectorEventListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorEventListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/collector/dead-letter/list` — 投递死信游标翻页（topic/state/时间窗；reason 是稳定枚举）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/collectordeadletterlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/collectordeadletterlistlogic.go`

请求：`ParamCollectorDeadLetterList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 否 | — | — |
| `State` | `state` | json | `string` | 否 | — | open/replayed/discarded，空表示全部 |
| `CtimeFrom` | `ctime_from` | json | `int64` | 否 | — | — |
| `CtimeTo` | `ctime_to` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

响应：`CollectorDeadLetterListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorDeadLetterListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/collector/policy/active` — 当前生效的采样与脱敏策略（无 ACTIVE 时由服务明确报错，不回空策略）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/collectorpolicyactivehandler.go`
- 业务实现：`gateway/admin/internal/logic/collectorpolicyactivelogic.go`

请求：无参数体。

响应：`CollectorPolicyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorPolicyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/collector/policy/list` — 策略版本游标翻页（含 ARCHIVED：历史批次的归因依据）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/collectorpolicylisthandler.go`
- 业务实现：`gateway/admin/internal/logic/collectorpolicylistlogic.go`

请求：`ParamCollectorPolicyList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | form | `int32` | 否 | — | 0 全部；1 DRAFT、2 ACTIVE、3 ARCHIVED |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`CollectorPolicyListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorPolicyListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/collector/health` — 采集与投递健康度：积压、限流、盐可用性与生效策略版本

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/collectorhealthhandler.go`
- 业务实现：`gateway/admin/internal/logic/collectorhealthlogic.go`

请求：无参数体。

响应：`CollectorHealthResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorHealthData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## event-collector 域运营路由（受 AdminPermission 保护）（AdminPermission，4 条）

> 四条写入口都要求会话身份 + idempotency_key 非空，operator 由会话渲染成
> gateway/admin:<admin_id>（表单不得声明自己是谁）；有 reason 位的两条一并先挡空。
> 采样比例区间、策略生命周期（谁能从 DRAFT 变 ACTIVE）、盐可用性、白名单是否覆盖
> 内置隐私底线、单次重放条数上限、租约围栏与投递重试上限全部由 event-collector 判定，
> 网关只转达入参并投影结论（AGENTS.md §5/§7）。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/collector/delivery/retry` | 推进到期未发送事件（轮次幂等；行层租约才是重复投递的围栏） | `collector:delivery` / `retry` | `collectorDeliveryRetry` | `collectordeliveryretrylogic.go` |
| POST | `/admin/collector/dead-letter/replay` | 重放投递死信（重新入队，不重新采样/脱敏；已是终态的计入 skipped） | `collector:deadletter` / `replay` | `collectorDeadLetterReplay` | `collectordeadletterreplaylogic.go` |
| POST | `/admin/collector/policy/upsert` | 新建/修改策略草稿（state 不可声明，服务按 DRAFT 落；ACTIVE 不可原地改） | `collector:policy` / `update` | `collectorPolicyUpsert` | `collectorpolicyupsertlogic.go` |
| POST | `/admin/collector/policy/activate` | 切换生效策略版本（旧 ACTIVE 转 ARCHIVED；expected_current_version 做乐观校验） | `collector:policy` / `enable` | `collectorPolicyActivate` | `collectorpolicyactivatelogic.go` |

### POST `/admin/collector/delivery/retry` — 推进到期未发送事件（轮次幂等；行层租约才是重复投递的围栏）

- 权限口径：AdminPermission · 权限点 `collector:delivery` / `retry`
- goctl 入口：`gateway/admin/internal/handler/collectordeliveryretryhandler.go`
- 业务实现：`gateway/admin/internal/logic/collectordeliveryretrylogic.go`

请求：`ParamCollectorDeliveryRetry`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 否 | — | 空表示全部 topic |
| `Now` | `now` | json | `int64` | 否 | — | — |
| `Limit` | `limit` | json | `int32` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`CollectorDeliveryRetryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorDeliveryRetryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/collector/dead-letter/replay` — 重放投递死信（重新入队，不重新采样/脱敏；已是终态的计入 skipped）

- 权限口径：AdminPermission · 权限点 `collector:deadletter` / `replay`
- goctl 入口：`gateway/admin/internal/handler/collectordeadletterreplayhandler.go`
- 业务实现：`gateway/admin/internal/logic/collectordeadletterreplaylogic.go`

请求：`ParamCollectorDeadLetterReplay`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeadLetterIds` | `dead_letter_ids` | json | `[]int64` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`CollectorDeadLetterReplayResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorDeadLetterReplayData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/collector/policy/upsert` — 新建/修改策略草稿（state 不可声明，服务按 DRAFT 落；ACTIVE 不可原地改）

- 权限口径：AdminPermission · 权限点 `collector:policy` / `update`
- goctl 入口：`gateway/admin/internal/handler/collectorpolicyupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/collectorpolicyupsertlogic.go`

请求：`ParamCollectorPolicyUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Version` | `version` | json | `string` | 是 | — | — |
| `SampleRules` | `sample_rules` | json | `[]CollectorSampleRule` | 否 | — | — |
| `SaltVersion` | `salt_version` | json | `int32` | 是 | — | — |
| `SaltRef` | `salt_ref` | json | `string` | 是 | — | — |
| `FieldWhitelist` | `field_whitelist` | json | `[]string` | 否 | — | — |
| `DropFields` | `drop_fields` | json | `[]string` | 否 | — | — |
| `MaxEventsPerBatch` | `max_events_per_batch` | json | `int32` | 否 | — | — |
| `MaxRequestBytes` | `max_request_bytes` | json | `int64` | 否 | — | — |
| `MaxEventPayloadBytes` | `max_event_payload_bytes` | json | `int32` | 否 | — | — |
| `MaxClockSkewSeconds` | `max_clock_skew_seconds` | json | `int32` | 否 | — | — |
| `MaxBackfillSeconds` | `max_backfill_seconds` | json | `int32` | 否 | — | — |
| `KeywordMaxRunes` | `keyword_max_runes` | json | `int32` | 否 | — | — |
| `RetentionDays` | `retention_days` | json | `int32` | 否 | — | — |
| `DeliverMaxAttempts` | `deliver_max_attempts` | json | `int32` | 否 | — | — |
| `RetryBaseSeconds` | `retry_base_seconds` | json | `int64` | 否 | — | — |
| `RetryMaxSeconds` | `retry_max_seconds` | json | `int64` | 否 | — | — |
| `Note` | `note` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`CollectorPolicyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorPolicyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/collector/policy/activate` — 切换生效策略版本（旧 ACTIVE 转 ARCHIVED；expected_current_version 做乐观校验）

- 权限口径：AdminPermission · 权限点 `collector:policy` / `enable`
- goctl 入口：`gateway/admin/internal/handler/collectorpolicyactivatehandler.go`
- 业务实现：`gateway/admin/internal/logic/collectorpolicyactivatelogic.go`

请求：`ParamCollectorPolicyActivate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Version` | `version` | json | `string` | 是 | — | — |
| `ExpectedCurrentVersion` | `expected_current_version` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`CollectorPolicyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorPolicyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamCollectorSchemaValidate`

> ParamCollectorSchemaValidate 单事件干跑校验入参：不落库、不投递，只回判定结论。 / 字段一一对齐 BehaviorEvent（无前缀）与 EventContext 的**入参位**（ctx_ 前缀）； / 服务端回填的 device_hash / ip_segment 不在这里——它们是判定的中间产物，不回带。 /  / ctx_device_id / ctx_ip 是明文入参，且只在请求生命周期内存在：埋点方要复现 / 「未登录且无设备标识」（REJECT_MISSING_SUBJECT）与「payload 含禁止入库字段」 / （REJECT_PRIVACY_FIELD）两类判定就必须能把明文给到服务，否则干跑结论与真采集不一致。 / CollectorSchemaValidateData 的字段集里没有任何位能承载它们（只有 valid/decision/reason/ / missing_fields/event_type/topic/schema_version/note），本路由也因此刻意不返回归一化后的 / 哈希值——那会把「给明文换一个摘要」变成这条免鉴权路由的副产品（AGENTS.md §7）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | json | `int32` | 否 | — | 0 按 SOURCE_CLIENT 判定，与采集入口同一口径 |
| `EventId` | `event_id` | json | `string` | 否 | — | — |
| `EventType` | `event_type` | json | `string` | 否 | — | — |
| `SchemaVersion` | `schema_version` | json | `int32` | 否 | — | — |
| `OccurredAt` | `occurred_at` | json | `int64` | 否 | — | — |
| `ReportedAt` | `reported_at` | json | `int64` | 否 | — | — |
| `Category` | `category` | json | `int32` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `ContentType` | `content_type` | json | `string` | 否 | — | — |
| `ContentId` | `content_id` | json | `int64` | 否 | — | — |
| `Aid` | `aid` | json | `int64` | 否 | — | — |
| `Vid` | `vid` | json | `string` | 否 | — | — |
| `TargetMid` | `target_mid` | json | `int64` | 否 | — | — |
| `SessionId` | `session_id` | json | `string` | 否 | — | — |
| `PositionMs` | `position_ms` | json | `int64` | 否 | — | — |
| `DurationMs` | `duration_ms` | json | `int64` | 否 | — | — |
| `BufferCount` | `buffer_count` | json | `int32` | 否 | — | — |
| `FirstFrameMs` | `first_frame_ms` | json | `int64` | 否 | — | — |
| `AvgBitrate` | `avg_bitrate` | json | `int32` | 否 | — | — |
| `ErrorCode` | `error_code` | json | `string` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `ResultIndex` | `result_index` | json | `int32` | 否 | — | — |
| `TargetUrl` | `target_url` | json | `string` | 否 | — | — |
| `Payload` | `payload` | json | `string` | 否 | — | 扩展 JSON 文本原文（干跑不回带） |
| `CtxMid` | `ctx_mid` | json | `int64` | 否 | — | — |
| `CtxDeviceId` | `ctx_device_id` | json | `string` | 否 | — | — |
| `CtxDeviceType` | `ctx_device_type` | json | `string` | 否 | — | — |
| `CtxIp` | `ctx_ip` | json | `string` | 否 | — | — |
| `CtxPlatform` | `ctx_platform` | json | `int32` | 否 | — | — |
| `CtxAppId` | `ctx_app_id` | json | `string` | 否 | — | — |
| `CtxAppVersion` | `ctx_app_version` | json | `string` | 否 | — | — |
| `CtxSdkVersion` | `ctx_sdk_version` | json | `string` | 否 | — | — |
| `CtxOsVersion` | `ctx_os_version` | json | `string` | 否 | — | — |
| `CtxNetworkType` | `ctx_network_type` | json | `string` | 否 | — | — |
| `CtxModel` | `ctx_model` | json | `string` | 否 | — | — |
| `CtxRegion` | `ctx_region` | json | `string` | 否 | — | — |
| `CtxSessionId` | `ctx_session_id` | json | `string` | 否 | — | — |
| `CtxPage` | `ctx_page` | json | `string` | 否 | — | — |
| `CtxSpm` | `ctx_spm` | json | `string` | 否 | — | 行为链路标识，非广告位参数（AGENTS.md §7） |

### `CollectorSchemaValidateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorSchemaValidateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCollectorBatchGet`

> ParamCollectorBatchGet 按 batch_id 精读批次（客户端上报后拿到的就是这个键）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BatchId` | `batch_id` | form | `string` | 是 | — | — |

### `CollectorBatchResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorBatchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCollectorBatchList`

> ParamCollectorBatchList 批次台账游标翻页。 / 台账随采集流量线性增长，「零条件 + 零时间边界」会被服务判 ErrInvalidPage / （不开放无界扫描）；page_size 上限与 cursor 语法也归服务，网关只挡负数与倒置区间。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | 0 表示不按 mid 过滤（游客上报的批次 mid 就是 0） |
| `DeviceHash` | `device_hash` | json | `string` | 否 | — | — |
| `IpSegment` | `ip_segment` | json | `string` | 否 | — | 脱敏段，如 203.0.113.0/24 |
| `CtimeFrom` | `ctime_from` | json | `int64` | 否 | — | — |
| `CtimeTo` | `ctime_to` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

### `CollectorBatchListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorBatchListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCollectorEventGet`

> ParamCollectorEventGet 按 event_id 精读单条台账（服务会用 Outbox 真值覆盖投递投影）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventId` | `event_id` | form | `string` | 是 | — | — |

### `CollectorEventResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorEventData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCollectorEventList`

> ParamCollectorEventList 事件台账游标翻页：decision/reason/delivery_state 是三个 / 互相独立的维度（「被拒」和「投递失败」是两件事），一起给才是 AND 收窄而不是拼条件。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `BatchId` | `batch_id` | json | `string` | 否 | — | — |
| `EventType` | `event_type` | json | `string` | 否 | — | — |
| `Category` | `category` | json | `int32` | 否 | — | — |
| `Decision` | `decision` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `int32` | 否 | — | — |
| `DeliveryState` | `delivery_state` | json | `int32` | 否 | — | — |
| `Topic` | `topic` | json | `string` | 否 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `DeviceHash` | `device_hash` | json | `string` | 否 | — | — |
| `CtimeFrom` | `ctime_from` | json | `int64` | 否 | — | — |
| `CtimeTo` | `ctime_to` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

### `CollectorEventListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorEventListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCollectorDeadLetterList`

> ParamCollectorDeadLetterList 死信翻页（topic/state + 时间窗，state 空表示全部）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 否 | — | — |
| `State` | `state` | json | `string` | 否 | — | open/replayed/discarded，空表示全部 |
| `CtimeFrom` | `ctime_from` | json | `int64` | 否 | — | — |
| `CtimeTo` | `ctime_to` | json | `int64` | 否 | — | — |
| `Cursor` | `cursor` | json | `string` | 否 | — | — |
| `PageSize` | `page_size` | json | `int32` | 否 | — | — |

### `CollectorDeadLetterListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorDeadLetterListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CollectorPolicyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorPolicyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCollectorPolicyList`

> ParamCollectorPolicyList 策略版本翻页（含 ARCHIVED：它们是历史批次的归因依据）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | form | `int32` | 否 | — | 0 全部；1 DRAFT、2 ACTIVE、3 ARCHIVED |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `CollectorPolicyListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorPolicyListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CollectorHealthResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorHealthData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCollectorDeliveryRetry`

> ParamCollectorDeliveryRetry 推进到期未发送事件（cron 兜底任务的运维手工 counterpart）。 / operator 不在表单里：由会话渲染成 gateway/admin:<admin_id>，请求体不能自称操作者。 / now=0 用服务当前时间、limit=0 用 Dispatch.BatchSize，负数两者都无对应语义故网关先拒； / idempotency_key 是「轮次幂等」键——没有它，点了两次的第二轮会把同一批在途事件再扫一遍。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 否 | — | 空表示全部 topic |
| `Now` | `now` | json | `int64` | 否 | — | — |
| `Limit` | `limit` | json | `int32` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `CollectorDeliveryRetryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorDeliveryRetryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCollectorDeadLetterReplay`

> ParamCollectorDeadLetterReplay 重放投递死信：重新入投递队列，不重新采样、不重新脱敏 / （首次入库定型的 policy_version/sanitize_version 必须保持，否则归因对不上）。 / 单次条数上限（MaxReplayPerRequest）由服务判定；reason 与 operator 一样是审计义务。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeadLetterIds` | `dead_letter_ids` | json | `[]int64` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `CollectorDeadLetterReplayResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CollectorDeadLetterReplayData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCollectorPolicyUpsert`

> ParamCollectorPolicyUpsert 新建/修改策略**草稿**。 / 表单刻意不声明 state：Upsert 只能碰 DRAFT，把 state 当「我要生效」用会让一个权限点 / 顺带完成另一件事，而生效是独立的 collector:policy/enable（网关不传即 UNSPECIFIED， / 服务按 DRAFT 落库并拒绝任何非 DRAFT 的声明）。 / 其余数值 0 一律是「继承服务/config 默认」的哨兵（inherit32/inherit64），负数没有语义； / salt_ref 必须是环境变量名而不是盐值，白名单不得覆盖内置隐私底线——两者都由服务判定。 / operator 由会话渲染，idempotency_key 必填；ACTIVE 不可原地改、ARCHIVED 只读。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Version` | `version` | json | `string` | 是 | — | — |
| `SampleRules` | `sample_rules` | json | `[]CollectorSampleRule` | 否 | — | — |
| `SaltVersion` | `salt_version` | json | `int32` | 是 | — | — |
| `SaltRef` | `salt_ref` | json | `string` | 是 | — | — |
| `FieldWhitelist` | `field_whitelist` | json | `[]string` | 否 | — | — |
| `DropFields` | `drop_fields` | json | `[]string` | 否 | — | — |
| `MaxEventsPerBatch` | `max_events_per_batch` | json | `int32` | 否 | — | — |
| `MaxRequestBytes` | `max_request_bytes` | json | `int64` | 否 | — | — |
| `MaxEventPayloadBytes` | `max_event_payload_bytes` | json | `int32` | 否 | — | — |
| `MaxClockSkewSeconds` | `max_clock_skew_seconds` | json | `int32` | 否 | — | — |
| `MaxBackfillSeconds` | `max_backfill_seconds` | json | `int32` | 否 | — | — |
| `KeywordMaxRunes` | `keyword_max_runes` | json | `int32` | 否 | — | — |
| `RetentionDays` | `retention_days` | json | `int32` | 否 | — | — |
| `DeliverMaxAttempts` | `deliver_max_attempts` | json | `int32` | 否 | — | — |
| `RetryBaseSeconds` | `retry_base_seconds` | json | `int64` | 否 | — | — |
| `RetryMaxSeconds` | `retry_max_seconds` | json | `int64` | 否 | — | — |
| `Note` | `note` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `ParamCollectorPolicyActivate`

> ParamCollectorPolicyActivate 切换生效策略版本（旧 ACTIVE 自动转 ARCHIVED，保留归因）。 / expected_current_version 非空时服务做乐观校验，避免并发误切；它是 optional， / 空表示「不管当前是哪版都切」——后台二次确认框应默认带上读到的当前版本。 / reason 在契约里就是审计义务（切换直接决定下游特征完整性），网关先挡空。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Version` | `version` | json | `string` | 是 | — | — |
| `ExpectedCurrentVersion` | `expected_current_version` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `CollectorSchemaValidateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Valid` | `valid` | json | `bool` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `MissingFields` | `missing_fields` | json | `[]string` | 是 | — | — |
| `EventType` | `event_type` | json | `string` | 是 | — | 归一化后的 event_type |
| `Topic` | `topic` | json | `string` | 是 | — | 归一化后的投递 topic |
| `SchemaVersion` | `schema_version` | json | `int32` | 是 | — | — |
| `Note` | `note` | json | `string` | 是 | — | 非致命提示（字段被截断/脱敏） |

### `CollectorBatchData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Batch` | `batch` | json | `CollectorIngestBatch` | 是 | — | — |

### `CollectorBatchListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CollectorIngestBatch` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `CollectorEventData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Record` | `record` | json | `CollectorEventRecord` | 是 | — | — |

### `CollectorEventListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CollectorEventRecord` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `CollectorDeadLetterListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CollectorDeadLetterInfo` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `CollectorPolicyData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Policy` | `policy` | json | `CollectorDispatchPolicy` | 是 | — | — |
| `Created` | `created` | json | `bool` | 是 | — | 只有 upsert 可能为 true：本次新建了草稿版本 |

### `CollectorPolicyListData`

> CollectorPolicyListData 策略版本台账投影。created 在这里没有语义（只读翻页）， / 所以不复用 CollectorPolicyResponse：那会多出一个恒为 false 的字段让人猜它代表什么。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CollectorDispatchPolicy` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `CollectorHealthData`

> CollectorHealthData 采集与投递健康度。 / 只输出「布尔 + 变量名 + 计数」：salt_available 说明盐能否从 Secret 读到，salt_ref 只是 / 变量名，盐值本身永不出现。active_salt_version=0 表示 ACTIVE 策略缺失，应直接视为不健康； / 无 ACTIVE 策略时本方法仍正常返回（暴露缺失正是健康检查的职责）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ServerTime` | `server_time` | json | `int64` | 是 | — | — |
| `Topics` | `topics` | json | `[]CollectorTopicHealth` | 是 | — | — |
| `BatchesRejectedLastHour` | `batches_rejected_last_hour` | json | `int64` | 是 | — | — |
| `RateLimitedLastHour` | `rate_limited_last_hour` | json | `int64` | 是 | — | 整批限流 + 逐条限流两处的合计 |
| `ActiveSaltVersion` | `active_salt_version` | json | `int32` | 是 | — | — |
| `PolicyVersion` | `policy_version` | json | `string` | 是 | — | — |
| `SaltRef` | `salt_ref` | json | `string` | 是 | — | — |
| `SaltAvailable` | `salt_available` | json | `bool` | 是 | — | — |
| `Version` | `version` | json | `string` | 是 | — | 服务构建版本 |

### `CollectorDeliveryRetryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scanned` | `scanned` | json | `int32` | 是 | — | — |
| `Sent` | `sent` | json | `int32` | 是 | — | — |
| `Retrying` | `retrying` | json | `int32` | 是 | — | — |
| `Dead` | `dead` | json | `int32` | 是 | — | — |
| `NextRunAt` | `next_run_at` | json | `int64` | 是 | — | 建议下一轮执行时间 |

### `CollectorDeadLetterReplayData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Replayed` | `replayed` | json | `int32` | 是 | — | — |
| `Skipped` | `skipped` | json | `int32` | 是 | — | 已是 replayed/discarded 终态被跳过的条数 |
| `FailedIds` | `failed_ids` | json | `[]int64` | 是 | — | 重新入队失败的死信 ID：留在 open，可原样再来一次 |

### `CollectorSampleRule`

> 边界（AGENTS.md §5/§7）：event-collector 只拥有「行为事件的接收与投递台账」—— / 批次（ec_ingest_batch）、逐条校验结论与去重（ec_event_record）、投递状态与 Outbox / （ec_pending_delivery）、死信（ec_dead_letter）与采样/脱敏策略版本（ec_dispatch_policy）。 / 行为事实不在这里：事件正文经 common/eventenvelope 投递 MQ 后由 spm 消费， / 后台既不提供「手工补一条行为」的入口，也不据此直接改写推荐结果。 /  / proto 15 个方法只开放 13 条（9 读 + 4 写）。刻意不接的 2 个逐条记在 / gateway/admin/README.md，它们是同一类入口： /   - CollectEvents：客户端 SDK 批量上报入口，归 gateway/app（终端面）。后台开面 /     等于给控制台一个「伪造任意 mid/设备的用户行为」的通道，而 SPM 的全部下游 /     （热度、留存、推荐特征）都会把这份伪造当成事实（AGENTS.md §7）。 /   - IngestServerEvents：服务端内部埋点入口，caller_service 与 idempotency_key /     表达的是**调用方服务身份**（engagement/playback/search 等）。网关不是被信任的 /     上报服务，代为上报会让信封 producer 失真，事后无法归因到真实埋点方。 /  / 隐私口径（AGENTS.md §7）：本域响应只出现 device_hash（加盐摘要）、ip_segment（脱敏段）、 / salt_version 与 salt_ref（环境变量名）。盐值、明文设备号/IP/手机号既不入库也不出自本面。 / 唯一接受明文入参的是 /schema/validate 干跑校验：它不落库、不投递，且响应字段集里 / 没有任何位能回带明文（详见该入参注释）。 / CollectorSampleRule 单类事件的采样比例（基点，10000=全量）。 / event_type="*" 是兜底规则；quality_events=true 表示这条只作用于播放质量类事件 / （问题排查期通常全量，否则最该看的卡顿样本先被自己采掉了）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventType` | `event_type` | json | `string` | 是 | — | — |
| `SampleBps` | `sample_bps` | json | `int32` | 是 | — | — |
| `QualityEvents` | `quality_events` | json | `bool` | 否 | — | — |

### `CollectorIngestBatch`

> CollectorIngestBatch 接收批次台账投影：只有计数与脱敏后的主体标识，没有任何事件明文。 / state=6（REJECTED）要配 top_reason 才读得懂「整批被拒」与「逐条部分拒」的差别； / last_error 是服务侧先脱敏再收敛到列宽的摘要，不是下游错误原文（ops.go redactSensitive）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `BatchId` | `batch_id` | json | `string` | 是 | — | — |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `CallerService` | `caller_service` | json | `string` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 是 | — | — |
| `AppId` | `app_id` | json | `string` | 是 | — | — |
| `AppVersion` | `app_version` | json | `string` | 是 | — | — |
| `SdkVersion` | `sdk_version` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `DeviceHash` | `device_hash` | json | `string` | 是 | — | — |
| `IpSegment` | `ip_segment` | json | `string` | 是 | — | — |
| `SaltVersion` | `salt_version` | json | `int32` | 是 | — | — |
| `PolicyVersion` | `policy_version` | json | `string` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Accepted` | `accepted` | json | `int32` | 是 | — | — |
| `Duplicated` | `duplicated` | json | `int32` | 是 | — | — |
| `Rejected` | `rejected` | json | `int32` | 是 | — | — |
| `SampledOut` | `sampled_out` | json | `int32` | 是 | — | — |
| `Dispatched` | `dispatched` | json | `int32` | 是 | — | — |
| `Dead` | `dead` | json | `int32` | 是 | — | — |
| `RequestBytes` | `request_bytes` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `TopReason` | `top_reason` | json | `int32` | 是 | — | — |
| `LastError` | `last_error` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `ReceivedAt` | `received_at` | json | `int64` | 是 | — | — |
| `FinishedAt` | `finished_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `CollectorEventRecord`

> CollectorEventRecord 单条事件的接收与投递台账（ec_event_record 投影）。 / 读单条时服务会用 Outbox（ec_pending_delivery）真值覆盖 delivery_*， / 因此这里的「已投递/在途」是事实而不是可能滞后的投影； / payload 只有 sha256 摘要与字节数，原文永不入库（AGENTS.md §7）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `BatchId` | `batch_id` | json | `string` | 是 | — | — |
| `EventType` | `event_type` | json | `string` | 是 | — | — |
| `Category` | `category` | json | `int32` | 是 | — | — |
| `SchemaVersion` | `schema_version` | json | `int32` | 是 | — | — |
| `OccurredAt` | `occurred_at` | json | `int64` | 是 | — | — |
| `ReceivedAt` | `received_at` | json | `int64` | 是 | — | — |
| `ClockSkewSeconds` | `clock_skew_seconds` | json | `int64` | 是 | — | — |
| `Decision` | `decision` | json | `int32` | 是 | — | 1 ACCEPTED、2 DUPLICATED、3 REJECTED、4 SAMPLED_OUT、5 DEFERRED |
| `Reason` | `reason` | json | `int32` | 是 | — | RejectReason 稳定枚举，1=NONE |
| `ReasonDetail` | `reason_detail` | json | `string` | 是 | — | — |
| `DeliveryState` | `delivery_state` | json | `int32` | 是 | — | — |
| `Topic` | `topic` | json | `string` | 是 | — | — |
| `EnvelopeEventId` | `envelope_event_id` | json | `string` | 是 | — | — |
| `DeliveryAttempts` | `delivery_attempts` | json | `int32` | 是 | — | — |
| `NextRetryAt` | `next_retry_at` | json | `int64` | 是 | — | — |
| `LastError` | `last_error` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `DeviceHash` | `device_hash` | json | `string` | 是 | — | — |
| `IpSegment` | `ip_segment` | json | `string` | 是 | — | — |
| `SaltVersion` | `salt_version` | json | `int32` | 是 | — | — |
| `ContentType` | `content_type` | json | `string` | 是 | — | — |
| `ContentId` | `content_id` | json | `int64` | 是 | — | — |
| `Vid` | `vid` | json | `string` | 是 | — | — |
| `TargetMid` | `target_mid` | json | `int64` | 是 | — | — |
| `PayloadDigest` | `payload_digest` | json | `string` | 是 | — | — |
| `PayloadBytes` | `payload_bytes` | json | `int32` | 是 | — | — |
| `SanitizeVersion` | `sanitize_version` | json | `string` | 是 | — | — |
| `PolicyVersion` | `policy_version` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `CollectorDeadLetterInfo`

> CollectorDeadLetterInfo 投递死信摘要。 / reason 是稳定枚举串（mq_timeout / mq_auth / mq_unreachable / mq_payload_oversize / / mq_canceled / dispatcher_missing / delivery_internal），运营按它筛选，不是人读拼接； / state=open 才可重放，replayed/discarded 是终态（重放时计入 skipped）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `BatchId` | `batch_id` | json | `string` | 是 | — | — |
| `EventType` | `event_type` | json | `string` | 是 | — | — |
| `Topic` | `topic` | json | `string` | 是 | — | — |
| `PayloadDigest` | `payload_digest` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Attempts` | `attempts` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |
| `CreatedAt` | `created_at` | json | `int64` | 是 | — | — |
| `HandledAt` | `handled_at` | json | `int64` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |

### `CollectorDispatchPolicy`

> CollectorDispatchPolicy 采样与脱敏策略版本（ec_dispatch_policy）。 / 每次采集都把生效 version 写进批次与事件记录，事后才能重放归因，所以历史版本只读不删； / salt_ref 只是取盐的环境变量名，盐值本身永不出现（proto 注释同义）。 / operator/ctime/mtime 是服务落库的事实列，后台靠它们回答「这一版是谁、什么时候配的」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Version` | `version` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 DRAFT、2 ACTIVE、3 ARCHIVED |
| `SampleRules` | `sample_rules` | json | `[]CollectorSampleRule` | 是 | — | — |
| `SaltVersion` | `salt_version` | json | `int32` | 是 | — | — |
| `SaltRef` | `salt_ref` | json | `string` | 是 | — | — |
| `FieldWhitelist` | `field_whitelist` | json | `[]string` | 是 | — | — |
| `DropFields` | `drop_fields` | json | `[]string` | 是 | — | — |
| `MaxEventsPerBatch` | `max_events_per_batch` | json | `int32` | 是 | — | — |
| `MaxRequestBytes` | `max_request_bytes` | json | `int64` | 是 | — | — |
| `MaxEventPayloadBytes` | `max_event_payload_bytes` | json | `int32` | 是 | — | — |
| `MaxClockSkewSeconds` | `max_clock_skew_seconds` | json | `int32` | 是 | — | — |
| `MaxBackfillSeconds` | `max_backfill_seconds` | json | `int32` | 是 | — | — |
| `KeywordMaxRunes` | `keyword_max_runes` | json | `int32` | 是 | — | — |
| `RetentionDays` | `retention_days` | json | `int32` | 是 | — | — |
| `DeliverMaxAttempts` | `deliver_max_attempts` | json | `int32` | 是 | — | — |
| `RetryBaseSeconds` | `retry_base_seconds` | json | `int64` | 是 | — | — |
| `RetryMaxSeconds` | `retry_max_seconds` | json | `int64` | 是 | — | — |
| `Note` | `note` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `CollectorTopicHealth`

> CollectorTopicHealth 单个 topic 的投递积压视图：只有 topic 与计数，没有任何主体标识。 / oldest_pending_ctime=0 表示该 topic 无积压，不是「有一条 1970 年的积压」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 是 | — | — |
| `Pending` | `pending` | json | `int64` | 是 | — | — |
| `Retrying` | `retrying` | json | `int64` | 是 | — | — |
| `DeadOpen` | `dead_open` | json | `int64` | 是 | — | — |
| `SentLastHour` | `sent_last_hour` | json | `int64` | 是 | — | — |
| `OldestPendingCtime` | `oldest_pending_ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/23-admin-collector.md -->
