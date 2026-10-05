# trade-order

商业订单服务（会员单 / 硬币包单）：下单、支付结论绑定、履约驱动、取消与退款审批。
**全站唯一的建单入口，也是订单事实与履约指令的唯一所有者。**

- **拥有数据**：订单主表（金额快照、状态机位点、履约结果与尝试次数）、状态流转台账（库 `go_video_trade_order`，表前缀 `to_`）。
- **提供能力**：下单（服务端重算金额 + 沙箱内联受理）、订单详情/我的订单/运营面查询、状态流转台账、取消、绑款补偿、执行/重试履约、卡单扫描、退款申请/审批/驳回。
- **对外契约**：`services/trade-order/rpc/tradeorder.proto`（`tradeorder.v1`，12 个 rpc 方法），etcd 注册键 `tradeorder.v1.rpc`，监听 `0.0.0.0:8162`。
- **依赖**：MySQL（硬依赖，金额与状态必须回源）、三个下游 RPC —— `membership`（取价 + 发放/回收）、`payment`（受理 + 退款 + 关单）、`coin`（硬币包发放/扣回）。Redis 只做预留位，本轮不写任何订单缓存。
- **不依赖 MQ / outbox / 定时任务本体**（见 §10）。
- **owner**：后端-商业化小组（正式 owner 待登记到 `docs/service-catalog.md`）。

## 1. 数据所有权：payment / membership 不回写订单

订单状态只能由本服务写。下游各自只拥有自己的事实，被本服务同步 RPC 调用时才写自己的库：

| 事实 | 唯一所有者 | 本服务侧的形态 |
|---|---|---|
| 订单状态、金额快照、履约结论 | **trade-order**（`to_order`） | 直接读写 |
| 资金台账（支付单、退款单、余额） | payment（`pm_*`） | 只存 `payment_no` 引用 |
| 会员身份与授予台账 | membership（`mb_*`） | 只存 `grant_ref` 引用 |
| 硬币余额与流水 | coin（`cn_*`） | 只存 `grant_ref`（`coin_flow:<id>`） |

```text
trade-order ──RPC──▶ membership   GetPlan / GrantMembership / RevokeMembership
            ──RPC──▶ payment      CreatePayment / GetPayment / ClosePayment / RefundPayment / ListRefunds
            ──RPC──▶ coin         GrantCoin
```

- **反向不存在**：payment 结清一笔支付、membership 完成一次授予，都**不会**回写 `to_order.state`。
  订单推进只可能由本服务的 RPC 调用链（CreateOrder / BindPayment / FulfillOrder / ApproveRefund）触发。
- 跨表/跨服务只存主键引用，不建外键、不直连对方库表或 Redis key（AGENTS.md §5）。
- 其他服务要读订单结论只能走 `tradeorder.v1.rpc`，禁止直连 `go_video_trade_order`。

不变式（排障与核对依据）：

```text
to_order.state 的每一次变化  == to_order_event 里的一行（同事务提交）
to_order.refunded_minor      == payment 侧 request_id = refund_<order_no> 的退款金额
to_order.amount_minor        == unit_price_minor × quantity（服务端重算，与客户端上报值无关）
```

## 2. 状态机

`model.CanTransition` 是唯一口径，且 `TransitionTx` 在写库前二次把关（logic + model 双层护栏）。
所有推进都是 CAS：`WHERE order_no = ? AND state = ? AND version = ?`，未命中即 `ErrConcurrentUpdate`（调用方回读，绝不覆盖）。

```text
主干（happy path）：
CreateOrder ─▶ CREATED ─▶ PAYING ─▶ PAID ─▶ FULFILLING ─▶ FULFILLED
（沙箱下内联推进，每步单独 CAS + 台账；BindPayment 是 PAYING→PAID 的补偿入口）

退款支线：
PAID / FULFILLED ─RequestRefund─▶ REFUND_REQUESTED
REFUND_REQUESTED ─ApproveRefund（先退钱）─▶ REFUND_APPROVED ─ApproveRefund（回收成功）─▶ REFUNDED
REFUND_REQUESTED ─RejectRefund（回到申请前原状态）─▶ PAID / FULFILLED

出边表（与 model.CanTransition 逐字一致）：
  CREATED          → PAYING | CANCELLED
  PAYING           → PAID | CANCELLED
  PAID             → FULFILLING | FAILED | REFUND_REQUESTED
  FULFILLING       → FULFILLING（重试一轮） | FULFILLED | FAILED | PAID（补偿边，见注 3）
  FULFILLED        → REFUND_REQUESTED
  REFUND_REQUESTED → REFUND_APPROVED | PAID | FULFILLED（驳回回原状态，见注 2）
  REFUND_APPROVED  → REFUNDED
  CANCELLED / FAILED / REFUNDED / REFUND_REJECTED → 无出边（终态或人工态）
```

| 从 | 允许到 | 触发方 |
|---|---|---|
| CREATED | PAYING / CANCELLED | CreateOrder 内联 / CancelOrder、BindPayment |
| PAYING | PAID / CANCELLED | CreateOrder、BindPayment / CancelOrder |
| PAID | FULFILLING / FAILED / REFUND_REQUESTED | FulfillOrder / 履约失败 / RequestRefund |
| FULFILLING | FULFILLING / FULFILLED / FAILED / **PAID** | 同态=再试一轮（`fulfill_attempts+1`）；PAID 见下方注 3 |
| FULFILLED | REFUND_REQUESTED | RequestRefund |
| REFUND_REQUESTED | REFUND_APPROVED / PAID / FULFILLED | ApproveRefund / RejectRefund（回原状态） |
| REFUND_APPROVED | REFUNDED | ApproveRefund（回收成功后） |
| CANCELLED / FAILED / REFUNDED | ——（无出边） | 人工态，见 §10 |

实现注记（三处刻意收紧/放宽，都不是随手改的）：

