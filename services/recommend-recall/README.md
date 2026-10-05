# recommend-recall

视频推荐召回服务。

- **拥有数据**：热门池、关注池、标签池、协同/向量候选和召回版本。
- **提供能力**：多路召回、冷启动、候选去重、过滤和降级到热门/关注流。
- **依赖**：`spm`/`feature-store`、`video`/`social-graph` 投影、Redis/OpenSearch。
- **约束**：不接收广告或商业化候选；推荐不可用时不影响详情和播放。

## 读写链路（本服务的实际形态）

离线侧 `UpsertPoolItems`（分批写条目 + 末批封版 READY）→ `PublishPoolVersion`（CAS 切 `recall_pool_current` 指针、旧版本置 RETIRED、同事务写 `recall_outbox` 与幂等标记）→ 在线侧 `RecallCandidates`（只认指针读条目、合并去重、按预算逐路降级、落 `recall_request_log` 审计）→ `GetRecallRequestLog`/`ListRecallRequestLogs` 回放、`PrunePoolVersions` 按保留窗口清理。`RollbackPoolVersion` 走同一套切换编排（`internal/logic/poolswitch.go`）。

事件投递侧（`internal/publisher/`，`common/outbox` 引擎的第 3 个使用方）：发布循环按 `Kafka.PollIntervalSec` 轮询 `recall_outbox` 的到期待发行（`state = 待发布 AND next_retry_at <= now ORDER BY id LIMIT BatchLimit`），逐条经 `common/outbox.CheckRow` 判形后投 `recall.pool.published.v1`，topic 取 `event_type + ".v" + schema_version`，分区键取 `aggregate_id`（`source:pool_key:version`，不是 `event_id`）。broker 受理成功才 `MarkPublished`（刷新 `mtime` 作为发布位点），失败走 `MarkRetry`（SQL 侧 `retry_count + 1` + 指数退避），累计到 `Kafka.MaxRetries` 由 `MarkFailed` 置死信。默认构建不链接队列运行时，`Kafka.Enabled` 示例配置为 `false`（见缺口 B6）。

## 数据与迁移

| 表 | 迁移 | 关键约束 |
|---|---|---|
| `recall_pool` | `deploy/migrations/recommend-recall/000001_create_recall_pool.sql` | `UNIQUE KEY uniq_pool_item(source,pool_key,version,aid)`（分批写靠它做 ODKU 幂等） |
| `recall_pool_version` | `.../000002_create_recall_pool_version.sql` | `UNIQUE KEY uniq_pool_version(source,pool_key,version)`、`uniq_pool_version_batch(source,pool_key,batch_id)` |
| `recall_pool_current` | `.../000003_create_recall_pool_current.sql` | `PRIMARY KEY(source,pool_key)`（无自增 id，指针是读路径唯一权威） |
| `recall_request_log` | `.../000004_create_recall_request_log.sql` | `UNIQUE KEY uniq_request_id`、`uniq_snapshot_id` |
| `recall_outbox` | `.../000005_create_recall_outbox.sql` | `UNIQUE KEY uniq_event_id` |
| `recall_idempotency` | `.../000006_create_recall_idempotency.sql` | `UNIQUE KEY uniq_scope_key(scope,idempotency_key)` |

迁移登记见 `deploy/migrations/README.md`（`go_video_recommend_recall`，6 条，`applied`）。迁移 DDL ↔ model 列的逐列对账由 `model/migration_parity_test.go` 静态守住（不连库）。

## 运行

```bash
go run recommendrecall.v1.go -f etc/recommendrecall.v1.yaml
```

`Name: recommendrecall.v1.rpc`，默认 `ListenOn: 0.0.0.0:8116`，etcd 注册同名 Key。启动期 `Recall.Validate()` 或 repository 组装失败直接 panic（不带病上线），并把「下游数据源未接线」显式打进启动日志。

要真正投递事件必须用带队列运行时的构建并打开开关（两者缺一都投递不出去，且都会显式说明而不是静默）：

```bash
go build -mod=readonly -tags recommendrecall_kafka -o /tmp/recommendrecall ./recommendrecall.v1.go
# 再把 etc yaml 的 Kafka.Enabled 置为 true；示例配置默认 false，此时进程照常起、只打日志不投递
```

`Kafka.Enabled=true` 而二进制没链接运行时（默认构建）时，`NewSender` 恒返回 `ErrKafkaRuntimeNotBuilt`，`logx.Must(ctx.startPublisher())` 让进程启动即失败；启动日志里的 `publisher.RuntimeNotes` 两分支都会写明当前是「未链接」还是「已链接但未启用」。

## 已知缺口

