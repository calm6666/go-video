# gateway/app

面向 Android、iOS、HarmonyOS、桌面客户端的 BFF 网关。属于 [gateway](..) 的独立子服务之一。

## 职责

- 对外公网 HTTP 入口，当前 198 条路由分布在 28 个前缀分组下（`/api`、`/account*`、`/x/*`、`/upload`、
  `/video`、`/catalog`、`/social`、`/feed`、`/engagement`、`/playback`、`/danmaku`、`/comment`、`/search`、
  `/inbox`、`/notification`、`/up`、`/moderation`、`/private-message`、`/live` 和商业化五组
  `/membership`、`/coin`、`/wallet`、`/order`、`/creator/revenue`）。逐前缀清单见[接口文档索引](../../docs/api/http/app/README.md)。
- 用户 access_token 校验、设备/客户端版本上下文、限流、风控、灰度和 trace_id。
- 面向端的 BFF 聚合和字段裁剪；不拥有用户、视频、评论等领域主数据。
- 长任务（上传、转码、审核、推荐计算）返回任务 ID，不在网关请求内同步等待。

## 边界

- 可以调用领域 RPC，不能直接读业务数据库或拼接数据库 model。
- 下游错误映射为稳定的公共错误码（见 [common/httpresponse](../../common/httpresponse)），不泄露 SQL、Token、对象存储签名。
- HTTP 响应统一使用四字段信封（`code`/`message`/`data`/`ttl`）。

## 路由

| 前缀 | 路由 | 下游 |
|---|---|---|
| `/api` | 健康检查 `GET /healthz` | - |
| `/account` | `/info`、`/infos`、`/info/by/name`、`/card`、`/cards`、`/profile`、`/profile/stat`、`/vip`、`/vips`、`/privacy`（白名单 appkey 中间件） | account gRPC |
| `/account/v1`、`/account/v2` | 老客户端兼容路由：`/info`、`/infos`、`/card`、`/vip`、`/myinfo`、`/userinfo` | account gRPC |
| `/x/member` | 资料：`/base`、`/batchBase`、`/moral`、`/exp`、`/level`、`/official`、`/web/*`、`/app/*/update` | user-profile gRPC |
| `/x/member/realname` | 实名认证：`/status`、`/info`、`/apply`、`/apply/status`、`/adult`、`/check`、`/tel/capture`、`/tel/capture/check` | user-profile gRPC |
| `/x/passport-login` | 登录与凭据：`GET /key`（RSA 公钥）、`POST /web/login`、`/web/captcha/*`、`/web/register`、`/exit`、`/token/renew`、`/web/cookie/info`、`/web/token/info`、`POST /password/set`、`/password/reset`、`GET /password/history/check`、`GET /login/logs` | account gRPC |
| `/upload` | 分片上传：`/init`、`/url`、`/complete`、`/abort`、`/status`（预签名，网关不持有 OSS 密钥） | upload gRPC |
| `/video` | 稿件：`POST /submissions`、`GET /submissions`、`GET /submissions/:aid`、`POST /submissions/:aid/transition`、`/submissions/:aid/update`、`/submissions/:aid/delete` | video gRPC |
| `/catalog` | 版权目录：`/works`、`/works/:work_id`、`/seasons/:season_id/episodes`、`/episodes/:epid`、`/zones` | catalog gRPC |
| `/social` | 关系：`/follow`、`/unfollow`、`/follower`、`/following`、`/is_following`、`/stat`；黑名单：`/black/add`、`/black/del`、`/black/check`、`/black/list`；特别关注：`/special/add`、`/special/del` | social-graph gRPC |
| `/feed` | 动态：`/pull`、`/user/:mid`、`/unread`、`/clear_unread`、`POST /pin`、`/unpin`、`/delete` | feed gRPC |
| `/engagement` | 互动：`/like`、`/has_like`、`/user_likes`、`/fav`、`/unfav`、`/fav/state`、`/fav/states`、`/folders`、`/folder/add`、`/folder/del`、`/share`、`/stats` | engagement gRPC |
| `/playback` | `POST /token`（短期防盗链播放地址，`request_id` 幂等）、`POST /heartbeat`、`GET /session` | playback + video + catalog + transcode gRPC |
| `/danmaku` | `POST /post`、`GET /list`、`POST /delete`、`POST /report`、`POST /user_block`、`GET /user_blocks` | danmaku gRPC |
| `/comment` | 评论：`POST /post`（落 `STATE_PENDING`，由审核推进）、`GET /list`、`GET /replies`、`POST /delete`、`POST /pin`、`POST /report`、`GET /stats` | comment gRPC |
| `/search` | `GET /query`、`/suggest`、`/hot`、`/config`、`/history`、`POST /history/delete`、`/history/clear`、`/query/report` | search-query gRPC |
| `/inbox` | `GET /messages`、`/unread`、`POST /read`、`/read/all`、`/delete` | inbox gRPC |
| `/notification` | 免打扰偏好：`GET /dnd`、`POST /dnd/update`（通知投递由服务端事件驱动，不对端开放群发） | notification gRPC |
| `/up` | 创作者：`GET /special`、`/specials`、`/attr`、`/switch`、`POST /switch` | creator gRPC |
| `/moderation` | 申诉：`POST /appeal`（审核结论仍由 moderation-orchestrator 判定） | moderation-orchestrator gRPC |
| `/private-message` | 私信：11 条（会话、发送、收件箱、举报等） | private-message gRPC |
| `/live` | 直播客户端面：15 条（房间、开播、弹幕礼物等） | live-room gRPC |
| `/membership` `/coin` `/wallet` `/order` | 商业化终端面：会员 6 + 投币 7 + 钱包 7 + 订单 6 条 | membership / coin / payment / trade-order gRPC |
| `/creator/revenue` | 创作者收益：8 条（收益概览、明细、结算单） | creator-revenue gRPC |

