# 运营面 · `/admin/notification`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| notification 域运营路由（services/notification/rpc/notification.proto） | 免鉴权 | 4 |
| notification 域写入口（受 AdminPermission 保护） | AdminPermission | 4 |

合计 **8** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## notification 域运营路由（services/notification/rpc/notification.proto）（免鉴权，4 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/notification/template/list` | 分页查询模板（code/channel/language/state 过滤） | `listNotifyTemplates` | `listnotifytemplateslogic.go` |
| GET | `/admin/notification/delivery/status` | 查询单条投递记录与供应商回执 | `getNotifyDeliveryStatus` | `getnotifydeliverystatuslogic.go` |
| GET | `/admin/notification/delivery/list` | 分页查询投递记录（mid/channel/state/biz_key/时间窗过滤） | `listNotifyDeliveries` | `listnotifydeliverieslogic.go` |
| GET | `/admin/notification/deadletter/list` | 分页查询死信（事件 ID/状态/topic 过滤） | `listNotifyDeadLetters` | `listnotifydeadletterslogic.go` |

### GET `/admin/notification/template/list` — 分页查询模板（code/channel/language/state 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listnotifytemplateshandler.go`
- 业务实现：`gateway/admin/internal/logic/listnotifytemplateslogic.go`

请求：`ParamListNotifyTemplates`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TemplateCode` | `template_code` | form | `string` | 否 | — | — |
| `Channel` | `channel` | form | `int32` | 否 | — | — |
| `Language` | `language` | form | `int32` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`NotifyTemplatesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyTemplatesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/notification/delivery/status` — 查询单条投递记录与供应商回执

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getnotifydeliverystatushandler.go`
- 业务实现：`gateway/admin/internal/logic/getnotifydeliverystatuslogic.go`

请求：`ParamNotifyDeliveryStatus`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeliveryId` | `delivery_id` | form | `string` | 是 | — | — |

响应：`NotifyDeliveryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyDeliveryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/notification/delivery/list` — 分页查询投递记录（mid/channel/state/biz_key/时间窗过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listnotifydeliverieshandler.go`
- 业务实现：`gateway/admin/internal/logic/listnotifydeliverieslogic.go`

请求：`ParamListNotifyDeliveries`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `Channel` | `channel` | form | `int32` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `BizKey` | `biz_key` | form | `string` | 否 | — | — |
| `StartCtime` | `start_ctime` | form | `int64` | 否 | — | — |
| `EndCtime` | `end_ctime` | form | `int64` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`NotifyDeliveriesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyDeliveriesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/notification/deadletter/list` — 分页查询死信（事件 ID/状态/topic 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listnotifydeadlettershandler.go`
- 业务实现：`gateway/admin/internal/logic/listnotifydeadletterslogic.go`

请求：`ParamListNotifyDeadLetters`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventId` | `event_id` | form | `string` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Topic` | `topic` | form | `string` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`NotifyDeadLettersResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyDeadLettersData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## notification 域写入口（受 AdminPermission 保护）（AdminPermission，4 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/notification/template/upsert` | 新增/更新通知模板（publish=false 存草稿，true 直接发布新版本） | `notify:template` / `update` | `upsertNotifyTemplate` | `upsertnotifytemplatelogic.go` |
| POST | `/admin/notification/template/publish` | 发布指定草稿版本（operator 必填，写审计） | `notify:template` / `publish` | `publishNotifyTemplate` | `publishnotifytemplatelogic.go` |
| POST | `/admin/notification/template/render` | 模板渲染预览（不落库，缺变量时返回 missing_vars） | `notify:template` / `render` | `renderNotifyTemplate` | `rendernotifytemplatelogic.go` |
| POST | `/admin/notification/deadletter/retry` | 重投死信（按 operator 记审计，返回新投递任务 ID） | `notify:deadletter` / `retry` | `retryNotifyDeadLetter` | `retrynotifydeadletterlogic.go` |

### POST `/admin/notification/template/upsert` — 新增/更新通知模板（publish=false 存草稿，true 直接发布新版本）

