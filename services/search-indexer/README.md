# search-indexer

搜索索引投影与零停机重建服务：把上游领域事件与显式推送的事实快照投影成 OpenSearch 内容索引，
并以「建新索引 + 别名原子切换」的方式完成零停机全量重建。

- **拥有数据**：本服务自有的 4 张表（`search_index_task`、`search_index_version`、
  `search_consumer_offset`、`search_dead_letter`，库 `go_video_search_indexer`）与 OpenSearch 内容索引/别名。
- **不拥有**：稿件、媒资、目录、版权、用户资料等主数据（AGENTS.md §5）。索引是投影，不是事实源，
  随时可由上游事件与 `_reindex` 重建。
- **提供能力**：`UpsertContentDoc` / `DeleteContentDoc`（单篇投影）、`SubmitRebuildTask` /
  `GetRebuildTask` / `ListRebuildTasks`（重建任务）、`SwitchAlias`（别名切换）、`GetIndexHealth`（巡检）。
- **依赖**：OpenSearch（HTTP，标准库客户端）、MySQL、Redis（可选）、领域事件
  `content.published.v1` / `engagement.action.v1`、上游服务的 RPC 推送（video / catalog / rights / engagement）。
- **约束**：所有写入按 `doc_revision`（毫秒事实版本）做 last-write-wins，旧版本永不覆盖新版本；
  重建与切换不得让查询别名出现空窗。

## 目录与生成边界

```text
rpc/searchindexer.proto         契约源文件（可手改）
rpc/*.pb.go, searchindexer.v1.go 生成物，禁止手改
internal/server/                goctl 生成的 gRPC server 适配层，禁止手改
internal/logic/                 手写用例（每个 RPC 方法一个文件）+ convert.go（rpc ↔ 投影映射）
internal/esclient/              OpenSearch 薄 HTTP 客户端（标准库实现，接口注入）
internal/repository/            MySQL 投影/流水 + OpenSearch 写入路径
internal/consumer/              事件消费、退避重试/死信状态机、重建执行器
internal/svc/                   依赖装配与 worker 生命周期（懒连接，构造不发网络握手）
model/                          本服务 4 张表的 go-zero model
etc/searchindexer.v1.yaml       本地配置示例（密钥一律留空）
```

改契约的唯一路径：编辑 `rpc/searchindexer.proto` → `powershell -File scripts/gen.ps1 -Service search-indexer`
→ 检查生成差异 → `go build ./services/search-indexer/...`。

## RPC 方法

| 方法 | 语义 | 关键约束 |
|---|---|---|
| `UpsertContentDoc` | 写入/覆盖一条内容投影 | `doc` 为空即报错（不猜字段）；`content_id` 与 `doc.content_id` 必须一致；旧版本返回 `outcome=skipped_stale` 而非报错 |
| `DeleteContentDoc` | 移除或降级投影 | `purge=false` 只把 `state` 置不可检索并推进 `doc_revision`（防止迟到的旧事件「复活」文档）；`purge=true` 物理删除；缺 `content_type` 直接报错，绝不「三种类型试一遍」 |
| `SubmitRebuildTask` | 登记重建任务 | `request_id` 必填且幂等（`uniq_request_id`），重复提交返回同一 `task_id`；提交阶段只写 MySQL，不被 OpenSearch 可用性阻塞 |
| `GetRebuildTask` / `ListRebuildTasks` | 查询任务与进度 | 列表按 `id` 倒序 keyset 分页，`cursor` 是上一页末行 id |
| `SwitchAlias` | 原子切别名 | 必须带 `expected_current` 做乐观校验；目标索引必须以 `<alias>_` 开头；空索引默认拒绝上线（`skip_health_check` 仅供紧急回滚） |
| `GetIndexHealth` | 别名/索引巡检 | 汇总登记状态、doc 数、集群 health、待重试与死信积压，给出 `ok/degraded/down` |

## 索引 schema 摘要

物理索引名 `<alias>_<schema>_<unix秒>`（全小写），文档主键 `<content_type>_<content_id>`
（不同内容类型各自发号，可能重号，必须带类型前缀）。mapping 由 `internal/esclient/mapping.go` 生成，
`mappings._meta` 携带 `schema_version` 与 `owner`，排障时无需靠索引名反推结构版本。

| 字段 | 类型 | 说明 |
|---|---|---|
| `content_id` / `content_type` | long / integer | 内容主键与类型（1 UGC、2 PGC 剧集、3 直播间） |
| `title` / `description` | text（写入 `go_video_title`、查询 `go_video_search`）+ `title.keyword` | 检索主字段；`title.keyword` 用于排序聚合 |
| `cover_url` | keyword（`index:false`） | 只存 CDN 地址，不参与检索 |
| `author_mid` / `author_name` | long / text+keyword（同上一行的中文分析器） | 作者昵称是快照，事实源仍是 user-profile |
| `typeid` / `type_name` / `tags` | integer / keyword / keyword | 分区与标签；`tags` 经去空白、去重、上限 32 归一 |
| `duration_sec` / `publish_at` / `ctime` | long | 时间与时长 |
| `state` | integer | 1 待审、2 已发布、3 下架、4 版权过期、5 删除；仅 `2` 参与公共检索 |
| `doc_revision` | long | 事实版本（Unix 毫秒），防旧覆盖新的守卫字段 |
| `heat.*` | object | `view/like/favorite/share/comment/danmaku_count` + `heat_score`(integer) + `heat_revision`(long) |
| `rights_expire_at` | long | PGC 版权窗口结束，0 表示不适用 |
| `language` / `subtitle_langs` | keyword | 语言与字幕 |
| `sensitive` | boolean | 审核敏感标记，由查询侧降权，不删除 |
| `schema_version` | integer | 文档结构版本，默认 1 |