上表只列到组一级，**逐条路由（方法、路径、入参位置、请求/响应类型）以生成文档为准**：
[docs/api/http/app/](../../docs/api/http/app/README.md) 按同一口径拆成 28 个分组文件，
可导入的调试集合见 [postman/](../../postman/README.md)。


`playback` 的 CDN 回源校验回调 `VerifyPlaybackToken` 是服务端到 CDN 的入口，不通过本网关对客户端暴露。

## 播放来源解析的假设（`internal/logic/playbacksource.go`）

- 数据所有权：`aid → 版次/媒资` 归 video（`GetPlayableSource`，只有 `PUBLISHED` 稿件可播），
  `epid → 媒资` 归 catalog（集必须已上架且 `asset_id > 0`），转码产物归 transcode。网关只是把三者拼起来。
- 交给 playback 签名的 `object_key` 组成规则是 `{output_bucket}/{output_key}`；
  这依赖 playback 的 `Sign.BaseURL` 配置为**不含 bucket 的 CDN 源站根**。换 bucket 或改回源规则时调整
  playback 配置，本网关契约不变（`signurl.NormalizeURI` 允许 `bucket/key` 形态）。
- 未指定 `template_id` 时按模板 `height`、其次 `bitrate` 选最高清晰度；模板详情读取失败不整体失败，
  退回候选顺序并把 `quality` 留空，由客户端按 `template_id` 展示。
- `/search/query/report` 只上报查询词，网关当前不传用户 IP，因此 risk-control 侧看到的 `ip_hash` 为空。

## 配置

`etc/app.yaml` 的下游 RPC 均为可选（未配置时客户端为 `nil`，对应 logic 返回明确的“服务未配置”错误），
共 24 个：`AccountRPC`、`UserProfileRPC`、`UploadRPC`、`VideoRPC`、`CatalogRPC`、`SocialGraphRPC`、`FeedRPC`、
`EngagementRPC`、`PlaybackRPC`、`DanmakuRPC`、`SearchQueryRPC`、`InboxRPC`、`TranscodeRPC`、
`CommentRPC`、`NotificationRPC`、`CreatorRPC`、`ModerationRPC`、`PrivateMessageRPC`、`LiveRoomRPC`、
`MembershipRPC`、`CoinRPC`、`PaymentRPC`、`TradeOrderRPC`、`CreatorRevenueRPC`。
etcd key 与服务 `Name` 一致：`moderation.v1.rpc` 对应目录名 `moderation-orchestrator`，
`search-query.v1.rpc` 带连字符（search-indexer 的 `searchindexer.v1.rpc` 不带，但网关不直连索引写入服务）。
`PrivacyAppKeys` 是 `/account/privacy` 的 appkey 白名单，`PassportRSAPublicKey` 与 account 服务私钥配对。
业务缓存字段名必须是 `CacheRedis`，不能写 `Redis`（与 go-zero `zrpc` 内嵌的 `RedisKeyConf` 冲突）。

