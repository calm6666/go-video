# moderation-worker

OCR、ASR、图像、音频识别 Worker，被动接收 `moderation-orchestrator` 调用执行识别任务。

- **拥有数据**：`worker_task`（识别任务执行记录，关联 orchestrator `task_id`、能力类型 `ocr/asr/image/audio`、算法版本、结构化结果片段和证据引用）。
- **提供能力**：执行 OCR/ASR/图像/音频识别、超时取消、结果回传；按 `worker_task_id` 或 `task_id` 查询结果。
- **依赖**：`asset`/`transcode` 提供的媒体 URI、Redis、MySQL；外部算法/FFmpeg（本期占位，不引入真实依赖）。
- **下游 RPC**：无（被动接收 orchestrator 调用，不主动调用其它服务）。
- **约束**：
  - Worker 不做最终发布决策（不写入 `PUBLISHED` 等稿件状态）。
  - 日志不输出完整媒体内容或敏感用户数据；命中证据按脱敏规则保留引用。
  - 占位实现：本期 `Run*` 方法返回空结果片段 + TODO 注释，不调用真实算法模型，后续接入 OCR/ASR/图像/音频算法服务时替换实现。

## 测试覆盖

离线单测（纯 Go 内存替身，不连真实 MySQL/Redis/Kafka/etcd，也不调用任何算法引擎）。
数字为主代理实测导出（`.gotmp/readme-metrics/moderation-worker.txt`、
`.gotmp/readme-test-aggregate.txt`），`grep -cE '^func Test'`（已排除 `TestMain`）/
`grep -c 't.Run('`，格式 `顶层/子用例`。

### 1. `internal/logic` — `27/0`（2 个用例文件 + `fakes_test.go` 替身层）

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `run_test.go` | 16/0 | `RunOCR`/`RunASR`/`RunImage`/`RunAudio` 四个入口**各自单独跑完**守卫表、正常路径、下游失败传播与不变量四组用例（能力值与错配方向不同，不是只跑一个代表方法）：守卫先于任何依赖、写库失败必须如实抛出而**绝不回 `TASK_STATE_SUCCEEDED`**（那等于让稿件绕过机审被放行，AGENTS.md §8，`TestRunFinishFailureIsNotReportedAsSuccess`）、只写本域的 `worker_task` 不写任何结论/稿件状态、同 `worker_task_id` 重投只有一行且终态必须落在合法 `TaskState` 枚举里（`TestRunOnlyEverAsksForALegalTerminalState`）、`params_json` 原样序列化、超时缺省回落配置值 |
| `gettaskresult_test.go` | 11/0 | orchestrator 读机审结论的唯一入口：读侧只做**投影**——库存什么状态就回什么状态，`PENDING`/`RUNNING`/`FAILED`/`TIMEOUT` 都不能被洗成 `SUCCEEDED`（`TestGetTaskResultNeverUpgradesNonSucceededStates`）、一次都不写（任何 Upsert/UpdateResult/缓存失效调用都是越界，`TestGetTaskResultIsReadOnly`）、`worker_task_id` 优先于 `task_id` 的查找顺序、按 `task_id` 取最新行、缺行报 NotFound 而不是空应答、读故障原样上抛不回退、请求 ctx 被使用 |

### 2. 其他层

- `internal/config/config_load_test.go` — `1/1`：`etc` 示例配置可被真实 `conf.Load` 加载。
- 合计 **28 顶层 + 1 子用例**（logic `27/0` + config `1/1`）。
- `model/` **无离线单测**：`ON DUPLICATE KEY UPDATE mtime`、`RowsAffected==0 ⇒ ErrTaskNotFound`、
  `ORDER BY mtime DESC LIMIT 1` 只由替身复刻语义。
- `internal/repository/`、`internal/svc/` **无离线单测**：真实 `*Repository` 本身在被测路径上
  （见第 4 小节的注入缝），但它的缓存键/TTL 行为与 `NewServiceContext` 装配没有独立断言。
- 本服务没有 `internal/consumer`、`internal/policy` 目录；`internal/server/`、`rpc/*.pb.go`
  与 handler 是 goctl/protoc 生成壳，不在单测范围内。
- `fakes_test.go` 是替身层，`top=0 sub=0` 属正常。

### 3. 构造器级覆盖

**5/5**：探针取 `internal/logic` 全部 `New*Logic(` 共 5 个
（`GetTaskResult / RunASR / RunAudio / RunImage / RunOCR`），`gaps:` 为空。
`t.Skip` 全仓实测口径中本服务为 0 条。

### 4. 注入缝与替身口径

