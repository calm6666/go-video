# coin

硬币与投币服务（硬币 = **社区虚拟币，不是钱**），对应参考仓库的 `member/bcoin` 与 `video/coin` 口径。

- **拥有数据**：硬币余额、单用户日额度、投币记录、硬币流水台账（库 `go_video_coin`，表前缀 `cn_`）。
- **提供能力**：投币（扣币 + 限额判定 + 幂等）、窗口内取消投币并全额退回、我的投币记录、单内容/批量投币汇总、投币人列表、发放与扣回（运营/订单履约）、流水台账分页、生效限额回显。
- **对外契约**：`services/coin/rpc/coin.proto`（`coin.v1`，10 个 rpc 方法），etcd 注册键 `coin.v1.rpc`，监听 `0.0.0.0:8163`。
- **依赖**：MySQL（硬依赖，余额判定必须落库）、Redis（可选，仅缓存展示口径汇总）。**不依赖 MQ、不依赖任何下游 RPC**。
- **owner**：后端-商业化小组（本轮由主 agent 落地，正式 owner 待登记到 `docs/service-catalog.md`）。

## 1. 数据所有权（AGENTS.md §5）

本服务是**硬币余额的唯一写入口**。硬币与 `payment` 的现金余额是**两套完全分开的账**：

| | 硬币（coin） | 现金（payment） |
|---|---|---|
| 语义 | 社区虚拟币，用于给内容投票 | 沙箱资金台账 |
| 获得 | 仅两条：运营发放（`GrantCoin`，`ADMIN_GRANT`）、买硬币包（`trade-order` 履约时调 `GrantCoin`，`ORDER_PACK` + 订单号） | 充值（沙箱台账） |
| 消耗 | 仅一条：投币 | 支付/退款 |
| 互换 | **不互换、不换算、不共表**：本服务不 import payment 的 rpc，也不直连其库表 | 同 |

全链路不涉及真实资金，没有任何出金/提现/退款到卡能力（AGENTS.md §1 2026-09-22 修订记录）。

禁止的越权写法：`video`/`engagement` 持有硬币余额或自行扣币；`creator-revenue` 把硬币当成收益金额；任何服务直读 `cn_*` 表。
`video.coin_count`、`engagement` 侧的投币计数都只是**投影**，只能由本服务的 `GetTargetSummary`/`BatchGetTargetSummary` 供数（本轮接线尚未落地，见 §7）。

记账不变式（对账与排障的唯一依据）：

```text
cn_account.balance == SUM(cn_flow.delta) WHERE mid = 同一 mid
```

因此「新建账户送初始币」也会写一条 `flow_type=ADMIN_GRANT`、`biz_no=INITIAL_BALANCE`、
`request_id=coin:init:<mid>` 的流水；`request_id` 由 mid 唯一决定，重复建仓在唯一索引上直接挡死。
排查某用户余额对不上时：`SELECT SUM(delta) FROM cn_flow WHERE mid = ?` 与 `cn_account.balance` 比对。

## 2. 表清单（`deploy/migrations/coin/000001_create_coin_tables.sql`）

| 表 | 作用 | 关键约束/索引 |
|---|---|---|
| `cn_account` | 余额唯一真值：`balance`/`total_tossed`/`version` | `PRIMARY KEY(mid)`；同一用户所有写操作的行锁串行点 |
| `cn_daily_toss` | 单用户单日投币量，一行一天 | `UNIQUE(mid, date)`，`date` 为 `YYYYMMDD` 整数 ⇒ **跨日天然重置**，不做手工清零 |
| `cn_toss` | 一人对一内容的投币聚合（事实表，不是投影） | `UNIQUE(mid, target_aid)`；`state` 1 ACTIVE / 2 CANCELLED（取消保留行作审计证据） |
| `cn_flow` | append-only 硬币台账 | `UNIQUE(request_id)` 幂等锚点；只 INSERT，禁止 UPDATE/DELETE |

