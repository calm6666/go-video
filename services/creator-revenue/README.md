# creator-revenue

创作者分成域服务：分成规则、参与关系、收益计量台账、周期结算单。

- 进程形态：gRPC（zrpc + etcd 注册），**不提供 HTTP**（AGENTS.md §3/§4，对外由 gateway 聚合）
- 监听：`0.0.0.0:8164`；注册键：`creatorrevenue.v1.rpc`（点号分段，不带连字符）
- 数据库：`go_video_creator_revenue`（本服务独占，7 张 `cr_*` 表）
- 契约来源：`rpc/creatorrevenue.proto`（16 个方法，全部已实现，无 `ErrNotImplemented` 残留）
- Owner：商业化/创作者收益方向；跨服务联动缺口见 §8「需要上游决策的点」

---

## 1. 职责边界与资金语义（AGENTS.md §1、§5、§7）

**本服务持有的事实**

1. 分成规则（一类收益怎么折算成金额）与其变更台账；
2. 创作者参与关系（谁在计划内、他当时确认的是哪个规则版本）；
3. 计量台账（**按规则折算后的应计金额**，不是原始行为事实）；
4. 周期结算单与分项（应计合计 + 封顶扣减 + 钱从哪来）。

**本服务不持有的事实（禁止越界）**

| 事实 | 真正的所有者 | 本服务的位置 |
|---|---|---|
| 有效播放/完播等行为量 | `spm` / `event-collector` | 只接收折算后的 `quantity` 上报（`RecordRevenueMetric`） |
| 投币流水 | `coin` | 同上，来源类型 `REVENUE_SOURCE_COIN` |
| 会员有效观看定义 | `membership` | 只认 `REVENUE_SOURCE_VIP_WATCH` 这个来源码与数量 |
| 硬币余额与投币记录 | `coin` | 不持有、不复算 |
| 资金台账（余额/充值/支付/退款） | `payment` | 不参与：分成不是资金流，只是应计台账 |
| 作者资料/等级 | `user-profile` / `creator` | 只存 `mid` 主键，不复制可变资料 |
| 广告参数、投放、广告位报表 | 范围外 | 契约里没有位，代码里没有路径（AGENTS.md §7） |

**金额三句话**

1. 所有 `*_minor` 都是 `int64` 最小货币单位（分），**禁止 float**；
2. 台账金额是**应计金额**，不是已支付金额；
3. **出金不在本期范围**：`cr_settlement.payout_state` 写入即 `1 (NOT_PAYABLE)` 且没有任何代码路径改它，
   `GetRevenueSummary.payout_available` 恒 `false`。「确认结算单」只表示应计口径冻结，**不等于打款**；
   提现/打款/银行卡/发票/税务/对账没有接口，被误调用只能得到明确错误（`model.ErrPayoutNotAvailable` 保留给未来的守门分支）。

**折算口径**（与 `model.ComputeAmountMinor` 逐字一致）

```text
amount_minor        = quantity × unit_price_per_1000_minor ÷ 1000   （整数除法，向下取整）
capped_amount_minor = 门槛拦截 → 0；否则按 (period, mid, source_type) 组内月度封顶分配
threshold_blocked   = 1 表示被 min_quantity 拦掉：不结算，也**不占用**封顶额度
```

余数（不足 1 分）丢弃且不跨周期追溯补偿：误差方向恒为「少算」，有利于平台而不利于作者，
不存在「平台欠账被抹掉、作者多领」的资金风险。取整方向要改必须先改本段与 model 注释，
不能让某次改动悄悄把往月金额算高。

溢出保护：`quantity × 单价` 超过 `int64` 时回 `ErrAmountOverflow`，**不回绕成负数**；
聚合侧（出单合计、封顶累加）同样带溢出判定，宁可整次出单失败。

---

## 2. 表清单与数据所有者（`deploy/migrations/creator-revenue/000001_create_creator_revenue_tables.sql`）

| 表 | 作用 | 关键约束 | 写入口 |
|---|---|---|---|
| `cr_revenue_rule` | 分成规则（一类收益→金额口径） | `uniq_rule_code`；`version` 乐观锁 | `UpsertRevenueRule`（只写 DRAFT）、`SetRevenueRuleState` |
| `cr_rule_change_log` | 规则变更台账（单价/状态 from→to） | `uniq_request_id`（幂等键） | 与规则主表同事务，只追加 |
| `cr_enrollment` | 参与关系（一人一行） | `uniq_mid`；`state` 1 ENROLLED/2 LEFT/3 SUSPENDED | `EnrollCreator`/`LeavePlan`/`SetEnrollmentState` |
| `cr_metric` | 计量台账（折算后的应计） | `uniq_metric_key(period,mid,aid,source_type)` | `RecordRevenueMetric` |
| `cr_metric_change_log` | 计量更正留痕（旧值不静默覆盖） | `idx_request_id`（非唯一，见 §7 缺口） | 与台账更正同事务，只追加 |
| `cr_settlement` | 周期结算单（应计合计 + 封顶扣减） | `uniq_settlement_no`、`uniq_active_period_mid(period,mid,void_seq)`、`uniq_request_id` | `GenerateSettlement`/`ConfirmSettlement` |
| `cr_settlement_item` | 结算分项（按来源聚合） | `uniq_no_source(settlement_no,source_type)` | 与结算单同事务 |

