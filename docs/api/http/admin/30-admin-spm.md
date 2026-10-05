# 运营面 · `/admin/spm`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 阶段 4 运营面：spm 域（行为指标与口径） | 免鉴权 | 10 |
| 阶段 4 运营面：spm 域（行为指标与口径） | AdminPermission | 5 |

合计 **15** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 阶段 4 运营面：spm 域（行为指标与口径）（免鉴权，10 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/spm/metric/get` | 单窗口指标读（found=false 表示该口径无此窗口，不伪造 0） | `spmMetricGet` | `spmmetricgetlogic.go` |
| POST | `/admin/spm/metric/batch-get` | 同主体多口径 × 连续窗口批量读（回值是稳定序的列表） | `spmMetricBatchGet` | `spmmetricbatchgetlogic.go` |
| POST | `/admin/spm/hot-subject/list` | 热点榜（回显实际使用的窗口与口径版本） | `spmHotSubjectList` | `spmhotsubjectlistlogic.go` |
| POST | `/admin/spm/retention/get` | cohort 留存曲线（注册日/首播日分桶） | `spmRetentionGet` | `spmretentiongetlogic.go` |
| POST | `/admin/spm/definition/get` | 单口径读（metric_version=0 = 当前 ACTIVE 版本） | `spmMetricDefinitionGet` | `spmmetricdefinitiongetlogic.go` |
| POST | `/admin/spm/definition/list` | 口径目录分页（含 DRAFT/RETIRED：历史窗口要靠旧口径解释） | `spmMetricDefinitionList` | `spmmetricdefinitionlistlogic.go` |
| POST | `/admin/spm/job/get` | 聚合作业进度（job_id 或 request_id 二选一） | `spmAggregationJobGet` | `spmaggregationjobgetlogic.go` |
| POST | `/admin/spm/job/list` | 聚合作业列表（按类型/状态/时间筛） | `spmAggregationJobList` | `spmaggregationjoblistlogic.go` |
| POST | `/admin/spm/consumer-state/list` | 消费链路状态汇总（回答「事件消费到哪了、有没有堆积」） | `spmConsumerStateList` | `spmconsumerstatelistlogic.go` |
| POST | `/admin/spm/dead-letter/list` | 死信台账（只读；重放属 event-collector，本域没有重放口） | `spmDeadLetterList` | `spmdeadletterlistlogic.go` |

### POST `/admin/spm/metric/get` — 单窗口指标读（found=false 表示该口径无此窗口，不伪造 0）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmmetricgethandler.go`
- 业务实现：`gateway/admin/internal/logic/spmmetricgetlogic.go`

请求：`ParamSpmMetricGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | 必填且 > 0（0 = UNSPECIFIED，服务拒） |
| `SubjectId` | `subject_id` | json | `int64` | 是 | — | — |
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `WindowStart` | `window_start` | json | `int64` | 否 | — | — |

响应：`SpmMetricGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/metric/batch-get` — 同主体多口径 × 连续窗口批量读（回值是稳定序的列表）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmmetricbatchgethandler.go`
- 业务实现：`gateway/admin/internal/logic/spmmetricbatchgetlogic.go`

请求：`ParamSpmMetricBatchGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | — |
| `SubjectId` | `subject_id` | json | `int64` | 是 | — | — |
| `Keys` | `keys` | json | `[]SpmMetricKey` | 是 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | — |
| `WindowStartFrom` | `window_start_from` | json | `int64` | 否 | — | — |
| `WindowCount` | `window_count` | json | `int32` | 否 | — | — |

响应：`SpmMetricBatchGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricBatchGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/hot-subject/list` — 热点榜（回显实际使用的窗口与口径版本）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmhotsubjectlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/spmhotsubjectlistlogic.go`

请求：`ParamSpmHotSubjectList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | — |
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | — |
| `WindowStart` | `window_start` | json | `int64` | 否 | — | — |
| `ZoneId` | `zone_id` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`SpmHotSubjectListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmHotSubjectListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/retention/get` — cohort 留存曲线（注册日/首播日分桶）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmretentiongethandler.go`
- 业务实现：`gateway/admin/internal/logic/spmretentiongetlogic.go`

