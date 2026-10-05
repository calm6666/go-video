# moderation-orchestrator

内容安全审核的**编排**服务：受理各领域服务的送审、持有审核任务/规则/结论/申诉四类数据、
回写机审结论、跑申诉两段流程。对应参考仓库 `workflow/filter/antispam` 的编排层思想。
本服务是「审核结论」域的数据所有者（见 [AGENTS.md §5](../../AGENTS.md)）。

> **架构约束**：领域微服务只暴露 gRPC，不提供 HTTP（AGENTS.md §3/§4）。
> 面向端的 `POST /moderation/appeal` 与面向运营的 `/admin/moderation/*`
> 由两个网关聚合（见「谁在用这个服务」）。
>
> **本 README 不描述任何 MQ 事件**：本服务没有 consumer、没有 producer、没有 topic、
> 没有消费者组、没有幂等键。原 stub 里「入 MQ 待处理」「消费 `content.published.v1`、
> `media.task.v1` 触发审核」「生产 `moderation.result.v1`」三句在代码里都不存在，
> 已删除，事实与证据见「gRPC API → 审核闭环」与「已知缺口 1/2」。

## 职责

- **送审受理**：`SubmitForReview` 建任务并置 `PENDING`，靠 `uniq_business_submission`
  唯一键 + 预检拒绝「同一对象已有审核中任务」。
- **结论回写**：`SubmitWorkerResult` 写 `moderation_result`（一任务一结论）并把任务推进到 `DONE`。
- **申诉流程**：`SubmitAppeal`（`DONE → APPEALED`）、`ProcessAppeal`（`APPEALED → APPEAL_DONE`）。
- **只读查询**：任务详情、审核结论、任务分页列表（运营后台用）。
- **不做的事**：不做 OCR/ASR/图像识别（那是 `moderation-worker`）、不写任何内容服务的表、
  不推进稿件状态、不生产事件、不持有人审工作台数据（后台只有 3 条读路由 + 1 条申诉裁决路由）。

## 数据所有权边界

对齐 AGENTS.md §5 的所有权表第 92 行：「审核结论 → `moderation-orchestrator`，
**禁止的做法：绕过审核直接发布**」。

**符合的部分**：4 张表只由本服务写，库 `go_video_moderation`，跨服务只能走 gRPC
（etcd key `moderation.v1.rpc`），Redis key 前缀 `mod:*`。本服务也从不写别人的库：
`internal/svc/servicecontext.go:22-30` 只装配 Config + Repository，没有任何下游 client。
审核结论的唯一写入口是 `Repository.SubmitWorkerResult`（`internal/repository/repository.go:286-302`）
与 `Repository.ProcessAppeal`（`:344-364`），二者只在 `internal/logic` 里被调用。

**与 AGENTS.md §5 冲突的部分（如实登记，共 3 条）**：

1. §5 要求「事务内写业务数据和 Outbox 记录，再由发布器投递事件」——本服务**根本没有 Outbox、
   没有事件、也没有事务**。`Repository` 不持有 `sqlx.SqlConn`
   （`internal/repository/repository.go:178-180`，注释写明「原 `conn` 字段是死重（本服务无跨表事务）」），
   三条「两步写」全部无原子性保证（详见缺口 3、5、6）。
2. §5 要求消费者按 `event_id` 去重 + 退避重试 + 死信 —— 本服务**没有消费者**，
   也没有 `internal/consumer` 目录（`internal/` 只有 `config/ logic/ repository/ server/ svc/`），
   所以这条约束目前对本服务是空转的（不是「做到了」，是「还没开始」）。
3. 「审核结论只能由本服务写入」在本服务侧成立，但**没有任何机制阻止绕过**：
   稿件的 `READY_FOR_REVIEW → APPROVED/REJECTED` 由 `video.TransitionState` 独占，
   而它的生产调用方是网关而不是本服务（证据见下一节）。

## gRPC API

package `moderation.v1`，端口 8093，etcd 注册 key `moderation.v1.rpc`，共 7 个方法
（`rpc/moderation.proto:172-187`）。

**分组轴**：proto 自身有 7 段 `--- ... ---` 注释（`:86/:102/:109/:120/:136/:149/:159`），
每段一个方法，一对一。7 段摊开等于平铺列表，因此按**调用方身份 × 是否推进任务状态机**
把 7 段合并为 4 组——这两个维度就是代码本身的划分依据（`internal/svc`、`internal/logic`
的守卫口径与 `internal/repository/repository.go` 的 `UpdateTaskState` 三个调用点）：

| 分组 | 方法 | 幂等/守卫口径 | 推进的状态边 |
|---|---|---|---|
| 送审受理（领域服务 → 本服务，proto `:86`） | `SubmitForReview` | 4 条入参守卫（`internal/logic/submitforreviewlogic.go:31-42`：`submission_id>0`、`content_type≠UNSPECIFIED`、`mid>0`、`business≠""`）；重复送审靠预检 + 唯一键双闸（`repository.go:209-215`、迁移 `:25`） | 建单 ⇒ `PENDING`；**不派发 worker**（`submitforreviewlogic.go:58-59`） |
| 结论回写（worker → 本服务，proto `:159`） | `SubmitWorkerResult` | 2 条守卫（`internal/logic/submitworkerresultlogic.go:31-35`：`task_id>0`、`verdict≠UNSPECIFIED`）；`reason` 长度、`worker_id` 身份都不校验；结论 Upsert 幂等（`model/moderationmodel.go:287-296`） | `PENDING/PROCESSING → DONE`（`repository.go:290-291`），迁移错误被吞成 `return nil`（`:293-295`） |
| 只读查询（运营后台/端，proto `:102/:109/:120`） | `GetTask` `GetResult` `ListTasks` | 唯一一组不写库的方法；`GetTask`/`GetResult` 走「缓存 → 回库 → 回填」（`repository.go:228-244`、`:265-281`）；`ListTasks` 无缓存且 `ps>50` 直接拒（`internal/logic/listtaskslogic.go:29-37`） | 无 |
| 申诉两段（用户提 / 运营裁，proto `:136/:149`） | `SubmitAppeal` `ProcessAppeal` | `SubmitAppeal` 3 条守卫（`submitappeallogic.go:31-39`），**不校验任务状态、不校验申诉人是不是作者**；`ProcessAppeal` 4 条守卫含 `handler>0`、`final_reason≠""`（`processappeallogic.go:31-42`） | `DONE → APPEALED`（`repository.go:313-314`）、`APPEALED → APPEAL_DONE`（`:354-355`），两处迁移错误同样被吞（`:315-317`、`:356-358`） |

