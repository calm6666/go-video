-- =====================================================================
-- catalog 服务 - 版权内容目录（作品/季/集/分区/标签）
-- =====================================================================
-- 数据所有者：catalog 服务（AGENTS.md §5）。PGC 目录不与 UGC 稿件混表。
-- 主键语义沿用参考模型：catalog_work.season_id 为作品主季 ID。
-- 分区/标签由运营维护，本服务只读（无写入 RPC），需另行初始化种子数据。
-- 回滚：DROP TABLE IF EXISTS `catalog_work`,`catalog_season`,`catalog_episode`,`catalog_zone`,`catalog_tag`;
-- =====================================================================

-- 作品主表
CREATE TABLE IF NOT EXISTS `catalog_work` (
  `season_id` BIGINT       NOT NULL AUTO_INCREMENT COMMENT '作品主季 ID（主键）',
  `title`     VARCHAR(255) NOT NULL DEFAULT '' COMMENT '标题',
  `cover`     VARCHAR(512) NOT NULL DEFAULT '' COMMENT '封面 URL',
  `typeid`    INT          NOT NULL DEFAULT 0 COMMENT '作品类型',
  `intro`     VARCHAR(2000) NOT NULL DEFAULT '' COMMENT '简介',
  `state`     TINYINT      NOT NULL DEFAULT 0 COMMENT '状态：0 草稿、1 上架、2 下架',
  `ctime`     BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`     BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`season_id`),
  KEY `idx_typeid_state` (`typeid`, `state`),
  KEY `idx_state_ctime` (`state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='版权作品主表';

-- 季表：work_id → catalog_work.season_id
CREATE TABLE IF NOT EXISTS `catalog_season` (
  `season_id` BIGINT       NOT NULL AUTO_INCREMENT COMMENT '本季 ID（主键）',
  `work_id`   BIGINT       NOT NULL DEFAULT 0 COMMENT '所属作品主季 ID',
  `season_no` INT          NOT NULL DEFAULT 0 COMMENT '季编号',
  `title`     VARCHAR(255) NOT NULL DEFAULT '' COMMENT '季标题',
  `cover`     VARCHAR(512) NOT NULL DEFAULT '' COMMENT '季封面',
  `state`     TINYINT      NOT NULL DEFAULT 0 COMMENT '状态：0 草稿、1 上架、2 下架',
  `ctime`     BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`     BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`season_id`),
  UNIQUE KEY `uniq_work_season` (`work_id`, `season_no`),
  KEY `idx_work_state` (`work_id`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='版权目录季表';

-- 集表：season_id → catalog_season.season_id；asset_id 引用 asset 服务媒资
CREATE TABLE IF NOT EXISTS `catalog_episode` (
  `epid`      BIGINT       NOT NULL AUTO_INCREMENT COMMENT '集 ID（主键）',
  `season_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '所属季 ID',
  `ep_no`     INT          NOT NULL DEFAULT 0 COMMENT '集编号',
  `title`     VARCHAR(255) NOT NULL DEFAULT '' COMMENT '集标题',
  `asset_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '关联媒资 ID（asset 服务拥有）',
  `duration`  BIGINT       NOT NULL DEFAULT 0 COMMENT '时长（秒）',
  `state`     TINYINT      NOT NULL DEFAULT 0 COMMENT '状态：0 草稿、1 上架、2 下架',
  `ctime`     BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`     BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`epid`),
  UNIQUE KEY `uniq_season_ep` (`season_id`, `ep_no`),
  KEY `idx_season_state` (`season_id`, `state`),
  KEY `idx_asset` (`asset_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='版权目录集表';

-- 分区表（运营维护种子数据，catalog 只读）
CREATE TABLE IF NOT EXISTS `catalog_zone` (
  `zoneid` INT         NOT NULL COMMENT '分区 ID',
  `name`   VARCHAR(64) NOT NULL DEFAULT '' COMMENT '分区名',
  `parent` INT         NOT NULL DEFAULT 0 COMMENT '父分区 ID（0 为顶级）',
  PRIMARY KEY (`zoneid`),
  KEY `idx_parent` (`parent`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='内容分区表';

-- 标签表（运营维护种子数据，catalog 只读；按名称检索走 idx_name）
CREATE TABLE IF NOT EXISTS `catalog_tag` (
  `tagid` BIGINT      NOT NULL AUTO_INCREMENT COMMENT '标签 ID（主键）',
  `name`  VARCHAR(64) NOT NULL DEFAULT '' COMMENT '标签名',
  PRIMARY KEY (`tagid`),
  KEY `idx_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='内容标签表';
