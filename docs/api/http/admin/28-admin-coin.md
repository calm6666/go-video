# 运营面 · `/admin/coin`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化运营面：coin 域（社区硬币，不是钱） | 免鉴权 | 3 |
| 商业化运营面：coin 域（社区硬币，不是钱） | AdminPermission | 1 |

合计 **4** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化运营面：coin 域（社区硬币，不是钱）（免鉴权，3 条）

> -------------------- coin 只读面（不进 routePermissions） --------------------
> 账户、流水台账与参数读取都是读取，与 membership/payment/order 读面同口径。
> 注意 /toss/config 也是只读：投币规则（日限、单片上限、取消窗口）由服务配置投影，
> 本仓库**不开**后台改这套参数的路由——它属运营配置（ops-config）域，不属 coin。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/coin/account/get` | 硬币账户（含今日额度；无账户时 found=false 且余额 0） | `coinAccountGet` | `coinaccountgetlogic.go` |
| POST | `/admin/coin/flow/list` | 硬币流水台账分页（区分投币/撤币/硬币包/运营发放，正入负出） | `coinFlowList` | `coinflowlistlogic.go` |
| POST | `/admin/coin/toss/config` | 生效投币参数（日限/单片上限/取消窗口/初始余额；只读，后台不改这套规则） | `coinTossConfig` | `cointossconfiglogic.go` |

### POST `/admin/coin/account/get` — 硬币账户（含今日额度；无账户时 found=false 且余额 0）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/coinaccountgethandler.go`
- 业务实现：`gateway/admin/internal/logic/coinaccountgetlogic.go`

请求：`ParamCoinAccountGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |

响应：`CoinAccountGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinAccountGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/coin/flow/list` — 硬币流水台账分页（区分投币/撤币/硬币包/运营发放，正入负出）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/coinflowlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/coinflowlistlogic.go`

请求：`ParamCoinFlowList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `FlowType` | `flow_type` | json | `int32` | 否 | — | — |
| `BizNo` | `biz_no` | json | `string` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`CoinFlowListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinFlowListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/coin/toss/config` — 生效投币参数（日限/单片上限/取消窗口/初始余额；只读，后台不改这套规则）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/cointossconfighandler.go`
- 业务实现：`gateway/admin/internal/logic/cointossconfiglogic.go`

请求：`ParamCoinTossConfig`

（该类型无字段：空请求 / 空响应。）

响应：`CoinTossConfigResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTossConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 商业化运营面：coin 域（社区硬币，不是钱）（AdminPermission，1 条）

> -------------------- coin 写面（受 AdminPermission 保护） --------------------
> 本域只有一条写入口，因此只有一个权限点：发放与扣回共用 GrantCoin（delta 决定方向），
> 契约里没有第二个方法可拆。它的补偿口径是 reason + idempotency_key + operator 三者都必填位，
> 且流水类型必须显式声明「买来的」还是「白送的」。硬币不是钱：这条口不产生资金流水。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/coin/grant` | 发放/扣回硬币（正负皆可；只接受 ORDER_PACK/ADMIN_GRANT，不产生资金流水） | `coin:grant` / `create` | `coinGrant` | `coingrantlogic.go` |

### POST `/admin/coin/grant` — 发放/扣回硬币（正负皆可；只接受 ORDER_PACK/ADMIN_GRANT，不产生资金流水）

- 权限口径：AdminPermission · 权限点 `coin:grant` / `create`
- goctl 入口：`gateway/admin/internal/handler/coingranthandler.go`
- 业务实现：`gateway/admin/internal/logic/coingrantlogic.go`

