# gateway/admin

面向管理后台 Web 的入口聚合网关。属于 [gateway](..) 的独立子服务之一。

## 职责

- 管理后台 HTTP 入口，当前 313 条路由分布在 32 个前缀分组下（`/admin/*` 各运营域 + `/x/member`）。逐前缀清单见[接口文档索引](../../docs/api/http/admin/README.md)。
- 管理员账号鉴权 + RBAC、IP 白名单、限流、操作审计、trace_id。
- 面向后台的响应聚合；不拥有用户、视频、评论等领域主数据。

## 边界

- **仅做入口聚合**（路由、鉴权、限流、响应聚合）；管理后台的业务逻辑、RBAC 规则、运营配置、审计落库全部由 [services/operation](../../services/operation) 等领域服务承担。
- `gateway/admin` 不直接读写业务数据库，不拥有领域主数据。
- 可以调用领域 RPC（尤其是 operation），不能拼接数据库 model。
- 下游错误映射为稳定的公共错误码（见 [common/httpresponse](../../common/httpresponse)），不泄露 SQL、Token、对象存储签名。
- HTTP 响应统一使用四字段信封（`code`/`message`/`data`/`ttl`）。
- 仅内网/VPN 访问，不对公网暴露。

## 路由

| 前缀 | 说明 | 下游 |
|---|---|---|
| `/admin` | 健康检查 `GET /admin/healthz` | - |
| `/admin/account` | 缓存运营：`GET /cache/del`（mid/modifiedAttr）、`POST /cache/clear`（JSON 消息） | account `DelCache` RPC |
| `/x/member` | 用户运营：`POST /morals/update`、`POST /moral/update`、`POST /moral/undo`、`POST /exp/set`、`POST /exp/update`、`POST /property/review/add`、`GET /realname/stripped/info`、`GET /realname/mid/by/card`、`GET /web/login/log`（登录日志） | user-profile / account gRPC |
| `/admin/video` | 稿件运营：3 条 | video gRPC |
| `/admin/catalog` | 版权目录运营：6 条 | catalog gRPC |
| `/admin/rights` | 版权窗口运营：5 条 | rights gRPC |
| `/admin/moderation` | 审核运营：4 条 | moderation-orchestrator gRPC |
| `/admin/transcode` | 转码运营：4 条 | transcode gRPC |
| `/admin/asset` | 媒资运营：2 条 | asset gRPC |
| `/admin/danmaku` | 屏蔽词运营：`POST /block_word`（ADD/UPDATE/DISABLE/DELETE）、`GET /block_words`（分页）。用户级屏蔽只在 `gateway/app` 暴露，后台不重复开面 | danmaku `BlockWord` / `ListBlockWords` |
| `/admin/search` | 索引运营：`POST /rebuild`、`GET /rebuild/:task_id`、`GET /rebuilds`、`POST /alias/switch`、`GET /health` | search-indexer 全部 5 个运营 RPC |
| `/admin/risk` | 风控运营与裁决调试：`POST /check`、`POST /report`、`GET /device/:device_id`、`POST /device`、`POST /punishment`、`POST /punishment/lift`、`GET /punishments`、`POST /rule`、`GET /rules`、`POST /list_entry`、`GET /list_entries` | risk-control 全部 11 个 RPC |
| `/admin/operation` | 后台自身运营面（22 个 RPC 全部 POST + JSON body）：`POST /login`、`POST /permission/verify`（这两个免鉴权），以及挂 `AdminPermission` 的 `/user/create`、`/user/update`、`/user/disable`、`/user/list`、`/role/assign`、`/role/create`、`/role/list`、`/role/delete`、`/permission/list`、`/permission/create`、`/menu/get`、`/menu/save`、`/config/get`、`/config/save`、`/task/submit`、`/task/get`、`/task/list`、`/task/cancel`、`/task/run`、`/audit/list` | operation 全部 22 个 RPC |
| `/admin/comment` | 评论运营：`GET /list`、`GET /replies`、`POST /delete`（`admin=true`）、`POST /pin`、`GET /stats` | comment gRPC |
| `/admin/notification` | 通知模板与投递：`POST /template/upsert`、`GET /template/list`、`POST /template/publish`、`POST /template/render`、`GET /delivery/status`、`GET /delivery/list`、`GET /deadletter/list`、`POST /deadletter/retry` | notification gRPC |
| `/admin/creator` | 创作者运营：`GET /groups`（分组字典）、`GET /group/mids`（分组名册）、`GET /high-ally-ups`（高能联盟签约） | creator gRPC |
| `/admin/inbox` | 站内信运营：`POST /message/send`（下发系统站内信，`idempotency_key` 必填）、`GET /unread/recompute`（单人未读快照修复）。**不提供代读/代改已读/代删**：收件箱正文属用户隐私 | inbox gRPC |
| `/admin/audit` | 审计台账（全部 POST + JSON body）：只读免中间件的 `/entry/list`、`/entry/get`、`/chain/verify`、`/export/list`、`/export/get`、`/retention/list`、`/archive/list` + 写入口 `/export/create`、`/export/run`、`/retention/save`、`/archive/run`（挂 `AdminPermission`）。链校验由 audit 服务的 `VerifyChain` 承担，网关不重算哈希 | audit gRPC |
| `/admin/ops` | 运营配置（全部 POST + JSON body）：只读免中间件的 `/config/list`、`/config/versions`、`/rollout/list`、`/topic/list`、`/topic/get`、`/slot/list`、`/switch/list` + 写入口 `/config/publish`、`/config/rollback`、`/rollout/save`、`/rollout/state`、`/topic/save`、`/topic/items/save`、`/slot/save`、`/slot/items/save`、`/switch/save`、`/cache/refresh`（挂 `AdminPermission`）。发布与回滚分开授权 | ops-config gRPC |
| `/admin/cron` | 定时任务（全部 POST + JSON body）：只读免中间件的 `/task/list`、`/task/get`、`/run/list`、`/run/get`、`/checkpoint/list`、`/checkpoint/get`、`/lease/list`、`/lease/get`、`/audit/list`、`/health/get` + 写入口 `/task/register`、`/task/update`、`/task/pause`、`/task/resume`、`/task/disable`、`/task/trigger`、`/run/retry`、`/checkpoint/save`（挂 `AdminPermission`）。worker 控制面（`ListDueTasks`/`AcquireLease`/`RenewLease`/`ReleaseLease`/`ReportTaskResult`）**刻意不开 HTTP** | cron gRPC |
| `/admin/live` | 直播运营（只到 live-room 域）：只读免中间件的 `/room`、`/room/list`、`/room/bans`、`/session`、`/session/list`、`/area/list`、`/anchor/list` + 写入口 `/room/close`、`/room/ban`、`/room/ban/lift`、`/setting/update`、`/area/upsert`（挂 `AdminPermission`）。**下架与封禁分开授权**（`live:room:close` / `live:ban:create`），封禁与解封也分开（`live:ban:lift`）。`/room/close` 的 `admin` 位由网关固定写 `true`，不接受客户端声明；观众侧/主播侧的开播流程（`CreateRoom`/`PrepareLive`/`StartLive`/`EndLive`/`UpdateRoomInfo`/`MutateAnchor`）与内部事件入口（`ReportStreamState`/`ApplyRoomModerationResult`）**不在 `/admin` 暴露**：前者是 anchor 自有的用户动作、契约里没有管理员身份，后者是服务间回调，开 HTTP 面等于给状态机开后门。媒体/流/网关域（live-media / live-ingest / live-gateway）本轮不接入 | live-room gRPC |
| `/admin/recommend` | 推荐控制面（recall 5 读 4 写 + rank 3 读 5 写，写入口全部 POST + JSON body）：只读免中间件的 `GET /pool/snapshot`、`GET /pool/version/list`、`GET /pool/config`、`GET /recall/log`、`POST /recall/log/list`、`GET /rank/decision`、`POST /rank/decision/list`、`GET /rank/runtime-config` + 写入口 `POST /pool/item/upsert`、`/pool/version/publish`、`/pool/version/rollback`、`/pool/version/prune`、`/rank/model/upsert`、`/rank/model/state`、`/rank/feature-config/upsert`、`/rank/experiment/upsert`、`/rank/experiment/state`（挂 `AdminPermission`）。**切换生效版本与回滚、清理分开授权**（`recommend:pool` 的 `publish`/`rollback`/`prune`），**登记与生效也分开**（`recommend:model`、`recommend:experiment` 各自的 `create`/`state`，特征登记单点 `recommend:feature:create`）。两个契约里都没有「把某个 aid 顶到前面 / 加权 / 屏蔽」的入参，网关也不发明这个面：运营能动的只有池数据与模型/特征/实验的登记与切换 | recommend-recall / recommend-rank gRPC |
| `/admin/collector` | 行为事件采集与投递台账（SPM 语义只到「用户行为分析」，AGENTS.md §7）：只读免中间件的 `GET /health`、`POST /schema/validate`、`GET /batch`、`POST /batch/list`、`GET /event`、`POST /event/list`、`POST /dead-letter/list`、`GET /policy/active`、`GET /policy/list` + 写入口 `POST /delivery/retry`、`POST /dead-letter/replay`、`POST /policy/upsert`、`POST /policy/activate`（挂 `AdminPermission`）。台账只出现 `device_hash`/`ip_segment`/`salt_version`/`salt_ref`/`payload_digest`，明文设备号与 IP 不出面 | event-collector gRPC |
| `/admin/private-message` | 私信运营面：只读免中间件的 `POST /report/list`（举报台账游标翻页，读取主体由 `operator_mid` 承载）+ 写入口 `POST /report/handle`（处置：驳回/撤回/转处罚/升级人审）、`POST /retention/purge`（留存到期物理清理，`dry_run` 先看影响面）挂 `AdminPermission`。台账里只有 `reason` 原因码与 `description`/`handle_note` 文本，**私信正文不经过本组路由**；`ApplyModerationVerdict` 刻意不开 HTTP 入口 | private-message gRPC |
| `/admin/spm` | 行为分析（SPM）运营面（全部 POST + JSON body）：只读免中间件的 `/metric/get`、`/metric/batch-get`、`/hot-subject/list`、`/retention/get`、`/definition/get`、`/definition/list`、`/job/get`、`/job/list`、`/consumer-state/list`、`/dead-letter/list` + 挂 `AdminPermission` 的 `/interest/get`（**唯一挂判定的读口**：定向读某一个 mid 的画像）、`/definition/upsert`（登记新口径版本）、`/definition/state`（上下架）、`/job/submit`（排队聚合作业）、`/metric/recompute`（按显式版本重算历史窗口）。**后台不生产指标、不改推荐结果、不改已登记口径**：`WriteMetricWindow` 是计算链路专属写回通道，刻意不开 HTTP（AGENTS.md §7 第 3 条）；画像只回受控词表，`interest_key`/`payload_preview` 不进网关日志 | spm gRPC |
| `/admin/open-platform` | 第三方开放应用与授权运营面（全部 POST + JSON body）：只读免中间件的 `/scope/list`（scope 目录，契约里没有任何操作者位）+ 挂 `AdminPermission` 的 `/application/get`、`/application/list`、`/quota/policy/list`、`/quota/usage/list`、`/webhook/list`、`/webhook/delivery/list`（**本域读面全部挂判定**：读的是「谁在调用我们、能调多少、事件推给了谁」）与 `/application/state`（推进应用状态机）、`/secret/rotate`、`/secret/revoke`、`/scope/grant`、`/authorization/revoke`、`/quota/policy/upsert`、`/quota/recompute`、`/webhook/delete`、`/webhook/delivery/retry`。**后台不代替归属者表达意愿、不签发也不换发用户凭证、不注入事实**：`RegisterApplication`/`RegisterWebhook`/`EnqueueWebhookEvent` 与 OAuth 五法（`IssueAuthorizationCode`/`ExchangeAuthorizationCode`/`RefreshAccessToken`/`IntrospectToken`/`AuthorizeRequest`）刻意不开 HTTP，共 8 个方法（理由见下方段落到 admin.api 段头注释）。`client_secret` 只在轮换响应里出现一次、`token_hint` 只在请求内使用，二者与回调 `url`、`redirect_uris`、`reason` 正文**永不进网关日志** | open-platform gRPC |
| `/admin/feature-store` | 在线特征的定义/版本/回填运营面（全部 POST + JSON body）：只读免中间件的 `/definition/get`、`/definition/list`、`/version-switch/list`、`/backfill/get`、`/backfill/list` + 挂 `AdminPermission` 的 `/entity-feature/list`（**本组唯一挂判定的读口**：定向读某一个主体的全部特征值）、`/definition/register`（登记新特征版本，服务强制 DRAFT）、`/definition/state`（DRAFT/ACTIVE/RETIRED）、`/definition/privacy`（隐私级别单独入口）、`/version/switch`（移动 ACTIVE 版本指针）、`/backfill/submit`（补历史值）、`/entity-feature/erase`（按主体擦除）、`/retention/purge`（手动补跑一轮 TTL 清理）。**后台不写特征值、不碰在线热路径**：`WriteFeatures` 是计算链路专属写回通道，`GetFeature`/`BatchGetFeatures` 是 recommend-* 的在线读（50×20 条与 1 MiB 的配额不该被排障页刷新吃掉），三者刻意不开 HTTP（AGENTS.md §7）；`entity_id` 与各条特征值只回给调用方、**永不进网关日志** | feature-store gRPC |
| `/admin/membership` | 会员运营：10 条（全部 POST + JSON body，写入口挂 `AdminPermission`）。**「开通即生效」是读会员授予表的真实结论**，不是伪造成功 | membership gRPC |
| `/admin/payment` | 资金台账运营：8 条。**只有沙箱台账**：渠道回调验签、退款到卡、提现、打款出金、对账文件、发票税务一律返回 not-configured，不返回假成功 | payment gRPC |
| `/admin/order` | 商业订单运营：6 条。订单状态机只由 trade-order 推进，本网关不代写 | trade-order gRPC |
| `/admin/coin` | 投币运营：4 条。硬币余额归 coin 服务，网关不持有 | coin gRPC |
| `/admin/creator-revenue` | 创作者分成运营：11 条。只读计量与结算台账，**任何出金动作都不在本面** | creator-revenue gRPC |

