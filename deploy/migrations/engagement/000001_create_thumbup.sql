-- 点赞用户记录
-- 幂等：唯一索引 (business, mid, message_id) 保证同用户对同对象只有一条记录。
-- owner：engagement 服务（deploy/migrations/engagement，库 go_video_engagement）；影响范围：新增 2 张表。
-- 回滚：DROP TABLE IF EXISTS `thumbup_stat`; DROP TABLE IF EXISTS `thumbup_like`;
CREATE TABLE IF NOT EXISTS `thumbup_like` (
  `id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `business`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务名（如 archive、dynamic）',
  `mid`        BIGINT       NOT NULL DEFAULT 0 COMMENT '点赞用户 ID',
  `up_mid`     BIGINT       NOT NULL DEFAULT 0 COMMENT '被点赞内容 UP 主 ID（用于通知）',
  `origin_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '来源 ID（如分区、UP 空间）',
  `message_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '对象 ID（如 aid、动态 ID）',
  `state`      TINYINT      NOT NULL DEFAULT 0 COMMENT '0 取消、1 like、2 dislike',
  `ctime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_business_mid_message` (`business`, `mid`, `message_id`),
  KEY `idx_origin_message` (`origin_id`, `message_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='点赞用户记录';

-- 点赞计数表
-- 每对 (business, origin_id, message_id) 一条；通过 ON DUPLICATE KEY UPDATE 维护。
CREATE TABLE IF NOT EXISTS `thumbup_stat` (
  `id`              BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `business`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务名',
  `origin_id`       BIGINT       NOT NULL DEFAULT 0 COMMENT '来源 ID',
  `message_id`      BIGINT       NOT NULL DEFAULT 0 COMMENT '对象 ID',
  `like_number`     BIGINT       NOT NULL DEFAULT 0 COMMENT '点赞数（展示用）',
  `dislike_number`  BIGINT       NOT NULL DEFAULT 0 COMMENT '点踩数',
  `like_change`     BIGINT       NOT NULL DEFAULT 0 COMMENT '运营修正点赞增量',
  `dislike_change`  BIGINT       NOT NULL DEFAULT 0 COMMENT '运营修正点踩增量',
  `ctime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_business_origin_message` (`business`, `origin_id`, `message_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='点赞计数表';
