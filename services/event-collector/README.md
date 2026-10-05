# event-collector

用户行为事件（AGENTS.md §7 的 SPM 语义 = 行为分析链路）的**采集入口**。只提供 gRPC
（`rpc/eventcollector.proto`，`eventcollector.v1`），无 `.api`：对端 SDK 的 HTTP 入口、
统一鉴权与响应信封都在 `gateway/app`。

不含任何广告位、投放、会员、订单、支付、投币、分成字段。

## 职责

1. 接收客户端 SDK 批量上报（`CollectEvents`）与服务端内部埋点（`IngestServerEvents`）。
2. 逐条校验：必填字段、schema 版本区间、时钟偏差与回补窗口、内容主键与归属主体、
   payload 白名单/禁止字段与大小上限，结论按 `rpc.RejectReason` 稳定枚举归类。
3. 隐私脱敏：设备号 → `model.SaltedHash`（HMAC-SHA256 + 盐版本），出口 IP →
   `model.IPSegment`（默认 /24）；**顺序是先校验、再脱敏、再采样、最后落库/投递**。
   明文只活在请求生命周期内，不入库、不进投递信封、不打日志。
4. 采样与去重：`event_id` 唯一键逐条去重，`batch_id` 唯一键批次幂等，
   采样按 `event_id` 做确定性判定（重放同一批得到同一结论）。
5. 投递推进：Outbox（`ec_pending_delivery`）+ 租约 + 指数退避 + 死信（`ec_dead_letter`）
   与人工重放（带操作人与理由的审计链）。
6. 策略版本化：采样/脱敏/大小上限参数收敛成 `ec_dispatch_policy` 版本，
   每条台账都记录当时生效的 `policy_version`/`salt_version`，保证事后可归因。

## 数据所有权

- 独占库 `go_video_event_collector`（`deploy/migrations/event-collector/`）。
  其他服务（含 `spm`、`cron`、`operation`）只能走 gRPC，禁止直连本库表或本服务 Redis key。
- 本服务不拥有行为事实：行为分析的事实源是 MQ topic（`behavior.<category>.v1`，
  由 `common/eventenvelope` 封装、`spm` 消费）。本库是**接收与投递台账**，不是分析表。
- 不写任何业务主库；事件不同步落 video/engagement 等表。
- 隐私分级：`device_hash`、`ip_segment`、`mid`、`keyword_digest`、`payload_digest` 都是
  不可逆摘要或脱敏段；盐值只在 Secret/环境变量（`Privacy.SaltRef` 只存变量名）。
  盐缺失时哈希路径返回 `model.ErrSaltMissing`，不退化成无盐哈希、不落明文。

## 表

| 表 | 作用 | 幂等/不变量 | 可重算 |
|---|---|---|---|
| `ec_ingest_batch` | 批次接收台账（来源、上下文脱敏列、结论汇总、状态） | `uniq_batch_id(batch_id)` | `total/accepted/duplicated/rejected/sampled_out/dispatched/dead` 为投影，`model.RecountFromRecords` 从事件台账重算 |
| `ec_event_record` | 逐条事件的校验结论与投递状态（正文只存摘要/对象存储引用） | `uniq_event_id(event_id)` | 是 `ec_ingest_batch` 计数的事实源；`delivery_state` 是投影，真值在 `ec_pending_delivery` |
| `ec_dispatch_policy` | 采样/脱敏/大小上限/退避与留存的版本化配置 | `uniq_version`、`uniq_active(active_flag)` 保证同一时刻至多一版 ACTIVE | 不是投影：ACTIVE 不可原地改，历史版本永久保留供归因 |
| `ec_pending_delivery` | MQ 投递 Outbox（退避时间 + 租约） | `uniq_event_topic(event_id, topic)` | `state=SENT` 历史行按留存清理 |
| `ec_dead_letter` | 投递死信摘要与人工处置审计（`operator/replay_key/replay_reason`） | `uniq_event_topic(event_id, topic)`；只从 `open` 迁移到终态 | `open` 行永不自动清理（保留证据） |