上表只列到组一级，**逐条路由（方法、路径、入参位置、请求/响应类型、所属权限点）以生成文档为准**：
[docs/api/http/admin/](../../docs/api/http/admin/README.md) 按同一口径拆成 32 个分组文件，
可导入的调试集合见 [postman/](../../postman/README.md)。集合里 `AdminPermission` 分组需要先回填
`admin_token` 环境变量，否则按 fail-closed 全部 403。

以上各路由组的每个写接口都要求操作者主体存在（审计必须有主体），并且**必须带着有效会话**：
`internal/logic/adminsubject.go` 是这类门槛的唯一入口，每一条受保护路由的 logic 若在读侧拿不到
`AdminPermission` 挂上的 `admin_id`，就在发出任何下游调用之前 fail-closed 报
`gateway/admin: admin session required`。两类主体处理不同（`internal/middleware/adminpermissionmiddleware.go`
判定通过后才有资格声称「这是一次已鉴权的后台操作」）：

- **后台账号 ID 空间**（risk-control 的 `operator`、notification 的 `op.operator_id`、
  search-indexer 只进本地审计日志的 `operator_id`）：会话值**覆盖**客户端声明值，不一致时以会话为准并
  按 Error 留痕。表单漏填 `operator_id` 不再是被拒理由——会话已经证明了是谁在操作。
- **用户 mid / 自由文本操作人**（comment/danmaku/inbox/private-message 的 `operator_mid`、
  catalog/rights/video/user-profile 与 moderation 的 `operator`）：**不覆盖**（`admin_id` 是
  `op_admin_user` 主键，mid 是账号 mid，两个编号空间），只要求「会话存在 + 主体非空」，
  并把 `admin_id` 与声称主体写进同一行日志。

带 `request_id`/`idempotency_key` 的写接口还要求该键非空（只 `TrimSpace` 判空、**不改写原值**，
任何截断或归一化都会让幂等键失去语义）；分页与批量上限沿用下游服务的口径
（comment 49、danmaku 100、search-indexer 100、risk-control 50、operation 100、
inbox 收件人数取 `Inbox.MaxRecipients`、live-room 列表 100 / 分区字典 200 由服务侧夹取并直接拒绝超限、
recommend-recall 池快照 200 / 版本台账 100 / 日志分页 100 / 单批写入 1000 / 每池保留版本下限 2、
recommend-rank 决策分页 100 / 单个特征配置 512 key / 决策摘要 top_aids 20——后两组的数字都在
`services/recommend-*/etc/*.yaml` 里，网关不复制），网关不擅自放大，也不复算下游已有的校验。
`/admin/recommend` 的 operator 一律由会话渲染成 `gateway/admin:<admin_id>`（与 `/admin/cron` 同一口径），
9 个写入口的表单类型都不暴露 `operator` 位；`0` 在这两个域普遍是「用服务默认值」的合法哨兵
（`version=0` 读 CURRENT、`ps=0` 用默认页大小、`max_rows=0` 由服务取批量上限、`mid=0` 游客），
只有 `target_state=0`（`*_UNSPECIFIED`）在两个服务里都没有对应语义，网关直接拒绝。
`VerifyPlaybackToken` 不在 `/admin` 暴露。

