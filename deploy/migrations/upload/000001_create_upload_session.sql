-- =====================================================================
-- upload 服务 - 上传会话表
-- =====================================================================
-- 数据所有者：upload 服务（AGENTS.md §5）。大文件本体在对象存储，
-- 本表只存会话元数据；asset_id 由 asset 服务回填，仅存引用。
-- 回滚：DROP TABLE IF EXISTS `upload_session`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `upload_session` (
  `upload_id`    VARCHAR(64)  NOT NULL COMMENT '上传会话 ID（业务主键）',
  `mid`          BIGINT       NOT NULL DEFAULT 0 COMMENT '用户 ID',
  `filename`     VARCHAR(255) NOT NULL DEFAULT '' COMMENT '文件名',
  `size`         BIGINT       NOT NULL DEFAULT 0 COMMENT '文件总大小（字节）',
  `typeid`       INT          NOT NULL DEFAULT 0 COMMENT '稿件类型 ID',
  `bucket`       VARCHAR(128) NOT NULL DEFAULT '' COMMENT '对象存储桶',
  `object_key`   VARCHAR(512) NOT NULL DEFAULT '' COMMENT '对象 key',
  `state`        TINYINT      NOT NULL DEFAULT 0 COMMENT '会话状态（SessionState 常量）',
  `chunk_size`   BIGINT       NOT NULL DEFAULT 0 COMMENT '分片大小（字节）',
  `total_chunks` INT          NOT NULL DEFAULT 0 COMMENT '分片总数',
  `md5`          CHAR(32)     NOT NULL DEFAULT '' COMMENT '完整文件 MD5',
  `asset_id`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '关联媒资 ID（asset 服务回填）',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`upload_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_md5` (`md5`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='上传会话表';
