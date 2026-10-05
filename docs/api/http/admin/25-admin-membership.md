# 运营面 · `/admin/membership`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化运营面：membership 域 | 免鉴权 | 5 |
| 商业化运营面：membership 域 | AdminPermission | 5 |

合计 **10** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化运营面：membership 域（免鉴权，5 条）

> -------------------- membership 只读面（不进 routePermissions） --------------------
> 与 audit / ops-config / cron / collector / private-message 的读面同一口径：会员页每次刷新
> 都会打一次 RPC，全量挂判定会把 operation 变成读放大瓶颈；这些读取本身不产生写入。
> 授予台账（/grant/list）也在读组——它是「谁在什么时候为什么动了时长」的证据，只读。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/membership/plan/list` | 套餐分页（含草稿与已下架；后台口径，不等于终端在售列表） | `membershipPlanList` | `membershipplanlistlogic.go` |
| POST | `/admin/membership/member/get` | 单用户会员身份 + 当前档位可得权益码（found=false 表示从未开通） | `membershipMemberGet` | `membershipmembergetlogic.go` |
| POST | `/admin/membership/grant/list` | 授予/变更台账分页（mid=0 为跨用户查；追溯每一行时长是谁动的） | `membershipGrantList` | `membershipgrantlistlogic.go` |
| POST | `/admin/membership/expiring/list` | 到期区间扫描（只读：核对 cron 将终结谁，后台不代为置过期） | `membershipExpiringList` | `membershipexpiringlistlogic.go` |
| POST | `/admin/membership/entitlement/list` | 权益码目录（enabled_only 可只看启用项） | `membershipEntitlementList` | `membershipentitlementlistlogic.go` |

### POST `/admin/membership/plan/list` — 套餐分页（含草稿与已下架；后台口径，不等于终端在售列表）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/membershipplanlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipplanlistlogic.go`

请求：`ParamMembershipPlanList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | 0 = UNSPECIFIED 不过滤 |
| `VipType` | `vip_type` | json | `int32` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | 匹配 plan_code/name 前缀 |
| `Page` | `page` | json | `int64` | 否 | — | 从 1 开始 |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`MembershipPlanListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipPlanListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/membership/member/get` — 单用户会员身份 + 当前档位可得权益码（found=false 表示从未开通）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/membershipmembergethandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipmembergetlogic.go`

请求：`ParamMembershipMemberGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 否 | — | — |

响应：`MembershipMemberResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipMemberData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/membership/grant/list` — 授予/变更台账分页（mid=0 为跨用户查；追溯每一行时长是谁动的）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/membershipgrantlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipgrantlistlogic.go`

请求：`ParamMembershipGrantList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `VipType` | `vip_type` | json | `int32` | 否 | — | — |
| `Source` | `source` | json | `int32` | 否 | — | — |
| `BizOrderNo` | `biz_order_no` | json | `string` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`MembershipGrantListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipGrantListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/membership/expiring/list` — 到期区间扫描（只读：核对 cron 将终结谁，后台不代为置过期）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/membershipexpiringlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipexpiringlistlogic.go`

请求：`ParamMembershipExpiringList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FromExpireAt` | `from_expire_at` | json | `int64` | 是 | — | — |
| `ToExpireAt` | `to_expire_at` | json | `int64` | 是 | — | — |
| `AutoRenewOnly` | `auto_renew_only` | json | `bool` | 否 | — | — |
| `Limit` | `limit` | json | `int64` | 否 | — | 超限由服务裁剪，不报错 |

响应：`MembershipExpiringListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipExpiringListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/membership/entitlement/list` — 权益码目录（enabled_only 可只看启用项）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/membershipentitlementlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipentitlementlistlogic.go`

请求：`ParamMembershipEntitlementList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EnabledOnly` | `enabled_only` | json | `bool` | 否 | — | — |

响应：`MembershipEntitlementListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipEntitlementListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 商业化运营面：membership 域（AdminPermission，5 条）

