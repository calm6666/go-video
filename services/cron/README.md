# cron

全仓**定时任务与补偿任务**的调度、租约、重试与审计所有者（gRPC，无 .api）。

- 依赖：`AGENTS.md` §3（定时任务集中在 `services/cron`）、§5（数据所有权）、`docs/commands.md` §5/§8。
- 契约：`rpc/cron.proto`（`cron.v1`，23 个方法）。
- 数据：`go_video_cron` 库 5 张自有表，迁移脚本 `deploy/migrations/cron/`。
- 本轮范围：**契约 + model + 迁移 SQL + goctl 生成 + 配置装配** + logic 实现与离线单测
  （23 个 RPC 方法均有构造器级用例，清单与口径见 §9），不返回假成功的零值响应；
  **调度循环与任务处理器仍未落地**（§8 缺口 1、2），服务当前只提供 RPC。

## 1. 边界（不可协商）

| 允许 | 禁止 |
| --- | --- |
| 读写 `go_video_cron` 的 5 张调度表 | 直连/读写其他服务的库、表、Redis key |
| 通过下游领域 RPC 推进业务状态 | import 其他服务的 `internal/model`、`internal/logic` |
| 记录任务定义/执行/租约/游标/审计 | 把业务数据或事件正文塞进 `params`/`result_summary` |
| 用 `CacheRedis` 做到期索引/计数加速 | 用 Redis 当调度真值源（真值恒在 MySQL） |

cron **不订阅 MQ**：任务由 MySQL 到期扫描驱动（§5）。需要「事件驱动的补偿」时，
由事件消费者所属服务落任务定义，cron 只按点触发——否则同一个业务动作会有两个驱动源。

## 2. 方法表（`rpc/cron.proto`，23 个）

### 任务定义（8）

| 方法 | 用途 | 关键约束 |
| --- | --- | --- |
| `RegisterTask` | 注册任务定义 | `task_key` 唯一；重复注册幂等返回 `created=false`；必须过 `ValidateTaskDefinition` + 注册表校验 |
| `UpdateTask` | 改调度/超时/重试/参数 | `expected_version` 乐观锁；`state`/`version` 以服务端为准；冲突报 `ErrVersionConflict` |
| `GetTask` | 查单个定义 | 不存在报 `ErrTaskNotFound`，不返回空投影 |
| `ListTasks` | 分页列定义 | cursor 优先；`page_size` 越界报 `ErrInvalidPageLimit` |
| `PauseTask` | 暂停（可恢复） | `reason` 必填；清空 `next_fire_at`；正在跑的执行不打断 |
| `ResumeTask` | 恢复 | 按 `misfire_policy` 重算 `next_fire_at` |
| `DisableTask` | 停用（终态） | 保留执行记录与游标；恢复只能重新注册 |
| `TriggerTask` | 立即触发/补跑某计划时刻 | `idempotency_key` 必填；同秒重复点击被唯一键挡回 |

### 到期扫描与租约（4）

| 方法 | 用途 | 关键约束 |
| --- | --- | --- |
| `ListDueTasks` | 拉到期清单 | 纯读，不写 run/lease；返回 `server_time` 供时钟校正 |
| `AcquireLease` | 抢占某计划时刻 | 事务内 `INSERT cron_task_run` + `SELECT ... FOR UPDATE` 抢租约；5 种 `LeaseOutcome` |
| `RenewLease` | 心跳续租 | `fence_token` 不一致 → `ErrLeaseLost`，调用方必须停止写下游 |
| `ReleaseLease` | 领取后放弃执行 | 只允许 `SKIPPED`/`CANCELED`；`fence_token` 不回退 |

### 结果与游标（4）

| 方法 | 用途 | 关键约束 |
| --- | --- | --- |
| `ReportTaskResult` | 上报结果（可带游标 CAS） | 重复上报终态 → `first_reported=false`；`FAILED` 按策略转 `RETRYING` 或终态 |
| `GetCheckpoint` / `ListCheckpoints` | 读增量游标 | 从未推进时 `found=false`，不是错误 |
| `SaveCheckpoint` | 独立推进游标 | `expected_version` CAS；冲突 `ErrCheckpointConflict` |

