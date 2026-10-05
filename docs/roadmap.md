# 开发路线图

## 阶段 0：骨架与契约

交付：go-zero 服务模板、网关健康检查、配置样例、错误码、trace、事件 envelope、数据库迁移规范、Compose 基础依赖、CI 基础检查。

验收：所有占位服务有职责 README；`go test ./...`、格式化和文档检查可在空实现仓库执行；没有商业化或小程序接口。

## 阶段 1：账号、UGC 投稿与播放闭环

交付：identity、content、media、playback、moderation、operation 的合并部署单元；账号登录、创作者资料、分片上传、转码、审核、稿件发布、播放签名和基础点赞/收藏。

验收：稿件状态机完整；失败可重试；播放不会暴露长期对象存储地址；所有发布事件可追踪。

## 阶段 2：社区和搜索

交付：comment、danmaku、social-graph、feed、notification、search-query；关注、动态、评论、弹幕、通知、搜索和历史记录。

验收：高频写入幂等；弹幕与评论隔离；黑名单可见性正确；搜索最终一致且可重建索引。

## 阶段 3：直播和版权目录

交付：live-room、live-ingest、live-media、live-gateway；catalog、rights；直播开关播、录制回放、作品/季/集、版权排期和自动下架。

验收：断流可恢复；回放重新经过审核；权利到期不会继续返回播放地址。

## 阶段 4：推荐和 SPM

交付：event-collector、spm、recommend-recall、recommend-rank、feature-store；行为采集、推荐特征、召回、排序、冷启动和降级。

验收：SPM 事件脱敏、可重放；推荐不可用时回退热门/关注流；不包含广告分析和商业化字段。

## 阶段 5：开放平台和规模化

交付：open-platform、服务独立扩缩容、索引/数据归档、跨可用区恢复、容量压测和客户端版本治理。

验收：OAuth 应用可撤销和限额；关键服务有 SLO、恢复目标和压测报告。

阶段之间不以“目录创建完成”为完成标准，而以 API、事件、权限、数据迁移、测试和运行指标全部具备为准。

## 实现进度（截至 2026-09-22）

已具备「protobuf 契约 + goctl 生成骨架 + `model`/迁移 + `internal/logic` 实现 + 网关接入」的服务：
（**注意：本表不含「单测已完成」的承诺**——logic 层的按方法用例覆盖见下方 2026-09-22 实测表，
43 个服务里 25 个为零；不要用本表推断某个服务已验证。）

| 范围 | 服务 | RPC 端口 / etcd key | 网关入口 |
|---|---|---|---|
| 阶段 0-1 | account、user-profile、creator、upload、asset、transcode、video、catalog、rights、moderation-orchestrator、moderation-worker、content-fingerprint | 见各服务 `etc/*.yaml` | `/api/*`、`/admin/*`（两个网关的路由总数见下一段：app 198 / admin 313） |
| 阶段 1 播放 | playback | 8102 / `playback.v1.rpc` | `POST /playback/token`、`POST /playback/heartbeat`、`GET /playback/session` |
| 阶段 1 运营 | operation | 8109 / `operation.v1.rpc` | `/admin/operation/*`（22 个路由 + `AdminPermission` 中间件） |
| 阶段 2 社区 | comment、engagement、social-graph、feed、danmaku | comment 8082 / `comment.v1.rpc`、danmaku 8103 / `danmaku.v1.rpc` | `/comment/*`、`/social/*`、`/feed/*`、`/engagement/*`、`/danmaku/*`（终端）；`/admin/comment/*`、`/admin/danmaku/*`（运营） |
| 阶段 2 通知 | inbox、notification | 8104 / `inbox.v1.rpc`、8108 / `notification.v1.rpc` | `/inbox/*`、`/notification/dnd*`（终端）；`/admin/notification/*`、`/admin/inbox/*`（运营） |
| 阶段 2 搜索 | search-indexer、search-query | 8106 / `searchindexer.v1.rpc`、8107 / `search-query.v1.rpc` | `/search/*`（终端）、`/admin/search/*`（重建、别名切换、健康巡检） |
| 阶段 1-2 风控 | risk-control | 8105 / `risk-control.v1.rpc` | `/admin/risk/*`（规则、名单、处罚、设备画像、调试） |
| 阶段 3 审计 | audit | 8110 / `audit.v1.rpc` | `/admin/audit/*`（检索、链校验、导出、保留策略、归档） |
| 阶段 3 运营配置 | ops-config | 8111 / `opsconfig.v1.rpc` | `/admin/ops/*`、`/admin/ops-config/*`（发布/回滚、灰度、专题/坑位、开关、缓存失效） |
| 阶段 3 调度 | cron | 8112 / `cron.v1.rpc` | `/admin/cron/*`（任务注册与启停、执行记录、游标、租约、手动触发与重试） |
| 阶段 3 直播 | live-room、live-ingest、live-media、live-gateway | 8119 / `liveroom.v1.rpc`、8118 / `liveingest.v1.rpc`、8120 / `livemedia.v1.rpc`、8121 / `livegateway.v1.rpc`（live-gateway 另有 WS 面 8122） | `/live/*`（终端，仅 live-room）；live-ingest/live-media/live-gateway 暂无运营路由 |
| 阶段 4 推荐与行为 | recommend-recall、recommend-rank、spm、event-collector、feature-store | 8116 / `recommendrecall.v1.rpc`、8123 / `recommendrank.v1.rpc`、8131 / `spm.v1.rpc`、8152 / `eventcollector.v1.rpc`、8130 / `featurestore.v1.rpc` | `/admin/recommend/*`（运营 17 条）、`/admin/collector/*`（运营 13 条）、`/admin/spm/*`（运营 15 条）、`/admin/feature-store/*`（运营 13 条）；无终端 HTTP 面（推荐链路与特征值只在服务间消费） |
| 阶段 3-5 私聊与开放平台 | private-message、open-platform | 8150 / `privatemessage.v1.rpc`、8151 / `openplatform.v1.rpc` | `/pm/*`（终端）；`/admin/open-platform/*`（运营 16 条）；open-platform 的**对外 OAuth / 开放 API HTTP 面仍无宿主**（只有 RPC） |
| 阶段 5 商业化 | membership、trade-order、payment、coin、creator-revenue | 8160 / `membership.v1.rpc`、8162 / `tradeorder.v1.rpc`、8161 / `payment.v1.rpc`、8163 / `coin.v1.rpc`、8164 / `creatorrevenue.v1.rpc` | `/membership/*`、`/order/*`、`/wallet/*`、`/coin/*`、`/creator/revenue/*`（终端）；`/admin/membership/*`、`/admin/order/*`、`/admin/payment/*`、`/admin/coin/*`、`/admin/creator-revenue/*`（运营） |

网关侧 RPC 覆盖率（43 个服务目录里 41 个已接网关客户端，共 577 个 RPC 方法，按 `svcCtx.<客户端>.<方法>`
调用点精确统计，2026-09-22 开放平台运营面接入后复算）：终端 + 后台可达 465 个；其余 112 个是**内部/控制面方法、服务间调用
或尚未接入的运营面**，不通过 HTTP 暴露（例：`payment` 的 `CreatePayment`/`GetPayment`/`ClosePayment`/
`RefundPayment` 由 `trade-order` 在下单与退款审批链路里调用，`event-collector` 的 `CollectEvents`/
`IngestServerEvents` 是服务间投递口，`recommend-*` 的 `RecallCandidates`/`RankCandidates` 由推荐链路调用）。
`content-fingerprint`、`moderation-worker` 两个目录尚无网关客户端。
`comment`、`creator`、`inbox`、`operation`、`risk-control`、`search-query`、`upload`、`video` 已 100% 可达。

商业化 5 个服务（阶段 5，2026-09-22 纳入）的范围裁定：广告投放、广告位分析、广告推荐**仍不在范围内**；
支付与充值只走沙箱台账，因此「开通即生效」是权益读侧对授予表的真实读结论，不是伪造成功。
需要真实资金渠道才能成立的能力——回调验签、退款到卡、提现、打款出金、对账文件、发票税务——
**刻意不开对外接口**，被调用时返回明确的 not-configured 错误；`creator-revenue` 的 `payout_state` 恒
`NOT_PAYABLE`、`payout_available` 恒 `false`。同理，订单的创建/取消/申退只属于买家（`gateway/app`），
后台只有退款审批与查单，避免出现「运营代客下单/申退」这条伪造用户意愿的路径。

**.api 补丁轮（2026-09-22）**：`gateway/admin/api/admin.api` 收口了 membership 域三处只存在于表单侧的
缺口——`ParamMembershipPlanUpsert` 补可选 `reason`、`ParamMembershipRevoke` 补 `plan_id`/`biz_order_no`/
`payment_no` 三个追溯位、revoke 的 `vip_type` 注释按实现改成「必填且必须 > 0」（服务侧 `requireVipType`
对 0 是硬错误，网关原样下传而不替调用方挑档位）。本轮只改网关契约与 logic 接线，五个商业化服务的
proto 与语义未动；`payment` 域的同类缺口（`GetWalletReply` 无 `found`、`PaymentPaymentItem` 不回传
`operator`/`remark`、`ParamPaymentRefundList` 无时间窗位、operator 列宽常量未导出）仍需先改 proto/`.api`
再动网关，逐条清单见 `gateway/admin/README.md`。

