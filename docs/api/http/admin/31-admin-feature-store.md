# 运营面 · `/admin/feature-store`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 阶段 4 运营面：feature-store 域（特征定义、版本与回填） | 免鉴权 | 5 |
| 阶段 4 运营面：feature-store 域（特征定义、版本与回填） | AdminPermission | 8 |

合计 **13** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 阶段 4 运营面：feature-store 域（特征定义、版本与回填）（免鉴权，5 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/feature-store/definition/get` | 单个特征定义读（version=0 = 当前 ACTIVE 版本；found=false 不伪造口径） | `fsDefinitionGet` | `fsdefinitiongetlogic.go` |
| POST | `/admin/feature-store/definition/list` | 特征定义目录分页（含 DRAFT/RETIRED，可按 scope/source/state/隐私上限过滤） | `fsDefinitionList` | `fsdefinitionlistlogic.go` |
| POST | `/admin/feature-store/version-switch/list` | 版本切换审计列表（谁在什么时候按什么理由切的；列表内读不出切换类别，见契约缺口） | `fsVersionSwitchList` | `fsversionswitchlistlogic.go` |
| POST | `/admin/feature-store/backfill/get` | 回填作业进度（job_id 或 request_id 二选一） | `fsBackfillGet` | `fsbackfillgetlogic.go` |
| POST | `/admin/feature-store/backfill/list` | 回填作业列表（按 key/状态/时间筛） | `fsBackfillList` | `fsbackfilllistlogic.go` |

### POST `/admin/feature-store/definition/get` — 单个特征定义读（version=0 = 当前 ACTIVE 版本；found=false 不伪造口径）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/fsdefinitiongethandler.go`
- 业务实现：`gateway/admin/internal/logic/fsdefinitiongetlogic.go`

请求：`ParamFsDefinitionGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 否 | — | 0 = 当前 ACTIVE 版本 |

响应：`FsDefinitionGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/definition/list` — 特征定义目录分页（含 DRAFT/RETIRED，可按 scope/source/state/隐私上限过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/fsdefinitionlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/fsdefinitionlistlogic.go`

请求：`ParamFsDefinitionList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKeyPrefix` | `feature_key_prefix` | json | `string` | 否 | — | — |
| `EntityScope` | `entity_scope` | json | `int32` | 否 | — | 0 = 不限 |
| `Source` | `source` | json | `int32` | 否 | — | 0 = 不限 |
| `State` | `state` | json | `int32` | 否 | — | 0 = 不限 |
| `MaxPrivacyLevel` | `max_privacy_level` | json | `int32` | 否 | — | 0 = 不限 |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | 服务侧 1..100，0 会被拒 |

响应：`FsDefinitionListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/version-switch/list` — 版本切换审计列表（谁在什么时候按什么理由切的；列表内读不出切换类别，见契约缺口）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/fsversionswitchlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/fsversionswitchlistlogic.go`

请求：`ParamFsVersionSwitchList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 否 | — | 空 = 全部 |
| `Since` | `since` | json | `int64` | 否 | — | 0 = 不限 |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |

响应：`FsVersionSwitchListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsVersionSwitchListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/backfill/get` — 回填作业进度（job_id 或 request_id 二选一）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/fsbackfillgethandler.go`
- 业务实现：`gateway/admin/internal/logic/fsbackfillgetlogic.go`

请求：`ParamFsBackfillGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobId` | `job_id` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

响应：`FsBackfillGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsBackfillGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/backfill/list` — 回填作业列表（按 key/状态/时间筛）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/fsbackfilllisthandler.go`
- 业务实现：`gateway/admin/internal/logic/fsbackfilllistlogic.go`

请求：`ParamFsBackfillList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Since` | `since` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |

