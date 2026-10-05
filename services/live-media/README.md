# live-media

直播媒体执行面：把「一路直播流」变成「可分发的多档输出 + 可回看的录制切片 + 一条普通视频稿件」，
再把过期产物安全回收。全链路都是**任务登记与状态投影**——真正的 FFmpeg/CDN/对象存储操作由外部 Worker 执行，
本服务只负责登记意图、约束状态机、留证据、发事件。

- 数据所有者：live-media 服务（本库 8 张 `live_*` 表只有本服务可读写，AGENTS.md §5）
- 数据库：`go_video_live_media`（`deploy/migrations/live-media/`）
- 注册中心 etcd Key：`livemedia.v1.rpc`，监听 `0.0.0.0:8120`
- 契约源：`rpc/livemedia.proto`（`rpc/*.pb.go`、`internal/server/`、`internal/types/`、入口 `livemedia.v1.go`
  均为 `goctl`/`protoc` 产物，禁止手改；改契约后执行 `powershell -File scripts/gen.ps1 -Service live-media`）
- 无 HTTP 面：只暴露 gRPC，观众/主播侧一律经 `gateway/app`、运营侧经 `gateway/admin` 聚合（AGENTS.md §6）
- 调用方（预期）：`live-ingest`（推流侧起停转码/录制）、`live-room`（房间档位与回放列表）、
  `live-gateway`（播放地址下发）、`gateway/*`、`services/cron`（超时清扫与回收调度）

## 1. 职责与边界

| 归本服务 | 不归本服务 |
|---|---|
| 转码/录制/回放/回收四类任务的登记、状态机、幂等与超时清扫 | 拉流、协议握手、流安全（推流鉴权、串流防护）→ `live-ingest` |
| 分发档位（自然键：房间+场次+档位+协议）的在线/下线事实 | 房间生命周期、开播/关播语义、观众鉴权 → `live-room` |
| 录制切片的序号、缺口探测与校验证据（回放可拼接性的真值） | 播放地址签发、CDN 调度、就近接入 → `live-gateway` + CDN 控制面 |
| 回放产物登记为普通稿件（**同步调 AssetRPC/VideoRPC** + 本地引用行 + 事件广播） | 审核结论、稿件状态推进 → `moderation-orchestrator` + `video` |
| 回收任务（登记意图 → 执行 → 回报计数）与对象存储真删 | 转码模板与 Worker 编排 → `transcode`；媒资去重/指纹 → `asset` |

三条不可让的边界（`livemedia.proto` 头部硬约束的展开）：

1. **回放不抄近路。** 回放产物必须走 `asset.RegisterAsset` → `video.CreateSubmission` →
   `moderation-orchestrator` 的普通视频全流程。本服务没有「直接发布回放」的入口：
   `live_replay_task` 的 `COMPLETED` 只能由 `ApplyReplayContentState`（video 事实投影）驱动，
   状态机在 `model.IsValidReplayTransition` 里就不允许 Worker 一步到 `COMPLETED`
   （`ReportReplayProgress` 上报 `COMPLETED` 会被判非法迁移）。
2. **实时链路与回放状态分开。** `live_stream_output`（在线/下线）与 `live_replay_asset_ref.review_state`
   （未同步/审核中/驳回/已发布/已下架/已删除）是两套语义，不能互相推断：断流不代表回放不可看，
   回放被下架也不代表档位要下线。
3. **大文件与凭据不入库。** 表里只有 `bucket` + 相对 `object_key`（无签名参数、无查询串），
   转码产物字节、切片内容、CDN 密钥一律在对象存储/Secret；`err_msg` 只存脱敏摘要（`VARCHAR(512)`）。

## 2. RPC 方法 ↔ 表 ↔ 幂等键

28 个方法分四类。**当前阶段 = 28 个方法已全部落地**（§7 给出验证口径与仍缺的部分）：
每个文件的注释按实现顺序写明了入参与业务键校验次序 → 幂等依据 → 允许的迁移 → 事务边界 →
下游依赖为 nil 时的显式报错 → 错误映射，实现严格照该次序落。

### 2.1 转码任务（`live_transcode_task`）

| 方法 | 表 | 幂等 / 并发依据 |
|---|---|---|
| `StartLiveTranscode` | `live_transcode_task` INSERT | **`uniq_request_id`**；命中唯一索引时返回既有任务（不报错、不新建），`ErrRequestIdDuplicated` 在 model 层包装 |
| `StopLiveTranscode` | 条件 UPDATE | `(task_id, state, version=expected_version)` CAS，`RowsAffected=0` → `ErrVersionConflict`/`ErrInvalidTransition` |
| `RetryLiveTranscode` | 条件 UPDATE | 仅 `FAILED→PENDING`；`attempt+1` 受 `max_attempts` 快照约束，超限 `ErrAttemptExhausted` |
| `CancelLiveTranscode` | 条件 UPDATE | 仅 `PENDING→CANCELLED`、`STOPPING→CANCELLED`；`RUNNING` 必须先 `STOPPING`（不留「取消即孤儿进程」） |
| `ReportLiveTranscodeProgress` | 条件 UPDATE | 无 `request_id`，靠 `expected_version` CAS + `timeout_at = now + 超时秒数` 续租；终态拒绝迟到的上报（`ErrTerminalState`） |
| `GetLiveTranscodeTask` / `ListLiveTranscodeTasks` | 只读 | `idx_room_state_ctime` / `idx_session_state` / `idx_template_state`；`page.ps` 超 `LiveMedia.MaxListPageSize` 当前由 `clampPage` 静默夹取（与 `ErrPsTooLarge` 的口径待统一，见 §8.8） |

### 2.2 分发档位（`live_stream_output`）

| 方法 | 表 | 幂等 / 并发依据 |
|---|---|---|
| `UpsertStreamOutput` | `live_stream_output` UPSERT | **天然键 `uniq_output_natural(room_id, live_session_id, bitrate_level, protocol)`**；同一天然键反复上下线是正常语义，因此本表 `request_id` **不建唯一索引**（只留最近一次），`state` 本身充当 CAS 条件（无 `version` 列） |
| `OfflineStreamOutput` | 条件 UPDATE | `state=在线 → 已下线`，写 `offline_reason`/`offline_at`；已下线时 `RowsAffected=0` 视为幂等成功 |
| `ListStreamOutputs` | 只读 | `idx_room_state`，默认只返回在线档位（`include_offline` 控制） |
| *（非 RPC）* `logic.OfflineSessionOutputs` | 批量条件 UPDATE + 逐档位 Outbox，同一事务 | 由 `live.state.v1` 的 `Stopped` 事件驱动（`internal/consumer`，见 §6.2）：事件只给房间+场次，所以一次摘掉该场次**全部**在线档位。幂等同样以 `state` 为 CAS 条件，`request_id` 写成 `evt:<event_id>` 只进事件 payload 用来区分自动/人工 |

### 2.3 录制与切片（`live_record_task`、`live_record_segment`）

| 方法 | 表 | 幂等 / 并发依据 |
|---|---|---|
| `StartLiveRecord` | `live_record_task` INSERT | **`uniq_request_id`**；`segment_seconds` 按配置夹取后快照，续录不回改 |
| `StopLiveRecord` | 条件 UPDATE | `RECORDING→STOPPING`，实际 `record_end_at` 只由 Worker 上报 `STOPPED` 写入 |
| `ReportLiveRecordProgress` | 条件 UPDATE | `expected_version` CAS；`last_seq` 用 `GREATEST(last_seq, ?)` 保证单调（`ErrSeqNotMonotonic`）；`heartbeat_at` 续租 `timeout_at` |
| `GetLiveRecordTask` / `ListLiveRecordTasks` | 只读 | `idx_room_state_ctime` / `idx_session_state` |
| `ReportRecordSegment` | `live_record_segment` `INSERT IGNORE` + 条件推进（`UpdateState`） | **`uniq_record_seq(record_id, seq)`**：`INSERT IGNORE` 命中已存在行时 `RowsAffected=0`，logic 回读既有行再按状态机推进，重复回报不产生第二行；状态按 `SegmentStateRank` 只前进不后退（`ErrInvalidTransition`）；`seq<=0` → `ErrInvalidSeq`；非缺口态缺 `bucket/object_key` → `ErrInvalidBucketRef` |
| `ListRecordSegments` | 只读 | **keyset 分页**（`after_seq` + `idx_record_state_seq`），禁止 `OFFSET` 深翻；`limit` 上限 `LiveMedia.MaxSegmentPageSize` |

### 2.4 回放（`live_replay_task`、`live_replay_asset_ref`）

