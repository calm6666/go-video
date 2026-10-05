# RPC · `engagement`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/engagement/rpc/engagement.proto` |
| protobuf 包 | `engagement.v1` |
| go_package | `go-video/services/engagement/rpc` |
| 发现用的 etcd key | `engagement.v1.rpc`（`services/engagement/etc/engagement.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`engagement.v1.rpc`） |
| 监听 | `8084`（`services/engagement/etc/engagement.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_engagement` |
| 方法数 | 16（service `Engagement`） |
| 网关消费方 | `app:EngagementRPC` |

## 契约说明

> 说明：本契约移植自参考仓库 openbilibili-go-common：
>   - app/service/main/thumbup：8 个点赞方法（Like/Stats/MultiStats/HasLike/
>     UserLikes/ItemLikes/UpdateCount/RawStat）
>   - app/service/main/favorite：核心收藏方法（AddFav/DelFav/IsFavored/
>     IsFavoreds/UserFolders/AddFolder/DelFolder）
>   - app/service/main/share：AddShare
> 依据 AGENTS.md §1，不移植 coin 服务（投币属商业化范围外）。
> favorite 的 30+ 高级收藏夹管理方法（MoveFavs/CopyFavs/SortFavs 等）不移植，
> 由 services/operation 管理后台承接。

## service `Engagement`

> Engagement 点赞、收藏、分享社区互动服务。 / 依据 AGENTS.md §1，不实现投币或任何商业化余额； / 依据 §5，本服务只持有用户行为事实、收藏夹、可重算计数， / 不复制视频主数据，不直接更新推荐结果。

gRPC 方法前缀：`engagement.v1.Engagement/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `Like` | [`LikeReq`](#message-likereq) | [`LikeReply`](#message-likereply) | 点赞/取消点赞/点踩（幂等：重复请求不重复增加计数） |
| 2 | `Stats` | [`StatsReq`](#message-statsreq) | [`StatsReply`](#message-statsreply) | 批量查询对象计数与当前用户状态 |
| 3 | `MultiStats` | [`MultiStatsReq`](#message-multistatsreq) | [`MultiStatsReply`](#message-multistatsreply) | 跨业务批量查询计数 |
| 4 | `HasLike` | [`HasLikeReq`](#message-haslikereq) | [`HasLikeReply`](#message-haslikereply) | 批量查询用户是否点赞 |
| 5 | `UserLikes` | [`UserLikesReq`](#message-userlikesreq) | [`UserLikesReply`](#message-userlikesreply) | 用户的点赞列表（分页） |
| 6 | `ItemLikes` | [`ItemLikesReq`](#message-itemlikesreq) | [`ItemLikesReply`](#message-itemlikesreply) | 对象的点赞人列表（分页） |
| 7 | `UpdateCount` | [`UpdateCountReq`](#message-updatecountreq) | [`EmptyReply`](#message-emptyreply) | 运营修改计数（增量） |
| 8 | `RawStat` | [`RawStatReq`](#message-rawstatreq) | [`RawStatReply`](#message-rawstatreply) | 查询原始计数（未修正值） |
| 9 | `AddFav` | [`AddFavReq`](#message-addfavreq) | [`EmptyReply`](#message-emptyreply) | 添加收藏 |
| 10 | `DelFav` | [`DelFavReq`](#message-delfavreq) | [`EmptyReply`](#message-emptyreply) | 删除收藏 |
| 11 | `IsFavored` | [`IsFavoredReq`](#message-isfavoredreq) | [`IsFavoredReply`](#message-isfavoredreply) | 查询是否已收藏 |
| 12 | `IsFavoreds` | [`IsFavoredsReq`](#message-isfavoredsreq) | [`IsFavoredsReply`](#message-isfavoredsreply) | 批量查询是否已收藏 |
| 13 | `UserFolders` | [`UserFoldersReq`](#message-userfoldersreq) | [`UserFoldersReply`](#message-userfoldersreply) | 用户收藏夹列表 |
| 14 | `AddFolder` | [`AddFolderReq`](#message-addfolderreq) | [`AddFolderReply`](#message-addfolderreply) | 创建收藏夹 |
| 15 | `DelFolder` | [`DelFolderReq`](#message-delfolderreq) | [`EmptyReply`](#message-emptyreply) | 删除收藏夹 |
| 16 | `AddShare` | [`AddShareReq`](#message-addsharereq) | [`AddShareReply`](#message-addsharereply) | 记录分享并返回分享数 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `Action`

> 点赞动作

| 值 | 编号 | 说明 |
|---|---|---|
| `ACTION_UNSPECIFIED` | 0 | 未指定 |
| `ACTION_LIKE` | 1 | 点赞 |
| `ACTION_CANCEL_LIKE` | 2 | 取消点赞 |
| `ACTION_DISLIKE` | 3 | 点踩 |
| `ACTION_CANCEL_DISLIKE` | 4 | 取消点踩 |

### enum `LikeState`

> 点赞状态

| 值 | 编号 | 说明 |
|---|---|---|
| `STATE_UNSPECIFIED` | 0 | 未指定 |
| `STATE_LIKE` | 1 | 已点赞 |
| `STATE_DISLIKE` | 2 | 已点踩 |

### message `LikeReq`

> --- 点赞（移植自 thumbup） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `business` | `string` | 1 | — | 业务名（如 "archive"） |
| `mid` | `int64` | 2 | — | 操作用户 ID |
| `up_mid` | `int64` | 3 | — | 被点赞内容 UP 主 ID（用于通知） |
| `origin_id` | `int64` | 4 | — | 来源 ID |
| `message_id` | `int64` | 5 | — | 对象 ID |
| `action` | [`Action`](#enum-action) | 6 | — | 动作 |
| `ip` | `string` | 7 | — | 调用方 IP |

### message `LikeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `origin_id` | `int64` | 1 | — | 来源 ID |
| `message_id` | `int64` | 2 | — | 对象 ID |
| `like_number` | `int64` | 3 | — | 当前点赞数 |
| `dislike_number` | `int64` | 4 | — | 当前点踩数 |

### message `StatState`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `origin_id` | `int64` | 1 | — | 来源 ID |
| `message_id` | `int64` | 2 | — | 对象 ID |
| `like_number` | `int64` | 3 | — | 点赞数 |
| `dislike_number` | `int64` | 4 | — | 点踩数 |
| `like_state` | [`LikeState`](#enum-likestate) | 5 | — | 当前用户状态 |

### message `StatsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `business` | `string` | 1 | — | 业务名 |
| `origin_id` | `int64` | 2 | — | 来源 ID |
| `message_ids` | `int64` | 3 | repeated | 对象 ID 列表（最多 100） |
| `mid` | `int64` | 4 | — | 当前用户（可选，不需要 like_state 时不填） |
| `ip` | `string` | 5 | — | 调用方 IP |

### message `StatsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stats` | [`map<int64, StatState>`](#message-statstate) | 1 | — | message_id → 计数与状态 |

### message `MultiStatsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 当前用户 |
| `business` | [`map<string, MultiStatsReq.Business>`](#message-multistatsreqbusiness) | 2 | — | 业务 → 记录列表 |
| `ip` | `string` | 3 | — | 调用方 IP |

### message `MultiStatsReq.Record`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `origin_id` | `int64` | 1 | — | 来源 ID |
| `message_id` | `int64` | 2 | — | 对象 ID |

### message `MultiStatsReq.Business`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `records` | [`MultiStatsReq.Record`](#message-multistatsreqrecord) | 1 | repeated | — |

### message `MultiStatsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `business` | [`map<string, MultiStatsReply.Records>`](#message-multistatsreplyrecords) | 1 | — | 业务 → 记录映射 |

### message `MultiStatsReply.Records`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `records` | [`map<int64, StatState>`](#message-statstate) | 1 | — | — |

### message `HasLikeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `business` | `string` | 1 | — | 业务名 |
| `message_ids` | `int64` | 2 | repeated | 对象 ID 列表 |
| `mid` | `int64` | 3 | — | 用户 ID |
| `ip` | `string` | 4 | — | 调用方 IP |

### message `UserLikeState`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `time` | `int64` | 2 | — | 点赞时间（Unix 秒） |
| `state` | [`LikeState`](#enum-likestate) | 3 | — | 点赞状态 |

### message `HasLikeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `states` | [`map<int64, UserLikeState>`](#message-userlikestate) | 1 | — | message_id → 状态 |

### message `UserLikesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `business` | `string` | 1 | — | 业务名 |
| `mid` | `int64` | 2 | — | 用户 ID |
| `pn` | `int32` | 3 | — | 页码（从 1 开始） |
| `ps` | `int32` | 4 | — | 每页大小（最大 50） |
| `ip` | `string` | 5 | — | 调用方 IP |

### message `ItemRecord`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `message_id` | `int64` | 1 | — | 对象 ID |
| `time` | `int64` | 2 | — | 点赞时间（Unix 秒） |

### message `UserLikesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 总数 |
| `items` | [`ItemRecord`](#message-itemrecord) | 2 | repeated | 点赞记录列表 |

### message `ItemLikesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `business` | `string` | 1 | — | 业务名 |
| `origin_id` | `int64` | 2 | — | 来源 ID |
| `message_id` | `int64` | 3 | — | 对象 ID |
| `last_mid` | `int64` | 4 | — | 上页最后 mid（去重翻页用） |
| `pn` | `int32` | 5 | — | 页码 |
| `ps` | `int32` | 6 | — | 每页大小（最大 50） |
| `ip` | `string` | 7 | — | 调用方 IP |

### message `UserRecord`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `time` | `int64` | 2 | — | 点赞时间（Unix 秒） |

### message `ItemLikesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `users` | [`UserRecord`](#message-userrecord) | 1 | repeated | 点赞用户列表 |

### message `UpdateCountReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `business` | `string` | 1 | — | 业务名 |
| `origin_id` | `int64` | 2 | — | 来源 ID |
| `message_id` | `int64` | 3 | — | 对象 ID |
| `like_change` | `int64` | 4 | — | 点赞数增量 |
| `dislike_change` | `int64` | 5 | — | 点踩数增量 |
| `operator` | `string` | 6 | — | 操作人（运营/系统） |
| `ip` | `string` | 7 | — | 调用方 IP |

### message `RawStatReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `business` | `string` | 1 | — | 业务名 |
| `origin_id` | `int64` | 2 | — | 来源 ID |
| `message_id` | `int64` | 3 | — | 对象 ID |
| `ip` | `string` | 4 | — | 调用方 IP |

### message `RawStatReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `origin_id` | `int64` | 1 | — | 来源 ID |
| `message_id` | `int64` | 2 | — | 对象 ID |
| `like_number` | `int64` | 3 | — | 点赞数（原始值） |
| `dislike_number` | `int64` | 4 | — | 点踩数（原始值） |
| `like_change` | `int64` | 5 | — | 点赞数修正增量 |
| `dislike_change` | `int64` | 6 | — | 点踩数修正增量 |

### message `AddFavReq`

> --- 收藏（移植自 favorite 核心） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tp` | `int32` | 1 | — | 收藏类型（与 obc 一致：2 视频、11 视频（ugv）、12 音频 等） |
| `mid` | `int64` | 2 | — | 用户 ID |
| `fid` | `int64` | 3 | — | 收藏夹 ID（0 默认夹） |
| `oid` | `int64` | 4 | — | 目标 ID |
| `otype` | `int32` | 5 | — | 目标子类型 |

### message `DelFavReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tp` | `int32` | 1 | — | 收藏类型 |
| `mid` | `int64` | 2 | — | 用户 ID |
| `fid` | `int64` | 3 | — | 收藏夹 ID |
| `oid` | `int64` | 4 | — | 目标 ID |
| `otype` | `int32` | 5 | — | 目标子类型 |

### message `IsFavoredReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tp` | `int32` | 1 | — | 收藏类型 |
| `mid` | `int64` | 2 | — | 用户 ID |
| `oid` | `int64` | 3 | — | 目标 ID |

### message `IsFavoredReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `faved` | `bool` | 1 | — | 是否已收藏 |

### message `IsFavoredsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tp` | `int32` | 1 | — | 收藏类型 |
| `mid` | `int64` | 2 | — | 用户 ID |
| `oids` | `int64` | 3 | repeated | 目标 ID 列表（最多 100） |

### message `IsFavoredsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `faveds` | `map<int64, bool>` | 1 | — | oid → 是否已收藏 |

### message `Folder`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `fid` | `int64` | 1 | — | 收藏夹 ID |
| `mid` | `int64` | 2 | — | 用户 ID |
| `name` | `string` | 3 | — | 收藏夹名 |
| `description` | `string` | 4 | — | 描述 |
| `cover` | `string` | 5 | — | 封面 URL |
| `public` | `int32` | 6 | — | 是否公开：0 私密、1 公开 |
| `state` | `int32` | 7 | — | 状态：0 正常、1 删除 |
| `ctime` | `int64` | 8 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 9 | — | 修改时间（Unix 秒） |
| `count` | `int32` | 10 | — | 收藏数量快照 |

### message `UserFoldersReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tp` | `int32` | 1 | — | 收藏类型 |
| `mid` | `int64` | 2 | — | 用户 ID |
| `vmid` | `int64` | 3 | — | 被查看用户 ID（用于他人主页） |
| `oid` | `int64` | 4 | — | 目标 ID（可选，用于"收藏到哪个夹"提示） |
| `all_count` | `bool` | 5 | — | 是否返回全部分类计数 |
| `otype` | `int32` | 6 | — | 目标子类型 |

### message `UserFoldersReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `folders` | [`Folder`](#message-folder) | 1 | repeated | 收藏夹列表 |

### message `AddFolderReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tp` | `int32` | 1 | — | 收藏类型 |
| `mid` | `int64` | 2 | — | 用户 ID |
| `name` | `string` | 3 | — | 收藏夹名 |
| `description` | `string` | 4 | — | 描述 |
| `cover` | `string` | 5 | — | 封面 URL |
| `public` | `int32` | 6 | — | 是否公开 |

### message `AddFolderReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `fid` | `int64` | 1 | — | 新收藏夹 ID |

### message `DelFolderReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tp` | `int32` | 1 | — | 收藏类型 |
| `mid` | `int64` | 2 | — | 用户 ID |
| `fid` | `int64` | 3 | — | 收藏夹 ID |

### message `AddShareReq`

> --- 分享（移植自 share） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `oid` | `int64` | 1 | — | 目标 ID |
| `mid` | `int64` | 2 | — | 用户 ID |
| `type` | `int32` | 3 | — | 目标类型 |
| `ip` | `string` | 4 | — | 调用方 IP |

### message `AddShareReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `shares` | `int64` | 1 | — | 当前分享数 |