响应：`FsBackfillListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsBackfillListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 阶段 4 运营面：feature-store 域（特征定义、版本与回填）（AdminPermission，8 条）

> -------------------- feature-store 受保护面（定向个人数据读 + 全部写） --------------------
> 这一组每一条都有明确后果：导出/擦除针对一个具体主体，注册/上下架/隐私调整/切版本改变所有
> 下游读到的东西，回填与清理是批量数据动作。因此定向个人读与七个写入口各占一个权限点。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/feature-store/entity-feature/list` | 按主体导出特征（隐私核对；可见级别受服务侧上限收敛） | `feature:entity-value` / `read` | `fsEntityFeatureList` | `fsentityfeaturelistlogic.go` |
| POST | `/admin/feature-store/definition/register` | 注册特征版本（只能新增版本；注册一律 DRAFT 入库，不可变字段冲突服务直接拒） | `feature:definition` / `create` | `fsDefinitionRegister` | `fsdefinitionregisterlogic.go` |
| POST | `/admin/feature-store/definition/state` | 特征状态迁移（DRAFT/ACTIVE/RETIRED；上线前置条件由服务判） | `feature:definition` / `state` | `fsDefinitionState` | `fsdefinitionstatelogic.go` |
| POST | `/admin/feature-store/definition/privacy` | 隐私级别调整（独立入口独立留痕：只改谁能读，不改值语义） | `feature:definition` / `privacy` | `fsDefinitionPrivacy` | `fsdefinitionprivacylogic.go` |
| POST | `/admin/feature-store/version/switch` | 切换对外生效的版本（乐观校验 + 追加式审计；回滚在审计里读不出来） | `feature:active-version` / `switch` | `fsVersionSwitch` | `fsversionswitchlogic.go` |
| POST | `/admin/feature-store/backfill/submit` | 提交回填作业（补历史值；worker 未接线前只落 PENDING 台账） | `feature:backfill-job` / `create` | `fsBackfillSubmit` | `fsbackfillsubmitlogic.go` |
| POST | `/admin/feature-store/entity-feature/erase` | 按主体擦除个体特征（隐私工单执行，物理删除不可逆；服务侧 operator 白名单再判一次） | `feature:entity-value` / `erase` | `fsEntityFeatureErase` | `fsentityfeatureeraselogic.go` |
| POST | `/admin/feature-store/retention/purge` | 手动清理 TTL 过期值（cron 的补收敛入口；limit 上限与未来截止时间由服务判） | `feature:value` / `purge` | `fsRetentionPurge` | `fsretentionpurgelogic.go` |

### POST `/admin/feature-store/entity-feature/list` — 按主体导出特征（隐私核对；可见级别受服务侧上限收敛）

- 权限口径：AdminPermission · 权限点 `feature:entity-value` / `read`
- goctl 入口：`gateway/admin/internal/handler/fsentityfeaturelisthandler.go`
- 业务实现：`gateway/admin/internal/logic/fsentityfeaturelistlogic.go`

