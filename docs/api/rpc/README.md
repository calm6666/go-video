# RPC 契约索引（43 个服务 / 589 个方法）

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

每个服务一个文件，含方法表、消息与枚举全量定义、etcd key、端口与网关消费方。

下面按**消费方**分节（不是按字母顺序摊平）：某服务被哪个网关的哪个配置字段引用，
是从两个网关的 `etc/*.yaml` 与 `internal/config/*.go` 读出来的，不是手工归类。

## 终端面与运营面都消费（18 个服务 / 245 个方法）

同一份契约既服务客户端用例，也被后台排障/运营页调用；改它要同时守两条兼容线。

| 服务 | 文档 | proto 包 | 发现用的 etcd key | 端口 | 方法 | 网关消费方 |
|---|---|---|---|---|---|---|
| `account` | [account.md](./account.md) | `account.v1` | `account.v1.rpc` | 8083 | 30 | app:AccountRPC、admin:AccountRPC |
| `catalog` | [catalog.md](./catalog.md) | `catalog.v1` | `catalog.v1.rpc` | 8096 | 12 | app:CatalogRPC、admin:CatalogRPC |
| `coin` | [coin.md](./coin.md) | `coin.v1` | `coin.v1.rpc` | 8163 | 10 | app:CoinRPC、admin:CoinRPC |
| `comment` | [comment.md](./comment.md) | `comment.v1` | `comment.v1.rpc` | 8082 | 7 | app:CommentRPC、admin:CommentRPC |
| `creator` | [creator.md](./creator.md) | `creator.v1` | `creator.v1.rpc` | 8086 | 8 | app:CreatorRPC、admin:CreatorRPC |
| `creator-revenue` | [creator-revenue.md](./creator-revenue.md) | `creatorrevenue.v1` | `creatorrevenue.v1.rpc` | 8164 | 16 | app:CreatorRevenueRPC、admin:CreatorRevenueRPC |
| `danmaku` | [danmaku.md](./danmaku.md) | `danmaku.v1` | `danmaku.v1.rpc` | 8103 | 9 | app:DanmakuRPC、admin:DanmakuRPC |
| `inbox` | [inbox.md](./inbox.md) | `inbox.v1` | `inbox.v1.rpc` | 8104 | 7 | app:InboxRPC、admin:InboxRPC |
| `live-room` | [live-room.md](./live-room.md) | `liveroom.v1` | `liveroom.v1.rpc` | 8119 | 21 | app:LiveRoomRPC、admin:LiveRoomRPC |
| `membership` | [membership.md](./membership.md) | `membership.v1` | `membership.v1.rpc` | 8160 | 16 | app:MembershipRPC、admin:MembershipRPC |
| `moderation-orchestrator` | [moderation-orchestrator.md](./moderation-orchestrator.md) | `moderation.v1` | `moderation.v1.rpc` | 8093 | 7 | app:ModerationRPC、admin:ModerationRPC |
| `notification` | [notification.md](./notification.md) | `notification.v1` | `notification.v1.rpc` | 8108 | 11 | app:NotificationRPC、admin:NotificationRPC |
| `payment` | [payment.md](./payment.md) | `payment.v1` | `payment.v1.rpc` | 8161 | 14 | app:PaymentRPC、admin:PaymentRPC |
| `private-message` | [private-message.md](./private-message.md) | `privatemessage.v1` | `privatemessage.v1.rpc` | 8150 | 15 | app:PrivateMessageRPC、admin:PrivateMessageRPC |
| `trade-order` | [trade-order.md](./trade-order.md) | `tradeorder.v1` | `tradeorder.v1.rpc` | 8162 | 12 | app:TradeOrderRPC、admin:TradeOrderRPC |
| `transcode` | [transcode.md](./transcode.md) | `transcode.v1` | `transcode.v1.rpc` | 8100 | 7 | app:TranscodeRPC、admin:TranscodeRPC |
| `user-profile` | [user-profile.md](./user-profile.md) | `userprofile.v1` | `user-profile.v1.rpc`（配置里 `Name` 写的是 `userprofile.v1.rpc`） | 8085 | 35 | app:UserProfileRPC、admin:UserProfileRPC |
| `video` | [video.md](./video.md) | `video.v1` | `video.v1.rpc` | 8095 | 8 | app:VideoRPC、admin:VideoRPC |

## 只有终端面消费（6 个服务 / 55 个方法）

面向 Android/iOS/HarmonyOS/桌面的读用例，版本兼容窗口最长，不能随意改字段语义。