settings：`refresh_interval=1s`、1 分片 0 副本（写入不逐条 refresh，重建收尾显式 `_refresh` 一次）。

### 分词器（`OpenSearch.Analyzer`）

`mappings._meta` 现在同时携带 `schema_version`、`analyzer` 与词典路径，排障时不用靠索引名反推结构。
索引结构（含 `settings.analysis`）只由 `internal/esclient.IndexBody` 生成；脚本要拿同一份 JSON 时
执行 `go run ./services/search-indexer/cmd/esmapping`，不要在 shell 里另抄一份 mapping。

| `Analyzer.Kind` | 写入 / 查询 tokenizer | 插件 | 说明 |
|---|---|---|---|
| `cjk`（默认） | `standard` + 内置 `cjk` bigram | 无 | 开箱可用，中文按二元组召回 |
| `ik` | `ik_max_word` / `ik_smart` | `analysis-ik` | 中文推荐；插件需自建镜像 |
| `smartcn` | `smartcn` / `smartcn` | `analysis-smartcn` | 缺插件时建索引直接报错，不静默降级 |
| `standard` | `standard` / `standard` | 无 | 中文不切分，仅限英文/ID 调试，禁止生产 |

词典：`Analyzer.StopwordsPath` 挂在**写入**分析器（改内容要重建索引），
`Analyzer.SynonymsPath` 只挂**查询**分析器并标记 `updateable`（改内容 reload 即可）。
文件在 `deploy/opensearch/analysis`，由 compose 只读挂载进容器；取值与验证方式见
[deploy/opensearch/README.md](../../deploy/opensearch/README.md)。
改分词族或词典结构等于改索引结构：递增 `OpenSearch.SchemaVersion` → 提交重建任务 → 切别名。

**隐私**：`ContentPublishedPayload` 只声明检索需要的字段，未声明字段在反序列化阶段被丢弃；
手机号、身份证、IP、Token 即使被上游误投也不会进入索引（AGENTS.md §7）。该行为由
`internal/consumer/mapping_test.go` 的 `TestDocFromContentEvent_DropsSensitiveFields` 锁定。

## 数据表与迁移

| 表 | 用途 | 幂等/索引 |
|---|---|---|
| `search_index_task` | 重建任务与断点游标 | `uniq_task_id`、`uniq_request_id`、`idx_state_id(state,id)` |
| `search_index_version` | 物理索引 ↔ 别名登记（唯一登记处） | `uniq_index_name`、`idx_alias_state`、`idx_state_mtime` |
| `search_consumer_offset` | 事件去重流水 + 退避重试队列 | `uniq_event_id`、`idx_state_next_retry(state,next_retry_at)` |
| `search_dead_letter` | 死信登记（只存 payload 摘要） | `uniq_event_id`、`idx_state_id`、`idx_state_ctime` |

迁移脚本：`deploy/migrations/search-indexer/000001_*.sql`、`000002_*.sql`（MySQL 8，含中文 COMMENT 与回滚说明）。
执行：`powershell -File scripts/migrate.ps1 -Service search-indexer`（需本地 MySQL，按 DSN 自动建库）。

## 事件消费、去重与死信

订阅 topic：`content.published.v1`（video/catalog/rights 发布、更新、下架、过期、删除）与
`engagement.action.v1`（互动计数快照）。处理链路：

1. 解析信封（`common/eventenvelope`，结构不合法直接判失败）；不支持的事件类型只记日志、不进死信。
2. `MarkEventReceived` 以 `uniq_event_id` 去重：重复投递返回 `duplicate`，跳过且**不写索引**。
3. 路由：`publish/update` → 整篇投影 upsert；`offline/expired` → 仅降级 `state`；`delete` → 物理删除；
   `engagement.action` → 只 `UpdatePartial` 更新 `heat`，不重建整篇（放大上游读压力）。
4. 状态机 `received → processing → succeeded / retry → dead_letter`，全部持久化在
   `search_consumer_offset`，**不依赖 MQ 重投**：`payload_json` 保存原文，进程重启后
   `RetrySweeper` 按 `next_retry_at` 继续指数退避（`RetryBackoffSec * 2^(n-1)`，上限 `MaxRetryBackoffSec`）。
5. 永久错误（未知 action、缺 `doc_revision`、缺热度绝对快照、payload 违规）直接转死信，不占重试配额；
   `MarkSucceeded/MarkDeadLetter` 会清空 `payload_json`，死信表只留 sha256 摘要前 32 hex。

**为什么互动不做「读-改-写累加」**：并发与重放都会双计，因此 `engagement.action` 必须携带绝对计数快照
+ `heat_revision`；缺失即 `ErrHeatSnapshotRequired` 转死信。正文投影未到（互动比发布更快）时
返回可重试错误，等 `content.published` 到齐。

**队列侧只有一条路径（2026-10-04 接线）**：`consumer.Handler` 实现 `kq.ConsumeHandler`，
`Consume(ctx, key, value)` 直接调 `Consumer.ProcessMessage`，**返回值就是位点决定**：

- 瞬时失败 → 返回错误 → `ForceCommit=false` 时不提交位点，Kafka 重投；
- 重投时 `search_consumer_offset` 已按 `uniq_event_id` 落行，`ProcessMessage` 判定重复并返回 `nil`
  → 位点前移，退避重试交给本进程的 `RetrySweeper`（权威在 MySQL，不在 broker）。
  这两半分别由 `TestHandlerConsumeKeepsOffsetUncommittedOnTransientFailure` 与
  `TestHandlerConsumeRedeliveryBecomesDuplicateAndCommits` 钉住；