### A. 在线召回的接线（发布阻塞）

1. **没有任何调用方**：`RecallCandidates` 与 `GetRecallConfig` 在全仓除本服务自身外零调用点，`gateway/app/` 整个目录里连 `recall` 字样都没有（grep 事实）。当前形态是「服务可跑、RPC 可达、终端拿不到召回结果」。要接通需要终端/网关侧新增召回入口，属跨服务契约变更，本轮不动代码。
2. **`Features` 生产恒为显式 stub**：`internal/repository/repository.go:111-113` 由 `New` 直接塞 `UnconfiguredFeatureSource{}`，`internal/svc/servicecontext.go:46` 组装时没有传任何下游依赖。于是 `GetUserInterest`/`GetBehaviorSignals`/`ListHotSubjects`/`GetVectorCandidates` 一律 `ErrSourceNotConfigured`（`internal/repository/featuresource.go:95-107`）——标签路、协同路的种子推导和向量路在生产**永不出数**，每次请求都以 `feature_unavailable` 记进降级原因与审计行。待接契约：`spm`/`feature-store`。
3. **`Visibility` 同样恒为 stub，导致合规/可见性过滤从未生效**：`FilterVisible` 稳定报错（`featuresource.go:112-115`），`filterVisibleChunks` 判依赖不可用后**一条都不剔除**（`internal/logic/recallcandidateslogic.go:394-426` + `internal/logic/recallsource.go:497-514` 的 fail-open 口径），候选原样下发，只带 `degraded=true`、`ttl_seconds=0`。也就是说"已下架稿件照样进候选"是今天的真实结论。钉：`TestRecallCandidatesWithProductionStubVisibilityServesEverything`（`internal/logic/recallcandidateslogic_test.go:659`，断言 aid 999 仍在响应里）。修法要接 `video` 侧可见性投影。
4. **拉黑/关注关系过滤器不存在**：契约里有 `VisibilitySource.BlockedUps` 与 `FollowingUps`，整条召回链路一次都没调用它们（`featuresource.go:117-125` 只有占位实现）；`recallcandidateslogic.go:56-58` 已声明该判定应由 video 可见性承担，而第 3 条说明可见性本身没接。关注路读的是 `recall_pool` 的 `mid:<mid>` 池（`recallsource.go:258-265`），不查 `social-graph`。钉：`TestRecallCandidatesBlacklistFilterIsNeverInvoked`（`recallcandidateslogic_test.go:697`）。
5. **没有任何离线池生产者**：池内容只能靠 `UpsertPoolItems` 写入，全仓唯一入口是运营路由 `/admin/recommend/pool/item/upsert`（`gateway/admin/api/admin.api:6480`）。全新部署里 `recall_pool_current` 为空 → `RecallCandidates` 一律 `pool_not_ready`，`GetRecallConfig` 打 "recall_pool_current has no published pool" 错误日志（`getrecallconfiglogic.go` 的 `readyPools`）。离线生成作业（读 spm/feature-store 产出候选并Publish）尚不存在。

### B. 事件与调度

