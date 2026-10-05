# RPC · `search-query`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/search-query/rpc/searchquery.proto` |
| protobuf 包 | `searchquery.v1` |
| go_package | `go-video/services/search-query/rpc` |
| 发现用的 etcd key | `search-query.v1.rpc`（`services/search-query/etc/searchquery.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`search-query.v1.rpc`） |
| 监听 | `8107`（`services/search-query/etc/searchquery.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_search_query` |
| 方法数 | 8（service `SearchQuery`） |
| 网关消费方 | `app:SearchQueryRPC` |

## 契约说明

> 说明：本服务是搜索链路的“查询侧”，索引文档由 search-indexer 构建（AGENTS.md §5：
> 索引是投影，不是事实源）。本服务只拥有查询衍生数据：搜索历史、查询日志摘要、
> 热词快照、屏蔽词与事件 Outbox；不写 video/catalog/user-profile 的任何主数据。
>
> PGC（番剧/影视）结果同样来自 search-indexer 写入的索引投影（doc_type=pgc，
> 文档由 catalog/rights 的 content.published.v1 事件驱动），本服务不直接查
> catalog 库，也不编造版权内容。
>
> 分页策略（docs/api-and-events.md §2）：cursor 优先，pn/ps 仅为兼容保留；
> ps 受 PsLimit 限制，from+size 超过 MaxOffset/MaxResultWindow 直接拒绝，
> 避免深分页拖垮引擎。
>
> 降级语义（README 同步说明）：OpenSearch 未配置或熔断打开时返回明确错误码
> （ErrSearchUnavailable），绝不返回伪造的空成功结果。

## service `SearchQuery`

> SearchQuery 面向客户端的搜索查询服务。

gRPC 方法前缀：`searchquery.v1.SearchQuery/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `Search` | [`SearchReq`](#message-searchreq) | [`SearchReply`](#message-searchreply) | Search 关键词搜索（cursor 优先分页；引擎不可用时返回明确错误码）。 |
| 2 | `Suggest` | [`SuggestReq`](#message-suggestreq) | [`SuggestReply`](#message-suggestreply) | Suggest 输入前缀联想（Redis ZSET 词典 + 冷启动回源索引前缀查询）。 |
| 3 | `HotKeywords` | [`HotKeywordsReq`](#message-hotkeywordsreq) | [`HotKeywordsReply`](#message-hotkeywordsreply) | HotKeywords 全站/分区热词（读 DB 快照表 + Redis 缓存）。 |
| 4 | `GetSearchConfig` | [`GetSearchConfigReq`](#message-getsearchconfigreq) | [`GetSearchConfigReply`](#message-getsearchconfigreply) | GetSearchConfig 该端/分区的排序与分页能力配置（无 UI 硬编码）。 |
| 5 | `ListSearchHistory` | [`ListSearchHistoryReq`](#message-listsearchhistoryreq) | [`ListSearchHistoryReply`](#message-listsearchhistoryreply) | ListSearchHistory 用户搜索历史（cursor 倒序分页）。 |
| 6 | `DeleteSearchHistory` | [`DeleteSearchHistoryReq`](#message-deletesearchhistoryreq) | [`DeleteSearchHistoryReply`](#message-deletesearchhistoryreply) | DeleteSearchHistory 删除单个历史词（需 confirm=true，物理删除）。 |
| 7 | `ClearSearchHistory` | [`ClearSearchHistoryReq`](#message-clearsearchhistoryreq) | [`ClearSearchHistoryReply`](#message-clearsearchhistoryreply) | ClearSearchHistory 清空用户全部历史（需 confirm=true，物理删除）。 |
| 8 | `ReportQuery` | [`ReportQueryReq`](#message-reportqueryreq) | [`ReportQueryReply`](#message-reportqueryreply) | ReportQuery 上报查询行为（写 query_log 与 search_outbox，同事务）。 |

## 消息与枚举

### enum `SearchType`

> 搜索对象类型。ALL 表示在同一查询别名内跨 doc_type 召回。

| 值 | 编号 | 说明 |
|---|---|---|
| `SEARCH_TYPE_UNSPECIFIED` | 0 | 未指定，等价于 ALL |
| `SEARCH_TYPE_ALL` | 1 | 全部（视频 + 用户 + PGC） |
| `SEARCH_TYPE_VIDEO` | 2 | UGC/PUGC 视频稿件 |
| `SEARCH_TYPE_USER` | 3 | 用户（up 主） |
| `SEARCH_TYPE_PGC` | 4 | 番剧/影视等版权内容目录投影 |

### enum `SortMode`

> 排序方式。UNSPECIFIED 表示使用 GetSearchConfig 返回的该端默认排序。

| 值 | 编号 | 说明 |
|---|---|---|
| `SORT_UNSPECIFIED` | 0 | 由服务端配置决定 |
| `SORT_COMPREHENSIVE` | 1 | 综合（引擎相关性 + 热度因子） |
| `SORT_LATEST` | 2 | 最新发布（pub_time 倒序） |
| `SORT_MOST_VIEW` | 3 | 播放量倒序 |
| `SORT_MOST_FANS` | 4 | 粉丝数倒序（仅 SEARCH_TYPE_USER 有效） |
| `SORT_HOT_SCORE` | 5 | 热度分（doc 内 hot_score 字段，SPM 侧写入） |

### enum `DurationBucket`

> 时长区间筛选（秒）。与索引 doc 的 duration 字段对应。

| 值 | 编号 | 说明 |
|---|---|---|
| `DURATION_UNSPECIFIED` | 0 | 不筛选 |
| `DURATION_LT_1MIN` | 1 | < 60s |
| `DURATION_1_10MIN` | 2 | [60s, 600s) |
| `DURATION_10_30MIN` | 3 | [600s, 1800s) |
| `DURATION_30_60MIN` | 4 | [1800s, 3600s) |
| `DURATION_GT_60MIN` | 5 | >= 3600s |

### enum `QueryResultState`

> 查询日志的结果状态（与 search_query_log.result_state 一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `RESULT_STATE_UNSPECIFIED` | 0 | — |
| `RESULT_STATE_OK` | 1 | ok：引擎返回且有命中 |
| `RESULT_STATE_EMPTY` | 2 | empty：引擎返回但零命中 |
| `RESULT_STATE_DEGRADED` | 3 | degraded：引擎不可用/熔断/超时，未返回结果 |
| `RESULT_STATE_BLOCKED` | 4 | blocked：命中屏蔽词，未查询引擎 |

### message `SearchHit`

> 命中结果（索引文档投影，字段由 search-indexer 的 mapping 约定，见 README）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `doc_type` | `string` | 1 | — | video / user / pgc |
| `doc_id` | `int64` | 2 | — | aid / mid / pgc work id（各自主键，本服务不解释语义） |
| `title` | `string` | 3 | — | 标题（可能已被 highlight 标签包裹） |
| `content_snippet` | `string` | 4 | — | 摘要/简介片段（可能含 highlight 标签） |
| `author_mid` | `int64` | 5 | — | UP 主 mid（video/pgc 投影字段；用户结果为自身 mid） |
| `author_name` | `string` | 6 | — | UP 主昵称快照（展示用，主资料仍归 user-profile） |
| `zone_id` | `int32` | 7 | — | 分区 ID（PGC 为 catalog 分区投影） |
| `cover_url` | `string` | 8 | — | 封面引用（索引快照；播放签名由 playback 负责） |
| `view_count` | `int64` | 9 | — | 播放数快照（非计数事实源） |
| `like_count` | `int64` | 10 | — | 点赞数快照（计数事实源为 engagement） |
| `danmaku_count` | `int64` | 11 | — | 弹幕数快照（计数事实源为 danmaku） |
| `fans_count` | `int64` | 12 | — | 粉丝数快照（仅 user；事实源为 social-graph） |
| `duration_sec` | `int32` | 13 | — | 时长（video/pgc） |
| `pub_time` | `int64` | 14 | — | 发布/上线时间（Unix 秒） |
| `score` | `double` | 15 | — | 引擎打分 |
| `highlights` | `map<string, string>` | 16 | — | 其它高亮片段（字段名 -> 文本） |
| `state` | `string` | 17 | — | 文档状态快照（例如 published/offline），仅用于排查 |

### message `SearchReq`

> --- 搜索 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `keyword` | `string` | 1 | — | 关键词（必填；长度受 Search.KeywordMaxLen 限制） |
| `search_type` | [`SearchType`](#enum-searchtype) | 2 | — | 搜索类型，UNSPECIFIED 视为 ALL |
| `zone_id` | `int32` | 3 | — | 分区筛选（0 表示不筛选） |
| `duration` | [`DurationBucket`](#enum-durationbucket) | 4 | — | 时长筛选 |
| `sort` | [`SortMode`](#enum-sortmode) | 5 | — | 排序，UNSPECIFIED 使用配置默认值 |
| `pn` | `int32` | 6 | — | 页码（从 1 开始）；传了 cursor 时忽略 |
| `ps` | `int32` | 7 | — | 每页大小，超过 PsLimit 会被截断 |
| `cursor` | `string` | 8 | — | 游标（优先于 pn/ps，见 §2 cursor 优先分页） |
| `viewer_mid` | `int64` | 9 | — | 查看者 mid（0 表示游客；用于风控与历史写入） |
| `platform` | `string` | 10 | — | android/ios/harmony/desktop（不写死 UI 行为） |
| `app_version` | `string` | 11 | — | 客户端版本，用于能力区分 |
| `request_id` | `string` | 12 | — | 幂等/追踪用请求 ID（客户端生成） |
| `trace_id` | `string` | 13 | — | 链路 trace_id |
| `published_after` | `int64` | 14 | — | 仅召回发布时间不早于该值（Unix 秒，0 不筛选） |
| `published_before` | `int64` | 15 | — | 仅召回发布时间不晚于该值（Unix 秒，0 不筛选） |

### message `SearchReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `hits` | [`SearchHit`](#message-searchhit) | 1 | repeated | 命中列表；引擎失败时不会返回本消息（走错误码） |
| `total` | `int64` | 2 | — | 命中总数（引擎近似值，受 track_total_hits 限制） |
| `pn` | `int32` | 3 | — | 当前页码（cursor 请求下由游标反解） |
| `ps` | `int32` | 4 | — | 当前页大小 |
| `next_cursor` | `string` | 5 | — | 下一页游标（空表示无更多或已达 offset 上限） |
| `has_more` | `bool` | 6 | — | 是否还有下一页 |
| `safe_filtered` | `bool` | 7 | — | 是否触发安全过滤（屏蔽词/风控裁剪命中） |
| `cache_hit` | `bool` | 8 | — | 是否命中结果短缓存（便于网关设置 ttl） |
| `ttl` | `int32` | 9 | — | 建议客户端缓存秒数（0 表示不建议缓存） |

### message `SuggestReq`

> --- 联想 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `keyword` | `string` | 1 | — | 前缀（必填） |
| `limit` | `int32` | 2 | — | 返回条数，受配置上限限制 |
| `viewer_mid` | `int64` | 3 | — | 查看者 mid（0 游客） |
| `platform` | `string` | 4 | — | 端标识 |
| `app_version` | `string` | 5 | — | 客户端版本 |
| `search_type` | [`SearchType`](#enum-searchtype) | 6 | — | 期望结果类型（当前仅用于候选池选择） |
| `trace_id` | `string` | 7 | — | — |

### message `SuggestItem`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `keyword` | `string` | 1 | — | 候选词 |
| `weight` | `double` | 2 | — | 权重（用于客户端排序展示，不含 UI 行为） |
| `source` | `string` | 3 | — | 候选来源：redis / opensearch |
| `from_history` | `bool` | 4 | — | 是否来自该用户历史（登录用户优先合并） |

### message `SuggestReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`SuggestItem`](#message-suggestitem) | 1 | repeated | 去重后的候选（保持权重降序） |
| `ttl` | `int32` | 2 | — | 建议缓存秒数 |

### message `HotKeywordsReq`

> --- 热词 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scope` | `string` | 1 | — | 作用域：global 或 zone:<zone_id>；空视为 global |
| `limit` | `int32` | 2 | — | 返回条数 |
| `viewer_mid` | `int64` | 3 | — | 查看者 mid（保留字段，当前不做个性化） |
| `platform` | `string` | 4 | — | 端标识 |
| `trace_id` | `string` | 5 | — | — |

### message `HotKeyword`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `keyword` | `string` | 1 | — | 热词 |
| `score` | `double` | 2 | — | 热度分（快照值） |
| `snapshot_at` | `int64` | 3 | — | 快照生成时间（Unix 秒） |
| `scope` | `string` | 4 | — | 所属作用域 |

### message `HotKeywordsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `keywords` | [`HotKeyword`](#message-hotkeyword) | 1 | repeated | 热词列表；快照为空时返回空列表并说明 freshness |
| `snapshot_at` | `int64` | 2 | — | 本批快照时间（0 表示尚无快照） |
| `from_cache` | `bool` | 3 | — | 是否命中 Redis 缓存 |
| `ttl` | `int32` | 4 | — | 建议缓存秒数 |

### message `GetSearchConfigReq`

> --- 查询配置 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `platform` | `string` | 1 | — | 端标识 |
| `app_version` | `string` | 2 | — | 客户端版本 |
| `search_type` | [`SearchType`](#enum-searchtype) | 3 | — | 目标类型（不同默认值） |
| `zone_id` | `int32` | 4 | — | 分区（分区可覆盖默认排序） |
| `trace_id` | `string` | 5 | — | — |

### message `GetSearchConfigReply`

> 搜索展示能力（只有参数与枚举，不包含任何 UI 文案/控件行为）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `default_sort` | [`SortMode`](#enum-sortmode) | 1 | — | 该端/分区默认排序 |
| `supported_sorts` | [`SortMode`](#enum-sortmode) | 2 | repeated | 允许的排序枚举 |
| `supported_types` | [`SearchType`](#enum-searchtype) | 3 | repeated | 允许的搜索类型枚举 |
| `supported_durations` | [`DurationBucket`](#enum-durationbucket) | 4 | repeated | 允许的时长筛选 |
| `ps_default` | `int32` | 5 | — | 默认页大小 |
| `ps_limit` | `int32` | 6 | — | 页大小上限 |
| `max_offset` | `int32` | 7 | — | 允许的最大起始 offset（深分页保护） |
| `keyword_max_len` | `int32` | 8 | — | 关键词最大字符数 |
| `cache_ttl_seconds` | `int32` | 9 | — | 结果缓存建议秒数 |
| `engine_available` | `bool` | 10 | — | 引擎是否已配置（false 时 Search 必然降级） |

### message `SearchHistoryItem`

> --- 搜索历史（隐私可控） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `keyword` | `string` | 1 | — | 关键词 |
| `ctime` | `int64` | 2 | — | 首次搜索时间（Unix 秒） |
| `mtime` | `int64` | 3 | — | 最近一次搜索时间（列表按此倒序） |
| `state` | `int32` | 4 | — | 0 正常、1 运营标记、2 待清理（非 0 不返回给客户端） |
| `platform` | `string` | 5 | — | 最近一次来源端 |

### message `ListSearchHistoryReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID（必填） |
| `cursor` | `string` | 2 | — | 游标（首页传空；编码格式见 README，按 mtime 倒序 keyset 分页） |
| `limit` | `int32` | 3 | — | 条数，受配置上限限制 |
| `platform` | `string` | 4 | — | — |
| `trace_id` | `string` | 5 | — | — |

### message `ListSearchHistoryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`SearchHistoryItem`](#message-searchhistoryitem) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | 空表示无更多 |
| `has_more` | `bool` | 3 | — | — |

### message `DeleteSearchHistoryReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID（必填） |
| `keyword` | `string` | 2 | — | 待删除关键词（必填，按 keyword_hash 精确定位） |
| `confirm` | `bool` | 3 | — | 必须为 true：二次确认语义，防止误调用（AGENTS.md 隐私要求） |
| `request_id` | `string` | 4 | — | — |
| `trace_id` | `string` | 5 | — | — |

### message `DeleteSearchHistoryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `deleted` | `int64` | 1 | — | 实际删除行数（0 表示无匹配，幂等） |
| `hard_deleted` | `bool` | 2 | — | 是否已物理删除（本服务为 true：隐私数据不做软删保留） |

### message `ClearSearchHistoryReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID（必填） |
| `confirm` | `bool` | 2 | — | 必须为 true：二次确认语义 |
| `request_id` | `string` | 3 | — | — |
| `trace_id` | `string` | 4 | — | — |

### message `ClearSearchHistoryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `deleted` | `int64` | 1 | — | 删除行数 |
| `hard_deleted` | `bool` | 2 | — | 是否物理删除 |

### message `ReportQueryReq`

> --- 查询行为上报（SPM 链路，AGENTS.md §7：只服务分析与推荐，不含广告） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `query_id` | `string` | 1 | — | 幂等键（必填，唯一索引；重复上报返回 deduplicated=true） |
| `keyword` | `string` | 2 | — | 关键词（入库前按隐私策略做规范化，不存原始 IP/设备号） |
| `search_type` | [`SearchType`](#enum-searchtype) | 3 | — | — |
| `mid` | `int64` | 4 | — | 0 表示游客 |
| `hit_count` | `int64` | 5 | — | 命中数（引擎返回；降级时为 0） |
| `result_state` | [`QueryResultState`](#enum-queryresultstate) | 6 | — | — |
| `latency_ms` | `int64` | 7 | — | 引擎耗时 |
| `platform` | `string` | 8 | — | — |
| `app_version` | `string` | 9 | — | — |
| `ip_hash` | `string` | 10 | — | 受控标识（哈希后，不存原始 IP） |
| `trace_id` | `string` | 11 | — | — |
| `device_id_hash` | `string` | 12 | — | 受控设备标识（哈希） |

### message `ReportQueryReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `accepted` | `bool` | 1 | — | 是否已落库（query_log + outbox 同事务） |
| `deduplicated` | `bool` | 2 | — | query_id 是否已存在（重复上报） |
| `event_id` | `string` | 3 | — | 事件信封 event_id（重复时为已存在的 ID，可能为空） |