- `kq` 的回调不给 partition/offset，因此流水表的 `partition_no`/`offset_no` 在这条路径下**恒为 0**
  （列注释「仅排障定位，不参与幂等判定」，见 `deploy/migrations/search-indexer/000002_create_consumer_offset_and_dead_letter_tables.sql:52-53`），
  由 `TestHandlerConsumeMapsCallbackAndDrivesStateMachine` 钉成「保持 0 而不是伪造位点」。
- Kafka 客户端只在 `-tags searchindexer_kafka` 的构建里编译（`internal/consumer/kafkaruntime_kafka.go`）；
  默认构建走 `kafkaruntime_disabled.go`，`NewKqFactory` 返回 `ErrKafkaRuntimeNotBuilt`。

死信重放：`search_dead_letter` **只存 sha256 摘要前 32 hex，不存原文**，所以没有「按位点回读重投」这条路。
运维入口是 `Repository.ListOpenDeadLetters`（按 `id` 升序，保持上游事件顺序）逐条取出 `event_id`/`topic`/
`reason`，由运维把原事件重新投回 `topic`（或改走 `UpsertContentDoc`/重建任务重新投影），
处理完成后 `MarkDeadLetterState(replayed|discarded)`；`state=open` 的行不得自动删除，
超 `DLQRetentionDays=30` 的**终态**行由 `services/cron` 归档后清理（AGENTS.md §8 审计要求）。

## 全量重建与零停机别名切换

```text
① SubmitRebuildTask(request_id, scope, alias)      → 只写 search_index_task(pending)，预分配 target_index
② RebuildRunner.ClaimNext                          → WHERE state='pending' ORDER BY id，再 CAS 置 running（多实例安全）
③ EnsureIndex(target_index)                        → 建索引并登记为 retiring（未激活，不承接写入）
④ 按 content_id 半开区间 [from, to) 切片 _reindex  → 来源固定是「别名当前 active 索引」，
   每片成功即 UpdateProgress 落 cursor_value        绝不直连上游库表（AGENTS.md §5）
⑤ 连续 StopAfterEmptySlices 个空切片判定结束        → 不依赖「最大 content_id」这种跨服务信息
⑥ RefreshIndex + CountIndex → GetIndexHealth 人工核对目标索引 doc 数与集群状态
⑦ SwitchAlias(alias, target_index, expected_current)→ 一次 _aliases 调用内 remove 旧 + add 新（add 放最后），
                                                      查询别名全程有指向 = 零停机
⑧ 旧索引转 retiring，观察期后 MarkIndexHistory → 运维清理
```

要点：

- 任务成功**不会自动切别名**：重建结果未经校验不得影响线上查询。
- 切片重复执行是幂等的（目标 `_id` 与源一致），进程退出时把任务标 `failed` 并保留 `cursor_value`，
  重提任务从断点续跑，不跳过任何区间。
- `SwitchAlias` 先切 OpenSearch 再更新登记表：若登记表更新失败，查询已经指向新索引，
  巡检接口会暴露不一致由人工补偿；反向顺序会造成「库里说切了但查询还在老索引」的静默丢失。
- 别名切换的 `expected_current` 是乐观校验：两个运维并发切换时，后者必须失败而不是静默摘掉前者的索引。

**回滚**：再次调用 `SwitchAlias`，`target_index` 填切换前的索引、`expected_current` 填当前（新）索引即可秒级回退
—— 旧索引在观察期内仍是 `retiring` 而非 `history`，文档未删。若新索引已经吃掉增量事件，回滚前先用
重建任务把这段时间的差量 `_reindex` 回旧索引，否则回退会丢增量。紧急场景（doc 数为 0 也要切）
才使用 `skip_health_check=true`，且必须在变更单里留痕。

## 配置 key 清单（`etc/searchindexer.v1.yaml`）

