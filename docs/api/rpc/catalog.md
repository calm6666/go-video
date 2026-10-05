# RPC · `catalog`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/catalog/rpc/catalog.proto` |
| protobuf 包 | `catalog.v1` |
| go_package | `go-video/services/catalog/rpc` |
| 发现用的 etcd key | `catalog.v1.rpc`（`services/catalog/etc/catalog.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`catalog.v1.rpc`） |
| 监听 | `8096`（`services/catalog/etc/catalog.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_catalog` |
| 方法数 | 12（service `Catalog`） |
| 网关消费方 | `app:CatalogRPC`、`admin:CatalogRPC` |

## 契约说明

> 版权内容目录服务。
> 数据所有权：catalog_work / catalog_season / catalog_episode /
>   catalog_zone / catalog_tag（见 AGENTS.md §5）。
> 普通用户投稿整片由 video 服务负责，catalog 不接受 UGC 写入。
> 发布集必须通过 rights 版权窗口与 asset 媒资就绪的跨服务校验，
> catalog 不复制 rights/asset 的主数据，只通过 RPC 读取结论（AGENTS.md §5/§8）。
> 字段命名遵循 snake_case，Go 生成代码由 goctl 产出 PascalCase。

## service `Catalog`

> Catalog 版权内容目录服务。 / 依据 AGENTS.md §1，不实现商业化；依据 §5，只持有本域主数据， / 跨服务通过 RPC 或版本化领域事件（content.published.v1）通信。

gRPC 方法前缀：`catalog.v1.Catalog/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `CreateWork` | [`CreateWorkReq`](#message-createworkreq) | [`WorkReply`](#message-workreply) | 运营创建作品 |
| 2 | `GetWork` | [`WorkReq`](#message-workreq) | [`WorkReply`](#message-workreply) | 查询作品 |
| 3 | `ListWorks` | [`ListReq`](#message-listreq) | [`WorksReply`](#message-worksreply) | 分页查询作品 |
| 4 | `CreateSeason` | [`CreateSeasonReq`](#message-createseasonreq) | [`SeasonReply`](#message-seasonreply) | 运营创建季 |
| 5 | `ListSeasons` | [`SeasonReq`](#message-seasonreq) | [`SeasonsReply`](#message-seasonsreply) | 查询某作品的季列表 |
| 6 | `CreateEpisode` | [`CreateEpisodeReq`](#message-createepisodereq) | [`EpisodeReply`](#message-episodereply) | 运营创建集（关联 asset_id；建集前经 asset RPC 校验媒资存在且已完成扫描探测） |
| 7 | `ListEpisodes` | [`SeasonReq`](#message-seasonreq) | [`EpisodesReply`](#message-episodesreply) | 查询某季的集列表 |
| 8 | `GetEpisode` | [`EpisodeReq`](#message-episodereq) | [`EpisodeReply`](#message-episodereply) | 查询集详情 |
| 9 | `PublishEpisode` | [`EpisodeReq`](#message-episodereq) | [`EpisodeReply`](#message-episodereply) | 上架集（状态流转到 PUBLISHED；先经 rights 版权窗口与 asset 媒资就绪校验） |
| 10 | `OfflineEpisode` | [`EpisodeReq`](#message-episodereq) | [`EpisodeReply`](#message-episodereply) | 下架集 |
| 11 | `ListZones` | [`EmptyReq`](#message-emptyreq) | [`ZonesReply`](#message-zonesreply) | 分区树（扁平列表） |
| 12 | `ListTags` | [`ListTagsReq`](#message-listtagsreq) | [`TagsReply`](#message-tagsreply) | 标签查询（按名字模糊或 ID 列表） |

## 消息与枚举

### message `EmptyReq`

> 空请求（用于 ListZones）

（空消息）

### message `WorkReply`

> --- 作品（catalog_work） --- / 作品类型常量：1 电影、2 电视剧、3 番剧、4 纪录片 / 作品状态常量：0 草稿、1 上架、2 下架 / 作品回复。season_id 既是作品 ID，也指向其主季 ID（电影为本身）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `season_id` | `int64` | 1 | — | 作品主季 ID（作品唯一标识） |
| `title` | `string` | 2 | — | 标题 |
| `cover` | `string` | 3 | — | 封面 URL |
| `typeid` | `int32` | 4 | — | 作品类型 |
| `intro` | `string` | 5 | — | 简介 |
| `state` | `int32` | 6 | — | 状态 |

### message `CreateWorkReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `title` | `string` | 1 | — | 标题 |
| `cover` | `string` | 2 | — | 封面 URL |
| `typeid` | `int32` | 3 | — | 作品类型 |
| `intro` | `string` | 4 | — | 简介 |
| `operator` | `string` | 5 | — | 运营操作人 |

### message `WorkReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `season_id` | `int64` | 1 | — | 作品主季 ID |

### message `ListReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `typeid` | `int32` | 1 | — | 可选类型过滤（0 不过滤） |
| `state` | `int32` | 2 | — | 可选状态过滤（-1 不过滤） |
| `pn` | `int32` | 3 | — | 页码（从 1 开始） |
| `ps` | `int32` | 4 | — | 每页大小（最大 50） |

### message `WorksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 总数 |
| `works` | [`WorkReply`](#message-workreply) | 2 | repeated | 作品列表 |

### message `SeasonReply`

> --- 季（catalog_season） --- / 季状态常量：0 草稿、1 上架、2 下架

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `season_id` | `int64` | 1 | — | 季 ID（本季自身标识） |
| `season_no` | `int32` | 2 | — | 季编号 |
| `title` | `string` | 3 | — | 季标题 |
| `cover` | `string` | 4 | — | 季封面 |
| `state` | `int32` | 5 | — | 状态 |

### message `CreateSeasonReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `season_id` | `int64` | 1 | — | 所属作品的主季 ID（父作品） |
| `season_no` | `int32` | 2 | — | 季编号 |
| `title` | `string` | 3 | — | 季标题 |
| `cover` | `string` | 4 | — | 季封面 |
| `operator` | `string` | 5 | — | 运营操作人 |

### message `SeasonReq`

> SeasonReq 在不同 RPC 中语义： /   ListSeasons：season_id 为作品主季 ID，返回该作品的全部季。 /   ListEpisodes：season_id 为具体季 ID，返回该季的全部集。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `season_id` | `int64` | 1 | — | 作品主季 ID（ListSeasons）或季 ID（ListEpisodes） |

### message `SeasonsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `seasons` | [`SeasonReply`](#message-seasonreply) | 1 | repeated | 季列表 |

### message `EpisodeReply`

> --- 集（catalog_episode） --- / 集状态常量：0 草稿、1 上架（PUBLISHED）、2 下架

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `epid` | `int64` | 1 | — | 集 ID |
| `season_id` | `int64` | 2 | — | 所属季 ID |
| `ep_no` | `int32` | 3 | — | 集编号 |
| `title` | `string` | 4 | — | 集标题 |
| `asset_id` | `int64` | 5 | — | 关联媒资 ID（由 asset 服务拥有） |
| `duration` | `int64` | 6 | — | 时长（秒） |
| `state` | `int32` | 7 | — | 状态 |

### message `CreateEpisodeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `season_id` | `int64` | 1 | — | 所属季 ID |
| `ep_no` | `int32` | 2 | — | 集编号 |
| `title` | `string` | 3 | — | 集标题 |
| `asset_id` | `int64` | 4 | — | 关联媒资 ID |
| `duration` | `int64` | 5 | — | 时长（秒） |
| `operator` | `string` | 6 | — | 运营操作人 |

### message `EpisodeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `epid` | `int64` | 1 | — | 集 ID |
| `operator_mid` | `int64` | 2 | — | operator_mid / region 供 PublishEpisode 做上架审计与按地区的版权窗口校验。 / 只追加字段号，不复用历史编号，旧客户端不传时按 0/"" 处理（服务端回落到配置默认地区）。 / 运营操作人 MID（0 表示上游未透传，仅记录不阻塞） |
| `region` | `string` | 3 | — | 上架地区代码（如 CN）；留空时回落到 catalog 配置的 DefaultRegion |

### message `EpisodesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `episodes` | [`EpisodeReply`](#message-episodereply) | 1 | repeated | 集列表 |

### message `ZoneReply`

> --- 分区（catalog_zone） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `zoneid` | `int32` | 1 | — | 分区 ID |
| `name` | `string` | 2 | — | 分区名 |
| `parent` | `int32` | 3 | — | 父分区 ID（0 为顶级） |

### message `ZonesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `zones` | [`ZoneReply`](#message-zonereply) | 1 | repeated | 分区列表（扁平，由调用方组装树） |

### message `TagReply`

> --- 标签（catalog_tag） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tagid` | `int64` | 1 | — | 标签 ID |
| `name` | `string` | 2 | — | 标签名 |

### message `ListTagsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `name` | `string` | 1 | — | 模糊匹配名（为空时按 tagids 查询） |
| `tagids` | `int64` | 2 | repeated | 指定 ID 列表（name 为空时生效，最多 100） |
| `pn` | `int32` | 3 | — | 页码（从 1 开始） |
| `ps` | `int32` | 4 | — | 每页大小（最大 50） |

### message `TagsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tags` | [`TagReply`](#message-tagreply) | 1 | repeated | 标签列表 |