请求：`ParamFsEntityFeatureList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | 必填且 > 0 |
| `EntityId` | `entity_id` | json | `string` | 是 | — | 必填：主键十进制串或受控哈希摘要，形态由服务判 |
| `MinPrivacyLevel` | `min_privacy_level` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |

响应：`FsEntityFeatureListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsEntityFeatureListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/definition/register` — 注册特征版本（只能新增版本；注册一律 DRAFT 入库，不可变字段冲突服务直接拒）

- 权限口径：AdminPermission · 权限点 `feature:definition` / `create`
- goctl 入口：`gateway/admin/internal/handler/fsdefinitionregisterhandler.go`
- 业务实现：`gateway/admin/internal/logic/fsdefinitionregisterlogic.go`

请求：`ParamFsDefinitionRegister`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `FsFeatureDefinitionInput` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`FsDefinitionRegisterResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionRegisterData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/definition/state` — 特征状态迁移（DRAFT/ACTIVE/RETIRED；上线前置条件由服务判）

- 权限口径：AdminPermission · 权限点 `feature:definition` / `state`
- goctl 入口：`gateway/admin/internal/handler/fsdefinitionstatehandler.go`
- 业务实现：`gateway/admin/internal/logic/fsdefinitionstatelogic.go`

请求：`ParamFsDefinitionState`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | 必填且 >= 1 |
| `State` | `state` | json | `int32` | 是 | — | 目标状态（0 = UNSPECIFIED 无值） |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`FsDefinitionStateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/definition/privacy` — 隐私级别调整（独立入口独立留痕：只改谁能读，不改值语义）

- 权限口径：AdminPermission · 权限点 `feature:definition` / `privacy`
- goctl 入口：`gateway/admin/internal/handler/fsdefinitionprivacyhandler.go`
- 业务实现：`gateway/admin/internal/logic/fsdefinitionprivacylogic.go`

请求：`ParamFsDefinitionPrivacy`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |
| `PrivacyLevel` | `privacy_level` | json | `int32` | 是 | — | 必填：不允许 UNSPECIFIED |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`FsDefinitionPrivacyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionPrivacyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/version/switch` — 切换对外生效的版本（乐观校验 + 追加式审计；回滚在审计里读不出来）

- 权限口径：AdminPermission · 权限点 `feature:active-version` / `switch`
- goctl 入口：`gateway/admin/internal/handler/fsversionswitchhandler.go`
- 业务实现：`gateway/admin/internal/logic/fsversionswitchlogic.go`

请求：`ParamFsVersionSwitch`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `FromVersion` | `from_version` | json | `int32` | 是 | — | — |
| `ToVersion` | `to_version` | json | `int32` | 是 | — | — |
| `ExpectedFromVersion` | `expected_from_version` | json | `int32` | 否 | — | 0 = 不校验 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`FsVersionSwitchResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsVersionSwitchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/backfill/submit` — 提交回填作业（补历史值；worker 未接线前只落 PENDING 台账）

- 权限口径：AdminPermission · 权限点 `feature:backfill-job` / `create`
- goctl 入口：`gateway/admin/internal/handler/fsbackfillsubmithandler.go`
- 业务实现：`gateway/admin/internal/logic/fsbackfillsubmitlogic.go`

请求：`ParamFsBackfillSubmit`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | 必填且 >= 1 |
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | — |
| `EntityIds` | `entity_ids` | json | `[]string` | 否 | — | 空 = 全量扫描 |
| `WindowFrom` | `window_from` | json | `int64` | 是 | — | — |
| `WindowTo` | `window_to` | json | `int64` | 否 | — | 0 = 当前时间 |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `AutoSwitch` | `auto_switch` | json | `bool` | 否 | — | — |
| `FromVersion` | `from_version` | json | `int32` | 否 | — | auto_switch 的乐观基线，0 = 不校验 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`FsBackfillSubmitResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsBackfillSubmitData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/entity-feature/erase` — 按主体擦除个体特征（隐私工单执行，物理删除不可逆；服务侧 operator 白名单再判一次）

- 权限口径：AdminPermission · 权限点 `feature:entity-value` / `erase`
- goctl 入口：`gateway/admin/internal/handler/fsentityfeatureerasehandler.go`
- 业务实现：`gateway/admin/internal/logic/fsentityfeatureeraselogic.go`

请求：`ParamFsEntityFeatureErase`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | — |
| `EntityId` | `entity_id` | json | `string` | 是 | — | — |
| `MinPrivacyLevel` | `min_privacy_level` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`FsEntityFeatureEraseResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsEntityFeatureEraseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/feature-store/retention/purge` — 手动清理 TTL 过期值（cron 的补收敛入口；limit 上限与未来截止时间由服务判）

- 权限口径：AdminPermission · 权限点 `feature:value` / `purge`
- goctl 入口：`gateway/admin/internal/handler/fsretentionpurgehandler.go`
- 业务实现：`gateway/admin/internal/logic/fsretentionpurgelogic.go`

请求：`ParamFsRetentionPurge`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Limit` | `limit` | json | `int64` | 否 | — | 0 = 服务上限 |
| `Before` | `before` | json | `int64` | 否 | — | 0 = 当前时间 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`FsRetentionPurgeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsRetentionPurgeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamFsDefinitionGet`