新接入的 14 个服务网关可达率（与上面「网关侧 RPC 覆盖率」同一口径，2026-09-22 复算）：audit 11/13、ops-config 17/20、
cron 18/23、private-message 14/15、live-room 18/21、live-ingest 13/22、live-media 23/28、
live-gateway 8/22、recommend-recall 9/10、recommend-rank 8/10、event-collector 13/15、spm 15/16、
feature-store 13/16、open-platform 16/24。
其中刻意不开放 HTTP 的方法：cron 的 `ListDueTasks`/`AcquireLease`/`RenewLease`/`ReleaseLease`/`ReportTaskResult`
（worker 控制面）、audit 的 `AppendAudit`/`BatchAppendAudit`（各领域服务内部写入）、
ops-config 的 `ResolveConfig`/`BatchResolveConfig`/`ResolveSlot`（读多写少，由各服务本地拉取）、
private-message 的 `ApplyModerationVerdict`（审核结论回写）、live-room 的 `ApplyRoomModerationResult`/
`ReportStreamState`/`AttachReplay`（审核回写与流状态由内部链路推进）、
spm 的 `WriteMetricWindow` 与 feature-store 的 `WriteFeatures`/`GetFeature`/`BatchGetFeatures`
（计算链路写回与在线热路径读，开后台口等于允许手工改指标/改特征，也会把在线配额吃在排障刷新上，
见 AGENTS.md §7）、live-gateway 的连接租约与心跳/转发族（信令面走 WS 与控制面内部调用）、
open-platform 的 `RegisterApplication`/`RegisterWebhook`（归属者注册，后台不代替开发者表达意愿）、
`IssueAuthorizationCode`/`ExchangeAuthorizationCode`/`RefreshAccessToken`/`IntrospectToken`/
`AuthorizeRequest`（用户凭证的签发、换发与探测，不是运营职权）与 `EnqueueWebhookEvent`
（回调事实由领域事件注入，手工入队等于伪造一次业务事件）。

| 未接入网关的 RPC | 服务 | 应由谁调用 |
|---|---|---|
| `AddExp3`、`AddMoral3`、`Relation3`、`Attentions3`、`Blacks3`、`Relations3`、`RichRelations3` | account | 老 `*3` 协议变体，v1 等价方法已接入；只为兼容保留 RPC |
| `RegisterAsset`、`UpdateAssetMeta`、`AddCover`、`ListCovers`、`AddSubtitle`、`ListSubtitles`、`AddScreenshot` | asset | upload/transcode 媒资管道与未来的索引/播放器字幕读取，阶段 3 接派发 |
| `SubmitTask`、`UpdateProgress` | transcode | 由上传/转码管道触发（`media.task.v1`），非端接口 |
| `SubmitTask`、`UpdateTaskResult`、`MatchByFingerprint`、`MatchByAsset` | content-fingerprint | transcode/审核管道与版权比对作业（`GetTask`、`ListTasks` 已在 `/admin/*` 暴露） |
| `RunOCR`、`RunASR`、`RunImage`、`RunAudio`、`GetTaskResult` | moderation-worker | moderation-orchestrator 派发（见下方“审核闭环”） |
| `SubmitForReview`、`SubmitWorkerResult` | moderation-orchestrator | 领域服务提交审核（danmaku 已接入）、worker 回写结果 |
| `ApplyModerationResult` | danmaku | moderation 结论回写（审核闭环缺口的另一端） |
| `SendNotification` | notification | 各领域服务经 `notification.request.v1` 触发；不给任何 HTTP 面开群发口子 |
| `PushFeed` | feed | 关系/内容事件驱动的写入投影 |
| `MultiStats`、`UpdateCount`、`RawStat` | engagement | 计数写回与批量投影（消费者/任务） |
| `ItemLikes` | engagement | “谁赞了”属隐私敏感读，需先定权限模型再开放 |
| `IsFollowedBatch` | social-graph | BFF 列表批量补关注态时再接，当前无对应路由需求 |
| `ListSeasons`、`ListTags` | catalog | 端侧目录浏览增量，按客户端需求再开路由 |
| `GetContract`、`GetWindow` | rights | 后台详情读；`/admin/rights` 的 list 接口已覆盖当前控制台需要 |
| `CheckPlayable` | rights | playback、catalog 服务侧已直连调用，不经网关 |
| `ListExpiring` | rights | `services/cron` 到期扫描（阶段 3） |
| `UpsertContentDoc`、`DeleteContentDoc` | search-indexer | 索引文档由 `content.published.v1` 消费者写入 |
| `VerifyPlaybackToken` | playback | CDN 回源校验，服务端到 CDN |
| `Members`、`NickUpdated`、`SetNickUpdated` | user-profile | 批量内部读与改名簿记 |
| `SetOfficialDoc`、`SetRank`、`AddUserMonitor`、`IsInMonitor` | user-profile | 认证资料与监控名单是运营写操作：认证需经 moderation，监控名单能力已由 `risk-control` 名单条目覆盖，避免同一能力两个所有者 |

**审核闭环尚未接通**（当前只具备契约与两端实现，缺中间派发）：
`moderation-orchestrator` 的 `ModerationWorkerRPC` 配置项已存在但无消费方（`ServiceContext` 未持有 worker client），
`SubmitForReview` 只落任务不派发；`SubmitWorkerResult` 落库后 `moderation.result.v1` 事件仍是
`// TODO(event)`，`video`/`comment`/`danmaku` 也没有对应消费者，因此稿件与弹幕/评论的机审推进只能靠
`/admin/moderation/*` 人工裁决或 `/video/submissions/:aid/transition` 的合法状态迁移。
上述缺口在阶段 3 与 `services/cron`、事件发布器一并补齐；**不得**为凑网关覆盖率把这些方法开成 HTTP 面。

**搜索读写契约尚未对齐（阻塞真实检索上线）**：`search-indexer` 写入的索引 mapping 与 `search-query`
读取侧使用的字段词汇是两套，读服务目前无法在 indexer 建的索引上跑通。差异集中在
`doc_type`↔`content_type`、`doc_id`↔`content_id`、`intro`↔`description`、`zone_id`↔`typeid`、
`pub_time`↔`publish_at`、`hot_score`↔`heat.heat_score`、各计数（`view_count`/`like_count`/`danmaku_count`）↔`heat.*`，
`state` 一边是字符串一边是枚举整数；`fans_count` 与 `doc_type=user` 的账号文档索引侧完全没有生产者。
别名同样不一致：读侧示例曾指向 `go_video_search_read`，而全仓只有 `search-indexer` 维护
`OpenSearch.IndexPrefix`（`go_video_content`），示例已临时改为同名，但它仍是过渡而非结论。
对齐方案（改读侧 DSL 与投影、或扩展写侧 mapping 与 user 文档流水线）需要先评审
`searchquery.v1` 与 `content.published.v1` 两个契约再落地，属于阻塞项而非可选优化；
`content.published.v1` 的生产侧已在 2026-10-05 接上 `video`，因此对齐时**改的只能是 search-indexer 的
投影与读侧 DSL**，不能让 video 递增 `schema_version` 去迁就索引（payload 字段只增不改，见
`docs/api-and-events.md` §5）。详细字段对照见 `services/search-query/README.md` 的“索引契约”一节。

