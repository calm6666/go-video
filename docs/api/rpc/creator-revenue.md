# RPC · `creator-revenue`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/creator-revenue/rpc/creatorrevenue.proto` |
| protobuf 包 | `creatorrevenue.v1` |
| go_package | `go-video/services/creator-revenue/rpc` |
| 发现用的 etcd key | `creatorrevenue.v1.rpc`（`services/creator-revenue/etc/creatorrevenue.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`creatorrevenue.v1.rpc`） |
| 监听 | `8164`（`services/creator-revenue/etc/creatorrevenue.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_creator_revenue` |
| 方法数 | 16（service `CreatorRevenue`） |
| 网关消费方 | `app:CreatorRevenueRPC`、`admin:CreatorRevenueRPC` |

## 契约说明

> 创作者分成域契约。
>
> 数据所有权（§5）：本服务持有分成规则、参与关系、收益计量台账与结算单。
> 计量原始事实（播放时长、完播、投币）归 spm / coin，本服务只保存**按规则折算后的台账**，
> 不复制内容主资料，也不反向修改它们的计数。
>
> 出金不在范围内（重要）：本服务只做到「算出该给多少并落成可审计的结算单」。
> 提现、打款、银行卡、发票、税务、对账文件一概不开接口——结算单的
> payout_state 恒为 NOT_PAYABLE，任何调用方都不能从这里得到「钱已出账」的结论。
> 台账金额是**应计金额**（分），不是已支付金额。
>
> 数据来源边界（§7）：计量输入只来自行为分析与投币事实，不接受任何广告参数。

## service `CreatorRevenue`

> CreatorRevenue 创作者分成服务。

gRPC 方法前缀：`creatorrevenue.v1.CreatorRevenue/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `UpsertRevenueRule` | [`UpsertRevenueRuleReq`](#message-upsertrevenuerulereq) | [`RevenueRuleInfo`](#message-revenueruleinfo) | 运营面：新增/修改规则草稿 |
| 2 | `SetRevenueRuleState` | [`SetRevenueRuleStateReq`](#message-setrevenuerulestatereq) | [`RevenueRuleInfo`](#message-revenueruleinfo) | 运营面：规则状态切换（DRAFT→ACTIVE→ARCHIVED） |
| 3 | `ListRevenueRules` | [`ListRevenueRulesReq`](#message-listrevenuerulesreq) | [`ListRevenueRulesReply`](#message-listrevenuerulesreply) | 规则列表（运营面可见全部，创作者端只查 ACTIVE） |
| 4 | `GetRevenueRule` | [`GetRevenueRuleReq`](#message-getrevenuerulereq) | [`GetRevenueRuleReply`](#message-getrevenuerulereply) | 规则读取（支持按历史版本复核） |
| 5 | `EnrollCreator` | [`EnrollCreatorReq`](#message-enrollcreatorreq) | [`EnrollCreatorReply`](#message-enrollcreatorreply) | 参加计划（必须带已确认的规则版本） |
| 6 | `LeavePlan` | [`LeavePlanReq`](#message-leaveplanreq) | [`LeavePlanReply`](#message-leaveplanreply) | 退出计划 |
| 7 | `SetEnrollmentState` | [`SetEnrollmentStateReq`](#message-setenrollmentstatereq) | [`SetEnrollmentStateReply`](#message-setenrollmentstatereply) | 运营面：暂停/恢复参与 |
| 8 | `GetEnrollment` | [`GetEnrollmentReq`](#message-getenrollmentreq) | [`GetEnrollmentReply`](#message-getenrollmentreply) | 查询参与状态 |
| 9 | `ListEnrollments` | [`ListEnrollmentsReq`](#message-listenrollmentsreq) | [`ListEnrollmentsReply`](#message-listenrollmentsreply) | 运营面：参与名单分页 |
| 10 | `RecordRevenueMetric` | [`RecordRevenueMetricReq`](#message-recordrevenuemetricreq) | [`RecordRevenueMetricReply`](#message-recordrevenuemetricreply) | 写入/更正计量台账（cron、spm 回填或运营手工激励） |
| 11 | `ListRevenueMetrics` | [`ListRevenueMetricsReq`](#message-listrevenuemetricsreq) | [`ListRevenueMetricsReply`](#message-listrevenuemetricsreply) | 计量台账分页 |
| 12 | `GenerateSettlement` | [`GenerateSettlementReq`](#message-generatesettlementreq) | [`GenerateSettlementReply`](#message-generatesettlementreply) | 生成/重算周期结算单 |
| 13 | `ConfirmSettlement` | [`ConfirmSettlementReq`](#message-confirmsettlementreq) | [`ConfirmSettlementReply`](#message-confirmsettlementreply) | 运营确认结算单（金额冻结） |
| 14 | `ListSettlements` | [`ListSettlementsReq`](#message-listsettlementsreq) | [`ListSettlementsReply`](#message-listsettlementsreply) | 结算单分页 |
| 15 | `GetSettlement` | [`GetSettlementReq`](#message-getsettlementreq) | [`GetSettlementReply`](#message-getsettlementreply) | 结算单详情（含分项） |
| 16 | `GetRevenueSummary` | [`GetRevenueSummaryReq`](#message-getrevenuesummaryreq) | [`GetRevenueSummaryReply`](#message-getrevenuesummaryreply) | 创作者端收益概览 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `RevenueSourceType`

> 收益来源类型

| 值 | 编号 | 说明 |
|---|---|---|
| `REVENUE_SOURCE_TYPE_UNSPECIFIED` | 0 | — |
| `REVENUE_SOURCE_VIP_WATCH` | 1 | 会员有效观看时长折算 |
| `REVENUE_SOURCE_COIN` | 2 | 收到的投币折算 |
| `REVENUE_SOURCE_INTERACTION` | 3 | 有效互动（点赞/收藏/分享）折算 |
| `REVENUE_SOURCE_ACTIVITY` | 4 | 运营活动激励（手工回填，必须有 reason） |

### enum `RuleState`

| 值 | 编号 | 说明 |
|---|---|---|
| `RULE_STATE_UNSPECIFIED` | 0 | — |
| `RULE_STATE_DRAFT` | 1 | — |
| `RULE_STATE_ACTIVE` | 2 | — |
| `RULE_STATE_ARCHIVED` | 3 | — |

### enum `EnrollmentState`

| 值 | 编号 | 说明 |
|---|---|---|
| `ENROLLMENT_STATE_UNSPECIFIED` | 0 | — |
| `ENROLLMENT_STATE_ENROLLED` | 1 | — |
| `ENROLLMENT_STATE_LEFT` | 2 | — |
| `ENROLLMENT_STATE_SUSPENDED` | 3 | 违规暂停（由运营置位，收益不结算） |

### enum `SettlementState`

| 值 | 编号 | 说明 |
|---|---|---|
| `SETTLEMENT_STATE_UNSPECIFIED` | 0 | — |
| `SETTLEMENT_STATE_DRAFT` | 1 | 已算出，未确认 |
| `SETTLEMENT_STATE_CONFIRMED` | 2 | 运营确认，金额冻结不可重算 |
| `SETTLEMENT_STATE_VOIDED` | 3 | 作废（重算前的旧单） |

### enum `PayoutState`

> 出金状态：本项目只有 NOT_PAYABLE 一个真实取值。 / 保留这个字段是为了让「没打款」在数据里可见，而不是靠 README 提醒。

| 值 | 编号 | 说明 |
|---|---|---|
| `PAYOUT_STATE_UNSPECIFIED` | 0 | — |
| `PAYOUT_STATE_NOT_PAYABLE` | 1 | 未接出金通道（当前恒为此值） |

### message `RevenueRuleInfo`

> 分成规则：一类收益怎么折算成金额。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule_id` | `int64` | 1 | — | — |
| `rule_code` | `string` | 2 | — | 稳定编码，计量台账按它定位规则 |
| `source_type` | [`RevenueSourceType`](#enum-revenuesourcetype) | 3 | — | — |
| `name` | `string` | 4 | — | — |
| `description` | `string` | 5 | — | — |
| `unit_price_per_1000_minor` | `int64` | 6 | — | 单价以「每 1000 单位」计，避免时长/互动这类小颗粒度收益被整除成 0。 |
| `currency` | `string` | 7 | — | — |
| `unit` | `string` | 8 | — | 计量单位：minute / coin / interaction |
| `min_quantity` | `int64` | 9 | — | 低于此量不结算（防刷门槛） |
| `monthly_cap_minor` | `int64` | 10 | — | 单用户单来源月度封顶，0 表示不限 |
| `state` | [`RuleState`](#enum-rulestate) | 11 | — | — |
| `effective_from` | `int64` | 12 | — | 生效起点（Unix 秒）：晚于它的周期才用本规则 |
| `version` | `int64` | 13 | — | — |
| `ctime` | `int64` | 14 | — | — |
| `mtime` | `int64` | 15 | — | — |
| `created_by` | `string` | 16 | — | — |
| `updated_by` | `string` | 17 | — | — |

### message `EnrollmentInfo`

> 参与关系（一人一行）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `state` | [`EnrollmentState`](#enum-enrollmentstate) | 2 | — | — |
| `agreed_rule_version` | `int64` | 3 | — | 参与者确认时看到的规则版本，必须可回溯 |
| `enrolled_at` | `int64` | 4 | — | — |
| `left_at` | `int64` | 5 | — | — |
| `updated_at` | `int64` | 6 | — | — |
| `operator` | `string` | 7 | — | 自助为 "user" |
| `remark` | `string` | 8 | — | — |

### message `RevenueMetricInfo`

> 计量台账：一条 = 某周期、某内容、某来源的折算结果。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `metric_id` | `int64` | 1 | — | — |
| `period` | `string` | 2 | — | YYYYMM |
| `mid` | `int64` | 3 | — | — |
| `aid` | `int64` | 4 | — | — |
| `source_type` | [`RevenueSourceType`](#enum-revenuesourcetype) | 5 | — | — |
| `rule_code` | `string` | 6 | — | — |
| `rule_version` | `int64` | 7 | — | 计算时锁定的规则版本（重算口径可追溯） |
| `quantity` | `int64` | 8 | — | — |
| `unit` | `string` | 9 | — | — |
| `amount_minor` | `int64` | 10 | — | 应计金额（分），未做封顶前 |
| `capped_amount_minor` | `int64` | 11 | — | 封顶/门槛后的实际应计 |
| `source_detail` | `string` | 12 | — | 计算依据摘要（例如「有效播放 12,345 次」），不含 PII |
| `ctime` | `int64` | 13 | — | — |
| `mtime` | `int64` | 14 | — | — |

### message `SettlementInfo`

> 结算单（一周期一人一单）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `settlement_no` | `string` | 1 | — | — |
| `period` | `string` | 2 | — | — |
| `mid` | `int64` | 3 | — | — |
| `amount_minor` | `int64` | 4 | — | 应计合计 |
| `cap_applied_minor` | `int64` | 5 | — | 因封顶被扣减的额度（透明化，不让运营猜） |
| `currency` | `string` | 6 | — | — |
| `metric_count` | `int64` | 7 | — | — |
| `state` | [`SettlementState`](#enum-settlementstate) | 8 | — | — |
| `payout_state` | [`PayoutState`](#enum-payoutstate) | 9 | — | 恒为 NOT_PAYABLE |
| `confirmed_at` | `int64` | 10 | — | — |
| `confirmed_by` | `string` | 11 | — | — |
| `void_reason` | `string` | 12 | — | — |
| `ctime` | `int64` | 13 | — | — |
| `mtime` | `int64` | 14 | — | — |

### message `SettlementItem`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `source_type` | [`RevenueSourceType`](#enum-revenuesourcetype) | 1 | — | — |
| `rule_code` | `string` | 2 | — | — |
| `quantity` | `int64` | 3 | — | — |
| `amount_minor` | `int64` | 4 | — | — |

### message `UpsertRevenueRuleReq`

> --- 规则（运营写、创作者只读 ACTIVE）---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule_id` | `int64` | 1 | — | — |
| `rule_code` | `string` | 2 | — | — |
| `source_type` | [`RevenueSourceType`](#enum-revenuesourcetype) | 3 | — | — |
| `name` | `string` | 4 | — | — |
| `description` | `string` | 5 | — | — |
| `unit_price_per_1000_minor` | `int64` | 6 | — | 负数拒绝 |
| `currency` | `string` | 7 | — | — |
| `unit` | `string` | 8 | — | — |
| `min_quantity` | `int64` | 9 | — | — |
| `monthly_cap_minor` | `int64` | 10 | — | — |
| `effective_from` | `int64` | 11 | — | — |
| `expected_version` | `int64` | 12 | — | — |
| `operator` | `string` | 13 | — | 网关按会话渲染 |
| `request_id` | `string` | 14 | — | — |
| `reason` | `string` | 15 | — | 必填：改单价是有后果的动作 |

### message `SetRevenueRuleStateReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule_id` | `int64` | 1 | — | — |
| `target_state` | [`RuleState`](#enum-rulestate) | 2 | — | 只允许 DRAFT→ACTIVE、ACTIVE→ARCHIVED、DRAFT→ARCHIVED |
| `expected_version` | `int64` | 3 | — | — |
| `operator` | `string` | 4 | — | — |
| `request_id` | `string` | 5 | — | — |
| `reason` | `string` | 6 | — | 必填 |

### message `ListRevenueRulesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | [`RuleState`](#enum-rulestate) | 1 | — | UNSPECIFIED 不过滤 |
| `source_type` | [`RevenueSourceType`](#enum-revenuesourcetype) | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `ListRevenueRulesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rules` | [`RevenueRuleInfo`](#message-revenueruleinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `GetRevenueRuleReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule_id` | `int64` | 1 | — | — |
| `rule_code` | `string` | 2 | — | — |
| `version` | `int64` | 3 | — | >0 时按历史版本读取（结算争议复核） |

### message `GetRevenueRuleReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `found` | `bool` | 1 | — | — |
| `rule` | [`RevenueRuleInfo`](#message-revenueruleinfo) | 2 | — | — |

### message `EnrollCreatorReq`

> --- 参与关系 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `agreed_rule_version` | `int64` | 2 | — | 必填：未确认规则版本不得参加 |
| `operator` | `string` | 3 | — | 自助为 "user" |
| `request_id` | `string` | 4 | — | — |

### message `EnrollCreatorReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `enrollment` | [`EnrollmentInfo`](#message-enrollmentinfo) | 2 | — | — |

### message `LeavePlanReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `operator` | `string` | 2 | — | — |
| `request_id` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | 运营代操作必填 |

### message `LeavePlanReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `enrollment` | [`EnrollmentInfo`](#message-enrollmentinfo) | 2 | — | — |

### message `SetEnrollmentStateReq`

> 运营侧状态变更：暂停/恢复（违规处置）。不参与自助。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `target_state` | [`EnrollmentState`](#enum-enrollmentstate) | 2 | — | 只允许 ENROLLED↔SUSPENDED |
| `operator` | `string` | 3 | — | — |
| `request_id` | `string` | 4 | — | — |
| `reason` | `string` | 5 | — | 必填 |

### message `SetEnrollmentStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `enrollment` | [`EnrollmentInfo`](#message-enrollmentinfo) | 2 | — | — |

### message `GetEnrollmentReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |

### message `GetEnrollmentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `found` | `bool` | 1 | — | — |
| `enrollment` | [`EnrollmentInfo`](#message-enrollmentinfo) | 2 | — | — |

### message `ListEnrollmentsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | [`EnrollmentState`](#enum-enrollmentstate) | 1 | — | — |
| `page` | `int64` | 2 | — | — |
| `size` | `int64` | 3 | — | — |

### message `ListEnrollmentsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `enrollments` | [`EnrollmentInfo`](#message-enrollmentinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `RecordRevenueMetricReq`

> --- 计量台账 --- / 幂等：唯一键为 (period, mid, aid, source_type)。重复上报视为「以本次为准的更正」， / 旧值写进变更日志而不是静默覆盖；已确认周期（结算单 CONFIRMED）拒绝更正。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `period` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `aid` | `int64` | 3 | — | — |
| `source_type` | [`RevenueSourceType`](#enum-revenuesourcetype) | 4 | — | — |
| `rule_code` | `string` | 5 | — | — |
| `quantity` | `int64` | 6 | — | 负数拒绝 |
| `operator` | `string` | 7 | — | "cron" / "spm" / 运营工号 |
| `request_id` | `string` | 8 | — | — |
| `source_detail` | `string` | 9 | — | — |
| `reason` | `string` | 10 | — | 手工更正必填 |

### message `RecordRevenueMetricReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `created` | `bool` | 1 | — | — |
| `corrected` | `bool` | 2 | — | true 表示覆盖了旧值（同唯一键的更正上报） |
| `metric` | [`RevenueMetricInfo`](#message-revenuemetricinfo) | 3 | — | — |

### message `ListRevenueMetricsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `period` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `aid` | `int64` | 3 | — | — |
| `source_type` | [`RevenueSourceType`](#enum-revenuesourcetype) | 4 | — | — |
| `page` | `int64` | 5 | — | — |
| `size` | `int64` | 6 | — | — |

### message `ListRevenueMetricsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `metrics` | [`RevenueMetricInfo`](#message-revenuemetricinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `GenerateSettlementReq`

> --- 结算 --- / 生成/重算某周期结算单。幂等：同 (period, mid) 重复调用不会重复出单。 / DRAFT 单可被重算覆盖；已 CONFIRMED 的单拒绝重算（要改必须先作废并显式留痕）。 / 只对 ENROLLED 且非 SUSPENDED 的作者出单。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `period` | `string` | 1 | — | YYYYMM |
| `mid` | `int64` | 2 | — | 0 表示该周期全量（运营/cron） |
| `force_void_confirmed` | `bool` | 3 | — | 危险位：true 时才允许把已确认单置 VOIDED 重算 |
| `operator` | `string` | 4 | — | — |
| `request_id` | `string` | 5 | — | — |
| `reason` | `string` | 6 | — | force_void_confirmed=true 时必填 |

### message `GenerateSettlementReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | 该周期该用户已有未作废单且本次未强制重算 |
| `generated` | `int64` | 2 | — | 本次出单数（全量时 >1） |
| `settlements` | [`SettlementInfo`](#message-settlementinfo) | 3 | repeated | 全量时只回前若干条，完整结果查台账 |
| `truncated` | `bool` | 4 | — | — |

### message `ConfirmSettlementReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `settlement_nos` | `string` | 1 | repeated | 批量确认；空列表拒绝 |
| `operator` | `string` | 2 | — | — |
| `request_id` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | 必填 |

### message `ConfirmSettlementReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `confirmed` | `int64` | 1 | — | — |
| `failed_nos` | `string` | 2 | repeated | 不满足确认条件的单号（状态不符/已作废） |

### message `ListSettlementsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `period` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `state` | [`SettlementState`](#enum-settlementstate) | 3 | — | — |
| `page` | `int64` | 4 | — | — |
| `size` | `int64` | 5 | — | — |

### message `ListSettlementsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `settlements` | [`SettlementInfo`](#message-settlementinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `GetSettlementReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `settlement_no` | `string` | 1 | — | — |
| `mid` | `int64` | 2 | — | 非 0 时校验归属 |

### message `GetSettlementReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `found` | `bool` | 1 | — | — |
| `settlement` | [`SettlementInfo`](#message-settlementinfo) | 2 | — | — |
| `items` | [`SettlementItem`](#message-settlementitem) | 3 | repeated | — |

### message `GetRevenueSummaryReq`

> 创作者端首页：本月预估 + 累计 + 参与状态，一次读全。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |

### message `GetRevenueSummaryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `enrollment` | [`EnrollmentInfo`](#message-enrollmentinfo) | 1 | — | 未参加时 found=false 语义由 enrollment.state 表达 |
| `current_period` | `string` | 2 | — | — |
| `current_estimate_minor` | `int64` | 3 | — | 本月已计量的应计（未结算，会随更正变动） |
| `total_confirmed_minor` | `int64` | 4 | — | 历史已确认应计合计 |
| `last_settled_period` | `int64` | 5 | — | 最近出单周期（YYYYMM 数值化），0 表示无 |
| `payout_available` | `bool` | 6 | — | 恒 false：本项目无出金通道，前端据此显示「暂不可提现」 |
| `payout_note` | `string` | 7 | — | — |
