# API、RPC 与事件契约

## 1. API 分层

### 1.1 统一响应信封

所有 HTTP 成功响应和业务错误响应都必须包含以下字段：

| 字段 | 类型 | 规则 |
|---|---|---|
| `code` | integer | `0` 表示成功；非零为稳定业务错误码，不能用 HTTP 状态码替代 |
| `message` | string | 成功固定 `ok`；错误消息可读、脱敏，不暴露 SQL、Token、堆栈或供应商密钥 |
| `data` | object | 成功数据对象；无数据必须为 `{}`，不能省略或返回 `null` |
| `ttl` | integer | 客户端缓存秒数；默认 `0` 表示不缓存，不能把权限/播放签名响应设置成长 TTL |

示例：

```json
{"code":0,"message":"ok","data":{"status":"ok"},"ttl":0}
```

`.api` 中定义具体的 `*Data` 和 `*Response` 类型，再使用 goctl 生成 types/handler；错误由 `common/httpresponse` 注册到 go-zero `httpx`，禁止在 handler 中拼接不一致的 JSON。RPC 不使用此 HTTP 信封，仍采用 protobuf 返回值和 gRPC status。

- `gateway/app` 按领域分组暴露公网路由（`/api/healthz`、`/account`、`/x/member`、`/x/passport-login`、
  `/upload`、`/video`、`/catalog`、`/social`、`/feed`、`/engagement`、`/playback`、`/danmaku`、`/comment`、
  `/notification`、`/up`、`/moderation`、`/search`、`/inbox`、`/private-message`、`/live`，以及商业化终端面
  `/membership`、`/wallet`、`/order`、`/coin`、`/creator/revenue`），
  负责端版本、鉴权、限流、风控和聚合；完整清单见 [gateway/app/README.md](../gateway/app/README.md)。
  商业化终端面**不含**「给当前用户开通会员」这类路由：会员只能由订单履约或运营授予产生，
  权益读取一律转 `membership` 的真实读结论；`payout_available` 恒为 `false`（出金不在本期范围）。
- `gateway/admin` 全部路由在 `/admin/<domain>/...` 下（`/admin/healthz`、`/admin/account`、`/admin/video`、
  `/admin/catalog`、`/admin/rights`、`/admin/moderation`、`/admin/transcode`、`/admin/asset`、`/admin/danmaku`、
  `/admin/comment`、`/admin/notification`、`/admin/creator`、`/admin/inbox`、`/admin/search`、
  `/admin/risk`、`/admin/operation`、`/admin/ops`、`/admin/audit`、`/admin/cron`、`/admin/collector`、
  `/admin/live`、`/admin/recommend`、`/admin/private-message`、行为分析面 `/admin/spm`、`/admin/feature-store`、
  第三方开放平台面 `/admin/open-platform`，商业化运营面为 `/admin/membership`、
  `/admin/payment`、`/admin/order`、`/admin/coin`、`/admin/creator-revenue`，另有历史前缀 `/x/member`），
  业务逻辑仍在对应领域服务。
  `AdminPermission` 中间件调用 `operation.VerifyAdminPermission` 做后台鉴权，**挂在各领域 `@server (... middleware: AdminPermission)`
  的写路由组上**（当前 166 条路由 / 162 个权限点 / 29 个域，覆盖 29 个前缀；`/admin`、`/admin/asset`、
  `/admin/creator` 三个前缀仍全组免鉴权）；只读列表/详情路由走免中间件组（后台列表页每次刷新都会打一次 RPC，全量挂判定会把 `operation`
  变成读放大瓶颈），其审计留痕由领域服务自己的 `*_event`/`audit_entry_id` 承担。
  例外只有 5 条受保护的 GET：两条「形状是 GET、后果是写」（缓存失效、未读重算）与三条定向个人数据读
  （实名脱敏查询、证件号反查 mid、单用户登录日志），逐条理由见
  `gateway/admin/internal/middleware/route_permission_drift_test.go` 的 `protectedGetAllowlist`。
  中间件的静态权限表对表外路径一律 fail-closed，带 `:param` 的路径按段级模式匹配（不会当前缀放行），
  因此**新增写路由必须同时登记 `routePermissions`**；
  权限点本身需要 `op_permission` seed 迁移落库才能生效，角色与绑定由 `op_role`/`op_role_permission`
  的派生种子建好（每轮 `domain_*` + `readonly` + `super_admin`，当前合计 31 角色 / 340 绑定，
  **不把角色绑给任何账号**），
  见 [gateway/admin/README.md](../gateway/admin/README.md)。
