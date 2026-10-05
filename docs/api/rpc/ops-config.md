# RPC · `ops-config`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/ops-config/rpc/opsconfig.proto` |
| protobuf 包 | `opsconfig.v1` |
| go_package | `go-video/services/ops-config/rpc` |
| 发现用的 etcd key | `opsconfig.v1.rpc`（`services/ops-config/etc/opsconfig.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`opsconfig.v1.rpc`） |
| 监听 | `8111`（`services/ops-config/etc/opsconfig.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_ops_config` |
| 方法数 | 20（service `OpsConfig`） |
| 网关消费方 | `admin:OpsConfigRPC` |

## 契约说明

> ops-config：内容运营配置服务（配置发布/版本/回滚/灰度、专题、推荐位与坑位、
> 客户端开关、缓存刷新）。
>
> 数据所有权（AGENTS.md §5）
>   * 本服务拥有 ops_config_item / ops_config_version / ops_rollout_rule /
>     ops_topic / ops_topic_item / ops_recommend_slot / ops_recommend_slot_item /
>     ops_client_switch，库名 go_video_ops_config。
>   * **分区与标签不在本服务**：catalog_zone / catalog_tag 由 catalog 持有
>     （见 deploy/migrations/catalog/000001_create_catalog_tables.sql 与
>     services/catalog/rpc/catalog.proto 的 ListZones/ListTags）。本服务只在
>     ops_topic.zone_ids / ops_topic.tag_ids 里保存 ID 引用，
>     不复制分区名/标签名等可变主资料；展示名由调用方经 catalog RPC 解析。
>   * 内容条目（稿件/作品/季/集）只以 item_type + item_id 引用，
>     真实存在性与上下架状态由 video / catalog / rights 判定，本服务不建副本。
>   * 管理员身份与 RBAC 归 operation；本服务不读写其表，只保存 operator_id 引用。
>
> 与 operation.op_config 的边界（待维护者裁决，见 README「所有权结论与待评审」）
>   * operation.op_config 是「运营后台自用的一行一键值对 + 乐观锁版本」，
>     没有发布/回滚/灰度/版本历史；本服务的 ops_config_item 是「带不可变版本快照、
>     灰度规则和缓存失效语义的发布式配置」。
>   * 本期不做数据搬迁（op_config 的迁移文件属于 deploy/migrations/operation，
>     不在本服务边界内），因此两侧并存。新增的**面向端展示与灰度**配置写本服务；
>     operation 既有键保持不动，等维护者确认后再按「迁移 + RPC」接管。
>
> 审计写入方（与 audit 的契约分工，避免两边都声称拥有）
>   * 本服务是自身配置动作的发起者，因此由**本服务**调用 audit.AppendAudit 写入
>     action_domain = ops_config 的审计条目（event_id 固定为
>     "ops-config:<request_id>:<action>"，同一次请求重放不会产生第二条审计）。
>   * 写入成功后把返回的 entry_id 记进 ops_config_version.audit_entry_id
>     （发布/回滚/挂灰度都产生版本行，形成双向可追链路）。
>     不产生版本行的动作（RefreshCache、SetRolloutRuleState、专题/坑位/开关的保存）
>     只把 entry_id 回给调用方：这类动作的事实源就是 audit 侧那条条目，
>     本服务不再复制一份「审计索引表」，避免同一事实出现两个所有者。
>   * audit 拥有条目本身；本服务不解释、不修改、不删除已写入的审计条目。
>   * 审计写失败**不回滚**业务写入（配置发布已成功），但必须打 Error 级日志并
>     把 audit_entry_id 留 0，由后续补偿任务重投；这样审计缺口可见而不是被静默吞掉。
>
> 商业化范围外（AGENTS.md §1）：推荐位/坑位只承载内容分发（UGC/PGC 条目、专题入口），
> 契约中不存在广告主、出价、排期购买、投放计费、分成等任何字段。
> 终端范围（AGENTS.md §1、§6）：平台枚举只覆盖 Android/iOS/HarmonyOS/桌面端，
> 项目不支持小程序；服务端不因平台不同返回任何 UI 结构或布局指令。

## service `OpsConfig`

> OpsConfig 运营配置服务：发布式配置、灰度与回滚、专题、推荐位、客户端开关、缓存刷新。

gRPC 方法前缀：`opsconfig.v1.OpsConfig/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `ResolveConfig` | [`ResolveConfigReq`](#message-resolveconfigreq) | [`ResolveConfigReply`](#message-resolveconfigreply) | 运行时解析单个配置（命中灰度规则，返回建议 TTL） |
| 2 | `BatchResolveConfig` | [`BatchResolveConfigReq`](#message-batchresolveconfigreq) | [`BatchResolveConfigReply`](#message-batchresolveconfigreply) | 批量解析（网关聚合用，最多 50 键） |
| 3 | `ListConfigs` | [`ListConfigsReq`](#message-listconfigsreq) | [`ListConfigsReply`](#message-listconfigsreply) | 后台分页列出配置项 |
| 4 | `PublishConfig` | [`PublishConfigReq`](#message-publishconfigreq) | [`PublishConfigReply`](#message-publishconfigreply) | 发布新版本（乐观锁 + 幂等 + 可选一并挂灰度规则） |
| 5 | `RollbackConfig` | [`RollbackConfigReq`](#message-rollbackconfigreq) | [`RollbackConfigReply`](#message-rollbackconfigreply) | 回滚到历史版本（生成新版本，不改写历史） |
| 6 | `ListConfigVersions` | [`ListConfigVersionsReq`](#message-listconfigversionsreq) | [`ListConfigVersionsReply`](#message-listconfigversionsreply) | 版本历史分页 |
| 7 | `SaveRolloutRule` | [`SaveRolloutRuleReq`](#message-saverolloutrulereq) | [`SaveRolloutRuleReply`](#message-saverolloutrulereply) | 新建/更新灰度规则（按 config_id + version + name upsert） |
| 8 | `ListRolloutRules` | [`ListRolloutRulesReq`](#message-listrolloutrulesreq) | [`ListRolloutRulesReply`](#message-listrolloutrulesreply) | 灰度规则分页查询 |
| 9 | `SetRolloutRuleState` | [`SetRolloutRuleStateReq`](#message-setrolloutrulestatereq) | [`SetRolloutRuleStateReply`](#message-setrolloutrulestatereply) | 启停灰度规则（软状态切换，保留放量证据） |
| 10 | `SaveTopic` | [`SaveTopicReq`](#message-savetopicreq) | [`SaveTopicReply`](#message-savetopicreply) | 新建/更新专题（zone_ids/tag_ids 只存引用） |
| 11 | `GetTopic` | [`GetTopicReq`](#message-gettopicreq) | [`GetTopicReply`](#message-gettopicreply) | 专题详情（可带条目） |
| 12 | `ListTopics` | [`ListTopicsReq`](#message-listtopicsreq) | [`ListTopicsReply`](#message-listtopicsreply) | 专题分页查询 |
| 13 | `SaveTopicItems` | [`SaveTopicItemsReq`](#message-savetopicitemsreq) | [`SaveTopicItemsReply`](#message-savetopicitemsreply) | 全量覆盖专题条目 |
| 14 | `SaveSlot` | [`SaveSlotReq`](#message-saveslotreq) | [`SaveSlotReply`](#message-saveslotreply) | 新建/更新推荐位定义 |
| 15 | `ListSlots` | [`ListSlotsReq`](#message-listslotsreq) | [`ListSlotsReply`](#message-listslotsreply) | 推荐位分页查询 |
| 16 | `SaveSlotItems` | [`SaveSlotItemsReq`](#message-saveslotitemsreq) | [`SaveSlotItemsReply`](#message-saveslotitemsreply) | 全量覆盖坑位条目与排期 |
| 17 | `ResolveSlot` | [`ResolveSlotReq`](#message-resolveslotreq) | [`ResolveSlotReply`](#message-resolveslotreply) | 运行时坑位视图（按端/版本/mid/时间过滤，只回引用） |
| 18 | `SaveClientSwitch` | [`SaveClientSwitchReq`](#message-saveclientswitchreq) | [`SaveClientSwitchReply`](#message-saveclientswitchreply) | 新建/更新客户端开关（按 switch_key + platform upsert） |
| 19 | `ListClientSwitches` | [`ListClientSwitchesReq`](#message-listclientswitchesreq) | [`ListClientSwitchesReply`](#message-listclientswitchesreply) | 客户端开关分页查询 |
| 20 | `RefreshCache` | [`RefreshCacheReq`](#message-refreshcachereq) | [`RefreshCacheReply`](#message-refreshcachereply) | 主动失效运行时缓存（必写审计） |

## 消息与枚举

### message `CallContext`

> CallContext 调用上下文。写接口必须带 request_id（幂等键）与 operator_id。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `operator_id` | `int64` | 1 | — | 操作管理员 admin_id（引用 operation.op_admin_user；0 时写接口拒绝） |
| `operator_name` | `string` | 2 | — | 展示用冗余（服务端不据此判权限） |
| `request_id` | `string` | 3 | — | 幂等键：所有写方法必填且非空 |
| `trace_id` | `string` | 4 | — | 链路 ID（透传给 audit 与日志） |
| `caller_service` | `string` | 5 | — | 调用方服务名（gateway/admin、cron ...） |
| `ip` | `string` | 6 | — | 来源 IP（本服务不落明文，只透传给 audit 做哈希） |

### enum `ClientPlatform`

> ClientPlatform 四类终端标识。服务端不写死任何端的 UI 行为， / 该枚举只用于「配置在哪些端生效」的范围界定。

| 值 | 编号 | 说明 |
|---|---|---|
| `CLIENT_PLATFORM_UNSPECIFIED` | 0 | — |
| `CLIENT_PLATFORM_ANDROID` | 1 | — |
| `CLIENT_PLATFORM_IOS` | 2 | — |
| `CLIENT_PLATFORM_HARMONY` | 3 | — |
| `CLIENT_PLATFORM_DESKTOP` | 4 | — |

### enum `RolloutMode`

> RolloutMode 灰度判定方式。多条规则按 priority 升序求第一命中； / 全不命中时回落到 ops_config_item 的正式版本（latest_version）。

| 值 | 编号 | 说明 |
|---|---|---|
| `ROLLOUT_MODE_UNSPECIFIED` | 0 | — |
| `ROLLOUT_MODE_FULL` | 1 | 全量 |
| `ROLLOUT_MODE_PERCENTAGE` | 2 | 按 mid 稳定分桶百分比（bucket = crc32(cfg_key + ":" + mid) % 100 < percentage） |
| `ROLLOUT_MODE_APP_VERSION` | 3 | 按 App 版本区间 |
| `ROLLOUT_MODE_PLATFORM` | 4 | 按端 |
| `ROLLOUT_MODE_MID_SUFFIX` | 5 | 按 mid 十进制尾号（"0,3,7" 命中任一尾号） |
| `ROLLOUT_MODE_WHITELIST` | 6 | 按 mid 白名单 |

### enum `ConfigValueType`

> ConfigValueType 配置值类型，写入前按类型校验，避免把脏值推给读取方。

| 值 | 编号 | 说明 |
|---|---|---|
| `CONFIG_VALUE_TYPE_UNSPECIFIED` | 0 | — |
| `CONFIG_VALUE_TYPE_STRING` | 1 | — |
| `CONFIG_VALUE_TYPE_INT` | 2 | — |
| `CONFIG_VALUE_TYPE_BOOL` | 3 | — |
| `CONFIG_VALUE_TYPE_JSON` | 4 | 不透明 JSON 文本；本服务不解析业务语义 |

### enum `ItemState`

> ItemState 通用启用/停用状态。

| 值 | 编号 | 说明 |
|---|---|---|
| `ITEM_STATE_UNSPECIFIED` | 0 | — |
| `ITEM_STATE_ON` | 1 | 启用/生效/上架 |
| `ITEM_STATE_OFF` | 2 | 停用/下线/下架 |

### message `TargetContext`

> TargetContext 运行时解析请求的调用侧上下文（灰度输入）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `platform` | [`ClientPlatform`](#enum-clientplatform) | 1 | — | 终端类型；UNSPECIFIED 表示不做端过滤 |
| `app_version` | `string` | 2 | — | 客户端版本号（点分十进制字符串，如 "7.2.10"） |
| `mid` | `int64` | 3 | — | 当前用户；0 表示未登录（不参与 mid 维度灰度） |
| `ignore_rollout` | `bool` | 4 | — | true 表示强制取正式版本（后台预览与排障用） |

### message `ConfigView`

> --- 1. 运行时读取（带灰度命中与缓存 TTL） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `config_id` | `int64` | 1 | — | — |
| `cfg_key` | `string` | 2 | — | 配置键（如 home.topic.enabled） |
| `scope` | `string` | 3 | — | global / android / ios / harmony / desktop |
| `value_type` | [`ConfigValueType`](#enum-configvaluetype) | 4 | — | — |
| `value` | `string` | 5 | — | 命中版本的值（文本承载） |
| `version` | `int64` | 6 | — | 命中版本号 |
| `rollout_rule_id` | `int64` | 7 | — | 命中的灰度规则，0 表示走正式版本 |
| `rollout_mode` | `string` | 8 | — | 命中方式标识（便于客户端与排障对齐，不驱动 UI） |
| `ttl` | `int32` | 9 | — | 建议缓存秒数（0 表示不建议缓存） |
| `epoch` | `int64` | 10 | — | 缓存代次：RefreshCache 后单调递增，客户端本地缓存可据此失效 |
| `published_by` | `string` | 11 | — | 发布该版本的操作人（冗余快照） |
| `published_at` | `int64` | 12 | — | 该版本发布时间（Unix 秒） |

### message `ResolveConfigReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `cfg_key` | `string` | 2 | — | 必填 |
| `scope` | `string` | 3 | — | 空表示 global |
| `target` | [`TargetContext`](#message-targetcontext) | 4 | — | — |
| `refresh` | `bool` | 5 | — | true 强制回源并回填缓存 |

### message `ResolveConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `config` | [`ConfigView`](#message-configview) | 1 | — | — |
| `found` | `bool` | 2 | — | false 表示键不存在或已停用（不返回 gRPC NotFound） |

### message `BatchResolveConfigReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `cfg_keys` | `string` | 2 | repeated | 单次最多 50 个键（网关聚合用） |
| `scope` | `string` | 3 | — | — |
| `target` | [`TargetContext`](#message-targetcontext) | 4 | — | — |

### message `BatchResolveConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `configs` | [`ConfigView`](#message-configview) | 1 | repeated | — |
| `missing_keys` | `string` | 2 | repeated | 未命中的键（含停用与不存在） |
| `ttl` | `int32` | 3 | — | 本批最小建议 TTL（网关按此缓存整批） |

### message `ConfigItem`

> --- 2. 配置项与发布/回滚 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `config_id` | `int64` | 1 | — | — |
| `cfg_key` | `string` | 2 | — | — |
| `scope` | `string` | 3 | — | — |
| `value_type` | [`ConfigValueType`](#enum-configvaluetype) | 4 | — | — |
| `title` | `string` | 5 | — | 中文名，后台展示 |
| `description` | `string` | 6 | — | — |
| `state` | `int32` | 7 | — | 1 启用、2 停用 |
| `latest_version` | `int64` | 8 | — | 当前正式版本，0 表示未发布 |
| `epoch` | `int64` | 9 | — | 缓存代次 |
| `operator_id` | `int64` | 10 | — | 最后操作人 |
| `ctime` | `int64` | 11 | — | — |
| `mtime` | `int64` | 12 | — | — |

### message `ListConfigsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `scope` | `string` | 2 | — | 空表示全部 |
| `keyword` | `string` | 3 | — | cfg_key/title 模糊 |
| `state` | `int32` | 4 | — | 0 全部 |
| `pn` | `int32` | 5 | — | 从 1 开始 |
| `ps` | `int32` | 6 | — | 上限 100 |

### message `ListConfigsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`ConfigItem`](#message-configitem) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `PublishConfigReq`

> PublishConfigReq 发布一个新版本。 / 语义：expect_version 是**当前生效版本号**（新建传 0）；成功后写入 / ops_config_version（不可变快照）、把 item.latest_version 推到新版本、epoch +1。 / 带 rollout 时新版本只做灰度生效，latest_version 保持不变，由 SetRolloutRuleState / 或再次全量发布收口。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | request_id 必填，作为幂等键与 audit event_id 组成 |
| `cfg_key` | `string` | 2 | — | — |
| `scope` | `string` | 3 | — | 空默认 global |
| `value_type` | [`ConfigValueType`](#enum-configvaluetype) | 4 | — | 新建必填；更新时 0 表示沿用 |
| `value` | `string` | 5 | — | 新值 |
| `expect_version` | `int64` | 6 | — | 乐观锁；0 表示新建 |
| `reason` | `string` | 7 | — | 变更原因（必填，透传给 audit） |
| `rollout` | [`RolloutRuleSpec`](#message-rolloutrulespec) | 8 | repeated | 可选：随本次发布同时挂灰度规则 |

### message `PublishConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `item` | [`ConfigItem`](#message-configitem) | 1 | — | — |
| `version` | [`ConfigVersion`](#message-configversion) | 2 | — | 新版本快照 |
| `rules` | [`RolloutRule`](#message-rolloutrule) | 3 | repeated | 本次一并写入的灰度规则 |
| `audit_entry_id` | `int64` | 4 | — | audit 条目引用，0 表示审计写入待补偿 |
| `reused` | `bool` | 5 | — | true 表示 request_id 命中已完成的发布（幂等回放） |

### message `RollbackConfigReq`

> RollbackConfigReq 回滚：不是删版本，而是把历史版本的值再发布成**新版本**， / 因此版本序列单调递增、历史不被改写（可审计）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `cfg_key` | `string` | 2 | — | — |
| `scope` | `string` | 3 | — | — |
| `to_version` | `int64` | 4 | — | 目标历史版本号 |
| `reason` | `string` | 5 | — | 回滚原因（必填） |

### message `RollbackConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `item` | [`ConfigItem`](#message-configitem) | 1 | — | — |
| `version` | [`ConfigVersion`](#message-configversion) | 2 | — | 新生成的回滚版本（change_type = rollback，rollback_from 指向 to_version） |
| `audit_entry_id` | `int64` | 3 | — | — |

### message `ConfigVersion`

> ConfigVersion 不可变版本快照。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `version_id` | `int64` | 1 | — | — |
| `config_id` | `int64` | 2 | — | — |
| `version` | `int64` | 3 | — | — |
| `value` | `string` | 4 | — | — |
| `value_type` | [`ConfigValueType`](#enum-configvaluetype) | 5 | — | — |
| `change_type` | `string` | 6 | — | create/publish/rollback |
| `rollback_from` | `int64` | 7 | — | 回滚来源版本，0 表示非回滚 |
| `operator_id` | `int64` | 8 | — | — |
| `operator_name` | `string` | 9 | — | 冗余快照 |
| `reason` | `string` | 10 | — | — |
| `audit_entry_id` | `int64` | 11 | — | audit.audit_entry 引用（谁写：本服务），0 表示待补偿 |
| `request_id` | `string` | 12 | — | 触发本次写入的幂等键 |
| `published_at` | `int64` | 13 | — | — |
| `ctime` | `int64` | 14 | — | — |

### message `ListConfigVersionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `cfg_key` | `string` | 2 | — | — |
| `scope` | `string` | 3 | — | — |
| `pn` | `int32` | 4 | — | — |
| `ps` | `int32` | 5 | — | — |

### message `ListConfigVersionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`ConfigVersion`](#message-configversion) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
| `latest_version` | `int64` | 3 | — | — |

### message `RolloutRule`

> --- 3. 灰度规则 --- / RolloutRule 灰度规则：作用于 (config_id, version)。多条规则按 priority 升序求首个命中。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule_id` | `int64` | 1 | — | — |
| `config_id` | `int64` | 2 | — | — |
| `version` | `int64` | 3 | — | 该规则放量的版本号 |
| `name` | `string` | 4 | — | 同 (config_id, version) 内唯一，作为 upsert 幂等句柄 |
| `mode` | [`RolloutMode`](#enum-rolloutmode) | 5 | — | — |
| `percentage` | `int32` | 6 | — | mode=PERCENTAGE 生效，0-100 |
| `app_version_min` | `string` | 7 | — | 闭区间下界（点分版本，服务端按段比较） |
| `app_version_max` | `string` | 8 | — | 闭区间上界，空表示不设上界 |
| `platforms` | [`ClientPlatform`](#enum-clientplatform) | 9 | repeated | 空表示不限端 |
| `mid_suffixes` | `string` | 10 | — | "0,3,7"，空表示不限 |
| `whitelist_mids` | `int64` | 11 | repeated | 白名单 mid |
| `priority` | `int32` | 12 | — | 小者先判定 |
| `state` | `int32` | 13 | — | 1 生效、2 停用 |
| `operator_id` | `int64` | 14 | — | — |
| `remark` | `string` | 15 | — | — |
| `start_at` | `int64` | 16 | — | 生效起始（Unix 秒），0 表示立即 |
| `end_at` | `int64` | 17 | — | 生效结束（Unix 秒），0 表示不设限 |
| `ctime` | `int64` | 18 | — | — |
| `mtime` | `int64` | 19 | — | — |

### message `RolloutRuleSpec`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `name` | `string` | 1 | — | 必填，幂等句柄 |
| `mode` | [`RolloutMode`](#enum-rolloutmode) | 2 | — | — |
| `percentage` | `int32` | 3 | — | — |
| `app_version_min` | `string` | 4 | — | — |
| `app_version_max` | `string` | 5 | — | — |
| `platforms` | [`ClientPlatform`](#enum-clientplatform) | 6 | repeated | — |
| `mid_suffixes` | `string` | 7 | — | — |
| `whitelist_mids` | `int64` | 8 | repeated | — |
| `priority` | `int32` | 9 | — | — |
| `remark` | `string` | 10 | — | — |
| `start_at` | `int64` | 11 | — | — |
| `end_at` | `int64` | 12 | — | — |

### message `SaveRolloutRuleReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | request_id 必填 |
| `cfg_key` | `string` | 2 | — | — |
| `scope` | `string` | 3 | — | — |
| `version` | `int64` | 4 | — | 目标版本号（必须是已发布的版本） |
| `rule` | [`RolloutRuleSpec`](#message-rolloutrulespec) | 5 | — | 按 (config_id, version, name) upsert |

### message `SaveRolloutRuleReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule` | [`RolloutRule`](#message-rolloutrule) | 1 | — | — |
| `audit_entry_id` | `int64` | 2 | — | — |

### message `ListRolloutRulesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `cfg_key` | `string` | 2 | — | 空表示全部（此时 scope 也忽略） |
| `scope` | `string` | 3 | — | — |
| `version` | `int64` | 4 | — | 0 表示全部版本 |
| `state` | `int32` | 5 | — | 0 全部 |
| `pn` | `int32` | 6 | — | — |
| `ps` | `int32` | 7 | — | — |

### message `ListRolloutRulesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`RolloutRule`](#message-rolloutrule) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `SetRolloutRuleStateReq`

> SetRolloutRuleStateReq 启停灰度规则（不物理删除，保留放量决策证据）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `rule_id` | `int64` | 2 | — | — |
| `state` | `int32` | 3 | — | 1 生效、2 停用 |
| `reason` | `string` | 4 | — | 必填（进 audit） |

### message `SetRolloutRuleStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rule` | [`RolloutRule`](#message-rolloutrule) | 1 | — | — |
| `audit_entry_id` | `int64` | 2 | — | — |

### message `Topic`

> --- 4. 专题 / 合集（引用分区与标签 ID，不复制主资料） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic_id` | `int64` | 1 | — | — |
| `slug` | `string` | 2 | — | 稳定标识（唯一，端上按此寻址） |
| `title` | `string` | 3 | — | — |
| `description` | `string` | 4 | — | — |
| `cover` | `string` | 5 | — | 封面 URL（对象存储地址由 playback/asset 侧签名，这里只存展示用地址或 object_key） |
| `zone_ids` | `int64` | 6 | repeated | 引用 catalog_zone.zoneid（本服务不存分区名） |
| `tag_ids` | `int64` | 7 | repeated | 引用 catalog_tag.tagid（本服务不存标签名） |
| `state` | `int32` | 8 | — | 1 上架、2 下架 |
| `sort` | `int32` | 9 | — | 列表排序，小者在前 |
| `start_at` | `int64` | 10 | — | 生效窗口起（Unix 秒） |
| `end_at` | `int64` | 11 | — | 生效窗口止，0 表示不限 |
| `version` | `int64` | 12 | — | 乐观锁 |
| `operator_id` | `int64` | 13 | — | — |
| `ctime` | `int64` | 14 | — | — |
| `mtime` | `int64` | 15 | — | — |

### message `SaveTopicReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `topic_id` | `int64` | 2 | — | 0 表示新建 |
| `slug` | `string` | 3 | — | 新建必填；更新时非空即改 slug |
| `title` | `string` | 4 | — | — |
| `description` | `string` | 5 | — | — |
| `cover` | `string` | 6 | — | — |
| `zone_ids` | `int64` | 7 | repeated | 全量覆盖 |
| `tag_ids` | `int64` | 8 | repeated | 全量覆盖 |
| `state` | `int32` | 9 | — | 0 视为 2（草稿语义：默认不上架） |
| `sort` | `int32` | 10 | — | — |
| `start_at` | `int64` | 11 | — | — |
| `end_at` | `int64` | 12 | — | — |
| `expect_version` | `int64` | 13 | — | 乐观锁：更新必填当前版本；0 表示新建 |
| `reason` | `string` | 14 | — | 变更原因（进 audit） |

### message `SaveTopicReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic` | [`Topic`](#message-topic) | 1 | — | — |
| `audit_entry_id` | `int64` | 2 | — | — |

### message `GetTopicReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `topic_id` | `int64` | 2 | — | 与 slug 二选一 |
| `slug` | `string` | 3 | — | — |
| `with_items` | `bool` | 4 | — | true 时返回条目 |
| `item_limit` | `int32` | 5 | — | with_items 时的条目上限，<=0 按服务端默认（100） |

### message `GetTopicReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic` | [`Topic`](#message-topic) | 1 | — | — |
| `items` | [`TopicItem`](#message-topicitem) | 2 | repeated | — |
| `found` | `bool` | 3 | — | — |
| `ttl` | `int32` | 4 | — | — |

### message `ListTopicsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `state` | `int32` | 2 | — | 0 全部 |
| `zone_id` | `int64` | 3 | — | 0 不过滤（按引用 ID 过滤，本服务不解析分区名） |
| `tag_id` | `int64` | 4 | — | 0 不过滤 |
| `keyword` | `string` | 5 | — | — |
| `pn` | `int32` | 6 | — | — |
| `ps` | `int32` | 7 | — | — |
| `online_only` | `bool` | 8 | — | true 时附加「当前时间在生效窗口内」 |

### message `ListTopicsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`Topic`](#message-topic) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `TopicItem`

> TopicItem 专题条目：只引用内容主键，不复制标题/时长等可变资料。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `topic_id` | `int64` | 2 | — | — |
| `item_type` | `string` | 3 | — | ugc_video / pgc_season / pgc_episode（值域固定，新增需评审） |
| `item_id` | `string` | 4 | — | 领域服务主键（aid / season_id / epid） |
| `position` | `int32` | 5 | — | 专题内排序，小者在前 |
| `state` | `int32` | 6 | — | 1 生效、2 移除 |
| `operator_id` | `int64` | 7 | — | — |
| `ctime` | `int64` | 8 | — | — |
| `mtime` | `int64` | 9 | — | — |

### message `SaveTopicItemsReq`

> SaveTopicItemsReq 全量覆盖某专题的条目集合（幂等：同一 request_id 重放结果一致）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `topic_id` | `int64` | 2 | — | — |
| `items` | [`TopicItem`](#message-topicitem) | 3 | repeated | 最多 500 条；position 必须从 1 连续 |
| `reason` | `string` | 4 | — | — |

### message `SaveTopicItemsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `topic_id` | `int64` | 1 | — | — |
| `total` | `int32` | 2 | — | — |
| `audit_entry_id` | `int64` | 3 | — | — |

### message `RecommendSlot`

> --- 5. 推荐位与坑位（内容分发，不含任何商业化投放字段） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `slot_id` | `int64` | 1 | — | — |
| `code` | `string` | 2 | — | 坑位编码（唯一，如 home.banner、detail.below_player） |
| `page` | `string` | 3 | — | 归属页面标识（服务端不解释布局） |
| `title` | `string` | 4 | — | — |
| `platforms` | [`ClientPlatform`](#enum-clientplatform) | 5 | repeated | 空表示不限端 |
| `capacity` | `int32` | 6 | — | 坑位数量上限 |
| `state` | `int32` | 7 | — | 1 启用、2 停用 |
| `version` | `int64` | 8 | — | 乐观锁 |
| `operator_id` | `int64` | 9 | — | — |
| `remark` | `string` | 10 | — | — |
| `ctime` | `int64` | 11 | — | — |
| `mtime` | `int64` | 12 | — | — |

### message `SlotItem`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `slot_id` | `int64` | 2 | — | — |
| `position` | `int32` | 3 | — | 1..capacity |
| `item_type` | `string` | 4 | — | ugc_video / pgc_season / pgc_episode / topic |
| `item_id` | `string` | 5 | — | 引用主键 |
| `weight` | `int32` | 6 | — | 同坑位并列时的次序（本服务不做推荐排序，见 README 缺口） |
| `start_at` | `int64` | 7 | — | 排期生效起（Unix 秒） |
| `end_at` | `int64` | 8 | — | 排期生效止，0 表示不限 |
| `state` | `int32` | 9 | — | 1 生效、2 停用 |
| `operator_id` | `int64` | 10 | — | — |
| `ctime` | `int64` | 11 | — | — |
| `mtime` | `int64` | 12 | — | — |

### message `SaveSlotReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `slot_id` | `int64` | 2 | — | 0 新建 |
| `code` | `string` | 3 | — | — |
| `page` | `string` | 4 | — | — |
| `title` | `string` | 5 | — | — |
| `platforms` | [`ClientPlatform`](#enum-clientplatform) | 6 | repeated | — |
| `capacity` | `int32` | 7 | — | — |
| `state` | `int32` | 8 | — | 0 视为 2 |
| `expect_version` | `int64` | 9 | — | — |
| `remark` | `string` | 10 | — | — |
| `reason` | `string` | 11 | — | — |

### message `SaveSlotReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `slot` | [`RecommendSlot`](#message-recommendslot) | 1 | — | — |
| `audit_entry_id` | `int64` | 2 | — | — |

### message `ListSlotsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `page` | `string` | 2 | — | 空表示全部 |
| `state` | `int32` | 3 | — | — |
| `platform` | [`ClientPlatform`](#enum-clientplatform) | 4 | — | UNSPECIFIED 不过滤 |
| `pn` | `int32` | 5 | — | — |
| `ps` | `int32` | 6 | — | — |

### message `ListSlotsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`RecommendSlot`](#message-recommendslot) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `SaveSlotItemsReq`

> SaveSlotItemsReq 全量覆盖一个坑位的条目集合（含排期）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `slot_id` | `int64` | 2 | — | — |
| `items` | [`SlotItem`](#message-slotitem) | 3 | repeated | 最多 200 条；position 不得重复且 <= capacity |
| `reason` | `string` | 4 | — | — |

### message `SaveSlotItemsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `slot_id` | `int64` | 1 | — | — |
| `total` | `int32` | 2 | — | — |
| `audit_entry_id` | `int64` | 3 | — | — |

### message `ResolveSlotReq`

> ResolveSlotReq 运行时坑位视图：按端 + 版本 + mid + 当前时间过滤， / 只回引用；条目标题/封面/状态由调用方（gateway/app）批量向 video/catalog 取。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `code` | `string` | 2 | — | 坑位编码 |
| `target` | [`TargetContext`](#message-targetcontext) | 3 | — | — |
| `at` | `int64` | 4 | — | 判定时间（Unix 秒），0 表示服务端当前时间 |
| `limit` | `int32` | 5 | — | 返回条数上限，<=0 按 capacity |

### message `ResolveSlotReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `slot` | [`RecommendSlot`](#message-recommendslot) | 1 | — | — |
| `items` | [`SlotItem`](#message-slotitem) | 2 | repeated | — |
| `found` | `bool` | 3 | — | — |
| `ttl` | `int32` | 4 | — | 建议网关缓存秒数 |

### message `ClientSwitch`

> --- 6. 客户端开关与版本门槛 --- / ClientSwitch 表达「某端从哪个版本起具备某能力」，不表达 UI 细节。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `switch_id` | `int64` | 1 | — | — |
| `switch_key` | `string` | 2 | — | 开关键（如 vertical_feed） |
| `platform` | [`ClientPlatform`](#enum-clientplatform) | 3 | — | 必填；不按端区分时用 cfg_key 走配置项 |
| `min_version` | `string` | 4 | — | 具备能力的最小版本，空表示不限 |
| `max_version` | `string` | 5 | — | 具备能力的最大版本，空表示不限 |
| `enabled` | `int32` | 6 | — | 1 开、2 关 |
| `config_id` | `int64` | 7 | — | 可选：关联的配置项（0 表示无关联） |
| `operator_id` | `int64` | 8 | — | — |
| `remark` | `string` | 9 | — | — |
| `version` | `int64` | 10 | — | 乐观锁 |
| `ctime` | `int64` | 11 | — | — |
| `mtime` | `int64` | 12 | — | — |

### message `SaveClientSwitchReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `switch_id` | `int64` | 2 | — | 0 新建（按 switch_key + platform upsert） |
| `switch_key` | `string` | 3 | — | — |
| `platform` | [`ClientPlatform`](#enum-clientplatform) | 4 | — | — |
| `min_version` | `string` | 5 | — | — |
| `max_version` | `string` | 6 | — | — |
| `enabled` | `int32` | 7 | — | 0 视为 2（默认关闭，避免误放量） |
| `config_id` | `int64` | 8 | — | — |
| `remark` | `string` | 9 | — | — |
| `expect_version` | `int64` | 10 | — | — |
| `reason` | `string` | 11 | — | — |

### message `SaveClientSwitchReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `switch` | [`ClientSwitch`](#message-clientswitch) | 1 | — | — |
| `audit_entry_id` | `int64` | 2 | — | — |

### message `ListClientSwitchesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `platform` | [`ClientPlatform`](#enum-clientplatform) | 2 | — | UNSPECIFIED 不过滤 |
| `switch_key` | `string` | 3 | — | 空不过滤 |
| `enabled` | `int32` | 4 | — | 0 不过滤 |
| `pn` | `int32` | 5 | — | — |
| `ps` | `int32` | 6 | — | — |

### message `ListClientSwitchesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`ClientSwitch`](#message-clientswitch) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `RefreshCacheReq`

> --- 7. 缓存刷新 --- / RefreshCacheReq 主动失效运行时缓存（多实例收敛）。 / 语义：按 scope/cfg_key 把 ops_config_item.epoch 递增并删除 Redis 读缓存； / target = all 时整域失效（用于故障兜底，必须写审计）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`CallContext`](#message-callcontext) | 1 | — | — |
| `target` | `string` | 2 | — | config / topic / slot / all |
| `cfg_key` | `string` | 3 | — | target=config 时定位单键，空表示该 scope 全部 |
| `scope` | `string` | 4 | — | 空表示 global |
| `topic_id` | `int64` | 5 | — | target=topic |
| `slot_id` | `int64` | 6 | — | target=slot |
| `reason` | `string` | 7 | — | 必填 |

### message `RefreshCacheReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `affected` | `int32` | 1 | — | 失效的缓存键数量 |
| `epoch` | `int64` | 2 | — | 刷新后的代次（target=all 时为 0） |
| `audit_entry_id` | `int64` | 3 | — | — |