**任务状态机没有显式迁移表**：全部合法边只存在于 `repository.go` 三处
`UpdateState` 的 `fromStates` 变参里（`:290`、`:313`、`:354`）+ model 的 CAS
（`model/moderationmodel.go:95-130`，`RowsAffected==0 ⇒ ErrInvalidStateTransition`）。
穷举矩阵在 `internal/logic/statemachine_test.go`，结论是**只有 4 条边**：
`1→3`、`2→3`、`3→4`、`4→5`。

**本契约没有的方法**（不要按 proto 注释或 `docs/` 里的设想来找）：
没有派发 RPC（无 `DispatchTask`/`AssignWorker`），`TASK_STATE_PROCESSING` 因此**没有任何生产者**
（`TestModerationStateMachineHasNoWriterForProcessing`）；没有人审结论写入口
（`SubmitWorkerResult` 是唯一写结论的方法，`moderation_result.reviewer` 列全仓无人写，
见缺口 7）；没有规则查询/写入 RPC（`moderation_rule` 的 3 个 model 方法全是死代码，见缺口 8）；
没有撤销任务的 RPC（`TASK_STATE_CANCELED=9` 与 `TaskStateCanceled` 常量零引用）；
没有 `ListAppeals`/`ListResults`（后台只能按 `task_id` 点查）；`GetTask` 不能按
`(business, submission_id)` 查（`FindBySubmission` 只被送审预检内部使用，
`model/moderationmodel.go:82-93`）。

### 审核闭环：结论如何回到 video（**答案是回不去**）

这是本服务最要紧的一条链路，逐跳实测结论：

```text
[已接线]  danmaku / live-room / private-message  --SubmitForReview-->  本服务(建单 PENDING)

[断点 1]  本服务  --应派发 worker-->  moderation-worker        （无代码，只有两行注释）
[断点 2]  moderation-worker  --应回调 SubmitWorkerResult-->  本服务 （零生产调用方）
[断点 3]  本服务  --应发布 moderation.result.v1-->  video / catalog / comment / danmaku
                                                          （只有 // TODO(event)）
[断点 4]  video 等  --应据结论推进-->  READY_FOR_REVIEW → APPROVED/REJECTED → PUBLISHED
                                       （无消费者；该边只有 video.TransitionState，
                                         调用方是两个网关与 operation，不含 moderation）
```

1. **入口半段是通的**：三个服务真的在调 `SubmitForReview`，且各自 `business` 不同
   （`services/danmaku/internal/repository/moderation_client.go:44-59` 用 `"danmaku"`+dmid；
   `services/live-room/internal/logic/createroomlogic.go:196-203` 用配置项
   `LiveRoom.ModerationBusiness`+room_id；`services/private-message/internal/logic/gate.go:299-306`
   用 `"private_message"`），未配置时显式报错而不是静默放行。
   `services/live-media` 只注入了 client（`services/live-media/internal/svc/servicecontext.go:92`、`:128`），
   **没有任何调用点**。
2. **本服务不派发**：`submitforreviewlogic.go:58-59` 是两行注释
   「当前实现占位：不实际调用 moderation-worker」；`config.ModerationWorkerRPC`
   （`internal/config/config.go:20-22`，`json:",optional"`）**从未被任何代码读取**，
   `ServiceContext` 里没有该字段（`internal/svc/servicecontext.go:14-17`）。
   `services/moderation-worker/` 全仓**没有一处**引用本服务客户端。
3. **结论回写没有调用方**：`.SubmitWorkerResult(` 在生产代码里**零命中**
   （唯一命中是 `services/live-room/internal/logic/fakes_test.go:3126` 的「不许被调用」替身）。
   即机审结论连写进本服务都没有通路。
4. **出向只有 TODO**：`internal/logic/submitworkerresultlogic.go:49`
   `// TODO(event): 发布 moderation.result.v1 事件，由 video/catalog/comment/danmaku 消费推进状态机。`
   `moderation.result.v1` 在全仓**只有文档生产者、没有代码生产者**
   （`docs/api-and-events.md:111` 登记了生产者、`:130` 自己承认「仍是 TODO」、
   `README.md:310`、`docs/roadmap.md:129` 四处登记）。
5. **消费端契约齐了但无人调用**：三个内容服务都实现了结论落地面
   （`danmaku.ApplyModerationResult`、`live-room.ApplyRoomModerationResult`、
   `private-message.ApplyModerationVerdict`），但 `.ApplyModerationResult(`、
   `.ApplyRoomModerationResult(`、`.ApplyModerationVerdict(` 在生产代码里
   **除生成桩和各自服务的单测外零调用方**。
6. **video 完全不知情**：`grep -rn -i "moderation" services/video/` 只有两处 README 文字，
   **零代码引用**。稿件状态机里 `READY_FOR_REVIEW → APPROVED/REJECTED → … → PUBLISHED`
   的唯一写入口是 `video.TransitionState`
   （`services/video/internal/logic/transitionstatelogic.go:31-63`），其生产调用方是
   `gateway/app/internal/logic/transitionstatelogic.go:39-45`、`gateway/admin`
   与 `services/operation/internal/repository/downstream.go:77`——**没有 moderation 这一路**。
   而网关那条路由的 `Operator`/`Target` 直接来自客户端 form 参数
   （`gateway/app/api/app.api:1071-1077` 的 `ParamTransition`，挂在
   `gateway/app/api/app.api:1105-1124` 的 `@server (prefix: /video)` 块下，
   该块**连 `middleware:` 行都没有**），
   所以「谁批准了这次发布」是不可信的。
7. **身份字段一律不可信**：`WorkerId`、`Handler`、`Mid`、`UpMid`、`Ip` 都是调用方自报，
   本服务不做任何身份校验（用例 `TestSubmitWorkerResultHasNoWorkerIdentityCheck`、
   `TestProcessAppealDoesNotCheckHandlerIdentity`、`TestSubmitAppealDoesNotCheckOwnership`），
   `moderation_result.reviewer` 更是恒为 0（见缺口 7）。

结论：**审核环是断的，且断在两处**（派发 + 回传）。当前唯一能让稿件变成 `APPROVED` 的路径是
运营/端直接调 `video.TransitionState`，等于「审核结论」与「发布决策」之间没有任何程序化联系。
这与 `docs/roadmap.md:127-133`「审核闭环尚未接通」的既有结论一致，本轮复核未变化。
登记为缺口 1（高优先级）与缺口 2。