- 权限口径：AdminPermission · 权限点 `notify:template` / `update`
- goctl 入口：`gateway/admin/internal/handler/upsertnotifytemplatehandler.go`
- 业务实现：`gateway/admin/internal/logic/upsertnotifytemplatelogic.go`

请求：`ParamUpsertNotifyTemplate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TemplateCode` | `template_code` | json | `string` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `Language` | `language` | json | `int32` | 是 | — | — |
| `TitleTpl` | `title_tpl` | json | `string` | 是 | — | — |
| `BodyTpl` | `body_tpl` | json | `string` | 是 | — | — |
| `Publish` | `publish` | json | `bool` | 否 | — | false 存草稿 |

响应：`NotifyTemplateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyTemplateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/notification/template/publish` — 发布指定草稿版本（operator 必填，写审计）

- 权限口径：AdminPermission · 权限点 `notify:template` / `publish`
- goctl 入口：`gateway/admin/internal/handler/publishnotifytemplatehandler.go`
- 业务实现：`gateway/admin/internal/logic/publishnotifytemplatelogic.go`

请求：`ParamPublishNotifyTemplate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TemplateCode` | `template_code` | json | `string` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `Language` | `language` | json | `int32` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |

响应：`NotifyTemplateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyTemplateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/notification/template/render` — 模板渲染预览（不落库，缺变量时返回 missing_vars）

- 权限口径：AdminPermission · 权限点 `notify:template` / `render`
- goctl 入口：`gateway/admin/internal/handler/rendernotifytemplatehandler.go`
- 业务实现：`gateway/admin/internal/logic/rendernotifytemplatelogic.go`

请求：`ParamRenderNotifyTemplate`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `TemplateCode` | `template_code` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 否 | — | 0 表示当前已发布版本 |
| `Language` | `language` | json | `int32` | 否 | — | — |
| `Params` | `params` | json | `map[string]string` | 否 | — | 禁止放明文联系方式 |

响应：`NotifyRenderResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyRenderData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/notification/deadletter/retry` — 重投死信（按 operator 记审计，返回新投递任务 ID）

- 权限口径：AdminPermission · 权限点 `notify:deadletter` / `retry`
- goctl 入口：`gateway/admin/internal/handler/retrynotifydeadletterhandler.go`
- 业务实现：`gateway/admin/internal/logic/retrynotifydeadletterlogic.go`

请求：`ParamRetryNotifyDeadLetter`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Id` | `id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |

响应：`NotifyRetryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyRetryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamListNotifyTemplates`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TemplateCode` | `template_code` | form | `string` | 否 | — | — |
| `Channel` | `channel` | form | `int32` | 否 | — | — |
| `Language` | `language` | form | `int32` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `NotifyTemplatesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyTemplatesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamNotifyDeliveryStatus`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeliveryId` | `delivery_id` | form | `string` | 是 | — | — |

### `NotifyDeliveryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyDeliveryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListNotifyDeliveries`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `Channel` | `channel` | form | `int32` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `BizKey` | `biz_key` | form | `string` | 否 | — | — |
| `StartCtime` | `start_ctime` | form | `int64` | 否 | — | — |
| `EndCtime` | `end_ctime` | form | `int64` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `NotifyDeliveriesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyDeliveriesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListNotifyDeadLetters`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventId` | `event_id` | form | `string` | 否 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `Topic` | `topic` | form | `string` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `NotifyDeadLettersResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyDeadLettersData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUpsertNotifyTemplate`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TemplateCode` | `template_code` | json | `string` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `Language` | `language` | json | `int32` | 是 | — | — |
| `TitleTpl` | `title_tpl` | json | `string` | 是 | — | — |
| `BodyTpl` | `body_tpl` | json | `string` | 是 | — | — |
| `Publish` | `publish` | json | `bool` | 否 | — | false 存草稿 |

### `NotifyTemplateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyTemplateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamPublishNotifyTemplate`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TemplateCode` | `template_code` | json | `string` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `Language` | `language` | json | `int32` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |

### `ParamRenderNotifyTemplate`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `TemplateCode` | `template_code` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 否 | — | 0 表示当前已发布版本 |
| `Language` | `language` | json | `int32` | 否 | — | — |
| `Params` | `params` | json | `map[string]string` | 否 | — | 禁止放明文联系方式 |

