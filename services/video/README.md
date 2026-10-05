# video

UGC/PUGC 稿件主服务，对应参考仓库的 `videoup/archive` 领域。

- **拥有数据**：稿件（video_submission）、稿件版本（video_version）、状态流转审计（video_audit_log）、领域事件 Outbox（video_outbox）。
- **提供能力**：创建草稿、查询稿件详情、分页查询、更新稿件元信息、删除稿件、推进状态机、按状态查询。
- **依赖**：MySQL（稿件/版本/审计/Outbox）、Redis（稿件详情缓存）、Kafka/Redpanda（仅 `content.published.v1` 生产侧，`-tags video_kafka`）、`creator`、`asset`、`transcode`、`moderation-orchestrator`、`rights`。
- **数据所有权**：只有 `video` 服务可写入 video_submission 与 video_audit_log；媒体 Worker、审核服务只能通过 TransitionState 推进合法中间状态，不能直接写入 `PUBLISHED`。
- **状态机**（TransitionState 严格校验）：

  ```text
  DRAFT → UPLOADING → UPLOADED → SCANNING → TRANSCODING
        → READY_FOR_REVIEW → APPROVED → SCHEDULED → PUBLISHED
                             ├→ REJECTED ⇄ APPEAL（APPEAL 回 READY_FOR_REVIEW 或再驳）
  PUBLISHED → OFFLINE ⇄ PUBLISHED（可反复上下线，每次都各自成事件）
  PUBLISHED → EXPIRED
  PUBLISHED / OFFLINE → DELETED
  DRAFT / UPLOADING / UPLOADED / REJECTED → DELETED（未公开过的稿件可直接删）
  EXPIRED、DELETED 无出边（终态）
  ```

  合法转换表详见 `internal/logic/statemachine.go`；非法转换返回 `ErrInvalidStateTransition`。
  用例遍历 14×14 全部状态对，因此本图与代码若漂移，红的是 `transition_state_logic_test.go`。
- **关键规则**：
  - 只有本服务可以将稿件推进到 `PUBLISHED`，且必须经过 `APPROVED → SCHEDULED → PUBLISHED`。
  - 媒体回调只能推进合法中间状态。
  - 改变对外可见性的转换，与状态、审计同事务写一行 `content.published.v1` 到 `video_outbox`，由 `internal/publisher` 投递（见「Outbox 发布」；该表迁移未执行时可见性转换整笔回滚，见「数据与迁移」）。
  - 稿件详情走 Redis 短 TTL 缓存，稿件变更时失效（**本期只有失效侧**，见「已知缺口 1」）。
  - 不实现会员、订单、支付、投币、广告等商业化能力（见 AGENTS.md §1）。

## 已知缺口

以下条目全部由 `internal/logic` 与 `internal/publisher` 用例钉住现状（用例名见每条末尾），
不是待办清单的重复；修任何一条时对应用例会红，请连同本表、`internal/logic/fakes_test.go`
头部声明与 `internal/repository/contentevent.go` 的字段清单注释一起更新。
`file:line` 均为登记时实际读过的代码位置。条目 1~7 是稿件域既有缺口，8~15 是本轮接
`content.published.v1` 生产者时新增或新暴露的缺口。

1. **稿件详情缓存只有失效侧、没有写入侧**。`repository.Cache` 只实现 `Ping` 与
   `DelSubmission`（`internal/repository/repository.go:20-52`），全仓没有任何写入 `video:sub:<aid>`
   （`repository.go:28`）的代码；`GetSubmission` 直接回源 MySQL（`repository.go:104-108`），
   `GetPlayableSource` 每次播放解析都是「稿件 SELECT + 版次 SELECT」两次回源。
   因此上面第 24 行的「走 Redis 短 TTL 缓存」目前只在失效侧成立，缓存恒为 100% 未命中。
   用例：`TestGetSubmissionDoesNotReadCache`、`TestGetPlayableSourceAlwaysGoesToDatabase`。
2. **失效缓存侧的两处不一致**：(a) 同样是一次状态流转，`TransitionState` 会失效详情缓存
   （`internal/logic/transitionstatelogic.go:57`），`DeleteSubmission` 一条缓存调用都不发
   （`internal/logic/deletesubmissionlogic.go:53-56`）；(b) 失效失败被静默吞掉——
   `repository.go:125` 与 `transitionstatelogic.go:57` 都是 `_ =`。
   现状无害（缺口 1 导致缓存恒空），一旦接入 cache-aside，就是「稿件已删除/已变更但详情仍命中旧值 60 秒」的成因。
   本轮之后删除还会额外投出一条 `action=delete` 事件：索引会撤，详情缓存不动，两侧结论从此不一致。
   用例：`TestDeleteSubmissionDoesNotInvalidateSubmissionCache`、
   `TestUpdateSubmissionIgnoresCacheInvalidationFailure`、`TestTransitionStateIgnoresCacheInvalidationFailure`。
3. **什么都没改的更新会被判「稿件不存在」**。本服务 DSN 未开 `clientFoundRows`
   （`etc/video.v1.yaml:14`），真实 MySQL 的 `RowsAffected` 是 changed rows，
   `model/submissionmodel.go:183-189` 据此返回 `ErrSubmissionNotFound`；而 `UpdateSubmission`
   把未传字段回填成旧值（`internal/logic/updatesubmissionlogic.go:49-68`），
   于是「一次空 PUT」在真库必然命中 0 行。属主改自己的草稿也可能拿到 404。
   用例：`TestUpdateSubmissionNoOpUpdateDivergence`（同一开关下有效更新仍成功的对照也在同一条里）。
   可能修法：UPDATE 语句带 `mtime = ?` 以外的真实改动判定、改用 `clientFoundRows`、
   或在 logic 侧短路「无改动即直接返回当前行」。修法涉及 DSN 语义，需单独评审。