- 路径不带全局 `/v1` 段：终端 API 路径必须与既有客户端一致，版本兼容通过按需新增的显式版本段
  （`/account/v1`、`/account/v2`）和 protobuf package（`*.v1`）表达；不得为了让路径“看起来有版本”而改动已发布路由。
- 服务内部 RPC 使用 protobuf，方法名表达领域动作，例如 `CreateSubmission`、`GetPlaybackToken`，不要暴露数据库 CRUD 作为公共协议。
- 管理接口使用独立权限域和路径，不把管理员字段混入普通客户端响应。
- 客户端支持 Android、iOS、HarmonyOS、桌面端；不创建小程序专用 API。

### 1.2 逐接口清单（全部由契约生成，按域分组）

上面的前缀清单只到「有哪些组」，**逐条路由/方法的权威清单是生成产物**，改接口先改 `.api`/`.proto` 再重新生成，不要手改产物：

| 产物 | 分组口径 | 内容 |
|---|---|---|
| [docs/api/http/app/](api/http/app/README.md) | 一个 `@server` 前缀一个文件（28 个） | 每条路由的方法、路径、鉴权、入参位置、请求/响应类型与类型附录 |
| [docs/api/http/admin/](api/http/admin/README.md) | 同上（32 个） | 额外标注该路由是否挂 `AdminPermission` 及权限点 |
| [docs/api/rpc/](api/rpc/README.md) | 一个服务一个文件（43 个） | 方法表、消息与枚举全量定义、etcd key、端口、网关消费方 |
| [postman/](../postman/README.md) | folder = 前缀分组，sub-folder = `@server` 小节 | 两个集合 + 环境变量，断言只校验四字段信封 |
| [scripts/rpc/](../scripts/rpc/README.md) | 按服务分节 | `grpcurl` 冒烟脚本（**未在本仓执行过**，需真实进程与 etcd） |

入参编码口径（决定请求怎么发，依据是 go-zero `httpx.Parse` 的实现）：`path`→路径段、
`form`→URL 查询串（POST 同样从查询串取）、`json`→JSON 请求体；挂在 `form` 上的结构体数组绑不上，
这类字段必须用 `json`。生成器把这条例做成门禁，违反时直接报错而不是产出误导文档。

## 2. 版本和兼容性

- protobuf package 和事件名带 `v1`；HTTP 路径不强制带版本段（见 §1 的路由说明），需要并行新旧行为时按需新增 `/v1`、`/v2` 组。
  新增字段优先可选/向后兼容，禁止复用已删除字段编号。
- 删除字段前至少经过一个兼容窗口；消费者先兼容新旧字段，生产者再切换，最后清理旧字段。
- API 响应统一包含业务 code、message、data 和 ttl；trace_id 通过响应头或网关扩展字段传递，内部错误不能泄漏 SQL、Token 或供应商密钥。
- 分页使用 cursor 优先；列表接口限制 page size，避免深分页拖垮数据库。

## 3. 通用请求约束

写请求建议包含 `request_id` 或 `idempotency_key`；服务端用业务唯一键和状态版本双重防重。所有远程调用设置 deadline；重试只用于明确幂等的查询或带幂等键的写入。

## 4. 事件 Envelope

```json
{
  "event_id": "01J...",
  "event_type": "content.published",
  "schema_version": 1,
  "occurred_at": "2026-08-24T00:00:00Z",
  "producer": "video",
  "trace_id": "...",
  "aggregate_type": "submission",
  "aggregate_id": "...",
  "payload": {}
}
```

必填字段用于去重、追踪、路由和版本判断。事件 payload 不放完整身份证、手机号、Token、原始 IP 等敏感数据；需要关联时使用受控 ID。

## 5. 核心事件