阶段 3-5 的 14 个服务（`live-room`/`live-ingest`/`live-media`/`live-gateway`、`recommend-recall`/`recommend-rank`、
`spm`/`event-collector`/`feature-store`、`open-platform`/`private-message`/`ops-config`/`audit`/`cron`）
已不再是空目录：每个都有 `rpc/*.proto` 契约、`model/`、`etc/*.yaml`（含 `Validate()` 与
`internal/config/config_load_test.go`）、可运行入口、`deploy/migrations/<svc>/` 迁移（已在隔离实例
`127.0.0.1:3399` 逐个 `up` + `status` 复验为 `applied`），并接进 `gateway/admin` 或 `gateway/app`。
**但 `internal/logic` 的证据强度不齐**：全部服务的 logic 都已落地（logic 层已无
`model.ErrNotImplemented` 空桩，也不返回伪造成功的空 Reply；仍会返回该哨兵的只剩三处显式降级：
`recommend-rank/internal/repository/downstream.go` 的未接线替身、`account/internal/repository/exp_moral.go`
在 user-profile client 未注入时的分支、`cron/internal/registry/registry.go:285` 对空结果的防线），
**覆盖重心已经转移**。2026-09-22 用「每个 `<X>Logic` 类型是否有 `New<X>Logic(` 用例」探针逐目录实测
得到 228/588，并记有「23 个服务零覆盖」；2026-10-03 用**同一探针**复测，43 个领域服务全部收口为
**588/588**，那条结论已不成立。2026-10-04 给 `social-graph` 新增 `RichRelations`（见下表与
`services/social-graph/README.md`），同一探针为 **589/589**；同日接 live-media 的 `live.state.v1` 消费者时
补了**非 RPC** 的 `OfflineSessionOutputs` logic（28 个 RPC 方法之外第 29 个构造器），逐目录重测为
**590/590**，下表只有 `live-media` 一行随本轮变化（28/28 → 29/29，11 / 241 / 50 → 12 / 251 / 53）。
2026-10-05 接 `upload` 的 `media.task.v1` 发布器时，构造器数不变（仍 5/5），但为「四写同事务、事件行内容、
取消不产事件」补了顶层用例，`upload` 一行 7 / 54 / 12 → 7 / 59 / 12，合计顶层随之 4669 → 4674
（子用例探针 `grep -ho 't\.Run(' services/*/internal/logic/*_test.go | wc -l` 当场复测仍是 1864，未变）。
计数口径沿用上一轮：顶层用例数不含 `TestMain`（`account`/`cron`/`live-gateway`/`operation` 四个目录各有一个，
用 `grep -cE '^func Test'` 裸数会各多 1）。
探针口径不变：只认「用例是否直接构造 logic」，所以它是覆盖**上界**
而不是下界：断言强度、有没有连真依赖、用例是否真在跑，都要看下面的三条补充。

| 目录 | 构造器 | logic 文件 / 顶层 / 子 | 目录 | 构造器 | logic 文件 / 顶层 / 子 |
|---|---|---|---|---|---|
| account | 30/30 | 31 / 250 / 59 | moderation-worker | 5/5 | 3 / 27 / 0 |
| asset | 10/10 | 11 / 76 / 18 | notification | 11/11 | 12 / 79 / 0 |
| audit | 13/13 | 7 / 86 / 58 | open-platform | 24/24 | 10 / 147 / 128 |
| catalog | 12/12 | 5 / 55 / 5 | operation | 22/22 | 5 / 81 / 26 |
| coin | 10/10 | 6 / 91 / 29 | ops-config | 20/20 | 11 / 108 / 43 |
| comment | 7/7 | 8 / 57 / 20 | payment | 14/14 | 6 / 121 / 25 |
| content-fingerprint | 6/6 | 7 / 48 / 0 | playback | 4/4 | 7 / 57 / 23 |
| creator-revenue | 16/16 | 12 / 201 / 69 | private-message | 15/15 | 8 / 65 / 5 |
| creator | 8/8 | 6 / 73 / 16 | recommend-rank | 10/10 | 2 / 6 / 0 |
| cron | 23/23 | 9 / 133 / 85 | recommend-recall | 10/10 | 9 / 105 / 24 |
| danmaku | 9/9 | 10 / 120 / 69 | rights | 9/9 | 3 / 43 / 0 |
| engagement | 16/16 | 18 / 138 / 23 | risk-control | 11/11 | 7 / 74 / 0 |
| event-collector | 15/15 | 3 / 7 / 0 | search-indexer | 7/7 | 9 / 93 / 28 |
| feature-store | 16/16 | 9 / 116 / 46 | search-query | 8/8 | 9 / 63 / 42 |
| feed | 8/8 | 8 / 71 / 0 | social-graph | 14/14 | 15 / 109 / 46 |
| inbox | 7/7 | 10 / 54 / 14 | spm | 16/16 | 5 / 39 / 126 |
| live-gateway | 22/22 | 14 / 250 / 230 | trade-order | 12/12 | 13 / 168 / 72 |
| live-ingest | 22/22 | 12 / 191 / 30 | transcode | 7/7 | 8 / 72 / 13 |
| live-media | 29/29 | 12 / 251 / 53 | upload | 5/5 | 7 / 59 / 12 |
| live-room | 21/21 | 21 / 362 / 198 | user-profile | 35/35 | 28 / 232 / 116 |
| membership | 16/16 | 7 / 136 / 56 | video | 8/8 | 8 / 77 / 37 |
| moderation-orchestrator | 7/7 | 9 / 83 / 20 | — | — | — |
| **合计（43 服务 / 590 个 logic 类型）** | **590/590** | **420 / 4674 / 1864** | **gateway/app、admin** | **75/198、229/313** | 37 / 519 / 55 |

这张表之外有三条补充，缺一条就会把它读成「已验证」：

1. **logic 以外的层远没有收口**（同一轮实测，按有测试文件的服务数 / 43 计）：
   `model` 23、`internal/repository` 13、`internal/consumer` 5（`inbox`/`notification`/`search-indexer`/`live-room`/`live-media`，
   最后一个 2026-10-04 接线，见 A 组第 4 条）、
   `internal/publisher` 4（`live-ingest`/`playback`/`live-media`/`recommend-recall`，各 5 个测试文件，见 A 组第 1 条）、
   `internal/policy` 3（`danmaku`/`notification`/`risk-control`）、`internal/svc` 3、
   `internal/config` 43/43。也就是说**20 个服务的 SQL、列名与索引命中没有任何自动化证明**，
   它们只由「迁移 ↔ model 的程序化列级比对」和隔离实例 `127.0.0.1:3399` 的建库复验兜着；
   逐服务分列见各服务 README 的「测试覆盖」节（本轮已把 43 个服务的这一节补齐）。
2. **`t.Skip` 要按两类读**（2026-10-04 实测：全仓 `*_test.go` 里共 8 处 `t.Skip`/`t.Skipf` 调用点，
   口径是 `\bt\.Skip(f?)\(`，别用裸串 `t.Skip`，那会把 `st.Skipped` 这类字段读成调用点而多数）。
   普查方法是「先静态枚举全部调用点，再对**含这些调用点的包**逐个 `go test -p 1 -count=1 -v`」，
   SKIP 行只可能来自这些点，所以不必整仓 `-v`。
   普查包（`.gotmp/gates/6-skip-census.txt`：live-gateway logic / live-ingest logic / live-media logic /
   notification policy / event-collector model / common/netutil，补跑 live-room consumer）
   合计 `--- SKIP` 3 行、`--- FAIL` 0 行，且整树 `go test -p 1 -count=1 ./...` rc=0：
   **缺陷哨兵 3 处（本轮全部实测触发）**——都在 `live-gateway`
   （`broadcast_logs_read_test.go:162` 1 条、`forward_paths_test.go:481`/`:959` 2 条），
   每条 `Skip` 的第一参数就是缺陷定位；2026-09-22 那轮 `live-gateway` 曾有 13 条，收口到 3 条的过程、
   以及剩余每条为什么不能只改代码，见 `services/live-gateway/README.md` 已知缺口 14、15。
   `live-ingest` 原有的那条（`streamstate_test.go` 的 `TestStreamStateMachine_StoppedClearsNodePointer`）
   **已于 2026-10-04 随缺陷 #1 修复去掉 `t.Skip`**，现在是真的在跑并钉住「停流必须清 `live_stream.node_id`」。
   **环境/条件跳过 5 处（本轮实测一条都没触发）**——`notification` 2（`policy/backoff_test.go:93`、
   `policy/dnd_test.go:17`，本机有 tzdata 所以不跳）、
   `common/netutil` 1（无非 loopback IPv4 网卡时跳过，本机有网卡所以不跳）、`event-collector/model` 1
   （迁移目录在仓库外时跳过，本机在仓库内所以不跳）、
   `live-media/internal/logic/fakes_test.go:146` 1（`requireNoEnvelopeBug` 的条件门，
   只有当失败原因正是缺陷 #1 才 Skip，且探针对照会用 `t.Fatalf` 拦住「缺陷已修却还隐身」；
   缺陷 #1 已修，本轮普查该包 `--- SKIP` 为 0）。后一类不是缺陷隐身，但**它会不会在某台机器上变成静默通过，
   取决于该机器的 tzdata/网卡/目录布局**，所以跨环境复跑时必须看 `-v` 里的 SKIP 行而不是只看退出码。
3. **剩下的真缺口在网关**：`gateway/app` 只有 75/198 个 logic 类型有直接用例（未覆盖 123 个，集中在
   上传、投稿、互动/收藏、会员、钱包、实名、搜索、弹幕与动态聚合面），`gateway/app` 与
   `gateway/admin` 合计 304/511。网关用例的形态是「打桩下游 client + 断言回复投影与错误不回显」，
   不建 gRPC 连接也不碰数据库，所以**网关侧的覆盖数不等于下游领域服务的覆盖数**；
   未覆盖方法名按域分组列在 `gateway/app/README.md`、`gateway/admin/README.md` 的「测试覆盖」节。