### `/admin/recommend` 刻意不开的方法

| RPC 方法 | 不开的理由 |
|---|---|
| `recall.RecallCandidates` | 终端侧在线召回热路径：限流、缓存、`ttl_seconds` 与降级声明都在推荐链路上（`gateway/app` 侧），后台再开一份等于两套口径和一个绕过端侧频控的取数口子 |
| `rank.RankCandidates` | 同上：在线排序主入口，入参是候选子集 + 用户上下文，不是后台查询；后台要看的是**已经发生**的决策（`/rank/decision`、`/rank/decision/list`） |
| `rank.GetExperimentAssignment` | 按主体（mid 十进制串或设备 sha256 摘要）查/登记分桶。把某个主体的实验归属推给运营界面，等于交付一次用户维度行为数据出口——权限点表里没有这一格，本轮也不打算加 |

除这三条外，两个 proto 里没有其它「非后台」方法：`recommend-recall` 的池生成/清理由 `services/cron` 复用同一批
`UpsertPoolItems`/`PrunePoolVersions` RPC（不是单独的服务间接口），`recommend-rank` 没有 consumer 或回调方法。
因此本组路由 = 两份契约里全部面向查询与控制的入口，无遗漏。

> 契约缺口（`/admin/recommend`）：`UpsertPoolItemsReq` **没有 operator 位**——`generator` 描述「这批数据由哪个作业产出」，
> 不是操作者，网关按表单原样透传并只把 `admin_id` 落日志，因此池版本行看不到「谁点的写入」。
> `PrunePoolVersionsReq` 既没有 `idempotency_key` 也没有 `trace_id`，`POST /pool/version/prune` 用网关侧的
> `request_id` 做防重与留痕键（无法传给服务做真正的重放去重；删除本身是单调动作，流程要求先 `dry_run=true`
> 核对 `scanned_versions` 再执行，`has_more=true` 表示需继续分批，网关不自动循环）。
> 另外两个 proto 的后台方法都不带 `trace_id`，所以 `.api` 里没有这个字段。补齐方式是先给契约加运营主体/追踪字段，
> 本轮不改 `services/**`。

> 依赖状态：`services/recommend-recall` 与 `services/recommend-rank` 的业务 logic 仍在并行落地
> （契约、rpc 桩与 SQL 迁移已就位）。`/admin/recommend` 是**诚实转发**：下游未实现或校验失败时原样返回下游错误
> （`Unimplemented`、`ErrInvalidPoolKey`、版本状态机拒绝、keep_versions 下限、权重和超限、分桶区间非法），
> 网关不伪造空池/空决策让它「看起来正常」，也不把这些判定在网关复算第二遍——那会出现两套结论。
> 后台联调建议在两个服务本地起来之后按 `GET /admin/recommend/pool/config`、`GET /admin/recommend/rank/runtime-config`
> 先确认下游可达，再看具体台账。

### `/admin/collector` 只读面（刻意不进 `routePermissions`）

9 条只读/诊断路由与 RPC 一一对应：`GET /health` → `GetCollectorHealth`、
`POST /schema/validate` → `ValidateEventSchema`、`GET /batch` → `GetIngestBatch`、
`POST /batch/list` → `ListIngestBatches`、`GET /event` → `GetEventRecord`、
`POST /event/list` → `ListEventRecords`、`POST /dead-letter/list` → `ListDeadLetters`、
`GET /policy/active` → `GetActiveDispatchPolicy`、`GET /policy/list` → `ListDispatchPolicies`。
`GetEventRecord` 不提供单独的「详情」契约：`ec_event_record` 的列表与单条读是同一份投影
（列表就是台账行，单条只多一个「用 Outbox 真值覆盖 `delivery_*`」的动作），
网关不为了看起来更丰富而拆出第二个响应类型。

**这些只读入口刻意不登记 `routePermissions`**，与 `/admin/audit` 的 `entry/list`、
`/admin/ops` 的 `config/list`、`/admin/cron` 的 `task/list` 同一口径：中间件对表外路由 fail-closed
（403），把排障读面登记进去等于让「RBAC 行没 seed」变成「采集台账看不到」——运营在事故现场
先被自己的一致性检查挡住，而这几条路由读到的又只是服务已经脱敏过的台账，不含可被越权滥用的写能力。
真正需要授权的是 4 个写入口（`AdminPermission` 组），它们与只读面分属两个 `@server` 块。

网关在这 9 条上只挡「下游没有对应语义」的传输层输入：`page_size`/`mid`/枚举位非负、
时间窗两端非负且不倒置、`batch_id`/`event_id` 去空后非空。
`page_size` 上限（`clampPage` 越界即 `ErrInvalidPage`，不静默截断）、`cursor` 语法
（非法即 `ErrInvalidCursor`，绝不退回第一页）、时间跨度上限、
「列表必须带至少一个收窄条件」与全部枚举白名单都由 `services/event-collector` 判定，
网关不复算第二遍（复算必然出现两套结论）。错误统一经
[common/httpresponse](../../common/httpresponse) 映射为四字段信封。

`eventcollector.v1` 的 15 个 RPC 里，本组只开 13 个，**刻意不接的 2 个**：

| RPC 方法 | 不开的理由 |
|---|---|
| `CollectEvents` | 客户端 SDK 批量上报入口，归 `gateway/app`（终端面）。后台开面等于给控制台一个「伪造任意 mid/设备行为」的通道，而 SPM 的全部下游（热度、留存、推荐特征）会把伪造当事实（AGENTS.md §7） |
| `IngestServerEvents` | 服务端内部埋点入口，`caller_service`/`idempotency_key` 表达的是**调用方服务身份**。网关不是被信任的上报服务，代为上报会让信封 producer 失真，事后无法归因到真实埋点方 |

> 写面口径：`POST /delivery/retry`、`/dead-letter/replay`、`/policy/upsert`、`/policy/activate`
> 与只读面分属两个 `@server` 块，4 个权限点按 cron/recommend 同一口径登记为
> `collector:delivery/retry`、`collector:deadletter/replay`、`collector:policy/update`、
> `collector:policy/enable`（**生效与改草稿分开**），operator 由会话渲染成
> `gateway/admin:<admin_id>`，表单不出现 `operator` 位，`idempotency_key`/`reason` 先挡空。
> 采样比例区间、策略生命周期（谁能把 DRAFT 变 ACTIVE）、盐可用性、白名单是否覆盖内置隐私底线、
> 单次重放条数上限、轮次幂等与投递重试上限全部由 event-collector 判定，网关只转达入参并投影结论。

> 依赖状态：`services/event-collector` 的读侧 logic 与 `deploy/migrations/event-collector` 已就位。
> 下游未实现或校验失败时本组路由原样返回下游错误，不伪造空台账。

### `/admin/spm` 的三条硬口径与刻意不开的方法

三条口径写死在 `api/admin.api` 的 spm 段头注释里，用例把守（`internal/logic/spm_admin_logic_test.go`）：

1. **后台不生产指标**：只有 `/metric/recompute`（从行为事实表按**显式**口径版本重算）与 `/job/submit`
   （排队一个聚合作业）两条推进数据的路径，两者都必须带幂等键；`metric_version=0` 在重算口被拒
   —— 悄悄拿此刻的 ACTIVE 版本去覆盖历史窗口，隔几天再点一次就是另一个结论。
2. **后台不改推荐结果**：本组没有任何「把某个 aid 顶到前面 / 调权重 / 屏蔽」的入参，
   `WriteMetricWindow`（计算链路写回窗口指标）刻意不开 HTTP —— 开了就等于允许手工改指标，
   违反 AGENTS.md §7 第 3 条。
3. **口径版本不可原地改**：`/definition/upsert` 只能新增版本，命中已登记的
   `(metric_key, metric_version)` 且规格不同时服务回 `ErrMetricVersionImmutable`，
   网关不预读列表去判断「算不算新增」；`created_by`/`ctime`/`mtime` 由服务按会话与库时钟渲染，
   表单里没有、网关也不造（自报经办人等于伪造审计主体）。

网关在这 15 条上只挡「下游没有对应语义」的形状：枚举位/主体不为 `UNSPECIFIED` 与 `<=0`、
幂等键非空、窗口区间不倒置（`from > to` 在服务侧圈不出任何窗口）、`ps`/`pn`/时间筛非负；
`ps` 上限 100、`keys` 最多 50 口径 × 30 窗口、`top_n` 上限 100、区间多大算过大、
状态机能否迁移、作业该不该受理全部由 `services/spm` 判定。契约里的 0 值哨兵
（`subject_type=0` 全部主体、`subject_id=0` 不限主体、`metric_key=""` 全部指标、
`metric_version=0` ACTIVE 版本、`window_start=0` 最近闭合窗口、`window_start_to=0` 当前时间、
`since=0` 不限时间、`ps=0` 默认页大小）一律原样下传，网关不替调用方挑值。
`BatchGetMetricsReply.points` 是 map，网关按 `(metric_key, metric_version, window_start)` 排序后摊平成列表
（map 遍历序随机，不排序会让同一请求两次刷新顺序不同，被误读成「数据变了」）。