| 事件 | 生产者 | 主要消费者 | 说明 |
|---|---|---|---|
| `media.task.v1` | upload（发布器已接线，`-tags upload_kafka`）；asset（无发布器） | transcode/fingerprint（**两侧都没有消费者实现**） | 转码、截图、字幕、指纹任务 |
| `content.published.v1` | video（发布器已接线，`-tags video_kafka`；`video_outbox` 迁移待执行）；catalog/rights（无发布器） | search-indexer（`-tags searchindexer_kafka`）、inbox（`-tags inbox_kafka`）；recommend/spm（**无消费者实现**） | 发布、下架、过期 |
| `engagement.action.v1` | engagement/social-graph（**全仓无发布点**） | inbox（`-tags inbox_kafka`）与 search-indexer（`-tags searchindexer_kafka`）都订阅了该 topic 但收不到；event-collector/spm/recommend 无消费者实现 | 点赞、收藏、关注、分享 |
| `moderation.result.v1` | moderation（**发布点仍是 `// TODO(event)`**） | video/catalog/comment/danmaku（**都没有消费者实现**） | 机审、人审、申诉结论 |
| `playback.heartbeat.v1` | playback（发布器已接线，`-tags playback_kafka`） | event-collector/spm（**两侧都还没有消费者实现**） | 播放进度和质量指标 |
| `live.state.v1` | live-ingest（发布器已接线，`-tags liveingest_kafka`） | live-room/inbox/live-media（已接线）；live-gateway（未订阅） | 流状态迁移事实：IDLE/PUBLISHING/INTERRUPTED/STOPPED |
| `notification.request.v1` | 各领域服务（**全仓无发布点**） | notification（`-tags notification_kafka`，消费实现完整） | 模板化通知请求 |
| `commerce.order.v1`（目标契约，未接线） | trade-order | membership/payment/inbox | 订单状态迁移与履约结果；本期由 `trade-order` 同步 RPC 编排，无 outbox 表 |
| `commerce.entitlement.v1`（目标契约，未接线） | membership | inbox/search/recommend | 会员权益授予/收回；当前只有 `mb_grant` 行可读，无事件发布器 |
| `coin.tossed.v1`（目标契约，未接线） | coin | spm/creator-revenue | 投币事实，是分成计量的候选来源；当前由 `RecordRevenueMetric` 同步上报 |
| `user.profile.updated.v1` | user-profile | account（`DelCache`，消费者未落地） | 资料/昵称/头像等变更，同事务写 `member_outbox` |
| `user.moral.notice.v1` | user-profile | notification（未接入） | 节操值跨过阈值的通知请求 |
| `livemedia.*.v1`（9 个，逐个列在下段） | live-media（发布器已接线，`-tags livemedia_kafka`） | 暂无订阅方 | 直播转码/录制/分发档位/回放/回收的事实事件 |
| `recall.pool.published.v1` | recommend-recall（发布器已接线，`-tags recommendrecall_kafka`） | 暂无订阅方 | 池版本上线/回滚的切换事实（`recall_outbox` 同事务写入） |

SPM 只消费用户行为和内容事件以服务视频推荐，不产生广告相关事件。

生产者按「一个事件只有一个写入者」登记（AGENTS.md §5）：`live.state.v1` 的唯一生产者是 `live-ingest`
（`live_ingest_outbox` → `PublishTopics`），`live-room` 只经 `ReportStreamState` 消费它并按 `event_id`
去重、按 `seq` 挡住乱序，不重复发布同一 topic。

**`live.state.v1` 的两端现在都在代码里**（2026-10-04）：

- 出站：`services/live-ingest/internal/publisher`，`svc.startPublisher()` 按 `Kafka.Enabled` 启动，
  发送端用 `kq.NewPusher(..., kq.WithSyncPush())` 同步投递，只有 broker 受理才 `MarkPublished`。
  代码在 `-tags liveingest_kafka` 后面，默认构建的 `NewSender` 恒返回 `ErrKafkaRuntimeNotBuilt`。
- 入站：`services/live-room/internal/consumer` 订阅 `live.state.v1`，把信封翻译成
  `ReportStreamStateReq` 后交给与 RPC 路径同一段 logic（因此入站与 RPC 调用共享同一套去重与乱序守卫）。
  代码在 `-tags liveroom_kafka` 后面。
- 另一个入站：`services/inbox/internal/consumer`（`-tags inbox_kafka`）按 `stream_state` 推导动作，
  3 → 断流提醒、4 → 停播提醒，1/2 不发信（开播面向粉丝，inbox 没有粉丝关系）。
