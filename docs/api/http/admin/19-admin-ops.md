# 运营面 · `/admin/ops`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| ops-config 域运营路由（services/ops-config/rpc/opsconfig.proto） | 免鉴权 | 7 |
| ops-config 域运营路由（services/ops-config/rpc/opsconfig.proto） | AdminPermission | 10 |

合计 **17** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## ops-config 域运营路由（services/ops-config/rpc/opsconfig.proto）（免鉴权，7 条）

> 只读面：配置项、版本历史、灰度规则、专题、推荐位、开关。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/ops/config/list` | 分页查询配置项（scope/keyword/state 过滤，ps 上限 100） | `opsConfigListConfigs` | `opsconfiglistconfigslogic.go` |
| POST | `/admin/ops/config/versions` | 分页查询某配置键的不可变版本历史（含当前正式版本号） | `opsConfigListVersions` | `opsconfiglistversionslogic.go` |
| POST | `/admin/ops/rollout/list` | 分页查询灰度规则（cfg_key/version/state 过滤） | `opsListRolloutRules` | `opslistrolloutruleslogic.go` |
| POST | `/admin/ops/topic/get` | 专题详情（with_items=true 时附带条目，未命中返回 found=false） | `opsGetTopic` | `opsgettopiclogic.go` |
| POST | `/admin/ops/topic/list` | 分页查询专题（state/zone/tag/keyword 过滤，online_only 附加生效窗口判定） | `opsListTopics` | `opslisttopicslogic.go` |
| POST | `/admin/ops/slot/list` | 分页查询推荐位定义（page/state/platform 过滤） | `opsListSlots` | `opslistslotslogic.go` |
| POST | `/admin/ops/switch/list` | 分页查询客户端开关（platform/switch_key/enabled 过滤） | `opsListSwitches` | `opslistswitcheslogic.go` |

### POST `/admin/ops/config/list` — 分页查询配置项（scope/keyword/state 过滤，ps 上限 100）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/opsconfiglistconfigshandler.go`
- 业务实现：`gateway/admin/internal/logic/opsconfiglistconfigslogic.go`

请求：`ParamOpsListConfigs`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OpsConfigsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsConfigsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/config/versions` — 分页查询某配置键的不可变版本历史（含当前正式版本号）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/opsconfiglistversionshandler.go`
- 业务实现：`gateway/admin/internal/logic/opsconfiglistversionslogic.go`

请求：`ParamOpsListConfigVersions`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OpsConfigVersionsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsConfigVersionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/rollout/list` — 分页查询灰度规则（cfg_key/version/state 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/opslistrolloutruleshandler.go`
- 业务实现：`gateway/admin/internal/logic/opslistrolloutruleslogic.go`

请求：`ParamOpsListRolloutRules`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 否 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Version` | `version` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OpsRolloutRulesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsRolloutRulesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/topic/get` — 专题详情（with_items=true 时附带条目，未命中返回 found=false）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/opsgettopichandler.go`
- 业务实现：`gateway/admin/internal/logic/opsgettopiclogic.go`

请求：`ParamOpsGetTopic`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `TopicId` | `topic_id` | json | `int64` | 否 | — | — |
| `Slug` | `slug` | json | `string` | 否 | — | — |
| `WithItems` | `with_items` | json | `bool` | 否 | — | — |
| `ItemLimit` | `item_limit` | json | `int32` | 否 | — | <=0 按服务端默认 100 |

响应：`OpsTopicDetailResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsTopicDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/topic/list` — 分页查询专题（state/zone/tag/keyword 过滤，online_only 附加生效窗口判定）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/opslisttopicshandler.go`
- 业务实现：`gateway/admin/internal/logic/opslisttopicslogic.go`

请求：`ParamOpsListTopics`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `ZoneId` | `zone_id` | json | `int64` | 否 | — | — |
| `TagId` | `tag_id` | json | `int64` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |
| `OnlineOnly` | `online_only` | json | `bool` | 否 | — | — |

