# upload

大文件分片上传会话服务，负责上传会话生命周期与 OSS 分片预签名 URL 签发。

- **拥有数据**：`upload_session`（上传会话：upload_id/mid/filename/size/typeid/bucket/object_key/state/chunk_size/total_chunks/md5/asset_id/ctime/mtime）、`upload_chunk`（分片清单：upload_id/chunk_no/size/etag/state/ctime/mtime）、`upload_outbox`（领域事件 Outbox：`media.task.v1` 的待投递事实）。
- **提供能力**：初始化上传会话、为分片签发短期预签名 PUT URL、客户端直传 OSS 后提交分片清单触发完成、取消上传、查询上传状态、把「上传已完成」可靠投递到 MQ。
- **依赖**：MySQL（分片元数据 + Outbox）、Redis（会话状态短缓存）、对象存储（OSS/MinIO，本期只有占位签名实现，见缺口 3）、Kafka（仅 `-tags upload_kafka` 链接，见「Outbox 发布」一节）、`account`（鉴权由 gateway 完成，本服务只接收 mid）、`asset`/`transcode`/`content-fingerprint`（设计上消费 `media.task.v1`，本服务不主动调用；**这三个服务目前都没有 consumer 实现**，见缺口 1）。
- **约束**：
  - 不接收文件字节进 MySQL，只存分片元数据；
  - 客户端直传 OSS，服务端只签发短期预签名 URL，不向客户端下发 OSS 长期密钥（本期 URL 是 mock 串，不含真实签名）；
  - 完成上传只推进到 `COMPLETED`，不直接写 `PUBLISHED`，由 `asset`/`transcode` 接管后续状态机。接管所需的
    `media.task.v1` **本服务已发布**（与 `COMPLETED` 同事务落 `upload_outbox`，由 `internal/publisher` 投递），
    但下游无消费方，所以端到端仍不通，见缺口 1；
  - 不实现商业化（会员、订单、支付、广告）。

## 分片上传流程

```text
1. 客户端 → InitUpload(mid, filename, size, typeid, md5, chunk_size, total_chunks)
   服务端生成 upload_id，分配 OSS bucket/object_key 占位，写 upload_session
   和 upload_chunk 清单（state=PENDING），返回 upload_id + 分片参数。
   （秒传：契约里有 instant 字段，但本期恒为 false，md5 从不参与查询，见缺口 2。）

2. 客户端 → GetUploadUrl(upload_id, chunk_no) 逐分片获取短期预签名 PUT URL。
   客户端按 URL 直传 OSS 分片，OSS 返回 ETag。
   首次取 URL 会把会话从 INITIALIZED 幂等推进到 UPLOADING。

3. 客户端上传完全部分片后 → CompleteUpload(upload_id, parts[{chunk_no,etag}], md5)
   服务端校验分片清单完整性（数量、覆盖、ETag 非空），逐片记录 ETag，
   然后在**同一个事务**里推进 upload_session.state = COMPLETED、回填 asset_id 占位
   （`asset-placeholder:<upload_id>`，后续 asset 服务接管后回填真实 ID）、可选写 md5，
   并写入一行 `upload_outbox`（事件 `media.task.v1`，分区键 = upload_id）。
   事务提交后才刷会话缓存；提交失败一条写都不生效（分片标记除外，见缺口 8）。
   OSS 侧的 CompleteMultipartUpload 仍是占位（缺口 3）。

4. 异常分支：AbortUpload(upload_id) 取消上传并删除分片清单
   （OSS 侧分片删除是占位，见缺口 3）；
   GetUploadStatus(upload_id) 查询会话状态与已完成分片列表。
```

## 状态机

```text
INITIALIZED → UPLOADING → COMPLETED → (投递 media.task.v1；下游接管仍断链，见缺口 1)
     └→ ABORTED
     └→ FAILED   ← 代码里存在，但当前不可达（见缺口 4）
```

`COMPLETED`/`ABORTED` 是终态；`FAILED` 不会被拒绝对外写操作，语义上当「可重试」处理。

## 已知缺口

按影响面分组。每条给出门槛位置与钉住该行为的测试；这些测试断言的是**当前真实行为**，修缺口时对应测试要一起改，不算回归。

### A. 契约与事件（发布阻塞）