### 查询与运维（7）

| 方法 | 用途 |
| --- | --- |
| `GetTaskRun` / `ListTaskRuns` | 执行记录点查与倒序游标分页 |
| `RetryRun` | 人工重试已终结执行（同 `(task_key, planned_at)` 追加 `attempt`） |
| `GetLease` / `ListLeases` | 租约快照（含「已过期可抢占」视图） |
| `ListTaskAudits` | 调度面变更审计（只追加，无更新/删除接口） |
| `GetSchedulerHealth` | 分组健康度：积压/运行中/退避/近一小时失败/过期租约 + 本进程可跑的 handler 清单 |

## 3. 表与迁移

| 表 | 迁移 | 幂等/索引要点 |
| --- | --- | --- |
| `cron_task_definition` | `000001_create_cron_task_tables.sql` | `UNIQUE KEY uniq_task_key(task_key)`；`idx_state_next_fire` 支撑到期扫描 |
| `cron_task_audit` | `000001_create_cron_task_tables.sql` | 只追加；`idx_task_ctime`/`idx_action_ctime` |
| `cron_task_run` | `000002_create_cron_run_lease_checkpoint_tables.sql` | `UNIQUE KEY uniq_fire_attempt(task_key, planned_at, attempt)` = 执行幂等身份 |
| `cron_task_lease` | `000002_...sql` | `UNIQUE KEY uniq_lease_key(lease_key)`；`idx_expire` 支撑抢占与过期巡检 |
| `cron_task_checkpoint` | `000002_...sql` | `UNIQUE KEY uniq_task_scope(task_key, scope_key)`；CAS `version` |

时间列统一 Unix 秒（`BIGINT`）。迁移脚本头部含用途/负责人/回滚/锁风险，全部
`CREATE TABLE IF NOT EXISTS`，可重复执行。执行方式见 `deploy/migrations/README.md`
与 `docs/commands.md` §8：**只能在隔离实例 `127.0.0.1:3399` 上跑，禁止连本机 3306**。

## 4. 状态机与租约语义

### 执行记录状态机（`model.CanTransition`）

```
PENDING ──► RUNNING ──► SUCCEEDED | RETRYING | FAILED | TIMEOUT | CANCELED | SKIPPED
   │           ▲            ▲
   │           └────────────┘  退避到期后由同一计划时刻的新 attempt 承接
   └──► SKIPPED | CANCELED | TIMEOUT | FAILED
```

- 终态（`SUCCEEDED/FAILED/TIMEOUT/CANCELED/SKIPPED`）**没有出边**：重试与人工重放都是
  追加新的 `attempt` 行，绝不把终态改回运行中 —— 历史轨迹不可篡改。
- `RETRYING` 是「本次尝试失败但还有重试机会」，`next_retry_at` 由
  `min(retry_base * 2^(attempt-1), retry_max)` 算出。

### 租约：可抢占 + TTL + 栅栏令牌

1. `AcquireLease` 在 `TransactCtx` 里对 `cron_task_lease` 做 `SELECT ... FOR UPDATE`，
   三分支：无人持有（首写 `fence_token=1`）/ 自己已持有（只续期，幂等重入）/
   `expire_at <= now` 可抢占（`fence_token+1`、`takeover_count+1`）。
2. 被抢占的旧实例之后任何一次 `RenewLease`/`ReportTaskResult` 都会因 `fence_token`
   不一致拿到 `ErrLeaseLost`，**必须立刻停止写下游**；`Lease.AbortOnLeaseLost=true`
   时调度侧同时取消处理器 ctx。
3. `Release` 只把 `expire_at` 归零、清 `owner_instance`，`fence_token` 永不回退，
   避免令牌复用让旧实例「复活」。
4. TTL 由 `svc.LeaseTTL()` 收敛到 `[Lease.MinTTLSeconds, Lease.MaxTTLSeconds]`，
   且要求 `HeartbeatSeconds*3 < TTL`；不满足直接报错，不静默降级。
