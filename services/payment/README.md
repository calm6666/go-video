# payment

资金域服务：现金余额、充值台账、支付受理、退款与资金流水。本服务是「钱」的唯一写入口
（AGENTS.md §5「资金台账（余额/充值/支付/退款流水）→ payment」）。

- 数据所有者：payment 服务（`go_video_payment` 库的 5 张 `pm_*` 表只有本服务可写）
- Owner：交易与资金域
- 数据库：`go_video_payment`（迁移在 `deploy/migrations/payment/`）
- 注册中心 etcd Key：`payment.v1.rpc`，默认监听 `0.0.0.0:8161`
- 契约源：`rpc/payment.proto`（`rpc/*.pb.go`、`internal/server/paymentserver.go`、
  入口 `payment.v1.go` 均为 `goctl`/`protoc` 产物，禁止手改；改契约后执行
  `./scripts/gen.ps1 -Service payment`）
- 无 HTTP 面：本服务只提供 gRPC，没有 `.api`；终端与运营页一律经 `gateway/app` /
  `gateway/admin` 聚合（AGENTS.md §6）
- 无 MQ、无 outbox、无 Redis 写入：台账判定只认 MySQL（见 §6）

## 1. 沙箱语义（先读这一节，否则会误判成真实收单）

本项目**不接任何真实支付渠道**。唯一可用渠道是 `PAY_CHANNEL_SANDBOX`，它只把数写进本地
台账：不请求第三方支付网关、不产生真实资金移动。因此：

- 「充值到账」「支付成功」是台账的**真实状态推进**（会员权益读侧据此判定生效），
  不是伪造成功；而「真实收单」在本项目根本不存在；
- `DescribeChannels` 把这件事做成可查询事实：恒 `sandbox_only=true`、
  `channels[].real_money=false`，`note` 写明「不产生真实资金移动」，供运营页显式标注；
- 需要真实渠道才成立的能力**一律不开接口**，被请求时返回显式错误而不是假成功。

| 能力 | 状态 | 被请求时的行为 |
|---|---|---|
| 渠道异步回调验签 | 不开（无回调） | 不存在入口；入账只能由 `SettleSandboxRecharge` 驱动 |
| 退款原路退回渠道/银行卡 | 不开 | `RefundPayment(to_balance=false)` → `FailedPrecondition`：`payment: refund to original channel not configured` |
| 提现、打款出金 | 不开 | 无 RPC 方法；出金只能体现为 `AdjustBalance` 的负向台账调整 |
| 对账文件下载/渠道对账 | 不开 | 无入口；本服务只有 `ListFlows` 自证台账（见 §9 缺口） |
| 发票、税务、汇率结算 | 不开 | 单一币种（`Payment.DefaultCurrency`），非默认币种直接 `InvalidArgument` 拒绝 |
| 非 SANDBOX 渠道（含 UNSPECIFIED） | 不存在 | `OpenRecharge`/`CreatePayment` 直接拒绝，绝不「看不见就当沙箱处理」 |

`Payment.AllowedChannels` 是渠道门禁配置：示例配置只放 `SANDBOX`。写了无法识别的渠道名
会在启动时记 error 日志并忽略；沙箱被关掉时 `OpenRecharge`/`SettleSandboxRecharge`/
`DescribeChannels(enabled=false)` 都会显式体现，`SettleSandboxRecharge` 拒绝入账。

**硬币不归本服务**：硬币（虚拟社区货币）的余额与投币记录在 `coin` 服务，与这里的现金余额
是两套独立的账，不互换、不折算、不共享流水表。

## 2. 表清单（前缀 `pm_`，与迁移 SQL 逐字一致）

| 表 | 作用 | 唯一键 | 关键索引 |
|---|---|---|---|
| `pm_wallet` | 余额账户（现金唯一真值） | `uniq_mid(mid)` | — |
| `pm_recharge` | 充值单 | `uniq_recharge_no`、`uniq_request_id` | `idx_mid_ctime`、`idx_state_ctime` |
| `pm_payment` | 支付单（一单一支付） | `uniq_payment_no`、`uniq_biz_order_no`、`uniq_request_id` | `idx_mid_ctime`、`idx_state_ctime` |
| `pm_refund` | 退款单 | `uniq_refund_no`、`uniq_request_id` | `idx_payment_no`、`idx_biz_order_no`、`idx_mid_ctime` |
| `pm_flow` | 资金流水（append-only） | `uniq_request_id`、`uniq_biz_type_biz_no` | `idx_mid_ctime`、`idx_biz_type_ctime`、`idx_biz_no` |