**没有 `coin_count` 汇总表**：`GetTargetSummary` 与 `BatchGetTargetSummary` 直接
`SELECT target_aid, SUM(\`count\`), COUNT(*) FROM cn_toss WHERE state=1 AND target_aid IN (...) GROUP BY target_aid`。
多存一份计数就多一个会漂移且无法自愈的事实源。

参与唯一性/精确匹配的列（`request_id`、`last_request_id`、`biz_no`）一律列级 `utf8mb4_bin`，
其余文本列表级 `utf8mb4_unicode_ci`：若唯一键走大小写折叠的 collation，两个仅大小写不同的
`request_id` 会被判为重复，其中一次**真实扣币会被静默丢弃**。

## 3. 限额与取消窗口口径

生效值全部来自配置（`Coin.*`），并由 `GetTossConfig` 回显；客户端不得写死（AGENTS.md §6）。

| 口径 | 默认 | 判定位置 |
|---|---|---|
| 每日可投上限 `DailyLimit` | 10 | `cn_daily_toss` 条件累加 `UPDATE ... WHERE tossed + ? <= ?` |
| 单内容累计上限 `PerTargetLimit` | 2 | `cn_toss` 条件累加 `UPDATE ... WHERE state=1 AND \`count\` + ? <= ?` |
| 取消窗口 `CancelWindowSeconds` | 86400（自 `last_tossed_at` 起算） | 事务内以锁定行重判 |
| 投币余额门槛 `MinBalanceToToss` | 1 | 条件扣减 `WHERE balance >= max(count, MinBalanceToToss)` |
| 单次发放/扣回绝对值 `MaxGrantDelta` | 1000 | `GrantCoin` 入参校验 |
| 分页上限 `MaxPageSize` / 默认 `DefaultPageSize` | 100 / 20 | `ServiceContext.PageSize`，超上限**报错不截断** |
| 批量汇总上限 `MaxBatchAids` | 50 | `BatchGetTargetSummary` 去重后裁剪（多屏列表页宁可少给几条） |
| 新建账户初始币 `InitialBalance` | 5 | 首次写操作（投币或发放）时懒建仓 |

`TossCoin` 在**一个事务**内按固定顺序完成，任何一步不满足都整体回滚：

```text
① cn_account 建仓/SELECT ... FOR UPDATE   ② request_id 复核（事务内）
③ cn_toss 行锁（只锁不改，先定锁序）        ④ cn_daily_toss 条件累加（日限）
⑤ cn_account 条件扣减（余额）               ⑥ cn_toss 落库（单片上限）
⑦ cn_flow 追加（负 delta）
```

- 锁顺序在投币/取消两条路径上完全一致（账户 → 投币记录 → 日额度 → 台账），写反就会死锁。
- 事务内**不调用任何外部 RPC / MQ**，持锁时间就是 4~7 条语句的长度。
- 不同用户互不阻塞；同一用户的并发被账户行锁串行化。

投币被拒的四类原因都是**业务结论**（`accepted=false` + `reason` + `reject_detail`），不是 gRPC 错误：
`TOSS_REJECT_INSUFFICIENT_BALANCE` / `DAILY_LIMIT` / `TARGET_LIMIT` / `TARGET_INVALID`。
只有入参非法（`mid<=0`、`request_id` 为空）才返回错误。

取消投币：

- 窗口内才允许，退回该记录 `count` 枚（余额 +`count`、一条 `CANCEL_TOSS` 正向流水、`cn_toss` 置 CANCELLED、
  `cn_daily_toss` 回退最后投币日的计数），同事务。
- `total_tossed` **不回退**——proto 把它定义为历史口径（曾经投出去过多少枚）。
- 超窗返回结论而不是静默成功；重复取消一律 `duplicated=true` 且不再退币。
- 取消后重新投币会在同一行「复活」（唯一键不变），`count` 被本次枚数**覆盖**而非累加，
  否则退回过的币会被再算一次，凭空放大 `coin_count`。
