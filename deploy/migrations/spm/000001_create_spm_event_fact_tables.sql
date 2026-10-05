-- =====================================================================
-- spm 服务 - 事件接入状态、死信留档与脱敏行为事实
-- =====================================================================
-- 库名：go_video_spm（见 services/spm/etc/spm.v1.yaml 的 DataSource）。
-- 数据所有者：spm 服务（AGENTS.md §7「用户行为分析」链路）。
--   本文件三张表分别对应 services/spm/model/consumer_offset.go、dead_letter.go、
--   behavior_event.go，列名与 db 标签逐一对应，不得只改一侧。
--   本库只保存行为事实与派生投影：稿件/媒资/版权窗口归 video/asset/rights，
--   用户主资料归 account/user-profile；这里只存它们的业务主键（content_id/aid/zone_id/
--   catalog_item_id/mid/target_mid），不建跨库外键。
-- 领域边界（AGENTS.md §7）：本文件不出现任何广告位、投放、计费或商业化报表字段；
--   ec_event_record.spm / EventContext.spm 那类「行为链路标识」也不落到本库列名里，
--   本库只有 action_key / dim_key 这类分析维度。
-- 隐私：不存明文设备号、手机号与原始 IP。行为主体维度只有 mid（登录用户主键）与
--   pseudonym（上游 event-collector / playback 已加盐摘要的 device_hash、mid_hash、
--   ip_segment）。明文摘要在消费者 mapping 阶段就已丢弃，本库无可还原字段。
-- 幂等：
--   spm_consumer_offset.uniq_event_id  是「投递是否处理过」的幂等真值；
--   spm_dead_letter.uniq_event_id + uniq_payload_digest 双键去重（event_id 为空写 NULL）；
--   spm_behavior_event.uniq_event_id   是事实行幂等键（INSERT ... ON DUPLICATE 自赋值）。
-- 回滚：
--   DROP TABLE IF EXISTS `spm_behavior_event`;
--   DROP TABLE IF EXISTS `spm_dead_letter`;
--   DROP TABLE IF EXISTS `spm_consumer_offset`;
-- 锁风险：
--   全部为新建空表，索引在空表上建立，无在线锁风险。
--   spm_behavior_event 是高写入大表（每条通过白名单的行为事件一行），后续分区/归档
--   按 event_day 或 ctime 由运维轮处理；本期只按 DeleteExpired 的 ctime 游标分批清理，
--   每批带 LIMIT（见 model.clampBatch），避免无界 DELETE 持有大量行锁。
--   本表有 8 个二级索引，写放大明显，是「事实可重算」这一设计的代价；新增维度索引
--   前必须先量一次写入 TPS 余量。
-- =====================================================================

