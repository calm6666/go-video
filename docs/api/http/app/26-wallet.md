# 终端面 · `/wallet`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto） | 免鉴权 | 7 |

合计 **7** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化：creator-revenue 域聚合（services/creator-revenue/rpc/creatorrevenue.proto）（免鉴权，7 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/wallet/balance` | 我的余额（现金台账，沙箱） | `walletBalance` | `walletbalancelogic.go` |
| GET | `/wallet/channels` | 渠道能力自述（sandbox_only/real_money，让端上显式知道不是真实资金） | `walletChannels` | `walletchannelslogic.go` |
| POST | `/wallet/recharge/open` | 开充值单（仅 SANDBOX 渠道） | `walletRechargeOpen` | `walletrechargeopenlogic.go` |
| POST | `/wallet/recharge/settle` | 沙箱结算充值单（把钱记到自己台账上；不请求任何第三方支付） | `walletRechargeSettle` | `walletrechargesettlelogic.go` |
| POST | `/wallet/recharge/cancel` | 取消未结算充值单 | `walletRechargeCancel` | `walletrechargecancellogic.go` |
| GET | `/wallet/recharges` | 我的充值单列表 | `walletRecharges` | `walletrechargeslogic.go` |
| GET | `/wallet/flows` | 我的资金流水 | `walletFlows` | `walletflowslogic.go` |

### GET `/wallet/balance` — 我的余额（现金台账，沙箱）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/walletbalancehandler.go`
- 业务实现：`gateway/app/internal/logic/walletbalancelogic.go`

请求：`ParamWalletBalance`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Currency` | `currency` | form | `string` | 否 | — | — |

响应：`WalletBalanceResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletBalanceData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/wallet/channels` — 渠道能力自述（sandbox_only/real_money，让端上显式知道不是真实资金）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/walletchannelshandler.go`
- 业务实现：`gateway/app/internal/logic/walletchannelslogic.go`

请求：无参数体。

响应：`WalletChannelsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletChannelsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/wallet/recharge/open` — 开充值单（仅 SANDBOX 渠道）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/walletrechargeopenhandler.go`
- 业务实现：`gateway/app/internal/logic/walletrechargeopenlogic.go`

请求：`ParamWalletRechargeOpen`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `AmountMinor` | `amount_minor` | form | `int64` | 是 | — | — |
| `Currency` | `currency` | form | `string` | 否 | — | — |
| `Channel` | `channel` | form | `int32` | 否 | — | 0/1 均按 SANDBOX；其他值由服务拒绝 |
| `RequestId` | `request_id` | form | `string` | 是 | — | 幂等键 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`WalletRechargeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletRechargeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/wallet/recharge/settle` — 沙箱结算充值单（把钱记到自己台账上；不请求任何第三方支付）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/walletrechargesettlehandler.go`
- 业务实现：`gateway/app/internal/logic/walletrechargesettlelogic.go`

请求：`ParamWalletRechargeNo`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RechargeNo` | `recharge_no` | form | `string` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`WalletRechargeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletRechargeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/wallet/recharge/cancel` — 取消未结算充值单

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/walletrechargecancelhandler.go`
- 业务实现：`gateway/app/internal/logic/walletrechargecancellogic.go`

请求：`ParamWalletRechargeNo`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RechargeNo` | `recharge_no` | form | `string` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

响应：`WalletRechargeResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletRechargeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/wallet/recharges` — 我的充值单列表

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/walletrechargeshandler.go`
- 业务实现：`gateway/app/internal/logic/walletrechargeslogic.go`

请求：`ParamWalletRecharges`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `FromTs` | `from_ts` | form | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | form | `int64` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`WalletRechargesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletRechargesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/wallet/flows` — 我的资金流水

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/walletflowshandler.go`
- 业务实现：`gateway/app/internal/logic/walletflowslogic.go`

请求：`ParamWalletFlows`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `BizType` | `biz_type` | form | `int32` | 否 | — | — |
| `BizNo` | `biz_no` | form | `string` | 否 | — | — |
| `FromTs` | `from_ts` | form | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | form | `int64` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

响应：`WalletFlowsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletFlowsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamWalletBalance`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Currency` | `currency` | form | `string` | 否 | — | — |