> 契约缺口（`/admin/spm`，本轮已上报，未改 `services/spm`）：
> `GetUserInterestReq` 只有 `mid`/`metric_version`/`top_n`，**没有 operator 位**，因此「谁读了这个人的画像」
> 在 spm 侧落不下证据（本组唯一挂判定的读口恰恰就是它），留痕只剩网关访问日志与 operation 的判定记录；
> `UpsertMetricDefinitionReq` 与 `RecomputeMetricsReq` 都没有 `reason` 位（`UpdateMetricDefinitionStateReq` 有），
> 所以「为什么要登记这个版本」「为什么重算这段窗口」在服务台账里没有落点，网关也不为它编一句理由、
> 更不把 `trace_id` 塞进 `request_id`（那是幂等键）；`trace_id` 因此一律只进网关日志。
> 补齐方式是先给 `spm.proto` 加运营主体与理由字段，再改 `.api` 重新生成。

### `/admin/feature-store` 的三条硬口径与刻意不开的方法

三条口径写死在 `api/admin.api` 的 feature-store 段头注释里，用例把守（`internal/logic/featurestore_admin_logic_test.go`）：

1. **后台不写特征值**：本组没有任何「把某个主体的值改成 X」的入参，`WriteFeatures` 是计算链路
   （spm 指标投影 / 离线模型产出 / 风控滑窗）的专属写回通道，开了就等于允许运营手捏特征值喂推荐，
   违反 AGENTS.md §7 第 2 条。后台能推进数据的只有 `/backfill/submit`（排队一次按口径补历史值）。
2. **后台不站在在线热路径上**：`GetFeature`/`BatchGetFeatures` 是 recommend-* 的取数入口，
   契约给了硬上限（≤50 特征 × ≤20 主体、1 MiB 响应）而不是无限读；排障页每次刷新都吃掉一份配额，
   等于让控制台与端侧推荐抢预算。主体维度的取值核对走 `/entity-feature/list`（分页、受隐私级别收敛、
   挂权限点），不是把热路径读口开给后台。
3. **隐私动作只走专门入口**：`/entity-feature/list`（导出）与 `/entity-feature/erase`（擦除）各占一个权限点，
   不与其他读或写共用；服务侧还有第二道闸（`Privacy.OperatorPrefixes` 白名单，**空白名单 = 谁都拒**、
   `ExportMaxPrivacyLevel` 收敛可见级别），网关既不代替它放行，也不在被收敛后「换个小范围重读一次」放宽范围。
   `entity_id` 与各条特征值都不进网关日志（§7 行为数据脱敏；服务的 `checkEntity` 同理不回显主体）。

网关在这 13 条上只挡「下游没有对应语义」的形状：幂等键非空、写入口的显式版本与枚举位不为 0、
`entity_scope` 与 `entity_id` 必须齐备、隐私过滤位非负、回填区间不倒置、`job_id`/`request_id` 二选一主体
至少给一个、`limit` 不越 int32（越界后截断会落到 0，而 0 恰好是「清到服务端上限」的哨兵）。
`ps` 上限 100、`entity_ids` 条数上限 1000、`entity_id` 的**形态**（DEVICE/IP_HASH 只接受十六进制摘要）、
值类型与维度是否自洽、TTL/默认值是否可用、不可变字段是否冲突、版本能否上线、切换的乐观基线是否命中、
`before` 是否晚于服务时钟、超限批大小如何夹取，全部由 `services/feature-store` 判定。
契约里的 0 值哨兵（`version=0` 按 ACTIVE 指针解析、`entity_ids` 为空 = 全量扫描、
`expected_from_version`/`from_version=0` 不做乐观校验、`window_to=0`/`before=0` 当前时间、
`limit=0` 服务端上限、`min`/`max_privacy_level=0` 不限级别、`since=0` 不限时间）一律原样下传，
网关不替调用方挑值。**与 spm 的一处实际差别**：本服务的 `ps` 没有「0 = 默认页大小」语义
（`model.ValidatePageSize` 判 1..100，0 直接拒），因此 `ps` 在 `.api` 里是必填、0 也挡在网关。

> 契约缺口（`/admin/feature-store`，本轮已上报，未改 `services/feature-store`）：
> `ListEntityFeaturesReq` 没有 operator 位（本组唯一挂判定的读口恰恰就是它），「谁导出了这个人的特征」
> 在服务侧落不下证据；`SwitchRecord` 没有 `switch_type` 位，回滚与前进切换在审计里长得一样，
> 只能靠 `from`/`to` 大小关系猜；`BackfillJob` 没有 `from_version` 位，`auto_switch` 作业的乐观基线读不回来；
> `PurgeExpiredReq` 是本域唯一没有 `reason` 位的写方法（手工补跑一轮清理的原因无处落）。
> 补齐方式是先给 `featurestore.proto` 加这些字段，再改 `.api` 重新生成。

### `/admin/open-platform` 的三条硬口径与刻意不开的方法

三条口径写死在 `api/admin.api` 的 open-platform 段头注释里，用例把守
（`internal/logic/openplatform_admin_logic_test.go`）：

1. **后台不代替归属者表达意愿**：`RegisterApplication`（提交申请并首次签发 secret）、
   `RegisterWebhook`（新增回调端点，需应用回显 challenge 才算验证通过）、
   `EnqueueWebhookEvent`（领域事件入队）都不开 HTTP。注册是开发者自助动作，改回调地址等价于
   换数据去向，事件只能由拥有事实的服务投递——后台手工造一条事件等于伪造一次投递。
   因此本域也没有「改端点地址」的入口，只有 `/webhook/delete`。
2. **后台不签发、不换发、不探测用户凭证**：OAuth 五法（`IssueAuthorizationCode` 要求
   `consent_given=true`，而同意只能由登录用户表达；`ExchangeAuthorizationCode`/
   `RefreshAccessToken` 会消费一次性授权码并轮换 refresh；`IntrospectToken`/`AuthorizeRequest`
   是网关鉴权热路径，入参本身是明文凭证）一律不开。后台开一个口等于允许运营以用户身份取 token，
   并把凭证明文引到后台日志链路上。
3. **后台不注入事实**：应用状态机、scope 获批集合、配额规则与投影都由 `services/open-platform`
   判定，网关只挡「没选主体、没给幂等键、明显不可能形状」（`app_id`/`endpoint_id`/`delivery_id`
   为 0，`target`/`target_status` 为 `*_UNSPECIFIED`，数值位为负，`window_end<=window_start`，
   `enabled=false` 在本 RPC 无路径）。

本域读面比 spm/feature-store **更严一档**：除 `/scope/list`（`ListScopesReq` 没有任何操作者位）外，
六条读全部挂 `AdminPermission`，其中五条还要求 `operator_mid`/`caller_mid > 0`——契约里 `mid==0`
的含义是「owner 自查，归属由网关校验」，而后台不是 owner、也没有做过那道校验；
`/application/list` 是唯一挂判定却给不出主体的例外（`ListApplicationsReq` 没有主体位，缺口 ②）。
读的是第三方凭证面与调用台账，不是「看个目录」。

`is_operator` / `operator` 位一律由网关置 `true`（表单没有也不该有这一位）；
但网关**不**用会话 `admin_id` 覆盖 `operator_mid`——两者不同编号空间（缺口 ①）。

> 契约缺口（`/admin/open-platform`，本轮已上报，未改 `services/open-platform`）：
> ① 操作者全部落在用户 mid 空间（`operator_mid`/`caller_mid`），而后台会话主体是
> `op_admin_user.admin_id`，与 `/admin/live` 同一问题——网关只能要求显式给出 mid 并把
> `admin_id` 与 mid 一起打日志；
> ② `ListApplicationsReq` 没有主体位，只看 `operator` 布尔，因此 `/application/list` 的台账里
> 落不下「谁拉的这一页」（`openSessionGate` 只能在网关日志留 admin_id）；
> ③ proto 没有暴露 `DisableQuotaPolicy`，且 `UpsertQuotaPolicyReq` 没有 `reason` 位，
> 服务对 `enabled=false` 失败关闭，所以本域**没有停用配额规则的路径**，网关就地拒绝
> `enabled=false` 并给出可执行的解释，而不是让调用方收到一句指向它没填过的字段的「reason required」；
> ④ `UpdateApplicationReq.expected_version` 在状态通道不被采纳（只有资料通道用乐观锁），
> 因此 `/application/state` 的表单里刻意没有这一位——暴露一个填了也不生效的乐观锁位是假契约；
> ⑤ `RecomputeQuotaReq.app_id` 的 proto 注释写「0 表示全部应用」，实现要求 `>0`，
> 本入口按实现对齐（必填 `>0`），注释与实现的 divergence 需在 `openplatform.proto` 一侧收敛。
> 补齐方式是先改 proto（含把注释与实现对齐），再改 `.api` 重新生成。

