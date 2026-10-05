-- =====================================================================
-- engagement 服务 - 领域事件 Outbox 表（engagement.action.v1）
-- =====================================================================
-- 目标库：`go_video_engagement`（由 scripts/migrate.ps1 按服务目录建库并写入 schema_migrations）。
-- 影响范围：新增 1 张表。
-- 用途：承载「某个对象的互动计数发生了一次真实变化」这一事实，供 search-indexer 打热度补丁、
--       inbox 判断是否产生互动站内信（docs/api-and-events.md §5）。
-- 数据所有者：engagement 服务（AGENTS.md §5「互动计数 owner = engagement」）。
--       本表只登记本服务自有事实：动作名、操作者、目标在本域的坐标
--       （business/origin_id/message_id 或 oid/tp/otype）与本服务持有的绝对计数快照。
--       刻意不复制的字段与理由（写在这张表的口径里，改动前先读）：
--       ① content_type（1 UGC、2 PGC、3 直播）：索引侧的内容类型，owner 是产出正文的服务，
--          互动侧只能给 business/tp，不能替它判定（详见 model/engagementoutboxmodel.go 的 Action* 注释）；
--       ② author_mid/target_mid/recipients/content_title：作者归属与标题分属 video/user-profile，
--          且 LikeReq.up_mid 是调用方传入的未校验值，把它投进事件等于让客户端指定通知收件人；
--       ③ view_count/comment_count/danmaku_count/heat_score：分属 playback/comment/danmaku/spm，
--          因此 counters 只携带 snapshot_fields 声明的那一到两列，消费侧按声明字段合并，不整块覆盖。
-- 状态机：state 取值与 model.OutboxState* 严格一致（0 待发布、1 已发布、2 失败），编号不可重排。
--       只有 0 会被 ListPending 取出；2 之后没有人工放行接口（见服务 README「已知缺口」）。
-- 幂等依赖（两个层次，别混淆）：
--       ① 本表的 uniq_event_id 保证同一 event_id 不会被写两行。该列用列级 `utf8mb4_bin`
--          且不给默认值（deploy/migrations/README.md「字符序约定」）：表级默认的 `_ci` 排序会折叠
--          大小写，让两个只差大小写的 event_id 撞上同一唯一键，表现为静默丢事件且不报错；
--          `DEFAULT ''` 则会让第二行「忘了填 event_id」的写入直接撞唯一键，不如按 NOT NULL 无默认报错。
--          本服务其它表的唯一键都走库表默认排序规则，这是第一处列级 COLLATE，
--          登记与断言在 model/migration_parity_test.go 的 binaryCollate 一侧。
--       ② 「同一个用户对同一个对象只算一次」不在本表约束内，那是 thumbup_like
--          (business, mid, message_id)、favorite_item (mid, oid, tp)、share_log (oid, mid, tp, day)
--          三个唯一键的职责；本表按状态变化追加行，同一对象可以被不同用户反复互动。
--       消费方仍必须按 event_id 去重：本表没有租约列，多副本发布会把同一行投两次。
-- 写入路径：Like/AddFav/DelFav/AddShare 在 TransactCtx 内写
--       （关系行/计数行与事件行同事务提交，装配见 internal/repository/eventaction.go），
--       计数没有变化的分支（重复点赞、当天重复分享）不写本表。
--       事务必须只含「互动写库 + 事件行」，禁止在事务内调用 Redis 影子计数器或下游 RPC。
-- 投递：internal/publisher 的发布循环按 id 升序投递，topic 由 event_type + schema_version 现场拼出
--       （`eventenvelope.Topic`），不在 yaml 里重复写 topic 字面量；分区键是 aggregate_id
--       （对象 ID 的十进制字符串），因此同一对象的事件落在同一分区。
-- 大文件与凭据：payload 只存动作、坐标、操作者 mid 与计数，禁止写入 IP、手机号、身份证、Token
--       与任何对象存储密钥（AGENTS.md §6、§7）。LikeReq.ip 因此不进事件。
-- 锁风险：CREATE TABLE IF NOT EXISTS 只取元数据锁，可在线执行；新建空表，索引无在线重建成本。
--       ListPending 走 idx_state_next_retry 前缀扫描并带 LIMIT，不做全表扫描。
-- 容量：每一次真实计数变化一行，且本服务没有清理任务，已发布行会无限增长；
--       热门对象的互动频率远高于稿件状态转换，积压速度按点赞 QPS 计。
--       归档/清理策略见服务 README「已知缺口」，本文件不做 DELETE 或分区（迁移只承担 DDL）。
-- 回滚：DROP TABLE IF EXISTS `engagement_outbox`;
--       （回滚前必须确认发布循环已停：表消失后 publisher 的 ListPending 会持续报错，
--        已投递但未被消费的事件无法重放，本表是唯一的事前记录点。
--        已提交但未投递的事件行会随表一起丢失，搜索索引里的热度从此永久落后且无人知晓。）
-- =====================================================================
CREATE TABLE IF NOT EXISTS `engagement_outbox` (
  `id`             BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键（发布器按此升序保证同对象顺序）',
  `event_id`       CHAR(26) COLLATE utf8mb4_bin NOT NULL COMMENT '事件唯一 ID（ULID，消费者据此幂等）：唯一键列，逐字节比较，不给默认值',
  `event_type`     VARCHAR(64) NOT NULL DEFAULT '' COMMENT '事件类型：engagement.action',
  `schema_version` INT         NOT NULL DEFAULT 1 COMMENT '事件 schema 版本',
  `aggregate_type` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '聚合根类型：content',
  `aggregate_id`   VARCHAR(64) NOT NULL DEFAULT '' COMMENT '聚合根 ID（对象 ID 十进制字符串，同时是分区键）',
  `payload`        MEDIUMTEXT  NOT NULL COMMENT '事件信封完整 JSON（common/eventenvelope.Envelope）',
  `state`          TINYINT     NOT NULL DEFAULT 0 COMMENT '发布状态：0 待发布、1 已发布、2 失败（超最大重试）',
  `retry_count`    INT         NOT NULL DEFAULT 0 COMMENT '已重试次数（用于指数退避）',
  `next_retry_at`  BIGINT      NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒，0 表示可立即投递）',
  `last_error`     VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次投递错误',
  `occurred_at`    BIGINT      NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒，与信封 occurred_at 对应）',
  `ctime`          BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒，发布器更新状态时刷新）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  KEY `idx_state_next_retry` (`state`, `next_retry_at`),
  KEY `idx_event_type_ctime` (`event_type`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='engagement 领域事件 Outbox 表：互动写事务内建行，发布器异步投递（幂等、退避重试、失败人工处理）';