1. **同态迁移只放开 `FULFILLING → FULFILLING`**：它代表「再试一轮履约」，必须落一行台账并让 `fulfill_attempts` 可见增长，否则重试历史不可审计。其余同态由 logic 短路成 `duplicated=true`，不进入状态机。
2. **`REFUND_REQUESTED → PAID` 是按契约意图补的边**：proto 注释只写了「驳回退款回到 FULFILLED」，但退款也可以从 PAID 发起（履约没成功的单同样能退），所以「回到申请前原状态」必然包含 PAID。原状态由台账反查（`FindLastTransitionTo`），不猜。
3. **`FULFILLING → PAID` 是一条补偿边**：CAS 到 FULFILLING 后进程崩溃，订单会永远卡在 FULFILLING（下游幂等键已发出但本地状态不知道）。BindPayment/人工修复可以把它退回 PAID 再走一遍完整履约。这条边超出了 proto 注释的字面集合，属**契约缺陷的临时绕行**，已记入 §10。

## 3. 表清单（`deploy/migrations/trade-order/000001_create_trade_order_tables.sql`）

| 表 | 作用 | 关键约束/索引 |
|---|---|---|
| `to_order` | 订单事实：金额快照、状态、`version`、履约结果与尝试次数 | `uniq_order_no`、`uniq_request_id`（建单幂等最终防线）、`idx_mid_state_created`（我的订单）、`idx_state_updated`（卡单扫描）、`idx_state_created`（运营面窗口扫描）、`idx_payment_no`（按支付单反查，**普通索引**：未支付行是 `''`，唯一索引会互相冲突） |
| `to_order_event` | 状态流转台账，与主表更新**同事务**提交；`RejectRefund` 反查原状态的依据 | `idx_order_ctime`、`idx_order_request`；只 INSERT，禁止 UPDATE/DELETE；同态行（from=to）用于「已取消单收到绑款」「款已退未回收」这类必须留证的拒绝 |

参与唯一性/精确匹配的列（`order_no`、`request_id`、`payment_no`、`plan_code`、`grant_ref`）列级 `utf8mb4_bin`，其余文本列表级 `utf8mb4_unicode_ci`：唯一键若走大小写折叠，两个仅大小写不同的 `request_id` 会被判重复，其中一次**真实建单会被静默丢弃**。

## 4. 金额与档位：一律服务端重算（CreateOrder）

客户端上报值只用于对撞，**绝不参与扣款**。取价与档位校验全部走 `membership.GetPlan`：

```text
1) 套餐必须 state = ON_SALE，且 platforms 含下单端（不可见/非在售 → 不建单）
2) 档位匹配：MEMBERSHIP 单要求 vip_type ∈ {PREMIUM, PREMIUM_PLUS} 且 duration_days > 0
             COIN_PACK  单要求每份硬币枚数 > 0（取自 PlanInfo.unit_count，见 §10 缺口 3）
3) unit_price    = prom_price_minor > 0 ? prom_price_minor : price_minor   （促销价优先）
   amount        = unit_price × quantity                                   （checkedMul 溢出保护）
   duration_days = plan.duration_days × plan.unit_count × quantity          （会员单）
   coin_amount   = plan.unit_count × quantity                              （硬币包）
4) 客户端 amount_minor != 0 且 != 服务端重算值
   → InvalidArgument，错误文本给出 server_amount_minor / client_amount_minor / unit_price_minor / quantity
```

`quantity <= 0` 视为 1；超过 `TradeOrder.MaxQuantityPerOrder` 按上限裁剪（proto 注释「上限由服务侧配置裁剪」），裁剪后金额对撞会把「按 11 份的钱买 10 份」挡掉。
`plan_id`/`plan_code`/`title`/`currency` 都取套餐的权威值（不是请求值），下单后套餐改价改名不影响历史单履约与退款。

## 5. 下游依赖与未配置行为（分级显式失败）

三个客户端都是 optional：`Endpoints`/`Target`/`Etcd.Hosts` 全空时 svc **不构造客户端**（字段为 nil），
启动时会打一条 `DownstreamNotes` 说明哪个能力在本环境必然失败。绝不连一个不存在的地址、绝不静默降级成假成功：

| 未配置 | 影响的能力 | 运行时报错 |
|---|---|---|
| `MembershipRPC` | CreateOrder 取价（唯一价格来源）、会员单履约与回收 | `trade-order: membership rpc not configured` |
| `PaymentRPC` | 受理支付、退款到余额、关单核对 | `trade-order: payment rpc not configured` → **直接不建单** |
| `CoinRPC` | 硬币包发放与扣回 | `trade-order: coin rpc not configured`（订单停在 FULFILLING/FAILED） |

关键判定：**没有 payment 就不建单**。本服务不持有资金台账，落一张「看起来已支付」或「永远付不了」的订单比拒绝下单危险得多。
履约失败则保持 `FULFILLING`/`FAILED` 并把摘要写进 `fulfill_detail`，绝不返回 `FULFILLED`。

## 6. 幂等口径

| 入口 | 幂等锚点 | 重放行为 |
|---|---|---|
| CreateOrder | `to_order.uniq_request_id`（客户端 `request_id`） | 命中唯一索引 → 回查首次订单；并**重新驱动受理**（`driveAccept`），回复 `duplicated=true` |
| CancelOrder / BindPayment / RequestRefund / RejectRefund | `to_order_event (order_no, request_id, to_state)` 台账查询 + 状态短路 | `duplicated=true`，不再动状态、不再调下游 |
| ApproveRefund | **状态本身**（`REFUND_APPROVED` 语义 = 款已退、回收未完成） | 必须可重入：若被 request_id 短路成 duplicated，权益就永远收不回来 |
| 所有下游调用 | 键由 `order_no` **派生**：`pay_ / grant_ / coinpack_ / revoke_ / refund_ / close_ / fulfill_ / fulfillfail_` + `_` + `order_no` | 重试退的是同一笔钱、发的是同一份权益；下游回 `duplicated=true` 一律算成功 |
| 状态推进 | CAS `WHERE order_no=? AND state=? AND version=?` | 未命中 → `ErrConcurrentUpdate`（Aborted），调用方回读重试而不是覆盖 |