4. **状态推进的丢失更新检测不到**。`repository.TransitionState` 的二次校验读的是
   `r.subMd.FindOne`，**不带事务 session**（`internal/repository/repository.go:137`），
   状态 UPDATE 又没有 `AND state = ?` 的 CAS 条件（`model/submissionmodel.go:194-196`）。
   因此在「二次校验之后、UPDATE 之前」这个窗口里别人的写入会被无条件覆盖，并且整调用返回成功、
   审计里的 `from_state` 是过期值（例：审核刚落的 REJECTED 被 APPROVED→SCHEDULED 覆盖，驳回痕迹丢失）。
   用例：`TestTransitionStateLostUpdateIsNotDetected`；对照（插队发生在二次校验之前会被检出并拒绝）
   `TestTransitionStateLostUpdateBeforeTxCheckIsDetected`、`TestDeleteSubmissionDetectsConcurrentStateChangeBeforeTxCheck`。
   修法：校验读走 session，或 `UPDATE ... WHERE aid = ? AND state = ?` 后按 `RowsAffected==0` 重试。
5. **TransitionState 不校验调用方身份，审计里的「谁」由调用方自报**。`TransitionReq`
   没有任何**可信**身份/权限字段（`rpc/video.proto:112-118`：只有自由文本 `operator`，
   以及同样由调用方自报、且 video 侧全程不读的 `ip`——`in.Ip` 在非测试代码里零引用），
   logic 也不校验（`internal/logic/transitionstatelogic.go:31-62`），`operator`/`reason`
   原样写入 `video_audit_log`，包括空串。对比同服务的 `UpdateSubmission`/`DeleteSubmission`
   都会做属主校验（`updatesubmissionlogic.go:43-45`、`deletesubmissionlogic.go:44-46`）。
   该 RPC 目前已被**终端**网关暴露：`POST /video/submissions/:aid/transition`
   （`gateway/app/api/app.api:1122-1124`）把客户端表单里的 `operator` 原样转发
   （`gateway/app/api/app.api:1071-1077` → `gateway/app/internal/logic/transitionstatelogic.go:39-45`），
   而 gateway/app 整体尚无登录态中间件（其自述见 `gateway/app/internal/logic/conv_commerce.go:28-32`）：
   全网关只有 `AppkeyVerify` 一个中间件、且只挂在 `/account` 的部分路由上
   （`gateway/app/api/app.api:461` → `gateway/app/internal/handler/routes.go:87-90`），
   `/video` 分组的 `@server` 块（`gateway/app/api/app.api:1105-1107`）**一个中间件都没有**，
   即这条写路由连 appkey 校验也不过；
   运营面 `gateway/admin` 则另有 `adminActorGate`
   （`gateway/admin/internal/logic/transitionvideosubmissionlogic.go:39`）。
   结论：AGENTS.md §8 要求的「删除/下架保留审计证据」在这条链路上既可留空也可伪造。
   **可达结论（本轮按矩阵逐边核对）**：由于转换矩阵允许
   `DRAFT→UPLOADING→UPLOADED→SCANNING→TRANSCODING→READY_FOR_REVIEW→APPROVED→SCHEDULED→PUBLISHED`
   的每一条边都由同一个无鉴权入口驱动（`internal/logic/statemachine.go:10-23`），
   未登录客户端只要发 8 个 POST 就能把**任意已存在 aid**（不要求是自己的稿件，
   `TransitionState` 不查属主，与 `UpdateSubmission`/`DeleteSubmission` 不对称）
   推到 `PUBLISHED`——即「跳过审核发布」在这条链路上是可达的；
   反向同理，`PUBLISHED→DELETED`/`OFFLINE` 也让任意调用方可以对他人稿件执行下架。
   另有一条同源的断链：**没有任何服务会把审核结论回灌给 video**
   （全仓 `video.TransitionState` 的调用方只有 `gateway/app` 与 `gateway/admin` 两处；
   `services/video/internal/` 下没有 `consumer/` 目录，moderation-orchestrator 也不发事件），
   所以「靠媒体/审核回调推进中间状态」目前只是文档承诺，实际只能由人手工推进。
   用例：`TestTransitionStateAuditRowRecordsCallerSuppliedIdentity`、`TestTransitionStateIgnoresCallerIdentity`，
   以及把上面那段「可达结论」逐边跑一遍并与删除侧对照的
   `TestUnauthenticatedPublishChainReachesPublishedWhileDeleteIsOwnerChecked`
   （同一条 aid：空 `operator` 推满 8 步到 `PUBLISHED` 且十条审计的 `operator` 全为空串，
   而 `DeleteSubmission` 用陌生 `mid` 被 `ErrNotOwner` 拒、换回属主 `mid` 即成功——
   两侧同时断言，任何一侧的校验被加上或拆掉都会红）。
   **本轮之后代价更高**：这 8 步里 `SCHEDULED→PUBLISHED` 那一步现在会同事务写一行
   `content.published.v1`，一旦 `Kafka.Enabled=true` 且 `-tags video_kafka`，未经审核的他人稿件
   会被投进搜索索引（下架/删除同理），不再是「只有数据库脏」。
   修法：由网关按会话渲染 `operator`（与 commerce 域同一做法）并在 video 侧要求可信调用方，
   或给该 RPC 加 gRPC metadata 身份；终端侧只保留 DeleteSubmission 这类带属主校验的入口。
6. **状态枚举越界值不被入参守卫拦下**。`stateFromRPC` 本身是无条件 `int32(s)` 强转
   （`internal/logic/convert.go:14-16`，不做任何校验），唯一的枚举判断坐在两个调用点上且只判 0
   （`internal/logic/transitionstatelogic.go:35-38`、`internal/logic/listbystatelogic.go:32-35`），
   于是 `target=99/-1/15` 会先打一条 SELECT，
   再被状态机拒成 `ErrInvalidStateTransition`；`ListByState(state=99)` 则白查一次 COUNT 后返回空列表。
   无数据风险，但枚举校验缺一段，且错误码不是 `ErrInvalidTargetState`。
   用例：`TestTransitionStateRejectsOutOfRangeEnumAfterReading`、既有 `TestListByStateAcceptsUnknownEnumValue`。
7. **`video_version.state` 不参与播放判定**（观察项）。`LatestPlayableVersion` 只按
   「`asset_id` 非空的最新版次」挑选（`internal/repository/repository.go:175-186`、
   SQL 见 `model/submissionmodel.go:237-247`），`state` 列（`deploy/migrations/video/000002_create_video_version.sql:13`）
   被原样投影给网关却不设门槛。本期全仓没有任何代码写入 `video_version.state`
   （`VideoVersionModel.Insert` 无调用方），因此暂无实际受害者；一旦开始写状态，
   这条就是「已废弃版次仍可播放」的成因。用例：`TestGetPlayableSourceDoesNotJudgeVersionState`。