### 谁在用这个服务

- **终端面（gateway/app）**：**1 条路由** `POST /moderation/appeal`
  （`gateway/app/api/app.api:3018-3025`，注册在
  `gateway/app/internal/handler/routes.go:974-981`，`@server` 块**无 middleware**、
  `rest.WithPrefix("/moderation")` 之外没有 `rest.WithMiddlewares`，
  `ParamSubmitAppeal` 的 `task_id`/`mid`/`content`/`ip` 全是 `form` 参数），
  logic 在 `gateway/app/internal/logic/submitappeallogic.go:38-43` 原样透传，
  其注释（`:32-33`）声称「作者归属与可申诉状态由 moderation 校验」——**本服务确实没有校验**
  （见缺口 5）。投影在 `gateway/app/internal/logic/conv_moderation.go:12`。
- **运营面（gateway/admin）**：**4 条路由**。读组 `GET /admin/moderation/tasks`、
  `/tasks/:task_id`、`/results/:task_id`（`gateway/admin/api/admin.api:860-876`）
  **无 `middleware:`**；写组 `POST /admin/moderation/appeals`（`:878-886`）有 `AdminPermission`，
  且 `handler` 取自会话而非请求体（`gateway/admin/internal/logic/processmoderationappeallogic.go:39`
  → `gateway/admin/internal/logic/adminsubject.go:98-108`）——这是本服务唯一一条身份可信的写入口。
- **其他领域服务（送审方）**：`danmaku`、`live-room`、`private-message`（各 1 处，见上一节）；
  `live-media` 已接线未调用。
- **运营服务（裁决方）**：`services/operation/internal/repository/downstream.go:119-134`
  的 `moderationGateway.RejectAppeal` 把 `final_verdict` **硬编码成 `VERDICT_REJECT`**，
  运营点「驳回申诉」在库里就永远是不通过。

## 数据模型与迁移

单文件迁移 `deploy/migrations/moderation-orchestrator/000001_create_moderation_tables.sql`，4 张表：

| 表 | 关键列 | 索引 | 迁移行号 |
|---|---|---|---|
| `moderation_task` | `submission_id`、`content_type`、`mid`、`up_mid`、`business`、`reason(500)`、`state`、`operator`、`ctime`、`mtime` | `PRIMARY(id)`、**`UNIQUE uniq_business_submission(business, submission_id)`**、`idx_mid_ctime`、`idx_state_id` | `:12-28` |
| `moderation_rule` | `name(128)`、`keywords(2000)`、`model_id(128)`、`priority`、`action`、`state`、`operator` | `PRIMARY(id)`、`KEY idx_state_priority(state, priority)` —— **无唯一键** | `:32-45` |
| `moderation_result` | `task_id`、`verdict`、`reason(500)`、`worker_id`、`reviewer`、`ctime` | `PRIMARY(id)`、`UNIQUE uniq_task(task_id)` | `:49-59` |
| `moderation_appeal` | `task_id`、`mid`、`content(1000)`、`final_verdict`、`final_reason(500)`、`handler`、`state`、`ctime`、`mtime` | `PRIMARY(id)`、`idx_task_state`、`idx_mid_ctime`、`idx_state_id` —— **`task_id` 上无唯一键** | `:63-78` |

无软删除列（`state` 是业务状态，不是 0/1 存活位）。回滚逐表
`DROP TABLE IF EXISTS`，写在各表注释里（`:6`、`:31`）。

两处**注释与实现互相矛盾**要注意：

- 迁移 `:10-11` 与 `model/moderationmodel.go:12-14` 都声称唯一键 +
  `ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`（`:57`）能实现
  「同一对象重复提审返回既有任务 ID 而不新增行」——语句本身没错，但它**一列都不改**，
  于是「返工复审」这条 `reason` 枚举里明写支持的场景（proto `:56`「如：发布、举报、复审」）
  永远拿不到新任务，且应答谎报状态（见缺口 4）。
- `model/moderationmodel.go:82-93` 的 `FindBySubmission` 带
  `ORDER BY id DESC LIMIT 1`，暗示一个对象可能有多行；`uniq_business_submission` 保证恒 ≤1 行，
  两个假设互斥。当前生产后果是「任务被物理清理后重复送审会成功建单」
  （预检放行 → INSERT 走全新主键 → 结论表按新 `task_id` 再写一条）。

Redis key（`internal/repository/repository.go:29-38`）：`mod:task:%d`、`mod:result:%d`、
`mod:appeal:%d`，三个 TTL 常量一律 `60` 秒；列表查询不缓存。
所有缓存写/失效都是 `_ = err`（`:223`、`:242`、`:258`、`:279`、`:299-300`、`:319-320`、`:360-362`），
失效失败即脏读（见缺口 6）。

## 配置与运行

- 示例配置：`etc/moderation.v1.yaml`。`Name` 与 `Etcd.Key` 都是 `moderation.v1.rpc`，
  `ListenOn: 0.0.0.0:8093`。
- 键名注意：业务缓存必须写 `CacheRedis` 而不是 `Redis`——`zrpc.RpcServerConf` 内嵌了同名
  `RedisKeyConf`，写 `Redis` 会让配置加载失败（`internal/config/config.go:15`、
  `etc/moderation.v1.yaml:8`，由 `internal/config/config_load_test.go` 守着）。
- 环境变量/配置项：`DataSource`（MySQL DSN，`etc/moderation.v1.yaml:14`，
  生产值从配置中心/Secret 注入）、`CacheRedis.Host`（`:9-11`）、
  `ModerationWorkerRPC`（`:16-22` 整段注释掉的占位，**代码从不读取**，见缺口 2）。
  该 DSN 未开 `clientFoundRows`，因此 `UpdateState`/`appeal.Update` 的
  `RowsAffected` 是 changed-rows 语义——「状态改成同值」会计 0 行，
  `model/moderationmodel.go:125-128`、`:385-388` 会把它误判成非法迁移/已处理。
- 入口 `moderation-orchestrator.v1.go:21-39`：`conf.MustLoad` → `svc.NewServiceContext`
  → `zrpc.MustNewServer` 注册 `ModerationOrchestratorServer`，Dev/Test 模式开 gRPC reflection。
- **健康检查形状**：本服务是纯 RPC，**没有 HTTP 端口**，AGENTS.md §4 的 `/api/healthz`、
  `/admin/healthz` 只属于两个网关。可用 gRPC 探针
  （`grpc_health_probe -addr=127.0.0.1:8093`）或 `scripts/rpc/smoke.ps1:320-334`
  逐方法打一遍 7 个 RPC。`Repository.Ping`（`internal/repository/repository.go:198-200`）
  存在但没有任何调用方，因此**没有就绪探针覆盖 Redis/MySQL 可达性**。
