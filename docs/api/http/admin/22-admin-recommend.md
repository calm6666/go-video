# 运营面 · `/admin/recommend`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| recommend 域运营路由（只读面） | 免鉴权 | 8 |
| recommend 域运营路由（受 AdminPermission 保护） | AdminPermission | 9 |

合计 **17** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## recommend 域运营路由（只读面）（免鉴权，8 条）

> 与 audit / ops-config / cron / live 同一口径：池快照、版本台账、召回日志、决策摘要与
> 运行时配置的读取走免中间件路由组——后台列表页每次刷新都会打一次 RPC，
> 全量挂判定会把 operation 变成读放大瓶颈；写面的 operator/reason 才是审计落点。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/recommend/pool/snapshot` | 池快照：读某个池某个版本的条目（version=0 表示当前生效版本） | `getPoolSnapshot` | `getpoolsnapshotlogic.go` |
| GET | `/admin/recommend/pool/version/list` | 池版本台账（含生成批次与产出方，回滚可行性检查带 include_retired） | `listPoolVersions` | `listpoolversionslogic.go` |
| GET | `/admin/recommend/pool/config` | 在线召回参数与有 CURRENT 版本的池摘要 | `getRecallConfig` | `getrecallconfiglogic.go` |
| GET | `/admin/recommend/recall/log` | 回放一次在线召回请求（按 request_id 或 snapshot_id） | `getRecallRequestLog` | `getrecallrequestloglogic.go` |
| POST | `/admin/recommend/recall/log/list` | 召回请求日志分页（按用户/场景/时间窗） | `listRecallRequestLogs` | `listrecallrequestlogslogic.go` |
| GET | `/admin/recommend/rank/decision` | 排序决策回放（按 decision_id 或 request_id） | `getRankDecision` | `getrankdecisionlogic.go` |
| POST | `/admin/recommend/rank/decision/list` | 排序决策摘要分页（实验/模型/场景过滤，可只看降级） | `listRankDecisions` | `listrankdecisionslogic.go` |
| GET | `/admin/recommend/rank/runtime-config` | 在线排序参数与当前生效的模型/特征/实验 | `getRankRuntimeConfig` | `getrankruntimeconfiglogic.go` |

### GET `/admin/recommend/pool/snapshot` — 池快照：读某个池某个版本的条目（version=0 表示当前生效版本）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getpoolsnapshothandler.go`
- 业务实现：`gateway/admin/internal/logic/getpoolsnapshotlogic.go`

请求：`ParamRecommendPoolSnapshot`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | form | `int32` | 是 | — | 必填：Source 枚举，0 无对应池 |
| `PoolKey` | `pool_key` | form | `string` | 是 | — | 必填：池键 |
| `Version` | `version` | form | `int64` | 否 | — | 0 表示读 recall_pool_current 指向的 CURRENT 版本 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | 0 表示用服务默认；上限 MaxPoolSnapshotPage 由服务夹取 |

响应：`RecommendPoolSnapshotResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolSnapshotData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/recommend/pool/version/list` — 池版本台账（含生成批次与产出方，回滚可行性检查带 include_retired）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listpoolversionshandler.go`
- 业务实现：`gateway/admin/internal/logic/listpoolversionslogic.go`

请求：`ParamRecommendPoolVersionList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | form | `int32` | 是 | — | — |
| `PoolKey` | `pool_key` | form | `string` | 是 | — | — |
| `Limit` | `limit` | form | `int32` | 否 | — | 上限 MaxVersionList（默认 100） |
| `IncludeRetired` | `include_retired` | form | `bool` | 否 | — | 回滚可行性检查必须带上退役版本 |

响应：`RecommendPoolVersionListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolVersionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/recommend/pool/config` — 在线召回参数与有 CURRENT 版本的池摘要

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getrecallconfighandler.go`
- 业务实现：`gateway/admin/internal/logic/getrecallconfiglogic.go`

请求：`ParamRecommendRecallConfig`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scene` | `scene` | form | `string` | 否 | — | 预留：场景级参数，当前服务回服务级 |
| `Mid` | `mid` | form | `int64` | 否 | — | 0 表示游客口径（游客只允许冷启动/热门路） |

响应：`RecommendRecallConfigResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendRecallConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/recommend/recall/log` — 回放一次在线召回请求（按 request_id 或 snapshot_id）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getrecallrequestloghandler.go`
- 业务实现：`gateway/admin/internal/logic/getrecallrequestloglogic.go`

请求：`ParamRecommendRecallLogGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RequestId` | `request_id` | form | `string` | 否 | — | — |
| `SnapshotId` | `snapshot_id` | form | `string` | 否 | — | — |

响应：`RecommendRecallLogResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendRecallLogData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/recall/log/list` — 召回请求日志分页（按用户/场景/时间窗）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listrecallrequestlogshandler.go`
- 业务实现：`gateway/admin/internal/logic/listrecallrequestlogslogic.go`

请求：`ParamRecommendRecallLogList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | 0 表示不按用户过滤 |
| `Scene` | `scene` | json | `string` | 否 | — | — |
| `FromTime` | `from_time` | json | `int64` | 否 | — | Unix 秒，含 |
| `ToTime` | `to_time` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 上限 MaxRequestLogPage（默认 100） |

响应：`RecommendRecallLogListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendRecallLogListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/recommend/rank/decision` — 排序决策回放（按 decision_id 或 request_id）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getrankdecisionhandler.go`
- 业务实现：`gateway/admin/internal/logic/getrankdecisionlogic.go`