6. **`recall.pool.published.v1`：发布器已接线，但投递语义从未在真实 broker 上验证过**（原缺口「只登记不投递」的当前形态；顺带修掉了接线时发现的读侧缺陷，见下）。本轮把 `model/outbox.go` 的引擎接口重塑为 `ListPending/MarkPublished/MarkRetry/MarkFailed`，接到 `common/outbox` 引擎与 `internal/publisher`，并在 `internal/svc/servicecontext.go:91 startPublisher` 装配。**接线时发现并修掉的缺陷**：`ListPending` 原读侧谓词是 `state IN (待发布, 失败)`，而死信是终态，于是每轮把已判死的行重新取出再投一次（判死形同虚设、毒事件永久循环）；现收紧为 `state = 待发布`，`MarkFailed` 的 `WHERE state = 待发布` 保证判死单向。钉：`TestListPendingSelectsOnlyPendingDueRows`（SQL 里出现 `state IN` 即失败）、`TestMarkFailedOnlyFromPending`。**仍然存在的缺口**，逐条：
    - a. **没有 broker 侧证据**：本仓库从未与任何 Kafka/Redpanda 联调过。默认构建不链接队列运行时，示例配置 `Kafka.Enabled=false`（`etc/recommendrecall.v1.yaml`，由 `example_yaml_test.go` 钉住），所以「单测全绿」只证明状态机与判形正确，不证明事件真的落到 topic。没有端到端用例，也没有可跑的黑盒验证脚本。
    - b. **零消费者**：`recall.pool.published.v1` 在全仓没有任何读取方（本服务没有 `internal/consumer`，其他服务也没订阅）。事件投出去之后无人消费，排序特征与缓存失效投影仍要靠 RPC 轮询（见 docs/roadmap.md 的 MQ 接线）。
    - c. **已发布行无人清理，也没有滞留告警**：`model/outbox.go` 的 `CountPending`(:203)/`CountStuck`(:216)/`DeleteSentBefore`(:233) 除 `model/outbox_sql_test.go` 外全仓零生产调用者，`recall_outbox` 因此只写不清、长期无限增长，且 `state=0` 堆积没有指标可见。与缺口 B7 的 `recall_request_log` 同型（都缺调度方）。
    - d. **判死没有人工释放入口**：`MarkFailed` 只能从待发布出发（防止把已发布行改回死信、伪造投递时间），因此死信是终态；rpc 层没有重投/解封方法，运维要恢复一条死信只能手改库。
    - e. **没有租约/抢占列**：`ListPending` 与状态推进之间不持锁，多副本同时运行会把同一行各投一次（至少一次语义，靠消费者按 `event_id` 去重兜底），且各副本各自 `retry_count + 1` 会累计超过 `Kafka.MaxRetries`。部署上要么单副本跑发布循环，要么接受超发。
    - f. **鉴权集群不可用**：go-queue v1.2.2 的 `kq.NewPusher` 不暴露 SASL/TLS 注入口，`KafkaConf` 因此刻意没有 `Username/Password/CaFile` 三个键；带鉴权的 broker 需要换客户端或升依赖（本轮不引新依赖）。
    - g. **迁移注释与代码漂移**：`deploy/migrations/recommend-recall/000005_create_recall_outbox.sql` 的说明仍写接线前的形态（提到已改名的 `MarkSent`、`ListPending` 的 `state IN (PENDING, FAILED)` 旧谓词、以及「`retry_count` 由 `MarkFailed` 每次 +1、`maxRetry` 作更新条件」，实际自增在 `MarkRetry`、判死由引擎决定）。已应用的迁移不改（本仓库迁移纪律：新增 `0000NN_*.sql`，禁止修改已存在文件），故登记为文档漂移而非补丁。
7. **`RequestLogRetentionSeconds` 是死配置**：`internal/config/config.go:128` 定义、`:195` 启动校验必须为正、`etc/recommendrecall.v1.yaml:68` 给了值，但 `internal/svc/servicecontext.go` 的 Options 里没有它、全仓无任何读取点。注释承诺的「由 services/cron 触发清理」不存在 → `recall_request_log` 只写不清，长期无限增长。
8. **`PrunePoolVersions` 没有作业排它**：`deploy/migrations/cron/000001..000002` 只建表、不含任何 `cron_task` 种子行，`services/cron/etc/cron.v1.yaml` 没有作业清单，全仓唯一入口是 HTTP `/admin/recommend/pool/version/prune`（`admin.api:6492`）。三道保护（保留窗口 / ACTIVE 指针 / 事务内 `FindForUpdate` 复核）只在人工点击时生效，没人排就永远不清。

### C. 契约与显示口径（与注释/文档不一致，按现状钉住）

9. **`RollbackPoolVersion` 接受从未上线的 READY 版本**，且事件仍标 `rollback=true`，与 `rollbackpoolversionlogic.go:33` 的「目标状态来源是 RETIRED」自相矛盾：下游按 rollback 标记做"退回旧批次"处理，实际收到的是根本没出过数的新批次。钉：`TestRollbackPoolVersionAcceptsNeverPublishedReadyVersionKnownGap`（`publishrollbacklogic_test.go:668`）——该用例钉的是当前真实行为，生产修好后必须同步改。
10. **`GetPoolSnapshot` 的 `published_at` 取版本行镜像而不是指针**：镜像没回填时，一个已上线的池会回 `0`，被 proto 注释的「未上线为 0」读成"这个池还没上线"。属运维面显示口径缺口（在线出数不受影响，指针才是权威）。钉：`TestGetPoolSnapshotPublishedAtComesFromVersionRowNotPointer`（`poolreadlogic_test.go:274`）。
11. **`ListPoolVersions` 的负 limit 不报错**：`-1` 归一成"按配置上限取"（最多 100 行），而不是本仓库多数接口的"负数即拒绝"，也不是"0 行"。钉：`TestListPoolVersionsNegativeLimitFallsBackToConfiguredMax`（`poolreadlogic_test.go:376`）。
12. **合规过滤筛到空时逐路统计看着正常**：候选为空、路记进 `dropped_sources`，但每路 `SourceStat` 既不置 degraded 也没有 error_code，排障时与"池上线了但没条目"无法区分（归因缺陷）。钉：`TestRecallCandidatesComplianceFilterToEmptyKeepsSourceStatsLookingNormal`（`recallcandidateslogic_test.go:718`）。

