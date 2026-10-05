# RPC · `risk-control`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/risk-control/rpc/riskcontrol.proto` |
| protobuf 包 | `riskcontrol.v1` |
| go_package | `go-video/services/risk-control/rpc` |
| 发现用的 etcd key | `risk-control.v1.rpc`（`services/risk-control/etc/riskcontrol.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`risk-control.v1.rpc`） |
| 监听 | `8105`（`services/risk-control/etc/riskcontrol.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_risk_control` |
| 方法数 | 11（service `RiskControl`） |
| 网关消费方 | `admin:RiskControlRPC` |

## 契约说明

> 说明：risk-control 是账号与行为风控领域服务（AGENTS.md §5 数据所有者）。
> 边界划分：
>   - 内容安全（稿件/评论/弹幕是否违规）归 moderation-orchestrator + moderation-worker；
>   - 本服务只裁决“谁、在什么设备/网络上、以什么频率做某个受保护动作”，
>     即账号风险、行为频率、设备画像与处罚状态；
>   - 本服务不判定内容违规，也不推进稿件状态机（AGENTS.md §8）。
> 契约约束：
>   - 裁决必须可解释：返回 decision + score + 命中的 rule_id/version 明细 + 决策依据 basis；
>   - 不接收也不返回明文 IP、手机号、身份证：IP 只以调用方预哈希的 ip_hash 传入，
>     设备号只以 device_id 传入并仅落库存 hash；
>   - 写接口全部要求幂等键（request_id / event_id / idempotency_key）；
>   - 不提供任何广告投放、广告位分析能力（AGENTS.md §1、§7 红线）。

## service `RiskControl`

> RiskControl 账号与行为风控服务。

gRPC 方法前缀：`riskcontrol.v1.RiskControl/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `CheckAction` | [`CheckActionReq`](#message-checkactionreq) | [`CheckActionReply`](#message-checkactionreply) | 同步裁决一次受保护动作（名单 → 处罚 → 规则 → 降级）。 |
| 2 | `ReportAction` | [`ReportActionReq`](#message-reportactionreq) | [`ReportActionReply`](#message-reportactionreply) | 行为上报，写 Redis 滑窗计数供 CheckAction 评估；不做 SPM 广告分析。 |
| 3 | `GetDeviceProfile` | [`GetDeviceProfileReq`](#message-getdeviceprofilereq) | [`GetDeviceProfileReply`](#message-getdeviceprofilereply) | 查询设备画像。 |
| 4 | `UpsertDeviceProfile` | [`UpsertDeviceProfileReq`](#message-upsertdeviceprofilereq) | [`UpsertDeviceProfileReply`](#message-upsertdeviceprofilereply) | 写入/更新设备画像与设备-账号关联。 |
| 5 | `ApplyPunishment` | [`ApplyPunishmentReq`](#message-applypunishmentreq) | [`ApplyPunishmentReply`](#message-applypunishmentreply) | 运营/审核下发处罚。 |
| 6 | `LiftPunishment` | [`LiftPunishmentReq`](#message-liftpunishmentreq) | [`LiftPunishmentReply`](#message-liftpunishmentreply) | 解除处罚（幂等，已终态返回当前状态）。 |
| 7 | `ListPunishments` | [`ListPunishmentsReq`](#message-listpunishmentsreq) | [`ListPunishmentsReply`](#message-listpunishmentsreply) | 分页查询处罚。 |
| 8 | `UpsertRule` | [`UpsertRuleReq`](#message-upsertrulereq) | [`UpsertRuleReply`](#message-upsertrulereply) | 新增/更新风控规则（版本递增，operator 必填）。 |
| 9 | `ListRules` | [`ListRulesReq`](#message-listrulesreq) | [`ListRulesReply`](#message-listrulesreply) | 分页查询风控规则。 |
| 10 | `UpsertListEntry` | [`UpsertListEntryReq`](#message-upsertlistentryreq) | [`UpsertListEntryReply`](#message-upsertlistentryreply) | 新增/更新名单条目。 |
| 11 | `GetListEntries` | [`GetListEntriesReq`](#message-getlistentriesreq) | [`GetListEntriesReply`](#message-getlistentriesreply) | 分页查询名单条目。 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `GuardedAction`

> GuardedAction 受风控保护的账号/行为动作。 / 数值稳定，新增动作只能追加，不得复用已删除编号。

| 值 | 编号 | 说明 |
|---|---|---|
| `ACTION_UNSPECIFIED` | 0 | 未指定（视为非法入参） |
| `ACTION_SUBMIT_VIDEO` | 1 | 投稿 |
| `ACTION_COMMENT` | 2 | 评论 |
| `ACTION_DANMAKU` | 3 | 弹幕 |
| `ACTION_FOLLOW` | 4 | 关注 |
| `ACTION_LOGIN` | 5 | 登录 |
| `ACTION_RENAME` | 6 | 改名 |
| `ACTION_LIVE_START` | 7 | 直播开播 |

### enum `Decision`

> Decision 风控裁决结果。

| 值 | 编号 | 说明 |
|---|---|---|
| `DECISION_UNSPECIFIED` | 0 | 未指定 |
| `DECISION_ALLOW` | 1 | 放行 |
| `DECISION_CHALLENGE` | 2 | 要求人机校验/二次校验后放行 |
| `DECISION_BLOCK` | 3 | 拒绝本次动作 |
| `DECISION_REVIEW` | 4 | 放行但结果进入人工/异步复核（不用于阻断类动作） |

### enum `PunishmentState`

> PunishmentState 处罚状态机：ACTIVE → LIFTED（运营解除）／EXPIRED（到期）。

| 值 | 编号 | 说明 |
|---|---|---|
| `PUNISHMENT_STATE_UNSPECIFIED` | 0 | — |
| `PUNISHMENT_STATE_ACTIVE` | 1 | 生效中 |
| `PUNISHMENT_STATE_LIFTED` | 2 | 已解除（终态） |
| `PUNISHMENT_STATE_EXPIRED` | 3 | 已过期（终态） |

### enum `ListType`

> ListType 名单类型。

| 值 | 编号 | 说明 |
|---|---|---|
| `LIST_TYPE_UNSPECIFIED` | 0 | — |
| `LIST_TYPE_BLACK` | 1 | 黑名单：命中直接 BLOCK |
| `LIST_TYPE_WHITE` | 2 | 白名单：命中跳过规则评估（不覆盖生效处罚） |

### enum `TargetType`

> TargetType 名单目标类型。

| 值 | 编号 | 说明 |
|---|---|---|
| `TARGET_TYPE_UNSPECIFIED` | 0 | — |
| `TARGET_TYPE_MID` | 1 | 账号 ID |
| `TARGET_TYPE_DEVICE` | 2 | 设备 hash（不落明文设备号） |
| `TARGET_TYPE_IP_HASH` | 3 | 调用方预哈希的 IP 摘要 |

### enum `Metric`

> Metric 规则评估使用的指标来源。 / 未实现的指标在评估时按“不可观测”跳过，不会被误判为命中。

| 值 | 编号 | 说明 |
|---|---|---|
| `METRIC_UNSPECIFIED` | 0 | — |
| `METRIC_ACTION_COUNT` | 1 | mid+action 滑窗内动作次数（Redis 计数器） |
| `METRIC_DEVICE_ACTION_COUNT` | 2 | device+action 滑窗内动作次数 |
| `METRIC_IP_ACTION_COUNT` | 3 | ip_hash+action 滑窗内动作次数 |
| `METRIC_DEVICE_RISK_SCORE` | 4 | 设备画像风险分（0-100） |
| `METRIC_DEVICE_MID_COUNT` | 5 | 设备关联账号数 |

### enum `CompareOp`

> CompareOp 阈值比较方向。

| 值 | 编号 | 说明 |
|---|---|---|
| `OP_UNSPECIFIED` | 0 | — |
| `OP_GT` | 1 | 观测值 > 阈值 |
| `OP_GTE` | 2 | 观测值 >= 阈值 |
| `OP_LT` | 3 | 观测值 < 阈值 |
| `OP_LTE` | 4 | 观测值 <= 阈值 |
| `OP_EQ` | 5 | 观测值 == 阈值 |

### message `Rule`

> --- 公共结构 --- / Rule 风控规则（决策依据可解释性的来源，version 递增以便解释历史裁决）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule_id` | `int64` | 1 | — | 规则 ID |
| `name` | `string` | 2 | — | 规则名（全局唯一） |
| `action_type` | [`GuardedAction`](#enum-guardedaction) | 3 | — | 适用动作，ACTION_UNSPECIFIED 表示全部动作 |
| `metric` | [`Metric`](#enum-metric) | 4 | — | 指标 |
| `op` | [`CompareOp`](#enum-compareop) | 5 | — | 比较方向 |
| `threshold` | `int64` | 6 | — | 阈值 |
| `window_seconds` | `int64` | 7 | — | 统计时间窗（秒） |
| `decision` | [`Decision`](#enum-decision) | 8 | — | 命中后的裁决 |
| `priority` | `int32` | 9 | — | 优先级（越大越先出现在 hit_rule_ids） |
| `state` | `int32` | 10 | — | 0 禁用、1 启用 |
| `version` | `int32` | 11 | — | 规则版本，任一评估字段变更即 +1 |
| `operator` | `int64` | 12 | — | 最近一次变更的运营 ID（审计必填） |
| `ctime` | `int64` | 13 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 14 | — | 修改时间（Unix 秒） |

### message `RuleHit`

> RuleHit 单条规则命中明细（决策解释）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule_id` | `int64` | 1 | — | — |
| `version` | `int32` | 2 | — | 命中时的规则版本 |
| `name` | `string` | 3 | — | — |
| `metric` | [`Metric`](#enum-metric) | 4 | — | — |
| `op` | [`CompareOp`](#enum-compareop) | 5 | — | — |
| `threshold` | `int64` | 6 | — | — |
| `observed` | `int64` | 7 | — | 命中时刻的观测值 |
| `window_seconds` | `int64` | 8 | — | — |
| `decision` | [`Decision`](#enum-decision) | 9 | — | — |
| `priority` | `int32` | 10 | — | — |

### message `PunishmentSnapshot`

> PunishmentSnapshot 生效中的处罚摘要（返回给调用方用于提示剩余时长）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `punishment_id` | `int64` | 1 | — | — |
| `scope` | [`GuardedAction`](#enum-guardedaction) | 2 | — | 生效范围，ACTION_UNSPECIFIED 表示全域 |
| `decision` | [`Decision`](#enum-decision) | 3 | — | — |
| `permanent` | `bool` | 4 | — | true 表示无期限 |
| `end_at` | `int64` | 5 | — | 到期时间（Unix 秒），0 表示永久 |
| `remaining_seconds` | `int64` | 6 | — | 剩余秒数，永久时为 0 |
| `reason_code` | `string` | 7 | — | 面向端的稳定原因码（不含运营内部说明） |

### message `Punishment`

> Punishment 处罚记录（运营视角完整字段，仅 admin 链路使用）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `punishment_id` | `int64` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `scope` | [`GuardedAction`](#enum-guardedaction) | 3 | — | — |
| `decision` | [`Decision`](#enum-decision) | 4 | — | — |
| `reason` | `string` | 5 | — | 运营内部说明，禁止下发终端 |
| `reason_code` | `string` | 6 | — | 面向端的稳定原因码 |
| `operator` | `int64` | 7 | — | 下发处罚的运营 ID |
| `start_at` | `int64` | 8 | — | — |
| `end_at` | `int64` | 9 | — | 0 表示永久 |
| `state` | [`PunishmentState`](#enum-punishmentstate) | 10 | — | — |
| `idempotency_key` | `string` | 11 | — | — |
| `lift_operator` | `int64` | 12 | — | 解除人（0 表示未解除/系统过期） |
| `ctime` | `int64` | 13 | — | — |
| `mtime` | `int64` | 14 | — | — |

### message `DeviceProfile`

> DeviceProfile 设备画像（只暴露 device_hash，不回传设备号原文）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `device_hash` | `string` | 1 | — | sha256(device_id) 十六进制，受控 ID |
| `labels` | `string` | 2 | repeated | 风险标签 |
| `risk_score` | `int32` | 3 | — | 0-100 |
| `first_seen` | `int64` | 4 | — | — |
| `last_seen` | `int64` | 5 | — | — |
| `related_mid_count` | `int64` | 6 | — | 关联账号数（来自 risk_device_mid） |
| `ctime` | `int64` | 7 | — | — |
| `mtime` | `int64` | 8 | — | — |

### message `CheckActionReq`

> --- CheckAction：同步裁决 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID（登录类动作可为 0） |
| `action` | [`GuardedAction`](#enum-guardedaction) | 2 | — | 受保护动作 |
| `device_id` | `string` | 3 | — | 设备标识原文，服务端只落库存 hash |
| `ip_hash` | `string` | 4 | — | 调用方预哈希的 IP 摘要（禁止传明文 IP） |
| `platform` | `string` | 5 | — | android/ios/harmony/desktop |
| `app_version` | `string` | 6 | — | — |
| `request_context` | `map<string, string>` | 7 | — | 业务上下文摘要（键值白名单，不入库敏感值） |
| `request_id` | `string` | 8 | — | 幂等键；为空时服务端生成 |
| `trace_id` | `string` | 9 | — | — |

### message `CheckActionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `request_id` | `string` | 1 | — | 回填实际使用的幂等键 |
| `decision` | [`Decision`](#enum-decision) | 2 | — | — |
| `score` | `int32` | 3 | — | 0-100，规则命中的严重度合成，见 README |
| `hit_rule_ids` | `int64` | 4 | repeated | 按 (priority DESC, rule_id ASC) 排序 |
| `rule_hits` | [`RuleHit`](#message-rulehit) | 5 | repeated | 与 hit_rule_ids 同序的命中明细 |
| `punishment` | [`PunishmentSnapshot`](#message-punishmentsnapshot) | 6 | — | 生效处罚摘要（无处罚时不返回） |
| `action_code` | `string` | 7 | — | 建议动作文案 code（客户端自行渲染，服务端不写死 UI） |
| `challenge_ttl_seconds` | `int64` | 8 | — | CHALLENGE 建议有效期，其它裁决为 0 |
| `basis` | `string` | 9 | — | 决策依据：blacklist/whitelist/punishment/rules/no_rule/fallback_db_unavailable/fallback_invalid |
| `skipped_rule_ids` | `int64` | 10 | repeated | 因依赖不可观测被跳过的规则 |
| `evaluated` | `bool` | 11 | — | 是否执行了规则评估（名单/处罚短路时为 false） |
| `degraded` | `bool` | 12 | — | 是否处于依赖故障降级路径 |

### message `ReportActionReq`

> --- ReportAction：行为上报（写滑窗计数，非 SPM 广告分析） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `action` | [`GuardedAction`](#enum-guardedaction) | 2 | — | — |
| `device_id` | `string` | 3 | — | — |
| `ip_hash` | `string` | 4 | — | — |
| `platform` | `string` | 5 | — | — |
| `count` | `int64` | 6 | — | 增量，<=0 视为 1 |
| `occurred_at` | `int64` | 7 | — | 事件发生时间（Unix 秒），0 表示服务端当前时间 |
| `event_id` | `string` | 8 | — | 幂等键：同 event_id 只计一次 |
| `trace_id` | `string` | 9 | — | — |

### message `ReportActionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `deduplicated` | `bool` | 1 | — | true 表示 event_id 命中去重，未累加 |
| `window_seconds` | `int64` | 2 | — | 本次使用的统计窗口 |
| `mid_count` | `int64` | 3 | — | 上报后 mid+action 窗口计数 |
| `device_count` | `int64` | 4 | — | 上报后 device+action 窗口计数（无设备号时为 0） |
| `ip_count` | `int64` | 5 | — | 上报后 ip_hash+action 窗口计数（无 ip_hash 时为 0） |

### message `GetDeviceProfileReq`

> --- 设备画像 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `device_id` | `string` | 1 | — | 设备标识原文（仅用于计算 hash，不落库） |
| `device_hash` | `string` | 2 | — | 也可直接传已受控 ID，优先 device_id |

### message `GetDeviceProfileReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `profile` | [`DeviceProfile`](#message-deviceprofile) | 1 | — | — |
| `found` | `bool` | 2 | — | — |

### message `UpsertDeviceProfileReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `device_id` | `string` | 1 | — | — |
| `device_hash` | `string` | 2 | — | — |
| `labels` | `string` | 3 | repeated | 追加合并，去重后按字典序存储 |
| `risk_score` | `int32` | 4 | — | >=0 时覆盖，<0 表示不修改 |
| `mid` | `int64` | 5 | — | >0 时记录设备-账号关联（幂等） |
| `source` | `string` | 6 | — | 写入来源：login/gateway/operation/system，审计用 |
| `operator` | `int64` | 7 | — | 人工改标签时必填（>0），系统写入可为 0 |
| `idempotency_key` | `string` | 8 | — | 可选：人工写入的幂等键 |

### message `UpsertDeviceProfileReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `profile` | [`DeviceProfile`](#message-deviceprofile) | 1 | — | — |
| `created` | `bool` | 2 | — | false 表示更新既有画像 |
| `relation_added` | `bool` | 3 | — | 本次是否新增设备-账号关联 |

### message `ApplyPunishmentReq`

> --- 处罚 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `scope` | [`GuardedAction`](#enum-guardedaction) | 2 | — | ACTION_UNSPECIFIED 表示全域处罚 |
| `decision` | [`Decision`](#enum-decision) | 3 | — | 生效裁决：BLOCK/CHALLENGE/REVIEW |
| `reason` | `string` | 4 | — | 运营内部说明 |
| `reason_code` | `string` | 5 | — | 面向端的稳定原因码 |
| `operator` | `int64` | 6 | — | 必填 >0，审计 |
| `duration_seconds` | `int64` | 7 | — | >0 限期处罚；0 表示永久 |
| `idempotency_key` | `string` | 8 | — | 必填：同 key 重复下发返回既有处罚 |
| `trace_id` | `string` | 9 | — | — |

### message `ApplyPunishmentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `punishment` | [`Punishment`](#message-punishment) | 1 | — | — |
| `created` | `bool` | 2 | — | false 表示幂等命中既有处罚 |

### message `LiftPunishmentReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `punishment_id` | `int64` | 1 | — | 优先使用；为 0 时按 (mid, scope) 取最新生效处罚 |
| `mid` | `int64` | 2 | — | — |
| `scope` | [`GuardedAction`](#enum-guardedaction) | 3 | — | — |
| `operator` | `int64` | 4 | — | 必填 >0 |
| `reason` | `string` | 5 | — | — |
| `idempotency_key` | `string` | 6 | — | 可选：重复解除的幂等键（仅用于日志关联） |

### message `LiftPunishmentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `punishment` | [`Punishment`](#message-punishment) | 1 | — | — |
| `changed` | `bool` | 2 | — | false 表示处罚已是终态（幂等返回当前状态） |

### message `ListPunishmentsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 0 表示不过滤 |
| `scope` | [`GuardedAction`](#enum-guardedaction) | 2 | — | ACTION_UNSPECIFIED 表示不过滤 |
| `state` | [`PunishmentState`](#enum-punishmentstate) | 3 | — | UNSPECIFIED 表示不过滤 |
| `only_active` | `bool` | 4 | — | true 时仅返回当前时间生效中的处罚 |
| `pn` | `int32` | 5 | — | — |
| `ps` | `int32` | 6 | — | 最大 50 |

### message `ListPunishmentsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `punishments` | [`Punishment`](#message-punishment) | 1 | repeated | — |
| `total` | `int32` | 2 | — | — |
| `pn` | `int32` | 3 | — | — |
| `ps` | `int32` | 4 | — | — |

### message `UpsertRuleReq`

> --- 规则管理 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule_id` | `int64` | 1 | — | 0 表示新建 |
| `name` | `string` | 2 | — | 全局唯一，新建必填 |
| `action_type` | [`GuardedAction`](#enum-guardedaction) | 3 | — | — |
| `metric` | [`Metric`](#enum-metric) | 4 | — | 必须是已实现指标 |
| `op` | [`CompareOp`](#enum-compareop) | 5 | — | — |
| `threshold` | `int64` | 6 | — | — |
| `window_seconds` | `int64` | 7 | — | >0 |
| `decision` | [`Decision`](#enum-decision) | 8 | — | 不能是 ALLOW（放行不是处罚性裁决） |
| `priority` | `int32` | 9 | — | — |
| `state` | `int32` | 10 | — | 0 禁用、1 启用 |
| `operator` | `int64` | 11 | — | 必填 >0：规则变更必须由 admin operator 审计 |
| `idempotency_key` | `string` | 12 | — | 可选：避免运营重试产生新版本 |

### message `UpsertRuleReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule` | [`Rule`](#message-rule) | 1 | — | — |
| `created` | `bool` | 2 | — | — |

### message `ListRulesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `action_type` | [`GuardedAction`](#enum-guardedaction) | 1 | — | ACTION_UNSPECIFIED 表示不过滤 |
| `metric` | [`Metric`](#enum-metric) | 2 | — | METRIC_UNSPECIFIED 表示不过滤 |
| `state` | `int32` | 3 | — | -1 不过滤，0/1 精确过滤 |
| `pn` | `int32` | 4 | — | — |
| `ps` | `int32` | 5 | — | 最大 50 |

### message `ListRulesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rules` | [`Rule`](#message-rule) | 1 | repeated | — |
| `total` | `int32` | 2 | — | — |
| `pn` | `int32` | 3 | — | — |
| `ps` | `int32` | 4 | — | — |

### message `UpsertListEntryReq`

> --- 名单管理（risk-control 拥有 risk_list 表，写入只能走本服务） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list_type` | [`ListType`](#enum-listtype) | 1 | — | — |
| `target_type` | [`TargetType`](#enum-targettype) | 2 | — | — |
| `target_value` | `string` | 3 | — | mid 十进制字符串 / device_hash / ip_hash，禁止明文 IP |
| `reason` | `string` | 4 | — | 运营内部说明 |
| `operator` | `int64` | 5 | — | 必填 >0 |
| `duration_seconds` | `int64` | 6 | — | >0 限期；0 永久 |
| `state` | `int32` | 7 | — | 0 停用、1 生效 |
| `idempotency_key` | `string` | 8 | — | 可选，审计用 |

### message `UpsertListEntryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entry` | [`ListEntry`](#message-listentry) | 1 | — | — |
| `created` | `bool` | 2 | — | — |

### message `ListEntry`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `list_type` | [`ListType`](#enum-listtype) | 2 | — | — |
| `target_type` | [`TargetType`](#enum-targettype) | 3 | — | — |
| `target_value` | `string` | 4 | — | — |
| `reason` | `string` | 5 | — | — |
| `operator` | `int64` | 6 | — | — |
| `expire_at` | `int64` | 7 | — | 0 表示永久 |
| `state` | `int32` | 8 | — | — |
| `ctime` | `int64` | 9 | — | — |
| `mtime` | `int64` | 10 | — | — |

### message `GetListEntriesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list_type` | [`ListType`](#enum-listtype) | 1 | — | UNSPECIFIED 表示不过滤 |
| `target_type` | [`TargetType`](#enum-targettype) | 2 | — | UNSPECIFIED 表示不过滤 |
| `target_value` | `string` | 3 | — | 空表示不过滤 |
| `state` | `int32` | 4 | — | -1 不过滤 |
| `pn` | `int32` | 5 | — | — |
| `ps` | `int32` | 6 | — | — |

### message `GetListEntriesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entries` | [`ListEntry`](#message-listentry) | 1 | repeated | — |
| `total` | `int32` | 2 | — | — |
| `pn` | `int32` | 3 | — | — |
| `ps` | `int32` | 4 | — | — |
