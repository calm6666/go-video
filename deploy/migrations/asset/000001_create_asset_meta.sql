-- =====================================================================
-- asset 服务 - 媒资元数据表
-- =====================================================================
-- 数据所有者：asset 服务。不把大文件写入 MySQL，只存对象存储引用与探测结果。
-- 回滚：DROP TABLE IF EXISTS `asset_meta`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `asset_meta` (
  `asset_id`   BIGINT       NOT NULL AUTO_INCREMENT COMMENT '媒资 ID（主键）',
  `upload_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '关联上传会话（upload 服务）',
  `mid`        BIGINT       NOT NULL DEFAULT 0 COMMENT '上传用户 ID',
  `bucket`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '对象存储桶',
  `object_key` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '对象键',
  `size`       BIGINT       NOT NULL DEFAULT 0 COMMENT '文件大小（字节）',
  `md5`        CHAR(32)     NOT NULL DEFAULT '' COMMENT '文件 MD5',
  `duration`   BIGINT       NOT NULL DEFAULT 0 COMMENT '时长（毫秒）',
  `width`      INT          NOT NULL DEFAULT 0 COMMENT '视频宽',
  `height`     INT          NOT NULL DEFAULT 0 COMMENT '视频高',
  `codec`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '编码',
  `state`      TINYINT      NOT NULL DEFAULT 0 COMMENT '媒资状态（model 状态常量）',
  `ctime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`asset_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  KEY `idx_md5` (`md5`),
  KEY `idx_upload` (`upload_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='媒资元数据表';
