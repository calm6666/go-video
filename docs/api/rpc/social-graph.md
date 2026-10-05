# RPC · `social-graph`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/social-graph/rpc/socialgraph.proto` |
| protobuf 包 | `socialgraph.v1` |
| go_package | `go-video/services/social-graph/rpc` |
| 发现用的 etcd key | `socialgraph.v1.rpc`（`services/social-graph/etc/socialgraph.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`socialgraph.v1.rpc`） |
| 监听 | `8091`（`services/social-graph/etc/socialgraph.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_social_graph` |
| 方法数 | 14（service `SocialGraph`） |
| 网关消费方 | `app:SocialGraphRPC` |

## 契约说明

> 说明：本契约移植自参考仓库 openbilibili-go-common app/service/main/relation。
> 依据 AGENTS.md §1/§5，本服务只持有关系事实、关系计数快照、特别关注与黑名单，
> 不复制用户主资料、不直接写推荐结果，不暴露 HTTP。
> 调用方（gateway、account、feed 等）通过 gRPC 访问；关注/拉黑校验中涉及
> "对方账号是否存在/状态"的部分由调用方负责，本服务不依赖 account RPC。

## service `SocialGraph`

> SocialGraph 用户关系链服务。 / 依据 AGENTS.md §1，不实现投币、支付或任何商业化余额； / 依据 §5，本服务独占 relation_follow/relation_black/relation_stat/relation_special 关系数据， / 其他服务只能通过本服务 RPC 或领域事件读取关系，不得直连本服务数据库。

gRPC 方法前缀：`socialgraph.v1.SocialGraph/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `Follow` | [`FollowReq`](#message-followreq) | [`EmptyReply`](#message-emptyreply) | 关注（幂等：重复不重复计数） |
| 2 | `Unfollow` | [`UnfollowReq`](#message-unfollowreq) | [`EmptyReply`](#message-emptyreply) | 取关（幂等） |
| 3 | `IsFollowing` | [`RelationReq`](#message-relationreq) | [`RelationReply`](#message-relationreply) | 查询 mid 是否关注 owner |
| 4 | `IsFollowedBatch` | [`RelationsReq`](#message-relationsreq) | [`RelationsReply`](#message-relationsreply) | 批量查询 mid 是否关注 owners |
| 5 | `RichRelations` | [`RichRelationsReq`](#message-richrelationsreq) | [`RichRelationsReply`](#message-richrelationsreply) | 批量查询 owner 与 mids 的全部关系位（双向关注 + 拉黑 + 特别关注） |
| 6 | `ListFollowing` | [`ListReq`](#message-listreq) | [`FollowingReply`](#message-followingreply) | mid 的关注列表（分页） |
| 7 | `ListFollower` | [`ListReq`](#message-listreq) | [`FollowerReply`](#message-followerreply) | mid 的粉丝列表（分页） |
| 8 | `Stat` | [`MidReq`](#message-midreq) | [`StatReply`](#message-statreply) | 查询关注数与粉丝数 |
| 9 | `AddBlack` | [`BlackReq`](#message-blackreq) | [`EmptyReply`](#message-emptyreply) | 拉黑（自动取关） |
| 10 | `DelBlack` | [`BlackReq`](#message-blackreq) | [`EmptyReply`](#message-emptyreply) | 取消拉黑 |
| 11 | `IsBlacked` | [`RelationReq`](#message-relationreq) | [`RelationReply`](#message-relationreply) | 查询 mid 是否拉黑 owner |
| 12 | `ListBlacks` | [`ListReq`](#message-listreq) | [`BlacksReply`](#message-blacksreply) | mid 的黑名单列表（分页） |
| 13 | `AddSpecial` | [`SpecialReq`](#message-specialreq) | [`EmptyReply`](#message-emptyreply) | 特别关注（必先关注） |
| 14 | `DelSpecial` | [`SpecialReq`](#message-specialreq) | [`EmptyReply`](#message-emptyreply) | 取消特别关注 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `RelationAttr`

> 关系类型属性位。 /  / **attr 字段是按位或的掩码**，不是单值枚举：FOLLOWING=1、FOLLOWER=2、BLACKED=4、 / SPECIAL=8 各占一位，MUTUAL=3 是 FOLLOWING|FOLLOWER 的派生便捷值（不是独立的位）， / 消费方判定互关应当写 `attr&1 != 0 && attr&2 != 0`，不要写 `attr == 3` / （同时被拉黑时 attr=7，等值判定会漏）。 / 列表接口（ListFollowing/ListFollower/ListBlacks）只在 attr 上填本列表那一位。 /  / 修订记录：2026-10-04 把 RELATION_ATTR_SPECIAL 从 5 改为 8。原值 5 不是 2 的幂， / 无法参与按位或；全仓无任何代码引用该常量（只有生成的枚举表和自动生成的接口文档）， / 且本仓库尚未有任何服务启动过，故按「掩码位」口径收口。

| 值 | 编号 | 说明 |
|---|---|---|
| `RELATION_ATTR_UNSPECIFIED` | 0 | 未指定/无关系 |
| `RELATION_ATTR_FOLLOWING` | 1 | bit0：发起方关注被查询方 |
| `RELATION_ATTR_FOLLOWER` | 2 | bit1：被查询方关注发起方 |
| `RELATION_ATTR_MUTUAL` | 3 | 派生值 = FOLLOWING\|FOLLOWER |
| `RELATION_ATTR_BLACKED` | 4 | bit2：发起方已拉黑被查询方 |
| `RELATION_ATTR_SPECIAL` | 8 | bit3：发起方特别关注被查询方 |

### message `FollowReq`

> --- 关注 --- / FollowReq 关注请求。 / 幂等：重复关注不会重复增加 following/follower 计数。 / 黑名单约束：若 mid 已拉黑 follower_mid，则关注前需先取消拉黑（由 logic 校验）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 操作用户 ID（关注发起方） |
| `follower_mid` | `int64` | 2 | — | 被关注者 ID |
| `real_ip` | `string` | 3 | — | 调用方 IP（审计用） |

### message `UnfollowReq`

> UnfollowReq 取关请求。幂等。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 操作用户 ID |
| `follower_mid` | `int64` | 2 | — | 被取关者 ID |
| `real_ip` | `string` | 3 | — | 调用方 IP |

### message `RelationReq`

> RelationReq 单个关系查询请求。 / 语义：mid 是否关注 owner（即 mid 的关注集合中是否包含 owner）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 发起方用户 ID |
| `owner` | `int64` | 2 | — | 被查询方用户 ID |
| `real_ip` | `string` | 3 | — | 调用方 IP |

### message `RelationReply`

> RelationReply 单个关系查询结果。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `following` | `bool` | 1 | — | mid 是否关注 owner |

### message `RelationsReq`

> RelationsReq 批量关系查询请求。 / 语义：mid 是否关注 owners 列表中的每一个。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 发起方用户 ID |
| `owners` | `int64` | 2 | repeated | 被查询方用户 ID 列表（最多 100） |
| `real_ip` | `string` | 3 | — | 调用方 IP |

### message `RelationsReply`

> RelationsReply 批量关系查询结果。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `following` | `map<int64, bool>` | 1 | — | owner → mid 是否关注 owner |

### message `RichRelationsReq`

> RichRelationsReq 批量富关系查询请求。 / 语义：以 owner 为视角，逐个给出 owner 与 mids 中每个用户之间的**全部**关系位。 / 与 IsFollowedBatch 的区别：那个只有 owner→X 一个方向、且只有一位；本 RPC 两个方向 / 加上特别关注/黑名单共四位，一次查询完成（调用方无需按方向扇出）。 / 上限：mids 最多 100 个（超过服务端直接报错，调用方必须自行切片）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `owner` | `int64` | 1 | — | 视角用户 ID |
| `mids` | `int64` | 2 | repeated | 被查询用户 ID 列表（最多 100） |
| `real_ip` | `string` | 3 | — | 调用方 IP |

### message `RichRelationsReply`

> RichRelationsReply 批量富关系查询结果。 / attrs：mid → RelationAttr 掩码（见 enum 注释的位定义）。 / 口径： /   - bit0(1) FOLLOWING = owner 关注 mid；bit1(2) FOLLOWER = mid 关注 owner； /     bit2(4) BLACKED = owner 拉黑 mid；bit3(8) SPECIAL = owner 特别关注 mid。 /   - 四位各自独立从对应表读出，**不做优先级压制**：拉黑不会清掉对方对自己的关注位， /     特别关注也不保证 FOLLOWING 位为真（本服务的 Unfollow 不动 relation_special， /     故存在 special 已置、关注已撤的行；要「特别关注且仍关注」请同时判两位）。 /   - 关系为空的 mid 返回 attr=0（不是缺键）；本服务不判断 mid 是否存在 /     （账号存在性由调用方负责，见文件头说明）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `attrs` | `map<int64, int32>` | 1 | — | — |

### message `ListReq`

> ListReq 分页列表请求（关注列表/粉丝列表/黑名单列表通用）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 查询用户 ID |
| `pn` | `int32` | 2 | — | 页码（从 1 开始） |
| `ps` | `int32` | 3 | — | 每页大小（最大 50） |
| `real_ip` | `string` | 4 | — | 调用方 IP |

### message `RelationItem`

> RelationItem 列表项。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `ctime` | `int64` | 2 | — | 关系建立时间（Unix 秒） |
| `attr` | `int32` | 3 | — | 关系属性位（参考 RelationAttr） |

### message `FollowingReply`

> FollowingReply 关注列表响应。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 关注总数 |
| `items` | [`RelationItem`](#message-relationitem) | 2 | repeated | 关注列表 |

### message `FollowerReply`

> FollowerReply 粉丝列表响应。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 粉丝总数 |
| `items` | [`RelationItem`](#message-relationitem) | 2 | repeated | 粉丝列表 |

### message `MidReq`

> --- 计数 --- / MidReq 单用户请求。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `real_ip` | `string` | 2 | — | 调用方 IP |

### message `StatReply`

> StatReply 关系计数响应。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `following` | `int64` | 1 | — | 关注数 |
| `follower` | `int64` | 2 | — | 粉丝数 |
| `whisper` | `int32` | 3 | — | 悄悄关注数（保留字段，本期固定 0） |

### message `BlackReq`

> --- 黑名单 --- / BlackReq 拉黑/取消拉黑请求。 / 拉黑时若 mid 已关注 black_mid，将自动取关。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 操作用户 ID |
| `black_mid` | `int64` | 2 | — | 被拉黑者 ID |
| `real_ip` | `string` | 3 | — | 调用方 IP |

### message `BlacksReply`

> BlacksReply 黑名单列表响应。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 拉黑总数 |
| `items` | [`RelationItem`](#message-relationitem) | 2 | repeated | 拉黑列表 |

### message `SpecialReq`

> --- 特别关注 --- / SpecialReq 特别关注请求。 / 约束：必先关注；未关注时返回 ErrSpecialNeedFollow。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 操作用户 ID |
| `special_mid` | `int64` | 2 | — | 被特别关注者 ID |
| `real_ip` | `string` | 3 | — | 调用方 IP |