`ServiceContext.Repository` 是具体类型 `*repository.Repository`，因此 `internal/repository` 暴露
`Cacher` 接口（`Ping`/`AcquireTaskLock`/`ReleaseTaskLock`/`DelTaskResult`，只含 Repository 真正调用的依赖面）
+ `NewWithDeps(cache, conn, workerMd)`；**生产路径仍只走 `New(rds, conn)`**。
logic 用例据此组装**真实 Repository**，只把缓存与 `model.WorkerTaskModel` 换成内存替身，
所以「Upsert 主键冲突口径、`RowsAffected==0 ⇒ ErrTaskNotFound`、终态写成功后失效缓存」整条链都在被测路径上。
`conn` 传 `nil`：本服务没有任何 Repository 方法用到 `sqlx.SqlConn`（单表写入、无跨表事务）。

本服务**没有外部算法/引擎客户端**（本期占位实现，`ServiceContext` 里没有引擎字段，也不新增），
因此「引擎超时/返回未知结论」这类下游故障只能落到两个真实下游上复现：
`worker_task` 写入失败与结果缓存失效失败。用例逐个注入，不虚构引擎替身。

`internal/logic/fakes_test.go` 记了四条替身纪律（值拷贝、按真实 SQL 口径处理副作用、
按**顺序**记录调用轨迹 `<pkg>.<method>:<key>`、按方法粒度注入错误），第四条最容易踩坑：
**布数据必须走静默写入路径**（`seedTask → put`、`fakeCache.warm`），否则序列断言会把布景也算进去。
另加两条本服务特有纪律：

5. `worker_task_id` 在 `Run*` 里可能是 ULID（不可预测），因此生成式用例只数调用次数 + 回读库存，
   精确序列期望里只出现**布景给定的** ID。
6. 每个替身方法记录拿到的是请求 ctx 还是 `context.Background()`，用来钉住超时/取消传播（缺陷 #3）；
   替身同时 honour ctx 取消，否则就是「永不失败的替身」，看不到真实后果。

它证明不了什么：替身只复刻 model 层 SQL 的**语义**（`ON DUPLICATE KEY UPDATE mtime` 只改 mtime、
`RowsAffected`、`ORDER BY mtime DESC LIMIT 1`），不证明 SQL 文本与列名本身；
真实 MySQL 的主键冲突返回码语义、`TINYINT` 越界值的实际存储、Redis 真身与 TTL 都不在断言范围内；
替身**不做回滚**，所以「两步写无事务」只能断言残留的半截 `PENDING` 行（缺陷 #1）。

### 5. 覆盖边界

1. 用例不连接 MySQL/Redis/Kafka/etcd/对象存储，也不起 gRPC server。
2. **只验证 worker 自己的执行记录写入链**：本服务不生产也不消费事件、没有下游 RPC（见开头「约束」与
   「下游 RPC：无」），因此**不验证 MQ 投递、不验证下游消费方的幂等、也不验证回传是否真的被
   `moderation-orchestrator` 接收并推进稿件**——「worker 被动接收 orchestrator 调用」这一前提在本仓
   的实现状态不在本节判定范围内，审核环的现状登记在
   `services/moderation-orchestrator/README.md` 的「审核闭环」小节与其缺口 1。
3. `internal/server`、`rpc/*.pb.go`、goctl 生成壳不在单测范围内。
4. **迁移未在目标实例复验**：`deploy/migrations/moderation-worker` 与本包替身之间**没有列级对账门禁**
   （替身头注自陈），本 README 也没有隔离实例 `127.0.0.1:3399` 的执行登记；
   缺陷 #5 记录的「包注释声称 `(task_id, capability)` 联合唯一、迁移里只有非唯一索引」
   正是这种「无对账」下才会漏掉的差异，目前只有 `TestSameTaskIDAndCapabilityCreatesSecondRow`
   钉住行为，不能证明 DDL。
