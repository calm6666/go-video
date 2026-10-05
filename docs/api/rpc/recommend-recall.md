# RPC · `recommend-recall`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/recommend-recall/rpc/recall.proto` |
| protobuf 包 | `recommendrecall.v1` |
| go_package | `go-video/services/recommend-recall/rpc` |
| 发现用的 etcd key | `recommendrecall.v1.rpc`（`services/recommend-recall/etc/recommendrecall.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`recommendrecall.v1.rpc`） |
| 监听 | `8116`（`services/recommend-recall/etc/recommendrecall.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_recommend_recall` |
| 方法数 | 10（service `Recall`） |
| 网关消费方 | `admin:RecommendRecallRPC` |

## 契约说明

> recommend-recall：多路召回领域服务（热门池 / 关注池 / 标签池 / 协同候选 / 向量候选）。
>
> 契约边界（AGENTS.md）：
>   - §7：召回只消费特征与池投影做候选，不提供广告位参数、不投放广告、不做商业化报表；
>     因此本契约**没有**任何"把某个 aid 手工顶到前面 / 加权 / 屏蔽某竞品"的写接口。
>     运营干预入口不在本服务（内容池准入由 operation + moderation 决定，见 README 待接线清单）。
>   - §5：候选只回传跨服务主键（aid）+ 来源标记 + 分数，不复制稿件、用户、媒资主数据；
>     稿件可见性、关系、已看事实分别归 video / social-graph / spm，本服务只读不写。
>   - §6：本服务是纯 gRPC 领域服务，面向端的响应聚合与响应信封由 gateway/app 负责。
>
> 可追溯性（本契约的核心设计）：
>   - 每个池由 (source, pool_key) 唯一寻址，pool_key 语法见 PoolRef 注释；
>   - 池条目按 version 分批写入，版本行登记 batch_id（生成批次）与 generator（产出方），
>     recall_pool_current 保存当前生效版本指针，切换/回滚都留审计；
>   - 在线召回结果逐条回带 pool_version + batch_id + snapshot_id，
>     并用 request_id 落在 recall_request_log，可用 GetRecallRequestLog 完整回放。
>
> 在线面红线：所有读方法都有条数上限；降级必须显式（DegradationInfo.degraded），
> 绝不返回"看起来正常但其实是空池"的成功。

## service `Recall`

> Recall 召回服务。

gRPC 方法前缀：`recommendrecall.v1.Recall/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `RecallCandidates` | [`RecallCandidatesReq`](#message-recallcandidatesreq) | [`RecallCandidatesReply`](#message-recallcandidatesreply) | 多路召回主入口：按路取数 -> 过滤 -> 去重合并 -> 显式降级声明 |
| 2 | `GetPoolSnapshot` | [`GetPoolSnapshotReq`](#message-getpoolsnapshotreq) | [`GetPoolSnapshotReply`](#message-getpoolsnapshotreply) | 读某个池的某个版本条目（运维/排障，分页有上限） |
| 3 | `ListPoolVersions` | [`ListPoolVersionsReq`](#message-listpoolversionsreq) | [`ListPoolVersionsReply`](#message-listpoolversionsreply) | 列出版本与生成批次（可追溯性） |
| 4 | `GetRecallRequestLog` | [`GetRecallRequestLogReq`](#message-getrecallrequestlogreq) | [`GetRecallRequestLogReply`](#message-getrecallrequestlogreply) | 回放一次在线召回请求（按 request_id 或 snapshot_id） |
| 5 | `ListRecallRequestLogs` | [`ListRecallRequestLogsReq`](#message-listrecallrequestlogsreq) | [`ListRecallRequestLogsReply`](#message-listrecallrequestlogsreply) | 分页查召回请求日志（运营/排障） |
| 6 | `UpsertPoolItems` | [`UpsertPoolItemsReq`](#message-upsertpoolitemsreq) | [`UpsertPoolItemsReply`](#message-upsertpoolitemsreply) | 分批写入池条目到指定版本（idempotency_key 幂等） |
| 7 | `PublishPoolVersion` | [`PublishPoolVersionReq`](#message-publishpoolversionreq) | [`PublishPoolVersionReply`](#message-publishpoolversionreply) | 原子切换池的当前生效版本（写审计 + 发事件） |
| 8 | `RollbackPoolVersion` | [`RollbackPoolVersionReq`](#message-rollbackpoolversionreq) | [`RollbackPoolVersionReply`](#message-rollbackpoolversionreply) | 回滚到历史版本（运营回滚开关） |
| 9 | `PrunePoolVersions` | [`PrunePoolVersionsReq`](#message-prunepoolversionsreq) | [`PrunePoolVersionsReply`](#message-prunepoolversionsreply) | 分批清理过期版本（由 services/cron 调用） |
| 10 | `GetRecallConfig` | [`GetRecallConfigReq`](#message-getrecallconfigreq) | [`GetRecallConfigReply`](#message-getrecallconfigreply) | 下发在线召回参数与池健康摘要 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `Platform`

> 客户端平台（AGENTS.md §1/§6：Android、iOS、HarmonyOS、电脑客户端；不支持小程序）。 / 编号与 playback/risk-control 等同仓契约保持一致，但本契约不 import 它们（proto 自包含）。

| 值 | 编号 | 说明 |
|---|---|---|
| `PLATFORM_UNSPECIFIED` | 0 | 未指定 |
| `PLATFORM_ANDROID` | 1 | Android |
| `PLATFORM_IOS` | 2 | iOS |
| `PLATFORM_HARMONY` | 3 | HarmonyOS |
| `PLATFORM_DESKTOP` | 4 | 电脑客户端 |

### enum `Source`

> 召回路 / 池类型。同一个枚举既用于池寻址（PoolRef.source），也用于候选来源标记， / 保证池与候选用同一套词汇，不出现两套编号需要翻译（本仓 search-query 的反面教训）。

| 值 | 编号 | 说明 |
|---|---|---|
| `SOURCE_UNSPECIFIED` | 0 | 未指定 |
| `SOURCE_HOT` | 1 | 热门池（全局或分区） |
| `SOURCE_FOLLOW` | 2 | 关注池（关注作者近期稿件） |
| `SOURCE_TAG` | 3 | 标签/分区池 |
| `SOURCE_COLLAB` | 4 | 协同过滤候选（i2i） |
| `SOURCE_VECTOR` | 5 | 向量近邻候选（u2i / i2i） |
| `SOURCE_COLD` | 6 | 冷启动池（新用户/未登录/无行为） |

### enum `PoolVersionState`

> 池版本状态（与 model.recall_pool_version.state 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `POOL_VERSION_STATE_UNSPECIFIED` | 0 | 未指定 |
| `POOL_VERSION_STATE_BUILDING` | 1 | 写入中，不可被在线读 |
| `POOL_VERSION_STATE_READY` | 2 | 写入完成并通过校验，等待切换 |
| `POOL_VERSION_STATE_CURRENT` | 3 | 当前生效版本（每个池至多一个） |
| `POOL_VERSION_STATE_RETIRED` | 4 | 已退役（保留供回滚与回放） |
| `POOL_VERSION_STATE_FAILED` | 5 | 生成失败（不在线出数） |

### enum `DegradeReason`

> 降级原因（与 model.DegradeReason* 一致）。0 表示未降级。

| 值 | 编号 | 说明 |
|---|---|---|
| `DEGRADE_REASON_UNSPECIFIED` | 0 | 未降级（degraded=false 时恒为此值） |
| `DEGRADE_REASON_POOL_NOT_READY` | 1 | 目标池没有 CURRENT 版本或版本已过期 |
| `DEGRADE_REASON_FEATURE_UNAVAILABLE` | 2 | 特征/行为下游不可读（spm、feature-store） |
| `DEGRADE_REASON_DOWNSTREAM_TIMEOUT` | 3 | 下游 RPC 超时或被熔断 |
| `DEGRADE_REASON_STORE_UNAVAILABLE` | 4 | Redis/MySQL 不可用 |
| `DEGRADE_REASON_BUDGET_EXHAUSTED` | 5 | 本服务内部时间预算耗尽，裁剪剩余召回路 |
| `DEGRADE_REASON_COLD_START` | 6 | 冷启动走保底池（非故障，但需显式声明） |
| `DEGRADE_REASON_ALL_SOURCES_EMPTY` | 7 | 所有请求路都无候选（回兜底池或返回空并声明） |

### message `PoolRef`

> 池寻址：source + pool_key 唯一定位一个池。pool_key 语法（受控，由 model.ValidatePoolKey 校验）： /   SOURCE_HOT    -> "global" 或 "zone:<typeid>" /   SOURCE_TAG    -> "tag:<tag_id>" /   SOURCE_COLLAB -> "aid:<seed_aid>" /   SOURCE_VECTOR -> "mid:<mid>"（用户向量近邻候选） /   SOURCE_FOLLOW -> "mid:<mid>" /   SOURCE_COLD   -> "platform:<platform>" 或 "global" / 其它组合视为非法（ErrInvalidPoolKey），不接受调用方自由拼接的字符串键。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `source` | [`Source`](#enum-source) | 1 | — | 召回路 / 池类型 |
| `pool_key` | `string` | 2 | — | 池键，语法见上 |

### message `RequestContext`

> 请求上下文（在线召回的身份与设备维度）。 / 只传主键与受控摘要：设备只传 sha256 摘要，明文设备号/手机号/IP 一律拒绝。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 当前用户 ID，0 表示游客/未登录（走冷启动） |
| `platform` | [`Platform`](#enum-platform) | 2 | — | 客户端平台（不写死任何端 UI 行为，只用于分池与过滤） |
| `app_version` | `string` | 3 | — | 客户端版本号 |
| `device_id_hash` | `string` | 4 | — | 设备受控 ID 摘要（sha256 hex），空表示未知设备 |
| `region` | `string` | 5 | — | 地区代码（分区偏好与版权可见性用） |
| `scene` | `string` | 6 | — | 场景稳定 key（如 home.feed / play.related），仅日志与配额维度 |
| `request_id` | `string` | 7 | — | 幂等与审计键；空则由服务端生成并在响应回传 |
| `trace_id` | `string` | 8 | — | 调用方透传 trace_id |

### message `Candidate`

> 单个候选（跨服务只传主键 + 来源 + 分数）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID（video 服务主键；本服务不落稿件数据） |
| `source` | [`Source`](#enum-source) | 2 | — | 来源标记（去重合并后保留优先级最高的一路） |
| `rank_in_source` | `int32` | 3 | — | 该路内序号（0 起）；排序服务不可用时按此回退召回原序 |
| `score` | `double` | 4 | — | 召回分（同路内可比的归一值，跨路不可比） |
| `pool_version` | `int64` | 5 | — | 产出该候选的池版本号（0 表示在线实时计算、无池快照） |
| `batch_id` | `string` | 6 | — | 生成批次 ID（排障/回放；实时路可为空） |
| `also_from` | [`Source`](#enum-source) | 7 | repeated | 去重合并时被合并掉的其他来源（可解释性） |

### message `SourceStat`

> 每一路召回的取数统计（排障与监控用）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `source` | [`Source`](#enum-source) | 1 | — | 召回路 |
| `planned` | `int32` | 2 | — | 计划取数条数 |
| `returned` | `int32` | 3 | — | 实际返回条数 |
| `pool_version` | `int64` | 4 | — | 实际读到的版本；0 表示没有可用 CURRENT 版本 |
| `batch_id` | `string` | 5 | — | 实际读到的批次 |
| `degraded` | `bool` | 6 | — | 该路是否降级/被丢弃 |
| `error_code` | `string` | 7 | — | 稳定错误 key（pool_not_ready/feature_unavailable/timeout/...） |

### message `DegradationInfo`

> 降级声明：响应里必须能表达"这是降级结果"。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `degraded` | `bool` | 1 | — | 是否发生降级 |
| `reason` | [`DegradeReason`](#enum-degradereason) | 2 | — | 主原因 |
| `dropped_sources` | [`Source`](#enum-source) | 3 | repeated | 被丢弃/未出数的请求路 |
| `served_sources` | [`Source`](#enum-source) | 4 | repeated | 实际出数的路（降级后可能只剩兜底池） |
| `cost_ms` | `int32` | 5 | — | 本次召回耗时（预算裁剪的排障依据） |
| `detail` | `string` | 6 | — | 排障文本，禁止包含用户敏感信息 |

### message `RecallCandidatesReq`

> --- 在线召回 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `context` | [`RequestContext`](#message-requestcontext) | 1 | — | — |
| `sources` | [`Source`](#enum-source) | 2 | repeated | 指定召回路；空表示用服务默认组合 |
| `limit` | `int32` | 3 | — | 总候选条数上限（<= 配置 MaxCandidates），0 表示用默认 |
| `seed_aids` | `int64` | 4 | repeated | 相似/协同召回的种子稿件，上限 MaxSeedAids |
| `seed_tag_ids` | `int64` | 5 | repeated | 标签召回的种子标签，上限 MaxSeedTags |
| `exclude_aids` | `int64` | 6 | repeated | 调用方已知需排除的 aid（如已下发本页），上限 MaxExcludeAids |
| `allow_degrade` | `bool` | 7 | — | false 时缺池/缺特征直接报错（压测与回放要求真实失败） |

### message `RecallCandidatesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `request_id` | `string` | 1 | — | 回显（新请求时为服务端生成的 ID），与 recall_request_log 对应 |
| `snapshot_id` | `string` | 2 | — | 本次召回快照 ID，排序服务回填以便完整回放 |
| `candidates` | [`Candidate`](#message-candidate) | 3 | repeated | — |
| `per_source` | [`SourceStat`](#message-sourcestat) | 4 | repeated | — |
| `degradation` | [`DegradationInfo`](#message-degradationinfo) | 5 | — | — |
| `ttl_seconds` | `int64` | 6 | — | 建议网关缓存秒数；降级结果固定 0（不建议缓存） |
| `server_time` | `int64` | 7 | — | 服务端时间（Unix 秒） |

### message `PoolItem`

> --- 池快照运维/排障读 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID |
| `score` | `double` | 2 | — | 池内分数 |
| `version` | `int64` | 3 | — | 所属版本 |
| `ctime` | `int64` | 4 | — | 写入时间（Unix 秒） |

### message `GetPoolSnapshotReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pool` | [`PoolRef`](#message-poolref) | 1 | — | — |
| `version` | `int64` | 2 | — | 0 表示读 recall_pool_current 指向的 CURRENT 版本 |
| `pn` | `int32` | 3 | — | 页码，1 起 |
| `ps` | `int32` | 4 | — | 页大小，上限 MaxPoolSnapshotPage |

### message `GetPoolSnapshotReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pool` | [`PoolRef`](#message-poolref) | 1 | — | — |
| `version` | `int64` | 2 | — | — |
| `batch_id` | `string` | 3 | — | — |
| `state` | [`PoolVersionState`](#enum-poolversionstate) | 4 | — | — |
| `item_count` | `int32` | 5 | — | 该版本总条数 |
| `items` | [`PoolItem`](#message-poolitem) | 6 | repeated | — |
| `has_more` | `bool` | 7 | — | — |
| `published_at` | `int64` | 8 | — | 切换生效时间（Unix 秒），未上线为 0 |

### message `PoolVersionInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pool` | [`PoolRef`](#message-poolref) | 1 | — | — |
| `version` | `int64` | 2 | — | — |
| `batch_id` | `string` | 3 | — | — |
| `generator` | `string` | 4 | — | 产出方（cron job 名 / 离线作业 ID） |
| `schema_version` | `int32` | 5 | — | 池条目结构版本 |
| `item_count` | `int32` | 6 | — | — |
| `state` | [`PoolVersionState`](#enum-poolversionstate) | 7 | — | — |
| `published_at` | `int64` | 8 | — | — |
| `operator` | `string` | 9 | — | — |
| `note` | `string` | 10 | — | — |
| `ctime` | `int64` | 11 | — | — |
| `mtime` | `int64` | 12 | — | — |

### message `ListPoolVersionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pool` | [`PoolRef`](#message-poolref) | 1 | — | — |
| `limit` | `int32` | 2 | — | 上限 MaxVersionList（默认 100） |
| `include_retired` | `bool` | 3 | — | 是否包含已退役版本（回滚可行性检查） |

### message `ListPoolVersionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `versions` | [`PoolVersionInfo`](#message-poolversioninfo) | 1 | repeated | — |
| `current_version` | `int64` | 2 | — | 当前生效版本，0 表示无 CURRENT（在线该池不出数） |

### message `RecallRequestLogInfo`

> --- 召回请求审计回放 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `request_id` | `string` | 1 | — | — |
| `snapshot_id` | `string` | 2 | — | — |
| `mid` | `int64` | 3 | — | — |
| `scene` | `string` | 4 | — | — |
| `platform` | [`Platform`](#enum-platform) | 5 | — | — |
| `app_version` | `string` | 6 | — | — |
| `region` | `string` | 7 | — | — |
| `requested_sources` | [`Source`](#enum-source) | 8 | repeated | — |
| `per_source` | [`SourceStat`](#message-sourcestat) | 9 | repeated | — |
| `candidate_count` | `int32` | 10 | — | — |
| `returned_count` | `int32` | 11 | — | — |
| `degraded` | `bool` | 12 | — | — |
| `reason` | [`DegradeReason`](#enum-degradereason) | 13 | — | — |
| `cost_ms` | `int32` | 14 | — | — |
| `versions_digest` | `string` | 15 | — | sha256(读取到的 (source,version) 序列)，用于比对回放 |
| `trace_id` | `string` | 16 | — | — |
| `ctime` | `int64` | 17 | — | — |

### message `GetRecallRequestLogReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `request_id` | `string` | 1 | — | 与 snapshot_id 二选一，至少一个非空 |
| `snapshot_id` | `string` | 2 | — | — |

### message `GetRecallRequestLogReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entry` | [`RecallRequestLogInfo`](#message-recallrequestloginfo) | 1 | — | 不存在时为 null（不返回空对象冒充命中） |

### message `ListRecallRequestLogsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 按用户查（0 表示不按用户过滤） |
| `scene` | `string` | 2 | — | 按场景查 |
| `from_time` | `int64` | 3 | — | Unix 秒，含 |
| `to_time` | `int64` | 4 | — | Unix 秒，含 |
| `pn` | `int32` | 5 | — | — |
| `ps` | `int32` | 6 | — | 上限 MaxRequestLogPage |

### message `ListRecallRequestLogsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entries` | [`RecallRequestLogInfo`](#message-recallrequestloginfo) | 1 | repeated | — |
| `has_more` | `bool` | 2 | — | — |

### message `UpsertPoolItemsReq`

> --- 离线池写入与版本切换（写接口全部带幂等键） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pool` | [`PoolRef`](#message-poolref) | 1 | — | — |
| `version` | `int64` | 2 | — | 目标版本号：首批写入即登记 BUILDING 版本行； |
| `batch_id` | `string` | 3 | — | 同一 (pool, version) 只能属于一个 batch_id，跨批次复用版本号被拒绝 / 生成批次 ID（可追溯） |
| `generator` | `string` | 4 | — | 产出方标识 |
| `schema_version` | `int32` | 5 | — | 条目结构版本，0 表示服务当前版本 |
| `items` | [`PoolItemInput`](#message-pooliteminput) | 6 | repeated | 单次条数上限 MaxBatchItems |
| `idempotency_key` | `string` | 7 | — | 幂等键：同 key 重放不重复写入（缺失直接拒绝） |
| `is_last_batch` | `bool` | 8 | — | 该版本的最后一批，置为 READY |

### message `PoolItemInput`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | — |
| `score` | `double` | 2 | — | — |

### message `UpsertPoolItemsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `version` | `int64` | 1 | — | — |
| `written` | `int32` | 2 | — | 本次写入/更新的行数 |
| `item_count` | `int32` | 3 | — | 该版本累计条数 |
| `state` | [`PoolVersionState`](#enum-poolversionstate) | 4 | — | 版本当前状态 |
| `deduplicated` | `bool` | 5 | — | true 表示 idempotency_key 命中重放 |

### message `PublishPoolVersionReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pool` | [`PoolRef`](#message-poolref) | 1 | — | — |
| `version` | `int64` | 2 | — | 目标版本（必须处于 READY） |
| `operator` | `string` | 3 | — | 操作者（作业/运营标识），必填 |
| `reason` | `string` | 4 | — | 切换原因，必填（审计） |
| `idempotency_key` | `string` | 5 | — | 幂等键，必填 |

### message `PublishPoolVersionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `switched` | `bool` | 1 | — | 本次调用是否真的切换了指针 |
| `previous_version` | `int64` | 2 | — | 切换前的 CURRENT |
| `current_version` | `int64` | 3 | — | 切换后的 CURRENT |
| `deduplicated` | `bool` | 4 | — | — |
| `event_id` | `string` | 5 | — | recall.pool.published 事件 ID（outbox） |

### message `RollbackPoolVersionReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pool` | [`PoolRef`](#message-poolref) | 1 | — | — |
| `target_version` | `int64` | 2 | — | 回滚到的历史版本（必须存在且已完成写入） |
| `operator` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | 回滚原因，必填 |
| `idempotency_key` | `string` | 5 | — | — |

### message `RollbackPoolVersionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `switched` | `bool` | 1 | — | — |
| `previous_version` | `int64` | 2 | — | — |
| `current_version` | `int64` | 3 | — | — |
| `deduplicated` | `bool` | 4 | — | — |
| `event_id` | `string` | 5 | — | — |

### message `PrunePoolVersionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pool` | [`PoolRef`](#message-poolref) | 1 | — | — |
| `keep_versions` | `int32` | 2 | — | 每个池保留最近 N 个非 CURRENT 版本（>= MinKeepVersions） |
| `max_rows` | `int64` | 3 | — | 单次最多删除行数（保护锁与主从延迟） |
| `dry_run` | `bool` | 4 | — | true 只统计不删除 |
| `operator` | `string` | 5 | — | — |

### message `PrunePoolVersionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scanned_versions` | `int32` | 1 | — | — |
| `deleted_rows` | `int32` | 2 | — | — |
| `dry_run` | `bool` | 3 | — | — |
| `has_more` | `bool` | 4 | — | true 表示还有可清理内容，调用方需继续分批 |

### message `GetRecallConfigReq`

> --- 在线面参数下发（客户端/网关据此控制请求规模） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scene` | `string` | 1 | — | 预留：场景级参数（当前返回服务级参数） |
| `mid` | `int64` | 2 | — | 0 表示游客（游客只允许冷启动/热门路） |

### message `PoolStatus`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pool` | [`PoolRef`](#message-poolref) | 1 | — | — |
| `current_version` | `int64` | 2 | — | — |
| `batch_id` | `string` | 3 | — | — |
| `item_count` | `int32` | 4 | — | — |
| `published_at` | `int64` | 5 | — | — |
| `stale` | `bool` | 6 | — | 超过 PoolStaleSeconds 未更新（在线仍可读，但需要告警） |

### message `GetRecallConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `max_candidates` | `int32` | 1 | — | 单次召回总条数硬上限 |
| `default_limit` | `int32` | 2 | — | 未指定 limit 时的默认条数 |
| `per_source_max` | `int32` | 3 | — | 单路最大条数 |
| `enabled_sources` | [`Source`](#enum-source) | 4 | repeated | 服务开启的召回路 |
| `default_sources` | [`Source`](#enum-source) | 5 | repeated | 未指定 sources 时的默认组合 |
| `max_seed_aids` | `int32` | 6 | — | — |
| `max_seed_tags` | `int32` | 7 | — | — |
| `max_exclude_aids` | `int32` | 8 | — | — |
| `degrade_enabled` | `bool` | 9 | — | 是否允许降级出数（关闭时依赖故障直接报错） |
| `fallback_source` | [`Source`](#enum-source) | 10 | — | 兜底路（热门或关注池） |
| `ttl_seconds` | `int64` | 11 | — | 正常结果建议缓存秒数 |
| `ready_pools` | [`PoolStatus`](#message-poolstatus) | 12 | repeated | 当前有 CURRENT 版本的池摘要 |