| 服务 | 文档 | proto 包 | 发现用的 etcd key | 端口 | 方法 | 网关消费方 |
|---|---|---|---|---|---|---|
| `engagement` | [engagement.md](./engagement.md) | `engagement.v1` | `engagement.v1.rpc` | 8084 | 16 | app:EngagementRPC |
| `feed` | [feed.md](./feed.md) | `feed.v1` | `feed.v1.rpc` | 8092 | 8 | app:FeedRPC |
| `playback` | [playback.md](./playback.md) | `playback.v1` | `playback.v1.rpc` | 8102 | 4 | app:PlaybackRPC |
| `search-query` | [search-query.md](./search-query.md) | `searchquery.v1` | `search-query.v1.rpc` | 8107 | 8 | app:SearchQueryRPC |
| `social-graph` | [social-graph.md](./social-graph.md) | `socialgraph.v1` | `socialgraph.v1.rpc` | 8091 | 14 | app:SocialGraphRPC |
| `upload` | [upload.md](./upload.md) | `upload.v1` | `upload.v1.rpc` | 8098 | 5 | app:UploadRPC |

## 只有运营面消费（17 个服务 / 278 个方法）

只被 `gateway/admin` 引用；鉴权与权限点判定在网关侧，契约里通常带 operator 位。

| 服务 | 文档 | proto 包 | 发现用的 etcd key | 端口 | 方法 | 网关消费方 |
|---|---|---|---|---|---|---|
| `asset` | [asset.md](./asset.md) | `asset.v1` | `asset.v1.rpc` | 8099 | 10 | admin:AssetRPC |
| `audit` | [audit.md](./audit.md) | `audit.v1` | `audit.v1.rpc` | 8110 | 13 | admin:AuditRPC |
| `cron` | [cron.md](./cron.md) | `cron.v1` | `cron.v1.rpc` | 8112 | 23 | admin:CronRPC |
| `event-collector` | [event-collector.md](./event-collector.md) | `eventcollector.v1` | `eventcollector.v1.rpc` | 8152 | 15 | admin:EventCollectorRPC |
| `feature-store` | [feature-store.md](./feature-store.md) | `featurestore.v1` | `featurestore.v1.rpc` | 8130 | 16 | admin:FeatureStoreRPC |
| `live-gateway` | [live-gateway.md](./live-gateway.md) | `livegateway.v1` | `livegateway.v1.rpc` | 8121 | 22 | admin:LiveGatewayRPC |
| `live-ingest` | [live-ingest.md](./live-ingest.md) | `liveingest.v1` | `liveingest.v1.rpc` | 8118 | 22 | admin:LiveIngestRPC |
| `live-media` | [live-media.md](./live-media.md) | `livemedia.v1` | `livemedia.v1.rpc` | 8120 | 28 | admin:LiveMediaRPC |
| `open-platform` | [open-platform.md](./open-platform.md) | `openplatform.v1` | `openplatform.v1.rpc` | 8151 | 24 | admin:OpenPlatformRPC |
| `operation` | [operation.md](./operation.md) | `operation.v1` | `operation.v1.rpc` | 8109 | 22 | admin:OperationRPC |
| `ops-config` | [ops-config.md](./ops-config.md) | `opsconfig.v1` | `opsconfig.v1.rpc` | 8111 | 20 | admin:OpsConfigRPC |
| `recommend-rank` | [recommend-rank.md](./recommend-rank.md) | `recommendrank.v1` | `recommendrank.v1.rpc` | 8123 | 10 | admin:RecommendRankRPC |
| `recommend-recall` | [recommend-recall.md](./recommend-recall.md) | `recommendrecall.v1` | `recommendrecall.v1.rpc` | 8116 | 10 | admin:RecommendRecallRPC |
| `rights` | [rights.md](./rights.md) | `rights.v1` | `rights.v1.rpc` | 8097 | 9 | admin:RightsRPC |
| `risk-control` | [risk-control.md](./risk-control.md) | `riskcontrol.v1` | `risk-control.v1.rpc` | 8105 | 11 | admin:RiskControlRPC |
| `search-indexer` | [search-indexer.md](./search-indexer.md) | `searchindexer.v1` | `searchindexer.v1.rpc` | 8106 | 7 | admin:SearchIndexerRPC |
| `spm` | [spm.md](./spm.md) | `spm.v1` | `spm.v1.rpc` | 8131 | 16 | admin:SpmRPC |

## 两个网关都不引用（服务间 / worker / 尚未接线）（2 个服务 / 11 个方法）

没有 HTTP 入口，只由其它服务或定时任务调用；冒烟时需要单独起进程。

| 服务 | 文档 | proto 包 | 发现用的 etcd key | 端口 | 方法 | 网关消费方 |
|---|---|---|---|---|---|---|
| `content-fingerprint` | [content-fingerprint.md](./content-fingerprint.md) | `fingerprint.v1` | `content-fingerprint.v1.rpc` | 8101 | 6 | — |
| `moderation-worker` | [moderation-worker.md](./moderation-worker.md) | `moderation.worker.v1` | `moderation-worker.v1.rpc` | 8094 | 5 | — |

调用方式见 [scripts/rpc/README.md](../../../scripts/rpc/README.md)。