响应：`OpsTopicsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsTopicsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/slot/list` — 分页查询推荐位定义（page/state/platform 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/opslistslotshandler.go`
- 业务实现：`gateway/admin/internal/logic/opslistslotslogic.go`

请求：`ParamOpsListSlots`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `Page` | `page` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Platform` | `platform` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OpsSlotsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSlotsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/switch/list` — 分页查询客户端开关（platform/switch_key/enabled 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/opslistswitcheshandler.go`
- 业务实现：`gateway/admin/internal/logic/opslistswitcheslogic.go`

请求：`ParamOpsListSwitches`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 否 | — | — |
| `SwitchKey` | `switch_key` | json | `string` | 否 | — | — |
| `Enabled` | `enabled` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OpsSwitchesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSwitchesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## ops-config 域运营路由（services/ops-config/rpc/opsconfig.proto）（AdminPermission，10 条）

> 写面：发布/回滚、灰度规则、专题与坑位、开关、缓存刷新。全部要求 ctx.request_id。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/ops/config/publish` | 发布配置新版本（expect_version 乐观锁，可一并挂灰度规则，写 audit） | `ops:config` / `publish` | `opsPublishConfig` | `opspublishconfiglogic.go` |
| POST | `/admin/ops/config/rollback` | 回滚到历史版本（生成新版本而不是删历史，change_type=rollback） | `ops:config` / `rollback` | `opsRollbackConfig` | `opsrollbackconfiglogic.go` |
| POST | `/admin/ops/rollout/save` | 新建/更新灰度规则（按 cfg_key+version+name upsert） | `ops:rollout` / `update` | `opsSaveRolloutRule` | `opssaverolloutrulelogic.go` |
| POST | `/admin/ops/rollout/state` | 启停灰度规则（软状态切换，保留放量证据） | `ops:rollout` / `enable` | `opsSetRolloutState` | `opssetrolloutstatelogic.go` |
| POST | `/admin/ops/topic/save` | 新建/更新专题（zone_ids/tag_ids 全量覆盖引用，expect_version 乐观锁） | `ops:topic` / `update` | `opsSaveTopic` | `opssavetopiclogic.go` |
| POST | `/admin/ops/topic/items/save` | 全量覆盖专题条目（最多 500 条，position 从 1 连续） | `ops:topic_item` / `update` | `opsSaveTopicItems` | `opssavetopicitemslogic.go` |
| POST | `/admin/ops/slot/save` | 新建/更新推荐位定义（code 唯一，capacity 上限由 ops-config 判定） | `ops:slot` / `update` | `opsSaveSlot` | `opssaveslotlogic.go` |
| POST | `/admin/ops/slot/items/save` | 全量覆盖坑位条目与排期（最多 200 条，position 不重复且不超过 capacity） | `ops:slot_item` / `update` | `opsSaveSlotItems` | `opssaveslotitemslogic.go` |
| POST | `/admin/ops/switch/save` | 新建/更新客户端开关（按 switch_key + platform upsert） | `ops:switch` / `update` | `opsSaveSwitch` | `opssaveswitchlogic.go` |
| POST | `/admin/ops/cache/refresh` | 主动失效运行时缓存（target config/topic/slot/all，递增 epoch 并写审计） | `ops:cache` / `refresh` | `opsRefreshCache` | `opsrefreshcachelogic.go` |

### POST `/admin/ops/config/publish` — 发布配置新版本（expect_version 乐观锁，可一并挂灰度规则，写 audit）

- 权限口径：AdminPermission · 权限点 `ops:config` / `publish`
- goctl 入口：`gateway/admin/internal/handler/opspublishconfighandler.go`
- 业务实现：`gateway/admin/internal/logic/opspublishconfiglogic.go`

请求：`ParamOpsPublishConfig`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | 空由 ops-config 默认 global |
| `ValueType` | `value_type` | json | `int32` | 否 | — | — |
| `Value` | `value` | json | `string` | 是 | — | — |
| `ExpectVersion` | `expect_version` | json | `int64` | 是 | — | 当前生效版本号，0 表示新建 |
| `Reason` | `reason` | json | `string` | 是 | — | 变更原因，透传给 audit |
| `Rollout` | `rollout` | json | `[]OpsRolloutRuleSpec` | 否 | — | — |

响应：`OpsPublishResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsPublishData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/config/rollback` — 回滚到历史版本（生成新版本而不是删历史，change_type=rollback）

- 权限口径：AdminPermission · 权限点 `ops:config` / `rollback`
- goctl 入口：`gateway/admin/internal/handler/opsrollbackconfighandler.go`
- 业务实现：`gateway/admin/internal/logic/opsrollbackconfiglogic.go`

请求：`ParamOpsRollbackConfig`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `ToVersion` | `to_version` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`OpsRollbackResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsRollbackData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/rollout/save` — 新建/更新灰度规则（按 cfg_key+version+name upsert）

- 权限口径：AdminPermission · 权限点 `ops:rollout` / `update`
- goctl 入口：`gateway/admin/internal/handler/opssaverolloutrulehandler.go`
- 业务实现：`gateway/admin/internal/logic/opssaverolloutrulelogic.go`

请求：`ParamOpsSaveRolloutRule`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Rule` | `rule` | json | `OpsRolloutRuleSpec` | 是 | — | — |