- **MQ 相关（AGENTS.md §5 要求逐项登记）**：topic 无、consumer group 无、
  `event_id` 幂等去重无、退避重试无、死信队列无、`internal/consumer` 目录不存在。
  即 §5 的事件纪律目前对本服务完全不适用，因为**没有任何事件**。
- 幂等性由数据库唯一键与 CAS 承担，不由事件 `event_id` 承担：
  `uniq_task` + Upsert（结论重放幂等）、`uniq_business_submission`（送审重放幂等）、
  `WHERE id=? AND state=0`（裁决重放**报错** `ErrAppealAlreadyHandled`，不是幂等）。

```powershell
# 生成（仓库根目录；修改 rpc/moderation.proto 后必须执行）
./scripts/gen.ps1 -Service moderation-orchestrator

# 运行
go run ./services/moderation-orchestrator -f services/moderation-orchestrator/etc/moderation.v1.yaml
```

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/etcd/MQ，不起 gRPC server）。
数字为主代理实测导出（`.gotmp/readme-metrics/moderation-orchestrator.txt`、
`.gotmp/readme-test-aggregate.txt`），`grep -cE '^func Test'`（已排除 `TestMain`）/
`grep -c 't.Run('`，格式 `顶层/子用例`。

### 1. `internal/logic` — `83/20`（8 个用例文件 + `fakes_test.go` 替身层）

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `submitworkerresult_test.go` | 14/2 | worker 回写：`PENDING/PROCESSING → DONE` 两条合法来源、`REVIEW` 结论同样把任务关成终态（`TestSubmitWorkerResultReviewVerdictStillClosesTask`，即缺口 7 的「转人审断头路」）、**永远产不出 `PUBLISHED`**（`TestSubmitWorkerResultCanNeverPublish`）、重放只推进一次、DONE 上再回写会覆盖已留痕结论且申诉结案后被静默改写、未知 `task_id` 写出孤儿结论、`WorkerId` 无身份校验（缺口 9）、Upsert 失败不推进而推进失败留下半截写入、缓存失效失败被吞 |
| `processappeal_test.go` | 13/2 | `APPEALED → APPEAL_DONE` 双侧关闭、保留原结论、已处理申诉报错 `ErrAppealAlreadyHandled`（不是幂等）、申诉行缺失也被报成「已处理」、任务态非法时仍关闭申诉、`handler` 身份不校验（缺口 9）、回读在缓存失效失败时拿到旧值（缺口 6）、`final_reason` 无长度闸（缺口 10） |
| `listtasks_test.go` | 11/2 | `ps` 守卫先于查询、分页归一化、逐字段投影且 `ORDER BY id DESC`、过滤条件逐列生效、`UNSPECIFIED` 枚举不可作过滤、跨页不重叠不遗漏、越页返回空但 `total` 诚实、`total==0` 不发第二条查询、两个失败分支分别原样上抛、不碰缓存也不写 |
| `submitappeal_test.go` | 10/3 | `DONE → APPEALED` 建 `PENDING` 申诉、绝不动原始结论、任何非法任务态都受理、**重放会插第二行**（`TestSubmitAppealReplayCreatesSecondRow`，对应 `task_id` 无唯一键）、不校验作者归属（缺口 5）、状态推进失败留下孤儿申诉行、`content` 无长度/空白闸 |
| `submitforreview_test.go` | 10/3 | 送审四守卫 + 建单 `PENDING`、在审重复送审被唯一键挡回、终态后重复送审**谎报状态**（`TestSubmitForReviewAfterTerminalStateLiesAboutState`，缺口 4）、`reason` 可为空、无枚举与长度校验、**永不推进过 `PENDING`**（`TestSubmitForReviewNeverAdvancesBeyondPending`，即缺口 1 的派发断点） |
| `gettask_test.go` | 9/2 | 守卫先于打库打缓存、读穿并回填、缓存命中不回库、不可用缓存条目回落 DB、未知 ID 报 NotFound、DB 故障原样上抛、缓存读故障降级到 DB、回填失败被吞、`GetTask` 无任何写副作用 |
| `getresult_test.go` | 9/2 | 与 `gettask` 同口径的读穿/回填/降级三态，外加两条本方法独有：无结论时是 `not found` 而非空对象、**永不读任务表**（`TestGetResultNeverReadsTaskTable`） |
| `statemachine_test.go` | 7/4 | 「全仓最要紧的状态机」矩阵：矩阵覆盖 proto 每个状态、合法边只有声明的三条、`PROCESSING` 没有任何写入方、happy path 每条边只推进一次、**没有 worker 回传就产不出 `PASS`**（`TestNoActionCanProducePassWithoutWorkerCallback`）、终态拒绝一切重开 |

### 2. 其他层

- `internal/config/config_load_test.go` — `1/1`：`etc/moderation.v1.yaml` 真实 `conf.Load`，
  反射守住业务缓存字段必须叫 `CacheRedis`（叫 `Redis` 与 `zrpc.RpcServerConf` 内嵌字段冲突，
  能编译但启动即挂）。
- 合计 **84 顶层 + 21 子用例**（logic `83/20` + config `1/1`）。
- `model/` **无离线单测**（0 个测试文件）：`UpdateState` 的 CAS、`ON DUPLICATE` 覆盖、
  `WHERE id=? AND state=0` 这些真实 SQL 只能由替身复刻语义，见第 4、5 小节。
- `internal/repository/`、`internal/svc/` **无离线单测**：`Cacher` 真身（`SetexCtx`/`GetCtx`/`DelCtx`
  与 60s TTL）与 `NewServiceContext` 装配不在断言范围内。
- 本服务没有 `internal/consumer`、`internal/policy` 目录（见「配置与运行」的 MQ 登记：
  topic/consumer group/`event_id` 去重/退避/死信全部为无）。
- `internal/server/`、`rpc/*.pb.go` 与 handler 是 goctl/protoc 生成壳，不在单测范围内。
- `fakes_test.go` 是替身层，`top=0 sub=0` 属正常。

### 3. 构造器级覆盖