透传客户端 `request_id` 给下游是本轮明确避免的失效模式：一次网络抖动就会变成「重复发放会员」或「重复退款」。

## 7. RPC 方法判定口径（12 个）

| 方法 | 口径要点 |
|---|---|
| `CreateOrder` | §4 全部校验 + 内联推进 `CREATED→PAYING→PAID→FULFILLING→FULFILLED`，每步单独 CAS + 台账；受理失败回 `accepted=false` + `reject_reason`（订单已落库，可稍后由 cron `FulfillOrder` 补） |
| `GetOrder` | 带 `mid` 时校验归属；**不存在与不是他的单都回 `found=false`**（不暴露订单号是否存在）。口径缺口见 §10 注 12：实现只把 `mid>0` 当「要校验」，`mid<0` 与 `mid=0` 一样走内部读 |
| `ListMyOrders` | 强制 `mid`（`mid<=0` → `ErrInvalidMid`，且发生在任何取数之前）；`page/size` 有界（`MaxPageSize`，越界拒绝不裁剪）；过滤只带 `mid/state/biz_type` 三位，不接受订单号、支付单号与时间窗；空列表返回非 nil |
| `ListOrders` | 运营面：跨用户（`mid=0`）要给 `from_ts/to_ts`，窗口 ≤ `MaxListWindowSeconds`，`max_window_seconds` 只收紧不放宽；越界拒绝不裁剪。**「至少一个过滤条件 + 完整时间窗」是文档口径，实现是二者缺一即放行**（§10 注 13），窗口本身也能被配置缺项抹掉（注 14） |
| `ListOrderEvents` | 台账按 `ctime ASC, event_id ASC` 正序（状态轨迹必须可读成正史）；`request_id` 不出服务（`OrderEventInfo` 无此位）。**契约里没有 mid 位，本方法不判归属**（§10 注 15） |
| `CancelOrder` | 只允许 CREATED/PAYING；主体二选一（`mid` 本人 / `operator` 运营）；`reason` 必填；已绑 `payment_no` 直接拒绝；PAYING 单先向 payment 核对结论（PENDING 先关单，已 PAID 拒绝取消，**查不到结论不取消**） |
| `BindPayment` | `payment_no` 非空 + 金额与订单一致才推进；已 PAID 及之后 `duplicated=true`；对 CANCELLED 单绑款 → 拒绝 + 同态台账留证 + Error 日志（资金事故信号） |
| `FulfillOrder` | 只受理 PAID/FULFILLING；事务内 CAS 到 FULFILLING + `attempts+1` + 台账，**提交后**才调下游；失败 CAS 到 FAILED 写摘要；受 `FulfillMaxAttempts` 与 `MinSecondsBetweenFulfillRetry` 双重限流 |
| `ListStuckOrders` | 只读扫描（不顺手改状态，`TestListStuckOrdersScansWithoutAnyCompensation` 逐列钉住）：卡住 = 状态在扫描集合内 **且** `updated_at` 严格早于 `now-older_than`（等于阈值那一秒不算，判据在 `model/to_order.go:591-592`）；`older_than_seconds<=0` 退化为 `OrderExpireSeconds`，配置也缺省时兜底 1800，绝不退化成全表；`states` 默认 PAYING/PAID/FULFILLING 三档，显式传入时只接受 stuckAllowedStates 七档（`FULFILLED` 虽非终态但也被拒 —— 它停下来是正常态，见注 16）；`limit` 越 `StuckScanMaxLimit` 拒绝不裁剪，`limit<=0` 用上限；结果 `updated_at ASC`，空结果非 nil。契约无分页（注 16） |
| `RequestRefund` | 只受理 PAID/FULFILLED；主体二选一 + `reason` 必填；`amount_minor=0` 才受理（**不支持部分退款**，§10 缺口 2）；只登记申请，不动钱不动权益 |
| `ApproveRefund` | `operator/reason/request_id/expected_version` 全必填；顺序=**先退钱再回收**；回收失败停在 `REFUND_APPROVED` + `fulfill_detail` + 同态台账 + `revoke_detail`，绝不标 `REFUNDED` |
| `RejectRefund` | 只驳回 `REFUND_REQUESTED`（`REFUND_APPROVED`/`REFUNDED` 款已退一律拒绝）；目标状态从台账反查「申请前原状态」，反查不到 → `ErrRefundOriginUnknown`（不猜 FULFILLED） |

`refunded_minor` 只在 payment 确认退款成功后、与状态推进同一事务里增量累加 —— 这是「重试不重复退钱」的本地依据。

## 8. 配置项（`etc/tradeorder.v1.yaml`，键与 `internal/config/config.go` 一一对应，由 `internal/config/config_load_test.go` 钉住）

| 键 | 默认 | 作用 |
|---|---|---|
| `CacheRedis` | — | 读侧缓存预留位（**不能叫 `Redis`**：`zrpc.RpcServerConf` 已内嵌同名字段，会让 `conf.Load` 报 `conflict key redis`）。本轮所有读路径都回源 MySQL |
| `DataSource` | — | `go_video_trade_order` DSN；**禁止 `clientFoundRows=true`**（CAS 靠 `RowsAffected` 判命中，开了会把并发抢先误判成推进成功） |
| `MembershipRPC` / `PaymentRPC` / `CoinRPC` | optional | 三个下游；整段缺省 = 不构造客户端 = 对应能力运行时显式失败（§5） |
| `TradeOrder.MaxQuantityPerOrder` | 10 | 单笔份数上限（裁剪后与上报金额对撞） |
| `TradeOrder.OrderExpireSeconds` | 1800 | 未支付关单时间，写入 `expire_at` 并透传给 `payment.CreatePayment.expire_at`；也是 `ListStuckOrders` 的默认阈值 |
| `TradeOrder.StuckScanMaxLimit` | 200 | 卡单扫描单次上限 |
| `TradeOrder.MaxListWindowSeconds` | 7776000 | 运营面跨用户最大时间窗（90 天）。**漏键安全（`conf.Load` 会补 `default=7776000`），显式写 0 或负数不安全**：logic 拿到非正值就直接跳过上限，见 §10 注 14 |
| `TradeOrder.MaxPageSize` | 100 | 分页 size 上限，越界拒绝 |
| `TradeOrder.FulfillMaxAttempts` | 5 | 履约累计尝试上限，超出必须人工介入 |
| `TradeOrder.MinSecondsBetweenFulfillRetry` | 30 | 两次履约尝试最小间隔（防 cron 打爆下游） |
| `TradeOrder.DefaultCurrency` | CNY | 套餐未声明币种时的币种；金额一律最小货币单位（分），全链路不用浮点 |