1. `media.task.v1` **本服务侧已发布，但全仓库没有消费方**：`CompleteUpload` 在 COMPLETED 的同一事务里写
   `upload_outbox`（`internal/repository/repository.go:300`），`internal/publisher` 的引擎按 `Kafka.Enabled` 启动投递，
   而 `asset`/`transcode`/`content-fingerprint` 三个服务都**没有** `internal/consumer` 目录
   （`transcode/README.md` 缺口、`content-fingerprint/README.md` 缺口 9 各自登记了这件事）。
   结果是事件出本服务即无人认领，媒资探测/转码/指纹链路的端到端仍不通（`docs/roadmap.md` A 组）。
   另一层是**开关**：默认构建不链接 Kafka 发送端（`-tags upload_kafka` 才有 `NewSender`），示例配置
   `Kafka.Enabled: false`，`upload/000003` 迁移未执行，三者齐备才会真的有一条消息离开进程；
   `Enabled=false` 时事件行只积不投，`RuntimeNotes` 只在启动 Info 里说明一次，没有告警。钉：
   `TestRequiredTopicIsCrossServiceContract`、`TestExampleYamlIsOneFlipFromPublishing`、
   `TestDefaultBuildRefusesToCreateSender`、`TestDefaultRuntimeNotes`、`TestTaggedRuntimeNotes`。
2. 秒传未实现：`internal/logic/inituploadlogic.go:62` 恒 `Instant: false`（`repository.go:148` 注明原因），库里为秒传建的 `idx_md5`（`deploy/migrations/upload/000001_create_upload_session.sql`）从不被查，InitUpload 连一次 `FindOne` 都不做。钉：`TestInitUploadNeverDeduplicatesByMd5`。

### B. 真实对象存储

3. 三个 OSS 动作全是占位：预签名 URL 是拼出来的 mock 串（`repository.go:223`，`signature=MOCK`），`completeMultipartUpload`（`:366`）与 `abortMultipartUpload`（`:377`）恒返回 `nil`。因此「分片真的传到了 OSS」「OSS 真的合并了」「取消真的删掉了 OSS 侧分片」三件事目前都不成立，`AbortUpload` 只删自己的元数据。事件 payload 因此只带 `bucket`/`object_key` 引用，不带任何凭据（钉：`TestCompleteUploadEventPayloadCarriesOnlyMediaFacts`）。钉：`TestCompleteUploadAcceptsFabricatedEtagsWithoutAnyUpload`、`TestGetUploadUrlPromotesInitializedSessionAndSigns`。
4. `FAILED` 分支不可达：唯一写 `SessionStateFailed` 的点是 `repository.go:269`，而它唯一的触发条件是 `:268` 那个恒 `nil` 的占位调用。同时 `CompleteUpload`/`GetUploadUrl`/`AbortUpload` 都不拒绝 `FAILED`，所以它是「够不到的状态」而不是「会发生的状态」。注意它也在事务**之外**：真接上 OSS 后，合并失败会把会话刷成 `FAILED` 而不留事件行，这条路径目前够不到，接入时要一并判。钉：`TestCompleteUploadRejectsTerminalAndUnknownSessions`、`TestAbortUploadAllowsFailedSession`。
5. `Endpoint` 语义在仓库内自相矛盾：`internal/config/config.go:64` 的注释示例写 `https://oss-cn-hangzhou.aliyuncs.com`，而 `repository.go:223` 自己拼 `https://<bucket>.<endpoint>/`，照注释填会得出双 `https://` 的死 URL。`etc/upload.v1.yaml:20` 用的是裸 host，是注释该改。钉：`TestGetUploadUrlMalformsWhenEndpointCarriesScheme`。
6. `GetUrlReply.headers` 恒为空 map、入参 `chunk_size` 在 `repository.go:225` 被显式丢弃（真实签名要用它做 Content-Length 约束），客户端目前拿不到任何「按签名要求附带」的 header。钉：`TestGetUploadUrlPromotesInitializedSessionAndSigns`。

### C. 事务、原子性与幂等