> ParamFsDefinitionGet 读单个定义（version=0 = 按 ACTIVE 指针解析）。 / found=false 有两种来源：这个 key 从未注册，或注册了但没有生效版本——两者在服务侧都不伪造口径。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 否 | — | 0 = 当前 ACTIVE 版本 |

### `FsDefinitionGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsDefinitionList`

> ParamFsDefinitionList 定义目录分页（含 DRAFT 与 RETIRED：历史值要靠旧版本解释）。 / 四个过滤位的 0 都是「不限」的合法哨兵，max_privacy_level 是调用方按自身授权收敛的可见上限。 / 前缀是否合法、ps 上限、隐私与 scope 是否自洽由服务判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKeyPrefix` | `feature_key_prefix` | json | `string` | 否 | — | — |
| `EntityScope` | `entity_scope` | json | `int32` | 否 | — | 0 = 不限 |
| `Source` | `source` | json | `int32` | 否 | — | 0 = 不限 |
| `State` | `state` | json | `int32` | 否 | — | 0 = 不限 |
| `MaxPrivacyLevel` | `max_privacy_level` | json | `int32` | 否 | — | 0 = 不限 |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | 服务侧 1..100，0 会被拒 |

### `FsDefinitionListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsVersionSwitchList`

> ParamFsVersionSwitchList 版本切换审计列表（回滚是否发生过、谁在什么时候切的，只能从这里核对）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 否 | — | 空 = 全部 |
| `Since` | `since` | json | `int64` | 否 | — | 0 = 不限 |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |

### `FsVersionSwitchListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsVersionSwitchListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsBackfillGet`

> ParamFsBackfillGet 查回填作业：job_id 与 request_id 二选一（后者是幂等回放用）。 / 两个都不给没有任何可查目标，网关先挡住——否则服务按空主键回一条 not found， / 后台会把「参数没给」读成「作业丢了」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobId` | `job_id` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

### `FsBackfillGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsBackfillGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsBackfillList`

