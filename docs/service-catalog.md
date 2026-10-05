# 服务目录与拆分说明

## 1. 领域服务清单

| 服务 | 数据所有权 | 第一阶段部署 | 主要依赖 |
|---|---|---:|---|
| `gateway/app` | 无 | 独立 | identity、content、playback、community |
| `gateway/admin` | 无 | 独立 | operation、identity、content |
| `account` | 账号、设备、会话 | 与 `user-profile`、`creator` 合并为 identity | MySQL、Redis |
| `user-profile` | 用户资料、空间和隐私 | identity | account |
| `creator` | 创作者认证、权限和配额 | identity | account、user-profile |
| `risk-control` | 风控规则、设备画像、处罚 | 第二阶段 | Redis、SPM |
| `video` | UGC/PUGC 稿件、版本、发布状态 | 与 catalog/rights 合并为 content | identity、media、moderation |
| `catalog` | 作品、季、集、分区、标签 | content | rights、asset |
| `rights` | 版权合同、地区和时间窗口 | content | catalog、cron |
| `upload` | 上传会话和分片 | 与 asset/transcode 合并为 media | OSS、Redis |
| `asset` | 原文件、封面、字幕、音轨、截图元数据 | media | OSS、MySQL |
| `transcode` | 转码任务和播放版本 | media；Worker 可独立 | FFmpeg、MQ、OSS |
| `content-fingerprint` | 指纹和重复/侵权匹配 | media/moderation 模块 | 媒体 Worker |
| `playback` | 播放会话、签名 URL、防盗链 | 第二阶段独立 | CDN、Redis、rights |
| `live-room` | 直播间和主播状态 | 与 live-ingest/live-media 合并为 live | Redis、MQ |
| `live-ingest` | 推流密钥、节点、流状态 | live | 推流网关、CDN |
| `live-media` | 直播转码、录制和回放 | live | FFmpeg、OSS |
| `live-gateway` | WebSocket 房间连接 | 第三阶段独立 | Redis、消息队列 |
| `social-graph` | 关注、粉丝、黑名单 | 与 feed/engagement 合并为 community | MySQL、Redis |
| `feed` | 动态和关注流投影 | community | social-graph、MQ |
| `engagement` | 点赞、收藏、分享、不感兴趣 | community；高流量时独立 | Redis、MySQL、MQ |
| `comment` | 评论、回复、举报 | 第二阶段独立 | MySQL、Redis、moderation |
| `danmaku` | 时间轴弹幕和屏蔽词 | 第二阶段独立 | Redis/MQ、moderation |
| `moderation-orchestrator` | 审核任务、规则、结论、申诉 | 与 worker 合并为 moderation | MQ、MySQL、审核能力 |
| `moderation-worker` | OCR/ASR/图像/音频结果 | moderation | FFmpeg/算法服务 |
| `search-indexer` | 搜索索引和版本 | 第三阶段 | MQ、OpenSearch |
| `search-query` | 查询、热词、排序配置 | 第二阶段独立 | OpenSearch、Redis |
| `recommend-recall` | 召回候选和特征读取 | 第四阶段 | SPM、Redis/OpenSearch |
| `recommend-rank` | 排序模型和实验配置 | 第四阶段 | SPM、feature-store |
| `event-collector` | 原始行为事件 | 第二阶段 | MQ、分析存储 |
| `spm` | 用户行为指标和推荐特征 | 与 event-collector 合并；规模后独立 | MQ、分析存储 |
| `feature-store` | 在线推荐特征 | 第四阶段 | SPM、Redis |
| `inbox` | 站内信和未读状态 | 与 notification 合并为 notify | MySQL、Redis |
| `private-message` | 私信会话和消息 | 第三阶段 | WebSocket、Redis、MySQL |
| `notification` | Push/短信/邮件投递记录 | notify | 供应商 SDK、MQ |
| `operation` | RBAC、运营配置、审核后台、审计 | 第一阶段 | 各领域 RPC |
| `ops-config` | 分区、标签、专题和推荐位 | operation | MySQL、Redis |
| `audit` | 管理操作和安全审计 | operation | append-only storage |
| `open-platform` | 应用、密钥、OAuth 授权、配额 | 第四阶段 | account、gateway |
| `cron` | 任务定义、执行记录和租约 | 独立任务进程 | MQ、各领域 RPC |
| `membership` | 会员套餐、会员身份、权益授予与判定 | 第五阶段（商业化） | MySQL、Redis；被 trade-order 同步调用 |
| `trade-order` | 商业订单状态机、履约指令、退款申请与审批 | 第五阶段（商业化） | membership、payment、coin（同步 RPC） |
| `payment` | 钱包余额、充值单、支付单、退款单、资金流水（沙箱台账） | 第五阶段（商业化） | MySQL；不调用真实支付渠道 |
| `coin` | 硬币余额、投币记录、每日上限与流水 | 第五阶段（商业化） | MySQL、Redis |
| `creator-revenue` | 分成规则、参与关系、计量与应结台账 | 第五阶段（商业化） | spm/coin 上报的计量事实；不出金 |

## 2. 部署合并原则

逻辑服务不等于进程。第一阶段推荐：

```text
gateway/app + gateway/admin
identity      = account + user-profile + creator
content       = video + catalog + rights
media         = upload + asset + transcode + content-fingerprint
community     = social-graph + feed + engagement
moderation    = moderation-orchestrator + moderation-worker
notify        = inbox + notification
operation     = operation + ops-config + audit
cron          = 定时任务和补偿任务
```

`playback`、`transcode`、`live-gateway`、`danmaku`、`engagement`、`search-query`、`event-collector` 和 `spm` 根据吞吐、资源或故障隔离需要优先独立。合并服务仍要保持包级领域边界和独立数据访问接口，后续拆分不能依赖跨包私有变量。

商业化面（2026-09-22 纳入范围）建议合并为一个 `commerce` 进程组，但数据所有权仍按服务分开：

```text
commerce = membership + trade-order + payment + coin + creator-revenue
```

合并部署的前提是每个域仍只写自己的 schema（`go_video_membership` / `go_video_trade_order` / `go_video_payment` / `go_video_coin` / `go_video_creator_revenue`）。广告投放、广告位分析和广告推荐不在范围内，商业化服务不得出现广告相关接口或字段（AGENTS.md §1、§7）。

## 3. 依赖方向

```text
gateway/app → identity/content/playback/community
gateway/admin → operation/identity/content
content → identity/media/moderation/rights
media → upload/asset/transcode/content-fingerprint
community → identity/content/moderation/notify
search/recommend/spm ← 领域事件（尽量不反向阻塞主链路）
cron → 领域 API/RPC（不直接写别人的数据库）
commerce: trade-order → membership/payment/coin（下单、扣款、履约、退款审批的编排方）
commerce: membership、payment、coin、creator-revenue 互不直连，只被编排方同步调用
creator-revenue ← spm/coin 的计量与投币事实（服务身份上报，不反向读取原始行为表）
```

`commerce` 内部是**单向编排**：只有 `trade-order` 能驱动会员开通、资金变动与硬币发放，`payment` 与 `membership` 之间不互调（订单先落支付单，再由订单调用授予）。权益读取（`CheckEntitlement`）由 `gateway/app` 和服务侧直接调 `membership`，网关不代为判定。

推荐、搜索、SPM、通知和统计默认最终一致；内容发布、权限、审核结论和播放授权由领域服务同步确认。