「同一周期同一作者至多一张在效单」由 `uniq_active_period_mid(period, mid, void_seq)` 实现：
在效行 `void_seq` 恒 0；作废时把该行 `void_seq` 改写成自己的 `settlement_id`（天然唯一），
于是 VOIDED 历史能留在同一 `(period, mid)` 下，新单又能占回 0 槽位——
既保住幂等语义，又满足「旧单置 VOIDED + 新单另起单号」的审计要求。

唯一性判定链上的列（`rule_code`/`period`/`settlement_no`/`request_id`）一律列级 `utf8mb4_bin`：
`_ci` 会折叠大小写，让只差大小写的幂等键互相吞且静默不报错。

---

## 3. RPC 方法判定口径表（16 个方法，与 proto 顺序一致）

| # | 方法 | 面 | 鉴权/身份 | 核心判定 | 幂等与写范围 |
|---|---|---|---|---|---|
| 1 | `UpsertRevenueRule` | 运营 | `operator`+`reason`+`request_id` 必填 | 只产生/修改 DRAFT；单价、封顶受配置护栏；`rule_id>0` 必带 `expected_version`，新建不得带 | 单事务：主表 + `cr_rule_change_log`（`uniq_request_id`）；撞键回滚后 `resolveRuleReplay` 回首次结果 |
| 2 | `SetRevenueRuleState` | 运营 | 同上 | 只允许 DRAFT→ACTIVE、ACTIVE→ARCHIVED、DRAFT→ARCHIVED；`ACTIVE` 时同来源旧 ACTIVE 自动归档 | 单事务：段锁 `source_type` → 锁本行 → 状态 CAS → 两笔台账（自动归档子键 `<request_id>#a<rule_id>`） |
| 3 | `ListRevenueRules` | 双 | 无（读） | state/source_type 越界报错而非空名单；`page/size` 折到 `MaxPageSize` | 只读；空列表投影 `[]` |
| 4 | `GetRevenueRule` | 双 | 无（读） | `rule_id` 优先，其次 `rule_code`；`version>0` 从变更台账 `from_` 列还原单价+状态；请求版本 > 当前 → `found=false` | 只读；不把现值冒充历史值 |
| 5 | `EnrollCreator` | 终端 | `operator`（自助固定 `user`）+`request_id` | `agreed_rule_version>0` 必填；`SUSPENDED` 拒绝自助重进 | 状态机幂等（已在目标态 → `duplicated=true` 零写入）；`uniq_mid` 兜并发首投；无台账表，`request_id` 只进日志 |
| 6 | `LeavePlan` | 终端/运营 | 同上；运营代操作需 `reason` | ENROLLED→LEFT；已 LEFT → `duplicated=true` | 同上（锁行后判态，`left_at` 只写一次） |
| 7 | `SetEnrollmentState` | 运营 | `operator≠user`、`reason` 必填 | 只处理 ENROLLED↔SUSPENDED；违规处置不接受被处置者自助解除 | 单事务锁行 CAS；已在目标态零写入 |
| 8 | `GetEnrollment` | 双 | 无（读） | 从未参加 → `found=false` + 空壳投影 | 只读 |
| 9 | `ListEnrollments` | 运营 | 无（读） | state 过滤越界报错；分页有界、`mid` 升序稳定遍历 | 只读；空列表投影 `[]` |
| 10 | `RecordRevenueMetric` | 系统（`spm`/`coin`/`cron`）/运营 | `operator`+`request_id` 必填；`reason` 见下 | 三道闸门：参与关系（锁 `cr_enrollment` 行，必须 ENROLLED）→ 规则（锁行后仍须 ACTIVE、来源一致、周期起点 ≥ `effective_from`）→ 结算状态（本周期单已 CONFIRMED → `ErrSettlementConfirmed`）；拒绝未来周期 | 单事务：闸门锁 + 台账 insert/更正 + `cr_metric_change_log` + 同组封顶重分配。重放判定 = `uniq_metric_key` + 同值（`created/corrected` 均 false，不写变更台账） |
| 11 | `ListRevenueMetrics` | 双 | 无（读） | **必须带 `period` 或 `mid`**（`ErrQueryScopeRequired`）：亿级表不做无界扫描；period 给了必须合法 | 只读；空列表投影 `[]` |
| 12 | `GenerateSettlement` | 运营/`cron` | `operator`+`request_id` 必填；`force_void_confirmed=true` ⇒ `reason` 必填（`ErrForceVoidReasonRequired`） | 拒绝未来周期；只对 ENROLLED 出单；无台账 → `ErrNoMetricsToSettle`（不出 0 元单冒充已结清）；金额只聚合 `capped_amount_minor`，不再折算；跨币种台账 → `ErrRuleCurrencyMismatch`；`mid=0` 全量批上限 `GenerateMaxBatch`，超出回 `truncated=true` | **每个 (period,mid) 一个事务**：`LockActive` → 幂等键回读 → 锁台账组 → 写主体+分项。DRAFT 就地重算（单号不变）；CONFIRMED 无强制位 → 零写入 `duplicated=true`；带强制位 → 旧单 VOIDED（原因落 `void_reason`）+ 新单号 |
| 13 | `ConfirmSettlement` | 运营 | `operator`+`reason`+`request_id` 必填 | 空列表拒绝；> `MaxConfirmBatch(200)` 拒绝；单号去重后逐张 CAS(`state=DRAFT AND void_seq=0`)；不满足条件的进 `failed_nos`；确认≠打款（`payout_state` 不动） | 整批**一个事务**（DB 故障整批回滚）；重放：已 CONFIRMED 且 `confirmed_by` 就是本次操作人 → 计入 `confirmed`，被他人确认 → `failed_nos` |
| 14 | `ListSettlements` | 运营 | 无（读） | **必须带 `period` 或 `mid`**；`state` 越界报错；`state=0` 不过滤（含 VOIDED，复核要看得到作废历史） | 只读；空列表投影 `[]` |
| 15 | `GetSettlement` | 双 | 无（读） | `settlement_no` 必填且不超列宽；`mid≠0` 校验归属，不一致 → `ErrForbidden`（不伪装成「查不到」）；单号不存在 → `found=false` | 只读；分项按 `source_type` 升序，空投影 `[]` |
| 16 | `GetRevenueSummary` | 终端 | 无（读，网关按会话取 mid） | 从未参加 → `state=UNSPECIFIED` 空壳；`current_estimate_minor` = 本周期 `capped_amount_minor` 合计（未收官，会随更正变动）；`total_confirmed_minor` = 窗口内已确认合计；`last_settled_period` 只认在效单 | 只读，4 次点查/聚合；`payout_available` 恒 `false` + `payout_note` 固定文案 |