8. **`video_outbox` 迁移未执行 ⇒ 任何改变可见性的转换都整笔回滚**。
   `deploy/migrations/video/000004_create_video_outbox.sql` 目前是 `pending`（见
   `deploy/migrations/README.md`，**从未在任何实例执行过**），而 `TransitionState` 在事务内
   `outbox.Insert`（`internal/repository/repository.go:184`）。表不存在时
   `SCHEDULED→PUBLISHED`、`PUBLISHED→OFFLINE/EXPIRED/DELETED` 这些转换会连同状态与审计一起回滚，
   即**发布路径不可用**；`DRAFT→UPLOADING` 一类非可见性转换不受影响（不写事件）。
   因此部署顺序必须是「先跑迁移，再上线本次代码」，回滚反之（先停发布循环与写事件，再 `DROP TABLE`）。
   用例：`TestOutboxInsertFailurePropagatesAndStops`（事件写失败 ⇒ 状态与审计停在写之前、
   缓存一次都不失效）、`TestVisibilityTransitionRefusesToRunWithoutOutboxModel`（没注入 model 时
   装配错误直接报错而不是静默跳过）。
9. **从未与真实 broker 联调；默认构建根本不带发送端**。真实接线只在
   `internal/publisher/kafkaruntime_kafka.go`（`-tags video_kafka`，`kq.NewPusher` + `WithSyncPush`）；
   默认构建的 `NewSender` 恒返回 `ErrKafkaRuntimeNotBuilt`（`kafkaruntime_disabled.go:23-25`），
   于是「默认构建 + `Kafka.Enabled=true`」在 `internal/svc` 的 `logx.Must` 处启动即失败，
   而不是安静地不投。带 tag 的用例只验证「建通道与参数守卫」，**不向已登记 topic 调 `Send`**
   （那会真的去拨 broker）。因此「事件送达 search-indexer 与 inbox」没有任何自动化证据。
   用例：`TestDefaultBuildRefusesToCreateSender`、`TestDefaultBuildStartPublisherFails`、
   `TestDefaultRuntimeNotes`（默认侧）与 `TestTaggedSenderBuildsChannelsWithoutNetwork`、
   `TestTaggedSendRejectsBeforeDialing`、`TestTaggedRuntimeNotes`（带 tag 侧）。
10. **对接带鉴权/TLS 的集群不可用**。`SenderSettings` 只有 `Brokers`/`Topics` 两个字段
    （`internal/publisher/params.go:16-26`），`KafkaConf` 也刻意没有 `Username`/`Password`/`CaFile`
    三个键（`internal/config/config.go:31-55`）：go-queue v1.2.2 的 `kq.NewPusher` 不暴露 dialer 注入口，
    加键位也接不上。这是能力缺失而非行为缺陷，没有可钉的用例；换集群前必须先解决依赖能力。
11. **没有租约列 ⇒ 多副本发布存在双发窗口**。`ListPending` 只按
    `state = 0 AND (next_retry_at = 0 OR next_retry_at <= ?)` 取行（`model/videooutboxmodel.go:102-110`），
    取行本身不把行占住，`state` 要到投递成功后才由 `MarkPublished` 改（`:115-124`）。
    两个副本同时跑就会把同一事件投两次。消费方必须按 `event_id` 去重（search-indexer 与 inbox 已如此设计），
    本服务不提供 Exactly-Once。用例：`TestRunOnceThroughRealStore` 只钉单副本口径的「取到即投、投成即置位点」。
12. **`state=2`（判死）没有人工放行接口**。列写歪或重试耗尽后，行永远停在 `state=2`，
    本服务没有任何解锁/重投入口（`internal/publisher/outbox_store.go:93-96` 只有 `MarkFailed`），
    只能由运维手工 `UPDATE video_outbox SET state=0, retry_count=0`。
    用例：`TestRunOnceJudgesDefectiveRowWithoutSending`（判死且不占用发送端）。
13. **已发布行没有清理与归档**。`video_outbox` 每次可见性转换加一行，本服务与 `services/cron`
    都没有清理任务（迁移文件第 37~38 行只登记不做 DDL 删除），`state=1` 的行会无限增长；
    `mtime` 同时被 `MarkRetry`/`MarkFailed` 刷新（`model/videooutboxmodel.go:126-140`），
    因此它**不是**可信的「发布时间」，事后按时间归档还要额外判据。目前没有用例（缺的是任务不是行为）。
14. **`doc_revision` 只到秒**。它等于同事务 `video_audit_log.ctime` × 1000
    （`internal/repository/contentevent.go:129`，一次转换只取一次时钟），所以同一稿件在同一秒内
    完成「下架再上线」时两条事件的版本号相同；search-indexer 的 `ShouldOverwrite` 用 `>=`，
    后到的旧事件仍会覆盖新文档。修法要么把审计时间升到毫秒，要么给 Outbox 加单调序号列。
    用例：`TestDocRevisionSharesOneClockWithAuditRow`（钉住同源，并把这个同秒并列的可能写进注释）。
15. **没有 `update` 动作：已发布稿件改元信息不通知索引**。`contentActionFor` 只看 `toState`
    （`internal/repository/contentevent.go:64-78`），而 `UpdateSubmission` 只允许 DRAFT
    （`internal/logic/updatesubmissionlogic.go`），因此「已发布稿件改标题/封面/分区」既进不了
    状态机也不产事件。要支持在线改正文，需要先放开 `PUBLISHED` 下的可改列，再新增 `action=update`
    并递增 `schema_version`（消费侧字段语义也要一起评审）。
    用例：`TestNonEventPathsNeverTouchTheOutboxTable`（update/create/读路径与播放解析一次事件都不产）。

## 数据与迁移