> -------------------- membership 写面（受 AdminPermission 保护） --------------------
> 套餐上下架与套餐编辑分开授权：能改草稿价格的人，不该顺带获得「让它对外可售」的能力。
> 授予与收回也分开：收回会立刻掐断用户已付费的能力，影响面与「多发几天」不对称。
> 权益码开关是全站的能力闸（关掉即所有人判否），单列一个权限点。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/membership/plan/upsert` | 新建/修改套餐草稿（无 state 位，改完不会自动生效） | `membership:plan` / `update` | `membershipPlanUpsert` | `membershipplanupsertlogic.go` |
| POST | `/admin/membership/plan/state` | 套餐上下架（只允许合法迁移；改价必须走新草稿，reason 必填） | `membership:plan` / `publish` | `membershipPlanState` | `membershipplanstatelogic.go` |
| POST | `/admin/membership/grant` | 运营手工开通/延长会员（沙箱台账之外的独立来源；不扣钱，reason 必填） | `membership:grant` / `create` | `membershipGrant` | `membershipgrantlogic.go` |
| POST | `/admin/membership/grant/revoke` | 收回会员（立即失效或按天扣回；重复提交回首次结论） | `membership:grant` / `revoke` | `membershipGrantRevoke` | `membershipgrantrevokelogic.go` |
| POST | `/admin/membership/entitlement/upsert` | 权益码新增/开关（关掉即全站该能力判否，故与套餐权限点分离） | `membership:entitlement` / `update` | `membershipEntitlementUpsert` | `membershipentitlementupsertlogic.go` |

### POST `/admin/membership/plan/upsert` — 新建/修改套餐草稿（无 state 位，改完不会自动生效）

- 权限口径：AdminPermission · 权限点 `membership:plan` / `update`
- goctl 入口：`gateway/admin/internal/handler/membershipplanupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipplanupsertlogic.go`

请求：`ParamMembershipPlanUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PlanId` | `plan_id` | json | `int64` | 否 | — | — |
| `PlanCode` | `plan_code` | json | `string` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |
| `DurationDays` | `duration_days` | json | `int32` | 是 | — | — |
| `UnitCount` | `unit_count` | json | `int32` | 是 | — | — |
| `PriceMinor` | `price_minor` | json | `int64` | 是 | — | 分，负数由服务拒 |
| `PromPriceMinor` | `prom_price_minor` | json | `int64` | 否 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | 显式币种，不因「默认 CNY」而省略 |
| `Platforms` | `platforms` | json | `[]int32` | 否 | — | — |
| `AutoRenewSupported` | `auto_renew_supported` | json | `bool` | 否 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | 新建传 0 |
| `Reason` | `reason` | json | `string` | 否 | — | → UpsertPlanReq.reason，落 mb_plan_change_log.reason；留空由服务回落成规格摘要 |
| `Operator` | `operator` | json | `int64` | 是 | — | 触发者后台 mid，必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，重复提交回首次结论 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`MembershipPlanUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipPlanUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/membership/plan/state` — 套餐上下架（只允许合法迁移；改价必须走新草稿，reason 必填）

- 权限口径：AdminPermission · 权限点 `membership:plan` / `publish`
- goctl 入口：`gateway/admin/internal/handler/membershipplanstatehandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipplanstatelogic.go`

请求：`ParamMembershipPlanState`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PlanId` | `plan_id` | json | `int64` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`MembershipPlanStateResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipPlanStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/membership/grant` — 运营手工开通/延长会员（沙箱台账之外的独立来源；不扣钱，reason 必填）

- 权限口径：AdminPermission · 权限点 `membership:grant` / `create`
- goctl 入口：`gateway/admin/internal/handler/membershipgranthandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipgrantlogic.go`