参与唯一性判定的列（单据号、`biz_order_no`、`request_id`、`biz_no`、`currency`、`destination`）
一律列级 `utf8mb4_bin`，表级 `utf8mb4_unicode_ci`：`*_ci` 会把大小写不同的两个 `request_id`
判成同一个键，从而误吞重放请求。

`pm_payment.operator/remark` 经 `PaymentInfo.operator/remark` 回显给运营面；
`pm_payment.last_request_id`（只用于幂等重放判定）与 `pm_recharge.client_trace_id` 仍是纯内部审计列，不回显。

## 3. RPC 方法判定口径

`service Payment`（gRPC，客户端 `rpc.NewPaymentClient(zrpc.MustNewClient(...))`）：

| 方法 | 判定要点 |
|---|---|
| `GetWallet` | 只读、不建行；账户不存在按 0 余额返回（`balance_minor=0`、`ctime/mtime=0`）；`frozen_minor` 恒 0，可用余额就是 `balance_minor`；读失败上抛错误，不折叠成 0 余额 |
| `DescribeChannels` | 恒 `sandbox_only=true`、`real_money=false`；`note` 必须保留「不产生真实资金移动」文案；`enabled` 反映 `AllowedChannels` |
| `OpenRecharge` | 金额必须在 `[MinRechargeMinor, MaxRechargeMinor]`；渠道只接受 SANDBOX；`request_id` 必填，命中重放返回首单 + `duplicated=true`；只落 PENDING，不动余额、不写流水 |
| `SettleSandboxRecharge` | **只对 PENDING 生效**；`duplicated=true` 用于已 SUCCESS 单（返回原单据/当前余额/原流水号）；`CANCELLED/FAILED` 拒绝；「改单据 + 加余额 + 写 RECHARGE 流水 + `settled_at`」同一事务；`operator`/`request_id` 必填 |
| `CancelRecharge` | `reason` 必填；只能取消 PENDING；已 SUCCESS 要撤回必须走退款/调整，错误消息里写明；已 CANCELLED → `duplicated=true`；不动余额、不写流水 |
| `ListRecharges` | `mid=0`（跨用户）必须给完整时间窗且不超 `MaxListWindowSeconds`；`size<=MaxPageSize`、偏移 `<=MaxListOffset`；空结果 → 空列表 + `total=0`；查询失败上抛 |
| `CreatePayment` | `amount_minor<=0` 拒绝；只受理 `BALANCE`/`SANDBOX_CHANNEL`；`biz_order_no` 唯一 → 同单号重放（含换 `request_id` 的同金额同币种重试）返回首单 + `duplicated=true`，金额或币种不同 → `AlreadyExists` 冲突，绝不静默改价；`BALANCE` 走条件扣减（不足即 `FailedPrecondition` 且零写入）；`SANDBOX_CHANNEL` 直接 PAID 且**不写余额流水**，`wallet` 为未变动快照 |
| `GetPayment` | `payment_no` 与 `biz_order_no` 二选一（都给 → 拒绝）；查不到 → `found=false`（合法结论）；查询失败上抛，绝不能把故障说成「没有支付单」 |
| `ClosePayment` | `reason` 必填；只有 PENDING 可关；PAID/已退款 → `FailedPrecondition` 并提示走 `RefundPayment`；已 CLOSED → `duplicated=true`；不动余额（PENDING 从未扣款） |
| `ListPayments` | 有界性同 `ListRecharges`；可按 state/method 过滤 |
| `RefundPayment` | `to_balance=false` → `FailedPrecondition: payment: refund to original channel not configured`；`to_balance=true` 仅对 `BALANCE` 支付方式成立（沙箱渠道单的钱从未进余额，退成余额等于凭空加钱 → 一并拒绝）；PENDING/FAILED/CLOSED 不可退；累计退款不超 `amount_minor - refunded_minor`（守卫写在 UPDATE 的 WHERE 里）；`amount_minor=0` 表示全额剩余可退；`reason`/`operator`/`request_id` 必填；「写 pm_refund + 更新 payment 的 refunded_minor/state + 加余额 + 写 REFUND 流水」同一事务 |
| `ListRefunds` | `size`/偏移有上限；跨用户查询（`mid=0`）必须给完整时间窗（且 ≤ `MaxListWindowSeconds`），或给 `payment_no` 按单号收敛，否则拒绝 |
| `ListFlows` | 同 `ListRecharges` 的有界性；给了 `biz_no` 也不免掉时间窗；`biz_type` 不在 1..4 内直接报错，不把「查错类型」伪装成「没有资金变动」 |
| `AdjustBalance` | `delta=0` 拒绝；绝对值不超 `MaxAdjustMinor`；`operator`/`reason`/`request_id` 必填；结果余额不得为负（负向走条件扣减）；写 `ADMIN_ADJUST` 流水（`biz_no` 为生成的调整单号）；同一事务；`request_id` 命中已有流水 → `duplicated=true` |