| 方法 | 表 | 幂等 / 并发依据 |
|---|---|---|
| `SubmitReplayTask` | `live_replay_task` INSERT + 区间体检 | **`uniq_request_id`**；`from_seq/to_seq` 缺省归一化为 `1..record.last_seq`；区间内 `MISSING/CORRUPT` 且 `allow_gaps=false` 且缺口数 > `LiveMedia.MaxReplayGapSegments` → `ErrReplayGapNotAllowed`；区间未被录制覆盖 → `ErrSegmentRangeNotRecorded` |
| `ReportReplayProgress` | 条件 UPDATE | `expected_version` CAS；**禁止上报 `COMPLETED`**；产物 `output_bucket/output_key` 只接受相对 key（`ErrInvalidBucketRef`） |
| `GetReplayTask` / `ListReplayTasks` | 只读 | `idx_record_range` / `idx_room_state_ctime` / `idx_anchor_state` / `idx_asset` |
| `BindReplayAsset` | `live_replay_asset_ref` UPSERT（`ON DUPLICATE KEY UPDATE`） | **`uniq_replay_id` + `uniq_asset_id` + `uniq_aid`**（三个唯一键：一场回放一条引用，且禁止同一 `asset`/`aid` 被两场回放认领，`ErrAssetRefConflict`）；`asset_id`/`aid` 为 0 时拒绝（`ErrInvalidAssetID`/`ErrInvalidAid`）；命中唯一键时走更新分支并按 `replay_id` 回读主键（`LastInsertId` 在该分支不可信） |
| `ApplyReplayContentState` | `live_replay_asset_ref` 投影 UPDATE（+ 反向推进 `live_replay_task`） | **按 `event_id` 幂等**（`last_event_id` 相同直接返回既有行）+ **乱序保护**（`review_state_at <= at` 才覆盖）；`review_state=0` 非法（`ErrInvalidReviewState`）；`review_state=Deleted` 时把引用行 `retention_state` 置 1（待回收），**不改 video 任何东西** |
| `ListReplayAssetRefs` | 只读 | `idx_anchor_id` / `idx_room_session` / `idx_review_state`；只读投影，不做任何推进 |

### 2.5 回收（`live_retention_task`）

| 方法 | 表 | 幂等 / 并发依据 |
|---|---|---|
| `SubmitRetentionTask` | `live_retention_task` INSERT | **`uniq_request_id`**；`target_kind` ∈ {切片, 回放产物, 分发残留}；`target_id` 与 `expire_before` 至少一个（`ErrRetentionTargetRequired`）、`reason` 必填（审计要求，`ErrRetentionReasonRequired`）；`purge` 默认 false（只登记并置待回收标记，`deleted` 恒为 0），true 才允许 Worker 真删对象引用；`batch_limit` 夹取到 1..500 |
| `ReportRetentionResult` | 条件 UPDATE | `expected_version` CAS；`scanned/deleted/skipped` 是**覆盖写**的 Worker 证据，绝不累加（重复上报不放大计数） |
| `GetRetentionTask` / `ListRetentionTasks` | 只读 | `idx_target_state` / `idx_kind_state` / `idx_room_state` |

错误一律由 `model` 的哨兵错误表达（`errors.go` 里 30+ 个：`ErrInvalidRoomID`、`ErrEmptyRequestID`、
`ErrRequestIdDuplicated`、`Err*NotFound`、`ErrInvalidTransition`、`ErrTerminalState`、`ErrVersionConflict`、
`ErrAttemptExhausted`、`ErrSeqNotMonotonic`、`ErrReplayGapNotAllowed`、`ErrInvalidBucketRef`、
`ErrAssetRefConflict`、`ErrInvalidReviewState`、`ErrEmptyEventID`、`ErrRetentionReasonRequired` …），
在 server 出口映射成 gRPC code；**不向调用方外泄 SQL 片段、拉流地址或对象存储签名**。

## 3. 数据表

迁移目录 `deploy/migrations/live-media/`（`CREATE TABLE IF NOT EXISTS` + InnoDB + utf8mb4 + 每列中文 COMMENT，
可重复执行；每个文件头部含 用途 / 数据所有者 / 影响 / 回滚 / 锁风险）：

| 文件 | 表 | 关键约束与索引 |
|---|---|---|
| `000001_create_live_media_realtime_tables.sql` | `live_transcode_task` | PK `task_id`；**`uniq_request_id`**；`idx_room_session_level`（同档位重复拉起检测）、`idx_room_state_ctime`、`idx_session_state`、`idx_template_state`、`idx_state_timeout`（超时清扫）；`progress` 标注「投影采样值，非唯一事实源」 |
| | `live_stream_output` | PK `output_id`；**`uniq_output_natural`**；`idx_room_state`、`idx_session_state`、`idx_state_expire`、`idx_task`；无 `version` 列（`state` 即 CAS 条件），`request_id` 明确不唯一 |
| `000002_create_live_record_tables.sql` | `live_record_task` | PK `record_id`；**`uniq_request_id`**；`idx_room_state_ctime`、`idx_session_state`、`idx_state_timeout`；`segment_count`/`gap_count`/`recorded_duration_ms` 三列标注「派生投影，可由切片表 `RefreshStats` 重算，非唯一事实源」 |
| | `live_record_segment` | PK `id`；**`uniq_record_seq(record_id, seq)`**；`idx_record_state_seq`（keyset 翻页）、`idx_room_record`、`idx_registered_at`（按首登时间回收）；**无 `ctime` 列**，`registered_at` 即首登时间且重放不覆盖；`MISSING/CORRUPT` 行的 `bucket/object_key` 为空串（产物不存在的证据形态） |
| `000003_create_live_replay_retention_tables.sql` | `live_replay_task` | PK `replay_id`；**`uniq_request_id`**；`idx_record_range`、`idx_room_state_ctime`、`idx_session_state`、`idx_anchor_state`、`idx_asset`；`asset_id/aid/bvid` 均为**跨服务主键引用，不建外键** |
| | `live_replay_asset_ref` | PK `id`；**`uniq_replay_id` + `uniq_asset_id` + `uniq_aid`**；`review_state`/`published_at`/`review_state_at`/`last_event_id`/`source` 五列属 **video 事实的只读投影**（单向 video→live-media，投影通道是唯一写入者）；`retention_state` 0 正常/1 待回收/2 已回收 |
| | `live_retention_task` | PK `retention_id`；**`uniq_request_id`**；`idx_target_state`、`idx_state_id`、`idx_kind_state`、`idx_room_state`；`scanned/deleted/skipped` 为 Worker 覆盖写证据 |
| | `live_media_outbox` | PK `id`；**`uniq_event_id(event_id CHAR(26))`**；`idx_state_retry(state,next_retry_at,id)`（发布器取到期行）、`idx_aggregate`、`idx_room_ctime`、`idx_state_published`；`state` 0 待发布/1 已发布/2 失败（与 `playback_outbox` 编号一致，**与 `live_ingest_outbox` 的 1/2/3 不同**）；`payload MEDIUMTEXT` 只放契约内字段 |

不建跨库外键（AGENTS.md §5）；`room_id`/`live_session_id`/`template_id`/`asset_id`/`aid`/`bvid`/`anchor_mid`
都只是**引用**，语义由属主服务决定。Redis 只做加速：任务详情 `TaskCacheTTLSeconds`、档位列表
`StreamOutputCacheTTLSeconds`、Worker 领取互斥标记；任何 key 清空都可由本库重建，不读写其它服务的 key。

Outbox 事件类型（`model/live_media_outbox.go`）：`livemedia.transcode.state.changed`、
`livemedia.record.state.changed`、`livemedia.record.stopped`、`livemedia.record.gap.detected`、
`livemedia.stream.output.online`、`livemedia.stream.output.offline`、`livemedia.replay.review.submitted`、
`livemedia.replay.content.state.changed`、`livemedia.retention.finished`。
Topic 由 `common/eventenvelope.Topic()` 逐事件推导（类型 + `.v` + `schema_version`，即
`livemedia.record.stopped.v1` 这样的 9 个 topic，已在 `etc` 的 `PublishTopics` 和
`docs/api-and-events.md` §5 登记），**不是**一个服务共用一个 topic。
事件名只能是「小写字母 / 数字 / 点号」，否则 `eventenvelope.New()` 直接拒绝、整笔业务事务回滚；
这条由 `common/eventenvelope/event_type_gate_test.go` 做全仓门禁。**任何业务写与对应 Outbox 行必须同事务提交**（否则会出现「状态变了、事件丢了」）。

## 4. 状态机

全部定义在 `model/errors.go`，logic 只做「取目标态 → 查迁移表 → 条件 UPDATE」，不允许自由写状态列。

1. **转码**：`PENDING→RUNNING→STOPPING→STOPPED`；`PENDING/RUNNING/STOPPING` 可 `→FAILED`；
   `PENDING/STOPPING→CANCELLED`；`FAILED→PENDING` 只能由 `RetryLiveTranscode` 触发。
   `RUNNING→RUNNING`（心跳）合法；`STOPPED/CANCELLED` 终态。`RUNNING` 不允许直接 `CANCELLED`。
2. **录制**：`PENDING→RECORDING→STOPPING→STOPPED`；`FAILED→PENDING`（重新登记）或
   `FAILED→RECORDING`（断点续录，从 `last_seq+1`）；但**绝不跳过缺口登记**——续录后必须重新 `ReportRecordSegment`。
3. **切片**：正向只有 `UPLOADING→UPLOADED→VERIFIED`，`VERIFIED` 之后不得回退；
   `MISSING→UPLOADING/UPLOADED`（补录到达）但不得直接 `VERIFIED`（必须重新校验）；
   `MISSING/CORRUPT` 判定作用于一切状态（含 `VERIFIED`，对象事后丢失必须能改判）；
   唯 `CORRUPT` 不再改判 `MISSING`；`old==new` 放行以支持 Worker 重复回报的幂等重放。
4. **回放**：`PENDING→MERGING→UPLOADING→REGISTERED→REVIEW_SUBMITTED→COMPLETED`，
   中间态可 `→FAILED`（终态），`PENDING/MERGING` 可 `→CANCELLED`；
   **`→COMPLETED` 的唯一入口是 `ApplyReplayContentState`**（Worker 侧不可达）。
5. **回收**：`PENDING→RUNNING→SUCCEEDED|FAILED|CANCELLED`，三个终态不可回退（迟到的上报只能命中 0 行）。
   配套的引用行生命周期：`retention_state` 正常→待回收→已回收。
   分发档位另有一套两态机：在线→已下线（终态，可被回收任务清理）。