### AdminPermission 中间件（`/admin/operation` 受保护路由）

- 从 `Authorization` 取凭证：接受 `Bearer adm_xxx`（scheme 大小写不敏感）与裸 token，
  其它 scheme（Basic/Digest）与只有 scheme 的写法一律按无凭证处理；token 永不进日志。
- 按 `internal/middleware/adminpermissionmiddleware.go` 里的**静态路由→(resource, action) 表**
  调用 `operation.VerifyAdminPermission`，网关不解析 token 摘要、不持有
  `OPERATION_ADMIN_TOKEN_SECRET`，也不缓存判定结果（缓存会退化「禁用账号立即失效」）。
- fail-closed：表外路由 403、无凭证 401、会话非法（`reason=admin_session_invalid`）401、
  权限不足 403（回显 reason）、未配置 `OperationRPC`／判定异常／`allowed` 却给不出 `admin_id` → 503。
  拒绝响应同样是四字段信封。
- 判定通过后把 `admin_id` 与命中角色写入 request context。logic 侧统一从
  `internal/logic/adminsubject.go` 的四个门槛读这份身份（见上面「主体口径」段）：
  `adminOperatorID` 覆盖后台账号 ID 空间的自报值、`adminSubjectGate`/`adminActorGate`
  要求 mid 或文本主体非空但不改写它、`adminSessionGate` 兜住契约里根本没有主体位的路由。
  `/admin/cron`、`/admin/recommend` 与 `/admin/feature-store` 的下游契约没有 `operator_id` 数值位，
  只有 `operator` 文本位，
  网关按同一口径渲染成 `gateway/admin:<admin_id>`（前缀是「哪个入口提交的」，不是实例地址）；
  表单类型里根本不出现 `operator`，也就无从伪造。
  「每一条 `AdminPermission` 路由的 logic 都真的读了会话」由
  `internal/logic/adminsubject_test.go` 的 `TestEveryProtectedRouteLogicUsesSession` 用源码扫描钉住：
  它从 `routes.go` 解析出挂了中间件的路由集合，与权限表逐条对齐后映射到 logic 文件，
  再对包内助手做传递闭包，任何一条既不调 `middleware.AdminFromContext` 也不经上述门槛的 logic 都会失败。
- 表内共 166 条（阶段1-2 之前的 125 条：operation 20 + live 25 + audit 4 + ops-config 10 + cron 8 +
  recommend 9 + collector 4 + 私信 2 + spm 5 + feature-store 8 + open-platform 15 + 商业化五域 15；
  阶段1-2 补齐的 41 条：member 9 + risk 7 + catalog 5 + notification 4 + rights 3 + inbox 2 + search 2 +
  video / transcode / moderation / comment / danmaku / order / payment / account 各 1～2）。
  166 条路由命中 162 个不同权限点（四对路由共用同一格：`account:cache`#invalidate 的 `/cache/del` 与
  `/cache/clear`、`member:moral`#update 的单个与批量、`openplatform:application`#read 的 get 与 list、
  `operation:task`#read 的 get 与 list），分属 29 个 domain。这张表与生成的
  `internal/handler/routes.go` 的一致性由 `internal/middleware/route_permission_drift_test.go`
  双向校验：挂了 `AdminPermission` 却没登记权限点的路由（会被永久 403）、以及登记了却没有
  对应受保护路由的死条目，都会在测试里失败。新增受保护路由时改 `.api` + 生成 + 登记权限点三步必须一起提交。
- 带路径参数的路由（`/admin/video/submissions/:aid/transition`、`/admin/catalog/episodes/:epid/publish`、
  `/admin/rights/windows/:window_id/expire` 等）按**段级模式**判定：表内键保留 `:param` 原文，
  `permissionFor` 先精确查表、再交给 `matchRoutePattern`（段数相等 + 字面段逐字相等 + `:param` 段非空）。
  因此路径参数不会被当成任意前缀放行，空段（`/episodes//publish`）与多一段（`/episodes/9/10/publish`）都判不中 → 403。
  用例见 `TestParamRoutesMatchConcretePaths`。
- 受保护组里的 GET 是**点名过的例外**，只有 5 条（`protectedGetAllowlist`）：
  `/admin/account/cache/del`、`/admin/inbox/unread/recompute` 形状是 GET 但后果是写；
  `/x/member/realname/stripped/info`、`/x/member/realname/mid/by/card`、`/x/member/web/login/log`
  是「被读主体明确的个人数据定向读」。本仓整体口径仍是写面挂判定、读面免鉴权
  （后台列表页每次刷新都打一次 RPC，全量挂判定会把 operation 变成读放大瓶颈），
  挂判定的读口只有上面这三条定向个人数据读。白名单与 routes.go 双向校验，
  新增一条受保护 GET 就得同时在这里写理由，否则用例失败。

> 契约缺口（仍在）：`operation.v1` 没有提供权限点目录（如 ListAllResources）契约，可判定的 resource
> 取值只取决于库内 `op_permission` 行。原本次「库里没有权限点行 → 受保护路由稳定 403」的缺口已由此前的
> 登记方式转为受测种子：`deploy/migrations/operation/000004_seed_op_permission.sql` 落这 162 个权限点
> （166 条路由里有四对共用同一权限点，其中一对是本域 `/application/get` 与 `/application/list`
> 共用 `openplatform:application:read`），
> `internal/middleware/seed_permission_test.go` 双向断言种子与 `routePermissions` 逐字相等，
> 所以「加了路由忘了发迁移」现在会在测试里失败，而不是等到运营点进 403。
> 角色侧现已有**派生默认角色**，分两轮：`000005_seed_op_role_grants.sql` 与
> `000006_seed_op_role_grants_stage12.sql` 都按 `op_permission.domain` 建 `domain_*` 角色、
> 按 `action='read'` 建 `readonly`、外加 `super_admin`，成员全部由 `SELECT` 现算，不写死任何自增 id。
> 两轮合起来在隔离实例上重放得到 **29 个域角色 + `readonly`（16 点）+ `super_admin`（162 点）= 31 角色 /
> 340 条绑定**，0 个权限点处于「无任何角色可挂」状态，重放第二遍计数不变。
> 之所以要第二轮：`scripts/migrate.ps1` 跳过已记录版本，已跑过 000005 的库不会重新并入新权限点，
> 所以每轮新增权限点都要配一条更高编号的派生绑定迁移；
> `services/operation/model/role_seed_test.go` 里的 `TestGrantsMigrationNeverTrailsPermissionSeed`
> 钉住「最大的权限点种子编号不得超过最大的角色绑定迁移编号」。
> 它们**不写** `op_admin_role`/`op_admin_user`：谁拿到哪个角色仍是人的决策，只是从「手写 162 条授权 SQL」
> 变成「挑一个已有角色挂上去」。仍缺的是权限点目录契约（`operation.v1` 没有 ListAllResources 之类），
> 以及 `domain_operation` 含 `operation:role:*`/`operation:admin_user:*` —— 拿到它就能给自己加权限，
> 不能当日常运营岗默认角色；新增的 `domain_inbox`（群发站内信）与 `domain_risk`（风控规则与名单）
> 同理属高影响面。`readonly` 刻意不含 `member:realname`#reverse-lookup（证件号反查 mid 可批量配对个人数据）。
> 门禁见 `services/operation/model/role_seed_test.go`。
> 另：`VerifyAdminPermissionReply` 不回传 `username`，`op.operator_name` 仍是客户端声明值。

> 契约缺口（`/admin/live`）：上面「以会话为准覆盖 operator」那条**不适用于 live 写路由**——
> `liveroom.*` 的操作者字段只有 `operator_mid`，它属用户 mid 空间，而会话给的是
> `op_admin_user.admin_id`，两者不是同一编号空间，覆盖等于把封禁/下架记到无关用户头上。
> 因此网关只要求表单显式给出 `operator_mid > 0`，并把 `admin_id` 与 `operator_mid` 一起打日志
> （「谁点的按钮」与「台账落在谁身上」两条都可追）。要真正闭环需在 `liveroom.proto` 补管理员主体
> （如 `admin_id` 字段或 `Operator` 消息），本轮不改契约。
> 同类缺口：`UpdateRoomSetting` 在服务侧沿用主播侧的「生效房主」校验，`BanRoom`/`CloseRoom(admin=true)`
> 才有运营语义。`POST /admin/live/setting/update` 按契约原样透传，`operator_mid` 不是房主时下游会回
> `ErrAnchorNotOwner`——网关不伪装房主、也不吞掉这个错误；后台需要改别人房间的设置时，走
> `POST /admin/live/room/close` 下架，或服务侧补一个 admin 主体后再放开。