### D. 装配守卫不对称

13. **四个方法对 `Repository == nil` 没有守卫，另外四个有**：无守卫 → 组装缺失时 panic —— `getpoolsnapshotlogic.go:38-39`、`listpoolversionslogic.go:43-44`、`getrecallrequestloglogic.go:50`、`listrecallrequestlogslogic.go:38-39`；有守卫 → 返回 `repository.ErrSourceNotConfigured` —— `recallcandidateslogic.go:65`、`getrecallconfiglogic.go:49`、`upsertpoolitemslogic.go:79`、`prunepoolversionslogic.go:84`。钉：`TestRequestLogReadsDivergeFromConfigReadsOnNilRepository`（`rrclog_test.go:411`）；任何一方被补齐，该用例变红并要求同步删除本条。

### E. 测试覆盖边界（如实声明，决定断言强度）

14. **缓存路径无离线覆盖**：`redis.Redis` 是具体类型而非接口，测试注入缝里 `Cache` 恒为 nil（`fakes_test.go:2089-2091`；`helpers.go:509`、`:544` 对 nil 直接跳过读写）。因此"缓存命中/回填/top-N 失效"一条都没断到，且所有池读用例的主降级原因都是 `store_unavailable`。
15. **`kept_items_remain` 结论在进程内不可达**：替身的 `Pool.DeleteByVersionsInTx` 与 `CountByVersionInTx` 读同一张 map（`fakes_test.go:666-760`），未命中 `max_rows` 时删完必然计数归零，所以逐版本结论只能覆盖到 `items_partially_deleted`（用 `max_rows=1` 触发，见 `rrcprune_test.go`），真库上"删了一批但该版本还有条目"的形态要靠真库验证。
16. **model 层只有 `outbox.go` 有 SQL 文本单测**：`model/outbox_sql_test.go` 用录制型 `sqlx.SqlConn` 替身钉住了它的列序、占位符、WHERE 守卫与错误包装；其余 `pool.go`/`poolversion.go`/`poolcurrent.go`/`requestlog.go`/`idempotency.go` 的 SQL 文本仍只由 `model/migration_parity_test.go` 做 DDL↔代码静态对账，logic 层替身复刻的是 WHERE/LIMIT/排序/唯一键冲突的**语义**，不证明 SQL 本身。真并发（行锁、隔离级别、`AUTO_INCREMENT` 回收）同样只由 `store.commitExternally` 注入一份对手变更来近似；`outbox_sql_test.go` 的替身也不执行 SQL，它对 MySQL 优化器行为（`idx_state_next_retry` 是否真被走到）同样沉默。

## 测试覆盖

离线单测（纯 Go 替身，不连 MySQL / Redis / etcd / Kafka / OpenSearch，也不需要网络）。
数字为 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。
规模合计 **169 顶层 + 34 子用例**（logic `105/24`、publisher `25/5`、model `30/2`、repository `6/1`、config `3/2`），
本服务 0 条用例处于 `t.Skip` 状态。
publisher 的两个构建口径用例集不同（`kafkaruntime_disabled.go` 与 `kafkaruntime_kafka.go` 由构建标签互斥）：
默认构建静态 `25/5`（把 `kafkaruntime_disabled_test.go` 换成 `kafkaruntime_kafka_test.go` 后为 `27/6`），
合计与上面的 `169/34` 按默认构建计，详见第 2 组。
`-v` 动态口径下本服务为 logic `105/110`、publisher `25/28`、model `30/5`，均 0 FAIL / 0 SKIP。

覆盖的是**纯计算链与读写投影**：池条目合并去重、按预算逐路降级、指针解析、CAS 切换编排、
台账与幂等标记、outbox 行的判形与状态推进。它**不是**召回质量：没有真实特征仓库（缺口 A2/A3 的两个 stub 恒不可用），
也**没有**线上 A/B；它同样**不是**投递成功：没有任何 broker 参与（缺口 B6a）。