7. `InitUpload` 的两次写不在同一事务（`repository.go:163` 插会话、`:177` 批量插清单）：清单插入失败时会话已落库，留下一个没有任何分片行的孤儿会话，且短缓存没写。孤儿会话在 `CompleteUpload` 会被清单行数门槛拦住（`TestCompleteUploadRejectsSessionWithoutManifestRows`），但 `GetUploadUrl` 照样给它签 URL。钉：`TestInitUploadChunkBatchFailureLeavesOrphanSession`。
8. `CompleteUpload` 边校验边写、且分片标记整体在事务之外：`:258` 的循环里校验一片就 `MarkUploaded` 一片，后面的分片失败时前面的 ETag 已落库；`:288` 起的 `TransactCtx` 只覆盖「md5 + asset_id + COMPLETED + 事件行」四写。因此清单不齐的失败会留下半截清单，而会话侧的四写要么全有要么全无。钉：`TestCompleteUploadRejectsPartNotInManifest`、`TestCompleteUploadMarkFailureLeavesRetryablePartialManifest`、`TestCompleteUploadFourWritesShareOneTransaction`。
9. `upload_outbox` 只有「投出去」和「判死」两条路，没有运维出口：`state=2` 的行没有任何解冻/重放接口（`common/outbox` 到上限即 `MarkFailed`，此后再无路径取到它），已发布行（`state=1`）没有归档或清理任务，本服务也没有 `cron` 侧登记；表上没有租约列，多副本同时跑 `RunOnce` 会把同一行投两次。当前唯一取证手段是直接查表（按 `aggregate_id = upload_id`）。钉：`TestRunOnceJudgesDefectiveRowWithoutSending`（判死写入 `unpublishable: ` 前缀）、`TestRunOnceRetriesOnSendError`（退避到上限的形态）。
10. `AbortUpload` 先删清单再改状态（`:340`→`:343`）：中间失败会得到「分片清单已清空、会话仍是 UPLOADING」的活会话，之后 `GetUploadUrl` 照样签 URL、`CompleteUpload` 因为清单空了只能报 `ErrChunkMismatch`，用户既传不完也取消不干净。钉：`TestAbortUploadStateFailureLeavesLiveSessionWithoutManifest`。
11. 重复取消不可区分：状态已是 `ABORTED` 时再 `AbortUpload` 会静默再跑一遍清理并返回 `EmptyReply`，调用方无法分辨首次成功与重放（全部写接口缺幂等键/版本，见 AGENTS.md §5）。钉：`TestAbortUploadRepeatedCancelReRunsCleanup`。

### D. 鉴权与归属

12. 五条 RPC 全部只按 `upload_id` 寻址，写路径不校验会话是否属于调用方：`GetUploadStatus` 能读别人的元数据，`AbortUpload` 能把别人的会话翻成 `ABORTED`，`CompleteUpload` 能给别人的会话写下 `asset-placeholder`、推进到 `COMPLETED` **并替它向下游派发一条转码任务**。唯一缓解是 `upload_id` 含 12 字节随机（`repository.go:389`），不构成授权。修法需要跨服务契约变更（gateway 注入可信 mid 后由本服务比对），本轮不动代码，只出缺口。钉：`TestUploadWritePathsAuthorizeByUploadIdOnly`。

### E. 输入校验与容量语义

13. `chunk_no` 无上界、不做存在性校验：`GetUploadUrl` 走的是会话而非分片行，`UploadChunkModel.FindOne` 在 logic 路径从未被调用，所以清单外的 `chunk_no=9999` 照样签名。钉：`TestGetUploadUrlSignsChunkNoBeyondManifest`。
14. 清单里每片大小一律记 `chunk_size`（`repository.go:171`），不区分最后一片，且 `size`/`chunk_size`/`total_chunks` 三者关系不校验：`uploaded_size` 可以超过声明的 `size`；`chunk_size` 也没有文档意义上的下限。同一组值会原样进 `media.task.v1` 的 payload（`size`/`chunk_size`/`total_chunks`），所以下游拿到的也是这套不自洽的数。钉：`TestGetUploadStatusUploadedSizeCanExceedDeclaredSize`、`TestInitUploadAcceptsChunkSizeBelowDocumentedMinimum`、`TestCompleteUploadEventPayloadCarriesOnlyMediaFacts`。
15. `md5`/`filename` 形态不校验：40 位十六进制（SHA-1）、空 md5、300 字符文件名都能过，文件名还直接进 `object_key`；库里 `md5` 是 `CHAR(32)`、`filename` 是 `VARCHAR(255)`，超长值在真库上会被截断或拒。注意 `CompleteUpload` 的 `SetMd5Tx` 也不校验，于是一条 40 位 md5 会**同时**进库（可能截断）和进事件 payload（原样 40 位），两侧读数不一致。钉：`TestInitUploadDoesNotValidateMd5OrFilenameShape`。
16. 缓存写失败一律被 `_ =` 吞掉（`:142`/`:180`/`:204`/`:208`/`:320`/`:360`）且不打日志：判定不看缓存，所以只丢短缓存，但 Redis 故障在本服务侧完全静默。钉：`TestGetUploadStatusSurvivesCacheBackfillFailure`。

### F. 事件覆盖面