5. 多副本无需选主：每个副本都 tick，冲突由「唯一键 + 租约 + 栅栏」在数据库层裁决。

### 幂等与重放

- **一次计划触发 = `(task_key, planned_at)`**；`UNIQUE(task_key, planned_at, attempt)`
  保证并发 claim 只有一个赢家。
- 处理器必须以 `(task_key, planned_at)` 作为幂等上下文：下游写接口一律带
  `idempotency_key`（用 `task_key:planned_at:attempt` 派生）。
- 增量型任务（归档、报表、索引重建、死信重放）用 `cron_task_checkpoint` 的 CAS 游标
  记录已处理水位。因此**重放同一计划时刻只会重新推进游标**：已推进的区间被游标挡住，
  只有未完成部分继续向前收敛，不会产生第二次副作用。
- 结果与游标在**同一个事务**里落地（`ReportTaskResult`），不存在「副作用完成但游标没前进」
  或「游标前进了但结果没上报」。

## 5. 任务注册表（`internal/registry`）

进程内 `handler` 名 → Go 实现的映射，是「DB 调度事实」与「代码能力」的接缝。

```go
registry.New().MustRegister(registry.Spec{
    Name:             "index.alias_patrol",   // = cron_task_definition.handler
    Description:      "巡检搜索索引别名并回收旧索引",
    SerialOnly:       true,                   // 全集群串行：concurrency_limit 必须为 1
    SuggestedTimeout: 120,                    // DB 未配 timeout_seconds 时兜底
    SuggestedMaxAttempts: 3,                  // DB 配置不得高于此值
    MinLeaseTTLSeconds: 120,                  // 代码侧下限，可高于全局 MinLeaseTTLSeconds
    RequiresDownstream: []string{"SearchIndexerRPC"},
    Idempotent:       true,                   // 以 (task_key, planned_at)+游标可安全重放
    // handler: 第二轮注入（registry.Register 要求同时给出实现）
})
```

- `Input`：`RunID/TaskKey/PlannedAt/Attempt/FenceToken/Params/SecretRefs/Operator/
  TriggerType/TraceID/Timeout/LeaseTTL`；`Output`：`ResultSummary/Skipped/SkipReason/
  Retryable/Checkpoint+ExpectedCheckpointVersion`。
- `Resolve`/`ValidateDefinition`/`EffectiveTimeout`/`Run` 是调度侧唯一入口。
- **同名重复注册直接失败**（`ErrHandlerDuplicate`），不覆盖；`SetHandler` 只能给已登记
  条目补实现。
- DB 有定义而进程无实现 → `model.ErrHandlerNotRegistered`，执行判失败并进退避，
  `GetSchedulerHealth` 里可见（AGENTS.md §9：不允许静默跳过）。
- 处理器只依赖 `registry` 的值对象与注入的下游接口，不依赖 `*ServiceContext`，
  可脱离 gRPC 单测。

## 6. 配置（`etc/cron.v1.yaml` + `internal/config`）

| 段 | 作用 | 关键项 |
| --- | --- | --- |
| `CacheRedis` | 到期索引/健康度计数加速副本 | **必须叫 `CacheRedis`**：叫 `Redis` 会与 `zrpc.RpcServerConf` 内嵌字段冲突，代码可编译但启动即 `conflict key redis` |
| `DataSource` | `go_video_cron` DSN | 密钥只从环境变量/配置中心注入 |
| `Scheduler` | tick 与批量 | `Enabled`、`TickSeconds=5`、`BatchSize=64`、`MaxConcurrentRuns=8`、`ReclaimIntervalSeconds=60` |
| `Lease` | 租约硬约束 | `HeartbeatSeconds=20`、`DefaultTTLSeconds=300`、`Min/MaxTTLSeconds`、`PreemptionEnabled`、`AbortOnLeaseLost` |
| `Task` | 业务参数 | 分页上下界、`RunRetentionDays=30`、`AuditRetentionDays=180`、`DeleteBatchSize`、`DefaultTimezone` |
| `*RPC` × 7 | 下游领域服务 | `optional`：`Target`/`Etcd.Hosts`/`Endpoints` 全空则不构造客户端 |