## 生成与运行

```powershell
# 生成（从仓库根目录；框架文件由 goctl 生成，logic 骨架内只填聚合逻辑）
goctl api validate -api gateway/app/api/app.api
goctl api go -api gateway/app/api/app.api -dir gateway/app
node scripts/gen-api-docs.mjs   # 路由/入参一变就重新生成分组文档与 Postman 集合

# 运行
go run ./gateway/app -f gateway/app/etc/app.yaml

# 健康检查
Invoke-WebRequest http://127.0.0.1:8080/api/healthz -UseBasicParsing
```

## 测试覆盖

### 1. 构造器级覆盖：`75/198`（**123 个 logic 没有构造器级用例**）

`internal/logic` 有 198 个 `NewXxxLogic` 构造器（与 198 条路由一一对应），探针逐个在 `*_test.go`
里查引用：**75 个有、123 个没有**。这 123 个方法目前**没有任何直接用例**——既没有构造器级断言，
也不在别的用例里被间接经过。下表按路由域分组列出，可直接据此排补测优先级
（缺口名单来源：`.gotmp/readme-test-aggregate.txt` 的 `### gateway/app` 段）。

| 域 | 未覆盖数 | 未覆盖的构造器（方法名） |
|---|---|---|
| 上传与投稿 | 9 | `InitUpload`、`GetUploadUrl`、`CompleteUpload`、`AbortUpload`、`GetUploadStatus`、`CreateSubmission`、`ListSubmissions`、`GetSubmission`、`TransitionState` |
| 账号与会话（登录/注册/凭据 + 老客户端读侧） | 25 | `Login`、`LoginKey`、`LoginCaptureCheck`、`LoginCaptureSend`、`Logout`、`Register`、`RenewToken`、`TokenInfo`、`CookieInfo`、`Info`、`Infos`、`InfoByName`、`Card`、`Cards`、`Profile`、`ProfileWithStat`、`Privacy`、`Vip`、`Vips`、`V1Info`、`V1Infos`、`V1Card`、`V1Vip`、`V2MyInfo`、`V2UserInfo` |
| 资料写侧与成长值（user-profile） | 16 | `UpdateBaseAll`、`UpdateUname`、`UpdateSign`、`UpdateBirthday`、`UpdateSex`、`UpdateFace`、`MemberBase`、`MemberBatchBase`、`MemberMoral`、`MemberExp`、`MemberLevel`、`MemberOfficial`、`MemberAccount`、`ExpLog`、`ExpReward`、`MoralLog` |
| 实名认证 | 8 | `RealnameStatus`、`RealnameInfo`、`RealnameApply`、`RealnameApplyStatus`、`RealnameAdult`、`RealnameCheck`、`RealnameTelCapture`、`RealnameCaptureCheck` |
| 版权目录 | 5 | `ListWorks`、`GetWork`、`ListEpisodes`、`GetEpisode`、`ListZones` |
| 关系与动态 | 9 | `Follow`、`Unfollow`、`IsFollowing`、`ListFollower`、`ListFollowing`、`SocialStat`、`PullFeed`、`ListUserFeed`、`ClearUnread` |
| 互动（收藏/点赞/分享） | 5 | `AddFav`、`DelFav`、`UserFolders`、`Like`、`EngagementStats` |
| 弹幕 | 6 | `PostDanmaku`、`ListDanmaku`、`DeleteDanmaku`、`ReportDanmaku`、`DanmakuUserBlock`、`ListDanmakuUserBlocks` |
| 搜索 | 8 | `Search`、`SearchSuggest`、`HotKeywords`、`SearchConfig`、`ListSearchHistory`、`DeleteSearchHistory`、`ClearSearchHistory`、`ReportQuery` |
| 站内信 | 6 | `ListInboxMessages`、`InboxUnread`、`GetUnreadCount`、`MarkInboxRead`、`MarkInboxAllRead`、`DeleteInboxMessage` |
| 播放 | 3 | `PlaybackToken`、`PlaybackHeartbeat`、`PlaybackSession` |
| 会员（终端面） | 6 | `MbMy`、`MbPlan`、`MbPlans`、`MbGrants`、`MbEntitlements`、`MbAutoRenew` |
| 钱包（终端面） | 6 | `WalletBalance`、`WalletFlows`、`WalletRecharges`、`WalletRechargeSettle`、`WalletRechargeCancel`、`WalletChannels` |
| 投币（终端面） | 7 | `CoinAccount`、`CoinConfig`、`CoinToss`、`CoinTossCancel`、`CoinTossMine`、`CoinTargetSummary`、`CoinTargetsSummary` |
| 订单（终端面） | 3 | `OrderList`、`OrderDetail`、`OrderCancel` |
| 健康检查 | 1 | `Health` |

