# RPC · `feature-store`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/feature-store/rpc/featurestore.proto` |
| protobuf 包 | `featurestore.v1` |
| go_package | `go-video/services/feature-store/rpc` |
| 发现用的 etcd key | `featurestore.v1.rpc`（`services/feature-store/etc/featurestore.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`featurestore.v1.rpc`） |
| 监听 | `8130`（`services/feature-store/etc/featurestore.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_feature_store` |
| 方法数 | 16（service `FeatureStore`） |
| 网关消费方 | `admin:FeatureStoreRPC` |

## 契约说明

> feature-store：在线特征读写 + 特征定义/版本/回填的所有者。
>
> 硬边界（AGENTS.md §7、services/feature-store/README.md）
>   * 不保存广告特征、支付特征、会员/订单/投币/分成特征。注册特征时
>     FeatureSource 与 feature_key 都要过白名单校验，来源枚举里根本不提供这些语义，
>     新增来源必须先评审契约（见 RegisterFeature 注释）。
>   * 特征值只能由「上游计算链路」写入：spm 指标、离线模型产出、风控自有滑窗。
>     本契约不提供任何「把排序结果写回特征」的方法。
>   * privacy_level 是必填项且不可为空：未声明隐私级别的特征一律拒绝注册与写入。
>
> 数据所有权（AGENTS.md §5）
>   * 本服务拥有 6 张自有表，库名 go_video_feature_store：
>     feature_definition（特征定义与版本）、feature_value（特征值，含 TTL）、
>     feature_active_version（每个 feature_key 对外的 ACTIVE 版本指针）、
>     feature_version_switch（版本切换审计，只追加）、
>     feature_backfill_job（回填任务与断点）、feature_write_receipt（批量写幂等回执）。
>   * 跨服务只传主键与受控标识：mid/aid/zone_id 的十进制字符串、设备与 IP 的哈希摘要。
>     明文手机号、身份证、原始 IP、明文设备号禁止出现在任何字段里。
>   * spm 的行为指标、video 的稿件、risk-control 的名单都不在本库复制，
>     本库只保存「加工后的特征值 + 定义 + 版本 + 回填任务」。
>
> 读链路与缓存策略
>   * 主读 Redis（key 见 README「缓存」一节），DB 兜底；Redis 缺失或过期时回源
>     feature_value 并按 Spm.BatchGetMetrics 的窗口口径判断是否仍然可用。
>   * 任何降级都必须显式表达（FeatureDegradation），调用方据此决定是否降级到冷启动策略；
>     服务绝不把「读不到」伪装成「值为 0」。
>
> 通用约定
>   * 时间统一 Unix 秒；分页统一 pn（从 1 开始）/ ps（上限见方法注释）+ total。
>   * 写接口全部要求 request_id 幂等键。批量读有硬上限（MaxBatchEntries / MaxBatchEntities），
>     超上限直接报错而不是截断，避免调用方以为拿到了全集。

## service `FeatureStore`

> FeatureStore 在线特征读写、版本与回填服务。

gRPC 方法前缀：`featurestore.v1.FeatureStore/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `RegisterFeature` | [`RegisterFeatureReq`](#message-registerfeaturereq) | [`RegisterFeatureReply`](#message-registerfeaturereply) | 注册特征版本（privacy_level 必填，不可变字段命中冲突时拒绝） |
| 2 | `UpdateFeatureState` | [`UpdateFeatureStateReq`](#message-updatefeaturestatereq) | [`UpdateFeatureStateReply`](#message-updatefeaturestatereply) | 变更特征状态（DRAFT/ACTIVE/RETIRED） |
| 3 | `UpdateFeaturePrivacy` | [`UpdateFeaturePrivacyReq`](#message-updatefeatureprivacyreq) | [`UpdateFeaturePrivacyReply`](#message-updatefeatureprivacyreply) | 调整隐私级别（独立入口，单独留痕） |
| 4 | `GetFeatureDefinition` | [`GetFeatureDefinitionReq`](#message-getfeaturedefinitionreq) | [`GetFeatureDefinitionReply`](#message-getfeaturedefinitionreply) | 查询单个特征定义 |
| 5 | `ListFeatureDefinitions` | [`ListFeatureDefinitionsReq`](#message-listfeaturedefinitionsreq) | [`ListFeatureDefinitionsReply`](#message-listfeaturedefinitionsreply) | 特征定义列表（分页、按 scope/source/state/隐私级别过滤） |
| 6 | `WriteFeatures` | [`WriteFeaturesReq`](#message-writefeaturesreq) | [`WriteFeaturesReply`](#message-writefeaturesreply) | 批量写入特征值（request_id 整批幂等，逐行返回结果） |
| 7 | `GetFeature` | [`GetFeatureReq`](#message-getfeaturereq) | [`GetFeatureReply`](#message-getfeaturereply) | 读取单个特征值（缺失必降级，降级必须显式表达） |
| 8 | `BatchGetFeatures` | [`BatchGetFeaturesReq`](#message-batchgetfeaturesreq) | [`BatchGetFeaturesReply`](#message-batchgetfeaturesreply) | 批量读取（feature × entity 笛卡尔积，有硬上限） |
| 9 | `SwitchFeatureVersion` | [`SwitchFeatureVersionReq`](#message-switchfeatureversionreq) | [`SwitchFeatureVersionReply`](#message-switchfeatureversionreply) | 切换对外生效的版本（乐观校验 + 审计留痕） |
| 10 | `ListVersionSwitches` | [`ListVersionSwitchesReq`](#message-listversionswitchesreq) | [`ListVersionSwitchesReply`](#message-listversionswitchesreply) | 版本切换审计列表 |
| 11 | `SubmitBackfillJob` | [`SubmitBackfillJobReq`](#message-submitbackfilljobreq) | [`SubmitBackfillJobReply`](#message-submitbackfilljobreply) | 提交回填任务（request_id 幂等） |
| 12 | `GetBackfillJob` | [`GetBackfillJobReq`](#message-getbackfilljobreq) | [`GetBackfillJobReply`](#message-getbackfilljobreply) | 查询回填任务（按 job_id 或 request_id） |
| 13 | `ListBackfillJobs` | [`ListBackfillJobsReq`](#message-listbackfilljobsreq) | [`ListBackfillJobsReply`](#message-listbackfilljobsreply) | 回填任务列表（分页） |
| 14 | `PurgeExpired` | [`PurgeExpiredReq`](#message-purgeexpiredreq) | [`PurgeExpiredReply`](#message-purgeexpiredreply) | 清理 TTL 过期值（cron 调用） |
| 15 | `EraseEntityFeatures` | [`EraseEntityFeaturesReq`](#message-eraseentityfeaturesreq) | [`EraseEntityFeaturesReply`](#message-eraseentityfeaturesreply) | 按主体删除个体特征（隐私工单执行） |
| 16 | `ListEntityFeatures` | [`ListEntityFeaturesReq`](#message-listentityfeaturesreq) | [`ListEntityFeaturesReply`](#message-listentityfeaturesreply) | 按主体导出特征（隐私核对） |

## 消息与枚举

### enum `EntityScope`

> 特征主体类型。entity_id 是字符串，因为设备/IP 维度只有哈希摘要形态。

| 值 | 编号 | 说明 |
|---|---|---|
| `ENTITY_SCOPE_UNSPECIFIED` | 0 | 未指定，任何请求出现即拒绝 |
| `ENTITY_SCOPE_MID` | 1 | 用户，entity_id = mid 十进制串 |
| `ENTITY_SCOPE_AID` | 2 | 稿件，entity_id = aid 十进制串 |
| `ENTITY_SCOPE_ZONE` | 3 | 分区，entity_id = zone_id 十进制串 |
| `ENTITY_SCOPE_DEVICE` | 4 | 设备，entity_id = 设备哈希摘要（禁止明文设备号） |
| `ENTITY_SCOPE_QUERY` | 5 | 搜索词，entity_id = 归一化后的词 |
| `ENTITY_SCOPE_CATALOG_ITEM` | 6 | 版权内容条目，entity_id = item_id 十进制串 |
| `ENTITY_SCOPE_IP_HASH` | 7 | IP 摘要，entity_id = ip_hash（禁止原始 IP） |

### enum `FeatureValueType`

> 特征值类型。注册后不可变更：类型漂移会让历史值无法解释。

| 值 | 编号 | 说明 |
|---|---|---|
| `FEATURE_VALUE_TYPE_UNSPECIFIED` | 0 | — |
| `FEATURE_VALUE_TYPE_INT64` | 1 | — |
| `FEATURE_VALUE_TYPE_DOUBLE` | 2 | — |
| `FEATURE_VALUE_TYPE_BOOL` | 3 | — |
| `FEATURE_VALUE_TYPE_STRING` | 4 | — |
| `FEATURE_VALUE_TYPE_INT64_LIST` | 5 | 定长 ID 列表（如最近点击的 aid） |
| `FEATURE_VALUE_TYPE_DOUBLE_LIST` | 6 | 定长数值向量（维度受定义约束，不引入外部向量库） |

### enum `FeatureSource`

> 特征来源。隐私与范围双重约束：不存在 AD / PAYMENT / MEMBERSHIP 等来源。

| 值 | 编号 | 说明 |
|---|---|---|
| `FEATURE_SOURCE_UNSPECIFIED` | 0 | 未指定，拒绝 |
| `FEATURE_SOURCE_SPM_METRIC` | 1 | spm 指标投影（内容热度、完播率等） |
| `FEATURE_SOURCE_SPM_INTEREST` | 2 | spm 用户兴趣画像 |
| `FEATURE_SOURCE_SPM_RETENTION` | 3 | spm 留存口径 |
| `FEATURE_SOURCE_OFFLINE_MODEL` | 4 | 离线模型产出（经回填作业导入） |
| `FEATURE_SOURCE_REALTIME_RULE` | 5 | 本服务/上游实时规则滑窗（如风控滑窗计数） |
| `FEATURE_SOURCE_STATIC_CONFIG` | 6 | 运营静态配置（内容属性、分区先验等非行为数据） |

### enum `PrivacyLevel`

> 隐私级别（必填）。数值越大越敏感，读写两侧的约束越严。

| 值 | 编号 | 说明 |
|---|---|---|
| `PRIVACY_LEVEL_UNSPECIFIED` | 0 | 未声明：RegisterFeature/WriteFeatures 一律拒绝 |
| `PRIVACY_LEVEL_PUBLIC_AGGREGATE` | 1 | 全站或内容级聚合，不含个体：热度、完播率 |
| `PRIVACY_LEVEL_CONTENT_ATTRIBUTE` | 2 | 内容属性派生，与个体身份无关 |
| `PRIVACY_LEVEL_PSEUDONYMOUS` | 3 | 与受控标识（设备哈希/IP 摘要）关联，可反推到设备 |
| `PRIVACY_LEVEL_USER_PROFILE` | 4 | 与 mid 关联的个体画像，最敏感：只允许白名单调用方读 |

### enum `FeatureDegradation`

> 降级原因。读接口的每条结果都必须带该字段，NONE 之外的值都要求调用方按冷启动处理。

| 值 | 编号 | 说明 |
|---|---|---|
| `FEATURE_DEGRADATION_UNSPECIFIED` | 0 | — |
| `FEATURE_DEGRADATION_NONE` | 1 | 正常值 |
| `FEATURE_DEGRADATION_DEFAULT_VALUE` | 2 | 无该主体特征，返回定义里的 default_value |
| `FEATURE_DEGRADATION_PREVIOUS_VERSION` | 3 | 目标版本无值，回退到上一个 ACTIVE 版本 |
| `FEATURE_DEGRADATION_EXPIRED` | 4 | 已超过 TTL，返回旧值仅供兜底，不可用于训练 |
| `FEATURE_DEGRADATION_SOURCE_UNAVAILABLE` | 5 | 上游（DB/Redis/spm）不可用，返回上次快照或默认值 |
| `FEATURE_DEGRADATION_FEATURE_RETIRED` | 6 | 特征已下线，返回默认值 |

### enum `FeatureState`

> 特征状态机：DRAFT → ACTIVE → RETIRED。RETIRED 不再接受写入，读侧返回默认值并标降级。

| 值 | 编号 | 说明 |
|---|---|---|
| `FEATURE_STATE_UNSPECIFIED` | 0 | — |
| `FEATURE_STATE_DRAFT` | 1 | — |
| `FEATURE_STATE_ACTIVE` | 2 | — |
| `FEATURE_STATE_RETIRED` | 3 | — |

### enum `BackfillState`

> 回填作业状态机：PENDING → RUNNING → SUCCEEDED / FAILED / CANCELLED。

| 值 | 编号 | 说明 |
|---|---|---|
| `BACKFILL_STATE_UNSPECIFIED` | 0 | — |
| `BACKFILL_STATE_PENDING` | 1 | — |
| `BACKFILL_STATE_RUNNING` | 2 | — |
| `BACKFILL_STATE_SUCCEEDED` | 3 | — |
| `BACKFILL_STATE_FAILED` | 4 | — |
| `BACKFILL_STATE_CANCELLED` | 5 | — |

### message `EmptyReply`

> 空响应

（空消息）

### message `FeatureDefinition`

> --- 特征定义 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | 特征键，如 u_play_finish_rate_7d |
| `version` | `int32` | 2 | — | 版本号，>= 1；同一 key 可多版本共存 |
| `name` | `string` | 3 | — | 展示名 |
| `value_type` | [`FeatureValueType`](#enum-featurevaluetype) | 4 | — | 值类型，注册后不可变 |
| `entity_scope` | [`EntityScope`](#enum-entityscope) | 5 | — | 主体类型，注册后不可变 |
| `source` | [`FeatureSource`](#enum-featuresource) | 6 | — | 来源，注册后不可变 |
| `privacy_level` | [`PrivacyLevel`](#enum-privacylevel) | 7 | — | 隐私级别，必填且不可为 UNSPECIFIED；可经审计提升/降低 |
| `window_seconds` | `int64` | 8 | — | 统计时间窗口（秒），0 = 无窗口（静态属性） |
| `ttl_seconds` | `int64` | 9 | — | 值存活时间（秒），<=0 视为「未声明 TTL」并拒绝注册 |
| `default_value` | `string` | 10 | — | 缺失降级用的默认值（按 value_type 序列化的字符串形态） |
| `dimension` | `int32` | 11 | — | 列表/向量类特征的元素个数上限，标量类填 0 |
| `state` | [`FeatureState`](#enum-featurestate) | 12 | — | — |
| `description` | `string` | 13 | — | 口径说明：为什么存在、怎么算 |
| `change_note` | `string` | 14 | — | 本版本的变更说明 |
| `created_by` | `string` | 15 | — | — |
| `ctime` | `int64` | 16 | — | — |
| `mtime` | `int64` | 17 | — | — |

### message `RegisterFeatureReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`FeatureDefinition`](#message-featuredefinition) | 1 | — | 新增特征版本。(feature_key, version) 已存在时：定义完全一致则幂等返回， / 任一不可变字段（value_type/entity_scope/source/window_seconds/dimension）不同则 / 返回 ErrFeatureDefinitionImmutable。已有版本永不原地改写。 / 必填 |
| `operator` | `string` | 2 | — | 必填（登记人，admin:<id> 或 system:<svc>） |
| `request_id` | `string` | 3 | — | 幂等键，必填 |

### message `RegisterFeatureReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `created` | `bool` | 1 | — | true = 新增了版本 |
| `reused` | `bool` | 2 | — | true = request_id 命中已有请求 |
| `definition` | [`FeatureDefinition`](#message-featuredefinition) | 3 | — | — |

### message `UpdateFeatureStateReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | 必填 |
| `version` | `int32` | 2 | — | 必填 |
| `state` | [`FeatureState`](#enum-featurestate) | 3 | — | 目标状态 |
| `operator` | `string` | 4 | — | 必填 |
| `reason` | `string` | 5 | — | 必填 |
| `request_id` | `string` | 6 | — | 幂等键，必填 |

### message `UpdateFeatureStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`FeatureDefinition`](#message-featuredefinition) | 1 | — | — |
| `reused` | `bool` | 2 | — | — |

### message `UpdateFeaturePrivacyReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | 隐私级别调整是独立入口：它不改变值语义，但改变谁能读，必须单独留痕。 / 必填 |
| `version` | `int32` | 2 | — | 必填 |
| `privacy_level` | [`PrivacyLevel`](#enum-privacylevel) | 3 | — | 必填，不允许 UNSPECIFIED |
| `operator` | `string` | 4 | — | — |
| `reason` | `string` | 5 | — | — |
| `request_id` | `string` | 6 | — | — |

### message `UpdateFeaturePrivacyReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`FeatureDefinition`](#message-featuredefinition) | 1 | — | — |
| `reused` | `bool` | 2 | — | — |

### message `GetFeatureDefinitionReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | 必填 |
| `version` | `int32` | 2 | — | 0 = 当前 ACTIVE 版本 |

### message `GetFeatureDefinitionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`FeatureDefinition`](#message-featuredefinition) | 1 | — | — |
| `found` | `bool` | 2 | — | — |

### message `ListFeatureDefinitionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key_prefix` | `string` | 1 | — | 空 = 不限 |
| `entity_scope` | [`EntityScope`](#enum-entityscope) | 2 | — | UNSPECIFIED = 不限 |
| `source` | [`FeatureSource`](#enum-featuresource) | 3 | — | UNSPECIFIED = 不限 |
| `state` | [`FeatureState`](#enum-featurestate) | 4 | — | UNSPECIFIED = 不限 |
| `max_privacy_level` | [`PrivacyLevel`](#enum-privacylevel) | 5 | — | 未填 = 不限；调用方按自身授权收敛 |
| `pn` | `int32` | 6 | — | — |
| `ps` | `int32` | 7 | — | 上限 100 |

### message `ListFeatureDefinitionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definitions` | [`FeatureDefinition`](#message-featuredefinition) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `FeatureValue`

> --- 特征值 --- / FeatureValue 承载一个特征值。必须按 value_type 填充对应字段，多余填充视为格式错误。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `value_type` | [`FeatureValueType`](#enum-featurevaluetype) | 1 | — | — |
| `int64_value` | `int64` | 2 | — | — |
| `double_value` | `double` | 3 | — | — |
| `bool_value` | `bool` | 4 | — | — |
| `string_value` | `string` | 5 | — | — |
| `int64_list` | `int64` | 6 | repeated | — |
| `double_list` | `double` | 7 | repeated | — |

### message `EntityRef`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entity_scope` | [`EntityScope`](#enum-entityscope) | 1 | — | 必填 |
| `entity_id` | `string` | 2 | — | 必填：主键十进制串或受控哈希摘要 |

### message `FeatureRef`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | 必填 |
| `version` | `int32` | 2 | — | 0 = 当前 ACTIVE 版本 |

### message `GetFeatureReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature` | [`FeatureRef`](#message-featureref) | 1 | — | 必填 |
| `entity` | [`EntityRef`](#message-entityref) | 2 | — | 必填 |
| `allow_stale` | `bool` | 3 | — | true = TTL 过期时仍返回旧值并标 EXPIRED；默认 false |

### message `FeatureEntry`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature` | [`FeatureRef`](#message-featureref) | 1 | — | — |
| `entity` | [`EntityRef`](#message-entityref) | 2 | — | — |
| `value` | [`FeatureValue`](#message-featurevalue) | 3 | — | — |
| `resolved_version` | `int32` | 4 | — | 实际返回的版本（可能与请求不同，见 degradation） |
| `degradation` | [`FeatureDegradation`](#enum-featuredegradation) | 5 | — | 必有值；NONE 表示正常 |
| `event_time` | `int64` | 6 | — | 该值的产出时间（Unix 秒） |
| `expire_at` | `int64` | 7 | — | TTL 到期时间（Unix 秒），0 = 未设 |
| `source_metric_key` | `string` | 8 | — | 上游口径追溯：来自 spm 时是 "<metric_key>@v<n>" |
| `ttl_seconds` | `int64` | 9 | — | 定义里的 TTL，便于调用方判断新鲜度 |

### message `GetFeatureReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entry` | [`FeatureEntry`](#message-featureentry) | 1 | — | entry 一定存在：找不到时返回降级条目（default/previous_version）， / 由 degradation 表达真实情况，绝不返回空 reply 让调用方猜。 |
| `found` | `bool` | 2 | — | false 表示只能给默认值级别的降级 |

### message `BatchGetFeaturesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `features` | [`FeatureRef`](#message-featureref) | 1 | repeated | 交叉读：feature × entity 的笛卡尔积，条目数上限 Spm 风格的硬限制（见 README）。 / 最多 50 |
| `entities` | [`EntityRef`](#message-entityref) | 2 | repeated | 最多 20，且 entity_scope 必须与 feature 定义的 scope 匹配 |
| `allow_stale` | `bool` | 3 | — | — |

### message `BatchGetFeaturesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entries` | [`FeatureEntry`](#message-featureentry) | 1 | repeated | 缺失项以降级条目补齐，调用方可按 feature_key+entity_id 索引 |
| `requested` | `int32` | 2 | — | 请求的笛卡尔积条目数 |
| `degraded` | `int32` | 3 | — | 其中降级条目数 |

### message `FeatureWrite`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature` | [`FeatureRef`](#message-featureref) | 1 | — | — |
| `entity` | [`EntityRef`](#message-entityref) | 2 | — | — |
| `value` | [`FeatureValue`](#message-featurevalue) | 3 | — | — |
| `event_time` | `int64` | 4 | — | 该特征值的产出时间（Unix 秒），0 = 服务端当前时间 |
| `source_metric_key` | `string` | 5 | — | 上游口径追溯，来自 spm 时必填 "<metric_key>@v<n>" |

### message `WriteFeaturesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `writes` | [`FeatureWrite`](#message-featurewrite) | 1 | repeated | 批量写：整批按 request_id 幂等。命中已完成的 request_id 时不重复写， / 返回首次结果（reused=true）。逐行结果在 results 里，格式/授权错误不会中断整批。 / 单次上限 500 行 |
| `writer` | [`FeatureSource`](#enum-featuresource) | 2 | — | 写入方身份，必须与特征定义的 source 一致或为受控系统 |
| `request_id` | `string` | 3 | — | 幂等键，必填 |
| `operator` | `string` | 4 | — | 写入者（system:<svc> / offline-job:<id>） |

### message `WriteFeaturesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `written` | `int32` | 1 | — | — |
| `rejected` | `int32` | 2 | — | — |
| `results` | [`WriteFeaturesReply.RowResult`](#message-writefeaturesreplyrowresult) | 3 | repeated | — |
| `reused` | `bool` | 4 | — | — |

### message `WriteFeaturesReply.RowResult`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | — |
| `entity_id` | `string` | 2 | — | — |
| `ok` | `bool` | 3 | — | — |
| `error` | `string` | 4 | — | 哨兵错误的短码，不含 SQL 与特征值原文 |

### message `SwitchFeatureVersionReq`

> --- 版本切换 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | 原子切换某个 feature_key 对外提供读的版本：ACTIVE 版本指针从 from 移到 to。 / expected_from_version 是乐观并发控制：与服务端当前值不一致时失败，不静默覆盖。 / 必填 |
| `from_version` | `int32` | 2 | — | 必填 |
| `to_version` | `int32` | 3 | — | 必填，目标版本必须已 ACTIVE 且不可变字段与 from 一致 |
| `expected_from_version` | `int32` | 4 | — | 乐观校验；0 = 不校验（仅回填作业内部使用） |
| `operator` | `string` | 5 | — | 必填 |
| `reason` | `string` | 6 | — | 必填：切换依据（离线评估结论、回滚单号） |
| `request_id` | `string` | 7 | — | 幂等键，必填 |

### message `SwitchFeatureVersionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `switched` | `bool` | 1 | — | — |
| `reused` | `bool` | 2 | — | — |
| `active_version` | `int32` | 3 | — | 切换后对外生效的版本 |
| `switch_id` | `int64` | 4 | — | — |

### message `ListVersionSwitchesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | 空 = 全部 |
| `since` | `int64` | 2 | — | 0 = 不限 |
| `pn` | `int32` | 3 | — | — |
| `ps` | `int32` | 4 | — | 上限 100 |

### message `ListVersionSwitchesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`ListVersionSwitchesReply.SwitchRecord`](#message-listversionswitchesreplyswitchrecord) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `ListVersionSwitchesReply.SwitchRecord`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `switch_id` | `int64` | 1 | — | — |
| `feature_key` | `string` | 2 | — | — |
| `from_version` | `int32` | 3 | — | — |
| `to_version` | `int32` | 4 | — | — |
| `operator` | `string` | 5 | — | — |
| `reason` | `string` | 6 | — | — |
| `request_id` | `string` | 7 | — | — |
| `ctime` | `int64` | 8 | — | — |

### message `SubmitBackfillJobReq`

> --- 回填任务 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | 为某个特征版本补历史值。source 说明从哪里取数： / 离线快照走「可替换的分析存储接口」（当前为 stub，见 README「契约缺口」）。 / 必填 |
| `version` | `int32` | 2 | — | 必填，目标版本必须处于 DRAFT（回填完成后才允许切 ACTIVE） |
| `entity_scope` | [`EntityScope`](#enum-entityscope) | 3 | — | 必填 |
| `entity_ids` | `string` | 4 | repeated | 空 = 全量扫描；显式列表时上限 1000 |
| `window_from` | `int64` | 5 | — | 回填数据的时间范围起点（Unix 秒） |
| `window_to` | `int64` | 6 | — | 0 = 当前时间 |
| `source` | [`FeatureSource`](#enum-featuresource) | 7 | — | 取数来源，必须与特征定义一致 |
| `auto_switch` | `bool` | 8 | — | true = 回填成功后自动切 ACTIVE（要求 from_version 唯一确定） |
| `from_version` | `int32` | 9 | — | auto_switch 时的乐观校验基线，0 = 不校验 |
| `request_id` | `string` | 10 | — | 幂等键，必填 |
| `operator` | `string` | 11 | — | 必填 |
| `reason` | `string` | 12 | — | 必填 |

### message `SubmitBackfillJobReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job_id` | `int64` | 1 | — | — |
| `reused` | `bool` | 2 | — | — |
| `job` | [`BackfillJob`](#message-backfilljob) | 3 | — | — |

### message `BackfillJob`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job_id` | `int64` | 1 | — | — |
| `feature_key` | `string` | 2 | — | — |
| `version` | `int32` | 3 | — | — |
| `entity_scope` | [`EntityScope`](#enum-entityscope) | 4 | — | — |
| `source` | [`FeatureSource`](#enum-featuresource) | 5 | — | — |
| `state` | [`BackfillState`](#enum-backfillstate) | 6 | — | — |
| `window_from` | `int64` | 7 | — | — |
| `window_to` | `int64` | 8 | — | — |
| `entities_total` | `int64` | 9 | — | 计划处理主体数，扫描前为 0 |
| `entities_done` | `int64` | 10 | — | — |
| `entities_failed` | `int64` | 11 | — | — |
| `cursor_entity_id` | `int64` | 12 | — | 断点续跑的游标（按主键升序） |
| `auto_switch` | `bool` | 13 | — | — |
| `request_id` | `string` | 14 | — | — |
| `operator` | `string` | 15 | — | — |
| `reason` | `string` | 16 | — | — |
| `last_error` | `string` | 17 | — | 截断保存，不含 SQL 与特征值原文 |
| `ctime` | `int64` | 18 | — | — |
| `mtime` | `int64` | 19 | — | — |
| `finished_at` | `int64` | 20 | — | — |

### message `GetBackfillJobReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job_id` | `int64` | 1 | — | 与 request_id 二选一 |
| `request_id` | `string` | 2 | — | — |

### message `GetBackfillJobReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `job` | [`BackfillJob`](#message-backfilljob) | 1 | — | — |
| `found` | `bool` | 2 | — | — |

### message `ListBackfillJobsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `feature_key` | `string` | 1 | — | 空 = 全部 |
| `state` | [`BackfillState`](#enum-backfillstate) | 2 | — | UNSPECIFIED = 全部 |
| `since` | `int64` | 3 | — | — |
| `pn` | `int32` | 4 | — | — |
| `ps` | `int32` | 5 | — | 上限 100 |

### message `ListBackfillJobsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `jobs` | [`BackfillJob`](#message-backfilljob) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `PurgeExpiredReq`

> --- 生命周期与隐私 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `limit` | `int32` | 1 | — | TTL 过期值清理，由 services/cron 周期调用（本服务不写别人的表，别人也不能写本表）。 / 单次清理行数上限，最大 5000 |
| `before` | `int64` | 2 | — | 只清 expire_at < before 的行；0 = 当前时间 |
| `request_id` | `string` | 3 | — | 幂等键，必填（同一 request_id 重放返回首次结果） |
| `operator` | `string` | 4 | — | 必填 |

### message `PurgeExpiredReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `purged` | `int32` | 1 | — | 本次删除行数 |
| `remaining` | `int64` | 2 | — | 估计的剩余过期行数（用于收敛判断，允许近似） |
| `reused` | `bool` | 3 | — | — |

### message `EraseEntityFeaturesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entity` | [`EntityRef`](#message-entityref) | 1 | — | 隐私删除：抹掉某个主体的全部个体特征值（保留定义与审计）。 / 高隐私级别特征（PRIVACY_LEVEL_USER_PROFILE / PSEUDONYMOUS）只允许受控调用方触发。 / 必填 |
| `min_privacy_level` | [`PrivacyLevel`](#enum-privacylevel) | 2 | — | 只删 >= 该级别的特征；未填 = 全部个体特征 |
| `operator` | `string` | 3 | — | 必填（隐私工单执行者） |
| `reason` | `string` | 4 | — | 必填（工单号） |
| `request_id` | `string` | 5 | — | 幂等键，必填 |

### message `EraseEntityFeaturesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `erased_rows` | `int32` | 1 | — | — |
| `features_touched` | `int32` | 2 | — | — |
| `reused` | `bool` | 3 | — | — |

### message `ListEntityFeaturesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entity` | [`EntityRef`](#message-entityref) | 1 | — | 主体维度的特征导出（隐私自助查询/擦除前核对用）。只读，分页有上限。 / 必填 |
| `min_privacy_level` | [`PrivacyLevel`](#enum-privacylevel) | 2 | — | 未填 = 全部 |
| `pn` | `int32` | 3 | — | — |
| `ps` | `int32` | 4 | — | 上限 100 |

### message `ListEntityFeaturesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `entries` | [`FeatureEntry`](#message-featureentry) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