| Key | 默认 | 说明 |
|---|---|---|
| `Name` / `ListenOn` / `Etcd.Hosts`/`Key` | `searchindexer.v1.rpc` / `0.0.0.0:8106` / `127.0.0.1:2379` / `searchindexer.v1.rpc` | zrpc 内嵌段 |
| `Log.ServiceName` / `Mode` / `Level` | — / console / info | 日志；`trace_id` 由 go-zero 注入 |
| `CacheRedis` | 必填 | **不能命名为 `Redis`**：与 `zrpc.RpcServerConf` 内嵌的 `RedisKeyConf` 撞 key，会让 `conf.Load` 直接失败。只承担写索引解析缓存（`si:act:<alias>`,30s）与别名切换互斥（`si:sw:<alias>`,60s）；Host 为空时退化为单实例语义，幂等仍由 MySQL 唯一索引保证 |
| `DataSource` | 必填 | 本服务自有库 DSN（`go_video_search_indexer`），禁止指向上游库 |
| `Kafka.Enabled` | false | 是否随进程启动 Kafka 消费者。默认 false：默认构建没链接运行时，置 true 会启动即失败（返回 `ErrKafkaRuntimeNotBuilt`），而不是「安静地不消费」。打开前先 `-tags searchindexer_kafka` 构建并在 broker 上验证 |
| `Kafka.Brokers` / `Group` / `Topics` | — / `search-indexer.v1` / 两个 v1 topic | 消费订阅参数；`Topics` 为空、`Brokers` 为空或含空串都会被 `consumer.ValidateKafka` 点名拒绝 |
| `Kafka.Offset` / `Conns` / `Consumers` / `Processors` | `last` / 1 / 2 / 4 | 透传给 `kq.KqConf`：起点只接受 `first|last`，后三者是每 topic 连接数 / 每连接拉取协程 / 并发处理协程 |
| `Kafka.ForceCommit` | false | false=处理失败不提交位点（重投由 `event_id` 去重兜底）；改 true 等于允许丢消息，只在排障时临时用 |
| `Kafka.Username` / `Password` / `CaFile` | 空 | SASL 与 TLS 只放路径/由 Secret 注入，示例配置必须留空（`TestExampleYamlIsReadyToEnable` 会钉住这条）；Username 与 Password 必须成对 |
| `Kafka.MaxRetries` / `InProcessAttempts` | 5 / 2 | 累计尝试上限 / 进程内即时重试次数（后者会被夹到前者以内） |
| `Kafka.RetryBackoffSec` / `MaxRetryBackoffSec` | 5 / 1800 | 指数退避基数与上限 |
| `Kafka.RetrySweepIntervalSec` / `RetryBatchLimit` | 10 / 50 | 清扫器轮询与单轮到期事件数（2026-10-04 移除了 `PollBatchSize`：拉取式 `Consumer.Run`/`MessageSource` 已随单一推送路径删除，`Options.BatchSize`/`PollErrorWait` 同时消失） |
| `Kafka.RetrySweeperEnabled` / `RebuildRunnerEnabled` / `RebuildPollIntervalSec` | true / true / 15 | 本进程 worker 开关 |
| `OpenSearch.Endpoints` | 必填 | 为空时 `esclient.New` 直接失败，服务不启动写路径 |
| `OpenSearch.IndexPrefix` | `go_video_content` | 同时是默认查询别名（search-query 读同一个别名） |
| `OpenSearch.SchemaVersion` | `v1` | 结构版本；改 mapping 必须递增并走「新索引 + 切别名」 |
| `OpenSearch.Username` / `Password` | 空 | 密码只由 Secret/环境变量注入，不得提交 |
| `OpenSearch.AllowAnonymousWrites` | false | 空密码时写操作返回 `ErrWriteGuarded`，不静默成功；本地匿名集群自测才显式改 true |
| `OpenSearch.TimeoutMs` / `MaxRetries` / `BulkActions` | 5000 / 2 / 500 | 只对幂等请求（显式 `_id` 的读写与 bulk）重试；`_aliases`/`_reindex`/`_update` 不重试 |
| `OpenSearch.ReindexSliceSpan` / `ReindexSliceSize` / `StopAfterEmptySlices` | 100000 / 2000 / 3 | 重建切片参数 |
| `OpenSearch.RetryOnConflict` | 3 | 热度部分更新的服务端冲突重试次数 |
| `OpenSearch.Analyzer.Kind` | `cjk` | 分词族 `cjk`/`ik`/`smartcn`/`standard`；非法取值在 `esclient.New` 阶段即失败 |
| `OpenSearch.Analyzer.StopwordsPath` | 空 | 停用词文件（容器内绝对路径），挂写入分析器；改内容需重建 |
| `OpenSearch.Analyzer.SynonymsPath` | 空 | 同义词文件（容器内绝对路径），只挂查询分析器且 `updateable` |

## 启动方式（worker 与消费者在哪个进程）

```bash
cd services/search-indexer
go run ./searchindexer.v1.go -f etc/searchindexer.v1.yaml
```

- 单一进程 = gRPC server + 后台 worker：`Consumer.RunRetrySweeper`（到期重试清扫）与
  `RebuildRunner.Run`（抢占并执行重建任务），由 `internal/svc` 用 `Kafka.*Enabled` 开关启动。
- worker 挂在 `ServiceContext` 的 `workerCtx` 上：`proc.AddWrapUpListener` 在 SIGTERM 时
  **先 `Supervisor.Stop()`（kq 的 Stop 会等在途消息）再取消上下文**，重建任务把续跑游标与
  `failed` 终态落库，之后 gRPC 才优雅退出。
- **Kafka 消费者的三种启动结果**（`internal/svc/servicecontext.go` 的 `startConsumer`，
  与 inbox 同构）：
  1. `Kafka.Enabled=false` → 不碰 broker，只跑清扫器与重建执行器；启动日志写明
     「索引写入只来自 UpsertContentDoc/DeleteContentDoc RPC 与重建任务」；
  2. `Enabled=true` 且用 `-tags searchindexer_kafka` 构建且 `Kafka.*` 参数完整 →
     `Supervisor` 为每个 topic 起一个消费者，`ServiceContext.Supervisor` 非 nil；
  3. `Enabled=true` 但运行时未链接、或参数不完整 → `logx.Must` 终止启动。
     **不存在「配置写错就静默不消费」这条路径**，任何分支都会往日志写 `RuntimeNotes`。
- 生产入口不再是 `Consumer.Run`：拉取式 `MessageSource`/`Run` 已于 2026-10-04 随单一推送路径删除
  （全仓没有任何 `MessageSource` 实现，保留两条路径只会让 README 与代码互相打脸）。
  `ProcessMessage` / `SweepOnce` 仍是幂等可重入入口，`services/cron` 与死信重放继续直接调它们。
- 健康检查：本服务是 gRPC，无 HTTP 探针；用 `grpcurl` 调 `SearchIndexer/GetIndexHealth`，
  或依赖 etcd 服务发现（Key `searchindexer.v1.rpc`）。
- 依赖：MySQL（含迁移）、Redis（可选）、OpenSearch、etcd。

## 测试覆盖

离线单测（纯 Go 替身，不连 MySQL / Redis / OpenSearch / Kafka / etcd，也不需要网络）。
数字为 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。
本服务 0 条用例处于 `t.Skip` 状态。