单生效版本的实现细节：ACTIVE 行写 `active_flag=1`，其余写 `NULL`
（MySQL 唯一索引允许多个 NULL），因此并发切换的第二笔必然撞唯一键 →
`model.ErrConcurrentUpdate`。切换必须跑在调用方事务里（`model.Activate` 在
`session == nil` 时直接拒绝），否则会留下「零个或两个 ACTIVE」。
该列刻意不进 model 查询投影（NULL 扫进 `int32` 语义不清），读写一律用 `state`。

## 方法 ↔ 幂等键

| RPC 方法 | 幂等键 / 约束 | 备注 |
|---|---|---|
| `CollectEvents` | `batch_id`（客户端生成）+ 逐条 `event_id` | 条数/字节双上限；重复批次回放首次结论；`client_seq` 识别重发 |
| `IngestServerEvents` | `batch_id` + `idempotency_key`（调用方动作键）+ `event_id` | 不采样、`trace_id` 必填、只接受 `SOURCE_SERVER` |
| `ValidateEventSchema` | 只读干跑，不落库不投递 | 与采集共用同一套校验函数，禁止两套规则 |
| `GetIngestBatch` / `ListIngestBatches` | 只读；`batch_id` / `(ctime,id)` 游标 | 只输出脱敏列；`ip_segment` 过滤器必须带前缀长度（/32 拒） |
| `GetEventRecord` / `ListEventRecords` | 只读；`event_id` / `(ctime,id)` 游标 | 台账非行为事实源；`GetEventRecord` 用 Outbox 覆盖 `delivery_state`，列表不覆盖 |
| `RetryPendingDelivery` | `idempotency_key`（同一轮只一个执行权）+ `uniq_event_topic` + 投递租约 `lease_owner/lease_until` | 由 `services/cron` 兜底；退避 `NextRetryAt`，超限转死信；`limit` 超 `Collector.BatchLimit` 报 `ErrBatchLimitTooLarge` 而非静默截断 |
| `ListDeadLetters` | 只读 | `state` 白名单校验；必须给 topic/state/时间窗之一 |
| `ReplayDeadLetter` | `idempotency_key` + `uniq_event_topic` | `operator`/`reason` 必填；只迁移 `open`，其余计 `skipped`；逐条回 `replayed/skipped/failed_ids`，不报 blanket success |
| `UpsertDispatchPolicy` | `idempotency_key` + `uniq_version` | 只能创建/修改 DRAFT；数值只允许比 config 更严（不允许放宽） |
| `ActivateDispatchPolicy` | `expected_current_version` 乐观校验 + `uniq_active` | 单事务原子切换，旧版转 ARCHIVED；盐不可用时切换前即 `ErrSaltMissing` |
| `GetActiveDispatchPolicy` / `ListDispatchPolicies` | 只读 | 无 ACTIVE 外抛 `model.ErrNoActivePolicy`（不回「空策略 = 全量」）；列表上限 500 且只接受 `0_<id>` 游标 |
| `GetCollectorHealth` | 只读 | 策略缺失 → `active_salt_version=0` 即不健康；`salt_available` 只回 bool |

## 运行

```bash
go run ./services/event-collector -f services/event-collector/etc/eventcollector.v1.yaml
go test ./services/event-collector/... -count=1
```

- gRPC 端口 `8152`，etcd key `eventcollector.v1.rpc`。
- 配置：`DataSource`（`go_video_event_collector`）、`CacheRedis`（业务 Redis 字段一律叫
  `CacheRedis`，叫 `Redis` 会与 `zrpc.RpcServerConf` 内嵌字段冲突，能编译但启动即挂）、
  `Collector`（上限与限流）、`Privacy`（盐版本与 `SaltRef`）、`Dispatch`（MQ 地址、批量、
  退避与租约）。`etc/eventcollector.v1.yaml` 是本地示例，密钥留空，真实值走 Secret/Vault。
- 启动期自检 `Config.Validate()`：采样基点、IP 前缀（/32 等于明文）、回补窗口、
  退避与租约关系、留存上限等越界即 `Severe` 终止启动。