响应：`OpsRolloutRuleResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsRolloutRuleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/rollout/state` — 启停灰度规则（软状态切换，保留放量证据）

- 权限口径：AdminPermission · 权限点 `ops:rollout` / `enable`
- goctl 入口：`gateway/admin/internal/handler/opssetrolloutstatehandler.go`
- 业务实现：`gateway/admin/internal/logic/opssetrolloutstatelogic.go`

请求：`ParamOpsSetRolloutState`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `RuleId` | `rule_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 生效、2 停用 |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`OpsRolloutRuleResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsRolloutRuleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/topic/save` — 新建/更新专题（zone_ids/tag_ids 全量覆盖引用，expect_version 乐观锁）

- 权限口径：AdminPermission · 权限点 `ops:topic` / `update`
- goctl 入口：`gateway/admin/internal/handler/opssavetopichandler.go`
- 业务实现：`gateway/admin/internal/logic/opssavetopiclogic.go`

请求：`ParamOpsSaveTopic`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `TopicId` | `topic_id` | json | `int64` | 否 | — | 0 新建 |
| `Slug` | `slug` | json | `string` | 否 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `Cover` | `cover` | json | `string` | 否 | — | — |
| `ZoneIds` | `zone_ids` | json | `[]int64` | 否 | — | 全量覆盖 |
| `TagIds` | `tag_ids` | json | `[]int64` | 否 | — | 全量覆盖 |
| `State` | `state` | json | `int32` | 否 | — | 0 视为 2（草稿默认不上架） |
| `Sort` | `sort` | json | `int32` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `ExpectVersion` | `expect_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`OpsTopicResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsTopicData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/topic/items/save` — 全量覆盖专题条目（最多 500 条，position 从 1 连续）

- 权限口径：AdminPermission · 权限点 `ops:topic_item` / `update`
- goctl 入口：`gateway/admin/internal/handler/opssavetopicitemshandler.go`
- 业务实现：`gateway/admin/internal/logic/opssavetopicitemslogic.go`

请求：`ParamOpsSaveTopicItems`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `TopicId` | `topic_id` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]OpsTopicItemSpec` | 是 | — | 全量覆盖，最多 500 条 |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`OpsTopicItemsSaveResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsTopicItemsSaveData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/slot/save` — 新建/更新推荐位定义（code 唯一，capacity 上限由 ops-config 判定）

- 权限口径：AdminPermission · 权限点 `ops:slot` / `update`
- goctl 入口：`gateway/admin/internal/handler/opssaveslothandler.go`
- 业务实现：`gateway/admin/internal/logic/opssaveslotlogic.go`

请求：`ParamOpsSaveSlot`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `SlotId` | `slot_id` | json | `int64` | 否 | — | — |
| `Code` | `code` | json | `string` | 是 | — | — |
| `Page` | `page` | json | `string` | 否 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Platforms` | `platforms` | json | `[]int32` | 否 | — | — |
| `Capacity` | `capacity` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | 0 视为 2 |
| `ExpectVersion` | `expect_version` | json | `int64` | 否 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`OpsSlotResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSlotData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/slot/items/save` — 全量覆盖坑位条目与排期（最多 200 条，position 不重复且不超过 capacity）