### 1. `internal/logic`（9 个文件含 `fakes_test.go`）— `105/24`

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `publishrollbacklogic_test.go` | 20/3 | 切换编排（`PublishPoolVersion`/`RollbackPoolVersion` 共用 `poolswitch.go`）：首次上线四写同提交（条目版本行 + 指针 + outbox + 幂等标记）、旧 CURRENT 退役、CAS 失败既不得覆盖也不得报成功、事件写失败整事务回滚、幂等重放（空 payload 必须大声失败）、同键不同版本拒绝、publish/rollback 作用域隔离、租约过期允许重执行、`RollbackPoolVersion` 接受从未上线的 READY 版本的**已知缺口哨兵**（缺口 C9，生产修好后该用例必须同步改） |
| `recallcandidateslogic_test.go` | 19/7 | 在线召回：入参门禁与门禁顺序、游客裁剪、合并去重与粗排（不手抄期望序，池内容先落成测试数据）、limit 裁剪与计数、生产 stub 下可见性 **fail-open 原样下发**（缺口 A3 的行为哨兵）、拉黑过滤器从未被调用（缺口 A4）、预算取消、降级开关、审计行可回放且不含设备标识、审计写失败不外抛 |
| `poolreadlogic_test.go` | 15/10 | 运维面读（"为什么没出数"的唯一排查入口）四条口径：门禁拒绝时一次依赖都不碰且门禁间有可判别的先后序、`CURRENT` 只能经 `recall_pool_current` 指针解析（`state=CURRENT` 是可重建冗余镜像）、`GetPoolSnapshot` 显式版本跳过指针读且未上线是错误而非空成功、分页 `has_more` 用登记 `item_count`、`published_at` 取版本行镜像（缺口 C10）、`ListPoolVersions` 负 limit 归一到配置上限（缺口 C11）、RETIRED 过滤下推、current 取指针而非最新行 |
| `upsertpoolitemslogic_test.go` | 17/2 | 离线写入语义：条目与版本行的关系（首批登记 BUILDING、末批封版 READY）、门禁先于任何依赖调用、批次归属复用即拒、分批累计 `item_count`、封版冲突整事务回滚（对手已提交状态必须留下）、中途故障不留半版本行 |
| `rrcprune_test.go` | 12/0 | 不可逆删除路径：参数逐项拒绝与闸门顺序、窗口内无可清时全零、条目先于版本行同事务删除、`dry_run` 零改动、指针版本即使在 RETIRED 镜像下也受保护、事务内 `FindForUpdate` 复核挡住并发发布、行已被他人删除可容忍、`max_rows` 命中只删条目、SQL 状态条件作为最后一道、事务失败两种删除一起回滚、候选页取满时 `has_more`、ACTIVE/READY 永不算候选 |
| `rrclog_test.go` | 10/0 | 召回审计回放：两个 ID 都缺才报 `ErrRequestLogRequired`、两者都给以 `request_id` 为准、17 列全字段投影、未命中与存储故障严格区分、控制文本畸形必须上抛、`Count`/`List` 共用同一谓词、单行解析失败整页报错、nil-Repository 不对称哨兵（缺口 D13） |
| `rrcconfig_test.go` | 9/0 | 在线参数面：下发值与真实校验值同源（改配置后逐项比对，无业务字面量）、游客裁剪与召回路径共用同一判定、负 TTL 钳到 0 并留日志、`ready_pools` 以指针为真值且明细批量取（无 N+1）、指针无版本行必须 `ErrVersionNotFound`、`MaxReadyPools` 上界守卫、`item_count` 超 int32 报错 |
| `contract_consistency_test.go` | 3/2 | 「model 常量与生成枚举同源、实现文件与契约方法同数」两类必然漂移点：降级原因 key、召回路枚举与 rpc 常量、幂等 scope 常量两侧一致（任一侧单独加值即失败） |
| `fakes_test.go` | 0/0（替身层） | 见第 5 组 |

### 2. `internal/publisher`（5 个测试文件）— 默认构建 `25/5`，`-tags recommendrecall_kafka` `27/6`

两个构建的测试文件集互斥（`//go:build !recommendrecall_kafka` / `//go:build recommendrecall_kafka`），
所以同一个包在两种构建下数量不同；`-v` 动态口径分别 `25/28` 与 `27/32`。

