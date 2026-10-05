# RPC · `membership`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/membership/rpc/membership.proto` |
| protobuf 包 | `membership.v1` |
| go_package | `go-video/services/membership/rpc` |
| 发现用的 etcd key | `membership.v1.rpc`（`services/membership/etc/membership.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`membership.v1.rpc`） |
| 监听 | `8160`（`services/membership/etc/membership.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_membership` |
| 方法数 | 16（service `Membership`） |
| 网关消费方 | `app:MembershipRPC`、`admin:MembershipRPC` |

## 契约说明

> 会员域契约（AGENTS.md §1 商业化范围 2026-09-22 修订后纳入）。
>
> 数据所有权（§5）：本服务持有套餐目录、会员身份、权益判定口径与授予台账；
> account 侧名片里的 vip 字段只是投影，事实源在这里。
>
> 资金语义（关键，别误读）：本服务不碰钱。开通会员的钱由 trade-order 建单、
> payment 走**沙箱台账**受理，履约时才调 GrantMembership。因此
> CheckEntitlement 的 granted=true 是「授予表里确实有没过期的一行」的真实读结论，
> 不是硬编码返回真；没有授予记录就返回未开通。真实支付渠道、退款到卡、提现出金
> 在本项目一概不开接口。

## service `Membership`

> Membership 会员身份、套餐与权益判定服务。

gRPC 方法前缀：`membership.v1.Membership/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `ListPlans` | [`ListPlansReq`](#message-listplansreq) | [`ListPlansReply`](#message-listplansreply) | 终端套餐列表（只含在售且平台可见） |
| 2 | `GetPlan` | [`GetPlanReq`](#message-getplanreq) | [`GetPlanReply`](#message-getplanreply) | 单个套餐读取（下单前置校验用） |
| 3 | `UpsertPlan` | [`UpsertPlanReq`](#message-upsertplanreq) | [`PlanInfo`](#message-planinfo) | 运营面：新建或修改套餐草稿 |
| 4 | `SetPlanState` | [`SetPlanStateReq`](#message-setplanstatereq) | [`PlanInfo`](#message-planinfo) | 运营面：上下架（带理由，写变更台账） |
| 5 | `ListPlansAdmin` | [`ListPlansAdminReq`](#message-listplansadminreq) | [`ListPlansAdminReply`](#message-listplansadminreply) | 运营面：分页查询全部套餐（含草稿与已下架） |
| 6 | `GetMembership` | [`GetMembershipReq`](#message-getmembershipreq) | [`GetMembershipReply`](#message-getmembershipreply) | 我的会员状态 |
| 7 | `CheckEntitlement` | [`CheckEntitlementReq`](#message-checkentitlementreq) | [`CheckEntitlementReply`](#message-checkentitlementreply) | 单项权益判定——全站唯一的会员权益口径出口 |
| 8 | `CheckEntitlements` | [`CheckEntitlementsReq`](#message-checkentitlementsreq) | [`CheckEntitlementsReply`](#message-checkentitlementsreply) | 多项权益判定（播放详情页等一次问多项） |
| 9 | `GrantMembership` | [`GrantMembershipReq`](#message-grantmembershipreq) | [`GrantMembershipReply`](#message-grantmembershipreply) | 开通/续期（只由订单履约或运营授权调用） |
| 10 | `RevokeMembership` | [`RevokeMembershipReq`](#message-revokemembershipreq) | [`RevokeMembershipReply`](#message-revokemembershipreply) | 收回（退款回收/运营纠错） |
| 11 | `SetAutoRenew` | [`SetAutoRenewReq`](#message-setautorenewreq) | [`SetAutoRenewReply`](#message-setautorenewreply) | 自动续费签约位翻转（沙箱，不建真实代扣协议） |
| 12 | `ListGrants` | [`ListGrantsReq`](#message-listgrantsreq) | [`ListGrantsReply`](#message-listgrantsreply) | 授予台账分页（运营面与用户面共用，mid=0 才有跨用户语义） |
| 13 | `ListExpiringMemberships` | [`ListExpiringMembershipsReq`](#message-listexpiringmembershipsreq) | [`ListExpiringMembershipsReply`](#message-listexpiringmembershipsreply) | cron：扫描到期区间 |
| 14 | `ExpireMembership` | [`ExpireMembershipReq`](#message-expiremembershipreq) | [`ExpireMembershipReply`](#message-expiremembershipreply) | cron：幂等置过期 |
| 15 | `ListEntitlements` | [`ListEntitlementsReq`](#message-listentitlementsreq) | [`ListEntitlementsReply`](#message-listentitlementsreply) | 权益码目录读取 |
| 16 | `UpsertEntitlement` | [`UpsertEntitlementReq`](#message-upsertentitlementreq) | [`EntitlementInfo`](#message-entitlementinfo) | 运营面：权益码新增/开关 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `VipType`

> 会员类型

| 值 | 编号 | 说明 |
|---|---|---|
| `VIP_TYPE_UNSPECIFIED` | 0 | — |
| `VIP_TYPE_PREMIUM` | 1 | 大会员 |
| `VIP_TYPE_PREMIUM_PLUS` | 2 | 超级大会员（权益取大会员的超集） |

### enum `PlanSaleState`

> 套餐售卖状态。DRAFT 不对外可见也不可下单；OFF_SALE 只对存量续费可见。

| 值 | 编号 | 说明 |
|---|---|---|
| `PLAN_SALE_STATE_UNSPECIFIED` | 0 | — |
| `PLAN_SALE_STATE_DRAFT` | 1 | — |
| `PLAN_SALE_STATE_ON_SALE` | 2 | — |
| `PLAN_SALE_STATE_OFF_SALE` | 3 | — |

### enum `PlanPlatform`

> 客户端平台位掩码：决定套餐在哪个端可见、能用什么支付方式。 / 不写死某个端的 UI 行为（§6），只提供可选性。

| 值 | 编号 | 说明 |
|---|---|---|
| `PLAN_PLATFORM_UNSPECIFIED` | 0 | — |
| `PLAN_PLATFORM_ANDROID` | 1 | — |
| `PLAN_PLATFORM_IOS` | 2 | — |
| `PLAN_PLATFORM_HARMONY` | 3 | — |
| `PLAN_PLATFORM_DESKTOP` | 4 | — |
| `PLAN_PLATFORM_WEB` | 5 | — |

### enum `GrantSource`

> 授予来源。除 ADMIN_OPS/EXPERIENCE 外都应能回溯到一条 payment 流水。

| 值 | 编号 | 说明 |
|---|---|---|
| `GRANT_SOURCE_UNSPECIFIED` | 0 | — |
| `GRANT_SOURCE_SANDBOX_PURCHASE` | 1 | 沙箱订单履约开通 |
| `GRANT_SOURCE_SANDBOX_AUTO_RENEW` | 2 | 沙箱自动续费（cron 触发，不产生真实扣款） |
| `GRANT_SOURCE_ADMIN_OPS` | 3 | 运营手工开通/延长（必须有 reason） |
| `GRANT_SOURCE_EXPERIENCE` | 4 | 体验会员（活动发放） |
| `GRANT_SOURCE_LEGACY_IMPORT` | 5 | 存量数据迁移 |

### enum `EntitlementReason`

> 权益判定未通过的原因。是结论枚举，不是错误码：调用方要能区分「没买」和「过期」。

| 值 | 编号 | 说明 |
|---|---|---|
| `ENTITLEMENT_REASON_UNSPECIFIED` | 0 | — |
| `ENTITLEMENT_GRANTED` | 1 | 通过 |
| `ENTITLEMENT_NO_MEMBERSHIP` | 2 | 从未开通 |
| `ENTITLEMENT_EXPIRED` | 3 | 曾开通但已过期 |
| `ENTITLEMENT_CODE_DISABLED` | 4 | 权益码本身被下线（运营在本地关掉了） |
| `ENTITLEMENT_CODE_UNKNOWN` | 5 | 权益码不存在（调用方传错，必须暴露而不是放行） |
| `ENTITLEMENT_MID_INVALID` | 6 | mid 非正数（游客态由调用方自己判定为不通过） |
| `ENTITLEMENT_TIER_NOT_ENOUGH` | 7 | 是有效会员但档位不足（持有档 < 权益码要求档），客户端要能区分「没会员」和「档位不够」 |

### message `PlanInfo`

> 套餐（SKU）。价格以最小货币单位（分）计，currency 固定 CNY 也仍要显式带上。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `plan_id` | `int64` | 1 | — | — |
| `plan_code` | `string` | 2 | — | 对外稳定编码，下单用它而不是 plan_id |
| `name` | `string` | 3 | — | — |
| `description` | `string` | 4 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 5 | — | — |
| `duration_days` | `int32` | 6 | — | 单个售卖单位的时长（月卡=31、季卡=93、年卡=366） |
| `unit_count` | `int32` | 7 | — | 一次购买包含几个 duration_days，年卡可为 12 |
| `price_minor` | `int64` | 8 | — | 原价（分） |
| `prom_price_minor` | `int64` | 9 | — | 促销价（分）；0 表示无促销 |
| `currency` | `string` | 10 | — | — |
| `platforms` | [`PlanPlatform`](#enum-planplatform) | 11 | repeated | 可见平台 |
| `auto_renew_supported` | `bool` | 12 | — | 是否支持签约自动续费 |
| `state` | [`PlanSaleState`](#enum-plansalestate) | 13 | — | — |
| `version` | `int64` | 14 | — | CAS 位 |
| `ctime` | `int64` | 15 | — | — |
| `mtime` | `int64` | 16 | — | — |
| `created_by` | `string` | 17 | — | — |
| `updated_by` | `string` | 18 | — | — |

### message `EntitlementInfo`

> 权益码目录项：某个能力需要哪一档会员。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `code` | `string` | 1 | — | 例如 vip.high_bitrate / vip.early_access |
| `name` | `string` | 2 | — | — |
| `description` | `string` | 3 | — | — |
| `min_vip_type` | [`VipType`](#enum-viptype) | 4 | — | 达到该档（含更高档）才通过 |
| `enabled` | `bool` | 5 | — | — |
| `version` | `int64` | 6 | — | — |
| `ctime` | `int64` | 7 | — | — |
| `mtime` | `int64` | 8 | — | — |

### message `MembershipInfo`

> 用户会员身份（按 mid + vip_type 一行）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 2 | — | — |
| `start_at` | `int64` | 3 | — | — |
| `expire_at` | `int64` | 4 | — | 到期时间（Unix 秒）；<= now 即已过期 |
| `auto_renew` | `bool` | 5 | — | 自动续费签约位（沙箱：只记录意愿，不产生真实扣款协议） |
| `auto_renew_channel` | `string` | 6 | — | 签约渠道标识，未签约为空 |
| `auto_renew_signed_at` | `int64` | 7 | — | — |
| `source` | [`GrantSource`](#enum-grantsource) | 8 | — | 最近一次变更来源 |
| `paid_month_count` | `int32` | 9 | — | 累计付费月数快照 |
| `version` | `int64` | 10 | — | — |
| `ctime` | `int64` | 11 | — | — |
| `mtime` | `int64` | 12 | — | — |

### message `GrantInfo`

> 授予/变更台账（谁、因为什么、动了多少天，必须可回溯）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `grant_id` | `int64` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 3 | — | — |
| `action` | `string` | 4 | — | GRANT / EXTEND / REVOKE / EXPIRE |
| `delta_days` | `int32` | 5 | — | 本次影响的时长，收回为负 |
| `plan_id` | `int64` | 6 | — | 关联套餐，0 表示无（运营手工/迁移） |
| `source` | [`GrantSource`](#enum-grantsource) | 7 | — | — |
| `biz_order_no` | `string` | 8 | — | 订单号引用（跨服务只存主键，不建外键） |
| `payment_no` | `string` | 9 | — | 资金流水号引用 |
| `before_expire_at` | `int64` | 10 | — | — |
| `after_expire_at` | `int64` | 11 | — | — |
| `operator` | `string` | 12 | — | 运营工号/服务身份；用户自助开通为 "user" |
| `request_id` | `string` | 13 | — | 幂等键 |
| `reason` | `string` | 14 | — | 台账摘要：不含 PII 与凭据 |
| `ctime` | `int64` | 15 | — | — |

### message `ListPlansReq`

> --- 套餐读侧 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `platform` | [`PlanPlatform`](#enum-planplatform) | 1 | — | 过滤：某端可见（UNSPECIFIED 表示不按平台过滤） |
| `vip_type` | [`VipType`](#enum-viptype) | 2 | — | 过滤档位，UNSPECIFIED 表示全部 |
| `on_sale_only` | `bool` | 3 | — | 终端面 true；admin 面 false |

### message `ListPlansReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `plans` | [`PlanInfo`](#message-planinfo) | 1 | repeated | — |

### message `GetPlanReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `plan_id` | `int64` | 1 | — | 与 plan_code 二选一 |
| `plan_code` | `string` | 2 | — | — |

### message `GetPlanReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `found` | `bool` | 1 | — | — |
| `plan` | [`PlanInfo`](#message-planinfo) | 2 | — | — |

### message `UpsertPlanReq`

> --- 套餐写侧（admin）---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `plan_id` | `int64` | 1 | — | plan_id/plan_code：plan_code 已存在则视为更新，否则新建。 |
| `plan_code` | `string` | 2 | — | — |
| `name` | `string` | 3 | — | — |
| `description` | `string` | 4 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 5 | — | — |
| `duration_days` | `int32` | 6 | — | — |
| `unit_count` | `int32` | 7 | — | — |
| `price_minor` | `int64` | 8 | — | — |
| `prom_price_minor` | `int64` | 9 | — | — |
| `currency` | `string` | 10 | — | — |
| `platforms` | [`PlanPlatform`](#enum-planplatform) | 11 | repeated | — |
| `auto_renew_supported` | `bool` | 12 | — | — |
| `expected_version` | `int64` | 13 | — | 更新时的 CAS 位；新建传 0 |
| `operator` | `string` | 14 | — | 由网关按会话渲染，不接受客户端自报 |
| `request_id` | `string` | 15 | — | 幂等键 |
| `reason` | `string` | 16 | — | 本次改档的理由（进套餐变更台账；空则记默认摘要） |

### message `SetPlanStateReq`

> 上下架。只允许 DRAFT→ON_SALE、ON_SALE→OFF_SALE、OFF_SALE→ON_SALE； / 改价不走这里，必须新建 DRAFT 版本再切换，避免「改完价格对已下单用户追溯生效」。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `plan_id` | `int64` | 1 | — | — |
| `target_state` | [`PlanSaleState`](#enum-plansalestate) | 2 | — | — |
| `expected_version` | `int64` | 3 | — | — |
| `operator` | `string` | 4 | — | — |
| `request_id` | `string` | 5 | — | — |
| `reason` | `string` | 6 | — | 上下架必须写理由，进变更台账 |

### message `ListPlansAdminReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | `int64` | 1 | — | 从 1 开始 |
| `size` | `int64` | 2 | — | — |
| `state` | [`PlanSaleState`](#enum-plansalestate) | 3 | — | UNSPECIFIED 表示不过滤 |
| `vip_type` | [`VipType`](#enum-viptype) | 4 | — | — |
| `keyword` | `string` | 5 | — | 匹配 plan_code/name 前缀 |

### message `ListPlansAdminReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `plans` | [`PlanInfo`](#message-planinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `GetMembershipReq`

> --- 会员身份读侧 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 2 | — | UNSPECIFIED 返回用户当前生效的最高档 |

### message `GetMembershipReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `found` | `bool` | 1 | — | 从未开通过 |
| `membership` | [`MembershipInfo`](#message-membershipinfo) | 2 | — | — |
| `server_now` | `int64` | 3 | — | 让调用方无需自取时钟即可判过期 |
| `granted_entitlements` | [`EntitlementInfo`](#message-entitlementinfo) | 4 | repeated | 当前档位可得的权益码（只读投影，便于详情页一次拿全） |

### message `CheckEntitlementReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `code` | `string` | 2 | — | — |

### message `CheckEntitlementReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `granted` | `bool` | 1 | — | — |
| `reason` | [`EntitlementReason`](#enum-entitlementreason) | 2 | — | — |
| `expire_at` | `int64` | 3 | — | 判定依据的那一行到期时间，未开通为 0 |
| `vip_type` | [`VipType`](#enum-viptype) | 4 | — | 判定依据的档位 |

### message `CheckEntitlementsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `codes` | `string` | 2 | repeated | 播放详情页一次问清多项能力 |

### message `EntitlementDecision`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `code` | `string` | 1 | — | — |
| `granted` | `bool` | 2 | — | — |
| `reason` | [`EntitlementReason`](#enum-entitlementreason) | 3 | — | — |

### message `CheckEntitlementsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `decisions` | [`EntitlementDecision`](#message-entitlementdecision) | 1 | repeated | — |
| `expire_at` | `int64` | 2 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 3 | — | — |

### message `GrantMembershipReq`

> --- 会员身份写侧 --- / 开通/续期。幂等由 request_id 唯一索引保证；同一 request_id 重放返回首次结果， / 第二次带不同参数不得静默改口径（返回冲突错误）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 2 | — | — |
| `plan_id` | `int64` | 3 | — | 0 表示无套餐（运营手工发放） |
| `delta_days` | `int32` | 4 | — | 本次增加的时长；REVOKE 走单独接口 |
| `source` | [`GrantSource`](#enum-grantsource) | 5 | — | — |
| `biz_order_no` | `string` | 6 | — | — |
| `payment_no` | `string` | 7 | — | — |
| `operator` | `string` | 8 | — | — |
| `request_id` | `string` | 9 | — | — |
| `reason` | `string` | 10 | — | — |

### message `GrantMembershipReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | true 表示命中 request_id 重放，未再次加时长 |
| `membership` | [`MembershipInfo`](#message-membershipinfo) | 2 | — | — |
| `grant_id` | `int64` | 3 | — | — |

### message `RevokeMembershipReq`

> 收回（运营纠错/退款回收）。剩余时长清零或按天扣回，两种语义由参数区分。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 2 | — | — |
| `clear_remaining` | `bool` | 3 | — | true 立即失效；false 只扣回 delta_days |
| `delta_days` | `int32` | 4 | — | — |
| `operator` | `string` | 5 | — | — |
| `request_id` | `string` | 6 | — | — |
| `reason` | `string` | 7 | — | 必填：收回是有后果的动作，无理由不受理 |
| `plan_id` | `int64` | 8 | — | 追溯位：退款审批链路（trade-order → RevokeMembership）必须能说明「收回的是哪一单的权益」， / 否则 mb_grant 上「退款回收」和「运营纠错」两类台账无法区分。三个字段可选， / 全空即视为运营手工收回（source=ADMIN_OPS）。 |
| `biz_order_no` | `string` | 9 | — | — |
| `payment_no` | `string` | 10 | — | — |

### message `RevokeMembershipReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `membership` | [`MembershipInfo`](#message-membershipinfo) | 2 | — | — |
| `grant_id` | `int64` | 3 | — | — |

### message `SetAutoRenewReq`

> 自动续费签约/解约。沙箱语义：只翻转意愿位，不建立任何真实代扣协议。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 2 | — | — |
| `on` | `bool` | 3 | — | — |
| `channel` | `string` | 4 | — | on=true 时的渠道标识（SANDBOX 之外的值一律拒绝） |
| `operator` | `string` | 5 | — | 用户自助时为 "user" |
| `request_id` | `string` | 6 | — | — |
| `reason` | `string` | 7 | — | — |

### message `SetAutoRenewReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `membership` | [`MembershipInfo`](#message-membershipinfo) | 2 | — | — |

### message `ListGrantsReq`

> --- 台账与批处理 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 0 表示跨用户查（admin 面） |
| `vip_type` | [`VipType`](#enum-viptype) | 2 | — | — |
| `source` | [`GrantSource`](#enum-grantsource) | 3 | — | — |
| `biz_order_no` | `string` | 4 | — | — |
| `from_ts` | `int64` | 5 | — | — |
| `to_ts` | `int64` | 6 | — | — |
| `page` | `int64` | 7 | — | — |
| `size` | `int64` | 8 | — | — |

### message `ListGrantsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `grants` | [`GrantInfo`](#message-grantinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `ListExpiringMembershipsReq`

> cron 扫描到期区间（按 expire_at 索引，闭区间）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `from_expire_at` | `int64` | 1 | — | — |
| `to_expire_at` | `int64` | 2 | — | — |
| `auto_renew_only` | `bool` | 3 | — | — |
| `limit` | `int64` | 4 | — | 单批上限，超限由服务侧裁剪而不是报错 |

### message `ListExpiringMembershipsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `memberships` | [`MembershipInfo`](#message-membershipinfo) | 1 | repeated | — |
| `next_expire_at_cursor` | `int64` | 2 | — | 0 表示本区间已扫完 |

### message `ExpireMembershipReq`

> cron 幂等置过期：只有 expire_at 确实早于 now 才生效，否则拒绝。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `vip_type` | [`VipType`](#enum-viptype) | 2 | — | — |
| `operator` | `string` | 3 | — | 固定 "cron" |
| `request_id` | `string` | 4 | — | — |
| `reason` | `string` | 5 | — | 进 EXPIRE 台账的理由（空则记默认摘要） |

### message `ExpireMembershipReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `skipped` | `bool` | 1 | — | 未到期或已处理过 |
| `grant_id` | `int64` | 2 | — | — |
| `membership` | [`MembershipInfo`](#message-membershipinfo) | 3 | — | — |

### message `UpsertEntitlementReq`

> --- 权益码目录（admin 写、内部读）---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `code` | `string` | 1 | — | — |
| `name` | `string` | 2 | — | — |
| `description` | `string` | 3 | — | — |
| `min_vip_type` | [`VipType`](#enum-viptype) | 4 | — | — |
| `enabled` | `bool` | 5 | — | — |
| `expected_version` | `int64` | 6 | — | — |
| `operator` | `string` | 7 | — | — |
| `request_id` | `string` | 8 | — | — |

### message `ListEntitlementsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `enabled_only` | `bool` | 1 | — | — |

### message `ListEntitlementsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entitlements` | [`EntitlementInfo`](#message-entitlementinfo) | 1 | repeated | — |