17. 只有「完成」产事件：`AbortUpload`、`FAILED` 与任何元数据变更都不写 `upload_outbox`，
   已投出的 `media.task.v1` 也没有撤回/补偿事件。因此下游一旦接上，会看到「任务派发」而永远看不到
   「这次上传作废了」，取消与失败的传播只能靠回查本服务 RPC。
   取消侧钉在 `TestAbortUploadDeletesManifestThenMarksAborted`（事件表 0 行、`TransactCtx` 0 次）。
   同一会话产两行事件目前只能由**并发完成**造成（两次 `FindOne` 都读到非终态，各自组一个 `event_id`，
   `uniq_event_id` 只挡同 ID 重复，不挡同会话双事件）；该形态离线单测复现不了（替身无隔离级别、无行锁），
   属未覆盖风险，下游必须按 `aggregate_id` 自行判重。

## 数据与迁移

| 表 | 迁移 | 关键约束 |
|---|---|---|
| `upload_session` | `deploy/migrations/upload/000001_create_upload_session.sql` | `PRIMARY KEY(upload_id)`、`KEY idx_mid_ctime`、`KEY idx_md5`、`md5 CHAR(32)` |
| `upload_chunk` | `deploy/migrations/upload/000002_create_upload_chunk.sql` | `PRIMARY KEY(id)`、`UNIQUE KEY uniq_upload_chunk(upload_id, chunk_no)` |
| `upload_outbox` | `deploy/migrations/upload/000003_create_upload_outbox.sql` | `UNIQUE KEY uniq_event_id`（列级 `utf8mb4_bin`、无默认值）、`KEY idx_state_next_retry`、`KEY idx_event_type_ctime` |

迁移登记见 `deploy/migrations/README.md`（`go_video_upload`，3 条：`000001`~`000002` 为 `applied`，
`000003` 为 `pending`，即**从未在任何实例执行过**）。这直接影响可用性：`000003` 未执行时
`CompleteUpload` 的四写事务会在 `outbox.Insert` 上失败，等于**上传完成整条路径不可用**，
因此**部署顺序必须是「先跑迁移，再上线本次代码」**，回滚同理（先回代码停发布循环，再 `DROP TABLE`）。
本服务没有 model↔DDL 的离线门禁（`migration_parity_test.go` 只在商业化五服务有）。

## Outbox 发布（`media.task.v1`）

`internal/publisher` 是 `common/outbox` 引擎的本服务适配器，形状与 playback/live-media/recommend-recall 一致：

- **产出**：唯一 topic `media.task.v1`（`eventenvelope.Topic(model.EventMediaTask, model.EventSchemaVersion)` 现场派生，
  配置里的 `Kafka.PublishTopics` 必须等于它，多写一个键名都让 `ValidatePublishKafka` 拒启动）。
  分区键 = `aggregate_id` = `upload_id`，所以同一会话的事件必然同分区、按 `id` 升序。
- **写入点唯一**：只有 `CompleteUpload` 在 COMPLETED 的同一事务内 `outbox.Insert`（`repository.go:300`）。
  `Repository.OutboxModel()`（`:135`）是给发布器取句柄的导出口，**它带 `Insert`，所以「只有完成路径写事件」
  目前是靠约定而不是靠类型挡住**；离线侧的兜底是 `fakes_test.go` 的 `fakeOutbox`：嵌入 nil 接口，
  logic 路径上出现 `ListPending`/`Mark*` 即 panic，`CompleteUpload` 之外的用例则靠固定序列断言里没有
  `outbox.Insert`（例如取消路径）。
- **不可发布判定**：`OutboxStore.toRow` 调 `outbox.CheckRow`，列与 payload 不同源（`event_id`/`aggregate_id` 对不上、
  topic 不归属本服务、分区键为空、payload 非法 JSON）即写 `last_error = "unpublishable: ..."` 并直接判 `state=2`，
  **不占用发送端**，也就不消耗重试次数。
- **构建标签**：Kafka 发送端在 `-tags upload_kafka` 之后才存在（`kafkaruntime_kafka.go`，用 `kq.NewPusher` +
  `WithSyncPush`）；默认构建的 `NewSender` 返回 `ErrKafkaRuntimeNotBuilt`，于是「开了 `Kafka.Enabled` 却用默认构建启动」
  会在 `logx.Must` 处启动即失败，而不是安静地不投。`KafkaConf` 刻意不含 `Group`（本服务只产不消），
  也不含 SASL/TLS 键位（go-queue v1.2.2 不支持注入 dialer）。
