# RPC · `search-indexer`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/search-indexer/rpc/searchindexer.proto` |
| protobuf 包 | `searchindexer.v1` |
| go_package | `go-video/services/search-indexer/rpc` |
| 发现用的 etcd key | `searchindexer.v1.rpc`（`services/search-indexer/etc/searchindexer.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`searchindexer.v1.rpc`） |
| 监听 | `8106`（`services/search-indexer/etc/searchindexer.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_search_indexer` |
| 方法数 | 7（service `SearchIndexer`） |
| 网关消费方 | `admin:SearchIndexerRPC` |

## 契约说明

> 说明：本契约定义 search-indexer（搜索索引构建）服务的对外 RPC。
>
> 数据所有权（AGENTS.md §5）：本服务只拥有「索引投影」相关表
> （search_index_task / search_index_version / search_consumer_offset /
>  search_dead_letter），不拥有稿件、媒资、目录、版权等主数据。
> 因此 ContentDoc 的字段只能来自：
>   1) 上游领域事件 payload（content.published.v1、engagement.action.v1）；
>   2) 上游服务通过本 RPC 显式推送的事实快照（回填/重建）。
> 禁止本服务直连 video/catalog/user-profile 等服务的 MySQL 表或 Redis 业务 key。
>
> 索引是投影而非事实源：所有写操作按 doc_revision（毫秒级事实版本）做
> last-write-wins，旧版本永远不能覆盖新版本；重建走「新索引 + 别名切换」，
> 查询别名（search-query 读取）在切换前后始终可用，实现零停机重建。

## service `SearchIndexer`

> SearchIndexer 搜索索引构建服务。 / 调用方：services/operation（管理后台重建/切换）、services/cron（回填）、 / 以及本服务 internal/consumer（事件消费走同一 Repository 写入路径）。

gRPC 方法前缀：`searchindexer.v1.SearchIndexer/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `UpsertContentDoc` | [`UpsertContentDocReq`](#message-upsertcontentdocreq) | [`UpsertContentDocReply`](#message-upsertcontentdocreply) | 按 content_id 写入/覆盖一条内容投影（上游显式触发或回填）。 / doc 必须携带 doc_revision；旧版本默认被拒绝，保证乱序/迟到事件不破坏终态。 |
| 2 | `DeleteContentDoc` | [`DeleteContentDocReq`](#message-deletecontentdocreq) | [`DeleteContentDocReply`](#message-deletecontentdocreply) | 下架/删除时移除或降级投影，并说明 CDN/搜索投影一致性处理。 |
| 3 | `SubmitRebuildTask` | [`SubmitRebuildTaskReq`](#message-submitrebuildtaskreq) | [`SubmitRebuildTaskReply`](#message-submitrebuildtaskreply) | 提交全量/分区重建任务（request_id 幂等），返回 task_id 与预分配目标索引。 |
| 4 | `GetRebuildTask` | [`GetRebuildTaskReq`](#message-getrebuildtaskreq) | [`RebuildTask`](#message-rebuildtask) | 查询单个重建任务进度。 |
| 5 | `ListRebuildTasks` | [`ListRebuildTasksReq`](#message-listrebuildtasksreq) | [`ListRebuildTasksReply`](#message-listrebuildtasksreply) | 分页查询重建任务（cursor + 状态过滤）。 |
| 6 | `SwitchAlias` | [`SwitchAliasReq`](#message-switchaliasreq) | [`SwitchAliasReply`](#message-switchaliasreply) | 把查询别名从旧索引切到新版本索引（expected_current 乐观校验，失败返回明确错误）。 |
| 7 | `GetIndexHealth` | [`GetIndexHealthReq`](#message-getindexhealthreq) | [`GetIndexHealthReply`](#message-getindexhealthreply) | 返回各别名/索引的 doc 数与健康状态，以及重试/死信积压。 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `ContentType`

> 内容类型（索引按内容类型区分字段来源与所有者）

| 值 | 编号 | 说明 |
|---|---|---|
| `CONTENT_TYPE_UNSPECIFIED` | 0 | 未指定（拒绝写入） |
| `CONTENT_TYPE_UGC_VIDEO` | 1 | UGC/PUGC 稿件，所有者 video |
| `CONTENT_TYPE_PGC_EPISODE` | 2 | 版权内容剧集，所有者 catalog/rights |
| `CONTENT_TYPE_LIVE_ROOM` | 3 | 直播间，所有者 live-room |

### enum `ContentState`

> 内容在索引中的可见状态（与 AGENTS.md §8 投稿状态机的可检索子集对齐）

| 值 | 编号 | 说明 |
|---|---|---|
| `CONTENT_STATE_UNSPECIFIED` | 0 | 未指定（拒绝写入） |
| `CONTENT_STATE_PENDING` | 1 | 已入库但未过审：仅运营侧可见，不参与公共检索 |
| `CONTENT_STATE_PUBLISHED` | 2 | 已发布：可检索 |
| `CONTENT_STATE_OFFLINE` | 3 | 下架：保留投影但不检索命中 |
| `CONTENT_STATE_EXPIRED` | 4 | 版权窗口过期：不检索命中 |
| `CONTENT_STATE_DELETED` | 5 | 删除：必须移除投影（见 DeleteContentDoc） |

### message `HeatSnapshot`

> HeatSnapshot 社区互动热度快照。 / 注意：点赞/收藏/分享属于社区能力，不包含投币、订单、广告等商业化字段（AGENTS.md §1）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `view_count` | `int64` | 1 | — | 播放量快照（来自 SPM 事件，不由本服务计算） |
| `like_count` | `int64` | 2 | — | 点赞数快照（engagement 事件） |
| `favorite_count` | `int64` | 3 | — | 收藏数快照 |
| `share_count` | `int64` | 4 | — | 分享数快照 |
| `comment_count` | `int64` | 5 | — | 评论数快照 |
| `danmaku_count` | `int64` | 6 | — | 弹幕数快照 |
| `heat_score` | `int32` | 7 | — | 归一化热度分值 [0,1000]，由上游/SPM 计算后推送 |
| `heat_revision` | `int64` | 8 | — | 热度快照版本（Unix 毫秒），用于热度字段 last-write-wins |

### message `ContentDoc`

> ContentDoc 一条内容在搜索索引中的完整投影。 / 字段来源约束：只允许事件 payload 或上游 RPC 推送；本服务不自行补齐事实字段。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `content_id` | `int64` | 1 | — | 内容主键（aid / episode_id / room_id） |
| `content_type` | [`ContentType`](#enum-contenttype) | 2 | — | 内容类型 |
| `title` | `string` | 3 | — | 标题 |
| `description` | `string` | 4 | — | 简介（已在上游做敏感词/链接清洗） |
| `cover_url` | `string` | 5 | — | 封面 URL（只存 CDN 地址，不存对象存储密钥） |
| `author_mid` | `int64` | 6 | — | 作者用户 ID（受控 ID，不含手机号/身份证等敏感信息） |
| `author_name` | `string` | 7 | — | 作者昵称快照（事实源仍是 user-profile） |
| `typeid` | `int32` | 8 | — | 分区 ID |
| `type_name` | `string` | 9 | — | 分区名快照 |
| `tags` | `string` | 10 | repeated | 标签 |
| `duration_sec` | `int64` | 11 | — | 时长（秒） |
| `publish_at` | `int64` | 12 | — | 发布时间（Unix 秒，定时发布时为计划时间） |
| `ctime` | `int64` | 13 | — | 内容创建时间（Unix 秒） |
| `state` | [`ContentState`](#enum-contentstate) | 14 | — | 可见状态 |
| `doc_revision` | `int64` | 15 | — | 事实版本（Unix 毫秒，来自 mtime 或版本号），必填且单调递增 |
| `heat` | [`HeatSnapshot`](#message-heatsnapshot) | 16 | — | 热度快照 |
| `rights_expire_at` | `int64` | 17 | — | 版权窗口结束时间（PGC），0 表示不适用 |
| `language` | `string` | 18 | — | 主语言（如 zh-CN） |
| `subtitle_langs` | `string` | 19 | repeated | 字幕语言列表 |
| `sensitive` | `bool` | 20 | — | 审核敏感标记（查询侧降权，不删除） |
| `schema_version` | `int32` | 21 | — | 文档结构版本，默认 1 |

### message `UpsertContentDocReq`

> --- 写入单篇投影 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `content_id` | `int64` | 1 | — | 内容主键（与 doc.content_id 必须一致） |
| `doc` | [`ContentDoc`](#message-contentdoc) | 2 | — | 上游推送的最新事实快照；为空时返回明确错误，不做「猜测式」重建 |
| `request_id` | `string` | 3 | — | 幂等键（可选，仅用于日志与审计串联） |
| `source` | `string` | 4 | — | 触发来源标记：rpc / event / backfill / rebuild |
| `force_overwrite` | `bool` | 5 | — | 仅运维回填使用：跳过 revision 防旧覆盖新校验，默认 false |

### message `UpsertContentDocReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `content_id` | `int64` | 1 | — | 内容主键 |
| `index` | `string` | 2 | — | 实际写入的物理索引名 |
| `outcome` | `string` | 3 | — | written / skipped_stale / created |
| `doc_revision` | `int64` | 4 | — | 写入后索引中生效的版本 |
| `took_ms` | `int64` | 5 | — | 本次写入耗时（毫秒） |

### message `DeleteContentDocReq`

> --- 删除投影 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `content_id` | `int64` | 1 | — | 内容主键 |
| `content_type` | [`ContentType`](#enum-contenttype) | 2 | — | 内容类型（用于分索引/校验） |
| `reason` | `string` | 3 | — | 删除原因：offline / expired / deleted / copyright_takedown |
| `purge` | `bool` | 4 | — | true 物理删除文档；false 仅把 state 置为不可检索 |

### message `DeleteContentDocReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `deleted` | `bool` | 1 | — | 是否真实删除了文档（false 表示仅降级为不可检索或未命中） |
| `index` | `string` | 2 | — | 操作的物理索引名 |
| `outcome` | `string` | 3 | — | deleted / marked / not_found |

### message `SubmitRebuildTaskReq`

> --- 重建任务 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scope` | `string` | 1 | — | 重建范围：full / partition / content_type |
| `scope_value` | `string` | 2 | — | partition 时为 typeid 区间 "1000-1999"；content_type 时为类型枚举值 1/2/3 |
| `alias` | `string` | 3 | — | 查询别名，空表示默认别名（OpenSearch.IndexPrefix） |
| `request_id` | `string` | 4 | — | 幂等键（必填）：同 request_id 重复提交返回同一 task_id |
| `operator` | `string` | 5 | — | 提交人（管理员账号或 cron job 名） |

### message `SubmitRebuildTaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `string` | 1 | — | 任务 ID（ULID） |
| `target_index` | `string` | 2 | — | 预分配的新物理索引名 |
| `state` | `string` | 3 | — | pending / running |
| `duplicated` | `bool` | 4 | — | true 表示命中 request_id 幂等，返回的是既有任务 |

### message `RebuildTask`

> RebuildTask 重建任务投影（对应 search_index_task 表）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `string` | 1 | — | 任务 ID |
| `scope` | `string` | 2 | — | full / partition / content_type |
| `scope_value` | `string` | 3 | — | 范围取值 |
| `state` | `string` | 4 | — | pending / running / succeeded / failed / canceled |
| `cursor_value` | `string` | 5 | — | 续跑游标（content_id 区间下界） |
| `total` | `int64` | 6 | — | 预计总量 |
| `processed` | `int64` | 7 | — | 已处理 |
| `failed` | `int64` | 8 | — | 失败数 |
| `target_index` | `string` | 9 | — | 目标索引 |
| `alias` | `string` | 10 | — | 目标别名 |
| `operator` | `string` | 11 | — | 提交人 |
| `request_id` | `string` | 12 | — | 幂等键 |
| `ctime` | `int64` | 13 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 14 | — | 修改时间（Unix 秒） |
| `started_at` | `int64` | 15 | — | 开始执行时间（Unix 秒） |
| `finished_at` | `int64` | 16 | — | 结束时间（Unix 秒） |
| `last_error` | `string` | 17 | — | 最近一次失败原因（脱敏） |
| `dlq_count` | `int64` | 18 | — | 观测值：当前待处理死信总数（重建期间事件消费失败的积压量） |

### message `GetRebuildTaskReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `string` | 1 | — | 任务 ID |

### message `ListRebuildTasksReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | `string` | 1 | — | 按状态过滤，空表示全部 |
| `cursor` | `string` | 2 | — | 上一页返回的 next_cursor，空表示从头开始 |
| `limit` | `int32` | 3 | — | 页大小，默认 20，最大 100（cursor 优先，避免深分页） |

### message `ListRebuildTasksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tasks` | [`RebuildTask`](#message-rebuildtask) | 1 | repeated | 任务列表（按 ctime 倒序） |
| `next_cursor` | `string` | 2 | — | 下一页游标，空表示结束 |

### message `SwitchAliasReq`

> --- 别名切换（零停机重建的关键步骤） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `alias` | `string` | 1 | — | 查询别名，空表示默认别名（OpenSearch.IndexPrefix） |
| `target_index` | `string` | 2 | — | 新索引（必须先完成重建并校验），必须以 <alias>_ 开头 |
| `expected_current` | `string` | 3 | — | 乐观校验：调用方认为当前指向的索引，必须匹配；空表示首次挂载别名 |
| `skip_health_check` | `bool` | 4 | — | 跳过目标索引 doc 数健康校验（仅紧急回滚使用） |
| `operator` | `string` | 5 | — | 操作人 |

### message `SwitchAliasReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `alias` | `string` | 1 | — | 别名 |
| `previous_index` | `string` | 2 | — | 切换前指向 |
| `current_index` | `string` | 3 | — | 切换后指向 |
| `doc_count` | `int64` | 4 | — | 新索引 doc 数 |
| `record_state` | `string` | 5 | — | 版本记录状态：active / history |

### message `GetIndexHealthReq`

> --- 索引健康 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `alias` | `string` | 1 | — | 指定别名；空表示返回全部已登记别名 |

### message `AliasStatus`

> AliasStatus 单个别名的健康投影。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `alias` | `string` | 1 | — | 别名 |
| `active_index` | `string` | 2 | — | 当前指向的物理索引 |
| `schema_version` | `string` | 3 | — | 索引结构版本（如 v1） |
| `doc_count` | `int64` | 4 | — | doc 数（读不到为 -1） |
| `index_exists` | `bool` | 5 | — | 物理索引是否存在 |
| `health` | `string` | 6 | — | green / yellow / red / missing |
| `state` | `string` | 7 | — | 版本记录状态：active / retiring / history |

### message `GetIndexHealthReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aliases` | [`AliasStatus`](#message-aliasstatus) | 1 | repeated | 各别名状态 |
| `retry_pending` | `int64` | 2 | — | 待重试事件数（search_consumer_offset state=retry） |
| `dead_letter` | `int64` | 3 | — | 死信数量（search_dead_letter） |
| `overall_state` | `string` | 4 | — | ok / degraded / down |