历史上「零覆盖」的那批服务（`account`/`user-profile`/`operation`/`live-room` 等）卡在同一个原因：
`internal/repository` 把数据访问包成具体类型且字段未导出，logic 用例无处塞替身。
`catalog`、`rights`、`playback` 三轮用最小改动解掉了它（`Cacher` 接口 + `NewWithDeps`，
生产路径仍只走 `New`，见这三个服务的 README），其余服务照此办理，本轮已铺满 43 个服务。
计数口径的教训保留在这里防止再次误读：`live-gateway` 的测试文件在 2026-09-22 之前**从未通过编译**
（占位符 `TraceIdUnused()`、`contextLikeAlias` 未定义），却被计入「有单测」——
`go build ./...` 和 `gofmt` 都不编译 `_test.go`，所以这条计数只有配 `go vet ./...` + `go test -p 1 ./...`
才可信。「有 N 个测试文件」既不等于在跑，也不等于断言都成立。
**同一个坑在 2026-10-03 又抓到一次**：`services/video/internal/logic` 的配对文件
`update_delete_logic_test.go` 与两个分方法文件有 3 个同名 `Test…` 加同名包级变量 `updateSuccessSeq`，
并且 `deleteSeq(101, tc.state)` 把 `int32` 当 `int64` 传，整个测试包从写下那天起就没编译过，
却被导出计成 `14/5` 个用例。处置见该服务 README「测试覆盖」节第 1 组：换名解冲突、
补 `int64()` 转换，**一条断言都没有删**（其中同名不等于同判定——配对文件打的是「五字段全给回原值」，
分方法文件打的是「空补丁」，两侧都保留）。改完 `go vet ./services/video/...` 无输出、
`go test -p 1 -count=1 ./services/video/...` 四个包全 `ok`。
这条也是为什么本轮所有 README 数字都改成「由 `grep` 实测导出、并且必须配 vet+test 才算证据」。
**同一个坑的第三种形态在 2026-10-03 当晚被抓到**：三个包的用例既编译得过、也计得到数，
但从未作为整包执行过，于是整树 `go test -p 1 -count=1 ./...` 第一次跑就把它们跑红，
首轮失败断言 10 条（当时的证据打在 `.gotmp/gates/4-test-full.txt`，已被复跑覆盖成空文件；
下面的定位来自当轮逐条复核，不是从日志回收）——
`gofmt`/`go build`/`go vet` 当时全部 rc=0，所以前三道门禁对此毫无察觉。
处置落在 12 处断言/期望点：`cron` 1（`lease_checkpoint_health_test.go:532` 期望写成裸字符串而实际值是切片形态）、
`operation` 6（五处 RBAC 快照 key 写成斜杠 `op:rbac:0/1`，生产是冒号 `op:rbac:%d:%d`，
见 `internal/repository/cache.go:22`；另一处多写了 `cache.Incr:op:menu:ver`，
而 `menuVersion` 只在首次分配版本时 Incr，见 `internal/repository/menu.go:73-81`）、
`live-ingest` 5（`revoke_tmp_test.go` 把「读密钥的探针调用」混进期望序列两处、
`revoke_tmp_test.go:329` 的副作用向量期望与「第二次调用零写入」的现实不符、
`seedRevokeFixture` 的 `seedNode` 覆盖行导致配额被抹掉、
`closestream_test.go:515` 按 `StopReason` 枚举而非 `model/streaminterruption.go:154` 的
`CONCAT` 语义期望中断原因、`healthreport_test.go:456` 的 seed 助手在逐个采样调用时撞 `uniq_report_id`）；
另加替身层自身一处缺陷：`fakeOutbox.ListByState` 的截断条件写反，
使读侧的 limit 断言一直是被削弱的（已按 `model/common.go:34` 的 `clampLimit` 复刻修正）。
**这 10 条全部判为断言侧或替身侧写错，一条都没有删、没有放宽**；逐条处置与各条现在钉住的真实形态，
写进这三个服务 README 的「测试覆盖」节（cron §9.5 第 6 条、operation §5 第 4 条、
live-ingest §5 开头的「整树 test 复核」）。要点是：**「本服务用例数」不等于「本服务用例跑过」，
逐包计数与逐包 `go test ./services/<x>/...` 都不足以收口，收口必须以整树 `go test -p 1 ./...` 为准**。
同一条口径下 `common/ratelimit` 的 `TestCoDelPushPop` 在 2026-09-22 的整树 `go test` 里首轮必失败
（写法是「先 `go Pop()` 再 Push」，而 Pop 在队列空时会立刻返回），已改成「后台 Push、确认报文入队后再 Pop」
并在该用例上连跑 25 次复验；但**它钉住的竞态本身仍在实现里**：`Pop` 用非阻塞发送通知 `Push`，
若这一下发送早于 `Push` 进入等待 select，判定会被丢掉，并且那个 channel 会被 `Pop` 归还进 `sync.Pool`
——而被丢掉的 `Push` 协程还持着同一个 channel 的引用，等它从 pool 里被下一次 `Push` 取用时，
两个等待方会互相串读通知。生产上表现为一次多余的超时等待，属于已知缺陷而非待写代码，
修它要连带改 `Push`/`Pop` 的通知协议，本轮未动。
同轮 `common/counter` 的滑动窗口用例也是这类假失败：它把 100 次 5ms tick 塞进刚好 500ms 的窗口，
负载下 tick 被拖慢（实测整包 1.96s）早期累加就滑出窗口，已改为窗口远大于累加时长并**收紧**成精确断言
（`Value()==100`，而不是放宽 80~100 的容差）——跨桶推进仍在被测路径上。
`event-collector` 的 Outbox 投递宿主、`live-media`/`live-gateway` 的媒体与信令流水线归属也仍未定，
属于设计待定项而不是待写代码。

跨服务契约缺口（接入时按 `// 契约缺口` 注释留在调用点，需在阶段 3 前评审）：
rights 无批量窗口校验且 `content_id` 粒度固定；rights/asset 的“不存在”只通过错误文本表达；
asset 时长单位是毫秒而 catalog 是秒；`video_version.asset_id` 是字符串而 asset/transcode 的 asset_id 是 int64；
user-profile 传明文 `real_ip` 而 risk-control 存 `ip_hash`；
operation 的权限点目录已改为受测种子：`deploy/migrations/operation/000004_seed_op_permission.sql` 登记
`routePermissions` 要求的全部 162 个 `(resource, action)` 权限点（29 个域；166 条受保护路由命中它们，
其中四对路由共用同一格），种子与中间件表的双向一致性由
`seed_permission_test.go` 把守，生成路由与表的漏登记/死条目、受保护 GET 的例外点名由
`route_permission_drift_test.go` 把守
（权限点清单仍以 `gateway/admin/internal/middleware/adminpermissionmiddleware.go` 的 `routePermissions` 为准）。
**角色绑定已由派生种子补上**：`000005_seed_op_role_grants.sql` 与 `000006_seed_op_role_grants_stage12.sql`
从 `op_permission.domain`/`action` 现算出 29 个域角色 + `readonly` + `super_admin`（隔离实例重放：31 角色 /
340 绑定 / 0 个权限点无人可挂 / 重放第二遍计数不变），一致性由
`services/operation/model/role_seed_test.go` 把守（含「权限点种子编号不得晚于角色绑定迁移」这条顺序耦合）。
它刻意不写 `op_admin_role`——**剩下的缺口是「谁拿哪个角色」**：这一步仍要运营人工做（且 `domain_operation`
含建角色/授权权限点、`domain_inbox` 能群发站内信、`domain_risk` 能改风控规则与名单，都不能当日常岗默认角色），
`operation.v1` 也还没有权限点目录契约可供页面枚举。
`feature-store` 已有契约，但与消费侧对不上：rank 的 `FeatureBatchSize=128`（`recommendrank.v1.yaml:54`
按此切分候选 aid 批量下发）超过 feature-store 单次批量读的主体数上限 `model.MaxBatchEntities = 20`
（`services/feature-store/model/types.go:501`，超出直接 `ErrTooManyEntries`；特征键数上限是 50），
接线后 rank 必须按 20 再切一层，否则第一笔真实调用就会失败；
`FeatureVector map[string]float64` 把 STRING/BOOL/LIST/VECTOR 四类特征压平、PGC 实体作用域未定义、
没有 `feature_config_version` 包版本概念、缺少新鲜度（staleness）字段。
`recommend-*`/`spm`/`event-collector` 三者的特征与事件字段口径同样待评审。
直播分区字典的所有权重复：`live-room.live_area` 与 `catalog.catalog_zone`（注释即「内容分区表」）、
`ops-config` 的运营标签目录三者语义高度重叠，当前按 proto 既成事实把直播分区放在 live-room
（`live_room.area_id` 引用 `live_area`，且迁移不写种子数据），是否合并为一个 root 需维护者裁决
（详见 `services/live-room/README.md` 的已知缺口）。
`open-platform` 的对外 HTTP 面（`/oauth/authorize`、`/oauth/token`、`/open/v1/*`）**还没有宿主**：
这些是第三方开发者入口，既不属于终端 BFF `gateway/app` 的职责，也不该放进只服务管理后台的 `gateway/admin`；
按 AGENTS.md §3 顶层不允许新增 `interfaces/` 之类的替代目录，所以需要评审「为 open-platform 单独 goctl 生成一个
`gateway/open` 子服务」还是「明确挂到 `gateway/app` 并单列限流与鉴权链」，定下来之前开放平台的 RPC 只能内网调用。