## 9. 运行

```powershell
# 1) 迁移（见 docs/commands.md §8）
./scripts/migrate.ps1 -Action up -Service trade-order

# 2) 契约变更后重新生成 goctl 代码（.proto 是唯一来源，禁止手改生成物）
./scripts/gen.ps1 -Service trade-order

# 3) 启动
go run ./services/trade-order -f services/trade-order/etc/tradeorder.v1.yaml

# 4) 本服务门禁
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
go build ./services/trade-order/... && go vet ./services/trade-order/... && gofmt -l services/trade-order
go test -p 1 -count=1 ./services/trade-order/...   # -p 1 的必要性见 §11.6
```

## 10. 已知缺口（尚未实现，禁止当成已交付）

1. **无 MQ / 无 outbox：履约靠同步 RPC + `FulfillOrder` 重试**。
   状态迁移只保证「主表 + 台账」同事务，没有事件外发，也没有待办表。
   下游抖动导致履约停在 FULFILLING/FAILED 的单，只能由 cron 调 `ListStuckOrders` 捞出来、再调 `FulfillOrder` 重试；
   `FAILED` 与 `REFUND_APPROVED` 这两类未完成态同样依赖这个外部驱动循环。
   **接线时必须补 `api/events` schema + outbox 表 + 按 `event_id` 去重的消费者**，而不是在事务里发 MQ。
2. **不支持部分退款**：proto 注释要求「会员单按未消耗时长折算、硬币包按未消耗硬币折算」，
   但 membership 没有「按订单号查剩余时长」的接口（`GetMembership` 只有合并后的整体区间，多次授予无法归因到单），
   coin 只有账户余额（分不清哪枚硬币来自哪一单）。**拿不到消耗事实就不给折算额度** ——
   `RequestRefund` 只接受 `amount_minor=0`（全额），任何自报部分金额回 `ErrRefundAmountInvalid`。
   配套口径：`ApproveRefund` 用 `RevokeMembership(ClearRemaining=true)`（全额退款 ⇒ 剩余时长作废），
   而不是「按天扣回」（会把别人授予的区间也扣掉）；硬币包用 `GrantCoin(delta=-coin_amount)` 整包扣回，
   用户已把硬币投出去时 coin 会拒绝扣成负数，订单就停在 `REFUND_APPROVED` 等人工处理（钱已退、权益收不回）。
   正解是给两个下游各加一个 per-order consumption 查询，或给订单加消耗快照列（要改 proto）。
3. **硬币包没有 SKU 档位口径**：`membership.PlanInfo` 没有「每份硬币枚数」字段，本轮把 `unit_count` 当约定枚数用；
   也没有「套餐属于哪种 biz_type」的判别列，`biz_type` 与套餐的匹配退化成「`unit_count > 0` 才算硬币包」，
   会员包与硬币包若被配成同一档位会串档。需要 membership 增列（`kind` / `coin_amount_per_unit`）。
4. **订单上没有 `vip_type` 快照**：`GrantMembership`/`RevokeMembership` 必须带 `VipType`，
   但 `to_order` 与 `OrderInfo` 都没有这一列，因此履约/退款时**回查 `membership.GetPlan`**。
   若期间套餐改档，就会按新档发放/回收。正解是给 `to_order` 增列 `vip_type` 快照（需改 proto + 迁移）。
5. **`FAILED` 是死胡同**：proto 的状态机注释里 `FAILED` 无出边，而 `FulfillOrder` 只受理 PAID/FULFILLING，
   所以耗尽重试的履约失败单**无法自助恢复**，`ListStuckOrders` 只能把它暴露给人工。
   需要契约补 `FAILED → FULFILLING`（或允许 `FulfillOrder` 受理 FAILED）。
6. **`REFUND_REJECTED(11)` 是枚举里的孤儿**：文件头迁移表既没给它入边也没给外边。
   `model.CanTransition` 因此**刻意不给** `REFUND_REQUESTED → REFUND_REJECTED` 这条边（一旦走到就是死胡同）：
   `RejectRefund` 按注释意图实现为「回到申请前原状态」，驳回事实只在 `to_order_event` 留痕，
   该值在主表里永不出现（`model.StateRefundRejected` 只用于枚举上界与 `StateName`）。
   若运营面要按状态筛「被驳回的单」，只能靠台账 `reason` 前缀，需要契约决策。
7. **无真实渠道 ⇒ 无对账**：`pay_method` 只有 `BALANCE` 与 `SANDBOX_CHANNEL`，都不产生真实资金移动。
   因此没有渠道回调、没有原路退回、没有对账单/差异单能力（proto 明确不开这些接口）。
   「订单 ↔ payment」的核对只能靠 `idx_payment_no` 与派生幂等键人工比对，不是自动对账。
8. **补偿边 `FULFILLING → PAID` 属实现自加**（§2 注 3）：绕的是「CAS 后崩溃」这个契约没覆盖的洞，
   正式解法是 `FulfillOrder` 支持幂等重入或引入 outbox。