5. 全仓 `t.Skip` 实测口径中本服务为 0 条。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/moderation-worker/...
gofmt -l services/moderation-worker   # 必须为空
go vet ./services/moderation-worker/...
```

`-p 1` 必须保留：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
本节只描述用例断言范围，不构成任何门禁结论。

### 7. 变异探针记录（2026-09-23，历史留档）

那一轮把生产规则逐条改坏，看用例是否真的红，三组全部被抓，探针后已还原：

- `finishRun` 吞掉 `FinishTask` 错误（`if err := ...; false && err != nil`）⇒
  `run_test.go:348: RunOCR 写终态失败：错误 = nil, want injected downstream timeout`、
  `run_test.go:419: RunOCR 请求已取消：错误 = nil, want context canceled`；
- `prepareRun` 的 task_id 守卫改成 `if false && in.GetTaskId() == ""` ⇒
  `run_test.go:159: RunOCR / task_id 为空：错误 = nil, want moderation-worker: invalid task_id`；
- `GetTaskResult` 的 `worker_task_id` 优先分支改成 `if false && ...` ⇒
  `gettaskresult_test.go:105: 优先级：第 1 次调用 = worker.FindByTaskID:tk-shared, want worker.FindOne:wt-a`
  等 10 个用例同时红。

## 已知缺口 / 疑似缺陷（2026-09-23 测试轮登记，均未改生产代码）

1. **建任务与写终态是两次独立写入，无事务、无补偿**：`FinishTask` 失败后那一行永远停在 `PENDING`，
   本服务也没有重试/cron 推它走（`TestRunFinishFailureIsNotReportedAsSuccess` 钉住现象）。
   收严方向：同一 `TransactCtx`，或让 orchestrator 侧对超时未终态的任务重新下发。
2. **`Repository.FinishTask` 用 `_ = r.cache.DelTaskResult(...)` 吞掉 Redis 错误**：
   终态已落库但陈旧结果缓存在 `mw:res:*` 里最长再活 60s（`TestRunResultCacheInvalidationFailureIsSwallowed`）。
   该用例修好后会红，届时把断言改成 `wantErrIs` + `reply == nil`。
3. **`prepareRun` 用 `context.Background()` 建任务**（`runcommon.go:67`），不吃 `l.ctx`：
   请求已取消时仍会写库，随后终态写入被取消 ⇒ 稳定泄漏一条 `PENDING` 半截行
   （`TestRunCreateTaskIgnoresRequestContext`、`TestRunCancelledRequestLeaksPendingRow`）。
4. **幂等只有主键一层，运行锁与「已在跑」判定全是死代码**：
   `Repository.AcquireRunLock`/`ReleaseRunLock`、`Cache.GetTaskResult`/`SetTaskResult`、
   `model.ErrTaskAlreadyRunning` 全仓无调用方；`WorkerTaskModel.Upsert` 的注释声称
   「已存在且状态为 RUNNING/SUCCEEDED 时返回 `ErrTaskAlreadyRunning`」，实际 SQL 是
   `ON DUPLICATE KEY UPDATE mtime = VALUES(mtime)`，既不报错也不拦重入
   （`TestRunNeverAcquiresTheReentryLock`）。结果缓存因此永远只被删、从不被写。
5. **`(task_id, capability)` 联合唯一约束不存在**：`internal/repository` 包注释这么声明，
   但 `deploy/migrations/moderation-worker/000001_create_worker_task.sql` 只有非唯一的 `idx_task_id_mtime`。
   换 `worker_task_id` 重投同一 `(task_id, capability)` 会写进第二行，
   按 `task_id` 反查落到哪一行由 MySQL 决定（`TestSameTaskIDAndCapabilityCreatesSecondRow`）。
6. **重投换了 `task_id` 时答复与库存分叉**：`worker_task_id` 相同 ⇒ 库存 `task_id` 保持第一次
   （`ON DUPLICATE` 只改 mtime），但回包按第二次入参回答 `task_id`，
   orchestrator 拿回包给的 `task_id` 反查必然 `NotFound`（`TestRunReplayUnderDifferentTaskIDAnswersSomethingItNeverStored`）。
7. **`GetTaskResult` 用 `_ = json.Unmarshal` 吞解析错误**（`gettaskresultlogic.go:56`）：
   形状不对的 `result_json` 与「识别成功且零命中」都回空列表 + 无错误，读侧无法区分（`TestGetTaskResultCorruptResultJSONIsSilentlyDropped`）。
8. **`GetTaskResult` 不校验 `state` 枚举**：DB 列是 `TINYINT`（默认 0）且无 CHECK 约束，
   越界值被 `rpc.TaskState(task.State)` 原样透传给 orchestrator（`TestGetTaskResultPassesThroughUnknownState`）。
9. **占位实现的语义边界**：`Run*` 只会要求写 `SUCCEEDED`（`elapsed_ms=0`、`algorithm_version=""`、
   `result_json="[]"`），全服务不存在写 `FAILED`/`TIMEOUT` 的代码路径，
   因此「算法超时/返回未知结论 ⇒ 降级人工」目前在 worker 侧无处表达（`TestRunOnlyEverAsksForALegalTerminalState`）。
   接入真算法时必须一并补上超时/未知 → `FAILED`/`TIMEOUT` + `error_message` 的分支。
10. **`params` 序列化错误被静默忽略**（`runcommon.go:41-46` 的 `if bs, err := json.Marshal(...); err == nil`）：
    失败时会落成空 `params_json`。`map[string]string` 目前不可能失败，所以没有对应用例，仅登记。