| 表 | 迁移 | 关键约束 |
|---|---|---|
| `video_submission` | `deploy/migrations/video/000001_create_video_submission.sql` | `PRIMARY KEY(aid)`、`KEY idx_mid_ctime`、`KEY idx_state_ctime`、`KEY idx_typeid_ctime` |
| `video_version` | `deploy/migrations/video/000002_create_video_version.sql` | `PRIMARY KEY(id)`、`UNIQUE KEY uniq_aid_version`、`KEY idx_asset`；`state` 列本期无人写入（见「已知缺口 7」） |
| `video_audit_log` | `deploy/migrations/video/000003_create_video_audit_log.sql` | 只追加审计；`PRIMARY KEY(id)`、`KEY idx_aid_id` |
| `video_outbox` | `deploy/migrations/video/000004_create_video_outbox.sql` | `UNIQUE KEY uniq_event_id`（列级 `utf8mb4_bin`、无默认值）、`KEY idx_state_next_retry`、`KEY idx_event_type_ctime` |

迁移登记见 `deploy/migrations/README.md`（`go_video_video`，4 条：`000001`~`000003` 为 `applied`，
`000004` 为 `pending`，即**从未在任何实例执行过**）。这一条决定本服务的可用性：`000004` 未执行时
`TransitionState` 的「状态 + 审计 + 事件」事务会在 `outbox.Insert` 上失败并整笔回滚，
等于**发布、下架、过期、删除四条可见性路径全部不可用**（`DRAFT→UPLOADING` 一类不产事件的转换不受影响）。
部署顺序与回滚顺序见「已知缺口 8」。本服务没有 model↔DDL 的离线门禁
（`migration_parity_test.go` 只在商业化五服务有），`model/videooutboxmodel.go` 的 SQL 文本也没有断言。

## Outbox 发布（`content.published.v1`）

`internal/publisher` 是 `common/outbox` 引擎的本服务适配器，形状与 playback/upload/live-media/recommend-recall 一致：

- **产出**：唯一 topic `content.published.v1`
  （`eventenvelope.Topic(model.EventContentPublished, model.EventSchemaVersion)` 现场派生，
  配置里的 `Kafka.PublishTopics` 必须等于它，多写一个键名都让 `ValidatePublishKafka` 拒启动）。
  分区键 = `aggregate_id` = `aid` 的十进制字符串，所以同一稿件的事件必然同分区、按 `id` 升序生效。
- **哪些转换产事件**：`internal/repository/contentevent.go` 的 `contentActionFor` 单点判定，
  `→PUBLISHED` 为 `publish`、`→OFFLINE` 为 `offline`、`→EXPIRED` 为 `expired`、
  `→DELETED` **仅当**删除前处于 `PUBLISHED/OFFLINE/EXPIRED`（草稿从未进过索引，不发「你的作品被删除」）。
  其余转换不改变对外可见性，一行都不写。判定表在测试侧独立重写
  （`internal/logic/content_event_logic_test.go` 的 `wantEventAction`），因此实现与用例是两条来源。
- **写入点唯一**：只有 `Repository.TransitionState` 在事务内 `outbox.Insert`（`repository.go:184`），
  事件在「状态 UPDATE 之前」组装（组装失败即整笔回滚，不留改了状态却没事件的窗口）。
  `CreateSubmission`/`UpdateSubmission` 与全部读路径都不碰 `video_outbox`
  （`TestNonEventPathsNeverTouchTheOutboxTable`）。`Repository.OutboxModel()`（`:101`）是给发布器取句柄的
  导出口，**它带 `Insert`，所以「只有状态推进写事件」目前是靠约定而不是靠类型挡住**。
- **payload 字段清单**（13 个键，`doc_revision`/`publish_at`/`reason` 的取值口径见
  `contentevent.go:22-57` 的注释）：`action`、`content_id`、`content_type`（恒 1）、`title`、`description`、
  `cover_url`、`author_mid`、`typeid`、`tags`（恒为数组，不会是 null）、`publish_at`（仅 publish）、
  `ctime`、`doc_revision`、`reason`（仅非空时出现）。
  时长/作者昵称/分区名/版权窗口/热度/敏感标记/收件人一律不代答（AGENTS.md §5）。
- **不可发布判定**：`OutboxStore.toRow` 调 `outbox.CheckRow`，列与 payload 不同源（`event_id`/`aggregate_id`
  对不上、topic 不归属本服务、分区键为空、payload 非法 JSON）即写 `last_error = "unpublishable: ..."`
  并直接判 `state=2`，**不占用发送端**，也就不消耗重试次数。
- **构建标签**：Kafka 发送端在 `-tags video_kafka` 之后才存在（`kafkaruntime_kafka.go`，
  `kq.NewPusher` + `WithSyncPush`，同步模式才有真实错误可退避）；默认构建的 `NewSender` 返回
  `ErrKafkaRuntimeNotBuilt`，于是「开了 `Kafka.Enabled` 却用默认构建启动」会在 `logx.Must` 处启动即失败，
  而不是安静地不投。`KafkaConf` 刻意不含 `Group`（本服务只产不消），也不含 SASL/TLS 键位（见「已知缺口 10」）。
- **启动/停止**：`internal/svc/servicecontext.go` 在 `NewServiceContext` 里按 `Kafka.Enabled` 决定是否
  `startPublisher()`（失败 `logx.Must` 直接崩，理由：静默不投等于新发布的稿件永远进不了索引），
  并注册 `proc.AddWrapUpListener` 关发送端；`Enabled=false` 时只打 `RuntimeNotes` 的 Info 行。
  投递结果**不写日志**（排障靠查表：`SELECT * FROM video_outbox WHERE aggregate_id=?`）。
- **下游**：`search-indexer`（`-tags searchindexer_kafka`，写/撤索引文档）与 `inbox`
  （`-tags inbox_kafka`，给作者发下架/过期/删除站内信）**有消费者实现**，
  但本仓库从未与真实 broker 联调，端到端送达没有证据（「已知缺口 9」）。

## 运行

```bash
go run video.v1.go -f etc/video.v1.yaml                    # 默认构建：不链接 Kafka 发送端
go run -tags video_kafka video.v1.go -f etc/video.v1.yaml  # 链接发送端；仍需 Kafka.Enabled=true 且 000004 已执行
```

健康检查与端口：`ListenOn: 0.0.0.0:8095`，服务发现 Key 为 `video.v1.rpc`（网关经 etcd 接入）。

## 测试覆盖

