-- =====================================================================
-- feed 服务 - 动态与关注流
-- =====================================================================
-- 数据所有者：feed 服务（AGENTS.md §5）。feed_inbox 是 fan-out 投影，
-- 唯一索引 (mid, feed_id) 保证重复投递不产生第二条收件记录。
-- 回滚：DROP TABLE IF EXISTS `feed_outbox`,`feed_inbox`,`feed_pin`,`feed_unread`;
-- =====================================================================

-- 动态主表（作者维度事实）
CREATE TABLE IF NOT EXISTS `feed_outbox` (
  `id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '动态 ID（主键）',
  `mid`        BIGINT       NOT NULL DEFAULT 0 COMMENT '发布者 ID',
  `oid`        BIGINT       NOT NULL DEFAULT 0 COMMENT '对象 ID（如视频稿件 ID）',
  `otype`      TINYINT      NOT NULL DEFAULT 0 COMMENT '对象类型：1 UGC 视频、2 PGC 番剧、3 直播、4 专栏',
  `action`     TINYINT      NOT NULL DEFAULT 0 COMMENT '动作类型：1 发布、2 转发、3 修改',
  `state`      TINYINT      NOT NULL DEFAULT 0 COMMENT '0 正常、1 已修改、2 已删除',
  `title`      VARCHAR(255) NOT NULL DEFAULT '' COMMENT '标题',
  `cover`      VARCHAR(512) NOT NULL DEFAULT '' COMMENT '封面 URL',
  `uri`        VARCHAR(512) NOT NULL DEFAULT '' COMMENT '跳转 URI',
  `forward_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '转发的源动态 ID（0 表示原创）',
  `ctime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '发布时间（Unix 秒）',
  `mtime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_forward` (`forward_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='动态主表';

-- 关注流收件表（fan-out 投影）
CREATE TABLE IF NOT EXISTS `feed_inbox` (
  `id`         BIGINT  NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `mid`        BIGINT  NOT NULL DEFAULT 0 COMMENT '粉丝（收件人）ID',
  `feed_id`    BIGINT  NOT NULL DEFAULT 0 COMMENT '动态 ID（feed_outbox.id）',
  `author_mid` BIGINT  NOT NULL DEFAULT 0 COMMENT '发布者 ID',
  `state`      TINYINT NOT NULL DEFAULT 0 COMMENT '0 正常、2 已删除',
  `ctime`      BIGINT  NOT NULL DEFAULT 0 COMMENT '收件时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_feed` (`mid`, `feed_id`),
  KEY `idx_mid_state_ctime` (`mid`, `state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='关注流收件表';

-- 个人主页置顶表：唯一索引 (mid, feed_id)
-- 回滚：DROP TABLE IF EXISTS `feed_pin`;
CREATE TABLE IF NOT EXISTS `feed_pin` (
  `id`      BIGINT  NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `mid`     BIGINT  NOT NULL DEFAULT 0 COMMENT '用户 ID',
  `feed_id` BIGINT  NOT NULL DEFAULT 0 COMMENT '动态 ID',
  `state`   TINYINT NOT NULL DEFAULT 0 COMMENT '0 正常、1 删除',
  `ctime`   BIGINT  NOT NULL DEFAULT 0 COMMENT '置顶时间（Unix 秒）',
  `mtime`   BIGINT  NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_feed` (`mid`, `feed_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='动态置顶表';

-- 未读计数表：每 mid 一行，增量落库且允许从事件重算。
-- 回滚：DROP TABLE IF EXISTS `feed_unread`;
CREATE TABLE IF NOT EXISTS `feed_unread` (
  `mid`    BIGINT  NOT NULL COMMENT '用户 ID（主键）',
  `unread` BIGINT  NOT NULL DEFAULT 0 COMMENT '未读数',
  `mtime`  BIGINT  NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`mid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='动态未读计数表';
