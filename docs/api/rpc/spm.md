# RPC · `spm`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/spm/rpc/spm.proto` |
| protobuf 包 | `spm.v1` |
| go_package | `go-video/services/spm/rpc` |
| 发现用的 etcd key | `spm.v1.rpc`（`services/spm/etc/spm.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`spm.v1.rpc`） |
| 监听 | `8131`（`services/spm/etc/spm.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_spm` |
| 方法数 | 16（service `Spm`） |
| 网关消费方 | `admin:SpmRPC` |

## 契约说明

> spm：用户行为分析与推荐特征计算的所有者（AGENTS.md §7）。
>
> 硬边界（AGENTS.md §7，违反即返工）
>   * SPM 在本项目只代表用户行为分析链路：采集 -> 指标 -> 热度/完播率/留存/兴趣/推荐特征。
>   * 本契约不提供任何广告位参数、广告投放、商业化报表或「广告推荐」相关字段与方法；
>     不得新增 ad_slot / campaign / billing / revenue 之类的语义。
>   * 本契约不提供「手工修改推荐结果」的接口：推荐链路只能
>     `spm 指标 -> feature-store 特征 -> recommend-recall/rank` 正向流动，
>     任何服务都不能反过来把排序结果写回 spm。
>   * 会员、订单、支付、投币、创作者分成不属于本期范围，因此本契约的指标集合里
>     不存在任何变现类口径。
>
> 数据所有权（AGENTS.md §5）
>   * 本服务拥有 10 张自有表，库名 go_video_spm：
>     spm_behavior_event（脱敏行为事实，唯一事实源）、spm_metric_definition（口径注册表）、
>     spm_metric_window、spm_user_interest、spm_retention_cohort、spm_window_watermark
>     （四张可从事实重算的投影，不是事实源）、
>     spm_aggregation_job（作业与租约）、spm_content_projection（content.published.v1 的
>     本地只读投影，只投影 subject/content_type/zone/state 等榜单过滤必需的最小列）、
>     spm_consumer_offset（消费状态机与位点）、spm_dead_letter（死信留档）。
>   * 行为事件只从 MQ（event-collector 投递的 behavior.* topic 与领域原始事件）进入，
>     本契约不提供事件明细写入口，
>     避免任何服务绕过 event-collector 的限流、采样与脱敏直接灌数据。
>   * 跨服务只传主键：content_id/content_type、aid、mid、zone_id、catalog_item_id。
>     不复制视频标题、用户昵称等可变主资料（那些归 video / catalog / user-profile）。
>     注意上游事件给的是 content_id + content_type 这一对，而不是 aid + zone_id：
>     UGC 的 content_id 就是 aid，PGC 的 content_id 是 catalog 的 episode_id；
>     分区（zone_id）与作品级 item_id 都不在行为事件里，只能由消费者 mapping 阶段
>     查 spm_content_projection 补齐，补不到就是 0（未知），本契约不猜测。
>
> 消费的事件词汇（与 common/eventenvelope 和 event-collector 契约逐字对齐）
>   * 信封字段固定为 event_id / event_type / schema_version / occurred_at / producer /
>     trace_id / aggregate_type / aggregate_id / payload：
>     - event_type 必须是小写点分串（eventenvelope.Validate 拒绝大写、首尾点、连续点），
>       因此写 `behavior.play` 而不是 `Behavior.Play`；
>     - schema_version 必须 > 0；
>     - occurred_at 是 **RFC3339 字符串**（不是 Unix 秒）。落到本契约的所有
>       int64 时间列/字段（如 MetricPoint.event_time）都必须先转成 Unix 秒，
>       解析失败的事件按坏消息处理，不能退化成「用当前时间顶上」。
>   * topic 名由 eventenvelope.Topic(event_type, schema_version) 派生，
>     即 `<event_type>.v<schema_version>`，例如 behavior.play + 1 -> behavior.play.v1。
>     本契约的 ListConsumerStateReq.topic / ListDeadLettersReq.topic 都用这个形态。
>   * 通道一：event-collector 归一化行为事件，event_type = behavior.<category>，
>     category 取自 rpc.BehaviorCategory 全集：play / click / search / skip / like /
>     favorite / follow / share / quality / exposure。行为明细字段（position_ms、
>     duration_ms、result_index、target_mid、session_id…）在 payload 里，
>     设备与 IP 只以 device_hash / ip_segment 摘要到达（明文在采集端就已丢弃）。
>   * 通道二：领域服务原始事件 playback.heartbeat、engagement.action、search.query
>     （docs/api-and-events.md §5 与各生产者 README）。
>   * 通道三：content.published 只用于维护 spm_content_projection，不参与行为计数。
>   * 事件入口只有 MQ：本契约不提供事件明细写 RPC，也不接受任何调用方直接写
>     spm_behavior_event。
>
> 口径版本化
>   * 每个指标由 (metric_key, metric_version) 唯一确定。口径变更（公式、分母定义、
>     去重窗口）必须新增一个 metric_version，禁止原地改写已有版本，
>     否则历史窗口数据会被静默重新解释（见 UpsertMetricDefinition 的不可变约束）。
>
> 隐私（AGENTS.md §7 第 1 条、docs/data-design.md §6）
>   * 本契约所有响应都不返回行为明细原文，只返回聚合值与脱敏后的兴趣权重；
>     不提供「按 mid 列出其全部行为事件」的读接口。
>   * 落库侧只允许 mid、aid、zone_id 与设备/会话的哈希摘要；明文设备号、手机号、
>     原始 IP 一律禁止入库（消费者在 mapping 阶段丢弃）。
>
> 通用约定
>   * 时间统一 Unix 秒（int64）。
>   * 分页统一 pn（从 1 开始）/ ps（服务端上限见各方法注释），响应回带 total。
>   * 写接口全部要求 request_id 幂等键；重复提交返回首次结果，不产生第二次写入。

## service `Spm`

> Spm 用户行为分析与推荐特征计算服务。 / 事件入口是 MQ（见 services/spm/README.md「消费链路」），gRPC 面只做 / 指标读、口径登记、作业编排与可观测；不提供行为明细写、不提供推荐结果覆盖。

gRPC 方法前缀：`spm.v1.Spm/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `GetMetric` | [`GetMetricReq`](#message-getmetricreq) | [`GetMetricReply`](#message-getmetricreply) | 读取单主体单口径单窗口的指标值 |
| 2 | `BatchGetMetrics` | [`BatchGetMetricsReq`](#message-batchgetmetricsreq) | [`BatchGetMetricsReply`](#message-batchgetmetricsreply) | 批量读取主体的一组指标/多个窗口（单次上限 50 口径 × 30 窗口） |
| 3 | `ListHotSubjects` | [`ListHotSubjectsReq`](#message-listhotsubjectsreq) | [`ListHotSubjectsReply`](#message-listhotsubjectsreply) | 热度榜投影（只读，按指标值倒序分页） |
| 4 | `GetUserInterest` | [`GetUserInterestReq`](#message-getuserinterestreq) | [`GetUserInterestReply`](#message-getuserinterestreply) | 用户兴趣画像（脱敏权重，不返回行为明细） |
| 5 | `GetRetention` | [`GetRetentionReq`](#message-getretentionreq) | [`GetRetentionReply`](#message-getretentionreply) | 留存曲线 |
| 6 | `WriteMetricWindow` | [`WriteMetricWindowReq`](#message-writemetricwindowreq) | [`WriteMetricWindowReply`](#message-writemetricwindowreply) | 聚合链路写回窗口指标（幂等覆盖，来源受 MetricSource 白名单约束） |
| 7 | `RecomputeMetrics` | [`RecomputeMetricsReq`](#message-recomputemetricsreq) | [`RecomputeMetricsReply`](#message-recomputemetricsreply) | 从事实表重算指标（计数/指标漂移的修复入口，派生 JOB_TYPE_RECOMPUTE 作业） |
| 8 | `UpsertMetricDefinition` | [`UpsertMetricDefinitionReq`](#message-upsertmetricdefinitionreq) | [`UpsertMetricDefinitionReply`](#message-upsertmetricdefinitionreply) | 登记指标口径新版本（已存在版本不可变） |
| 9 | `UpdateMetricDefinitionState` | [`UpdateMetricDefinitionStateReq`](#message-updatemetricdefinitionstatereq) | [`UpdateMetricDefinitionStateReply`](#message-updatemetricdefinitionstatereply) | 变更口径状态（DRAFT/ACTIVE/RETIRED），不删除历史口径 |
| 10 | `GetMetricDefinition` | [`GetMetricDefinitionReq`](#message-getmetricdefinitionreq) | [`GetMetricDefinitionReply`](#message-getmetricdefinitionreply) | 查询单个口径 |
| 11 | `ListMetricDefinitions` | [`ListMetricDefinitionsReq`](#message-listmetricdefinitionsreq) | [`ListMetricDefinitionsReply`](#message-listmetricdefinitionsreply) | 口径列表（分页） |
| 12 | `SubmitAggregationJob` | [`SubmitAggregationJobReq`](#message-submitaggregationjobreq) | [`SubmitAggregationJobReply`](#message-submitaggregationjobreply) | 提交实时/离线聚合作业（request_id 幂等） |
| 13 | `GetAggregationJob` | [`GetAggregationJobReq`](#message-getaggregationjobreq) | [`GetAggregationJobReply`](#message-getaggregationjobreply) | 查询作业（按 job_id 或 request_id） |
| 14 | `ListAggregationJobs` | [`ListAggregationJobsReq`](#message-listaggregationjobsreq) | [`ListAggregationJobsReply`](#message-listaggregationjobsreply) | 作业列表（分页） |
| 15 | `ListConsumerState` | [`ListConsumerStateReq`](#message-listconsumerstatereq) | [`ListConsumerStateReply`](#message-listconsumerstatereply) | 消费状态汇总（位点与堆积） |
| 16 | `ListDeadLetters` | [`ListDeadLettersReq`](#message-listdeadlettersreq) | [`ListDeadLettersReply`](#message-listdeadlettersreply) | 死信留档查询（只读；重放属于 services/cron 的待接线项） |

## 消息与枚举

### enum `SubjectType`

> 聚合主体类型。跨服务只传主键，因此这里只声明「主键的语义」。

| 值 | 编号 | 说明 |
|---|---|---|
| `SUBJECT_TYPE_UNSPECIFIED` | 0 | 未指定，任何请求出现即拒绝 |
| `SUBJECT_TYPE_AID` | 1 | 稿件（video 服务的 aid） |
| `SUBJECT_TYPE_ZONE` | 2 | 分区（video/catalog 的 zone_id） |
| `SUBJECT_TYPE_MID` | 3 | 用户（account 的 mid，用于创作者维度指标） |
| `SUBJECT_TYPE_CATALOG_ITEM` | 4 | 版权内容条目（catalog 的 item_id） |

### enum `WindowType`

> 统计窗口粒度。window_start 必须是该粒度的左边界（由服务端规整并回显）。

| 值 | 编号 | 说明 |
|---|---|---|
| `WINDOW_TYPE_UNSPECIFIED` | 0 | 未指定，任何请求出现即拒绝 |
| `WINDOW_TYPE_5_MIN` | 1 | 实时窗口：5 分钟 |
| `WINDOW_TYPE_HOUR` | 2 | 实时窗口：1 小时 |
| `WINDOW_TYPE_DAY` | 3 | 天级窗口（离线回填与留存口径的默认粒度） |
| `WINDOW_TYPE_WEEK` | 4 | 自然周 |
| `WINDOW_TYPE_TOTAL` | 5 | 全量累计（window_start 固定为 0） |

### enum `MetricSource`

> 指标写入来源。写回通道只接受计算链路，不接受人工覆盖： / 「运营觉得某个稿件该火」不是指标数据源（AGENTS.md §7 第 3 条）。

| 值 | 编号 | 说明 |
|---|---|---|
| `METRIC_SOURCE_UNSPECIFIED` | 0 | 未指定，拒绝 |
| `METRIC_SOURCE_REALTIME` | 1 | 本服务实时窗口聚合器（读 spm_behavior_event） |
| `METRIC_SOURCE_OFFLINE` | 2 | 离线回填作业（外部分析引擎，见 README「契约缺口」） |
| `METRIC_SOURCE_RECOMPUTE` | 3 | 从事实表重算的修复链路（RecomputeMetrics） |

### enum `DefinitionState`

> 指标口径版本状态。没有 DELETE 语义：历史窗口的解释依赖口径定义持续可查。

| 值 | 编号 | 说明 |
|---|---|---|
| `DEFINITION_STATE_UNSPECIFIED` | 0 | 未指定 |
| `DEFINITION_STATE_DRAFT` | 1 | 已登记但未参与对外读 |
| `DEFINITION_STATE_ACTIVE` | 2 | 可被读接口取到 |
| `DEFINITION_STATE_RETIRED` | 3 | 只保留口径说明，不再写入 |

### enum `JobType`

> 聚合作业类型。

| 值 | 编号 | 说明 |
|---|---|---|
| `JOB_TYPE_UNSPECIFIED` | 0 | 未指定，拒绝 |
| `JOB_TYPE_REALTIME` | 1 | 实时窗口聚合（本服务内部调度或 cron 触发） |
| `JOB_TYPE_OFFLINE_BACKFILL` | 2 | 离线回填（含历史窗口重算） |
| `JOB_TYPE_RECOMPUTE` | 3 | 由 RecomputeMetrics 派生的修复作业 |

### enum `JobState`

> 作业状态机：PENDING → RUNNING → SUCCEEDED / FAILED / CANCELLED，终态不可回退。

| 值 | 编号 | 说明 |
|---|---|---|
| `JOB_STATE_UNSPECIFIED` | 0 | — |
| `JOB_STATE_PENDING` | 1 | — |
| `JOB_STATE_RUNNING` | 2 | — |
| `JOB_STATE_SUCCEEDED` | 3 | — |
| `JOB_STATE_FAILED` | 4 | — |
| `JOB_STATE_CANCELLED` | 5 | — |

### enum `ConsumerState`

> 消费状态（docs/api-and-events.md §6 要求的最小状态集合）。

| 值 | 编号 | 说明 |
|---|---|---|
| `CONSUMER_STATE_UNSPECIFIED` | 0 | — |
| `CONSUMER_STATE_RECEIVED` | 1 | — |
| `CONSUMER_STATE_PROCESSING` | 2 | — |
| `CONSUMER_STATE_SUCCEEDED` | 3 | — |
| `CONSUMER_STATE_RETRY` | 4 | — |
| `CONSUMER_STATE_DEAD_LETTER` | 5 | — |

### message `EmptyReply`

> 空响应

（空消息）

### message `MetricPoint`

> --- 指标值 --- / MetricPoint 一个「主体 × 指标口径版本 × 窗口」的取值。 / 比率类指标必须同时给出 numerator/denominator，否则跨窗口合并只能靠加权平均近似， / 会让完播率等口径在回填后对不上（这是重算链路的正确性前提）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `metric_key` | `string` | 1 | — | 指标键，如 play_finish_rate |
| `metric_version` | `int32` | 2 | — | 口径版本 |
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 3 | — | 主体类型 |
| `subject_id` | `int64` | 4 | — | 主体主键（aid/zone_id/mid/catalog_item_id） |
| `window_type` | [`WindowType`](#enum-windowtype) | 5 | — | 窗口粒度 |
| `window_start` | `int64` | 6 | — | 窗口左边界（Unix 秒，已按粒度规整） |
| `value` | `double` | 7 | — | 指标值（计数类同样落在这一列） |
| `numerator` | `int64` | 8 | — | 分子（比率类必填，计数类填 0） |
| `denominator` | `int64` | 9 | — | 分母（比率类必填，计数类填 0） |
| `sample_count` | `int64` | 10 | — | 参与聚合的样本数（事件条数） |
| `event_time` | `int64` | 11 | — | 该窗口最后一次推进时间（Unix 秒） |

### message `GetMetricReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 1 | — | 必填 |
| `subject_id` | `int64` | 2 | — | 必填 |
| `metric_key` | `string` | 3 | — | 必填 |
| `metric_version` | `int32` | 4 | — | 0 = 使用当前 ACTIVE 版本 |
| `window_type` | [`WindowType`](#enum-windowtype) | 5 | — | 必填 |
| `window_start` | `int64` | 6 | — | 0 = 最近一个已闭合窗口 |

### message `GetMetricReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `point` | [`MetricPoint`](#message-metricpoint) | 1 | — | — |
| `found` | `bool` | 2 | — | false 时 point 为零值：口径没有该窗口数据，不伪造 0 |

### message `BatchGetMetricsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 1 | — | 必填 |
| `subject_id` | `int64` | 2 | — | 必填 |
| `keys` | [`BatchGetMetricsReq.Key`](#message-batchgetmetricsreqkey) | 3 | repeated | 最多 50 个口径 |
| `window_type` | [`WindowType`](#enum-windowtype) | 4 | — | 必填 |
| `window_start_from` | `int64` | 5 | — | 起始窗口（含），0 = 最近窗口 |
| `window_count` | `int32` | 6 | — | 连续窗口数，1..30 |

### message `BatchGetMetricsReq.Key`

> 单个主体的一组指标；subject 维度批量由调用方分多次请求。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `metric_key` | `string` | 1 | — | — |
| `metric_version` | `int32` | 2 | — | 0 = ACTIVE 版本 |

### message `BatchGetMetricsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `points` | [`map<string, MetricPoint>`](#message-metricpoint) | 1 | — | key = "<metric_key>@v<metric_version>:<window_start>"，缺数据的窗口不出现。 |

### message `ListHotSubjectsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 1 | — | 只读榜单投影：按指标值倒序取主体主键，供推荐召回/分区页使用。 / 必填 |
| `metric_key` | `string` | 2 | — | 必填 |
| `metric_version` | `int32` | 3 | — | 0 = ACTIVE 版本 |
| `window_type` | [`WindowType`](#enum-windowtype) | 4 | — | 必填 |
| `window_start` | `int64` | 5 | — | 0 = 最近一个已闭合窗口 |
| `zone_id` | `int64` | 6 | — | 0 = 不限分区（仅 subject_type=AID/CATALOG_ITEM 时有意义） |
| `pn` | `int32` | 7 | — | 从 1 开始 |
| `ps` | `int32` | 8 | — | 每页大小，上限 100 |

### message `ListHotSubjectsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `subjects` | [`ListHotSubjectsReply.HotSubject`](#message-listhotsubjectsreplyhotsubject) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `window_start` | `int64` | 3 | — | 实际使用的窗口左边界 |
| `metric_version` | `int32` | 4 | — | 实际使用的口径版本 |

### message `ListHotSubjectsReply.HotSubject`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `subject_id` | `int64` | 1 | — | 主体主键 |
| `value` | `double` | 2 | — | 指标值 |
| `numerator` | `int64` | 3 | — | — |
| `denominator` | `int64` | 4 | — | — |
| `rank` | `int32` | 5 | — | 该窗口内的名次（从 1 开始） |

### message `GetUserInterestReq`

> --- 用户兴趣（脱敏后的画像投影） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 必填 |
| `metric_version` | `int32` | 2 | — | 0 = ACTIVE 版本 |
| `top_n` | `int32` | 3 | — | 返回权重最高的 N 个兴趣标签，上限 100 |

### message `GetUserInterestReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `interests` | [`GetUserInterestReply.Interest`](#message-getuserinterestreplyinterest) | 1 | repeated | — |
| `metric_version` | `int32` | 2 | — | — |
| `stale` | `bool` | 3 | — | true = 画像已过期（超过留存窗口），调用方应按冷启动策略处理 |

### message `GetUserInterestReply.Interest`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `interest_key` | `string` | 1 | — | 兴趣键：zone:<id> / tag:<id> / up:<mid> 等受控词表 |
| `weight` | `double` | 2 | — | 归一化权重 |
| `sample_count` | `int64` | 3 | — | 支撑该兴趣的样本数 |
| `event_time` | `int64` | 4 | — | 最近一次更新时间（Unix 秒） |

### message `GetRetentionReq`

> --- 留存 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `cohort_type` | [`GetRetentionReq.CohortType`](#enum-getretentionreqcohorttype) | 1 | — | 必填 |
| `cohort_date` | `int64` | 2 | — | 分桶日（Unix 秒，服务端按天规整） |
| `max_day` | `int32` | 3 | — | 取到第 N 日留存，1..90 |
| `metric_version` | `int32` | 4 | — | 0 = ACTIVE 版本 |
| `zone_id` | `int64` | 5 | — | 0 = 全站 |

### enum `GetRetentionReq.CohortType`

> cohort 维度：注册日 / 首次播放日 / 指定行为首日。

| 值 | 编号 | 说明 |
|---|---|---|
| `COHORT_TYPE_UNSPECIFIED` | 0 | — |
| `COHORT_TYPE_REGISTER_DAY` | 1 | 按注册日分桶 |
| `COHORT_TYPE_FIRST_PLAY_DAY` | 2 | 按首次播放日分桶 |

### message `GetRetentionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `points` | [`GetRetentionReply.RetentionPoint`](#message-getretentionreplyretentionpoint) | 1 | repeated | — |
| `metric_version` | `int32` | 2 | — | — |
| `cohort_date` | `int64` | 3 | — | 实际使用的分桶日 |

### message `GetRetentionReply.RetentionPoint`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `day_offset` | `int32` | 1 | — | 第 N 日（0 = cohort 当日） |
| `cohort_size` | `int64` | 2 | — | 分桶规模 |
| `retained` | `int64` | 3 | — | 第 N 日仍活跃数 |
| `rate` | `double` | 4 | — | retained / cohort_size |

### message `WriteMetricWindowReq`

> --- 指标写入与修复 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `points` | [`MetricPoint`](#message-metricpoint) | 1 | repeated | 聚合链路写回窗口指标（本服务内部聚合器、离线回填引擎、重算作业）。 / 幂等：uniq(subject_type, subject_id, metric_key, metric_version, window_type, / window_start) 命中时整行覆盖并保留 mtime 单调性——同窗口的重放必须是幂等覆盖， / 不能累加。晚于已写入 event_time 的迟到数据默认拒绝（allow_late_write=false）。 / 单次上限 500 |
| `source` | [`MetricSource`](#enum-metricsource) | 2 | — | 必填，且只能是计算链路来源 |
| `request_id` | `string` | 3 | — | 幂等键，必填 |
| `allow_late_write` | `bool` | 4 | — | 回填历史窗口时置 true |

### message `WriteMetricWindowReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `written` | `int32` | 1 | — | 实际写入行数 |
| `rejected` | `int32` | 2 | — | 因口径未登记/版本不 ACTIVE/迟到被拒的行数 |
| `rejected_keys` | `string` | 3 | repeated | 被拒行的 "<subject_type>:<subject_id>:<metric_key>" 摘要 |

### message `RecomputeMetricsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 1 | — | 计数/指标漂移的唯一修复入口（docs/data-design.md §5）：从 spm_behavior_event / 事实表按指定口径版本重算窗口，而不是人工改线上缓存。 / 必填 |
| `subject_id` | `int64` | 2 | — | 必填 |
| `metric_key` | `string` | 3 | — | 必填 |
| `metric_version` | `int32` | 4 | — | 必填（显式版本，避免悄悄按新版本改写历史） |
| `window_type` | [`WindowType`](#enum-windowtype) | 5 | — | 必填 |
| `window_start_from` | `int64` | 6 | — | 起始窗口（含） |
| `window_start_to` | `int64` | 7 | — | 结束窗口（含），0 = 当前时间 |
| `request_id` | `string` | 8 | — | 幂等键，必填 |
| `operator` | `string` | 9 | — | 触发者（system/cron/admin:<id>），仅留痕用 |

### message `RecomputeMetricsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job_id` | `int64` | 1 | — | 生成的修复作业，进度由 GetAggregationJob 查询 |
| `reused` | `bool` | 2 | — | true = request_id 命中已有作业 |
| `windows_planned` | `int32` | 3 | — | 计划重算的窗口数 |

### message `MetricDefinition`

> --- 指标口径注册表 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `metric_key` | `string` | 1 | — | 指标键 |
| `metric_version` | `int32` | 2 | — | 口径版本，>= 1 |
| `name` | `string` | 3 | — | 展示名 |
| `formula` | `string` | 4 | — | 口径公式说明（人读，必须写清分子/分母/去重键） |
| `unit` | `string` | 5 | — | 单位：count / ratio / seconds / score |
| `supported_windows` | [`WindowType`](#enum-windowtype) | 6 | repeated | 允许的窗口粒度 |
| `source_event_types` | `string` | 7 | — | 依赖的事件类型，CSV，必须落在 model.SupportedEventType |
| `state` | [`DefinitionState`](#enum-definitionstate) | 8 | — | 白名单内，如 "playback.heartbeat,behavior.exposure" |
| `description` | `string` | 9 | — | 变更说明：为什么需要新版本 |
| `created_by` | `string` | 10 | — | 登记人 |
| `ctime` | `int64` | 11 | — | Unix 秒 |
| `mtime` | `int64` | 12 | — | Unix 秒 |

### message `UpsertMetricDefinitionReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`MetricDefinition`](#message-metricdefinition) | 1 | — | 只能「新增版本」：命中已存在的 (metric_key, metric_version) 时， / 若公式/单位/窗口与登记值不一致返回 ErrMetricVersionImmutable，绝不覆盖。 / 必填，metric_version >= 1 |
| `operator` | `string` | 2 | — | 必填 |
| `request_id` | `string` | 3 | — | 幂等键，必填 |

### message `UpsertMetricDefinitionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `created` | `bool` | 1 | — | true = 新增了口径版本 |
| `reused` | `bool` | 2 | — | true = request_id 命中已有请求，definition 未变 |
| `definition` | [`MetricDefinition`](#message-metricdefinition) | 3 | — | — |

### message `UpdateMetricDefinitionStateReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `metric_key` | `string` | 1 | — | 必填 |
| `metric_version` | `int32` | 2 | — | 必填 |
| `state` | [`DefinitionState`](#enum-definitionstate) | 3 | — | 目标状态（不允许回到 UNSPECIFIED） |
| `operator` | `string` | 4 | — | 必填 |
| `reason` | `string` | 5 | — | 必填：状态变更理由 |
| `request_id` | `string` | 6 | — | 幂等键，必填 |

### message `UpdateMetricDefinitionStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`MetricDefinition`](#message-metricdefinition) | 1 | — | — |
| `reused` | `bool` | 2 | — | — |

### message `GetMetricDefinitionReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `metric_key` | `string` | 1 | — | 必填 |
| `metric_version` | `int32` | 2 | — | 0 = 当前 ACTIVE 版本 |

### message `GetMetricDefinitionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`MetricDefinition`](#message-metricdefinition) | 1 | — | — |
| `found` | `bool` | 2 | — | — |

### message `ListMetricDefinitionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `metric_key` | `string` | 1 | — | 空 = 不限 |
| `state` | [`DefinitionState`](#enum-definitionstate) | 2 | — | UNSPECIFIED = 不限 |
| `pn` | `int32` | 3 | — | — |
| `ps` | `int32` | 4 | — | 上限 100 |

### message `ListMetricDefinitionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definitions` | [`MetricDefinition`](#message-metricdefinition) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `SubmitAggregationJobReq`

> --- 聚合作业 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job_type` | [`JobType`](#enum-jobtype) | 1 | — | 必填 |
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 2 | — | 0 = 全部主体 |
| `subject_id` | `int64` | 3 | — | 0 = 不限主体 |
| `metric_key` | `string` | 4 | — | 空 = 该作业类型下的全部指标 |
| `metric_version` | `int32` | 5 | — | 0 = ACTIVE 版本 |
| `window_type` | [`WindowType`](#enum-windowtype) | 6 | — | 必填 |
| `window_start_from` | `int64` | 7 | — | 起始窗口（含） |
| `window_start_to` | `int64` | 8 | — | 结束窗口（含），0 = 当前时间 |
| `request_id` | `string` | 9 | — | 幂等键，必填 |
| `operator` | `string` | 10 | — | 触发者 |
| `reason` | `string` | 11 | — | 触发原因（回填范围说明、故障修复单号等） |

### message `SubmitAggregationJobReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job_id` | `int64` | 1 | — | — |
| `reused` | `bool` | 2 | — | — |
| `job` | [`AggregationJob`](#message-aggregationjob) | 3 | — | — |

### message `AggregationJob`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job_id` | `int64` | 1 | — | — |
| `job_type` | [`JobType`](#enum-jobtype) | 2 | — | — |
| `state` | [`JobState`](#enum-jobstate) | 3 | — | — |
| `subject_type` | [`SubjectType`](#enum-subjecttype) | 4 | — | — |
| `subject_id` | `int64` | 5 | — | — |
| `metric_key` | `string` | 6 | — | — |
| `metric_version` | `int32` | 7 | — | — |
| `window_type` | [`WindowType`](#enum-windowtype) | 8 | — | — |
| `window_start_from` | `int64` | 9 | — | — |
| `window_start_to` | `int64` | 10 | — | — |
| `windows_total` | `int32` | 11 | — | 计划窗口数 |
| `windows_done` | `int32` | 12 | — | 已完成窗口数 |
| `windows_failed` | `int32` | 13 | — | 失败窗口数 |
| `request_id` | `string` | 14 | — | — |
| `operator` | `string` | 15 | — | — |
| `reason` | `string` | 16 | — | — |
| `last_error` | `string` | 17 | — | 截断保存，不含堆栈与 SQL |
| `ctime` | `int64` | 18 | — | — |
| `mtime` | `int64` | 19 | — | — |
| `finished_at` | `int64` | 20 | — | — |

### message `GetAggregationJobReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job_id` | `int64` | 1 | — | 与 request_id 二选一 |
| `request_id` | `string` | 2 | — | 幂等回放用 |

### message `GetAggregationJobReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job` | [`AggregationJob`](#message-aggregationjob) | 1 | — | — |
| `found` | `bool` | 2 | — | — |

### message `ListAggregationJobsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job_type` | [`JobType`](#enum-jobtype) | 1 | — | UNSPECIFIED = 不限 |
| `state` | [`JobState`](#enum-jobstate) | 2 | — | UNSPECIFIED = 不限 |
| `since` | `int64` | 3 | — | 0 = 不限时间 |
| `pn` | `int32` | 4 | — | — |
| `ps` | `int32` | 5 | — | 上限 100 |

### message `ListAggregationJobsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `jobs` | [`AggregationJob`](#message-aggregationjob) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `ListConsumerStateReq`

> --- 消费链路可观测（只读） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic` | `string` | 1 | — | 按 topic 汇总消费状态，回答「事件消费到哪了、有没有堆积」。 / 空 = 全部 |
| `state` | [`ConsumerState`](#enum-consumerstate) | 2 | — | UNSPECIFIED = 全部 |
| `pn` | `int32` | 3 | — | — |
| `ps` | `int32` | 4 | — | 上限 100 |

### message `ListConsumerStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rows` | [`ListConsumerStateReply.Row`](#message-listconsumerstatereplyrow) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `ListConsumerStateReply.Row`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic` | `string` | 1 | — | — |
| `state` | [`ConsumerState`](#enum-consumerstate) | 2 | — | — |
| `count` | `int64` | 3 | — | 该状态的事件条数 |
| `oldest_ctime` | `int64` | 4 | — | 最早一条的接收时间（Unix 秒） |
| `last_msg_offset` | `int64` | 5 | — | 该 topic 已处理到的最大位点 |
| `last_event_time` | `int64` | 6 | — | 该 topic 最近一次推进时间 |

### message `ListDeadLettersReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic` | `string` | 1 | — | 空 = 全部 |
| `state` | `string` | 2 | — | open/replayed/ignored，空 = 全部 |
| `since` | `int64` | 3 | — | 0 = 不限 |
| `pn` | `int32` | 4 | — | — |
| `ps` | `int32` | 5 | — | 上限 100 |

### message `ListDeadLettersReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`ListDeadLettersReply.DeadLetter`](#message-listdeadlettersreplydeadletter) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `ListDeadLettersReply.DeadLetter`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `event_id` | `string` | 2 | — | 信封不可解析时为空串 |
| `event_type` | `string` | 3 | — | — |
| `topic` | `string` | 4 | — | — |
| `payload_digest` | `string` | 5 | — | sha256:<hex> |
| `payload_preview` | `string` | 6 | — | 脱敏前缀，不含行为原文与标识符 |
| `reason` | `string` | 7 | — | — |
| `state` | `string` | 8 | — | — |
| `ctime` | `int64` | 9 | — | — |