错误统一由 `model/errors.go` 定义，**自带 gRPC code**（proto 注释对每个判定都指定了
FailedPrecondition / InvalidArgument / NotFound / AlreadyExists / Aborted，这是跨服务契约，
不能让网关按未知错误兜底成 500）。消息一律 `payment: ` 前缀，不含 SQL 片段、PII 与凭据。

## 4. 资金不变式（改动前必读）

1. **余额扣减只能是条件更新**：
   `UPDATE pm_wallet SET balance_minor = balance_minor - ? WHERE mid = ? AND currency = ? AND balance_minor >= ?`
   影响行数 0 就是余额不足。禁止「先查后改」——那会在并发下超扣。
   唯一实现入口是 `model.WalletModel.ApplyDeltaTx`（正负皆走条件式），logic 不得自己拼 UPDATE。
2. **多表写入必须同事务**：改单据 + 改余额 + 写流水在同一个 `Models.Tx`（`TransactCtx`）里，
   任何一步失败整体回滚；不允许出现「已扣款无支付单」或「已入账无流水」。
   回滚后返回的错误一律基于回读的真实台账状态，不回显未提交数据。
3. **状态推进用 CAS**：`WHERE state = <前置状态>`，0 行即被并发推进，回读后给结论
   （重放或拒绝），不假装成功。
4. **流水 append-only**：`pm_flow` 不提供任何 UPDATE/DELETE 方法；要纠正只能再记一条反向
   `ADMIN_ADJUST` 流水。`uniq_request_id` + `uniq_biz_type_biz_no` 是重复入账的第二、三道防线
   ——即使单据状态判定被绕过，同一张单据也只可能落一条流水。
5. **金额一律 int64 最小货币单位（分）+ 显式币种**，禁止浮点；DB 侧 `CHECK` 约束
   （余额非负、`refunded_minor <= amount_minor`、`delta_minor <> 0`）兜住旁路写入。
6. **钱包行按需安全建行**：`INSERT ... ON DUPLICATE KEY UPDATE id = id`（并发/重复不报错，
   且对既有行加锁）；只读路径（`GetWallet`）不建行。

## 5. 幂等口径

| 写操作 | 幂等键 | 重放表现 |
|---|---|---|
| `OpenRecharge` | `pm_recharge.request_id` | `duplicated=true` + 首次充值单 |
| `SettleSandboxRecharge` | 单据状态 CAS + `pm_flow.request_id` + `uniq_biz_type_biz_no` | `duplicated=true` + 原流水号，不重复入账 |
| `CancelRecharge` | `state=PENDING` CAS | `duplicated=true` |
| `CreatePayment` | `pm_payment.request_id`；跨重试还有 `uniq_biz_order_no` | `duplicated=true` + 首次支付单；金额/币种不符 → 冲突错误 |
| `ClosePayment` | `state=PENDING` CAS | `duplicated=true` |
| `RefundPayment` | `pm_refund.request_id` + `pm_payment` 累计守卫 | `duplicated=true` + 原退款单 |
| `AdjustBalance` | `pm_flow.request_id` | `duplicated=true` + 原流水号；号被别的业务用过 → `AlreadyExists` |