- 权限口径：AdminPermission · 权限点 `ops:slot_item` / `update`
- goctl 入口：`gateway/admin/internal/handler/opssaveslotitemshandler.go`
- 业务实现：`gateway/admin/internal/logic/opssaveslotitemslogic.go`

请求：`ParamOpsSaveSlotItems`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `SlotId` | `slot_id` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]OpsSlotItemSpec` | 是 | — | 全量覆盖，最多 200 条且 position 不重复 |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`OpsSlotItemsSaveResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSlotItemsSaveData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/switch/save` — 新建/更新客户端开关（按 switch_key + platform upsert）

- 权限口径：AdminPermission · 权限点 `ops:switch` / `update`
- goctl 入口：`gateway/admin/internal/handler/opssaveswitchhandler.go`
- 业务实现：`gateway/admin/internal/logic/opssaveswitchlogic.go`

请求：`ParamOpsSaveSwitch`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `SwitchId` | `switch_id` | json | `int64` | 否 | — | 0 新建（按 switch_key + platform upsert） |
| `SwitchKey` | `switch_key` | json | `string` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 是 | — | — |
| `MinVersion` | `min_version` | json | `string` | 否 | — | — |
| `MaxVersion` | `max_version` | json | `string` | 否 | — | — |
| `Enabled` | `enabled` | json | `int32` | 否 | — | 0 视为 2，避免误放量 |
| `ConfigId` | `config_id` | json | `int64` | 否 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |
| `ExpectVersion` | `expect_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`OpsSwitchResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSwitchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/ops/cache/refresh` — 主动失效运行时缓存（target config/topic/slot/all，递增 epoch 并写审计）

- 权限口径：AdminPermission · 权限点 `ops:cache` / `refresh`
- goctl 入口：`gateway/admin/internal/handler/opsrefreshcachehandler.go`
- 业务实现：`gateway/admin/internal/logic/opsrefreshcachelogic.go`

请求：`ParamOpsRefreshCache`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `Target` | `target` | json | `string` | 是 | — | config / topic / slot / all |
| `CfgKey` | `cfg_key` | json | `string` | 否 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `TopicId` | `topic_id` | json | `int64` | 否 | — | — |
| `SlotId` | `slot_id` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`OpsRefreshCacheResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsRefreshCacheData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamOpsListConfigs`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OpsConfigsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsConfigsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsListConfigVersions`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OpsConfigVersionsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsConfigVersionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsListRolloutRules`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 否 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Version` | `version` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OpsRolloutRulesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsRolloutRulesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsGetTopic`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `TopicId` | `topic_id` | json | `int64` | 否 | — | — |
| `Slug` | `slug` | json | `string` | 否 | — | — |
| `WithItems` | `with_items` | json | `bool` | 否 | — | — |
| `ItemLimit` | `item_limit` | json | `int32` | 否 | — | <=0 按服务端默认 100 |

### `OpsTopicDetailResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsTopicDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsListTopics`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `ZoneId` | `zone_id` | json | `int64` | 否 | — | — |
| `TagId` | `tag_id` | json | `int64` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |
| `OnlineOnly` | `online_only` | json | `bool` | 否 | — | — |

### `OpsTopicsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsTopicsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsListSlots`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `Page` | `page` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Platform` | `platform` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OpsSlotsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSlotsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsListSwitches`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 否 | — | — |
| `SwitchKey` | `switch_key` | json | `string` | 否 | — | — |
| `Enabled` | `enabled` | json | `int32` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OpsSwitchesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSwitchesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsPublishConfig`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | 空由 ops-config 默认 global |
| `ValueType` | `value_type` | json | `int32` | 否 | — | — |
| `Value` | `value` | json | `string` | 是 | — | — |
| `ExpectVersion` | `expect_version` | json | `int64` | 是 | — | 当前生效版本号，0 表示新建 |
| `Reason` | `reason` | json | `string` | 是 | — | 变更原因，透传给 audit |
| `Rollout` | `rollout` | json | `[]OpsRolloutRuleSpec` | 否 | — | — |

### `OpsPublishResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsPublishData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsRollbackConfig`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `ToVersion` | `to_version` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `OpsRollbackResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsRollbackData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsSaveRolloutRule`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Rule` | `rule` | json | `OpsRolloutRuleSpec` | 是 | — | — |