并发控制统一为「条件 UPDATE + `RowsAffected` 判定」，杜绝读-改-写；有 `version` 列的表要求上报方回传
`expected_version`，`version = version + 1` 由 `model.conditionalUpdate` 统一追加（该 helper 同时拒绝
空 SET 与空 WHERE，避免「一条校验漏了就把全表刷新」）。

## 5. 配置 key（`etc/livemedia.v1.yaml`）

| Key | 说明 | 默认 |
|---|---|---|
| `Name` / `ListenOn` / `Etcd.Key` | 服务名 / 监听 / 注册 key | `livemedia.v1.rpc` / `0.0.0.0:8120`（全仓唯一）/ `livemedia.v1.rpc` |
| `CacheRedis` | 业务缓存。**键名不能写 `Redis`**：`zrpc.RpcServerConf` 内嵌同名 `RedisKeyConf`，`conf.Load` 会报 `conflict key redis`，代码可编译但启动即失败（`internal/config/config_load_test.go` 是该回归） | `127.0.0.1:6379`, node |
| `DataSource` | MySQL DSN，库名必须为 `go_video_live_media` | 本地示例；生产从配置中心/Secret 注入 |
| `AssetRPC` / `VideoRPC` / `ModerationRPC` | 下游 zrpc client 配置位（回放全流程与投影读取），`json:",optional"`；本轮 `ServiceContext` 不构造 | etcd key `asset.v1.rpc` / `video.v1.rpc` / `moderation.v1.rpc`，`NonBlock: true` |
| `LiveMedia.DefaultTranscodeTimeoutSeconds` / `DefaultRecordTimeoutSeconds` | 无心跳判超时秒数，写入时固化为 `timeout_at` 快照 | 60 / 90 |
| `LiveMedia.DefaultMaxAttempts` | 转码重试上限，登记时快照进 `max_attempts`（配置变更不回改历史行） | 3 |
| `LiveMedia.DefaultRecordSegmentSeconds` / `MaxRecordSegmentSeconds` | 分片时长默认值与上限（超过夹取） | 10 / 60 |
| `LiveMedia.DefaultSegmentPageSize` / `MaxSegmentPageSize` | `ListRecordSegments` keyset 分页 | 200 / 500 |
| `LiveMedia.MaxReplayGapSegments` | 回放区间允许的缺口数；**默认 0**：宁可拒绝提交，也不产出时间轴断裂的回放 | 0 |
| `LiveMedia.ReplayTitleMaxLength` | 回放标题 rune 上限（透传给 video 前本地夹取） | 80 |
| `LiveMedia.DefaultRetentionBatchLimit` / `MaxRetentionBatchLimit` | 回收批量（超过夹取不报错） | 100 / 500 |
| `LiveMedia.MaxListPageSize` | 列表类方法每页上限，超过 `ErrPsTooLarge` | 50 |
| `LiveMedia.TaskCacheTTLSeconds` / `StreamOutputCacheTTLSeconds` | 任务详情 / 档位列表缓存秒数，0 关闭 | 60 / 15 |
| `LiveMedia.TaskTimeoutSweepIntervalSeconds` / `TaskTimeoutSweepEnabled` | 超时清扫轮询间隔 / 是否启动。**本轮显式 false**：清扫依赖 logic 与 Worker 接线 | 15 / false |
| `Kafka.Enabled` | 是否随进程启动事件链路。**一个开关管两条链路**：Outbox 发布器（`internal/publisher`）与 `live.state.v1` 消费者（`internal/consumer`）同时启停，刻意不提供 `ConsumeEnabled` 这类半开关，否则运维会以为能单独关掉一半。**示例配置钉住 false**：默认构建没链接 kq，置 true 会让 `NewSender`/`NewKqFactory` 返回 `ErrKafkaRuntimeNotBuilt` 并让启动失败 | false |
| `Kafka.Brokers` / `PublishTopics` | 队列地址与投递目标。`PublishTopics` 必须与 model 的 9 个 `EventType*` 派生 topic **集合相等**，缺项与多写都在启动时被 `ValidatePublishKafka` 点名拒绝 | `127.0.0.1:9092` / 9 条 `livemedia.*.v1` |
| `Kafka.MaxRetries` | **共用键**：发布侧达到即置 `state=失败`，消费侧达到即记 `given_up` 日志并提交位点（`internal/consumer.OptionsFrom`）。两侧同源，不出现「发布器还在退避、消费者已放弃」 | 5 |
| `Kafka.RetryBackoffSec` / `RetryMaxBackoffSec` / `PollIntervalSec` / `BatchLimit` / `SendTimeoutSec` | 发布循环参数，逐键映射见 `internal/publisher/params.go`（上限低于基数会被拒） | 5 秒 / 1800 秒 / 2 秒 / 100 / 5 秒 |
| `Kafka.Group` / `SubscribeTopics` | 消费侧参数。`SubscribeTopics` 只能是 `live.state.v1` 一个：订阅本服务的 9 条出站 topic 等于把自己的事件读回来再丢掉（还会和发布循环组成回环），订阅别的 topic 则没有映射，两者都被 `ValidateKafka` 直接拒绝 | `live-media.v1` / `[live.state.v1]` |
| `Kafka.Offset` / `Conns` / `Consumers` / `Processors` / `ForceCommit` | 消费循环参数（口径与 live-room、live-gateway 同名键一致）。`Offset=first` 会重放保留窗口内的旧停播事件，对下线是安全的（CAS 幂等）但只在补账时用；`ForceCommit=true` 等于把「档位没摘下来」变成静默丢事件，只在排障时短期开启 | `last` / 1 / 2 / 4 / false |
| `Kafka.Username` / `Password` / `CaFile` | SASL 与 TLS 参数，**只被消费侧读取**（`kq.NewPusher` 在 go-queue v1.2.2 不暴露注入口，发布侧读到也没有作用）。`CaFile` 必须可读，否则 kq 内部 `log.Fatal` 打死进程，因此 `ValidateKafka` 提前以普通错误拒绝；生产值进 Secret/Vault，绝不写进 etc 示例 | 空 |

## 6. 启动与自检

```bash
cd services/live-media

powershell -File scripts/gen.ps1 -Service live-media                    # 契约变更后重新生成
powershell -File scripts/migrate.ps1 -Action up -Service live-media     # 建库建表（需本地 MySQL，见 §7）
go run ./services/live-media -f services/live-media/etc/livemedia.v1.yaml
```

```bash
go build ./services/live-media/... && go vet ./services/live-media/... && go test ./services/live-media/...
gofmt -l services/live-media    # 必须无输出
```

### 6.1 事件发布（`live_media_outbox` → 9 个 `livemedia.*.v1` topic）

2026-10-04 接线。发布循环本体不在本服务里：顺序、退避、判死、写库失败中断这些与业务无关的决策
都在 `common/outbox`，`internal/publisher/` 只剩三段适配：

| 文件 | 承担什么 |
|---|---|
| `outbox_store.go` | 列 ↔ `outbox.Row` 映射、`RequiredTopics()`（由 model 的 9 个 `EventType*` 派生）、`CheckRow` 的 topic 归属判定、条件 UPDATE 命中 0 行的折叠口径 |
| `params.go` | `Kafka.*` → 发送端参数与循环参数；`ValidatePublishKafka` 对 `PublishTopics` 做**集合相等**校验（缺一条 → 那类事件每轮撞「没有发送通道」直到判死；多一条 → 白占通道） |
| `kafkaruntime_disabled.go`（`!livemedia_kafka`） | 恒返回 `ErrKafkaRuntimeNotBuilt`，并打印「未链接」的启动说明 |
| `kafkaruntime_kafka.go`（`livemedia_kafka`） | 每 topic 一个 `kq.NewPusher(..., kq.WithSyncPush())`：**同步投递，broker 受理才 `MarkPublished`**；未登记 topic 与空分区键在触网前拒发 |

`svc.NewServiceContext` 里 `logx.Must(ctx.startPublisher())`：`Kafka.Enabled=true` 而二进制没链接
发送端时**启动即失败**，不会「安静地不发事件」；`Enabled=false` 是允许的正常姿态，只打印运行时说明。
`Stop()` 与 `proc.AddWrapUpListener` 负责优雅收尾（先停循环，再关通道）。

口径三条，不要读成别的：

- 分区键是行的 `aggregate_id`（任务号），不是 `event_id`：同一任务的顺序事件必须同分区。
- `MarkRetry` 用的是 SQL 侧 `retry_count = retry_count + 1`，忽略引擎传来的计数（单实例两者恒等，
  多副本不会被两个副本各算一次互相覆盖）。代价：本表没有租约列，多副本并发时判死边界可能比
  `Kafka.MaxRetries` 多投几轮（见缺口 6）。
- 条件 UPDATE 命中 0 行记日志而不是报错：该行只可能已被别的副本判定或已被清理，
  冒泡成错误会中断整批，把同批正常行也拖回「本轮没结论」。

```bash
# 带标签的构建/检查/用例（默认构建同样要跑）
go build -tags livemedia_kafka ./services/live-media/...
go vet -tags livemedia_kafka ./services/live-media/...
go test -p 1 -count=1 -tags livemedia_kafka ./services/live-media/internal/publisher/
```

带标签的 `kafkaruntime_kafka_test.go` 只建通道、不调已登记 topic 的 `Send`（`kq.NewPusher` 懒解析地址，
构造与关闭都不产生网络往返），所以它离线可跑，也**不代表能与任何 broker 通信**。
本仓库从未做过 Kafka 实机联调，这 9 个出站 topic 在仓库内还没有任何消费者（见缺口 1 的 ① 与
`docs/roadmap.md` A 组；本服务自己新接的消费者只读入站的 `live.state.v1`，见 §6.2）。

