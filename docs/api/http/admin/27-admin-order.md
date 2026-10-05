# 运营面 · `/admin/order`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| 商业化运营面：trade-order 域（订单状态机） | 免鉴权 | 4 |
| 商业化运营面：trade-order 域（订单状态机） | AdminPermission | 2 |

合计 **6** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## 商业化运营面：trade-order 域（订单状态机）（免鉴权，4 条）

> -------------------- trade-order 只读面（不进 routePermissions） --------------------
> 订单检索、详情、流转台账与卡单扫描都是读取：客服/排障每单要刷好几次，
> 挂判定只会把 operation 变成读放大瓶颈。/event/list 尤其重要——它是「谁在什么时候
> 凭什么把单推到哪一步」的证据链，只读不构成写能力。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/order/list` | 订单分页（有界窗口；mid=0 且无过滤条件时由服务拒绝全表扫） | `orderList` | `orderlistlogic.go` |
| POST | `/admin/order/get` | 订单详情（mid 非 0 时服务校验归属，越权按 not-found 回） | `orderGet` | `ordergetlogic.go` |
| POST | `/admin/order/event/list` | 状态流转台账（每次迁移一行，含操作者与理由） | `orderEventList` | `ordereventlistlogic.go` |
| POST | `/admin/order/stuck/list` | 卡单扫描（只读：核对哪些单停在 PAYING/PAID/FULFILLING 超时，后台不代为推进） | `orderStuckList` | `orderstucklistlogic.go` |

### POST `/admin/order/list` — 订单分页（有界窗口；mid=0 且无过滤条件时由服务拒绝全表扫）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/orderlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/orderlistlogic.go`

请求：`ParamOrderList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `BizType` | `biz_type` | json | `int32` | 否 | — | — |
| `PayMethod` | `pay_method` | json | `int32` | 否 | — | — |
| `OrderNo` | `order_no` | json | `string` | 否 | — | — |
| `PaymentNo` | `payment_no` | json | `string` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |
| `MaxWindowSeconds` | `max_window_seconds` | json | `int64` | 否 | — | — |

响应：`OrderListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/order/get` — 订单详情（mid 非 0 时服务校验归属，越权按 not-found 回）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/ordergethandler.go`
- 业务实现：`gateway/admin/internal/logic/ordergetlogic.go`

请求：`ParamOrderGet`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |

响应：`OrderGetResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/order/event/list` — 状态流转台账（每次迁移一行，含操作者与理由）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/ordereventlisthandler.go`
- 业务实现：`gateway/admin/internal/logic/ordereventlistlogic.go`

请求：`ParamOrderEventList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

响应：`OrderEventListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderEventListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/order/stuck/list` — 卡单扫描（只读：核对哪些单停在 PAYING/PAID/FULFILLING 超时，后台不代为推进）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/orderstucklisthandler.go`
- 业务实现：`gateway/admin/internal/logic/orderstucklistlogic.go`

请求：`ParamOrderStuckList`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OlderThanSeconds` | `older_than_seconds` | json | `int64` | 是 | — | — |
| `States` | `states` | json | `[]int32` | 否 | — | 空 = 服务默认卡单状态集合 |
| `Limit` | `limit` | json | `int64` | 否 | — | — |

响应：`OrderStuckListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderStuckListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 商业化运营面：trade-order 域（订单状态机）（AdminPermission，2 条）

> -------------------- trade-order 写面（受 AdminPermission 保护） --------------------
> 只有退款裁决两条，且 approve 与 reject 分开授权：
>   - approve 是真会动钱的动作（退款到余额）并连带回收权益，是本域最重的写口；
>   - reject 只是把单退回原状态、不动钱，但直接决定用户的申退诉求被否，
>     因此也要单独留 reason 与操作者，不给「能驳回」的人顺带获得「能退款」的能力。
> 状态机合法迁移集合、可退金额上限（按已消耗时长折算）、CAS 冲突全部由 trade-order 判定。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/order/refund/approve` | 审批通过退款（先退余额再回收权益；部分成功原样投影在 revoke_detail） | `order:refund` / `approve` | `orderRefundApprove` | `orderrefundapprovelogic.go` |
| POST | `/admin/order/refund/reject` | 驳回退款（订单回原状态，不动钱不动权益；reason 必填） | `order:refund` / `reject` | `orderRefundReject` | `orderrefundrejectlogic.go` |

### POST `/admin/order/refund/approve` — 审批通过退款（先退余额再回收权益；部分成功原样投影在 revoke_detail）

- 权限口径：AdminPermission · 权限点 `order:refund` / `approve`
- goctl 入口：`gateway/admin/internal/handler/orderrefundapprovehandler.go`
- 业务实现：`gateway/admin/internal/logic/orderrefundapprovelogic.go`