请求：`ParamSpmRetentionGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `CohortType` | `cohort_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `CohortDate` | `cohort_date` | json | `int64` | 是 | — | Unix 秒，服务按天规整 |
| `MaxDay` | `max_day` | json | `int32` | 否 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `ZoneId` | `zone_id` | json | `int64` | 否 | — | — |

响应：`SpmRetentionGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmRetentionGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/definition/get` — 单口径读（metric_version=0 = 当前 ACTIVE 版本）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmmetricdefinitiongethandler.go`
- 业务实现：`gateway/admin/internal/logic/spmmetricdefinitiongetlogic.go`

请求：`ParamSpmMetricDefinitionGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |

响应：`SpmMetricDefinitionGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricDefinitionGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/definition/list` — 口径目录分页（含 DRAFT/RETIRED：历史窗口要靠旧口径解释）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmmetricdefinitionlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/spmmetricdefinitionlistlogic.go`

请求：`ParamSpmMetricDefinitionList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | 0 = 不限 |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`SpmMetricDefinitionListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricDefinitionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/job/get` — 聚合作业进度（job_id 或 request_id 二选一）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmaggregationjobgethandler.go`
- 业务实现：`gateway/admin/internal/logic/spmaggregationjobgetlogic.go`

请求：`ParamSpmAggregationJobGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobId` | `job_id` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

响应：`SpmAggregationJobGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmAggregationJobGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/job/list` — 聚合作业列表（按类型/状态/时间筛）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmaggregationjoblisthandler.go`
- 业务实现：`gateway/admin/internal/logic/spmaggregationjoblistlogic.go`

请求：`ParamSpmAggregationJobList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobType` | `job_type` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Since` | `since` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`SpmAggregationJobListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmAggregationJobListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/consumer-state/list` — 消费链路状态汇总（回答「事件消费到哪了、有没有堆积」）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmconsumerstatelisthandler.go`
- 业务实现：`gateway/admin/internal/logic/spmconsumerstatelistlogic.go`

请求：`ParamSpmConsumerStateList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`SpmConsumerStateListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmConsumerStateListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/dead-letter/list` — 死信台账（只读；重放属 event-collector，本域没有重放口）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/spmdeadletterlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/spmdeadletterlistlogic.go`

请求：`ParamSpmDeadLetterList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 否 | — | — |
| `State` | `state` | json | `string` | 否 | — | open/replayed/ignored，空 = 全部 |
| `Since` | `since` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`SpmDeadLetterListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmDeadLetterListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 阶段 4 运营面：spm 域（行为指标与口径）（AdminPermission，5 条）

> -------------------- spm 个人画像读（受 AdminPermission 保护） --------------------
> GetUserInterest 与上面的榜单不同：它是「针对某一个 mid 的兴趣画像」，属定向个人数据读，
> 每次调用都有明确的被读主体，因此挂权限点（spm:interest / read），让授权可以单独收回，
> 也让这条路径进 operation 的判定与留痕，而不是和「刷新一下页面」的读混在一档。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/spm/interest/get` | 单用户兴趣画像（脱敏受控词表；stale=true 时调用方应按冷启动口径解释） | `spm:interest` / `read` | `spmUserInterestGet` | `spmuserinterestgetlogic.go` |
| POST | `/admin/spm/definition/upsert` | 登记新口径版本（只能新增，改已登记版本服务回 ErrMetricVersionImmutable；无删除语义） | `spm:definition` / `create` | `spmMetricDefinitionUpsert` | `spmmetricdefinitionupsertlogic.go` |
| POST | `/admin/spm/definition/state` | 口径上下架（DRAFT/ACTIVE/RETIRED；retire 后不再写入但历史窗口仍可解释） | `spm:definition` / `state` | `spmMetricDefinitionState` | `spmmetricdefinitionstatelogic.go` |
| POST | `/admin/spm/job/submit` | 提交聚合作业（实时/离线回填/重算；reason 说明回填范围或故障单号） | `spm:job` / `create` | `spmAggregationJobSubmit` | `spmaggregationjobsubmitlogic.go` |
| POST | `/admin/spm/metric/recompute` | 指标漂移修复重算（从事实表按指定口径版本重算，唯一正当的「改指标」路径） | `spm:metric` / `recompute` | `spmMetricRecompute` | `spmmetricrecomputelogic.go` |

### POST `/admin/spm/interest/get` — 单用户兴趣画像（脱敏受控词表；stale=true 时调用方应按冷启动口径解释）

- 权限口径：AdminPermission · 权限点 `spm:interest` / `read`
- goctl 入口：`gateway/admin/internal/handler/spmuserinterestgethandler.go`
- 业务实现：`gateway/admin/internal/logic/spmuserinterestgetlogic.go`

请求：`ParamSpmUserInterestGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | 必填且 > 0 |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `TopN` | `top_n` | json | `int32` | 否 | — | — |