### `OpsRolloutRuleResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsRolloutRuleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsSetRolloutState`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `RuleId` | `rule_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 生效、2 停用 |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `ParamOpsSaveTopic`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `TopicId` | `topic_id` | json | `int64` | 否 | — | 0 新建 |
| `Slug` | `slug` | json | `string` | 否 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `Cover` | `cover` | json | `string` | 否 | — | — |
| `ZoneIds` | `zone_ids` | json | `[]int64` | 否 | — | 全量覆盖 |
| `TagIds` | `tag_ids` | json | `[]int64` | 否 | — | 全量覆盖 |
| `State` | `state` | json | `int32` | 否 | — | 0 视为 2（草稿默认不上架） |
| `Sort` | `sort` | json | `int32` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `ExpectVersion` | `expect_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `OpsTopicResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsTopicData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsSaveTopicItems`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `TopicId` | `topic_id` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]OpsTopicItemSpec` | 是 | — | 全量覆盖，最多 500 条 |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `OpsTopicItemsSaveResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsTopicItemsSaveData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsSaveSlot`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `SlotId` | `slot_id` | json | `int64` | 否 | — | — |
| `Code` | `code` | json | `string` | 是 | — | — |
| `Page` | `page` | json | `string` | 否 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Platforms` | `platforms` | json | `[]int32` | 否 | — | — |
| `Capacity` | `capacity` | json | `int32` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | 0 视为 2 |
| `ExpectVersion` | `expect_version` | json | `int64` | 否 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `OpsSlotResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSlotData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsSaveSlotItems`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `SlotId` | `slot_id` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]OpsSlotItemSpec` | 是 | — | 全量覆盖，最多 200 条且 position 不重复 |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `OpsSlotItemsSaveResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSlotItemsSaveData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsSaveSwitch`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `SwitchId` | `switch_id` | json | `int64` | 否 | — | 0 新建（按 switch_key + platform upsert） |
| `SwitchKey` | `switch_key` | json | `string` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 是 | — | — |
| `MinVersion` | `min_version` | json | `string` | 否 | — | — |
| `MaxVersion` | `max_version` | json | `string` | 否 | — | — |
| `Enabled` | `enabled` | json | `int32` | 否 | — | 0 视为 2，避免误放量 |
| `ConfigId` | `config_id` | json | `int64` | 否 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |
| `ExpectVersion` | `expect_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `OpsSwitchResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsSwitchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOpsRefreshCache`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Ctx` | `ctx` | json | `OpsCallContext` | 是 | — | — |
| `Target` | `target` | json | `string` | 是 | — | config / topic / slot / all |
| `CfgKey` | `cfg_key` | json | `string` | 否 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `TopicId` | `topic_id` | json | `int64` | 否 | — | — |
| `SlotId` | `slot_id` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `OpsRefreshCacheResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OpsRefreshCacheData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `OpsCallContext`

> ops-config 是「发布式配置」：不可变版本快照 + 灰度规则 + 回滚再生成新版本， / 与 operation.op_config 的一行一键值对并存（边界与待评审项见 services/ops-config/README.md）。 / 字段口径逐项对齐 opsconfig.v1.*，网关不发明 ops-config 没有的方法： /   * ResolveConfig/BatchResolveConfig/ResolveSlot 是面向端的运行时解析，归 gateway/app， /     后台不开放（后台预览走 PublishConfig 的 expect_version 与 ListConfigVersions）； /   * 分区/标签只存 zone_ids/tag_ids 引用，展示名由调用方经 catalog RPC 解析，本契约不复制主资料； /   * 推荐位/坑位不含任何投放、计费、分成字段（AGENTS.md §1 商业化范围外）； /   * audit_entry_id 是 ops-config 写 audit 后回传的证据引用，0 表示审计待补偿， /     网关不把它当成失败，也不自行补写审计（同一事实只有一个所有者）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `OperatorName` | `operator_name` | json | `string` | 否 | — | 冗余展示名，权限判定不采信 |
| `RequestId` | `request_id` | json | `string` | 否 | — | 写接口必填（幂等键 + audit event_id 组成） |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `Ip` | `ip` | json | `string` | 否 | — | — |

### `OpsConfigsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]OpsConfigItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `OpsConfigVersionsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]OpsConfigVersion` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `LatestVersion` | `latest_version` | json | `int64` | 是 | — | — |

