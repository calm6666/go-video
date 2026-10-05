-- =====================================================================
-- upload 服务 - 上传分片表
-- =====================================================================
-- 唯一索引 (upload_id, chunk_no) 保证分片清单幂等（InsertBatch 重放安全）。
-- 取消上传按 upload_id 清理（DeleteByUpload）。
-- owner：upload 服务（deploy/migrations/upload，库 go_video_upload）；影响范围：新增 1 张表。
-- 回滚：DROP TABLE IF EXISTS `upload_chunk`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `upload_chunk` (
  `id`        BIGINT      NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `upload_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '上传会话 ID',
  `chunk_no`  INT         NOT NULL DEFAULT 0 COMMENT '分片序号（从 1 开始）',
  `size`      BIGINT      NOT NULL DEFAULT 0 COMMENT '分片大小（字节）',
  `etag`      VARCHAR(128) NOT NULL DEFAULT '' COMMENT '分片 ETag（OSS 返回）',
  `state`     TINYINT     NOT NULL DEFAULT 0 COMMENT '分片状态（ChunkState 常量）',
  `ctime`     BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`     BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_upload_chunk` (`upload_id`, `chunk_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='上传分片表';