---

## 4. 幂等、事务顺序与并发

### 4.1 三套幂等机制，各按各的粒度

| 机制 | 用在哪 | 落库位置 | 重放结论 |
|---|---|---|---|
| **变更台账唯一键** `cr_rule_change_log.uniq_request_id` | `UpsertRevenueRule`、`SetRevenueRuleState` | 整条 `request_id` 原样落库 | 事务回滚后 `resolveRuleReplay` 只读回首次结果；键属于别的 rule_code 或其后又有变更 → `ErrRequestIDConflict` |
| **行级派生幂等键** `<request_id>#<period>#<mid>` | `GenerateSettlement`（`mid=0` 时一次请求产多单） | `cr_settlement.uniq_request_id` | 事务内 `FindByRequest` 先抢键回读；事务外撞键回滚后再回读（`settleReplayed`）。回读能命中 **VOIDED** 行——重放看到的必须是当时那张单，否则会二次出单=二次应计 |
| **状态机本身** | `EnrollCreator`/`LeavePlan`/`SetEnrollmentState`/`ConfirmSettlement`/`RecordRevenueMetric` | 无独立键（这些表没有幂等位列） | 「已在目标态/同值重放」→ `duplicated=true`（或 `created=corrected=false`）且**零写入**：重复调用不会把 `enrolled_at`/`confirmed_at` 推到今天，也不会覆盖当时的 operator/remark/原因 |

派生键必须**在入口预算后缀宽度**（`requireScopedRequestID`）：否则 INSERT 才在 `VARCHAR(64)` 上失败，
要么报截断、要么把幂等键悄悄改短，两种都会让「同一 request_id 重放」判不出来。
子键后缀预算：`SetRevenueRuleState` 自动归档 `#a<rule_id>` = 21 字节；`GenerateSettlement` `#<period>#<mid>` = 27 字节。

### 4.2 事务边界（为什么必须这么大）

- **规则写**：主表 + 变更台账同事务。只写主表 = 「改价生效但没人知道是谁改的」；只写台账 = 「有人声称改了价但金额没变」。两种都比不改更糟。
- **计量写**：参与锁 + 规则锁 + 台账行 + 更正留痕 + 同组封顶重分配同事务。留痕与覆盖分开提交，一旦中间失败，「原来算多少」永久丢失。
- **出单写**：结算主体 + 分项 + 幂等键同事务。只有主体 = 看得到总额看不到钱从哪来；只有分项 = 孤儿数据。
- **批量出单刻意按 (period,mid) 拆事务**：全量一批 500 人，锁整月台账在一个事务里会把在线写拖死；
  合法可跳过结论（扫描快照后被暂停/退出）逐条 `Errorf` 留痕并继续，其余错误整请求失败——
  把 DB 故障折叠成「少出一个人」，等于让运营以为这个月已经出完单。

### 4.3 锁顺序（全服务统一，跨方法不要改）

1. `cr_revenue_rule`：先按 `source_type` 整段（`LockActiveBySource`，WHERE 不带 state），再锁本行——
   否则两条同来源规则并发生效会互相看不见对方，最终留下两条 ACTIVE，计量按哪条折算成了悬案。
2. `cr_enrollment`（`uniq_mid` 行锁）：计量写入的串行点。
3. `cr_settlement`（`uniq_active_period_mid` 上的 `LockActive`，同时是 `(period,mid)` 出单槽位锁）。
4. `cr_metric` 组行（`ListGroupForUpdate`，按 `source_type` 升序、组内按 `aid, metric_id` 升序）：
   锁与读同一语句完成；出单与封顶重分配共用这把锁，所以「出单读到的金额」与「事务结束后台账里的金额」必然一致。
5. `cr_settlement`（`uniq_active_period_mid`）→ 出单只读 `cr_enrollment`、不锁它：
   暂停不能把已经算完的应计悄悄抹掉，下一次重算才反映新状态。