- 第三个入站：`services/live-media/internal/consumer`（`-tags livemedia_kafka`，与服务自己的发布器
  共用同一个标签和同一个 `Kafka.Enabled`）只认 `stream_state=4`（Stopped），翻译成
  `logic.OfflineSessionOutputs` 把**该场次的全部在线档位**整场下线并逐档位登记
  `livemedia.stream.output.offline`；1/2/3 一律不动作（中断可在宽限期重连，此时摘全房间档位会把
  一次网络抖动变成观众可见的整场停播）。缺 `session_id` 的 Stopped 事件按契约违反拒收，
  因为它只能靠 `live_session_id` 界定范围，按房间下线会误伤刚开播的场次（`mapping.go` 有完整口径）。

payload 字段以 `live-ingest` README 的表为准：`stream_id`、`room_id`、**`anchor_mid`**、`session_id`、
`stream_state`、`stream_seq`、`occurred_at`、`interrupted_seconds`、`reason`、`trace_id`，
`event_id` 在信封上。`anchor_mid` 是给 inbox 的路由事实（主播本人要收通知，而主播 ID 只在 `live_stream` 行里）；
live-room 侧解码进结构体但不转发，`ReportStreamStateReq` 里没有这个字段（房间→主播的绑定它自己就有），
live-media 侧解码后也完全不用它（下线按 `room_id + live_session_id` 定位，与主播身份无关）。

**`playback.heartbeat.v1` 只有出站**（2026-10-04 接线）：`services/playback/internal/publisher` 把
`playback_outbox` 的待发行投递到该 topic，发布循环复用 `common/outbox` 引擎（退避、判死、统计与
逐行状态推进都在引擎里），`svc.startPublisher()` 按 `Kafka.Enabled` 启动，代码在 `-tags playback_kafka`
后面。分区键是行的 `aggregate_id`（即 `session_id`，`outbox_store.go:105`），`PublishTopics` 只登记
`playback.heartbeat.v1` 一个（`RequiredTopic()` 从 model 常量推导，`outbox_store.go:40`，
其他 topic 的行会被判死而不是误发）。**入站没有任何实现**：`event-collector` 与
`spm` 都没有 `internal/consumer` 目录，`deploy` 里也没有建 topic 的脚本，所以这条链路目前是
「有生产者、无消费者」，播放量和完播率的正式口径仍按 playback README 缺口 6 处理。

**`livemedia.*.v1` 九个 topic 只有出站**（2026-10-04 接线）：`services/live-media/internal/publisher`
把 `live_media_outbox` 的待发行投递到这 9 个 topic，发布循环同样复用 `common/outbox` 引擎，
`svc.startPublisher()` 按 `Kafka.Enabled` 启动，代码在 `-tags livemedia_kafka` 后面。
与 playback 的三点差异都要按本服务的口径读：

- topic 集合由 model 的 9 个 `EventType*` 常量派生（`RequiredTopics()`，`outbox_store.go:62`），
  `ValidatePublishKafka` 要求 `Kafka.PublishTopics` 与它**集合相等**：少一条，那类事件的每一行
  都会撞「没有发送通道」退避到判死；多一条，白占通道并让人误判「这个 topic 有人在发」。
  两侧漂移在启动时被拒绝，运行期 `Send` 仍拒发未登记 topic（两道独立的闸）。
- 分区键是行的 `aggregate_id`（任务号，`outbox_store.go:164`），不是 `event_id`：
  同一任务的顺序事件必须同分区，否则下游看到的迁移次序不可控。
- `ListPending` 只取 `state=0`（待发布），这是本轮修掉的 live-media 缺陷：该表的 `MarkRetry` 把行留在
  待发布态、`MarkFailed` 才置 `state=2`（判死），而原取行 SQL 写的是 `state IN (0, 2)`，
  等于每轮都把死信重新捞出来投，判死形同虚设。取行集合与状态编号必须逐表核对：
  `recommend-recall` 的 `recall_outbox` 当时同样是 `state IN (?, ?)`，**2026-10-05 接线时一并收口**
  （现在也是 `state = ?`，`model/outbox.go:118`），四张事件 outbox 表的取行条件已一致。

**这 9 个出站 topic 的入站仍然是零**：本仓库没有任何服务订阅它们，`docs/roadmap.md` 的 MQ 接线项也未把
它们列给某个订阅方；回放拼接、回收计数、档位下线的下游投影因此不会发生
（live-media 新接的读者只订阅入站的 `live.state.v1`，见上面「第三个入站」那条，不在这 9 条之内）。
投递语义同样从未在真实 broker 上验证过；发布侧还有一个依赖级限制：`kq.NewPusher` 在 go-queue v1.2.2
不暴露 SASL/TLS 注入口，所以带鉴权的集群发布链路对接不了（消费侧的 `Username`/`Password`/`CaFile`
三键只作用于 `kq.NewQueue`，见 live-media README §5 与已知缺口 1、3）。