- **跨日取消的已知简化**：只回退 `last_toss_date` 那一个日桶，并用 `GREATEST(tossed - count, 0)` 夹底。
  即「先投 1 枚（昨天）+ 再投 1 枚（今天）后取消」会把今天的额度回退 2 枚（若今天不足则夹到 0），
  不会凭空多出昨日额度，但可能多给今日额度。窗口只有 24 小时且 `PerTargetLimit` 很小，影响面有限。

## 4. 幂等

- 三类写接口（`TossCoin`/`CancelToss`/`GrantCoin`）都要求 `request_id`，唯一索引
  `cn_flow.uniq_request_id` 是最终防线（Redis 不承担幂等职责）。
- 重放路径：先查 `request_id` 对应流水（快路径）→ 命中则**返回首次结论**并置 `duplicated=true`，
  绝不重复扣币；事务内再复核一次，堵住「查键 → 扣币 → 写键」之间的并发窗口
  （同 `request_id` 必然同 `mid`，而 `mid` 的账户行锁已在第一步持有）。
- **同 `request_id` 参数不同**（不同 mid/aid/枚数/流水类型/订单号）→ `ErrIdempotencyConflict` 报错，
  不任选一份结论：那是调用方把幂等键用串了，静默复用会吞掉用户真实的一枚币。
- 被拒的投币不写流水（没有任何余额变动），所以「拒绝的重放」由服务重新判定，
  结论可能随时间变化（例如跨过午夜后 `DAILY_LIMIT` 变为可投）；这是可接受的，
  因为拒绝本身无副作用。`duplicated` 只在真实变更被复用时才为 true。

## 5. 配置

`services/coin/etc/coin.v1.yaml`（键名与 `internal/config/config.go` 一一对应，
由 `internal/config/config_load_test.go` 在 CI 拦住漂移）：

```yaml
Name: coin.v1.rpc
ListenOn: 0.0.0.0:8163
Etcd: { Hosts: [127.0.0.1:2379], Key: coin.v1.rpc }   # 注册键不带连字符
CacheRedis: { Host: 127.0.0.1:6379, Type: node }      # 必须叫 CacheRedis，叫 Redis 会 conflict key redis
DataSource: root:root@tcp(127.0.0.1:3306)/go_video_coin?charset=utf8mb4&parseTime=true&loc=Local
Coin: { ... }                                          # 见 §3 表格
```

两个必须守住的 DSN/配置细节：

- **不得加 `clientFoundRows=true`**：条件更新与幂等全部依赖 `RowsAffected = 实际变更行数`，
  改成匹配行数会让「余额不足」「重复请求」判定整体失真（迁移 SQL 里也记了这条）。
- `loc=Local` 与日桶 `YYYYMMDD`（`model.DayNo` 用服务本地时区）必须同源，否则服务端和 DB 里的
  「今日」不是同一天。
- 生产密码进 Secret/Vault，不提交到 `etc/`。

## 6. 运行

```powershell
# 1) 迁移（见 docs/commands.md §8）
./scripts/migrate.ps1 -Action up -Service coin

# 2) 契约变更后重新生成 goctl 代码（.proto 是唯一来源，禁止手改生成物）
./scripts/gen.ps1 -Service coin

# 3) 启动
go run ./services/coin -f services/coin/etc/coin.v1.yaml

# 4) 本服务门禁
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
go build ./services/coin/... && go vet ./services/coin/... && gofmt -l services/coin
go test ./services/coin/...
```

## 7. 已知缺口（尚未实现，禁止当成已交付）

1. **不校验稿件存在性**：`target_aid <= 0` 才判 `TOSS_REJECT_TARGET_INVALID`；
   aid 是否为合法已发布稿件由网关/详情页与 `video` 侧保证。本服务不越权调 `video` RPC 反查
   （proto 注释已锁定该口径）。副作用：可以向不存在的 aid 投币并扣币，需要由网关侧鉴权补齐。
