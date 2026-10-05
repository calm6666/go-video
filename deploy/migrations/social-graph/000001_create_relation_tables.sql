-- =====================================================================
-- social-graph 服务 - 关注/取关关系表
-- =====================================================================
-- 数据所有者：social-graph 服务（AGENTS.md §5）。
-- 软删除：state=1 表示已取关，行保留用于关系历史与幂等重放。
-- 唯一索引 (mid, follower_mid) 支撑 INSERT ... ON DUPLICATE KEY UPDATE 幂等写入。
-- 回滚：DROP TABLE IF EXISTS `relation_follow`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `relation_follow` (
  `id`           BIGINT  NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `mid`          BIGINT  NOT NULL DEFAULT 0 COMMENT '关注发起方用户 ID',
  `follower_mid` BIGINT  NOT NULL DEFAULT 0 COMMENT '被关注者用户 ID',
  `attr`         INT     NOT NULL DEFAULT 0 COMMENT '关系属性位（保留）',
  `state`        TINYINT NOT NULL DEFAULT 0 COMMENT '0 正常、1 已取关',
  `ctime`        BIGINT  NOT NULL DEFAULT 0 COMMENT '关注时间（Unix 秒）',
  `mtime`        BIGINT  NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_follower` (`mid`, `follower_mid`),
  KEY `idx_mid_state_ctime` (`mid`, `state`, `ctime`),
  KEY `idx_follower_state_ctime` (`follower_mid`, `state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='关注关系表';

-- 黑名单表：唯一索引 (mid, black_mid)，state=1 表示已取消拉黑
-- 回滚：DROP TABLE IF EXISTS `relation_black`;
CREATE TABLE IF NOT EXISTS `relation_black` (
  `id`        BIGINT  NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `mid`       BIGINT  NOT NULL DEFAULT 0 COMMENT '拉黑发起方用户 ID',
  `black_mid` BIGINT  NOT NULL DEFAULT 0 COMMENT '被拉黑者用户 ID',
  `state`     TINYINT NOT NULL DEFAULT 0 COMMENT '0 正常、1 已取消拉黑',
  `ctime`     BIGINT  NOT NULL DEFAULT 0 COMMENT '拉黑时间（Unix 秒）',
  `mtime`     BIGINT  NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_black` (`mid`, `black_mid`),
  KEY `idx_black_mid_state` (`black_mid`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户黑名单表';

-- 特别关注表：唯一索引 (mid, special_mid)
-- 回滚：DROP TABLE IF EXISTS `relation_special`;
CREATE TABLE IF NOT EXISTS `relation_special` (
  `id`          BIGINT  NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `mid`         BIGINT  NOT NULL DEFAULT 0 COMMENT '操作用户 ID',
  `special_mid` BIGINT  NOT NULL DEFAULT 0 COMMENT '被特别关注者 ID',
  `state`       TINYINT NOT NULL DEFAULT 0 COMMENT '0 正常、1 已取消',
  `ctime`       BIGINT  NOT NULL DEFAULT 0 COMMENT '操作时间（Unix 秒）',
  `mtime`       BIGINT  NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_special` (`mid`, `special_mid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='特别关注表';

-- 关系计数表：每 mid 一行，增量通过 ON DUPLICATE KEY UPDATE 累加。
-- 计数为展示投影，可由关系表重算（不作为唯一事实源）。
-- 回滚：DROP TABLE IF EXISTS `relation_stat`;
CREATE TABLE IF NOT EXISTS `relation_stat` (
  `id`        BIGINT  NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `mid`       BIGINT  NOT NULL DEFAULT 0 COMMENT '用户 ID',
  `following` BIGINT  NOT NULL DEFAULT 0 COMMENT '关注数',
  `follower`  BIGINT  NOT NULL DEFAULT 0 COMMENT '粉丝数',
  `whisper`   BIGINT  NOT NULL DEFAULT 0 COMMENT '悄悄关注数（保留，本期固定 0）',
  `ctime`     BIGINT  NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`     BIGINT  NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid` (`mid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='关系计数表';