请求：`ParamOrderRefundApprove`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | 订单 CAS |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 审批人只能由会话渲染，必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id；重复提交回首次结论 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OrderRefundApproveResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderRefundApproveData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/order/refund/reject` — 驳回退款（订单回原状态，不动钱不动权益；reason 必填）

- 权限口径：AdminPermission · 权限点 `order:refund` / `reject`
- goctl 入口：`gateway/admin/internal/handler/orderrefundrejecthandler.go`
- 业务实现：`gateway/admin/internal/logic/orderrefundrejectlogic.go`

请求：`ParamOrderRefundReject`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`OrderRefundRejectResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderRefundRejectData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamOrderList`

> ParamOrderList 1:1 对应 ListOrdersReq。 / 无界扫描由服务拒绝：mid=0 且没有任何过滤条件时 ListOrders 不开放全表扫， / max_window_seconds=0 表示用服务侧默认窗口（不是「不限」）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `BizType` | `biz_type` | json | `int32` | 否 | — | — |
| `PayMethod` | `pay_method` | json | `int32` | 否 | — | — |
| `OrderNo` | `order_no` | json | `string` | 否 | — | — |
| `PaymentNo` | `payment_no` | json | `string` | 否 | — | — |
| `FromTs` | `from_ts` | json | `int64` | 否 | — | — |
| `ToTs` | `to_ts` | json | `int64` | 否 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |
| `MaxWindowSeconds` | `max_window_seconds` | json | `int64` | 否 | — | — |

### `OrderListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderGet`

> ParamOrderGet 1:1 对应 GetOrderReq；mid 非 0 时服务校验归属， / 越权按 not-found 语义回而不是 FORBIDDEN（不泄露「这单存在」这件事）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 否 | — | — |

### `OrderGetResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderGetData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderEventList`

> ParamOrderEventList 1:1 对应 ListOrderEventsReq：每次状态迁移一行，含操作者与理由。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `Page` | `page` | json | `int64` | 否 | — | — |
| `Size` | `size` | json | `int64` | 否 | — | — |

### `OrderEventListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderEventListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderStuckList`

> ParamOrderStuckList 1:1 对应 ListStuckOrdersReq（cron 的扫描面，后台只读它做核对， / 真正的推进由服务/cron 做；本路由不提供任何补偿动作）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OlderThanSeconds` | `older_than_seconds` | json | `int64` | 是 | — | — |
| `States` | `states` | json | `[]int32` | 否 | — | 空 = 服务默认卡单状态集合 |
| `Limit` | `limit` | json | `int64` | 否 | — | — |

### `OrderStuckListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderStuckListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderRefundApprove`

> ParamOrderRefundApprove 1:1 对应 ApproveRefundReq（operator/request_id/reason/expected_version）。 / 通过退款是**两条链路一次完成**：先调 payment 退回余额，再按业务回收权益 / （会员单调 membership.RevokeMembership 扣回未使用时长）。回收失败**不撤销已退的款**， / 订单停在 REFUND_APPROVED 并把差异写进 revoke_detail——网关原样投影这个部分成功， / 绝不把它美化成「全部成功」。reason 必填；expected_version 防审批与用户取消并发。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `ExpectedVersion` | `expected_version` | json | `int64` | 否 | — | 订单 CAS |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 审批人只能由会话渲染，必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id；重复提交回首次结论 |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OrderRefundApproveResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderRefundApproveData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamOrderRefundReject`

> ParamOrderRefundReject 1:1 对应 RejectRefundReq：驳回把订单退回原状态（FULFILLED）， / 不动钱、不动权益，但同样是用户可感知的结论，因此 reason 必填并写流转台账。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | 必须 > 0 |
| `IdempotencyKey` | `idempotency_key` | json | `string` | 是 | — | → request_id |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `OrderRefundRejectResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OrderRefundRejectData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `OrderListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]OrderItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `OrderGetData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Found` | `found` | json | `bool` | 是 | — | — |
| `Order` | `order` | json | `OrderItem` | 是 | — | — |

### `OrderEventListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]OrderEventItem` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Page` | `page` | json | `int64` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |

### `OrderStuckListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `List` | `list` | json | `[]OrderItem` | 是 | — | — |

### `OrderRefundApproveData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `RefundNo` | `refund_no` | json | `string` | 是 | — | — |
| `RevokeDetail` | `revoke_detail` | json | `string` | 是 | — | 含「已退但未回收」这种部分成功 |
| `Order` | `order` | json | `OrderItem` | 是 | — | — |

### `OrderRefundRejectData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |
| `Order` | `order` | json | `OrderItem` | 是 | — | — |

### `OrderItem`