`internal/config/config_load_test.go` 会用真实 `conf.Load` 加载 `etc/` 下**每个** yaml，
并校验：默认值真的落到字段上（tick/心跳与 TTL 的比例、分页与留存天数上下界顺序）、
最小配置（不含任何下游 RPC）可加载、完整示例配置里的下游客户端都能被识别为已配置。

**MQ 段刻意不存在**：cron 不消费也不生产事件（§1）。第二轮若引入「任务完成事件」，
需先补 `common/eventenvelope` 的事件类型定义与 `docs/api-and-events.md` 的 topic 约定。

## 7. 下游依赖（只用已存在的 RPC 契约）

| 配置键 | 计划任务（第二轮） | 用到的方法 |
| --- | --- | --- |
| `RightsRPC` | `rights.expire_scan` | `ListExpiring` → `ExpireWindow` |
| `CatalogRPC` | `catalog.window_offline` | `OfflineEpisode` / `PublishEpisode` |
| `VideoRPC` | `submission.state_sweep` | `ListByState` → `TransitionState` |
| `SearchIndexerRPC` | `index.rebuild_stale`、`index.alias_patrol` | `SubmitRebuildTask` / `GetRebuildTask` / `SwitchAlias` |
| `NotificationRPC` | `notification.deadletter_replay` | `ListDeadLetters` → `RetryDeadLetter` |
| `EngagementRPC` | `engagement.count_repair` | `RawStat` → `UpdateCount` |
| `InboxRPC` | `inbox.unread_recompute` | `RecomputeUnread` |

下游未配置时的行为：任务以 `model.ErrDownstreamNotConfigured` 失败并进退避，
`DownstreamNotes()` 在启动日志里逐条说明。**绝不退化成「绕过 RPC 直接写别人的库」**。

## 8. 已知缺口

1. **调度循环未实现**（第二轮）：`NewServiceContext` 里尚未启动 tick worker 与孤儿执行
   回收器。接入方式与 inbox 一致——`threading.GoSafeCtx` 启动循环 +
   `proc.AddWrapUpListener` 收尾，因此 goctl 生成的入口 `cron.v1.go` 无需手改。
   当前 `Scheduler.Enabled`/`ReclaimIntervalSeconds` 等配置已可加载并被测试校验，
   但服务实际只会提供 RPC，不会自动跑任务。
2. **任务处理器为空**：`internal/registry` 只有注册表结构与单测，没有任何 `Spec` 注册；
   上表 7 个 `task_key` 是计划清单，不是已实现能力。
3. **跨服务契约缺口**（`// 契约缺口`，均在本轮 README 记录，不改他人代码）：
   - `feature-store`（特征快照重建）与 `spm`（埋点配置刷新）尚未实现，
     cron 不预置其 RPC 配置，等对应服务落地后新增 `*RPC` 段。
   - 缺少「任务完成事件」契约：报表类任务若要让下游感知完成，需要
     `common/eventenvelope` 定义 `cron.task.completed` 一类事件（当前不做）。
   - `recommend-*`、`live-*`、`private-message`、`open-platform` 未实现，
     相关清理/补偿任务本轮不登记。
4. **分页口径**：本服务统一用 cursor（`docs/api-and-events.md` §2「cursor 优先」），
   而阶段 1 之前的一些 proto 仍是 `pn/ps`。两轮 proto 内部保持一致，跨服务不统一，
   需要单独一轮收敛。
5. **`BuildVersion()` 只是 `go 版本 + pid`**：没有 `-ldflags` 注入构建信息（全仓现状如此）。

## 9. 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/etcd，不起 gRPC，不用 `time.Sleep`）。
数字为主代理实测导出（`.gotmp/readme-metrics/cron.txt`、`.gotmp/readme-test-aggregate.txt`），
`grep -cE '^func Test'`（已排除 `TestMain`）/ `grep -c 't.Run('`，格式 `顶层/子用例`。