用例清单（静态口径 = `grep -cE '^func Test'` / `grep -c 't.Run('`；动态口径 = `-v` 的 `--- PASS` 行数，
表驱动用例按展开计数。`kafkaruntime_disabled_test.go` 与 `kafkaruntime_kafka_test.go` 由构建标签互斥，
所以两个构建的总数不是同一份文件集合）：

| 文件 | 生效构建 | 静态 顶层/子 | 动态 顶层/子 | 钉住了什么 |
|---|---|---|---|---|
| `outbox_store_test.go` | 两者 | 13/5 | 13/23 | 9 个 topic 的字面量与顺序（跨服务契约，改名等于换掉所有订阅方）；`EventType*` 常量与 `eventTypes` 逐条双向对齐（漏登记不是编译错误，而是那类事件每行被直接判死）；列 → `Row` 映射与 9 类缺陷的不可发布原因逐条点名；`now/limit` 原样透传（在这层吞参数等于提前或超量投递）；`MarkRetry` 的 `publishedAt/nextRetryAt/lastError` 不错位、引擎次数不落到时间戳列；0 行折叠为日志、真写库失败点名表名；端到端 `RunOnce` 只走 `ListPending` + 一个状态方法（`Insert` 属 logic 的事务权限）；投递失败落 `MarkRetry` 而不是写「已发布」；同批内「0 行让位」跑完 3 行、「写库失败」在第 2 行中断并把剩余留给下一轮 |
| `params_test.go` | 两者 | 7/1 | 7/14 | `SenderSettingsFrom` 去空白去重且按声明顺序、`Brokers` 是复制不是底层切片；`OptionsFrom` 六个旋钮逐键映射且 `Name` 必须是本服务标签；`ValidatePublishKafka` 对 14 种不合规配置逐键点名（少一个 topic、多一个不产出的 topic、同一外来 topic 写两遍只点一次、上限小于基数…），错误同时带 `live-media` 前缀；空 `PublishTopics` 一次列全 9 个且顺序稳定；五个键同时坏时一次报全（分号分隔）；`NewPublisher` 四条出口与参数透传、构造不自行启动循环；`Kafka.MaxRetries` 超 int32 绕成负数时配置层放行、引擎拦住且错误同时点名 `Options.MaxAttempts` 与配置键 |
| `example_yaml_test.go` | 两者 | 3/0 | 3/0 | 用真实 `conf.Load` 读 `etc/livemedia.v1.yaml`：`Enabled` 必须保持 false 而其余键全部合格（「只差一次翻转」）；`PublishTopics` 与 model 派生的 9 个 topic **逐个同序**；`Brokers`/`Group`/`SubscribeTopics` 三个预留位也钉住（改了就是契约二次漂移）；六个旋钮字面量与 yaml 注释同源；用示例配置装配并投递一次（分区键落在 `aggregate_id`、`SendTimeoutSec=5` 真的进 ctx、位点落在调用前后区间）；补 `Kafka` 段不得把 `Name`/`DataSource`/`CacheRedis`/`LiveMedia` 改坏 |
| `kafkaruntime_disabled_test.go` | `!livemedia_kafka` | 2/0 | 2/0 | 默认构建即使参数全对也拿不到发送端：返回 `ErrKafkaRuntimeNotBuilt`、`sender` 恒 nil、错误文本给可执行下一步（`livemedia_kafka`/`broker`/`Kafka.Enabled` 三个词都在）；启动说明点名积压表 `live_media_outbox` 与「事件留在库里」的后果；`Enabled=true` 也只会说「未链接」，不许出现「已链接/已在投递」 |
| `kafkaruntime_kafka_test.go` | `livemedia_kafka` | 4/1 | 4/4 | 带标签构建能逐 topic 建通道（9 个翻倍只建 9 条，漏建一个等于那类事件全判死）、`Close` 幂等且之后 topic 列表清空；四类不完整参数在**建通道之前**被拒且不返回半截发送端；未登记 topic 与空分区键在**触网之前**被拒（不假装投递成功）；`RuntimeNotes` 区分 `Enabled` 两侧并逐条枚举 9 个 topic，同时承认从未联调、下游没有消费者 |
| **合计** | 默认构建 | 25/6 | 25/37 | |
|  | 带标签构建 | 27/7 | 27/41 | |

`common/outbox` 那份循环（顺序、退避曲线、判死边界、启停与并发、`CheckRow` 判据）的用例不在本包，
见 [common/outbox/README.md](../../common/outbox/README.md) 的「测试覆盖」；本包只钉「换成 live-media 的表与配置之后
结论仍然正确」，不重复钉引擎行为。

### 6.2 事件消费（`live.state.v1` → 本场次档位整场下线）

2026-10-04 接线。链路只有一条（`docs/api-and-events.md` §5）：

```text
live-ingest 停播（stream_state=Stopped）→ live.state.v1
  → consumer.Handler.Consume → logic.OfflineSessionOutputs
  → live_stream_output 该场次全部在线档位置为已下线 + 逐档位登记 livemedia.stream.output.offline
```

生产者只有房间和场次，没有档位维度，所以这里的动作是**整场摘除**而不是逐档位判定。
写入用的能力与运营手工下线完全相同（`StreamOutputs.MarkOfflineTx` + `appendOutboxEvent` 同一事务），
因此 broker 的「至少一次」投递不会把档位下线两次：`MarkOfflineTx` 的 WHERE 带 `state=在线`，
第二次扫不到行、`Affected=0`。

| 文件 | 承担什么 |
|---|---|
| `mapping.go` | `live.state.v1` 的信封 → `OfflineCommand` 翻译；`SupportedTopic` 由 `eventenvelope.Topic` 推导；`StreamStatePayload` 逐字段对齐生产者的 `stateEventPayload`（只声明用到的字段，上游误投敏感字段不会进本服务日志）；`Interpret` 的判定顺序（先契约校验后决策）与「错误 ⇒ 事件为 nil」不变式；下线原因固定 `model.ReasonSourceLost` |
| `handler.go` | 三条判定线：本包能判定的契约违反不送进 logic；`Affected=0` 是成功结论（位点照常前进）；依赖故障不提交位点但按 `event_id` 在本进程封顶。`Outcome.Commit()`、`Stats` 五类计数、重试台账与容量逐出都在这里 |
| `queue.go` | `QueueFactory`/`MessageQueue` 抽象（Kafka 客户端只出现在一个文件里）、`ValidateKafka` 逐键校验、`EffectiveTopics`、`SettingsFrom`、`Supervisor` 的启停与半途失败回滚 |
| `wiring.go` | 把 `Applicator` 接到 `logic.OfflineSessionOutputsLogic`（每条消息新建 logic 实例，避免日志串到第一条消息的 trace）、`Start` 的三种结果（关闭 / 启动 / 参数不完整即失败） |
| `kafkaruntime_disabled.go`（`!livemedia_kafka`） | 恒返回 `ErrKafkaRuntimeNotBuilt`，`RuntimeNotes` 打印「未链接」与「关开关的后果」 |
| `kafkaruntime_kafka.go`（`livemedia_kafka`） | `kq.NewQueue` 逐 topic 建消费者；`ServiceConf` 显式带上 Name/Log/Mode（留空会被 kq 用默认值重设整个进程的全局 logger） |

口径四条，不要读成别的：

- **没有消费位点表 / inbox 表**（与 inbox、search-indexer、notification 的差别）：幂等由行本身的
  `state` CAS 条件承担，乱序由 `live_session_id` 维度隔离（迟到的上一场 `Stopped` 只命中上一场的档位行）。
  代价是失败事件没有持久化死信，见缺口 3。
- **达到尝试上限后提交位点**：`given_up` 的错误日志是「这条断流事件被丢掉」的唯一证据，
  此后本场档位一直挂着，直到 `online_expire_at` 到点被 `MarkExpiredOffline` 收掉（登记时不带有效期的
  档位只能靠运营逐档位 `OfflineStreamOutput`）。人工恢复入口就是 `ListStreamOutputs` + `OfflineStreamOutput`。
- **同一个 `Kafka.Enabled` 与同一个构建标签管两条链路**：本服务的发布循环与消费循环要么一起可用、
  要么一起拒绝启动，不会出现「档位下线事件发得出去、断流事件收不进来」的半接线进程。
  排查「档位不自动下线」先看这个开关（`consumer.Start` 与 `RuntimeNotes` 都会打印结论）。
- **`Kafka.MaxRetries` 是一个键、两个环**：发布侧的判死上限与消费侧的进程内尝试上限同源，
  运维调它就同时收紧两侧容忍度。

```bash
# 默认构建与带标签构建都必须跑（两个带标签测试文件互斥，见下面表格的「生效构建」列）
go test -p 1 -count=1 ./services/live-media/internal/consumer/
go build -tags livemedia_kafka ./services/live-media/...
go vet -tags livemedia_kafka ./services/live-media/...
go test -p 1 -count=1 -tags livemedia_kafka ./services/live-media/internal/consumer/
```

用例清单（静态口径 = `grep -cE '^func Test'` / `grep -c 't.Run('`；动态口径 = `-v` 的 `--- PASS` 行数，
表驱动用例按展开计数。全程不连 broker、不连 MySQL：`QueueFactory` 与 `Applicator` 都是包内替身）：