## 发布阻塞项（2026-10-03 逐条重新复核，按域分组）

这一节的每条都在本轮用 `grep`/`go build` 当场重新导出，不是引用上一轮的记录；每条都带 `file:line`。
**分组轴是「阻塞的原因」**，不是服务名，因为同一个服务的多条阻塞要分开排期。

### A. 事件面：`live.state.v1` 与 `content.published.v1` 两端都在代码里，`playback.heartbeat.v1`、9 条 `livemedia.*.v1`、`recall.pool.published.v1` 与 `media.task.v1` 有生产者无消费者，其余事件仍缺生产者

1. **MQ 生产者六处（前三处 2026-10-04，后三处 2026-10-05）**：`services/live-ingest/internal/publisher` 用
   `kq.NewPusher(..., kq.WithSyncPush())` 把 `live_ingest_outbox` 同步投到 `live.state.v1`，
   由 `svc.startPublisher()` 按 `Kafka.Enabled` 启动，代码在 `-tags liveingest_kafka` 后面。
   同日新增第二处：`services/playback/internal/publisher` 投 `playback.heartbeat.v1`
   （`-tags playback_kafka`），它**不重写循环**：顺序、退避、判死、写库失败中断这些
   与业务无关的决策抽到了 `common/outbox`（`outbox.New`/`RunOnce`/`Start`/`Stop` +
   `CheckRow` 的列↔payload 反查），服务内只剩「列映射」「topic 归属」「配置→参数」三段适配。
   同日新增第三处：`services/live-media/internal/publisher` 投 9 个 `livemedia.*.v1`
   （`-tags livemedia_kafka`），同样是 `common/outbox` 的使用方，与 playback 的差别只有两条：
   `PublishTopics` 的校验是**与 model 的 9 个 `EventType*` 派生集合相等**（少一条 → 该类事件
   每轮撞「没有发送通道」直到判死），以及条件 UPDATE 命中 0 行记日志而不中断本批
   （无租约列的多副本部署里这是正常让位，见 `services/live-media/README.md` §6.1）。
   2026-10-05 新增第四处：`services/recommend-recall/internal/publisher` 投 `recall.pool.published.v1`
   （`-tags recommendrecall_kafka`），仍是 `common/outbox` 的使用方，与前三者的差别有三条：
   本服务的 `recall_outbox.ListPending` 对 `limit <= 0` 或 `> model.MaxOutboxBatch` 是**返回错误**
   而不是钳制（live-media 钳到默认 100），所以 `ValidatePublishKafka` 多一道
   `Kafka.BatchLimit <= model.MaxOutboxBatch` 的构造期门禁（每轮报错的发布器等于没接）；
   `PublishTopics` 的校验是**逐条必须等于 model 派生出的唯一 topic**（多写一条即拒，防止白占通道还让人
   以为"这个 topic 有人在发"），而 topic 与事件类型常量的同源由
   `TestRequiredTopicsCoverDeclaredEventTypes` 从源码扫 `model/*.go` 的 `Event*` 常量来钉
   （找到 0 个或 1 个以外的值都判失败，避免"没有事件类型所以全部通过"）；
   发布时间落在 `mtime` 而不是独立的 `published_at` 列（表里没有那一列）。
   **接线时发现并修掉一处读侧缺陷**：`recall_outbox.ListPending` 原谓词是
   `state IN (待发布, 失败)`，死信每轮被重新取出重投，现收紧为 `state = 待发布`
   （`model/outbox.go:118`），由 `model/outbox_sql_test.go` 的
   `TestListPendingSelectsOnlyPendingDueRows`（SQL 出现 `state IN` 即失败）与
   `TestMarkFailedOnlyFromPending` 钉住。
   2026-10-05 新增第五处：`services/upload/internal/publisher` 投 `media.task.v1`
   （`-tags upload_kafka`），`common/outbox` 的第四个使用方，三条只有它才有的形状：
   事件行与状态推进**在同一个 `TransactCtx` 里**（`internal/repository/repository.go:288`~`:300` 一次写
   「回填 md5 + 回填 asset_id + 会话 `COMPLETED` + 事件行」，缓存在提交之后才刷，所以「已 COMPLETED 却无事件行」
   在正常路径不可达）；`PublishTopics` 的校验是**必须恰好等于派生出的唯一 topic**（与 recommend-recall 同一条口径）；
   分区键取 `upload_id`（聚合类型 `upload_session`），于是同一会话的媒资事件必然同分区。
   代价也如实登记：`upload_outbox` 的建表脚本是本轮新加的（`deploy/migrations/upload/000003`），
   **从未在任何实例执行过**，
   所以在新代码里「表还没建」会让 `CompleteUpload` 整条路径失败（原来只是不发事件、状态照推），
   上线顺序必须是先 `up` 迁移再发代码，见 `deploy/migrations/README.md` 的 `pending` 标记。
   2026-10-05 新增第六处：`services/video/internal/publisher` 投 `content.published.v1`
   （`-tags video_kafka`），`common/outbox` 的第五个使用方，四条只有它才有的形状：
   它是唯一**按「可见性判定表」决定要不要写事件行**的发布器（`internal/repository/contentevent.go:64 contentActionFor`：
   只有 `→PUBLISHED/OFFLINE/EXPIRED` 与「来源态公开过」的 `→DELETED` 产事件，`DRAFT→UPLOADING` 一类中间态一行都不写，
   所以「状态推进了但没有事件」在这条链上是正常结论而不是漏投）；
   事件行、状态、审计**三写在同一个 `TransactCtx` 里**（`internal/repository/repository.go:184`），
   且 `DeleteSubmission` 与 `TransitionState` 复用同一条仓储路径；
   payload 的 `doc_revision` 与同批 `video_audit_log.ctime` 同源（秒 × 1000），
   消费方 `search-indexer` 缺这个键就判契约违反（`internal/consumer/mapping.go:168`）；
   分区键取 `aid`，因此同一稿件的上下线必然同分区按 `id` 升序。
   判定表本身由 `internal/logic/content_event_logic_test.go` 的 `TestActionMappingCoversEveryLegalEdge`
   逐条遍历 `legalTransitions` 的每一条边钉住（期望值在测试里独立重写，不照抄实现），
   状态机加一条边而没回答「这条要不要通知索引」就会红。
   代价与 upload 同形且更宽：`deploy/migrations/video/000004_create_video_outbox.sql` **从未在任何实例执行过**，
   未执行时**发布与下架转换整笔回滚**（不只是丢事件），先迁移再上线是硬顺序。
   本条也是全仓第二条「两端都在代码里」的链路（消费者 `search-indexer`、`inbox` 早已存在），
   但两侧同样从未与 broker 联调，且 video 不产出热度事实，索引文档的 heat 只能等
   `engagement.action.v1` 的生产者（第 5 条）打补丁。
   因此「事务内写业务数据 + Outbox，再由发布器投递」这条链的最后一跳**在这六条事件上已经存在**，
   但除它们以外仍然是 0：`grep -rnE 'NewPusher|kq\.Pusher' services common gateway` 的非测试命中
   只有 `live-ingest`、`playback`、`live-media`、`recommend-recall`、`upload`、`video` 六个 `kafkaruntime_kafka.go`
   （外加各自 `params.go`/`config.go` 里的注释），其余 topic 仍不会离开进程。
   六者都**没有 broker 侧证据**，且 `playback.heartbeat.v1`、9 个 `livemedia.*.v1`、
   `recall.pool.published.v1` 与 `media.task.v1` 连仓库内消费者都没有（见第 5 条）。
2. **8 个服务会写事件 Outbox 行**：`playback`（`internal/repository/repository.go:123`）、
   `search-query`（`internal/repository/querylog.go:66`）、`user-profile`
   （`internal/repository/outbox.go:83`）、`live-media`（`internal/logic/helpers.go:817`）、
   `live-ingest`（`internal/logic/streamstate.go:307`）、`recommend-recall`
   （`internal/logic/poolswitch.go:266`）、`upload`（`internal/repository/repository.go:300`，
   在 `CompleteUpload` 的 `TransactCtx` 回调内，`:288` 起）、`video`
   （`internal/repository/repository.go:184`，与状态推进、审计日志同一个 `TransactCtx` 回调内，
   2026-10-05 接上）。
   `feed` 不算：它写的 `feed_outbox` 是「动态主表」（作者个人发件箱），列里没有
   `event_id`/`event_type`/发布状态，与事件 Outbox 同名不同物（`services/feed/internal/logic/pushfeedlogic.go:37`）。