| 文件 | 构建 | 顶层/子 | 钉住了什么 |
|---|---|---|---|
| `outbox_store_test.go` | 两者 | 14/4 | 存储适配与端到端一轮：`RequiredTopic()` 与 `model.TopicPoolPublished` 同源（改常量不改配置就红）；**声明事件类型必须被 `RequiredTopics()` 全覆盖**（`declaredEventTypes` 直接扫 `model/*.go` 源码里的 `Event* = "…"` 常量，找到 0 个或 1 个以外的值都 `t.Fatal`，防止"因为没有事件类型所以全部通过"的空转）；`toRow` 的列映射与畸形判定（9 个子用例：别的服务的事件、schema v2、空 event_type/event_id/聚合根、非 JSON payload、缺 producer、payload 与列的 event_id/aggregate_id 不一致，全部落 `Defect` 而不是丢弃）；`ListPending` 的 `now/limit` 原样透传与错误必须点名 `recall_outbox`；三个 `Mark*` 逐位委托（`MarkRetry` 的引擎次数不得写进 `next_retry_at`，位置错就会被当成"立即可投"）；0 行（条件未命中）只记日志不外抛，而真错误必须冒泡且带 id 与表名；`RunOnce` 走真实 `OutboxStore` + 录制型 `fakeSender` 的完整一轮：topic/分区键/payload/超时上界/发布位点与调用序列（`ListPending → MarkPublished:21`），发送失败 → `MarkRetry`，判死 → `MarkFailed`，外来事件直接判死，空批次一次状态写都不碰 |
| `params_test.go` | 两者 | 6/1 | 配置到引擎参数的映射与门禁：`SenderSettingsFrom` 去空白/去重/复制（不改写配置切片）；`OptionsFrom` 七个键逐字段核对（`2s/100/5/2s/1800s/5s`，漏一个键就是"配置写了没用"）；`ValidatePublishKafka` 的 14 个子用例逐条点名失效键（含本服务特有的一道：`BatchLimit=1001` 必须拒，因为 `recall_outbox.ListPending` 对超限报错、每轮报错的发布器等于没接；边界 `1000` 必须过，防止把守卫写成"永远拒"）；错误聚合（一次报出全部失效键，而不是修一个才看见下一个）；`NewPublisher` 的四个出口（库为 nil、发送端为 nil、配置不完整、配置合法）与 `Options()` 透传、构造后 `!Running()`；**配置层与引擎层的分工**：`MaxRetries=1<<31` 能过本服务校验，必须由引擎拒绝且错误同时点名 `Options.MaxAttempts` 与 `Kafka.MaxRetries` |
| `example_yaml_test.go` | 两者 | 3/0 | 真实 `conf.Load("../../etc/recommendrecall.v1.yaml")`（不是手写结构体）：钉住示例配置是"只差一个开关"而非"缺参数"（`Enabled=false`、但 `ValidatePublishKafka` 过、`PublishTopics` 与 `RequiredTopics()` 同序、`Brokers` 非空、`BatchLimit ≤ MaxOutboxBatch`、`OptionsFrom` 七个字段逐项等于预期）；用这份配置真的构造发布器并跑一轮投递（断到 `key == "1:hot:51"`、`0 < timeout ≤ 5s`、发布位点落在调用窗口内）；顺带钉住同一份 yaml 的其余小节仍可加载（`Recall.Validate()` 通过、`EnabledSources` 非空、幂等保留期 > 租约） |
| `kafkaruntime_disabled_test.go` | 默认 | 2/0 | 默认构建必须"显式失败而不是安静不发"：`NewSender` 任何参数都返回 `ErrKafkaRuntimeNotBuilt`，且错误文本里同时出现 `-tags recommendrecall_kafka`、broker、`Kafka.Enabled`（运维照错误就能操作）；`RuntimeNotes` 两分支都说明"未链接"，`Enabled=true` 分支不许出现"已链接/已在投递"，必须点出 `recall_outbox`、`RequiredTopic()` 和"池版本切换在下游无从生成投影" |
| `kafkaruntime_kafka_test.go` | 带标签 | 4/1 | 真实 `kq` 发送端在**不联网**前提下的可验证部分：构造期按 topic 建立同步通道并去重到 1 条、`Close()` 幂等且清空 `Topics()`；四类不完整设置构造期即拒（空 brokers、broker 含空串（子用例逐个点名索引）、无 topic、topic 含空串）；`Send` 在未登记 topic 与空分区键两种情况下**在拨号之前**返回错误（绝不静默丢事件，也不会因为"测试里没有 broker"而挂住）；`RuntimeNotes` 两分支都强调"已链接 ≠ 已验证"，本仓库无 broker 证据、该 topic 尚无消费者 |

发布链路的**口径**（不是缺口，是刻意选择，改任何一条都要先动这些断言）：

