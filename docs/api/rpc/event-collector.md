# RPC · `event-collector`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/event-collector/rpc/eventcollector.proto` |
| protobuf 包 | `eventcollector.v1` |
| go_package | `go-video/services/event-collector/rpc` |
| 发现用的 etcd key | `eventcollector.v1.rpc`（`services/event-collector/etc/eventcollector.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`eventcollector.v1.rpc`） |
| 监听 | `8152`（`services/event-collector/etc/eventcollector.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_event_collector` |
| 方法数 | 15（service `EventCollector`） |
| 网关消费方 | `admin:EventCollectorRPC` |

## 契约说明

> 说明：event-collector 是用户行为事件（AGENTS.md §7 的 SPM 语义 = 用户行为分析链路）
> 的**采集入口**，只提供 gRPC（无 .api）；对端入口在 gateway/app。
>
> 硬约束（AGENTS.md §5、§7）：
>   - 事件本体**不同步写业务主库**。本服务库 go_video_event_collector 只保存：
>     接收批次（event_ingest_batch）、逐条校验/拒绝结论与去重键（event_record）、
>     采样与脱敏配置版本（event_dispatch_policy）、投递记录与死信（event_record.delivery_* /
>     event_dead_letter）。事件正文经 common/eventenvelope 投递 MQ，由 spm 消费。
>   - 明文 IP、设备号、手机号等不得入库，也不得进入事件 payload：入口处只保留
>     加盐哈希（device_hash）、IP 段（ip_segment，默认 /24）与盐版本，原始值仅在
>     请求生命周期内存在。盐由 `Collector.Privacy.SaltRef` 指向的环境变量/Secret 注入。
>   - 不承载任何广告位、投放、会员、订单、支付、投币、分成字段；本文件出现的
>     `spm`/`trace_position` 只是页面行为链路标识（AGENTS.md §7）。
>
> 投递语义：接收事务只保证「事件已可靠落接收库」，MQ 发送由 internal/dispatcher
> 异步完成（至少一次 + 下游按 event_id 去重）。

## service `EventCollector`

> EventCollector 用户行为事件采集入口（AGENTS.md §7：只服务行为分析，不含广告参数）。

gRPC 方法前缀：`eventcollector.v1.EventCollector/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `CollectEvents` | [`CollectEventsReq`](#message-collecteventsreq) | [`CollectEventsReply`](#message-collecteventsreply) | 客户端 SDK 批量上报（batch_id 幂等，受条数/字节/限流约束）。 |
| 2 | `IngestServerEvents` | [`IngestServerEventsReq`](#message-ingestservereventsreq) | [`IngestServerEventsReply`](#message-ingestservereventsreply) | 服务端内部埋点上报（不采样、trace_id 必填、要求服务身份）。 |
| 3 | `ValidateEventSchema` | [`ValidateEventSchemaReq`](#message-validateeventschemareq) | [`ValidateEventSchemaReply`](#message-validateeventschemareply) | 单事件干跑校验：不落库不投递，返回缺失字段与归一化结果。 |
| 4 | `GetIngestBatch` | [`GetIngestBatchReq`](#message-getingestbatchreq) | [`GetIngestBatchReply`](#message-getingestbatchreply) | 查询批次接收台账。 |
| 5 | `ListIngestBatches` | [`ListIngestBatchesReq`](#message-listingestbatchesreq) | [`ListIngestBatchesReply`](#message-listingestbatchesreply) | 分页查询批次台账。 |
| 6 | `GetEventRecord` | [`GetEventRecordReq`](#message-geteventrecordreq) | [`GetEventRecordReply`](#message-geteventrecordreply) | 查询单条事件的校验结论与投递状态。 |
| 7 | `ListEventRecords` | [`ListEventRecordsReq`](#message-listeventrecordsreq) | [`ListEventRecordsReply`](#message-listeventrecordsreply) | 分页查询事件台账。 |
| 8 | `RetryPendingDelivery` | [`RetryPendingDeliveryReq`](#message-retrypendingdeliveryreq) | [`RetryPendingDeliveryReply`](#message-retrypendingdeliveryreply) | 推进到期未发送事件（cron 兜底任务与运维入口）。 |
| 9 | `ListDeadLetters` | [`ListDeadLettersReq`](#message-listdeadlettersreq) | [`ListDeadLettersReply`](#message-listdeadlettersreply) | 分页查询投递死信。 |
| 10 | `ReplayDeadLetter` | [`ReplayDeadLetterReq`](#message-replaydeadletterreq) | [`ReplayDeadLetterReply`](#message-replaydeadletterreply) | 重放投递死信。 |
| 11 | `UpsertDispatchPolicy` | [`UpsertDispatchPolicyReq`](#message-upsertdispatchpolicyreq) | [`DispatchPolicyReply`](#message-dispatchpolicyreply) | 新建/修改采样与脱敏策略草稿。 |
| 12 | `ActivateDispatchPolicy` | [`ActivateDispatchPolicyReq`](#message-activatedispatchpolicyreq) | [`DispatchPolicyReply`](#message-dispatchpolicyreply) | 切换生效策略版本（旧版本转 ARCHIVED，保留归因）。 |
| 13 | `GetActiveDispatchPolicy` | [`GetActiveDispatchPolicyReq`](#message-getactivedispatchpolicyreq) | [`DispatchPolicyReply`](#message-dispatchpolicyreply) | 查询当前生效策略。 |
| 14 | `ListDispatchPolicies` | [`ListDispatchPoliciesReq`](#message-listdispatchpoliciesreq) | [`ListDispatchPoliciesReply`](#message-listdispatchpoliciesreply) | 分页查询策略版本。 |
| 15 | `GetCollectorHealth` | [`GetCollectorHealthReq`](#message-getcollectorhealthreq) | [`GetCollectorHealthReply`](#message-getcollectorhealthreply) | 采集与投递健康度。 |

## 消息与枚举

### message `EmptyReply`

> 空响应。

（空消息）

### enum `Source`

> 事件来源。

| 值 | 编号 | 说明 |
|---|---|---|
| `SOURCE_UNSPECIFIED` | 0 | — |
| `SOURCE_CLIENT` | 1 | 客户端 SDK 批量上报（经 gateway/app，可采样） |
| `SOURCE_SERVER` | 2 | 服务端内部埋点（engagement/playback/search 等，不采样、需服务身份） |

### enum `Platform`

> 客户端平台（AGENTS.md §1 的四类端 + 服务端自报）。

| 值 | 编号 | 说明 |
|---|---|---|
| `PLATFORM_UNSPECIFIED` | 0 | — |
| `PLATFORM_ANDROID` | 1 | — |
| `PLATFORM_IOS` | 2 | — |
| `PLATFORM_HARMONYOS` | 3 | — |
| `PLATFORM_DESKTOP` | 4 | — |
| `PLATFORM_SERVER` | 5 | Source=SOURCE_SERVER 时的默认值 |

### enum `BehaviorCategory`

> 行为事件类别：决定 event_type 与投递 topic（AGENTS.md §7 列出的链路）。

| 值 | 编号 | 说明 |
|---|---|---|
| `BEHAVIOR_CATEGORY_UNSPECIFIED` | 0 | — |
| `BEHAVIOR_CATEGORY_PLAY` | 1 | 播放开始/恢复：behavior.play.v1 |
| `BEHAVIOR_CATEGORY_CLICK` | 2 | 点击：behavior.click.v1 |
| `BEHAVIOR_CATEGORY_SEARCH` | 3 | 搜索提交/结果点击：behavior.search.v1 |
| `BEHAVIOR_CATEGORY_SKIP` | 4 | 跳过/关闭/不感兴趣：behavior.skip.v1 |
| `BEHAVIOR_CATEGORY_LIKE` | 5 | 点赞：behavior.like.v1 |
| `BEHAVIOR_CATEGORY_FAVORITE` | 6 | 收藏：behavior.favorite.v1 |
| `BEHAVIOR_CATEGORY_FOLLOW` | 7 | 关注：behavior.follow.v1 |
| `BEHAVIOR_CATEGORY_SHARE` | 8 | 分享：behavior.share.v1 |
| `BEHAVIOR_CATEGORY_QUALITY` | 9 | 播放质量（卡顿、首帧、错误码）：behavior.quality.v1 |
| `BEHAVIOR_CATEGORY_EXPOSURE` | 10 | 曝光：behavior.exposure.v1 |

### enum `EventDecision`

> 单条事件的处理结论。

| 值 | 编号 | 说明 |
|---|---|---|
| `EVENT_DECISION_UNSPECIFIED` | 0 | — |
| `EVENT_DECISION_ACCEPTED` | 1 | 通过校验，已落接收库并等待投递 |
| `EVENT_DECISION_DUPLICATED` | 2 | event_id 已存在，不重复落库、不重复投递 |
| `EVENT_DECISION_REJECTED` | 3 | 校验失败，按 reason 记录拒绝原因 |
| `EVENT_DECISION_SAMPLED_OUT` | 4 | 命中采样规则，主动丢弃（不落投递队列） |
| `EVENT_DECISION_DEFERRED` | 5 | 批次被限流/降级，整批未落库，客户端需稍后重试 |

### enum `RejectReason`

> 拒绝或丢弃原因（稳定枚举，客户端与埋点方据此修正，不做人读拼接）。

| 值 | 编号 | 说明 |
|---|---|---|
| `REJECT_REASON_UNSPECIFIED` | 0 | — |
| `REJECT_REASON_NONE` | 1 | 无问题 |
| `REJECT_MISSING_EVENT_ID` | 2 | event_id 缺失 |
| `REJECT_MISSING_EVENT_TYPE` | 3 | event_type 缺失且无法由 category 推导 |
| `REJECT_UNSUPPORTED_EVENT_TYPE` | 4 | event_type 不在白名单 |
| `REJECT_MISSING_SCHEMA_VERSION` | 5 | schema_version 缺失 |
| `REJECT_UNSUPPORTED_SCHEMA_VERSION` | 6 | schema_version 高于服务端支持版本 |
| `REJECT_MISSING_OCCURRED_AT` | 7 | occurred_at 缺失 |
| `REJECT_CLOCK_SKEW` | 8 | occurred_at 与服务器时间偏差超过阈值（明显时间漂移） |
| `REJECT_TIME_IN_FUTURE` | 9 | occurred_at 在未来 |
| `REJECT_MISSING_TRACE_ID` | 10 | trace_id 缺失（服务端来源必填） |
| `REJECT_MISSING_CONTENT_KEY` | 11 | 内容主键（content_id/vid/aid/session_id）全缺 |
| `REJECT_MISSING_SUBJECT` | 12 | 未登录且无设备标识，事件无法归属 |
| `REJECT_EVENT_TOO_OLD` | 13 | 超过允许的回补窗口 |
| `REJECT_PRIVACY_FIELD` | 14 | payload 含禁止入库字段（明文 IP/设备号/手机号等） |
| `REJECT_PAYLOAD_TOO_LARGE` | 15 | 单事件 payload 超限 |
| `REJECT_INVALID_PAYLOAD` | 16 | payload 不是合法 JSON 对象 |
| `REJECT_INVALID_METRIC` | 17 | 数值指标越界（负数、位置大于时长等） |
| `REJECT_RATE_LIMITED` | 18 | 触发 mid/设备/IP 段限流 |
| `REJECT_BATCH_TOO_LARGE` | 19 | 单请求条数或字节超限 |
| `REJECT_BATCH_ID_MISSING` | 20 | batch_id 缺失，无法做批次幂等 |
| `REJECT_SOURCE_NOT_ALLOWED` | 21 | 来源与方法不匹配（客户端方法收到 SOURCE_SERVER 等） |
| `REJECT_SAMPLING_POLICY_STALE` | 22 | 客户端声明的策略版本无法解析，退回服务端默认策略 |
| `REJECT_STORE_FAILED` | 23 | 接收库写入失败（DB 故障，整批可重试） |
| `REJECT_INTERNAL` | 24 | 其他内部错误，细节只进日志与 trace |

### enum `DeliveryState`

> 投递状态（event_record.delivery_state）。

| 值 | 编号 | 说明 |
|---|---|---|
| `DELIVERY_STATE_UNSPECIFIED` | 0 | — |
| `DELIVERY_STATE_NONE` | 1 | 不投递（被采样丢弃或重复计数） |
| `DELIVERY_STATE_PENDING` | 2 | 待投递 |
| `DELIVERY_STATE_SENT` | 3 | 已投递到 MQ |
| `DELIVERY_STATE_RETRYING` | 4 | 投递失败，退避中 |
| `DELIVERY_STATE_DEAD` | 5 | 超过最大次数，已转 event_dead_letter |

### enum `BatchState`

> 批次接收状态（event_ingest_batch.state）。

| 值 | 编号 | 说明 |
|---|---|---|
| `BATCH_STATE_UNSPECIFIED` | 0 | — |
| `BATCH_STATE_RECEIVED` | 1 | 已落库，尚未完成校验 |
| `BATCH_STATE_VALIDATED` | 2 | 校验完成，待投递或已投递 |
| `BATCH_STATE_DISPATCHING` | 3 | 投递中 |
| `BATCH_STATE_DISPATCHED` | 4 | 全部待投递事件已发送 |
| `BATCH_STATE_PARTIAL` | 5 | 部分事件投递失败（退避或死信中） |
| `BATCH_STATE_REJECTED` | 6 | 整批被拒（大小/来源/限流） |

### enum `PolicyState`

> 采集与投递策略状态（event_dispatch_policy.state）。

| 值 | 编号 | 说明 |
|---|---|---|
| `POLICY_STATE_UNSPECIFIED` | 0 | — |
| `POLICY_STATE_DRAFT` | 1 | 草稿：可修改，不生效 |
| `POLICY_STATE_ACTIVE` | 2 | 生效中：同一时刻只有一版 ACTIVE |
| `POLICY_STATE_ARCHIVED` | 3 | 已下线：只读，历史批次的归因依据 |

### message `EventContext`

> EventContext 一次批量上报共享的上下文（避免每条事件重复携带、压扁请求体）。 / 注意：本消息里的 device_id / ip 是**明文入参**，服务端只做加盐哈希与脱敏， / 绝不写入数据库，也不会出现在投递到 MQ 的信封里。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 登录用户 mid，0 表示未登录 |
| `device_id` | `string` | 2 | — | 客户端设备号明文（idfa/oaid/harmony odid/桌面机器码） |
| `device_type` | `string` | 3 | — | 设备号类型：idfa/oaid/odid/machine_id/... |
| `device_hash` | `string` | 4 | — | 服务端回填：sha256(salt_version, device_id)，只读 |
| `ip` | `string` | 5 | — | 出口 IP 明文（网关透传），只用于哈希与 IP 段 |
| `ip_segment` | `string` | 6 | — | 服务端回填：脱敏 IP 段，如 203.0.113.0/24 |
| `platform` | [`Platform`](#enum-platform) | 7 | — | — |
| `app_id` | `string` | 8 | — | 应用标识（区分多产品） |
| `app_version` | `string` | 9 | — | 客户端版本 |
| `sdk_version` | `string` | 10 | — | 埋点 SDK 版本 |
| `os_version` | `string` | 11 | — | 系统版本 |
| `network_type` | `string` | 12 | — | wifi/2g/3g/4g/5g/unknown |
| `model` | `string` | 13 | — | 机型（不含序列号） |
| `region` | `string` | 14 | — | 地区代码，与版权地域判断无关，只作行为维度 |
| `session_id` | `string` | 15 | — | 客户端会话 ID（不是 playback_session 主键时也允许） |
| `page` | `string` | 16 | — | 页面标识 |
| `spm` | `string` | 17 | — | 行为链路标识（AGENTS.md §7：仅用户行为分析，非广告位） |

### message `BehaviorEvent`

> BehaviorEvent 单条行为事件。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `event_id` | `string` | 1 | — | 必填：全局唯一（客户端建议用 ULID/UUID），服务端按它去重 |
| `event_type` | `string` | 2 | — | 建议显式给出 behavior.<category>；为空时由 category 推导 |
| `schema_version` | `int32` | 3 | — | 必填：事件 payload 结构版本，>=1 |
| `occurred_at` | `int64` | 4 | — | 必填：事件发生时间（Unix 秒，客户端本地时钟） |
| `reported_at` | `int64` | 5 | — | 客户端产生上报的时刻（Unix 秒，用于离线回补识别） |
| `category` | [`BehaviorCategory`](#enum-behaviorcategory) | 6 | — | — |
| `trace_id` | `string` | 7 | — | 链路 ID：SOURCE_SERVER 必填，SOURCE_CLIENT 缺失时服务端补 |
| `content_type` | `string` | 8 | — | 内容主类型：ugc/pgc/live/keyword/... 与内容主键配套 |
| `content_id` | `int64` | 9 | — | 内容 ID（catalog 作品/集或稿件 ID） |
| `aid` | `int64` | 10 | — | 稿件 aid（UGC 场景） |
| `vid` | `string` | 11 | — | 稿件 vid（UGC 场景） |
| `target_mid` | `int64` | 12 | — | 行为对象用户（关注/分享对象），0 表示无 |
| `session_id` | `string` | 13 | — | 播放/会话主键，quality/play 场景用于串联 |
| `position_ms` | `int64` | 14 | — | 播放进度 |
| `duration_ms` | `int64` | 15 | — | 内容总时长 |
| `buffer_count` | `int32` | 16 | — | 卡顿次数（quality） |
| `first_frame_ms` | `int64` | 17 | — | 首帧耗时（quality） |
| `avg_bitrate` | `int32` | 18 | — | 平均码率 bps（quality） |
| `error_code` | `string` | 19 | — | 播放错误码（quality），稳定枚举字符串 |
| `keyword` | `string` | 20 | — | 搜索词（search 场景，已按长度截断，不入库明文超长子串） |
| `result_index` | `int32` | 21 | — | 点击/跳过在结果列表中的位次（0 表示不适用） |
| `target_url` | `string` | 22 | — | 跳转目标标识（客户端自定义 scheme/路由，不是外部 URL 明文） |
| `payload` | `string` | 23 | — | 扩展 JSON 文本：只允许白名单字段，受大小限制，禁止敏感明文 |

### message `EventResult`

> EventResult 单条事件的处理结论（与请求中的顺序一致，用 index 对齐）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `index` | `int32` | 1 | — | 对应请求 events 下标，从 0 开始 |
| `event_id` | `string` | 2 | — | 归一化后的事件 ID（服务端补齐时返回真值） |
| `decision` | [`EventDecision`](#enum-eventdecision) | 3 | — | — |
| `reason` | [`RejectReason`](#enum-rejectreason) | 4 | — | decision=REJECTED/SAMPLED_OUT/DEFERRED 时的原因 |
| `message` | `string` | 5 | — | 人读提示，已脱敏，不含 payload 原文 |
| `topic` | `string` | 6 | — | 决定投递的目标 topic（未投递时为空） |
| `schema_version` | `int32` | 7 | — | 服务端采用的 schema 版本 |

### message `CollectEventsReq`

> --- 批量采集 --- / CollectEventsReq 客户端 SDK 批量上报。 / 幂等：batch_id 由客户端生成并保证唯一，重复上报返回首次结果（deduplicated=true）， / 单请求受 MaxEventsPerBatch 与 MaxRequestBytes 双重限制。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `batch_id` | `string` | 1 | — | 必填：批次幂等键 |
| `source` | [`Source`](#enum-source) | 2 | — | 未指定按 SOURCE_CLIENT 处理 |
| `context` | [`EventContext`](#message-eventcontext) | 3 | — | 必填：批次共享上下文 |
| `events` | [`BehaviorEvent`](#message-behaviorevent) | 4 | repeated | 必填：至少 1 条 |
| `policy_version` | `string` | 5 | — | 客户端缓存的策略版本，空表示用服务端 ACTIVE 版本 |
| `client_seq` | `int32` | 6 | — | 客户端自增序号，用于识别乱序/重发批次 |
| `request_id` | `string` | 7 | — | 传输层请求 ID，排障用 |

### message `CollectEventsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `batch_id` | `string` | 1 | — | — |
| `state` | [`BatchState`](#enum-batchstate) | 2 | — | — |
| `total` | `int32` | 3 | — | — |
| `accepted` | `int32` | 4 | — | — |
| `duplicated` | `int32` | 5 | — | — |
| `rejected` | `int32` | 6 | — | — |
| `sampled_out` | `int32` | 7 | — | — |
| `results` | [`EventResult`](#message-eventresult) | 8 | repeated | — |
| `policy_version` | `string` | 9 | — | 本批实际采用的采样/脱敏版本（落库字段） |
| `next_report_interval_seconds` | `int32` | 10 | — | 建议客户端下次上报间隔（降级/限流时放大） |
| `retry_after_ms` | `int32` | 11 | — | decision=DEFERRED/REJECT_RATE_LIMITED 时的退避提示 |
| `degraded` | `bool` | 12 | — | true 表示服务端处于降级状态（限流或依赖故障） |
| `server_time` | `int64` | 13 | — | 供客户端校准时钟偏移，避免系统性时间漂移 |

### message `IngestServerEventsReq`

> IngestServerEventsReq 服务端内部埋点入口（engagement/playback/search 等）。 / 与客户端入口的差异：不做采样、trace_id 必填、要求调用方服务身份， / 且 idempotency_key 必填（跨进程重试常见）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `batch_id` | `string` | 1 | — | 必填：批次幂等键 |
| `caller_service` | `string` | 2 | — | 必填：来源服务名（如 engagement），作为信封 producer |
| `idempotency_key` | `string` | 3 | — | 必填：同一次动作唯一 |
| `context` | [`EventContext`](#message-eventcontext) | 4 | — | 可只填 mid/platform=PLATFORM_SERVER |
| `events` | [`BehaviorEvent`](#message-behaviorevent) | 5 | repeated | — |
| `trace_id` | `string` | 6 | — | 批次级 trace_id，事件未自带时继承 |

### message `IngestServerEventsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `batch_id` | `string` | 1 | — | — |
| `state` | [`BatchState`](#enum-batchstate) | 2 | — | — |
| `total` | `int32` | 3 | — | — |
| `accepted` | `int32` | 4 | — | — |
| `duplicated` | `int32` | 5 | — | — |
| `rejected` | `int32` | 6 | — | — |
| `results` | [`EventResult`](#message-eventresult) | 7 | repeated | — |
| `policy_version` | `string` | 8 | — | — |
| `retry_after_ms` | `int32` | 9 | — | — |
| `degraded` | `bool` | 10 | — | — |

### message `ValidateEventSchemaReq`

> ValidateEventSchemaReq 单事件干跑校验：不落库、不投递，供网关与埋点方自检。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `source` | [`Source`](#enum-source) | 1 | — | — |
| `event` | [`BehaviorEvent`](#message-behaviorevent) | 2 | — | — |
| `context` | [`EventContext`](#message-eventcontext) | 3 | — | 可空：用于校验「内容主键 + 归属主体」是否齐备 |

### message `ValidateEventSchemaReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `valid` | `bool` | 1 | — | — |
| `decision` | [`EventDecision`](#enum-eventdecision) | 2 | — | — |
| `reason` | [`RejectReason`](#enum-rejectreason) | 3 | — | — |
| `missing_fields` | `string` | 4 | repeated | 缺失的必填字段名，便于客户端定位 |
| `event_type` | `string` | 5 | — | 归一化后的 event_type |
| `topic` | `string` | 6 | — | 归一化后的投递 topic |
| `schema_version` | `int32` | 7 | — | — |
| `note` | `string` | 8 | — | 非致命提示（如字段被截断、被脱敏） |

### message `IngestBatch`

> --- 批次与事件记录查询 --- / IngestBatch 接收批次投影（不含任何事件明文）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `batch_id` | `string` | 2 | — | — |
| `source` | [`Source`](#enum-source) | 3 | — | — |
| `caller_service` | `string` | 4 | — | — |
| `platform` | [`Platform`](#enum-platform) | 5 | — | — |
| `app_id` | `string` | 6 | — | — |
| `app_version` | `string` | 7 | — | — |
| `sdk_version` | `string` | 8 | — | — |
| `mid` | `int64` | 9 | — | — |
| `device_hash` | `string` | 10 | — | 加盐哈希，非明文 |
| `ip_segment` | `string` | 11 | — | 脱敏 IP 段，非明文 |
| `salt_version` | `int32` | 12 | — | 本次使用的盐版本，轮换后可重算 |
| `policy_version` | `string` | 13 | — | 采样/脱敏配置版本 |
| `total` | `int32` | 14 | — | — |
| `accepted` | `int32` | 15 | — | — |
| `duplicated` | `int32` | 16 | — | — |
| `rejected` | `int32` | 17 | — | — |
| `sampled_out` | `int32` | 18 | — | — |
| `dispatched` | `int32` | 19 | — | — |
| `dead` | `int32` | 20 | — | — |
| `request_bytes` | `int64` | 21 | — | — |
| `state` | [`BatchState`](#enum-batchstate) | 22 | — | — |
| `top_reason` | [`RejectReason`](#enum-rejectreason) | 23 | — | 整批最主要的拒绝原因（无问题为 NONE） |
| `last_error` | `string` | 24 | — | — |
| `trace_id` | `string` | 25 | — | — |
| `received_at` | `int64` | 26 | — | — |
| `finished_at` | `int64` | 27 | — | — |
| `ctime` | `int64` | 28 | — | — |
| `mtime` | `int64` | 29 | — | — |

### message `GetIngestBatchReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `batch_id` | `string` | 1 | — | — |

### message `GetIngestBatchReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `batch` | [`IngestBatch`](#message-ingestbatch) | 1 | — | — |
| `found` | `bool` | 2 | — | — |

### message `ListIngestBatchesReq`

> ListIngestBatchesReq 批次分页（按 (ctime, id) 倒序游标）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `source` | [`Source`](#enum-source) | 1 | — | — |
| `state` | [`BatchState`](#enum-batchstate) | 2 | — | — |
| `mid` | `int64` | 3 | — | — |
| `device_hash` | `string` | 4 | — | — |
| `ip_segment` | `string` | 5 | — | — |
| `ctime_from` | `int64` | 6 | — | — |
| `ctime_to` | `int64` | 7 | — | — |
| `cursor` | `string` | 8 | — | — |
| `page_size` | `int32` | 9 | — | — |

### message `ListIngestBatchesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`IngestBatch`](#message-ingestbatch) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | — |

### message `EventRecord`

> EventRecord 单条事件的接收与投递记录（event_record 投影，payload 只存摘要）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `event_id` | `string` | 2 | — | — |
| `batch_id` | `string` | 3 | — | — |
| `event_type` | `string` | 4 | — | — |
| `category` | [`BehaviorCategory`](#enum-behaviorcategory) | 5 | — | — |
| `schema_version` | `int32` | 6 | — | — |
| `occurred_at` | `int64` | 7 | — | — |
| `received_at` | `int64` | 8 | — | — |
| `clock_skew_seconds` | `int64` | 9 | — | occurred_at 与服务器时间的偏差，用于识别时钟漂移 |
| `decision` | [`EventDecision`](#enum-eventdecision) | 10 | — | — |
| `reason` | [`RejectReason`](#enum-rejectreason) | 11 | — | — |
| `reason_detail` | `string` | 12 | — | 已脱敏的补充说明 |
| `delivery_state` | [`DeliveryState`](#enum-deliverystate) | 13 | — | — |
| `topic` | `string` | 14 | — | — |
| `envelope_event_id` | `string` | 15 | — | 投递信封的 event_id（与入参 event_id 分开记账） |
| `delivery_attempts` | `int32` | 16 | — | — |
| `next_retry_at` | `int64` | 17 | — | — |
| `last_error` | `string` | 18 | — | — |
| `mid` | `int64` | 19 | — | — |
| `device_hash` | `string` | 20 | — | — |
| `ip_segment` | `string` | 21 | — | — |
| `salt_version` | `int32` | 22 | — | — |
| `content_type` | `string` | 23 | — | — |
| `content_id` | `int64` | 24 | — | — |
| `vid` | `string` | 25 | — | — |
| `target_mid` | `int64` | 26 | — | — |
| `payload_digest` | `string` | 27 | — | sha256:<hex>，原文不入库 |
| `payload_bytes` | `int32` | 28 | — | — |
| `sanitize_version` | `string` | 29 | — | 实际生效的脱敏规则版本 |
| `policy_version` | `string` | 30 | — | — |
| `trace_id` | `string` | 31 | — | — |
| `ctime` | `int64` | 32 | — | — |
| `mtime` | `int64` | 33 | — | — |

### message `GetEventRecordReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `event_id` | `string` | 1 | — | — |

### message `GetEventRecordReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `record` | [`EventRecord`](#message-eventrecord) | 1 | — | — |
| `found` | `bool` | 2 | — | — |

### message `ListEventRecordsReq`

> ListEventRecordsReq 事件记录分页（按 (ctime, id) 倒序游标）。 / 说明：本表是接收与投递台账，不是行为事实表；行为分析请消费 MQ topic。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `batch_id` | `string` | 1 | — | — |
| `event_type` | `string` | 2 | — | — |
| `category` | [`BehaviorCategory`](#enum-behaviorcategory) | 3 | — | — |
| `decision` | [`EventDecision`](#enum-eventdecision) | 4 | — | — |
| `reason` | [`RejectReason`](#enum-rejectreason) | 5 | — | — |
| `delivery_state` | [`DeliveryState`](#enum-deliverystate) | 6 | — | — |
| `topic` | `string` | 7 | — | — |
| `mid` | `int64` | 8 | — | — |
| `device_hash` | `string` | 9 | — | — |
| `ctime_from` | `int64` | 10 | — | — |
| `ctime_to` | `int64` | 11 | — | — |
| `cursor` | `string` | 12 | — | — |
| `page_size` | `int32` | 13 | — | — |

### message `ListEventRecordsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`EventRecord`](#message-eventrecord) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | — |

### message `RetryPendingDeliveryReq`

> --- 投递推进 --- / RetryPendingDeliveryReq 推进到期未发送的事件（由 services/cron 的投递兜底任务调用， / 也可运维手工触发）。limit 与 topic 双重约束，避免一次扫全表。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic` | `string` | 1 | — | 空表示全部 topic |
| `now` | `int64` | 2 | — | 0 表示服务端当前时间 |
| `limit` | `int32` | 3 | — | 0 表示服务端默认 Dispatch.BatchSize |
| `idempotency_key` | `string` | 4 | — | 必填：防止重复提交同一轮推进 |
| `operator` | `string` | 5 | — | — |

### message `RetryPendingDeliveryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scanned` | `int32` | 1 | — | — |
| `sent` | `int32` | 2 | — | — |
| `retrying` | `int32` | 3 | — | — |
| `dead` | `int32` | 4 | — | — |
| `next_run_at` | `int64` | 5 | — | 建议下一轮执行时间 |

### message `DeadLetter`

> DeadLetter 投递死信（超过最大重试次数的事件摘要，原文不入库）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `event_id` | `string` | 2 | — | — |
| `batch_id` | `string` | 3 | — | — |
| `event_type` | `string` | 4 | — | — |
| `topic` | `string` | 5 | — | — |
| `payload_digest` | `string` | 6 | — | — |
| `reason` | `string` | 7 | — | — |
| `attempts` | `int32` | 8 | — | — |
| `state` | `string` | 9 | — | open/replayed/discarded |
| `created_at` | `int64` | 10 | — | — |
| `handled_at` | `int64` | 11 | — | — |
| `operator` | `string` | 12 | — | — |

### message `ListDeadLettersReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic` | `string` | 1 | — | — |
| `state` | `string` | 2 | — | 空表示全部 |
| `ctime_from` | `int64` | 3 | — | — |
| `ctime_to` | `int64` | 4 | — | — |
| `cursor` | `string` | 5 | — | — |
| `page_size` | `int32` | 6 | — | — |

### message `ListDeadLettersReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`DeadLetter`](#message-deadletter) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | — |

### message `ReplayDeadLetterReq`

> ReplayDeadLetterReq 重放死信：重新入投递队列，不重新校验采样。 / 幂等：idempotency_key + 单条 event_id 唯一约束，重复重放返回首次结果。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `dead_letter_ids` | `int64` | 1 | repeated | 单次上限 MaxReplayPerRequest |
| `idempotency_key` | `string` | 2 | — | — |
| `operator` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | 必填：为什么重放 |

### message `ReplayDeadLetterReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `replayed` | `int32` | 1 | — | — |
| `skipped` | `int32` | 2 | — | 已是 replayed/discarded 状态被跳过的数量 |
| `failed_ids` | `int64` | 3 | repeated | 重放时重新入队失败的死信 ID |

### message `SampleRule`

> --- 采样与脱敏配置版本 --- / SampleRule 单类事件的采样比例（基点，10000 = 全量）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `event_type` | `string` | 1 | — | behavior.play 等；"*" 表示兜底规则 |
| `sample_bps` | `int32` | 2 | — | 0..10000 |
| `quality_events` | `bool` | 3 | — | true 表示该规则只作用于播放质量类事件（问题排查期通常全量） |

### message `DispatchPolicy`

> DispatchPolicy 采样与脱敏配置版本（event_dispatch_policy）。 / 每次采集都把生效的 version 写进批次与事件记录，保证事后能重放归因； / 密钥、盐值本身不入库，只写 SaltRef 指向的环境变量名。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `version` | `string` | 1 | — | 语义化版本，如 2026.09.20-1，唯一 |
| `state` | [`PolicyState`](#enum-policystate) | 2 | — | — |
| `sample_rules` | [`SampleRule`](#message-samplerule) | 3 | repeated | — |
| `salt_version` | `int32` | 4 | — | 脱敏哈希盐版本，轮换后旧数据不可逆推 |
| `salt_ref` | `string` | 5 | — | 取盐的环境变量名（如 EVENT_COLLECTOR_SALT_V2），不含值 |
| `field_whitelist` | `string` | 6 | repeated | payload 允许保留的字段名 |
| `drop_fields` | `string` | 7 | repeated | 明确禁止入库的字段名（明文 ip/imei/phone/token 等） |
| `max_events_per_batch` | `int32` | 8 | — | — |
| `max_request_bytes` | `int64` | 9 | — | — |
| `max_event_payload_bytes` | `int32` | 10 | — | — |
| `max_clock_skew_seconds` | `int32` | 11 | — | 允许的未来/过去偏移绝对值 |
| `max_backfill_seconds` | `int32` | 12 | — | 允许的回补窗口（occurred_at 过旧即拒绝） |
| `keyword_max_runes` | `int32` | 13 | — | 搜索词截断长度 |
| `retention_days` | `int32` | 14 | — | 接收台账保留天数，超期由 cron 清理 |
| `deliver_max_attempts` | `int32` | 15 | — | 投递重试上限，超过转死信 |
| `retry_base_seconds` | `int64` | 16 | — | — |
| `retry_max_seconds` | `int64` | 17 | — | — |
| `note` | `string` | 18 | — | — |
| `operator` | `string` | 19 | — | — |
| `ctime` | `int64` | 20 | — | — |
| `mtime` | `int64` | 21 | — | — |

### message `UpsertDispatchPolicyReq`

> UpsertDispatchPolicyReq 新建/修改草稿版本（ACTIVE 版本不可原地改，只能新建）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `policy` | [`DispatchPolicy`](#message-dispatchpolicy) | 1 | — | — |
| `idempotency_key` | `string` | 2 | — | — |
| `operator` | `string` | 3 | — | — |

### message `DispatchPolicyReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `policy` | [`DispatchPolicy`](#message-dispatchpolicy) | 1 | — | — |
| `created` | `bool` | 2 | — | — |

### message `ActivateDispatchPolicyReq`

> ActivateDispatchPolicyReq 切换生效版本。 / expected_current_version 非空时做乐观校验，避免并发误切；旧 ACTIVE 自动转 ARCHIVED。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `version` | `string` | 1 | — | — |
| `expected_current_version` | `string` | 2 | — | — |
| `idempotency_key` | `string` | 3 | — | — |
| `operator` | `string` | 4 | — | — |
| `reason` | `string` | 5 | — | — |

### message `GetActiveDispatchPolicyReq`

（空消息）

### message `ListDispatchPoliciesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | [`PolicyState`](#enum-policystate) | 1 | — | — |
| `cursor` | `string` | 2 | — | — |
| `page_size` | `int32` | 3 | — | — |

### message `ListDispatchPoliciesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`DispatchPolicy`](#message-dispatchpolicy) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | — |

### message `TopicHealth`

> --- 健康度 --- / TopicHealth 单个 topic 的投递积压视图。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic` | `string` | 1 | — | — |
| `pending` | `int64` | 2 | — | — |
| `retrying` | `int64` | 3 | — | — |
| `dead_open` | `int64` | 4 | — | — |
| `sent_last_hour` | `int64` | 5 | — | — |
| `oldest_pending_ctime` | `int64` | 6 | — | 0 表示无积压 |

### message `GetCollectorHealthReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `now` | `int64` | 1 | — | — |

### message `GetCollectorHealthReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `server_time` | `int64` | 1 | — | — |
| `topics` | [`TopicHealth`](#message-topichealth) | 2 | repeated | — |
| `batches_rejected_last_hour` | `int64` | 3 | — | — |
| `rate_limited_last_hour` | `int64` | 4 | — | 触发限流的请求数（mid/设备/IP 段维度合计） |
| `active_salt_version` | `int32` | 5 | — | 生效策略的盐版本，0 表示策略缺失（应视为不健康） |
| `policy_version` | `string` | 6 | — | — |
| `salt_ref` | `string` | 7 | — | 只报告变量名，不报告盐值 |
| `salt_available` | `bool` | 8 | — | 盐是否可从 Secret 读到（缺失时隐私脱敏无法执行） |
| `version` | `string` | 9 | — | 服务构建版本 |