9. **单测覆盖的是判定层，不是数据库语义**：`internal/logic` 为 12 个用例文件 + `fakes_test.go`
   内存替身（不含测试函数），共 **168 个顶层用例 / 72 个 `t.Run` 子用例**，`t.Skip` 计数为 0；
   12 个 `New<X>Logic` 构造器**全部**有用例引用（补齐之前 GetOrder/ListMyOrders/ListOrderEvents/
   ListOrders/ListStuckOrders 五个为零引用）。逐文件明细、替身口径与覆盖边界见 §11。
   读侧用例统一用两条缝把口径钉住：`assertCalls` 断言**触库序列**（守卫是否发生在取数之前），
   `markWrites`/`assertNoWritesAfter` 断言**写副作用增量为零**（读路径不许顺手补偿）。
   `model/migration_parity_test.go` 另有 27 个门禁函数（子用例 12）把 `deploy/migrations/trade-order/000001_*.sql`
   与 struct tag / SELECT 列常量 / INSERT 实参 / 枚举逐值 / 列宽常量逐条对齐（不连库，纯解析 + 反射）。
   仍未验证的是：真驱动下 `RowsAffected` 与唯一索引、`uniq_order_no`/`uniq_request_id` 的撞键行为，
   以及并发双扣/重放的窗口 —— 需要容器化 MySQL 集成测试。
10. **`ListStuckOrders` 不含 expire 关单动作**：`expire_at` 已写入但没有 cron 消费它把超时未付单关成 CANCELLED
    （`ClosePayment` + CAS）。接线时在 `services/cron` 注册任务，走 `CancelOrder` 而不是绕过状态机直接改库。
11. **库侧无兜底约束**（2026-09-22 漂移门禁复盘）：本迁移**没有任何 `CHECK`**（payment 有 4 条），
    所以 `refunded_minor <= amount_minor`、`amount_minor > 0` 只由 model 的条件 UPDATE 守着，绕过 model 的修数能写出负毛利；
    三个金额列（`amount_minor`/`unit_price_minor`/`refunded_minor`）带 `DEFAULT 0`，INSERT 漏传会静默记 0 而不是报错；
    `paid_at`/`fulfilled_at`/`closed_at` 三列注释未写「Unix 秒」而 `created_at`/`updated_at`/`expire_at` 写了
    （门禁只对参与比较的列设口径，故未发红）；`plan_code` 声明了 `utf8mb4_bin` 却没有索引，当前也没有按它过滤的查询 ——
    将来运营要按套餐筛单时必须先补索引，否则是全表扫。

以下 12–16 由读侧用例轮（GetOrder / ListMyOrders / ListOrderEvents / ListOrders / ListStuckOrders）登记。
**五条都没有改生产代码**：12/13/14 是实现缺陷，用例注释里带 `缺陷：<file:line>` 并按现状断言；
15/16 是契约与文档口径缺口（`ListOrderEvents` 无 mid 位、`ListStuckOrders` 无分页），
同样有哨兵用例钉住当前形状。改动实现或 proto 时必须同步翻转这些期望值。
12/13/15 属鉴权与审计类，14 属配置类，单列说明。

12. **`GetOrder` 的归属闸门只认 `mid>0`（鉴权 + 审计）**：`internal/logic/getorderlogic.go:44` 写的是
    `if mid := in.GetMid(); mid > 0 && order.Mid != mid`，而契约注释 `rpc/tradeorder.proto:141`
    与 `getorderlogic.go:30` 说的都是「**非 0** 时校验归属」—— 于是 `mid<0` 落进「不校验」分支，
    与 `mid=0`（服务内部口径）混为一路：直连 gRPC 的调用方传 `mid=-1` 即可读任意用户的订单本体
    （金额、`payment_no`、`grant_ref`、`client_trace_id` 全在 `OrderInfo` 里），
    而且不走 `getorderlogic.go:45` 那行越权告警日志，审计里不留痕。同包 `ListMyOrders` 对 `mid<=0`
    是直接 `ErrInvalidMid`（`listmyorderslogic.go:34-36`），两个入口口径不一致。
    当前爆炸半径有限：`gateway/app/internal/logic/conv_commerce.go:38-43`（`requireMid` 拒 `mid<=0`）与
    `gateway/admin/internal/logic/ordergetlogic.go:53`（`orderNonNeg` 拒负数）各挡了一道 —— 但那是**网关的巧合**，
    不是服务侧闸门，网关一改就成真。修法：`mid==0` 才走内部读，`mid<0` 回 `ErrInvalidMid`。
    哨兵用例：`TestGetOrderNegativeMidCurrentlySkipsOwnershipCheck`。
13. **`ListOrders` 的「过滤条件 + 时间窗」实现是 OR，不是文档里的 AND**：`listorderslogic.go:64` 判
    `mid<=0 && !filtered && !hasWindow`（两者都没有才 `ErrFilterRequired`），所以「只给一个窗口、零过滤条件」
    的跨用户查询被放行，在 `to_order` 上退化成 `created_at` 区间扫（回表全用户订单）。
    这与 `listorderslogic.go:34` 的函数头与本表 §7 的原描述相反，也与本方法「拒绝而不是裁剪」的自述相反；
    叠加后台只读组不挂权限点（`gateway/admin/internal/handler/routes.go:1903-1929`，一个管理员会话即可调用），
    它既是锁风险也是全量订单读口。修法：跨用户且非点位查询时 `!filtered` 直接 `ErrFilterRequired`，
    或把文档改成实际口径并另设更低的窗口上限。哨兵用例：`TestListOrdersWindowOnlyCrossUserScanIsAccepted`。