- 分区键取 `aggregate_id`（形如 `source:pool_key:version`），不是 `event_id`：同一池版本的多个事件要落在同一分区才有相对顺序。
- 只有 broker 返回成功才写 `state=已发布`（`kq.WithSyncPush()`）；异步模式下 kq 把失败只写日志、调用方拿到 `nil`，等于把"没送出去"写成"已发布"。
- 判死是单向的：`MarkFailed` 只从待发布出发；`MarkPublished`/`MarkRetry` 的 `WHERE state IN (待发布, 失败)` 允许救回已被判死的行，但不允许改动已发布行。
- `retry_count` 由 SQL 侧 `+ 1`，不是引擎把内存值写回去（多副本各自覆盖会互相丢计数）；因此 `MarkRetry` 的引擎入参 `retryCount` 被刻意忽略，`outbox_store_test.go` 有专门用例钉住"它不会串位到 `next_retry_at`"。
- 本服务的 `ListPending` 对 `limit = 0` 或 `> model.MaxOutboxBatch` 直接返回错误（`ErrInvalidLimit`/`ErrLimitTooLarge`），不像 live-media 那样钳制成默认 100：这里的口径是"配置错了必须显式失败"，配套的门禁在 `ValidatePublishKafka`。
- `last_error` 在 Go 侧按 512 字节裁剪且**回退到 UTF-8 字符边界**（`trimLastError`）：半个多字节序列写进 `utf8mb4` 列会让状态写库自己失败，引擎把写库失败当整批中断，故障面从一行放大到一批。

### 3. 其他层（同口径实测）

- `model/` — **2 文件 `30/2`**：

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `migration_parity_test.go` | 12/1 | 迁移 DDL ↔ model 的**双向逐列**静态对账（纯解析文件、不连库，见「数据与迁移」一节） |
| `outbox_sql_test.go` | 18/1 | `recall_outbox` 的 SQL 文本与实参逐位（录制型 `sqlx.SqlConn`/`sqlx.Session` 替身，不执行 SQL）：读侧只取到期待发行且**出现 `state IN` 即失败**（接线前的死信重投缺陷，缺口 B6）、`limit` 的 `0/-1/1001` 与 `1000` 边界（`ErrInvalidLimit`/`ErrLimitTooLarge`，且拒绝时一条 SQL 都不发）、`ErrNoRows` 折叠成空而真错误必须点名表名、`MarkPublished` 用 `mtime` 承载发布位点且 `publishedAt=0` 回落注入时钟（写 0 会被清理条件排除）、0 行是业务结论不是错误、`RowsAffected` 拿不到必须报错（不能塌成"没行"）、`MarkRetry` 七个实参逐位（SET 段保持待发布态、`retry_count = retry_count + 1` 是 SQL 表达式不占位）、`MarkFailed` 的 `WHERE` 只有待发布、`CountPending`/`CountStuck` **刻意**与候选集口径不同（统计含死信，运维要看得见堆积；两查询若被"顺手合并成一份 WHERE"即红）、`DeleteSentBefore` 只碰已发布且 `maxRows > 5000`/`before = 0` 都拒、`trimLastError` 的三档字节边界（512 纯 ASCII / 511+汉字跨界 / 510 全中文）与落库实参合法、`Insert` 的 4 个必填项在**发 SQL 之前**拒、13 列列序与实参逐位（错一位就是把 payload 写进 `aggregate_id`）、`schema_version=0` 回填 1、传入 session 时必须走调用方事务（全局连接计数为零）而不是自动提交、1062 映射 `ErrEventExists` 而非重复错误 |
- `internal/repository/` — **1 文件 `6/1`**：`repository_test.go` 的 `TestNewRejectsNilConn` 钉构造期失败关闭
  （本服务所有方法都要读 MySQL，没有「没库也能起」的路径）+ Options 校验与 stub 数据源的显式不可用语义。
- `internal/config/` — **1 文件 `3/2`**：`config_load_test.go` 加载 `etc` 示例并跑 `Validate()`
  （非法组合启动即 panic）。
- 本服务没有 `internal/consumer` 目录（`recall.pool.published.v1` 无订阅方，见缺口 B6b）；
  发布器在 `internal/publisher/`，其装配方 `internal/svc/` **无离线单测**：
  `startPublisher`（`servicecontext.go:91`）的三种启动结果与 `Stop()` 幂等只在代码与
  启动日志里可核对，`proc.AddWrapUpListener` 的接线无法在进程内断言。
  `internal/server/`、`rpc/*.pb.go` 是 goctl/protoc 生成壳，不在单测范围。

### 4. 构造器级覆盖

`internal/logic` 的 10 个 RPC 构造器 **10/10**（探针 `PROBE 10 gaps:` 后为空，无缺口名字），
每个方法都有直接调用其构造器的用例。`internal/publisher` 的公开构造器
`NewOutboxStore`、`NewSender`、`NewPublisher`、`SenderSettingsFrom`、`OptionsFrom`、
`ValidatePublishKafka`、`RequiredTopic`/`RequiredTopics`、`RuntimeNotes` 也全部被直接调用
（**9/9**，两种构建各自成立；带标签构建下 `NewSender` 走真实通道，默认构建走未就绪分支）。

### 5. 替身层与断言口径