### `OpsRolloutRulesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]OpsRolloutRule` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `OpsTopicDetailData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `OpsTopic` | 是 | — | — |
| `Items` | `items` | json | `[]OpsTopicItem` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |
| `CacheTTL` | `ttl` | json | `int32` | 是 | — | ops-config 的建议缓存秒数（信封 ttl 保持 0，避免与外层混用） |

### `OpsTopicsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]OpsTopic` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `OpsSlotsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]OpsRecommendSlot` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `OpsSwitchesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]OpsClientSwitch` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `OpsRolloutRuleSpec`

> 灰度规则声明（对齐 opsconfig.v1.RolloutRuleSpec：不含服务端生成的 id/config_id/version）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Name` | `name` | json | `string` | 是 | — | — |
| `Mode` | `mode` | json | `int32` | 是 | — | — |
| `Percentage` | `percentage` | json | `int32` | 否 | — | — |
| `AppVersionMin` | `app_version_min` | json | `string` | 否 | — | — |
| `AppVersionMax` | `app_version_max` | json | `string` | 否 | — | — |
| `Platforms` | `platforms` | json | `[]int32` | 否 | — | — |
| `MidSuffixes` | `mid_suffixes` | json | `string` | 否 | — | — |
| `WhitelistMids` | `whitelist_mids` | json | `[]int64` | 否 | — | — |
| `Priority` | `priority` | json | `int32` | 否 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |

### `OpsPublishData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Item` | `item` | json | `OpsConfigItem` | 是 | — | — |
| `Version` | `version` | json | `OpsConfigVersion` | 是 | — | — |
| `Rules` | `rules` | json | `[]OpsRolloutRule` | 是 | — | — |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |

### `OpsRollbackData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Item` | `item` | json | `OpsConfigItem` | 是 | — | — |
| `Version` | `version` | json | `OpsConfigVersion` | 是 | — | — |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |

### `OpsRolloutRuleData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Rule` | `rule` | json | `OpsRolloutRule` | 是 | — | — |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |

### `OpsTopicData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Topic` | `topic` | json | `OpsTopic` | 是 | — | — |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |

### `OpsTopicItemSpec`

> 专题条目声明（写入侧：id/topic_id/operator_id/时间戳由 ops-config 生成，不接受客户端声明）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ItemType` | `item_type` | json | `string` | 是 | — | — |
| `ItemIid` | `item_id` | json | `string` | 是 | — | — |
| `Position` | `position` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |

### `OpsTopicItemsSaveData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TopicId` | `topic_id` | json | `int64` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |

### `OpsSlotData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Slot` | `slot` | json | `OpsRecommendSlot` | 是 | — | — |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |

### `OpsSlotItemSpec`