- **启动/停止**：`internal/svc` 在 `NewServiceContext` 里按 `Kafka.Enabled` 决定是否 `startPublisher()`，
  注册 `proc.AddWrapUpListener` 做优雅收尾；`Enabled=false` 时只打一条 `RuntimeNotes` Info。
  投递结果**不写日志**（排障靠查表：`SELECT * FROM upload_outbox WHERE aggregate_id=?`）。
- **从未联调 broker**：仓库内没有任何一次与真实 Kafka 的端到端跑通记录，本服务同样没有。

## 运行

```bash
go run upload.v1.go -f etc/upload.v1.yaml                 # 默认构建：不链接 Kafka 发送端
go run -tags upload_kafka upload.v1.go -f etc/upload.v1.yaml   # 链接发送端；仍需 Kafka.Enabled=true 且 000003 已执行
```

健康检查与服务发现由 `gateway` 通过 etcd 完成，Key 为 `upload.v1.rpc`。

## 测试覆盖

离线单测（纯 Go 内存替身），不连 MySQL/Redis/对象存储/etcd/MQ，也不依赖网络。
静态数由 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 导出；
动态数是 `go test -v` 里 `--- PASS`（顶层）与 `^    --- PASS`（子用例）的实测条数，
两者不相等是因为子用例由 `for range` 表驱动展开。口径统一写成 `静态顶层/静态子行 → 动态顶层+子`。

| 构建 | 范围 | 顶层静态 | 子静态行 | 动态 PASS | FAIL/SKIP |
|---|---|---|---|---|---|
| default（未链接 Kafka） | `./services/upload/...` | 81（logic 59 + publisher 21 + config 1） | 15（logic 12 + publisher 2 + config 1） | 81 顶层 + 50 子 = **131** | 0 / 0 |
| `-tags upload_kafka` | 只有 `internal/publisher` 变（`-3` 个 default 专属、`+4` 个 tagged 专属） | 22 | 3 | 22 顶层 + 25 子 = **47** | 0 / 0 |

本服务没有处于 `t.Skip` 状态的用例，也没有任何「永真断言」占位。

### 1. `internal/logic`（7 个文件：6 个用例文件 + `fakes_test.go` 替身层）— 静态 `59/12`，动态 `59+28`

| 文件 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|
| `initupload_test.go` | 7 | 4 | 门槛顺序 `mid → filename → size → total_chunks`，被拒时零 SQL 零缓存；一次初始化写「1 行会话 + n 行清单」且顺序固定；清单批插失败留下**没有清单的活会话**（缺口 7）；秒传完全不生效：同 `md5` 重放得到两个互不相干的会话、`instant` 恒 false、`md5`/`filename` 形态不校验（缺口 2、15） |
| `getuploadurl_test.go` | 13 | 6 | 终态门槛两层把关（缓存命中直接判、不回源；冷/坏时由库里状态判并回填）；`INITIALIZED→UPLOADING` 只在首次签发时推进、第二次不得再写；TTL 由配置决定、`<=0` 落 900 秒；URL 绝不带 OSS 长期密钥；推进失败宁可不签；钉住「清单外的 `chunk_no=9999` 照样签」（缺口 13）与「`Endpoint` 带 scheme 时拼出死 URL」（缺口 5） |
| `completeupload_test.go` | 19 | 2 | 判定链**一次缓存都不读**、全以库为准；清单与 `total_chunks` 的条数/覆盖/ETag 三重门槛，门槛失败不留半截写；成功序「清单全标 → 一次 `TransactCtx` 内 `SetMd5Tx`→`UpdateAssetIdTx`→`UpdateStateTx`→`outbox.Insert` → 提交后才刷缓存」，并单独钉住**四写同事务**（`transactions==1`、三条 session 写各自拿到 tx 会话、事件行拿到 tx）；事件行逐列对信封（`event_id`/`aggregate_id`/`event_type`/`schema_version` 同源、派生 topic 恰为 `media.task.v1`、`Validate()` 过）、payload 只带媒资事实且**不含**任何 `signature=MOCK`/`X-Amz`/`AccessKey`/调用方 IP；任一写失败 ⇒ 事务标记回滚且不刷缓存；组不出信封时会话/事件/缓存三者一律零写；钉住两个真实缺陷：从未上传任何分片也能凭客户端自报 ETag 完成并产出 1 行事件、中途写失败留下已标分片而会话仍 `UPLOADING`（缺口 8） |
| `abortupload_test.go` | 9 | 0 | 只有 `COMPLETED` 被拒（清单留作审计证据），`ABORTED`/`FAILED` 允许再取消；清理序固定（OSS 占位 → 删清单 → 置 `ABORTED` → 刷缓存）；**取消不产事件、不开事务**（事件表 0 行 + `TransactCtx` 0 次，缺口 17）；删清单失败时状态与缓存都不动、库里保持完整可重试；置状态失败留下「清单已空、会话还活着」的形态（缺口 10）；重复取消静默重跑清理、与首次成功不可区分（缺口 11） |
| `getuploadstatus_test.go` | 10 | 0 | 判定只看库但每次成功回填状态缓存；进度口径是 `state >= UPLOADED`（`VERIFIED` 算完成、待上传不算）；分片列表按 `chunk_no` 升序原样透出；末片不反映余数所以 `uploaded_size` 可超过声明的 `size`（缺口 14）；缓存回填失败被 `_ =` 吞掉而请求仍成功（缺口 16） |
| `authz_test.go` | 1 | 0 | 五条 RPC 的授权凭证只有 `upload_id`：读得走他人会话的文件名/`object_key`/`md5`，也能取消或「完成」他人会话；`upload_id` 的随机性属「不可猜测」而不是「已鉴权」（缺口 12） |
| `fakes_test.go` | 0 | 0 | 替身层与断言工具，`top=0 sub=0` 是正常形态，不是遗漏；装配口径见第 5 组 |