**`recall.pool.published.v1` 也只有出站**（2026-10-05 接线）：`services/recommend-recall/internal/publisher`
把 `recall_outbox` 的到期待发行投到这一个 topic，发布循环同样复用 `common/outbox` 引擎，
`svc.startPublisher()` 按 `Kafka.Enabled` 启动，代码在 `-tags recommendrecall_kafka` 后面。
本服务的三条口径与他处不同，接消费方时要先读：

- 一条事件对应**一次池版本切换**（`PublishPoolVersion` 与 `RollbackPoolVersion` 共用
  `internal/logic/poolswitch.go:239 writePublishedEvent`，靠 payload 的 `rollback` 区分），
  正文是 `(source, pool_key, version, previous_version, batch_id, item_count, generator, operator, published_at)`，
  不含 mid/IP/设备号；`pool_key` 的 `mid:<mid>` 形态在落库前按 logic 的脱敏口径处理。
- 分区键是 `aggregate_id`，形如 `source:pool_key:version`（`aggregateIDOf`，`poolswitch.go:308`）。
  它与 live-media 的「任务号」语义不同：**每次切换的键都不同**，所以同一池的事件不保证同分区，
  只保证同一版本行的相关事件相邻；下游要按 `recall_pool_current` 指针而不是分区顺序判新旧。
- 发布时间写在这行的 `mtime` 上，表里没有独立的 `published_at` 列；清理条件因此是
  `state=已发布 AND occurred_at < ?`（`DeleteSentBefore`，`model/outbox.go:233`），
  而该查询与 `CountPending`/`CountStuck` 目前**没有生产调用者**，已发布行不会自动收敛。

`recommend-recall` 不订阅任何 topic（`etc` 没有 `SubscribeTopics`/`Group`，服务里没有 `internal/consumer`），
所以 `recall.pool.published.v1` 与 `playback.heartbeat.v1`、9 个 `livemedia.*.v1` 同属「有生产者、零消费者」：
排序特征、缓存失效等下游投影仍然只能靠 RPC 轮询，不能按"推荐已收到切换通知"对待。

**`media.task.v1` 同样只有出站**（2026-10-05 接线）：`services/upload/internal/publisher` 把
`upload_outbox` 的到期待发行投到这一个 topic，发布循环复用 `common/outbox` 引擎，
`svc.startPublisher()` 按 `Kafka.Enabled` 启动，代码在 `-tags upload_kafka` 后面。三条只有本服务才有的口径：

- 事件与状态推进**在同一个事务里**：`CompleteUpload` 的 `TransactCtx` 一次写入
  「回填 md5、回填 asset_id、状态转 `COMPLETED`、事件行」（`internal/repository/repository.go:288`~`:300`），
  缓存刷写在提交之后。因此「会话已 COMPLETED 但 `upload_outbox` 没有行」在正常路径下不可达；
  反过来，`upload_outbox` 表不存在时**整条完成路径失败**（见下一条），不是「状态照改、事件丢失」。
- 该表是 `upload` 目录的第 3 条迁移（本轮从 2 条增至 3 条）：
  `deploy/migrations/upload/000003_create_upload_outbox.sql`，**仍未在任何实例执行过**。
  上线顺序因此是硬约束：先 `up` 迁移，再发带发布器的代码；
  回滚要先停发布器再回滚代码，否则新代码写不进事件行（`docs/roadmap.md` MQ 段与本服务 README「数据与迁移」同一口径）。
- 分区键取 `aggregate_id`，即 `upload_id`（`internal/repository/mediatask.go:79` 的聚合类型是
  `upload_session`），于是同一次上传的媒资派生事件必然同分区、按 `id` 升序。
  载荷是 `media.task.v1` 的 v1 契约（字段只增不改，删改要递增 `schema_version`）：
  `upload_id`、`mid`、`filename`、`size`、`typeid`、`md5`、`bucket`、`object_key`、`asset_id`、
  `chunk_size`、`total_chunks`、`completed_at`；**不含预签名 URL、任何 AK/SK 与调用方 IP**
  （AGENTS.md §6，钉在 `TestCompleteUploadEventPayloadCarriesOnlyMediaFacts`）。
  两点下游必须先知道：`filename` 是用户可控字符串且会原样进事件；`size`/`chunk_size`/`total_chunks`
  三者关系在本服务侧不校验（README 缺口 14），所以拿到的可能是不自洽的数，媒资探测要以对象存储实测为准。