-- 消费状态与位点表：事件消费状态机 + 可重放位点（docs/api-and-events.md §6）。
-- 状态机：received -> processing -> succeeded -> / -> retry(退避) -> ... -> dead_letter。
-- 写入方：本服务消费者（未来 consumer 轮）；读取方：Spm.ListConsumerState。
-- 幂等：uniq_event_id 让重复/乱序投递只能读到既有状态，绝不会把 succeeded 改回 processing。
CREATE TABLE IF NOT EXISTS `spm_consumer_offset` (
  `id`            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `event_id`      VARCHAR(64)     NOT NULL COMMENT '信封 event_id（幂等真值，唯一索引）',
  `event_type`    VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '事件类型，如 behavior.play / playback.heartbeat',
  `topic`         VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '来源 topic（<event_type>.v<schema_version>）',
  `partition_no`  INT             NOT NULL DEFAULT 0 COMMENT 'Kafka 分区（kq 不上报时为 0）',
  `msg_offset`    BIGINT          NOT NULL DEFAULT 0 COMMENT 'Kafka 位点（消费到哪了的证据）',
  `state`         VARCHAR(16)     NOT NULL DEFAULT 'received' COMMENT '消费状态：received/processing/succeeded/retry/dead_letter',
  `retry_count`   INT             NOT NULL DEFAULT 0 COMMENT '已失败次数（退避序列的依据）',
  `next_retry_at` BIGINT          NOT NULL DEFAULT 0 COMMENT '退避到期时间（Unix 秒，0 表示可立即处理）',
  `last_error`    VARCHAR(512)    NOT NULL DEFAULT '' COMMENT '最近失败原因（已脱敏截断，不含堆栈与 SQL）',
  `payload`       MEDIUMTEXT      NULL COMMENT '原始信封 JSON：只在失败路径保留，成功行由 MarkSucceeded 清空（不长期堆积原文）',
  `occurred_at`   BIGINT          NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒，来自信封 RFC3339 转换）',
  `consume_from`  VARCHAR(16)     NOT NULL DEFAULT '' COMMENT '首次启动起点（first/last），位点可观测用',
  `ctime`         BIGINT          NOT NULL DEFAULT 0 COMMENT '首次收到时间（Unix 秒）',
  `mtime`         BIGINT          NOT NULL DEFAULT 0 COMMENT '状态变更时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  -- 退避重投扫描：state=retry 且 next_retry_at 到期，按到期时间先进先出
  KEY `idx_state_next_retry` (`state`, `next_retry_at`),
  -- ListConsumerState 的 topic × state 汇总
  KEY `idx_topic_state` (`topic`, `state`),
  -- 清理已终结记录：state=succeeded 且 ctime 早于保留期
  KEY `idx_state_ctime` (`state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='事件消费状态机与位点（幂等真值 + 可重放证据链），不保存成功事件的原文';

-- 死信留档表：只存摘要与脱敏前缀，重放靠 event_id 回上游取原文。
-- 写入方：本服务消费者判死路径；读取方：Spm.ListDeadLetters（只读）。
-- 幂等：uniq_event_id（信封可解析）+ uniq_payload_digest（信封不可解析时按内容去重）。
--   event_id 允许 NULL —— MySQL 的 NULL 不参与唯一约束，坏消息才能各留一行，
--   因此读取侧必须 COALESCE(event_id,'')（见 model.deadLetterColumns）。
CREATE TABLE IF NOT EXISTS `spm_dead_letter` (
  `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `event_id`        VARCHAR(64)     NULL COMMENT '信封 event_id；信封不可解析时为 NULL（不写空串，避免互相撞唯一键）',
  `event_type`      VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '事件类型（不可解析时为空串）',
  `topic`           VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '来源 topic',
  `partition_no`    INT             NOT NULL DEFAULT 0 COMMENT 'Kafka 分区（重放定位）',
  `msg_offset`      BIGINT          NOT NULL DEFAULT 0 COMMENT 'Kafka 位点（重放定位）',
  `payload_digest`  VARCHAR(80)     NOT NULL COMMENT 'sha256:<hex>，判断两条投递是否同一内容（唯一索引）',
  `payload_preview` VARCHAR(512)    NOT NULL DEFAULT '' COMMENT '脱敏前缀，不含行为原文与标识符',
  `reason`          VARCHAR(512)    NOT NULL DEFAULT '' COMMENT '判死原因（稳定错误名，不做人读拼接）',
  `error_count`     INT             NOT NULL DEFAULT 0 COMMENT '同一摘要/事件累计失败次数（重复投递识别）',
  `state`           VARCHAR(16)     NOT NULL DEFAULT 'open' COMMENT '留档状态：open/replayed/ignored',
  `operator`        VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '处理人（重放/忽略留痕）',
  `processed_at`    BIGINT          NOT NULL DEFAULT 0 COMMENT '人工处理时间（Unix 秒，0 表示未处理）',
  `ctime`           BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  UNIQUE KEY `uniq_payload_digest` (`payload_digest`),
  -- 死信列表按 topic + 状态过滤，倒序翻页走 (topic, state, id)
  KEY `idx_topic_state` (`topic`, `state`),
  -- 健康检查/告警：待处理死信数（state=open + 时间窗）
  KEY `idx_state_ctime` (`state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='死信留档（只存摘要与脱敏前缀）：重放需要原文时按 event_id 回上游，不在本服务留副本';

-- 脱敏行为事实表：spm 唯一的事实源，所有指标/画像/留存投影都必须能从它重算。
-- 写入方：只有本服务消费者的 InsertIfAbsent（契约层不提供事件明细写 RPC，AGENTS.md §7）。
-- 字段来源对齐（见 services/spm/README.md「上游字段对齐」）：
--   content_id/content_type/aid/target_mid/session -> 上游事件同名字段；
--   content_type 取值在本库统一为整数 1 UGC、2 PGC、3 直播（playback/engagement/
--   content.published 侧是整数，event-collector 的 behavior.* 侧是字符串枚举，
--   由消费者 mapping 阶段归一，字符串->整数的对应关系缺失是已登记的契约缺口）；
--   zone_id 上游 behavior.* 与 playback.heartbeat 都不带，只能由 mapping 阶段查
--   spm_content_projection（投影自 content.published 的 typeid）补齐，补不到就是 0；
--   num_value 承载归一化数值（播放进度秒数、completion 完播比例、结果位次、首帧耗时等）。
-- 幂等：uniq_event_id + InsertIfAbsent 的 `event_id = event_id` 自赋值，重复投递 affected=0。
CREATE TABLE IF NOT EXISTS `spm_behavior_event` (
  `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `event_id`        VARCHAR(64)     NOT NULL COMMENT '信封 event_id（幂等真值，唯一索引）',
  `event_type`      VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '事件类型：behavior.play / playback.heartbeat / engagement.action …（白名单见 model.SupportedEventType）',
  `action_key`      VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '归一化动作键（play/finish/click/exposure/…），与 event_type 分开存以便跨通道对齐口径',
  `source_channel`  TINYINT         NOT NULL DEFAULT 0 COMMENT '来源通道：1 event-collector 归一化、2 领域原始事件、3 content.published（只服务投影，不参与行为计数）',
  `schema_version`  INT             NOT NULL DEFAULT 1 COMMENT '信封 schema_version，回放时按版本解析 payload',
  `occurred_at`     BIGINT          NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒，由信封 RFC3339 转换；解析失败按坏消息处理，不用当前时间顶替）',
  `event_day`       BIGINT          NOT NULL DEFAULT 0 COMMENT '发生日 UTC 零点（天级窗口与留存分桶的对齐键）',
  `mid`             BIGINT          NOT NULL DEFAULT 0 COMMENT '行为发起用户 mid，0 表示未登录或事件只带假名（playback.heartbeat 只给 mid_hash）',
  `pseudonym`       VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '假名摘要：device_hash / mid_hash / ip_segment，均为上游已加盐摘要，本库不含还原材料',
  `pseudonym_kind`  VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '假名来源：device / mid_hash / ip_segment',
  `aid`             BIGINT          NOT NULL DEFAULT 0 COMMENT '稿件 aid（只有 event-collector 的 behavior.* 通道带；UGC 场景 content_id 与 aid 同值）',
  `content_id`      BIGINT          NOT NULL DEFAULT 0 COMMENT '内容主键（上游 content_id：UGC=aid、PGC=episode_id）',
  `content_type`    TINYINT         NOT NULL DEFAULT 0 COMMENT '内容类型：0 不适用、1 UGC、2 PGC、3 直播（与 playback/engagement 契约编号一致）',
  `zone_id`         BIGINT          NOT NULL DEFAULT 0 COMMENT '分区（映射自上游 typeid/zoneid，behavior.* 事件本身不带，0 = 分区未知）',
  `catalog_item_id` BIGINT          NOT NULL DEFAULT 0 COMMENT '版权内容作品级条目 ID（本期上游只给 episode 级 content_id，故该列多为 0，见 README 契约缺口）',
  `target_mid`      BIGINT          NOT NULL DEFAULT 0 COMMENT '被作用用户（关注/点赞 UP 主等），0 表示无',
  `dim_key`         VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '受控附加维度名（quality/result_index/…），禁止自由文本',
  `dim_value`       VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '受控附加维度值（枚举/短串，不存关键词原文与标题）',
  `num_value`       DOUBLE          NOT NULL DEFAULT 0 COMMENT '归一化数值（进度秒数、完播比例、位次、首帧耗时等）',
  `trace_id`        VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `topic`           VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '来源 topic（含版本后缀），排障与重放定位',
  `partition_no`    INT             NOT NULL DEFAULT 0 COMMENT 'Kafka 分区（kq 不上报时为 0）',
  `msg_offset`      BIGINT          NOT NULL DEFAULT 0 COMMENT 'Kafka 位点（可重放定位）',
  `ctime`           BIGINT          NOT NULL DEFAULT 0 COMMENT '落库时间（Unix 秒，清理保留期按此列）',
  `mtime`           BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒，事实行只写不改，保留以对齐全库约定）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  -- AggregateWindow / ScanForRetention 的公共条件是 event_type IN (...) AND occurred_at BETWEEN
  KEY `idx_event_type_occurred` (`event_type`, `occurred_at`),
  -- 主体维度聚合：每个可分组列一条 (列, occurred_at) 索引，列名与 model.allowedSubjectColumns 一一对应
  KEY `idx_content_occurred` (`content_type`, `content_id`, `occurred_at`),
  KEY `idx_aid_occurred` (`aid`, `occurred_at`),
  KEY `idx_zone_occurred` (`zone_id`, `occurred_at`),
  KEY `idx_catalog_item_occurred` (`catalog_item_id`, `occurred_at`),
  KEY `idx_target_mid_occurred` (`target_mid`, `occurred_at`),
  -- 留存：按 mid 游标扫描区间内活跃用户（GROUP BY mid）与按天口径
  KEY `idx_mid_occurred` (`mid`, `occurred_at`),
  KEY `idx_mid_event_day` (`mid`, `event_day`),
  -- 事实保留期清理：ctime 游标分批删除
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='脱敏后的行为事件事实（唯一事实源）：窗口指标、兴趣画像与留存投影都可由它重算';