### 2. `internal/publisher`（5 个用例文件，按构建标签分裂）— 静态 `21/2`，动态 `21+21`

| 文件 | 构建 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|---|
| `outbox_store_test.go` | 两者都有 | 9 | 1（`TestToRowFlagsDefects` 展开 9 条） | `RequiredTopic()` 的字面量就是跨服务契约（改成别的值本用例即红）；`NewOutboxStore(nil)` 拒；列 → `Row` 映射逐列（分区键取 `aggregate_id`、topic 由两列现场拼）；9 种「不可发布」形态逐条点名字（topic 不归属、版本对不上、空 `event_type`/`event_id`/聚合根、payload 非 JSON、信封 `producer` 与列不同源、`event_id`/`aggregate_id` 与列不一致）；`ListPending` 跳过 nil 行、原样透传 `now`/`limit`、错误串里带 `upload_outbox`；三个 `Mark*` 参数不被吞；用**真适配器** + 假 model 走 `RunOnce` 端到端（成功置已发布并回填 `published_at`、发送失败进退避、判死行一次都不碰发送端且 `last_error` 带 `unpublishable: `） |
| `params_test.go` | 两者都有 | 6 | 1（`TestValidatePublishKafkaRejectsIncompleteConfig` 展开 12 条） | `SenderSettingsFrom` 去重去空白且不改配置切片（拷贝）；`OptionsFrom` 六个旋钮逐键对位、`MaxRetries → MaxAttempts`；12 种残缺配置逐个点名且错误串都带 `upload` 前缀；多条坏键一次报全（聚合式校验，不是首错即返）；`NewPublisher` 四条出口（校验失败 / model 为 nil / sender 为 nil / 成功），成功实例还要透传 `OptionsFrom` 且**不自启循环**（启动时机归 svc，否则测试里会留下跑着的协程）；配置层放行但引擎层拒绝的组合被拦住（`MaxRetries=2^31` ⇒ 错误同时提 `Options.MaxAttempts` 与 `Kafka.MaxRetries`） |
| `example_yaml_test.go` | 两者都有 | 3 | 0 | 用 `conf.Load` 真读 `etc/upload.v1.yaml`：`Enabled` 必须是 `false`（改成 `true` 本用例即红，防「示例配置默认往 broker 打」）、其余参数必须齐备、topic 恰等于 `RequiredTopic()`、brokers 恰为 `127.0.0.1:9092`、`OptionsFrom` 结果等于那六个字面量；用示例配置真装配一个发布器跑 `RunOnce`（投递 1 条、分区键 `up-41`、超时 ≤5s、`published_at` 落在真实时钟窗口内）；同一文件的 OSS 段仍可加载且 `AccessKey`/`SecretKey` **必须为空**（示例配置不得带凭据，真实凭据只从 Secret/Vault 注入） |
| `kafkaruntime_disabled_test.go` | 仅 default（`!upload_kafka`） | 3 | 0 | 默认构建 `NewSender` 必失败且错误文案同时提 `upload_kafka`、`broker`、`Kafka.Enabled`；`RuntimeNotes` 两条口径（未链接 + 后果：`upload_outbox` 只积不投、下游链路无从开始、且该 topic 还没有消费方）；把示例配置翻成 `Enabled=true` 后用 svc 的入口形状跑，得到 `ErrKafkaRuntimeNotBuilt` 且 sender 为 nil ⇒ 「开开关就启动失败」而不是静默 |
| `kafkaruntime_kafka_test.go` | 仅 `-tags upload_kafka` | 4 | 1（4 条守卫） | 建 pusher 不触网（构造即返回、`Close` 幂等且清空 topics、重复 topic 去重）；四条入口守卫逐条（无 broker、broker 含空串、无 topic、topic 含空串）；`Send` 在触网前拒两类（未注册的 topic 用 `content.published.v1` 探、空分区键）；`RuntimeNotes` 口径为「已链接」而非「已在投递」，并明说从未联调 broker 且无消费方 |

