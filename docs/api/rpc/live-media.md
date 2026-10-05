# RPC · `live-media`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/live-media/rpc/livemedia.proto` |
| protobuf 包 | `livemedia.v1` |
| go_package | `go-video/services/live-media/rpc` |
| 发现用的 etcd key | `livemedia.v1.rpc`（`services/live-media/etc/livemedia.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`livemedia.v1.rpc`） |
| 监听 | `8120`（`services/live-media/etc/livemedia.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_live_media` |
| 方法数 | 28（service `LiveMedia`） |
| 网关消费方 | `admin:LiveMediaRPC` |

## 契约说明

> live-media 服务：直播实时媒体的转码、分发、录制、回放拼接与回收的领域服务。
>
> 数据所有权（AGENTS.md §5）：本服务拥有
>   live_transcode_task（直播转码任务）、live_stream_output（分发输出/码率梯）、
>   live_record_task（录制任务）、live_record_segment（录制切片）、
>   live_replay_task（回放拼接任务）、live_replay_asset_ref（回放资产引用）、
>   live_retention_task（回收任务）、live_media_outbox（事件 Outbox）。
> 直播间、推流密钥与流状态归 live-room / live-ingest；媒资元数据归 asset；稿件与发布
> 状态归 video；转码模板主数据归 transcode。本服务只保存它们的主键引用，绝不复制主数据。
>
> 硬约束：
>   1. 回放必须走普通视频的审核与发布链路（asset.RegisterAsset → video.CreateSubmission →
>      moderation-orchestrator.SubmitForReview）。本服务只写 aid/asset_id 引用，
>      永不把回放标记成"已发布"；rpc.ReviewState 只是来自 video 的只读投影。
>   2. 直播实时链路（live_stream_output）与回放发布状态分开建模：断流即下线档位，
>      与回放是否过审无关。
>   3. 大文件不进 MySQL，只存 bucket/object_key 引用（AGENTS.md §5、docs/migrations 约定）。
>   4. 任务状态机一律用「条件 UPDATE + RowsAffected」推进，非法迁移返回错误，不做假成功。
>   5. 真实 FFmpeg / CDN / 对象存储调用不在本服务进程内发生：由 Worker 拉起，
>      本契约只提供任务登记、进度回报与产物登记入口（见服务 README 的已知缺口）。
>
> 时间字段统一为 Unix 秒（字段名以 _at 结尾，0 表示未设置）；时长用毫秒（*_ms）。
> 跨服务只传主键：room_id / live_session_id / asset_id / aid / template_id / mid。
>  
> ---------------------------------------------------------------------------
> 枚举
> ---------------------------------------------------------------------------

## service `LiveMedia`

> --------------------------------------------------------------------------- / 服务定义 / --------------------------------------------------------------------------- / LiveMedia 直播转码、分发、录制、回放与回收服务。 / 注意：本服务不提供任何会员/付费直播/商业化能力（AGENTS.md §1）。

gRPC 方法前缀：`livemedia.v1.LiveMedia/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `StartLiveTranscode` | [`StartLiveTranscodeReq`](#message-startlivetranscodereq) | [`LiveTranscodeTaskInfo`](#message-livetranscodetaskinfo) | --- 直播转码任务：启停、重试、超时、取消、进度上报 --- / 登记直播转码任务（PENDING），request_id 幂等；不在此调用 FFmpeg |
| 2 | `StopLiveTranscode` | [`StopLiveTranscodeReq`](#message-stoplivetranscodereq) | [`LiveTranscodeTaskInfo`](#message-livetranscodetaskinfo) | 请求停止（RUNNING→STOPPING，Worker 收尾后 STOPPED） |
| 3 | `RetryLiveTranscode` | [`RetryLiveTranscodeReq`](#message-retrylivetranscodereq) | [`LiveTranscodeTaskInfo`](#message-livetranscodetaskinfo) | 重试失败任务（FAILED→PENDING，attempt+1，受 max_attempts 限制） |
| 4 | `CancelLiveTranscode` | [`CancelLiveTranscodeReq`](#message-cancellivetranscodereq) | [`LiveTranscodeTaskInfo`](#message-livetranscodetaskinfo) | 取消未运行/停止中的任务（PENDING\|STOPPING→CANCELLED 终态） |
| 5 | `ReportLiveTranscodeProgress` | [`ReportLiveTranscodeProgressReq`](#message-reportlivetranscodeprogressreq) | [`LiveTranscodeTaskInfo`](#message-livetranscodetaskinfo) | Worker 上报心跳/进度/终态（含超时判定），条件 UPDATE + 版本校验 |
| 6 | `GetLiveTranscodeTask` | [`LiveTranscodeTaskReq`](#message-livetranscodetaskreq) | [`LiveTranscodeTaskInfo`](#message-livetranscodetaskinfo) | 查询单个转码任务 |
| 7 | `ListLiveTranscodeTasks` | [`ListLiveTranscodeTasksReq`](#message-listlivetranscodetasksreq) | [`ListLiveTranscodeTasksReply`](#message-listlivetranscodetasksreply) | 分页查询转码任务（房间/场次/状态/模板） |
| 8 | `UpsertStreamOutput` | [`UpsertStreamOutputReq`](#message-upsertstreamoutputreq) | [`StreamOutputInfo`](#message-streamoutputinfo) | --- 分发输出（直播实时链路） --- / 登记或刷新一个码率档位的分发输出（(room,session,level,protocol) 唯一） |
| 9 | `OfflineStreamOutput` | [`OfflineStreamOutputReq`](#message-offlinestreamoutputreq) | [`StreamOutputInfo`](#message-streamoutputinfo) | 下线一个档位（断流/到期/人工），与回放发布状态无关 |
| 10 | `ListStreamOutputs` | [`ListStreamOutputsReq`](#message-liststreamoutputsreq) | [`ListStreamOutputsReply`](#message-liststreamoutputsreply) | 查询房间当前可分发档位（live-gateway / live-room 只读投影） |
| 11 | `StartLiveRecord` | [`StartLiveRecordReq`](#message-startliverecordreq) | [`LiveRecordTaskInfo`](#message-liverecordtaskinfo) | --- 录制任务与切片 --- / 登记录制任务（PENDING），request_id 幂等 |
| 12 | `StopLiveRecord` | [`StopLiveRecordReq`](#message-stopliverecordreq) | [`LiveRecordTaskInfo`](#message-liverecordtaskinfo) | 停止录制（RECORDING→STOPPING，最后一片落库后 STOPPED） |
| 13 | `ReportLiveRecordProgress` | [`ReportLiveRecordProgressReq`](#message-reportliverecordprogressreq) | [`LiveRecordTaskInfo`](#message-liverecordtaskinfo) | Worker 上报录制心跳与状态（含超时/断点续录） |
| 14 | `GetLiveRecordTask` | [`LiveRecordTaskReq`](#message-liverecordtaskreq) | [`LiveRecordTaskInfo`](#message-liverecordtaskinfo) | 查询单个录制任务（含 last_seq，供断点续录） |
| 15 | `ListLiveRecordTasks` | [`ListLiveRecordTasksReq`](#message-listliverecordtasksreq) | [`ListLiveRecordTasksReply`](#message-listliverecordtasksreply) | 分页查询录制任务 |
| 16 | `ReportRecordSegment` | [`ReportRecordSegmentReq`](#message-reportrecordsegmentreq) | [`RecordSegmentInfo`](#message-recordsegmentinfo) | 逐片登记切片（(record_id,seq) 幂等，缺口必须显式登记 MISSING） |
| 17 | `ListRecordSegments` | [`ListRecordSegmentsReq`](#message-listrecordsegmentsreq) | [`ListRecordSegmentsReply`](#message-listrecordsegmentsreply) | keyset 分页拉取切片（回放拼接与排障） |
| 18 | `SubmitReplayTask` | [`SubmitReplayTaskReq`](#message-submitreplaytaskreq) | [`LiveReplayTaskInfo`](#message-livereplaytaskinfo) | --- 回放拼接与资产引用 --- / 提交回放拼接任务（只登记与校验切片区间，不拼接、不发布） |
| 19 | `ReportReplayProgress` | [`ReportReplayProgressReq`](#message-reportreplayprogressreq) | [`LiveReplayTaskInfo`](#message-livereplaytaskinfo) | Worker 上报回放进度（拼接/上传/登记/送审） |
| 20 | `GetReplayTask` | [`ReplayTaskReq`](#message-replaytaskreq) | [`LiveReplayTaskInfo`](#message-livereplaytaskinfo) | 查询单个回放任务 |
| 21 | `ListReplayTasks` | [`ListReplayTasksReq`](#message-listreplaytasksreq) | [`ListReplayTasksReply`](#message-listreplaytasksreply) | 分页查询回放任务 |
| 22 | `BindReplayAsset` | [`BindReplayAssetReq`](#message-bindreplayassetreq) | [`ReplayAssetRefInfo`](#message-replayassetrefinfo) | 回填回放产物与 asset/稿件的引用关系（只存引用，不推进稿件状态） |
| 23 | `ApplyReplayContentState` | [`ApplyReplayContentStateReq`](#message-applyreplaycontentstatereq) | [`ReplayAssetRefInfo`](#message-replayassetrefinfo) | 同步 video 侧审核/发布投影（单向：video → live-media） |
| 24 | `ListReplayAssetRefs` | [`ListReplayAssetRefsReq`](#message-listreplayassetrefsreq) | [`ListReplayAssetRefsReply`](#message-listreplayassetrefsreply) | 分页查询回放资产引用（房间/场次/主播/投影状态） |
| 25 | `SubmitRetentionTask` | [`SubmitRetentionTaskReq`](#message-submitretentiontaskreq) | [`LiveRetentionTaskInfo`](#message-liveretentiontaskinfo) | --- 回收任务 --- / 提交回收任务（超期切片/回放产物/残留档位），先登记后执行，保留审计证据 |
| 26 | `ReportRetentionResult` | [`ReportRetentionResultReq`](#message-reportretentionresultreq) | [`LiveRetentionTaskInfo`](#message-liveretentiontaskinfo) | Worker 上报回收结果（扫描/删除/跳过计数） |
| 27 | `GetRetentionTask` | [`RetentionTaskReq`](#message-retentiontaskreq) | [`LiveRetentionTaskInfo`](#message-liveretentiontaskinfo) | 查询单个回收任务 |
| 28 | `ListRetentionTasks` | [`ListRetentionTasksReq`](#message-listretentiontasksreq) | [`ListRetentionTasksReply`](#message-listretentiontasksreply) | 分页查询回收任务 |

## 消息与枚举

### enum `LiveTranscodeState`

> 直播转码任务状态机（live_transcode_task.state）： /  / PENDING ──Start/Worker 拉起──▶ RUNNING ──Stop──▶ STOPPING ──▶ STOPPED(终态) /    ▲                              │ /    │──Retry── FAILED ◀──超时/异常/断流┘ / PENDING ──Cancel──▶ CANCELLED(终态) /  / FAILED → PENDING 只允许通过 RetryLiveTranscode（attempt+1），其它写操作不得复活终态任务。

| 值 | 编号 | 说明 |
|---|---|---|
| `LIVE_TRANSCODE_STATE_UNSPECIFIED` | 0 | 未指定（仅用作查询"不过滤"） |
| `LIVE_TRANSCODE_STATE_PENDING` | 1 | 待拉起（任务已登记，Worker 尚未启动 FFmpeg） |
| `LIVE_TRANSCODE_STATE_RUNNING` | 2 | 运行中（Worker 已拉起并持续上报心跳） |
| `LIVE_TRANSCODE_STATE_STOPPING` | 3 | 停止中（已下发停止指令，等待 Worker 收尾） |
| `LIVE_TRANSCODE_STATE_STOPPED` | 4 | 已停止（终态） |
| `LIVE_TRANSCODE_STATE_FAILED` | 5 | 失败（可 Retry；超过 max_attempts 后由运营/回收处理） |
| `LIVE_TRANSCODE_STATE_CANCELLED` | 6 | 已取消（终态） |

### enum `LiveRecordState`

> 录制任务状态机（live_record_task.state），与转码任务同构但独立： / 录制必须能断点续录，因此 last_seq 只增不减，STOPPED 后才允许拼接回放。

| 值 | 编号 | 说明 |
|---|---|---|
| `LIVE_RECORD_STATE_UNSPECIFIED` | 0 | 未指定 |
| `LIVE_RECORD_STATE_PENDING` | 1 | 待开始（已登记时间区间） |
| `LIVE_RECORD_STATE_RECORDING` | 2 | 录制中 |
| `LIVE_RECORD_STATE_STOPPING` | 3 | 停止中（等待最后一片落库） |
| `LIVE_RECORD_STATE_STOPPED` | 4 | 已停止（终态，可拼接回放） |
| `LIVE_RECORD_STATE_FAILED` | 5 | 失败（可 Retry 续录，从 last_seq+1 继续） |
| `LIVE_RECORD_STATE_CANCELLED` | 6 | 已取消（终态） |

### enum `SegmentState`

> 录制切片状态（live_record_segment.state）。 / 切片是回放拼接的最小单位，必须能表达"缺口"，否则回放会出现时间轴空洞。

| 值 | 编号 | 说明 |
|---|---|---|
| `SEGMENT_STATE_UNSPECIFIED` | 0 | 未指定 |
| `SEGMENT_STATE_UPLOADING` | 1 | 上传中（Worker 已切片，对象存储尚未确认） |
| `SEGMENT_STATE_UPLOADED` | 2 | 已上传（对象存储已确认，未校验） |
| `SEGMENT_STATE_VERIFIED` | 3 | 已校验（时长/大小/checksum 通过，可参与拼接） |
| `SEGMENT_STATE_MISSING` | 4 | 缺失（断点续录时探测到的空洞，回放拼接需跳过并记录） |
| `SEGMENT_STATE_CORRUPT` | 5 | 损坏（校验失败，永不参与拼接） |

### enum `ReplayState`

> 回放拼接任务状态机（live_replay_task.state）。 / 注意：终态是 REVIEW_SUBMITTED / COMPLETED，其中 COMPLETED 只表示"由 video 投影得知回放已可用"， / 本服务不会、也不允许把自己或稿件写成已发布（AGENTS.md §5/§8）。

| 值 | 编号 | 说明 |
|---|---|---|
| `REPLAY_STATE_UNSPECIFIED` | 0 | 未指定 |
| `REPLAY_STATE_PENDING` | 1 | 待拼接 |
| `REPLAY_STATE_MERGING` | 2 | 拼接中（Worker 正在切片合并/转封装） |
| `REPLAY_STATE_UPLOADING` | 3 | 产物上传中（对象存储写入） |
| `REPLAY_STATE_REGISTERED` | 4 | 已登记媒资（asset_id 已回填） |
| `REPLAY_STATE_REVIEW_SUBMITTED` | 5 | 已提交审核（aid 已回填，等待 moderation/video 结论） |
| `REPLAY_STATE_COMPLETED` | 6 | 回放可用（只读投影：video 侧已发布） |
| `REPLAY_STATE_FAILED` | 7 | 失败（终态，可重新提交新任务） |
| `REPLAY_STATE_CANCELLED` | 8 | 已取消（终态） |

### enum `ReviewState`

> 回放所引用稿件的审核/发布投影（live_replay_asset_ref.review_state）。 / 该值只由 ApplyReplayContentState 或 content.published.v1 消费者写入， / 事实源永远是 video 服务；本枚举只是本地可读投影，不得反向推进稿件状态。

| 值 | 编号 | 说明 |
|---|---|---|
| `REVIEW_STATE_UNSPECIFIED` | 0 | 未同步（尚未拿到 video 的结论） |
| `REVIEW_STATE_REVIEWING` | 1 | 审核中 |
| `REVIEW_STATE_REJECTED` | 2 | 驳回（回放不可用） |
| `REVIEW_STATE_PUBLISHED` | 3 | video 侧已发布（回放可对外提供） |
| `REVIEW_STATE_OFFLINE` | 4 | video 侧已下架 |
| `REVIEW_STATE_DELETED` | 5 | video 侧已删除（引用需回收） |

### enum `StreamProtocol`

> 分发输出协议（live_stream_output.protocol）。

| 值 | 编号 | 说明 |
|---|---|---|
| `STREAM_PROTOCOL_UNSPECIFIED` | 0 | 未指定 |
| `STREAM_PROTOCOL_HLS` | 1 | HLS（m3u8 + ts/ll-hls 分片） |
| `STREAM_PROTOCOL_HTTP_FLV` | 2 | HTTP-FLV（CDN 边缘拉流） |
| `STREAM_PROTOCOL_RTMP` | 3 | RTMP 转出（兼容老播放器） |
| `STREAM_PROTOCOL_ARTC` | 4 | 低延迟 RTC 分发（依赖 CDN 能力，见已知缺口） |

### enum `BitrateLevel`

> 码率档位（live_stream_output.bitrate_level）。档位是直播侧的稳定语义， / 具体宽高码率参数在下发任务时以快照字段写入，不复制 transcode 模板主数据。

| 值 | 编号 | 说明 |
|---|---|---|
| `BITRATE_LEVEL_UNSPECIFIED` | 0 | 未指定 |
| `BITRATE_LEVEL_SOURCE` | 1 | 原画（不转码，仅分发） |
| `BITRATE_LEVEL_UHD` | 2 | 1080p |
| `BITRATE_LEVEL_HD` | 3 | 720p |
| `BITRATE_LEVEL_SD` | 4 | 480p |
| `BITRATE_LEVEL_LD` | 5 | 360p |
| `BITRATE_LEVEL_AUDIO` | 6 | 纯音频 |

### enum `RetentionTargetKind`

> 回收任务的回收对象（live_retention_task.target_kind）。

| 值 | 编号 | 说明 |
|---|---|---|
| `RETENTION_TARGET_KIND_UNSPECIFIED` | 0 | 未指定 |
| `RETENTION_TARGET_KIND_SEGMENT` | 1 | 录制切片（超期或已被回放合并吸收） |
| `RETENTION_TARGET_KIND_REPLAY` | 2 | 回放产物（引用已删除或超期） |
| `RETENTION_TARGET_KIND_STREAM_OUTPUT` | 3 | 直播分发残留（断流后未清理的档位） |

### enum `RetentionState`

> 回收任务状态（live_retention_task.state）。 / 回收是"先登记意图、再执行、最后留证"的三步流程，禁止边查边删（无法审计，AGENTS.md §8）。

| 值 | 编号 | 说明 |
|---|---|---|
| `RETENTION_STATE_UNSPECIFIED` | 0 | 未指定 |
| `RETENTION_STATE_PENDING` | 1 | 待执行（含 dry_run 预演） |
| `RETENTION_STATE_RUNNING` | 2 | 执行中 |
| `RETENTION_STATE_SUCCEEDED` | 3 | 已完成（终态） |
| `RETENTION_STATE_FAILED` | 4 | 失败（终态，需人工介入） |
| `RETENTION_STATE_CANCELLED` | 5 | 已取消（终态） |

### enum `FailureReason`

> 任务失败原因分类（errno 之外的可读语义，便于运营聚合与告警）。

| 值 | 编号 | 说明 |
|---|---|---|
| `FAILURE_REASON_UNSPECIFIED` | 0 | 未指定 |
| `FAILURE_REASON_TIMEOUT` | 1 | 超时（timeout_at 到期未上报心跳） |
| `FAILURE_REASON_SOURCE_LOST` | 2 | 源流丢失/断流 |
| `FAILURE_REASON_WORKER_CRASH` | 3 | Worker 异常退出 |
| `FAILURE_REASON_STORAGE` | 4 | 对象存储写入失败 |
| `FAILURE_REASON_CDN` | 5 | CDN 调用失败 |
| `FAILURE_REASON_UPSTREAM_DENIED` | 6 | 上游服务拒绝（asset/video/moderation 返回业务错误） |
| `FAILURE_REASON_MANUAL` | 7 | 人工停止/取消 |
| `FAILURE_REASON_DATA_GAP` | 8 | 数据缺口（录制切片空洞导致无法拼接） |

### message `EmptyReply`

> --------------------------------------------------------------------------- / 公共结构 / --------------------------------------------------------------------------- / 空响应

（空消息）

### message `PageParam`

> 统一分页参数：pn 从 1 开始，ps 上限 50（服务端会夹取，不返回错误）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pn` | `int32` | 1 | — | 页码（从 1 开始，<=0 视为 1） |
| `ps` | `int32` | 2 | — | 每页大小（最大 50，<=0 或超限取默认 20） |

### message `PageResult`

> 统一分页响应头部。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 符合条件的总行数 |

### message `StartLiveTranscodeReq`

> --------------------------------------------------------------------------- / 直播转码任务 / ---------------------------------------------------------------------------

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 直播间 ID（live-room 主键） |
| `live_session_id` | `int64` | 2 | — | 直播场次 ID（live-room/live-ingest 主键，0 表示未提供） |
| `template_id` | `int64` | 3 | — | 转码模板 ID（transcode 主键；直播模板能力见已知缺口） |
| `bitrate_level` | [`BitrateLevel`](#enum-bitratelevel) | 4 | — | 码率档位 |
| `protocol` | [`StreamProtocol`](#enum-streamprotocol) | 5 | — | 输出协议 |
| `source_ref` | `string` | 6 | — | 拉流源引用（live-ingest 侧的短期地址或流标识，禁止存长期密钥） |
| `anchor_mid` | `int64` | 7 | — | 主播 mid（仅审计用，不做商业化判断） |
| `max_attempts` | `int32` | 8 | — | 最大重试次数（0 用服务端默认） |
| `timeout_seconds` | `int32` | 9 | — | 无心跳超时秒数（0 用服务端默认） |
| `request_id` | `string` | 10 | — | 幂等键：同 request_id 重放返回同一任务 |
| `trace_id` | `string` | 11 | — | 调用方透传 trace_id |

### message `LiveTranscodeTaskInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |
| `room_id` | `int64` | 2 | — | 直播间 ID |
| `live_session_id` | `int64` | 3 | — | 直播场次 ID |
| `template_id` | `int64` | 4 | — | 转码模板 ID（引用，不复制模板主数据） |
| `bitrate_level` | [`BitrateLevel`](#enum-bitratelevel) | 5 | — | 码率档位 |
| `protocol` | [`StreamProtocol`](#enum-streamprotocol) | 6 | — | 输出协议 |
| `source_ref` | `string` | 7 | — | 拉流源引用 |
| `anchor_mid` | `int64` | 8 | — | 主播 mid |
| `state` | [`LiveTranscodeState`](#enum-livetranscodestate) | 9 | — | 状态 |
| `progress` | `int32` | 10 | — | 进度或健康度提示（0-100，含义见 state 注释） |
| `attempt` | `int32` | 11 | — | 已执行次数（Retry 时 +1） |
| `max_attempts` | `int32` | 12 | — | 最大重试次数 |
| `started_at` | `int64` | 13 | — | 实际启动时间（Unix 秒） |
| `stopped_at` | `int64` | 14 | — | 实际停止时间（Unix 秒，0 表示未停止） |
| `heartbeat_at` | `int64` | 15 | — | 最近一次 Worker 心跳时间（Unix 秒） |
| `timeout_at` | `int64` | 16 | — | 心跳超时判定时刻（Unix 秒） |
| `version` | `int64` | 17 | — | 乐观并发版本号（上报时必须回传 expected_version） |
| `reason` | [`FailureReason`](#enum-failurereason) | 18 | — | 失败/停止原因 |
| `errno` | `int32` | 19 | — | 错误码（0 表示无错误） |
| `err_msg` | `string` | 20 | — | 脱敏错误信息（不含密钥/完整 URL） |
| `request_id` | `string` | 21 | — | 幂等键 |
| `trace_id` | `string` | 22 | — | 最近一次调用的 trace_id |
| `ctime` | `int64` | 23 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 24 | — | 修改时间（Unix 秒） |

### message `LiveTranscodeTaskReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |

### message `StopLiveTranscodeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |
| `expected_version` | `int64` | 2 | — | 乐观并发版本（0 表示不校验，仍受状态机约束） |
| `reason` | [`FailureReason`](#enum-failurereason) | 3 | — | 停止原因（MANUAL/UPSTREAM_DENIED 等） |
| `request_id` | `string` | 4 | — | 幂等键 |
| `operator` | `string` | 5 | — | 操作者标识（system/运营账号，仅审计） |
| `trace_id` | `string` | 6 | — | — |

### message `RetryLiveTranscodeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID（必须处于 FAILED） |
| `expected_version` | `int64` | 2 | — | 乐观并发版本 |
| `reason` | `string` | 3 | — | 重试说明 |
| `request_id` | `string` | 4 | — | 幂等键（同 request_id 重放不会重复 ++attempt） |
| `operator` | `string` | 5 | — | — |
| `trace_id` | `string` | 6 | — | — |

### message `CancelLiveTranscodeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | — |
| `expected_version` | `int64` | 2 | — | — |
| `reason` | [`FailureReason`](#enum-failurereason) | 3 | — | 取消原因 |
| `request_id` | `string` | 4 | — | 幂等键 |
| `operator` | `string` | 5 | — | — |
| `trace_id` | `string` | 6 | — | — |

### message `ReportLiveTranscodeProgressReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | — |
| `expected_version` | `int64` | 2 | — | 上报方持有的版本；不匹配返回冲突，服务端不覆盖新状态 |
| `state` | [`LiveTranscodeState`](#enum-livetranscodestate) | 3 | — | 目标状态（RUNNING/STOPPING/STOPPED/FAILED） |
| `progress` | `int32` | 4 | — | 进度（0-100） |
| `reason` | [`FailureReason`](#enum-failurereason) | 5 | — | 失败/停止原因（FAILED/TIMEOUT 必填） |
| `errno` | `int32` | 6 | — | — |
| `err_msg` | `string` | 7 | — | — |
| `worker_id` | `string` | 8 | — | Worker 标识（仅审计与排障） |
| `trace_id` | `string` | 9 | — | — |

### message `ListLiveTranscodeTasksReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 按房间过滤（<=0 不过滤） |
| `live_session_id` | `int64` | 2 | — | 按场次过滤（<=0 不过滤） |
| `state` | [`LiveTranscodeState`](#enum-livetranscodestate) | 3 | — | 按状态过滤（UNSPECIFIED 不过滤） |
| `template_id` | `int64` | 4 | — | 按模板过滤（<=0 不过滤） |
| `page` | [`PageParam`](#message-pageparam) | 5 | — | — |

### message `ListLiveTranscodeTasksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | [`PageResult`](#message-pageresult) | 1 | — | — |
| `tasks` | [`LiveTranscodeTaskInfo`](#message-livetranscodetaskinfo) | 2 | repeated | — |

### message `UpsertStreamOutputReq`

> --------------------------------------------------------------------------- / 分发输出（直播实时链路，与回放发布状态无关） / ---------------------------------------------------------------------------

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `live_session_id` | `int64` | 2 | — | — |
| `task_id` | `int64` | 3 | — | 产生该输出的转码任务（0 表示源流直出不经转码） |
| `bitrate_level` | [`BitrateLevel`](#enum-bitratelevel) | 4 | — | — |
| `protocol` | [`StreamProtocol`](#enum-streamprotocol) | 5 | — | — |
| `bucket` | `string` | 6 | — | 产物只存引用：playlist/流路径是对象存储或 CDN 的相对路径，不含签名与密钥。 |
| `object_key` | `string` | 7 | — | — |
| `cdn_domain` | `string` | 8 | — | CDN 域名（配置项，非密钥） |
| `width` | `int32` | 9 | — | 档位参数快照（下发时刻的值，不是 transcode 模板的镜像；模板变更不回写历史行）。 |
| `height` | `int32` | 10 | — | — |
| `bitrate_kbps` | `int32` | 11 | — | — |
| `fps` | `int32` | 12 | — | — |
| `online_expire_at` | `int64` | 13 | — | 在线有效期（Unix 秒，0 表示由断流事件下线） |
| `request_id` | `string` | 14 | — | 幂等键；(room,session,level,protocol) 唯一约束兜底 |
| `trace_id` | `string` | 15 | — | — |

### message `StreamOutputInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `output_id` | `int64` | 1 | — | — |
| `room_id` | `int64` | 2 | — | — |
| `live_session_id` | `int64` | 3 | — | — |
| `task_id` | `int64` | 4 | — | — |
| `bitrate_level` | [`BitrateLevel`](#enum-bitratelevel) | 5 | — | — |
| `protocol` | [`StreamProtocol`](#enum-streamprotocol) | 6 | — | — |
| `bucket` | `string` | 7 | — | — |
| `object_key` | `string` | 8 | — | — |
| `cdn_domain` | `string` | 9 | — | — |
| `width` | `int32` | 10 | — | — |
| `height` | `int32` | 11 | — | — |
| `bitrate_kbps` | `int32` | 12 | — | — |
| `fps` | `int32` | 13 | — | — |
| `state` | `int32` | 14 | — | 1 在线、2 已下线（与 rpc 无枚举一致，避免误用直播房间状态） |
| `online_at` | `int64` | 15 | — | 上线时间（Unix 秒） |
| `offline_at` | `int64` | 16 | — | 下线时间（Unix 秒，0 表示仍在线） |
| `online_expire_at` | `int64` | 17 | — | 在线有效期（Unix 秒） |
| `request_id` | `string` | 18 | — | — |
| `ctime` | `int64` | 19 | — | — |
| `mtime` | `int64` | 20 | — | — |
| `reason` | [`FailureReason`](#enum-failurereason) | 21 | — | 下线原因（state=2 时有效：源流丢失/到期/人工） |

### message `OfflineStreamOutputReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `output_id` | `int64` | 1 | — | output_id 与 (room_id, bitrate_level, protocol) 二选一 |
| `room_id` | `int64` | 2 | — | — |
| `bitrate_level` | [`BitrateLevel`](#enum-bitratelevel) | 3 | — | — |
| `protocol` | [`StreamProtocol`](#enum-streamprotocol) | 4 | — | — |
| `reason` | [`FailureReason`](#enum-failurereason) | 5 | — | 下线原因（源流丢失/人工/到期） |
| `request_id` | `string` | 6 | — | 幂等键（重复下线同一输出返回同一结果） |
| `trace_id` | `string` | 7 | — | — |

### message `ListStreamOutputsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 必填：房间维度查询当前档位 |
| `live_session_id` | `int64` | 2 | — | <=0 表示只看当前在线档位 |
| `include_offline` | `bool` | 3 | — | 是否包含已下线档位（默认只返回在线） |
| `page` | [`PageParam`](#message-pageparam) | 4 | — | — |

### message `ListStreamOutputsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | [`PageResult`](#message-pageresult) | 1 | — | — |
| `outputs` | [`StreamOutputInfo`](#message-streamoutputinfo) | 2 | repeated | — |

### message `StartLiveRecordReq`

> --------------------------------------------------------------------------- / 录制任务与切片 / ---------------------------------------------------------------------------

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `live_session_id` | `int64` | 2 | — | — |
| `source_task_id` | `int64` | 3 | — | 从哪个转码任务/源流录制（0 表示原画源） |
| `start_at` | `int64` | 4 | — | 期望录制起点（Unix 秒，0 表示立即） |
| `end_at` | `int64` | 5 | — | 期望录制终点（Unix 秒，0 表示随场次结束） |
| `segment_seconds` | `int32` | 6 | — | 分片时长（秒，0 用服务端默认） |
| `timeout_seconds` | `int32` | 7 | — | Worker 无心跳超时（秒，0 用服务端默认） |
| `output_bucket` | `string` | 8 | — | 切片存储桶（引用，不存凭据） |
| `output_prefix` | `string` | 9 | — | 切片对象 key 前缀 |
| `request_id` | `string` | 10 | — | 幂等键 |
| `trace_id` | `string` | 11 | — | — |

### message `LiveRecordTaskInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `record_id` | `int64` | 1 | — | — |
| `room_id` | `int64` | 2 | — | — |
| `live_session_id` | `int64` | 3 | — | — |
| `source_task_id` | `int64` | 4 | — | — |
| `state` | [`LiveRecordState`](#enum-liverecordstate) | 5 | — | — |
| `start_at` | `int64` | 6 | — | 期望起点（Unix 秒） |
| `end_at` | `int64` | 7 | — | 期望终点（Unix 秒） |
| `record_start_at` | `int64` | 8 | — | 实际开始录制时间（Unix 秒） |
| `record_end_at` | `int64` | 9 | — | 实际结束时间（Unix 秒，0 表示未结束） |
| `segment_seconds` | `int32` | 10 | — | 分片时长（秒） |
| `last_seq` | `int64` | 11 | — | 已登记的最大切片序号（断点续录起点 = last_seq+1） |
| `segment_count` | `int64` | 12 | — | 已登记切片数（含 MISSING） |
| `gap_count` | `int64` | 13 | — | 缺口（MISSING/CORRUPT）切片数 |
| `recorded_duration_ms` | `int64` | 14 | — | 有效录制时长（毫秒，VERIFIED 切片求和） |
| `output_bucket` | `string` | 15 | — | — |
| `output_prefix` | `string` | 16 | — | — |
| `heartbeat_at` | `int64` | 17 | — | — |
| `timeout_at` | `int64` | 18 | — | — |
| `version` | `int64` | 19 | — | — |
| `reason` | [`FailureReason`](#enum-failurereason) | 20 | — | — |
| `errno` | `int32` | 21 | — | — |
| `err_msg` | `string` | 22 | — | — |
| `request_id` | `string` | 23 | — | — |
| `trace_id` | `string` | 24 | — | — |
| `ctime` | `int64` | 25 | — | — |
| `mtime` | `int64` | 26 | — | — |

### message `LiveRecordTaskReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `record_id` | `int64` | 1 | — | — |

### message `StopLiveRecordReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `record_id` | `int64` | 1 | — | — |
| `expected_version` | `int64` | 2 | — | — |
| `end_at` | `int64` | 3 | — | 期望结束时刻（Unix 秒，0 表示立即） |
| `reason` | [`FailureReason`](#enum-failurereason) | 4 | — | — |
| `request_id` | `string` | 5 | — | — |
| `operator` | `string` | 6 | — | — |
| `trace_id` | `string` | 7 | — | — |

### message `ReportLiveRecordProgressReq`

> ReportLiveRecordProgress：Worker 上报录制心跳与状态。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `record_id` | `int64` | 1 | — | — |
| `expected_version` | `int64` | 2 | — | 乐观并发版本 |
| `state` | [`LiveRecordState`](#enum-liverecordstate) | 3 | — | RECORDING/STOPPING/STOPPED/FAILED |
| `last_seq` | `int64` | 4 | — | Worker 已产出的最大序号（服务端只接受单调递增） |
| `heartbeat_at` | `int64` | 5 | — | 0 表示由服务端取当前时间（Unix 秒） |
| `reason` | [`FailureReason`](#enum-failurereason) | 6 | — | — |
| `errno` | `int32` | 7 | — | — |
| `err_msg` | `string` | 8 | — | — |
| `worker_id` | `string` | 9 | — | — |
| `trace_id` | `string` | 10 | — | — |

### message `ReportRecordSegmentReq`

> ReportRecordSegment：Worker 逐片登记。 / 幂等由 (record_id, seq) 唯一键保证：同一 seq 重放只更新校验信息，不新增行、不回退状态。 / 断点续录：Worker 从 GetLiveRecordTask.last_seq+1 继续；检测到空洞时用 state=MISSING 显式登记， / 不得静默跳过（回放拼接需要知道时间轴上有洞）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `record_id` | `int64` | 1 | — | — |
| `seq` | `int64` | 2 | — | 切片序号（从 1 开始，必须 > 0） |
| `start_at` | `int64` | 3 | — | 切片起点（Unix 秒） |
| `end_at` | `int64` | 4 | — | 切片终点（Unix 秒） |
| `duration_ms` | `int64` | 5 | — | 切片时长（毫秒） |
| `state` | [`SegmentState`](#enum-segmentstate) | 6 | — | UPLOADING/UPLOADED/VERIFIED/MISSING/CORRUPT |
| `bucket` | `string` | 7 | — | 对象存储桶（MISSING 时可为空） |
| `object_key` | `string` | 8 | — | 切片对象 key |
| `size_bytes` | `int64` | 9 | — | 字节数 |
| `checksum` | `string` | 10 | — | 内容摘要（sha256 hex，脱敏：不是凭据） |
| `worker_id` | `string` | 11 | — | — |
| `trace_id` | `string` | 12 | — | — |

### message `RecordSegmentInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | 自增主键 |
| `record_id` | `int64` | 2 | — | — |
| `room_id` | `int64` | 3 | — | — |
| `live_session_id` | `int64` | 4 | — | — |
| `seq` | `int64` | 5 | — | — |
| `start_at` | `int64` | 6 | — | Unix 秒 |
| `end_at` | `int64` | 7 | — | Unix 秒 |
| `duration_ms` | `int64` | 8 | — | — |
| `state` | [`SegmentState`](#enum-segmentstate) | 9 | — | — |
| `bucket` | `string` | 10 | — | — |
| `object_key` | `string` | 11 | — | — |
| `size_bytes` | `int64` | 12 | — | — |
| `checksum` | `string` | 13 | — | — |
| `worker_id` | `string` | 14 | — | — |
| `registered_at` | `int64` | 15 | — | 首次登记时间（Unix 秒） |
| `mtime` | `int64` | 16 | — | 最近一次更新时间（Unix 秒） |

### message `ListRecordSegmentsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `record_id` | `int64` | 1 | — | 必填 |
| `state` | [`SegmentState`](#enum-segmentstate) | 2 | — | 按状态过滤（UNSPECIFIED 不过滤） |
| `after_seq` | `int64` | 3 | — | keyset 游标：只返回 seq > after_seq 的切片（0 表示从头） |
| `limit` | `int32` | 4 | — | 单次返回上限（最大 500，默认 200） |

### message `ListRecordSegmentsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `segments` | [`RecordSegmentInfo`](#message-recordsegmentinfo) | 1 | repeated | — |
| `next_after_seq` | `int64` | 2 | — | 下一页游标；无更多数据时返回最后一条 seq |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | 该 record_id 下的切片总数（含缺口） |

### message `ListLiveRecordTasksReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `live_session_id` | `int64` | 2 | — | — |
| `state` | [`LiveRecordState`](#enum-liverecordstate) | 3 | — | — |
| `page` | [`PageParam`](#message-pageparam) | 4 | — | — |

### message `ListLiveRecordTasksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | [`PageResult`](#message-pageresult) | 1 | — | — |
| `tasks` | [`LiveRecordTaskInfo`](#message-liverecordtaskinfo) | 2 | repeated | — |

### message `SubmitReplayTaskReq`

> --------------------------------------------------------------------------- / 回放拼接与回收 / --------------------------------------------------------------------------- / SubmitReplayTask：登记回放拼接任务。 / 本方法只做「登记 + 校验切片区间」，不做拼接、不写 asset、不建稿件、更不推进发布状态； / 实际链路由 Worker 依次调用 ReportReplayProgress / BindReplayAsset 回填。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `live_session_id` | `int64` | 2 | — | — |
| `record_id` | `int64` | 3 | — | 录制任务 ID（必填，回放素材来源） |
| `from_seq` | `int64` | 4 | — | 起始切片序号（<=0 表示从 1） |
| `to_seq` | `int64` | 5 | — | 结束切片序号（<=0 表示到最后） |
| `start_at` | `int64` | 6 | — | 可选时间区间起点（Unix 秒） |
| `end_at` | `int64` | 7 | — | 可选时间区间终点（Unix 秒） |
| `allow_gaps` | `bool` | 8 | — | 是否允许切片缺口（false 时缺口直接拒绝提交，避免产出坏回放） |
| `anchor_mid` | `int64` | 9 | — | 回放稿件归属主播 mid（video.CreateSubmission 的投稿人） |
| `title` | `string` | 10 | — | 回放标题（透传给稿件，不在本服务做审核判断） |
| `description` | `string` | 11 | — | — |
| `request_id` | `string` | 12 | — | 幂等键 |
| `trace_id` | `string` | 13 | — | — |

### message `LiveReplayTaskInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `replay_id` | `int64` | 1 | — | — |
| `room_id` | `int64` | 2 | — | — |
| `live_session_id` | `int64` | 3 | — | — |
| `record_id` | `int64` | 4 | — | — |
| `state` | [`ReplayState`](#enum-replaystate) | 5 | — | — |
| `from_seq` | `int64` | 6 | — | — |
| `to_seq` | `int64` | 7 | — | — |
| `segment_count` | `int64` | 8 | — | 参与拼接的有效切片数 |
| `gap_count` | `int64` | 9 | — | 区间内缺口切片数 |
| `start_at` | `int64` | 10 | — | 回放覆盖区间起点（Unix 秒） |
| `end_at` | `int64` | 11 | — | 终点 |
| `duration_ms` | `int64` | 12 | — | 拼接后时长（毫秒） |
| `allow_gaps` | `bool` | 13 | — | — |
| `output_bucket` | `string` | 14 | — | 产物引用（大文件不入 MySQL） |
| `output_key` | `string` | 15 | — | — |
| `asset_id` | `int64` | 16 | — | 登记的媒资 ID（asset 主键，0 表示未登记） |
| `aid` | `int64` | 17 | — | 稿件 ID（video 主键，0 表示未建稿） |
| `bvid` | `string` | 18 | — | 稿件 bvid（冗余展示字段，事实源仍是 video） |
| `anchor_mid` | `int64` | 19 | — | — |
| `title` | `string` | 20 | — | — |
| `version` | `int64` | 21 | — | — |
| `reason` | [`FailureReason`](#enum-failurereason) | 22 | — | — |
| `errno` | `int32` | 23 | — | — |
| `err_msg` | `string` | 24 | — | — |
| `request_id` | `string` | 25 | — | — |
| `trace_id` | `string` | 26 | — | — |
| `ctime` | `int64` | 27 | — | — |
| `mtime` | `int64` | 28 | — | — |

### message `ReplayTaskReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `replay_id` | `int64` | 1 | — | — |

### message `ReportReplayProgressReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `replay_id` | `int64` | 1 | — | — |
| `expected_version` | `int64` | 2 | — | 乐观并发版本 |
| `state` | [`ReplayState`](#enum-replaystate) | 3 | — | 目标状态（受状态机约束） |
| `segment_count` | `int64` | 4 | — | — |
| `gap_count` | `int64` | 5 | — | — |
| `duration_ms` | `int64` | 6 | — | — |
| `output_bucket` | `string` | 7 | — | — |
| `output_key` | `string` | 8 | — | — |
| `reason` | [`FailureReason`](#enum-failurereason) | 9 | — | — |
| `errno` | `int32` | 10 | — | — |
| `err_msg` | `string` | 11 | — | — |
| `worker_id` | `string` | 12 | — | — |
| `trace_id` | `string` | 13 | — | — |

### message `BindReplayAssetReq`

> BindReplayAsset 回填「回放产物 ↔ asset/稿件」引用。 / 关键约束：本方法只在 live_replay_asset_ref 里保存 asset_id/aid 引用， / 不写 asset_meta、不写 video_submission、不调用任何推进稿件状态的路径。 / 回放的审核与发布由 video/moderation-orchestrator 负责（AGENTS.md §5/§8）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `replay_id` | `int64` | 1 | — | — |
| `asset_id` | `int64` | 2 | — | asset.RegisterAsset 返回的媒资 ID |
| `aid` | `int64` | 3 | — | video.CreateSubmission 返回的稿件 ID |
| `bvid` | `string` | 4 | — | — |
| `bucket` | `string` | 5 | — | — |
| `object_key` | `string` | 6 | — | — |
| `duration_ms` | `int64` | 7 | — | — |
| `request_id` | `string` | 8 | — | 幂等键（同 replay_id 重复绑定同值返回成功） |
| `trace_id` | `string` | 9 | — | — |

### message `ReplayAssetRefInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `room_id` | `int64` | 2 | — | — |
| `live_session_id` | `int64` | 3 | — | — |
| `replay_id` | `int64` | 4 | — | — |
| `record_id` | `int64` | 5 | — | — |
| `asset_id` | `int64` | 6 | — | — |
| `aid` | `int64` | 7 | — | — |
| `bvid` | `string` | 8 | — | — |
| `anchor_mid` | `int64` | 9 | — | — |
| `bucket` | `string` | 10 | — | — |
| `object_key` | `string` | 11 | — | — |
| `duration_ms` | `int64` | 12 | — | — |
| `segment_from_seq` | `int64` | 13 | — | — |
| `segment_to_seq` | `int64` | 14 | — | — |
| `gap_count` | `int64` | 15 | — | — |
| `review_state` | [`ReviewState`](#enum-reviewstate) | 16 | — | 只读投影，事实源是 video |
| `review_state_at` | `int64` | 17 | — | 投影同步时间（Unix 秒） |
| `retention_state` | `int32` | 18 | — | 0 正常、1 待回收、2 已回收（引用行的生命周期） |
| `published_at` | `int64` | 19 | — | video 侧发布时间（Unix 秒，投影值，0 表示未发布） |
| `ctime` | `int64` | 20 | — | — |
| `mtime` | `int64` | 21 | — | — |

### message `ListReplayAssetRefsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 按房间过滤（<=0 不过滤） |
| `live_session_id` | `int64` | 2 | — | 按场次过滤 |
| `review_state` | [`ReviewState`](#enum-reviewstate) | 3 | — | 按投影状态过滤 |
| `anchor_mid` | `int64` | 4 | — | 按主播过滤 |
| `page` | [`PageParam`](#message-pageparam) | 5 | — | — |

### message `ListReplayAssetRefsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | [`PageResult`](#message-pageresult) | 1 | — | — |
| `refs` | [`ReplayAssetRefInfo`](#message-replayassetrefinfo) | 2 | repeated | — |

### message `ApplyReplayContentStateReq`

> ApplyReplayContentState 同步 video 侧的审核/发布投影。 / 调用方：content.published.v1 消费者，或运营/排障链路上的显式刷新。 / 本方法只更新本地投影字段，永不调用 video 的状态推进接口（方向单一：video → live-media）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `replay_id` | `int64` | 1 | — | replay_id 与 asset_id 至少一个 |
| `asset_id` | `int64` | 2 | — | — |
| `review_state` | [`ReviewState`](#enum-reviewstate) | 3 | — | 必填：来自 video 的事实状态 |
| `published_at` | `int64` | 4 | — | video 侧发布时间（Unix 秒） |
| `event_id` | `string` | 5 | — | 驱动本次同步的事件 ID（按 event_id 幂等） |
| `source` | `string` | 6 | — | 来源：content.published.v1 / video.rpc / manual |
| `trace_id` | `string` | 7 | — | — |

### message `ListReplayTasksReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | — |
| `live_session_id` | `int64` | 2 | — | — |
| `state` | [`ReplayState`](#enum-replaystate) | 3 | — | — |
| `page` | [`PageParam`](#message-pageparam) | 4 | — | — |

### message `ListReplayTasksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | [`PageResult`](#message-pageresult) | 1 | — | — |
| `tasks` | [`LiveReplayTaskInfo`](#message-livereplaytaskinfo) | 2 | repeated | — |

### message `SubmitRetentionTaskReq`

> --------------------------------------------------------------------------- / 回收任务 / ---------------------------------------------------------------------------

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `target_kind` | [`RetentionTargetKind`](#enum-retentiontargetkind) | 1 | — | 回收对象类型 |
| `room_id` | `int64` | 2 | — | <=0 表示全局扫描 |
| `target_id` | `int64` | 3 | — | 指定回收对象主键（0 表示按 expire_before 批量） |
| `expire_before` | `int64` | 4 | — | 只回收该时刻（Unix 秒）之前到期的对象 |
| `purge` | `bool` | 5 | — | false 只登记并置标记，true 才真正删除对象存储引用 |
| `batch_limit` | `int32` | 6 | — | 单次处理上限（最大 500，默认 100） |
| `reason` | `string` | 7 | — | 回收原因（审计必填：超期/切片被回放吸收/房间删除） |
| `request_id` | `string` | 8 | — | 幂等键 |
| `operator` | `string` | 9 | — | — |
| `trace_id` | `string` | 10 | — | — |

### message `LiveRetentionTaskInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `retention_id` | `int64` | 1 | — | — |
| `target_kind` | [`RetentionTargetKind`](#enum-retentiontargetkind) | 2 | — | — |
| `room_id` | `int64` | 3 | — | — |
| `target_id` | `int64` | 4 | — | — |
| `expire_before` | `int64` | 5 | — | — |
| `purge` | `bool` | 6 | — | — |
| `batch_limit` | `int32` | 7 | — | — |
| `state` | [`RetentionState`](#enum-retentionstate) | 8 | — | — |
| `scanned` | `int32` | 9 | — | 扫描命中行数 |
| `deleted` | `int32` | 10 | — | 实际删除行数（purge=false 时恒为 0） |
| `skipped` | `int32` | 11 | — | 跳过行数（仍被引用/状态不允许） |
| `reason` | `string` | 12 | — | — |
| `operator` | `string` | 13 | — | — |
| `version` | `int64` | 14 | — | — |
| `fail_reason` | [`FailureReason`](#enum-failurereason) | 15 | — | — |
| `errno` | `int32` | 16 | — | — |
| `err_msg` | `string` | 17 | — | — |
| `request_id` | `string` | 18 | — | — |
| `trace_id` | `string` | 19 | — | — |
| `ctime` | `int64` | 20 | — | — |
| `mtime` | `int64` | 21 | — | — |

### message `RetentionTaskReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `retention_id` | `int64` | 1 | — | — |

### message `ReportRetentionResultReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `retention_id` | `int64` | 1 | — | — |
| `expected_version` | `int64` | 2 | — | — |
| `state` | [`RetentionState`](#enum-retentionstate) | 3 | — | RUNNING/SUCCEEDED/FAILED/CANCELLED |
| `scanned` | `int32` | 4 | — | — |
| `deleted` | `int32` | 5 | — | — |
| `skipped` | `int32` | 6 | — | — |
| `fail_reason` | [`FailureReason`](#enum-failurereason) | 7 | — | — |
| `errno` | `int32` | 8 | — | — |
| `err_msg` | `string` | 9 | — | — |
| `worker_id` | `string` | 10 | — | — |
| `trace_id` | `string` | 11 | — | — |

### message `ListRetentionTasksReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `target_kind` | [`RetentionTargetKind`](#enum-retentiontargetkind) | 1 | — | — |
| `state` | [`RetentionState`](#enum-retentionstate) | 2 | — | — |
| `room_id` | `int64` | 3 | — | — |
| `page` | [`PageParam`](#message-pageparam) | 4 | — | — |

### message `ListRetentionTasksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `page` | [`PageResult`](#message-pageresult) | 1 | — | — |
| `tasks` | [`LiveRetentionTaskInfo`](#message-liveretentiontaskinfo) | 2 | repeated | — |