请求：`ParamRankDecisionGet`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DecisionId` | `decision_id` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 否 | — | — |

响应：`RankDecisionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankDecisionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/rank/decision/list` — 排序决策摘要分页（实验/模型/场景过滤，可只看降级）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listrankdecisionshandler.go`
- 业务实现：`gateway/admin/internal/logic/listrankdecisionslogic.go`

请求：`ParamRankDecisionList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ExpKey` | `exp_key` | json | `string` | 否 | — | — |
| `VariantKey` | `variant_key` | json | `string` | 否 | — | — |
| `ModelKey` | `model_key` | json | `string` | 否 | — | — |
| `ModelVersion` | `model_version` | json | `string` | 否 | — | — |
| `Scene` | `scene` | json | `string` | 否 | — | — |
| `FromTime` | `from_time` | json | `int64` | 否 | — | Unix 秒，含 |
| `ToTime` | `to_time` | json | `int64` | 否 | — | — |
| `OnlyDegraded` | `only_degraded` | json | `bool` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 上限 MaxDecisionPage（默认 100） |

响应：`RankDecisionListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankDecisionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/recommend/rank/runtime-config` — 在线排序参数与当前生效的模型/特征/实验

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getrankruntimeconfighandler.go`
- 业务实现：`gateway/admin/internal/logic/getrankruntimeconfiglogic.go`

请求：`ParamRankRuntimeConfig`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scene` | `scene` | form | `string` | 否 | — | — |
| `ModelKey` | `model_key` | form | `string` | 否 | — | 空表示服务默认 model_key |

响应：`RankRuntimeConfigResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankRuntimeConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## recommend 域运营路由（受 AdminPermission 保护）（AdminPermission，9 条）

> 九条写入口都要求会话身份 + idempotency_key（清理路由为 request_id）非空，
> operator 由会话渲染成 admin:<admin_id>，不接受表单声明；reason 按契约必填的路由一并校验非空。
> 池条目的实际写入、版本指针切换、模型/特征/实验的状态迁移全部在 recommend-recall / recommend-rank 侧，
> 网关只转达入参并投影结论（AGENTS.md §5/§7）。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/recommend/pool/item/upsert` | 分批写入池条目到指定版本（is_last_batch 置 READY；幂等键命中重放不重复写） | `recommend:pool` / `update` | `upsertPoolItems` | `upsertpoolitemslogic.go` |
| POST | `/admin/recommend/pool/version/publish` | 原子切换池的当前生效版本（READY→CURRENT，写审计并发 recall.pool.published 事件） | `recommend:pool` / `publish` | `publishPoolVersion` | `publishpoolversionlogic.go` |
| POST | `/admin/recommend/pool/version/rollback` | 回滚池版本到历史版本（运营回滚开关，与 publish 同一响应形态） | `recommend:pool` / `rollback` | `rollbackPoolVersion` | `rollbackpoolversionlogic.go` |
| POST | `/admin/recommend/pool/version/prune` | 分批清理过期池版本（dry_run 先核数；keep_versions 下限由服务守护） | `recommend:pool` / `prune` | `prunePoolVersions` | `prunepoolversionslogic.go` |
| POST | `/admin/recommend/rank/model/upsert` | 登记/更新模型版本元数据（版本不可变，元数据变更 revision+1） | `recommend:model` / `create` | `upsertModelVersion` | `upsertmodelversionlogic.go` |
| POST | `/admin/recommend/rank/model/state` | 切换模型版本状态（READY/ACTIVE/RETIRED；激活即回滚开关，需 reason） | `recommend:model` / `state` | `setModelVersionState` | `setmodelversionstatelogic.go` |
| POST | `/admin/recommend/rank/feature-config/upsert` | 登记/更新特征配置版本（feature_keys 清单与缺失值策略） | `recommend:feature` / `create` | `upsertFeatureConfig` | `upsertfeatureconfiglogic.go` |
| POST | `/admin/recommend/rank/experiment/upsert` | 新建/修改实验变体（分桶区间与 hash_seed 变更需 reason 说明） | `recommend:experiment` / `create` | `upsertExperiment` | `upsertexperimentlogic.go` |
| POST | `/admin/recommend/rank/experiment/state` | 实验状态迁移（RUNNING/PAUSED/STOPPED，与模型状态分属不同权限点） | `recommend:experiment` / `state` | `setExperimentState` | `setexperimentstatelogic.go` |

### POST `/admin/recommend/pool/item/upsert` — 分批写入池条目到指定版本（is_last_batch 置 READY；幂等键命中重放不重复写）

- 权限口径：AdminPermission · 权限点 `recommend:pool` / `update`
- goctl 入口：`gateway/admin/internal/handler/upsertpoolitemshandler.go`
- 业务实现：`gateway/admin/internal/logic/upsertpoolitemslogic.go`

请求：`ParamRecommendPoolItemUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `BatchId` | `batch_id` | json | `string` | 是 | — | — |
| `Generator` | `generator` | json | `string` | 是 | — | 作业标识，允许声明（它描述来源，不描述身份） |
| `SchemaVersion` | `schema_version` | json | `int32` | 否 | — | 0 = 服务当前版本 |
| `Items` | `items` | json | `[]RecommendPoolItemInput` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | 必填，原样透传 |
| `IsLastBatch` | `is_last_batch` | json | `bool` | 否 | — | — |

响应：`RecommendPoolItemUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolItemUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/pool/version/publish` — 原子切换池的当前生效版本（READY→CURRENT，写审计并发 recall.pool.published 事件）

- 权限口径：AdminPermission · 权限点 `recommend:pool` / `publish`
- goctl 入口：`gateway/admin/internal/handler/publishpoolversionhandler.go`
- 业务实现：`gateway/admin/internal/logic/publishpoolversionlogic.go`

请求：`ParamRecommendPoolVersionPublish`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | 必须处于 READY，是否可切换由服务判定 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RecommendPoolVersionSwitchResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolVersionSwitchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/pool/version/rollback` — 回滚池版本到历史版本（运营回滚开关，与 publish 同一响应形态）

