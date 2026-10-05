-- =====================================================================
-- video 服务 - 稿件版本表
-- =====================================================================
-- 数据所有者：video 服务。asset_id 指向 asset 服务拥有的媒资，仅存引用。
-- 唯一索引 (aid, version) 保证版本号不重复。
-- 回滚：DROP TABLE IF EXISTS `video_version`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `video_version` (
  `id`       BIGINT      NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `aid`      BIGINT      NOT NULL DEFAULT 0 COMMENT '稿件 ID',
  `version`  BIGINT      NOT NULL DEFAULT 0 COMMENT '版本号（稿件内递增）',
  `asset_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '关联媒资 ID（asset 服务拥有）',
  `state`    TINYINT     NOT NULL DEFAULT 0 COMMENT '版本状态',
  `ctime`    BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_aid_version` (`aid`, `version`),
  KEY `idx_asset` (`asset_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='稿件版本表';
