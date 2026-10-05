-- =====================================================================
-- ops-config 服务 - 专题两张表（专题主记录 / 专题条目）
-- =====================================================================
-- 数据库：go_video_ops_config。数据所有者：ops-config 服务。
--
-- 表与代码一一对应（列名严格取自 model/*.go 的 db tag 与 SQL 字符串）：
--   * ops_topic       model/ops_topic.go       Topic       —— 专题主记录（含引用列表与生效窗口）。
--   * ops_topic_item  model/ops_topic_item.go  TopicItem   —— 专题内的内容条目（全量覆盖语义）。
--
-- 边界（本文件最重要的结论，另见 services/ops-config/README.md）：
--   1. **分区与标签不归本服务**。catalog 服务已落地 catalog_zone / catalog_tag
--      （deploy/migrations/catalog/000001_create_catalog_tables.sql），AGENTS.md §5 也把
--      分区/标签判给 catalog。本表只有 zone_ids / tag_ids 两列**引用 ID**，
--      不存分区名、标签名、封面、排序等任何可变主数据 —— 否则改名与改图就会出现两份真相。
--      同理：item_id 只存内容主键（aid / season_id / epid），本服务不校验其存在性，
--      也不复制标题（连不上别人的库，跨库外键在 AGENTS.md §5 下被明确禁止）。
--   2. 「运营配置」归本服务：专题的编排、生效窗口、上下架与灰度都是运营配置行为；
--      被编排的对象（稿件、分区、标签）仍是内容域的。
--
-- 存储形态约定：
--   zone_ids / tag_ids 用 ",1,2,3," 形式存 VARCHAR，Go 侧 IDListContains 与 SQL 侧 IDListLike
--   （`LIKE '%,12,%'`）是同一条规则的两个实现，一致性由 model 单测锁定。
--   为什么不用关系表：读侧只需要「按引用过滤 + 展示全部引用」，从来不需要 JOIN 或聚合；
--   代价是按分区/标签反查只能走 LIKE（本表是**后台低频**表，千级行）。
--   model 的注释里已写明：若这个过滤进入运行时热路径，正确做法是加一张 ops_topic_zone_ref
--   关系表，而不是把 LIKE 搬进热路径 —— 这是留给后续演进的门，不是本期遗漏。
--
-- 唯一键与幂等：
--   1. uniq_slug(slug)：slug 是端上的**寻址标识**（下发后不可随意改名），必须唯一；
--      同时它就是 SaveTopic 新建时的幂等/冲突仲裁点（冲突 → ErrTopicSlugConflict）。
--      UpdateWithVersion 改 slug 前会先探一次 `slug = ? AND topic_id <> ?`，把驱动错误变成
--      业务错误，但真正的仲裁仍然是这个唯一键。
--   2. uniq_topic_position(topic_id, position)：SaveTopicItems 是全量覆盖
--      （事务内 DELETE by topic_id + 单条多值 INSERT），位置重复即撞键。
--      它同时保证「同一专题不会出现两个第 1 位」—— 位置是端上直接渲染的次序，
--      重复位置的后果是内容凭空少一条，很难从日志看出来。
--
-- 索引取自真实查询路径：
--   * idx_state_sort(state, sort, topic_id)：ListTopics 的
--     `WHERE state = ? ... ORDER BY sort ASC, topic_id ASC LIMIT ? OFFSET ?`
--     与 ListOnline 的排序部分（时间窗是 OR 条件，不进索引，由 SQL 过滤 + Go 侧 InWindow 复核）。
--     排序带 topic_id 作为 tiebreaker：sort 允许并列，没有它翻页会重复/漏行。
--   * ops_topic_item 的 ListByTopic（`WHERE topic_id = ? [AND state = ?] ORDER BY position ASC`）
--     直接走 uniq_topic_position 前缀，因此不另建 (topic_id, position) 索引。
--   * ops_topic_item.idx_ref(item_type, item_id, state)：内容下架时反查「哪些专题还挂着它」
--     （FindByItemRef 的 `WHERE item_type = ? AND item_id = ? AND state = ?`），
--     state 进索引是因为反查只要生效中的引用，否则每次下架都要人工排除停用行。
--
-- 无计数列说明：本表不存 item_count。「有多少条」必须 COUNT 出来，
-- 存下来就是一个需要靠触发器或双写维护的投影，而专题条目数是百级、COUNT 走索引前缀即可。
--
-- owner：平台治理（ops-config 服务）；影响范围：新增 2 张表，不改任何既有表。
-- 回滚：
--   DROP TABLE IF EXISTS `ops_topic_item`;
--   DROP TABLE IF EXISTS `ops_topic`;
--   专题是运营手工编排的数据，回滚前必须先导出（无自动重建来源）：
--   它与 ops_config_version 不同，没有任何地方存有它的全量历史，只能整表备份。
-- 锁风险：
--   * 新建空表，不锁既有表。
--   * SaveTopicItems 的事务是「DELETE 该专题全部条目 + 一次多值 INSERT（<= 500 行）」，
--     行锁范围限定在单个 topic_id 内，持锁毫秒级；不同专题之间不互相阻塞。
--     **不要在事务里调 audit 或其它 RPC**（与 000001 同一约束）。
--   * SaveTopic 的乐观锁是条件 UPDATE（`WHERE topic_id = ? AND version = ?`）+ RowsAffected，
--     不用 SELECT ... FOR UPDATE：两个运营同时改同一个专题时，后一个必须看见失败并回读，
--     而不是被前一个的锁阻塞到超时。
-- =====================================================================

-- 专题主记录：编排与生效窗口；分区/标签只存引用 ID。
CREATE TABLE IF NOT EXISTS `ops_topic` (
  `topic_id`    BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键；后台与缓存按它定位',
  `slug`        VARCHAR(64)  NOT NULL COMMENT '稳定标识，格式 ^[a-z0-9][a-z0-9_-]{1,63}$（model.topicSlugRe）。端上按此寻址，下发后改名等于打不开，故建唯一键',
  `title`       VARCHAR(128) NOT NULL DEFAULT '' COMMENT '专题名（必填 → ErrTopicTitleRequired）',
  `description` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '简介，可空',
  `cover`       VARCHAR(512) NOT NULL DEFAULT '' COMMENT '封面：只存展示地址或 object_key 引用，不存二进制、不存签名地址（AGENTS.md §5「大文件与对象存储地址不入库」的口径）',
  `zone_ids`    VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '引用 catalog_zone.zoneid 列表，存储形态 ",1,2,"，上限 model.MaxTopicRefIDs（可被 OpsTopic.MaxRefIDs 收紧）。**绝不存分区名**：分区主数据归 catalog（AGENTS.md §5）',
  `tag_ids`     VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '引用 catalog_tag.tagid 列表，同上。本服务 DataSource 不指向 go_video_catalog，因此既读不到也不校验其存在性 —— 这是刻意的边界，不是缺口',
  `state`       TINYINT      NOT NULL DEFAULT 2 COMMENT '1 上架、2 下架。默认 2：新建即上架会让一个还没挂条目的空专题直接被端上取到',
  `sort`        INT          NOT NULL DEFAULT 0 COMMENT '列表排序，小者在前；允许并列，并列时以 topic_id 决定次序（见 idx_state_sort）',
  `start_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '生效窗口起（Unix 秒），0 表示不限',
  `end_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '生效窗口止（Unix 秒），0 表示不限；非 0 时 end_at<=start_at 即拒（ErrTopicTimeRangeInvalid）',
  `version`     BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观锁版本，SaveTopicReq.expect_version 比对的就是它；每次成功更新 +1',
  `operator_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '最后操作人 admin_id（引用 operation，不复制资料）',
  `ctime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '最后修改时间（Unix 秒）。SaveTopicItems 也会 TouchMtime 它，给「按 mtime 增量拉取」的调用方一个信号',
  PRIMARY KEY (`topic_id`),
  UNIQUE KEY `uniq_slug` (`slug`),
  KEY `idx_state_sort` (`state`, `sort`, `topic_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='运营专题主表（编排与生效窗口；分区/标签只存 catalog 的 ID 引用，不复制主数据）';

-- 专题条目：全量覆盖语义，位置唯一且从 1 连续。
CREATE TABLE IF NOT EXISTS `ops_topic_item` (
  `id`          BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `topic_id`    BIGINT      NOT NULL COMMENT '所属专题（ops_topic.topic_id）；不建外键，同库同服务写入',
  `item_type`   VARCHAR(16) NOT NULL COMMENT '内容类型：ugc_video / pgc_season / pgc_episode（值域固定，新增需评审）。专题内**不允许**再套专题（model.ValidItemType(s, false)）—— 嵌套引用会让一次下架产生不可预测的连锁空洞',
  `item_id`     VARCHAR(32) NOT NULL COMMENT '内容主键字符串（aid / season_id / epid）。只存引用：不校验存在性、不复制标题（AGENTS.md §5）',
  `position`    INT         NOT NULL COMMENT '专题内次序，小者在前；SaveTopicItems 要求 1..n 连续无洞（ErrTopicPositionNotSequential）—— 有洞说明调用方漏传，而不是运营想留空位',
  `state`       TINYINT     NOT NULL DEFAULT 1 COMMENT '1 生效、2 移除。全量覆盖是「以这次集合为准」，因此停用行只可能来自更早的写入或未来的单条停用接口',
  `operator_id` BIGINT      NOT NULL DEFAULT 0 COMMENT '最后操作人 admin_id',
  `ctime`       BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）；整批共用同一个 ts，便于事后确认「这一批是同一动作」',
  `mtime`       BIGINT      NOT NULL DEFAULT 0 COMMENT '最后修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_topic_position` (`topic_id`, `position`),
  KEY `idx_ref` (`item_type`, `item_id`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='专题条目表（引用内容主键 + 位置；由 SaveTopicItems 在一个事务内全量覆盖）';