- 权限口径：AdminPermission · 权限点 `recommend:pool` / `rollback`
- goctl 入口：`gateway/admin/internal/handler/rollbackpoolversionhandler.go`
- 业务实现：`gateway/admin/internal/logic/rollbackpoolversionlogic.go`

请求：`ParamRecommendPoolVersionRollback`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `TargetVersion` | `target_version` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RecommendPoolVersionSwitchResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolVersionSwitchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/pool/version/prune` — 分批清理过期池版本（dry_run 先核数；keep_versions 下限由服务守护）

- 权限口径：AdminPermission · 权限点 `recommend:pool` / `prune`
- goctl 入口：`gateway/admin/internal/handler/prunepoolversionshandler.go`
- 业务实现：`gateway/admin/internal/logic/prunepoolversionslogic.go`

请求：`ParamRecommendPoolVersionPrune`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `KeepVersions` | `keep_versions` | json | `int32` | 是 | — | — |
| `MaxRows` | `max_rows` | json | `int64` | 否 | — | 0 表示由服务取默认批量上限 |
| `DryRun` | `dry_run` | json | `bool` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | 门槛键：非空即可，原样落日志 |

响应：`RecommendPoolVersionPruneResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolVersionPruneData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/rank/model/upsert` — 登记/更新模型版本元数据（版本不可变，元数据变更 revision+1）

- 权限口径：AdminPermission · 权限点 `recommend:model` / `create`
- goctl 入口：`gateway/admin/internal/handler/upsertmodelversionhandler.go`
- 业务实现：`gateway/admin/internal/logic/upsertmodelversionlogic.go`

请求：`ParamRankModelVersionUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ModelKey` | `model_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `string` | 是 | — | — |
| `FeatureConfigVersion` | `feature_config_version` | json | `string` | 是 | — | 必须已存在，由服务判定 |
| `ObjectiveWeights` | `objective_weights` | json | `[]RankObjectiveWeight` | 否 | — | — |
| `ArtifactRef` | `artifact_ref` | json | `string` | 否 | — | — |
| `OfflineMetrics` | `offline_metrics` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RankModelVersionUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankModelVersionUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/rank/model/state` — 切换模型版本状态（READY/ACTIVE/RETIRED；激活即回滚开关，需 reason）

- 权限口径：AdminPermission · 权限点 `recommend:model` / `state`
- goctl 入口：`gateway/admin/internal/handler/setmodelversionstatehandler.go`
- 业务实现：`gateway/admin/internal/logic/setmodelversionstatelogic.go`

请求：`ParamRankModelStateSet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ModelKey` | `model_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `string` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | 2 READY、3 ACTIVE、4 RETIRED |
| `Reason` | `reason` | json | `string` | 是 | — | 契约必填：激活/回滚理由是审计要求 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RankModelStateSetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankModelStateSetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/rank/feature-config/upsert` — 登记/更新特征配置版本（feature_keys 清单与缺失值策略）

- 权限口径：AdminPermission · 权限点 `recommend:feature` / `create`
- goctl 入口：`gateway/admin/internal/handler/upsertfeatureconfighandler.go`
- 业务实现：`gateway/admin/internal/logic/upsertfeatureconfiglogic.go`

请求：`ParamRankFeatureConfigUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConfigVersion` | `config_version` | json | `string` | 是 | — | — |
| `FeatureKeys` | `feature_keys` | json | `[]string` | 是 | — | — |
| `MissingPolicy` | `missing_policy` | json | `string` | 否 | — | default / drop_source / reject |
| `FeatureStoreScene` | `feature_store_scene` | json | `string` | 否 | — | 将来接 feature-store 的读取场景 key |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RankFeatureConfigUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankFeatureConfigUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/rank/experiment/upsert` — 新建/修改实验变体（分桶区间与 hash_seed 变更需 reason 说明）

- 权限口径：AdminPermission · 权限点 `recommend:experiment` / `create`
- goctl 入口：`gateway/admin/internal/handler/upsertexperimenthandler.go`
- 业务实现：`gateway/admin/internal/logic/upsertexperimentlogic.go`