> ParamFsBackfillList 作业列表（按 key/状态/时间筛）。state=0 = 全部。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Since` | `since` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |

### `FsBackfillListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsBackfillListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsEntityFeatureList`

> ParamFsEntityFeatureList 按主体导出特征（隐私核对：擦除前确认范围、自助查询）。 / min_privacy_level=0 是「全部个体特征」的合法哨兵；实际能看到哪一档由服务侧 / Privacy.ExportMaxPrivacyLevel 封顶（超出时服务回空集而不是放宽上限）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | 必填且 > 0 |
| `EntityId` | `entity_id` | json | `string` | 是 | — | 必填：主键十进制串或受控哈希摘要，形态由服务判 |
| `MinPrivacyLevel` | `min_privacy_level` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |

### `FsEntityFeatureListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsEntityFeatureListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsDefinitionRegister`

> ParamFsDefinitionRegister 注册一个特征版本。 / 表单**没有** operator 位：操作者只能由后台会话渲染成 gateway/admin:<admin_id>。 / 「为什么要这个版本」的落点是 change_note（说明位），网关不另造传不出去的字段。 / idempotency_key → request_id 原值透传（改一个字符等于换一次执行权）。 / 命中已存在的 (feature_key, version)：不可变字段全等则 reused=true，任一不同服务回 / ErrFeatureDefinitionImmutable——网关不预读一次列表去判「算不算新增」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `FsFeatureDefinitionInput` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `FsDefinitionRegisterResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionRegisterData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsDefinitionState`

> ParamFsDefinitionState 特征状态迁移（DRAFT/ACTIVE/RETIRED）。 / reason 必填（服务同判）：升 ACTIVE 会改变所有下游读取方拿到的版本，降 RETIRED 会让在线读 / 立刻变成 FEATURE_RETIRED 降级，无理由不受理。version 必须显式（0 不是「最新」）。 / 上线前置（该版本有值或有上一版本、无未完成回填）由服务判，网关不预告结论。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | 必填且 >= 1 |
| `State` | `state` | json | `int32` | 是 | — | 目标状态（0 = UNSPECIFIED 无值） |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `FsDefinitionStateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsDefinitionPrivacy`

> ParamFsDefinitionPrivacy 隐私级别调整：独立入口、独立权限点、独立审计（switch_type=privacy_change）。 / 它不动值语义，只动「谁能读」——收紧会让下游读不到该特征，放宽则是隐私承诺的变化， / 因此绝不跟着 /definition/state 一起授予。级别与 entity_scope 是否自洽由服务判 / （个体维度却标 PUBLIC_AGGREGATE 这种组合在 model.PrivacyMatchesScope 处就被拒）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |
| `PrivacyLevel` | `privacy_level` | json | `int32` | 是 | — | 必填：不允许 UNSPECIFIED |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `FsDefinitionPrivacyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsDefinitionPrivacyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsVersionSwitch`

> ParamFsVersionSwitch 切换对外生效的版本（ACTIVE 指针 from → to）。 / from_version 与 to_version 都要显式且不相等（服务同判）；expected_from_version 是乐观并发校验， / **0 = 不校验**是契约里的合法值（回填作业内部使用），网关不把它当成漏填也不替它填当前值—— / 但后台表单应当带上当初读到的基线，否则等于放弃并发保护。 / reason 必填：切换依据（离线评估结论、回滚单号）。契约表达不出「这是一次回滚」（缺口在服务 README）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `FromVersion` | `from_version` | json | `int32` | 是 | — | — |
| `ToVersion` | `to_version` | json | `int32` | 是 | — | — |
| `ExpectedFromVersion` | `expected_from_version` | json | `int32` | 否 | — | 0 = 不校验 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `FsVersionSwitchResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsVersionSwitchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsBackfillSubmit`

> ParamFsBackfillSubmit 提交回填作业（给某个 DRAFT 版本补历史值）。 / entity_ids 为空是「全量扫描」的合法哨兵，不是漏填；上限 1000 与总字节上限由服务判（超限报错不截断）。 / window_to=0 = 当前时间；auto_switch=true 时 from_version 是乐观基线（0 = 不校验，同上保留语义）。 / 目标版本必须处于 DRAFT（回填完成后才允许切 ACTIVE）、source 必须与定义一致、 / 该不该受理这一轮回填，全部由 feature-store 判定。 / **本服务侧 worker 尚未接线**（Backfill.WorkerEnabled=false），提交只会落到 PENDING 台账， / 进度不会自己前进——网关不伪造「已在跑」的结论，进度以 /backfill/get 的回值为准。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | 必填且 >= 1 |
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | — |
| `EntityIds` | `entity_ids` | json | `[]string` | 否 | — | 空 = 全量扫描 |
| `WindowFrom` | `window_from` | json | `int64` | 是 | — | — |
| `WindowTo` | `window_to` | json | `int64` | 否 | — | 0 = 当前时间 |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `AutoSwitch` | `auto_switch` | json | `bool` | 否 | — | — |
| `FromVersion` | `from_version` | json | `int32` | 否 | — | auto_switch 的乐观基线，0 = 不校验 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `FsBackfillSubmitResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsBackfillSubmitData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsEntityFeatureErase`

> ParamFsEntityFeatureErase 按主体擦除个体特征（隐私工单执行）。 / 物理删除值行（保留定义与审计），不可逆且没有撤销口；min_privacy_level=0 = 全部个体特征。 / reason 必填且写工单号——本域唯一能证明「这次删除是被授权的」的外部线索。 / 真正的授权判定在服务侧两道：operator 前缀白名单（空白名单一律拒绝）+ 隐私级别范围。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | — |
| `EntityId` | `entity_id` | json | `string` | 是 | — | — |
| `MinPrivacyLevel` | `min_privacy_level` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `FsEntityFeatureEraseResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsEntityFeatureEraseData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamFsRetentionPurge`

> ParamFsRetentionPurge 手动触发 TTL 过期值清理（正常由 cron 周期调用，本入口只用于补一轮收敛）。 / limit 由服务夹到硬上限（越界按上限跑完这一轮，不整轮失败）；before=0 = 当前时间； / before 晚于服务时钟会被拒——那会把还没过期的值当过期删掉，是数据丢失而不是清理（网关不比自己钟）。 / 契约缺口：PurgeExpiredReq **没有 reason 位**，手工清理的动机只能留在网关日志与 operation 审计里， / 服务侧回执只有 operator + request_id（已在 README 登记）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Limit` | `limit` | json | `int64` | 否 | — | 0 = 服务上限 |
| `Before` | `before` | json | `int64` | 否 | — | 0 = 当前时间 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `FsRetentionPurgeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `FsRetentionPurgeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `FsDefinitionGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Definition` | `definition` | json | `FsFeatureDefinition` | 是 | — | — |