14. **`MaxListWindowSeconds` 被填成非正值等于取消上限（配置类）**：`listorderslogic.go:76` 判
    `if maxWindow > 0 && toTs-fromTs > maxWindow`，`maxWindow` 直接取配置（`:72`），
    所以值 ≤ 0 时整个上限被跳过（漏键本身安全 —— `config.go:56` 的 `default=7776000` 会由 `conf.Load` 补上；
    出事的是显式写 `0`/负数的 yaml、以及任何不走 `conf.Load` 构造 `Config` 的路径）。
    `:73` 的「调用方只能收紧」写作 `want>0 && want<maxWindow`，`maxWindow=0` 时连调用方自报的
    `max_window_seconds` 也一并失效（0 不比 0 小）—— 唯一的界交给了配置文件。
    同包 `paginate` 对 `MaxPageSize<=0` 兜底 100（`helpers.go:106-108`）、`ListStuckOrders` 对
    `StuckScanMaxLimit<=0` 兜底 200（`liststuckorderslogic.go:91-92`），只有这里没兜底，口径不一致。
    修法：`maxWindow<=0` 回落到 `config.go:56` 的同一常量或显式报「配置非法」，并给 config 加载补一条非负校验。
    哨兵用例：`TestListOrdersZeroMaxListWindowUnbindsCrossUserScan`。
15. **`ListOrderEvents` 契约里没有 `mid` 位 ⇒ 归属只能由调用方预检**：`rpc/tradeorder.proto:186-190` 的
    `ListOrderEventsReq` 只有 `order_no/page/size`，本方法物理上无法判归属（`listordereventslogic.go:32` 也这么写）。
    现状是终端面由 `gateway/app/internal/logic/ordereventslogic.go:55-60` 先 `GetOrder(order_no, mid)` 预检；
    后台面 `gateway/admin/internal/logic/ordereventlistlogic.go` 直读，任何订单号的台账都对管理员会话敞开。
    同时 `listordereventslogic.go:33`「台账行里没有 PII（operator/reason 都是写入侧脱敏过的摘要）」这句**比实现强**：
    写入侧只做 `truncate(reason, 500)`（`helpers.go:398`）与折叠空白的 `sanitize`（`helpers.go:75`），
    没有任何脱敏；用户在 `RequestRefund.reason` 里写了什么，台账就原样存什么。
    修法（需改 proto）：给 `ListOrderEventsReq` 加 `mid`，非 0 时按 `to_order.mid` 做 join 级归属过滤；
    或把台账 reason 改为白名单模板 + 独立的用户备注列。哨兵用例：`TestListOrderEventsDoesNotCheckOwnership`。
16. **`ListStuckOrders` 无分页、且 `FULFILLED` 不在可扫集合内**：`ListStuckOrdersReq` 只有
    `older_than_seconds/states/limit`，`Reply` 只有 `orders`（无 `total`、无游标），
    所以一次巡检最多拿 `StuckScanMaxLimit` 条、超出部分无解 —— 网关侧也明确不伪造 page/size/total
    （`gateway/admin/internal/logic/orderstucklistlogic.go` 的契约缺口段）。
    修法：proto 加 `total` 与 `updated_at` 游标（`updated_at ASC` 已是天然游标列）。
    另一侧：`stuckAllowedStates`（`liststuckorderslogic.go:39-47`）允许 CREATED/PAYING/PAID/FULFILLING/
    FAILED/REFUND_REQUESTED/REFUND_APPROVED 七档，**独独排除 `FULFILLED`** —— 它不是终态
    （`model.CanTransition` 给了 `FULFILLED → REFUND_REQUESTED`，`to_order.go:151-152`），
    但「履约完成后再没动静」是正常态而不是卡单，扫它只会让 cron 重试已结案件；
    卡在 REFUND_REQUESTED 过久的单因此也依赖 `RefundRequested` 这一档显式扫描（默认集合不含，见 §7）。
    哨兵用例：`TestListStuckOrdersDefaultStateSetIsExactlyThreeInFlightStates`、
    `TestListStuckOrdersRejectsClosedStateWithItsOwnReason`。

## 11. 测试覆盖

离线单测（纯 Go 替身 + 假下游 RPC client，不连 MySQL/Redis/etcd/MQ，也不起真 gRPC server）。
数字来自 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 的实测导出，
格式 `顶层/子用例`。规模：`internal/logic` 13 个文件 `168/72`（12 个用例文件 + `fakes_test.go` `0/0`）、
`model` 1 个文件 `27/12`、`internal/config` 1 个文件 `4/0`；`t.Skip` 0 条。

### 11.1 `internal/logic` 用例清单

**A. 写侧与资金推进（订单状态机 + 台账 + 下游）**