> 运维提示：`etc/admin.yaml` 的 `Timeout` 默认 3000ms，而 `POST /task/run` 会让 operation
> 按 `run_steps`（默认 100）逐步调用下游 RPC，人工点「推进」可能先触发网关超时——
> 服务端仍会跑完，重试请沿用同一个 `op.request_id`；定时推进应走 `services/cron`。

路由风格参考参考仓库 member 服务的内部运营 HTTP 面（`/x/internal/member/*`），
按本仓库架构迁移到管理后台网关并以 RPC 聚合。

## 配置

`etc/admin.yaml` 共 **35** 个下游 RPC 配置（与 `internal/config/config.go` 的 `*RPC` 字段一一对，
由 `config_load_test.go` 双向校验；`LiveMediaRPC` 与 `PrivateMessageRPC` 是本表此前漏计的两项，
商业化五域 `MembershipRPC / PaymentRPC / TradeOrderRPC / CoinRPC / CreatorRevenueRPC` 是 2026-09-22 商业化轮新增的五项，
`SpmRPC`（`spm.v1.rpc`）与 `FeatureStoreRPC`（`featurestore.v1.rpc`）是同日行为分析运营面新增的两项，
`OpenPlatformRPC`（`openplatform.v1.rpc`）是同日第三方开放平台运营面新增的一项），
etcd key 与服务 `Name` 一致：`AccountRPC`（`account.v1.rpc`）、
`UserProfileRPC`（`user-profile.v1.rpc`）、`VideoRPC`（`video.v1.rpc`）、`CatalogRPC`（`catalog.v1.rpc`）、
`RightsRPC`（`rights.v1.rpc`）、`ModerationRPC`（`moderation.v1.rpc`）、`TranscodeRPC`（`transcode.v1.rpc`）、
`AssetRPC`（`asset.v1.rpc`）、`DanmakuRPC`（`danmaku.v1.rpc`）、`SearchIndexerRPC`（`searchindexer.v1.rpc`，
注意服务侧键名无连字符）、`RiskControlRPC`（`risk-control.v1.rpc`）、`OperationRPC`（`operation.v1.rpc`）、
`CommentRPC`（`comment.v1.rpc`）、`NotificationRPC`（`notification.v1.rpc`）、`CreatorRPC`（`creator.v1.rpc`）、
`InboxRPC`（`inbox.v1.rpc`）、`AuditRPC`（`audit.v1.rpc`）、`OpsConfigRPC`（`opsconfig.v1.rpc`）、
`CronRPC`（`cron.v1.rpc`）、`RecommendRecallRPC`（`recommendrecall.v1.rpc`）、`RecommendRankRPC`（`recommendrank.v1.rpc`）、
`LiveIngestRPC`（`liveingest.v1.rpc`）、`LiveGatewayRPC`（`livegateway.v1.rpc`）、
`EventCollectorRPC`（`eventcollector.v1.rpc`，服务侧注册名同样不带连字符，写成 `event-collector.v1.rpc`
会永远发现不了对端；未配置时 `/admin/collector` 的 13 条路由一律返回「event-collector service not configured」
而不是伪造空台账），
`LiveRoomRPC`（`liveroom.v1.rpc`，键名同样无连字符；`/admin/live` 用它做房间域读写，
live-ingest / live-gateway / live-media 三个媒体域已各自接入 `/admin/live/*` 路由组；
`RecommendRecallRPC`/`RecommendRankRPC` 由 `/admin/recommend` 的 17 条路由使用，未配置时一律返回「recommend-recall/rank service not configured」而不是伪造空台账）。
`PrivateMessageRPC`（`privatemessage.v1.rpc`，服务侧注册名不带连字符，写成
`private-message.v1.rpc` 会永远发现不了对端；未配置时 `/admin/private-message` 的 3 条路由一律返回
「private-message service client not configured」而不是伪造空举报台账）。
商业化五域 `MembershipRPC`（`membership.v1.rpc`）、`PaymentRPC`（`payment.v1.rpc`）、
`TradeOrderRPC`（`tradeorder.v1.rpc`，服务目录是 `services/trade-order` 但注册名不带连字符）、
`CoinRPC`（`coin.v1.rpc`）、`CreatorRevenueRPC`（`creatorrevenue.v1.rpc`，同上口径），
未配置时对应域的 39 条路由一律返回「not configured」而不是伪造空台账。
`SpmRPC`（`spm.v1.rpc`，服务目录 `services/spm`、注册名与键都不带连字符，端口 8131）供
`/admin/spm` 的 15 条路由使用；未配置时一律返回「spm service not configured」，
不返回空榜单——那会把「下游没接」读成「这条内容没人看」。
`FeatureStoreRPC`（`featurestore.v1.rpc`，服务目录是 `services/feature-store` 但注册名与 etcd key
都不带连字符，写成 `feature-store.v1.rpc` 会永远发现不了对端；端口 8130）供 `/admin/feature-store`
的 13 条路由使用；未配置时一律返回「feature-store service client not configured」，
不返回空特征目录——那会把「下游没接」读成「这个域一个特征都没登记」。
`OpenPlatformRPC`（`openplatform.v1.rpc`，服务目录是 `services/open-platform` 但注册名与 etcd key
都不带连字符，写成 `open-platform.v1.rpc` 会永远发现不了对端；端口 8151）供 `/admin/open-platform`
的 16 条路由使用；未配置时一律返回「open-platform service client not configured」，
不返回空应用目录——那会把「下游没接」读成「一个第三方应用都没接入」。

> 商业化 logic 接线进度：五域 39 条（membership 10 / payment 8 / order 6 / coin 4 / creator-revenue 11）
> 全部接完，不再有显式失败的桩。统一口径：读接口真调下游；写接口以会话 `admin_id` 渲染
> `gateway/admin:<id>` 作 operator、幂等键原样映射 `request_id`、无会话身份 fail-closed 且一次
> RPC 都不发、分页三元组回读自 reply 不复算。
> membership 域原先的三个契约缺口已在 2026-09-22 的 `.api` 补丁轮收口（改 `gateway/admin/api/admin.api`
> 后 goctl 重新生成，服务语义未动）：`ParamMembershipPlanUpsert` 补 `reason`（可选，逐字下传落
> `mb_plan_change_log.reason`，留空由服务回落成规格摘要）；`ParamMembershipRevoke` 补
> `plan_id`/`biz_order_no`/`payment_no` 三个追溯位（网关不填默认、不 trim，只挡负 `plan_id` 与含空白的
> 单号——跨服务引用是精确匹配，带空格等于一条永远对不上账的台账），后台因此能区分「退款回收」与
> 「运营纠错」；`admin.api` 里 revoke `vip_type=0` 表示「由服务按最高档处理」的错误注释已按实现改成
> 「必填且必须 > 0」，网关仍原样下传 0 让调用方收到真实的 `ErrInvalidVipType`。
> payment 域同批缺口：`GetWalletReply` 没有 `found` 位而服务明写「不建行、0 余额」，后台分不清
> 「从未开户」与「已花光」；`PaymentPaymentItem` 不回传 `operator`/`remark`（proto 第 15/16 位的注释
> 正是「运营面必须看到经办人」），`/payment/list` 看不到推单人；`ParamPaymentRefundList` 无
> `from_ts`/`to_ts`，`mid=0` 且不给 `payment_no` 时服务必回 `ErrListWindowRequired`（网关不代填窗口）；
> payment 未导出 operator 列宽常量（membership 有 `model.MaxOperatorLength`），64 目前是网关侧第二处副本。