### 9.1 `internal/logic` — `133/85`（8 个用例文件 + `fakes_test.go` 替身层）

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `lease_flow_test.go` | 21/24 | 租约与栅栏的裁决逻辑：同一计划时刻重复领取幂等（`ALREADY_CLAIMED`/`ALREADY_OWNED`，不产生第二行）、只有过期租约可接管且接管必递增 `fence_token`（`TestClaimTakeoverOnlyAfterExpiryAndBumpsFence`）、旧实例随后一律 `ErrLeaseLost`、并发上限命中留 `SKIPPED` 行而非静默丢计划点、`attempt` 只能服务端产生、结果与游标同事务（`TestReportTaskResultAdvancesCheckpointAtomically` 断游标 CAS 冲突时执行记录整体回滚） |
| `rules_test.go` | 15/41 | 纯规则层表驱动：上报裁决矩阵与退避收敛、枚举投影拒绝 `UNSPECIFIED` 与未知值、游标编解码拒绝垃圾、分页与文本上限边界、`MisfirePolicy` 三条互不相同的期望指针（任一算错即撞别的分支）、配置被改坏时的收敛行为、审计详情稳定且安全 |
| `task_definition_write_test.go` | 22/12 | `RegisterTask`/`UpdateTask`：校验先于开事务（`txRuns`/`defs`/`audits` 三零值同时成立）、唯一键命中即幂等且绝不覆盖线上调度、审计写失败与 CAS 未命中必须整体回滚；`TestRegisterTaskDropsAutoIncrementIdSentinel`、`TestUpdateTaskIgnoresExpectedVersionSentinel` 等 `*Sentinel` 用例按现状钉住与文档不符的语义 |
| `task_state_transition_test.go` | 19/8 | `PauseTask`/`ResumeTask`/`DisableTask` 的三处真实差异（入参门槛、`allowedFrom` 集合、调度指针落点）、拒绝发生在开事务之前、幂等重入一行不改也不写审计、`Resume` 按 `MisfirePolicy` 重算、审计写失败连状态带指针整体回滚（`TestResumeTaskRejectsUnsetMisfirePolicySentinel` 钉现状） |
| `trigger_retry_scan_test.go` | 20/0 | `TriggerTask`/`RetryRun`/`ListDueTasks` 决定「一次执行会不会跑第二遍」：非法入参在**第一次读库之前**被拒（`readCalls` 轨迹为空即证据）、排队与审计同生共死、`TriggerTask` 事务外取号与 `RetryRun` 事务内取号的差别用 `maxAttemptStaleBy` 开关测出来、到期＝状态 `ENABLED` 且指针已过期且 `ListDueTasks` 一行都不写 |
| `lease_checkpoint_health_test.go` | 15/0 | 读侧「租约/游标/健康度」：缺失用 `found=false` 表达且「已释放租约」与「从未 claim」可区分、`lease_key` 拼接（`task_key + "/" + scope`）与空白清理口径、复合游标（`task_key \x1f scope_key`）非法即报错不退首页、`only_expired` 用服务端时钟且 `expire_at=0` 两边都不算、健康度分组聚合；`...Sentinel` 三条钉住时钟回显与缺失 handler 被隐藏的现状；`TestListLeasesCursorSkipsRowsSentinel` 见 9.5 第 6 条 |
| `run_read_test.go` | 11/0 | `GetTaskRun`/`ListTaskRuns`/`ListTaskAudits`：三种「查无此人」口径的不对称必须被测出来而不是靠猜、分页三件套的校验顺序决定「零成本报错」还是「大表扫描」、游标方向与 `ORDER BY` 一致（倒序、`id < cursor`）、审计不给时间窗时的兜底下界、库里存了但契约没透出的列以哨兵钉住（`TestGetTaskRunHidesOperatorSentinel`、`TestListTaskRunsDoesNotTrimTaskKeySentinel`） |
| `task_definition_read_test.go` | 10/0 | `GetTask`/`ListTasks`：坏 `task_key`/越界 `page_size`/坏游标在碰库之前拒绝（`listCalls` 零调用）、不存在必须是 `ErrTaskNotFound` 且不带 definition、reflect 守卫保证逐列投影「加列必然要补断言」、排序/`has_more`/`total` 三个概念互不冒充 |