> 坑位条目声明（写入侧：id/slot_id/operator_id/时间戳由服务端生成）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Position` | `position` | json | `int32` | 是 | — | — |
| `ItemType` | `item_type` | json | `string` | 是 | — | — |
| `ItemIid` | `item_id` | json | `string` | 是 | — | — |
| `Weight` | `weight` | json | `int32` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |

### `OpsSlotItemsSaveData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SlotId` | `slot_id` | json | `int64` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |

### `OpsSwitchData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Switch` | `switch` | json | `OpsClientSwitch` | 是 | — | — |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |

### `OpsRefreshCacheData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Affected` | `affected` | json | `int32` | 是 | — | — |
| `Epoch` | `epoch` | json | `int64` | 是 | — | target=all 时为 0 |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |

### `OpsConfigItem`

> 配置项投影（对齐 opsconfig.v1.ConfigItem；latest_version=0 表示未发布， / epoch 是缓存代次，RefreshCache 后单调递增）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ConfigId` | `config_id` | json | `int64` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 是 | — | — |
| `ValueType` | `value_type` | json | `int32` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `LatestVersion` | `latest_version` | json | `int64` | 是 | — | — |
| `Epoch` | `epoch` | json | `int64` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OpsConfigVersion`

> 不可变版本快照（对齐 opsconfig.v1.ConfigVersion；change_type: create/publish/rollback， / rollback_from 指向被回滚的历史版本，0 表示非回滚）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `VersionId` | `version_id` | json | `int64` | 是 | — | — |
| `ConfigId` | `config_id` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Value` | `value` | json | `string` | 是 | — | — |
| `ValueType` | `value_type` | json | `int32` | 是 | — | — |
| `ChangeType` | `change_type` | json | `string` | 是 | — | — |
| `RollbackFrom` | `rollback_from` | json | `int64` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `OperatorName` | `operator_name` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `AuditEntryId` | `audit_entry_id` | json | `int64` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `PublishedAt` | `published_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `OpsRolloutRule`

> 灰度规则（对齐 opsconfig.v1.RolloutRule；mode 见 ROLLOUT_MODE_* 数值， / 多条规则按 priority 升序取首个命中，全不命中回落正式版本）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RuleId` | `rule_id` | json | `int64` | 是 | — | — |
| `ConfigId` | `config_id` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Mode` | `mode` | json | `int32` | 是 | — | — |
| `Percentage` | `percentage` | json | `int32` | 是 | — | — |
| `AppVersionMin` | `app_version_min` | json | `string` | 是 | — | — |
| `AppVersionMax` | `app_version_max` | json | `string` | 是 | — | — |
| `Platforms` | `platforms` | json | `[]int32` | 是 | — | — |
| `MidSuffixes` | `mid_suffixes` | json | `string` | 是 | — | — |
| `WhitelistMids` | `whitelist_mids` | json | `[]int64` | 是 | — | — |
| `Priority` | `priority` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OpsTopic`

> 专题（对齐 opsconfig.v1.Topic；zone_ids/tag_ids 只存 catalog 引用，version 是乐观锁）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TopicId` | `topic_id` | json | `int64` | 是 | — | — |
| `Slug` | `slug` | json | `string` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `Cover` | `cover` | json | `string` | 是 | — | — |
| `ZoneIds` | `zone_ids` | json | `[]int64` | 是 | — | — |
| `TagIds` | `tag_ids` | json | `[]int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Sort` | `sort` | json | `int32` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `EndAt` | `end_at` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OpsTopicItem`

> 专题条目（对齐 opsconfig.v1.TopicItem：只引用内容主键，不复制标题/时长）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `TopicId` | `topic_id` | json | `int64` | 是 | — | — |
| `ItemType` | `item_type` | json | `string` | 是 | — | — |
| `ItemIid` | `item_id` | json | `string` | 是 | — | — |
| `Position` | `position` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OpsRecommendSlot`

> 推荐位（对齐 opsconfig.v1.RecommendSlot；code 唯一，capacity 是坑位数量上限）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SlotId` | `slot_id` | json | `int64` | 是 | — | — |
| `Code` | `code` | json | `string` | 是 | — | — |
| `Page` | `page` | json | `string` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Platforms` | `platforms` | json | `[]int32` | 是 | — | — |
| `Capacity` | `capacity` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OpsClientSwitch`

> 客户端开关（对齐 opsconfig.v1.ClientSwitch；只表达「某端从哪个版本起具备某能力」， / 不表达任何 UI 细节；enabled: 1 开、2 关）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SwitchId` | `switch_id` | json | `int64` | 是 | — | — |
| `SwitchKey` | `switch_key` | json | `string` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 是 | — | — |
| `MinVersion` | `min_version` | json | `string` | 是 | — | — |
| `MaxVersion` | `max_version` | json | `string` | 是 | — | — |
| `Enabled` | `enabled` | json | `int32` | 是 | — | — |
| `ConfigId` | `config_id` | json | `int64` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/19-admin-ops.md -->