离线单测（纯 Go 内存替身），不连接 MySQL、Redis、etcd、MQ、对象存储，也不依赖网络。
静态数由 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 导出；
动态数是 `go test -v` 里 `--- PASS`（顶层）与 `^    --- PASS`（子用例）的实测条数，
两者不相等是因为子用例由 `for range` 表驱动展开（例如遍历状态机全部合法边）。口径写成 `静态顶层/静态子行 → 动态顶层+子`。

| 构建 | 范围 | 顶层静态 | 子静态行 | 动态 PASS | FAIL/SKIP |
|---|---|---|---|---|---|
| default（未链接 Kafka） | `./services/video/...` | 115（logic 88 + publisher 22 + repository 4 + config 1） | 47（logic 44 + publisher 2 + config 1） | 115 顶层 + 332 子 = **447** | 0 / 0 |
| `-tags video_kafka` | 只有 `internal/publisher` 变（`-3` 个 default 专属、`+4` 个 tagged 专属），其余三包数字同上 | 23（publisher 单独） | 3 | 23 顶层 + 25 子 = **48** | 0 / 0 |

本服务没有处于 `t.Skip` 状态的用例，也没有任何「永真断言」占位。
上面「已知缺口」每条末尾的用例名就是本节表格里的这些判定，两边一一对应。

### 1. `internal/logic`（9 个文件：8 个用例文件 + `fakes_test.go` 替身层）— 静态 `88/44`，动态 `88+310`

| 文件 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|
| `createsubmission_logic_test.go` | 4 | 1 | 入参守卫 `mid → title → typeid` 先于任何依赖（投稿是网关高频入口，非法请求不许打到 MySQL）；草稿落库并由 INSERT 回填 `aid`；第二条投稿不复用第一个 `aid`；INSERT 失败原样上抛 |
| `read_logic_test.go` | 16 | 1 | 详情读只发一条 SELECT，且 model 行 → `rpc.Submission` 的 10 个字段逐个对上（字段绑错列即红）；「查无此行」是 `ErrSubmissionNotFound`、读失败必须原样上抛（两种结论不许混）；详情**不读缓存**（缺口 1）、`GetSubmission` 不做属主收窄；`ListSubmissions` 的 `ps` 守卫先于依赖、过滤条件与排序、第二页内容、越页仍照报 `total`、空集不发取行的 SELECT、`typeid` 过滤与错误透传；`ListByState` 的守卫顺序、按等值 `state` 过滤、未知枚举值照样查一次 COUNT（缺口 6）、错误上抛 |
| `update_submission_logic_test.go` | 8 | 9 | 可改列只有 `title/desc/cover/typeid/tag` 五个，`mid/state/ctime/aid` 不进 SET；「未传＝保持原值」⇒ 传空串**清不掉**任何字段；顺序是「读 → UPDATE → 失效详情缓存 → 复读」且应答来自复读而不是内存拼装；属主与状态两类守卫；`RowsAffected==0 → ErrSubmissionNotFound`（含真库 changed-rows 差异 ⇒「什么都没改的更新被判稿件不存在」，缺口 3）；依赖错误上抛；缓存失效失败被 `_ =` 吞掉（缺口 2） |
| `delete_submission_logic_test.go` | 10 | 6 | 删除是**状态流转到 `DELETED`**、不是物理删除（行还在、meta 列原封不动，只有 `state` 与 `mtime` 变）；与 `TransitionState` 复用同一条仓储路径，状态 + 审计行在同一个 `TransactCtx`；重复删除走 `StateDeleted` 短路、幂等且不再写第二条审计；陌生 `mid` 在短路**之前**就被 `ErrNotOwner` 拒；`operator` 由 logic 拼成 `owner:<mid>`、`reason` 固定串，调用方无法伪造（与 `TransitionState` 的自报口径相反）；允许/拒绝的来源状态跟随矩阵；删除**不失效**详情缓存（缺口 2b）；事务分阶段失败各自的残留形态；二次校验前被插队可检出（本轮起 `deleteTxSeq` 在「删除前公开过」的来源态上多一步 `video_outbox.Insert`，`DRAFT/UPLOADING/UPLOADED/REJECTED` 四种则没有） |
| `transition_state_logic_test.go` | 14 | 9 | 合法转换表的唯一来源是 `statemachine.go` 的 `legalTransitions`，用例遍历 14×14 全部状态对（不手抄枚举）；§8 的「禁止抄近路写 `PUBLISHED`」= 这张表没有跨级入边、中间态直发被拒；入参守卫只有 `aid>0` 与 `target != UNSPECIFIED`，越界枚举（99/-1）不被守卫拦下而在读库之后被状态机拒（缺口 6）；一次推进 = `TransactCtx` 内「二次校验读 → 状态 UPDATE → 审计 INSERT」+ 随后失效详情缓存 + 复读拼应答（本轮起：该转换改变可见性时事务里再多一步 `video_outbox.Insert`，期望序列由本包的 `wantEventAction` 判定表决定，而不是照抄实现）；`TestUnauthenticatedPublishChainReachesPublishedWhileDeleteIsOwnerChecked` 同一 `aid` 上双侧同时断言：空 `operator` 推满 8 步到 `PUBLISHED`（十条审计 `operator` 全为空串）而 `DeleteSubmission` 要属主（缺口 5 的可达结论）；并发窗口只在「二次校验之前」闭合，之后是检测不到的丢失更新（缺口 4，对照用例钉住前后两侧）；审计行记的是调用方自报身份、logic 忽略调用方身份；失效缓存失败被吞 |
| `get_playable_source_logic_test.go` | 11 | 6 | 只有 `state == PUBLISHED` 的稿件才会去查 `video_version`，`SCHEDULED`（定时未到）与其它任何状态都在首读之后直接拒绝（§8）；全程**只读**——一个事务、一条写 SQL、一次缓存失效都不许出现；可播放版次 = `ORDER BY version DESC` 之后第一个 `asset_id` 非空的版次，所以最新几个版次没登记媒资时会回退到旧版次；「没有可播放版次」在仓储层是 `(nil, nil)`、由 logic 判成 `ErrSubmissionNotPlayable`，而不是返回 `version` 为 nil 的成功应答；不判定版次 `state`（缺口 7）；稿件详情缓存**从不被读**、每次播放解析都回源（缺口 1）；忽略请求方 `mid` |
| `update_delete_logic_test.go` | 14 | 5 | 更新 + 删除两条路径的对照文件：守卫顺序与拒绝次序、空字段合并 vs 整体替换、写错误传播、缓存失效错误被吞、复读失败仍报失败、No-Op 更新差异；删除侧的非属主/缺行、非法来源态、状态 + 审计同事务、已删除幂等、推进错误传播（本轮起本文件的 `deleteSeq` 与分方法文件同源判定：`PUBLISHED/OFFLINE→DELETED` 的期望序列尾部多一步 `video_outbox.Insert`）。**本轮处置记录**：该文件与两个分方法文件在包内有 3 个同名 `Test…` 与同名 `updateSuccessSeq`，同包重复声明会让整个 logic 测试包编译失败（`go build ./...` 与 `gofmt` 都看不见，只有 `go vet`/`go test` 才报）；处置是**给本文件的三个用例与那个包级变量加 `Pair` 前缀换名**（`TestPairUpdateSubmissionGuardOrderKeepsRowUntouched`、`TestPairUpdateSubmissionSameValuesPatchIsNotFound`、`TestPairDeleteSubmissionGuardOrderKeepsRowUntouched`、`pairUpdateSuccessSeq`），一条断言都没有删；其中 SameValues 那条与分方法文件的 `TestUpdateSubmissionNoOpUpdateDivergence` 打的是不同入参（「五字段全给回原值」vs「空补丁」），两侧都保留 |
| `content_event_logic_test.go` | 11 | 7 | 本轮新增，`content.published.v1` 的生产侧判定：`SCHEDULED→PUBLISHED` 恰好写一行、六个业务列与 `state/retry_count/next_retry_at/last_error` 的初值逐个对上（信封与列同源，`occurred_at` 由 RFC3339 反解后必须等于列值）；payload 的键集合钉成等式（publish 13 键、offline 12 键无 `publish_at`、空 `reason` 再多掉一键，证明 `omitempty` 真的生效）；事件取的是**事务内二次校验读到的行**而不是 logic 的首读（用 hook 在两次读之间改标题才能分辨）；`doc_revision` 与同批 `video_audit_log.ctime` 同源（× 1000）、`publish_at` 只在 publish 上赋值、第二次转换的 `event_id` 必须不同（同稿件反复上下线各自成事件）；`TestActionMappingCoversEveryLegalEdge` 遍历 `legalTransitions` 的**每一条合法边**，事件行的有无与 `action` 必须逐条等于本文件独立重写的 `wantEventAction` 判定表（矩阵加边就会红，逼改状态机的人回答「这条要不要通知索引」）；删除侧分叉：未公开过的四种来源态删除写审计但零事件，公开过的写一行 `delete` 且 `reason` 与审计行同串；`EXPIRED` 目前没有出边因此删不掉（判定表那一格仍声明 `delete`，放开边时若漏投会红）；`tags` 恒为数组且空白段丢掉；`outbox.Insert` 失败 ⇒ 整笔回滚、状态与审计停在写之前、缓存一次都不失效；没注入 `VideoOutboxModel` 时可见性转换必须报错而不是静默跳过（非可见性转换仍可）；`CreateSubmission`/`UpdateSubmission`/三个读方法/`GetPlayableSource` 一条 `video_outbox.` 调用都不许出现 |
| `fakes_test.go` | 0 | 0 | 替身层与装配缝，并登记 8 个 RPC 方法与文件的分工；纪律是读返回值拷贝、`Insert` 按 `max+1` 分配主键且**只写生产 SQL 出现的列**、`UpdateState`/审计 `Insert`/事件 `Insert` 只接受非 nil 事务会话、未实现的写方法返回 `errUnexpectedDependency`（不放行假成功；`fakeOutboxModel` 的 `ListPending`/`Mark*` 因此是「logic 不许读 Outbox 位点」的类型级证明）、布数据走静默路径 |

