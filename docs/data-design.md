# 数据设计与一致性

## 1. 核心对象

```text
User ─ Creator ─ Submission ─ Asset ─ Variant ─ PlaybackSession
                    │             └─ Subtitle/AudioTrack/Cover
                    └─ ModerationTask
Work ─ Season ─ Episode ─ PGC Rights
User ─ SocialGraph ─ Feed ─ Engagement
Video/Live ─ Comment/Reply 与 Danmaku
Playback/Interaction ─ Event ─ SPM Feature ─ Recommend
User ─ Membership/Grant ─ Entitlement
Order ─ Payment/Wallet/Flow ─ Refund
User ─ CoinAccount ─ Toss ─ CoinFlow ─ Content
Metric(播放/投币事实) ─ RevenueRule ─ Enrollment ─ Settlement
```

`Submission` 是用户投稿稿件，`Work/Season/Episode` 是作品目录；PGC 内容可以引用媒资，但不能把版权目录简化为普通视频表。

## 2. 状态机

投稿/媒资：

```text
DRAFT → UPLOADING → UPLOADED → SCANNING → TRANSCODING
      → READY_FOR_REVIEW → APPROVED → SCHEDULED → PUBLISHED
                         └→ REJECTED/APPEAL
PUBLISHED → OFFLINE/EXPIRED/DELETED
```

版权窗口：`待录入 → 待验权 → 生效 → 排期/上架 → 到期/撤权 → 下架`。

状态迁移由数据所有者服务执行，使用乐观锁/version；外部回调必须校验任务 ID、签名和当前状态。

## 3. MySQL

- 初期单实例、按领域 schema 和清晰索引；读副本、归档和冷热分层优先于分库分表。
- 事务表保存事实和必要快照；统计计数、搜索索引、推荐特征均可从事件重算。
- 评论、弹幕、行为事件按时间归档；大文件只存对象存储，MySQL 保存 metadata、校验和、状态和地址引用。
- 删除用户时按数据分类执行软删除、匿名化或物理删除，记录审计和保留期限。

## 4. Redis

用途：详情缓存、短期播放授权、会话、幂等键、热点保护、排行榜、连接路由和限流。Redis 不是唯一事实源；关键事实必须落在领域库或可重放事件中。缓存使用随机 TTL、空值保护和降级。

## 5. 高并发计数

点赞、收藏、播放进度等先写行为事实或幂等记录，再异步聚合；读取允许短暂延迟。重复消费、网络重试和客户端重复点击不能重复计数。计数修复必须有重算任务，不能人工直接改线上缓存作为永久修复。

## 6. 隐私与数据最小化

SPM 事件按用途采集最小字段，用户标识按隐私策略脱敏；不采集与视频推荐无关的敏感字段，不把广告标识、支付信息或身份证信息加入事件。日志中手机号、Token、对象存储签名和原始内容 URL 必须脱敏。

## 7. 商业化数据不变量（资金、权益、硬币、分成）

本期商业化只走沙箱台账，不调用任何真实支付渠道，因此以下不变量是"开通即生效"能成立的前提，而不是伪造成功的借口：

- **金额只用最小货币单位整数**：`*_minor` 为 `int64`，配合显式 `currency`（如 `CNY`）。禁止浮点参与任何金额存储、计算或 RPC 字段。
- **每个写入口有幂等键**：`request_id` / `idempotency_key` 建唯一索引（`utf8mb4_bin` 排序规则，避免大小写折叠导致幂等键互相吞并），命中重复时返回首次结论并标记 `duplicated=true` / `replayed=true`，不产生第二笔台账。
- **状态推进用版本号 CAS**：订单、支付单、充值单、结算单都带 `expected_version`/`version`，非法跃迁由数据所有者服务拒绝，网关不判定状态机。
- **余额扣减用条件更新**：`UPDATE ... SET balance = balance - ? WHERE mid = ? AND balance >= ?`，受影响行数为 0 即判定余额不足，禁止先读后写。
- **余额与硬币不可互换**：`payment` 的现金余额台账与 `coin` 的硬币余额是两套账，任何接口都不能把二者当作同一种额度扣减。
- **权益读侧只读授予表**：`granted=true` 必须是"授予表里确实存在未过期的一行"的真实读结论；无记录即未开通，并返回原因枚举（而非错误码）。
- **分成只存换算结果**：`creator-revenue` 只保存规则版本、计量事实引用与应计金额，原始播放/投币事实仍由 `spm` / `coin` 持有；出金、提现、打款不在范围（`payout_state` 恒 `NOT_PAYABLE`）。
- **真实资金能力不开接口**：渠道回调验签、退款到卡、提现、对账文件、发票税务返回明确的 not-configured 错误（`FailedPrecondition`），禁止返回假成功。
- **删除与撤回留证据**：订单、退款、资金调整、结算确认都必须进 `*_event` / 流水表，操作者 mid 与 trace_id 落库，供 `audit` 侧核对。