### 3. 其他层

- `internal/config`：1 个文件 `1/1`。`config_load_test.go` 逐个加载 `etc/*.yaml`（子用例按文件名展开），
  反射递归断言非匿名结构体里的 `*DataSource` 与 `redis.RedisConf.Host` 非空；缓存字段必须叫 `CacheRedis`，
  与 `zrpc.RpcServerConf` 内嵌的同名字段撞车会让服务启动即报 `conflict key redis`。`Kafka` 段的键位形状由
  `internal/publisher` 的 `example_yaml_test.go` 用同一份 yaml 把守，不在这个包里重复。
- `internal/repository`（注入缝与真实 Repository 的宿主）**无离线单测**：只被 logic 用例经由它跑，不单独断言它。
  `mediatask.go` 的信封装配同样只被 logic 用例覆盖（事件行反解 + `Validate()`），没有独立用例。
- `model/`（`uploadmodel.go`、`uploadoutboxmodel.go`、`errors.go`）**无离线单测**：SQL 文本、占位符个数、
  列宽与索引只能由迁移与集成环境证明。
- `internal/svc` **无离线单测**；`startPublisher` 的失败路径由 `kafkaruntime_disabled_test.go` 用
  「复刻 svc 入口形状」的方式钉住，不是真调 `NewServiceContext`（那会连 Redis/MySQL）。
- 本服务没有 `internal/consumer`、`internal/policy` 层：`media.task.v1` 只有生产侧，
  消费方应在 asset/transcode/content-fingerprint，三处都还没落地（缺口 1）。

### 4. 构造器级覆盖：**logic 5/5，publisher 3/3**

探针取 `internal/logic` 全部 `New*Logic(`，共 5 个（`InitUpload`、`GetUploadUrl`、`CompleteUpload`、
`AbortUpload`、`GetUploadStatus`），逐个能在 `*_test.go` 里查到直接引用，`gaps:` 为空。
每个 RPC 方法都有直接驱动自身构造器的用例，不存在「只有间接断言」的方法。
`internal/publisher` 的构造器：`NewOutboxStore`、`NewPublisher`、`NewSender`（default 与 tagged 各自的实现）
都有直接引用，其中 `NewSender` 两种构建各钉一头（default 必失败 / tagged 必成功且不触网、坏参数必拒）。

### 5. 替身层与断言口径

- logic 侧的装配缝在 `internal/repository`：`Cacher` 接口 +
  `NewWithDeps(内存缓存, fakeConn, 内存 sessionMd, 内存 chunkMd, 内存 outboxMd, 假 OSS 配置)`
  组装的是**真实 Repository**，只换它的 5 个依赖与连接。因此「缓存快路径有没有跳过 DB、状态机门槛、
  分片清单校验、完成/取消的写入次序、完成四写是否同事务」整条判定链都落在被测路径上，而不是把 Repository 一起 mock 掉。
- 四条替身纪律（`fakes_test.go` 头部）：每次读返回值拷贝；副作用按顺序记进四个 model 替身共享的一条
  `callLog`，断言序列而不是次数；每步**先记录再判故障**，于是「打到了依赖才失败」与
  「根本没打依赖」不会在断言里长得一样；布数据走静默写入路径（`warm`/`put`/`seedOne`），
  所以序列断言从第 0 条数起。