`request_id` **必填**（写接口的 `InvalidArgument: payment: request_id required for write
operations`）：没有它无法区分客户端重试与重复下单，服务端不代造。`operator` 对动钱的写
操作同样必填（审计主体）。同一 `request_id` 被复用到别的资金动作上会返回
`payment: request_id already used by another ledger entry, nothing was charged or credited,
retry with a new request_id`（事务已回滚，台账未动）。

## 6. 配置项（`etc/payment.v1.yaml`，键与 `internal/config/config.go` 一一对应，由 `internal/config/config_load_test.go` 钉住）

| 键 | 示例值 | 含义 |
|---|---|---|
| `Name` / `Etcd.Key` | `payment.v1.rpc` | 服务名与注册键（不带连字符） |
| `ListenOn` | `0.0.0.0:8161` | gRPC 监听 |
| `CacheRedis` | `127.0.0.1:6379` / `node` | 预留给读侧列表缓存；**当前不构造客户端、不写缓存**。键名不能叫 `Redis`（与 `zrpc.RpcServerConf` 内嵌字段冲突，`conf.Load` 会报 `conflict key redis`） |
| `DataSource` | `root:root@tcp(127.0.0.1:3306)/go_video_payment?...` | 本服务唯一可写库；生产从配置中心/Secret 注入 |
| `Payment.DefaultCurrency` | `CNY` | 单一币种；其他币种请求直接拒绝，不做隐式换汇 |
| `Payment.MinRechargeMinor` / `MaxRechargeMinor` | `1` / `200000` | 单笔充值下限/上限（分），上限即 2000 元，防手滑 |
| `Payment.MaxAdjustMinor` | `1000000` | 单次运营调整绝对值上限（分） |
| `Payment.MaxPageSize` | `100` | 列表单次条数上限 |
| `Payment.MaxListWindowSeconds` | `2592000` | 跨用户查询时间窗上限（30 天），超窗拒绝而不是全表扫 |
| `Payment.MaxListOffset` | `10000` | 列表偏移上限，兜底深翻页 |
| `Payment.AllowedChannels` | `[SANDBOX]` | 渠道门禁；整段缺失按「只放行 SANDBOX」并记告警，未知渠道名记告警并忽略 |

## 7. 运行与迁移

```bash
# 契约变更后重新生成（禁止手改生成物）
./scripts/gen.ps1 -Service payment

# 建库建表（幂等脚本，可重复执行）
./scripts/migrate.ps1 -Action up -Service payment

# 启动
go run ./services/payment -f services/payment/etc/payment.v1.yaml
```

## 8. 测试覆盖

离线单测（纯 Go 替身，不连 MySQL/Redis/etcd/MQ/对象存储，也不起 gRPC server）。
数字来自 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 的实测导出，
格式 `顶层/子用例`。规模：`internal/logic` 6 个文件 `121/25`（含 `fakes_test.go` 替身层 `0/0`）、
`model` 1 个文件 `18/13`、`internal/config` 1 个文件 `2/0`；`t.Skip` 0 条。

### 1. `internal/logic` 用例清单

