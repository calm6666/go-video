-- =====================================================================
-- danmaku 服务 - 弹幕主表 + 分段计数索引表
-- =====================================================================
-- 数据所有者：danmaku 服务（AGENTS.md §5）。其他服务禁止直接读写本目录的表，
-- 包括 comment 服务：弹幕不复用评论表的实时事务，两者只共享 oid 语义。
--
-- 分段与索引设计（弹幕量级远大于评论，必须按时间分段读）：
--   1. seg_no = progress_ms / (Danmaku.SegmentSeconds * 1000)，服务端计算后落库，
--      客户端拉取一律带 (oid, seg_no) 窗口，命中 idx_oid_seg 的范围扫描；
--      禁止按 progress_ms 逐条查询，避免大表回表。
--   2. 读取路径先查 Redis 段缓存 dm:seg:<oid>:<seg>，miss 段才回源本表并回填，
--      空段同样回填以防击穿。
--   3. 段计数派生到 danmaku_segment（小表），客户端据此决定预取窗口，
--      避免对主表做 COUNT(*)。
--   4. 归档：主表按 ctime 冷热分层，历史分段（如 180 天前）由 services/cron
--      迁移到归档库/对象存储；删除与驳回均为软状态（state=3/4），
--      保留行作为审计证据（AGENTS.md §8），因此不做物理 DELETE。
--   5. 写入幂等：uniq_idempotency(idempotency_key) 是最终防线，
--      键为服务端按 oid:mid:客户端键派生的 SHA-256 hex（64 字符），
--      天然避免不同用户选到同一客户端键。
--
-- 回滚：
--   DROP TABLE IF EXISTS `danmaku_segment`;
--   DROP TABLE IF EXISTS `danmaku`;
-- 锁风险：仅建表（CREATE TABLE IF NOT EXISTS），可重复执行，无锁风险；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `danmaku` (
  `dmid`              BIGINT        NOT NULL AUTO_INCREMENT COMMENT '弹幕 ID（主键）',
  `oid`               BIGINT        NOT NULL COMMENT '内容主键（视频 aid / 直播 room_id）',
  `aid`               BIGINT        NOT NULL DEFAULT 0 COMMENT '稿件 ID（归档投影用，缺省同 oid）',
  `mid`               BIGINT        NOT NULL DEFAULT 0 COMMENT '发送者用户 ID（不支持游客发送）',
  `progress_ms`       BIGINT        NOT NULL DEFAULT 0 COMMENT '时间轴位置（毫秒）',
  `mode`              TINYINT       NOT NULL DEFAULT 1 COMMENT '展示模式：1 滚动、2 底部、3 顶部、4 彩色、5 高级',
  `fontsize`          SMALLINT      NOT NULL DEFAULT 25 COMMENT '字号（点）',
  `color`             INT UNSIGNED  NOT NULL DEFAULT 16777215 COMMENT 'RGB 颜色整数值，默认白色',
  `content`           VARCHAR(100)  NOT NULL COMMENT '弹幕正文（存原文，屏蔽词只在判定侧生效）',
  `state`             TINYINT       NOT NULL DEFAULT 1 COMMENT '状态：0 正常、1 待审核、2 折叠、3 删除、4 审核驳回',
  `pool`              TINYINT       NOT NULL DEFAULT 2 COMMENT '弹幕池：1 普通池、2 审核池、4 屏蔽池',
  `seg_no`            INT           NOT NULL DEFAULT 0 COMMENT '时间分段号 = progress_ms / (SegmentSeconds*1000)',
  `idempotency_key`   VARCHAR(64)   NOT NULL COMMENT '幂等键（服务端派生哈希，客户端重试必须复用）',
  `moderation_task_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '单机审任务 ID（0 表示尚未送审）',
  `trace_id`          VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`             BIGINT        NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT        NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`dmid`),
  UNIQUE KEY `uniq_idempotency` (`idempotency_key`),
  KEY `idx_oid_seg` (`oid`, `seg_no`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_state_ctime` (`state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='弹幕主表（按 oid+seg_no 分段读取）';

-- 分段计数索引表：danmaku 的派生投影，允许与主表存在秒级偏差，
-- 可由 services/cron 从主表按 (oid, seg_no) 重算修复，不作为唯一事实源。
-- 回滚：DROP TABLE IF EXISTS `danmaku_segment`;
CREATE TABLE IF NOT EXISTS `danmaku_segment` (
  `id`     BIGINT  NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `oid`    BIGINT  NOT NULL COMMENT '内容主键',
  `seg_no` INT     NOT NULL DEFAULT 0 COMMENT '时间分段号',
  `count`  INT     NOT NULL DEFAULT 0 COMMENT '段内可下发弹幕数（state=0 且 pool=1）',
  `mtime`  BIGINT  NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_oid_seg` (`oid`, `seg_no`),
  KEY `idx_mtime` (`mtime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='弹幕分段计数索引表（派生投影，可重算）';