### `FsDefinitionListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definitions` | `definitions` | json | `[]FsFeatureDefinition` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `FsVersionSwitchListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]FsVersionSwitchRecord` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `FsBackfillGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Job` | `job` | json | `FsBackfillJob` | 是 | — | — |

### `FsBackfillListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Jobs` | `jobs` | json | `[]FsBackfillJob` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `FsEntityFeatureListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Entries` | `entries` | json | `[]FsFeatureEntry` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `FsFeatureDefinitionInput`

> FsFeatureDefinitionInput 是注册新版本的表单位： / 没有 state 位——注册一律以 DRAFT 入库（服务侧强制，「注册即生效」等于绕过评审）， / 传 state=ACTIVE 只会换来一次 ErrFeatureStateTransition；上线只能走 /definition/state。 / 也没有 created_by/ctime/mtime：经办人与库时钟由服务按会话渲染，网关自报等于伪造审计主体。 / 隐私级别、TTL、维度、默认值可否解析、来源是否要求窗口，全部由 feature-store 判定（§5）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | 必填且 >= 1 |
| `Name` | `name` | json | `string` | 否 | — | — |
| `ValueType` | `value_type` | json | `int32` | 是 | — | — |
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | — |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `PrivacyLevel` | `privacy_level` | json | `int32` | 是 | — | 必填：未声明隐私级别的特征不允许存在 |
| `WindowSeconds` | `window_seconds` | json | `int64` | 否 | — | — |
| `TTLSeconds` | `ttl_seconds` | json | `int64` | 是 | — | — |
| `DefaultValue` | `default_value` | json | `string` | 否 | — | — |
| `Dimension` | `dimension` | json | `int32` | 否 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `ChangeNote` | `change_note` | json | `string` | 否 | — | — |

### `FsDefinitionRegisterData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Created` | `created` | json | `bool` | 是 | — | true = 真的新增了一个版本 |
| `Reused` | `reused` | json | `bool` | 是 | — | true = 幂等键命中已有请求 |
| `Definition` | `definition` | json | `FsFeatureDefinition` | 是 | — | — |

### `FsDefinitionStateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `FsFeatureDefinition` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |

### `FsDefinitionPrivacyData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Definition` | `definition` | json | `FsFeatureDefinition` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |

### `FsVersionSwitchData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Switched` | `switched` | json | `bool` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |
| `ActiveVersion` | `active_version` | json | `int32` | 是 | — | 切换后对外生效的版本 |
| `SwitchId` | `switch_id` | json | `int64` | 是 | — | — |

### `FsBackfillSubmitData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobId` | `job_id` | json | `int64` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |
| `Job` | `job` | json | `FsBackfillJob` | 是 | — | — |

### `FsEntityFeatureEraseData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ErasedRows` | `erased_rows` | json | `int32` | 是 | — | — |
| `FeaturesTouched` | `features_touched` | json | `int32` | 是 | — | 口径是「本次真删到的行里 distinct feature_key」 |
| `Reused` | `reused` | json | `bool` | 是 | — | — |

### `FsRetentionPurgeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Purged` | `purged` | json | `int32` | 是 | — | 本次删除行数 |
| `Remaining` | `remaining` | json | `int64` | 是 | — | 剩余过期行数估计（允许近似，供收敛判断） |
| `Reused` | `reused` | json | `bool` | 是 | — | — |

### `FsFeatureDefinition`