### 1. `internal/logic`（9 个文件含 `fakes_test.go`）— `93/28`

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `getindexhealth_test.go` | 21/2 | 巡检作为排障入口的裁决：**单个依赖读失败进结果而不让接口失败**，登记表读不到必须直接失败；物理索引缺失=`down`、doc 数错误=`degraded`、集群非 green=`down`/`degraded` 分层；空登记表=down、未知别名过滤=down、`down` 优先于 `degraded`；重复 active 行去重、跳过非 active 行；积压统计一侧故障不掩盖另一侧；快照回写与别名目标失败被吞；`TestGetIndexHealth_IsReadOnly` 钉住「巡检不写任何东西」 |
| `switchalias_test.go` | 17/4 | 零停机切换的**顺序与缓存失效时机**：先在 OpenSearch 原子切别名再更新登记表，成功后立刻失效写索引缓存；首次挂载登记目标索引、已登记只刷 doc 数、目标已是唯一 owner 时 noop、`remove` 陈旧 + `add` 放最后；`expected_current` 乐观校验不符时零改动；空索引默认拒收（`skip_health_check` 才放行）；锁忙跳过全部 IO、锁错误上抛；ES 已生效而登记表失败时保留「已应用但缓存陈旧」的真实状态；无 Redis 降级 |
| `deletecontentdoc_test.go` | 13/8 | 「下架/删除只动投影」三件事：守卫先于任何 IO（坏入参断言调用轨迹为空）；`reason → state` 映射与 `purge` 物理删除两条路；降级必须推进 `doc_revision`（当前值+1）以防迟到旧事件复活文档；缓存命中跳过登记表读、miss 回源 active 索引、无 Redis 降级；投影不存在时跳过更新且不伪装成功；purge 重放幂等；「完全没建索引」是 no-op 成功；只处理入参指定的 `content_type` |
| `upsertcontentdoc_test.go` | 11/5 | 走真实 Repository 的投影写语义：`doc_revision` 的 last-write-wins 守卫（同版本重写放行、迟到返回 `skipped_stale` 且**不发写请求**）、`force_overwrite` 跳过 probe、首次使用 bootstrap 并登记 active 索引、索引名冲突加后缀重试、并发登记采纳他人索引、已登记但非 active 快拒、`content_id` 与 `doc.content_id` 不一致守卫、脏源不被当作缺失、依赖故障一律上抛 |
| `listrebuildtasks_test.go` | 10/3 | keyset 游标翻页、进度字段映射、`limit` 夹取、状态过滤**先校验后查询**、空状态=不过滤、坏游标在观测之前传播、**一页只读一次死信观测值**、观测失败逐行降级、空结果返回非 nil 切片、`TestListRebuildTasks_TouchesNoIndexOrCache` 钉住列表读不碰索引与缓存 |
| `submitrebuildtask_test.go` | 9/4 | 提交阶段只写 MySQL：`TestSubmitRebuildTask_RegistersTaskWithoutTouchingOpenSearch`；`request_id` 幂等重放返回原任务、并发抢跑采纳他人任务、命中但不可见则失败；必填与 scope 校验先于任何 IO；scope/别名归一；预分配目标索引名与 repository 命名同源（不在此建索引） |
| `getrebuildtask_test.go` | 6/1 | 任务本体查询与终态上报；未命中判定先于观测；空 `task_id` 拒绝；死信观测失败被吞而 `Find` 失败上抛 |
| `convert_test.go` | 6/1 | rpc ↔ 投影纯映射：`DocFromRPC` 要求快照、字段逐条映射、未知 `state` 拒绝；`TaskToRPC`、`OverallState`、`AliasHealthToRPC` 的 `ok/degraded/down` 结论 |
| `fakes_test.go` | 0/0（替身层） | 见第 4 组 |

### 2. 其他层（同口径实测）

- `internal/consumer` — **6 文件 `51/1`**（默认构建编译到 48 个顶层用例，`-tags searchindexer_kafka`
  是 49 个：两个 `kafkaruntime_*_test.go` 按标签互斥）：
  - `consumer_test.go`(17/0) 用 fake `Store` 驱动
    `received → processing → succeeded/retry/dead_letter` 合法迁移序列、去重命中**不写索引**、
    坏信封合成主键进死信、正文未就绪退避、热度快照过旧判成功、缺快照进死信、流水表故障必须冒泡、
    `SweepOnce` 状态推进与引擎恢复后重入；
  - `mapping_test.go`(12/0) 钉事件 → 投影逐字段映射、`doc_revision`/热度版本回退、敏感字段丢弃
    （`TestDocFromContentEvent_DropsSensitiveFields`）、标签归一、action 分类、topic 与 `occurred_at` 派生；
  - `retry_test.go`(6/0) 钉指数退避含上限与溢出保护（永不返回负值）、`NextState`/重试截止、
    永久错误分类、失败摘要截断、参数归一化；
  - `queue_test.go`(11/1) 钉 MQ 适配层这一道新接缝：`ValidateKafka` 的 12 条逐项拒绝（每个缺失字段
    必须在错误里点名本键，不许含糊）、`SettingsFrom` 把 `KafkaConf` 每个字段都映射进 `Settings`、
    `Handler.Consume` 把 kq 回调映射成状态机驱动（断言完整调用序列
    `received → processing:evt-0001 → upsert → succeeded:evt-0001`，且 `partition_no`/`offset_no`
    保持 0 而**不伪造位点**）、瞬时失败必须返回非 nil（`ForceCommit=false` 下位点不提交 → 重投）、
    重投经 `uniq_event_id` 去重后返回 nil（位点得以推进）、未绑定状态机时报错而不是静默成功、
    `NewSupervisor` 的四类入参守卫与 topic 去重/裁剪、**半启动回滚**（第二条队列启动失败时事件序列
    恰好只有 `stop:<第一条>`，且 `started` 仍为 false 可重试）、启动顺序与逆序停止与 `Stop` 幂等、
    工厂返回 nil 队列被拒、`TestExampleYamlIsReadyToEnable` 用 `conf.Load` 真读 `etc/` 示例配置并
    钉住 `Enabled=false`/`ForceCommit=false`/`Offset=last`/无 SASL 凭据，且只翻开关即可构造出
    订阅两条既定 topic 的 Supervisor；
  - `kafkaruntime_disabled_test.go`(2/0，`!searchindexer_kafka`) 钉默认构建**不链接** kq：
    `NewKqFactory()` 返回 `ErrKafkaRuntimeNotBuilt` 哨兵（错误文案必须同时含
    `searchindexer_kafka` 与 `broker`，即指向真实解法而不是假成功），且 `RuntimeNotes` 只说
    「未链接」，永不说「已启动 topics=」；`Enabled=false` 的注记必须点名 `UpsertContentDoc`
    这条真实写入路径，清扫器关闭时说明「不会被重投」；
  - `kafkaruntime_kafka_test.go`(3/0，`searchindexer_kafka`) 钉标签构建下工厂非 nil、
    空 topic / nil handler 在**触达 kq 之前**就被拒、注记说「已链接」而永不说「已在消费」。
  **注意**：这批用例证明的是消费判定/去重/退避状态机与**适配器契约**（`Handler` ↔ kq 回调的映射、
  位点与回滚语义），不是真实 Kafka 投递。本仓库从未与任何 broker 联调，见「已知缺口」1。