请求：`ParamMembershipGrant`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |
| `PlanId` | `plan_id` | json | `int64` | 否 | — | 0 = 无套餐（手工发放） |
| `DeltaDays` | `delta_days` | json | `int32` | 是 | — | — |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `BizOrderNo` | `biz_order_no` | json | `string` | 否 | — | — |
| `PaymentNo` | `payment_no` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，重放不二次加时长 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`MembershipGrantResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipGrantData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/membership/grant/revoke` — 收回会员（立即失效或按天扣回；重复提交回首次结论）

- 权限口径：AdminPermission · 权限点 `membership:grant` / `revoke`
- goctl 入口：`gateway/admin/internal/handler/membershipgrantrevokehandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipgrantrevokelogic.go`

请求：`ParamMembershipRevoke`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | 必填且必须 > 0：服务侧 requireVipType 对 0 直接回 ErrInvalidVipType，网关不替你挑档 |
| `ClearRemaining` | `clear_remaining` | json | `bool` | 否 | — | — |
| `DeltaDays` | `delta_days` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `PlanId` | `plan_id` | json | `int64` | 否 | — | 0 表示无套餐 |
| `BizOrderNo` | `biz_order_no` | json | `string` | 否 | — | 订单号引用（跨服务只存主键，不建外键） |
| `PaymentNo` | `payment_no` | json | `string` | 否 | — | 资金流水号引用 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`MembershipRevokeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipRevokeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/membership/entitlement/upsert` — 权益码新增/开关（关掉即全站该能力判否，故与套餐权限点分离）

- 权限口径：AdminPermission · 权限点 `membership:entitlement` / `update`
- goctl 入口：`gateway/admin/internal/handler/membershipentitlementupserthandler.go`
- 业务实现：`gateway/admin/internal/logic/membershipentitlementupsertlogic.go`

请求：`ParamMembershipEntitlementUpsert`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `string` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `MinVipType` | `min_vip_type` | json | `int32` | 是 | — | — |
| `Enabled` | `enabled` | json | `bool` | 否 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`MembershipEntitlementUpsertResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipEntitlementUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamMembershipPlanList`

> ParamMembershipPlanList 1:1 对应 ListPlansAdminReq（含草稿与已下架；终端面用的 / ListPlans 只回在售且平台可见，不开放给后台，避免两个口径）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `State` | `state` | json | `int32` | 否 | — | 0 = UNSPECIFIED 不过滤 |
| `VipType` | `vip_type` | json | `int32` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | 匹配 plan_code/name 前缀 |
| `Page` | `page` | json | `int64` | 否 | — | 从 1 开始 |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `MembershipPlanListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipPlanListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMembershipMemberGet`

> ParamMembershipMemberGet 1:1 对应 GetMembershipReq；vip_type=0 回当前生效的最高档。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 否 | — | — |

### `MembershipMemberResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipMemberData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMembershipGrantList`

> ParamMembershipGrantList 1:1 对应 ListGrantsReq。mid=0 才有跨用户语义（后台面）， / 翻页上限与「哪些组合算无界扫描」由 membership 判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `VipType` | `vip_type` | json | `int32` | 否 | — | — |
| `Source` | `source` | json | `int32` | 否 | — | — |
| `BizOrderNo` | `biz_order_no` | json | `string` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `MembershipGrantListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipGrantListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMembershipExpiringList`

> ParamMembershipExpiringList 1:1 对应 ListExpiringMembershipsReq。本路由**只读**： / 后台靠它核对 cron 下一批要终结谁，真正置过期的是 cron 侧的 ExpireMembership。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FromExpireAt` | `from_expire_at` | json | `int64` | 是 | — | — |
| `ToExpireAt` | `to_expire_at` | json | `int64` | 是 | — | — |
| `AutoRenewOnly` | `auto_renew_only` | json | `bool` | 否 | — | — |
| `Limit` | `limit` | json | `int64` | 否 | — | 超限由服务裁剪，不报错 |

### `MembershipExpiringListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipExpiringListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMembershipEntitlementList`

> ParamMembershipEntitlementList 1:1 对应 ListEntitlementsReq。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EnabledOnly` | `enabled_only` | json | `bool` | 否 | — | — |

