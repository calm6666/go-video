# 终端面 · `/membership`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto） | 免鉴权 | 6 |

合计 **6** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto）（免鉴权，6 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/membership/plans` | 会员套餐列表（终端可见档位；含下架档需 all=true，供续费页） | `mbPlans` | `mbplanslogic.go` |
| GET | `/membership/plan` | 单个套餐读取（下单前置展示） | `mbPlan` | `mbplanlogic.go` |
| GET | `/membership/my` | 我的会员状态（含服务端时钟与可得权益码） | `mbMy` | `mbmylogic.go` |
| GET | `/membership/entitlements` | 批量权益判定（播放详情页一次问多项） | `mbEntitlements` | `mbentitlementslogic.go` |
| POST | `/membership/autorenew/set` | 自动续费签约/解约（沙箱：只记录意愿，不建立真实代扣协议） | `mbAutoRenew` | `mbautorenewlogic.go` |
| GET | `/membership/grants` | 我的会员开通记录（用户侧台账） | `mbGrants` | `mbgrantslogic.go` |

### GET `/membership/plans` — 会员套餐列表（终端可见档位；含下架档需 all=true，供续费页）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/mbplanshandler.go`
- 业务实现：`gateway/app/internal/logic/mbplanslogic.go`

请求：`ParamMbPlans`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Platform` | `platform` | form | `int32` | 否 | — | 1 android、2 ios、3 harmony、4 desktop、5 web；0 不按平台过滤 |
| `VipType` | `vip_type` | form | `int32` | 否 | — | 0 全部档位 |
| `All` | `all` | form | `bool` | 否 | — | true 时包含已下架套餐（续费页要能显示旧档） |

响应：`MbPlansResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbPlansData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/membership/plan` — 单个套餐读取（下单前置展示）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/mbplanhandler.go`
- 业务实现：`gateway/app/internal/logic/mbplanlogic.go`

请求：`ParamMbPlan`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PlanId` | `plan_id` | form | `int64` | 否 | — | — |
| `PlanCode` | `plan_code` | form | `string` | 否 | — | — |

响应：`MbPlanResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbPlan` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/membership/my` — 我的会员状态（含服务端时钟与可得权益码）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/mbmyhandler.go`
- 业务实现：`gateway/app/internal/logic/mbmylogic.go`

请求：`ParamMbMy`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `VipType` | `vip_type` | form | `int32` | 否 | — | 0 表示返回当前生效的最高档 |

响应：`MbMyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbMyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/membership/entitlements` — 批量权益判定（播放详情页一次问多项）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/mbentitlementshandler.go`
- 业务实现：`gateway/app/internal/logic/mbentitlementslogic.go`

请求：`ParamMbEntitlements`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Codes` | `codes` | form | `[]string` | 是 | split | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`MbEntitlementsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbEntitlementsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/membership/autorenew/set` — 自动续费签约/解约（沙箱：只记录意愿，不建立真实代扣协议）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/mbautorenewhandler.go`
- 业务实现：`gateway/app/internal/logic/mbautorenewlogic.go`

请求：`ParamMbAutoRenew`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `VipType` | `vip_type` | form | `int32` | 是 | — | — |
| `On` | `on` | form | `bool` | 是 | — | — |
| `Channel` | `channel` | form | `string` | 否 | — | on=true 必填；本项目只接受 SANDBOX |
| `RequestId` | `request_id` | form | `string` | 是 | — | 幂等键，原样透传给下游 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`MbAutoRenewResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbAutoRenewData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/membership/grants` — 我的会员开通记录（用户侧台账）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/mbgrantshandler.go`
- 业务实现：`gateway/app/internal/logic/mbgrantslogic.go`

请求：`ParamMbGrants`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `VipType` | `vip_type` | form | `int32` | 否 | — | — |
| `FromTs` | `from_ts` | form | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | form | `int64` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | 0 由服务取默认并截断到上限 |

响应：`MbGrantsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbGrantsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamMbPlans`

> 边界（AGENTS.md §1 商业化范围修订条、§5）：网关只做参数校验、主体透传与 DTO 投影裁剪， / 不在网关判「是不是会员」——权益判定唯一出口是 membership.CheckEntitlement(s)， / 网关复制一份档位比较逻辑就等于造第二个事实源。 / 资金不经网关：本文件**没有**「给我开通会员」这条路由。开通只能由订单履约 / （/order/create → trade-order → membership.GrantMembership）或运营授权（gateway/admin）触发； / 终端能写的只有自动续费签约位（不产生任何真实扣款协议）。 / 身份口径沿用本文件既有约定：mid 为必填 form 参数（gateway/app 未配置 jwt 中间件）， / 真实登录态校验属网关侧已知缺口，见 gateway/app/README.md「商业化路由的安全边界」。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Platform` | `platform` | form | `int32` | 否 | — | 1 android、2 ios、3 harmony、4 desktop、5 web；0 不按平台过滤 |
| `VipType` | `vip_type` | form | `int32` | 否 | — | 0 全部档位 |
| `All` | `all` | form | `bool` | 否 | — | true 时包含已下架套餐（续费页要能显示旧档） |

