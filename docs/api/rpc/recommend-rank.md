# RPC · `recommend-rank`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/recommend-rank/rpc/rank.proto` |
| protobuf 包 | `recommendrank.v1` |
| go_package | `go-video/services/recommend-rank/rpc` |
| 发现用的 etcd key | `recommendrank.v1.rpc`（`services/recommend-rank/etc/recommendrank.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`recommendrank.v1.rpc`） |
| 监听 | `8123`（`services/recommend-rank/etc/recommendrank.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_recommend_rank` |
| 方法数 | 10（service `Rank`） |
| 网关消费方 | `admin:RecommendRankRPC` |

## 契约说明

> recommend-rank：多目标排序领域服务（模型版本、特征配置、A/B 实验、频控与打散、安全过滤）。
>
> 契约边界（AGENTS.md）：
>   - §7：排序只消费候选与特征做打分排序，不提供广告位参数、不做广告投放与商业化报表；
>     本契约**没有**"手工置顶/加权某 aid""按付费能力排序"之类的入参。
>     也不提供"改写推荐结果"的写接口：运营能改的只有模型版本、特征配置与实验状态，
>     且每次改动都带 operator/reason 并可审计。
>   - §5：候选由调用方（gateway/app 经 recommend-recall）传入，跨服务只传主键 aid；
>     本服务不落稿件、用户、媒资主数据，也不写任何其他服务的表。
>   - 硬约束：**任何情况下返回的 items 都是入参 candidates 的子集**，
>     不得凭空生成 aid，不得返回超出候选集的结果；候选为空时返回空列表 + 明确原因。
>
> 可审计性（本契约的核心设计）：
>   - 每次排序产出一个 decision_id，落 rank_decision_log：入参条数、出参条数、
>     过滤计数、model_key/model_version/feature_config_version、exp_key/variant_key/bucket_no、
>     有序 aid 的 sha256 摘要（result_digest）；
>   - 调用方（recommend-recall 的 snapshot_id + 本次 request_id）可把整条链路回放出来。
>
> 降级红线：模型/特征/存储故障时按 FallbackStrategy 回退召回原序并置 degraded=true，
> 不返回"看起来正常"的空结果冒充成功；排序服务整体不可用时，由调用方直接使用
> recommend-recall 的 rank_in_source 顺序（见 README 降级矩阵）。

## service `Rank`

> Rank 排序服务。

gRPC 方法前缀：`recommendrank.v1.Rank/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `RankCandidates` | [`RankCandidatesReq`](#message-rankcandidatesreq) | [`RankCandidatesReply`](#message-rankcandidatesreply) | 多目标排序：入参候选子集内排序，输出可审计结果摘要 |
| 2 | `GetRankDecision` | [`GetRankDecisionReq`](#message-getrankdecisionreq) | [`GetRankDecisionReply`](#message-getrankdecisionreply) | 按 decision_id/request_id 回放一次排序决策 |
| 3 | `ListRankDecisions` | [`ListRankDecisionsReq`](#message-listrankdecisionsreq) | [`ListRankDecisionsReply`](#message-listrankdecisionsreply) | 分页查排序决策摘要（审计与实验核对） |
| 4 | `UpsertModelVersion` | [`UpsertModelVersionReq`](#message-upsertmodelversionreq) | [`UpsertModelVersionReply`](#message-upsertmodelversionreply) | 登记/更新模型版本元数据（版本不可变，元数据变更 revision+1） |
| 5 | `SetModelVersionState` | [`SetModelVersionStateReq`](#message-setmodelversionstatereq) | [`SetModelVersionStateReply`](#message-setmodelversionstatereply) | 切换模型版本状态（READY/ACTIVE/RETIRED；激活是回滚开关） |
| 6 | `UpsertFeatureConfig` | [`UpsertFeatureConfigReq`](#message-upsertfeatureconfigreq) | [`UpsertFeatureConfigReply`](#message-upsertfeatureconfigreply) | 登记/更新特征配置版本 |
| 7 | `UpsertExperiment` | [`UpsertExperimentReq`](#message-upsertexperimentreq) | [`UpsertExperimentReply`](#message-upsertexperimentreply) | 新建/修改实验变体（同 (exp_key,variant_key) 唯一，变更 revision+1） |
| 8 | `SetExperimentState` | [`SetExperimentStateReq`](#message-setexperimentstatereq) | [`SetExperimentStateReply`](#message-setexperimentstatereply) | 实验状态迁移（RUNNING/PAUSED/STOPPED） |
| 9 | `GetExperimentAssignment` | [`GetExperimentAssignmentReq`](#message-getexperimentassignmentreq) | [`GetExperimentAssignmentReply`](#message-getexperimentassignmentreply) | 查询/登记主体在实验中的稳定分桶 |
| 10 | `GetRankRuntimeConfig` | [`GetRankRuntimeConfigReq`](#message-getrankruntimeconfigreq) | [`GetRankRuntimeConfigReply`](#message-getrankruntimeconfigreply) | 下发在线排序参数与当前生效的模型/特征/实验状态 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `Platform`

> 客户端平台（AGENTS.md §1/§6）。编号与 recommend-recall 契约一致但本文件不 import 它。

| 值 | 编号 | 说明 |
|---|---|---|
| `PLATFORM_UNSPECIFIED` | 0 | 未指定 |
| `PLATFORM_ANDROID` | 1 | Android |
| `PLATFORM_IOS` | 2 | iOS |
| `PLATFORM_HARMONY` | 3 | HarmonyOS |
| `PLATFORM_DESKTOP` | 4 | 电脑客户端 |

### enum `RankSource`

> 候选来源标记。编号与 recommend-recall 的 Source 逐一对齐（同名同值）， / 映射由调用方（gateway/app）负责，两服务互不 import； / 一致性由 internal/logic/contract_consistency_test.go 钉住，禁止随意改号。

| 值 | 编号 | 说明 |
|---|---|---|
| `RANK_SOURCE_UNSPECIFIED` | 0 | 未指定 |
| `RANK_SOURCE_HOT` | 1 | 热门池 |
| `RANK_SOURCE_FOLLOW` | 2 | 关注池 |
| `RANK_SOURCE_TAG` | 3 | 标签池 |
| `RANK_SOURCE_COLLAB` | 4 | 协同候选 |
| `RANK_SOURCE_VECTOR` | 5 | 向量候选 |
| `RANK_SOURCE_COLD` | 6 | 冷启动池 |

### enum `ModelVersionState`

> 模型版本状态（与 model.rank_model_version.state 一致）。 / 合法迁移：DRAFT -> READY -> ACTIVE -> RETIRED；READY -> RETIRED。ACTIVE 每 model_key 至多一个。

| 值 | 编号 | 说明 |
|---|---|---|
| `MODEL_VERSION_STATE_UNSPECIFIED` | 0 | 未指定 |
| `MODEL_VERSION_STATE_DRAFT` | 1 | 登记中，不可上线 |
| `MODEL_VERSION_STATE_READY` | 2 | 校验通过，可激活 |
| `MODEL_VERSION_STATE_ACTIVE` | 3 | 当前生效 |
| `MODEL_VERSION_STATE_RETIRED` | 4 | 已下线（保留供审计与回滚） |

### enum `ExperimentState`

> 实验状态（与 model.rank_experiment.state 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `EXPERIMENT_STATE_UNSPECIFIED` | 0 | 未指定 |
| `EXPERIMENT_STATE_DRAFT` | 1 | 草稿 |
| `EXPERIMENT_STATE_RUNNING` | 2 | 分流中 |
| `EXPERIMENT_STATE_PAUSED` | 3 | 暂停（已分桶保持不变） |
| `EXPERIMENT_STATE_STOPPED` | 4 | 结束（终态） |

### enum `SubjectType`

> 分桶主体类型：登录用户或设备受控摘要（游客）。

| 值 | 编号 | 说明 |
|---|---|---|
| `SUBJECT_TYPE_UNSPECIFIED` | 0 | 未指定 |
| `SUBJECT_TYPE_MID` | 1 | subject_id = mid 的十进制字符串 |
| `SUBJECT_TYPE_DEVICE` | 2 | subject_id = 设备 sha256 摘要（禁止明文设备号） |

### enum `RankDegradeReason`

> 降级原因（与 model.RankDegradeReason* 一致）。0 表示未降级。

| 值 | 编号 | 说明 |
|---|---|---|
| `RANK_DEGRADE_REASON_UNSPECIFIED` | 0 | 未降级 |
| `RANK_DEGRADE_REASON_MODEL_UNAVAILABLE` | 1 | 无 ACTIVE 模型版本或模型加载失败 |
| `RANK_DEGRADE_REASON_FEATURE_UNAVAILABLE` | 2 | 特征读取下游不可用（feature-store/spm） |
| `RANK_DEGRADE_REASON_STORE_UNAVAILABLE` | 3 | Redis/MySQL 不可用 |
| `RANK_DEGRADE_REASON_BUDGET_EXHAUSTED` | 4 | 打分时间预算耗尽，裁剪未打分候选 |
| `RANK_DEGRADE_REASON_SAFETY_UNAVAILABLE` | 5 | 内容安全结论不可读（按不通过处理并声明） |
| `RANK_DEGRADE_REASON_EXPERIMENT_UNAVAILABLE` | 6 | 实验配置不可读，退回默认变体 |
| `RANK_DEGRADE_REASON_EMPTY_CANDIDATES` | 7 | 入参候选为空（不是故障，但必须显式） |

### enum `FallbackStrategy`

> 降级时采用的兜底策略。0 表示未降级。

| 值 | 编号 | 说明 |
|---|---|---|
| `FALLBACK_STRATEGY_UNSPECIFIED` | 0 | 未降级 |
| `FALLBACK_STRATEGY_RECALL_ORDER` | 1 | 回退召回原序（source 优先级 + rank_in_source） |
| `FALLBACK_STRATEGY_PREVIOUS_MODEL` | 2 | 回退到上一个 ACTIVE 模型版本 |
| `FALLBACK_STRATEGY_SAFETY_ONLY` | 3 | 只跑安全过滤与去重，不打分 |

### message `RequestContext`

> 请求上下文（AGENTS.md §6：能区分平台/版本/设备，但不写死任何端 UI 行为）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 当前用户 ID，0 表示游客 |
| `platform` | [`Platform`](#enum-platform) | 2 | — | 客户端平台 |
| `app_version` | `string` | 3 | — | 客户端版本号 |
| `device_id_hash` | `string` | 4 | — | 设备受控摘要（sha256 hex）；明文设备号被拒绝 |
| `region` | `string` | 5 | — | 地区代码 |
| `scene` | `string` | 6 | — | 场景稳定 key（home.feed / play.related ...） |
| `request_id` | `string` | 7 | — | 幂等与审计键；同 key 重放返回同一 decision |
| `trace_id` | `string` | 8 | — | 调用方透传 trace_id |

### message `CandidateInput`

> 入参候选（只接受主键 + 来源 + 召回分数；不接受任何"人工加权/置顶"字段）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID |
| `source` | [`RankSource`](#enum-ranksource) | 2 | — | 来源标记 |
| `recall_score` | `double` | 3 | — | 召回分（跨路不可比，仅用于兜底排序） |
| `rank_in_source` | `int32` | 4 | — | 召回路内序号（0 起），回退召回原序时使用 |
| `pool_version` | `int64` | 5 | — | 召回池版本（审计回指） |
| `batch_id` | `string` | 6 | — | 召回生成批次（审计回指） |

### message `RankedItem`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID（必定来自入参 candidates） |
| `score` | `double` | 2 | — | 最终排序分（多目标加权后的合成值） |
| `objectives` | [`ObjectiveScore`](#message-objectivescore) | 3 | repeated | 各目标预估值（可解释，不含商业化目标） |
| `source` | [`RankSource`](#enum-ranksource) | 4 | — | 原样回传来源 |
| `recall_rank` | `int32` | 5 | — | 原样回传召回路内序号 |
| `reason_code` | `string` | 6 | — | 稳定 key（rk.because.follow / rk.because.hot ...），文案由客户端渲染 |
| `original_index` | `int32` | 7 | — | 在入参 candidates 中的下标，用于验证"没有凭空产生" |

### message `ObjectiveScore`

> 单个优化目标的预估值。目标集合是受控枚举，新增目标必须先登记再使用。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `objective` | `string` | 1 | — | 受控 key：pred_click / pred_finish / pred_interact / pred_negative |
| `value` | `double` | 2 | — | 概率或归一值 |

### message `FilterStat`

> 过滤与打散统计（丢弃原因必须可见，不允许静默吞候选）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `safety_filtered` | `int32` | 1 | — | 内容安全/审核不可见被剔除 |
| `frequency_filtered` | `int32` | 2 | — | 频控（同作者/同标签/近期已推上限）被剔除 |
| `dedup_filtered` | `int32` | 3 | — | 入参重复 aid 去重 |
| `diversified_moved` | `int32` | 4 | — | 打散导致位置移动（不减少条数） |
| `truncated` | `int32` | 5 | — | 超出 limit 被截断 |

### message `DegradationInfo`

> 降级声明。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `degraded` | `bool` | 1 | — | — |
| `reason` | [`RankDegradeReason`](#enum-rankdegradereason) | 2 | — | — |
| `fallback` | [`FallbackStrategy`](#enum-fallbackstrategy) | 3 | — | — |
| `scored_items` | `int32` | 4 | — | 真正被模型打分的条数 |
| `cost_ms` | `int32` | 5 | — | 本次排序耗时 |
| `detail` | `string` | 6 | — | 排障文本，禁止包含用户敏感信息 |

### message `RankCandidatesReq`

> --- 在线排序 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `context` | [`RequestContext`](#message-requestcontext) | 1 | — | — |
| `candidates` | [`CandidateInput`](#message-candidateinput) | 2 | repeated | 上限 MaxCandidates，超限直接报错（不静默裁剪） |
| `snapshot_id` | `string` | 3 | — | recommend-recall 返回的召回快照 ID（审计回指） |
| `limit` | `int32` | 4 | — | 出参条数（<= MaxReturn） |
| `allow_degrade` | `bool` | 5 | — | false 时依赖故障直接报错（压测/回放） |
| `idempotency_key` | `string` | 6 | — | 幂等键；为空时服务端用 request_id 兜底 |

### message `RankCandidatesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `decision_id` | `string` | 1 | — | 本次排序的审计 ID（rank_decision_log.decision_id） |
| `request_id` | `string` | 2 | — | 回显或服务端生成 |
| `items` | [`RankedItem`](#message-rankeditem) | 3 | repeated | 入参候选的子集，按最终分降序 |
| `result_digest` | `string` | 4 | — | sha256(有序 aid 列表, 分隔符 "\|")，回放比对 |
| `model_key` | `string` | 5 | — | — |
| `model_version` | `string` | 6 | — | — |
| `feature_config_version` | `string` | 7 | — | — |
| `exp_key` | `string` | 8 | — | 命中的实验（未命中为空） |
| `variant_key` | `string` | 9 | — | 命中的变体（未命中为 default） |
| `bucket_no` | `int32` | 10 | — | 分桶号 |
| `recall_snapshot_id` | `string` | 11 | — | 回指召回快照 |
| `input_count` | `int32` | 12 | — | — |
| `returned_count` | `int32` | 13 | — | — |
| `filters` | [`FilterStat`](#message-filterstat) | 14 | — | — |
| `degradation` | [`DegradationInfo`](#message-degradationinfo) | 15 | — | — |
| `ttl_seconds` | `int64` | 16 | — | 建议网关缓存秒数；降级结果固定 0 |

### message `RankDecisionInfo`

> --- 审计读（结果摘要必须可查） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `decision_id` | `string` | 1 | — | — |
| `request_id` | `string` | 2 | — | — |
| `trace_id` | `string` | 3 | — | — |
| `snapshot_id` | `string` | 4 | — | — |
| `mid` | `int64` | 5 | — | — |
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 6 | — | — |
| `subject_id` | `string` | 7 | — | mid 字符串或设备摘要（不含明文设备号） |
| `scene` | `string` | 8 | — | — |
| `platform` | [`Platform`](#enum-platform) | 9 | — | — |
| `app_version` | `string` | 10 | — | — |
| `exp_key` | `string` | 11 | — | — |
| `variant_key` | `string` | 12 | — | — |
| `bucket_no` | `int32` | 13 | — | — |
| `model_key` | `string` | 14 | — | — |
| `model_version` | `string` | 15 | — | — |
| `feature_config_version` | `string` | 16 | — | — |
| `input_count` | `int32` | 17 | — | — |
| `returned_count` | `int32` | 18 | — | — |
| `result_digest` | `string` | 19 | — | — |
| `top_aids` | `int64` | 20 | repeated | 前 N 个 aid（N 由配置 MaxDigestAids 决定），人工排障用 |
| `degraded` | `bool` | 21 | — | — |
| `reason` | [`RankDegradeReason`](#enum-rankdegradereason) | 22 | — | — |
| `fallback` | [`FallbackStrategy`](#enum-fallbackstrategy) | 23 | — | — |
| `filters` | [`FilterStat`](#message-filterstat) | 24 | — | — |
| `cost_ms` | `int32` | 25 | — | — |
| `ctime` | `int64` | 26 | — | — |

### message `GetRankDecisionReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `decision_id` | `string` | 1 | — | 与 request_id 二选一 |
| `request_id` | `string` | 2 | — | — |

### message `GetRankDecisionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entry` | [`RankDecisionInfo`](#message-rankdecisioninfo) | 1 | — | 不存在时为 null，不返回空对象冒充命中 |

### message `ListRankDecisionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `exp_key` | `string` | 1 | — | — |
| `variant_key` | `string` | 2 | — | — |
| `model_key` | `string` | 3 | — | — |
| `model_version` | `string` | 4 | — | — |
| `scene` | `string` | 5 | — | — |
| `from_time` | `int64` | 6 | — | Unix 秒，含 |
| `to_time` | `int64` | 7 | — | Unix 秒，含 |
| `only_degraded` | `bool` | 8 | — | — |
| `pn` | `int32` | 9 | — | — |
| `ps` | `int32` | 10 | — | 上限 MaxDecisionPage |

### message `ListRankDecisionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entries` | [`RankDecisionInfo`](#message-rankdecisioninfo) | 1 | repeated | — |
| `has_more` | `bool` | 2 | — | — |

### message `UpsertModelVersionReq`

> --- 模型版本与特征配置（写接口全部带 operator/reason 与幂等键） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `model_key` | `string` | 1 | — | 逻辑模型名（如 home_feed_multi_gate） |
| `version` | `string` | 2 | — | 版本号（不可变，注册后只能改状态） |
| `feature_config_version` | `string` | 3 | — | 绑定的特征配置版本（必须已存在） |
| `objective_weights` | [`ObjectiveWeight`](#message-objectiveweight) | 4 | repeated | 多目标权重（受控目标集合） |
| `artifact_ref` | `string` | 5 | — | 模型工件引用（对象存储 key，禁止内联模型本体与密钥） |
| `offline_metrics` | `string` | 6 | — | 离线指标 JSON（AUC/校准等），仅审计展示，不参与在线决策 |
| `operator` | `string` | 7 | — | — |
| `reason` | `string` | 8 | — | — |
| `idempotency_key` | `string` | 9 | — | — |

### message `ObjectiveWeight`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `objective` | `string` | 1 | — | 受控 key，同 ObjectiveScore.objective |
| `weight` | `double` | 2 | — | >= 0；权重和不做强制归一，但服务端校验不超过 10 |

### message `UpsertModelVersionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `model_key` | `string` | 1 | — | — |
| `version` | `string` | 2 | — | — |
| `state` | [`ModelVersionState`](#enum-modelversionstate) | 3 | — | — |
| `revision` | `int32` | 4 | — | 元数据修订号（权重/工件变更 +1），版本号不变 |
| `deduplicated` | `bool` | 5 | — | — |

### message `SetModelVersionStateReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `model_key` | `string` | 1 | — | — |
| `version` | `string` | 2 | — | — |
| `target_state` | [`ModelVersionState`](#enum-modelversionstate) | 3 | — | 只允许 READY/ACTIVE/RETIRED |
| `operator` | `string` | 4 | — | — |
| `reason` | `string` | 5 | — | 必填：激活/回滚理由是审计要求 |
| `idempotency_key` | `string` | 6 | — | — |

### message `SetModelVersionStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `changed` | `bool` | 1 | — | — |
| `state` | [`ModelVersionState`](#enum-modelversionstate) | 2 | — | — |
| `previous_active_version` | `string` | 3 | — | 该 model_key 切换前的 ACTIVE |
| `deduplicated` | `bool` | 4 | — | — |
| `event_id` | `string` | 5 | — | 预留：状态变更事件（本期未接 MQ，见 README 缺口） |

### message `UpsertFeatureConfigReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `config_version` | `string` | 1 | — | 特征配置版本（不可变标识） |
| `feature_keys` | `string` | 2 | repeated | 特征清单（受控 key，上限 MaxFeatureKeys） |
| `missing_policy` | `string` | 3 | — | 缺失值策略：default / drop_source / reject（受控枚举） |
| `feature_store_scene` | `string` | 4 | — | 将来接 feature-store 的读取场景 key |
| `operator` | `string` | 5 | — | — |
| `reason` | `string` | 6 | — | — |
| `idempotency_key` | `string` | 7 | — | — |

### message `UpsertFeatureConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `config_version` | `string` | 1 | — | — |
| `feature_count` | `int32` | 2 | — | — |
| `revision` | `int32` | 3 | — | — |
| `deduplicated` | `bool` | 4 | — | — |

### message `UpsertExperimentReq`

> --- A/B 实验与分桶 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `exp_key` | `string` | 1 | — | — |
| `variant_key` | `string` | 2 | — | 同一 exp_key 下的变体（含 "control"） |
| `layer_key` | `string` | 3 | — | 互斥层（同层互斥分流，不同层正交） |
| `hash_seed` | `string` | 4 | — | 分桶哈希盐（变更会导致重新分桶，需 reason 说明） |
| `bucket_start` | `int32` | 5 | — | [start, end) 左闭右开，0 <= start < end <= 1000 |
| `bucket_end` | `int32` | 6 | — | — |
| `model_key` | `string` | 7 | — | — |
| `model_version` | `string` | 8 | — | 空表示沿用 ACTIVE 版本 |
| `feature_config_version` | `string` | 9 | — | — |
| `overrides` | `string` | 10 | — | 参数覆盖 JSON（受控 key，禁止出现商业化字段） |
| `start_at` | `int64` | 11 | — | Unix 秒 |
| `end_at` | `int64` | 12 | — | Unix 秒，0 表示未设定 |
| `operator` | `string` | 13 | — | — |
| `reason` | `string` | 14 | — | — |
| `idempotency_key` | `string` | 15 | — | — |

### message `ExperimentInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `exp_key` | `string` | 1 | — | — |
| `variant_key` | `string` | 2 | — | — |
| `layer_key` | `string` | 3 | — | — |
| `bucket_start` | `int32` | 4 | — | — |
| `bucket_end` | `int32` | 5 | — | — |
| `model_key` | `string` | 6 | — | — |
| `model_version` | `string` | 7 | — | — |
| `feature_config_version` | `string` | 8 | — | — |
| `overrides` | `string` | 9 | — | — |
| `state` | [`ExperimentState`](#enum-experimentstate) | 10 | — | — |
| `revision` | `int32` | 11 | — | — |
| `start_at` | `int64` | 12 | — | — |
| `end_at` | `int64` | 13 | — | — |
| `operator` | `string` | 14 | — | — |
| `reason` | `string` | 15 | — | — |
| `ctime` | `int64` | 16 | — | — |
| `mtime` | `int64` | 17 | — | — |

### message `UpsertExperimentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `experiment` | [`ExperimentInfo`](#message-experimentinfo) | 1 | — | — |
| `deduplicated` | `bool` | 2 | — | — |

### message `SetExperimentStateReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `exp_key` | `string` | 1 | — | — |
| `variant_key` | `string` | 2 | — | — |
| `target_state` | [`ExperimentState`](#enum-experimentstate) | 3 | — | RUNNING/PAUSED/STOPPED |
| `operator` | `string` | 4 | — | — |
| `reason` | `string` | 5 | — | — |
| `idempotency_key` | `string` | 6 | — | — |

### message `SetExperimentStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `changed` | `bool` | 1 | — | — |
| `state` | [`ExperimentState`](#enum-experimentstate) | 2 | — | — |
| `deduplicated` | `bool` | 3 | — | — |

### message `GetExperimentAssignmentReq`

> 分桶查询：同一主体对同一实验的分配是稳定的（首次访问落库，之后 sticky 复用）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `exp_key` | `string` | 1 | — | — |
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 2 | — | — |
| `subject_id` | `string` | 3 | — | mid 十进制字符串或设备 sha256 摘要 |
| `bucket_count` | `int32` | 4 | — | 桶总数，0 表示用配置 BucketCount（默认 1000） |

### message `GetExperimentAssignmentReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `exp_key` | `string` | 1 | — | — |
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 2 | — | — |
| `subject_id` | `string` | 3 | — | — |
| `bucket_no` | `int32` | 4 | — | — |
| `variant_key` | `string` | 5 | — | 未命中任何 RUNNING 变体时为 "control" |
| `newly_assigned` | `bool` | 6 | — | false 表示 sticky 复用或纯计算结果 |
| `assigned_at` | `int64` | 7 | — | Unix 秒 |
| `hash_seed` | `string` | 8 | — | — |

### message `GetRankRuntimeConfigReq`

> --- 在线面参数与运行时状态 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scene` | `string` | 1 | — | 预留：场景级参数 |
| `model_key` | `string` | 2 | — | 空表示默认 model_key |

### message `GetRankRuntimeConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `model_key` | `string` | 1 | — | — |
| `active_model_version` | `string` | 2 | — | 空表示无 ACTIVE 模型（在线必然降级） |
| `feature_config_version` | `string` | 3 | — | — |
| `max_candidates` | `int32` | 4 | — | — |
| `max_return` | `int32` | 5 | — | — |
| `objectives` | `string` | 6 | repeated | 启用的优化目标受控 key |
| `degrade_enabled` | `bool` | 7 | — | — |
| `fallback` | [`FallbackStrategy`](#enum-fallbackstrategy) | 8 | — | 故障时的兜底策略 |
| `score_budget_ms` | `int64` | 9 | — | 打分时间预算 |
| `ttl_seconds` | `int64` | 10 | — | 正常结果建议缓存秒数 |
| `running_experiments` | [`ExperimentInfo`](#message-experimentinfo) | 11 | repeated | — |
| `config_revision` | `string` | 12 | — | 配置代次摘要（灰度核对用） |