**7/7**：探针取 `internal/logic` 全部 `New*Logic(` 共 7 个，`gaps:` 为空，与 7 个 RPC 方法一一对应
（调用缝引用次数：`ProcessAppeal` 21、`SubmitWorkerResult` 20、`SubmitAppeal` 16、
`SubmitForReview` 13、`ListTasks` 12、`GetResult` 11、`GetTask` 10）。
每个方法都有一张 `Test<方法>GuardsRunBeforeAnyDependency` 表驱动用例守着「守卫失败时一个依赖都不许碰」。
`t.Skip` 全仓实测口径中本服务为 0 条。

### 4. 替身层与断言口径

`ServiceContext.Repository` 是具体类型 `*repository.Repository`，生产构造走 `repository.New`
（真 Redis + 真 MySQL），因此用例统一用 `repository.NewWithDeps(内存缓存, 内存
taskMd/ruleMd/resultMd/appealMd)` 组装**真实 Repository**，只替换它的 5 个依赖，
让「缓存读穿/回填/失效、状态机 CAS（`UpdateState` 的 `fromStates` 门槛）、
结论 Upsert 的唯一键覆盖、申诉 `state=0` 门槛」整条链留在被测路径上。

替身复刻的语义（`fakes_test.go` 头注的四条纪律）：每次读返回值拷贝；`Insert` 忽略入参主键、
自增分配并回填；`uniq_business_submission` 命中时走 `ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)`
的语义（只回既有 id、**一列都不改**）；`moderation_result` 的 `uniq_task` 命中时整行覆盖；
`appeal.Update` 带 `AND state = 0`，未命中即 `ErrAppealAlreadyHandled`；`UpdateState` 是 CAS，
旧态不在 `fromStates` 内 ⇒ `RowsAffected=0` ⇒ `ErrInvalidStateTransition`。
Redis key 逐字复刻（`mod:task:%d`、`mod:result:%d`、`mod:appeal:%d`），任一处键漂移即红；
`List` 的 `pn/ps` 钳制、`state/content_type/mid > 0` 才过滤、`ORDER BY id DESC`、
`total==0 ⇒ 不发第二条 SELECT` 与 `model/moderationmodel.go` 同语义。
副作用按**顺序**记录（`callLog`，`<pkg>.<method>:<key>`），并记 ctx 传播（`lostCtx`）：
替身收到的 ctx 若丢了用例塞进的标记值，就是「换成 Background 打库」。
布数据走静默写入路径（`put`/`warm`，不记轨迹），所以序列断言从 0 起数。

它证明不了什么：SQL 文本与列名本身；真实 MySQL 的 `LAST_INSERT_ID(id)` 回读、
`ON DUPLICATE` 是否保留 `ctime`；Redis 真身与 60s TTL 的实际行为；
`uniq_business_submission` 是否真的存在（替身按二元组直接建键）；并发下的两步写竞态（替身单线程）。
DSN 未开 `clientFoundRows` 带来的 changed-rows 语义（见「配置与运行」）也只能在真机验证。

断言口径：拒绝类断「`callLog` 为空」而不是「返回了错误」；原子性类断**残留形态**
（`...LeavesHalfAdvanced`、`...LeavesOrphanAppeal`、`...LeavesHalfWritten`）；
与文档不符的现状不跳过，而以用例名直接钉出（`...LiesAboutState`、`...ReplayCreatesSecondRow`、
`...DoesNotCheckOwnership`、`...HasNoFinalReasonLengthGuard`），改生产代码时这些用例会红。

### 5. 覆盖边界

1. 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储。
2. **只验证本服务自己的状态机判定与落库序列**：`internal/consumer` 不存在、没有 Outbox、
   没有事件生产，所以「业务写与事件写是否同一事务」这一项的真实结论是「根本没有事件写」
   （缺口 1）。`moderation.result.v1` 在仓库里只有文档生产者、没有代码生产者也没有消费者，
   这条事实登记在本 README 的「审核闭环」小节与缺口 1，本节不重复判定，也**没有任何用例
   试图验证事件投递或下游消费方的幂等**。
3. 同样不在覆盖内：`SubmitForReview` 的派发分支、`SubmitWorkerResult` 的事件发布分支、
   `ModerationWorkerRPC` 的 client 装配（缺口 2：配置项从不被代码读取）、
   `moderation-worker` 的实际识别调用、`video`/`comment`/`danmaku` 侧的消费者——
   这五段既无实现也无用例。
4. `statemachine_test.go` 的矩阵是**手工抄写的期望表**（该文件头注自陈），
   它证明「代码行为与手写期望一致」，不证明「期望本身符合业务」。
5. **迁移未在目标实例复验**：本 README 没有隔离实例（`127.0.0.1:3399`）执行或列级对账的登记，
   仓库里也没有 moderation 的迁移↔model 列级对账门禁；唯一键与索引的存在性只有替身模拟。