**注意**：`commerce_logic_test.go`（12 条）覆盖的是商业化终端面里已有断言的那部分（含
`/creator/revenue` 八条），上表列出的会员/钱包/投币/订单 22 个构造器**不在其内**；
`/account` 与 `/x/member` 两组的读侧也基本是空白，只有 `passport_logic_test.go` 打了
设置/重置/历史校验/登录记录这一小片。

### 2. `internal/logic` 用例清单（13 个用例文件）— `117/11`

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `live_logic_test.go` | 23/0 | live-room 终端面：入参映射（含枚举转换）、DTO→响应投影与裁剪、幂等键必填、终端入口的动作白名单、错误原样上抛；开播资格/风控/资料审核/房间状态机都在服务内判定，网关不代判；反射断言「内部字段没泄漏到终端响应结构」 |
| `pm_logic_test.go` | 16/0 | private-message：请求→RPC 入参映射、DTO→响应投影、错误传播、**未配置客户端时的显式失败**；门禁/幂等/密文/审核结论属服务职责，不在网关重复实现 |
| `commerce_logic_test.go` | 12/0 | 商业化五域面向端的闸门拦在哪、幂等键怎么透传、operator 怎么渲染、可选过滤位原样传递、下游结论（`found`/`duplicated`/`accepted`）怎么投影成 `code:0` 信封；金额、状态机推进、权益判定都不在此 |
| `engagement_extra_logic_test.go` | 10/2 | 收藏状态/收藏夹/分享/点赞状态的请求装配与回包投影（`LikeState` 枚举转 int32、map 与 repeated 字段） |
| `social_extra_logic_test.go` | 9/2 | social-graph 新增路由：字段怎么装配、回包怎么投影成 HTTP types、错误是否原样上抛 |
| `comment_logic_test.go` | 8/2 | comment：入参装配（含 `SortMode` 枚举与待审初始状态）、RPC 回复投影成客户端 types（列表、计数快照、信封四字段） |
| `playbacksource_test.go` | 8/1 | 播放来源解析（`aid`/`epid` → 版次/媒资/模板选择）在三个下游客户端接口上的组装规则 |
| `creator_logic_test.go` | 6/2 | creator：`mid/mids`/`from`/`state` 透传，`UpSpecial.group_ids`（repeated）与 `UpsSpecial.up_specials`（map）的投影 |
| `notification_logic_test.go` | 5/0 | 终端只暴露本人通道偏好与免打扰设置；`Channel` 枚举与 int32 通道码双向转换、`found` 语义、信封四字段 |
| `feed_extra_logic_test.go` | 5/2 | feed 三个写接口（置顶/取消置顶/删除）的 `mid`/`feed_id`/`operator`/`real_ip` 透传与 `EmptyReply`→四字段信封 |
| `passport_logic_test.go` | 7/0 | passport 补充路由：RSA 密文原样透传、回复投影，以及**响应体与错误路径都不得回显口令** |
| `moderation_logic_test.go` | 4/0 | 申诉路由：eligibility 与结论判定属 moderation，网关只转发 `mid`/`ip` 并投影回复 |
| `video_extra_logic_test.go` | 4/0 | 投稿补充路由（编辑元信息、软删）：字段原样透传、回复投影成信封、下游错误上抛 |