### 2. `internal/publisher`（5 个用例文件）— default 静态 `22/2` → 动态 `22+21`；`-tags video_kafka` 静态 `23/3` → 动态 `23+25`

两个构建只有 `kafkaruntime_*_test.go` 这一对互换（`-3`/`+4` 个顶层、`+1` 个子静态行），
其余三个文件在两侧都编译、都跑。

| 文件 | 构建 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|---|
| `outbox_store_test.go` | 两侧 | 10 | 1（表驱动展开 9） | 列 → `outbox.Row` 的映射（topic 由 `event_type`+`schema_version` 现场拼出、分区键取 `aggregate_id` 即 aid 的十进制串、payload 原样透传不重新编码）；`RequiredTopic()` 必须等于 `"content.published.v1"` 且等于 `eventenvelope.Topic(model 常量)`（改名等于换掉 search-indexer 与 inbox 的订阅）；`toRow` 的九条「不可发布」判定逐条点名（event_type 写歪 / schema 升 v2 / event_type 空 / event_id 空 / 聚合根空 / payload 非 JSON / payload 缺 producer / payload 的 event_id 与列不一致 / payload 的 aggregate_id 与列不一致）；`ListPending` 跳过 nil 行、`now`/`limit` 原样透传（吞掉参数＝提前投或超量投）、读库错误必须同时保留底层错误与表名；三个状态写方法把 `publishedAt`/`retry_count`/`next_retry_at`/`last_error` 逐参数交给 model 且调用序列固定；`RunOnce` 端到端（真适配器 + 假 model + 假发送端）只走 `ListPending`+`MarkPublished`，**不出现 `Insert`**（写事务属于 logic 的权限）；同一 aid 的两条相邻事件（先发布、后下架）同分区且按 id 升序投出（分区键换成 event_id 就等于允许「已发布」覆盖「已下架」）；投递失败 ⇒ `MarkRetry(1, 未来)` 且**不**写已发布（`RunOnce` 返回 nil 是引擎口径，证据只能在行状态里找）；不同源的行直接 `MarkFailed` 且 `last_error` 以 `unpublishable: ` 开头、一次都不触达发送端、不消耗重试；编译期 `var _ outbox.Store = (*OutboxStore)(nil)` 钉接口签名漂移 |
| `params_test.go` | 两侧 | 6 | 1（表驱动展开 12） | `SenderSettingsFrom` 去空白、去重、保持声明顺序，且 `Brokers` 必须是新切片（改派生结果不能污染配置）；`OptionsFrom` 六个旋钮对六个配置键逐一对号，`Name` 必须等于本服务标签（日志要能分辨是哪个服务的循环）；`ValidatePublishKafka` 十二种不合规配置各自的键名出现在错误里（broker 缺失/含空串、topic 缺失/含空串、**多写一个本服务不产出的 topic**、四个数值旋钮为 0、退避上限小于基数），错误同时给出 `RequiredTopic()`；校验是聚合式的（四条坏键 ⇒ 至少三个分隔，运维一次看全）；`NewPublisher` 的四条出口（config→model→sender→engine）与 `pub.Options()` 透传、`!pub.Running()`；配置层放行而引擎层拒绝的组合必须被拦住（`MaxRetries=1<<31` 过 config 层，错误里同时点名 `Options.MaxAttempts` 与 `Kafka.MaxRetries`） |
| `example_yaml_test.go` | 两侧 | 3 | 0 | `etc/video.v1.yaml` 与发布器之间「只差一个开关」：当前 `Enabled=false`，把它翻成 true 后 `ValidatePublishKafka` 全绿、`PublishTopics` 恰好等于 `RequiredTopic()`、六个旋钮映射成 `Options` 字面量；用示例配置 + 假 model + 假发送端真的能装配出发布器并投出那一条（topic、分区键 `101`、`publishedAt` 落在调用前后区间内、发送超时等于配置的 5s）；顺带防住同文件与发布无关的风险：`Name=video.v1.rpc`、`ListenOn=0.0.0.0:8095`、`CacheRedis.Host` 非空、`DataSource` 指向 `127.0.0.1` 的 `go_video_video`，失败文本里口令必须先 `ReplaceAll` 成 `<redacted>`（缺口 10 的口径来源；yaml 可加载性本身仍归 `internal/config/config_load_test.go`） |
| `kafkaruntime_disabled_test.go` | default（`!video_kafka`） | 3 | 0 | 默认构建的真实上限：参数完全合规也拿不到发送端（nil + `errors.Is(err, ErrKafkaRuntimeNotBuilt)`），错误文本必须同时给出 `video_kafka`、`broker`、`Kafka.Enabled` 三个可执行的下一步；`RuntimeNotes` 在 `Enabled=false` 时点名「未链接」+ 积压位置 `video_outbox` + topic + 在等的下游 `search-indexer`/`inbox`（否则运维不知道去哪查、谁在落后），`Enabled=true` 时只剩一条；按 svc 的入口形状确认「打开开关就炸」，且错误里不能同时返回非 nil 发送端（否则 svc 会带着发不出东西的对象继续启动）（缺口 9） |
| `kafkaruntime_kafka_test.go` | `-tags video_kafka` | 4 | 1（表驱动展开 4） | 带 tag 的构建能按配置建立发送通道并优雅 `Close`（去重到一条 topic、Close 后再 Close 幂等、Close 后 `Topics()` 为空），全程不触网；四条入口守卫逐条点名（brokers/topics/空的 topic/空的 broker）；两类错误在**触网之前**返回：未建立通道的 topic（`未建立发送通道`）与空分区键（`partition key`）；`RuntimeNotes` 的「已链接」不等于「已在投递」口径：`Enabled=false` 仍点名 `video_outbox`，`Enabled=true` 说明里必须出现「已链接」「联调」与两个下游，且**不再**提 `video_outbox`（此时没有积压可解释） |