| 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- |
| `helpers_test.go` | 18/9 | 单据号只认 `common/idgen` 的 ULID，熵源耗尽必须失败、不得退化成时间戳拼接；币种单一、不做隐式换汇，且默认币种跟随配置；理由长度按 **rune** 计（100 个中文不能被静默截断）；`page/size/offset/时间窗` 越界是**拒绝**而不是悄悄改小；`WalletInfo` 无行即 0 余额；投影 nil 安全、空列表不等于 nil、台账字段不被投影吞掉 |
| `payment_logic_test.go` | 26/2 | `CreatePayment` 的 BALANCE 路径「条件扣减 + 落支付单 + 落流水」同事务：余额不足、无钱包行、币种不符三者一个都不写，写流水失败整体回滚；SANDBOX_CHANNEL 受理即 `PAID` 但绝不动余额、不写流水，并受渠道闸门约束；`biz_order_no` 一单一支付——同金额同币种按重放返回，换金额或换币种判 `AlreadyExists` 冲突、绝不静默改价；唯一键冲突（并发对手先提交）回滚后回读，收敛成「重放首单」或「号被复用」；`ClosePayment` 只推进 PENDING 且不动余额不写流水，已 PAID/已退款被挡回并指向 `RefundPayment`（关单不是回滚资金的口子），CAS 未命中判 `Aborted`；`TestPaymentChainKeepsSandboxGateWithoutConfig` 锁「配置缺失时仍不放开渠道路径」 |
| `readpath_sandbox_test.go` | 13/5 | 「只有沙箱、没有真实资金」是可查询事实而不是 README 承诺：`DescribeChannels` 必须显式声明 `sandbox_only` / `real_money=false` 并点名列出未配置的出金能力，`Enabled` 跟随渠道闸门；`TestNotConfiguredPathsNeverFakeSuccess` 锁每个真实资金入口返回 not-configured 而非空成功；`KnownChannelsHasNoRealChannel` 锁渠道表里没有真实渠道；跨用户列表必须自带时间窗 + `page/size`（本用户查询可免窗口），读不到单据回 `found=false`/空列表，但 DB 故障不得被折成空结果冒充成功；未知 `biz_type` 的流水查询直接拒 |
| `recharge_logic_test.go` | 26/6 | `OpenRecharge` 只落 PENDING——不动余额、不写流水（入账唯一入口是 `SettleSandboxRecharge`）；非沙箱渠道被拒、沙箱开关关闭回 not-configured；`request_id` 幂等（重放返回首单），并发插入回落到重放，撞键后回读不到行则如实报错；结算的「改单据 + 加余额 + 写流水」同事务，写流水失败回滚入账、CAS 0 行回滚全部、败者回放赢家；`SettleSandboxRecharge` 只入账一次（重放不二次加钱）、非法前态拒绝、已入账的单不能靠 `CancelRecharge` 抹掉；取消 PENDING 不动钱且天然幂等 |
| `wallet_refund_logic_test.go` | 38/3 | `AdjustBalance` 只有 `AJ_` 前缀流水、没有单据（靠 reason + operator 留痕，与充值两条路径不得混用），负向调整走条件扣减、余额永不为负、拒绝时一条流水都不写，`request_id` 被充值占用/换 mid 复用均判冲突；`RefundPayment` 只支持退到余额且只对余额支付成立——退到渠道一律 not-configured、沙箱渠道单退成余额被拒（等于凭空加钱），累计退款不得超 `amount_minor`（守卫写在 UPDATE 的 WHERE，0 行即整笔回滚），顺序部分退 + 超额拒 + 全退后再拒，「退款单 + 支付单 + 余额 + 流水」四步同事务；`GetWallet` 缺账户读 0 且不建行；`TestLedgerChainKeepsFlowsConsistentWithBalance` 锁余额快照链与流水链一致，`TestLedgerWritesNeverUseRealChannelConfig` 锁台账写入不引用真实渠道配置 |

`fakes_test.go`（`0/0`）是替身与装配层，见第 4 组；它不是遗漏的用例文件。

### 2. 其他层

- `model`：`migration_parity_test.go` `18/13` —— 把 `deploy/migrations/payment` 的建表 SQL 与
  model 结构体 db tag / `SELECT` 列常量 / `INSERT` 实参做双向漂移比对（不连库，纯解析 + 反射）：
  逐表逐列存在性与类型、每列必须有 COMMENT、金额列必须 BIGINT 且注释写明「分」、
  参与唯一键的**字符**列必须 `utf8mb4_bin`（整数列 `mid`/`biz_type` 不要求排序规则）、
  数值枚举注释与常量集合逐值一致、只追加台账不得有 `mtime`、列宽不得小于代码上限、
  `uniq_request_id` / `uniq_biz_order_no` 必须是 UNIQUE、不变式 CHECK 在位、
  迁移卫生（禁 `FOREIGN KEY`/`TRUNCATE`/`DROP`/`GRANT`/`DELETE`）与目标库声明、时间列为 Unix 秒 BIGINT、
  窗口子句必须参数化。该门禁此前抓出过两处真实问题（`pm_wallet.frozen_minor` 注释漏单位、
  头注释未声明目标库），并纠正过一条误判规则（整数列不该被要求登记排序规则）。