3. **发布器七处，语义差别要分清**：`user-profile` 的 `r.outbox.Start()`
   （`internal/repository/repository.go:110`）投递目标不是 MQ，`deliver` 把 `profile.updated` 路由到
   account 的 `DelCache` RPC（`outbox.go:194-210`），节操通知只写日志（`outbox.go:212-222`），
   而且它自己那套循环用 `_ = p.model.MarkFailed(...)` 吞掉写库错误（`:172`、`:181`）；
   `live-ingest`、`playback`、`live-media`、`recommend-recall`、`upload`、`video` 的 `internal/publisher` 才是本仓真正的 MQ 发布器，
   后五者是 `common/outbox` 的使用方（playback 第一个、live-media 第二个，都在 2026-10-04；
   recommend-recall 第三个、upload 第四个、video 第五个，都在 2026-10-05）。
   其余 2 处（`search-query` 的 `search_outbox`，加 `user-profile` 的 `user.moral.notice`）的事件行
   **没有任何 MQ 读取方**，`MarkPublished` 永远不会被调用。
   读侧缺陷已随接线全部修完，本轮不再有"待修"项：`recall_outbox`（`model/outbox.go:118`）的 `ListPending`
   原条件是 `state IN (待发布, 失败)`，判死的行会被每轮重新投出去，死信等于没有死信；
   2026-10-05 接线时改成 `state = ?`（只取待发布），并由 `model/outbox_sql_test.go` 的
   `TestListPendingSelectsOnlyPendingDueRows` 钉住「SQL 里出现 `state IN` 即失败」，
   堆积可见性交给刻意不同候选集的 `CountPending`（`:203`，含失败态）与 `CountStuck`（`:216`）。
   五张表的取行条件现在都是 `state = 待发布`：`live_ingest_outbox`（`model/outbox.go:138`）、
   `playback_outbox`（`model/outboxmodel.go:103`），以及本轮修掉的 `live_media_outbox`
   （`model/live_media_outbox.go:186`）：它在 2026-10-03 的登记里还是 `state IN (0, 2)`
   （即本条原描述的第二个缺陷），接线时改成了 `state = ?` 并由
   `model/live_media_outbox_sql_test.go` 的 `TestListPendingSelectsOnlyPendingDueRows` 钉住，
   判死行的可见性改由刻意不同候选集的 `CountPending`（`:260`，含失败态）承担。
   新加的 `upload_outbox` 从一开始就是 `WHERE state = 0 AND (next_retry_at = 0 OR next_retry_at <= ?)`
   （`model/uploadoutboxmodel.go:103`），但**没有等价的 SQL 文本门禁**：`services/upload/model` 里没有
   任何 `_test.go`，所以这一条只能靠读代码确认，补 `upload_outbox_sql_test.go` 与另四张表对齐是后续项
   （`services/upload/README.md` §「测试覆盖」的 model 行按同一口径声明）。
   第六张 `video_outbox` 是同一种缺口，也是同一句取行条件（`services/video/model/videooutboxmodel.go:104`
   的 `WHERE state = 0 AND (next_retry_at = 0 OR next_retry_at <= ?)`），`services/video/model` 同样
   没有任何 `_test.go`；它的候选集正确性目前只由 `internal/publisher` 的替身层用例从**调用方**一侧
   钉住（`TestRunOnceThroughRealStore` 断言一轮发布对 model 的调用序列恰好是
   `ListPending,MarkPublished:<id>`），SQL 文本本身没有门禁，与另五张表对齐同样是后续项
   （`services/video/README.md` §「测试覆盖」按同一口径声明）。
4. **消费侧存在 5 个 `internal/consumer`**：`inbox`、`notification`、`search-indexer`、`live-room`
   （2026-10-04 新增，是 `live.state.v1` 的入站端，也是本仓第一条端到端事件链）、`live-media`
   （同日新增，同一条 `live.state.v1` 的第二个入站端：主播停播即整场档位下线，见本条最后一个子项）。
   五者的 Kafka 运行时都按构建标签开关（`-tags inbox_kafka` / `-tags notification_kafka` /
   `-tags searchindexer_kafka` / `-tags liveroom_kafka` / `-tags livemedia_kafka`），默认构建显式返回「未链接运行时」而不是伪造成功；本轮
   `go build -tags inbox_kafka ./services/inbox/...`
   与 `go build -tags notification_kafka ./services/notification/...` 均 rc=0，
   2026-10-04 又补了 `go vet -tags inbox_kafka` / `-tags notification_kafka`、`-mod=readonly` 构建、
   以及两条标签各自的 `go test -p 1 -count=1 -tags ...`，全部 rc=0（命令清单见 `docs/commands.md`），
   所以标签侧可编译、可静态检查、可跑该包用例，但**未做实机联调**。
   live-media 的消费者是五者里唯一**不新增标签**的一条：它与本服务的发布器共用
   `-tags livemedia_kafka` 与同一个 `Kafka.Enabled` 开关（没有单独的 `ConsumeEnabled`），
   所以打开这一个开关同时启动两条链路、关掉则两条都不跑，部署时不能只想要一条。
   - **同日：`search-indexer` 也补齐了适配层并挂上标签**（此前它连 `MessageSource` 都只有一个接口、
     零实现，README 还把接线方案写成要实现那个永不存在的拉取源）。新增
     `internal/consumer/queue.go`（`MessageQueue`/`Settings`/`QueueFactory`/`Handler`/`SettingsFrom`/
     `ValidateKafka`/`Supervisor`，无标签）、`kafkaruntime_disabled.go`（`!searchindexer_kafka`，
     返回 `ErrKafkaRuntimeNotBuilt`）、`kafkaruntime_kafka.go`（`searchindexer_kafka`，映射成
     `kq.KqConf` 并带上 `ServiceConf{Name,Log,Mode}`）、`svc.startConsumer()` 三分支与
     `RuntimeNotes()` 启动日志、`Kafka.Enabled`/`Offset`/`Conns`/`Consumers`/`Processors`/`ForceCommit`/
     SASL 配置位；同时**删除**拉取式 `Consumer.Run`、`MessageSource`、`Options.BatchSize`/`PollErrorWait`
     与 `Kafka.PollBatchSize`，只留一条推送路径。默认与带标签两侧 `go build`/`go vet`/
     `go test -p 1 -count=1 ./services/search-indexer/...` 均 rc=0，`internal/consumer`
     用例从 `3 文件 35/0` 增至 `6 文件 51/1`（新增 16 条钉适配器契约）。
     这只是把「消费者能否编译」变成「消费链路能否接线」：`content.published.v1` 的生产点在
     2026-10-05 补上了（见本组第 1、2 条），`engagement.action.v1` 仍全仓无生产点（见第 5 条），
     所以 search-indexer 这两条入站链只有前一条能收到事件，索引文档的 heat 字段仍只能是零；
     已经端到端打通的事件有两条：`live.state.v1`（生产者在 live-ingest，消费者在 live-room、inbox 与
     live-media）与 `content.published.v1`（生产者在 video，消费者在 search-indexer 与 inbox），
     两者都只有离线证据、从未与真实 broker 联调。
   - **原来登记的「文档陈旧点」已于 2026-10-04 逐处改正**：`inbox/internal/consumer/kafkaruntime_stub.go`、
     `kafkaruntime_kafka.go`、`notification/internal/consumer/dispatcher.go`、`kafkaruntime_disabled.go`、
     `kafkaruntime_kafka.go` 五处注释与 `services/inbox/README.md` §3/缺口 1 都写着
     「go-queue 只是 go.mod 的 indirect 依赖、直接 import 需要授权依赖变更」，
     而 `go.mod:9` 已把 `github.com/zeromicro/go-queue v1.2.2` 列为**直接** require
     （`segmentio/kafka-go v0.4.47` 在 indirect 块与 `go.sum` 都齐）。同一轮顺带改掉另外两处的同类误述：
     `services/live-media/README.md` 缺口 3 的「go-queue/kq 属未链接依赖」与
     `services/search-query/README.md` 缺口首条的「kq 依赖未就绪」。真实情况：**依赖侧不阻塞，
     缺的是发布器/消费者实现**。同日二次全仓扫描又清掉 `search-indexer` 的三处同类误述
     （称「kafka-go 无 `go.sum` 记录、无法编译、仓库禁止新增依赖」）：`internal/consumer/consumer.go:4-9`、
     `internal/config/config.go:31-34`、`etc/searchindexer.v1.yaml` 的 `Kafka` 段注释；改后该服务
     `gofmt` / `go build` / `go vet` / `go test -p 1 -count=1 ./services/search-indexer/...` 全部 rc=0。
     第三次扫描再清掉漏网的 `services/notification/etc/notification.v1.yaml:23`
     （原写「go.mod 未把 go-queue/kafka-go 提升为直接依赖」）。教训：这类误述分散在 .go / .yaml / .md
     三类文件里，只 grep `.go` 会漏。
     现在写的是真实理由：保留构建标签是因为没有 broker 可验证消费语义。结论不变，理由换掉，
     接线时不要再照旧注释去做 `go get`/`go mod tidy`。
   - **同日：`live-media` 补上 `live.state.v1` 的第二个入站端**（此前该服务的 `SubscribeTopics`/`Group`
     只是配置位、没有任何读取方，断流后的档位下线只能靠调用方逐个调 `OfflineStreamOutput`）。新增
     `internal/consumer/mapping.go`（信封 → `OfflineCommand`，`Interpret` 先校验后决策：只有
     `stream_state=4` 触发动作，1/2/3 一律 `DecisionNone`，其中中断态不下线是因为宽限期内可续推，
     此时摘全房间档位会把一次网络抖动变成观众可见的整场停播；`Stopped` 却缺 `session_id` 判为契约违反，
     绝不退化成「按房间下线」，否则一条迟到的旧事件就能把刚开播的场次打死）、
     `handler.go`（进程内重试计数 + `given_up` 日志 + `Skipped`/`Noop`/`Applied` 口径）、
     `queue.go`（`MessageQueue`/`Settings`/`QueueFactory`/`Supervisor`/`SettingsFrom`/`ValidateKafka`）、
     `wiring.go` 与 `kafkaruntime_{disabled,kafka}.go`、`example_yaml_test.go`（把 `etc` 模板当生产配置验证：
     少一个消费键、`Enabled` 被写成 true、SASL 三键被写死进模板都会红）。
     接线点在入口 `livemedia.v1.go:36` 而不是 `ServiceContext`：消费侧要调用 logic，logic 又依赖 svc，
     写进 svc 会成环；发布循环则已经在 `svc.startPublisher()`（`internal/svc/servicecontext.go:121`）里起好。
     与 inbox、live-room 的关键差别是**本服务没有消费位点表**：下线的幂等性由 `MarkOfflineTx` 的 CAS 条件
     （`state=在线`）保证，同一事件重投第二次扫不到行、`Affected=0`，所以不需要按 `event_id` 去重；
     代价是失败事件没有持久化死信，达到 `Kafka.MaxRetries` 只在 `internal/consumer/handler.go:223`
     留一条 `given_up` 错误日志就提交位点，兜底是到期清扫 `MarkExpiredOffline` 或运营逐档位下线。
     默认与带标签两侧 `go build`/`go vet`/`go test -p 1 -count=1 ./services/live-media/...`、
     以及 `go test -p 1 -count=1 -tags livemedia_kafka ./services/live-media/internal/consumer/` 均 rc=0；
     `internal/consumer` 是 7 个测试文件、静态 41 顶层 / 10 子（`-v` 动态 41 / 60），0 FAIL、0 SKIP。