响应：`SpmUserInterestGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmUserInterestGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/definition/upsert` — 登记新口径版本（只能新增，改已登记版本服务回 ErrMetricVersionImmutable；无删除语义）

- 权限口径：AdminPermission · 权限点 `spm:definition` / `create`
- goctl 入口：`gateway/admin/internal/handler/spmmetricdefinitionupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/spmmetricdefinitionupsertlogic.go`

请求：`ParamSpmMetricDefinitionUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `SpmMetricDefinitionInput` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`SpmMetricDefinitionUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricDefinitionUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/definition/state` — 口径上下架（DRAFT/ACTIVE/RETIRED；retire 后不再写入但历史窗口仍可解释）

- 权限口径：AdminPermission · 权限点 `spm:definition` / `state`
- goctl 入口：`gateway/admin/internal/handler/spmmetricdefinitionstatehandler.go`
- 业务实现：`gateway/admin/internal/logic/spmmetricdefinitionstatelogic.go`

请求：`ParamSpmMetricDefinitionState`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | 必填且 > 0：状态迁移必须指向确切版本，0 不是「最新」 |
| `State` | `state` | json | `int32` | 是 | — | 目标状态（0 = UNSPECIFIED 在契约里没有语义） |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`SpmMetricDefinitionStateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricDefinitionStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/job/submit` — 提交聚合作业（实时/离线回填/重算；reason 说明回填范围或故障单号）

- 权限口径：AdminPermission · 权限点 `spm:job` / `create`
- goctl 入口：`gateway/admin/internal/handler/spmaggregationjobsubmithandler.go`
- 业务实现：`gateway/admin/internal/logic/spmaggregationjobsubmitlogic.go`

请求：`ParamSpmAggregationJobSubmit`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobType` | `job_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `SubjectType` | `subject_type` | json | `int32` | 否 | — | — |
| `SubjectId` | `subject_id` | json | `int64` | 否 | — | — |
| `MetricKey` | `metric_key` | json | `string` | 否 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `WindowStartFrom` | `window_start_from` | json | `int64` | 是 | — | — |
| `WindowStartTo` | `window_start_to` | json | `int64` | 否 | — | 0 = 当前时间 |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`SpmAggregationJobSubmitResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmAggregationJobSubmitData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/spm/metric/recompute` — 指标漂移修复重算（从事实表按指定口径版本重算，唯一正当的「改指标」路径）

- 权限口径：AdminPermission · 权限点 `spm:metric` / `recompute`
- goctl 入口：`gateway/admin/internal/handler/spmmetricrecomputehandler.go`
- 业务实现：`gateway/admin/internal/logic/spmmetricrecomputelogic.go`

请求：`ParamSpmMetricRecompute`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `SubjectId` | `subject_id` | json | `int64` | 是 | — | — |
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | 必填且 > 0 |
| `WindowType` | `window_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `WindowStartFrom` | `window_start_from` | json | `int64` | 是 | — | — |
| `WindowStartTo` | `window_start_to` | json | `int64` | 否 | — | 0 = 当前时间 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`SpmMetricRecomputeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricRecomputeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamSpmMetricGet`

> --- 只读面（不进 routePermissions） --- / ParamSpmMetricGet 读单个「主体 × 口径 × 窗口」的指标值。 / metric_version=0 与 window_start=0 是契约里的合法哨兵（分别表示「当前 ACTIVE 版本」与 / 「最近一个已闭合窗口」），网关不改写、不补具体值——补了就变成读一个可能没人算过的窗口。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | 必填且 > 0（0 = UNSPECIFIED，服务拒） |
| `SubjectId` | `subject_id` | json | `int64` | 是 | — | — |
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `WindowStart` | `window_start` | json | `int64` | 否 | — | — |