### `MembershipEntitlementListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipEntitlementListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMembershipPlanUpsert`

> ParamMembershipPlanUpsert 1:1 对应 UpsertPlanReq 的可填位（plan_code 已存在即更新）。 / 表单**没有 state 位**：上下架是独立动作（membership:plan/publish），改草稿不该顺带生效。 / 时长、价格区间、平台合法性、expected_version 的 CAS 语义全在 membership 侧判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PlanId` | `plan_id` | json | `int64` | 否 | — | — |
| `PlanCode` | `plan_code` | json | `string` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |
| `DurationDays` | `duration_days` | json | `int32` | 是 | — | — |
| `UnitCount` | `unit_count` | json | `int32` | 是 | — | — |
| `PriceMinor` | `price_minor` | json | `int64` | 是 | — | 分，负数由服务拒 |
| `PromPriceMinor` | `prom_price_minor` | json | `int64` | 否 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | 显式币种，不因「默认 CNY」而省略 |
| `Platforms` | `platforms` | json | `[]int32` | 否 | — | — |
| `AutoRenewSupported` | `auto_renew_supported` | json | `bool` | 否 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | 新建传 0 |
| `Reason` | `reason` | json | `string` | 否 | — | → UpsertPlanReq.reason，落 mb_plan_change_log.reason；留空由服务回落成规格摘要 |
| `Operator` | `operator` | json | `int64` | 是 | — | 触发者后台 mid，必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，重复提交回首次结论 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `MembershipPlanUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipPlanUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMembershipPlanState`

> ParamMembershipPlanState 1:1 对应 SetPlanStateReq。只允许 DRAFT→ON_SALE、 / ON_SALE→OFF_SALE、OFF_SALE→ON_SALE；**改价不走这里**（必须新建 DRAFT 再切换）， / 否则会出现「改完价格对已下单用户追溯生效」。reason 必填并进变更台账。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PlanId` | `plan_id` | json | `int64` | 是 | — | — |
| `TargetState` | `target_state` | json | `int32` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `MembershipPlanStateResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipPlanStateData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMembershipGrant`

> ParamMembershipGrant 1:1 对应 GrantMembershipReq（运营手工开通/延长）。 / source=ADMIN_OPS/EXPERIENCE 时 reason 必填；除这两种外的来源应当能回溯到一条 / payment 流水，所以后台不该用它们补数——来源取值的合法性由 membership 判定。 / 本路由不扣钱、不建支付单：它只写会员身份与台账（§1 资金语义）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |
| `PlanId` | `plan_id` | json | `int64` | 否 | — | 0 = 无套餐（手工发放） |
| `DeltaDays` | `delta_days` | json | `int32` | 是 | — | — |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `BizOrderNo` | `biz_order_no` | json | `string` | 否 | — | — |
| `PaymentNo` | `payment_no` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，重放不二次加时长 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `MembershipGrantResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipGrantData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMembershipRevoke`

> ParamMembershipRevoke 1:1 对应 RevokeMembershipReq。clear_remaining=true 立即失效， / false 只按 delta_days 扣回——两种语义差很多，网关不做默认值兜底（optional 即 false， / 由调用方显式选择）。reason 必填：收回是有后果的动作，无理由服务侧不受理。 / plan_id/biz_order_no/payment_no 是追溯位（服务口径：三位全空即「运营手工收回」， / 带单号的台账才可能被退款流程对账）；网关不填默认、不猜单号，原样下传。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | 必填且必须 > 0：服务侧 requireVipType 对 0 直接回 ErrInvalidVipType，网关不替你挑档 |
| `ClearRemaining` | `clear_remaining` | json | `bool` | 否 | — | — |
| `DeltaDays` | `delta_days` | json | `int32` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `PlanId` | `plan_id` | json | `int64` | 否 | — | 0 表示无套餐 |
| `BizOrderNo` | `biz_order_no` | json | `string` | 否 | — | 订单号引用（跨服务只存主键，不建外键） |
| `PaymentNo` | `payment_no` | json | `string` | 否 | — | 资金流水号引用 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `MembershipRevokeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipRevokeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMembershipEntitlementUpsert`

> ParamMembershipEntitlementUpsert 1:1 对应 UpsertEntitlementReq。 / enabled=false 会让所有依赖该权益码的能力立刻判否（ENTITLEMENT_CODE_DISABLED）， / 所以它与套餐上下架是两个独立权限点，不互相顺带。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `string` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |
| `MinVipType` | `min_vip_type` | json | `int32` | 是 | — | — |
| `Enabled` | `enabled` | json | `bool` | 否 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `MembershipEntitlementUpsertResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MembershipEntitlementUpsertData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `MembershipPlanListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]MembershipPlanItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `MembershipMemberData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | false = 从未开通过（不是错误） |
| `Membership` | `membership` | json | `MembershipMemberItem` | 是 | — | granted_entitlements 是只读投影，让详情页一次读全；它不替代服务间权益判定 RPC。 |
| `ServerNow` | `server_now` | json | `int64` | 是 | — | — |
| `GrantedEntitlements` | `granted_entitlements` | json | `[]MembershipEntitlementItem` | 是 | — | — |

### `MembershipGrantListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]MembershipGrantItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `MembershipExpiringListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]MembershipMemberItem` | 是 | — | — |
| `NextExpireAtCursor` | `next_expire_at_cursor` | json | `int64` | 是 | — | 0 = 本区间已扫完 |

### `MembershipEntitlementListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]MembershipEntitlementItem` | 是 | — | — |

### `MembershipPlanUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Plan` | `plan` | json | `MembershipPlanItem` | 是 | — | — |

### `MembershipPlanStateData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Plan` | `plan` | json | `MembershipPlanItem` | 是 | — | — |

### `MembershipGrantData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | true = 命中幂等键，回首次结论 |
| `GrantId` | `grant_id` | json | `int64` | 是 | — | — |
| `Membership` | `membership` | json | `MembershipMemberItem` | 是 | — | — |

### `MembershipRevokeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `GrantId` | `grant_id` | json | `int64` | 是 | — | — |
| `Membership` | `membership` | json | `MembershipMemberItem` | 是 | — | — |

### `MembershipEntitlementUpsertData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Entitlement` | `entitlement` | json | `MembershipEntitlementItem` | 是 | — | — |

### `MembershipPlanItem`