- `internal/esclient` — **6 文件 `38/3`**：`bulk_test.go`(9/1) `_bulk` NDJSON 行结构、delete 只有动作行、
  结尾换行、缺 `_id`/未知动作拒绝、`<>&` 不被 HTML 转义、逐项响应展平与 409/404 判定；
  `document_test.go`(9/1) 投影校验、防旧覆盖新、部分更新体只含 `heat`、mapping 与投影结构同源、
  敏感字段不出现在结构里；`mapping_test.go`(7/1) 四种分词族的 tokenizer 组合、默认策略不依赖插件、
  停用词挂写入侧而同义词只挂查询侧且 `updateable`、未配词典不留悬空引用、非法 `Analyzer.Kind`
  在建客户端阶段即报错、`_meta` 落地 `schema_version`/`analyzer`/词典路径；`aliases_test.go`(8)
  `expected_current` 不匹配、首次挂载、noop、**`add` 必为最后一个动作**、请求体字节级格式；
  `reindex_test.go`(2) `_reindex` 区间 `[from,to)`、scope 过滤叠加、`conflicts=proceed`；
  `sample_docs_test.go`(3) `deploy/opensearch/samples/content.sample.ndjson` 必须过 `ContentDoc.Validate`、
  主键互不覆盖、字段全在 mapping 内、不含敏感取值。
  **注意**：这批钉的是**索引体/请求体构造与分词配置装配**，不连 Elasticsearch/OpenSearch。
- `internal/repository` — **1 文件 `6/1`**：`repository_test.go` 钉投影层纯函数规则（`Options` 归一化、
  别名与物理索引命名、`ValidateScope` 取值、死信 payload 摘要算法、`SanitizeError` 单行化与截断、
  `Cache` 为 nil 时退化单实例语义）。
- `internal/config` — **1 文件 `1/1`**：`config_load_test.go` 的 `TestExampleConfigsLoad` 用 `conf.Load`
  真实加载 `etc/` 每个 yaml，回归「`CacheRedis` 不得命名成 `Redis`」（与 `zrpc.RpcServerConf` 内嵌的
  `RedisKeyConf` 撞 key，能编译但启动即报 `conflict key redis`）。
- `model/`（本服务 4 张表的 go-zero 模型）**无离线单测**；`internal/svc/`（依赖装配与 worker 生命周期）
  **无离线单测**；`internal/server/` 是 goctl 生成壳，不在单测范围。

### 3. 构造器级覆盖

`internal/logic` 的 7 个 RPC 构造器 **7/7** 有构造器级用例（探针 `PROBE 7 gaps:` 后为空，无缺口）。

### 4. 替身层与断言口径

`internal/logic/fakes_test.go` 的组装口径：logic → **真实 Repository**（`repository.NewWithDeps` /
`NewWithModels`）→ 手写 fake（model 四张表 + `esclient.Client` + 缓存），**绝不 mock Repository**——
那样只会测到「调了哪个方法」，测不到投影层的真实规则（last-write-wins 守卫、别名切换顺序、
缓存失效时机、条件更新的极性）。四条 fake 纪律：① 读侧返回值拷贝，生产代码改不动夹具里的行；
② `Insert` 只发主键，不自动补生产 SQL 没写的列；③ 维护**有序** callLog（`"<pkg>.<method>:<key>"`），
用例用 `wantOps` 断言完整序列；④ 播种静默（`seed*` 不写 callLog），否则序列断言被种子污染。

证明不了的：fake 只复刻 HTTP 与 SQL 的**语义**，不证明 SQL 文本、列名、索引命中与驱动返回的
matched/changed rows；`esclient` 侧只断到请求体结构与响应解析，OpenSearch 的真实行为
（分词效果、`_bulk`/`_aliases`/`_reindex` 往返、集群健康）由 `scripts/es-init.ps1` 对真实集群验证。

### 5. 覆盖边界

- 用例不连接 MySQL / Redis / OpenSearch（Elasticsearch 兼容层）/ Kafka / etcd / 对象存储；
  外部依赖一律走已有接口（`esclient.Client`、`consumer.Store`、`consumer.RebuildStore`、
  `consumer.QueueFactory`）注入 fake，
  fake 按用例返回真实错误。