封顶分配为什么必须按 `aid, metric_id` 升序：同一批数据要在任何写入顺序下得到相同结果，
否则「同一份台账两次算出两个金额」，运营无法解释也无法复核。

### 4.4 状态机

```text
规则     DRAFT ──激活──> ACTIVE ──归档──> ARCHIVED(终态)
           └────────────归档────────────┘     恢复只能新建草稿（历史台账解释才唯一）
参与     ENROLLED ⇄ SUSPENDED（运营处置）      ENROLLED ──> LEFT ──重新参加──> ENROLLED
计量     仅当参与=ENROLLED 且规则=ACTIVE 且本周期在效单未 CONFIRMED
结算     DRAFT ──重算──> DRAFT（同单号覆盖）    DRAFT ──确认──> CONFIRMED（金额冻结）
         CONFIRMED ──force_void──> VOIDED + 新单号 DRAFT    VOIDED 只读，不可复活
```

---

## 5. 配置（`etc/creatorrevenue.v1.yaml`，段名 `CreatorRevenue`）

| 键 | 默认 | 作用 | 配错的后果 |
|---|---|---|---|
| `Name` / `Etcd.Key` | `creatorrevenue.v1.rpc` | 注册键 | 带连字符不会启动失败，只会让服务发现永远为空、调用超时 |
| `ListenOn` | `0.0.0.0:8164` | 监听 | 写 `127.0.0.1` 容器内其它服务连不上 |
| `CacheRedis` | `127.0.0.1:6379` | 只读加速位（**键名固定 `CacheRedis`**） | 叫 `Redis` 会与 `zrpc.RpcServerConf` 内嵌字段冲突，`conf.Load` 直接报 `conflict key redis` |
| `DataSource` | `go_video_creator_revenue` | 本服务自有库 | 未配置时所有接口回 `ErrDBNotConfigured`，不返回零值冒充成功 |
| `CreatorRevenue.DefaultCurrency` | `CNY` | 规则/结算单记账币种兜底 | 留空会让出单在「无法确定记账币种」处失败 |
| `CreatorRevenue.MaxPageSize` | `100` | 四类列表单页上限 | 0/负数会让分页失去上限 |
| `CreatorRevenue.MaxRuleUnitPricePer1000Minor` | `1000000` | 单价护栏（分/千单位） | 关掉它，「把 ¥1 看成 ¥1000」会在结算里放大成资金事故 |
| `CreatorRevenue.MaxMonthlyCapMinor` | `50000000` | 封顶护栏（0=不限） | 小于单价护栏时规则永远配不出可用组合 |
| `CreatorRevenue.GenerateMaxBatch` | `500` | 全量出单单批上限 | 0/负数会退化成无界批量（代码按 500 兜底，但仍必须配） |
| `CreatorRevenue.SummaryRecentPeriods` | `12` | 概览「已确认合计」回看周期数（0=全历史） | 负数会让 `PeriodCutoff` 报错；`GetRevenueSummaryReply` 没有暴露窗口的位（§7） |

护栏与价格的分工：**价格写在 `cr_revenue_rule`（走 DRAFT→ACTIVE 状态机），配置只负责「手滑写成天价时拒写」和「分页/批量不失控」两件事。**

---

## 6. 运行

```powershell
# 1) 迁移（幂等建表，见 docs/commands.md §8）
./scripts/migrate.ps1 -Action up -Service creator-revenue

# 2) 契约变更后重新生成（.proto 是唯一来源，禁止手改 rpc/*.pb.go 与 internal/server/）
./scripts/gen.ps1 -Service creator-revenue

# 3) 启动
go run ./services/creator-revenue -f services/creator-revenue/etc/creatorrevenue.v1.yaml

# 4) 本服务门禁（Windows + Git Bash：先隔离 GOCACHE/GOTMPDIR，禁止 ./...）
cd services/creator-revenue   # 或全程用 ./services/creator-revenue/... 限定范围
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
gofmt -l services/creator-revenue                       # 必须无输出
go build ./services/creator-revenue/...
go vet   ./services/creator-revenue/...
go test -p 1 -count=1 ./services/creator-revenue/...
```

调用链（谁在什么时候调）：

```text
spm / coin / cron ──RecordRevenueMetric──┐
运营工号（活动激励回填，必带 reason）──────┘→ creator-revenue → cr_metric
cron 月度收官任务 ──GenerateSettlement(mid=0)──> cr_settlement(DRAFT) + 分项
gateway/admin      ──ConfirmSettlement──> CONFIRMED（应计口径冻结，仍不出金）
gateway/app        ──GetRevenueSummary / GetEnrollment / ListRevenueRules(ACTIVE)──> 创作者端
```

---

## 7. 已知缺口（尚未实现，禁止当成已交付）

1. **无 MQ、无 outbox、不发领域事件**：本服务没有 `internal/consumer/`，也不写 outbox 表。
   结算确认、台账更正目前**没有任何下游能感知**（会员/交易域都有 `*_biz_request` 或事件位，这里没有）。
   要接事件得先补 `api/` 的 schema 与版本、再在本服务加发布器与去重，不能先加配置。