- 迁移：`deploy/migrations/event-collector/000001_*.sql`、`000002_*.sql`，
  由 `scripts/migrate.ps1` 在隔离实例执行（见 `docs/commands.md` §8）。

## 当前阶段

- **15 个 logic 已全部实现**，`internal/logic/` 下不再有 `model.ErrNotImplemented`
  （该哨兵仍保留在 `model/errors.go` 里备用）。goctl 生成的
  `return &rpc.XxxReply{}, nil` 假成功已全部删除。
- 手写 companion（与 goctl 文件同目录，不含任何可再生成代码）：`helpers.go`
  上限/盐/ACTIVE 策略解析/幂等抢占，`ingest.go` 采集主流程，`conv.go`
  model ↔ proto 投影，`ops.go` 写类 RPC 的轮次幂等（`withRoundDedup`）与错误文本
  脱敏，`reads.go` 分页窗口与过滤条件白名单，`policy.go` 策略入参校验，
  `dispatch.go` 投递机（租约 → 发送 → CAS → 退避/死信 → 批次收敛）。
- 写类 RPC 幂等是两层：Redis 轮次围栏（`govideo:ec:op:<method>:<key>`，600s）
  只是加速器，真值在 MySQL 唯一键与状态机 CAS（`uniq_version`、`uniq_active`、
  `uniq_event_topic` + `lease_owner` + `WHERE state IN (...)`），清缓存后重放
  得到同一结论。
- 读类 RPC 统一口径：`page_size` 走 `model.ClampPageSize`（上限
  `Collector.MaxPageSize`）、`(ctime,id)` 游标 + 多取一行探 `has_more`、时间窗最多
  90 天；「无过滤条件 + 无时间窗」的全表扫描直接拒（`model.ErrInvalidPage`）；
  未知 id 回 `found=false` 而 DB 错误原样外抛，不会伪装成空结果。
  列表里的 `delivery_state` 是台账投影（不逐行联查 Outbox，避免 N+1），
  只有 `GetEventRecord` 用 `ec_pending_delivery` 覆盖成真值。
- 出口脱敏：过滤参数本身也必须是「已脱敏形态」（`h1:` + 64 hex、带前缀长度的
  `ip_segment`、白名单 `topic`/`event_type`），否则
  `model.ErrPrivacyFieldForbidden` —— 它们会成为 SQL 参数进入通用/慢查询日志
  （AGENTS.md §7）；所有对外自由文本经 `redactSensitive` 去掉 kv 明文、IPv4/IPv6
  与超长串。
- **迁移 SQL 未在 MySQL 执行过**：列名/列序由 `model/migration_test.go` 做静态逐列比对兜底。
- **没有 MQ producer / consumer**：`internal/dispatcher`、`internal/consumer` 均未创建。
  投递机已在 logic 侧写完，唯一待替换的接缝是 `logic.dispatcherSender`
  （未实现前它 fail closed，见已知缺口 1）。`Dispatch.Endpoints` 示例为空 →
  事件只落 Outbox，等待 producer 落地。
- 未接入 `gateway/app`（采集入口需要单独评审限流与鉴权口径）。

## 已知缺口

1. **MQ producer（`internal/dispatcher`）未实现**，这是本轮唯一没能闭合的硬缺口。
   投递机（`logic.runPendingRound`：`ReapExpiredLeases` → `ClaimDue` → 逐行
   `Send` → 带 `lease_owner` 围栏的 CAS → 退避/死信 → 批次收敛）已经写完，
   但 `logic.dispatcherSender` 现在只返回 `model.ErrDispatcherMissing`。
   刻意让它 fail closed **在抢任何租约之前**：若先 `ClaimDue` 再报缺 producer，
   健康的 Outbox 行会被退避计数一路推到死信，那是伪造故障。
   接上真实 writer 只需替换这一个函数（其余路径不需改）。
   同一理由，`ReplayDeadLetter` 在**任何写之前**检查 sender 可用性，否则会把行标成
   `replayed` 却仍不投递，积压从看板上消失。