- 真实分词与召回质量**未被验证**：`esclient/mapping_test.go` 只断 `IndexBody` 生成的 JSON 结构与
  tokenizer/filter 挂位，不启动引擎；词典命中效果要跑 `scripts/es-init.ps1`（含 `_analyze` 实测）。
- 迁移 SQL 与真实库的列级对账**未在目标实例复验**：本 README「已知缺口」2 明写未连真实 OpenSearch/MySQL，
  `model/` 也没有 migration↔DDL 的逐列门禁；上线前需按该条命令在隔离实例执行 `scripts/migrate.ps1`。
- `internal/server/`、`rpc/*.pb.go`、入口 `searchindexer.v1.go` 是 goctl/protoc 生成壳，不在单测范围内。
- 消费只有**一条推送路径**（kq 回调 → `Handler.Consume` → `ProcessMessage`）。原来的拉取循环
  `Consumer.Run` / `MessageSource` 已删除（全仓无实现，且与 kq 的推送模型不符），
  因此不存在「消费循环没有用例」的空白；取而代之的是 `queue_test.go` 对适配器契约的用例：
  **回调映射、位点不提交/推进、半启动回滚**都在 fake `QueueFactory` 上验证过。
  仍然证明不了的是真实 broker 行为（重平衡、分区分配、`kq` 内部的提交时机与协程生命周期）。
  本仓库从未与任何 Kafka/redpanda 实例建立过连接，见「已知缺口」1。

### 6. 验证命令

消费者适配器受构建标签开关影响，所以**两侧都要跑**：默认构建（不链接 kq）与
`-tags searchindexer_kafka`（链接 kq）。两侧命令均已实测 rc=0（2026-10-04）。

```bash
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
# 默认构建
go build ./services/search-indexer/...
go vet ./services/search-indexer/...
gofmt -l services/search-indexer            # 必须无输出
go test -p 1 -count=1 ./services/search-indexer/...
# 链接 Kafka 运行时的构建（只证明可编译/可静态检查/该包用例通过，不证明能与 broker 通信）
go build -tags searchindexer_kafka ./services/search-indexer/...
go vet -tags searchindexer_kafka ./services/search-indexer/...
go test -p 1 -count=1 -tags searchindexer_kafka ./services/search-indexer/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发编译/运行多个测试包会 OOM（`errno=1455`），
本仓库的测试门禁一律串行跑包（见 docs/commands.md）。

### 索引结构导出与本地集群验证

```bash
# 取进程实际会建的索引结构（settings + mappings + 分词），可 -pretty 便于 diff
go run ./services/search-indexer/cmd/esmapping -schema-version v1 -analyzer cjk -stopwords-path /usr/share/opensearch/config/analysis/go_video_stopwords.txt
```

```powershell
# 起集群后一次性验证：健康 → 插件实测 → 建开发索引 → _meta → _analyze → 样例文档 → 冒烟查询
docker compose -f deploy/docker-compose/docker-compose.yml up -d opensearch
.\scripts\es-init.ps1 -Endpoint http://127.0.0.1:9200 -Index go_video_content_dev_v1 -Analyzer cjk
```

脚本只写显式命名的物理索引：目标是别名、`_meta.analyzer` 与本次参数不一致或缺少 `_meta` 都会终止。
别名切换与回填永远走 `POST /admin/search/rebuild`、`POST /admin/search/alias/switch`。

## 已知缺口

1. **Kafka 适配器已接线，但只有编译级证据；且上游没有生产者，事件自动消费仍打不通**。
   2026-10-04 按 `services/inbox` / `services/notification` 的既有模式补齐了本服务的适配层，
   并由构建标签 `searchindexer_kafka` 开关：
   - `internal/consumer/queue.go`（无标签）：`MessageQueue` / `Settings` / `QueueFactory` /
     `Handler`（桥接 `Consumer.ProcessMessage`）/ `SettingsFrom` / `ValidateKafka` / `Supervisor`
     （启动失败回滚、逆序停止、幂等）；
   - `internal/consumer/kafkaruntime_disabled.go`（`!searchindexer_kafka`，默认）：`NewKqFactory()`
     返回 `ErrKafkaRuntimeNotBuilt`，不伪造成功；
   - `internal/consumer/kafkaruntime_kafka.go`（`searchindexer_kafka`）：把 `Settings` 映射成
     `kq.KqConf`（含 `ServiceConf{Name,Log,Mode}`，否则 kq 会重置进程级日志）后交给 `kq.NewQueue`；
   - `internal/svc/servicecontext.go` 的 `startConsumer()` 三分支：`Enabled=false` → 不起消费者；
     链接了运行时 → `NewSupervisor` + `Start`；默认构建开了 `Enabled` → 启动即报错而不是静默跳过。
     `RuntimeNotes()` 把「未链接 / 已链接」与「本轮索引写入实际来自哪里」打进启动日志。
   - 原拉取路径 `Consumer.Run` / `MessageSource` / `Kafka.PollBatchSize` 已删除（全仓无实现，
     且与 kq 的推送模型不符），消费只剩一条路径，避免文档与代码互相矛盾。

   **仍然缺两段，且都不是本服务能独自关闭的**：
   - **验证层级只到「可编译、可 `go vet`、该包单测通过」**（默认与 `-tags searchindexer_kafka`
     两侧 rc=0，见「测试覆盖」§6）。本仓库从未与任何 Kafka/redpanda 实例建过连接，
     重平衡、分区分配、kq 的实际提交时机均未经过实机验证；`deploy/docker-compose` 的
     redpanda 是 `PLAINTEXT://localhost:9092`，与 `etc/searchindexer.v1.yaml` 的 `Kafka.Brokers`
     一致，但这是人工核对而非实测。真实启用还要：起 broker → 在隔离库执行本服务迁移 →
     `-tags searchindexer_kafka` 构建 → 把 `Kafka.Enabled` 置 `true` → 观察启动日志的
     「事件消费者已启动」并按 `search_consumer_offset` / `search_dead_letter` 对账。
     本轮**未启动任何进程，也未连接任何中间件**。
   - **两条订阅链的上游状态不同**：`content.published.v1` 的生产侧已在 2026-10-05 由 `video` 接上
     （同事务写 `video_outbox` + `services/video/internal/publisher`，`-tags video_kafka`），
     所以这条 topic 不再是「永不来」；`engagement.action.v1` 在全仓仍没有发布点，
     索引文档的 `heat` 补丁路径因此还没有任何输入，热度只能等 `engagement` 侧接线
     （缺口登记见 docs/roadmap.md 的「MQ 接线」第 5 条）。
     两侧的口径都一样：生产端与消费端都在代码里，但都从未与真实 broker 联调（见上一条）。
     全仓事件 outbox 表现在是 8 张，其中 6 张接了 MQ 发布器
     （`live_ingest_outbox`、`live_media_outbox`、`playback_outbox`、`recall_outbox`、`upload_outbox`、
     `video_outbox`），`member_outbox` 与 `search_outbox` 的写入方仍没有 MQ 读取方、`MarkPublished` 永不被调用；
     `feed_outbox` 不计入，它是「动态主表」而不是事件 outbox。
     索引写入另一条与 MQ 无关的可用路径是 RPC：`UpsertContentDoc` / `DeleteContentDoc`（上游或回填任务调用），
     以及 `services/cron` 经 `SubmitRebuildTask` / `GetRebuildTask` 触发的重建任务
     （cron 与本服务是两个二进制，它只能走 RPC，不可能像在进程内那样调本包的 `ProcessMessage`）；
     退避重试与死信状态机不依赖 MQ，跑在 `search_consumer_offset` 上，重启后仍能继续。
   - 顺带记录：本服务内曾有三处「依赖缺失、无法 import」的误述（`internal/consumer/consumer.go` 头部注释、
     `internal/config/config.go` 的 `KafkaConf` 注释、`etc/searchindexer.v1.yaml` 的 `Kafka` 段），
     已于同日改成本条口径。`github.com/zeromicro/go-queue v1.2.2` 是 `go.mod:9` 的**直接**依赖，
     接线不需要任何依赖变更，也不要照旧注释去 `go get` / `go mod tidy`。