入站是零：`asset`、`transcode`、`content-fingerprint` 三个服务目录都在，但都没有 `internal/consumer`
（`content-fingerprint` 只在 `internal/logic/submittasklogic.go:29` 留了一条 `TODO(后续)`，
等着被这个 topic 唤醒）。`deploy/` 里也没有建 topic 的脚本。所以「上传完成 → 转码任务开始」这条链
目前**只走到落库**，不能按“媒资链路已打通”对待。

**`content.published.v1` 两端现在都在代码里**（2026-10-05 接生产侧）：`services/video/internal/publisher`
把 `video_outbox` 的到期待发行投到这一个 topic，发布循环复用 `common/outbox` 引擎，
`svc.startPublisher()` 按 `Kafka.Enabled` 启动（失败即 `logx.Must` 崩，静默不投等于新稿件永远进不了索引），
代码在 `-tags video_kafka` 后面。四条只有本服务才有的口径，接消费方与运维都要先读：

- **一条事件对应一次「改变对外可见性」的状态转换**，判定单点在
  `services/video/internal/repository/contentevent.go:64 contentActionFor`：
  `→PUBLISHED` 产 `publish`，`→OFFLINE` 产 `offline`，`→EXPIRED` 产 `expired`，
  `→DELETED` 只在来源态属于 `{PUBLISHED, OFFLINE, EXPIRED}` 时产 `delete`（草稿、上传中、被驳回的稿件
  从未进过索引，给作者发「你的作品已被删除」既失真也没意义）。其余转换一行都不写。
  `update` 这个 action 在 video 侧**永远不会产出**：`UpdateSubmission` 只允许 DRAFT 态改正文
  （AGENTS.md §8 的「已发布稿件不许原地改」），所以消费方收到的 `update` 只能来自 catalog/rights
  的发布器，而它们目前都没有发布器。
- 事件行与状态、审计**在同一个 `TransactCtx` 里提交**（`internal/repository/repository.go:184`），
  `DeleteSubmission` 与 `TransitionState` 复用同一条路径。因此 `deploy/migrations/video/000004_create_video_outbox.sql`
  未执行时是**发布与下架转换整笔回滚**（状态与审计一起退写），不是「状态照改、事件丢失」。
  该迁移至今 `pending`（见 `deploy/migrations/README.md`），上线顺序必须是先迁移再发代码。
- payload 只放 video 拥有的 13 个键（`action`、`content_id`、`content_type` 恒 1、`title`、`description`、
  `cover_url`、`author_mid`、`typeid`、`tags`、`ctime`、`doc_revision`、`publish_at`、`reason`）：
  时长、作者昵称、分区名、版权窗口、热度与敏感标记分属 asset/user-profile/catalog/rights/spm/moderation，
  本事件一律不代答（AGENTS.md §5），消费侧对应字段解成零值。`doc_revision` 与同批
  `video_audit_log.ctime` 同源（秒 × 1000），`publish_at` 只在 `publish` 上赋值，
  `tags` 恒为数组（无标签是 `[]` 而不是 `null`，`null` 会被索引读成「不更新标签」）。
- 分区键是 `aggregate_id` = `aid` 的十进制字符串，所以同一稿件的上下线事件必然同分区、按 `id` 升序生效；
  换成 `event_id` 就等于允许「已发布」覆盖「已下架」。

入站两侧都有实现：`search-indexer`（`-tags searchindexer_kafka`）按 `action` 写/撤索引文档，
`inbox`（`-tags inbox_kafka`）给作者发下架/过期/删除站内信。两处口径与生产侧的已知落差：
`search-indexer` 要求 `doc_revision` 非零（`internal/consumer/mapping.go:168` 缺失即判契约违反），
本服务总是带上；但它同时读 `heat`/`heat_revision`，而 video 不产出热度事实，
所以发布事件建起来的索引文档热度全零，要等 `engagement.action.v1` 的生产者（尚未接线，见下）打补丁。
`esclient.ShouldOverwrite` 用的是 `>=`（`internal/esclient/document.go:128`），
而本服务的 `doc_revision` 只有秒级精度（同秒内的两次转换会拿到相同版本），
因此「同秒上下线」时索引可能接受后到的那条，顺序结论靠分区保序而不是版本号，见 video README 缺口 14。
**两端从未与真实 broker 联调**，所以这条链路的证据级仍是「编译/静态检查/离线单测」。