2. 事件 schema 登记表/校验真值未定（见下方疑点），`ValidateEventSchema` 目前只约定
   「与采集路径共用同一套校验」。
3. 计数漂移对账任务（`RecountFromRecords`）只在 model 层提供，缺 cron 侧调用方。
   本轮之后它从「可选」变成「必需」：死信重放成功后
   `ec_ingest_batch.dispatched` 会再 +1 而 `dead` 不回退（model 只有累加 API，
   契约内无回退位），只有按 `ec_event_record.delivery_state` 重算才能收敛；
   投影累加失败也只记 `logx.Errorf`（Outbox 是真值），留下的缺口同样由它补齐。
4. 台账清理（`DeleteBefore` / `DeleteSentBefore`）仍无执行者，需要 `services/cron`
   注册周期任务；策略里的 `retention_days` 已被解析进生效上限（`limits`）也确实没有
   任何一个调用方据它删行。
   （`ReapExpiredLeases` 已不再是缺口：投递轮开始时调用，回收崩溃实例遗留的租约。）
5. 限流目前只有配置位（`Collector.MidQps/DeviceQps/IPSegmentQps`）与进程级令牌桶，
   多维 Redis 计数窗口未实现，`risk-control` 联动未接：`ServiceContext.SPMClient()` /
   `RiskControlClient()` 已备好「未配置即 `Err*NotConfigured`」的出口，但没有任何
   logic 调用它们，所以多实例部署时各实例独立限流。
6. payload 归档对象存储（`blob_bucket`/`blob_object_key`）只有列，无实现。
7. 策略变更审计只有日志：`ActivateDispatchPolicy` 的 `operator`/`reason` 与
   `UpsertDispatchPolicy` 的操作人只写 `logx.Infof`（`reason` 过 `redactSensitive`），
   因为本库没有策略审计表而契约冻结、不能加表。死信有 `ec_dead_letter` 的
   `operator/replay_key/replay_reason` 可回溯，策略切换没有等价物。
8. `ec_ingest_batch.finished_at` 由 `Finish` 保证「只写一次」（`WHERE finished_at = 0`），
   因此重放死信让批次重新有在途事件时既不会重置也不会刷新完成时刻；
   批次状态能从 `DISPATCHED` 迁回 `PARTIAL`（出现死信时），但时间轴上的「最后一次
   收敛时刻」只能看 `mtime`。
9. 盐可用性只在 `ActivateDispatchPolicy` 强制（`ensureSaltUsable`），
   `UpsertDispatchPolicy` 只校验 `salt_version > 0` 与 `salt_ref` 是环境变量名形状：
   允许在密钥注入前先把新策略草稿写好，代价是「草稿期无法证明盐可用」。
   `GetCollectorHealth.salt_available` 也只代表**应答的那一个实例**能读到盐。

## 疑点（需要跨服务拍板）

- **事件 schema 归属**：`event_type` 白名单、`schema_version` 兼容矩阵与 topic 命名
  到底是 `event-collector` 自有配置，还是以 `services/spm` 为真值？本服务已按
  「白名单与采样规则在本服务 `ec_dispatch_policy`、schema 真值可选走 `SPMRPC`」实现契约，
  但 `Collector.SupportedSchemaVersion` 与 `spm` 的事件登记如果各写一套，
  会出现「采集放行、下游解析失败」。倾向：spm 拥有 schema 定义，本服务只缓存并执行。
- **是否自建 Outbox 发布器**：本表已按 Outbox 语义建（事件与投递意图同事务）。
  若 `common/kq` 侧要求生产者自带重试，就与本服务的 `ec_pending_delivery` + `cron` 兜底重复；
  若下游接受「至少一次 + 按 `event_id` 去重」，则保留自建发布器（可控退避与死信审计）。
  当前按后者设计，待与 `spm` 消费端确认后定稿。

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/etcd/MQ/对象存储）。数字为主代理实测导出
（`.gotmp/readme-metrics/event-collector.txt`、`.gotmp/readme-test-aggregate.txt`），
`grep -cE '^func Test'`（已排除 `TestMain`）/ `grep -c 't.Run('`，格式 `顶层/子用例`。

