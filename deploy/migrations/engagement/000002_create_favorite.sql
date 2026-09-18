-- 收藏夹表
CREATE TABLE IF NOT EXISTS `favorite_folder` (
  `fid`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '收藏夹 ID',
  `mid`         BIGINT       NOT NULL DEFAULT 0 COMMENT '用户 ID',
  `name`        VARCHAR(100) NOT NULL DEFAULT '' COMMENT '收藏夹名',
  `description` VARCHAR(500) NOT NULL DEFAULT '' COMMENT '收藏夹描述',
  `cover`       VARCHAR(500) NOT NULL DEFAULT '' COMMENT '封面 URL',
  `public`      TINYINT      NOT NULL DEFAULT 0 COMMENT '0 私密、1 公开',
  `state`       TINYINT      NOT NULL DEFAULT 0 COMMENT '0 正常、1 删除',
  `count`       INT          NOT NULL DEFAULT 0 COMMENT '收藏数量快照',
  `ctime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`fid`),
  KEY `idx_mid_state` (`mid`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='收藏夹';

-- 收藏项表
-- 幂等：唯一索引 (mid, oid, tp) 保证同用户对同对象只收藏一次。
-- 收藏在不同收藏夹间移动通过更新 fid 实现，不重复插入。
CREATE TABLE IF NOT EXISTS `favorite_item` (
  `id`     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `oid`    BIGINT       NOT NULL DEFAULT 0 COMMENT '目标 ID',
  `mid`    BIGINT       NOT NULL DEFAULT 0 COMMENT '用户 ID',
  `fid`    BIGINT       NOT NULL DEFAULT 0 COMMENT '收藏夹 ID',
  `tp`     TINYINT      NOT NULL DEFAULT 0 COMMENT '收藏类型：2 视频等',
  `otype`  TINYINT      NOT NULL DEFAULT 0 COMMENT '目标子类型',
  `state`  TINYINT      NOT NULL DEFAULT 0 COMMENT '0 正常、1 已取消',
  `ctime`  BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`  BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_oid_tp` (`mid`, `oid`, `tp`),
  KEY `idx_mid_fid` (`mid`, `fid`),
  KEY `idx_oid_tp` (`oid`, `tp`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='收藏项';