> 链接前置条件（已在契约轮修掉）：`services/membership/rpc` 的 descriptor 文件路径必须是
> `services/membership/rpc/membership.proto`（带目录），不能是裸名 `membership.proto`。
> protobuf 的全局注册表按文件路径去重，而 `go.etcd.io/etcd/api/v3/membershippb` 已注册了
> 裸名 `membership.proto`，本网关经 zrpc 服务发现同时链接了 clientv3 与该 rpc 包，
> 裸名会在 init 阶段 panic `proto: file "membership.proto" is already registered`。
> 现在 `scripts/gen.ps1` 的 `$descriptorPrefixedProtos` 会为该 proto 补一次仓库根相对路径生成
> （`protoc -I . --go_opt=paths=source_relative`），生成物位置与 Go 包名都不变，网关侧无改动。
> 验证方式：`go test -p 1 -count=1 ./gateway/admin/internal/logic/` 默认冲突策略下即可通过
> （修复前只有设 `GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn` 才跑得起来）。
除 `AccountRPC`、`UserProfileRPC` 外均为 `optional`，未配置时对应路由返回「service not configured」而不是启动失败；
`OperationRPC` 未配置时 `AdminPermission` 中间件对**所有**受保护路由返回 503（fail-closed），
不会退化成免鉴权。网关不持有 `OPERATION_ADMIN_TOKEN_SECRET`，登录口令只透传给
`operation.AdminLogin`。业务缓存 `CacheRedis` 留在各服务侧，网关不持有。

## 生成与运行

```powershell
# 生成（从仓库根目录；框架文件由 goctl 生成，logic 骨架内只填聚合逻辑）
goctl api validate -api gateway/admin/api/admin.api
goctl api go -api gateway/admin/api/admin.api -dir gateway/admin
node scripts/gen-api-docs.mjs   # 路由/权限点一变就重新生成分组文档与 Postman 集合

# 运行
go run ./gateway/admin -f gateway/admin/etc/admin.yaml

# 健康检查
Invoke-WebRequest http://127.0.0.1:8081/admin/healthz -UseBasicParsing
```

## 测试覆盖

### 1. 构造器级覆盖：`229/313`（**84 个 logic 没有构造器级用例**）

`internal/logic` 有 313 个 `NewXxxLogic` 构造器（与 313 条路由一一对应），探针逐个在 `*_test.go`
里查引用：**229 个有、84 个没有**（名单来源：`.gotmp/readme-metrics/gateway-admin-probe.txt`）。
没有用例的这 84 个方法目前**没有装配断言也没有投影断言**，下表按运营域分组，可据此排补测优先级。

| 域 | 未覆盖数 | 未覆盖的构造器（方法名） |
|---|---|---|
| 直播 ingest/网关面（节点、路由、流键、配额、广播与断流事件） | 21 | `LiveIngestNodeList`、`LiveIngestNodeUpsert`、`LiveNodeAssignmentList`、`LiveRoomRouteList`、`LiveRoomRouteDrain`、`LiveRoomConnectionList`、`LiveConnectionKick`、`LiveAccessQuotaGet`、`LiveAccessQuotaUpsert`、`LiveBroadcastSend`、`LiveBroadcastLogList`、`LiveFailedEventRetry`、`LiveStreamList`、`LiveStreamGet`、`LiveStreamClose`、`LiveStreamHealth`、`LiveStreamKeyList`、`LiveStreamKeyGet`、`LiveStreamKeyRevoke`、`LiveStreamEventList`、`LiveStreamInterruptionList` |
| operation 后台自身面（账号/RBAC/菜单/配置/任务/鉴权） | 14 | `VerifyAdminPermission`、`CreatePermission`、`ListPermissions`、`CreateRole`、`ListRoles`、`DeleteRole`、`AssignRoles`、`UpdateAdminUser`、`DisableAdminUser`、`SaveMenu`、`GetOpsConfig`、`CancelAdminTask`、`ListAdminTasks`、`GetAdminTask` |
| 用户运营（user-profile / account） | 11 | `MoralsUpdate`、`MoralUpdate`、`MoralUndo`、`ExpSet`、`ExpUpdate`、`PropertyReview`、`RealnameStripped`、`RealnameMidByCard`、`LoginLog`、`CacheClear`、`CacheDel` |
| 版权目录与窗口（catalog / rights） | 9 | `CreateCatalogSeason`、`CreateCatalogEpisode`、`ListCatalogWorks`、`OfflineCatalogEpisode`、`CreateRightsContract`、`ListRightsContracts`、`CreateRightsWindow`、`ListRightsWindows`、`ExpireRightsWindow` |
| 风控（risk-control） | 9 | `RiskCheck`、`RiskReport`、`GetRiskDevice`、`UpsertRiskDevice`、`UpsertRiskRule`、`ListRiskListEntries`、`ApplyPunishment`、`LiftPunishment`、`ListPunishments` |
| 媒资与转码（asset / transcode） | 6 | `GetAsset`、`ListAssets`、`GetTranscodeTask`、`ListTranscodeTasks`、`ListTranscodeTemplates`、`CreateTranscodeTemplate` |
| 审核（moderation-orchestrator） | 4 | `GetModerationTask`、`ListModerationTasks`、`GetModerationResult`、`ProcessModerationAppeal` |
| 索引运营（search-indexer） | 4 | `SubmitRebuildTask`、`GetRebuildTask`、`SwitchAlias`、`GetIndexHealth` |
| 稿件（video） | 3 | `GetVideoSubmission`、`ListVideoSubmissions`、`TransitionVideoSubmission` |
| 弹幕屏蔽词（danmaku） | 2 | `BlockWord`、`ListBlockWords` |
| 健康检查 | 1 | `Health` |

**读侧要点**：`conv_operation_test.go`（16/3）与 `conv_risk_test.go`（17/4）存在，但它们锁的是
投影与入参归一的**共同口径**，没有逐方法覆盖上表列出的那些构造器；`conv_search_test.go`(9/1) 同理。
「媒资/转码、审核、稿件、目录与版权、风控、用户运营、直播 ingest/网关」这七组的写入口
在网关侧**没有逐条用例**，其权限点登记仍由 §3 的两条漂移门禁守住（登记 ≠ 行为验证）。

### 2. `internal/logic` 用例清单（24 个文件）— `402/44`