### 9.2 其他层

- `model/model_rules_test.go` — `27/1`：不连库也必须正确的调度规则。执行状态机矩阵（终态永不回退、
  人工重试只追加 `attempt`）、退避上限收敛（不能「失败越多跑得越勤」也不能算成负数）、
  租约过期判定与 TTL 夹紧（配置矛盾时行为确定）、任务定义自洽校验、分页收敛、枚举合法性、
  复合游标编解码、cron 表达式解析与下一个计划时刻（含时区回落）。
- `model/migration_parity_test.go` — `14/10`：结构体 `db` tag / `SELECT` 列常量 / `INSERT` 列表
  ↔ `deploy/migrations/cron` 建表语句逐列一致；四个幂等唯一键
  （`uniq_task_key`/`uniq_fire_attempt`/`uniq_lease_key`/`uniq_task_scope`）必须仍是 UNIQUE；
  查询索引存在；文本列宽与 model 校验上限对齐；枚举列文档齐全；时间列是 Unix 秒 `BIGINT`；
  无历史商业化/小程序列；迁移文件自描述。
- `internal/registry/registry_test.go` — `9/0`（取自单文件明细；分层汇总只枚举
  logic/model/config 三层，所以这一层没有 `A` 行）：坏 `Spec` 拒绝、名字含斜杠或空格拒绝、
  同名重复注册不覆盖（`TestDuplicateRegisterDoesNotOverwrite`）、`Resolve` 要求已注入 handler、
  `Run` 拒绝 nil 输出并原样传播 handler 错误、`ValidateDefinition` 执行代码侧边界、
  `EffectiveTimeout` 回落 `Spec`、名字有序且不重复。
- `internal/config/config_load_test.go` — `3/1`：`etc/` 下每个 yaml 真实 `conf.Load`；
  默认值真的落到字段上（tick/心跳与 TTL 比例、分页与留存上下界顺序）；
  最小配置（不含任何下游 RPC）与完整示例都能加载。
- `internal/svc/` **无离线单测**：`NewServiceContext` 的下游 client 构造与 `LeaseTTL()` 收敛
  只被 logic 用例间接覆盖，没有独立断言；`internal/server/` 与 `rpc/*.pb.go` 是 goctl 生成壳，
  不在单测范围内。
- 本服务没有 `internal/repository`、`internal/policy`、`internal/consumer` 目录
  （§1：cron 不订阅 MQ，调度真值恒在 MySQL）。
- `fakes_test.go` 是替身层，`top=0 sub=0` 属正常。

### 9.3 构造器级覆盖

**23/23**：探针取 `internal/logic` 全部 `New*Logic(` 共 23 个，逐个回查 `*_test.go` 引用，`gaps:` 为空，
与 §2 的 23 个方法一一对应。

### 9.4 替身层与断言口径

`internal/logic/fakes_test.go` 的 `fakeDB` 是一次用例的全部内存态，`TransactCtx` 在入口快照它、
回调报错时整体回滚——所以能真正断言「游标 CAS 冲突不得留下半条结果」，而不是只断错误类型。
复刻的语义：`ClaimForFire` 复刻 `uniq_fire_attempt(task_key, planned_at, attempt)`、
`InsertTx` 复刻 `uniq_task_key` + `ON DUPLICATE KEY UPDATE id=id`、`SetStateTx` 按乐观锁版本判定；
`runListCall`/`leaseListCall`/`cpListCall`/`dueCall`/`auditListCall`/`listCall`/`stateCall`
逐次记录分页与状态迁移的**实参快照**（「拒绝发生在触库之前」这类断言的证据就来自这里）。
故障注入：`unreadable` 造「唯一键说这行存在、按 key 却读不回」的库不一致；
`maxAttemptStaleBy` 造「事务外 `MAX(attempt)` 读到旧快照」的竞态形状。