规模合计 **119 个顶层用例 + 12 个子用例**（logic 117/11、config 2/1），0 条 skip。

### 3. 其他层

| 层 | 文件 | 顶层/子 | 内容 |
|---|---|---|---|
| `internal/config` | 1 | 2/1 | `etc/app.yaml` 可被 go-zero 加载；24 个下游 RPC 全为 `optional` 时的加载形状 |
| `internal/middleware` | 0 | — | **无离线单测**（access token 校验、限流、风控、灰度、`PrivacyAppKeys` 白名单都没有用例） |
| `internal/svc` | 0 | — | **无离线单测**（`ServiceContext` 的客户端装配与 etcd 发现） |
| `internal/handler` / `internal/types` | 0 | — | **无离线单测**（goctl 生成壳，见 §5） |

### 4. 替身层与断言口径

- 打桩方式统一是**内嵌 go-zero 生成的 client 接口 + 只覆盖本用例需要的方法**，
  断言对象是「请求装配」和「回复投影」两件事；**不建 gRPC 连接、不碰数据库**。
  未被子用例实现的方法继承接口，一旦被调用即 panic，用来暴露意外的下游调用。
- 因此这些用例**证明不了**：下游服务是否真的接受该请求、下游字段的取值是否合法、
  状态机能否迁移、金额与配额判定、幂等命中与否——那些都是领域服务的结论（AGENTS.md §5）。
- 断言口径：投影类断逐字段等值（含 `repeated`/map 展平后的顺序）；拒绝类断「零次下游调用」；
  隐私类断「口令/内部字段不在响应结构里」；未配置客户端时断显式「service not configured」，
  而不是伪造空数据。

### 5. 覆盖边界（不可省略）

- **网关用例永远不等于下游领域服务的用例**。本目录的 117 条 logic 用例只覆盖「HTTP↔RPC 的装配与投影」，
  每条都跑在 client 接口替身上；同一条业务判定的真实结论只存在于
  `services/<domain>` 自己的测试里（如 `services/account`、`services/membership`）。
  把这里看到的数字当成端到端验证是错的。
- 未覆盖的 123 个构造器（§1）目前既没有装配断言也没有投影断言，属于**真实缺口**，不是「已被间接覆盖」。
- 用例不连接 MySQL、Redis、etcd、MQ、Elasticsearch 或对象存储；本服务不拥有数据、
  没有 `deploy/migrations` 下的迁移，因此不存在「迁移 SQL ↔ 真实库列级对账」这一层验证——
  该结论只说明本网关无库可验，**不等于**下游服务已复验。
- `internal/handler`、`internal/types`、`*.pb.go`、`internal/svc` 等 goctl/protoc 生成壳不在单测范围；
  路由与 `.api` 的一致性由 `scripts/gen-api-docs.mjs` 的生成文档与 `gateway/admin` 侧的
  `routes.go` 漂移门禁承担（本服务无对应门禁）。
- 端到端（真实 HTTP 请求 → 真实下游 RPC → 播放地址可用）从未在本目录内验证过。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./gateway/app/...   # -p 1 必须带：Windows 页面文件限制，并发跑多个测试包会 OOM(errno=1455)
gofmt -l gateway/app                      # 必须为空
go vet ./gateway/app/...                  # 应无输出
```

契约变更后按「生成与运行」节的 `goctl api go` + `node scripts/gen-api-docs.mjs` 重新生成，
禁止手改 `internal/handler`、`internal/types`。本节只声明覆盖范围与口径，不含任何一次运行的结论。
