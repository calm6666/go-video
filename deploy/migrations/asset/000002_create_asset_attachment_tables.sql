-- =====================================================================
-- asset 服务 - 封面表
-- =====================================================================
-- 数据所有者：asset 服务。封面与大文件一样只存对象存储引用。
-- 回滚：DROP TABLE IF EXISTS `asset_cover`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `asset_cover` (
  `cover_id`   BIGINT       NOT NULL AUTO_INCREMENT COMMENT '封面 ID（主键）',
  `asset_id`   BIGINT       NOT NULL DEFAULT 0 COMMENT '关联媒资 ID',
  `bucket`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '对象存储桶',
  `object_key` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '对象键',
  `width`      INT          NOT NULL DEFAULT 0 COMMENT '宽',
  `height`     INT          NOT NULL DEFAULT 0 COMMENT '高',
  `ctime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`cover_id`),
  KEY `idx_asset` (`asset_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='媒资封面表';

-- 字幕表
-- 同一媒资可有多语言字幕；(asset_id, lang) 唯一保证同语言只保留一条。
-- 回滚：DROP TABLE IF EXISTS `asset_subtitle`;
CREATE TABLE IF NOT EXISTS `asset_subtitle` (
  `sub_id`     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '字幕 ID（主键）',
  `asset_id`   BIGINT       NOT NULL DEFAULT 0 COMMENT '关联媒资 ID',
  `lang`       VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '语言代码',
  `bucket`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '对象存储桶',
  `object_key` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '对象键',
  `ctime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`sub_id`),
  UNIQUE KEY `uniq_asset_lang` (`asset_id`, `lang`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='媒资字幕表';

-- 截图表（转码/探测产物，按媒资时间点保存）
-- 回滚：DROP TABLE IF EXISTS `asset_screenshot`;
CREATE TABLE IF NOT EXISTS `asset_screenshot` (
  `shot_id`    BIGINT       NOT NULL AUTO_INCREMENT COMMENT '截图 ID（主键）',
  `asset_id`   BIGINT       NOT NULL DEFAULT 0 COMMENT '关联媒资 ID',
  `bucket`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '对象存储桶',
  `object_key` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '对象键',
  `timestamp`  BIGINT       NOT NULL DEFAULT 0 COMMENT '画面时间点（毫秒）',
  `ctime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`shot_id`),
  KEY `idx_asset_timestamp` (`asset_id`, `timestamp`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='媒资截图表';