| 文件 | 生效构建 | 静态 顶层/子 | 动态 顶层/子 | 钉住了什么 |
|---|---|---|---|---|
| `mapping_test.go` | 两者 | 12/6 | 12/29 | `SupportedTopic` 必须等于 `live.state.v1`（改版本号会让订阅串与实际 topic 静默错位，表现是「消费者在跑、一条也收不到」）；用**生产者源码里逐字节抄下来的消息**跑一遍翻译（房间 32 / 场次 77 / 停播）；三种非 `Stopped` 状态各自为什么不动作；`Interpret` 的 18 条契约违反逐条断「错误哨兵 + `DecisionNone` + 事件为 nil + 文案点名那个字段」；信封层与业务层两道门禁分开测（`ParseEnvelope` 拒 5 种坏信封，`handEnv` 绕过 marshal 校验才能测到第二道）；`event_id` 长度边界按 rune 不按字节；`trace_id` 的回退次序（payload 优先、空白才取信封）；`session_id=1` 这种最小值必须放行（把它当缺省会让真实事件被丢掉）；正则读回 live-ingest 的 payload 与状态枚举逐取值比对，任一侧重排即红 |
| `handler_test.go` | 两者 | 13/3 | 13/16 | 位点能不能前进的判定：`Applied`/`Noop` 提交、契约非法提交且**logic 调用次数为 0**（11 种非法逐个测，白烧去重键的代价在这里能直接证明）、依赖故障返回原错误且不提交、到上限才放弃并留下 `LastError`；成功一次要清空台账（否则历史失败会把下一次真故障直接判死）；`(nil, nil)` 按故障处理；台账容量逐出只影响封顶不影响正确性；并发投递的计数精确等于投递数（本机无 cgo，跑不了 `-race`，用总量断言兜住）；`Outcome` 的 `Commit()` 真值表与字符串标签；`OptionsFrom` 把 `Kafka.MaxRetries` 接到尝试上限、缺省归一化为 5 |
| `queue_test.go` | 两者 | 8/1 | 8/15 | `ValidateKafka` 对 15 种不完整配置逐键点名（订阅自己的出站 topic = 回环、订阅没有映射的 topic、`brokers` 里只有空串也算空）；`CaFile` 不可读必须以普通 error 提前拒绝（kq 读到会 `log.Fatal` 让进程无声消失），且错误里绝不回显口令与用户名；`EffectiveTopics` 去空白去重保序；`SettingsFrom` 透传 Name/Log/Mode；`Supervisor` 的生命周期（重复 `Start` 拒绝、`Stop` 幂等、传给工厂的 Settings 与 `SettingsFrom` 全等、Handler 是同一个指针）；多 topic 半途失败逆序回滚且每条队列只关一次；工厂返回 `(nil, nil)` 不得当成已启动 |
| `example_yaml_test.go` | 两者 | 2/0 | 2/0 | 用真实 `conf.Load` 读 `etc/livemedia.v1.yaml`：`Enabled` 保持 false 而其余消费键全部合格（「只差一次翻转」是真的）；`Offset/Conns/Consumers/Processors/ForceCommit/MaxRetries` 六个字面量与 README 配置表、etc 注释同源；SASL/TLS 三键必须留空（模板里写死会被抄进生产）；用示例配置 + 假工厂装配并启动，断实际订阅 topic、尝试上限来自 `Kafka.MaxRetries`、日志身份与主服务一致 |
| `wiring_test.go` | 两者 | 3/0 | 3/0 | `Enabled=false` 时 `Start` 返回 `(nil, nil)` 且不碰任何工厂（本进程不消费是允许的正常姿态，但不能静默）；`Enabled=true` 而 `ServiceContext` 为 nil 时错误点名 ServiceContext；`NewSvcApplicator` 恒可装配（字段映射本身不在这里断言，logic 入参口径由 `internal/logic/session_offline_test.go` 负责，这里重复断言只会得到永真结论） |
| `kafkaruntime_disabled_test.go` | `!livemedia_kafka` | 3/0 | 3/0 | 默认构建即使参数全对也拿不到消费者：`NewKqFactory` 返回 `ErrKafkaRuntimeNotBuilt`；`RuntimeNotes` 说清「关掉开关后档位只能靠 RPC 与到期清扫下线」；`Start` 在真配置下以错误终止而不是带着「以为在消费」的进程对外服务 |
| `kafkaruntime_kafka_test.go` | `livemedia_kafka` | 3/0 | 3/0 | 带标签构建工厂可用、逐 topic 建消费者且 `Handler` 确实满足 `kq.ConsumeHandler`（编译期断言 `var _ kq.ConsumeHandler = (*Handler)(nil)`）；缺 handler / 缺 topic 在触网前被拒；`RuntimeNotes` 承认「`kq.NewQueue` 不做网络握手，位点语义必须联调后才算成立」 |
| **合计** | 默认构建 | 41/10 | 41/60 | 两个构建的顶层/子用例数相同（`disabled` 与 `kafka` 两个文件互为 3/0 替换），但**不是同一份文件集合** |
|  | 带标签构建 | 41/10 | 41/60 | 0 条 FAIL、0 条 SKIP（2026-10-04 实测口径） |

## 7. 当前阶段与测试覆盖

**当前阶段 = 逻辑轮、logic 单测轮与事件双向接线（发布 + 消费）均已落地（logic 轮 2026-09-22，
发布器轮与消费者轮 2026-10-04）**：

- `internal/logic/` 下 28 个 RPC 方法**全部实现**，不再有 `model.ErrNotImplemented`，
  也不再有 goctl 的 `// todo: add your logic here` 或伪造成功的空 Reply。
  读侧（11 个）只投影 + 夹取分页，判据在 model；写侧（含 Stop/Cancel/Offline/Submit/Report 共 17 个）
  一律「条件 UPDATE 推进合法迁移 + 同事务 Outbox」，依赖为 nil 时显式报错而不是静默降级。
  每个文件保留原「将来行为」次序注释（现已是「已实现行为」），改判定链时按该次序复核。
- 第 29 个 logic 类型 `OfflineSessionOutputs`（2026-10-04 新增）**不是 RPC 方法**：
  它由 `live.state.v1` 消费者调用（见 §6.2），入参是包内的 `OfflineSessionOutputsInput` 而不是
  `rpc.*Req`，因为没有终端会按场次批量摘档位。
- `internal/logic/` 单测已覆盖**全部 29 个 logic 类型**（探针 `gaps:` 为空，即 `29/29`），
  12 个测试文件、静态 `251` 条顶层用例 + `53` 条 `t.Run`（`-v` 展开为 `251/252`），
  全部不连 MySQL/Redis/Kafka。逐文件清单、替身口径与覆盖边界见下面的 §7.1–§7.5。
- `model/` 非测试文件 10 个（8 张表各一个 + `errors.go` + `now.go`）完整可用：状态机、条件 UPDATE 构造、
  keyset 分页、投影重算、时钟注入（`SetClock`）、MySQL 1062 识别都在。
- 事件发布侧（2026-10-04）：`internal/publisher`（5 个文件）把 `live_media_outbox` 的待发行同步投到
  9 个 `livemedia.*.v1`，循环复用 `common/outbox`、本服务只剩三段适配，启停在
  `internal/svc/servicecontext.go` 的 `startPublisher()`；同一轮把 `model/live_media_outbox.go`
  的 `ListPending` 从 `state IN (待发布, 失败)` 改成 `state = 待发布`（判死行不再每轮被重投，
  积压可见性交给候选集刻意不同的 `CountPending`），并新增 `model/live_media_outbox_sql_test.go`
  钉住取行与状态 SQL 的文本。口径、命令与已知限制见 §6.1 与缺口 1、6。
- 事件消费侧（2026-10-04）：`internal/consumer`（6 个文件）把 `live.state.v1` 的停播事件接成
  「本场次在线档位整场下线」，与发布侧共用同一个 `Kafka.Enabled` 和同一个 `-tags livemedia_kafka`，
  启停在入口 `livemedia.v1.go` 里由 `consumer.Start()` 返回 `*Supervisor` 并 `defer Stop()`；
  同一轮新增 `model/live_stream_output_sql_test.go`（钉 `ListOnlineBySession` 的 SQL 与实参顺序）与
  `internal/logic/session_offline_test.go`。口径、命令与已知限制见 §6.2 与缺口 3。
- `deploy/migrations/live-media/*.sql` **已在隔离实例执行并复验**：`127.0.0.1:3399`
  （数据目录 `.gotmp/mysql-data`）上库 `go_video_live_media` 的 8 张业务表与
  `deploy/migrations/live-media/` 三个文件的 `CREATE TABLE` 逐一对上（2026-09-21），
  `deploy/migrations/README.md` 记 `live-media | go_video_live_media | 3 | applied`。
  列名侧另有 model `db` tag ↔ DDL 的逐表比对：`live_transcode_task` 24、`live_stream_output` 22、
  `live_record_task` 26、`live_record_segment` 17、`live_replay_task` 29、`live_replay_asset_ref` 25、
  `live_retention_task` 21、`live_media_outbox` 17（切片表 model 侧另有 9 个 `SegmentStats`
  查询别名字段，不是表列）。**真实/共享实例仍未执行**：本机 `127.0.0.1:3306` 是维护者真实库，
  全程禁止写入，上线仍须由运维在目标实例执行并核对 `schema_migrations`。

### 7.1 logic 用例清单（`internal/logic`，12 个文件，静态 `251/53`、`-v` 动态 `251/252`）