### 3. 其他层

- `internal/repository`（1 个文件 `4/0`）：`version_test.go` 直接测 `LatestPlayableVersion`——
  只按「`version` 倒序后第一个 `asset_id` 非空」挑版次、全行都没有 `asset_id`、空列表、model 报错上抛；
  `fakeVersionModel` 只实现 `ListByAid`，因此「这条读不写库、也不触达连接」是被替身强制的。
  同包内的 `Repository.TransitionState`/`Cache` 等**没有**独立用例，只在 logic 用例里被真实驱动。
- `internal/config`（1 个文件 `1/1`）：`config_load_test.go` 逐个加载 `etc/*.yaml`（子用例按文件名展开），
  反射递归断 `*DataSource` 与 `redis.RedisConf.Host` 非空；缓存字段命名撞 `zrpc.RpcServerConf`
  内嵌的 `Redis` 会让服务启动即报 `conflict key redis`。
- `model/`（`submissionmodel.go`、`videooutboxmodel.go`、`errors.go`、`now.go`）**无离线单测**：
  四条迁移的列级对账、`RowsAffected` 语义、`ORDER BY`/`LIMIT/OFFSET` 文本、
  `video_outbox` 的 `ListPending`/`Mark*` SQL 都没有门禁。全仓 12 个服务有 `model/migration_parity_test.go`
  那一类 model↔DDL 对账（`recommend-recall`、`live-room`、`engagement` 等都在内），**本服务不在内**，
  因此「`000004` 的列与 `VideoOutboxModel` 的查询列一致」只有人工阅读保证，属缺口 8 的一部分。
- `internal/svc` **无离线单测**：`NewServiceContext` 里「`Kafka.Enabled` ⇒ `startPublisher()`，失败 `logx.Must`
  直接崩」与 `proc.AddWrapUpListener` 收尾只由 `internal/publisher` 的 `TestDefaultBuildStartPublisherFails`
  按同一入口形状间接钉住，装配本身（含 etcd、Redis 连接）没有被驱动；
  `internal/server`、`rpc/*.pb.go` 是 goctl 生成壳。
- 本服务没有 `internal/consumer`、`internal/policy` 层——缺口 5 已登记「没有任何服务把审核结论
  回灌给 video」，所以「媒体/审核回调推进中间状态」这条 §8 承诺既没有代码也没有用例。

### 4. 构造器级覆盖：logic **8/8**，publisher **3/3**

- `internal/logic`：探针取全部 `New*Logic(`，共 8 个（`CreateSubmission`、`GetSubmission`、
  `ListSubmissions`、`ListByState`、`UpdateSubmission`、`DeleteSubmission`、`TransitionState`、
  `GetPlayableSource`），`gaps:` 为空，每个 RPC 方法都有直接驱动自身构造器的用例。