2. **`coin_count` 投影未接线**：`video`/`engagement` 侧的投币计数投影本轮不由本服务写
   （跨域写入违反 §5），也没有事件供数。当前消费方必须直接调 `GetTargetSummary`/`BatchGetTargetSummary`。
3. **不写 outbox、不发 MQ**：`coin.tossed.v1` 之类事件尚未定义 schema，投币后的推荐/热度联动、
   创作中心统计都拿不到增量。接线时必须补 `api/events` schema、生产者与按 `event_id` 去重的消费者。
4. **风控与作者自投恒不触发**：`TOSS_REJECT_RISK_BLOCKED`（`risk-control` 未接线）与
   `TOSS_REJECT_SELF_TOSS`（需要 `video` 侧作者身份，未接线）是 proto 里的预留位，
   本服务**永远不会返回**这两个值。接线前不要在客户端为它们写分支逻辑。
5. **无过期能力**：`COIN_FLOW_TYPE_EXPIRE` 只是枚举占位，没有过期任务，`cn_flow` 里永不出现该类型。
6. **契约缺口收口情况（2026-09-22 主 agent 已改 `.proto` 并重新生成）**：
   - `CancelTossReply.reject_detail = 7` 已补，取消被拒时给「已超过 N 秒撤币窗口」这类可读结论；
   - `TossRejectReason` 已新增 `TOSS_REJECT_CANCEL_WINDOW_EXPIRED = 8`，超窗不再借用 `TARGET_LIMIT`；
   - `GrantCoinReply` **刻意不加** `accepted/reason`：发放只由服务身份调用，「扣回会穿底」是调用方的
     编程错误而不是用户可见结论，用 gRPC 错误上抛才不会让 trade-order 把失败当成记账成功；
   - `BatchGetTargetSummaryReq` 已补 `reserved 1;`，编号不复用的意图写进契约。
7. **真库集成仍缺**（logic/model 单测已于 2026-09-22 补齐：`internal/logic` 6 个文件 `91/29`、
   `model` 2 个文件 `30/10`、`internal/config` 1 个文件 `4/0`，明细见 §8「测试覆盖」）：
   余额条件扣减、日限跨日、幂等重放/冲突、取消窗口边界都由内存替身 + 可注入时钟（`model.SetClockForTest`）钉住，
   不连 MySQL。因此唯一索引、`CHECK` 约束与 `RowsAffected` 在真实驱动下的语义仍未验证，需要容器化集成测试补齐。
8. **`cn_flow` 无归档执行者**：台账只增不删，冷热分层/归档需要 `services/cron` 落地任务与保留窗口配置。

## 8. 测试覆盖