> 契约来源 services/membership/rpc/membership.proto（本轮契约冻结，只接网关不改服务）。 /  / 本域资金/权益语义边界（AGENTS.md §1 商业化范围 2026-09-22 修订）：membership **不碰钱**。 / 开通会员的钱由 trade-order 建单、payment 走**沙箱台账**受理，履约时才调 GrantMembership； / 因此后台这条 /grant 路径只写会员身份与授予台账，既不产生资金流水，也不请求任何真实支付渠道。 / 权益判定的 granted=true 是「授予表里确实有未过期的一行」的真实读结论，无授予记录即未开通； / 后台不提供「把某人硬判成会员」的开关，也不伪造成功（§1 资金语义）。 / 金额一律 int64 最小货币单位（分）+ 显式 currency，全程不用浮点（§6）。 /  / 主体与幂等口径（与 collector / live / private-message 面一致）： /   - operator 是后台 admin_id（op_admin_user 主键，**不是用户 mid**），必须 > 0； /     proto 的 operator 是字符串，由 logic 渲染成 gateway/admin:<operator> 下传， /     请求体不得自称是别的身份； /   - idempotency_key 映射到 proto 的 request_id（唯一索引）：重复提交命中首次结论、 /     回 duplicated=true，不会二次加时长；同键改参数由服务判冲突而不是静默改口径； /   - trace_id 只用于网关日志关联（membership.proto 的 req 没有该字段，不下传）。 /  / 刻意**不开**的路由（理由写在这里，不是漏实现）： /   - ExpireMembership：到期终结归 services/cron 与服务侧（只有 expire_at 真早于 now 才生效）， /     后台点它等于手工改会员状态，会让台账里的 operator 说谎； /   - SetAutoRenew：自动续费签约/解约是**用户自身动作**，归 gateway/app；后台代签既不合法 /     也无从体现用户意愿； /   - CheckEntitlement / CheckEntitlements：权益判定是服务间读（播放、下载等能力位）， /     每次播放都要判一次，做成后台 HTTP 接口只会多一条绕过后端判定的公开口径。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PlanId` | `plan_id` | json | `int64` | 是 | — | — |
| `PlanCode` | `plan_code` | json | `string` | 是 | — | 对外稳定编码，下单用它而不是 plan_id |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | VipType：1 大会员 2 超级大会员（超集） |
| `DurationDays` | `duration_days` | json | `int32` | 是 | — | 单个售卖单位时长 |
| `UnitCount` | `unit_count` | json | `int32` | 是 | — | 一次购买含几个 duration_days |
| `PriceMinor` | `price_minor` | json | `int64` | 是 | — | 原价（分） |
| `PromPriceMinor` | `prom_price_minor` | json | `int64` | 是 | — | 促销价（分）；0 表示无促销 |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Platforms` | `platforms` | json | `[]int32` | 是 | — | PlanPlatform 列表，可见端 |
| `AutoRenewSupported` | `auto_renew_supported` | json | `bool` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | PlanSaleState：1 DRAFT 2 ON_SALE 3 OFF_SALE |
| `Version` | `version` | json | `int64` | 是 | — | CAS 位 |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `CreatedBy` | `created_by` | json | `string` | 是 | — | — |
| `UpdatedBy` | `updated_by` | json | `string` | 是 | — | — |

### `MembershipMemberItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | <= server_now 即已过期 |
| `AutoRenew` | `auto_renew` | json | `bool` | 是 | — | 沙箱：只记录意愿，不建真实代扣协议 |
| `AutoRenewChannel` | `auto_renew_channel` | json | `string` | 是 | — | — |
| `AutoRenewSignedAt` | `auto_renew_signed_at` | json | `int64` | 是 | — | — |
| `Source` | `source` | json | `int32` | 是 | — | GrantSource：最近一次变更来源 |
| `PaidMonthCount` | `paid_month_count` | json | `int32` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `MembershipEntitlementItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `string` | 是 | — | 例如 vip.high_bitrate |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `MinVipType` | `min_vip_type` | json | `int32` | 是 | — | 达到该档（含更高档）才通过 |
| `Enabled` | `enabled` | json | `bool` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `MembershipGrantItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `GrantId` | `grant_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |
| `Action` | `action` | json | `string` | 是 | — | GRANT / EXTEND / REVOKE / EXPIRE |
| `DeltaDays` | `delta_days` | json | `int32` | 是 | — | 收回为负 |
| `PlanId` | `plan_id` | json | `int64` | 是 | — | 0 表示无套餐（运营手工/迁移） |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `BizOrderNo` | `biz_order_no` | json | `string` | 是 | — | 跨服务只存主键引用，不建外键 |
| `PaymentNo` | `payment_no` | json | `string` | 是 | — | — |
| `BeforeExpireAt` | `before_expire_at` | json | `int64` | 是 | — | — |
| `AfterExpireAt` | `after_expire_at` | json | `int64` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | "user" / 运营工号 / "cron" |
| `RequestId` | `request_id` | json | `string` | 是 | — | 幂等键 |
| `Reason` | `reason` | json | `string` | 是 | — | 台账摘要：不含 PII 与凭据 |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/25-admin-membership.md -->