### `MbPlansResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbPlansData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMbPlan`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PlanId` | `plan_id` | form | `int64` | 否 | — | — |
| `PlanCode` | `plan_code` | form | `string` | 否 | — | — |

### `MbPlanResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbPlan` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMbMy`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `VipType` | `vip_type` | form | `int32` | 否 | — | 0 表示返回当前生效的最高档 |

### `MbMyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbMyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMbEntitlements`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Codes` | `codes` | form | `[]string` | 是 | split | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `MbEntitlementsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbEntitlementsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMbAutoRenew`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `VipType` | `vip_type` | form | `int32` | 是 | — | — |
| `On` | `on` | form | `bool` | 是 | — | — |
| `Channel` | `channel` | form | `string` | 否 | — | on=true 必填；本项目只接受 SANDBOX |
| `RequestId` | `request_id` | form | `string` | 是 | — | 幂等键，原样透传给下游 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `MbAutoRenewResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbAutoRenewData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamMbGrants`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `VipType` | `vip_type` | form | `int32` | 否 | — | — |
| `FromTs` | `from_ts` | form | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | form | `int64` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | 0 由服务取默认并截断到上限 |

### `MbGrantsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `MbGrantsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `MbPlansData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Plans` | `plans` | json | `[]MbPlan` | 是 | — | — |

### `MbPlan`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PlanId` | `plan_id` | json | `int64` | 是 | — | — |
| `PlanCode` | `plan_code` | json | `string` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |
| `DurationDays` | `duration_days` | json | `int32` | 是 | — | — |
| `UnitCount` | `unit_count` | json | `int32` | 是 | — | — |
| `PriceMinor` | `price_minor` | json | `int64` | 是 | — | 原价（分） |
| `PromPriceMinor` | `prom_price_minor` | json | `int64` | 是 | — | 0 表示无促销 |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Platforms` | `platforms` | json | `[]int32` | 是 | — | — |
| `AutoRenewSupported` | `auto_renew_supported` | json | `bool` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 草稿、2 在售、3 已下架 |

### `MbMyData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | false = 从未开通（不是错误） |
| `Membership` | `membership` | json | `MbMembership` | 是 | — | — |
| `ServerNow` | `server_now` | json | `int64` | 是 | — | — |
| `Entitlements` | `entitlements` | json | `[]MbEntitlementBrief` | 是 | — | — |

### `MbEntitlementsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Decisions` | `decisions` | json | `[]MbEntitlementDecision` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |

### `MbAutoRenewData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `Membership` | `membership` | json | `MbMembership` | 是 | — | — |

### `MbGrantsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Grants` | `grants` | json | `[]MbGrant` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `PageSize` | `page_size` | json | `int64` | 是 | — | — |

### `MbMembership`

> MbMembership 会员身份投影。expire_at<=server_now 即已过期， / 网关不替客户端算「还剩几天」，只把两个时间戳都给出去。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |
| `StartAt` | `start_at` | json | `int64` | 是 | — | — |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | — |
| `AutoRenew` | `auto_renew` | json | `bool` | 是 | — | — |
| `AutoRenewChannel` | `auto_renew_channel` | json | `string` | 是 | — | — |
| `AutoRenewSignedAt` | `auto_renew_signed_at` | json | `int64` | 是 | — | — |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `PaidMonthCount` | `paid_month_count` | json | `int32` | 是 | — | — |

### `MbEntitlementBrief`

> MbEntitlementBrief 当前档位可得的权益码（只给展示用的码与名称，判定不在此）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `string` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `MinVipType` | `min_vip_type` | json | `int32` | 是 | — | — |

### `MbEntitlementDecision`

> MbEntitlementDecision 逐项判定结论。reason 是**结论**不是错误码： / 「没买过」「已过期」「权益码被下线」「码不存在」在端上文案完全不同，不能合并成 granted=false。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `string` | 是 | — | — |
| `Granted` | `granted` | json | `bool` | 是 | — | — |
| `Reason` | `reason` | json | `int32` | 是 | — | — |

### `MbGrant`

> MbGrant 用户侧「我的会员开通记录」。刻意不投影 operator/payment_no： / 那是运营与对账口径，对用户没有信息量，且 operator 里可能出现工号。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `GrantId` | `grant_id` | json | `int64` | 是 | — | — |
| `VipType` | `vip_type` | json | `int32` | 是 | — | — |
| `Action` | `action` | json | `string` | 是 | — | — |
| `DeltaDays` | `delta_days` | json | `int32` | 是 | — | — |
| `PlanId` | `plan_id` | json | `int64` | 是 | — | — |
| `Source` | `source` | json | `int32` | 是 | — | — |
| `BizOrderNo` | `biz_order_no` | json | `string` | 是 | — | — |
| `BeforeExpireAt` | `before_expire_at` | json | `int64` | 是 | — | — |
| `AfterExpireAt` | `after_expire_at` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/24-membership.md -->