2. **无 cron 自动出单接线**：`GenerateSettlement` 的 `mid=0` 全量批已实现，但 `services/cron`
   里**没有**「每月 N 日为上个周期出单」的任务登记，生产上必须人工调用。周期未收官也允许出单（预估口径），
   所以「什么时候算收官」是编排层策略，不在本服务写死。
3. **出金能力刻意不实现**：`payout_state` 恒 `NOT_PAYABLE`、`payout_available` 恒 `false`；
   提现/打款/银行卡/发票/税务/对账不开接口，也不会有「已打款」语义。`model.ErrPayoutNotAvailable`
   是为未来误接线准备的守门错误，现在没有调用点。
4. **单测覆盖的是判定层，不是数据库语义**：`internal/logic` 现有 11 个用例文件 + `fakes_test.go`
   内存替身（不含测试函数），共 **201 个顶层用例 / 69 个 `t.Run` 子用例**；
   `model/migration_parity_test.go` 另有 28 个门禁函数（子用例 14）把 `deploy/migrations/creator-revenue/000001_*.sql`
   与 99 列 struct tag、13 个 `*_minor`、7 个逐值枚举列、`void_seq` 复合唯一键、只追加台账、
   「应计 ≠ 已支付」出金红线逐条对齐（不连库，纯解析 + 反射）。
   逐文件明细、替身口径与覆盖边界见 §9。
   仍未验证：真驱动下 `RowsAffected`/唯一键撞键/`TransactCtx` 回滚语义，需要容器化 MySQL 集成测试。
   **不许**为了通过而删断言或把校验改成常开。
5. **契约缺口（proto/表未给位置，已在代码注释标注，不改 proto 的前提下只能记这里）**：
   - `RevenueMetricInfo` 没有 `corrected` 位（DB 有列）：读侧无法区分「这行被更正过」；
   - `RecordRevenueMetricReply` 没有 `duplicated` 位：同值重放只能靠 `created=false && corrected=false` 表达；
   - `ConfirmSettlement` 的 `reason`/`request_id` **无处落库**（没有结算确认审计表）：现在只进服务日志；
     建议新增 `000002_*.sql` 的 `cr_settlement_confirm_log(settlement_no, operator, reason, request_id UNIQUE, ctime)`；
   - `GetRevenueSummaryReply.total_confirmed_minor` 是**窗口内**合计（`SummaryRecentPeriods`，默认 12 个月），
     但字段注释写的是「历史已确认合计」且没有暴露窗口的位；要全历史请把配置设成 0；
   - `GenerateSettlementReply` 没有逐 mid 的失败明细位：全量批里的跳过只能在服务端日志里查；
   - `cr_enrollment` 没有变更台账表：暂停/恢复的 `reason` 折进 `remark` 单值字段，**只保留最后一条**原因。
6. **DRAFT 结算单重算无历史留痕**：就地覆盖金额，旧 DRAFT 值不进任何表（VOIDED 单保留金额与 `void_reason`）。
   语义上 DRAFT 是「未对外承诺」，可接受；若要做「重算前后差异」复核面板，需要新增结算变更台账表。
7. **封顶按「本次写入所依据的 ACTIVE 规则」重分配**：换 ACTIVE 规则（新 `monthly_cap_minor`）后
   **不会自动重算历史周期**，只有该组再次被上报时才会按新额度重分。要「改封顶即影响已计台账」
   必须显式跑一遍回填（`RecordRevenueMetric` 同值上报即可触发重分配），这是刻意保守的默认。
8. **迁移建表已复验，作废重算路径未跑**：`deploy/migrations/creator-revenue/000001_*.sql` 已于 2026-09-22
   在隔离实例（`127.0.0.1:3399`）`up` + `status` 跑通，`go_video_creator_revenue` 8 张表建成。
   但 `uniq_active_period_mid` + `void_seq=settlement_id` 的作废重算路径仍未被真实执行验证过
   （尤其确认 InnoDB 把 `void_seq` 改写后能重新占住 0 槽位）——建表通过不等于该不变式已证明，
   需要一次带真库的集成用例或人工 SQL 演练收口。
9. **CacheRedis 未被任何读路径使用**：概览/规则读取全部直接回源 MySQL。缓存位是预留，
   不代表已有缓存；接入前不得声称「概览走缓存」。
10. **库侧无兜底约束 + 派生枚举注释未指向权威列**（2026-09-22 漂移门禁复盘，均**未**改 SQL，只登记）：
   - `void_seq` 只能是 `0` 或本行 `settlement_id` 这条不变式**没有 `CHECK`/触发器支撑**，只由 `model.(*crSettlement).Void()`
     唯一写路径 + `uniq_active_period_mid` 共同保证；绕过 model 的修数能造出同一 `(period, mid)` 两张在效单。
     门禁只钉住可证明的部分（列存在、类型、`AND void_seq = ?` 守卫在位），不能替你证明库会拒绝脏写；
   - `cr_rule_change_log.source_type`/`from_state`/`to_state`、`cr_metric_change_log.source_type`、
     `cr_settlement_item.source_type` 五个派生列注释只写「收益来源」「变更前状态」，既不逐值也不点名权威列，
     而它们所在的 `000001` 文件头明示「禁止修改本文件、变更须新增 `0000NN_*.sql`」——
     因此这五处口径缺口**留待新增 `000002_*.sql` 的 `ALTER ... MODIFY COLUMN` 收口**，本轮没有把它们塞进门禁来制造假绿；
   - `cr_metric_change_log` 无任何唯一键（`request_id` 只是可空普通索引）：重复更正台账在库侧无去重手段，
     实际靠 `cr_metric.UpdateCorrection` 的 `WHERE metric_id = ? AND quantity = ?` CAS 兜底，
     门禁把这两者钉在一起（若该台账出现 `uniq_request_id` 反而判失败，防止误以为库侧已去重）；
   - `cr_rule_change_log.ListByRule` 按 `to_version DESC` 排序，而索引只有 `(rule_code, from_version)` 与
     `(rule_code, ctime)` ⇒ 每规则行数少时只是 filesort，接运营面板翻页前要先补 `(rule_code, to_version)`。