> 契约来源 services/feature-store/rpc/featurestore.proto（冻结）。本服务是「在线特征读什么版本、 / 读不到时怎么显式降级、版本是谁在什么时候按什么理由切的」的所有者；它**不是**特征计算引擎 / （完播率/热度/兴趣的口径属 spm，AGENTS.md §7），也不是广告或商业化分析存储。 /  / 本域三条硬口径，决定了下面为什么只开这 13 条路由： /   1. **后台不写特征值**：WriteFeatures 不接路由——值是计算链路的产物（spm 指标投影、离线模型 /      回填、风控自有滑窗），proto 要求 writer 身份必须与特征定义的 source 一致或为受控系统。 /      开一个后台写口等于让「运营觉得这个用户该被这样理解」变成在线特征，排序侧会立刻把它读进去， /      而且这条值没有任何上游能重算出来（回源与重放依据就此失真）； /   2. **后台不碰在线热路径**：GetFeature/BatchGetFeatures 不接路由——它们是 recommend-* 每次排序 /      都要打的读接口（50 特征 × 20 主体 + 1 MiB 三道闸），后台列表页刷新不该消耗这份配额， /      也不该出现「排障页把批量读打满、真实排序读不到特征」的自伤。要看单个主体的特征走 /      /entity-feature/list（隐私核对用，挂权限点），要看口径走 /definition/get； /   3. **隐私动作只走专属入口**：EraseEntityFeatures 与 ListEntityFeatures 都带明确的被擦/被读主体， /      前者不可逆（值物理删除，只保留定义与审计），因此两者各占一个权限点，不并进 definition 域。 /      本服务侧还有第二道闸：operator 必须命中 feature-store 的 Privacy.OperatorPrefixes 白名单， /      **空白名单 = 谁都拒绝**，网关不代替它放行，也不猜测擦除结果。 /  / 时间统一 Unix 秒；分页沿用契约 pn/ps，但**本页 ps 没有 0 = 默认页的语义**（服务侧判 1..100， / 0 会被拒），因此后台表单必须显式给页大小，网关不替调用方填默认值。 / 全部请求体都带 entity_id/feature 值这类个体维度数据，一律不进网关日志（§7 行为数据脱敏）。 / FsFeatureDefinition 1:1 对应 rpc.FeatureDefinition（服务回读的定义行）。 / value_type/entity_scope/source 三位注册后不可变，跨版本必须摘要一致——所以本域没有 DELETE， / 口径要变只能注册新版本再切指针（否则同一个 key 的含义会在排序侧脚下漂移）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `ValueType` | `value_type` | json | `int32` | 是 | — | — |
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | — |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `PrivacyLevel` | `privacy_level` | json | `int32` | 是 | — | — |
| `WindowSeconds` | `window_seconds` | json | `int64` | 是 | — | 0 = 无窗口（静态属性） |
| `TTLSeconds` | `ttl_seconds` | json | `int64` | 是 | — | <=0 服务侧一律拒绝注册 |
| `DefaultValue` | `default_value` | json | `string` | 是 | — | 降级用的默认值（按 value_type 序列化的字符串形态） |
| `Dimension` | `dimension` | json | `int32` | 是 | — | 列表/向量类的元素个数上限，标量类为 0 |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | 口径说明：为什么存在、怎么算（服务侧必填） |
| `ChangeNote` | `change_note` | json | `string` | 是 | — | — |
| `CreatedBy` | `created_by` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `FsVersionSwitchRecord`