**分布要如实说**：本服务的用例重心**不在 `internal/logic`**——logic 层只有 1 个用例文件、
7 个顶层用例、0 个子用例；判定链的主体由 `model` 层的 22 个用例兜住
（`pure_test.go` 19 个纯函数：隐私哈希、IP 段、采样、退避、状态枚举；
`migration_test.go` 3 个列级比对）。把「构造器 15/15」读成「logic 覆盖充分」是错的，见第 3 小节。

### 1. `internal/logic` — `7/0`（1 个用例文件 + `fakes_test.go`、`testsupport_test.go`）

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `idempotency_test.go` | 7/0 | 幂等不变量一族：同 `batch_id` 重放不产生第二份台账与 Outbox 且回**首次结论**、同 `event_id` 跨批次只计 `duplicated`、单个请求内重复 `event_id`、`IngestServerEvents` 的 `idempotency_key` 必须与库存批次匹配、批次写冲突回滚后仍走重放、被拒批次重投复用同一行、`sampled_out` 重放结论不变（`TestCollectEvents_SameBatchIDReplayProducesNoSecondLedger`、`TestCollectEvents_SampledOutStaysSampledOutOnReplay`）。头注声明这些用例**全部跑在 `Cache=nil` 的形态**：Redis 短路只是加速器，真值在 MySQL 唯一键 |

`fakes_test.go` 与 `testsupport_test.go` 是替身层与断言工具（`top=0 sub=0`，属正常）。
其中 `testsupport_test.go:612-668` 是 15 个 `NewXxxLogic` 的**统一调用缝**
（`testEnv.collect/ingestServer/validate/retryPending/...`），构造器探针命中的就是这里。

### 2. 其他层

- `model/pure_test.go` — `19/0`：本服务真正的纯判定面。`SaltedHash` 稳定且不泄漏原值片段、
  `IPSegment` 掩掉主机位、payload/keyword 摘要不暴露正文、采样边界与生效采样率规则选择、
  `SchemaSupported` 版本兼容、事件类型归一、`NextRetryAt` 单调且封顶、死信判定、
  状态枚举互斥、策略状态机、`active_flag` 只在 ACTIVE、游标与分页口径、租约键不得为空、
  `IsNotFound`/`IsDuplicate` 归一、不可用策略拒绝、占位符与截断。
  头注自陈「只覆盖不连库的纯函数，任何需要 MySQL 的行为都不在这里断言（迁移未在实例执行）」。
- `model/migration_test.go` — `3/0`：结构体 `db` 标签 ↔ `deploy/migrations/event-collector`
  建表 SQL **同名同序**逐列比对、投影列表一致、占位符数量与列列表匹配；
  唯一例外是 `ec_dispatch_policy.active_flag`（只作 UNIQUE KEY 写入标记，不进查询投影）。
- `internal/config/config_load_test.go` — `4/1`：`etc` 示例逐个真实 `conf.Load` + 反射遍历字段 +
  `Validate()`；字段不得命名成 `Redis`；盐值不得提交进仓库；危险默认值必须被拒。
- `internal/svc/` **无离线单测**：`ServiceContext` 的 Redis/下游 client 装配、
  `SPMClient()`/`RiskControlClient()` 的「未配置即报错」出口都没有独立用例
  （「已知缺口」5 登记的正是这两个 client 无任何 logic 调用方）。
- 本服务没有 `internal/repository`、`internal/policy`、`internal/consumer`、`internal/dispatcher`
  目录（「当前阶段」已登记 producer/consumer 未创建）；`internal/server`、`rpc/*.pb.go`
  是 goctl/protoc 生成壳，不在单测范围内。

### 3. 构造器级覆盖