`internal/logic/fakes_test.go` 不产出任何生产代码，替身复刻的是 SQL 与并发编排的**语义**：
唯一键冲突即 `reused`/拒绝、CAS 条件不命中即 `applied=false`、`ORDER BY + LIMIT` 与 WHERE 条件、
事务内 `FindForUpdate` 复核、批次归属与幂等标记；真实并发用 `store.commitExternally`
注入一份对手变更来近似。拒绝类用例断「零次 recorded op」而不是「返回了错误」。

它证明不了什么——三条已在「已知缺口 E 组」逐条登记，本节只索引不重复：
E14 缓存路径无离线覆盖（`redis.Redis` 是具体类型，注入缝里 `Cache` 恒为 nil，
所以所有池读用例的主降级原因是 `store_unavailable`）；E15 `kept_items_remain` 结论在进程内不可达
（替身的删与数读同一张 map）；E16 model 层只有 `outbox.go` 有 SQL 文本单测，其余四个 model 文件仍只做静态对账。

`internal/publisher` 与 `model` 的替身是另一类：**录制型**而不是语义型。
`outbox_store_test.go` 的 `fakeOutboxModel` 记录实参供逐位比对（嵌入 `model.RecallOutboxModel`，
未覆盖的方法一旦被调用直接 panic）；`model/outbox_sql_test.go` 的 `obxSQL`/`obxTx` 只把 SQL 文本与
实参存下来，**不解析、不执行**，所以它能证明"发出去的语句长什么样"，
证明不了"MySQL 执行它得到什么"（执行计划、行锁、字符集折叠都看不到）。
`kafkaruntime_kafka_test.go` 会真的构造 `kq.Pusher` 对象（进程内，惰性连接），但一条都不 `Send`
到已登记的 topic，因此不会拨号、不会挂住，也不构成 broker 证据。

### 6. 覆盖边界

- 用例不连接 MySQL / Redis / etcd / Kafka / OpenSearch / 对象存储，也不启动 gRPC server。
  带标签构建会构造真实的 `kq.Pusher` 对象，但不对已登记 topic 发送，因此仍不产生网络流量。
- **没有真实特征仓库**：`Features` 与 `Visibility` 在生产是 `UnconfiguredFeatureSource{}`（缺口 A2/A3），
  标签路、协同路、向量路永不出数；用例因此钉的是「stub 不可用时的 fail-open 与降级记录」，
  不是「特征参与过召回」。**没有线上 A/B**：本服务契约里没有实验维度。
- **没有调用方、没有 broker 证据、没有清理与调度方**：`RecallCandidates`/`GetRecallConfig` 全仓零调用点（缺口 A1）；
  `recall.pool.published.v1` 的发布链路本轮已接线（`internal/publisher` + `common/outbox`，装配在 `internal/svc`），
  但**从未在真实 broker 上验证过**，且该 topic 全仓没有消费者，已发布行也没有清理作业（缺口 B6a/B6b/B6c，
  默认构建与示例配置都停在「不投递」）；`PrunePoolVersions` 与 `RequestLogRetentionSeconds` 无 cron 排它（缺口 B7/B8）。
  这几条是缺口事实，本节不把它写成"事件已在飞"。
- 迁移 SQL：本 README「数据与迁移」记 `go_video_recommend_recall` 6 条 `applied`，
  列级一致性只有 `model/migration_parity_test.go` 的静态对账；**未在目标实例（含隔离实例
  `127.0.0.1:3399`）做列级/EXPLAIN 复验**。
- `internal/svc`、`internal/server`、`rpc/*.pb.go`、入口 `recommendrecall.v1.go` 不在单测范围内。

### 7. 验证命令

```bash
# 默认构建（publisher 25/28、model 30/5、logic 105/110，均 0 FAIL / 0 SKIP）
go test -mod=readonly -p 1 -count=1 ./services/recommend-recall/...
go vet -mod=readonly ./services/recommend-recall/...
gofmt -l services/recommend-recall       # 必须无输出

# 带队列运行时的构建：publisher 换一套文件（27/32），其余包不受标签影响
go build -mod=readonly -tags recommendrecall_kafka ./services/recommend-recall/...
go vet -mod=readonly -tags recommendrecall_kafka ./services/recommend-recall/internal/publisher/
go test -mod=readonly -tags recommendrecall_kafka -p 1 -count=1 ./services/recommend-recall/internal/publisher/
```

`-p 1` 是硬要求：Windows 页面文件限制下并发编译/运行多个测试包会 OOM（`errno=1455`），
测试门禁一律串行跑包（见 docs/commands.md）。两个构建都要跑：只跑默认构建时，
`kafkaruntime_kafka.go` 连编译都没被验证过。