5. **topic 声明与事件两端仍对不上（`live.state.v1`、`content.published.v1` 除外）**：`etc` 里声明 `SubscribeTopics` 的是
   `live-gateway`/`live-media`/`live-room`，声明 `PublishTopics` 的是六个服务：`live-ingest`、`live-media`、
   `playback`、`recommend-recall`、`upload`、`video`（后两条是 2026-10-05 接线时补的；
   `inbox`/`search-indexer` 用 `Kafka.Topics`，`notification` 用单键 `Kafka.RequestTopic`）。
   `live-ingest` 的 `PublishTopics: [live.state.v1]` 与 `live-room`/`live-media` 的 `SubscribeTopics: [live.state.v1]`
   现在两端都有实现兜着（生产者在 `internal/publisher`，消费者在 `internal/consumer`）；
   闭合的意思是「生产端与三个消费端都在代码里，且各自只写自己域的结论」：live-room 按 `event_id`
   去重、按 `seq` 挡乱序后推进流状态，inbox 用 `inbox_consumer_offset` 状态机去重再推站内信，
   live-media 不建位点表（下线的幂等性由 `MarkOfflineTx` 的 `state=在线` CAS 条件承担）。
   `content.published.v1` 是同日补上的第二条：`video/etc/video.v1.yaml` 的 `PublishTopics` 与
   `search-indexer`/`inbox` 的 `Kafka.Topics` 两端都有实现，video 侧同事务写 `video_outbox`、
   `internal/publisher`（`-tags video_kafka`）投递，消费侧的字段映射见
   `services/search-indexer/internal/consumer/mapping.go`。
   其余事件**有契约无生产者**：
   `engagement.action.v1`（inbox 与 search-indexer 都在等，后者靠它打 heat 补丁）、
   `notification.request.v1`（`notification/internal/consumer/eventhandler.go` 是完整消费实现，
   全仓无任何生产点）。
   `media.task.v1` 已在 2026-10-05 从这条清单上划掉：`upload/internal/repository/repository.go:300`
   在 `TransactCtx`（`:288`）里与状态推进同事务写 `upload_outbox`，`internal/publisher` 投出去；
   `:273` 回填的仍是 `asset-placeholder:<uploadID>`（占位没变，变的是「事件是否落库」）。
   没划掉的是另一端：`asset`、`transcode`、`content-fingerprint` 三个服务目录都没有 `internal/consumer`
   （`content-fingerprint/internal/logic/submittasklogic.go:29` 还挂着 `TODO(后续)` 等这个 topic），
   所以「上传完成 → 媒资处理」目前只走到本服务的表。
   `live-media` 是「出站、入站都接了」的形状：它同时声明 `SubscribeTopics` 与 `PublishTopics`，
   也同事务写 `live_media_outbox`（全仓 8 张**事件** outbox 表之一：`live_ingest_outbox`、
   `live_media_outbox`、`member_outbox`、`playback_outbox`、`recall_outbox`、`search_outbox`、
   `upload_outbox`、`video_outbox`；
   `feed_outbox` 不算，它是「动态主表」，列里没有 `event_id`/`event_type`，
   见 `deploy/migrations/feed/000001_create_feed_tables.sql`）。
   2026-10-04 出站侧接上了：`internal/publisher`（`-tags livemedia_kafka`）把待发行同步投到
   9 个 `livemedia.*.v1`，`PublishTopics` 与 model 的 `EventType*` 派生集合做**相等**校验。
   **入站侧同日接上**：`internal/consumer`（见本组第 4 条最后一个子项）读 `SubscribeTopics: [live.state.v1]`
   与 `Group`，只在 `stream_state=4` 时把本场次全部在线档位下线，用的就是本服务自己的
   `logic.OfflineSessionOutputs`。仍没接的是消费位点/死信表（`given_up` 只有错误日志），
   以及这 9 个**出站** topic 的下游消费者，见 `services/live-media/README.md` 缺口 1 与缺口 3。
   所以这 9 个出站 topic 与 `playback.heartbeat.v1`、`recall.pool.published.v1`、`media.task.v1` 同属
   「有生产者、仓库内零消费者」，回放拼接、档位下线、回收计数这些下游触发点一个也不会发生。
   `recommend-recall`（2026-10-05 接线）连配置位都没有入站侧：它只产出事件，
   `etc` 里没有 `SubscribeTopics`/`Group`，本服务也没有 `internal/consumer`；
   它的缺口是投递语义从未在 broker 上验证、以及已发布行没有清理与滞留告警读取方
   （`CountPending`/`CountStuck`/`DeleteSentBefore` 零生产调用者），
   详见 `services/recommend-recall/README.md` 缺口 B6。
   `upload`（同日接线）是同一个形状，且多一条只有它才有的部署期风险：`upload_outbox` 的建表脚本
   `deploy/migrations/upload/000003_create_upload_outbox.sql` **从未在任何实例执行过**
   （`deploy/migrations/README.md` 里登记为 `pending`），而本服务的新代码把事件行写进了完成事务，
   所以「先迁移、再上线」在这里是硬顺序而不是建议；此外表上既没有租约列也没有判死行的解冻入口，
   多副本同起 `RunOnce` 会把同一行投两次（无 broker、无生产库写入，因此这两点只有代码证据，
   见 `services/upload/README.md` 缺口 9 与缺口 17）。
   接线次序建议照 `live.state.v1` 的做法：先定 payload 与 `event_type`，再补发布器（照
   `services/live-ingest/internal/publisher`），最后补消费者（照 `services/live-room/internal/consumer`），
   两端各自带构建标签与 `RuntimeNotes`。
6. **审核闭环未接通**（与上面「审核闭环尚未接通」一段一致，本轮重新确认）：
   `moderation.result.v1` 仍是 `// TODO(event)`（`moderation-orchestrator/internal/logic/submitworkerresultlogic.go:49`），
   该服务**没有** `internal/consumer` 目录；而消费侧的入口 RPC 已经实现并有单测
   （`danmaku/internal/logic/applymoderationresultlogic.go`、
   `live-room/internal/logic/applyroommoderationresultlogic.go`）。
   结论要精确：**不是「两端没实现」，而是「两端实现了、中间的事件没发」**，
   所以机审推进目前只能靠 `/admin/moderation/*` 人工裁决或稿件状态迁移接口。

### B. 后台执行：有 logic、没有驱动方