- `internal/config`：`config_load_test.go` `2/0` —— `TestLoadReleaseYaml` 用 `conf.Load` 真实加载
  `etc/payment.v1.yaml`，端口/服务名/Etcd 键与部署登记值一致，且 `Config` 每个字段都在 yaml 里
  被显式赋值（漏配会被 `default` 静默填上，这条靠逐字段断言拦住）；`TestMinimalYamlAppliesDefaults`
  锁最小配置下各护栏值的默认口径。两条都只解析配置，不连 MySQL/Redis/etcd。
- `internal/svc`：**无离线单测**（该层只做依赖装配，不含判定）。
- 本服务没有 `internal/repository`、`internal/consumer`、`internal/policy` 目录：
  数据访问全部在 `model`，无 MQ 消费者，也没有策略层（无 outbox 见 §9 缺口 1、2）。

### 3. 构造器级覆盖

**14/14**：探针取 `internal/logic` 全部 `New*Logic(` 构造器，逐个在 `*_test.go` 里查引用，无缺口。
读侧与写侧都有从入口进来的用例，事务内函数（`applyXxxInTx` 一类）另有 `helpers_test.go` 与
`*_logic_test.go` 的构造器级用例双重覆盖。

### 4. 替身层与断言口径

`internal/logic/fakes_test.go` 提供「内存版资金台账」+「可回滚假事务」：

- 注入依据是 `svc.ServiceContext.Models` 由 `model.NewModels(sqlx.SqlConn)` 构造、五张表字段都是
  接口类型，测试先用假连接构造、再逐字段换成内存实现。
- 复刻的是**语义**而不是锁：唯一键命中即回 `1062`（`IsDuplicate` 复用生产实现，判定不与线上漂移）、
  CAS 条件不命中即 0 行、余额不足即 0 行、事务回调报错则整体回滚。断言的是 logic 面对这些返回值
  时的裁决，也就是无库条件下唯一可证明的部分。
- 每个假实现只覆写被测路径用到的方法，其余方法落到以 `fakeConn` 构造的真实 model 上，而 `fakeConn`
  只实现 `TransactCtx`，其它 sqlx 方法都是对 nil 接口发起调用——走到即 panic，失败是响的，
  不会被写成通过。

断言口径：拒绝类用例断「余额/单据/流水零写入」而不是「返回了错误」；幂等类断「重放返回首单 +
台账零增量」；列表类断「过滤在 SQL 侧生效 + 空列表非 nil + 故障必须报错」。

它**证明不了**：真实 SQL 文本与列名（由 `model/migration_parity_test.go` 的文本比对兜）、
索引是否被命中、驱动返回的 matched vs changed rows（DSN 禁 `clientFoundRows`）、
MySQL 唯一索引与 `CHECK (balance_minor >= 0)` 的拒绝行为、并发超扣所需的真实行锁。

### 5. 覆盖边界（不可省略）

- 用例不连接 MySQL/Redis/etcd/MQ/对象存储/搜索引擎，也不通过真实 gRPC 打到任何下游。
- **真实资金路径在单测里不存在**：渠道回调验签、退款到卡、提现、打款出金、对账文件、发票税务
  本期都不实现，因此不是「被覆盖了」，而是「没有这条路径」（AGENTS.md §1，详见 §9 缺口 1）。
  本服务唯一可证的是它们被调用时返回明确的 not-configured、绝不返回假成功
  （`TestNotConfiguredPathsNeverFakeSuccess`、`TestRefundPaymentToChannelIsNotConfigured`、
  `TestLedgerWritesNeverUseRealChannelConfig`）。支付与充值只走沙箱台账。
- 迁移 SQL 与真实库的列级对账只在隔离实例 `127.0.0.1:3399` 复验过（`applied` 记录见 §9 缺口 7
  与 `deploy/migrations/README.md`）；真实/共享实例未执行，上线仍须由运维在目标实例跑
  `./scripts/migrate.ps1 -Action up -Service payment`。`migration_parity_test.go` 本身不连库。