| 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- |
| `createorder_test.go` | 26/14 | 金额只信 `membership.GetPlan` 的重算值，客户端上报值仅用于防改价校验、绝不参与扣款；建单主表行与出生台账同事务（台账失败回滚整张单）；内联推进 `CREATED→PAYING→PAID→FULFILLING→FULFILLED` 每步一条台账、不许跳步；下游缺失或失败只如实报告——`TestCreateOrderPaymentNotSettledDoesNotFakePaid`、`TestCreateOrderFulfillmentFailureReportsPaidButPending`、`TestCreateOrderPaymentAmountDriftStopsBeforeFulfillment` 三向锁死「伪造已支付/已发放」；重放已 FULFILLED 单不再扣款、卡在 PAYING 的单在重放时被重驱动、参数变更当前判不出（按现状钉）；硬币包档位闸门、无 coin client 时标 `FAILED`；标题/`plan_code` 快照、币种兜底链、超长 `request_id` 截断到列宽 |
| `bindpayment_test.go` | 9/3 | 补偿推进口：`PAYING→PAID`、`CREATED` 单补写 `PAYING`；PAID 及之后一律幂等；金额不一致直接拒绝（不固化错账）；已取消订单收到绑款是资金事故信号——拒绝推进但**仍写同态台账留证**（`TestBindPaymentOnCancelledOrderLeavesEvidence`）；CAS 未命中回 `duplicated` 带最新状态；`operator` 截断与上限 |
| `cancelorder_test.go` | 9/14 | 只允许取消「钱还没动」的单：`PAYING` 取消前必须先向 payment 核对结论（钱可能已在对侧扣掉），已绑款、已 PAID 及之后一律拒绝；越权与不存在走**同一个 not-found 出口**，不泄露订单号是否存在；`reason` 不外泄；并发未命中重读而非覆盖；入参校验矩阵 |
| `fulfillorder_test.go` | 15/8 | 本服务最关键的一条顺序：事务内 CAS 到 `FULFILLING`（`attempts+1` + 台账）→ **提交后**才调下游 → 成功再 CAS 到 `FULFILLED`；已 `FULFILLED` 幂等重放；`FulfillMaxAttempts`/最小重试间隔闸门；下游回 `duplicated` 记成功；下游失败标 `FAILED` 并如实返回错误；缺下游 client 显式失败（不静默跳过）；档位守卫发生在调下游之前；台账失败回滚 `FULFILLING` 步；`TestFulfillOrderFinalStepRollbackLeavesAuditableGap` 钉住「末步回滚留下可审计缺口」的现状；默认用 `order_no` 派生幂等键；`operator` 取调用方；`runFulfill` 被两个入口共用同一实现 |
| `refund_test.go` | 27/28 | `RequestRefund` 只登记申请、一分钱都不动，只接受 `amount_minor=0`（部分退款在本沙箱不支持，任何自报金额回 `ErrRefundAmountInvalid`），状态/在途规则、必须带 payment 引用且仅余额支付可退；`ApproveRefund` 的顺序是「先退钱 → 记账 → 再回收」，回收失败必须停在 `REFUND_APPROVED` 绝不标 `REFUNDED`，重入而非重复发放，只记 payment 实际移动额，已部分记账时跳过重复退款，硬币包整包扣回、硬币已投出则停下等人工，CAS 未命中发生在钱已动之后（现状钉住），台账失败回滚退款记账；`RejectRefund` 实现为「回到申请前原状态」，`REFUND_REJECTED` 不进入主表（那条边是死胡同），驳回事实只留 `to_order_event` |

**B. 读侧（守卫次序 / 归属 / 分页口径）**

| 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- |
| `getorder_test.go` | 8/0 | `order_no` 必填且裁剪发生在触库之前；带 `mid` 时校验归属，「不存在」与「不是你的」**应答形状相同**（防订单号枚举，AGENTS.md §5 越权读）；`mid=0` 是服务内部口径、不做归属过滤；库存故障绝不降级成 `found=false`；`TestGetOrderNegativeMidCurrentlySkipsOwnershipCheck` 把负 `mid` 落进「不校验」分支的现状钉成哨兵（缺口 §10 第 12 条） |
| `listmyorders_test.go` | 10/0 | 终端面 `mid` 强制必填、不存在跨用户路径；**不接受**时间窗/订单号/支付单号等运营过滤位（可填就是越权读别人的通道）；`page/size` 越界拒绝不裁剪；过滤必须在 SQL 侧生效（`total` 是过滤后的总数，否则翻页翻空）；空列表回非 nil 切片；故障不伪装成空列表 |
| `listorders_test.go` | 12/0 | 跨用户必须给点位查询或完整时间窗且窗口不超 `MaxListWindowSeconds`，调用方 `max_window_seconds` 只能收紧；「拒绝而不是裁剪」，且所有守卫都发生在触库之前（写成先查后判就已经发生锁风险）；两条缺陷哨兵：`TestListOrdersWindowOnlyCrossUserScanIsAccepted`（窗口+条件实际是 OR，§10 第 13 条）、`TestListOrdersZeroMaxListWindowUnbindsCrossUserScan`（配置填 0 等于取消上限，§10 第 14 条） |
| `listorderevents_test.go` | 7/0 | 台账按 `ctime ASC, event_id ASC` 正序返回、logic 不重排不反转不错位翻页（排序事实源在 `model/to_order_event.go`，替身照抄）；`order_no` 守卫先于分页换算与触库；契约无 `mid` 位 ⇒ 本方法物理上无法判归属（`TestListOrderEventsDoesNotCheckOwnership` 钉现状，§10 第 15 条）；`request_id` 是内部幂等键、不出现在应答里 |
| `liststuckorders_test.go` | 13/0 | 纯扫描：一步都不许写（用触库序列 + 写基线双重钉住，沙箱下「顺手补偿」等于伪造已补偿）；「卡住」＝状态 ∈ 扫描集合**且** `updated_at` 严格早于 cutoff（阈值那一秒不算卡住，`<` 写成 `<=` 必红）；cutoff 只由 `older_than_seconds`/`OrderExpireSeconds`/硬兜底 1800 决定，`older_than<=0` 不得退化成全表扫；状态守卫在 limit 守卫之前、更在触库之前；超上限拒绝不裁剪（裁剪会让 cron 漏单）；`updated_at ASC` 最老先修；默认状态集合恰为三个在途态、`FULFILLED` 被排除（§10 第 16 条）；空结果非 nil |

**C. 状态机与纯函数（跨方法的宪法与落库文本形状）**

| 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- |
| `statemachine_test.go` | 13/1 | 迁移矩阵的期望边集是**手抄自 `rpc/tradeorder.proto` 文件头注释**的独立事实（不从 `CanTransition` 反推，避免同义反复）；终态无出边；`REFUND_REJECTED` 无入边；只有 `FULFILLING` 允许同态重试；实现自加的补偿边 `FULFILLING → PAID` 单独钉住（谁偷偷加边先在这里响）；未定义枚举值被拒；`StateName` 覆盖每个已定义状态；model 层拒绝非法迁移、CAS 未命中不算错误、迁移自增 `version` 并记台账、台账失败回滚主行、退款链线性 |
| `helpers_test.go` | 19/4 | `helpers.go` 纯函数口径（截断、脱敏、分页、投影、金额乘法）+ 「列宽常量与迁移 SQL 对齐」这条跨文件不变量；这些函数决定落到 `to_order`/`to_order_event` 的文本形状，放松的表现不是报错而是静默写坏（半个中文、超长被 MySQL 裁、凭据落库） |