离线单测（纯 Go 内存替身 + 可注入时钟，不连 MySQL/Redis/etcd/MQ）。数字由
`grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，6 个文件 `91/29`）

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **余额写入口（AGENTS.md §5 的唯一扣币/退币/发放路径）** | | | |
| `tosscoin_test.go` | 22 | 3 | §3 的事务锁序与「四类拒绝都是业务结论而不是 gRPC 错误」；畸形入参不碰库；`target_aid<=0` 是结论；`count<=0` 归一为 1；单次就超 `PerTargetLimit` 时直接跳过事务；余额不足/日限/单片上限三类失败**整体回滚**；日限拒绝要回剩余配额、日额读故障只降级 detail；跨日天然重置；已取消行「复活」时 `count` 被覆盖而不是累加；库里出现未知 `state` 要如实上报而不是忽略；`request_id` 重放返回首次结论且不二次扣币、参数不同判冲突、并发重放判 `duplicated`、回滚后键消失是错误而不是成功；提交后回读失败仍报 accepted |
| `canceltoss_test.go` | 15 | 2 | 窗口内全额退回且余额/流水/投币状态/日额度必须同事务；只能撤生效中的记录；重复取消 `duplicated=true` 不再退币；窗口边界与「锁定行上重判窗口」（不是入参时刻）；事务内记录消失；台账写失败整体回滚；跨日取消只回退最后投币日且 `GREATEST(...,0)` 夹底；`total_tossed` 不回退（proto 锁定的历史口径）；代客取消必须给理由并留痕；结论回读失败原样上抛 |
| `grantcoin_test.go` | 18 | 4 | 「哪些流水类型能从这儿进来」就是数据所有权边界——伪造的 `flow_type` 一律拒；`operator` 恒必填、`ADMIN_GRANT` 要理由、`ORDER_PACK` 要订单号；`delta` 绝对值上限；发放/扣回在单事务内生效；懒建仓保留 `INITIAL_BALANCE` 首笔流水；扣回不得把余额写成负数（可精确扣到 0）；幂等重放/参数冲突/与投币流水撞键/超长 `biz_no` 仍是重放；`request_id` 读失败上抛；台账写失败回滚余额；回显用变更后的快照；超长凭据是裁剪而不是拒绝 |
| **读侧与投影** | | | |
| `queries_test.go` | 20 | 14 | 两条核心：读失败必须上抛、不能折叠成「余额 0 / 空台账 / 没投过币」（把故障伪装成业务事实）；分页与批量裁剪口径——超上限的 `size` 报错不静默截断，超上限的 `aids` 裁剪而不是让整屏 feed 失败。另有「看一眼余额不许顺手建仓」、`GetTossConfig` 回显生效值、`ListMyTosses`/`ListTargetTossers`/`ListCoinFlows` 的过滤与分页、`ListCoinFlows` 拒绝无界扫描、读路径永不进事务、无缓存时回落 MySQL、读路径用注入时钟的「今日」 |
| `conv_test.go` | 16 | 6 | 投影层纯函数：无账户时也回显限额、行字段逐项投影、nil 行处理、契约字段全覆盖、未知枚举折叠而不是透传、`validFlowType` 只认枚举集合、初始建仓流水自述、`remark`/id 按 rune 而非字节截断、`aids` 先去重再裁剪、汇总永远回结构体、汇总缓存 key 带命名空间且缓存关闭路径可达、缓存字段名不漂移、分页与 offset 口径 |
| **替身层（无用例）** | | | |
| `fakes_test.go` | 0 | 0 | 内存版 model 与假事务，见第 4 组 |

### 2. 其他层

- `model`（2 文件 `30/10`）：
  - `model_rules_test.go` `18/2`：「不需要数据库就能定死」的运行期折叠口径——校验必须发生在 SQL 之前
    （每个写入函数都断言「返回哨兵错误且一条 SQL 都没发出去」，否则 1406/1366 会让整笔已判定成功的
    扣币事务回滚）；`RowsAffected`/`ErrNoRows` 的折叠是 logic 全部结论的真值来源（取不到 `RowsAffected`
    当 0 会把成功扣减误判成「余额不足」，把读失败塌成 `(nil, nil)` 会把「库不通」冒充「这人余额 0」）；
    INSERT 实参顺序逐列比对；`FOR UPDATE` 只许在事务里跑。`fakeSQL` 同时充当 `sqlx.SqlConn` 与
    `sqlx.Session`，未预期的调用一律 panic（宁炸不默）。
  - `migration_parity_test.go` `12/8`：把「结构体 db tag / SELECT 列常量 / 条件更新 SQL ↔
    `deploy/migrations/coin` 建表语句」变成可执行门禁（迁移文件只读，不改任何 SQL）——
    `cn_flow.uniq_request_id`、`cn_account.PRIMARY(mid)`、`cn_daily_toss.uniq_mid_date` 这些键的存在性，
    `request_id`/`biz_no` 必须列级 `utf8mb4_bin`（表级 unicode_ci 会把 `'ABC'`/`'abc'` 判成同一个键、
    静默丢单）、整型宽度与 Go 字段匹配（`target_aid` 建成 INT 会让超出 21 亿的 aid 溢出成负数）、
    条件更新 WHERE 子句文本一致。
- `internal/config`（1 文件 `4/0`）：`TestLoadReleaseYaml`（etc yaml 可完整解析，服务名/端口与
  `docs/service-catalog.md` 登记值一致，期望值直接引用 `Sanitize` 的兜底常量，避免「文档说 10、兜底给 20」）；
  `TestYamlKeysMatchStructFields`（yaml 键 ↔ Config 字段一一对应，多写的键会被 `conf.Load` 静默忽略——
  运维以为改了限额其实没改）；`TestMinimalYamlAppliesDefaults`（只写必填项时 `default` 标签全部生效）；
  `TestSanitizeRejectsUnusableLimits`（限额配成非正数必须收敛到可用值）。
- 本服务**没有** `internal/repository` 目录（model 接口直接挂在 `svc.ServiceContext` 上），
  因此也不存在该层的单测；`internal/svc/` **无离线单测**（分页收敛等口径由 `conv_test.go`/`queries_test.go`
  经装配间接经过）。
- 本服务没有 `internal/consumer`、`internal/policy` 目录（不写 Outbox、不发 MQ，见 §7.3）。

### 3. 构造器级覆盖

`10/10`：探针取 `internal/logic` 全部 `New*Logic(`（10 个，与 `coin.proto` 的 10 个 rpc 方法一一对应），
逐个在 `*_test.go` 里查引用，`gaps:` 为空。

### 4. 替身层与断言口径

`internal/logic/fakes_test.go` 提供内存版 model 与假事务，注入点不改实现：

- `svc.ServiceContext` 的四个 model 字段是接口类型（`AccountModel`/`DailyTossModel`/`TossModel`/`FlowModel`），
  测试直接赋值即可，不需要连 MySQL。
- 复刻的是**语义**而不是锁：唯一键命中即 `created=false` / 插入报 1062；条件 UPDATE
  （`balance >= ?`、`tossed + ? <= ?`、`count + ? <= ?`、`state = ?`）不命中即返回 false；
  回退用 GREATEST 夹底到 0。断言的是 logic 面对这些返回值的裁决顺序。
- 余额写口（`DeductForTossTx`/`RefundForCancelTx`/`ApplyGrantTx`/`Flows.InsertTx`）在 session 为 `nil`
  时当场 panic——「硬币余额只能通过本服务事务内的写接口变动」这条数据所有权边界在单测里就是响的。
- 每个假实现只嵌入接口并覆写被测路径用到的方法，其余方法由内嵌的 nil 接口提升，
  测试走到未实现分支立刻 panic，失败不会被写成通过。
- 它证明不了什么：真库的行锁与并发交错、唯一索引与 `CHECK` 约束的实际生效、`RowsAffected` 在真实驱动下
  是 matched 还是 changed（§5 已写明 DSN 不得加 `clientFoundRows=true`，但这条只能在真库验证）、
  跨日 `DayNo` 与 DB 时区是否同源、Redis 缓存 TTL 与失效。这些都留给容器化集成测试（§7.7）。

### 5. 覆盖边界

- 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储，也不起 gRPC 服务端；时钟由
  `model.SetClockForTest` 注入。
- **0 条用例处于 skip**（主代理实测口径）；不要把用例文件数当成断言都在跑。
- 迁移 SQL 与真实库的列级对账由 `model/migration_parity_test.go` 静态完成（不连库）；
  迁移文件本身在隔离实例 `127.0.0.1:3399` 的 `up` + `status` 由 `deploy/migrations/README.md`
  登记为 `coin | go_video_coin | 1 | applied`，真实/共享实例未执行，本 README 不把它升级成已验证。
- `coin_count` 投影未接线、不发 MQ 事件、风控与作者自投恒不触发（§7.2/7.3/7.4），
  因此「投币后热度联动」「被风控拦下的投币」不在离线覆盖内。
- `internal/server/coinserver.go`、`rpc/*.pb.go` 等 goctl 生成壳不在单测范围内（本服务无 HTTP handler）。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/coin/...
gofmt -l services/coin           # 必须为空
go vet ./services/coin/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。