请求：`ParamRankExperimentUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ExpKey` | `exp_key` | json | `string` | 是 | — | — |
| `VariantKey` | `variant_key` | json | `string` | 是 | — | 同一 exp_key 下唯一，含 "control" |
| `LayerKey` | `layer_key` | json | `string` | 否 | — | — |
| `HashSeed` | `hash_seed` | json | `string` | 否 | — | — |
| `BucketStart` | `bucket_start` | json | `int32` | 是 | — | — |
| `BucketEnd` | `bucket_end` | json | `int32` | 是 | — | — |
| `ModelKey` | `model_key` | json | `string` | 否 | — | — |
| `ModelVersion` | `model_version` | json | `string` | 否 | — | 空表示沿用 ACTIVE 版本 |
| `FeatureConfigVersion` | `feature_config_version` | json | `string` | 否 | — | — |
| `Overrides` | `overrides` | json | `string` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | Unix 秒 |
| `EndAt` | `end_at` | json | `int64` | 否 | — | 0 表示未设定 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RankExperimentUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankExperimentUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/recommend/rank/experiment/state` — 实验状态迁移（RUNNING/PAUSED/STOPPED，与模型状态分属不同权限点）

- 权限口径：AdminPermission · 权限点 `recommend:experiment` / `state`
- goctl 入口：`gateway/admin/internal/handler/setexperimentstatehandler.go`
- 业务实现：`gateway/admin/internal/logic/setexperimentstatelogic.go`

请求：`ParamRankExperimentStateSet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ExpKey` | `exp_key` | json | `string` | 是 | — | — |
| `VariantKey` | `variant_key` | json | `string` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | 2 RUNNING、3 PAUSED、4 STOPPED |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

响应：`RankExperimentStateSetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankExperimentStateSetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamRecommendPoolSnapshot`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | form | `int32` | 是 | — | 必填：Source 枚举，0 无对应池 |
| `PoolKey` | `pool_key` | form | `string` | 是 | — | 必填：池键 |
| `Version` | `version` | form | `int64` | 否 | — | 0 表示读 recall_pool_current 指向的 CURRENT 版本 |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 否 | — | 0 表示用服务默认；上限 MaxPoolSnapshotPage 由服务夹取 |

### `RecommendPoolSnapshotResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolSnapshotData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRecommendPoolVersionList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | form | `int32` | 是 | — | — |
| `PoolKey` | `pool_key` | form | `string` | 是 | — | — |
| `Limit` | `limit` | form | `int32` | 否 | — | 上限 MaxVersionList（默认 100） |
| `IncludeRetired` | `include_retired` | form | `bool` | 否 | — | 回滚可行性检查必须带上退役版本 |

### `RecommendPoolVersionListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolVersionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRecommendRecallConfig`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scene` | `scene` | form | `string` | 否 | — | 预留：场景级参数，当前服务回服务级 |
| `Mid` | `mid` | form | `int64` | 否 | — | 0 表示游客口径（游客只允许冷启动/热门路） |

### `RecommendRecallConfigResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendRecallConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRecommendRecallLogGet`

> ParamRecommendRecallLogGet 回放一次在线召回：request_id 与 snapshot_id 至少给一个， / 两个都空时下游会去查空主键并回「没有这条记录」，网关先拒（与 live 的 subject 门槛同一口径）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RequestId` | `request_id` | form | `string` | 否 | — | — |
| `SnapshotId` | `snapshot_id` | form | `string` | 否 | — | — |

### `RecommendRecallLogResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendRecallLogData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRecommendRecallLogList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | 0 表示不按用户过滤 |
| `Scene` | `scene` | json | `string` | 否 | — | — |
| `FromTime` | `from_time` | json | `int64` | 否 | — | Unix 秒，含 |
| `ToTime` | `to_time` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 上限 MaxRequestLogPage（默认 100） |

### `RecommendRecallLogListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendRecallLogListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRankDecisionGet`

> ParamRankDecisionGet 回放一次排序决策：decision_id 与 request_id 至少给一个。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DecisionId` | `decision_id` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 否 | — | — |

### `RankDecisionResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankDecisionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRankDecisionList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ExpKey` | `exp_key` | json | `string` | 否 | — | — |
| `VariantKey` | `variant_key` | json | `string` | 否 | — | — |
| `ModelKey` | `model_key` | json | `string` | 否 | — | — |
| `ModelVersion` | `model_version` | json | `string` | 否 | — | — |
| `Scene` | `scene` | json | `string` | 否 | — | — |
| `FromTime` | `from_time` | json | `int64` | 否 | — | Unix 秒，含 |
| `ToTime` | `to_time` | json | `int64` | 否 | — | — |
| `OnlyDegraded` | `only_degraded` | json | `bool` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | json | `int32` | 否 | — | 上限 MaxDecisionPage（默认 100） |

### `RankDecisionListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankDecisionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRankRuntimeConfig`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scene` | `scene` | form | `string` | 否 | — | — |
| `ModelKey` | `model_key` | form | `string` | 否 | — | 空表示服务默认 model_key |

### `RankRuntimeConfigResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankRuntimeConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRecommendPoolItemUpsert`

> ParamRecommendPoolItemUpsert 分批写入某个版本。 / version 与 batch_id 必须成对给出（同一 (pool,version) 只能属于一个 batch_id，跨批次复用被服务拒绝）； / items 单次条数上限 MaxBatchItems（默认 1000）由服务夹取； / is_last_batch 置 READY 的判定在服务侧，网关不代为推断「这批是不是最后一批」。 / 契约缺口：UpsertPoolItemsReq 没有 operator 位（generator 是「产出方标识」，不是操作者）， / 因此本路由的「谁写的」只能落在网关日志的 admin_id 上，版本行里没有操作者（见 README）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `BatchId` | `batch_id` | json | `string` | 是 | — | — |
| `Generator` | `generator` | json | `string` | 是 | — | 作业标识，允许声明（它描述来源，不描述身份） |
| `SchemaVersion` | `schema_version` | json | `int32` | 否 | — | 0 = 服务当前版本 |
| `Items` | `items` | json | `[]RecommendPoolItemInput` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | 必填，原样透传 |
| `IsLastBatch` | `is_last_batch` | json | `bool` | 否 | — | — |

### `RecommendPoolItemUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolItemUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRecommendPoolVersionPublish`

> ParamRecommendPoolVersionPublish 原子切换生效版本。operator 由会话渲染，表单不得声明； / reason 与 idempotency_key 都是契约必填项（切换是审计事件）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | 必须处于 READY，是否可切换由服务判定 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RecommendPoolVersionSwitchResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolVersionSwitchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRecommendPoolVersionRollback`

> ParamRecommendPoolVersionRollback 回滚到历史版本。target_version 是否存在、是否已完成写入 / 由 recommend-recall 判定（READY/CURRENT/RETIRED 才可能可回滚，网关不预判）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `TargetVersion` | `target_version` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `ParamRecommendPoolVersionPrune`

> ParamRecommendPoolVersionPrune 分批清理过期版本（保护锁与主从延迟靠 max_rows 分批）。 / keep_versions 的下限（MinKeepVersions，默认 2）由服务判定，网关不复制这个数—— / 服务改默认值时后台不必跟着改代码。 / 契约缺口：PrunePoolVersionsReq 既没有 idempotency_key 也没有 trace_id，所以本路由的 / request_id 只用于网关侧留痕与表单防重，无法传给服务做真正的重放去重； / 删除类动作可重放但结果单调（已删的行不会再删），仍要求 dry_run 先行核数（见 README）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `KeepVersions` | `keep_versions` | json | `int32` | 是 | — | — |
| `MaxRows` | `max_rows` | json | `int64` | 否 | — | 0 表示由服务取默认批量上限 |
| `DryRun` | `dry_run` | json | `bool` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | 门槛键：非空即可，原样落日志 |

### `RecommendPoolVersionPruneResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RecommendPoolVersionPruneData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRankModelVersionUpsert`

> ParamRankModelVersionUpsert 登记/更新模型版本元数据。 / version 不可变（注册后只能改状态），元数据变更由服务把 revision +1； / artifact_ref 只是对象存储 key，模型本体与任何密钥都不经网关（AGENTS.md §4）。 / offline_metrics 是仅展示用的 JSON 文本，不参与在线决策，网关原样透传不解析。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ModelKey` | `model_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `string` | 是 | — | — |
| `FeatureConfigVersion` | `feature_config_version` | json | `string` | 是 | — | 必须已存在，由服务判定 |
| `ObjectiveWeights` | `objective_weights` | json | `[]RankObjectiveWeight` | 否 | — | — |
| `ArtifactRef` | `artifact_ref` | json | `string` | 否 | — | — |
| `OfflineMetrics` | `offline_metrics` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RankModelVersionUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankModelVersionUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRankModelStateSet`

> ParamRankModelStateSet 模型版本状态迁移（READY/ACTIVE/RETIRED）。 / ACTIVE 每 model_key 至多一个，因此「激活新版本」就是回滚开关的反向操作： / previous_active_version 必须回传，后台才能确认下线的是哪一个。 / 目标状态是否合法（例如 DRAFT 直接跳 ACTIVE）由服务判，网关不复制状态机。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ModelKey` | `model_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `string` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | 2 READY、3 ACTIVE、4 RETIRED |
| `Reason` | `reason` | json | `string` | 是 | — | 契约必填：激活/回滚理由是审计要求 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RankModelStateSetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankModelStateSetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRankFeatureConfigUpsert`

> ParamRankFeatureConfigUpsert 登记/更新特征配置版本（config_version 不可变标识）。 / feature_keys 条数上限 MaxFeatureKeys（默认 512）与 missing_policy 取值集合都由服务判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConfigVersion` | `config_version` | json | `string` | 是 | — | — |
| `FeatureKeys` | `feature_keys` | json | `[]string` | 是 | — | — |
| `MissingPolicy` | `missing_policy` | json | `string` | 否 | — | default / drop_source / reject |
| `FeatureStoreScene` | `feature_store_scene` | json | `string` | 否 | — | 将来接 feature-store 的读取场景 key |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RankFeatureConfigUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankFeatureConfigUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRankExperimentUpsert`

> ParamRankExperimentUpsert 新建/修改实验变体。operator 由会话渲染，表单不得声明； / hash_seed 变更会导致重新分桶，所以 reason 必填说明改了什么。 / 分桶区间、同层互斥、model_version 是否可绑定全部由 recommend-rank 判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ExpKey` | `exp_key` | json | `string` | 是 | — | — |
| `VariantKey` | `variant_key` | json | `string` | 是 | — | 同一 exp_key 下唯一，含 "control" |
| `LayerKey` | `layer_key` | json | `string` | 否 | — | — |
| `HashSeed` | `hash_seed` | json | `string` | 否 | — | — |
| `BucketStart` | `bucket_start` | json | `int32` | 是 | — | — |
| `BucketEnd` | `bucket_end` | json | `int32` | 是 | — | — |
| `ModelKey` | `model_key` | json | `string` | 否 | — | — |
| `ModelVersion` | `model_version` | json | `string` | 否 | — | 空表示沿用 ACTIVE 版本 |
| `FeatureConfigVersion` | `feature_config_version` | json | `string` | 否 | — | — |
| `Overrides` | `overrides` | json | `string` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | Unix 秒 |
| `EndAt` | `end_at` | json | `int64` | 否 | — | 0 表示未设定 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RankExperimentUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankExperimentUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRankExperimentStateSet`

> ParamRankExperimentStateSet 实验状态迁移（RUNNING/PAUSED/STOPPED）。 / 暂停保持已分桶、结束是终态，两者影响面差别很大，所以与 model/state 分属不同权限点； / 合法迁移与窗口重叠判定都在服务侧。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ExpKey` | `exp_key` | json | `string` | 是 | — | — |
| `VariantKey` | `variant_key` | json | `string` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | 2 RUNNING、3 PAUSED、4 STOPPED |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |

### `RankExperimentStateSetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `RankExperimentStateSetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `RecommendPoolSnapshotData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `BatchId` | `batch_id` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | PoolVersionState：1 写入中、2 就绪、3 生效、4 退役、5 失败 |
| `ItemCount` | `item_count` | json | `int32` | 是 | — | — |
| `Items` | `items` | json | `[]RecommendPoolItem` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `PublishedAt` | `published_at` | json | `int64` | 是 | — | 0 表示该版本尚未上线 |

