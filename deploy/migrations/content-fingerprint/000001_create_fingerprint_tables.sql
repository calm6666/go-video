-- =====================================================================
-- content-fingerprint 服务 - 指纹任务与指纹事实
-- =====================================================================
-- 数据所有者：content-fingerprint（AGENTS.md §5）。用于重复投稿与版权比对，
-- 指纹 key/hash 存本表，向量或大对象放对象存储，不进 MySQL。
-- 回滚：DROP TABLE IF EXISTS `fingerprint_task`,`fingerprint_record`;
-- =====================================================================

-- 指纹抽取任务表
-- 唯一索引 (asset_id, fp_type)：同一媒资同种指纹只保留一条任务记录，
-- 失败重试由 UpdateResult 在原行上推进状态（PENDING → SUCCEEDED/FAILED）。
CREATE TABLE IF NOT EXISTS `fingerprint_task` (
  `id`        BIGINT      NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `task_id`   BIGINT      NOT NULL DEFAULT 0 COMMENT '业务任务 ID（跨服务引用）',
  `asset_id`  BIGINT      NOT NULL DEFAULT 0 COMMENT '关联媒资 ID',
  `fp_type`   TINYINT     NOT NULL DEFAULT 0 COMMENT '指纹类型：1 video、2 audio',
  `video_key` VARCHAR(128) NOT NULL DEFAULT '' COMMENT '视频指纹 key（SUCCEEDED 后回写）',
  `audio_key` VARCHAR(128) NOT NULL DEFAULT '' COMMENT '音频指纹 key（SUCCEEDED 后回写）',
  `state`     TINYINT     NOT NULL DEFAULT 1 COMMENT '任务状态：1 PENDING、2 SUCCEEDED、3 FAILED',
  `ctime`     BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`     BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_asset_fptype` (`asset_id`, `fp_type`),
  UNIQUE KEY `uniq_task_id` (`task_id`),
  KEY `idx_state_ctime` (`state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='指纹抽取任务表';

-- 指纹事实表：唯一索引 (asset_id, fp_type)，Upsert 保留最新一条。
CREATE TABLE IF NOT EXISTS `fingerprint_record` (
  `id`       BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `asset_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '媒资 ID',
  `fp_type`  TINYINT      NOT NULL DEFAULT 0 COMMENT '指纹类型：1 video、2 audio',
  `key`      VARCHAR(128) NOT NULL DEFAULT '' COMMENT '指纹 key（用于检索）',
  `hash`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '指纹哈希（用于精确比对）',
  `ctime`    BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_asset_fptype` (`asset_id`, `fp_type`),
  KEY `idx_key` (`key`),
  KEY `idx_hash` (`hash`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='内容指纹记录表';