### `WalletBalanceResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletBalanceData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `WalletChannelsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletChannelsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamWalletRechargeOpen`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `AmountMinor` | `amount_minor` | form | `int64` | 是 | — | — |
| `Currency` | `currency` | form | `string` | 否 | — | — |
| `Channel` | `channel` | form | `int32` | 否 | — | 0/1 均按 SANDBOX；其他值由服务拒绝 |
| `RequestId` | `request_id` | form | `string` | 是 | — | 幂等键 |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `WalletRechargeResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletRechargeData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamWalletRechargeNo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RechargeNo` | `recharge_no` | form | `string` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 否 | — | — |
| `TraceId` | `trace_id` | form | `string` | 否 | — | — |

### `ParamWalletRecharges`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `State` | `state` | form | `int32` | 否 | — | — |
| `FromTs` | `from_ts` | form | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | form | `int64` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `WalletRechargesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletRechargesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamWalletFlows`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `BizType` | `biz_type` | form | `int32` | 否 | — | — |
| `BizNo` | `biz_no` | form | `string` | 否 | — | — |
| `FromTs` | `from_ts` | form | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | form | `int64` | 否 | — | — |
| `Page` | `page` | form | `int32` | 是 | default=1 | — |
| `PageSize` | `page_size` | form | `int32` | 否 | — | — |

### `WalletFlowsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `WalletFlowsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `WalletBalanceData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Wallet` | `wallet` | json | `WalletBalance` | 是 | — | — |

### `WalletChannelsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SandboxOnly` | `sandbox_only` | json | `bool` | 是 | — | — |
| `Channels` | `channels` | json | `[]WalletChannelState` | 是 | — | — |
| `CurrencyDefault` | `currency_default` | json | `string` | 是 | — | — |

### `WalletRechargeData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `Recharge` | `recharge` | json | `WalletRecharge` | 是 | — | — |
| `Wallet` | `wallet` | json | `WalletBalance` | 是 | — | — |

### `WalletRechargesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Recharges` | `recharges` | json | `[]WalletRecharge` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `PageSize` | `page_size` | json | `int64` | 是 | — | — |

### `WalletFlowsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Flows` | `flows` | json | `[]WalletFlow` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `PageSize` | `page_size` | json | `int64` | 是 | — | — |

### `WalletBalance`

> 沙箱声明（必须让端上也能读到，而不是只写在文档里）：/wallet/channels 恒返回 / sandbox_only=true、real_money=false；充值单的「结算」不是「支付成功」， / 它是沙箱里唯一把钱记到账上的动作，不请求任何第三方支付。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `BalanceMinor` | `balance_minor` | json | `int64` | 是 | — | — |
| `FrozenMinor` | `frozen_minor` | json | `int64` | 是 | — | 本项目无预授权流程，恒为 0，别当可用余额 |
| `Currency` | `currency` | json | `string` | 是 | — | — |

### `WalletChannelState`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `Enabled` | `enabled` | json | `bool` | 是 | — | — |
| `RealMoney` | `real_money` | json | `bool` | 是 | — | 恒 false |
| `Note` | `note` | json | `string` | 是 | — | — |

### `WalletRecharge`

> WalletRecharge 充值单投影。settled_at=0 表示尚未入账。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RechargeNo` | `recharge_no` | json | `string` | 是 | — | — |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Channel` | `channel` | json | `int32` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | 1 待结算、2 已入账、3 已取消、4 失败 |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `SettledAt` | `settled_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `WalletFlow`

> WalletFlow 资金流水（本人可见）。正入负出。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `FlowId` | `flow_id` | json | `int64` | 是 | — | — |
| `BizType` | `biz_type` | json | `int32` | 是 | — | 1 充值、2 消费、3 退款、4 运营调整 |
| `BizNo` | `biz_no` | json | `string` | 是 | — | — |
| `DeltaMinor` | `delta_minor` | json | `int64` | 是 | — | — |
| `BalanceAfterMinor` | `balance_after_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/26-wallet.md -->