### `RecommendPoolVersionListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]RecommendPoolVersionInfo` | 是 | — | — |
| `CurrentVersion` | `current_version` | json | `int64` | 是 | — | 0 = 该池无 CURRENT，在线不出数 |

### `RecommendRecallConfigData`

> RecommendRecallConfigData 在线召回参数。这里回的是**服务当前生效的配置**， / 后台据此判断「为什么这次只出了 120 条」，不用于改写任何请求参数默认值。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MaxCandidates` | `max_candidates` | json | `int32` | 是 | — | — |
| `DefaultLimit` | `default_limit` | json | `int32` | 是 | — | — |
| `PerSourceMax` | `per_source_max` | json | `int32` | 是 | — | — |
| `EnabledSources` | `enabled_sources` | json | `[]int32` | 是 | — | — |
| `DefaultSources` | `default_sources` | json | `[]int32` | 是 | — | — |
| `MaxSeedAids` | `max_seed_aids` | json | `int32` | 是 | — | — |
| `MaxSeedTags` | `max_seed_tags` | json | `int32` | 是 | — | — |
| `MaxExcludeAids` | `max_exclude_aids` | json | `int32` | 是 | — | — |
| `DegradeEnabled` | `degrade_enabled` | json | `bool` | 是 | — | false 时依赖故障直接报错，不降级出数 |
| `FallbackSource` | `fallback_source` | json | `int32` | 是 | — | — |
| `TtlSeconds` | `ttl_seconds` | json | `int64` | 是 | — | 服务建议的缓存秒数，网关自己不缓存 |
| `ReadyPools` | `ready_pools` | json | `[]RecommendPoolStatus` | 是 | — | — |

### `RecommendRecallLogData`

> RecommendRecallLogData found 表达「服务有没有回这一条」：GetRecallRequestLog 在 entry 为 / null 时（未命中）不能靠全零值的 entry 冒充命中，与 live 的 has_setting 同一口径。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Entry` | `entry` | json | `RecommendRequestLog` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |

### `RecommendRecallLogListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]RecommendRequestLog` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |

### `RankDecisionData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Entry` | `entry` | json | `RankDecisionInfo` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | entry 为 null（未命中）时 false，不拿全零值冒充命中 |

### `RankDecisionListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]RankDecisionInfo` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |

### `RankRuntimeConfigData`

> RankRuntimeConfigData 当前生效的模型/特征/实验与在线参数。 / active_model_version 为空是**必须让后台看见**的事实：此时在线必然降级（契约注释即如此）， / 网关不把它兜成「看起来正常」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ModelKey` | `model_key` | json | `string` | 是 | — | — |
| `ActiveModelVersion` | `active_model_version` | json | `string` | 是 | — | — |
| `FeatureConfigVersion` | `feature_config_version` | json | `string` | 是 | — | — |
| `MaxCandidates` | `max_candidates` | json | `int32` | 是 | — | — |
| `MaxReturn` | `max_return` | json | `int32` | 是 | — | — |
| `Objectives` | `objectives` | json | `[]string` | 是 | — | 受控 key：pred_click/pred_finish/pred_interact/pred_negative |
| `DegradeEnabled` | `degrade_enabled` | json | `bool` | 是 | — | — |
| `Fallback` | `fallback` | json | `int32` | 是 | — | — |
| `ScoreBudgetMs` | `score_budget_ms` | json | `int64` | 是 | — | — |
| `TtlSeconds` | `ttl_seconds` | json | `int64` | 是 | — | — |
| `RunningExperiments` | `running_experiments` | json | `[]RankExperimentInfo` | 是 | — | — |
| `ConfigRevision` | `config_revision` | json | `string` | 是 | — | 配置代次摘要，灰度核对用 |

### `RecommendPoolRef`

>                                       services/recommend-rank/rpc/rank.proto） ==================== / 边界（AGENTS.md §5/§7）：召回池与池版本、召回请求回放归 recommend-recall；模型版本、特征配置、 / A/B 实验与排序决策摘要归 recommend-rank。网关既不在此置顶/加权某个 aid，也不改写推荐结果—— / 两个契约本身就**没有**这类入参（recall.proto 头注释、rank.proto「不提供改写推荐结果的写接口」）， / 运营能动的只有池条目的分批写入与版本切换、模型/特征/实验的登记与状态迁移。 /  / 本域刻意不开的路由（逐条理由见 gateway/admin/README.md）： /   - RecallCandidates / RankCandidates：在线热路径，属 gateway/app 与推荐链路，不是后台查询； /   - GetExperimentAssignment：按主体（mid/设备摘要）查分桶，是用户维度读取， /     递给操作者等于让控制台读某个主体的实验状态，admin 面没有这项能力。 /  / 主体口径（与 cron/live-gateway 同一套，比 live-room 更严）：两个契约的操作者字段都是 / `operator string`（审计用），因此一律由会话 admin_id 渲染成 `admin:<admin_id>`， / **表单不声明 operator**；`UpsertPoolItems`/`PrunePoolVersions` 的 proto 里没有 operator 位 / （前者只有 generator=产出方标识，后者只有 operator 但无 reason），逐条缺口见 README。 /  / 可追溯性缺口：两个契约面向后台的这些方法**都没有 trace_id 字段**（不同于 cron/live）， / 所以链路只能落在网关日志与 request_id/idempotency_key 上，补法是先给 proto 加字段（本轮不改 services/**）。 /  / 分页与批量上限全部沿用服务口径（recall: MaxPoolSnapshotPage 200 / MaxVersionList 100 / / MaxRequestLogPage 100 / MaxBatchItems 1000 / MinKeepVersions 2；rank: MaxDecisionPage 100 / / MaxFeatureKeys 512），由下游夹取或拒绝，网关不擅自放大也不复算池键语法（model.ValidatePoolKey 在服务侧）。 / RecommendPoolRef 池寻址（recall.proto PoolRef）。pool_key 的合法语法由服务判定， / 网关只挡住 source 缺省与 pool_key 空串——否则后台收到的是「池不存在/未就绪」而不是「少传了参数」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | json | `int32` | 是 | — | 1 热门、2 关注、3 标签、4 协同、5 向量、6 冷启动（Source 枚举编号） |
| `PoolKey` | `pool_key` | json | `string` | 是 | — | global / zone:<typeid> / tag:<tag_id> / aid:<seed> / mid:<mid> / platform:<p> |

### `RecommendPoolItemInput`

> RecommendPoolItemInput 待写入的池条目：只有 aid 与分数。分数是否为该路可比的归一值、 / aid 是否真的存在且可见都由 recommend-recall 与 video 的既有事实判定，网关不预筛。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `Score` | `score` | json | `float64` | 是 | — | — |

### `RecommendPoolItemUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Written` | `written` | json | `int32` | 是 | — | — |
| `ItemCount` | `item_count` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | true = 幂等键命中重放，未重复写入 |

### `RecommendPoolVersionSwitchData`

> RecommendPoolVersionSwitchData 切换类结论：switched=false + deduplicated=true 是幂等重放的 / 正常结果（不是失败），previous/current 一并回传，后台据此核对指针是否真的动了。 / event_id 是 recall.pool.published 的 outbox 事件 ID，便于对账下游投影。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Switched` | `switched` | json | `bool` | 是 | — | — |
| `PreviousVersion` | `previous_version` | json | `int64` | 是 | — | — |
| `CurrentVersion` | `current_version` | json | `int64` | 是 | — | — |
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |

### `RecommendPoolVersionPruneData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ScannedVersions` | `scanned_versions` | json | `int32` | 是 | — | — |
| `DeletedRows` | `deleted_rows` | json | `int32` | 是 | — | — |
| `DryRun` | `dry_run` | json | `bool` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | true = 还有可清理内容，需继续分批 |

### `RankObjectiveWeight`

> RankObjectiveWeight 多目标权重：objective 是受控 key，权重取值范围（服务端校验总和不超过 10） / 由 recommend-rank 判定，网关不做归一化也不补默认权重。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Objective` | `objective` | json | `string` | 是 | — | — |
| `Weight` | `weight` | json | `float64` | 是 | — | — |

### `RankModelVersionUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ModelKey` | `model_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Revision` | `revision` | json | `int32` | 是 | — | — |
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | — |

### `RankModelStateSetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Changed` | `changed` | json | `bool` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `PreviousActiveVersion` | `previous_active_version` | json | `string` | 是 | — | — |
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | 契约本期预留（未接 MQ），为空表示尚无事件 |

### `RankFeatureConfigUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConfigVersion` | `config_version` | json | `string` | 是 | — | — |
| `FeatureCount` | `feature_count` | json | `int32` | 是 | — | — |
| `Revision` | `revision` | json | `int32` | 是 | — | — |
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | — |

### `RankExperimentUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Experiment` | `experiment` | json | `RankExperimentInfo` | 是 | — | — |
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | — |

### `RankExperimentStateSetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Changed` | `changed` | json | `bool` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | — |

### `RecommendPoolItem`

> RecommendPoolItem 池条目。只有 aid + 分数 + 版本 + 写入时间：稿件标题/封面/状态一律不在此复制 / （AGENTS.md §5：候选只回传跨服务主键），要看内容去 video 域的后台页按 aid 查。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `Score` | `score` | json | `float64` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `RecommendPoolVersionInfo`

> RecommendPoolVersionInfo 版本行：batch_id + generator 是「这批数据是谁在什么时候产的」的证据， / operator/note 是切换留痕，缺了它们就只能对着版本号猜。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `BatchId` | `batch_id` | json | `string` | 是 | — | — |
| `Generator` | `generator` | json | `string` | 是 | — | — |
| `SchemaVersion` | `schema_version` | json | `int32` | 是 | — | — |
| `ItemCount` | `item_count` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `PublishedAt` | `published_at` | json | `int64` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Note` | `note` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `RecommendPoolStatus`