> 契约来源 services/trade-order/rpc/tradeorder.proto（冻结）。 / 数据所有权（§5）：订单事实与履约指令**只属于本服务**——payment 只写资金台账、 / membership 只写会员授予，两者都不得回写订单状态；本域路由只是把这个状态机暴露给后台的 / 两个「人工裁决」位，不提供任何直接改状态的口子。 /  / 金额语义：订单金额一律**服务端重算**（下单时向 membership 取套餐价，客户端上报值只做一致性 / 校验，不作为扣款依据）；pay_method 只有 BALANCE 与 SANDBOX_CHANNEL 两档，都不产生真实资金移动。 / 需要真实渠道才成立的能力（渠道回调、原路退回、发票、对账单下载）不开接口，也不做后台路由。 / 退款是「退回余额」这一步才涉及钱，且必须由 approve 这一个动作驱动 trade-order→payment 链路。 /  / 刻意**不开**的路由（理由写在这里，不是漏实现）： /   - CreateOrder / ListMyOrders / CancelOrder / RequestRefund：买家的单只能由**买家自己** /     发起、取消、申退（归 gateway/app）。后台代下单等于伪造用户意愿，代取消等于替用户决定 /     不要自己付过钱的东西； /   - BindPayment：支付侧结论绑定/补偿推进，调用方是 cron 与对账链路（服务身份）。 /     从后台点它，等于让人手工决定「这笔钱算付了」，与 payment 台账的真值可能背离； /   - FulfillOrder：履约推进由服务/cron 驱动（PAID→FULFILLING→FULFILLED），失败时订单停在 /     FAILED 等人工或 cron 重试。后台直接触发履约会出现「运营按一次就多送几天」的重复发放， /     而幂等键的持有方应当是调度侧而不是按钮。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `BizType` | `biz_type` | json | `int32` | 是 | — | OrderBizType：1 会员 2 硬币包 |
| `PlanId` | `plan_id` | json | `int64` | 是 | — | — |
| `PlanCode` | `plan_code` | json | `string` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | 下单时快照的商品名（改价改名不影响历史单） |
| `Quantity` | `quantity` | json | `int32` | 是 | — | — |
| `DurationDays` | `duration_days` | json | `int32` | 是 | — | 会员单：本次总时长 |
| `CoinAmount` | `coin_amount` | json | `int32` | 是 | — | 硬币包：本次发放硬币数 |
| `UnitPriceMinor` | `unit_price_minor` | json | `int64` | 是 | — | 服务端重算的单价快照（分） |
| `AmountMinor` | `amount_minor` | json | `int64` | 是 | — | 应付=实付总额（分） |
| `RefundedMinor` | `refunded_minor` | json | `int64` | 是 | — | — |
| `Currency` | `currency` | json | `string` | 是 | — | — |
| `PayMethod` | `pay_method` | json | `int32` | 是 | — | 1 余额 2 沙箱渠道 |
| `State` | `state` | json | `int32` | 是 | — | OrderState：见 proto 的 11 个取值 |
| `FulfillState` | `fulfill_state` | json | `int32` | 是 | — | 履约是「该给的东西给到没有」，区别于订单状态 |
| `FulfillAttempts` | `fulfill_attempts` | json | `int32` | 是 | — | — |
| `FulfillDetail` | `fulfill_detail` | json | `string` | 是 | — | 最近一次失败摘要（不写堆栈、不写 PII） |
| `PaymentNo` | `payment_no` | json | `string` | 是 | — | — |
| `GrantRef` | `grant_ref` | json | `string` | 是 | — | 履约产物引用：会员 grant_id 或硬币 flow_id |
| `ExpireAt` | `expire_at` | json | `int64` | 是 | — | 未支付关单时间 |
| `ClientTraceId` | `client_trace_id` | json | `string` | 是 | — | — |
| `Platform` | `platform` | json | `int32` | 是 | — | 下单端（Android/iOS/Harmony/桌面/Web） |
| `RequestId` | `request_id` | json | `string` | 是 | — | 建单幂等键 |
| `Version` | `version` | json | `int64` | 是 | — | CAS |
| `CreatedAt` | `created_at` | json | `int64` | 是 | — | — |
| `UpdatedAt` | `updated_at` | json | `int64` | 是 | — | — |
| `PaidAt` | `paid_at` | json | `int64` | 是 | — | — |
| `FulfilledAt` | `fulfilled_at` | json | `int64` | 是 | — | — |
| `ClosedAt` | `closed_at` | json | `int64` | 是 | — | — |

### `OrderEventItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `EventId` | `event_id` | json | `int64` | 是 | — | — |
| `OrderNo` | `order_no` | json | `string` | 是 | — | — |
| `FromState` | `from_state` | json | `int32` | 是 | — | — |
| `ToState` | `to_state` | json | `int32` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | "user" / 运营工号 / "cron" / "system" |
| `Reason` | `reason` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/27-admin-order.md -->