## 8. 需要上游/编排层决策的点

| 点 | 需要谁做 | 说明 |
|---|---|---|
| `gateway/admin` 商业化运营路由（规则/名单/台账/结算单/确认/强制重算） | 网关侧 | 本服务契约齐了；`force_void_confirmed` 与 `ConfirmSettlement` 必须挂权限点并留审计 |
| `gateway/app` 创作者端收益页（`GetRevenueSummary` + 台账分页） | 网关侧 | `payout_available=false` 必须渲染成「暂不可提现」而不是隐藏入口，`payout_note` 是服务端给的结论 |
| 月度收官出单任务 | `services/cron` | 需要「哪个时点算上月经确定量」的业务口径；调用方式：`GenerateSettlement(period=上一月, mid=0, operator=cron, request_id=幂等键)` |
| `spm`/`coin` 的折算量上报 | 各自服务 | 本服务只接受折算后 `quantity`；**禁止** `spm` 直接产出金额或直写 `cr_metric`（AGENTS.md §5、§7） |
| 迁移与文档登记 | 已完成（2026-09-22） | `deploy/migrations/README.md` 已登记 `go_video_creator_revenue`（1 个迁移文件，隔离实例复验 `applied`，8 张表）；端口 8164 + etcd key `creatorrevenue.v1.rpc` 与运营/终端路由前缀已收录在 `docs/roadmap.md` 阶段 5 一行 |

## 9. 测试覆盖