> RecommendPoolStatus 有 CURRENT 版本的池摘要。stale 只表示「超过 PoolStaleSeconds 没更新」， / 在线仍可读——它不是故障位，网关不把它改写成错误。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Pool` | `pool` | json | `RecommendPoolRef` | 是 | — | — |
| `CurrentVersion` | `current_version` | json | `int64` | 是 | — | — |
| `BatchId` | `batch_id` | json | `string` | 是 | — | — |
| `ItemCount` | `item_count` | json | `int32` | 是 | — | — |
| `PublishedAt` | `published_at` | json | `int64` | 是 | — | — |
| `Stale` | `stale` | json | `bool` | 是 | — | — |

### `RecommendRequestLog`

> RecommendRequestLog 召回请求日志。只有 mid/scene/platform/app_version/region 这类 / 受控维度与摘要字段（versions_digest），契约本身不落设备号，网关也不会去别处拼用户资料。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `SnapshotId` | `snapshot_id` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | 0 游客 |
| `Scene` | `scene` | json | `string` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 是 | — | 1 Android、2 iOS、3 HarmonyOS、4 桌面 |
| `AppVersion` | `app_version` | json | `string` | 是 | — | — |
| `Region` | `region` | json | `string` | 是 | — | — |
| `RequestedSources` | `requested_sources` | json | `[]int32` | 是 | — | — |
| `PerSource` | `per_source` | json | `[]RecommendSourceStat` | 是 | — | — |
| `CandidateCount` | `candidate_count` | json | `int32` | 是 | — | — |
| `ReturnedCount` | `returned_count` | json | `int32` | 是 | — | — |
| `Degraded` | `degraded` | json | `bool` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | DegradeReason，0 表示未降级 |
| `CostMs` | `cost_ms` | json | `int32` | 是 | — | — |
| `VersionsDigest` | `versions_digest` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `RankDecisionInfo`

> RankDecisionInfo 排序决策摘要。降级位是**平铺**的 degraded/reason/fallback（rank.proto 的 / RankDecisionInfo 并不嵌套 DegradationInfo，网关不自造一层）， / result_digest/top_aids 用来人工核对「有没有凭空产生 aid」， / subject_id 是 mid 的十进制字符串或设备 sha256 摘要（契约即如此，明文设备号进不来）， / 它只在响应体里出现，网关日志一律不打（AGENTS.md §7 行为数据脱敏）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DecisionId` | `decision_id` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `SnapshotId` | `snapshot_id` | json | `string` | 是 | — | 回指召回快照，整条链路可回放 |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `SubjectType` | `subject_type` | json | `int32` | 是 | — | 1 mid、2 设备摘要 |
| `SubjectId` | `subject_id` | json | `string` | 是 | — | — |
| `Scene` | `scene` | json | `string` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 是 | — | — |
| `AppVersion` | `app_version` | json | `string` | 是 | — | — |
| `ExpKey` | `exp_key` | json | `string` | 是 | — | — |
| `VariantKey` | `variant_key` | json | `string` | 是 | — | — |
| `BucketNo` | `bucket_no` | json | `int32` | 是 | — | — |
| `ModelKey` | `model_key` | json | `string` | 是 | — | — |
| `ModelVersion` | `model_version` | json | `string` | 是 | — | — |
| `FeatureConfigVersion` | `feature_config_version` | json | `string` | 是 | — | — |
| `InputCount` | `input_count` | json | `int32` | 是 | — | — |
| `ReturnedCount` | `returned_count` | json | `int32` | 是 | — | — |
| `ResultDigest` | `result_digest` | json | `string` | 是 | — | — |
| `TopAids` | `top_aids` | json | `[]int64` | 是 | — | 条数由服务 MaxDigestAids 决定 |
| `Degraded` | `degraded` | json | `bool` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |
| `Fallback` | `fallback` | json | `int32` | 是 | — | — |
| `Filters` | `filters` | json | `RankFilterStat` | 是 | — | — |
| `CostMs` | `cost_ms` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `RankExperimentInfo`

> RankExperimentInfo 实验变体（ExperimentInfo 的完整投影，含 operator/reason 审计列）。 / bucket_start/bucket_end 是 [start,end) 千分位区间，是否合法、同层是否互斥由 recommend-rank 判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ExpKey` | `exp_key` | json | `string` | 是 | — | — |
| `VariantKey` | `variant_key` | json | `string` | 是 | — | — |
| `LayerKey` | `layer_key` | json | `string` | 是 | — | — |
| `BucketStart` | `bucket_start` | json | `int32` | 是 | — | — |
| `BucketEnd` | `bucket_end` | json | `int32` | 是 | — | — |
| `ModelKey` | `model_key` | json | `string` | 是 | — | — |
| `ModelVersion` | `model_version` | json | `string` | 是 | — | — |
| `FeatureConfigVersion` | `feature_config_version` | json | `string` | 是 | — | — |
| `Overrides` | `overrides` | json | `string` | 是 | — | 受控参数覆盖 JSON，契约禁止商业化字段 |
| `State` | `state` | json | `int32` | 是 | — | ExperimentState：1 草稿、2 分流中、3 暂停、4 结束 |
| `Revision` | `revision` | json | `int32` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | 0 表示未设定 |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `RecommendSourceStat`

> RecommendSourceStat 单路取数统计。degraded + error_code 是「这一路为什么没出数」的稳定口径， / 网关不把它折叠成「返回 0 条」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Source` | `source` | json | `int32` | 是 | — | — |
| `Planned` | `planned` | json | `int32` | 是 | — | — |
| `Returned` | `returned` | json | `int32` | 是 | — | — |
| `PoolVersion` | `pool_version` | json | `int64` | 是 | — | — |
| `BatchId` | `batch_id` | json | `string` | 是 | — | — |
| `Degraded` | `degraded` | json | `bool` | 是 | — | — |
| `ErrorCode` | `error_code` | json | `string` | 是 | — | — |

### `RankFilterStat`

> RankFilterStat 过滤与打散统计。契约红线是「不允许静默吞候选」， / 所以五类丢弃计数必须逐列回传给后台，网关不折叠成一个 total_dropped。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SafetyFiltered` | `safety_filtered` | json | `int32` | 是 | — | — |
| `FrequencyFiltered` | `frequency_filtered` | json | `int32` | 是 | — | — |
| `DedupFiltered` | `dedup_filtered` | json | `int32` | 是 | — | — |
| `DiversifiedMoved` | `diversified_moved` | json | `int32` | 是 | — | 只移动位置，不减少条数 |
| `Truncated` | `truncated` | json | `int32` | 是 | — | — |


<!-- file: docs/api/http/admin/22-admin-recommend.md -->