- 复刻的 model SQL 语义：`session.UpdateState`（及其 `UpdateStateTx`）在 `RowsAffected==0` 时返回 `ErrUploadNotFound`，
  而 `UpdateAssetId`/`SetMd5` 不检查影响行（真 SQL 更新 0 行也不报错）；三个写各有 `*Tx` 变体，
  替身对两者记**不同**的 op 名，所以「事务里用的是 Tx 版本」是可断言的；`chunk.MarkUploaded` 行不存在返回
  `ErrChunkNotFound`；`chunk.InsertBatch` 对空清单直接返回、不发 SQL；`chunk.ListByUpload` 带
  `ORDER BY chunk_no ASC`；`outbox.Insert` 把「是否拿到事务会话」留在 `gotTx`；`FindOne` 的语义是
  「不存在返回 `(nil, nil)`」而不是 `sql.ErrNoRows`。
- publisher 侧不复刻 SQL：它用**真适配器**（`OutboxStore`）+ 假 `UploadOutboxModel` + 假 `Sender`，
  被测的是映射、判定与引擎编排；`fakeSender` 记录 topic/分区键/payload/超时，因此「投了几条、投到哪、
  用什么键」都是断言出来的。
- **它证明不了**：SQL 文本与 `model/*.go` 的占位符个数、`upload_session`/`upload_chunk`/`upload_outbox` 的
  列宽与索引、`uniq_upload_chunk(upload_id, chunk_no)` 与 `uniq_event_id(event_id)` 的重复插入冲突、
  `md5 CHAR(32)` 的截断或拒写、`chunk.id`/`outbox.id` 的真实 `AUTO_INCREMENT`（替身按代码意图自增分配）。
  `fakeConn` 也不模拟 MySQL 回滚：它只能证明「几次写落在同一次 `TransactCtx` 的会话上、失败时事务被标记回滚」，
  不能证明「已写入的行会被数据库撤回」。

### 6. 覆盖边界（如实声明）

- 用例不连接 MySQL、Redis、对象存储、etcd、MQ，也没有任何真实客户端被驱动。
- 媒体字节既不进 MySQL、也不进用例：分片 PUT 与 OSS 的 `CompleteMultipartUpload`/`AbortMultipartUpload`
  在仓库内仍是 TODO 占位，本包**不可能**断言它们接通；相应用例只钉住「当前没做」这一事实（缺口 3、4），
  修缺口时这些断言要一起改。
- `media.task.v1` 的**发布侧**已被覆盖到「事件行内容与列/信封同源、四写同事务、投递走真适配器」，
  但**没有任何一条消息真的到过 broker**：Kafka 发送端只在 `-tags upload_kafka` 下存在，
  而 `kq.NewPusher`/`Close` 与 `Send` 的触网路径在测试里一律提前返回，全仓库也没有跑通真实 Kafka 的记录。
  同样，下游 asset/transcode/content-fingerprint 没有 consumer，端到端断在出口之后（缺口 1）。
- 预签名链路只有**离线判定**被验证：URL 是拼出来的 mock 串（`signature=MOCK`），过期时间、TTL 兜底、
  「不透出长期密钥」是被测结论；真实 OSS 是否会接受该 URL、上报的 ETag 是否来自真实上传都不在覆盖内，
  因此「分片真的传到了 OSS」目前不成立（缺口 3）。完成上传后下游接管的回调链同样未覆盖。
- 列级对账：本服务没有 model↔DDL 的离线门禁。迁移只在 `deploy/migrations/README.md` 登记
  （库 `go_video_upload`，3 条，`000001`~`000002` 为 `applied`、`000003` 为 `pending`；
  两个词的定义分别见该文件「当前覆盖」的「复验」列），真实/共享实例从未写入，
  上线仍须由维护者在目标实例执行，且**必须先于本次代码上线**。
- `internal/server`、`rpc/*.pb.go`、handler 等 goctl 生成壳不在单测范围内；改契约必须先改
  `rpc/upload.proto` 再执行统一生成（`docs/commands.md`）。

### 7. 验证命令

```bash
go test -p 1 -count=1 ./services/upload/...
gofmt -l services/upload
go vet ./services/upload/...

# 带标签一侧（两套都必须绿）
go build -tags upload_kafka ./services/upload/...
go vet   -tags upload_kafka ./services/upload/...
go test  -p 1 -count=1 -tags upload_kafka ./services/upload/...
```

- `-p 1` **必须保留**：Windows 页面文件限制下，并发链接多个测试包会因内存耗尽失败（`errno=1455`）；
  `-count=1` 关闭测试缓存，保证每次真的重跑。
- 整树 `go test ./...` 一次只跑一条命令，不要并发跑多个包级 test（同上内存限制）。
- 口径说明：`go vet` 期望无输出、`gofmt -l` 期望为空列表。五道门禁由主代理串行统一执行，
  本节只登记命令与口径，不在文档里代为声明执行结论。