请求：`ParamCoinGrant`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Delta` | `delta` | json | `int64` | 是 | — | 不允许为 0 |
| `FlowType` | `flow_type` | json | `int32` | 是 | — | 只接受 ORDER_PACK / ADMIN_GRANT |
| `BizNo` | `biz_no` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | ADMIN_GRANT 必填，由服务判定 |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，重复提交回首次结论 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`CoinGrantResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinGrantData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamCoinAccountGet`

> ParamCoinAccountGet 1:1 对应 GetCoinAccountReq。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |

### `CoinAccountGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinAccountGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCoinFlowList`

> ParamCoinFlowList 1:1 对应 ListCoinFlowsReq；mid=0 为跨用户（运营面）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `FlowType` | `flow_type` | json | `int32` | 否 | — | — |
| `BizNo` | `biz_no` | json | `string` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `CoinFlowListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinFlowListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCoinTossConfig`

> ParamCoinTossConfig 对应 GetTossConfigReq（空请求）：日限/单片上限/取消窗口都来自服务端配置， / 客户端不写死（§6），运营页也要能读到当前生效值。

（该类型无字段：空请求 / 空响应。）

### `CoinTossConfigResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinTossConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCoinGrant`

> ParamCoinGrant 1:1 对应 GrantCoinReq（单一发放口：正数发放、负数扣回）。 / flow_type 只接受 ORDER_PACK / ADMIN_GRANT——把「买来的」和「白送的」混在一个类型里， / 对账时就再也分不开了。ADMIN_GRANT 的 reason 必填；biz_no 建议带工单号。 / 日限/单片上限/取消窗口等规则与额度判定全在 coin 侧，网关不代为放宽。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Delta` | `delta` | json | `int64` | 是 | — | 不允许为 0 |
| `FlowType` | `flow_type` | json | `int32` | 是 | — | 只接受 ORDER_PACK / ADMIN_GRANT |
| `BizNo` | `biz_no` | json | `string` | 否 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | ADMIN_GRANT 必填，由服务判定 |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id，重复提交回首次结论 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `CoinGrantResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `CoinGrantData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `CoinAccountGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | 从未有过账户时 found=false 且 balance=0，不是错误 |
| `Account` | `account` | json | `CoinAccountItem` | 是 | — | — |

### `CoinFlowListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]CoinFlowItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `CoinTossConfigData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DailyLimit` | `daily_limit` | json | `int64` | 是 | — | — |
| `PerTargetLimit` | `per_target_limit` | json | `int64` | 是 | — | — |
| `CancelWindowSeconds` | `cancel_window_seconds` | json | `int64` | 是 | — | — |
| `MinBalanceToToss` | `min_balance_to_toss` | json | `int64` | 是 | — | — |
| `InitialBalance` | `initial_balance` | json | `int64` | 是 | — | 新建账户初始硬币（沙箱便利，非赠送规则） |

### `CoinGrantData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `FlowId` | `flow_id` | json | `int64` | 是 | — | — |
| `Account` | `account` | json | `CoinAccountItem` | 是 | — | — |

### `CoinAccountItem`

> 契约来源 services/coin/rpc/coin.proto（冻结）。 /  / 语义边界（AGENTS.md §1/§5）：硬币是**社区虚拟币**，不是钱。coin 服务持有余额、投币记录与 / 每日额度；video/engagement 侧的 coin_count 只是投影。硬币与 payment 的现金余额是两套账， / **不互换、不折算**，本域任何路由都不出现「金额」与「硬币」互相换算的字段。 / 硬币的获得只有两条路：运营发放（GrantCoin，必须有 reason）与买硬币包（trade-order 履约时 / 调 GrantCoin，biz_no 带订单号）；消耗只有投币。全链路不涉及真实资金。 /  / 刻意**不开**的路由（理由写在这里，不是漏实现）： /   - TossCoin / CancelToss / ListMyTosses：投币与撤币是**终端用户动作**（归 gateway/app）。 /     后台替用户投币会伪造「谁喜欢这条内容」的信号，直接污染 spm 的互动特征（§7）； /   - GetTargetSummary / BatchGetTargetSummary / ListTargetTossers：内容侧聚合读， /     服务详情页与审核面（归 gateway/app 与 moderation），后台列表不需要按内容维度翻。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Balance` | `balance` | json | `int64` | 是 | — | 当前可用硬币（不是钱） |
| `TotalTossed` | `total_tossed` | json | `int64` | 是 | — | 累计投出，不因取消而回退 |
| `TodayTossed` | `today_tossed` | json | `int64` | 是 | — | — |
| `TodayLimit` | `today_limit` | json | `int64` | 是 | — | 服务侧配置投影，客户端不写死 |
| `PerTargetLimit` | `per_target_limit` | json | `int64` | 是 | — | — |
| `CancelWindowSeconds` | `cancel_window_seconds` | json | `int64` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `CoinFlowItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FlowId` | `flow_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `FlowType` | `flow_type` | json | `int32` | 是 | — | CoinFlowType：1 投币 2 撤币 3 硬币包 4 运营发放 5 过期(未启用) |
| `Delta` | `delta` | json | `int64` | 是 | — | 正入负出 |
| `BalanceAfter` | `balance_after` | json | `int64` | 是 | — | — |
| `TargetAid` | `target_aid` | json | `int64` | 是 | — | 投币类流水才有 |
| `BizNo` | `biz_no` | json | `string` | 是 | — | 订单号/工单号 |
| `Operator` | `operator` | json | `string` | 是 | — | "user" / "trade-order" / 运营工号 / "cron" |
| `RequestId` | `request_id` | json | `string` | 是 | — | 幂等键（唯一索引） |
| `Remark` | `remark` | json | `string` | 是 | — | 摘要，不含 PII |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/28-admin-coin.md -->