### `SpmMetricGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmMetricBatchGet`

> ParamSpmMetricBatchGet 读同一主体的一组口径在连续窗口上的取值。 / keys 上限 50、window_count 1..30 由服务判（网关不裁剪、不去重：悄悄丢掉一个键， / 后台看到的就是「这个口径没数据」）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | — |
| `SubjectId` | `subject_id` | json | `int64` | 是 | — | — |
| `Keys` | `keys` | json | `[]SpmMetricKey` | 是 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | — |
| `WindowStartFrom` | `window_start_from` | json | `int64` | 否 | — | — |
| `WindowCount` | `window_count` | json | `int32` | 否 | — | — |

### `SpmMetricBatchGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricBatchGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmHotSubjectList`

> ParamSpmHotSubjectList 榜单投影（按指标值倒序取主体主键）。zone_id=0 表示不限分区。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | — |
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | — |
| `WindowStart` | `window_start` | json | `int64` | 否 | — | — |
| `ZoneId` | `zone_id` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `SpmHotSubjectListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmHotSubjectListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmRetentionGet`

> ParamSpmRetentionGet cohort 留存曲线（注册日 / 首次播放日两种分桶）。 / max_day 1..90 的上限由服务判（超了是拒，不是夹取——夹取会让后台以为曲线只有 90 天）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `CohortType` | `cohort_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `CohortDate` | `cohort_date` | json | `int64` | 是 | — | Unix 秒，服务按天规整 |
| `MaxDay` | `max_day` | json | `int32` | 否 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `ZoneId` | `zone_id` | json | `int64` | 否 | — | — |

### `SpmRetentionGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmRetentionGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmMetricDefinitionGet`

> ParamSpmMetricDefinitionGet 读单个口径（metric_version=0 = 当前 ACTIVE 版本）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |

### `SpmMetricDefinitionGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricDefinitionGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmMetricDefinitionList`

> ParamSpmMetricDefinitionList 口径目录分页（含 DRAFT 与 RETIRED；后台要看历史解释）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | 0 = 不限 |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `SpmMetricDefinitionListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricDefinitionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmAggregationJobGet`

> ParamSpmAggregationJobGet 查作业进度：job_id 与 request_id 二选一（后者是幂等回放用）。 / 两个都不给没有任何可查目标，网关先挡住——否则服务按空主键查出一条「不存在」， / 后台会把参数缺失读成「作业丢了」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobId` | `job_id` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

### `SpmAggregationJobGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmAggregationJobGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmAggregationJobList`

> ParamSpmAggregationJobList 作业列表（按类型/状态/时间筛）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobType` | `job_type` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Since` | `since` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `SpmAggregationJobListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmAggregationJobListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmConsumerStateList`

> ParamSpmConsumerStateList 消费链路堆积视图（「事件消费到哪了」）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `SpmConsumerStateListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmConsumerStateListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmDeadLetterList`

> ParamSpmDeadLetterList 死信台账（只读：重放口在 event-collector，不在本域，见其 README）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 否 | — | — |
| `State` | `state` | json | `string` | 否 | — | open/replayed/ignored，空 = 全部 |
| `Since` | `since` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `SpmDeadLetterListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmDeadLetterListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmUserInterestGet`

> ParamSpmUserInterestGet 定向个人画像读。top_n 上限 100 由服务判（超了是拒不是裁）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | 必填且 > 0 |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `TopN` | `top_n` | json | `int32` | 否 | — | — |

### `SpmUserInterestGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmUserInterestGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmMetricDefinitionUpsert`

