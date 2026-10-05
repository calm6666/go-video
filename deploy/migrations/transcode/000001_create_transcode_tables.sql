-- =====================================================================
-- transcode 服务 - 转码任务表
-- =====================================================================
-- 数据所有者：transcode 服务。task_id 为自增主键（model.Insert 不写该列）。
-- 大文件不进 MySQL，仅存输入/输出对象存储引用（AGENTS.md §5）。
-- 回滚：DROP TABLE IF EXISTS `transcode_task`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `transcode_task` (
  `task_id`       BIGINT       NOT NULL AUTO_INCREMENT COMMENT '任务 ID（主键）',
  `asset_id`      BIGINT       NOT NULL DEFAULT 0 COMMENT '媒资 ID（asset 服务拥有）',
  `template_id`   BIGINT       NOT NULL DEFAULT 0 COMMENT '转码模板 ID',
  `input_bucket`  VARCHAR(128) NOT NULL DEFAULT '' COMMENT '输入对象存储桶',
  `input_key`     VARCHAR(512) NOT NULL DEFAULT '' COMMENT '输入对象 key',
  `output_bucket` VARCHAR(128) NOT NULL DEFAULT '' COMMENT '输出对象存储桶',
  `output_key`    VARCHAR(512) NOT NULL DEFAULT '' COMMENT '输出对象 key',
  `state`         TINYINT      NOT NULL DEFAULT 0 COMMENT '任务状态（TaskState 常量）',
  `progress`      INT          NOT NULL DEFAULT 0 COMMENT '进度（0-100）',
  `errno`         INT          NOT NULL DEFAULT 0 COMMENT '错误码',
  `err_msg`       VARCHAR(500) NOT NULL DEFAULT '' COMMENT '错误信息',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`task_id`),
  KEY `idx_asset` (`asset_id`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  KEY `idx_template` (`template_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='转码任务表';

-- 转码模板表
-- template_id 自增主键；name 唯一防止重名模板。
-- 回滚：DROP TABLE IF EXISTS `transcode_template`;
CREATE TABLE IF NOT EXISTS `transcode_template` (
  `template_id`     BIGINT      NOT NULL AUTO_INCREMENT COMMENT '模板 ID（主键）',
  `name`            VARCHAR(128) NOT NULL DEFAULT '' COMMENT '模板名',
  `codec`           VARCHAR(64) NOT NULL DEFAULT '' COMMENT '编码器（h264/hevc/aac 等）',
  `width`           INT         NOT NULL DEFAULT 0 COMMENT '视频宽（0 自适应）',
  `height`          INT         NOT NULL DEFAULT 0 COMMENT '视频高（0 自适应）',
  `bitrate`         INT         NOT NULL DEFAULT 0 COMMENT '目标码率（kbps，0 自适应）',
  `fps`             INT         NOT NULL DEFAULT 0 COMMENT '帧率（0 跟随源）',
  `segment_seconds` INT         NOT NULL DEFAULT 0 COMMENT 'HLS 分片时长（秒，0 不分片）',
  `ctime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`template_id`),
  UNIQUE KEY `uniq_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='转码模板表';