数字由 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`；
动态列取 2026-09-22 之后各轮 `-v` 的 `--- PASS` 行数（表驱动用例按展开计数），0 条 FAIL、0 条 SKIP。
按域分四组：

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **转码与录制生命周期** | | | |
| `transcode_lifecycle_test.go` | 42 | 13 | 六个方法（Stop/Report/Retry/Cancel/Get/List）各自钉：非法迁移零副作用（不写行、不写事件、不 `++version`）；幂等口径以**状态**为准而不是以 `request_id` 为准（`request_id` 是登记幂等键，状态推进不得覆写）；返回体必须等于提交后的行；重试到 `max_attempts` 上限、心跳不占事务、`err_msg` 脱敏、越界 `progress` 夹取、深分页拒绝、`task_id DESC` 次序、漏读回读时绝不回空 Reply |
| `record_lifecycle_test.go` | 21 | 4 | Start/Stop 录制：`request_id` 重放不产生第二行、同场次复用进行中的任务不双录、非法状态迁移零副作用、业务写与 Outbox 同事务（事件写失败连任务行一起回滚）；INSERT 撞键回读既有行而不是伪造成功；跨房间的 source task 拒绝；`operator` 脱敏与超长截断各有独立用例 |
| `record_progress_test.go` | 17 | 3 | 断点续录可信的四条：`last_seq` 只前进（迟到的旧上报必须拒）；计数列一律由切片表重算而不是按上报值累加；终态不可复活且同态重放零写零事件；高频心跳不占事务、不发事件（否则 Outbox 被采样淹掉）；另有「缺口读在事务外」与 `RefreshStatsTx` 失败回滚 |
| `record_read_test.go` | 26 | 6 | 写侧只有 `ReportRecordSegment`（切片只增不减、`uniq_record_seq` 的 INSERT IGNORE 0 行必须回读区分「已是目标态」与「状态机不允许」；缺口显式补 `MISSING` 行且 bucket/object_key/checksum 必须为空串；逐片登记不得 `++version`；父任务终态拒登但 FAILED 可续录）；三个读方法以 `wantNoWrites` + `wantEvents(nil)` 收尾——读接口一旦顺手重算写回就等于给只读账号开写路径并撞废 Worker 的 version |
| **回放链路** | | | |
| `replay_lifecycle_test.go` | 28 | 0 | 四条防线：`SubmitReplayTask` 裁决录制能否拼回放（必须 STOPPED，缺口量由 `allow_gaps` 与 `maxGapSegments` 共同决定）；`ReportReplayProgress` 的三个状态永不上报（PENDING/CANCELLED/COMPLETED 归属别处），越界失败关闭；幂等口径四方法各不同（天然键/expected_version/`last_event_id`+`review_state_at` 单调）；事件词表只有两条，`SubmitReplayTask`/`BindReplayAsset` 每例都断 `wantEvents(nil)`；`TestReplayMethodsNeverTouchNilClients` 钉住「下游三个 zrpc client 恒 nil 时照常工作，需要下游事实的动作回显式哨兵而不伪造 asset_id/aid」 |
| `replay_bind_test.go` | 27 | 0 | 两个跨服务边界写方法守的是**写权限清单**而不是自己的状态机：`BindReplayAsset` 一条事件都不发、不越权推进回放任务状态（patch 带刚读到的原状态、只回填主键列）、房间/场次/录制主键/区间/缺口一律取自任务行（跨房间绑定在结构上不可表达）；`ApplyReplayContentState` 只能改五个投影列加两条被契约允许的联动；引用 UPSERT 与任务回填同事务，一半失败必整体回滚 |
| `replay_read_test.go` | 15 | 1 | 三个读方法只读；查无此行必须 `ErrReplayTaskNotFound` 而不是零值 Info（否则 Worker 把 version=0 当 expected_version 后永远撞冲突）；缓存只对终态开（`taskDetailTTL` 纯函数 + 本包 Cache 恒 nil 的「每次必回源」两条一起构成证据）；归一后的 pn/ps/过滤值只能靠 `db.lastReplayList`/`lastReplayRefList` 钉，并钉住「rpc 未暴露的 RecordId/AnchorMid 恒传 0、OnlyUnsynced 恒 false」；`LiveReplayTaskInfo` 无 `review_state` 用反射钉成结构事实 |
| **档位与回收** | | | |
| `stream_output_test.go` | 25 | 9 | 无 version 列的三方法：幂等只来自天然键 `uniq_output_natural`，同键重登记复用同一行，而 `request_id` **没有**唯一索引、绝不允许被当幂等键（否则第二次「重新上线」被静默吞掉）；`state` 本身是 CAS 条件，命中 0 行要回读归因且「同一次下线只留一份证据」；响应必须等于提交后回读的行而不是入参回显；跨房间 output_id、歧义天然键、竞态到未知状态各自失败关闭；事件 payload 不含 bucket/object_key/cdn |
| `session_offline_test.go` | 10 | 3（动态 12） | 断流事件驱动的整场下线（2026-10-04 随消费者接线新增），钉它与运营手工 `OfflineStreamOutput` 的三个差别：一次调用覆盖**多个**档位且每档各登记一条事件（下游按 `output_id` 收敛投影）；全部档位的「条件下线 + 事件」在同一事务，一半成功一半失败必须整体回滚（否则出现「源已断但档位仍可播」的中间态，比整场不动更难查）；幂等不靠事件表而靠 `MarkOfflineTx` 的 `state=在线` CAS，同事件重投第二次 `Affected=0` 是**成功**结论（消费者据此提交位点）。另有校验门禁表 10 行逐字段点名（房间/场次/原因/事件 ID 各自为什么不能放行）、`event_id` 恰好取列宽上限时必须放行（少一字符就是把边界写死）、竞态档位（读完被别人改成已下线）跳过而不炸整批、`Scanned` 溢出 `MaxSessionOutputs` 失败关闭而不是截断、`trace_id` 只截自己不截事件引用 |
| `retention_test.go` | 37 | 14 | 全服务唯一「授权删对象存储」的入口，主轴是三条防线：`SubmitRetentionTask` 一个对象都不删、审计字段缺失或超列宽一律拒绝而不是截断保存（截断后的归因是另一个人）、定点回收必须回读对象并校验房间归属与当前状态、批量任务准入交给 Worker 逐行判定、三层幂等都必须「什么都不写」；`ReportRetentionResult` 的计数是证据不是增量，三条自洽判据任一不满足整条拒收（含 `purge=false` 时 `deleted` 必须为 0）、必须先认领再报成功、终态只允许同值重放；List/Get 只读；`SubmitRetentionTask` 侧每例断 `wantEvents(nil)`（词表只有 `livemedia.retention.finished`） |
| **信封契约与替身** | | | |
| `outbox_contract_test.go` | 3 | 0 | 两个已修生产缺陷的回归门禁：9 个 `EventType*` 常量逐个过 `common/eventenvelope` 语法契约（缺陷 #1，曾让每个带事件的写事务恒回滚）；`StartLiveRecord`/`StartLiveTranscode` 必须返回自己提交的那一行（缺陷 #2，曾丢弃 `InsertTx` 主键，把事件写成 `aggregate_id="0"` 并对已成功登记的任务报 NotFound） |
| `fakes_test.go` | 0 | 0 | 内存版 model 与假事务，见 §7.4 |

写用例时发现的两个实现缺陷（#6 批量回收被定点准入读挡死、#7 成功上报被 `RetentionPatch` 的 nil
语义反向留痕）已在被测代码内修掉并留了回归护栏：
`TestSubmitRetentionTaskRegistersBatchSweepWithoutReadingObjects` 钉「一条定位读都不许发生」，
`TestReportRetentionResultClaimThenSucceededClearsTrace` 钉成功边的 errno/err_msg/fail_reason 三列清零；
后者做过变异验证（把成功边重新置 nil，用例即红且指向 errno/err_msg 两列）。

### 7.2 其他层

- `model`（3 个测试文件；静态 `32/7`，`-v` 动态 `32/13`）：`live_media_model_test.go`（静态 `15/0`）只测不连库的纯逻辑（五个状态机 +
  引用行生命周期的正例与**回退/越级/终态改写**反例；切片「前置状态集合」与迁移表一致性
  （防止 `MISSING` 跳过校验直接进拼接）；`clampPage` 边界（0/负数/超上限）；`buildWhere` 永不退化成全表；
  `stateInFragment` 占位符计数；`conditionalUpdate` 拒绝空 SET/空 WHERE；入参守卫在触库前返回
  （`conn=nil` 构造，误用即 panic）；`SetClock` 驱动的超时判定；`isDuplicateErr`；
  `ErrNotImplemented` 与其它哨兵不混淆。
  `live_media_outbox_sql_test.go`（静态 `13/1`、动态 `13/4`，2026-10-04 随发布器接线新增）与
  `live_stream_output_sql_test.go`（静态 `4/6`、动态 `4/9`，同日随消费者接线新增）用**记录型假
  `sqlx.SqlConn`**（不 import 驱动、不连库，手法先例是 `services/coin/model/model_rules_test.go`）
  钉住**发给驱动的语句文本与实参顺序本身**，因为 logic 单测的内存替身只能证明「拿到这些行怎么裁决」，
  证明不了「发给驱动的是哪条语句」。
  Outbox 侧：`ListPending` 只取 `state = 待发布` 且
  `next_retry_at` 到期（判死行不得被每轮重投，这是本轮修掉的缺陷）、`limit <= 0` 夹取与
  `ErrNoRows → (nil, nil)`、`CountPending` 的候选集**刻意包含**失败态（运维要看得见积压）、
  三个 `Mark*` 的 `state` 守卫与位点列（`published_at` 独立列 vs 复用 `mtime` 的 playback 不同）、
  `retry_count = retry_count + 1` 写在 SQL 侧、`MarkPublished` 命中 0 行返回 `0` 而不是错误、
  驱动错误逐条冒泡并点名表名、`trimLastError` 的 512 上限**不切断 UTF-8**、
  `Insert` 的四条必填守卫（`4` 个子用例）与列序↔实参逐位对应、1062 归一成 `ErrRequestIdDuplicated`。
  档位侧三条只有钉 SQL 才能红的用例：`ListOnlineBySession` 的 `room_id`/`live_session_id`
  实参顺序写反时查询照样成功但会摘掉**别房**的档位；`LIMIT` 写成 `MaxSessionOutputs` 而不是 `+1`
  时溢出永远判不出来；扫空与驱动报错必须分别返回 `(nil, nil)` 与 error（混同等于把 DB 故障说成「这场没档位」）。

- `internal/config`（1 文件 `1/1`）：`config_load_test.go` 用 `conf.Load` 真实加载 `etc/*.yaml`
  并显式调用 `Validate()`（`CacheRedis.Host` 非空 → 覆盖 `conflict key redis` 这类启动级回归；
  `DataSource` 必须含 `go_video_live_media`；`LiveMedia.*` 上限自洽：默认值 ≤ 上限、超时秒数为正、
  `TaskTimeoutSweepEnabled` 在契约轮必须为 false）。`ListenOn` 全仓唯一性由人工核对（当前 `0.0.0.0:8120`）。
- `internal/repository`、`internal/svc`：**无离线单测**，且本服务**没有** `internal/repository` 目录
  （见已知缺口 2）；`internal/svc` 里三个可选 zrpc client 的构造只有间接断言
  （`TestReplayMethodsNeverTouchNilClients` 证明 logic 不调它们）。
- `internal/publisher`（5 个文件；静态口径：默认构建 `25/6`、`-tags livemedia_kafka` 构建 `27/7`，
  两个带标签文件互斥；`-v` 动态数与逐文件清单见 §6.1 末尾）：
  它是 `common/outbox` 的第二个使用方，用例只钉适配层（列映射、topic 归属、逐键配置校验、
  0 行折叠、示例配置装配），循环本体（顺序/退避/判死/启停）由 `common/outbox` 的用例负责。
- `internal/consumer`（6 个非测试文件 + 7 个测试文件；静态 `41/10`、动态 `41/60`，
  默认构建与带标签构建**数字相同但文件集合不同**，逐文件清单见 §6.2 末尾）：
  只承担 `live.state.v1` 一条链路，替身是包内的 `fakeFactory`/`fakeQueue`/`fakeApplicator`，
  全程不建 broker 连接、不连 MySQL。它证明不了位点语义（见缺口 3）。
- 本服务没有 `internal/policy` 目录：事件双向（发布 + 消费）本轮已接线（§6.1、§6.2），
  还缺的是超时清扫与回收执行器背后的策略层（见缺口 4、8）。
- 没有 `model/migration_parity_test.go`：迁移 ↔ model 的自动列对账仍是人工比对（已知缺口 12）。

### 7.3 构造器级覆盖

`29/29`：探针取 `internal/logic` 全部 `New*Logic(`（29 个 = 28 个 RPC 方法 + 非 RPC 的
`NewOfflineSessionOutputsLogic`，后者在 `session_offline_test.go:72` 等处直接调用），
逐个在 `*_test.go` 里查引用，`gaps:` 为空。

### 7.4 替身层与断言口径

替身在 `internal/logic/fakes_test.go`（内存版 model + 假事务），注入不碰驱动：
`svc.ServiceContext` 的八个 model 字段是接口类型，测试直接赋值。

- 复刻的**语义**（不是 SQL）：唯一键命中即返回包装 `ErrRequestIdDuplicated` 的错误；
  `INSERT IGNORE` 命中即 0 行；CAS（`state IN (...)` + `version=expected`）不命中即 0 行；
  `last_seq` 用 GREATEST 只前进；终态行的 patch 不再推进；「没有这行」返回 `(nil, nil)`
  而「读不动这行」返回错误。fake 逐条对齐 model 的真实实现——`InsertTx` 只**返回**自增主键、
  不回填结构体字段（`live_record_task.go:181`、`live_transcode_task.go:180`），
  录制登记 SQL 里 `last_seq/segment_count/gap_count/recorded_duration_ms` 与 `version` 是写死的字面量
  （`live_record_task.go:167`）；fake 若「好心」回填就会把生产缺陷掩盖掉。
- `TransactCtx` 在入口快照内存态、回调报错时整体回滚，所以能真断「事务中途失败不留半成品行、
  不留下半条事件」；但调用计数（`calls`）不参与回滚——它记录「发生过几次尝试」，
  因此副作用断言一律「计数增量 + 数据状态」两条一起看。
- 每个 fake 只内嵌接口并覆写被测路径用到的方法，其余方法由内嵌的 nil 接口提升：
  测试一旦走到未实现的方法当场 panic（响的失败）。
- 共同断言口径：入参门禁发生在任何读之前（`assertNo*Reads`）；失败路径零副作用
  （`snapshotWrites`/`wantNoWrites`/`firedWrites` 钉「副作用集合恰好是允许的那几个」）；
  凭据不入库不入事件（`wantNoLeak`）；交错（读完快照后行被别人改掉）一律用 `db.onHit` 构造，
  断言的是**归因结论 + 没有半成品**，而不是交错后的最终库态。
- 本包 `Cache` 恒为 nil，读侧因此必然走「缓存缺失 → 回源主表」那条分支（与线上未配置 Redis 时一致）。
- 它证明不了什么：真实 SQL 文本与拼写（含 GREATEST）、索引是否真的存在、
  `INSERT IGNORE` 是否按 `uniq_record_seq` 去重、事务隔离级别与行锁、
  `ORDER BY` 在并列值上的实际次序、驱动返回的 matched vs changed rows——
  这些由 `model/live_media_model_test.go` 的纯逻辑与迁移 SQL 负责，不在 logic 单测里假装覆盖。

消费者侧（`internal/consumer/*_test.go`）另有一组替身，同样是「内存态 + 记录调用」：

- `fakeApplicator` 按脚本应答并**记录每一次入参**，所以「契约非法的事件绝不进 logic」
  是 `calls == 0` 直接证明的，不是靠读注释相信；脚本用尽后复用最后一条，
  因此「重投第 N 次」这类用例不必重复灌脚本。
- `fakeFactory` / `fakeQueue` 只记 `Start`/`Stop` 次数并把动作写进共享的 `order`/`done` 序列，
  用来断**回滚与关闭次序**（半途失败要逆序关掉已建好的、每条队列只能被关闭一次）。
  多 topic 的逐条回滚分支只能靠包内手工装配 `Supervisor` 覆盖，因为真实配置经
  `ValidateKafka` 只允许一个 topic。
- `recordingSQL`（`model/live_stream_output_sql_test.go:27`）只实现档位表读侧用到的
  `QueryRowsCtx`，其余方法一律 panic：「用例其实什么都没断言」必须炸，不能安静通过。
- 三个替身都**不含任何 broker 行为**：不验证分区分配、位点提交、重平衡与 `ForceCommit`
  在真实客户端里的效果（缺口 3）。

### 7.5 覆盖边界

- 用例不连接 MySQL/Redis/Kafka/etcd/Elasticsearch/对象存储，也不起 gRPC 服务端；
  下游 Asset/Video/Moderation 三个 zrpc client 在本包恒为 nil。
- **本服务当前 0 条用例处于 skip**（主代理实测口径）。需要知道的是：`fakes_test.go:132-150`
  保留一个「若缺陷 #1 复发则 `t.Skipf`」的条件闸门 `requireNoEnvelopeBug`，
  缺陷 #1 已于 2026-09-22 修掉，该分支目前不会触发；`record_lifecycle_test.go:18` 与
  `record_progress_test.go:20` 的头注释仍在说「会以 Skip 出现」，那是缺陷修复前的描述。
  不要把用例文件数当成断言都在跑。**2026-10-03 整树 `go test -p 1 -count=1 -v` 逐包普查确认**：
  本包 `--- SKIP` 行数 0、`--- FAIL` 行数 0，`requireNoEnvelopeBug` 那条条件闸门确实没有触发。
  **2026-10-04 消费者接线后重新普查**（`go test -p 1 -count=1 -v ./services/live-media/...`）：
  5 个包全 ok，合计 `350` 条顶层 + `363` 条子用例，`--- FAIL` 与 `--- SKIP` 均为 0。
- 迁移 SQL 与真实库的列级对账只在隔离实例 `127.0.0.1:3399`（库 `go_video_live_media`，8 张业务表 ↔
  3 个迁移文件，2026-09-21）复验过，见上面第 4 个 bullet；真实/共享实例仍未执行。
  逐表列数比对（`live_transcode_task` 24 列等）是人工记录，不是自动门禁（已知缺口 12）。
- Outbox 的**投递循环本轮已接线，但没有 broker 证据**：`internal/publisher` 的用例把「取哪些行、
  投到哪个 topic、失败如何写回状态」钉在内存替身与记录型假连接上（§6.1），而「事件真的送达队列」
  「同步 `Send` 的失败语义确实如假设」不在离线覆盖内。同样不在覆盖内的还有超时清扫与回收执行器
  （已知缺口 4/8），所以「超时任务真的被清扫」「对象真的被删」这类结论一个都给不出。
- 消费者侧同理，且要把话说准：**「断流后档位整场下线」这条业务判定已有离线证据**
  （§6.2 的 41 条用例覆盖翻译、判定线、位点结论、启停与回滚），
  **没有的证据是 broker 语义**：分区分配、位点提交与重平衡、`ForceCommit=false` 时 kq 是否真的
  不提交、同一事件被 broker 重投时的实际次数，全部未验证（缺口 3）。
  因此「主播停播后观众侧不再拿到死档位」这句结论目前只能说到「代码路径与事务写入正确」，
  不能说「线上已生效」。
- `internal/server`、`rpc/*.pb.go` 等 goctl 生成壳不在单测范围内（本服务无 HTTP handler）。

### 7.6 验证命令

```bash
go test -p 1 -count=1 ./services/live-media/...
gofmt -l services/live-media    # 必须为空
go vet ./services/live-media/...
# 事件两侧带标签（§6.1 发布器、§6.2 消费者）：默认构建与带标签构建都必须跑
go build -tags livemedia_kafka ./services/live-media/...
go vet -tags livemedia_kafka ./services/live-media/...
go test -p 1 -count=1 -tags livemedia_kafka ./services/live-media/internal/publisher/
go test -p 1 -count=1 -tags livemedia_kafka ./services/live-media/internal/consumer/
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。

## 8. 已知缺口（实现/上线前必须处理）

1. **发布器已接线，但「投递语义」仍无 broker 证据**（2026-10-04）：`internal/publisher` 复用
   `common/outbox` 把 `live_media_outbox` 的待发行同步投到 9 个 `livemedia.*.v1` topic，
   代码在 `-tags livemedia_kafka` 后面，接线与口径见 §6.1。**证据层级只到「可编译 / 可静态检查 /
   该包单测通过」**：本仓库从未与任何 broker 联调，`Enabled` 在示例配置里恒为 false。
   仍缺的三件：① 这 9 个 topic 在仓库内仍没有任何消费者（本服务新接的消费者只读 `live.state.v1`，
   下游触发点不会发生，见 `docs/roadmap.md` A 组）；② `kq.NewPusher` 不暴露 SASL/TLS 注入口
   （`go-queue v1.2.2`），带鉴权的集群用现有依赖对接不了：配置里的 `Username`/`Password`/`CaFile`
   三键**只被消费侧读取**，发布侧即使填了也没有任何作用（`ValidateKafka` 会校验、`ValidatePublishKafka` 不管）；
   ③ 判死（`state=2`）后没有人工放行/重投接口，只能改库或等 `services/cron` 补运维入口。
   `ServiceContext.Notes()` 现在只报下游 client 与缓存的配置状态，发布器口径改由
   `publisher.RuntimeNotes()` 在 `startPublisher()` 里打印。
2. **无 `internal/repository`，跨服务编排没有调用点**：`svc` 已按配置可选构造 Asset/Video/Moderation
   三个下游 client，但**当前没有任何 logic 调用它们**（回放 → asset → video → moderation 的 Worker 侧
   编排要在 consumer 或 `services/cron` 落地时接线）。客户端为 nil 时调用点必须显式报错，不得当作检查通过。
3. **`live.state.v1` 的消费链路已接线，但两处残余项仍在**（2026-10-04）：
   `internal/consumer` 把停播事件接成「本场次在线档位整场下线」，六段文件、口径与用例见 §6.2，
   入口 `livemedia.v1.go` 用 `logx.Must(consumer.Start(...))` 保证参数不完整时进程不启动。
   还缺的不是「有没有消费者」，而是下面三件：
   ① **没有持久化死信 / 消费位点表**（与 inbox、search-indexer、notification 的方案不同）：
      幂等由 `MarkOfflineTx` 的 `state=在线` CAS 条件与 `live_session_id` 维度承担，
      所以本服务刻意不建 inbox 表；代价是「达到 `Kafka.MaxRetries` 上限后放弃重投」的事件
      **只留下一条 `given_up` 错误日志**（`internal/consumer/handler.go:223`），位点一提交就没有别的痕迹，
      这段时间里该场次的档位一直挂着（可能继续被 live-gateway 分发给观众），
      直到 `online_expire_at` 到点被 `MarkExpiredOffline` 收掉；登记时不带有效期的档位
      只能靠运营用 `ListStreamOutputs` + `OfflineStreamOutput` **逐档位**下线。
      要把它变成可查询、可重放的证据，需要新增迁移（消费位点/死信表）与运维入口。
   ② **没有任何 broker 侧验证**：分区分配、位点提交、重平衡、`ForceCommit=false` 的实际效果
      都只有离线替身（§7.5）。「断流后档位自动下线」这句结论在线上要等联调才能写。
   ③ **`livemedia.stream.output.offline` 事件发出去后没有下游消费者**，所以 live-gateway/playback
      侧的投影收敛目前仍只能靠 RPC 拉取（缺口 1 的 ①）。
4. **无超时清扫 Worker / 回收执行器**：`idx_state_timeout`、`idx_state_expire`、`idx_registered_at`
   与 `ListTimedOut`（转码/录制）、`MarkExpiredOffline`（到期档位）、`PurgeByRecord`（切片清理）、
   `PurgePublished`（Outbox 已发布事件）等能力已在 model 就位，
   但没有调度方（应由 `services/cron` 或本服务内的清扫循环调用）。`TaskTimeoutSweepEnabled` 当前为 false。
5. **列表过滤维度不足**：`ListReplayTasksReq` 没有 `record_id`/`anchor_mid` 过滤位（表上有
   `idx_record_range`、`idx_anchor_state` 却用不上）；`ListReplayAssetRefsReq` 无法表达「只看未同步的投影」
   与「只看待回收引用」。改 `.proto` 后需跑 `scripts/gen.ps1 -Service live-media`。
6. **多副本下的判死边界没有租约保护**：退避纯函数本身已由 `common/outbox` 提供
   （`backoff = base * 2^(n-1)`，夹在 `[base, max]`，`common/outbox/outbox.go:360` 的
   `nextRetryAt` 与 `TestBackoffCurveIsExponentialAndCapped` 钉住），
   本服务不再各写各的。剩下的真实缺口在并发计数：`live_media_outbox` 没有 `lease_owner`/`lease_until`
   这类列，两个副本可以同时取到同一行、各自投一次，`retry_count` 由 SQL 侧 `+1` 自增所以计数不会互相覆盖，
   但**引擎是按自己读到的那一份 `retry_count` 判死**，因此并发时某条事件可能比 `Kafka.MaxRetries`
   多投一到两轮才被判定（at-least-once 语义本身不受影响，`event_id` 唯一索引 + 消费侧去重承担幂等）。
   要收紧只能加租约列（需要新迁移），或在 `svc` 侧保证单副本启动发布循环。
7. **`ErrPsTooLarge` 无使用点**：`ps` 越界由 `clampPage`（model 层）与 logic 侧同口径静默夹取
   （避免打挂 DB），与契约里「超限报错」的语义不同；`ErrInvalidCursor`（游标非法/归零）和
   `ErrTitleTooLong`（标题超上限）已有使用点（`helpers.go`/`listrecordsegmentslogic.go`、
   `replaypolicy.go`），但**还没有直接断言这两个错误的单测**。列表类要改判为拒绝必须同步 proto 注释和单测。
8. **回收的「真删」没有 Storage 适配层**：`live_retention_task` 只描述意图与计数证据，
   真正删除对象需要 bucket 级客户端；本期未建 `internal/repository/storage*.go`。
   闸门已在契约里：`purge=false` 只登记并把引用行置 `retention_state=1`（`deleted` 恒为 0），
   必须先有人核对 `scanned/skipped` 计数再提交 `purge=true` 的任务，才允许 Worker 真删对象引用
   （迁移列注释与 `SubmitRetentionTask`/`ReportRetentionResult` 注释已写明）。
9. **`live_session_id=0` 的历史行无法归属场次**：登记表允许 0（登记时未提供），
    但回放/回收按场次对账时会漏掉这些行；`live-ingest`/`live-room` 接线时必须保证透传真实场次 ID。
10. **`operator` 只做长度与空白校验、不脱敏**：`sanitizeOperator`（`helpers.go`）拒绝空值与超列宽（64），
    但**不擦除凭据形态**——调用方把带签名参数的地址写进 `operator` 会原样入库并进事件 payload。
    `reason`/`err_msg` 侧有 `sanitize*` 脱敏，`retention_test.go` 的 `TestSubmitRetentionTaskRedactsCredentialsInReason`
    只覆盖 `reason`。改这一列会同时影响转码与录制两条链路的 3 个调用点，本轮刻意未动，
    留待与 `common` 侧脱敏口径统一后一次改齐（届时补 operator 脱敏用例）。
11. **切片没有按单行的删除入口**：`model` 只有 `PurgeByRecordTx(record_id, limit)`（按录制任务粒度），
    所以 `target_kind=SEGMENT` 的回收在登记与上报两侧都**不碰本地切片行**，只记告警与计数证据
    （见 `reportretentionresultlogic.go` 函数头）。补该能力时要一并给
    `TestReportRetentionResultSegmentPurgeLeavesRowsAndStillRecordsEvidence` 换断言，
    不能让「行仍在」这一护栏悄悄被删掉。
12. **没有迁移↔model 的自动对账门禁**：`internal/logic/helpers.go` 的列宽常量、`model` 的 `db` tag、
    `xColumns` SELECT 常量与 `deploy/migrations/live-media/*.sql` 四处文本的一致性目前是人工比对
    （上节逐表列数即该次比对的记录）。仓库内已有 11 个服务落了 `model/migration_parity_test.go`
    （不连库、只读 SQL 文本比对列集合/唯一键/索引），本服务与另外 31 个带 `model/` 的服务同样尚未纳入，
    漂移的故障形态是运行期 `Unknown column` 或字段错位扫描，而不是启动失败。