### `NotifyRenderResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyRenderData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRetryNotifyDeadLetter`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Id` | `id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |

### `NotifyRetryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `NotifyRetryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `NotifyTemplatesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]NotifyTemplateInfo` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `NotifyDeliveryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Delivery` | `delivery` | json | `NotifyDeliveryInfo` | 是 | — | — |
| `Found` | `found` | json | `bool` | 是 | — | — |

### `NotifyDeliveriesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]NotifyDeliveryInfo` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `NotifyDeadLettersData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]NotifyDeadLetterInfo` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |

### `AdminOpContext`

> 管理后台领域服务（RBAC / 菜单 / 运营配置 / 管理任务 / 审计索引）。 / 字段口径逐项对齐 services/operation/rpc/operation.proto（operation.v1.*）， / 不新增下游没有的字段；下游缺失的能力在 logic 里以「契约缺口」注释标注。 / AdminOpContext 是 operation 除 AdminLogin/VerifyAdminPermission 外所有方法 / 必带的操作者上下文（proto OpContext：审计主体与链路信息）。 / 网关把它作为统一嵌套字段 op 放进每个 POST 体，字段名与 proto 一一对应： /   - operator_id：审计主体。中间件用后台会话解析出的 admin_id 覆盖客户端声明值； /   - request_id：幂等键，写接口必填（proto 注释「写接口必填」）； /   - ip / user_agent：operation 服务侧只做哈希与脱敏后入审计索引。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `OperatorName` | `operator_name` | json | `string` | 否 | — | — |
| `Ip` | `ip` | json | `string` | 否 | — | — |
| `UserAgent` | `user_agent` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

### `NotifyTemplateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Template` | `template` | json | `NotifyTemplateInfo` | 是 | — | — |

### `NotifyRenderData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Title` | `title` | json | `string` | 是 | — | — |
| `Body` | `body` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |
| `Language` | `language` | json | `int32` | 是 | — | — |
| `MissingVars` | `missing_vars` | json | `[]string` | 是 | — | — |
| `Rejected` | `rejected` | json | `bool` | 是 | — | missing_vars 非空即视为渲染失败 |

### `NotifyRetryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeliveryIds` | `delivery_ids` | json | `[]string` | 是 | — | — |
| `Retried` | `retried` | json | `int32` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |

### `NotifyTemplateInfo`

> 模板/投递/死信是运营面能力；终端只保留本人免打扰偏好（gateway/app）。 / 依据 AGENTS.md §1 不提供任何营销群发入口：SendNotification 属领域服务与 / 事件消费链路调用，不在后台开放。明文手机号/邮箱不进入本契约（docs §4）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `TemplateCode` | `template_code` | json | `string` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `Language` | `language` | json | `int32` | 是 | — | — |
| `TitleTpl` | `title_tpl` | json | `string` | 是 | — | — |
| `BodyTpl` | `body_tpl` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `NotifyDeliveryInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DeliveryId` | `delivery_id` | json | `string` | 是 | — | — |
| `BizKey` | `biz_key` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `TemplateCode` | `template_code` | json | `string` | 是 | — | — |
| `TemplateVer` | `template_version` | json | `int32` | 是 | — | — |
| `TargetRef` | `target_ref` | json | `string` | 是 | — | — |
| `PayloadDigest` | `payload_digest` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Provider` | `provider` | json | `string` | 是 | — | — |
| `ProviderMsgId` | `provider_msg_id` | json | `string` | 是 | — | — |
| `RetryCount` | `retry_count` | json | `int32` | 是 | — | — |
| `NextRetryAt` | `next_retry_at` | json | `int64` | 是 | — | — |
| `LastError` | `last_error` | json | `string` | 是 | — | — |
| `SentAt` | `sent_at` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `Priority` | `priority` | json | `int32` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `NotifyDeadLetterInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |
| `EventType` | `event_type` | json | `string` | 是 | — | — |
| `Topic` | `topic` | json | `string` | 是 | — | — |
| `PayloadDigest` | `payload_digest` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/15-admin-notification.md -->
