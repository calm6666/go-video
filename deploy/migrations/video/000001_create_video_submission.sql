-- =====================================================================
-- video 服务 - 稿件主表
-- =====================================================================
-- 数据所有者：video 服务（AGENTS.md §5）。其他服务禁止直接读写本表。
-- state 取值与 services/video/model 的 SubmissionState 常量一致，
-- 状态迁移只能由 video 服务按状态机推进（AGENTS.md §8）。
-- 回滚：DROP TABLE IF EXISTS `video_submission`;
-- 锁风险：仅建表，无锁风险；后续变更新增迁移文件，禁止修改本文件。
-- =====================================================================
CREATE TABLE IF NOT EXISTS `video_submission` (
  `aid`    BIGINT       NOT NULL AUTO_INCREMENT COMMENT '稿件 ID（主键）',
  `mid`    BIGINT       NOT NULL DEFAULT 0 COMMENT '投稿用户 ID',
  `title`  VARCHAR(255) NOT NULL DEFAULT '' COMMENT '标题',
  `desc`   VARCHAR(2000) NOT NULL DEFAULT '' COMMENT '简介',
  `cover`  VARCHAR(512) NOT NULL DEFAULT '' COMMENT '封面 URL',
  `typeid` INT          NOT NULL DEFAULT 0 COMMENT '分区 ID（catalog_zone）',
  `tag`    VARCHAR(512) NOT NULL DEFAULT '' COMMENT '标签（逗号分隔）',
  `state`  TINYINT      NOT NULL DEFAULT 0 COMMENT '稿件状态（SubmissionState 枚举，0 DRAFT）',
  `ctime`  BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`  BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`aid`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  KEY `idx_typeid_ctime` (`typeid`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='视频稿件主表';