> ParamSpmMetricDefinitionUpsert 新增口径版本。 / 表单**没有** operator 位：操作者只能由后台会话渲染成 gateway/admin:<admin_id>， / 让请求体自报「我是谁」等于把审计主体交给调用方编。 / 表单也**没有**独立的 reason 位：UpsertMetricDefinitionReq 只有 definition/operator/request_id / 三个位，「为什么要新版本」的落点是 definition.description（契约里唯一的说明位）， / 网关不再另造一个传不出去的字段。 / idempotency_key → request_id 原值透传（改一个字符等于换一次执行权）。 / 公式是否自洽、单位是否支持、窗口组合是否合法、事件类型是否在白名单内、 / 版本号能否使用一律由 spm 判定（§5：口径注册表属 spm）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `SpmMetricDefinitionInput` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `SpmMetricDefinitionUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricDefinitionUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmMetricDefinitionState`

> ParamSpmMetricDefinitionState 口径状态迁移。reason 必填（proto 同义）：上下架会改变 / 后续所有读请求取到的「当前版本」，无理由不受理。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | 必填且 > 0：状态迁移必须指向确切版本，0 不是「最新」 |
| `State` | `state` | json | `int32` | 是 | — | 目标状态（0 = UNSPECIFIED 在契约里没有语义） |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `SpmMetricDefinitionStateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricDefinitionStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmAggregationJobSubmit`

> ParamSpmAggregationJobSubmit 提交聚合作业。subject_type=0 / subject_id=0 / metric_key 空 / 都是「范围更大」的合法哨兵（全部主体 / 不限主体 / 该类型下全部指标）：网关不把它们 / 折叠成 0 长度范围，也不因「看起来像漏填」而拒绝——但一次全量回填该不该受理由服务判。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobType` | `job_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `SubjectType` | `subject_type` | json | `int32` | 否 | — | — |
| `SubjectId` | `subject_id` | json | `int64` | 否 | — | — |
| `MetricKey` | `metric_key` | json | `string` | 否 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `WindowStartFrom` | `window_start_from` | json | `int64` | 是 | — | — |
| `WindowStartTo` | `window_start_to` | json | `int64` | 否 | — | 0 = 当前时间 |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `SpmAggregationJobSubmitResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmAggregationJobSubmitData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSpmMetricRecompute`

> ParamSpmMetricRecompute 重算入口（计数/指标漂移的唯一修复路径，见 docs/data-design.md §5）。 / metric_version 在 proto 里是**必填显式版本**（0 不行）：悄悄按新版本重算会改写历史窗口的解释。 / 本方法没有 reason 位（契约缺口，已在 README 登记）：重算留痕只能靠 operator + request_id。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `SubjectId` | `subject_id` | json | `int64` | 是 | — | — |
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | 必填且 > 0 |
| `WindowType` | `window_type` | json | `int32` | 是 | — | 必填且 > 0 |
| `WindowStartFrom` | `window_start_from` | json | `int64` | 是 | — | — |
| `WindowStartTo` | `window_start_to` | json | `int64` | 否 | — | 0 = 当前时间 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `SpmMetricRecomputeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SpmMetricRecomputeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `SpmMetricGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | false = 该口径在此窗口没有数据（不是 0） |
| `Point` | `point` | json | `SpmMetricPoint` | 是 | — | — |

### `SpmMetricKey`

> SpmMetricKey 是 BatchGetMetrics 的口径键位（metric_version=0 表示用当前 ACTIVE 版本）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 否 | — | — |

### `SpmMetricBatchGetData`

> SpmMetricBatchGetData 把服务的 map<string, MetricPoint> 摊平成稳定序的列表： / map 遍历序随机，原样吐给后台会让同一请求两次刷新顺序不同。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Points` | `points` | json | `[]SpmMetricPoint` | 是 | — | — |

### `SpmHotSubjectListData`

> SpmHotSubjectListData 的 window_start/metric_version 是「实际用了哪个窗口、哪版口径」的回显， / 排障时必须以服务回值为准（传 0 时用的是最近闭合窗口，网关猜不出来）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Subjects` | `subjects` | json | `[]SpmHotSubject` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `WindowStart` | `window_start` | json | `int64` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | — |

### `SpmRetentionGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Points` | `points` | json | `[]SpmRetentionPoint` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | — |
| `CohortDate` | `cohort_date` | json | `int64` | 是 | — | 实际使用的分桶日 |

### `SpmMetricDefinitionGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Definition` | `definition` | json | `SpmMetricDefinition` | 是 | — | — |