上表其余事件仍是目标契约。**双端都在代码里的链路目前有两条**：`live.state.v1` 与 `content.published.v1`
（后者的生产侧 2026-10-05 接线，且 `video_outbox` 迁移待执行）。已注册的消费者共五个：
`inbox`（`internal/consumer/mapping.go` 订阅 `engagement.action.v1`、`content.published.v1`、`live.state.v1`）、
`search-indexer`（`content.published.v1` 投影）、`notification`（`notification.request.v1`）、
`live-media`（`live.state.v1` → 本场次档位整场下线，2026-10-04）、
`live-room`（`live.state.v1` → `ReportStreamState`，2026-10-04）；
`moderation-orchestrator` 的 `moderation.result.v1` 仍是 `// TODO(event)`（`submitworkerresultlogic.go:49`），
没有生产者，因此 live-room 刻意不订阅该 topic（订阅一个没人发、且没有死信表的 topic 只会让排障更绕）。
`event-collector` → `spm` → `recommend-*` 的行为事件链路尚未定 topic 名与
schema 归属（`event-collector`/`spm` 的 etc 里没有 `SubscribeTopics`/`PublishTopics`），
`live-gateway` 也还没有消费者；这些都属于阶段 3-4 待办，不能按“事件已打通”对待。

消费/生产循环一共十一条，藏在十个构建标签后面（消费者 `-tags inbox_kafka` / `-tags notification_kafka` /
`-tags searchindexer_kafka` / `-tags liveroom_kafka`，生产者 `-tags liveingest_kafka` /
`-tags playback_kafka` / `-tags livemedia_kafka` / `-tags recommendrecall_kafka` / `-tags upload_kafka` /
`-tags video_kafka`）：
`livemedia_kafka` 同时覆盖 live-media 的发布器与消费者两侧（`internal/publisher/kafkaruntime_*.go` 与
`internal/consumer/kafkaruntime_*.go` 用同一个标签、同一个 `Kafka.Enabled` 开关），所以标签数比循环数少一，
不能按「一个标签只有一条链路」来数。
默认构建启动时明确写「未链接运行时」而不是静默空转；
示例配置的开关都是 `Enabled: false`。所以**「已接线」只到「编译/静态检查/单测」这一级**，
本仓库从未与任何 broker 联调过，命令与逐服务口径见 `docs/commands.md` 与各服务 README「已知缺口」。

**Topic 由 `common/eventenvelope.Topic(event_type, schema_version)` 逐事件推导**（`<event_type>.v<N>`），
不存在「一个服务一个 topic」的聚合写法；服务 `etc` 的 `PublishTopics` 必须逐个列出本服务可能写出的
topic，否则订阅方无法按 topic 授权。`live-media` 自有事件共 9 个：
`livemedia.transcode.state.changed`、`livemedia.record.state.changed`、`livemedia.record.stopped`、
`livemedia.record.gap.detected`、`livemedia.stream.output.online`、`livemedia.stream.output.offline`、
`livemedia.replay.review.submitted`、`livemedia.replay.content.state.changed`、
`livemedia.retention.finished`（各自 `.v1`）。

`event_type` 的语法（小写字母/数字/点号，不得以点号开头结尾或连写）由 `eventenvelope.New()` 强制，
违规名会让**每一次 Outbox 写入连同业务事务一起回滚**；因此 `common/eventenvelope/event_type_gate_test.go`
扫描全仓 `Event*` 常量做门禁，并校验 `/internal/consumer/` 下出现的名字都有对应生产者。新增事件名必须
同时在本节登记，不能只在服务里定义。

## 6. Outbox 与消费状态

领域事务同时写业务表和 `outbox_event`；发布器成功后记录发送时间和次数。消费者维护 `consumer_offset` 或业务处理表，状态至少包含 `received/processing/succeeded/retry/dead_letter`。任何重复、乱序或迟到消息都不能破坏最终状态。