证明不了什么（替身头注自陈）：**真正的并发正确性由 MySQL 的 `uniq_*` 与 `SELECT ... FOR UPDATE`
保证，单元测试无法也不该复刻锁**——这里复刻的是语义（唯一键命中即 `created=false`、
CAS 条件不命中即返回 `false`）；同时不证明 SQL 文本、列名与索引命中，
也不证明驱动返回的 matched vs changed rows。
另有两处由用例自己登记的盲区：`task_definition_read_test.go` 说明 `model` 返回 error 的分支
（fake 只会返回内存里的行）与 `Cache *redis.Redis` 相关分支未覆盖（本仓无 miniredis 且禁止加依赖）；
`lease_flow_test.go` 说明 `AcquireLease` 经 `Registry` 的 happy path 未覆盖
（注册表无法从包外注入 handler）。

断言口径：拒绝类断「调用轨迹为空」而不是「返回了错误」；回滚类断库内真实的残留形态；
已知不成立的语义不跳过，而是以 `*Sentinel` 用例钉成「当前真相」——修复生产代码后这些用例会红，
逼着同步改断言。

### 9.5 覆盖边界

1. 用例不连接 MySQL/Redis/etcd/MQ/对象存储，也不启动调度循环。
2. **只验证 cron 自己的调度判定链与落库序列**：本服务不订阅 MQ、也不生产事件
   （§1 与 §6「MQ 段刻意不存在」），因此**没有任何用例会断言消息投递或下游消费方的幂等**；
   「任务完成事件」契约本身尚缺（§8 缺口 3）。
3. **不验证跨服务事件/调用闭环**：§7 的 7 个 `*RPC` 下游是计划清单，`internal/registry` 里没有任何
   `Spec` 注册（§8 缺口 2），调度循环未实现（§8 缺口 1）。用例只断到「logic 决定该碰哪个 model
   方法、顺序与实参是什么」，不证明 rights/catalog/video/search-indexer 等下游真的被推进。
4. **迁移未在目标实例复验**：§3 只登记了「只能在隔离实例 `127.0.0.1:3399` 执行、禁止连本机 3306」
   这条纪律，本节不声称已在该实例复验；`model/migration_parity_test.go` 头注自陈本轮迁移没有在
   MySQL 上跑过，列级正确性目前只由那份静态一致性门禁兜住。
5. 全仓 `t.Skip` 实测口径中本服务为 0 条。
6. **数字与「跑绿」是两件事**（2026-10-03 的实测教训）：本包的用例在此之前只被计数与编译检查过，
   从未作为整包执行；整树 `go test -p 1 -count=1 ./...` 第一次把它跑红，唯一失败点是
   `lease_checkpoint_health_test.go:532` 的 `TestListLeasesCursorSkipsRowsSentinel`——期望写成裸字符串
   `"s3.task"` 而实际值是切片形态 `"[s3.task]"`，两侧口径不同导致这条一直在红。已把期望改成
   `[]string{s3.LeaseKey}`（同文件 `:531` 留注），**没有放宽任何断言**：改完它仍然精确表达
   「第二页把第一页最后一行又给了一遍」的当前现实，而漏读/重复读缺口本身仍未修复，
   继续由 `seen[s1.LeaseKey]==false`（`lease_checkpoint_health_test.go:540`）与
   `len(seen)==2`（`:543`）两条守着——生产侧把游标改成复合键那天，这两条会红着提醒撤销本节缺口。
   结论：**README 里「本服务用例数」不能当作「本服务用例跑过」的证据**；每轮的收口必须以整树
   `go test -p 1` 为准，逐包计数只能说明规模。

### 9.6 验证命令

```bash
go test -p 1 -count=1 ./services/cron/...
gofmt -l services/cron       # 必须为空
go vet ./services/cron/...
```

`-p 1` 必须保留：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
本节只描述用例断言范围，不构成任何门禁结论；契约变更后按 `docs/commands.md` 重新生成
`internal/server` 与 `rpc`，禁止手改生成物。