### `SpmMetricDefinitionListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definitions` | `definitions` | json | `[]SpmMetricDefinition` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `SpmAggregationJobGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Job` | `job` | json | `SpmAggregationJob` | 是 | — | — |

### `SpmAggregationJobListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Jobs` | `jobs` | json | `[]SpmAggregationJob` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `SpmConsumerStateListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rows` | `rows` | json | `[]SpmConsumerStateRow` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `SpmDeadLetterListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]SpmDeadLetter` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `SpmUserInterestGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Interests` | `interests` | json | `[]SpmInterest` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | — |
| `Stale` | `stale` | json | `bool` | 是 | — | true = 画像超出留存窗口，应按冷启动处理 |

### `SpmMetricDefinitionInput`

> SpmMetricDefinitionInput 是登记新口径的表单位：没有 created_by/ctime/mtime（由服务按会话与库时钟渲染）， / 也没有「改旧版本」的位——只能新增版本。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | >= 1，0 由服务拒 |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Formula` | `formula` | json | `string` | 是 | — | — |
| `Unit` | `unit` | json | `string` | 是 | — | — |
| `SupportedWindows` | `supported_windows` | json | `[]int32` | 否 | — | — |
| `SourceEventTypes` | `source_event_types` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | 0 = 由服务按 DRAFT 登记；上下架仍走 /definition/state |
| `Description` | `description` | json | `string` | 否 | — | 为什么需要新版本 |

### `SpmMetricDefinitionUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Created` | `created` | json | `bool` | 是 | — | true = 真的新增了一个口径版本 |
| `Reused` | `reused` | json | `bool` | 是 | — | true = 幂等键命中已有请求，未发生第二次写入 |
| `Definition` | `definition` | json | `SpmMetricDefinition` | 是 | — | — |

### `SpmMetricDefinitionStateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `SpmMetricDefinition` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |

### `SpmAggregationJobSubmitData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobId` | `job_id` | json | `int64` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |
| `Job` | `job` | json | `SpmAggregationJob` | 是 | — | — |

### `SpmMetricRecomputeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobId` | `job_id` | json | `int64` | 是 | — | 修复作业，进度去 /job/get 查 |
| `Reused` | `reused` | json | `bool` | 是 | — | true = 幂等键命中已有作业，没有排第二个 |
| `WindowsPlanned` | `windows_planned` | json | `int32` | 是 | — | — |

### `SpmMetricPoint`

> 契约来源 services/spm/rpc/spm.proto（冻结）。spm 是「行为事实 → 指标」的唯一计算方（§7）： / 它消费 event-collector 投递的事件、按登记的口径版本聚合窗口，推荐侧只读它的产出。 /  / 本域三条硬口径，决定了下面为什么只开这 15 条路由： /   1. **后台不生产指标**：WriteMetricWindow 不接路由——proto 明写「写回通道只接受计算链路来源」 /      （实时聚合器 / 离线回填 / 重算作业）。开一个后台写口等于让「运营觉得某稿该火」变成一个 /      指标源，直接违反 §7 第 3 条，且之后任何一次重算都会把它冲掉、留下一条解释不了的差异； /   2. **后台不改推荐结果**：本域没有任何「把某个 aid 加权/置顶/屏蔽」的入参，运营能动的只有 /      口径登记（新增版本、上下架）与作业触发（回填、重算）。要看结果去 /admin/recommend 的决策回放； /   3. **口径版本不可原地改**：命中已存在的 (metric_key, metric_version) 且规格不同，服务回 /      ErrMetricVersionImmutable，绝不覆盖——历史窗口的解释依赖旧口径持续可查，所以本域也没有 DELETE。 /  / 时间统一 Unix 秒；窗口左边界由服务按粒度规整并回显（网关不做时区/对齐运算）。 / 分页沿用契约的 pn/ps（ps 上限 100 由服务夹取），响应 total 以服务回值为准、网关不复算。 / SpmMetricPoint 1:1 对应 rpc.MetricPoint：一个「主体 × 口径版本 × 窗口」的取值。 / 比率类指标必须同时带 numerator/denominator，否则跨窗口合并只能加权平均近似（重算前提）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | — |
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | — |
| `SubjectId` | `subject_id` | json | `int64` | 是 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | — |
| `WindowStart` | `window_start` | json | `int64` | 是 | — | — |
| `Value` | `value` | json | `float64` | 是 | — | — |
| `Numerator` | `numerator` | json | `int64` | 是 | — | — |
| `Denominator` | `denominator` | json | `int64` | 是 | — | — |
| `SampleCount` | `sample_count` | json | `int64` | 是 | — | — |
| `EventTime` | `event_time` | json | `int64` | 是 | — | — |