| 域 | 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|---|
| 会话不变量 | `adminsubject_test.go` | 4/7 | 四个主体门槛行为正确（有会话才放行、后台账号 ID 以会话为准）；**源码扫描**钉住「每条 `AdminPermission` 受保护路由的 logic 都真的读了会话」：从 `routes.go` 解析受保护集合 → 与权限表逐条对齐 → 映射 logic 文件 → 对包内助手取传递闭包 |
| 商业化 | `commerce_order_coin_admin_logic_test.go` | 31/0 | trade-order/coin 面：订单状态机与余额判定**不由网关复算**；不伪造成功；本轮刻意不在网关挡的两位留在用例里 |
| 采集台账 | `collector_admin_logic_test.go` | 31/0 | rpc→types **逐字段不丢**（批次七类计数、事件 `delivery_*` 五列、死信 reason/attempts/state、策略每位数值与 operator/ctime/mtime）；入参原样交给下游（`batch_id`/`event_id`/`cursor`/`idempotency_key` 只 TrimSpace 判空） |
| 商业化 | `commerce_revenue_admin_logic_test.go` | 28/0 | creator-revenue 11 条：单价护栏/min_quantity/monthly_cap/规则状态机/period 合法性全归服务；用例记录网关**不**挡的三位（单价负数、currency 留空、force_void_confirmed 的条件 reason） |
| 运营配置 | `opsconfig_admin_logic_test.go` | 22/0 | 主体与会话归因、`caller_service` 固定值、写接口 `request_id` 门槛、分页归一、闭集参数、枚举互转、投影含 `audit_entry_id` 证据指针、错误原样上抛；`expect_version` 冲突等规则不复算 |
| 直播 | `live_admin_logic_test.go` | 21/3 | rpc→types 逐字段不丢（含终端面裁掉的 `last_stream_seq`/`record_id`/`ban reason` 等）；三道写入口门槛（会话、`operator_mid`、`request_id`）；`CloseRoom` 的 `admin` 位只能由网关固定；未配置下游一律 fail-closed |
| 直播媒体 | `livemedia_admin_logic_test.go` | 21/0 | 逐字段不丢（`version`/`heartbeat_at`/`attempt` 是「能不能停」的依据，`last_seq`/`gap_count` 是「回放有没有洞」的证据，`scanned`/`deleted`/`skipped` 是「有没有误删」的凭证）；幂等键、对象存储引用、区间不改写不补默认 |
| 推荐 | `recommend_admin_logic_test.go` | 30/0 | 逐字段不丢（`batch_id`/`generator`/`schema_version`/`versions_digest`/`top_aids`/降级原因）；池键、幂等键、特征 key 顺序（含重复项）不去重不改写；未配置下游/请求体缺失/形态非法时**不得产生下游调用**，也不折叠成空结果冒充成功 |
| 商业化 | `commerce_membership_admin_logic_test.go` | 19/0 | membership 十条：档位够不够、状态机能否迁移、时长区间、币种、幂等指纹、「付费来源能否回溯到支付流水」都归服务；网关只锁不伪造成功、错误原样上抛（`duplicated=true` 例外） |
| 第三方开放 | `openplatform_admin_logic_test.go` | 18/0 | 逐字段不丢（`changed`/`replayed`/`rejected`/`deleted`/`deliveries_suppressed`/`payload_digest`）；15 条受保护入口无会话即 fail-closed 且 `operator_mid`/`caller_mid` 必须 >0；**不用 `admin_id` 覆盖运营自报 mid**；`is_operator` 由网关置 true |
| 定时任务 | `cron_admin_logic_test.go` | 23/8 | 枚举双向映射一一对应、0 值不当合法枚举、operator 只来自会话、`idempotency_key`/`reason` 门槛、乐观锁版本边界、游标分页非负与形态、服务端记账字段不得由客户端声明、投影逐字段不丢（`fence_token`/`takeover_count`） |
| 风控 | `conv_risk_test.go` | 17/4 | 枚举是否被挡在入口、投影是否搬运了服务端字段、分页归一是否与服务端同源 |
| 资金台账 | `commerce_payment_admin_logic_test.go` | 17/0 | payment 八条：状态机、余额、单次调整上限、币种、幂等号占用、渠道开关都归服务；未配置客户端时一律 not configured，不伪造成功台账 |
| operation | `conv_operation_test.go` | 16/3 | `OpContext` 主体/幂等键装配、分页与步骤规模与服务对齐、RPC 消息投影成后台 types |
| SPM | `spm_admin_logic_test.go` | 15/3 | 投影逐字段不丢（`found`/`reused`/`stale`/`windows_failed`/`payload_digest`）；`BatchGetMetrics` 的 map 摊平成稳定序；operator 只由会话渲染、五条受保护入口无会话即 fail-closed；写入口形状门槛一律**在调用下游之前**失败；0 值哨兵原样下传 |
| 审计 | `audit_admin_logic_test.go` | 14/1 | 主体与会话归因、`caller_service` 固定值、写接口 `request_id` 门槛、分页归一、枚举互转、投影完整性、错误原样上抛；链校验算法归 audit 服务，网关不重算 |
| 评论/弹幕 | `comment_admin_logic_test.go` | 13/4 | 审计主体怎么传、`admin=true` 是否真的落到下游、分页与排序枚举怎么升维、RPC 投影成后台 types；`fakeCommentCli` 只实现用到的 5 个方法，其余继承接口（越界即 panic） |
| 通知 | `notification_admin_logic_test.go` | 13/4 | `operator` 字符串从 `AdminOpContext` 派生、JSON int32 与 proto 枚举互转、投影、错误上抛；频控/免打扰/渲染/重投判定不在此重复 |
| 特征库 | `featurestore_admin_logic_test.go` | 13/2 | 投影逐字段不丢（`resolved_version`/`degradation`/`cursor_entity_id`/`remaining` 这些排障与审计证据）；operator 只由会话渲染、八条受保护入口无会话 fail-closed；写入口形状门槛在调用下游前失败；0 值哨兵原样下传 |
| 站内信 | `inbox_admin_logic_test.go` | 10/1 | 存在性校验、审计归属、枚举互转、未读 map 的投影顺序、错误原样上抛；去重/人数上限/幂等命中归 inbox |
| 索引运营 | `conv_search_test.go` | 9/1 | 投影与入参归一口径（无网络环境下秒级跑完） |
| 创作者 | `creator_admin_logic_test.go` | 8/2 | protobuf map 展平成稳定有序数组、分页与批量规模收敛到服务侧上限；分组语义/名册归属/签约判定留在 creator |
| 私信 | `privatemessage_admin_logic_test.go` | 7/1 | 举报台账 13 位逐字段不丢 + 清理四计数；`cursor`/`idempotency_key` 只判空不改写、0 是合法哨兵；写入口无会话 fail-closed、主体位必须 >0、`report_id`/`action` 的 0 必须被拒 |
| 目录 | `catalog_admin_logic_test.go` | 2/0 | catalog 写入口的两类主体口径：自由文本操作人只要求「非空 + 有会话」，**绝不用 `admin_id` 改写**；契约无操作者位时只能靠会话门槛保证事后可追 |

规模合计 **413 个顶层用例 + 47 个子用例**（logic 402/44、middleware 10/2、config 1/1），0 条 skip。

### 3. 其他层

| 层 | 文件 | 顶层/子 | 内容 |
|---|---|---|---|
| `internal/middleware` | 3 | 10/2 | `adminpermissionmiddleware_test.go`(6/2) token 怎么取、路由需要什么权限、判定结果如何落成 HTTP 状态码与四字段信封（假 client 只实现 `VerifyAdminPermission`，其余越界即 panic）；`route_permission_drift_test.go`(3/0) `routePermissions` ↔ 生成的 `internal/handler/routes.go` **双向**校验（挂了中间件没登记＝永久 403；登记了没路由＝死条目）；`seed_permission_test.go`(1/0) `routePermissions` ↔ `op_permission` 种子迁移逐字相等（含反向漂移）。本节只是把该 README「AdminPermission 中间件」段落的门禁落到数字上，未改写其事实 |
| `internal/config` | 1 | 1/1 | `etc/admin.yaml` 可加载，并与 `internal/config/config.go` 的 35 个 `*RPC` 字段一一对应 |
| `internal/handler` / `internal/types` | 0 | — | **无离线单测**（goctl 生成壳，见 §5） |
| `internal/svc` | 0 | — | **无离线单测**（客户端装配、etcd 发现、`OperationRPC` 未配置时的 503 退化路径只在 logic 用例里以「未配置即 fail-closed」间接出现） |

### 4. 替身层与断言口径

- 每个用例文件**自带**假客户端：内嵌 goctl 生成的 `*Client` 接口 + 只覆盖本文件需要的方法，
  未实现的方法继承接口、被调用即 panic，用来暴露意外的下游调用。
  **不建 gRPC 连接、不起 server、不碰数据库**，所以整套用例可在无网络环境秒级跑完。
- 断言对象只有两件事：请求怎么装配成 RPC 入参、回复怎么投影成后台 types（含信封四字段）。
  断言口径：投影类断**逐字段等值**（含 map/repeated 展平后的顺序，裁掉一位就红）；
  拒绝类断「零次下游调用」；operator 类断「只能由会话渲染」；0 值哨兵断「原样下传、网关不代填」。
- 证明不了的东西：下游是否接受该请求、领域判定（状态机/配额/金额/幂等/隐私级别/索引存在性）、
  真实 RBAC 授权结果、真实 HTTP 路由匹配与中间件链——那些分别是各 `services/*` 与运行期的结论。

### 5. 覆盖边界（不可省略）

- **网关覆盖永远不等于下游领域服务的覆盖。** 本目录 402 条 logic 用例钉的是「`.api` ↔ RPC 契约的
  装配与投影」，一条都没有穿过真实 gRPC。某条运营路由的行为是否可用，要看对应领域服务
  （如 `services/operation`、`services/cron`、`services/feature-store`）自己 README 的测试覆盖节，
  **不能拿这里的数字当端到端验证**。
- 未覆盖的 84 个构造器（§1）是真实缺口，不是「已被间接覆盖」；其中包含风控、审核、版权窗口、
  媒资转码等**写入口**。
- 权限点两条门禁（`route_permission_drift_test.go`、`seed_permission_test.go`）是**静态文本集合的
  双向比对**：它们能挡住「加路由忘登记」「种子落后于权限表」，但**不验证** `VerifyAdminPermission`
  的实际判定结果（那是 `services/operation` 的 RBAC 用例 + 真实库授权的责任）。
- 用例不连接 MySQL、Redis、etcd、MQ、Elasticsearch 或对象存储；本服务不拥有数据、
  没有 `deploy/migrations` 下的迁移，所以不存在「迁移 SQL ↔ 真实库列级对账」这层验证——
  该结论只说明本网关无库可验，**不等于**下游服务已在隔离实例（`127.0.0.1:3399`）复验过。
- `internal/handler`、`internal/types`、`*.pb.go`、`internal/svc` 等 goctl/protoc 生成壳不在单测范围。
- 前置条件提醒：本包能跑起来依赖 `services/membership/rpc` 的 descriptor 带仓库根相对路径
  （见「配置」节记录的 `membership.proto` 全局注册冲突），该条件不满足时 `internal/logic` 包
  会在 init 阶段失败。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./gateway/admin/...   # -p 1 必须带：Windows 页面文件限制，并发跑多个测试包会 OOM(errno=1455)
gofmt -l gateway/admin                      # 必须为空
go vet ./gateway/admin/...                  # 应无输出
```

`.api` 或权限点变更后按「生成与运行」节重新生成 + `node scripts/gen-api-docs.mjs`，
再跑上面三条；禁止手改 `internal/handler`、`internal/types`。本节只声明覆盖范围与口径，
不含任何一次运行的结论。