`fakes_test.go`（`0/0`）是替身与假下游层，见 §11.4，不是漏计的用例文件。

### 11.2 其他层

- `model`：`migration_parity_test.go` `27/12` —— 把 model 结构体 db tag / 列常量 / `INSERT` 语句 /
  logic 校验上限与 `deploy/migrations/trade-order/000001_create_trade_order_tables.sql` 钉成可执行门禁
  （纯文本解析 + 反射，不连库）：`uniq_order_no`/`uniq_request_id` 必须是唯一键、参与唯一性判定的
  字符列必须 `utf8mb4_bin`（整数列不登记排序规则要求）、金额列必须 BIGINT 且注释写明「分」、
  11 个状态值逐个有注释、承载退款的列注释必须写明「只走沙箱台账、不退到卡」、
  logic 的列宽上限直接读 `internal/logic/helpers.go` 而不是抄一份副本。
- `internal/config`：`config_load_test.go` `4/0` —— 用 `conf.Load` 真实加载 `etc/tradeorder.v1.yaml`，
  并以**字面量**（不引用 `default` 标签）钉死 `MaxQuantityPerOrder`/`OrderExpireSeconds`/
  `StuckScanMaxLimit`/`MaxListWindowSeconds`/`MaxPageSize`/`FulfillMaxAttempts`/
  `MinSecondsBetweenFulfillRetry`/`DefaultCurrency` 的取值：这些数字是运维据以调参的口径，
  yaml 与 `config.go` 漂移时这里是唯一拦截点。
- `internal/svc`：**无离线单测**（只做依赖装配，判定都在 logic 层）。
- 本服务没有 `internal/repository`、`internal/consumer`、`internal/policy` 目录
  （无 MQ 消费者与 outbox，见 §10 第 1 条）。

### 11.3 构造器级覆盖

**12/12**：探针取 `internal/logic` 全部 `New*Logic(`，逐个在 `*_test.go` 里查引用，无缺口。
其中 GetOrder / ListMyOrders / ListOrderEvents / ListOrders / ListStuckOrders 五个读侧方法此前为零引用，
现各有独立用例文件（§11.1 B 组）。

### 11.4 替身层与断言口径

`internal/logic/fakes_test.go` 提供「内存版 model」+「假事务」+「假下游 RPC 客户端」
（membership / payment / coin 是 goctl 生成的 client 接口，可直接赋值）：

- 注入依据是 `svc.ServiceContext` 的 `Orders`/`OrderEvents` 为接口类型、`Conn` 只被用来开事务。
- 复刻的是**语义**而不是并发：唯一键命中即 `ErrDuplicateRequest`、
  「`UPDATE ... WHERE state=? AND version=?`」条件不命中即 `(false, nil)`、非法迁移即报错、
  事务回调报错即整体回滚。真实正确性由 MySQL 的 `uniq_*` 与 `RowsAffected`（DSN 禁 `clientFoundRows`）保证。
- 每个假实现只嵌入接口并覆写被测路径用到的方法，其余方法由内嵌 nil 接口提升，走到即 panic——
  失败是响的，不会被写成「通过」。
- `OrderUpdate` 的可写列集合在 model 里是私有白名单，替身用只读反射取出 `SET` 片段并按 model 的
  SQL 语义施加到内存行；遇到未知片段直接 panic，于是「加了新列却没同步假实现」会以测试失败暴露，
  而不是静默丢写入。

断言口径：读侧统一用 `assertCalls` 断**触库序列**（守卫是否发生在取数之前）、
`markWrites`/`assertNoWritesAfter` 断**写副作用增量为零**（读路径不许顺手补偿）；
资金类用例一律断「哪几行没写、哪几步没调下游」，而不是断「返回了错误」。

它**证明不了**：真实 SQL 文本与列名（由 `model/migration_parity_test.go` 兜）、索引是否命中、
真驱动下 `RowsAffected` 的 matched vs changed 语义、`uniq_order_no`/`uniq_request_id` 的撞键行为、
并发双扣与重放的真实时间窗。

### 11.5 覆盖边界（不可省略）

- 用例不连接 MySQL/Redis/etcd/MQ/对象存储；下游 membership/payment/coin 全部是进程内假 client，
  `FulfillOrder` 的「提交后才调下游」这一顺序在进程内可证，但下游真的幂等性不可证。
- **真实资金路径在单测里不存在**：渠道回调验签、原路退回渠道、退款到卡、提现、打款出金、
  对账文件、发票税务本期都不实现（AGENTS.md §1、§10 第 7 条），因此不是「被覆盖了」而是
  「没有这条路径」。用例能钉的是缺配置/越界时**分级显式失败**（§5）与
  `ApproveRefund` 缺下游时 `TestApproveRefundMissingDownstreamConfigured` 的 not-configured 形状。
  支付与退款只走沙箱台账，「款已退、权益未回收」停在 `REFUND_APPROVED` 是被钉住的现状。
- 迁移 SQL 与真实库的列级对账：本 README 的验证节（§9）**未**声明在隔离实例 `127.0.0.1:3399`
  做过列级复验，因此本节按「未在目标实例复验」口径描述；建表应用状态以
  `deploy/migrations/README.md` 的登记表为准，上线仍须由运维在目标实例执行迁移命令。
  `model/migration_parity_test.go` 只做文本 + 反射比对，不连库。
- `internal/server`、`rpc/*.pb.go`、`internal/svc` 与入口模板等 goctl 生成壳不在单测范围内。
- 本节未复核缺口清单，缺口的权威登记在 §10「已知缺口」（§10 第 9 条与本节口径一致）。

### 11.6 验证命令

```bash
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
go test -p 1 -count=1 ./services/trade-order/...
gofmt -l services/trade-order        # 必须无输出
go vet ./services/trade-order/...    # 必须无输出
```

`-p 1` 是硬要求：Windows 页面文件限制下并发编译/运行多个测试包会 OOM（errno=1455）。
构建与生成一致性门禁见 §9。