### `SpmHotSubject`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SubjectId` | `subject_id` | json | `int64` | 是 | — | — |
| `Value` | `value` | json | `float64` | 是 | — | — |
| `Numerator` | `numerator` | json | `int64` | 是 | — | — |
| `Denominator` | `denominator` | json | `int64` | 是 | — | — |
| `Rank` | `rank` | json | `int32` | 是 | — | — |

### `SpmRetentionPoint`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DayOffset` | `day_offset` | json | `int32` | 是 | — | 0 = cohort 当日 |
| `CohortSize` | `cohort_size` | json | `int64` | 是 | — | — |
| `Retained` | `retained` | json | `int64` | 是 | — | — |
| `Rate` | `rate` | json | `float64` | 是 | — | — |

### `SpmMetricDefinition`

> SpmMetricDefinition 是服务回读的口径行（created_by/ctime/mtime 以库为准，网关不本地造）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Formula` | `formula` | json | `string` | 是 | — | 人读口径说明：分子/分母/去重键 |
| `Unit` | `unit` | json | `string` | 是 | — | count / ratio / seconds / score |
| `SupportedWindows` | `supported_windows` | json | `[]int32` | 是 | — | — |
| `SourceEventTypes` | `source_event_types` | json | `string` | 是 | — | CSV，必须落在服务的事件白名单内 |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `CreatedBy` | `created_by` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `SpmAggregationJob`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobId` | `job_id` | json | `int64` | 是 | — | — |
| `JobType` | `job_type` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | — |
| `SubjectId` | `subject_id` | json | `int64` | 是 | — | — |
| `MetricKey` | `metric_key` | json | `string` | 是 | — | — |
| `MetricVersion` | `metric_version` | json | `int32` | 是 | — | — |
| `WindowType` | `window_type` | json | `int32` | 是 | — | — |
| `WindowStartFrom` | `window_start_from` | json | `int64` | 是 | — | — |
| `WindowStartTo` | `window_start_to` | json | `int64` | 是 | — | — |
| `WindowsTotal` | `windows_total` | json | `int32` | 是 | — | — |
| `WindowsDone` | `windows_done` | json | `int32` | 是 | — | — |
| `WindowsFailed` | `windows_failed` | json | `int32` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `LastError` | `last_error` | json | `string` | 是 | — | 服务侧截断保存，不含堆栈与 SQL |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `FinishedAt` | `finished_at` | json | `int64` | 是 | — | — |

### `SpmConsumerStateRow`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Count` | `count` | json | `int64` | 是 | — | — |
| `OldestCtime` | `oldest_ctime` | json | `int64` | 是 | — | — |
| `LastMsgOffset` | `last_msg_offset` | json | `int64` | 是 | — | — |
| `LastEventTime` | `last_event_time` | json | `int64` | 是 | — | — |

### `SpmDeadLetter`

> SpmDeadLetter 是死信行的后台投影。payload_preview 已是服务侧脱敏前缀（不含行为原文与标识符）， / 网关不再加工，也不把它写进日志（§7）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | 信封不可解析时为空串 |
| `EventType` | `event_type` | json | `string` | 是 | — | — |
| `Topic` | `topic` | json | `string` | 是 | — | — |
| `PayloadDigest` | `payload_digest` | json | `string` | 是 | — | sha256:<hex> |
| `PayloadPreview` | `payload_preview` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `SpmInterest`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `InterestKey` | `interest_key` | json | `string` | 是 | — | 受控词表：zone:<id> / tag:<id> / up:<mid> |
| `Weight` | `weight` | json | `float64` | 是 | — | — |
| `SampleCount` | `sample_count` | json | `int64` | 是 | — | — |
| `EventTime` | `event_time` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/30-admin-spm.md -->