离线单测（纯 Go 替身，不连 MySQL/Redis/etcd/MQ，也不起 gRPC server）。数字来自
`grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 的实测导出，格式 `顶层/子用例`。
规模：`internal/logic` 12 个文件 `201/69`（11 个用例文件 + `fakes_test.go` `0/0`）、
`model` 1 个文件 `28/14`、`internal/config` 1 个文件 `3/1`；`t.Skip` 0 条。

### 9.1 `internal/logic` 用例清单（按域分组）

**A. 计量域（`cr_metric` + 变更台账）**

| 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- |
| `metric_entry_test.go` | 19/6 | `RecordRevenueMetric` 从入口进来才看得到的四件事：闸门开在**事务之外**（入参不合法时 `db.calls` 从 0 起、一条语句都不执行，不烧事务不抢行锁）；事务内完整语句序列（本服务唯一的应计写入路径，多一条读＝多抢一次行锁、少一条写＝漏记账）；幂等与留痕用「调用前取基线 → 调用后取差值」而不是全局累加；出金边界——计量写入绝不碰 `cr_settlement` 的任何写语句、不推进任何 `payout_state`。两条现状按原样钉住：`cr_metric` 没有 `request_id` 列 ⇒ 同值重放在库里不留痕；对外投影没有 `threshold_blocked` ⇒ 调用方只能从 `capped==0` 反推 |
| `metricmutate_test.go` | 13/5 | `metricmutate.go` 的计量台账口径：闸门顺序、折算**复用 model 的纯函数**（不在 logic 重算一遍）、幂等（唯一键 + 同值两条路径）、更正留痕的顺序，以及月度封顶的**组级确定性重分配**（`reallocGroupCap`） |

**B. 规则域（`cr_revenue_rule` + `cr_rule_change_log`）**

| 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- |
| `rulewrite_test.go` | 21/11 | 规则写侧：只写 DRAFT、状态机三条合法边、同来源唯一 ACTIVE 的自动归档、「主表 + `cr_rule_change_log` 同事务」、`request_id` 幂等回放（`rulereplay.go` + `UpsertRevenueRule`/`SetRevenueRuleState` 入口） |
| `rule_read_test.go` | 16/7 | `GetRevenueRule`/`ListRevenueRules`：定位优先级 `rule_id > rule_code > 报错` 用「两个都给但指向不同行」判别（只断「不报错」是永真断言）；历史版本回放是资金复核的根——版本大于当前 ⇒ `found=false`，变更台账缺失 ⇒ `found=false` 且**绝不回现单价**；「首条 `from_version >= ?`」的排序事实源在 `model/cr_rule_change_log.go`，种子按插入序与版本序**相反**放（插入序泄漏即红）；读故障与「查不到」分开（原始错误上抛）；现状钉住：`ListRevenueRules` 只校验 `source_type`、不校验 `state` |

**C. 结算域（`cr_settlement` + `cr_settlement_item`）**

| 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- |
| `settlecompute_test.go` | 16/7 | `settlecompute.go` 出单口径：金额**只从 `capped_amount_minor` 聚合**（不看原始 quantity）、幂等重放、在效单状态机、闸门与币种判定，以及「确认 ≠ 打款」在数据上的体现（新单写入即 `state=DRAFT`、`payout_state=NOT_PAYABLE`） |
| `settlement_entry_test.go` | 20/7 | `GenerateSettlement`/`ConfirmSettlement` 入口：闸门（不得晚于当前周期、危险位必须带原因、`mid=0` 与 `mid>0` 的语义分叉）、全量模式的批量上限与「只回前若干条」的回显形状、逐作者事务的**失败隔离**边界；确认只推 `state`、`payout_state` 一个字都不动，确认原因/幂等键当前**无处落库**（没有结算确认审计表）按现状钉住 |
| `settlement_read_test.go` | 27/9 | `GetSettlement`/`ListSettlements`：归属校验逐条钉死——`mid>0` 不一致回 `ErrForbidden` 且停在分项表之前（读轨迹逐项等长才判得出「停在哪」），`mid=0` 当前**完全没有归属校验**（原样钉住并登记 §7 结算域高危缺口）；「查不到」与「查不了」分开（DB 故障原始上抛，绝不折成 `found=false`，否则调用方会去重发结算）；`VOIDED` 单照样查得到且带 `void_reason`（作废是审计证据不是删除）、`state=0` 的列表默认**含 VOIDED**；出金边界——读侧只回 `payout_state` 原值、不执行任何写语句；排序事实源（分项 `ORDER BY source_type ASC`、列表 `ORDER BY settlement_id DESC`，替身照抄、种子反序插入）；现状钉住：状态越界复用了 `ErrInvalidRuleState` |

**D. 参与关系域（`cr_enrollment`）**

| 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- |
| `enrollment_test.go` | 17/4 | `EnrollCreator`/`LeavePlan`/`SetEnrollmentState`：合法前置状态、重复调用的幂等结论、时间戳三态（保持/写入/清零，`IF(? > 0, ?, 原值)` 语义）、确认版本只能单向前移，以及「暂停是运营处置、退出是合约关系」的分工 |
| `enrollment_read_test.go` | 16/5 | `GetEnrollment`/`ListEnrollments`：闸门必须发生在任何数据访问之前（`len(db.reads)==0` + `len(db.calls)==0`，非法 mid 不该烧掉一次查询）；「未参加 → `found=false`」与「读故障 → 原样上抛」必须分开（创作者端首页不能把故障渲染成「你还没加入计划」）；顺序期望抄 `model/cr_enrollment.go` 的 `ORDER BY mid ASC`，分页拼接与全量回读逐行比对；折出来的 offset/limit 必须真的进了查询（只看回显 page/size 判不出截断）；两入口请求里**没有调用方身份位** ⇒ 服务端无从校验「你是不是你」，按现状钉住 |

**E. 概览读侧与纯函数**

| 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- |
| `summary_read_test.go` | 22/8 | `GetRevenueSummary`/`ListRevenueMetrics`：`current_estimate_minor` 取的是**封顶后应计** `capped_amount_minor`，且 SQL 谓词只有 `period+mid`（既不过滤 `threshold_blocked` 也不看是否出过单）；`total_confirmed_minor` 的窗口由 `CreatorRevenue.SummaryRecentPeriods` 决定、边界含 cutoff，期望值用测试里独立推导的 `periodBack` 算（不复用被测的 `model.PeriodCutoff`，否则是拿实现当期望值的循环论证）；`last_settled_period` 的在效判据是 `void_seq=0` 而不是 `state`；出金红线——`payout_available` 恒 false 且结论必须由服务端 `payout_note` 给出；两请求无身份位 ⇒ 跨作者枚举只拿得到全量（现状钉住） |
| `helpers_test.go` | 14/0 | `helpers.go` 纯函数：入参闸门（宽度/必填/幂等键留白）、行→RPC 投影的**逐字段**一致（投影漏字段是「接口没报错但运营看不到」那一类最贵的 bug，用 `proto.Equal` 两侧同口径比较）、裁尾函数必须落在字符边界上 |

`fakes_test.go`（`0/0`）是替身层，见 §9.4，不是漏计的用例文件。

### 9.2 其他层

- `model`：`migration_parity_test.go` `28/14` —— 把 model 结构体 db tag / `SELECT` 列常量 /
  `INSERT` 列清单与 `deploy/migrations/creator-revenue` 的建表语句逐列双向比对（不连库）：
  金额列必须是 BIGINT 分单位（一旦改成 DECIMAL/FLOAT，`ComputeAmountMinor` 的
  「整数除法向下取整、余数丢弃」口径立即失真、误差方向不再恒定）；`uniq_rule_code`/
  `uniq_metric_key`/`uniq_active_period_mid`/`uniq_request_id` 必须是唯一键；参与唯一性判定的
  字符列必须 `utf8mb4_bin`（整数列不登记排序规则要求）；`payout_state` 与金额列的**注释**必须写明
  「恒 NOT_PAYABLE」「应计、不是已支付」这条范围红线；只追加台账、`void_seq` 复合唯一键逐条对齐。
- `internal/config`：`config_load_test.go` `3/1` —— 用 `conf.Load` **真实加载** `etc/` 下每个示例配置，
  拦住两类只有真实加载才暴露的问题：业务缓存字段命名成 `Redis`（与 `zrpc.RpcServerConf` 内嵌的
  `RedisKeyConf` 同名，代码可编译但启动瞬间 `conflict key redis`）、yaml 多写键被静默忽略或
  `Config` 有的键 yaml 漏写被 `default` 悄悄补上（护栏值配丢了没人发现）。
- `internal/svc`：**无离线单测**（只做依赖装配）。
- 本服务没有 `internal/repository`、`internal/consumer`、`internal/policy` 目录：
  数据访问在 `model`，无 MQ 消费者与 outbox（见 §7 第 1 条），无策略层。

### 9.3 构造器级覆盖

**16/16**：探针取 `internal/logic` 全部 `New*Logic(`（对应 §3 的 16 个 RPC 方法），
逐个在 `*_test.go` 里查引用，无缺口。事务内函数（`applyMetricInTx`/`applySettlementInTx`/
`applyRuleDraftInTx`/`applyRuleStateChange`/`applyEnrollmentTransition`）另有
`metricmutate`/`settlecompute`/`rulewrite`/`enrollment` 的入口级用例双重覆盖——
只测事务函数进不去「闸门在事务之外」这类结论，所以两组都存在。

### 9.4 替身层与断言口径

`internal/logic/fakes_test.go` 提供内存版 `cr_*` 表，**实现的是 `sqlx.Session`**：

- 为什么必须做到这一层：本服务写侧口径全部押在一个事务里，事务内用
  `model.NewXxxModel(sqlx.NewSqlConnFromSession(tx))` 绑定会话，只替换
  `svc.ServiceContext` 的 model 字段无法进入这些被测函数。
- 按语句签名把请求落到内存表，并复刻真库的**判定语义**：`uniq_mid`/`uniq_rule_code`/
  `uniq_metric_key`/`uniq_active_period_mid`/`uniq_request_id`/`uniq_no_source` 命中即回
  可被 `model.IsDuplicateErr` 识别的 `1062`；CAS 语句按真实 WHERE 求值，不命中即
  `RowsAffected=0`（DSN 禁 `clientFoundRows`，logic 就是把「0 行」当并发失败裁决的）；
  `cr_enrollment.Transition` 的 `IF(? > 0, ?, 原值)` 三态时间戳语义照抄；
  `cr_settlement.Void` 的 `void_seq = settlement_id` 照抄（释放槽位而不是删行）。
- 未实现的语句一律 panic，绝不返回空结果冒充「查不到」；`TransactCtx` 在回调报错时整体回滚快照，
  用于断言「主表与台账要么都在要么都不在」。

断言口径：三套幂等各按各的粒度（§4.1）用**基线差值**断；列表类期望序一律抄 model 的 `ORDER BY`
事实源并把种子**反序插入**，所以「logic 自己重排/把游标页错位」必红；故障类期望是
「原始错误上抛」而不是 `found=false`。

它**证明不了**：InnoDB 的 next-key 锁与真实并发（单测无法也不该复刻锁）、真驱动下 `RowsAffected`
的 matched vs changed 语义、SQL 文本与列名/索引是否命中（由 `model/migration_parity_test.go` 兜）、
`CacheRedis` 的实际读写（本服务读路径当前不用缓存，见 §7 第 9 条）。

### 9.5 覆盖边界（不可省略）

- 用例不连接 MySQL/Redis/etcd/MQ/对象存储，也不起真 gRPC server；`spm`/`coin`/`cron` 的调用方
  在测试里只是直接调用 logic 入口，跨服务的「谁上报折算量」这一事实不在本节证明范围。
- **出金路径在单测里不存在**：提现、打款、银行卡、发票、税务、对账本期都不实现
  （AGENTS.md §1、§5，见 §7 第 3 条），因此不是「被覆盖了」而是「没有这条路径」。
  用例能钉的是边界**不被越过**：计量与结算写侧不触碰任何 `payout_*` 写语句、
  读侧只回 `payout_state` 原值且 `payout_available` 恒 false。分成金额是「应计」，
  「已支付」语义在服务里不存在，`model.ErrPayoutNotAvailable` 当前没有调用点。
- 迁移 SQL 与真实库的列级对账：`deploy/migrations/creator-revenue/000001_*.sql` 已于 2026-09-22
  在隔离实例 `127.0.0.1:3399` 执行并复验（`go_video_creator_revenue` 8 张表 `applied`，见 §7 第 8 条）；
  但 `uniq_active_period_mid` + `void_seq=settlement_id` 的**作废重算路径**未在真实实例执行过，
  建表通过不等于该不变式已证明。真实/共享实例仍未执行，上线须由运维在目标实例跑迁移。
- `internal/server`、`rpc/*.pb.go`、`internal/svc` 与入口模板等 goctl 生成壳不在单测范围内。
- 本节未复核缺口清单，缺口的权威登记在 §7「已知缺口」（§7 第 4 条与本节口径一致）。

### 9.6 验证命令

```bash
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
go test -p 1 -count=1 ./services/creator-revenue/...
gofmt -l services/creator-revenue    # 必须无输出
go vet ./services/creator-revenue/...  # 必须无输出
```

`-p 1` 是硬要求：Windows 页面文件限制下并发编译/运行多个测试包会 OOM（errno=1455）。
§6 的完整门禁序列（build / vet / gofmt / test）与生成命令不变。
