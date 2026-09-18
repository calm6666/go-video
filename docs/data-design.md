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