**15/15**（探针口径）：`internal/logic` 的 15 个 `New*Logic(` 全部被 `*_test.go` 引用，`gaps:` 为空。
但这 15 次引用**都来自 `testsupport_test.go` 的调用缝**，不是 15 组判定链断言——
`internal/logic` 只有 7 个顶层用例，集中在 `CollectEvents`/`IngestServerEvents` 的幂等族；
其余 13 个方法（`ValidateEventSchema`、`GetIngestBatch`/`ListIngestBatches`/
`GetEventRecord`/`ListEventRecords`、`RetryPendingDelivery`、`ListDeadLetters`、
`ReplayDeadLetter`、四个策略方法与 `GetCollectorHealth`）**没有专属用例**，
其判定正确性只由 `model/pure_test.go` 的纯函数与 `config` 的 `Validate()` 间接支撑。
这是本服务当前最明确的覆盖短板。

### 4. 替身层与断言口径

`fakes_test.go` 的 `fakeDB` 是五张表（`ec_ingest_batch`/`ec_event_record`/`ec_dispatch_policy`/
`ec_pending_delivery`/`ec_dead_letter`）的内存副本，语义对齐真实 model：
`uniq_batch_id`、`uniq_event_id`、`uniq_version`、`uniq_active`、`uniq_event_topic` 都以
「命中即静默跳过并回 `created=false` / inserted 行数不足」实现；`clock` 可注入固定时间，
让分页用例造出可预期的时间序；`txCalls/begins/commits` 记录事务边界。
`testsupport_test.go` 的断言口径：`wantErr` 只认 `errors.Is`（断的是同一条哨兵，不是字符串巧合），
`wantFail` 把「失败被伪造成成功」当成最坏的一类通过，`e.effects`/`e.requireSameEffects`
比对重放前后的副作用集合，布数据走 `seedBatch/seedRecord/seedPending` 静默入口。

证明不了什么：真实 SQL 文本、列名与索引命中；MySQL 唯一键的**并发**行为（替身单线程，
`TestCollectEvents_ConcurrentBatchWriteRollsBackAndServesReplay` 断的是回滚后的形状而非锁）；
`Cache`/`redis.Redis` 真身与 TTL 行为；驱动返回的 matched vs changed rows；
真实网络故障与 MQ 客户端语义。

### 5. 覆盖边界

1. 用例不连接 MySQL/Redis/etcd/MQ/对象存储，也不起 gRPC server。
2. **不验证 MQ 真实投递**：`logic.dispatcherSender` 现在只返回 `model.ErrDispatcherMissing`
   （已知缺口 1，刻意 fail closed 在抢任何租约之前），所以「消息发出去了」这类结论**没有任何用例
   可以断言**；Outbox 侧目前只在幂等族里以台账形态被断（`seedPending`/`pendingOf`/`mustPending`），
   投递轮次的退避与租约围栏 CAS 没有专属用例（见第 3 小节）。
3. **不验证下游消费方的幂等，也不验证跨服务事件闭环**：本库只是接收与投递台账，
   行为事实源是 `behavior.<category>.v1` topic（见「数据所有权」），而 `spm` 侧消费者、
   事件 schema 真值归属、是否自建发布器都还停在「疑点」节待拍板；
   `RecountFromRecords` 的 cron 调用方、台账清理的执行者同样不存在（缺口 3、4）。
4. **迁移未在目标实例复验**：「当前阶段」自陈迁移 SQL 未在 MySQL 执行过，
   `model/migration_test.go` 头注同样声明本轮未在实例执行；
   列名/列序只由那份静态逐列比对兜住，列宽、索引与真实 DDL 行为没有复验记录。
5. payload 归档对象存储只有列、无实现（缺口 6），因此「正文进对象存储、台账只存引用」
   这条设计目前无用例。
6. 全仓 `t.Skip` 实测口径中本服务为 0 条。

### 6. 验证命令

```bash
go test -p 1 -count=1 ./services/event-collector/...
gofmt -l services/event-collector   # 必须为空
go vet ./services/event-collector/...
```

`-p 1` 必须保留：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
「运行」小节的 `go test ./services/event-collector/... -count=1` 缺这个参数，按本节命令执行。
本节只描述用例断言范围，不构成任何门禁结论。