1. **`cron` 不会跑**：调度循环未实现，`internal/registry` 里没有任何 `Spec` 注册（见
   `services/cron/README.md` 已知缺口 1、2）。全仓因此**没有任何东西在推进任何定时任务**。
2. **`cron` 没有种子**：`deploy/migrations/cron/` 只有 `000001`、`000002` 两个建表文件，
   没有任何 `cron_task_definition` 行，即「任务定义表恒空」——即使调度器接上也是一天空转。
3. **`operation` 的 `RunAdminTask` 没有调用方**：`grep -rn RunAdminTask services gateway` 只命中
   它自己的 logic/config（`internal/logic/runadmintasklogic.go:27`、`internal/config/config.go:97`），
   既没有 cron `Spec`，也没有 gateway/admin 路由，批量运营任务提交后停在 `pending`。
4. **`ops-config` 的发布没有消费者**：三个 `internal/consumer` 里都不读运营配置发布事件，
   配置改了只能靠各服务重启或缓存 TTL。
5. **`transcode` 是占位实现**：`SubmitTask` 只 Insert 一条 `PENDING`，不调 FFmpeg、不发 MQ、不写缓存
   （`services/transcode/internal/logic/submittasklogic.go:29`，日志自陈 `(placeholder, no FFmpeg)` `:61`），
   真实转码 Worker 属未落地范围。**不得**据此认为媒资已可播放（AGENTS.md §8）。

### C. 跨服务接线：配置项存在但适配层/契约缺失（第 1 条的适配层已于 2026-10-03 补上，契约缺口已于 2026-10-04 关闭）

1. **`account → social-graph`：装配与六个关系 RPC 已全部接上真实读，剩下的是「降级静默」**。
   新增 `services/account/internal/repository/socialgraph_client.go`，`serviceContext.go:37-40`
   在配置了 `SocialGraphRPC.Etcd.Hosts`/`Target` 时注入适配器；适配器按下游硬上限自己切片与分页
   （`IsFollowedBatch`/`RichRelations` 每片 ≤100，对应 `isfollowedbatchlogic.go:32` 与
   `richrelationslogic.go:35`；`ListFollowing`/`ListBlacks`
   以 ps=50 翻到 total 或空页为止，见 `listfollowinglogic.go:29`，并有 100 页硬停——
   宁可报错也不让一次账号聚合变成无界扇出），批量任一片失败即整体报错，不回半张关系表。
   `Relation3`/`Relations3`/`Attentions3`/`Blacks3`/`RichRelations3` 与 `Card3`/`ProfileWithStat3`
   的关注/粉丝计数因此在配置后可达真实 RPC；`services/account/etc/account.v1.yaml` 的示例段已打开，
   并被 `TestDownstreamRpcBlocksAreConfigured` 钉住（不许重新注释、Etcd Key 必须与对端注册一致）。
   - **原「`RichRelations` 属接口口径缺口」已于 2026-10-04 关闭**（见 `services/social-graph/README.md` 缺口 8、
     `services/account/README.md` 缺口 H20）。落地内容：social-graph 新增 `RichRelations` RPC
     （`socialgraph.proto:187`），`RelationAttr` 按「掩码位」重排并把 `SPECIAL` 从 5 改成 8
     （改前全仓无代码引用该常量，修订记录写在枚举注释里）；数据所有者侧一次算清四位
     （`social-graph/internal/repository/repository.go:451`：follow 双向 + black + special 四条
     `IN (?)` 存在性查询按位或，任一失败整体报错，不读恒为 0 的 `relation_follow.attr` 列，
     也不碰缓存）；**没有新迁移**——四条查询全落在既有索引上。
     account 适配器删掉 `ErrRichRelationsUnsupported`，改为切片 + 逐键透传。
     仍未改的两处事实：三个列表 RPC 的 `RelationItem.attr` 照旧按整张列表填常量
     （`listfollowinglogic.go:45` 恒 1），即「从关注列表看不出特别关注/互关」仍是 social-graph 缺口 5。
   - **未配置与 RPC 报错两种形态仍静默，且从未实机验证**：nil 分支照旧回空默认值
     （`relation.go:14`、`:29`、`:51`、`:67`、`:85`、`:108`），RPC 报错也只落日志后补默认值
     （`:18`、`:36`、`:55`、`:71`、`:91-93`、`:112`），因此「下游挂了」与「确实没关系」
     在 account 的应答里不可区分；要收严得先定错误语义（显式 not-configured / 降级标记），
     属 account 与网关的口径决策。本仓库没有任何服务被启动过，
     `zrpc.MustNewClient` 在 etcd 不可达时的启动表现未验证（见上面 E 组）。
2. **`feature-store ↔ recommend-rank` 批量上限冲突**：rank 按 `FeatureBatchSize=128`
   （`recommendrank.v1.yaml:54`）切分候选下发，而 feature-store 单次主体数上限是
   `model.MaxBatchEntities = 20`（`services/feature-store/model/types.go:501`，超出直接
   `ErrTooManyEntries`）——见上面 feature-store 段，接线前必须在 rank 侧再切一层。
3. **rights/asset 的四个契约缺口**、**search 读写两侧字段词汇是两套**：见上面两节，均需在阶段 3 前评审，
   属于「网关侧已尽力、必须先改 `.proto`/mapping」的一类，本轮不可解。

### D. 面与能力：已实现但不可对外交付

1. **管理员 2FA 是「配置即锁死」**：`services/operation` 的 `checkSecondFactor` 强制要求带码并去
   `account.CheckCapture` 校验，但全仓没有发送通道（`grep -rn SendCapture services/operation gateway/admin`
   非测试代码本轮实测**零命中**，`admin.api` 也没有对应路由），Redis 里永远不会出现
   `cap_code_1_<target>`（键派生 `services/account/internal/repository/login.go:81`），
   错码计入防爆破 → 5 次后锁定。详见 `services/operation/README.md` 已知缺口 B。
2. **`live-gateway` 的 `SendToUser` 在不鉴权的分支上先暴露在线态**：目标离线时先读在线集合、
   命中为空就 `return NO_LEASE`，鉴权链在其之后（`sendtouserlogic.go:74`、`:88-98`），
   构成在线态侧信道。本轮只用例钉住现状并登记（`services/live-gateway/README.md` 已知缺口 17），
   修它要连带改判定次序，属于发布前必须关闭项。
3. **停流不清节点指针（2026-10-04 已修）**：`live-ingest` 迁移到 `STOPPED` 时会
   `NodeAssignment.Release`（`internal/logic/streamstate.go:265`）并 `IngestNode.ReleaseQuota`（`:273`），
   原先没把 `live_stream.node_id` 抹掉，于是终态流仍「住」在已归还配额的节点上，同一事实两个口径。
   现在停流事务用与 `ReleaseIngestNode` 同一条 `SetNode(streamID, "", s.NodeID)` CAS 清指针，
   原 `t.Skip` 断言已启用为回归防线（`services/live-ingest/internal/logic/streamstate_test.go` 的
   `TestStreamStateMachine_StoppedClearsNodePointer`）。**残留**：历史上已停的流行仍带旧 `node_id`，
   需要一次性数据修复（见 `services/live-ingest/README.md` 已知缺口 10）。
4. **`recommend-recall` 的特征源是显式未接线实现**：`UnconfiguredFeatureSource`
   （`internal/repository/featuresource.go:86-100`）三类查询都返回「未配置」错误，
   刻意不返回空数组冒充「没有兴趣标签」。召回真正可用要等 feature-store 侧接线（见 C1/C2）。

### E. 尚未执行的验证（不要把「门禁绿」读成「能上线」）

1. **迁移只在隔离实例复验**：所有服务的迁移按纪律只应用到 `127.0.0.1:3399`（库名逐服务分开），
   **从未**在真实/共享实例执行；本机维护者的 MySQL80（`127.0.0.1:3306`）禁止写入。
2. **端到端未做过**：没有任何一次跨服务的真实 gRPC 调用、没有起过 MQ/Elasticsearch/对象存储，
   `scripts/rpc/smoke.ps1` / `smoke.sh` 与 Postman 集合**只是生成出来的产物，从未被执行过**，
   它们的「接口可达」结论需要真实实例才能成立。
3. **本轮五道门禁的实际结果**（2026-10-03，串行、整树一次只跑一个）：
   `gofmt -l api common gateway services scripts` 空输出；`go build ./...` rc=0；`go vet ./...` rc=0；
   `go test -p 1 -count=1 ./...` rc=0（`-p 1` 必须保留：Windows 页面文件限制下并发跑多个测试包会 OOM errno=1455）；
   `node scripts/gen-api-docs.mjs --check` 通过（app 198 / admin 313，114 个产物均为最新）。
   以上只覆盖「编译 + 静态检查 + 离线单测 + 文档一致性」，与 A/B/C/D 各组缺口互不矛盾。
4. **本轮没有提交任何东西**：未 `git add`、未 `commit`、未 `push`；所有改动都留在工作区待审阅。