- `internal/publisher`：三个导出构造器各有用例：`NewOutboxStore`（`TestNewOutboxStoreRejectsNil`
  钉 nil model 拒绝，同文件另有六个用例经它装配真适配器跑读取/状态写/`RunOnce`）、`NewPublisher`
  （`TestNewPublisherAssembly` 钉四条出口 + `TestNewPublisherDelegatesEngineValidation` 钉引擎层拒绝）、
  `NewSender`（默认侧 `TestDefaultBuildRefusesToCreateSender`/`TestDefaultBuildStartPublisherFails`，
  带 tag 侧 `TestTaggedSenderBuildsChannelsWithoutNetwork`/`TestTaggedSenderRejectsIncompleteSettings`）。

### 5. 替身层与断言口径

- 装配缝：`internal/repository` 的 `NewWithDeps(内存缓存, 假连接, 内存 model)` 组装**真实 Repository**，
  只换它的 6 个依赖（缓存、连接、submission/version/audit/outbox 四个 model，
  `repository.go:94-98`）；于是「状态推进是否在一个事务内写状态 + 审计 + 事件行」
  「失效缓存发生在落库之后还是之前」「读是否走 model」都整条留在被测路径上，
  而不是把 Repository 一起 mock 掉。
- 断言口径：有序 `callLog`（`<表>.<方法>[:<键>]`）既数次数也断顺序——「守卫拒绝后一次依赖调用都不许发生」
  「List 的 COUNT 先于 SELECT 且同口径」都是顺序结论；状态期望一律由 `statemachine.go` 的
  `legalTransitions` 反推，不手抄枚举；替身**不做回滚**，所以失败用例断言的是「库里实际还剩什么」。
- **它证明不了**：SQL 文本与列清单（缺口 3 的 changed-rows 差异只能靠 `TestUpdateSubmissionNoOpUpdateDivergence`
  登记口径 + 真库验证，内存替身按 matched-rows 建模）、`video_submission.aid` 的真实
  `AUTO_INCREMENT`、`video_audit_log` 的只追加约束与索引、`video_outbox.uniq_event_id` 的唯一约束与
  `event_id` 的列级 `utf8mb4_bin`（因此缺口 11 的双发窗口、以及「只差大小写的 event_id 撞唯一键」
  在替身上都不会暴露）、`deploy/migrations/video/*.sql` 与
  model 的列级一致性、真实 Redis 的 `DEL`/`SETEX` 行为、`TransactCtx` 在真库上的原子性。
  发布器侧同理：`internal/publisher` 的五个文件用假 model 与假发送端，
  真实 broker 的分区分配、`kq` 的重连与 `WithSyncPush` 的确认语义一概不被断言。

### 6. 覆盖边界（不可省略）

- 用例不连接 MySQL、Redis、etcd、MQ、对象存储，也不依赖网络；`creator`/`asset`/`transcode`/
  `moderation-orchestrator`/`rights` 五个依赖都不被驱动，稿件域内没有任何真实下游调用。
- 媒体字节不进 MySQL、也不进用例：本服务只持稿件主数据与版次引用，播放地址由 `playback` 签名——
  **预签名与转码产物链路在本服务侧只有离线判定**：`GetPlayableSource` 钉住「哪个版次算可播」，
  而 `asset_id` 指向的对象存储里到底有没有文件、转过码没有，一概不成立也不被断言。
- **回调链路只有离线判定**：`TransitionState` 的 §8 状态机门槛、事务写序（状态 + 审计 + Outbox 三写）
  与审计投影是被测结论；但真实媒体/审核回调入口不存在（无 consumer），所以「审核结论真的回灌」
  没有自动化验证，缺口 5 的可达结论来自用例内连续调用 8 次 logic，而不是来自任何端到端环境。
- **事件链路的口径是「离线接线完整、线上从未验证」**：`content.published.v1` 的生产侧本轮已接通
  （logic 事务内写 `video_outbox` + `internal/publisher` 的发布循环 + svc 装配），
  不再是「由调用方/TODO 接入」；但两个前置仍未成立：`000004` 迁移未在任何实例执行（缺口 8），
  默认构建不带发送端且带 tag 侧也从未与真实 broker 联调（缺口 9），
  因此「发布/下架真的撤了索引」「作者真的收到站内信」只有契约与离线判定，没有送达证据。
  下游 `search-indexer`（`-tags searchindexer_kafka`）与 `inbox`（`-tags inbox_kafka`）有消费者实现，
  但同样没有端到端记录。
- 迁移 SQL ↔ 真实库的列级对账：本 README 没有声明在隔离实例 `127.0.0.1:3399` 复验过列级一致性，
  因此按**未在目标实例复验**处理；`model/` 无离线单测，本包也没有迁移↔model 的自动对账门禁，
  迁移登记只以 `deploy/migrations/README.md` 为权威。
- `internal/server`、`rpc/*.pb.go`、handler 等 goctl 生成壳不在单测范围内；
  改契约先改 `rpc/video.proto`（与 `api/*.api`），再执行统一生成（`docs/commands.md`）。

### 7. 验证命令

```bash
go test -p 1 -count=1 ./services/video/...
go test -p 1 -count=1 -tags video_kafka ./services/video/internal/publisher/
gofmt -l services/video
go vet ./services/video/...
```

- `-p 1` **必须保留**：Windows 页面文件限制下并发链接多个测试包会 OOM（`errno=1455`）；
  `-count=1` 关闭测试缓存。
- 第二条命令只跑 `internal/publisher`：`video_kafka` 在别的包里换不出任何文件，
  带 tag 跑整树（`go test -p 1 -count=1 -tags video_kafka ./services/video/...`）结果相同、同样 rc=0，
  但只有单独跑这一包才能把「带标签侧 23 顶层 / 48 条」与默认侧「22 顶层 / 43 条」对上号，
  红的时候直接归因到发布侧适配层。
- `go vet` 期望无输出、`gofmt -l` 期望为空列表。五道门禁统一串行执行，本节只登记命令与口径，
  不在文档里声明执行结论。
