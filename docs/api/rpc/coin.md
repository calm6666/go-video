# RPC · `coin`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/coin/rpc/coin.proto` |
| protobuf 包 | `coin.v1` |
| go_package | `go-video/services/coin/rpc` |
| 发现用的 etcd key | `coin.v1.rpc`（`services/coin/etc/coin.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`coin.v1.rpc`） |
| 监听 | `8163`（`services/coin/etc/coin.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_coin` |
| 方法数 | 10（service `Coin`） |
| 网关消费方 | `app:CoinRPC`、`admin:CoinRPC` |

## 契约说明

> 投币域契约（硬币 = 社区虚拟币，不是钱）。
>
> 数据所有权（§5）：本服务持有硬币余额、投币记录与每日额度；
> video/engagement 侧的 coin_count 只是投影，必须由本服务的事件或回算修正，
> 不允许别处直接改计数，也不允许与 payment 的现金余额互换（两套账）。
>
> 与商业化的关系：硬币的**获得**有两条路——运营发放（GrantCoin，需 reason）
> 和买硬币包（trade-order 履约时调 GrantCoin，biz_no 带订单号）；
> 硬币的**消耗**只有投币。全链路都不涉及真实资金。

## service `Coin`

> Coin 硬币与投币服务。

gRPC 方法前缀：`coin.v1.Coin/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `GetCoinAccount` | [`GetCoinAccountReq`](#message-getcoinaccountreq) | [`GetCoinAccountReply`](#message-getcoinaccountreply) | 我的硬币账户（含今日额度） |
| 2 | `TossCoin` | [`TossCoinReq`](#message-tosscoinreq) | [`TossCoinReply`](#message-tosscoinreply) | 投币（扣币 + 记录 + 限额判定，幂等） |
| 3 | `CancelToss` | [`CancelTossReq`](#message-canceltossreq) | [`CancelTossReply`](#message-canceltossreply) | 取消投币（窗口内全额退回） |
| 4 | `ListMyTosses` | [`ListMyTossesReq`](#message-listmytossesreq) | [`ListMyTossesReply`](#message-listmytossesreply) | 我的投币记录 |
| 5 | `GetTargetSummary` | [`GetTargetSummaryReq`](#message-gettargetsummaryreq) | [`GetTargetSummaryReply`](#message-gettargetsummaryreply) | 单内容投币汇总 |
| 6 | `BatchGetTargetSummary` | [`BatchGetTargetSummaryReq`](#message-batchgettargetsummaryreq) | [`BatchGetTargetSummaryReply`](#message-batchgettargetsummaryreply) | 列表页批量汇总 |
| 7 | `ListTargetTossers` | [`ListTargetTossersReq`](#message-listtargettossersreq) | [`ListTargetTossersReply`](#message-listtargettossersreply) | 谁投了这条内容（运营/排障） |
| 8 | `GrantCoin` | [`GrantCoinReq`](#message-grantcoinreq) | [`GrantCoinReply`](#message-grantcoinreply) | 发放/扣回硬币（运营授权或订单履约） |
| 9 | `ListCoinFlows` | [`ListCoinFlowsReq`](#message-listcoinflowsreq) | [`ListCoinFlowsReply`](#message-listcoinflowsreply) | 硬币流水台账分页 |
| 10 | `GetTossConfig` | [`GetTossConfigReq`](#message-gettossconfigreq) | [`GetTossConfigReply`](#message-gettossconfigreply) | 生效参数读取 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `CoinFlowType`

> 流水类型

| 值 | 编号 | 说明 |
|---|---|---|
| `COIN_FLOW_TYPE_UNSPECIFIED` | 0 | — |
| `COIN_FLOW_TYPE_TOSS` | 1 | 投币扣减（负） |
| `COIN_FLOW_TYPE_CANCEL_TOSS` | 2 | 取消投币退回（正） |
| `COIN_FLOW_TYPE_ORDER_PACK` | 3 | 硬币包履约发放（正） |
| `COIN_FLOW_TYPE_ADMIN_GRANT` | 4 | 运营发放或扣回（正负皆可） |
| `COIN_FLOW_TYPE_EXPIRE` | 5 | 预留：硬币过期（本项目未开启，恒不出现） |

### enum `TossState`

| 值 | 编号 | 说明 |
|---|---|---|
| `TOSS_STATE_UNSPECIFIED` | 0 | — |
| `TOSS_STATE_ACTIVE` | 1 | — |
| `TOSS_STATE_CANCELLED` | 2 | — |

### enum `Platform`

| 值 | 编号 | 说明 |
|---|---|---|
| `PLATFORM_UNSPECIFIED` | 0 | — |
| `PLATFORM_ANDROID` | 1 | — |
| `PLATFORM_IOS` | 2 | — |
| `PLATFORM_HARMONY` | 3 | — |
| `PLATFORM_DESKTOP` | 4 | — |
| `PLATFORM_WEB` | 5 | — |

### enum `TossRejectReason`

> 投币被拒时的结论。与错误码分开：余额不足/超日限/超单片上限都是**业务结论**， / 客户端要能据此决定文案与是否引导充值。

| 值 | 编号 | 说明 |
|---|---|---|
| `TOSS_REJECT_REASON_UNSPECIFIED` | 0 | — |
| `TOSS_ACCEPTED` | 1 | — |
| `TOSS_REJECT_INSUFFICIENT_BALANCE` | 2 | — |
| `TOSS_REJECT_DAILY_LIMIT` | 3 | — |
| `TOSS_REJECT_TARGET_LIMIT` | 4 | 单个内容累计已达上限 |
| `TOSS_REJECT_TARGET_INVALID` | 5 | aid 非正数（是否存在由 video 侧判定，不在这里越权查） |
| `TOSS_REJECT_SELF_TOSS` | 6 | 预留：作者给自己投币（需 social-graph 之外的作者身份，本轮不判定） |
| `TOSS_REJECT_RISK_BLOCKED` | 7 | 预留：风控拦截（risk-control 未接线，恒不出现） |
| `TOSS_REJECT_CANCEL_WINDOW_EXPIRED` | 8 | 撤币窗口已过（不是限额问题，客户端要能区分文案） |

### message `CoinAccountInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `balance` | `int64` | 2 | — | 当前可用硬币 |
| `total_tossed` | `int64` | 3 | — | 累计投出（历史口径，不因取消而回退） |
| `today_tossed` | `int64` | 4 | — | 今日已投 |
| `today_limit` | `int64` | 5 | — | 今日上限（服务侧配置投影） |
| `per_target_limit` | `int64` | 6 | — | 单内容上限 |
| `cancel_window_seconds` | `int64` | 7 | — | 取消投币的时间窗 |
| `version` | `int64` | 8 | — | — |
| `ctime` | `int64` | 9 | — | — |
| `mtime` | `int64` | 10 | — | — |

### message `TossInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `toss_id` | `int64` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `target_aid` | `int64` | 3 | — | — |
| `count` | `int32` | 4 | — | 该片累计投币数 |
| `state` | [`TossState`](#enum-tossstate) | 5 | — | — |
| `first_tossed_at` | `int64` | 6 | — | — |
| `last_tossed_at` | `int64` | 7 | — | — |
| `cancelled_at` | `int64` | 8 | — | — |
| `last_request_id` | `string` | 9 | — | — |

### message `CoinFlowInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `flow_id` | `int64` | 1 | — | — |
| `mid` | `int64` | 2 | — | — |
| `flow_type` | [`CoinFlowType`](#enum-coinflowtype) | 3 | — | — |
| `delta` | `int64` | 4 | — | 正入负出 |
| `balance_after` | `int64` | 5 | — | — |
| `target_aid` | `int64` | 6 | — | 投币类流水才有 |
| `biz_no` | `string` | 7 | — | 订单号/工单号 |
| `operator` | `string` | 8 | — | "user" / "trade-order" / 运营工号 / "cron" |
| `request_id` | `string` | 9 | — | 幂等键（唯一索引） |
| `remark` | `string` | 10 | — | 摘要，不含 PII |
| `ctime` | `int64` | 11 | — | — |

### message `TargetCoinSummary`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | — |
| `coin_count` | `int64` | 2 | — | 该片收到的硬币总数（按未取消的投币记录聚合） |
| `coin_user_count` | `int64` | 3 | — | 投币人数 |
| `like_count` | `int64` | 4 | — | 恒为 0：点赞归 engagement，本服务不持有，留位是为了让调用方别去猜 |

### message `GetCoinAccountReq`

> --- 账户与投币 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |

### message `GetCoinAccountReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `found` | `bool` | 1 | — | 从未有过硬币账户时 found=false 且 balance=0，不是错误 |
| `account` | [`CoinAccountInfo`](#message-coinaccountinfo) | 2 | — | — |

### message `TossCoinReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `target_aid` | `int64` | 2 | — | — |
| `count` | `int32` | 3 | — | 本次投几枚，<=0 视为 1；超单片上限由服务侧判定 |
| `request_id` | `string` | 4 | — | 必填幂等键（同一 request_id 重放不重复扣币） |
| `platform` | [`Platform`](#enum-platform) | 5 | — | — |
| `client_trace_id` | `string` | 6 | — | — |

### message `TossCoinReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `accepted` | `bool` | 1 | — | — |
| `reason` | [`TossRejectReason`](#enum-tossrejectreason) | 2 | — | — |
| `duplicated` | `bool` | 3 | — | 命中 request_id 重放 |
| `account` | [`CoinAccountInfo`](#message-coinaccountinfo) | 4 | — | — |
| `toss` | [`TossInfo`](#message-tossinfo) | 5 | — | — |
| `flow_id` | `int64` | 6 | — | — |
| `reject_detail` | `string` | 7 | — | 可读结论（例如「今日还可投 2 枚」），不承载错误码语义 |

### message `CancelTossReq`

> 取消投币：全额退回该用户对该内容投出的硬币，记录置 CANCELLED。 / 只在 cancel_window_seconds 窗口内允许；超窗返回结论而不是静默成功。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `target_aid` | `int64` | 2 | — | — |
| `request_id` | `string` | 3 | — | — |
| `operator` | `string` | 4 | — | 用户自助为 "user"；运营代操作填工号 |
| `reason` | `string` | 5 | — | 运营代操作必填 |

### message `CancelTossReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `cancelled` | `bool` | 1 | — | — |
| `reason` | [`TossRejectReason`](#enum-tossrejectreason) | 2 | — | — |
| `duplicated` | `bool` | 3 | — | — |
| `account` | [`CoinAccountInfo`](#message-coinaccountinfo) | 4 | — | — |
| `toss` | [`TossInfo`](#message-tossinfo) | 5 | — | — |
| `flow_id` | `int64` | 6 | — | — |
| `reject_detail` | `string` | 7 | — | 拒绝时给终端看的补充文案（与 TossCoinReply.reject_detail 同口径） |

### message `ListMyTossesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `state` | [`TossState`](#enum-tossstate) | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `ListMyTossesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tosses` | [`TossInfo`](#message-tossinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `GetTargetSummaryReq`

> --- 内容侧读取（详情页、列表页）---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | — |

### message `GetTargetSummaryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `summary` | [`TargetCoinSummary`](#message-targetcoinsummary) | 1 | — | — |

### message `BatchGetTargetSummaryReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aids` | `int64` | 2 | repeated | — |

### message `BatchGetTargetSummaryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `summaries` | [`TargetCoinSummary`](#message-targetcoinsummary) | 1 | repeated | — |

### message `ListTargetTossersReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `target_aid` | `int64` | 1 | — | — |
| `page` | `int64` | 2 | — | — |
| `size` | `int64` | 3 | — | — |

### message `ListTargetTossersReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tosses` | [`TossInfo`](#message-tossinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `GrantCoinReq`

> --- 发放（运营/订单履约）--- / 单一发放入口：正数发放、负数扣回。必须带 reason 与 request_id。 / 订单履约调用时 operator="trade-order"、biz_no=订单号，便于对账时区分「白送的」和「买来的」。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | — |
| `delta` | `int64` | 2 | — | 不允许为 0 |
| `flow_type` | [`CoinFlowType`](#enum-coinflowtype) | 3 | — | 只接受 ORDER_PACK / ADMIN_GRANT |
| `biz_no` | `string` | 4 | — | — |
| `operator` | `string` | 5 | — | — |
| `request_id` | `string` | 6 | — | — |
| `reason` | `string` | 7 | — | ADMIN_GRANT 必填；ORDER_PACK 可空但建议带订单摘要 |

### message `GrantCoinReply`

> GrantCoinReply 刻意不给 accepted/reason 结论通道：发放只由服务身份调用（订单履约、运营补币）， / 「会让余额变负」这类拒绝是调用方的编程错误而非用户可见结论，用 gRPC error 表达才不会让 / 调用方把它当成成功记账。终端可见的投币/撤币结论走 TossCoinReply/CancelTossReply 的 reason 枚举。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `duplicated` | `bool` | 1 | — | — |
| `account` | [`CoinAccountInfo`](#message-coinaccountinfo) | 2 | — | — |
| `flow_id` | `int64` | 3 | — | — |

### message `ListCoinFlowsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 0 表示跨用户（运营面） |
| `flow_type` | [`CoinFlowType`](#enum-coinflowtype) | 2 | — | — |
| `biz_no` | `string` | 3 | — | — |
| `from_ts` | `int64` | 4 | — | — |
| `to_ts` | `int64` | 5 | — | — |
| `page` | `int64` | 6 | — | — |
| `size` | `int64` | 7 | — | — |

### message `ListCoinFlowsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `flows` | [`CoinFlowInfo`](#message-coinflowinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `page` | `int64` | 3 | — | — |
| `size` | `int64` | 4 | — | — |

### message `GetTossConfigReq`

> 参数自述：日限/单片上限/取消窗口都来自服务端配置， / 客户端不写死（§6），运营页也要能读到当前生效值。

（空消息）

### message `GetTossConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `daily_limit` | `int64` | 1 | — | — |
| `per_target_limit` | `int64` | 2 | — | — |
| `cancel_window_seconds` | `int64` | 3 | — | — |
| `min_balance_to_toss` | `int64` | 4 | — | — |
| `initial_balance` | `int64` | 5 | — | 新建账户的初始硬币（沙箱便利，非真实赠送规则） |