- `internal/server`、`rpc/*.pb.go`、`rpc/pb` 与入口模板等 goctl 生成壳不在单测范围内。
- 真库集成测试（唯一索引 / CHECK / `RowsAffected` / 并发行锁）尚未建立，登记见 §9 缺口 2。

### 6. 验证命令

```bash
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
go test -p 1 -count=1 ./services/payment/...
gofmt -l services/payment            # 必须无输出
go vet ./services/payment/...        # 必须无输出
```

`-p 1` 是硬要求，不是风格：Windows 页面文件限制下并发编译/运行多个测试包会 OOM（errno=1455）。
构建与生成一致性门禁见 §7。

## 9. 已知缺口

1. **无真实渠道、无对账拉取**：没有渠道回调、没有对账文件、没有提现/打款出金入口。
   `pm_flow` 只能自证内部一致（余额快照链 + 单据链），无法与外部银行/渠道流水核对。
2. **资金不变式仍缺真库验证**：logic 层已经用可回滚假事务覆盖了「条件扣减失败即整体回滚」
   「重放不二次入账」「退款累计守卫」「`uniq_biz_type_biz_no` 撞键回判赢家」等路径（§8），
   但这些是替身层断言；MySQL 真实唯一索引、`CHECK (balance_minor >= 0)` 与 `RowsAffected` 语义
   需要集成测试或压测脚本才算闭环。
3. **契约缺口（2026-09-22 主 agent 已补齐前两条，proto 已重新生成）**：
   - ~~`ListRefundsReq` 没有 `from_ts/to_ts`~~ → 已补 `from_ts = 5` / `to_ts = 6`，
     `ListRefunds` 改为与 `ListPayments` 同口径的 `requireListBounds` 收口（跨用户给完整时间窗即可，
     给 `payment_no` 时可省时间窗）；原 `ErrCrossUserRefundNeedsPaymentNo` 随之删除。
   - ~~`PaymentInfo` 不回显推进审计列~~ → 已补 `operator = 15` / `remark = 16` 并在
     `paymentInfo()` 投影；`last_request_id` 仍是纯内部列（只用于幂等重放判定），`pm_recharge.client_trace_id`
     仍未回显。
   - `PaymentState_PAYMENT_STATE_PENDING` 在本服务两条受理路径下不可达（沙箱受理即终态），
     因此 `ClosePayment` 在沙箱下几乎只会返回「已 PAID 拒绝」；保留分支是为未来真实渠道；
   - `RefundPayment` 无「按订单号退款」入口，只有 `payment_no`，订单侧需先 `GetPayment`。
4. **单币种、单账户**：一个 mid 一只钱包，不支持多币种账户与子账户（例如把充值余额与
   活动赠金分账）；`frozen_minor` 无预授权流程，恒为 0。
5. **无自动过期关闭**：`pm_payment.expire_at` 只做入参校验与存储，没有 cron 扫过期 PENDING
   单（沙箱下不会产生 PENDING 单，接真实渠道时必须补，位置在 `services/cron`）。
6. **沙箱渠道支付单的退款黑洞**：`PAY_METHOD_SANDBOX_CHANNEL` 的单既不能退渠道（未配置）
   也不能退余额（未占用余额），任何退款请求都被显式拒绝。若产品要求这类单可退，
   需要先在契约层定义「渠道收单如何退」的落账口径，不能靠给台账加钱糊过去。
7. **文档登记已补齐**：`deploy/migrations/README.md` 已登记 `go_video_payment`（1 个迁移文件、
   5 张业务表 `pm_wallet`/`pm_recharge`/`pm_payment`/`pm_refund`/`pm_flow`，
   2026-09-22 在隔离实例 `127.0.0.1:3399` 复验 `applied`）；同日因 model 漂移门禁修了列注释与头注释后，
   又在隔离实例用一次性 scratch 库把该文件重新 `CREATE` 一遍、核对注释真的落到列定义上，随后删除 scratch 库，
   `go_video_payment` 本身未被改动。端口 8161 + etcd key
   `payment.v1.rpc` 收录在 `docs/roadmap.md` 阶段 5 的端口/注册表一行。