6. 全仓 `t.Skip` 实测口径中本服务为 0 条。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/moderation-orchestrator/...
gofmt -l services/moderation-orchestrator   # 必须为空
go vet ./services/moderation-orchestrator/...
```

`-p 1` 必须保留：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
本节只描述用例断言范围，不构成任何门禁结论；契约变更后按上一节的 `./scripts/gen.ps1`
重新生成 `internal/server` 与 `rpc`，禁止手改生成物。

## 已知缺口

以下条目全部由本轮实际 Read/Grep 确认，每条给现状、影响、收严位置和 `file:line`。
标注「用例」的，改生产代码前请连同断言一起改。

1. **【高优先级】审核结论无法真正推进稿件状态——审核环断在「派发」和「回传」两处**。
   现状：`SubmitForReview` 不派发（`internal/logic/submitforreviewlogic.go:58-59`），
   `SubmitWorkerResult` 零生产调用方（全仓 `.SubmitWorkerResult(` 只出现在
   `services/live-room/internal/logic/fakes_test.go:3126` 的禁调替身里），
   出向事件仍是 `// TODO(event)`（`internal/logic/submitworkerresultlogic.go:49`），
   `moderation.result.v1` 只有文档生产者（`docs/api-and-events.md:111`，
   同一份文档 `:130-131` 自陈「仍是 `// TODO(event)`，未发布」、`README.md:310`），
   三个内容服务的结论落地面（`ApplyModerationResult`/`ApplyRoomModerationResult`/
   `ApplyModerationVerdict`）同样零生产调用方，`services/video/` 对本服务零代码引用。
   影响：**「过审」这件事在系统里目前不存在**——`PASS` 结论落到 `moderation_result` 后
   不会推进任何内容状态；稿件的 `READY_FOR_REVIEW → APPROVED/REJECTED → PUBLISHED`
   只能由运营/端直接调 `video.TransitionState` 完成，等于审核与发布解绑。
   若要收严：(a) `internal/svc/servicecontext.go` 装配 `c.ModerationWorkerRPC` 并在
   `submitforreviewlogic.go:58` 处派发；(b) 补 outbox 表 + 发布器，把 `:49` 的 TODO 落地，
   或改为同步回调内容服务的 `Apply*` 方法；(c) 与 `danmaku`/`live-room`/`private-message`
   的 `Apply*` 接线并加 `event_id` 去重（AGENTS.md §5）。
   用例：`TestModerationStateMachineHasNoWriterForProcessing`、
   `TestNoActionCanProducePassWithoutWorkerCallback`、`TestSubmitForReviewNeverAdvancesBeyondPending`。
2. **`ModerationWorkerRPC` 是纯装饰配置：声明了、注释里写了、代码里从不读**。
   现状：`internal/config/config.go:20-22` 定义（`json:",optional"`），
   `etc/moderation.v1.yaml:16-22` 给了注释掉的示例，但 `internal/svc/servicecontext.go:14-17`
   的 `ServiceContext` 只有 `Config` 与 `Repository` 两个字段，全仓没有第二处引用；
   `model.ErrWorkerNotConfigured`（`model/errors.go:46`）也因此**零引用**。
   影响：运维照注释填了 worker 地址也不会生效，配置项本身成了一个错误的「已支持」信号。
   若要收严：在 `NewServiceContext` 里按 `len(Etcd.Hosts)>0 || Target!=""` 装配 client，
   未配置时让送审返回可见错误——抄 danmaku 的口径：svc 侧留日志
   （`services/danmaku/internal/svc/servicecontext.go:57-66`），
   logic 侧直接返回 `model.ErrModerationNotConfigured`
   （`services/danmaku/internal/logic/postdanmakulogic.go:185`、
   `services/danmaku/model/errors.go:52`）。
3. **`SubmitWorkerResult` 先写结论后校验状态，非法迁移被吞成成功**。
   现状：`internal/repository/repository.go:286-302` —— 第 287 行先 `resultMd.Upsert`，
   第 290 行才 `UpdateState(task_id, DONE, PENDING, PROCESSING)`，
   第 293-295 行把 `ErrInvalidStateTransition` **直接 `return nil`**。
   于是三种场景都「成功」：(a) 任务已 `DONE` ⇒ 已审计过的结论被覆盖；
   (b) 任务已 `APPEAL_DONE` ⇒ 申诉终局结论被静默改写；
   (c) `task_id` 根本不存在 ⇒ 写出一条**孤儿结论**（`moderation_result.task_id`
   无外键，迁移 `:51`），且缓存里没有 key 可失效。
   成功路径同样不原子：Upsert 提交后 CAS 因网络失败 ⇒ 「有结论但任务仍 PENDING」的半截态，
   且没有 Outbox 或对账任务能收拾（缺口 1 的同一根因）。
   影响：**审核结论的可追溯性被破坏**（AGENTS.md §5 的「审核结论可追溯」），
   申诉终局可被 worker 重放覆盖。
   若要收严：先 CAS 再 Upsert，或把两步放进同一 `TransactCtx`；
   `ErrInvalidStateTransition` 要区分「重复回写（幂等）」与「终态被改写（应拒绝）」，
   最省事的做法是先 `FindOne` 判态再 Upsert。
   用例：`TestSubmitWorkerResultOnDoneTaskOverwritesAuditedConclusion`、
   `TestSubmitWorkerResultAfterAppealDoneSilentlyRewritesVerdict`、
   `TestSubmitWorkerResultOnUnknownTaskWritesOrphanConclusion`、
   `TestSubmitWorkerResultPropagatesStateFailureAndLeavesHalfWritten`、
   `TestSubmitWorkerResultReviewVerdictStillClosesTask`（`VERDICT_REVIEW`「转人审」
   也会把任务当 `DONE` 关掉）。
4. **`uniq_business_submission` 让「复审」不可能新建任务，且应答谎报状态**。
   现状：`model/moderationmodel.go:52-67` 的 `Insert` 用
   `ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)` —— 命中唯一键时**返回旧 id 且一列都不改**；
   预检（`internal/repository/repository.go:209-215`）只在旧任务处于 `PENDING/PROCESSING` 时
   报 `ErrDuplicateTask`，旧任务已是 `DONE/APPEALED/APPEAL_DONE/9/0` 就放行；
   而 `submitforreviewlogic.go:62` 返回的是**内存里的 `t`**（`State` 在
   `repository.go:216` 被硬置 `PENDING`、`Ctime` 在 `model:53-55` 被刷新），
   不是回读的行。
   影响：(a) 同一 `(business, submission_id)` 一生只能有一个审核任务，
   proto `:56` 与 `model` 注释都写着 `reason` 支持「举报、复审」，实际复审只能拿到旧任务 id；
   (b) 调用方（如 `services/danmaku/internal/repository/moderation_client.go:52-57`）
   读到的是 `state=PENDING` + 新 `ctime`，而库里那条仍是 `DONE` —— **应答与库不一致**，
   送审方会以为重新进队了。
   若要收严：唯一键改成 `(business, submission_id, state)` 之外的「一对象一终态」模型，
   或新建「送审流水」表；至少 `repository.go:221` 之后回读一次再投影。
   用例：`TestSubmitForReviewAfterTerminalStateLiesAboutState`、
   `TestSubmitForReviewCreatesPendingTask`。
5. **申诉两处不校验：不校验任务状态、不校验申诉人**。
   现状：`Repository.SubmitAppeal`（`internal/repository/repository.go:307-322`）
   **先** `appealMd.Insert`（:308-310）**再** CAS `DONE → APPEALED`（:313-314），
   且 :315-317 只在错误**不是** `ErrInvalidStateTransition` 时才返回——
   非法状态下的申诉行照样落库，函数返回成功。`logic`（`submitappeallogic.go:31-39`）
   也不比对 `in.Mid` 与 `task.Mid`。
   影响：(a) 对 `PENDING`（还没审）、`APPEAL_DONE`（已终局）、甚至**不存在的 task_id**
   都能提交申诉，并在 `moderation_appeal` 里留下孤儿行（该表 `task_id` 无唯一键，迁移 `:75`）；
   (b) 任何人可对任何任务提申诉——`gateway/app` 的 `mid` 是 form 自报且该路由无 middleware
   （`gateway/app/api/app.api:3018-3025`），`gateway/app/internal/logic/submitappeallogic.go:32-33`
   的注释「由 moderation 校验作者归属」是错的；(c) 同一申诉重放会**新增第二行**
   （`uniq` 缺失 + 先 Insert 后 CAS）。
   若要收严：`SubmitAppeal` 里先 `taskMd.FindOne` 校验 `State==DONE` 与
   `t.Mid==a.Mid` 再 Insert，并把两步写放进一个事务。
   用例：`TestSubmitAppealIsAcceptedInEveryIllegalTaskState`、
   `TestSubmitAppealReplayCreatesSecondRow`、`TestSubmitAppealDoesNotCheckOwnership`、
   `TestSubmitAppealPropagatesStateFailureAndLeavesOrphanAppeal`。
6. **`ProcessAppeal` 先结案后推进任务，且回读可能拿到旧缓存**。
   现状：`internal/repository/repository.go:344-364` —— 第 345 行 `appealMd.Update`
   （带 `AND state = 0`，`model/moderationmodel.go:374-390`）先把申诉标记为已处理，
   第 354-358 行才推进 `APPEALED → APPEAL_DONE`，同样把非法迁移**吞掉**；
   第 348 行 `FindOne` 无行时（`a == nil`）**静默跳过状态推进**并返回 nil（:352-361）。
   缓存侧：`logic` 在 :51 用 `GetAppeal` 回查作为应答，而 `repository.go:362` 的
   `DelAppeal` 是 `_ =`，失效失败即脏读。
   影响：(a) 任务处于 `PENDING`/`DONE`/`APPEAL_DONE` 时申诉仍被结案，
   出现「申诉已处理但任务从未进入申诉态」的自相矛盾对；
   (b) `appeal_id` 不存在时 `appealMd.Update` 返回 `aff==0` → `ErrAppealAlreadyHandled`
   （`model:385-388`），**「不存在」被上报成「已被别人处理」**，运营看到误导性错误；
   (c) 未开 `clientFoundRows`，`Update` 写回同值会计 0 行，同样误报。
   若要收严：`Update` 先 `FindOne` 区分 `ErrAppealNotFound` 与已处理；两步写进同一事务；
   `DelAppeal` 失败要报错或改成写穿。
   用例：`TestProcessAppealClosesAppealEvenWhenTaskStateIsIllegal`、
   `TestProcessAppealOnMissingAppealIsReportedAsAlreadyHandled`、
   `TestProcessAppealPropagatesReadBackFailureAndLeavesHalfAdvanced`、
   `TestProcessAppealReadBackHitsStaleCacheWhenInvalidationFails`。
7. **人审没有入口，`reviewer` 列恒为 0；`verdict=REVIEW`「转人审」是断头路**。
   现状：`submitworkerresultlogic.go:38-43` 组装 `model.ModerationResult` 时
   只填 `TaskID/Verdict/Reason/WorkerID`，**从不填 `Reviewer`**（proto 有字段
   `rpc/moderation.proto:69`、表有列 迁移 `:55`、`resultToRPC` 也照抄 `convert.go:38`）；
   本服务 7 个方法里没有任何「人审结论」RPC；`VERDICT_REVIEW`（proto `:44`）在 `internal/`
   生产代码里**一次都没出现**——两处 verdict 校验都只是「≠ UNSPECIFIED」
   （`submitworkerresultlogic.go:34`、`processappeallogic.go:37`），
   `model.VerdictReview` 常量零引用，
   而 `repository.go:290` 对三种 verdict 一律推进到 `DONE`。
   影响：**「审核结论可追溯、操作人必填」这条约束（AGENTS.md §5 与本服务原 README 都承诺了）
   在数据上不成立**——
   每条结论的 `reviewer` 都是 0，机审 `worker_id` 又是调用方自报（见缺口 9）；
   `REVIEW` 结论既没有下游处理，又和 `PASS`/`REJECT` 一样把任务关掉。
   若要收严：要么补 `SubmitReviewResult`（带可信 handler）RPC 并填 `Reviewer`，
   要么删掉 `reviewer` 列/字段与 `VERDICT_REVIEW` 枚举，别让契约承诺没有实现的能力。
   用例：`TestSubmitWorkerResultReviewVerdictStillClosesTask`、
   `TestSubmitWorkerResultCanNeverPublish`（后半句守住了「不能直接置 APPROVED」）。
8. **`moderation_rule` 整张表是死数据面：无 RPC、Repository 无方法、model 三方法零调用方**。
   现状：`internal/repository/repository.go:159-165` 持有 `ruleMd` 字段，
   但 `Repository` 的任何一个导出方法都不读它；
   `model/moderationmodel.go:182-256` 的 `Insert`/`FindOne`/`ListEnabled`
   在生产代码里零调用方，`model.ErrRuleNotFound`（`model/errors.go:36`）、
   `RuleStateEnabled`（`:74`）同样零引用。表本身也没有唯一键（迁移 `:32-45`），
   `name`/`model_id` 可任意重复。
   影响：AGENTS.md §5 让本服务拥有「规则版本」数据，proto 与迁移都建了表，
   但**没有任何路径能写入或读出规则**——「命中哪条规则审的」不可追溯，
   本 README 的「职责」里也不该承诺规则查询能力（原 stub 承诺了「规则查询」）。
   若要收严：补规则的 CRUD RPC 并让 `SubmitWorkerResult` 关联 `rule_id`，
   或者删掉表与 model 三方法，把规则归到 `risk-control` 的名单/规则能力上
   （AGENTS.md §5「同一能力只有一个所有者」）。
9. **三个身份字段（`WorkerId`/`Handler`/`Mid`）与 `Ip` 全由调用方自报，服务侧零校验**。
   现状：`submitworkerresultlogic.go:31-35` 只校验 `task_id>0` 与 `verdict≠UNSPECIFIED`，
   不比对 `worker_id` 与被派工关系（也没有派工，见缺口 1）；
   `processappeallogic.go:34-36` 只要求 `handler>0`，任何值都接受；
   `moderation_task.operator`（proto `:60`）在 `submitforreviewlogic.go:44-51`
   里**根本没赋值**，永远写 0。
   影响：**审核裁决记录里的操作人不可作为审计证据**；唯一的例外是经
   `gateway/admin` 的 `POST /admin/moderation/appeals`（`handler` 取自会话，
   `gateway/admin/internal/logic/processmoderationappeallogic.go:39`），
   但那是网关侧行为，任何直连 gRPC 的调用方都能伪造。
   若要收严：服务侧接入 gRPC 内部身份（mTLS/服务 token），按服务白名单
   校验 `worker_id`/`handler` 的来源，并在 proto 上把 `handler` 改成服务端派生字段。
   用例：`TestSubmitWorkerResultHasNoWorkerIdentityCheck`、
   `TestProcessAppealDoesNotCheckHandlerIdentity`。
10. **长度/枚举边界一律不校验，超限时暴露为 500 而不是参数错误**。
    现状：本服务只有 11 条 `model.Err*` 参数守卫（`model/errors.go:6-47`），
    没有任何一处比对列宽：`business` 列 64（迁移 `:18`）、`reason` 列 500（`:19`）、
    `content` 列 1000（`:67`）、`final_reason` 列 500（`:69`）；
    `content_type` 也不校验是否 ≤ `CONTENT_TYPE_LIVE`（proto `:20-27`），
    `verdict` 不校验 ≤ 3。对比：调用方已经知道自己会越界，
    `services/private-message/internal/logic/gate.go:305` 主动 `truncateRunes(reason, maxReasonRunes)`。
    影响：MySQL 在严格模式下抛 1406，`model` 侧包装成 error 上抛
    （`model/moderationmodel.go:60`、`:293`、`:352`），gRPC 返回 `codes.Unknown`，
    网关只能显示系统错误，调用方拿不到「参数非法」这一档。
    枚举侧反向问题：越界整数值被 `int32()` 原样落库
    （`submitforreviewlogic.go:46`、`submitworkerresultlogic.go:40`、
    `processappeallogic.go:45`），而 `List` 只在 `> 0` 时才加 WHERE 条件
    （`model/moderationmodel.go:148-155`），所以 `state=0`/`content_type=0` 的脏行
    **写进去就再也无法单独筛出来**。
    若要收严：`submitforreviewlogic.go:42` 之后补 `utf8.RuneCountInString` 与枚举上界校验。
    用例：`TestSubmitForReviewHasNoEnumOrLengthValidation`、
    `TestSubmitAppealHasNoContentLengthOrBlankGuard`、
    `TestProcessAppealHasNoFinalReasonLengthGuard`、
    `TestListTasksCannotFilterByUnspecifiedEnums`。
11. **`ListTasks` 是分页 COUNT + 无界 OFFSET，且 `total` 与实际可读页会漂移**。
    现状：`internal/logic/listtaskslogic.go:29-37` 允许任意 `pn`；
    `model/moderationmodel.go:132-180` 每条查询都先 `SELECT COUNT(*)`，
    再 `... ORDER BY id DESC LIMIT ? OFFSET ?`，深翻页无游标；
    `idx_mid_ctime`/`idx_state_id`（迁移 `:26-27`）都不覆盖 `ORDER BY id DESC` 的组合，
    多过滤条件时靠 `WHERE 1=1` 动态拼接（`model:141-155`）。
    影响：运营后台可被一次 `pn=999999` 拖住；`total==0` 时短路不发第二条 SELECT
    （`model:165-167`）是好的，但两次查询非同一事务，翻页期间有写入就会重复/漏行。
    若要收严：改游标分页（`id < ?`）并给 `ps` 加会话级默认，或限制 `pn*ps` 上限。
    用例：`TestListTasksBeyondLastPageReturnsEmptyButHonestTotal`、
    `TestListTasksIsCacheFreeAndWriteFree`、`TestListTasksPagesDoNotOverlapAndCoverEverything`。
12. **`GetTask` 与 `GetResult` 之间没有一致性；`GetResult` 从不看任务表**。
    现状：`internal/repository/repository.go:265-281` 的 `GetResult` 只查
    `resultMd.FindOne(taskID)`，与 `TaskState` 完全无关；`convert.go:29-41` 也不投影状态。
    影响：结合缺口 3(c)，运营/端可以拿到「一个不存在任务」或「已 DONE 但结论被重放覆盖」的结论，
    而且缓存命中时（`repository.go:266-271`）返回的是 60 秒内的旧 JSON，
    `ID>0`/`TaskID>0` 的可用性判断只挡空对象，不挡过期对象。
    若要收严：`GetResult` 一并返回任务状态，或在结论不存在时按 `task.state` 显式区分
    「未审」与「审完无结论（脏数据）」。
    用例：`TestGetResultNeverReadsTaskTable`、`TestGetResultFallsBackToDatabaseOnUnusableCacheEntry`。
13. **缓存序列化失败会写进 Redis 一个空值键**。
    现状：`internal/repository/helpers.go:12-18` 的 `jsonMustMarshal` 在出错时
    **返回 nil** 而不是 panic 或跳过，调用点全部是
    `_ = r.cache.SetTask(ctx, id, jsonMustMarshal(t))`（`repository.go:223`、`:242`、
    `:279`、`:320`、`:339`）。`redis.SetexCtx` 收到 nil 字节会存入空字符串。
    影响：读侧 `jsonUnmarshal` 报错 → 走回库分支（`repository.go:231` 的
    `err == nil && t.ID > 0` 判定），功能上降级可用，但**每次读都白读一次 Redis 再打库**，
    且 `Cache` 里没有区分「空值」与「miss」的标记。
    若要收严：`jsonMustMarshal` 返回 `([]byte, error)`，调用点失败即跳过写缓存。
    用例：`TestGetTaskFallsBackToDatabaseOnUnusableCacheEntry`、
    `TestSubmitForReviewCacheFailureIsSwallowed`。
14. **`TASK_STATE_CANCELED`（9）与脏状态 `0` 无人产生也无人能收拾**。
    现状：proto `:37`、`model/errors.go:59` 都定义了 `CANCELED`，
    但**没有任何方法能把任务置为 9**（三个 CAS 目标态只有 3/4/5，见
    `repository.go:290`、`:313`、`:354`），也没有撤销 RPC；
    `state` 列默认 0（迁移 `:20`），若人工修数或未来新代码写入 `0`/`9`，
    这三条边**都无法再离开**（`TestTerminalStatesRejectEveryReopenAttemptButOnlyInState`）。
    影响：稿件被撤回后审核任务无法撤销，永远占着
    `uniq_business_submission` 这个唯一键，导致该对象**永久不能再送审**（缺口 4 的加重项）。
    若要收严：补 `CancelTask`（`PENDING/PROCESSING → CANCELED`），
    并把 `CANCELED` 从「占唯一键」的集合里排除。
    用例：`TestModerationStateMachineOnlyHasTheThreeDeclaredEdges`、
    `TestTerminalStatesRejectEveryReopenAttemptButOnlyInState`。