2. **未连过真实 OpenSearch / MySQL**。`deploy/docker-compose` 已提供 `opensearch` 服务，但本轮环境
   没有可用的 docker daemon，也没有真实 MySQL 实例，因此验证只覆盖编译、`go vet`、`gofmt`、
   纯逻辑单测，以及用本地 HTTP 桩跑通 `scripts/es-init.ps1` 的完整请求序列
   （建索引 → `_meta` 校验 → `_analyze` → `_bulk` → `_count` → `_search`）；
   真实 OpenSearch 的分词效果、`_bulk`/`_aliases`/`_reindex` 集群往返仍未验证。
   本地一次跑通：
   - `docker compose -f deploy/docker-compose/docker-compose.yml up -d opensearch`（需用户授权拉取镜像）；
     匿名集群要把 `OpenSearch.AllowAnonymousWrites` 显式置 `true`，否则所有写返回 `ErrWriteGuarded`。
   - `.\scripts\es-init.ps1 -Index go_video_content_dev_v1 -Analyzer cjk`：脚本自己会实测
     `_cat/plugins`，缺 ik/smartcn 时直接拒绝而不是降级。
   - MySQL 侧：在隔离实例执行 `scripts/migrate.ps1 -Service search-indexer` 后，
     用 RPC `SubmitRebuildTask` → `GetRebuildTask` 验证 `cursor_value` 断点与 `uniq_request_id` 幂等。
   上述动作都需要用户授权与真实实例，本轮未执行。
3. **只维护一个别名**：`OpenSearch.IndexPrefix`（默认 `go_video_content`）既是写入别名也是查询别名，
   重建流程只原子切换它。若查询侧要用独立的只读别名，必须由同一次切换一起维护，
   否则切别名后只读别名会留在正在退役的物理索引上。
4. **`SubmitRebuildTask` 的 `scope=partition` 按 `typeid` 区间过滤**（`ScopeFilter`），
   而 `cursor_value` 按 `content_id` 切片：两个维度叠加时，跨分区的大区间会产生较多空切片。
   若后续要按 `content_id` 精确分区，需要扩表列并在契约里说明，不能沿用现有 `scope_value` 语义。
5. **`HeatScore` 由上游打分、本服务不计算**：`heat_score` 只透传 SPM/推荐推送的归一化分值；
   上游未推送时索引里恒为 0，排序侧（search-query）需按 `heat.*` 计数自行加权。
6. **管理后台入口已接线，写入口已挂判定**：`gateway/admin` 的 `/admin/search/*` 直接调用本服务
   （`POST /rebuild`、`GET /rebuild/:task_id`、`GET /rebuilds`、`POST /alias/switch`、`GET /health`）。
   其中两条写路由已在 `routePermissions` 登记并挂 `AdminPermission`
   （`search:index`#rebuild、`search:alias`#switch），权限点由
   `deploy/migrations/operation/000004_seed_op_permission.sql` 落库、角色由 `000006` 并入 `domain_search`；
   三条读口（任务详情、任务列表、健康）仍走免鉴权组，与本仓「读面免鉴权」口径一致。
   死信重放（`ReplayDeadLetter`）目前没有 HTTP 入口。
7. **未做 `go test -race`**：Windows 下 race 需要 cgo 工具链，本轮未运行；worker 与 gRPC 并发路径
   的竞态验证需在 Linux CI 上补一次。