> FsVersionSwitchRecord 是 rpc.ListVersionSwitchesReply.SwitchRecord 的投影（只追加的审计行）。 / 契约缺口：**SwitchRecord 没有 switch_type 位**，因此后台从这条列表里读不出某一行属于 / 激活/切换/回滚/状态变更/隐私变更/回填自动切换中的哪一类，也读不出「这次是不是回滚」 / （实现只在 to_version == previous_version 时把审计记成 rollback，见服务 README 缺口 4）。 / 网关不猜：原样回列表，判类别属下一轮契约改动。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SwitchId` | `switch_id` | json | `int64` | 是 | — | — |
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `FromVersion` | `from_version` | json | `int32` | 是 | — | — |
| `ToVersion` | `to_version` | json | `int32` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `FsBackfillJob`

> FsBackfillJob 1:1 对应 rpc.BackfillJob：回填台账（范围、进度、断点、错误）。 / entities_total 在全量扫描提交时固定为 0（服务没有跨域「主体全集」可读，猜分母等于说谎）， / 由 worker 开跑后填；cursor_entity_id 是断点续跑游标（按主键升序）。 / 契约缺口：作业投影里**没有 from_version**（auto_switch 的乐观基线只在提交入参里出现过一次）， / 事后复查作业无法回答「当初是以哪个基线换的约」，只能回 /version-switch/list 比对时间线。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `JobId` | `job_id` | json | `int64` | 是 | — | — |
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | — |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `WindowFrom` | `window_from` | json | `int64` | 是 | — | — |
| `WindowTo` | `window_to` | json | `int64` | 是 | — | — |
| `EntitiesTotal` | `entities_total` | json | `int64` | 是 | — | — |
| `EntitiesDone` | `entities_done` | json | `int64` | 是 | — | — |
| `EntitiesFailed` | `entities_failed` | json | `int64` | 是 | — | — |
| `CursorEntityId` | `cursor_entity_id` | json | `int64` | 是 | — | — |
| `AutoSwitch` | `auto_switch` | json | `bool` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `LastError` | `last_error` | json | `string` | 是 | — | 服务侧截断保存，不含 SQL 与特征值原文 |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `FinishedAt` | `finished_at` | json | `int64` | 是 | — | — |

### `FsFeatureEntry`

> FsFeatureEntry 是 rpc.FeatureEntry 的后台投影（把 feature/entity 两层引用摊平， / 语义与字段一位不减）。resolved_version 与 degradation 是这条投影存在的理由： / 「服务实际给了哪个版本」和「为什么给的是这个值」必须原样回给后台， / 网关不折叠成 value=0、也不把降级伪装成正常值。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FeatureKey` | `feature_key` | json | `string` | 是 | — | — |
| `FeatureVersion` | `feature_version` | json | `int32` | 是 | — | 请求侧要求的版本（0 = 按 ACTIVE 指针） |
| `EntityScope` | `entity_scope` | json | `int32` | 是 | — | — |
| `EntityId` | `entity_id` | json | `string` | 是 | — | 个体标识：只回给调用方，永不进网关日志 |
| `Value` | `value` | json | `FsFeatureValue` | 是 | — | — |
| `ResolvedVersion` | `resolved_version` | json | `int32` | 是 | — | 实际返回的版本，可能与请求不同 |
| `Degradation` | `degradation` | json | `int32` | 是 | — | 必有值；NONE 才是正常 |
| `EventTime` | `event_time` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | 0 = 未设 |
| `SourceMetricKey` | `source_metric_key` | json | `string` | 是 | — | 上游口径追溯：来自 spm 时是 "<metric_key>@v<n>" |
| `TTLSeconds` | `ttl_seconds` | json | `int64` | 是 | — | — |

### `FsFeatureValue`

> FsFeatureValue 1:1 对应 rpc.FeatureValue：按 value_type 取用对应的一位， / 多余填充在服务的 payload 转换里就是格式错误（网关不代为挑选或清零）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ValueType` | `value_type` | json | `int32` | 是 | — | — |
| `Int64Value` | `int64_value` | json | `int64` | 是 | — | — |
| `DoubleValue` | `double_value` | json | `float64` | 是 | — | — |
| `BoolValue` | `bool_value` | json | `bool` | 是 | — | — |
| `StringValue` | `string_value` | json | `string` | 是 | — | — |
| `Int64List` | `int64_list` | json | `[]int64` | 是 | — | — |
| `DoubleList` | `double_list` | json | `[]float64` | 是 | — | — |


<!-- file: docs/api/http/admin/31-admin-feature-store.md -->
